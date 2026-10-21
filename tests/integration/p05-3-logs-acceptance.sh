#!/bin/sh
# P05.3 Linux 验收脚本：journal 结构化查询、筛选、游标翻页、实时流与子进程回收——
# 在真实 journald 上逐条验证。
#
# 运行位置：Ubuntu 22.04/24.04 验收 VM 内，**必须以 root 运行**（代理只能在 root 下
# 启动，并用 upanel-web 身份去查 socket；生成日志要用 systemd-run 与 /dev/kmsg）。
# 不依赖 Go 工具链：用宿主交叉编译好的 Linux 二进制。
#
# 用法：
#   sudo BIN_DIR=/tmp/upanel-bin sh tests/integration/p05-3-logs-acceptance.sh
#
# 可选环境变量：
#   PORT=19451            Web 监听端口
#   WAIT_IDLE=1           是否等 60 秒空闲超时那一帧（0 则记 NOT RUN）
#   STREAM_IDLE_WAIT=90   等空闲结束帧的上限秒数
#
# 退出码：0 全部通过；1 有失败项；2 环境不满足（非 root）。
#
# 覆盖的验收点（STAGES §7 P05.3、CONTRACTS §10/§10.3、PLAN §14）：
#   1. Web 非 root、代理 root、socket 归 socket_group；代理能力表里有 logs.query
#   2. 按 unit 与时间窗口查询；unit 过滤是**限制性**的，不是装饰
#   3. 按级别过滤（下限语义）：level=error 拿不到 info 行，level=info 拿得到 error 行
#   4. 时间窗口边界：from≥to、跨度>24h、手写日期、未来的 to 全部 400
#   5. 条数上限（1—1000）与单页字节上限（1 MiB）真的生效，并被显式标记 truncated
#   6. 降序键集翻页：跨页无重复游标、每个标记出现且只出现一次、到底 next_cursor=null
#   7. 被 journalctl 拒绝的游标回 400，而不是静默变成“没有更早的日志”
#   8. 秘密过滤：password=/token= 被抹掉且 redacted=true；普通行 redacted=false
#   9. 权限：未登录 401、viewer 403（两条路径都是）、admin 200
#  10. 实时流：SSE 头、`id` 是 journal 游标、事件名 entry/end、Last-Event-ID 续传不重复
#  11. **结束订阅不残留 journalctl**：流在跑时子进程存在，断开后 15 秒内归零
#  12. 并发上限：同一账号第 5 条流 429，前 4 条不受影响
#  13. 空闲超时：安静系统上 60 秒后收到 end(reason=idle_timeout)，不是永远挂着
#
# 明确不覆盖（NOT RUN，原因写在这里，不在脚本里假装通过）：
#   - 单条消息 8 KiB 截断（truncated=true）：**真实 journald 给不出来**。实测 systemd 255
#     对超过 4086 字节的 MESSAGE 直接返回 null（字节数组），我们的代码把它标成
#     binary=true、message=""，这条在 §6 里有真实断言；而“8 KiB 以上的字符串被截断”
#     只能由 internal/journal/journal_test.go 的合成输入覆盖。
#   - 浏览器渲染：VM 上没有浏览器。页面级证据在宿主上用真实载荷回放
#     （web/src/components/realdata-logs.test.ts 读 web/src/fixtures/p05-3-vm/）。
#   - journald 磁盘不可写 / journal 目录被删：需要故意破坏系统日志，不属于本阶段验收。

set -u

BIN_DIR=${BIN_DIR:-/tmp/upanel-bin}
HERE=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)

if [ "$(id -u)" -ne 0 ]; then
  printf '本脚本必须以 root 运行：sudo BIN_DIR=%s sh %s\n' "$BIN_DIR" "$0" >&2
  exit 2
fi

AGENT="$BIN_DIR/upanel-agent"
WEB="$BIN_DIR/upanel-web"
CTL="$BIN_DIR/upanelctl"
VALIDATE="$HERE/p05-3-payload-validate.py"

WORK=${WORK:-/var/tmp/upanel-accept-p05-3}
PAYLOADS=$WORK/payloads
RUN_DIR=/run/upanel
SOCK=$RUN_DIR/agent.sock
SYS_CFG=/etc/upanel/upanel.json
AGENT_DB_DIR=/var/lib/upanel-agent
AGENT_DB=$AGENT_DB_DIR/agent.db
KEY_FILE=/etc/upanel/agent.key
WEB_DB_DIR=$WORK/webdata
WEB_DB=$WEB_DB_DIR/upanel.db
PORT=${PORT:-19451}
BASE="http://127.0.0.1:$PORT"
ORIGIN="$BASE"
WEB_USER=upanel-web
PASSWORD='vm-correct-horse-9'
VIEWER_PASSWORD='vm-correct-horse-8'

# 本次验收生成日志用的单元名。用独立的瞬时单元而不是随便 logger 一句：
# unit 过滤必须真的按 _SYSTEMD_UNIT 命中，用别的进程写进来的日志验不了这一条。
# 单元名带本次进程号：同名单元在 journal 里是累加的，上一轮（或手工探针）留下的
# 同名日志会把“password= 只有一条”这类断言顶掉。名字仍然满足 unit 形状校验。
RUN_TAG=$$
UNIT_MARK=upanel-accept-logs-$RUN_TAG.service
UNIT_BIG=upanel-accept-big-$RUN_TAG.service
UNIT_HUGE=upanel-accept-huge-$RUN_TAG.service
UNIT_STREAM=upanel-accept-stream-$RUN_TAG.service
IDENT=upanel-accept
BIG_ENTRIES=300
# 单条 MESSAGE 超过 4086 字节就变 null，"big-N=" 前缀最长 8 字节，正文留 4000，
# 这样 journald 里存的是字符串而不是 byte array（否则撞不到页字节上限）。
BIG_BYTES=4000
MARKERS=30

LOGS_PATH=/api/v1/logs
STREAM_PATH=/api/v1/logs/stream
WAIT_IDLE=${WAIT_IDLE:-1}
STREAM_IDLE_WAIT=${STREAM_IDLE_WAIT:-90}

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

# jget <文件> <python 表达式，d 为解析后的 JSON>
jget() {
  python3 -c 'import json,sys
d=json.load(open(sys.argv[1], encoding="utf-8"))
print(eval(sys.argv[2]))' "$1" "$2" 2>/dev/null
}

# jgetx <python 表达式>：读 $WORK/resp.json（不落盘时的临时判断）。
resp_get() {
  python3 -c 'import json,sys
try:
    d=json.load(open(sys.argv[1], encoding="utf-8"))
except Exception:
    print("")
    raise SystemExit
print(eval(sys.argv[2]))' "$WORK/resp.json" "$1" 2>/dev/null
}

# dbq <数据库文件> <SQL>：只读查询，返回第一行第一列。
dbq() {
  python3 - "$1" "$2" <<'PY'
import sqlite3, sys
con = sqlite3.connect("file:%s?mode=ro" % sys.argv[1], uri=True)
try:
    row = con.execute(sys.argv[2]).fetchone()
    print("" if row is None else ("" if row[0] is None else row[0]))
finally:
    con.close()
PY
}

journalctl_children() {
  _n=$(pgrep -c journalctl 2>/dev/null) || _n=0
  printf '%s' "${_n:-0}"
}

cleanup() {
  kill_streams
  stop_web
  stop_agent
  # 验收用的瞬时单元不该留活着的进程；-collect 会在退出后自动清掉单元。
  for unit in "$UNIT_MARK" "$UNIT_BIG" "$UNIT_HUGE" "$UNIT_STREAM"; do
    systemctl reset-failed "$unit" >/dev/null 2>&1
  done
}
trap cleanup EXIT
trap 'cleanup; exit 130' INT
trap 'cleanup; exit 143' TERM

kill_streams() {
  for pid in ${STREAM_PIDS:-}; do
    kill -TERM "$pid" 2>/dev/null
  done
  sleep 0.3
  for pid in ${STREAM_PIDS:-}; do
    kill -KILL "$pid" 2>/dev/null
  done
  STREAM_PIDS=""
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

# api <jar> <csrf> <方法> <路径> [body]：响应写到 $WORK/resp.json。
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

# query <载荷名> <查询串>：GET /logs 并保存载荷，回显 HTTP 状态码。
# 载荷按名字固定保存——载荷校验程序按名字找文件，缺一个就是 FAIL，不留“这次没跑”的余地。
query() {
  _code=$(api "$JAR" "$CSRF" GET "$LOGS_PATH${2}")
  cp "$WORK/resp.json" "$PAYLOADS/$1" 2>/dev/null
  printf '%s' "$_code"
}

# query_anon <载荷名> <查询串>：不带 Cookie，验未登录。
query_anon() {
  _code=$(curl -s -o "$WORK/resp.json" -w '%{http_code}' "$BASE$LOGS_PATH${2}")
  cp "$WORK/resp.json" "$PAYLOADS/$1" 2>/dev/null
  printf '%s' "$_code"
}

# emit_unit <单元名> <shell 脚本>：在瞬时单元里执行一段脚本，退出后单元自动清掉。
# 这样写出来的日志带 _SYSTEMD_UNIT=<单元名>，unit 过滤才有真实对象可测。
emit_unit() {
  systemd-run --unit="$1" --wait --collect --quiet --property=Type=oneshot \
    /bin/sh -c "$2" >/dev/null 2>>"$WORK/logs/emit.log"
}

iso_now() { date -u +%Y-%m-%dT%H:%M:%SZ; }
iso_shift() { date -u -d "$1" +%Y-%m-%dT%H:%M:%SZ; }
# 游标里带 ';'（journald 的游标格式），拼进 URL 必须 percent-encode：
# 未转义的 ';' 在 Go 的 url.ParseQuery 里是非法分隔符，整对参数会被丢掉，
# 服务端于是把它当成“没给游标”，翻页拿着同一个 next_cursor 原地打转。
urlenc() { python3 -c 'import sys,urllib.parse;print(urllib.parse.quote(sys.argv[1],safe=""))' "$1"; }

# stream_open <前缀> <查询串> [额外 curl 参数...]：后台开一条 SSE，帧写到 $WORK/<前缀>.frames
stream_open() {
  _prefix=$1; _qs=$2; shift 2
  _out=$WORK/$_prefix.frames
  : >"$_out"
  curl -s -N -b "$JAR" -D "$WORK/$_prefix.headers" --max-time 120 \
    "$@" "$BASE$STREAM_PATH$_qs" >>"$_out" 2>"$WORK/$_prefix.err" &
  STREAM_PIDS="$STREAM_PIDS $!"
  eval "STREAM_PID_$(echo "$_prefix" | tr -cd 'a-z0-9_')=$!"
  printf '%s' "$!"
}

# stream_wait <前缀> <模式> <超时秒>：等帧文件里出现某段文字。
stream_wait() {
  _prefix=$1; _pattern=$2; _limit=$3
  _i=0
  _max=$((_limit * 10))
  while [ "$_i" -lt "$_max" ]; do
    if grep -q -- "$_pattern" "$WORK/$_prefix.frames" 2>/dev/null; then return 0; fi
    sleep 0.1
    _i=$((_i + 1))
  done
  return 1
}

# stream_last_id <前缀>：帧文件里最后一个 `id: ...` 的值（journal 游标）。
stream_last_id() {
  sed -n 's/^id: //p' "$WORK/$1.frames" 2>/dev/null | tail -1
}

# stream_kill <前缀>：关掉某条流，等 curl 退出。
stream_kill() {
  _pid=$(eval "printf '%s' \"\${STREAM_PID_$(echo "$1" | tr -cd 'a-z0-9_')-}\"")
  if [ -n "${_pid:-}" ]; then
    kill -TERM "$_pid" 2>/dev/null
    wait "$_pid" 2>/dev/null
  fi
  return 0
}

# wait_no_journalctl <超时秒>：等到没有 journalctl 子进程为止，回显最后看到的数量。
wait_no_journalctl() {
  _limit=$1; _i=0; _n=1
  _max=$((_limit * 4))
  while [ "$_i" -lt "$_max" ]; do
    _n=$(journalctl_children)
    [ "${_n:-0}" -eq 0 ] && { printf '0'; return 0; }
    sleep 0.25
    _i=$((_i + 1))
  done
  printf '%s' "${_n:-?}"
  return 1
}

echo "== 0. 环境 =="
if [ "$(id -u)" -eq 0 ]; then pass "验收脚本以 root 运行"; else fail "验收脚本以 root 运行" "UID $(id -u)"; fi
for b in "$AGENT" "$WEB" "$CTL"; do
  if [ -x "$b" ]; then pass "二进制存在且可执行 $(basename "$b")"; else fail "二进制存在且可执行 $(basename "$b")" "$b"; fi
done
if [ -f "$VALIDATE" ]; then pass "载荷校验程序存在"; else fail "载荷校验程序存在" "$VALIDATE"; fi
for t in python3 journalctl systemd-run logger curl; do
  if command -v "$t" >/dev/null 2>&1; then pass "依赖命令可用 $t"; else fail "依赖命令可用 $t" "缺少 $t"; fi
done
if [ -w /dev/kmsg ]; then pass "/dev/kmsg 可写（能制造一条可定位的内核日志）"; else notrun "内核日志断言" "/dev/kmsg 不可写"; fi

if ! id -u "$WEB_USER" >/dev/null 2>&1; then
  note "创建系统账号 $WEB_USER"
  useradd --system --no-create-home --shell /usr/sbin/nologin "$WEB_USER" 2>/dev/null
fi
if id -u "$WEB_USER" >/dev/null 2>&1; then pass "Web 账号 $WEB_USER 存在"; else fail "Web 账号 $WEB_USER 存在" "无法创建"; fi

if curl -fsS "$BASE/api/v1/health/live" >/dev/null 2>&1; then
  echo "$PORT 上已有服务在监听；先停掉遗留的 upanel-web 再跑验收" >&2
  exit 1
fi

rm -rf "$WORK"
mkdir -p "$WORK/logs" "$PAYLOADS" "$WEB_DB_DIR"
chmod 0755 "$WORK" "$PAYLOADS" "$WORK/logs"
chown -R "$WEB_USER" "$WEB_DB_DIR"
STREAM_PIDS=""
systemctl --version 2>/dev/null | head -1 >"$WORK/logs/systemd-version.txt" || true

echo
echo "== 1. 配置与启动（代理 root、Web 非 root） =="
install -d -m 0755 /etc/upanel
install -d -m 0700 "$AGENT_DB_DIR"
rm -f "$AGENT_DB" "$AGENT_DB-wal" "$AGENT_DB-shm"
install -d -m 0755 "$RUN_DIR"
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
if grep -q '已启用日志读取' "$AGENT_LOG" 2>/dev/null; then
  pass "代理已启用日志读取（journalctl 已定位）"
else
  fail "代理已启用日志读取" "$(grep -c '未找到 journalctl' "$AGENT_LOG" 2>/dev/null) 行“未找到 journalctl”"
fi

# 能力表：这是“能力清单不撒谎”的同一个接口，少报等于隐藏功能，多报等于编造。
# 两个细节都踩过：接口只认 Web 账号（root 得到 403），字段名是 supported_queries。
runuser -u "$WEB_USER" -- curl -s --unix-socket "$SOCK" \
  http://localhost/internal/v1/capabilities >"$PAYLOADS/agent-capabilities.json" 2>/dev/null
CAP_QUERIES=$(jget "$PAYLOADS/agent-capabilities.json" 'sorted(d["data"]["supported_queries"])')
expect_eq "能力表里登记了 logs.query" "True" \
  "$(jget "$PAYLOADS/agent-capabilities.json" '"logs.query" in d["data"]["supported_queries"]')"
note "代理登记的查询：$CAP_QUERIES"

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
expect_eq "agent.sock 归 root:$WEB_USER" "root:$WEB_USER" "$(stat -c '%U:%G' "$SOCK" 2>/dev/null)"
expect_eq "agent.sock 不是 0666" "no" \
  "$(stat -c '%a' "$SOCK" 2>/dev/null | grep -qx 666 && echo yes || echo no)"

JAR=$WORK/admin.jar
expect_eq "管理员登录" "200" "$(login admin "$PASSWORD" "$JAR" login-admin)"
CSRF=$(csrf_of "$WORK/login-admin.json")
if [ -n "$CSRF" ]; then pass "拿到 CSRF 令牌"; else fail "拿到 CSRF 令牌" "登录响应里没有 csrf_token"; fi

api "$JAR" "$CSRF" POST /api/v1/users \
  "{\"username\":\"viewer\",\"display_name\":\"只读\",\"role\":\"viewer\",\"initial_password\":\"$VIEWER_PASSWORD\"}" \
  >"$WORK/viewer-code.txt"
expect_eq "创建 viewer 账号" "201" "$(cat "$WORK/viewer-code.txt")"
chown_web_db
VJAR=$WORK/viewer.jar
expect_eq "viewer 登录" "200" "$(login viewer "$VIEWER_PASSWORD" "$VJAR" login-viewer)"

echo
echo "== 2. 制造可定位的真实日志 =="
T_START=$(iso_shift '-2 sec')
# 标记日志交给 p05-3-emit-markers.sh：它用一个常驻的 systemd-cat 写入并回读自证。
# 用 logger 一条条写时 journald 偶尔记不住发送者的 cgroup（实测 30 条丢 3 条的
# _SYSTEMD_UNIT），那几条就不在 `journalctl -u` 里，断言会莫名其妙地失败。
emit_unit "$UNIT_MARK" "sh '$BIN_DIR/tests/integration/p05-3-emit-markers.sh' '$UNIT_MARK' '$MARKERS' '$IDENT'"
emit_unit "$UNIT_STREAM" 'logger -t '"$IDENT"' "stream-baseline-1"'
if [ -w /dev/kmsg ]; then
  echo "upanel-accept-kmsg-marker-1 内核日志注入" >/dev/kmsg 2>/dev/null || true
fi
# 大页：300 条约 4 KiB 的字符串消息，用来撞单页 1 MiB 上限。
# 单条 4 KiB 是刻意的：实测 systemd 255 对超过 4086 字节的 MESSAGE 直接给 null，
# 那种消息在 API 里是 binary=true、message=""，不计入页字节——拿它撞字节上限是撞不动的。
emit_unit "$UNIT_BIG" '
python3 -c "
import sys
n = '"$BIG_ENTRIES"'
size = '"$BIG_BYTES"'
w = sys.stdout.write
for i in range(1, n + 1):
    w(\"big-%d=\" % i + \"p\" * size + \"\\n\")
" | systemd-cat -t '"$IDENT"'-big
'
# 单条超长消息：journald 会给 null（不是字符串），API 必须标成 binary 而不是一条空日志。
emit_unit "$UNIT_HUGE" '
python3 -c "
import sys
sys.stdout.write(\"huge=\" + \"z\" * 20000 + \"\\n\")
" | systemd-cat -t '"$IDENT"'-huge
'
sleep 1
T_END=$(iso_shift '+1 min')
WIN="from=$T_START&to=$T_END"
note "日志窗口 $T_START → $T_END"
MARK_COUNT=$(journalctl -u "$UNIT_MARK" --since "$T_START" -o cat --no-pager 2>/dev/null | wc -l | tr -d ' ')
note "$UNIT_MARK 在这个窗口里有 $MARK_COUNT 行（至少 $((MARKERS + 3))：$MARKERS 条标记 + 3 条特殊条目，另加 systemd 自己写的 Starting/Finished）"
if [ "${MARK_COUNT:-0}" -ge $((MARKERS + 3)) ]; then
  pass "标记日志已写入 journald（$MARK_COUNT 行）"
else
  fail "标记日志已写入 journald" "只有 $MARK_COUNT 行"
fi

echo
echo "== 3. 按 unit 与时间窗口查询 =="
expect_eq "GET /logs（unit+窗口）状态码" "200" "$(query logs-basic.json "?source=journal&unit=$UNIT_MARK&$WIN&limit=200")"
expect_eq "数据源回显 journal" "journal" "$(jget "$PAYLOADS/logs-basic.json" 'd["data"]["source"]')"
expect_eq "unit 过滤回显" "$UNIT_MARK" "$(jget "$PAYLOADS/logs-basic.json" 'd["data"]["unit"]')"
expect_eq "entries 非空" "True" "$(jget "$PAYLOADS/logs-basic.json" 'len(d["data"]["entries"]) > 0')"
# 注意 init.scope：systemd 自己写的 “Starting <unit>” / “Finished <unit>”，
# _SYSTEMD_UNIT 是 init.scope（它记的是“systemd 在说这个单元”），而 journalctl -u
# 仍然把它们算进该单元。API 如实回传原始字段，所以这两种都算数。
expect_eq "所有条目都属于该 unit" "True" \
  "$(jget "$PAYLOADS/logs-basic.json" 'all(e.get("unit") in {"'"$UNIT_MARK"'", "init.scope"} and (e.get("unit") != "init.scope" or "'"$UNIT_MARK"'" in e["message"]) for e in d["data"]["entries"])')"
expect_eq "时间倒序（最新在前）" "True" \
  "$(jget "$PAYLOADS/logs-basic.json" 'all(a["timestamp"] >= b["timestamp"] for a, b in zip(d["data"]["entries"], d["data"]["entries"][1:]))')"
expect_eq "每条都有游标" "True" \
  "$(jget "$PAYLOADS/logs-basic.json" 'all(isinstance(e.get("cursor"), str) and e["cursor"] != "" for e in d["data"]["entries"])')"
expect_eq "游标不重复" "True" \
  "$(jget "$PAYLOADS/logs-basic.json" 'len({e["cursor"] for e in d["data"]["entries"]}) == len(d["data"]["entries"])')"
expect_eq "级别都在闭集内" "True" \
  "$(jget "$PAYLOADS/logs-basic.json" 'all(e["level"] in {"emergency","alert","critical","error","warning","notice","info","debug","unknown"} for e in d["data"]["entries"])')"
expect_eq "能读到我们写的标记" "True" \
  "$(jget "$PAYLOADS/logs-basic.json" 'sum(1 for e in d["data"]["entries"] if "marker-i=" in e["message"]) >= '"$MARKERS"'')"
expect_eq "identifier 是写入者自报的名字" "True" \
  "$(jget "$PAYLOADS/logs-basic.json" 'any(e.get("identifier") == "'"$IDENT"'" for e in d["data"]["entries"])')"

# unit 过滤必须是限制性的：同一个窗口换成 ssh.service，我们的标记一条都不该出现。
expect_eq "换一个 unit 查询状态码" "200" "$(query logs-other-unit.json "?source=journal&unit=ssh.service&$WIN&limit=200")"
expect_eq "unit 过滤是限制性的（别的 unit 里没有我们的标记）" "0" \
  "$(jget "$PAYLOADS/logs-other-unit.json" 'sum(1 for e in d["data"]["entries"] if "marker-i=" in e["message"] or "big-" in e["message"])')"

echo
echo "== 4. 按级别过滤（下限语义） =="
expect_eq "level=error 状态码" "200" "$(query logs-level-error.json "?source=journal&unit=$UNIT_MARK&$WIN&level=error&limit=200")"
expect_eq "level=error 拿得到 error 行" "True" \
  "$(jget "$PAYLOADS/logs-level-error.json" 'any("marker-err-1" in e["message"] for e in d["data"]["entries"])')"
expect_eq "level=error 拿不到 info 行" "0" \
  "$(jget "$PAYLOADS/logs-level-error.json" 'sum(1 for e in d["data"]["entries"] if "marker-i=" in e["message"])')"
expect_eq "level=error 里所有级别都不比 error 轻" "True" \
  "$(jget "$PAYLOADS/logs-level-error.json" 'all(e["level"] in {"emergency","alert","critical","error"} for e in d["data"]["entries"])')"
expect_eq "level 过滤回显" "error" "$(jget "$PAYLOADS/logs-level-error.json" 'd["data"]["level"]')"

expect_eq "level=info 状态码" "200" "$(query logs-level-info.json "?source=journal&unit=$UNIT_MARK&$WIN&level=info&limit=200")"
expect_eq "level=info 同时拿得到 info 与 error" "True" \
  "$(jget "$PAYLOADS/logs-level-info.json" 'any("marker-i=" in e["message"] for e in d["data"]["entries"]) and any("marker-err-1" in e["message"] for e in d["data"]["entries"])')"

expect_eq "level=unknown 不是合法入参" "400" "$(query err-level-unknown.json "?level=unknown&$WIN")"
expect_eq "level=bogus 是 400" "400" "$(query err-level-bogus.json "?level=bogus&$WIN")"

echo
echo "== 5. 时间窗口边界 =="
FROM_LATE=$(iso_shift '-1 min')
TO_EARLY=$(iso_shift '-2 min')
expect_eq "from 晚于 to 是 400" "400" "$(query err-order.json "?from=$FROM_LATE&to=$TO_EARLY")"
SPAN_FROM=$(iso_shift '-30 hours')
expect_eq "跨度超过 24 小时是 400" "400" "$(query err-span.json "?from=$SPAN_FROM&to=$T_END")"
expect_eq "手写日期不是 RFC 3339，400" "400" "$(query err-from-format.json '?from=2026-10-21')"
expect_eq "to=明天 是 400" "400" "$(query err-to-format.json '?to=tomorrow')"
FUTURE=$(iso_shift '+3 hours')
expect_eq "未来的 to 是 400" "400" "$(query err-future.json "?from=$T_START&to=$FUTURE")"

expect_eq "只给 from 状态码" "200" "$(query logs-from-only.json "?source=journal&unit=$UNIT_MARK&from=$T_START&limit=5")"
# 只给 from 时 to 取“现在”，跨度因此是 now-from，不是 1 小时；
# 反过来，只给 to 时 from 才是 to-1 小时。两条一起才能说明窗口是怎么补的。
expect_eq "只给 from 时 to 取当前时间" "True" \
  "$(python3 - "$PAYLOADS/logs-from-only.json" "$T_START" <<'PYEOF'
import json, sys, datetime as dt
d = json.load(open(sys.argv[1], encoding="utf-8"))["data"]
f = dt.datetime.fromisoformat(d["from"].replace("Z", "+00:00"))
t = dt.datetime.fromisoformat(d["to"].replace("Z", "+00:00"))
want = dt.datetime.fromisoformat(sys.argv[2].replace("Z", "+00:00"))
now = dt.datetime.now(dt.timezone.utc)
print(f == want and abs((now - t).total_seconds()) <= 120 and 0 < (t - f).total_seconds() <= 3600)
PYEOF
)"
TO_ONLY=$(iso_shift '-30 min')
expect_eq "只给 to 状态码" "200" "$(query logs-to-only.json "?source=journal&unit=$UNIT_MARK&to=$TO_ONLY&limit=5")"
expect_eq "只给 to 时窗口是前 1 小时" "True" \
  "$(python3 - "$PAYLOADS/logs-to-only.json" "$TO_ONLY" <<'PYEOF'
import json, sys, datetime as dt
d = json.load(open(sys.argv[1], encoding="utf-8"))["data"]
f = dt.datetime.fromisoformat(d["from"].replace("Z", "+00:00"))
t = dt.datetime.fromisoformat(d["to"].replace("Z", "+00:00"))
want = dt.datetime.fromisoformat(sys.argv[2].replace("Z", "+00:00"))
print(t == want and (t - f).total_seconds() == 3600)
PYEOF
)"
expect_eq "什么都不给状态码（默认窗口）" "200" "$(query logs-default-window.json '?source=journal&limit=5')"
expect_eq "默认窗口是最近 1 小时" "True" \
  "$(python3 - "$PAYLOADS/logs-default-window.json" <<'PYEOF'
import json, sys, datetime as dt
d = json.load(open(sys.argv[1], encoding="utf-8"))["data"]
f = dt.datetime.fromisoformat(d["from"].replace("Z", "+00:00"))
t = dt.datetime.fromisoformat(d["to"].replace("Z", "+00:00"))
now = dt.datetime.now(dt.timezone.utc)
print((t - f).total_seconds() == 3600 and abs((now - t).total_seconds()) <= 120)
PYEOF
)"

echo
echo "== 6. 条数上限、单页字节上限与截断标记 =="
expect_eq "limit=0 是 400" "400" "$(query err-limit-zero.json '?limit=0')"
expect_eq "limit=1001 是 400" "400" "$(query err-limit-1001.json '?limit=1001')"
expect_eq "limit=abc 是 400" "400" "$(query err-limit-text.json '?limit=abc')"
expect_eq "limit=5 状态码" "200" "$(query logs-limit5.json "?source=journal&unit=$UNIT_MARK&$WIN&limit=5")"
expect_eq "limit=5 只回 5 条" "5" "$(jget "$PAYLOADS/logs-limit5.json" 'len(d["data"]["entries"])')"
expect_eq "limit=5 时 truncated=true（不是只有这些）" "True" "$(jget "$PAYLOADS/logs-limit5.json" 'd["data"]["truncated"]')"
expect_eq "limit=5 时给了 next_cursor" "True" "$(jget "$PAYLOADS/logs-limit5.json" 'isinstance(d["data"]["next_cursor"], str)')"

expect_eq "大页查询状态码" "200" "$(query logs-page-bytes.json "?source=journal&unit=$UNIT_BIG&$WIN&limit=1000")"
expect_eq "单页字节上限触发 truncated" "True" "$(jget "$PAYLOADS/logs-page-bytes.json" 'd["data"]["truncated"]')"
expect_eq "截断原因是 page_bytes" "page_bytes" "$(jget "$PAYLOADS/logs-page-bytes.json" 'd["data"]["truncated_reason"]')"
expect_eq "单页消息累计不超过 1 MiB" "True" \
  "$(jget "$PAYLOADS/logs-page-bytes.json" 'sum(len(e["message"].encode()) for e in d["data"]["entries"]) <= (1 << 20) + 4096')"
expect_eq "字节上限先于条数上限触发（不足 1000 条）" "True" \
  "$(jget "$PAYLOADS/logs-page-bytes.json" 'len(d["data"]["entries"]) < 1000')"
if [ "$(journalctl -u "$UNIT_BIG" --since "$T_START" -o cat --no-pager 2>/dev/null | wc -l | tr -d ' ')" -ge "$BIG_ENTRIES" ]; then
  pass "大页的 $BIG_ENTRIES 条日志确实写进了 journald"
else
  notrun "大页日志写入数量" "journald 里不足 $BIG_ENTRIES 条"
fi

expect_eq "超长单条消息查询状态码" "200" "$(query logs-huge.json "?source=journal&unit=$UNIT_HUGE&$WIN&limit=10")"
expect_eq "超长消息被标成 binary（journald 没给字符串）" "True" \
  "$(jget "$PAYLOADS/logs-huge.json" 'any(e["binary"] for e in d["data"]["entries"])')"
expect_eq "binary 条目的 message 是空串（不是一行空白日志）" "True" \
  "$(jget "$PAYLOADS/logs-huge.json" 'all(e["message"] == "" for e in d["data"]["entries"] if e["binary"])')"
notrun "单条消息 8 KiB 截断（truncated=true）" \
  "真实 journald 对超过 4086 字节的 MESSAGE 直接给 null，走的是 binary 分支；该路径只由 internal/journal 的合成输入单测覆盖"

echo
echo "== 7. 降序键集翻页 =="
PAGE=1
CODE=$(query logs-page-1.json "?source=journal&unit=$UNIT_MARK&$WIN&limit=5")
expect_eq "第 1 页状态码" "200" "$CODE"
CURSOR=$(jget "$PAYLOADS/logs-page-1.json" 'd["data"]["next_cursor"] or ""')
FIRST_CURSOR=$CURSOR
PAGES_OK=1
while [ -n "$CURSOR" ] && [ "$PAGE" -lt 20 ]; do
  PAGE=$((PAGE + 1))
  CODE=$(query "logs-page-$PAGE.json" "?source=journal&unit=$UNIT_MARK&$WIN&limit=5&cursor=$(urlenc "$CURSOR")")
  used=$CURSOR
  if [ "$CODE" != "200" ]; then
    fail "第 $PAGE 页状态码" "得到 $CODE（cursor=$used）"
    PAGES_OK=0
    break
  fi
  if [ "$(jget "$PAYLOADS/logs-page-$PAGE.json" 'any(e["cursor"] == "'"$used"'" for e in d["data"]["entries"])')" = "True" ]; then
    fail "第 $PAGE 页没有重复上一页的边界条目" "cursor=$used 又出现了一次"
    PAGES_OK=0
  fi
  CURSOR=$(jget "$PAYLOADS/logs-page-$PAGE.json" 'd["data"]["next_cursor"] or ""')
done
if [ "$PAGES_OK" -eq 1 ]; then pass "翻页过程中每一页都没有重复边界条目（共 $PAGE 页）"; fi
expect_eq "翻到窗口起点时 next_cursor=null" "" "$CURSOR"
PAGE_COUNT=$PAGE
printf '%s' "$PAGE_COUNT" >"$PAYLOADS/page-count.txt"
note "共翻出 $PAGE_COUNT 页（limit=5）"
if [ "$PAGE_COUNT" -ge 7 ]; then
  pass "翻页页数与数据量相当（$PAGE_COUNT 页 × 5 条）"
else
  fail "翻页页数与数据量相当" "只有 $PAGE_COUNT 页"
fi
expect_eq "30 个标记跨页出现且只出现一次" "True" \
  "$(python3 - "$PAYLOADS" "$PAGE_COUNT" "$MARKERS" <<'PY'
import json, os, sys
d, pages, want = sys.argv[1], int(sys.argv[2]), int(sys.argv[3])
seen = {}
cursors = []
for i in range(1, pages + 1):
    body = json.load(open(os.path.join(d, "logs-page-%d.json" % i), encoding="utf-8"))
    for e in body["data"]["entries"]:
        cursors.append(e["cursor"])
        msg = e["message"]
        if msg.startswith("marker-i="):
            seen[msg] = seen.get(msg, 0) + 1
missing = [k for k in ("marker-i=%d" % i for i in range(1, want + 1)) if k not in seen]
dup = [k for k, n in seen.items() if n != 1]
print(len(missing) == 0 and len(dup) == 0 and len(cursors) == len(set(cursors)))
if missing or dup:
    print("missing=%s dup=%s" % (missing[:3], dup[:3]))
PY
)"
expect_eq "跨页游标全局唯一" "True" \
  "$(python3 - "$PAYLOADS" "$PAGE_COUNT" <<'PY'
import json, os, sys
d, pages = sys.argv[1], int(sys.argv[2])
cursors = []
for i in range(1, pages + 1):
    body = json.load(open(os.path.join(d, "logs-page-%d.json" % i), encoding="utf-8"))
    cursors += [e["cursor"] for e in body["data"]["entries"]]
print(len(cursors) == len(set(cursors)))
PY
)"

# 未转义的游标必须是 400，而不是“当成没给游标”悄悄回第一页：
# 静默失败会让人以为翻页正常，其实一直在看同一页。
expect_eq "未转义的游标是 400（不是被当成没给）" "400"   "$(query err-cursor-raw.json "?source=journal&unit=$UNIT_MARK&$WIN&limit=5&cursor=$FIRST_CURSOR")"
expect_eq "被 journalctl 拒绝的游标是 400" "400" "$(query err-cursor-invalid.json '?cursor=abcdefghij')"
expect_eq "游标字符集外的内容是 400" "400" "$(query err-cursor-charset.json '?cursor=abc%20def%22%3Brm')"
expect_eq "错误码是 VALIDATION_ERROR" "VALIDATION_ERROR" "$(jget "$PAYLOADS/err-cursor-invalid.json" 'd["error"]["code"]')"

echo
echo "== 8. 秘密过滤 =="
expect_eq "含 password= 的条目被标成 redacted" "True" \
  "$(jget "$PAYLOADS/logs-basic.json" 'any(e["redacted"] for e in d["data"]["entries"] if "password=" in e["message"])')"
expect_eq "password 的值不出现在响应里" "0" \
  "$(jget "$PAYLOADS/logs-basic.json" 'd["data"]["entries"].__str__().count("hunter2secret")')"
expect_eq "含 token= 的条目被标成 redacted" "True" \
  "$(jget "$PAYLOADS/logs-basic.json" 'any(e["redacted"] for e in d["data"]["entries"] if "token=" in e["message"])')"
expect_eq "token 的值不出现在响应里" "0" \
  "$(jget "$PAYLOADS/logs-basic.json" 'd["data"]["entries"].__str__().count("abcdef123456")')"
expect_eq "普通条目没有被误标成 redacted" "True" \
  "$(jget "$PAYLOADS/logs-basic.json" 'any(not e["redacted"] for e in d["data"]["entries"] if "marker-i=" in e["message"])')"
expect_eq "脱敏后的原文仍能看出这是什么日志" "True" \
  "$(jget "$PAYLOADS/logs-basic.json" 'any("password=" in e["message"] and "登录失败" in e["message"] for e in d["data"]["entries"])')"

echo
echo "== 9. 内核日志（source=kernel） =="
expect_eq "source=kernel 状态码" "200" "$(query logs-kernel.json "?source=kernel&$WIN&limit=200")"
expect_eq "数据源回显 kernel" "kernel" "$(jget "$PAYLOADS/logs-kernel.json" 'd["data"]["source"]')"
expect_eq "内核条目没有 unit 字段" "True" \
  "$(jget "$PAYLOADS/logs-kernel.json" 'all(not e.get("unit") for e in d["data"]["entries"])')"
if [ -w /dev/kmsg ]; then
  expect_eq "注入的内核日志能在 source=kernel 里读到" "True" \
    "$(jget "$PAYLOADS/logs-kernel.json" 'any("upanel-accept-kmsg-marker-1" in e["message"] for e in d["data"]["entries"])')"
else
  notrun "注入的内核日志可读" "/dev/kmsg 不可写"
fi
expect_eq "source=kernel 不接受 unit 过滤" "400" "$(query err-unit-kernel.json "?source=kernel&unit=$UNIT_MARK")"
expect_eq "不在白名单的 source 是 400" "400" "$(query err-source.json '?source=syslog')"
expect_eq "unit 没有后缀是 400" "400" "$(query err-unit-shape.json '?unit=ssh')"
expect_eq "unit 带通配符是 400" "400" "$(query err-unit-glob.json '?unit=ssh*')"
expect_eq "unit 是路径是 400" "400" "$(query err-unit-path.json '?unit=/etc/passwd')"

echo
echo "== 10. 权限：未登录 401、viewer 403 =="
expect_eq "未登录 GET /logs 是 401" "401" "$(query_anon err-no-auth.json '?limit=1')"
expect_eq "未登录的错误码" "UNAUTHENTICATED" "$(jget "$PAYLOADS/err-no-auth.json" 'd["error"]["code"]')"
_code=$(curl -s -o "$PAYLOADS/err-no-auth-stream.json" -w '%{http_code}' "$BASE$STREAM_PATH?limit=1")
expect_eq "未登录 GET /logs/stream 是 401" "401" "$_code"
VCSRF=$(csrf_of "$WORK/login-viewer.json")
_code=$(api "$VJAR" "$VCSRF" GET "$LOGS_PATH?limit=1")
cp "$WORK/resp.json" "$PAYLOADS/logs-viewer.json" 2>/dev/null
expect_eq "viewer GET /logs 是 403" "403" "$_code"
expect_eq "viewer 的错误码是 FORBIDDEN" "FORBIDDEN" "$(jget "$PAYLOADS/logs-viewer.json" 'd["error"]["code"]')"
expect_eq "viewer 拿不到任何条目文本" "0" \
  "$(jget "$PAYLOADS/logs-viewer.json" 'len(json.dumps(d, ensure_ascii=False).split("marker-i=")) - 1')"
_code=$(curl -s -b "$VJAR" -o "$PAYLOADS/logs-viewer-stream.json" -w '%{http_code}' "$BASE$STREAM_PATH?limit=1")
expect_eq "viewer GET /logs/stream 是 403" "403" "$_code"
expect_eq "viewer 流错误码是 FORBIDDEN" "FORBIDDEN" "$(jget "$PAYLOADS/logs-viewer-stream.json" 'd["error"]["code"]')"

echo
echo "== 11. 实时流（SSE） =="
stream_open live "?source=journal&unit=$UNIT_STREAM&limit=50" >/dev/null
if stream_wait live ': connected' 15; then pass "流建立并收到 ': connected' 注释帧"; else fail "流建立" "$(cat "$WORK/live.err" 2>/dev/null | head -3)"; fi
CT=$(sed -n 's/^[Cc]ontent-[Tt]ype: *//p' "$WORK/live.headers" 2>/dev/null | tr -d '\r' | tail -1)
case "$CT" in
  text/event-stream*) pass "Content-Type 是 text/event-stream（$CT）" ;;
  *) fail "Content-Type 是 text/event-stream" "${CT:-缺失}" ;;
esac
CC=$(sed -n 's/^[Cc]ache-[Cc]ontrol: *//p' "$WORK/live.headers" 2>/dev/null | tr -d '\r' | tail -1)
expect_eq "Cache-Control: no-store" "no-store" "$CC"
XAB=$(sed -n 's/^X-Accel-Buffering: *//p' "$WORK/live.headers" 2>/dev/null | tr -d '\r' | tail -1)
expect_eq "X-Accel-Buffering: no（反代不缓冲）" "no" "$XAB"

note "流在跑时的 journalctl 子进程数：$(journalctl_children)"
if [ "$(journalctl_children)" -ge 1 ]; then
  pass "流真的挂了一个 journalctl 子进程（不是假装在跟）"
else
  fail "流真的挂了一个 journalctl 子进程" "一个都没有"
fi

emit_unit "$UNIT_STREAM" 'logger -t '"$IDENT"' "stream-marker-1 实时到达"'
if stream_wait live 'stream-marker-1' 20; then pass "实时流收到新写入的日志"; else fail "实时流收到新写入的日志" "20 秒内没等到"; fi
FIRST_ID=$(stream_last_id live | tr -d '
')
if [ -n "$FIRST_ID" ]; then pass "entry 帧带 id（journal 游标）：${FIRST_ID%%,*}"; else fail "entry 帧带 id" "帧里没有 id 行"; fi
expect_eq "entry 帧的 data 与 LogEntry 同形（不是信封）" "True" \
  "$(python3 - "$WORK/live.frames" <<'PY'
import json, sys
entries = []
for line in open(sys.argv[1], encoding="utf-8", errors="replace"):
    line = line.strip()
    if line.startswith("data: "):
        entries.append(json.loads(line[6:]))
hit = [e for e in entries if isinstance(e, dict) and "cursor" in e]
print(len(hit) > 0 and all({"cursor","timestamp","level","message"} <= set(e) for e in hit))
PY
)"

echo
echo "== 12. 断线续传（Last-Event-ID 之后开始） =="
if [ -n "$FIRST_ID" ]; then
  stream_kill live
  stream_open resume "?source=journal&unit=$UNIT_STREAM&limit=50" -H "Last-Event-ID: $FIRST_ID" >/dev/null
  if stream_wait resume ': connected' 15; then pass "带 Last-Event-ID 重开流成功"; else fail "带 Last-Event-ID 重开流" "没有 connected 帧"; fi
  emit_unit "$UNIT_STREAM" 'logger -t '"$IDENT"' "stream-marker-2 续传后的新日志"'
  if stream_wait resume 'stream-marker-2' 20; then pass "续传后收到新条目"; else fail "续传后收到新条目" "20 秒内没等到"; fi
  if grep -q 'stream-marker-1' "$WORK/resume.frames" 2>/dev/null; then
    fail "续传不重发已经收到过的条目" "stream-marker-1 又出现了一次"
  else
    pass "续传不重发已经收到过的条目"
  fi
  stream_kill resume
else
  fail "断线续传" "前一步没拿到游标"
fi

echo
echo "== 13. 结束订阅不残留 journalctl 子进程 =="
LEFT=$(wait_no_journalctl 15)
expect_eq "断开 15 秒内 journalctl 子进程归零" "0" "$LEFT"
if pgrep -af journalctl >"$WORK/journalctl-after.txt" 2>/dev/null; then
  fail "没有残留的 journalctl 命令行" "$(cat "$WORK/journalctl-after.txt")"
else
  pass "没有残留的 journalctl 命令行"
fi

echo
echo "== 14. 并发上限（同一账号 4 条） =="
STREAM_PIDS=""
for i in 1 2 3 4; do
  stream_open "conc$i" '?source=journal&limit=10' >/dev/null
  sleep 0.4
done
OPENED=0
for i in 1 2 3 4; do
  if stream_wait "conc$i" ': connected' 10; then OPENED=$((OPENED + 1)); fi
done
expect_eq "前 4 条流都能建立" "4" "$OPENED"
_code=$(curl -s -b "$JAR" -o "$PAYLOADS/err-stream-429.json" -w '%{http_code}' --max-time 5 "$BASE$STREAM_PATH?limit=10")
expect_eq "第 5 条流是 429" "429" "$_code"
expect_eq "429 的错误码是 VALIDATION_ERROR" "VALIDATION_ERROR" "$(jget "$PAYLOADS/err-stream-429.json" 'd["error"]["code"]')"
if [ "$(journalctl_children)" -ge 4 ]; then
  pass "4 条流对应至少 4 个 journalctl 子进程（$(journalctl_children) 个）"
else
  fail "4 条流对应至少 4 个 journalctl 子进程" "只有 $(journalctl_children) 个"
fi
kill_streams
LEFT=$(wait_no_journalctl 15)
expect_eq "关掉 4 条流后 journalctl 子进程归零" "0" "$LEFT"

echo
echo "== 15. 空闲超时收流 =="
if [ "$WAIT_IDLE" = "1" ]; then
  stream_open idle '?source=journal&unit=nonexistent-accept.service&limit=10' >/dev/null
  if stream_wait idle ': connected' 15; then pass "空闲流已建立（订阅一个永远不会有新日志的 unit）"; else fail "空闲流建立" "没有 connected 帧"; fi
  if stream_wait idle 'event: end' "$STREAM_IDLE_WAIT"; then
    pass "空闲超时后服务端主动收流（不是永远挂着）"
    expect_eq "end 帧的原因是 idle_timeout" "idle_timeout" \
      "$(sed -n 's/^data: //p' "$WORK/idle.frames" | tail -1 | python3 -c 'import json,sys
try:
    print(json.load(sys.stdin).get("reason",""))
except Exception:
    print("")' 2>/dev/null)"
  else
    fail "空闲超时后服务端主动收流" "${STREAM_IDLE_WAIT} 秒内没有 end 帧"
  fi
  stream_kill idle
  LEFT=$(wait_no_journalctl 15)
  expect_eq "空闲收流后 journalctl 子进程归零" "0" "$LEFT"
else
  notrun "空闲超时收流" "WAIT_IDLE=0，本轮不等 60 秒空闲窗口"
fi

echo
echo "== 16. 载荷校验（字段、闭集、null 语义、截断与权限） =="
if [ -f "$VALIDATE" ]; then
  python3 "$VALIDATE" "$PAYLOADS"
  VALIDATE_RC=$?
  if [ "$VALIDATE_RC" -eq 0 ]; then pass "载荷校验通过"; else fail "载荷校验通过" "退出码 $VALIDATE_RC"; fi
else
  notrun "载荷校验" "找不到 $VALIDATE"
fi

echo
echo "== 17. 汇总 =="
printf 'PASS=%s FAIL=%s NOT RUN=%s\n' "$PASSED" "$FAILED" "$NOTRUN"
[ -n "$FAILED_NAMES" ] && printf '失败项:%s\n' "$FAILED_NAMES"
printf 'NOT RUN 不等于通过；每一条都在脚本头部写明了原因。\n'
if [ "$FAILED" -eq 0 ]; then
  exit 0
fi
exit 1
