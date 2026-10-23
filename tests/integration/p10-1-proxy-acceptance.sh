#!/bin/sh
# P10.1 Linux 验收脚本：Nginx 发现、域名冲突判断、站点定义登记。
#
# 这一份要证明的不是“接口返回 200”，而是下面这些容易被“写完了”骗过去的事实：
#
#   1) **Nginx 现状是真读出来的**：版本、单元状态、nginx.conf、conf.d/*.conf、
#      sites-enabled/*（含符号链接）都在；sites-enabled/default 是符号链接，
#      它只能出现一次——按解析后的真实路径去重，否则同一份文件里的 server_name
#      会被算两遍，证据也会指两次。
#   2) **冲突结论带证据，unknown 不是 clear**：普通 server_name 命中 → taken（文件+行，
#      指向 server_name 那一行而不是 server 块的开始）；`*.x.com` 命中子域 → taken；
#      正则 server_name → unknown（判不了，可能占住）；解析失败的文件 → unknown。
#   3) **判不了就拒绝写入**：上面两种 unknown 下 POST /proxies 都是 409，库里一条记录都
#      不留下。代理停掉时是 503 AGENT_UNAVAILABLE，与此同时 GET /proxies 仍是 200，
#      只是 nginx.read_error 写明读不到——把“读不到”说成“没有冲突”会直接导致覆盖。
#   4) **面板端口永远是禁区**：upstream_port 等于这台机器上 Web 真正监听的端口 → 400。
#      这条规则用的是真实端口，不是写死的 9443。
#   5) **域名归一化真的生效**：大写与末尾的点被吃掉，同一个域名建不出第二条记录。
#   6) **secret_ref 只进不出**：basic_auth_users 的响应里只有 name；审计里也没有它。
#   7) **P10.1 一个字节都没写进 /etc/nginx**：整段写操作前后对 /etc/nginx 做内容摘要，
#      两次必须完全一致；`nginx -t` 在收尾时仍然通过。
#
# 用法（root）：
#   sudo BIN_DIR=/var/tmp/upanel-bin sh tests/integration/p10-1-proxy-acceptance.sh
#
# 环境变量：PORT（默认 19495）、WORK、BIN_DIR。
#
# 退出码：0 全部通过；1 有失败项；2 环境不满足（非 root / 二进制缺失 / 没有 nginx）。
#
# 关于夹具的破坏性：本脚本要在 /etc/nginx/conf.d 里临时放几份**故意坏掉**的配置
# （未闭合的 server 块），用来验“解析失败必须让结论变成 unknown”。它们只被读取，
# 期间绝不 reload nginx；用完立刻删除，收尾时再删一次并 `nginx -t` 自证。
# 也就是说：**不要在没有把握的机器上跑它**，它适合一次性的验收虚拟机。
#
# 明确不覆盖（NOT RUN，原因写在这里，不在脚本里假装通过）：
#   - **配置生成/原子写入/nginx -t 门禁/reload/失败恢复**：那是 P10.2 的 proxy.apply，
#     本阶段没有实现，脚本不验不存在的东西。
#   - **证书与 HTTPS**：P10.3。
#   - **基本认证口令的实际校验**：需要 P10.2 生成 htpasswd 之后才有意义。
#   - **浏览器渲染**：VM 上没有浏览器。

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
if ! command -v nginx >/dev/null 2>&1; then
  printf '本机没有 nginx，P10.1 的验收无法进行\n' >&2
  exit 2
fi

WORK=${WORK:-/var/tmp/upanel-accept-p10-1}
PAYLOADS=$WORK/payloads
RUN_DIR=/run/upanel
SOCK=$RUN_DIR/agent.sock
SYS_CFG=/etc/upanel/upanel.json
AGENT_DB_DIR=/var/lib/upanel-agent
AGENT_DB=$AGENT_DB_DIR/agent.db
KEY_FILE=/etc/upanel/agent.key
WEB_DB_DIR=$WORK/webdata
WEB_DB=$WEB_DB_DIR/upanel.db
PORT=${PORT:-19495}
BASE="http://127.0.0.1:$PORT"
ORIGIN="$BASE"
WEB_USER=upanel-web
PASSWORD='vm-correct-horse-9'
VIEWER_PASSWORD='vm-viewer-horse-4'

PROXIES=/api/v1/proxies
CONFLICT="$PROXIES/conflict"
NGX_FACTS="$PROXIES/nginx"
USERS=/api/v1/users

# 夹具：conf.d 里的三份“别人的配置” + 一份故意坏掉的文件（用完立刻删）。
NGINX_CONF_D=/etc/nginx/conf.d
NGINX_SITES_ENABLED=/etc/nginx/sites-enabled
FIX_TAKEN=$NGINX_CONF_D/p101-taken.conf
FIX_WILD=$NGINX_CONF_D/p101-wild.conf
FIX_REGEX=$NGINX_CONF_D/p101-regex.conf
FIX_BROKEN=$NGINX_CONF_D/p101-broken.conf

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

# jget 用 python3 读 JSON：这台机器上没有 jq（与 P05—P09.5 的取舍一致）。
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

# nginx_digest 是 /etc/nginx 全部文件内容的摘要（路径 + 内容）。
#
# 用它证明“面板没有动过 Nginx”：两次摘要相等是内容级证据，不是“我们没调用写函数”。
nginx_digest() {
  find /etc/nginx -type f -print0 2>/dev/null | sort -z | xargs -0 sha256sum 2>/dev/null | sha256sum |
    awk '{print $1}'
}

cleanup() {
  for _pid in "${WEB_PID:-}" "${AGENT_PID:-}"; do
    if [ -n "$_pid" ] && kill -0 "$_pid" 2>/dev/null; then
      kill -TERM "$_pid" 2>/dev/null
      wait "$_pid" 2>/dev/null
    fi
  done
  for f in "$FIX_TAKEN" "$FIX_WILD" "$FIX_REGEX" "$FIX_BROKEN"; do
    rm -f "$f"
  done
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
emsg() { jget "$WORK/resp.json" 'd["error"]["message"]'; }
dget() { jget "$WORK/resp.json" "d[\"data\"]$1"; }
dstr() { jget "$WORK/resp.json" 'json.dumps(d.get("data"), ensure_ascii=False)'; }

# create_site <载荷名> <body>：回显状态码；标识写进 $PAYLOADS/site-<名>.json。
create_site() {
  _code=$(api "$JAR" "$CSRF" POST "$PROXIES" "$2")
  cp "$WORK/resp.json" "$PAYLOADS/site-$1.json" 2>/dev/null
  printf '%s' "$_code"
}

# sget <载荷名> <字段路径>：从已保存的响应里取 data 下的字段。
sget() { jget "$PAYLOADS/site-$1.json" "d[\"data\"]$2"; }

# conflict_of <域名> [exclude_id]：把冲突结论写进 $WORK/conflict.json，回显 state。
conflict_state() {
  _url="$CONFLICT?domain=$1"
  [ -n "${2:-}" ] && _url="$_url&exclude_id=$2"
  api "$JAR" "$CSRF" GET "$_url" >/dev/null
  cp "$WORK/resp.json" "$WORK/conflict.json" 2>/dev/null
  jget "$WORK/conflict.json" 'd["data"]["state"]'
}

# 站点数（独立证据：直接读库，不信接口说的话）。
site_count() { dbq 'SELECT COUNT(*) FROM proxies'; }

mkdir -p "$WORK/logs" "$PAYLOADS" "$WEB_DB_DIR"
# 库所在目录必须属于 Web 用户：sqlite 要在旁边建 -wal/-shm，目录不可写就是
# “attempt to write a readonly database”，而那个报错看起来像库坏了。
chown "$WEB_USER" "$WEB_DB_DIR"

echo "== 1. 配置与启动（代理 root、Web 非 root） =="
install -d -m 0755 /etc/upanel
install -d -m 0700 "$AGENT_DB_DIR"
install -d -m 0755 "$RUN_DIR"
# 只清 Web 库：代理库里的卷与秘密登记是真机状态，脚本不销毁。
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
expect_eq "数据库迁移到第 9 版（0009_proxies）" "9" "$(dbq 'SELECT MAX(version) FROM schema_migrations')"

JAR=$WORK/admin.jar
expect_eq "管理员登录" "200" "$(login admin "$PASSWORD" "$JAR" login-admin)"
CSRF=$(csrf_of "$WORK/login-admin.json")
expect_ne "拿到 CSRF 令牌" "" "$CSRF"

echo
echo "== 2. Nginx 现状：真的读出来了 =="
# 夹具：两份“别人的配置”，每份一个 server 块。端口只是占位，本阶段不会 reload，
# 因此不会真的去 bind。
#
# 这里故意不一次放齐：正则 server_name 会让**所有**域名的结论变成 unknown（判不了），
# 所以它必须单独一段出现，否则前面的 clear/taken 断言会被它污染成 unknown，
# 看起来像“崩了”，其实是被自己的夹具挡住的。
cat >"$FIX_TAKEN" <<'CONF'
server {
  listen 8081;
  server_name p101-taken.example.com;
}
CONF
cat >"$FIX_WILD" <<'CONF'
server {
  listen 8082;
  server_name *.p101wild.example.com;
}
CONF
chmod 0644 "$FIX_TAKEN" "$FIX_WILD"
# 独立证据：文件确实在磁盘上、内容确实是这几行。
expect_eq "夹具 taken 配置就位" "1" "$(grep -c 'p101-taken.example.com' "$FIX_TAKEN")"
expect_eq "sites-enabled/default 是符号链接（去重要靠解析真实路径）" "symlink" \
  "$(test -L "$NGINX_SITES_ENABLED/default" && echo symlink || echo regular)"

NGX_CODE=$(api "$JAR" "$CSRF" GET "$NGX_FACTS")
cp "$WORK/resp.json" "$PAYLOADS/nginx-facts.json"
expect_eq "GET /proxies/nginx 状态码" "200" "$NGX_CODE"
expect_eq "nginx 可用" "True" "$(jget "$PAYLOADS/nginx-facts.json" 'd["data"]["state"]["available"]')"
expect_eq "配置目录不是“不存在”" "False" "$(jget "$PAYLOADS/nginx-facts.json" 'd["data"]["state"]["config_missing"]')"
NGX_VER=$(jget "$PAYLOADS/nginx-facts.json" 'd["data"]["state"]["version"]')
expect_ne "读到 nginx 版本" "" "$NGX_VER"
note "版本：$NGX_VER"
expect_eq "读到 nginx.service 的状态" "active" "$(jget "$PAYLOADS/nginx-facts.json" 'd["data"]["state"]["unit_state"]')"
expect_eq "server_names 里有夹具域名" "True" \
  "$(jget "$PAYLOADS/nginx-facts.json" 'any(n=="p101-taken.example.com" for n in d["data"]["server_names"])')"
# 符号链接只读一次：sites-enabled/default 指向 sites-available/default，
# 同一个真实文件不能被算成两份，否则里面的 server_name 会被数两遍、证据会指两次。
expect_eq "同一份配置只登记一次（default 只出现一条）" "1" \
  "$(jget "$PAYLOADS/nginx-facts.json" 'len([f for f in d["data"]["files"] if f["path"].endswith("/default")])')"
expect_eq "文件路径互不重复" "True" \
  "$(jget "$PAYLOADS/nginx-facts.json" 'len({f["path"] for f in d["data"]["files"]}) == len(d["data"]["files"])')"
expect_eq "files 里出现过 sites-enabled/default 或 nginx.conf" "True" \
  "$(jget "$PAYLOADS/nginx-facts.json" 'any(f["path"].endswith("/nginx.conf") or "/sites-enabled/" in f["path"] for f in d["data"]["files"])')"
expect_eq "读到的文件都带 server 块结构（至少一个块）" "True" \
  "$(jget "$PAYLOADS/nginx-facts.json" 'any(len(f["servers"])>0 for f in d["data"]["files"])')"

echo
echo "== 3. 域名冲突判断 =="
expect_eq "没人占的域名 → clear" "clear" "$(conflict_state shop.p101free.example.com)"
expect_eq "…clear 时没有 matches" "0" "$(jget "$WORK/conflict.json" 'len(d["data"]["matches"])')"
expect_eq "现有配置声明的域名 → taken" "taken" "$(conflict_state p101-taken.example.com)"
expect_eq "…证据指向 server_name 那一行（不是 server 块开头）" "3" \
  "$(python3 - "$WORK/conflict.json" <<'PY'
import json, sys
d = json.load(open(sys.argv[1], encoding="utf-8"))["data"]["matches"]
print(d[0]["line"] if d else "")
PY
)"
expect_eq "…证据带文件与原文" "/etc/nginx/conf.d/p101-taken.conf|p101-taken.example.com" \
  "$(python3 - "$WORK/conflict.json" <<'PY'
import json, sys
d = json.load(open(sys.argv[1], encoding="utf-8"))["data"]["matches"]
print("%s|%s" % (d[0]["file"], d[0]["name"]) if d else "")
PY
)"
expect_eq "通配符 *.x 会占住子域 → taken" "taken" "$(conflict_state a.p101wild.example.com)"
expect_eq "…通配符的原文被如实带出" "*.p101wild.example.com" \
  "$(python3 - "$WORK/conflict.json" <<'PY'
import json, sys
d = json.load(open(sys.argv[1], encoding="utf-8"))["data"]["matches"]
print(d[0]["name"] if d else "")
PY
)"
expect_eq "…通配符不占住它自己的上级域（*.x 不等于 x）" "clear" "$(conflict_state p101wild.example.com)"

# 正则 server_name：认不出它到底占哪个域名，所以**任何**域名的结论都只能是 unknown。
# 这一段单独放，测完立刻删，免得把后面的 clear 断言全染成 unknown。
cat >"$FIX_REGEX" <<'CONF'
server {
  listen 8083;
  server_name ~^re\d+\.p101regex\.example\.com$;
}
CONF
chmod 0644 "$FIX_REGEX"
expect_eq "有正则 server_name 时目标域名 → unknown" "unknown" "$(conflict_state re1.p101regex.example.com)"
expect_eq "…unknown 有理由" "True" \
  "$(python3 - "$WORK/conflict.json" <<'PY'
import json, sys
print("True" if json.load(open(sys.argv[1], encoding="utf-8"))["data"]["reason"] else "False")
PY
)"
expect_eq "…同一条正则也把无关域名压成 unknown（判不了就是判不了）" "unknown" \
  "$(conflict_state shop.p101free.example.com)"
expect_eq "…正则在场时连无关域名也拒绝写入（409）" "409" \
  "$(create_site regexblock '{"domain":"shop.p101free.example.com","upstream_target_id":"local_8090","upstream_port":8090}')"
expect_eq "…库里没有留下记录" "0" "$(site_count)"
rm -f "$FIX_REGEX"
expect_eq "删掉正则后同一域名恢复 clear（正对照）" "clear" "$(conflict_state shop.p101free.example.com)"

# 解析失败的文件：里面写了什么不知道，所以同样压成 unknown。
cat >"$FIX_BROKEN" <<'CONF'
server {
  server_name p101-broken.example.com;
CONF
chmod 0644 "$FIX_BROKEN"
expect_eq "坏文件的结论 → unknown" "unknown" "$(conflict_state p101-broken.example.com)"
expect_eq "…broken_files 点出这份文件" "True" \
  "$(jget "$WORK/conflict.json" 'any("p101-broken.conf" in f for f in d["data"]["broken_files"])')"

# 坏文件同理挡住写入。
expect_eq "坏文件在场时登记被拒绝（409）" "409" \
  "$(create_site broken '{"domain":"p101-broken.example.com","upstream_target_id":"local_8090","upstream_port":8090}')"
expect_eq "…库里没有留下记录" "0" "$(site_count)"
rm -f "$FIX_BROKEN"

# 正对照：同一份事实去掉坏文件之后，这个域名就是 clear 了。
expect_eq "删掉坏文件后同一域名 → clear（证明拦住它的确实是那份文件）" "clear" \
  "$(conflict_state p101-broken.example.com)"

# 到这里所有夹具清掉，之后面板的任何写操作都必须是“连 /etc/nginx 都没碰”。
rm -f "$FIX_TAKEN" "$FIX_WILD" "$FIX_REGEX" "$FIX_BROKEN"
DIGEST_BEFORE=$(nginx_digest)
expect_ne "拿到 /etc/nginx 的内容摘要" "" "$DIGEST_BEFORE"

echo
echo "== 4. 站点登记：校验、面板端口、归一化、秘密 =="
expect_eq "上游端口等于面板端口 → 400" "400" \
  "$(create_site panelport "{\"domain\":\"p101-port.example.com\",\"upstream_target_id\":\"local_8090\",\"upstream_port\":$PORT}")"
expect_contains "…错误说明指向面板端口" "管理面板" "$(emsg)"
expect_eq "通配符域名被拒绝（初期不支持）" "400" \
  "$(create_site wild '{"domain":"*.p101w.example.com","upstream_target_id":"local_8090","upstream_port":8090}')"
expect_eq "带协议的域名被拒绝" "400" \
  "$(create_site scheme '{"domain":"http://p101s.example.com","upstream_target_id":"local_8090","upstream_port":8090}')"
expect_eq "非法目标标识被拒绝" "400" \
  "$(create_site target '{"domain":"p101t.example.com","upstream_target_id":"../etc/passwd","upstream_port":8090}')"
expect_eq "cidr 模式不给网段被拒绝" "400" \
  "$(create_site cidrempty '{"domain":"p101c.example.com","upstream_target_id":"local_8090","upstream_port":8090,"access_mode":"cidr"}')"
expect_eq "public 模式给了网段也是错误（不静默存下）" "400" \
  "$(create_site cidrpub '{"domain":"p101cp.example.com","upstream_target_id":"local_8090","upstream_port":8090,"access_mode":"public","allowed_cidrs":["10.0.0.0/8"]}')"
expect_eq "basic_auth 模式重复用户名被拒绝" "400" \
  "$(create_site dup '{"domain":"p101d.example.com","upstream_target_id":"local_8090","upstream_port":8090,"access_mode":"basic_auth","basic_auth_users":[{"name":"ops","secret_ref":"sec_0123456789abcdef0123456789abcdef"},{"name":"ops","secret_ref":"sec_0123456789abcdef0123456789abcdef"}]}')"
expect_eq "以上全部被拒绝后库里仍是 0 条" "0" "$(site_count)"

# 正常登记：大写 + 末尾的点必须被归一化，默认值必须与 nginx 自己的默认一致。
expect_eq "登记成功" "201" \
  "$(create_site ok '{"domain":"P101-OK.Example.COM.","upstream_target_id":"local_8090","upstream_port":8090,"websocket_enabled":true}')"
SITE_ID=$(sget ok '["id"]')
case "$SITE_ID" in
  site_*) pass "站点标识形状正确（$SITE_ID）" ;;
  *) fail "站点标识形状正确" "$SITE_ID" ;;
esac
expect_eq "域名被归一化（小写、去掉末尾的点）" "p101-ok.example.com" "$(sget ok '["domain"]')"
expect_eq "默认 max_body_bytes = 1 MiB" "1048576" "$(sget ok '["max_body_bytes"]')"
expect_eq "默认 read_timeout_seconds = 60" "60" "$(sget ok '["read_timeout_seconds"]')"
expect_eq "默认访问模式 public" "public" "$(sget ok '["access_mode"]')"
expect_eq "默认上游协议 http" "http" "$(sget ok '["upstream_scheme"]')"
expect_eq "首次登记 enabled=true、version=1" "True|1" "$(sget ok '["enabled"]')|$(sget ok '["version"]')"
expect_eq "没有关联应用时 app_id 是 null" "None" "$(sget ok '["app_id"]')"
expect_eq "整组接口没有返回任何“已生效”字段" "0" \
  "$(dstr | python3 -c 'import sys,json
d=json.load(sys.stdin)
bad=[k for k in d if k.startswith("applied") or k in ("config_path","config_hash")]
print(len(bad))')"

# 同一个域名（换个大小写）建不出第二条：库层唯一 + 冲突判断都要挡住。
expect_eq "同一域名重复登记被拒绝" "409" \
  "$(create_site dupdom '{"domain":"p101-ok.example.com","upstream_target_id":"local_8090","upstream_port":8090}')"
expect_eq "…库里仍只有 1 条" "1" "$(site_count)"

# basic_auth：响应里只有 name，没有 secret_ref；审计里也不能出现。
SECRET=sec_0123456789abcdef0123456789abcdef
expect_eq "登记 basic_auth 站点" "201" \
  "$(create_site auth "{\"domain\":\"p101-auth.example.com\",\"upstream_target_id\":\"local_8090\",\"upstream_port\":8090,\"access_mode\":\"basic_auth\",\"basic_auth_users\":[{\"name\":\"ops\",\"secret_ref\":\"$SECRET\"}]}")"
expect_not_contains "响应里没有 secret_ref" "$SECRET" "$(cat "$PAYLOADS/site-auth.json")"
expect_eq "响应里只有用户名" "ops" "$(python3 - "$PAYLOADS/site-auth.json" <<'PY'
import json, sys
u = json.load(open(sys.argv[1], encoding="utf-8"))["data"]["basic_auth_users"]
print(",".join(x["name"] for x in u) if u else "")
PY
)"
expect_eq "审计里没有 secret_ref" "0" \
  "$(dbq "SELECT COUNT(*) FROM audit_events WHERE redacted_details LIKE '%$SECRET%'")"
# 到此刻有两次成功的 proxy.create（ok 与 auth），审计必须一条不少。
expect_eq "审计记下了两次 proxy.create" "2" \
  "$(dbq "SELECT COUNT(*) FROM audit_events WHERE action='proxy.create' AND outcome='success'")"
expect_eq "审计里写明 applied=false（P10.1 不写配置）" "2" \
  "$(dbq "SELECT COUNT(*) FROM audit_events WHERE action='proxy.create' AND redacted_details LIKE '%\"applied\":false%'")"

echo
echo "== 5. 修改与删除（乐观锁、冲突只看自己） =="
expect_eq "只改超时（域名不变）成功" "200" \
  "$(api "$JAR" "$CSRF" PATCH "$PROXIES/$SITE_ID" '{"version":1,"read_timeout_seconds":30}')"
expect_eq "…版本 +1、超时已改" "2|30" "$(dget '["version"]')|$(dget '["read_timeout_seconds"]')"
expect_eq "过期版本被拒绝（409）" "409" \
  "$(api "$JAR" "$CSRF" PATCH "$PROXIES/$SITE_ID" '{"version":1,"read_timeout_seconds":90}')"
expect_eq "…错误码是 VERSION_CONFLICT" "VERSION_CONFLICT" "$(ecode)"
expect_eq "空 patch 被拒绝（400）" "400" \
  "$(api "$JAR" "$CSRF" PATCH "$PROXIES/$SITE_ID" '{"version":2}')"
# 改成另一条面板记录已经在用的域名：面板自己的记录也算“已占用”，不然两个站点会
# 抢同一个 server_name，而这正是 P10.2 生成配置时最容易踩的一脚。
expect_eq "改成别的记录已用的域名 → 409" "409" \
  "$(api "$JAR" "$CSRF" PATCH "$PROXIES/$SITE_ID" '{"version":2,"domain":"p101-auth.example.com"}')"
expect_eq "…版本没有前进（还是 2）" "2" \
  "$(api "$JAR" "$CSRF" GET "$PROXIES/$SITE_ID" >/dev/null; dget '["version"]')"

# 冲突接口带 exclude_id 时，自己那条记录的域名不算冲突。
expect_eq "conflict 带 exclude_id 时自己是 clear" "clear" "$(conflict_state p101-ok.example.com "$SITE_ID")"

expect_eq "缺 version 的删除被拒绝（400）" "400" "$(api "$JAR" "$CSRF" DELETE "$PROXIES/$SITE_ID")"
expect_eq "版本不对的删除被拒绝（409）" "409" \
  "$(api "$JAR" "$CSRF" DELETE "$PROXIES/$SITE_ID" '{"version":1}')"
expect_eq "删除成功" "200" "$(api "$JAR" "$CSRF" DELETE "$PROXIES/$SITE_ID" '{"version":2}')"
expect_eq "…删除后详情 404" "404" "$(api "$JAR" "$CSRF" GET "$PROXIES/$SITE_ID")"

echo
echo "== 6. 权限与“没有代理”时的行为 =="
# viewer 越权：站点定义里有上游与访问策略，是侦察材料。
api "$JAR" "$CSRF" POST "$USERS" \
  "{\"username\":\"alice\",\"display_name\":\"alice\",\"role\":\"viewer\",\"initial_password\":\"$VIEWER_PASSWORD\"}" \
  >/dev/null
VIEWER_JAR=$WORK/viewer.jar
expect_eq "viewer 登录" "200" "$(login alice "$VIEWER_PASSWORD" "$VIEWER_JAR" login-viewer)"
VIEWER_CSRF=$(csrf_of "$WORK/login-viewer.json")
expect_eq "viewer 读站点列表 → 403" "403" "$(api "$VIEWER_JAR" "$VIEWER_CSRF" GET "$PROXIES")"
expect_eq "viewer 读 Nginx 现状 → 403" "403" "$(api "$VIEWER_JAR" "$VIEWER_CSRF" GET "$NGX_FACTS")"
expect_eq "viewer 登记站点 → 403" "403" \
  "$(api "$VIEWER_JAR" "$VIEWER_CSRF" POST "$PROXIES" '{"domain":"p101-v.example.com","upstream_target_id":"local_8090","upstream_port":8090}')"
expect_eq "未登录读站点列表 → 401" "401" \
  "$(curl -s -o /dev/null -w '%{http_code}' "$BASE$PROXIES" -H "Origin: $ORIGIN")"

# 停掉代理：列表照常（定义在库里），写入必须 503。
if stop_agent; then pass "代理已停止（模拟代理不可用）"; else fail "停止代理" ""; fi
expect_eq "没有代理时列表仍是 200" "200" "$(api "$JAR" "$CSRF" GET "$PROXIES")"
expect_eq "…nginx.read_error 写明读不到" "agent_unavailable" "$(dget '["nginx"]["read_error"]')"
expect_eq "…不把“读不到”说成“主机上没有 Nginx”" "False" "$(dget '["nginx"]["config_missing"]')"
BEFORE=$(site_count)
expect_eq "没有代理时登记被拒绝（503）" "503" \
  "$(create_site noagent '{"domain":"p101-noagent.example.com","upstream_target_id":"local_8090","upstream_port":8090}')"
expect_eq "…错误码 AGENT_UNAVAILABLE" "AGENT_UNAVAILABLE" "$(ecode)"
expect_eq "…库里没有留下记录" "$BEFORE" "$(site_count)"
expect_eq "没有代理时冲突判断 → 503" "503" "$(api "$JAR" "$CSRF" GET "$CONFLICT?domain=p101-x.example.com")"
if start_agent; then pass "代理已重新启动"; else fail "重启代理" "$(tail -5 "$AGENT_LOG" 2>/dev/null)"; fi
# 面板到代理是长连接池：代理刚回来时池里可能还存着死连接，给它几轮重连时间再断言。
RECOVERED=0
for _ in $(seq 1 25); do
  api "$JAR" "$CSRF" GET "$NGX_FACTS" >/dev/null
  if [ "$(dget '["state"]["read_error"]')" = "" ]; then RECOVERED=1; break; fi
  sleep 0.2
done
if [ "$RECOVERED" = "1" ]; then
  pass "代理回来后事实读取恢复"
else
  fail "代理回来后事实读取恢复" "$(dget '["state"]["read_error"]')"
fi
expect_eq "代理回来后登记恢复（201）" "201" \
  "$(create_site again '{"domain":"p101-again.example.com","upstream_target_id":"local_8090","upstream_port":8090}')"

echo
echo "== 7. 面板没有动过 Nginx =="
DIGEST_AFTER=$(nginx_digest)
expect_eq "/etc/nginx 内容摘要前后一致" "$DIGEST_BEFORE" "$DIGEST_AFTER"
if nginx -t >"$WORK/logs/nginx-t.log" 2>&1; then
  pass "nginx -t 仍然通过（配置没有被改坏）"
else
  fail "nginx -t 仍然通过" "$(tail -3 "$WORK/logs/nginx-t.log")"
fi
expect_eq "conf.d 里没有留下任何夹具" "0" \
  "$(ls "$NGINX_CONF_D" 2>/dev/null | grep -c '^p101-' || true)"
api "$JAR" "$CSRF" GET "$NGX_FACTS" >/dev/null
expect_eq "面板自己的配置文件数为 0（P10.1 不生成 upanel-*.conf）" "0" \
  "$(dget '["state"]["managed_file_count"]')"

echo
echo "== 汇总 =="
printf '通过 %d，失败 %d，未执行 %d\n' "$PASSED" "$FAILED" "$NOTRUN"
if [ "$FAILED" -ne 0 ]; then
  printf '失败项：%s\n' "$FAILED_NAMES"
  exit 1
fi
exit 0
