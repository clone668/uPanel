#!/usr/bin/env bash
# P02.2 账号管理冒烟（可在开发宿主运行，仅使用回环地址与临时数据库）。
#
# 覆盖：admin/viewer 权限边界、账号增删改查、字段级脱敏 DTO、limit/cursor 分页、
# 乐观并发 version（409）、自我提权拒绝（403）、再认证窗口（403 REAUTH_REQUIRED →
# 403 错误密码 → 200 有效窗口）、改密码与删账号使目标会话立即失效、最后管理员不变式、
# 审计记录追加且不含口令、日志不含秘密。
#
# 不做的事：不在本机验证 systemd 或真实 Linux 服务；最后管理员不变式在接口层表现为
# 自我修改 403（actor 自己就是管理员，因此 422 分支只能由服务层单测覆盖）。
# 真实 Ubuntu 上的验收见 tests/integration/p02-2-acceptance.sh。
set -uo pipefail
# 脚本里有中文比较：固定 Python 输出为 UTF-8，避免 Windows 控制台的 GBK 编码把
# 比较对象和输出弄成乱码（乱码会让断言假失败或假通过）。
export PYTHONIOENCODING=utf-8

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT" || exit 1

PORT=18881
BASE="http://127.0.0.1:${PORT}"
ORIGIN="http://127.0.0.1:${PORT}"
PASSWORD="correct-horse-9"
VIEWER_PASSWORD="viewer-horse-42"
VIEWER_NEW_PASSWORD="viewer-horse-77"
WORKDIR="${UPANEL_SMOKE_DIR:-$(python -c 'import tempfile;print(tempfile.mkdtemp(prefix="upanel-p022-"))')}"
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

# jget <file> <python 表达式，d 为解析后的 JSON>
jget() {
	python -c 'import json,sys
d=json.load(open(sys.argv[1], encoding="utf-8"))
print(eval(sys.argv[2]))' "$1" "$2"
}

# do_login <username> <password> <headers-file> <body-file> -> 打印状态码
do_login() {
	curl -s -D "$3" -o "$4" -w '%{http_code}' -X POST "$BASE/api/v1/auth/login" \
		-H 'Content-Type: application/json' -H 'X-UPanel-Request: 1' -H "Origin: $ORIGIN" \
		-d "{\"username\":\"$1\",\"password\":\"$2\"}"
}

# cookie_of <headers-file>
cookie_of() {
	grep -i '^set-cookie:' "$1" | tr -d '\r' | sed -e 's/^[Ss]et-[Cc]ookie: //' -e 's/;.*//'
}

echo "== 0. 构建 =="
mkdir -p "$WORKDIR"
go build -o "$WORKDIR/upanel-web" ./cmd/upanel-web || { echo "构建 upanel-web 失败"; exit 1; }
go build -o "$WORKDIR/upanelctl" ./cmd/upanelctl || { echo "构建 upanelctl 失败"; exit 1; }

echo "== 1. init-admin 与启动 mock 服务 =="
printf '%s\n%s\n' "$PASSWORD" "$PASSWORD" | "$WORKDIR/upanelctl" init-admin --db "$DB" >/dev/null 2>&1
expect_eq "init-admin 退出码" "0" "$?"

"$WORKDIR/upanel-web" --mode mock --db "$DB" >"$WORKDIR/web.log" 2>&1 &
WEB_PID=$!
for _ in $(seq 1 50); do
	if curl -fsS "$BASE/api/v1/health/live" >/dev/null 2>&1; then break; fi
	command sleep 0.2 2>/dev/null || sleep 0.2
done
expect_eq "健康检查" "200" "$(curl -s -o /dev/null -w '%{http_code}' "$BASE/api/v1/health/live")"

echo "== 2. admin 登录 =="
ADMIN_H="$WORKDIR/admin-login.headers"
ADMIN_B="$WORKDIR/admin-login.json"
expect_eq "admin 登录 → 200" "200" "$(do_login admin "$PASSWORD" "$ADMIN_H" "$ADMIN_B")"
ADMIN_COOKIE="$(cookie_of "$ADMIN_H")"
ADMIN_CSRF="$(jget "$ADMIN_B" 'd["data"]["csrf_token"]')"

echo "== 3. 未登录与 viewer 权限边界 =="
expect_eq "未登录 GET /users → 401" "401" "$(curl -s -o /dev/null -w '%{http_code}' "$BASE/api/v1/users")"
expect_eq "未登录 GET /users 错误码" "UNAUTHENTICATED" \
	"$(curl -s "$BASE/api/v1/users" | python -c 'import json,sys;print(json.load(sys.stdin)["error"]["code"])')"
expect_eq "未登录建账号 → 401" "401" "$(curl -s -o /dev/null -w '%{http_code}' -X POST "$BASE/api/v1/users" \
	-H 'Content-Type: application/json' -H "Origin: $ORIGIN" -H "X-CSRF-Token: x" \
	-d '{"username":"eve","role":"viewer","initial_password":"eve-horse-123"}')"

echo "== 4. 创建 viewer 与输入校验 =="
CREATE_BODY='{"username":"alice","display_name":"爱丽丝","role":"viewer","initial_password":"'"$VIEWER_PASSWORD"'"}'
CREATE_REQ="$WORKDIR/create.req"
printf '%s' "$CREATE_BODY" >"$CREATE_REQ"
CREATE_B="$WORKDIR/create.json"
code=$(curl -s -o "$CREATE_B" -w '%{http_code}' -X POST "$BASE/api/v1/users" \
	-H 'Content-Type: application/json' -H "Origin: $ORIGIN" \
	-H "Cookie: $ADMIN_COOKIE" -H "X-CSRF-Token: $ADMIN_CSRF" --data-binary @"$CREATE_REQ")
expect_eq "创建 viewer → 201" "201" "$code"
expect_not_contains "创建响应不回显密码" "$VIEWER_PASSWORD" "$(cat "$CREATE_B")"
expect_not_contains "创建响应不含哈希" '$argon2id$' "$(cat "$CREATE_B")"
VIEWER_ID="$(jget "$CREATE_B" 'd["data"]["id"]')"
VIEWER_VERSION="$(jget "$CREATE_B" 'd["data"]["version"]')"
expect_eq "新账号 version = 1" "1" "$VIEWER_VERSION"

code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$BASE/api/v1/users" \
	-H 'Content-Type: application/json' -H "Origin: $ORIGIN" \
	-H "Cookie: $ADMIN_COOKIE" -H "X-CSRF-Token: $ADMIN_CSRF" --data-binary @"$CREATE_REQ")
expect_eq "重复用户名 → 422" "422" "$code"

code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$BASE/api/v1/users" \
	-H 'Content-Type: application/json' -H "Origin: $ORIGIN" \
	-H "Cookie: $ADMIN_COOKIE" -H "X-CSRF-Token: $ADMIN_CSRF" \
	-d '{"username":"Admin'"'"'--","role":"viewer","initial_password":"inject-horse-1"}')
expect_eq "SQL 注入形状的用户名 → 400" "400" "$code"

code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$BASE/api/v1/users" \
	-H 'Content-Type: application/json' -H "Origin: $ORIGIN" \
	-H "Cookie: $ADMIN_COOKIE" -H "X-CSRF-Token: $ADMIN_CSRF" \
	-d '{"username":"bob","role":"viewer","initial_password":"bob-horse-123","password_hash":"x"}')
expect_eq "未知字段 → 400" "400" "$code"

code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$BASE/api/v1/users" \
	-H 'Content-Type: application/json' -H "Origin: $ORIGIN" \
	-H "Cookie: $ADMIN_COOKIE" -H "X-CSRF-Token: $ADMIN_CSRF" \
	-d '{"username":"bob","role":"root","initial_password":"bob-horse-123"}')
expect_eq "未知角色 → 400" "400" "$code"

code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$BASE/api/v1/users" \
	-H 'Content-Type: application/json' -H "Origin: $ORIGIN" \
	-H "Cookie: $ADMIN_COOKIE" -H "X-CSRF-Token: $ADMIN_CSRF" \
	-d '{"username":"bob","role":"viewer","initial_password":"short"}')
expect_eq "弱密码 → 400" "400" "$code"

code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$BASE/api/v1/users" \
	-H 'Content-Type: application/json' -H "Origin: $ORIGIN" \
	-H "Cookie: $ADMIN_COOKIE" --data-binary @"$CREATE_REQ")
expect_eq "缺 CSRF → 403" "403" "$code"

echo "== 5. 列表脱敏与分页 =="
LIST_B="$WORKDIR/users.json"
code=$(curl -s -o "$LIST_B" -w '%{http_code}' "$BASE/api/v1/users" -H "Cookie: $ADMIN_COOKIE")
expect_eq "账号列表 → 200" "200" "$code"
LIST_RAW="$(cat "$LIST_B")"
expect_contains "列表有 next_cursor 字段" '"next_cursor":null' "$LIST_RAW"
expect_contains "列表含 username" '"username":"alice"' "$LIST_RAW"
for forbidden in password argon2 csrf session_hash reauth; do
	expect_not_contains "列表脱敏：不含 $forbidden" "$forbidden" "$LIST_RAW"
done

PAGE1_B="$WORKDIR/users-page1.json"
curl -s -o "$PAGE1_B" "$BASE/api/v1/users?limit=1" -H "Cookie: $ADMIN_COOKIE"
expect_eq "limit=1 只返回一条" "1" "$(jget "$PAGE1_B" 'len(d["data"]["items"])')"
CURSOR="$(jget "$PAGE1_B" 'd["data"]["next_cursor"]')"
expect_not_contains "游标不含明文用户名" "admin" "$CURSOR"

PAGE2_B="$WORKDIR/users-page2.json"
code=$(curl -s -o "$PAGE2_B" -w '%{http_code}' "$BASE/api/v1/users?limit=1&cursor=$CURSOR" \
	-H "Cookie: $ADMIN_COOKIE")
expect_eq "翻页 → 200" "200" "$code"
expect_eq "第二页是 alice" "alice" "$(jget "$PAGE2_B" 'd["data"]["items"][0]["username"]')"
expect_eq "第二页无 next_cursor" "None" "$(jget "$PAGE2_B" 'd["data"]["next_cursor"]')"

expect_eq "limit=0 → 400" "400" "$(curl -s -o /dev/null -w '%{http_code}' "$BASE/api/v1/users?limit=0" -H "Cookie: $ADMIN_COOKIE")"
expect_eq "limit=201 → 400" "400" "$(curl -s -o /dev/null -w '%{http_code}' "$BASE/api/v1/users?limit=201" -H "Cookie: $ADMIN_COOKIE")"
expect_eq "坏游标 → 400" "400" "$(curl -s -o /dev/null -w '%{http_code}' "$BASE/api/v1/users?cursor=bad" -H "Cookie: $ADMIN_COOKIE")"

echo "== 6. viewer 会话越权 =="
VIEWER_H="$WORKDIR/alice-login.headers"
VIEWER_B="$WORKDIR/alice-login.json"
expect_eq "viewer 登录 → 200" "200" "$(do_login alice "$VIEWER_PASSWORD" "$VIEWER_H" "$VIEWER_B")"
VIEWER_COOKIE="$(cookie_of "$VIEWER_H")"
VIEWER_CSRF="$(jget "$VIEWER_B" 'd["data"]["csrf_token"]')"
expect_eq "viewer 读取账号列表 → 403" "403" \
	"$(curl -s -o /dev/null -w '%{http_code}' "$BASE/api/v1/users" -H "Cookie: $VIEWER_COOKIE")"
expect_eq "viewer 建账号 → 403" "403" \
	"$(curl -s -o /dev/null -w '%{http_code}' -X POST "$BASE/api/v1/users" \
		-H 'Content-Type: application/json' -H "Origin: $ORIGIN" \
		-H "Cookie: $VIEWER_COOKIE" -H "X-CSRF-Token: $VIEWER_CSRF" --data-binary @"$CREATE_REQ")"
expect_eq "viewer 再认证 → 403" "403" \
	"$(curl -s -o /dev/null -w '%{http_code}' -X POST "$BASE/api/v1/auth/reauth" \
		-H 'Content-Type: application/json' -H "Origin: $ORIGIN" \
		-H "Cookie: $VIEWER_COOKIE" -H "X-CSRF-Token: $VIEWER_CSRF" \
		-d "{\"password\":\"$VIEWER_PASSWORD\"}")"

echo "== 7. 再认证窗口 =="
PATCH_BODY='{"version":'"$VIEWER_VERSION"',"display_name":"爱丽丝"}'
PATCH_REQ="$WORKDIR/patch.req"
printf '%s' "$PATCH_BODY" >"$PATCH_REQ"
REAUTH_B="$WORKDIR/reauth.json"
code=$(curl -s -o /dev/null -w '%{http_code}' -X PATCH "$BASE/api/v1/users/$VIEWER_ID" \
	-H 'Content-Type: application/json' -H "Origin: $ORIGIN" \
	-H "Cookie: $ADMIN_COOKIE" -H "X-CSRF-Token: $ADMIN_CSRF" --data-binary @"$PATCH_REQ")
expect_eq "未再认证 PATCH → 403" "403" "$code"
expect_eq "未再认证错误码" "REAUTH_REQUIRED" \
	"$(curl -s -X PATCH "$BASE/api/v1/users/$VIEWER_ID" -H 'Content-Type: application/json' \
		-H "Origin: $ORIGIN" -H "Cookie: $ADMIN_COOKIE" -H "X-CSRF-Token: $ADMIN_CSRF" \
		--data-binary @"$PATCH_REQ" | python -c 'import json,sys;print(json.load(sys.stdin)["error"]["code"])')"

code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$BASE/api/v1/auth/reauth" \
	-H 'Content-Type: application/json' -H "Origin: $ORIGIN" \
	-H "Cookie: $ADMIN_COOKIE" -H "X-CSRF-Token: $ADMIN_CSRF" \
	-d '{"password":"wrong-password-1"}')
expect_eq "错误密码再认证 → 403" "403" "$code"

code=$(curl -s -o /dev/null -w '%{http_code}' -X PATCH "$BASE/api/v1/users/$VIEWER_ID" \
	-H 'Content-Type: application/json' -H "Origin: $ORIGIN" \
	-H "Cookie: $ADMIN_COOKIE" -H "X-CSRF-Token: $ADMIN_CSRF" --data-binary @"$PATCH_REQ")
expect_eq "失败的再认证不获得窗口" "403" "$code"

code=$(curl -s -o "$REAUTH_B" -w '%{http_code}' -X POST "$BASE/api/v1/auth/reauth" \
	-H 'Content-Type: application/json' -H "Origin: $ORIGIN" \
	-H "Cookie: $ADMIN_COOKIE" -H "X-CSRF-Token: $ADMIN_CSRF" \
	-d "{\"password\":\"$PASSWORD\"}")
expect_eq "正确密码再认证 → 200" "200" "$code"
REAUTH_UNTIL="$(jget "$REAUTH_B" 'd["data"]["reauth_expires_at"]')"
expect_contains "再认证返回 RFC3339 UTC" "Z" "$REAUTH_UNTIL"
expect_not_contains "再认证响应不含密码" "$PASSWORD" "$(cat "$REAUTH_B")"

code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$BASE/api/v1/auth/reauth" \
	-H 'Content-Type: application/json' -H "Origin: $ORIGIN" \
	-H "Cookie: $ADMIN_COOKIE" -H "X-CSRF-Token: $ADMIN_CSRF" \
	-d "{\"password\":\"$PASSWORD\",\"totp\":\"123456\"}")
expect_eq "P14 之前提交 totp → 400" "400" "$code"

echo "== 8. 修改账号与 version 并发 =="
UPDATE_B="$WORKDIR/update.json"
code=$(curl -s -o "$UPDATE_B" -w '%{http_code}' -X PATCH "$BASE/api/v1/users/$VIEWER_ID" \
	-H 'Content-Type: application/json' -H "Origin: $ORIGIN" \
	-H "Cookie: $ADMIN_COOKIE" -H "X-CSRF-Token: $ADMIN_CSRF" --data-binary @"$PATCH_REQ")
expect_eq "再认证后 PATCH → 200" "200" "$code"
expect_eq "PATCH 后 version 自增" "2" "$(jget "$UPDATE_B" 'd["data"]["version"]')"
expect_eq "PATCH 后 display_name 生效" "爱丽丝" "$(jget "$UPDATE_B" 'd["data"]["display_name"]')"

code=$(curl -s -o /dev/null -w '%{http_code}' -X PATCH "$BASE/api/v1/users/$VIEWER_ID" \
	-H 'Content-Type: application/json' -H "Origin: $ORIGIN" \
	-H "Cookie: $ADMIN_COOKIE" -H "X-CSRF-Token: $ADMIN_CSRF" --data-binary @"$PATCH_REQ")
expect_eq "过期 version → 409" "409" "$code"

code=$(curl -s -o /dev/null -w '%{http_code}' -X PATCH "$BASE/api/v1/users/$VIEWER_ID" \
	-H 'Content-Type: application/json' -H "Origin: $ORIGIN" \
	-H "Cookie: $ADMIN_COOKIE" -H "X-CSRF-Token: $ADMIN_CSRF" -d '{"version":2}')
expect_eq "没有任何可改字段 → 400" "400" "$code"

ADMIN_ID="$(jget "$LIST_B" '[u["id"] for u in d["data"]["items"] if u["username"]=="admin"][0]')"
ADMIN_VERSION="$(jget "$LIST_B" '[u["version"] for u in d["data"]["items"] if u["username"]=="admin"][0]')"
code=$(curl -s -o /dev/null -w '%{http_code}' -X PATCH "$BASE/api/v1/users/$ADMIN_ID" \
	-H 'Content-Type: application/json' -H "Origin: $ORIGIN" \
	-H "Cookie: $ADMIN_COOKIE" -H "X-CSRF-Token: $ADMIN_CSRF" \
	-d '{"version":'"$ADMIN_VERSION"',"role":"viewer"}')
expect_eq "改自己的角色 → 403" "403" "$code"
code=$(curl -s -o /dev/null -w '%{http_code}' -X PATCH "$BASE/api/v1/users/$ADMIN_ID" \
	-H 'Content-Type: application/json' -H "Origin: $ORIGIN" \
	-H "Cookie: $ADMIN_COOKIE" -H "X-CSRF-Token: $ADMIN_CSRF" \
	-d '{"version":'"$ADMIN_VERSION"',"disabled":true}')
expect_eq "禁用自己 → 403" "403" "$code"
expect_eq "自己被拒后仍是可用管理员" "False" \
	"$(jget "$LIST_B" '[u["disabled"] for u in d["data"]["items"] if u["username"]=="admin"][0]')"

echo "== 9. 重置密码使目标会话失效 =="
code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$BASE/api/v1/users/$VIEWER_ID/password" \
	-H 'Content-Type: application/json' -H "Origin: $ORIGIN" \
	-H "Cookie: $ADMIN_COOKIE" -H "X-CSRF-Token: $ADMIN_CSRF" \
	-d "{\"new_password\":\"$VIEWER_NEW_PASSWORD\"}")
expect_eq "重置密码 → 200" "200" "$code"
expect_eq "重置后旧会话 → 401" "401" \
	"$(curl -s -o /dev/null -w '%{http_code}' "$BASE/api/v1/auth/session" -H "Cookie: $VIEWER_COOKIE")"
expect_eq "旧密码登录 → 401" "401" "$(do_login alice "$VIEWER_PASSWORD" "$WORKDIR/x1.h" "$WORKDIR/x1.json")"
expect_eq "新密码登录 → 200" "200" "$(do_login alice "$VIEWER_NEW_PASSWORD" "$WORKDIR/x2.h" "$WORKDIR/x2.json")"
VIEWER_COOKIE="$(cookie_of "$WORKDIR/x2.h")"

code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$BASE/api/v1/users/$VIEWER_ID/password" \
	-H 'Content-Type: application/json' -H "Origin: $ORIGIN" \
	-H "Cookie: $ADMIN_COOKIE" -H "X-CSRF-Token: $ADMIN_CSRF" -d '{"new_password":"short"}')
expect_eq "重置成弱密码 → 400" "400" "$code"

echo "== 10. 删除账号 =="
code=$(curl -s -o /dev/null -w '%{http_code}' -X DELETE "$BASE/api/v1/users/$VIEWER_ID" \
	-H "Origin: $ORIGIN" -H "Cookie: $ADMIN_COOKIE" -H "X-CSRF-Token: $ADMIN_CSRF")
expect_eq "缺 version 删除 → 400" "400" "$code"
code=$(curl -s -o /dev/null -w '%{http_code}' -X DELETE "$BASE/api/v1/users/$VIEWER_ID?version=99" \
	-H "Origin: $ORIGIN" -H "Cookie: $ADMIN_COOKIE" -H "X-CSRF-Token: $ADMIN_CSRF")
expect_eq "错误 version 删除 → 409" "409" "$code"
code=$(curl -s -o /dev/null -w '%{http_code}' -X DELETE "$BASE/api/v1/users/$ADMIN_ID?version=$ADMIN_VERSION" \
	-H "Origin: $ORIGIN" -H "Cookie: $ADMIN_COOKIE" -H "X-CSRF-Token: $ADMIN_CSRF")
expect_eq "删除自己 → 403" "403" "$code"

code=$(curl -s -o /dev/null -w '%{http_code}' -X DELETE "$BASE/api/v1/users/$VIEWER_ID?version=3" \
	-H "Origin: $ORIGIN" -H "Cookie: $ADMIN_COOKIE" -H "X-CSRF-Token: $ADMIN_CSRF")
expect_eq "正确 version 删除 → 200" "200" "$code"
expect_eq "删除后目标会话 → 401" "401" \
	"$(curl -s -o /dev/null -w '%{http_code}' "$BASE/api/v1/auth/session" -H "Cookie: $VIEWER_COOKIE")"
expect_eq "重复删除 → 404" "404" \
	"$(curl -s -o /dev/null -w '%{http_code}' -X DELETE "$BASE/api/v1/users/$VIEWER_ID?version=3" \
		-H "Origin: $ORIGIN" -H "Cookie: $ADMIN_COOKIE" -H "X-CSRF-Token: $ADMIN_CSRF")"
expect_eq "删除后列表只剩 admin" "1" \
	"$(curl -s "$BASE/api/v1/users" -H "Cookie: $ADMIN_COOKIE" | python -c 'import json,sys;print(len(json.load(sys.stdin)["data"]["items"]))')"

echo "== 11. 审计：追加式记录且不含口令 =="
AUDIT_SUMMARY="$(python - "$DB" <<'PY'
import sqlite3, sys
con = sqlite3.connect(sys.argv[1])
rows = con.execute(
    "SELECT action, actor_snapshot, target, outcome, redacted_details FROM audit_events ORDER BY id"
).fetchall()
print("COUNT=%d" % len(rows))
print("ACTIONS=%s" % ",".join(sorted({r[0] for r in rows})))
print("OUTCOMES=%s" % ",".join(sorted({r[3] for r in rows})))
print("TARGETS=%s" % ",".join(sorted({r[2].split(":")[0] for r in rows})))
print("OUTCOME_SUCCESS=%s" % ("YES" if any(r[3] == "success" for r in rows) else "NO"))
print("OUTCOME_FAILURE=%s" % ("YES" if any(r[3] == "failure" for r in rows) else "NO"))
print("FAILURE_HAS_STAGE=%s" % ("YES" if all(
    "stage" in (r[4] or "") for r in rows if r[3] == "failure") else "NO"))
print("SUCCESS_TARGET_USER=%s" % ("YES" if any(
    r[3] == "success" and r[2].startswith("user:") for r in rows) else "NO"))
print("SNAPSHOT_HAS_ROLE=%s" % any("admin" in (r[1] or "") for r in rows))
print("DETAILS=%s" % " ".join(str(r[4]) for r in rows))
PY
)"
AUDIT_COUNT="$(printf '%s\n' "$AUDIT_SUMMARY" | sed -n 's/^COUNT=//p')"
if [ "${AUDIT_COUNT:-0}" -ge 4 ]; then pass "审计至少记录 4 条（创建/修改/重置/删除）"; else fail "审计条数不足：$AUDIT_COUNT"; fi
expect_contains "审计包含 user.create" "user.create" "$AUDIT_SUMMARY"
expect_contains "审计包含 user.update" "user.update" "$AUDIT_SUMMARY"
expect_contains "审计包含 user.password.reset" "user.password.reset" "$AUDIT_SUMMARY"
expect_contains "审计包含 user.delete" "user.delete" "$AUDIT_SUMMARY"
# P02.4：成功事件与失败/被拒绝事件都要留痕；成功事件带对象类型 target。
expect_contains "审计有成功事件" "OUTCOME_SUCCESS=YES" "$AUDIT_SUMMARY"
expect_contains "审计成功事件带对象类型 target" "SUCCESS_TARGET_USER=YES" "$AUDIT_SUMMARY"
expect_contains "审计记录被拒绝的请求（failure）" "OUTCOME_FAILURE=YES" "$AUDIT_SUMMARY"
expect_contains "失败事件带 stage 字段" "FAILURE_HAS_STAGE=YES" "$AUDIT_SUMMARY"
expect_not_contains "审计不含登录口令" "$PASSWORD" "$AUDIT_SUMMARY"
expect_not_contains "审计不含 viewer 口令" "$VIEWER_PASSWORD" "$AUDIT_SUMMARY"
expect_not_contains "审计不含新口令" "$VIEWER_NEW_PASSWORD" "$AUDIT_SUMMARY"
expect_not_contains "审计不含口令哈希" '$argon2id$' "$AUDIT_SUMMARY"

python - "$DB" <<'PY'
import sqlite3, sys
con = sqlite3.connect(sys.argv[1])
for stmt in ("UPDATE audit_events SET outcome='failure'",
             "DELETE FROM audit_events"):
    try:
        con.execute(stmt)
        con.commit()
        print("APPEND_ONLY=NO:%s" % stmt)
        break
    except sqlite3.DatabaseError as exc:
        if "只追加" not in str(exc):
            print("APPEND_ONLY=UNEXPECTED:%s" % exc)
            break
else:
    print("APPEND_ONLY=YES")
PY

echo "== 12. 日志与数据库文件不含秘密 =="
LOG="$(cat "$WORKDIR/web.log")"
expect_not_contains "日志不含 admin 口令" "$PASSWORD" "$LOG"
expect_not_contains "日志不含 viewer 口令" "$VIEWER_PASSWORD" "$LOG"
expect_not_contains "日志不含 CSRF 令牌" "$ADMIN_CSRF" "$LOG"
expect_not_contains "日志不含会话 Cookie" "${ADMIN_COOKIE#__Host-upanel_session=}" "$LOG"
DB_BLOB="$(cat "$DB" "$DB-wal" 2>/dev/null | tr -d '\000')"
expect_not_contains "数据库不含明文 admin 口令" "$PASSWORD" "$DB_BLOB"
expect_not_contains "数据库不含明文 viewer 口令" "$VIEWER_NEW_PASSWORD" "$DB_BLOB"

echo
printf '结果: PASS=%d FAIL=%d\n' "$PASS" "$FAIL"
printf '工作目录（含日志与响应）: %s\n' "$WORKDIR"
[ "$FAIL" -eq 0 ]
