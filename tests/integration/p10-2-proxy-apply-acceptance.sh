#!/bin/sh
# P10.2 Linux 验收脚本：配置生成 → 原子写入 → nginx -t 门禁 → reload → 失败恢复。
#
# 这一份要证明的不是“任务返回 succeeded”，而是下面这些容易被“跑完了”骗过去的事实：
#
#   1) **站点真的在提供服务**：写完之后用 `curl -H "Host: <域名>" http://127.0.0.1/`
#      穿过这台机器上真实的 nginx 拿到上游的页面。配置文件对不对不是证据，
#      请求被转到上游才是证据（Docker 里没有 DNS，Host 头就是域名解析的替身）。
#   2) **写入是原子的**：正式文件 content 与渲染结果一致，权限 644，而且我们**从不**
#      直接写正式路径——候选文件 + rename，见 §5 的 ops 顺序断言（单元测试）与
#      §6 的“写完之后正式路径的内容一次成型”。
#   3) **nginx -t 是门禁，reload 是生效**：中间的 worker pid 必须换过一代；
#      `nginx -t` 脚本自己再跑一遍（不信任务自报）。
#   4) **校验不过就回到原样**：制造一台“本来就有坏配置”的机器，让候选被拦下，
#      断言托管文件的内容哈希与执行前**逐字节相等**、worker pid 没换（没有 reload）、
#      作业是 failed 而不是 succeeded。删掉坏文件后同一次操作能成功——正对照。
#   5) **不影响管理端**：托管配置只监听 80（`listen 9443` 这种行不可能出现），
#      而且**在 nginx 配置坏掉的那段时间里面板照常登录**。面板不走 nginx，
#      这句话必须由“坏配置期间登录成功”来证明，不是靠注释。
#   6) **幂等**：同样内容再应用一次不写、不备份、不 reload（file_state=unchanged，
#      reload=skipped_unchanged），文件哈希与备份份数都不变。
#   7) **applied 是推导出来的，不是库里的列**：手工改一下托管文件，GET /proxies/:id
#      立刻变成 stale；重新应用后回到 applied，并且**备份里留着被手工改过的那一份**。
#   8) **basic auth 的哈希 nginx 真的认**：带口令 200、不带口令 401。
#      这是 apr1 实现唯一的真实判据——单元测试用 openssl 对齐，这里用 nginx 对齐。
#   9) **改名清理**：域名改过之后旧文件被移除（备份在前），旧域名不再被代理。
#  10) **/etc/nginx 里只多了面板自己的文件**：其它所有文件的内容摘要前后完全一致。
#
# 用法（root）：
#   sudo BIN_DIR=/var/tmp/upanel-bin sh tests/integration/p10-2-proxy-apply-acceptance.sh
#
# 环境变量：PORT（面板，默认 19497）、UPSTREAM_PORT（默认 18080）、WORK、BIN_DIR。
#
# 退出码：0 全部通过；1 有失败项；2 环境不满足（非 root / 二进制缺失 / 没有 nginx）。
#
# 关于夹具的破坏性：本脚本会真的在 /etc/nginx/conf.d 里写托管配置、在
# /etc/nginx/upanel-auth/ 里写账号文件、并 reload nginx，还会临时放一份**故意坏掉**
# 的配置（未闭合的 server 块）来验证门禁。收尾时会停用所有站点、删掉夹具、
# 再跑一次 `nginx -t` 自证。也就是说：**只在一次性的验收虚拟机上跑它**，
# 不要在日常服务器上跑。
#
# 明确不覆盖（NOT RUN，原因写在这里，不在脚本里假装通过）：
#   - **443 与证书**：P10.3。托管站点此刻只监听 80，`listen 443` 一行都没有。
#   - **管理面板的 9443/HTTPS 与 SSH 隧道文档**：P10.4 的打包内容。本脚本里的面板
#     监听 19497，断言用的是**真实端口**而不是写死的 9443；"上游端口不得等于面板端口"
#     这条规则在代理侧已经由单元测试覆盖（internal/proxies/apply_test.go、
#     internal/agent/proxyaction_test.go），通过 HTTP 打不到它——Web 总会用真实端口
#     覆盖参数里的值，这正是它该有的样子。
#   - **真实域名解析与外部访问**：VM 没有 DNS 记录，用 Host 头代替。
#   - **浏览器渲染**：VM 上没有浏览器。
#   - **WebSocket 长连接实测**：上游是 python http.server，不支持 Upgrade；
#     只验配置里 Upgrade/Connection 成对出现（单元测试断言），不做握手实测。
#   - **"回滚后重跑 nginx -t 通过"这一条分支**：它要求配置树在回滚之后重新变好，
#     而外面能造出来的坏配置不会因为我们回滚了自己那一份就好起来（§7b 走的正是
#     "回滚成功但仍校验失败"那条）。要走到"回滚后树是好的"，只能让候选本身非法，
#     而渲染是纯函数且已被单元测试钉死不会产出非法配置——所以这条分支由
#     internal/proxies/apply_test.go 用注入的 runner 覆盖，这里不假装跑过。

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
NGINX_BIN=$(command -v nginx 2>/dev/null || printf '/usr/sbin/nginx')
if [ ! -x "$NGINX_BIN" ]; then
  printf '本机没有 nginx，P10.2 的验收无法进行\n' >&2
  exit 2
fi
if ! command -v python3 >/dev/null 2>&1; then
  printf '缺少 python3（用来读 JSON 与起测试上游）\n' >&2
  exit 2
fi

WORK=${WORK:-/var/tmp/upanel-accept-p10-2}
PAYLOADS=$WORK/payloads
SECRETS_DIR=$WORK/secrets
UPSTREAM_DIR=$WORK/upstream
RUN_DIR=/run/upanel
SOCK=$RUN_DIR/agent.sock
SYS_CFG=/etc/upanel/upanel.json
AGENT_DB_DIR=/var/lib/upanel-agent
AGENT_DB=$AGENT_DB_DIR/agent.db
KEY_FILE=/etc/upanel/agent.key
WEB_DB_DIR=$WORK/webdata
WEB_DB=$WEB_DB_DIR/upanel.db
PORT=${PORT:-19497}
UPSTREAM_PORT=${UPSTREAM_PORT:-18080}
BASE="http://127.0.0.1:$PORT"
ORIGIN="$BASE"
WEB_USER=upanel-web
PASSWORD='vm-correct-horse-9'

PROXIES=/api/v1/proxies
PREFLIGHT_PATH=/api/v1/operations/preflight
JOBS=/api/v1/jobs
SECRETS_PATH=/api/v1/secrets

NGINX_CONF_D=/etc/nginx/conf.d
NGINX_AUTH_D=/etc/nginx/upanel-auth
BACKUP_DIR=/var/lib/upanel-agent/nginx-backups
FIX_BROKEN=$NGINX_CONF_D/p102-broken.conf

DOM_A=p102a.example.test
DOM_A2=p102a-renamed.example.test
DOM_B=p102b.example.test
DOM_C=p102c.example.test
MARKER='p102-upstream-marker'

AGENT_LOG=$WORK/logs/agent.log
WEB_LOG=$WORK/logs/web.log
UPSTREAM_LOG=$WORK/logs/upstream.log
AGENT_PID=""
WEB_PID=""
UPSTREAM_PID=""

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

# web_durations <路径>：从 Web 的访问日志里取某个路径所有请求的耗时（毫秒），
# 每行一个。访问日志的 time 字段是**发出那一刻**（请求结束），不是开始，所以
# 排序按时间线读时不要拿它当起点。
web_durations() {
  if [ ! -f "$WEB_LOG" ]; then return 0; fi
  python3 - "$WEB_LOG" "$1" <<'PY'
import json, sys
log, want = sys.argv[1], sys.argv[2]
for line in open(log):
    line = line.strip()
    if not line.startswith("{"):
        continue
    try:
        d = json.loads(line)
    except Exception:
        continue
    if d.get("path") == want:
        print(d.get("duration_ms") or 0)
PY
}

# web_max_duration <路径>：其中最大的一次；没有记录就打印 0。
web_max_duration() {
  _d=$(web_durations "$1")
  if [ -z "$_d" ]; then
    printf '0'
    return 0
  fi
  printf '%s\n' "$_d" | sort -n | tail -1
}

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

# jget <文件> <python 表达式，d 为解析后的 JSON>
#
# 求值出错时打印 ERR:<类型>:<消息>：空字符串会让“表达式写错”看起来像
# “响应里没这个字段”，而这两件事的下一步完全不同。
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

# dbq <只读查询>：直接读 Web 库（先 checkpoint，避免读到旧 WAL 快照）。
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

# ---- 与 Nginx 相关的观测 ----

# nginx_foreign_digest 是 /etc/nginx 里**不属于面板**的所有文件的内容摘要。
#
# 用它证明“面板只动了自己的文件”：两次摘要相等是内容级证据，不是“我们没调用写函数”。
nginx_foreign_digest() {
  find /etc/nginx -type f ! -path '*/conf.d/upanel-*' ! -path '*/upanel-auth/*' -print0 2>/dev/null |
    sort -z | xargs -0 sha256sum 2>/dev/null | sha256sum | awk '{print $1}'
}

# nginx_workers 是当前 worker 进程的 pid 列表。
#
# reload 不换 master、只换 worker：这是判断“到底有没有 reload”最直接的事实。
nginx_workers() { pgrep -f 'nginx: worker process' 2>/dev/null | sort | tr '\n' ' '; }

# managed_conf 列出面板自己的托管配置文件。
managed_conf() { ls "$NGINX_CONF_D"/upanel-*.conf 2>/dev/null | sort | tr '\n' ' '; }

# conf_of <域名>：托管配置文件路径。
conf_of() { printf '%s/upanel-%s.conf' "$NGINX_CONF_D" "$1"; }

# file_hash <路径>：内容 sha256（不存在时打印 missing）。
file_hash() {
  if [ -f "$1" ]; then sha256sum "$1" | awk '{print $1}'; else printf 'missing'; fi
}

# backup_count：备份目录里的文件份数（不存在算 0）。
backup_count() { ls -1 "$BACKUP_DIR" 2>/dev/null | wc -l | tr -d ' '; }

# nginx_t_ok：脚本自己跑一遍 nginx -t（不信任务自报）。
nginx_t_ok() { "$NGINX_BIN" -t >/dev/null 2>&1 && printf 'ok' || printf 'fail'; }

cleanup() {
  for _pid in "${UPSTREAM_PID:-}" "${WEB_PID:-}" "${AGENT_PID:-}"; do
    if [ -n "$_pid" ] && kill -0 "$_pid" 2>/dev/null; then
      kill -TERM "$_pid" 2>/dev/null
      wait "$_pid" 2>/dev/null
    fi
  done
  rm -f "$FIX_BROKEN"
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

# api <jar> <csrf> <方法> <路径> [body]：响应体落在 $WORK/resp.json。
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

ecode() { jget "$WORK/resp.json" 'd["error"]["code"]'; }
dget() { jget "$WORK/resp.json" "d[\"data\"]$1"; }

# create_site <载荷名> <body>：回显状态码；响应另存 $PAYLOADS/site-<名>.json。
create_site() {
  _code=$(api "$JAR" "$CSRF" POST "$PROXIES" "$2")
  cp "$WORK/resp.json" "$PAYLOADS/site-$1.json" 2>/dev/null
  printf '%s' "$_code"
}
# sget <载荷名> <data 下的字段路径>
sget() { jget "$PAYLOADS/site-$1.json" "d[\"data\"]$2"; }

# patch_site <载荷名> <id> <body>：回显状态码；响应另存。
patch_site() {
  _code=$(api "$JAR" "$CSRF" PATCH "$PROXIES/$2" "$3")
  cp "$WORK/resp.json" "$PAYLOADS/site-$1.json" 2>/dev/null
  printf '%s' "$_code"
}

# get_site <载荷名> <id>：回显状态码；响应另存（applied 状态从这里读）。
get_site() {
  _code=$(api "$JAR" "$CSRF" GET "$PROXIES/$2")
  cp "$WORK/resp.json" "$PAYLOADS/site-$1.json" 2>/dev/null
  printf '%s' "$_code"
}

# wait_applied <载荷名> <站点 id> <期望状态>
#
# 展示层的 Nginx 现状是**带 5 秒快照缓存**的（读列表不该每次都打一遍代理）。
# 因此刚应用完就 GET 有可能读到的还是上一份快照——那是缓存，不是“状态没变”。
# 这里等到快照刷新为止（最多 15 秒），断言仍然写在调用方，读的是这一次的响应。
wait_applied() {
  _i=0
  while [ "$_i" -lt 15 ]; do
    get_site "$1" "$2" >/dev/null
    [ "$(sget "$1" '["applied"]["state"]')" = "$3" ] && return 0
    _i=$((_i + 1))
    sleep 1
  done
  return 0
}

# put_secret <用途> <密码文件> <引用>：密码从文件进请求体，argv 里看不到明文。
put_secret() {
  python3 -c 'import json,sys
body={"purpose":sys.argv[1],"value":open(sys.argv[2], encoding="utf-8").read()}
if len(sys.argv)>3 and sys.argv[3]:
    body["reference"]=sys.argv[3]
print(json.dumps(body))' "$1" "$2" "${3:-}" >"$SECRETS_DIR/req.json"
  chmod 600 "$SECRETS_DIR/req.json"
  _code=$(curl -s -b "$JAR" -o "$WORK/secret.json" -w '%{http_code}' -X POST "$BASE$SECRETS_PATH" \
    -H 'Content-Type: application/json' -H "Origin: $ORIGIN" -H "X-CSRF-Token: $CSRF" \
    -d @"$SECRETS_DIR/req.json")
  shred -u "$SECRETS_DIR/req.json" 2>/dev/null || rm -f "$SECRETS_DIR/req.json"
  printf '%s' "$_code"
}

# preflight <动作> <参数 JSON>：响应另存 $WORK/preflight.json，回显状态码。
#
# 代理侧单次调用有 10 秒的整体超时（agentclient.DefaultTimeout）。预检要现采一次
# Nginx 现状，还要顺带确认上游目标在不在（这一项会触发应用/容器发现）——缓存过期时
# 这一次采集有可能越过 10 秒，回来的是 503 AGENT_UNAVAILABLE：那是**调用超时**，
# 不是判决，也不是"代理坏了"。它的消息读起来像后者，所以这里把耗时打印出来，
# 并且对这种传输层失败重试几次——断言的内容是判决，不是超时。
#
# 这是 P10.2 验收过程中观察到的既有问题（预检链路的超时设置），记录在 PROGRESS.md，
# 不在这里改：预检不是本阶段的产物，改它属于另一个范围。
preflight() {
  _try=0
  while : ; do
    printf '{"action":"%s","params":%s}' "$1" "$2" >"$WORK/pre.json"
    _out=$(curl -s -b "$JAR" -o "$WORK/resp.json" -w '%{http_code} %{time_total}' \
      -X POST "$BASE$PREFLIGHT_PATH" \
      -H 'Content-Type: application/json' -H "Origin: $ORIGIN" -H "X-CSRF-Token: $CSRF" \
      -d @"$WORK/pre.json")
    PRE_CODE=${_out%% *}
    PRE_TIME=${_out#* }
    cp "$WORK/resp.json" "$WORK/preflight.json"
    if [ "$PRE_CODE" = "503" ] &&
      [ "$(jget "$WORK/preflight.json" 'd["error"]["code"]')" = "AGENT_UNAVAILABLE" ] &&
      [ "$_try" -lt 4 ]; then
      _try=$((_try + 1))
      # 诊断走 stderr：本函数是在 $( ) 里被调用的，写 stdout 会被当成返回值收走。
      note "预检返回 503 AGENT_UNAVAILABLE（用时 ${PRE_TIME}s，第 $_try 次重试）：代理调用超时，不是判决" >&2
      sleep 3
      continue
    fi
    break
  done
  case "$PRE_TIME" in
    5.*|6.*|7.*|8.*|9.*) note "这一次预检用了 ${PRE_TIME}s（代理调用上限 10s，余量不大）" >&2 ;;
  esac
  printf '%s' "$PRE_CODE"
}

# submit_job <preflight_id> <幂等键>：响应存为 $WORK/submit.json，回显状态码。
submit_job() {
  python3 -c 'import json,sys
print(json.dumps({"preflight_id": sys.argv[1]}))' "$1" >"$WORK/jobreq.json"
  _code=$(curl -s -b "$JAR" -o "$WORK/resp.json" -w '%{http_code}' -X POST "$BASE$JOBS" \
    -H 'Content-Type: application/json' -H "Origin: $ORIGIN" -H "X-CSRF-Token: $CSRF" \
    -H "Idempotency-Key: $2" -d @"$WORK/jobreq.json")
  cp "$WORK/resp.json" "$WORK/submit.json"
  printf '%s' "$_code"
}

# wait_job <job_id>：轮询到终态，任务视图存为 $WORK/job.json，回显终态或 timeout。
wait_job() {
  _i=0
  while [ "$_i" -lt 400 ]; do
    api "$JAR" "$CSRF" GET "/api/v1/jobs/$1" >/dev/null
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

pget() { jget "$WORK/preflight.json" "$1"; }
jobget() { jget "$WORK/job.json" "$1"; }
pallowed() { pget 'd["data"]["allowed"]'; }
pall() { pget '",".join(b["code"] for b in d["data"]["blockers"])'; }
pwhy() { pget '"; ".join(b["message"] for b in d["data"]["blockers"])'; }
pwarn() { pget '",".join(w["code"] for w in (d["data"].get("warnings") or []))'; }
pimpact() { pget '",".join(i["kind"] for i in d["data"]["impacts"])'; }
plocks() { pget '",".join(d["data"]["locks"])'; }
preauth() { pget 'bool(d["data"].get("requires_reauth"))'; }
jstatus() { jobget 'd["data"]["status"]'; }
joutcome() { jobget 'd["data"]["result"]["outcome"]'; }
jverified() { jobget 'bool(d["data"]["result"].get("verified_at"))'; }
jerr() { jobget '(d["data"].get("error") or {}).get("code") or ""'; }
jerrmsg() { jobget '(d["data"].get("error") or {}).get("message") or ""'; }
jerrhint() { jobget '(d["data"].get("error") or {}).get("recovery_hint") or ""'; }
jrecovery() { jobget '(d["data"].get("result") or {}).get("recovery_hint") or ""'; }
jobobserved() { jobget "d[\"data\"][\"result\"][\"observed_state\"][\"$1\"]"; }
jobdata() { jobget "d[\"data\"][\"result\"][\"data\"].get(\"$1\")"; }

# run_apply <站点 id> <幂等键>
#
# 预检 → 再认证（proxy.apply 要求 R）→ 提交 → 等终态。结果放在全局变量里
# （不放进 $( ) 里：那会开子 shell，变量带不回来）。
run_apply() {
  PRE_CODE=$(preflight proxy.apply "{\"site_id\":\"$1\"}")
  cp "$WORK/preflight.json" "$PAYLOADS/preflight-$2.json"
  PRE_ALLOWED=$(pallowed)
  PRE_ID=$(pget 'd["data"]["preflight_id"]')
  if [ "$PRE_ALLOWED" = "True" ]; then
    REAUTH_CODE=$(api "$JAR" "$CSRF" POST /api/v1/auth/reauth "{\"password\":\"$PASSWORD\"}")
    [ "$REAUTH_CODE" = "200" ] || fail "再认证（$2）" "状态码 $REAUTH_CODE"
  fi
  if [ "$PRE_CODE" != "200" ] || [ -z "$PRE_ID" ]; then
    JOB_STATE="preflight:$PRE_CODE"
    JOB_ID=""
    : >"$WORK/job.json"
    return 0
  fi
  JOB_CODE=$(submit_job "$PRE_ID" "$2")
  cp "$WORK/submit.json" "$PAYLOADS/submit-$2.json"
  if [ "$JOB_CODE" = "202" ]; then
    JOB_ID=$(jget "$WORK/submit.json" 'd["data"]["job_id"]')
    JOB_STATE=$(wait_job "$JOB_ID")
    cp "$WORK/job.json" "$PAYLOADS/job-$2.json"
  else
    JOB_STATE="submit:$JOB_CODE"
    JOB_ID=""
    : >"$WORK/job.json"
    fail "提交（$2）" "状态码 $JOB_CODE，响应 $(head -c 300 "$WORK/submit.json")"
  fi
}

# via_nginx <域名> [curl 附加参数...]：穿过本机 nginx 取一次页面，回显响应体。
via_nginx() {
  _dom=$1; shift
  curl -s --max-time 10 -H "Host: $_dom" "$@" "http://127.0.0.1/" 2>/dev/null
}

mkdir -p "$WORK/logs" "$PAYLOADS" "$SECRETS_DIR" "$WEB_DB_DIR" "$UPSTREAM_DIR"
chown "$WEB_USER" "$WEB_DB_DIR"
# 两份日志从头记。start_agent / start_web 都是追加写：不截断的话，上一轮留下的
# 慢请求会让本轮的耗时断言（§13b）说谎——第一次重跑就是这么误报的。
: >"$AGENT_LOG"
: >"$WEB_LOG"

echo "== 0. 干净起点 =="
rm -f "$NGINX_CONF_D"/upanel-*.conf 2>/dev/null
rm -rf "$NGINX_AUTH_D" 2>/dev/null
rm -f "$FIX_BROKEN" 2>/dev/null
expect_eq "起点：没有面板的托管配置" "" "$(managed_conf)"
expect_eq "起点：nginx -t 通过" "ok" "$(nginx_t_ok)"
FOREIGN_BEFORE=$(nginx_foreign_digest)
note "非面板配置摘要（前）：$FOREIGN_BEFORE"
WORKERS_BASELINE=$(nginx_workers)
note "起始 worker：$WORKERS_BASELINE"

echo
echo "== 1. 配置与启动（代理 root、Web 非 root） =="
install -d -m 0755 /etc/upanel
install -d -m 0700 "$AGENT_DB_DIR"
install -d -m 0755 "$RUN_DIR"
rm -f "$WEB_DB" "$WEB_DB-wal" "$WEB_DB-shm"
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
expect_eq "数据库迁移到第 9 版（P10.2 不新增表）" "9" "$(dbq 'SELECT MAX(version) FROM schema_migrations')"

JAR=$WORK/admin.jar
expect_eq "管理员登录" "200" "$(login admin "$PASSWORD" "$JAR" login-admin)"
CSRF=$(csrf_of "$WORK/login-admin.json")
expect_ne "拿到 CSRF 令牌" "" "$CSRF"

# 登录要多久也记一笔。理由：验收机上 argon2id（m=128 MiB、t=3、p=2）实测
# 3.7–3.9 秒一次，而 DECISIONS Q23 记的是 100–300ms——差一个数量级。这不是
# 本阶段的缺陷，但“登录慢”很容易被当成网络抖动放过去，所以让它出现在日志里。
LOGIN_MS=$(web_max_duration /api/v1/auth/login)
note "本次登录耗时：${LOGIN_MS}ms（密码哈希是一次性成本，属 P02 参数标定范围）"

echo
echo "== 2. 测试上游（真实 HTTP 服务） =="
printf '%s\n' "$MARKER" >"$UPSTREAM_DIR/index.html"
( cd "$UPSTREAM_DIR" && exec python3 -m http.server "$UPSTREAM_PORT" --bind 127.0.0.1 ) \
  >>"$UPSTREAM_LOG" 2>&1 &
UPSTREAM_PID=$!
UP_OK=0
for _ in $(seq 1 50); do
  if curl -fsS "http://127.0.0.1:$UPSTREAM_PORT/" >/dev/null 2>&1; then UP_OK=1; break; fi
  sleep 0.1
done
if [ "$UP_OK" = "1" ]; then
  pass "上游在 127.0.0.1:$UPSTREAM_PORT 提供 HTTP 服务"
else
  fail "上游启动" "$(tail -3 "$UPSTREAM_LOG" 2>/dev/null)"
fi

echo
echo "== 3. 登记站点（P10.1 的路径；此时一个字节都不该写 /etc/nginx） =="
CONF_BEFORE_REG=$(managed_conf)
expect_eq "登记前托管配置为空" "" "$CONF_BEFORE_REG"
expect_eq "登记站点 A" "201" "$(create_site a "{\"domain\":\"$DOM_A\",\"upstream_target_id\":\"local_$UPSTREAM_PORT\",\"upstream_port\":$UPSTREAM_PORT}")"
SITE_A=$(sget a '["id"]')
expect_ne "拿到站点 A 的 id" "" "$SITE_A"
expect_eq "登记站点 A 之后仍然没有托管配置（登记不写配置）" "" "$(managed_conf)"

echo
echo "== 4. 预检：计划说清楚要动什么 =="
PRE_CODE=$(preflight proxy.apply "{\"site_id\":\"$SITE_A\"}")
cp "$WORK/preflight.json" "$PAYLOADS/preflight-plan.json"
expect_eq "预检状态码" "200" "$PRE_CODE"
expect_eq "计划放行" "True" "$(pallowed)"
expect_contains "影响里写明会改配置" "nginx_config" "$(pimpact)"
expect_contains "影响里写明会 reload" "nginx_reload" "$(pimpact)"
expect_contains "锁里有全局配置锁" "nginx:config" "$(plocks)"
expect_eq "要求再认证（改的是对外暴露面）" "True" "$(preauth)"
expect_eq "预检没有写任何配置" "" "$(managed_conf)"

# 客户端**不能**自带定义：参数只认 site_id，定义由面板从库里展开。
INJECT=$(preflight proxy.apply "{\"site_id\":\"$SITE_A\",\"definition\":{\"domain\":\"evil.example.test\",\"upstream_target_id\":\"local_1\",\"upstream_port\":1,\"access_mode\":\"public\",\"max_body_bytes\":1024,\"read_timeout_seconds\":60,\"enabled\":true}}")
expect_eq "预检拒绝客户端自带的定义（400）" "400" "$INJECT"
expect_eq "…错误码是参数不合法" "VALIDATION_ERROR" "$(jget "$WORK/preflight.json" 'd["error"]["code"]')"

echo
echo "== 5. 执行 proxy.apply：写入 → nginx -t → reload =="
CONF_A=$(conf_of "$DOM_A")
run_apply "$SITE_A" p102-apply-a
expect_eq "作业终态" "succeeded" "$JOB_STATE"
expect_eq "结果是 verified" "verified" "$(joutcome)"
expect_eq "带 verified_at" "True" "$(jverified)"
expect_eq "file_state=written" "written" "$(jobdata file_state)"
expect_eq "reload=done" "done" "$(jobdata reload)"
expect_eq "served=true" "True" "$(jobobserved served)"
expect_eq "tested=true" "True" "$(jobdata tested)"
expect_eq "config_file 就是托管路径" "$CONF_A" "$(jobdata config_file)"

expect_eq "托管配置真的在磁盘上" "present" "$(test -f "$CONF_A" && echo present || echo missing)"
expect_eq "权限是 644（nginx 可读、别的进程不可写）" "644" "$(stat -c %a "$CONF_A" 2>/dev/null)"
CONF_BODY=$(cat "$CONF_A" 2>/dev/null)
expect_contains "第一行是托管标记（改名清理靠它）" "# 由 uPanel 管理：站点 $SITE_A" "$CONF_BODY"
expect_contains "server_name 就是登记的域名" "server_name $DOM_A;" "$CONF_BODY"
expect_contains "上游指向本机端口" "proxy_pass http://127.0.0.1:$UPSTREAM_PORT;" "$CONF_BODY"
expect_contains "监听 80" "listen 80;" "$CONF_BODY"
expect_not_contains "没有任何一行监听面板端口 $PORT" "listen $PORT" "$CONF_BODY"
expect_not_contains "没有 443（那是 P10.3）" "listen 443" "$CONF_BODY"

expect_eq "脚本自己跑 nginx -t 也通过" "ok" "$(nginx_t_ok)"
WORKERS_AFTER=$(nginx_workers)
expect_ne "worker 换过一代（reload 真的发生了）" "$WORKERS_BASELINE" "$WORKERS_AFTER"

# 最要紧的一条：域名真的被代理到上游。
BODY=$(via_nginx "$DOM_A")
expect_contains "穿过 nginx 拿到上游页面（域名真的在服务）" "$MARKER" "$BODY"

wait_applied a-view "$SITE_A" applied
expect_eq "GET /proxies/:id 的 applied.state=applied" "applied" "$(sget a-view '["applied"]["state"]')"
expect_eq "applied 里带着配置路径" "$CONF_A" "$(sget a-view '["applied"]["config_file"]')"

echo
echo "== 6. 幂等：同样内容再应用一次什么都不做 =="
HASH_BEFORE=$(file_hash "$CONF_A")
BACKUPS_BEFORE=$(backup_count)
WORKERS_BEFORE=$(nginx_workers)
run_apply "$SITE_A" p102-apply-a-replay
expect_eq "重复应用的作业终态" "succeeded" "$JOB_STATE"
expect_eq "file_state=unchanged" "unchanged" "$(jobdata file_state)"
expect_eq "reload=skipped_unchanged" "skipped_unchanged" "$(jobdata reload)"
expect_eq "served 仍然是 true" "True" "$(jobobserved served)"
expect_eq "文件内容一个字节都没变" "$HASH_BEFORE" "$(file_hash "$CONF_A")"
expect_eq "没有新增备份" "$BACKUPS_BEFORE" "$(backup_count)"
expect_eq "worker 没有换（没有多余 reload）" "$WORKERS_BEFORE" "$(nginx_workers)"
expect_eq "重复应用之后仍然在提供服务" "$MARKER" "$(via_nginx "$DOM_A" | grep -o "$MARKER" | head -1)"

echo
echo "== 7. 失败恢复（一）：判定能看出来的坏配置 → 预检就拦下 =="
# 这一段和下一段故意用两种不同的坏配置，因为它们走到两个不同的分支：
#
#   7a  结构坏（括号没闭合）：面板的解析器看得见 → 判不了域名冲突 → 预检拦下，
#       连候选都不会写。这是“机器本来就坏了”时该有的行为：别往上叠。
#   7b  指令不认识：结构是好的，面板的解析器看不出来（它没有指令白名单），
#       nginx -t 才知道 → 门禁在写入之后拦下 → 回滚。这是门禁存在的理由。
#
# 两段合起来才是完整的一句话：**能看出来的时候不写，看不出来的时候写了也要能退回去。**
cat >"$FIX_BROKEN" <<'CONF'
server {
  listen 8099;
  server_name p102-broken.example.test;
CONF
chmod 0644 "$FIX_BROKEN"
expect_eq "夹具（未闭合的 server 块）让 nginx -t 失败" "fail" "$(nginx_t_ok)"

HASH_GOOD=$(file_hash "$CONF_A")
WORKERS_GOOD=$(nginx_workers)
PATCH_CODE=$(patch_site a "$SITE_A" '{"version":1,"read_timeout_seconds":120}')
expect_eq "改一次读超时（让内容真的要变）" "200" "$PATCH_CODE"

# 坏配置在场的这段时间里，管理面板必须照常可用——面板不走 nginx。
expect_eq "nginx 配置坏着的时候面板健康检查仍然 200" "200" \
  "$(curl -s -o /dev/null -w '%{http_code}' "$BASE/api/v1/health/live")"
expect_eq "…而且仍然能登录" "200" "$(login admin "$PASSWORD" "$WORK/broken-window.jar" login-broken)"
note "登录成功即证明：管理端不经过 nginx，配置写坏的最坏结果是站点不可用、面板照常"

PRE_CODE=$(preflight proxy.apply "{\"site_id\":\"$SITE_A\"}")
cp "$WORK/preflight.json" "$PAYLOADS/preflight-broken-tree.json"
expect_eq "预检状态码" "200" "$PRE_CODE"
expect_eq "配置树坏着的时候预检不放行" "False" "$(pallowed)"
expect_contains "…拦下的理由是判不了域名冲突" "PROXY_DOMAIN_UNKNOWN" "$(pall)"
expect_ne "…并且说明为什么判不了" "" "$(pwhy)"
expect_eq "…托管文件一个字节都没动" "$HASH_GOOD" "$(file_hash "$CONF_A")"
expect_eq "…worker 也没有换（没有 reload）" "$WORKERS_GOOD" "$(nginx_workers)"
expect_eq "…站点仍在用旧配置提供服务" "$MARKER" "$(via_nginx "$DOM_A" | grep -o "$MARKER" | head -1)"

rm -f "$FIX_BROKEN"
expect_eq "删掉坏文件后 nginx -t 恢复" "ok" "$(nginx_t_ok)"

echo
echo "== 7b. 失败恢复（二）：判定看不见的坏配置 → 写下去、被门禁拦下、回滚 =="
# 结构合法、指令非法：面板的解析器只能说“这里有个 server 块、域名是这个”，
# 判断不出 `p102_no_such_directive` 根本不存在。所以预检会放行——这不是缺陷，
# 而是分工：面板判“域名能不能用”，nginx -t 判“这份配置能不能加载”。
# 门禁与回滚就是为这一段窗口存在的。
cat >"$FIX_BROKEN" <<'CONF'
server {
  listen 8099;
  server_name p102-invalid.example.test;
  p102_no_such_directive on;
}
CONF
chmod 0644 "$FIX_BROKEN"
expect_eq "夹具（不认识的指令）确实让 nginx -t 失败" "fail" "$(nginx_t_ok)"

WORKERS_GOOD2=$(nginx_workers)
run_apply "$SITE_A" p102-apply-invalid-tree
expect_eq "预检确实放行了（面板看不出这条指令不存在）" "True" "$PRE_ALLOWED"
expect_eq "作业终态是 failed（不是 succeeded）" "failed" "$JOB_STATE"
expect_eq "错误码是 COMMAND_FAILED" "COMMAND_FAILED" "$(jerr)"
expect_contains "错误里点明是 nginx -t 没过" "nginx -t" "$(jerrmsg)"
expect_eq "托管文件逐字节回到旧内容（回滚生效）" "$HASH_GOOD" "$(file_hash "$CONF_A")"
expect_eq "worker 没有换（失败时绝不 reload）" "$WORKERS_GOOD2" "$(nginx_workers)"
expect_eq "站点仍在用旧配置提供服务" "$MARKER" "$(via_nginx "$DOM_A" | grep -o "$MARKER" | head -1)"
expect_eq "面板在失败之后照常健康" "200" "$(curl -s -o /dev/null -w '%{http_code}' "$BASE/api/v1/health/live")"

# 正对照：把那份坏配置删掉，同一条记录再应用一次就该成功。
rm -f "$FIX_BROKEN"
expect_eq "删掉坏文件后 nginx -t 恢复" "ok" "$(nginx_t_ok)"
run_apply "$SITE_A" p102-apply-recovered
expect_eq "重试的作业终态" "succeeded" "$JOB_STATE"
expect_eq "重试结果 verified" "verified" "$(joutcome)"
expect_contains "新配置带上了新的读超时" "proxy_read_timeout 120s;" "$(cat "$CONF_A")"
expect_ne "这次内容确实变了" "$HASH_GOOD" "$(file_hash "$CONF_A")"
expect_eq "而且真的 reload 了" "done" "$(jobdata reload)"

echo
echo "== 8. applied 是从事实推导的：手工改动会变成 stale =="
printf '# 这是有人手工加的一行\n' >>"$CONF_A"
wait_applied a-view "$SITE_A" stale
expect_eq "手工改过之后 applied.state=stale" "stale" "$(sget a-view '["applied"]["state"]')"
expect_ne "…说明里有理由" "" "$(sget a-view '["applied"]["reason"]')"
BACKUPS_BEFORE=$(backup_count)
run_apply "$SITE_A" p102-apply-after-drift
expect_eq "重新应用之后作业成功" "succeeded" "$JOB_STATE"
expect_eq "file_state=written（内容被改回来了）" "written" "$(jobdata file_state)"
wait_applied a-view "$SITE_A" applied
expect_eq "applied.state 回到 applied" "applied" "$(sget a-view '["applied"]["state"]')"
expect_eq "手工改动的那一份进了备份（备份在前）" "$((BACKUPS_BEFORE + 1))" "$(backup_count)"
expect_not_contains "托管文件里不再有手工那一行" "手工加的一行" "$(cat "$CONF_A")"

echo
echo "== 9. 改名：旧文件被清掉，旧域名不再被代理 =="
CONF_A2=$(conf_of "$DOM_A2")
PATCH_CODE=$(patch_site a "$SITE_A" "{\"version\":2,\"domain\":\"$DOM_A2\"}")
expect_eq "把域名改成 $DOM_A2" "200" "$PATCH_CODE"
run_apply "$SITE_A" p102-apply-rename
expect_eq "改名后的作业终态" "succeeded" "$JOB_STATE"
expect_eq "旧域名文件被移除" "missing" "$(test -f "$CONF_A" && echo present || echo missing)"
expect_eq "新域名文件在" "present" "$(test -f "$CONF_A2" && echo present || echo missing)"
expect_contains "恢复提示里提到移除了旧配置" "旧配置" "$(jrecovery)"
expect_eq "这台机器上只剩一份托管配置" "1" "$(managed_conf | wc -w | tr -d ' ')"
expect_contains "新域名在服务" "$MARKER" "$(via_nginx "$DOM_A2")"
expect_not_contains "旧域名不再指向上游" "$MARKER" "$(via_nginx "$DOM_A")"

echo
echo "== 10. 停用 → 文件移除；用删除记录证明“孤儿配置”门禁 =="
PATCH_CODE=$(patch_site a "$SITE_A" '{"version":3,"enabled":false}')
expect_eq "把站点停用" "200" "$PATCH_CODE"
run_apply "$SITE_A" p102-apply-disable
expect_eq "停用的作业终态" "succeeded" "$JOB_STATE"
expect_eq "file_state=removed" "removed" "$(jobdata file_state)"
expect_eq "served=false" "False" "$(jobobserved served)"
expect_eq "托管配置被移除" "missing" "$(test -f "$CONF_A2" && echo present || echo missing)"
wait_applied a-view "$SITE_A" not_applied
expect_eq "停用之后 applied.state=not_applied" "not_applied" "$(sget a-view '["applied"]["state"]')"
expect_not_contains "停用之后域名不再被代理" "$MARKER" "$(via_nginx "$DOM_A2")"
expect_eq "停用也留了备份（改主意时文件还在）" "True" \
  "$(ls -1 "$BACKUP_DIR" 2>/dev/null | grep -c "upanel-$DOM_A2.conf" | awk '{print ($1>0)?"True":"False"}')"

# 启用回去，然后试着删记录：托管配置还在，必须 409。
PATCH_CODE=$(patch_site a "$SITE_A" '{"version":4,"enabled":true}')
expect_eq "重新启用" "200" "$PATCH_CODE"
run_apply "$SITE_A" p102-apply-enable
expect_eq "重新启用的作业终态" "succeeded" "$JOB_STATE"
expect_eq "文件回来了" "present" "$(test -f "$CONF_A2" && echo present || echo missing)"
expect_eq "在服务" "$MARKER" "$(via_nginx "$DOM_A2" | grep -o "$MARKER" | head -1)"
DELETE_CODE=$(api "$JAR" "$CSRF" DELETE "$PROXIES/$SITE_A?version=5")
expect_eq "配置还在时删除记录被拒绝（409）" "409" "$DELETE_CODE"
expect_eq "…错误码是依赖阻塞" "DEPENDENCY_BLOCKED" "$(ecode)"
expect_eq "…记录还在（没有被悄悄删掉）" "1" "$(dbq "SELECT COUNT(*) FROM proxies WHERE id='$SITE_A'")"

# 先停用并应用（移除配置），再删记录：这时候才允许。
#
# 这里要等：文件确实已经删了，但**展示层的快照有 5 秒缓存**，删除前的判断读的
# 就是那份快照。所以头几次可能还是 409 —— 那是缓存，不是文件还在。断言的是
# “不超过 15 秒之后允许删”，而不是“立刻允许删”。
PATCH_CODE=$(patch_site a "$SITE_A" '{"version":5,"enabled":false}')
expect_eq "再次停用" "200" "$PATCH_CODE"
run_apply "$SITE_A" p102-apply-disable-again
expect_eq "移除配置的作业终态" "succeeded" "$JOB_STATE"
DELETE_CODE=""
_i=0
while [ "$_i" -lt 15 ]; do
  DELETE_CODE=$(api "$JAR" "$CSRF" DELETE "$PROXIES/$SITE_A?version=6")
  [ "$DELETE_CODE" != "409" ] && break
  _i=$((_i + 1))
  sleep 1
done
expect_eq "配置移除之后删除记录成功（200）" "200" "$DELETE_CODE"
expect_eq "库里没有这条记录了" "0" "$(dbq "SELECT COUNT(*) FROM proxies WHERE id='$SITE_A'")"

echo
echo "== 11. 站点 B：新域名从零开始（正对照） =="
expect_eq "登记站点 B" "201" "$(create_site b "{\"domain\":\"$DOM_B\",\"upstream_target_id\":\"local_$UPSTREAM_PORT\",\"upstream_port\":$UPSTREAM_PORT,\"websocket_enabled\":true}")"
SITE_B=$(sget b '["id"]')
CONF_B=$(conf_of "$DOM_B")
run_apply "$SITE_B" p102-apply-b
expect_eq "站点 B 作业终态" "succeeded" "$JOB_STATE"
expect_eq "站点 B 结果 verified" "verified" "$(joutcome)"
expect_contains "WebSocket 开关写进了配置" "proxy_set_header Upgrade \$http_upgrade;" "$(cat "$CONF_B")"
expect_contains "…并与 Connection 成对出现" 'proxy_set_header Connection "upgrade";' "$(cat "$CONF_B")"
expect_contains "站点 B 在服务" "$MARKER" "$(via_nginx "$DOM_B")"

echo
echo "== 12. basic auth：apr1 哈希 nginx 真的认 =="
BASIC_PW='vm-proxy-horse-6'
printf '%s' "$BASIC_PW" >"$SECRETS_DIR/proxy.pw"
chmod 600 "$SECRETS_DIR/proxy.pw"
# 秘密通道要求 5 分钟内的再认证。上面 §5 那次可能已经过期，这里明确再认证一次，
# 免得“传秘密失败”被读成“秘密通道坏了”。
REAUTH_CODE=$(api "$JAR" "$CSRF" POST /api/v1/auth/reauth "{\"password\":\"$PASSWORD\"}")
expect_eq "传秘密之前再认证" "200" "$REAUTH_CODE"
expect_eq "上传代理口令到秘密通道" "201" "$(put_secret proxy_basic_password "$SECRETS_DIR/proxy.pw" accept-p10-2-proxy)"
shred -u "$SECRETS_DIR/proxy.pw" 2>/dev/null || rm -f "$SECRETS_DIR/proxy.pw"
SECRET_REF=$(jget "$WORK/secret.json" 'd["data"]["secret_ref"]')
expect_ne "拿到 secret_ref" "" "$SECRET_REF"
expect_eq "秘密响应里没有明文" "0" \
  "$(grep -a -c -F "$BASIC_PW" "$WORK/secret.json" 2>/dev/null || true)"

expect_eq "登记站点 C（basic_auth）" "201" "$(create_site c "{\"domain\":\"$DOM_C\",\"upstream_target_id\":\"local_$UPSTREAM_PORT\",\"upstream_port\":$UPSTREAM_PORT,\"access_mode\":\"basic_auth\",\"basic_auth_users\":[{\"name\":\"alice\",\"secret_ref\":\"$SECRET_REF\"}]}")"
SITE_C=$(sget c '["id"]')
CONF_C=$(conf_of "$DOM_C")
AUTH_C=$NGINX_AUTH_D/$SITE_C.htpasswd
run_apply "$SITE_C" p102-apply-c
expect_eq "站点 C 作业终态" "succeeded" "$JOB_STATE"
expect_eq "站点 C 结果 verified" "verified" "$(joutcome)"
expect_eq "账号文件在 upanel-auth/ 下" "present" "$(test -f "$AUTH_C" && echo present || echo missing)"
AUTH_BODY=$(cat "$AUTH_C" 2>/dev/null)
expect_contains "账号文件是 apr1 哈希" "alice:\$apr1\$" "$AUTH_BODY"
expect_not_contains "账号文件里没有明文口令" "$BASIC_PW" "$AUTH_BODY"
expect_contains "配置引用账号文件" "auth_basic_user_file $AUTH_C;" "$(cat "$CONF_C")"
AUTH_MODE=$(stat -c %a "$AUTH_C" 2>/dev/null)
case "$AUTH_MODE" in
  640|644) pass "账号文件权限是 $AUTH_MODE（640 交给 worker 组，或 644 并在报告里点名风险）" ;;
  *) fail "账号文件权限" "得到 $AUTH_MODE，期望 640 或 644" ;;
esac
# 最要紧的两条：nginx 真的认这份哈希。
expect_eq "不带口令访问受保护站点 → 401" "401" \
  "$(curl -s -o /dev/null -w '%{http_code}' -H "Host: $DOM_C" http://127.0.0.1/)"
expect_eq "带正确口令 → 200" "200" \
  "$(curl -s -o /dev/null -w '%{http_code}' -u "alice:$BASIC_PW" -H "Host: $DOM_C" http://127.0.0.1/)"
expect_eq "…并且拿到上游页面" "$MARKER" \
  "$(via_nginx "$DOM_C" -u "alice:$BASIC_PW" | grep -o "$MARKER" | head -1)"
expect_eq "错误口令 → 401" "401" \
  "$(curl -s -o /dev/null -w '%{http_code}' -u "alice:wrong-password" -H "Host: $DOM_C" http://127.0.0.1/)"

echo
echo "== 13. /etc/nginx 里只多了面板自己的文件 =="
FOREIGN_AFTER=$(nginx_foreign_digest)
expect_eq "非面板配置内容摘要前后一致" "$FOREIGN_BEFORE" "$FOREIGN_AFTER"
MANAGED_COUNT=$(managed_conf | wc -w | tr -d ' ')
expect_eq "托管配置文件数（只剩站点 B 与站点 C）" "2" "$MANAGED_COUNT"
expect_contains "托管文件名都带 upanel- 前缀" "upanel-$DOM_B.conf" "$(managed_conf)"
note "托管文件：$(managed_conf)"
note "账号文件：$(ls -1 "$NGINX_AUTH_D" 2>/dev/null | tr '\n' ' ')"

# 留证据：托管配置、账号文件（只有哈希，没有明文）、权限、以及整棵树的 `nginx -T`。
# 这些是在盘上真实存在的东西，收尾会把它们删掉，所以先抄一份到 payloads/。
cp "$CONF_B" "$PAYLOADS/render-$DOM_B.conf" 2>/dev/null
cp "$CONF_C" "$PAYLOADS/render-$DOM_C.conf" 2>/dev/null
cp "$AUTH_C" "$PAYLOADS/htpasswd-site-c" 2>/dev/null
stat -c '%a %U:%G %n' "$CONF_B" "$CONF_C" "$AUTH_C" >"$PAYLOADS/permissions.txt" 2>/dev/null
"$NGINX_BIN" -T >"$PAYLOADS/nginx-T.txt" 2>&1 || true
pass "已留存渲染结果、账号文件、权限与 nginx -T 到 payloads/"

echo
echo "== 13b. 预检耗时：不许再出现“代理不可用”的假警报 =="
# 这条检查的来历不是虚构：本轮验收早期，预检偶尔要 6.7–9.3 秒，其中一次顶到
# agentclient 的 10 秒上限，用户看到 503 AGENT_UNAVAILABLE——“代理不可用”，
# 而 socket、权限、进程全都好好的。当时代理侧没有任何耗时日志，只能靠 Web 侧
# 计时倒推。现在代理会为超过 500ms 的请求记一笔（internal/agent/peer.go），
# 这条断言把“预检会不会被代理拖慢”变成每次验收都会回答的问题。
slow_pre=$(grep '"msg":"代理请求耗时偏长"' "$AGENT_LOG" 2>/dev/null | grep -c '/internal/v1/preflights' || true)
[ -n "$slow_pre" ] || slow_pre=0
expect_eq "代理侧没有把任何一次预检算成慢请求（>500ms）" "0" "$slow_pre"

MAX_PRE=$(web_max_duration /api/v1/operations/preflight)
note "本次运行最慢的一次预检：${MAX_PRE}ms（预算 10000ms）"
if [ "${MAX_PRE:-0}" -lt 2000 ]; then
  pass "最慢预检也在 2 秒以内"
else
  fail "最慢预检超过 2 秒" "${MAX_PRE}ms"
fi

echo
echo "== 14. 收尾：停用剩余站点、清掉夹具、机器回到干净状态 =="
# 停用是两步：改记录（enabled=false），再应用一次（渲染结果就是"文件不该存在"）。
# 只应用不改记录，等于拿同一份定义再跑一遍——那是幂等重放，不是停用。
PATCH_CODE=$(patch_site b-clean "$SITE_B" '{"version":1,"enabled":false}')
expect_eq "把站点 B 标为停用" "200" "$PATCH_CODE"
run_apply "$SITE_B" p102-apply-cleanup-b
expect_eq "停用站点 B" "succeeded" "$JOB_STATE"
expect_eq "…站点 B 的配置被移除" "missing" "$(test -f "$CONF_B" && echo present || echo missing)"
PATCH_CODE=$(patch_site c-clean "$SITE_C" '{"version":1,"enabled":false}')
expect_eq "把站点 C 标为停用" "200" "$PATCH_CODE"
run_apply "$SITE_C" p102-apply-cleanup-c
expect_eq "停用站点 C" "succeeded" "$JOB_STATE"
expect_eq "…站点 C 的账号文件被一起清掉" "missing" "$(test -f "$AUTH_C" && echo present || echo missing)"
expect_eq "托管配置全部移除" "" "$(managed_conf)"
expect_eq "账号文件目录里没有残留" "" "$(ls -1 "$NGINX_AUTH_D" 2>/dev/null | tr '\n' ' ')"
rm -f "$FIX_BROKEN"
expect_eq "收尾时 nginx -t 通过" "ok" "$(nginx_t_ok)"
rm -f "$WEB_DB" "$WEB_DB-wal" "$WEB_DB-shm" 2>/dev/null
pass "Web 库已清理（代理库里的卷与秘密是按需的，脚本不动）"

echo
echo "== NOT RUN（本阶段不验证的东西，写在这里而不是假装通过） =="
notrun "443 与证书签发/续期" "P10.3。托管站点当前只监听 80，脚本断言了配置里没有 listen 443"
notrun "管理面板 HTTPS 与 SSH 隧道文档" "P10.4 的打包内容；本脚本用真实端口 $PORT 验证“配置写坏不影响面板”"
notrun "代理侧对 panel_port 的二次校验（HTTP 不可达）" "Web 总会用真实端口覆盖参数；该规则由 internal/proxies 与 internal/agent 的单元测试覆盖"
notrun "WebSocket 握手实测" "测试上游（python http.server）不支持 Upgrade；只验配置里 Upgrade/Connection 成对出现"
notrun "浏览器渲染与交互" "VM 上没有浏览器"
notrun "真实 DNS 解析与外部访问" "VM 没有域名解析，用 Host 头代替"
notrun "密码哈希参数的跨机器标定" \
  "验收机实测 argon2id（m=128 MiB、t=3、p=2）单次 3.7–3.9 秒，与 DECISIONS Q23 记的 100–300ms 不符；属 P02 参数标定，改动会动到安全参数，不在 P10.2 里顺手改"

echo
echo "=============================================="
printf '通过 %s 项，失败 %s 项，未执行 %s 项\n' "$PASSED" "$FAILED" "$NOTRUN"
if [ "$FAILED" -gt 0 ]; then
  printf '失败项：%s\n' "$FAILED_NAMES"
  exit 1
fi
exit 0
