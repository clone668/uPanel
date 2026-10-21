#!/bin/sh
# P01.1 Linux 验收脚本。
#
# 运行位置：Ubuntu 22.04/24.04 验收 VM 内（用户需能 sudo；VM 内 upanel 用户为 NOPASSWD）。
# 用途：验证两个服务入口与 CLI 的真实行为，不依赖 Go/Node 工具链（使用交叉编译好的二进制）。
#
# 用法：
#   BIN_DIR=/tmp sh tests/integration/p01-1-acceptance.sh
#
# 退出码：0 全部通过；1 有失败项。
#
# 覆盖的验收点（对应 STAGES.md P01 验收 + 本轮 P01.1 范围）：
#   1. 版本与 CLI（upanelctl version / check-config）
#   2. mock 模式只绑定 127.0.0.1:18881，且拒绝其他地址
#   3. production 模式由非 root 用户运行，监听 127.0.0.1:9443
#   4. 存活检查响应符合契约，未知路径为 JSON 404（不返回 HTML/200）
#   5. 优雅停止（SIGTERM）返回 0 且清理资源
#   6. 代理拒绝非 root；以 root 运行后只创建 0660 Unix socket，不监听 TCP
#   7. 代理只提供 capabilities，未注册动作时如实返回空数组
#   8. 代理拒绝覆盖同名非 socket 文件

set -u

BIN_DIR=${BIN_DIR:-/tmp}
WEB="$BIN_DIR/upanel-web"
AGENT="$BIN_DIR/upanel-agent"
CTL="$BIN_DIR/upanelctl"

PASSED=0
FAILED=0
FAILED_NAMES=""

pass() { PASSED=$((PASSED + 1)); printf 'PASS  %s\n' "$1"; }
fail() { FAILED=$((FAILED + 1)); FAILED_NAMES="$FAILED_NAMES $1"; printf 'FAIL  %s: %s\n' "$1" "${2:-}"; }

# expect_contains NAME HAYSTACK NEEDLE
expect_contains() {
  case $2 in
    *"$3"*) pass "$1" ;;
    *) fail "$1" "输出中缺少 '$3'（实际: $2）" ;;
  esac
}

expect_eq() {
  if [ "$2" = "$3" ]; then pass "$1"; else fail "$1" "期望 '$3'，实际 '$2'"; fi
}

cleanup() {
  if [ -n "${WEB_PID:-}" ]; then kill -TERM "$WEB_PID" 2>/dev/null; wait "$WEB_PID" 2>/dev/null; fi
  if [ -n "${AGENT_PID:-}" ]; then $SUDO pkill -TERM -f "$AGENT" 2>/dev/null; fi
}
trap cleanup EXIT INT TERM

# ---------------------------------------------------------------- 前提
[ -x "$WEB" ] || { echo "缺少可执行文件 $WEB"; exit 1; }
[ -x "$AGENT" ] || { echo "缺少可执行文件 $AGENT"; exit 1; }
[ -x "$CTL" ] || { echo "缺少可执行文件 $CTL"; exit 1; }

if [ "$(id -u)" -eq 0 ]; then SUDO=""; else SUDO="sudo"; fi

# 显式指定配置文件，避免 VM 上残留的 /etc/upanel/upanel.json 影响结果可复现性。
CFG="$BIN_DIR/accept-config.json"
cat > "$CFG" <<'JSON'
{
  "web": {"mode": "production", "listen_address": "127.0.0.1:9443", "shutdown_timeout_seconds": 10},
  "agent": {"socket_path": "/run/upanel/agent.sock", "shutdown_timeout_seconds": 10},
  "log": {"level": "info", "format": "text"}
}
JSON

# mock 模式必须绑定 127.0.0.1:18881，因此单独一份不含 listen_address 的配置；
# 若复用上面的配置（已显式写 9443），--mode mock 会按设计直接报错，而不是静默改写端口。
CFG_MOCK="$BIN_DIR/accept-config-mock.json"
cat > "$CFG_MOCK" <<'JSON'
{
  "web": {"mode": "mock", "shutdown_timeout_seconds": 10},
  "log": {"level": "info", "format": "text"}
}
JSON

echo "== 环境 =="
echo "系统: $(. /etc/os-release; echo "$PRETTY_NAME")"
echo "内核: $(uname -r)"
echo "用户: $(id -un) (uid=$(id -u))"
echo "二进制目录: $BIN_DIR"
echo "配置文件: $CFG"
echo "mock 配置: $CFG_MOCK"
echo

# ---------------------------------------------------------------- 1. 版本与 CLI
echo "== 1. 版本与配置检查 =="
VERSION_OUT=$("$CTL" version 2>&1)
expect_contains "ctl-version" "$VERSION_OUT" "upanelctl"

CONFIG_OUT=$("$CTL" check-config --config "$CFG" 2>&1)
expect_contains "ctl-check-config-web" "$CONFIG_OUT" "127.0.0.1:9443"
expect_contains "ctl-check-config-socket" "$CONFIG_OUT" "/run/upanel/agent.sock"

# P01.2 起数据库是 Web 的硬依赖：脚本显式给出临时数据库路径，
# 否则 Web 会因缺少可写数据库而拒绝启动（这是设计行为，不是故障）。
DB="$BIN_DIR/accept-p01-1.db"
rm -f "$DB" "$DB-wal" "$DB-shm"

printf '{"web":{"mode":"mock","mdoe":"typo"}}' > "$BIN_DIR/bad-config.json"
"$CTL" check-config --config "$BIN_DIR/bad-config.json" >/dev/null 2>&1
expect_eq "ctl-rejects-unknown-field" "$?" "1"
echo

# ---------------------------------------------------------------- 2. mock 模式
echo "== 2. mock 模式 =="
"$WEB" --config "$CFG_MOCK" --db "$DB" > "$BIN_DIR/accept-mock.log" 2>&1 &
WEB_PID=$!
sleep 2

LISTEN_MOCK=$(ss -ltn 2>/dev/null | awk '$4 ~ /:18881$/ {print $4}' | head -n 1)
expect_eq "mock-listens-18881" "$LISTEN_MOCK" "127.0.0.1:18881"

MOCK_BODY=$(curl -s --max-time 5 http://127.0.0.1:18881/api/v1/health/live)
expect_contains "mock-health-live" "$MOCK_BODY" '{"data":{"status":"ok"}'

# P01.4 起前端产物嵌进二进制，未知的**根路径**由 SPA 回退返回 index.html（200），
# 未知的 **API 路径**才必须是 JSON 404。这两条断言跟着行为一起改，不是放宽：
# “未知 API 不得被 SPA 吞掉”比原来的“根路径 404”是更强的边界，也仍然是 404 + NOT_FOUND。
MOCK_ROOT_CODE=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 http://127.0.0.1:18881/)
expect_eq "mock-unknown-root-serves-spa" "$MOCK_ROOT_CODE" "200"

MOCK_ROOT_BODY=$(curl -s --max-time 5 http://127.0.0.1:18881/)
expect_contains "mock-unknown-root-serves-spa-html" "$MOCK_ROOT_BODY" '<title>uPanel</title>'

MOCK_API_CODE=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 http://127.0.0.1:18881/api/v1/no-such-endpoint)
expect_eq "mock-unknown-api-404" "$MOCK_API_CODE" "404"

MOCK_API_BODY=$(curl -s --max-time 5 http://127.0.0.1:18881/api/v1/no-such-endpoint)
expect_contains "mock-unknown-api-json" "$MOCK_API_BODY" '"code":"NOT_FOUND"'

kill -TERM "$WEB_PID" 2>/dev/null
wait "$WEB_PID" 2>/dev/null
WEB_EXIT=$?
WEB_PID=""
expect_eq "mock-graceful-shutdown" "$WEB_EXIT" "0"

"$WEB" --config "$CFG_MOCK" --listen 0.0.0.0:18881 > "$BIN_DIR/accept-mock-bad.log" 2>&1
expect_eq "mock-rejects-non-loopback" "$?" "2"
echo

# ---------------------------------------------------------------- 3. production 模式
echo "== 3. production 模式 =="
"$WEB" --config "$CFG" --mode production --db "$DB" > "$BIN_DIR/accept-prod.log" 2>&1 &
WEB_PID=$!
sleep 2

LISTEN_PROD=$(ss -ltn 2>/dev/null | awk '$4 ~ /:9443$/ {print $4}' | head -n 1)
expect_eq "prod-listens-9443" "$LISTEN_PROD" "127.0.0.1:9443"

PROD_BODY=$(curl -s --max-time 5 http://127.0.0.1:9443/api/v1/health/live)
expect_contains "prod-health-live" "$PROD_BODY" '"request_id"'

RID=$(curl -s -D - -o /dev/null --max-time 5 -H 'X-Request-ID: acceptance-rid-1' \
  http://127.0.0.1:9443/api/v1/health/live | tr -d '\r' | sed -n 's/^[Xx]-[Rr]equest-[Ii][Dd]: *//p')
expect_eq "prod-request-id-echo" "$RID" "acceptance-rid-1"

METHOD_CODE=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 -X POST http://127.0.0.1:9443/api/v1/health/live)
expect_eq "prod-method-not-allowed" "$METHOD_CODE" "405"

kill -TERM "$WEB_PID" 2>/dev/null
wait "$WEB_PID" 2>/dev/null
expect_eq "prod-graceful-shutdown" "$?" "0"
WEB_PID=""

# PLAN.md 第 6 节：面板自身要监听 HTTPS 9443，TLS 在 P10 接入；在此之前
# production 绑定非回环地址必须直接失败，不得把明文管理界面暴露到网络上。
"$WEB" --config "$CFG" --listen 0.0.0.0:9443 > "$BIN_DIR/accept-prod-nonloopback.log" 2>&1
expect_eq "prod-rejects-non-loopback-before-tls" "$?" "2"
echo

# ---------------------------------------------------------------- 4. 代理
echo "== 4. 特权代理 =="
SOCKET=/run/upanel/agent.sock

if [ "$(id -u)" -ne 0 ]; then
  "$AGENT" --config "$CFG" > "$BIN_DIR/accept-agent-noroot.log" 2>&1
  expect_eq "agent-refuses-nonroot" "$?" "1"
fi

# P02.3 起代理会自检 socket 目录：0750 root:root 会让 Web 账号进不去，必须 0755 root:root。
# 这条断言不是“为了让测试变绿”而改的，是代理真的拒绝了不安全的目录（日志里有原文）。
$SUDO install -d -m 0755 -o root -g root /run/upanel || fail "agent-socket-dir" "无法创建 /run/upanel"

# P02.4 起代理没有密钥就拒绝启动（秘密存储的前提）。P01.1 的脚本早于这个要求，
# 所以在干净机器上要先补一次 -init-key；已存在时不覆盖。
if ! $SUDO test -s /etc/upanel/agent.key; then
  $SUDO "$AGENT" --config "$CFG" -init-key > "$BIN_DIR/accept-agent-initkey.log" 2>&1
  expect_eq "agent-init-key" "$?" "0"
fi

$SUDO "$AGENT" --config "$CFG" > "$BIN_DIR/accept-agent.log" 2>&1 &
AGENT_PID=$!
sleep 2

SOCKET_MODE=$($SUDO stat -c '%F %A' "$SOCKET" 2>/dev/null)
expect_eq "agent-socket-0660" "$SOCKET_MODE" "socket srw-rw----"

TCP_COUNT=$(ss -ltnp 2>/dev/null | grep -c "upanel-agent")
expect_eq "agent-no-tcp-listener" "$TCP_COUNT" "0"

# P02.3 起普通接口只认 upanel-web 这个 UID，root 与其他 UID 一律 403（SO_PEERCRED）。
# P01.1 的脚本当时是以 root 直接问代理，现在必须先“以 Web 账号身份”问，
# 并且**单独**断言 root 被拒——边界本身也是验收项，不是绕过去就算。
if [ -z "$SUDO" ]; then AS_WEB="runuser -u upanel-web --"; else AS_WEB="sudo -u upanel-web"; fi

CAPS=$($AS_WEB curl -s --max-time 5 --unix-socket "$SOCKET" http://localhost/internal/v1/capabilities)
# capabilities 是全量闭集：生产代理只要依赖齐备就会登记全部查询与动作，
# P05.1 起 supported_actions 不再为空。断言**完全等于**当前应有的集合——
# 少报是隐藏能力，多报是编造能力，两种都算失败（DECISIONS Q86/Q115）。
# 新增查询/动作时必须同步更新这两行。
EXPECTED_QUERIES="['logs.query', 'processes.list', 'services.list', 'services.unit', 'storage.topology', 'system.summary']"
EXPECTED_ACTIONS="['process.signal', 'service.control']"
CAPS_QUERIES=$(printf '%s' "$CAPS" | python3 -c 'import json,sys; print(json.load(sys.stdin)["data"]["supported_queries"])')
CAPS_ACTIONS=$(printf '%s' "$CAPS" | python3 -c 'import json,sys; print(json.load(sys.stdin)["data"]["supported_actions"])')
expect_contains "agent-capabilities-version" "$CAPS" '"version"'
expect_eq "agent-capabilities-queries-exact" "$EXPECTED_QUERIES" "$CAPS_QUERIES"
expect_eq "agent-capabilities-actions-exact" "$EXPECTED_ACTIONS" "$CAPS_ACTIONS"
expect_contains "agent-capabilities-mode" "$CAPS" '"mode":"production"'

ROOT_CODE=$($SUDO curl -s -o /dev/null -w '%{http_code}' --max-time 5 --unix-socket "$SOCKET" \
  http://localhost/internal/v1/capabilities)
expect_eq "agent-root-forbidden" "$ROOT_CODE" "403"

UNKNOWN_CODE=$($AS_WEB curl -s -o /dev/null -w '%{http_code}' --max-time 5 --unix-socket "$SOCKET" \
  http://localhost/internal/v1/no-such-endpoint)
expect_eq "agent-unknown-path-404" "$UNKNOWN_CODE" "404"

# /internal/v1/streams 在 P05.3 已实现：GET 它是 405，不是“未知路径”。
STREAMS_GET=$($AS_WEB curl -s -o /dev/null -w '%{http_code}' --max-time 5 --unix-socket "$SOCKET"   http://localhost/internal/v1/streams)
expect_eq "agent-streams-get-405" "$STREAMS_GET" "405"

$SUDO pkill -TERM -f "$AGENT" 2>/dev/null
sleep 2
AGENT_PID=""
if $SUDO test -e "$SOCKET"; then fail "agent-socket-cleanup" "socket 未清理"; else pass "agent-socket-cleanup"; fi

# 同名非 socket 文件必须拒绝覆盖
$SUDO touch "$SOCKET"
$SUDO "$AGENT" --config "$CFG" > "$BIN_DIR/accept-agent-file.log" 2>&1
AGENT_EXIT=$?
expect_eq "agent-refuses-overwrite-file" "$AGENT_EXIT" "1"
if $SUDO test -f "$SOCKET"; then pass "agent-keeps-existing-file"; else fail "agent-keeps-existing-file" "文件被删除"; fi
$SUDO rm -f "$SOCKET"
echo

# ---------------------------------------------------------------- 汇总
echo "== 汇总 =="
echo "PASS $PASSED / FAIL $FAILED"
[ -n "$FAILED_NAMES" ] && echo "失败项:$FAILED_NAMES"
[ "$FAILED" -eq 0 ] || exit 1
exit 0
