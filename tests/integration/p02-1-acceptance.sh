#!/bin/sh
# P02.1 Linux 验收脚本：认证链路在真实 Ubuntu 上的行为。
#
# 运行位置：Ubuntu 22.04/24.04 验收 VM 内，用**非 root** 用户运行（脚本会断言这一点）。
# 不依赖 Go 工具链：使用宿主交叉编译好的 Linux 二进制。
#
# 用法：
#   BIN_DIR=/tmp/upanel-bin sh tests/integration/p02-1-acceptance.sh
#
# 退出码：0 全部通过；1 有失败项。
#
# 覆盖的验收点（对应 CONTRACTS §2 + STAGES.md P02 认证部分）：
#   1. 交付的 systemd 单元不以 root 运行 Web
#   2. upanelctl init-admin 创建首个管理员；非终端两行输入必须一致；已有账号时拒绝
#   3. production 模式在 127.0.0.1:9443 明文启动，进程与库文件都不属于 root
#   4. 登录入口四道检查（JSON Content-Type / X-UPanel-Request / Origin / 未知字段）
#   5. Cookie 属性（__Host- 前缀、HttpOnly、Secure、SameSite=Strict、Path=/、无 Domain）
#   6. 会话读取、CSRF 轮换、无 Cookie / 伪造 Cookie 均 401
#   7. 进程重启后会话仍有效（会话在库里，不在内存）
#   8. 退出必须带 CSRF，退出后同一 Cookie 立即 401
#   9. 连续失败触发 429 且带 retry_after_seconds
#  10. 数据库文件里没有明文口令，也没有明文会话令牌或 CSRF 令牌
#  11. 日志里没有口令与令牌

set -u

BIN_DIR=${BIN_DIR:-/tmp/upanel-bin}
WEB="$BIN_DIR/upanel-web"
AGENT="$BIN_DIR/upanel-agent"
CTL="$BIN_DIR/upanelctl"

ROOT=$(cd "$(dirname "$0")/../.." && pwd)
WORK=${WORK:-/var/tmp/upanel-accept-p02-1}
DB="$WORK/upanel.db"
PORT=9443
BASE="http://127.0.0.1:$PORT"
ORIGIN="$BASE"
PASSWORD='vm-correct-horse-9'
LOGDIR="$WORK/logs"

PASSED=0
FAILED=0
FAILED_NAMES=""
WEB_PID=""

pass() { PASSED=$((PASSED + 1)); printf 'PASS  %s\n' "$1"; }
fail() { FAILED=$((FAILED + 1)); FAILED_NAMES="$FAILED_NAMES | $1"; printf 'FAIL  %s: %s\n' "$1" "${2:-}"; }

expect_eq() { if [ "$2" = "$3" ]; then pass "$1"; else fail "$1" "期望 '$3'，实际 '$2'"; fi; }

expect_contains() {
  case $3 in
    *"$2"*) pass "$1" ;;
    *) fail "$1" "缺少 '$2'（实际: $3）" ;;
  esac
}

expect_not_contains() {
  case $3 in
    *"$2"*) fail "$1" "不应出现 '$2'" ;;
    *) pass "$1" ;;
  esac
}

json_field() {
  python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["data"][sys.argv[2]])' "$1" "$2" 2>/dev/null
}

cleanup() {
  if [ -n "$WEB_PID" ] && kill -0 "$WEB_PID" 2>/dev/null; then
    kill -TERM "$WEB_PID" 2>/dev/null
    wait "$WEB_PID" 2>/dev/null
  fi
}
trap cleanup EXIT INT TERM

start_web() {
  "$WEB" --mode production --db "$DB" --listen "127.0.0.1:$PORT" >>"$LOGDIR/web.log" 2>&1 &
  WEB_PID=$!
  for _ in $(seq 1 100); do
    if curl -fsS "$BASE/api/v1/health/live" >/dev/null 2>&1; then return 0; fi
    sleep 0.2
  done
  return 1
}

stop_web() {
  if [ -n "$WEB_PID" ]; then
    kill -TERM "$WEB_PID" 2>/dev/null
    wait "$WEB_PID" 2>/dev/null
    WEB_PID=""
  fi
}

login() {
  # login <password> ; 输出 http 状态码，响应体与头写入 $WORK
  curl -s -D "$WORK/login.headers" -o "$WORK/login.json" -w '%{http_code}' \
    -X POST "$BASE/api/v1/auth/login" \
    -H 'Content-Type: application/json' -H 'X-UPanel-Request: 1' -H "Origin: $ORIGIN" \
    -d "{\"username\":\"admin\",\"password\":\"$1\"}"
}

echo "== 0. 环境与交付物 =="
[ "$(id -u)" -ne 0 ] && pass "验收脚本以非 root 用户运行（$(id -un)）" || fail "验收脚本以非 root 用户运行" "当前是 root"
for b in "$WEB" "$AGENT" "$CTL"; do
  if [ -x "$b" ]; then pass "二进制存在且可执行 $(basename "$b")"; else fail "二进制存在且可执行 $(basename "$b")" "$b"; fi
done
VERSION_OUT=$("$CTL" version 2>&1)
VERSION_CODE=$?
expect_eq "upanelctl version 退出码" "0" "$VERSION_CODE"
expect_contains "version 输出含程序名" "upanelctl " "$VERSION_OUT"

UNIT="$ROOT/deploy/systemd/upanel-web.service"
if [ -f "$UNIT" ]; then
  UNIT_BODY=$(cat "$UNIT")
  expect_contains "systemd 单元指定非 root 用户" "User=upanel-web" "$UNIT_BODY"
  expect_not_contains "systemd 单元不以 root 运行" "User=root" "$UNIT_BODY"
  expect_not_contains "systemd 单元不含万能 shell" "ExecStart=/bin/sh" "$UNIT_BODY"
else
  fail "systemd 单元存在" "$UNIT 缺失"
fi

rm -rf "$WORK"
mkdir -p "$LOGDIR" || { echo "无法创建 $LOGDIR"; exit 1; }

echo "== 1. init-admin =="
INIT_OUT=$(printf '%s\n%s\n' "$PASSWORD" "$PASSWORD" | "$CTL" init-admin --db "$DB" --username admin --display-name "管理员" --json 2>"$WORK/init.err")
INIT_CODE=$?
expect_eq "init-admin 退出码" "0" "$INIT_CODE"
expect_contains "输出含用户名 admin" '"username": "admin"' "$INIT_OUT"
expect_not_contains "输出不含明文口令" "$PASSWORD" "$INIT_OUT"
expect_not_contains "stderr 不含明文口令" "$PASSWORD" "$(cat "$WORK/init.err" 2>/dev/null)"

MISMATCH_CODE=$(printf '%s\n%s\n' "$PASSWORD" "${PASSWORD}x" | "$CTL" init-admin --db "$WORK/mismatch.db" --username admin >/dev/null 2>"$WORK/mismatch.err"; echo $?)
expect_eq "两次输入不一致时拒绝" "1" "$MISMATCH_CODE"

DUP_CODE=$(printf '%s\n%s\n' "$PASSWORD" "$PASSWORD" | "$CTL" init-admin --db "$DB" --username second >/dev/null 2>"$WORK/dup.err"; echo $?)
expect_eq "库里已有账号时拒绝再次初始化" "1" "$DUP_CODE"
expect_contains "拒绝原因可读" "只用于首次初始化" "$(cat "$WORK/dup.err")"

echo "== 2. production 启动与非 root 身份 =="
if start_web; then pass "production 模式在 127.0.0.1:9443 启动"; else fail "production 模式启动" "$(tail -5 "$LOGDIR/web.log" 2>/dev/null)"; fi
live=$(curl -s -o /dev/null -w '%{http_code}' "$BASE/api/v1/health/live")
expect_eq "健康检查" "200" "$live"

WEB_UID=$(ps -o uid= -p "$WEB_PID" 2>/dev/null | tr -d ' ')
if [ -n "$WEB_UID" ] && [ "$WEB_UID" != "0" ]; then pass "Web 进程 uid=$WEB_UID（非 root）"; else fail "Web 进程不以 root 运行" "uid=$WEB_UID"; fi

DB_OWNER=$(stat -c %U "$DB" 2>/dev/null)
expect_eq "数据库文件属于运行用户" "$(id -un)" "$DB_OWNER"
DB_MODE=$(stat -c %a "$DB" 2>/dev/null)
case $DB_MODE in
  600|640|660) pass "数据库文件权限不向其他用户开放（$DB_MODE）" ;;
  *) fail "数据库文件权限不向其他用户开放" "mode=$DB_MODE" ;;
esac

echo "== 3. 登录入口检查 =="
code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$BASE/api/v1/auth/login" \
  -H 'Content-Type: application/json' -H "Origin: $ORIGIN" \
  -d "{\"username\":\"admin\",\"password\":\"$PASSWORD\"}")
expect_eq "缺少 X-UPanel-Request → 未通过" "400" "$code"

code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$BASE/api/v1/auth/login" \
  -H 'Content-Type: text/plain' -H 'X-UPanel-Request: 1' -H "Origin: $ORIGIN" \
  -d "{\"username\":\"admin\",\"password\":\"$PASSWORD\"}")
expect_eq "非 JSON Content-Type → 未通过" "400" "$code"

code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$BASE/api/v1/auth/login" \
  -H 'Content-Type: application/json' -H 'X-UPanel-Request: 1' -H 'Origin: http://evil.example.com' \
  -d "{\"username\":\"admin\",\"password\":\"$PASSWORD\"}")
expect_eq "陌生 Origin → 403" "403" "$code"

NO_ORIGIN=$(curl -s -X POST "$BASE/api/v1/auth/login" \
  -H 'Content-Type: application/json' -H 'X-UPanel-Request: 1' \
  -d "{\"username\":\"admin\",\"password\":\"$PASSWORD\"}")
expect_contains "缺少 Origin 返回 CSRF_FAILED" "CSRF_FAILED" "$NO_ORIGIN"

UNKNOWN=$(curl -s -X POST "$BASE/api/v1/auth/login" \
  -H 'Content-Type: application/json' -H 'X-UPanel-Request: 1' -H "Origin: $ORIGIN" \
  -d "{\"username\":\"admin\",\"password\":\"$PASSWORD\",\"role\":\"admin\"}")
expect_contains "未知字段被拒绝" "VALIDATION_ERROR" "$UNKNOWN"

code=$(login 'wrong-password-1')
expect_eq "错误密码 → 401" "401" "$code"
expect_not_contains "错误密码响应不泄露账号是否存在" "不存在" "$(cat "$WORK/login.json")"

echo "== 4. 正确登录与 Cookie 属性 =="
code=$(login "$PASSWORD")
expect_eq "正确口令 → 200" "200" "$code"
SET_COOKIE=$(grep -i '^set-cookie:' "$WORK/login.headers" | tr -d '\r')
expect_contains "Cookie 名 __Host-upanel_session" "__Host-upanel_session=" "$SET_COOKIE"
expect_contains "Cookie HttpOnly" "HttpOnly" "$SET_COOKIE"
expect_contains "Cookie Secure" "Secure" "$SET_COOKIE"
expect_contains "Cookie SameSite=Strict" "SameSite=Strict" "$SET_COOKIE"
expect_contains "Cookie Path=/" "Path=/" "$SET_COOKIE"
expect_not_contains "Cookie 不带 Domain" "Domain=" "$SET_COOKIE"
expect_not_contains "登录响应不含口令" "$PASSWORD" "$(cat "$WORK/login.json")"
expect_not_contains "登录响应不含哈希" '$argon2id$' "$(cat "$WORK/login.json")"

CSRF=$(json_field "$WORK/login.json" csrf_token)
COOKIE=$(printf '%s' "$SET_COOKIE" | sed -e 's/^[Ss]et-[Cc]ookie: //' -e 's/;.*//')
[ -n "$COOKIE" ] && [ -n "$CSRF" ] && pass "拿到会话 Cookie 与 CSRF 令牌" || fail "拿到会话 Cookie 与 CSRF 令牌"

echo "== 5. 会话读取与 CSRF 轮换 =="
code=$(curl -s -o "$WORK/session.json" -w '%{http_code}' "$BASE/api/v1/auth/session" -H "Cookie: $COOKIE")
expect_eq "GET session → 200" "200" "$code"
NEW_CSRF=$(json_field "$WORK/session.json" csrf_token)
[ -n "$NEW_CSRF" ] && [ "$NEW_CSRF" != "$CSRF" ] && pass "CSRF 令牌已轮换" || fail "CSRF 令牌已轮换"
expect_contains "reauth_expires_at 为 null" '"reauth_expires_at":null' "$(cat "$WORK/session.json")"

code=$(curl -s -o /dev/null -w '%{http_code}' "$BASE/api/v1/auth/session")
expect_eq "无 Cookie 读 session → 401" "401" "$code"
code=$(curl -s -o /dev/null -w '%{http_code}' "$BASE/api/v1/auth/session" -H 'Cookie: __Host-upanel_session=deadbeef')
expect_eq "伪造 Cookie → 401" "401" "$code"

echo "== 6. 进程重启后会话仍然有效 =="
TOKEN_AT_RESTART="$COOKIE"
stop_web
if start_web; then pass "重启后服务恢复"; else fail "重启后服务恢复" "$(tail -5 "$LOGDIR/web.log" 2>/dev/null)"; fi
code=$(curl -s -o "$WORK/session2.json" -w '%{http_code}' "$BASE/api/v1/auth/session" -H "Cookie: $TOKEN_AT_RESTART")
expect_eq "重启前的会话仍可用" "200" "$code"
RESTART_CSRF=$(json_field "$WORK/session2.json" csrf_token)

echo "== 7. 退出 =="
code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$BASE/api/v1/auth/logout" -H "Cookie: $TOKEN_AT_RESTART" -H "Origin: $ORIGIN")
expect_eq "无 CSRF 头退出 → 403" "403" "$code"
code=$(curl -s -D "$WORK/logout.headers" -o /dev/null -w '%{http_code}' -X POST "$BASE/api/v1/auth/logout" \
  -H "Cookie: $TOKEN_AT_RESTART" -H "Origin: $ORIGIN" -H "X-CSRF-Token: $RESTART_CSRF")
expect_eq "正确 CSRF 退出 → 200" "200" "$code"
expect_contains "退出清除 Cookie" "Max-Age=0" "$(grep -i '^set-cookie:' "$WORK/logout.headers" | tr -d '\r')"
code=$(curl -s -o /dev/null -w '%{http_code}' "$BASE/api/v1/auth/session" -H "Cookie: $TOKEN_AT_RESTART")
expect_eq "退出后同一 Cookie → 401" "401" "$code"

echo "== 8. 连续失败触发限速 =="
LIMITED=""
for _ in $(seq 1 12); do
  code=$(login 'wrong-password-1')
  if [ "$code" = "429" ]; then LIMITED="yes"; break; fi
done
expect_eq "连续失败后返回 429" "yes" "$LIMITED"
LIMIT_BODY=$(curl -s -X POST "$BASE/api/v1/auth/login" \
  -H 'Content-Type: application/json' -H 'X-UPanel-Request: 1' -H "Origin: $ORIGIN" \
  -d '{"username":"admin","password":"wrong-password-1"}')
expect_contains "限速响应带 retry_after_seconds" "retry_after_seconds" "$LIMIT_BODY"

echo "== 9. 数据库里没有明文秘密 =="
# WAL 模式：数据库最新内容可能还在 -wal 里，两个文件一起看。
DB_BLOB=$(cat "$DB" "$DB-wal" 2>/dev/null | tr -d '\000')
expect_not_contains "库里没有明文口令" "$PASSWORD" "$DB_BLOB"
expect_contains "库里保存的是 Argon2id 哈希" 'argon2id' "$DB_BLOB"
expect_not_contains "库里没有明文会话令牌" "$(printf '%s' "$COOKIE" | sed 's/^__Host-upanel_session=//')" "$DB_BLOB"
expect_not_contains "库里没有明文 CSRF 令牌" "$NEW_CSRF" "$DB_BLOB"

echo "== 10. 日志边界 =="
WEB_LOG=$(cat "$LOGDIR/web.log" 2>/dev/null)
expect_not_contains "Web 日志不含口令" "$PASSWORD" "$WEB_LOG"
expect_not_contains "Web 日志不含 CSRF 令牌" "$NEW_CSRF" "$WEB_LOG"
expect_contains "Web 日志记录了登录请求" "/api/v1/auth/login" "$WEB_LOG"

echo
printf '结果: PASS=%d FAIL=%d\n' "$PASSED" "$FAILED"
if [ "$FAILED" -ne 0 ]; then printf '失败项:%s\n' "$FAILED_NAMES"; fi
printf '工作目录: %s\n' "$WORK"
[ "$FAILED" -eq 0 ]
