#!/bin/sh
# P03.3 Linux 验收脚本：SSE 事件流、断线重连、日志限额、任务中心契约与错误详情。
#
# 运行位置：Ubuntu 22.04/24.04 验收 VM 内，**必须以 root 运行**（代理只能以 root 启动）。
# 不依赖 Go 工具链：二进制由宿主交叉编译，脚本只做黑盒验收与查库。
#
# 用法：
#   sudo BIN_DIR=/tmp/upanel-bin sh tests/integration/p03-3-events-acceptance.sh
#
# 退出码：0 全部通过；1 有失败项；2 环境不满足（非 root）。
#
# 覆盖的验收点（STAGES P03.3、CONTRACTS §4.3、PLAN §6）：
#   1. GET /jobs/:id/events 是 text/event-stream：首帧注释、id/event/data 三段、seq 递增
#   2. 事件由代理产生、Web 镜像（payload 带 agent_seq/agent_kind），不是 Web 自造的进度
#   3. 断线重连带 Last-Event-ID：不重发已看过的事件，也不重新执行任务
#   4. 关闭事件流（等价于关掉浏览器标签）不影响任务执行到底
#   5. 非法 Last-Event-ID → 400；未登录 → 401；viewer → 403（流与日志都只有 admin）
#   6. 历史被清理后重连先收到 gap（history_pruned），而不是假装没有新事件
#   7. 日志分页：limit 边界、cursor 续页、非法 cursor → 400
#   8. 每个账号最多 4 条事件流，第 5 条 429；关闭后名额释放
#   9. job_events 只追加：状态事件不能删；日志事件可按容量回收
#  10. 终态不能回到 running（状态机由数据库触发器兜底）
#
# 说明：日志“超出 1000 行时删最旧的日志、保留状态事件”的裁剪行为在宿主单元测试
#   TestPruneJobLogCapacity 中验证；VM 这里验证的是迁移 0006 落地后的触发器语义。

set -u

BIN_DIR=${BIN_DIR:-/tmp/upanel-bin}
if [ "$(id -u)" -ne 0 ]; then
  printf '本脚本必须以 root 运行：sudo BIN_DIR=%s sh %s\n' "$BIN_DIR" "$0" >&2
  exit 2
fi

AGENT="$BIN_DIR/upanel-agent"
WEB="$BIN_DIR/upanel-web"
CTL="$BIN_DIR/upanelctl"

WORK=${WORK:-/var/tmp/upanel-accept-p03-3}
RUN_DIR=/run/upanel
SOCK=$RUN_DIR/agent.sock
SYS_CFG=/etc/upanel/upanel.json
AGENT_DB_DIR=/var/lib/upanel-agent
AGENT_DB=$AGENT_DB_DIR/agent.db
KEY_FILE=/etc/upanel/agent.key
WEB_DB_DIR=$WORK/webdata
WEB_DB=$WEB_DB_DIR/upanel.db
PORT=19445
BASE="http://127.0.0.1:$PORT"
ORIGIN="$BASE"
WEB_USER=upanel-web
PASSWORD='vm-correct-horse-9'
REPLAY_PASSWORD='vm-replay-horse-7'

AGENT_LOG=$WORK/logs/agent.log
WEB_LOG=$WORK/logs/web.log

PASSED=0
FAILED=0
FAILED_NAMES=""
AGENT_PID=""
WEB_PID=""
STREAM_PIDS=""

pass() { PASSED=$((PASSED + 1)); printf 'PASS  %s\n' "$1"; }
fail() { FAILED=$((FAILED + 1)); FAILED_NAMES="$FAILED_NAMES | $1"; printf 'FAIL  %s: %s\n' "$1" "${2:-}"; }
note() { printf 'NOTE  %s\n' "$1"; }

expect_eq() { if [ "$2" = "$3" ]; then pass "$1"; else fail "$1" "期望 '$2'，实际 '$3'"; fi; }

expect_contains() {
  case $3 in
    *"$2"*) pass "$1" ;;
    *) fail "$1" "缺少 '$2'（实际: $3）" ;;
  esac
}

expect_not_contains() {
  case $3 in
    *"$2"*) fail "$1" "不应出现 '$2'" ;;
    *) pass "$1" ;;
  esac
}

# jget <文件> <python 表达式，d 为解析后的 JSON>
jget() {
  python3 -c 'import json,sys
d=json.load(open(sys.argv[1], encoding="utf-8"))
print(eval(sys.argv[2]))' "$1" "$2" 2>/dev/null
}

# dbq <数据库> <SQL>：只读查询，返回第一行第一列。
dbq() {
  python3 - "$1" "$2" <<'PY'
import sqlite3, sys
con = sqlite3.connect("file:%s?mode=ro" % sys.argv[1], uri=True)
try:
    row = con.execute(sys.argv[2]).fetchone()
    print("" if row is None else ("" if row[0] is None else row[0]))
finally:
    con.close()
PY
}

# dbexec <数据库> <SQL>：写操作以 Web 账号身份执行（保持文件属主不变）。
dbexec() {
  runuser -u "$WEB_USER" -- python3 - "$1" "$2" <<'PY'
import sqlite3, sys
con = sqlite3.connect(sys.argv[1], timeout=5)
try:
    con.execute(sys.argv[2])
    con.commit()
finally:
    con.close()
PY
}

# dbrefuse <数据库> <期望被拒绝的 SQL>：语句必须失败（触发器/约束生效）。
dbrefuse() {
  if runuser -u "$WEB_USER" -- python3 - "$1" "$2" >/dev/null 2>&1 <<'PY'
import sqlite3, sys
con = sqlite3.connect(sys.argv[1], timeout=5)
try:
    con.execute(sys.argv[2])
    con.commit()
except sqlite3.Error as exc:
    print(exc)
    sys.exit(0)
con.close()
sys.exit(1)
PY
  then
    return 0
  fi
  return 1
}

cleanup() {
  for pid in $STREAM_PIDS; do kill -TERM "$pid" 2>/dev/null; done
  STREAM_PIDS=""
  if [ -n "$WEB_PID" ] && kill -0 "$WEB_PID" 2>/dev/null; then
    kill -TERM "$WEB_PID" 2>/dev/null
    wait "$WEB_PID" 2>/dev/null
  fi
  if [ -n "$AGENT_PID" ] && kill -0 "$AGENT_PID" 2>/dev/null; then
    kill -TERM "$AGENT_PID" 2>/dev/null
    wait "$AGENT_PID" 2>/dev/null
  fi
  rm -f "$SOCK"
}
trap cleanup EXIT
trap 'cleanup; exit 130' INT
trap 'cleanup; exit 143' TERM

start_agent() {
  "$AGENT" --config "$SYS_CFG" -acceptance-actions >>"$AGENT_LOG" 2>&1 &
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

login() {
  curl -s -c "$3" -o "$WORK/$4.json" -w '%{http_code}' -X POST "$BASE/api/v1/auth/login" \
    -H 'Content-Type: application/json' -H 'X-UPanel-Request: 1' -H "Origin: $ORIGIN" \
    -d "{\"username\":\"$1\",\"password\":\"$2\"}"
}

csrf_of() { jget "$1" 'd["data"]["csrf_token"]'; }

# api <jar> <csrf> <方法> <路径> [body] [idempotency-key]
api() {
  jar=$1; csrf=$2; method=$3; path=$4; body=${5:-}; key=${6:-}
  if [ -n "$body" ]; then
    printf '%s' "$body" >"$WORK/req.json"
    if [ -n "$key" ]; then
      curl -s -b "$jar" -o "$WORK/resp.json" -w '%{http_code}' -X "$method" "$BASE$path" \
        -H 'Content-Type: application/json' -H "Origin: $ORIGIN" -H "X-CSRF-Token: $csrf" \
        -H "Idempotency-Key: $key" -d @"$WORK/req.json"
    else
      curl -s -b "$jar" -o "$WORK/resp.json" -w '%{http_code}' -X "$method" "$BASE$path" \
        -H 'Content-Type: application/json' -H "Origin: $ORIGIN" -H "X-CSRF-Token: $csrf" \
        -d @"$WORK/req.json"
    fi
  else
    curl -s -b "$jar" -o "$WORK/resp.json" -w '%{http_code}' -X "$method" "$BASE$path" \
      -H "Origin: $ORIGIN" -H "X-CSRF-Token: $csrf"
  fi
}

# sse_collect <json 名> <路径> <Last-Event-ID 或空> <最长秒数>
# 把流写进 $WORK/<name>.sse，把响应头写进 $WORK/<name>.hdr；返回 curl 退出码（超时为 28）。
sse_collect() {
  _name=$1; _path=$2; _last=$3; _secs=$4
  if [ -n "$_last" ]; then
    curl -sN -b "$WORK/admin.jar" -D "$WORK/$_name.hdr" -o "$WORK/$_name.sse" \
      --max-time "$_secs" -H "Last-Event-ID: $_last" -H "Origin: $ORIGIN" \
      "$BASE$_path" >/dev/null 2>&1
  else
    curl -sN -b "$WORK/admin.jar" -D "$WORK/$_name.hdr" -o "$WORK/$_name.sse" \
      --max-time "$_secs" -H "Origin: $ORIGIN" "$BASE$_path" >/dev/null 2>&1
  fi
  return $?
}

sse_field() { grep -a "^$2: " "$1" 2>/dev/null | sed "s/^$2: //" | tr -d '\r'; }
# 业务事件帧数（注释心跳不算）。grep 无匹配时退出码为 1 且输出 "0"，不能再用 || echo 补一次。
sse_count() {
  if [ ! -f "$1" ]; then echo 0; return; fi
  _count=$(grep -ac "^event: " "$1" 2>/dev/null || true)
  echo "${_count:-0}"
}

wait_for_status() {
  _job=$1; _want=$2; _secs=${3:-10}
  _n=0
  while [ "$_n" -lt "$((_secs * 10))" ]; do
    api "$WORK/admin.jar" "$ADMIN_CSRF" GET "/api/v1/jobs/$_job" >/dev/null
    _cur=$(jget "$WORK/resp.json" 'd["data"]["status"]')
    [ "$_cur" = "$_want" ] && return 0
    case $_cur in
      succeeded|failed|canceled|interrupted) return 1 ;;
    esac
    sleep 0.1
    _n=$((_n + 1))
  done
  return 1
}

PREFLIGHT_PATH=/api/v1/operations/preflight

echo "== 0. 环境"
if [ "$(id -u)" -eq 0 ]; then pass "验收脚本以 root 运行"; else fail "验收脚本以 root 运行" "当前 UID $(id -u)"; fi
for b in "$AGENT" "$WEB" "$CTL"; do
  if [ -x "$b" ]; then pass "二进制存在且可执行 $(basename "$b")"; else fail "二进制存在且可执行 $(basename "$b")" "$b"; fi
done
if command -v python3 >/dev/null 2>&1; then pass "python3 可用于查库与解析帧"; else fail "python3 可用" "缺少 python3"; fi

if ! id -u "$WEB_USER" >/dev/null 2>&1; then
  note "创建系统账号 $WEB_USER（验收环境未预置）"
  useradd --system --no-create-home --shell /usr/sbin/nologin "$WEB_USER" 2>/dev/null
fi
if id -u "$WEB_USER" >/dev/null 2>&1; then pass "Web 账号 $WEB_USER 存在"; else fail "Web 账号 $WEB_USER 存在" "无法创建"; fi

if curl -fsS "$BASE/api/v1/health/live" >/dev/null 2>&1; then
  echo "$PORT 上已有服务在监听；先停掉遗留的 upanel-web 再跑验收" >&2
  exit 1
fi

rm -rf "$WORK"
mkdir -p "$WORK/logs" "$WEB_DB_DIR"
chmod 0755 "$WORK"
chown -R "$WEB_USER" "$WEB_DB_DIR" "$WORK/logs"

echo
echo "== 1. 启动代理与 Web =="
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
    "shutdown_timeout_seconds": 10
  },
  "agent": {
    "socket_path": "$SOCK",
    "shutdown_timeout_seconds": 10,
    "web_user": "$WEB_USER",
    "socket_group": "$WEB_USER",
    "database_path": "$AGENT_DB",
    "secret_key_path": "$KEY_FILE"
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

rm -f "$KEY_FILE"
if "$AGENT" --config "$SYS_CFG" -init-key >"$WORK/init-key.log" 2>&1; then
  pass "-init-key 生成代理密钥"
else
  fail "-init-key 生成代理密钥" "$(tail -3 "$WORK/init-key.log" 2>/dev/null)"
fi

if start_agent; then pass "代理以 root 启动并创建 socket"; else fail "代理以 root 启动" "$(tail -5 "$AGENT_LOG" 2>/dev/null)"; fi
expect_eq "代理进程 uid=0" "0" "$(ps -o uid= -p "$AGENT_PID" 2>/dev/null | tr -d ' ')"

INIT_OUT=$(printf '%s\n%s\n' "$PASSWORD" "$PASSWORD" | "$CTL" init-admin --config "$SYS_CFG" --db "$WEB_DB" \
  --username admin --display-name "管理员" --json 2>"$WORK/init.err")
expect_eq "init-admin 退出码" "0" "$?"
ADMIN_ID=$(printf '%s' "$INIT_OUT" | python3 -c 'import json,sys; print(json.load(sys.stdin)["user_id"])' 2>/dev/null)
if [ -n "$ADMIN_ID" ] && [ "$ADMIN_ID" -gt 0 ] 2>/dev/null; then pass "管理员 user_id=$ADMIN_ID"; else fail "管理员 user_id" "$INIT_OUT"; fi
chown "$WEB_USER" "$WEB_DB"
for suffix in "" "-wal" "-shm"; do
  [ -e "$WEB_DB$suffix" ] && chown "$WEB_USER" "$WEB_DB$suffix"
done

if start_web; then pass "Web 以 $WEB_USER 身份在 $BASE 启动"; else fail "Web 启动" "$(tail -5 "$WEB_LOG" 2>/dev/null)"; fi
WEB_CHILD_PID=$(pgrep -u "$WEB_USER" -f "upanel-web --mode production --db $WEB_DB" | head -n 1)
WEB_UID=$(ps -o uid= -p "${WEB_CHILD_PID:-0}" 2>/dev/null | tr -d ' ')
expect_eq "Web 进程以 $WEB_USER 运行" "$(id -u "$WEB_USER")" "$WEB_UID"
ROOT_WEB=$(ps -eo uid=,comm= | awk '$1 == 0 && $2 == "upanel-web"' | head -n 1)
expect_eq "没有以 root 运行的 Web 进程" "" "$ROOT_WEB"

code=$(login admin "$PASSWORD" "$WORK/admin.jar" login-admin)
expect_eq "管理员登录 200" "200" "$code"
if [ "$code" != "200" ]; then
  fail "登录失败，后续断言无法进行" "$(cat "$WORK/login-admin.json" 2>/dev/null)"
  printf '通过 %d 项，失败 %d 项\n' "$PASSED" "$FAILED"
  exit 1
fi
ADMIN_CSRF=$(csrf_of "$WORK/login-admin.json")
code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST /api/v1/auth/reauth "{\"password\":\"$PASSWORD\"}")
expect_eq "密码再认证 200" "200" "$code"

echo
echo "== 2. 提交一个 probe 任务，事件由代理产生 =="
code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "$PREFLIGHT_PATH" '{"action":"acceptance.probe","params":{"label":"alpha"}}')
expect_eq "probe 预检 200" "200" "$code"
PF_ID=$(jget "$WORK/resp.json" 'd["data"]["preflight_id"]')
PF_PHRASE=$(jget "$WORK/resp.json" 'd["data"]["confirmation_phrase"]')
code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST /api/v1/jobs \
  "{\"preflight_id\":\"$PF_ID\",\"confirmation_phrase\":\"$PF_PHRASE\"}" "p033-probe-1")
expect_eq "probe 提交 202" "202" "$code"
JOB=$(jget "$WORK/resp.json" 'd["data"]["job_id"]')
if [ -n "$JOB" ]; then pass "创建结果含 job_id=$JOB"; else fail "创建结果含 job_id" "$(cat "$WORK/resp.json")"; fi
EVENTS_URL=$(jget "$WORK/resp.json" 'd["data"]["events_url"]')
expect_eq "events_url 指向事件流" "/api/v1/jobs/$JOB/events" "$EVENTS_URL"

if wait_for_status "$JOB" succeeded 10; then pass "probe 任务到达 succeeded"; else fail "probe 任务到达 succeeded" "当前 $(jget "$WORK/resp.json" 'd["data"]["status"]')"; fi

echo
echo "== 3. 事件流：帧格式、镜像来源、seq 递增 =="
sse_collect stream-basic "/api/v1/jobs/$JOB/events" "" 3
expect_contains "响应是 text/event-stream" "text/event-stream" "$(cat "$WORK/stream-basic.hdr")"
expect_contains "事件流禁用缓存" "Cache-Control: no-store" "$(cat "$WORK/stream-basic.hdr")"
expect_contains "关闭反向代理缓冲" "X-Accel-Buffering: no" "$(cat "$WORK/stream-basic.hdr")"

BASIC=$WORK/stream-basic.sse
expect_contains "首帧是注释（客户端据此确认流已建立）" ": connected" "$(head -c 200 "$BASIC")"
expect_not_contains "首帧不是业务事件" "event: " "$(head -n 1 "$BASIC")"

IDS=$(sse_field "$BASIC" id)
if [ -n "$IDS" ]; then pass "帧里带持久递增的 id"; else fail "帧里带 id" "$(head -c 300 "$BASIC")"; fi
MONOTONIC=$(printf '%s\n' "$IDS" | python3 -c 'import sys
vals = [int(x) for x in sys.stdin.read().split()]
print("ok" if vals and all(b > a for a, b in zip(vals, vals[1:])) else "bad")')
expect_eq "id 严格递增（客户端据此续传）" "ok" "$MONOTONIC"

KINDS=$(sse_field "$BASIC" event | sort -u | tr '\n' ',')
expect_contains "事件类型里有 status" "status," "$KINDS"
expect_contains "事件里有代理产生的事件（镜像而非自造）" "agent_kind" "$(cat "$BASIC")"
expect_contains "代理事件保留 agent_seq 以便追溯" "agent_seq" "$(cat "$BASIC")"
DATA_LINE=$(grep -a '^data: ' "$BASIC" | head -n 1 | sed 's/^data: //')
expect_contains "data 是带 seq 的 JSON" '"seq":' "$DATA_LINE"
expect_contains "data 声明 kind" '"kind":' "$DATA_LINE"
LAST_ID=$(printf '%s\n' "$IDS" | tail -n 1)

echo
echo "== 4. 断线重连：不重发、不重跑 =="
OPS_BEFORE=$(dbq "$AGENT_DB" 'SELECT COUNT(*) FROM operations')
sse_collect stream-resume "/api/v1/jobs/$JOB/events" "$LAST_ID" 3
RESUME_EVENTS=$(sse_count "$WORK/stream-resume.sse")
expect_eq "带 Last-Event-ID 重连不重发已看过的事件" "0" "$RESUME_EVENTS"
OPS_AFTER=$(dbq "$AGENT_DB" 'SELECT COUNT(*) FROM operations')
expect_eq "重连不触发新的执行" "$OPS_BEFORE" "$OPS_AFTER"
RESUME_TAIL=$(tail -c 120 "$WORK/stream-resume.sse")
expect_not_contains "重连后的流里没有终态回放" '"succeeded"' "$RESUME_TAIL"

echo
echo "== 5. 输入校验与权限 =="
code=$(curl -s -o "$WORK/bad-last.json" -w '%{http_code}' -b "$WORK/admin.jar" \
  -H "Last-Event-ID: abc" -H "Origin: $ORIGIN" "$BASE/api/v1/jobs/$JOB/events")
expect_eq "非法 Last-Event-ID → 400" "400" "$code"
expect_eq "非法 Last-Event-ID 的错误码" "VALIDATION_ERROR" "$(jget "$WORK/bad-last.json" 'd["error"]["code"]')"

code=$(curl -s -o "$WORK/anon.json" -w '%{http_code}' -H "Origin: $ORIGIN" "$BASE/api/v1/jobs/$JOB/events")
expect_eq "未登录打开事件流 → 401" "401" "$code"

code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST /api/v1/users \
  "{\"username\":\"watcher\",\"display_name\":\"只读\",\"role\":\"viewer\",\"initial_password\":\"$REPLAY_PASSWORD\"}")
expect_eq "创建 viewer 账号 201" "201" "$code"
code=$(login watcher "$REPLAY_PASSWORD" "$WORK/watcher.jar" login-watcher)
expect_eq "viewer 登录 200" "200" "$code"
VIEWER_CSRF=$(csrf_of "$WORK/login-watcher.json")

code=$(curl -s -o "$WORK/viewer-events.json" -w '%{http_code}' -b "$WORK/watcher.jar" \
  -H "Origin: $ORIGIN" "$BASE/api/v1/jobs/$JOB/events")
expect_eq "viewer 打开事件流 → 403" "403" "$code"
expect_eq "viewer 事件流的错误码" "FORBIDDEN" "$(jget "$WORK/viewer-events.json" 'd["error"]["code"]')"

code=$(curl -s -o "$WORK/viewer-logs.json" -w '%{http_code}' -b "$WORK/watcher.jar" "$BASE/api/v1/jobs/$JOB/logs")
expect_eq "viewer 读日志 → 403" "403" "$code"

# viewer 的可见范围：接口对 viewer 开放（V），但任务按 user_id 归属，别人的任务返回 404。
# 这是 P03.2 就确定的归属策略（防 IDOR），P03.3 不改；契约里“viewer 不含原始参数/日志”
# 由 DTO 本身与 admin-only 的 events/logs 满足。见 docs/DECISIONS.md Q69。
code=$(api "$WORK/watcher.jar" "$VIEWER_CSRF" GET "/api/v1/jobs/$JOB")
expect_eq "viewer 读别人的任务 → 404（归属隔离）" "404" "$code"
code=$(api "$WORK/watcher.jar" "$VIEWER_CSRF" GET "/api/v1/jobs")
expect_eq "viewer 读任务列表 200（接口对 viewer 开放）" "200" "$code"
expect_eq "viewer 的列表里没有别人的任务" "0" "$(jget "$WORK/resp.json" 'len(d["data"]["jobs"])')"

echo
echo "== 6. 日志分页 =="
# 先读基数再写入：代理自己也会镜像 log 行，用增量断言才不会被“到底有几行”绑住。
LOGS_BEFORE=$(dbq "$WEB_DB" "SELECT COUNT(*) FROM job_events WHERE job_id='$JOB' AND kind='log'")
dbexec "$WEB_DB" "INSERT INTO job_events (job_id, seq, kind, redacted_payload, created_at)
  SELECT '$JOB', COALESCE(MAX(seq),0)+1, 'log', '{\"message\":\"第一行\"}', '2026-10-20T09:00:01Z' FROM job_events WHERE job_id='$JOB'"
dbexec "$WEB_DB" "INSERT INTO job_events (job_id, seq, kind, redacted_payload, created_at)
  SELECT '$JOB', COALESCE(MAX(seq),0)+1, 'log', '{\"message\":\"第二行\"}', '2026-10-20T09:00:02Z' FROM job_events WHERE job_id='$JOB'"
dbexec "$WEB_DB" "INSERT INTO job_events (job_id, seq, kind, redacted_payload, created_at)
  SELECT '$JOB', COALESCE(MAX(seq),0)+1, 'log', '{\"message\":\"第三行\"}', '2026-10-20T09:00:03Z' FROM job_events WHERE job_id='$JOB'"
dbexec "$WEB_DB" "INSERT INTO job_events (job_id, seq, kind, redacted_payload, created_at)
  SELECT '$JOB', COALESCE(MAX(seq),0)+1, 'log', '{\"message\":\"第四行\"}', '2026-10-20T09:00:04Z' FROM job_events WHERE job_id='$JOB'"
LOGS_AFTER=$(dbq "$WEB_DB" "SELECT COUNT(*) FROM job_events WHERE job_id='$JOB' AND kind='log'")
expect_eq "以 upanel-web 身份可写入日志行" "$((LOGS_BEFORE + 4))" "$LOGS_AFTER"

code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" GET "/api/v1/jobs/$JOB/logs?limit=2")
expect_eq "日志第一页 200" "200" "$code"
PAGE_ONE_N=$(jget "$WORK/resp.json" 'len(d["data"]["logs"])')
expect_eq "limit=2 返回 2 行" "2" "$PAGE_ONE_N"
CURSOR=$(jget "$WORK/resp.json" 'd["data"]["next_cursor"]')
if [ -n "$CURSOR" ]; then pass "还有下一页时返回 next_cursor"; else fail "返回 next_cursor" "$(cat "$WORK/resp.json")"; fi
FIRST_SEQ=$(jget "$WORK/resp.json" 'd["data"]["logs"][0]["seq"]')
PAGE_KIND=$(jget "$WORK/resp.json" 'd["data"]["logs"][0]["kind"]')
expect_eq "日志条目 kind=log" "log" "$PAGE_KIND"

code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" GET "/api/v1/jobs/$JOB/logs?limit=2&cursor=$CURSOR")
expect_eq "日志第二页 200" "200" "$code"
SECOND_SEQ=$(jget "$WORK/resp.json" 'd["data"]["logs"][0]["seq"]')
if [ -n "$SECOND_SEQ" ] && [ "$SECOND_SEQ" -gt "$FIRST_SEQ" ] 2>/dev/null; then
  pass "续页 seq 大于上一页（不重复、不跳过）"
else
  fail "续页 seq 大于上一页" "上一页 $FIRST_SEQ，本页 $SECOND_SEQ"
fi

code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" GET "/api/v1/jobs/$JOB/logs?cursor=bm90LWEtY3Vyc29y")
expect_eq "非法游标 → 400" "400" "$code"
code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" GET "/api/v1/jobs/$JOB/logs?limit=0")
expect_eq "limit=0 → 400" "400" "$code"
code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" GET "/api/v1/jobs/$JOB/logs?limit=201")
expect_eq "limit=201 → 400" "400" "$code"

echo
echo "== 7. 历史被清理后重连：先发 gap =="
PREFLIGHT_OF_JOB=$(dbq "$WEB_DB" "SELECT preflight_id FROM jobs WHERE id='$JOB'")
SYNTH=job_0000000000000000000000000000p33g
dbexec "$WEB_DB" "INSERT INTO jobs (id, operation_id, preflight_id, user_id, action, status, step, created_at, updated_at, needs_sync)
  VALUES ('$SYNTH', 'op_00000000000000000000000330gap1', '$PREFLIGHT_OF_JOB', $ADMIN_ID, 'acceptance.probe', 'running', 'ping', '2026-10-20T09:00:00Z', '2026-10-20T09:00:00Z', 1)"
dbexec "$WEB_DB" "INSERT INTO job_events (job_id, seq, kind, redacted_payload, created_at) VALUES ('$SYNTH', 5, 'status', '{\"status\":\"running\"}', '2026-10-20T09:00:00Z')"
dbexec "$WEB_DB" "INSERT INTO job_events (job_id, seq, kind, redacted_payload, created_at) VALUES ('$SYNTH', 6, 'status', '{\"status\":\"running\"}', '2026-10-20T09:00:01Z')"
sse_collect stream-gap "/api/v1/jobs/$SYNTH/events" 1 3
expect_contains "历史缺失时发 gap 帧" "event: gap" "$(cat "$WORK/stream-gap.sse")"
expect_contains "gap 说明历史已被清理" "history_pruned" "$(cat "$WORK/stream-gap.sse")"

echo
echo "== 8. 只追加：状态事件不可删、日志可按容量回收、终态不可回退 =="
if dbrefuse "$WEB_DB" "DELETE FROM job_events WHERE job_id='$JOB' AND kind='status'"; then
  pass "状态事件不能删除（触发器生效）"
else
  fail "状态事件不能删除" "DELETE 竟然成功了"
fi
if dbrefuse "$WEB_DB" "UPDATE job_events SET redacted_payload='{}' WHERE job_id='$JOB'"; then
  pass "事件改写被拒绝（只追加）"
else
  fail "事件改写被拒绝" "UPDATE 竟然成功了"
fi
if dbrefuse "$WEB_DB" "DELETE FROM job_events WHERE job_id='$JOB' AND kind='log' AND seq = (SELECT MAX(seq) FROM job_events WHERE job_id='$JOB' AND kind='log')"; then
  fail "日志行可按容量回收" "DELETE 被拒绝，迁移 0006 未生效"
else
  pass "日志行可按容量回收（迁移 0006）"
fi
if dbrefuse "$WEB_DB" "UPDATE jobs SET status='running' WHERE id='$JOB'"; then
  pass "终态不能回到 running（触发器生效）"
else
  fail "终态不能回到 running" "UPDATE 竟然成功了"
fi

echo
echo "== 9. 并发连接上限 =="
for i in 1 2 3 4; do
  curl -sN -b "$WORK/admin.jar" --max-time 12 "$BASE/api/v1/jobs/$JOB/events" >/dev/null 2>&1 &
  STREAM_PIDS="$STREAM_PIDS $!"
done
sleep 1
code=$(curl -s -o "$WORK/fifth.json" -w '%{http_code}' -b "$WORK/admin.jar" \
  -H "Origin: $ORIGIN" "$BASE/api/v1/jobs/$JOB/events")
expect_eq "第 5 条事件流 → 429" "429" "$code"
for pid in $STREAM_PIDS; do kill -TERM "$pid" 2>/dev/null; done
STREAM_PIDS=""
sleep 2
curl -sN -b "$WORK/admin.jar" -D "$WORK/reopen.hdr" -o /dev/null --max-time 1 \
  -H "Origin: $ORIGIN" "$BASE/api/v1/jobs/$JOB/events" >/dev/null 2>&1
expect_contains "关闭旧流后名额释放（可再次建立）" " 200" "$(head -n 1 "$WORK/reopen.hdr")"

echo
echo "== 10. 关闭事件流不影响任务执行 =="
code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "$PREFLIGHT_PATH" '{"action":"acceptance.ping","params":{}}')
expect_eq "ping 预检 200" "200" "$code"
PF_ID=$(jget "$WORK/resp.json" 'd["data"]["preflight_id"]')
code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST /api/v1/jobs \
  "{\"preflight_id\":\"$PF_ID\",\"confirmation_phrase\":\"\"}" "p033-ping-1")
expect_eq "ping 提交 202" "202" "$code"
PING_JOB=$(jget "$WORK/resp.json" 'd["data"]["job_id"]')
# 只订阅 1 秒就断开：等价于用户打开详情后立刻关掉标签页。
sse_collect stream-early "/api/v1/jobs/$PING_JOB/events" "" 1
if wait_for_status "$PING_JOB" succeeded 15; then
  pass "关闭事件流后任务仍执行到底（succeeded）"
else
  STATUS_NOW=$(jget "$WORK/resp.json" 'd["data"]["status"]')
  case $STATUS_NOW in
    canceled) pass "关闭事件流后任务仍执行到底（该动作按策略结束为 canceled）" ;;
    *) fail "关闭事件流后任务仍执行到底" "当前 $STATUS_NOW" ;;
  esac
fi

echo
echo "== 11. 取消与状态机 =="
code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "/api/v1/jobs/$PING_JOB/cancel")
case $code in
  202) pass "取消可取消动作 → 202" ;;
  409) pass "已进入终态的动作取消 → 409（不假装成功）" ;;
  *) fail "取消返回 202 或 409" "HTTP $code" ;;
esac
code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "/api/v1/jobs/$JOB/cancel")
expect_eq "不可取消的动作取消 → 409" "409" "$code"

echo
echo "== 12. 日志文件检查 =="
expect_not_contains "Web 日志没有把 CSRF 令牌写进去" "$ADMIN_CSRF" "$(cat "$WEB_LOG")"
expect_not_contains "Web 日志没有把密码写进去" "$PASSWORD" "$(cat "$WEB_LOG")"
expect_not_contains "代理日志没有把密码写进去" "$PASSWORD" "$(cat "$AGENT_LOG")"

echo
printf '通过 %d 项，失败 %d 项\n' "$PASSED" "$FAILED"
if [ "$FAILED" -eq 0 ]; then
  echo "P03.3 验收全部通过"
  exit 0
fi
echo "失败项:$FAILED_NAMES"
exit 1
