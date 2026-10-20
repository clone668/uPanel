#!/bin/sh
# P04.1 Linux 验收脚本：系统基础资源采集（CPU、内存、负载、网络、主机信息），
# 5 秒轮询缓存与采集时间戳。
#
# 运行位置：Ubuntu 22.04/24.04 验收 VM 内，**必须以 root 运行**（代理只能在 root 下启动，
# 并且要用 upanel-web 身份去查 socket）。不依赖 Go 工具链：用宿主交叉编译好的 Linux 二进制。
#
# 用法：
#   sudo BIN_DIR=/tmp/upanel-bin sh tests/integration/p04-1-system-acceptance.sh
#
# 退出码：0 全部通过；1 有失败项；2 环境不满足（非 root）。
#
# 覆盖的验收点（STAGES P04.1、PLAN §8、CONTRACTS §3/§15）：
#   1. 代理启动后 capabilities 如实报告 system.summary，写动作仍然为空
#   2. system.summary 返回的真实数值与同一台 VM 上 /proc 的结果一致
#      （内存总量按字节精确比对、核数、负载、内存可用、boot_id、发行版 ID、开机时长）
#   3. 采集时间戳：collected_at 是最近 5 秒内的 UTC 时间，interval_ms=5000，stale=false
#   4. 轮询缓存：等待一个采集间隔后 collected_at 前进，且前进量在一个间隔附近
#   5. 使用率与速率来自相邻两次样本：第一份快照可以是 null，间隔之后必须给出
#      0—100 的使用率、非负速率，sample_window_ms 落在合理区间
#   6. 网卡计数器与 /proc/net/dev 一致：先读一次，睡满一个采集间隔（保证下一份快照在读取之后
#      才采），再调用，最后读一次，接口值必须落在两次读取之间，且只增不减
#   7. 参数边界：system.summary 不接受任何参数（含伪装成命令的字段）；未登记查询被拒
#   8. Web：未登录 401；viewer 也能读（V）；数值来自代理，不是 Web 自己编的
#   9. 采集不可用不阻塞登录：停掉代理后登录仍成功，/system/summary 返回 503 AGENT_UNAVAILABLE；
#      代理恢复后接口自动恢复
#  10. 日志卫生：agent/web 日志里不出现 shell 命令行与秘密

set -u

BIN_DIR=${BIN_DIR:-/tmp/upanel-bin}
if [ "$(id -u)" -ne 0 ]; then
  printf '本脚本必须以 root 运行：sudo BIN_DIR=%s sh %s\n' "$BIN_DIR" "$0" >&2
  exit 2
fi

AGENT="$BIN_DIR/upanel-agent"
WEB="$BIN_DIR/upanel-web"
CTL="$BIN_DIR/upanelctl"

WORK=${WORK:-/var/tmp/upanel-accept-p04-1}
RUN_DIR=/run/upanel
SOCK=$RUN_DIR/agent.sock
SYS_CFG=/etc/upanel/upanel.json
AGENT_DB_DIR=/var/lib/upanel-agent
AGENT_DB=$AGENT_DB_DIR/agent.db
KEY_FILE=/etc/upanel/agent.key
WEB_DB_DIR=$WORK/webdata
WEB_DB=$WEB_DB_DIR/upanel.db
PORT=19445
BASE="http://127.0.0.1:$PORT"
ORIGIN="$BASE"
WEB_USER=upanel-web
PASSWORD='vm-correct-horse-9'
VIEWER_PASSWORD='vm-viewer-horse-42'

AGENT_LOG=$WORK/logs/agent.log
WEB_LOG=$WORK/logs/web.log

PASSED=0
FAILED=0
FAILED_NAMES=""
AGENT_PID=""
WEB_PID=""
CODE=""
RESP=""

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

# jget <文件> <python 表达式，d 为解析后的 JSON>；null 打印 None。
jget() {
  python3 -c 'import json,sys
d=json.load(open(sys.argv[1], encoding="utf-8"))
print(eval(sys.argv[2]))' "$1" "$2" 2>/dev/null
}

# jgetx <文件> <附加参数…> <表达式>：表达式里可以用 a[0]、a[1]… 取附加参数。
# 用于把脚本里量出来的 /proc 数值带进断言，避免在单引号字符串里做 shell 展开（不会展开）。
jgetx() {
  f=$1; shift
  python3 -c 'import json,sys
f=sys.argv[1]; a=sys.argv[2:-1]; e=sys.argv[-1]
d=json.load(open(f, encoding="utf-8"))
print(eval(e))' "$f" "$@" 2>/dev/null
}

# 时间戳比较必须自己解析成 datetime，不能靠字符串比大小；而 Go 的 RFC3339Nano 会给出
# 9 位小数秒，Ubuntu 22.04 的 Python 3.10 只认 3/6 位，fromisoformat 会直接抛错，
# 所以统一先截到 6 位（毫秒以内对这里的判断没有意义）。
ts_norm='import re,datetime,sys
def n(x):
    return datetime.datetime.fromisoformat(re.sub(r"\.(\d{6})\d+", r".\1", x).replace("Z","+00:00"))'

# ts_age_lt <iso> <秒>：时间戳距今不到 N 秒 → True
ts_age_lt() {
  python3 -c "$ts_norm
now=datetime.datetime.now(datetime.timezone.utc)
print((now-n(sys.argv[1])).total_seconds() < float(sys.argv[2]))" "$1" "$2" 2>/dev/null
}

# ts_gap_in <a> <b> <下界> <上界>：b-a 落在区间内 → True
ts_gap_in() {
  python3 -c "$ts_norm
d=(n(sys.argv[2])-n(sys.argv[1])).total_seconds()
print(float(sys.argv[3]) <= d <= float(sys.argv[4]))" "$1" "$2" "$3" "$4" 2>/dev/null
}

# call <as> <method> <path> [json]：以指定身份访问代理 socket。CODE=状态码，RESP=响应文件。
call() {
  as=$1; method=$2; path=$3; body=${4:-}
  out="$WORK/out-$as/resp.json"
  url="http://agent$path"
  if [ -n "$body" ]; then
    printf '%s' "$body" >"$WORK/req.json"
    chmod 0644 "$WORK/req.json"
    args="-sS -o $out -w %{http_code} -X $method -H Content-Type:application/json --data-binary @$WORK/req.json --unix-socket $SOCK $url"
  else
    args="-sS -o $out -w %{http_code} -X $method --unix-socket $SOCK $url"
  fi
  RESP="$out"
  case $as in
    web) CODE=$(runuser -u "$WEB_USER" -- curl $args 2>/dev/null) ;;
    *)   fail "call-args" "未知调用身份 $as"; CODE="" ;;
  esac
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

login() {
  curl -s -c "$3" -o "$WORK/$4.json" -w '%{http_code}' -X POST "$BASE/api/v1/auth/login" \
    -H 'Content-Type: application/json' -H 'X-UPanel-Request: 1' -H "Origin: $ORIGIN" \
    -d "{\"username\":\"$1\",\"password\":\"$2\"}"
}

csrf_of() { jget "$1" 'd["data"]["csrf_token"]'; }

# api <jar> <csrf> <方法> <路径> [body]
api() {
  jar=$1; csrf=$2; method=$3; path=$4; body=${5:-}
  if [ -n "$body" ]; then
    printf '%s' "$body" >"$WORK/req.json"
    curl -s -b "$jar" -o "$WORK/resp.json" -w '%{http_code}' -X "$method" "$BASE$path" \
      -H 'Content-Type: application/json' -H "Origin: $ORIGIN" -H "X-CSRF-Token: $csrf" \
      -d @"$WORK/req.json"
  else
    curl -s -b "$jar" -o "$WORK/resp.json" -w '%{http_code}' -X "$method" "$BASE$path" \
      -H "Origin: $ORIGIN"
  fi
}

cleanup() {
  stop_web
  stop_agent
}
trap cleanup EXIT
trap 'cleanup; exit 130' INT
trap 'cleanup; exit 143' TERM

echo "== 0. 环境"
if [ "$(id -u)" -eq 0 ]; then pass "验收脚本以 root 运行"; else fail "验收脚本以 root 运行" "当前 UID $(id -u)"; fi
for b in "$AGENT" "$WEB" "$CTL"; do
  if [ -x "$b" ]; then pass "二进制存在且可执行 $(basename "$b")"; else fail "二进制存在且可执行 $(basename "$b")" "$b"; fi
done
if command -v python3 >/dev/null 2>&1; then pass "python3 可用于解析响应"; else fail "python3 可用于解析响应" "缺少 python3"; fi

if ! id -u "$WEB_USER" >/dev/null 2>&1; then
  note "创建系统账号 $WEB_USER（验收环境未预置）"
  useradd --system --no-create-home --shell /usr/sbin/nologin "$WEB_USER" 2>/dev/null
fi
if id -u "$WEB_USER" >/dev/null 2>&1; then pass "Web 账号 $WEB_USER 存在"; else fail "Web 账号 $WEB_USER 存在" "无法创建"; fi

if curl -fsS "$BASE/api/v1/health/live" >/dev/null 2>&1; then
  echo "$PORT 上已有服务在监听；先停掉遗留的 upanel-web 再跑验收" >&2
  exit 1
fi

rm -rf "$WORK"
mkdir -p "$WORK/logs" "$WORK/out-web" "$WEB_DB_DIR"
chmod 0755 "$WORK" "$WORK/out-web" "$WORK/logs"
chown "$WEB_USER" "$WORK/out-web"
# Web 库目录必须属于 Web 账号：SQLite 要在同目录建 -wal/-shm，目录不可写就是
# “attempt to write a readonly database”，Web 会拒绝启动。
chown -R "$WEB_USER" "$WEB_DB_DIR"

echo
echo "== 1. 启动代理（生产登记表，不带验收动作） =="
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
    "shutdown_timeout_seconds": 10
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
  rm -f "$KEY_FILE"
  if "$AGENT" --config "$SYS_CFG" -init-key >"$WORK/init-key.log" 2>&1; then
    pass "-init-key 生成代理密钥"
  else
    fail "-init-key 生成代理密钥" "$(tail -3 "$WORK/init-key.log" 2>/dev/null)"
  fi
fi

if start_agent; then pass "代理以 root 启动并创建 socket"; else fail "代理以 root 启动" "$(tail -5 "$AGENT_LOG" 2>/dev/null)"; fi
expect_eq "代理进程 uid=0" "0" "$(ps -o uid= -p "$AGENT_PID" 2>/dev/null | tr -d ' ')"

call web GET /internal/v1/capabilities
expect_eq "capabilities 200" "200" "$CODE"
expect_eq "capabilities 报告 system.summary" "['system.summary']" "$(jget "$RESP" 'd["data"]["supported_queries"]')"
expect_eq "capabilities 仍不报告任何写动作" "[]" "$(jget "$RESP" 'd["data"]["supported_actions"]')"

echo
echo "== 2. 采集结果与本机 /proc 一致 =="
MEM_KB=$(awk '/^MemTotal:/ {print $2}' /proc/meminfo)
MEM_BYTES=$((${MEM_KB:-0} * 1024))
AVAIL_KB=$(awk '/^MemAvailable:/ {print $2}' /proc/meminfo)
NPROC=$(nproc)
BOOT_ID=$(cat /proc/sys/kernel/random/boot_id)
OS_ID=$(sh -c '. /etc/os-release 2>/dev/null; printf %s "$ID"')
KERNEL=$(uname -r)
UPTIME_BEFORE=$(cut -d' ' -f1 /proc/uptime)

call web POST /internal/v1/queries '{"query":"system.summary"}'
UPTIME_AFTER=$(cut -d' ' -f1 /proc/uptime)
AVAIL_KB_AFTER=$(awk '/^MemAvailable:/ {print $2}' /proc/meminfo)
expect_eq "system.summary 200" "200" "$CODE"
if [ "$CODE" != "200" ]; then
  fail "采集失败，后续断言无法进行" "$(head -c 400 "$RESP" 2>/dev/null)"
  printf '通过 %d 项，失败 %d 项\n' "$PASSED" "$FAILED"
  exit 1
fi
cp "$RESP" "$WORK/summary-1.json"

# 总量是常量，按字节精确比对；可用量在两次读取之间本来就会动（页缓存），
# 用 64 MiB 容差比对，既证明数值来自 /proc/meminfo 又不做假精确。
expect_eq "内存总量按字节与 /proc/meminfo 一致" "$MEM_BYTES" "$(jget "$WORK/summary-1.json" 'd["data"]["memory"]["total_bytes"]')"
expect_eq "内存可用量与 /proc/meminfo 同量级（±64 MiB）" "True" \
  "$(jgetx "$WORK/summary-1.json" "$AVAIL_KB" "$AVAIL_KB_AFTER" \
    'any(abs(int(d["data"]["memory"]["available_bytes"]) // 1024 - int(x)) <= 65536 for x in a)')"
expect_eq "内存段 status=ok" "ok" "$(jget "$WORK/summary-1.json" 'd["data"]["memory"]["status"]')"
expect_eq "已用内存 = Total - Available" "True" \
  "$(jget "$WORK/summary-1.json" 'int(d["data"]["memory"]["total_bytes"]) - int(d["data"]["memory"]["available_bytes"]) == int(d["data"]["memory"]["used_bytes"])')"
expect_eq "逻辑核数量与 nproc 一致" "$NPROC" "$(jget "$WORK/summary-1.json" 'd["data"]["cpu"]["logical_cores"]')"
expect_eq "CPU 段 status=ok" "ok" "$(jget "$WORK/summary-1.json" 'd["data"]["cpu"]["status"]')"
expect_eq "boot_id 与内核一致" "$BOOT_ID" "$(jget "$WORK/summary-1.json" 'd["data"]["host"]["boot_id"]')"
expect_eq "发行版 ID 与 /etc/os-release 一致" "$OS_ID" "$(jget "$WORK/summary-1.json" 'd["data"]["host"]["os_id"]')"
expect_eq "内核版本与 uname -r 一致" "$KERNEL" "$(jget "$WORK/summary-1.json" 'd["data"]["host"]["kernel_release"]')"
expect_eq "主机名非空" "True" "$(jget "$WORK/summary-1.json" 'bool(d["data"]["host"]["hostname"])')"
# 快照最老 5 秒：下界放宽到 5 秒前，上界只到读取时刻之后 2 秒。
expect_eq "开机时长落在 /proc/uptime 的合理区间内" "True" \
  "$(jgetx "$WORK/summary-1.json" "$UPTIME_BEFORE" "$UPTIME_AFTER" \
    'float(a[0]) - 5 <= d["data"]["host"]["uptime_seconds"] <= float(a[1]) + 2')"
expect_eq "负载 1 分钟值与 /proc/loadavg 可比" "True" \
  "$(jget "$WORK/summary-1.json" 'abs(d["data"]["load"]["load1"] - float(open("/proc/loadavg").read().split()[0])) < 1.0')"
expect_eq "负载段 status=ok" "ok" "$(jget "$WORK/summary-1.json" 'd["data"]["load"]["status"]')"

expect_eq "网卡列表包含 loopback" "True" \
  "$(jget "$WORK/summary-1.json" 'any(i["loopback"] for i in d["data"]["network"]["interfaces"])')"
expect_eq "网卡段 status=ok" "ok" "$(jget "$WORK/summary-1.json" 'd["data"]["network"]["status"]')"
expect_eq "每个网卡的速率都要么是 null 要么非负" "True" \
  "$(jget "$WORK/summary-1.json" 'all(i["rx_bytes_per_second"] is None or i["rx_bytes_per_second"] >= 0 for i in d["data"]["network"]["interfaces"])')"

echo
echo "== 3. 采集时间戳与轮询缓存 =="
T1=$(jget "$WORK/summary-1.json" 'd["data"]["collected_at"]')
expect_eq "interval_ms=5000（PLAN §8：基础资源每 5 秒）" "5000" "$(jget "$WORK/summary-1.json" 'd["data"]["interval_ms"]')"
expect_eq "健康机器上 degraded=false" "False" "$(jget "$WORK/summary-1.json" 'd["data"]["degraded"]')"
expect_eq "collected_at 不早于 10 秒前（是最近一份快照）" "True" "$(ts_age_lt "$T1" 10)"
expect_eq "stale=false（采集循环在跑）" "False" "$(jget "$WORK/summary-1.json" 'd["data"]["stale"]')"

note "等 6 秒，跨过一个采集间隔再查"
# 计数器在采集窗口内会自然增长。先读一次 /proc/net/dev，睡满一个间隔（保证下一份快照
# 是这次读取之后采的），再调用，最后读一次：接口值必须落在 [前, 后] 之间。
RX_IFACE=$(awk -F: '$1 ~ /^ *ens4$/ {print $1}' /proc/net/dev | tr -d ' ')
RX_BEFORE=$(awk -F: '/^ *ens4:/ {print $2}' /proc/net/dev | awk '{print $1}')
sleep 6
call web POST /internal/v1/queries '{"query":"system.summary"}'
RX_AFTER=$(awk -F: '/^ *ens4:/ {print $2}' /proc/net/dev | awk '{print $1}')
expect_eq "第二次查询 200" "200" "$CODE"
cp "$RESP" "$WORK/summary-2.json"
if [ -n "${RX_BEFORE:-}" ] && [ -n "${RX_AFTER:-}" ]; then
  pass "VM 上存在 ens4 网卡（可以比对计数器）"
  expect_eq "ens4 rx_bytes 落在采集前后的 /proc/net/dev 区间内" "True" \
    "$(jgetx "$WORK/summary-2.json" "$RX_BEFORE" "$RX_AFTER" \
      'int(a[0]) <= int([i for i in d["data"]["network"]["interfaces"] if i["name"]=="ens4"][0]["rx_bytes"]) <= int(a[1])')"
  # 第一份快照作为额外参数传进来（d 是第二份）：直接写两个文件路径时，
  # jgetx 的第一个参数会被当成 d，方向会反过来，断言就变成“只减不增”了。
  expect_eq "ens4 计数器在快照之间只增不减" "True" \
    "$(jgetx "$WORK/summary-2.json" "$WORK/summary-1.json" \
      'int([i for i in json.load(open(a[0]))["data"]["network"]["interfaces"] if i["name"]=="ens4"][0]["rx_bytes"]) <= int([i for i in d["data"]["network"]["interfaces"] if i["name"]=="ens4"][0]["rx_bytes"])')"
else
  note "$RX_IFACE 不在 /proc/net/dev 里（验收 VM 网卡名可能不同），跳过计数器区间断言"
fi
T2=$(jget "$WORK/summary-2.json" 'd["data"]["collected_at"]')
expect_eq "collected_at 在一个间隔之后前进了" "True" "$(ts_gap_in "$T1" "$T2" 4 9)"

echo
echo "== 4. 使用率与速率来自相邻两次样本 =="
expect_eq "usage_percent 非 null 且在 0—100 之间" "True" \
  "$(jget "$WORK/summary-2.json" 'd["data"]["cpu"]["usage_percent"] is not None and 0 <= d["data"]["cpu"]["usage_percent"] <= 100')"
expect_eq "sample_window_ms 是一个采集间隔量级" "True" \
  "$(jget "$WORK/summary-2.json" '4000 <= (d["data"]["cpu"].get("sample_window_ms") or 0) <= 9000')"
expect_eq "按核使用率数量与逻辑核一致" "True" \
  "$(jget "$WORK/summary-2.json" 'len(d["data"]["cpu"]["per_cpu"]) == d["data"]["cpu"]["logical_cores"]')"
expect_eq "至少一块网卡给出了非 null 速率（已有前一次样本）" "True" \
  "$(jget "$WORK/summary-2.json" 'any(i["rx_bytes_per_second"] is not None for i in d["data"]["network"]["interfaces"])')"

echo
echo "== 5. 查询边界：没有万能入口 =="
call web POST /internal/v1/queries '{"query":"system.summary","params":{"command":"id -u"}}'
expect_eq "system.summary 不接受参数 400" "400" "$CODE"
call web POST /internal/v1/queries '{"query":"system.summary","params":{"blob":"x"}}'
expect_eq "未知参数字段 400" "400" "$CODE"
call web POST /internal/v1/queries '{"query":"system.nonexistent"}'
expect_eq "未登记查询 400" "400" "$CODE"
expect_contains "未登记查询说明原因" "未登记" "$(cat "$RESP" 2>/dev/null)"
call web GET /internal/v1/queries
expect_eq "查询接口只接受 POST" "405" "$CODE"

echo
echo "== 6. Web 端透传（权限 V、数值来自代理） =="
printf '%s\n%s\n' "$PASSWORD" "$PASSWORD" | "$CTL" init-admin --config "$SYS_CFG" --db "$WEB_DB" \
  --username admin --display-name "管理员" --json >"$WORK/init.out" 2>"$WORK/init.err"
INIT_RC=$?
expect_eq "init-admin 退出码" "0" "$INIT_RC"
chown "$WEB_USER" "$WEB_DB" 2>/dev/null
for suffix in "" "-wal" "-shm"; do
  [ -e "$WEB_DB$suffix" ] && chown "$WEB_USER" "$WEB_DB$suffix"
done
if start_web; then pass "Web 以 $WEB_USER 身份在 $BASE 启动"; else fail "Web 启动" "$(tail -5 "$WEB_LOG" 2>/dev/null)"; fi
WEB_CHILD_PID=$(pgrep -u "$WEB_USER" -f "upanel-web --mode production --db $WEB_DB" | head -n 1)
expect_eq "Web 进程以 $WEB_USER 运行" "$(id -u "$WEB_USER")" "$(ps -o uid= -p "${WEB_CHILD_PID:-0}" 2>/dev/null | tr -d ' ')"

ANON=$(curl -s -o "$WORK/anon.json" -w '%{http_code}' "$BASE/api/v1/system/summary")
expect_eq "未登录读系统信息 401" "401" "$ANON"
expect_eq "未登录错误码" "UNAUTHENTICATED" "$(jget "$WORK/anon.json" 'd["error"]["code"]')"

code=$(login admin "$PASSWORD" "$WORK/admin.jar" login-admin)
expect_eq "管理员登录 200" "200" "$code"
if [ "$code" != "200" ]; then
  fail "登录失败，后续断言无法进行" "$(cat "$WORK/login-admin.json" 2>/dev/null)"
  printf '通过 %d 项，失败 %d 项\n' "$PASSED" "$FAILED"
  exit 1
fi
ADMIN_CSRF=$(csrf_of "$WORK/login-admin.json")

code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" GET "/api/v1/system/summary")
expect_eq "GET /api/v1/system/summary 200" "200" "$code"
cp "$WORK/resp.json" "$WORK/web-summary.json"
expect_eq "Web 返回的内存总量与 /proc/meminfo 一致" "$MEM_BYTES" "$(jget "$WORK/web-summary.json" 'd["data"]["memory"]["total_bytes"]')"
expect_eq "Web 返回的 collected_at 是最近 10 秒内的快照" "True" \
  "$(ts_age_lt "$(jget "$WORK/web-summary.json" 'd["data"]["collected_at"]')" 10)"
expect_eq "Web 与代理返回同一个 interval_ms" "5000" "$(jget "$WORK/web-summary.json" 'd["data"]["interval_ms"]')"
expect_eq "Web 没有自己编网卡列表" "True" \
  "$(jget "$WORK/web-summary.json" 'len(d["data"]["network"]["interfaces"]) >= 1')"

# viewer 也能读（CONTRACTS §3：权限 V）。
code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "/api/v1/users" \
  "{\"username\":\"alice\",\"display_name\":\"alice\",\"role\":\"viewer\",\"initial_password\":\"$VIEWER_PASSWORD\"}")
expect_eq "创建 viewer 201" "201" "$code"
code=$(login alice "$VIEWER_PASSWORD" "$WORK/viewer.jar" login-viewer)
expect_eq "viewer 登录 200" "200" "$code"
code=$(api "$WORK/viewer.jar" "$(csrf_of "$WORK/login-viewer.json")" GET "/api/v1/system/summary")
expect_eq "viewer 读系统信息 200" "200" "$code"

echo
echo "== 7. 采集不可用不阻塞登录 =="
stop_agent
expect_eq "代理已停止（socket 不存在）" "False" "$([ -S "$SOCK" ] && echo True || echo False)"
code=$(login admin "$PASSWORD" "$WORK/admin2.jar" login-admin2)
expect_eq "代理停机时登录仍然 200（采集失败不卡住登录）" "200" "$code"
code=$(api "$WORK/admin2.jar" "$(csrf_of "$WORK/login-admin2.json")" GET "/api/v1/system/summary")
expect_eq "代理停机时 /system/summary 503" "503" "$code"
expect_eq "代理停机时错误码" "AGENT_UNAVAILABLE" "$(jget "$WORK/resp.json" 'd["error"]["code"]')"
expect_eq "代理停机时不返回任何数值" "False" "$(jget "$WORK/resp.json" 'bool(d.get("data"))')"

if start_agent; then pass "代理恢复启动"; else fail "代理恢复启动" "$(tail -5 "$AGENT_LOG" 2>/dev/null)"; fi
code=$(api "$WORK/admin2.jar" "$(csrf_of "$WORK/login-admin2.json")" GET "/api/v1/system/summary")
expect_eq "代理恢复后 /system/summary 200" "200" "$code"
expect_eq "恢复后数值仍然是 /proc 的真实值" "$MEM_BYTES" "$(jget "$WORK/resp.json" 'd["data"]["memory"]["total_bytes"]')"

echo
echo "== 8. 日志卫生 =="
if [ -f "$AGENT_LOG" ]; then
  if grep -q 'system.summary' "$AGENT_LOG" 2>/dev/null; then
    note "代理日志里出现了查询名（允许，是能力名不是命令）"
  fi
  expect_eq "代理日志里没有 shell 命令行痕迹" "0" "$(grep -c 'sh -c' "$AGENT_LOG" 2>/dev/null | head -n 1)"
  expect_eq "代理日志里没有私钥泄漏" "0" "$(grep -c 'BEGIN PRIVATE KEY' "$AGENT_LOG" 2>/dev/null | head -n 1)"
else
  fail "代理日志存在" "$AGENT_LOG"
fi
if [ -f "$WEB_LOG" ]; then
  expect_eq "Web 日志里没有 panic" "0" "$(grep -c 'panic' "$WEB_LOG" 2>/dev/null | head -n 1)"
else
  fail "Web 日志存在" "$WEB_LOG"
fi

echo
printf '通过 %d 项，失败 %d 项\n' "$PASSED" "$FAILED"
if [ "$FAILED" -ne 0 ]; then
  printf '失败项:%s\n' "$FAILED_NAMES"
  exit 1
fi
exit 0
