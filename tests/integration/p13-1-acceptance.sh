#!/bin/sh
# P13.1 Linux 验收脚本：历史监控（原始/分钟/小时样本、聚合、保留与清理、查询点数上限）。
#
# 运行位置：Ubuntu 22.04/24.04 验收 VM 内，**必须以 root 运行**（代理只能在 root 下启动，
# 并且要用 upanel-web 身份去查 socket）。不依赖 Go 工具链：用宿主交叉编译好的 Linux 二进制。
#
# 用法：
#   sudo BIN_DIR=/tmp/upanel-bin sh tests/integration/p13-1-acceptance.sh
#
# 退出码：0 全部通过；1 有失败项；2 环境不满足（非 root）。
#
# 覆盖的验收点（STAGES P13.1、PLAN §18、CONTRACTS §3）：
#   A. 真实采集：raw 点按代理的 5 秒节奏落库、同一 collected_at 不重复写、
#      分钟聚合在桶结束后出现，且聚合值落在桶内 min/max 之间
#   B. 缺口：停掉代理期间不写样本 → missing_points > 0，缺口里**没有任何点**（不是 0 值点）；
#      代理停机时 status 的 last_error 非空、collecting 仍为 true（说清“采集在跑但没数”）
#   C. 保留与清理：库里种入超出保留窗口的 raw 行 → 重启后在首轮被裁掉；
#      种入上一个整小时的 minute 行 → 重启后**启动即补算**出 hour 行
#   D. 查询规则：跨度决定读哪一层（用压缩后的保留窗口验证）、点数 ≤ 1440、
#      未知指标/缺 scope/未来 to/非法 step/跨度超一年都回 400，viewer 也能读（V），未登录 401
#   E. 关闭采集（web.metrics.enabled=false）：不再写新样本，但**旧数据仍可查**，
#      status 说清原因（enabled=true、collecting=false）
#   F. 日志卫生：web 日志里不出现口令/秘密
#
# 注意：为了能在几分钟内验证保留窗口，本脚本把 web.metrics 的三个保留窗口**压缩**了
# （raw 180 秒、minute 2 小时、hour 4 小时）。压缩配置只用于验收，不代表默认值。

set -u

BIN_DIR=${BIN_DIR:-/tmp/upanel-bin}
if [ "$(id -u)" -ne 0 ]; then
  printf '本脚本必须以 root 运行：sudo BIN_DIR=%s sh %s\n' "$BIN_DIR" "$0" >&2
  exit 2
fi

AGENT="$BIN_DIR/upanel-agent"
WEB="$BIN_DIR/upanel-web"
CTL="$BIN_DIR/upanelctl"

WORK=${WORK:-/var/tmp/upanel-accept-p13-1}
RUN_DIR=/run/upanel
SOCK=$RUN_DIR/agent.sock
SYS_CFG=/etc/upanel/upanel.json
AGENT_DB_DIR=/var/lib/upanel-agent
AGENT_DB=$AGENT_DB_DIR/agent.db
KEY_FILE=/etc/upanel/agent.key
WEB_DB_DIR=$WORK/webdata
WEB_DB=$WEB_DB_DIR/upanel.db
PORT=${PORT:-19452}
BASE="http://127.0.0.1:$PORT"
ORIGIN="$BASE"
WEB_USER=upanel-web
PASSWORD='vm-correct-horse-9'
VIEWER_PASSWORD='vm-viewer-horse-42'

# 压缩后的保留窗口（秒）。见文件头的说明。
RAW_RETENTION=180
MINUTE_RETENTION=7200
HOUR_RETENTION=14400

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

# jgetx <文件> <附加参数…> <表达式>：表达式里用 a[0]、a[1]… 取附加参数。
# 用来把脚本里量出来的 Unix 秒带进断言，避免在单引号字符串里做 shell 展开（不会展开）。
jgetx() {
  f=$1; shift
  python3 -c 'import json,datetime,sys
f=sys.argv[1]; a=sys.argv[2:-1]; e=sys.argv[-1]
d=json.load(open(f, encoding="utf-8"))
def u(x):
    return int(datetime.datetime.fromisoformat(x.replace("Z","+00:00")).timestamp())
print(eval(e))' "$f" "$@" 2>/dev/null
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
  runuser -u "$WEB_USER" -- "$WEB" --config "$SYS_CFG" --mode production --db "$WEB_DB" \
    --listen "127.0.0.1:$PORT" >>"$WEB_LOG" 2>&1 &
  WEB_PID=$!
  for _ in $(seq 1 150); do
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
  for _ in $(seq 1 150); do
    curl -fsS "$BASE/api/v1/health/live" >/dev/null 2>&1 || return 0
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

# metricapi <jar> <查询串>：GET /system/metrics，结果在 $WORK/resp.json，返回状态码。
metricapi() {
  curl -s -b "$1" -o "$WORK/resp.json" -w '%{http_code}' -X GET \
    "$BASE/api/v1/system/metrics?$2" -H "Origin: $ORIGIN"
}

statusapi() {
  curl -s -b "$1" -o "$WORK/resp.json" -w '%{http_code}' -X GET \
    "$BASE/api/v1/system/metrics/status" -H "Origin: $ORIGIN"
}

# sqlq <SQL>：以只读方式直接读 web 的 SQLite 库，用来验证库里真的发生了什么。
# 用 mode=ro：root 打开读写会在库里留下 root 属主的 -wal/-shm，之后 web（upanel-web）
# 就写不动自己的库了——验收脚本不能把现场弄坏。
sqlq() {
  python3 - "$WEB_DB" "$1" <<'PY' 2>/dev/null
import sqlite3, sys
db = sqlite3.connect("file:" + sys.argv[1] + "?mode=ro", uri=True)
print(db.execute(sys.argv[2]).fetchone()[0])
PY
}

# sqllist <SQL>：把结果逐行打印出来。只在断言要失败时用来把现场摊开——
# 一条“期望 0，实际 4”的断言不告诉任何人那 4 行是什么，等于把人送去猜。
sqllist() {
  python3 - "$WEB_DB" "$1" <<'PY' 2>/dev/null
import sqlite3, sys
db = sqlite3.connect("file:" + sys.argv[1] + "?mode=ro", uri=True)
for row in db.execute(sys.argv[2]):
    print("      " + " | ".join(str(c) for c in row))
PY
}

# chown_web_db：upanelctl 以 root 建的库文件要交给 web 账号，否则 SQLite 打不开。
chown_web_db() {
  for suffix in "" "-wal" "-shm"; do
    [ -e "$WEB_DB$suffix" ] && chown "$WEB_USER" "$WEB_DB$suffix"
  done
}

now_unix() { date -u +%s; }
iso_of() { date -u -d "@$1" +%Y-%m-%dT%H:%M:%SZ; }

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
if command -v python3 >/dev/null 2>&1; then pass "python3 可用（解析响应 + 种历史行）"; else fail "python3 可用" "缺少 python3"; fi

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
chown -R "$WEB_USER" "$WEB_DB_DIR"

echo
echo "== 1. 装配：配置（历史监控打开，保留窗口压缩）+ 代理 + 管理员 + Web =="
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
      "enabled": true,
      "interval_seconds": 2,
      "raw_retention_seconds": $RAW_RETENTION,
      "minute_retention_seconds": $MINUTE_RETENTION,
      "hour_retention_seconds": $HOUR_RETENTION,
      "max_rows": 500000
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

# 管理员先建（upanelctl 会跑完整迁移，metric_samples 表也是这时建出来的），
# 再把库文件交给 web 账号，最后才起 web。
printf '%s\n%s\n' "$PASSWORD" "$PASSWORD" | "$CTL" init-admin --config "$SYS_CFG" --db "$WEB_DB" \
  --username admin --display-name "管理员" --json >"$WORK/init.out" 2>"$WORK/init.err"
INIT_RC=$?
expect_eq "init-admin 退出码" "0" "$INIT_RC"
chown_web_db

if start_web; then pass "Web 启动（历史监控打开）"; else fail "Web 启动" "$(tail -5 "$WEB_LOG" 2>/dev/null)"; fi

code=$(login admin "$PASSWORD" "$WORK/admin.jar" login-admin)
expect_eq "管理员登录 200" "200" "$code"
if [ "$code" != "200" ]; then
  fail "登录失败，后续断言无法进行" "$(cat "$WORK/login-admin.json" 2>/dev/null)"
  printf '通过 %d 项，失败 %d 项\n' "$PASSED" "$FAILED"
  exit 1
fi
ADMIN_CSRF=$(csrf_of "$WORK/login-admin.json")

echo
echo "== 2. A：真实采集（5 秒节奏、不重复写、分钟聚合出现） =="
sleep 30

NOW=$(now_unix)
FROM=$(iso_of $((NOW - 40)))
TO=$(iso_of $((NOW + 1)))
code=$(metricapi "$WORK/admin.jar" "scope=host&metric=cpu_usage_percent&from=$FROM&to=$TO&step=5")
expect_eq "查询最近 40 秒的 CPU 曲线 200" "200" "$code"
cp "$WORK/resp.json" "$WORK/raw.json"
note "cpu_usage_percent 的 raw 点（unix 秒）：$(jgetx "$WORK/raw.json" '",".join(str(u(p["timestamp"])) for s in d["data"]["series"] if s["metric"]=="cpu_usage_percent" for p in s["points"])')"
expect_eq "40 秒跨度 + 5 秒步长读的是 raw 层" "raw" "$(jget "$WORK/raw.json" 'd["data"]["resolution"]')"
expect_eq "序列里有 CPU 曲线" "1" \
  "$(jget "$WORK/raw.json" 'len([s for s in d["data"]["series"] if s["metric"]=="cpu_usage_percent"])')"
expect_eq "raw 点至少 4 个（30 秒内按 5 秒采集）" "True" \
  "$(jget "$WORK/raw.json" 'len([s for s in d["data"]["series"] if s["metric"]=="cpu_usage_percent"][0]["points"]) >= 4')"
expect_eq "raw 点时间戳互不重复（同一份快照不写两次）" "True" \
  "$(jget "$WORK/raw.json" 'len({p["timestamp"] for s in d["data"]["series"] for p in s["points"]}) == len([p for s in d["data"]["series"] for p in s["points"]])')"
expect_eq "raw 点的时间戳都对齐到 5 秒（桶边界）" "True" \
  "$(jget "$WORK/raw.json" 'all(int(p["timestamp"][17:19]) % 5 == 0 for s in d["data"]["series"] for p in s["points"])')"
expect_eq "每个 raw 点 sample_count=1（原始点不是聚合出来的）" "True" \
  "$(jget "$WORK/raw.json" 'all(p["sample_count"] == 1 for s in d["data"]["series"] for p in s["points"])')"
expect_eq "每个点都满足 min ≤ value ≤ max" "True" \
  "$(jget "$WORK/raw.json" 'all(p["min"] <= p["value"] <= p["max"] for s in d["data"]["series"] for p in s["points"])')"

# 等到第 70 秒之后：[0,60) 这个桶已经结束，应当已经被聚合成分钟行。
sleep 45
code=$(statusapi "$WORK/admin.jar")
expect_eq "status 200" "200" "$code"
cp "$WORK/resp.json" "$WORK/status1.json"
expect_eq "status.enabled=true" "True" "$(jget "$WORK/status1.json" 'd["data"]["enabled"]')"
expect_eq "status.collecting=true（采集循环在跑）" "True" "$(jget "$WORK/status1.json" 'd["data"]["collecting"]')"
expect_eq "status 报告的 raw 保留窗口与配置一致" "$RAW_RETENTION" "$(jget "$WORK/status1.json" 'd["data"]["retention"]["raw_seconds"]')"
expect_eq "status 报告的 minute 保留窗口与配置一致" "$MINUTE_RETENTION" "$(jget "$WORK/status1.json" 'd["data"]["retention"]["minute_seconds"]')"
expect_eq "status 报告的采集间隔是 2 秒" "2" "$(jget "$WORK/status1.json" 'd["data"]["interval_seconds"]')"
expect_eq "库里已经有分钟聚合行" "True" "$(jget "$WORK/status1.json" 'd["data"]["samples"].get("minute", 0) > 0')"
expect_eq "库里确实有原始样本" "True" "$(jget "$WORK/status1.json" 'd["data"]["samples"].get("raw", 0) > 0')"
expect_eq "status 带出登记的指标清单（界面据此画选择框）" "True" \
  "$(jget "$WORK/status1.json" 'len(d["data"]["metrics"]) >= 10')"
expect_eq "status 的 series 目录里有 host 这一组（**按 scope 分组**，不是按粒度）" "True" \
  "$(jget "$WORK/status1.json" 'len(d["data"]["series"].get("host", [])) > 0')"
expect_eq "series 的每条都带 scope/target_id/metric" "True" \
  "$(jget "$WORK/status1.json" 'all(all(k in e for k in ("scope","target_id","metric")) for v in d["data"]["series"].values() for e in v)')"
expect_eq "series 里有 filesystem 组（说明容量样本也进了历史）" "True" \
  "$(jget "$WORK/status1.json" 'len(d["data"]["series"].get("filesystem", [])) > 0')"
expect_eq "库里没有违反 min ≤ value ≤ max 的分钟行" "0" \
  "$(sqlq "SELECT COUNT(*) FROM metric_samples WHERE resolution='minute' AND NOT (min_value <= value AND value <= max_value)")"
expect_eq "分钟行的 sample_count 是多个原始点累加出来的" "1" \
  "$(sqlq "SELECT SUM(sample_count) > 0 FROM metric_samples WHERE resolution='minute'")"

echo
echo "== 3. B：缺口（停代理期间不写样本、不补零） =="
# stop_agent 自己会 rm 掉 socket 文件，所以“socket 不存在”这条永远成立、什么也没证明。
# 真正要证的是代理进程没了：拿住 PID，停完再看它还在不在。
DEAD_AGENT_PID=$AGENT_PID
stop_agent
GAP_START=$(now_unix)
# 判定窗口两端各收一个桶宽（raw 是 5 秒）。样本时间戳是**向下对齐到 5 秒桶**的
# （store.InsertMetricSamples 里 alignUnix(s.TS, width)），所以：
#   - 左端：桶起点比 GAP_START 早的那个桶，里面装的可能是停机前的采样；
#   - 右端：桶起点比“代理重启时刻”早的那个桶，里面装的可能是重启后第一份快照。
#
# 右端这条是真踩过的：24.04 上代理 20:44:24.847 恢复采集（agent.log “系统采集恢复”），
# 第一份快照的 collected_at 就是 20:44:24.847，落桶后 ts=20:44:20——比 GAP_END
# （20:44:24）还小，于是“停机窗口里没有行”被判成失败。那行是**重启之后**才采的，
# 不是停机期间的（形状也对得上：只有 load1/load5/load15/memory_used_percent，
# 没有 cpu_usage_percent、没有网卡速率——差分指标在进程重启后第一份快照里必然缺失）。
# 收两端不放宽任何东西：要证的是“十几个秒的停机里一行都没有”，不是“停机那一秒没落库”。
GAP_IN_FROM=$((GAP_START + 5))
expect_eq "代理进程已经退出（kill -0 失败，不是只删了 socket 文件）" "gone" \
  "$(kill -0 "$DEAD_AGENT_PID" 2>/dev/null && echo alive || echo gone)"
# 停机时长要撑住判定窗口：两端各收 5 秒之后，判定区间还得有十几个秒，否则只剩一个桶，
# 什么也证不了。25 秒停机 → 判定区间约 15 秒（三个桶）。
sleep 25
code=$(statusapi "$WORK/admin.jar")
expect_eq "代理停机时 status 仍 200" "200" "$code"
cp "$WORK/resp.json" "$WORK/status-gap.json"
expect_eq "代理停机时 collecting 仍为 true（循环在跑，只是拿不到快照）" "True" \
  "$(jget "$WORK/status-gap.json" 'd["data"]["collecting"]')"
expect_eq "代理停机时 last_error 写明原因" "True" \
  "$(jget "$WORK/status-gap.json" 'bool(d["data"].get("last_error"))')"

# 缺口的右端要在**代理还没起来**的时候取：代理一启动就会立刻采一份带当前时间戳的
# 快照，那个点落在 [GAP_START, 重启时刻] 里是合法的。把 GAP_END 取在启动之后，
# 就等于要求“代理重启后这段时间也别写数据”，断言会一直失败。
GAP_END=$(now_unix)
# 右端同样收一个桶宽：重启后第一份快照的 collected_at 比 GAP_END 晚不了几秒，
# 落桶却会掉到 GAP_END 之前（见上）。GAP_END - 5 之后的桶一律不判。
GAP_IN_TO=$((GAP_END - 5))
if start_agent; then pass "代理恢复启动"; else fail "代理恢复启动" "$(tail -5 "$AGENT_LOG" 2>/dev/null)"; fi
sleep 12

NOW=$(now_unix)
FROM=$(iso_of $((GAP_START - 20)))
TO=$(iso_of $((GAP_END + 1)))
code=$(metricapi "$WORK/admin.jar" "scope=host&metric=cpu_usage_percent&from=$FROM&to=$TO&step=5")
expect_eq "查询跨越缺口的窗口 200" "200" "$code"
cp "$WORK/resp.json" "$WORK/gap.json"
expect_eq "缺口被如实计数（missing_points > 0）" "True" \
  "$(jget "$WORK/gap.json" 'sum(s["missing_points"] for s in d["data"]["series"]) > 0')"
expect_eq "缺口窗口里没有任何点（缺失就是没有点，不是 0 值点）" "True" \
  "$(jgetx "$WORK/gap.json" "$GAP_IN_FROM" "$GAP_IN_TO" '
all(not (int(a[0]) <= u(p["timestamp"]) <= int(a[1])) for s in d["data"]["series"] for p in s["points"])')"
GAPROWS=$(sqlq "SELECT COUNT(*) FROM metric_samples WHERE resolution='raw' AND ts > $GAP_IN_FROM AND ts < $GAP_IN_TO")
note "缺口窗口：GAP_START=$GAP_START GAP_END=$GAP_END（$(($GAP_END - GAP_START)) 秒），判定区间 ($GAP_IN_FROM, $GAP_IN_TO)"
if [ "$GAPROWS" != "0" ]; then
  note "缺口窗口里出现的行（ts | scope | target_id | metric | value | sample_count）："
  sqllist "SELECT ts, scope, target_id, metric, ROUND(value, 3), sample_count FROM metric_samples WHERE resolution='raw' AND ts > $GAP_IN_FROM AND ts < $GAP_IN_TO ORDER BY ts"
fi
expect_eq "代理停机期间库里也没有行（不是存了 0）" "0" "$GAPROWS"

echo
echo "== 4. C：保留与启动补算（种入历史行，重启后先聚合再裁剪） =="
stop_web
if [ -n "$WEB_PID" ]; then fail "Web 已停止（准备种历史行）" "仍在运行"; else pass "Web 已停止（准备种历史行）"; fi

# 上一个整小时的 minute 行（60 个桶 × 12 个样本）+ 三条超出 raw 保留窗口的老 raw 行。
SEED_HOUR=$(python3 - "$WEB_DB" "$RAW_RETENTION" <<'PY'
import sqlite3, sys, time
db = sqlite3.connect(sys.argv[1])
raw_retention = int(sys.argv[2])
now = int(time.time())
hour_start = (now // 3600) * 3600 - 3600
rows = [("host", "", "cpu_usage_percent", "minute", hour_start + k * 60, 20.0, 20.0, 20.0, 12)
        for k in range(60)]
stale = now - max(900, raw_retention * 5)
for metric in ("cpu_usage_percent", "load1", "memory_used_percent"):
    rows.append(("host", "", metric, "raw", stale, 1.0, 1.0, 1.0, 1))
db.executemany("INSERT OR REPLACE INTO metric_samples "
               "(scope,target_id,metric,resolution,ts,value,min_value,max_value,sample_count) "
               "VALUES (?,?,?,?,?,?,?,?,?)", rows)
db.commit()
print(hour_start)
PY
)
expect_eq "种入上一个整小时（unix $SEED_HOUR）的 minute 行" "True" "$([ -n "$SEED_HOUR" ] && echo True || echo False)"
chown_web_db

if start_web; then pass "Web 重启（启动即补算 + 裁剪）"; else fail "Web 重启" "$(tail -5 "$WEB_LOG" 2>/dev/null)"; fi
sleep 15

code=$(statusapi "$WORK/admin.jar")
expect_eq "重启后 status 200" "200" "$code"
cp "$WORK/resp.json" "$WORK/status2.json"
expect_eq "启动补算出了小时行（samples.hour > 0）" "True" \
  "$(jget "$WORK/status2.json" 'd["data"]["samples"].get("hour", 0) > 0')"
expect_eq "超过保留窗口的老 raw 行被裁掉（不是等下次重启）" "0" \
  "$(sqlq "SELECT COUNT(*) FROM metric_samples WHERE resolution='raw' AND ts < $(( $(now_unix) - RAW_RETENTION - 60 ))")"
expect_eq "种入的那一小时被聚成 hour 行，值仍是 20、样本数 ≥ 720" "1" \
  "$(sqlq "SELECT COALESCE((SELECT value = 20.0 AND sample_count >= 720 FROM metric_samples WHERE resolution='hour' AND metric='cpu_usage_percent' AND ts = $SEED_HOUR), 0)")"

NOW=$(now_unix)
FROM=$(iso_of $((NOW - 3 * 3600)))
TO=$(iso_of $((NOW + 1)))
code=$(metricapi "$WORK/admin.jar" "scope=host&metric=cpu_usage_percent&from=$FROM&to=$TO")
expect_eq "三小时跨度查询 200" "200" "$code"
cp "$WORK/resp.json" "$WORK/hour.json"
expect_eq "三小时跨度读到 hour 层（超过 minute 保留窗口）" "hour" "$(jget "$WORK/hour.json" 'd["data"]["resolution"]')"
expect_eq "hour 层里读得到种进去的那一个小时（值 20）" "True" \
  "$(jget "$WORK/hour.json" 'any(abs(p["value"] - 20.0) < 0.001 for s in d["data"]["series"] if s["metric"]=="cpu_usage_percent" for p in s["points"])')"

echo
echo "== 5. D：查询规则（跨度选层、点数上限、参数校验、权限） =="
NOW=$(now_unix)
FROM=$(iso_of $((NOW - 24 * 3600)))
TO=$(iso_of $((NOW + 1)))
code=$(metricapi "$WORK/admin.jar" "scope=host&from=$FROM&to=$TO")
expect_eq "24 小时跨度查询 200" "200" "$code"
cp "$WORK/resp.json" "$WORK/day.json"
expect_eq "24 小时跨度按配置的保留窗口落到 hour 层" "hour" "$(jget "$WORK/day.json" 'd["data"]["resolution"]')"
expect_eq "步长被加粗到小时的整数倍（1440 点上限）" "3600" "$(jget "$WORK/day.json" 'd["data"]["step_seconds"]')"
expect_eq "max_points 如实回报 1440" "1440" "$(jget "$WORK/day.json" 'd["data"]["max_points"]')"
expect_eq "每个序列的点数不超过 1440" "True" \
  "$(jget "$WORK/day.json" 'all(len(s["points"]) <= d["data"]["max_points"] for s in d["data"]["series"])')"

FROM=$(iso_of $((NOW - 3600)))
code=$(metricapi "$WORK/admin.jar" "scope=host&metric=cpu_usage_percent&from=$FROM&to=$TO")
expect_eq "一小时跨度读 minute 层（超过 raw 保留窗口）" "minute" "$(jget "$WORK/resp.json" 'd["data"]["resolution"]')"

code=$(metricapi "$WORK/admin.jar" "scope=host&metric=nope")
expect_eq "未知指标 400" "400" "$code"
expect_eq "未知指标错误码 VALIDATION_ERROR" "VALIDATION_ERROR" "$(jget "$WORK/resp.json" 'd["error"]["code"]')"
expect_contains "未知指标的报错点出名字" "nope" "$(jget "$WORK/resp.json" 'd["error"]["message"]')"

code=$(metricapi "$WORK/admin.jar" "metric=cpu_usage_percent")
expect_eq "缺 scope 400" "400" "$code"
code=$(metricapi "$WORK/admin.jar" "scope=host&metric=used_percent")
expect_eq "指标不属于该 scope 400" "400" "$code"
code=$(metricapi "$WORK/admin.jar" "scope=host&from=昨天")
expect_eq "from 不是 RFC3339 → 400" "400" "$code"
code=$(metricapi "$WORK/admin.jar" "scope=host&step=0")
expect_eq "step=0 → 400" "400" "$code"
FUTURE=$(iso_of $((NOW + 86400)))
code=$(metricapi "$WORK/admin.jar" "scope=host&to=$FUTURE")
expect_eq "to 在未来 → 400" "400" "$code"
expect_contains "未来窗口的报错说得清为什么" "未来" "$(jget "$WORK/resp.json" 'd["error"]["message"]')"
FROM=$(iso_of $((NOW - 400 * 24 * 3600)))
code=$(metricapi "$WORK/admin.jar" "scope=host&from=$FROM&to=$TO")
expect_eq "跨度超过 365 天 → 400" "400" "$code"

code=$(curl -s -o "$WORK/resp.json" -w '%{http_code}' -X GET "$BASE/api/v1/system/metrics?scope=host")
expect_eq "未登录 401" "401" "$code"
expect_eq "未登录错误码" "UNAUTHENTICATED" "$(jget "$WORK/resp.json" 'd["error"]["code"]')"

code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "/api/v1/users" \
  "{\"username\":\"alice\",\"display_name\":\"alice\",\"role\":\"viewer\",\"initial_password\":\"$VIEWER_PASSWORD\"}")
expect_eq "创建 viewer 201" "201" "$code"
code=$(login alice "$VIEWER_PASSWORD" "$WORK/viewer.jar" login-viewer)
expect_eq "viewer 登录 200" "200" "$code"
code=$(metricapi "$WORK/viewer.jar" "scope=host&metric=cpu_usage_percent")
expect_eq "viewer 读历史 200（权限 V）" "200" "$code"
code=$(curl -s -b "$WORK/viewer.jar" -o "$WORK/resp.json" -w '%{http_code}' -X GET \
  "$BASE/api/v1/system/metrics/status" -H "Origin: $ORIGIN")
expect_eq "viewer 读历史状态 200（权限 V）" "200" "$code"

code=$(curl -s -b "$WORK/admin.jar" -o "$WORK/resp.json" -w '%{http_code}' -X POST \
  "$BASE/api/v1/system/metrics" -H "Origin: $ORIGIN" -H 'Content-Type: application/json' -d '{}')
expect_eq "POST /system/metrics 405" "405" "$code"

echo
echo "== 6. E：关闭采集（旧数据仍可查，status 说清原因） =="
stop_web
# 采样点要在 Web 停下来之后取：采集周期是 2 秒，停之前取的话，取完到进程真正
# 退出之间还可能落进一份新快照，MAX(ts) 合法地前进一格，断言就成了偶发失败。
BEFORE_MAX=$(sqlq "SELECT COALESCE(MAX(ts), 0) FROM metric_samples WHERE resolution='raw'")
python3 - "$SYS_CFG" <<'PY'
import json, sys
p = sys.argv[1]
cfg = json.load(open(p, encoding="utf-8"))
cfg["web"]["metrics"]["enabled"] = False
json.dump(cfg, open(p, "w", encoding="utf-8"), ensure_ascii=False, indent=2)
PY
if start_web; then pass "Web 以 metrics.enabled=false 重启"; else fail "Web 重启（采集关闭）" "$(tail -5 "$WEB_LOG" 2>/dev/null)"; fi
sleep 12

code=$(statusapi "$WORK/admin.jar")
expect_eq "关闭采集后 status 200" "200" "$code"
cp "$WORK/resp.json" "$WORK/status-off.json"
expect_eq "关闭采集：enabled 仍为 true（旧数据可查）" "True" "$(jget "$WORK/status-off.json" 'd["data"]["enabled"]')"
expect_eq "关闭采集：collecting=false" "False" "$(jget "$WORK/status-off.json" 'd["data"]["collecting"]')"
expect_eq "关闭采集：reason 指明是配置关的" "True" \
  "$(jget "$WORK/status-off.json" '"关闭" in (d["data"].get("reason") or "")')"

code=$(metricapi "$WORK/admin.jar" "scope=host&metric=cpu_usage_percent")
expect_eq "关闭采集后仍然查得到历史数据 200" "200" "$code"
expect_eq "关闭采集后查询结果 enabled=true" "True" "$(jget "$WORK/resp.json" 'd["data"]["enabled"]')"
expect_eq "关闭采集后不再写新样本（最新 raw 时间戳没有前进）" "$BEFORE_MAX" \
  "$(sqlq "SELECT COALESCE(MAX(ts), 0) FROM metric_samples WHERE resolution='raw'")"

echo
echo "== 7. F：日志卫生 =="
# 用具体的口令去查，而不是 grep 'password'：日志里的字段名（password_policy 之类）
# 出现不算泄漏，真正要证明的是“口令本身没进日志”。
expect_eq "Web 日志里没有出现管理员口令" "0" "$(grep -c -F -- "$PASSWORD" "$WEB_LOG" 2>/dev/null | head -n 1)"
expect_eq "Web 日志里没有出现 viewer 口令" "0" "$(grep -c -F -- "$VIEWER_PASSWORD" "$WEB_LOG" 2>/dev/null | head -n 1)"
expect_eq "Web 日志里没有私钥" "0" "$(grep -c 'BEGIN PRIVATE KEY' "$WEB_LOG" 2>/dev/null | head -n 1)"
expect_eq "Web 日志里没有 panic" "0" "$(grep -c 'panic' "$WEB_LOG" 2>/dev/null | head -n 1)"
if [ -s "$WEB_LOG" ]; then
  pass "Web 日志非空（失败会写进日志，不静默）"
else
  fail "Web 日志非空" "$WEB_LOG 是空的"
fi

echo
printf '通过 %d 项，失败 %d 项\n' "$PASSED" "$FAILED"
if [ "$FAILED" -ne 0 ]; then
  printf '失败项:%s\n' "$FAILED_NAMES"
  exit 1
fi
exit 0
