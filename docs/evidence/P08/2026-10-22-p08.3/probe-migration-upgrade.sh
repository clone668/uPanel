#!/bin/sh
# 迁移升级路径探针（P08.3，Q240 的回归证据）。
#
# 主验收脚本每次都把代理库删掉重建，所以它证明的是“全新建库能带上 runtime_revoked_json”。
# 而真机上的缺陷形态是**已存在的库**：P08.2 时期建的 agent.db 里 shares 只有 15 列，
# 升级到带 0006 的二进制时必须能就地补列，而且旧行要拿到 DEFAULT（空串 = 没撤销过）。
#
# 这个探针只碰自己的临时目录（/var/tmp/upanel-mig-probe），全程不碰 /var/lib/upanel-agent：
#   1. 用当前二进制在临时状态目录里建库（0001..0006 全部应用）；
#   2. 塞一行共享，再把 runtime_revoked_json 列 DROP 掉、把最高版本那条迁移记录删掉，
#      用 pragma_table_info 证明“现在就是升级前的 15 列形状”；
#   3. 再起一次代理，看它是否把 0006 补上、旧行在该列上是不是空串、
#      并且**没有**重新应用其他迁移（最高版本正好从 5 变成 6）。
#
# 不做的事：不伪造“从 P08.2 的真实库升级”——那要一份 P08.2 的二进制。这里用
# DROP COLUMN 手工复原形状，并把“形状是复原的、不是历史原件”写在输出里，不冒充历史证据。
#
# 用法：sudo sh probe-migration-upgrade.sh

set -eu

BIN=${BIN:-/var/tmp/upanel-bin}
PROBE=${PROBE:-/var/tmp/upanel-mig-probe}
DB=$PROBE/agent.db
CFG=$PROBE/agent.json
KEY=$PROBE/agent.key
RUN=$PROBE/run
SOCK=$RUN/agent.sock

say() { printf '%s\n' "$*"; }
section() { printf '\n== %s ==\n' "$*"; }

# 这台虚拟机上没有 sqlite3 命令行（探针用 python3 的 sqlite3 模块，验收脚本本来也依赖它）。
# sq_q <查询>：执行一条语句，把结果按行打印（多列用 | 连）。
sq_q() {
  python3 - "$DB" "$1" <<'PY'
import sqlite3, sys
con = sqlite3.connect(sys.argv[1])
try:
    for row in con.execute(sys.argv[2]).fetchall():
        print("|".join("" if v is None else str(v) for v in row))
except Exception as exc:
    print("ERR:%s:%s" % (type(exc).__name__, exc))
    sys.exit(1)
finally:
    con.commit()
    con.close()
PY
}

# sq_exec：从 stdin 读一段可能有多条语句的 SQL。
#
# 这里必须用 `python3 -c`，不能用 `python3 - <<'PY'`：后者会把**脚本**接到 stdin 上，
# 于是调用者的 heredoc 被顶掉，executescript 读到空字符串——“一行都没插进去”却静默成功。
sq_exec() {
  python3 -c '
import sqlite3, sys
con = sqlite3.connect(sys.argv[1])
try:
    con.executescript(sys.stdin.read())
    con.commit()
except Exception as exc:
    print("ERR:%s:%s" % (type(exc).__name__, exc))
    sys.exit(1)
finally:
    con.close()
' "$DB"
}

section "0. 准备临时状态目录（不碰 /var/lib/upanel-agent）"
rm -rf "$PROBE"
install -d -m 0700 "$PROBE"
install -d -m 0755 "$RUN"
say "探针目录: $PROBE"

cat >"$CFG" <<JSON
{
  "agent": {
    "socket_path": "$SOCK",
    "shutdown_timeout_seconds": 5,
    "web_user": "upanel-web",
    "socket_group": "upanel-web",
    "database_path": "$DB",
    "secret_key_path": "$KEY",
    "storage": { "smart_enabled": false, "smart_interval_minutes": 15, "network_capacity_timeout_seconds": 2 },
    "services": {},
    "logs": {}
  },
  "database": { "path": "$PROBE/web.db", "busy_timeout_ms": 5000 },
  "log": { "level": "info", "format": "json" }
}
JSON
chmod 0600 "$CFG"
"$BIN/upanel-agent" --config "$CFG" -init-key >"$PROBE/init-key.log" 2>&1 \
  || { say "生成密钥失败："; cat "$PROBE/init-key.log"; exit 2; }

start_agent() {
  "$BIN/upanel-agent" --config "$CFG" >"$PROBE/agent-$1.log" 2>&1 &
  _pid=$!
  _i=0
  while [ "$_i" -lt 150 ]; do
    if [ -S "$SOCK" ]; then return 0; fi
    if ! kill -0 "$_pid" 2>/dev/null; then
      say "代理起不来，日志："
      cat "$PROBE/agent-$1.log"
      return 1
    fi
    _i=$((_i + 1))
    sleep 0.1
  done
  say "代理 15 秒内没建出 socket"
  return 1
}

stop_agent() {
  pkill -f "upanel-agent --config $CFG" 2>/dev/null || true
  _i=0
  while [ "$_i" -lt 50 ]; do
    pgrep -f "upanel-agent --config $CFG" >/dev/null 2>&1 || return 0
    _i=$((_i + 1))
    sleep 0.1
  done
  pkill -9 -f "upanel-agent --config $CFG" 2>/dev/null || true
}

section "1. 用当前二进制建库（0001..0006 全部应用）"
start_agent first || exit 2
stop_agent
say "schema_migrations:"
sq_q 'SELECT version, name FROM schema_migrations ORDER BY version;'
say "shares 列数: $(sq_q "SELECT count(*) FROM pragma_table_info('shares');")"
say "runtime_revoked_json 在列里: $(sq_q "SELECT count(*) FROM pragma_table_info('shares') WHERE name='runtime_revoked_json';")"

section "2. 手工复原“升级前”的形状（DROP COLUMN + 删掉最高版本那条迁移记录）"
# 先塞一行数据：升级之后旧行在该列上必须是空串，不能是 NULL——扫描器按空串判“没撤销过”。
sq_exec <<'SQL'
INSERT INTO managed_resources
  (id, kind, display_name, slug, device_id, filesystem_uuid, filesystem_type,
   identity_fingerprint, config_json, desired_state, version, last_operation_id,
   created_at, updated_at)
VALUES
  ('11111111-1111-1111-1111-111111111111', 'volume', '探针卷', 'probe',
   'dev_probe', '22222222-2222-2222-2222-222222222222', 'ext4', 'fp_probe', '{}',
   'mounted', '1', '', 1750000000, 1750000000);
INSERT INTO shares
  (id, name, protocol, volume_id, relative_path, enabled, read_only, definition_json,
   config_hash, directory_prepared, config_applied, version, last_operation_id,
   created_at, updated_at, runtime_revoked_json)
VALUES
  ('shr_33333333333333333333333333333333', 'probe-share', 'smb',
   '11111111-1111-1111-1111-111111111111', 'team', 1, 0,
   '{"name":"probe-share","protocol":"smb","volume_id":"11111111-1111-1111-1111-111111111111","relative_path":"team","enabled":true,"read_only":false,"allowed_cidrs":["10.0.0.0/8"],"smb_users":[{"account_id":"sac_44444444444444444444444444444444","access":"read"}]}',
   '', 0, 0, '1', '', 1750000000, 1750000000,
   '{"at":"2026-10-22T11:42:07Z","volume_id":"11111111-1111-1111-1111-111111111111","volume_name":"探针卷","volume_state":"unmounted","reason":"探针"}');
SQL
SEEDED=$(sq_q 'SELECT count(*) FROM shares;')
say "塞完数据之后 shares 行数: $SEEDED"
# 夹具必须真的落进库里：插不进去而继续往下跑，后面每条断言都会对着空表说话，
# 而“空表上的断言”永远看起来是对的。
[ "$SEEDED" = "1" ] || { say "FAIL 夹具没插进去（executescript 读到了空输入？）"; exit 2; }
sq_q 'ALTER TABLE shares DROP COLUMN runtime_revoked_json;' >/dev/null
say "DROP COLUMN 之后 shares 行数: $(sq_q 'SELECT count(*) FROM shares;')"
sq_q 'DELETE FROM schema_migrations WHERE version = (SELECT MAX(version) FROM schema_migrations);' >/dev/null
say "升级前的 shares 列数: $(sq_q "SELECT count(*) FROM pragma_table_info('shares');")"
say "升级前的最高迁移版本: $(sq_q 'SELECT MAX(version) FROM schema_migrations;')"
say "升级前的列名（复原形状，不是历史原件）:"
sq_q "SELECT group_concat(name, ', ') FROM pragma_table_info('shares');"

section "3. 再起一次代理：0006 应当就位"
start_agent second || exit 2
stop_agent
say "schema_migrations:"
sq_q 'SELECT version, name FROM schema_migrations ORDER BY version;'
COLS=$(sq_q "SELECT count(*) FROM pragma_table_info('shares');")
HAS=$(sq_q "SELECT count(*) FROM pragma_table_info('shares') WHERE name='runtime_revoked_json';")
VAL=$(sq_q "SELECT quote(runtime_revoked_json) FROM shares WHERE id='shr_33333333333333333333333333333333';")
VERSION=$(sq_q 'SELECT MAX(version) FROM schema_migrations;')
KEPT=$(sq_q "SELECT count(*) FROM shares WHERE id='shr_33333333333333333333333333333333';")
say "升级后 shares 列数: $COLS"
say "升级后 runtime_revoked_json 在列里: $HAS"
say "旧行在该列上的值: $VAL（期望 ''）"
say "升级后旧行还在: $KEPT"
say "升级后的最高迁移版本: $VERSION"

FAIL=0
[ "$HAS" = "1" ] || { say "FAIL 升级之后没有补上 runtime_revoked_json 列"; FAIL=$((FAIL + 1)); }
[ "$VAL" = "''" ] || { say "FAIL 旧行在该列上的值不是空串：$VAL（空串才是“没撤销过”，NULL 会让扫描器猜）"; FAIL=$((FAIL + 1)); }
[ "$KEPT" = "1" ] || { say "FAIL 升级把旧数据弄丢了"; FAIL=$((FAIL + 1)); }
[ "$VERSION" = "6" ] || { say "FAIL 最高迁移版本是 $VERSION，期望 6：多应用或漏应用了别的迁移"; FAIL=$((FAIL + 1)); }

printf '\n探针结果：%s\n' "$([ "$FAIL" = 0 ] && echo '通过（升级路径可用）' || echo "失败 $FAIL 条")"
exit "$FAIL"
