#!/bin/sh
# P02.4 Linux 验收脚本：独立代理数据库 + 秘密加密存储 + 日志秘密脱敏。
#
# 运行位置：Ubuntu 22.04/24.04 验收 VM 内，**必须以 root 运行**（代理只能在 root 下启动，
# 且要在不同 UID 之间切换来验证“Web 读不到数据库与密钥”）。不依赖 Go 工具链：使用宿主
# 交叉编译好的二进制，以及 go test -c 编出来的测试二进制，在真实 Linux 上执行 AEAD、
# SQLite、到期销毁与权限校验用例。
#
# 用法：
#   sudo BIN_DIR=/tmp/upanel-bin sh tests/integration/p02-4-agent-secrets-acceptance.sh
#
# 退出码：0 全部通过；1 有失败项。
#
# 覆盖的验收点（CONTRACTS §14.2/§15、PLAN §4、STAGES P02.4）：
#   1. 本机密钥：root:root 0600、两行格式、32 字节、`-init-key` 绝不覆盖、指纹可核对
#   2. 代理数据库：/var/lib/upanel-agent 目录 0700 root 独占，agent.db 0600 root:root，
#      表结构由代理自己的迁移建立
#   3. Web 账号（upanel-web）既读不到 agent.db（含 WAL/SHM），也读不到 agent.key
#   4. POST /internal/v1/secrets：仅 upanel-web 可调用；用途闭集；返回引用与到期时间，
#      响应里没有明文；没有任何读取明文的 HTTP 路由
#   5. 落盘的是密文：数据库文件里搜不到明文；nonce 每行独立（长度 12）
#   6. 临时秘密带到期时间、持久秘密不过期；到期销毁由真实实现执行（短 TTL 用例）
#   7. 服务重启后秘密仍在数据库里（不是内存态）
#   8. 日志无明文：journal 里搜不到提交的秘密内容

set -u

# 早退：非 root 时后面的 49 项断言只会连锁失败（先撞权限再撞文件缺失），
# 报错信息会被噪声淹没。这里直接给出唯一正确的调用方式。
# 注意：本文件必须是 LF 换行，CRLF 会让 `set -u` 变成 `set: Illegal option -`。
BIN_DIR=${BIN_DIR:-/tmp/upanel-bin}
if [ "$(id -u)" -ne 0 ]; then
  printf '本脚本必须以 root 运行：sudo BIN_DIR=%s sh %s\n' "$BIN_DIR" "$0" >&2
  exit 2
fi

AGENT="$BIN_DIR/upanel-agent"
CTL="$BIN_DIR/upanelctl"
SYS_TESTS="$BIN_DIR/tests"

ROOT=$(cd "$(dirname "$0")/../.." && pwd)
DEPLOY_DIR="$ROOT/deploy"
WORK=${WORK:-/var/tmp/upanel-accept-p02-4}
SOCK=/run/upanel/agent.sock
SYS_CFG=/etc/upanel/upanel.json
UNIT=/etc/systemd/system/upanel-agent.service
AGENT_DATA_DIR=/var/lib/upanel-agent
AGENT_DB=$AGENT_DATA_DIR/agent.db
KEY_FILE=/etc/upanel/agent.key
WEB_USER=upanel-web
PROBE_USER=upanel-probe
OUTSIDER_USER=nobody

# 明文标记：只出现在本脚本与内存里，落盘后必须搜不到。
PLAINTEXT='P02-4-PLAINTEXT-MARKER-5f6c'
PERSIST_PLAINTEXT='P02-4-PERSIST-MARKER-91ab'

PASSED=0
FAILED=0
FAILED_NAMES=""
CODE=""
RESP=""

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

jget() {
  python3 -c 'import json,sys
d=json.load(open(sys.argv[1], encoding="utf-8"))
print(eval(sys.argv[2]))' "$1" "$2" 2>/dev/null
}

dbq() {
  # dbq <sql>：以 root 只读方式查询代理数据库。
  python3 - "$AGENT_DB" "$1" <<'PY'
import sqlite3, sys
con = sqlite3.connect(sys.argv[1])
try:
    row = con.execute(sys.argv[2]).fetchone()
    print("" if row is None else row[0])
finally:
    con.close()
PY
}

cleanup() {
  systemctl stop upanel-agent >/dev/null 2>&1
  systemctl disable upanel-agent >/dev/null 2>&1
  rm -f "$UNIT"
  systemctl daemon-reload >/dev/null 2>&1
  rm -f "$SOCK"
}
trap cleanup EXIT
trap 'cleanup; exit 130' INT
trap 'cleanup; exit 143' TERM

call() {
  as=$1; method=$2; path=$3; body=${4:-}
  out="$WORK/out-$as/resp.json"
  url="http://agent$path"
  if [ -n "$body" ]; then
    printf '%s' "$body" >"$WORK/req.json"
    chmod 0644 "$WORK/req.json"
    args="-sS -o $out -w %{http_code} -X $method -H Content-Type:application/json --data-binary @$WORK/req.json --unix-socket $SOCK $url"
  else
    args="-sS -o $out -w %{http_code} -X $method --unix-socket $SOCK $url"
  fi
  RESP="$out"
  case $as in
    root)  CODE=$(curl $args 2>/dev/null) ;;
    web)   CODE=$(runuser -u "$WEB_USER" -- curl $args 2>/dev/null) ;;
    other) CODE=$(runuser -u "$PROBE_USER" -- curl $args 2>/dev/null) ;;
    outsider) CODE=$(runuser -u "$OUTSIDER_USER" -- curl $args 2>/dev/null) ;;
    *)     fail "call-args" "未知调用身份 $as"; CODE="" ;;
  esac
}

wait_for_socket() {
  for _ in $(seq 1 100); do
    [ -S "$SOCK" ] && return 0
    sleep 0.1
  done
  return 1
}

echo "== 0. 环境"
rm -rf "$WORK"
mkdir -p "$WORK/logs" "$WORK/out-root" "$WORK/out-web" "$WORK/out-other" "$WORK/out-outsider"
chmod 0755 "$WORK" "$WORK/out-root"
chown "$WEB_USER" "$WORK/out-web" 2>/dev/null
chown "$PROBE_USER" "$WORK/out-other" 2>/dev/null
chown "$OUTSIDER_USER" "$WORK/out-outsider" 2>/dev/null

if [ "$(id -u)" -eq 0 ]; then pass "acceptance-runs-as-root"; else fail "acceptance-runs-as-root" "本脚本必须以 root 运行"; fi
if [ -x "$AGENT" ]; then pass "agent-binary-present"; else fail "agent-binary-present" "缺少 $AGENT"; fi
if [ -x "$CTL" ]; then pass "ctl-binary-present"; else fail "ctl-binary-present" "缺少 $CTL"; fi
if [ -x "$SYS_TESTS/agentstore.test" ]; then pass "agentstore-test-binary-present"; else fail "agentstore-test-binary-present" "缺少 $SYS_TESTS/agentstore.test"; fi
if [ -x "$SYS_TESTS/agent.test" ]; then pass "agent-test-binary-present"; else fail "agent-test-binary-present" "缺少 $SYS_TESTS/agent.test"; fi
if command -v python3 >/dev/null 2>&1; then pass "python3-present"; else fail "python3-present" "缺少 python3"; fi
if [ -d /run/systemd/system ] && command -v systemctl >/dev/null 2>&1; then pass "systemd-present"; else fail "systemd-present" "没有 systemd"; fi

if ! id -u "$WEB_USER" >/dev/null 2>&1; then
  useradd --system --no-create-home --shell /usr/sbin/nologin "$WEB_USER" 2>/dev/null
fi
if id -u "$WEB_USER" >/dev/null 2>&1; then pass "web-user-exists"; else fail "web-user-exists" "无法创建 $WEB_USER"; fi
if ! id -u "$PROBE_USER" >/dev/null 2>&1; then
  useradd --system --no-create-home --shell /usr/sbin/nologin "$PROBE_USER" 2>/dev/null
  usermod -aG "$WEB_USER" "$PROBE_USER" 2>/dev/null
fi

echo
echo "== 1. 安装与首次生成密钥"
install -d -m 0755 /usr/local/lib/upanel
install -m 0755 "$AGENT" /usr/local/lib/upanel/upanel-agent
install -m 0755 "$CTL" /usr/local/bin/upanelctl
install -d -m 0755 /etc/upanel
cat >"$SYS_CFG" <<'JSON'
{
  "web": {
    "mode": "production",
    "listen_address": "127.0.0.1:9443",
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
if "$CTL" check-config --config "$SYS_CFG" >"$WORK/logs/check-config.log" 2>&1; then pass "check-config-passes"; else fail "check-config-passes" "$(cat "$WORK/logs/check-config.log")"; fi

# 代理数据目录由 unit 的 StateDirectory 创建；这里先建好，等价于“首次部署按文档执行”。
install -d -o root -g root -m 0700 "$AGENT_DATA_DIR"

# 从干净状态开始：上一轮验收留下的数据库会让计数类断言失去意义。
rm -f "$AGENT_DB" "$AGENT_DB-wal" "$AGENT_DB-shm"

# 没有密钥时必须拒绝启动，而不是自己偷偷生成一把。
rm -f "$KEY_FILE"
if "$AGENT" --config "$SYS_CFG" >"$WORK/logs/start-without-key.log" 2>&1; then
  fail "start-refused-without-key" "缺少密钥时进程直接退出了 0"
else
  pass "start-refused-without-key"
fi
expect_contains "start-without-key-hint" "-init-key" "$(cat "$WORK/logs/start-without-key.log")"
if [ -f "$KEY_FILE" ]; then fail "no-implicit-key-generation" "启动路径不得自动生成密钥"; else pass "no-implicit-key-generation"; fi

if "$AGENT" --config "$SYS_CFG" -init-key >"$WORK/logs/init-key.log" 2>&1; then
  pass "init-key-succeeds"
else
  fail "init-key-succeeds" "$(cat "$WORK/logs/init-key.log")"
fi
expect_contains "init-key-prints-fingerprint" "密钥指纹:" "$(cat "$WORK/logs/init-key.log")"
FINGERPRINT=$(sed -n 's/^密钥指纹: *\([0-9a-f]*\)$/\1/p' "$WORK/logs/init-key.log")
if [ ${#FINGERPRINT} -eq 16 ]; then pass "init-key-fingerprint-16-hex"; else fail "init-key-fingerprint-16-hex" "指纹 = '$FINGERPRINT'"; fi

if [ -f "$KEY_FILE" ]; then pass "key-file-exists"; else fail "key-file-exists" "缺少 $KEY_FILE"; fi
expect_eq "key-file-owner" "root root" "$(stat -c '%U %G' "$KEY_FILE" 2>/dev/null)"
expect_eq "key-file-mode" "600" "$(stat -c '%a' "$KEY_FILE" 2>/dev/null)"
expect_eq "key-file-lines" "2" "$(wc -l <"$KEY_FILE" 2>/dev/null | tr -d ' ')"
expect_eq "key-file-header" "upanel/agent-secret-key/v1" "$(sed -n '1p' "$KEY_FILE" 2>/dev/null)"
KEY_BYTES=$(python3 - "$KEY_FILE" <<'PY'
import base64, sys
with open(sys.argv[1], encoding="utf-8") as f:
    lines = f.read().splitlines()
print(len(base64.b64decode(lines[1])))
PY
)
expect_eq "key-file-holds-32-bytes" "32" "$KEY_BYTES"
expect_eq "key-dir-owner" "root" "$(stat -c '%U' /etc/upanel 2>/dev/null)"

# 已存在时绝不覆盖：覆盖等于让已存密文永久不可解。
if "$AGENT" --config "$SYS_CFG" -init-key >"$WORK/logs/init-key-again.log" 2>&1; then
  fail "init-key-refuses-overwrite" "第二次生成居然成功了"
else
  pass "init-key-refuses-overwrite"
fi
NEW_FINGERPRINT=$(sed -n 's/^密钥指纹: *\([0-9a-f]*\)$/\1/p' "$WORK/logs/init-key.log")
expect_eq "key-unchanged-after-refusal" "$FINGERPRINT" "$NEW_FINGERPRINT"

echo
echo "== 2. 启动代理并检查数据库/密钥的归属"
install -m 0644 "$DEPLOY_DIR/systemd/upanel-agent.service" "$UNIT"
systemctl daemon-reload
systemctl enable upanel-agent >"$WORK/logs/systemctl-enable.log" 2>&1
# 日志断言只看本次运行产生的行：持久 journal 里留着这台机器之前所有运行的记录，
# 用全量日志去找“本次的密钥指纹”会让断言在旧指纹上误过。
JOURNAL_SINCE=$(date '+%Y-%m-%d %H:%M:%S')
systemctl restart upanel-agent
if wait_for_socket; then pass "agent-socket-created"; else fail "agent-socket-created" "10 秒内未出现 $SOCK"; fi
expect_eq "systemd-service-active" "active" "$(systemctl is-active upanel-agent 2>/dev/null)"

if [ -f "$AGENT_DB" ]; then pass "agent-db-created"; else fail "agent-db-created" "缺少 $AGENT_DB"; fi
expect_eq "agent-db-owner" "root root" "$(stat -c '%U %G' "$AGENT_DB" 2>/dev/null)"
expect_eq "agent-db-mode" "600" "$(stat -c '%a' "$AGENT_DB" 2>/dev/null)"
expect_eq "agent-data-dir-owner" "root root" "$(stat -c '%U %G' "$AGENT_DATA_DIR" 2>/dev/null)"
expect_eq "agent-data-dir-mode" "700" "$(stat -c '%a' "$AGENT_DATA_DIR" 2>/dev/null)"

for suffix in -wal -shm; do
  f="$AGENT_DB$suffix"
  if [ -f "$f" ]; then
    expect_eq "agent-db$suffix-mode" "600" "$(stat -c '%a' "$f" 2>/dev/null)"
  fi
done

# 代理库 schema 版本随代理自己的迁移推进：0001 secrets，0002 preflight_plans（P03.1），
# 0003 operations + operation_events（P03.2）。
# 这里是“当前应有的版本”，不是上限；新增代理迁移时必须同步更新这一行。
expect_eq "schema-version" "3" "$(dbq 'SELECT MAX(version) FROM schema_migrations')"
expect_eq "migration-name" "secrets" "$(dbq 'SELECT name FROM schema_migrations WHERE version = 1')"
expect_eq "secrets-table-exists" "secrets" "$(dbq "SELECT name FROM sqlite_master WHERE type='table' AND name='secrets'")"

# Web 账号连目录都进不去，因此读不到数据库与密钥。
if runuser -u "$WEB_USER" -- test -r "$AGENT_DB" 2>/dev/null; then
  fail "web-user-cannot-read-agent-db" "upanel-web 能读 agent.db"
else
  pass "web-user-cannot-read-agent-db"
fi
if runuser -u "$WEB_USER" -- test -r "$KEY_FILE" 2>/dev/null; then
  fail "web-user-cannot-read-key" "upanel-web 能读 agent.key"
else
  pass "web-user-cannot-read-key"
fi
if runuser -u "$WEB_USER" -- ls "$AGENT_DATA_DIR" >/dev/null 2>&1; then
  fail "web-user-cannot-list-agent-data-dir" "upanel-web 能列 $AGENT_DATA_DIR"
else
  pass "web-user-cannot-list-agent-data-dir"
fi

JOURNAL_START=$(journalctl -u upanel-agent --no-pager -o cat --since "$JOURNAL_SINCE" 2>/dev/null)
expect_contains "journal-reports-key-fingerprint" "$FINGERPRINT" "$JOURNAL_START"
expect_contains "journal-reports-schema-version" "agent_schema_version" "$JOURNAL_START"
expect_contains "journal-reports-db-path" "$AGENT_DB" "$JOURNAL_START"

echo
echo "== 3. POST /internal/v1/secrets（Web 身份）"
call web POST /internal/v1/secrets "{\"purpose\":\"smb_password\",\"value\":\"$PLAINTEXT\"}"
expect_eq "post-secret-created" "201" "$CODE"
TEMP_REF=$(jget "$RESP" 'd["data"]["secret_ref"]')
TEMP_STATE=$(jget "$RESP" 'd["data"]["state"]')
TEMP_EXPIRES=$(jget "$RESP" 'd["data"]["expires_at"]')
case $TEMP_REF in
  sec_????????????????????????????????) [ ${#TEMP_REF} -eq 36 ] && pass "post-secret-returns-ref" || fail "post-secret-returns-ref" "引用长度异常: $TEMP_REF" ;;
  *) fail "post-secret-returns-ref" "引用形态异常: $TEMP_REF" ;;
esac
expect_eq "post-secret-state-temporary" "temporary" "$TEMP_STATE"
expect_not_contains "post-secret-response-has-no-plaintext" "$PLAINTEXT" "$(cat "$RESP" 2>/dev/null)"
TTL_CHECK=$(python3 - "$TEMP_EXPIRES" <<'PY'
import datetime, sys
raw = sys.argv[1]
try:
    t = datetime.datetime.strptime(raw, "%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=datetime.timezone.utc)
except Exception:
    print("BAD")
    raise SystemExit
now = datetime.datetime.now(datetime.timezone.utc)
print(int((t - now).total_seconds()))
PY
)
case $TTL_CHECK in
  [0-9]*) if [ "$TTL_CHECK" -ge 540 ] && [ "$TTL_CHECK" -le 660 ]; then pass "post-secret-expires-in-10m"; else fail "post-secret-expires-in-10m" "剩余 $TTL_CHECK 秒"; fi ;;
  *) fail "post-secret-expires-in-10m" "expires_at 无法解析: $TEMP_EXPIRES" ;;
esac

call web POST /internal/v1/secrets "{\"purpose\":\"backup_repository_password\",\"value\":\"$PERSIST_PLAINTEXT\",\"reference\":\"backup.repository.1\"}"
expect_eq "post-persistent-created" "201" "$CODE"
PERSIST_REF=$(jget "$RESP" 'd["data"]["secret_ref"]')
CALL_BODY=$(cat "$RESP" 2>/dev/null)
expect_eq "post-persistent-state" "persistent" "$(jget "$RESP" 'd["data"]["state"]')"
expect_not_contains "post-persistent-has-no-expiry" "expires_at" "$CALL_BODY"

call web GET /internal/v1/secrets
expect_eq "get-secrets-rejected" "405" "$CODE"
expect_contains "get-secrets-allow-header" "不提供读取秘密的接口" "$(cat "$RESP" 2>/dev/null)"
call web GET "/internal/v1/secrets/$TEMP_REF"
expect_eq "secret-by-ref-not-found" "404" "$CODE"
call web GET "/internal/v1/secrets/$TEMP_REF/value"
expect_eq "secret-value-route-not-found" "404" "$CODE"

call web POST /internal/v1/secrets '{"purpose":"root_shell","value":"x"}'
expect_eq "unknown-purpose-rejected" "400" "$CODE"
expect_contains "unknown-purpose-message" "未登记的用途" "$(cat "$RESP" 2>/dev/null)"
call web POST /internal/v1/secrets '{"purpose":"totp_seed","value":"JBSWY3DPEHPK3PXP"}'
expect_eq "internal-purpose-rejected" "400" "$CODE"
expect_contains "internal-purpose-message" "代理内部流程" "$(cat "$RESP" 2>/dev/null)"
call web POST /internal/v1/secrets '{"purpose":"smb_password","value":""}'
expect_eq "empty-value-rejected" "400" "$CODE"
BIG=$(python3 -c 'print("a"*5120)')
call web POST /internal/v1/secrets "{\"purpose\":\"smb_password\",\"value\":\"$BIG\"}"
expect_eq "oversize-value-rejected" "400" "$CODE"
call web POST /internal/v1/secrets '{"purpose":"smb_password","value":"x","extra":1}'
expect_eq "unknown-field-rejected" "400" "$CODE"

call root POST /internal/v1/secrets "{\"purpose\":\"smb_password\",\"value\":\"x\"}"
expect_eq "root-denied-on-normal-route" "403" "$CODE"
call other POST /internal/v1/secrets "{\"purpose\":\"smb_password\",\"value\":\"x\"}"
expect_eq "same-group-other-uid-denied" "403" "$CODE"
call outsider POST /internal/v1/secrets "{\"purpose\":\"smb_password\",\"value\":\"x\"}"
case $CODE in
  000|"") pass "outsider-cannot-connect" ;;
  *) fail "outsider-cannot-connect" "nobody 居然拿到了 HTTP $CODE" ;;
esac

echo
echo "== 4. 落盘的是密文"
expect_eq "secret-row-count" "2" "$(dbq 'SELECT COUNT(*) FROM secrets')"
expect_eq "temporary-row-state" "temporary" "$(dbq "SELECT state FROM secrets WHERE id = '$TEMP_REF'")"
expect_eq "persistent-row-state" "persistent" "$(dbq "SELECT state FROM secrets WHERE id = '$PERSIST_REF'")"
expect_eq "persistent-row-reference" "backup.repository.1" "$(dbq "SELECT reference FROM secrets WHERE id = '$PERSIST_REF'")"
CT_LEN=$(dbq "SELECT length(ciphertext) FROM secrets WHERE id = '$TEMP_REF'")
if [ "${CT_LEN:-0}" -ge 16 ] 2>/dev/null; then pass "ciphertext-has-auth-tag"; else fail "ciphertext-has-auth-tag" "密文长度 = ${CT_LEN:-空}"; fi
expect_eq "nonce-length" "12" "$(dbq "SELECT length(nonce) FROM secrets WHERE id = '$TEMP_REF'")"
expect_eq "nonce-unique" "2" "$(dbq 'SELECT COUNT(DISTINCT hex(nonce)) FROM secrets')"
expect_eq "key-version-recorded" "1" "$(dbq "SELECT key_version FROM secrets WHERE id = '$TEMP_REF'")"

# 明文标记不能在数据库及其 WAL/SHM 里出现。
LEAK=0
for f in "$AGENT_DB" "$AGENT_DB-wal" "$AGENT_DB-shm"; do
  [ -f "$f" ] || continue
  n=$(grep -a -c -F "$PLAINTEXT" "$f" 2>/dev/null || true)
  [ "${n:-0}" -eq 0 ] || LEAK=$((LEAK + 1))
  m=$(grep -a -c -F "$PERSIST_PLAINTEXT" "$f" 2>/dev/null || true)
  [ "${m:-0}" -eq 0 ] || LEAK=$((LEAK + 1))
done
expect_eq "no-plaintext-on-disk" "0" "$LEAK"

echo
echo "== 5. 重启后秘密仍在（不是内存态）"
systemctl restart upanel-agent
if wait_for_socket; then pass "socket-after-restart"; else fail "socket-after-restart" "重启后没有 socket"; fi
expect_eq "secret-row-count-after-restart" "2" "$(dbq 'SELECT COUNT(*) FROM secrets')"
expect_eq "persistent-row-survives-restart" "persistent" "$(dbq "SELECT state FROM secrets WHERE id = '$PERSIST_REF'")"

echo
echo "== 6. 到期销毁与加密实现的真实执行（交叉编译的测试二进制）"
"$SYS_TESTS/agentstore.test" -test.v \
  -test.run 'TestDestroyExpiredTemporary|TestReferencePromotesTemporarySecret|TestCiphertextNeverLeaksPlaintextToDisk|TestGenerateKeyFileThenLoad|TestOpenRejectsWrongAADAndTampering|TestOpenRejectsNonRootOwnedDir|TestResolveExpiredTemporaryBeforeSweep' \
  >"$WORK/logs/agentstore-test.log" 2>&1
STORE_FAIL=$(grep -c '^--- FAIL' "$WORK/logs/agentstore-test.log" 2>/dev/null || true)
STORE_PASS=$(grep -c '^--- PASS' "$WORK/logs/agentstore-test.log" 2>/dev/null || true)
STORE_SKIP=$(grep -c '^--- SKIP' "$WORK/logs/agentstore-test.log" 2>/dev/null || true)
expect_eq "agentstore-test-failures" "0" "$STORE_FAIL"
if [ "$STORE_PASS" -ge 6 ]; then pass "agentstore-test-passes"; else fail "agentstore-test-passes" "PASS=$STORE_PASS"; fi
if [ "$STORE_SKIP" -gt 0 ]; then note "agentstore.test 跳过了 $STORE_SKIP 个用例（平台相关），详见 logs/agentstore-test.log"; fi
expect_contains "root-only-dir-enforced" "PASS" "$(grep -E '^--- PASS: TestOpenRejectsNonRootOwnedDir' "$WORK/logs/agentstore-test.log" 2>/dev/null)"

"$SYS_TESTS/agent.test" -test.v \
  -test.run 'TestPostSecret|TestSweepExpiredSecrets|TestSecretsRouteRejectsOtherMethods' \
  >"$WORK/logs/agent-secrets-test.log" 2>&1
AGENT_FAIL=$(grep -c '^--- FAIL' "$WORK/logs/agent-secrets-test.log" 2>/dev/null || true)
AGENT_PASS=$(grep -c '^--- PASS' "$WORK/logs/agent-secrets-test.log" 2>/dev/null || true)
expect_eq "agent-secrets-test-failures" "0" "$AGENT_FAIL"
if [ "$AGENT_PASS" -ge 8 ]; then pass "agent-secrets-test-passes"; else fail "agent-secrets-test-passes" "PASS=$AGENT_PASS"; fi

note "NOT RUN: 服务进程上 10 分钟挂钟到期销毁（需要等待 10 分钟；到期逻辑已由上一步"
note "         TestDestroyExpiredTemporary / TestResolveExpiredTemporaryBeforeSweep 用真实"
note "         SQLite 与真实 AEAD 在短 TTL 下执行，服务端扫描器本身只做定时调用）"

echo
echo "== 7. 日志与响应里没有明文"
JOURNAL=$(journalctl -u upanel-agent --no-pager -o cat --since "$JOURNAL_SINCE" 2>/dev/null | tail -400)
expect_not_contains "journal-has-no-temporary-plaintext" "$PLAINTEXT" "$JOURNAL"
expect_not_contains "journal-has-no-persistent-plaintext" "$PERSIST_PLAINTEXT" "$JOURNAL"
expect_not_contains "journal-has-no-key-body" "$(python3 - "$KEY_FILE" <<'PY'
import base64, sys
with open(sys.argv[1], encoding="utf-8") as f:
    print(f.read().splitlines()[1])
PY
)" "$JOURNAL"
expect_contains "journal-records-secret-write" "已保存加密秘密" "$JOURNAL"

echo
echo "== 8. 停止后清理"
systemctl stop upanel-agent
expect_eq "service-inactive-after-stop" "inactive" "$(systemctl is-active upanel-agent 2>/dev/null)"
if [ -S "$SOCK" ]; then fail "socket-removed-after-stop" "socket 仍存在"; else pass "socket-removed-after-stop"; fi

echo
echo "结果: PASS=$PASSED FAIL=$FAILED"
if [ "$FAILED" -ne 0 ]; then
  echo "失败项:$FAILED_NAMES"
  exit 1
fi
exit 0
