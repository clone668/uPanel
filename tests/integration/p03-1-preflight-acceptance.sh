#!/bin/sh
# P03.1 Linux 验收脚本：预检 DTO、120 秒期限、动作/参数/身份绑定、再认证、确认短语、快照版本。
#
# 运行位置：Ubuntu 22.04/24.04 验收 VM 内，**必须以 root 运行**。
#   - 代理只能在 root 下启动，且要按 SO_PEERCRED 区分 Web 账号；
#   - Web 必须**以 upanel-web 身份**启动，否则它连不上代理 socket（这是边界的本体，
#     不是测试麻烦）；
#   - 不依赖 Go 工具链：二进制由宿主交叉编译，脚本只做黑盒验收与查库。
#
# 用法：
#   sudo BIN_DIR=/tmp/upanel-bin sh tests/integration/p03-1-preflight-acceptance.sh
#
# 退出码：0 全部通过；1 有失败项；2 环境不满足（非 root）。
#
# 覆盖的验收点（STAGES P03.1、CONTRACTS §4.1/§14.1/§14.2/§15、PLAN §6）：
#   1. 代理以 root 启动、socket 为 root:upanel-web 0660；生产登记表为空，
#      验收动作表只在显式 `-acceptance-actions` 时出现
#   2. POST /api/v1/operations/preflight 返回契约 DTO（字段齐全、blockers 为 []、
#      不需要短语时为 null；不暴露 state_version 与代理侧引用）
#   3. 期限 120 秒：响应 expires_at 与 Web 库一致，代理库里 expires_at-created_at 恰为 120000ms
#   4. 绑定：Web 库记录 user_id / session_hash / action / params_hash / state_version /
#      agent_ref；代理库记录同一 user_id 与 session_hash（两端可对账）
#   5. 参数规范化：只有空白差异 → 同一 params_hash；内容不同 → 不同 params_hash
#   6. 再认证与确认短语由登记表决定：acceptance.probe 要求两者，acceptance.ping 都不要求
#   7. 短语只在响应里出现一次：代理库只有哈希，Web 库没有短语，日志里没有明文
#   8. 权限：未登录 401、viewer 403、缺 CSRF 403；被拒绝的请求不落库
#   9. 未登记动作 → 400 VALIDATION_ERROR 且两端都不落库；代理停止后 → 503 AGENT_UNAVAILABLE
#  10. Web 进程与 Web 库都不属于 root（预检不改变这一点）

set -u

BIN_DIR=${BIN_DIR:-/tmp/upanel-bin}
if [ "$(id -u)" -ne 0 ]; then
  printf '本脚本必须以 root 运行：sudo BIN_DIR=%s sh %s\n' "$BIN_DIR" "$0" >&2
  exit 2
fi

AGENT="$BIN_DIR/upanel-agent"
WEB="$BIN_DIR/upanel-web"
CTL="$BIN_DIR/upanelctl"

ROOT=$(cd "$(dirname "$0")/../.." && pwd)
WORK=${WORK:-/var/tmp/upanel-accept-p03-1}
RUN_DIR=/run/upanel
SOCK=$RUN_DIR/agent.sock
SYS_CFG=/etc/upanel/upanel.json
AGENT_DB_DIR=/var/lib/upanel-agent
AGENT_DB=$AGENT_DB_DIR/agent.db
KEY_FILE=/etc/upanel/agent.key
WEB_DB_DIR=$WORK/webdata
WEB_DB=$WEB_DB_DIR/upanel.db
PORT=19443
BASE="http://127.0.0.1:$PORT"
ORIGIN="$BASE"
WEB_USER=upanel-web
PASSWORD='vm-correct-horse-9'
VIEWER_PASSWORD='vm-viewer-horse-42'

AGENT_LOG=$WORK/logs/agent.log
WEB_LOG=$WORK/logs/web.log

PASSED=0
FAILED=0
FAILED_NAMES=""
CODE=""
AGENT_PID=""
WEB_PID=""

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

expect_not_contains() {
  case $3 in
    *"$2"*) fail "$1" "不应出现 '$2'" ;;
    *) pass "$1" ;;
  esac
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
  if [ -n "$WEB_PID" ] && kill -0 "$WEB_PID" 2>/dev/null; then
    kill -TERM "$WEB_PID" 2>/dev/null
    wait "$WEB_PID" 2>/dev/null
  fi
  if [ -n "$AGENT_PID" ] && kill -0 "$AGENT_PID" 2>/dev/null; then
    kill -TERM "$AGENT_PID" 2>/dev/null
    wait "$AGENT_PID" 2>/dev/null
  fi
  rm -f "$SOCK"
}
trap cleanup EXIT
trap 'cleanup; exit 130' INT
trap 'cleanup; exit 143' TERM

start_agent() {
  "$AGENT" --config "$SYS_CFG" -acceptance-actions >>"$AGENT_LOG" 2>&1 &
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

# login <用户名> <口令> <jar 文件> <标签>：响应写到 $WORK/<标签>.json，回显 HTTP 状态码。
login() {
  curl -s -c "$3" -o "$WORK/$4.json" -w '%{http_code}' -X POST "$BASE/api/v1/auth/login" \
    -H 'Content-Type: application/json' -H 'X-UPanel-Request: 1' -H "Origin: $ORIGIN" \
    -d "{\"username\":\"$1\",\"password\":\"$2\"}"
}

csrf_of() { jget "$1" 'd["data"]["csrf_token"]'; }

# api <jar> <csrf> <方法> <路径> [body]：响应写到 $WORK/resp.json，回显 HTTP 状态码。
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

PREFLIGHT_PATH=/api/v1/operations/preflight

echo "== 0. 环境"
if [ "$(id -u)" -eq 0 ]; then pass "验收脚本以 root 运行"; else fail "验收脚本以 root 运行" "当前 UID $(id -u)"; fi
for b in "$AGENT" "$WEB" "$CTL"; do
  if [ -x "$b" ]; then pass "二进制存在且可执行 $(basename "$b")"; else fail "二进制存在且可执行 $(basename "$b")" "$b"; fi
done
if command -v python3 >/dev/null 2>&1; then pass "python3 可用于查库"; else fail "python3 可用于查库" "缺少 python3"; fi

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
mkdir -p "$WORK/logs" "$WEB_DB_DIR"
chmod 0755 "$WORK"
chown -R "$WEB_USER" "$WEB_DB_DIR" "$WORK/logs"

echo
echo "== 1. 启动代理（验收动作表） =="
install -d -m 0755 /etc/upanel
install -d -m 0700 "$AGENT_DB_DIR"
# 代理库每轮从空库开始：上一轮留下的计划会让“被拒绝的请求不落库”这类计数失去意义。
rm -f "$AGENT_DB" "$AGENT_DB-wal" "$AGENT_DB-shm"
install -d -m 0755 "$RUN_DIR"
chmod 0755 "$RUN_DIR"
chown root:root "$RUN_DIR"
expect_eq "socket 目录属主 root" "root" "$(stat -c '%U' "$RUN_DIR")"
expect_eq "socket 目录权限 755" "755" "$(stat -c '%a' "$RUN_DIR")"

cat >"$SYS_CFG" <<'JSON'
{
  "web": {
    "mode": "production",
    "listen_address": "127.0.0.1:19443",
    "shutdown_timeout_seconds": 10
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
    "path": "/var/lib/upanel/upanel.db",
    "busy_timeout_ms": 5000
  },
  "log": {
    "level": "info",
    "format": "json"
  }
}
JSON
chmod 0644 "$SYS_CFG"

# 本轮不验证密钥生命周期（P02.4 已覆盖），只准备一把可用的密钥；不覆盖任何已存在的密钥。
rm -f "$KEY_FILE"
if "$AGENT" --config "$SYS_CFG" -init-key >"$WORK/init-key.log" 2>&1; then
  pass "-init-key 生成代理密钥"
else
  fail "-init-key 生成代理密钥" "$(tail -3 "$WORK/init-key.log" 2>/dev/null)"
fi
expect_eq "密钥文件属主 root" "root" "$(stat -c '%U' "$KEY_FILE" 2>/dev/null)"
expect_eq "密钥文件权限 600" "600" "$(stat -c '%a' "$KEY_FILE" 2>/dev/null)"
expect_eq "代理库目录权限 700" "700" "$(stat -c '%a' "$AGENT_DB_DIR")"

if start_agent; then pass "代理以 root 启动并创建 socket"; else fail "代理以 root 启动" "$(tail -5 "$AGENT_LOG" 2>/dev/null)"; fi
expect_eq "socket 属组" "$WEB_USER" "$(stat -c '%G' "$SOCK" 2>/dev/null)"
expect_eq "socket 权限 660" "660" "$(stat -c '%a' "$SOCK" 2>/dev/null)"
expect_eq "代理进程 uid=0" "0" "$(ps -o uid= -p "$AGENT_PID" 2>/dev/null | tr -d ' ')"
expect_contains "代理已启用验收动作表" "已启用验收专用动作表" "$(cat "$AGENT_LOG")"
expect_contains "代理日志记录预检 TTL" '"preflight_ttl_seconds":120' "$(cat "$AGENT_LOG")"

# capabilities 必须如实报告验收动作；生产表本身仍为空（另一份断言在 p02-3，此处只核对开关）。
caps=$(runuser -u "$WEB_USER" -- curl -sS --unix-socket "$SOCK" http://agent/internal/v1/capabilities 2>/dev/null)
expect_contains "capabilities 含 acceptance.probe" "acceptance.probe" "$caps"
expect_contains "capabilities 含 acceptance.ping" "acceptance.ping" "$caps"

echo
echo "== 2. 管理员与 Web 启动（非 root） =="
INIT_OUT=$(printf '%s\n%s\n' "$PASSWORD" "$PASSWORD" | "$CTL" init-admin --config "$SYS_CFG" --db "$WEB_DB" \
  --username admin --display-name "管理员" --json 2>"$WORK/init.err")
expect_eq "init-admin 退出码" "0" "$?"
ADMIN_ID=$(printf '%s' "$INIT_OUT" | python3 -c 'import json,sys; print(json.load(sys.stdin)["user_id"])' 2>/dev/null)
if [ -n "$ADMIN_ID" ] && [ "$ADMIN_ID" -gt 0 ] 2>/dev/null; then pass "管理员 user_id=$ADMIN_ID"; else fail "管理员 user_id" "$INIT_OUT"; fi
chown "$WEB_USER" "$WEB_DB"
for suffix in "" "-wal" "-shm"; do
  [ -e "$WEB_DB$suffix" ] && chown "$WEB_USER" "$WEB_DB$suffix"
done

if start_web; then pass "Web 以 $WEB_USER 身份在 $BASE 启动"; else fail "Web 启动" "$(tail -5 "$WEB_LOG" 2>/dev/null)"; fi
# $WEB_PID 是 runuser 自己（必然 root）；真正提供服务的是它 fork 出来的 Web 进程。
WEB_CHILD_PID=$(pgrep -u "$WEB_USER" -f "upanel-web --mode production --db $WEB_DB" | head -n 1)
WEB_UID=$(ps -o uid= -p "${WEB_CHILD_PID:-0}" 2>/dev/null | tr -d ' ')
WEB_EXPECT_UID=$(id -u "$WEB_USER")
if [ -n "$WEB_UID" ] && [ "$WEB_UID" = "$WEB_EXPECT_UID" ]; then
  pass "Web 进程 uid=$WEB_UID（$WEB_USER，非 root）"
else
  fail "Web 进程以 $WEB_USER 运行" "uid=$WEB_UID，期望 $WEB_EXPECT_UID"
fi
# 注意不能用 pgrep -f：runuser 自己的命令行里也带着 upanel-web 的参数串。
# 按“可执行文件名 + 属主 UID”判断才准确。
ROOT_WEB=$(ps -eo uid=,comm= | awk '$1 == 0 && $2 == "upanel-web"' | head -n 1)
expect_eq "没有以 root 运行的 Web 进程" "" "$ROOT_WEB"
expect_eq "Web 库属主是 $WEB_USER" "$WEB_USER" "$(stat -c '%U' "$WEB_DB")"

code=$(login admin "$PASSWORD" "$WORK/admin.jar" login-admin)
expect_eq "管理员登录 200" "200" "$code"
ADMIN_CSRF=$(csrf_of "$WORK/login-admin.json")
if [ -n "$ADMIN_CSRF" ]; then pass "登录返回 CSRF 令牌"; else fail "登录返回 CSRF 令牌" "空"; fi

echo
echo "== 3. 预检主流程（acceptance.probe） =="
code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "$PREFLIGHT_PATH" '{"action":"acceptance.probe","params":{"label":"alpha"}}')
expect_eq "预检返回 200" "200" "$code"
cp "$WORK/resp.json" "$WORK/pre-probe.json"

expect_eq "action 回显" "acceptance.probe" "$(jget "$WORK/pre-probe.json" 'd["data"]["action"]')"
expect_eq "allowed 为 true" "True" "$(jget "$WORK/pre-probe.json" 'd["data"]["allowed"]')"
expect_eq "requires_reauth 为 true" "True" "$(jget "$WORK/pre-probe.json" 'd["data"]["requires_reauth"]')"
expect_eq "cancellable 为 false" "False" "$(jget "$WORK/pre-probe.json" 'd["data"]["cancellable"]')"
expect_eq "blockers 是空数组" "[]" "$(jget "$WORK/pre-probe.json" 'json.dumps(d["data"]["blockers"])')"
expect_eq "locks 只有 acceptance:probe" '["acceptance:probe"]' "$(jget "$WORK/pre-probe.json" 'json.dumps(d["data"]["locks"])')"
expect_eq "warnings 标记验收专用" "acceptance_only" "$(jget "$WORK/pre-probe.json" 'd["data"]["warnings"][0]["code"]')"
expect_eq "impacts 说明不改动系统" "none" "$(jget "$WORK/pre-probe.json" 'd["data"]["impacts"][0]["kind"]')"
expect_contains "target 有展示名" "alpha" "$(jget "$WORK/pre-probe.json" 'd["data"]["target"]["display_name"]')"
if [ -n "$(jget "$WORK/pre-probe.json" 'd["data"]["target"]["identity_fingerprint"]')" ]; then
  pass "target 有身份指纹"
else
  fail "target 有身份指纹" "空"
fi
PREFLIGHT_ID=$(jget "$WORK/pre-probe.json" 'd["data"]["preflight_id"]')
case $PREFLIGHT_ID in
  pf_*) pass "preflight_id 形态为 pf_<hex>" ;;
  *) fail "preflight_id 形态为 pf_<hex>" "$PREFLIGHT_ID" ;;
esac

# 确认短语：服务端生成，形如 "PROBE K7M2QX"（无 I/O/0/1）。
PHRASE=$(jget "$WORK/pre-probe.json" 'd["data"]["confirmation_phrase"]')
if printf '%s' "$PHRASE" | grep -Eq '^PROBE [ABCDEFGHJKLMNPQRSTUVWXYZ23456789]{6}$'; then
  pass "确认短语形态为 PROBE + 6 位无歧义码"
else
  fail "确认短语形态" "$PHRASE"
fi

# 公开 DTO 不得暴露内部绑定信息。
AGENT_REF_FOR_CHECK=$(dbq "$WEB_DB" "SELECT agent_ref FROM preflights WHERE id = '$PREFLIGHT_ID'")
expect_not_contains "响应不含代理侧计划引用" "$AGENT_REF_FOR_CHECK" "$(cat "$WORK/resp.json")"
expect_not_contains "响应不含 state_version" "state_version" "$(cat "$WORK/resp.json")"

echo
echo "== 4. 期限 120 秒与两端绑定 =="
EXPIRES_AT=$(jget "$WORK/pre-probe.json" 'd["data"]["expires_at"]')
TTL_DELTA=$(python3 - "$EXPIRES_AT" <<'PY'
import datetime, sys
expires = datetime.datetime.strptime(sys.argv[1], "%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=datetime.timezone.utc)
now = datetime.datetime.now(datetime.timezone.utc)
print(int((expires - now).total_seconds()))
PY
)
if [ "$TTL_DELTA" -ge 115 ] && [ "$TTL_DELTA" -le 121 ]; then
  pass "expires_at 距今 ${TTL_DELTA}s（120 秒有效期）"
else
  fail "expires_at 距今 ${TTL_DELTA}s" "期望 115—121"
fi

AGENT_REF=$(dbq "$WEB_DB" "SELECT agent_ref FROM preflights WHERE id = '$PREFLIGHT_ID'")
case $AGENT_REF in
  pf_*) pass "Web 库记录了代理侧计划引用" ;;
  *) fail "Web 库记录了代理侧计划引用" "$AGENT_REF" ;;
esac

expect_eq "Web 库 expires_at 与响应一致" "$EXPIRES_AT" \
  "$(dbq "$WEB_DB" "SELECT expires_at FROM preflights WHERE id = '$PREFLIGHT_ID'")"
expect_eq "代理库期限恰为 120000ms" "120000" \
  "$(dbq "$AGENT_DB" "SELECT expires_at - created_at FROM preflight_plans WHERE id = '$AGENT_REF'")"
expect_eq "代理库 expires_at 与响应一致" "$EXPIRES_AT" \
  "$(dbq "$AGENT_DB" "SELECT strftime('%Y-%m-%dT%H:%M:%SZ', expires_at/1000, 'unixepoch') FROM preflight_plans WHERE id = '$AGENT_REF'")"

expect_eq "Web 库 user_id 是登录账号" "$ADMIN_ID" \
  "$(dbq "$WEB_DB" "SELECT user_id FROM preflights WHERE id = '$PREFLIGHT_ID'")"
expect_eq "代理库 user_id 与 Web 库一致" "$ADMIN_ID" \
  "$(dbq "$AGENT_DB" "SELECT user_id FROM preflight_plans WHERE id = '$AGENT_REF'")"
expect_eq "两端 session_hash 一致" "$(dbq "$WEB_DB" "SELECT session_hash FROM preflights WHERE id = '$PREFLIGHT_ID'")" \
  "$(dbq "$AGENT_DB" "SELECT session_hash FROM preflight_plans WHERE id = '$AGENT_REF'")"
expect_eq "session_hash 为 64 位十六进制" "64" \
  "$(dbq "$WEB_DB" "SELECT length(session_hash) FROM preflights WHERE id = '$PREFLIGHT_ID'")"
expect_eq "params_hash 为 64 位十六进制" "64" \
  "$(dbq "$WEB_DB" "SELECT length(params_hash) FROM preflights WHERE id = '$PREFLIGHT_ID'")"
expect_eq "代理库记录状态版本" "1" \
  "$(dbq "$AGENT_DB" "SELECT state_version FROM preflight_plans WHERE id = '$AGENT_REF'")"
expect_eq "Web 库记录状态版本" "1" \
  "$(dbq "$WEB_DB" "SELECT state_version FROM preflights WHERE id = '$PREFLIGHT_ID'")"
expect_eq "预检未被消费（consumed_job_id 为空）" "" \
  "$(dbq "$WEB_DB" "SELECT consumed_job_id FROM preflights WHERE id = '$PREFLIGHT_ID'")"
expect_eq "代理库计划未被使用（used_at=0）" "0" \
  "$(dbq "$AGENT_DB" "SELECT used_at FROM preflight_plans WHERE id = '$AGENT_REF'")"

echo
echo "== 5. 参数规范化与哈希绑定 =="
code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "$PREFLIGHT_PATH" '{"action":"acceptance.probe","params":{ "label" : "alpha" }}')
expect_eq "带空白的同参数预检返回 200" "200" "$code"
ID_SPACED=$(jget "$WORK/resp.json" 'd["data"]["preflight_id"]')
HASH_COMPACT=$(dbq "$WEB_DB" "SELECT params_hash FROM preflights WHERE id = '$PREFLIGHT_ID'")
HASH_SPACED=$(dbq "$WEB_DB" "SELECT params_hash FROM preflights WHERE id = '$ID_SPACED'")
expect_eq "只有空白差异得到同一 params_hash" "$HASH_COMPACT" "$HASH_SPACED"

code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "$PREFLIGHT_PATH" '{"action":"acceptance.probe","params":{"label":"beta"}}')
expect_eq "不同参数预检返回 200" "200" "$code"
ID_BETA=$(jget "$WORK/resp.json" 'd["data"]["preflight_id"]')
HASH_BETA=$(dbq "$WEB_DB" "SELECT params_hash FROM preflights WHERE id = '$ID_BETA'")
if [ "$HASH_BETA" != "$HASH_COMPACT" ]; then pass "不同参数得到不同 params_hash"; else fail "不同参数得到不同 params_hash" "$HASH_BETA"; fi

echo
echo "== 6. 策略由登记表决定（acceptance.ping 对照） =="
code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "$PREFLIGHT_PATH" '{"action":"acceptance.ping","params":{}}')
expect_eq "ping 预检返回 200" "200" "$code"
expect_eq "ping 不要求再认证" "False" "$(jget "$WORK/resp.json" 'd["data"]["requires_reauth"]')"
expect_eq "ping 不需要短语（字段为 null）" "None" "$(jget "$WORK/resp.json" 'd["data"]["confirmation_phrase"]')"
expect_eq "ping 允许取消" "True" "$(jget "$WORK/resp.json" 'd["data"]["cancellable"]')"
expect_contains "ping 不需要短语时字段仍存在" '"confirmation_phrase":null' "$(cat "$WORK/resp.json")"

echo
echo "== 7. 短语只出现一次：库里只有哈希 =="
expect_eq "代理库只存 64 位短语哈希" "64" \
  "$(dbq "$AGENT_DB" "SELECT length(phrase_hash) FROM preflight_plans WHERE id = '$AGENT_REF'")"
if [ "$(dbq "$AGENT_DB" "SELECT phrase_hash FROM preflight_plans WHERE id = '$AGENT_REF'")" = "$PHRASE" ]; then
  fail "代理库不存短语明文" "库中等于明文"
else
  pass "代理库不存短语明文"
fi
# 直接在数据库文件里搜短语：只查库表是不够的——短语可能落在 WAL、空闲页或索引里。
if grep -a -q -F "$PHRASE" "$AGENT_DB" 2>/dev/null; then
  fail "代理数据库文件中搜不到短语" "命中了 $PHRASE"
else
  pass "代理数据库文件中搜不到短语"
fi
for suffix in -wal -shm; do
  if [ -e "$AGENT_DB$suffix" ] && grep -a -q -F "$PHRASE" "$AGENT_DB$suffix" 2>/dev/null; then
    fail "代理数据库${suffix} 中搜不到短语" "命中"
  fi
done
# 表里唯一与短语有关的列是 requires_phrase（布尔标记），没有存放短语本身的列。
expect_eq "Web 库只有 requires_phrase，没有短语内容列" "requires_phrase" \
  "$(dbq "$WEB_DB" "SELECT group_concat(name, ',') FROM pragma_table_info('preflights') WHERE name LIKE '%phrase%'")"
expect_not_contains "代理日志没有短语明文" "$PHRASE" "$(cat "$AGENT_LOG")"
expect_not_contains "Web 日志没有短语明文" "$PHRASE" "$(cat "$WEB_LOG")"
expect_contains "代理日志记录了预检事件" "已生成预检计划" "$(cat "$AGENT_LOG")"
expect_contains "Web 日志记录了预检登记" "已登记预检" "$(cat "$WEB_LOG")"

echo
echo "== 8. 权限：未登录 / viewer / 缺 CSRF =="
anon_code=$(curl -s -o "$WORK/anon.json" -w '%{http_code}' -X POST "$BASE$PREFLIGHT_PATH" \
  -H 'Content-Type: application/json' -H "Origin: $ORIGIN" -d '{"action":"acceptance.probe","params":{}}')
expect_eq "未登录预检返回 401" "401" "$anon_code"

code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST /api/v1/users \
  "{\"username\":\"viewer1\",\"display_name\":\"Viewer\",\"role\":\"viewer\",\"initial_password\":\"$VIEWER_PASSWORD\"}")
expect_eq "创建 viewer 返回 201" "201" "$code"
code=$(login viewer1 "$VIEWER_PASSWORD" "$WORK/viewer.jar" login-viewer)
expect_eq "viewer 登录 200" "200" "$code"
VIEWER_CSRF=$(csrf_of "$WORK/login-viewer.json")
code=$(api "$WORK/viewer.jar" "$VIEWER_CSRF" POST "$PREFLIGHT_PATH" '{"action":"acceptance.probe","params":{}}')
expect_eq "viewer 预检返回 403" "403" "$code"
expect_contains "viewer 被拒的错误码" "FORBIDDEN" "$(cat "$WORK/resp.json")"

csrf_missing=$(curl -s -b "$WORK/admin.jar" -o "$WORK/nocsrf.json" -w '%{http_code}' -X POST "$BASE$PREFLIGHT_PATH" \
  -H 'Content-Type: application/json' -H "Origin: $ORIGIN" -d '{"action":"acceptance.probe","params":{}}')
expect_eq "缺 CSRF 预检返回 403" "403" "$csrf_missing"
expect_contains "缺 CSRF 的错误码" "CSRF_FAILED" "$(cat "$WORK/nocsrf.json")"

before_rows=$(dbq "$WEB_DB" "SELECT COUNT(*) FROM preflights")
expect_eq "被拒绝的请求没有落库" "$before_rows" "$(dbq "$WEB_DB" "SELECT COUNT(*) FROM preflights")"

echo
echo "== 9. 请求形状与未登记动作 =="
code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "$PREFLIGHT_PATH" '{"action":"acceptance.probe","params":{},"allowed":true}')
expect_eq "夹带计划外字段返回 400" "400" "$code"
expect_contains "夹带字段的错误码" "VALIDATION_ERROR" "$(cat "$WORK/resp.json")"

code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "$PREFLIGHT_PATH" '{"action":"storage.volume.mount","params":{}}')
expect_eq "未登记动作返回 400" "400" "$code"
expect_contains "未登记动作的错误码" "VALIDATION_ERROR" "$(cat "$WORK/resp.json")"
expect_contains "未登记动作的错误信息可读" "未登记的动作" "$(cat "$WORK/resp.json")"

code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "$PREFLIGHT_PATH" '{"action":"format /dev/sdb","params":{}}')
expect_eq "非法动作名返回 400" "400" "$code"

agent_rows=$(dbq "$AGENT_DB" "SELECT COUNT(*) FROM preflight_plans")
expect_eq "被拒绝的请求在代理库也没有记录" "0" \
  "$(dbq "$AGENT_DB" "SELECT COUNT(*) FROM preflight_plans WHERE action NOT LIKE 'acceptance.%'")"
if [ "$agent_rows" = "4" ]; then
  pass "代理库恰好保存 4 条成功预检（probe、带空白 probe、beta、ping）"
else
  fail "代理库保存成功的预检" "有 $agent_rows 条，期望 4"
fi

echo
echo "== 10. 代理不可用 =="
stop_agent
code=$(api "$WORK/admin.jar" "$ADMIN_CSRF" POST "$PREFLIGHT_PATH" '{"action":"acceptance.probe","params":{}}')
expect_eq "代理停止后返回 503" "503" "$code"
expect_contains "代理不可用的错误码" "AGENT_UNAVAILABLE" "$(cat "$WORK/resp.json")"
after_rows=$(dbq "$WEB_DB" "SELECT COUNT(*) FROM preflights")
expect_eq "代理不可用时没有落库" "$before_rows" "$after_rows"

echo
echo "== 11. 汇总 =="
printf '通过 %d 项，失败 %d 项\n' "$PASSED" "$FAILED"
if [ "$FAILED" -ne 0 ]; then
  printf '失败项:%s\n' "$FAILED_NAMES" >&2
  exit 1
fi
printf 'P03.1 验收全部通过\n'
exit 0
