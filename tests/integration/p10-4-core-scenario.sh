#!/bin/sh
# P10.4 Linux 验收脚本（第二部分）：在**装好的那一套**上跑完整核心场景。
#
# 第一部分（p10-4-install-acceptance.sh）证明的是“装得成、装完是浏览器可用的、
# 权限是文档写的那样”。这一部分接在它后面，用的是 systemd 起的那两个进程、
# /etc/upanel/upanel.json 这份真实配置、9443 上的真实 TLS —— 不再自己起进程，
# 也不再手写一份验收专用配置。差别就在这里：
#
#   自起进程跑通 = “代码能工作”；装好的单元跑通 = “**交付物**能工作”。
#
# 要证明的（对应 STAGES §12 第 4 条的验收行）：
#
#   1) **域名正常访问指定应用**：站点登记 → 应用 → `curl` 用域名穿过本机 nginx
#      拿到**这个上游**的 marker 页面（不是 nginx 欢迎页）。
#   2) **WebSocket 不断流**：上游按 0.5 秒推 12 条，客户端（走 nginx）12 条全收到、
#      相邻间隔没有异常拉长、客户端发的一条被 echo 回来。
#   3) **同域名 / 端口冲突拒绝**：重复域名 409；上游端口填成面板自己的端口 400。
#   4) **错误 Nginx 配置不破坏已有站点**：塞一份 nginx -t 过不去的配置 → 应用失败、
#      托管文件逐字节回滚、worker 不换、旧站点照常服务、**面板照常登录**。
#   5) **证书测试用 staging**：签发走本地 Pebble（测试 ACME），签发者里是 Pebble；
#      绑定之后 443 用系统信任链（不给 -k）取到 marker，服务的那张证书指纹与账本一致；
#      wss 同样 12 条不断流。
#   6) **私钥不可经文件 / API / 日志读取**：upanel-web 账号读不到 privkey.pem；证书接口响应、
#      面板库、面板日志里都搜不到 "PRIVATE KEY"。
#   7) **重启之后仍然成立**：两个单元 restart 之后 443、wss、面板登录都还在。
#
# 用法（root，在装完的机器上）：
#   sudo PASSWORD='...' sh tests/integration/p10-4-core-scenario.sh
#
# 环境变量：PASSWORD（默认装机器时那个口令）、WORK（默认 /var/tmp/upanel-accept-p10-4-core）、
# WS_PORT（默认 18091）、PEBBLE_BIN（默认 /usr/local/bin/pebble）、
# PEBBLE_CONFIG（默认 /opt/p103/pebble/config.json）、CERT_MODE（pebble|skip，默认 pebble）、
# RUN_TAG（默认当前时间；决定测试域名 p104a-<RUN_TAG>.example.test 与幂等键前缀）。
#
# 退出码：0 全部通过；1 有失败项；2 环境不满足。
#
# 对这台机器的改动（收尾逐个撤销）：
#   - /etc/hosts 加两条测试域名（备份 → 还原）；
#   - /etc/upanel/upanel.json 临时加 agent.certificates.staging_directory_url
#     （原文件备份 → 还原 → restart）；
#   - 系统信任库临时加 Pebble 的 API 证书与根证书（删掉并 update-ca-certificates）；
#   - iptables nat 把 Pebble 的校验口 5002 引到 80（删掉那条规则）；
#   - /etc/nginx/conf.d/upanel-*.conf、/etc/ssl/upanel、/etc/letsencrypt/live/p104a-*
#     下的验收产物（删掉）；
#   - certbot 里**测试 ACME** 的账号登记目录（/etc/letsencrypt/accounts/<测试 host:port>/，
#     起一个全新的 Pebble 时先删掉，好让 certbot 重新注册；真实 Let's Encrypt 的账号不碰）；
#   - 起/停 Pebble 与测试上游。
#
# 明确不覆盖（NOT RUN，原因写在这里，不假装通过）：
#   - **真实公网域名与真实 Let's Encrypt**：会消耗速率限额，且需要公网 80 端口。
#     签发这一段连的是本地 Pebble（测试 ACME）；生产地址那一条由“必须显式选 staging”
#     + 配置默认值共同保证，见 p10-3-cert-acceptance.sh。
#   - **浏览器渲染与“地址栏变绿”**：VM 上没有浏览器。HTTPS 用 curl/openssl 与
#     python 的 ssl 验证（含系统信任链），渲染事实不在这一层。
#   - **Windows / macOS 宿主**：0.1 不支持。
#   - **22.04 与 24.04 的差异行**：同一个脚本在两边各跑一遍，结果分别落在证据目录里；
#     脚本本身不假设发行版，只打印它。

set -u

WORK=${WORK:-/var/tmp/upanel-accept-p10-4-core}
WS_PORT=${WS_PORT:-18091}
PEBBLE_BIN=${PEBBLE_BIN:-/usr/local/bin/pebble}
PEBBLE_CONFIG=${PEBBLE_CONFIG:-/opt/p103/pebble/config.json}
CERT_MODE=${CERT_MODE:-pebble}
PASSWORD=${PASSWORD:-install-correct-horse-9}
HERE=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
WS_UPSTREAM="$HERE/p10-4-ws-upstream.py"
WS_CLIENT="$HERE/p10-4-ws-through-proxy.py"

SYS_CFG=/etc/upanel/upanel.json
PANEL_CERT=/etc/upanel/tls/panel.pem
BIN_DIR=/usr/local/bin
CTL=$BIN_DIR/upanelctl
WEB_USER=upanel-web
# Web 账号的 uid 由 useradd --system 分配，不是固定值（24.04 上是 999，22.04 上另算），
# 所以这里不写死一个数字：要断言的是“它不是 0、也读不到私钥”，不是“它等于 999”。

PROXIES=/api/v1/proxies
PREFLIGHT_PATH=/api/v1/operations/preflight
JOBS=/api/v1/jobs
CERT_PATH=/api/v1/certificates

NGINX_CONF_D=/etc/nginx/conf.d
NGINX_AUTH_D=/etc/nginx/upanel-auth
LIVE_DIR=/etc/letsencrypt/live
ACME_WEBROOT=/var/www/upanel-acme
ACME_API_URL=https://127.0.0.1:14000/dir
ACME_API_CERT=/opt/p103/pebble/certs/cert.pem
ACME_API_KEY=/opt/p103/pebble/certs/key.pem
ACME_ROOT_URL=https://127.0.0.1:15000/roots/0
CA_TRUST_DIR=/usr/local/share/ca-certificates
CA_API_FILE=$CA_TRUST_DIR/p104-acme-api.crt
CA_ROOT_FILE=$CA_TRUST_DIR/p104-acme-root.crt
FIX_BROKEN=$NGINX_CONF_D/p104-broken.conf
FIX_INVALID=$NGINX_CONF_D/p104-invalid.conf

# 每轮一个**唯一**的测试域名与幂等键。两个理由都是产品行为，不该为了跑第二遍去改产品：
#   · ACME 对同一个域名一小时内只允许一次签发（失败的尝试也算一次），共用域名会让一小时
#     内的第二次执行在“签发”那一步收到 429 RESOURCE_BUSY；
#   · Idempotency-Key 绑定请求体：同一个键配另一份 request body 就是 409
#     IDEMPOTENCY_CONFLICT —— 换成新站点 id 之后旧的 `p104-apply-a` 键必然撞车。
# 所以每轮换域名、换幂等键（RUN_TAG 可以外部指定以复现某一轮）。
RUN_TAG=${RUN_TAG:-$(date +%m%d-%H%M%S)}
DOM_A=${DOM_A:-p104a-$RUN_TAG.example.test}
WS_TICKS=12
MARKER=p104-upstream-marker

LOGS=$WORK/logs
PAYLOADS=$WORK/payloads
WEBS=$WORK/ws
AGENT_LOG=$LOGS/agent.log
WEB_LOG=$LOGS/web.log
UPSTREAM_LOG=$LOGS/upstream.log
PEBBLE_LOG=$LOGS/pebble.log
PASSED=0
FAILED=0
NOTRUN=0
FAILED_NAMES=""
UPSTREAM_PID=""
PEBBLE_STARTED=0
CONFIG_PATCHED=0
HOSTS_PATCHED=0
CA_INSTALLED=0
NAT_ADDED=0
BASE=""
ORIGIN=""
JAR=$WORK/jar.txt
CSRF=""

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

# file_hash <路径>：内容 sha256（不存在时打印 missing）。
file_hash() {
  if [ -f "$1" ]; then sha256sum "$1" | awk '{print $1}'; else printf 'missing'; fi
}
conf_of() { printf '%s/upanel-%s.conf' "$NGINX_CONF_D" "$1"; }
managed_conf() { ls "$NGINX_CONF_D"/upanel-*.conf 2>/dev/null | sort | tr '\n' ' '; }
nginx_workers() { pgrep -f 'nginx: worker process' 2>/dev/null | sort | tr '\n' ' '; }
nginx_t_ok() { nginx -t >/dev/null 2>&1 && printf 'ok' || printf 'fail'; }

# site_body：取站点正文。
# §6 之后站点绑了证书，80 上只剩 ACME 校验位置和一个 301 到 https（见
# internal/proxies/render.go 的 writeHTTPToHTTPSBlock），所以取正文一律跟跳转：
# 既验证“旧配置里那条跳转还在”，也真的把 443 上的正文取回来。
site_body() { curl -sSL --max-time 20 -H "Host: $DOM_A" http://127.0.0.1/ 2>/dev/null; }
site_body_tls() { curl -sS --max-time 20 "https://$DOM_A/" 2>/dev/null; }

# wait_body <期望文本> <超时秒> <取正文的命令>……：轮询到正文里出现期望文本为止。
# 为什么不能用一个瞬时请求判“域名访问到的是这个上游”：nginx 的优雅 reload 有一个很短的
# 窗口，reload 命令返回之后旧 worker 仍可能应答一两个**新**连接。22.04 / nginx 1.18 上实测：
# 单次 reload 之后 6—111ms 内新配置开始应答（5 次取样），而连续 reload 时 20 次里有 7 次
# 抓到旧配置。产品侧没有假装“reload 返回＝已经生效”（`verified` 的含义是落盘+读回+reload
# 完成，见 internal/proxies/applyrun.go），验收侧也就不该把 10 毫秒的窗口当成功能坏了。
# 这里等到出现为止，并把等了多少毫秒打出来——等得久就是信号，不是噪声。
wait_body() {
  _want=$1; _secs=$2; shift 2
  _t0=$(date +%s%N)
  _deadline=$(( $(date +%s) + _secs ))
  _body=""
  while :; do
    _body=$("$@")
    case "$_body" in *"$_want"*) break ;; esac
    [ "$(date +%s)" -lt "$_deadline" ] || break
    sleep 0.1
  done
  # 走 stderr：这两个函数的结果是**标准输出上的正文/状态码**，调用方常写成 $(…)。
  # 把提示打进 stdout 就会被一起捕获，`expect_eq` 于是拿“提示 + 结果”去比“结果”（第一次改就踩了）。
  note "等正文用了 $(( ($(date +%s%N) - _t0) / 1000000 ))ms" >&2
  printf '%s' "$_body"
}

# wait_code <期望状态码> <秒> <curl 参数...>：等状态码**收敛**，再断言。
# 用在“reload 之后应该变成什么状态”这类断言上：它断言的是“N 秒内收敛”，不是“这一刻已经是”。
# 收敛不了照样红，并把最后拿到的码写进 note —— 不是把断言放松成“随便”。
wait_code() {
  _want=$1; _secs=$2; shift 2
  _t0=$(date +%s%N)
  _deadline=$(( $(date +%s) + _secs ))
  _code=""
  while :; do
    _code=$(curl -sS -o /dev/null -w '%{http_code}' --max-time 10 "$@" 2>/dev/null)
    [ "$_code" = "$_want" ] && break
    [ "$(date +%s)" -lt "$_deadline" ] || break
    sleep 0.1
  done
  note "等状态码 $_want 用了 $(( ($(date +%s%N) - _t0) / 1000000 ))ms（最后拿到 ${_code:-空}）" >&2
  printf '%s' "$_code"
}

# panel <curl 参数...>：所有面板请求都带上安装的那张证书（不加 -k）。
panel() { curl -sS --cacert "$PANEL_CERT" --max-time 20 "$@"; }

login() {
  _jar=$1; _pw=$2
  _code=$(panel -c "$_jar" -o "$WORK/login.json" -w '%{http_code}' -X POST "$BASE/api/v1/auth/login" \
    -H 'Content-Type: application/json' -H "Origin: $ORIGIN" -H 'X-UPanel-Request: 1' \
    -d "{\"username\":\"admin\",\"password\":\"$_pw\"}")
  # CSRF 令牌跟会话绑定，每次登录换一枚。脚本中途会再登录一次（第 8 节“重启后还能用
  # 口令登录”），那一步之后第 2 节拿到的令牌就属于旧会话了 —— 收尾的 PATCH 会回
  # 403 CSRF_FAILED。那不是产品缺陷，是脚本举着一枚过期令牌去写。
  # 令牌落在文件里而不是变量里：`$(login ...)` 是**子 shell**，里面赋值传不出来
  # （第一次这样修，收尾照样 403），所以每次写操作都从文件里现读。
  # 文件按 cookie jar 分：第 7 节会用另一个 jar 再登录一次（验证“配置坏着也能登录”），
  # 那次登录换的是**另一个会话**的令牌，不能拿它盖掉主会话的令牌 —— 否则接下来用主 jar
  # 发的预检全部 403 CSRF_FAILED（第二次这样修踩的坑）。
  if [ "$_code" = '200' ]; then
    _fresh=$(csrf_of)
    case "$_fresh" in ''|ERR:*|None) ;; *) CSRF=$_fresh; printf '%s' "$_fresh" >"$(csrf_file "$_jar")" ;; esac
  fi
  printf '%s' "$_code"
}
csrf_of() { jget "$WORK/login.json" 'd["data"]["csrf_token"]'; }
csrf_file() { printf '%s/csrf-%s.txt' "$WORK" "$(basename "$1")"; }
csrf_now() { cat "$(csrf_file "$JAR")" 2>/dev/null; }

# api <方法> <路径> [body]：响应体落在 $WORK/resp.json，回显状态码。
api() {
  _method=$1; _path=$2; _body=${3:-}
  if [ -n "$_body" ]; then
    printf '%s' "$_body" >"$WORK/req.json"
    panel -b "$JAR" -o "$WORK/resp.json" -w '%{http_code}' -X "$_method" "$BASE$_path" \
      -H 'Content-Type: application/json' -H "Origin: $ORIGIN" -H "X-CSRF-Token: $(csrf_now)" \
      -d @"$WORK/req.json"
  else
    panel -b "$JAR" -o "$WORK/resp.json" -w '%{http_code}' -X "$_method" "$BASE$_path" \
      -H "Origin: $ORIGIN" -H "X-CSRF-Token: $(csrf_now)"
  fi
}

# preflight <动作> <参数 JSON>：响应存 $WORK/preflight.json，回显状态码。
# 代理侧单次调用有 10 秒整体超时，缓存过期时会回 503 AGENT_UNAVAILABLE —— 那是
# 调用超时，不是判决。只对这种情况重试，并把耗时打出来。
preflight() {
  _try=0
  while : ; do
    printf '{"action":"%s","params":%s}' "$1" "$2" >"$WORK/pre.json"
    _out=$(panel -b "$JAR" -o "$WORK/resp.json" -w '%{http_code} %{time_total}' \
      -X POST "$BASE$PREFLIGHT_PATH" \
      -H 'Content-Type: application/json' -H "Origin: $ORIGIN" -H "X-CSRF-Token: $(csrf_now)" \
      -d @"$WORK/pre.json")
    PRE_CODE=${_out%% *}
    PRE_TIME=${_out#* }
    cp "$WORK/resp.json" "$WORK/preflight.json"
    if [ "$PRE_CODE" = "503" ] &&
      [ "$(jget "$WORK/preflight.json" 'd["error"]["code"]')" = "AGENT_UNAVAILABLE" ] &&
      [ "$_try" -lt 4 ]; then
      _try=$((_try + 1))
      note "预检返回 503 AGENT_UNAVAILABLE（用时 ${PRE_TIME}s，第 $_try 次重试）：代理调用超时，不是判决" >&2
      sleep 3
      continue
    fi
    break
  done
  printf '%s' "$PRE_CODE"
}

pget() { jget "$WORK/preflight.json" "$1"; }
pallowed() { pget 'd["data"]["allowed"]'; }
pall() { pget '",".join(b["code"] for b in d["data"]["blockers"])'; }
pwhy() { pget '"; ".join(b["message"] for b in d["data"]["blockers"])'; }
p_id() { pget 'd["data"]["preflight_id"]'; }
jobget() { jget "$WORK/job.json" "$1"; }
jstatus() { jobget 'd["data"]["status"]'; }
joutcome() { jobget 'd["data"]["result"]["outcome"]'; }
jerrmsg() { jobget '(d["data"].get("error") or {}).get("message") or ""'; }
jobdata() { jobget "d[\"data\"][\"result\"][\"data\"].get(\"$1\")"; }

# issue_badnonce <作业开始时刻>：这一次签发失败是不是测试 ACME 的**随机 nonce 拒绝**。
#
# 判据只能取自代理的 journal：作业接口对这种失败只回“动作执行失败”（错误码 COMMAND_FAILED），
# 真正的 ACME 原因写在 agent 日志里 —— 那正是 P10.3 修 certbot 错误透传时补的链路，
# 它进日志、不进 API（这一点记在结果文件里，是产品的一个已知粗糙面，不是本轮要改的）。
# 只搜“本次尝试开始之后”的日志，避免拿上一轮的旧记录触发重试。
issue_badnonce() {
  journalctl -u upanel-agent --since "$1" --no-pager 2>/dev/null | grep -q 'anti-replay nonce'
}

submit_job() {
  python3 -c 'import json,sys
print(json.dumps({"preflight_id": sys.argv[1]}))' "$1" >"$WORK/jobreq.json"
  panel -b "$JAR" -o "$WORK/resp.json" -w '%{http_code}' -X POST "$BASE$JOBS" \
    -H 'Content-Type: application/json' -H "Origin: $ORIGIN" -H "X-CSRF-Token: $(csrf_now)" \
    -H "Idempotency-Key: $2" -d @"$WORK/jobreq.json"
  cp "$WORK/resp.json" "$WORK/submit.json"
}

wait_job() {
  _i=0
  while [ "$_i" -lt 900 ]; do
    api GET "/api/v1/jobs/$1" >/dev/null
    cp "$WORK/resp.json" "$WORK/job.json"
    _st=$(jstatus)
    case "$_st" in
      succeeded|failed|canceled|interrupted) printf '%s' "$_st"; return 0 ;;
    esac
    _i=$((_i + 1))
    sleep 0.2
  done
  printf 'timeout'
}

# run_action <动作> <参数 JSON> <幂等键后缀>：预检 → 再认证（需要 R 的动作）→ 提交 → 等终态。
# 结论放进全局变量 JOB_STATE / PRE_ALLOWED / PRE_CODE，不进 $( )（子 shell 带不回来）。
run_action() {
  PRE_CODE=$(preflight "$1" "$2")
  cp "$WORK/preflight.json" "$PAYLOADS/preflight-$3.json"
  PRE_ALLOWED=$(pallowed)
  _pid=$(p_id)
  if [ "$PRE_CODE" = "200" ] && [ "$PRE_ALLOWED" = "True" ]; then
    _rc=$(api POST /api/v1/auth/reauth "{\"password\":\"$PASSWORD\"}")
    [ "$_rc" = "200" ] || fail "再认证（$3）" "状态码 $_rc"
  fi
  if [ "$PRE_CODE" != "200" ] || [ -z "$_pid" ]; then
    JOB_STATE="preflight:$PRE_CODE"
    JOB_ID=""
    : >"$WORK/job.json"
    return 0
  fi
  _jc=$(submit_job "$_pid" "$RUN_TAG-$3")
  cp "$WORK/submit.json" "$PAYLOADS/submit-$3.json"
  if [ "$_jc" = "202" ]; then
    JOB_ID=$(jget "$WORK/submit.json" 'd["data"]["job_id"]')
    JOB_STATE=$(wait_job "$JOB_ID")
    cp "$WORK/job.json" "$PAYLOADS/job-$3.json"
  else
    JOB_STATE="submit:$_jc"
    JOB_ID=""
    : >"$WORK/job.json"
    fail "提交（$3）" "状态码 $_jc，响应 $(head -c 300 "$WORK/submit.json")"
  fi
}

# site_get <路径>：GET 一个站点，响应存 $WORK/site.json。
site_get() { api GET "$PROXIES/$1" >/dev/null; cp "$WORK/resp.json" "$WORK/site.json"; }
sget() { jget "$WORK/site.json" "d[\"data\"]$1"; }

# remove_site <id> <幂等键后缀>：按**文档写的顺序**移除一个站点。
#   ① PATCH enabled=false → proxy.apply 一次：这一步把托管配置从 /etc/nginx/conf.d 移除；
#   ② 再 DELETE 记录（?version= 用第 ② 步之后的版本）。
# 顺序不能反。记录删了而文件还在，就留下一份没有任何界面能解释、却仍在生效并代理流量的
# server 块，所以服务端对这种删除回 409（P10.2 的孤儿配置门禁）。
# 早先收尾是 "DELETE 然后 rm -f 配置文件"：DELETE 被 409 挡住没人看，rm 又把证据删了，
# 结果记录留在库里，下一轮在“登记站点”那一步 409 —— 一次假失败掩盖一次真失败。
# 返回 0/1，失败原因打在 note 里，由调用方决定算 FAIL 还是只是提示。
remove_site() {
  _rid=$1; _rsuf=$2
  site_get "$_rid"
  _rver=$(sget '["version"]')
  case "$_rver" in ''|*ERR*|None) note "读不到站点 $_rid 的 version，没有移除"; return 1 ;; esac
  if [ "$(sget '["enabled"]')" != 'False' ]; then
    _rc=$(api PATCH "$PROXIES/$_rid" "{\"version\":$_rver,\"enabled\":false}")
    if [ "$_rc" != '200' ]; then
      note "置 enabled=false 失败：HTTP $_rc $(head -c 200 "$WORK/resp.json")"
      return 1
    fi
    site_get "$_rid"
    _rver=$(sget '["version"]')
  fi
  run_action proxy.apply "{\"site_id\":\"$_rid\"}" "$_rsuf"
  case "$JOB_STATE" in
    succeeded) ;;
    *) note "移除托管配置的 apply 终态 $JOB_STATE：$(jerrmsg)"; return 1 ;;
  esac
  _rc=$(api DELETE "$PROXIES/$_rid?version=$_rver")
  if [ "$_rc" != '200' ]; then
    note "删除记录失败：HTTP $_rc $(head -c 200 "$WORK/resp.json")"
    return 1
  fi
  return 0
}

# ws_run <名字> <url> [--ca 文件]：跑一次 WebSocket 透传，报告存 $WEBS/<名字>.json。
ws_run() {
  _name=$1; _url=$2; shift 2
  python3 "$WS_CLIENT" --url "$_url" --ticks "$WS_TICKS" --out "$WEBS/$_name.json" "$@" \
    >"$LOGS/ws-$_name.txt" 2>&1
  printf '%s' "$WEBS/$_name.json"
}

fingerprint_of() {
  openssl x509 -in "$1" -noout -fingerprint -sha256 2>/dev/null | sed 's/^.*=//; s/://g' | tr 'A-F' 'a-f'
}

cleanup() {
  if [ "$CONFIG_PATCHED" = "1" ] && [ -f "$WORK/upanel.json.orig" ]; then
    cp -p "$WORK/upanel.json.orig" "$SYS_CFG"
    systemctl restart upanel-agent upanel-web >/dev/null 2>&1
    note "已还原 $SYS_CFG 并重启两个单元"
  fi
  rm -f "$FIX_BROKEN" "$FIX_INVALID" 2>/dev/null
  if [ "$HOSTS_PATCHED" = "1" ] && [ -f "$WORK/hosts.orig" ]; then
    cp -p "$WORK/hosts.orig" /etc/hosts
  fi
  if [ "$CA_INSTALLED" = "1" ]; then
    rm -f "$CA_API_FILE" "$CA_ROOT_FILE"
    update-ca-certificates >/dev/null 2>&1
  fi
  if [ "$NAT_ADDED" = "1" ]; then
    iptables -t nat -D OUTPUT -p tcp -d 127.0.0.1 --dport 5002 -j REDIRECT --to-ports 80 2>/dev/null
    iptables -t nat -D PREROUTING -p tcp -d 127.0.0.1 --dport 5002 -j REDIRECT --to-ports 80 2>/dev/null
  fi
  if [ -n "${UPSTREAM_PID:-}" ] && kill -0 "$UPSTREAM_PID" 2>/dev/null; then
    kill -TERM "$UPSTREAM_PID" 2>/dev/null
    wait "$UPSTREAM_PID" 2>/dev/null
  fi
  if [ "$PEBBLE_STARTED" = "1" ] && [ -n "${PEBBLE_PID:-}" ] && kill -0 "$PEBBLE_PID" 2>/dev/null; then
    kill -TERM "$PEBBLE_PID" 2>/dev/null
    wait "$PEBBLE_PID" 2>/dev/null
  fi
  rm -f "$NGINX_CONF_D"/upanel-*.conf 2>/dev/null
  rm -rf "$NGINX_AUTH_D" 2>/dev/null
  rm -rf "$LIVE_DIR"/p104a-* 2>/dev/null
  nginx -t >/dev/null 2>&1 && systemctl reload nginx >/dev/null 2>&1
  return 0
}
trap cleanup EXIT
trap 'cleanup; exit 130' INT
trap 'cleanup; exit 143' TERM

if [ "$(id -u)" -ne 0 ]; then
  printf '本脚本必须以 root 运行：sudo PASSWORD=... sh %s\n' "$0" >&2
  exit 2
fi
for f in "$CTL" "$WS_UPSTREAM" "$WS_CLIENT"; do
  if [ ! -e "$f" ]; then printf '缺少 %s\n' "$f" >&2; exit 2; fi
done
for tool in curl openssl python3 nginx systemctl iptables journalctl; do
  if ! command -v "$tool" >/dev/null 2>&1; then printf '缺少依赖：%s\n' "$tool" >&2; exit 2; fi
done
if [ ! -s "$SYS_CFG" ]; then
  printf '找不到 %s：先跑 p10-4-install-acceptance.sh 把 0.1 装起来\n' "$SYS_CFG" >&2
  exit 2
fi
rm -rf "$WORK"
mkdir -p "$LOGS" "$PAYLOADS" "$WEBS"

# ---------------------------------------------------------------- 0. 起点
printf '==== P10.4 核心场景（第二部分）====\n'
printf '\n--- 0. 环境与起点 ---\n'
OS_ID=$(sed -n 's/^ID=//p' /etc/os-release | tr -d '"')
OS_VER=$(sed -n 's/^VERSION_ID=//p' /etc/os-release | tr -d '"')
note "发行版：$OS_ID $OS_VER"
note "内核：$(uname -r)"
note "nginx：$(nginx -v 2>&1)"
if command -v certbot >/dev/null 2>&1; then
  note "certbot：$(certbot --version 2>&1)"
else
  note "certbot：未安装"
fi
note "upanel-web sha256：$(sha256sum "$BIN_DIR/upanel-web" | awk '{print $1}')"
note "upanelctl sha256：$(sha256sum "$CTL" | awk '{print $1}')"
note "upanel-agent sha256：$(sha256sum /usr/local/lib/upanel/upanel-agent 2>/dev/null | awk '{print $1}')"
if [ "$OS_VER" != "22.04" ] && [ "$OS_VER" != "24.04" ]; then
  fail "发行版在支持范围内（22.04 / 24.04）" "$OS_ID $OS_VER"
else
  pass "发行版在支持范围内（$OS_VER）"
fi

expect_eq "upanel-web.service 处于 active" 'active' "$(systemctl is-active upanel-web.service)"
expect_eq "upanel-agent.service 处于 active" 'active' "$(systemctl is-active upanel-agent.service)"
expect_eq "装机用的那张面板证书在" 'yes' "$([ -s "$PANEL_CERT" ] && echo yes || echo no)"

CFG_JSON=$("$CTL" check-config --config "$SYS_CFG" --json 2>/dev/null)
CFG_LISTEN=$(printf '%s\n' "$CFG_JSON" | sed -n 's/.*"web_listen": *"\([^"]*\)".*/\1/p')
if [ -z "$CFG_LISTEN" ]; then
  printf '读不出 web.listen_address（check-config --json 失败）：%s\n' "$CFG_JSON" >&2
  exit 2
fi
if printf '%s' "$CFG_JSON" | grep -q '"web_tls"'; then
  BASE="https://$CFG_LISTEN"
else
  printf '面板没有启用 TLS（%s）：本脚本要求先按 P10.4 第一部分装出 HTTPS 面板\n' "$CFG_LISTEN" >&2
  exit 2
fi
ORIGIN=$BASE
PANEL_PORT=${CFG_LISTEN##*:}
note "面板：$BASE（端口 $PANEL_PORT）"

expect_eq "用安装的证书当 CA 取 health 返回 200" '200' \
  "$(panel -o /dev/null -w '%{http_code}' "$BASE/api/v1/health/live")"
expect_eq "nginx 正在运行" 'active' "$(systemctl is-active nginx)"
expect_eq "起点：没有面板的托管配置" '' "$(managed_conf)"
expect_eq "起点：nginx -t 通过" 'ok' "$(nginx_t_ok)"
if [ ! -d "$ACME_WEBROOT" ]; then
  install -d -m 0755 "$ACME_WEBROOT"
fi
expect_eq "ACME 校验目录存在" 'yes' "$([ -d "$ACME_WEBROOT" ] && echo yes || echo no)"

# ---------------------------------------------------------------- 1. 夹具
printf '\n--- 1. 网络与测试 ACME 夹具 ---\n'
cp /etc/hosts "$WORK/hosts.orig"
HOSTS_PATCHED=1
grep -qE "[[:space:]]$DOM_A([[:space:]]|$)" /etc/hosts || printf '127.0.0.1 %s\n' "$DOM_A" >>/etc/hosts
if getent hosts "$DOM_A" >/dev/null 2>&1; then
  pass "测试域名解析到本机（/etc/hosts，收尾还原）"
else
  fail "测试域名解析" "getent hosts $DOM_A 失败"
fi

if [ "$CERT_MODE" = "pebble" ] && [ -x "$PEBBLE_BIN" ] && [ -f "$PEBBLE_CONFIG" ]; then
  # Pebble 的管理/ACME 接口本身走 TLS，那对证书**必须事先存在**：Pebble 会自己生成
  # CA 链，但不会给自己生成 API 证书，缺了就直接退出：
  #   Calling ListenAndServeTLS() for admin interface: open .../certs/cert.pem: no such file or directory
  # 这是夹具的前置条件，不是产品行为——所以脚本自己补（与 24.04 上那份同形：自签、
  # CN=localhost、SAN 含 127.0.0.1），缺了就在这里生成并说明。
  if [ ! -s "$ACME_API_CERT" ] || [ ! -s "$ACME_API_KEY" ]; then
    mkdir -p "$(dirname "$ACME_API_CERT")"
    openssl req -x509 -newkey rsa:2048 -nodes \
      -keyout "$ACME_API_KEY" -out "$ACME_API_CERT" \
      -days 825 -subj /CN=localhost -addext subjectAltName=DNS:localhost,IP:127.0.0.1 \
      >/dev/null 2>&1 \
      && note "生成的测试 ACME 接口证书：$ACME_API_CERT（夹具前置条件，不是产品行为）"
  fi
  # 夹具必须**确定**：不沿用上一轮留下的 Pebble。
  #
  # 一开始这里是“已经在跑就直接用”，真机上因此红过一次：Pebble 把账号和 anti-replay
  # nonce 都放在内存里，certbot 却把账号 URL 缓存在 /etc/letsencrypt/accounts/<host:port>/。
  # 沿用一个不是本脚本起的实例，两边的状态就可能对不上——抓到过
  #   JWS has an invalid anti-replay nonce: -r8ZYUQifyJKd7XYG4oAiQ
  # （代理那边只报“certbot 退出码 1”，原因在 /var/log/letsencrypt/letsencrypt.log 里）。
  # 而且“是不是我起的”这种状态会让两轮验收走不同的分支，这不是夹具该有的不确定性。
  # 所以：有在跑的先停掉，一律起一个全新的，再清掉只属于测试 ACME 的账号登记。
  if curl -fsS --cacert "$ACME_API_CERT" "$ACME_API_URL" >/dev/null 2>&1; then
    note "上一轮留下的测试 ACME 还在跑：停掉它，改为起一个全新的实例（夹具确定性）"
    # 按进程名精确匹配（`-x pebble`）而不是只按路径：24.04 上抓到过上一轮是
    # `pebble -config /opt/p103/pebble/config.json`（走 PATH 起的），
    # `/usr/local/bin/pebble` 这个模式匹配不到它的 argv，pkill 于是“成功返回、什么都没杀”。
    pkill -x pebble 2>/dev/null
    pkill -f "$PEBBLE_BIN" 2>/dev/null
    for _ in $(seq 1 40); do
      curl -fsS --cacert "$ACME_API_CERT" "$ACME_API_URL" >/dev/null 2>&1 || break
      sleep 0.25
    done
    if curl -fsS --cacert "$ACME_API_CERT" "$ACME_API_URL" >/dev/null 2>&1; then
      fail "停掉上一轮的测试 ACME" "10 秒后 $ACME_API_URL 仍在响应：另一份 pebble 进程没被 pkill 带走（$(pgrep -a -f 'pebble -config' | tr '\n' ' ')）"
    fi
  fi
  # 这个 Pebble 会**随机拒绝 5% 的合法 nonce**（启动日志原话：
  # 「Configured to reject 5% of good nonces」），本意是逼实现方把重试写对。
  # 试过在派生配置里写 `"nonceErrPercent": 0` 把它关掉：**无效**——这一版把 0 当成“没配”，
  # 实测日志仍是 5%（2026-10-23 在 24.04 上验过）。所以不在夹具里硬掰，
  # 改为在第 6 节把“坏运气”与“产品坏”分开：只对 badNonce 重试，且每次记账。
  nohup "$PEBBLE_BIN" -config "$PEBBLE_CONFIG" >"$PEBBLE_LOG" 2>&1 &
  PEBBLE_PID=$!
  PEBBLE_STARTED=1
  _ok=0
  for _ in $(seq 1 60); do
    if curl -fsS --cacert "$ACME_API_CERT" "$ACME_API_URL" >/dev/null 2>&1; then _ok=1; break; fi
    sleep 0.5
  done
  if [ "$_ok" = "1" ]; then
    pass "Pebble 启动并提供 ACME 目录（全新实例）"
  else
    fail "Pebble 启动" "$(tail -n 5 "$PEBBLE_LOG" 2>/dev/null | tr '\n' ' ')"
  fi
  # 断言的是“这一次的随机拒绝比例就是 5%”：看 Pebble 自己打的配置行，不看我们传的参数。
  # 它提醒读日志的人：偶发一次 badNonce 是夹具的既定随机性，不是产品缺陷（第 6 节据此重试）。
  expect_contains "测试 ACME 的随机拒绝比例（5%，Pebble 的既定行为）" 'reject 5% of good nonces' \
    "$(head -n 20 "$PEBBLE_LOG" 2>/dev/null | tr '\n' ' ')"
  # 新实例的内存里没有任何账号，certbot 手里的旧登记必然失效，清掉让它在本次实例上重新注册。
  # 只删这个测试目录那一层；机器上真实 Let's Encrypt 的账号一个字都不动。
  _cb_host=${ACME_API_URL#https://}
  _cb_host=${_cb_host%%/*}
  if [ -d "/etc/letsencrypt/accounts/$_cb_host" ]; then
    rm -rf "/etc/letsencrypt/accounts/$_cb_host"
    note "清掉 certbot 里测试 ACME（$_cb_host）的旧账号登记：账号只活在 Pebble 内存里，重启即失效"
  fi
  expect_contains "测试 ACME 目录里没有生产地址" '127.0.0.1:14000' \
    "$(curl -fsS --cacert "$ACME_API_CERT" "$ACME_API_URL" 2>/dev/null | tr -d '\n')"
  expect_not_contains "…也没有 Let's Encrypt 正式环境" 'acme-v02.api.letsencrypt.org' \
    "$(curl -fsS --cacert "$ACME_API_CERT" "$ACME_API_URL" 2>/dev/null | tr -d '\n')"

  install -d -m 0755 "$CA_TRUST_DIR"
  cp "$ACME_API_CERT" "$CA_API_FILE"
  curl -fsS --cacert "$ACME_API_CERT" "$ACME_ROOT_URL" -o "$WORK/acme-root.pem" 2>/dev/null
  if [ -s "$WORK/acme-root.pem" ]; then
    cp "$WORK/acme-root.pem" "$CA_ROOT_FILE"
    pass "取到测试 ACME 的根证书"
  else
    fail "取到测试 ACME 的根证书" "GET $ACME_ROOT_URL 失败"
  fi
  CA_INSTALLED=1
  update-ca-certificates >/dev/null 2>&1
  if curl -fsS "$ACME_API_URL" >/dev/null 2>&1; then
    pass "系统信任库接受测试 ACME 的 API 证书（certbot 因此能连上）"
  else
    fail "系统信任库接受测试 ACME 的证书" "curl $ACME_API_URL 失败"
  fi
  CERTIFI_PATH=$(python3 -c 'import certifi;print(certifi.where())' 2>/dev/null)
  if [ "$CERTIFI_PATH" = "/etc/ssl/certs/ca-certificates.crt" ]; then
    pass "certbot 的 CA 包与系统信任库是同一个文件"
  else
    fail "certbot 的 CA 包与系统信任库不是同一个文件" "certifi → ${CERTIFI_PATH:-未安装}（夹具问题，不是产品问题）"
  fi

  # Pebble 的 HTTP-01 校验连 5002；本机 nginx 只听 80，用 nat 把这一跳引过去。
  iptables -t nat -C OUTPUT -p tcp -d 127.0.0.1 --dport 5002 -j REDIRECT --to-ports 80 2>/dev/null ||
    iptables -t nat -A OUTPUT -p tcp -d 127.0.0.1 --dport 5002 -j REDIRECT --to-ports 80
  iptables -t nat -C PREROUTING -p tcp -d 127.0.0.1 --dport 5002 -j REDIRECT --to-ports 80 2>/dev/null ||
    iptables -t nat -A PREROUTING -p tcp -d 127.0.0.1 --dport 5002 -j REDIRECT --to-ports 80
  NAT_ADDED=1
  pass "校验口重定向就位（5002 → 80，收尾删除）"

  # 让代理连本地测试 ACME：只加 certificates.staging_directory_url，其余一个字不动。
  cp -p "$SYS_CFG" "$WORK/upanel.json.orig"
  _CFG_UID=$(stat -c '%u' "$SYS_CFG")
  _CFG_GID=$(stat -c '%g' "$SYS_CFG")
  python3 - "$SYS_CFG" "$ACME_API_URL" "$_CFG_UID" "$_CFG_GID" <<'PY'
import json, os, sys
path, url, uid, gid = sys.argv[1], sys.argv[2], int(sys.argv[3]), int(sys.argv[4])
with open(path, encoding="utf-8") as fh:
    cfg = json.load(fh)
agent = cfg.setdefault("agent", {})
certs = agent.setdefault("certificates", {})
certs["staging_directory_url"] = url
tmp = path + ".p104tmp"
with open(tmp, "w", encoding="utf-8") as fh:
    json.dump(cfg, fh, ensure_ascii=False, indent=2)
    fh.write("\n")
os.chmod(tmp, 0o640)
os.chown(tmp, uid, gid)
os.replace(tmp, path)
PY
  CONFIG_PATCHED=1
  expect_eq "改过配置之后 check-config 仍然通过" '0' \
    "$("$CTL" check-config --config "$SYS_CFG" >/dev/null 2>&1; echo $?)"
  expect_contains "配置里的 staging 地址指向本地测试 ACME" "$ACME_API_URL" "$(cat "$SYS_CFG")"
  systemctl restart upanel-agent upanel-web
  _ok=0
  for _ in $(seq 1 60); do
    if [ "$(panel -o /dev/null -w '%{http_code}' "$BASE/api/v1/health/live")" = "200" ]; then _ok=1; break; fi
    sleep 0.5
  done
  if [ "$_ok" = "1" ]; then
    pass "改配置并重启之后面板恢复 200"
  else
    fail "改配置并重启之后面板恢复 200" "60 秒内没起来，见 journalctl -u upanel-web"
  fi
else
  notrun "证书签发与 443 场景" "本机没有 Pebble（PEBBLE_BIN=$PEBBLE_BIN，PEBBLE_CONFIG=$PEBBLE_CONFIG），CERT_MODE=$CERT_MODE"
fi

# ---------------------------------------------------------------- 2. 登录
printf '\n--- 2. 登录（HTTPS + Cookie + CSRF）---\n'
LOGIN_CODE=$(login "$JAR" "$PASSWORD")
expect_eq "管理员口令登录返回 200" '200' "$LOGIN_CODE"
CSRF=$(csrf_of)
if [ -n "$CSRF" ] && [ "${CSRF#ERR:}" = "$CSRF" ]; then
  pass "拿到 CSRF 令牌（后续写操作要用）"
else
  fail "拿到 CSRF 令牌" "登录响应：$(head -c 300 "$WORK/login.json")"
fi

# ---------------------------------------------------------------- 3. 上游与站点
printf '\n--- 3. 被测应用与站点（域名 → nginx → 上游）---\n'
python3 "$WS_UPSTREAM" --port "$WS_PORT" --marker "$MARKER" \
  --ticks "$WS_TICKS" --interval 0.5 --log "$UPSTREAM_LOG" &
UPSTREAM_PID=$!
_ok=0
for _ in $(seq 1 50); do
  if curl -s --max-time 2 "http://127.0.0.1:$WS_PORT/" | grep -q "$MARKER"; then _ok=1; break; fi
  sleep 0.1
done
if [ "$_ok" = "1" ]; then
  pass "上游应用已就绪（http://127.0.0.1:$WS_PORT 返回 marker）"
else
  fail "上游应用就绪" "$(tail -n 5 "$UPSTREAM_LOG" 2>/dev/null | tr '\n' ' ')"
fi

SITE_BODY="{\"domain\":\"$DOM_A\",\"upstream_target_id\":\"local_$WS_PORT\",\"upstream_port\":$WS_PORT,\"websocket_enabled\":true}"

# 起点：同名记录只可能是同一轮（同一个 RUN_TAG）中断留下的。按文档路径清掉，
# 而不是绕过那道 409——否则“重复登记 409”这条断言会被自己的残留触发，看起来像产品缺陷。
# 清掉之后重新登记，断言的仍然是“第一次登记”。
EXISTING_ID=$(api GET "$PROXIES?domain=$DOM_A" >/dev/null; \
  jget "$WORK/resp.json" 'd["data"]["items"][0]["id"] if d["data"]["items"] else ""')
if [ -n "$EXISTING_ID" ] && [ "${EXISTING_ID#ERR:}" = "$EXISTING_ID" ]; then
  note "发现上一轮留下的站点记录 $EXISTING_ID（上一轮中断在收尾之前），按文档路径移除"
  if remove_site "$EXISTING_ID" cleanup-leftover; then
    note "残留记录已按 enabled=false → apply → DELETE 移除"
  else
    fail "清掉上一轮的残留站点" "移除失败，见上面的 note"
  fi
fi

SITE_CODE=$(api POST "$PROXIES" "$SITE_BODY")
cp "$WORK/resp.json" "$PAYLOADS/site-create.json"
expect_eq "登记站点 $DOM_A" '201' "$SITE_CODE"
SITE_A=$(jget "$PAYLOADS/site-create.json" 'd["data"]["id"]')
if [ -n "$SITE_A" ]; then pass "站点标识：$SITE_A"; else fail "站点标识" "创建响应里没有 id"; fi
expect_eq "站点默认带 websocket_enabled" 'True' \
  "$(jget "$PAYLOADS/site-create.json" 'bool(d["data"]["websocket_enabled"])')"

run_action proxy.apply "{\"site_id\":\"$SITE_A\"}" apply-a
expect_eq "应用站点：预检放行" 'True' "$PRE_ALLOWED"
expect_eq "应用站点：作业终态 succeeded" 'succeeded' "$JOB_STATE"
expect_eq "应用站点：结果 verified" 'verified' "$(joutcome)"
CONF_A=$(conf_of "$DOM_A")
expect_eq "托管配置文件已写出" 'yes' "$([ -f "$CONF_A" ] && echo yes || echo no)"
expect_eq "脚本自己再跑一遍 nginx -t" 'ok' "$(nginx_t_ok)"
expect_contains "托管配置里带上了 Upgrade/Connection（WebSocket 那一对）" 'Upgrade' "$(cat "$CONF_A" 2>/dev/null)"
expect_not_contains "托管配置不监听面板端口" "listen $PANEL_PORT" "$(cat "$CONF_A" 2>/dev/null)"

HTTP_BODY=$(wait_body "$MARKER" 15 site_body)
expect_contains "域名访问到的是这个上游（marker 命中）" "$MARKER" "$HTTP_BODY"
expect_not_contains "…不是 nginx 的默认欢迎页" 'Welcome to nginx' "$HTTP_BODY"

# ---------------------------------------------------------------- 4. WebSocket 不断流
printf '\n--- 4. WebSocket 不断流（域名 → nginx → 上游推流）---\n'
WS_HTTP=$(ws_run ws-http "ws://$DOM_A/ws")
expect_eq "WS 握手返回 101（Upgrade 穿过了 nginx）" '101' "$(jget "$WS_HTTP" 'd["status"]')"
expect_eq "Sec-WebSocket-Accept 校验通过（是真的代理到上游）" 'True' "$(jget "$WS_HTTP" 'bool(d["accept_ok"])')"
expect_eq "收到的推流条数" "$WS_TICKS" "$(jget "$WS_HTTP" 'd["ticks_seen"]')"
expect_eq "客户端发出的文本帧被 echo 回来" 'echo:p104-ping' "$(jget "$WS_HTTP" 'd["echo"]')"
_ERR=$(jget "$WS_HTTP" 'd["error"]')
expect_eq "过程没有异常结束" 'None' "$_ERR"
MAX_GAP=$(jget "$WS_HTTP" 'd["max_gap_ms"]')
case "$MAX_GAP" in
  ''|*ERR*|None) fail "相邻帧间隔可读（不断流的量化判据）" "max_gap_ms=$MAX_GAP" ;;
  *)
    if [ "$MAX_GAP" -lt 1500 ]; then
      pass "相邻帧间隔没有异常拉长（max=${MAX_GAP}ms，节拍 500ms）"
    else
      fail "相邻帧间隔没有异常拉长（不断流）" "max_gap_ms=$MAX_GAP（节拍 500ms，阈值 1500ms）"
    fi
    ;;
esac
expect_eq "整段连接没被服务端提前关闭" 'False' "$(jget "$WS_HTTP" 'bool(d["closed_by_server"])')"

# ---------------------------------------------------------------- 5. 冲突拒绝
printf '\n--- 5. 同域名 / 端口冲突必须拒绝 ---\n'
DUP_CODE=$(api POST "$PROXIES" "$SITE_BODY")
cp "$WORK/resp.json" "$PAYLOADS/site-duplicate.json"
expect_eq "重复登记同一个域名返回 409" '409' "$DUP_CODE"
expect_contains "冲突说明点名域名" '域名' "$(jget "$PAYLOADS/site-duplicate.json" 'd["error"]["message"]')"

P_PANEL_CODE=$(api POST "$PROXIES" \
  "{\"domain\":\"p104port.example.test\",\"upstream_target_id\":\"local_$WS_PORT\",\"upstream_port\":$PANEL_PORT}")
cp "$WORK/resp.json" "$PAYLOADS/site-panelport.json"
expect_eq "上游端口填成面板自己的端口返回 400" '400' "$P_PANEL_CODE"
expect_contains "拒绝理由点明是面板端口" '面板' \
  "$(jget "$PAYLOADS/site-panelport.json" 'd["error"]["message"]')"
expect_contains "…并且给出了面板的端口号" "$PANEL_PORT" \
  "$(jget "$PAYLOADS/site-panelport.json" 'd["error"]["message"]')"

# ---------------------------------------------------------------- 6. 证书（staging）+ 443
printf '\n--- 6. staging 签发、绑定与 443 ---\n'
if [ "$CERT_MODE" != "pebble" ]; then
  notrun "staging 签发与 443" "CERT_MODE=$CERT_MODE（没有测试 ACME，签发段不执行）"
elif [ "$CONFIG_PATCHED" != "1" ]; then
  notrun "staging 签发与 443" "测试 ACME 没起来（见第 1 节）"
else
  # 签发这一步要把**外部 CA 的随机失败**和产品缺陷分开。
  #   · Pebble 故意随机拒绝 5% 的**合法** nonce（它自己启动日志里就写着
  #     「Configured to reject 5% of good nonces」；这一版不接受把比例设成 0，
  #     写进去也按默认 5% 处理，实测日志仍是 5%）；
  #   · certbot 2.9 的 acme 在取证书那条路径（`poll_finalization` → `_post_as_get`）上**不重试**，
  #     一次随机拒绝就把整个签发打成 failed（24.04 上抓到过，ACME 回 badNonce）。
  # 这是外部 CA 的既定行为，不是产品的问题：产品把 CA 的拒绝如实报出来是对的。
  # 所以脚本重试，但**只对 badNonce 重试**，最多 3 次，每次都记账并把失败那份作业另存；
  # 其它任何错误（比如真的签发不下来）不重试，照样红。
  # 幂等键每次不同：同一个键配同一份请求体会被服务端当成重放，拿回来的还是那份失败的作业。
  ISSUE_TRY=0
  while :; do
    ISSUE_TRY=$((ISSUE_TRY + 1))
    _suffix=issue-a
    [ "$ISSUE_TRY" -gt 1 ] && _suffix="issue-a-try$ISSUE_TRY"
    _JSTART=$(date '+%Y-%m-%d %H:%M:%S')
    run_action certificate.issue \
      "{\"domain\":\"$DOM_A\",\"email\":\"ops@example.test\",\"staging\":true}" "$_suffix"
    [ "$JOB_STATE" = "succeeded" ] && break
    if [ "$ISSUE_TRY" -lt 3 ] && issue_badnonce "$_JSTART"; then
      cp "$PAYLOADS/job-$_suffix.json" "$PAYLOADS/job-$_suffix-failed.json" 2>/dev/null
      note "第 $ISSUE_TRY 次签发被测试 ACME 的随机 nonce 拒绝（badNonce，Pebble 的既定行为）：重试" >&2
      continue
    fi
    break
  done
  note "签发尝试次数：$ISSUE_TRY"
  expect_eq "签发：预检放行" 'True' "$PRE_ALLOWED"
  expect_eq "签发：作业终态 succeeded" 'succeeded' "$JOB_STATE"
  expect_eq "签发：结果 verified" 'verified' "$(joutcome)"

  CERT_LIST=$PAYLOADS/certlist.json
  panel -b "$JAR" -o "$CERT_LIST" "$BASE$CERT_PATH?domain=$DOM_A"
  expect_eq "证书列表里出现这条记录" '1' "$(jget "$CERT_LIST" 'len(d["data"]["items"])')"
  CERT_ID=$(jget "$CERT_LIST" 'd["data"]["items"][0]["id"]')
  CERT_FP=$(jget "$CERT_LIST" 'd["data"]["items"][0]["fingerprint"]')
  expect_eq "记为 staging 签发（不是正式环境）" 'True' \
    "$(jget "$CERT_LIST" 'bool(d["data"]["items"][0]["staging"])')"
  expect_contains "签发者是测试环境（Pebble）" 'Pebble' \
    "$(jget "$CERT_LIST" 'd["data"]["items"][0]["issuer"]')"
  note "证书指纹：$CERT_FP"

  site_get "$SITE_A"
  PATCH_BODY="{\"version\":$(sget '["version"]'),\"certificate_id\":\"$CERT_ID\"}"
  PATCH_CODE=$(api PATCH "$PROXIES/$SITE_A" "$PATCH_BODY")
  expect_eq "把证书绑定到站点" '200' "$PATCH_CODE"
  site_get "$SITE_A"
  expect_eq "站点记录的证书标识" "$CERT_ID" "$(sget '["certificate_id"]')"

  run_action proxy.apply "{\"site_id\":\"$SITE_A\"}" apply-a-tls
  expect_eq "带证书应用：作业终态 succeeded" 'succeeded' "$JOB_STATE"
  # 这次应用**必须真的 reload 过**：配置写对了但没人 reload，等于没生效。
  # 以前这两个字段没人看，于是一次“单元状态读不到 → 跳过 reload”的缺陷要等到
  # 443 拒绝连接、wss 失败、重启后失败一连串红才暴露（DECISIONS Q309）。
  # 断言放在最靠近根因的地方：这条红会直接点名 reload，而不是让人从 16 条下游里倒推。
  expect_eq "带证书应用：这次确实 reload 了" 'done' "$(jobdata reload)"
  expect_eq "带证书应用：served=true" 'True' "$(jobdata served)"
  expect_contains "托管配置开始监听 443" 'listen 443' "$(cat "$CONF_A" 2>/dev/null)"
  # 绑了证书之后 80 的行为必须被固定下来：只留 ACME 校验位置，其余 301 到 https。
  # 后面第 7、8 节的“站点仍然服务”都按这个行为取正文（site_body 跟跳转）。
  #
  # 这一条**必须等收敛**，不能只问一次：`nginx -s reload` 返回 0 只说明新配置被接受并派给
  # master，新 worker 接管连接是之后的事（交班窗口 P10.4 实测 6—111ms，Q308）。reload 之后
  # 立刻问一次 301，问到的可能是还没交班的旧 worker —— 那是夹具的时间差，不是产品没生效。
  # 22.04 上真机上红过一次：443、wss、重启后全部通过，只有这一条拿回了旧配置的 200。
  expect_eq "绑了证书之后 80 是 301 到 https" '301' \
    "$(wait_code 301 10 -H "Host: $DOM_A" http://127.0.0.1/)"
  expect_contains "301 指向同一个域名的 https" "https://$DOM_A" \
    "$(curl -sSI --max-time 10 -H "Host: $DOM_A" http://127.0.0.1/ 2>/dev/null | tr -d '\r')"
  # http-01 续期走的是 http://<域名>/.well-known/acme-challenge/…：如果 301 排在
  # 校验位置前面，续期那一刻就被跳到 https，而 https 上还是旧证书 —— 死锁。
  # 所以这里比的是行号，不是“文件里有没有这个词”。
  ACME_LINE=$(grep -n 'acme-challenge' "$CONF_A" 2>/dev/null | head -1 | cut -d: -f1)
  REDIR_LINE=$(grep -n 'return 301' "$CONF_A" 2>/dev/null | head -1 | cut -d: -f1)
  if [ -n "$ACME_LINE" ] && [ -n "$REDIR_LINE" ] && [ "$ACME_LINE" -lt "$REDIR_LINE" ]; then
    pass "80 上的 ACME 校验位置排在 301 之前（行 $ACME_LINE < $REDIR_LINE）"
  else
    fail "80 上的 ACME 校验位置排在 301 之前" "acme=$ACME_LINE redirect=$REDIR_LINE"
  fi

  HTTPS_BODY=$(wait_body "$MARKER" 15 site_body_tls)
  expect_contains "443 用系统信任链取到 marker（不给 -k）" "$MARKER" "$HTTPS_BODY"

  SERVED_FP=$(echo | openssl s_client -connect "$DOM_A:443" -servername "$DOM_A" 2>/dev/null \
    | openssl x509 -noout -fingerprint -sha256 2>/dev/null | sed 's/^.*=//; s/://g' | tr 'A-F' 'a-f')
  expect_eq "443 上服务的证书指纹与账本记录一致" \
    "$(printf '%s' "$CERT_FP" | tr 'A-F' 'a-f')" "$SERVED_FP"

  # wss 的 TLS 用**显式给的根**（Pebble 的根），不吃系统信任库：
  #   22.04 这台机器上 `ssl.create_default_context()` 一个信任锚都没有（实测
  #   `len(get_ca_certs())==0`；`load_verify_locations(capath="/etc/ssl/certs")` 也是 0，
  #   而 24.04 上同一个调用是 120）—— 那是这台镜像的 OpenSSL/capath 行为，不是产品的问题。
  #   “系统信任链可用”这一条已经由上面那条 curl 断言证明（curl 用同一个 bundle，能验通）；
  #   WebSocket 这一段要证明的是**穿过 nginx 的推流不断**，所以把根明确交给客户端，
  #   而不是让 TLS 结论取决于哪台机器的 python 怎么读 ca 目录。
  WS_TLS=$(ws_run ws-tls "wss://$DOM_A/ws" --ca "$WORK/acme-root.pem")
  expect_eq "wss 握手返回 101" '101' "$(jget "$WS_TLS" 'd["status"]')"
  expect_eq "wss 收到的推流条数" "$WS_TICKS" "$(jget "$WS_TLS" 'd["ticks_seen"]')"
  expect_eq "wss 的 echo 回来" 'echo:p104-ping' "$(jget "$WS_TLS" 'd["echo"]')"
  expect_eq "wss 过程没有异常结束" 'None' "$(jget "$WS_TLS" 'd["error"]')"
  TLS_VER=$(jget "$WS_TLS" 'd["tls"]["version"]')
  expect_contains "wss 走的是 TLS 1.2 及以上" 'TLSv1.' "$TLS_VER"
  expect_eq "wss 对端证书指纹与账本一致" \
    "$(printf '%s' "$CERT_FP" | tr 'A-F' 'a-f')" "$(jget "$WS_TLS" 'd["tls"]["peer_fingerprint_sha256"]')"

  # 私钥不可读：upanel-web 账号（不是 root）读不到 certbot 的 privkey，接口/库/日志里也没有私钥内容。
  if runuser -u "$WEB_USER" -- test -r "$LIVE_DIR/$DOM_A/privkey.pem"; then
    fail "upanel-web 读不到站点私钥（$LIVE_DIR/$DOM_A/privkey.pem）" '竟然可读'
  else
    pass "upanel-web 读不到站点私钥（certbot 目录 0700 root）"
  fi
  if runuser -u "$WEB_USER" -- test -r "$NGINX_CONF_D/upanel-$DOM_A.conf"; then
    pass "upanel-web 读得到托管配置（nginx 自己需要读）"
  else
    fail "upanel-web 读得到托管配置" '读不到'
  fi
  # 托管配置里**必然**有私钥路径：那是 nginx 的 ssl_certificate_key，不是泄漏。
  # 早先这里断言“配置里不出现 privkey”，等于要求 nginx 不加载私钥 —— 判据本身错了。
  # 该断言的是两件真事：① 配置里没有私钥内容（材料级泄漏）；② 配置里写的那个路径
  # 面板账号读不到（只有 root 与 nginx master 能读）。
  expect_not_contains "托管配置里没有私钥内容" 'PRIVATE KEY' "$(cat "$CONF_A" 2>/dev/null)"
  CONF_KEYPATH=$(awk '/^[[:space:]]*ssl_certificate_key[[:space:]]/{print $2; exit}' \
    "$CONF_A" 2>/dev/null | tr -d ';')
  expect_eq "配置里给 nginx 的私钥路径就是账本里的那条" \
    "$LIVE_DIR/$DOM_A/privkey.pem" "$CONF_KEYPATH"
  if [ -n "$CONF_KEYPATH" ] && runuser -u "$WEB_USER" -- test -r "$CONF_KEYPATH"; then
    fail "配置里写的私钥路径面板账号读不到" "竟然可读：$CONF_KEYPATH"
  else
    pass "配置里写的私钥路径面板账号读不到（$CONF_KEYPATH）"
  fi
  DETAIL=$PAYLOADS/cert-detail.json
  panel -b "$JAR" -o "$DETAIL" "$BASE$CERT_PATH/$CERT_ID"
  expect_not_contains "证书详情响应里没有私钥内容" 'PRIVATE KEY' "$(cat "$DETAIL" 2>/dev/null)"
  expect_not_contains "证书详情响应里没有私钥路径" 'privkey' "$(cat "$DETAIL" 2>/dev/null)"
  WEB_DB=/var/lib/upanel/upanel.db
  if [ -f "$WEB_DB" ]; then
    _leak=$(python3 - "$WEB_DB" <<'PY' 2>/dev/null
import sqlite3, sys
con = sqlite3.connect(sys.argv[1])
hits = 0
for (name,) in con.execute("SELECT name FROM sqlite_master WHERE type='table'"):
    try:
        rows = con.execute('SELECT * FROM "%s"' % name).fetchall()
    except Exception:
        continue
    for row in rows:
        for cell in row:
            if isinstance(cell, (str, bytes)) and b"PRIVATE KEY" in (cell.encode() if isinstance(cell, str) else cell):
                hits += 1
print(hits)
PY
)
    expect_eq "面板库里搜不到私钥内容" '0' "$_leak"
  else
    notrun "面板库里搜不到私钥内容" "$WEB_DB 不存在（面板库路径与预期不同）"
  fi
  if grep -rq 'PRIVATE KEY' /var/log/upanel 2>/dev/null; then
    fail "面板日志里搜不到私钥内容" '发现 PRIVATE KEY'
  else
    pass "面板日志里搜不到私钥内容"
  fi
fi

# ---------------------------------------------------------------- 7. 坏配置无害
printf '\n--- 7. 错误 Nginx 配置：不破坏已有站点，面板照常登录 ---\n'
cat >"$FIX_INVALID" <<'CONF'
server {
  listen 8098;
  server_name p104-invalid.example.test;
  p104_no_such_directive on;
}
CONF
chmod 0644 "$FIX_INVALID"
expect_eq "夹具（不认识的指令）确实让 nginx -t 失败" 'fail' "$(nginx_t_ok)"
HASH_GOOD=$(file_hash "$CONF_A")
WORKERS_GOOD=$(nginx_workers)

expect_eq "nginx 配置坏着的时候面板 health 仍然 200" '200' \
  "$(panel -o /dev/null -w '%{http_code}' "$BASE/api/v1/health/live")"
expect_eq "…而且仍然能登录" '200' "$(login "$WORK/broken.jar" "$PASSWORD")"
note "登录成功即证明：管理端不经过 nginx，配置写坏的最坏结果是站点不可用、面板照常"

site_get "$SITE_A"
_VER=$(sget '["version"]')
run_action proxy.apply "{\"site_id\":\"$SITE_A\"}" apply-broken-tree
if [ "$JOB_STATE" = "failed" ]; then
  pass "坏配置在场时应用失败（不是 succeeded）"
  expect_contains "失败理由点明是 nginx -t" 'nginx -t' "$(jerrmsg)"
  # nginx 自己的原话必须出现：作业说明里只有“校验失败：”而没有原因，等于让人去猜。
  expect_contains "…并且带回了 nginx 自己说的话" 'nginx:' "$(jerrmsg)"
else
  fail "坏配置在场时应用失败" "终态 $JOB_STATE（预检 $PRE_CODE，放行 $PRE_ALLOWED）"
fi
expect_eq "托管文件逐字节回到旧内容（回滚生效）" "$HASH_GOOD" "$(file_hash "$CONF_A")"
expect_eq "worker 没有换（失败时绝不 reload）" "$WORKERS_GOOD" "$(nginx_workers)"
HTTP_STILL=$(wait_body "$MARKER" 15 site_body)
expect_contains "旧站点仍在用旧配置提供服务" "$MARKER" "$HTTP_STILL"

rm -f "$FIX_INVALID"
expect_eq "删掉坏文件后 nginx -t 恢复" 'ok' "$(nginx_t_ok)"
run_action proxy.apply "{\"site_id\":\"$SITE_A\"}" apply-recovered
# 这一步的内容没变（版本、字段都一样），所以终态可能是 succeeded 且 file_state=unchanged。
case "$JOB_STATE" in
  succeeded)
    pass "恢复之后同一条记录再应用成功"
    ;;
  failed)
    fail "恢复之后同一条记录再应用成功" "$(jerrmsg)"
    ;;
  *)
    fail "恢复之后同一条记录再应用成功" "终态 $JOB_STATE"
    ;;
esac
expect_eq "恢复之后 nginx -t 仍然通过" 'ok' "$(nginx_t_ok)"
expect_contains "恢复之后站点照常服务" "$MARKER" "$(wait_body "$MARKER" 15 site_body)"

# ---------------------------------------------------------------- 8. 重启
printf '\n--- 8. 重启两个单元之后核心场景仍然成立 ---\n'
systemctl restart upanel-agent upanel-web
_ok=0
for _ in $(seq 1 60); do
  if [ "$(panel -o /dev/null -w '%{http_code}' "$BASE/api/v1/health/live")" = "200" ]; then _ok=1; break; fi
  sleep 0.5
done
expect_eq "重启后面板 health 200" '1' "$_ok"
expect_eq "重启后代理 socket 就位" 'yes' "$([ -S /run/upanel/agent.sock ] && echo yes || echo no)"
expect_eq "重启后可以用口令登录" '200' "$(login "$JAR" "$PASSWORD")"
expect_contains "重启后 HTTP 站点仍然服务" "$MARKER" "$(wait_body "$MARKER" 15 site_body)"
if [ -f "$NGINX_CONF_D/upanel-$DOM_A.conf" ] && grep -q 'listen 443' "$NGINX_CONF_D/upanel-$DOM_A.conf"; then
  expect_contains "重启后 443 仍然服务" "$MARKER" "$(wait_body "$MARKER" 15 site_body_tls)"
  WS_AFTER=$(ws_run ws-after-restart "wss://$DOM_A/ws" --ca "$WORK/acme-root.pem")
  expect_eq "重启后 wss 仍然 12 条不断流" "$WS_TICKS" "$(jget "$WS_AFTER" 'd["ticks_seen"]')"
else
  notrun "重启后 443 / wss" "该站点没有绑定证书（第 6 节未执行）"
fi

# ---------------------------------------------------------------- 9. 收尾（显式，便于证据阅读）
printf '\n--- 9. 收尾 ---\n'
if [ -n "${SITE_A:-}" ]; then
  if remove_site "$SITE_A" teardown; then
    pass "收尾：站点按 enabled=false → apply → DELETE 移除"
  else
    fail "收尾：站点按文档路径移除" "见上面的 note（残留会被下一轮的起点清理接住）"
  fi
  expect_eq "移除之后托管配置不在了" 'no' "$([ -f "$CONF_A" ] && echo yes || echo no)"
  expect_eq "移除之后站点记录也查不到了" '404' "$(api GET "$PROXIES/$SITE_A")"
fi
# 兜底：前面几节的夹具文件（p104-broken.conf / p104-invalid.conf）各节自己删，这里只清
# 可能残留的托管配置。上面两条断言已经证明正常的移除路径把文件删掉了，这条 rm 不遮任何东西。
rm -f "$NGINX_CONF_D"/upanel-*.conf
rm -rf "$NGINX_AUTH_D"
expect_eq "删除托管配置之后 nginx -t 通过" 'ok' "$(nginx_t_ok)"
systemctl reload nginx >/dev/null 2>&1
if [ "$(systemctl is-active nginx)" = "active" ]; then
  pass "nginx 在收尾之后仍然 active"
else
  fail "nginx 在收尾之后仍然 active" "$(systemctl is-active nginx)"
fi
expect_eq "收尾后面板仍然 200（站点删除不影响管理端）" '200' \
  "$(panel -o /dev/null -w '%{http_code}' "$BASE/api/v1/health/live")"

printf '\n==== P10.4 核心场景（第二部分）汇总 ====\n'
printf 'PASS=%d FAIL=%d NOT RUN=%d\n' "$PASSED" "$FAILED" "$NOTRUN"
if [ -n "$FAILED_NAMES" ]; then
  printf '失败项：%s\n' "$FAILED_NAMES"
fi
printf '日志：%s\n' "$LOGS"
if [ "$FAILED" -gt 0 ]; then
  exit 1
fi
exit 0
