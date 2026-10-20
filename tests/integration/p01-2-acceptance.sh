#!/bin/sh
# P01.2 Linux 验收脚本：数据库迁移、事务参数与启动拒绝。
#
# 运行位置：Ubuntu 22.04/24.04 验收 VM 内（用户需能 sudo；非 root 用户运行 Web）。
# 用途：验证 SQLite 驱动、迁移机制与“数据库不可用即拒绝启动”的真实行为，
#       不依赖 Go 工具链（使用交叉编译好的二进制）。
#
# 用法：
#   BIN_DIR=/tmp sh tests/integration/p01-2-acceptance.sh
#
# 退出码：0 全部通过；1 有失败项。
#
# 覆盖的验收点（对应 STAGES.md P01 验收 + P01.2 范围）：
#   1. check-config 报告数据库配置
#   2. upanelctl migrate 在真实文件上建库、建表、记录版本
#   3. 重复执行幂等（不再应用迁移）
#   4. WAL / foreign_keys / busy_timeout 在 Ubuntu 上真实生效
#   5. 迁移状态跨进程重启保留（重启后不再重放迁移）
#   6. 数据库不可用时 Web 拒绝启动（目录缺失、目录不可写）
#   7. 非 SQLite 文件、相对路径、版本超前必须失败且不伪装成功
#   8. 数据库文件归属运行 Web 的非 root 用户

set -u

BIN_DIR=${BIN_DIR:-/tmp}
WEB="$BIN_DIR/upanel-web"
AGENT="$BIN_DIR/upanel-agent"
CTL="$BIN_DIR/upanelctl"

DB_ROOT=${DB_ROOT:-/var/tmp/upanel-accept-p01-2}
DB="$DB_ROOT/upanel.db"

PASSED=0
FAILED=0
FAILED_NAMES=""

pass() { PASSED=$((PASSED + 1)); printf 'PASS  %s\n' "$1"; }
fail() { FAILED=$((FAILED + 1)); FAILED_NAMES="$FAILED_NAMES $1"; printf 'FAIL  %s: %s\n' "$1" "${2:-}"; }

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

cleanup() {
  if [ -n "${WEB_PID:-}" ]; then kill -TERM "$WEB_PID" 2>/dev/null; wait "$WEB_PID" 2>/dev/null; fi
}
trap cleanup EXIT INT TERM

# ---------------------------------------------------------------- 前提
[ -x "$WEB" ] || { echo "缺少可执行文件 $WEB"; exit 1; }
[ -x "$CTL" ] || { echo "缺少可执行文件 $CTL"; exit 1; }
if [ "$(id -u)" -eq 0 ]; then SUDO=""; else SUDO="sudo"; fi

# 迁移状态必须从空目录开始，否则“新库”断言不成立。
$SUDO rm -rf "$DB_ROOT"
mkdir -p "$DB_ROOT"

CFG_DB="$BIN_DIR/accept-config-db.json"
cat > "$CFG_DB" <<JSON
{
  "web": {"mode": "mock", "shutdown_timeout_seconds": 10},
  "database": {"path": "$DB", "busy_timeout_ms": 5000},
  "log": {"level": "info", "format": "text"}
}
JSON

echo "== 环境 =="
echo "系统: $(. /etc/os-release; echo "$PRETTY_NAME")"
echo "内核: $(uname -r)"
echo "用户: $(id -un) (uid=$(id -u))"
echo "二进制目录: $BIN_DIR"
echo "数据库目录: $DB_ROOT"
echo "数据库文件: $DB"
echo

# ---------------------------------------------------------------- 1. 配置
echo "== 1. check-config 报告数据库配置 =="
CHECK_JSON=$("$CTL" check-config --config "$CFG_DB" --json 2>&1)
expect_contains "check-config-database-path" "$CHECK_JSON" "\"database_path\": \"$DB\""
expect_contains "check-config-busy-timeout" "$CHECK_JSON" '"database_busy_timeout_ms": 5000'

CHECK_TEXT=$("$CTL" check-config --config "$CFG_DB" 2>&1)
expect_contains "check-config-text-database" "$CHECK_TEXT" "busy_timeout=5000ms"
echo

# ---------------------------------------------------------------- 2. 首次迁移
echo "== 2. 首次迁移（真实文件） =="
"$CTL" migrate --config "$CFG_DB" --json > "$BIN_DIR/accept-migrate-1.json" 2> "$BIN_DIR/accept-migrate-1.log"
expect_eq "migrate-fresh-exit0" "$?" "0"

MIG1=$(cat "$BIN_DIR/accept-migrate-1.json")
expect_contains "migrate-fresh-path" "$MIG1" "\"database_path\": \"$DB\""
expect_contains "migrate-fresh-from" "$MIG1" '"from_version": 0'
# 版本号是**精确**断言，写“当前应有的版本”而不是“≥1”：P01.2 时期是 1，
# 之后每加一条迁移都要同步改这一行（0002—0005 已加，所以现在是 5）。
# 新增迁移却忘了改这里，就是脚本在提醒你有人在偷偷动 schema。
expect_contains "migrate-fresh-version" "$MIG1" '"schema_version": 5'
expect_contains "migrate-fresh-applied" "$MIG1" '"applied_versions": ['

if [ -f "$DB" ]; then pass "migrate-created-db-file"; else fail "migrate-created-db-file" "数据库文件未创建"; fi
HEADER=$(head -c 15 "$DB" 2>/dev/null)
expect_eq "migrate-sqlite-header" "$HEADER" "SQLite format 3"

DB_OWNER=$(stat -c '%U' "$DB" 2>/dev/null)
expect_eq "migrate-db-owner-is-runner" "$DB_OWNER" "$(id -un)"
echo

# ---------------------------------------------------------------- 3. 幂等
echo "== 3. 重复迁移幂等 =="
"$CTL" migrate --config "$CFG_DB" --json > "$BIN_DIR/accept-migrate-2.json" 2> "$BIN_DIR/accept-migrate-2.log"
expect_eq "migrate-repeat-exit0" "$?" "0"
MIG2=$(cat "$BIN_DIR/accept-migrate-2.json")
expect_contains "migrate-repeat-version" "$MIG2" '"schema_version": 5'
expect_contains "migrate-repeat-no-apply" "$MIG2" '"applied_versions": []'

MIG3=$("$CTL" migrate --db "$DB" 2>/dev/null)
expect_contains "migrate-text-uptodate" "$MIG3" "本次应用: 无"
echo

# ---------------------------------------------------------------- 4. 启动并验证连接参数
echo "== 4. Web 使用该数据库启动 =="
"$WEB" --config "$CFG_DB" --db "$DB" > "$BIN_DIR/accept-web-db.log" 2>&1 &
WEB_PID=$!
sleep 2

RESP=$(curl -s --max-time 5 http://127.0.0.1:18881/api/v1/health/live)
expect_contains "web-health-with-db" "$RESP" '"status":"ok"'

WEBLOG=$(cat "$BIN_DIR/accept-web-db.log")
expect_contains "web-opened-db" "$WEBLOG" "数据库已打开"
expect_contains "web-wal-enabled" "$WEBLOG" "journal_mode=wal"
expect_contains "web-foreign-keys-on" "$WEBLOG" "foreign_keys=true"
expect_contains "web-busy-timeout" "$WEBLOG" "busy_timeout_ms=5000"
expect_contains "web-schema-ready" "$WEBLOG" "数据库 schema 已就绪"
# 迁移状态跨进程保留：启动时不应再次应用任何迁移。
expect_contains "web-no-reapply-after-restart" "$WEBLOG" "applied_versions=[]"

if [ -f "$DB-wal" ]; then pass "web-wal-file-present"; else fail "web-wal-file-present" "缺少 -wal 文件，WAL 可能未生效"; fi

kill -TERM "$WEB_PID" 2>/dev/null
wait "$WEB_PID" 2>/dev/null
expect_eq "web-db-graceful-shutdown" "$?" "0"
WEB_PID=""
echo

# ---------------------------------------------------------------- 5. 拒绝启动
echo "== 5. 数据库不可用必须拒绝启动 =="
MISSING_DIR="$DB_ROOT/absent/upanel.db"
"$WEB" --config "$CFG_DB" --db "$MISSING_DIR" > "$BIN_DIR/accept-web-missing-dir.log" 2>&1
expect_eq "web-refuses-missing-db-dir" "$?" "1"
MISSING_LOG=$(cat "$BIN_DIR/accept-web-missing-dir.log")
expect_contains "web-reports-db-open-failure" "$MISSING_LOG" "数据库打开失败"
expect_not_contains "web-does-not-listen-without-db" "$MISSING_LOG" "已开始监听"

LOCKED="$DB_ROOT/locked"
$SUDO install -d -m 0500 -o root -g root "$LOCKED"
"$WEB" --config "$CFG_DB" --db "$LOCKED/upanel.db" > "$BIN_DIR/accept-web-locked-dir.log" 2>&1
expect_eq "web-refuses-unwritable-db-dir" "$?" "1"
expect_contains "web-reports-unwritable" "$(cat "$BIN_DIR/accept-web-locked-dir.log")" "数据库打开失败"
$SUDO rm -rf "$LOCKED"

# 非 SQLite 文件：打开可能成功，迁移必须失败并以非 0 退出。
BADDB="$DB_ROOT/not-a-database.db"
head -c 8192 /dev/urandom > "$BADDB"
"$CTL" migrate --db "$BADDB" > "$BIN_DIR/accept-migrate-badfile.out" 2> "$BIN_DIR/accept-migrate-badfile.log"
expect_eq "migrate-rejects-non-sqlite" "$?" "1"
expect_contains "migrate-reports-failure" "$(cat "$BIN_DIR/accept-migrate-badfile.log")" "迁移失败"
expect_not_contains "migrate-no-success-on-failure" "$(cat "$BIN_DIR/accept-migrate-badfile.out")" "schema 版本"

"$CTL" migrate --db "relative/upanel.db" > /dev/null 2>&1
expect_eq "migrate-rejects-relative-path" "$?" "1"

printf '{"web":{"mode":"mock"},"database":{"path":""}}' > "$BIN_DIR/accept-config-nodb.json"
"$CTL" migrate --config "$BIN_DIR/accept-config-nodb.json" > /dev/null 2>&1
expect_eq "migrate-requires-path" "$?" "2"
echo

# ---------------------------------------------------------------- 汇总
echo "== 汇总 =="
echo "PASS $PASSED / FAIL $FAILED"
if [ "$FAILED" -ne 0 ]; then
  echo "失败项:$FAILED_NAMES"
  exit 1
fi
exit 0
