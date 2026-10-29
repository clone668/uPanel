#!/bin/sh
# P13.4 Linux 验收脚本：确认动作、投递账读路径、历史曲线的"缺失不是 0"、采集陈旧三态。
#
# 运行位置：Ubuntu 22.04/24.04 验收 VM 内，**必须以 root 运行**。不依赖 Go 工具链：
# 用宿主交叉编译好的 Linux 二进制。
#
# 用法：
#   sudo BIN_DIR=/tmp/upanel-bin sh tests/integration/p13-4-acceptance.sh
#
# 退出码：0 全部通过；1 有失败项；2 环境不满足（非 root、缺 python3/runuser）。
#
# 覆盖的验收点（STAGES P13.4、PLAN §18、CONTRACTS §13.4）：
#   A. 确认权限与校验：未登录 401、viewer 403（只读能看事件但不能认领）、admin 缺 CSRF
#      403、id 非正整数 400、未知 id 404 且消息是"告警事件不存在"、被拒的每一次都落审计
#   B. 确认语义：真事件上确认成功；`acked_by` 是会话里的人而不是提交上来的名字；
#      **state 仍是 firing**（确认不是恢复）；第二个管理员再确认时时间与人都不变
#      （幂等 + 首告者胜）；确认之后事件仍然出现在 `state=firing` 列表里
#   C. 投递账读路径：未登录 401、viewer 403、admin 200；行里带着**投递当时**的通道快照
#      与事件引用；state/kind 过滤按闭集生效、越界枚举 400；limit 正整数校验、
#      超出上限按 200 截断而不是报错；kind=test 在没有测试发送时是空
#   D. **缺失不是 0**：真停采集 45 秒，缺口区间在 `metric_samples` 里**一行都没有**；
#      查询结果的 `missing_points` 数得出缺的桶、相邻两点真的断开、缺口里没有 value=0
#      的假点；重新采集之后缺口**不被回填**（历史是历史）
#   E. 采集陈旧三态：`/system/metrics/status` 的 enabled/collecting/interval_seconds/
#      last_sample_at 可读且互相自洽；把历史关掉重启，`/system/metrics` 回
#      enabled=false + 空 series + 说明原因，而不是"没有数据"
#   F. 卫生：web/agent 日志无 panic；确认与查询都没有写系统（本脚本不涉及任何系统写操作）
#
# 为什么不用浏览器验前端：这一层的界面（历史曲线、告警列表、确认按钮）由宿主侧的
# `vue/server-renderer` 渲染测试与 `web-smoke` 覆盖（DECISIONS Q21），VM 上跑的是二进制，
# 没有 Playwright。断言"前端会怎么显示"必须靠那些测试，这里只钉**它拿到的事实**。
#
# 为什么用受控种入而不压 CPU：告警的"持续 30 秒"是契约尺度，压机器会让断言跟着调度走。
# 这里给一条不存在的网卡 accept0 写 rx_bytes_per_sec 样本（5 秒一个桶），值由
# $WORK/probe.phase 决定（high=5000 / low=10），哪一秒超限由脚本说了算。

set -u

# 秘密走文件而不是 argv：请求体先写进 $WORK/req.json（本 umask 下是 0600）再用
# curl -d @file 送出去。这样 ps 里看不到口令。
umask 077

BIN_DIR=${BIN_DIR:-/tmp/upanel-bin}
if [ "$(id -u)" -ne 0 ]; then
  printf '本脚本必须以 root 运行：sudo BIN_DIR=%s sh %s\n' "$BIN_DIR" "$0" >&2
  exit 2
fi

AGENT="$BIN_DIR/upanel-agent"
WEB="$BIN_DIR/upanel-web"
CTL="$BIN_DIR/upanelctl"

WORK=${WORK:-/var/tmp/upanel-accept-p13-4}
RUN_DIR=/run/upanel
SOCK=$RUN_DIR/agent.sock
SYS_CFG=/etc/upanel/upanel.json
AGENT_DB_DIR=/var/lib/upanel-agent
AGENT_DB=$AGENT_DB_DIR/agent.db
KEY_FILE=/etc/upanel/agent.key
WEB_DB_DIR=$WORK/webdata
WEB_DB=$WEB_DB_DIR/upanel.db
PORT=${PORT:-19464}
BASE="http://127.0.0.1:$PORT"
ORIGIN="$BASE"
WEB_USER=upanel-web
PASSWORD='vm-correct-horse-9'
VIEWER_PASSWORD='vm-viewer-horse-42'
BOB_PASSWORD='vm-second-admin-7'

# 采集节奏 2 秒：raw 保留一小时足够放下本轮所有查询窗口。容量与小时的保留窗口给宽，
# 免得清理线程把正在被判定的样本裁掉。
RAW_RETENTION=3600
MINUTE_RETENTION=7200
HOUR_RETENTION=14400

# 受控种入的序列：一条不存在的网卡，只有本脚本会写它。
SEED_TARGET=accept0
SEED_METRIC=rx_bytes_per_sec
PHASE_FILE=$WORK/probe.phase

AGENT_LOG=$WORK/logs/agent.log
WEB_LOG=$WORK/logs/web.log
SEED_LOG=$WORK/logs/seed.log

PASSED=0
FAILED=0
FAILED_NAMES=""
AGENT_PID=""
WEB_PID=""
SEED_PID=""

pass() { PASSED=$((PASSED + 1)); printf 'PASS  %s\n' "$1"; }
fail() { FAILED=$((FAILED + 1)); FAILED_NAMES="$FAILED_NAMES | $1"; printf 'FAIL  %s: %s\n' "$1" "${2:-}"; }
note() { printf 'NOTE  %s\n' "$1"; }

expect_eq() { if [ "$2" = "$3" ]; then pass "$1"; else fail "$1" "期望 '$2'，实际 '$3'"; fi; }
expect_ge() { if is_num "$3" && [ "$3" -ge "$2" ]; then pass "$1"; else fail "$1" "期望 ≥ $2，实际 '$3'"; fi; }

# is_num <值>：能不能安全地当整数用。从库里读回来的东西可能是 <missing> 或
# <sql-error: ...>，对它们做算术会让 dash 直接退场。
is_num() {
  case ${1:-} in
    ''|*[!0-9-]*) return 1 ;;
    -) return 1 ;;
    *) return 0 ;;
  esac
}

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

# jget <文件> <python 表达式>：把表达式的结果印出来，d 是解析后的响应体。
# 布尔统一印成 true/false：Python 的 True/False 与 JSON 的 true/false 混在一份脚本里，
# 会让断言写成 "false" 而实际拿到 "False"，那种失败看起来像产品不生效。
jget() {
  python3 -c 'import json,sys
d=json.load(open(sys.argv[1], encoding="utf-8"))
v=eval(sys.argv[2])
print(str(v).lower() if isinstance(v, bool) else v)' "$1" "$2" 2>/dev/null
}

# sqlq <单值查询>：查不到时印 <missing>，查询本身出错才印 <sql-error: ...>。
# 两者必须分开，否则下一个把结果拼进 SQL 的断言报出来的是语法错误而不是"没这一行"。
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

now_unix() { date -u +%s; }
iso_of() { date -u -d "@$1" +%Y-%m-%dT%H:%M:%SZ; }

# 库里"有多少行样本"的小工具：wait_until 里要反复问，写进一行会读不懂。
host_samples() { sqlq "SELECT COUNT(*) FROM metric_samples WHERE scope = 'host'"; }
seed_samples() { sqlq "SELECT COUNT(*) FROM metric_samples WHERE scope = 'interface' AND target_id = 'accept0'"; }
seed_samples_after() {
  sqlq "SELECT COUNT(*) FROM metric_samples WHERE scope = 'interface' AND target_id = 'accept0' AND ts > $1"
}

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

# 种入器：往 metric_samples 写 accept0 的 rx_bytes_per_sec，值取自 phase 文件。
# 直写库而不是走采集：主机上根本没有 accept0 这块网卡，而"告警会不会响"要由脚本说了算。
write_seeder() {
  cat >"$WORK/seed.py" <<'PY'
import sqlite3
import sys
import time

SCOPE, TARGET, METRIC = "interface", "accept0", "rx_bytes_per_sec"
BUCKET = 5
VALUES = {"high": 5000.0, "low": 10.0}


def main():
    db_path, phase_path = sys.argv[1], sys.argv[2]
    db = sqlite3.connect(db_path, timeout=15)
    db.execute("PRAGMA busy_timeout = 15000")

    def value_now():
        try:
            with open(phase_path, encoding="utf-8") as f:
                return VALUES.get(f.read().strip())
        except OSError:
            return None

    while True:
        t = time.time()
        wake = (int(t) // BUCKET + 1) * BUCKET
        time.sleep(max(0.0, wake - t))
        value = value_now()
        if value is None:
            continue
        try:
            db.execute(
                "INSERT INTO metric_samples"
                " (scope, target_id, metric, resolution, ts, value, min_value, max_value, sample_count)"
                " VALUES (?, ?, ?, 'raw', ?, ?, ?, ?, 1)"
                " ON CONFLICT (scope, target_id, metric, resolution, ts)"
                " DO UPDATE SET value=excluded.value, min_value=excluded.min_value,"
                " max_value=excluded.max_value",
                (SCOPE, TARGET, METRIC, wake, value, value, value))
            db.commit()
        except sqlite3.Error as exc:
            print("seed: 写入失败（继续）: %s" % exc, file=sys.stderr, flush=True)


main()
PY
  chmod 0644 "$WORK/seed.py"
}

start_seed() {
  runuser -u "$WEB_USER" -- python3 "$WORK/seed.py" "$WEB_DB" "$PHASE_FILE" \
    >>"$SEED_LOG" 2>&1 &
  SEED_PID=$!
}

stop_seed() {
  if [ -n "$SEED_PID" ] && kill -0 "$SEED_PID" 2>/dev/null; then
    kill -TERM "$SEED_PID" 2>/dev/null
    wait "$SEED_PID" 2>/dev/null
  fi
  SEED_PID=""
}

# phase <high|low>：切种入的值（写临时文件再 mv，避免种入循环读到半行）。
# 必须 chmod 0644：种入以 Web 账号运行，而本脚本的 umask 是 077。
phase() {
  printf '%s\n' "$1" >"$PHASE_FILE.tmp" && chmod 0644 "$PHASE_FILE.tmp" \
    && mv -f "$PHASE_FILE.tmp" "$PHASE_FILE"
}

# ---- HTTP ----

login() {
  curl -s -c "$3" -o "$WORK/$4.json" -w '%{http_code}' -X POST "$BASE/api/v1/auth/login" \
    -H 'Content-Type: application/json' -H 'X-UPanel-Request: 1' -H "Origin: $ORIGIN" \
    -d "{\"username\":\"$1\",\"password\":\"$2\"}"
}

csrf_of() { jget "$1" 'd["data"]["csrf_token"]'; }

# api <jar> <csrf> <method> <path> [body] → HTTP 状态码，响应写进 $WORK/resp.json
#
# CSRF 令牌**始终**带上：早先版本只在有 body 时才加这个头，于是所有无 body 的写请求
# 都拿到 403 CSRF_FAILED——那不是产品的边界，是脚本的 bug。
api() {
  jar=$1; csrf=$2; method=$3; path=$4; body=${5:-}
  if [ -n "$body" ]; then
    printf '%s' "$body" >"$WORK/req.json"
    curl -s -b "$jar" -o "$WORK/resp.json" -w '%{http_code}' -X "$method" "$BASE$path" \
      -H 'Content-Type: application/json' -H "Origin: $ORIGIN" -H "X-CSRF-Token: $csrf" \
      -d @"$WORK/req.json"
  else
    curl -s -b "$jar" -o "$WORK/resp.json" -w '%{http_code}' -X "$method" "$BASE$path" \
      -H "Origin: $ORIGIN" -H "X-CSRF-Token: $csrf"
  fi
}

# apiout <jar> <output> <path>：只读 GET，结果写进指定文件（带 Origin，不带 CSRF）。
apiout() {
  curl -s -b "${1:--}" -o "$2" -w '%{http_code}' -X GET "$BASE$3" -H "Origin: $ORIGIN"
}

# wait_until <说明> <超时秒> <shell 片段>：片段返回 0 即通过。
# 片段用 eval 执行，读的是全局变量；超时就如实报"没等到"，不静默继续。
wait_until() {
  desc=$1; limit=$2; shift 2
  deadline=$(( $(now_unix) + limit ))
  while :; do
    if eval "$*" >/dev/null 2>&1; then return 0; fi
    if [ "$(now_unix)" -ge "$deadline" ]; then
      printf 'NOTE  等待超时（%s 秒）：%s\n' "$limit" "$desc"
      return 1
    fi
    sleep 2
  done
}

event_state_of() {
  case ${1:-} in
    ''|*[!0-9]*) printf '<no-rule>\n' ;;
    *) sqlq "SELECT state FROM alert_events WHERE rule_id = $1 ORDER BY id DESC LIMIT 1" ;;
  esac
}

chown_web_db() {
  for suffix in "" "-wal" "-shm"; do
    [ -e "$WEB_DB$suffix" ] && chown "$WEB_USER" "$WEB_DB$suffix"
  done
}

cleanup() {
  stop_seed
  stop_web
  stop_agent
}
trap cleanup EXIT
trap 'cleanup; exit 130' INT
trap 'cleanup; exit 143' TERM

install -d -m 0700 "$WORK" "$WORK/logs"
# $WORK 自身必须可穿过（0711）：Web 以 upanel-web 身份运行，进不去 /var/tmp 下这个 0700
# 目录的话，症状是"数据库打开失败：permission denied"——看起来像产品的库坏了，其实是
# 脚本把门锁上了。只给穿过、不给列目录；里面的日志仍是 root 0700。
chmod 0711 "$WORK"
rm -rf "$WEB_DB_DIR"
install -d -m 0750 "$WEB_DB_DIR"
chown "$WEB_USER" "$WEB_DB_DIR"
: >"$AGENT_LOG"
: >"$WEB_LOG"
: >"$SEED_LOG"

echo "== 0. 环境"
if [ "$(id -u)" -eq 0 ]; then pass "验收脚本以 root 运行"; else fail "验收脚本以 root 运行" "当前 UID $(id -u)"; fi
for b in "$AGENT" "$WEB" "$CTL"; do
  if [ -x "$b" ]; then pass "二进制存在且可执行 $(basename "$b")"; else fail "二进制存在且可执行 $(basename "$b")" "$b"; fi
done
if command -v python3 >/dev/null 2>&1; then pass "python3 可用（种入与响应解析）"; else fail "python3 可用" "缺少 python3"; fi
if command -v runuser >/dev/null 2>&1; then pass "runuser 可用（以 Web 身份写库）"; else fail "runuser 可用" "缺少 runuser"; fi
if id -u "$WEB_USER" >/dev/null 2>&1; then pass "服务账号 $WEB_USER 存在"; else fail "服务账号 $WEB_USER 存在" "缺账号"; fi

echo
echo "== 1. 装配：代理 + 管理员 + 第二个管理员 + viewer + Web =="

install -d -m 0755 /etc/upanel
install -d -m 0700 "$AGENT_DB_DIR"
rm -f "$AGENT_DB" "$AGENT_DB-wal" "$AGENT_DB-shm"
rm -rf "$WEB_DB_DIR"
install -d -m 0750 "$WEB_DB_DIR"
chown "$WEB_USER" "$WEB_DB_DIR"
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

printf '%s\n%s\n' "$PASSWORD" "$PASSWORD" | "$CTL" init-admin --config "$SYS_CFG" --db "$WEB_DB" \
  --username admin --display-name "管理员" --json >"$WORK/init.out" 2>"$WORK/init.err"
expect_eq "init-admin 退出码" "0" "$?"
chown_web_db

if start_web; then pass "Web 启动"; else fail "Web 启动" "$(tail -5 "$WEB_LOG" 2>/dev/null)"; fi

code=$(login admin "$PASSWORD" "$WORK/admin.jar" login-admin)
expect_eq "管理员登录 200" "200" "$code"
if [ "$code" != "200" ]; then
  fail "登录失败，后续断言无法进行" "$(cat "$WORK/login-admin.json" 2>/dev/null)"
  printf '通过 %d 项，失败 %d 项\n' "$PASSED" "$FAILED"
  exit 1
fi
ADMIN_CSRF=$(csrf_of "$WORK/login-admin.json")

# 第二个管理员：用来验"首告者胜"——同一个名字自己盖两次是验不出来的。
code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "/api/v1/users" \
  "{\"username\":\"bob\",\"display_name\":\"bob\",\"role\":\"admin\",\"initial_password\":\"$BOB_PASSWORD\"}")
expect_eq "创建第二个管理员 201" "201" "$code"
code=$(login bob "$BOB_PASSWORD" "$WORK/bob.jar" login-bob)
expect_eq "第二个管理员登录 200" "200" "$code"
BOB_CSRF=$(csrf_of "$WORK/login-bob.json")

code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "/api/v1/users" \
  "{\"username\":\"alice\",\"display_name\":\"alice\",\"role\":\"viewer\",\"initial_password\":\"$VIEWER_PASSWORD\"}")
expect_eq "创建 viewer 201" "201" "$code"
code=$(login alice "$VIEWER_PASSWORD" "$WORK/viewer.jar" login-viewer)
expect_eq "viewer 登录 200" "200" "$code"
VIEWER_CSRF=$(csrf_of "$WORK/login-viewer.json")

echo
echo "== 2. D：采集状态与历史查询的基线 =="

code=$(apiout "$WORK/admin.jar" "$WORK/status.json" "/api/v1/system/metrics/status")
expect_eq "读采集状态 200" "200" "$code"
expect_eq "历史监控已开启" "true" "$(jget "$WORK/status.json" 'd["data"]["enabled"]')"
expect_eq "采集循环在跑" "true" "$(jget "$WORK/status.json" 'd["data"]["collecting"]')"
expect_eq "采集节奏是配置里的 2 秒" "2" "$(jget "$WORK/status.json" 'd["data"]["interval_seconds"]')"

# 等第一个真实样本落库：没有它，"陈旧"与"正常"分不开。
if wait_until "第一次主机采集写库" 60 '[ "$(host_samples)" -ge 1 ] 2>/dev/null'; then
  pass "主机指标开始落库"
else
  fail "主机指标开始落库" "60 秒内 metric_samples 里没有 host 行"
fi
expect_eq "status 的 last_sample_at 不是 null" "true" \
  "$(jget "$WORK/status.json" 'd["data"]["last_sample_at"] is not None')"
# 采集在跑就不能说陈旧：判据是 max(3×2s, 20s)，刚起 60 秒之内一定在窗口内。
#
# 量尺用**同一份响应里的** generated_at，不用宿主侧的 now。这份响应是面板在生成它的
# 那一刻自报的，`generated_at - last_sample_at` 才是"面板自己觉得多久没落样本"。
# 用 now 去比会在慢机上假红：24.04 走 TCG 单线程时，从取回状态到算出结果之间能过去
# 十几秒（中间一次 sqlite 查询加两次 python3 冷启动，每次三四秒），样本明明是每 5 秒
# 一条、库里连续无缺口，却因为"尺子自己走到了后面"被判成陈旧。那是尺子的问题，不是
# 产品的问题——所以换成响应内自洽的口径，并把量到的秒数记进日志。
fresh_age=$(python3 -c 'import json,sys,datetime
def parse(s):
    return datetime.datetime.strptime(s[:19], "%Y-%m-%dT%H:%M:%S").replace(tzinfo=datetime.timezone.utc)
d=json.load(open(sys.argv[1], encoding="utf-8"))
s=d["data"]["last_sample_at"]
g=d["data"]["generated_at"]
if s is None or g is None: print(-1); raise SystemExit
print(int((parse(g)-parse(s)).total_seconds()))' "$WORK/status.json")
printf 'NOTE  状态自报：generated_at 比 last_sample_at 晚 %s 秒\n' "$fresh_age"
expect_eq "刚落的样本让健康度落在 20 秒判据内" "true" \
  "$([ "${fresh_age:-x}" -ge 0 ] 2>/dev/null && [ "$fresh_age" -le 20 ] && echo true || echo false)"

write_seeder
phase high
start_seed
if wait_until "种入序列写出第一个点" 60 '[ "$(seed_samples)" -ge 1 ] 2>/dev/null'; then
  pass "受控序列 accept0 开始写入（5 秒一个桶）"
else
  fail "受控序列 accept0 开始写入" "$(tail -3 "$SEED_LOG" 2>/dev/null)"
fi

# 规则两条，打在同一个序列上：
#   R1 用来验确认（触发后一直高，事件长时间 firing）
#   R2 用来验"已恢复的事件也能确认"（同样触发，之后随种入转低而恢复）
# 阈值必须高于恢复线（死区），否则 400。
code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "/api/v1/alerts/rules" \
  "{\"name\":\"P13.4-确认\",\"target_id\":\"$SEED_TARGET\",\"metric\":\"$SEED_METRIC\",\"comparison\":\"gte\",\"threshold\":4000,\"recovery_threshold\":100,\"duration_seconds\":30,\"cooldown_seconds\":0}")
expect_eq "建规则 R1（确认用）201" "201" "$code"
R1=$(jget "$WORK/resp.json" 'd["data"]["id"]')
expect_eq "规则 R1 的 state 初始是 ok" "ok" "$(jget "$WORK/resp.json" 'd["data"]["state"]')"
# scope 由 metric + target_id 推出（面板不接受提交 scope）：这条钉住"推出来的对"。
expect_eq "规则 R1 的 scope 被推成 interface" "interface" "$(jget "$WORK/resp.json" 'd["data"]["scope"]')"

code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "/api/v1/alerts/rules" \
  "{\"name\":\"P13.4-恢复后确认\",\"target_id\":\"$SEED_TARGET\",\"metric\":\"$SEED_METRIC\",\"comparison\":\"gte\",\"threshold\":4500,\"recovery_threshold\":100,\"duration_seconds\":30,\"cooldown_seconds\":0}")
expect_eq "建规则 R2（恢复后确认用）201" "201" "$code"
R2=$(jget "$WORK/resp.json" 'd["data"]["id"]')

if wait_until "两条规则都进入 firing" 150 '[ "$(event_state_of '"$R1"')" = firing ] && [ "$(event_state_of '"$R2"')" = firing ]'; then
  pass "受控序列触发两条规则（事件已产生）"
else
  fail "受控序列触发两条规则" "R1=$(event_state_of "$R1") R2=$(event_state_of "$R2")"
fi

R1_EVENT=$(sqlq "SELECT id FROM alert_events WHERE rule_id = $R1 ORDER BY id DESC LIMIT 1")
R2_EVENT=$(sqlq "SELECT id FROM alert_events WHERE rule_id = $R2 ORDER BY id DESC LIMIT 1")
note "事件：R1=$R1_EVENT R2=$R2_EVENT"
if is_num "$R1_EVENT" && is_num "$R2_EVENT"; then
  pass "两条事件都拿到了 id"
else
  fail "两条事件都拿到了 id" "R1=$R1_EVENT R2=$R2_EVENT"
  printf '通过 %d 项，失败 %d 项\n' "$PASSED" "$FAILED"
  exit 1
fi

echo
echo "== 3. A：确认的权限与校验 =="

code=$(curl -s -o "$WORK/resp.json" -w '%{http_code}' -X POST \
  "$BASE/api/v1/alerts/events/$R1_EVENT/ack" -H "Origin: $ORIGIN")
expect_eq "未登录确认 401" "401" "$code"
expect_eq "未登录的错误码" "UNAUTHENTICATED" "$(jget "$WORK/resp.json" 'd["error"]["code"]')"

code=$(api "$WORK/viewer.jar" "$VIEWER_CSRF" POST "/api/v1/alerts/events/$R1_EVENT/ack")
expect_eq "viewer 确认 403（只读能看事件，但不能认领）" "403" "$code"
expect_eq "viewer 被拒的错误码" "FORBIDDEN" "$(jget "$WORK/resp.json" 'd["error"]["code"]')"

code=$(curl -s -b "$WORK/admin.jar" -o "$WORK/resp.json" -w '%{http_code}' -X POST \
  "$BASE/api/v1/alerts/events/$R1_EVENT/ack" -H "Origin: $ORIGIN")
expect_eq "管理员不带 CSRF 确认 403" "403" "$code"

code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "/api/v1/alerts/events/0/ack")
expect_eq "id=0 → 400" "400" "$code"
expect_eq "id=0 的错误码" "VALIDATION_ERROR" "$(jget "$WORK/resp.json" 'd["error"]["code"]')"
expect_contains "报错点出 id 的规则" "id" "$(jget "$WORK/resp.json" 'd["error"]["message"]')"

code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "/api/v1/alerts/events/abc/ack")
expect_eq "id=abc → 400" "400" "$code"

code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "/api/v1/alerts/events/99999999/ack")
expect_eq "不存在的事件 → 404" "404" "$code"
expect_eq "不存在的错误码" "NOT_FOUND" "$(jget "$WORK/resp.json" 'd["error"]["code"]')"
expect_eq "不存在的报错原文" "告警事件不存在" "$(jget "$WORK/resp.json" 'd["error"]["message"]')"

# 上面五次被拒，库里要有痕迹：确认是一条会写审计的动作，被拒也是事实。
expect_ge "被拒的确认都落了审计" 4 "$(sqlq "SELECT COUNT(*) FROM audit_events WHERE action = 'alert.event.ack' AND outcome = 'failure'")"
expect_contains "被拒审计里记着错误码" "NOT_FOUND" \
  "$(sqlq "SELECT redacted_details FROM audit_events WHERE action = 'alert.event.ack' AND outcome = 'failure' ORDER BY id DESC LIMIT 1")"

echo
echo "== 4. B：确认的语义（不是恢复、幂等、首告者胜） =="

expect_eq "确认前 acked_at 是空的" "1" "$(sqlq "SELECT COUNT(*) FROM alert_events WHERE id = $R1_EVENT AND acked_at IS NULL")"

code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "/api/v1/alerts/events/$R1_EVENT/ack")
expect_eq "管理员确认 200" "200" "$code"
expect_eq "确认回的是这条事件" "$R1_EVENT" "$(jget "$WORK/resp.json" 'd["data"]["id"]')"
expect_eq "确认人取自会话（不是提交上来的名字）" "admin" "$(jget "$WORK/resp.json" 'd["data"]["acked_by"]')"
expect_eq "acked_at 不是 null" "true" "$(jget "$WORK/resp.json" 'd["data"]["acked_at"] is not None')"
# 这一条是整个 P13.4 的核心：确认**不改**状态机。
expect_eq "确认之后 state 仍是 firing" "firing" "$(jget "$WORK/resp.json" 'd["data"]["state"]')"
expect_eq "确认不写 resolved_at" "true" "$(jget "$WORK/resp.json" 'd["data"]["resolved_at"] is None')"

R1_ACKED_AT=$(sqlq "SELECT acked_at FROM alert_events WHERE id = $R1_EVENT")
R1_ACKED_BY=$(sqlq "SELECT acked_by FROM alert_events WHERE id = $R1_EVENT")
expect_eq "库里记下了确认人" "admin" "$R1_ACKED_BY"

code=$(apiout "$WORK/admin.jar" "$WORK/events.json" "/api/v1/alerts/events?state=firing")
expect_eq "确认之后仍出现在 state=firing 列表里" "200" "$code"
expect_ge "firing 列表里那条事件还在" 1 \
  "$(jget "$WORK/events.json" "len([e for e in d['data'] if e['id'] == $R1_EVENT])")"

# 第二个管理员再确认：时间与人都不变。改掉的话会变成"最后一个看的人"，那是另一列的事。
code=$(api "$WORK/bob.jar" "$BOB_CSRF" POST "/api/v1/alerts/events/$R1_EVENT/ack")
expect_eq "第二个管理员确认也是 200（幂等）" "200" "$code"
expect_eq "第二次确认保留第一个确认人" "admin" "$(jget "$WORK/resp.json" 'd["data"]["acked_by"]')"
expect_eq "第二次确认不刷新时间" "$(python3 -c 'import sys;print(sys.argv[1])' "$R1_ACKED_AT")" \
  "$(jget "$WORK/resp.json" 'd["data"]["acked_at"]')"
expect_eq "第二次确认之后库里的 acked_by 还是 admin" "admin" \
  "$(sqlq "SELECT acked_by FROM alert_events WHERE id = $R1_EVENT")"
expect_eq "第二次确认之后 acked_at 一秒都没动" "$R1_ACKED_AT" \
  "$(sqlq "SELECT acked_at FROM alert_events WHERE id = $R1_EVENT")"

expect_eq "两次成功确认各落一条审计" "2" \
  "$(sqlq "SELECT COUNT(*) FROM audit_events WHERE action = 'alert.event.ack' AND outcome = 'success'")"
expect_eq "审计的 target 指向这条事件" "alert_event:$R1_EVENT" \
  "$(sqlq "SELECT target FROM audit_events WHERE action = 'alert.event.ack' AND outcome = 'success' ORDER BY id DESC LIMIT 1")"
expect_contains "审计 details 里记着确认人" 'acked_by' \
  "$(sqlq "SELECT redacted_details FROM audit_events WHERE action = 'alert.event.ack' AND outcome = 'success' ORDER BY id DESC LIMIT 1")"
expect_contains "审计 details 里记着当时的状态" 'firing' \
  "$(sqlq "SELECT redacted_details FROM audit_events WHERE action = 'alert.event.ack' AND outcome = 'success' ORDER BY id DESC LIMIT 1")"

# viewer 只读：能看事件列表，看不到投递账（下节）。
code=$(apiout "$WORK/viewer.jar" "$WORK/events-viewer.json" "/api/v1/alerts/events")
expect_eq "viewer 读事件列表 200" "200" "$code"

echo
echo "== 5. C：投递账读路径 =="

code=$(curl -s -o "$WORK/resp.json" -w '%{http_code}' -X GET "$BASE/api/v1/notification-deliveries" \
  -H "Origin: $ORIGIN")
expect_eq "未登录读投递账 401" "401" "$code"

code=$(apiout "$WORK/viewer.jar" "$WORK/deliveries-viewer.json" "/api/v1/notification-deliveries")
expect_eq "viewer 读投递账 403（账里有收件邮箱与正文）" "403" "$code"

code=$(apiout "$WORK/admin.jar" "$WORK/deliveries.json" "/api/v1/notification-deliveries")
expect_eq "管理员读投递账 200" "200" "$code"
expect_ge "两条事件各入了一行 fired 投递" 2 "$(jget "$WORK/deliveries.json" 'len(d["data"])')"

# 行里要能回答"这封发去哪、对应哪个事件"。
expect_eq "投递行带着事件引用" "true" \
  "$(jget "$WORK/deliveries.json" "[x for x in d['data'] if x['event_id'] in ($R1_EVENT, $R2_EVENT)] != []")"
expect_eq "投递行带着事件所属规则名" "true" \
  "$(jget "$WORK/deliveries.json" "any(x['event_rule'].startswith('P13.4') for x in d['data'] if x['event_id'] in ($R1_EVENT, $R2_EVENT))")"
expect_eq "投递行带着通道快照（名字/类型/目标）" "true" \
  "$(jget "$WORK/deliveries.json" "all(x['channel_name'] and x['channel_type'] == 'in_app' for x in d['data'])")"
expect_eq "in_app 的投递写入即 sent" "true" \
  "$(jget "$WORK/deliveries.json" "all(x['state'] == 'sent' for x in d['data'])")"
expect_eq "投递行有 next_attempt_at 字段（已发出=不会再排）" "true" \
  "$(jget "$WORK/deliveries.json" "all('next_attempt_at' in x for x in d['data'])")"

code=$(apiout "$WORK/admin.jar" "$WORK/deliveries-sent.json" "/api/v1/notification-deliveries?state=sent")
expect_eq "state=sent 过滤 200" "200" "$code"
expect_ge "state=sent 里有行" 2 "$(jget "$WORK/deliveries-sent.json" 'len(d["data"])')"
code=$(apiout "$WORK/admin.jar" "$WORK/deliveries-pending.json" "/api/v1/notification-deliveries?state=pending")
expect_eq "state=pending 过滤 200" "200" "$code"
expect_eq "只配了 in_app，没有 pending 的投递" "0" "$(jget "$WORK/deliveries-pending.json" 'len(d["data"])')"
code=$(apiout "$WORK/admin.jar" "$WORK/deliveries-failed.json" "/api/v1/notification-deliveries?state=failed")
expect_eq "state=failed 过滤 200" "200" "$code"
expect_eq "没有失败过的投递" "0" "$(jget "$WORK/deliveries-failed.json" 'len(d["data"])')"

code=$(apiout "$WORK/admin.jar" "$WORK/deliveries-test.json" "/api/v1/notification-deliveries?kind=test")
expect_eq "kind=test 过滤 200" "200" "$code"
expect_eq "没做过测试发送，账里就没有 test 行" "0" "$(jget "$WORK/deliveries-test.json" 'len(d["data"])')"
code=$(apiout "$WORK/admin.jar" "$WORK/deliveries-fired.json" "/api/v1/notification-deliveries?kind=fired")
expect_eq "kind=fired 过滤 200" "200" "$code"
expect_ge "kind=fired 至少有两条" 2 "$(jget "$WORK/deliveries-fired.json" 'len(d["data"])')"

code=$(apiout "$WORK/admin.jar" "$WORK/deliveries-limit.json" "/api/v1/notification-deliveries?limit=1")
expect_eq "limit=1 → 200" "200" "$code"
expect_eq "limit=1 只回一行" "1" "$(jget "$WORK/deliveries-limit.json" 'len(d["data"])')"

code=$(apiout "$WORK/admin.jar" "$WORK/resp.json" "/api/v1/notification-deliveries?limit=0")
expect_eq "limit=0 → 400" "400" "$code"
expect_eq "limit=0 的错误码" "VALIDATION_ERROR" "$(jget "$WORK/resp.json" 'd["error"]["code"]')"
expect_contains "报错说清 limit 要正整数" "正整数" "$(jget "$WORK/resp.json" 'd["error"]["message"]')"

code=$(apiout "$WORK/admin.jar" "$WORK/resp.json" "/api/v1/notification-deliveries?limit=abc")
expect_eq "limit=abc → 400" "400" "$code"

# 超过上限按 200 截断，不报错："多要一点"不是参数错，是上限在生效。
code=$(apiout "$WORK/admin.jar" "$WORK/deliveries-big.json" "/api/v1/notification-deliveries?limit=9999")
expect_eq "limit=9999 → 200（按上限截断）" "200" "$code"
expect_eq "截断后不超过 200 行" "true" "$(jget "$WORK/deliveries-big.json" 'len(d["data"]) <= 200')"

code=$(apiout "$WORK/admin.jar" "$WORK/resp.json" "/api/v1/notification-deliveries?state=weird")
expect_eq "state 取闭集外的值 → 400" "400" "$code"
expect_contains "报错列出允许的 state" "pending" "$(jget "$WORK/resp.json" 'd["error"]["message"]')"
code=$(apiout "$WORK/admin.jar" "$WORK/resp.json" "/api/v1/notification-deliveries?kind=weird")
expect_eq "kind 取闭集外的值 → 400" "400" "$code"
expect_contains "报错列出允许的 kind" "fired" "$(jget "$WORK/resp.json" 'd["error"]["message"]')"

echo
echo "== 6. D：缺失不是 0（真停采集 45 秒） =="

GAP_START=$(now_unix)
stop_seed
note "种入已停，等 45 秒制造真实缺口（$GAP_START 起）"
sleep 45
GAP_END=$(now_unix)
phase high
start_seed
if wait_until "种入恢复后重新有点" 40 '[ "$(seed_samples_after '"$GAP_END"')" -ge 1 ] 2>/dev/null'; then
  pass "种入恢复，缺口之后重新有点"
else
  fail "种入恢复后重新有点" "（$GAP_END 之后没有新桶）"
fi

# 缺口区间内必须**一行都没有**：这是"缺失不是 0"的根，服务端没有可以补的东西。
GAP_ROWS=$(sqlq "SELECT COUNT(*) FROM metric_samples WHERE scope = 'interface' AND target_id = 'accept0' AND metric = 'rx_bytes_per_sec' AND resolution = 'raw' AND ts > $GAP_START AND ts < $GAP_END")
expect_eq "缺口区间里一行样本都没有（不是写了 0）" "0" "$GAP_ROWS"

FROM=$(iso_of $((GAP_START - 30)))
# 窗口右端取 GAP_END（不往后延）：它是过去的时刻，区间里的桶已经定型，不会因为再等
# 几秒就多出一个点。第一版写成 GAP_END+10，那个未来时刻在第二次查询时已经被种入填了
# 一个新桶，missing_points 少 1 —— 看起来像"缺口被回填了"，其实是窗口自己在长。
TO=$(iso_of "$GAP_END")
code=$(apiout "$WORK/admin.jar" "$WORK/gap.json" "/api/v1/system/metrics?scope=interface&target_id=$SEED_TARGET&metric=$SEED_METRIC&from=$FROM&to=$TO&step=5")
expect_eq "查含缺口的窗口 200" "200" "$code"
expect_eq "返回一条序列" "1" "$(jget "$WORK/gap.json" 'len(d["data"]["series"])')"
expect_ge "missing_points 数得出缺的桶（≥ 6）" 6 "$(jget "$WORK/gap.json" 'd["data"]["series"][0]["missing_points"]')"
expect_eq "缺口区间里没有任何点（缺失不被补成 0）" "0" \
  "$(python3 -c 'import json,sys,datetime
d=json.load(open(sys.argv[1], encoding="utf-8"))
g1,g2=int(sys.argv[2]),int(sys.argv[3])
def ts(p):
    return int(datetime.datetime.strptime(p["timestamp"][:19], "%Y-%m-%dT%H:%M:%S")
               .replace(tzinfo=datetime.timezone.utc).timestamp())
print(len([p for p in d["data"]["series"][0]["points"] if g1 < ts(p) < g2]))' "$WORK/gap.json" "$GAP_START" "$GAP_END")"
# "断开"要看得到缺口**两端**的点，所以这个窗口右端要伸到缺口之后（+30 秒）。它和上面
# 那个窗口是两个问题：上面问"缺了几个桶"（窗口必须定型），这里问"图上连不连"（窗口必须
# 含住断点的两侧）。用一个窗口同时问两件事，就会像第一版那样只有一个能成立。
TO_B=$(iso_of $((GAP_END + 30)))
code=$(apiout "$WORK/admin.jar" "$WORK/gap-break.json" "/api/v1/system/metrics?scope=interface&target_id=$SEED_TARGET&metric=$SEED_METRIC&from=$FROM&to=$TO_B&step=5")
expect_eq "查含缺口两端的窗口 200" "200" "$code"
expect_eq "相邻两点真的断开（最大间隔 ≥ 20 秒）" "true" \
  "$(python3 -c 'import json,sys,datetime
d=json.load(open(sys.argv[1], encoding="utf-8"))
def ts(p):
    return int(datetime.datetime.strptime(p["timestamp"][:19], "%Y-%m-%dT%H:%M:%S")
               .replace(tzinfo=datetime.timezone.utc).timestamp())
pts=sorted(d["data"]["series"][0]["points"], key=ts)
gap=max([ts(b)-ts(a) for a,b in zip(pts, pts[1:])] or [0])
print("true" if gap >= 20 else "false")' "$WORK/gap-break.json")"
expect_eq "观察到的首尾都不是 null" "true" \
  "$(jget "$WORK/gap.json" 'd["data"]["observed_from"] is not None and d["data"]["observed_to"] is not None')"

# 再查一次：缺口**不被回填**。历史是历史，一段时间没采到就永远是空的。
GAP_MISSING_1=$(jget "$WORK/gap.json" 'd["data"]["series"][0]["missing_points"]')
sleep 10
code=$(apiout "$WORK/admin.jar" "$WORK/gap2.json" "/api/v1/system/metrics?scope=interface&target_id=$SEED_TARGET&metric=$SEED_METRIC&from=$FROM&to=$TO&step=5")
expect_eq "第二次查同一个窗口 200" "200" "$code"
expect_eq "缺口不被回填（missing_points 不变）" "$GAP_MISSING_1" \
  "$(jget "$WORK/gap2.json" 'd["data"]["series"][0]["missing_points"]')"
# 同样的过滤条件必须带全（metric + resolution='raw'）：聚合出来的 minute 桶覆盖了缺口
# 的一段时会落在缺口区间里，那是**聚合的正常产物**（它只代表那几分钟里真有样本的部分），
# 不是"缺口被填了"。第一版漏了这两个条件，于是把自己的分钟聚合读成了回填。
expect_eq "缺口区间仍然一行原始样本都没有" "0" \
  "$(sqlq "SELECT COUNT(*) FROM metric_samples WHERE scope = 'interface' AND target_id = 'accept0' AND metric = 'rx_bytes_per_sec' AND resolution = 'raw' AND ts > $GAP_START AND ts < $GAP_END")"

echo
echo "== 7. B 续：已恢复的事件也能确认 =="

phase low
if wait_until "R2 的事件恢复" 150 '[ "$(event_state_of '"$R2"')" = resolved ]'; then
  pass "种入转低之后 R2 的事件恢复（resolved）"
else
  fail "R2 的事件恢复" "state=$(event_state_of "$R2")"
fi
expect_eq "恢复事件写了 resolved_at" "1" "$(sqlq "SELECT COUNT(*) FROM alert_events WHERE id = $R2_EVENT AND resolved_at IS NOT NULL")"
expect_eq "恢复事件在确认之前没人确认过" "0" "$(sqlq "SELECT COUNT(*) FROM alert_events WHERE id = $R2_EVENT AND acked_at IS NOT NULL")"

code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "/api/v1/alerts/events/$R2_EVENT/ack")
expect_eq "已恢复的事件确认 200（我看到了，在恢复之后仍然有意义）" "200" "$code"
expect_eq "确认之后 state 仍是 resolved" "resolved" "$(jget "$WORK/resp.json" 'd["data"]["state"]')"
expect_eq "确认写了确认人" "admin" "$(jget "$WORK/resp.json" 'd["data"]["acked_by"]')"
expect_eq "确认不碰 resolved_at" "1" "$(sqlq "SELECT COUNT(*) FROM alert_events WHERE id = $R2_EVENT AND resolved_at IS NOT NULL")"

# 恢复也应当入账（一条被告知开始的告警，要有一条"它结束了"来收尾）。
DELIVERED_RESOLVED=$(sqlq "SELECT COUNT(*) FROM notification_deliveries WHERE event_id = $R2_EVENT AND kind = 'resolved'")
expect_eq "恢复事件同样入了一行投递（kind=resolved）" "1" "$DELIVERED_RESOLVED"

echo
echo "== 8. E：未开启历史时说得清是未开启 =="

stop_web
python3 - "$SYS_CFG" <<'PY'
import json, sys
path = sys.argv[1]
with open(path, encoding="utf-8") as f:
    cfg = json.load(f)
cfg["web"]["metrics"]["enabled"] = False
with open(path, "w", encoding="utf-8") as f:
    json.dump(cfg, f, ensure_ascii=False, indent=2)
PY
if start_web; then pass "以 metrics.enabled=false 重启 Web"; else fail "以 metrics.enabled=false 重启 Web" "$(tail -5 "$WEB_LOG" 2>/dev/null)"; fi

# 两种"没在记"必须分开说，这是 P13.4 口径里最容易搞混的一处：
#   status.enabled=false     → 这个进程**没有库**，一条历史都答不了（演示模式/未装配）
#   status.enabled=true 且 collecting=false → 库在、旧数据查得到，只是不再写新的
# 配置关掉采集属于后者。第一版按前者写，把一份正常工作的产品判成了错。
code=$(apiout "$WORK/admin.jar" "$WORK/status-off.json" "/api/v1/system/metrics/status")
expect_eq "关掉采集后读状态 200" "200" "$code"
expect_eq "库还在，所以 enabled 仍是 true（能回答历史问题）" "true" \
  "$(jget "$WORK/status-off.json" 'd["data"]["enabled"]')"
expect_eq "采集循环没有在跑" "false" "$(jget "$WORK/status-off.json" 'd["data"]["collecting"]')"
expect_eq "status 说明了为什么不在记" "true" \
  "$(jget "$WORK/status-off.json" 'bool(d["data"].get("reason"))')"
expect_contains "原因指向配置里那一项" "web.metrics.enabled" \
  "$(jget "$WORK/status-off.json" 'd["data"].get("reason") or ""')"

code=$(apiout "$WORK/admin.jar" "$WORK/metrics-off.json" "/api/v1/system/metrics?scope=host")
expect_eq "关掉采集后查历史仍 200（不是报错）" "200" "$code"
expect_ge "旧数据照样查得到（关的是采集，不是历史）" 1 \
  "$(jget "$WORK/metrics-off.json" 'len(d["data"]["series"] or [])')"

# 真正要钉的是"不再写新的"：等 10 秒，host 行数一个都不许涨。
HOST_ROWS_BEFORE=$(host_samples)
sleep 10
HOST_ROWS_AFTER=$(host_samples)
expect_eq "关掉采集后 10 秒内没有新样本落库" "$HOST_ROWS_BEFORE" "$HOST_ROWS_AFTER"
expect_eq "重启后的进程从没写过样本（last_sample_at 是 null）" "true" \
  "$(apiout "$WORK/admin.jar" "$WORK/status-off2.json" "/api/v1/system/metrics/status" >/dev/null; jget "$WORK/status-off2.json" 'd["data"]["last_sample_at"] is None')"

echo
echo "== 9. F：卫生 =="

# grep -c 在没有匹配时既印 0 又退 1：直接取值会把 "0\n0" 拼进断言。
PANIC_AGENT=$(grep -ci panic "$AGENT_LOG" 2>/dev/null | head -1)
PANIC_WEB=$(grep -ci panic "$WEB_LOG" 2>/dev/null | head -1)
[ -n "$PANIC_AGENT" ] || PANIC_AGENT=0
[ -n "$PANIC_WEB" ] || PANIC_WEB=0
expect_eq "代理日志没有 panic" "0" "$PANIC_AGENT"
expect_eq "Web 日志没有 panic" "0" "$PANIC_WEB"
expect_eq "确认动作没有写系统（本脚本没有产生任何 operation）" "1" \
  "$(sqlq "SELECT COUNT(*) = 0 FROM jobs WHERE action LIKE '%ack%'")"

echo
echo "=========================== 汇总 ==========================="
printf '通过 %d 项，失败 %d 项\n' "$PASSED" "$FAILED"
if [ -n "$FAILED_NAMES" ]; then printf '失败项:%s\n' "$FAILED_NAMES"; fi
echo "============================================================"
[ "$FAILED" -eq 0 ] || exit 1
exit 0
