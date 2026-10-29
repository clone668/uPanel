#!/bin/sh
# P13.3 Linux 验收脚本：通知通道、秘密引用、SMTP/HTTPS 投递、显式测试发送、投递账与失败重试上限。
#
# 运行位置：Ubuntu 22.04/24.04 验收 VM 内，**必须以 root 运行**。不依赖 Go 工具链：
# 用宿主交叉编译好的 Linux 二进制。
#
# 用法：
#   sudo BIN_DIR=/tmp/upanel-bin sh tests/integration/p13-3-acceptance.sh
#
# 退出码：0 全部通过；1 有失败项；2 环境不满足（非 root、缺 openssl、端口被占）。
#
# 覆盖的验收点（STAGES P13.3、PLAN §18、CONTRACTS §13.3）：
#   A. 通道校验与权限：viewer/未登录读不到（通道列表里带着收件邮箱与回调地址）、写要
#      admin + CSRF；in_app 只有一条且不可删；smtp 没有明文模式、用户名与口令必须同进
#      同出；webhook 只收 https、拒 userinfo 与 #片段；max_attempts 越界 400；
#      PATCH 缺 version 400、旧 version 409、未知 id 404
#   B. 秘密引用：POST /secrets 要再认证；口令与令牌只在代理库里，面板只存 secret_ref；
#      响应里没有明文
#   C. **保存配置不发送**：建好/改好 smtp 与 webhook 通道之后，本地接收器一个请求都没
#      收到、投递账 0 行（保存一次就往外面喷一封邮件是取消不掉的行为）
#   D. 显式测试发送：smtp STARTTLS 带 AUTH → 本地收到那封信且正文与契约逐字一致；
#      隐式 TLS 同样通；对端 535 → 回 200 + 投递行 failed + **对端原话**、且只连了一次
#      （测试发送不重试）；webhook 200/500/302/超时四种回执各按契约判定，302 不跟随；
#      令牌以 Authorization: Bearer 到达（日志里只有它的 sha256）；用途不对的秘密被
#      **代理**拒绝；in_app 测试发送 400
#   E. 告警驱动与投递账（受控种入的一条序列）：静默期间的事件**一条投递都不入队**；
#      触发事件按通道各一行、同通道同 kind 只有一行（去重）；失败按 30 秒退避后重试，
#      退避时间在 next_attempt_at 上看得见；先坏后好的通道第二次尝试成功；用尽
#      max_attempts 的通道判 failed；恢复事件同样发；停用通道的 pending 判 failed 且
#      不计入 attempts
#   F. 审计与卫生：notification.channel.create/update/delete/test/manage 落在
#      audit_events；审计里 secret_ref 被红字化；**口令与令牌不出现在面板库、代理库、
#      两端日志与接收器日志里**；web 日志没有 panic
#
# 为什么自带接收器：验收不能向任何真实的第三方联系人发信（STAGES P13.3 的边界）。
# 本脚本在一个进程里起 SMTP(STARTTLS)、SMTP(隐式 TLS) 与 HTTPS 回调三个本地服务，
# 应答码由控制文件决定，于是"对端临时拒绝/永久拒绝/要求重定向/超时"都能被摆出来。
#
# 为什么装一个本地 CA：代理对 SMTP 与 webhook 都做真 TLS 校验（关校验等于把"发到哪"
# 交给任何能劫持网线的人）。自签证书过不了校验，所以脚本生成一个只在本次验收存在的
# CA，装进系统信任库，结束时删掉。**不是**关掉校验，也不是跳过 TLS。
#
# 为什么用受控种入而不压 CPU：告警的"持续 30 秒"是契约尺度，靠压机器制造超限会让
# 断言跟着负载调度走。这里给一条不存在的网卡 accept0 写 rx_bytes_per_sec 样本，
# 写什么值由 $WORK/probe.phase 决定（high=5000 / low=10），哪一秒超限由脚本说了算。

set -u

# 秘密走文件而不是 argv：请求体先写进 $WORK/req.json（本 umask 下是 0600）再用
# curl -d @file 送出去。这样 ps 里看不到 SMTP 口令与 webhook 令牌。
umask 077

BIN_DIR=${BIN_DIR:-/tmp/upanel-bin}
if [ "$(id -u)" -ne 0 ]; then
  printf '本脚本必须以 root 运行：sudo BIN_DIR=%s sh %s\n' "$BIN_DIR" "$0" >&2
  exit 2
fi

AGENT="$BIN_DIR/upanel-agent"
WEB="$BIN_DIR/upanel-web"
CTL="$BIN_DIR/upanelctl"

WORK=${WORK:-/var/tmp/upanel-accept-p13-3}
RUN_DIR=/run/upanel
SOCK=$RUN_DIR/agent.sock
SYS_CFG=/etc/upanel/upanel.json
AGENT_DB_DIR=/var/lib/upanel-agent
AGENT_DB=$AGENT_DB_DIR/agent.db
KEY_FILE=/etc/upanel/agent.key
WEB_DB_DIR=$WORK/webdata
WEB_DB=$WEB_DB_DIR/upanel.db
PORT=${PORT:-19463}
BASE="http://127.0.0.1:$PORT"
ORIGIN="$BASE"
WEB_USER=upanel-web
PASSWORD='vm-correct-horse-9'
VIEWER_PASSWORD='vm-viewer-horse-42'

# 本地接收器的三个端口。分开是为了断言"隐式 TLS 与 STARTTLS 是两条真的路径"，
# 而不是同一条被两种配置碰巧都能走。
SMTP_STARTTLS_PORT=${SMTP_STARTTLS_PORT:-19471}
SMTP_TLS_PORT=${SMTP_TLS_PORT:-19472}
WEBHOOK_PORT=${WEBHOOK_PORT:-19470}

RECV_DIR=$WORK/recv
SMTP_LOG=$RECV_DIR/smtp.jsonl
HTTP_LOG=$RECV_DIR/http.jsonl
CONTROL=$RECV_DIR/control.json
CA_DIR=$WORK/ca
CA_CERT=$CA_DIR/ca.crt
SRV_CERT=$CA_DIR/server.crt
SRV_KEY=$CA_DIR/server.key
CA_INSTALLED=/usr/local/share/ca-certificates/upanel-accept-p133.crt

# 两个秘密在脚本里生成，只经由 $WORK 下的 0600 文件传递：
#   smtp_password   接收器用来核口令（它必须知道期望值）
#   hook_token      用来算 Authorization 的 sha256（日志里只留哈希）
SMTP_SECRET=$(python3 -c 'import secrets; print("smtp-" + secrets.token_hex(12))')
HOOK_TOKEN=$(python3 -c 'import secrets; print("hook-" + secrets.token_hex(12))')

# 接收窗口给足：验收期间不希望清理线程把正在被判定的样本裁掉。
RAW_RETENTION=3600
MINUTE_RETENTION=7200
HOUR_RETENTION=14400

# 受控种入的序列（见文件头说明）：一条不存在的网卡，只有本脚本会写它。
SEED_TARGET=accept0
SEED_METRIC=rx_bytes_per_sec
PHASE_FILE=$WORK/probe.phase

AGENT_LOG=$WORK/logs/agent.log
WEB_LOG=$WORK/logs/web.log
RECV_LOG=$WORK/logs/receiver.log

PASSED=0
FAILED=0
FAILED_NAMES=""
AGENT_PID=""
WEB_PID=""
SEED_PID=""
RECV_PID=""

pass() { PASSED=$((PASSED + 1)); printf 'PASS  %s\n' "$1"; }
fail() { FAILED=$((FAILED + 1)); FAILED_NAMES="$FAILED_NAMES | $1"; printf 'FAIL  %s: %s\n' "$1" "${2:-}"; }
note() { printf 'NOTE  %s\n' "$1"; }

expect_eq() { if [ "$2" = "$3" ]; then pass "$1"; else fail "$1" "期望 '$2'，实际 '$3'"; fi; }

# is_num <值>：能不能安全地当整数用。从库里读回来的东西可能是 <missing> 或
# <sql-error: ...>，对它们做算术会让 dash 直接退场。
is_num() {
  case ${1:-} in
    ''|*[!0-9-]*) return 1 ;;
    -) return 1 ;;
    *) return 0 ;;
  esac
}

expect_contains() {
  case $3 in
    *"$2"*) pass "$1" ;;
    *) fail "$1" "缺少 '$2'（实际: $3）" ;;
  esac
}

expect_not_contains() {
  case $3 in
    *"$2"*) fail "$1" "不该出现 '$2'（实际: $3）" ;;
    *) pass "$1" ;;
  esac
}

# jget <文件> <python 表达式>：把表达式的结果印出来。
# 布尔统一印成 true/false：Python 的 True/False 与 JSON 的 true/false 在同一份脚本里
# 混用，会让断言写成 "false" 而实际拿到 "False"，那种失败看起来像产品不生效。
jget() {
  python3 -c 'import json,sys
d=json.load(open(sys.argv[1], encoding="utf-8"))
v=eval(sys.argv[2])
print(str(v).lower() if isinstance(v, bool) else v)' "$1" "$2" 2>/dev/null
}

# jsonl_eval <日志> <表达式>：表达式里 r 是全部记录，m 是 message/request 记录，
# c 是 connect 记录。找不到文件时按空列表算（"一条都没收到"也要能断言）。
jsonl_eval() {
  python3 - "$1" "$2" <<'PY' 2>/dev/null
import json, sys
try:
    with open(sys.argv[1], encoding="utf-8") as f:
        r = [json.loads(line) for line in f if line.strip()]
except OSError:
    r = []
m = [x for x in r if x.get("event") in ("message", "request") and x.get("path") != "/ping"]
c = [x for x in r if x.get("event") == "connect"]
v = eval(sys.argv[2])
print(str(v).lower() if isinstance(v, bool) else v)
PY
}

# 契约里测试通知的正文（internal/notifications/deliveries.go renderTestMessage）。
# 这里**照着契约重写一遍**而不是从产品里拿：两边算出同一个 sha256，才说明发出去的
# 就是那句原文，而不是"某种差不多的中文"。
test_body_sha() {
  python3 - "$1" "$2" "$3" <<'PY'
import hashlib, sys
body = ("这是一条来自 uPanel 的测试通知。\n通道：%s（%s）\n目的地：%s\n"
        "收到这条消息说明通道参数可用；它不代表任何主机发生了异常。") % (sys.argv[1], sys.argv[2], sys.argv[3])
print(hashlib.sha256(body.encode("utf-8")).hexdigest())
PY
}

# token_sha：从 stdin 读，**不从 argv 读**。令牌走 argv 会出现在进程列表里，
# 那正是"秘密不进 argv"要挡的东西。
token_sha() {
  python3 -c 'import hashlib,sys; print(hashlib.sha256(sys.stdin.buffer.read()).hexdigest())'
}

# sqlq <单值查询>：查不到时印 <missing>，查询本身出错才印 <sql-error: ...>。
# 两者必须分开：第一版把"没有这一行"也打成 sql-error，于是下一个把结果拼进
# SQL 的断言拿到一串带引号的错误文本，报出来的是语法错误而不是"事件没产生"。
sqlq() {
  python3 - "$WEB_DB" "$1" <<'PY'
import sqlite3, sys
try:
    db = sqlite3.connect("file:" + sys.argv[1] + "?mode=ro", uri=True, timeout=15)
    db.execute("PRAGMA busy_timeout = 15000")
    row = db.execute(sys.argv[2]).fetchone()
except Exception as exc:
    print("<sql-error: %s>" % exc)
else:
    print("<missing>" if row is None or row[0] is None else row[0])
PY
}

# dfield <WHERE> <列>：最近一条匹配的投递行的某一列。没有记录时印 <missing>，
# 与"值是空串"分开——否则"投递还没写进去"会被读成"字段是空的"。
dfield() {
  python3 - "$WEB_DB" "$1" "$2" <<'PY'
import sqlite3, sys
try:
    db = sqlite3.connect("file:" + sys.argv[1] + "?mode=ro", uri=True, timeout=15)
    db.execute("PRAGMA busy_timeout = 15000")
    row = db.execute("SELECT " + sys.argv[3] + " FROM notification_deliveries WHERE "
                     + sys.argv[2] + " ORDER BY id DESC LIMIT 1").fetchone()
except Exception as exc:
    print("<sql-error: %s>" % exc)
else:
    print("<missing>" if row is None else ("" if row[0] is None else row[0]))
PY
}

dcount() {
  python3 - "$WEB_DB" "$1" <<'PY'
import sqlite3, sys
try:
    db = sqlite3.connect("file:" + sys.argv[1] + "?mode=ro", uri=True, timeout=15)
    db.execute("PRAGMA busy_timeout = 15000")
    print(db.execute("SELECT COUNT(*) FROM notification_deliveries WHERE " + sys.argv[2]).fetchone()[0])
except Exception as exc:
    print("<sql-error: %s>" % exc)
PY
}

# dlist <WHERE>：把匹配的投递行印成一行一条，用于失败时的取证（不带正文）。
dlist() {
  python3 - "$WEB_DB" "$1" <<'PY' 2>/dev/null
import sqlite3, sys
db = sqlite3.connect("file:" + sys.argv[1] + "?mode=ro", uri=True, timeout=15)
db.execute("PRAGMA busy_timeout = 15000")
sql = ("SELECT channel_name, kind, state, attempts, max_attempts, last_status,"
       " next_attempt_at, COALESCE(sent_at,''), last_error"
       "  FROM notification_deliveries WHERE " + sys.argv[2] + " ORDER BY id")
for row in db.execute(sql):
    print("      " + " | ".join(str(c) for c in row))
PY
}

now_unix() { date -u +%s; }
iso_of() { date -u -d "@$1" +%Y-%m-%dT%H:%M:%SZ; }

start_agent() {
  "$AGENT" --config "$SYS_CFG" >>"$AGENT_LOG" 2>&1 &
  AGENT_PID=$!
  for _ in $(seq 1 100); do
    [ -S "$SOCK" ] && return 0
    sleep 0.1
  done
  return 1
}

stop_agent() {
  if [ -n "$AGENT_PID" ] && kill -0 "$AGENT_PID" 2>/dev/null; then
    kill -TERM "$AGENT_PID" 2>/dev/null
    wait "$AGENT_PID" 2>/dev/null
  fi
  AGENT_PID=""
  rm -f "$SOCK"
}

start_web() {
  runuser -u "$WEB_USER" -- "$WEB" --config "$SYS_CFG" --mode production --db "$WEB_DB" \
    --listen "127.0.0.1:$PORT" >>"$WEB_LOG" 2>&1 &
  WEB_PID=$!
  for _ in $(seq 1 150); do
    if curl -fsS "$BASE/api/v1/health/live" >/dev/null 2>&1; then return 0; fi
    sleep 0.2
  done
  return 1
}

stop_web() {
  if [ -n "$WEB_PID" ] && kill -0 "$WEB_PID" 2>/dev/null; then
    kill -TERM "$WEB_PID" 2>/dev/null
    wait "$WEB_PID" 2>/dev/null
  fi
  WEB_PID=""
  for _ in $(seq 1 150); do
    curl -fsS "$BASE/api/v1/health/live" >/dev/null 2>&1 || return 0
    sleep 0.2
  done
  return 1
}

stop_recv() {
  if [ -n "$RECV_PID" ] && kill -0 "$RECV_PID" 2>/dev/null; then
    kill -TERM "$RECV_PID" 2>/dev/null
    wait "$RECV_PID" 2>/dev/null
  fi
  RECV_PID=""
}

# write_receiver：本地接收器（SMTP STARTTLS / SMTP 隐式 TLS / HTTPS 回调）。
#
# 自己写而不装 aiosmtpd：24.04 的 Python 3.12 已经删掉标准库 smtpd，验收 VM 又不该
# 为了跑一次验收去联网装包。这里只要"收一封信 + 回一个应答码"，状态机比依赖可靠。
write_receiver() {
  cat >"$RECV_DIR/receiver.py" <<'PY'
# 应答由 recv/control.json 决定，脚本改这个文件就能让对端换一种拒绝方式，不必重启：
#
#   {"smtp": {"auth": "ok" | "reject" | "tempfail"},
#    "http": {"/ok": {"status": 200, "body": "ok"},
#             "/always500": {"status": 500, "body": "boom"},
#             "/redirect": {"status": 302, "location": "https://example.invalid/x"},
#             "/slow": {"sleep": 12}}}
#
# **日志里不出现任何秘密**：AUTH 只记 auth_ok 与用户名；HTTP 只记 Authorization 的
# sha256 与是否存在。验收要证明的是"令牌被正确带上"，不是"把它抄进日志"。

import asyncio
import base64
import hashlib
import json
import os
import ssl
import sys
import time
import urllib.parse

RECV_DIR, CERT, KEY = sys.argv[1], sys.argv[2], sys.argv[3]
STARTTLS_PORT, IMPLICIT_PORT, HTTPS_PORT = (int(x) for x in sys.argv[4:7])

CONTROL = os.path.join(RECV_DIR, "control.json")
PASSWORD_FILE = os.path.join(RECV_DIR, "smtp_password")
SMTP_LOG = os.path.join(RECV_DIR, "smtp.jsonl")
HTTP_LOG = os.path.join(RECV_DIR, "http.jsonl")


def control():
    try:
        with open(CONTROL, encoding="utf-8") as f:
            return json.load(f)
    except (OSError, ValueError):
        return {}


def log(path, record):
    record["ts"] = round(time.time(), 3)
    with open(path, "a", encoding="utf-8") as f:
        f.write(json.dumps(record, ensure_ascii=False) + "\n")
        f.flush()


def server_ctx():
    ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    ctx.minimum_version = ssl.TLSVersion.TLSv1_2
    ctx.load_cert_chain(CERT, KEY)
    return ctx


CTX = server_ctx()


def expected_password():
    try:
        with open(PASSWORD_FILE, encoding="utf-8") as f:
            return f.read().strip()
    except OSError:
        return ""


def sha256(text):
    return hashlib.sha256(text.encode("utf-8", "replace")).hexdigest()


class TLSBridge(asyncio.Protocol):
    """握手期间对端的数据只进缓冲区，绝不直接当命令解析。

    start_tls 换好协议后，解密出来的应用数据会送到**这里**（也就是传给 start_tls
    的那个协议对象），而不是 SMTPSession。如果直接把 SMTPSession 传进去，"对端在
    握手后的第一句 EHLO"会在 start_tls 还没返回时到达（3.12 实测相差几毫秒），被
    当成握手前的明文丢掉，对端只能一直等到超时——这是最开始那一版在 3.12 上的真
    故障：openssl s_client 过得去，Go 的 net/smtp 一句都过不去。搭一层桥，先收着，
    握手返回后再重放。
    """

    def __init__(self, session):
        self.session = session
        self.ready = False

    def connection_made(self, transport):
        pass

    def data_received(self, data):
        if self.ready:
            self.session.data_received(data)
        else:
            self.session.pending_in += data

    def eof_received(self):
        return False

    def connection_lost(self, exc):
        self.session.connection_lost(exc)


class SMTPSession(asyncio.Protocol):
    def __init__(self, loop, implicit):
        self.loop = loop
        self.implicit = implicit
        self.tls = implicit
        self.buf = b""
        self.paused = False
        self.pending_in = b""
        self.transport = None
        self.state = "cmd"
        self.auth = {"tried": False, "ok": None, "user": ""}
        self.mail_from = ""
        self.rcpt = []
        self.data_lines = []

    # ---- 传输层 ----
    def connection_made(self, transport):
        self.transport = transport
        log(SMTP_LOG, {"event": "connect",
                       "tls_mode": "implicit" if self.implicit else "starttls"})
        if self.implicit:
            # 隐式 TLS：对端连上就开始握手。必须在**数据回调之前**停下读取，
            # 否则 ClientHello 会被当成 SMTP 命令吃掉（那会让握手永远等不到）。
            # start_tls 自己会 pause/resume，这里多暂停一次不影响它。
            transport.pause_reading()
            self.paused = True
            self.loop.create_task(self._upgrade())
        else:
            self.write("220 accept.test ESMTP uPanel acceptance")

    def write(self, line):
        if self.transport is None:
            return
        self.transport.write((line + "\r\n").encode("utf-8"))

    async def _upgrade(self):
        try:
            # server_side=True 必须显式给：start_tls 的默认值是 False（客户端），
            # 漏掉它握手会以"对端不是 TLS"失败，看起来像证书问题。
            # 第三个参数是桥，不是 self：理由见 TLSBridge 的注释。
            bridge = TLSBridge(self)
            new_transport = await self.loop.start_tls(
                self.transport, bridge, CTX, server_side=True)
        except Exception as exc:
            log(SMTP_LOG, {"event": "error", "where": "tls", "error": str(exc)})
            if self.transport is not None:
                self.transport.close()
            return
        # 握手之后写的字节必须走新传输，否则它们以明文发出去——那是最坏的一种失败。
        self.transport = new_transport
        self.tls = True
        # STARTTLS 之后旧缓冲必须丢掉：升级前读到的明文不该被当成命令执行。
        self.buf = b""
        self.paused = False
        pending, self.pending_in = self.pending_in, b""
        bridge.ready = True
        if self.implicit:
            self.write("220 accept.test ESMTP uPanel acceptance")
        if pending:
            self.data_received(pending)

    def data_received(self, data):
        if self.paused:
            return
        self.buf += data
        while b"\n" in self.buf:
            raw, self.buf = self.buf.split(b"\n", 1)
            self.handle(raw.rstrip(b"\r").decode("utf-8", "replace"))
            if self.paused:
                return

    # ---- 协议 ----
    def handle(self, line):
        if self.state == "data":
            if line == ".":
                self.finish_data()
                return
            self.data_lines.append(line[1:] if line.startswith("..") else line)
            return
        if self.state == "auth":
            self.state = "cmd"
            self.finish_auth(line.strip())
            return
        upper = line.upper()
        if upper.startswith("EHLO") or upper.startswith("HELO"):
            self.write("250-accept.test")
            if not self.tls:
                self.write("250-STARTTLS")
            else:
                self.write("250-AUTH PLAIN")
            self.write("250-8BITMIME")
            self.write("250 OK")
        elif upper == "STARTTLS":
            if self.tls:
                self.write("503 5.5.1 TLS already active")
                return
            # 先停读再回 220：对端收到 220 就发 ClientHello，那几字节不能落进
            # data_received 被当成命令解析（start_tls 自己会 resume）。
            self.transport.pause_reading()
            self.write("220 2.0.0 Ready to start TLS")
            self.paused = True
            self.loop.create_task(self._upgrade())
        elif upper.startswith("AUTH PLAIN"):
            arg = line.strip()[len("AUTH PLAIN"):].strip()
            self.auth["tried"] = True
            if not arg:
                self.state = "auth"
                self.write("334 ")
            else:
                self.finish_auth(arg)
        elif upper.startswith("MAIL FROM:"):
            # "MAIL FROM:<a@b> BODY=8BITMIME"：第一个空格之后是 ESMTP 参数，不是地址。
            # 不切掉它，信封发件人会被记成 "<a@b> BODY=8BITMIME"，看着像被改了。
            self.mail_from = line.strip()[len("MAIL FROM:"):].split(" ")[0].strip().strip("<>")
            self.rcpt = []
            self.write("250 2.1.0 Ok")
        elif upper.startswith("RCPT TO:"):
            self.rcpt.append(line.strip()[len("RCPT TO:"):].strip().strip("<>"))
            self.write("250 2.1.5 Ok")
        elif upper == "DATA":
            self.state = "data"
            self.data_lines = []
            self.write("354 End data with <CR><LF>.<CR><LF>")
        elif upper == "RSET":
            self.mail_from, self.rcpt, self.data_lines = "", [], []
            self.write("250 2.0.0 Ok")
        elif upper == "NOOP":
            self.write("250 2.0.0 Ok")
        elif upper == "QUIT":
            self.write("221 2.0.0 Bye")
            if self.transport is not None:
                self.transport.close()
        else:
            self.write("500 5.5.2 Command unrecognized")

    def finish_auth(self, arg):
        try:
            parts = base64.b64decode(arg + "===").split(b"\x00")
            user = parts[1].decode("utf-8", "replace")
            password = parts[2].decode("utf-8", "replace")
        except Exception:
            user, password = "", ""
        self.auth["user"] = user
        mode = (control().get("smtp") or {}).get("auth", "ok")
        good = user != "" and password != "" and password == expected_password()
        if mode == "reject":
            self.auth["ok"] = False
            self.write("535 5.7.8 Authentication credentials invalid")
        elif mode == "tempfail":
            self.auth["ok"] = None
            self.write("451 4.7.0 Try again later")
        elif good:
            self.auth["ok"] = True
            self.write("235 2.7.0 Authentication successful")
        else:
            self.auth["ok"] = False
            self.write("535 5.7.8 Authentication credentials invalid")

    def finish_data(self):
        self.state = "cmd"
        head, _, body = "\r\n".join(self.data_lines).partition("\r\n\r\n")
        headers = {}
        for line in head.split("\r\n"):
            if ":" in line:
                key, value = line.split(":", 1)
                headers[key.strip().lower()] = value.strip()
        packed = "".join(part.strip() for part in body.split("\r\n"))
        try:
            decoded = base64.b64decode(packed).decode("utf-8", "replace")
        except Exception:
            decoded = ""
        log(SMTP_LOG, {
            "event": "message",
            "tls_mode": "implicit" if self.implicit else "starttls",
            "mail_from": self.mail_from,
            "rcpt": self.rcpt,
            "subject": headers.get("subject", ""),
            "auth_tried": self.auth["tried"],
            "auth_ok": self.auth["ok"],
            "auth_user": self.auth["user"],
            "body_sha256": sha256(decoded),
            "body_head": decoded[:160],
            "body_len": len(decoded),
        })
        self.data_lines = []
        self.write("250 2.0.0 Ok: queued as ACCEPT")


class HTTPHandler:
    def __init__(self, reader, writer):
        self.reader, self.writer = reader, writer

    async def run(self):
        try:
            head = await asyncio.wait_for(self.reader.readuntil(b"\r\n\r\n"), timeout=20)
        except Exception:
            self.close()
            return
        lines = head.decode("latin-1").split("\r\n")
        parts = (lines[0].split(" ") + ["", ""])[:3]
        method, target = parts[0], parts[1]
        headers = {}
        for line in lines[1:]:
            if ":" in line:
                key, value = line.split(":", 1)
                headers[key.strip().lower()] = value.strip()
        try:
            length = int(headers.get("content-length", "0") or 0)
        except ValueError:
            length = 0
        body = b""
        if length:
            try:
                body = await asyncio.wait_for(self.reader.readexactly(min(length, 65536)), timeout=20)
            except Exception:
                body = b""
        path = urllib.parse.urlsplit(target).path
        record = {
            "event": "request",
            "method": method,
            "path": path,
            "authorization_present": "authorization" in headers,
            "authorization_sha256": sha256(headers.get("authorization", "")) if headers.get("authorization") else "",
            "body_head": body.decode("utf-8", "replace")[:400],
        }
        log(HTTP_LOG, record)
        rule = (control().get("http") or {}).get(path, {})
        sleep = float(rule.get("sleep", 0) or 0)
        if sleep > 0:
            await asyncio.sleep(sleep)
        status = int(rule.get("status", 200))
        payload = str(rule.get("body", "ok")).encode("utf-8")
        reason = {200: "OK", 201: "Created", 204: "No Content", 302: "Found",
                  429: "Too Many Requests", 500: "Internal Server Error"}.get(status, "Status")
        out = ["HTTP/1.1 %d %s" % (status, reason)]
        if rule.get("location"):
            out.append("Location: %s" % rule["location"])
        out.append("Content-Type: text/plain; charset=utf-8")
        out.append("Content-Length: %d" % len(payload))
        out.append("Connection: close")
        out.append("")
        out.append("")
        try:
            self.writer.write("\r\n".join(out).encode("utf-8") + payload)
            await self.writer.drain()
        except Exception:
            # 对端先放弃了（/slow 就是这种情形）。这不是接收器的错，如实记一笔。
            log(HTTP_LOG, {"event": "aborted", "path": path, "status": status})
        self.close()

    def close(self):
        try:
            self.writer.close()
        except Exception:
            pass


async def main():
    loop = asyncio.get_running_loop()
    servers = []
    servers.append(await loop.create_server(
        lambda: SMTPSession(loop, False), "127.0.0.1", STARTTLS_PORT))
    servers.append(await loop.create_server(
        lambda: SMTPSession(loop, True), "127.0.0.1", IMPLICIT_PORT))

    async def http_client(reader, writer):
        await HTTPHandler(reader, writer).run()

    servers.append(await asyncio.start_server(
        http_client, "127.0.0.1", HTTPS_PORT, ssl=CTX))
    print("receiver: smtp-starttls=%d smtp-implicit=%d https=%d"
          % (STARTTLS_PORT, IMPLICIT_PORT, HTTPS_PORT), flush=True)
    await asyncio.gather(*(s.serve_forever() for s in servers))


asyncio.run(main())
PY
  chmod 0755 "$RECV_DIR/receiver.py"
}

# write_control：重写接收器的应答表。整份覆盖，不留上一轮的残留。
write_control() {
  cat >"$CONTROL" <<JSON
{"smtp": {"auth": "${1:-ok}"},
 "http": {
   "/ok": {"status": 200, "body": "ok"},
   "/always500": {"status": 500, "body": "boom: acceptance always fails here"},
   "/flaky": {"status": ${2:-500}, "body": "flaky: not yet"},
   "/redirect": {"status": 302, "location": "https://example.invalid/should-not-be-followed"},
   "/slow": {"sleep": 12}
 }}
JSON
  chmod 0600 "$CONTROL"
}

start_recv() {
  python3 "$RECV_DIR/receiver.py" "$RECV_DIR" "$SRV_CERT" "$SRV_KEY" \
    "$SMTP_STARTTLS_PORT" "$SMTP_TLS_PORT" "$WEBHOOK_PORT" >>"$RECV_LOG" 2>&1 &
  RECV_PID=$!
  for _ in $(seq 1 100); do
    if curl -fsS --cacert "$CA_CERT" "https://127.0.0.1:$WEBHOOK_PORT/ping" >/dev/null 2>&1; then
      return 0
    fi
    sleep 0.2
  done
  return 1
}

# write_seeder：受控种入（与 p13-2 同一份写法与理由）。
write_seeder() {
  cat >"$WORK/seed.py" <<'PY'
import sqlite3
import sys
import time

SCOPE, TARGET, METRIC = "interface", "accept0", "rx_bytes_per_sec"
BUCKET = 5
VALUES = {"high": 5000.0, "low": 10.0}


def main():
    db_path, phase_path = sys.argv[1], sys.argv[2]
    backfill = int(sys.argv[3]) if len(sys.argv) > 3 else 0
    db = sqlite3.connect(db_path, timeout=15)
    db.execute("PRAGMA busy_timeout = 15000")

    def value_now():
        try:
            with open(phase_path, encoding="utf-8") as f:
                return VALUES.get(f.read().strip())
        except OSError:
            return None

    def write(ts, value):
        db.execute(
            "INSERT INTO metric_samples"
            " (scope, target_id, metric, resolution, ts, value, min_value, max_value, sample_count)"
            " VALUES (?, ?, ?, 'raw', ?, ?, ?, ?, 1)"
            " ON CONFLICT (scope, target_id, metric, resolution, ts)"
            " DO UPDATE SET value=excluded.value, min_value=excluded.min_value,"
            " max_value=excluded.max_value",
            (SCOPE, TARGET, METRIC, ts, value, value, value))

    if backfill > 0:
        value = value_now()
        if value is not None:
            now = int(time.time())
            start = ((now - backfill) // BUCKET) * BUCKET
            end = (now // BUCKET) * BUCKET
            ts = start
            while ts < end:
                write(ts, value)
                ts += BUCKET
            db.commit()
            print("seed: 回填 %d 秒（%s）" % (end - start, value), flush=True)

    while True:
        t = time.time()
        wake = (int(t) // BUCKET + 1) * BUCKET
        time.sleep(max(0.0, wake - t))
        value = value_now()
        if value is None:
            continue
        try:
            write(wake, value)
            db.commit()
        except sqlite3.Error as exc:
            print("seed: 写入失败（继续）: %s" % exc, file=sys.stderr, flush=True)


main()
PY
  chmod 0644 "$WORK/seed.py"
}

start_seed() {
  runuser -u "$WEB_USER" -- python3 "$WORK/seed.py" "$WEB_DB" "$PHASE_FILE" "${1:-0}" \
    >>"$WORK/logs/seed.log" 2>&1 &
  SEED_PID=$!
}

stop_seed() {
  if [ -n "$SEED_PID" ] && kill -0 "$SEED_PID" 2>/dev/null; then
    kill -TERM "$SEED_PID" 2>/dev/null
    wait "$SEED_PID" 2>/dev/null
  fi
  SEED_PID=""
}

# phase <high|low>：切换种入的值（写临时文件再 mv，避免种入循环读到半行）。
#
# 必须 chmod 0644：种入进程以 Web 账号运行，而本脚本的 umask 是 077（秘密不落进
# 别人读得到的地方）。第一版漏了这一步，种入读不到 phase 文件就按"没有值"跳过，
# 一个样本都不写——告警自然不会触发，后面二十几条断言全部连坐。
phase() {
  printf '%s\n' "$1" >"$PHASE_FILE.tmp" && chmod 0644 "$PHASE_FILE.tmp" \
    && mv -f "$PHASE_FILE.tmp" "$PHASE_FILE"
}

login() {
  curl -s -c "$3" -o "$WORK/$4.json" -w '%{http_code}' -X POST "$BASE/api/v1/auth/login" \
    -H 'Content-Type: application/json' -H 'X-UPanel-Request: 1' -H "Origin: $ORIGIN" \
    -d "{\"username\":\"$1\",\"password\":\"$2\"}"
}

csrf_of() { jget "$1" 'd["data"]["csrf_token"]'; }

# api <jar> <csrf> <method> <path> [body]
#
# CSRF 令牌**始终**带上（DELETE 没有 body，早先版本只在有 body 时才加这个头，
# 于是所有 DELETE 都拿到 403 CSRF_FAILED——那不是产品的边界，是脚本的 bug）。
api() {
  jar=$1; csrf=$2; method=$3; path=$4; body=${5:-}
  if [ -n "$body" ]; then
    printf '%s' "$body" >"$WORK/req.json"
    curl -s -b "$jar" -o "$WORK/resp.json" -w '%{http_code}' -X "$method" "$BASE$path" \
      -H 'Content-Type: application/json' -H "Origin: $ORIGIN" -H "X-CSRF-Token: $csrf" \
      -d @"$WORK/req.json"
  else
    curl -s -b "$jar" -o "$WORK/resp.json" -w '%{http_code}' -X "$method" "$BASE$path" \
      -H "Origin: $ORIGIN" -H "X-CSRF-Token: $csrf"
  fi
}

channelsapi() {
  curl -s -b "${1:-$WORK/admin.jar}" -o "$WORK/channels.json" -w '%{http_code}' -X GET \
    "$BASE/api/v1/notification-channels" -H "Origin: $ORIGIN"
}

chan_id_by_name() {
  python3 -c 'import json,sys
d=json.load(open(sys.argv[1], encoding="utf-8"))
for c in d["data"]:
    if c["name"]==sys.argv[2]:
        print(c["id"]); break
else:
    print("")' "$WORK/channels.json" "$1" 2>/dev/null
}

chan_version() {
  python3 -c 'import json,sys
d=json.load(open(sys.argv[1], encoding="utf-8"))
for c in d["data"]:
    if c["id"]==int(sys.argv[2]):
        print(c["version"]); break
else:
    print("")' "$WORK/channels.json" "$1" 2>/dev/null
}

# wait_delivery <说明> <WHERE> <列> <期望> <超时秒>
wait_delivery() {
  desc=$1; where=$2; col=$3; want=$4; limit=${5:-90}
  deadline=$(( $(now_unix) + limit ))
  got=""
  while :; do
    got=$(dfield "$where" "$col")
    if [ "$got" = "$want" ]; then return 0; fi
    if [ "$(now_unix)" -ge "$deadline" ]; then
      printf 'NOTE  等待超时（%s）：%s 期望 %s，实际 %s\n' "$desc" "$col" "$want" "$got"
      return 1
    fi
    sleep 2
  done
}

# wait_events <rule_id> <最少条数> <超时秒>：等告警引擎把事件写进库里。
wait_events() {
  rule=$1; want=${2:-1}; limit=${3:-90}
  deadline=$(( $(now_unix) + limit ))
  got=0
  while :; do
    got=$(sqlq "SELECT COUNT(*) FROM alert_events WHERE rule_id = $rule")
    case $got in ''|*[!0-9]*) got=0 ;; esac
    if [ "$got" -ge "$want" ]; then return 0; fi
    if [ "$(now_unix)" -ge "$deadline" ]; then
      printf 'NOTE  等待事件超时：规则 %s 只写了 %s 条（期望 %s）\n' "$rule" "$got" "$want"
      return 1
    fi
    sleep 3
  done
}

chown_web_db() {
  for suffix in "" "-wal" "-shm"; do
    [ -e "$WEB_DB$suffix" ] && chown "$WEB_USER" "$WEB_DB$suffix"
  done
}

cleanup() {
  stop_seed
  stop_recv
  stop_web
  stop_agent
  # 本地 CA 只在这次验收里存在过：留着它等于给这台机器装了一个"谁都能签"的根。
  if [ -f "$CA_INSTALLED" ]; then
    rm -f "$CA_INSTALLED"
    update-ca-certificates --fresh >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT
trap 'cleanup; exit 130' INT
trap 'cleanup; exit 143' TERM

echo "== 0. 环境"
if [ "$(id -u)" -eq 0 ]; then pass "验收脚本以 root 运行"; else fail "验收脚本以 root 运行" "当前 UID $(id -u)"; fi
for b in "$AGENT" "$WEB" "$CTL"; do
  if [ -x "$b" ]; then pass "二进制存在且可执行 $(basename "$b")"; else fail "二进制存在且可执行 $(basename "$b")" "$b"; fi
done
if command -v python3 >/dev/null 2>&1; then pass "python3 可用（接收器、种入、解析响应）"; else fail "python3 可用" "缺少 python3"; fi
if command -v runuser >/dev/null 2>&1; then pass "runuser 可用（以 Web 身份写库）"; else fail "runuser 可用" "缺少 runuser"; fi

# openssl 与 update-ca-certificates 是 TLS 验收的前提：缺了就只能跳过 TLS 校验，
# 那是把要验的东西删掉。这种机器上直接停（退出码 2 = 环境不满足）。
for need in openssl update-ca-certificates; do
  if command -v "$need" >/dev/null 2>&1; then
    pass "$need 可用"
  else
    printf '环境不满足：缺少 %s。本脚本要用它生成/安装只在验收期内有效的本地 CA；\n' "$need" >&2
    printf '不装 CA 就只能关掉代理的 TLS 校验，那等于把要验的东西删掉。先 apt-get install -y %s。\n' \
      "$( [ "$need" = openssl ] && echo openssl || echo ca-certificates )" >&2
    exit 2
  fi
done

PORTS="$PORT $SMTP_STARTTLS_PORT $SMTP_TLS_PORT $WEBHOOK_PORT"
for p in $PORTS; do
  # 先看面板端口上是不是已经有服务在跑（遗留的 upanel-web）：那种情况下不该继续，
  # 否则后面的断言会对着上一轮的进程说话。
  if [ "$p" = "$PORT" ] && curl -fsS "$BASE/api/v1/health/live" >/dev/null 2>&1; then
    printf '%s 上已有服务在监听（遗留的 upanel-web）；先停掉它再跑验收\n' "$PORT" >&2
    exit 1
  fi
  if python3 -c 'import socket,sys
s=socket.socket()
try:
    s.bind(("127.0.0.1", int(sys.argv[1])))
except OSError:
    raise SystemExit(1)
finally:
    s.close()' "$p" 2>/dev/null; then
    pass "端口 $p 空闲"
  else
    fail "端口 $p 空闲" "已被占用"
  fi
done

if ! id -u "$WEB_USER" >/dev/null 2>&1; then
  note "创建系统账号 $WEB_USER（验收环境未预置）"
  useradd --system --no-create-home --shell /usr/sbin/nologin "$WEB_USER" 2>/dev/null
fi
if id -u "$WEB_USER" >/dev/null 2>&1; then pass "Web 账号 $WEB_USER 存在"; else fail "Web 账号 $WEB_USER 存在" "无法创建"; fi

rm -rf "$WORK"
mkdir -p "$WORK/logs" "$RECV_DIR" "$CA_DIR" "$WEB_DB_DIR"
chmod 0755 "$WORK" "$WORK/logs" "$RECV_DIR"
chown -R "$WEB_USER" "$WEB_DB_DIR"

# 接收器要知道期望的 SMTP 口令才能核 AUTH。写文件而不是给 argv：进程列表不是记事本。
printf '%s' "$SMTP_SECRET" >"$RECV_DIR/smtp_password"
chmod 0600 "$RECV_DIR/smtp_password"

echo
echo "== 1. 装配：本地 CA + 本地接收器 + 代理 + 管理员 + Web =="

# 本地 CA + 一张给 127.0.0.1 的服务端证书。SAN 里必须有 IP：代理是用 IP 连的，
# 只签 DNS 名字的证书在 Go 那边会以"证书不匹配"失败，而那看起来像产品不支持 TLS。
openssl req -x509 -newkey rsa:2048 -nodes -days 2 -sha256 \
  -keyout "$CA_DIR/ca.key" -out "$CA_CERT" \
  -subj "/O=uPanel Acceptance/CN=uPanel Acceptance CA" >"$WORK/logs/ca.log" 2>&1
if [ -s "$CA_CERT" ]; then pass "生成验收用本地 CA"; else fail "生成验收用本地 CA" "$(tail -3 "$WORK/logs/ca.log")"; fi

openssl req -newkey rsa:2048 -nodes \
  -keyout "$SRV_KEY" -out "$CA_DIR/server.csr" \
  -subj "/O=uPanel Acceptance/CN=127.0.0.1" >>"$WORK/logs/ca.log" 2>&1
cat >"$CA_DIR/server.ext" <<'EXT'
basicConstraints = CA:FALSE
keyUsage = digitalSignature, keyEncipherment
extendedKeyUsage = serverAuth
subjectAltName = IP:127.0.0.1, DNS:accept.test
EXT
openssl x509 -req -in "$CA_DIR/server.csr" -CA "$CA_CERT" -CAkey "$CA_DIR/ca.key" \
  -CAcreateserial -days 2 -sha256 -extfile "$CA_DIR/server.ext" \
  -out "$SRV_CERT" >>"$WORK/logs/ca.log" 2>&1
chmod 0600 "$SRV_KEY"

cp "$CA_CERT" "$CA_INSTALLED"
chmod 0644 "$CA_INSTALLED"
if update-ca-certificates >"$WORK/logs/ca-install.log" 2>&1; then
  pass "本地 CA 装进系统信任库"
else
  fail "本地 CA 装进系统信任库" "$(tail -3 "$WORK/logs/ca-install.log")"
fi
# 用系统信任库本身去验一次：Go 读的就是这个文件，"装上了"不能只看命令退出码。
if openssl verify -CAfile /etc/ssl/certs/ca-certificates.crt "$SRV_CERT" >>"$WORK/logs/ca-install.log" 2>&1; then
  pass "系统信任库能验通过接收器的证书（代理侧 TLS 校验的前提）"
else
  fail "系统信任库能验通过接收器的证书" "$(tail -3 "$WORK/logs/ca-install.log")"
fi

write_receiver
write_control ok 500
if start_recv; then
  pass "本地接收器启动（SMTP $SMTP_STARTTLS_PORT 显式 TLS / $SMTP_TLS_PORT 隐式 TLS / HTTPS $WEBHOOK_PORT）"
else
  fail "本地接收器启动" "$(tail -5 "$RECV_LOG" 2>/dev/null)"
fi

install -d -m 0755 /etc/upanel
install -d -m 0700 "$AGENT_DB_DIR"
rm -f "$AGENT_DB" "$AGENT_DB-wal" "$AGENT_DB-shm"
install -d -m 0755 "$RUN_DIR"
chmod 0755 "$RUN_DIR"
chown root:root "$RUN_DIR"

cat >"$SYS_CFG" <<JSON
{
  "web": {
    "mode": "production",
    "listen_address": "127.0.0.1:$PORT",
    "shutdown_timeout_seconds": 10,
    "metrics": {
      "enabled": true,
      "interval_seconds": 2,
      "raw_retention_seconds": $RAW_RETENTION,
      "minute_retention_seconds": $MINUTE_RETENTION,
      "hour_retention_seconds": $HOUR_RETENTION,
      "max_rows": 500000
    }
  },
  "agent": {
    "socket_path": "/run/upanel/agent.sock",
    "shutdown_timeout_seconds": 10,
    "web_user": "upanel-web",
    "socket_group": "upanel-web",
    "database_path": "/var/lib/upanel-agent/agent.db",
    "secret_key_path": "/etc/upanel/agent.key"
  },
  "database": {
    "path": "$WEB_DB",
    "busy_timeout_ms": 5000
  },
  "log": {
    "level": "info",
    "format": "json"
  }
}
JSON
chmod 0644 "$SYS_CFG"

if [ -f "$KEY_FILE" ]; then
  pass "代理密钥文件已存在（沿用上一轮）"
else
  if "$AGENT" --config "$SYS_CFG" -init-key >"$WORK/init-key.log" 2>&1; then
    pass "-init-key 生成代理密钥"
  else
    fail "-init-key 生成代理密钥" "$(tail -3 "$WORK/init-key.log" 2>/dev/null)"
  fi
fi

if start_agent; then pass "代理启动并创建 socket"; else fail "代理启动" "$(tail -5 "$AGENT_LOG" 2>/dev/null)"; fi

printf '%s\n%s\n' "$PASSWORD" "$PASSWORD" | "$CTL" init-admin --config "$SYS_CFG" --db "$WEB_DB" \
  --username admin --display-name "管理员" --json >"$WORK/init.out" 2>"$WORK/init.err"
expect_eq "init-admin 退出码" "0" "$?"
chown_web_db

if start_web; then pass "Web 启动"; else fail "Web 启动" "$(tail -5 "$WEB_LOG" 2>/dev/null)"; fi

code=$(login admin "$PASSWORD" "$WORK/admin.jar" login-admin)
expect_eq "管理员登录 200" "200" "$code"
if [ "$code" != "200" ]; then
  fail "登录失败，后续断言无法进行" "$(cat "$WORK/login-admin.json" 2>/dev/null)"
  printf '通过 %d 项，失败 %d 项\n' "$PASSED" "$FAILED"
  exit 1
fi
ADMIN_CSRF=$(csrf_of "$WORK/login-admin.json")

code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "/api/v1/users" \
  "{\"username\":\"alice\",\"display_name\":\"alice\",\"role\":\"viewer\",\"initial_password\":\"$VIEWER_PASSWORD\"}")
expect_eq "创建 viewer 201" "201" "$code"
code=$(login alice "$VIEWER_PASSWORD" "$WORK/viewer.jar" login-viewer)
expect_eq "viewer 登录 200" "200" "$code"
VIEWER_CSRF=$(csrf_of "$WORK/login-viewer.json")

echo
echo "== 2. A：通道校验与权限 =="

code=$(channelsapi "$WORK/viewer.jar")
expect_eq "viewer 读通道列表 403（通道列表就是“告警会发给谁”）" "403" "$code"
expect_eq "viewer 被拒的错误码" "FORBIDDEN" "$(jget "$WORK/channels.json" 'd["error"]["code"]')"

code=$(curl -s -o "$WORK/resp.json" -w '%{http_code}' -X GET "$BASE/api/v1/notification-channels" \
  -H "Origin: $ORIGIN")
expect_eq "未登录读通道 401" "401" "$code"
expect_eq "未登录的错误码" "UNAUTHENTICATED" "$(jget "$WORK/resp.json" 'd["error"]["code"]')"

code=$(channelsapi)
expect_eq "管理员读通道列表 200" "200" "$code"
expect_eq "迁移种下的面板内通道只有一条" "1" "$(jget "$WORK/channels.json" 'len(d["data"])')"
INAPP_ID=$(jget "$WORK/channels.json" 'd["data"][0]["id"]')
expect_eq "面板内通道的 type" "in_app" "$(jget "$WORK/channels.json" 'd["data"][0]["type"]')"
expect_eq "面板内通道默认启用" "true" "$(jget "$WORK/channels.json" 'd["data"][0]["enabled"]')"
expect_eq "面板内通道没有秘密" "true" "$(jget "$WORK/channels.json" 'd["data"][0]["secret_ref"] is None')"
expect_eq "面板内通道不需要重试（max_attempts=1）" "1" "$(jget "$WORK/channels.json" 'd["data"][0]["max_attempts"]')"

code=$(curl -s -b "$WORK/admin.jar" -o "$WORK/resp.json" -w '%{http_code}' -X POST \
  "$BASE/api/v1/notification-channels" -H "Origin: $ORIGIN" \
  -H 'Content-Type: application/json' -d '{"name":"x","type":"webhook","settings":{"url":"https://example.invalid/h"}}')
expect_eq "管理员不带 CSRF 建通道 403" "403" "$code"

code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "/api/v1/notification-channels" \
  '{"name":"第二条面板内通知","type":"in_app"}')
expect_eq "再建一条 in_app → 400（收件箱只有一条）" "400" "$code"
expect_eq "重复 in_app 的错误码" "VALIDATION_ERROR" "$(jget "$WORK/resp.json" 'd["error"]["code"]')"
expect_contains "报错说清面板内通知只有一条" "只有一条" "$(jget "$WORK/resp.json" 'd["error"]["message"]')"

code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "/api/v1/notification-channels" \
  '{"name":"怪类型","type":"sms"}')
expect_eq "type 取闭集外的值 → 400" "400" "$code"
expect_contains "报错列出允许的三种类型" "in_app" "$(jget "$WORK/resp.json" 'd["error"]["message"]')"

code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "/api/v1/notification-channels" \
  '{"name":"","type":"webhook","settings":{"url":"https://example.invalid/h"}}')
expect_eq "name 为空 → 400" "400" "$code"
expect_contains "报错点出 name" "name" "$(jget "$WORK/resp.json" 'd["error"]["message"]')"

code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "/api/v1/notification-channels" \
  '{"name":"没主机","type":"smtp","settings":{"port":587,"tls":"starttls","from":"a@b.c","to":["a@b.c"]}}')
expect_eq "smtp 缺 host → 400" "400" "$code"
expect_contains "报错点出 settings.host" "settings.host" "$(jget "$WORK/resp.json" 'd["error"]["message"]')"

code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "/api/v1/notification-channels" \
  '{"name":"没端口","type":"smtp","settings":{"host":"127.0.0.1","tls":"starttls","from":"a@b.c","to":["a@b.c"]}}')
expect_eq "smtp 缺 port → 400" "400" "$code"
expect_contains "报错点出 settings.port" "settings.port" "$(jget "$WORK/resp.json" 'd["error"]["message"]')"

code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "/api/v1/notification-channels" \
  '{"name":"明文 SMTP","type":"smtp","settings":{"host":"127.0.0.1","port":25,"tls":"none","from":"a@b.c","to":["a@b.c"]}}')
expect_eq "smtp 要明文模式 → 400（没有这个选项）" "400" "$code"
expect_contains "报错点出 settings.tls" "settings.tls" "$(jget "$WORK/resp.json" 'd["error"]["message"]')"
expect_contains "报错说明只接受 starttls/implicit" "starttls" "$(jget "$WORK/resp.json" 'd["error"]["message"]')"

code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "/api/v1/notification-channels" \
  '{"name":"坏发件人","type":"smtp","settings":{"host":"127.0.0.1","port":587,"tls":"starttls","from":"nope","to":["a@b.c"]}}')
expect_eq "smtp 发件人不是地址 → 400" "400" "$code"
expect_contains "报错点出 settings.from" "settings.from" "$(jget "$WORK/resp.json" 'd["error"]["message"]')"

code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "/api/v1/notification-channels" \
  '{"name":"没 收 件 人","type":"smtp","settings":{"host":"127.0.0.1","port":587,"tls":"starttls","from":"a@b.c","to":[]}}')
expect_eq "smtp 没有收件人 → 400" "400" "$code"
expect_contains "报错点出 settings.to" "settings.to" "$(jget "$WORK/resp.json" 'd["error"]["message"]')"

code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "/api/v1/notification-channels" \
  '{"name":"有用户名没口令","type":"smtp","settings":{"host":"127.0.0.1","port":587,"tls":"starttls","from":"a@b.c","to":["a@b.c"],"username":"u"}}')
expect_eq "smtp 给用户名但没 secret_ref → 400" "400" "$code"
expect_contains "报错说清要先保存口令" "secret_ref" "$(jget "$WORK/resp.json" 'd["error"]["message"]')"

code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "/api/v1/notification-channels" \
  '{"name":"有口令没用户名","type":"smtp","secret_ref":"sec_x","settings":{"host":"127.0.0.1","port":587,"tls":"starttls","from":"a@b.c","to":["a@b.c"]}}')
expect_eq "smtp 给 secret_ref 但没用户名 → 400" "400" "$code"
expect_contains "报错点出 settings.username" "settings.username" "$(jget "$WORK/resp.json" 'd["error"]["message"]')"

code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "/api/v1/notification-channels" \
  '{"name":"明文回调","type":"webhook","settings":{"url":"http://127.0.0.1/hook"}}')
expect_eq "webhook 用 http → 400（告警正文不走明文）" "400" "$code"
expect_contains "报错点出 settings.url" "settings.url" "$(jget "$WORK/resp.json" 'd["error"]["message"]')"
expect_contains "报错说明只接受 https" "https" "$(jget "$WORK/resp.json" 'd["error"]["message"]')"

code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "/api/v1/notification-channels" \
  '{"name":"URL 里带口令","type":"webhook","settings":{"url":"https://user:pass@127.0.0.1/hook"}}')
expect_eq "webhook 地址带 userinfo → 400" "400" "$code"
expect_contains "报错说清令牌该用 secret_ref" "secret_ref" "$(jget "$WORK/resp.json" 'd["error"]["message"]')"

code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "/api/v1/notification-channels" \
  '{"name":"带片段","type":"webhook","settings":{"url":"https://127.0.0.1/hook#x"}}')
expect_eq "webhook 地址带 #片段 → 400" "400" "$code"
expect_contains "报错点出片段不会被发出去" "片段" "$(jget "$WORK/resp.json" 'd["error"]["message"]')"

code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "/api/v1/notification-channels" \
  '{"name":"重试太多次","type":"webhook","max_attempts":11,"settings":{"url":"https://127.0.0.1/hook"}}')
expect_eq "max_attempts 超过上限 → 400" "400" "$code"
expect_contains "报错点出 max_attempts" "max_attempts" "$(jget "$WORK/resp.json" 'd["error"]["message"]')"

code=$(channelsapi)
expect_eq "失败的创建一条都没落库（列表里仍只有面板内通道）" "1" "$(jget "$WORK/channels.json" 'len(d["data"])')"

code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" PATCH "/api/v1/notification-channels/999999" '{"name":"x","version":1}')
expect_eq "改不存在的通道 → 404" "404" "$code"
expect_eq "不存在的错误码" "NOT_FOUND" "$(jget "$WORK/resp.json" 'd["error"]["code"]')"
code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" DELETE "/api/v1/notification-channels/999999")
expect_eq "删不存在的通道 → 404" "404" "$code"
code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" DELETE "/api/v1/notification-channels/$INAPP_ID")
expect_eq "删面板内通道 → 400（改用停用）" "400" "$code"
expect_contains "报错说清改用 enabled=false" "enabled=false" "$(jget "$WORK/resp.json" 'd["error"]["message"]')"

echo
echo "== 3. B：秘密引用（口令与令牌只在代理库里） =="

SECRET_BODY="{\"purpose\":\"notification_password\",\"value\":\"$SMTP_SECRET\",\"reference\":\"accept-p133-smtp\"}"
code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "/api/v1/secrets" "$SECRET_BODY")
expect_eq "未再认证就上传秘密 → 403" "403" "$code"
expect_eq "再认证要求的错误码" "REAUTH_REQUIRED" "$(jget "$WORK/resp.json" 'd["error"]["code"]')"

code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "/api/v1/auth/reauth" '{"password":"wrong-password-1"}')
expect_eq "错误口令再认证 → 403" "403" "$code"
code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "/api/v1/auth/reauth" "{\"password\":\"$PASSWORD\"}")
expect_eq "正确口令再认证 → 200" "200" "$code"

code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "/api/v1/secrets" "$SECRET_BODY")
expect_eq "上传 SMTP 口令（用途 notification_password）201" "201" "$code"
SMTP_REF=$(jget "$WORK/resp.json" 'd["data"]["secret_ref"]')
expect_eq "上传返回的是引用不是明文" "notification_password" "$(jget "$WORK/resp.json" 'd["data"]["purpose"]')"
expect_not_contains "响应里没有口令本身" "$SMTP_SECRET" "$(cat "$WORK/resp.json")"

code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "/api/v1/secrets" \
  "{\"purpose\":\"notification_password\",\"value\":\"$HOOK_TOKEN\",\"reference\":\"accept-p133-hook\"}")
expect_eq "上传 webhook 令牌 201" "201" "$code"
HOOK_REF=$(jget "$WORK/resp.json" 'd["data"]["secret_ref"]')
expect_not_contains "响应里没有令牌本身" "$HOOK_TOKEN" "$(cat "$WORK/resp.json")"

# 诱饵：同样是秘密、同样能上传，但用途是备份仓库口令。后面用它证明**代理自己**
# 核对用途——否则面板只要把仓库口令的引用填进自己控制的 SMTP 通道，就能让代理
# 把仓库口令当 AUTH 口令发到自己的服务器上。
code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "/api/v1/secrets" \
  "{\"purpose\":\"backup_repository_password\",\"value\":\"$SMTP_SECRET\",\"reference\":\"accept-p133-decoy\"}")
expect_eq "上传用途不同的诱饵秘密 201" "201" "$code"
DECOY_REF=$(jget "$WORK/resp.json" 'd["data"]["secret_ref"]')

code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "/api/v1/secrets" \
  "{\"purpose\":\"no_such_purpose\",\"value\":\"$SMTP_SECRET\"}")
expect_eq "用途取闭集外的值 → 400" "400" "$code"
code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "/api/v1/secrets" \
  '{"purpose":"notification_password","value":""}')
expect_eq "空秘密 → 400" "400" "$code"

note "三份秘密引用都拿到了（值不打印：引用是“去解哪份密文”的线索，按秘密对待）"

if [ -z "$SMTP_REF" ] || [ -z "$HOOK_REF" ] || [ -z "$DECOY_REF" ]; then
  fail "秘密引用拿不到，后续投递断言无法进行" "$SMTP_REF / $HOOK_REF / $DECOY_REF"
  printf '通过 %d 项，失败 %d 项\n' "$PASSED" "$FAILED"
  exit 1
fi

echo
echo "== 4. C/D：保存不发送 + SMTP 测试发送 =="

SMTP_NAME="验收-邮件-显式"
SMTP_TO="ops@accept.test"
code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "/api/v1/notification-channels" \
  "{\"name\":\"$SMTP_NAME\",\"type\":\"smtp\",\"secret_ref\":\"$SMTP_REF\",\"settings\":{\"host\":\"127.0.0.1\",\"port\":$SMTP_STARTTLS_PORT,\"tls\":\"starttls\",\"from\":\"upanel@accept.test\",\"to\":[\"$SMTP_TO\"],\"username\":\"upanel\"}}")
expect_eq "建 STARTTLS 邮件通道 201" "201" "$code"
SMTP_ID=$(jget "$WORK/resp.json" 'd["data"]["id"]')
expect_eq "max_attempts 不给就用默认 3" "3" "$(jget "$WORK/resp.json" 'd["data"]["max_attempts"]')"
expect_eq "target 是收件人列表（人读形式，不含秘密）" "$SMTP_TO" "$(jget "$WORK/resp.json" 'd["data"]["target"]')"
expect_not_contains "创建通道的响应里没有口令" "$SMTP_SECRET" "$(cat "$WORK/resp.json")"

IMPL_NAME="验收-邮件-隐式"
code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "/api/v1/notification-channels" \
  "{\"name\":\"$IMPL_NAME\",\"type\":\"smtp\",\"settings\":{\"host\":\"127.0.0.1\",\"port\":$SMTP_TLS_PORT,\"tls\":\"implicit\",\"from\":\"upanel@accept.test\",\"to\":[\"implicit@accept.test\"]}}")
expect_eq "建隐式 TLS 邮件通道 201（无 AUTH）" "201" "$code"
IMPL_ID=$(jget "$WORK/resp.json" 'd["data"]["id"]')

WEBHOOK_NAME="验收-回调-正常"
code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "/api/v1/notification-channels" \
  "{\"name\":\"$WEBHOOK_NAME\",\"type\":\"webhook\",\"secret_ref\":\"$HOOK_REF\",\"settings\":{\"url\":\"https://127.0.0.1:$WEBHOOK_PORT/ok\"}}")
expect_eq "建 webhook 通道 201" "201" "$code"
HOOK_ID=$(jget "$WORK/resp.json" 'd["data"]["id"]')
expect_eq "webhook 的 target 是 URL" "https://127.0.0.1:$WEBHOOK_PORT/ok" "$(jget "$WORK/resp.json" 'd["data"]["target"]')"

# **保存不发送**：三次保存之后，本地接收器一个 SMTP 会话都没有、一个 HTTP 请求都没有，
# 投递账 0 行。这是 CONTRACTS §13 那条"保存绝不自动发"的真机证据。
expect_eq "保存通道没有触发任何 SMTP 连接" "0" "$(jsonl_eval "$SMTP_LOG" 'len(c)')"
expect_eq "保存通道没有触发任何 HTTP 请求" "0" "$(jsonl_eval "$HTTP_LOG" 'len(m)')"
expect_eq "保存通道没有写投递账" "0" "$(dcount '1 = 1')"

# PATCH 的版本语义（在一个真实通道上验，不是对着 999999）。
code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" PATCH "/api/v1/notification-channels/$SMTP_ID" '{"name":"改个名"}')
expect_eq "PATCH 不给 version → 400" "400" "$code"
expect_contains "报错说明 version 的用途" "version" "$(jget "$WORK/resp.json" 'd["error"]["message"]')"
code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" PATCH "/api/v1/notification-channels/$SMTP_ID" \
  '{"name":"改个名","version":1}')
expect_eq "PATCH 带正确 version 200" "200" "$code"
expect_eq "改名生效" "改个名" "$(jget "$WORK/resp.json" 'd["data"]["name"]')"
expect_eq "version 递增" "2" "$(jget "$WORK/resp.json" 'd["data"]["version"]')"
expect_eq "没提的字段沿用原值（PATCH 是叠加不是覆盖）" "$SMTP_TO" "$(jget "$WORK/resp.json" 'd["data"]["settings"]["to"][0]')"
code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" PATCH "/api/v1/notification-channels/$SMTP_ID" \
  '{"name":"用旧 version 改","version":1}')
expect_eq "旧 version PATCH → 409" "409" "$code"
expect_eq "冲突的错误码" "VERSION_CONFLICT" "$(jget "$WORK/resp.json" 'd["error"]["code"]')"
# 名字改回去，后面的断言与投递账里的通道名对得上。
api "$WORK/admin.jar" "$ADMIN_CSRF" PATCH "/api/v1/notification-channels/$SMTP_ID" \
  "{\"name\":\"$SMTP_NAME\",\"version\":2}" >/dev/null
expect_eq "改名之后投递账仍然是 0 行（PATCH 也不发送）" "0" "$(dcount '1 = 1')"

EXPECT_SMTP_SHA=$(test_body_sha "$SMTP_NAME" smtp "$SMTP_TO")

code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "/api/v1/notification-channels/$SMTP_ID/test")
expect_eq "显式测试发送（STARTTLS + AUTH）200" "200" "$code"
expect_eq "测试投递 kind=test" "test" "$(jget "$WORK/resp.json" 'd["data"]["kind"]')"
expect_eq "测试投递 state=sent" "sent" "$(jget "$WORK/resp.json" 'd["data"]["state"]')"
expect_eq "测试投递 attempts=1（只发一次）" "1" "$(jget "$WORK/resp.json" 'd["data"]["attempts"]')"
expect_eq "测试投递 last_status 是 SMTP 的 250" "250" "$(jget "$WORK/resp.json" 'd["data"]["last_status"]')"
expect_eq "测试投递有 sent_at" "true" "$(jget "$WORK/resp.json" 'd["data"]["sent_at"] is not None')"

expect_eq "接收器收到一封信" "1" "$(jsonl_eval "$SMTP_LOG" 'len([x for x in m if x["tls_mode"]=="starttls"])')"
expect_eq "信封标题是 [测试] 通道名" "[测试] $SMTP_NAME" "$(jsonl_eval "$SMTP_LOG" 'm[-1]["subject"]')"
expect_eq "信封发件人" "upanel@accept.test" "$(jsonl_eval "$SMTP_LOG" 'm[-1]["mail_from"]')"
expect_eq "信封收件人" "['$SMTP_TO']" "$(jsonl_eval "$SMTP_LOG" 'm[-1]["rcpt"]')"
expect_eq "AUTH 用的是那个用户名" "upanel" "$(jsonl_eval "$SMTP_LOG" 'm[-1]["auth_user"]')"
expect_eq "AUTH 用口令认证成功（口令经 secret_ref 由代理解出）" "true" "$(jsonl_eval "$SMTP_LOG" 'm[-1]["auth_ok"]')"
# 正文逐字对照：脚本按契约重写一遍，两边 sha256 相等才算"就是那句原文"。
expect_eq "正文与契约里的测试通知逐字一致（base64 解码后 sha256 相同）" "$EXPECT_SMTP_SHA" \
  "$(jsonl_eval "$SMTP_LOG" 'm[-1]["body_sha256"]')"

code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "/api/v1/notification-channels/$IMPL_ID/test")
expect_eq "显式测试发送（隐式 TLS）200" "200" "$code"
expect_eq "隐式 TLS 投递 sent" "sent" "$(jget "$WORK/resp.json" 'd["data"]["state"]')"
expect_eq "隐式 TLS 收到的那封信走的是 implicit" "1" \
  "$(jsonl_eval "$SMTP_LOG" 'len([x for x in m if x["tls_mode"]=="implicit"])')"
expect_eq "隐式通道没有配用户名，就没有 AUTH" "false" "$(jsonl_eval "$SMTP_LOG" 'm[-1]["auth_tried"]')"

# 对端 535：认证失败是**永久**拒绝（5xx）。测试发送只发一次，所以接收器只该多一个会话。
CONNS_BEFORE=$(jsonl_eval "$SMTP_LOG" 'len([x for x in c if x["tls_mode"]=="starttls"])')
write_control reject 500
code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "/api/v1/notification-channels/$SMTP_ID/test")
expect_eq "对端 535 时接口仍回 200（投递失败不是接口失败）" "200" "$code"
expect_eq "535 的投递 state=failed" "failed" "$(jget "$WORK/resp.json" 'd["data"]["state"]')"
expect_eq "535 的 last_status 是 535" "535" "$(jget "$WORK/resp.json" 'd["data"]["last_status"]')"
expect_contains "last_error 里是对端原话" "535" "$(jget "$WORK/resp.json" 'd["data"]["last_error"]')"
expect_contains "last_error 说清是哪一步失败" "认证失败" "$(jget "$WORK/resp.json" 'd["data"]["last_error"]')"
expect_not_contains "last_error 里没有口令" "$SMTP_SECRET" "$(jget "$WORK/resp.json" 'd["data"]["last_error"]')"
CONNS_AFTER=$(jsonl_eval "$SMTP_LOG" 'len([x for x in c if x["tls_mode"]=="starttls"])')
expect_eq "测试发送不重试：只多连了一次" "$((CONNS_BEFORE + 1))" "$CONNS_AFTER"
expect_eq "AUTH 被拒的信没被收下（显式 TLS 那一路仍然只有一封成功的）" "1" \
  "$(jsonl_eval "$SMTP_LOG" 'len([x for x in m if x["tls_mode"]=="starttls"])')"
write_control ok 500

echo
echo "== 5. D：webhook 测试发送（回执语义、令牌、不跟随重定向、8 秒上限） =="

EXPECT_HOOK_AUTH_SHA=$(printf 'Bearer %s' "$HOOK_TOKEN" | token_sha)
code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "/api/v1/notification-channels/$HOOK_ID/test")
expect_eq "webhook 测试发送 200" "200" "$code"
expect_eq "webhook 投递 sent" "sent" "$(jget "$WORK/resp.json" 'd["data"]["state"]')"
expect_eq "webhook last_status 是 200" "200" "$(jget "$WORK/resp.json" 'd["data"]["last_status"]')"
expect_eq "接收器收到一个请求" "1" "$(jsonl_eval "$HTTP_LOG" 'len(m)')"
expect_eq "请求打到配置的那条路径" "/ok" "$(jsonl_eval "$HTTP_LOG" 'm[-1]["path"]')"
expect_eq "令牌以 Authorization 头到达（比对 sha256，日志里没有明文）" "$EXPECT_HOOK_AUTH_SHA" \
  "$(jsonl_eval "$HTTP_LOG" 'm[-1]["authorization_sha256"]')"
expect_contains "请求体带 source=upanel" '"source":"upanel"' "$(jsonl_eval "$HTTP_LOG" 'm[-1]["body_head"]')"
expect_contains "请求体带测试标题" "[测试] $WEBHOOK_NAME" "$(jsonl_eval "$HTTP_LOG" 'm[-1]["body_head"]')"

# /always500：5xx 是"对端现在不行"，值得重试——但测试发送不重试，所以只该有一个请求。
code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "/api/v1/notification-channels" \
  "{\"name\":\"验收-回调-必失败\",\"type\":\"webhook\",\"max_attempts\":2,\"enabled\":false,\"settings\":{\"url\":\"https://127.0.0.1:$WEBHOOK_PORT/always500\"}}")
expect_eq "建（停用的）必失败通道 201" "201" "$code"
FAIL_ID=$(jget "$WORK/resp.json" 'd["data"]["id"]')
FAIL_VER=$(jget "$WORK/resp.json" 'd["data"]["version"]')
code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "/api/v1/notification-channels/$FAIL_ID/test")
expect_eq "对端 500 时接口仍回 200" "200" "$code"
expect_eq "500 的 state=failed" "failed" "$(jget "$WORK/resp.json" 'd["data"]["state"]')"
expect_eq "500 的 last_status 是 500" "500" "$(jget "$WORK/resp.json" 'd["data"]["last_status"]')"
expect_contains "last_error 里是对端原话" "boom" "$(jget "$WORK/resp.json" 'd["data"]["last_error"]')"
expect_eq "测试发送不重试：/always500 只被打了一次" "1" \
  "$(jsonl_eval "$HTTP_LOG" 'len([x for x in m if x["path"]=="/always500"])')"

# /redirect：代理不跟随重定向（跟随等于让对端把我们的告警正文引到一个没配过的地址）。
code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "/api/v1/notification-channels" \
  "{\"name\":\"验收-回调-重定向\",\"type\":\"webhook\",\"enabled\":false,\"settings\":{\"url\":\"https://127.0.0.1:$WEBHOOK_PORT/redirect\"}}")
expect_eq "建重定向通道 201" "201" "$code"
REDIR_ID=$(jget "$WORK/resp.json" 'd["data"]["id"]')
code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "/api/v1/notification-channels/$REDIR_ID/test")
expect_eq "302 → 投递 failed" "failed" "$(jget "$WORK/resp.json" 'd["data"]["state"]')"
expect_eq "302 的 last_status 是 302" "302" "$(jget "$WORK/resp.json" 'd["data"]["last_status"]')"
expect_contains "last_error 说明不接受重定向" "重定向" "$(jget "$WORK/resp.json" 'd["data"]["last_error"]')"
expect_eq "没有跟随重定向：只打了一次 /redirect" "1" \
  "$(jsonl_eval "$HTTP_LOG" 'len([x for x in m if x["path"]=="/redirect"])')"

# /slow：单次投递总时长上限 8 秒。对端 12 秒不回，投递必须自己结束，而且不能让面板
# 先把它掐掉（面板的调用上限是 10 秒）。
code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "/api/v1/notification-channels" \
  "{\"name\":\"验收-回调-慢\",\"type\":\"webhook\",\"enabled\":false,\"settings\":{\"url\":\"https://127.0.0.1:$WEBHOOK_PORT/slow\"}}")
expect_eq "建慢通道 201" "201" "$code"
SLOW_ID=$(jget "$WORK/resp.json" 'd["data"]["id"]')
SLOW_T0=$(now_unix)
code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "/api/v1/notification-channels/$SLOW_ID/test")
SLOW_ELAPSED=$(( $(now_unix) - SLOW_T0 ))
expect_eq "对端 12 秒不回 → 投递 failed" "failed" "$(jget "$WORK/resp.json" 'd["data"]["state"]')"
# 两种都收：product 用的是带 8 秒 deadline 的 context，Go 打出来的原话是
# "context deadline exceeded"；http.Client 自带 Timeout 时才是 "Client.Timeout"。
# 契约（CONTRACTS §13.3）只要求"超时算可重试的失败并说清原因"，没规定 Go 的措辞。
SLOW_ERR=$(jget "$WORK/resp.json" 'd["data"]["last_error"]')
case $SLOW_ERR in
  *"Client.Timeout"*|*"context deadline exceeded"*) pass "last_error 说明是超时（$SLOW_ERR）" ;;
  *) fail "last_error 说明是超时" "实际: $SLOW_ERR" ;;
esac
if [ "$SLOW_ELAPSED" -ge 7 ] && [ "$SLOW_ELAPSED" -le 10 ]; then
  pass "投递在 8 秒上限上结束（实测 ${SLOW_ELAPSED} 秒，面板的调用上限是 10 秒）"
else
  fail "投递在 8 秒上限上结束" "实测 ${SLOW_ELAPSED} 秒，期望 7—10 秒之间"
fi

# 用途不对的秘密：面板不知道用途（它只存引用），**代理自己**必须拒绝。否则"不能读明文"
# 就能被绕成"把明文发给我"。
code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "/api/v1/notification-channels" \
  "{\"name\":\"验收-用途不对\",\"type\":\"smtp\",\"enabled\":false,\"secret_ref\":\"$DECOY_REF\",\"settings\":{\"host\":\"127.0.0.1\",\"port\":$SMTP_STARTTLS_PORT,\"tls\":\"starttls\",\"from\":\"upanel@accept.test\",\"to\":[\"ops@accept.test\"],\"username\":\"upanel\"}}")
expect_eq "面板侧不看秘密用途（它只存引用）→ 建通道 201" "201" "$code"
DECOY_ID=$(jget "$WORK/resp.json" 'd["data"]["id"]')
DECOY_CONNS=$(jsonl_eval "$SMTP_LOG" 'len([x for x in c if x["tls_mode"]=="starttls"])')
code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "/api/v1/notification-channels/$DECOY_ID/test")
expect_eq "用途不对时投递 failed" "failed" "$(jget "$WORK/resp.json" 'd["data"]["state"]')"
expect_contains "投递账里说明原因就是用途" "notification_password" "$(jget "$WORK/resp.json" 'd["data"]["last_error"]')"
expect_eq "用途不对的连接根本没建起来（代理在核用途时就拒了）" "$DECOY_CONNS" \
  "$(jsonl_eval "$SMTP_LOG" 'len([x for x in c if x["tls_mode"]=="starttls"])')"

code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "/api/v1/notification-channels/$INAPP_ID/test")
expect_eq "in_app 测试发送 → 400（没有外部动作可做）" "400" "$code"
expect_eq "in_app 的错误码" "VALIDATION_ERROR" "$(jget "$WORK/resp.json" 'd["error"]["code"]')"
expect_contains "报错说清它的投递就是收件箱里的一行" "收件箱" "$(jget "$WORK/resp.json" 'd["error"]["message"]')"

# 这三条只为了验测试发送，不参与事件扇出：删掉它们（同时把 delete 审计与投递账的
# ON DELETE SET NULL 走一遍）。
for pair in "$REDIR_ID:重定向" "$SLOW_ID:慢" "$DECOY_ID:用途不对"; do
  id=${pair%%:*}
  label=${pair#*:}
  code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" DELETE "/api/v1/notification-channels/$id")
  expect_eq "删除测试通道（$label）200" "200" "$code"
done
expect_eq "删掉的通道在投递账里留下历史（channel_id 置空但行还在）" "3" \
  "$(dcount 'channel_id IS NULL')"

echo
echo "== 6. E：告警驱动、退避重试、上限、停用判死（受控种入） =="

# 受控序列就位：一条不存在的网卡，由 seed.py 每 5 秒写一个 rx_bytes_per_sec 样本。
write_seeder
phase high
start_seed 40
seed_deadline=$(( $(now_unix) + 25 ))
SEEDED=0
while :; do
  SEEDED=$(sqlq "SELECT COUNT(*) FROM metric_samples
                  WHERE scope = 'interface' AND target_id = '$SEED_TARGET'
                    AND metric = '$SEED_METRIC' AND resolution = 'raw'")
  case ${SEEDED:-} in ''|*[!0-9]*) SEEDED=0 ;; esac
  [ "$SEEDED" -ge 5 ] && break
  [ "$(now_unix)" -ge "$seed_deadline" ] && break
  sleep 1
done
if [ "$SEEDED" -ge 5 ]; then
  pass "受控序列就位（$SEED_TARGET/$SEED_METRIC 已写入 $SEEDED 个样本）"
else
  fail "受控序列就位" "只写入 $SEEDED 个样本，见 $WORK/logs/seed.log"
fi

# ---- 6.1 静默期间的事件一条投递都不入队 ----
code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" PUT "/api/v1/alerts/silence" \
  "{\"starts_at\":\"$(iso_of $(( $(now_unix) - 60 )))\",\"ends_at\":\"$(iso_of $(( $(now_unix) + 900 )))\",\"reason\":\"验收：静默期间不发通知\"}")
expect_eq "开启维护静默 200" "200" "$code"

code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "/api/v1/alerts/rules" \
  "{\"name\":\"静默期间的告警\",\"metric\":\"$SEED_METRIC\",\"target_id\":\"$SEED_TARGET\",\"comparison\":\"gte\",\"threshold\":1000,\"recovery_threshold\":500,\"duration_seconds\":30,\"cooldown_seconds\":0}")
expect_eq "建静默期间的规则 201" "201" "$code"
SIL_RULE=$(jget "$WORK/resp.json" 'd["data"]["id"]')
if wait_events "$SIL_RULE" 1 120; then
  pass "静默期间规则照常触发（静默只压通知，不压判定）"
else
  fail "静默期间规则照常触发" "等了 120 秒也没有事件，见 $WEB_LOG"
fi
SIL_EVENT=$(sqlq "SELECT id FROM alert_events WHERE rule_id = $SIL_RULE ORDER BY id DESC LIMIT 1")
expect_eq "这条事件被标成 silenced" "1" \
  "$(sqlq "SELECT silenced FROM alert_events WHERE id = ${SIL_EVENT:-0}")"
expect_eq "静默事件一条投递都没入队" "0" "$(dcount "event_id = ${SIL_EVENT:-0}")"

code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" PUT "/api/v1/alerts/silence" '{}')
expect_eq "清除静默 200" "200" "$code"
code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" DELETE "/api/v1/alerts/rules/$SIL_RULE")
expect_eq "删掉静默期间的规则 200" "200" "$code"
expect_eq "收口静默事件也不补发投递" "0" "$(dcount "event_id = ${SIL_EVENT:-0}")"

# ---- 6.2 触发 → 每个启用通道一行；先坏后好的通道第二次成功；用尽上限的判 failed ----
code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" PATCH "/api/v1/notification-channels/$FAIL_ID" \
  "{\"enabled\":true,\"version\":$FAIL_VER}")
expect_eq "启用必失败通道 200" "200" "$code"
code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "/api/v1/notification-channels" \
  "{\"name\":\"验收-回调-先坏后好\",\"type\":\"webhook\",\"max_attempts\":3,\"settings\":{\"url\":\"https://127.0.0.1:$WEBHOOK_PORT/flaky\"}}")
expect_eq "建先坏后好的通道 201" "201" "$code"
FLAKY_ID=$(jget "$WORK/resp.json" 'd["data"]["id"]')
write_control ok 500

code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "/api/v1/alerts/rules" \
  "{\"name\":\"网卡入向流量持续偏高\",\"metric\":\"$SEED_METRIC\",\"target_id\":\"$SEED_TARGET\",\"comparison\":\"gte\",\"threshold\":1000,\"recovery_threshold\":500,\"duration_seconds\":30,\"cooldown_seconds\":0}")
expect_eq "建 R1 201" "201" "$code"
R1=$(jget "$WORK/resp.json" 'd["data"]["id"]')
if wait_events "$R1" 1 120; then
  pass "R1 持续够 30 秒后触发"
else
  fail "R1 触发" "等了 120 秒也没有事件，见 $WEB_LOG"
fi
E1=$(sqlq "SELECT id FROM alert_events WHERE rule_id = $R1 ORDER BY id DESC LIMIT 1")
# E1 会被拼进后面的每一条 SQL。查不到（或查询出错）时把它落成 0：那些断言本来就
# 会因为"没有这一行"而失败，不该再连带出二十条 "near \"<\": syntax error"。
is_num "$E1" || E1=0
note "事件 E1=$E1，通道：in_app=$INAPP_ID smtp=$SMTP_ID fail=$FAIL_ID flaky=$FLAKY_ID"

# 必失败通道：第一次尝试落在 pending，退避到 30 秒之后。
#
# **先查它，再查别的通道。** next_attempt_at 只能和"现在"比，而别的等待（SMTP 那封
# 信、in_app 那行）可能耗掉二十几秒——那之后再来量"还剩多少秒"，量到的是 12 秒，
# 看着像退避算错了，其实只是我们来得晚。这条断言的窗口只有 30 秒，就得第一个量。
if wait_delivery "必失败通道第一次尝试" "event_id = ${E1:-0} AND channel_id = $FAIL_ID AND kind = 'fired'" "attempts" 1 40; then
  pass "必失败通道做了第一次尝试"
else
  fail "必失败通道做了第一次尝试" "$(dlist "event_id = ${E1:-0} AND channel_id = $FAIL_ID")"
fi
FAIL_NEXT=$(dfield "event_id = ${E1:-0} AND channel_id = $FAIL_ID AND kind = 'fired'" "next_attempt_at")
FAIL_STATE=$(dfield "event_id = ${E1:-0} AND channel_id = $FAIL_ID AND kind = 'fired'" "state")
NOW=$(now_unix)
expect_eq "第一次失败后状态是 pending（还在等重试）" "pending" "$FAIL_STATE"
# 退避时长用**账本内部的两个时刻相减**来证，不和我们到达的时机比。
#
# 上一版量的是"此刻还剩多少秒"，要求 20..35，结果量到 19 判了失败——产品没错，
# 是我们把"我们到达的时刻"当成了"退避的长度"。so 换个不漂的量法：首投的
# next_attempt_at 减 created_at，只允许一个调度节拍的误差（见下面的 30..35）。
#
# created_at 是 ISO 文本（'2026-10-26T09:03:43Z'），strftime 不认尾巴上的 Z，会把
# 它算成 NULL —— 先捋掉再换成 epoch。next_attempt_at 在"还没排过"和"已经收口"时
# 都是 0，所以 COALESCE 兜一下，再靠 is_num 挡住非法值。
if is_num "$FAIL_NEXT"; then
  DELAY=$((FAIL_NEXT - NOW))
  BACKOFF=$(sqlq "SELECT CAST(next_attempt_at AS INTEGER) - CAST(strftime('%s', replace(created_at, 'Z', '')) AS INTEGER)
                  FROM notification_deliveries
                  WHERE event_id = ${E1:-0} AND channel_id = $FAIL_ID AND kind = 'fired'")
  # 30..35 这个区间不是"随便放点余量"：产品按**这一轮调度的时刻 + 30 秒**排下一次
  # （dispatch.go 的 `now` 取自 DispatchOnce 开头，deliveries.go 的
  # `next = now.Add(retryDelay(attempt))`，retryBaseDelay = 30 秒），而调度节拍是
  # dispatchInterval = 5 秒——行是入队时立刻到期的，所以它一定落在入队后的第一个节拍上，
  # 也就是 created_at 之后 0..5 秒。于是下界的 30 秒是契约要的"不早于 30 秒"，上界的
  # 35 秒正好是"再多一个节拍"：区间宽度就等于节拍本身，没有别的空隙可以藏。
  # 量到 35 说明这一行入队后正好等到下一个节拍才被发出（上一版量到 19 秒，是因为拿
  # "还剩多少秒"去比 20..35 的窗口——那是我们到达的时刻，不是退避的长度）。
  if is_num "$BACKOFF" && [ "$BACKOFF" -ge 30 ] && [ "$BACKOFF" -le 35 ]; then
    pass "退避时间是 30 秒（next_attempt_at - created_at = $BACKOFF，节拍 5 秒内，此刻还剩 ${DELAY} 秒）"
  else
    fail "退避时间是 30 秒" "next_attempt_at - created_at = '$BACKOFF'（应为 30，最多再加一个 5 秒节拍；此刻还剩 ${DELAY} 秒）"
  fi
else
  fail "退避时间是 30 秒" "next_attempt_at 是 '$FAIL_NEXT'"
fi
expect_contains "失败原因是对端原话（500）" "boom" \
  "$(dfield "event_id = ${E1:-0} AND channel_id = $FAIL_ID AND kind = 'fired'" last_error)"

# 先坏后好：翻成 200，下一次退避到点就该成功。
write_control ok 200
if wait_delivery "先坏后好通道重试成功" "event_id = ${E1:-0} AND channel_id = $FLAKY_ID AND kind = 'fired'" "state" sent 90; then
  pass "先坏后好的通道第二次尝试送达（退避到点真的会重试）"
else
  fail "先坏后好的通道第二次尝试送达" "$(dlist "event_id = ${E1:-0} AND channel_id = $FLAKY_ID")"
fi
expect_eq "先坏后好的 attempts=2（一次失败 + 一次成功）" "2" \
  "$(dfield "event_id = ${E1:-0} AND channel_id = $FLAKY_ID AND kind = 'fired'" 'attempts')"
if wait_delivery "必失败通道用尽上限" "event_id = ${E1:-0} AND channel_id = $FAIL_ID AND kind = 'fired'" "state" failed 90; then
  pass "必失败通道在 max_attempts 用尽后判 failed"
else
  fail "必失败通道在 max_attempts 用尽后判 failed" "$(dlist "event_id = ${E1:-0} AND channel_id = $FAIL_ID")"
fi
expect_eq "判死时 attempts 正好等于 max_attempts" "2|2" \
  "$(dfield "event_id = ${E1:-0} AND channel_id = $FAIL_ID AND kind = 'fired'" "attempts || '|' || max_attempts")"

# 邮件通道与收件箱这两条不急，放在时间敏感的退避断言之后量：它们只证明"最终怎样"，
# 而 next_attempt_at 要在失败发生后的半分钟内读才有意义。
if wait_delivery "smtp 的 fired 投递" "event_id = ${E1:-0} AND channel_id = $SMTP_ID AND kind = 'fired'" "state" sent 40; then
  pass "触发事件：邮件通道已送达"
else
  fail "触发事件：邮件通道已送达" "$(dlist "event_id = ${E1:-0} AND channel_id = $SMTP_ID")"
fi
expect_eq "触发事件：面板内通知写入即 sent" "sent" \
  "$(dfield "event_id = ${E1:-0} AND channel_id = $INAPP_ID AND kind = 'fired'" 'state')"
# attempts=0 是对的：in_app 一次都没"尝试"过——它不出门，写进投递账就是到达。
# 能证明它不重试的是 max_attempts=1 加上 attempts 停在 0。
expect_eq "面板内通知不重试（attempts=0, max_attempts=1）" "0|1" \
  "$(dfield "event_id = ${E1:-0} AND channel_id = $INAPP_ID AND kind = 'fired'" "attempts || '|' || max_attempts")"

# 去重：同一事件在同一通道上每种 kind 只有一行（重试是改这一行，不是再插一行）。
expect_eq "同事件同通道同 kind 只有一行（去重）" "0" \
  "$(sqlq "SELECT COUNT(*) FROM (SELECT channel_id, kind, COUNT(*) c FROM notification_deliveries
            WHERE event_id = ${E1:-0} GROUP BY channel_id, kind HAVING c > 1)")"

# ---- 6.3 恢复事件同样发；停用通道的 pending 判 failed 且不计入 attempts ----
phase low
resolved_deadline=$(( $(now_unix) + 120 ))
RESOLVED=0
while :; do
  RESOLVED=$(sqlq "SELECT COUNT(*) FROM alert_events WHERE id = ${E1:-0} AND state = 'resolved'")
  case ${RESOLVED:-} in ''|*[!0-9]*) RESOLVED=0 ;; esac
  [ "$RESOLVED" -ge 1 ] && break
  [ "$(now_unix)" -ge "$resolved_deadline" ] && break
  sleep 3
done
if [ "$RESOLVED" -ge 1 ]; then
  pass "回落到恢复线内并持续够 30 秒后收口"
else
  fail "回落到恢复线内并持续够 30 秒后收口" "等了 120 秒事件仍是 firing"
fi
# 停用必失败通道：pending 的恢复投递下一次调度就该判死，且 attempts 不涨。
#
# 这里的窗口同样是 30 秒：它的第一次尝试失败之后 30 秒就会重试，重试就失败、就
# attempts+1，那时候再停用已经晚了——上一版正是因为先把 SMTP 那封恢复信等完
# （最多 40 秒）才来停用，量到的是"用尽上限判死"，而不是"通道停用判死"。
# 所以：**等到它正在等退避的那一刻**（state=pending 且 next_attempt_at 还在未来），
# 立刻记下 attempts，立刻停用。这样停用一定落在两次尝试之间。
ABANDON_PHASE=""
ABANDON_READY=no
ABANDON_ATTEMPTS=""
abandon_deadline=$(( $(now_unix) + 120 ))
while :; do
  ABANDON_PHASE=$(dfield "event_id = ${E1:-0} AND channel_id = $FAIL_ID AND kind = 'resolved'" \
    "state || '|' || attempts || '|' || COALESCE(next_attempt_at, 0)")
  STATE_NOW=${ABANDON_PHASE%%|*}
  REST=${ABANDON_PHASE#*|}
  ATTEMPTS_NOW=${REST%%|*}
  NEXT_NOW=${REST#*|}
  # 已经试过一次、而且下一次排在未来（留 3 秒余量）——这才是能下手的窗口。
  if [ "$STATE_NOW" = "pending" ] && is_num "$ATTEMPTS_NOW" && [ "$ATTEMPTS_NOW" -ge 1 ] \
     && is_num "$NEXT_NOW" && [ "$NEXT_NOW" -gt $(( $(now_unix) + 3 )) ]; then
    ABANDON_READY=yes
    break
  fi
  # 它自己先用尽上限判死了：这时候再停用已经证明不了"停用判死"，如实记下来。
  if [ "$STATE_NOW" = "failed" ]; then
    ABANDON_READY=late
    break
  fi
  [ "$(now_unix)" -ge "$abandon_deadline" ] && break
  sleep 1
done
ABANDON_ATTEMPTS=$ATTEMPTS_NOW
note "停用前：必失败通道的恢复投递是 $ABANDON_PHASE（等到退避窗口：$ABANDON_READY）"
# 版本从**现场读的**列表里取：前面 PATCH enabled=true 已经把它推到 2，用开局那份
# channels.json 会拿到 1，然后吃一个 409——那是脚本的错，不是产品的。
#
# 这里每一步都要断言。上一版写成了 chan_version "$WORK/channels.json" "$FAIL_ID"，
# 而它只吃一个参数（文件名是写死的），于是 id 变成了那句路径、int() 抛异常、
# 输出被 2>/dev/null 吞掉，FAIL_VER_NOW 是空串，body 拼成 {"enabled":false,"version":}
# ——产品回 400 VALIDATION_ERROR，停用从未发生，后面两条断言连坐误报成"产品把停用
# 判成了 boom"。空版本号不许发出去。
CH_CODE=$(channelsapi)
if [ "$CH_CODE" = "200" ]; then
  pass "停用前重新读通道列表（拿到现场版本号）"
else
  fail "停用前重新读通道列表" "HTTP $CH_CODE：$(head -c 200 "$WORK/channels.json" 2>/dev/null)"
fi
FAIL_VER_NOW=$(chan_version "$FAIL_ID")
if is_num "$FAIL_VER_NOW"; then
  pass "从现场列表里取到必失败通道的版本号（$FAIL_VER_NOW）"
else
  fail "从现场列表里取到必失败通道的版本号" "取到的是 '$FAIL_VER_NOW'，不能拼进 PATCH 的 body"
fi
DIS_CODE="not-sent"
if is_num "$FAIL_VER_NOW"; then
  DIS_CODE=$(api "$WORK/admin.jar" "$ADMIN_CSRF" PATCH "/api/v1/notification-channels/$FAIL_ID" \
    "{\"enabled\":false,\"version\":$FAIL_VER_NOW}")
fi
expect_eq "停用必失败通道的 PATCH 成功" "200" "$DIS_CODE"
if [ "$DIS_CODE" = "200" ]; then
  expect_eq "停用生效（enabled=false）" "false" "$(jget "$WORK/resp.json" 'd["data"]["enabled"]')"
else
  fail "停用生效（enabled=false）" "PATCH 没成功：$(head -c 300 "$WORK/resp.json" 2>/dev/null)"
fi
ABANDON_DEADLINE=$(( $(now_unix) + 30 ))
ABANDONED=""
while :; do
  ABANDONED=$(dfield "event_id = ${E1:-0} AND channel_id = $FAIL_ID AND kind = 'resolved'" "state || '|' || attempts")
  [ "$ABANDONED" = "failed|${ABANDON_ATTEMPTS:-x}" ] && break
  [ "$(now_unix)" -ge "$ABANDON_DEADLINE" ] && break
  sleep 2
done
# 没赶上窗口（或者停用根本没发出去）时，这两条不能假装通过，也不能把"用尽上限"
# 说成"停用判死"——那是两件事。如实报"没赶上"，并带上当时看到的样子。
if [ "$ABANDON_READY" = "yes" ] && [ "$DIS_CODE" = "200" ]; then
  expect_eq "停用后 pending 判 failed，attempts 仍是 $ABANDON_ATTEMPTS（一次都没多算）" \
    "failed|${ABANDON_ATTEMPTS:-x}" "$ABANDONED"
  expect_contains "判死原因说是通道停用" "停用" \
    "$(dfield "event_id = ${E1:-0} AND channel_id = $FAIL_ID AND kind = 'resolved'" last_error)"
else
  fail "停用后 pending 判 failed，attempts 仍是 ${ABANDON_ATTEMPTS:-?}（一次都没多算）" \
    "没赶上退避窗口（ready=$ABANDON_READY，停用 PATCH=$DIS_CODE），实际 $ABANDONED"
  fail "判死原因说是通道停用" \
    "没赶上退避窗口（ready=$ABANDON_READY，停用 PATCH=$DIS_CODE），原因未验证：$(dfield "event_id = ${E1:-0} AND channel_id = $FAIL_ID AND kind = 'resolved'" last_error)"
fi

# 邮件通道那封恢复信不急，放在停用之后量：它只证明"最终送达"，没有 30 秒窗口。
if wait_delivery "smtp 的 resolved 投递" "event_id = ${E1:-0} AND channel_id = $SMTP_ID AND kind = 'resolved'" "state" sent 40; then
  pass "恢复事件也发出去（它对应的通道已送达）"
else
  fail "恢复事件也发出去" "$(dlist "event_id = ${E1:-0} AND channel_id = $SMTP_ID")"
fi
# LIKE 在 SQLite 里回的是 0/1，不是 true/false。要一个说得清的布尔就自己翻译。
expect_eq "恢复事件的正文说清是恢复" "true" \
  "$(sqlq "SELECT CASE WHEN body LIKE '%恢复时间%' THEN 'true' ELSE 'false' END
            FROM notification_deliveries
            WHERE event_id = ${E1:-0} AND channel_id = $SMTP_ID AND kind = 'resolved'")"

# 停用的通道不再参与扇出：再触发一次只看它有没有新行。
expect_eq "停用通道上没有更多的行（停用之后不再入队）" "2" \
  "$(dcount "channel_id = $FAIL_ID AND event_id IS NOT NULL")"

echo
echo "== 7. F：审计、日志卫生与“秘密没有落进任何不该落的地方” =="

audit_count() { sqlq "SELECT COUNT(*) FROM audit_events WHERE action = '$1'"; }
for action in notification.channel.create notification.channel.update \
               notification.channel.delete notification.channel.test \
               notification.channel.manage; do
  n=$(audit_count "$action")
  case ${n:-} in ''|*[!0-9]*) n=0 ;; esac
  if [ "$n" -ge 1 ]; then
    pass "审计里有 $action（$n 条）"
  else
    fail "审计里有 $action" "一条都没有"
  fi
done

AUDIT_DETAILS=$(sqlq "SELECT redacted_details FROM audit_events
                       WHERE action = 'notification.channel.create' ORDER BY id DESC LIMIT 1")
expect_contains "审计记了通道类型" '"type"' "$AUDIT_DETAILS"
expect_contains "审计里 secret_ref 被红字化（Q423 已知限制）" '"[redacted]"' "$AUDIT_DETAILS"
expect_not_contains "审计里没有 SMTP 口令" "$SMTP_SECRET" "$AUDIT_DETAILS"
expect_not_contains "审计里没有 webhook 令牌" "$HOOK_TOKEN" "$AUDIT_DETAILS"

# 秘密可能落进的地方：整个工作目录（面板库含 -wal/-shm、代理库、两端日志、接收器
# 日志、种入日志都在里面）。库文件是二进制的，用 grep -a 直接扫字节。
#
# **白名单只有两个刻意为之的 0600 文件**：接收器要拿它核 AUTH 的期望口令，
# 以及 curl 的请求体（写完即被下一次调用覆盖）。除此之外任何地方出现明文都算失败。
WHITELIST="$RECV_DIR/smtp_password $WORK/req.json"
LEAKS=""
for pattern in "$SMTP_SECRET:smtp" "$HOOK_TOKEN:token"; do
  value=${pattern%:*}
  label=${pattern##*:}
  for hit in $(grep -rla -- "$value" "$WORK" "$AGENT_DB" "$AGENT_DB-wal" "$AGENT_DB-shm" 2>/dev/null); do
    case " $WHITELIST " in
      *" $hit "*) continue ;;
    esac
    LEAKS="$LEAKS $label:$hit"
  done
done
if [ -z "$LEAKS" ]; then
  pass "口令与令牌没有出现在面板库/代理库/任何日志里（扫了整个 $WORK）"
else
  fail "口令与令牌没有出现在面板库/代理库/任何日志里" "$LEAKS"
fi
if [ "$(stat -c %a "$RECV_DIR/smtp_password")" = "600" ]; then
  pass "接收器读的口令文件是 0600"
else
  fail "接收器读的口令文件是 0600" "$(stat -c %a "$RECV_DIR/smtp_password")"
fi

if grep -qa 'panic:' "$WEB_LOG" 2>/dev/null; then
  fail "web 日志没有 panic" "$(grep -a -m1 'panic:' "$WEB_LOG")"
else
  pass "web 日志没有 panic"
fi
if grep -qa 'panic:' "$AGENT_LOG" 2>/dev/null; then
  fail "代理日志没有 panic" "$(grep -a -m1 'panic:' "$AGENT_LOG")"
else
  pass "代理日志没有 panic"
fi
if grep -qa 'upanel: 代理返回 500' "$WEB_LOG" 2>/dev/null; then
  fail "web 日志没有 500 的代理错误" "$(grep -a -m1 '代理返回 500' "$WEB_LOG")"
else
  pass "web 日志没有“代理返回 500”"
fi

echo
echo "== 8. 结论 =="
note "投递账（全部行，最后 20 条）："
dlist "1 = 1" | tail -20
printf '\n通过 %d 项，失败 %d 项\n' "$PASSED" "$FAILED"
if [ "$FAILED" -ne 0 ]; then
  printf '失败项：%s\n' "$FAILED_NAMES"
  printf '日志：%s（web）%s（代理）%s（接收器）\n' "$WEB_LOG" "$AGENT_LOG" "$RECV_LOG"
  exit 1
fi
printf '全部通过。日志与接收器记录在 %s\n' "$WORK"
exit 0
