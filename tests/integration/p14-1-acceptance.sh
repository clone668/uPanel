#!/bin/sh
# P14.1 Linux 验收脚本：网络现状的四条只读端点与"能不能改"的判定。
#
# 运行位置：Ubuntu 22.04/24.04 验收 VM 内，**必须以 root 运行**。不依赖 Go 工具链：
# 用宿主交叉编译好的 Linux 二进制。
#
# 用法：
#   sudo BIN_DIR=/tmp/upanel-bin sh tests/integration/p14-1-acceptance.sh
#
# 退出码：0 全部通过；1 有失败项；2 环境不满足（非 root、缺二进制/python3/runuser、缺服务账号）。
#
# 覆盖的验收点（STAGES P14.1、PLAN §19、CONTRACTS §13.5）：
#   A. 权限：四条端点未登录 401、viewer 403、admin 200；**写入口不存在**
#      （POST 同一路径回 404/405——P14.1 不提供任何变更入口，也不提前放出 P14.2 的半成品）
#   B. 网卡：形状与闭集；每张网卡的 `physical` 与 `kind` 与 **sysfs 现场事实**逐个对照；
#      `ip` 报的网卡集合不能漏（现场有的响应里必须有）；回环地址的 family/scope
#   C. "能不能改"：用 sysfs 与 /run 下的现场事实**独立重算**一遍判定，
#      与响应里的 backend/editable/target/reasons 逐项比对；editable=false 时 reasons 非空、
#      backend 不认识时不硬判
#   D. 路由：家族闭集、目标写成网段、`metric`/`pref_set` 成对、默认路由现场网关必须出现
#   E. 监听端口：与 `/proc/net/{tcp,tcp6}` 的 LISTEN 集合双向比对（不漏报、不臆造）；
#      本面板自己的端口出现且 `pid` 等于 Web 进程号；`inode` 内部字段不出现在响应里；
#      通配/回环标记与绑定地址自洽
#   F. 防火墙：manager 闭集；装了没启用与没装分开；`ufw status verbose` 的现场状态与
#      默认策略、规则条数逐项对照；editable 只在 ufw 独占时为 true
#   G. 三条口径：`issues` 是数组（不是 null）、source/code 都在闭集内；
#      四条响应都带可解析的 `collected_at` 且是本轮采集的
#   H. **只读**：四条请求跑完，`/etc/netplan` 与 `/etc/ufw` 下的文件清单与哈希一字不变
#   I. 卫生：web/agent 日志无 panic，两个进程仍在。
#
# 为什么这些断言不靠 mock：mock 只能证明"handler 会把代理给的结构原样吐出去"（那部分
# 由 `internal/api/network_test.go` 覆盖）。这一层要证明的是**代理从这台机器的内核与
# 文件系统里读出来的东西是对的**，所以每条关键断言都配一条现场独立读数：sysfs 判网卡
# 类型、/run 判配置归属、/proc/net 判监听、ufw 自己判防火墙。两边对不上就是缺陷，
# 不管哪边看起来更合理。
#
# 为什么不做任何系统写：P14.1 的范围就是只读。脚本不建网桥、不加 ufw 规则、不改 netplan，
# 连"造一个第三方管理冲突"都不做——那需要写系统配置，属于 P14.2 的预检与回滚该验的事。
# 本轮判定用的是这台机器**本来就有**的事实（cloud-init 的 netplan 配置、默认的 ufw 状态）。

set -u

umask 077

BIN_DIR=${BIN_DIR:-/tmp/upanel-bin}
if [ "$(id -u)" -ne 0 ]; then
  printf '本脚本必须以 root 运行：sudo BIN_DIR=%s sh %s\n' "$BIN_DIR" "$0" >&2
  exit 2
fi

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
AGENT="$BIN_DIR/upanel-agent"
WEB="$BIN_DIR/upanel-web"
CTL="$BIN_DIR/upanelctl"

WORK=${WORK:-/var/tmp/upanel-accept-p14-1}
TMPD=$WORK/tmp
RUN_DIR=/run/upanel
SOCK=$RUN_DIR/agent.sock
SYS_CFG=/etc/upanel/upanel.json
AGENT_DB_DIR=/var/lib/upanel-agent
AGENT_DB=$AGENT_DB_DIR/agent.db
KEY_FILE=/etc/upanel/agent.key
WEB_DB_DIR=$WORK/webdata
WEB_DB=$WEB_DB_DIR/upanel.db
PORT=${PORT:-19471}
BASE="http://127.0.0.1:$PORT"
ORIGIN="$BASE"
WEB_USER=upanel-web
PASSWORD='vm-p141-admin-31'
VIEWER_PASSWORD='vm-p141-viewer-58'

AGENT_LOG=$WORK/logs/agent.log
WEB_LOG=$WORK/logs/web.log
JSON_IN=$TMPD/json.in
JSON_OUT=$TMPD/json.out

PASSED=0
FAILED=0
FAILED_NAMES=""
AGENT_PID=""
WEB_PID=""
WEB_APP_PID=""
JSON_PID=""

pass() { PASSED=$((PASSED + 1)); printf 'PASS  %s\n' "$1"; }
fail() { FAILED=$((FAILED + 1)); FAILED_NAMES="$FAILED_NAMES | $1"; printf 'FAIL  %s: %s\n' "$1" "${2:-}"; }
note() { printf 'NOTE  %s\n' "$1"; }

expect_eq() { if [ "$2" = "$3" ]; then pass "$1"; else fail "$1" "期望 '$2'，实际 '$3'"; fi; }
expect_contains() {
  case $3 in
    *"$2"*) pass "$1" ;;
    *) fail "$1" "缺少 '$2'（实际: $3）" ;;
  esac
}
expect_not_contains() {
  case $3 in
    *"$2"*) fail "$1" "不该出现 '$2'（实际: $3）" ;;
    *) pass "$1" ;;
  esac
}
# in_set <用例名> <值> <空格分隔的闭集>
in_set() {
  for _v in $3; do
    if [ "$2" = "$_v" ]; then pass "$1"; return 0; fi
  done
  fail "$1" "'$2' 不在闭集 {$3}"
  return 1
}
# expect_true/false 收的是 json.dumps 出来的小写 true/false。
expect_true() { expect_eq "$1" "true" "$2"; }
expect_false() { expect_eq "$1" "false" "$2"; }

# ---- JSON 助手 ----
#
# 每一问都要起一次 python3：24.04 走 TCG 时一次冷启动 8—17 秒（见 json_helper.py 的说明），
# 而本脚本要问一百多次。所以解释器**常驻**，一问一答。
JSON_TAB=$(printf '\t')
JSON_SEQ=0
jget() {
  JSON_SEQ=$((JSON_SEQ + 1)); _seq=$JSON_SEQ
  printf '%s\tget\t%s\t%s\n' "$_seq" "$1" "$2" >&7
  IFS=$JSON_TAB read -r _got _val <&8
  if [ "${_got:-}" != "$_seq" ]; then printf 'ERR:helper-mismatch'; return 0; fi
  printf '%s' "$_val"
}
# jbd <文件> <表达式>：用 json.dumps 出口，布尔是 true/false、数组是 [...]。
# 直接 jget 一个布尔会拿到 Python 的 True——那种失败看起来像产品没生效。
jbd() { jget "$1" "json.dumps($2)"; }

# ---- 进程 ----

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
  runuser -u "$WEB_USER" -- "$WEB" --config "$SYS_CFG" --mode production --db "$WEB_DB" \
    --listen "127.0.0.1:$PORT" >>"$WEB_LOG" 2>&1 &
  WEB_PID=$!
  for _ in $(seq 1 200); do
    if curl -fsS "$BASE/api/v1/health/live" >/dev/null 2>&1; then
      WEB_APP_PID=$(web_child_pid)
      return 0
    fi
    sleep 0.2
  done
  return 1
}

# web_child_pid：runuser 是壳，真正监听端口的是它 fork 出来的子进程。
# 拿去和监听表里的 pid 对照的必须是子进程号，否则"端口持有者"这条断言永远差一个壳。
web_child_pid() {
  for _ in $(seq 1 50); do
    _c=$(tr ' ' '\n' <"/proc/$WEB_PID/task/$WEB_PID/children" 2>/dev/null | grep -m1 -E '^[0-9]+$')
    if [ -n "$_c" ]; then printf '%s' "$_c"; return 0; fi
    sleep 0.1
  done
  printf '%s' "$WEB_PID"
}

stop_web() {
  if [ -n "$WEB_PID" ] && kill -0 "$WEB_PID" 2>/dev/null; then
    kill -TERM "$WEB_PID" 2>/dev/null
    wait "$WEB_PID" 2>/dev/null
  fi
  WEB_PID=""
  for _ in $(seq 1 150); do
    curl -fsS "$BASE/api/v1/health/live" >/dev/null 2>&1 || return 0
    sleep 0.2
  done
  return 1
}

stop_json() {
  if [ -n "$JSON_PID" ] && kill -0 "$JSON_PID" 2>/dev/null; then
    kill -TERM "$JSON_PID" 2>/dev/null
    wait "$JSON_PID" 2>/dev/null
  fi
}

cleanup() {
  stop_json
  stop_web
  stop_agent
}
trap cleanup EXIT
trap 'cleanup; exit 130' INT
trap 'cleanup; exit 143' TERM

# ---- HTTP ----

login() {
  curl -s -c "$3" -o "$WORK/$4.json" -w '%{http_code}' -X POST "$BASE/api/v1/auth/login" \
    -H 'Content-Type: application/json' -H 'X-UPanel-Request: 1' -H "Origin: $ORIGIN" \
    -d "{\"username\":\"$1\",\"password\":\"$2\"}"
}
csrf_of() { jget "$1" 'd["data"]["csrf_token"]'; }

# apiout <jar|-> <输出文件> <路径> [方法] [csrf] → HTTP 状态码
#
# 只读 GET 不需要 CSRF（契约里 CSRF 只保护写），带 Origin 是为了走同源检查。
apiout() {
  _jar=$1; _out=$2; _path=$3; _method=${4:-GET}; _csrf=${5:-}
  if [ -n "$_csrf" ]; then
    curl -s -b "$_jar" -o "$_out" -w '%{http_code}' -X "$_method" "$BASE$_path" \
      -H "Origin: $ORIGIN" -H "X-CSRF-Token: $_csrf"
  else
    curl -s -b "$_jar" -o "$_out" -w '%{http_code}' -X "$_method" "$BASE$_path" -H "Origin: $ORIGIN"
  fi
}

# ---- 现场事实（全部只读） ----

# physical_eths：现场真实存在的物理以太网卡，一行一个。
# 判据与产品同源但**独立实现**：有 device 目录说明落在硬件上；type 1 是 ARPHRD_ETHER；
# 有 bridge/bonding/wireless/phy80211 目录或在 /proc/net/vlan 里出现，就不是"普通以太网卡"。
physical_eths() {
  for _d in /sys/class/net/*; do
    [ -d "$_d" ] || continue
    _n=${_d##*/}
    [ "$_n" = lo ] && continue
    [ -d "$_d/device" ] || continue
    [ "$(cat "$_d/type" 2>/dev/null)" = 1 ] || continue
    [ -d "$_d/bridge" ] && continue
    [ -d "$_d/bonding" ] && continue
    [ -d "$_d/wireless" ] && continue
    [ -d "$_d/phy80211" ] && continue
    [ -e "/proc/net/vlan/$_n" ] && continue
    printf '%s\n' "$_n"
  done
}

# complex_kinds：现场存在的"超出 1.0 支持范围"的接口种类（空格分隔）。
complex_kinds() {
  _out=""
  for _kind in bridge bonding wireless phy80211; do
    for _d in /sys/class/net/*; do
      [ -d "$_d/$_kind" ] || continue
      _out="$_out $_kind"
      break
    done
  done
  if [ -d /proc/net/vlan ] && [ "$(ls /proc/net/vlan 2>/dev/null | grep -vc '^config$')" -gt 0 ]; then
    _out="$_out vlan"
  fi
  printf '%s' "$_out"
}

# netplan_files：/etc/netplan 下的配置文件名（空格分隔，没有就空）。
netplan_files() {
  _out=""
  for _f in /etc/netplan/*.yaml /etc/netplan/*.yml; do
    [ -f "$_f" ] || continue
    _out="$_out ${_f##*/}"
  done
  printf '%s' "$_out"
}

netplan_present() { [ -n "$(netplan_files)" ] && echo yes || echo no; }
networkd_rendered() {
  set -- /run/systemd/network/10-netplan-*.network
  [ -f "$1" ] && echo yes || echo no
}
nm_rendered() {
  set -- /run/NetworkManager/system-connections/netplan-*
  [ -e "$1" ] && echo yes || echo no
}
nm_running() { [ -d /run/NetworkManager ] && echo yes || echo no; }

# expected_backend：按 CONTRACTS §13.5 的口径，从现场事实独立重算配置归属。
expected_backend() {
  if [ "$(netplan_present)" = yes ]; then
    if [ "$(networkd_rendered)" = yes ]; then echo netplan-networkd
    elif [ "$(nm_rendered)" = yes ]; then echo netplan-networkmanager
    else echo unknown
    fi
  else
    if [ "$(nm_rendered)" = yes ] || [ "$(nm_running)" = yes ]; then echo networkmanager
    else echo unknown
    fi
  fi
}

# sqlq <单值查询>：查不到印 <missing>，查询本身出错印 <sql-error: ...>。两者分开，
# 否则下一个断言报出来的是"语法错误"而不是"没这一行"。
sqlq() {
  python3 - "$WEB_DB" "$1" <<'PY'
import sqlite3, sys
try:
    db = sqlite3.connect("file:" + sys.argv[1] + "?mode=ro", uri=True, timeout=15)
    db.execute("PRAGMA busy_timeout = 15000")
    row = db.execute(sys.argv[2]).fetchone()
except Exception as exc:
    print("<sql-error: %s>" % exc)
else:
    print("<missing>" if row is None or row[0] is None else row[0])
PY
}

# hash_tree <目录>：清单+哈希。目录不存在时印 <absent>，与"空目录"分开。
hash_tree() {
  if [ ! -d "$1" ]; then printf '<absent>\n'; return 0; fi
  (
    cd "$1" 2>/dev/null || exit 0
    find . -type f -print0 2>/dev/null | sort -z | xargs -0 -r sha256sum 2>/dev/null
  )
}

install -d -m 0700 "$WORK" "$WORK/logs" "$TMPD"
chmod 0711 "$WORK"
rm -rf "$WEB_DB_DIR"
install -d -m 0750 "$WEB_DB_DIR"
chown "$WEB_USER" "$WEB_DB_DIR"
: >"$AGENT_LOG"
: >"$WEB_LOG"
mkdir -p "$WORK/payloads"

mkfifo "$JSON_IN" "$JSON_OUT"
python3 "$SCRIPT_DIR/json_helper.py" <"$JSON_IN" >"$JSON_OUT" 2>"$TMPD/json_helper.log" &
JSON_PID=$!
exec 7>"$JSON_IN" 8<"$JSON_OUT"

echo "== 0. 环境"
if [ "$(id -u)" -eq 0 ]; then pass "验收脚本以 root 运行"; else fail "验收脚本以 root 运行" "当前 UID $(id -u)"; fi
for b in "$AGENT" "$WEB" "$CTL"; do
  if [ -x "$b" ]; then pass "二进制存在且可执行 $(basename "$b")"; else fail "二进制存在且可执行 $(basename "$b")" "$b"; fi
done
if command -v python3 >/dev/null 2>&1; then pass "python3 可用（响应解析与 /proc 解析）"; else fail "python3 可用" "缺少 python3"; fi
if command -v runuser >/dev/null 2>&1; then pass "runuser 可用（以 Web 身份运行）"; else fail "runuser 可用" "缺少 runuser"; fi
if id -u "$WEB_USER" >/dev/null 2>&1; then pass "服务账号 $WEB_USER 存在"; else fail "服务账号 $WEB_USER 存在" "缺账号"; fi

echo
echo "== 1. 装配：代理 + 管理员 + viewer + Web =="

install -d -m 0755 /etc/upanel
install -d -m 0700 "$AGENT_DB_DIR"
rm -f "$AGENT_DB" "$AGENT_DB-wal" "$AGENT_DB-shm"
install -d -m 0755 "$RUN_DIR"
chmod 0755 "$RUN_DIR"
chown root:root "$RUN_DIR"

cat >"$SYS_CFG" <<JSON
{
  "web": {
    "mode": "production",
    "listen_address": "127.0.0.1:$PORT",
    "shutdown_timeout_seconds": 10,
    "metrics": {
      "enabled": false,
      "interval_seconds": 5,
      "raw_retention_seconds": 3600,
      "minute_retention_seconds": 7200,
      "hour_retention_seconds": 14400,
      "max_rows": 100000
    }
  },
  "agent": {
    "socket_path": "/run/upanel/agent.sock",
    "shutdown_timeout_seconds": 10,
    "web_user": "upanel-web",
    "socket_group": "upanel-web",
    "database_path": "/var/lib/upanel-agent/agent.db",
    "secret_key_path": "/etc/upanel/agent.key"
  },
  "database": {
    "path": "$WEB_DB",
    "busy_timeout_ms": 5000
  },
  "log": {
    "level": "info",
    "format": "json"
  }
}
JSON
chmod 0644 "$SYS_CFG"

if [ -f "$KEY_FILE" ]; then
  pass "代理密钥文件已存在（沿用上一轮）"
else
  if "$AGENT" --config "$SYS_CFG" -init-key >"$WORK/init-key.log" 2>&1; then
    pass "-init-key 生成代理密钥"
  else
    fail "-init-key 生成代理密钥" "$(tail -3 "$WORK/init-key.log" 2>/dev/null)"
  fi
fi

if start_agent; then pass "代理启动并创建 socket"; else fail "代理启动" "$(tail -5 "$AGENT_LOG" 2>/dev/null)"; fi

printf '%s\n%s\n' "$PASSWORD" "$PASSWORD" | "$CTL" init-admin --config "$SYS_CFG" --db "$WEB_DB" \
  --username admin --display-name "管理员" --json >"$WORK/init.out" 2>"$WORK/init.err"
expect_eq "init-admin 退出码" "0" "$?"
for suffix in "" "-wal" "-shm"; do
  [ -e "$WEB_DB$suffix" ] && chown "$WEB_USER" "$WEB_DB$suffix"
done

if start_web; then pass "Web 启动"; else fail "Web 启动" "$(tail -5 "$WEB_LOG" 2>/dev/null)"; fi

code=$(login admin "$PASSWORD" "$WORK/admin.jar" login-admin)
expect_eq "管理员登录 200" "200" "$code"
if [ "$code" != "200" ]; then
  fail "登录失败，后续断言无法进行" "$(cat "$WORK/login-admin.json" 2>/dev/null)"
  printf '通过 %d 项，失败 %d 项\n' "$PASSED" "$FAILED"
  exit 1
fi
ADMIN_CSRF=$(csrf_of "$WORK/login-admin.json")

printf '{"username":"alice","display_name":"alice","role":"viewer","initial_password":"%s"}' \
  "$VIEWER_PASSWORD" >"$WORK/req.json"
code=$(curl -s -b "$WORK/admin.jar" -o "$WORK/resp.json" -w '%{http_code}' -X POST "$BASE/api/v1/users" \
  -H 'Content-Type: application/json' -H "Origin: $ORIGIN" -H "X-CSRF-Token: $ADMIN_CSRF" -d @"$WORK/req.json")
expect_eq "创建 viewer 201" "201" "$code"
code=$(login alice "$VIEWER_PASSWORD" "$WORK/viewer.jar" login-viewer)
expect_eq "viewer 登录 200" "200" "$code"

echo
echo "== 2. A：四条端点的权限与"没有写入口" =="

for path in /api/v1/network/interfaces /api/v1/network/routes /api/v1/network/listeners /api/v1/firewall; do
  slug=$(printf '%s' "$path" | tr '/' '-')
  code=$(apiout - "$WORK/anonymous$slug.json" "$path")
  expect_eq "未登录 $path → 401" "401" "$code"
  code=$(apiout "$WORK/viewer.jar" "$WORK/viewer$slug.json" "$path")
  expect_eq "viewer $path → 403" "403" "$code"
  if [ "$code" = "403" ] && [ "$path" = "/api/v1/firewall" ]; then
    # 被拒的理由要是产品自己给的错误码，不是一个空的 200 数据体。
    expect_eq "viewer 被拒时给出 FORBIDDEN 而不是空数据" "FORBIDDEN" \
      "$(jget "$WORK/viewer$slug.json" 'd["error"]["code"]')"
  fi
done

# 写入口必须不存在：P14.2/P14.3 的写路径要走预检 → 任务 → 回滚，不能从这里提前漏出来。
# 断言的不是"回 403"，而是"这个路由根本没有注册"（404/405）——一个被权限挡住的写入口
# 仍然是一个入口，将来某个中间件顺序改一下就会露出来。
for path in /api/v1/network/interfaces /api/v1/network/routes /api/v1/network/listeners /api/v1/firewall; do
  code=$(apiout "$WORK/admin.jar" "$WORK/write-probe.json" "$path" POST "$ADMIN_CSRF")
  case $code in
    404|405) pass "写入口不存在：POST $path → $code" ;;
    *) fail "写入口不存在：POST $path" "期望 404/405，实际 $code（$(head -c 120 "$WORK/write-probe.json" 2>/dev/null)）" ;;
  esac
done
# P14.2 的 apply 也还不能存在：它的预检、回滚 timer、审计都还没做，先露一个能改地址的
# 入口比没有更糟。这一条同时钉住"本轮范围没有偷偷扩张"。
code=$(apiout "$WORK/admin.jar" "$WORK/write-probe.json" "/api/v1/network/apply" POST "$ADMIN_CSRF")
case $code in
  404|405) pass "P14.2 的写入口也还没有：POST /api/v1/network/apply → $code" ;;
  *) fail "P14.2 的写入口也还没有" "期望 404/405，实际 $code" ;;
esac

echo
echo "== 3. B/G：GET /network/interfaces 的形状与现场对照 =="

code=$(apiout "$WORK/admin.jar" "$WORK/interfaces.json" "/api/v1/network/interfaces")
expect_eq "admin 读网卡列表 200" "200" "$code"

I=$WORK/interfaces.json
expect_contains "网卡列表有 collected_at" "collected_at" "$(head -c 400 "$I")"
collected=$(jget "$I" 'd["data"]["collected_at"]')
age=$(( $(date -u +%s) - $(date -u -d "$collected" +%s 2>/dev/null || echo 0) ))
if [ "$age" -ge -5 ] && [ "$age" -le 300 ]; then
  pass "collected_at 是本轮采集的（$age 秒前）"
else
  fail "collected_at 是本轮采集的" "解析或新鲜度不对：'$collected' → $age 秒"
fi

names=$(jbd "$I" "[i['name'] for i in d['data']['interfaces']]")
expect_not_contains "interfaces 不是空数组" "[]" "$names"
expect_contains "回环在列表里" '"lo"' "$names"

# --- 逐张网卡对照 sysfs ---
for _n in $(printf '%s' "$names" | tr -d '[]" ' | tr ',' ' '); do
  _d=/sys/class/net/$_n
  if [ ! -d "$_d" ]; then
    fail "网卡 $_n 在本机存在" "sysfs 里没有 /sys/class/net/$_n：响应里有、现场没有"
    continue
  fi
  pass "网卡 $_n 在本机存在"
  # physical：/sys/class/net/<name>/device 是否存在
  if [ -d "$_d/device" ]; then _want_phys=true; else _want_phys=false; fi
  expect_eq "网卡 $_n 的 physical 与 sysfs 一致" "$_want_phys" \
    "$(jbd "$I" "[i['physical'] for i in d['data']['interfaces'] if i['name']=='$_n'][0]")"
  # kind：bridge/bond/wifi/vlan 的四类目录与 /proc/net/vlan
  if [ -d "$_d/bridge" ]; then _want_kind=bridge
  elif [ -d "$_d/bonding" ]; then _want_kind=bond
  elif [ -d "$_d/wireless" ] || [ -d "$_d/phy80211" ]; then _want_kind=wifi
  elif [ -e "/proc/net/vlan/$_n" ]; then _want_kind=vlan
  elif [ "$_n" = lo ]; then _want_kind=loopback
  elif [ "$(cat "$_d/type" 2>/dev/null)" = 1 ]; then _want_kind=ethernet
  elif [ -e "$_d/tun_flags" ]; then _want_kind=tun
  else _want_kind=other
  fi
  expect_eq "网卡 $_n 的 kind 与 sysfs 一致" "$_want_kind" \
    "$(jget "$I" "[i['kind'] for i in d['data']['interfaces'] if i['name']=='$_n'][0]")"
  # MTU：直接读 sysfs
  expect_eq "网卡 $_n 的 mtu 与 sysfs 一致" "$(cat "$_d/mtu" 2>/dev/null)" \
    "$(jget "$I" "[i['mtu'] for i in d['data']['interfaces'] if i['name']=='$_n'][0]")"
  # carrier：三态。sysfs 有 carrier 文件时必须是 true/false，没有时必须是 null。
  if [ -e "$_d/carrier" ]; then
    _cv=$(cat "$_d/carrier" 2>/dev/null)
    if [ "$_cv" = 1 ]; then _want_carrier=true; else _want_carrier=false; fi
  else
    _want_carrier=null
  fi
  expect_eq "网卡 $_n 的 carrier 三态与 sysfs 一致" "$_want_carrier" \
    "$(jbd "$I" "[i['carrier'] for i in d['data']['interfaces'] if i['name']=='$_n'][0]")"
done

# 现场有的网卡，响应里不能漏。方向是"现场 ⊆ 响应"：反过来（响应里有现场没有的）由上面
# 逐张 sysfs 存在的断言覆盖，两个方向都钉住。
missed=""
for _d in /sys/class/net/*; do
  [ -d "$_d" ] || continue
  _n=${_d##*/}
  case $names in
    *"\"$_n\""*) ;;
    *) missed="$missed $_n" ;;
  esac
done
if [ -z "$missed" ]; then
  pass "现场全部网卡都在响应里"
else
  fail "现场全部网卡都在响应里" "漏了:$missed"
fi

# 回环地址的 family/scope
expect_eq "lo 上有 ipv4 的 127.0.0.1" "[\"127.0.0.1\"]" \
  "$(jbd "$I" "[a['address'] for a in [i for i in d['data']['interfaces'] if i['name']=='lo'][0]['addresses'] if a['family']=='ipv4']")"
expect_eq "127.0.0.1 的前缀长度是 8" "8" \
  "$(jget "$I" "[a['prefix_len'] for a in [i for i in d['data']['interfaces'] if i['name']=='lo'][0]['addresses'] if a['address']=='127.0.0.1'][0]")"
expect_eq "127.0.0.1 的 scope 是 host" "host" \
  "$(jget "$I" "[a['scope'] for a in [i for i in d['data']['interfaces'] if i['name']=='lo'][0]['addresses'] if a['address']=='127.0.0.1'][0]")"

# 现场某张物理网卡上的地址，响应里必须有（用 ip 现场读，不信任响应）
first_eth=$(physical_eths | head -1)
if [ -n "$first_eth" ]; then
  live_v4=$(ip -4 -o addr show dev "$first_eth" 2>/dev/null | awk '{print $4}' | cut -d/ -f1 | head -1)
  if [ -n "$live_v4" ]; then
    expect_contains "现场 $first_eth 的地址 $live_v4 出现在响应里" "\"$live_v4\"" \
      "$(jbd "$I" "[a['address'] for i in d['data']['interfaces'] if i['name']=='$first_eth' for a in i['addresses']]")"
  else
    note "$first_eth 现场没有 IPv4 地址，跳过地址对照"
  fi
else
  fail "本机有物理以太网卡" "physical_eths 一个都没找到，后面几条判定会失去对照物"
fi

echo
echo "== 4. C：能不能改——用现场事实独立重算判定 =="

expect_true "响应里有 capability" "$(jbd "$I" "'capability' in d['data'] and d['data']['capability'] is not None")"
got_backend=$(jget "$I" 'd["data"]["capability"]["backend"]')
want_backend=$(expected_backend)
in_set "capability.backend 在闭集内" "$got_backend" \
  "netplan-networkd netplan-networkmanager networkmanager unknown"
expect_eq "capability.backend 与 /run 下的渲染产物一致" "$want_backend" "$got_backend"

phys_count=$(physical_eths | grep -c . )
complex=$(complex_kinds)
if [ "$(nm_running)" = yes ]; then _nm=yes; else _nm=no; fi
if [ "$want_backend" = netplan-networkd ] && [ -z "$complex" ] && [ "$phys_count" -eq 1 ] && [ "$_nm" = no ]; then
  want_editable=true
else
  want_editable=false
fi
got_editable=$(jbd "$I" "d['data']['capability']['editable']")
expect_eq "capability.editable 与现场事实一致（backend=$want_backend 物理网卡=$phys_count 复杂接口='$complex' NM=$_nm）" \
  "$want_editable" "$got_editable"

got_reasons=$(jbd "$I" "d['data']['capability']['reasons']")
if [ "$want_editable" = false ]; then
  reasons_n=$(jbd "$I" "len(d['data']['capability']['reasons'])")
  if [ "${reasons_n}" -ge 1 ] 2>/dev/null; then
    pass "不可改时给出了 $reasons_n 条原因"
  else
    fail "不可改时给出了原因" "reasons 长度 '$reasons_n'（$got_reasons）"
  fi
  # 原因是给人看的一句话，不是错误码。响应里的中文在 JSON 里是 \uXXXX 转义，
  # 所以"含 \u"就是"含中文"；纯 ASCII 的 reasons 多半是 "unsupported" 之类。
  case $got_reasons in
    *'\u'*) pass "原因是人话（含中文）" ;;
    *) fail "原因是人话" "reasons 里没有任何中文，看起来是错误码：$got_reasons" ;;
  esac
else
  expect_eq "可改时 reasons 是空数组" "[]" "$got_reasons"
  got_target=$(jget "$I" 'd["data"]["capability"]["target"]')
  expect_eq "可改时 target 就是那张唯一的物理网卡" "$first_eth" "$got_target"
  expect_eq "target 在网卡列表里且 physical=true" "true" \
    "$(jbd "$I" "any(i['name']=='$got_target' and i['physical'] for i in d['data']['interfaces'])")"
fi

echo
echo "== 5. D/G：GET /network/routes =="

code=$(apiout "$WORK/admin.jar" "$WORK/routes.json" "/api/v1/network/routes")
expect_eq "admin 读路由表 200" "200" "$code"
R=$WORK/routes.json

bad_family=$(jbd "$R" "sorted(set(r['family'] for r in d['data']['routes']) - {'ipv4','ipv6'})")
expect_eq "路由家族都在闭集内" "[]" "$bad_family"
bad_dest=$(jbd "$R" "sorted(set(r['destination'] for r in d['data']['routes'] if '/' not in r['destination']))")
expect_eq "目标一律写成网段（含前缀长度）" "[]" "$bad_dest"

# metric/pref_set 成对：pref_set=false 时 metric 必须是 0（0 本身是合法优先级）。
bad_pair=$(jbd "$R" "[r for r in d['data']['routes'] if not r['pref_set'] and r['metric'] != 0]")
expect_eq "pref_set=false 的路由 metric 是 0" "[]" "$bad_pair"

# 默认路由的标记与目标自洽
bad_default=$(jbd "$R" "[r for r in d['data']['routes'] if r['default'] != (r['destination'] in ('0.0.0.0/0','::/0'))]")
expect_eq "default 标记与目标网段自洽" "[]" "$bad_default"

# 现场默认网关必须在 gateways 里（带 family 与地址），这是"这台机器从哪儿出去"的一句话。
live_gw4=$(ip -4 route show default 2>/dev/null | awk '/default/ {print $3; exit}')
live_dev4=$(ip -4 route show default 2>/dev/null | awk '/default/ {for(i=1;i<=NF;i++) if($i=="dev") print $(i+1); exit}')
if [ -n "$live_gw4" ]; then
  expect_contains "gateways 里有现场 IPv4 默认网关 $live_gw4" "$live_gw4" \
    "$(jbd "$R" "[g['address'] for g in d['data']['gateways']]")"
  if [ -n "$live_dev4" ]; then
    expect_contains "默认网关的出接口是 $live_dev4" "$live_dev4" \
      "$(jbd "$R" "[g['device'] for g in d['data']['gateways']]")"
  fi
  # 默认路由必须同时出现在 routes 里（gateways 是便利视图，不是替代品）
  expect_contains "默认路由也在 routes 里" '"0.0.0.0/0"' \
    "$(jbd "$R" "[r['destination'] for r in d['data']['routes'] if r['family']=='ipv4']")"
else
  note "本机没有 IPv4 默认路由，跳过网关对照"
fi
live_gw6=$(ip -6 route show default 2>/dev/null | awk '/default/ {print $3; exit}')
if [ -n "$live_gw6" ]; then
  expect_contains "gateways 里有现场 IPv6 默认网关" "$live_gw6" \
    "$(jbd "$R" "[g['address'] for g in d['data']['gateways']]")"
  expect_contains "IPv6 默认路由也在 routes 里" '"::/0"' \
    "$(jbd "$R" "[r['destination'] for r in d['data']['routes'] if r['family']=='ipv6']")"
else
  note "本机没有 IPv6 默认路由，跳过 IPv6 网关对照"
fi

echo
echo "== 6. E/G：GET /network/listeners 与 /proc/net 现场对照 =="

code=$(apiout "$WORK/admin.jar" "$WORK/listeners.json" "/api/v1/network/listeners")
expect_eq "admin 读监听端口 200" "200" "$code"
L=$WORK/listeners.json

# 内部字段 inode 不能上线：它是内核对象的编号，对读者没用，也是"我们怎么反查的"实现细节。
expect_not_contains "响应里没有内部字段 inode" '"inode"' "$(cat "$L")"
expect_false "truncated=false（本机监听数远低于 2000 条上限）" "$(jbd "$L" "d['data']['truncated']")"

snap_listeners() {
  python3 - "$1" <<'PY'
import socket, sys

LISTEN = "0A"


def v4(h):
    return socket.inet_ntoa(bytes.fromhex(h)[::-1])


def v6(h):
    words = [h[i:i + 8] for i in range(0, 32, 8)]
    return socket.inet_ntop(socket.AF_INET6, b"".join(bytes.fromhex(w)[::-1] for w in words))


out = []
for path, fam, conv in (("/proc/net/tcp", "ipv4", v4), ("/proc/net/tcp6", "ipv6", v6)):
    try:
        with open(path, encoding="ascii", errors="replace") as fh:
            lines = fh.readlines()[1:]
    except OSError as exc:
        print("ERR:%s" % exc)
        raise SystemExit(1)
    for line in lines:
        cols = line.split()
        if len(cols) < 10 or cols[3] != LISTEN:
            continue
        addr, port = cols[1].split(":")
        out.append("%s|%s|%d" % (fam, conv(addr), int(port, 16)))
with open(sys.argv[1], "w", encoding="utf-8") as fh:
    fh.write("\n".join(sorted(set(out))) + "\n")
PY
}

SNAP=$WORK/listen-snapshot.txt
if snap_listeners "$SNAP"; then
  pass "现场 /proc/net/{tcp,tcp6} 的 LISTEN 集合读到了（$(grep -c . "$SNAP") 条）"
  cmp_out=$(python3 - "$SNAP" "$L" <<'PY'
import json, sys

with open(sys.argv[1], encoding="utf-8") as fh:
    snap = {line.strip() for line in fh if line.strip()}
with open(sys.argv[2], encoding="utf-8") as fh:
    resp = json.load(fh)["data"]["listeners"]
got = {"%s|%s|%d" % (l["family"], l["address"], int(l["port"]))
       for l in resp if l["protocol"] == "tcp"}
print("missing=" + ",".join(sorted(snap - got)))
print("extra=" + ",".join(sorted(got - snap)))
PY
)
  missing=$(printf '%s\n' "$cmp_out" | sed -n 's/^missing=//p')
  extra=$(printf '%s\n' "$cmp_out" | sed -n 's/^extra=//p')
  expect_eq "现场 TCP 监听一条都没漏" "" "$missing"
  expect_eq "响应里没有臆造的 TCP 监听" "" "$extra"
else
  fail "现场 /proc/net 的 LISTEN 集合读到了" "单个协议栈读不动，后面的比对没有对照物"
fi

# 标记们与绑定地址自洽：0.0.0.0/:: 是通配（对外），127.0.0.1/::1 是回环。
bad_wild=$(jbd "$L" "[l for l in d['data']['listeners'] if l['wildcard'] != (l['address'] in ('0.0.0.0','::'))]")
expect_eq "wildcard 标记与绑定地址自洽" "[]" "$bad_wild"
# loopback 标记：127.0.0.0/8 整段都是回环（systemd-resolved 的 127.0.0.53 就在这段里），
# 加上 ::1。只认 127.0.0.1 会把"回环段里的第二个地址"判成对外可达。
bad_loop=$(jbd "$L" "[l for l in d['data']['listeners'] if l['loopback'] and not (l['address'].startswith('127.') or l['address'] == '::1')]")
expect_eq "loopback 标记只出现在回环段上" "[]" "$bad_loop"

# 本面板自己的端口必须出现在它自己报的监听表里，且 pid 就是 Web 进程号。
# 这一条同时验了 inode 反查：端口是已知的，pid 是已知的，两边对上才算真的读到了持有者。
self_entries=$(jbd "$L" "[l for l in d['data']['listeners'] if l['port']==$PORT]")
expect_not_contains "本面板端口 $PORT 出现在监听表里" "[]" "$self_entries"
# 两条：① 报出来的 pid 确实是监听这个端口的那一个（与 runuser 的子进程号对上）；
# ② 它的 comm 是 upanel-web——这一条不依赖脚本自己的记账，直接问内核。
LI_PID=$(jget "$L" "[l['pid'] for l in d['data']['listeners'] if l['port']==$PORT][0]")
expect_eq "占用 $PORT 的 pid 就是 Web 进程" "$WEB_APP_PID" "$LI_PID"
if [ -n "$LI_PID" ] && [ "$LI_PID" != "0" ]; then
  expect_eq "这个 pid 的内核 comm 是 upanel-web" "upanel-web" "$(cat "/proc/$LI_PID/comm" 2>/dev/null)"
else
  fail "端口 $PORT 反查到了持有者 pid" "pid=$LI_PID"
fi

# sshd / systemd 的监听（端口 22）应当能反查到持有者：这一条验的是"root 跑代理时进程
# 归属真的读得到"。**不写死进程名**——22.04 是 sshd 自己听，24.04 用 ssh.socket 由
# PID 1（systemd）听、收到连接再交出去，两者都对。所以判据是"报出来的进程名与内核里
# 那个 pid 的 comm 一致"，这条不依赖我事先知道该是哪个进程。
ssh_pid=$(jget "$L" "[l['pid'] for l in d['data']['listeners'] if l['protocol']=='tcp' and l['port']==22][0]")
ssh_proc=$(jget "$L" "[l['process'] for l in d['data']['listeners'] if l['protocol']=='tcp' and l['port']==22][0]")
case "${ssh_pid:-}" in
  ''|0|*[!0-9]*)
    note "本机没有监听 22 的 TCP 端口（或反查不到持有者：pid=$ssh_pid），跳过进程反查对照"
    ;;
  *)
    expect_eq "22 端口报出的进程名与内核 comm 一致" "$(cat "/proc/$ssh_pid/comm" 2>/dev/null)" "$ssh_proc"
    ;;
esac

# 端口与协议闭集
bad_proto=$(jbd "$L" "sorted(set(l['protocol'] for l in d['data']['listeners']) - {'tcp','udp'})")
expect_eq "协议都在闭集内" "[]" "$bad_proto"

echo
echo "== 7. F/G：GET /firewall 与 ufw 现场对照 =="

code=$(apiout "$WORK/admin.jar" "$WORK/firewall.json" "/api/v1/firewall")
expect_eq "admin 读防火墙 200" "200" "$code"
F=$WORK/firewall.json

# `ufw` 是个 Python 脚本，慢机上第一次调用可能撞上命令上限（24.04 走 TCG 时实测到过），
# 产品会如实记一条 `ufw/timeout` 并回一份"规则读不到"的状态——那是正确答案，不是缺陷。
# 但就着这份状态去对照"规则条数"会变成 0 == 0 的假绿，所以重读一次：读得到才对照，
# 读不到就在下面的分支里记 NOT RUN。第一次超时这件事本身留在日志里。
if [ "$(jbd "$F" "any(i['source']=='ufw' and i['code']=='timeout' for i in d['data']['issues'])")" = true ]; then
  note "第一次读防火墙时 ufw 超时（产品如实记了 ufw/timeout），重读一次再对照"
  code=$(apiout "$WORK/admin.jar" "$WORK/firewall-retry.json" "/api/v1/firewall")
  expect_eq "重读防火墙 200" "200" "$code"
  F=$WORK/firewall-retry.json
fi

got_manager=$(jget "$F" 'd["data"]["manager"]')
in_set "firewall.manager 在闭集内" "$got_manager" "ufw firewalld nftables iptables-persistent none unknown"

ufw_bin=""
for _d in /usr/sbin /sbin /usr/bin /bin; do
  [ -x "$_d/ufw" ] && ufw_bin="$_d/ufw" && break
done
firewalld_present=no
for _d in /usr/sbin /sbin /usr/bin /bin; do
  [ -e "$_d/firewalld" ] && firewalld_present=yes
  [ -e "$_d/firewall-cmd" ] && firewalld_present=yes
done
[ -d /run/firewalld ] && firewalld_present=yes
# nftables 的"在管"证据是 enable 过的服务，不是 /etc/nftables.conf 在不在：
# 那个文件每台 Ubuntu 都有（包里自带的默认规则集），服务默认 disabled。
nft_enabled=no
for _w in /etc/systemd/system/*.wants/nftables.service; do
  if [ -e "$_w" ]; then nft_enabled=yes; break; fi
done
nft_conf=yes; [ -f /etc/nftables.conf ] || nft_conf=no
ipt_conf=no; { [ -f /etc/iptables/rules.v4 ] || [ -f /etc/iptables/rules.v6 ]; } && ipt_conf=yes
note "现场：nftables.conf=$nft_conf nftables.service(enable)=$nft_enabled iptables-persistent=$ipt_conf firewalld=$firewalld_present"

if [ -n "$ufw_bin" ]; then
  expect_true "本机有 ufw → installed=true" "$(jbd "$F" "d['data']['installed']")"
  if [ "$firewalld_present" = no ] && [ "$nft_enabled" = no ] && [ "$ipt_conf" = no ]; then
    expect_eq "ufw 独占 → manager=ufw" "ufw" "$got_manager"
    expect_true "ufw 独占 → editable=true" "$(jbd "$F" "d['data']['editable']")"
  else
    expect_eq "ufw 与别的管理器同存 → manager=unknown" "unknown" "$got_manager"
    expect_false "同存时 editable=false" "$(jbd "$F" "d['data']['editable']")"
    expect_eq "同存时给出了原因" "true" "$(jbd "$F" "len(d['data']['reasons']) >= 1")"
  fi

  # 响应自报"ufw 读不到"时，后面这些对照就没有依据了：读不到规则不等于没有规则，
  # 把 0 条期望和 0 条实际判成"一致"是假绿——24.04 首轮就是这样：ufw 在 TCG 上
  # 超过 6 秒的上限，产品如实记了一条 ufw/timeout，脚本却"验过"了规则条数。
  ufw_unreadable=$(jbd "$F" "any(i['source']=='ufw' and i['code'] in ('timeout','failed','permission_denied') for i in d['data']['issues'])")
  if [ "$ufw_unreadable" = true ]; then
    note "NOT RUN ufw 状态对照：响应自报读不到（$(jbd "$F" "[i['code'] for i in d['data']['issues'] if i['source']=='ufw']")）"
    note "现场自己读一遍只作参考：$("$ufw_bin" status numbered 2>&1 | head -2 | tr '\n' ' ')"
  else
    verbose=$("$ufw_bin" status verbose 2>&1)
    live_status=$(printf '%s\n' "$verbose" | sed -n 's/^Status: //p' | head -1)
    case $live_status in
      active)
        expect_true "ufw 现场 active → enabled=true" "$(jbd "$F" "d['data']['enabled']")"
        ;;
      inactive)
        expect_false "ufw 现场 inactive → enabled=false" "$(jbd "$F" "d['data']['enabled']")"
        ;;
      *)
        note "ufw status verbose 的第一行不是 Status:（现场：$live_status），跳过启用状态对照"
        ;;
    esac

    # 默认策略：从 "Default: deny (incoming), allow (outgoing), disabled (routed)" 里逐项取。
    # 逐项锚定再取词，不做贪婪匹配：`.*\([a-z]*\) (incoming)` 那种写法会只抓到词尾一个字母。
    defaults=$(printf '%s\n' "$verbose" | sed -n 's/^Default: //p' | head -1)
    live_incoming=$(printf '%s' "$defaults" | sed -n 's/^\([a-z]*\) (incoming).*/\1/p')
    live_outgoing=$(printf '%s' "$defaults" | sed -n 's/.*, \([a-z]*\) (outgoing).*/\1/p')
    live_routed=$(printf '%s' "$defaults" | sed -n 's/.*, \([a-z]*\) (routed).*/\1/p')
    if [ -n "$live_incoming" ]; then
      expect_eq "默认 incoming 策略与 ufw 现场一致" "$live_incoming" "$(jget "$F" 'd["data"]["default_incoming"]')"
      expect_eq "默认 outgoing 策略与 ufw 现场一致" "$live_outgoing" "$(jget "$F" 'd["data"]["default_outgoing"]')"
      expect_eq "默认 routed 策略与 ufw 现场一致" "$live_routed" "$(jget "$F" 'd["data"]["default_routed"]')"
    else
      note "ufw 现场没给出 Default 行（未启用时是这样），跳过默认策略对照"
    fi

    # 规则条数：`ufw status numbered` 里以 [ 开头的行就是要报出来的规则。
    live_rules=$("$ufw_bin" status numbered 2>/dev/null | grep -c '^\[' )
    expect_eq "规则条数与 ufw status numbered 一致" "$live_rules" \
      "$(jbd "$F" "len(d['data']['rules'])")"
  fi
  # 这两条只看响应自身的形状，不依赖现场读数，所以不放进上面的分支。
  bad_action=$(jbd "$F" "sorted(set(r['action'] for r in d['data']['rules']) - {'ALLOW','DENY','REJECT','LIMIT'})")
  expect_eq "规则 action 在闭集内" "[]" "$bad_action"
  bad_raw=$(jbd "$F" "[r for r in d['data']['rules'] if not r['raw']]")
  expect_eq "每条规则都带 UFW 原话" "[]" "$bad_raw"
else
  expect_false "本机没有 ufw → installed=false" "$(jbd "$F" "d['data']['installed']")"
  expect_eq "没有 ufw 时 manager 不是 ufw" "true" \
    "$(jbd "$F" "d['data']['manager'] != 'ufw'")"
  expect_false "没有 ufw 时 editable=false" "$(jbd "$F" "d['data']['editable']")"
  # 没装 ufw 必须有一条 missing 的 issue，而不是静默的"没有防火墙"
  expect_contains "没有 ufw 时 issues 里记了 ufw/missing" '"ufw"' \
    "$(jbd "$F" "[i['source'] for i in d['data']['issues']]")"
fi

if [ "$got_manager" != "ufw" ]; then
  reasons_n=$(jbd "$F" "len(d['data']['reasons'])")
  if [ "${reasons_n}" -ge 1 ] 2>/dev/null; then
    pass "非 ufw 时给出了 ${reasons_n} 条不可改的原因"
  else
    fail "非 ufw 时给出了不可改的原因" "reasons 长度 '$reasons_n'"
  fi
  expect_eq "manager_reasons 也给了（判定的依据）" "true" \
    "$(jbd "$F" "len(d['data']['manager_reasons']) >= 1")"
fi

echo
echo "== 8. G：issues 语义（四条都查） =="

for f in interfaces routes listeners firewall; do
  path=$WORK/$f.json
  expect_eq "$f：issues 是数组" "true" "$(jbd "$path" "isinstance(d['data']['issues'], list)")"
  bad_src=$(jbd "$path" "sorted(set(i['source'] for i in d['data']['issues']) - {'sysfs','ip','proc-net','netplan','ufw','systemd'})")
  expect_eq "$f：issues.source 都在闭集内" "[]" "$bad_src"
  bad_code=$(jbd "$path" "sorted(set(i['code'] for i in d['data']['issues']) - {'missing','permission_denied','timeout','unparsable','failed'})")
  expect_eq "$f：issues.code 都在闭集内" "[]" "$bad_code"
  empty_detail=$(jbd "$path" "[i for i in d['data']['issues'] if not i.get('detail')]")
  expect_eq "$f：每条 issue 都带说明" "[]" "$empty_detail"
  n=$(jbd "$path" "len(d['data']['issues'])")
  note "$f 的 issues 条数：$n"
done

echo
echo "== 9. H：只读——四条请求不碰系统 =="

# 只比"写路径将来会碰的那些文件"：netplan 的配置源与 ufw 的规则集。比 `ip addr` 的输出
# 没有意义——DHCP 租约的 valid_lft 每秒都在变，那种红是尺子自己在动。
before_netplan=$(hash_tree /etc/netplan)
before_ufw=$(hash_tree /etc/ufw)
before_sysnet=$(hash_tree /etc/systemd/network)

apiout "$WORK/admin.jar" "$WORK/reread1.json" "/api/v1/network/interfaces" >/dev/null
apiout "$WORK/admin.jar" "$WORK/reread2.json" "/api/v1/network/routes" >/dev/null
apiout "$WORK/admin.jar" "$WORK/reread3.json" "/api/v1/network/listeners" >/dev/null
apiout "$WORK/admin.jar" "$WORK/reread4.json" "/api/v1/firewall" >/dev/null

after_netplan=$(hash_tree /etc/netplan)
after_ufw=$(hash_tree /etc/ufw)
after_sysnet=$(hash_tree /etc/systemd/network)

expect_eq "/etc/netplan 的清单与哈希没变" "$before_netplan" "$after_netplan"
expect_eq "/etc/ufw 的清单与哈希没变" "$before_ufw" "$after_ufw"
expect_eq "/etc/systemd/network 的清单与哈希没变" "$before_sysnet" "$after_sysnet"

# 两次读的 collected_at 必须不同（除极端快的情况）：它们是两次独立的采集，不是缓存的
# 固定成功。差 0 秒说明"每次请求都真的去读了"，差 0 说明可能是个写死的常量。
c1=$(jget "$WORK/listeners.json" 'd["data"]["collected_at"]')
c2=$(jget "$WORK/reread3.json" 'd["data"]["collected_at"]')
if [ "$c1" = "$c2" ]; then
  note "两次采集的 collected_at 相同（$c1）：同一秒内完成，不作为失败"
else
  pass "两次采集的 collected_at 不同（$c1 → $c2），不是缓存"
fi

echo
echo "== 10. I：卫生 =="

if grep -qiE 'panic:|goroutine [0-9]+ \[' "$AGENT_LOG" "$WEB_LOG" 2>/dev/null; then
  fail "日志里没有 panic" "$(grep -im1 -E 'panic:' "$AGENT_LOG" "$WEB_LOG" 2>/dev/null)"
else
  pass "日志里没有 panic"
fi
if kill -0 "$AGENT_PID" 2>/dev/null; then pass "代理仍在运行"; else fail "代理仍在运行" "进程已退出"; fi
if kill -0 "$WEB_PID" 2>/dev/null; then pass "Web 仍在运行"; else fail "Web 仍在运行" "进程已退出"; fi

# 审计：被拒的读要留痕，成功的读不留痕。
#
# 前半条是安全要求（"谁在什么时候试图看这台机器的网络全貌"），后半条是可用性要求：
# 页面一打开就是四条 GET，刷新一次再加四条——成功的读也记的话，审计表里只剩这一种行，
# 真正的管理事件会被淹掉（与 §9 收藏不写审计同一条理由，见 CONTRACTS §13.5）。
denied_rows=$(sqlq "SELECT COUNT(*) FROM audit_events WHERE action IN ('network.manage', 'firewall.manage') AND outcome = 'failure'")
if [ "${denied_rows:-x}" -ge 4 ] 2>/dev/null; then
  pass "viewer 的四次越权读取各落了一条审计（$denied_rows 条 failure）"
else
  fail "viewer 的四次越权读取各落了一条审计" "audit_events 里只有 '$denied_rows' 条，期望 ≥4"
fi
ok_rows=$(sqlq "SELECT COUNT(*) FROM audit_events WHERE action IN ('network.manage', 'firewall.manage') AND outcome = 'success'")
if [ "${ok_rows:-x}" = "0" ] 2>/dev/null; then
  pass "成功的读取不写审计（页面刷新不会把审计表灌满）"
else
  fail "成功的读取不写审计" "出现了 '$ok_rows' 条 outcome=success 的读取审计"
fi

echo
printf '通过 %d 项，失败 %d 项\n' "$PASSED" "$FAILED"
if [ -n "$FAILED_NAMES" ]; then printf '失败项:%s\n' "$FAILED_NAMES"; fi
if [ "$FAILED" -gt 0 ]; then exit 1; fi
exit 0
