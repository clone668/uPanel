#!/bin/sh
# P12.5：证书续期 timer 接入 + 备份页与计划任务页所依赖的接口形状的真机验收。
#
# 验的东西有两类，都不是"接口返回 200"：
#
#	1) **证书续期这条链路到底跑不跑得通**：面板建计划 → 代理写单元 → systemd 唤醒
#	   → 代理提交 certificate.renew → certbot 真的被调起来 → 结果与两处事实（盘上
#	   那张证书、面板账本里那一行）一致。用本机 Pebble（Let's Encrypt 的测试 ACME
#	   实现，P10.3 留下的夹具 /opt/p103/pebble）走**真实**的 ACME 流程。
#	2) **页面依赖的那些形状**：快照列表带不带大小、脚本详情是不是 `{script:…}`、
#	   每个动作"要不要再认证"是不是与代理的 ActionPolicy 一致、prune 的待删名单由
#	   预检算而不是页面拼、打进去的前端产物里有没有这些动作。这些每一条错了界面都
#	   看起来能用、实际做错事（P12.5 开发期就撞到过两次：再认证标志与 ActionPolicy
#	   不一致、`fetchScript` 形状不对）。
#
# 前置：同一台一次性 Ubuntu VM 已跑过 p12-2-backup-acceptance.sh（WORK 未删，里面有
#       管理员账号、面板库、备份仓库与策略），二进制在 BIN_DIR（agent/web/upanelctl）。
#       需要 /opt/p103/pebble（P10.3 夹具）与 /usr/bin/certbot。
# 用法：sudo BIN_DIR=/var/tmp/upanel-bin sh tests/integration/p12-5-acceptance.sh
# 退出码：0 全部通过；1 有失败；2 环境/前置证据不满足。
#
# 夹具说明（**写在明面上的取巧**）：
#   - HTTP-01 的两个口：面板签发的可达性探测打 80，Pebble 的校验打它的 httpPort
#     5002。两个口都由 `python3 -m http.server` 直接对着同一个 webroot 提供，不引
#     nginx、不改 iptables——这一轮验的是证书与计划，不是反向代理。
#   - §6/§7 要证明"到期时定时续期真的会换证书、失败时真的会留下原因"，而 Pebble 签
#     出来的证书离到期还远（certbot 的 --keep-until-expiring 因此会跳过）。所以那两节
#     把**盘上那张已存在的证书**换成一张三天后过期的自签证书（写在 live/archive 的
#     标准位置），再做一次定时续期。夹具是"一台证书快过期的服务器"这个前提，续期
#     本身是真的。
set -u

BIN_DIR=${BIN_DIR:-/var/tmp/upanel-bin}
PREV=${P12_2_WORK:-/var/tmp/upanel-accept-p12-2}
WORK=${WORK:-/var/tmp/upanel-accept-p12-5}
PORT=${PORT:-19525}
AGENT="$BIN_DIR/upanel-agent"
WEB="$BIN_DIR/upanel-web"
CTL="$BIN_DIR/upanelctl"
SYS_CFG=/etc/upanel/upanel.json
KEY_FILE=/etc/upanel/agent.key
SOCK=/run/upanel/agent.sock
WEB_USER=upanel-web
PASSWORD='p12-2-admin-horse-9'
ROOT_PATH=$PREV/data-root
AGENT_DB=$PREV/agent/agent.db
WEB_DB=$PREV/webdata/upanel.db
UNIT_DIR=/etc/systemd/system
BASE="http://127.0.0.1:$PORT"
ORIGIN=$BASE
TMPD=/tmp/p12-5.$$
SCRIPT_DIR_SRC=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
SUFFIX=$(openssl rand -hex 3)
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
PEBBLE_PID=""
WEB80_PID=""
WEB5002_PID=""
CERT_A=""
CERT_B=""
SCH_A=""
SCH_B=""
TMP_SCH=""
TMP_SCRIPT=""

# ACME 夹具
PEBBLE_BIN=${PEBBLE_BIN:-/usr/local/bin/pebble}
PEBBLE_CFG_SRC=${PEBBLE_CFG_SRC:-/opt/p103/pebble/config.json}
# Pebble 的 API 证书不沿用 /opt/p103 里那张：它的有效期是 P10.3 造夹具那天算的，
# 到今天已经过期，certbot 会死在 TLS 校验上（首轮验收就是这么红的）。这里现生成一张
# 十年的自签证书当 Pebble 的监听证书，同时当信任锚用——签发流程本身不受影响。
PEBBLE_API_CERT=$WORK/pebble/certs/cert.pem
PEBBLE_API_KEY=$WORK/pebble/certs/key.pem
ACME_URL=https://127.0.0.1:14000/dir
ACME_WEBROOT=/var/www/upanel-acme
CA_FILE=/usr/local/share/ca-certificates/p125-acme-api.crt
DOM_A="p125a-$SUFFIX.example.test"
DOM_B="p125b-$SUFFIX.example.test"
EMAIL="ops@example.test"

pass() { PASSED=$((PASSED + 1)); printf 'PASS  %s\n' "$1"; printf 'PASS\t%s\n' "$1" >>"$WORK/logs/results.txt"; }
fail() { FAILED=$((FAILED + 1)); FAILED_NAMES="$FAILED_NAMES | $1"; printf 'FAIL  %s: %s\n' "$1" "${2:-}"; printf 'FAIL\t%s\t%s\n' "$1" "${2:-}" >>"$WORK/logs/results.txt"; }
notrun() { NOTRUN=$((NOTRUN + 1)); printf 'NOT RUN  %s：%s\n' "$1" "$2"; printf 'NOTRUN\t%s\t%s\n' "$1" "$2" >>"$WORK/logs/results.txt"; }
note() { printf '      %s\n' "$1"; }
expect_eq() { if [ "$2" = "$3" ]; then pass "$1"; else fail "$1" "期望 '$2'，实际 '$3'"; fi; }
expect_ne() { if [ "$2" != "$3" ]; then pass "$1"; else fail "$1" "不应等于 '$2'"; fi; }
expect_contains() { case "$3" in *"$2"*) pass "$1" ;; *) fail "$1" "期望包含 '$2'，实际 '$3'" ;; esac; }
expect_not_contains() { case "$3" in *"$2"*) fail "$1" "不应包含 '$2'，实际 '$3'" ;; *) pass "$1" ;; esac; }
expect_file_has() { if grep -qF -- "$2" "$3" 2>/dev/null; then pass "$1"; else fail "$1" "产物 $3 里没有 '$2'"; fi; }
# expect_timestamp：断言某个取值"确实是一个时间"，而不是"非空"。
# 非空太容易满足了——jget 出错时回的是 ERR:KeyError:…，那也是非空。
expect_timestamp() {
  case "$2" in
    ''|None|ERR:*) fail "$1" "期望一个时间，实际 '$2'" ;;
    *T*Z|*T*+*) pass "$1" ;;
    *) fail "$1" "不像 RFC3339 时间：'$2'" ;;
  esac
}
live_of() { printf '/etc/letsencrypt/live/%s' "$1"; }

# wait_quiet：调用外部 ACME 客户端之前，先等这台 2 vCPU 的模拟机把上一节自己的活
# 干完。
#
# 为什么要它：产品给 certbot 的上限是 120 秒（internal/certificates/certbot.go，
# 注释写明"正常 30 秒内结束"）。在空闲机上实测一次完整签发 12.7s / 15.1s / 12.0s，
# 是上限的十分之一；但在**跑验收的时候**这台机的 1 分钟负载会到 6~8、运行队列 9 条
# （当日 sar 采样：13:40 负载 7.34、14:40 负载 8.28，两次 certbot 超时正好落在其中），
# 9 条可运行任务分 2 个 TCG 模拟核，每个进程只拿到约五分之一核，再叠上 TCG 本身的
# 数倍减速——十几秒的活就长到一百多秒，正好压在上限上。所以那不是产品缺陷，是夹具
# 自己制造的拥挤：真实机器上不会有"验收脚本在旁边跑"这件事。
#
# 等的是夹具的噪声，不是放宽判据：断言一条没动，负载不回落就照常跑并记一行。
wait_quiet() {
  _wq=0
  while [ "$_wq" -lt 18 ]; do
    _l=$(cut -d' ' -f1 /proc/loadavg 2>/dev/null)
    case "$_l" in ''|*[!0-9.]*) _l=99 ;; esac
    if awk "BEGIN{exit !($_l < 1.5)}"; then
      note "$1：1 分钟负载已回落到 $_l，继续"
      return 0
    fi
    _wq=$((_wq + 1))
    sleep 10
  done
  note "$1：等了 180 秒负载仍在 $_l，不再等（断言不变，超时就如实算失败）"
}

cleanup() {
  if [ -n "${JSON_PID:-}" ] && kill -0 "$JSON_PID" 2>/dev/null; then kill -TERM "$JSON_PID" 2>/dev/null; fi
  for _id in "$SCH_A" "$SCH_B" "$TMP_SCH"; do
    [ -n "$_id" ] || continue
    systemctl stop "upanel-$_id.timer" >/dev/null 2>&1
    systemctl disable "upanel-$_id.timer" >/dev/null 2>&1
    rm -f "$UNIT_DIR/upanel-$_id.timer" "$UNIT_DIR/upanel-$_id.service"
  done
  for _pid in "${WEB5002_PID:-}" "${WEB80_PID:-}" "${PEBBLE_PID:-}" "${WEB_PID:-}"; do
    [ -n "$_pid" ] || continue
    if kill -0 "$_pid" 2>/dev/null; then kill -TERM "$_pid" 2>/dev/null; fi
  done
  if [ -n "${AGENT_PID:-}" ] && kill -0 "$AGENT_PID" 2>/dev/null; then
    kill -TERM "$AGENT_PID" 2>/dev/null; wait "$AGENT_PID" 2>/dev/null
  fi
  systemctl daemon-reload >/dev/null 2>&1
  # 夹具回收：hosts、信任库、两个测试域名在 certbot 目录里的痕迹。
  sed -i "/ $DOM_A\$/d;/ $DOM_B\$/d" /etc/hosts 2>/dev/null
  for _d in "$DOM_A" "$DOM_B"; do
    rm -rf "$(live_of "$_d")" "/etc/letsencrypt/archive/$_d" "/etc/letsencrypt/renewal/$_d.conf"
  done
  if [ -f "$CA_FILE" ]; then rm -f "$CA_FILE"; update-ca-certificates >/dev/null 2>&1; fi
  rm -rf "$TMPD"
  return 0
}
trap cleanup EXIT
trap 'cleanup; exit 130' INT
trap 'cleanup; exit 143' TERM

if [ "$(id -u)" -ne 0 ]; then echo '必须以 root 运行' >&2; exit 2; fi
for f in "$AGENT" "$WEB" "$CTL" "$PREV/logs/results.txt" "$PREV/payloads/policy-create.json" \
         "$SCRIPT_DIR_SRC/json_helper.py" "$PEBBLE_BIN" "$PEBBLE_CFG_SRC" /usr/bin/certbot; do
  if [ ! -f "$f" ]; then echo "缺少前置文件：$f" >&2; exit 2; fi
done
if grep -q '^FAIL' "$PREV/logs/results.txt"; then echo 'P12.2 前置结果含 FAIL，先修复前置阶段' >&2; exit 2; fi
if pgrep -x upanel-agent >/dev/null 2>&1 || pgrep -x upanel-web >/dev/null 2>&1; then echo '已有 upanel 进程在运行' >&2; exit 2; fi
if ! command -v systemd-analyze >/dev/null 2>&1; then echo '缺少 systemd-analyze：本脚本无法验日历，拒绝降级' >&2; exit 2; fi

rm -rf "$WORK"
mkdir -p "$WORK/logs" "$WORK/payloads" "$TMPD" "$WORK/pebble"
chmod 1777 "$TMPD"
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
  # GET/HEAD 没有 body：第三个参数是"要不要把响应存成 payload"，不是请求体。
  # （首轮验收把它们当 body 发了，于是 list-policies.json 之类的 payload 一个都没落盘。）
  case "$1" in
    GET|HEAD) _method=$1; _path=$2; _body=""; _keep=${3:-} ;;
    *) _method=$1; _path=$2; _body=${3:-}; _keep=${4:-} ;;
  esac
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
submit_job() {
  printf '{"preflight_id":"%s","confirmation_phrase":"%s"}' "$1" "${3:-}" >"$WORK/jobreq.json"
  _code=$(curl -s -b "$JAR" -c "$JAR" -o "$WORK/resp.json" -w '%{http_code}' -X POST "$BASE/api/v1/jobs" -H 'Content-Type: application/json' -H "Origin: $ORIGIN" -H "X-CSRF-Token: $CSRF" -H "Idempotency-Key: p12-5-$2-$(openssl rand -hex 8)" -d @"$WORK/jobreq.json")
  cp "$WORK/resp.json" "$WORK/submit.json"
  cp "$WORK/resp.json" "$PAYLOADS/submit-$2.json" 2>/dev/null
  printf '%s' "$_code"
}
PREF_CUR=""
# resubmit：预检在"再认证 + 提交"之间过期时重做一次预检再提，理由与 P12.3/P12.4 同——
# 证书签发与备份都可能跑过 5 分钟的预检窗口，过期不是缺陷。
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
  while [ "$_i" -lt 3000 ]; do
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
# run_action：再认证 → 预检 → 提交 → 等终态。终态放在 JOB_STATE，同时逐条断言前三步。
run_action() {
  _name=$1; _action=$2; _params=$3
  expect_eq "$_name-reauth" "200" "$(reauth)"
  expect_eq "$_name-preflight-200" "200" "$(preflight "$_action" "$_params" "$_name")"
  expect_eq "$_name-preflight-allowed" "True" "$(pget 'd["data"]["allowed"]')"
  PREF_CUR=$(pget 'd["data"]["preflight_id"]')
  expect_eq "$_name-submit-202" "202" "$(resubmit "$_name" "$_action" "$_params")"
  finish_job "$_name"
}
defn() {
  printf '{"name":"%s","kind":"%s","target_id":"%s","calendar_expression":"%s","timezone":"%s","persistent":%s,"max_runtime_seconds":%s,"enabled":%s}' \
    "$1" "$2" "$3" "$4" "$5" "$6" "$7" "$8"
}
create_params() { printf '{"version":0,"definition":%s}' "$1"; }
sched_field() { api GET "/api/v1/schedules/$1" >/dev/null; jget "$WORK/resp.json" "d[\"data\"][\"schedule\"][\"$2\"]"; }
sched_def() { api GET "/api/v1/schedules/$1" >/dev/null; jget "$WORK/resp.json" "d[\"data\"][\"schedule\"][\"definition\"][\"$2\"]"; }
# sched_cert 读计划定义里的种类参数（certificate.domain / certificate.staging）。
# 不能拿 sched_def 拼——那个表达式按中括号拼字段名，塞不进 certificate["domain"] 这种下标。
sched_cert() { api GET "/api/v1/schedules/$1" >/dev/null; jget "$WORK/resp.json" "json.dumps(((d['data']['schedule'].get('definition') or {}).get('certificate') or {}).get('$2'))"; }
# next_elapse_of 读一条计划的下次触发时刻，最多等 N 秒。next_elapse 是代理现算的
# （systemd-analyze calendar），计划刚建好的一瞬可能还没有，所以要等。
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
# sqlq 在面板库上跑一条只读查询。
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
job_rowid_max() { _v=$(sqlq 'SELECT COALESCE(MAX(rowid),0) FROM jobs'); printf '%s' "${_v:-0}"; }
# wait_job_row 等一条**这一轮新产生**的定时触发的任务被对账导入并走到终态；返回
# "id|status|needs_sync|normalized_params|error_json"。watermark 是触发前的 MAX(rowid)，
# 少了它就会把上一节那条已经成功的行当成新的（定时触发的 action 名是一样的）。
wait_job_row() {
  python3 - "$WEB_DB" "$1" "${3:-0}" "${2:-180}" <<'PY'
import sqlite3, sys, time
db, action, watermark = sys.argv[1], sys.argv[2], int(sys.argv[3])
deadline = time.time() + float(sys.argv[4])
con = sqlite3.connect('file:%s?mode=ro' % db, uri=True, timeout=10)
last = ''
while True:
    row = con.execute(
        "SELECT j.id, j.status, j.needs_sync, p.normalized_params, j.error_json FROM jobs j "
        "JOIN preflights p ON p.consumed_job_id = j.id "
        "WHERE j.action = ? AND j.rowid > ? "
        "ORDER BY j.rowid DESC LIMIT 1", (action, watermark)).fetchone()
    if row:
        last = '\t'.join('' if c is None else str(c) for c in row)
        if row[1] in ('succeeded', 'failed', 'canceled', 'interrupted') and row[2] == 0:
            print(last); con.close(); sys.exit(0)
    if time.time() > deadline:
        print(last if last else 'timeout'); con.close(); sys.exit(0)
    time.sleep(3)
PY
}
job_row_field() { printf '%s' "$1" | cut -f"$2"; }
# job_failure_text 按账本 failureText 的规则把任务的 error_json 拼成一句话：代理给了
# code 与 message 时是 "code：message"（理由见 internal/certificates/ledger.go 的
# failureText），只有 message 时是 message。拼成带引号的形式，好直接和 last_error 逐字比。
job_failure_text() {
  python3 - "$1" <<'PY'
import json, sys
raw = sys.argv[1]
try:
    obj = json.loads(raw) if raw else {}
except Exception:
    obj = {}
code = (obj.get('code') or '').strip()
msg = (obj.get('message') or '').strip()
if code and msg:
    text = code + '：' + msg
elif msg:
    text = msg
else:
    text = code
print(json.dumps(text[:2000]))
PY
}
cert_id_of() {
  api GET "/api/v1/certificates" >/dev/null
  jget "$WORK/resp.json" "next((c['id'] for c in d['data']['items'] if c['domain']=='$1'), '')"
}
cert_field() {
  api GET "/api/v1/certificates/$2" >/dev/null
  jget "$WORK/resp.json" "json.dumps(d['data']['certificate'].get('$1') or '')"
}
wait_cert_changed() {
  _id=$1; _field=$2; _old=$3; _limit=${4:-60}; _n=0; _v=""
  while [ "$_n" -lt "$_limit" ]; do
    _v=$(cert_field "$_field" "$_id")
    case "$_v" in
      ''|"\"$_old\""|None|ERR:*) : ;;
      *) printf '%s' "$_v"; return 0 ;;
    esac
    _n=$((_n + 1)); sleep 2
  done
  printf '%s' "$_v"
}

printf '== 0. 前置环境与 ACME 夹具 ==\n'
{
  uname -a
  systemctl --version | head -2
  sha256sum "$AGENT" "$WEB" "$CTL"
  printf 'P12.2: '; tail -1 "$PREV/logs/results.txt"
  printf 'certbot: '; certbot --version 2>&1 | head -1
  printf 'pebble-cfg: '; tr -d '\n ' <"$PEBBLE_CFG_SRC"; printf '\n'
} >"$PAYLOADS/env.txt" 2>&1
pass "p12-2-prerequisite-loaded"

install -m 0755 "$CTL" /usr/local/bin/upanelctl
expect_eq "upanelctl-installed" "0" "$([ -x /usr/local/bin/upanelctl ] && echo 0 || echo 1)"

for _d in "$DOM_A" "$DOM_B"; do
  sed -i "/ $_d\$/d" /etc/hosts
  printf '127.0.0.1 %s\n' "$_d" >>/etc/hosts
done
expect_eq "hosts-points-to-loopback" "2" "$(grep -cE " $DOM_A\$| $DOM_B\$" /etc/hosts)"

mkdir -p "$ACME_WEBROOT/.well-known/acme-challenge"
chmod 0755 "$ACME_WEBROOT" "$ACME_WEBROOT/.well-known" "$ACME_WEBROOT/.well-known/acme-challenge"
printf 'p125-probe\n' >"$ACME_WEBROOT/.well-known/acme-challenge/p125-probe"
chmod 0644 "$ACME_WEBROOT/.well-known/acme-challenge/p125-probe"

# 两个口：80 给面板的探测与 ACME 服务器的 HTTP-01；Pebble 的 httpPort 是 5002，
# 让它直接连 5002，不引 iptables、不引 nginx。
# 80 与 5002 必须是空的：这两个口由下面的 http.server 现占，占不到就没有 HTTP-01
# 夹具。占不到的时候它只会在日志里留一段 traceback 然后退出，而后面每一条证书断言
# 都会以"探针不是 200"的形式连带变红——22.04 第一轮就是这样：P10 验收留下的 nginx
# 开机就占着 :80，§0 第一条红，下游 68 条跟着红，一条真缺陷都没有。环境不满足时
# 不产出假红，直接按头文件里写的"前置证据不满足"退 2。
BUSY_PORTS=""
for _p in 80 5002; do
  if ss -ltnH 2>/dev/null | awk '{print $4}' | grep -qE "[:.]$_p\$"; then
    BUSY_PORTS="$BUSY_PORTS $_p"
  fi
done
if [ -n "$BUSY_PORTS" ]; then
  printf 'NOT RUN  端口%s 已被其它进程占用，HTTP-01 夹具起不来。\n' "$BUSY_PORTS"
  printf '         停掉占用者再跑（22.04 上多半是 systemctl stop nginx）。\n'
  exit 2
fi

( cd "$ACME_WEBROOT" && exec python3 -m http.server 80 --bind 127.0.0.1 ) >"$WORK/logs/http80.log" 2>&1 &
WEB80_PID=$!
( cd "$ACME_WEBROOT" && exec python3 -m http.server 5002 --bind 127.0.0.1 ) >"$WORK/logs/http5002.log" 2>&1 &
WEB5002_PID=$!
OK80=no; OK5002=no
for _ in $(seq 1 150); do
  if [ "$(curl -s -o /dev/null -w '%{http_code}' "http://$DOM_A/.well-known/acme-challenge/p125-probe")" = "200" ]; then OK80=yes; fi
  if [ "$(curl -s -o /dev/null -w '%{http_code}' "http://$DOM_A:5002/.well-known/acme-challenge/p125-probe")" = "200" ]; then OK5002=yes; fi
  [ "$OK80" = yes ] && [ "$OK5002" = yes ] && break
  sleep 0.2
done
expect_eq "http01-webroot-port-80" "yes" "$OK80"
expect_eq "http01-webroot-port-5002" "yes" "$OK5002"

mkdir -p "$WORK/pebble/certs"
openssl req -x509 -newkey rsa:2048 -nodes -sha256 -days 3650 \
  -subj "/CN=pebble-acme-api" \
  -addext "subjectAltName=IP:127.0.0.1,DNS:localhost" \
  -addext "basicConstraints=critical,CA:TRUE" \
  -keyout "$PEBBLE_API_KEY" -out "$PEBBLE_API_CERT" >"$WORK/logs/pebble-cert.log" 2>&1
expect_eq "pebble-api-cert-made" "0" "$([ -s "$PEBBLE_API_CERT" ] && [ -s "$PEBBLE_API_KEY" ] && echo 0 || echo 1)"
python3 - "$PEBBLE_CFG_SRC" "$WORK/pebble/config.json" "$PEBBLE_API_CERT" "$PEBBLE_API_KEY" <<'PY'
import json, sys
src, dst, cert, key = sys.argv[1:5]
with open(src, encoding='utf-8') as fh:
    cfg = json.load(fh)
cfg['pebble']['certificate'] = cert
cfg['pebble']['privateKey'] = key
with open(dst, 'w', encoding='utf-8') as fh:
    json.dump(cfg, fh, indent=2)
PY
expect_eq "pebble-config-points-at-our-cert" "0" "$(grep -qF "\"$PEBBLE_API_CERT\"" "$WORK/pebble/config.json" && echo 0 || echo 1)"
"$PEBBLE_BIN" -config "$WORK/pebble/config.json" >"$WORK/logs/pebble.log" 2>&1 &
PEBBLE_PID=$!
PEBBLE_UP=no
for _ in $(seq 1 150); do
  if curl -fsS --cacert "$PEBBLE_API_CERT" "$ACME_URL" >/dev/null 2>&1; then PEBBLE_UP=yes; break; fi
  sleep 0.2
done
expect_eq "pebble-acme-directory" "yes" "$PEBBLE_UP"
# certbot 要连 Pebble 的 HTTPS API：把它的 API 证书放进系统信任库，并确认
# **不加 --cacert 也能取到目录**——那才是"信任库真的生效了"，而不是"带着那个文件
# 就通了"。
cp "$PEBBLE_API_CERT" "$CA_FILE"
update-ca-certificates >/dev/null 2>&1
TRUSTED=no
for _ in $(seq 1 30); do
  if curl -fsS "$ACME_URL" >/dev/null 2>&1; then TRUSTED=yes; break; fi
  sleep 0.2
done
expect_eq "acme-api-trusted-by-system-store" "yes" "$TRUSTED"
# certbot 用 python-requests，它的信任库是 certifi 那份。两者不是同一个文件时，
# "更新系统信任库"不等于"certbot 也信了"，签发会死在 TLS 校验上。
CERTIFI_PATH=$(python3 -c 'import certifi; print(certifi.where())' 2>/dev/null)
expect_eq "certbot-ca-bundle-is-system-store" "/etc/ssl/certs/ca-certificates.crt" "$CERTIFI_PATH"
# certbot 的 ACME 账号只存在 Pebble 的**内存**里：每一轮都是一个新 Pebble，而
# /etc/letsencrypt 里留着上一轮注册的账号，于是第一张证书就死在
# `urn:ietf:params:acme:error:accountDoesNotExist`（P10.4 在真机上撞过同一个形状，
# 见 internal/certificates/certbot_test.go 的开头）。只清掉指向这台测试 ACME 的那
# 一份账号，别的一概不碰。
ACME_HOSTPORT=$(printf '%s' "$ACME_URL" | sed 's|^[a-z]*://||; s|/.*$||')
ACCT_DIR=/etc/letsencrypt/accounts/$ACME_HOSTPORT
rm -rf "$ACCT_DIR"
expect_eq "certbot-account-cache-cleared" "0" "$([ -e "$ACCT_DIR" ] && echo 1 || echo 0)"
# 账号注册是这台机器上的**一次性事实**（真实机器上早就注册过了），把它摆在签发任务
# 之外：一次 certbot 调用的产品上限是 120 秒（internal/certificates/certbot.go），
# 而 TCG 模拟的验收机上光 python 起来一次就要几十秒——让注册挤进那个窗口，量到的
# 是机器慢，不是产品超时。
REG_LOG=$WORK/logs/certbot-register.log
if certbot register --server "$ACME_URL" --email "$EMAIL" --agree-tos \
     --non-interactive --no-eff-email >"$REG_LOG" 2>&1; then
  pass "certbot-account-registered"
else
  fail "certbot-account-registered" "$(tail -2 "$REG_LOG" | tr '\n' ' ')"
fi
expect_eq "certbot-account-on-disk" "yes" \
  "$([ -n "$(find "$ACCT_DIR" -name regr.json 2>/dev/null | head -1)" ] && echo yes || echo no)"

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
    "backup": { "panel_database_path": "$WEB_DB", "panel_copy_dir": "$PREV/panel-copies" },
    "certificates": {
      "certbot_path": "/usr/bin/certbot",
      "acme_webroot": "$ACME_WEBROOT",
      "staging_directory_url": "$ACME_URL"
    }
  },
  "database": { "path": "$WEB_DB", "busy_timeout_ms": 5000 },
  "log": { "level": "info", "format": "json" }
}
JSON
chmod 0644 "$SYS_CFG"
install -d -m 0755 /run/upanel
"$AGENT" --config "$SYS_CFG" >"$WORK/logs/agent.log" 2>&1 & AGENT_PID=$!
AGENT_READY=no
for _ in $(seq 1 300); do
  if [ -S "$SOCK" ] && runuser -u "$WEB_USER" -- curl -fsS -o /dev/null --unix-socket "$SOCK" http://agent/internal/v1/capabilities 2>/dev/null; then AGENT_READY=yes; break; fi
  kill -0 "$AGENT_PID" 2>/dev/null || break
  sleep 0.1
done
expect_eq "agent-started" "yes" "$AGENT_READY"
if [ "$AGENT_READY" != "yes" ]; then
  note "代理没能应答，日志："
  while IFS= read -r _line; do note "  $_line"; done <"$WORK/logs/agent.log"
fi
runuser -u "$WEB_USER" -- "$WEB" --mode production --db "$WEB_DB" --listen "127.0.0.1:$PORT" >"$WORK/logs/web.log" 2>&1 & WEB_PID=$!
WEB_UP=no
for _ in $(seq 1 300); do if curl -fsS "$BASE/api/v1/health/live" >/dev/null 2>&1; then WEB_UP=yes; break; fi; sleep 0.1; done
expect_eq "web-started" "yes" "$WEB_UP"
expect_eq "web-not-root" "0" "$(ps -eo uid=,comm= | awk '$2=="upanel-web" && $1==0 {n++} END {print n+0}')"

JAR=$WORK/admin.cookies
CODE=$(printf '{"username":"admin","password":"%s"}' "$PASSWORD" | curl -s -c "$JAR" -o "$PAYLOADS/login.json" -w '%{http_code}' -X POST "$BASE/api/v1/auth/login" -H 'Content-Type: application/json' -H 'X-UPanel-Request: 1' -H "Origin: $ORIGIN" -d @-)
expect_eq "login-admin" "200" "$CODE"
CSRF=$(jget "$PAYLOADS/login.json" 'd["data"]["csrf_token"]')

# 能力清单要经 agent.sock 取，而 socket 只认 web_user。uplevel 目录 root 所有、
# upanel-web 写不进去（首轮 capabilities.json 因此根本没落盘），所以先落到 1777 的
# TMPD，再由 root 拷进 payloads。
runuser -u "$WEB_USER" -- curl -s -o "$TMPD/capabilities.json" --unix-socket "$SOCK" http://agent/internal/v1/capabilities
cp "$TMPD/capabilities.json" "$PAYLOADS/capabilities.json" 2>/dev/null
for _a in certificate.issue certificate.renew backup.run backup.restore backup.verify backup.prune schedule.apply script.run; do
  expect_eq "capabilities-$_a" "True" "$(jget "$PAYLOADS/capabilities.json" "\"$_a\" in d[\"data\"][\"supported_actions\"]")"
done

printf '\n== 1. 备份页要的读链路：形状必须与页面声明一致 ==\n'
# 仓库与策略直接取 P12.2 建好的那一条（$PREV/payloads/policy-create.json 里有它当时
# 的 id）。不自己排序取 items[0]：那会变成"哪条排第一就验哪条"，换个工作区就换一套
# 断言。
REPO_ID=$(jget "$PREV/payloads/policy-create.json" 'd["data"].get("repository_id") or ""')
POLICY_ID=$(jget "$PREV/payloads/policy-create.json" 'd["data"]["id"]')
expect_contains "p12-2-policy-id" "pol_" "$POLICY_ID"
expect_eq "repositories-list-200" "200" "$(api GET /api/v1/backup/repositories list-repos)"
if [ -z "$REPO_ID" ]; then
  REPO_ID=$(rget "next(p['repository_id'] for p in d['data']['items'] if p['id']=='$POLICY_ID')")
fi
expect_contains "repository-id-shape" "rep_" "$REPO_ID"
expect_eq "repositories-has-our-repo" "True" \
  "$(rget "\"$REPO_ID\" in [r[\"id\"] for r in d[\"data\"][\"items\"]]")"
REPO_VER=$(rget "next(r['version'] for r in d['data']['items'] if r['id']=='$REPO_ID')")
expect_eq "policies-list-200" "200" "$(api GET /api/v1/backup/policies list-policies)"
expect_eq "policies-has-our-policy" "True" \
  "$(rget "\"$POLICY_ID\" in [p[\"id\"] for p in d[\"data\"][\"items\"]]")"
expect_eq "policies-binds-our-repo" "$REPO_ID" \
  "$(rget "next(p['repository_id'] for p in d['data']['items'] if p['id']=='$POLICY_ID')")"
POLICY_VER=$(rget "next(p['version'] for p in d['data']['items'] if p['id']=='$POLICY_ID')")

# 快照列表不带大小：列表不跑 restic stats（分钟级代价），页面必须点开详情才读得到。
expect_eq "snapshots-list-200" "200" "$(api GET "/api/v1/backup/snapshots?repository_id=$REPO_ID" list-snapshots)"
expect_eq "snapshots-size-included-false" "False" "$(rget 'd["data"]["size_included"]')"
expect_eq "snapshots-list-items-not-empty" "True" "$(rget 'len(d["data"]["items"]) > 0')"
expect_eq "snapshots-list-has-no-size" "True" "$(rget 'all("size_bytes" not in s and "file_count" not in s for s in d["data"]["items"])')"
expect_eq "snapshots-list-count-matches" "True" "$(rget 'd["data"]["count"] == len(d["data"]["items"])')"
expect_eq "snapshots-list-has-short-id" "True" "$(rget 'bool(d["data"]["items"][0].get("short_id"))')"
SNAP_ID=$(rget 'd["data"]["items"][0]["snapshot_id"]')
expect_eq "snapshot-detail-200" "200" "$(api GET "/api/v1/backup/snapshots/$SNAP_ID?repository_id=$REPO_ID" snapshot-detail)"
expect_eq "snapshot-detail-found" "True" "$(rget 'd["data"]["found"]')"
expect_eq "snapshot-detail-has-size" "True" "$(rget 'd["data"]["snapshot"].get("size_bytes") is not None')"

# 脚本详情是 {script: …} 信封（前端曾经按裸对象声明，编译期不吭声、运行时读不到正文）。
expect_eq "scripts-list-200" "200" "$(api GET /api/v1/scripts list-scripts)"
SCRIPT_ID=$(rget 'next((s["id"] for s in d["data"]["items"]), "")')
if [ -n "$SCRIPT_ID" ]; then
  expect_eq "scripts-list-has-no-content" "True" "$(rget 'all("content" not in s for s in d["data"]["items"])')"
  expect_eq "script-detail-envelope" "True" \
    "$(api GET "/api/v1/scripts/$SCRIPT_ID" script-detail >/dev/null; jget "$WORK/resp.json" '"script" in d["data"] and isinstance(d["data"]["script"], dict)')"
  expect_eq "script-detail-has-content" "True" \
    "$(api GET "/api/v1/scripts/$SCRIPT_ID" >/dev/null; jget "$WORK/resp.json" 'bool(d["data"]["script"].get("content"))')"
fi

printf '\n== 2. 再认证矩阵：页面的 needsReauth 必须与代理的 ActionPolicy 一致 ==\n'
# 先把两个"可数"的目标建出来：一份脚本、一条挂在它上面的计划。矩阵里 script.* 与
# schedule.delete 都需要真实 id 才能走到代理（虚构 id 会先 404，量不到策略）。
printf '{"script_id":null,"version":0,"name":"p125 探针 %s","content":"#!/bin/sh\\necho p125-probe\\n"}' "$SUFFIX" >"$TMPD/probe.json"
PROBE_BODY=$(cat "$TMPD/probe.json")
run_action m-script-apply script.apply "$PROBE_BODY"
expect_eq "m-script-apply-succeeded" "succeeded" "$JOB_STATE"
TMP_SCRIPT=$(jobget 'd["data"]["result"]["data"]["script_id"]')
TMP_SCRIPT_VER=$(jobget 'str(d["data"]["result"]["data"]["version"])')
expect_contains "m-script-id-shape" "scr_" "$TMP_SCRIPT"
MSCH_PARAMS=$(create_params "$(defn "p125 矩阵 $SUFFIX" user_script "$TMP_SCRIPT" "*-*-* 06:30:00" "UTC" false 120 true)")
run_action m-schedule-apply schedule.apply "$MSCH_PARAMS"
expect_eq "m-schedule-apply-succeeded" "succeeded" "$JOB_STATE"
TMP_SCH=$(jobget 'd["data"]["result"]["data"]["schedule_id"]')
TMP_SCH_VER=$(jobget 'str(d["data"]["result"]["data"]["version"])')
expect_contains "m-schedule-id-shape" "sch_" "$TMP_SCH"
# 有了真脚本，§1 那条没验成的形状这一下补齐（列表不带正文、详情是信封且带正文）。
expect_eq "script-detail-envelope" "True" \
  "$(api GET "/api/v1/scripts/$TMP_SCRIPT" script-detail >/dev/null; jget "$WORK/resp.json" '"script" in d["data"] and isinstance(d["data"]["script"], dict)')"
expect_eq "script-detail-has-content" "True" \
  "$(api GET "/api/v1/scripts/$TMP_SCRIPT" >/dev/null; jget "$WORK/resp.json" 'bool(d["data"]["script"].get("content"))')"

# 五个备份动作：backup.verify 是唯一不需要再认证的（只读，跑一次抽样恢复，不改仓库）。
# 期望值写 JSON 字面量（true/false）：断言比的是 json.dumps(requires_reauth)，不是 Python 的 True。
for _pair in "backup.repository.initialize:true" "backup.run:true" "backup.restore:true" "backup.prune:true" "backup.verify:false"; do
  _a=$(printf '%s' "$_pair" | cut -d: -f1)
  _want=$(printf '%s' "$_pair" | cut -d: -f2)
  case "$_a" in
    backup.repository.initialize) _p="{\"repository_id\":\"$REPO_ID\",\"version\":$REPO_VER}" ;;
    backup.run) _p="{\"backup_policy_id\":\"$POLICY_ID\",\"version\":$POLICY_VER}" ;;
    backup.restore) _p="{\"repository_id\":\"$REPO_ID\",\"snapshot_id\":\"$SNAP_ID\",\"target_root_id\":\"root1\",\"relative_path\":\"p125-restore-$SUFFIX\",\"overwrite\":false}" ;;
    backup.prune) _p="{\"backup_policy_id\":\"$POLICY_ID\",\"version\":$POLICY_VER}" ;;
    *) _p="{\"repository_id\":\"$REPO_ID\",\"snapshot_id\":\"$SNAP_ID\",\"sample_restore\":false}" ;;
  esac
  expect_eq "preflight-$_a-200" "200" "$(preflight "$_a" "$_p" "reauth-$_a")"
  expect_eq "requires-reauth-$_a" "$_want" "$(pget 'json.dumps(d["data"]["requires_reauth"])')"
done
expect_eq "preflight-schedule.apply-200" "200" "$(preflight schedule.apply "$MSCH_PARAMS" reauth-schedule-apply)"
expect_eq "requires-reauth-schedule.apply" "true" "$(pget 'json.dumps(d["data"]["requires_reauth"])')"
expect_eq "preflight-schedule.delete-200" "200" "$(preflight schedule.delete "{\"schedule_id\":\"$TMP_SCH\",\"version\":$TMP_SCH_VER}" reauth-schedule-delete)"
expect_eq "requires-reauth-schedule.delete" "true" "$(pget 'json.dumps(d["data"]["requires_reauth"])')"
expect_eq "preflight-script.apply-200" "200" "$(preflight script.apply "$PROBE_BODY" reauth-script-apply)"
expect_eq "requires-reauth-script.apply" "true" "$(pget 'json.dumps(d["data"]["requires_reauth"])')"
expect_eq "preflight-script.run-200" "200" "$(preflight script.run "{\"script_id\":\"$TMP_SCRIPT\",\"version\":$TMP_SCRIPT_VER,\"max_runtime_seconds\":60}" reauth-script-run)"
expect_eq "requires-reauth-script.run" "true" "$(pget 'json.dumps(d["data"]["requires_reauth"])')"
expect_eq "preflight-script.delete-200" "200" "$(preflight script.delete "{\"script_id\":\"$TMP_SCRIPT\",\"version\":$TMP_SCRIPT_VER}" reauth-script-delete)"
expect_eq "requires-reauth-script.delete" "true" "$(pget 'json.dumps(d["data"]["requires_reauth"])')"

# 矩阵建出来的两条要真删掉：计划先删（它引用着脚本），脚本后删。两条都走完整链路。
run_action m-cleanup-schedule schedule.delete "{\"schedule_id\":\"$TMP_SCH\",\"version\":$TMP_SCH_VER}"
expect_eq "m-cleanup-schedule-succeeded" "succeeded" "$JOB_STATE"
TMP_SCH=""
run_action m-cleanup-script script.delete "{\"script_id\":\"$TMP_SCRIPT\",\"version\":$TMP_SCRIPT_VER}"
expect_eq "m-cleanup-script-succeeded" "succeeded" "$JOB_STATE"
TMP_SCRIPT=""

printf '\n== 3. 备份动作：页面发的参数形状与 prune 的待删名单 ==\n'
RUN_PARAMS="{\"backup_policy_id\":\"$POLICY_ID\",\"version\":$POLICY_VER}"
run_action backup-run-1 backup.run "$RUN_PARAMS"
expect_eq "backup-run-1-succeeded" "succeeded" "$JOB_STATE"
expect_eq "backup-run-1-outcome-verified" '"verified"' "$(jobget 'json.dumps(d["data"]["result"].get("outcome"))')"
run_action backup-run-2 backup.run "$RUN_PARAMS"
expect_eq "backup-run-2-succeeded" "succeeded" "$JOB_STATE"

# 调用方自带 apps 必须被拒：apps 由面板按策略展开（契约 §12.2 (4)）。
expect_eq "backup-run-rejects-caller-apps-reauth" "200" "$(reauth)"
expect_eq "backup-run-rejects-caller-apps" "400" \
  "$(preflight backup.run "{\"backup_policy_id\":\"$POLICY_ID\",\"version\":$POLICY_VER,\"apps\":[\"app_x\"]}" backup-run-with-apps)"

# prune 要有东西可删才验得动"名单是预检算的"。把保留条数收到 2 条：仓库里现在至少
# 有 P12.2 留下的几条加这一轮的两条。
api GET /api/v1/backup/policies list-policies >/dev/null
PATCH_BODY=$(python3 - "$PAYLOADS/list-policies.json" "$POLICY_ID" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
p = next(i for i in d["data"]["items"] if i["id"] == sys.argv[2])
print(json.dumps({
    "name": p["name"], "repository_id": p["repository_id"], "sources": p["sources"],
    "consistency_mode": p["consistency_mode"], "app_ids": p.get("app_ids") or [],
    "retention": {"keep_last": 2}, "version": p["version"],
}, ensure_ascii=False))
PY
)
expect_eq "policy-retention-patch" "200" "$(api PATCH "/api/v1/backup/policies/$POLICY_ID" "$PATCH_BODY" policy-patch)"
POLICY_VER=$(jget "$PAYLOADS/policy-patch.json" 'd["data"]["version"]')
api GET "/api/v1/backup/snapshots?repository_id=$REPO_ID" >/dev/null
SNAP_COUNT=$(rget 'len(d["data"]["items"])')
note "仓库里现在有 $SNAP_COUNT 条快照，保留 2 条。"

expect_eq "prune-reauth" "200" "$(reauth)"
expect_eq "prune-preflight-200" "200" "$(preflight backup.prune "{\"backup_policy_id\":\"$POLICY_ID\",\"version\":$POLICY_VER}" prune)"
expect_eq "prune-preflight-allowed" "True" "$(pget 'd["data"]["allowed"]')"
PRUNE_IDS=$(pget 'json.dumps((d["data"]["normalized_params"] or {}).get("expected_delete_ids") or [])')
PRUNE_N=$(pget 'len((d["data"]["normalized_params"] or {}).get("expected_delete_ids") or [])')
note "预检算出的待删快照：$PRUNE_IDS（$PRUNE_N 条）"
expect_contains "prune-requires-phrase" "True" "$(pget 'bool(d["data"].get("confirmation_phrase"))')"
PRUNE_PHRASE=$(pget 'd["data"].get("confirmation_phrase") or ""')
PREF_CUR=$(pget 'd["data"]["preflight_id"]')
PRUNE_CODE=$(resubmit prune backup.prune "{\"backup_policy_id\":\"$POLICY_ID\",\"version\":$POLICY_VER}" "$PRUNE_PHRASE")
expect_contains "prune-submit-accepted" "202" "$PRUNE_CODE"
finish_job prune
expect_eq "prune-succeeded" "succeeded" "$JOB_STATE"
if [ "$PRUNE_N" -gt 0 ]; then
  api GET "/api/v1/backup/snapshots?repository_id=$REPO_ID" >/dev/null
  SNAP_AFTER=$(rget 'len(d["data"]["items"])')
  _gone=0
  for _sid in $(printf '%s' "$PRUNE_IDS" | tr -d '[]"' | tr ',' ' '); do
    [ -n "$_sid" ] || continue
    if [ "$(rget "\"$_sid\" in [s[\"snapshot_id\"] for s in d[\"data\"][\"items\"]]")" = "False" ]; then
      _gone=$((_gone + 1))
    fi
  done
  expect_eq "prune-deleted-what-preflight-said" "$PRUNE_N" "$_gone"
  # 快照总数减去这次删掉的就是删完剩下的：这条只验"清单真的变短了、且正好短了这么多"。
  expect_eq "prune-snapshot-list-shrank" "$((SNAP_COUNT - PRUNE_N))" "$SNAP_AFTER"
  # "保留 2 条"不能靠数仓库总条数来断言。这个仓库从 P12.1 起一直在用，历次验收留下的
  # 快照（别的策略、别的轮次）都躺在里面，总数与这条策略的保留规则无关——第五轮真机
  # 就是这么红的：期望 2、实际 16，而"该删的都删了"那条是过的。真正的判据是让**产品**
  # 再算一次：保留规则已经满足时，新的预检必须得出"没有要删的"。
  expect_eq "prune-after-preflight-200" "200" \
    "$(preflight backup.prune "{\"backup_policy_id\":\"$POLICY_ID\",\"version\":$POLICY_VER}" prune-after)"
  expect_eq "prune-left-keep-last" "0" \
    "$(pget 'len((d["data"]["normalized_params"] or {}).get("expected_delete_ids") or [])')"
else
  notrun "prune-deleted-what-preflight-said" "保留规则这次没有要删的快照（仓库里只有 $SNAP_COUNT 条）"
fi

printf '\n== 4. 证书续期计划：定义由面板按账本展开，单元由代理写 ==\n'
wait_quiet "签发之前"
expect_eq "issue-a-reauth" "200" "$(reauth)"
ISSUE_A="{\"domain\":\"$DOM_A\",\"email\":\"$EMAIL\",\"staging\":true}"
expect_eq "issue-a-preflight" "200" "$(preflight certificate.issue "$ISSUE_A" issue-a)"
expect_eq "issue-a-allowed" "True" "$(pget 'd["data"]["allowed"]')"
PREF_CUR=$(pget 'd["data"]["preflight_id"]')
expect_eq "issue-a-submit" "202" "$(resubmit issue-a certificate.issue "$ISSUE_A")"
finish_job issue-a
expect_eq "issue-a-succeeded" "succeeded" "$JOB_STATE"
if [ "$JOB_STATE" != "succeeded" ]; then
  note "签发失败原因：$(jobget 'json.dumps(d["data"].get("error") or d["data"]["result"].get("recovery_hint") or "")')"
  note "certbot 日志尾部："
  tail -5 /var/log/letsencrypt/letsencrypt.log 2>/dev/null | while IFS= read -r _l; do note "  $_l"; done
fi
expect_eq "issue-a-outcome-verified" '"verified"' "$(jobget 'json.dumps(d["data"]["result"].get("outcome"))')"
expect_contains "issue-a-issuer-is-pebble" "Pebble" "$(jobget 'json.dumps(d["data"]["result"]["observed_state"].get("issuer"))')"
A_NOT_AFTER=$(jobget 'd["data"]["result"]["observed_state"].get("not_after") or ""')
A_FP=$(jobget 'd["data"]["result"]["observed_state"].get("fingerprint") or ""')
expect_timestamp "issue-a-has-not-after" "$A_NOT_AFTER"
CERT_A=$(cert_id_of "$DOM_A")
expect_contains "ledger-a-id-shape" "crt_" "$CERT_A"
expect_eq "ledger-a-source-certbot" '"certbot"' "$(cert_field source "$CERT_A")"
expect_eq "ledger-a-staging" "true" "$(cert_field staging "$CERT_A")"
expect_eq "ledger-a-fingerprint-matches-report" "\"$A_FP\"" "$(cert_field fingerprint "$CERT_A")"

# 种类参数由面板按账本填：调用方给的域名对不上就拒绝（静默覆盖会把界面 bug
# 变成一条看起来正常的计划，而它续的是另一张证书）。
CERT_PARAMS_WRONG="{\"version\":0,\"definition\":{\"name\":\"p125-wrong-$SUFFIX\",\"kind\":\"certificate_renewal\",\"target_id\":\"$CERT_A\",\"certificate\":{\"domain\":\"other.example.test\",\"staging\":true},\"calendar_expression\":\"*-*-* 04:30:00\",\"timezone\":\"UTC\",\"persistent\":false,\"max_runtime_seconds\":600,\"enabled\":true}}"
expect_eq "schedule-cert-wrong-409-reauth" "200" "$(reauth)"
expect_eq "schedule-cert-domain-mismatch-rejected" "409" \
  "$(preflight schedule.apply "$CERT_PARAMS_WRONG" schedule-cert-wrong)"

SCHED_PARAMS=$(create_params "$(defn "p125 续期 $SUFFIX" certificate_renewal "$CERT_A" "*-*-* 04:30:00" "UTC" false 600 true)")
run_action schedule-a schedule.apply "$SCHED_PARAMS"
expect_eq "schedule-a-succeeded" "succeeded" "$JOB_STATE"
SCH_A=$(jobget 'd["data"]["result"]["data"]["schedule_id"]')
expect_contains "schedule-a-id-shape" "sch_" "$SCH_A"
expect_eq "schedule-a-kind" "certificate_renewal" "$(sched_field "$SCH_A" kind)"
expect_eq "schedule-a-cert-domain-from-ledger" "\"$DOM_A\"" "$(sched_cert "$SCH_A" domain)"
expect_eq "schedule-a-cert-staging-from-ledger" "true" "$(sched_cert "$SCH_A" staging)"
expect_eq "schedule-a-installed" "True" "$(sched_field "$SCH_A" installed)"
expect_eq "schedule-a-active" "active" "$(sched_field "$SCH_A" active_state)"
A_ELAPSE=$(next_elapse_of "$SCH_A" 30)
expect_timestamp "schedule-a-next-elapse" "$A_ELAPSE"
expect_contains "schedule-a-next-elapse-wallclock" "T04:30:00" "$A_ELAPSE"
expect_contains "schedule-a-timer-oncalendar" "OnCalendar=*-*-* 04:30:00 UTC" "$(cat "$UNIT_DIR/upanel-$SCH_A.timer" 2>/dev/null)"
# 单元里的 Exec 行必须恰好一条，且就是那条 --wait（Q379 的判据：行为而不是行文）。
expect_eq "schedule-a-single-exec-line" "1" "$(grep -c '^Exec' "$UNIT_DIR/upanel-$SCH_A.service" 2>/dev/null)"
expect_contains "schedule-a-exec-wait" "schedule-trigger --schedule-id $SCH_A --wait" \
  "$(grep '^Exec' "$UNIT_DIR/upanel-$SCH_A.service" 2>/dev/null)"

printf '\n== 5. 定时续期（还没到期）：被唤醒、真的调了 certbot、不强制重签 ==\n'
LOG_BEFORE=$(wc -c </var/log/letsencrypt/letsencrypt.log 2>/dev/null || echo 0)
# watermark 与"A 的尝试时间"都必须在**拉起单元之前**取。两件事都是"触发会改变的量"：
# 单元一跑，代理几秒内就执行完、面板下一次对账就把任务导进来并把账本写回。第六轮
# 真机就是这么红的——触发成功、certbot 参数全对，但水位是在触发之后取的，导进来的
# 那一行 rowid 落在水位以下，等它等于没等；账本的 before 也是触发之后才读的，
# 于是 before == after。（§6/§7 一直是"先取水位再触发"，所以它们没红。）
WM=$(job_rowid_max)
A_ATTEMPT_BEFORE=$(cert_field last_attempt_at "$CERT_A")
wait_quiet "定时触发之前"
expect_eq "schedule-a-unit-run" "0" "$(timeout 300 systemctl start "upanel-$SCH_A.service" >"$WORK/logs/unit-a.log" 2>&1; echo $?)"
TRIG=no
for _ in $(seq 1 60); do
  _t=$(api GET "/api/v1/schedules/$SCH_A" >/dev/null; jget "$WORK/resp.json" 'json.dumps([t["outcome"] for t in (d["data"]["triggers"] or [])])')
  case "$_t" in
    *triggered*) TRIG=yes; break ;;
  esac
  sleep 2
done
expect_eq "schedule-a-trigger-recorded" "yes" "$TRIG"
expect_eq "schedule-a-trigger-action" "certificate.renew" \
  "$(api GET "/api/v1/schedules/$SCH_A" >/dev/null; jget "$WORK/resp.json" 'next((t["action"] for t in (d["data"]["triggers"] or []) if t["outcome"]=="triggered"), "")')"

note "任务表 watermark=$WM"
JOB_ROW_A=$(wait_job_row certificate.renew 240 "$WM")
note "面板任务表里的那一行：$JOB_ROW_A"
expect_eq "schedule-a-job-imported-succeeded" "succeeded" "$(job_row_field "$JOB_ROW_A" 2)"
expect_eq "schedule-a-job-needs-sync-cleared" "0" "$(job_row_field "$JOB_ROW_A" 3)"
_A_PARAMS=$(job_row_field "$JOB_ROW_A" 4 | tr -d ' ')
expect_contains "schedule-a-job-params-scheduled-true" '"scheduled":true' "$_A_PARAMS"
expect_contains "schedule-a-job-params-domain" "$DOM_A" "$_A_PARAMS"
expect_contains "schedule-a-job-params-staging" '"staging":true' "$_A_PARAMS"
expect_not_contains "schedule-a-job-params-no-certificate-id" '"certificate_id"' "$_A_PARAMS"

# certbot 的参数：--keep-until-expiring 在，--force-renewal 不在。这是"定时续期不烧
# 速率限额"的唯一直接证据（面板与账本都只说结果，命令行才是原因）。
LOG_SEG=$(tail -c +$((LOG_BEFORE + 1)) /var/log/letsencrypt/letsencrypt.log 2>/dev/null)
expect_contains "certbot-args-keep-until-expiring" "--keep-until-expiring" "$LOG_SEG"
expect_not_contains "certbot-args-no-force-renewal" "--force-renewal" "$LOG_SEG"
expect_contains "certbot-args-our-domain" "$DOM_A" "$LOG_SEG"

# 账本：定时那次写回（last_attempt_at 前进），而证书本身没换（未到期）。
# before 在拉起单元之前就取好了（见本节开头）。
A_ATTEMPT_AFTER=$(wait_cert_changed "$CERT_A" last_attempt_at "$A_ATTEMPT_BEFORE" 120)
expect_ne "ledger-a-attempt-updated" "$A_ATTEMPT_BEFORE" "$A_ATTEMPT_AFTER"
expect_eq "ledger-a-cert-not-replaced" "\"$A_FP\"" "$(cert_field fingerprint "$CERT_A")"
expect_eq "ledger-a-not-after-unchanged" "\"$A_NOT_AFTER\"" "$(cert_field not_after "$CERT_A")"

printf '\n== 6. 定时续期（快到期）：该续就续，换的是一张真的 Pebble 证书 ==\n'
wait_quiet "签发 B 之前"
ISSUE_B="{\"domain\":\"$DOM_B\",\"email\":\"$EMAIL\",\"staging\":true}"
expect_eq "issue-b-reauth" "200" "$(reauth)"
expect_eq "issue-b-preflight" "200" "$(preflight certificate.issue "$ISSUE_B" issue-b)"
expect_eq "issue-b-allowed" "True" "$(pget 'd["data"]["allowed"]')"
PREF_CUR=$(pget 'd["data"]["preflight_id"]')
expect_eq "issue-b-submit" "202" "$(resubmit issue-b certificate.issue "$ISSUE_B")"
finish_job issue-b
expect_eq "issue-b-succeeded" "succeeded" "$JOB_STATE"
CERT_B=$(cert_id_of "$DOM_B")
expect_contains "ledger-b-id-shape" "crt_" "$CERT_B"
B_FP_BEFORE=$(cert_field fingerprint "$CERT_B")
B_NOT_AFTER_BEFORE=$(cert_field not_after "$CERT_B")

# 夹具：把盘上这张证书换成三天后过期的（"一台证书快到期的服务器"）。certbot 的
# --keep-until-expiring 按盘上那张的到期时间判"该不该续"，所以这一次它会真的续。
B_LIVE=$(live_of "$DOM_B")
B_ARCHIVE=/etc/letsencrypt/archive/$DOM_B
openssl req -x509 -newkey rsa:2048 -nodes -days 3 \
  -keyout "$TMPD/expiring.key" -out "$TMPD/expiring.crt" \
  -subj "/CN=$DOM_B" -addext "subjectAltName=DNS:$DOM_B" >/dev/null 2>&1
expect_eq "fixture-expiring-cert-made" "0" "$([ -s "$TMPD/expiring.crt" ] && echo 0 || echo 1)"
for _f in cert1.pem fullchain1.pem chain1.pem; do cp "$TMPD/expiring.crt" "$B_ARCHIVE/$_f"; done
cp "$TMPD/expiring.key" "$B_ARCHIVE/privkey1.pem"
chmod 0600 "$B_ARCHIVE/privkey1.pem"
B_EXPIRES=$(openssl x509 -in "$B_LIVE/cert.pem" -noout -enddate 2>/dev/null | cut -d= -f2)
note "盘上这张证书现在是：$B_EXPIRES（三天后过期）"

SCHED_B_PARAMS=$(create_params "$(defn "p125 续期B $SUFFIX" certificate_renewal "$CERT_B" "*-*-* 05:30:00" "UTC" false 600 true)")
run_action schedule-b schedule.apply "$SCHED_B_PARAMS"
expect_eq "schedule-b-succeeded" "succeeded" "$JOB_STATE"
SCH_B=$(jobget 'd["data"]["result"]["data"]["schedule_id"]')
LOG_BEFORE=$(wc -c </var/log/letsencrypt/letsencrypt.log 2>/dev/null || echo 0)
WM=$(job_rowid_max)
expect_eq "schedule-b-unit-run" "0" "$(timeout 400 systemctl start "upanel-$SCH_B.service" >"$WORK/logs/unit-b.log" 2>&1; echo $?)"
JOB_ROW_B=$(wait_job_row certificate.renew 400 "$WM")
note "面板任务表里的那一行：$JOB_ROW_B"
expect_eq "schedule-b-job-succeeded" "succeeded" "$(job_row_field "$JOB_ROW_B" 2)"
_B_PARAMS=$(job_row_field "$JOB_ROW_B" 4 | tr -d ' ')
expect_contains "schedule-b-job-params-domain" "$DOM_B" "$_B_PARAMS"
B_ISSUER=$(openssl x509 -in "$B_LIVE/cert.pem" -noout -issuer 2>/dev/null)
expect_contains "disk-b-cert-reissued-by-pebble" "Pebble" "$B_ISSUER"
B_NOT_AFTER_DISK=$(openssl x509 -in "$B_LIVE/cert.pem" -noout -enddate 2>/dev/null | cut -d= -f2)
expect_ne "disk-b-cert-enddate-changed" "$B_EXPIRES" "$B_NOT_AFTER_DISK"
expect_contains "certbot-args-keep-until-expiring-b" "--keep-until-expiring" "$(tail -c +$((LOG_BEFORE + 1)) /var/log/letsencrypt/letsencrypt.log 2>/dev/null)"
B_FP_AFTER=$(wait_cert_changed "$CERT_B" fingerprint "$B_FP_BEFORE" 240)
expect_ne "ledger-b-fingerprint-changed" "$B_FP_BEFORE" "$B_FP_AFTER"
B_NOT_AFTER_AFTER=$(wait_cert_changed "$CERT_B" not_after "$B_NOT_AFTER_BEFORE" 240)
expect_ne "ledger-b-not-after-changed" "$B_NOT_AFTER_BEFORE" "$B_NOT_AFTER_AFTER"

printf '\n== 7. 定时续期失败时，账本上留下的是原因（不是一句"没发生"） ==\n'
# 两件事一起做，缺一件都造不出"执行期失败"：
#   - 把 A 盘上那张证书也换成三天后到期的：不换的话 certbot 根本不会去动它，
#     --keep-until-expiring 直接跳过，任务还是成功。
#   - 停掉 ACME 目录（14000），但**保持 80 开着**：代理在预检里探的是 80，
#     80 通了预检才会放行，失败落在 certbot 真去申请那一步。若把 80 关掉，触发会
#     被判成 blocked、根本不产生任务，账本上也就没有可写的原因。
#
# 为什么停的是目录而不是校验口 5002（第 10 轮真机红过一次，六条断言全指这里）：
# 当时关掉 5002、80 照开，任务却 succeeded，盘上还换了一张新的 Pebble 证书。
# certbot 日志把原因写得很清楚——17:21:12 那次订单拿回来的授权是
#   "status":"valid","validated":"2026-10-25T17:05:50Z","expires":"2026-10-25T18:05:52Z"
# 也就是 §5 通过的那张 HTTP-01 授权被 Pebble 缓存了一小时，§7 的订单直接是 ready，
# certbot 一次校验都没做。缓存这条路存在，"关校验口"就永远造不出执行期失败。
# 停目录之后失败落在 certbot 自己的第一次请求上，走的是同一段 FailureReason 链路
# （certbot 退出码非 0 → certbotFailure → 任务 error.message → 账本 last_error）。
A_LIVE=$(live_of "$DOM_A")
A_ARCHIVE=/etc/letsencrypt/archive/$DOM_A
wait_quiet "失败场景的触发之前"
for _f in cert1.pem fullchain1.pem chain1.pem; do cp "$TMPD/expiring.crt" "$A_ARCHIVE/$_f"; done
cp "$TMPD/expiring.key" "$A_ARCHIVE/privkey1.pem"
chmod 0600 "$A_ARCHIVE/privkey1.pem"
kill -TERM "$PEBBLE_PID" 2>/dev/null; wait "$PEBBLE_PID" 2>/dev/null; PEBBLE_PID=""
expect_eq "acme-directory-down" "000" "$(curl -s -o /dev/null -w '%{http_code}' --max-time 3 "$ACME_URL")"
expect_eq "probe-port-80-still-up" "200" "$(curl -s -o /dev/null -w '%{http_code}' --max-time 3 "http://$DOM_A/.well-known/acme-challenge/p125-probe")"
WM=$(job_rowid_max)
timeout 400 systemctl start "upanel-$SCH_A.service" >"$WORK/logs/unit-a-fail.log" 2>&1
JOB_ROW_FAIL=$(wait_job_row certificate.renew 400 "$WM")
note "面板任务表里的那一行：$JOB_ROW_FAIL"
expect_eq "schedule-fail-job-failed" "failed" "$(job_row_field "$JOB_ROW_FAIL" 2)"
JOB_FAIL_MSG=$(job_failure_text "$(job_row_field "$JOB_ROW_FAIL" 5)")
expect_ne "schedule-fail-job-has-reason" '""' "$JOB_FAIL_MSG"
# 原因里必须有 certbot 自己的话，而不是那句"动作执行失败"：这是 FailureReason 那条
# 链路的真机判据（内部 300 字、压成一行）。
expect_contains "schedule-fail-job-reason-mentions-certbot" "certbot" "$JOB_FAIL_MSG"
A_LAST_ERROR=$(wait_cert_changed "$CERT_A" last_error "" 120)
expect_ne "ledger-a-last-error-written" '""' "$A_LAST_ERROR"
expect_eq "ledger-a-last-error-is-job-reason" "$JOB_FAIL_MSG" "$A_LAST_ERROR"
# 证书没被换掉：失败不该留下"账本说新证书、盘上还是旧证书"的假象。
expect_eq "ledger-a-fingerprint-after-failure" "\"$A_FP\"" "$(cert_field fingerprint "$CERT_A")"
expect_eq "disk-a-cert-still-the-expiring-one" "$(openssl x509 -in "$TMPD/expiring.crt" -noout -fingerprint -sha256 2>/dev/null | cut -d= -f2)" \
  "$(openssl x509 -in "$A_LIVE/cert.pem" -noout -fingerprint -sha256 2>/dev/null | cut -d= -f2)"
"$PEBBLE_BIN" -config "$WORK/pebble/config.json" >"$WORK/logs/pebble-restored.log" 2>&1 &
PEBBLE_PID=$!
OK_PEBBLE=no
for _ in $(seq 1 150); do
  if curl -fsS "$ACME_URL" >/dev/null 2>&1; then OK_PEBBLE=yes; break; fi
  sleep 0.2
done
expect_eq "acme-directory-restored" "yes" "$OK_PEBBLE"

printf '\n== 8. 页面产物：两个视图真的打进了二进制，且带着这一阶段的动作 ==\n'
expect_eq "ui-index-200" "200" "$(curl -s -o "$TMPD/index.html" -w '%{http_code}' "$BASE/")"
expect_contains "ui-index-has-app-root" 'id="app"' "$(cat "$TMPD/index.html")"
expect_contains "ui-index-injects-mode" "__UPANEL_MODE__" "$(cat "$TMPD/index.html")"
expect_eq "ui-route-backup-falls-back" "200" "$(curl -s -o "$TMPD/route-backup.html" -w '%{http_code}' "$BASE/backup")"
expect_contains "ui-route-backup-is-index" 'id="app"' "$(cat "$TMPD/route-backup.html")"
expect_eq "ui-route-schedules-falls-back" "200" "$(curl -s -o "$TMPD/route-schedules.html" -w '%{http_code}' "$BASE/schedules")"
expect_contains "ui-route-schedules-is-index" 'id="app"' "$(cat "$TMPD/route-schedules.html")"
CSP=$(curl -sD - -o /dev/null "$BASE/" | tr -d '\r' | grep -i '^content-security-policy:' | head -1)
expect_contains "ui-csp-header" "default-src" "$CSP"
ENTRY_JS=$(grep -oE '/assets/[A-Za-z0-9_.-]+\.js' "$TMPD/index.html" | head -1)
expect_ne "ui-entry-js-referenced" "" "$ENTRY_JS"
expect_eq "ui-entry-js-200" "200" "$(curl -s -o "$TMPD/entry.js" -w '%{http_code}' "$BASE$ENTRY_JS")"
# 把入口引用到的所有 js 取下来拼成一份再找字符串：视图是动态 chunk，动作名可能被
# 提到共享 chunk 里，只搜入口或只搜某一个 chunk 都会漏。
mkdir -p "$TMPD/uijs"
cp "$TMPD/entry.js" "$TMPD/uijs/000-entry.js"
: >"$TMPD/uijs/fetched.list"
_ui_n=0
# 两层抓取：入口引用视图 chunk，视图 chunk 又引用共享 chunk（动作名与字段名就在那一层，
# 例如 A 页面的 api 模块）。只抓一层会漏掉"backup.restore"这种被提到共享 chunk 的字。
_ui_round=0
while [ "$_ui_round" -lt 2 ]; do
  _ui_round=$((_ui_round + 1))
  _new=0
  for _p in $(grep -ohE 'assets/[A-Za-z0-9_.-]+\.js' "$TMPD/uijs"/*.js 2>/dev/null | sort -u); do
    grep -qxF "$_p" "$TMPD/uijs/fetched.list" && continue
    printf '%s\n' "$_p" >>"$TMPD/uijs/fetched.list"
    _new=$((_new + 1))
    _ui_n=$((_ui_n + 1))
    curl -s -o "$TMPD/uijs/$_ui_n.js" "$BASE/$_p"
  done
  note "第 $_ui_round 层取回 $_new 个 js"
  [ "$_new" -eq 0 ] && break
done
cat "$TMPD/uijs"/*.js >"$TMPD/uijs-all.txt" 2>/dev/null
expect_file_has "ui-bundle-has-backup-policies" "backup/policies" "$TMPD/uijs-all.txt"
expect_file_has "ui-bundle-has-backup-run" "backup.run" "$TMPD/uijs-all.txt"
expect_file_has "ui-bundle-has-backup-verify" "backup.verify" "$TMPD/uijs-all.txt"
expect_file_has "ui-bundle-has-backup-prune" "backup.prune" "$TMPD/uijs-all.txt"
expect_file_has "ui-bundle-has-backup-restore" "backup.restore" "$TMPD/uijs-all.txt"
expect_file_has "ui-bundle-has-schedule-apply" "schedule.apply" "$TMPD/uijs-all.txt"
expect_file_has "ui-bundle-has-cert-renewal-kind" "certificate_renewal" "$TMPD/uijs-all.txt"
expect_file_has "ui-bundle-has-requires-reauth" "requires_reauth" "$TMPD/uijs-all.txt"
expect_file_has "ui-bundle-has-snapshot-size" "size_included" "$TMPD/uijs-all.txt"

printf '\n== 9. 页面要的读接口都活着（admin 视角） ==\n'
# 换一张新会话再问。整轮里 §5/§6/§7 各自要等 certbot、等任务、等账本，一台 2 vCPU
# 的客户机上能跑到四十来分钟（22.04 第一轮 38.5 分钟），而登录会话有绝对有效期：
# 22.04 那一轮 §9 的五条读到的是 401——红的不是接口，是"拿四十分钟前的会话去问"。
# 这一节验的是"页面要的读接口在不在"，不是"会话能活多久"，重新登录一次才是它该
# 关心的前提。（24.04 跑 21 分钟时这五条是绿的，所以这问题只在慢机上暴露。）
JAR_NEW=$TMPD/jar9.txt
CODE9=$(printf '{"username":"admin","password":"%s"}' "$PASSWORD" | curl -s -c "$JAR_NEW" -o "$PAYLOADS/login-section9.json" -w '%{http_code}' -X POST "$BASE/api/v1/auth/login" -H 'Content-Type: application/json' -H 'X-UPanel-Request: 1' -H "Origin: $ORIGIN" -d @-)
expect_eq "ui-relogin-for-read-checks" "200" "$CODE9"
JAR=$JAR_NEW
CSRF=$(jget "$PAYLOADS/login-section9.json" 'd["data"]["csrf_token"]')
for _p in /api/v1/backup/repositories /api/v1/backup/policies /api/v1/schedules /api/v1/scripts /api/v1/certificates; do
  expect_eq "ui-read-$_p" "200" "$(api GET "$_p")"
done
expect_eq "ui-schedules-list-has-our-schedules" "True" \
  "$(api GET /api/v1/schedules >/dev/null; jget "$WORK/resp.json" 'len(d["data"]["items"]) >= 2')"

printf '\n== 小结 ==\n'
printf 'PASS %s / FAIL %s / NOT RUN %s\n' "$PASSED" "$FAILED" "$NOTRUN"
if [ "$FAILED" -gt 0 ]; then
  printf '失败的断言：%s\n' "$FAILED_NAMES"
  exit 1
fi
exit 0
