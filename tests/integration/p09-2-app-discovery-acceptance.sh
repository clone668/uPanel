#!/bin/sh
# P09.2 的 Linux 验收：本机端口/服务/容器发现、管理员确认候选、健康探测与 SSRF 约束。
#
# 这一份要证明的不是“接口返回 200”，而是下面这些容易被“写完了”骗过去的事实：
#
#   1) **发现是真的在本机上采集的**：我们自己起一个监听，它必须出现在候选里；没人监听的
#      端口必须不在。候选不是硬编码列表，也不是“本机什么都没有”的空列表。
#   2) **确认候选就是创建应用**：用候选的 target_id 建应用成功；形状合法但发现结果里没有
#      的 target 是 422 并带 details.target_id；写成 IP/主机名/URL 是 400，一条都不落库。
#   3) **SSRF 是结构性的**：探测记录里的地址永远是 127.0.0.1；云元数据地址、外部主机、
#      完整 URL 在 target_id 这个形状里根本写不出来。
#   4) **三种 unknown 分得开**：没启用（not_applicable）、从没探过（unknown + checked_at
#      null）、探过但没结论（unknown + 原因）。代理不可用写 unknown 而**不是** error，
#      并且那一轮不填 address/url——没发出请求就留空，不能假装探过。
#   5) **异常要说明白**：HTTP 500 是 error 且带 status_code=500；连上但不回应是 error 且
#      原因里写“超时”。不是一句含糊的“不可用”。
#   6) **权限**：候选列表只给 admin（侦察材料），健康结论 V 可读；未登录 401。
#   7) **没有代理就没有候选**：停掉代理，/apps/discovery 是 503 AGENT_UNAVAILABLE，而不是
#      一份空列表；带健康检查的写入是 503 且库里一行都没多；纯元数据写入不受影响。
#
# 用法（root）：
#   sudo BIN_DIR=/var/tmp/upanel-bin sh tests/integration/p09-2-app-discovery-acceptance.sh
#
# 环境变量：PORT（默认 19482）、WORK、BIN_DIR。
#
# 退出码：0 全部通过；1 有失败项；2 环境不满足。
#
# 明确不验（NOT RUN，原因写在这里，不在脚本里假装通过）：
#   - **容器相关的三项只在有 docker 的机器上跑**（本脚本用 `command -v docker` 判断）。
#     没有 docker 时它们被标成 NOT RUN；有 docker 时第 3b、8b 节真的起两个容器，验
#     “容器候选与发布端口”“running 但 HTTP 500 明确异常”“重建后旧绑定要求重新确认”。
#     `alpine` 镜像拉不下来时同样标 NOT RUN，而不是把“没跑”说成“通过”。
#   - **卷离线禁止启动受管理依赖应用**：P09.2 没有任何启停接口（`capabilities.start=false`），
#     这条验收属于 P09.4 的生命周期动作；本轮只钉住“按钮确实不存在”。
#   - **浏览器里的真实渲染**：脚本验的是页面读到的载荷；DOM 由 vitest 覆盖（P09.5）。

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

WORK=${WORK:-/var/tmp/upanel-accept-p09-2}
PAYLOADS=$WORK/payloads
RUN_DIR=/run/upanel
SOCK=$RUN_DIR/agent.sock
SYS_CFG=/etc/upanel/upanel.json
AGENT_DB_DIR=/var/lib/upanel-agent
AGENT_DB=$AGENT_DB_DIR/agent.db
KEY_FILE=/etc/upanel/agent.key
WEB_DB_DIR=$WORK/webdata
WEB_DB=$WEB_DB_DIR/upanel.db
PORT=${PORT:-19482}
BASE="http://127.0.0.1:$PORT"
ORIGIN="$BASE"
WEB_USER=upanel-web
PASSWORD='vm-correct-horse-9'
VIEWER_PASSWORD='vm-viewer-horse-4'

APPS=/api/v1/apps
USERS=/api/v1/users
AGENT_LOG=$WORK/logs/agent.log
WEB_LOG=$WORK/logs/web.log
AGENT_PID=""
WEB_PID=""
SRV_OK_PID=""
SRV_500_PID=""
SRV_SLOW_PID=""
CREATE_CODE=""

# 三个“被测服务”的端口。它们都只绑回环：候选的 loopback 为真，面板才敢去探。
P_OK=$((PORT + 1))
P_500=$((PORT + 2))
P_SLOW=$((PORT + 3))
# 一个永远没人监听的端口，用来证明“候选只在真的有监听时出现”。
P_DARK=$((PORT + 4))

# 两个容器（只有本机有 docker 时才会起）。端口同样只在本机回环上：
#   CNT_BAD  容器 running，容器里回的是 HTTP 500 —— “容器 running 但 HTTP 500”就是它。
#   CNT_OK   容器 running，回 HTTP 200，用来证明容器候选能被正常探测。
CNT_BAD_PORT=$((PORT + 5))
CNT_OK_PORT=$((PORT + 6))
CNT_BAD_NAME=p092-cnt-bad
CNT_OK_NAME=p092-cnt-ok
CNT_IMAGE=${CNT_IMAGE:-alpine:3.20}
CNT_BAD_ID=""
CNT_OK_ID=""
CNT_READY=0

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

# jget 用 python3 读 JSON：这台机器上没有 jq（与 P08/P09.1 的取舍一致，不为验收脚本装包）。
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

# dbq 在 Web 数据库上跑一条只读查询（先 checkpoint，避免读到旧 WAL 快照）。
dbq() {
  python3 - "$WEB_DB" "$1" <<'PY' 2>/dev/null
import sqlite3, sys
con = sqlite3.connect(sys.argv[1])
try:
    con.execute("PRAGMA wal_checkpoint(TRUNCATE)")
except Exception:
    pass
row = con.execute(sys.argv[2]).fetchone()
print("" if row is None else ("" if row[0] is None else row[0]))
con.close()
PY
}

dbbool() {
  python3 - "$WEB_DB" "$1" <<'PY' 2>/dev/null
import sqlite3, sys
con = sqlite3.connect(sys.argv[1])
try:
    con.execute("PRAGMA wal_checkpoint(TRUNCATE)")
except Exception:
    pass
row = con.execute(sys.argv[2]).fetchone()
print(bool(row[0]) if row else False)
con.close()
PY
}

cleanup() {
  for _pid in "${WEB_PID:-}" "${AGENT_PID:-}" "${SRV_OK_PID:-}" "${SRV_500_PID:-}" "${SRV_SLOW_PID:-}"; do
    if [ -n "$_pid" ] && kill -0 "$_pid" 2>/dev/null; then
      kill -TERM "$_pid" 2>/dev/null
      wait "$_pid" 2>/dev/null
    fi
  done
  # 容器是脚本自己起的夹具，收尾时删掉：不留在机器上，也不占用端口。
  if [ "${HAS_DOCKER:-0}" -eq 1 ]; then
    docker rm -f "$CNT_BAD_NAME" "$CNT_OK_NAME" >/dev/null 2>&1 || true
  fi
  rm -f "$SOCK"
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

stop_agent() {
  if [ -n "${AGENT_PID:-}" ] && kill -0 "$AGENT_PID" 2>/dev/null; then
    kill -TERM "$AGENT_PID" 2>/dev/null
    wait "$AGENT_PID" 2>/dev/null
  fi
  AGENT_PID=""
  rm -f "$SOCK"
  for _ in $(seq 1 50); do
    [ -S "$SOCK" ] || return 0
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

# api 发一次请求，返回状态码，响应体落在 $WORK/resp.json。
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

# api_anon 不带会话（未登录用例）。
api_anon() {
  curl -s -o "$WORK/resp.json" -w '%{http_code}' -X "$1" "$BASE$2" -H "Origin: $ORIGIN"
}

ecode() { jget "$WORK/resp.json" 'd["error"]["code"]'; }
emsg() { jget "$WORK/resp.json" 'd["error"]["message"]'; }
dget() { jget "$WORK/resp.json" "d[\"data\"]$1"; }

# create_app 建一条应用：状态码写进 $CREATE_CODE，响应体留在 $WORK/app.json。
create_app() {
  CREATE_CODE=$(api "$JAR" "$CSRF" POST "$APPS" "$1")
  cp "$WORK/resp.json" "$WORK/app.json"
}

# candidate_for_port 从最近一次发现结果里挑出“监听这个端口的候选”。
candidate_for_port() {
  jget "$WORK/discovery.json" "next((c[\"target_id\"] for c in d[\"data\"][\"candidates\"] if any(l[\"port\"] == $1 for l in c[\"listeners\"])), \"\")"
}

# wait_health 轮询一个应用的健康结论，直到 checked_at 出现（或状态变成 $2）；打印最后的状态。
wait_health() {
  _id=$1; _want=${2:-checked}; _limit=${3:-60}
  _i=0
  _state=""; _checked=""
  while [ "$_i" -lt "$_limit" ]; do
    api "$JAR" "$CSRF" GET "$APPS/$_id/health" >/dev/null
    cp "$WORK/resp.json" "$WORK/health.json"
    _state=$(jget "$WORK/health.json" 'd["data"]["http_state"]')
    _checked=$(jget "$WORK/health.json" 'd["data"]["checked_at"]')
    case "$_want" in
      checked)
        [ "$_checked" != "None" ] && [ -n "$_checked" ] && { printf 'checked'; return 0; } ;;
      *)
        [ "$_state" = "$_want" ] && [ "$_checked" != "None" ] && [ -n "$_checked" ] && {
          printf '%s' "$_state"; return 0; } ;;
    esac
    _i=$((_i + 1))
    sleep 1
  done
  printf 'timeout:%s:%s' "$_state" "$_checked"
}

mkdir -p "$WORK/logs" "$PAYLOADS" "$WEB_DB_DIR"
chown -R "$WEB_USER" "$WEB_DB_DIR"

echo "== 0. 环境与安全守卫 =="
. /etc/os-release
note "系统：$PRETTY_NAME"
expect_eq "以 root 运行" "0" "$(id -u)"
if id "$WEB_USER" >/dev/null 2>&1; then pass "Web 运行身份存在（$WEB_USER）"; else
  fail "Web 运行身份存在" "没有 $WEB_USER 账号"; exit 2; fi
expect_not_contains "Web 身份没有登录 shell" "$(getent passwd "$WEB_USER" | cut -d: -f7)" "/bin/bash"
if command -v python3 >/dev/null 2>&1; then pass "python3 可用（解析 JSON 与读库）"; else
  fail "python3 可用" "没有 python3，脚本无法解析载荷"; exit 2; fi
for _p in "$PORT" "$P_OK" "$P_500" "$P_SLOW" "$P_DARK" "$CNT_BAD_PORT" "$CNT_OK_PORT"; do
  if ss -lntH "sport = :$_p" 2>/dev/null | grep -q .; then
    fail "端口 $_p 空闲" "已被占用"; exit 2
  fi
done
pass "端口 $PORT/$P_OK/$P_500/$P_SLOW/$P_DARK/$CNT_BAD_PORT/$CNT_OK_PORT 空闲"

HAS_DOCKER=0
if command -v docker >/dev/null 2>&1; then
  HAS_DOCKER=1
  note "本机有 docker：$(docker --version 2>/dev/null | head -1)"
else
  note "本机没有 docker：容器相关的三条验收将标记为 NOT RUN"
fi

echo
echo "== 1. 配置与启动（代理 root、Web 非 root） =="
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
  if "$AGENT" --config "$SYS_CFG" -init-key >>"$WORK/logs/init-key.log" 2>&1; then
    pass "-init-key 生成代理密钥"
  else
    fail "-init-key 生成代理密钥" "$(tail -3 "$WORK/logs/init-key.log" 2>/dev/null)"
  fi
fi
if start_agent; then pass "代理以 root 启动并创建 socket"; else fail "代理以 root 启动" "$(tail -5 "$AGENT_LOG" 2>/dev/null)"; fi
expect_eq "代理进程 uid=0" "0" "$(ps -o uid= -p "$AGENT_PID" 2>/dev/null | tr -d ' ')"

printf '%s\n%s\n' "$PASSWORD" "$PASSWORD" | "$CTL" init-admin --config "$SYS_CFG" --db "$WEB_DB" \
  --username admin --display-name "管理员" --json >"$WORK/init.out" 2>"$WORK/init.err"
expect_eq "init-admin 退出码" "0" "$?"
chown_web_db
if start_web; then pass "Web 在 $BASE 启动"; else fail "Web 启动" "$(tail -5 "$WEB_LOG" 2>/dev/null)"; fi
LISTEN_PID=$(ss -lptnH "sport = :$PORT" 2>/dev/null | sed -n 's/.*pid=\([0-9]*\).*/\1/p' | head -1)
[ -z "$LISTEN_PID" ] && LISTEN_PID=$WEB_PID
WEB_UID=$(ps -o uid= -p "$LISTEN_PID" 2>/dev/null | tr -d ' ')
if [ -n "$WEB_UID" ] && [ "$WEB_UID" != "0" ]; then
  pass "监听 $PORT 的 Web 进程不是 root（pid=$LISTEN_PID uid=$WEB_UID）"
else
  fail "监听 $PORT 的 Web 进程不是 root" "pid=${LISTEN_PID:-未知} uid=${WEB_UID:-未知}"
fi
expect_eq "数据库迁移到第 8 版（0008_app_health）" "8" "$(dbq 'SELECT MAX(version) FROM schema_migrations')"

JAR=$WORK/admin.jar
expect_eq "管理员登录" "200" "$(login admin "$PASSWORD" "$JAR" login-admin)"
CSRF=$(csrf_of "$WORK/login-admin.json")
expect_ne "拿到 CSRF 令牌" "" "$CSRF"

echo
echo "== 2. 三个只绑回环的被测服务（发现与探测的对象） =="
# 200 服务：正常站点。
python3 -m http.server "$P_OK" --bind 127.0.0.1 >"$WORK/logs/srv-ok.log" 2>&1 &
SRV_OK_PID=$!
# 500 服务：进程活着、端口在听、状态码是 500 —— “容器 running 但 HTTP 500”的探测层等价物。
cat >"$WORK/srv500.py" <<'PY'
from http.server import BaseHTTPRequestHandler, HTTPServer
import sys
PORT = int(sys.argv[1])
class H(BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(500)
        self.send_header("Content-Length", "0")
        self.end_headers()
    def log_message(self, *a):
        pass
HTTPServer(("127.0.0.1", PORT), H).serve_forever()
PY
python3 "$WORK/srv500.py" "$P_500" >"$WORK/logs/srv-500.log" 2>&1 &
SRV_500_PID=$!
# 只接受连接但永不回应的服务：用来验“超时要说明白”，而不是含糊的“不可用”。
cat >"$WORK/srvslow.py" <<'PY'
import socket, sys, time
PORT = int(sys.argv[1])
s = socket.socket()
s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.bind(("127.0.0.1", PORT))
s.listen(16)
while True:
    conn, _ = s.accept()
    time.sleep(60)
    conn.close()
PY
python3 "$WORK/srvslow.py" "$P_SLOW" >"$WORK/logs/srv-slow.log" 2>&1 &
SRV_SLOW_PID=$!
_ok=1
for _p in "$P_OK" "$P_500" "$P_SLOW"; do
  _i=0; _listening=0
  while [ "$_i" -lt 50 ]; do
    if ss -lntH "sport = :$_p" 2>/dev/null | grep -q .; then _listening=1; break; fi
    _i=$((_i + 1)); sleep 0.1
  done
  [ "$_listening" -eq 1 ] || { _ok=0; note "端口 $_p 没有起来"; }
done
if [ "$_ok" -eq 1 ]; then pass "三个被测服务都在监听（$P_OK/$P_500/$P_SLOW）"; else
  fail "三个被测服务都在监听" "$(tail -2 "$WORK"/logs/srv-*.log 2>/dev/null)"; fi
expect_eq "200 服务的真实响应码" "200" "$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$P_OK/")"
expect_eq "500 服务的真实响应码" "500" "$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$P_500/")"

# 两个容器夹具。容器里用 busybox 的 nc 手工缝一个 HTTP 回应，于是“容器 running 但
# HTTP 500”是真的容器回真的 500，不是探测层伪造的等价物。
if [ "$HAS_DOCKER" -eq 1 ]; then
  if ! docker image inspect "$CNT_IMAGE" >/dev/null 2>&1; then
    note "本机没有 $CNT_IMAGE，尝试拉取（需要外网；失败则容器段标 NOT RUN）"
    docker pull "$CNT_IMAGE" >"$WORK/logs/docker-pull.log" 2>&1 || true
  fi
  if docker image inspect "$CNT_IMAGE" >/dev/null 2>&1; then
    docker rm -f "$CNT_BAD_NAME" "$CNT_OK_NAME" >/dev/null 2>&1 || true
    BAD_BODY='HTTP/1.1 500 Internal Server Error\r\nContent-Length: 4\r\nConnection: close\r\n\r\nboom'
    OK_BODY='HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok'
    docker run -d --name "$CNT_BAD_NAME" -p "$CNT_BAD_PORT:80" "$CNT_IMAGE" \
      sh -c "while true; do printf '$BAD_BODY' | nc -l -p 80; done" >"$WORK/logs/docker-run-bad.log" 2>&1 || true
    docker run -d --name "$CNT_OK_NAME" -p "$CNT_OK_PORT:80" "$CNT_IMAGE" \
      sh -c "while true; do printf '$OK_BODY' | nc -l -p 80; done" >"$WORK/logs/docker-run-ok.log" 2>&1 || true
    _i=0
    while [ "$_i" -lt 60 ]; do
      _bad_code=$(curl -s -m 2 -o /dev/null -w '%{http_code}' "http://127.0.0.1:$CNT_BAD_PORT/" 2>/dev/null || true)
      _ok_code=$(curl -s -m 2 -o /dev/null -w '%{http_code}' "http://127.0.0.1:$CNT_OK_PORT/" 2>/dev/null || true)
      if [ "$_bad_code" = "500" ] && [ "$_ok_code" = "200" ]; then CNT_READY=1; break; fi
      _i=$((_i + 1)); sleep 0.2
    done
    CNT_BAD_ID=$(docker inspect -f '{{.Id}}' "$CNT_BAD_NAME" 2>/dev/null || true)
    CNT_OK_ID=$(docker inspect -f '{{.Id}}' "$CNT_OK_NAME" 2>/dev/null || true)
    if [ "$CNT_READY" -eq 1 ]; then
      pass "两个容器都起来了（$CNT_BAD_NAME → 500，$CNT_OK_NAME → 200）"
    else
      note "容器响应：bad='${_bad_code:-}' ok='${_ok_code:-}'（$(tail -1 "$WORK/logs/docker-run-bad.log" 2>/dev/null)）"
    fi
  else
    note "拉不到 $CNT_IMAGE：容器段标 NOT RUN"
  fi
fi

echo
echo "== 3. 发现：候选来自本机，权限与代理边界都成立 =="
expect_eq "未登录读候选" "401" "$(api_anon GET /api/v1/apps/discovery)"
api "$JAR" "$CSRF" GET /api/v1/apps/discovery >/dev/null
cp "$WORK/resp.json" "$WORK/discovery.json"
cp "$WORK/discovery.json" "$PAYLOADS/discovery-admin.json"
DCODE=$(jget "$WORK/discovery.json" 'd["data"]["status"]')
case "$DCODE" in
  ok|partial) pass "发现状态是 ok/partial（实际 $DCODE）" ;;
  *) fail "发现状态是 ok/partial" "实际 '$DCODE'：$(jget "$WORK/discovery.json" 'd["data"].get("error")')" ;;
esac
expect_ne "候选不是空列表（空列表会被读成“本机什么都没有”）" "0" \
  "$(jget "$WORK/discovery.json" 'len(d["data"]["candidates"])')"

T_OK=$(candidate_for_port "$P_OK")
T_500=$(candidate_for_port "$P_500")
T_SLOW=$(candidate_for_port "$P_SLOW")
T_DARK=$(candidate_for_port "$P_DARK")
expect_ne "200 服务出现在候选里（$P_OK）" "" "$T_OK"
expect_ne "500 服务出现在候选里（$P_500）" "" "$T_500"
expect_ne "不回应的服务出现在候选里（$P_SLOW）" "" "$T_SLOW"
expect_eq "没人监听的端口不在候选里（$P_DARK）" "" "$T_DARK"
note "候选标识：ok=$T_OK 500=$T_500 slow=$T_SLOW"

# 同一个监听（地址+端口+协议）不能同时属于两条候选。真实机器上会发生这件事：
# .socket 单元的监听套接字由 pid 1 持有，守护进程起来后自己也持有同一个端口，
# 于是“0.0.0.0:22”在套接字表里出现两次。列表里并排两条“端口 22”不是信息，是噪音。
DUP_LISTEN=$(jget "$WORK/discovery.json" 'sum(1 for k,v in __import__("collections").Counter((l["address"],l["port"],l["protocol"]) for c in d["data"]["candidates"] for l in c["listeners"]).items() if v > 1)')
expect_eq "候选之间没有重复的监听（地址+端口+协议）" "0" "$DUP_LISTEN"

# 候选标识的形状就是契约：主机名/IP/端口/URL 在这个形状里写不出来。
SHAPE=$(jget "$WORK/discovery.json" 'all(__import__("re").match(r"^(local|cntr)_[a-z0-9][a-z0-9_-]{0,47}$", c["target_id"]) for c in d["data"]["candidates"])')
expect_eq "所有候选的 target_id 形状合法（表达不出主机或 URL）" "True" "$SHAPE"
expect_eq "回环监听被标成可探测" "True" \
  "$(jget "$WORK/discovery.json" "next((str(c[\"probe_available\"]) for c in d[\"data\"][\"candidates\"] if c[\"target_id\"] == \"$T_OK\"), \"missing\")")"
expect_eq "候选里有 loopback 监听（面板才敢探）" "True" \
  "$(jget "$WORK/discovery.json" "next((str(any(l[\"loopback\"] for l in c[\"listeners\"])) for c in d[\"data\"][\"candidates\"] if c[\"target_id\"] == \"$T_OK\"), \"missing\")")"
expect_eq "发现结果里没有已绑定应用（还没建）" "0" \
  "$(jget "$WORK/discovery.json" 'len([c for c in d["data"]["candidates"] if c["bound_app_id"]])')"
expect_eq "没有失效绑定（还没建）" "0" "$(jget "$WORK/discovery.json" 'len(d["data"]["missing_bindings"])')"

# viewer：候选列表是侦察材料，不该给只读用户。
VIEW_CODE=$(api "$JAR" "$CSRF" POST "$USERS" \
  "{\"username\":\"p092-viewer\",\"display_name\":\"只读\",\"role\":\"viewer\",\"initial_password\":\"$VIEWER_PASSWORD\"}")
expect_eq "建 viewer 账号" "201" "$VIEW_CODE"
VJAR=$WORK/viewer.jar
expect_eq "viewer 登录" "200" "$(login p092-viewer "$VIEWER_PASSWORD" "$VJAR" login-viewer)"
VCSRF=$(csrf_of "$WORK/login-viewer.json")
expect_ne "viewer 拿到 CSRF 令牌" "" "$VCSRF"
expect_eq "viewer 读候选列表" "403" "$(api "$VJAR" "$VCSRF" GET /api/v1/apps/discovery)"

echo
echo "== 3b. 容器候选（真 docker） =="
if [ "$HAS_DOCKER" -eq 1 ] && [ "$CNT_READY" -eq 1 ]; then
  api "$JAR" "$CSRF" GET /api/v1/apps/discovery >/dev/null
  cp "$WORK/resp.json" "$WORK/discovery-cnt.json"
  cp "$WORK/resp.json" "$WORK/discovery.json"
  cp "$WORK/discovery-cnt.json" "$PAYLOADS/discovery-containers.json"
  expect_eq "docker 来源这一轮采集成功" "ok" \
    "$(jget "$WORK/discovery-cnt.json" 'next((s["status"] for s in d["data"]["sources"] if s["source"] == "docker"), "")')"
  CB=$(candidate_for_port "$CNT_BAD_PORT")
  CO=$(candidate_for_port "$CNT_OK_PORT")
  expect_ne "HTTP 500 的容器出现在候选里（$CNT_BAD_PORT）" "" "$CB"
  expect_ne "HTTP 200 的容器出现在候选里（$CNT_OK_PORT）" "" "$CO"
  expect_eq "容器候选的身份是完整 64 位容器 ID（不是名字、不是短 ID）" "$CNT_BAD_ID" \
    "$(jget "$WORK/discovery-cnt.json" "next((c.get(\"container_id\", \"\") for c in d[\"data\"][\"candidates\"] if c[\"target_id\"] == \"$CB\"), \"\")")"
  expect_eq "容器候选的 target_id 以 cntr_ 开头（容器无法被说成任意主机）" "True" \
    "$(case "$CB" in cntr_*) echo True ;; *) echo False ;; esac)"
  expect_eq "容器候选的建议端口就是它发布的宿主机端口" "$CNT_BAD_PORT" \
    "$(jget "$WORK/discovery-cnt.json" "next((c[\"suggested_port\"] for c in d[\"data\"][\"candidates\"] if c[\"target_id\"] == \"$CB\"), 0)")"
  expect_eq "容器候选能探测（端口发布在回环上）" "True" \
    "$(jget "$WORK/discovery-cnt.json" "next((str(c[\"probe_available\"]) for c in d[\"data\"][\"candidates\"] if c[\"target_id\"] == \"$CB\"), \"missing\")")"
  # docker-proxy 会替发布端口监听一遍；那不是第二个服务。第 3 节的“没有重复监听”已经
  # 覆盖了这条，这里点名容器端口，避免它只在“恰好没有容器”的时候成立。
  expect_eq "容器发布端口没有额外生出一条 local_ 候选" "True" \
    "$(jget "$WORK/discovery-cnt.json" "all(not c[\"target_id\"].startswith(\"local_\") for c in d[\"data\"][\"candidates\"] if any(l[\"port\"] in ($CNT_BAD_PORT, $CNT_OK_PORT) for l in c[\"listeners\"]))")"
else
  notrun "容器候选与容器端口" "本机没有可用的 docker，或两个容器没起来"
fi

echo
echo "== 4. 确认候选 = 创建应用（没有第二个“确认”接口） =="
create_app '{"name":"正常站点","type":"link","public_url":"http://192.168.56.10:9000/"}'
expect_eq "建一个 link 应用" "201" "$CREATE_CODE"
LINK_ID=$(jget "$WORK/app.json" 'd["data"]["id"]')
expect_eq "link 的能力里没有启停按钮" "False" \
  "$(jget "$WORK/app.json" 'any(bool(d["data"]["capabilities"][k]) for k in ("start","stop","restart","logs"))')"
expect_eq "link 的 http_state 是 not_applicable" "not_applicable" \
  "$(jget "$WORK/app.json" 'd["data"]["http_state"]')"

# 形状合法但发现结果里没有的目标：422，并指出是哪一个。
GHOST_CODE=$(api "$JAR" "$CSRF" POST "$APPS" \
  '{"name":"幽灵目标","type":"link","public_url":"http://192.168.56.10:9001/","health_check":{"enabled":true,"target_id":"local_not_there","port":8080}}')
cp "$WORK/resp.json" "$PAYLOADS/create-ghost.json"
expect_eq "未发现的 target_id 被拒" "422" "$GHOST_CODE"
expect_eq "422 的 details 指出是哪个 target" "local_not_there" \
  "$(jget "$WORK/resp.json" 'str(d["error"]["details"]["target_id"])')"
expect_eq "未发现的 target 没有落库" "0" "$(dbq "SELECT COUNT(*) FROM apps WHERE name = '幽灵目标'")"

# 不是 target 形状的东西：400（形状校验，在写入前）。
for _bad in 169.254.169.254 localhost:8080 http://127.0.0.1:8080/ Local_1 host_1; do
  BAD_CODE=$(api "$JAR" "$CSRF" POST "$APPS" \
    "{\"name\":\"坏目标\",\"type\":\"link\",\"public_url\":\"http://192.168.56.10:9/\",\"health_check\":{\"enabled\":true,\"target_id\":\"$_bad\",\"port\":80}}")
  expect_eq "非 target 形状被拒（$_bad）" "400" "$BAD_CODE"
done
expect_eq "坏目标一条都没落库" "0" "$(dbq "SELECT COUNT(*) FROM apps WHERE name = '坏目标'")"

echo
echo "== 5. 健康探测：三种 unknown 分得开 =="
# 未启用健康检查：not_applicable，并且说明不会发请求。
api "$JAR" "$CSRF" GET "$APPS/$LINK_ID/health" >/dev/null
cp "$WORK/resp.json" "$PAYLOADS/health-disabled.json"
expect_eq "未启用时 http_state" "not_applicable" "$(dget '["http_state"]')"
expect_eq "未启用时 checked_at 是 null" "None" "$(dget '["checked_at"]')"
expect_ne "未启用时必须说明原因" "" "$(dget '["note"]')"

# 启用但从没探过：unknown + checked_at null。新建之后立即读（调度器 5 秒一拍，这里抢先读）。
create_app "{\"name\":\"待探测站点\",\"type\":\"link\",\"public_url\":\"http://192.168.56.10:9002/\",\"health_check\":{\"enabled\":true,\"target_id\":\"$T_OK\",\"port\":$P_OK,\"path\":\"/\",\"interval_seconds\":30,\"timeout_seconds\":3}}"
expect_eq "带健康检查的应用创建成功" "201" "$CREATE_CODE"
PENDING_ID=$(jget "$WORK/app.json" 'd["data"]["id"]')
api "$JAR" "$CSRF" GET "$APPS/$PENDING_ID/health" >/dev/null
FIRST_STATE=$(dget '["http_state"]')
FIRST_CHECKED=$(dget '["checked_at"]')
if [ "$FIRST_CHECKED" = "None" ]; then
  expect_eq "从没探过时 http_state 是 unknown" "unknown" "$FIRST_STATE"
  expect_ne "从没探过时必须说明原因" "" "$(dget '["note"]')"
else
  # 调度器可能在这两行之间已经探完了一轮。那不是失败，但也不能当“从没探过”通过。
  note "读到的时候已经探过一轮（checked_at=$FIRST_CHECKED）"
  notrun "从没探过的 unknown" "调度器在本项读取前已完成首轮探测（5 秒一拍）"
fi

echo
echo "== 6. 真实结论：200 → ok，500 → error（带状态码），不回应 → error（带超时） =="
create_app "{\"name\":\"200 服务\",\"type\":\"link\",\"public_url\":\"http://192.168.56.10:9010/\",\"health_check\":{\"enabled\":true,\"target_id\":\"$T_OK\",\"port\":$P_OK,\"path\":\"/\",\"interval_seconds\":30,\"timeout_seconds\":3}}"
OK_ID=$(jget "$WORK/app.json" 'd["data"]["id"]')
create_app "{\"name\":\"500 服务\",\"type\":\"link\",\"public_url\":\"http://192.168.56.10:9011/\",\"health_check\":{\"enabled\":true,\"target_id\":\"$T_500\",\"port\":$P_500,\"path\":\"/\",\"interval_seconds\":30,\"timeout_seconds\":3}}"
FIVE_ID=$(jget "$WORK/app.json" 'd["data"]["id"]')
create_app "{\"name\":\"不回应服务\",\"type\":\"link\",\"public_url\":\"http://192.168.56.10:9012/\",\"health_check\":{\"enabled\":true,\"target_id\":\"$T_SLOW\",\"port\":$P_SLOW,\"path\":\"/\",\"interval_seconds\":30,\"timeout_seconds\":1}}"
SLOW_ID=$(jget "$WORK/app.json" 'd["data"]["id"]')

expect_eq "200 服务的探测结论" "ok" "$(wait_health "$OK_ID" ok 40)"
cp "$WORK/health.json" "$PAYLOADS/health-ok.json"
expect_eq "ok 记录里的状态码" "200" "$(dget '["status_code"]')"
expect_eq "探测地址是回环" "127.0.0.1" "$(dget '["address"]')"
expect_contains "探测 URL 指向 127.0.0.1 的同一端口" "http://127.0.0.1:$P_OK/" "$(dget '["url"]')"

expect_eq "500 服务的探测结论" "error" "$(wait_health "$FIVE_ID" error 40)"
cp "$WORK/health.json" "$PAYLOADS/health-500.json"
expect_eq "error 记录带真实状态码 500（不是含糊的“不可用”）" "500" "$(dget '["status_code"]')"
expect_ne "500 必须说明原因" "" "$(dget '["last_error"]')"

expect_eq "不回应的服务探测结论" "error" "$(wait_health "$SLOW_ID" error 60)"
cp "$WORK/health.json" "$PAYLOADS/health-timeout.json"
expect_contains "超时要说明白（原因里提到超时）" "超时" "$(dget '["last_error"]')"
expect_eq "没拿到状态码时 status_code 是 null（不是 0）" "None" "$(dget '["status_code"]')"

# 落库的地址也必须只有回环：这是“面板不会请求任意地址”的可复核证据。
expect_eq "库里的探测地址全是 127.0.0.1" "True" \
  "$(dbbool "SELECT COUNT(*) > 0 AND SUM(CASE WHEN address <> '127.0.0.1' THEN 1 ELSE 0 END) = 0 FROM app_health_results")"
expect_eq "库里的 URL 只有 http(s)://127.0.0.1 前缀" "True" \
  "$(dbbool "SELECT SUM(CASE WHEN url NOT LIKE 'http://127.0.0.1:%' AND url NOT LIKE 'https://127.0.0.1:%' THEN 1 ELSE 0 END) = 0 FROM app_health_results WHERE url <> ''")"
expect_eq "探测记录里没有云元数据地址" "0" \
  "$(dbq "SELECT COUNT(*) FROM app_health_results WHERE url LIKE '%169.254.169.254%' OR address LIKE '%169.254%'")"
expect_eq "app_health_results 一行一个应用" "True" \
  "$(dbbool "SELECT COUNT(*) = COUNT(DISTINCT app_id) FROM app_health_results")"

# viewer 能读健康结论（V），但读不到候选列表（A，第 3 节已验）。
expect_eq "viewer 读健康结论" "200" "$(api "$VJAR" "$VCSRF" GET "$APPS/$OK_ID/health")"

echo
echo "== 7. 服务停掉之后：结论必须变成 error，而不是停在 ok =="
kill -TERM "$SRV_OK_PID" 2>/dev/null
wait "$SRV_OK_PID" 2>/dev/null
SRV_OK_PID=""
_i=0
while [ "$_i" -lt 50 ]; do
  ss -lntH "sport = :$P_OK" 2>/dev/null | grep -q . || break
  _i=$((_i + 1)); sleep 0.1
done
api "$JAR" "$CSRF" GET "$APPS/$OK_ID/health" >/dev/null
cp "$WORK/resp.json" "$WORK/health-before.json"
BEFORE_CHECKED=$(jget "$WORK/health-before.json" 'd["data"]["checked_at"]')
expect_eq "停服务之前结论是 ok" "ok" "$(jget "$WORK/health-before.json" 'd["data"]["http_state"]')"
# 下一次探测要等这个应用自己的 interval（30 秒）：轮询到结论变化为止。
NEW_STATE=$(wait_health "$OK_ID" error 60)
cp "$WORK/health.json" "$PAYLOADS/health-stopped.json"
AFTER_CHECKED=$(jget "$WORK/health.json" 'd["data"]["checked_at"]')
expect_eq "服务停掉后结论变成 error" "error" "$NEW_STATE"
expect_ne "checked_at 前进了（是新的观测，不是旧结论）" "$BEFORE_CHECKED" "$AFTER_CHECKED"
expect_eq "连不上时 status_code 是 null" "None" "$(dget '["status_code"]')"
expect_ne "连不上必须说明原因" "" "$(dget '["last_error"]')"

echo
echo "== 8. 代理不可用：503 而不是空列表，unknown 而不是 error =="
# 8a. 发现接口：停掉代理后是 503 AGENT_UNAVAILABLE。
stop_agent
expect_eq "代理停掉后读候选" "503" "$(api "$JAR" "$CSRF" GET /api/v1/apps/discovery)"
expect_eq "503 的错误码" "AGENT_UNAVAILABLE" "$(ecode)"
RAW=$(cat "$WORK/resp.json")
expect_not_contains "503 的响应里没有空候选列表" '"candidates"' "$RAW"

# 8b. 带健康检查的写入：503，并且库里一行都不多；纯元数据写入不受影响。
BEFORE_APPS=$(dbq "SELECT COUNT(*) FROM apps")
DOWN_CODE=$(api "$JAR" "$CSRF" POST "$APPS" \
  "{\"name\":\"代理停了还要建\",\"type\":\"link\",\"public_url\":\"http://192.168.56.10:9020/\",\"health_check\":{\"enabled\":true,\"target_id\":\"$T_500\",\"port\":$P_500}}")
cp "$WORK/resp.json" "$PAYLOADS/create-agent-down.json"
expect_eq "代理不可用时带健康检查的写入" "503" "$DOWN_CODE"
expect_eq "503 的错误码是 AGENT_UNAVAILABLE" "AGENT_UNAVAILABLE" "$(ecode)"
expect_eq "代理不可用时一行都没写" "$BEFORE_APPS" "$(dbq 'SELECT COUNT(*) FROM apps')"
PLAIN_CODE=$(api "$JAR" "$CSRF" POST "$APPS" '{"name":"纯元数据","type":"link","public_url":"http://192.168.56.10:9021/"}')
expect_eq "不带健康检查的写入不受代理影响" "201" "$PLAIN_CODE"

# 8c. 探测：代理不可用时写 unknown（不是 error），并且不填地址。
if start_agent; then pass "代理恢复启动"; else fail "代理恢复启动" "$(tail -3 "$AGENT_LOG")"; fi
create_app "{\"name\":\"代理掉线时的探测\",\"type\":\"link\",\"public_url\":\"http://192.168.56.10:9022/\",\"health_check\":{\"enabled\":true,\"target_id\":\"$T_500\",\"port\":$P_500,\"path\":\"/\",\"interval_seconds\":30,\"timeout_seconds\":3}}"
expect_eq "建一个指向 500 服务的应用（此时代理在线）" "201" "$CREATE_CODE"
UNK_ID=$(jget "$WORK/app.json" 'd["data"]["id"]')
FIRST=$(wait_health "$UNK_ID" checked 40)
note "代理在线时的首轮结论：$FIRST"
stop_agent
UNK_STATE=$(wait_health "$UNK_ID" unknown 90)
cp "$WORK/health.json" "$PAYLOADS/health-agent-down.json"
expect_eq "代理不可用时的结论是 unknown（不是 error）" "unknown" "$UNK_STATE"
expect_contains "unknown 必须说明是代理不可用" "代理不可用" "$(dget '["last_error"]')"
expect_eq "没有发出请求时 address 留空" "None" "$(dget '["address"]')"
expect_eq "没有发出请求时 url 留空" "None" "$(dget '["url"]')"
expect_eq "没有发出请求时 status_code 是 null" "None" "$(dget '["status_code"]')"
expect_eq "库里的 unknown 记录 address 是空串而不是 127.0.0.1" "0" \
  "$(dbq "SELECT COUNT(*) FROM app_health_results WHERE http_state = 'unknown' AND address <> ''")"
if start_agent; then pass "代理再次启动"; else fail "代理再次启动" "$(tail -3 "$AGENT_LOG")"; fi

echo
echo "== 8b. 容器：running 但 HTTP 500 明确异常；重建后旧绑定要求重新确认 =="
if [ "$HAS_DOCKER" -eq 1 ] && [ "$CNT_READY" -eq 1 ]; then
  # 确认候选就是创建应用：绑定用完整容器 ID，健康检查指向容器的发布端口。
  create_app "{\"name\":\"容器站点\",\"type\":\"docker\",\"binding\":{\"container_id\":\"$CNT_BAD_ID\"},\"public_url\":\"http://192.168.56.10:9031/\",\"health_check\":{\"enabled\":true,\"target_id\":\"$CB\",\"port\":$CNT_BAD_PORT,\"path\":\"/\",\"interval_seconds\":30,\"timeout_seconds\":3}}"
  expect_eq "用容器候选建应用（确认候选就是 POST /apps）" "201" "$CREATE_CODE"
  CNT_APP_ID=$(jget "$WORK/app.json" 'd["data"]["id"]')
  expect_eq "应用类型是 docker" "docker" "$(jget "$WORK/app.json" 'd["data"]["type"]')"
  CSTATE=$(wait_health "$CNT_APP_ID" error 60)
  cp "$WORK/health.json" "$PAYLOADS/health-container-500.json"
  expect_eq "容器 running 但回 500：结论是 error（不是 ok、不是 unknown）" "error" "$CSTATE"
  expect_eq "容器 500 带真实状态码 500" "500" "$(dget '["status_code"]')"
  expect_contains "容器 500 要说明原因" "500" "$(dget '["last_error"]')"
  expect_eq "容器探测地址仍然是回环" "127.0.0.1" "$(dget '["address"]')"
  expect_contains "容器探测 URL 指向 127.0.0.1 的发布端口" "127.0.0.1:$CNT_BAD_PORT" "$(dget '["url"]')"

  # 重建：删掉容器，用同一个名字起一个新的。名字一样、ID 不一样——这就是 STAGES 第 11 节
  # 点名的场景。面板不能因为名字相同就把旧应用接到这个没人确认过的新容器上。
  docker rm -f "$CNT_BAD_NAME" >/dev/null 2>&1 || true
  docker run -d --name "$CNT_BAD_NAME" -p "$CNT_BAD_PORT:80" "$CNT_IMAGE" \
    sh -c "while true; do printf '$BAD_BODY' | nc -l -p 80; done" >"$WORK/logs/docker-run-bad2.log" 2>&1 || true
  _i=0
  while [ "$_i" -lt 60 ]; do
    _code=$(curl -s -m 2 -o /dev/null -w '%{http_code}' "http://127.0.0.1:$CNT_BAD_PORT/" 2>/dev/null || true)
    [ "$_code" = "500" ] && break
    _i=$((_i + 1)); sleep 0.2
  done
  NEW_ID=$(docker inspect -f '{{.Id}}' "$CNT_BAD_NAME" 2>/dev/null || true)
  expect_ne "重建后容器 ID 真的变了（名字照旧）" "$CNT_BAD_ID" "$NEW_ID"

  # 发现结果有 15 秒缓存：等快照追上“旧容器已经不在了”这个事实，再看合并结果。
  _i=0; MISSING_REASON=""
  while [ "$_i" -lt 40 ]; do
    api "$JAR" "$CSRF" GET /api/v1/apps/discovery >/dev/null
    cp "$WORK/resp.json" "$WORK/discovery-rebuild.json"
    MISSING_REASON=$(jget "$WORK/discovery-rebuild.json" "next((m.get(\"reason\", \"\") for m in d[\"data\"][\"missing_bindings\"] if m.get(\"app_id\") == \"$CNT_APP_ID\"), \"\")")
    [ -n "$MISSING_REASON" ] && break
    _i=$((_i + 1)); sleep 1
  done
  cp "$WORK/discovery-rebuild.json" "$PAYLOADS/discovery-rebuild.json"
  expect_ne "重建后旧绑定进 missing_bindings" "" "$MISSING_REASON"
  expect_contains "旧绑定要求重新确认（而不是自动接到新容器上）" "重新确认" "$MISSING_REASON"
  api "$JAR" "$CSRF" GET "$APPS/$CNT_APP_ID" >/dev/null
  expect_contains "应用记录里的绑定仍是旧容器 ID（没有被偷偷改绑）" "$CNT_BAD_ID" \
    "$(jget "$WORK/resp.json" 'json.dumps(d["data"].get("binding") or {}, ensure_ascii=False)')"
  expect_eq "新容器是一个独立的候选，没有自动绑到那个应用上" "0" \
    "$(jget "$WORK/discovery-rebuild.json" "len([c for c in d[\"data\"][\"candidates\"] if c.get(\"container_id\") == \"$NEW_ID\" and c.get(\"bound_app_id\")])")"
  expect_eq "旧容器 ID 已经不是候选（它真的没了）" "0" \
    "$(jget "$WORK/discovery-rebuild.json" "len([c for c in d[\"data\"][\"candidates\"] if c.get(\"container_id\") == \"$CNT_BAD_ID\"])")"

  # 反过来的一刀：**没采成**不等于**不在了**。停掉 docker 守护进程（容器跟着停），
  # docker 来源会如实变成 unavailable。这时那个容器应用不许出现在 missing_bindings 里
  # ——否则用户会照着“容器不在了”去删一条其实好好的应用。整个快照的 sources 已经写着
  # 哪个来源失败了，那才是该看的地方。
  systemctl stop docker.socket docker.service >/dev/null 2>&1 || true
  _i=0; DSRC=""; FALSE_ISSUE=""
  while [ "$_i" -lt 45 ]; do
    api "$JAR" "$CSRF" GET /api/v1/apps/discovery >/dev/null
    cp "$WORK/resp.json" "$WORK/discovery-docker-down.json"
    DSRC=$(jget "$WORK/discovery-docker-down.json" 'next((s["status"] for s in d["data"]["sources"] if s["source"] == "docker"), "")')
    FALSE_ISSUE=$(jget "$WORK/discovery-docker-down.json" "next((m.get(\"reason\", \"\") for m in d[\"data\"][\"missing_bindings\"] if m.get(\"app_id\") == \"$CNT_APP_ID\"), \"\")")
    case "$DSRC" in unavailable|error) break ;; esac
    _i=$((_i + 1)); sleep 1
  done
  cp "$WORK/discovery-docker-down.json" "$PAYLOADS/discovery-docker-down.json"
  # 状态词用 unavailable（没守护进程/没命令）还是 error（守护进程在但读不出来）取决于
  # 现场：停掉 docker.socket 后套接字文件有时还在，`docker ps` 就会失败成 error。
  # 两者都算“这一轮没采到”，关键是**不能是 ok**。
  case "$DSRC" in
    unavailable|error) pass "docker 采不到时来源状态如实是 $DSRC（不是 ok）" ;;
    *) fail "docker 采不到时来源状态如实是 unavailable/error" "实际 '$DSRC'" ;;
  esac
  expect_eq "docker 采不到时，容器应用不被报成“容器不在了”" "" "$FALSE_ISSUE"
  expect_eq "一个来源失败时总状态是 partial（失败要说出来）" "partial" \
    "$(jget "$WORK/discovery-docker-down.json" 'd["data"]["status"]')"
  expect_contains "失败原因写在快照上，而不是伪装成“容器不存在”" "docker" \
    "$(jget "$WORK/discovery-docker-down.json" 'd["data"].get("error") or ""')"
  systemctl start docker.socket docker.service >/dev/null 2>&1 || true
else
  notrun "容器 running 但 HTTP 500" "本机没有可用的 docker，或两个容器没起来"
  notrun "容器重建后旧绑定要求重新确认" "本机没有可用的 docker，或两个容器没起来"
fi

echo
echo "== 9. 本轮不验的（明确标出来） =="
if [ "$HAS_DOCKER" -ne 1 ]; then
  notrun "容器候选与容器端口" "本机没有 docker，第 3b 节没有执行"
  notrun "容器 running 但 HTTP 500" "本机没有 docker（探测层的同一断言已在第 6 节用真实 500 覆盖）"
  notrun "容器重建后旧绑定要求重新确认" "本机没有 docker（合并逻辑由 internal/api 单元用例覆盖）"
fi
if [ "$CNT_READY" -ne 1 ] && [ "$HAS_DOCKER" -eq 1 ]; then
  notrun "容器相关的三项（3b/8b）" "有 docker，但两个容器夹具没起来（镜像拉不下来或端口被占）"
fi
notrun "卷离线禁止启动受管理依赖应用" "P09.2 没有启停接口（capabilities.start=false），属于 P09.4"
# 这一条不是 NOT RUN：现在就该钉住“按钮不存在”，否则页面会先渲染再失败。
api "$JAR" "$CSRF" GET "$APPS/$OK_ID" >/dev/null
expect_eq "P09.2 里 start/stop/restart/logs 全为 false" "False" \
  "$(jget "$WORK/resp.json" 'any(bool(d["data"]["capabilities"][k]) for k in ("start","stop","restart","logs"))')"

echo
echo "== 10. 收尾不变量 =="
FINAL_PID=$(ss -lptnH "sport = :$PORT" 2>/dev/null | sed -n 's/.*pid=\([0-9]*\).*/\1/p' | head -1)
FINAL_UID=$(ps -o uid= -p "${FINAL_PID:-0}" 2>/dev/null | tr -d ' ')
if [ -n "$FINAL_PID" ] && [ -n "$FINAL_UID" ] && [ "$FINAL_UID" != "0" ]; then
  pass "监听 $PORT 的 Web 进程到最后都不是 root（uid=$FINAL_UID）"
else
  fail "监听 $PORT 的 Web 进程到最后都不是 root" "pid=${FINAL_PID:-无} uid=${FINAL_UID:-无}"
fi
expect_eq "代理进程仍是 root" "0" "$(ps -o uid= -p "$AGENT_PID" 2>/dev/null | tr -d ' ')"
expect_eq "应用表与健康表都在" "2" \
  "$(dbq "SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name IN ('apps','app_health_results')")"
if grep -qiE 'panic|fatal' "$WEB_LOG" "$AGENT_LOG" 2>/dev/null; then
  fail "日志里没有 panic/fatal" "$(grep -icE 'panic|fatal' "$WEB_LOG" "$AGENT_LOG" 2>/dev/null | tr '\n' ' ')"
else
  pass "日志里没有 panic/fatal"
fi
if grep -q '169.254.169.254' "$WEB_LOG" "$AGENT_LOG" 2>/dev/null; then
  fail "日志里没有云元数据地址的请求" "$(grep -c '169.254.169.254' "$WEB_LOG" "$AGENT_LOG" 2>/dev/null | tr '\n' ' ')"
else
  pass "日志里没有云元数据地址的请求"
fi

echo
echo "=========================== 汇总 ==========================="
printf 'PASS %s / FAIL %s / NOT RUN %s\n' "$PASSED" "$FAILED" "$NOTRUN"
if [ -n "$FAILED_NAMES" ]; then printf '失败项:%s\n' "$FAILED_NAMES"; fi
printf 'NOT RUN 不等于通过。\n'
echo "============================================================"
[ "$FAILED" -eq 0 ] || exit 1
exit 0
