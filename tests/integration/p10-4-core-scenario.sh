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
# PEBBLE_CONFIG（默认 /opt/p103/pebble/config.json）、CERT_MODE（pebble|skip，默认 pebble）。
#
# 退出码：0 全部通过；1 有失败项；2 环境不满足。
#
# 对这台机器的改动（收尾逐个撤销）：
#   - /etc/hosts 加两条测试域名（备份 → 还原）；
#   - /etc/upanel/upanel.json 临时加 agent.certificates.staging_directory_url
#     （原文件备份 → 还原 → restart）；
#   - 系统信任库临时加 Pebble 的 API 证书与根证书（删掉并 update-ca-certificates）；
#   - iptables nat 把 Pebble 的校验口 5002 引到 80（删掉那条规则）；
#   - /etc/nginx/conf.d/upanel-*.conf、/etc/ssl/upanel、/etc/letsencrypt/live/p104*
#     下的验收产物（删掉）；
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
ACME_ROOT_URL=https://127.0.0.1:15000/roots/0
CA_TRUST_DIR=/usr/local/share/ca-certificates
CA_API_FILE=$CA_TRUST_DIR/p104-acme-api.crt
CA_ROOT_FILE=$CA_TRUST_DIR/p104-acme-root.crt
FIX_BROKEN=$NGINX_CONF_D/p104-broken.conf
FIX_INVALID=$NGINX_CONF_D/p104-invalid.conf

DOM_A=p104a.example.test
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

# panel <curl 参数...>：所有面板请求都带上安装的那张证书（不加 -k）。
panel() { curl -sS --cacert "$PANEL_CERT" --max-time 20 "$@"; }

login() {
  _jar=$1; _pw=$2
  panel -c "$_jar" -o "$WORK/login.json" -w '%{http_code}' -X POST "$BASE/api/v1/auth/login" \
    -H 'Content-Type: application/json' -H "Origin: $ORIGIN" -H 'X-UPanel-Request: 1' \
    -d "{\"username\":\"admin\",\"password\":\"$_pw\"}"
}
csrf_of() { jget "$WORK/login.json" 'd["data"]["csrf_token"]'; }

# api <方法> <路径> [body]：响应体落在 $WORK/resp.json，回显状态码。
api() {
  _method=$1; _path=$2; _body=${3:-}
  if [ -n "$_body" ]; then
    printf '%s' "$_body" >"$WORK/req.json"
    panel -b "$JAR" -o "$WORK/resp.json" -w '%{http_code}' -X "$_method" "$BASE$_path" \
      -H 'Content-Type: application/json' -H "Origin: $ORIGIN" -H "X-CSRF-Token: $CSRF" \
      -d @"$WORK/req.json"
  else
    panel -b "$JAR" -o "$WORK/resp.json" -w '%{http_code}' -X "$_method" "$BASE$_path" \
      -H "Origin: $ORIGIN" -H "X-CSRF-Token: $CSRF"
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
      -H 'Content-Type: application/json' -H "Origin: $ORIGIN" -H "X-CSRF-Token: $CSRF" \
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

submit_job() {
  python3 -c 'import json,sys
print(json.dumps({"preflight_id": sys.argv[1]}))' "$1" >"$WORK/jobreq.json"
  panel -b "$JAR" -o "$WORK/resp.json" -w '%{http_code}' -X POST "$BASE$JOBS" \
    -H 'Content-Type: application/json' -H "Origin: $ORIGIN" -H "X-CSRF-Token: $CSRF" \
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
  _jc=$(submit_job "$_pid" "p104-$3")
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
for tool in curl openssl python3 nginx systemctl iptables; do
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
  if curl -fsS --cacert "$ACME_API_CERT" "$ACME_API_URL" >/dev/null 2>&1; then
    pass "测试 ACME（Pebble）已在运行"
  else
    nohup "$PEBBLE_BIN" -config "$PEBBLE_CONFIG" >"$PEBBLE_LOG" 2>&1 &
    PEBBLE_PID=$!
    PEBBLE_STARTED=1
    _ok=0
    for _ in $(seq 1 60); do
      if curl -fsS --cacert "$ACME_API_CERT" "$ACME_API_URL" >/dev/null 2>&1; then _ok=1; break; fi
      sleep 0.5
    done
    if [ "$_ok" = "1" ]; then
      pass "Pebble 启动并提供 ACME 目录"
    else
      fail "Pebble 启动" "$(tail -n 5 "$PEBBLE_LOG" 2>/dev/null | tr '\n' ' ')"
    fi
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

HTTP_BODY=$(curl -sS --max-time 10 -H "Host: $DOM_A" http://127.0.0.1/ 2>/dev/null)
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
  run_action certificate.issue \
    "{\"domain\":\"$DOM_A\",\"email\":\"ops@example.test\",\"staging\":true}" issue-a
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
  expect_contains "托管配置开始监听 443" 'listen 443' "$(cat "$CONF_A" 2>/dev/null)"

  HTTPS_BODY=$(curl -sS --max-time 10 "https://$DOM_A/" 2>/dev/null)
  expect_contains "443 用系统信任链取到 marker（不给 -k）" "$MARKER" "$HTTPS_BODY"

  SERVED_FP=$(echo | openssl s_client -connect "$DOM_A:443" -servername "$DOM_A" 2>/dev/null \
    | openssl x509 -noout -fingerprint -sha256 2>/dev/null | sed 's/^.*=//; s/://g' | tr 'A-F' 'a-f')
  expect_eq "443 上服务的证书指纹与账本记录一致" \
    "$(printf '%s' "$CERT_FP" | tr 'A-F' 'a-f')" "$SERVED_FP"

  WS_TLS=$(ws_run ws-tls "wss://$DOM_A/ws")
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
  expect_not_contains "托管配置里没有私钥路径" 'privkey' "$(cat "$CONF_A" 2>/dev/null)"
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
HTTP_STILL=$(curl -sS --max-time 10 -H "Host: $DOM_A" http://127.0.0.1/ 2>/dev/null)
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
expect_contains "恢复之后站点照常服务" "$MARKER" \
  "$(curl -sS --max-time 10 -H "Host: $DOM_A" http://127.0.0.1/ 2>/dev/null)"

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
expect_contains "重启后 HTTP 站点仍然服务" "$MARKER" \
  "$(curl -sS --max-time 10 -H "Host: $DOM_A" http://127.0.0.1/ 2>/dev/null)"
if [ -f "$NGINX_CONF_D/upanel-$DOM_A.conf" ] && grep -q 'listen 443' "$NGINX_CONF_D/upanel-$DOM_A.conf"; then
  expect_contains "重启后 443 仍然服务" "$MARKER" \
    "$(curl -sS --max-time 10 "https://$DOM_A/" 2>/dev/null)"
  WS_AFTER=$(ws_run ws-after-restart "wss://$DOM_A/ws")
  expect_eq "重启后 wss 仍然 12 条不断流" "$WS_TICKS" "$(jget "$WS_AFTER" 'd["ticks_seen"]')"
else
  notrun "重启后 443 / wss" "该站点没有绑定证书（第 6 节未执行）"
fi

# ---------------------------------------------------------------- 9. 收尾（显式，便于证据阅读）
printf '\n--- 9. 收尾 ---\n'
if [ -n "${SITE_A:-}" ]; then
  api DELETE "$PROXIES/$SITE_A" >/dev/null 2>&1
fi
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
