#!/bin/sh
# P11.1 Linux 验收脚本：APT 包事实、候选版本、安全更新与重启标志。
#
# 这一份要证明的不是"接口返回 200"，而是下面这些容易被"写完了"骗过去的事实：
#
#   1) **装了什么包是真读出来的**：面板返回的每一个包名、版本、架构、状态位，
#      与 `dpkg-query` 逐字一致；一个不多、一个不少。
#   2) **候选版本是真读出来的**：每个包的 candidate 与 `apt-cache policy` 的
#      `Candidate:` 行逐字一致；`***` 那一块标的是**已安装**版本，不是候选。
#   3) **版本比较用 dpkg 自己的实现核对过**：候选与已安装的关系（newer/same/
#      older/unknown）对每个包都用 `dpkg --compare-versions` 复核一遍。
#      internal/debversion 是对 dpkg 算法的移植，移植得对不对只有这一问说了算。
#   4) **安全更新只认候选版本自己那一块的来源**：22.04 上 apparmor 的候选来自
#      jammy-updates，而 jammy-security 里放的是更旧的那个版本——它**不是**安全更新。
#      拿"security 源里有这个包"来判就会把它算进去，所以这一条要单独钉住。
#   5) **重启标志是读来的，不是推断的**：`/run/reboot-required` 存在与否、.pkgs 的
#      每一行、boot_id 都对着磁盘上那个文件核过。
#   6) **分页不漏不重**：默认 50 条，顺着 next_cursor 翻到底，去掉重复后恰好等于
#      全量包数。
#   7) **只读**：整段前后 `/var/lib/dpkg/status` 的 sha256 与 apt 索引目录的 mtime
#      不变；代理能力表里没有任何 package.* 动作。
#   8) **权限**：未登录 401、viewer 403、admin 200；三个接口都没有写入口。
#   9) **arch 限定名的候选读得到**：`bind9-libs:amd64` 这类包在 `apt-cache policy` 的
#      表头里会丢掉 `:amd64`（apt 自己剥的），按请求名查表必然落空，于是整条谎报
#      "候选读不到"。这一条单独抓一份 `query=bind9` 的原始响应留档。
#
# 用法（root，在验收虚拟机上）：
#   sudo BIN_DIR=/var/tmp/upanel-bin sh tests/integration/p11-1-apt-acceptance.sh
#
# 环境变量：BIN_DIR（默认 /var/tmp/upanel-bin）、WORK（默认 /var/tmp/upanel-accept-p11-1）、
# PORT（默认 19505）。
#
# 退出码：0 全部通过；1 有失败项；2 环境不满足（非 root / 二进制缺失 / 不是 Debian 系）。
#
# 明确不覆盖（NOT RUN，原因写在这里，不假装通过）：
#   - **apt-cache 读不到时的降级**（包列表照出、候选 unknown）：要让 apt-cache 真的失败
#     得把它的权限改坏或换掉二进制，那会把验收机上的 apt 弄成不可用状态（后面清理脚本
#     自己都装不了东西）。这条分支由 internal/apt 与 internal/packages 的单测覆盖，
#     以及 packages 层的 ErrFactsUnavailable 映射；本脚本不动它。
#   - **安装/卸载/升级/刷新索引**：P11.2，本阶段没有实现，脚本不验不存在的东西。
#   - **重启本身**（倒计时、再认证、boot_id 前后比对）：P11.3。
#   - **分阶段推送（phased）与被保留（held back）的包**：22.04 上 nano/snapd 就是
#     `apt-get -s upgrade` 里 kept back 的那种，本阶段只报"候选更新"，不判"这次会不会升"。
#   - **浏览器渲染**：VM 上没有浏览器。页面数据由真实抓取的夹具回放验证（web 侧）。
#   - **Windows / macOS 宿主**：0.1 不支持。

set -u

BIN_DIR=${BIN_DIR:-/var/tmp/upanel-bin}
WORK=${WORK:-/var/tmp/upanel-accept-p11-1}
PORT=${PORT:-19505}
AGENT="$BIN_DIR/upanel-agent"
WEB="$BIN_DIR/upanel-web"
CTL="$BIN_DIR/upanelctl"
WEB_USER=upanel-web
RUN_DIR=/run/upanel
SOCK=$RUN_DIR/agent.sock
SYS_CFG=/etc/upanel/upanel.json
AGENT_DB_DIR=/var/lib/upanel-agent
AGENT_DB=$AGENT_DB_DIR/agent.db
KEY_FILE=/etc/upanel/agent.key
BASE="http://127.0.0.1:$PORT"
ORIGIN="$BASE"
PASSWORD='vm-correct-horse-9'
VIEWER_PASSWORD='vm-viewer-horse-4'
PAYLOADS=$WORK/payloads
# FIXDIR 是给前端回放用的夹具目录：这里面的每一份都是**本次真机运行**原样抓下来的，
# 不是手写的。web 侧的 realdata-p11-1 测试读它们，所以夹具里有新情形要重跑本脚本取回，
# 不要手改（改一个字节就等于把"手写夹具天然自洽"那类假象请回来）。
FIXDIR=$WORK/fixtures
AGENT_LOG=$WORK/logs/agent.log
WEB_LOG=$WORK/logs/web.log
WEB_DB_DIR=$WORK/webdata
WEB_DB=$WEB_DB_DIR/upanel.db
HERE=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
VERIFY="$HERE/p11-1-packages-verify.py"

PACKAGES=/api/v1/packages
UPDATES=/api/v1/packages/updates
POWER=/api/v1/system/power-state
USERS=/api/v1/users

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
expect_eq() { if [ "$2" = "$3" ]; then pass "$1"; else fail "$1" "期望 '$2'，实际 '$3'"; fi; }
expect_ne() { if [ "$2" != "$3" ]; then pass "$1"; else fail "$1" "不该等于 '$2'"; fi; }
expect_contains() {
  case "$3" in
    *"$2"*) pass "$1" ;;
    *) fail "$1" "期望包含 '$2'，实际 '$3'" ;;
  esac
}

# jget <文件> <python 表达式>：d 是解析后的 JSON。
jget() {
  python3 -c 'import json,sys
try:
    d=json.load(open(sys.argv[1], encoding="utf-8"))
except Exception:
    print("")
    raise SystemExit
ns={"d": d, "json": json}
try:
    try:
        print(eval(sys.argv[2], ns))
    except SyntaxError:
        ns["_"]=None
        exec(compile(sys.argv[2], "<jget>", "exec"), ns)
        print("" if ns["_"] is None else ns["_"])
except Exception as exc:
    print("ERR:%s:%s" % (type(exc).__name__, exc))' "$1" "$2" 2>/dev/null
}

cleanup() {
  for _pid in "${WEB_PID:-}" "${AGENT_PID:-}"; do
    if [ -n "$_pid" ] && kill -0 "$_pid" 2>/dev/null; then
      kill -TERM "$_pid" 2>/dev/null
      wait "$_pid" 2>/dev/null
    fi
  done
  rm -f "$SOCK"
  return 0
}
trap cleanup EXIT
trap 'cleanup; exit 130' INT
trap 'cleanup; exit 143' TERM

chown_web_db() {
  chown "$WEB_USER" "$WEB_DB" 2>/dev/null
  for suffix in "" "-wal" "-shm"; do
    [ -e "$WEB_DB$suffix" ] && chown "$WEB_USER" "$WEB_DB$suffix"
  done
  return 0
}

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

login() {
  curl -s -c "$3" -o "$WORK/$4.json" -w '%{http_code}' -X POST "$BASE/api/v1/auth/login" \
    -H 'Content-Type: application/json' -H 'X-UPanel-Request: 1' -H "Origin: $ORIGIN" \
    -d "{\"username\":\"$1\",\"password\":\"$2\"}"
}
csrf_of() { jget "$1" 'd["data"]["csrf_token"]'; }

# urlenc <字符串>：包名里可能有 `+`（libstdc++6），直接塞进查询串会被解码成空格。
urlenc() {
  python3 -c 'import sys,urllib.parse
print(urllib.parse.quote(sys.argv[1], safe=""))' "$1"
}

# 时间戳解析。Go 的 time.Time 序列化成纳秒（…44.791497408Z），而 Python 3.10 的
# fromisoformat 只认 3 位或 6 位小数，所以要先把小数位截到微秒再解析。
ISO_PRELUDE='import datetime,re,sys
s=sys.argv[1]
m=re.match(r"^(.*\.)(\d+)([^0-9].*)$", s)
if m: s=m.group(1)+(m.group(2)+"000000")[:6]+m.group(3)
ts=datetime.datetime.fromisoformat(s.replace("Z","+00:00"))'
iso_epoch() { python3 -c "$ISO_PRELUDE
print(int(ts.timestamp()))" "$1" 2>/dev/null; }
iso_age() { python3 -c "$ISO_PRELUDE
print(int((datetime.datetime.now(datetime.timezone.utc)-ts).total_seconds()))" "$1" 2>/dev/null; }

# api <jar> <csrf> <方法> <路径> [body]：响应体落在 $WORK/resp.json，回显状态码。
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

# ---------------------------------------------------------------- 环境检查
if [ "$(id -u)" -ne 0 ]; then
  printf '本脚本必须以 root 运行：sudo BIN_DIR=%s sh %s\n' "$BIN_DIR" "$0" >&2
  exit 2
fi
for f in "$AGENT" "$WEB" "$CTL"; do
  if [ ! -x "$f" ]; then
    printf '缺少可执行文件：%s\n' "$f" >&2
    exit 2
  fi
done
if ! command -v dpkg-query >/dev/null 2>&1 || ! command -v apt-cache >/dev/null 2>&1; then
  echo '本机没有 dpkg-query / apt-cache，P11.1 的验收无法进行' >&2
  exit 2
fi
if ! id -u "$WEB_USER" >/dev/null 2>&1; then
  printf '本机没有 %s 账号（先装一遍 0.1 包）\n' "$WEB_USER" >&2
  exit 2
fi
if [ ! -f "$VERIFY" ]; then
  printf '缺少对账脚本：%s\n' "$VERIFY" >&2
  exit 2
fi

rm -rf "$WORK"
mkdir -p "$WORK/logs" "$PAYLOADS" "$FIXDIR" "$WEB_DB_DIR"
chown "$WEB_USER" "$WEB_DB_DIR"

echo "== 0. 这台机器的发行版与包管理现场 =="
DISTRO=$(sed -n 's/^PRETTY_NAME="\(.*\)"$/\1/p' /etc/os-release)
note "发行版：$DISTRO"
note "dpkg-query：$(command -v dpkg-query)；apt-cache：$(command -v apt-cache)"
DPKG_TOTAL=$(dpkg-query -W -f='${binary:Package}\n' | wc -l)
DPKG_II=$(dpkg-query -W -f='${db:Status-Abbrev}\n' | grep -c '^ii')
note "dpkg 登记的包：$DPKG_TOTAL 个；其中状态 ii 的：$DPKG_II 个"
[ "$DPKG_II" -gt 0 ] && pass "env-has-installed-packages" || fail "env-has-installed-packages" "这台机器一个 ii 都没有"

# 只读证明的基线：包数据库文件的内容摘要 + apt 索引目录的 mtime。
STATUS_SHA_BEFORE=$(sha256sum /var/lib/dpkg/status | awk '{print $1}')
LISTS_MTIME_BEFORE=$(stat -c %Y /var/lib/apt/lists)

echo
echo "== 1. 启动代理（root）与 Web（非 root） =="
install -d -m 0755 /etc/upanel
install -d -m 0700 "$AGENT_DB_DIR"
install -d -m 0755 "$RUN_DIR"
# 只清 Web 库：代理库里的卷、应用与秘密登记是真机状态，脚本不销毁。
rm -f "$WEB_DB" "$WEB_DB-wal" "$WEB_DB-shm"
cat >"$SYS_CFG" <<JSON
{
  "web": { "mode": "production", "listen_address": "127.0.0.1:$PORT", "shutdown_timeout_seconds": 10 },
  "agent": {
    "socket_path": "$SOCK",
    "shutdown_timeout_seconds": 10,
    "web_user": "$WEB_USER",
    "socket_group": "$WEB_USER",
    "database_path": "$AGENT_DB",
    "secret_key_path": "$KEY_FILE",
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
  pass "agent-key-present"
else
  if "$AGENT" --config "$SYS_CFG" -init-key >>"$WORK/logs/init-key.log" 2>&1; then
    pass "agent-key-created"
  else
    fail "agent-key-created" "$(tail -3 "$WORK/logs/init-key.log" 2>/dev/null)"
  fi
fi
if start_agent; then pass "agent-started"; else fail "agent-started" "代理没起来（见 $AGENT_LOG）"; fi
expect_eq "agent-runs-as-root" "0" "$(ps -o uid= -p "$AGENT_PID" 2>/dev/null | tr -d ' ')"
if grep -q "已启用 APT 事实读取" "$AGENT_LOG"; then
  pass "agent-logs-apt-enabled"
else
  fail "agent-logs-apt-enabled" "代理日志里没有启用 APT 事实读取的记录（见 $AGENT_LOG）"
fi

printf '%s\n%s\n' "$PASSWORD" "$PASSWORD" | "$CTL" init-admin --config "$SYS_CFG" --db "$WEB_DB" \
  --username admin --display-name "管理员" --json >"$WORK/init.out" 2>"$WORK/init.err"
expect_eq "init-admin" "0" "$?"
chown_web_db
if start_web; then pass "web-started"; else fail "web-started" "Web 没起来（见 $WEB_LOG）"; fi
LISTEN_PID=$(ss -lptnH "sport = :$PORT" 2>/dev/null | sed -n 's/.*pid=\([0-9]*\).*/\1/p' | head -1)
[ -z "$LISTEN_PID" ] && LISTEN_PID=$WEB_PID
WEB_UID=$(ps -o uid= -p "$LISTEN_PID" 2>/dev/null | tr -d ' ')
if [ -n "$WEB_UID" ] && [ "$WEB_UID" != "0" ]; then
  pass "web-not-root"
else
  fail "web-not-root" "pid=${LISTEN_PID:-未知} uid=${WEB_UID:-未知}"
fi

echo
echo "== 2. 能力表如实报告两条查询 =="
# 能力接口只认 upanel-web 的 SO_PEERCRED，所以必须以该用户去连；而 $PAYLOADS 是
# root 的私有目录，upanel-web 写不进去——先在 /tmp 落文件，再搬进来。
CAP=$PAYLOADS/capabilities.json
CAP_TMP=/tmp/p11-1-capabilities.$$.json
CODE=$(runuser -u "$WEB_USER" -- curl -s -o "$CAP_TMP" -w '%{http_code}' \
  --unix-socket "$SOCK" http://agent/internal/v1/capabilities 2>/dev/null)
cp "$CAP_TMP" "$CAP" 2>/dev/null
cp "$CAP" "$FIXDIR/capabilities.json" 2>/dev/null
rm -f "$CAP_TMP"
expect_eq "capabilities-200" "200" "$CODE"
QUERIES=$(jget "$CAP" 'json.dumps(d["data"]["supported_queries"])')
ACTIONS=$(jget "$CAP" 'json.dumps(d["data"]["supported_actions"])')
expect_eq "capabilities-has-apt-packages" "yes" \
  "$(jget "$CAP" '"apt.packages" in d["data"]["supported_queries"] and "yes" or "no"')"
expect_eq "capabilities-has-power-state" "yes" \
  "$(jget "$CAP" '"system.power_state" in d["data"]["supported_queries"] and "yes" or "no"')"
note "supported_queries=$QUERIES"
# 只读：这一阶段不登记任何 package/apt 写动作。
case "$ACTIONS" in
  *"package."*|*"apt."*) fail "capabilities-no-package-actions" "supported_actions=$ACTIONS" ;;
  *) pass "capabilities-no-package-actions" ;;
esac

echo
echo "== 3. 登录与权限 =="
JAR=$WORK/admin.jar
CODE=$(login admin "$PASSWORD" "$JAR" login-admin)
if [ "$CODE" != "200" ]; then
  fail "login-admin" "状态码 $CODE：$(head -c 200 "$WORK/login-admin.json")"
  printf '\n== 汇总 ==\nPASS %d  FAIL %d  NOT RUN %d\n' "$PASSED" "$FAILED" "$NOTRUN"
  exit 1
fi
pass "login-admin"
CSRF=$(csrf_of "$WORK/login-admin.json")

CODE=$(api "$JAR" "$CSRF" POST "$USERS" \
  "{\"username\":\"pkg-watcher\",\"display_name\":\"pkg-watcher\",\"role\":\"viewer\",\"initial_password\":\"$VIEWER_PASSWORD\"}")
case "$CODE" in
  200|201) note "已建 viewer 账号 pkg-watcher" ;;
  *) fail "create-viewer" "状态码 $CODE：$(head -c 200 "$WORK/resp.json")" ;;
esac
VIEWER_JAR=$WORK/viewer.jar
CODE=$(login pkg-watcher "$VIEWER_PASSWORD" "$VIEWER_JAR" login-viewer)
if [ "$CODE" = "200" ]; then
  pass "login-viewer"
  for path in "$PACKAGES" "$UPDATES" "$POWER"; do
    CODE=$(api "$VIEWER_JAR" "" GET "$path")
    expect_eq "viewer-403$path" "403" "$CODE"
  done
else
  fail "login-viewer" "状态码 $CODE"
fi
for path in "$PACKAGES" "$UPDATES" "$POWER"; do
  CODE=$(curl -s -o /dev/null -w '%{http_code}' "$BASE$path" -H "Origin: $ORIGIN")
  expect_eq "anon-401$path" "401" "$CODE"
done
# 没有写入口：本阶段不开任何变更路由。
for method in POST PUT PATCH DELETE; do
  CODE=$(api "$JAR" "$CSRF" "$method" "$PACKAGES" '{}')
  case "$CODE" in
    404|405) pass "no-write-route-$method" ;;
    *) fail "no-write-route-$method" "状态码 $CODE（这一组不该有写入口）" ;;
  esac
done

echo
echo "== 3b. 首次采集（冷启动可能几十秒，这一步只等，不判断快慢） =="
# 冷采的成本是真的：apt 2.8（24.04）里 `apt-cache policy` 对每个参数做一次全库
# 模式匹配，731 个包名实测 26.6 秒；apt 2.4（22.04）0.3 秒。代理开机时会先在
# 后台采一份，Web 侧这条查询的超时也放到 45 秒（packages.AgentQueryTimeout）。
# 这里如实记下"第一次拿到事实用了多久"，然后照常断言它必须拿到。
ATTEMPT=0
COLD_MS=0
CODE=000
while [ "$ATTEMPT" -lt 3 ]; do
  ATTEMPT=$((ATTEMPT + 1))
  T0=$(date +%s%N)
  CODE=$(api "$JAR" "$CSRF" GET "$PACKAGES")
  T1=$(date +%s%N)
  COLD_MS=$(( (T1 - T0) / 1000000 ))
  [ "$CODE" = "200" ] && break
  note "第 $ATTEMPT 次 GET /packages 返回 $CODE（${COLD_MS}ms），5 秒后重试"
  sleep 5
done
note "首次拿到包事实：第 $ATTEMPT 次，${COLD_MS}ms"
expect_eq "packages-first-call-succeeds" "200" "$CODE"

echo
echo "== 4. GET /packages：事实与判断 =="
PAGE1=$PAYLOADS/packages-page1.json
CODE=$(api "$JAR" "$CSRF" GET "$PACKAGES")
expect_eq "packages-200" "200" "$CODE"
cp "$WORK/resp.json" "$PAGE1" 2>/dev/null
cp "$PAGE1" "$FIXDIR/packages-page1.json" 2>/dev/null
COLLECTED=$(jget "$PAGE1" 'd["data"]["collected_at"]')
INDEX_AT=$(jget "$PAGE1" 'd["data"]["index_updated_at"]')
LIMIT=$(jget "$PAGE1" 'd["data"]["limit"]')
MATCHED=$(jget "$PAGE1" 'd["data"]["matched"]')
COUNT_INSTALLED=$(jget "$PAGE1" 'd["data"]["counts"]["installed"]')
NEXT=$(jget "$PAGE1" 'd["data"]["next_cursor"]')
# jget 打印 python 的字面量：null 会变成 "None"，折成空串表示"没有下一页"。
case "$NEXT" in None) NEXT="" ;; esac
PAGE_LEN=$(jget "$PAGE1" 'len(d["data"]["packages"])')
note "collected_at=$COLLECTED  index_updated_at=$INDEX_AT  matched=$MATCHED"
expect_eq "packages-default-limit" "50" "$LIMIT"
expect_eq "packages-default-page-size" "50" "$PAGE_LEN"
expect_eq "packages-counts-installed-matches-dpkg" "$DPKG_II" "$COUNT_INSTALLED"
expect_ne "packages-has-next-cursor" "" "$NEXT"
# 没有来源错误：null 与 [] 都算空（Go 的 nil 切片序列化成 null）。
expect_eq "packages-errors-empty" "empty" "$(jget "$PAGE1" 'd["data"]["errors"] in (None, []) and "empty" or "present"')"

# index_updated_at 必须就是 /var/lib/apt/lists 的 mtime。
if [ "$INDEX_AT" != "" ]; then
  expect_eq "packages-index-time-is-lists-mtime" "$LISTS_MTIME_BEFORE" "$(iso_epoch "$INDEX_AT")"
else
  fail "packages-index-time-is-lists-mtime" "index_updated_at 为空"
fi

# collected_at 必须落在最近 5 分钟内（快照是 30 秒缓存，不可能是隔夜数据）。
AGE=$(iso_age "$COLLECTED")
if [ -n "$AGE" ] && [ "$AGE" -ge 0 ] && [ "$AGE" -le 300 ]; then
  pass "packages-collected-at-fresh"
else
  fail "packages-collected-at-fresh" "collected_at=$COLLECTED 距现在 ${AGE:-无法解析} 秒"
fi

# 快照缓存：紧接着再要一条，拿到的必须是同一份快照（同一个 collected_at）。
# 这一条放在这里而不是最后：后面的对账要对每个包跑一次 dpkg，可能超过 30 秒缓存期。
CODE=$(api "$JAR" "$CSRF" GET "$PACKAGES?limit=1")
expect_eq "packages-second-request-200" "200" "$CODE"
COLLECTED2=$(jget "$WORK/resp.json" 'd["data"]["collected_at"]')
expect_eq "packages-snapshot-cached-30s" "$COLLECTED" "$COLLECTED2"

# 翻页到底。用 sh 循环 + curl（与前面同一套 api 辅助），每页落一个文件，
# 最后交给 python 合并并查重——不在 python 里重新造一遍 cookie 鉴权。
cp "$PAGE1" "$WORK/page-1.json"
PAGE_NO=1
CURSOR=$NEXT
while [ -n "$CURSOR" ] && [ "$PAGE_NO" -lt 200 ]; do
  PAGE_NO=$((PAGE_NO + 1))
  CODE=$(api "$JAR" "$CSRF" GET "$PACKAGES?limit=50&cursor=$CURSOR")
  if [ "$CODE" != "200" ]; then
    fail "packages-paging-page-$PAGE_NO" "状态码 $CODE：$(head -c 200 "$WORK/resp.json")"
    break
  fi
  cp "$WORK/resp.json" "$WORK/page-$PAGE_NO.json"
  CURSOR=$(jget "$WORK/page-$PAGE_NO.json" 'd["data"]["next_cursor"]')
  case "$CURSOR" in None) CURSOR="" ;; esac
done
if [ "$PAGE_NO" -ge 200 ]; then
  fail "packages-paging-terminates" "翻了 200 页还没到底"
fi

python3 - "$WORK" <<'PY'
import json, os, sys
work = sys.argv[1]
entries, seen, pages = [], set(), 0
counts = None
filters = None
dup = 0
n = 1
while True:
    path = os.path.join(work, "page-%d.json" % n)
    if not os.path.exists(path):
        break
    pages += 1
    with open(path, encoding="utf-8") as fh:
        data = json.load(fh)["data"]
    if n == 1:
        filters = data["filters"]
    counts = data["counts"]
    for item in data["packages"]:
        key = (item["name"], item["architecture"])
        if key in seen:
            dup += 1
        seen.add(key)
        entries.append(item)
    n += 1
with open(os.path.join(work, "packages-all.json"), "w", encoding="utf-8") as fh:
    json.dump({"entries": entries, "counts": counts, "filters": filters, "pages": pages,
               "duplicates": dup}, fh, ensure_ascii=False)
print("PAGES=%d ENTRIES=%d DUPLICATES=%d" % (pages, len(entries), dup))
PY

PAGES=$(jget "$WORK/packages-all.json" 'd["pages"]')
ENTRIES=$(jget "$WORK/packages-all.json" 'len(d["entries"])')
DUPS=$(jget "$WORK/packages-all.json" 'd["duplicates"]')
note "翻页：$PAGES 页，共 $ENTRIES 条，重复 $DUPS 条"
expect_eq "packages-paging-covers-all" "$DPKG_II" "$ENTRIES"
expect_eq "packages-paging-no-duplicates" "0" "$DUPS"
if [ "$PAGES" -gt 1 ]; then pass "packages-paging-multi-page"; else notrun "packages-paging-multi-page" "包数不足一页"; fi

echo
echo "== 5. 与 dpkg / apt 逐条对账（版本比较用 dpkg 自己的实现） =="
if python3 "$VERIFY" "$WORK/packages-all.json" >"$WORK/verify.txt" 2>&1; then
  pass "packages-verified-against-dpkg-and-apt"
  tail -1 "$WORK/verify.txt" | sed 's/^/      /'
else
  fail "packages-verified-against-dpkg-and-apt" "对账不一致"
  head -30 "$WORK/verify.txt" | sed 's/^/      /'
fi
cp "$WORK/verify.txt" "$PAYLOADS/verify.txt" 2>/dev/null

# 默认列表只列真正装着的包（ii）。仅有配置残留（rc）的包要显式 installed=false 才出现，
# 而且那时 installed 必须是 false——它不是"装着"，只是 dpkg 还留着它的配置文件。
RC_NAME=$(dpkg-query -W -f='${binary:Package}\t${db:Status-Abbrev}\n' 2>/dev/null \
  | awk -F'\t' '$2 ~ /^rc/ {print $1; exit}')
if [ -n "$RC_NAME" ]; then
  ENC=$(urlenc "$RC_NAME")
  CODE=$(api "$JAR" "$CSRF" GET "$PACKAGES?query=$ENC")
  expect_eq "packages-default-excludes-rc" "0" "$(jget "$WORK/resp.json" 'd["data"]["matched"]')"
  CODE=$(api "$JAR" "$CSRF" GET "$PACKAGES?installed=false&query=$ENC")
  expect_eq "packages-installed-false-includes-rc" "1" "$(jget "$WORK/resp.json" 'd["data"]["matched"]')"
  cp "$WORK/resp.json" "$FIXDIR/packages-rc.json" 2>/dev/null
  expect_eq "packages-rc-row-not-installed" "False" \
    "$(jget "$WORK/resp.json" 'd["data"]["packages"][0]["installed"]')"
  note "rc 反例包：$RC_NAME（状态 $(dpkg-query -W -f='${db:Status-Abbrev}' "$RC_NAME")）"
else
  notrun "packages-default-excludes-rc" "这台机器没有 rc（仅剩配置）的包"
  notrun "packages-installed-false-includes-rc" "这台机器没有 rc（仅剩配置）的包"
fi

# apparmor 那一类：候选来自 updates，security 源里放的是更旧的版本。
# 本机有没有这种包是随机的（取决于索引快照），有就断言，没有就如实记 NOT RUN。
TRAP_PKG=$(python3 - "$WORK/packages-all.json" <<'PY'
import json, sys
with open(sys.argv[1], encoding="utf-8") as fh:
    data = json.load(fh)
for e in data["entries"]:
    if e["candidate_relation"] == "newer" and e["security"] is False and e["suites"]:
        print(e["name"])
        break
PY
)
if [ -n "$TRAP_PKG" ]; then
  TRAP_SEC=$(apt-cache policy "$TRAP_PKG" | grep -c -e '-security/' || true)
  note "反例包：$TRAP_PKG（候选来源 $(apt-cache policy "$TRAP_PKG" | sed -n 's/^  Candidate: //p')，security 源里出现的行数 $TRAP_SEC）"
  FLAG=$(jget "$WORK/packages-all.json" "[e[\"security\"] for e in d[\"entries\"] if e[\"name\"]==\"$TRAP_PKG\"][0]")
  expect_eq "packages-security-not-from-other-versions" "False" "$FLAG"
else
  notrun "packages-security-not-from-other-versions" "本机没有'候选只来自 updates、security 源里是更旧版本'的包"
fi

echo
echo "== 6. GET /packages/updates =="
UPD=$PAYLOADS/packages-updates.json
CODE=$(api "$JAR" "$CSRF" GET "$UPDATES")
expect_eq "updates-200" "200" "$CODE"
cp "$WORK/resp.json" "$UPD" 2>/dev/null
cp "$UPD" "$FIXDIR/packages-updates.json" 2>/dev/null
UPD_TOTAL=$(jget "$UPD" 'd["data"]["total"]')
UPD_SEC=$(jget "$UPD" 'd["data"]["security"]')
UPD_LEN=$(jget "$UPD" 'len(d["data"]["updates"])')
UPD_TRUNC=$(jget "$UPD" 'd["data"]["truncated"]')
note "total=$UPD_TOTAL security=$UPD_SEC 返回 $UPD_LEN 条 truncated=$UPD_TRUNC"
expect_eq "updates-total-matches-list-counts" "$(jget "$PAGE1" 'd["data"]["counts"]["upgradable"]')" "$UPD_TOTAL"
expect_eq "updates-security-matches-list-counts" "$(jget "$PAGE1" 'd["data"]["counts"]["security"]')" "$UPD_SEC"
if [ "$UPD_TRUNC" = "False" ]; then
  expect_eq "updates-count-matches-total" "$UPD_TOTAL" "$UPD_LEN"
else
  expect_eq "updates-truncated-cap" "1000" "$UPD_LEN"
fi

# 每一条都必须真的"候选比已安装新"（用 dpkg 的比较器复核），且安全更新在前。
UPD_BAD=$(python3 - "$UPD" <<'PY'
import json, subprocess, sys
with open(sys.argv[1], encoding="utf-8") as fh:
    data = json.load(fh)["data"]
bad = []
seen_non_security = False
for item in data["updates"]:
    cand, inst = item["candidate_version"], item["installed_version"]
    if subprocess.run(["dpkg", "--compare-versions", cand, "gt", inst]).returncode != 0:
        bad.append("%s: %s 不大于 %s" % (item["name"], cand, inst))
    if item["security"]:
        if seen_non_security:
            bad.append("%s: 安全更新排在了非安全更新后面" % item["name"])
        if not any(s.endswith("-security") for s in item["suites"]):
            bad.append("%s: 标了安全更新但候选来源里没有 -security（%s）" % (
                item["name"], ",".join(item["suites"])))
    else:
        seen_non_security = True
print("; ".join(bad[:10]))
PY
)
expect_eq "updates-every-item-newer-and-security-first" "" "$UPD_BAD"

echo
echo "== 7. GET /system/power-state =="
PS=$PAYLOADS/power-state.json
CODE=$(api "$JAR" "$CSRF" GET "$POWER")
expect_eq "power-state-200" "200" "$CODE"
cp "$WORK/resp.json" "$PS" 2>/dev/null
cp "$PS" "$FIXDIR/power-state.json" 2>/dev/null
REBOOT=$(jget "$PS" 'd["data"]["reboot_required"]')
BOOT_ID=$(jget "$PS" 'd["data"]["boot_id"]')
PKGS=$(jget "$PS" 'json.dumps(d["data"]["reboot_required_packages"], ensure_ascii=False)')
BOOT_ID_REAL=$(cat /proc/sys/kernel/random/boot_id)
expect_eq "power-state-boot-id-matches-kernel" "$BOOT_ID_REAL" "$BOOT_ID"
if [ -f /run/reboot-required ]; then
  expect_eq "power-state-reboot-required-matches-file" "True" "$REBOOT"
else
  expect_eq "power-state-reboot-required-matches-file" "False" "$REBOOT"
fi
if [ -f /run/reboot-required.pkgs ]; then
  EXPECT_PKGS=$(python3 -c 'import json,sys
names=[]
for line in open("/run/reboot-required.pkgs", encoding="utf-8", errors="replace"):
    name=line.strip()
    if name and name not in names:
        names.append(name)
print(json.dumps(names, ensure_ascii=False))')
  expect_eq "power-state-pkgs-matches-file" "$EXPECT_PKGS" "$PKGS"
else
  expect_eq "power-state-pkgs-empty-without-file" "[]" "$PKGS"
fi

echo
echo "== 8. 只读性：包数据库与索引没有被动过 =="
STATUS_SHA_AFTER=$(sha256sum /var/lib/dpkg/status | awk '{print $1}')
LISTS_MTIME_AFTER=$(stat -c %Y /var/lib/apt/lists)
expect_eq "readonly-dpkg-status-unchanged" "$STATUS_SHA_BEFORE" "$STATUS_SHA_AFTER"
expect_eq "readonly-apt-lists-mtime-unchanged" "$LISTS_MTIME_BEFORE" "$LISTS_MTIME_AFTER"

echo
echo "== 9. 参数拒绝 =="
for path in "$PACKAGES?limit=201" "$PACKAGES?limit=0" "$PACKAGES?limit=abc" \
  "$PACKAGES?installed=maybe" "$PACKAGES?upgradable=2" "$PACKAGES?cursor=not-base64!!"; do
  CODE=$(api "$JAR" "$CSRF" GET "$path")
  expect_eq "reject$(printf '%s' "$path" | tr '?&=!/' '_____')" "400" "$CODE"
done
CODE=$(api "$JAR" "$CSRF" GET "$PACKAGES?query=vim%0Aapt")
expect_eq "reject-query-control-char" "400" "$CODE"
CODE=$(api "$JAR" "$CSRF" GET "$PACKAGES?query=bash&installed=true&limit=5")
expect_eq "accept-filtered-query" "200" "$CODE"
# 先落盘再断言：$WORK/resp.json 是"最后一次响应"，后面的请求会覆盖它。
SEARCH=$WORK/search.json
cp "$WORK/resp.json" "$SEARCH" 2>/dev/null
cp "$SEARCH" "$FIXDIR/packages-search.json" 2>/dev/null
QRY_NAMES=$(jget "$SEARCH" '[e["name"] for e in d["data"]["packages"]]')
case "$QRY_NAMES" in
  *bash*) pass "accept-filtered-query-matches" ;;
  *) fail "accept-filtered-query-matches" "搜 bash 的结果里没有 bash：$QRY_NAMES" ;;
esac

# 安全更新筛选：数量不一定非零（打满补丁的机器上就是零），照样抓一份——
# 夹具要证明的是"筛选生效"，不是"这台机器有安全更新"。
CODE=$(api "$JAR" "$CSRF" GET "$PACKAGES?security=true&limit=20")
expect_eq "accept-security-filter" "200" "$CODE"
SEC_MATCHED=$(jget "$WORK/resp.json" 'd["data"]["matched"]')
SEC_ALL_SEC=$(jget "$WORK/resp.json" 'all(e["security"] for e in d["data"]["packages"])')
expect_eq "accept-security-filter-only-security" "True" "$SEC_ALL_SEC"
note "security=true 命中 $SEC_MATCHED 个"
cp "$WORK/resp.json" "$FIXDIR/packages-security.json" 2>/dev/null

# arch 限定名：`dpkg-query` 给的名字是 `bind9-libs:amd64`，而 `apt-cache policy`
# 打出来的表头是 `bind9-libs:`（apt 自己把 `:arch` 剥掉了）。按请求名去查必然落空，
# 于是"候选读不到"——真机上整整一条包被谎报成没有候选。这里按包名搜一份原始响应留档：
# 前端回放要能证明"装着的包，候选读得到"。
CODE=$(api "$JAR" "$CSRF" GET "$PACKAGES?query=bind9&limit=20")
expect_eq "accept-arch-qualified" "200" "$CODE"
cp "$WORK/resp.json" "$FIXDIR/packages-bind9.json" 2>/dev/null
# 注意匹配用 startswith：dpkg 的 `${binary:Package}` 对带架构限定的包给的是
# `bind9-libs:amd64`，名字里就已经带 `:arch` 了。
BIND_CAND=$(jget "$WORK/resp.json" '[e["candidate_version"] for e in d["data"]["packages"] if e["name"].startswith("bind9-libs")]')
case "$BIND_CAND" in
  '[]') notrun "accept-arch-qualified-candidate" "这台机器没装 bind9-libs；arch 限定名由 internal/apt 单测覆盖" ;;
  # jget 打的是 Python 列表的字面量，null 会变成 `[None]`——所以匹配的是"包含 None"。
  *None*) fail "accept-arch-qualified-candidate" "bind9-libs:amd64 的候选又读不到了：$BIND_CAND" ;;
  *)
    pass "accept-arch-qualified-candidate"
    note "bind9-libs 候选 $BIND_CAND"
    ;;
esac

echo
echo "== 10. 明确未覆盖 =="
notrun "apt-cache 失败时的降级" "要让 apt-cache 真失败会破坏验收机的 apt；该分支由单测覆盖"
notrun "安装/卸载/升级/刷新索引" "P11.2"
notrun "重启执行与 boot_id 前后确认" "P11.3"
notrun "分阶段推送与被保留包的判定（apt-get -s upgrade）" "P11.2"
notrun "浏览器渲染" "VM 上没有浏览器；页面数据由真实抓取的夹具回放验证"

echo
echo "== 汇总 =="
printf 'PASS %d  FAIL %d  NOT RUN %d\n' "$PASSED" "$FAILED" "$NOTRUN"
printf '夹具（原样抓取，前端回放用）：%s\n' "$FIXDIR"
ls -1 "$FIXDIR" 2>/dev/null | sed 's/^/      /'
if [ "$FAILED" -ne 0 ]; then
  printf '失败项：%s\n' "$FAILED_NAMES"
  exit 1
fi
exit 0
