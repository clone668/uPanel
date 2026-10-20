#!/bin/sh
# P03.2 Linux 验收脚本：持久 jobs、幂等键、稳定 operation_id、代理执行记录、资源锁、排队与取消。
#
# 运行位置：Ubuntu 22.04/24.04 验收 VM 内，**必须以 root 运行**。
#   - 代理只能在 root 下启动，并按 SO_PEERCRED 区分 Web 账号；
#   - Web 必须以 upanel-web 身份启动（连不上 socket 就是边界本身的行为，不是测试麻烦）；
#   - 不依赖 Go 工具链：二进制由宿主交叉编译，脚本只做黑盒验收与查库。
#
# 用法：
#   sudo BIN_DIR=/tmp/upanel-bin sh tests/integration/p03-2-jobs-acceptance.sh
#
# 退出码：0 全部通过；1 有失败项；2 环境不满足（非 root）。
#
# 覆盖的验收点（STAGES P03.2、CONTRACTS §4.2/§4.3/§14.1/§14.2、PLAN §6）：
#   1. 代理以 root 启动（验收动作表）；Web 以非 root 运行；两端库各自记录同一 operation_id
#   2. POST /jobs 需要 Idempotency-Key 与 CSRF；202 返回 job_id 与 events_url
#   3. 同一次请求提交十次 → 代理只执行一次（jobs / operations / idempotency_keys 各 1 行）
#   4. 相同幂等键不同请求 → 409 IDEMPOTENCY_CONFLICT
#   5. 预检一次性：换键复用同一预检 → 409 PREFLIGHT_STALE
#   6. 预检过期 → 409 PREFLIGHT_EXPIRED，且不落 job、不惊动代理
#   7. 缺少再认证 → 403 REAUTH_REQUIRED；短语不符 → 400 CONFIRMATION_MISMATCH，且 job 记为 failed
#   8. Web 在提交后退出：任务继续在代理侧执行；Web 恢复后按 job_id 仍能找到同一个 operation
#   9. 代理被 kill -9：操作标为 interrupted，重启后**不自动重跑**
#  10. 不可取消的动作（acceptance.probe）不显示可取消，取消返回 409
#  11. 可取消的动作（acceptance.ping）取消返回 202，状态回写 canceled
#  12. 权限：未登录 401、viewer 提交 403、缺 CSRF 403；被拒绝的请求不落库
#  13. 进度未知时为 null（不是 0），终态不可回到 running

set -u

BIN_DIR=${BIN_DIR:-/tmp/upanel-bin}
if [ "$(id -u)" -ne 0 ]; then
  printf '本脚本必须以 root 运行：sudo BIN_DIR=%s sh %s\n' "$BIN_DIR" "$0" >&2
  exit 2
fi

AGENT="$BIN_DIR/upanel-agent"
WEB="$BIN_DIR/upanel-web"
CTL="$BIN_DIR/upanelctl"

WORK=${WORK:-/var/tmp/upanel-accept-p03-2}
RUN_DIR=/run/upanel
SOCK=$RUN_DIR/agent.sock
SYS_CFG=/etc/upanel/upanel.json
AGENT_DB_DIR=/var/lib/upanel-agent
AGENT_DB=$AGENT_DB_DIR/agent.db
KEY_FILE=/etc/upanel/agent.key
WEB_DB_DIR=$WORK/webdata
WEB_DB=$WEB_DB_DIR/upanel.db
PORT=19444
BASE="http://127.0.0.1:$PORT"
ORIGIN="$BASE"
WEB_USER=upanel-web
PASSWORD='vm-correct-horse-9'
VIEWER_PASSWORD='vm-viewer-horse-42'

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

# dbexec <数据库文件> <SQL>：写操作以 Web 账号身份执行，避免把文件属主改成 root。
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

# wait_for_stage <数据库> <表> <where> <stage> <超时秒>：轮询直到 stage 命中。
wait_for_stage() {
  _db=$1; _where=$2; _want=$3; _secs=${4:-5}
  _n=0
  while [ "$_n" -lt "$((_secs * 10))" ]; do
    _cur=$(dbq "$_db" "SELECT stage FROM operations WHERE $_where")
    [ "$_cur" = "$_want" ] && return 0
    sleep 0.1
    _n=$((_n + 1))
  done
  return 1
}

cleanup() {
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

PREFLIGHT_PATH=/api/v1/operations/preflight
JOBS_PATH=/api/v1/jobs

echo "== 0. 环境"
if [ "$(id -u)" -eq 0 ]; then pass "验收脚本以 root 运行"; else fail "验收脚本以 root 运行" "当前 UID $(id -u)"; fi
for b in "$AGENT" "$WEB" "$CTL"; do
  if [ -x "$b" ]; then pass "二进制存在且可执行 $(basename "$b")"; else fail "二进制存在且可执行 $(basename "$b")" "$b"; fi
done
if command -v python3 >/dev/null 2>&1; then pass "python3 可用于查库"; else fail "python3 可用于查库" "缺少 python3"; fi

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
# 代理库每轮从空库开始：上一轮的 operations 会让“只执行了一次”的计数失去意义。
rm -f "$AGENT_DB" "$AGENT_DB-wal" "$AGENT_DB-shm"
install -d -m 0755 "$RUN_DIR"
chmod 0755 "$RUN_DIR"
chown root:root "$RUN_DIR"

cat >"$SYS_CFG" <<'JSON'
{
  "web": {
    "mode": "production",
    "listen_address": "127.0.0.1:19444",
    "shutdown_timeout_seconds": 10
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
    "path": "/var/tmp/upanel-accept-p03-2/webdata/upanel.db",
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
expect_contains "代理已启用验收动作表" "已启用验收专用动作表" "$(cat "$AGENT_LOG")"
expect_contains "代理日志报告操作超时" '"operation_timeout_seconds":600' "$(cat "$AGENT_LOG")"

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
# 登录失败时后面的断言全都是连锁失败，没有诊断价值；直接收工并如实报错。
# 注意退出码必须是 1（有失败项），不能是 2（环境不满足）——登录 400/401 是脚本或产品的问题，
# 不是“非 root”这种环境前置条件不满足。
if [ "$code" != "200" ]; then
  fail "登录失败，后续断言无法进行" "$(cat "$WORK/login-admin.json" 2>/dev/null)"
  printf '通过 %d 项，失败 %d 项\n' "$PASSED" "$FAILED"
  exit 1
fi
ADMIN_CSRF=$(csrf_of "$WORK/login-admin.json")

# 预检辅助：生成一份 acceptance.probe 预检，回显 preflight_id 与短语到全局变量。
new_probe_preflight() {
  code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "$PREFLIGHT_PATH" '{"action":"acceptance.probe","params":{"label":"alpha"}}')
  [ "$code" = "200" ] || { fail "生成 probe 预检" "HTTP $code"; return 1; }
  PF_ID=$(jget "$WORK/resp.json" 'd["data"]["preflight_id"]')
  PF_PHRASE=$(jget "$WORK/resp.json" 'd["data"]["confirmation_phrase"]')
}

new_ping_preflight() {
  code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "$PREFLIGHT_PATH" '{"action":"acceptance.ping","params":{}}')
  [ "$code" = "200" ] || { fail "生成 ping 预检" "HTTP $code"; return 1; }
  PF_ID=$(jget "$WORK/resp.json" 'd["data"]["preflight_id"]')
  PF_PHRASE=""
}

echo
echo "== 2. 再认证与提交主流程 =="
code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST /api/v1/auth/reauth "{\"password\":\"$PASSWORD\"}")
expect_eq "密码再认证 200" "200" "$code"

new_probe_preflight || true
PROBE_PF=${PF_ID:-}
PROBE_PHRASE=${PF_PHRASE:-}
if [ -z "$PROBE_PF" ]; then
  fail "probe 预检没拿到 preflight_id，主流程无法继续"
  printf '通过 %d 项，失败 %d 项\n' "$PASSED" "$FAILED"
  exit 1
fi
note "probe 预检 $PROBE_PF"

# 缺幂等键：直接拒绝，且不落库。
code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "$JOBS_PATH" "{\"preflight_id\":\"$PROBE_PF\",\"confirmation_phrase\":\"$PROBE_PHRASE\"}")
expect_eq "缺 Idempotency-Key 返回 400" "400" "$code"
expect_contains "缺幂等键的错误码" "VALIDATION_ERROR" "$(cat "$WORK/resp.json")"
expect_eq "缺幂等键没有留下任务" "0" "$(dbq "$WEB_DB" "SELECT COUNT(*) FROM jobs")"

code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "$JOBS_PATH" \
  "{\"preflight_id\":\"$PROBE_PF\",\"confirmation_phrase\":\"$PROBE_PHRASE\"}" "$WORK/key-1")
expect_eq "提交任务返回 202" "202" "$code"
cp "$WORK/resp.json" "$WORK/job-submit.json"
JOB_ID=$(jget "$WORK/job-submit.json" 'd["data"]["job_id"]')
case $JOB_ID in
  job_*) pass "job_id 形态为 job_<hex>" ;;
  *) fail "job_id 形态" "$JOB_ID" ;;
esac
expect_eq "提交后状态是 queued" "queued" "$(jget "$WORK/job-submit.json" 'd["data"]["status"]')"
expect_eq "events_url 指向该任务" "/api/v1/jobs/$JOB_ID/events" "$(jget "$WORK/job-submit.json" 'd["data"]["events_url"]')"

OPERATION_ID=$(dbq "$WEB_DB" "SELECT operation_id FROM jobs WHERE id = '$JOB_ID'")
case $OPERATION_ID in
  op_*) pass "Web 库记录了稳定 operation_id" ;;
  *) fail "operation_id 形态" "$OPERATION_ID" ;;
esac
expect_eq "代理库有同一个 operation_id" "$OPERATION_ID" \
  "$(dbq "$AGENT_DB" "SELECT operation_id FROM operations WHERE operation_id = '$OPERATION_ID'")"
expect_eq "Web 库记录了消费它的 job" "$JOB_ID" \
  "$(dbq "$WEB_DB" "SELECT consumed_job_id FROM preflights WHERE id = '$PROBE_PF'")"
expect_eq "幂等记录保留期 ≥ 24 小时" "1" \
  "$(dbq "$WEB_DB" "SELECT (julianday(expires_at) - julianday(created_at)) >= 1.0 FROM idempotency_keys WHERE job_id = '$JOB_ID'")"
expect_eq "jobs.needs_sync 已清除" "0" "$(dbq "$WEB_DB" "SELECT needs_sync FROM jobs WHERE id = '$JOB_ID'")"

if wait_for_stage "$AGENT_DB" "operation_id = '$OPERATION_ID'" succeeded 5; then
  pass "probe 操作在代理侧已核验为 succeeded"
else
  fail "probe 操作终态" "stage=$(dbq "$AGENT_DB" "SELECT stage FROM operations WHERE operation_id = '$OPERATION_ID'")"
fi
code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" GET "$JOBS_PATH/$JOB_ID")
expect_eq "读回任务 200" "200" "$code"
cp "$WORK/resp.json" "$WORK/job-read.json"
expect_eq "任务状态 succeeded" "succeeded" "$(jget "$WORK/job-read.json" 'd["data"]["status"]')"
expect_eq "进度未知时为 null" "None" "$(jget "$WORK/job-read.json" 'd["data"]["progress_percent"]')"
expect_contains "结果含 outcome" "verified" "$(jget "$WORK/job-read.json" 'json.dumps(d["data"]["result"])')"
if [ -n "$(jget "$WORK/job-read.json" 'd["data"]["finished_at"]')" ]; then
  pass "终态任务带 finished_at"
else
  fail "终态任务带 finished_at" "null"
fi

echo
echo "== 3. 同一次请求提交十次只执行一次 =="
new_probe_preflight || true
TEN_PF=${PF_ID:-}
TEN_PHRASE=${PF_PHRASE:-}
TEN_BODY="{\"preflight_id\":\"$TEN_PF\",\"confirmation_phrase\":\"$TEN_PHRASE\"}"
TEN_ID=""
i=0
while [ "$i" -lt 10 ]; do
  code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "$JOBS_PATH" "$TEN_BODY" "repeat-key")
  if [ "$code" != "202" ]; then
    fail "第 $((i + 1)) 次提交返回 202" "HTTP $code: $(cat "$WORK/resp.json")"
    break
  fi
  ONE_ID=$(jget "$WORK/resp.json" 'd["data"]["job_id"]')
  if [ -z "$TEN_ID" ]; then TEN_ID=$ONE_ID; fi
  if [ "$ONE_ID" != "$TEN_ID" ]; then
    fail "十次提交复用了同一个 job" "第 $((i + 1)) 次给了 $ONE_ID"
    break
  fi
  i=$((i + 1))
done
pass "十次提交都返回同一个 job_id（$TEN_ID）"
repeat_ops=$(dbq "$WEB_DB" "SELECT operation_id FROM jobs WHERE id = '$TEN_ID'")
expect_eq "十个请求只产生一条 jobs 记录" "1" "$(dbq "$WEB_DB" "SELECT COUNT(*) FROM jobs WHERE id = '$TEN_ID'")"
expect_eq "十个请求只产生一条幂等记录" "1" \
  "$(dbq "$WEB_DB" "SELECT COUNT(*) FROM idempotency_keys WHERE job_id = '$TEN_ID'")"
expect_eq "代理库只有一条对应操作" "1" \
  "$(dbq "$AGENT_DB" "SELECT COUNT(*) FROM operations WHERE operation_id = '$repeat_ops'")"
wait_for_stage "$AGENT_DB" "operation_id = '$repeat_ops'" succeeded 5 >/dev/null 2>&1
expect_eq "代理执行记录只有一条 accepted 事件" "1" \
  "$(dbq "$AGENT_DB" "SELECT COUNT(*) FROM operation_events WHERE operation_id = '$repeat_ops' AND kind = 'accepted'")"

echo
echo "== 4. 幂等冲突与预检一次性 =="
code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "$JOBS_PATH" \
  "{\"preflight_id\":\"$TEN_PF\",\"confirmation_phrase\":\"WRONG PHRASE\"}" "repeat-key")
expect_eq "同键不同请求返回 409" "409" "$code"
expect_contains "同键不同请求的错误码" "IDEMPOTENCY_CONFLICT" "$(cat "$WORK/resp.json")"

new_probe_preflight || true
code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "$JOBS_PATH" \
  "{\"preflight_id\":\"$PF_ID\",\"confirmation_phrase\":\"$PF_PHRASE\"}" "once-key-a")
expect_eq "首次使用预检返回 202" "202" "$code"
code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "$JOBS_PATH" \
  "{\"preflight_id\":\"$PF_ID\",\"confirmation_phrase\":\"$PF_PHRASE\"}" "once-key-b")
expect_eq "同一预检换键再提交返回 409" "409" "$code"
expect_contains "预检已被使用的错误码" "PREFLIGHT_STALE" "$(cat "$WORK/resp.json")"

echo
echo "== 5. 预检过期 =="
new_probe_preflight || true
EXP_PF=${PF_ID:-}
EXP_PHRASE=${PF_PHRASE:-}
# 把到期时间改到过去：等价于管理员在预检页上停满了 120 秒。
PAST=$(python3 -c 'import datetime; print((datetime.datetime.now(datetime.timezone.utc) - datetime.timedelta(seconds=5)).strftime("%Y-%m-%dT%H:%M:%SZ"))')
dbexec "$WEB_DB" "UPDATE preflights SET expires_at = '$PAST' WHERE id = '$EXP_PF'"
before_jobs=$(dbq "$WEB_DB" "SELECT COUNT(*) FROM jobs")
code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "$JOBS_PATH" \
  "{\"preflight_id\":\"$EXP_PF\",\"confirmation_phrase\":\"$EXP_PHRASE\"}" "expired-key")
expect_eq "过期预检返回 409" "409" "$code"
expect_contains "过期预检的错误码" "PREFLIGHT_EXPIRED" "$(cat "$WORK/resp.json")"
expect_eq "过期预检没有落 job" "$before_jobs" "$(dbq "$WEB_DB" "SELECT COUNT(*) FROM jobs")"

echo
echo "== 6. 再认证缺失与短语不符 =="
# 重新登录拿一个没有再认证记录的会话：预检可以生成，提交必须被拒。
code=$(login admin "$PASSWORD" "$WORK/fresh.jar" login-fresh)
expect_eq "第二次登录 200" "200" "$code"
FRESH_CSRF=$(csrf_of "$WORK/login-fresh.json")
code=$(api "$WORK/fresh.jar" "$FRESH_CSRF" POST "$PREFLIGHT_PATH" \
  '{"action":"acceptance.probe","params":{"label":"alpha"}}')
expect_eq "新会话生成预检 200" "200" "$code"
FRESH_PF=$(jget "$WORK/resp.json" 'd["data"]["preflight_id"]')
FRESH_PHRASE=$(jget "$WORK/resp.json" 'd["data"]["confirmation_phrase"]')
before_jobs=$(dbq "$WEB_DB" "SELECT COUNT(*) FROM jobs")
code=$(api "$WORK/fresh.jar" "$FRESH_CSRF" POST "$JOBS_PATH" \
  "{\"preflight_id\":\"$FRESH_PF\",\"confirmation_phrase\":\"$FRESH_PHRASE\"}" "no-reauth-key")
expect_eq "未再认证提交返回 403" "403" "$code"
expect_contains "未再认证的错误码" "REAUTH_REQUIRED" "$(cat "$WORK/resp.json")"
expect_eq "未再认证没有落 job" "$before_jobs" "$(dbq "$WEB_DB" "SELECT COUNT(*) FROM jobs")"

new_probe_preflight || true
WRONG_PF=${PF_ID:-}
before_jobs=$(dbq "$WEB_DB" "SELECT COUNT(*) FROM jobs")
code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "$JOBS_PATH" \
  "{\"preflight_id\":\"$WRONG_PF\",\"confirmation_phrase\":\"PROBE ZZZZZZ\"}" "wrong-phrase-key")
expect_eq "短语不符返回 400" "400" "$code"
expect_contains "短语不符的错误码" "CONFIRMATION_MISMATCH" "$(cat "$WORK/resp.json")"
expect_eq "短语不符留下一条 failed 任务（拒绝也要留痕）" "failed" \
  "$(dbq "$WEB_DB" "SELECT status FROM jobs ORDER BY created_at DESC LIMIT 1")"

echo
echo "== 7. Web 退出后任务继续执行，恢复后仍能找到原 operation =="
new_ping_preflight || true
DOWN_PF=${PF_ID:-}
code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "$JOBS_PATH" "{\"preflight_id\":\"$DOWN_PF\"}" "down-key")
expect_eq "ping 任务提交 202" "202" "$code"
DOWN_JOB=$(jget "$WORK/resp.json" 'd["data"]["job_id"]')
DOWN_OP=$(dbq "$WEB_DB" "SELECT operation_id FROM jobs WHERE id = '$DOWN_JOB'")
if stop_web; then pass "Web 在任务执行期间退出"; else fail "Web 停止" "仍在监听"; fi
if wait_for_stage "$AGENT_DB" "operation_id = '$DOWN_OP'" succeeded 8; then
  pass "Web 不在时任务仍然执行完成（执行不依赖 HTTP 连接）"
else
  fail "Web 退出后任务继续执行" "stage=$(dbq "$AGENT_DB" "SELECT stage FROM operations WHERE operation_id = '$DOWN_OP'")"
fi
if start_web; then pass "Web 重新启动"; else fail "Web 重新启动" "$(tail -5 "$WEB_LOG" 2>/dev/null)"; fi
code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" GET "$JOBS_PATH/$DOWN_JOB")
expect_eq "Web 恢复后仍能按 job_id 读到任务" "200" "$code"
expect_eq "任务状态已同步为 succeeded" "succeeded" "$(jget "$WORK/resp.json" 'd["data"]["status"]')"
expect_eq "读到的仍是同一个 operation" "$DOWN_OP" \
  "$(dbq "$WEB_DB" "SELECT operation_id FROM jobs WHERE id = '$DOWN_JOB'")"

echo
echo "== 8. 代理崩溃：标 interrupted 且不自动重跑 =="
new_ping_preflight || true
CRASH_PF=${PF_ID:-}
code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "$JOBS_PATH" "{\"preflight_id\":\"$CRASH_PF\"}" "crash-key")
expect_eq "崩溃前提交 202" "202" "$code"
CRASH_JOB=$(jget "$WORK/resp.json" 'd["data"]["job_id"]')
CRASH_OP=$(dbq "$WEB_DB" "SELECT operation_id FROM jobs WHERE id = '$CRASH_JOB'")
before_ops=$(dbq "$AGENT_DB" "SELECT COUNT(*) FROM operations")
if kill -9 "$AGENT_PID" 2>/dev/null; then pass "代理被 kill -9"; else fail "代理被 kill -9" "无法发送信号"; fi
wait "$AGENT_PID" 2>/dev/null
AGENT_PID=""
rm -f "$SOCK"
sleep 0.5
if start_agent; then pass "代理重启"; else fail "代理重启" "$(tail -5 "$AGENT_LOG" 2>/dev/null)"; fi
expect_eq "崩溃中的操作被标为 interrupted" "interrupted" \
  "$(dbq "$AGENT_DB" "SELECT stage FROM operations WHERE operation_id = '$CRASH_OP'")"
expect_eq "崩溃操作有结束时间" "1" \
  "$(dbq "$AGENT_DB" "SELECT finished_at <> '' FROM operations WHERE operation_id = '$CRASH_OP'")"
expect_eq "没有自动重跑（操作行数不变）" "$before_ops" "$(dbq "$AGENT_DB" "SELECT COUNT(*) FROM operations")"
expect_eq "崩溃操作只有一条 accepted 事件" "1" \
  "$(dbq "$AGENT_DB" "SELECT COUNT(*) FROM operation_events WHERE operation_id = '$CRASH_OP' AND kind = 'accepted'")"
code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" GET "$JOBS_PATH/$CRASH_JOB")
expect_eq "读回崩溃任务 200" "200" "$code"
expect_eq "崩溃任务状态是 interrupted" "interrupted" "$(jget "$WORK/resp.json" 'd["data"]["status"]')"

echo
echo "== 9. 取消规则 =="
new_probe_preflight || true
NC_PF=${PF_ID:-}
code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "$JOBS_PATH" \
  "{\"preflight_id\":\"$NC_PF\",\"confirmation_phrase\":\"$PF_PHRASE\"}" "nocancel-key")
expect_eq "probe 任务提交 202" "202" "$code"
NC_JOB=$(jget "$WORK/resp.json" 'd["data"]["job_id"]')
code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" GET "$JOBS_PATH/$NC_JOB")
expect_eq "不可取消动作的 cancellable 为 false" "False" "$(jget "$WORK/resp.json" 'd["data"]["cancellable"]')"
code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "$JOBS_PATH/$NC_JOB/cancel")
expect_eq "取消不可取消的任务返回 409" "409" "$code"
# 两种 409 都是真话：任务还在跑 → 策略不允许（VALIDATION_ERROR）；
# 100ms 的 probe 已经跑完 → 终态不可变更（VERSION_CONFLICT）。
case $(cat "$WORK/resp.json") in
  *VALIDATION_ERROR*) pass "取消被拒：动作不可取消（VALIDATION_ERROR）" ;;
  *VERSION_CONFLICT*) pass "取消被拒：任务已进入终态（VERSION_CONFLICT）" ;;
  *) fail "不可取消的错误码" "$(cat "$WORK/resp.json")" ;;
esac

new_ping_preflight || true
C_PF=${PF_ID:-}
code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "$JOBS_PATH" "{\"preflight_id\":\"$C_PF\"}" "cancel-key")
expect_eq "ping 任务提交 202" "202" "$code"
C_JOB=$(jget "$WORK/resp.json" 'd["data"]["job_id"]')
code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" GET "$JOBS_PATH/$C_JOB")
expect_eq "可取消动作的 cancellable 为 true" "True" "$(jget "$WORK/resp.json" 'd["data"]["cancellable"]')"
code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "$JOBS_PATH/$C_JOB/cancel")
expect_eq "取消可取消的任务返回 202" "202" "$code"
C_STATUS=$(jget "$WORK/resp.json" 'd["data"]["status"]')
case $C_STATUS in
  canceled|running) pass "取消后状态为 $C_STATUS（请求已记录）" ;;
  *) fail "取消后状态" "$C_STATUS" ;;
esac
if [ "$C_STATUS" = "running" ]; then
  # 取消请求已记录但执行器还在收尾：等它落到终态。
  n=0
  while [ "$n" -lt 50 ]; do
    code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" GET "$JOBS_PATH/$C_JOB")
    C_STATUS=$(jget "$WORK/resp.json" 'd["data"]["status"]')
    [ "$C_STATUS" = "canceled" ] && break
    sleep 0.1
    n=$((n + 1))
  done
fi
expect_eq "ping 任务最终落在 canceled" "canceled" "$C_STATUS"
expect_eq "cancellation_requested 已置位" "True" "$(jget "$WORK/resp.json" 'd["data"]["cancellation_requested"]')"

echo
echo "== 10. 权限与请求形状 =="
anon_code=$(curl -s -o "$WORK/anon.json" -w '%{http_code}' -X POST "$BASE$JOBS_PATH" \
  -H 'Content-Type: application/json' -H "Origin: $ORIGIN" -H 'Idempotency-Key: anon' -d '{}')
expect_eq "未登录提交返回 401" "401" "$anon_code"

code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST /api/v1/users \
  "{\"username\":\"viewer1\",\"display_name\":\"Viewer\",\"role\":\"viewer\",\"initial_password\":\"$VIEWER_PASSWORD\"}")
expect_eq "创建 viewer 返回 201" "201" "$code"
code=$(login viewer1 "$VIEWER_PASSWORD" "$WORK/viewer.jar" login-viewer)
expect_eq "viewer 登录 200" "200" "$code"
VIEWER_CSRF=$(csrf_of "$WORK/login-viewer.json")
code=$(api "$WORK/viewer.jar" "$VIEWER_CSRF" POST "$JOBS_PATH" '{}' 'viewer-key')
expect_eq "viewer 提交任务返回 403" "403" "$code"
expect_contains "viewer 的错误码" "FORBIDDEN" "$(cat "$WORK/resp.json")"
code=$(api "$WORK/viewer.jar" "$VIEWER_CSRF" GET "$JOBS_PATH")
expect_eq "viewer 读任务列表 200（只含自己的）" "200" "$code"
expect_eq "viewer 的列表里没有别人的任务" "0" "$(jget "$WORK/resp.json" 'len(d["data"]["jobs"])')"

csrf_missing=$(curl -s -b "$WORK/admin.jar" -o "$WORK/nocsrf.json" -w '%{http_code}' -X POST "$BASE$JOBS_PATH" \
  -H 'Content-Type: application/json' -H "Origin: $ORIGIN" -H 'Idempotency-Key: k' \
  -d "{\"preflight_id\":\"$PROBE_PF\",\"confirmation_phrase\":\"x\"}")
expect_eq "缺 CSRF 提交返回 403" "403" "$csrf_missing"
expect_contains "缺 CSRF 的错误码" "CSRF_FAILED" "$(cat "$WORK/nocsrf.json")"

new_probe_preflight || true
code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "$JOBS_PATH" \
  "{\"preflight_id\":\"$PF_ID\",\"action\":\"storage.filesystem.format\"}" "extra-key")
expect_eq "夹带 action 字段返回 400" "400" "$code"
expect_contains "夹带字段的错误码" "VALIDATION_ERROR" "$(cat "$WORK/resp.json")"

echo
echo "== 11. 终态不可回退与日志卫生 =="
expect_eq "jobs 终态触发器存在" "1" \
  "$(dbq "$WEB_DB" "SELECT COUNT(*) FROM sqlite_master WHERE type = 'trigger' AND name = 'jobs_terminal_is_final'")"
expect_eq "job_events 只追加触发器存在" "2" \
  "$(dbq "$WEB_DB" "SELECT COUNT(*) FROM sqlite_master WHERE type = 'trigger' AND name LIKE 'job_events_append_only%'")"
expect_eq "operations 终态触发器存在" "1" \
  "$(dbq "$AGENT_DB" "SELECT COUNT(*) FROM sqlite_master WHERE type = 'trigger' AND name = 'operations_terminal_is_final'")"
expect_not_contains "Web 日志不含确认短语明文" "$PROBE_PHRASE" "$(cat "$WEB_LOG")"
expect_contains "Web 日志记录了操作提交" "已提交操作" "$(cat "$WEB_LOG")"
expect_not_contains "代理日志不含确认短语明文" "$PROBE_PHRASE" "$(cat "$AGENT_LOG")"
expect_eq "Web 库不含确认短语列" "" \
  "$(dbq "$WEB_DB" "SELECT group_concat(name, ',') FROM pragma_table_info('jobs') WHERE name LIKE '%phrase%'")"

echo
echo "== 12. 汇总 =="
printf '通过 %d 项，失败 %d 项\n' "$PASSED" "$FAILED"
if [ "$FAILED" -ne 0 ]; then
  printf '失败项:%s\n' "$FAILED_NAMES" >&2
  exit 1
fi
printf 'P03.2 验收全部通过\n'
exit 0
