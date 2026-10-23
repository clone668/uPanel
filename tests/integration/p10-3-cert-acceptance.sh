#!/bin/sh
# P10.3 Linux 验收脚本：Certbot 签发（staging）、正式签发的显式选择、手动导入、
# 到期信息、私钥保护与续期动作。
#
# 这一份要证明的不是“任务返回 succeeded”，而是下面这些“跑完了也说明不了”的事实：
#
#   1) **签发走完了真实的 ACME 流程**：本机起一个 Pebble（Let's Encrypt 的测试
#      实现），certbot 通过 HTTP-01 校验拿到证书。校验路径是本机真实的 nginx 提供的
#      ——脚本自己先按 Pebble 会用的那条路（域名 → 5002 端口 → nginx → webroot）
#      取回一个自己写下的文件，再让 certbot 走一遍。
#   2) **staging 是默认，正式要显式选**：缺 staging 的请求被面板拒绝（400），
#      staging=true 时签发出来的证书签发者里有 Pebble（测试环境），而“正式”这个
#      选择的地址来自 root 独占的配置，不接受 HTTP 参数。
#   3) **私钥不进面板**：certbot 的私钥留在 /etc/letsencrypt（0600）；手动导入的
#      私钥只经 POST /secrets 进代理的加密库，由代理物化成 0600 root 的文件。
#      uid 999（upanel-web）读不到任何一个；所有接口响应、面板库、审计、日志里
#      都搜不到私钥内容或私钥路径。
#   4) **绑定之后 443 真的在服务**：证书装到站点上、应用之后，绕过 Host 头直接
#      用域名 + 系统信任链（不是 -k）取到上游的页面；再用 s_client 校对 serving
#      的那张证书的指纹与库里这条记录一致。
#   5) **到期信息是算出来的**：20 天有效期的证书报 expiring，正常报 valid，
#      已经过期的证书在预检就被挡下（不是“装上去让浏览器报警”）。
#   6) **续期有动作、有限流**：同一域名一小时内再次尝试被拒（429，依据是任务表，
#      失败的尝试同样算一次）；把时间戳前移之后再续期，记录标识不变、站点绑定不变、
#      指纹变了，443 立刻服务新证书。
#
# 用法（root）：
#   sudo BIN_DIR=/var/tmp/upanel-bin sh tests/integration/p10-3-cert-acceptance.sh
#
# 环境变量：PORT（面板，默认 19498）、UPSTREAM_PORT（默认 18081）、WORK、BIN_DIR、
# PEBBLE_BIN（默认 /usr/local/bin/pebble）、PEBBLE_CONFIG（默认 /opt/p103/pebble/config.json）。
#
# 退出码：0 全部通过；1 有失败项；2 环境不满足。
#
# 对这台机器的改动（都是**验收夹具**，收尾时逐个撤销）：
#   - /etc/hosts 里加三条测试域名（备份 → 还原）；
#   - iptables nat：5002 → 80（Pebble 的 HTTP-01 校验口，让校验落到本机 nginx）；
#   - 系统信任库里加两张 CA：Pebble 的 API 证书（certbot 要连它）与 Pebble 的
#     根证书（curl 要信任它签出来的站点证书）。收尾时删掉并重建信任库；
#   - /etc/nginx/conf.d/upanel-*.conf 与 /etc/ssl/upanel、/etc/letsencrypt/live
#     下的测试域名产物；
#   - 起/停 upanel-agent、upanel-web、Pebble。
#
# 明确不覆盖（NOT RUN，原因写在这里，不在脚本里假装通过）：
#   - **真实 Let's Encrypt**：会消耗速率限额，且需要公网域名与 80 端口。本脚本
#     只连本地 Pebble；生产地址那一条由配置默认值 + 面板的“必须显式选 staging”
#     两条共同保证，不在这里签发。
#   - **真实公网 DNS 与外部可达性**：用 /etc/hosts + nat 重定向代替。
#   - **浏览器渲染与告警**：VM 上没有浏览器；staging 证书在浏览器里“不受信任”的
#     表现不做验证。
#   - **timer 自动续期**：P12 才接入。本脚本验的是“续期这个动作成立”。
#   - **ACME 的 DNS-01 / 通配符**：产品只做 HTTP-01，不支持通配符域名。
#   - **证书吊销与 OCSP**：不在 P10.3 范围。

set -u

BIN_DIR=${BIN_DIR:-/var/tmp/upanel-bin}
WORK=${WORK:-/var/tmp/upanel-accept-p10-3}
PEBBLE_BIN=${PEBBLE_BIN:-/usr/local/bin/pebble}
PEBBLE_CONFIG=${PEBBLE_CONFIG:-/opt/p103/pebble/config.json}
PEBBLE_LOG=$WORK/logs/pebble.log
PAYLOADS=$WORK/payloads
SECRETS_DIR=$WORK/secrets
UPSTREAM_DIR=$WORK/upstream
CERTS_DIR=$WORK/certs
RUN_DIR=/run/upanel
SOCK=$RUN_DIR/agent.sock
SYS_CFG=/etc/upanel/upanel.json
AGENT_DB_DIR=/var/lib/upanel-agent
KEY_FILE=/etc/upanel/agent.key
WEB_DB_DIR=$WORK/webdata
WEB_DB=$WEB_DB_DIR/upanel.db
PORT=${PORT:-19498}
UPSTREAM_PORT=${UPSTREAM_PORT:-18081}
BASE="http://127.0.0.1:$PORT"
ORIGIN="$BASE"
WEB_USER=upanel-web
WEB_UID_EXPECTED=999
PASSWORD='vm-correct-horse-9'

CERT_PATH=/api/v1/certificates
UPLOADS_PATH=/api/v1/certificates/uploads
PREFLIGHT_PATH=/api/v1/operations/preflight
JOBS=/api/v1/jobs
SECRETS_PATH=/api/v1/secrets

NGINX_CONF_D=/etc/nginx/conf.d
ACME_WEBROOT=/var/www/upanel-acme
MANUAL_CERT_DIR=/etc/ssl/upanel
LIVE_DIR=/etc/letsencrypt/live
ACME_API_URL=https://127.0.0.1:14000/dir
ACME_ROOT_URL=https://127.0.0.1:15000/roots/0
ACME_API_CERT=/opt/p103/pebble/certs/cert.pem

CA_TRUST_DIR=/usr/local/share/ca-certificates
CA_API_FILE=$CA_TRUST_DIR/p103-acme-api.crt
CA_ROOT_FILE=$CA_TRUST_DIR/p103-acme-root.crt

DOM_ISSUE=p103a.example.test
DOM_IMPORT=p103b.example.test
DOM_EXPIRING=p103c.example.test
DOM_DEAD=p103d.example.test
MARKER='p103-upstream-marker'

AGENT_LOG=$WORK/logs/agent.log
WEB_LOG=$WORK/logs/web.log
UPSTREAM_LOG=$WORK/logs/upstream.log
AGENT_PID=""
WEB_PID=""
UPSTREAM_PID=""
PEBBLE_PID=""

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
CERTBOT_BIN=$(command -v certbot 2>/dev/null || printf '/usr/bin/certbot')
for item in "$NGINX_BIN" "$CERTBOT_BIN" "$PEBBLE_BIN"; do
  if [ ! -x "$item" ]; then
    printf '缺少依赖：%s\n' "$item" >&2
    exit 2
  fi
done
if [ ! -f "$PEBBLE_CONFIG" ]; then
  printf '缺少 Pebble 配置：%s\n' "$PEBBLE_CONFIG" >&2
  exit 2
fi
if ! command -v python3 >/dev/null 2>&1 || ! command -v openssl >/dev/null 2>&1; then
  printf '缺少 python3 或 openssl\n' >&2
  exit 2
fi
if ! command -v iptables >/dev/null 2>&1; then
  printf '缺少 iptables（用来把 Pebble 的校验口引到 nginx）\n' >&2
  exit 2
fi

# jget <文件> <python 表达式，d 为解析后的 JSON>
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

# dbq <只读查询>：直接读面板库（先 checkpoint，避免读到旧 WAL 快照）。
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

# dbexec <写语句>：验收夹具用。只用于“把时间前移”这种让限流可测的操作。
dbexec() {
  python3 - "$WEB_DB" "$1" <<'PY' 2>/dev/null
import sqlite3, sys
con = sqlite3.connect(sys.argv[1])
con.execute(sys.argv[2])
con.commit()
con.close()
PY
}

# db_search <表名> <列清单> <关键字>：在面板库里全表搜关键字，命中就打印表名。
#
# 私钥保护的证据是“搜不到”，所以这个函数必须真的扫过内容，而不是查一下列名。
db_search() {
  python3 - "$WEB_DB" "$1" "$2" "$3" <<'PY' 2>/dev/null
import sqlite3, sys
db, table, cols, needle = sys.argv[1], sys.argv[2], sys.argv[3], sys.argv[4]
con = sqlite3.connect(db)
try:
    rows = con.execute("SELECT %s FROM %s" % (cols, table)).fetchall()
except Exception as exc:
    # 表名/列名写错会走到这里。**不能让“查不到”冒充“没搜到”**——那正是这条
    # 断言最容易被骗过去的方式，所以把错误打出来，让调用方判失败。
    print("ERR:%s" % exc)
    raise SystemExit
hits = 0
for row in rows:
    for value in row:
        if isinstance(value, str) and needle in value:
            hits += 1
if hits:
    print("%s:%d" % (table, hits))
con.close()
PY
}

# loggrep <文件> <关键字>：在日志里搜关键字，回显命中行数（没有命中就是 0）。
#
# 注意 `grep -c` 命中 0 行时**退出码是 1**，所以不能写 `grep -c ... || printf 0`：
# 那会把 "0" 打印两遍，回显变成两行的 "0"，任何与 "0" 的比较都会失败，而日志其实很干净。
# 第一遍运行就是被这个坑骗成“日志里有私钥”的。
loggrep() {
  if [ ! -f "$1" ]; then printf '0'; return 0; fi
  _n=$(grep -c -- "$2" "$1" 2>/dev/null)
  [ -z "$_n" ] && _n=0
  printf '%s' "$_n"
}

# bodygrep <目录> <关键字>：在所有保存下来的响应体里搜关键字，回显命中的文件数。
bodygrep() {
  grep -rl -- "$2" "$1" 2>/dev/null | wc -l | tr -d ' '
}

# cert_body_leak <关键字>：只在**证书接口**的响应体里搜。
#
# 范围是有意的：任务结果与预检影响里会出现落地路径（“写到哪个文件”是运维要知道的事，
# CONTRACTS §9.3(7)），但证书 DTO 里一个路径都不能有。把两者混在一个 glob 里，
# 要么会漏掉真正的泄漏，要么会把刻意的行为判成泄漏。
cert_body_leak() {
  _hits=0
  for _f in "$PAYLOADS"/certlist-*.json "$PAYLOADS"/certdetail-*.json \
    "$PAYLOADS"/upload-*.json "$WORK/secret.json"; do
    [ -f "$_f" ] || continue
    if grep -q -- "$1" "$_f" 2>/dev/null; then
      _hits=$((_hits + 1))
      note "  命中 $(basename "$_f")"
    fi
  done
  printf '%s' "$_hits"
}

# cert_fp_upper <PEM>：算 sha256 指纹，转成 openssl 那种大写十六进制（无冒号）。
cert_fp_upper() {
  openssl x509 -in "$1" -noout -fingerprint -sha256 2>/dev/null |
    sed 's/^.*=//' | tr -d ':' | tr 'a-f' 'A-F'
}

# served_fp_upper <域名>：从 443 上取回真正在服务的那张证书的指纹。
served_fp_upper() {
  openssl s_client -connect 127.0.0.1:443 -servername "$1" </dev/null 2>/dev/null |
    openssl x509 -noout -fingerprint -sha256 2>/dev/null |
    sed 's/^.*=//' | tr -d ':' | tr 'a-f' 'A-F'
}

# 应用之后问 443 一律带期限，别问一次就下结论。
#
# 原因：`nginx -s reload` 是异步的——命令返回时 master 才刚收到 HUP，新 worker 还没
# 接手监听。第一遍在"应用之后立刻问"上栽了两次：一次判出"reload 之后还是旧证书"
# （假的，产品自己等 12×250ms 就对上了），一次把站点 B 的 443 读成了站点 A 的证书
# （那时候只有 A 那一块在跑，SNI 落到默认块）。**等，是断言的一部分。**
wait_http_probe() {   # <期望值> <取一次的函数> <参数> [秒]
  _want=$1; _fn=$2; _arg=$3; _deadline=$(( $(date +%s) + ${4:-10} ))
  while :; do
    _got=$("$_fn" "$_arg")
    if [ "$_got" = "$_want" ]; then printf '%s' "$_got"; return 0; fi
    if [ "$(date +%s)" -ge "$_deadline" ]; then printf '%s' "$_got"; return 0; fi
    sleep 0.3
  done
}
# served_fp_upper_wait <域名> <期望指纹（大写）>
served_fp_upper_wait() { wait_http_probe "$2" served_fp_upper "$1" 10; }

# https_body <域名> [CA 文件]：拿一次 443 上的响应体（不给 CA 就走系统信任链）。
https_body() {
  if [ -n "${2:-}" ]; then
    curl -sS --max-time 10 --cacert "$2" --resolve "$1:443:127.0.0.1" "https://$1/" 2>/dev/null
  else
    curl -sS --max-time 10 --resolve "$1:443:127.0.0.1" "https://$1/" 2>/dev/null
  fi
}

# https_body_wait <域名> <CA 文件或空> <期望响应体>
https_body_wait() {
  _d=$1; _ca=$2; _want=$3; _deadline=$(( $(date +%s) + 10 ))
  while :; do
    _got=$(https_body "$_d" "$_ca")
    if [ "$_got" = "$_want" ]; then printf '%s' "$_got"; return 0; fi
    if [ "$(date +%s)" -ge "$_deadline" ]; then printf '%s' "$_got"; return 0; fi
    sleep 0.3
  done
}

# mode_of <路径>：八进制权限位（例如 600）。用 -L 解引用：certbot 的 live/ 下全是
# 指向 archive/ 的符号链接，而符号链接自身的权限位永远是 777（没有意义）。
mode_of() {
  stat -L -c '%a' "$1" 2>/dev/null || printf 'missing'
}
owner_of() { stat -L -c '%U' "$1" 2>/dev/null || printf 'missing'; }
nginx_t_ok() { "$NGINX_BIN" -t >/dev/null 2>&1 && printf 'ok' || printf 'fail'; }
managed_conf() { ls "$NGINX_CONF_D"/upanel-*.conf 2>/dev/null | sort | tr '\n' ' '; }

cleanup() {
  for _pid in "${UPSTREAM_PID:-}" "${WEB_PID:-}" "${AGENT_PID:-}"; do
    if [ -n "$_pid" ] && kill -0 "$_pid" 2>/dev/null; then
      kill -TERM "$_pid" 2>/dev/null
      wait "$_pid" 2>/dev/null
    fi
  done
  rm -f "$SOCK"
  # 托管配置与证书产物（只删测试域名的那些）。
  rm -f "$NGINX_CONF_D"/upanel-*.conf 2>/dev/null
  rm -rf /etc/nginx/upanel-auth 2>/dev/null
  for d in "$DOM_ISSUE" "$DOM_IMPORT" "$DOM_EXPIRING" "$DOM_DEAD"; do
    rm -f "$MANUAL_CERT_DIR/$d.pem" "$MANUAL_CERT_DIR/$d.key" 2>/dev/null
    rm -rf "$LIVE_DIR/$d" 2>/dev/null
    rm -rf "/etc/letsencrypt/archive/$d" "/etc/letsencrypt/renewal/$d.conf" 2>/dev/null
  done
  # 校验文件
  rm -rf "$ACME_WEBROOT/.well-known/acme-challenge" 2>/dev/null
  # nat 规则
  iptables -t nat -D OUTPUT -p tcp -d 127.0.0.1 --dport 5002 -j REDIRECT --to-ports 80 2>/dev/null
  iptables -t nat -D PREROUTING -p tcp -d 127.0.0.1 --dport 5002 -j REDIRECT --to-ports 80 2>/dev/null
  # 信任库
  if [ -f "$CA_API_FILE" ] || [ -f "$CA_ROOT_FILE" ]; then
    rm -f "$CA_API_FILE" "$CA_ROOT_FILE"
    update-ca-certificates >/dev/null 2>&1
  fi
  # /etc/hosts
  if [ -f "$WORK/hosts.orig" ]; then
    cp "$WORK/hosts.orig" /etc/hosts
  fi
  "$NGINX_BIN" -t >/dev/null 2>&1 && systemctl reload nginx >/dev/null 2>&1
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
emsg() { jget "$WORK/resp.json" 'd["error"]["message"]'; }
dget() { jget "$WORK/resp.json" "d[\"data\"]$1"; }

# api_save <载荷名>：把当前响应体存下来（后面做“响应里没有私钥”的全量扫描）。
api_save() { cp "$WORK/resp.json" "$PAYLOADS/$1.json" 2>/dev/null; }

create_site() {
  _code=$(api "$JAR" "$CSRF" POST /api/v1/proxies "$2")
  cp "$WORK/resp.json" "$PAYLOADS/site-$1.json" 2>/dev/null
  printf '%s' "$_code"
}
sget() { jget "$PAYLOADS/site-$1.json" "d[\"data\"]$2"; }
patch_site() {
  _code=$(api "$JAR" "$CSRF" PATCH "/api/v1/proxies/$2" "$3")
  cp "$WORK/resp.json" "$PAYLOADS/site-$1.json" 2>/dev/null
  printf '%s' "$_code"
}
get_site() {
  _code=$(api "$JAR" "$CSRF" GET "/api/v1/proxies/$2")
  cp "$WORK/resp.json" "$PAYLOADS/site-$1.json" 2>/dev/null
  printf '%s' "$_code"
}
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

# reauth：写秘密与执行证书动作都要求 5 分钟内重新认证过。
reauth() {
  _code=$(api "$JAR" "$CSRF" POST /api/v1/auth/reauth "{\"password\":\"$PASSWORD\"}")
  [ "$_code" = "200" ] || fail "再认证" "状态码 $_code"
  printf '%s' "$_code"
}

put_secret() {
  reauth >/dev/null
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

# upload_cert <文件> <保存名>：POST /certificates/uploads，回显状态码。
upload_cert() {
  _code=$(curl -s -b "$JAR" -o "$WORK/resp.json" -w '%{http_code}' -X POST "$BASE$UPLOADS_PATH" \
    -H "Origin: $ORIGIN" -H "X-CSRF-Token: $CSRF" -F "certificate=@$1")
  cp "$WORK/resp.json" "$PAYLOADS/$2.json" 2>/dev/null
  printf '%s' "$_code"
}

# preflight <动作> <参数 JSON>：响应存为 $WORK/preflight.json，回显状态码。
#
# 证书的预检比站点的更重：签发要现跑一次 certbot --version、还要对域名做一次
# HTTP-01 可达性探测（真的发一个 HTTP 请求）。代理侧单次调用整体 10 秒，
# 慢的时候回来的是 503 AGENT_UNAVAILABLE——那是调用超时，不是判决。所以只对
# 这一种失败重试，判定的内容仍然是判决本身。
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
      note "预检 503 AGENT_UNAVAILABLE（用时 ${PRE_TIME}s，第 $_try 次重试）：代理调用超时，不是判决" >&2
      sleep 3
      continue
    fi
    break
  done
  printf '%s' "$PRE_CODE"
}

submit_job() {
  python3 -c 'import json,sys
print(json.dumps({"preflight_id": sys.argv[1]}))' "$1" >"$WORK/jobreq.json"
  _code=$(curl -s -b "$JAR" -o "$WORK/resp.json" -w '%{http_code}' -X POST "$BASE$JOBS" \
    -H 'Content-Type: application/json' -H "Origin: $ORIGIN" -H "X-CSRF-Token: $CSRF" \
    -H "Idempotency-Key: $2" -d @"$WORK/jobreq.json")
  cp "$WORK/resp.json" "$WORK/submit.json"
  printf '%s' "$_code"
}

wait_job() {
  _i=0
  while [ "$_i" -lt 600 ]; do
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
jobobserved() { jobget "d[\"data\"][\"result\"][\"observed_state\"][\"$1\"]"; }
jerrmsg() { jobget '(d["data"].get("error") or {}).get("message") or ""'; }

# run_cert_action <动作> <参数 JSON> <幂等键后缀>
#
# 预检 → 再认证（证书动作都要求 R）→ 提交 → 等终态。结果放在全局变量里：
# 放进 $( ) 会开子 shell，变量带不回来。
run_cert_action() {
  PRE_CODE=$(preflight "$1" "$2")
  cp "$WORK/preflight.json" "$PAYLOADS/preflight-$3.json"
  PRE_ALLOWED=$(pallowed)
  PRE_ID=$(pget 'd["data"]["preflight_id"]')
  if [ "$PRE_ALLOWED" = "True" ] && [ "$PRE_CODE" = "200" ]; then
    REAUTH_CODE=$(api "$JAR" "$CSRF" POST /api/v1/auth/reauth "{\"password\":\"$PASSWORD\"}")
    [ "$REAUTH_CODE" = "200" ] || fail "再认证（$3）" "状态码 $REAUTH_CODE"
  fi
  if [ "$PRE_CODE" != "200" ] || [ -z "$PRE_ID" ]; then
    JOB_STATE="preflight:$PRE_CODE"
    JOB_ID=""
    : >"$WORK/job.json"
    return 0
  fi
  JOB_CODE=$(submit_job "$PRE_ID" "p103-$3")
  cp "$WORK/submit.json" "$PAYLOADS/submit-$3.json"
  if [ "$JOB_CODE" = "202" ]; then
    JOB_ID=$(jget "$WORK/submit.json" 'd["data"]["job_id"]')
    JOB_STATE=$(wait_job "$JOB_ID")
    cp "$WORK/job.json" "$PAYLOADS/job-$3.json"
  else
    JOB_STATE="submit:$JOB_CODE"
    JOB_ID=""
    : >"$WORK/job.json"
    fail "提交（$3）" "状态码 $JOB_CODE，响应 $(head -c 300 "$WORK/submit.json")"
  fi
}

# wait_cert_row <域名> <超时秒>：等账本写入那一行（任务终态与账本写入是同一刻，但仍等一下）。
wait_cert_row() {
  _i=0
  while [ "$_i" -lt "$2" ]; do
    _code=$(api "$JAR" "$CSRF" GET "$CERT_PATH?domain=$1")
    cp "$WORK/resp.json" "$PAYLOADS/certlist-$1.json"
    _n=$(jget "$PAYLOADS/certlist-$1.json" 'len(d["data"]["items"])')
    [ "$_n" = "1" ] && return 0
    _i=$((_i + 1))
    sleep 1
  done
  return 0
}
csget() { jget "$PAYLOADS/certlist-$1.json" "d[\"data\"][\"items\"][0][\"$2\"]"; }

mkdir -p "$WORK/logs" "$PAYLOADS" "$SECRETS_DIR" "$WEB_DB_DIR" "$UPSTREAM_DIR" "$CERTS_DIR"
chown "$WEB_USER" "$WEB_DB_DIR" 2>/dev/null
: >"$AGENT_LOG"
: >"$WEB_LOG"

echo "== 0. 干净起点 =="
rm -f "$NGINX_CONF_D"/upanel-*.conf 2>/dev/null
rm -rf /etc/nginx/upanel-auth 2>/dev/null
rm -f "$MANUAL_CERT_DIR/$DOM_IMPORT.pem" "$MANUAL_CERT_DIR/$DOM_IMPORT.key" 2>/dev/null
rm -rf "$LIVE_DIR/$DOM_ISSUE" 2>/dev/null
expect_eq "起点：没有面板的托管配置" "" "$(managed_conf)"
expect_eq "起点：nginx -t 通过" "ok" "$(nginx_t_ok)"
if [ ! -d "$ACME_WEBROOT" ]; then
  mkdir -p "$ACME_WEBROOT"
  chmod 0755 "$ACME_WEBROOT"
fi
pass "校验根目录存在（$ACME_WEBROOT）"

echo
echo "== 1. 测试 ACME（Pebble）+ 信任链 =="
if ! curl -fsS --cacert /opt/p103/pebble/certs/cert.pem "$ACME_API_URL" >/dev/null 2>&1; then
  nohup "$PEBBLE_BIN" -config "$PEBBLE_CONFIG" >"$PEBBLE_LOG" 2>&1 &
  PEBBLE_PID=$!
  _ok=0
  for _ in $(seq 1 60); do
    if curl -fsS --cacert /opt/p103/pebble/certs/cert.pem "$ACME_API_URL" >/dev/null 2>&1; then
      _ok=1
      break
    fi
    sleep 0.5
  done
  if [ "$_ok" = "1" ]; then pass "Pebble 启动并提供 ACME 目录"; else
    fail "Pebble 启动" "$(tail -5 "$PEBBLE_LOG" 2>/dev/null)"
  fi
else
  pass "Pebble 已在运行"
fi

# certbot 要连 Pebble 的 HTTPS API：把 Pebble 的 API 证书放进系统信任库。
# 生产环境里这一步不存在（Let's Encrypt 的证书本来就被信任），它只是“本地 CA”的代价。
cp /opt/p103/pebble/certs/cert.pem "$CA_API_FILE"
update-ca-certificates >/dev/null 2>&1
if curl -fsS "$ACME_API_URL" >/dev/null 2>&1; then
  pass "系统信任库接受测试 ACME 的 API 证书（certbot 因此能连上）"
else
  fail "系统信任库接受测试 ACME 证书" "curl $ACME_API_URL 失败"
fi

# certbot 用 python-requests，它的信任库是 certifi 那份，不是系统库。代理给子进程的
# 环境是固定的（只有 PATH/LANG/LC_ALL），塞不进 REQUESTS_CA_BUNDLE。所以只有当
# certifi 指向系统库时，“更新系统信任库”才等于“certbot 也信了”。这一条必须查，
# 否则后面签发失败会被误读成产品的问题。
CERTIFI_PATH=$(python3 -c 'import certifi;print(certifi.where())' 2>/dev/null)
if [ "$CERTIFI_PATH" = "/etc/ssl/certs/ca-certificates.crt" ]; then
  pass "certbot 的 CA 包与系统信任库是同一个文件（certifi → $CERTIFI_PATH）"
else
  fail "certbot 的 CA 包与系统信任库不是同一个文件" "certifi → ${CERTIFI_PATH:-未安装}；签发会在 TLS 校验处失败，这是夹具问题不是产品问题"
fi
expect_not_contains "目录里没有 Let's Encrypt 正式环境" "acme-v02.api.letsencrypt.org" \
  "$(curl -fsS "$ACME_API_URL" 2>/dev/null)"
expect_not_contains "目录里没有 Let's Encrypt 测试环境" "acme-staging-v02.api.letsencrypt.org" \
  "$(curl -fsS "$ACME_API_URL" 2>/dev/null)"
# Pebble 的目录里有 caaIdentities "pebble.letsencrypt.org"——那是它自己的名字，不是
# 生产地址，所以判据是“两个生产目录都不在里面”，不是“没出现过 letsencrypt 字样”。
expect_contains "目录里的端点都指向本地测试服务" "https://127.0.0.1:14000/" \
  "$(curl -fsS "$ACME_API_URL" 2>/dev/null)"

# Pebble 的根证书：它签出来的站点证书要靠它才被信任（下面 443 的 curl 不加 -k）。
# 管理接口本身也是 HTTPS，并且要认它自己的 API 证书。
curl -fsS --cacert "$ACME_API_CERT" "$ACME_ROOT_URL" -o "$CERTS_DIR/acme-root.pem" 2>/dev/null
if [ -s "$CERTS_DIR/acme-root.pem" ]; then
  pass "取到测试 ACME 的根证书"
  cp "$CERTS_DIR/acme-root.pem" "$CA_ROOT_FILE"
  update-ca-certificates >/dev/null 2>&1
else
  fail "取到测试 ACME 的根证书" "GET $ACME_ROOT_URL 失败"
fi

echo
echo "== 2. 网络夹具（HOSTS + 校验口重定向） =="
if [ ! -f "$WORK/hosts.orig" ]; then cp /etc/hosts "$WORK/hosts.orig"; fi
for d in "$DOM_ISSUE" "$DOM_IMPORT" "$DOM_EXPIRING" "$DOM_DEAD"; do
  grep -qE "[[:space:]]$d([[:space:]]|$)" /etc/hosts || printf '127.0.0.1 %s\n' "$d" >>/etc/hosts
done
if getent hosts "$DOM_ISSUE" >/dev/null 2>&1; then
  pass "测试域名解析到本机（/etc/hosts）"
else
  fail "测试域名解析" "getent hosts $DOM_ISSUE 失败"
fi

# Pebble 的 HTTP-01 校验连的是它配置里的 httpPort（5002）。本机 nginx 只听 80，
# 所以用 nat 把这半个端口引到 80——**这正是产品在真实环境里的路径**（域名 → 80 →
# nginx 的 /.well-known/acme-challenge/ → webroot），只是端口号被 Pebble 换掉了。
iptables -t nat -C OUTPUT -p tcp -d 127.0.0.1 --dport 5002 -j REDIRECT --to-ports 80 2>/dev/null ||
  iptables -t nat -A OUTPUT -p tcp -d 127.0.0.1 --dport 5002 -j REDIRECT --to-ports 80
iptables -t nat -C PREROUTING -p tcp -d 127.0.0.1 --dport 5002 -j REDIRECT --to-ports 80 2>/dev/null ||
  iptables -t nat -A PREROUTING -p tcp -d 127.0.0.1 --dport 5002 -j REDIRECT --to-ports 80
pass "校验口重定向就位（5002 → 80）"

if systemctl is-active --quiet nginx; then
  pass "nginx 正在运行"
else
  systemctl start nginx >/dev/null 2>&1 && pass "nginx 已启动" || fail "nginx 启动" "systemctl start nginx 失败"
fi

echo
echo "== 3. 配置与启动（代理 root、Web 非 root） =="
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
    "logs": {},
    "certificates": { "staging_directory_url": "$ACME_API_URL" }
  },
  "database": { "path": "$WEB_DB", "busy_timeout_ms": 5000 },
  "log": { "level": "info", "format": "json" }
}
JSON
chmod 0644 "$SYS_CFG"
note "staging 目录地址指向本地测试 ACME：$ACME_API_URL"

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
expect_eq "upanel-web 的 uid 是计划值" "$WEB_UID_EXPECTED" \
  "$(id -u "$WEB_USER" 2>/dev/null || printf 'missing')"
expect_eq "数据库迁移到第 10 版（P10.3 引入证书表）" "10" "$(dbq 'SELECT MAX(version) FROM schema_migrations')"

JAR=$WORK/admin.jar
expect_eq "管理员登录" "200" "$(login admin "$PASSWORD" "$JAR" login-admin)"
CSRF=$(csrf_of "$WORK/login-admin.json")
expect_ne "拿到 CSRF 令牌" "" "$CSRF"

echo
echo "== 4. 上游与站点（先只有 80） =="
printf '%s\n' "$MARKER" >"$UPSTREAM_DIR/index.html"
( cd "$UPSTREAM_DIR" && exec python3 -m http.server "$UPSTREAM_PORT" --bind 127.0.0.1 ) \
  >>"$UPSTREAM_LOG" 2>&1 &
UPSTREAM_PID=$!
for _ in $(seq 1 50); do
  curl -fsS "http://127.0.0.1:$UPSTREAM_PORT/" >/dev/null 2>&1 && break
  sleep 0.1
done
if curl -fsS "http://127.0.0.1:$UPSTREAM_PORT/" >/dev/null 2>&1; then
  pass "上游在 127.0.0.1:$UPSTREAM_PORT 提供 HTTP 服务"
else
  fail "上游启动" "$(tail -3 "$UPSTREAM_LOG" 2>/dev/null)"
fi

expect_eq "登记站点 A（$DOM_ISSUE）" "201" \
  "$(create_site a "{\"domain\":\"$DOM_ISSUE\",\"upstream_target_id\":\"local_$UPSTREAM_PORT\",\"upstream_port\":$UPSTREAM_PORT}")"
SITE_A=$(sget a '["id"]')
expect_ne "拿到站点 A 的 id" "" "$SITE_A"
expect_eq "站点 A 此刻没有绑定证书" "" "$(sget a '.get("certificate_id") or ""')"

run_cert_action proxy.apply "{\"site_id\":\"$SITE_A\"}" apply-a1
expect_eq "应用站点 A 的终态" "succeeded" "$JOB_STATE"
expect_eq "应用站点 A 的结果已核验" "True" "$(jverified)"
wait_applied a "$SITE_A" applied
expect_eq "站点 A 的 applied 状态" "applied" "$(sget a '["applied"]["state"]')"
expect_eq "站点 A 在 80 上服务上游页面" "$MARKER" \
  "$(curl -s --max-time 10 -H "Host: $DOM_ISSUE" http://127.0.0.1/ 2>/dev/null)"

# 脚本自己先走一遍 Pebble 将要走的那条路：域名 → 5002 → nginx → webroot。
TOKEN="p103-$(head -c 8 /dev/urandom | od -An -tx1 | tr -d ' \n')"
install -d -m 0755 "$ACME_WEBROOT/.well-known/acme-challenge"
printf '%s\n' "$TOKEN" >"$ACME_WEBROOT/.well-known/acme-challenge/$TOKEN"
chmod 0644 "$ACME_WEBROOT/.well-known/acme-challenge/$TOKEN"
GOT=$(curl -s --max-time 10 "http://$DOM_ISSUE:5002/.well-known/acme-challenge/$TOKEN" 2>/dev/null | tr -d '\n')
expect_eq "HTTP-01 校验路径通（域名:5002 → nginx → webroot）" "$TOKEN" "$GOT"
rm -f "$ACME_WEBROOT/.well-known/acme-challenge/$TOKEN"

echo
echo "== 5. 签发：staging 必须显式选、证书真的落到盘上 =="
expect_eq "签发前 $LIVE_DIR/$DOM_ISSUE 不存在" "missing" "$(mode_of "$LIVE_DIR/$DOM_ISSUE")"

# 5a. 缺 staging：面板拒绝，且不产生预检（更不会消耗 ACME 请求）。
PRE_CODE=$(preflight certificate.issue "{\"domain\":\"$DOM_ISSUE\",\"email\":\"ops@example.test\"}")
api_save preflight-issue-nostaging
expect_eq "缺 staging 的签发被拒" "400" "$PRE_CODE"
expect_eq "缺 staging 的错误码" "VALIDATION_ERROR" "$(jget "$WORK/preflight.json" 'd["error"]["code"]')"
expect_contains "错误信息指出缺的是 staging" "staging" "$(jget "$WORK/preflight.json" 'd["error"]["message"]')"

# 5b. 正常签发（staging=true）。
run_cert_action certificate.issue \
  "{\"domain\":\"$DOM_ISSUE\",\"email\":\"ops@example.test\",\"staging\":true}" issue-a
cp "$WORK/job.json" "$PAYLOADS/job-issue-a.json"
expect_eq "签发的预检放行" "True" "$PRE_ALLOWED"
expect_eq "签发的终态" "succeeded" "$JOB_STATE"
expect_eq "签发的结果是 verified" "verified" "$(joutcome)"
expect_eq "签发结果带核验时间" "True" "$(jverified)"
expect_contains "签发的锁里有 ACME 全局锁" "acme:certbot" "$(pget '",".join(d["data"]["locks"])')"
expect_contains "签发的锁里有该域名的锁" "certificate:$DOM_ISSUE" "$(pget '",".join(d["data"]["locks"])')"
expect_eq "签发要求再认证" "True" "$(preauth)"

expect_eq "certbot 写下了 fullchain.pem" "644" "$(mode_of "$LIVE_DIR/$DOM_ISSUE/fullchain.pem")"
expect_eq "certbot 写下了 privkey.pem" "600" "$(mode_of "$LIVE_DIR/$DOM_ISSUE/privkey.pem")"
# live/<域名> 自己是 0755（里面只有符号链接），真正把非 root 挡在外面的是它的父目录：
# certbot 把 /etc/letsencrypt/live 建成 0700，而 archive/ 里的真文件里私钥是 0600。
expect_eq "/etc/letsencrypt/live 只有 root 能进" "700" "$(mode_of "$LIVE_DIR")"
# certbot 会在 live/<域名>/ 里放一个 README（明文，写的是证书路径与到期日，不含密钥），
# 除此之外的条目必须是符号链接——真正的内容在 archive/，那一层只有 root 能读。
expect_eq "live/<域名> 里除 README 外没有真文件" "0" \
  "$(find "$LIVE_DIR/$DOM_ISSUE" -maxdepth 1 -type f ! -name README 2>/dev/null | wc -l | tr -d ' ')"
expect_eq "live/<域名> 里的四个 pem 都是符号链接" "4" \
  "$(find "$LIVE_DIR/$DOM_ISSUE" -maxdepth 1 -type l -name '*.pem' 2>/dev/null | wc -l | tr -d ' ')"
expect_eq "privkey.pem 属主是 root" "root" "$(owner_of "$LIVE_DIR/$DOM_ISSUE/privkey.pem")"
if runuser -u "$WEB_USER" -- cat "$LIVE_DIR/$DOM_ISSUE/privkey.pem" >/dev/null 2>&1; then
  fail "uid $WEB_UID_EXPECTED 读不到 certbot 的私钥" "竟然读到了"
else
  pass "uid $WEB_UID_EXPECTED 读不到 certbot 的私钥"
fi
if runuser -u "$WEB_USER" -- cat "$LIVE_DIR/$DOM_ISSUE/fullchain.pem" >/dev/null 2>&1; then
  fail "uid $WEB_UID_EXPECTED 读不到 certbot 的证书链" "竟然读到了"
else
  pass "uid $WEB_UID_EXPECTED 读不到 certbot 的证书链（目录 0700）"
fi

wait_cert_row "$DOM_ISSUE" 10
cp "$PAYLOADS/certlist-$DOM_ISSUE.json" "$PAYLOADS/certlist-issue.json"
expect_eq "证书列表里出现这条记录" "1" "$(jget "$PAYLOADS/certlist-issue.json" 'len(d["data"]["items"])')"
expect_eq "来路是 certbot" "certbot" "$(csget "$DOM_ISSUE" source)"
expect_eq "记为 staging 签发" "True" "$(csget "$DOM_ISSUE" staging)"
expect_eq "私钥保管方是 certbot" "certbot" "$(csget "$DOM_ISSUE" key_storage)"
expect_eq "到期状态是 valid" "valid" "$(csget "$DOM_ISSUE" renewal_state)"
expect_contains "签发者是测试环境（Pebble）" "Pebble" "$(csget "$DOM_ISSUE" issuer)"
CERT_A_ID=$(csget "$DOM_ISSUE" id)
CERT_A_FP=$(csget "$DOM_ISSUE" fingerprint)
expect_ne "拿到证书标识" "" "$CERT_A_ID"
expect_ne "拿到指纹" "" "$CERT_A_FP"
DISK_FP=$(cert_fp_upper "$LIVE_DIR/$DOM_ISSUE/cert.pem")
expect_eq "库里的指纹与盘上的证书一致" "$DISK_FP" "$(printf '%s' "$CERT_A_FP" | tr 'a-f' 'A-F')"
NOT_AFTER_A=$(csget "$DOM_ISSUE" not_after)

# 到期时间是算出来的，所以这里验的是“算得对”：拿响应里的 not_after 按同一条规则
# （21 天临期窗口）重算一遍，必须与接口给的 renewal_state 一致。
#
# 不写死“约 90 天”：那是 Let's Encrypt 的叶子证书有效期，测试 CA（Pebble）不一定
# 用这个数。写死它就成了对测试 CA 的假设，而不是对产品的断言。
STATE_EXPECT=$(python3 - "$NOT_AFTER_A" <<'PY'
import sys, datetime
raw = sys.argv[1].replace("Z", "+00:00")
when = datetime.datetime.fromisoformat(raw)
now = datetime.datetime.now(datetime.timezone.utc)
window = datetime.timedelta(days=21)
if when <= now:
    print("expired")
elif when <= now + window:
    print("expiring")
else:
    print("valid")
PY
)
expect_eq "renewal_state 与 not_after 的推导一致" "$STATE_EXPECT" "$(csget "$DOM_ISSUE" renewal_state)"
DAYS_LEFT=$(python3 - "$NOT_AFTER_A" <<'PY'
import sys, datetime
raw = sys.argv[1].replace("Z", "+00:00")
when = datetime.datetime.fromisoformat(raw)
print(int((when - datetime.datetime.now(datetime.timezone.utc)).total_seconds() // 86400))
PY
)
note "测试 CA 给出的有效期约 ${DAYS_LEFT} 天（Let's Encrypt 的 90 天不在这里断言）"
DISK_NOT_AFTER=$(date -u -d "$(openssl x509 -in "$LIVE_DIR/$DOM_ISSUE/cert.pem" -noout -enddate 2>/dev/null | sed 's/^notAfter=//')" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null)
expect_eq "库里的到期时间与盘上证书一致" "${DISK_NOT_AFTER%Z}" "$(printf '%s' "$NOT_AFTER_A" | sed 's/Z$//')"

# 5c. 现场事实（certificate.state）：能读到盘上那张证书，且只说“在不在、mode 多少”。
DETAIL_CODE=$(api "$JAR" "$CSRF" GET "$CERT_PATH/$CERT_A_ID")
api_save certdetail-issue
expect_eq "详情接口状态码" "200" "$DETAIL_CODE"
expect_eq "详情里的记录标识" "$CERT_A_ID" "$(jget "$WORK/resp.json" 'd["data"]["certificate"]["id"]')"
expect_eq "现场事实可读" "True" "$(jget "$WORK/resp.json" 'bool(d["data"]["observed"].get("available"))')"
expect_eq "现场读到私钥存在" "True" "$(jget "$WORK/resp.json" 'bool(d["data"]["observed"].get("key_present"))')"
expect_eq "现场读到的私钥权限是 0600" "0600" "$(jget "$WORK/resp.json" 'd["data"]["observed"].get("key_mode")')"
expect_eq "现场读到的 staging 标记" "True" "$(jget "$WORK/resp.json" 'bool(d["data"]["observed"].get("staging"))')"

echo
echo "== 6. 绑定证书 → 443 真的在服务 =="
expect_eq "给站点 A 绑定证书" "200" \
  "$(patch_site a "$SITE_A" "{\"version\":$(sget a '["version"]'),\"certificate_id\":\"$CERT_A_ID\"}")"
expect_eq "站点 A 的证书标识" "$CERT_A_ID" "$(sget a '["certificate_id"]')"

run_cert_action proxy.apply "{\"site_id\":\"$SITE_A\"}" apply-a2
expect_eq "绑定后应用的终态" "succeeded" "$JOB_STATE"
wait_applied a "$SITE_A" applied
expect_eq "绑定后 applied 状态" "applied" "$(sget a '["applied"]["state"]')"
CONF_A=$NGINX_CONF_D/upanel-$DOM_ISSUE.conf
expect_contains "托管配置里有 443" "listen 443 ssl" "$(cat "$CONF_A" 2>/dev/null)"
expect_contains "托管配置里的证书路径来自记录" "$LIVE_DIR/$DOM_ISSUE/fullchain.pem" "$(cat "$CONF_A" 2>/dev/null)"
expect_not_contains "托管配置里没有私钥内容" "PRIVATE KEY" "$(cat "$CONF_A" 2>/dev/null)"

expect_eq "80 端口跳转到 https" "301" \
  "$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 -H "Host: $DOM_ISSUE" http://127.0.0.1/ 2>/dev/null)"
# 先等 443 上换成本次应用的那张证书，再谈响应体：reload 是异步的，见 wait_http_probe 的注释。
SERVED_FP=$(served_fp_upper_wait "$DOM_ISSUE" "$(printf '%s' "$CERT_A_FP" | tr 'a-f' 'A-F')")
expect_eq "443 上正在服务的证书就是这条记录" \
  "$(printf '%s' "$CERT_A_FP" | tr 'a-f' 'A-F')" "$SERVED_FP"
HTTPS_BODY=$(https_body_wait "$DOM_ISSUE" "" "$MARKER")
expect_eq "443 上取到上游页面（系统信任链，无 -k）" "$MARKER" "$HTTPS_BODY"
if [ "$HTTPS_BODY" = "$MARKER" ]; then
  pass "证书链被系统信任（curl 校验通过了链）"
else
  fail "证书链验证" "响应体不是上游标记：$(printf '%s' "$HTTPS_BODY" | head -c 200)"
fi
expect_eq "ACP 之后 nginx -t 仍然通过" "ok" "$(nginx_t_ok)"

echo
echo "== 7. 续期动作与限流 =="
# 7a. 刚签发完就续期：被限流（依据是任务表里的尝试，失败的也算）。
PRE_CODE=$(preflight certificate.renew "{\"domain\":\"$DOM_ISSUE\",\"staging\":true}")
api_save preflight-renew-cooldown
expect_eq "一小时内再次续期被拒" "429" "$PRE_CODE"
expect_eq "限流的错误码" "RESOURCE_BUSY" "$(jget "$WORK/preflight.json" 'd["error"]["code"]')"
expect_eq "限流期间没有产生任务" "0" \
  "$(dbq "SELECT COUNT(*) FROM jobs WHERE action = 'certificate.renew'")"

# 7b. 把这次尝试的时间前移一小时以上——**验收夹具**，不是产品行为。
#     限流读两处：任务表（RecentAttempts）与证书记录上的 last_attempt_at。
OLD_TS=$(date -u -d '3 hours ago' +%Y-%m-%dT%H:%M:%SZ)
dbexec "UPDATE jobs SET created_at = '$OLD_TS' WHERE action IN ('certificate.issue','certificate.renew')"
dbexec "UPDATE certificates SET last_attempt_at = '$OLD_TS'"
note "已把签发尝试的时间前移到 $OLD_TS（只为让续期可测）"

run_cert_action certificate.renew "{\"domain\":\"$DOM_ISSUE\",\"staging\":true}" renew-a
expect_eq "续期的终态" "succeeded" "$JOB_STATE"
expect_eq "续期的结果是 verified" "verified" "$(joutcome)"
wait_cert_row "$DOM_ISSUE" 10
cp "$PAYLOADS/certlist-$DOM_ISSUE.json" "$PAYLOADS/certlist-renew.json"
expect_eq "续期后仍然只有一条记录（按域名唯一）" "1" \
  "$(jget "$PAYLOADS/certlist-renew.json" 'len(d["data"]["items"])')"
expect_eq "续期不换记录标识" "$CERT_A_ID" "$(csget "$DOM_ISSUE" id)"
CERT_A_FP2=$(csget "$DOM_ISSUE" fingerprint)
expect_ne "续期换了证书（指纹变了）" "$CERT_A_FP" "$CERT_A_FP2"
expect_eq "站点绑定没被续期改掉" "$CERT_A_ID" "$(sget a '["certificate_id"]')"
NOT_AFTER_B=$(csget "$DOM_ISSUE" not_after)
expect_ne "到期时间被刷新" "$NOT_AFTER_A" "$NOT_AFTER_B"

# 续期只换盘上的文件，**不碰 Nginx**：worker 启动时把证书读进内存，文件换了它不会
# 自动重读。所以此刻 443 上仍然是旧证书——这不是缺陷，是“续期”和“让新证书生效”
# 两件事，把它们混成一条断言，就永远分不清是证书没换上还是服务没重载。
# 这条断言也是给 P12 的：timer 必须在续期之后走一次应用（reload），否则新证书
# 躺在盘上没人用，而到期提醒却已经清掉了。
SERVED_FP2=$(served_fp_upper "$DOM_ISSUE")
expect_eq "续期之后、重新应用之前，443 仍在用旧证书" \
  "$(printf '%s' "$CERT_A_FP" | tr 'a-f' 'A-F')" "$SERVED_FP2"
HTTPS_BODY=$(curl -sS --max-time 10 --resolve "$DOM_ISSUE:443:127.0.0.1" "https://$DOM_ISSUE/" 2>/dev/null)
expect_eq "续期没有打断正在服务的旧证书" "$MARKER" "$HTTPS_BODY"

# 让它生效：再应用一次站点（P12 的 timer 在续期之后要做的就是这一步）。
# 注意这一次应用**配置字节没变**——续期只换证书文件，渲染结果逐字节相同。
# 所以它是本阶段最关键的一条：如果 reload 判据只看"配置有没有变"，这一次会得到
# skipped_unchanged，新证书躺在盘上、443 上还是旧的，而报告全绿。
run_cert_action proxy.apply "{\"site_id\":\"$SITE_A\"}" apply-a3
expect_eq "续期后重新应用的终态" "succeeded" "$JOB_STATE"
expect_eq "重新应用的结果是 verified" "verified" "$(joutcome)"
expect_eq "重新应用时配置字节确实没变" "unchanged" \
  "$(jget "$PAYLOADS/job-apply-a3.json" 'd["data"]["result"]["data"]["file_state"]')"
expect_eq "配置没变、证书换了 → 仍然 reload" "done" \
  "$(jget "$PAYLOADS/job-apply-a3.json" 'd["data"]["result"]["data"]["reload"]')"
expect_eq "reload 的理由写明是证书轮换" "cert_rotated" \
  "$(jget "$PAYLOADS/job-apply-a3.json" 'd["data"]["result"]["observed_state"]["reload_reason"]')"
expect_eq "报告里记下 reload 前 443 用的是旧证书" "$CERT_A_FP" \
  "$(jget "$PAYLOADS/job-apply-a3.json" 'd["data"]["result"]["observed_state"]["served_cert_fingerprint"]')"
expect_eq "报告里记下 reload 后 443 用的是新证书" "$CERT_A_FP2" \
  "$(jget "$PAYLOADS/job-apply-a3.json" 'd["data"]["result"]["observed_state"]["served_cert_after_reload"]')"
if grep -q "仍然不是磁盘上这张证书" "$PAYLOADS/job-apply-a3.json" 2>/dev/null; then
  fail "reload 之后没有陈旧告警" "报告里说 443 上仍然不是新证书"
else
  pass "reload 之后没有陈旧告警"
fi
RELOADED_FP=$(served_fp_upper_wait "$DOM_ISSUE" "$(printf '%s' "$CERT_A_FP2" | tr 'a-f' 'A-F')")
expect_eq "重新应用之后 443 服务的是续期后的证书" \
  "$(printf '%s' "$CERT_A_FP2" | tr 'a-f' 'A-F')" "$RELOADED_FP"
HTTPS_BODY=$(https_body_wait "$DOM_ISSUE" "" "$MARKER")
expect_eq "重新应用之后 443 仍然服务上游页面" "$MARKER" "$HTTPS_BODY"

# 再生效一次：此刻 443 已经用的是盘上这张证书，就不该再 reload——否则每次应用都
# 重载一次服务，"幂等"就成了空话。
run_cert_action proxy.apply "{\"site_id\":\"$SITE_A\"}" apply-a4
expect_eq "第三轮应用又是 skipped_unchanged（不再无谓 reload）" "skipped_unchanged" \
  "$(jget "$PAYLOADS/job-apply-a4.json" 'd["data"]["result"]["data"]["reload"]')"
if [ "$(jget "$PAYLOADS/job-apply-a4.json" 'd["data"]["result"]["observed_state"].get("reload_reason") or ""')" != "" ]; then
  fail "第三轮没有 reload 理由" "证书没换却写了 reload_reason"
else
  pass "第三轮没有 reload 理由（因为根本没有 reload）"
fi

echo
echo "== 8. 手动导入：公钥走上传、私钥走秘密通道 =="
# 一张测试 CA，由它签叶子证书。两件事必须分开：
#   - **叶子不能是 CA**。`openssl req -x509` 造出来的自签证书默认 CA:TRUE，那样一张
#     “站点证书”会被产品正确地挡在预检（CERTIFICATE_IS_CA）——第一遍就是在这里
#     倒了 18 条断言，根因是我造的夹具错了，不是产品错了。
#   - **信任锚要单独给**。curl 不会拿一张 CA:FALSE 的叶子当锚点，所以证书链里带 CA，
#     校验时用 --cacert 指向那张 CA。
openssl req -x509 -newkey rsa:2048 -nodes -days 3650 -subj "/CN=p103 test CA" \
  -addext "basicConstraints=critical,CA:TRUE" -addext "keyUsage=critical,keyCertSign,cRLSign" \
  -keyout "$CERTS_DIR/p103-ca.key" -out "$CERTS_DIR/p103-ca.pem" >/dev/null 2>&1
chmod 600 "$CERTS_DIR/p103-ca.key"
expect_eq "生成一张测试 CA" "ok" "$([ -s "$CERTS_DIR/p103-ca.pem" ] && printf ok || printf missing)"

# mint_leaf <域名> <天数>：签一张叶子证书，输出 <域名>.pem（叶子）、<域名>-chain.pem
# （叶子 + CA）、<域名>.key。
mint_leaf() {
  _dom=$1
  _days=$2
  printf 'subjectAltName=DNS:%s\nbasicConstraints=critical,CA:FALSE\nkeyUsage=critical,digitalSignature,keyEncipherment\nextendedKeyUsage=serverAuth\n' \
    "$_dom" >"$CERTS_DIR/leaf.ext"
  openssl req -new -newkey rsa:2048 -nodes -subj "/CN=$_dom" \
    -keyout "$CERTS_DIR/$_dom.key" -out "$CERTS_DIR/$_dom.csr" >/dev/null 2>&1
  openssl x509 -req -in "$CERTS_DIR/$_dom.csr" -CA "$CERTS_DIR/p103-ca.pem" \
    -CAkey "$CERTS_DIR/p103-ca.key" -CAcreateserial -days "$_days" \
    -extfile "$CERTS_DIR/leaf.ext" -out "$CERTS_DIR/$_dom.pem" >/dev/null 2>&1
  cat "$CERTS_DIR/$_dom.pem" "$CERTS_DIR/p103-ca.pem" >"$CERTS_DIR/$_dom-chain.pem"
  chmod 600 "$CERTS_DIR/$_dom.key"
}

mint_leaf "$DOM_IMPORT" 365
expect_eq "签出一张覆盖 $DOM_IMPORT 的叶子证书（CA:FALSE）" "ok" \
  "$([ -s "$CERTS_DIR/$DOM_IMPORT-chain.pem" ] && printf ok || printf missing)"
if openssl x509 -in "$CERTS_DIR/$DOM_IMPORT.pem" -noout -text 2>/dev/null | grep -q "CA:FALSE"; then
  pass "叶子证书不是 CA（CA:FALSE）"
else
  fail "叶子证书不是 CA" "没读到 CA:FALSE，夹具不对"
fi

# 8a. 夹带私钥的上传直接拒绝（最常见的误操作）。
cat "$CERTS_DIR/$DOM_IMPORT-chain.pem" "$CERTS_DIR/$DOM_IMPORT.key" >"$CERTS_DIR/$DOM_IMPORT-both.pem"
expect_eq "带私钥的上传被拒" "400" "$(upload_cert "$CERTS_DIR/$DOM_IMPORT-both.pem" upload-both)"
expect_contains "拒绝信息说明了正确做法" "POST /secrets" "$(jget "$PAYLOADS/upload-both.json" 'd["error"]["message"]')"
rm -f "$CERTS_DIR/$DOM_IMPORT-both.pem"

# 8b. 只传公钥证书 → 暂存。
expect_eq "只上传公钥证书" "201" "$(upload_cert "$CERTS_DIR/$DOM_IMPORT-chain.pem" upload-ok)"
UPLOAD_ID=$(jget "$PAYLOADS/upload-ok.json" 'd["data"]["certificate_upload_id"]')
expect_ne "拿到上传标识" "" "$UPLOAD_ID"
expect_contains "上传结果的覆盖域名" "$DOM_IMPORT" "$(jget "$PAYLOADS/upload-ok.json" '",".join(d["data"]["domains"])')"
expect_eq "上传不写证书表（导入才写）" "1" "$(dbq 'SELECT COUNT(*) FROM certificates')"

# 8c. 私钥经 /secrets（标准输入式的请求体，argv 里没有明文）。
expect_eq "上传私钥到秘密存储" "201" "$(put_secret tls_private_key "$CERTS_DIR/$DOM_IMPORT.key")"
KEY_REF=$(jget "$WORK/secret.json" 'd["data"]["secret_ref"]')
expect_ne "拿到私钥引用" "" "$KEY_REF"
expect_eq "私钥用途被接受" "tls_private_key" "$(jget "$WORK/secret.json" 'd["data"]["purpose"]')"

# 8d. 导入。
run_cert_action certificate.import \
  "{\"domain\":\"$DOM_IMPORT\",\"certificate_upload_id\":\"$UPLOAD_ID\",\"private_key_secret_ref\":\"$KEY_REF\"}" import-b
expect_eq "导入的预检放行" "True" "$PRE_ALLOWED"
expect_contains "导入的影响里写明会写私钥文件" "private_key_file" "$(pget '",".join(i["kind"] for i in d["data"]["impacts"])')"
expect_eq "导入的终态" "succeeded" "$JOB_STATE"
expect_eq "导入的结果是 verified" "verified" "$(joutcome)"

expect_eq "证书落地权限 0644" "644" "$(mode_of "$MANUAL_CERT_DIR/$DOM_IMPORT.pem")"
expect_eq "私钥落地权限 0600" "600" "$(mode_of "$MANUAL_CERT_DIR/$DOM_IMPORT.key")"
expect_eq "证书目录只有 root 能进" "700" "$(mode_of "$MANUAL_CERT_DIR")"
expect_eq "私钥属主是 root" "root" "$(owner_of "$MANUAL_CERT_DIR/$DOM_IMPORT.key")"
if runuser -u "$WEB_USER" -- cat "$MANUAL_CERT_DIR/$DOM_IMPORT.key" >/dev/null 2>&1; then
  fail "uid $WEB_UID_EXPECTED 读不到手动导入的私钥" "竟然读到了"
else
  pass "uid $WEB_UID_EXPECTED 读不到手动导入的私钥"
fi
if runuser -u "$WEB_USER" -- cat "$MANUAL_CERT_DIR/$DOM_IMPORT.pem" >/dev/null 2>&1; then
  fail "uid $WEB_UID_EXPECTED 读不到导入的证书" "竟然读到了"
else
  pass "uid $WEB_UID_EXPECTED 读不到导入的证书（目录 0700）"
fi
expect_eq "盘上的私钥与上传的那把一致" "$(openssl pkey -in "$CERTS_DIR/$DOM_IMPORT.key" -pubout 2>/dev/null | sha256sum | awk '{print $1}')" \
  "$(openssl pkey -in "$MANUAL_CERT_DIR/$DOM_IMPORT.key" -pubout 2>/dev/null | sha256sum | awk '{print $1}')"

wait_cert_row "$DOM_IMPORT" 10
cp "$PAYLOADS/certlist-$DOM_IMPORT.json" "$PAYLOADS/certlist-import.json"
expect_eq "导入记录的来路" "manual" "$(csget "$DOM_IMPORT" source)"
expect_eq "导入记录的私钥保管方" "secret_store" "$(csget "$DOM_IMPORT" key_storage)"
expect_eq "导入记录不是 staging" "False" "$(csget "$DOM_IMPORT" staging)"
CERT_B_ID=$(csget "$DOM_IMPORT" id)
CERT_B_FP=$(csget "$DOM_IMPORT" fingerprint)

# 8e. 绑定导入的证书 → 443 上用这张自签证书服务。
expect_eq "登记站点 B（$DOM_IMPORT）" "201" \
  "$(create_site b "{\"domain\":\"$DOM_IMPORT\",\"upstream_target_id\":\"local_$UPSTREAM_PORT\",\"upstream_port\":$UPSTREAM_PORT}")"
SITE_B=$(sget b '["id"]')
expect_eq "给站点 B 绑定导入的证书" "200" \
  "$(patch_site b "$SITE_B" "{\"version\":$(sget b '["version"]'),\"certificate_id\":\"$CERT_B_ID\"}")"
run_cert_action proxy.apply "{\"site_id\":\"$SITE_B\"}" apply-b
expect_eq "站点 B 应用的终态" "succeeded" "$JOB_STATE"
wait_applied b "$SITE_B" applied
expect_eq "站点 B 的 applied 状态" "applied" "$(sget b '["applied"]["state"]')"
# 站点 B 是这一轮新写下去的第二份 443 配置：reload 之后还得等新 worker 把它接上，
# 否则 SNI 找不到 B 的 server 块，会落到默认块上——第一遍读到的就是站点 A 的证书。
SERVED_B_FP=$(served_fp_upper_wait "$DOM_IMPORT" "$(printf '%s' "$CERT_B_FP" | tr 'a-f' 'A-F')")
expect_eq "站点 B 服务的就是导入的证书" \
  "$(printf '%s' "$CERT_B_FP" | tr 'a-f' 'A-F')" "$SERVED_B_FP"
HTTPS_B=$(https_body_wait "$DOM_IMPORT" "$CERTS_DIR/p103-ca.pem" "$MARKER")
expect_eq "站点 B 在 443 上服务上游页面" "$MARKER" "$HTTPS_B"

echo
echo "== 9. 到期信息：valid / expiring / 过期被挡 =="
mint_leaf "$DOM_EXPIRING" 20
expect_eq "上传 20 天有效期的证书" "201" "$(upload_cert "$CERTS_DIR/$DOM_EXPIRING-chain.pem" upload-expiring)"
UPLOAD_EXP=$(jget "$PAYLOADS/upload-expiring.json" 'd["data"]["certificate_upload_id"]')
expect_eq "上传 20 天证书的私钥" "201" "$(put_secret tls_private_key "$CERTS_DIR/$DOM_EXPIRING.key")"
KEY_REF_EXP=$(jget "$WORK/secret.json" 'd["data"]["secret_ref"]')
run_cert_action certificate.import \
  "{\"domain\":\"$DOM_EXPIRING\",\"certificate_upload_id\":\"$UPLOAD_EXP\",\"private_key_secret_ref\":\"$KEY_REF_EXP\"}" import-expiring
expect_eq "导入 20 天证书的终态" "succeeded" "$JOB_STATE"
wait_cert_row "$DOM_EXPIRING" 10
cp "$PAYLOADS/certlist-$DOM_EXPIRING.json" "$PAYLOADS/certlist-expiring.json"
expect_eq "20 天有效期的证书报 expiring" "expiring" "$(csget "$DOM_EXPIRING" renewal_state)"

# 已经过期的证书在预检就被挡下：装上去只会让浏览器报警。
#
# 造这张证书不能用 `openssl req -x509 -not_before/…`：那是 OpenSSL 3.2 才有的参数，
# 24.04 上是 3.0.13（本脚本第一遍运行正是在这里退化成 NOT RUN）。certbot 依赖
# python 的 cryptography，所以这里用它来签一张“上个月就过期”的叶子证书——依赖是
# 真实存在的（没有它就没有 certbot），不是额外要求。
if python3 - "$CERTS_DIR" "$DOM_DEAD" <<'PY'
import datetime, os, sys
from cryptography import x509
from cryptography.hazmat.primitives import hashes, serialization
from cryptography.hazmat.primitives.asymmetric import rsa
from cryptography.x509.oid import NameOID

out_dir, domain = sys.argv[1], sys.argv[2]
with open(os.path.join(out_dir, "p103-ca.pem"), "rb") as fh:
    ca = x509.load_pem_x509_certificate(fh.read())
with open(os.path.join(out_dir, "p103-ca.key"), "rb") as fh:
    ca_key = serialization.load_pem_private_key(fh.read(), None)

key = rsa.generate_private_key(public_exponent=65537, key_size=2048)
now = datetime.datetime.now(datetime.timezone.utc)
cert = (
    x509.CertificateBuilder()
    .subject_name(x509.Name([x509.NameAttribute(NameOID.COMMON_NAME, domain)]))
    .issuer_name(ca.subject)
    .public_key(key.public_key())
    .serial_number(x509.random_serial_number())
    .not_valid_before(now - datetime.timedelta(days=400))
    .not_valid_after(now - datetime.timedelta(days=35))
    .add_extension(x509.SubjectAlternativeName([x509.DNSName(domain)]), critical=False)
    .add_extension(x509.BasicConstraints(ca=False, path_length=None), critical=True)
    .add_extension(x509.KeyUsage(
        digital_signature=True, content_commitment=False, key_encipherment=True,
        data_encipherment=False, key_agreement=False, key_cert_sign=False,
        crl_sign=False, encipher_only=False, decipher_only=False), critical=True)
    .sign(ca_key, hashes.SHA256())
)
with open(os.path.join(out_dir, domain + ".key"), "wb") as fh:
    fh.write(key.private_bytes(serialization.Encoding.PEM,
                               serialization.PrivateFormat.TraditionalOpenSSL,
                               serialization.NoEncryption()))
with open(os.path.join(out_dir, domain + "-chain.pem"), "wb") as fh:
    fh.write(cert.public_bytes(serialization.Encoding.PEM))
    fh.write(ca.public_bytes(serialization.Encoding.PEM))
PY
then
  if openssl x509 -in "$CERTS_DIR/$DOM_DEAD-chain.pem" -noout -checkend 0 >/dev/null 2>&1; then
    notrun "过期证书的预检门禁" "造出来的证书还没过期（夹具问题，不是产品问题）"
  else
    pass "生成一张已经过期的证书（$DOM_DEAD）"
    expect_eq "上传过期证书" "201" "$(upload_cert "$CERTS_DIR/$DOM_DEAD-chain.pem" upload-dead)"
    UPLOAD_DEAD=$(jget "$PAYLOADS/upload-dead.json" 'd["data"]["certificate_upload_id"]')
    expect_eq "上传过期证书的私钥" "201" "$(put_secret tls_private_key "$CERTS_DIR/$DOM_DEAD.key")"
    KEY_REF_DEAD=$(jget "$WORK/secret.json" 'd["data"]["secret_ref"]')
    PRE_CODE=$(preflight certificate.import \
      "{\"domain\":\"$DOM_DEAD\",\"certificate_upload_id\":\"$UPLOAD_DEAD\",\"private_key_secret_ref\":\"$KEY_REF_DEAD\"}")
    api_save preflight-import-dead
    expect_eq "过期证书的预检状态码" "200" "$PRE_CODE"
    expect_eq "过期证书的预检不放行" "False" "$(pallowed)"
    expect_contains "阻塞原因指出证书已过期" "CERTIFICATE_EXPIRED" "$(pall)"
    expect_eq "过期证书没有被写进证书表" "0" \
      "$(dbq "SELECT COUNT(*) FROM certificates WHERE domain = '$DOM_DEAD'")"
  fi
else
  notrun "过期证书的预检门禁" "python 的 cryptography 不可用，造不出一张已经过期的证书"
fi

echo
echo "== 10. 私钥保护：所有出口都搜一遍 =="
KEY_LINE=$(sed -n '2p' "$CERTS_DIR/$DOM_IMPORT.key")
if [ -n "$KEY_LINE" ]; then
  pass "取到一把真实私钥的内容片段（用来当搜索词）"
else
  fail "取到私钥内容片段" "私钥文件为空"
fi

for target in "$WEB_LOG" "$AGENT_LOG" "$WORK/logs/init-key.log"; do
  n=$(loggrep "$target" "PRIVATE KEY")
  if [ "$n" = "0" ]; then pass "日志里没有 PRIVATE KEY：$(basename "$target")"; else
    fail "日志里出现 PRIVATE KEY：$target" "$n 行"
  fi
done
if [ -n "$KEY_LINE" ]; then
  n=$(loggrep "$AGENT_LOG" "$KEY_LINE")
  expect_eq "代理日志里没有私钥正文" "0" "$n"
  n=$(loggrep "$WEB_LOG" "$KEY_LINE")
  expect_eq "Web 日志里没有私钥正文" "0" "$n"
fi

for table_cols in \
  "certificates:id,domain,key_path,cert_path,key_secret_ref,acme_email,last_error" \
  "preflights:normalized_params,target_json" \
  "jobs:result_json,error_json" \
  "job_events:redacted_payload" \
  "audit_events:redacted_details"; do
  table=${table_cols%%:*}
  cols=${table_cols#*:}
  hit=$(db_search "$table" "$cols" "PRIVATE KEY")
  if [ -z "$hit" ]; then pass "面板库里没有私钥内容：$table"; else fail "面板库里出现私钥内容：$table" "$hit"; fi
done
if [ -n "$KEY_LINE" ]; then
  hit=$(db_search "preflights" "normalized_params,target_json" "$KEY_LINE")
  expect_eq "预检参数里没有私钥正文" "" "$hit"
  hit=$(db_search "jobs" "result_json,error_json" "$KEY_LINE")
  expect_eq "任务结果里没有私钥正文" "" "$hit"
fi

# 私钥路径也算泄漏：证书接口的响应里一个都不许有。
expect_eq "证书接口响应里没有 key_path" "0" "$(cert_body_leak "key_path")"
expect_eq "证书接口响应里没有 cert_path" "0" "$(cert_body_leak "cert_path")"
expect_eq "证书接口响应里没有 privkey" "0" "$(cert_body_leak "privkey")"
expect_eq "证书接口响应里没有手动证书目录" "0" "$(cert_body_leak "$MANUAL_CERT_DIR")"
expect_eq "证书接口响应里没有 certbot 目录" "0" "$(cert_body_leak "/etc/letsencrypt")"
expect_eq "证书接口响应里没有 key_secret_ref" "0" "$(cert_body_leak "key_secret_ref")"

# 任务报告里出现落地路径是**有意的**（CONTRACTS §9.3(7)：运维要知道文件写到了哪儿），
# 把它写成一条断言，是为了让“路径只出现在这里”这件事可查——将来有人把这些字段
# 加进证书 DTO，上面的六条会立刻红。
if grep -q -- "key_path" "$PAYLOADS/job-issue-a.json" 2>/dev/null; then
  pass "任务报告里带着落地路径（有意的，且只有路径没有内容）"
else
  fail "任务报告里带着落地路径" "job-issue-a 里没有 key_path，与 §9.3(7) 的说明不一致"
fi

LEAK=$(bodygrep "$PAYLOADS" "PRIVATE KEY")
expect_eq "所有响应体里没有 PRIVATE KEY" "0" "$LEAK"
if [ -n "$KEY_LINE" ]; then
  LEAK=$(bodygrep "$PAYLOADS" "$KEY_LINE")
  expect_eq "所有响应体里没有私钥正文" "0" "$LEAK"
fi

echo
echo "== 11. 审计留痕 =="
AUDIT_UPLOAD=$(dbq "SELECT COUNT(*) FROM audit_events WHERE action = 'certificate.upload'")
if [ "${AUDIT_UPLOAD:-0}" -ge 1 ]; then pass "上传留下审计（${AUDIT_UPLOAD} 条）"; else
  fail "上传留下审计" "0 条"
fi
AUDIT_JOBS=$(dbq "SELECT COUNT(*) FROM audit_events WHERE action = 'job.submit' AND job_id IS NOT NULL")
if [ "${AUDIT_JOBS:-0}" -ge 1 ]; then pass "证书任务留下审计（${AUDIT_JOBS} 条）"; else
  fail "证书任务留下审计" "0 条"
fi
AUDIT_PREFLIGHT=$(dbq "SELECT COUNT(*) FROM audit_events WHERE action = 'preflight.expand' OR action = 'preflight.create'")
note "预检相关审计 ${AUDIT_PREFLIGHT:-0} 条（动作名不在本轮断言范围内）"
AUDIT_PRIV=$(dbq "SELECT COUNT(*) FROM audit_events WHERE redacted_details LIKE '%PRIVATE KEY%'")
expect_eq "审计里没有私钥内容" "0" "${AUDIT_PRIV:-0}"

echo
echo "== 12. 收尾自证 =="
cleanup
trap - EXIT INT TERM
expect_eq "收尾：托管配置已清理" "" "$(managed_conf)"
expect_eq "收尾：nginx -t 通过" "ok" "$(nginx_t_ok)"
expect_eq "收尾：/etc/hosts 已还原" "$(sha256sum "$WORK/hosts.orig" | awk '{print $1}')" \
  "$(sha256sum /etc/hosts | awk '{print $1}')"
expect_eq "收尾：nat 规则已删除" "" "$(iptables -t nat -S OUTPUT 2>/dev/null | grep 5002 || true)"

echo
echo "== 不覆盖项（NOT RUN） =="
notrun "真实 Let's Encrypt 签发" "会消耗速率限额且需要公网域名与 80 端口；本脚本只连本地 Pebble"
notrun "正式签发（staging=false）" "同上：正式地址那一条由配置默认值与“必须显式选择”共同保证"
notrun "真实公网 DNS / 外部可达性" "用 /etc/hosts 与 nat 重定向代替"
notrun "浏览器信任表现" "VM 没有浏览器；staging 证书在浏览器里的告警未验证"
notrun "timer 自动续期" "P12 才接入；本脚本只验证续期动作本身"
notrun "DNS-01 / 通配符证书" "产品只做 HTTP-01"
notrun "证书吊销与 OCSP" "不在 P10.3 范围"

echo
printf '通过 %d，失败 %d，未覆盖 %d\n' "$PASSED" "$FAILED" "$NOTRUN"
if [ "$FAILED" -gt 0 ]; then
  printf '失败项：%s\n' "$FAILED_NAMES"
  exit 1
fi
exit 0
