#!/bin/sh
# P09.5 Linux 验收脚本：应用卡片页所依赖的每一条事实——能力、状态区分、分组、收藏、
# 入口地址、探测目标、启停路径与依赖卷门禁。
#
# 这一份要证明的不是“页面能画出来”，而是下面这些**页面照着说人话**的前提在真机上成立：
#
#   1) **入口（link）没有启停按钮**：link 的 capabilities 除 open 之外全 false，
#      runtime_state / dependency_protection 都是 not_applicable。卡片的锚点来自
#      public_url，而不是“点一下再失败”。
#   2) **入口是服务器地址**：生产模式拒绝回环入口（否则浏览器打开的是访问者自己的
#      机器）；同一个地址从服务器的非回环地址真的能打开（独立证据：curl 打的是
#      服务器 LAN 地址上的那个站点，不是 127.0.0.1）。
#   3) **探测目标里填不进别的机器**：health_check 没有 host/ip/url 字段，target_id
#      受 `^(local|cntr)_…` 结构约束；把 IP、主机名、URL 填进去是 400，形状对但不存在的
#      目标是 422。探测实际请求的地址恒为 127.0.0.1（探测结论里 address/url 都在
#      回环上）——SSRF 是结构性的，不是“填完再校验”。
#   4) **容器 running 但 HTTP 500 是明确的异常**：runtime_state=running 与
#      http_state=error 同时成立，页面说“在跑，但不健康”，而不是二选一。
#   5) **重建之后旧绑定要求重新确认**：ID 变了就是看不见，三项生命周期能力全 false，
#      库里存的仍是旧 ID（不按名字自动接手）。
#   6) **卷离线禁止启动受管理依赖应用**：restart=no 的容器 + 依赖卷 = managed；
#      卷未挂载时 start 预检 allowed=false 且带 VOLUME_OFFLINE，提交得到 409
#      DEPENDENCY_BLOCKED，容器状态不变；同一时刻 stop 不被拦。
#   7) **启停真的走预检与任务**：卡片的 stop/start 按钮发出的是 service.control ——
#      脚本照同一条路走一遍（预检 →（需要时再认证）→ 任务 → 独立证据 systemctl），
#      任务结果被抓成载荷供页面回放。
#   8) **分组与收藏是页面分桶的输入**：分组按 sort_order 稳定排序、非空分组不许删
#      （422 DEPENDENCY_BLOCKED）、收藏是**当前用户自己的**（viewer 收藏之后 admin
#      看到的仍是未收藏）。
#   9) **搜索在前端**：`GET /apps?q=…` 不是契约参数，结果集不因它变化；`group_id`
#      才是服务端的过滤，形状不对按“没有这个分组”处理而不是报错。
#
# 用法（root）：
#   sudo BIN_DIR=/var/tmp/upanel-bin sh tests/integration/p09-5-apps-ui-acceptance.sh
#
# 环境变量：PORT（默认 19495）、WORK、BIN_DIR、CNT_IMAGE（默认 alpine:3.20）、
#           PORT_500（默认 18895，500 容器）、PORT_OK（默认 18896，200 容器）、
#           PORT_SITE（默认 18897，发布到服务器 LAN 地址的站点容器）。
#
# 退出码：0 全部通过；1 有失败项；2 环境不满足。
#
# 明确不覆盖（NOT RUN，原因写在这里，不在脚本里假装通过）：
#   - **浏览器里的真实渲染与点击**：VM 上没有浏览器。DOM 由 vitest 回放本脚本抓回的
#     载荷覆盖（web/src/components/realdata-p09-5.test.ts）。那是“页面吃的形状对不对”
#     的回归锁，不是浏览器验收。
#   - **入口地址在别人的浏览器里打得开**：脚本能证明这个地址是服务器的 LAN 地址、
#     从服务器自己打它返回 200、并且面板拒绝保存回环地址；但“远程浏览器 → 反代 → 站点”
#     这条链取决于部署（DNS、证书、反代），本机验不了。
#   - **依赖卷在运行时失联后自动停止应用**：CONTRACTS §8.4(7) 尚未实现，不验不存在的东西。
#   - **分组拖动排序的拖拽交互**：只验 sort_order 这个契约字段与它的排序结果。
#
# **本脚本不销毁代理数据库**。/var/lib/upanel-agent/agent.db 里存着受管理卷的登记与
# fstab 托管块的对应关系。依赖卷那一段用“本次新建的夹具卷”，收尾时退出管理并还原
# fstab；没有可用分区时整段标 NOT RUN。

set -u

BIN_DIR=${BIN_DIR:-/var/tmp/upanel-bin}
HERE=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)

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
if ! command -v docker >/dev/null 2>&1; then
  printf '本机没有 docker，P09.5 的容器相关验收无法进行\n' >&2
  exit 2
fi

WORK=${WORK:-/var/tmp/upanel-accept-p09-5}
PAYLOADS=$WORK/payloads
FIXDIR=$WORK/fixtures
RUN_DIR=/run/upanel
SOCK=$RUN_DIR/agent.sock
SYS_CFG=/etc/upanel/upanel.json
AGENT_DB_DIR=/var/lib/upanel-agent
AGENT_DB=$AGENT_DB_DIR/agent.db
KEY_FILE=/etc/upanel/agent.key
WEB_DB_DIR=$WORK/webdata
WEB_DB=$WEB_DB_DIR/upanel.db
PORT=${PORT:-19495}
PORT_500=${PORT_500:-18895}
PORT_OK=${PORT_OK:-18896}
PORT_SITE=${PORT_SITE:-18897}
BASE="http://127.0.0.1:$PORT"
ORIGIN="$BASE"
WEB_USER=upanel-web
PASSWORD='vm-correct-horse-9'
VIEWER_PASSWORD='vm-viewer-horse-4'

CNT_IMAGE=${CNT_IMAGE:-alpine:3.20}
# 夹具名字全部带 upn-p095- 前缀，收尾时按名字删干净。
NAME_LOGS=upn-p095-logs
NAME_OKHTTP=upn-p095-ok
NAME_500=upn-p095-500
NAME_SITE=upn-p095-site
NAME_GATED=upn-p095-gated
NAME_ALWAYS=upn-p095-always
NAME_REBUILD=upn-p095-rebuild

# systemd 夹具单元：会周期性写日志，这样 journal 载荷不是空页；也不是受保护单元
# （受保护单元的能力集合按契约就是空的，拿它验“运行中的单元有 stop 能力”只会验到保护规则）。
FIXTURE_UNIT=upn-p095-svc.service
FIXTURE_UNIT_PATH=/etc/systemd/system/$FIXTURE_UNIT

APPS=/api/v1/apps
GROUPS=/api/v1/app-groups
USERS=/api/v1/users
SERVICES_PATH=/api/v1/services
VOLUMES_PATH=/api/v1/storage/volumes
PREFLIGHT_PATH=/api/v1/operations/preflight
JOBS_PATH=/api/v1/jobs

AGENT_LOG=$WORK/logs/agent.log
WEB_LOG=$WORK/logs/web.log
AGENT_PID=""
WEB_PID=""
WEB_RUNNING=0

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

# jget 用 python3 读 JSON：这台机器上没有 jq（与 P05—P09.4 的取舍一致）。
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

# cap <载荷名>：把当前响应体抓成页面回放用的夹具。
cap() { cp "$WORK/resp.json" "$FIXDIR/$1.json" 2>/dev/null; }

cleanup() {
  for _pid in "${WEB_PID:-}" "${AGENT_PID:-}"; do
    if [ -n "$_pid" ] && kill -0 "$_pid" 2>/dev/null; then
      kill -TERM "$_pid" 2>/dev/null
      wait "$_pid" 2>/dev/null
    fi
  done
  # 容器都是脚本自己造的夹具，收尾时删掉：不留在机器上。
  docker rm -f "$NAME_LOGS" "$NAME_OKHTTP" "$NAME_500" "$NAME_SITE" "$NAME_GATED" \
    "$NAME_ALWAYS" "$NAME_REBUILD" >/dev/null 2>&1 || true
  if [ -f "$FIXTURE_UNIT_PATH" ]; then
    systemctl disable --now "$FIXTURE_UNIT" >/dev/null 2>&1 || true
    rm -f "$FIXTURE_UNIT_PATH"
    systemctl daemon-reload >/dev/null 2>&1 || true
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
    if curl -fsS "$BASE/api/v1/health/live" >/dev/null 2>&1; then WEB_RUNNING=1; return 0; fi
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

# api <jar> <csrf> <方法> <路径> [body] [幂等键]：响应体落在 $WORK/resp.json。
api() {
  _jar=$1; _csrf=$2; _method=$3; _path=$4; _body=${5:-}; _key=${6:-}
  if [ -n "$_body" ]; then
    printf '%s' "$_body" >"$WORK/req.json"
    if [ -n "$_key" ]; then
      curl -s -b "$_jar" -o "$WORK/resp.json" -w '%{http_code}' -X "$_method" "$BASE$_path" \
        -H 'Content-Type: application/json' -H "Origin: $ORIGIN" -H "X-CSRF-Token: $_csrf" \
        -H "Idempotency-Key: $_key" -d @"$WORK/req.json"
    else
      curl -s -b "$_jar" -o "$WORK/resp.json" -w '%{http_code}' -X "$_method" "$BASE$_path" \
        -H 'Content-Type: application/json' -H "Origin: $ORIGIN" -H "X-CSRF-Token: $_csrf" \
        -d @"$WORK/req.json"
    fi
  else
    curl -s -b "$_jar" -o "$WORK/resp.json" -w '%{http_code}' -X "$_method" "$BASE$_path" \
      -H "Origin: $ORIGIN" -H "X-CSRF-Token: $_csrf"
  fi
}

api_anon() {
  curl -s -o "$WORK/resp.json" -w '%{http_code}' -X "$1" "$BASE$2" -H "Origin: $ORIGIN"
}

ecode() { jget "$WORK/resp.json" 'd["error"]["code"]'; }
emsg() { jget "$WORK/resp.json" 'd["error"]["message"]'; }
dget() { jget "$WORK/resp.json" "d[\"data\"]$1"; }

# create_app <载荷名> <body>：回显状态码；新建出来的标识写进 $WORK/lastapp。
#
# 标识**不能**在函数里赋给一个变量再往外传：调用点用了 $(create_app …)，命令替换
# 跑在子壳里，函数里设的变量出了子壳就没了。文件是这里唯一诚实的交接方式。
create_app() {
  _code=$(api "$JAR" "$CSRF" POST "$APPS" "$2")
  cp "$WORK/resp.json" "$FIXDIR/app-$1.json" 2>/dev/null
  cp "$WORK/resp.json" "$PAYLOADS/app-$1.json" 2>/dev/null
  jget "$FIXDIR/app-$1.json" 'd["data"]["id"]' >"$WORK/lastapp"
  printf '%s' "$_code"
}
app_id() { cat "$WORK/lastapp"; }

# app_version <app_id>：此刻的 version。PATCH 的乐观锁要的是**现在**的版本号。
app_version() {
  api "$JAR" "$CSRF" GET "$APPS/$1" >/dev/null
  jget "$WORK/resp.json" 'd["data"]["version"]'
}

# apppatch <载荷名> <app_id> <除 version 之外的字段片段>：自动带上当前 version。
apppatch() {
  _v=$(app_version "$2")
  _code=$(api "$JAR" "$CSRF" PATCH "$APPS/$2" "{\"version\":$_v${3:+,$3}}")
  cp "$WORK/resp.json" "$FIXDIR/app-$1.json" 2>/dev/null
  printf '%s' "$_code"
}

# appget <载荷名> <app_id> <字段路径>：重新读一次应用，字段从最新载荷取。
appget() {
  api "$JAR" "$CSRF" GET "$APPS/$2" >/dev/null
  cp "$WORK/resp.json" "$FIXDIR/app-$1.json" 2>/dev/null
  jget "$FIXDIR/app-$1.json" "d[\"data\"]$3"
}

# appexpr <载荷名> <app_id> <整表达式>：appget 只能接字段后缀（它把 "$3" 拼在 d["data"]
# 后面），要给一整个表达式（`a and not b`）就必须走这个入口，否则拼出来是语法错误。
appexpr() {
  api "$JAR" "$CSRF" GET "$APPS/$2" >/dev/null
  cp "$WORK/resp.json" "$FIXDIR/app-$1.json" 2>/dev/null
  jget "$FIXDIR/app-$1.json" "$3"
}

# wait_health <app_id> <期望状态> <超时秒>：轮询健康结论直到状态变成期望（或超时）。
wait_health() {
  _id=$1; _want=$2; _limit=$(( ${3:-60} * 2 )); _i=0; _state=unknown
  while [ "$_i" -lt "$_limit" ]; do
    api "$JAR" "$CSRF" GET "$APPS/$_id/health" >/dev/null
    cp "$WORK/resp.json" "$WORK/health.json"
    _state=$(jget "$WORK/health.json" 'd["data"]["http_state"]')
    [ "$_state" = "$_want" ] && { printf '%s' "$_state"; return 0; }
    _i=$((_i + 1))
    sleep 0.5
  done
  printf '%s' "${_state:-unknown}"
  return 1
}

# wait_unit <单元名> <超时秒>：等服务快照里出现这个单元（采集是快照 + 缓存）。
wait_unit() {
  _unit=$1; _limit=$(( ${2:-60} * 2 )); _i=0; _seen=False
  while [ "$_i" -lt "$_limit" ]; do
    api "$JAR" "$CSRF" GET "$SERVICES_PATH" >/dev/null
    cp "$WORK/resp.json" "$WORK/services.json"
    _seen=$(jget "$WORK/services.json" "any(u['unit']=='$_unit' for u in (d['data']['units'] or []))")
    [ "$_seen" = "True" ] && { printf 'True'; return 0; }
    _i=$((_i + 1))
    sleep 0.5
  done
  printf '%s' "${_seen:-False}"
  return 1
}

# wait_app <载荷名> <app_id> <整表达式> <超时秒>：等服务快照追上再断言。
#
# 应用摘要里的 runtime_state 来自 services 采集器的快照，默认 10 秒一轮。刚发完
# service.control，摘要在下一轮采集之前仍然是旧的——这不是缓存出错，是「摘要说的是
# 我读到的那一轮事实」。断言“停机之后卡片上出现 start”必须等那一轮，否则它测的是
# 采集器的刷新频率，不是卡片的规则。读到就立刻返回，不做无条件等待。
wait_app() {
  _name=$1; _id=$2; _expr=$3; _limit=$(( ${4:-60} * 2 )); _i=0; _ok=False
  while [ "$_i" -lt "$_limit" ]; do
    _ok=$(appexpr "$_name" "$_id" "$_expr" 2>/dev/null || printf 'False')
    [ "$_ok" = "True" ] && { printf 'True'; return 0; }
    _i=$((_i + 1))
    sleep 0.5
  done
  printf '%s' "${_ok:-False}"
  return 1
}

# preflight_action <动作> <参数 JSON> <载荷名>：预检响应存成 preflight-<名>.json，回显状态码。
preflight_action() {
  printf '{"action":"%s","params":%s}' "$1" "$2" >"$WORK/pre.json"
  _code=$(curl -s -b "$JAR" -o "$WORK/preflight.json" -w '%{http_code}' \
    -X POST "$BASE$PREFLIGHT_PATH" \
    -H 'Content-Type: application/json' -H "Origin: $ORIGIN" -H "X-CSRF-Token: $CSRF" \
    -d @"$WORK/pre.json")
  cp "$WORK/preflight.json" "$FIXDIR/preflight-$3.json" 2>/dev/null
  cp "$WORK/preflight.json" "$PAYLOADS/preflight-$3.json" 2>/dev/null
  printf '%s' "$_code"
}

# submit_action <载荷名> <preflight_id> <确认短语>：短语为空时不发这个字段。
submit_action() {
  python3 -c 'import json,sys
d={"preflight_id":sys.argv[1]}
if sys.argv[2]:
    d["confirmation_phrase"]=sys.argv[2]
print(json.dumps(d))' "$2" "${3:-}" >"$WORK/jobreq.json"
  _code=$(curl -s -b "$JAR" -o "$WORK/resp.json" -w '%{http_code}' -X POST "$BASE$JOBS_PATH" \
    -H 'Content-Type: application/json' -H "Origin: $ORIGIN" -H "X-CSRF-Token: $CSRF" \
    -H "Idempotency-Key: p09-5-$1-$$" -d @"$WORK/jobreq.json")
  cp "$WORK/resp.json" "$PAYLOADS/submit-$1.json" 2>/dev/null
  printf '%s' "$_code"
}

# wait_job <job_id>：轮询到终态，任务视图存 $WORK/job.json，回显终态或 timeout。
wait_job() {
  _i=0
  while [ "$_i" -lt 300 ]; do
    api "$JAR" "$CSRF" GET "$JOBS_PATH/$1" >/dev/null
    cp "$WORK/resp.json" "$WORK/job.json"
    _st=$(jget "$WORK/job.json" 'd["data"]["status"]')
    case "$_st" in
      succeeded|failed|canceled|interrupted) printf '%s' "$_st"; return 0 ;;
    esac
    _i=$((_i + 1))
    sleep 0.2
  done
  printf 'timeout'
}

# run_action <动作> <参数 JSON> <载荷名>：预检 →（需再认证就先再认证）→ 提交 → 等终态。
# 结果放在全局变量里（不放进 $( )：那会开子壳，变量带不回来）。
PRE_CODE=""; PRE_ALLOWED=""; PRE_ID=""; PRE_PHRASE=""; PRE_REAUTH=""
JOB_CODE=""; JOB_ID=""; JOB_STATE=""
run_action() {
  PRE_CODE=$(preflight_action "$1" "$2" "$3")
  PRE_ALLOWED=$(jget "$PAYLOADS/preflight-$3.json" 'd["data"]["allowed"]')
  PRE_ID=$(jget "$PAYLOADS/preflight-$3.json" 'd["data"]["preflight_id"]')
  PRE_PHRASE=$(jget "$PAYLOADS/preflight-$3.json" 'd["data"].get("confirmation_phrase") or ""')
  PRE_REAUTH=$(jget "$PAYLOADS/preflight-$3.json" 'bool(d["data"].get("requires_reauth"))')
  if [ "$PRE_REAUTH" = "True" ]; then
    if [ "$(api "$JAR" "$CSRF" POST /api/v1/auth/reauth "{\"password\":\"$PASSWORD\"}")" != "200" ]; then
      fail "再认证（$3）" "$(emsg)"
    fi
  fi
  JOB_CODE=$(submit_action "$3" "$PRE_ID" "$PRE_PHRASE")
  if [ "$JOB_CODE" = "202" ]; then
    JOB_ID=$(jget "$PAYLOADS/submit-$3.json" 'd["data"]["job_id"]')
    JOB_STATE=$(wait_job "$JOB_ID")
    cp "$WORK/job.json" "$FIXDIR/job-$3.json" 2>/dev/null
  else
    JOB_ID=""
    JOB_STATE="submit:$JOB_CODE"
  fi
}
pfget() { jget "$FIXDIR/preflight-$1.json" "d[\"data\"]$2"; }
pfexpr() { jget "$FIXDIR/preflight-$1.json" "$2"; }
jobdata() { jget "$WORK/job.json" "d[\"data\"][\"result\"][\"data\"].get(\"$1\")"; }
jobobserved() { jget "$WORK/job.json" "d[\"data\"][\"result\"][\"observed_state\"].get(\"$1\")"; }
jobverified() { jget "$WORK/job.json" 'bool(d["data"]["result"].get("verified_at"))'; }

# cstate <容器名>：docker 自己报告的容器状态（独立证据，不读产品的解析结果）。
cstate() { docker inspect -f '{{.State.Status}}' "$1" 2>/dev/null || printf 'missing'; }

# vfield <卷标识> <字段>：从最近一次卷列表里取字段。
volrefresh() {
  api "$JAR" "$CSRF" GET "$VOLUMES_PATH" >/dev/null
  cp "$WORK/resp.json" "$WORK/volumes.json"
}
vfield() {
  python3 - "$WORK/volumes.json" "$1" "$2" <<'PY' 2>/dev/null
import json, sys
d = json.load(open(sys.argv[1], encoding="utf-8"))
vid, field = sys.argv[2], sys.argv[3]
for v in (d.get("data") or {}).get("volumes") or []:
    if v.get("id") == vid:
        print(v.get(field) if v.get(field) is not None else "")
        raise SystemExit
print("missing")
PY
}
volstate() {
  python3 - "$WORK/volumes.json" "$1" <<'PY' 2>/dev/null
import json, sys
d = json.load(open(sys.argv[1], encoding="utf-8"))
for v in (d.get("data") or {}).get("volumes") or []:
    if v.get("id") == sys.argv[2]:
        print(v.get("observed_state") or "unknown")
        raise SystemExit
print("missing")
PY
}

# pick_volume_device：挑一块“可以拿来登记成受管理卷”的分区。
# lsblk 只用 -l -n -p：util-linux 2.39 的 -r（raw）与 -l（list）互斥，两个一起给会直接报错。
pick_volume_device() {
  if [ -n "${VOL_DEV:-}" ]; then printf '%s' "$VOL_DEV"; return 0; fi
  _fstab_uuids=$(grep -o 'UUID=[0-9a-fA-F-]*' /etc/fstab 2>/dev/null | cut -d= -f2 | tr '\n' ' ')
  for _dev in $(lsblk -l -n -p -o NAME,TYPE,FSTYPE,MOUNTPOINT 2>/dev/null \
                 | awk '$2=="part" && $3=="ext4" && $4==""{print $1}'); do
    _uuid=$(blkid -p -o value -s UUID "$_dev" 2>/dev/null | head -1)
    [ -z "$_uuid" ] && continue
    case " $_fstab_uuids " in *" $_uuid "*) continue ;; esac
    basename "$_dev"
    return 0
  done
  printf ''
}

# vm_ip：这台机器的非回环地址（入口地址必须是它，不能是 127.0.0.1）。
vm_ip() {
  if [ -n "${VM_IP:-}" ]; then printf '%s' "$VM_IP"; return 0; fi
  _ip=$(ip -4 -o addr show scope global 2>/dev/null | awk 'NR==1{split($4,a,"/"); print a[1]}')
  [ -z "$_ip" ] && _ip=$(hostname -I 2>/dev/null | awk '{print $1}')
  printf '%s' "$_ip"
}

mkdir -p "$WORK/logs" "$PAYLOADS" "$FIXDIR" "$WEB_DB_DIR"
chown -R "$WEB_USER" "$WEB_DB_DIR"

echo "== 0. 环境与安全守卫 =="
. /etc/os-release
note "系统：$PRETTY_NAME"
expect_eq "以 root 运行" "0" "$(id -u)"
if id "$WEB_USER" >/dev/null 2>&1; then
  pass "Web 运行身份存在（$WEB_USER）"
else
  fail "Web 运行身份存在" "没有 $WEB_USER 账号"
  exit 2
fi
if id -nG "$WEB_USER" 2>/dev/null | tr ' ' '\n' | grep -qx docker; then
  fail "Web 身份不在 docker 组" "实际上在"
else
  pass "Web 身份不在 docker 组"
fi
if command -v python3 >/dev/null 2>&1; then pass "python3 可用（解析 JSON）"; else
  fail "python3 可用" "没有 python3，脚本无法解析载荷"; exit 2; fi
note "docker 版本：$(docker version --format '{{.Server.Version}}' 2>/dev/null || printf '不可读')"
if docker image inspect "$CNT_IMAGE" >/dev/null 2>&1; then
  pass "夹具镜像已在本机（$CNT_IMAGE）"
else
  if docker pull -q "$CNT_IMAGE" >/dev/null 2>&1; then
    pass "夹具镜像拉取成功（$CNT_IMAGE）"
  else
    fail "夹具镜像可用" "拉不到 $CNT_IMAGE"
  fi
fi
for _port in "$PORT" "$PORT_500" "$PORT_OK" "$PORT_SITE"; do
  if ss -lntH "sport = :$_port" 2>/dev/null | grep -q .; then
    fail "端口 $_port 空闲" "已被占用"; exit 2
  else
    pass "端口 $_port 空闲"
  fi
done
HOST_IP=$(vm_ip)
if [ -n "$HOST_IP" ]; then
  note "服务器非回环地址：$HOST_IP（入口地址用它，不用 127.0.0.1）"
else
  note "这台机器没有非回环 IPv4 地址：入口地址那一段会用保留域名，可达性检查标 NOT RUN"
fi

echo
echo "== 1. 配置与启动（代理 root、Web 非 root） =="
install -d -m 0755 /etc/upanel
install -d -m 0700 "$AGENT_DB_DIR"
install -d -m 0755 "$RUN_DIR"
# 只清 Web 库：代理库里的卷登记与 fstab 托管块是这台机器的真实状态，脚本不销毁。
rm -f "$WEB_DB" "$WEB_DB-wal" "$WEB_DB-shm"
if [ -e "$AGENT_DB" ]; then
  note "沿用已有的代理数据库（保留受管理卷登记）"
fi
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
JAR=$WORK/admin.jar
expect_eq "管理员登录" "200" "$(login admin "$PASSWORD" "$JAR" login-admin)"
CSRF=$(csrf_of "$WORK/login-admin.json")
expect_ne "拿到 CSRF 令牌" "" "$CSRF"

echo
echo "== 2. 夹具：容器与 systemd 单元 =="
docker rm -f "$NAME_LOGS" "$NAME_OKHTTP" "$NAME_500" "$NAME_SITE" "$NAME_GATED" \
  "$NAME_ALWAYS" "$NAME_REBUILD" >/dev/null 2>&1 || true
# 日志容器：周期性写一行，docker logs 才有东西可看（空日志页不算验过日志）。
ID_LOGS=$(docker run -d --name "$NAME_LOGS" "$CNT_IMAGE" \
  sh -c 'while true; do echo "upn-p095 docker heartbeat"; sleep 5; done')
ID_OKHTTP=$(docker run -d --name "$NAME_OKHTTP" -p "127.0.0.1:$PORT_OK:8080" "$CNT_IMAGE" \
  sh -c 'while true; do printf "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 2\r\n\r\nok" | nc -l -p 8080 -w 1; done')
ID_500=$(docker run -d --name "$NAME_500" -p "127.0.0.1:$PORT_500:8080" "$CNT_IMAGE" \
  sh -c 'while true; do printf "HTTP/1.1 500 Internal Server Error\r\nContent-Type: text/plain\r\nContent-Length: 4\r\n\r\nnope" | nc -l -p 8080 -w 1; done')
ID_ALWAYS=$(docker run -d --name "$NAME_ALWAYS" --restart always "$CNT_IMAGE" sleep 3600)
ID_GATED=$(docker run -d --name "$NAME_GATED" "$CNT_IMAGE" sleep 3600)
ID_REBUILD=$(docker run -d --name "$NAME_REBUILD" "$CNT_IMAGE" sleep 3600)
if [ -n "$HOST_IP" ]; then
  # 站点容器：只发布到服务器的非回环地址。它就是“入口地址”指向的那个东西——
  # 从 127.0.0.1 打它是不通的（独立证据：入口不是回环）。
  ID_SITE=$(docker run -d --name "$NAME_SITE" -p "$HOST_IP:$PORT_SITE:8080" "$CNT_IMAGE" \
    sh -c 'while true; do printf "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 4\r\n\r\nsite" | nc -l -p 8080 -w 1; done')
else
  ID_SITE=""
fi
for _pair in "ID_LOGS=$ID_LOGS" "ID_OKHTTP=$ID_OKHTTP" "ID_500=$ID_500" "ID_ALWAYS=$ID_ALWAYS" \
             "ID_GATED=$ID_GATED" "ID_REBUILD=$ID_REBUILD"; do
  case $_pair in
    *=) fail "容器夹具创建：${_pair%%=*}" "没有拿到容器 ID" ;;
  esac
done
expect_eq "日志容器 running" "running" "$(cstate "$NAME_LOGS")"
expect_eq "200 容器直接给出 200（独立证据：curl 不打产品）" "200" \
  "$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "http://127.0.0.1:$PORT_OK/")"
expect_eq "500 容器直接给出 500（独立证据）" "500" \
  "$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "http://127.0.0.1:$PORT_500/")"
expect_eq "gated 容器已停止（门禁夹具）" "exited" "$(docker stop "$NAME_GATED" >/dev/null; cstate "$NAME_GATED")"
if [ -n "$ID_SITE" ]; then
  expect_eq "站点容器在服务器地址上作答（入口地址指向它）" "200" \
    "$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "http://$HOST_IP:$PORT_SITE/")"
  expect_eq "…同一端口在回环上没有东西（证明入口不是回环）" "000" \
    "$(curl -s -o /dev/null -w '%{http_code}' --max-time 2 "http://127.0.0.1:$PORT_SITE/")"
fi

cat >"$FIXTURE_UNIT_PATH" <<'UNIT'
[Unit]
Description=uPanel P09.5 验收夹具（脚本创建，收尾删除）
After=network.target

[Service]
Type=simple
ExecStart=/bin/sh -c 'while true; do echo "upn-p095 heartbeat"; sleep 5; done'
Restart=no

[Install]
WantedBy=multi-user.target
UNIT
chmod 0644 "$FIXTURE_UNIT_PATH"
systemctl daemon-reload >/dev/null 2>&1
if systemctl enable --now "$FIXTURE_UNIT" >/dev/null 2>&1; then
  pass "夹具单元 $FIXTURE_UNIT 已 enabled 并启动"
else
  fail "夹具单元 $FIXTURE_UNIT 启动" "$(systemctl status "$FIXTURE_UNIT" --no-pager 2>&1 | tail -3)"
fi
expect_eq "独立证据：systemctl 报告夹具单元 active" "active" \
  "$(systemctl is-active "$FIXTURE_UNIT" 2>/dev/null)"
expect_eq "服务快照里已经能看到夹具单元（等采集跟上）" "True" "$(wait_unit "$FIXTURE_UNIT" 60)"

echo
echo "== 3. 应用矩阵：能力来自底层对象，不是来自类型 =="

# 3.1 入口（link）：没有运行时，也就没有启停。
if [ -n "$HOST_IP" ]; then
  ENTRY_URL="http://$HOST_IP:$PORT_SITE/"
  SITE_BASE="http://$HOST_IP:$PORT_SITE"
else
  ENTRY_URL="https://panel.example.com"
fi
expect_eq "入口（link）应用创建" "201" \
  "$(create_app link "{\"name\":\"对外站点\",\"type\":\"link\",\"description\":\"反代后的站点入口\",\"public_url\":\"$ENTRY_URL\",\"icon_key\":\"globe\"}")"
LINK_ID=$(app_id)
expect_eq "link 的 public_url 就是服务器地址" "$ENTRY_URL" "$(dget '["public_url"]')"
expect_eq "link 的 runtime_state" "not_applicable" "$(dget '["runtime_state"]')"
expect_eq "link 的 runtime_note 是 null（没有话要说）" "None" "$(dget '["runtime_note"]')"
expect_eq "link 的 dependency_protection" "not_applicable" "$(dget '["dependency_protection"]')"
expect_eq "link 的 http_state" "not_applicable" "$(dget '["http_state"]')"
expect_eq "link 只有 open 能力（启停日志全 false → 卡片上没有启停按钮）" "True" \
  "$(jget "$FIXDIR/app-link.json" 'd["data"]["capabilities"]["open"] and not (d["data"]["capabilities"]["start"] or d["data"]["capabilities"]["stop"] or d["data"]["capabilities"]["restart"] or d["data"]["capabilities"]["logs"])')"

# 另外两个入口：同一个夹具站点在同一端口上对任何路径都回 200，所以这两条地址是**真的**
# 能打开的东西，不是“看起来像 URL 的字符串”。首页的「常用入口」需要有不止一个可打开条目，
# 否则那一块的分组顺序与数量筛选在载荷里根本看不出来。
if [ -n "$HOST_IP" ]; then
  expect_eq "入口（link）应用创建：下载站" "201" \
    "$(create_app link-files "{\"name\":\"下载站\",\"type\":\"link\",\"public_url\":\"$SITE_BASE/files/\",\"icon_key\":\"files\"}")"
  LINK_FILES=$(app_id)
  expect_eq "…它的入口地址此刻真的作答 200（独立证据：curl 打服务器地址）" "200" \
    "$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "$SITE_BASE/files/")"
  expect_eq "入口（link）应用创建：图库" "201" \
    "$(create_app link-media "{\"name\":\"图库\",\"type\":\"link\",\"public_url\":\"$SITE_BASE/media/\",\"icon_key\":\"media\"}")"
  LINK_MEDIA=$(app_id)
  expect_eq "…它的入口地址此刻真的作答 200" "200" \
    "$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "$SITE_BASE/media/")"
else
  LINK_FILES=""
  LINK_MEDIA=""
  notrun "另外两个可打开的入口" "这台机器没有非回环地址，没有可指向的真实站点"
fi

# 3.2 systemd：运行中的单元有 stop/restart，没有 start。
expect_eq "systemd 应用（绑在 running 的单元上）创建" "201" \
  "$(create_app sysd "{\"name\":\"验收夹具服务\",\"type\":\"systemd\",\"binding\":{\"unit\":\"$FIXTURE_UNIT\"}}")"
SYS_ID=$(app_id)
expect_eq "systemd(running) 的 runtime_state" "running" "$(dget '["runtime_state"]')"
expect_eq "运行中的单元不出现 start 能力" "False" "$(dget '["capabilities"]["start"]')"
expect_eq "运行中的单元出现 stop 能力" "True" "$(dget '["capabilities"]["stop"]')"
expect_eq "运行中的单元出现 restart 能力" "True" "$(dget '["capabilities"]["restart"]')"
expect_eq "systemd 应用能读日志" "True" "$(dget '["capabilities"]["logs"]')"
expect_eq "没有 public_url 时 open 能力是 false（卡片上不是锚点）" "False" \
  "$(dget '["capabilities"]["open"]')"

# 3.3 受保护单元：能力来自对象。它是 running，但只有日志。
expect_eq "受保护单元（ssh.service）建应用" "201" \
  "$(create_app sysd-ssh '{"name":"远程登录","type":"systemd","binding":{"unit":"ssh.service"}}')"
SYS_SSH=$(app_id)
expect_eq "受保护单元的 runtime_state 照样是 running" "running" "$(dget '["runtime_state"]')"
expect_eq "受保护单元三项生命周期能力全 false" "True" \
  "$(jget "$FIXDIR/app-sysd-ssh.json" 'not (d["data"]["capabilities"]["start"] or d["data"]["capabilities"]["stop"] or d["data"]["capabilities"]["restart"])')"
expect_eq "但受保护单元仍能读日志（读日志不是写操作）" "True" \
  "$(jget "$FIXDIR/app-sysd-ssh.json" 'd["data"]["capabilities"]["logs"]')"

# 3.4 绑定不存在的单元：unknown + 要求重新确认，不是 stopped。
expect_eq "绑定不存在的单元创建" "201" \
  "$(create_app sysd-missing '{"name":"幽灵服务","type":"systemd","binding":{"unit":"upn-p095-missing.service"}}')"
SYS_MISSING=$(app_id)
expect_eq "绑定不存在的单元 → runtime_state=unknown（不是 stopped）" "unknown" "$(dget '["runtime_state"]')"
expect_contains "…note 要求重新确认绑定" "重新确认绑定" "$(dget '["runtime_note"]')"
expect_eq "…三项生命周期能力全 false" "True" \
  "$(jget "$FIXDIR/app-sysd-missing.json" 'not (d["data"]["capabilities"]["start"] or d["data"]["capabilities"]["stop"] or d["data"]["capabilities"]["restart"])')"

# 3.5 docker：running 的容器没有 start，stopped 的才出现 start。
expect_eq "docker 应用（日志容器）创建" "201" \
  "$(create_app docker-logs "{\"name\":\"网盘\",\"type\":\"docker\",\"binding\":{\"container_id\":\"$ID_LOGS\"},\"icon_key\":\"files\"}")"
DOCKER_LOGS=$(app_id)
expect_eq "docker(running) 的 runtime_state" "running" "$(dget '["runtime_state"]')"
expect_eq "docker(running) 不出现 start 能力" "False" "$(dget '["capabilities"]["start"]')"
expect_eq "docker(running) 出现 stop 能力" "True" "$(dget '["capabilities"]["stop"]')"
expect_eq "docker(running) 能读日志" "True" "$(dget '["capabilities"]["logs"]')"

expect_eq "docker 应用（stopped 的容器）创建" "201" \
  "$(create_app docker-gated "{\"name\":\"数据服务\",\"type\":\"docker\",\"binding\":{\"container_id\":\"$ID_GATED\"},\"icon_key\":\"database\"}")"
GATED_ID=$(app_id)
expect_eq "docker(stopped) 的 runtime_state" "stopped" "$(dget '["runtime_state"]')"
expect_eq "docker(stopped) 出现 start 能力" "True" "$(dget '["capabilities"]["start"]')"
expect_eq "docker(stopped) 不出现 stop 能力" "False" "$(dget '["capabilities"]["stop"]')"

# 独立证据：库里存的绑定就是容器的完整 ID，摘要不是从名字猜出来的。
expect_eq "独立证据：apps.binding_json 里是完整容器 ID" \
  "{\"container_id\":\"$ID_LOGS\"}" \
  "$(python3 -c 'import sqlite3,sys
con=sqlite3.connect(sys.argv[1]); con.execute("PRAGMA wal_checkpoint(TRUNCATE)")
row=con.execute("SELECT binding_json FROM apps WHERE id = ?", (sys.argv[2],)).fetchone()
print("" if row is None else row[0])' "$WEB_DB" "$DOCKER_LOGS")"

echo
echo "== 4. 状态区分：运行时与 HTTP 各说各的 =="
api "$JAR" "$CSRF" GET "/api/v1/apps/discovery" >/dev/null
cap discovery
T_OK=$(jget "$WORK/resp.json" "next((c[\"target_id\"] for c in d[\"data\"][\"candidates\"] if c.get(\"container_id\") == \"$ID_OKHTTP\"), \"\")")
P_OK=$(jget "$WORK/resp.json" "next((c[\"suggested_port\"] for c in d[\"data\"][\"candidates\"] if c.get(\"container_id\") == \"$ID_OKHTTP\"), 0)")
T_500=$(jget "$WORK/resp.json" "next((c[\"target_id\"] for c in d[\"data\"][\"candidates\"] if c.get(\"container_id\") == \"$ID_500\"), \"\")")
P_500=$(jget "$WORK/resp.json" "next((c[\"suggested_port\"] for c in d[\"data\"][\"candidates\"] if c.get(\"container_id\") == \"$ID_500\"), 0)")

if [ -n "$T_OK" ]; then
  pass "发现结果里找到了 200 容器的目标标识（$T_OK，建议端口 $P_OK）"
  expect_eq "发现出来的宿主端口就是夹具发布的那个" "$PORT_OK" "$P_OK"
  expect_eq "docker 应用（带健康检查，指向 200 容器）创建" "201" \
    "$(create_app docker-ok "{\"name\":\"媒体库\",\"type\":\"docker\",\"binding\":{\"container_id\":\"$ID_OKHTTP\"},\"icon_key\":\"media\",\"health_check\":{\"enabled\":true,\"target_id\":\"$T_OK\",\"port\":$P_OK,\"path\":\"/\",\"interval_seconds\":30,\"timeout_seconds\":3}}")"
  OK_ID=$(app_id)
  expect_eq "健康探测得出结论：ok" "ok" "$(wait_health "$OK_ID" ok 90)"
  cp "$WORK/health.json" "$FIXDIR/health-ok.json"
  expect_eq "…状态码 200" "200" "$(jget "$WORK/health.json" 'd["data"]["status_code"]')"
  expect_eq "…探测实际请求的地址恒为 127.0.0.1" "127.0.0.1" \
    "$(jget "$WORK/health.json" 'd["data"]["address"]')"
  expect_contains "…完整 URL 在回环上" "http://127.0.0.1:" \
    "$(jget "$WORK/health.json" 'd["data"]["url"]')"
  expect_eq "…http_state 在应用摘要里也是 ok" "ok" \
    "$(appget docker-ok "$OK_ID" '["http_state"]')"
else
  notrun "200 容器的健康探测" "发现结果里没有 $NAME_OKHTTP 的候选（端口未发布到回环？）"
  OK_ID=""
fi

if [ -n "$T_500" ]; then
  pass "发现结果里找到了 500 容器的目标标识（$T_500，建议端口 $P_500）"
  expect_eq "docker 应用（带健康检查，指向 500 容器）创建" "201" \
    "$(create_app docker-500 "{\"name\":\"坏站点\",\"type\":\"docker\",\"binding\":{\"container_id\":\"$ID_500\"},\"health_check\":{\"enabled\":true,\"target_id\":\"$T_500\",\"port\":$P_500,\"path\":\"/\",\"interval_seconds\":30,\"timeout_seconds\":3}}")"
  FIVE_ID=$(app_id)
  expect_eq "健康探测得出结论：error" "error" "$(wait_health "$FIVE_ID" error 90)"
  cp "$WORK/health.json" "$FIXDIR/health-error.json"
  expect_eq "…拿到的状态码是 500（不是 0，也不是 null）" "500" \
    "$(jget "$WORK/health.json" 'd["data"]["status_code"]')"
  expect_eq "…同时 runtime_state 仍是 running（在跑，但不健康）" "running" \
    "$(appget docker-500 "$FIVE_ID" '["runtime_state"]')"
  expect_eq "…http_state 在应用摘要里是 error" "error" \
    "$(jget "$FIXDIR/app-docker-500.json" 'd["data"]["http_state"]')"
  expect_eq "…last_checked_at 有值" "True" \
    "$(jget "$FIXDIR/app-docker-500.json" 'bool(d["data"]["last_checked_at"])')"
  expect_eq "…卡片照旧给得出 stop（能停一个不健康的服务）" "True" \
    "$(jget "$FIXDIR/app-docker-500.json" 'd["data"]["capabilities"]["stop"]')"
else
  notrun "500 容器的健康探测" "发现结果里没有 $NAME_500 的候选（端口未发布到回环？）"
  FIVE_ID=""
fi

echo
echo "== 5. 重建：ID 变了就是看不见，要求重新确认 =="
expect_eq "重建前的容器应用创建" "201" \
  "$(create_app docker-rebuild "{\"name\":\"会被重建的容器\",\"type\":\"docker\",\"binding\":{\"container_id\":\"$ID_REBUILD\"},\"icon_key\":\"docker\"}")"
REBUILD_APP=$(app_id)
expect_eq "重建前 runtime_state" "running" "$(dget '["runtime_state"]')"
docker rm -f "$NAME_REBUILD" >/dev/null 2>&1
NEW_ID=$(docker run -d --name "$NAME_REBUILD" "$CNT_IMAGE" sleep 3600)
expect_ne "重建出来的容器 ID 与旧的不同（独立证据）" "$ID_REBUILD" "$NEW_ID"
expect_eq "旧绑定 → runtime_state=unknown" "unknown" \
  "$(appget docker-rebuild "$REBUILD_APP" '["runtime_state"]')"
expect_contains "…note 说明容器可能已被重建、要重新确认绑定" "重新确认绑定" \
  "$(jget "$FIXDIR/app-docker-rebuild.json" 'd["data"]["runtime_note"]')"
expect_eq "…不出现 start 能力（看不见的容器没有按钮）" "False" \
  "$(jget "$FIXDIR/app-docker-rebuild.json" 'd["data"]["capabilities"]["start"]')"
expect_eq "…也不给日志能力" "False" \
  "$(jget "$FIXDIR/app-docker-rebuild.json" 'd["data"]["capabilities"]["logs"]')"
expect_eq "…名字相同但不自动接手（库里仍是旧 ID）" "{\"container_id\":\"$ID_REBUILD\"}" \
  "$(python3 -c 'import sqlite3,sys
con=sqlite3.connect(sys.argv[1]); con.execute("PRAGMA wal_checkpoint(TRUNCATE)")
row=con.execute("SELECT binding_json FROM apps WHERE id = ?", (sys.argv[2],)).fetchone()
print("" if row is None else row[0])' "$WEB_DB" "$REBUILD_APP")"
api "$JAR" "$CSRF" GET "/api/v1/apps/discovery" >/dev/null
cap discovery-rebuild
expect_eq "…发现结果把它列进 missing_bindings（页面据此点名要求重新确认）" "True" \
  "$(jget "$WORK/resp.json" "any(b.get(\"app_id\") == \"$REBUILD_APP\" for b in d[\"data\"][\"missing_bindings\"])")"

echo
echo "== 6. 入口是服务器地址，探测只走回环（SSRF 是结构性的） =="
# 6.1 生产模式拒绝回环入口：否则浏览器打开的是访问者自己的机器。
expect_eq "入口地址是回环 → 400" "400" \
  "$(api "$JAR" "$CSRF" POST "$APPS" '{"name":"骗人的入口","type":"link","public_url":"http://127.0.0.1:8080/"}')"
expect_eq "…错误码 VALIDATION_ERROR" "VALIDATION_ERROR" "$(ecode)"
expect_contains "…说明回环入口会被打开成访问者自己的机器" "本机回环地址" "$(emsg)"
expect_eq "入口地址是 localhost → 400" "400" \
  "$(api "$JAR" "$CSRF" POST "$APPS" '{"name":"骗人的入口2","type":"link","public_url":"http://localhost:3000/"}')"
expect_eq "入口地址带用户名密码 → 400" "400" \
  "$(api "$JAR" "$CSRF" POST "$APPS" '{"name":"骗人的入口3","type":"link","public_url":"https://user:pw@panel.example.com/"}')"
expect_eq "入口地址是 javascript: → 400" "400" \
  "$(api "$JAR" "$CSRF" POST "$APPS" '{"name":"骗人的入口4","type":"link","public_url":"javascript:alert(1)"}')"

# 6.2 探测目标里填不进别的机器：target_id 受结构约束，没有 host/ip/url 字段。
expect_eq "把云元数据地址填进 target_id → 400" "400" \
  "$(api "$JAR" "$CSRF" POST "$APPS" '{"name":"嗅探元数据","type":"docker","binding":{"container_id":"'"$ID_LOGS"'"},"health_check":{"enabled":true,"target_id":"169.254.169.254","port":80,"path":"/","interval_seconds":30,"timeout_seconds":3}}')"
expect_eq "…错误码 VALIDATION_ERROR" "VALIDATION_ERROR" "$(ecode)"
expect_eq "把主机名填进 target_id → 400" "400" \
  "$(api "$JAR" "$CSRF" POST "$APPS" '{"name":"嗅探内网","type":"docker","binding":{"container_id":"'"$ID_LOGS"'"},"health_check":{"enabled":true,"target_id":"db.internal","port":3306,"path":"/","interval_seconds":30,"timeout_seconds":3}}')"
expect_eq "把完整 URL 填进 target_id → 400" "400" \
  "$(api "$JAR" "$CSRF" POST "$APPS" '{"name":"嗅探 URL","type":"docker","binding":{"container_id":"'"$ID_LOGS"'"},"health_check":{"enabled":true,"target_id":"http://169.254.169.254/latest/meta-data/","port":80,"path":"/","interval_seconds":30,"timeout_seconds":3}}')"
expect_eq "形状对但目标不在发现结果里 → 422" "422" \
  "$(api "$JAR" "$CSRF" POST "$APPS" '{"name":"嗅探不存在","type":"docker","binding":{"container_id":"'"$ID_LOGS"'"},"health_check":{"enabled":true,"target_id":"cntr_deadbeefdeadbeefdeadbeef","port":80,"path":"/","interval_seconds":30,"timeout_seconds":3}}')"
expect_eq "…错误码 VALIDATION_ERROR（明确说目标不在发现结果里）" "VALIDATION_ERROR" "$(ecode)"
expect_contains "…消息里点名 target_id" "发现结果" "$(emsg)"
# 结构约束是双向的：路径也不能是完整 URL。
expect_eq "path 里塞完整 URL → 400" "400" \
  "$(api "$JAR" "$CSRF" POST "$APPS" '{"name":"嗅探 path","type":"docker","binding":{"container_id":"'"$ID_LOGS"'"},"health_check":{"enabled":true,"target_id":"'"$T_OK"'","port":'"${P_OK:-0}"',"path":"http://169.254.169.254/","interval_seconds":30,"timeout_seconds":3}}')"
if [ -n "${T_OK:-}" ]; then
  expect_eq "…错误码 VALIDATION_ERROR" "VALIDATION_ERROR" "$(ecode)"
fi

echo
echo "== 7. 分组与收藏：页面分桶的输入 =="
expect_eq "建分组「基础设施」" "201" \
  "$(api "$JAR" "$CSRF" POST "$GROUPS" '{"name":"基础设施","sort_order":10}')"
GRP1=$(dget '["id"]')
expect_eq "建分组「媒体」" "201" \
  "$(api "$JAR" "$CSRF" POST "$GROUPS" '{"name":"媒体","sort_order":20}')"
GRP2=$(dget '["id"]')
expect_eq "重名分组 → 422" "422" \
  "$(api "$JAR" "$CSRF" POST "$GROUPS" '{"name":"基础设施"}')"
expect_eq "…错误码 VALIDATION_ERROR" "VALIDATION_ERROR" "$(ecode)"

expect_eq "把 systemd 应用放进「基础设施」" "200" \
  "$(apppatch sysd-grouped "$SYS_ID" "\"group_id\":\"$GRP1\"")"
expect_eq "把日志容器应用放进「基础设施」" "200" \
  "$(apppatch docker-logs-grouped "$DOCKER_LOGS" "\"group_id\":\"$GRP1\"")"
if [ -n "${OK_ID:-}" ]; then
  expect_eq "把媒体库放进「媒体」" "200" \
    "$(apppatch docker-ok-grouped "$OK_ID" "\"group_id\":\"$GRP2\"")"
fi
if [ -n "${LINK_MEDIA:-}" ]; then
  expect_eq "把图库放进「媒体」（这个分组里就有两条了）" "200" \
    "$(apppatch link-media-grouped "$LINK_MEDIA" "\"group_id\":\"$GRP2\"")"
fi

# 7.1 服务端过滤：group_id 是契约参数，形状不对按“没有这个分组”处理，不报错。
api "$JAR" "$CSRF" GET "$APPS?limit=200" >/dev/null
cap apps
N_ALL=$(dget '["items"].__len__()')
expect_eq "列表载荷是 {items, next_cursor}" "True" \
  "$(jget "$WORK/resp.json" 'isinstance(d["data"]["items"], list) and "next_cursor" in d["data"]')"
expect_eq "按分组过滤 → 200" "200" "$(api "$JAR" "$CSRF" GET "$APPS?group_id=$GRP1")"
expect_eq "…只有这个分组的两条" "2" "$(dget '["items"].__len__()')"
expect_eq "…每一条的 group_id 都是它" "True" \
  "$(dget '["items"].__len__() == 2 and all(a["group_id"] == "'"$GRP1"'" for a in d["data"]["items"])')"
expect_eq "形状对但不存在分组 → 200 且空（不报错）" "200" \
  "$(api "$JAR" "$CSRF" GET "$APPS?group_id=grp_00000000000000000000000000000000")"
expect_eq "…空列表" "0" "$(dget '["items"].__len__()')"
expect_eq "形状不对的 group_id → 400（不静默变成空）" "400" \
  "$(api "$JAR" "$CSRF" GET "$APPS?group_id=not-a-group")"
expect_eq "…错误码 VALIDATION_ERROR，说清是 group_id" "VALIDATION_ERROR" "$(ecode)"
expect_contains "…消息点名出问题的是分组标识（不缩写成一个“参数错误”）" "分组标识" "$(emsg)"
# 反面必须区分开：形状对但不存在是 200 + 空（“这个分组里没有应用”），
# 形状错是 400（“你给的过滤器我读不懂”）——两者不能都变成一个空列表。

# 7.2 搜索在前端：q 不是契约参数，结果集不因它变化。
expect_eq "GET /apps?q=… 仍返回 200（服务端没有搜索参数）" "200" "$(api "$JAR" "$CSRF" GET "$APPS?q=zzz-nothing")"
expect_eq "…结果集与不带 q 时一样大（搜索是页面的事）" "$N_ALL" "$(dget '["items"].__len__()')"

# 7.3 收藏是当前用户自己的偏好。
expect_eq "收藏入口应用" "200" \
  "$(api "$JAR" "$CSRF" PUT "$APPS/$LINK_ID/favorite" '{"favorite":true}')"
expect_eq "…返回 {favorite:true}" "True" "$(dget '["favorite"]')"
if [ -n "${OK_ID:-}" ]; then
  expect_eq "收藏媒体库" "200" \
    "$(api "$JAR" "$CSRF" PUT "$APPS/$OK_ID/favorite" '{"favorite":true}')"
fi
expect_eq "取消收藏再收藏是幂等的（不报冲突、不需要 version）" "200" \
  "$(api "$JAR" "$CSRF" PUT "$APPS/$LINK_ID/favorite" '{"favorite":true}')"
api "$JAR" "$CSRF" GET "$APPS?limit=200" >/dev/null
expect_eq "两条应用在 admin 的列表里是收藏" "2" \
  "$(jget "$WORK/resp.json" 'sum(1 for a in d["data"]["items"] if a["favorite"])')"

# 7.4 非空分组不许删（不顺手把应用移出分组）。
G1V=$(api "$JAR" "$CSRF" GET "$GROUPS" >/dev/null; jget "$WORK/resp.json" \
  "next((g[\"version\"] for g in d[\"data\"][\"items\"] if g[\"id\"] == \"$GRP1\"), 0)")
expect_eq "删除非空分组 → 422" "422" "$(api "$JAR" "$CSRF" DELETE "$GROUPS/$GRP1?version=$G1V")"
expect_eq "…错误码 DEPENDENCY_BLOCKED" "DEPENDENCY_BLOCKED" "$(ecode)"
expect_contains "…说明先把应用移出去" "先把" "$(emsg)"
api "$JAR" "$CSRF" GET "$APPS?group_id=$GRP1" >/dev/null
expect_eq "…分组里的应用没有被顺手移走" "2" "$(dget '["items"].__len__()')"

# 7.5 空分组可以删；改名与排序走 PATCH。
expect_eq "建临时分组" "201" "$(api "$JAR" "$CSRF" POST "$GROUPS" '{"name":"临时分组"}')"
TMP_ID=$(dget '["id"]')
TMP_V=$(dget '["version"]')
expect_eq "删空分组 → 200" "200" "$(api "$JAR" "$CSRF" DELETE "$GROUPS/$TMP_ID?version=$TMP_V")"
api "$JAR" "$CSRF" GET "$GROUPS" >/dev/null
expect_eq "…列表里不再有它" "False" \
  "$(jget "$WORK/resp.json" "any(g[\"id\"] == \"$TMP_ID\" for g in d[\"data\"][\"items\"])")"
expect_eq "重命名「媒体」并调前排序" "200" \
  "$(api "$JAR" "$CSRF" PATCH "$GROUPS/$GRP2" \
     "{\"version\":$(jget "$WORK/resp.json" "next((g[\"version\"] for g in d[\"data\"][\"items\"] if g[\"id\"] == \"$GRP2\"), 1)"),\"name\":\"影音\",\"sort_order\":5}")"
api "$JAR" "$CSRF" GET "$GROUPS" >/dev/null
cap app-groups
expect_eq "分组列表按 sort_order 升序（影音在前）" "$GRP2" \
  "$(jget "$WORK/resp.json" 'd["data"]["items"][0]["id"]')"
expect_eq "…分组名是改后的" "影音" "$(jget "$WORK/resp.json" 'd["data"]["items"][0]["name"]')"
expect_eq "…排序值都 >= 0（契约的最小值）" "True" \
  "$(jget "$WORK/resp.json" 'all(g["sort_order"] >= 0 for g in d["data"]["items"])')"

echo
echo "== 8. 启停真的走预检与任务（service.control） =="
# 卡片上的 stop 按钮发的是 service.control——脚本照同一条路走一遍。
run_action service.control "{\"unit\":\"$FIXTURE_UNIT\",\"operation\":\"stop\"}" appstop
expect_eq "stop 预检" "200" "$PRE_CODE"
expect_eq "…预检允许（受保护名单里没有它）" "True" "$PRE_ALLOWED"
expect_eq "…任务终态" "succeeded" "$JOB_STATE"
expect_eq "…结果经过核验（verified）" "True" "$(jobverified)"
expect_eq "…独立证据：systemctl 报告单元 inactive" "inactive" \
  "$(systemctl is-active "$FIXTURE_UNIT" 2>/dev/null || true)"
expect_eq "…任务结果里的操作是 stop" "stop" "$(jobobserved operation)"
expect_eq "…执行后读数里的 active_state 是 inactive" "inactive" "$(jobobserved active_state)"
expect_eq "停机之后卡片上应该出现 start、不出现 stop" "True" \
  "$(wait_app sysd-stopped "$SYS_ID" 'd["data"]["capabilities"]["start"] and not d["data"]["capabilities"]["stop"]' 60)"

run_action service.control "{\"unit\":\"$FIXTURE_UNIT\",\"operation\":\"start\"}" appstart
expect_eq "start 预检" "200" "$PRE_CODE"
expect_eq "…预检允许" "True" "$PRE_ALLOWED"
expect_eq "…任务终态" "succeeded" "$JOB_STATE"
expect_eq "…独立证据：systemctl 报告单元 active" "active" \
  "$(systemctl is-active "$FIXTURE_UNIT" 2>/dev/null)"
expect_eq "启动之后卡片上应该出现 stop、不出现 start" "True" \
  "$(wait_app sysd-started "$SYS_ID" 'd["data"]["capabilities"]["stop"] and not d["data"]["capabilities"]["start"]' 60)"

# 受保护单元：预检就不允许，任务步骤根本走不出去。
run_action service.control '{"unit":"ssh.service","operation":"stop"}' sshstop
expect_eq "对受保护单元 stop：预检仍 200（判决在 allowed 里）" "200" "$PRE_CODE"
expect_eq "…allowed=false" "False" "$PRE_ALLOWED"
expect_eq "…没有提交出任务" "False" \
  "$(python3 -c 'import sys;print(sys.argv[1] == "202")' "$JOB_CODE")"
expect_eq "…独立证据：ssh.service 还在跑" "active" "$(systemctl is-active ssh.service 2>/dev/null || true)"

echo
echo "== 9. 依赖卷离线：受管理应用不许启动 =="
VOL_DEV=$(pick_volume_device)
VOL_ID=""
if [ -z "$VOL_DEV" ]; then
  notrun "依赖卷门禁（managed 应用）" "这台机器上没有可用的 ext4 分区（可用 VOL_DEV=sdc1 指定）"
else
  note "依赖卷夹具用分区：$VOL_DEV"
  run_action storage.volume.import \
    "{\"device_id\":\"$VOL_DEV\",\"slug\":\"p095gate\",\"display_name\":\"P09.5 门禁夹具\"}" volimport
  expect_eq "导入卷的预检" "200" "$PRE_CODE"
  expect_eq "…预检允许" "True" "$PRE_ALLOWED"
  expect_eq "导入卷任务终态" "succeeded" "$JOB_STATE"
  VOL_ID=$(jobdata volume_id)
  expect_contains "…卷标识是 vol_ 形状" "vol_" "$VOL_ID"
  volrefresh
  expect_eq "…登记之后它确实未挂载（这就是门禁夹具）" "unmounted" "$(volstate "$VOL_ID")"

  # 9.1 restart=no + 依赖卷 → managed；restart=always → unmanaged，理由各自写明。
  expect_eq "给 stopped 容器加上依赖卷" "200" \
    "$(apppatch docker-gated-dep "$GATED_ID" "\"volume_dependencies\":[\"$VOL_ID\"]")"
  expect_eq "docker(restart=no) + 依赖卷 → managed" "managed" \
    "$(appget docker-gated-dep "$GATED_ID" '["dependency_protection"]')"
  expect_contains "…note 说明容器不会自己启动" "不会自己启动" \
    "$(jget "$FIXDIR/app-docker-gated-dep.json" 'd["data"]["dependency_protection_note"]')"
  expect_eq "…runtime_state 与 protection 是两件事（stopped + managed）" "stopped" \
    "$(jget "$FIXDIR/app-docker-gated-dep.json" 'd["data"]["runtime_state"]')"

  expect_eq "always 容器建应用（带依赖卷）" "201" \
    "$(create_app docker-always "{\"name\":\"自启服务\",\"type\":\"docker\",\"binding\":{\"container_id\":\"$ID_ALWAYS\"},\"volume_dependencies\":[\"$VOL_ID\"]}")"
  ALWAYS_ID=$(app_id)
  expect_eq "docker(restart=always) + 依赖卷 → unmanaged" "unmanaged" \
    "$(dget '["dependency_protection"]')"
  expect_contains "…note 点明是 restart 策略绕开了面板" "always" \
    "$(dget '["dependency_protection_note"]')"

  # 9.2 卷未挂载 → start 预检被门禁钉成不允许；提交 409；容器状态不变。
  CODE=$(preflight_action container.control \
    "{\"container_id\":\"$ID_GATED\",\"operation\":\"start\"}" start-gated)
  expect_eq "start 预检仍返回 200（判决在 allowed 里，不是 HTTP 错误）" "200" "$CODE"
  expect_eq "…allowed=false" "False" "$(pfget start-gated '["allowed"]')"
  expect_eq "…阻塞码是 VOLUME_OFFLINE" "True" \
    "$(pfexpr start-gated 'any(b["code"] == "VOLUME_OFFLINE" for b in d["data"]["blockers"])')"
  expect_contains "…阻塞项里写着卷标识" "$VOL_ID" "$(pfget start-gated '["blockers"]')"
  PF=$(pfget start-gated '["preflight_id"]')
  SUB=$(submit_action gated "$PF" "$(pfget start-gated '["confirmation_phrase"]')")
  expect_eq "提交被门禁拦住的预检 → 409" "409" "$SUB"
  expect_eq "…错误码 DEPENDENCY_BLOCKED" "DEPENDENCY_BLOCKED" "$(ecode)"
  expect_eq "…容器没有被启动（独立证据：docker 报告 exited）" "exited" "$(cstate "$NAME_GATED")"

  # 9.3 同一时刻 stop 不被拦：门禁只拦启动。用**带依赖卷的 running 容器**验，
  # 这样“有依赖就一律拦”这个误解就没有余地了。
  expect_eq "给 running 的日志容器也加上同一个依赖卷" "200" \
    "$(apppatch docker-logs-dep "$DOCKER_LOGS" "\"volume_dependencies\":[\"$VOL_ID\"]")"
  CODE=$(preflight_action container.control \
    "{\"container_id\":\"$ID_LOGS\",\"operation\":\"stop\"}" stop-running)
  expect_eq "stop 预检" "200" "$CODE"
  expect_eq "…stop 不被门禁拦住（allowed=true）" "True" "$(pfget stop-running '["allowed"]')"
  expect_eq "…也没有 VOLUME_ 开头的阻塞项" "False" \
    "$(pfexpr stop-running 'any(b["code"].startswith("VOLUME_") for b in d["data"]["blockers"])')"

  # 9.4 正对照：把卷挂起来，同一份 start 预检 allowed=true——
  # 拦住它的是卷的状态，不是“有依赖就一律拦”。
  volrefresh
  run_action storage.volume.mount \
    "{\"volume_id\":\"$VOL_ID\",\"version\":\"$(vfield "$VOL_ID" version)\"}" volmount
  expect_eq "挂载夹具卷：任务终态" "succeeded" "$JOB_STATE"
  volrefresh
  expect_eq "…卷现在挂着（observed_state）" "mounted" "$(volstate "$VOL_ID")"
  CODE=$(preflight_action container.control \
    "{\"container_id\":\"$ID_GATED\",\"operation\":\"start\"}" start-mounted)
  expect_eq "正对照：卷挂着时 allowed=true（门禁没拦）" "True" "$(pfget start-mounted '["allowed"]')"
  expect_eq "正对照：没有任何 VOLUME_ 阻塞项" "False" \
    "$(pfexpr start-mounted 'any(b["code"].startswith("VOLUME_") for b in d["data"]["blockers"])')"
  expect_eq "卷挂着时 protection 仍是 managed（看的是重启策略）" "managed" \
    "$(appget docker-gated-dep "$GATED_ID" '["dependency_protection"]')"
  volrefresh
  run_action storage.volume.unmount \
    "{\"volume_id\":\"$VOL_ID\",\"version\":\"$(vfield "$VOL_ID" version)\"}" volunmount
  expect_eq "卸载夹具卷：任务终态" "succeeded" "$JOB_STATE"
fi

echo
echo "== 10. 权限：viewer 能看能收藏，不能改 =="
expect_eq "建 viewer 账号" "201" \
  "$(api "$JAR" "$CSRF" POST "$USERS" "{\"username\":\"p095viewer\",\"display_name\":\"观察员\",\"role\":\"viewer\",\"initial_password\":\"$VIEWER_PASSWORD\"}")"
VJAR=$WORK/viewer.jar
expect_eq "viewer 登录" "200" "$(login p095viewer "$VIEWER_PASSWORD" "$VJAR" login-viewer)"
VCSRF=$(csrf_of "$WORK/login-viewer.json")
expect_eq "viewer 读应用列表（摘要可见）" "200" "$(api "$VJAR" "$VCSRF" GET "$APPS")"
expect_eq "viewer 读分组列表" "200" "$(api "$VJAR" "$VCSRF" GET "$GROUPS")"
expect_eq "viewer 读发现结果 → 403（本机监听与容器是侦察材料）" "403" \
  "$(api "$VJAR" "$VCSRF" GET "/api/v1/apps/discovery")"
expect_eq "viewer 建应用 → 403" "403" \
  "$(api "$VJAR" "$VCSRF" POST "$APPS" '{"name":" viewer 想建","type":"link","public_url":"https://x.example.com/"}')"
expect_eq "viewer 建分组 → 403" "403" "$(api "$VJAR" "$VCSRF" POST "$GROUPS" '{"name":"viewer 的分组"}')"
expect_eq "viewer 读应用日志 → 403" "403" "$(api "$VJAR" "$VCSRF" GET "$APPS/$SYS_ID/logs")"
# 收藏是当前用户自己的偏好，不是管理动作：viewer 能收藏，且不影响 admin 看到的。
expect_eq "viewer 收藏媒体库 → 200" "200" \
  "$(api "$VJAR" "$VCSRF" PUT "$APPS/$SYS_ID/favorite" '{"favorite":true}')"
api "$VJAR" "$VCSRF" GET "$APPS?limit=200" >/dev/null
expect_eq "…viewer 自己的列表里它是收藏" "True" \
  "$(jget "$WORK/resp.json" "next((a[\"favorite\"] for a in d[\"data\"][\"items\"] if a[\"id\"] == \"$SYS_ID\"), \"missing\")")"
api "$JAR" "$CSRF" GET "$APPS?limit=200" >/dev/null
expect_eq "…admin 的列表里它仍是未收藏（收藏按用户存）" "False" \
  "$(jget "$WORK/resp.json" "next((a[\"favorite\"] for a in d[\"data\"][\"items\"] if a[\"id\"] == \"$SYS_ID\"), \"missing\")")"
expect_eq "匿名读应用列表 → 401" "401" "$(api_anon GET "$APPS")"
expect_eq "匿名收藏 → 401" "401" "$(api_anon PUT "$APPS/$SYS_ID/favorite")"

echo
echo "== 11. 抓取页面载荷（供 vitest 回放） =="
# 应用日志：两个来源各抓一份，页面靠 source 判别渲染。
expect_eq "systemd 应用日志" "200" "$(api "$JAR" "$CSRF" GET "$APPS/$SYS_ID/logs?limit=20")"
cap app-logs-journal
expect_eq "…source=journal" "journal" "$(dget '["source"]')"
expect_eq "…unit 来自绑定" "$FIXTURE_UNIT" "$(dget '["unit"]')"
expect_eq "…有条目（夹具单元每 5 秒写一行）" "True" \
  "$(dget '["page"]["entries"].__len__() > 0')"
expect_eq "docker 应用日志" "200" "$(api "$JAR" "$CSRF" GET "$APPS/$DOCKER_LOGS/logs?tail=20")"
cap app-logs-docker
expect_eq "…source=docker" "docker" "$(dget '["source"]')"
expect_eq "…container_id 来自绑定" "$ID_LOGS" "$(dget '["container_id"]')"
expect_eq "…有条目（夹具容器每 5 秒写一行）" "True" \
  "$(dget '["page"]["entries"].__len__() > 0')"

# 全量列表：这一份就是页面 loadApps() 拿到的东西，分桶与常用入口都从它算。
expect_eq "应用列表（全量）" "200" "$(api "$JAR" "$CSRF" GET "$APPS?limit=200")"
cap apps
api "$JAR" "$CSRF" GET "$GROUPS" >/dev/null
cap app-groups
api "$JAR" "$CSRF" GET "/api/v1/apps/discovery" >/dev/null
cap discovery
if [ -n "${FIVE_ID:-}" ]; then
  api "$JAR" "$CSRF" GET "$APPS/$FIVE_ID/health" >/dev/null
  cap health-error
fi
if [ -n "${OK_ID:-}" ]; then
  api "$JAR" "$CSRF" GET "$APPS/$OK_ID/health" >/dev/null
  cap health-ok
fi
note "夹具落在 $FIXDIR：$(ls "$FIXDIR" | tr '\n' ' ')"

echo
echo "== 12. 收尾：把登记的夹具卷退出管理，删夹具应用 =="
if [ -z "$VOL_ID" ]; then
  notrun "退出管理夹具卷" "这次运行没有登记夹具卷"
else
  volrefresh
  VOL_UUID=$(vfield "$VOL_ID" filesystem_uuid)
  run_action storage.volume.forget \
    "{\"volume_id\":\"$VOL_ID\",\"version\":\"$(vfield "$VOL_ID" version)\"}" volforget
  expect_eq "退出管理：预检" "200" "$PRE_CODE"
  expect_eq "…任务终态" "succeeded" "$JOB_STATE"
  expect_eq "…独立证据：fstab 里不再有它的 UUID" "0" \
    "$(grep -c "UUID=$VOL_UUID" /etc/fstab 2>/dev/null || true)"
  volrefresh
  expect_eq "…独立证据：卷列表里不再有它" "missing" "$(vfield "$VOL_ID" id)"
fi

# 夹具应用逐条删掉（版本号取当前值）。删不掉不算失败，但要报出来：留着就会影响
# 下一次运行看到的机器状态。
applist() {
  api "$JAR" "$CSRF" GET "$APPS?limit=200" >/dev/null
  jget "$WORK/resp.json" '"\n".join("%s %s" % (a["id"], a["version"]) for a in d["data"]["items"])'
}
applist | while read -r _id _ver; do
  [ -z "$_id" ] && continue
  api "$JAR" "$CSRF" DELETE "$APPS/$_id?version=$_ver" >/dev/null || true
done
api "$JAR" "$CSRF" GET "$APPS?limit=200" >/dev/null
expect_eq "夹具应用已全部删除" "0" "$(jget "$WORK/resp.json" 'd["data"]["items"].__len__()')"
# 分组要先整份存下来再删：循环里每次 DELETE 都会覆盖 $WORK/resp.json，边删边读版本号
# 会读到上一次删除的响应（data 是空对象），第二个分组就永远删不掉。
api "$JAR" "$CSRF" GET "$GROUPS" >/dev/null
cp "$WORK/resp.json" "$WORK/groups-left.json"
for _row in $(jget "$WORK/groups-left.json" '"\n".join("%s %s" % (g["id"], g["version"]) for g in d["data"]["items"])'); do
  case $_row in
    grp_*) _gid=$_row ;;
    *)
      api "$JAR" "$CSRF" DELETE "$GROUPS/$_gid?version=$_row" >/dev/null || true
      ;;
  esac
done
api "$JAR" "$CSRF" GET "$GROUPS" >/dev/null
expect_eq "夹具分组已全部删除" "0" "$(jget "$WORK/resp.json" 'd["data"]["items"].__len__()')"
note "容器与 systemd 夹具单元由 trap cleanup 删除（含重建过的那个）"

echo
echo "== 汇总 =="
printf 'PASS %s / FAIL %s / NOT RUN %s\n' "$PASSED" "$FAILED" "$NOTRUN"
if [ "$FAILED" -gt 0 ]; then
  printf 'FAILED:%s\n' "$FAILED_NAMES"
  exit 1
fi
exit 0
