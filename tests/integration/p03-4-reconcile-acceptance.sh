#!/bin/sh
# P03.4 Linux 验收脚本：Web 与代理重启对账、重复提交、执行结果未知的 interrupted 处理。
#
# 运行位置：Ubuntu 22.04/24.04 验收 VM 内，**必须以 root 运行**（代理只能以 root 启动，
# 脚本还要能 kill 代理、替换代理库来制造“重启”和“代理侧数据丢失”）。
# 不依赖 Go 工具链：二进制由宿主交叉编译，脚本只做黑盒验收与查库。
#
# 用法：
#   sudo BIN_DIR=/tmp/upanel-bin sh tests/integration/p03-4-reconcile-acceptance.sh
#
# 退出码：0 全部通过；1 有失败项；2 环境不满足（非 root）。
#
# 覆盖的验收点（STAGES P03.4、CONTRACTS §4.2/§4.3、JOB-01/JOB-03）：
#   1. 生产登记表为空：不带 -acceptance-actions 时代理拒绝验收动作，启用了才接受
#   2. 同一个幂等键提交十次：一个任务、一次代理操作；并发同一个键也是同一个任务
#   3. 同一个键配不同请求 → 409 IDEMPOTENCY_CONFLICT
#   4. Web 重启后重放同一个键：拿回原任务，代理侧操作数不变（不重新执行）
#   5. 提交时代理不可用 → 任务留在 queued/needs_sync=1；代理恢复后**启动扫描**把它判为
#      failed + unconfirmed（提交没到达），而不是永远挂着
#   6. 代理被 kill -9（崩溃）→ 重启后把遗留操作标 interrupted；Web 启动扫描同步为
#      interrupted，代理侧操作数不变（没有自动重放）
#   7. 代理侧记录丢失（换空库）→ 已确认过的任务变成 interrupted + unknown_result
#      （结果未知），不是 failed；这条由**后台周期扫描**完成，不靠读接口触发
#   8. 终态不回退、interrupted 不可取消、没有“重试”接口
#   9. Web 日志里有启动对账的记录，日志里没有口令 / CSRF 令牌
#
# 说明：宿主单元测试覆盖同一批语义的细粒度分支（internal/operations/reconcile_test.go），
#   VM 这里验证它们在真实进程、真实 socket、真实崩溃下成立。

set -u

BIN_DIR=${BIN_DIR:-/tmp/upanel-bin}
if [ "$(id -u)" -ne 0 ]; then
  printf '本脚本必须以 root 运行：sudo BIN_DIR=%s sh %s\n' "$BIN_DIR" "$0" >&2
  exit 2
fi

AGENT="$BIN_DIR/upanel-agent"
WEB="$BIN_DIR/upanel-web"
CTL="$BIN_DIR/upanelctl"

WORK=${WORK:-/var/tmp/upanel-accept-p03-4}
RUN_DIR=/run/upanel
SOCK=$RUN_DIR/agent.sock
SYS_CFG=/etc/upanel/upanel.json
AGENT_DB_DIR=/var/lib/upanel-agent
AGENT_DB=$AGENT_DB_DIR/agent.db
KEY_FILE=/etc/upanel/agent.key
WEB_DB_DIR=$WORK/webdata
WEB_DB=$WEB_DB_DIR/upanel.db
PORT=19446
BASE="http://127.0.0.1:$PORT"
ORIGIN="$BASE"
WEB_USER=upanel-web
PASSWORD='vm-correct-horse-9'
OTHER_PASSWORD='vm-other-horse-3'

AGENT_LOG=$WORK/logs/agent.log
WEB_LOG=$WORK/logs/web.log

PASSED=0
FAILED=0
FAILED_NAMES=""
AGENT_PID=""
WEB_PID=""

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

# weblog_has <子串>：日志里是否出现过这个子串。
weblog_has() {
  if grep -qF -- "$1" "$WEB_LOG" 2>/dev/null; then return 0; fi
  return 1
}

stop_web() {
  if [ -n "$WEB_PID" ] && kill -0 "$WEB_PID" 2>/dev/null; then
    kill -TERM "$WEB_PID" 2>/dev/null
    wait "$WEB_PID" 2>/dev/null
  fi
  # runuser 不保证把信号转给子进程：按进程名兜底，确保端口真的释放，
  # 否则下一次 start_web 会因为“地址已占用”直接失败，看起来像功能坏了。
  pkill -TERM -u "$WEB_USER" -f "upanel-web --mode production --db $WEB_DB" 2>/dev/null
  for _ in $(seq 1 100); do
    curl -fsS "$BASE/api/v1/health/live" >/dev/null 2>&1 || { WEB_PID=""; return 0; }
    sleep 0.1
  done
  pkill -KILL -u "$WEB_USER" -f "upanel-web --mode production --db $WEB_DB" 2>/dev/null
  WEB_PID=""
  return 0
}

stop_agent() {
  if [ -n "$AGENT_PID" ] && kill -0 "$AGENT_PID" 2>/dev/null; then
    kill -TERM "$AGENT_PID" 2>/dev/null
    wait "$AGENT_PID" 2>/dev/null
  fi
  AGENT_PID=""
  rm -f "$SOCK"
}

cleanup() {
  stop_web
  stop_agent
}
trap cleanup EXIT
trap 'cleanup; exit 130' INT
trap 'cleanup; exit 143' TERM

# start_agent [extra args...]
start_agent() {
  "$AGENT" --config "$SYS_CFG" "$@" >>"$AGENT_LOG" 2>&1 &
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
  for _ in $(seq 1 150); do
    if curl -fsS "$BASE/api/v1/health/live" >/dev/null 2>&1; then return 0; fi
    sleep 0.2
  done
  return 1
}

login() {
  curl -s --max-time 60 -c "$3" -o "$WORK/$4.json" -w '%{http_code}' -X POST "$BASE/api/v1/auth/login" \
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
      curl -s --max-time 60 -b "$jar" -o "$WORK/resp.json" -w '%{http_code}' -X "$method" "$BASE$path" \
        -H 'Content-Type: application/json' -H "Origin: $ORIGIN" -H "X-CSRF-Token: $csrf" \
        -H "Idempotency-Key: $key" -d @"$WORK/req.json"
    else
      curl -s --max-time 60 -b "$jar" -o "$WORK/resp.json" -w '%{http_code}' -X "$method" "$BASE$path" \
        -H 'Content-Type: application/json' -H "Origin: $ORIGIN" -H "X-CSRF-Token: $csrf" \
        -d @"$WORK/req.json"
    fi
  else
    curl -s --max-time 60 -b "$jar" -o "$WORK/resp.json" -w '%{http_code}' -X "$method" "$BASE$path" \
      -H "Origin: $ORIGIN" -H "X-CSRF-Token: $csrf"
  fi
}

# preflight <action> <params>：生成预检，把响应写进 $WORK/resp.json。
preflight() {
  api "$WORK/admin.jar" "$ADMIN_CSRF" POST /api/v1/operations/preflight \
    "{\"action\":\"$1\",\"params\":$2}"
}

# submit <preflight_id> <phrase> <key> <输出名>：提交任务，返回 HTTP 码，响应写进 <输出名>.json。
submit() {
  curl -s --max-time 60 -b "$WORK/admin.jar" -o "$WORK/$4.json" -w '%{http_code}' -X POST "$BASE/api/v1/jobs" \
    -H 'Content-Type: application/json' -H "Origin: $ORIGIN" -H "X-CSRF-Token: $ADMIN_CSRF" \
    -H "Idempotency-Key: $3" \
    -d "{\"preflight_id\":\"$1\",\"confirmation_phrase\":\"$2\"}"
}

agent_ops() { dbq "$AGENT_DB" 'SELECT COUNT(*) FROM operations'; }

# wait_db <job_id> <SQL 里 status 的期望值> <秒>
wait_db_status() {
  _job=$1; _want=$2; _secs=${3:-40}
  _n=0
  while [ "$_n" -lt "$((_secs * 10))" ]; do
    _cur=$(dbq "$WEB_DB" "SELECT status FROM jobs WHERE id = '$_job'")
    [ "$_cur" = "$_want" ] && return 0
    sleep 0.1
    _n=$((_n + 1))
  done
  return 1
}

wait_for_status() {
  _job=$1; _want=$2; _secs=${3:-10}
  _n=0
  while [ "$_n" -lt "$((_secs * 10))" ]; do
    api "$WORK/admin.jar" "$ADMIN_CSRF" GET "/api/v1/jobs/$_job" >/dev/null
    _cur=$(jget "$WORK/resp.json" 'd["data"]["status"]')
    [ "$_cur" = "$_want" ] && return 0
    sleep 0.1
    _n=$((_n + 1))
  done
  return 1
}

echo "== 0. 环境"
if [ "$(id -u)" -eq 0 ]; then pass "验收脚本以 root 运行"; else fail "验收脚本以 root 运行" "当前 UID $(id -u)"; fi
for b in "$AGENT" "$WEB" "$CTL"; do
  if [ -x "$b" ]; then pass "二进制存在且可执行 $(basename "$b")"; else fail "二进制存在且可执行 $(basename "$b")" "$b"; fi
done
if command -v python3 >/dev/null 2>&1; then pass "python3 可用于查库与解析 JSON"; else fail "python3 可用" "缺少 python3"; fi

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
echo "== 1. 启动代理（生产登记表）与 Web =="
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

if start_agent; then pass "代理（不带 -acceptance-actions）启动"; else fail "代理启动" "$(tail -5 "$AGENT_LOG" 2>/dev/null)"; fi

INIT_OUT=$(printf '%s\n%s\n' "$PASSWORD" "$PASSWORD" | "$CTL" init-admin --config "$SYS_CFG" --db "$WEB_DB" \
  --username admin --display-name "管理员" --json 2>"$WORK/init.err")
expect_eq "init-admin 退出码" "0" "$?"
chown "$WEB_USER" "$WEB_DB"
for suffix in "" "-wal" "-shm"; do
  [ -e "$WEB_DB$suffix" ] && chown "$WEB_USER" "$WEB_DB$suffix"
done

if start_web; then pass "Web 以 $WEB_USER 身份在 $BASE 启动"; else fail "Web 启动" "$(tail -5 "$WEB_LOG" 2>/dev/null)"; fi

echo
echo "== 2. 生产登记表为空：验收动作只在显式启用时存在 =="
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

code=$(preflight 'acceptance.probe' '{"label":"alpha"}')
expect_eq "生产模式拒绝验收动作 → 400" "400" "$code"
expect_eq "拒绝码是 VALIDATION_ERROR" "VALIDATION_ERROR" "$(jget "$WORK/resp.json" 'd["error"]["code"]')"
expect_contains "拒绝原因写的是动作未登记" "未登记的动作" "$(jget "$WORK/resp.json" 'd["error"]["message"]')"

stop_agent
if start_agent -acceptance-actions; then pass "代理以 -acceptance-actions 重启（验收动作显式启用）"; else fail "代理重启" "$(tail -5 "$AGENT_LOG")"; fi
code=$(preflight 'acceptance.probe' '{"label":"alpha"}')
expect_eq "启用验收动作后预检 200" "200" "$code"

echo
echo "== 3. 重复提交：十次同一个键，一次执行 =="
PF_ID=$(jget "$WORK/resp.json" 'd["data"]["preflight_id"]')
PF_PHRASE=$(jget "$WORK/resp.json" 'd["data"]["confirmation_phrase"]')
OPS_BEFORE=$(agent_ops)
FIRST_JOB=""
i=1
while [ "$i" -le 10 ]; do
  code=$(submit "$PF_ID" "$PF_PHRASE" "p034-ten" "submit-ten-$i")
  if [ "$code" != "202" ]; then
    fail "第 $i 次提交返回 202" "HTTP $code：$(cat "$WORK/submit-ten-$i.json")"
    break
  fi
  _job=$(jget "$WORK/submit-ten-$i.json" 'd["data"]["job_id"]')
  if [ -z "$FIRST_JOB" ]; then
    FIRST_JOB=$_job
  elif [ "$_job" != "$FIRST_JOB" ]; then
    fail "第 $i 次提交拿到原任务" "得到 $_job，首次是 $FIRST_JOB"
  fi
  i=$((i + 1))
done
if [ "$i" -gt 10 ]; then pass "十次提交都返回 202"; fi
expect_eq "十次提交产生一个任务" "1" "$(dbq "$WEB_DB" 'SELECT COUNT(*) FROM jobs')"
OPS_AFTER=$(agent_ops)
expect_eq "十次提交只让代理收到一次操作" "$((OPS_BEFORE + 1))" "$OPS_AFTER"

echo
echo "== 4. 并发提交同一个键：仍然一个任务 =="
code=$(preflight 'acceptance.ping' '{"note":"concurrent"}')
expect_eq "并发用例的预检 200" "200" "$code"
PF2=$(jget "$WORK/resp.json" 'd["data"]["preflight_id"]')
PF2_PHRASE=$(jget "$WORK/resp.json" 'd["data"]["confirmation_phrase"]')
OPS_BEFORE=$(agent_ops)
n=1
SUBMIT_PIDS=""
while [ "$n" -le 5 ]; do
  submit "$PF2" "$PF2_PHRASE" "p034-concurrent" "concurrent-$n" >"$WORK/concurrent-$n.code" &
  SUBMIT_PIDS="$SUBMIT_PIDS $!"
  n=$((n + 1))
done
# 只等这五路提交：不带参数的 wait 会连 start_agent / start_web 起的后台进程一起等，
# 那两个直到脚本结束才退出——第一版脚本就是卡在这里。
for _pid in $SUBMIT_PIDS; do wait "$_pid" 2>/dev/null; done
CONCURRENT_JOBS=$(for n in 1 2 3 4 5; do jget "$WORK/concurrent-$n.json" 'd["data"]["job_id"]'; done | sort -u | wc -l | tr -d ' ')
expect_eq "五路并发提交只产生一个任务 id" "1" "$CONCURRENT_JOBS"
CONCURRENT_JOB=$(jget "$WORK/concurrent-1.json" 'd["data"]["job_id"]')
for n in 2 3 4 5; do
  _j=$(jget "$WORK/concurrent-$n.json" 'd["data"]["job_id"]')
  [ "$_j" = "$CONCURRENT_JOB" ] || fail "并发第 $n 路拿到原任务" "得到 $_j，期望 $CONCURRENT_JOB"
done
pass "并发各路都拿到同一个任务 id"
OPS_AFTER=$(agent_ops)
expect_eq "五路并发只让代理收到一次操作" "$((OPS_BEFORE + 1))" "$OPS_AFTER"

echo
echo "== 5. 同一个键配不同请求 → 409 =="
# 幂等判定按“键 + 请求内容”的哈希：这里把确认短语换掉，请求内容就不同了。
# 同一个键配不同内容必须冲突，否则“换个参数再提交一次”会被当成重放而静默吞掉。
JOBS_BEFORE=$(dbq "$WEB_DB" 'SELECT COUNT(*) FROM jobs')
code=$(submit "$PF_ID" "PROBE ZZZZ" "p034-ten" "conflict")
expect_eq "同键不同请求 → 409" "409" "$code"
expect_eq "同键不同请求的错误码" "IDEMPOTENCY_CONFLICT" "$(jget "$WORK/conflict.json" 'd["error"]["code"]')"
expect_eq "冲突没有产生新任务" "$JOBS_BEFORE" "$(dbq "$WEB_DB" 'SELECT COUNT(*) FROM jobs')"

echo
echo "== 6. Web 重启后重放同一个键：原任务、不重新执行 =="
OPS_BEFORE=$(agent_ops)
JOBS_BEFORE=$(dbq "$WEB_DB" 'SELECT COUNT(*) FROM jobs')
stop_web
if start_web; then pass "Web 重启成功"; else fail "Web 重启" "$(tail -5 "$WEB_LOG")"; fi
code=$(login admin "$PASSWORD" "$WORK/admin.jar" login-admin2)
expect_eq "重启后重新登录 200" "200" "$code"
ADMIN_CSRF=$(csrf_of "$WORK/login-admin2.json")
code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST /api/v1/auth/reauth "{\"password\":\"$PASSWORD\"}")
expect_eq "重启后再认证 200" "200" "$code"
code=$(submit "$PF_ID" "$PF_PHRASE" "p034-ten" "after-restart")
expect_eq "重启后重放同一个键 202" "202" "$code"
expect_eq "重启后重放拿回原任务" "$FIRST_JOB" "$(jget "$WORK/after-restart.json" 'd["data"]["job_id"]')"
expect_eq "重启后重放没有新增任务" "$JOBS_BEFORE" "$(dbq "$WEB_DB" 'SELECT COUNT(*) FROM jobs')"
expect_eq "重启后重放没有让代理收到新操作" "$OPS_BEFORE" "$(agent_ops)"

echo
echo "== 7. 提交没到达：代理恢复后由启动扫描判为 failed/unconfirmed =="
code=$(preflight 'acceptance.probe' '{"label":"noagent"}')
expect_eq "提交前预检 200" "200" "$code"
PF3=$(jget "$WORK/resp.json" 'd["data"]["preflight_id"]')
PF3_PHRASE=$(jget "$WORK/resp.json" 'd["data"]["confirmation_phrase"]')
stop_agent
code=$(submit "$PF3" "$PF3_PHRASE" "p034-noagent" "noagent")
expect_eq "代理不可用时提交仍受理（202）" "202" "$code"
NOAGENT_JOB=$(jget "$WORK/noagent.json" 'd["data"]["job_id"]')
expect_eq "任务留在 queued" "queued" "$(jget "$WORK/noagent.json" 'd["data"]["status"]')"
expect_eq "任务带待对账标记" "1" "$(dbq "$WEB_DB" "SELECT needs_sync FROM jobs WHERE id = '$NOAGENT_JOB'")"
if start_agent -acceptance-actions; then pass "代理恢复启动"; else fail "代理恢复启动" "$(tail -5 "$AGENT_LOG")"; fi
# 走“启动扫描”而不是读接口：重启 Web，让启动那一轮扫描负责判定。
stop_web
if start_web; then pass "Web 重启（触发启动对账）"; else fail "Web 重启" "$(tail -5 "$WEB_LOG")"; fi
if wait_db_status "$NOAGENT_JOB" failed 20; then
  pass "启动扫描把未到达的提交判为 failed"
else
  fail "启动扫描把未到达的提交判为 failed" "status=$(dbq "$WEB_DB" "SELECT status FROM jobs WHERE id = '$NOAGENT_JOB'")"
fi
expect_eq "失败步骤是 unconfirmed" "unconfirmed" "$(dbq "$WEB_DB" "SELECT step FROM jobs WHERE id = '$NOAGENT_JOB'")"
expect_contains "错误码指出提交可能未到达" "AGENT_UNAVAILABLE" \
  "$(dbq "$WEB_DB" "SELECT error_json FROM jobs WHERE id = '$NOAGENT_JOB'")"
if weblog_has "任务对账扫描完成"; then
  pass "启动对账在日志里留了扫描记录"
else
  fail "启动对账在日志里留了扫描记录" "Web 日志里没有“任务对账扫描完成”"
fi

code=$(login admin "$PASSWORD" "$WORK/admin.jar" login-admin3)
expect_eq "再次登录 200" "200" "$code"
ADMIN_CSRF=$(csrf_of "$WORK/login-admin3.json")
api "$WORK/admin.jar" "$ADMIN_CSRF" POST /api/v1/auth/reauth "{\"password\":\"$PASSWORD\"}" >/dev/null

echo
echo "== 8. 代理崩溃：遗留操作标 interrupted，Web 同步为 interrupted 且不重放 =="
code=$(preflight 'acceptance.ping' '{"note":"crash"}')
expect_eq "崩溃用例预检 200" "200" "$code"
PF4=$(jget "$WORK/resp.json" 'd["data"]["preflight_id"]')
PF4_PHRASE=$(jget "$WORK/resp.json" 'd["data"]["confirmation_phrase"]')
code=$(submit "$PF4" "$PF4_PHRASE" "p034-crash" "crash")
expect_eq "崩溃用例提交 202" "202" "$code"
CRASH_JOB=$(jget "$WORK/crash.json" 'd["data"]["job_id"]')
if wait_for_status "$CRASH_JOB" running 10; then pass "任务进入 running"; else
  fail "任务进入 running" "$(jget "$WORK/resp.json" 'd["data"]["status"]')"
fi
OPS_BEFORE=$(agent_ops)
kill -KILL "$AGENT_PID" 2>/dev/null
wait "$AGENT_PID" 2>/dev/null
AGENT_PID=""
rm -f "$SOCK"
note "代理被 kill -9（模拟崩溃，来不及写任何收尾）"
if start_agent -acceptance-actions; then pass "代理崩溃后重启"; else fail "代理崩溃后重启" "$(tail -5 "$AGENT_LOG")"; fi
# 断言要指名这条操作：用 Web 库里记的 operation_id 去查代理库。
# 按 created_at 取“最新一条”在同秒级精度下不唯一，取到别的操作会让断言失去意义。
CRASH_OP=$(dbq "$WEB_DB" "SELECT operation_id FROM jobs WHERE id = '$CRASH_JOB'")
expect_eq "代理侧遗留操作被标 interrupted" "interrupted" \
  "$(dbq "$AGENT_DB" "SELECT stage FROM operations WHERE operation_id = '$CRASH_OP'")"
stop_web
if start_web; then pass "Web 重启（触发启动对账）"; else fail "Web 重启" "$(tail -5 "$WEB_LOG")"; fi
if wait_db_status "$CRASH_JOB" interrupted 20; then
  pass "Web 启动扫描把任务同步为 interrupted"
else
  fail "Web 启动扫描把任务同步为 interrupted" "status=$(dbq "$WEB_DB" "SELECT status FROM jobs WHERE id = '$CRASH_JOB'")"
fi
expect_eq "崩溃后的任务不再可取消" "0" "$(dbq "$WEB_DB" "SELECT cancellable FROM jobs WHERE id = '$CRASH_JOB'")"
expect_contains "错误码是 OPERATION_INTERRUPTED" "OPERATION_INTERRUPTED" \
  "$(dbq "$WEB_DB" "SELECT error_json FROM jobs WHERE id = '$CRASH_JOB'")"
expect_eq "对账没有重新提交（代理操作数不变）" "$OPS_BEFORE" "$(agent_ops)"

echo
echo "== 9. 终态不回退、不可取消、没有重试接口 =="
code=$(login admin "$PASSWORD" "$WORK/admin.jar" login-admin4)
expect_eq "再次登录 200" "200" "$code"
ADMIN_CSRF=$(csrf_of "$WORK/login-admin4.json")
api "$WORK/admin.jar" "$ADMIN_CSRF" POST /api/v1/auth/reauth "{\"password\":\"$PASSWORD\"}" >/dev/null
code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "/api/v1/jobs/$CRASH_JOB/cancel" '{}')
expect_eq "取消 interrupted 任务 → 409" "409" "$code"
code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" GET "/api/v1/jobs/$CRASH_JOB")
expect_eq "再读一次仍是 interrupted" "interrupted" "$(jget "$WORK/resp.json" 'd["data"]["status"]')"
code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "/api/v1/jobs/$CRASH_JOB/retry" '{}')
expect_eq "没有“重试”接口" "404" "$code"
code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "/api/v1/jobs/$CRASH_JOB/resume" '{}')
expect_eq "没有“继续”接口" "404" "$code"

echo
echo "== 10. 代理侧记录丢失：结果未知必须是 interrupted，而不是 failed =="
code=$(preflight 'acceptance.ping' '{"note":"lost"}')
expect_eq "丢失用例预检 200" "200" "$code"
PF5=$(jget "$WORK/resp.json" 'd["data"]["preflight_id"]')
PF5_PHRASE=$(jget "$WORK/resp.json" 'd["data"]["confirmation_phrase"]')
code=$(submit "$PF5" "$PF5_PHRASE" "p034-lost" "lost")
expect_eq "丢失用例提交 202" "202" "$code"
LOST_JOB=$(jget "$WORK/lost.json" 'd["data"]["job_id"]')
if wait_for_status "$LOST_JOB" running 10; then pass "丢失用例进入 running（代理已确认过这条操作）"; else
  fail "丢失用例进入 running" "$(jget "$WORK/resp.json" 'd["data"]["status"]')"
fi
expect_eq "同步过的任务不再带待对账标记" "0" "$(dbq "$WEB_DB" "SELECT needs_sync FROM jobs WHERE id = '$LOST_JOB'")"
OPS_BEFORE=$(agent_ops)

# 制造“代理侧数据丢失”：杀掉代理，把代理库挪走，重启后是一份空库。
kill -KILL "$AGENT_PID" 2>/dev/null
wait "$AGENT_PID" 2>/dev/null
AGENT_PID=""
rm -f "$SOCK"
mv "$AGENT_DB" "$AGENT_DB.lost"
rm -f "$AGENT_DB-wal" "$AGENT_DB-shm"
if start_agent -acceptance-actions; then pass "代理以空库重启（模拟代理侧记录丢失）"; else fail "代理以空库重启" "$(tail -5 "$AGENT_LOG")"; fi
LOST_OP=$(dbq "$WEB_DB" "SELECT operation_id FROM jobs WHERE id = '$LOST_JOB'")
expect_eq "空库里确实没有这条操作" "" "$(dbq "$AGENT_DB" "SELECT operation_id FROM operations WHERE operation_id = '$LOST_OP'")"

# 这条走**后台周期扫描**：不读接口、不重启 Web，最多等一个扫描间隔 + 余量。
note "等待后台对账扫描（每 30 秒一次，最多等 45 秒）"
if wait_db_status "$LOST_JOB" interrupted 45; then
  pass "后台扫描把“代理查不到”的任务判为 interrupted"
else
  fail "后台扫描把“代理查不到”的任务判为 interrupted" "status=$(dbq "$WEB_DB" "SELECT status FROM jobs WHERE id = '$LOST_JOB'")"
fi
expect_eq "步骤写的是 unknown_result" "unknown_result" \
  "$(dbq "$WEB_DB" "SELECT step FROM jobs WHERE id = '$LOST_JOB'")"
expect_contains "错误体指出结果未知" "OPERATION_INTERRUPTED" \
  "$(dbq "$WEB_DB" "SELECT error_json FROM jobs WHERE id = '$LOST_JOB'")"
expect_contains "错误体要求先核对实际状态" "核对" \
  "$(dbq "$WEB_DB" "SELECT error_json FROM jobs WHERE id = '$LOST_JOB'")"
expect_eq "结果未知没有被写成 failed" "interrupted" \
  "$(dbq "$WEB_DB" "SELECT status FROM jobs WHERE id = '$LOST_JOB'")"
expect_eq "对账同样没有重新提交" "0" "$(dbq "$AGENT_DB" 'SELECT COUNT(*) FROM operations')"

echo
echo "== 11. 日志：启动对账有记录，且不含口令与 CSRF =="
expect_contains "Web 日志里有“开始任务对账”" "开始任务对账" "$(cat "$WEB_LOG")"
expect_contains "Web 日志里有扫描结果" "任务对账扫描完成" "$(cat "$WEB_LOG")"
expect_contains "扫描结果带未确认计数" "unconfirmed" "$(cat "$WEB_LOG")"
expect_not_contains "Web 日志不含登录口令" "$PASSWORD" "$(cat "$WEB_LOG")"
CSRF_IN_LOG=""
ADMIN_CSRF=$(csrf_of "$WORK/login-admin4.json")
if [ -n "$ADMIN_CSRF" ]; then
  if weblog_has "$ADMIN_CSRF"; then CSRF_IN_LOG="$ADMIN_CSRF"; fi
fi
expect_eq "Web 日志不含 CSRF 令牌" "" "$CSRF_IN_LOG"
expect_not_contains "代理日志不含登录口令" "$PASSWORD" "$(cat "$AGENT_LOG")"

echo
printf '通过 %d 项，失败 %d 项\n' "$PASSED" "$FAILED"
if [ "$FAILED" -gt 0 ]; then
  printf '失败项:%s\n' "$FAILED_NAMES"
  exit 1
fi
exit 0
