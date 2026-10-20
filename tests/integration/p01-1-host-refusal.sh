#!/bin/sh
# P01.1 宿主侧行为核验（Windows + Git Bash）。
#
# 用途：在没有 Linux 的宿主上验证「非 Linux 的 production 必须显式拒绝启动」「mock 只绑
# 127.0.0.1:18881」「未知路径不得返回页面或 200」这几条 P01 验收条款。
# 这是一个**负面/契约**核验脚本：它证明宿主上的拒绝与契约行为，不能证明 Ubuntu 上的可用性
# （Ubuntu 验收见 tests/integration/p01-1-acceptance.sh）。
#
# 用法（仓库根目录）：
#   sh tests/integration/p01-1-host-refusal.sh
#
# 退出码：0 期望行为全部出现；1 有偏差。

set -u
cd "$(dirname "$0")/../.." || exit 1

PASSED=0
FAILED=0
pass() { PASSED=$((PASSED + 1)); printf 'PASS  %s\n' "$1"; }
fail() { FAILED=$((FAILED + 1)); printf 'FAIL  %s: %s\n' "$1" "${2:-}"; }

echo "== 构建 Windows 二进制 =="
go build -o bin/upanel-web.exe ./cmd/upanel-web &&
  go build -o bin/upanel-agent.exe ./cmd/upanel-agent &&
  go build -o bin/upanelctl.exe ./cmd/upanelctl || { echo "构建失败"; exit 1; }
echo

echo "== 1. CLI =="
echo '$ bin/upanel-web.exe --version'
bin/upanel-web.exe --version
echo '$ bin/upanelctl.exe version'
bin/upanelctl.exe version
echo

echo "== 2. 非 Linux 上 production 必须拒绝启动 =="
echo '$ bin/upanel-web.exe --mode production'
bin/upanel-web.exe --mode production
CODE=$?
[ "$CODE" = "1" ] && pass "web-production-refused" || fail "web-production-refused" "退出码 $CODE"

echo '$ bin/upanel-web.exe --mode production --listen 0.0.0.0:9443'
bin/upanel-web.exe --mode production --listen 0.0.0.0:9443
CODE=$?
[ "$CODE" = "2" ] && pass "web-production-nonloopback-refused" || fail "web-production-nonloopback-refused" "退出码 $CODE"

echo '$ bin/upanel-agent.exe'
bin/upanel-agent.exe
CODE=$?
[ "$CODE" = "1" ] && pass "agent-refused-on-windows" || fail "agent-refused-on-windows" "退出码 $CODE"

echo '$ bin/upanel-agent.exe --mode mock'
bin/upanel-agent.exe --mode mock >/dev/null 2>&1
CODE=$?
[ "$CODE" = "2" ] && pass "agent-has-no-mode-flag" || fail "agent-has-no-mode-flag" "退出码 $CODE"
echo

echo "== 3. mock 模式契约 =="
# P01.2 起数据库是 Web 的硬依赖：脚本给出明确的可写路径（Windows 绝对路径），
# 否则 Web 会按设计拒绝启动，而不是进入 mock 模式。
DB="$(pwd -W)/bin/accept-host.db"
rm -f "$DB" "$DB-wal" "$DB-shm"
bin/upanel-web.exe --mode mock --db "$DB" > /tmp/mock.log 2>&1 &
MOCK_PID=$!
sleep 2

BODY=$(curl -s --max-time 5 http://127.0.0.1:18881/api/v1/health/live)
case $BODY in
  *'"status":"ok"'*) pass "mock-health-live" ;;
  *) fail "mock-health-live" "$BODY" ;;
esac

RID_BODY=$(curl -s --max-time 5 -H 'X-Request-ID: host-check-1' http://127.0.0.1:18881/api/v1/health/live)
RID_HEADER=$(curl -s -D - -o /dev/null --max-time 5 -H 'X-Request-ID: host-check-1' \
  http://127.0.0.1:18881/api/v1/health/live | tr -d '\r' | sed -n 's/^[Xx]-[Rr]equest-[Ii][Dd]: *//p')
case $RID_BODY in
  *'"request_id":"host-check-1"'*) pass "mock-request-id-in-body" ;;
  *) fail "mock-request-id-in-body" "$RID_BODY" ;;
esac
[ "$RID_HEADER" = "host-check-1" ] && pass "mock-request-id-header" || fail "mock-request-id-header" "$RID_HEADER"

ROOT_CODE=$(curl -s -o /tmp/root.txt -w '%{http_code}' --max-time 5 http://127.0.0.1:18881/)
ROOT_BODY=$(cat /tmp/root.txt)
[ "$ROOT_CODE" = "404" ] && pass "mock-root-404" || fail "mock-root-404" "HTTP $ROOT_CODE"
case $ROOT_BODY in
  *'"code":"NOT_FOUND"'*) pass "mock-root-json" ;;
  *) fail "mock-root-json" "$ROOT_BODY" ;;
esac

IDX_CODE=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 http://127.0.0.1:18881/index.html)
[ "$IDX_CODE" = "404" ] && pass "mock-index-html-not-served" || fail "mock-index-html-not-served" "HTTP $IDX_CODE"

POST_CODE=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 -X POST http://127.0.0.1:18881/api/v1/health/live)
[ "$POST_CODE" = "405" ] && pass "mock-method-not-allowed" || fail "mock-method-not-allowed" "HTTP $POST_CODE"

echo '$ bin/upanel-web.exe --mode mock --listen 0.0.0.0:18881'
bin/upanel-web.exe --mode mock --listen 0.0.0.0:18881 > /dev/null 2>&1
CODE=$?
[ "$CODE" = "2" ] && pass "mock-nonloopback-refused" || fail "mock-nonloopback-refused" "退出码 $CODE"

kill -TERM $MOCK_PID 2>/dev/null
wait $MOCK_PID 2>/dev/null
CODE=$?
[ "$CODE" = "143" ] && pass "mock-sigterm-exit" || fail "mock-sigterm-exit" "退出码 $CODE"
sleep 1
LEFT=$(curl -s -o /dev/null -w '%{http_code}' --max-time 2 http://127.0.0.1:18881/api/v1/health/live || true)
[ "$LEFT" = "000" ] && pass "mock-port-released" || fail "mock-port-released" "仍可访问: $LEFT"
echo

echo "== 4. 配置文件与 CLI =="
cat > /tmp/upanel-host.json <<'JSON'
{"web":{"listen_address":"127.0.0.1:18881"},"log":{"format":"text","level":"debug"}}
JSON
echo '$ bin/upanelctl.exe check-config --config /tmp/upanel-host.json'
bin/upanelctl.exe check-config --config /tmp/upanel-host.json
CODE=$?
[ "$CODE" = "0" ] && pass "ctl-check-config" || fail "ctl-check-config" "退出码 $CODE"

echo '$ bin/upanelctl.exe check-config --config /tmp/not-exist.json'
bin/upanelctl.exe check-config --config /tmp/not-exist.json > /dev/null 2>&1
CODE=$?
[ "$CODE" = "1" ] && pass "ctl-missing-config-refused" || fail "ctl-missing-config-refused" "退出码 $CODE"
echo

echo "== 汇总 =="
echo "PASS $PASSED / FAIL $FAILED"
[ "$FAILED" -eq 0 ] || exit 1
exit 0
