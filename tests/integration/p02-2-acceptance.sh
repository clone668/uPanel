#!/bin/sh
# P02.2 Linux 验收脚本：账号管理、再认证与审计在真实 Ubuntu 上的行为。
#
# 运行位置：Ubuntu 22.04/24.04 验收 VM 内，用**非 root** 用户运行（脚本会断言这一点）。
# 不依赖 Go 工具链：使用宿主交叉编译好的 Linux 二进制。
#
# 用法：
#   BIN_DIR=/tmp/upanel-bin sh tests/integration/p02-2-acceptance.sh
#
# 退出码：0 全部通过；1 有失败项。
#
# 覆盖的验收点（对应 CONTRACTS §2 账号表 + STAGES.md P02.2 + P02 验收清单）：
#   1. 非 root 运行；systemd 单元仍是非 root 身份
#   2. init-admin 创建首个管理员；production 在 127.0.0.1:9443 启动
#   3. 账号管理：创建、列表脱敏 DTO、limit/cursor 分页、输入校验（SQL 注入形状/未知字段/未知角色/弱密码）
#   4. viewer 越权一律 403（含再认证接口）
#   5. 再认证窗口：未再认证 403 REAUTH_REQUIRED、错误口令 403、正确口令 200 并带 reauth_expires_at
#   6. 乐观并发 version：成功后自增、过期 version 409、缺 version 400
#   7. 自我修改（改角色/禁用自己/删除自己）403；最后一个可用管理员不被移除
#   8. 重置口令使目标会话立即 401；旧口令 401、新口令 200
#   9. 删除账号使目标会话立即 401；重复删除 404
#  10. 审计：追加式事件、动作齐全、不含口令；UPDATE/DELETE 被数据库触发器拒绝
#  11. 数据库与 -wal/-shm 文件权限不向其他用户开放；库里与日志里都没有明文口令

set -u

BIN_DIR=${BIN_DIR:-/tmp/upanel-bin}
WEB="$BIN_DIR/upanel-web"
AGENT="$BIN_DIR/upanel-agent"
CTL="$BIN_DIR/upanelctl"

ROOT=$(cd "$(dirname "$0")/../.." && pwd)
WORK=${WORK:-/var/tmp/upanel-accept-p02-2}
DB="$WORK/upanel.db"
PORT=9443
BASE="http://127.0.0.1:$PORT"
ORIGIN="$BASE"
PASSWORD='vm-correct-horse-9'
VIEWER_PASSWORD='vm-viewer-horse-42'
VIEWER_NEW_PASSWORD='vm-viewer-horse-77'
LOGDIR="$WORK/logs"

PASSED=0
FAILED=0
FAILED_NAMES=""
WEB_PID=""

pass() { PASSED=$((PASSED + 1)); printf 'PASS  %s\n' "$1"; }
fail() { FAILED=$((FAILED + 1)); FAILED_NAMES="$FAILED_NAMES | $1"; printf 'FAIL  %s: %s\n' "$1" "${2:-}"; }

expect_eq() { if [ "$2" = "$3" ]; then pass "$1"; else fail "$1" "期望 '$2'，实际 '$3'"; fi; }

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

# jget <file> <python 表达式，d 为解析后的 JSON>
jget() {
  python3 -c 'import json,sys
d=json.load(open(sys.argv[1], encoding="utf-8"))
print(eval(sys.argv[2]))' "$1" "$2" 2>/dev/null
}

cleanup() {
  if [ -n "$WEB_PID" ] && kill -0 "$WEB_PID" 2>/dev/null; then
    kill -TERM "$WEB_PID" 2>/dev/null
    wait "$WEB_PID" 2>/dev/null
  fi
}
trap cleanup EXIT
trap 'cleanup; exit 130' INT
trap 'cleanup; exit 143' TERM

start_web() {
  "$WEB" --mode production --db "$DB" --listen "127.0.0.1:$PORT" >>"$LOGDIR/web.log" 2>&1 &
  WEB_PID=$!
  for _ in $(seq 1 100); do
    if curl -fsS "$BASE/api/v1/health/live" >/dev/null 2>&1; then return 0; fi
    sleep 0.2
  done
  return 1
}

# login <username> <password> <tag>：响应体写到 $WORK/<tag>.json，Cookie 与 CSRF 回显
login() {
  H="$WORK/$3.headers"
  curl -s -D "$H" -o "$WORK/$3.json" -w '%{http_code}' -X POST "$BASE/api/v1/auth/login" \
    -H 'Content-Type: application/json' -H 'X-UPanel-Request: 1' -H "Origin: $ORIGIN" \
    -d "{\"username\":\"$1\",\"password\":\"$2\"}"
}

cookie_of() {
  grep -i '^set-cookie:' "$1" | tr -d '\r' | sed -e 's/^[Ss]et-[Cc]ookie: //' -e 's/;.*//'
}

echo "== 0. 环境与交付物 =="
if [ "$(id -u)" -ne 0 ]; then pass "验收脚本以非 root 用户运行（$(id -un)）"; else fail "验收脚本以非 root 用户运行" "当前是 root"; fi
for b in "$WEB" "$AGENT" "$CTL"; do
  if [ -x "$b" ]; then pass "二进制存在且可执行 $(basename "$b")"; else fail "二进制存在且可执行 $(basename "$b")" "$b"; fi
done

UNIT="$ROOT/deploy/systemd/upanel-web.service"
if [ -f "$UNIT" ]; then
  UNIT_BODY=$(cat "$UNIT")
  expect_contains "systemd 单元指定非 root 用户" "User=upanel-web" "$UNIT_BODY"
  expect_not_contains "systemd 单元不以 root 运行" "User=root" "$UNIT_BODY"
else
  fail "systemd 单元存在" "$UNIT 缺失"
fi

rm -rf "$WORK"
mkdir -p "$LOGDIR" || { echo "无法创建 $LOGDIR"; exit 1; }

echo "== 1. init-admin 与 production 启动 =="
INIT_OUT=$(printf '%s\n%s\n' "$PASSWORD" "$PASSWORD" | "$CTL" init-admin --db "$DB" --username admin --display-name "管理员" --json 2>"$WORK/init.err")
expect_eq "init-admin 退出码" "0" "$?"
expect_contains "输出含用户名 admin" '"username": "admin"' "$INIT_OUT"
expect_not_contains "输出不含明文口令" "$PASSWORD" "$INIT_OUT"

if curl -fsS "$BASE/api/v1/health/live" >/dev/null 2>&1; then
  echo "9443 上已有服务在监听；先停掉遗留的 upanel-web 再跑验收" >&2
  exit 1
fi
if start_web; then pass "production 模式在 127.0.0.1:9443 启动"; else fail "production 模式启动" "$(tail -5 "$LOGDIR/web.log" 2>/dev/null)"; fi
expect_eq "健康检查" "200" "$(curl -s -o /dev/null -w '%{http_code}' "$BASE/api/v1/health/live")"

WEB_UID=$(ps -o uid= -p "$WEB_PID" 2>/dev/null | tr -d ' ')
if [ -n "$WEB_UID" ] && [ "$WEB_UID" != "0" ]; then pass "Web 进程 uid=$WEB_UID（非 root）"; else fail "Web 进程不以 root 运行" "uid=$WEB_UID"; fi

echo "== 2. 管理员登录 =="
expect_eq "admin 登录 → 200" "200" "$(login admin "$PASSWORD" admin)"
ADMIN_COOKIE=$(cookie_of "$WORK/admin.headers")
ADMIN_CSRF=$(jget "$WORK/admin.json" 'd["data"]["csrf_token"]')
if [ -n "$ADMIN_COOKIE" ] && [ -n "$ADMIN_CSRF" ]; then pass "拿到管理员会话与 CSRF"; else fail "拿到管理员会话与 CSRF" "cookie/csrf 为空"; fi

api() {
  # api <method> <path> <cookie> <csrf> [body-file] ；打印状态码，响应写到 $WORK/resp.json
  if [ -n "${5:-}" ]; then
    curl -s -o "$WORK/resp.json" -w '%{http_code}' -X "$1" "$BASE$2" \
      -H 'Content-Type: application/json' -H "Origin: $ORIGIN" \
      -H "Cookie: $3" -H "X-CSRF-Token: $4" --data-binary @"$5"
  else
    curl -s -o "$WORK/resp.json" -w '%{http_code}' -X "$1" "$BASE$2" \
      -H 'Content-Type: application/json' -H "Origin: $ORIGIN" \
      -H "Cookie: $3" -H "X-CSRF-Token: $4"
  fi
}

echo "== 3. 创建账号与输入校验 =="
printf '%s' '{"username":"alice","display_name":"爱丽丝","role":"viewer","initial_password":"'"$VIEWER_PASSWORD"'"}' >"$WORK/create.req"
expect_eq "创建 viewer → 201" "201" "$(api POST /api/v1/users "$ADMIN_COOKIE" "$ADMIN_CSRF" "$WORK/create.req")"
expect_not_contains "创建响应不回显口令" "$VIEWER_PASSWORD" "$(cat "$WORK/resp.json")"
expect_not_contains "创建响应不含哈希" '$argon2id$' "$(cat "$WORK/resp.json")"
expect_eq "display_name 原样保存" "爱丽丝" "$(jget "$WORK/resp.json" 'd["data"]["display_name"]')"
VIEWER_ID=$(jget "$WORK/resp.json" 'd["data"]["id"]')
VIEWER_VERSION=$(jget "$WORK/resp.json" 'd["data"]["version"]')
expect_eq "新账号 version = 1" "1" "$VIEWER_VERSION"

expect_eq "重复用户名 → 422" "422" "$(api POST /api/v1/users "$ADMIN_COOKIE" "$ADMIN_CSRF" "$WORK/create.req")"

printf '%s' '{"username":"Admin'"'"'--","role":"viewer","initial_password":"inject-horse-1"}' >"$WORK/inject.req"
expect_eq "SQL 注入形状用户名 → 400" "400" "$(api POST /api/v1/users "$ADMIN_COOKIE" "$ADMIN_CSRF" "$WORK/inject.req")"

printf '%s' '{"username":"bob","role":"viewer","initial_password":"bob-horse-123","password_hash":"x"}' >"$WORK/unknown.req"
expect_eq "未知字段 → 400" "400" "$(api POST /api/v1/users "$ADMIN_COOKIE" "$ADMIN_CSRF" "$WORK/unknown.req")"

printf '%s' '{"username":"bob","role":"root","initial_password":"bob-horse-123"}' >"$WORK/role.req"
expect_eq "未知角色 → 400" "400" "$(api POST /api/v1/users "$ADMIN_COOKIE" "$ADMIN_CSRF" "$WORK/role.req")"

printf '%s' '{"username":"bob","role":"viewer","initial_password":"short"}' >"$WORK/weak.req"
expect_eq "弱口令 → 400" "400" "$(api POST /api/v1/users "$ADMIN_COOKIE" "$ADMIN_CSRF" "$WORK/weak.req")"

code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$BASE/api/v1/users" \
  -H 'Content-Type: application/json' -H "Origin: $ORIGIN" \
  -H "Cookie: $ADMIN_COOKIE" --data-binary @"$WORK/create.req")
expect_eq "缺 CSRF 的写操作 → 403" "403" "$code"

echo "== 4. 列表脱敏与分页 =="
expect_eq "账号列表 → 200" "200" "$(api GET /api/v1/users "$ADMIN_COOKIE" "")"
LIST_RAW=$(cat "$WORK/resp.json")
expect_contains "列表带 next_cursor 字段" '"next_cursor":null' "$LIST_RAW"
expect_contains "列表含 username" '"username":"alice"' "$LIST_RAW"
for forbidden in password argon2 csrf reauth session_hash; do
  expect_not_contains "列表脱敏：不含 $forbidden" "$forbidden" "$LIST_RAW"
done
expect_eq "列表里 alice 是 viewer" "viewer" "$(jget "$WORK/resp.json" '[u["role"] for u in d["data"]["items"] if u["username"]=="alice"][0]')"

api GET '/api/v1/users?limit=1' "$ADMIN_COOKIE" "" >/dev/null
expect_eq "limit=1 只返回一条" "1" "$(jget "$WORK/resp.json" 'len(d["data"]["items"])')"
CURSOR=$(jget "$WORK/resp.json" 'd["data"]["next_cursor"]')
if [ -n "$CURSOR" ]; then pass "第一页有游标"; else fail "第一页有游标" "next_cursor 为空"; fi
expect_not_contains "游标不含明文用户名" "admin" "$CURSOR"
api GET "/api/v1/users?limit=1&cursor=$CURSOR" "$ADMIN_COOKIE" "" >/dev/null
expect_eq "第二页是 alice" "alice" "$(jget "$WORK/resp.json" 'd["data"]["items"][0]["username"]')"
expect_eq "第二页无 next_cursor" "None" "$(jget "$WORK/resp.json" 'd["data"]["next_cursor"]')"
expect_eq "limit=0 → 400" "400" "$(api GET '/api/v1/users?limit=0' "$ADMIN_COOKIE" "")"
expect_eq "limit=201 → 400" "400" "$(api GET '/api/v1/users?limit=201' "$ADMIN_COOKIE" "")"
expect_eq "坏游标 → 400" "400" "$(api GET '/api/v1/users?cursor=bad' "$ADMIN_COOKIE" "")"

echo "== 5. viewer 越权一律 403 =="
expect_eq "viewer 登录 → 200" "200" "$(login alice "$VIEWER_PASSWORD" viewer)"
VIEWER_COOKIE=$(cookie_of "$WORK/viewer.headers")
VIEWER_CSRF=$(jget "$WORK/viewer.json" 'd["data"]["csrf_token"]')
expect_eq "viewer 读账号列表 → 403" "403" "$(api GET /api/v1/users "$VIEWER_COOKIE" "")"
expect_eq "viewer 建账号 → 403" "403" "$(api POST /api/v1/users "$VIEWER_COOKIE" "$VIEWER_CSRF" "$WORK/create.req")"
printf '%s' "{\"version\":$VIEWER_VERSION,\"display_name\":\"x\"}" >"$WORK/patch-x.req"
printf '%s' '{"new_password":"viewer-cannot-horse-1"}' >"$WORK/password-viewer.req"
expect_eq "viewer 改账号 → 403" "403" "$(api PATCH "/api/v1/users/$VIEWER_ID" "$VIEWER_COOKIE" "$VIEWER_CSRF" "$WORK/patch-x.req")"
expect_eq "viewer 重置口令 → 403" "403" "$(api POST "/api/v1/users/$VIEWER_ID/password" "$VIEWER_COOKIE" "$VIEWER_CSRF" "$WORK/password-viewer.req")"
expect_eq "viewer 删账号 → 403" "403" "$(api DELETE "/api/v1/users/$VIEWER_ID" "$VIEWER_COOKIE" "$VIEWER_CSRF")"
printf '%s' "{\"password\":\"$VIEWER_PASSWORD\"}" >"$WORK/reauth-viewer.req"
expect_eq "viewer 再认证 → 403" "403" "$(api POST /api/v1/auth/reauth "$VIEWER_COOKIE" "$VIEWER_CSRF" "$WORK/reauth-viewer.req")"

echo "== 6. 再认证窗口 =="
printf '%s' "{\"version\":$VIEWER_VERSION,\"display_name\":\"爱丽丝\"}" >"$WORK/patch.req"
expect_eq "未再认证 PATCH → 403" "403" "$(api PATCH "/api/v1/users/$VIEWER_ID" "$ADMIN_COOKIE" "$ADMIN_CSRF" "$WORK/patch.req")"
expect_contains "错误码 REAUTH_REQUIRED" "REAUTH_REQUIRED" "$(cat "$WORK/resp.json")"

printf '%s' '{"password":"wrong-password-1"}' >"$WORK/reauth-bad.req"
expect_eq "错误口令再认证 → 403" "403" "$(api POST /api/v1/auth/reauth "$ADMIN_COOKIE" "$ADMIN_CSRF" "$WORK/reauth-bad.req")"
expect_eq "失败的再认证不获得窗口" "403" "$(api PATCH "/api/v1/users/$VIEWER_ID" "$ADMIN_COOKIE" "$ADMIN_CSRF" "$WORK/patch.req")"

printf '%s' "{\"password\":\"$PASSWORD\"}" >"$WORK/reauth.req"
expect_eq "正确口令再认证 → 200" "200" "$(api POST /api/v1/auth/reauth "$ADMIN_COOKIE" "$ADMIN_CSRF" "$WORK/reauth.req")"
REAUTH_UNTIL=$(jget "$WORK/resp.json" 'd["data"]["reauth_expires_at"]')
expect_contains "再认证时间戳是 UTC" "Z" "$REAUTH_UNTIL"
expect_not_contains "再认证响应不含口令" "$PASSWORD" "$(cat "$WORK/resp.json")"

printf '%s' "{\"password\":\"$PASSWORD\",\"totp\":\"123456\"}" >"$WORK/reauth-totp.req"
expect_eq "P14 之前提交 totp → 400" "400" "$(api POST /api/v1/auth/reauth "$ADMIN_COOKIE" "$ADMIN_CSRF" "$WORK/reauth-totp.req")"

echo "== 7. 修改账号与 version 并发 =="
expect_eq "再认证后 PATCH → 200" "200" "$(api PATCH "/api/v1/users/$VIEWER_ID" "$ADMIN_COOKIE" "$ADMIN_CSRF" "$WORK/patch.req")"
expect_eq "PATCH 后 version 自增" "2" "$(jget "$WORK/resp.json" 'd["data"]["version"]')"
expect_eq "过期 version → 409" "409" "$(api PATCH "/api/v1/users/$VIEWER_ID" "$ADMIN_COOKIE" "$ADMIN_CSRF" "$WORK/patch.req")"
printf '%s' '{"version":2}' >"$WORK/empty-patch.req"
expect_eq "没有任何可改字段 → 400" "400" "$(api PATCH "/api/v1/users/$VIEWER_ID" "$ADMIN_COOKIE" "$ADMIN_CSRF" "$WORK/empty-patch.req")"

# 登录响应的 user 是公开 DTO（id/username/display_name/role），没有 version；
# 自我修改用例需要的 id 与 version 从账号列表取。
api GET /api/v1/users "$ADMIN_COOKIE" "" >/dev/null
ADMIN_ID=$(jget "$WORK/resp.json" '[u["id"] for u in d["data"]["items"] if u["username"]=="admin"][0]')
ADMIN_VERSION=$(jget "$WORK/resp.json" '[u["version"] for u in d["data"]["items"] if u["username"]=="admin"][0]')
printf '%s' "{\"version\":$ADMIN_VERSION,\"role\":\"viewer\"}" >"$WORK/self-role.req"
expect_eq "改自己的角色 → 403" "403" "$(api PATCH "/api/v1/users/$ADMIN_ID" "$ADMIN_COOKIE" "$ADMIN_CSRF" "$WORK/self-role.req")"
printf '%s' "{\"version\":$ADMIN_VERSION,\"disabled\":true}" >"$WORK/self-disable.req"
expect_eq "禁用自己 → 403" "403" "$(api PATCH "/api/v1/users/$ADMIN_ID" "$ADMIN_COOKIE" "$ADMIN_CSRF" "$WORK/self-disable.req")"
expect_eq "删除自己 → 403" "403" "$(api DELETE "/api/v1/users/$ADMIN_ID?version=$ADMIN_VERSION" "$ADMIN_COOKIE" "$ADMIN_CSRF")"

# 最后一个可用管理员仍在：列表里唯一 admin 未被禁用、未被降级。
api GET /api/v1/users "$ADMIN_COOKIE" "" >/dev/null
expect_eq "仍是可用管理员" "1" "$(jget "$WORK/resp.json" 'len([u for u in d["data"]["items"] if u["role"]=="admin" and not u["disabled"]])')"

echo "== 8. 重置口令使目标会话失效 =="
printf '%s' "{\"new_password\":\"$VIEWER_NEW_PASSWORD\"}" >"$WORK/password.req"
expect_eq "重置口令 → 200" "200" "$(api POST "/api/v1/users/$VIEWER_ID/password" "$ADMIN_COOKIE" "$ADMIN_CSRF" "$WORK/password.req")"
expect_not_contains "重置响应不回显新口令" "$VIEWER_NEW_PASSWORD" "$(cat "$WORK/resp.json")"
expect_eq "重置后目标旧会话 → 401" "401" "$(curl -s -o /dev/null -w '%{http_code}' "$BASE/api/v1/auth/session" -H "Cookie: $VIEWER_COOKIE")"
expect_eq "旧口令登录 → 401" "401" "$(login alice "$VIEWER_PASSWORD" old)"
expect_eq "新口令登录 → 200" "200" "$(login alice "$VIEWER_NEW_PASSWORD" new)"
VIEWER_COOKIE=$(cookie_of "$WORK/new.headers")

printf '%s' '{"new_password":"short"}' >"$WORK/weak-new.req"
expect_eq "重置成弱口令 → 400" "400" "$(api POST "/api/v1/users/$VIEWER_ID/password" "$ADMIN_COOKIE" "$ADMIN_CSRF" "$WORK/weak-new.req")"

echo "== 9. 删除账号 =="
expect_eq "缺 version 删除 → 400" "400" "$(api DELETE "/api/v1/users/$VIEWER_ID" "$ADMIN_COOKIE" "$ADMIN_CSRF")"
expect_eq "错误 version 删除 → 409" "409" "$(api DELETE "/api/v1/users/$VIEWER_ID?version=99" "$ADMIN_COOKIE" "$ADMIN_CSRF")"
expect_eq "正确 version 删除 → 200" "200" "$(api DELETE "/api/v1/users/$VIEWER_ID?version=3" "$ADMIN_COOKIE" "$ADMIN_CSRF")"
expect_eq "删除后目标会话 → 401" "401" "$(curl -s -o /dev/null -w '%{http_code}' "$BASE/api/v1/auth/session" -H "Cookie: $VIEWER_COOKIE")"
expect_eq "重复删除 → 404" "404" "$(api DELETE "/api/v1/users/$VIEWER_ID?version=3" "$ADMIN_COOKIE" "$ADMIN_CSRF")"
api GET /api/v1/users "$ADMIN_COOKIE" "" >/dev/null
expect_eq "删除后列表只剩 admin" "1" "$(jget "$WORK/resp.json" 'len(d["data"]["items"])')"

echo "== 10. 审计：追加式记录且不含口令 =="
# 断言口径（PLAN.md 第 332 行）：正常管理员操作必须有审计；**失败和被拒绝的高风险操作也有记录**。
# 因此这里不能笼统断言“所有审计都是 success”——本脚本自己就会制造 403/409/422，
# 那些行必须是 outcome=failure 并且带 stage/error_code。分三个维度断言：
#   ① 业务动作（user.create/update/password.reset/delete）的记录全部 success；
#   ② 被拒绝/失败的高风险请求确实留下了 failure 记录，且 target 仍是 user；
#   ③ 任何一行都不含口令、口令哈希。
AUDIT_SUMMARY=$(python3 - "$DB" <<'PY'
import json, sqlite3, sys
con = sqlite3.connect(sys.argv[1])
rows = con.execute(
    "SELECT action, actor_snapshot, target, outcome, redacted_details FROM audit_events ORDER BY id"
).fetchall()
business = {"user.create", "user.update", "user.password.reset", "user.delete"}
bad = ["%s=%s" % (r[0], r[3]) for r in rows if r[0] in business and r[3] != "success"]
failures = [r for r in rows if r[3] == "failure"]
failure_targets = sorted({r[2].split(":")[0] for r in failures})
# failure 行的 details 必须是结构化拒绝信息（stage/error_code），且不能夹带口令类键。
denied_details_ok = "YES"
for r in failures:
    try:
        d = json.loads(r[4])
    except Exception:
        denied_details_ok = "NO:details-not-json"
        break
    if not d.get("stage") or not d.get("error_code"):
        denied_details_ok = "NO:missing-stage-or-code"
        break
    if any(k for k in d if "password" in k.lower() or "token" in k.lower()):
        denied_details_ok = "NO:secret-key-in-details"
        break
print("COUNT=%d" % len(rows))
print("ACTIONS=%s" % ",".join(sorted({r[0] for r in rows})))
print("OUTCOMES=%s" % ",".join(sorted({r[3] for r in rows})))
print("TARGETS=%s" % ",".join(sorted({r[2].split(":")[0] for r in rows})))
print("BUSINESS_NON_SUCCESS=%s" % ",".join(bad))
print("BUSINESS_SUCCESS=%s" % ",".join(sorted(
    {r[0] for r in rows if r[0] in business and r[3] == "success"})))
print("BUSINESS_TARGETS=%s" % ",".join(sorted({r[2].split(":")[0] for r in rows if r[0] in business})))
print("FAILURES=%d" % len(failures))
print("FAILURE_TARGETS=%s" % ",".join(failure_targets))
print("FAILURE_HAS_USER=%s" % ("YES" if "user" in failure_targets else "NO"))
print("FAILURE_TARGETS_UNEXPECTED=%s" % ",".join(
    t for t in failure_targets if t not in ("user", "session")))
print("DENIED_DETAILS=%s" % denied_details_ok)
print("DETAILS=%s" % " ".join(str(r[4]) for r in rows))
PY
)
AUDIT_COUNT=$(printf '%s\n' "$AUDIT_SUMMARY" | sed -n 's/^COUNT=//p')
if [ "${AUDIT_COUNT:-0}" -ge 4 ]; then pass "审计至少 4 条（创建/修改/重置/删除）"; else fail "审计条数" "count=$AUDIT_COUNT"; fi
for action in user.create user.update user.password.reset user.delete; do
  expect_contains "审计含 $action" "$action" "$AUDIT_SUMMARY"
done
# 业务动作**允许**有 failure 行：被拒绝的写入本来就要留记录（PLAN.md 第 332 行）。
# 但每个业务动作都必须至少有一条 success —— 只记失败等于没证明“这次操作真的成功过”。
expect_eq "四个业务动作都有 success 审计" \
  "user.create,user.delete,user.password.reset,user.update" \
  "$(printf '%s\n' "$AUDIT_SUMMARY" | sed -n 's/^BUSINESS_SUCCESS=//p')"
expect_eq "业务动作的审计 target 是 user" "user" "$(printf '%s\n' "$AUDIT_SUMMARY" | sed -n 's/^BUSINESS_TARGETS=//p')"
AUDIT_FAILURES=$(printf '%s\n' "$AUDIT_SUMMARY" | sed -n 's/^FAILURES=//p')
if [ "${AUDIT_FAILURES:-0}" -ge 1 ]; then
  pass "被拒绝的高风险请求留了 failure 审计（$AUDIT_FAILURES 条）"
else
  fail "被拒绝的高风险请求留了 failure 审计" "failures=$AUDIT_FAILURES"
fi
expect_eq "failure 审计覆盖 user 对象" "YES" "$(printf '%s\n' "$AUDIT_SUMMARY" | sed -n 's/^FAILURE_HAS_USER=//p')"
expect_eq "failure 审计的 target 只出现 user/session" "" "$(printf '%s\n' "$AUDIT_SUMMARY" | sed -n 's/^FAILURE_TARGETS_UNEXPECTED=//p')"
expect_eq "failure 审计记为结构化拒绝信息" "YES" "$(printf '%s\n' "$AUDIT_SUMMARY" | sed -n 's/^DENIED_DETAILS=//p')"
expect_not_contains "审计不含管理员口令" "$PASSWORD" "$AUDIT_SUMMARY"
expect_not_contains "审计不含 viewer 口令" "$VIEWER_NEW_PASSWORD" "$AUDIT_SUMMARY"
expect_not_contains "审计不含口令哈希" '$argon2id$' "$AUDIT_SUMMARY"

APPEND_ONLY=$(python3 - "$DB" <<'PY'
import sqlite3, sys
con = sqlite3.connect(sys.argv[1])
result = "YES"
for stmt in ("UPDATE audit_events SET outcome='failure'",
             "DELETE FROM audit_events"):
    try:
        con.execute(stmt)
        con.commit()
        result = "NO:" + stmt
        break
    except sqlite3.DatabaseError as exc:
        if "只追加" not in str(exc):
            result = "UNEXPECTED:" + str(exc)
            break
print("APPEND_ONLY=" + result)
PY
)
expect_contains "UPDATE/DELETE 被触发器拒绝" "APPEND_ONLY=YES" "$APPEND_ONLY"

echo "== 11. 文件权限、库内容与日志 =="
for f in "$DB" "$DB-wal" "$DB-shm"; do
  [ -e "$f" ] || continue
  MODE=$(stat -c %a "$f" 2>/dev/null)
  OWNER=$(stat -c %U "$f" 2>/dev/null)
  case $MODE in
    600|640|660) pass "$(basename "$f") 权限不向其他用户开放（$MODE）" ;;
    *) fail "$(basename "$f") 权限不向其他用户开放" "mode=$MODE" ;;
  esac
  expect_eq "$(basename "$f") 属于运行用户" "$(id -un)" "$OWNER"
done

DB_BLOB=$(cat "$DB" "$DB-wal" 2>/dev/null | tr -d '\000')
expect_not_contains "库里不含明文管理员口令" "$PASSWORD" "$DB_BLOB"
expect_not_contains "库里不含明文 viewer 口令" "$VIEWER_NEW_PASSWORD" "$DB_BLOB"
LOG=$(cat "$LOGDIR/web.log" 2>/dev/null)
expect_not_contains "日志不含管理员口令" "$PASSWORD" "$LOG"
expect_not_contains "日志不含 viewer 口令" "$VIEWER_NEW_PASSWORD" "$LOG"
expect_not_contains "日志不含 CSRF 令牌" "$ADMIN_CSRF" "$LOG"

echo
printf '结果: PASS=%d FAIL=%d\n' "$PASSED" "$FAILED"
if [ "$FAILED" -ne 0 ]; then printf '失败项:%s\n' "$FAILED_NAMES"; fi
printf '工作目录: %s\n' "$WORK"
[ "$FAILED" -eq 0 ]
