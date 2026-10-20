#!/bin/sh
# P01.4 Linux 验收脚本：前端嵌入、同源 API/静态页、systemd 单元与真实启动。
#
# 运行位置：Ubuntu 22.04/24.04 验收 VM 内（推荐用能 sudo 的非 root 用户运行）。
# 用途：验证嵌入式前端二进制的真实行为，不依赖 Go/Node 工具链（使用交叉编译好的二进制）。
#
# 用法：
#   BIN_DIR=/tmp DEPLOY_DIR=/tmp/deploy sh tests/integration/p01-4-acceptance.sh
#   INSTALL_SYSTEMD=0 ...   # 跳过 systemd 安装/启动段（该段会标 NOT RUN）
#
# 退出码：0 全部通过；1 有失败项（NOT RUN 不算失败，但会在结尾单独列出）。
#
# 覆盖的验收点（对应 STAGES.md P01 验收 + P01.4 范围）：
#   1. 二进制内嵌前端资源，页面由二进制自身提供（不是反向代理或磁盘目录）
#   2. 页面注入运行模式；mock 页面注入 mock，production 页面注入 production
#   3. 安全响应头与 CSP nonce 生效（每次响应 nonce 不同，不允许 script-src 'unsafe-inline'）
#   4. 带哈希的静态资源可加载并设置长缓存；index 不缓存
#   5. SPA 深链接回退到 index；缺失的静态资源返回 404，不伪装成页面
#   6. 未知 API（含 /api、/api/ 与不存在的方法）返回 JSON 404，绝不返回 index.html
#   7. 请求方法不允许返回 405 且带 Allow
#   8. mock 只监听 127.0.0.1:18881；master 配置写错地址时拒绝启动
#   9. production 只监听回环地址，非回环地址拒绝启动
#  10. Web 进程以非 root 运行（systemd 段验证 User=upanel-web 与 MainPID 归属）
#  11. systemd 单元可被 systemd-analyze 校验；可选真实安装启动并访问 9443

set -u

BIN_DIR=${BIN_DIR:-/tmp}
DEPLOY_DIR=${DEPLOY_DIR:-$BIN_DIR/deploy}
WEB="$BIN_DIR/upanel-web"
CTL="$BIN_DIR/upanelctl"
UNIT_SRC="$DEPLOY_DIR/systemd/upanel-web.service"

INSTALL_SYSTEMD=${INSTALL_SYSTEMD:-1}
RUN_ROOT=${RUN_ROOT:-/var/tmp/upanel-accept-p01-4}
MOCK_DB="$RUN_ROOT/mock/upanel.db"
PROD_DB="$RUN_ROOT/prod/upanel.db"

PASSED=0
FAILED=0
NOTRUN=0
FAILED_NAMES=""

pass() { PASSED=$((PASSED + 1)); printf 'PASS    %s\n' "$1"; }
fail() { FAILED=$((FAILED + 1)); FAILED_NAMES="$FAILED_NAMES $1"; printf 'FAIL    %s: %s\n' "$1" "${2:-}"; }
notrun() { NOTRUN=$((NOTRUN + 1)); printf 'NOT RUN %s: %s\n' "$1" "${2:-}"; }

expect_contains() {
  case $2 in
    *"$3"*) pass "$1" ;;
    *) fail "$1" "输出中缺少 '$3'（实际: $2）" ;;
  esac
}

expect_not_contains() {
  case $2 in
    *"$3"*) fail "$1" "输出中不应出现 '$3'（实际: $2）" ;;
    *) pass "$1" ;;
  esac
}

expect_eq() {
  if [ "$2" = "$3" ]; then pass "$1"; else fail "$1" "期望 '$3'，实际 '$2'"; fi
}

# header <文件> <头名>：取响应头值（大小写不敏感，去掉 \r）。
header() {
  tr -d '\r' < "$1" | awk -v want="$(printf '%s' "$2" | tr 'A-Z' 'a-z')" '
    index(tolower($0), want ":") == 1 { sub(/^[^:]*:[ ]*/, ""); print; exit }
  '
}

WEB_PID=""
PROD_PID=""

# as_user <用户名> <命令...>：以指定普通用户执行（root 用 runuser，非 root 用 sudo -u）。
as_user() {
  target=$1
  shift
  if [ -z "$SUDO" ]; then
    runuser -u "$target" -- "$@"
  else
    $SUDO -u "$target" -- "$@"
  fi
}

cleanup() {
  for pid in $WEB_PID $PROD_PID; do
    if [ -n "$pid" ]; then kill -TERM "$pid" 2>/dev/null; wait "$pid" 2>/dev/null; fi
  done
}
trap cleanup EXIT INT TERM

# ---------------------------------------------------------------- 0. 前提
[ -x "$WEB" ] || { echo "缺少可执行文件 $WEB"; exit 1; }
[ -x "$CTL" ] || { echo "缺少可执行文件 $CTL"; exit 1; }

if [ "$(uname -s)" != "Linux" ]; then echo "本脚本必须在 Linux 上运行，当前 $(uname -s)"; exit 1; fi
if [ "$(id -u)" -eq 0 ]; then
  # P01 验收要求 Web 不以 root 运行；以 root 跑脚本无法证明这一点，直接拒绝。
  echo "请用带 sudo 权限的普通用户运行本脚本（当前为 root）"
  exit 1
fi
SUDO="sudo"

$SUDO rm -rf "$RUN_ROOT"
mkdir -p "$RUN_ROOT/mock" "$RUN_ROOT/prod"

MOCK_CFG="$BIN_DIR/accept-p01-4-mock.json"
cat > "$MOCK_CFG" <<JSON
{
  "web": {"mode": "mock", "listen_address": "127.0.0.1:18881", "shutdown_timeout_seconds": 10},
  "database": {"path": "$MOCK_DB", "busy_timeout_ms": 5000},
  "log": {"level": "info", "format": "text"}
}
JSON

PROD_CFG="$BIN_DIR/accept-p01-4-prod.json"
cat > "$PROD_CFG" <<JSON
{
  "web": {"mode": "production", "listen_address": "127.0.0.1:9443", "shutdown_timeout_seconds": 10},
  "database": {"path": "$PROD_DB", "busy_timeout_ms": 5000},
  "log": {"level": "info", "format": "text"}
}
JSON

"$CTL" migrate --config "$MOCK_CFG" > "$BIN_DIR/accept-p01-4-migrate-mock.log" 2>&1
[ $? -eq 0 ] || { echo "迁移失败，见 $BIN_DIR/accept-p01-4-migrate-mock.log"; exit 1; }

echo "== 1. mock 模式：二进制自带前端 =="
"$WEB" --config "$MOCK_CFG" > "$BIN_DIR/accept-p01-4-web-mock.log" 2>&1 &
WEB_PID=$!
# 等待监听就绪（最多 30 次 × 0.2s）。
tries=0
until curl -s -o /dev/null --max-time 2 http://127.0.0.1:18881/api/v1/health/live; do
  tries=$((tries + 1))
  [ "$tries" -ge 30 ] && break
  sleep 0.2
done

MOCK_LOG=$(cat "$BIN_DIR/accept-p01-4-web-mock.log")
expect_contains "web-reports-embedded-frontend" "$MOCK_LOG" "前端资源已就绪"
expect_contains "web-reports-embedded-source" "$MOCK_LOG" "嵌入二进制"
expect_contains "web-listens-loopback-18881" "$MOCK_LOG" "address=127.0.0.1:18881"

HDR="$BIN_DIR/accept-p01-4-index.headers"
BODY="$BIN_DIR/accept-p01-4-index.html"
CODE=$(curl -s -w '%{http_code}' -D "$HDR" -o "$BODY" --max-time 5 http://127.0.0.1:18881/)
expect_eq "index-status-200" "$CODE" "200"
expect_eq "index-content-type-html" "$(header "$HDR" Content-Type)" "text/html; charset=utf-8"
expect_eq "index-cache-control-no-store" "$(header "$HDR" Cache-Control)" "no-store"
expect_eq "index-nosniff" "$(header "$HDR" X-Content-Type-Options)" "nosniff"
expect_eq "index-frame-deny" "$(header "$HDR" X-Frame-Options)" "DENY"
expect_eq "index-referrer-policy" "$(header "$HDR" Referrer-Policy)" "no-referrer"
expect_eq "index-coop" "$(header "$HDR" Cross-Origin-Opener-Policy)" "same-origin"

INDEX_HTML=$(cat "$BODY")
expect_contains "index-has-mount-point" "$INDEX_HTML" 'id="app"'
expect_contains "index-injects-mock-mode" "$INDEX_HTML" 'window.__UPANEL_MODE__="mock"'

CSP=$(header "$HDR" Content-Security-Policy)
expect_contains "csp-script-src-self" "$CSP" "script-src 'self'"
expect_not_contains "csp-script-src-same-origin-only" "$CSP" "script-src 'self' 'unsafe-inline'"
expect_contains "csp-default-src-self" "$CSP" "default-src 'self'"
expect_contains "csp-object-src-none" "$CSP" "object-src 'none'"
expect_contains "csp-frame-ancestors-none" "$CSP" "frame-ancestors 'none'"

NONCE1=$(printf '%s' "$CSP" | sed -n "s/.*'nonce-\([^']*\)'.*/\1/p")
[ -n "$NONCE1" ] && pass "csp-mock-nonce-present" || fail "csp-mock-nonce-present" "CSP 中没有 nonce"
INLINE_NONCE=$(printf '%s' "$INDEX_HTML" | sed -n 's/.*<script nonce="\([^"]*\)".*/\1/p')
expect_eq "index-inline-script-uses-csp-nonce" "$INLINE_NONCE" "$NONCE1"

HDR2="$BIN_DIR/accept-p01-4-index-2.headers"
curl -s -D "$HDR2" -o /dev/null --max-time 5 http://127.0.0.1:18881/
CSP2=$(header "$HDR2" Content-Security-Policy)
NONCE2=$(printf '%s' "$CSP2" | sed -n "s/.*'nonce-\([^']*\)'.*/\1/p")
if [ -n "$NONCE2" ] && [ "$NONCE1" != "$NONCE2" ]; then
  pass "csp-nonce-rotates-per-response"
else
  fail "csp-nonce-rotates-per-response" "两次响应 nonce 相同或缺失（$NONCE1 / $NONCE2）"
fi

# ---------------------------------------------------------------- 2. 静态资源与 SPA 回退
echo
echo "== 2. 静态资源、SPA 回退与 404 边界 =="
ASSET=$(printf '%s' "$INDEX_HTML" | grep -o '/assets/[A-Za-z0-9._-]*\.js' | head -1)
if [ -n "$ASSET" ]; then
  AHDR="$BIN_DIR/accept-p01-4-asset.headers"
  ACODE=$(curl -s -w '%{http_code}' -D "$AHDR" -o "$BIN_DIR/accept-p01-4-asset.js" --max-time 5 "http://127.0.0.1:18881$ASSET")
  expect_eq "hashed-asset-status-200" "$ACODE" "200"
  expect_eq "hashed-asset-content-type-js" "$(header "$AHDR" Content-Type)" "text/javascript; charset=utf-8"
  expect_contains "hashed-asset-immutable-cache" "$(header "$AHDR" Cache-Control)" "immutable"
  ASIZE=$(wc -c < "$BIN_DIR/accept-p01-4-asset.js" | tr -d ' ')
  if [ "$ASIZE" -gt 1000 ]; then pass "hashed-asset-nonempty"; else fail "hashed-asset-nonempty" "资源只有 $ASIZE 字节"; fi
  case $ASSET in
    *-*.*) pass "hashed-asset-name-hashed" ;;
    *) fail "hashed-asset-name-hashed" "资源名不像带哈希构建产物: $ASSET" ;;
  esac
else
  fail "hashed-asset-reference-present" "index.html 中没有 /assets/*.js 引用"
fi

for path in /storage /settings/network; do
  HDRX="$BIN_DIR/accept-p01-4-spa.headers"
  CODE=$(curl -s -w '%{http_code}' -D "$HDRX" -o "$BIN_DIR/accept-p01-4-spa.html" --max-time 5 "http://127.0.0.1:18881$path")
  expect_eq "spa-fallback$path-status-200" "$CODE" "200"
  expect_contains "spa-fallback$path-html" "$(cat "$BIN_DIR/accept-p01-4-spa.html")" 'id="app"'
  expect_eq "spa-fallback$path-content-type" "$(header "$HDRX" Content-Type)" "text/html; charset=utf-8"
done

MHDR="$BIN_DIR/accept-p01-4-missing.headers"
MCODE=$(curl -s -w '%{http_code}' -D "$MHDR" -o "$BIN_DIR/accept-p01-4-missing.body" --max-time 5 http://127.0.0.1:18881/assets/missing-abc123.js)
expect_eq "missing-asset-status-404" "$MCODE" "404"
expect_not_contains "missing-asset-not-index" "$(cat "$BIN_DIR/accept-p01-4-missing.body")" 'id="app"'

# 目录穿越与可疑路径不得泄露宿主文件；页面路径回退到 index 是 SPA 的预期行为。
for probe in "/../../etc/passwd" "/assets/../../../etc/passwd" "/static/..%2f..%2fetc%2fpasswd"; do
  TBODY="$BIN_DIR/accept-p01-4-traversal.body"
  TCODE=$(curl -s -w '%{http_code}' -o "$TBODY" --path-as-is --max-time 5 "http://127.0.0.1:18881$probe")
  TDATA=$(cat "$TBODY")
  expect_not_contains "traversal$probe-no-leak" "$TDATA" "root:x:"
  case $TCODE in
    200) expect_contains "traversal$probe-spa-only" "$TDATA" 'id="app"' ;;
    400|404) pass "traversal$probe-rejected" ;;
    *) fail "traversal$probe-status-sane" "状态码 $TCODE" ;;
  esac
done

# ---------------------------------------------------------------- 3. 同源 API 边界
echo
echo "== 3. 同源 API 与未知路径 =="
HHDR="$BIN_DIR/accept-p01-4-health.headers"
HCODE=$(curl -s -w '%{http_code}' -D "$HHDR" -o "$BIN_DIR/accept-p01-4-health.json" --max-time 5 http://127.0.0.1:18881/api/v1/health/live)
HEALTH=$(cat "$BIN_DIR/accept-p01-4-health.json")
expect_eq "api-health-status-200" "$HCODE" "200"
expect_contains "api-health-envelope" "$HEALTH" '{"data":'
expect_contains "api-health-request-id-body" "$HEALTH" '"request_id":'
expect_not_contains "api-health-not-html" "$HEALTH" 'id="app"'
RID=$(header "$HHDR" X-Request-ID)
if [ -n "$RID" ]; then pass "api-health-request-id-header"; else fail "api-health-request-id-header" "缺少 X-Request-ID"; fi

for path in /api /api/ /api/v1/unknown /api/v1/health/unknown /api/v2/health/live; do
  UHDR="$BIN_DIR/accept-p01-4-unknown.headers"
  UCODE=$(curl -s -w '%{http_code}' -D "$UHDR" -o "$BIN_DIR/accept-p01-4-unknown.json" --max-time 5 "http://127.0.0.1:18881$path")
  UBODY=$(cat "$BIN_DIR/accept-p01-4-unknown.json")
  expect_eq "unknown-api$path-status-404" "$UCODE" "404"
  expect_contains "unknown-api$path-code" "$UBODY" '"code":"NOT_FOUND"'
  expect_contains "unknown-api$path-json" "$(header "$UHDR" Content-Type)" "application/json"
  expect_not_contains "unknown-api$path-not-index" "$UBODY" 'id="app"'
done

# 未知 API 用 POST 也必须 404 JSON，不能因为“方法不匹配”而落到静态回退。
UHDR="$BIN_DIR/accept-p01-4-unknown-post.headers"
UCODE=$(curl -s -w '%{http_code}' -X POST -D "$UHDR" -o "$BIN_DIR/accept-p01-4-unknown-post.json" --max-time 5 \
  http://127.0.0.1:18881/api/v1/unknown)
expect_eq "unknown-api-post-status-404" "$UCODE" "404"
expect_not_contains "unknown-api-post-not-index" "$(cat "$BIN_DIR/accept-p01-4-unknown-post.json")" 'id="app"'

# ---------------------------------------------------------------- 4. 405 与监听面
echo
echo "== 4. 方法限制、监听面与非 root =="
PHDR="$BIN_DIR/accept-p01-4-post-root.headers"
PCODE=$(curl -s -w '%{http_code}' -X POST -D "$PHDR" -o "$BIN_DIR/accept-p01-4-post-root.body" --max-time 5 http://127.0.0.1:18881/)
expect_eq "post-root-status-405" "$PCODE" "405"
expect_contains "post-root-allow-header" "$(header "$PHDR" Allow)" "GET"

PHDR2="$BIN_DIR/accept-p01-4-post-health.headers"
PCODE2=$(curl -s -w '%{http_code}' -X POST -D "$PHDR2" -o /dev/null --max-time 5 http://127.0.0.1:18881/api/v1/health/live)
expect_eq "post-health-status-405" "$PCODE2" "405"
expect_contains "post-health-allow-header" "$(header "$PHDR2" Allow)" "GET"

if command -v ss >/dev/null 2>&1; then
  LISTEN=$(ss -ltn 2>/dev/null)
  expect_contains "mock-listens-127.0.0.1-18881" "$LISTEN" "127.0.0.1:18881"
  expect_not_contains "mock-does-not-listen-wildcard" "$LISTEN" "0.0.0.0:18881"
  expect_not_contains "production-port-not-open-in-mock" "$LISTEN" "127.0.0.1:9443"
else
  notrun "listen-surface" "缺少 ss 命令"
fi

RUNNER_USER=$(id -un)
if [ "$(id -u)" -ne 0 ]; then pass "acceptance-runs-as-non-root"; else fail "acceptance-runs-as-non-root" "本脚本以 root 运行，无法证明 Web 以非 root 身份启动"; fi
WEB_USER=$(ps -o user= -p "$WEB_PID" 2>/dev/null | tr -d ' ')
if [ -n "$WEB_USER" ] && [ "$WEB_USER" != "root" ]; then
  pass "web-process-not-root"
else
  fail "web-process-not-root" "Web 进程用户为 '${WEB_USER:-未知}'"
fi
expect_eq "web-process-user-is-runner" "$WEB_USER" "$RUNNER_USER"

# mock 模式只允许固定地址：命令行覆盖与配置文件显式写错都必须拒绝。
"$WEB" --mode mock --listen 0.0.0.0:18881 --db "$MOCK_DB" > "$BIN_DIR/accept-p01-4-mock-wild.log" 2>&1
expect_eq "mock-refuses-wildcard-exit2" "$?" "2"
expect_contains "mock-refuses-wildcard-message" "$(cat "$BIN_DIR/accept-p01-4-mock-wild.log")" "mock 模式只能绑定 127.0.0.1:18881"

BAD_CFG="$BIN_DIR/accept-p01-4-mock-bad.json"
cat > "$BAD_CFG" <<JSON
{
  "web": {"mode": "mock", "listen_address": "0.0.0.0:18881"},
  "database": {"path": "$MOCK_DB"}
}
JSON
"$WEB" --config "$BAD_CFG" > "$BIN_DIR/accept-p01-4-mock-bad.log" 2>&1
expect_eq "mock-config-refuses-wildcard-exit2" "$?" "2"

kill -TERM "$WEB_PID" 2>/dev/null
wait "$WEB_PID" 2>/dev/null
expect_eq "mock-graceful-shutdown" "$?" "0"
WEB_PID=""

# ---------------------------------------------------------------- 5. production 模式
echo
echo "== 5. production 模式 =="
"$WEB" --config "$PROD_CFG" > "$BIN_DIR/accept-p01-4-web-prod.log" 2>&1 &
PROD_PID=$!
tries=0
until curl -s -o /dev/null --max-time 2 http://127.0.0.1:9443/api/v1/health/live; do
  tries=$((tries + 1))
  [ "$tries" -ge 30 ] && break
  sleep 0.2
done

PHDR3="$BIN_DIR/accept-p01-4-prod.headers"
PCODE3=$(curl -s -w '%{http_code}' -D "$PHDR3" -o "$BIN_DIR/accept-p01-4-prod.html" --max-time 5 http://127.0.0.1:9443/)
PROD_HTML=$(cat "$BIN_DIR/accept-p01-4-prod.html")
expect_eq "production-index-status-200" "$PCODE3" "200"
expect_contains "production-injects-production-mode" "$PROD_HTML" 'window.__UPANEL_MODE__="production"'
expect_not_contains "production-does-not-inject-mock" "$PROD_HTML" 'window.__UPANEL_MODE__="mock"'
expect_contains "production-api-health" "$(curl -s --max-time 5 http://127.0.0.1:9443/api/v1/health/live)" '"status":"ok"'
expect_contains "production-unknown-api-404" \
  "$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 http://127.0.0.1:9443/api/v1/unknown)" "404"

if command -v ss >/dev/null 2>&1; then
  LISTEN2=$(ss -ltn 2>/dev/null)
  expect_contains "production-listens-127.0.0.1-9443" "$LISTEN2" "127.0.0.1:9443"
  expect_not_contains "production-does-not-open-mock-port" "$LISTEN2" "127.0.0.1:18881"
fi

# production 在 TLS 就绪前只允许回环：非回环必须拒绝启动。
"$WEB" --config "$PROD_CFG" --listen 0.0.0.0:9443 > "$BIN_DIR/accept-p01-4-prod-wild.log" 2>&1
expect_eq "production-refuses-non-loopback-exit2" "$?" "2"
expect_contains "production-refuses-non-loopback-message" \
  "$(cat "$BIN_DIR/accept-p01-4-prod-wild.log")" "只能绑定回环地址"

kill -TERM "$PROD_PID" 2>/dev/null
wait "$PROD_PID" 2>/dev/null
expect_eq "production-graceful-shutdown" "$?" "0"
PROD_PID=""

# ---------------------------------------------------------------- 6. systemd 单元
echo
echo "== 6. systemd 单元模板 =="
if [ ! -f "$UNIT_SRC" ]; then
  fail "systemd-unit-present" "缺少 $UNIT_SRC（DEPLOY_DIR 应为包含 systemd/ 的 deploy 目录）"
else
  pass "systemd-unit-present"
  UNIT_TEXT=$(cat "$UNIT_SRC")
  expect_contains "unit-user-not-root" "$UNIT_TEXT" "User=upanel-web"
  expect_contains "unit-execstart-web" "$UNIT_TEXT" "ExecStart=/usr/local/bin/upanel-web"
  expect_contains "unit-hardening-nnp" "$UNIT_TEXT" "NoNewPrivileges=yes"
  expect_contains "unit-hardening-protect-system" "$UNIT_TEXT" "ProtectSystem=strict"
  expect_contains "unit-hardening-protect-home" "$UNIT_TEXT" "ProtectHome=yes"
  expect_contains "unit-state-directory" "$UNIT_TEXT" "StateDirectory=upanel"
  expect_not_contains "unit-no-root-user" "$UNIT_TEXT" "User=root"
fi

# ---------------------------------------------------------------- 7. 真实 systemd 安装启动
echo
echo "== 7. 真实 systemd 安装与启动 =="
if [ "$INSTALL_SYSTEMD" != "1" ]; then
  notrun "systemd-install-start" "INSTALL_SYSTEMD=0，按调用方要求跳过"
elif [ ! -d /run/systemd/system ] || ! command -v systemctl >/dev/null 2>&1; then
  notrun "systemd-install-start" "当前环境不是 systemd 引导（无 /run/systemd/system）"
elif [ -n "$SUDO" ] && ! $SUDO -n true 2>/dev/null; then
  notrun "systemd-install-start" "非 root 且 sudo 需要密码（脚本非交互）"
else
  SYS_CFG=/etc/upanel/upanel.json
  $SUDO install -d -m 0755 /etc/upanel
  $SUDO install -m 0755 "$WEB" /usr/local/bin/upanel-web
  $SUDO install -m 0755 "$CTL" /usr/local/bin/upanelctl
  cat > "$BIN_DIR/accept-p01-4-sys.json" <<JSON
{
  "web": {"mode": "production", "listen_address": "127.0.0.1:9443", "shutdown_timeout_seconds": 10},
  "agent": {"socket_path": "/run/upanel/agent.sock", "shutdown_timeout_seconds": 10},
  "database": {"path": "/var/lib/upanel/upanel.db", "busy_timeout_ms": 5000},
  "log": {"level": "info", "format": "json"}
}
JSON
  $SUDO install -m 0644 "$BIN_DIR/accept-p01-4-sys.json" "$SYS_CFG"

  if ! id -u upanel-web >/dev/null 2>&1; then
    $SUDO useradd --system --home-dir /var/lib/upanel --no-create-home \
      --shell /usr/sbin/nologin upanel-web
  fi
  pass "systemd-user-exists"
  # StateDirectory 在首次启动时才创建；迁移需要先有可写目录。
  $SUDO install -d -m 0750 -o upanel-web -g upanel-web /var/lib/upanel

  $SUDO -u upanel-web /usr/local/bin/upanelctl migrate --config "$SYS_CFG" \
    > "$BIN_DIR/accept-p01-4-sys-migrate.log" 2>&1
  expect_eq "systemd-migrate-as-service-user-exit0" "$?" "0"
  $SUDO install -m 0644 "$UNIT_SRC" /etc/systemd/system/upanel-web.service
  $SUDO systemctl daemon-reload

  # 单元语法与引用校验（此时 /usr/local/bin 下的二进制已安装，避免“缺少可执行文件”误报）。
  if command -v systemd-analyze >/dev/null 2>&1; then
    VERIFY_OUT=$(systemd-analyze verify /etc/systemd/system/upanel-web.service 2>&1)
    VERIFY_RC=$?
    expect_eq "systemd-analyze-verify-exit0" "$VERIFY_RC" "0"
    if [ -n "$VERIFY_OUT" ]; then printf '      verify 输出:\n%s\n' "$VERIFY_OUT"; fi
  else
    notrun "systemd-analyze-verify" "系统中没有 systemd-analyze"
  fi

  $SUDO systemctl enable upanel-web > "$BIN_DIR/accept-p01-4-systemctl-enable.log" 2>&1
  $SUDO systemctl restart upanel-web
  tries=0
  until curl -s -o /dev/null --max-time 2 http://127.0.0.1:9443/api/v1/health/live; do
    tries=$((tries + 1))
    [ "$tries" -ge 40 ] && break
    sleep 0.25
  done

  ACTIVE=$($SUDO systemctl is-active upanel-web 2>/dev/null)
  expect_eq "systemd-service-active" "$ACTIVE" "active"

  SHDR="$BIN_DIR/accept-p01-4-systemd.headers"
  SCODE=$(curl -s -w '%{http_code}' -D "$SHDR" -o "$BIN_DIR/accept-p01-4-systemd.html" --max-time 5 http://127.0.0.1:9443/)
  expect_eq "systemd-service-serves-index-200" "$SCODE" "200"
  expect_contains "systemd-service-injects-production" \
    "$(cat "$BIN_DIR/accept-p01-4-systemd.html")" 'window.__UPANEL_MODE__="production"'

  MAINPID=$($SUDO systemctl show -p MainPID --value upanel-web 2>/dev/null | tr -d ' ')
  if [ -n "$MAINPID" ] && [ "$MAINPID" != "0" ]; then
    pass "systemd-mainpid-reports"
    MAINUSER=$(ps -o user= -p "$MAINPID" 2>/dev/null | tr -d ' ')
    expect_eq "systemd-service-runs-as-upanel-web" "$MAINUSER" "upanel-web"
    if [ "$MAINPID" -eq 1 ]; then fail "systemd-service-not-pid1" "MainPID 为 1"; else pass "systemd-service-not-pid1"; fi
  else
    fail "systemd-mainpid-reports" "MainPID 为 '${MAINPID:-空}'"
  fi

  UNIT_USER=$($SUDO systemctl show -p User --value upanel-web 2>/dev/null | tr -d ' ')
  expect_eq "systemd-unit-user-property" "$UNIT_USER" "upanel-web"

  # 数据库文件必须落在 StateDirectory 下，且归属服务用户。
  # /var/lib/upanel 为 0750 且归属服务用户，普通用户读不到，必须用 sudo 检查。
  if $SUDO test -f /var/lib/upanel/upanel.db; then
    pass "systemd-db-file-present"
    expect_eq "systemd-db-owner" "$($SUDO stat -c '%U' /var/lib/upanel/upanel.db)" "upanel-web"
    expect_eq "systemd-db-mode-inaccessible-to-others" \
      "$($SUDO stat -c '%a' /var/lib/upanel)" "750"
  else
    fail "systemd-db-file-present" "/var/lib/upanel/upanel.db 不存在"
  fi

  $SUDO systemctl stop upanel-web
  expect_eq "systemd-service-stopped-inactive" "$($SUDO systemctl is-active upanel-web 2>/dev/null)" "inactive"
  # 停止后端口必须释放，不能留下僵尸监听。
  if command -v ss >/dev/null 2>&1; then
    expect_not_contains "systemd-port-released-after-stop" "$(ss -ltn 2>/dev/null)" "127.0.0.1:9443"
  fi
  $SUDO systemctl disable upanel-web > /dev/null 2>&1
  $SUDO rm -f /etc/systemd/system/upanel-web.service
  $SUDO systemctl daemon-reload
  pass "systemd-unit-removed-after-test"
fi

# ---------------------------------------------------------------- 汇总
echo
printf '通过: %d  失败: %d  未执行: %d\n' "$PASSED" "$FAILED" "$NOTRUN"
if [ "$FAILED" -gt 0 ]; then
  printf '失败项:%s\n' "$FAILED_NAMES"
  exit 1
fi
exit 0
