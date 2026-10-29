#!/bin/sh
# P12.4：受管理 timer、时区、错过执行、不重叠、超时与受限脚本的真机验收。
#
# 验的东西只有一类：**这份定义到底会不会在没有人看着的时候按它说的做事**。
# 所以不验"接口返回 201"，而验 systemd 里的单元、systemctl 的原话、脚本实际以谁的身份
# 跑、以及触发之后落到面板任务表里那一行。
#
# 前置：同一台一次性 Ubuntu VM 已跑过 p12-2-backup-acceptance.sh（WORK 未删，里面有
#       管理员账号、面板库、代理库与数据根），二进制在 BIN_DIR（agent/web/upanelctl）。
# 用法：sudo BIN_DIR=/var/tmp/upanel-bin sh tests/integration/p12-4-schedule-acceptance.sh
# 退出码：0 全部通过；1 有失败；2 环境/前置证据不满足。
set -u

BIN_DIR=${BIN_DIR:-/var/tmp/upanel-bin}
PREV=${P12_2_WORK:-/var/tmp/upanel-accept-p12-2}
WORK=${WORK:-/var/tmp/upanel-accept-p12-4}
PORT=${PORT:-19524}
AGENT="$BIN_DIR/upanel-agent"
WEB="$BIN_DIR/upanel-web"
CTL="$BIN_DIR/upanelctl"
SYS_CFG=/etc/upanel/upanel.json
KEY_FILE=/etc/upanel/agent.key
SOCK=/run/upanel/agent.sock
WEB_USER=upanel-web
PASSWORD='p12-2-admin-horse-9'
VIEWER_PASSWORD='p12-4-viewer-pass-1'
ROOT_PATH=$PREV/data-root
AGENT_DB=$PREV/agent/agent.db
WEB_DB_DIR=$PREV/webdata
WEB_DB=$WEB_DB_DIR/upanel.db
UNIT_DIR=/etc/systemd/system
SCRIPT_DIR=/var/lib/upanel-agent/scripts
BASE="http://127.0.0.1:$PORT"
ORIGIN=$BASE
TMPD=/tmp/p12-4.$$
SCRIPT_DIR_SRC=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
SESSION_HASH=$(openssl rand -hex 32)
SUFFIX=$(openssl rand -hex 4)
JSON_TAB=$(printf '\t')
JSON_SEQ=0
JSON_IN=$TMPD/json.in
JSON_OUT=$TMPD/json.out
JSON_PID=""
PASSED=0
FAILED=0
NOTRUN=0
FAILED_NAMES=""
AGENT_PID=""
WEB_PID=""
SCH=""
SCH_UTC=""
SCH_TIMEOUT=""
SCRIPT_PROBE=""
SCRIPT_SLEEP=""
VIEWER_ID=""
KEEP_AGENT=1

pass() { PASSED=$((PASSED + 1)); printf 'PASS  %s\n' "$1"; printf 'PASS\t%s\n' "$1" >>"$WORK/logs/results.txt"; }
fail() { FAILED=$((FAILED + 1)); FAILED_NAMES="$FAILED_NAMES | $1"; printf 'FAIL  %s: %s\n' "$1" "${2:-}"; printf 'FAIL\t%s\t%s\n' "$1" "${2:-}" >>"$WORK/logs/results.txt"; }
notrun() { NOTRUN=$((NOTRUN + 1)); printf 'NOT RUN  %s：%s\n' "$1" "$2"; printf 'NOTRUN\t%s\t%s\n' "$1" "$2" >>"$WORK/logs/results.txt"; }
note() { printf '      %s\n' "$1"; }
expect_eq() { if [ "$2" = "$3" ]; then pass "$1"; else fail "$1" "期望 '$2'，实际 '$3'"; fi; }
expect_ne() { if [ "$2" != "$3" ]; then pass "$1"; else fail "$1" "不应等于 '$2'"; fi; }
expect_contains() { case "$3" in *"$2"*) pass "$1" ;; *) fail "$1" "期望包含 '$2'，实际 '$3'" ;; esac; }
expect_not_contains() { case "$3" in *"$2"*) fail "$1" "不应包含 '$2'，实际 '$3'" ;; *) pass "$1" ;; esac; }
expect_ge() { if [ "$2" -ge "$3" ] 2>/dev/null; then pass "$1"; else fail "$1" "期望 >= $3，实际 '$2'"; fi; }
expect_le() { if [ "$2" -le "$3" ] 2>/dev/null; then pass "$1"; else fail "$1" "期望 <= $3，实际 '$2'"; fi; }
file_sha() { sha256sum "$1" 2>/dev/null | awk '{print $1}'; }

cleanup() {
  if [ -n "${JSON_PID:-}" ] && kill -0 "$JSON_PID" 2>/dev/null; then kill -TERM "$JSON_PID" 2>/dev/null; fi
  for _id in "$SCH" "$SCH_UTC" "$SCH_TIMEOUT"; do
    [ -n "$_id" ] || continue
    systemctl stop "upanel-$_id.timer" >/dev/null 2>&1
    rm -f "$UNIT_DIR/upanel-$_id.timer" "$UNIT_DIR/upanel-$_id.service"
  done
  systemctl stop 'upanel-sch_*.timer' >/dev/null 2>&1
  systemctl daemon-reload >/dev/null 2>&1
  if [ -n "${WEB_PID:-}" ] && kill -0 "$WEB_PID" 2>/dev/null; then kill -TERM "$WEB_PID" 2>/dev/null; wait "$WEB_PID" 2>/dev/null; fi
  if [ "$KEEP_AGENT" -eq 1 ] && [ -n "${AGENT_PID:-}" ] && kill -0 "$AGENT_PID" 2>/dev/null; then
    kill -TERM "$AGENT_PID" 2>/dev/null; wait "$AGENT_PID" 2>/dev/null
  fi
  rm -f /etc/p12-4-should-fail
  rm -rf "$TMPD"
  return 0
}
trap cleanup EXIT
trap 'cleanup; exit 130' INT
trap 'cleanup; exit 143' TERM

if [ "$(id -u)" -ne 0 ]; then echo '必须以 root 运行' >&2; exit 2; fi
for f in "$AGENT" "$WEB" "$CTL" "$PREV/logs/results.txt" "$PREV/payloads/policy-create.json" "$SCRIPT_DIR_SRC/json_helper.py"; do
  if [ ! -f "$f" ]; then echo "缺少前置文件：$f" >&2; exit 2; fi
done
if grep -q '^FAIL' "$PREV/logs/results.txt"; then echo 'P12.2 前置结果含 FAIL，先修复前置阶段' >&2; exit 2; fi
if pgrep -x upanel-agent >/dev/null 2>&1 || pgrep -x upanel-web >/dev/null 2>&1; then echo '已有 upanel 进程在运行' >&2; exit 2; fi
if ! command -v systemd-analyze >/dev/null 2>&1; then echo '缺少 systemd-analyze：本脚本无法验日历，拒绝降级' >&2; exit 2; fi

rm -rf "$WORK"
mkdir -p "$WORK/logs" "$WORK/payloads" "$TMPD" "$WORK/probe"
chmod 1777 "$TMPD"
# 探针目录必须对 upanel-shell 可写：探针的作用是"以它的身份写点东西下来"，而
# 根建的目录默认 0755，脚本里的重定向会静默失败——退出码还是 0（最后一句 echo 成功），
# 三条身份断言拿到空文件。P12.4 第一轮 24.04 就是这么红的（`script-run-identity` 空）。
chmod 1777 "$WORK/probe"
RESULTS=$WORK/logs/results.txt
PAYLOADS=$WORK/payloads
: >"$RESULTS"

mkfifo "$JSON_IN" "$JSON_OUT"
python3 "$SCRIPT_DIR_SRC/json_helper.py" <"$JSON_IN" >"$JSON_OUT" 2>"$TMPD/json_helper.log" &
JSON_PID=$!
exec 7>"$JSON_IN" 8<"$JSON_OUT"

jget() {
  JSON_SEQ=$((JSON_SEQ + 1)); _seq=$JSON_SEQ
  printf '%s\tget\t%s\t%s\n' "$_seq" "$1" "$2" >&7
  IFS=$JSON_TAB read -r _got _val <&8
  if [ "${_got:-}" != "$_seq" ]; then printf 'ERR:helper-mismatch'; return 0; fi
  printf '%s' "$_val"
}
api() {
  _method=$1; _path=$2; _body=${3:-}; _keep=${4:-}
  if [ -n "$_body" ]; then
    printf '%s' "$_body" >"$WORK/req.json"
    _code=$(curl -s -b "$JAR" -c "$JAR" -o "$WORK/resp.json" -w '%{http_code}' -X "$_method" "$BASE$_path" -H 'Content-Type: application/json' -H "Origin: $ORIGIN" -H "X-CSRF-Token: $CSRF" -d @"$WORK/req.json")
  else
    _code=$(curl -s -b "$JAR" -c "$JAR" -o "$WORK/resp.json" -w '%{http_code}' -X "$_method" "$BASE$_path" -H "Origin: $ORIGIN" -H "X-CSRF-Token: $CSRF")
  fi
  if [ -n "$_keep" ]; then cp "$WORK/resp.json" "$PAYLOADS/$_keep.json" 2>/dev/null; fi
  printf '%s' "$_code"
}
rget() { jget "$WORK/resp.json" "$1"; }
reauth() {
  _code=$(printf '{"password":"%s"}' "$PASSWORD" | curl -s -b "$JAR" -c "$JAR" -o "$WORK/resp.json" -w '%{http_code}' -X POST "$BASE/api/v1/auth/reauth" -H 'Content-Type: application/json' -H "Origin: $ORIGIN" -H "X-CSRF-Token: $CSRF" -d @-)
  if [ -n "${1:-}" ]; then cp "$WORK/resp.json" "$PAYLOADS/$1.json" 2>/dev/null; fi
  printf '%s' "$_code"
}
preflight() {
  printf '{"action":"%s","params":%s}' "$1" "$2" >"$WORK/pre.json"
  _code=$(curl -s -b "$JAR" -c "$JAR" -o "$WORK/resp.json" -w '%{http_code}' -X POST "$BASE/api/v1/operations/preflight" -H 'Content-Type: application/json' -H "Origin: $ORIGIN" -H "X-CSRF-Token: $CSRF" -d @"$WORK/pre.json")
  if [ -n "${3:-}" ]; then cp "$WORK/resp.json" "$PAYLOADS/preflight-$3.json" 2>/dev/null; fi
  cp "$WORK/resp.json" "$WORK/preflight.json"
  printf '%s' "$_code"
}
pget() { jget "$WORK/preflight.json" "$1"; }
pblockers() { pget 'json.dumps([b["code"] for b in (d["data"].get("blockers") or [])])'; }
submit_job() {
  printf '{"preflight_id":"%s","confirmation_phrase":"%s"}' "$1" "${3:-}" >"$WORK/jobreq.json"
  _code=$(curl -s -b "$JAR" -c "$JAR" -o "$WORK/resp.json" -w '%{http_code}' -X POST "$BASE/api/v1/jobs" -H 'Content-Type: application/json' -H "Origin: $ORIGIN" -H "X-CSRF-Token: $CSRF" -H "Idempotency-Key: p12-4-$2-$(openssl rand -hex 8)" -d @"$WORK/jobreq.json")
  cp "$WORK/resp.json" "$WORK/submit.json"
  cp "$WORK/resp.json" "$PAYLOADS/submit-$2.json" 2>/dev/null
  printf '%s' "$_code"
}
# resubmit：预检在"再认证 + 提交"之间过期时重做一次预检再提，理由与 P12.3 同。
PREF_CUR=""
resubmit() {
  _code=$(submit_job "$PREF_CUR" "$1" "${4:-}")
  if [ "$_code" = "409" ]; then
    _ecode=$(jget "$WORK/submit.json" 'd["error"]["code"]')
    case "$_ecode" in
      PREFLIGHT_EXPIRED|PREFLIGHT_STALE)
        cp "$WORK/submit.json" "$PAYLOADS/submit-$1-rejected.json" 2>/dev/null
        note "$1：提交时预检已失效（$_ecode），重做预检后重提。"
        expect_eq "$1-preflight-reissued" "200" "$(preflight "$2" "$3" "$1-reissue")"
        expect_eq "$1-preflight-reissued-allowed" "True" "$(pget 'd["data"]["allowed"]')"
        PREF_CUR=$(pget 'd["data"]["preflight_id"]')
        _p=$(pget 'd["data"].get("confirmation_phrase") or ""')
        if [ -n "$_p" ]; then _use=$_p; else _use=${4:-}; fi
        _code=$(submit_job "$PREF_CUR" "$1" "$_use")
        ;;
    esac
  fi
  printf '%s' "$_code"
}
job_id_of() { _v=$(jget "$1" 'd["data"]["job_id"]'); case "$_v" in ERR:*) printf '' ;; *) printf '%s' "$_v" ;; esac; }
wait_job() {
  if [ -z "${1:-}" ]; then printf 'no-job-id'; return 0; fi
  _i=0; _unauth=0
  while [ "$_i" -lt 2400 ]; do
    _code=$(api GET "/api/v1/jobs/$1")
    cp "$WORK/resp.json" "$WORK/job.json"
    if [ "$_code" = "401" ]; then
      _unauth=$((_unauth + 1))
      if [ "$_unauth" -ge 5 ]; then printf 'unauthenticated'; return 0; fi
    else
      _unauth=0
    fi
    _st=$(jget "$WORK/job.json" 'd["data"]["status"]')
    case "$_st" in succeeded|failed|canceled|interrupted) printf '%s' "$_st"; return 0 ;; esac
    _i=$((_i + 1)); sleep 0.2
  done
  printf 'timeout'
}
jobget() { jget "$WORK/job.json" "$1"; }
finish_job() {
  JOB_ID=$(job_id_of "$WORK/submit.json")
  JOB_STATE=$(wait_job "$JOB_ID")
  cp "$WORK/job.json" "$PAYLOADS/job-$1.json" 2>/dev/null
}
# defn 拼一条定义（字段名与 CONTRACTS 第 12 节一致）。
defn() {
  printf '{"name":"%s","kind":"%s","target_id":"%s","calendar_expression":"%s","timezone":"%s","persistent":%s,"max_runtime_seconds":%s,"enabled":%s}' \
    "$1" "$2" "$3" "$4" "$5" "$6" "$7" "$8"
}
create_params() { printf '{"version":0,"definition":%s}' "$1"; }
update_params() { printf '{"schedule_id":"%s","version":%s,"definition":%s}' "$1" "$2" "$3"; }
# apply_schedule 走完整链：再认证 → 预检 → 提交 → 等待终态，把 job 响应留在 job.json。
apply_schedule() {
  _name=$1; _params=$2
  expect_eq "$_name-reauth" "200" "$(reauth)"
  expect_eq "$_name-preflight-200" "200" "$(preflight schedule.apply "$_params" "$_name")"
  expect_eq "$_name-preflight-allowed" "True" "$(pget 'd["data"]["allowed"]')"
  PREF_CUR=$(pget 'd["data"]["preflight_id"]')
  expect_eq "$_name-submit-202" "202" "$(resubmit "$_name" schedule.apply "$_params")"
  finish_job "$_name"
  expect_eq "$_name-succeeded" "succeeded" "$JOB_STATE"
}
sched_field() { api GET "/api/v1/schedules/$1" >/dev/null; jget "$WORK/resp.json" "d[\"data\"][\"schedule\"][\"$2\"]"; }
sched_def() { api GET "/api/v1/schedules/$1" >/dev/null; jget "$WORK/resp.json" "d[\"data\"][\"schedule\"][\"definition\"][\"$2\"]"; }
sched_blockers() { api GET "/api/v1/schedules/$1" >/dev/null; jget "$WORK/resp.json" 'json.dumps([b["code"] for b in (d["data"]["schedule"].get("blockers") or [])])'; }
sched_triggers() { api GET "/api/v1/schedules/$1" >/dev/null; jget "$WORK/resp.json" 'json.dumps([(t["outcome"], t["trigger_id"]) for t in (d["data"]["triggers"] or [])])'; }
# next_elapse_of 读一条计划的下次触发时刻，最多等 N 秒；读不到就把最后一次的原始值
# 交回去（空 / None / ERR:…），由断言说清现场。
#
# 为什么要等：next_elapse 是代理现算的（systemd-analyze calendar），计划刚建好的一瞬
# 可能还读不到；它读不到时字段是 JSON null（jget 印成 None），而 `[ -n "$X" ]` 对
# "None" 是**真**——P12.4 第三轮 24.04 的时区两条断言就是这样拿到一个空字符串的：
# 守卫没拦住，python 解析 "None" 抛异常，断言只看见 ''。等 + 认 None，才量得准。
next_elapse_of() {
  _id=$1; _limit=${2:-15}; _n=0; _v=""
  while [ "$_n" -lt "$_limit" ]; do
    api GET "/api/v1/schedules/$_id" >/dev/null
    _v=$(jget "$WORK/resp.json" 'd["data"]["schedule"].get("next_elapse") or ""')
    case "$_v" in
      ''|None|ERR:*) : ;;
      *) printf '%s' "$_v"; return 0 ;;
    esac
    _n=$((_n + 1)); sleep 1
  done
  printf '%s' "$_v"
}
sched_blockers_of() { api GET "/api/v1/schedules/$1" >/dev/null; jget "$WORK/resp.json" 'json.dumps([b["code"] for b in (d["data"]["schedule"].get("blockers") or [])])'; }
# sqlq 在面板库上跑一条只读查询（一进程一次；轮询另见 import_probe）。
sqlq() {
  python3 - "$WEB_DB" "$1" <<'PY'
import sqlite3, sys
con = sqlite3.connect('file:%s?mode=ro' % sys.argv[1], uri=True, timeout=10)
cur = con.execute(sys.argv[2])
rows = cur.fetchall()
print('\t'.join('|'.join('' if c is None else str(c) for c in r) for r in rows))
con.close()
PY
}
# import_probe 轮询面板库，等“代理产生的定时操作”被对账导入**并收敛**；一次 python3
# 启动内轮询，因为这台机器上一次解释器启动要好几秒，几百次调用会把 120 秒的预检窗口吃光。
#
# 为什么要等收敛而不是"看到行就返回"：导入落下的那一刻，needs_sync 一定是 1、事件只有
# 一条（它自己那条 transferred 事件）——这是设计的中间态，不是缺陷。任务中心本来就
# 靠下一轮对账（≤30 秒）把它推到代理说的阶段。只看到行就断言，量到的是"导入刚完成
# 的那一瞬"：P12.4 第三轮 24.04 就是这样红的两条（needs_sync 读成 1、事件数读成 1，
# 而同一行在三十秒后已经是对的）。等不到就把它当下那一行原样打出来，让人看得见
# needs_sync 卡在几——不静默、也不把"没等到"说成"通过"。
#
# 收敛的判据是**终态**，不是 needs_sync==0。这个区别是 22.04 一轮买来的：
# needs_sync 在"这一轮对账确实读到了代理、但操作还在跑"时也会被清成 0（那是对的，
# 它问的是"抄回来过没有"），于是"needs_sync==0 且事件≥2"在操作**还在跑**时就已经
# 成立。探针据此提前返回，量到 running，而同一行 28 秒后自己走到了 failed——断言
# 罚的是探针，不是产品（DECISIONS Q386）。要断言终态，就等终态。
import_probe() {
  python3 - "$WEB_DB" "$1" "${2:-180}" <<'PY'
import sqlite3, sys, time
db, op = sys.argv[1], sys.argv[2]
deadline = time.time() + float(sys.argv[3] if len(sys.argv) > 3 else 180)
SYSID = 1 << 62
con = sqlite3.connect('file:%s?mode=ro' % db, uri=True, timeout=10)
last = None
while True:
    row = con.execute(
        "SELECT id, status, cancellable, needs_sync, action, user_id FROM jobs WHERE operation_id = ?", (op,)).fetchone()
    if row:
        jid = row[0]
        events = con.execute("SELECT COUNT(*) FROM job_events WHERE job_id = ?", (jid,)).fetchone()[0]
        pf = con.execute("SELECT COUNT(*) FROM preflights WHERE consumed_job_id = ?", (jid,)).fetchone()[0]
        user = con.execute("SELECT username, role, disabled FROM users WHERE id = ?", (SYSID,)).fetchone()
        last = '\t'.join([str(x) for x in row] + [str(events), str(pf),
                                                  '|'.join(str(x) for x in user) if user else 'no-row'])
        if row[1] in ('succeeded', 'failed', 'canceled', 'interrupted') and events >= 2:
            print(last)
            con.close()
            sys.exit(0)
    if time.time() > deadline:
        con.close()
        print(last if last is not None else 'timeout')
        sys.exit(0)
    time.sleep(3)
PY
}

printf '== 0. 前置环境 ==\n'
{
  uname -a
  systemctl --version | head -2
  systemd-analyze --version | head -1
  sha256sum "$AGENT" "$WEB" "$CTL"
  printf 'P12.2: '; tail -1 "$PREV/logs/results.txt"
} >"$PAYLOADS/env.txt" 2>&1
pass "p12-2-prerequisite-loaded"

# 单元里的 ExecStart 指的是 /usr/local/bin/upanelctl（部署路径）。真机上它必须在那儿，
# 否则 timer 到点只会把一个不存在的路径交给 systemd。装的是同一份二进制，并留哈希记录。
install -m 0755 "$CTL" /usr/local/bin/upanelctl
expect_eq "upanelctl-installed" "0" "$([ -x /usr/local/bin/upanelctl ] && echo 0 || echo 1)"
expect_eq "upanelctl-hash-matches" "$(file_sha "$CTL")" "$(file_sha /usr/local/bin/upanelctl)"

if id -u upanel-shell >/dev/null 2>&1; then
  pass "upanel-shell-exists"
  expect_eq "upanel-shell-not-root" "0" "$([ "$(id -u upanel-shell)" != "0" ] && echo 0 || echo 1)"
  expect_not_contains "upanel-shell-no-sudo-group" "sudo" "$(id -nG upanel-shell)"
  expect_not_contains "upanel-shell-no-docker-group" "docker" "$(id -nG upanel-shell)"
  expect_not_contains "upanel-shell-no-disk-group" "disk" "$(id -nG upanel-shell)"
else
  fail "upanel-shell-exists" "受管理脚本没有运行身份；安装器负责创建该账号"
fi

cat >"$SYS_CFG" <<JSON
{
  "web": { "mode": "production", "listen_address": "127.0.0.1:$PORT", "shutdown_timeout_seconds": 10 },
  "agent": {
    "socket_path": "$SOCK", "shutdown_timeout_seconds": 10,
    "web_user": "$WEB_USER", "socket_group": "$WEB_USER",
    "database_path": "$AGENT_DB", "secret_key_path": "$KEY_FILE",
    "storage": { "smart_enabled": false, "smart_interval_minutes": 15, "network_capacity_timeout_seconds": 2 },
    "services": {}, "logs": {},
    "files": { "roots": [
      { "id": "root1", "name": "验收数据", "path": "$ROOT_PATH", "writable": true }
    ] },
    "backup": { "panel_database_path": "$WEB_DB", "panel_copy_dir": "$PREV/panel-copies" }
  },
  "database": { "path": "$WEB_DB", "busy_timeout_ms": 5000 },
  "log": { "level": "info", "format": "json" }
}
JSON
chmod 0644 "$SYS_CFG"
install -d -m 0755 /run/upanel
"$AGENT" --config "$SYS_CFG" >"$WORK/logs/agent.log" 2>&1 & AGENT_PID=$!
AGENT_READY=no
for _ in $(seq 1 200); do
  if [ -S "$SOCK" ] && runuser -u "$WEB_USER" -- curl -fsS -o /dev/null --unix-socket "$SOCK" http://agent/internal/v1/capabilities 2>/dev/null; then AGENT_READY=yes; break; fi
  kill -0 "$AGENT_PID" 2>/dev/null || break
  sleep 0.1
done
expect_eq "agent-started" "yes" "$AGENT_READY"
if [ "$AGENT_READY" != "yes" ]; then
  note "代理没能应答，日志尾部："
  sed -n '$p;1,20p' "$WORK/logs/agent.log" | while IFS= read -r _line; do note "  $_line"; done
fi
runuser -u "$WEB_USER" -- "$WEB" --mode production --db "$WEB_DB" --listen "127.0.0.1:$PORT" >"$WORK/logs/web.log" 2>&1 & WEB_PID=$!
WEB_UP=no
for _ in $(seq 1 200); do if curl -fsS "$BASE/api/v1/health/live" >/dev/null 2>&1; then WEB_UP=yes; break; fi; sleep 0.1; done
expect_eq "web-started" "yes" "$WEB_UP"
expect_eq "web-not-root" "0" "$(ps -eo uid=,comm= | awk '$2=="upanel-web" && $1==0 {n++} END {print n+0}')"

JAR=$WORK/admin.cookies
CODE=$(printf '{"username":"admin","password":"%s"}' "$PASSWORD" | curl -s -c "$JAR" -o "$PAYLOADS/login.json" -w '%{http_code}' -X POST "$BASE/api/v1/auth/login" -H 'Content-Type: application/json' -H 'X-UPanel-Request: 1' -H "Origin: $ORIGIN" -d @-)
expect_eq "login-admin" "200" "$CODE"
CSRF=$(jget "$PAYLOADS/login.json" 'd["data"]["csrf_token"]')

runuser -u "$WEB_USER" -- curl -s -o "$TMPD/capabilities.json" --unix-socket "$SOCK" http://agent/internal/v1/capabilities
cp "$TMPD/capabilities.json" "$PAYLOADS/capabilities.json"
for _a in schedule.apply schedule.delete script.apply script.delete script.run; do
  expect_eq "capabilities-$_a" "True" "$(jget "$PAYLOADS/capabilities.json" "\"$_a\" in d[\"data\"][\"supported_actions\"]")"
done
# 触发路由只给 root：它不是"能力"，是形状；能力表里不该出现一个调用就 403 的入口。
expect_not_contains "capabilities-no-trigger-action" "schedule.trigger" "$(cat "$PAYLOADS/capabilities.json")"

printf '\n== 1. 受管理脚本：登记、落盘权限、正文只在详情里 ==\n'
cat >"$TMPD/probe.sh" <<PROBE
#!/bin/sh
# 验收探针：证明自己是谁、能不能写特权路径。
# 写证据失败就当场 exit 3：写不出证据却回 0，等于让三条身份断言拿到空文件而静默通过。
id -un > "$WORK/probe/whoami.txt" || exit 3
id -u >> "$WORK/probe/whoami.txt" || exit 3
id -nG > "$WORK/probe/groups.txt" || exit 3
touch /etc/p12-4-should-fail 2>"$WORK/probe/etc-write.err"
echo "rc=\$?" >> "$WORK/probe/etc-write.err"
echo probe-done
PROBE
cat >"$TMPD/sleeper.sh" <<'SLEEPER'
#!/bin/sh
sleep 45
echo never-printed
SLEEPER
python3 - "$TMPD/probe.sh" "p12-4 探针 $SUFFIX" >"$TMPD/probe.json" <<'PY'
import json, sys
print(json.dumps({"script_id": None, "version": 0, "name": sys.argv[2], "content": open(sys.argv[1]).read()}))
PY
python3 - "$TMPD/sleeper.sh" "p12-4 超时 $SUFFIX" >"$TMPD/sleeper.json" <<'PY'
import json, sys
print(json.dumps({"script_id": None, "version": 0, "name": sys.argv[2], "content": open(sys.argv[1]).read()}))
PY

PROBE_BODY=$(cat "$TMPD/probe.json")
expect_eq "script-probe-reauth" "200" "$(reauth)"
expect_eq "script-probe-preflight-200" "200" "$(preflight script.apply "$PROBE_BODY" script-probe)"
expect_eq "script-probe-preflight-allowed" "True" "$(pget 'd["data"]["allowed"]')"
expect_contains "script-probe-impact-execute-as" "upanel-shell" "$(pget 'json.dumps([i["description"] for i in d["data"].get("impacts") or []])')"
PREF_CUR=$(pget 'd["data"]["preflight_id"]')
expect_eq "script-probe-submit-202" "202" "$(resubmit script-probe script.apply "$PROBE_BODY")"
finish_job script-probe
expect_eq "script-probe-succeeded" "succeeded" "$JOB_STATE"
SCRIPT_PROBE=$(jobget 'd["data"]["result"]["data"]["script_id"]')
expect_contains "script-probe-id-shape" "scr_" "$SCRIPT_PROBE"

# 第二份探针：库里有 UNIQUE(kind, target_id)——一个对象只接受一条计划，而下面要验
# "同一表达式在两个时区是两个时刻"，那需要两条计划。所以这里再建一份内容相同的脚本，
# 让时区那一节验的是时区，不是这条约束（约束本身单独一条断言，见 §3 的 target-taken）。
python3 - "$TMPD/probe.sh" "p12-4 探针UTC $SUFFIX" >"$TMPD/probe-utc.json" <<'PY'
import json, sys
print(json.dumps({"script_id": None, "version": 0, "name": sys.argv[2], "content": open(sys.argv[1]).read()}))
PY
PROBE_UTC_BODY=$(cat "$TMPD/probe-utc.json")
expect_eq "script-probe-utc-reauth" "200" "$(reauth)"
expect_eq "script-probe-utc-preflight-200" "200" "$(preflight script.apply "$PROBE_UTC_BODY" script-probe-utc)"
PREF_CUR=$(pget 'd["data"]["preflight_id"]')
expect_eq "script-probe-utc-submit-202" "202" "$(resubmit script-probe-utc script.apply "$PROBE_UTC_BODY")"
finish_job script-probe-utc
expect_eq "script-probe-utc-succeeded" "succeeded" "$JOB_STATE"
SCRIPT_PROBE_UTC=$(jobget 'd["data"]["result"]["data"]["script_id"]')
expect_contains "script-probe-utc-id-shape" "scr_" "$SCRIPT_PROBE_UTC"

# 落盘名是 <id>.sh（internal/scripts 的 PathFor），不是光秃的 id。
PROBE_PATH=$SCRIPT_DIR/$SCRIPT_PROBE.sh
expect_eq "script-probe-file-exists" "0" "$([ -f "$PROBE_PATH" ] && echo 0 || echo 1)"
expect_eq "script-probe-file-mode" "750 root upanel-shell" "$(stat -c '%a %U %G' "$PROBE_PATH" 2>/dev/null)"
expect_eq "script-probe-file-not-world-readable" "0" "$(stat -c '%a' "$PROBE_PATH" 2>/dev/null | awk '{print ($1 % 10 == 0) ? 0 : 1}')"

CODE=$(api GET /api/v1/scripts "" scripts-list)
expect_eq "scripts-list-200" "200" "$CODE"
expect_eq "scripts-list-has-no-content" "False" "$(rget 'any("content" in i for i in d["data"]["items"])')"
CODE=$(api GET "/api/v1/scripts/$SCRIPT_PROBE" "" script-detail)
expect_eq "script-detail-200" "200" "$CODE"
expect_eq "script-detail-interpreter" "/bin/sh" "$(rget 'd["data"]["script"]["interpreter"]')"
SCRIPT_HASH=$(rget 'd["data"]["script"]["content_hash"]')
expect_eq "script-detail-hash-shape" "64" "$(printf '%s' "$SCRIPT_HASH" | wc -c | tr -d ' ')"
# 正文的 SHA-256 必须等于 content_hash：运行时核对的就是这一对，它错了就永远跑不起来。
# 用一次 python3 独立算（不借 helper 的表达式，免得"用被测的东西证明被测的东西"）。
expect_eq "script-detail-content-matches-hash" "True" "$(python3 - "$PAYLOADS/script-detail.json" <<'PY'
import hashlib, json, sys
s = json.load(open(sys.argv[1]))["data"]["script"]
print(hashlib.sha256(s["content"].encode()).hexdigest() == s["content_hash"])
PY
)"
expect_contains "script-detail-content-is-probe" "id -un" "$(rget 'json.dumps(d["data"]["script"]["content"])')"

printf '\n== 2. 立即运行：以 upanel-shell 身份、拿不到 root ==\n'
RUN_BODY=$(printf '{"script_id":"%s","version":1,"max_runtime_seconds":60}' "$SCRIPT_PROBE")
expect_eq "script-run-reauth" "200" "$(reauth)"
expect_eq "script-run-preflight-200" "200" "$(preflight script.run "$RUN_BODY" script-run)"
expect_eq "script-run-preflight-allowed" "True" "$(pget 'd["data"]["allowed"]')"
PREF_CUR=$(pget 'd["data"]["preflight_id"]')
expect_eq "script-run-submit-202" "202" "$(resubmit script-run script.run "$RUN_BODY")"
finish_job script-run
expect_eq "script-run-succeeded" "succeeded" "$JOB_STATE"
expect_eq "script-run-exit-code" "0" "$(jobget 'str(d["data"]["result"]["observed_state"]["exit_code"])')"
expect_eq "script-run-identity" "upanel-shell
$(id -u upanel-shell)" "$(cat "$WORK/probe/whoami.txt" 2>/dev/null)"
expect_ne "script-run-uid-not-zero" "0" "$(sed -n '2p' "$WORK/probe/whoami.txt" 2>/dev/null)"
expect_contains "script-run-groups-written" "upanel-shell" "$(cat "$WORK/probe/groups.txt" 2>/dev/null)"
expect_contains "script-run-cannot-write-etc" "rc=1" "$(cat "$WORK/probe/etc-write.err" 2>/dev/null)"
expect_eq "script-run-no-root-file" "0" "$([ -e /etc/p12-4-should-fail ] && echo 1 || echo 0)"

printf '\n== 3. 计划：定义 → 单元 → systemd 的原话 ==\n'
DEF_ASIA=$(defn "p12-4 每日 $SUFFIX" user_script "$SCRIPT_PROBE" "*-*-* 04:30:00" "Asia/Shanghai" true 120 true)
apply_schedule schedule-create "$(create_params "$DEF_ASIA")"
SCH=$(jobget 'd["data"]["result"]["data"]["schedule_id"]')
expect_contains "schedule-create-id-shape" "sch_" "$SCH"
expect_eq "schedule-create-version" "1" "$(jobget 'str(d["data"]["result"]["data"]["version"])')"
expect_eq "schedule-file-service" "0" "$([ -f "$UNIT_DIR/upanel-$SCH.service" ] && echo 0 || echo 1)"
expect_eq "schedule-file-timer" "0" "$([ -f "$UNIT_DIR/upanel-$SCH.timer" ] && echo 0 || echo 1)"

expect_contains "unit-service-execstart-fixed" \
  "ExecStart=/usr/local/bin/upanelctl schedule-trigger --schedule-id $SCH --wait" "$(cat "$UNIT_DIR/upanel-$SCH.service")"
expect_contains "unit-service-type-oneshot" "Type=oneshot" "$(cat "$UNIT_DIR/upanel-$SCH.service")"
# --wait 是不重叠的实现位置：单元主进程守到操作终态才退出。
# 判据是"整个单元里只有一条 Exec 行，且它带 --wait"——数 --wait 出现的次数会把
# 单元文件里的注释行也算进去（注释解释机制时会写到它），那是行文，不是行为。
EXEC_LINES=$(grep -c '^Exec' "$UNIT_DIR/upanel-$SCH.service" 2>/dev/null)
if [ "$EXEC_LINES" = "1" ] && grep -qx "ExecStart=/usr/local/bin/upanelctl schedule-trigger --schedule-id $SCH --wait" "$UNIT_DIR/upanel-$SCH.service"; then
  pass "unit-execstart-single-line"
else
  fail "unit-execstart-single-line" "期望恰好一条带 --wait 的 ExecStart，实际 Exec 行数=$EXEC_LINES"
fi
expect_contains "unit-service-user-root" "User=root" "$(cat "$UNIT_DIR/upanel-$SCH.service")"
# 时限 = 计划最长运行 + 60 秒收尾（见 units.go 的 TriggerWaitSlackSeconds）。
expect_contains "unit-service-timeout" "TimeoutStartSec=180s" "$(cat "$UNIT_DIR/upanel-$SCH.service")"
expect_not_contains "unit-service-no-app-params" "script_id" "$(cat "$UNIT_DIR/upanel-$SCH.service")"
expect_contains "unit-timer-oncalendar-timezone" "OnCalendar=*-*-* 04:30:00 Asia/Shanghai" "$(cat "$UNIT_DIR/upanel-$SCH.timer")"
expect_contains "unit-timer-persistent-true" "Persistent=true" "$(cat "$UNIT_DIR/upanel-$SCH.timer")"
expect_contains "unit-timer-accuracy" "AccuracySec=1s" "$(cat "$UNIT_DIR/upanel-$SCH.timer")"
expect_contains "unit-timer-target-service" "Unit=upanel-$SCH.service" "$(cat "$UNIT_DIR/upanel-$SCH.timer")"
expect_contains "unit-timer-wantedby" "WantedBy=timers.target" "$(cat "$UNIT_DIR/upanel-$SCH.timer")"
if systemd-analyze verify "upanel-$SCH.timer" >"$TMPD/verify.log" 2>&1; then pass "unit-verify-ok"; else fail "unit-verify-ok" "$(cat "$TMPD/verify.log")"; fi
systemctl daemon-reload
expect_eq "unit-is-enabled" "enabled" "$(systemctl is-enabled "upanel-$SCH.timer" 2>/dev/null)"
expect_eq "unit-is-active" "active" "$(systemctl is-active "upanel-$SCH.timer" 2>/dev/null)"
expect_contains "unit-listed-by-list-timers" "upanel-$SCH.timer" "$(systemctl list-timers --all --no-legend 2>/dev/null)"

# 一个对象只接受一条计划（库里有 UNIQUE(kind, target_id)）。这条约束必须在**预检**就说
# 话：让它到执行阶段才炸，形状就是"预检说允许、提交以后失败"——真机现场见过一次
# （同一份脚本再排一个 UTC 时间窗，提交后操作失败在"这个对象已经被另一条计划任务使用"）。
DUP_DEF=$(defn "p12-4 重复 $SUFFIX" user_script "$SCRIPT_PROBE" "*-*-* 06:00:00" "Asia/Shanghai" true 120 true)
expect_eq "schedule-target-taken-preflight-200" "200" "$(preflight schedule.apply "$(create_params "$DUP_DEF")" schedule-target-taken)"
expect_eq "schedule-target-taken-blocked" "False" "$(pget 'd["data"]["allowed"]')"
expect_contains "schedule-target-taken-blocker" "SCHEDULE_TARGET_TAKEN" "$(pblockers)"
# 重名同样在预检就挡（另一个对象、同一个名字）。
DUP_NAME_DEF=$(defn "p12-4 每日 $SUFFIX" user_script "$SCRIPT_PROBE_UTC" "*-*-* 07:00:00" "Asia/Shanghai" true 120 true)
expect_eq "schedule-name-taken-preflight-200" "200" "$(preflight schedule.apply "$(create_params "$DUP_NAME_DEF")" schedule-name-taken)"
expect_eq "schedule-name-taken-blocked" "False" "$(pget 'd["data"]["allowed"]')"
expect_contains "schedule-name-taken-blocker" "SCHEDULE_NAME_TAKEN" "$(pblockers)"

CODE=$(api GET /api/v1/schedules "" schedules-list)
expect_eq "schedules-list-200" "200" "$CODE"
expect_eq "schedules-list-contains" "True" "$(rget "\"$SCH\" in json.dumps(d[\"data\"][\"items\"])")"
expect_eq "schedule-installed" "True" "$(sched_field "$SCH" installed)"
expect_eq "schedule-unit-file-state" "enabled" "$(sched_field "$SCH" unit_file_state)"
expect_eq "schedule-active-state" "active" "$(sched_field "$SCH" active_state)"
expect_eq "schedule-timer-ref" "upanel-$SCH.timer" "$(sched_field "$SCH" timer_ref)"
expect_eq "schedule-blockers-empty" "[]" "$(sched_blockers "$SCH")"
expect_eq "schedule-def-timezone" "Asia/Shanghai" "$(sched_def "$SCH" timezone)"
expect_eq "schedule-def-persistent" "True" "$(sched_def "$SCH" persistent)"
expect_eq "schedule-def-max-runtime" "120" "$(sched_def "$SCH" max_runtime_seconds)"

# 下次触发必须是"该时区的 04:30:00"，不是主机的 04:30。
CODE=$(api GET "/api/v1/schedules/$SCH" "" schedule-detail-asia)
NEXT_ASIA=$(next_elapse_of "$SCH" 15)
if [ -n "$NEXT_ASIA" ]; then
  pass "schedule-next-elapse-present"
  expect_eq "schedule-next-elapse-wallclock-asia" "04:30:00" "$(python3 - "$NEXT_ASIA" <<'PY'
import sys, datetime
from zoneinfo import ZoneInfo
t = datetime.datetime.fromisoformat(sys.argv[1].replace("Z", "+00:00"))
print(t.astimezone(ZoneInfo("Asia/Shanghai")).strftime("%H:%M:%S"))
PY
)"
  expect_eq "schedule-next-elapse-in-future" "True" "$(python3 - "$NEXT_ASIA" <<'PY'
import sys, datetime
t = datetime.datetime.fromisoformat(sys.argv[1].replace("Z", "+00:00"))
print(datetime.datetime.now(datetime.timezone.utc) < t)
PY
)"
else
  fail "schedule-next-elapse-present" "等了 15 秒仍没读到 next_elapse（原值 '$NEXT_ASIA'），阻塞项 $(sched_blockers_of "$SCH")"
fi

printf '\n== 4. 时区：同一表达式、两个时区，是两个时刻 ==\n'
DEF_UTC=$(defn "p12-4 UTC $SUFFIX" user_script "$SCRIPT_PROBE_UTC" "*-*-* 04:30:00" "UTC" true 120 true)
apply_schedule schedule-utc "$(create_params "$DEF_UTC")"
SCH_UTC=$(jobget 'd["data"]["result"]["data"]["schedule_id"]')
expect_contains "unit-timer-oncalendar-utc" "OnCalendar=*-*-* 04:30:00 UTC" "$(cat "$UNIT_DIR/upanel-$SCH_UTC.timer")"
NEXT_UTC=$(next_elapse_of "$SCH_UTC" 15)
if [ -n "$NEXT_ASIA" ] && [ -n "$NEXT_UTC" ]; then
  expect_eq "timezone-offset-eight-hours-mod-day" "True" "$(python3 - "$NEXT_ASIA" "$NEXT_UTC" <<'PY'
import sys, datetime
a = datetime.datetime.fromisoformat(sys.argv[1].replace("Z", "+00:00"))
b = datetime.datetime.fromisoformat(sys.argv[2].replace("Z", "+00:00"))
diff = int((a - b).total_seconds()) % 86400
print(diff in (28800, 57600))
PY
)"
  expect_eq "timezone-wallclock-utc" "04:30:00" "$(python3 - "$NEXT_UTC" <<'PY'
import sys, datetime
t = datetime.datetime.fromisoformat(sys.argv[1].replace("Z", "+00:00"))
print(t.strftime("%H:%M:%S"))
PY
)"
else
  # 读不到就如实红：这是"不会触发"或"读不出来"，监控面板上看起来是同一件事，
  # 而两种都不能当成通过。把两条的原始值与时区计划的阻塞项一起打出来。
  fail "timezone-offset-eight-hours-mod-day" \
    "两条计划没有同时读到 next_elapse（Asia='$NEXT_ASIA'，UTC='$NEXT_UTC'，UTC 阻塞项 $(sched_blockers_of "$SCH_UTC")）"
  fail "timezone-wallclock-utc" "UTC 计划的 next_elapse 读不到（原值 '$NEXT_UTC'）"
fi

printf '\n== 5. 错过执行策略与停用：改的是同一份定义，单元跟着改 ==\n'
DEF_UTC_NOPERSIST=$(defn "p12-4 UTC $SUFFIX" user_script "$SCRIPT_PROBE_UTC" "*-*-* 04:30:00" "UTC" false 120 true)
apply_schedule schedule-update-nopersist "$(update_params "$SCH_UTC" 1 "$DEF_UTC_NOPERSIST")"
expect_eq "schedule-update-version" "2" "$(jobget 'str(d["data"]["result"]["data"]["version"])')"
expect_contains "unit-timer-persistent-false" "Persistent=false" "$(cat "$UNIT_DIR/upanel-$SCH_UTC.timer")"
expect_contains "unit-timer-misfire-documented" "错过就跳过" "$(cat "$UNIT_DIR/upanel-$SCH_UTC.timer")"

DEF_UTC_OFF=$(defn "p12-4 UTC $SUFFIX" user_script "$SCRIPT_PROBE_UTC" "*-*-* 04:30:00" "UTC" false 120 false)
apply_schedule schedule-disable "$(update_params "$SCH_UTC" 2 "$DEF_UTC_OFF")"
expect_eq "schedule-disable-version" "3" "$(jobget 'str(d["data"]["result"]["data"]["version"])')"
expect_eq "schedule-disable-files-gone" "0" "$([ -e "$UNIT_DIR/upanel-$SCH_UTC.timer" ] && echo 1 || echo 0)"
expect_eq "schedule-disable-installed-false" "False" "$(sched_field "$SCH_UTC" installed)"
expect_eq "schedule-disable-enabled-false" "False" "$(sched_def "$SCH_UTC" enabled)"
expect_ne "schedule-disable-unit-file-state" "enabled" "$(sched_field "$SCH_UTC" unit_file_state)"

# 停用的计划不能被触发：定时器已经不在，但"万一"由代理拦。
TRIG_DISABLED=$("$CTL" schedule-trigger --schedule-id "$SCH_UTC" --socket "$SOCK" --json 2>"$TMPD/trig-disabled.err")
TRIG_DISABLED_RC=$?
expect_ne "trigger-disabled-rejected" "0" "$TRIG_DISABLED_RC"
expect_contains "trigger-disabled-reason" "停用" "$(cat "$TMPD/trig-disabled.err")$TRIG_DISABLED"

printf '\n== 6. 触发：稳定 ID、重复判决、只认 root ==\n'
# "同一分钟内再触发一次"是本节的核心断言，而两次调用有可能正好跨过分钟边界——
# 那不是缺陷，是两条不同的槽。跨了就重来一次，而不是把失败判给产品（Q361 的同一条纪律）。
trigger_once() {
  "$CTL" schedule-trigger --schedule-id "$SCH" --socket "$SOCK" --json >"$TMPD/$1.json" 2>"$TMPD/$1.err"
  printf '%s' "$?"
}
SLOT_SHARED=no
_attempt=0
while [ "$_attempt" -lt 3 ]; do
  RC1=$(trigger_once trig1)
  SLOT1=$(jget "$TMPD/trig1.json" 'd["slot_at"]')
  RC2=$(trigger_once trig2)
  SLOT2=$(jget "$TMPD/trig2.json" 'd["slot_at"]')
  if [ "$SLOT1" = "$SLOT2" ]; then SLOT_SHARED=yes; break; fi
  note "两次触发跨过了分钟边界（$SLOT1 → $SLOT2），重来一次。"
  _attempt=$((_attempt + 1))
done
cp "$TMPD/trig1.json" "$PAYLOADS/trigger-first.json"
cp "$TMPD/trig2.json" "$PAYLOADS/trigger-second.json"
expect_eq "trigger-same-slot" "yes" "$SLOT_SHARED"
expect_eq "trigger-first-exit" "0" "$RC1"
expect_eq "trigger-second-exit" "0" "$RC2"
expect_eq "trigger-first-outcome" "triggered" "$(jget "$PAYLOADS/trigger-first.json" 'd["outcome"]')"
TRIG1_ID=$(jget "$PAYLOADS/trigger-first.json" 'd["trigger_id"]')
OP1=$(jget "$PAYLOADS/trigger-first.json" 'd["operation_id"]')
expect_contains "trigger-first-trigger-id-shape" "trg_" "$TRIG1_ID"
expect_contains "trigger-first-operation-id-shape" "op_" "$OP1"
expect_eq "trigger-first-slot-utc-minute" "00" "$(jget "$PAYLOADS/trigger-first.json" 'd["slot_at"][17:19]')"

# 同一分钟里再触发一次：这是 systemd 重试/手工 systemctl start 的形状，必须判重放。
expect_eq "trigger-second-duplicate" "True" "$(jget "$PAYLOADS/trigger-second.json" 'd["duplicate"]')"
expect_eq "trigger-second-same-operation" "$OP1" "$(jget "$PAYLOADS/trigger-second.json" 'd["operation_id"]')"
expect_eq "trigger-second-same-trigger-id" "$TRIG1_ID" "$(jget "$PAYLOADS/trigger-second.json" 'd["trigger_id"]')"

TRIGGERS=$(sched_triggers "$SCH")
expect_eq "trigger-record-count" "1" "$(jget "$WORK/resp.json" 'str(len(d["data"]["triggers"]))')"
expect_contains "trigger-record-outcome" "triggered" "$TRIGGERS"

# Web 账号不能触发：这条路由按形状只放行 UID 0。
runuser -u "$WEB_USER" -- "$CTL" schedule-trigger --schedule-id "$SCH" --socket "$SOCK" --json >"$TMPD/trig-web.out" 2>"$TMPD/trig-web.err"
WEB_TRIG_RC=$?
expect_ne "trigger-web-account-rejected" "0" "$WEB_TRIG_RC"
expect_contains "trigger-web-account-reason" "无权" "$(cat "$TMPD/trig-web.err")$(cat "$TMPD/trig-web.out")"

# 伪造一个"一小时前"的槽：代理必须按自己的时钟重算，不接受调用方自称的时间。
FORGED=$(python3 - "$SCH" <<'PY'
import hashlib, sys, datetime
slot = datetime.datetime.now(datetime.timezone.utc).replace(second=0, microsecond=0) - datetime.timedelta(hours=1)
print("trg_" + hashlib.sha256(("trigger\n" + sys.argv[1] + "\n" + slot.strftime("%Y-%m-%dT%H:%M:%SZ")).encode()).hexdigest()[:32])
PY
)
CODE=$(printf '{"trigger_id":"%s"}' "$FORGED" | curl -s -o "$TMPD/forged.json" -w '%{http_code}' --unix-socket "$SOCK" -X POST "http://agent/internal/v1/schedules/$SCH/trigger" -H 'Content-Type: application/json' -d @-)
cp "$TMPD/forged.json" "$PAYLOADS/trigger-forged.json"
expect_eq "trigger-forged-slot-400" "400" "$CODE"
expect_contains "trigger-forged-slot-reason" "时间槽" "$(jget "$PAYLOADS/trigger-forged.json" 'd["error"]["message"]')"

printf '\n== 7. 定时操作被面板对账导入任务表 ==\n'
IMPORT=$(import_probe "$OP1" 200)
if [ "$IMPORT" = "timeout" ]; then
  fail "import-job-row" "200 秒内没有在面板库看到 operation_id=$OP1 的任务行"
  notrun "import-job-status" "没有导入行"
else
  pass "import-job-row"
  IMPORT_ROW=$(printf '%s' "$IMPORT" | cut -f1)
  IMPORT_STATUS=$(printf '%s' "$IMPORT" | cut -f2)
  IMPORT_CANCEL=$(printf '%s' "$IMPORT" | cut -f3)
  IMPORT_SYNC=$(printf '%s' "$IMPORT" | cut -f4)
  IMPORT_ACTION=$(printf '%s' "$IMPORT" | cut -f5)
  IMPORT_UID=$(printf '%s' "$IMPORT" | cut -f6)
  IMPORT_EVENTS=$(printf '%s' "$IMPORT" | cut -f7)
  IMPORT_PF=$(printf '%s' "$IMPORT" | cut -f8)
  IMPORT_USER=$(printf '%s' "$IMPORT" | cut -f9)
  printf '%s\n' "$IMPORT" >"$PAYLOADS/import-row.txt"
  expect_eq "import-job-status" "succeeded" "$IMPORT_STATUS"
  expect_eq "import-job-action" "script.run" "$IMPORT_ACTION"
  expect_eq "import-job-user-is-system" "4611686018427387904" "$IMPORT_UID"
  expect_eq "import-job-cancellable-false" "0" "$IMPORT_CANCEL"
  expect_eq "import-job-needs-sync-cleared" "0" "$IMPORT_SYNC"
  expect_ge "import-job-has-events" "$IMPORT_EVENTS" 2
  expect_eq "import-job-has-preflight-row" "1" "$IMPORT_PF"
  expect_eq "import-system-user-row" "scheduler|viewer|1" "$IMPORT_USER"
fi

CODE=$(api GET /api/v1/users "" users-list)
expect_eq "users-list-200" "200" "$CODE"
expect_not_contains "users-list-hides-scheduler" "scheduler" "$(cat "$PAYLOADS/users-list.json")"

LOGIN_SCHED=$(printf '{"username":"scheduler","password":"whatever-long-enough"}' | curl -s -o "$TMPD/login-sched.json" -w '%{http_code}' -X POST "$BASE/api/v1/auth/login" -H 'Content-Type: application/json' -H 'X-UPanel-Request: 1' -H "Origin: $ORIGIN" -d @-)
LOGIN_GHOST=$(printf '{"username":"p12-4-ghost-%s","password":"whatever-long-enough"}' "$SUFFIX" | curl -s -o "$TMPD/login-ghost.json" -w '%{http_code}' -X POST "$BASE/api/v1/auth/login" -H 'Content-Type: application/json' -H 'X-UPanel-Request: 1' -H "Origin: $ORIGIN" -d @-)
cp "$TMPD/login-sched.json" "$PAYLOADS/login-scheduler.json"
cp "$TMPD/login-ghost.json" "$PAYLOADS/login-ghost.json"
expect_eq "scheduler-login-rejected" "401" "$LOGIN_SCHED"
expect_eq "scheduler-login-code-same-as-unknown" \
  "$(jget "$PAYLOADS/login-ghost.json" 'd["error"]["code"]')" "$(jget "$PAYLOADS/login-scheduler.json" 'd["error"]["code"]')"
expect_eq "scheduler-login-message-same-as-unknown" \
  "$(jget "$PAYLOADS/login-ghost.json" 'd["error"]["message"]')" "$(jget "$PAYLOADS/login-scheduler.json" 'd["error"]["message"]')"

printf '\n== 8. 超时：上限是定义里的数，不是"跑到天荒地老" ==\n'
SLEEP_BODY=$(cat "$TMPD/sleeper.json")
expect_eq "script-sleep-reauth" "200" "$(reauth)"
expect_eq "script-sleep-preflight-200" "200" "$(preflight script.apply "$SLEEP_BODY" script-sleep)"
expect_eq "script-sleep-preflight-allowed" "True" "$(pget 'd["data"]["allowed"]')"
PREF_CUR=$(pget 'd["data"]["preflight_id"]')
expect_eq "script-sleep-submit-202" "202" "$(resubmit script-sleep script.apply "$SLEEP_BODY")"
finish_job script-sleep
expect_eq "script-sleep-succeeded" "succeeded" "$JOB_STATE"
SCRIPT_SLEEP=$(jobget 'd["data"]["result"]["data"]["script_id"]')

DEF_TIMEOUT=$(defn "p12-4 超时 $SUFFIX" user_script "$SCRIPT_SLEEP" "*-*-* 05:00:00" "Asia/Shanghai" false 3 true)
apply_schedule schedule-timeout "$(create_params "$DEF_TIMEOUT")"
SCH_TIMEOUT=$(jobget 'd["data"]["result"]["data"]["schedule_id"]')
expect_contains "unit-timeout-from-definition" "TimeoutStartSec=63s" "$(cat "$UNIT_DIR/upanel-$SCH_TIMEOUT.service")"
# 这一次刻意**不带** --wait：这里要知道的是"代理受理了、后台真的被杀掉了"。
# --wait 自己会不会退出、退出码对不对，由下面 §8a 单独验。
"$CTL" schedule-trigger --schedule-id "$SCH_TIMEOUT" --socket "$SOCK" --json >"$TMPD/trig-timeout.json" 2>"$TMPD/trig-timeout.err"
expect_eq "timeout-trigger-accepted" "0" "$?"
OP_TIMEOUT=$(jget "$TMPD/trig-timeout.json" 'd["operation_id"]')
cp "$TMPD/trig-timeout.json" "$PAYLOADS/trigger-timeout.json"
TIMEOUT_IMPORT=$(import_probe "$OP_TIMEOUT" 200)
if [ "$TIMEOUT_IMPORT" = "timeout" ]; then
  fail "timeout-operation-imported" "没在面板库看到超时那次的任务行；kill 是否生效只能从代理库读"
  notrun "timeout-operation-failed" "没有导入行"
else
  pass "timeout-operation-imported"
  expect_eq "timeout-operation-failed" "failed" "$(printf '%s' "$TIMEOUT_IMPORT" | cut -f2)"
  ERR=$(sqlq "SELECT error_json FROM jobs WHERE operation_id = '$OP_TIMEOUT'")
  expect_contains "timeout-operation-reason" "超过 3 秒被终止" "$ERR"
  printf '%s\n' "$ERR" >"$PAYLOADS/timeout-error.txt"
fi
sleep 1
expect_eq "timeout-no-leftover-sleep" "0" "$(pgrep -f 'sleep 45' 2>/dev/null | wc -l | tr -d ' ')"

printf '\n== 8a. --wait 真的在看这次操作（而不是卡在某个 403 上）==\n'
# 这一条是 P12.4 第一轮在 24.04 上抓到的真缺陷的回归断言：那时候 --wait 每 2 秒撞一次
# 403「socket 对端 UID 无权调用该接口」，于是它**永远不退出**——单元一直 activating，
# 被 TimeoutStartSec 掐掉。看起来"不重叠成立"，实际根本没在等。
# 现在同一个 3 秒上限的计划再带 --wait 跑一次：操作会在 3 秒时被代理杀掉，所以
# upanelctl 必须自己发现终态、带着失败原因退出 1，而且是在时限内退出。
_WAIT_T0=$(date +%s)
"$CTL" schedule-trigger --schedule-id "$SCH_TIMEOUT" --socket "$SOCK" --wait \
  >"$TMPD/wait-timeout.out" 2>"$TMPD/wait-timeout.err"
_WAIT_RC=$?
_WAIT_ELAPSED=$(( $(date +%s) - _WAIT_T0 ))
cp "$TMPD/wait-timeout.out" "$PAYLOADS/wait-timeout.out"
cp "$TMPD/wait-timeout.err" "$PAYLOADS/wait-timeout.err"
expect_eq "wait-failed-exit" "1" "$_WAIT_RC"
expect_contains "wait-reports-terminal-stage" "结束于 failed" "$(cat "$TMPD/wait-timeout.out")"
expect_le "wait-exits-within-deadline" "$_WAIT_ELAPSED" 35
expect_not_contains "wait-no-permission-warnings" "无权调用" "$(cat "$TMPD/wait-timeout.err")"

printf '\n== 8b. 不重叠：上一次还在跑时，定时器不会起第二次 ==\n'
# 75 秒的脚本 + 每分钟一次的日历：第一次在分钟边界跑起来，T+60 的唤醒正好落在单元
# active 期间。单元带 --wait，一直 active 到操作终态，systemd 不会为正在运行的
# oneshot 再起一个实例——所以 T+60 那次唤醒只更新下一次到期时间，不产生第二条触发。
# 这一条必须由真 systemd 跑，不能靠读单元文本推断。
cat >"$TMPD/long.sh" <<'LONG'
#!/bin/sh
sleep 75
echo long-done
LONG
python3 - "$TMPD/long.sh" "p12-4 长跑 $SUFFIX" >"$TMPD/long.json" <<'PY'
import json, sys
print(json.dumps({"script_id": None, "version": 0, "name": sys.argv[2], "content": open(sys.argv[1]).read()}))
PY
LONG_BODY=$(cat "$TMPD/long.json")
expect_eq "script-long-reauth" "200" "$(reauth)"
expect_eq "script-long-preflight-200" "200" "$(preflight script.apply "$LONG_BODY" script-long)"
expect_eq "script-long-preflight-allowed" "True" "$(pget 'd["data"]["allowed"]')"
PREF_CUR=$(pget 'd["data"]["preflight_id"]')
expect_eq "script-long-submit-202" "202" "$(resubmit script-long script.apply "$LONG_BODY")"
finish_job script-long
expect_eq "script-long-succeeded" "succeeded" "$JOB_STATE"
SCRIPT_LONG=$(jobget 'd["data"]["result"]["data"]["script_id"]')

sched_trigger_count() { api GET "/api/v1/schedules/$1" >/dev/null; jget "$WORK/resp.json" 'len(d["data"]["triggers"] or [])'; }
sched_slot0() { api GET "/api/v1/schedules/$1" >/dev/null; jget "$WORK/resp.json" 'd["data"]["triggers"][0]["slot_at"]'; }

DEF_OVERLAP=$(defn "p12-4 不重叠 $SUFFIX" user_script "$SCRIPT_LONG" "*-*-* *:*:00" "UTC" false 120 true)
apply_schedule schedule-overlap "$(create_params "$DEF_OVERLAP")"
SCH_OVERLAP=$(jobget 'd["data"]["result"]["data"]["schedule_id"]')
expect_contains "unit-overlap-oncalendar-minute" "OnCalendar=*-*-* *:*:00 UTC" "$(cat "$UNIT_DIR/upanel-$SCH_OVERLAP.timer")"
expect_eq "unit-overlap-enabled" "enabled" "$(systemctl is-enabled "upanel-$SCH_OVERLAP.timer" 2>/dev/null)"

FIRST_SLOT=""
for _ in $(seq 1 120); do
  if [ "$(sched_trigger_count "$SCH_OVERLAP")" != "0" ]; then FIRST_SLOT=$(sched_slot0 "$SCH_OVERLAP"); break; fi
  sleep 1
done
if [ -z "$FIRST_SLOT" ]; then
  fail "overlap-first-trigger" "120 秒内没有看到第一次触发；定时器根本没跑起来"
  notrun "overlap-first-run-outlives-the-wake" "没有第一次触发"
  notrun "overlap-runs-do-not-overlap" "没有第一次触发"
else
  pass "overlap-first-trigger"
  # 等到 T+65 秒：T+60 的那次唤醒已经发生过了。这里**不看**那一刻的 is-active——
  # "睡到某一秒再看一眼"的判据在慢机器上会漂：22.04（systemd 249）那一轮里睡醒时
  # 第一次运行已经结束，读到 inactive 就判红，可 journal 表明那次运行持续了 79 秒、
  # 第二次唤醒确实撞在它运行中。判据改成从 journal 读事实：第一次运行持续多久、
  # 几次运行之间有没有交叠。
  WAIT_FOR=$(python3 - "$FIRST_SLOT" <<'PY'
import sys, datetime
slot = datetime.datetime.fromisoformat(sys.argv[1].replace("Z", "+00:00"))
now = datetime.datetime.now(datetime.timezone.utc)
left = 65 - (now - slot).total_seconds()
print(max(0, int(left + 0.5)))
PY
)
  sleep "$WAIT_FOR"
  # "单元还在跑"有两种可能：真的在等这次操作，或者卡在某个权限错误上永远不退。
  # journal 能分开它们：前者有这个操作 ID 的等待行，后者是每 2 秒一条无权调用。
  _OVL_LOG=$(journalctl -u "upanel-$SCH_OVERLAP.service" --no-pager 2>/dev/null)
  printf '%s\n' "$_OVL_LOG" >"$WORK/logs/overlap-unit-journal.txt"
  expect_contains "overlap-wait-reads-own-operation" "等待操作 op_" "$_OVL_LOG"
  expect_not_contains "overlap-wait-no-permission-warnings" "无权调用" "$_OVL_LOG"
  # 收掉定时器，免得 T+120 又跑一次；随后单元应当自己回到 inactive。
  systemctl disable --now "upanel-$SCH_OVERLAP.timer" >/dev/null 2>&1
  _ran=0
  for _ in $(seq 1 90); do
    if [ "$(systemctl is-active "upanel-$SCH_OVERLAP.service" 2>/dev/null)" = "inactive" ]; then _ran=1; break; fi
    sleep 1
  done
  expect_eq "overlap-unit-finished" "1" "$_ran"
  # 结束的原因也必须对：Result=success 说明它是"操作成功所以退出"，
  # 不是被 TimeoutStartSec 掐掉的（那种是 timeout）。
  expect_eq "overlap-unit-result" "success" "$(systemctl show -p Result --value "upanel-$SCH_OVERLAP.service" 2>/dev/null)"
  # "到期时单元还在跑"之后到底发生什么，systemd 有两手：跳过，或者把这次唤醒**排队**
  # 到当前这次运行结束之后再起一次（24.04/systemd 255 与 22.04/systemd 249 都是后者）。
  # 所以"总共只触发一次"不是契约，**两次运行不重叠**才是：一条 Starting 之后、下一条
  # Starting 之前，必须先有那条 Finished/Failed。这才是 §14 那句"同计划重复触发仅运行
  # 一次"要保的东西。
  #
  # 判据不能认单元名：24.04 的 journal 写 "Starting upanel-sch_x.service - uPanel 计划
  # 任务：…"，22.04 只写 "Starting uPanel 计划任务：…"，单元名根本不出现——按
  # "Starting upanel-" 去 grep 在 22.04 上只会得到空文件，于是"序列为空"被判成重叠
  # （DECISIONS Q387）。journalctl -u 已经把范围限死在这一个单元上，所以只认动词。
  journalctl -u "upanel-$SCH_OVERLAP.service" --no-pager -o short-iso 2>/dev/null \
    >"$WORK/logs/overlap-unit-sequence.txt" || true
  _OVL_FACTS=$(python3 - "$WORK/logs/overlap-unit-sequence.txt" <<'PY'
import sys, re, datetime
path = sys.argv[1]
stamp = re.compile(r"([+-]\d{2})(\d{2})$")
def when(ts):
    return datetime.datetime.fromisoformat(stamp.sub(r"\1:\2", ts))
seq, started, first = [], None, None
for line in open(path, encoding="utf-8"):
    if "systemd[" not in line:
        continue          # upanelctl 自己的日志行不是"运行开始/结束"
    ts, _, rest = line.partition(" ")
    msg = rest.split(": ", 1)[1] if ": " in rest else ""
    if msg.startswith("Starting "):
        seq.append("S")
        if started is None:
            started = when(ts)
    elif msg.startswith("Finished ") or "Failed with result" in msg:
        seq.append("F")
        if first is None and started is not None:
            first = int(round((when(ts) - started).total_seconds()))
ok, open_run = bool(seq), False
for e in seq:
    if e == "S":
        if open_run:
            ok = False
            break
        open_run = True
    else:
        if not open_run:
            ok = False
            break
        open_run = False
verdict = "sequential" if ok else ("empty" if not seq else "overlap:" + "".join(seq))
print("%s\t%d\t%s" % (verdict, first if first is not None else -1, "".join(seq)))
PY
)
  expect_eq "overlap-runs-do-not-overlap" "sequential" "$(printf '%s' "$_OVL_FACTS" | cut -f1)"
  # 第一次运行必须比一次唤醒的间隔长：T+60 那次唤醒撞上来时，单元确实还在跑。
  # 这是"不重叠"里"上一次还在跑"那一半的证据，来自 journal 的时长，不是某一次的抽样。
  expect_ge "overlap-first-run-outlives-the-wake" "$(printf '%s' "$_OVL_FACTS" | cut -f2)" 61
  note "单元启动/结束序列 $(printf '%s' "$_OVL_FACTS" | cut -f3)，第一次运行 $(printf '%s' "$_OVL_FACTS" | cut -f2) 秒，触发记录 $(sched_trigger_count "$SCH_OVERLAP") 条"
fi

printf '\n== 9. 证书续期定时任务的接线（P12.5）==\n'
DEF_CRT=$(defn "p12-4 证书 $SUFFIX" certificate_renewal "crt_00000000000000000000000000000000" "*-*-* 03:00:00" "Asia/Shanghai" false 300 true)
expect_eq "schedule-cert-reauth" "200" "$(reauth)"
CODE=$(preflight schedule.apply "$(create_params "$DEF_CRT")" schedule-cert)
expect_eq "schedule-cert-preflight-200" "200" "$CODE"
expect_eq "schedule-cert-preflight-blocked" "False" "$(pget 'd["data"]["allowed"]')"
# 这条拒绝必须点名"还没接线"，而不是让管理员以为是自己填错了：
# 允许保存的话，建出来的是一条每晚失败一次、而且失败得很安静的 timer。
expect_contains "schedule-cert-blocked-kind" "SCHEDULE_KIND_UNSUPPORTED" "$(pblockers)"
expect_contains "schedule-cert-blocked-says-p12-5" "P12.5" "$(pget 'json.dumps([b["message"] for b in (d["data"].get("blockers") or [])])')"

printf '\n== 10. 备份策略也能被定时（内置动作复用任务框架）==\n'
POL=$(jget "$PREV/payloads/policy-create.json" 'd["data"]["id"]')
DEF_BACKUP=$(defn "p12-4 备份 $SUFFIX" backup "$POL" "*-*-* 02:00:00" "Asia/Shanghai" true 3600 true)
apply_schedule schedule-backup "$(create_params "$DEF_BACKUP")"
SCH_BACKUP=$(jobget 'd["data"]["result"]["data"]["schedule_id"]')
expect_eq "schedule-backup-installed" "True" "$(sched_field "$SCH_BACKUP" installed)"
expect_eq "schedule-backup-blockers-empty" "[]" "$(sched_blockers "$SCH_BACKUP")"
expect_contains "unit-backup-timeout" "TimeoutStartSec=3660s" "$(cat "$UNIT_DIR/upanel-$SCH_BACKUP.service")"
# 不触发：这一节的目的是"定义能落成单元"，真的跑一遍备份属于 P12.2 的范围。

printf '\n== 11. 代理不在时：503，不是空列表 ==\n'
kill -TERM "$AGENT_PID" 2>/dev/null; wait "$AGENT_PID" 2>/dev/null; AGENT_PID=""
CODE=$(api GET /api/v1/schedules "" schedules-down)
expect_eq "schedules-agent-down-503" "503" "$CODE"
expect_eq "schedules-agent-down-code" "AGENT_UNAVAILABLE" "$(rget 'd["error"]["code"]')"
CODE=$(api GET /api/v1/scripts "" scripts-down)
expect_eq "scripts-agent-down-503" "503" "$CODE"
expect_eq "scripts-agent-down-code" "AGENT_UNAVAILABLE" "$(rget 'd["error"]["code"]')"
CODE=$(api GET "/api/v1/schedules/$SCH" "" schedule-down)
expect_eq "schedule-detail-agent-down-503" "503" "$CODE"
# id 形状在问代理之前就拦：代理不在也不能让"参数不对"变成"读不到"。
CODE=$(api GET /api/v1/schedules/sch_nothex "" schedule-bad-id)
expect_eq "schedule-bad-id-400" "400" "$CODE"
expect_eq "schedule-bad-id-code" "VALIDATION_ERROR" "$(rget 'd["error"]["code"]')"
CODE=$(api GET /api/v1/scripts/scr_nothex "" script-bad-id)
expect_eq "script-bad-id-400" "400" "$CODE"

"$AGENT" --config "$SYS_CFG" >"$WORK/logs/agent2.log" 2>&1 & AGENT_PID=$!
for _ in $(seq 1 200); do
  if [ -S "$SOCK" ] && runuser -u "$WEB_USER" -- curl -fsS -o /dev/null --unix-socket "$SOCK" http://agent/internal/v1/capabilities 2>/dev/null; then break; fi
  sleep 0.1
done
# socket 文件存在不等于代理起来了：崩溃留下的 socket 会骗人。要问它一句。
AGENT_BACK=no
for _ in $(seq 1 200); do
  if [ -S "$SOCK" ] && runuser -u "$WEB_USER" -- curl -fsS -o /dev/null --unix-socket "$SOCK" http://agent/internal/v1/capabilities 2>/dev/null; then AGENT_BACK=yes; break; fi
  sleep 0.1
done
expect_eq "agent-restarted" "yes" "$AGENT_BACK"
CODE=$(api GET /api/v1/schedules "" schedules-up)
expect_eq "schedules-back-200" "200" "$CODE"
expect_ge "schedules-back-not-empty" "$(rget 'len(d["data"]["items"])')" 2
CODE=$(api GET "/api/v1/schedules/sch_00000000000000000000000000000000" "" schedule-unknown)
expect_eq "schedule-unknown-404" "404" "$CODE"

printf '\n== 12. 权限：viewer 读不到计划与脚本 ==\n'
CODE=$(api POST /api/v1/users "{\"username\":\"p12-4-viewer-$SUFFIX\",\"display_name\":\"P12.4 viewer\",\"role\":\"viewer\",\"initial_password\":\"$VIEWER_PASSWORD\"}" viewer-create)
expect_eq "viewer-create-201" "201" "$CODE"
VIEWER_ID=$(rget 'd["data"]["id"]')
VJAR=$WORK/viewer.cookies
CODE=$(printf '{"username":"p12-4-viewer-%s","password":"%s"}' "$SUFFIX" "$VIEWER_PASSWORD" | curl -s -c "$VJAR" -o "$TMPD/viewer-login.json" -w '%{http_code}' -X POST "$BASE/api/v1/auth/login" -H 'Content-Type: application/json' -H 'X-UPanel-Request: 1' -H "Origin: $ORIGIN" -d @-)
expect_eq "viewer-login-200" "200" "$CODE"
VCSRF=$(jget "$TMPD/viewer-login.json" 'd["data"]["csrf_token"]')
_vn=0
: >"$PAYLOADS/viewer-forbidden.txt"
for _p in /api/v1/schedules /api/v1/scripts "/api/v1/schedules/$SCH" "/api/v1/scripts/$SCRIPT_PROBE"; do
  _vn=$((_vn + 1))
  _code=$(curl -s -b "$VJAR" -o "$TMPD/viewer.json" -w '%{http_code}' "$BASE$_p" -H "Origin: $ORIGIN" -H "X-CSRF-Token: $VCSRF")
  expect_eq "viewer-forbidden-$_vn" "403" "$_code"
  printf 'GET %s -> %s\n' "$_p" "$_code" >>"$PAYLOADS/viewer-forbidden.txt"
done

printf '\n== 13. 删除：引用保护、单元清理、文件清理 ==\n'
DEL_SCRIPT=$(printf '{"script_id":"%s","version":1}' "$SCRIPT_PROBE")
expect_eq "script-delete-reauth" "200" "$(reauth)"
expect_eq "script-delete-preflight-200" "200" "$(preflight script.delete "$DEL_SCRIPT" script-delete-in-use)"
expect_eq "script-delete-blocked-while-referenced" "False" "$(pget 'd["data"]["allowed"]')"
expect_contains "script-delete-in-use-code" "SCRIPT_IN_USE" "$(pblockers)"

DEL_SCH=$(printf '{"schedule_id":"%s","version":1}' "$SCH")
apply_schedule_delete() {
  _name=$1; _params=$2
  expect_eq "$_name-reauth" "200" "$(reauth)"
  expect_eq "$_name-preflight-200" "200" "$(preflight schedule.delete "$_params" "$_name")"
  expect_eq "$_name-preflight-allowed" "True" "$(pget 'd["data"]["allowed"]')"
  PREF_CUR=$(pget 'd["data"]["preflight_id"]')
  expect_eq "$_name-submit-202" "202" "$(resubmit "$_name" schedule.delete "$_params")"
  finish_job "$_name"
  expect_eq "$_name-succeeded" "succeeded" "$JOB_STATE"
}
apply_schedule_delete schedule-delete "$DEL_SCH"
expect_eq "schedule-delete-files-gone" "0" "$([ -e "$UNIT_DIR/upanel-$SCH.timer" ] && echo 1 || echo 0)"
expect_eq "schedule-delete-service-gone" "0" "$([ -e "$UNIT_DIR/upanel-$SCH.service" ] && echo 1 || echo 0)"
expect_ne "schedule-delete-not-enabled" "enabled" "$(systemctl is-enabled "upanel-$SCH.timer" 2>/dev/null)"
CODE=$(api GET "/api/v1/schedules/$SCH" "" schedule-after-delete)
expect_eq "schedule-delete-404" "404" "$CODE"

expect_eq "script-delete-allowed-after-schedule" "200" "$(reauth)"
expect_eq "script-delete-preflight2-200" "200" "$(preflight script.delete "$DEL_SCRIPT" script-delete2)"
expect_eq "script-delete2-allowed" "True" "$(pget 'd["data"]["allowed"]')"
PREF_CUR=$(pget 'd["data"]["preflight_id"]')
expect_eq "script-delete2-submit-202" "202" "$(resubmit script-delete2 script.delete "$DEL_SCRIPT")"
finish_job script-delete2
expect_eq "script-delete2-succeeded" "succeeded" "$JOB_STATE"
expect_eq "script-delete2-file-gone" "0" "$([ -e "$PROBE_PATH" ] && echo 1 || echo 0)"

# 收尾：把这一轮建的其余计划都删掉，VM 上不留会自己跑的东西。
for _pair in "$SCH_UTC:3" "$SCH_TIMEOUT:1" "$SCH_BACKUP:1" "$SCH_OVERLAP:1"; do
  _id=${_pair%:*}; _ver=${_pair#*:}; _params=$(printf '{"schedule_id":"%s","version":%s}' "$_id" "$_ver")
  _code=$(reauth)
  _code=$(preflight schedule.delete "$_params" "cleanup-$_id")
  _allowed=$(pget 'd["data"]["allowed"]')
  if [ "$_allowed" = "True" ]; then
    PREF_CUR=$(pget 'd["data"]["preflight_id"]')
    _code=$(resubmit "cleanup-$_id" schedule.delete "$_params")
    finish_job "cleanup-$_id"
    expect_eq "cleanup-delete-$_id" "succeeded" "$JOB_STATE"
  else
    notrun "cleanup-delete-$_id" "预检不放行：$(pblockers)"
  fi
done
expect_eq "cleanup-no-managed-units" "0" "$(ls "$UNIT_DIR" 2>/dev/null | grep -c '^upanel-sch_')"

printf '\n=========================== 汇总 ===========================\n'
printf 'PASS %s / FAIL %s / NOT RUN %s\n' "$PASSED" "$FAILED" "$NOTRUN"
if [ "$FAILED" -gt 0 ]; then printf '失败项:%s\n' "$FAILED_NAMES"; fi
printf 'NOT RUN 不等于通过；剩余项见 docs/PROGRESS.md 的 P12.4 一节。\n'
printf '============================================================\n'
[ "$FAILED" -eq 0 ] || exit 1
exit 0
