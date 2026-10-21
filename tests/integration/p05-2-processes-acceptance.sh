#!/bin/sh
# P05.2 Linux 验收脚本：进程列表、PID+starttime 身份、TERM/KILL 分开的流程、
# 禁止终止关键进程——在真实 /proc 与真实内核上逐条验证。
#
# 运行位置：Ubuntu 22.04/24.04 验收 VM 内，**必须以 root 运行**（代理只能在 root 下启动，
# 并用 upanel-web 身份去查 socket）。不依赖 Go 工具链：用宿主交叉编译好的 Linux 二进制。
#
# 用法：
#   sudo BIN_DIR=/tmp/upanel-bin sh tests/integration/p05-2-processes-acceptance.sh
#
# 退出码：0 全部通过；1 有失败项；2 环境不满足（非 root）。
#
# 覆盖的验收点（STAGES §7 P05.2、CONTRACTS §5/§10.2、PLAN §14）：
#   1. Web 非 root、代理 root、socket 归 socket_group（与 P04/P05.1 同一条边界）
#   2. GET /processes 的字段、闭集、null 语义、排序与契约一致；5 秒快照
#   3. 采集与 ps 对照：PID 1、内核线程、面板/代理自身、SSH 的进程都在，且保护类别正确
#   4. CPU 百分比来自两次采样的差值（不是常数、不是编的）
#   5. TERM：预检 → 任务 → 进程真的退出；不要求再认证与短语
#   6. KILL：预检要求再认证 + 确认短语；短语错 400、没再认证 403、补齐后才执行
#   7. 禁止终止：PID 1 / 内核线程 / 代理自身 / SSH / 僵尸，预检全部 allowed=false
#   8. 参数边界：不存在的 PID、starttime 不符、signal 非闭集、缺 starttime
#   9. PID 重用：旧预检不能杀新进程（新进程必须活着），并以 ns_last_pid 强制复用
#  10. viewer 读列表与预检都是 403；审计里有对应的 preflight/job 记录
#
# 明确不覆盖（NOT RUN，原因写在这里，不在脚本里假装通过）：
#   - 浏览器渲染：VM 上没有浏览器。页面级证据在宿主上用真实载荷回放
#     （web/src/components/realdata-processes.test.ts 读 web/src/fixtures/p05-2-vm/）。
#   - TERM 超时（进程忽略 SIGTERM）与 KILL 后仍存在（D 状态）：需要人造一个
#     忽略信号并停在不可中断等待里的进程，正常内核上做不到；这两条仅由
#     internal/processes/signal_test.go 的 fake 覆盖。
#   - 结束日志订阅不残留 journalctl 子进程：属于 P05.3（日志模块）。

set -u

BIN_DIR=${BIN_DIR:-/tmp/upanel-bin}
HERE=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)

if [ "$(id -u)" -ne 0 ]; then
  printf '本脚本必须以 root 运行：sudo BIN_DIR=%s sh %s\n' "$BIN_DIR" "$0" >&2
  exit 2
fi

AGENT="$BIN_DIR/upanel-agent"
WEB="$BIN_DIR/upanel-web"
CTL="$BIN_DIR/upanelctl"
VALIDATE="$HERE/p05-2-payload-validate.py"

WORK=${WORK:-/var/tmp/upanel-accept-p05-2}
PAYLOADS=$WORK/payloads
RUN_DIR=/run/upanel
SOCK=$RUN_DIR/agent.sock
SYS_CFG=/etc/upanel/upanel.json
AGENT_DB_DIR=/var/lib/upanel-agent
AGENT_DB=$AGENT_DB_DIR/agent.db
KEY_FILE=/etc/upanel/agent.key
WEB_DB_DIR=$WORK/webdata
WEB_DB=$WEB_DB_DIR/upanel.db
PORT=${PORT:-19450}
BASE="http://127.0.0.1:$PORT"
ORIGIN="$BASE"
WEB_USER=upanel-web
PASSWORD='vm-correct-horse-9'
VIEWER_PASSWORD='vm-correct-horse-8'

AGENT_LOG=$WORK/logs/agent.log
WEB_LOG=$WORK/logs/web.log
AGENT_PID=""
WEB_PID=""

PREFLIGHT_PATH=/api/v1/operations/preflight
JOBS_PATH=/api/v1/jobs
PROCESSES_PATH=/api/v1/processes

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

# jget <文件> <python 表达式，d 为解析后的 JSON>
jget() {
  python3 -c 'import json,sys
d=json.load(open(sys.argv[1], encoding="utf-8"))
print(eval(sys.argv[2]))' "$1" "$2" 2>/dev/null
}

# dbq <数据库文件> <SQL>：只读查询，返回第一行第一列。
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

# proc_starttime <pid>：/proc/<pid>/stat 第 22 字段（开机以来的时钟滴答数）。
proc_starttime() {
  python3 - "$1" <<'PY'
import sys
try:
    raw = open("/proc/%s/stat" % sys.argv[1], encoding="utf-8", errors="replace").read()
except OSError:
    print("")
    raise SystemExit
rest = raw[raw.rindex(")") + 2:].split()
print(rest[19] if len(rest) > 19 else "")
PY
}

# proc_state <pid>：状态字母（R/S/D/Z/T/t/X...）。
proc_state() {
  python3 - "$1" <<'PY'
import sys
try:
    raw = open("/proc/%s/stat" % sys.argv[1], encoding="utf-8", errors="replace").read()
except OSError:
    print("")
    raise SystemExit
rest = raw[raw.rindex(")") + 2:].split()
print(rest[0] if rest else "")
PY
}

alive() { [ -d "/proc/$1" ]; }

# spawn_sleep <秒>：启动一个孤儿 sleep（父进程退出后由 PID 1 接管），回显它的 PID。
# 孤儿而不是当前 shell 的子进程，是为了让“信号发出后进程真的消失”可被观察：
# 子进程被杀后会先变僵尸，直到 shell 回收，那会混淆“已退出”的判定。
spawn_sleep() {
  sh -c "sleep $1 >/dev/null 2>&1 & echo \$!"
}

# spawn_busy：启动一个占满单核的孤儿进程（yes 写 /dev/null），回显 PID。
spawn_busy() {
  sh -c 'yes >/dev/null 2>&1 & echo $!'
}

cleanup() {
  stop_web
  stop_agent
  # 不留任何测试进程。
  for pid in ${TEST_PIDS:-}; do
    kill -KILL "$pid" 2>/dev/null
  done
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
  if [ -n "$AGENT_PID" ] && kill -0 "$AGENT_PID" 2>/dev/null; then
    kill -TERM "$AGENT_PID" 2>/dev/null
    wait "$AGENT_PID" 2>/dev/null
  fi
  AGENT_PID=""
  rm -f "$SOCK"
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

stop_web() {
  if [ -n "$WEB_PID" ] && kill -0 "$WEB_PID" 2>/dev/null; then
    kill -TERM "$WEB_PID" 2>/dev/null
    wait "$WEB_PID" 2>/dev/null
  fi
  WEB_PID=""
  for _ in $(seq 1 100); do
    curl -fsS "$BASE/api/v1/health/live" >/dev/null 2>&1 || return 0
    sleep 0.1
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

# api <jar> <csrf> <方法> <路径> [body] [idempotency-key]：响应写到 $WORK/resp.json。
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

# get_processes <载荷名>：GET /processes 并保存载荷，回显 HTTP 状态码。
get_processes() {
  _code=$(api "$JAR" "$CSRF" GET "$PROCESSES_PATH")
  cp "$WORK/resp.json" "$PAYLOADS/$1" 2>/dev/null
  printf '%s' "$_code"
}

# wait_in_snapshot <pid> <载荷名> [cpu]: 反复 GET，直到快照里出现这个 pid
# （可选：直到它的 cpu_percent 非空）再保存载荷。
#
# 为什么必须这样做：列表是 5 秒缓存快照（CONTRACTS 10.2）。刚 fork 出来的进程
# 不会立刻出现在里面，拿 spawn 之前的快照去断言“它不在列表里”，断言的是采集器
# 还没轮到的那段时间——那是脚本的错，不是产品的错。CPU 百分比更要两份样本：
# 进程第一次出现时没有上一样本，值就是 null。
wait_in_snapshot() {
  _pid=$1; _name=$2; _want_cpu=${3:-}
  _i=0
  while [ "$_i" -lt 60 ]; do
    _code=$(api "$JAR" "$CSRF" GET "$PROCESSES_PATH")
    if python3 - "$WORK/resp.json" "$_pid" "$_want_cpu" <<'PY'
import json, sys
d = json.load(open(sys.argv[1], encoding="utf-8"))
ps = d.get("data", {}).get("processes") or []
hit = [p for p in ps if p.get("pid") == int(sys.argv[2])]
if not hit:
    raise SystemExit(1)
if sys.argv[3] == "cpu" and hit[0].get("cpu_percent") is None:
    raise SystemExit(1)
raise SystemExit(0)
PY
    then
      cp "$WORK/resp.json" "$PAYLOADS/$_name"
      printf '%s' "$_code"
      return 0
    fi
    sleep 0.5
    _i=$((_i + 1))
  done
  cp "$WORK/resp.json" "$PAYLOADS/$_name" 2>/dev/null
  printf '%s' "${_code:-000}"
  return 1
}

# preflight <载荷名> <pid> <starttime> <signal>：把响应存成 preflight-<名>.json。
preflight() {
  _body=$(printf '{"action":"process.signal","params":{"pid":%s,"start_time_ticks":%s,"signal":"%s"}}' \
    "$2" "$3" "$4")
  _code=$(api "$JAR" "$CSRF" POST "$PREFLIGHT_PATH" "$_body")
  cp "$WORK/resp.json" "$PAYLOADS/preflight-$1.json" 2>/dev/null
  printf '%s' "$_code"
}

# submit <载荷名> <preflight_id> [短语]：把响应存成 job-submit-<名>.json。
submit() {
  if [ -n "${3:-}" ]; then
    _body=$(printf '{"preflight_id":"%s","confirmation_phrase":"%s"}' "$2" "$3")
  else
    _body=$(printf '{"preflight_id":"%s"}' "$2")
  fi
  _code=$(api "$JAR" "$CSRF" POST "$JOBS_PATH" "$_body" "p05-2-$1-$$")
  cp "$WORK/resp.json" "$PAYLOADS/job-submit-$1.json" 2>/dev/null
  printf '%s' "$_code"
}

# wait_terminal_job <job_id> <超时秒>：轮询到终态，回显 status。
wait_terminal_job() {
  _jid=$1; _limit=$(( ${2:-40} * 2 )); _i=0; _st=unknown
  while [ "$_i" -lt "$_limit" ]; do
    api "$JAR" "$CSRF" GET "$JOBS_PATH/$_jid" >/dev/null
    _st=$(jget "$WORK/resp.json" 'd["data"]["status"]')
    case $_st in
      succeeded|failed|canceled|interrupted) printf '%s' "$_st"; return 0 ;;
    esac
    sleep 0.5
    _i=$((_i + 1))
  done
  printf '%s' "${_st:-unknown}"
  return 1
}

# poll_job <载荷名> <job_id>：拿到终态任务并保存载荷，回显 status。
poll_job() {
  _st=$(wait_terminal_job "$2" 40)
  api "$JAR" "$CSRF" GET "$JOBS_PATH/$2" >/dev/null
  cp "$WORK/resp.json" "$PAYLOADS/job-$1.json" 2>/dev/null
  printf '%s' "$_st"
}

echo "== 0. 环境 =="
if [ "$(id -u)" -eq 0 ]; then pass "验收脚本以 root 运行"; else fail "验收脚本以 root 运行" "UID $(id -u)"; fi
for b in "$AGENT" "$WEB" "$CTL"; do
  if [ -x "$b" ]; then pass "二进制存在且可执行 $(basename "$b")"; else fail "二进制存在且可执行 $(basename "$b")" "$b"; fi
done
if [ -f "$VALIDATE" ]; then pass "载荷校验程序存在"; else fail "载荷校验程序存在" "$VALIDATE"; fi
if command -v python3 >/dev/null 2>&1; then pass "python3 可用"; else fail "python3 可用" "缺少 python3"; fi
if [ -r /proc/1/stat ] && [ -r /proc/self/stat ]; then pass "/proc 可读"; else fail "/proc 可读" "无法读取 /proc"; fi

if ! id -u "$WEB_USER" >/dev/null 2>&1; then
  note "创建系统账号 $WEB_USER"
  useradd --system --no-create-home --shell /usr/sbin/nologin "$WEB_USER" 2>/dev/null
fi
if id -u "$WEB_USER" >/dev/null 2>&1; then pass "Web 账号 $WEB_USER 存在"; else fail "Web 账号 $WEB_USER 存在" "无法创建"; fi

if curl -fsS "$BASE/api/v1/health/live" >/dev/null 2>&1; then
  echo "$PORT 上已有服务在监听；先停掉遗留的 upanel-web 再跑验收" >&2
  exit 1
fi

rm -rf "$WORK"
mkdir -p "$WORK/logs" "$PAYLOADS" "$WEB_DB_DIR"
chmod 0755 "$WORK" "$PAYLOADS" "$WORK/logs"
chown -R "$WEB_USER" "$WEB_DB_DIR"
TEST_PIDS=""

echo
echo "== 1. 配置与启动（代理 root、Web 非 root） =="
install -d -m 0755 /etc/upanel
install -d -m 0700 "$AGENT_DB_DIR"
rm -f "$AGENT_DB" "$AGENT_DB-wal" "$AGENT_DB-shm"
install -d -m 0755 "$RUN_DIR"
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
    "services": {}
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
if grep -q '已启用进程状态采集' "$AGENT_LOG" 2>/dev/null; then
  pass "代理已启用进程采集"
else
  notrun "代理已启用进程采集" "日志里没有找到采集启用行（按接口结果继续判断）"
fi
if grep -q '已启用验收专用动作表' "$AGENT_LOG" 2>/dev/null; then
  fail "代理没有启用验收专用动作表" "日志里出现验收动作表，真实动作会被跳过"
else
  pass "代理使用生产动作登记表（process.signal 可用）"
fi

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
expect_eq "agent.sock 归 root:$WEB_USER" "root:$WEB_USER" "$(stat -c '%U:%G' "$SOCK" 2>/dev/null)"
expect_eq "agent.sock 不是 0666" "no" \
  "$(stat -c '%a' "$SOCK" 2>/dev/null | grep -qx 666 && echo yes || echo no)"

JAR=$WORK/admin.jar
expect_eq "管理员登录" "200" "$(login admin "$PASSWORD" "$JAR" login-admin)"
CSRF=$(csrf_of "$WORK/login-admin.json")
if [ -n "$CSRF" ]; then pass "拿到 CSRF 令牌"; else fail "拿到 CSRF 令牌" "登录响应里没有 csrf_token"; fi

api "$JAR" "$CSRF" POST /api/v1/users \
  "{\"username\":\"viewer\",\"display_name\":\"只读\",\"role\":\"viewer\",\"initial_password\":\"$VIEWER_PASSWORD\"}" \
  >"$WORK/viewer-code.txt"
expect_eq "创建 viewer 账号" "201" "$(cat "$WORK/viewer-code.txt")"
chown_web_db
VJAR=$WORK/viewer.jar
expect_eq "viewer 登录" "200" "$(login viewer "$VIEWER_PASSWORD" "$VJAR" login-viewer)"

echo
echo "== 2. GET /processes：契约、快照、闭集 =="
SLEEP_PID=$(spawn_sleep 600)
TEST_PIDS="$TEST_PIDS $SLEEP_PID"
SLEEP_START=$(proc_starttime "$SLEEP_PID")
if alive "$SLEEP_PID" && [ -n "$SLEEP_START" ]; then
  pass "测试目标进程已启动（pid=$SLEEP_PID starttime=$SLEEP_START）"
else
  fail "测试目标进程已启动" "pid=$SLEEP_PID starttime='$SLEEP_START'"
fi

expect_eq "GET /processes 状态码" "200" "$(wait_in_snapshot "$SLEEP_PID" processes-first.json)"
expect_eq "采集状态是 ok" "ok" "$(jget "$PAYLOADS/processes-first.json" 'd["data"]["status"]')"
expect_eq "采集间隔是 5000ms" "5000" "$(jget "$PAYLOADS/processes-first.json" 'd["data"]["interval_ms"]')"
expect_eq "未被截断" "False" "$(jget "$PAYLOADS/processes-first.json" 'd["data"]["truncated"]')"
expect_eq "total 与 processes 长度一致" \
  "$(jget "$PAYLOADS/processes-first.json" 'd["data"]["total"]')" \
  "$(jget "$PAYLOADS/processes-first.json" 'len(d["data"]["processes"])')"
expect_eq "PID 升序" "True" \
  "$(jget "$PAYLOADS/processes-first.json" 'all(a["pid"] < b["pid"] for a, b in zip(d["data"]["processes"], d["data"]["processes"][1:]))')"
expect_eq "PID 不重复" "True" \
  "$(jget "$PAYLOADS/processes-first.json" 'len({p["pid"] for p in d["data"]["processes"]}) == len(d["data"]["processes"])')"

# 现场对照：ps 的 PID 集合。快照是 5 秒的，进程会来回，因此只要求
# “快照里的 PID 在 ps 里也出现过，或当时确实已经退出”，并把不一致数打出来。
ps -e -o pid=,ppid=,stat=,args= --no-headers >"$PAYLOADS/ps-first.txt" 2>/dev/null
PS_COUNT=$(wc -l <"$PAYLOADS/ps-first.txt" | tr -d ' ')
SNAP_COUNT=$(jget "$PAYLOADS/processes-first.json" 'd["data"]["total"]')
note "快照 $SNAP_COUNT 个进程，ps 同时看到 $PS_COUNT 行（含正在退出的）"
if [ "$SNAP_COUNT" -ge 20 ]; then
  pass "快照规模与真机相当（≥20 个进程）"
else
  fail "快照规模与真机相当" "只有 $SNAP_COUNT 个"
fi
MISSING=$(python3 - "$PAYLOADS/processes-first.json" "$PAYLOADS/ps-first.txt" <<'PY'
import json, sys
snap = json.load(open(sys.argv[1], encoding="utf-8"))["data"]["processes"]
ps = set()
for line in open(sys.argv[2], encoding="utf-8"):
    parts = line.split(None, 3)
    if parts:
        ps.add(int(parts[0]))
gone = [p["pid"] for p in snap if p["pid"] not in ps]
print(len(gone))
PY
)
if [ "$MISSING" -le 3 ]; then
  pass "快照与 ps 的 PID 集合基本一致（差异 $MISSING 个，允许采集中途退出的进程）"
else
  fail "快照与 ps 的 PID 集合基本一致" "有 $MISSING 个 PID 在 ps 里找不到"
fi

expect_eq "我们的 sleep 在列表里" "True" \
  "$(python3 - "$PAYLOADS/processes-first.json" "$SLEEP_PID" <<'PY'
import json, sys
snap = json.load(open(sys.argv[1], encoding="utf-8"))["data"]["processes"]
print(any(p["pid"] == int(sys.argv[2]) for p in snap))
PY
)"
expect_eq "sleep 的 starttime 与 /proc 一致" "$SLEEP_START" \
  "$(python3 - "$PAYLOADS/processes-first.json" "$SLEEP_PID" <<'PY'
import json, sys
snap = json.load(open(sys.argv[1], encoding="utf-8"))["data"]["processes"]
hit = [p for p in snap if p["pid"] == int(sys.argv[2])]
print(hit[0]["start_time_ticks"] if hit else "")
PY
)"
expect_eq "sleep 未被保护且有 TERM/KILL 能力" "False|['TERM', 'KILL']" \
  "$(python3 - "$PAYLOADS/processes-first.json" "$SLEEP_PID" <<'PY'
import json, sys
snap = json.load(open(sys.argv[1], encoding="utf-8"))["data"]["processes"]
hit = [p for p in snap if p["pid"] == int(sys.argv[2])]
print("%s|%s" % (hit[0]["protection"]["protected"], hit[0]["capabilities"]) if hit else "missing")
PY
)"
expect_eq "sleep 的命令摘要是 'sleep 600'" "sleep 600" \
  "$(python3 - "$PAYLOADS/processes-first.json" "$SLEEP_PID" <<'PY'
import json, sys
snap = json.load(open(sys.argv[1], encoding="utf-8"))["data"]["processes"]
hit = [p for p in snap if p["pid"] == int(sys.argv[2])]
print(hit[0]["command"] if hit else "")
PY
)"

echo
echo "== 3. 采集正确性：PID 1 / 内核线程 / 自身 / SSH / 僵尸 =="
expect_eq "PID 1 在列表里且受保护（init）" "True|init|[]" \
  "$(python3 - "$PAYLOADS/processes-first.json" <<'PY'
import json, sys
snap = json.load(open(sys.argv[1], encoding="utf-8"))["data"]["processes"]
hit = [p for p in snap if p["pid"] == 1]
print("%s|%s|%s" % (hit[0]["protection"]["protected"], hit[0]["protection"].get("category"),
                    hit[0]["capabilities"]) if hit else "missing")
PY
)"
expect_eq "PID 1 的 starttime 与 /proc 一致" "$(proc_starttime 1)" \
  "$(python3 - "$PAYLOADS/processes-first.json" <<'PY'
import json, sys
snap = json.load(open(sys.argv[1], encoding="utf-8"))["data"]["processes"]
hit = [p for p in snap if p["pid"] == 1]
print(hit[0]["start_time_ticks"] if hit else "")
PY
)"

KTHREAD=$(python3 - "$PAYLOADS/processes-first.json" <<'PY'
import json, sys
snap = json.load(open(sys.argv[1], encoding="utf-8"))["data"]["processes"]
for p in snap:
    if p["protection"].get("category") == "kernel_thread":
        print("%d %d" % (p["pid"], p["start_time_ticks"]))
        break
PY
)
if [ -n "$KTHREAD" ]; then
  pass "列表里存在内核线程并标注 kernel_thread（${KTHREAD%% *}）"
else
  fail "列表里存在内核线程并标注 kernel_thread" "一个都没有"
fi

expect_eq "代理自身受保护（self）" "True|self|[]" \
  "$(python3 - "$PAYLOADS/processes-first.json" "$AGENT_PID" <<'PY'
import json, sys
snap = json.load(open(sys.argv[1], encoding="utf-8"))["data"]["processes"]
hit = [p for p in snap if p["pid"] == int(sys.argv[2])]
print("%s|%s|%s" % (hit[0]["protection"]["protected"], hit[0]["protection"].get("category"),
                    hit[0]["capabilities"]) if hit else "missing")
PY
)"
expect_eq "面板本身受保护（self）" "True|self|[]" \
  "$(python3 - "$PAYLOADS/processes-first.json" "$LISTEN_PID" <<'PY'
import json, sys
snap = json.load(open(sys.argv[1], encoding="utf-8"))["data"]["processes"]
hit = [p for p in snap if p["pid"] == int(sys.argv[2])]
print("%s|%s|%s" % (hit[0]["protection"]["protected"], hit[0]["protection"].get("category"),
                    hit[0]["capabilities"]) if hit else "missing")
PY
)"

SSHD_PID=$(pgrep -x sshd 2>/dev/null | head -1)
if [ -n "$SSHD_PID" ]; then
  expect_eq "sshd 受保护（ssh）" "True|ssh|[]" \
    "$(python3 - "$PAYLOADS/processes-first.json" "$SSHD_PID" <<'PY'
import json, sys
snap = json.load(open(sys.argv[1], encoding="utf-8"))["data"]["processes"]
hit = [p for p in snap if p["pid"] == int(sys.argv[2])]
print("%s|%s|%s" % (hit[0]["protection"]["protected"], hit[0]["protection"].get("category"),
                    hit[0]["capabilities"]) if hit else "missing")
PY
)"
else
  notrun "sshd 受保护（ssh）" "这台机器上没有 sshd 进程（socket 激活未建立连接？）"
fi

# 僵尸进程：fork 出来立刻退出的子进程，父进程（python）先睡 20 秒不回收。
# 20 秒而不是 1—2 秒：快照每 5 秒采集一次，僵尸要先被采进快照、再等预检跑完，
# 活得比一次采集还短的僵尸测不到位。
python3 - "$WORK/zombie.pid" <<'PY' &
import os, sys, time
pid = os.fork()
if pid == 0:
    os._exit(0)
open(sys.argv[1], "w").write(str(pid))
time.sleep(20)
PY
ZOMBIE_WAITER=$!
TEST_PIDS="$TEST_PIDS $ZOMBIE_WAITER"
for _ in $(seq 1 50); do
  [ -s "$WORK/zombie.pid" ] && break
  sleep 0.1
done
ZOMBIE_PID=$(cat "$WORK/zombie.pid" 2>/dev/null)
if [ -n "$ZOMBIE_PID" ] && [ "$(proc_state "$ZOMBIE_PID")" = "Z" ]; then
  pass "已构造出一个僵尸进程（pid=$ZOMBIE_PID）"
  wait_in_snapshot "$ZOMBIE_PID" processes-zombie.json >/dev/null
  expect_eq "僵尸进程没有能力、内存为 null" "True|[]|None" \
    "$(python3 - "$PAYLOADS/processes-zombie.json" "$ZOMBIE_PID" <<'PY'
import json, sys
snap = json.load(open(sys.argv[1], encoding="utf-8"))["data"]["processes"]
hit = [p for p in snap if p["pid"] == int(sys.argv[2])]
if not hit:
    print("missing")
else:
    p = hit[0]
    print("%s|%s|%r" % (p["state"] == "zombie", p["capabilities"], p["memory_bytes"]))
PY
)"
expect_eq "僵尸进程的预检是 PROCESS_ALREADY_EXITED" "200|False|PROCESS_ALREADY_EXITED" \
  "$(c=$(preflight zombie "$ZOMBIE_PID" "$(proc_starttime "$ZOMBIE_PID")" TERM); \
     printf '%s|%s|%s' "$c" \
       "$(jget "$PAYLOADS/preflight-zombie.json" 'd["data"]["allowed"]')" \
       "$(jget "$PAYLOADS/preflight-zombie.json" '",".join(b["code"] for b in d["data"]["blockers"])')")"
else
  notrun "僵尸进程的采集与拒绝" "没有构造出僵尸进程（pid=${ZOMBIE_PID:-空}）"
fi

echo
echo "== 4. CPU 百分比来自两次采样 =="
BUSY_PID=$(spawn_busy)
TEST_PIDS="$TEST_PIDS $BUSY_PID"
# 等到快照里既有这个进程、又给了它一个非空的 CPU 百分比（第一份样本没有参照）。
wait_in_snapshot "$BUSY_PID" processes-cpu.json cpu >/dev/null
BUSY_CPU=$(python3 - "$PAYLOADS/processes-cpu.json" "$BUSY_PID" <<'PY'
import json, sys
snap = json.load(open(sys.argv[1], encoding="utf-8"))["data"]["processes"]
hit = [p for p in snap if p["pid"] == int(sys.argv[2])]
print("missing" if not hit else str(hit[0]["cpu_percent"]))
PY
)
if [ "$BUSY_CPU" = "missing" ]; then
  fail "忙进程出现在快照里" "pid=$BUSY_PID 不在列表里"
elif python3 -c 'import sys; sys.exit(0 if float(sys.argv[1]) > 50 else 1)' "$BUSY_CPU" 2>/dev/null; then
  pass "忙进程的 CPU 百分比反映真实占用（$BUSY_CPU%，单核基准）"
else
  fail "忙进程的 CPU 百分比反映真实占用" "得到 $BUSY_CPU"
fi
expect_eq "空闲 sleep 的 CPU 接近 0" "True" \
  "$(python3 - "$PAYLOADS/processes-cpu.json" "$SLEEP_PID" <<'PY'
import json, sys
snap = json.load(open(sys.argv[1], encoding="utf-8"))["data"]["processes"]
hit = [p for p in snap if p["pid"] == int(sys.argv[2])]
print(str(bool(hit) and (hit[0]["cpu_percent"] is None or hit[0]["cpu_percent"] < 5)))
PY
)"
kill -KILL "$BUSY_PID" 2>/dev/null

echo
echo "== 5. TERM：预检 → 任务 → 进程真的退出 =="
expect_eq "TERM 预检状态码" "200" "$(preflight term-ok "$SLEEP_PID" "$SLEEP_START" TERM)"
expect_eq "TERM 预检允许执行" "True" "$(jget "$PAYLOADS/preflight-term-ok.json" 'd["data"]["allowed"]')"
expect_eq "TERM 不需要再认证" "False" "$(jget "$PAYLOADS/preflight-term-ok.json" 'd["data"]["requires_reauth"]')"
expect_eq "TERM 不需要确认短语" "None" "$(jget "$PAYLOADS/preflight-term-ok.json" 'd["data"]["confirmation_phrase"]')"
expect_eq "TERM 的资源锁是 process:<pid>:<starttime>" "process:$SLEEP_PID:$SLEEP_START" \
  "$(jget "$PAYLOADS/preflight-term-ok.json" 'd["data"]["locks"][0]')"
expect_eq "TERM 的告警是 process_terminate" "process_terminate" \
  "$(jget "$PAYLOADS/preflight-term-ok.json" 'd["data"]["warnings"][0]["code"]')"
expect_eq "TERM 的规范化参数带 start_time_ticks" "$SLEEP_PID|$SLEEP_START|TERM" \
  "$(jget "$PAYLOADS/preflight-term-ok.json" '"%s|%s|%s" % (d["data"]["normalized_params"]["pid"], d["data"]["normalized_params"]["start_time_ticks"], d["data"]["normalized_params"]["signal"])')"
expect_eq "TERM 的目标显示名带 PID 与命令" "True" \
  "$(jget "$PAYLOADS/preflight-term-ok.json" 'str('"$SLEEP_PID"') in d["data"]["target"]["display_name"] and "sleep" in d["data"]["target"]["display_name"]')"

TERM_PF=$(jget "$PAYLOADS/preflight-term-ok.json" 'd["data"]["preflight_id"]')
expect_eq "TERM 任务提交被接受" "202" "$(submit term-ok "$TERM_PF" "")"
TERM_JOB=$(jget "$PAYLOADS/job-submit-term-ok.json" 'd["data"]["job_id"]')
expect_eq "TERM 任务终态是 succeeded" "succeeded" "$(poll_job term-ok "$TERM_JOB")"
sleep 0.5
expect_eq "信号已经送达" "True" "$(jget "$PAYLOADS/job-term-ok.json" 'd["data"]["result"]["data"]["delivered"]')"
expect_eq "读回确认进程已退出" "True" "$(jget "$PAYLOADS/job-term-ok.json" 'd["data"]["result"]["observed_state"]["gone"]')"
expect_eq "任务结果里的 starttime 与预检一致" "$SLEEP_START" \
  "$(jget "$PAYLOADS/job-term-ok.json" 'd["data"]["result"]["data"]["start_time_ticks"]')"
if alive "$SLEEP_PID"; then
  fail "TERM 之后进程真的消失" "pid=$SLEEP_PID 仍然存在（状态 $(proc_state "$SLEEP_PID")）"
else
  pass "TERM 之后进程真的消失（跨过面板自己说的那一层）"
fi
expect_eq "TERM 任务不可取消" "False" "$(jget "$PAYLOADS/job-term-ok.json" 'd["data"]["cancellable"]')"

echo
echo "== 6. KILL：再认证 + 确认短语 =="
KILL_PID=$(spawn_sleep 600)
TEST_PIDS="$TEST_PIDS $KILL_PID"
KILL_START=$(proc_starttime "$KILL_PID")
expect_eq "KILL 预检状态码" "200" "$(preflight kill-ok "$KILL_PID" "$KILL_START" KILL)"
expect_eq "KILL 预检允许执行" "True" "$(jget "$PAYLOADS/preflight-kill-ok.json" 'd["data"]["allowed"]')"
expect_eq "KILL 要求再认证" "True" "$(jget "$PAYLOADS/preflight-kill-ok.json" 'd["data"]["requires_reauth"]')"
expect_eq "KILL 的确认短语以 KILL 开头" "True" \
  "$(jget "$PAYLOADS/preflight-kill-ok.json" 'd["data"]["confirmation_phrase"].startswith("KILL ")')"
expect_eq "KILL 的告警是 process_force_kill" "process_force_kill" \
  "$(jget "$PAYLOADS/preflight-kill-ok.json" 'd["data"]["warnings"][0]["code"]')"
KILL_PHRASE=$(jget "$PAYLOADS/preflight-kill-ok.json" 'd["data"]["confirmation_phrase"]')
KILL_PF=$(jget "$PAYLOADS/preflight-kill-ok.json" 'd["data"]["preflight_id"]')

# 6a. 短语对、但没有再认证：403 REAUTH_REQUIRED，且预检**没有被消费**（下一次还能用）。
expect_eq "没有再认证被拒绝（403）" "403" "$(submit kill-no-reauth "$KILL_PF" "$KILL_PHRASE")"
expect_eq "没有再认证的错误码" "REAUTH_REQUIRED" \
  "$(jget "$PAYLOADS/job-submit-kill-no-reauth.json" 'd["error"]["code"]')"
if alive "$KILL_PID"; then
  pass "没有再认证时进程没有被碰过（pid=$KILL_PID 仍在）"
else
  fail "没有再认证时进程没有被碰过" "pid=$KILL_PID 已经不在了"
fi
expect_eq "预检仍然可用（没有被这次拒绝消费）" "0" \
  "$(dbq "$WEB_DB" "SELECT count(*) FROM preflights WHERE id = '$KILL_PF' AND consumed_job_id IS NOT NULL")"

# 6b. 再认证之后短语仍然要对：错的短语 400，进程一根手指都没被碰。
expect_eq "再认证" "200" \
  "$(api "$JAR" "$CSRF" POST /api/v1/auth/reauth "{\"password\":\"$PASSWORD\"}")"
expect_eq "短语不符被拒绝（400）" "400" "$(submit kill-bad-phrase "$KILL_PF" "KILL WRONG1 AA")"
expect_eq "短语不符的错误码" "CONFIRMATION_MISMATCH" \
  "$(jget "$PAYLOADS/job-submit-kill-bad-phrase.json" 'd["error"]["code"]')"
if alive "$KILL_PID"; then
  pass "短语不符时进程没有被碰过（pid=$KILL_PID 仍在）"
else
  fail "短语不符时进程没有被碰过" "pid=$KILL_PID 已经不在了"
fi

# 6c. 被代理拒绝的那次提交已经用掉了预检（一次性），因此重新预检再提交。
expect_eq "KILL 重新预检" "200" "$(preflight kill-ok-2 "$KILL_PID" "$KILL_START" KILL)"
KILL_PHRASE2=$(jget "$PAYLOADS/preflight-kill-ok-2.json" 'd["data"]["confirmation_phrase"]')
KILL_PF2=$(jget "$PAYLOADS/preflight-kill-ok-2.json" 'd["data"]["preflight_id"]')
expect_eq "再认证之后提交被接受" "202" "$(submit kill-ok "$KILL_PF2" "$KILL_PHRASE2")"
KILL_JOB=$(jget "$PAYLOADS/job-submit-kill-ok.json" 'd["data"]["job_id"]')
expect_eq "KILL 任务终态是 succeeded" "succeeded" "$(poll_job kill-ok "$KILL_JOB")"
expect_eq "KILL 的任务不可取消" "False" "$(jget "$PAYLOADS/job-kill-ok.json" 'd["data"]["cancellable"]')"
if alive "$KILL_PID"; then
  fail "KILL 之后进程消失" "pid=$KILL_PID 仍然存在（状态 $(proc_state "$KILL_PID")）"
else
  pass "KILL 之后进程消失"
fi

# 6d. 同一份预检不能再用第二次。
expect_eq "同一份预检第二次提交被拒（409）" "409" "$(submit kill-replay "$KILL_PF2" "$KILL_PHRASE2")"
expect_eq "重放的拒绝码是 PREFLIGHT_STALE" "PREFLIGHT_STALE" \
  "$(jget "$PAYLOADS/job-submit-kill-replay.json" 'd["error"]["code"]')"

echo
echo "== 7. 禁止终止关键进程 =="
INIT_START=$(proc_starttime 1)
expect_eq "PID 1 的 TERM 预检被拒" "200|False|PROCESS_PROTECTED" \
  "$(c=$(preflight protected-init 1 "$INIT_START" TERM); \
     printf '%s|%s|%s' "$c" \
       "$(jget "$PAYLOADS/preflight-protected-init.json" 'd["data"]["allowed"]')" \
       "$(jget "$PAYLOADS/preflight-protected-init.json" '",".join(b["code"] for b in d["data"]["blockers"])')")"
INIT_PF=$(jget "$PAYLOADS/preflight-protected-init.json" 'd["data"]["preflight_id"]')
expect_eq "被拒的预检不能提交（409）" "409" "$(submit protected-init "$INIT_PF" "")"
expect_eq "PID 1 仍然活着" "True" "$(alive 1 && echo True || echo False)"

AGENT_START=$(proc_starttime "$AGENT_PID")
expect_eq "代理自身的 KILL 预检被拒" "200|False|PROCESS_PROTECTED" \
  "$(c=$(preflight protected-self "$AGENT_PID" "$AGENT_START" KILL); \
     printf '%s|%s|%s' "$c" \
       "$(jget "$PAYLOADS/preflight-protected-self.json" 'd["data"]["allowed"]')" \
       "$(jget "$PAYLOADS/preflight-protected-self.json" '",".join(b["code"] for b in d["data"]["blockers"])')")"
# 门槛按信号算，与 allowed 无关：被拒的 KILL 仍然带着再认证要求与短语。
# 拦下它的是 Allowed=false（409 DEPENDENCY_BLOCKED），不是把门槛藏起来——界面据此
# 才知道“这不是一道能用密码跨过去的门”。
SELF_PF=$(jget "$PAYLOADS/preflight-protected-self.json" 'd["data"]["preflight_id"]')
SELF_PHRASE=$(jget "$PAYLOADS/preflight-protected-self.json" 'd["data"]["confirmation_phrase"]')
expect_eq "被拒的 KILL 预检仍要求再认证" "True|True" \
  "$(jget "$PAYLOADS/preflight-protected-self.json" '"%s|%s" % (d["data"]["requires_reauth"], d["data"]["confirmation_phrase"].startswith("KILL "))')"
expect_eq "被拒的 KILL 预检提交被拒（409）" "409" "$(submit protected-self "$SELF_PF" "$SELF_PHRASE")"
expect_eq "被拒的 KILL 预检的错误码是 DEPENDENCY_BLOCKED" "DEPENDENCY_BLOCKED" \
  "$(jget "$PAYLOADS/job-submit-protected-self.json" 'd["error"]["code"]')"
expect_eq "被拒的 KILL 预检没有被消费" "0" \
  "$(dbq "$WEB_DB" "SELECT count(*) FROM preflights WHERE id = '$SELF_PF' AND consumed_job_id IS NOT NULL")"
if alive "$AGENT_PID"; then pass "代理进程仍然活着"; else fail "代理进程仍然活着" "代理不见了"; fi

if [ -n "$KTHREAD" ]; then
  set -- $KTHREAD
  expect_eq "内核线程的 TERM 预检被拒" "200|False|PROCESS_PROTECTED" \
    "$(c=$(preflight protected-kernel "$1" "$2" TERM); \
       printf '%s|%s|%s' "$c" \
         "$(jget "$PAYLOADS/preflight-protected-kernel.json" 'd["data"]["allowed"]')" \
         "$(jget "$PAYLOADS/preflight-protected-kernel.json" '",".join(b["code"] for b in d["data"]["blockers"])')")"
fi

if [ -n "$SSHD_PID" ]; then
  expect_eq "SSH 的 KILL 预检被拒" "200|False|PROCESS_PROTECTED" \
    "$(c=$(preflight protected-ssh "$SSHD_PID" "$(proc_starttime "$SSHD_PID")" KILL); \
       printf '%s|%s|%s' "$c" \
         "$(jget "$PAYLOADS/preflight-protected-ssh.json" 'd["data"]["allowed"]')" \
         "$(jget "$PAYLOADS/preflight-protected-ssh.json" '",".join(b["code"] for b in d["data"]["blockers"])')")"
  if alive "$SSHD_PID"; then pass "SSH 进程仍然活着"; else fail "SSH 进程仍然活着" "sshd 不见了"; fi
fi

echo
echo "== 8. 参数边界 =="
GONE_PID=$(spawn_sleep 5)
TEST_PIDS="$TEST_PIDS $GONE_PID"
GONE_START=$(proc_starttime "$GONE_PID")
kill -KILL "$GONE_PID" 2>/dev/null
for _ in $(seq 1 50); do alive "$GONE_PID" || break; sleep 0.1; done
expect_eq "不存在的 PID：PROCESS_NOT_FOUND" "200|False|PROCESS_NOT_FOUND" \
  "$(c=$(preflight notfound "$GONE_PID" "$GONE_START" TERM); \
     printf '%s|%s|%s' "$c" \
       "$(jget "$PAYLOADS/preflight-notfound.json" 'd["data"]["allowed"]')" \
       "$(jget "$PAYLOADS/preflight-notfound.json" '",".join(b["code"] for b in d["data"]["blockers"])')")"
expect_eq "不存在的 PID 的展示名说明它不存在" "True" \
  "$(jget "$PAYLOADS/preflight-notfound.json" '"不存在" in d["data"]["target"]["display_name"]')"

# starttime 不符：PID 1 真的存在，但给的 starttime 不是它的。身份判定必须先于保护判定，
# 所以这里的阻塞码应当是 IDENTITY_MISMATCH 而不是 PROTECTED。
expect_eq "starttime 不符：PROCESS_IDENTITY_MISMATCH" "200|False|PROCESS_IDENTITY_MISMATCH" \
  "$(c=$(preflight identity-mismatch 1 $((INIT_START + 1)) TERM); \
     printf '%s|%s|%s' "$c" \
       "$(jget "$PAYLOADS/preflight-identity-mismatch.json" 'd["data"]["allowed"]')" \
       "$(jget "$PAYLOADS/preflight-identity-mismatch.json" '",".join(b["code"] for b in d["data"]["blockers"])')")"
expect_eq "身份不符的预检不持有可执行的锁以外的东西（锁仍是该 pid 的）" "process:1:$((INIT_START + 1))" \
  "$(jget "$PAYLOADS/preflight-identity-mismatch.json" 'd["data"]["locks"][0]')"

expect_eq "signal 不在闭集：400" "400" \
  "$(api "$JAR" "$CSRF" POST "$PREFLIGHT_PATH" \
     "{\"action\":\"process.signal\",\"params\":{\"pid\":1,\"start_time_ticks\":$INIT_START,\"signal\":\"SIGKILL\"}}")"
cp "$WORK/resp.json" "$PAYLOADS/preflight-bad-signal.json" 2>/dev/null
expect_eq "signal 不在闭集的错误码" "VALIDATION_ERROR" \
  "$(jget "$PAYLOADS/preflight-bad-signal.json" 'd["error"]["code"]')"
expect_eq "缺少 start_time_ticks：400" "400" \
  "$(api "$JAR" "$CSRF" POST "$PREFLIGHT_PATH" \
     "{\"action\":\"process.signal\",\"params\":{\"pid\":1,\"signal\":\"TERM\"}}")"
expect_eq "缺少 start_time_ticks 的错误码" "VALIDATION_ERROR" \
  "$(jget "$WORK/resp.json" 'd["error"]["code"]')"
expect_eq "pid=0：400" "400" \
  "$(api "$JAR" "$CSRF" POST "$PREFLIGHT_PATH" \
     "{\"action\":\"process.signal\",\"params\":{\"pid\":0,\"start_time_ticks\":12345,\"signal\":\"TERM\"}}")"
expect_eq "未登记的动作被拒绝（400）" "400" \
  "$(api "$JAR" "$CSRF" POST "$PREFLIGHT_PATH" \
     '{"action":"process.killall","params":{"signal":"KILL"}}')"

echo
echo "== 9. PID 重用：旧预检不能杀新进程 =="
REUSE_PID=$(spawn_sleep 600)
REUSE_START=$(proc_starttime "$REUSE_PID")
expect_eq "重用测试目标已启动" "True" "$([ -n "$REUSE_START" ] && echo True || echo False)"
expect_eq "重用前的预检是允许的" "200|True" \
  "$(c=$(preflight reused-old "$REUSE_PID" "$REUSE_START" TERM); \
     printf '%s|%s' "$c" "$(jget "$PAYLOADS/preflight-reused-old.json" 'd["data"]["allowed"]')")"
REUSE_PF=$(jget "$PAYLOADS/preflight-reused-old.json" 'd["data"]["preflight_id"]')
REUSE_IDENTITY=$(jget "$PAYLOADS/preflight-reused-old.json" 'd["data"]["target"]["identity_fingerprint"]')

# 面板之外把老进程杀掉，再逼内核把同一个 PID 分给新进程。
kill -KILL "$REUSE_PID" 2>/dev/null
for _ in $(seq 1 50); do alive "$REUSE_PID" || break; sleep 0.1; done
if alive "$REUSE_PID"; then
  fail "老进程已退出" "pid=$REUSE_PID 仍然存在"
else
  pass "老进程已退出（PID $REUSE_PID 空出来了）"
fi

NEW_PID=""
NEW_START=""
if [ -w /proc/sys/kernel/ns_last_pid ] && [ "$REUSE_PID" -gt 1 ]; then
  for _ in $(seq 1 40); do
    printf '%s' "$((REUSE_PID - 1))" >/proc/sys/kernel/ns_last_pid 2>/dev/null
    sleep 600 >/dev/null 2>&1 &
    _pid=$!
    if [ "$_pid" = "$REUSE_PID" ]; then
      NEW_PID=$_pid
      NEW_START=$(proc_starttime "$_pid")
      break
    fi
    kill -KILL "$_pid" 2>/dev/null
    wait "$_pid" 2>/dev/null
  done
fi
TEST_PIDS="$TEST_PIDS ${NEW_PID:-}"

if [ -n "$NEW_PID" ]; then
  pass "内核把同一个 PID 分给了新进程（pid=$NEW_PID，starttime 从 $REUSE_START 变成 $NEW_START）"
  if [ "$NEW_START" != "$REUSE_START" ]; then
    pass "新进程的 starttime 与老进程不同（身份不同）"
  else
    fail "新进程的 starttime 与老进程不同" "两边都是 $NEW_START"
  fi
  expect_eq "旧预检的身份指纹不是新进程的" "True" \
    "$(python3 - "$PAYLOADS/preflight-reused-old.json" "$NEW_PID" "$NEW_START" <<'PY'
import hashlib, json, sys
old = json.load(open(sys.argv[1], encoding="utf-8"))["data"]["target"]["identity_fingerprint"]
new = "sha256:" + hashlib.sha256(("process/v1|%s|%s" % (sys.argv[2], sys.argv[3])).encode()).hexdigest()
print(old != new)
PY
)"
  expect_eq "拿旧预检提交被拒绝（409）" "409" "$(submit reused-old "$REUSE_PF" "")"
  expect_eq "拒绝码是 DEVICE_IDENTITY_CHANGED" "DEVICE_IDENTITY_CHANGED" \
    "$(jget "$PAYLOADS/job-submit-reused-old.json" 'd["error"]["code"]')"
  if alive "$NEW_PID" && [ "$(proc_starttime "$NEW_PID")" = "$NEW_START" ]; then
    pass "被重用的 PID 上的新进程毫发无伤（这正是本子任务的核心验收点）"
  else
    fail "被重用的 PID 上的新进程毫发无伤" "pid=$NEW_PID starttime 现在是 '$(proc_starttime "$NEW_PID")'"
  fi
  expect_eq "对新进程重新预检才是允许的" "200|True" \
    "$(c=$(preflight reused-new "$NEW_PID" "$NEW_START" TERM); \
       printf '%s|%s' "$c" "$(jget "$PAYLOADS/preflight-reused-new.json" 'd["data"]["allowed"]')")"
  NEW_PF=$(jget "$PAYLOADS/preflight-reused-new.json" 'd["data"]["preflight_id"]')
  expect_eq "按新身份提交成功" "202" "$(submit reused-new "$NEW_PF" "")"
  NEW_JOB=$(jget "$PAYLOADS/job-submit-reused-new.json" 'd["data"]["job_id"]')
  expect_eq "新身份的任务成功" "succeeded" "$(poll_job reused-new "$NEW_JOB")"
else
  notrun "PID 重用（旧预检不能杀新进程）" \
    "/proc/sys/kernel/ns_last_pid 不可写或 40 次内没能让内核复用 PID $REUSE_PID；旧预检的失效由 agent 单元测试与 409 复核覆盖"
fi

echo
echo "== 10. 权限与审计 =="
# CONTRACTS 第 10 节的权限列把 GET /processes 写成 admin：进程列表带命令行与属主，
# 不是“任何登录用户可读”的服务状态。viewer 拿到的是 403，不是一份裁剪过的列表。
expect_eq "viewer 读进程列表被拒（403）" "403" "$(api "$VJAR" "" GET "$PROCESSES_PATH")"
cp "$WORK/resp.json" "$PAYLOADS/processes-viewer-forbidden.json" 2>/dev/null
expect_eq "viewer 读进程列表的错误码" "FORBIDDEN" \
  "$(jget "$PAYLOADS/processes-viewer-forbidden.json" 'd["error"]["code"]')"
VIEWER_PID=$(spawn_sleep 60)
TEST_PIDS="$TEST_PIDS $VIEWER_PID"
VIEWER_START=$(proc_starttime "$VIEWER_PID")
expect_eq "viewer 不能发起进程信号预检（403）" "403" \
  "$(api "$VJAR" "" POST "$PREFLIGHT_PATH" \
     "{\"action\":\"process.signal\",\"params\":{\"pid\":$VIEWER_PID,\"start_time_ticks\":$VIEWER_START,\"signal\":\"TERM\"}}")"
cp "$WORK/resp.json" "$PAYLOADS/preflight-viewer.json" 2>/dev/null
expect_eq "viewer 被拒的错误码" "FORBIDDEN" \
  "$(jget "$PAYLOADS/preflight-viewer.json" 'd["error"]["code"]')"
if alive "$VIEWER_PID"; then
  pass "viewer 的尝试没有影响进程（pid=$VIEWER_PID 仍在）"
else
  fail "viewer 的尝试没有影响进程" "pid=$VIEWER_PID 已经不在了"
fi
kill -KILL "$VIEWER_PID" 2>/dev/null

# 审计：Web 侧留下 operation.preflight 与 job.submit，代理侧留下 operations 记录。
PREFLIGHT_AUDIT=$(dbq "$WEB_DB" "SELECT count(*) FROM audit_events WHERE action = 'operation.preflight' AND target = 'preflight:process.signal'")
if [ "${PREFLIGHT_AUDIT:-0}" -ge 3 ]; then
  pass "审计里有 $PREFLIGHT_AUDIT 条 process.signal 的预检记录"
else
  fail "审计里有 process.signal 的预检记录" "只有 '${PREFLIGHT_AUDIT}' 条"
fi
SUBMIT_AUDIT=$(dbq "$WEB_DB" "SELECT count(*) FROM audit_events WHERE action = 'job.submit' AND outcome = 'success' AND redacted_details LIKE '%process.signal%'")
if [ "${SUBMIT_AUDIT:-0}" -ge 2 ]; then
  pass "审计里有 $SUBMIT_AUDIT 条 process.signal 的任务提交记录"
else
  fail "审计里有 process.signal 的任务提交记录" "只有 '${SUBMIT_AUDIT}' 条"
fi
DENIED_AUDIT=$(dbq "$WEB_DB" "SELECT count(*) FROM audit_events WHERE outcome = 'failure' AND (target = 'preflight:process.signal' OR action IN ('job.submit', 'preflight.create', 'operation.preflight'))")
if [ "${DENIED_AUDIT:-0}" -ge 1 ]; then
  pass "被拒绝的请求也进了审计（$DENIED_AUDIT 条失败记录）"
else
  fail "被拒绝的请求也进了审计" "没有失败记录"
fi
AGENT_OPS=$(dbq "$AGENT_DB" "SELECT count(*) FROM operations WHERE action = 'process.signal'")
if [ "${AGENT_OPS:-0}" -ge 2 ]; then
  pass "代理侧有 $AGENT_OPS 条 process.signal 操作记录"
else
  fail "代理侧有 process.signal 操作记录" "只有 '${AGENT_OPS}' 条"
fi
AGENT_STAGES=$(dbq "$AGENT_DB" "SELECT group_concat(DISTINCT stage) FROM operations WHERE action = 'process.signal'")
case "$AGENT_STAGES" in
  *succeeded*) pass "代理侧操作里有 succeeded 终态（$AGENT_STAGES）" ;;
  *) fail "代理侧操作里有 succeeded 终态" "$AGENT_STAGES" ;;
esac
PHRASE_ROWS=$(dbq "$AGENT_DB" "SELECT count(*) FROM preflight_plans WHERE action = 'process.signal' AND phrase_hash <> ''")
if [ "${PHRASE_ROWS:-0}" -ge 1 ]; then
  pass "KILL 的预检计划里存的是短语哈希（$PHRASE_ROWS 条非空，明文不落库）"
else
  fail "KILL 的预检计划里存的是短语哈希" "没有非空 phrase_hash"
fi

echo
echo "== 11. 载荷校验（字段、闭集、null 语义、与 /proc 对照） =="
if [ -f "$VALIDATE" ]; then
  python3 "$VALIDATE" "$PAYLOADS"
  VALIDATE_RC=$?
  if [ "$VALIDATE_RC" -eq 0 ]; then pass "载荷校验通过"; else fail "载荷校验通过" "退出码 $VALIDATE_RC"; fi
else
  notrun "载荷校验" "找不到 $VALIDATE"
fi

echo
echo "== 12. 汇总 =="
printf 'PASS=%s FAIL=%s NOT RUN=%s\n' "$PASSED" "$FAILED" "$NOTRUN"
[ -n "$FAILED_NAMES" ] && printf '失败项:%s\n' "$FAILED_NAMES"
printf 'NOT RUN 不等于通过；每一条都在脚本头部写明了原因。\n'
if [ "$FAILED" -eq 0 ]; then
  exit 0
fi
exit 1

