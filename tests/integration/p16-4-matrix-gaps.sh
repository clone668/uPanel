#!/bin/sh
# P16.4 收尾：把验收矩阵里三条"部分"中的两条补成真机读数。
#
#   1) **APP-02**：探测上游回 `302 Location: ...` 时，面板记录的是什么、有没有跟着跳。
#      这一条以前只有"客户端不跟随重定向"这句代码属性 —— 本轮造两个真上游：
#        - 回落环金丝雀：如果面板跟着跳了，金丝雀服务器会收到请求；
#        - 回云元数据地址：要求它照样只记 302，不去连 169.254.169.254。
#   2) **FILE-02**：
#        - **ENOSPC**：一块 16 MiB 的 tmpfs 当可写根，声明 8 MiB（声明时确实放得下），
#          然后在"声明之后、传字节之前"把它填满 —— 写的时候真的撞 ENOSPC。
#          断言：413 + VALIDATION_ERROR + 消息是"空间不足"，暂存的 .part 被删、目标没被创建、
#          上传记录留在 failed（清理暂存 ≠ 假装没发生）。
#        - **断流**：传一半掐断连接。断言：这条上传进 failed、暂存被清、目标不存在。
#        - **超大上传真的推一遍**：2 GiB，同时按 0.5 秒采样代理 RSS，证明是流式有界，
#          而不是"先吃进内存再写盘"。落位后 sha256 必须一致。
#
# 用法（root）：
#   sudo BIN_DIR=/var/tmp/upanel-bin sh tests/integration/p16-4-matrix-gaps.sh
#
# 环境变量：PORT（默认 19490）、WORK、BIN_DIR、KEEP_BIG（=1 保留 2 GiB 载荷便于复查）。
#
# 退出码：0 全部通过；1 有失败项；2 环境不满足。
#
# 明确不验（NOT RUN，不在这里假装通过）：
#   - **DISK-02 的 LVM/RAID 根盘**：要一台根盘在 LVM 上的机器（`P00-VM-C`），本脚本不覆盖。
#   - **并发编辑真竞态**：单机顺序脚本制造不出可靠的并发窗口（与 P06.2 同一取舍）。

set -u

BIN_DIR=${BIN_DIR:-/var/tmp/upanel-bin}

if [ "$(id -u)" -ne 0 ]; then
  printf '本脚本必须以 root 运行：sudo BIN_DIR=%s sh %s\n' "$BIN_DIR" "$0" >&2
  exit 2
fi

AGENT="$BIN_DIR/upanel-agent"
WEB="$BIN_DIR/upanel-web"
CTL="$BIN_DIR/upanelctl"

for f in "$AGENT" "$WEB" "$CTL"; do
  if [ ! -x "$f" ]; then
    printf '缺少可执行文件：%s\n' "$f" >&2
    exit 2
  fi
done

WORK=${WORK:-/var/tmp/upanel-accept-p16-4}
PAYLOADS=$WORK/payloads
LOGS=$WORK/logs
RUN_DIR=/run/upanel
SOCK=$RUN_DIR/agent.sock
SYS_CFG=/etc/upanel/upanel.json
AGENT_DB_DIR=/var/lib/upanel-agent
AGENT_DB=$AGENT_DB_DIR/agent.db
KEY_FILE=/etc/upanel/agent.key
WEB_DB_DIR=$WORK/webdata
WEB_DB=$WEB_DB_DIR/upanel.db
PORT=${PORT:-19490}
BASE="http://127.0.0.1:$PORT"
ORIGIN="$BASE"
WEB_USER=upanel-web
PASSWORD='vm-correct-horse-9'

# 三个被测上游：302→金丝雀、302→云元数据、金丝雀本身。
P_REDIR=$((PORT + 1))
P_META=$((PORT + 2))
P_CANARY=$((PORT + 3))

ROOT_RW=$WORK/rw
ROOT_SMALL=$WORK/small          # tmpfs 挂载点（ENOSPC 现场）
# 为什么要 128 MiB 而不是"刚好装得下 8 MiB"：容量预检除了要留出声明字节，还硬留
# 64 MiB 余量（internal/files/staging.go 的 uploadReserveMargin）。根比 64 MiB 还小的话，
# 任何上传在**声明阶段**就被 413 掉，根本走不到写入——那就测不到 ENOSPC 了。
ROOT_SMALL_SIZE=128m
MAX_UPLOAD=3221225472           # 3 GiB：让 2 GiB 的上传落在合法区间里
ENOSPC_DECLARE=8388608          # 8 MiB：声明时放得下
BIG_BYTES=2147483648            # 2 GiB

APPS=/api/v1/apps
FILES=/api/v1/files
AGENT_LOG=$LOGS/agent.log
WEB_LOG=$LOGS/web.log
CANARY_LOG=$LOGS/canary.log
AGENT_PID=""
WEB_PID=""
SRV_PID=""
SAMPLER_PID=""
SMALL_MOUNTED=0
UNITS_WERE=""

PASSED=0
FAILED=0
NOTRUN=0
FAILED_NAMES=""

pass() { PASSED=$((PASSED + 1)); printf 'PASS  %s\n' "$1"; }
fail() {
  FAILED=$((FAILED + 1))
  FAILED_NAMES="$FAILED_NAMES | $1"
  printf 'FAIL  %s: %s\n' "$1" "${2:-}"
}
notrun() { NOTRUN=$((NOTRUN + 1)); printf 'NOT RUN  %s：%s\n' "$1" "$2"; }
note() { printf '      %s\n' "$1"; }
expect_eq() {
  if [ "$2" = "$3" ]; then pass "$1"; else fail "$1" "期望 '$2'，实际 '$3'"; fi
}
expect_ne() {
  if [ "$2" != "$3" ]; then pass "$1"; else fail "$1" "不该等于 '$2'"; fi
}
expect_contains() {
  case "$3" in
    *"$2"*) pass "$1" ;;
    *) fail "$1" "期望包含 '$2'，实际 '$3'" ;;
  esac
}
expect_not_contains() {
  case "$3" in
    *"$2"*) fail "$1" "不该包含 '$2'，实际 '$3'" ;;
    *) pass "$1" ;;
  esac
}

jget() {
  python3 -c 'import json,sys
try:
    d=json.load(open(sys.argv[1], encoding="utf-8"))
except Exception:
    print("")
    raise SystemExit
src=sys.argv[2]
ns={"d": d, "json": json}
try:
    try:
        print(eval(src, ns))
    except SyntaxError:
        ns["_"]=None
        exec(compile(src, "<jget>", "exec"), ns)
        print("" if ns["_"] is None else ns["_"])
except Exception as exc:
    print("ERR:%s:%s" % (type(exc).__name__, exc))' "$1" "$2" 2>/dev/null
}

resp_get() { jget "$WORK/resp.json" "$1"; }
jobget() { jget "$WORK/job.json" "$1"; }
joutcome() { jobget 'd["data"]["result"]["outcome"]'; }
jobserved() { jobget "d[\"data\"][\"result\"][\"observed_state\"][\"$1\"]"; }
sha_of() { sha256sum "$1" 2>/dev/null | cut -d' ' -f1; }
urlenc() { python3 -c 'import sys,urllib.parse;print(urllib.parse.quote(sys.argv[1],safe=""))' "$1"; }

# staging_parts <root>：暂存目录里还剩几个 .part（真正占着磁盘的那些）。
staging_parts() {
  find "$1/.upanel/staging" -maxdepth 1 -name 'up_*.part' 2>/dev/null | wc -l | tr -d ' '
}
staging_records() {
  find "$1/.upanel/staging" -maxdepth 1 -name 'up_*.json' 2>/dev/null | wc -l | tr -d ' '
}
record_exists() { [ -e "$1/.upanel/staging/$2.json" ] && echo yes || echo no; }

cleanup() {
  if [ -n "${SAMPLER_PID:-}" ] && kill -0 "$SAMPLER_PID" 2>/dev/null; then
    kill -TERM "$SAMPLER_PID" 2>/dev/null
  fi
  for _pid in "${WEB_PID:-}" "${AGENT_PID:-}" "${SRV_PID:-}"; do
    if [ -n "$_pid" ] && kill -0 "$_pid" 2>/dev/null; then
      kill -TERM "$_pid" 2>/dev/null
      wait "$_pid" 2>/dev/null
    fi
  done
  rm -f "$SOCK"
  if [ "$SMALL_MOUNTED" -eq 1 ]; then
    umount "$ROOT_SMALL" 2>/dev/null
  fi
  # 脚本自己起的服务原样还回去（停过的话）。
  case "$UNITS_WERE" in
    *agent*) systemctl start upanel-agent >/dev/null 2>&1 ;;
  esac
  case "$UNITS_WERE" in
    *web*) systemctl start upanel-web >/dev/null 2>&1 ;;
  esac
  return 0
}
trap cleanup EXIT
trap 'cleanup; exit 130' INT
trap 'cleanup; exit 143' TERM

start_agent() {
  "$AGENT" --config "$SYS_CFG" >>"$AGENT_LOG" 2>&1 &
  AGENT_PID=$!
  for _ in $(seq 1 100); do
    [ -S "$SOCK" ] && return 0
    sleep 0.1
  done
  return 1
}

start_web() {
  runuser -u "$WEB_USER" -- "$WEB" --mode production --db "$WEB_DB" --listen "127.0.0.1:$PORT" \
    >>"$WEB_LOG" 2>&1 &
  WEB_PID=$!
  for _ in $(seq 1 100); do
    if curl -fsS "$BASE/api/v1/health/live" >/dev/null 2>&1; then return 0; fi
    sleep 0.2
  done
  return 1
}

chown_web_db() {
  chown "$WEB_USER" "$WEB_DB" 2>/dev/null
  for suffix in "" "-wal" "-shm"; do
    [ -e "$WEB_DB$suffix" ] && chown "$WEB_USER" "$WEB_DB$suffix"
  done
  return 0
}

login() {
  curl -s -c "$3" -o "$WORK/$4.json" -w '%{http_code}' -X POST "$BASE/api/v1/auth/login" \
    -H 'Content-Type: application/json' -H 'X-UPanel-Request: 1' -H "Origin: $ORIGIN" \
    -d "{\"username\":\"$1\",\"password\":\"$2\"}"
}
csrf_of() { jget "$1" 'd["data"]["csrf_token"]'; }

api() {
  _jar=$1; _csrf=$2; _method=$3; _path=$4; _body=${5:-}
  if [ -n "$_body" ]; then
    printf '%s' "$_body" >"$WORK/req.json"
    curl -s -b "$_jar" -o "$WORK/resp.json" -w '%{http_code}' -X "$_method" "$BASE$_path" \
      -H 'Content-Type: application/json' -H "Origin: $ORIGIN" -H "X-CSRF-Token: $_csrf" \
      -d @"$WORK/req.json"
  else
    curl -s -b "$_jar" -o "$WORK/resp.json" -w '%{http_code}' -X "$_method" "$BASE$_path" \
      -H "Origin: $ORIGIN" -H "X-CSRF-Token: $_csrf"
  fi
}

preflight() {
  printf '{"action":"%s","params":%s}' "$1" "$2" >"$WORK/pre.json"
  _code=$(curl -s -b "$JAR" -o "$WORK/resp.json" -w '%{http_code}' \
    -X POST "$BASE/api/v1/operations/preflight" \
    -H 'Content-Type: application/json' -H "Origin: $ORIGIN" -H "X-CSRF-Token: $CSRF" \
    -d @"$WORK/pre.json")
  cp "$WORK/resp.json" "$WORK/preflight.json"
  printf '%s' "$_code"
}
pget() { jget "$WORK/preflight.json" "$1"; }

submit_job() {
  python3 - "$1" "${2:-}" >"$WORK/jobreq.json" <<'PY'
import json, sys
print(json.dumps({"preflight_id": sys.argv[1], "confirmation_phrase": sys.argv[2]}))
PY
  _code=$(curl -s -b "$JAR" -o "$WORK/resp.json" -w '%{http_code}' -X POST "$BASE/api/v1/jobs" \
    -H 'Content-Type: application/json' -H "Origin: $ORIGIN" -H "X-CSRF-Token: $CSRF" \
    -H "Idempotency-Key: $3" -d @"$WORK/jobreq.json")
  cp "$WORK/resp.json" "$WORK/submit.json"
  printf '%s' "$_code"
}

wait_job() {
  _i=0
  while [ "$_i" -lt 600 ]; do
    api "$JAR" "$CSRF" GET "/api/v1/jobs/$1" >/dev/null
    cp "$WORK/resp.json" "$WORK/job.json"
    _st=$(jobget 'd["data"]["status"]')
    case "$_st" in
      succeeded|failed|canceled|interrupted) printf '%s' "$_st"; return 0 ;;
    esac
    _i=$((_i + 1))
    sleep 0.5
  done
  printf 'timeout'
}

submit_and_wait() {
  _code=$(submit_job "$1" "${2:-}" "$3")
  if [ "$_code" != "202" ]; then printf 'submit:%s' "$_code"; return 0; fi
  _id=$(jget "$WORK/submit.json" 'd["data"]["job_id"]')
  wait_job "$_id"
}

# put_bytes <upload_id> <文件> [额外 curl 参数...]：回显状态码。
put_bytes() {
  _up=$1; _file=$2; shift 2
  curl -s -b "$JAR" -o "$WORK/resp.json" -w '%{http_code}' -X PUT \
    "$BASE$FILES/uploads/$_up/content" -H 'Content-Type: application/octet-stream' \
    -H "Origin: $ORIGIN" -H "X-CSRF-Token: $CSRF" --data-binary @"$_file" "$@"
}

# put_stream 用 -T 从磁盘边读边发，不把整个文件读进内存。
# 2 GiB 用 --data-binary 会直接 "out of memory"（实测撞过），而且那样测的也不是"流式"。
put_stream() {
  _up=$1; _file=$2; shift 2
  curl -s -b "$JAR" -o "$WORK/resp.json" -w '%{http_code}' -X PUT \
    "$BASE$FILES/uploads/$_up/content" -H 'Content-Type: application/octet-stream' \
    -H "Origin: $ORIGIN" -H "X-CSRF-Token: $CSRF" -T "$_file" "$@"
}

mkdir -p "$LOGS" "$PAYLOADS" "$WEB_DB_DIR" "$ROOT_RW" "$ROOT_SMALL"
chown -R "$WEB_USER" "$WEB_DB_DIR"

echo "== 0. 环境与起点 =="
. /etc/os-release
note "系统：$PRETTY_NAME"
note "内核：$(uname -r)"
expect_eq "以 root 运行" "0" "$(id -u)"
if id "$WEB_USER" >/dev/null 2>&1; then pass "Web 运行身份存在（$WEB_USER）"; else
  fail "Web 运行身份存在" "没有 $WEB_USER 账号"; exit 2; fi
for t in python3 curl runuser mount umount df dd sha256sum; do
  if command -v "$t" >/dev/null 2>&1; then pass "依赖命令可用 $t"; else fail "依赖命令可用 $t" "缺少"; exit 2; fi
done

# 上一轮 P16 装过的服务占着同一个 socket 与端口，先停掉并把原状态记下来，收尾还回去。
for u in upanel-agent upanel-web; do
  if systemctl is-active --quiet "$u" 2>/dev/null; then
    UNITS_WERE="$UNITS_WERE $(printf '%s' "$u" | sed 's/^upanel-//')"
    systemctl stop "$u" >/dev/null 2>&1 || true
  fi
done
[ -n "$UNITS_WERE" ] && note "停掉并将在收尾还原的单元：$UNITS_WERE"
# stop 是异步的：刚发完命令时可能是 activating/deactivating，等它落定再断言。
_i=0
while [ "$_i" -lt 40 ]; do
  if [ "$(systemctl is-active upanel-agent 2>/dev/null)" = "inactive" ] &&
     [ "$(systemctl is-active upanel-web 2>/dev/null)" = "inactive" ]; then
    break
  fi
  _i=$((_i + 1)); sleep 0.5
done
expect_eq "代理单元已停（否则 socket 被占）" "inactive" "$(systemctl is-active upanel-agent 2>/dev/null || :)"
expect_eq "Web 单元已停" "inactive" "$(systemctl is-active upanel-web 2>/dev/null || :)"
expect_eq "端口 $PORT 空闲" "" "$(ss -lntH "sport = :$PORT" 2>/dev/null)"

rm -rf "$WORK/webdata" "$WORK/rw" "$WORK/payloads" "$WORK/logs"
mkdir -p "$LOGS" "$PAYLOADS" "$WEB_DB_DIR" "$ROOT_RW"
chown -R "$WEB_USER" "$WEB_DB_DIR"
chmod 0755 "$WORK"

echo
echo "== 1. ENOSPC 现场：一块 $ROOT_SMALL_SIZE 的 tmpfs 当可写根 =="
if mount -t tmpfs -o "size=$ROOT_SMALL_SIZE,mode=0777" tmpfs "$ROOT_SMALL"; then
  SMALL_MOUNTED=1
  pass "tmpfs 已挂到 $ROOT_SMALL"
else
  fail "tmpfs 挂载" "mount 失败，ENOSPC 一节无法进行"
fi
SMALL_AVAIL_KB=$(df -k --output=avail "$ROOT_SMALL" 2>/dev/null | tail -1 | tr -d ' ')
expect_eq "tmpfs 容量是 128 MiB 上下（131072 KiB 附近）" "1" \
  "$([ "${SMALL_AVAIL_KB:-0}" -ge 120000 ] && [ "${SMALL_AVAIL_KB:-0}" -le 132000 ] && echo 1 || echo 0)"
note "可用：${SMALL_AVAIL_KB:-?} KiB；声明要在写入阶段才撞 ENOSPC，所以根必须比 64 MiB 余量大"

echo
echo "== 2. 配置与启动（代理 root、Web 非 root） =="
install -d -m 0755 /etc/upanel
install -d -m 0700 "$AGENT_DB_DIR"
install -d -m 0755 "$RUN_DIR"
rm -f "$AGENT_DB" "$AGENT_DB-wal" "$AGENT_DB-shm"
rm -f "$WEB_DB" "$WEB_DB-wal" "$WEB_DB-shm"
cat >"$SYS_CFG" <<JSON
{
  "web": { "mode": "production", "listen_address": "127.0.0.1:$PORT", "shutdown_timeout_seconds": 10 },
  "agent": {
    "socket_path": "/run/upanel/agent.sock",
    "shutdown_timeout_seconds": 10,
    "web_user": "upanel-web",
    "socket_group": "upanel-web",
    "database_path": "/var/lib/upanel-agent/agent.db",
    "secret_key_path": "/etc/upanel/agent.key",
    "files": {
      "max_upload_bytes": $MAX_UPLOAD,
      "roots": [
        { "id": "rw", "name": "可写根", "path": "$ROOT_RW", "writable": true },
        { "id": "small", "name": "tmpfs ENOSPC 现场", "path": "$ROOT_SMALL", "writable": true }
      ]
    },
    "storage": { "smart_enabled": false, "smart_interval_minutes": 15, "network_capacity_timeout_seconds": 2 },
    "services": {},
    "logs": {}
  },
  "database": { "path": "$WEB_DB", "busy_timeout_ms": 5000 },
  "log": { "level": "info", "format": "json" }
}
JSON
chmod 0644 "$SYS_CFG"

if [ -f "$KEY_FILE" ]; then
  pass "代理密钥文件已存在（沿用）"
else
  if "$AGENT" --config "$SYS_CFG" -init-key >>"$LOGS/init-key.log" 2>&1; then
    pass "-init-key 生成代理密钥"
  else
    fail "-init-key 生成代理密钥" "$(tail -3 "$LOGS/init-key.log" 2>/dev/null)"; exit 2
  fi
fi
if start_agent; then pass "代理以 root 启动并创建 socket"; else
  fail "代理以 root 启动" "$(tail -5 "$AGENT_LOG" 2>/dev/null)"; exit 2; fi
expect_eq "代理进程 uid=0" "0" "$(ps -o uid= -p "$AGENT_PID" 2>/dev/null | tr -d ' ')"

printf '%s\n%s\n' "$PASSWORD" "$PASSWORD" | "$CTL" init-admin --config "$SYS_CFG" --db "$WEB_DB" \
  --username admin --display-name "管理员" --json >"$WORK/init.out" 2>"$WORK/init.err"
expect_eq "init-admin 退出码" "0" "$?"
chown_web_db
if start_web; then pass "Web 在 $BASE 启动"; else
  fail "Web 启动" "$(tail -5 "$WEB_LOG" 2>/dev/null)"; exit 2; fi
WEB_UID=$(ps -o uid= -p "$WEB_PID" 2>/dev/null | tr -d ' ')
note "runuser 外壳 pid=$WEB_PID uid=${WEB_UID:-?}（这个外壳本身是 root，不算数）"
# 真正要看的是"谁在监听这个端口"，不是 runuser 外壳的 uid。
LISTEN_PID=$(ss -lntpH "sport = :$PORT" 2>/dev/null | sed -n 's/.*pid=\([0-9]*\).*/\1/p' | head -1)
REAL_UID=$(ps -o uid= -p "${LISTEN_PID:-0}" 2>/dev/null | tr -d ' ')
REAL_USER=$(ps -o user= -p "${LISTEN_PID:-0}" 2>/dev/null | tr -d ' ')
if [ -n "$REAL_UID" ] && [ "$REAL_UID" != "0" ]; then
  pass "监听 $PORT 的 Web 进程不是 root（pid=$LISTEN_PID uid=$REAL_UID user=$REAL_USER）"
else
  fail "监听 $PORT 的 Web 进程不是 root" "uid=${REAL_UID:-未知} user=${REAL_USER:-未知}"
fi

JAR=$WORK/admin.jar
expect_eq "管理员登录" "200" "$(login admin "$PASSWORD" "$JAR" login-admin)"
CSRF=$(csrf_of "$WORK/login-admin.json")
expect_ne "拿到 CSRF 令牌" "" "$CSRF"

echo
echo "== 3. APP-02：302 到底怎么记、有没有跟着跳 =="
# 三个上游都用同一个 python 脚本，模式由参数决定；金丝雀把每次命中写一行日志。
cat >"$WORK/srv.py" <<'PY'
import sys, http.server, socketserver

mode = sys.argv[1]
port = int(sys.argv[2])
arg = sys.argv[3] if len(sys.argv) > 3 else ""
logf = sys.argv[4] if len(sys.argv) > 4 else ""


class H(http.server.BaseHTTPRequestHandler):
    def _log(self, line):
        if logf:
            with open(logf, "a", encoding="utf-8") as fh:
                fh.write(line + "\n")

    def do_GET(self):
        self._log("GET %s" % self.path)
        if mode == "redirect":
            self.send_response(302)
            self.send_header("Location", arg)
            self.send_header("Content-Length", "0")
            self.end_headers()
            return
        body = b"STOLEN-SECRET\n"
        self.send_response(200)
        self.send_header("Content-Type", "text/plain")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *a):
        pass


socketserver.TCPServer.allow_reuse_address = True
with socketserver.TCPServer(("127.0.0.1", port), H) as srv:
    srv.serve_forever()
PY

: >"$CANARY_LOG"
python3 "$WORK/srv.py" canary "$P_CANARY" "" "$CANARY_LOG" >"$LOGS/canary-srv.log" 2>&1 &
SRV_PID=$!
python3 "$WORK/srv.py" redirect "$P_REDIR" "http://127.0.0.1:$P_CANARY/stolen" "$LOGS/redir-hits.log" >"$LOGS/redir-srv.log" 2>&1 &
REDIR_PID=$!
python3 "$WORK/srv.py" redirect "$P_META" "http://169.254.169.254/latest/meta-data/iam/security-credentials/" "$LOGS/meta-hits.log" >"$LOGS/meta-srv.log" 2>&1 &
META_PID=$!
for _p in "$P_CANARY" "$P_REDIR" "$P_META"; do
  _i=0
  while [ "$_i" -lt 50 ]; do
    ss -lntH "sport = :$_p" 2>/dev/null | grep -q . && break
    _i=$((_i + 1)); sleep 0.1
  done
done
expect_eq "金丝雀在 $P_CANARY 监听" "1" "$(ss -lntH "sport = :$P_CANARY" 2>/dev/null | wc -l | tr -d ' ')"
expect_eq "302→金丝雀 上游在 $P_REDIR 监听" "1" "$(ss -lntH "sport = :$P_REDIR" 2>/dev/null | wc -l | tr -d ' ')"
expect_eq "302→云元数据 上游在 $P_META 监听" "1" "$(ss -lntH "sport = :$P_META" 2>/dev/null | wc -l | tr -d ' ')"

api "$JAR" "$CSRF" GET "$APPS/discovery" >/dev/null
cp "$WORK/resp.json" "$PAYLOADS/discovery.json"
DISC_STATUS=$(jget "$PAYLOADS/discovery.json" 'd["data"]["status"]')
DISC_SRC=$(jget "$PAYLOADS/discovery.json" '"/".join("%s=%s" % (s["source"], s["status"]) for s in d["data"]["sources"])')
note "发现状态：$DISC_STATUS（来源：$DISC_SRC）"
# ok 和 partial 都算正常：这台 VM 没装 docker，docker 那个来源本来就该报不可用。
# error 才是"所有来源都失败"，那种结果不能被读成"本机什么都没有"。
expect_ne "发现结果不是 error（至少本机监听这一路读到了）" "error" "$DISC_STATUS"
case "$DISC_STATUS" in
  ok|partial) pass "发现状态在 ok/partial 之内（partial 的来由写在上一条 note 里）" ;;
  *) fail "发现状态在 ok/partial 之内" "实际 $DISC_STATUS" ;;
esac

cand_for() {
  jget "$PAYLOADS/discovery.json" "next((c[\"target_id\"] for c in d[\"data\"][\"candidates\"] if any(l[\"port\"] == $1 for l in c[\"listeners\"])), \"\")"
}
T_REDIR=$(cand_for "$P_REDIR")
T_META=$(cand_for "$P_META")
expect_ne "发现里有 302→金丝雀 的候选" "" "$T_REDIR"
expect_ne "发现里有 302→云元数据 的候选" "" "$T_META"

create_app() {
  _code=$(api "$JAR" "$CSRF" POST "$APPS" "$1")
  cp "$WORK/resp.json" "$WORK/app.json"
  printf '%s' "$_code"
}
wait_health() {
  _id=$1; _i=0
  while [ "$_i" -lt 60 ]; do
    api "$JAR" "$CSRF" GET "$APPS/$_id/health" >/dev/null
    cp "$WORK/resp.json" "$WORK/health.json"
    _c=$(jget "$WORK/health.json" 'd["data"]["checked_at"]')
    # jget 出错时回的是 "ERR:..." 字符串，那种"非空"不能当成探到了。
    case "$_c" in
      ""|None|ERR:*) : ;;
      *) return 0 ;;
    esac
    _i=$((_i + 1)); sleep 1
  done
  return 1
}
hget() { jget "$WORK/health.json" "d[\"data\"]$1"; }

expect_eq "建应用（302→金丝雀）" "201" \
  "$(create_app "{\"name\":\"P164 302 金丝雀\",\"type\":\"link\",\"public_url\":\"http://192.168.56.10:$P_REDIR/\",\"health_check\":{\"enabled\":true,\"target_id\":\"$T_REDIR\",\"port\":$P_REDIR,\"path\":\"/\",\"interval_seconds\":30,\"timeout_seconds\":3}}")"
APP_REDIR=$(jget "$WORK/app.json" 'd["data"]["id"]')
cp "$WORK/app.json" "$PAYLOADS/app-redir.json"

expect_eq "建应用（302→云元数据）" "201" \
  "$(create_app "{\"name\":\"P164 302 元数据\",\"type\":\"link\",\"public_url\":\"http://192.168.56.10:$P_META/\",\"health_check\":{\"enabled\":true,\"target_id\":\"$T_META\",\"port\":$P_META,\"path\":\"/\",\"interval_seconds\":30,\"timeout_seconds\":3}}")"
APP_META=$(jget "$WORK/app.json" 'd["data"]["id"]')
cp "$WORK/app.json" "$PAYLOADS/app-meta.json"

if wait_health "$APP_REDIR"; then pass "302→金丝雀 探出了结论"; else fail "302→金丝雀 探测" "60 次轮询仍是 checked_at=null"; fi
cp "$WORK/health.json" "$PAYLOADS/health-redir.json"
note "http_state=$(hget '["http_state"]') status_code=$(hget '["status_code"]') address=$(hget '["address"]')"
# 302 落在默认的 200—399 集合里：结论是 ok，状态码如实记成 302。
expect_eq "302 的 http_state 是 ok（不是 error）" "ok" "$(hget '["http_state"]')"
expect_eq "记录的 status_code 是 302" "302" "$(hget '["status_code"]')"
# 关键断言：面板没有跟随重定向。跟随了金丝雀就会留下命中记录。
CANARY_HITS=$(wc -l <"$CANARY_LOG" 2>/dev/null | tr -d ' ')
expect_eq "金丝雀**一次都没被请求**（没跟随重定向）" "0" "${CANARY_HITS:-0}"
REDIR_HITS=$(wc -l <"$LOGS/redir-hits.log" 2>/dev/null | tr -d ' ')
expect_eq "上游确实被探过一次（302 不是编出来的）" "1" "${REDIR_HITS:-0}"

if wait_health "$APP_META"; then pass "302→云元数据 探出了结论"; else fail "302→云元数据 探测" "60 次轮询仍是 checked_at=null"; fi
cp "$WORK/health.json" "$PAYLOADS/health-meta.json"
note "http_state=$(hget '["http_state"]') status_code=$(hget '["status_code"]')"
expect_eq "指向 169.254.169.254 的 302 也照样只记 302" "302" "$(hget '["status_code"]')"
expect_eq "指向元数据的 302 不算错误" "ok" "$(hget '["http_state"]')"
META_HITS=$(wc -l <"$LOGS/meta-hits.log" 2>/dev/null | tr -d ' ')
expect_eq "元数据上游被探过一次" "1" "${META_HITS:-0}"
# 探测记录里的地址必须还是回环，不能变成 Location 里那个。
expect_not_contains "健康记录里的地址没有被换成 169.254.169.254" "169.254" \
  "$(hget '["address"]')$(hget '["url"]')"
expect_not_contains "健康记录里没有金丝雀的内容" "STOLEN" "$(cat "$WORK/health.json")"

echo
echo "== 4. FILE-02 之一：写的时候真的撞 ENOSPC =="
# 先放一个"已有版本"进去，用来证明失败的上传不会把它改掉。
printf 'v1-original\n' >"$ROOT_SMALL/existing.bin"
EXIST_SHA=$(sha_of "$ROOT_SMALL/existing.bin")

expect_eq "声明 8 MiB 上传（声明时确实放得下）" "201" \
  "$(api "$JAR" "$CSRF" POST "$FILES/uploads" \
     "{\"root_id\":\"small\",\"path\":\"existing.bin\",\"size_bytes\":\"$ENOSPC_DECLARE\"}")"
cp "$WORK/resp.json" "$PAYLOADS/upload-enospc-created.json"
UP_SMALL=$(resp_get 'd["data"]["upload_id"]')
expect_ne "拿到 upload_id" "" "$UP_SMALL"
note "声明时 tmpfs 可用：$(df -k --output=avail "$ROOT_SMALL" | tail -1 | tr -d ' ') KiB（声明 ${ENOSPC_DECLARE} 字节）"

# 声明之后、传字节之前把盘填满，只留 512 KiB 给元数据收尾。
FREE_KB=$(df -k --output=avail "$ROOT_SMALL" | tail -1 | tr -d ' ')
FILL_KB=$((FREE_KB - 512))
if [ "$FILL_KB" -gt 0 ]; then
  dd if=/dev/zero of="$ROOT_SMALL/filler.bin" bs=1k count="$FILL_KB" >/dev/null 2>&1 || true
fi
LEFT_KB=$(df -k --output=avail "$ROOT_SMALL" | tail -1 | tr -d ' ')
note "填充后可用：${LEFT_KB} KiB"
expect_eq "现场造好了：剩余空间小于要传的 8 MiB" "1" "$([ "${LEFT_KB:-999999}" -lt 8192 ] && echo 1 || echo 0)"

python3 - "$WORK/eight-mib.bin" "$ENOSPC_DECLARE" <<'PY'
import sys
path, size = sys.argv[1], int(sys.argv[2])
buf = bytearray(size)
for i in range(size):
    buf[i] = (i * 31 + 7) % 251
open(path, "wb").write(buf)
PY
ENOSPC_CODE=$(put_bytes "$UP_SMALL" "$WORK/eight-mib.bin")
cp "$WORK/resp.json" "$PAYLOADS/upload-enospc-put.json"
note "PUT 返回 $ENOSPC_CODE：$(resp_get 'd["error"]["code"]') / $(resp_get 'd["error"]["message"]')"
expect_eq "写时空间不足是 413（不是 500）" "413" "$ENOSPC_CODE"
expect_eq "错误码是 VALIDATION_ERROR" "VALIDATION_ERROR" "$(resp_get 'd["error"]["code"]')"
expect_contains "错误消息说清楚是空间不足" "空间不足" "$(resp_get 'd["error"]["message"]')"

expect_eq "目标文件没有被这次失败的上传改写" "$EXIST_SHA" "$(sha_of "$ROOT_SMALL/existing.bin")"
expect_eq "暂存里没有残留的 .part" "0" "$(staging_parts "$ROOT_SMALL")"
expect_eq "上传记录留着（failed，不是假装没发生）" "1" "$(staging_records "$ROOT_SMALL")"
expect_eq "上传视图 state=failed" "200" "$(api "$JAR" "$CSRF" GET "$FILES/uploads/$UP_SMALL")"
expect_eq "state" "failed" "$(resp_get 'd["data"]["state"]')"
expect_eq "失败之后 received_bytes 归零" "0" "$(resp_get 'd["data"]["received_bytes"]')"
ENOSPC_LOG=$(grep -c "上传失败，暂存已删除" "$AGENT_LOG" 2>/dev/null || true)
expect_ne "代理日志记了这次 ENOSPC（不是静默失败）" "0" "${ENOSPC_LOG:-0}"
note "日志原文：$(grep -m1 '上传失败，暂存已删除' "$AGENT_LOG" 2>/dev/null | cut -c1-200)"

rm -f "$ROOT_SMALL/filler.bin"
echo "      清掉填充文件，tmpfs 现在可用 $(df -k --output=avail "$ROOT_SMALL" | tail -1 | tr -d ' ') KiB"

echo
echo "== 5. FILE-02 之二：断流 =="
expect_eq "声明 8 MiB 上传（大根）" "201" \
  "$(api "$JAR" "$CSRF" POST "$FILES/uploads" \
     "{\"root_id\":\"rw\",\"path\":\"broken.bin\",\"size_bytes\":\"$ENOSPC_DECLARE\"}")"
UP_BROKEN=$(resp_get 'd["data"]["upload_id"]')
note "upload_id=$UP_BROKEN"
# 限速 + 超时：连接会在传完之前被掐断，服务端看到的是半截 body。
BROKEN_CODE=$(put_bytes "$UP_BROKEN" "$WORK/eight-mib.bin" --limit-rate 1M --max-time 3 2>/dev/null)
BROKEN_RC=$?
note "被掐断的 PUT：http_code=${BROKEN_CODE:-（空）} curl 退出码=$BROKEN_RC"
# curl 掐断时不会拿到最终状态码（可能只拿到 100 Continue 这个过渡响应），
# 所以这一条只断言"没正常收完"，真正要看的判定在下面的服务端状态上。
expect_ne "客户端不是正常收尾（退出码非 0）" "0" "$BROKEN_RC"
sleep 1
expect_eq "掐断之后这条上传进 failed" "200" "$(api "$JAR" "$CSRF" GET "$FILES/uploads/$UP_BROKEN")"
expect_eq "state" "failed" "$(resp_get 'd["data"]["state"]')"
expect_eq "断流的暂存被清掉" "0" "$(staging_parts "$ROOT_RW")"
expect_eq "目标文件不存在" "no" "$([ -e "$ROOT_RW/broken.bin" ] && echo yes || echo no)"

echo
echo "== 6. FILE-02 之三：2 GiB 真的推一遍（流式有界） =="
BIG=$WORK/big.bin
if [ ! -s "$BIG" ]; then
  note "生成 2 GiB 载荷（全零，落盘不压缩）"
  dd if=/dev/zero of="$BIG" bs=1M count=2048 status=none 2>/dev/null
fi
expect_eq "载荷大小是 2 GiB" "$BIG_BYTES" "$(stat -c %s "$BIG")"
BIG_SHA=$(sha_of "$BIG")

AGENT_RSS_BEFORE=$(ps -o rss= -p "$AGENT_PID" | tr -d ' ')
note "上传前代理 RSS：${AGENT_RSS_BEFORE} KiB"
# 0.5 秒采一次，记峰值。
(
  peak=0
  while kill -0 "$AGENT_PID" 2>/dev/null; do
    r=$(ps -o rss= -p "$AGENT_PID" 2>/dev/null | tr -d ' ')
    [ -n "$r" ] && [ "$r" -gt "$peak" ] && peak=$r && printf '%s\n' "$peak" >"$WORK/agent-rss-peak.txt"
    sleep 0.5
  done
) &
SAMPLER_PID=$!

expect_eq "声明 2 GiB 上传" "201" \
  "$(api "$JAR" "$CSRF" POST "$FILES/uploads" \
     "{\"root_id\":\"rw\",\"path\":\"big.bin\",\"size_bytes\":\"$BIG_BYTES\"}")"
cp "$WORK/resp.json" "$PAYLOADS/upload-2gib-created.json"
UP_BIG=$(resp_get 'd["data"]["upload_id"]')
note "upload_id=$UP_BIG"

T0=$(date +%s)
BIG_CODE=$(put_stream "$UP_BIG" "$BIG")
BIG_RC=$?
T1=$(date +%s)
cp "$WORK/resp.json" "$PAYLOADS/upload-2gib-put.json"
note "PUT 2 GiB 用了 $((T1 - T0)) 秒，返回 $BIG_CODE（curl 退出码 $BIG_RC）"
expect_eq "2 GiB 上传收完" "200" "$BIG_CODE"
expect_eq "received_bytes 等于 2 GiB" "$BIG_BYTES" "$(resp_get 'int(d["data"]["received_bytes"])')"

kill -TERM "$SAMPLER_PID" 2>/dev/null
wait "$SAMPLER_PID" 2>/dev/null
SAMPLER_PID=""
RSS_PEAK=$(cat "$WORK/agent-rss-peak.txt" 2>/dev/null | tr -d ' ')
AGENT_RSS_AFTER=$(ps -o rss= -p "$AGENT_PID" | tr -d ' ')
note "峰值 RSS：${RSS_PEAK:-?} KiB（上传前 ${AGENT_RSS_BEFORE}，上传后 ${AGENT_RSS_AFTER}）"
# 2 GiB = 2097152 KiB。如果先把 2 GiB 吃进内存，RSS 至少要涨到这个数的一半。
RSS_LIMIT_KB=$((BIG_BYTES / 1024 / 4))   # 512 MiB
if [ -n "${RSS_PEAK:-}" ] && [ "$RSS_PEAK" -lt "$RSS_LIMIT_KB" ]; then
  pass "流式有界：峰值 RSS ${RSS_PEAK} KiB 远小于 2 GiB 的 1/4（${RSS_LIMIT_KB} KiB）"
else
  fail "流式有界" "峰值 RSS ${RSS_PEAK:-未知} KiB 不小于 ${RSS_LIMIT_KB} KiB"
fi

expect_eq "提交落位预检" "200" \
  "$(preflight file.commit_upload "{\"upload_id\":\"$UP_BIG\",\"overwrite\":false}")"
BIG_PF=$(pget 'd["data"]["preflight_id"]')
expect_eq "落位任务成功" "succeeded" "$(submit_and_wait "$BIG_PF" "" "k-p164-big")"
expect_eq "落位 outcome 是 verified" "verified" "$(joutcome)"
expect_eq "落位后大小是 2 GiB" "$BIG_BYTES" "$(jobserved size_bytes)"
expect_eq "落位后 sha256 与载荷一致" "$BIG_SHA" "$(sha_of "$ROOT_RW/big.bin")"
# 只断言这一条的记录消失：大根里还躺着 §5 那条 failed 记录（broken.bin），它是清理暂存
# 而不是抹掉历史，留着才对。
expect_eq "2 GiB 这条上传的记录随落位消失" "no" "$(record_exists "$ROOT_RW" "$UP_BIG")"
expect_eq "落位后大根里没有残留的 .part" "0" "$(staging_parts "$ROOT_RW")"
expect_eq "落位后查这条上传是 404" "404" "$(api "$JAR" "$CSRF" GET "$FILES/uploads/$UP_BIG")"

# 超过上限（3 GiB + 1）仍然是 413。
expect_eq "声明超过 max_upload_bytes 是 413" "413" \
  "$(api "$JAR" "$CSRF" POST "$FILES/uploads" \
     "{\"root_id\":\"rw\",\"path\":\"too-big.bin\",\"size_bytes\":\"$((MAX_UPLOAD + 1))\"}")"
expect_eq "超限的错误码是 VALIDATION_ERROR" "VALIDATION_ERROR" "$(resp_get 'd["error"]["code"]')"
expect_eq "超限不留暂存" "0" "$(staging_parts "$ROOT_RW")"

echo
echo "== 7. 收尾 =="
rm -f "$ROOT_RW/big.bin"
if [ "${KEEP_BIG:-0}" = "1" ]; then
  note "KEEP_BIG=1：保留 $BIG"
else
  rm -f "$BIG"
fi
expect_eq "收尾后大根里没有 2 GiB 残留" "0" \
  "$([ -e "$ROOT_RW/big.bin" ] && echo 1 || echo 0)"
expect_eq "rm 之后空间回来了（大根可用 > 20 GiB）" "1" \
  "$([ "$(df -k --output=avail "$ROOT_RW" | tail -1 | tr -d ' ')" -gt 20971520 ] && echo 1 || echo 0)"

echo
echo "=========================== 汇总 ==========================="
printf 'PASS %d / FAIL %d / NOT RUN %d\n' "$PASSED" "$FAILED" "$NOTRUN"
[ -n "$FAILED_NAMES" ] && printf '失败项：%s\n' "$FAILED_NAMES"
printf 'NOT RUN 不等于通过；DISK-02 的 LVM/RAID 根盘不在本脚本覆盖范围内。\n'
[ "$FAILED" -eq 0 ] || exit 1
exit 0
