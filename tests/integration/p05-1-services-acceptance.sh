#!/bin/sh
# P05.1 Linux 验收脚本：服务列表/详情/保护名单，以及通过预检与任务执行的启动/停止/
# 重启/开机启动设置，在真实 systemd 上是否与契约一致。
#
# 运行位置：Ubuntu 22.04/24.04 验收 VM 内，**必须以 root 运行**（代理只能在 root 下启动，
# 并用 upanel-web 身份去查 socket）。不依赖 Go 工具链：用宿主交叉编译好的 Linux 二进制。
#
# 用法：
#   sudo BIN_DIR=/tmp/upanel-bin sh tests/integration/p05-1-services-acceptance.sh
#
# 退出码：0 全部通过；1 有失败项；2 环境不满足（非 root）。
#
# 覆盖的验收点（STAGES §7 P05.1、CONTRACTS §5/§10.1、PLAN §14）：
#   1. Web 非 root、代理 root、socket 归 socket_group（与 P04 同一条边界）
#   2. GET /services 与 /services/:unit 的字段、闭集、null 语义与契约一致
#   3. 列表是缓存快照（10 秒），详情与预检现场读取；snapshot_version 不含时间噪声
#   4. 单元集合与 systemctl 枚举结果对照，状态与 is-active/is-enabled 逐项对照
#   5. 保护名单：journald/logind/udevd/ssh 等受保护，capabilities 为空，预检拒绝
#   6. 真实启停/重启/开机启动：预检 → 任务 → 读回验证，并与 systemctl 现场对照
#   7. 「systemctl 返回 0」不算成功：ExecStart 失败的服务必须让任务失败
#   8. 单元名与参数边界：不存在的单元不当命令执行，注入式参数被拒绝
#   9. viewer 能看状态、不能写；审计里有对应的 preflight/job.submit 记录
#
# 明确不覆盖（NOT RUN，原因写在这里，不在脚本里假装通过）：
#   - 浏览器渲染：VM 上没有浏览器。页面级证据在宿主上用真实载荷回放
#     （web/src/components/realdata.test.ts 读 web/src/fixtures/p05-1-vm/），
#     这里只保证「前端拿到的数据是真的」。
#   - 执行后未收敛（命令成功但状态 15 秒内没变）：这条路径要人为构造一个
#     “systemctl 成功但状态不变”的服务，正常 systemd 上做不到，仅由
#     internal/services/collector_test.go 的 fake 覆盖（标 NOT RUN）。
#   - 面板自身（upanel-web/upanel-agent）作为 systemd 单元的情形：验收环境里它们是
#     直接启动的进程，不是 unit；它们的保护判定由单元测试覆盖。

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
VALIDATE="$HERE/p05-1-payload-validate.py"

WORK=${WORK:-/var/tmp/upanel-accept-p05-1}
PAYLOADS=$WORK/payloads
RUN_DIR=/run/upanel
SOCK=$RUN_DIR/agent.sock
SYS_CFG=/etc/upanel/upanel.json
AGENT_DB_DIR=/var/lib/upanel-agent
AGENT_DB=$AGENT_DB_DIR/agent.db
KEY_FILE=/etc/upanel/agent.key
WEB_DB_DIR=$WORK/webdata
WEB_DB=$WEB_DB_DIR/upanel.db
PORT=${PORT:-19449}
BASE="http://127.0.0.1:$PORT"
ORIGIN="$BASE"
WEB_USER=upanel-web
PASSWORD='vm-correct-horse-9'
VIEWER_PASSWORD='vm-correct-horse-8'

UNIT=upanel-accept.service
UNIT_DIR=/etc/systemd/system
FAIL_UNIT=upanel-accept-fail.service

AGENT_LOG=$WORK/logs/agent.log
WEB_LOG=$WORK/logs/web.log
AGENT_PID=""
WEB_PID=""

PREFLIGHT_PATH=/api/v1/operations/preflight
JOBS_PATH=/api/v1/jobs

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

cleanup() {
  stop_web
  stop_agent
  # 不留任何验收服务：停掉、取消开机启动、删掉单元文件。
  systemctl stop "$UNIT" "$FAIL_UNIT" >/dev/null 2>&1
  systemctl disable "$UNIT" >/dev/null 2>&1
  rm -f "$UNIT_DIR/$UNIT" "$UNIT_DIR/$FAIL_UNIT"
  systemctl daemon-reload >/dev/null 2>&1
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

csrf_of() { jget "$1" 'd["data"]["csrf_token"]'; }

# api <jar> <csrf> <方法> <路径> [body] [idempotency-key]：响应写到 $WORK/resp.json。
api() {
  jar=$1; csrf=$2; method=$3; path=$4; body=${5:-}; key=${6:-}
  if [ -n "$body" ]; then
    printf '%s' "$body" >"$WORK/req.json"
    if [ -n "$key" ]; then
      curl -s -b "$jar" -o "$WORK/resp.json" -w '%{http_code}' -X "$method" "$BASE$path" \
        -H 'Content-Type: application/json' -H "Origin: $ORIGIN" -H "X-CSRF-Token: $csrf" \
        -H "Idempotency-Key: $key" -d @"$WORK/req.json"
    else
      curl -s -b "$jar" -o "$WORK/resp.json" -w '%{http_code}' -X "$method" "$BASE$path" \
        -H 'Content-Type: application/json' -H "Origin: $ORIGIN" -H "X-CSRF-Token: $csrf" \
        -d @"$WORK/req.json"
    fi
  else
    curl -s -b "$jar" -o "$WORK/resp.json" -w '%{http_code}' -X "$method" "$BASE$path" \
      -H "Origin: $ORIGIN" -H "X-CSRF-Token: $csrf"
  fi
}

# wait_terminal_job <job_id> <超时秒>：轮询到终态，回显 status。
wait_terminal_job() {
  _jid=$1; _limit=$(( ${2:-40} * 2 )); _i=0; _st=unknown
  while [ "$_i" -lt "$_limit" ]; do
    api "$JAR" "$CSRF" GET "$JOBS_PATH/$_jid" >/dev/null
    _st=$(jget "$WORK/resp.json" 'd["data"]["status"]')
    case $_st in
      succeeded|failed|canceled|interrupted) printf '%s' "$_st"; return 0 ;;
    esac
    sleep 0.5
    _i=$((_i + 1))
  done
  printf '%s' "${_st:-unknown}"
  return 1
}

# preflight <载荷名> <请求体>：把响应存成 preflight-<名>.json，回显 HTTP 状态码。
preflight() {
  _code=$(api "$JAR" "$CSRF" POST "$PREFLIGHT_PATH" "$2")
  cp "$WORK/resp.json" "$PAYLOADS/preflight-$1.json" 2>/dev/null
  printf '%s' "$_code"
}

# submit_job <载荷名> <preflight_id>：把响应存成 job-submit-<名>.json，回显 HTTP 状态码。
submit_job() {
  _code=$(api "$JAR" "$CSRF" POST "$JOBS_PATH" "{\"preflight_id\":\"$2\"}" "p05-1-$1-$$")
  cp "$WORK/resp.json" "$PAYLOADS/job-submit-$1.json" 2>/dev/null
  printf '%s' "$_code"
}

# run_action <载荷名> <unit> <operation>：预检 → 提交 → 等到终态，回显任务状态。
# 任何一步的 HTTP 不对，回显 http-<码> 并返回 1（调用处的 expect_eq 会把它标成失败）。
run_action() {
  _body=$(printf '{"action":"service.control","params":{"unit":"%s","operation":"%s"}}' "$2" "$3")
  _code=$(preflight "$1" "$_body")
  [ "$_code" = 200 ] || { printf 'http-%s' "$_code"; return 1; }
  _pf=$(jget "$PAYLOADS/preflight-$1.json" 'd["data"]["preflight_id"]')
  _code=$(submit_job "$1" "$_pf")
  [ "$_code" = 202 ] || { printf 'http-%s' "$_code"; return 1; }
  _jid=$(jget "$PAYLOADS/job-submit-$1.json" 'd["data"]["job_id"]')
  _st=$(wait_terminal_job "$_jid" 40)
  api "$JAR" "$CSRF" GET "$JOBS_PATH/$_jid" >/dev/null
  cp "$WORK/resp.json" "$PAYLOADS/job-$1.json" 2>/dev/null
  printf '%s' "$_st"
}

# live_state <unit>：现场读 is-active / is-enabled（真值来源，不是面板说的）。
live_active() { systemctl is-active "$1" 2>/dev/null || true; }
live_enabled() { systemctl is-enabled "$1" 2>/dev/null || true; }

echo "== 0. 环境 =="
if [ "$(id -u)" -eq 0 ]; then pass "验收脚本以 root 运行"; else fail "验收脚本以 root 运行" "UID $(id -u)"; fi
for b in "$AGENT" "$WEB" "$CTL"; do
  if [ -x "$b" ]; then pass "二进制存在且可执行 $(basename "$b")"; else fail "二进制存在且可执行 $(basename "$b")" "$b"; fi
done
if [ -f "$VALIDATE" ]; then pass "载荷校验程序存在"; else fail "载荷校验程序存在" "$VALIDATE"; fi
if command -v python3 >/dev/null 2>&1; then pass "python3 可用"; else fail "python3 可用" "缺少 python3"; fi
if [ -d /run/systemd/system ] && command -v systemctl >/dev/null 2>&1; then
  pass "本机由 systemd 管理（$(systemctl --version | head -1)）"
else
  fail "本机由 systemd 管理" "没有 /run/systemd/system 或 systemctl，无法验收服务模块"
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
echo "== 1. 准备无害验收单元 =="
# 这两个单元只存在于验收 VM：一个能正常启停（用来验真实动作），一个 ExecStart 必然失败
# （用来验「systemctl 返回 0 才算成功」这条被拒绝）。都不带任何与系统的耦合。
cat >"$UNIT_DIR/$UNIT" <<'UNIT'
[Unit]
Description=uPanel acceptance target service (test VM only)
After=network.target

[Service]
Type=simple
ExecStart=/bin/sleep 1000000

[Install]
WantedBy=multi-user.target
UNIT
cat >"$UNIT_DIR/$FAIL_UNIT" <<'UNIT'
[Unit]
Description=uPanel acceptance failing service (test VM only)

[Service]
Type=oneshot
ExecStart=/bin/false
UNIT
chmod 0644 "$UNIT_DIR/$UNIT" "$UNIT_DIR/$FAIL_UNIT"
if systemctl daemon-reload >/dev/null 2>&1; then
  pass "写入验收单元并 daemon-reload（$UNIT、$FAIL_UNIT）"
else
  fail "写入验收单元并 daemon-reload" "systemctl daemon-reload 失败"
fi
systemctl stop "$UNIT" >/dev/null 2>&1
systemctl disable "$UNIT" >/dev/null 2>&1
systemctl reset-failed "$FAIL_UNIT" >/dev/null 2>&1
expect_eq "起始状态 $UNIT 是 inactive" "inactive" "$(live_active "$UNIT")"
expect_eq "起始状态 $UNIT 是 disabled" "disabled" "$(live_enabled "$UNIT")"

echo
echo "== 2. 配置与启动（代理 root、Web 非 root） =="
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
    "services": {}
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

# 真实动作只能通过生产登记表进来：验收模式（-acceptance-actions）不登记 service.control。
if grep -q '已启用验收专用动作表' "$AGENT_LOG" 2>/dev/null; then
  fail "代理没有启用验收专用动作表" "日志里出现验收动作表，真实动作会被跳过"
else
  pass "代理使用生产动作登记表（service.control 可用）"
fi
if grep -q '"已启用服务状态采集"' "$AGENT_LOG" 2>/dev/null; then
  pass "代理已启用服务状态采集"
else
  note "代理日志里没有找到服务采集启用行，继续按接口结果判断"
fi

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

# 只读账号：验证「能看状态、不能写」。
api "$JAR" "$CSRF" POST /api/v1/users \
  "{\"username\":\"viewer\",\"display_name\":\"只读\",\"role\":\"viewer\",\"initial_password\":\"$VIEWER_PASSWORD\"}" \
  >"$WORK/viewer-code.txt"
expect_eq "创建 viewer 账号" "201" "$(cat "$WORK/viewer-code.txt")"
chown_web_db
VJAR=$WORK/viewer.jar
expect_eq "viewer 登录" "200" "$(login viewer "$VIEWER_PASSWORD" "$VJAR" login-viewer)"
VCSRF=$(csrf_of "$WORK/login-viewer.json")

echo
echo "== 3. GET /services（列表快照） =="
# 采集器是独立 goroutine，代理起来后要等第一份快照落地；否则拿到的是
# 「服务状态尚未完成第一次采集」——那也是一条正确的失败快照，但不是这里要验的东西。
waited=0
while [ "$waited" -lt 40 ]; do
  code=$(api "$JAR" "$CSRF" GET /api/v1/services)
  st=$(jget "$WORK/resp.json" 'd["data"]["status"]')
  n=$(jget "$WORK/resp.json" 'len(d["data"]["units"])')
  case ${n:-} in ''|*[!0-9]*) n=0 ;; esac
  if [ "$code" = 200 ] && [ "$st" = ok ] && [ "$n" -gt 0 ]; then break; fi
  sleep 1
  waited=$((waited + 1))
done
cp "$WORK/resp.json" "$PAYLOADS/services.json"
expect_eq "GET /services" "200" "${code:-}"
expect_eq "列表 status=ok" "ok" "$(jget "$PAYLOADS/services.json" 'd["data"]["status"]')"
UNITS=$(jget "$PAYLOADS/services.json" 'len(d["data"]["units"])')
if [ "${UNITS:-0}" -gt 0 ]; then
  pass "列表非空（$UNITS 个单元）"
else
  fail "列表非空" "0 个单元：真实 Ubuntu 上不可能"
fi
expect_eq "interval_ms 是 10000" "10000" "$(jget "$PAYLOADS/services.json" 'd["data"]["interval_ms"]')"
expect_eq "truncated=false" "False" "$(jget "$PAYLOADS/services.json" 'd["data"]["truncated"]')"
expect_eq "request_id 非空" "yes" \
  "$([ -n "$(jget "$PAYLOADS/services.json" 'd["request_id"]')" ] && echo yes || echo no)"

# 列表是快照：连着两次读，collected_at 与 snapshot_version 都不该变
# （变了说明每次请求都在重采集，而不是读那份 10 秒快照）。
# 采集恰好在这两次读之间推进时 collected_at 会变—那是节奏而不是缺陷，所以最多重试三次。
SNAPSHOT_STABLE=no
tries=0
while [ "$tries" -lt 3 ]; do
  api "$JAR" "$CSRF" GET /api/v1/services >/dev/null
  A_COLLECTED=$(jget "$WORK/resp.json" 'd["data"]["collected_at"]')
  A_VERSION=$(jget "$WORK/resp.json" 'd["data"]["snapshot_version"]')
  api "$JAR" "$CSRF" GET /api/v1/services >/dev/null
  B_COLLECTED=$(jget "$WORK/resp.json" 'd["data"]["collected_at"]')
  B_VERSION=$(jget "$WORK/resp.json" 'd["data"]["snapshot_version"]')
  if [ "$A_COLLECTED" = "$B_COLLECTED" ] && [ -n "$A_COLLECTED" ]; then
    SNAPSHOT_STABLE=yes
    break
  fi
  tries=$((tries + 1))
done
expect_eq "连续两次读取 collected_at 不变（读的是快照）" "yes" "$SNAPSHOT_STABLE"
if [ "$SNAPSHOT_STABLE" = yes ]; then
  expect_eq "连续两次读取 snapshot_version 不变" "$A_VERSION" "$B_VERSION"
fi

# 等到下一次采集：collected_at 必须前进（10 秒节奏是真的在跑）。
# snapshot_version 是否“只由状态内容决定”，由载荷校验程序按契约里的算法重算一遍来验，
# 不靠“两次读之间别的服务没变”这种假设——那正是验收脚本最容易写出的错误前提。
BEFORE_COLLECTED=$(jget "$PAYLOADS/services.json" 'd["data"]["collected_at"]')
waited=0
ADVANCED=no
while [ "$waited" -lt 25 ]; do
  sleep 2
  waited=$((waited + 2))
  api "$JAR" "$CSRF" GET /api/v1/services >/dev/null
  cur=$(jget "$WORK/resp.json" 'd["data"]["collected_at"]')
  if [ "$cur" != "$BEFORE_COLLECTED" ]; then
    ADVANCED=yes
    break
  fi
done
if [ "$ADVANCED" = yes ]; then
  pass "采集在 ${waited}s 内推进了 collected_at（10 秒节奏）"
else
  fail "采集在 25s 内推进了 collected_at" "collected_at 一直是 $BEFORE_COLLECTED"
fi
cp "$WORK/resp.json" "$PAYLOADS/services-after.json"

echo
echo "== 4. 单元集合与 systemctl 枚举对照 =="
# 单元文件清单保留原始三列：第二列（state）里有没有 alias，是“哪些名字是别名”的唯一来源。
systemctl list-unit-files --type=service --no-legend --plain 2>/dev/null \
  >"$PAYLOADS/systemctl-unit-files-raw.txt"
awk '$2 == "alias" { print $1 }' "$PAYLOADS/systemctl-unit-files-raw.txt" | sort -u \
  >"$PAYLOADS/systemctl-aliases.txt"
awk '{ print $1 }' "$PAYLOADS/systemctl-unit-files-raw.txt" \
  | grep -E '^[A-Za-z0-9][A-Za-z0-9:_.@-]*\.service$' \
  | grep -vxF -f "$PAYLOADS/systemctl-aliases.txt" >"$PAYLOADS/systemctl-unit-files.txt"
systemctl list-units --type=service --all --no-legend --plain 2>/dev/null | awk '{print $1}' \
  | grep -E '^[A-Za-z0-9][A-Za-z0-9:_.@-]*\.service$' >"$PAYLOADS/systemctl-units.txt"
cat "$PAYLOADS/systemctl-unit-files.txt" "$PAYLOADS/systemctl-units.txt" | sort -u >"$PAYLOADS/systemctl-union-all.txt"
# 模板单元（`foo@.service`，没有实例）与别名单元（UnitFileState=alias）不进入接口列表
# （CONTRACTS.md 10.1）：前者会让 systemctl show 中断整次调用，后者是另一个单元的名字。
# 对照集合也要去掉它们，留下的实例单元与规范名仍然要求一个不少。
grep -vE '@\.service$' "$PAYLOADS/systemctl-union-all.txt" >"$PAYLOADS/systemctl-union.txt"
TEMPLATES=$(grep -cE '@\.service$' "$PAYLOADS/systemctl-union-all.txt" | tr -d ' ')
ALIASES=$(wc -l <"$PAYLOADS/systemctl-aliases.txt" | tr -d ' ')
note "本机有 $TEMPLATES 个模板单元、$ALIASES 个别名单元（按契约都不出现在列表里）"
SYS_COUNT=$(wc -l <"$PAYLOADS/systemctl-union.txt" | tr -d ' ')
note "systemctl 枚举到 $SYS_COUNT 个服务单元，接口返回 $UNITS 个"
# 这里的下限不是随便的 20：必须与真实枚举量级一致。7 个单元的“正常”列表正是
# P05.1 首轮踩到的坑（模板名让 systemctl show 中断在中途），当时只差一点就没被发现。
if [ "${UNITS:-0}" -ge $((SYS_COUNT - 5)) ] && [ "${UNITS:-0}" -le $((SYS_COUNT + 5)) ]; then
  pass "接口单元数与 systemctl 枚举量级一致（$UNITS vs $SYS_COUNT）"
else
  fail "接口单元数与 systemctl 枚举量级一致" "接口 $UNITS 个，systemctl $SYS_COUNT 个（差 5 个以上）"
fi
if [ "${UNITS:-0}" -ge 20 ]; then
  pass "接口单元数与真实机器规模相当（≥20）"
else
  fail "接口单元数与真实机器规模相当" "只有 $UNITS 个"
fi

# 别名与模板的负向断言：既要“不出现”，也要“不接受为操作目标”。
# 优先挑一个**不是模板**的别名（dbus-org.* 这类），此时 404 的理由是“没有这个单元”，
# 而不是“模板需要实例”。
ALIAS_SAMPLE=$(grep -vE '@\.service$' "$PAYLOADS/systemctl-aliases.txt" 2>/dev/null | head -1)
if [ -z "$ALIAS_SAMPLE" ]; then
  ALIAS_SAMPLE=$(head -1 "$PAYLOADS/systemctl-aliases.txt" 2>/dev/null)
fi
if [ -n "$ALIAS_SAMPLE" ]; then
  expect_eq "别名单元 $ALIAS_SAMPLE 不在列表里" "False" \
    "$(jget "$PAYLOADS/services.json" "any(u['unit'] == '$ALIAS_SAMPLE' for u in d['data']['units'])")"
  code=$(api "$JAR" "$CSRF" GET "/api/v1/services/$ALIAS_SAMPLE")
  cp "$WORK/resp.json" "$PAYLOADS/alias-detail.json"
  expect_eq "别名单元详情返回 404（请用规范名）" "404" "$code"
  # 别名被剔除不等于单元消失了：它的规范名（systemctl show 给的 Id）必须在列表里。
  CANON=$(systemctl show --no-pager -p Id -- "$ALIAS_SAMPLE" 2>/dev/null | sed -n 's/^Id=//p')
  if [ -n "$CANON" ]; then
    expect_eq "别名的规范名 $CANON 在列表里" "True" \
      "$(jget "$PAYLOADS/services.json" "any(u['unit'] == '$CANON' for u in d['data']['units'])")"
  fi
else
  notrun "别名单元" "本机没有 UnitFileState=alias 的服务单元"
fi

TPL_SAMPLE=$(grep -E '@\.service$' "$PAYLOADS/systemctl-union-all.txt" 2>/dev/null | head -1)
if [ -n "$TPL_SAMPLE" ]; then
  expect_eq "模板单元 $TPL_SAMPLE 不在列表里" "False" \
    "$(jget "$PAYLOADS/services.json" "any(u['unit'] == '$TPL_SAMPLE' for u in d['data']['units'])")"
  code=$(api "$JAR" "$CSRF" GET "/api/v1/services/$TPL_SAMPLE")
  cp "$WORK/resp.json" "$PAYLOADS/template-detail.json"
  expect_eq "模板单元详情返回 404" "404" "$code"
  if jget "$PAYLOADS/template-detail.json" 'd["error"]["message"]' | grep -q '实例'; then
    pass "模板单元的 404 说明“需要指定实例”"
  else
    fail "模板单元的 404 说明“需要指定实例”" "$(jget "$PAYLOADS/template-detail.json" 'd["error"]["message"]')"
  fi
else
  notrun "模板单元" "本机没有模板服务单元"
fi

echo
echo "== 5. 详情、非法单元名与路径穿越 =="
code=$(api "$JAR" "$CSRF" GET "/api/v1/services/$UNIT")
cp "$WORK/resp.json" "$PAYLOADS/unit-target.json"
expect_eq "GET /services/$UNIT" "200" "$code"
expect_eq "详情里的单元名一致" "$UNIT" "$(jget "$PAYLOADS/unit-target.json" 'd["data"]["unit"]["unit"]')"

for bad in '..%2F..%2Fetc%2Fpasswd' 'cron' '-o.service' 'a%20b.service' 'x%3By.service'; do
  code=$(api "$JAR" "$CSRF" GET "/api/v1/services/$bad")
  cp "$WORK/resp.json" "$PAYLOADS/invalid.json"
  case $code in
    400|404) pass "非法单元名被拒绝：$bad（HTTP $code）" ;;
    *) fail "非法单元名被拒绝：$bad" "HTTP $code" ;;
  esac
  if grep -q 'root:' "$PAYLOADS/invalid.json" 2>/dev/null; then
    fail "非法单元名响应里不含系统文件内容：$bad" "响应里出现 root:"
  else
    pass "非法单元名响应里不含系统文件内容：$bad"
  fi
done

code=$(api "$JAR" "$CSRF" GET /api/v1/services/upanel-doesnotexist.service)
cp "$WORK/resp.json" "$PAYLOADS/notfound.json"
expect_eq "不存在的单元返回 404" "404" "$code"
expect_eq "不存在的单元错误码" "NOT_FOUND" "$(jget "$PAYLOADS/notfound.json" 'd["error"]["code"]')"

echo
echo "== 6. 保护名单 =="
# 这些单元在 Ubuntu 22.04/24.04 上都存在；ssh 因为要 SSH 进 VM，必然存在。
for u in systemd-journald.service systemd-logind.service systemd-udevd.service ssh.service; do
  code=$(api "$JAR" "$CSRF" GET "/api/v1/services/$u")
  if [ "$code" != 200 ]; then
    notrun "受保护单元 $u" "本机没有这个单元（HTTP $code），跳过"
    continue
  fi
  cp "$WORK/resp.json" "$PAYLOADS/protected-$u.json"
  expect_eq "$u protection.protected" "True" "$(jget "$WORK/resp.json" 'd["data"]["unit"]["protection"]["protected"]')"
  expect_eq "$u capabilities 为空" "[]" "$(jget "$WORK/resp.json" 'd["data"]["unit"]["capabilities"]')"
  if [ -n "$(jget "$WORK/resp.json" 'd["data"]["unit"]["protection"]["category"]')" ]; then
    pass "$u 有保护类别"
  else
    fail "$u 有保护类别" "category 为空"
  fi
  if [ -n "$(jget "$WORK/resp.json" 'd["data"]["unit"]["protection"]["reasons"]')" ]; then
    pass "$u 有保护理由（给管理员看的中文说明）"
  else
    fail "$u 有保护理由" "reasons 为空"
  fi
done

ACTIVE_BEFORE=$(live_active systemd-journald.service)
# 受保护单元只做预检，**不提交任务**：如果哪天保护判定失效，这里也不能真的去停日志服务。
code=$(preflight protected \
  '{"action":"service.control","params":{"unit":"systemd-journald.service","operation":"stop"}}')
expect_eq "受保护单元的预检返回 200（结论在 allowed 里）" "200" "$code"
expect_eq "受保护单元的预检 allowed=false" "False" \
  "$(jget "$PAYLOADS/preflight-protected.json" 'd["data"]["allowed"]')"
expect_eq "受保护单元的阻塞码" "SERVICE_PROTECTED" \
  "$(jget "$PAYLOADS/preflight-protected.json" 'd["data"]["blockers"][0]["code"]')"
expect_eq "受保护单元在现场没有被改动" "$ACTIVE_BEFORE" "$(live_active systemd-journald.service)"

code=$(preflight notfound '{"action":"service.control","params":{"unit":"upanel-doesnotexist.service","operation":"start"}}')
expect_eq "不存在的单元预检返回 200（结论在里面）" "200" "$code"
expect_eq "不存在的单元预检 allowed=false" "False" "$(jget "$PAYLOADS/preflight-notfound.json" 'd["data"]["allowed"]')"
expect_eq "不存在的单元阻塞码是 SERVICE_NOT_FOUND" "SERVICE_NOT_FOUND" \
  "$(jget "$PAYLOADS/preflight-notfound.json" 'd["data"]["blockers"][0]["code"]')"
if [ -e "$UNIT_DIR/upanel-doesnotexist.service" ]; then
  fail "不存在的单元没有被当成命令执行" "居然出现了单元文件"
else
  pass "不存在的单元没有被当成命令执行（没有生成任何单元文件）"
fi

# 模板单元：预检必须给 SERVICE_STATE_CONFLICT（“名字不完整”），而不是 NOT_FOUND，
# 也不能真的把模板名交给 systemctl。选一个不是别名的模板名来测。
TPL_PREFLIGHT=$(grep -E '@\.service$' "$PAYLOADS/systemctl-union-all.txt" 2>/dev/null \
  | grep -vxF -f "$PAYLOADS/systemctl-aliases.txt" | head -1)
if [ -n "$TPL_PREFLIGHT" ]; then
  code=$(preflight template '{"action":"service.control","params":{"unit":"'"$TPL_PREFLIGHT"'","operation":"start"}}')
  expect_eq "模板单元 $TPL_PREFLIGHT 预检返回 200（结论在 allowed 里）" "200" "$code"
  expect_eq "模板单元预检 allowed=false" "False" \
    "$(jget "$PAYLOADS/preflight-template.json" 'd["data"]["allowed"]')"
  expect_eq "模板单元的阻塞码是 SERVICE_STATE_CONFLICT" "SERVICE_STATE_CONFLICT" \
    "$(jget "$PAYLOADS/preflight-template.json" 'd["data"]["blockers"][0]["code"]')"
else
  notrun "模板单元预检" "本机没有非别名的模板服务单元"
fi

echo
echo "== 7. 参数与注入边界 =="
inject_case() {
  _name=$1; _body=$2
  _code=$(preflight "$_name" "$_body")
  case $_code in
    400) pass "$_name 被拒绝（400）" ;;
    *) fail "$_name 被拒绝" "HTTP $_code" ;;
  esac
}
inject_case 'inject-operation' '{"action":"service.control","params":{"unit":"cron.service","operation":"start; reboot"}}'
inject_case 'inject-unit' '{"action":"service.control","params":{"unit":"cron.service; reboot","operation":"start"}}'
inject_case 'inject-flag' '{"action":"service.control","params":{"unit":"cron.service","operation":"restart --no-block"}}'
inject_case 'inject-extra' '{"action":"service.control","params":{"unit":"cron.service","operation":"start","args":["--all"]}}'
inject_case 'inject-unknown-op' '{"action":"service.control","params":{"unit":"cron.service","operation":"mask"}}'
inject_case 'inject-empty-unit' '{"action":"service.control","params":{"unit":"","operation":"start"}}'
for f in preflight-inject-operation.json preflight-inject-unit.json preflight-inject-flag.json; do
  [ -f "$PAYLOADS/$f" ] || continue
  if grep -qi 'reboot\|no-block' "$PAYLOADS/$f"; then
    note "$f 的响应里回显了被拒绝的原文（判定需要，但没有执行）"
  fi
done

echo
echo "== 8. 真实启停/重启/开机启动（预检 → 任务 → 读回） =="

# 8.1 start
st=$(run_action start-ok "$UNIT" start)
expect_eq "start 任务成功" "succeeded" "$st"
expect_eq "start 现场状态是 active" "active" "$(live_active "$UNIT")"
expect_eq "start observed_state.verified" "True" \
  "$(jget "$PAYLOADS/job-start-ok.json" 'd["data"]["result"]["observed_state"]["verified"]')"
expect_eq "start observed_state.active_state" "active" \
  "$(jget "$PAYLOADS/job-start-ok.json" 'd["data"]["result"]["observed_state"]["active_state"]')"
PID_BEFORE=$(jget "$PAYLOADS/job-start-ok.json" 'd["data"]["result"]["observed_state"].get("main_pid")')
if [ -n "$PID_BEFORE" ] && [ "$PID_BEFORE" != "None" ] && [ "$PID_BEFORE" -gt 0 ] 2>/dev/null; then
  pass "start 后 main_pid=$PID_BEFORE（真实进程号）"
else
  fail "start 后 main_pid 是真实进程号" "main_pid=$PID_BEFORE"
fi
STARTED_BEFORE=$(api "$JAR" "$CSRF" GET "/api/v1/services/$UNIT" >/dev/null; jget "$WORK/resp.json" 'd["data"]["unit"]["started_at"]')
if [ -n "$STARTED_BEFORE" ] && [ "$STARTED_BEFORE" != "None" ]; then
  pass "started_at 有实际时间（$STARTED_BEFORE）"
else
  fail "started_at 有实际时间" "started_at=$STARTED_BEFORE"
fi

# 8.2 状态冲突：已经 active 还 start
code=$(preflight start-again "{\"action\":\"service.control\",\"params\":{\"unit\":\"$UNIT\",\"operation\":\"start\"}}")
expect_eq "重复 start 的预检返回 200" "200" "$code"
expect_eq "重复 start allowed=false" "False" "$(jget "$PAYLOADS/preflight-start-again.json" 'd["data"]["allowed"]')"
expect_eq "重复 start 阻塞码" "SERVICE_STATE_CONFLICT" \
  "$(jget "$PAYLOADS/preflight-start-again.json" 'd["data"]["blockers"][0]["code"]')"
if jget "$PAYLOADS/job-submit-start-again.json" 'd["data"]["job_id"]' >/dev/null 2>&1; then
  fail "重复 start 没有生成任务" "居然有任务"
else
  pass "重复 start 没有生成任务"
fi

# 8.3 restart：必须真的重启（ActiveEnterTimestamp 前进），不是“看起来还是 active”
sleep 1
st=$(run_action restart-ok "$UNIT" restart)
expect_eq "restart 任务成功" "succeeded" "$st"
expect_eq "restart observed_state.verified" "True" \
  "$(jget "$PAYLOADS/job-restart-ok.json" 'd["data"]["result"]["observed_state"]["verified"]')"
STARTED_AFTER=$(api "$JAR" "$CSRF" GET "/api/v1/services/$UNIT" >/dev/null; \
  jget "$WORK/resp.json" 'd["data"]["unit"]["started_at"]')
if [ -n "$STARTED_AFTER" ] && [ "$STARTED_AFTER" != "$STARTED_BEFORE" ]; then
  pass "restart 后 started_at 前进（$STARTED_BEFORE → $STARTED_AFTER）"
else
  fail "restart 后 started_at 前进" "还是 $STARTED_AFTER"
fi

# 8.4 enable / disable
st=$(run_action enable-ok "$UNIT" enable)
expect_eq "enable 任务成功" "succeeded" "$st"
expect_eq "enable 现场状态是 enabled" "enabled" "$(live_enabled "$UNIT")"
expect_eq "enable observed_state.enablement" "enabled" \
  "$(jget "$PAYLOADS/job-enable-ok.json" 'd["data"]["result"]["observed_state"]["enablement"]')"

st=$(run_action disable-ok "$UNIT" disable)
expect_eq "disable 任务成功" "succeeded" "$st"
expect_eq "disable 现场状态是 disabled" "disabled" "$(live_enabled "$UNIT")"
# disable 只改开机启动，不该顺手停掉正在跑的服务。
expect_eq "disable 没有停止运行中的服务" "active" "$(live_active "$UNIT")"
expect_eq "disable 的告警是 scheduled_start_removed" "scheduled_start_removed" \
  "$(jget "$PAYLOADS/preflight-disable-ok.json" 'd["data"]["warnings"][0]["code"]')"

# 8.5 不可 enable 的单元（没有 [Install] 段）
code=$(preflight notenableable "{\"action\":\"service.control\",\"params\":{\"unit\":\"$FAIL_UNIT\",\"operation\":\"enable\"}}")
expect_eq "无 [Install] 段单元 enable 预检返回 200" "200" "$code"
expect_eq "无 [Install] 段单元 allowed=false" "False" "$(jget "$PAYLOADS/preflight-notenableable.json" 'd["data"]["allowed"]')"
expect_eq "无 [Install] 段单元阻塞码" "SERVICE_NOT_ENABLEABLE" \
  "$(jget "$PAYLOADS/preflight-notenableable.json" 'd["data"]["blockers"][0]["code"]')"

# 8.6 stop
st=$(run_action stop-ok "$UNIT" stop)
expect_eq "stop 任务成功" "succeeded" "$st"
expect_eq "stop 现场状态是 inactive" "inactive" "$(live_active "$UNIT")"
expect_eq "stop observed_state.verified" "True" \
  "$(jget "$PAYLOADS/job-stop-ok.json" 'd["data"]["result"]["observed_state"]["verified"]')"
expect_eq "stop 的告警是 service_interruption" "service_interruption" \
  "$(jget "$PAYLOADS/preflight-stop-ok.json" 'd["data"]["warnings"][0]["code"]')"

# 停止之后 main_pid 必须是 null，不是 0（systemd 的 MainPID=0 不是进程号）。
api "$JAR" "$CSRF" GET "/api/v1/services/$UNIT" >/dev/null
cp "$WORK/resp.json" "$PAYLOADS/unit-target-stopped.json"
expect_eq "停止后 main_pid 是 null" "None" "$(jget "$PAYLOADS/unit-target-stopped.json" 'd["data"]["unit"]["main_pid"]')"
expect_eq "停止后 capabilities 只剩 start/enable" "['start', 'enable']" \
  "$(jget "$PAYLOADS/unit-target-stopped.json" 'd["data"]["unit"]["capabilities"]')"

# 8.7 再冲突一次：inactive 还 stop / restart
code=$(preflight stop-again "{\"action\":\"service.control\",\"params\":{\"unit\":\"$UNIT\",\"operation\":\"stop\"}}")
expect_eq "重复 stop allowed=false" "False" "$(jget "$PAYLOADS/preflight-stop-again.json" 'd["data"]["allowed"]')"
expect_eq "重复 stop 阻塞码" "SERVICE_STATE_CONFLICT" \
  "$(jget "$PAYLOADS/preflight-stop-again.json" 'd["data"]["blockers"][0]["code"]')"
code=$(preflight restart-inactive "{\"action\":\"service.control\",\"params\":{\"unit\":\"$UNIT\",\"operation\":\"restart\"}}")
expect_eq "未运行时 restart allowed=false" "False" \
  "$(jget "$PAYLOADS/preflight-restart-inactive.json" 'd["data"]["allowed"]')"
expect_eq "未运行时 restart 阻塞码" "SERVICE_STATE_CONFLICT" \
  "$(jget "$PAYLOADS/preflight-restart-inactive.json" 'd["data"]["blockers"][0]["code"]')"

echo
echo "== 9. 失败必须失败（systemctl 非 0 → 任务 failed，绝不 succeeded） =="
st=$(run_action start-fail "$FAIL_UNIT" start)
expect_eq "ExecStart 失败的服务：任务状态是 failed" "failed" "$st"
FAILED_RESULT=$(jget "$PAYLOADS/job-start-fail.json" 'd["data"]["result"]')
# 契约（openapi Job.result）：没有结果时是空对象 {}，不是 null、也不是一份假结果。
expect_eq "失败任务的 result 是空对象（不伪造 observed_state）" "{}" "$FAILED_RESULT"
FAILED_KEYS=$(jget "$PAYLOADS/job-start-fail.json" 'sorted((d["data"]["result"] or {}).keys())')
expect_eq "失败任务的 result 里没有任何结果字段" "[]" "$FAILED_KEYS"
FAILED_MSG=$(jget "$PAYLOADS/job-start-fail.json" 'd["data"]["error"]["message"]')
if [ -n "$FAILED_MSG" ] && [ "$FAILED_MSG" != "None" ]; then
  pass "失败任务带可读错误：$FAILED_MSG"
else
  fail "失败任务带可读错误" "$FAILED_MSG"
fi
case $(live_active "$FAIL_UNIT") in
  failed|inactive) pass "失败服务现场状态是 $(live_active "$FAIL_UNIT")（不是 active）" ;;
  *) fail "失败服务现场状态不是 active" "$(live_active "$FAIL_UNIT")" ;;
esac
expect_eq "失败任务记在 jobs 表里且终态是 failed" "failed" \
  "$(dbq "$WEB_DB" "SELECT status FROM jobs WHERE id = '$(jget "$PAYLOADS/job-submit-start-fail.json" 'd["data"]["job_id"]')'")"
if grep -q '退出状态 1' "$AGENT_LOG" 2>/dev/null; then
  pass "代理日志记下了 systemctl 的真实退出状态（失败原因没有丢）"
else
  fail "代理日志记下 systemctl 的退出状态" "日志里没有『退出状态 1』"
fi

echo
echo "== 10. viewer 只读 =="
code=$(curl -s -b "$VJAR" -o "$WORK/viewer-services.json" -w '%{http_code}' "$BASE/api/v1/services")
expect_eq "viewer 能读列表" "200" "$code"
code=$(curl -s -b "$VJAR" -o "$WORK/viewer-unit.json" -w '%{http_code}' "$BASE/api/v1/services/$UNIT")
expect_eq "viewer 能读详情" "200" "$code"
code=$(curl -s -b "$VJAR" -o "$WORK/viewer-preflight.json" -w '%{http_code}' -X POST \
  "$BASE$PREFLIGHT_PATH" -H 'Content-Type: application/json' -H "Origin: $ORIGIN" \
  -H "X-CSRF-Token: $VCSRF" \
  -d "{\"action\":\"service.control\",\"params\":{\"unit\":\"$UNIT\",\"operation\":\"start\"}}")
expect_eq "viewer 不能创建预检" "403" "$code"
code=$(curl -s -b "$VJAR" -o "$WORK/viewer-job.json" -w '%{http_code}' -X POST "$BASE$JOBS_PATH" \
  -H 'Content-Type: application/json' -H "Origin: $ORIGIN" -H "X-CSRF-Token: $VCSRF" \
  -H "Idempotency-Key: viewer-attempt" -d '{"preflight_id":"whatever"}')
expect_eq "viewer 不能提交任务" "403" "$code"

echo
echo "== 11. 审计 =="
expect_eq "审计里有 service.control 的预检记录" "yes" \
  "$([ "$(dbq "$WEB_DB" "SELECT COUNT(*) FROM audit_events WHERE action = 'operation.preflight' AND target = 'preflight:service.control'")" -gt 0 ] && echo yes || echo no)"
expect_eq "审计里有对应的 job.submit 记录" "yes" \
  "$([ "$(dbq "$WEB_DB" "SELECT COUNT(*) FROM audit_events WHERE action = 'job.submit' AND job_id IS NOT NULL")" -gt 0 ] && echo yes || echo no)"
if dbq "$WEB_DB" "SELECT redacted_details FROM audit_events WHERE action = 'job.submit' LIMIT 1" | grep -q "$PASSWORD"; then
  fail "审计不含管理员口令" "redacted_details 里出现口令"
else
  pass "审计不含管理员口令"
fi

echo
echo "== 12. 载荷校验（字段、闭集、null 语义、与 systemctl 对照） =="
# 抓一份最终列表与一份最终详情，供前端真实载荷回放使用。
api "$JAR" "$CSRF" GET /api/v1/services >/dev/null
cp "$WORK/resp.json" "$PAYLOADS/services-final.json"
api "$JAR" "$CSRF" GET "/api/v1/services/$UNIT" >/dev/null
cp "$WORK/resp.json" "$PAYLOADS/unit-target-final.json"

# 抽样做逐项对照：受保护的那几个 + 列表里前 8 个可操作单元。
python3 - "$PAYLOADS/services-final.json" >"$PAYLOADS/sample-units.txt" <<'PY'
import json, sys
body = json.load(open(sys.argv[1], encoding="utf-8"))
units = body["data"]["units"]
pick = [u["unit"] for u in units if u["protection"]["protected"]][:6]
rest = [u["unit"] for u in units if not u["protection"]["protected"] and u["load_state"] == "loaded"][:8]
for name in pick + rest:
    print(name)
PY
: >"$PAYLOADS/crosscheck.tsv"
: >"$PAYLOADS/unit-map.tsv"
n=0
while read -r u; do
  [ -z "$u" ] && continue
  n=$((n + 1))
  code=$(api "$JAR" "$CSRF" GET "/api/v1/services/$u")
  if [ "$code" != 200 ]; then
    note "抽样单元 $u 详情返回 $code，跳过对照"
    continue
  fi
  cp "$WORK/resp.json" "$PAYLOADS/unit-$n.json"
  printf '%s\t%s\n' "$n" "$u" >>"$PAYLOADS/unit-map.tsv"
  printf '%s\t%s\t%s\n' "$u" "$(live_active "$u")" "$(live_enabled "$u")" >>"$PAYLOADS/crosscheck.tsv"
done <"$PAYLOADS/sample-units.txt"
note "逐项对照了 $n 个单元（is-active / is-enabled）"

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
echo "== 13. 日志卫生 =="
if grep -q "$PASSWORD" "$WEB_LOG" "$AGENT_LOG" 2>/dev/null; then
  fail "日志不含管理员口令"
else
  pass "日志不含管理员口令"
fi
if grep -q 'vm-correct-horse' "$WEB_LOG" 2>/dev/null; then
  fail "Web 日志不含任何验收口令"
else
  pass "Web 日志不含任何验收口令"
fi
if grep -qi 'no-block\|reboot' "$AGENT_LOG" 2>/dev/null; then
  note "代理日志里出现了被拒绝的注入片段（只应出现在拒绝记录里，可人工核对）"
else
  pass "代理日志里没有被拒绝的注入命令被执行"
fi

echo
echo "== 14. 无法在本环境覆盖的项（NOT RUN） =="
notrun "执行后未收敛（systemctl 成功但状态 15 秒内没变）" \
  "需要人为构造一个“命令成功但状态不变”的服务，正常 systemd 上做不到；由 fake 单测覆盖"
notrun "浏览器渲染（真实数据）" \
  "VM 上没有浏览器；页面级证据在宿主上回放这些载荷（web/src/fixtures/p05-1-vm/）"
notrun "面板自身作为 systemd 单元（upanel-web/upanel-agent）" \
  "验收环境里它们是直接启动的进程，不是 unit；保护判定由单元测试覆盖"

echo
printf '结果: PASS=%d FAIL=%d NOT RUN=%d\n' "$PASSED" "$FAILED" "$NOTRUN"
printf '载荷目录（可拷回宿主做前端真实数据回归）: %s\n' "$PAYLOADS"
[ -n "$FAILED_NAMES" ] && printf '失败项:%s\n' "$FAILED_NAMES"
[ "$FAILED" -eq 0 ]
