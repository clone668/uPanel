#!/bin/sh
# P02.3 Linux 验收脚本：代理 Unix socket、SO_PEERCRED 的 UID 边界、固定登记表、
# 结构化只读查询通道与命令执行器。
#
# 运行位置：Ubuntu 22.04/24.04 验收 VM 内，**必须以 root 运行**（代理本身只能以 root 启动，
# 并且需要在不同 UID 之间切换来验证 socket 边界）。不依赖 Go 工具链：使用宿主交叉编译好的
# Linux 二进制，其中包含用 go test -c 编出来的测试二进制，用来在真实 Linux 内核上执行
# 命令执行器（时限/输出上限/进程组终止）与登记表/边界用例。
#
# 用法：
#   sudo BIN_DIR=/tmp/upanel-bin sh tests/integration/p02-3-agent-acceptance.sh
#
# 退出码：0 全部通过；1 有失败项。
#
# 覆盖的验收点（CONTRACTS §15 + STAGES.md P02.3 + P02 验收清单里与代理相关的部分）：
#   1. 代理以 root 通过 systemd 启动；socket 为 root:upanel-web 0660，/run/upanel 为 root:root 0755
#      （目录可穿越、不可写：Web 账号要能走到 socket，但不能在里面新建或替换文件）
#   2. 普通接口只允许 upanel-web（Web 账号）；root 与其他 UID 一律 403 FORBIDDEN
#   3. capabilities 如实报告能力：写动作表为空，查询表恰好是已登记的那几项，不多报
#   4. 未登记 query、命令字符串字段、未知字段、超长请求体、非 POST 一律拒绝
#   5. 未知路径 404（Web）与 403（root）：代理没有“万能转发”
#   6. 启动期拒绝：非 root、账号不存在、配置里写数字 UID、socket 路径被普通文件占据
#   7. 命令执行器在真实 Linux 上按时限终止进程、截断输出、不继承父进程环境
#   8. 停止后 socket 被清理，服务不再活跃；日志里没有秘密

set -u

# 早退：非 root 时后面的断言只会连锁失败（先撞 systemd 权限再撞 socket 缺失），
# 真正的错因会被噪声淹没。这里直接给出唯一正确的调用方式。
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
WORK=${WORK:-/var/tmp/upanel-accept-p02-3}
SOCK=/run/upanel/agent.sock
RUN_DIR=/run/upanel
SYS_CFG=/etc/upanel/upanel.json
UNIT=/etc/systemd/system/upanel-agent.service
WEB_USER=upanel-web
# 与 Web 账号同组、但不是 Web 账号的用户：用来证明拒绝来自 SO_PEERCRED 的 UID 判断，
# 而不是文件权限。它能打开 socket，但代理必须给它 403。
PROBE_USER=upanel-probe
# 完全无关的用户：连 socket 的文件权限都过不去，只能拿到连接失败。
OUTSIDER_USER=nobody

# capabilities 的完整闭集。生产代理只要依赖齐备就会登记全部查询与动作，
# 断言**完全等于**当前应有的集合：少报是隐藏能力，多报是编造能力，两种都算失败。
# 新增查询/动作时同步改这两行（P05.1 加 service.control，P05.2 加 processes.list / process.signal）。
EXPECTED_QUERIES="['logs.query', 'processes.list', 'services.list', 'services.unit', 'storage.topology', 'system.summary']"
EXPECTED_ACTIONS="['process.signal', 'service.control']"

PASSED=0
FAILED=0
FAILED_NAMES=""
CODE=""
RESP=""

pass() { PASSED=$((PASSED + 1)); printf 'PASS  %s\n' "$1"; }
fail() { FAILED=$((FAILED + 1)); FAILED_NAMES="$FAILED_NAMES | $1"; printf 'FAIL  %s: %s\n' "$1" "${2:-}"; }
# 不计入断言的记录行：用于环境噪声（例如系统自带单元在旧版 systemd 下的告警）。
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

# call <as: web|root|other> <method> <path> [json]
# 结果：CODE=HTTP 状态码，响应体位于 $RESP。
# 每个身份有自己的输出目录：curl 以对应用户运行，必须能在那里面写响应文件。
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

if [ "$(id -u)" -eq 0 ]; then pass "acceptance-runs-as-root"; else fail "acceptance-runs-as-root" "本脚本必须以 root 运行（代理只能在 root 下启动）"; fi
if [ -x "$AGENT" ]; then pass "agent-binary-present"; else fail "agent-binary-present" "缺少 $AGENT"; fi
if [ -x "$CTL" ]; then pass "ctl-binary-present"; else fail "ctl-binary-present" "缺少 $CTL"; fi
if [ -d /run/systemd/system ] && command -v systemctl >/dev/null 2>&1; then pass "systemd-present"; else fail "systemd-present" "没有 systemd，无法验证真实托管形态"; fi
if command -v python3 >/dev/null 2>&1; then pass "python3-present"; else fail "python3-present" "缺少 python3"; fi

if ! id -u "$WEB_USER" >/dev/null 2>&1; then
  useradd --system --no-create-home --shell /usr/sbin/nologin "$WEB_USER" 2>/dev/null
fi
if id -u "$WEB_USER" >/dev/null 2>&1; then pass "web-user-exists"; else fail "web-user-exists" "无法创建 $WEB_USER"; fi
chown "$WEB_USER" "$WORK/out-web" 2>/dev/null

if ! id -u "$PROBE_USER" >/dev/null 2>&1; then
  useradd --system --no-create-home --shell /usr/sbin/nologin "$PROBE_USER" 2>/dev/null
fi
adduser_out=$(usermod -aG "$WEB_USER" "$PROBE_USER" 2>&1)
if [ -z "$adduser_out" ]; then pass "probe-user-in-web-group"; else fail "probe-user-in-web-group" "$adduser_out"; fi
chown "$PROBE_USER" "$WORK/out-other" 2>/dev/null

echo
echo "== 1. 安装并启动代理服务"
install -d -m 0755 /usr/local/lib/upanel
install -m 0755 "$AGENT" /usr/local/lib/upanel/upanel-agent
if [ -x /usr/local/lib/upanel/upanel-agent ]; then pass "agent-installed"; else fail "agent-installed" "安装失败"; fi
expect_eq "agent-binary-owner-is-root" "root" "$(stat -c '%U' /usr/local/lib/upanel/upanel-agent)"
expect_eq "agent-binary-mode-755" "755" "$(stat -c '%a' /usr/local/lib/upanel/upanel-agent)"

# 单元的 ExecStartPre 用 upanelctl check-config，必须是本轮的二进制；
# 上一次验收留下的旧 CLI 不认识新配置字段，会让服务直接起不来。
install -m 0755 "$CTL" /usr/local/bin/upanelctl
if [ -x /usr/local/bin/upanelctl ]; then pass "ctl-installed"; else fail "ctl-installed" "安装失败"; fi

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
expect_eq "config-dir-owner-root" "root" "$(stat -c '%U' /etc/upanel)"
if runuser -u "$WEB_USER" -- test -w /etc/upanel; then
  fail "config-dir-not-writable-by-web-user" "$WEB_USER 能写 /etc/upanel"
else
  pass "config-dir-not-writable-by-web-user"
fi
if "$CTL" check-config --config "$SYS_CFG" >"$WORK/logs/check-config.log" 2>&1; then pass "check-config-passes"; else fail "check-config-passes" "$(cat "$WORK/logs/check-config.log")"; fi

UNIT_TEXT=$(cat "$DEPLOY_DIR/systemd/upanel-agent.service")
expect_contains "unit-runs-as-root" "User=root" "$UNIT_TEXT"
expect_contains "unit-execstart-agent" "ExecStart=/usr/local/lib/upanel/upanel-agent" "$UNIT_TEXT"
expect_contains "unit-runtime-directory" "RuntimeDirectory=upanel" "$UNIT_TEXT"
expect_not_contains "unit-has-no-private-mounts" "PrivateMounts=yes" "$UNIT_TEXT"
expect_not_contains "unit-has-no-private-devices" "PrivateDevices=yes" "$UNIT_TEXT"

# P02.4 起代理依赖独立数据库与本机密钥：缺任何一样都会拒绝启动（不降级）。
# 数据库目录由单元的 StateDirectory=upanel-agent 以 root:root 0700 创建；密钥必须
# 先以 root 生成一次。这里只做启动前置，不在此脚本里断言它们（P02.4 脚本负责）。
install -d -o root -g root -m 0700 /var/lib/upanel-agent
if [ ! -f /etc/upanel/agent.key ]; then
  /usr/local/lib/upanel/upanel-agent --config "$SYS_CFG" -init-key     >"$WORK/logs/init-secret-key.log" 2>&1     || fail "init-secret-key" "$(cat "$WORK/logs/init-secret-key.log")"
fi

install -m 0644 "$DEPLOY_DIR/systemd/upanel-agent.service" "$UNIT"
systemctl daemon-reload
# systemd-analyze verify 会把单元的依赖单元一起加载，输出里因此会混入系统自带单元的告警
# （22.04 的 snapd.service 在 systemd 249 下就报 "Unknown key name 'RestartMode'"）。
# 这里只判**本单元自身**的告警，其余原文记入日志和 NOTE，不当作 uPanel 的问题。
VERIFY_OUT=$(systemd-analyze verify "$UNIT" 2>&1)
printf '%s\n' "$VERIFY_OUT" >"$WORK/logs/systemd-analyze-verify.log"
VERIFY_OWN=$(printf '%s\n' "$VERIFY_OUT" | grep -E 'upanel-agent\.service' || true)
if [ -z "$VERIFY_OWN" ]; then pass "systemd-analyze-verify-clean"; else fail "systemd-analyze-verify-clean" "$VERIFY_OWN"; fi
VERIFY_FOREIGN=$(printf '%s\n' "$VERIFY_OUT" | grep -vE 'upanel-agent\.service' | grep -v '^$' || true)
if [ -n "$VERIFY_FOREIGN" ]; then
  note "systemd-analyze 报告了与本单元无关的告警（已忽略，原文见 logs/systemd-analyze-verify.log）：$(printf '%s' "$VERIFY_FOREIGN" | head -2 | tr '\n' ' ')"
fi

systemctl enable upanel-agent >"$WORK/logs/systemctl-enable.log" 2>&1
# 日志断言的起点：只检查**本次运行**产生的日志。持久 journal 会保留这台机器上之前所有
# 运行的记录（例如 P02.4 写入秘密时会记录 purpose=smb_password），用全量日志做
# “不含 password/secret”断言会把历史行算到这次头上，既误报也无法复现。
JOURNAL_SINCE=$(date '+%Y-%m-%d %H:%M:%S')
systemctl restart upanel-agent
if wait_for_socket; then pass "agent-socket-created"; else fail "agent-socket-created" "10 秒内未出现 $SOCK"; fi
expect_eq "systemd-service-active" "active" "$(systemctl is-active upanel-agent 2>/dev/null)"

MAINPID=$(systemctl show -p MainPID --value upanel-agent 2>/dev/null | tr -d ' ')
if [ -n "$MAINPID" ] && [ "$MAINPID" != "0" ]; then pass "systemd-mainpid-present"; else fail "systemd-mainpid-present" "MainPID=$MAINPID"; fi
expect_eq "agent-process-runs-as-root" "root" "$(ps -o user= -p "$MAINPID" 2>/dev/null | tr -d ' ')"

echo
echo "== 2. socket 与目录权限"
if [ -S "$SOCK" ]; then pass "socket-is-unix-socket"; else fail "socket-is-unix-socket" "$SOCK 不是 socket"; fi
expect_eq "socket-owner-root" "root" "$(stat -c '%U' "$SOCK" 2>/dev/null)"
expect_eq "socket-group-web-user" "$WEB_USER" "$(stat -c '%G' "$SOCK" 2>/dev/null)"
expect_eq "socket-mode-660" "660" "$(stat -c '%a' "$SOCK" 2>/dev/null)"
expect_eq "run-dir-owner-root" "root" "$(stat -c '%U' "$RUN_DIR" 2>/dev/null)"
expect_eq "run-dir-mode-755" "755" "$(stat -c '%a' "$RUN_DIR" 2>/dev/null)"
if runuser -u "$OUTSIDER_USER" -- touch "$RUN_DIR/probe" >/dev/null 2>&1; then
  fail "run-dir-not-writable-by-others" "$OUTSIDER_USER 能在 $RUN_DIR 里创建文件"
  rm -f "$RUN_DIR/probe"
else
  pass "run-dir-not-writable-by-others"
fi
TCP_LISTEN=$({ ss -ltnp 2>/dev/null || true; } | grep -c "pid=$MAINPID," || true)
expect_eq "agent-has-no-tcp-listener" "0" "$TCP_LISTEN"

echo
echo "== 3. capabilities 只对 Web 账号开放"
call web GET /internal/v1/capabilities
expect_eq "capabilities-web-200" "200" "$CODE"
expect_eq "capabilities-mode-production" "production" "$(jget "$RESP" 'd["data"]["mode"]')"
# 查询与动作表随阶段增长：P04.1 加 system.summary，P04.2 加 storage.topology，
# P05.1 加 services.* / service.control，P05.2 加 processes.list / process.signal。
# 这里断言**完全等于**当前应有的集合（字典序）：少报是隐藏能力，多报是编造能力，两种都算失败。
expect_eq "capabilities-queries-exact" "$EXPECTED_QUERIES" "$(jget "$RESP" 'd["data"]["supported_queries"]')"
expect_eq "capabilities-actions-exact" "$EXPECTED_ACTIONS" "$(jget "$RESP" 'd["data"]["supported_actions"]')"
CAPS_BODY=$(cat "$RESP" 2>/dev/null)

call root GET /internal/v1/capabilities
expect_eq "capabilities-root-403" "403" "$CODE"
expect_eq "capabilities-root-code" "FORBIDDEN" "$(jget "$RESP" 'd["error"]["code"]')"

call other GET /internal/v1/capabilities
expect_eq "same-group-other-uid-403" "403" "$CODE"
expect_eq "same-group-other-uid-code" "FORBIDDEN" "$(jget "$RESP" 'd["error"]["code"]')"
expect_contains "denied-without-leaking-allowed-uid" "无权调用" "$(cat "$RESP" 2>/dev/null)"

# 完全无关的用户连 socket 的 0660 权限都过不去：这里是文件系统层的拒绝，
# 不是代理返回的 403。两层都要真实存在。
call outsider GET /internal/v1/capabilities
expect_eq "outsider-cannot-connect" "000" "$CODE"

echo
echo "== 4. 结构化只读查询：只认登记过的名字与字段"
call web POST /internal/v1/queries '{"query":"system.nonexistent"}'
expect_eq "unknown-query-400" "400" "$CODE"
expect_eq "unknown-query-code" "VALIDATION_ERROR" "$(jget "$RESP" 'd["error"]["code"]')"
expect_contains "unknown-query-message" "未登记" "$(cat "$RESP" 2>/dev/null)"
# system.summary 从 P04.1 起是登记过的查询：它必须返回 200，但**仍然不接受任何参数**
# （下面三条把它当成“已登记但拒绝夹带”的样本，比原来用未登记名字更严格）。
call web POST /internal/v1/queries '{"query":"system.summary"}'
expect_eq "registered-query-200" "200" "$CODE"
expect_eq "registered-query-has-data" "True" "$(jget "$RESP" 'bool(d.get("data"))')"

call web POST /internal/v1/queries '{"query":"command","params":{"command":"id"}}'
expect_eq "command-query-rejected-400" "400" "$CODE"

call web POST /internal/v1/queries '{"query":"system.summary","command":"id -u"}'
expect_eq "unknown-field-envelope-400" "400" "$CODE"

call web POST /internal/v1/queries '{"query":"system.summary","params":{"command":"id -u"}}'
expect_eq "unknown-field-params-400" "400" "$CODE"

call web POST /internal/v1/queries 'not json'
expect_eq "non-json-body-400" "400" "$CODE"

call web POST /internal/v1/queries ''
expect_eq "empty-body-400" "400" "$CODE"

HUGE=$(python3 -c 'print("x"*(70*1024))')
call web POST /internal/v1/queries "{\"query\":\"system.summary\",\"params\":{\"blob\":\"$HUGE\"}}"
expect_eq "oversize-body-413" "413" "$CODE"

call web GET /internal/v1/queries
expect_eq "queries-requires-post-405" "405" "$CODE"

echo
echo "== 5. 没有万能转发：未知路径与 root 边界"
call web GET /
expect_eq "root-path-404-for-web" "404" "$CODE"
call root GET /internal/v1/operations
expect_eq "unknown-path-403-for-root" "403" "$CODE"
call other GET /internal/v1/queries
expect_eq "same-group-other-uid-403-on-queries" "403" "$CODE"
# 未知路径用一个真的不存在的端点；/internal/v1/streams 已实现，GET 它是 405。
call web GET /internal/v1/no-such-endpoint
expect_eq "unknown-path-404-for-web" "404" "$CODE"
call web GET /internal/v1/streams
expect_eq "streams-requires-post-405" "405" "$CODE"
# /internal/v1/operations 已在 P03.2 实现：夹带未定义字段必须被拒绝，不能靠“未实现”挡。
call web POST /internal/v1/operations '{"action":"system.power"}'
expect_eq "operations-rejects-unknown-fields-400" "400" "$CODE"

echo
echo "== 6. 启动期拒绝（不降级、不放行）"
cat >"$WORK/cfg-baduser.json" <<'JSON'
{
  "web": {"mode": "production", "listen_address": "127.0.0.1:9443"},
  "agent": {"socket_path": "/run/upanel/agent.sock", "web_user": "upanel-no-such-user"},
  "database": {"path": "/var/lib/upanel/upanel.db"}
}
JSON
"$AGENT" --config "$WORK/cfg-baduser.json" --socket "$WORK/nope.sock" >"$WORK/logs/baduser.log" 2>&1
BADUSER_RC=$?
expect_eq "bad-web-user-exit-1" "1" "$BADUSER_RC"
expect_contains "bad-web-user-message" "不存在" "$(cat "$WORK/logs/baduser.log")"

cat >"$WORK/cfg-numeric.json" <<'JSON'
{
  "web": {"mode": "production", "listen_address": "127.0.0.1:9443"},
  "agent": {"socket_path": "/run/upanel/agent.sock", "web_user": "1000"},
  "database": {"path": "/var/lib/upanel/upanel.db"}
}
JSON
"$AGENT" --config "$WORK/cfg-numeric.json" --socket "$WORK/nope.sock" >"$WORK/logs/numeric.log" 2>&1
NUMERIC_RC=$?
expect_eq "numeric-web-user-exit-2" "2" "$NUMERIC_RC"
expect_contains "numeric-web-user-message" "agent.web_user" "$(cat "$WORK/logs/numeric.log")"

runuser -u "$WEB_USER" -- "$AGENT" --config "$SYS_CFG" --socket "$WORK/nonroot.sock" >"$WORK/logs/nonroot.log" 2>&1
NONROOT_RC=$?
expect_eq "agent-refuses-non-root-1" "1" "$NONROOT_RC"
expect_contains "agent-refuses-non-root-message" "必须以 root 运行" "$(cat "$WORK/logs/nonroot.log")"

install -d -m 0755 "$WORK/sockdir"
printf 'not a socket\n' >"$WORK/sockdir/agent.sock"
"$AGENT" --config "$SYS_CFG" --socket "$WORK/sockdir/agent.sock" >"$WORK/logs/occupied.log" 2>&1
OCCUPIED_RC=$?
expect_eq "agent-refuses-non-socket-path-1" "1" "$OCCUPIED_RC"
expect_contains "agent-refuses-non-socket-message" "拒绝覆盖" "$(cat "$WORK/logs/occupied.log")"
expect_eq "agent-kept-foreign-file" "not a socket" "$(cat "$WORK/sockdir/agent.sock")"

echo
echo "== 7. 命令执行器与登记表：真实 Linux 内核上跑单元测试二进制"
if [ -x "$SYS_TESTS/syscmd.test" ]; then
  "$SYS_TESTS/syscmd.test" -test.v >"$WORK/logs/syscmd-test.log" 2>&1
  SYS_RC=$?
  SYS_PASS=$(grep -c '^--- PASS' "$WORK/logs/syscmd-test.log")
  SYS_FAIL=$(grep -c '^--- FAIL' "$WORK/logs/syscmd-test.log")
  expect_eq "syscmd-unit-tests-exit-0" "0" "$SYS_RC"
  expect_eq "syscmd-unit-tests-no-fail" "0" "$SYS_FAIL"
  if [ "$SYS_PASS" -ge 8 ]; then pass "syscmd-unit-tests-count($SYS_PASS)"; else fail "syscmd-unit-tests-count" "只跑了 $SYS_PASS 个用例"; fi
  expect_contains "syscmd-timeout-case-ran" "PASS: TestRunTimesOutAndKillsProcess" "$(cat "$WORK/logs/syscmd-test.log")"
  expect_contains "syscmd-env-case-ran" "PASS: TestRunUsesFixedEnvironment" "$(cat "$WORK/logs/syscmd-test.log")"
  expect_contains "syscmd-truncate-case-ran" "PASS: TestRunTruncatesOutputAtLimit" "$(cat "$WORK/logs/syscmd-test.log")"
else
  fail "syscmd-unit-tests-exit-0" "缺少 $SYS_TESTS/syscmd.test（宿主需用 go test -c 交叉编译）"
fi

if [ -x "$SYS_TESTS/agent.test" ]; then
  "$SYS_TESTS/agent.test" -test.v >"$WORK/logs/agent-test.log" 2>&1
  AG_RC=$?
  AG_FAIL=$(grep -c '^--- FAIL' "$WORK/logs/agent-test.log")
  AG_PASS=$(grep -c '^--- PASS' "$WORK/logs/agent-test.log")
  expect_eq "agent-unit-tests-exit-0" "0" "$AG_RC"
  expect_eq "agent-unit-tests-no-fail" "0" "$AG_FAIL"
  if [ "$AG_PASS" -ge 15 ]; then pass "agent-unit-tests-count($AG_PASS)"; else fail "agent-unit-tests-count" "只跑了 $AG_PASS 个用例"; fi
  expect_contains "agent-gate-root-denied-ran" "PASS: TestGateDeniesRootOnNormalRoutes" "$(cat "$WORK/logs/agent-test.log")"
  expect_contains "agent-gate-other-uid-ran" "PASS: TestGateDeniesOtherUIDs" "$(cat "$WORK/logs/agent-test.log")"
  expect_contains "agent-unknown-query-ran" "PASS: TestQueriesRejectsUnknownQuery" "$(cat "$WORK/logs/agent-test.log")"
else
  fail "agent-unit-tests-exit-0" "缺少 $SYS_TESTS/agent.test"
fi

echo
echo "== 8. 停止与清理"
systemctl stop upanel-agent
STOPPED=0
for _ in $(seq 1 50); do
  if [ ! -S "$SOCK" ]; then STOPPED=1; break; fi
  sleep 0.1
done
expect_eq "socket-removed-on-stop" "1" "$STOPPED"
expect_eq "systemd-service-inactive" "inactive" "$(systemctl is-active upanel-agent 2>/dev/null)"

echo
echo "== 9. 日志与秘密"
JOURNAL=$(journalctl -u upanel-agent --no-pager -o cat --since "$JOURNAL_SINCE" 2>/dev/null | head -200)
if [ -n "$JOURNAL" ]; then pass "journal-readable"; else fail "journal-readable" "没有读到代理日志"; fi
expect_contains "journal-shows-listening" "代理已开始监听" "$JOURNAL"
expect_not_contains "journal-has-no-password" "password" "$JOURNAL"
expect_not_contains "journal-has-no-secret" "secret" "$JOURNAL"
expect_contains "capabilities-report-production" "production" "$CAPS_BODY"

echo
echo "=================================================="
echo "PASS $PASSED / FAIL $FAILED"
if [ "$FAILED" -ne 0 ]; then
  echo "失败项:$FAILED_NAMES"
  exit 1
fi
exit 0
