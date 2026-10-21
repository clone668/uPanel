#!/bin/sh
# P06.3 Linux 验收脚本：受限 Web 终端——以 upanel-shell 启动 PTY、浏览器式
# WebSocket 握手（Origin/子协议/一次性许可）、帧协议、输入限速、窗口调整、
# 断流宽限、空闲与最长会话、数量上限、关闭与元数据审计。
#
# 运行位置：Ubuntu 24.04 验收 VM 内，**必须以 root 运行**
#   （代理只能在 root 下启动；还要能创建 upanel-shell 账号、读代理密钥验证
#    它读不到、以及查询审计库）。
# 不依赖 Go 工具链：用宿主交叉编译好的 Linux 二进制。
#
# 用法：
#   sudo BIN_DIR=/tmp/upanel-bin sh tests/integration/p06-3-terminal-acceptance.sh
#
# 可选环境变量：
#   PORT=19463        Web 监听端口
#   SKIP_SLOW=1       跳过长等待段（空闲 60 秒 / 最长 120 秒），并把它们记为 NOT RUN
#
# 退出码：0 全部通过；1 有失败项；2 环境不满足（非 root / 缺依赖）。
#
# 覆盖的验收点（STAGES §8 P06.3、CONTRACTS §10、PLAN §14/§15）：
#   1. 能力表登记 terminal.create 与 terminal.sessions（账号不可用时应**不登记**，见第 19 节）
#   2. terminal.create：预检报告 interactive_access、要求再认证、不要短语；
#      未再认证提交 403 REAUTH_REQUIRED 且不产生执行；再认证后任务 succeeded，
#      结果里 session_id / 账号 / uid / 状态 / 时限齐全并读回一致
#   3. 会话列表：账号与三个时限、两个数量上限、attached_at 为空；
#      别人的账号看不到、也领不到许可（404，不是 403 也不是空列表）
#   4. 连接许可：必须带 CSRF；形状是 32 位十六进制；只经子协议传递；
#      30 秒过期；**一次性**（同一张再用是 403）
#   5. 握手四道门：无 Origin / 错 Origin → 403；无子协议 → 400；
#      有子协议无许可、许可不属于该会话 → 403；每次拒绝都留下审计阶段
#   6. 连上之后真的是 upanel-shell：uid 与配置一致、属组只有自己、
#      sudo 不可用、/etc/shadow 与代理密钥读不到、系统目录写不了、
#      有真 PTY（/dev/pts/*）、cwd 是 home、环境里没有代理的东西
#   7. 窗口调整真的到了 PTY（stty size 读回 43 132）
#   8. 帧协议闭集：客户端发 run/output 之类的帧、非 JSON、空 input、
#      超过 64 KiB 的 input、超过帧上限的输入 → FRAME_INVALID / 被断开，
#      且**没有任何命令被执行**（用一个不存在的文件当探针）
#   9. 输入速率上限：一次 400 帧 → RATE_LIMITED，连接被断开，会话还在宽限期里
#  10. 单连接：已连接时再连一个 → 409 RESOURCE_BUSY（不是"悄悄踢掉前一个"）
#  11. 断流宽限：不发 close 直接断 TCP，会话变 detached；宽限内重连能接上
#      **同一个 PTY**（上一连接里 export 的变量还在）
#  12. 宽限到期：会话从表里消失（404），upanel-shell 的 shell 进程被回收
#  13. 空闲关闭：60 秒无输入 → exit 帧 reason=idle
#  14. 最长会话：120 秒到点 → exit 帧 reason=expired（即使一直有活动）
#  15. 数量上限：每账号 1 个（预检就给出 TERMINAL_SESSION_LIMIT 阻塞项）、
#      全局 2 个（第三个账号的任务 failed 且 code=RESOURCE_BUSY）
#  16. 关闭：reason 不在枚举内 400、未带 CSRF 403、关闭成功、**重复关闭同样成功**
#  17. 审计：terminal.attach（成功 + origin/protocol/grant 阶段失败）与 terminal.close
#      都有行；审计里没有终端内容（标记串不出现、没有超长字段）
#  18. 权限：匿名 401、viewer 403（列表、领许可、连、关四处）
#  19. 账号不可用时：代理**不登记**终端能力，Web 的四个接口是 501 NOT_IMPLEMENTED
#      （而不是 400 或 500），且指出的原因是账号本身
#  20. 回归：概览/文件/服务/日志/进程在加了终端之后仍然正常
#
# 明确不覆盖（NOT RUN，原因写在这里，不在脚本里假装通过）：
#   - 浏览器渲染与终端页交互（xterm.js、粘贴、滚动）：VM 上没有浏览器。
#     页面级证据在宿主上用真实载荷回放，帧协议与握手在这一侧已经逐帧验证。
#   - 契约的默认时限本身（15 分钟空闲 / 1 小时最长 / 60 秒宽限）：验收不可能等
#     一小时。脚本把三个时限**收紧**到 60 / 120 / 10 秒，验证的是同一条代码路径
#     （config 的 Validate 只允许收紧，放宽会被拒绝——那条由单元用例覆盖）。
#   - 65 条连接同时打开（全局上限 2 已验）、100 个账号并发创建（同一条互斥）。
#   - Web 进程崩溃后会话的归宿：代理侧 10 秒宽限到期即回收，与第 12 节同一路径；
#     真正的崩溃注入需要杀 Web 进程，会打断本轮后续断言。
#   - 真实磁盘满 / fork 失败（EMFILE/ENOMEM）：由 internal/terminal 的单元用例覆盖。
#   - 代理重启后 Web 侧的终端状态：代理重启会清空会话表，属停机行为，不是功能。

set -u

BIN_DIR=${BIN_DIR:-/tmp/upanel-bin}
HERE=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
WS_CLIENT=${WS_CLIENT:-$HERE/p06-3-ws-client.py}

if [ "$(id -u)" -ne 0 ]; then
  printf '本脚本必须以 root 运行：sudo BIN_DIR=%s sh %s\n' "$BIN_DIR" "$0" >&2
  exit 2
fi

AGENT="$BIN_DIR/upanel-agent"
WEB="$BIN_DIR/upanel-web"
CTL="$BIN_DIR/upanelctl"

# 传输过来的二进制默认是 0644（scp 不带 -p 时按 umask 落地），执行位必须先补上，
# 否则第一个 "启动代理" 步骤会因为 Permission denied 而整轮失去意义。这里顺手
# 补执行位，不改属主和内容。
chmod +x "$AGENT" "$WEB" "$CTL" 2>/dev/null || true

WORK=${WORK:-/var/tmp/upanel-accept-p06-3}
PAYLOADS=$WORK/payloads
PLANS=$WORK/plans
RUN_DIR=/run/upanel
SOCK=$RUN_DIR/agent.sock
SYS_CFG=/etc/upanel/upanel.json
AGENT_DB_DIR=/var/lib/upanel-agent
AGENT_DB=$AGENT_DB_DIR/agent.db
KEY_FILE=/etc/upanel/agent.key
WEB_DB_DIR=$WORK/webdata
WEB_DB=$WEB_DB_DIR/upanel.db
PORT=${PORT:-19463}
BASE="http://127.0.0.1:$PORT"
ORIGIN="$BASE"
WEB_USER=upanel-web
SHELL_USER=upanel-shell
SHELL_HOME=/var/lib/upanel-shell
PASSWORD='vm-correct-horse-9'
ADMIN2_PASSWORD='vm-correct-horse-7'
ADMIN3_PASSWORD='vm-correct-horse-6'
VIEWER_PASSWORD='vm-correct-horse-8'

# 三个时限只有收紧：契约是 900 / 3600 / 60 秒，这里压到 60 / 120 / 10 秒。
IDLE_SECONDS=60
MAX_SECONDS=120
GRACE_SECONDS=10

TERM=/api/v1/terminal
COOKIE_NAME=__Host-upanel_session
PROBE=/etc/upanel/p06-3-should-not-exist

AGENT_LOG=$WORK/logs/agent.log
WEB_LOG=$WORK/logs/web.log
AGENT_PID=""
WEB_PID=""

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
expect_contains() {
  case "$3" in
    *"$2"*) pass "$1" ;;
    *) fail "$1" "输出里没有 '$2'" ;;
  esac
}
expect_not_contains() {
  case "$3" in
    *"$2"*) fail "$1" "输出里不该出现 '$2'" ;;
    *) pass "$1" ;;
  esac
}

# expect_steps <标签> <报告名> <逗号分隔的期望名>
#
# 断言客户端计划里的每一条期望都**执行过且成立**。"没执行"与"执行了但失败"
# 要分开：计划在握手阶段就断了的时候，后面的步骤根本不会跑，这时"没有失败的
# 期望"绝不能算通过。
expect_steps() {
  _label=$1; _name=$2; _want=$3
  _res=$(python3 - "$WORK/ws-$_name.json" "$_want" <<'PY'
import json, sys
try:
    report = json.load(open(sys.argv[1], encoding="utf-8"))
except Exception as exc:
    print("FAIL:报告读不出来（%s）" % exc)
    raise SystemExit(0)
want = [w for w in sys.argv[2].split(",") if w]
seen = {e.get("name"): e.get("ok") for e in report.get("expectations", [])}
missing = [w for w in want if w not in seen]
failed = [w for w in want if w in seen and not seen[w]]
if missing or failed:
    print("FAIL:未执行=%s 未成立=%s" % (missing, failed))
else:
    print("OK:%d" % len(want))
PY
)
  case "$_res" in
    OK:*) pass "$1（$(printf '%s' "$_res" | cut -d: -f2) 条期望全部成立）" ;;
    *) fail "$1" "$_res" ;;
  esac
}

# jget <文件> <python 表达式，d 为解析后的 JSON>
# 表达式写错时打印 ERR:<类型>:<消息>，避免"表达式错"被当成"字段为空"。
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
resp_get() { jget "$WORK/resp.json" "$1"; }
pget() { jget "$WORK/preflight.json" "$1"; }
jobget() { jget "$WORK/job.json" "$1"; }
grant_get() { jget "$WORK/grant.json" "$1"; }
wsget() { jget "$WORK/ws-$1.json" "$2"; }
wsout() { jget "$WORK/ws-$1.json" 'd["output"]'; }

cleanup() {
  stop_web
  stop_agent
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

# api <jar> <csrf> <方法> <路径> [body]：响应写到 $WORK/resp.json，回显状态码。
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

# apf <jar> <csrf> <动作> <参数 JSON>：预检结论存 $WORK/preflight.json，回显状态码。
apf() {
  printf '{"action":"%s","params":%s}' "$3" "$4" >"$WORK/pre.json"
  _code=$(curl -s -b "$1" -o "$WORK/resp.json" -w '%{http_code}' \
    -X POST "$BASE/api/v1/operations/preflight" \
    -H 'Content-Type: application/json' -H "Origin: $ORIGIN" -H "X-CSRF-Token: $2" \
    -d @"$WORK/pre.json")
  cp "$WORK/resp.json" "$WORK/preflight.json"
  printf '%s' "$_code"
}
preflight() { apf "$JAR" "$CSRF" "$1" "$2"; }

# asub <jar> <csrf> <preflight_id> <短语> <幂等键>：响应存 $WORK/submit.json。
asub() {
  python3 - "$3" "${4:-}" >"$WORK/jobreq.json" <<'PY'
import json, sys
print(json.dumps({"preflight_id": sys.argv[1], "confirmation_phrase": sys.argv[2]}))
PY
  _code=$(curl -s -b "$1" -o "$WORK/resp.json" -w '%{http_code}' -X POST "$BASE/api/v1/jobs" \
    -H 'Content-Type: application/json' -H "Origin: $ORIGIN" -H "X-CSRF-Token: $2" \
    -H "Idempotency-Key: $5" -d @"$WORK/jobreq.json")
  cp "$WORK/resp.json" "$WORK/submit.json"
  printf '%s' "$_code"
}
submit_job() { asub "$JAR" "$CSRF" "$@"; }

# await_job <jar> <job_id>：轮询到终态，任务视图存 $WORK/job.json，回显终态。
await_job() {
  _i=0
  while [ "$_i" -lt 150 ]; do
    curl -s -b "$1" -o "$WORK/resp.json" "$BASE/api/v1/jobs/$2" >/dev/null
    cp "$WORK/resp.json" "$WORK/job.json"
    _st=$(jobget 'd["data"]["status"]')
    case "$_st" in
      succeeded|failed|canceled|interrupted) printf '%s' "$_st"; return 0 ;;
    esac
    _i=$((_i + 1))
    sleep 0.2
  done
  printf 'timeout'
}

# submit_and_wait <preflight_id> <短语> <幂等键>：提交阶段被拒时回显 submit:<码>。
submit_and_wait() {
  _code=$(submit_job "$1" "${2:-}" "$3")
  if [ "$_code" != "202" ]; then printf 'submit:%s' "$_code"; return 0; fi
  await_job "$JAR" "$(jget "$WORK/submit.json" 'd["data"]["job_id"]')"
}

# run_as <jar> <csrf> <动作> <参数> <短语> <幂等键>：换账号跑一次动作。
run_as() {
  _pf=$(apf "$1" "$2" "$3" "$4")
  if [ "$_pf" != "200" ]; then printf 'preflight:%s' "$_pf"; return 0; fi
  _id=$(pget 'd["data"]["preflight_id"]')
  _sc=$(asub "$1" "$2" "$_id" "${5:-}" "$6")
  if [ "$_sc" != "202" ]; then printf 'submit:%s' "$_sc"; return 0; fi
  await_job "$1" "$(jget "$WORK/submit.json" 'd["data"]["job_id"]')"
}

jstatus() { jobget 'd["data"]["status"]'; }
joutcome() { jobget 'd["data"]["result"]["outcome"]'; }
jerr() { jobget 'd["data"]["error"]["code"]'; }
jobserved() { jobget "d[\"data\"][\"result\"][\"observed_state\"][\"$1\"]"; }
jdata() { jobget "d[\"data\"][\"result\"][\"data\"].get(\"$1\")"; }
pblocker() { pget "d[\"data\"][\"blockers\"][$1][\"code\"]"; }
pimpact() { pget "d[\"data\"][\"impacts\"][$1][\"kind\"]"; }

# cookie_of <jar>：从 curl 的 cookie 罐里取会话值（__Host- 前缀也在第 6 列）。
cookie_of() { awk -v n="$COOKIE_NAME" '$6 == n { print $7 }' "$1" | tail -1; }

# ── 终端 HTTP 面 ─────────────────────────────────────────────────────────────

sessions_list() { api "$JAR" "$CSRF" GET "$TERM/sessions"; }
# grant <jar> <csrf> <session_id>：领一张连接许可，响应存 $WORK/grant.json。
grant() {
  _code=$(curl -s -b "$1" -o "$WORK/resp.json" -w '%{http_code}' -X POST \
    "$BASE$TERM/sessions/$3/connection-grants" -H "Origin: $ORIGIN" -H "X-CSRF-Token: $2")
  cp "$WORK/resp.json" "$WORK/grant.json"
  printf '%s' "$_code"
}
grant_as() { grant "$JAR" "$CSRF" "$1"; }

# close_session <jar> <csrf> <session_id> [reason]：DELETE，响应存 resp.json。
close_session() {
  _path="$TERM/sessions/$3"
  [ -n "${4:-}" ] && _path="$_path?reason=$4"
  api "$1" "$2" DELETE "$_path"
}
# session_state_as <jar> <csrf> <session_id>：用指定账号的会话列表读状态。
# 别人的会话在列表里本来就不出现，所以“查会话状态”必须用**它自己的**账号：
# 用 admin1 的罐子查 admin2 的会话永远得到 gone，而那是断言自己造的假象。
session_state_as() {
  api "$1" "$2" GET "$TERM/sessions" >/dev/null
  resp_get "next((s[\"state\"] for s in d[\"data\"][\"sessions\"] if s[\"session_id\"] == \"$3\"), \"gone\")"
}
session_state() { session_state_as "$JAR" "$CSRF" "$1"; }
# wait_state_as <jar> <csrf> <session_id> <状态|gone> <最多等几秒>：轮询到状态变化。
wait_state_as() {
  _i=0
  _want=$4
  _limit=$(python3 -c "print(int(float('$5')/0.25))")
  _cur=$(session_state_as "$1" "$2" "$3")
  while [ "$_i" -lt "$_limit" ]; do
    _cur=$(session_state_as "$1" "$2" "$3")
    if [ "$_cur" = "$_want" ] || { [ "$_want" = "gone" ] && [ "$_cur" = "gone" ]; }; then
      printf '%s' "$_cur"; return 0
    fi
    # 列表里的状态只会少，不会错：已经 gone 就不必再等。
    if [ "$_cur" = "gone" ] && [ "$_want" != "gone" ]; then printf '%s' "$_cur"; return 0; fi
    _i=$((_i + 1))
    sleep 0.25
  done
  printf '%s' "$_cur"
}
# wait_state <session_id> <状态|gone> <最多等几秒>：admin1 视角。
wait_state() { wait_state_as "$JAR" "$CSRF" "$1" "$2" "$3"; }

# ── WebSocket 客户端 ────────────────────────────────────────────────────────
#
# ws <名称> <路径> <origin|none> <Cookie 头> <协议列表> <计划文件|-> [额外参数...]
# 报告写 $WORK/ws-<名称>.json，客户端自己的输出写 $WORK/ws-<名称>.log，回显退出码。
ws() {
  _name=$1; _path=$2; _origin=$3; _ck=$4; _proto=$5; _plan=$6
  shift 6
  set -- "$@" --url "ws://127.0.0.1:$PORT$_path" --origin "$_origin" --cookie "$_ck" \
    --protocol "$_proto" --out "$WORK/ws-$_name.json"
  if [ -n "$_plan" ] && [ "$_plan" != "-" ]; then set -- "$@" --plan "$_plan"; fi
  python3 "$WS_CLIENT" "$@" >"$WORK/ws-$_name.log" 2>&1
  printf '%s' $?
}

# connect <名称> <session_id> <计划|-> [额外参数...]：领一张新许可再连（正常路径）。
connect() {
  _name=$1; _sid=$2; _plan=$3
  shift 3
  _gc=$(grant_as "$_sid")
  if [ "$_gc" != "200" ]; then printf 'grant:%s' "$_gc"; return 0; fi
  _g=$(grant_get 'd["data"]["grant"]')
  ws "$_name" "$TERM/sessions/$_sid/connect" "$ORIGIN" "$CK" \
    "upanel.term.v1,upanel.grant.$_g" "$_plan" "$@"
}
# expect_ok <名称> <名称>：断言这次连接真的升级成功（101 + accept 正确）。
expect_ws_up() {
  expect_eq "$1 状态码 101" "101" "$(wsget "$2" 'd["status"]')"
  expect_eq "$1 协商到 upanel.term.v1" "upanel.term.v1" "$(wsget "$2" 'd["subprotocol"]')"
  expect_eq "$1 accept 校验" "True" "$(wsget "$2" 'd["accept_ok"]')"
}
# expect_expect <名称> <期望名>：断言客户端计划里的某条期望成立。
expect_named() {
  expect_eq "$1" "True" "$(wsget "$2" "any(e[\"name\"] == \"$3\" and e[\"ok\"] for e in d[\"expectations\"])")"
}

# shell_proc_count：upanel-shell 名下的进程数。宽限期到期后这里必须是 0，
# 否则“会话没了但 shell 还挂着”就只是表里的一句假话。
# pgrep -c 在没有匹配时仍会打印 0 并返回 1，所以这里数行数，而不是用 -c。
shell_proc_count() { pgrep -u "$SHELL_USER" 2>/dev/null | wc -l | tr -d ' '; }

# wait_shell_procs <期望个数> <最多等几秒>：轮询到进程数达到期望值，回显最终个数。
#
# 为什么不能只数一次：会话从表里消失（gone）与子进程被回收完成不是同一个瞬间——
# 前者是会话表的状态，后者要等 SIGKILL 之后内核回收。紧接着 gone 的那一次采样
# 常常正好落在这两步之间，于是断言会因为几百毫秒的时序差而失败，而功能是好的。
wait_shell_procs() {
  _i=0
  _limit=$(python3 -c "print(int(float('$2')/0.25))")
  _cur=$(shell_proc_count)
  while [ "$_i" -lt "$_limit" ]; do
    _cur=$(shell_proc_count)
    if [ "$_cur" = "$1" ]; then printf '%s' "$_cur"; return 0; fi
    _i=$((_i + 1))
    sleep 0.25
  done
  printf '%s' "$_cur"
}

echo "== 0. 环境 =="
for b in "$AGENT" "$WEB" "$CTL"; do
  if [ -x "$b" ]; then pass "二进制存在且可执行 $(basename "$b")"; else fail "二进制存在且可执行 $(basename "$b")" "$b"; fi
done
if [ -r "$WS_CLIENT" ]; then pass "WebSocket 验收客户端存在"; else fail "WebSocket 验收客户端存在" "$WS_CLIENT"; fi
for t in python3 curl runuser stat awk sed ss pgrep; do
  if command -v "$t" >/dev/null 2>&1; then pass "依赖命令可用 $t"; else fail "依赖命令可用 $t" "缺少 $t"; fi
done
python3 -c 'import sys; sys.exit(0 if sys.version_info >= (3, 6) else 1)' \
  && pass "python3 版本 ≥ 3.6" || fail "python3 版本 ≥ 3.6" "$(python3 -V 2>&1)"
if ! id -u "$WEB_USER" >/dev/null 2>&1; then
  note "创建系统账号 $WEB_USER"
  useradd --system --no-create-home --shell /usr/sbin/nologin "$WEB_USER" 2>/dev/null
fi
if id -u "$WEB_USER" >/dev/null 2>&1; then pass "Web 账号 $WEB_USER 存在"; else fail "Web 账号 $WEB_USER 存在" "无法创建"; fi

# 终端账号按安装步骤造：系统账号、无家目录（-M）、显式建 home 并收到 0750。
if ! id -u "$SHELL_USER" >/dev/null 2>&1; then
  note "按安装步骤创建终端账号 $SHELL_USER"
  useradd -r -M -d "$SHELL_HOME" -s /bin/bash "$SHELL_USER" 2>/dev/null
fi
install -d -m 0750 -o "$SHELL_USER" -g "$SHELL_USER" "$SHELL_HOME"
if id -u "$SHELL_USER" >/dev/null 2>&1; then pass "终端账号 $SHELL_USER 存在"; else fail "终端账号 $SHELL_USER 存在" "无法创建"; fi
SHELL_UID=$(id -u "$SHELL_USER" 2>/dev/null)
SHELL_GROUPS=$(id -nG "$SHELL_USER" 2>/dev/null)
expect_eq "终端账号不是 root" "no" "$([ "$SHELL_UID" = 0 ] && echo yes || echo no)"
expect_eq "终端账号的属组只有它自己（不在 sudo/docker/disk 里）" "$SHELL_USER" "$SHELL_GROUPS"
expect_eq "终端账号的 shell 是 /bin/bash" "/bin/bash" "$(getent passwd "$SHELL_USER" | cut -d: -f7)"
expect_eq "终端 home 权限是 0750" "750" "$(stat -c '%a' "$SHELL_HOME" 2>/dev/null)"
expect_eq "终端 home 属主" "$SHELL_USER" "$(stat -c '%U' "$SHELL_HOME" 2>/dev/null)"

if curl -fsS "$BASE/api/v1/health/live" >/dev/null 2>&1; then
  echo "$PORT 上已有服务在监听；先停掉遗留的 upanel-web 再跑验收" >&2
  exit 1
fi
rm -rf "$PROBE" "$WORK"
mkdir -p "$WORK/logs" "$PAYLOADS" "$PLANS" "$WEB_DB_DIR"
chmod 0755 "$WORK" "$PAYLOADS" "$PLANS" "$WORK/logs"
chown -R "$WEB_USER" "$WEB_DB_DIR"
uname -r >"$WORK/logs/kernel.txt" 2>/dev/null || true

echo
echo "== 1. 配置与启动（代理 root、Web 非 root、终端能力已登记） =="
install -d -m 0755 /etc/upanel
install -d -m 0700 "$AGENT_DB_DIR"
install -d -m 0755 "$RUN_DIR"
rm -f "$AGENT_DB" "$AGENT_DB-wal" "$AGENT_DB-shm"
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
    "logs": {},
    "files": { "max_upload_bytes": 1048576, "roots": [] },
    "terminal": {
      "shell_user": "$SHELL_USER",
      "home_path": "$SHELL_HOME",
      "idle_timeout_seconds": $IDLE_SECONDS,
      "max_duration_seconds": $MAX_SECONDS,
      "disconnect_grace_seconds": $GRACE_SECONDS
    }
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
expect_eq "代理密钥文件对其他人不可读" "600" "$(stat -c '%a' "$KEY_FILE" 2>/dev/null)"

if start_agent; then pass "代理以 root 启动并创建 socket"; else fail "代理以 root 启动" "$(tail -5 "$AGENT_LOG" 2>/dev/null)"; fi
expect_eq "代理进程 uid=0" "0" "$(ps -o uid= -p "$AGENT_PID" 2>/dev/null | tr -d ' ')"

runuser -u "$WEB_USER" -- curl -s --unix-socket "$SOCK" \
  http://localhost/internal/v1/capabilities >"$PAYLOADS/agent-capabilities.json" 2>/dev/null
# terminal.create 是**动作**（写操作走预检 → 任务），terminal.sessions 是**查询**
# （只读列表）。两张表分开断言：把它们都放进 supported_actions 会得到一个永远为
# 假的条件，而永远为假的断言等于没断言。
expect_eq "能力表登记了动作 terminal.create" "True" \
  "$(jget "$PAYLOADS/agent-capabilities.json" '
_ = "terminal.create" in d["data"]["supported_actions"]')"
expect_eq "能力表登记了查询 terminal.sessions" "True" \
  "$(jget "$PAYLOADS/agent-capabilities.json" '
_ = "terminal.sessions" in d["data"]["supported_queries"]')"
expect_eq "能力表没有登记任何“执行命令”类能力" "False" \
  "$(jget "$PAYLOADS/agent-capabilities.json" '
_ = bool([a for a in d["data"]["supported_actions"] + d["data"]["supported_queries"]
          if "exec" in a or "shell" in a or "command" in a])')"
note "代理登记的动作数：$(jget "$PAYLOADS/agent-capabilities.json" 'len(d["data"]["supported_actions"])')"
note "代理登记的查询数：$(jget "$PAYLOADS/agent-capabilities.json" 'len(d["data"]["supported_queries"])')"
case "$(grep -c '已启用受限终端' "$AGENT_LOG" 2>/dev/null)" in
  0) fail "代理日志记录了终端启用" "日志里没有“已启用受限终端”" ;;
  *) pass "代理日志记录了终端启用" ;;
esac
# 日志是 JSON：idle_timeout 以 "1m0s" 的形式出现。这里只做回显，
# 真正的判决在第 4 节——时限的值从 API 读回来比对，不靠日志文本。
note "代理日志：$(grep '已启用受限终端' "$AGENT_LOG" 2>/dev/null | head -1 | cut -c1-240)"

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
if [ -n "$CSRF" ]; then pass "拿到 CSRF 令牌"; else fail "拿到 CSRF 令牌" "登录响应里没有 csrf_token"; fi
CK="$COOKIE_NAME=$(cookie_of "$JAR")"
case "$CK" in
  *"=") fail "从 cookie 罐里取到会话值" "值看起来是空的：$CK" ;;
  *) pass "从 cookie 罐里取到会话值" ;;
esac

api "$JAR" "$CSRF" POST /api/v1/users \
  "{\"username\":\"viewer\",\"display_name\":\"只读\",\"role\":\"viewer\",\"initial_password\":\"$VIEWER_PASSWORD\"}" \
  >"$WORK/viewer-code.txt"
expect_eq "创建 viewer 账号" "201" "$(cat "$WORK/viewer-code.txt")"
api "$JAR" "$CSRF" POST /api/v1/users \
  "{\"username\":\"admin2\",\"display_name\":\"管理员二\",\"role\":\"admin\",\"initial_password\":\"$ADMIN2_PASSWORD\"}" \
  >"$WORK/admin2-code.txt"
expect_eq "创建第二个管理员账号" "201" "$(cat "$WORK/admin2-code.txt")"
api "$JAR" "$CSRF" POST /api/v1/users \
  "{\"username\":\"admin3\",\"display_name\":\"管理员三\",\"role\":\"admin\",\"initial_password\":\"$ADMIN3_PASSWORD\"}" \
  >"$WORK/admin3-code.txt"
expect_eq "创建第三个管理员账号" "201" "$(cat "$WORK/admin3-code.txt")"
chown_web_db
VJAR=$WORK/viewer.jar
A2JAR=$WORK/admin2.jar
A3JAR=$WORK/admin3.jar
expect_eq "viewer 登录" "200" "$(login viewer "$VIEWER_PASSWORD" "$VJAR" login-viewer)"
expect_eq "admin2 登录" "200" "$(login admin2 "$ADMIN2_PASSWORD" "$A2JAR" login-admin2)"
CSRF2=$(csrf_of "$WORK/login-admin2.json")
expect_eq "admin3 登录" "200" "$(login admin3 "$ADMIN3_PASSWORD" "$A3JAR" login-admin3)"
CSRF3=$(csrf_of "$WORK/login-admin3.json")

# 计划文件：所有需要时序的断言都用客户端计划表达，避免 shell 里写读循环。
MARKER="p06mark-$(python3 -c 'import uuid;print(uuid.uuid4().hex[:10])')"
KEPT="kept-$(python3 -c 'import uuid;print(uuid.uuid4().hex[:10])')"
python3 - "$PLANS" "$MARKER" "$KEPT" "$SHELL_HOME" <<'PY'
import json, os, sys
plans, marker, kept, home = sys.argv[1], sys.argv[2], sys.argv[3], sys.argv[4]

def write(name, steps):
    with open(os.path.join(plans, name), "w", encoding="utf-8") as fh:
        json.dump({"steps": steps}, fh, ensure_ascii=False, indent=1)

# 身份与边界（TERM-01）。每条断言的期望串都用 "$(...)" 拼出来，
# 这样它不会出现在被 PTY 回显的那一行输入里——否则“回显”就能让断言通过。
ident = [
    {"expect_frame": {"type": "ready", "name": "ready"}},
    {"input": "echo \"USER:$(id -un)\"\r"},
    {"expect_output": {"contains": "USER:%s" % "upanel-shell", "name": "账号名"}},
    {"input": "echo \"UID:$(id -u)\"\r"},
    {"expect_output": {"contains": "UID:%s" % os.environ.get("SHELL_UID", ""), "name": "uid"}},
    {"input": "echo \"GROUPS:$(id -nG)\"\r"},
    {"expect_output": {"contains": "GROUPS:%s" % "upanel-shell", "name": "属组"}},
    {"input": "echo \"PRIV:$(id -nG | tr ' ' '\\n' | grep -cE '^(root|sudo|docker|disk|adm|wheel|shadow)$')\"\r"},
    {"expect_output": {"contains": "PRIV:0", "name": "没有特权组"}},
    {"input": "echo \"PWD:$(pwd)\"\r"},
    {"expect_output": {"contains": "PWD:%s" % home, "name": "工作目录是 home"}},
    {"input": "echo \"HOME:$HOME\"\r"},
    {"expect_output": {"contains": "HOME:%s" % home, "name": "HOME 是 home"}},
    {"input": "echo \"TTY:$(tty)\"\r"},
    {"expect_output": {"contains": "TTY:/dev/pts/", "name": "真的在 PTY 上"}},
    {"input": "echo \"SUDO:$(sudo -n true 2>/dev/null && echo yes || echo no)\"\r"},
    {"expect_output": {"contains": "SUDO:no", "name": "sudo 不可用"}},
    {"input": "echo \"SHADOW:$(cat /etc/shadow >/dev/null 2>&1 && echo readable || echo denied)\"\r"},
    {"expect_output": {"contains": "SHADOW:denied", "name": "/etc/shadow 读不到"}},
    {"input": "echo \"AGENTKEY:$(cat /etc/upanel/agent.key >/dev/null 2>&1 && echo readable || echo denied)\"\r"},
    {"expect_output": {"contains": "AGENTKEY:denied", "name": "代理密钥读不到"}},
    {"input": "echo \"SYSWRITE:$(touch /etc/upanel/p06-3-should-not-exist 2>/dev/null && echo allowed || echo denied)\"\r"},
    {"expect_output": {"contains": "SYSWRITE:denied", "name": "系统目录写不了"}},
    {"input": "echo \"PROC1:$(head -1 /proc/1/comm >/dev/null 2>&1 && echo readable || echo denied)\"\r"},
    {"expect_output": {"contains": "PROC1:readable", "name": "/proc/1 可读（降权不等于被关在外面）"}},
    {"input": "echo \"LEAK:$(env | grep -cE '^(UPANEL|AGENT_|LD_PRELOAD|SUDO_)')\"\r"},
    {"expect_output": {"contains": "LEAK:0", "name": "环境里没有代理/提权变量"}},
    {"input": "echo \"MARKER:%s\"\r" % marker},
    {"expect_output": {"contains": "MARKER:%s" % marker, "name": "标记串"}},
    {"input": "export P06KEPT=%s\r" % kept},
    {"input": "echo \"KEPT:$(echo $P06KEPT)\"\r"},
    {"expect_output": {"contains": "KEPT:%s" % kept, "name": "导出变量"}},
    {"close": True},
]
write("identity.json", ident)

write("resize.json", [
    {"expect_frame": {"type": "ready", "name": "ready"}},
    {"resize": [132, 43]},
    {"input": "echo \"SIZE:$(stty size)\"\r"},
    {"expect_output": {"contains": "SIZE:43 132", "name": "PTY 尺寸 43x132"}},
    {"resize": [80, 24]},
    {"input": "echo \"SIZE2:$(stty size)\"\r"},
    {"expect_output": {"contains": "SIZE2:24 80", "name": "改回 24x80"}},
    {"close": True},
])

# 帧协议违规：每一条都必须是 FRAME_INVALID，并且不执行任何东西。
write("frame-run.json", [
    {"expect_frame": {"type": "ready", "name": "ready"}},
    {"send": {"type": "run", "command": "touch /tmp/p06-3-executed"}},
    {"expect_frame": {"type": "error", "code": "FRAME_INVALID", "name": "run 帧被拒"}},
])
write("frame-output.json", [
    {"expect_frame": {"type": "ready", "name": "ready"}},
    {"send": {"type": "output", "data_b64": "aGk="}},
    {"expect_frame": {"type": "error", "code": "FRAME_INVALID", "name": "output 帧被拒"}},
])
write("frame-json.json", [
    {"expect_frame": {"type": "ready", "name": "ready"}},
    {"send_raw": "{this is not json"},
    {"expect_frame": {"type": "error", "code": "FRAME_INVALID", "name": "非 JSON 被拒"}},
])
write("frame-empty.json", [
    {"expect_frame": {"type": "ready", "name": "ready"}},
    {"send": {"type": "input"}},
    {"expect_frame": {"type": "error", "code": "FRAME_INVALID", "name": "空 input 被拒"}},
])
write("frame-size.json", [
    {"expect_frame": {"type": "ready", "name": "ready"}},
    {"input": "a" * 70000},
    {"expect_frame": {"type": "error", "code": "FRAME_INVALID", "name": "单帧 70 KiB 被拒"}},
])
# 超过**整帧**上限（95573 字节）的输入：Web 那一跳直接断开（gorilla 的读上限），
# 代理根本看不到这一帧。断言的是"被断开"而不是某个错误码。
write("frame-huge.json", [
    {"expect_frame": {"type": "ready", "name": "ready"}},
    {"input": "a" * 100000},
    {"drain": 3},
])
write("ratelimit.json", [
    {"expect_frame": {"type": "ready", "name": "ready"}},
    # burst 的文本必须是**无害**的：限流不是“一帧都不给过”，前若干帧会被真的
    # 写进 PTY。用 "x" 的话这些字符留在 shell 的行缓冲里，下一条命令就变成
    # "xxxxexport ..."，而失败会出现在后面完全无关的断言上。换行等于空命令行，
    # 只多几个提示符。
    {"burst": {"count": 400, "text": "\n"}},
    {"expect_frame": {"type": "error", "code": "RATE_LIMITED", "timeout": 15, "name": "超速率被拒"}},
])
# 断流：什么都不发就断 TCP（RST），会话进入宽限期。
write("grace-abort.json", [
    {"expect_frame": {"type": "ready", "name": "ready"}},
    {"input": "export P06KEEP=%s\r" % kept},
    {"input": "echo \"BEFORE:$(echo $P06KEEP)\"\r"},
    {"expect_output": {"contains": "BEFORE:%s" % kept, "name": "断流前变量已设"}},
    {"abort": True},
])
# 宽限内重连：变量还在 = 同一个 shell 进程，不是新开的。
write("grace-reattach.json", [
    {"expect_frame": {"type": "ready", "name": "ready"}},
    {"input": "echo \"AFTER:$(echo $P06KEEP)\"\r"},
    {"expect_output": {"contains": "AFTER:%s" % kept, "name": "重连接上同一个 PTY"}},
    {"close": True},
])
# 单连接占用：第一条连接挂着，第二条必须 409。
write("hold.json", [
    {"expect_frame": {"type": "ready", "name": "ready"}},
    {"sleep": 8},
    {"close": True},
])
# 空闲关闭：连上、什么都不做，等 exit 帧（reason=idle）。
write("idle.json", [
    {"expect_frame": {"type": "ready", "name": "ready"}},
    {"expect_frame": {"type": "exit", "timeout": 100, "name": "空闲到点"}},
    {"drain": 1},
])
# 最长会话：一直有调整窗口（刷新活动时间），到绝对寿命仍然要被收掉。
maxsteps = []
for i in range(14):
    maxsteps.append({"sleep": 8})
    maxsteps.append({"resize": [100 + (i % 5), 30 + (i % 3)]})
maxsteps.append({"sleep": 8})
maxsteps.append({"expect_frame": {"type": "exit", "timeout": 45, "name": "最长到点"}})
maxsteps.append({"drain": 1})
write("maxduration.json", maxsteps)

write("probe.json", [
    {"expect_frame": {"type": "ready", "name": "ready"}},
    {"input": "echo \"PROBE:$(echo ok)\"\r"},
    {"expect_output": {"contains": "PROBE:ok", "name": "会话可用"}},
    {"close": True},
])
print("plans:", sorted(os.listdir(plans)))
PY
if [ -f "$PLANS/identity.json" ]; then pass "客户端计划文件已生成"; else fail "客户端计划文件已生成" "plans 目录为空"; fi
# 身份计划里要断言 uid，生成时从环境取；这里补一次以免计划文件被 js 生成早于变量导出。
SHELL_UID=$SHELL_UID python3 - "$PLANS/identity.json" "$SHELL_UID" <<'PY'
import json, sys
path, uid = sys.argv[1], sys.argv[2]
plan = json.load(open(path, encoding="utf-8"))
for step in plan["steps"]:
    exp = step.get("expect_output")
    if exp and exp.get("name") == "uid":
        exp["contains"] = "UID:%s" % uid
json.dump(plan, open(path, "w", encoding="utf-8"), ensure_ascii=False, indent=1)
PY
note "标记串：$MARKER（只应出现在终端里，不应出现在审计或日志里）"

echo
echo "== 2. 权限：匿名 401、viewer 403（列表/领许可/连/关） =="
ANON=$(curl -s -o "$PAYLOADS/anon-sessions.json" -w '%{http_code}' "$BASE$TERM/sessions" \
  -H "Origin: $ORIGIN")
expect_eq "匿名列会话 401" "401" "$ANON"
expect_eq "匿名列会话的错误码" "UNAUTHENTICATED" \
  "$(jget "$PAYLOADS/anon-sessions.json" 'd["error"]["code"]')"
expect_eq "viewer 列会话 403" "403" "$(api "$VJAR" "" GET "$TERM/sessions")"
expect_eq "viewer 列会话的错误码" "FORBIDDEN" "$(resp_get 'd["error"]["code"]')"
VCSRF=$(csrf_of "$WORK/login-viewer.json")
expect_eq "viewer 领连接许可 403" "403" "$(grant "$VJAR" "$VCSRF" "0"$(printf '0%.0s' $(seq 1 31)))"
VSTATE=$(curl -s -b "$VJAR" -o "$PAYLOADS/viewer-connect.json" -w '%{http_code}' \
  "$BASE$TERM/sessions/$(printf 'a%.0s' $(seq 1 32))/connect" -H "Origin: $ORIGIN")
expect_eq "viewer 连终端 403" "403" "$VSTATE"
expect_eq "viewer 关会话 403" "403" "$(close_session "$VJAR" "$VCSRF" "$(printf 'a%.0s' $(seq 1 32))")"
expect_eq "viewer 关会话的错误码" "FORBIDDEN" "$(resp_get 'd["error"]["code"]')"

echo
echo "== 3. terminal.create：预检 → 未再认证被拒 → 再认证 → 任务 =="
expect_eq "创建终端预检 200" "200" "$(preflight terminal.create '{"cols":100,"rows":30}')"
cp "$WORK/preflight.json" "$PAYLOADS/preflight-create.json"
expect_eq "预检允许（账号与数量都还有位置）" "True" "$(pget 'd["data"]["allowed"]')"
expect_eq "预检要求再认证" "True" "$(pget 'd["data"]["requires_reauth"]')"
expect_eq "预检不要求确认短语" "True" "$(pget 'd["data"]["confirmation_phrase"] is None')"
expect_eq "impact 是 interactive_access" "interactive_access" "$(pimpact 0)"
expect_contains "目标身份写明是 upanel-shell" "$SHELL_USER" "$(pget 'd["data"]["target"]["display_name"]')"
expect_contains "impact 文案说明这不是 root 终端" "不是 root" "$(pget 'd["data"]["impacts"][0]["description"]')"
PF_CREATE=$(pget 'd["data"]["preflight_id"]')
expect_eq "未再认证提交终端创建：403" "submit:403" "$(submit_and_wait "$PF_CREATE" "" "k-term-noauth")"
expect_eq "未再认证的错误码" "REAUTH_REQUIRED" "$(jget "$WORK/submit.json" 'd["error"]["code"]')"
api "$JAR" "$CSRF" GET '/api/v1/jobs?limit=50' >/dev/null
expect_eq "被拒的提交没有产生任务" "0" \
  "$(resp_get 'len([j for j in d["data"]["jobs"] if j.get("action") == "terminal.create"])')"
expect_eq "再认证" "200" "$(api "$JAR" "$CSRF" POST /api/v1/auth/reauth "{\"password\":\"$PASSWORD\"}")"
cp "$WORK/resp.json" "$PAYLOADS/reauth.json"
expect_eq "再认证后同一份预检可以执行" "succeeded" "$(submit_and_wait "$PF_CREATE" "" "k-term-create")"
cp "$WORK/job.json" "$PAYLOADS/job-create.json"
S1=$(jdata session_id)
expect_eq "任务结果 outcome 是 verified" "verified" "$(joutcome)"
expect_eq "结果的账号" "$SHELL_USER" "$(jdata account)"
expect_eq "结果的 uid 与账号一致" "$SHELL_UID" "$(jdata account_uid)"
expect_eq "结果的初始状态是 detached" "detached" "$(jdata state)"
expect_eq "读回的 cols" "100" "$(jobserved cols)"
expect_eq "读回的 rows" "30" "$(jobserved rows)"
expect_eq "结果里的 session_id 与 observed_state 一致" "$S1" "$(jobserved session_id)"
expect_eq "session_id 是 32 位小写十六进制" "True" \
  "$(jget "$WORK/job.json" "bool(__import__('re').fullmatch(r'[0-9a-f]{32}', d['data']['result']['data']['session_id']))")"
expect_eq "结果带上了空闲时限（60 秒）" "$IDLE_SECONDS" "$(jdata idle_timeout_seconds)"
expect_eq "结果带上了最长寿命（120 秒）" "$MAX_SECONDS" "$(jdata max_duration_seconds)"
note "会话 id：$S1"

echo
echo "== 4. 会话列表与归属隔离 =="
expect_eq "列会话 200" "200" "$(sessions_list)"
cp "$WORK/resp.json" "$PAYLOADS/sessions.json"
expect_eq "列表里的账号" "$SHELL_USER" "$(resp_get 'd["data"]["account"]')"
expect_eq "列表里的 uid" "$SHELL_UID" "$(resp_get 'd["data"]["account_uid"]')"
expect_eq "空闲时限" "$IDLE_SECONDS" "$(resp_get 'd["data"]["idle_timeout_seconds"]')"
expect_eq "最长寿命" "$MAX_SECONDS" "$(resp_get 'd["data"]["max_duration_seconds"]')"
expect_eq "断流宽限" "$GRACE_SECONDS" "$(resp_get 'd["data"]["disconnect_grace_seconds"]')"
expect_eq "每账号上限 1" "1" "$(resp_get 'd["data"]["max_per_user"]')"
expect_eq "全局上限 2" "2" "$(resp_get 'd["data"]["max_sessions"]')"
expect_eq "只有自己那一个会话" "1" "$(resp_get 'len(d["data"]["sessions"])')"
expect_eq "会话状态是 detached" "detached" "$(resp_get 'd["data"]["sessions"][0]["state"]')"
expect_eq "还没连过，attached_at 为空" "True" \
  "$(resp_get 'd["data"]["sessions"][0]["attached_at"] is None')"
expect_eq "会话的时限字段与全局一致" "True" \
  "$(resp_get 'all(s["idle_timeout_seconds"] == d["data"]["idle_timeout_seconds"] and s["max_duration_seconds"] == d["data"]["max_duration_seconds"] for s in d["data"]["sessions"])')"
expect_eq "admin2 看不到别人的会话" "0" \
  "$(api "$A2JAR" "$CSRF2" GET "$TERM/sessions" >/dev/null; resp_get 'len(d["data"]["sessions"])')"
expect_eq "admin2 领别人的会话许可 → 404" "404" "$(grant "$A2JAR" "$CSRF2" "$S1")"
expect_eq "404 的错误码" "NOT_FOUND" "$(grant_get 'd["error"]["code"]')"
expect_eq "admin2 关别人的会话 → 404" "404" "$(close_session "$A2JAR" "$CSRF2" "$S1")"
expect_eq "不存在的会话 id（形状对）→ 404" "404" \
  "$(grant_as "$(printf 'b%.0s' $(seq 1 32))")"
expect_eq "形状不对的会话 id → 404" "404" "$(grant_as 'not-a-session-id')"

# getgrant <session_id>：领一张许可并只回显许可本身（状态码丢弃）。
getgrant() { grant_as "$1" >/dev/null; grant_get 'd["data"]["grant"]'; }

echo
echo "== 5. 一次性连接许可 =="
_code=$(curl -s -b "$JAR" -o "$WORK/resp.json" -w '%{http_code}' -X POST \
  "$BASE$TERM/sessions/$S1/connection-grants" -H "Origin: $ORIGIN")
expect_eq "领许可必须带 CSRF" "403" "$_code"
expect_eq "缺 CSRF 的错误码" "CSRF_FAILED" "$(resp_get 'd["error"]["code"]')"
expect_eq "领许可 200" "200" "$(grant_as "$S1")"
cp "$WORK/grant.json" "$PAYLOADS/grant.json"
expect_eq "许可是 32 位小写十六进制" "True" \
  "$(grant_get "bool(__import__('re').fullmatch(r'[0-9a-f]{32}', d['data']['grant']))")"
expect_eq "许可里写明了协议名" "upanel.term.v1" "$(grant_get 'd["data"]["protocol"]')"
expect_eq "许可子协议名是 upanel.grant.<id>" "True" \
  "$(grant_get 'd["data"]["grant_protocol"] == "upanel.grant." + d["data"]["grant"]')"
expect_eq "connect_path 以 /connect 结尾" "True" \
  "$(grant_get 'd["data"]["connect_path"].endswith("/connect")')"
expect_eq "connect_path 没有查询参数" "True" \
  "$(grant_get 'chr(63) not in d["data"]["connect_path"]')"
expect_eq "connect_path 里没有许可（许可不进 URL）" "True" \
  "$(grant_get 'd["data"]["grant"] not in d["data"]["connect_path"]')"
expect_eq "许可的剩余有效期在 31 秒内" "True" "$(grant_get '
_ = 0 < (__import__("datetime").datetime.strptime(d["data"]["expires_at"], "%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=__import__("datetime").timezone.utc) - __import__("datetime").datetime.now(__import__("datetime").timezone.utc)).total_seconds() <= 31')"
G_SINGLE=$(grant_get 'd["data"]["grant"]')
ws single "$TERM/sessions/$S1/connect" "$ORIGIN" "$CK" "upanel.term.v1,upanel.grant.$G_SINGLE" "$PLANS/probe.json"
expect_ws_up "许可第一次使用" single
expect_steps "许可第一次使用时终端可用" single "ready,会话可用"
ws single-reuse "$TERM/sessions/$S1/connect" "$ORIGIN" "$CK" "upanel.term.v1,upanel.grant.$G_SINGLE" -
expect_eq "同一张许可再用一次 403" "403" "$(wsget single-reuse 'd["status"]')"
expect_eq "复用许可的错误码" "FORBIDDEN" "$(wsget single-reuse 'json.loads(d["body"])["error"]["code"]')"

echo
echo "== 6. 握手四道门：Origin → 子协议 → 许可 → 代理 =="
G=$(getgrant "$S1")
ws noorigin "$TERM/sessions/$S1/connect" none "$CK" "upanel.term.v1,upanel.grant.$G" -
expect_eq "没有 Origin 的握手 403" "403" "$(wsget noorigin 'd["status"]')"
expect_eq "没有 Origin 的错误码" "CSRF_FAILED" "$(wsget noorigin 'json.loads(d["body"])["error"]["code"]')"

G=$(getgrant "$S1")
ws badorigin "$TERM/sessions/$S1/connect" "http://evil.example:8443" "$CK" "upanel.term.v1,upanel.grant.$G" -
expect_eq "不在允许列表里的 Origin 403" "403" "$(wsget badorigin 'd["status"]')"
expect_eq "错误 Origin 的错误码" "CSRF_FAILED" "$(wsget badorigin 'json.loads(d["body"])["error"]["code"]')"

G=$(getgrant "$S1")
ws noproto "$TERM/sessions/$S1/connect" "$ORIGIN" "$CK" "" -
expect_eq "没声明子协议 400" "400" "$(wsget noproto 'd["status"]')"
expect_eq "没声明子协议的错误码" "VALIDATION_ERROR" "$(wsget noproto 'json.loads(d["body"])["error"]["code"]')"

ws nogrant "$TERM/sessions/$S1/connect" "$ORIGIN" "$CK" "upanel.term.v1" -
expect_eq "有子协议但没有许可 403" "403" "$(wsget nogrant 'd["status"]')"
expect_eq "没有许可的错误码" "FORBIDDEN" "$(wsget nogrant 'json.loads(d["body"])["error"]["code"]')"

ws fakegrant "$TERM/sessions/$S1/connect" "$ORIGIN" "$CK" \
  "upanel.term.v1,upanel.grant.$(printf 'f%.0s' $(seq 1 32))" -
expect_eq "伪造的许可 403" "403" "$(wsget fakegrant 'd["status"]')"
expect_eq "伪造许可的错误码" "FORBIDDEN" "$(wsget fakegrant 'json.loads(d["body"])["error"]["code"]')"

G=$(getgrant "$S1")
ws twogrant "$TERM/sessions/$S1/connect" "$ORIGIN" "$CK" \
  "upanel.term.v1,upanel.grant.$G,upanel.grant.$(printf 'e%.0s' $(seq 1 32))" -
expect_eq "两个许可令牌的畸形握手 403" "403" "$(wsget twogrant 'd["status"]')"

expect_eq "被拒绝的握手没有留下连接（会话仍是 detached）" "detached" "$(session_state "$S1")"

echo
echo "== 7. 连上之后真的是 upanel-shell（TERM-01） =="
connect ident "$S1" "$PLANS/identity.json"
expect_ws_up "身份连接" ident
expect_eq "身份连接没有客户端侧错误" "None" "$(wsget ident 'd["error"]')"
expect_steps "终端里的身份与边界" ident \
  "ready,账号名,uid,属组,没有特权组,工作目录是 home,HOME 是 home,真的在 PTY 上,sudo 不可用,/etc/shadow 读不到,代理密钥读不到,系统目录写不了,/proc/1 可读（降权不等于被关在外面）,环境里没有代理/提权变量,标记串,导出变量"
expect_eq "ready 帧报告的账号" "$SHELL_USER" "$(wsget ident 'next((f["account"] for f in d["frames"] if f.get("type") == "ready"), None)')"
expect_eq "ready 帧报告的 uid" "$SHELL_UID" "$(wsget ident 'next((f["account_uid"] for f in d["frames"] if f.get("type") == "ready"), None)')"
expect_eq "ready 帧报告的会话 id" "$S1" "$(wsget ident 'next((f["session_id"] for f in d["frames"] if f.get("type") == "ready"), None)')"
expect_eq "终端里没有一行是 uid 0" "0" "$(wsout ident | grep -c '^UID:0$')"
expect_eq "终端里没有一行是 root" "0" "$(wsout ident | grep -c '^USER:root$')"
expect_eq "探测文件没有被创建" "no" "$([ -e "$PROBE" ] && echo yes || echo no)"
expect_eq "被拒绝的 run 帧没有执行任何东西" "no" "$([ -e /tmp/p06-3-executed ] && echo yes || echo no)"
cp "$WORK/ws-ident.json" "$PAYLOADS/ws-identity.json"
expect_eq "连接结束后会话回到 detached" "detached" "$(wait_state "$S1" detached 10)"

echo
echo "== 8. 窗口调整真的到了 PTY =="
connect resize "$S1" "$PLANS/resize.json"
expect_ws_up "窗口调整连接" resize
expect_steps "stty 读回的尺寸" resize "ready,PTY 尺寸 43x132,改回 24x80"
cp "$WORK/ws-resize.json" "$PAYLOADS/ws-resize.json"

echo
echo "== 9. 帧协议闭集：越界一律拒绝并断开 =="
# 每条违规都断言：升级成功 → 收到 FRAME_INVALID → 连接被断开 → 会话仍在（宽限期内）。
frame_case() {
  _name=$1; _plan=$2; _step=$3
  _state=$(wait_state "$S1" detached 10)
  if [ "$_state" != "detached" ]; then fail "$_name 前会话可用" "状态是 $_state"; return 0; fi
  connect "$_name" "$S1" "$_plan"
  expect_ws_up "$_name 升级" "$_name"
  expect_steps "$_name 被拒" "$_name" "ready,$_step"
  expect_eq "$_name 之后连接被断开" "True" "$(wsget "$_name" 'd["closed_by_server"]')"
  # 断开的原因要能看见：4001 = 帧不合法。只有“连接断了”的话，协议违规和掉线在
  # 浏览器里长得一模一样。
  expect_eq "$_name 的关闭码是 4001" "4001" "$(wsget "$_name" 'd["close_code"]')"
  expect_eq "$_name 之后会话还在（断流宽限）" "detached" "$(wait_state "$S1" detached 10)"
}
frame_case frame-run "$PLANS/frame-run.json" "run 帧被拒"
frame_case frame-output "$PLANS/frame-output.json" "output 帧被拒"
frame_case frame-json "$PLANS/frame-json.json" "非 JSON 被拒"
frame_case frame-empty "$PLANS/frame-empty.json" "空 input 被拒"
frame_case frame-size "$PLANS/frame-size.json" "单帧 70 KiB 被拒"
expect_not_contains "越界帧的错误信息里不回显 base64 内容" "YWFhYWFhYWFh" \
  "$(wsget frame-size '",".join(str(f.get("message","")) for f in d["frames"])')"

# 整帧超上限：Web 那一跳直接断开（gorilla 的读上限），没有错误帧可给。
_state=$(wait_state "$S1" detached 10)
connect frame-huge "$S1" "$PLANS/frame-huge.json"
expect_ws_up "超大帧连接" frame-huge
expect_eq "超过整帧上限：连接被断开" "True" "$(wsget frame-huge 'd["closed_by_server"]')"
expect_eq "超过整帧上限：会话仍可用" "detached" "$(wait_state "$S1" detached 10)"
expect_eq "没有任何东西被 100 KB 的输入执行" "no" "$([ -e /tmp/p06-3-executed ] && echo yes || echo no)"

echo
echo "== 10. 输入速率上限 =="
_state=$(wait_state "$S1" detached 10)
connect ratelimit "$S1" "$PLANS/ratelimit.json"
expect_ws_up "速率限制连接" ratelimit
expect_eq "一次发 400 帧" "400" "$(wsget ratelimit 'd.get("burst_sent")')"
expect_steps "超速率被拒" ratelimit "ready,超速率被拒"
expect_eq "超速率之后连接被断开" "True" "$(wsget ratelimit 'd["closed_by_server"]')"
expect_eq "超速率的关闭码是 4002" "4002" "$(wsget ratelimit 'd["close_code"]')"
expect_eq "超速率之后会话还在宽限期里" "detached" "$(wait_state "$S1" detached 10)"

echo
echo "== 11. 单连接：已连接时再连一个 → 409 =="
G=$(getgrant "$S1")
ws hold "$TERM/sessions/$S1/connect" "$ORIGIN" "$CK" "upanel.term.v1,upanel.grant.$G" "$PLANS/hold.json" &
HOLD_PID=$!
expect_eq "第一条连接确实连上了" "attached" "$(wait_state "$S1" attached 10)"
connect second "$S1" -
expect_eq "第二条连接 409" "409" "$(wsget second 'd["status"]')"
expect_eq "第二条连接的错误码" "RESOURCE_BUSY" "$(wsget second 'json.loads(d["body"])["error"]["code"]')"
expect_eq "第一条连接没有被踢掉" "attached" "$(session_state "$S1")"
wait "$HOLD_PID" 2>/dev/null
expect_ws_up "占用连接本身是好的" hold

echo
echo "== 12. 断流宽限：宽限内重连接上同一个 PTY =="
connect grace-abort "$S1" "$PLANS/grace-abort.json"
expect_ws_up "断流前的连接" grace-abort
expect_steps "断流前设置变量" grace-abort "ready,断流前变量已设"
expect_eq "RST 之后会话进入 detached" "detached" "$(wait_state "$S1" detached 5)"
connect grace-reattach "$S1" "$PLANS/grace-reattach.json"
expect_ws_up "宽限内重连" grace-reattach
expect_steps "重连后变量还在（同一个 shell）" grace-reattach "ready,重连接上同一个 PTY"
note "宽限 $GRACE_SECONDS 秒：重连成功说明 PTY 没有被断流关掉"

echo
echo "== 13. 宽限到期：会话消失、shell 被回收 =="
expect_eq "宽限到期后会话从表里消失" "gone" "$(wait_state "$S1" gone $((GRACE_SECONDS + 8)))"
expect_eq "关闭一个已经不存在的会话仍然成功（幂等）" "200" "$(close_session "$JAR" "$CSRF" "$S1" user)"
expect_eq "幂等关闭的响应" "True" "$(resp_get 'd["data"]["closed"]')"
expect_eq "关闭响应里的会话 id" "$S1" "$(resp_get 'd["data"]["session_id"]')"
expect_eq "宽限到期后没有残留 shell 进程" "0" "$(wait_shell_procs 0 10)"

echo
echo "== 14. 空闲关闭（${IDLE_SECONDS} 秒无输入） =="
if [ "${SKIP_SLOW:-0}" = "1" ]; then
  notrun "空闲关闭" "SKIP_SLOW=1，未等待 ${IDLE_SECONDS} 秒"
  S2=""
else
  expect_eq "创建会话前再认证（再认证 5 分钟有效，慢段之后要重新做）" "200" \
    "$(api "$JAR" "$CSRF" POST /api/v1/auth/reauth "{\"password\":\"$PASSWORD\"}")"
  expect_eq "再建一个会话" "succeeded" "$(run_as "$JAR" "$CSRF" terminal.create '{"cols":90,"rows":25}' "" "k-term-idle")"
  S2=$(jdata session_id)
  note "会话 id：$S2"
  connect idle "$S2" "$PLANS/idle.json"
  expect_ws_up "空闲连接" idle
  expect_steps "空闲到点收到 exit 帧" idle "ready,空闲到点"
  cp "$WORK/ws-idle.json" "$PAYLOADS/ws-idle.json"
  expect_eq "空闲结束的原因是 idle" "idle" "$(wsget idle 'd["exit"]["reason"]')"
  expect_eq "空闲结束后会话消失" "gone" "$(wait_state "$S2" gone 10)"
fi

echo
echo "== 15. 最长会话（${MAX_SECONDS} 秒到点，即使一直有活动） =="
if [ "${SKIP_SLOW:-0}" = "1" ]; then
  notrun "最长会话" "SKIP_SLOW=1，未等待 ${MAX_SECONDS} 秒"
  S3=""
else
  expect_eq "创建会话前再认证（再认证 5 分钟有效，慢段之后要重新做）" "200" \
    "$(api "$JAR" "$CSRF" POST /api/v1/auth/reauth "{\"password\":\"$PASSWORD\"}")"
  expect_eq "再建一个会话" "succeeded" "$(run_as "$JAR" "$CSRF" terminal.create '{"cols":100,"rows":30}' "" "k-term-max")"
  S3=$(jdata session_id)
  note "会话 id：$S3（这一条要等 ${MAX_SECONDS} 秒）"
  connect maxdur "$S3" "$PLANS/maxduration.json"
  expect_ws_up "最长会话连接" maxdur
  expect_steps "最长到点收到 exit 帧" maxdur "最长到点"
  cp "$WORK/ws-maxdur.json" "$PAYLOADS/ws-maxduration.json"
  expect_eq "最长会话结束的原因是 expired" "expired" "$(wsget maxdur 'd["exit"]["reason"]')"
  expect_eq "最长会话结束后会话消失" "gone" "$(wait_state "$S3" gone 10)"
fi

echo
echo "== 16. 数量上限：每账号 1、全局 2 =="
expect_eq "创建会话前再认证" "200" "$(api "$JAR" "$CSRF" POST /api/v1/auth/reauth "{\"password\":\"$PASSWORD\"}")"
expect_eq "admin1 建第 1 个会话" "succeeded" "$(run_as "$JAR" "$CSRF" terminal.create '{"cols":80,"rows":24}' "" "k-term-l1")"
S4=$(jdata session_id)
expect_eq "admin1 的第 2 个会话：预检" "200" "$(preflight terminal.create '{"cols":80,"rows":24}')"
expect_eq "预检不允许" "False" "$(pget 'd["data"]["allowed"]')"
expect_eq "阻塞项是 TERMINAL_SESSION_LIMIT" "TERMINAL_SESSION_LIMIT" "$(pblocker 0)"
expect_contains "阻塞项说明了上限" "最多 1 个" "$(pget 'd["data"]["blockers"][0]["message"]')"
expect_eq "admin2 再认证" "200" "$(api "$A2JAR" "$CSRF2" POST /api/v1/auth/reauth "{\"password\":\"$ADMIN2_PASSWORD\"}")"
expect_eq "admin2 建会话（全局第 2 个）" "succeeded" "$(run_as "$A2JAR" "$CSRF2" terminal.create '{"cols":80,"rows":24}' "" "k-term-l2")"
S5=$(jdata session_id)
expect_eq "admin3 再认证" "200" "$(api "$A3JAR" "$CSRF3" POST /api/v1/auth/reauth "{\"password\":\"$ADMIN3_PASSWORD\"}")"
expect_eq "admin3 建会话（全局第 3 个）任务失败" "failed" "$(run_as "$A3JAR" "$CSRF3" terminal.create '{"cols":80,"rows":24}' "" "k-term-l3")"
expect_eq "全局上限的错误码是 RESOURCE_BUSY" "RESOURCE_BUSY" "$(jerr)"
cp "$WORK/job.json" "$PAYLOADS/job-limit.json"
expect_eq "admin1 的会话仍在" "detached" "$(session_state "$S4")"
expect_eq "admin2 的会话仍在" "detached" "$(session_state_as "$A2JAR" "$CSRF2" "$S5")"

echo
echo "== 17. 关闭、幂等与元数据审计 =="
expect_eq "reason 不在枚举内 → 400" "400" "$(close_session "$JAR" "$CSRF" "$S4" "because-i-said-so")"
expect_eq "非法 reason 的错误码" "VALIDATION_ERROR" "$(resp_get 'd["error"]["code"]')"
_nocsrf=$(curl -s -b "$JAR" -o "$WORK/resp.json" -w '%{http_code}' -X DELETE \
  "$BASE$TERM/sessions/$S4" -H "Origin: $ORIGIN")
expect_eq "关闭必须带 CSRF" "403" "$_nocsrf"
expect_eq "关闭成功" "200" "$(close_session "$JAR" "$CSRF" "$S4" user)"
cp "$WORK/resp.json" "$PAYLOADS/close.json"
expect_eq "关闭响应 closed=true" "True" "$(resp_get 'd["data"]["closed"]')"
expect_eq "重复关闭仍然成功（幂等）" "200" "$(close_session "$JAR" "$CSRF" "$S4" user)"
expect_eq "关闭后会话从表里消失" "gone" "$(wait_state "$S4" gone 5)"
expect_eq "admin2 关闭自己的会话" "200" "$(close_session "$A2JAR" "$CSRF2" "$S5" user)"
expect_eq "admin2 的会话也消失了" "gone" "$(wait_state_as "$A2JAR" "$CSRF2" "$S5" gone 5)"

# 审计：只记元数据。把库拷出来查（不锁生产库），行里不许有终端内容。
cp "$WEB_DB" "$WORK/audit-copy.db" 2>/dev/null
for suffix in "-wal" "-shm"; do [ -e "$WEB_DB$suffix" ] && cp "$WEB_DB$suffix" "$WORK/audit-copy.db$suffix"; done
python3 - "$WORK/audit-copy.db" "$PAYLOADS/audit-terminal.json" <<'PY'
import json, sqlite3, sys
db, out = sys.argv[1], sys.argv[2]
con = sqlite3.connect(db)
rows = con.execute("""
    SELECT id, actor_id, action, target, outcome, redacted_details, created_at
    FROM audit_events WHERE action LIKE 'terminal.%' ORDER BY id""").fetchall()
data = []
for r in rows:
    try:
        details = json.loads(r[5])
    except Exception:
        details = {"_unparsable": r[5]}
    data.append({"id": r[0], "actor_id": r[1], "action": r[2], "target": r[3],
                 "outcome": r[4], "details": details, "created_at": r[6]})
json.dump(data, open(out, "w", encoding="utf-8"), ensure_ascii=False, indent=1)
print("审计行 %d 条" % len(data))
PY
AGET() { jget "$PAYLOADS/audit-terminal.json" "$1"; }
expect_eq "审计有 terminal.attach 的 connected 阶段" "True" \
  "$(AGET 'any(r["action"] == "terminal.attach" and r["outcome"] == "success" and r["details"].get("stage") == "connected" for r in d)')"
expect_eq "审计记了握手中被拒的 origin 阶段" "True" \
  "$(AGET 'any(r["details"].get("stage") == "origin" and r["outcome"] == "failure" for r in d)')"
expect_eq "审计记了握手中被拒的 protocol 阶段" "True" \
  "$(AGET 'any(r["details"].get("stage") == "protocol" and r["outcome"] == "failure" for r in d)')"
expect_eq "审计记了握手中被拒的 grant 阶段" "True" \
  "$(AGET 'any(r["details"].get("stage") == "grant" and r["outcome"] == "failure" for r in d)')"
expect_eq "审计有 terminal.close" "True" \
  "$(AGET 'any(r["action"] == "terminal.close" and r["outcome"] == "success" for r in d)')"
# target 必须**精确**等于会话：不是“以 terminal: 开头”就算数——那样把一个拼错的
# id 写进审计也能通过。三种行分开断言（用计数，0 = 不合规的行一条都没有）：
#   1. 走到处理函数的行：target == "terminal:" + details.session_id；
#   2. 握手中途被拒的行（还没有审计上下文）：target == "terminal:" + 路径里的 id；
#   3. 列表接口被拒的行：路径里没有会话 id，target 只能是 user，不能冒充某个会话。
# 每一类都先要求“确实有这种行”，避免空集合让 all() 白通过。
WALKED='r["action"] in ("terminal.attach","terminal.close") and r["details"].get("session_id")'
REJECTED='r["action"] in ("terminal.attach","terminal.close") and not r["details"].get("session_id") and "/sessions/" in r["details"].get("http_path","")'
LISTED='r["action"] in ("terminal.attach","terminal.close") and not r["details"].get("session_id") and "/sessions/" not in r["details"].get("http_path","")'
expect_eq "至少有走完整握手的审计行" "True" \
  "$(AGET "any($WALKED for r in d)")"
expect_eq "审计 target 与会话 id 完全一致" "0" \
  "$(AGET "sum(1 for r in d if $WALKED and r[\"target\"] != \"terminal:\" + r[\"details\"][\"session_id\"])")"
expect_eq "至少有握手中被拒的审计行" "True" \
  "$(AGET "any($REJECTED for r in d)")"
expect_eq "握手中被拒的审计行也精确指向路径里的会话" "0" \
  "$(AGET "sum(1 for r in d if $REJECTED and r[\"target\"] != \"terminal:\" + r[\"details\"][\"http_path\"].split(\"/sessions/\")[1].split(\"/\")[0])")"
expect_eq "列表接口的拒绝不冒充某个会话" "0" \
  "$(AGET "sum(1 for r in d if $LISTED and r[\"target\"] != \"user\")")"
expect_eq "审计里没有终端内容（标记串不出现）" "0" \
  "$(grep -c "$MARKER" "$PAYLOADS/audit-terminal.json" 2>/dev/null | head -1)"
expect_eq "审计行没有超长字段（终端内容会很长）" "True" \
  "$(AGET 'max([len(json.dumps(r["details"], ensure_ascii=False)) for r in d] or [0]) < 300')"
expect_eq "代理日志里没有终端内容（标记串不出现）" "0" "$(grep -c "$MARKER" "$AGENT_LOG" 2>/dev/null)"
expect_eq "Web 日志里没有终端内容（标记串不出现）" "0" "$(grep -c "$MARKER" "$WEB_LOG" 2>/dev/null)"

echo
echo "== 18. 账号不可用：不登记能力，接口是 501 =="
stop_web
stop_agent
usermod -s /usr/sbin/nologin "$SHELL_USER"
if start_agent; then pass "账号不可用时代理仍能启动"; else fail "账号不可用时代理仍能启动" "$(tail -5 "$AGENT_LOG")"; fi
case "$(grep -c '终端能力未启用' "$AGENT_LOG" 2>/dev/null)" in
  0) fail "代理日志说明了终端未启用" "日志里没有“终端能力未启用”" ;;
  *) pass "代理日志说明了终端能力未启用" ;;
esac
runuser -u "$WEB_USER" -- curl -s --unix-socket "$SOCK" \
  http://localhost/internal/v1/capabilities >"$PAYLOADS/agent-capabilities-noaccount.json" 2>/dev/null
expect_eq "能力表里没有 terminal.create" "False" \
  "$(jget "$PAYLOADS/agent-capabilities-noaccount.json" '"terminal.create" in d["data"]["supported_actions"]')"
expect_eq "能力表里没有 terminal.sessions" "False" \
  "$(jget "$PAYLOADS/agent-capabilities-noaccount.json" '"terminal.sessions" in d["data"]["supported_queries"]')"
if start_web; then pass "Web 在账号不可用时仍能启动"; else fail "Web 在账号不可用时启动" "$(tail -5 "$WEB_LOG")"; fi
expect_eq "账号不可用时列会话是 501" "501" "$(sessions_list)"
expect_eq "501 的错误码是 NOT_IMPLEMENTED" "NOT_IMPLEMENTED" "$(resp_get 'd["error"]["code"]')"
expect_eq "账号不可用时领许可是 501" "501" "$(grant_as "$(printf 'c%.0s' $(seq 1 32))")"
expect_eq "账号不可用时创建动作未登记（预检 400）" "400" \
  "$(preflight terminal.create '{"cols":80,"rows":24}')"
expect_eq "未登记动作的错误码" "VALIDATION_ERROR" "$(resp_get 'd["error"]["code"]')"
usermod -s /bin/bash "$SHELL_USER"
stop_web
stop_agent
if start_agent; then pass "恢复账号后代理重新启用终端"; else fail "恢复账号后代理启动" "$(tail -5 "$AGENT_LOG")"; fi
if start_web; then pass "Web 重新启动"; else fail "Web 重启" "$(tail -5 "$WEB_LOG")"; fi
expect_eq "恢复后列会话 200" "200" "$(sessions_list)"
expect_eq "恢复后没有遗留会话" "0" "$(resp_get 'len(d["data"]["sessions"])')"

echo
echo "== 19. 回归：终端之外仍然正常 =="
expect_eq "健康检查" "200" "$(curl -s -o /dev/null -w '%{http_code}' "$BASE/api/v1/health/live")"
expect_eq "系统概要" "200" "$(api "$JAR" "$CSRF" GET /api/v1/system/summary)"
expect_eq "存储设备列表" "200" "$(api "$JAR" "$CSRF" GET /api/v1/storage/devices)"
expect_eq "服务列表" "200" "$(api "$JAR" "$CSRF" GET /api/v1/services)"
expect_eq "进程列表" "200" "$(api "$JAR" "$CSRF" GET '/api/v1/processes?limit=5')"
expect_eq "日志查询" "200" "$(api "$JAR" "$CSRF" GET '/api/v1/logs?limit=10')"
expect_eq "文件根列表" "200" "$(api "$JAR" "$CSRF" GET /api/v1/files/roots)"
expect_eq "任务列表" "200" "$(api "$JAR" "$CSRF" GET '/api/v1/jobs?limit=20')"
expect_eq "没有残留的 upanel-shell shell 进程" "0" "$(wait_shell_procs 0 10)"
expect_eq "探测文件始终没有被创建" "no" "$([ -e "$PROBE" ] && echo yes || echo no)"

echo
echo "== 20. 载荷归档 =="
cp "$AGENT_LOG" "$PAYLOADS/agent.log" 2>/dev/null
python3 - "$PAYLOADS" "$AGENT_LOG" "$WEB_LOG" <<'PY'
import json, os, sys
out, agent_log, web_log = sys.argv[1], sys.argv[2], sys.argv[3]
summary = {"payloads": sorted(os.listdir(out))}
for name, path in (("agent.log", agent_log), ("web.log", web_log)):
    try:
        lines = open(path, encoding="utf-8", errors="replace").read().splitlines()
    except OSError:
        lines = []
    keys = ("已启用受限终端", "终端已连接", "终端连接结束", "终端会话超时结束",
            "终端 WebSocket", "拒绝", "error", "ERROR", "WARN")
    summary[name] = [line for line in lines if any(k in line for k in keys)][-80:]
json.dump(summary, open(os.path.join(out, "log-summary.json"), "w", encoding="utf-8"),
          ensure_ascii=False, indent=2)
print("载荷 %d 个，日志摘要 %d 行 / %d 行" %
      (len(summary["payloads"]), len(summary["agent.log"]), len(summary["web.log"])))
PY
if [ -f "$PAYLOADS/log-summary.json" ]; then
  pass "载荷与日志摘要已归档（$PAYLOADS）"
else
  fail "载荷与日志摘要已归档" "log-summary.json 没生成"
fi

echo
printf '通过 %d 项，失败 %d 项，未执行 %d 项\n' "$PASSED" "$FAILED" "$NOTRUN"
if [ "$FAILED" -ne 0 ]; then
  printf '失败项：%s\n' "$FAILED_NAMES"
  exit 1
fi
exit 0
