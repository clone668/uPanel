#!/bin/sh
# P09.4 Linux 验收脚本：应用与它所绑定的 systemd 单元 / 容器的运行状态、日志、能力，
# 依赖卷的“受管理 / 未接管”判定，以及启动路径上的依赖卷门禁。
#
# 这一份要证明的不是“接口返回 200”，而是下面这些容易被“写完了”骗过去的事实：
#
#   1) **摘要是观测结果，不是类型的属性**：同一个 systemd 应用绑在 running 的单元上是
#      running、绑在不存在的单元上是 unknown 并写清“请重新确认绑定”；同一份代码在
#      代理停掉时给 unknown，绝不给 stopped——把“读不到”说成“停了”会把人骗去点启动。
#   2) **能力来自底层对象**：running 的容器不出现 start、stopped 的容器才出现 start；
#      绑定的容器不在本机列表里时三项全 false（看不见的东西没有按钮）。
#   3) **容器 running 但 HTTP 500 是明确的异常**：runtime_state=running 与
#      http_state=error 同时成立，页面据此说“在跑，但不健康”，而不是二选一。
#   4) **依赖保护判定是诚实的**：restart=no 的容器 + 依赖卷 = managed；restart=always
#      的容器 = unmanaged；systemd 一律 unmanaged（面板不改写单元文件）；没有依赖卷 =
#      not_applicable。每一种都必须带一句“为什么”。
#   5) **门禁只拦启动、只收紧**：依赖卷未挂载时 start/restart 预检 allowed=false 且带
#      VOLUME_OFFLINE，提交得到 409 DEPENDENCY_BLOCKED，容器状态不变；同一时刻 stop
#      预检不被拦（停机在任何状态下都安全）。依赖卷挂着时同一份预检 allowed=true——
#      证明拦住它的是卷的状态，不是“有依赖就一律拦”。
#   6) **容器重建后旧绑定要求重新确认**：ID 变了就是看不见，面板不按名字自动接手。
#   7) **/apps/:id/logs 只判别、不开新口子**：对象来自绑定，请求里的 unit/source 是 400；
#      两个来源的参数集合不互相串味；link 422、compose 501、viewer 403。
#
# 用法（root）：
#   sudo BIN_DIR=/var/tmp/upanel-bin sh tests/integration/p09-4-app-runtime-acceptance.sh
#
# 环境变量：PORT（默认 19494）、WORK、BIN_DIR、CNT_IMAGE（默认 alpine:3.20）、
#           PORT_500（500 容器发布到回环的宿主端口，默认 18894）。
#
# 退出码：0 全部通过；1 有失败项；2 环境不满足（非 root / 二进制缺失 / 没有 docker）。
#
# **本脚本不销毁代理数据库**。/var/lib/upanel-agent/agent.db 里存着受管理卷的登记与
# fstab 托管块的对应关系，删掉它等于把一台真机器上的卷绑定抹掉。因此依赖卷的两段
# 用的是“这台机器上已登记的卷”，靠 /storage/volumes 现场发现：
#   - 有一个状态不是 mounted 的卷 → 用它验 VOLUME_OFFLINE 与 409；
#   - 有一个 mounted 的卷 → 用它做正对照（同一份预检 allowed=true）；
#   - 一个都没有 → 那两段标 NOT RUN，原因写明，不假装通过。
#
# 明确不覆盖（NOT RUN，原因写在这里，不在脚本里假装通过）：
#   - **VOLUME_MISSING / VOLUME_IDENTITY_CHANGED / VOLUME_CHECK_UNAVAILABLE**：
#     三条都要先把一台真机器上的卷登记改坏（退出管理 / 换一块盘 / 停掉代理），
#     代价是污染后面所有用例，且换盘需要重做 P07 的设备流程。三个判决由
#     internal/appruntime/appruntime_test.go 的 TestGateVolumeVerdicts /
#     TestGateFailsClosedWithoutVolumeSource 覆盖。
#   - **systemd 应用的启停执行**：P09.4 不改写单元文件，也不给 systemd 应用加启停按钮，
#     所以真机上只验“能力与保护判定”，不执行 service.control。
#   - **浏览器渲染**：VM 上没有浏览器；DOM 由 vitest 覆盖（P09.5）。
#   - **运行时卷失联自动停止**：P09.4 没实现（CONTRACTS §8.4(7)），脚本不验不存在的东西。

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
  printf '本机没有 docker，P09.4 的容器相关验收无法进行\n' >&2
  exit 2
fi

WORK=${WORK:-/var/tmp/upanel-accept-p09-4}
PAYLOADS=$WORK/payloads
RUN_DIR=/run/upanel
SOCK=$RUN_DIR/agent.sock
SYS_CFG=/etc/upanel/upanel.json
AGENT_DB_DIR=/var/lib/upanel-agent
AGENT_DB=$AGENT_DB_DIR/agent.db
KEY_FILE=/etc/upanel/agent.key
WEB_DB_DIR=$WORK/webdata
WEB_DB=$WEB_DB_DIR/upanel.db
PORT=${PORT:-19494}
PORT_500=${PORT_500:-18894}
BASE="http://127.0.0.1:$PORT"
ORIGIN="$BASE"
WEB_USER=upanel-web
PASSWORD='vm-correct-horse-9'
VIEWER_PASSWORD='vm-viewer-horse-4'

CNT_IMAGE=${CNT_IMAGE:-alpine:3.20}
# 夹具名字全部带 upn-p094- 前缀，收尾时按名字删干净。
NAME_OK=upn-p094-ok
NAME_500=upn-p094-500
NAME_GATED=upn-p094-gated
NAME_ALWAYS=upn-p094-always
NAME_REBUILD=upn-p094-rebuild
NAME_COMPOSE=upn-p094-compose
COMPOSE_PROJECT=upnp094

# systemd 夹具单元：一台验收机上能“随便起停”的服务，不能挑 ssh/docker 这类受保护单元
# （它们的能力集合按契约就是空的，拿它们验“运行中的单元有 stop 能力”只会验到保护规则）。
FIXTURE_UNIT=upn-p094-svc.service
FIXTURE_UNIT_PATH=/etc/systemd/system/$FIXTURE_UNIT

APPS=/api/v1/apps
USERS=/api/v1/users
SERVICES_PATH=/api/v1/services
VOLUMES_PATH=/api/v1/storage/volumes
DEPLOYMENTS=/api/v1/deployments
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

# jget 用 python3 读 JSON：这台机器上没有 jq（与 P05—P09.3 的取舍一致）。
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

cleanup() {
  for _pid in "${WEB_PID:-}" "${AGENT_PID:-}"; do
    if [ -n "$_pid" ] && kill -0 "$_pid" 2>/dev/null; then
      kill -TERM "$_pid" 2>/dev/null
      wait "$_pid" 2>/dev/null
    fi
  done
  # 容器都是脚本自己造的夹具，收尾时删掉：不留在机器上。
  docker rm -f "$NAME_OK" "$NAME_500" "$NAME_GATED" "$NAME_ALWAYS" "$NAME_REBUILD" \
    "$NAME_COMPOSE" >/dev/null 2>&1 || true
  # systemd 夹具单元同样收走：禁用、删文件、重载。
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

# preflight <载荷名> <body>：响应存成 preflight-<名>.json，回显 HTTP 状态码。
preflight() {
  printf '%s' "$2" >"$WORK/pre.json"
  _code=$(curl -s -b "$JAR" -o "$WORK/preflight.json" -w '%{http_code}' \
    -X POST "$BASE$PREFLIGHT_PATH" \
    -H 'Content-Type: application/json' -H "Origin: $ORIGIN" -H "X-CSRF-Token: $CSRF" \
    -d @"$WORK/pre.json")
  cp "$WORK/preflight.json" "$PAYLOADS/preflight-$1.json" 2>/dev/null
  printf '%s' "$_code"
}

# submit_job <载荷名> <preflight_id>：响应存成 job-submit-<名>.json，回显 HTTP 状态码。
submit_job() {
  _code=$(api "$JAR" "$CSRF" POST "$JOBS_PATH" "{\"preflight_id\":\"$2\"}" "p09-4-$1-$$")
  cp "$WORK/resp.json" "$PAYLOADS/job-submit-$1.json" 2>/dev/null
  printf '%s' "$_code"
}

# pfget <载荷名> <路径>：从前一份预检载荷里取字段。
pfget() { jget "$PAYLOADS/preflight-$1.json" "d[\"data\"]$2"; }

# pfexpr <载荷名> <表达式>：在整份预检载荷上求值。
#
# pfget 是把 "$2" 拼在 d["data"] 后面的，只能给 ["allowed"] 这种后缀；要给整个表达式
# （any(...) / len(...)）就必须走这个入口，否则拼出来是语法错误。
pfexpr() { jget "$PAYLOADS/preflight-$1.json" "$2"; }

# create_app <载荷名> <body>：回显状态码；新建出来的标识写进 $WORK/lastapp。
#
# 标识**不能**在函数里赋给一个变量再往外传：调用点用了 $(create_app …)，命令替换
# 跑在子壳里，函数里设的变量出了子壳就没了。文件是这里唯一诚实的交接方式。
create_app() {
  _code=$(api "$JAR" "$CSRF" POST "$APPS" "$2")
  cp "$WORK/resp.json" "$PAYLOADS/app-$1.json" 2>/dev/null
  jget "$PAYLOADS/app-$1.json" 'd["data"]["id"]' >"$WORK/lastapp"
  printf '%s' "$_code"
}
app_id() { cat "$WORK/lastapp"; }

# app_version <app_id>：此刻的 version。PATCH 的乐观锁要的是**现在**的版本号，
# 不是“改完之后的版本号”——给 2 而库里是 1，只会拿到 409 VERSION_CONFLICT。
app_version() {
  api "$JAR" "$CSRF" GET "$APPS/$1" >/dev/null
  jget "$WORK/resp.json" 'd["data"]["version"]'
}

# apppatch <载荷名> <app_id> <除 version 之外的字段片段>：自动带上当前 version。
apppatch() {
  _v=$(app_version "$2")
  _code=$(api "$JAR" "$CSRF" PATCH "$APPS/$2" "{\"version\":$_v${3:+,$3}}")
  cp "$WORK/resp.json" "$PAYLOADS/app-$1.json" 2>/dev/null
  printf '%s' "$_code"
}

# appget <载荷名> <app_id> <字段路径>：重新读一次应用，字段从最新载荷取。
appget() {
  api "$JAR" "$CSRF" GET "$APPS/$2" >/dev/null
  cp "$WORK/resp.json" "$PAYLOADS/app-$1.json" 2>/dev/null
  jget "$PAYLOADS/app-$1.json" "d[\"data\"]$3"
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

# wait_unit <单元名> <超时秒>：等服务快照里出现这个单元。
#
# 为什么必须等：服务列表是采集成快照之后缓存的，新写的单元要等下一轮采集才看得见。
# 脚本刚 systemctl enable --now 完就去建应用，会读到“绑定的单元不在列表里”——那不是
# 产品的 bug，是脚本拿旧快照去断言新事实。等快照自己追上来再建。
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

# cstate <容器名>：docker 自己报告的容器状态（独立证据，不读产品的解析结果）。
cstate() { docker inspect -f '{{.State.Status}}' "$1" 2>/dev/null || printf 'missing'; }
crestart() { docker inspect -f '{{.HostConfig.RestartPolicy.Name}}' "$1" 2>/dev/null || printf 'missing'; }

# 受管理卷列表的最新载荷。
VOLS_FILE=$WORK/volumes.json

# volstate <卷标识>：卷此刻的观测状态（独立证据来自 /storage/volumes 载荷）。
volstate() {
  python3 - "$VOLS_FILE" "$1" <<'PY' 2>/dev/null
import json, sys
d = json.load(open(sys.argv[1], encoding="utf-8"))
vid = sys.argv[2]
for v in (d.get("data") or {}).get("volumes") or []:
    if v.get("id") == vid:
        print(v.get("observed_state") or "unknown")
        raise SystemExit
print("missing")
PY
}

# compose_id <项目名>：按 CONTRACTS 与 containers.ProjectID 的同一算法独立算出 dep_ 标识。
compose_id() {
  python3 -c 'import hashlib,sys
print("dep_" + hashlib.sha256(("compose:" + sys.argv[1]).encode()).hexdigest()[:32])' "$1"
}

# preflight_action <动作> <参数 JSON> <载荷名>：预检响应存成 preflight-<名>.json，回显状态码。
preflight_action() {
  printf '{"action":"%s","params":%s}' "$1" "$2" >"$WORK/pre.json"
  _code=$(curl -s -b "$JAR" -o "$WORK/preflight.json" -w '%{http_code}' \
    -X POST "$BASE$PREFLIGHT_PATH" \
    -H 'Content-Type: application/json' -H "Origin: $ORIGIN" -H "X-CSRF-Token: $CSRF" \
    -d @"$WORK/pre.json")
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
    -H "Idempotency-Key: p09-4-$1-$$" -d @"$WORK/jobreq.json")
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
  else
    JOB_ID=""
    JOB_STATE="submit:$JOB_CODE"
  fi
}
jobdata() { jget "$WORK/job.json" "d[\"data\"][\"result\"][\"data\"].get(\"$1\")"; }
jobobserved() { jget "$WORK/job.json" "d[\"data\"][\"result\"][\"observed_state\"].get(\"$1\")"; }
jobverified() { jget "$WORK/job.json" 'bool(d["data"]["result"].get("verified_at"))'; }

# volrefresh：重新读一次受管理卷列表（observed_state 与 version 都是现场算的）。
volrefresh() {
  api "$JAR" "$CSRF" GET "$VOLUMES_PATH" >/dev/null
  cp "$WORK/resp.json" "$VOLS_FILE"
}

# vfield <卷标识> <字段>：从最近一次卷列表里取字段。
vfield() {
  python3 - "$VOLS_FILE" "$1" "$2" <<'PY' 2>/dev/null
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

# pick_volume_device：挑一块“可以拿来登记成受管理卷”的分区。
#
# 三条都满足才算候选：是分区、有 ext4、当前没挂载；并且它的 UUID 不在现有 fstab 里
# （已经在 fstab 里的分区登记会被判成重复）。挑不到就返回空，调用方把依赖卷两段标
# NOT RUN——不假装通过。
#
# lsblk 只用 -l -n -p：util-linux 2.39 的 -r（raw）与 -l（list）互斥，两个一起给会直接
# 报错退出、拿到空列表。空 MOUNTPOINT 在 list 模式下是行尾空白，awk 会给 $4=""。
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

mkdir -p "$WORK/logs" "$PAYLOADS" "$WEB_DB_DIR"
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
if command -v python3 >/dev/null 2>&1; then pass "python3 可用（解析 JSON 与读库）"; else
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
if ss -lntH "sport = :$PORT" 2>/dev/null | grep -q .; then
  fail "端口 $PORT 空闲" "已被占用"; exit 2
else
  pass "端口 $PORT 空闲"
fi
if ss -lntH "sport = :$PORT_500" 2>/dev/null | grep -q .; then
  fail "端口 $PORT_500 空闲" "已被占用（500 容器要发布它）"; exit 2
else
  pass "端口 $PORT_500 空闲"
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
expect_eq "数据库迁移到第 8 版（P09.4 无新迁移）" "8" "$(dbq 'SELECT MAX(version) FROM schema_migrations')"

JAR=$WORK/admin.jar
expect_eq "管理员登录" "200" "$(login admin "$PASSWORD" "$JAR" login-admin)"
CSRF=$(csrf_of "$WORK/login-admin.json")
expect_ne "拿到 CSRF 令牌" "" "$CSRF"

echo
echo "== 2. 夹具：容器与受管理卷 =="
docker rm -f "$NAME_OK" "$NAME_500" "$NAME_GATED" "$NAME_ALWAYS" "$NAME_REBUILD" \
  "$NAME_COMPOSE" >/dev/null 2>&1 || true
ID_OK=$(docker run -d --name "$NAME_OK" "$CNT_IMAGE" sleep 3600)
ID_500=$(docker run -d --name "$NAME_500" -p "127.0.0.1:$PORT_500:8080" "$CNT_IMAGE" \
  sh -c 'while true; do printf "HTTP/1.1 500 Internal Server Error\r\nContent-Type: text/plain\r\nContent-Length: 4\r\n\r\nnope" | nc -l -p 8080 -w 1; done')
ID_ALWAYS=$(docker run -d --name "$NAME_ALWAYS" --restart always "$CNT_IMAGE" sleep 3600)
ID_GATED=$(docker run -d --name "$NAME_GATED" "$CNT_IMAGE" sleep 3600)
ID_REBUILD=$(docker run -d --name "$NAME_REBUILD" "$CNT_IMAGE" sleep 3600)
# compose 项目的夹具：只打标签（0.1 不执行外部 compose 文件）。
ID_COMPOSE=$(docker run -d --name "$NAME_COMPOSE" \
  -l "com.docker.compose.project=$COMPOSE_PROJECT" \
  -l "com.docker.compose.service=api" \
  -l "com.docker.compose.container-number=1" \
  -l "com.docker.compose.project.working_dir=/srv/$COMPOSE_PROJECT" \
  -l "com.docker.compose.project.config_files=/srv/$COMPOSE_PROJECT/compose.yaml" \
  "$CNT_IMAGE" sleep 3600)
for _pair in "ID_OK=$ID_OK" "ID_500=$ID_500" "ID_ALWAYS=$ID_ALWAYS" \
             "ID_GATED=$ID_GATED" "ID_REBUILD=$ID_REBUILD" "ID_COMPOSE=$ID_COMPOSE"; do
  case $_pair in
    *=) fail "容器夹具创建：${_pair%%=*}" "没有拿到容器 ID" ;;
  esac
done
expect_eq "ok 容器 running" "running" "$(cstate "$NAME_OK")"
expect_eq "500 容器 running" "running" "$(cstate "$NAME_500")"
expect_eq "always 容器的重启策略由 docker 自己报告" "always" "$(crestart "$NAME_ALWAYS")"
expect_eq "ok 容器的重启策略是默认值（no）" "no" "$(crestart "$NAME_OK")"
expect_eq "500 容器直接给出 500（独立证据：curl 不打产品）" "500" \
  "$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "http://127.0.0.1:$PORT_500/")"
docker stop "$NAME_GATED" >/dev/null
expect_eq "gated 容器已停止（门禁夹具）" "exited" "$(cstate "$NAME_GATED")"

# systemd 夹具：一个自己的单元，参数稳定，且不在保护名单里。
cat >"$FIXTURE_UNIT_PATH" <<'UNIT'
[Unit]
Description=uPanel P09.4 验收夹具（脚本创建，收尾删除）
After=network.target

[Service]
Type=simple
ExecStart=/bin/sleep infinity
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
expect_eq "…独立证据：systemctl 报告夹具单元 enabled" "enabled" \
  "$(systemctl is-enabled "$FIXTURE_UNIT" 2>/dev/null)"
expect_eq "服务快照里已经能看到夹具单元（等采集跟上）" "True" "$(wait_unit "$FIXTURE_UNIT" 60)"

api "$JAR" "$CSRF" GET "$VOLUMES_PATH" >/dev/null
cp "$WORK/resp.json" "$VOLS_FILE"
# 依赖卷夹具：走产品自己的登记路径造一个受管理卷。
#
# 为什么不用“机器上已经有的卷”：一台刚装好的机器上不会有；而要在真机上验“卷没挂上
# 就不许启动”，就必须有一个真的登记过、真的没挂的卷。登记（import）只写 fstab 与
# 代理库，不动盘上的数据；脚本收尾时会卸载并退出管理，把机器还原。
VOL_DEV=$(pick_volume_device)
VOL_ID=""
if [ -z "$VOL_DEV" ]; then
  notrun "依赖卷夹具（登记一个受管理卷）" "这台机器上没有可用的 ext4 分区（可用 VOL_DEV=sdc1 指定）"
else
  note "依赖卷夹具用分区：$VOL_DEV"
  run_action storage.volume.import \
    "{\"device_id\":\"$VOL_DEV\",\"slug\":\"p094gate\",\"display_name\":\"P09.4 门禁夹具\"}" import
  expect_eq "导入卷的预检" "200" "$PRE_CODE"
  expect_eq "…预检允许" "True" "$PRE_ALLOWED"
  expect_eq "导入卷任务终态" "succeeded" "$JOB_STATE"
  expect_eq "…结果经过核验（verified）" "True" "$(jobverified)"
  VOL_ID=$(jobdata volume_id)
  expect_contains "…卷标识是 vol_ 形状" "vol_" "$VOL_ID"
  expect_eq "…核验来源里有 fstab 读回" "True" \
    "$(python3 -c 'import sys;print("fstab_reread" in sys.argv[1])' "$(jobobserved checked_via)")"
  volrefresh
  expect_eq "…登记之后它确实未挂载（这就是门禁夹具）" "unmounted" "$(volstate "$VOL_ID")"
  expect_eq "…独立证据：内核挂载表里没有它的挂载点" "False" \
    "$(python3 -c 'import sys;print(bool(sys.argv[1]))' "$(findmnt -n -o TARGET "$(vfield "$VOL_ID" mount_path)" 2>/dev/null)")"
fi

echo
echo "== 3. 摘要是观测结果，不是类型的属性 =="
expect_eq "link 应用创建" "201" \
  "$(create_app link '{"name":"面板入口","type":"link","public_url":"https://panel.example.com","icon_key":"globe"}')"
LINK_ID=$(app_id)
expect_eq "link 的 runtime_state" "not_applicable" "$(dget '["runtime_state"]')"
expect_eq "link 的 runtime_note 是 null（没有话要说）" "None" "$(dget '["runtime_note"]')"
expect_eq "link 的 dependency_protection" "not_applicable" "$(dget '["dependency_protection"]')"
expect_eq "link 的 dependency_protection_note 是 null" "None" "$(dget '["dependency_protection_note"]')"
expect_eq "link 没有启停与日志能力" "True" \
  "$(jget "$PAYLOADS/app-link.json" 'not (d["data"]["capabilities"]["start"] or d["data"]["capabilities"]["stop"] or d["data"]["capabilities"]["restart"] or d["data"]["capabilities"]["logs"])')"

expect_eq "systemd 应用（绑在 running 的单元上）创建" "201" \
  "$(create_app sysd "{\"name\":\"验收夹具服务\",\"type\":\"systemd\",\"binding\":{\"unit\":\"$FIXTURE_UNIT\"}}")"
SYS_ID=$(app_id)
expect_eq "systemd(running) 的 runtime_state" "running" "$(dget '["runtime_state"]')"
expect_eq "systemd(running) 的 runtime_note 是 null" "None" "$(dget '["runtime_note"]')"
expect_eq "运行中的单元不出现 start 能力" "False" "$(dget '["capabilities"]["start"]')"
expect_eq "运行中的单元出现 stop 能力" "True" "$(dget '["capabilities"]["stop"]')"
expect_eq "运行中的单元出现 restart 能力" "True" "$(dget '["capabilities"]["restart"]')"
expect_eq "systemd 应用能读日志" "True" "$(dget '["capabilities"]["logs"]')"
expect_eq "systemd 应用没有依赖卷时 protection" "not_applicable" "$(dget '["dependency_protection"]')"

# 受保护单元的能力按契约是空的：能力来自对象，不来自“它是个 systemd 服务”。
expect_eq "受保护单元（ssh.service）建应用" "201" \
  "$(create_app sysd-ssh '{"name":"远程登录","type":"systemd","binding":{"unit":"ssh.service"}}')"
SYS_SSH=$(app_id)
expect_eq "受保护单元的 runtime_state 照样是 running" "running" "$(dget '["runtime_state"]')"
expect_eq "受保护单元三项生命周期能力全 false" "True" \
  "$(jget "$PAYLOADS/app-sysd-ssh.json" 'not (d["data"]["capabilities"]["start"] or d["data"]["capabilities"]["stop"] or d["data"]["capabilities"]["restart"])')"
expect_eq "但受保护单元仍能读日志（读日志不是写操作）" "True" \
  "$(jget "$PAYLOADS/app-sysd-ssh.json" 'd["data"]["capabilities"]["logs"]')"

expect_eq "systemd 应用（绑在不存在的单元上）创建" "201" \
  "$(create_app sysd-missing '{"name":"幽灵服务","type":"systemd","binding":{"unit":"upn-p094-missing.service"}}')"
SYS_MISSING=$(app_id)
expect_eq "绑定不存在的单元 → runtime_state=unknown（不是 stopped）" "unknown" "$(dget '["runtime_state"]')"
expect_contains "…note 要求重新确认绑定" "重新确认绑定" "$(dget '["runtime_note"]')"
expect_eq "…三项生命周期能力全 false" "True" \
  "$(jget "$PAYLOADS/app-sysd-missing.json" 'not (d["data"]["capabilities"]["start"] or d["data"]["capabilities"]["stop"] or d["data"]["capabilities"]["restart"])')"

expect_eq "docker 应用（绑在 running 的容器上）创建" "201" \
  "$(create_app docker-ok "{\"name\":\"网盘\",\"type\":\"docker\",\"binding\":{\"container_id\":\"$ID_OK\"}}")"
DOCKER_OK=$(app_id)
expect_eq "docker(running) 的 runtime_state" "running" "$(dget '["runtime_state"]')"
expect_eq "docker(running) 不出现 start 能力" "False" "$(dget '["capabilities"]["start"]')"
expect_eq "docker(running) 出现 stop 能力" "True" "$(dget '["capabilities"]["stop"]')"
expect_eq "docker(running) 能读日志" "True" "$(dget '["capabilities"]["logs"]')"

expect_eq "docker 应用（绑在 stopped 的容器上）创建" "201" \
  "$(create_app docker-gated "{\"name\":\"数据服务\",\"type\":\"docker\",\"binding\":{\"container_id\":\"$ID_GATED\"}}")"
GATED_ID=$(app_id)
expect_eq "docker(stopped) 的 runtime_state" "stopped" "$(dget '["runtime_state"]')"
expect_eq "docker(stopped) 出现 start 能力" "True" "$(dget '["capabilities"]["start"]')"
expect_eq "docker(stopped) 不出现 stop 能力" "False" "$(dget '["capabilities"]["stop"]')"

# 独立证据：库里存的绑定就是容器的完整 ID，摘要不是从名字猜出来的。
expect_eq "独立证据：apps.binding_json 里是完整容器 ID" \
  "{\"container_id\":\"$ID_OK\"}" "$(dbq "SELECT binding_json FROM apps WHERE id = '$DOCKER_OK'")"

echo
echo "== 4. 容器 running 但 HTTP 500：明确异常，而不是二选一 =="
api "$JAR" "$CSRF" GET "/api/v1/apps/discovery" >/dev/null
cp "$WORK/resp.json" "$PAYLOADS/discovery.json"
T_500=$(jget "$PAYLOADS/discovery.json" "next((c[\"target_id\"] for c in d[\"data\"][\"candidates\"] if c.get(\"container_id\") == \"$ID_500\"), \"\")")
P_500=$(jget "$PAYLOADS/discovery.json" "next((c[\"suggested_port\"] for c in d[\"data\"][\"candidates\"] if c.get(\"container_id\") == \"$ID_500\"), 0)")
if [ -n "$T_500" ]; then
  pass "发现结果里找到了 500 容器的目标标识（$T_500，建议端口 $P_500）"
  expect_eq "发现出来的宿主端口就是夹具发布的那个" "$PORT_500" "$P_500"
  expect_eq "docker 应用（带健康检查）创建" "201" \
    "$(create_app docker-500 "{\"name\":\"坏站点\",\"type\":\"docker\",\"binding\":{\"container_id\":\"$ID_500\"},\"health_check\":{\"enabled\":true,\"target_id\":\"$T_500\",\"port\":$P_500,\"path\":\"/\",\"interval_seconds\":30,\"timeout_seconds\":3}}")"
  FIVE_ID=$(app_id)
  expect_eq "健康探测得出结论：error" "error" "$(wait_health "$FIVE_ID" error 90)"
  cp "$WORK/health.json" "$PAYLOADS/health-500.json"
  expect_eq "…拿到的状态码是 500（不是 0，也不是 null）" "500" \
    "$(jget "$WORK/health.json" 'd["data"]["status_code"]')"
  expect_eq "…同时 runtime_state 仍是 running（在跑，但不健康）" "running" \
    "$(appget docker-500 "$FIVE_ID" '["runtime_state"]')"
  expect_eq "…http_state 在应用摘要里也是 error" "error" \
    "$(jget "$PAYLOADS/app-docker-500.json" 'd["data"]["http_state"]')"
  expect_eq "…last_checked_at 有值" "True" \
    "$(jget "$PAYLOADS/app-docker-500.json" 'bool(d["data"]["last_checked_at"])')"
else
  notrun "500 容器的健康探测" "发现结果里没有 $NAME_500 的候选（端口未发布到回环？）"
fi

echo
echo "== 5. 依赖保护：受管理 / 未接管，每种都说清理由 =="
# 5.1 没有依赖卷 → not_applicable（有卷才有“接管”这回事）。
expect_eq "docker(无依赖卷) 的 protection" "not_applicable" \
  "$(appget docker-ok "$DOCKER_OK" '["dependency_protection"]')"
expect_eq "…note 是 null" "None" \
  "$(jget "$PAYLOADS/app-docker-ok.json" 'd["data"]["dependency_protection_note"]')"

if [ -n "$VOL_ID" ]; then
  # 5.2 restart=no 的容器 + 依赖卷 → managed（面板是唯一的自动启动者）。
  expect_eq "给 stopped 容器加上依赖卷" "200" \
    "$(apppatch docker-gated "$GATED_ID" "\"volume_dependencies\":[\"$VOL_ID\"]")"
  expect_eq "docker(restart=no) + 依赖卷 → managed" "managed" \
    "$(appget docker-gated "$GATED_ID" '["dependency_protection"]')"
  expect_contains "…note 说明容器不会自己启动" "不会自己启动" \
    "$(jget "$PAYLOADS/app-docker-gated.json" 'd["data"]["dependency_protection_note"]')"
  expect_eq "…runtime_state 与 protection 是两件事（stopped + managed）" "stopped" \
    "$(jget "$PAYLOADS/app-docker-gated.json" 'd["data"]["runtime_state"]')"

  # 5.3 restart=always 的容器 + 依赖卷 → unmanaged（docker 自己会拉起它）。
  expect_eq "always 容器建应用" "201" \
    "$(create_app docker-always "{\"name\":\"自启服务\",\"type\":\"docker\",\"binding\":{\"container_id\":\"$ID_ALWAYS\"},\"volume_dependencies\":[\"$VOL_ID\"]}")"
  ALWAYS_ID=$(app_id)
  expect_eq "docker(restart=always) + 依赖卷 → unmanaged" "unmanaged" \
    "$(dget '["dependency_protection"]')"
  expect_contains "…note 点明是 restart 策略绕开了面板" "always" "$(dget '["dependency_protection_note"]')"

  # 5.4 systemd + 依赖卷 → 一律 unmanaged：P09.4 不改写单元文件。
  expect_eq "systemd 应用带依赖卷" "200" \
    "$(apppatch sysd "$SYS_ID" "\"volume_dependencies\":[\"$VOL_ID\"]")"
  expect_eq "systemd + 依赖卷 → unmanaged" "unmanaged" \
    "$(appget sysd "$SYS_ID" '["dependency_protection"]')"
  expect_contains "…note 说明面板没有改写单元的开机依赖" "RequiresMountsFor" \
    "$(jget "$PAYLOADS/app-sysd.json" 'd["data"]["dependency_protection_note"]')"
else
  notrun "依赖保护判定（docker managed / restart=always / systemd）" "没有可用的依赖卷夹具"
fi

echo
echo "== 6. 启动门禁：拦启动、不拦停机、只收紧 =="
if [ -z "$VOL_ID" ]; then
  notrun "依赖卷门禁" "没有非 mounted 的受管理卷，无法构造 VOLUME_OFFLINE"
else
  expect_eq "卷此刻确实未挂载（独立证据）" "True" \
    "$(python3 -c 'import sys;print(sys.argv[1] != "mounted")' "$(volstate "$VOL_ID")")"

  # 6.1 未挂载的依赖卷 → start 预检被门禁钉成不允许。
  CODE=$(preflight start-gated "{\"action\":\"container.control\",\"params\":{\"container_id\":\"$ID_GATED\",\"operation\":\"start\"}}")
  expect_eq "start 预检仍返回 200（判决在 allowed 里，不是 HTTP 错误）" "200" "$CODE"
  expect_eq "…allowed=false" "False" "$(pfget start-gated '["allowed"]')"
  expect_eq "…阻塞码是 VOLUME_OFFLINE" "True" \
    "$(pfexpr start-gated 'any(b["code"] == "VOLUME_OFFLINE" for b in d["data"]["blockers"])')"
  expect_contains "…阻塞项里写着卷标识" "$VOL_ID" "$(pfget start-gated '["blockers"]')"

  # 6.2 提交被钉住的预检 → 409 DEPENDENCY_BLOCKED，容器状态不变。
  PF=$(pfget start-gated '["preflight_id"]')
  expect_eq "预检标识存在" "True" \
    "$(python3 -c 'import sys;print(sys.argv[1].startswith("pf_"))' "$PF")"
  SUB=$(submit_job gated "$PF")
  expect_eq "提交被门禁拦住的预检 → 409" "409" "$SUB"
  expect_eq "…错误码 DEPENDENCY_BLOCKED" "DEPENDENCY_BLOCKED" "$(ecode)"
  expect_eq "…容器没有被启动（独立证据：docker 报告 exited）" "exited" "$(cstate "$NAME_GATED")"

  # 6.3 stop 不在门禁范围内：同一台机器、同一个未挂载的卷，停机预检不被拦。
  expect_eq "给 running 容器加上同一个依赖卷" "200" \
    "$(apppatch docker-ok-dep "$DOCKER_OK" "\"volume_dependencies\":[\"$VOL_ID\"]")"
  CODE=$(preflight stop-ok "{\"action\":\"container.control\",\"params\":{\"container_id\":\"$ID_OK\",\"operation\":\"stop\"}}")
  expect_eq "stop 预检" "200" "$CODE"
  expect_eq "…stop 不被门禁拦住（allowed=true）" "True" "$(pfget stop-ok '["allowed"]')"
  expect_eq "…也没有 VOLUME_ 开头的阻塞项" "False" \
    "$(pfexpr stop-ok 'any(b["code"].startswith("VOLUME_") for b in d["data"]["blockers"])')"

  # 6.4 门禁只对“有依赖关系的应用”生效：没登记成应用的容器不被拦。
  CODE=$(preflight stop-unbound "{\"action\":\"container.control\",\"params\":{\"container_id\":\"$ID_ALWAYS\",\"operation\":\"stop\"}}")
  expect_eq "未绑定为应用的容器 stop 预检" "200" "$CODE"
  expect_eq "…没有 VOLUME_ 阻塞项" "False" \
    "$(pfexpr stop-unbound 'any(b["code"].startswith("VOLUME_") for b in d["data"]["blockers"])')"
fi

if [ -n "$VOL_ID" ]; then
  # 6.5 正对照：把同一个夹具卷挂起来，同一份 start 预检 allowed=true——拦住它的是卷的
  # 状态，不是“有依赖就一律拦”。
  volrefresh
  VVERSION=$(vfield "$VOL_ID" version)
  VMOUNT=$(vfield "$VOL_ID" mount_path)
  run_action storage.volume.mount "{\"volume_id\":\"$VOL_ID\",\"version\":\"$VVERSION\"}" mount
  expect_eq "挂载夹具卷：任务终态" "succeeded" "$JOB_STATE"
  volrefresh
  expect_eq "…卷现在挂着（observed_state）" "mounted" "$(volstate "$VOL_ID")"
  expect_eq "…独立证据：内核挂载表里有它" "$VMOUNT" \
    "$(findmnt -n -o TARGET "$VMOUNT" 2>/dev/null | head -1)"

  RESULT=$(preflight start-mounted "{\"action\":\"container.control\",\"params\":{\"container_id\":\"$ID_GATED\",\"operation\":\"start\"}}")
  expect_eq "正对照：预检返回 200" "200" "$RESULT"
  expect_eq "正对照：卷挂着时 allowed=true（门禁没拦）" "True" "$(pfget start-mounted '["allowed"]')"
  expect_eq "正对照：没有任何 VOLUME_ 阻塞项" "False" \
    "$(pfexpr start-mounted 'any(b["code"].startswith("VOLUME_") for b in d["data"]["blockers"])')"
  # protection 判定看的是重启策略，不跟着 allowed 走：卷挂着也仍是 managed。
  expect_eq "卷挂着时 protection 仍是 managed" "managed" \
    "$(appget docker-gated "$GATED_ID" '["dependency_protection"]')"

  # 再卸回去：后面的段落（以及下一次运行）看到的仍是一个未挂载的卷。
  volrefresh
  VVERSION=$(vfield "$VOL_ID" version)
  run_action storage.volume.unmount "{\"volume_id\":\"$VOL_ID\",\"version\":\"$VVERSION\"}" unmount
  expect_eq "卸载夹具卷：任务终态" "succeeded" "$JOB_STATE"
  volrefresh
  expect_eq "…卷回到未挂载（门禁重新生效）" "unmounted" "$(volstate "$VOL_ID")"
else
  notrun "门禁正对照（挂载中的依赖卷不拦）" "没有可用的依赖卷夹具"
fi

echo
echo "== 7. 容器重建：ID 变了就是看不见，要求重新确认绑定 =="
expect_eq "重建前的容器应用创建" "201" \
  "$(create_app docker-rebuild "{\"name\":\"会被重建的容器\",\"type\":\"docker\",\"binding\":{\"container_id\":\"$ID_REBUILD\"}}")"
REBUILD_APP=$(app_id)
expect_eq "重建前 runtime_state" "running" "$(dget '["runtime_state"]')"
docker rm -f "$NAME_REBUILD" >/dev/null 2>&1
NEW_ID=$(docker run -d --name "$NAME_REBUILD" "$CNT_IMAGE" sleep 3600)
expect_ne "重建出来的容器 ID 与旧的不同（独立证据）" "$ID_REBUILD" "$NEW_ID"
expect_eq "旧绑定 → runtime_state=unknown" "unknown" \
  "$(appget docker-rebuild "$REBUILD_APP" '["runtime_state"]')"
expect_contains "…note 说明容器可能已被重建、要重新确认绑定" "重新确认绑定" \
  "$(jget "$PAYLOADS/app-docker-rebuild.json" 'd["data"]["runtime_note"]')"
expect_eq "…不出现 start 能力（看不见的容器没有按钮）" "False" \
  "$(jget "$PAYLOADS/app-docker-rebuild.json" 'd["data"]["capabilities"]["start"]')"
expect_eq "…也不给日志能力" "False" \
  "$(jget "$PAYLOADS/app-docker-rebuild.json" 'd["data"]["capabilities"]["logs"]')"
expect_eq "…名字相同但不自动接手（库里仍是旧 ID）" "{\"container_id\":\"$ID_REBUILD\"}" \
  "$(dbq "SELECT binding_json FROM apps WHERE id = '$REBUILD_APP'")"

echo
echo "== 8. /apps/:id/logs：只判别，不开新口子 =="
expect_eq "systemd 应用日志" "200" "$(api "$JAR" "$CSRF" GET "$APPS/$SYS_ID/logs")"
cp "$WORK/resp.json" "$PAYLOADS/applogs-journal.json"
expect_eq "…source=journal" "journal" "$(dget '["source"]')"
expect_eq "…unit 来自绑定" "$FIXTURE_UNIT" "$(dget '["unit"]')"
expect_eq "…page 是 /logs 的同一套形状（带 source）" "journal" "$(dget '["page"]["source"]')"
expect_eq "…page 里没有 container_id 这套字段" "False" \
  "$(jget "$PAYLOADS/applogs-journal.json" '"container_id" in d["data"]["page"]')"
expect_eq "…条目是数组（没有日志也是 []，不是 null）" "True" \
  "$(jget "$PAYLOADS/applogs-journal.json" 'isinstance(d["data"]["page"]["entries"], list)')"

expect_eq "docker 应用日志" "200" "$(api "$JAR" "$CSRF" GET "$APPS/$DOCKER_OK/logs")"
cp "$WORK/resp.json" "$PAYLOADS/applogs-docker.json"
expect_eq "…source=docker" "docker" "$(dget '["source"]')"
expect_eq "…container_id 来自绑定" "$ID_OK" "$(dget '["container_id"]')"
expect_eq "…page 是 /containers/:id/logs 的同一套形状" "$ID_OK" "$(dget '["page"]["container_id"]')"
expect_eq "…page 里没有 unit/level 这套字段" "False" \
  "$(jget "$PAYLOADS/applogs-docker.json" '"unit" in d["data"]["page"] or "level" in d["data"]["page"]')"

# 8.1 对象来自绑定：请求里给 unit/source 一律 400，而不是被悄悄忽略。
expect_eq "journal 应用带 unit 参数 → 400" "400" \
  "$(api "$JAR" "$CSRF" GET "$APPS/$SYS_ID/logs?unit=other.service")"
expect_eq "…错误码 VALIDATION_ERROR" "VALIDATION_ERROR" "$(ecode)"
expect_contains "…说明绑定决定对象" "绑定" "$(emsg)"
expect_eq "journal 应用带 source 参数 → 400" "400" \
  "$(api "$JAR" "$CSRF" GET "$APPS/$SYS_ID/logs?source=kernel")"
expect_eq "journal 应用带 docker 的 tail → 400" "400" \
  "$(api "$JAR" "$CSRF" GET "$APPS/$SYS_ID/logs?tail=10")"
expect_eq "docker 应用带 level → 400" "400" \
  "$(api "$JAR" "$CSRF" GET "$APPS/$DOCKER_OK/logs?level=error")"
expect_eq "docker 应用带 from → 400" "400" \
  "$(api "$JAR" "$CSRF" GET "$APPS/$DOCKER_OK/logs?from=2026-01-01T00:00:00Z")"
expect_eq "docker 应用带 unit → 400" "400" \
  "$(api "$JAR" "$CSRF" GET "$APPS/$DOCKER_OK/logs?unit=ssh.service")"
# 8.2 合法的来源内参数仍然传下去。
expect_eq "journal 应用带 level 与 limit" "200" \
  "$(api "$JAR" "$CSRF" GET "$APPS/$SYS_ID/logs?level=info&limit=5")"
expect_eq "…limit 生效（条目数不超过 5）" "True" \
  "$(dget '["page"]["entries"].__len__() <= 5')"
expect_eq "docker 应用带 tail" "200" "$(api "$JAR" "$CSRF" GET "$APPS/$DOCKER_OK/logs?tail=3")"

# 8.3 link 与 compose：没有可读的运行时，明确拒绝而不是空页。
expect_eq "link 应用日志 → 422" "422" "$(api "$JAR" "$CSRF" GET "$APPS/$LINK_ID/logs")"
expect_eq "…错误码 VALIDATION_ERROR" "VALIDATION_ERROR" "$(ecode)"
DEP_ID=$(compose_id "$COMPOSE_PROJECT")
expect_eq "compose 应用创建" "201" \
  "$(create_app compose "{\"name\":\"监控栈\",\"type\":\"compose\",\"binding\":{\"deployment_id\":\"$DEP_ID\"}}")"
COMPOSE_APP=$(app_id)
expect_eq "compose 的 runtime_state 是 unknown（部署能力 1.0 才有）" "unknown" "$(dget '["runtime_state"]')"
expect_contains "…note 说明部署能力未落地" "1.0" "$(dget '["runtime_note"]')"
expect_eq "compose 应用日志 → 501" "501" "$(api "$JAR" "$CSRF" GET "$APPS/$COMPOSE_APP/logs")"
expect_eq "…错误码 NOT_IMPLEMENTED" "NOT_IMPLEMENTED" "$(ecode)"
# 独立证据：面板给这个 compose 项目算出的 dep_ 标识与脚本自己算的一致。
expect_eq "compose 项目标识与独立计算一致" "$DEP_ID" \
  "$(api "$JAR" "$CSRF" GET "$DEPLOYMENTS" >/dev/null; jget "$WORK/resp.json" "next((p[\"id\"] for p in d[\"data\"][\"projects\"] if p[\"name\"] == \"$COMPOSE_PROJECT\"), \"\")")"

# 8.4 权限：日志是 admin 的（里面有口令形态的字符串、内网地址）。
expect_eq "建 viewer 账号" "201" \
  "$(api "$JAR" "$CSRF" POST "$USERS" "{\"username\":\"p094viewer\",\"display_name\":\"观察员\",\"role\":\"viewer\",\"initial_password\":\"$VIEWER_PASSWORD\"}")"
VJAR=$WORK/viewer.jar
expect_eq "viewer 登录" "200" "$(login p094viewer "$VIEWER_PASSWORD" "$VJAR" login-viewer)"
VCSRF=$(csrf_of "$WORK/login-viewer.json")
expect_eq "viewer 读应用列表（摘要可见）" "200" "$(api "$VJAR" "$VCSRF" GET "$APPS")"
expect_eq "viewer 读应用日志 → 403" "403" "$(api "$VJAR" "$VCSRF" GET "$APPS/$SYS_ID/logs")"
expect_eq "匿名读应用日志 → 401" "401" "$(api_anon GET "$APPS/$SYS_ID/logs")"

echo
echo "== 9. 代理停掉：说“没读到”，不说“停了” =="
stop_agent
expect_eq "代理停掉后 /apps 仍是 200（不因代理不可用而 503）" "200" "$(api "$JAR" "$CSRF" GET "$APPS")"
cp "$WORK/resp.json" "$PAYLOADS/apps-agent-down.json"
expect_eq "docker 应用的 runtime_state 变成 unknown" "unknown" \
  "$(jget "$PAYLOADS/apps-agent-down.json" "next((a[\"runtime_state\"] for a in d[\"data\"][\"items\"] if a[\"id\"] == \"$DOCKER_OK\"), \"missing\")")"
expect_contains "…note 说明这一轮没读到" "没有读到" \
  "$(jget "$PAYLOADS/apps-agent-down.json" "next((a[\"runtime_note\"] or \"\" for a in d[\"data\"][\"items\"] if a[\"id\"] == \"$DOCKER_OK\"), \"\")")"
if [ -n "$VOL_ID" ]; then
  # 有依赖卷、又读不到容器状态：判断不了它会不会在卷挂载前自己起来，因此不能说 managed。
  expect_eq "…依赖保护同时降级成 unmanaged（判断不了就不能说接管了）" "unmanaged" \
    "$(jget "$PAYLOADS/apps-agent-down.json" "next((a[\"dependency_protection\"] for a in d[\"data\"][\"items\"] if a[\"id\"] == \"$DOCKER_OK\"), \"missing\")")"
else
  notrun "代理停掉时的依赖保护降级" "这个夹具没有依赖卷（没有可用的卷时跳过）"
fi
expect_eq "link 应用不受影响（本来就不需要代理）" "not_applicable" \
  "$(jget "$PAYLOADS/apps-agent-down.json" "next((a[\"runtime_state\"] for a in d[\"data\"][\"items\"] if a[\"id\"] == \"$LINK_ID\"), \"missing\")")"
expect_eq "应用日志在代理停掉时是 503" "503" "$(api "$JAR" "$CSRF" GET "$APPS/$SYS_ID/logs")"
if start_agent; then pass "代理重新启动（收尾用）"; else fail "代理重新启动" "$(tail -3 "$AGENT_LOG" 2>/dev/null)"; fi

echo
echo "== 10. 收尾：把登记的夹具卷退出管理（还原机器） =="
if [ -z "$VOL_ID" ]; then
  notrun "退出管理夹具卷" "这次运行没有登记夹具卷"
else
  volrefresh
  VOL_UUID=$(vfield "$VOL_ID" filesystem_uuid)
  VVERSION=$(vfield "$VOL_ID" version)
  run_action storage.volume.forget "{\"volume_id\":\"$VOL_ID\",\"version\":\"$VVERSION\"}" forget
  expect_eq "退出管理：预检" "200" "$PRE_CODE"
  expect_eq "…预检允许" "True" "$PRE_ALLOWED"
  expect_eq "…任务终态" "succeeded" "$JOB_STATE"
  expect_eq "…独立证据：fstab 里不再有它的 UUID" "0" \
    "$(grep -c "UUID=$VOL_UUID" /etc/fstab 2>/dev/null || true)"
  volrefresh
  expect_eq "…独立证据：卷列表里不再有它" "missing" "$(vfield "$VOL_ID" id)"
fi

echo
echo "== 汇总 =="
printf 'PASS %s / FAIL %s / NOT RUN %s\n' "$PASSED" "$FAILED" "$NOTRUN"
if [ "$FAILED" -gt 0 ]; then
  printf 'FAILED:%s\n' "$FAILED_NAMES"
  exit 1
fi
exit 0
