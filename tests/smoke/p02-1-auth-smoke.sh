#!/usr/bin/env bash
# P02.1 认证最小链路冒烟（可在开发宿主运行，仅使用回环地址与临时数据库）。
#
# 覆盖：init-admin 创建首个管理员、登录入口四道检查（Content-Type / X-UPanel-Request /
# Origin / JSON 严格解析）、密码错误 401、连续失败 429、Cookie 属性、session 轮换 CSRF、
# 退出需要 CSRF、退出后会话立即失效、日志里不出现明文密码。
#
# 不做的事：不在本机验证 systemd、不在本机做磁盘/网络操作。
# 真实 Ubuntu 上的验收见 tests/integration/p02-1-acceptance.sh。
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT" || exit 1

PORT=18881
BASE="http://127.0.0.1:${PORT}"
ORIGIN="http://127.0.0.1:${PORT}"
PASSWORD="correct-horse-9"
WORKDIR="${UPANEL_SMOKE_DIR:-$(python -c 'import tempfile;print(tempfile.mkdtemp(prefix="upanel-p021-"))')}"
DB="${WORKDIR}/upanel.db"
WEB_PID=""

PASS=0
FAIL=0
pass() { PASS=$((PASS + 1)); printf 'PASS  %s\n' "$1"; }
fail() { FAIL=$((FAIL + 1)); printf 'FAIL  %s\n' "$1"; }

cleanup() {
	if [ -n "$WEB_PID" ] && kill -0 "$WEB_PID" 2>/dev/null; then
		kill "$WEB_PID" 2>/dev/null
		wait "$WEB_PID" 2>/dev/null
	fi
}
trap cleanup EXIT

expect_eq() {
	local name="$1" want="$2" got="$3"
	if [ "$want" = "$got" ]; then pass "$name"; else fail "$name（期望 $want，实际 $got）"; fi
}

expect_contains() {
	local name="$1" needle="$2" haystack="$3"
	case "$haystack" in
	*"$needle"*) pass "$name" ;;
	*) fail "$name（缺少 $needle）" ;;
	esac
}

expect_not_contains() {
	local name="$1" needle="$2" haystack="$3"
	case "$haystack" in
	*"$needle"*) fail "$name（不应出现 $needle）" ;;
	*) pass "$name" ;;
	esac
}

echo "== 0. 构建 =="
mkdir -p "$WORKDIR"
go build -o "$WORKDIR/upanel-web" ./cmd/upanel-web || { echo "构建 upanel-web 失败"; exit 1; }
go build -o "$WORKDIR/upanelctl" ./cmd/upanelctl || { echo "构建 upanelctl 失败"; exit 1; }

echo "== 1. init-admin 创建第一个管理员 =="
INIT_OUT="$(printf '%s\n%s\n' "$PASSWORD" "$PASSWORD" | "$WORKDIR/upanelctl" init-admin --db "$DB" --json 2>"$WORKDIR/init.stderr")"
INIT_CODE=$?
expect_eq "init-admin 退出码" "0" "$INIT_CODE"
expect_contains "init-admin 输出包含 admin" '"username": "admin"' "$INIT_OUT"
expect_not_contains "init-admin 输出不含密码" "$PASSWORD" "$INIT_OUT"
expect_not_contains "init-admin stderr 不含密码" "$PASSWORD" "$(cat "$WORKDIR/init.stderr" 2>/dev/null)"

SECOND_CODE=$(printf '%s\n%s\n' "$PASSWORD" "$PASSWORD" | "$WORKDIR/upanelctl" init-admin --db "$DB" >/dev/null 2>"$WORKDIR/init2.stderr"; echo $?)
expect_eq "重复 init-admin 被拒绝" "1" "$SECOND_CODE"
expect_contains "重复 init-admin 说明原因" "只用于首次初始化" "$(cat "$WORKDIR/init2.stderr")"

echo "== 2. 启动 mock 模式 Web 服务 =="
"$WORKDIR/upanel-web" --mode mock --db "$DB" >"$WORKDIR/web.log" 2>&1 &
WEB_PID=$!
for _ in $(seq 1 50); do
	if curl -fsS "$BASE/api/v1/health/live" >/dev/null 2>&1; then break; fi
	command sleep 0.2 2>/dev/null || sleep 0.2
done
live=$(curl -s -o /dev/null -w '%{http_code}' "$BASE/api/v1/health/live")
expect_eq "健康检查" "200" "$live"

echo "== 3. 登录入口检查 =="
code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$BASE/api/v1/auth/login" \
	-H 'Content-Type: application/json' -H "Origin: $ORIGIN" \
	-d "{\"username\":\"admin\",\"password\":\"$PASSWORD\"}")
expect_eq "缺少 X-UPanel-Request → 400" "400" "$code"

code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$BASE/api/v1/auth/login" \
	-H 'Content-Type: text/plain' -H 'X-UPanel-Request: 1' -H "Origin: $ORIGIN" \
	-d "{\"username\":\"admin\",\"password\":\"$PASSWORD\"}")
expect_eq "非 JSON Content-Type → 400" "400" "$code"

code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$BASE/api/v1/auth/login" \
	-H 'Content-Type: application/json' -H 'X-UPanel-Request: 1' -H 'Origin: http://evil.example.com' \
	-d "{\"username\":\"admin\",\"password\":\"$PASSWORD\"}")
expect_eq "陌生 Origin → 403" "403" "$code"

NO_ORIGIN_BODY=$(curl -s -X POST "$BASE/api/v1/auth/login" \
	-H 'Content-Type: application/json' -H 'X-UPanel-Request: 1' \
	-d "{\"username\":\"admin\",\"password\":\"$PASSWORD\"}")
expect_contains "缺少 Origin 返回 CSRF_FAILED" "CSRF_FAILED" "$NO_ORIGIN_BODY"

UNKNOWN_JSON=$(curl -s -X POST "$BASE/api/v1/auth/login" \
	-H 'Content-Type: application/json' -H 'X-UPanel-Request: 1' -H "Origin: $ORIGIN" \
	-d "{\"username\":\"admin\",\"password\":\"$PASSWORD\",\"admin\":true}")
expect_contains "未知字段被拒绝" "VALIDATION_ERROR" "$UNKNOWN_JSON"

code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$BASE/api/v1/auth/login" \
	-H 'Content-Type: application/json' -H 'X-UPanel-Request: 1' -H "Origin: $ORIGIN" \
	-d '{"username":"admin","password":"wrong-password-1"}')
expect_eq "错误密码 → 401" "401" "$code"

echo "== 4. 正确登录与 Cookie 属性 =="
LOGIN_HEADERS="$WORKDIR/login.headers"
LOGIN_BODY="$WORKDIR/login.json"
code=$(curl -s -D "$LOGIN_HEADERS" -o "$LOGIN_BODY" -w '%{http_code}' -X POST "$BASE/api/v1/auth/login" \
	-H 'Content-Type: application/json; charset=utf-8' -H 'X-UPanel-Request: 1' -H "Origin: $ORIGIN" \
	-d "{\"username\":\"admin\",\"password\":\"$PASSWORD\"}")
expect_eq "正确密码 → 200" "200" "$code"

SET_COOKIE="$(grep -i '^set-cookie:' "$LOGIN_HEADERS" | tr -d '\r')"
expect_contains "Cookie 名为 __Host-upanel_session" "__Host-upanel_session=" "$SET_COOKIE"
expect_contains "Cookie HttpOnly" "HttpOnly" "$SET_COOKIE"
expect_contains "Cookie Secure" "Secure" "$SET_COOKIE"
expect_contains "Cookie SameSite=Strict" "SameSite=Strict" "$SET_COOKIE"
expect_contains "Cookie Path=/" "Path=/" "$SET_COOKIE"
expect_not_contains "Cookie 不带 Domain" "Domain=" "$SET_COOKIE"
expect_not_contains "登录响应不含密码" "$PASSWORD" "$(cat "$LOGIN_BODY")"
expect_not_contains "登录响应不含哈希" '$argon2id$' "$(cat "$LOGIN_BODY")"

CSRF="$(python -c 'import json,sys;print(json.load(open(sys.argv[1]))["data"]["csrf_token"])' "$LOGIN_BODY")"
COOKIE="$(printf '%s' "$SET_COOKIE" | sed -e 's/^[Ss]et-[Cc]ookie: //' -e 's/;.*//')"
if [ -n "$CSRF" ] && [ -n "$COOKIE" ]; then pass "拿到会话 Cookie 与 CSRF 令牌"; else fail "登录响应缺少令牌"; fi

echo "== 5. session 读取与 CSRF 轮换 =="
SESSION_BODY="$WORKDIR/session.json"
code=$(curl -s -o "$SESSION_BODY" -w '%{http_code}' "$BASE/api/v1/auth/session" -H "Cookie: $COOKIE")
expect_eq "GET session → 200" "200" "$code"
NEW_CSRF="$(python -c 'import json,sys;print(json.load(open(sys.argv[1]))["data"]["csrf_token"])' "$SESSION_BODY")"
if [ "$NEW_CSRF" != "$CSRF" ] && [ -n "$NEW_CSRF" ]; then pass "CSRF 令牌已轮换"; else fail "CSRF 令牌未轮换"; fi
expect_contains "reauth_expires_at 为 null" '"reauth_expires_at":null' "$(cat "$SESSION_BODY")"

code=$(curl -s -o /dev/null -w '%{http_code}' "$BASE/api/v1/auth/session")
expect_eq "无 Cookie 访问 session → 401" "401" "$code"

code=$(curl -s -o /dev/null -w '%{http_code}' "$BASE/api/v1/auth/session" -H 'Cookie: __Host-upanel_session=deadbeef')
expect_eq "伪造 Cookie → 401" "401" "$code"

echo "== 6. 退出需要 CSRF，且立即失效 =="
code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$BASE/api/v1/auth/logout" \
	-H "Cookie: $COOKIE" -H "Origin: $ORIGIN")
expect_eq "无 CSRF 头退出 → 403" "403" "$code"

code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$BASE/api/v1/auth/logout" \
	-H "Cookie: $COOKIE" -H "Origin: $ORIGIN" -H "X-CSRF-Token: $CSRF")
expect_eq "旧 CSRF 令牌退出 → 403" "403" "$code"

LOGOUT_HEADERS="$WORKDIR/logout.headers"
code=$(curl -s -D "$LOGOUT_HEADERS" -o /dev/null -w '%{http_code}' -X POST "$BASE/api/v1/auth/logout" \
	-H "Cookie: $COOKIE" -H "Origin: $ORIGIN" -H "X-CSRF-Token: $NEW_CSRF")
expect_eq "正确 CSRF 退出 → 200" "200" "$code"
expect_contains "退出清除 Cookie" "Max-Age=0" "$(grep -i '^set-cookie:' "$LOGOUT_HEADERS" | tr -d '\r')"

code=$(curl -s -o /dev/null -w '%{http_code}' "$BASE/api/v1/auth/session" -H "Cookie: $COOKIE")
expect_eq "退出后同一 Cookie → 401" "401" "$code"

echo "== 7. 连续失败触发限速（429） =="
LIMITED=""
for _ in $(seq 1 10); do
	code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$BASE/api/v1/auth/login" \
		-H 'Content-Type: application/json' -H 'X-UPanel-Request: 1' -H "Origin: $ORIGIN" \
		-d '{"username":"admin","password":"wrong-password-1"}')
	if [ "$code" = "429" ]; then LIMITED="yes"; break; fi
done
expect_eq "连续失败后返回 429" "yes" "$LIMITED"

LIMIT_BODY=$(curl -s -X POST "$BASE/api/v1/auth/login" \
	-H 'Content-Type: application/json' -H 'X-UPanel-Request: 1' -H "Origin: $ORIGIN" \
	-d '{"username":"admin","password":"wrong-password-1"}')
expect_contains "限速响应带 retry_after_seconds" "retry_after_seconds" "$LIMIT_BODY"

echo "== 8. 日志不含秘密 =="
expect_not_contains "web 日志不含密码" "$PASSWORD" "$(cat "$WORKDIR/web.log")"
expect_not_contains "web 日志不含 CSRF 令牌" "$CSRF" "$(cat "$WORKDIR/web.log")"
expect_contains "web 日志记录了登录请求" "/api/v1/auth/login" "$(cat "$WORKDIR/web.log")"

echo
printf '结果: PASS=%d FAIL=%d\n' "$PASS" "$FAIL"
printf '工作目录（含日志与响应）: %s\n' "$WORKDIR"
[ "$FAIL" -eq 0 ]
