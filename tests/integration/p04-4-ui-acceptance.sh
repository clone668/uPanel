#!/bin/sh
# P04.4 Linux 验收脚本：前端读的那四份真实响应（/system/summary、/storage/topology、
# /storage/devices、/storage/devices/{id}）在真实 Ubuntu 上是否与契约和系统工具一致。
#
# 运行位置：Ubuntu 22.04/24.04 验收 VM 内，**必须以 root 运行**（代理只能在 root 下启动，
# 并且要用 upanel-web 身份去查 socket）。不依赖 Go 工具链：用宿主交叉编译好的 Linux 二进制。
#
# 用法：
#   sudo BIN_DIR=/tmp/upanel-bin sh tests/integration/p04-4-ui-acceptance.sh
#
# 退出码：0 全部通过；1 有失败项；2 环境不满足（非 root）。
#
# 覆盖的验收点（STAGES §6 P04.4、CONTRACTS §6/§6.2、PLAN §7/§8）：
#   1. Web 以非 root 身份运行，代理是 root，socket 归 socket_group
#   2. 四份响应的字段与契约一致（含 io 对象、字节字符串、health 闭集、reachable 三态）
#   3. 未知值一律 null，不出现 0 冒充（温度、容量、速率、可达性）
#   4. healthy 必须有 smartctl/NVMe 健康日志的依据（与 smartctl 逐盘对照）
#   5. 本阶段 capabilities 为空：界面上不会出现任何写动作
#   6. 与 lsblk / df -B1 / /proc/diskstats 交叉验证（磁盘集合、字节数、挂载容量、计数）
#   7. 设备 id 只接受合法字符：路径穿越式的 id 被拒绝
#
# 明确不覆盖（NOT RUN，原因写在这里，不在脚本里假装通过）：
#   - 浏览器渲染：VM 上没有浏览器，页面级证据来自宿主上的
#     tests/smoke/p04-4-ui-smoke.sh（真实 upanel-web + Playwright，mock 模式）。
#     这里只保证“前端拿到的数据是真的”，不保证“像素是对的”。
#   - 睡眠盘、USB 盘、真实 SATA：见 p04-3 脚本头部的说明，这两台 VM 做不到。

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
VALIDATE="$HERE/p04-4-payload-validate.py"

WORK=${WORK:-/var/tmp/upanel-accept-p04-4}
PAYLOADS=$WORK/payloads
RUN_DIR=/run/upanel
SOCK=$RUN_DIR/agent.sock
SYS_CFG=/etc/upanel/upanel.json
AGENT_DB_DIR=/var/lib/upanel-agent
AGENT_DB=$AGENT_DB_DIR/agent.db
KEY_FILE=/etc/upanel/agent.key
WEB_DB_DIR=$WORK/webdata
WEB_DB=$WEB_DB_DIR/upanel.db
PORT=${PORT:-19448}
BASE="http://127.0.0.1:$PORT"
ORIGIN="$BASE"
WEB_USER=upanel-web
PASSWORD='vm-correct-horse-9'

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
  FAILED_NAMES="$FAILED_NAMES $1"
  printf 'FAIL  %s%s\n' "$1" "${2:+（$2）}"
}
notrun() { NOTRUN=$((NOTRUN + 1)); printf 'NOT RUN  %s：%s\n' "$1" "$2"; }
note() { printf '      %s\n' "$1"; }
expect_eq() {
  if [ "$2" = "$3" ]; then pass "$1"; else fail "$1" "期望 $2，实际 $3"; fi
}

cleanup() {
  stop_web
  stop_agent
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

# fetch <jar> <路径> <输出文件>：以登录身份 GET，返回状态码。
fetch() {
  curl -s -b "$1" -o "$3" -w '%{http_code}' "$BASE$2"
}

echo "== 0. 环境 =="
if [ "$(id -u)" -eq 0 ]; then pass "验收脚本以 root 运行"; else fail "验收脚本以 root 运行" "UID $(id -u)"; fi
for b in "$AGENT" "$WEB" "$CTL"; do
  if [ -x "$b" ]; then pass "二进制存在且可执行 $(basename "$b")"; else fail "二进制存在且可执行 $(basename "$b")" "$b"; fi
done
if [ -f "$VALIDATE" ]; then pass "载荷校验程序存在"; else fail "载荷校验程序存在" "$VALIDATE"; fi
if command -v python3 >/dev/null 2>&1; then pass "python3 可用"; else fail "python3 可用" "缺少 python3"; fi
if command -v lsblk >/dev/null 2>&1 && command -v df >/dev/null 2>&1; then
  pass "系统工具有 lsblk/df，可以做交叉验证"
else
  fail "系统工具有 lsblk/df" "缺少 util-linux 或 coreutils"
fi
if command -v smartctl >/dev/null 2>&1; then
  pass "本机有 smartctl（$(command -v smartctl)）"
else
  notrun "与 smartctl 交叉验证" "本机没有 smartctl，物理盘健康只能是 unknown"
fi

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
    "storage": { "smart_enabled": true, "smart_interval_minutes": 15, "network_capacity_timeout_seconds": 2 }
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
# $! 是 runuser 的 pid（它自己还是 root），所以要看真正监听端口的那个进程。
LISTEN_PID=$(ss -lptnH "sport = :$PORT" 2>/dev/null | sed -n 's/.*pid=\([0-9]*\).*/\1/p' | head -1)
if [ -z "$LISTEN_PID" ]; then
  LISTEN_PID=$WEB_PID
fi
WEB_UID=$(ps -o uid= -p "$LISTEN_PID" 2>/dev/null | tr -d ' ')
if [ -n "$WEB_UID" ] && [ "$WEB_UID" != "0" ]; then
  pass "监听 $PORT 的 Web 进程不是 root（pid=$LISTEN_PID uid=$WEB_UID）"
else
  fail "监听 $PORT 的 Web 进程不是 root" "pid=${LISTEN_PID:-未知} uid=${WEB_UID:-未知}"
fi
expect_eq "Web 身份是 $WEB_USER" "$WEB_USER" "$(ps -o user= -p "$LISTEN_PID" 2>/dev/null | tr -d ' ')"
expect_eq "agent.sock 归 root:$WEB_USER" "root:$WEB_USER" \
  "$(stat -c '%U:%G' "$SOCK" 2>/dev/null)"
expect_eq "agent.sock 不是 0666" "no" \
  "$(stat -c '%a' "$SOCK" 2>/dev/null | grep -qx 666 && echo yes || echo no)"

JAR=$WORK/admin.jar
expect_eq "管理员登录" "200" "$(login admin "$PASSWORD" "$JAR" login-admin)"

echo
echo "== 1b. 等第一次 SMART 采集落地 =="
# 为什么要等：健康的第一次探测紧跟着拓扑启动，但它是**独立 goroutine**，比拓扑慢；
# 刚启动就抓响应会拿到 "SMART 采集尚未完成" 的占位（unknown），于是“健康字段对不对”
# 这件事就变成了空转。等所有整盘都有 checked_at 再抓，才算真的验到了。
# 上界由 HEALTH_WAIT 控制（默认 120 秒）：首轮探测一般 20 秒内完成，超时就如实记 NOT RUN。
HEALTH_WAIT=${HEALTH_WAIT:-120}
waited=0
health_pending() {
  fetch "$JAR" /api/v1/storage/devices "$WORK/health-probe.json" >/dev/null 2>&1 || return 2
  python3 - "$WORK/health-probe.json" <<'PY'
import json, sys
body = json.load(open(sys.argv[1]))
data = body.get("data", body)
missing = [
    d["id"] for d in data.get("devices", [])
    if d.get("kind") == "disk" and not (d.get("health") or {}).get("checked_at")
]
print(" ".join(missing))
sys.exit(1 if missing else 0)
PY
}
HEALTH_DONE=no
while [ "$waited" -lt "$HEALTH_WAIT" ]; do
  if health_pending >"$WORK/health-missing.txt" 2>&1; then
    HEALTH_DONE=yes
    break
  fi
  sleep 5
  waited=$((waited + 5))
done
if [ "$HEALTH_DONE" = yes ]; then
  pass "首次 SMART 采集已完成（等待 ${waited}s，全部整盘都有 checked_at）"
else
  notrun "首次 SMART 采集已完成" \
    "等了 ${HEALTH_WAIT}s 仍有整盘没有 checked_at：$(tr -d '\n' <"$WORK/health-missing.txt" 2>/dev/null)；health 只能是 unknown，逐盘对照记 NOT RUN"
fi

echo
echo "== 2. 抓四份真实响应 =="
CODE=$(fetch "$JAR" /api/v1/system/summary "$PAYLOADS/summary.json")
expect_eq "GET /system/summary" "200" "$CODE"
CODE=$(fetch "$JAR" /api/v1/storage/topology "$PAYLOADS/topology.json")
expect_eq "GET /storage/topology" "200" "$CODE"
CODE=$(fetch "$JAR" /api/v1/storage/devices "$PAYLOADS/devices.json")
expect_eq "GET /storage/devices" "200" "$CODE"

# 详情：每块整盘抓一份（最多 4 块，够覆盖多盘关系，也不至于把脚本拖长）。
DISK_IDS=$(python3 -c '
import json,sys
body=json.load(open(sys.argv[1]))
data=body.get("data", body)
disks=[d["id"] for d in data.get("devices", []) if d.get("kind")=="disk"][:8]
print(" ".join(disks))' "$PAYLOADS/devices.json" 2>/dev/null)
if [ -z "$DISK_IDS" ]; then
  fail "设备列表里至少有一块整盘" "没有 kind=disk 的设备"
else
  note "整盘: $DISK_IDS"
  n=0
  for id in $DISK_IDS; do
    n=$((n + 1))
    [ "$n" -gt 4 ] && break
    CODE=$(fetch "$JAR" "/api/v1/storage/devices/$id" "$PAYLOADS/detail-$id.json")
    expect_eq "GET /storage/devices/$id" "200" "$CODE"
  done
fi

# 设备 id 只接受 [A-Za-z0-9._-]：路径穿越式的 id 必须被拒绝，不能落到任何文件/设备上。
CODE=$(fetch "$JAR" '/api/v1/storage/devices/..%2F..%2Fetc%2Fshadow' "$PAYLOADS/traversal.json")
if [ "$CODE" = "400" ] || [ "$CODE" = "404" ]; then
  pass "路径穿越式设备 id 被拒绝（HTTP $CODE）"
else
  fail "路径穿越式设备 id 被拒绝" "HTTP $CODE：$(head -c 200 "$PAYLOADS/traversal.json")"
fi
if grep -q 'root:' "$PAYLOADS/traversal.json" 2>/dev/null; then
  fail "路径穿越响应里不含系统文件内容"
else
  pass "路径穿越响应里不含系统文件内容"
fi

echo
echo "== 3. 载荷与契约、与系统工具一致 =="
if python3 "$VALIDATE" "$PAYLOADS" >"$WORK/validate.out" 2>&1; then
  VALIDATE_CODE=0
else
  VALIDATE_CODE=$?
fi
cat "$WORK/validate.out"
if [ "$VALIDATE_CODE" -eq 0 ]; then
  pass "载荷校验全部通过"
else
  fail "载荷校验全部通过" "见上面的 FAIL 行（$WORK/validate.out）"
fi

echo
echo "== 4. 日志卫生 =="
if grep -q "$PASSWORD" "$WEB_LOG" "$AGENT_LOG" 2>/dev/null; then
  fail "日志不含管理员密码"
else
  pass "日志不含管理员密码"
fi

echo
printf '结果: PASS=%d FAIL=%d NOT RUN=%d\n' "$PASSED" "$FAILED" "$NOTRUN"
printf '载荷目录（可拷回宿主做前端渲染回归）: %s\n' "$PAYLOADS"
[ -n "$FAILED_NAMES" ] && printf '失败项:%s\n' "$FAILED_NAMES"
[ "$FAILED" -eq 0 ]
