#!/bin/sh
# P12.3：真实 Web → agent → restic 的恢复、覆盖、验证、保留清理与失败处理。
# 前置：同一台一次性 Ubuntu VM 已跑过 p12-2-backup-acceptance.sh，且其 WORK 未删除。
# 本脚本复用该阶段的真实仓库、管理员和秘密，不重建一套平行实现。
#
# 用法：sudo BIN_DIR=/var/tmp/upanel-bin sh tests/integration/p12-3-recovery-acceptance.sh
# 退出码：0 全部通过；1 有失败；2 环境/前置证据不满足。
set -u

BIN_DIR=${BIN_DIR:-/var/tmp/upanel-bin}
PREV=${P12_2_WORK:-/var/tmp/upanel-accept-p12-2}
WORK=${WORK:-/var/tmp/upanel-accept-p12-3}
PORT=${PORT:-19523}
AGENT="$BIN_DIR/upanel-agent"
WEB="$BIN_DIR/upanel-web"
SYS_CFG=/etc/upanel/upanel.json
KEY_FILE=/etc/upanel/agent.key
SOCK=/run/upanel/agent.sock
WEB_USER=upanel-web
PASSWORD='p12-2-admin-horse-9'
ROOT_PATH=$PREV/data-root
AGENT_DB=$PREV/agent/agent.db
WEB_DB_DIR=$PREV/webdata
WEB_DB=$WEB_DB_DIR/upanel.db
PANEL_COPIES=$PREV/panel-copies
APP_UNIT=p12-2-backup-app.service
APP_UNIT_FILE=/etc/systemd/system/$APP_UNIT
FULL_ROOT=$PREV/full-restore-target
BASE="http://127.0.0.1:$PORT"
ORIGIN=$BASE
TMPD=/tmp/p12-3.$$
SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
SESSION_HASH=$(openssl rand -hex 32)
SUFFIX=$(openssl rand -hex 4)
SOURCE_REL=data-recovery-$SUFFIX
NEW_REL=p12-3-new-$SUFFIX
OVERWRITE_REL=p12-3-overwrite-$SUFFIX
NETWORK_REL=p12-3-network-$SUFFIX
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
FULL_MOUNTED=0

pass() { PASSED=$((PASSED + 1)); printf 'PASS  %s\n' "$1"; printf 'PASS\t%s\n' "$1" >>"$WORK/logs/results.txt"; }
fail() { FAILED=$((FAILED + 1)); FAILED_NAMES="$FAILED_NAMES | $1"; printf 'FAIL  %s: %s\n' "$1" "${2:-}"; printf 'FAIL\t%s\t%s\n' "$1" "${2:-}" >>"$WORK/logs/results.txt"; }
notrun() { NOTRUN=$((NOTRUN + 1)); printf 'NOT RUN  %s：%s\n' "$1" "$2"; printf 'NOTRUN\t%s\t%s\n' "$1" "$2" >>"$WORK/logs/results.txt"; }
note() { printf '      %s\n' "$1"; }
expect_eq() { if [ "$2" = "$3" ]; then pass "$1"; else fail "$1" "期望 '$2'，实际 '$3'"; fi; }
expect_contains() { case "$3" in *"$2"*) pass "$1" ;; *) fail "$1" "期望包含 '$2'，实际 '$3'" ;; esac; }
expect_ge() { if [ "$2" -ge "$3" ] 2>/dev/null; then pass "$1"; else fail "$1" "期望 >= $3，实际 '$2'"; fi; }
expect_gt() { if [ "$2" -gt "$3" ] 2>/dev/null; then pass "$1"; else fail "$1" "期望 > $3，实际 '$2'"; fi; }
file_sha() { sha256sum "$1" 2>/dev/null | awk '{print $1}'; }
# cookie_expiry 读 Cookie 罐里会话 Cookie 的到期时刻（epoch 秒）。
# 它必须随会话滑动而前移：只在登录时下发一次到期时间，就是"用着满 30 分钟被踢"。
cookie_expiry() { awk '$6 == "__Host-upanel_session" { print $5 }' "$JAR" 2>/dev/null; }

cleanup() {
  if [ -n "${JSON_PID:-}" ] && kill -0 "$JSON_PID" 2>/dev/null; then kill -TERM "$JSON_PID" 2>/dev/null; fi
  if [ -n "${WEB_PID:-}" ] && kill -0 "$WEB_PID" 2>/dev/null; then kill -TERM "$WEB_PID" 2>/dev/null; wait "$WEB_PID" 2>/dev/null; fi
  if [ -n "${AGENT_PID:-}" ] && kill -0 "$AGENT_PID" 2>/dev/null; then kill -TERM "$AGENT_PID" 2>/dev/null; wait "$AGENT_PID" 2>/dev/null; fi
  systemctl stop "$APP_UNIT" >/dev/null 2>&1
  rm -f "$APP_UNIT_FILE"
  systemctl daemon-reload >/dev/null 2>&1
  rm -rf "$FULL_ROOT/inode-filler" "$FULL_ROOT/filler"
  if [ "$FULL_MOUNTED" -eq 1 ]; then umount "$FULL_ROOT" >/dev/null 2>&1; FULL_MOUNTED=0; fi
  rm -f "$WORK/req.json" "$WORK/pre.json" "$WORK/jobreq.json"
  rm -rf "$TMPD"
  return 0
}
trap cleanup EXIT
trap 'cleanup; exit 130' INT
trap 'cleanup; exit 143' TERM

if [ "$(id -u)" -ne 0 ]; then echo '必须以 root 运行' >&2; exit 2; fi
for f in "$AGENT" "$WEB" "$PREV/payloads/repo-create.json" "$PREV/payloads/repo-sftp.json" "$PREV/payloads/job-files.json" "$PREV/payloads/app-create.json" "$PREV/logs/results.txt"; do
  if [ ! -f "$f" ]; then echo "缺少 P12.2 前置文件：$f" >&2; exit 2; fi
done
if ! grep -q '^FAIL' "$PREV/logs/results.txt"; then :; else echo 'P12.2 前置结果含 FAIL，先修复前置阶段' >&2; exit 2; fi
if pgrep -x upanel-agent >/dev/null 2>&1 || pgrep -x upanel-web >/dev/null 2>&1; then echo '已有 upanel 进程在运行' >&2; exit 2; fi
# 每次从干净挂载开始：上一轮被中断时旧 tmpfs 还挂在这里，静默复用会把它的残留
# 文件（甚至 inode 用量）带进本轮夹具。nr_inodes 是第 4 节故意耗尽 inode 用的。
if mountpoint -q "$FULL_ROOT"; then
  if ! umount "$FULL_ROOT"; then echo '旧 tmpfs 仍被占用，停止；不拿系统盘代替' >&2; exit 2; fi
fi
install -d -m 0755 "$FULL_ROOT"
if ! mount -t tmpfs -o size=256m,nr_inodes=32 tmpfs "$FULL_ROOT"; then echo '独立 256 MiB tmpfs 挂载失败，停止；不拿系统盘代替' >&2; exit 2; fi
FULL_MOUNTED=1
rm -rf "$WORK" "$ROOT_PATH/$NEW_REL" "$ROOT_PATH/$OVERWRITE_REL" "$ROOT_PATH/$NETWORK_REL"
mkdir -p "$WORK/logs" "$WORK/payloads" "$TMPD" "$ROOT_PATH/$SOURCE_REL"
chmod 1777 "$TMPD"
mkfifo "$JSON_IN" "$JSON_OUT"
python3 "$SCRIPT_DIR/json_helper.py" <"$JSON_IN" >"$JSON_OUT" 2>"$TMPD/json_helper.log" &
JSON_PID=$!
exec 7>"$JSON_IN" 8<"$JSON_OUT"
RESULTS=$WORK/logs/results.txt
PAYLOADS=$WORK/payloads
: >"$RESULTS"

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
  _code=$(curl -s -b "$JAR" -c "$JAR" -o "$WORK/resp.json" -w '%{http_code}' -X POST "$BASE/api/v1/jobs" -H 'Content-Type: application/json' -H "Origin: $ORIGIN" -H "X-CSRF-Token: $CSRF" -H "Idempotency-Key: p12-3-$2-$(openssl rand -hex 8)" -d @"$WORK/jobreq.json")
  cp "$WORK/resp.json" "$WORK/submit.json"
  cp "$WORK/resp.json" "$PAYLOADS/submit-$2.json" 2>/dev/null
  printf '%s' "$_code"
}

# resubmit 在“预检因代理忙而过期”时重做一次预检再提交。
#
# 代理正在收尾上一个任务（例如发布后的复核遍历）时，POST /jobs 会排在它后面；
# 等请求回来，预检窗口（约 110 秒）已经过了，得到 409 PREFLIGHT_EXPIRED。
# 这不是产品缺陷，而是"预检有时限"的正常后果；判据仍然是"有效预检的提交必须被接受"，
# 不是放宽成"什么都能提交"。重做过预检会在结果里留下 <用例名>-preflight-reissued。
# 调用前把当前预检 id 放进 PREF_CUR；参数依次是 用例名、动作、预检参数 JSON、确认短语。
PREF_CUR=""
resubmit() {
  _code=$(submit_job "$PREF_CUR" "$1" "${4:-}")
  if [ "$_code" = "409" ]; then
    _ecode=$(jget "$WORK/submit.json" 'd["error"]["code"]')
    case "$_ecode" in
      PREFLIGHT_EXPIRED|PREFLIGHT_STALE)
        cp "$WORK/submit.json" "$PAYLOADS/submit-$1-rejected.json" 2>/dev/null
        note "$1：提交时预检已失效（$_ecode），重做预检后重提。" >&2
        expect_eq "$1-preflight-reissued" "200" "$(preflight "$2" "$3" "$1-reissue")" >&2
        expect_eq "$1-preflight-reissued-allowed" "True" "$(pget 'd["data"]["allowed"]')" >&2
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
  _i=0
  _unauth=0
  while [ "$_i" -lt 1200 ]; do
    _code=$(api GET "/api/v1/jobs/$1")
    cp "$WORK/resp.json" "$WORK/job.json"
    # 401 不能被当成“还在跑”：那会让本节空转到超时，把会话失效伪装成任务慢。
    # 连续 5 次 401 直接返回 unauthenticated，让断言带着真实原因变红。
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
  _name=$1
  JOB_ID=$(job_id_of "$WORK/submit.json")
  JOB_STATE=$(wait_job "$JOB_ID")
  cp "$WORK/job.json" "$PAYLOADS/job-$_name.json" 2>/dev/null
}
run_backup() {
  _policy=$1; _version=$2; _name=$3
  expect_eq "$_name-reauth" "200" "$(reauth)"
  expect_eq "$_name-preflight-200" "200" "$(preflight backup.run "{\"backup_policy_id\":\"$_policy\",\"version\":$_version}" "$_name")"
  expect_eq "$_name-preflight-allowed" "True" "$(pget 'd["data"]["allowed"]')"
  _pref=$(pget 'd["data"]["preflight_id"]')
  PREF_CUR=$_pref
  expect_eq "$_name-submit-202" "202" "$(resubmit "$_name" backup.run "{\"backup_policy_id\":\"$_policy\",\"version\":$_version}")"
  finish_job "$_name"
  expect_eq "$_name-succeeded" "succeeded" "$JOB_STATE"
}
restore_params() { printf '{"repository_id":"%s","snapshot_id":"%s","target_root_id":"%s","relative_path":"%s","overwrite":%s}' "$1" "$2" "$3" "$4" "$5"; }
run_restore() {
  _name=$1; _repo=$2; _snap=$3; _root=$4; _rel=$5; _overwrite=$6
  expect_eq "$_name-reauth" "200" "$(reauth)"
  expect_eq "$_name-preflight-200" "200" "$(preflight backup.restore "$(restore_params "$_repo" "$_snap" "$_root" "$_rel" "$_overwrite")" "$_name")"
  expect_eq "$_name-preflight-allowed" "True" "$(pget 'd["data"]["allowed"]')"
  _pref=$(pget 'd["data"]["preflight_id"]')
  _phrase=$(pget 'd["data"].get("confirmation_phrase") or ""')
  PREF_CUR=$_pref
  expect_eq "$_name-submit-202" "202" "$(resubmit "$_name" backup.restore "$(restore_params "$_repo" "$_snap" "$_root" "$_rel" "$_overwrite")" "$_phrase")"
}

REP=$(jget "$PREV/payloads/repo-create.json" 'd["data"]["id"]')
REP_SFTP=$(jget "$PREV/payloads/repo-sftp.json" 'd["data"]["id"]')
SNAP_OTHER=$(jget "$PREV/payloads/job-files.json" 'd["data"]["result"]["data"]["snapshot_id"]')
APP_ID=$(jget "$PREV/payloads/app-create.json" 'd["data"]["id"]')

printf '== 0. 前置、版本与独立故障卷 ==\n'
{
  restic version
  systemctl --version | head -2
  sha256sum "$AGENT" "$WEB"
  printf 'P12.2: '; tail -1 "$PREV/logs/results.txt"
} >"$PAYLOADS/env.txt" 2>&1
expect_eq "full-target-is-tmpfs" "tmpfs" "$(findmnt -n -o FSTYPE -T "$FULL_ROOT")"
pass "p12-2-prerequisite-loaded"

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
      { "id": "root1", "name": "验收数据", "path": "$ROOT_PATH", "writable": true },
      { "id": "root2", "name": "独立恢复故障卷", "path": "$FULL_ROOT", "writable": true }
    ] },
    "backup": { "panel_database_path": "$WEB_DB", "panel_copy_dir": "$PANEL_COPIES" }
  },
  "database": { "path": "$WEB_DB", "busy_timeout_ms": 5000 },
  "log": { "level": "info", "format": "json" }
}
JSON
chmod 0644 "$SYS_CFG"
install -d -m 0755 /run/upanel
"$AGENT" --config "$SYS_CFG" >"$WORK/logs/agent.log" 2>&1 & AGENT_PID=$!
for _ in $(seq 1 150); do
  if [ -S "$SOCK" ] && runuser -u "$WEB_USER" -- curl -fsS -o /dev/null --unix-socket "$SOCK" http://agent/internal/v1/capabilities 2>/dev/null; then break; fi
  sleep 0.1
done
expect_eq "agent-started" "0" "$([ -S "$SOCK" ] && echo 0 || echo 1)"
runuser -u "$WEB_USER" -- "$WEB" --mode production --db "$WEB_DB" --listen "127.0.0.1:$PORT" >"$WORK/logs/web.log" 2>&1 & WEB_PID=$!
WEB_UP=no
for _ in $(seq 1 150); do if curl -fsS "$BASE/api/v1/health/live" >/dev/null 2>&1; then WEB_UP=yes; break; fi; sleep 0.1; done
expect_eq "web-started" "yes" "$WEB_UP"
expect_eq "web-not-root" "0" "$(ps -eo uid=,comm= | awk '$2=="upanel-web" && $1==0 {n++} END {print n+0}')"
JAR=$WORK/admin.cookies
CODE=$(printf '{"username":"admin","password":"%s"}' "$PASSWORD" | curl -s -c "$JAR" -o "$PAYLOADS/login.json" -w '%{http_code}' -X POST "$BASE/api/v1/auth/login" -H 'Content-Type: application/json' -H 'X-UPanel-Request: 1' -H "Origin: $ORIGIN" -d @-)
expect_eq "login-admin" "200" "$CODE"
CSRF=$(jget "$PAYLOADS/login.json" 'd["data"]["csrf_token"]')
COOKIE_EXPIRY_LOGIN=$(cookie_expiry)
if [ -n "$COOKIE_EXPIRY_LOGIN" ]; then pass "login-cookie-expiry-recorded"; else fail "login-cookie-expiry-recorded" "Cookie 罐里没有会话 Cookie"; fi
CODE=$(runuser -u "$WEB_USER" -- curl -s -o "$TMPD/capabilities.json" -w '%{http_code}' --unix-socket "$SOCK" http://agent/internal/v1/capabilities)
cp "$TMPD/capabilities.json" "$PAYLOADS/capabilities.json"
expect_eq "capabilities-200" "200" "$CODE"
expect_eq "capabilities-restore" "True" "$(jget "$PAYLOADS/capabilities.json" '"backup.restore" in d["data"]["supported_actions"]')"
expect_eq "capabilities-verify" "True" "$(jget "$PAYLOADS/capabilities.json" '"backup.verify" in d["data"]["supported_actions"]')"
expect_eq "capabilities-prune" "True" "$(jget "$PAYLOADS/capabilities.json" '"backup.prune" in d["data"]["supported_actions"]')"

printf '\n== 1. 96 MiB 真实快照：本地与 SFTP ==\n'
dd if=/dev/urandom of="$ROOT_PATH/$SOURCE_REL/random.bin" bs=1M count=96 status=none
printf 'recovery text\n' >"$ROOT_PATH/$SOURCE_REL/note.txt"
SOURCE_SHA=$(file_sha "$ROOT_PATH/$SOURCE_REL/random.bin")
CODE=$(api POST /api/v1/backup/policies "{\"name\":\"p12-3 recovery $SUFFIX\",\"repository_id\":\"$REP\",\"sources\":[{\"kind\":\"root\",\"root_id\":\"root1\",\"relative_path\":\"$SOURCE_REL\"}],\"consistency_mode\":\"files_only\",\"retention\":{\"keep_last\":1}}" policy-recovery)
expect_eq "policy-recovery-create" "201" "$CODE"
POL=$(jget "$PAYLOADS/policy-recovery.json" 'd["data"]["id"]')
CODE=$(api POST /api/v1/backup/policies "{\"name\":\"p12-3 network $SUFFIX\",\"repository_id\":\"$REP_SFTP\",\"sources\":[{\"kind\":\"root\",\"root_id\":\"root1\",\"relative_path\":\"$SOURCE_REL\"}],\"consistency_mode\":\"files_only\",\"retention\":{}}" policy-network)
expect_eq "policy-network-create" "201" "$CODE"
POL_NET=$(jget "$PAYLOADS/policy-network.json" 'd["data"]["id"]')
run_backup "$POL" 1 recovery-baseline
SNAP=$(jobget 'd["data"]["result"]["data"]["snapshot_id"]')
expect_eq "recovery-snapshot-64hex" "64" "$(printf '%s' "$SNAP" | wc -c)"
run_backup "$POL_NET" 1 network-baseline
SNAP_NET=$(jobget 'd["data"]["result"]["data"]["snapshot_id"]')
expect_eq "network-snapshot-64hex" "64" "$(printf '%s' "$SNAP_NET" | wc -c)"

printf '\n== 2. 恢复到新目录、默认覆盖保护与结构/抽样验证 ==\n'
printf 'outside must survive\n' >"$ROOT_PATH/$NEW_REL.outside"
OUTSIDE_SHA=$(file_sha "$ROOT_PATH/$NEW_REL.outside")
run_restore restore-new "$REP" "$SNAP" root1 "$NEW_REL" false
finish_job restore-new
expect_eq "restore-new-succeeded" "succeeded" "$JOB_STATE"
RESTORED_FILE=$ROOT_PATH/$NEW_REL$ROOT_PATH/$SOURCE_REL/random.bin
expect_eq "restore-new-hash" "$SOURCE_SHA" "$(file_sha "$RESTORED_FILE")"
expect_eq "restore-new-outside-unchanged" "$OUTSIDE_SHA" "$(file_sha "$ROOT_PATH/$NEW_REL.outside")"
expect_eq "restore-new-published" "True" "$(jget "$PAYLOADS/job-restore-new.json" 'd["data"]["result"]["observed_state"]["published"]')"
expect_eq "restore-new-verified" "True" "$(jget "$PAYLOADS/job-restore-new.json" 'd["data"]["result"]["observed_state"]["verified"]')"
# 到这里已经跑过十几分钟并且一直在发请求：会话 Cookie 的到期时间必须已经前移。
# 这一条直接盯 Q357——库里滑动续期、Cookie 不续发的话，下面所有节都会在 30 分钟处全 401。
expect_gt "session-cookie-slipped" "$(cookie_expiry)" "${COOKIE_EXPIRY_LOGIN:-0}"
CODE=$(preflight backup.restore "$(restore_params "$REP" "$SNAP" root1 "$NEW_REL" false)" restore-nonempty-default)
expect_eq "restore-nonempty-preflight-200" "200" "$CODE"
expect_eq "restore-nonempty-blocked" "False" "$(pget 'd["data"]["allowed"]')"
expect_contains "restore-nonempty-blocker" "BACKUP_RESTORE_TARGET_BLOCKED" "$(pblockers)"
# 调用方伪造 apps 必须被严格解码拒绝，不能借它缩小覆盖恢复的停机范围。
CODE=$(preflight backup.restore "{\"repository_id\":\"$REP\",\"snapshot_id\":\"$SNAP\",\"target_root_id\":\"root1\",\"relative_path\":\"$NEW_REL-apps\",\"overwrite\":false,\"apps\":[]}" restore-caller-apps)
expect_eq "restore-caller-apps-400" "400" "$CODE"
expect_eq "verify-structure-preflight" "200" "$(preflight backup.verify "{\"repository_id\":\"$REP\",\"snapshot_id\":\"$SNAP\",\"sample_restore\":false}" verify-structure)"
PREF=$(pget 'd["data"]["preflight_id"]')
PREF_CUR=$PREF
expect_eq "verify-structure-submit" "202" "$(resubmit verify-structure backup.verify "{\"repository_id\":\"$REP\",\"snapshot_id\":\"$SNAP\",\"sample_restore\":false}")"
finish_job verify-structure
expect_eq "verify-structure-succeeded" "succeeded" "$JOB_STATE"
expect_eq "verify-structure-not-restore-proof" "False" "$(jget "$PAYLOADS/job-verify-structure.json" 'd["data"]["result"]["observed_state"]["restore_verified"]')"
CODE=$(api GET "/api/v1/backup/snapshots/$SNAP?repository_id=$REP" "" snapshot-after-structure)
expect_eq "structure-check-no-verification-time" "None" "$(jget "$PAYLOADS/snapshot-after-structure.json" 'd["data"]["snapshot"]["restore_verified_at"]')"
expect_eq "verify-sample-preflight" "200" "$(preflight backup.verify "{\"repository_id\":\"$REP\",\"snapshot_id\":\"$SNAP\",\"sample_restore\":true}" verify-sample)"
PREF=$(pget 'd["data"]["preflight_id"]')
PREF_CUR=$PREF
expect_eq "verify-sample-submit" "202" "$(resubmit verify-sample backup.verify "{\"repository_id\":\"$REP\",\"snapshot_id\":\"$SNAP\",\"sample_restore\":true}")"
finish_job verify-sample
expect_eq "verify-sample-succeeded" "succeeded" "$JOB_STATE"
expect_eq "verify-sample-files" "1" "$(jget "$PAYLOADS/job-verify-sample.json" 'd["data"]["result"]["observed_state"]["files_checked"]')"
CODE=$(api GET "/api/v1/backup/snapshots/$SNAP?repository_id=$REP" "" snapshot-after-sample)
SAMPLE_AT=$(jget "$PAYLOADS/snapshot-after-sample.json" 'd["data"]["snapshot"]["restore_verified_at"]')
if [ "$SAMPLE_AT" != "None" ] && [ -n "$SAMPLE_AT" ]; then pass "sample-restore-persisted"; else fail "sample-restore-persisted" "$SAMPLE_AT"; fi

printf '\n== 3. 覆盖恢复：真停/拉起应用、整目录替换 ==\n'
cat >"$APP_UNIT_FILE" <<UNIT
[Unit]
Description=P12.3 覆盖恢复应用
[Service]
Type=simple
ExecStart=/bin/sh -c 'while :; do date +%%s >> $ROOT_PATH/data/heartbeat-p12-3.log; sleep 1; done'
Restart=no
UNIT
systemctl daemon-reload
systemctl start "$APP_UNIT"
expect_eq "overwrite-app-started" "active" "$(systemctl is-active "$APP_UNIT")"
ACTIVE_BEFORE=$(systemctl show -p ActiveEnterTimestamp --value "$APP_UNIT")
mkdir -p "$ROOT_PATH/$OVERWRITE_REL"
printf 'old target survives only until verified replacement\n' >"$ROOT_PATH/$OVERWRITE_REL/stale.txt"
run_restore restore-overwrite "$REP" "$SNAP" root1 "$OVERWRITE_REL" true
expect_contains "overwrite-apps-expanded" "$APP_UNIT" "$(pget 'json.dumps(d["data"]["normalized_params"].get("apps"))')"
expect_contains "overwrite-confirmation" "OVERWRITE" "$(pget 'd["data"].get("confirmation_phrase") or ""')"
finish_job restore-overwrite
expect_eq "restore-overwrite-succeeded" "succeeded" "$JOB_STATE"
expect_eq "overwrite-stale-removed" "1" "$([ ! -e "$ROOT_PATH/$OVERWRITE_REL/stale.txt" ] && echo 1 || echo 0)"
expect_eq "overwrite-restored-hash" "$SOURCE_SHA" "$(file_sha "$ROOT_PATH/$OVERWRITE_REL$ROOT_PATH/$SOURCE_REL/random.bin")"
expect_eq "overwrite-app-active" "active" "$(systemctl is-active "$APP_UNIT")"
ACTIVE_AFTER=$(systemctl show -p ActiveEnterTimestamp --value "$APP_UNIT")
if [ -n "$ACTIVE_BEFORE" ] && [ "$ACTIVE_BEFORE" != "$ACTIVE_AFTER" ]; then pass "overwrite-app-really-restarted"; else fail "overwrite-app-really-restarted" "$ACTIVE_BEFORE → $ACTIVE_AFTER"; fi
expect_eq "overwrite-apps-restored" "True" "$(jget "$PAYLOADS/job-restore-overwrite.json" 'd["data"]["result"]["observed_state"]["apps_restored"]')"

printf '\n== 4. 恢复写满独立 tmpfs：字节充足但 inode 耗尽，要求失败且不发布 ==\n'
# 不能先把卷按字节填满再预检：产品的 BACKUP_NO_SPACE 会正确拦下，恢复根本不会开始，
# 那样测不到“写到一半 ENOSPC”。inode 上限是一个预检不估算、但真实写入会撞上的
# ENOSPC 条件：字节还剩 250 MiB 左右，预检放行；restic 建到第三个节点时无 inode 可用。
# 第 0 节已用 size=256m,nr_inodes=32 挂载，这里的空闲 inode 数是确定值。
mkdir -p "$FULL_ROOT/overwrite" "$FULL_ROOT/inode-filler"
printf 'original after ENOSPC\n' >"$FULL_ROOT/overwrite/original.txt"
FULL_ORIGINAL_SHA=$(file_sha "$FULL_ROOT/overwrite/original.txt")
# root、overwrite、original.txt、inode-filler 先占 4 个 inode；再建 25 个空文件，
# 正好剩 3 个。暂存目录占 1 个，restic 能启动并建出两棵目录后再撞 ENOSPC。
_i=0
while [ "$_i" -lt 25 ]; do
  : >"$FULL_ROOT/inode-filler/node-$_i" || break
  _i=$((_i + 1))
done
df -Pk "$FULL_ROOT" >"$PAYLOADS/enospc-df-before-restore.txt"
df -Pi "$FULL_ROOT" >"$PAYLOADS/enospc-dfi-before-restore.txt"
AVAIL_LEFT_KB=$(df -Pk "$FULL_ROOT" | awk 'NR==2 {print $4}')
INODES_FREE=$(df -Pi "$FULL_ROOT" | awk 'NR==2 {print $4}')
expect_gt "enospc-volume-bytes-available" "${AVAIL_LEFT_KB:-0}" 163840
expect_eq "enospc-volume-inodes-exhausted" "3" "$INODES_FREE"

run_restore restore-enospc "$REP" "$SNAP" root2 overwrite true
JOB_ENOSPC=$(job_id_of "$WORK/submit.json")
JOB_STATE=$(wait_job "$JOB_ENOSPC")
cp "$WORK/job.json" "$PAYLOADS/job-restore-enospc.json"
expect_eq "restore-enospc-failed" "failed" "$JOB_STATE"
expect_eq "enospc-error-code" "COMMAND_FAILED" "$(jget "$PAYLOADS/job-restore-enospc.json" 'd["data"]["error"]["code"]')"
expect_contains "enospc-restic-restore-observed" "restic restore" "$(jget "$PAYLOADS/job-restore-enospc.json" 'd["data"]["error"]["message"]')"
expect_contains "enospc-no-space-reported" "no space left on device" "$(jget "$PAYLOADS/job-restore-enospc.json" 'd["data"]["error"]["message"]')"
expect_eq "enospc-original-content-preserved" "$FULL_ORIGINAL_SHA" "$(file_sha "$FULL_ROOT/overwrite/original.txt")"
expect_eq "enospc-new-tree-not-published" "1" "$([ ! -e "$FULL_ROOT/overwrite$ROOT_PATH" ] && echo 1 || echo 0)"
expect_eq "enospc-app-still-recovered" "active" "$(systemctl is-active "$APP_UNIT")"
# 失败的恢复不能把半棵暂存树留在目标父目录里。
STAGE_LEFT=$(find "$FULL_ROOT" -maxdepth 1 -name '.upanel-restore-*' 2>/dev/null | wc -l)
expect_eq "enospc-no-stage-left" "0" "$STAGE_LEFT"
rm -rf "$FULL_ROOT/inode-filler"
df -Pk "$FULL_ROOT" >"$PAYLOADS/enospc-df-after.txt"
df -Pi "$FULL_ROOT" >"$PAYLOADS/enospc-dfi-after.txt"
AVAIL_AFTER_KB=$(df -Pk "$FULL_ROOT" | awk 'NR==2 {print $4}')
if [ "${AVAIL_AFTER_KB:-0}" -gt 131072 ] 2>/dev/null; then pass "enospc-volume-restored"; else fail "enospc-volume-restored" "清理 filler 后仍只剩 ${AVAIL_AFTER_KB:-?} KiB"; fi

printf '\n== 5. SFTP 恢复中断：失败且原目录不变 ==\n'
mkdir -p "$ROOT_PATH/$NETWORK_REL"
printf 'original after network cut\n' >"$ROOT_PATH/$NETWORK_REL/original.txt"
NETWORK_ORIGINAL_SHA=$(file_sha "$ROOT_PATH/$NETWORK_REL/original.txt")
run_restore restore-network-cut "$REP_SFTP" "$SNAP_NET" root1 "$NETWORK_REL" true
JOB_NETWORK=$(job_id_of "$WORK/submit.json")
SSH_PID=""
_i=0
while [ "$_i" -lt 900 ] && [ -z "$SSH_PID" ]; do
  for _rpid in $(pgrep -x restic 2>/dev/null); do
    _args=$(tr '\000' ' ' <"/proc/$_rpid/cmdline" 2>/dev/null)
    case "$_args" in
      *"restore $SNAP_NET"*)
        for _spid in $(ps -eo pid=,ppid=,comm= | awk -v p="$_rpid" '$2==p && $3=="ssh" {print $1}'); do SSH_PID=$_spid; break; done
        ;;
    esac
    [ -n "$SSH_PID" ] && break
  done
  [ -n "$SSH_PID" ] && break
  _i=$((_i + 1)); sleep 0.1
done
if [ -n "$SSH_PID" ]; then
  printf 'restic restore ssh pid=%s\n' "$SSH_PID" >"$PAYLOADS/network-cut.txt"
  kill -TERM "$SSH_PID" 2>/dev/null
  pass "network-restore-ssh-killed"
else
  fail "network-restore-ssh-killed" "restore 结束之前没有找到它的 SFTP ssh 子进程"
fi
JOB_STATE=$(wait_job "$JOB_NETWORK")
cp "$WORK/job.json" "$PAYLOADS/job-restore-network-cut.json"
expect_eq "restore-network-cut-failed" "failed" "$JOB_STATE"
expect_eq "network-original-content-preserved" "$NETWORK_ORIGINAL_SHA" "$(file_sha "$ROOT_PATH/$NETWORK_REL/original.txt")"
expect_eq "network-new-tree-not-published" "1" "$([ ! -e "$ROOT_PATH/$NETWORK_REL$ROOT_PATH" ] && echo 1 || echo 0)"
expect_eq "network-app-still-recovered" "active" "$(systemctl is-active "$APP_UNIT")"
CODE=$(api GET "/api/v1/backup/snapshots?repository_id=$REP_SFTP" "" snapshots-sftp-after-restore-cut)
expect_eq "network-snapshot-still-readable" "True" "$(jget "$PAYLOADS/snapshots-sftp-after-restore-cut.json" 'any(s["snapshot_id"]=="'"$SNAP_NET"'" for s in d["data"]["items"])')"

printf '\n== 6. 显式保留清理：冻结集合、过期拒绝、只删本策略 ==\n'
run_backup "$POL" 1 prune-second
SNAP2=$(jobget 'd["data"]["result"]["data"]["snapshot_id"]')
expect_eq "prune-preflight-200" "200" "$(preflight backup.prune "{\"backup_policy_id\":\"$POL\",\"version\":1}" prune-stale)"
expect_eq "prune-preflight-allowed" "True" "$(pget 'd["data"]["allowed"]')"
STALE_DELETE=$(pget 'json.dumps(d["data"]["normalized_params"].get("expected_delete_ids"))')
expect_contains "prune-preflight-names-old" "$SNAP" "$STALE_DELETE"
STALE_PREF=$(pget 'd["data"]["preflight_id"]')
STALE_PHRASE=$(pget 'd["data"].get("confirmation_phrase") or ""')
run_backup "$POL" 1 prune-third
SNAP3=$(jobget 'd["data"]["result"]["data"]["snapshot_id"]')
CODE=$(submit_job "$STALE_PREF" prune-stale "$STALE_PHRASE")
expect_eq "prune-stale-submit-409" "409" "$CODE"
expect_eq "prune-stale-code" "PREFLIGHT_STALE" "$(jget "$WORK/submit.json" 'd["error"]["code"]')"
CODE=$(api GET "/api/v1/backup/snapshots?repository_id=$REP&policy_id=$POL" "" snapshots-after-stale-prune)
expect_eq "prune-stale-deleted-nothing-old" "True" "$(jget "$PAYLOADS/snapshots-after-stale-prune.json" 'any(s["snapshot_id"]=="'"$SNAP"'" for s in d["data"]["items"])')"
expect_eq "prune-stale-deleted-nothing-second" "True" "$(jget "$PAYLOADS/snapshots-after-stale-prune.json" 'any(s["snapshot_id"]=="'"$SNAP2"'" for s in d["data"]["items"])')"
expect_eq "prune-success-reauth" "200" "$(reauth)"
expect_eq "prune-success-preflight-200" "200" "$(preflight backup.prune "{\"backup_policy_id\":\"$POL\",\"version\":1}" prune-success)"
PREF=$(pget 'd["data"]["preflight_id"]')
PHRASE=$(pget 'd["data"].get("confirmation_phrase") or ""')
expect_contains "prune-confirmation-delete" "DELETE" "$PHRASE"
PREF_CUR=$PREF
expect_eq "prune-success-submit-202" "202" "$(resubmit prune-success backup.prune "{\"backup_policy_id\":\"$POL\",\"version\":1}" "$PHRASE")"
finish_job prune-success
expect_eq "prune-success-succeeded" "succeeded" "$JOB_STATE"
expect_eq "prune-readback-verified" "True" "$(jget "$PAYLOADS/job-prune-success.json" 'd["data"]["result"]["observed_state"]["verified"]')"
CODE=$(api GET "/api/v1/backup/snapshots?repository_id=$REP&policy_id=$POL" "" snapshots-after-prune)
expect_eq "prune-old-deleted" "False" "$(jget "$PAYLOADS/snapshots-after-prune.json" 'any(s["snapshot_id"]=="'"$SNAP"'" for s in d["data"]["items"])')"
expect_eq "prune-second-deleted" "False" "$(jget "$PAYLOADS/snapshots-after-prune.json" 'any(s["snapshot_id"]=="'"$SNAP2"'" for s in d["data"]["items"])')"
expect_eq "prune-newest-kept" "True" "$(jget "$PAYLOADS/snapshots-after-prune.json" 'any(s["snapshot_id"]=="'"$SNAP3"'" for s in d["data"]["items"])')"
CODE=$(api GET "/api/v1/backup/snapshots?repository_id=$REP" "" snapshots-all-after-prune)
expect_eq "prune-other-policy-kept" "True" "$(jget "$PAYLOADS/snapshots-all-after-prune.json" 'any(s["snapshot_id"]=="'"$SNAP_OTHER"'" for s in d["data"]["items"])')"
CODE=$(api GET "/api/v1/backup/snapshots?repository_id=$REP_SFTP" "" snapshots-sftp-after-prune)
expect_eq "network-repository-list-after-prune" "200" "$CODE"
expect_eq "network-repository-untouched-by-local-prune" "True" "$(jget "$PAYLOADS/snapshots-sftp-after-prune.json" 'any(s["snapshot_id"]=="'"$SNAP_NET"'" for s in d["data"]["items"])')"
# 收尾复核一次：整轮跑了将近一小时，Cookie 到期时间必须一直跟着往前推。
expect_gt "session-cookie-still-sliding" "$(cookie_expiry)" "${COOKIE_EXPIRY_LOGIN:-0}"

printf '\n== 结果 ==\n'
# 计数器在 $(...) 子 shell 里会丢：resubmit 内部那几条断言就是跑在子 shell 里的。
# 结果文件是追加写的，跨子 shell 都在，所以最终以文件为准重新数一遍。
PASSED=$(awk -F'\t' '$1 == "PASS" { n++ } END { print n + 0 }' "$RESULTS")
FAILED=$(awk -F'\t' '$1 == "FAIL" { n++ } END { print n + 0 }' "$RESULTS")
NOTRUN=$(awk -F'\t' '$1 == "NOTRUN" { n++ } END { print n + 0 }' "$RESULTS")
FAILED_NAMES=$(awk -F'\t' '$1 == "FAIL" { printf " | %s", $2 }' "$RESULTS")
printf 'PASS=%s FAIL=%s NOT RUN=%s\n' "$PASSED" "$FAILED" "$NOTRUN"
printf 'SUMMARY\tPASS=%s FAIL=%s NOT RUN=%s\n' "$PASSED" "$FAILED" "$NOTRUN" >>"$RESULTS"
if [ "$FAILED" -ne 0 ]; then printf '失败项：%s\n' "$FAILED_NAMES"; exit 1; fi
exit 0
