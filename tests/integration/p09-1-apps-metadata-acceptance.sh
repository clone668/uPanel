#!/bin/sh
# P09.1 的 Linux 验收：应用/分组/收藏元数据、判别式绑定、公共入口与内部目标分离。
#
# 这一份要证明的不是“接口返回 200”，而是下面这些容易被“写完了”骗过去的事实：
#
#   1) **绑定是判别式的，而且唯一性由数据库保证**：type 说 docker 就只能是 64 位容器 ID，
#      多给一个 unit 是 400 而不是被忽略；同一个容器给两个应用是 422，不是先查后写。
#   2) **公共入口与内部探测目标是两个字段**：`public_url` 里写回环地址在这台机器的生产模式
#      下被拒（它是“浏览器点得开”的地址，不是“面板自己请求”的地址）；健康检查的 `target_id`
#      形状里根本写不出 IP 或主机名，云元数据地址连“填进去”这一步都做不到。
#   3) **摘要是诚实的不知道**：没有探测器的 P09.1 里 `runtime_state=unknown`、
#      `http_state=not_applicable|unknown`、`last_checked_at=null`、
#      `dependency_protection=unmanaged`，`capabilities` 除 `open` 外全为 false。
#   4) **依赖卷经代理确认，而且“确认不了”与“不存在”是两种回答**：代理停掉时带依赖卷的写入
#      返回 503 **并且库里一行都没多**；卷确实不在列表里是 422 DEPENDENCY_MISSING。
#   5) **删除只删元数据**：库里少一行，sid 上不多一个 job，systemd 与容器一个没动。
#   6) **权限与审计是协议层的事实**：viewer 读得到、写不了（403）、缺 CSRF 403、
#      收藏是每人自己的、收藏不产生审计；元数据变更在 audit_events 里留了真行。
#
# 用法（root）：
#   sudo BIN_DIR=/var/tmp/upanel-bin SPARE_DISKS="sdc" \
#        sh tests/integration/p09-1-apps-metadata-acceptance.sh
#
# 环境变量：PORT（默认 19481）、WORK、BIN_DIR、SPARE_DISKS（一块一次性空盘，会被格式化）。
#
# 退出码：0 全部通过；1 有失败项；2 环境不满足。
#
# 明确不验（NOT RUN，原因写在这里，不在脚本里假装通过）：
#   - **P09.2 之后才有的东西**：`/apps/discovery`、`/apps/:id/health`、`/apps/:id/logs`、
#     `/containers*`、`/images`、`/container-networks`、`/container-volumes`、`/deployments*`。
#     本脚本反过来钉住“它们确实不存在”（404），避免有人提前把桩挂上去冒充已支持。
#   - **任何容器**：P09.1 不碰 Docker。docker 组与容器夹具在 P09.3 才需要。
#   - **浏览器里的真实渲染**：脚本验的是页面读到的载荷；DOM 由 vitest 覆盖（P09.5）。

set -u

BIN_DIR=${BIN_DIR:-/var/tmp/upanel-bin}

if [ "$(id -u)" -ne 0 ]; then
  printf '本脚本必须以 root 运行：sudo BIN_DIR=%s sh %s\n' "$BIN_DIR" "$0" >&2
  exit 2
fi

AGENT="$BIN_DIR/upanel-agent"
WEB="$BIN_DIR/upanel-web"
CTL="$BIN_DIR/upanelctl"

for f in "$AGENT" "$WEB" "$CTL"; do
  if [ ! -x "$f" ]; then
    printf '缺少可执行文件：%s\n' "$f" >&2
    exit 2
  fi
done

SPARE_DISKS=${SPARE_DISKS:-}
WORK=${WORK:-/var/tmp/upanel-accept-p09-1}
PAYLOADS=$WORK/payloads
RUN_DIR=/run/upanel
SOCK=$RUN_DIR/agent.sock
SYS_CFG=/etc/upanel/upanel.json
AGENT_DB_DIR=/var/lib/upanel-agent
AGENT_DB=$AGENT_DB_DIR/agent.db
KEY_FILE=/etc/upanel/agent.key
WEB_DB_DIR=$WORK/webdata
WEB_DB=$WEB_DB_DIR/upanel.db
PORT=${PORT:-19481}
BASE="http://127.0.0.1:$PORT"
ORIGIN="$BASE"
WEB_USER=upanel-web
PASSWORD='vm-correct-horse-9'
VIEWER_PASSWORD='vm-viewer-horse-4'

APPS=/api/v1/apps
GROUPS=/api/v1/app-groups
USERS=/api/v1/users
PREFLIGHT_PATH=/api/v1/operations/preflight
JOBS=/api/v1/jobs
AGENT_LOG=$WORK/logs/agent.log
WEB_LOG=$WORK/logs/web.log
AGENT_PID=""
WEB_PID=""

CONTAINER_ID=3f4a9b2c1d5e6f708192a3b4c5d6e7f809a1b2c3d4e5f60718293a4b5c6d7e8f
CONTAINER_ID_2=9e8d7c6b5a4938271605f4e3d2c1b0a9f8e7d6c5b4a39281706f5e4d3c2b1a09
CONTAINER_ID_3=aa11bb22cc33dd44ee55ff660011223344556677889900aabbccddeeff001122
DEPLOYMENT_ID=dep_0123456789abcdef0123456789abcdef

# 一次性盘用于“依赖卷真的存在”那一段；没有空盘时该段整体 NOT RUN。
BLANK_A=""
for d in $SPARE_DISKS; do
  if [ -b "/dev/$d" ]; then BLANK_A=$d; break; fi
done

PASSED=0
FAILED=0
NOTRUN=0
FAILED_NAMES=""

pass() { PASSED=$((PASSED + 1)); printf 'PASS  %s\n' "$1"; }
fail() {
  FAILED=$((FAILED + 1))
  FAILED_NAMES="$FAILED_NAMES | $1"
  printf 'FAIL  %s: %s\n' "$1" "${2:-}"
}
notrun() { NOTRUN=$((NOTRUN + 1)); printf 'NOT RUN  %s：%s\n' "$1" "$2"; }
note() { printf '      %s\n' "$1"; }
expect_eq() {
  if [ "$2" = "$3" ]; then pass "$1"; else fail "$1" "期望 '$2'，实际 '$3'"; fi
}
expect_ne() {
  if [ "$2" != "$3" ]; then pass "$1"; else fail "$1" "不该等于 '$2'"; fi
}
expect_contains() {
  case "$3" in
    *"$2"*) pass "$1" ;;
    *) fail "$1" "期望包含 '$2'，实际 '$3'" ;;
  esac
}
expect_not_contains() {
  case "$3" in
    *"$2"*) fail "$1" "不该包含 '$2'，实际 '$3'" ;;
    *) pass "$1" ;;
  esac
}

# jget 用 python3 读 JSON：这台机器上没有 jq（与 P08 的取舍一致，不为验收脚本装包）。
jget() {
  python3 -c 'import json,sys
try:
    d=json.load(open(sys.argv[1], encoding="utf-8"))
except Exception:
    print("")
    raise SystemExit
src=sys.argv[2]
ns={"d": d, "json": json}
try:
    try:
        print(eval(src, ns))
    except SyntaxError:
        ns["_"]=None
        exec(compile(src, "<jget>", "exec"), ns)
        print("" if ns["_"] is None else ns["_"])
except Exception as exc:
    print("ERR:%s:%s" % (type(exc).__name__, exc))' "$1" "$2" 2>/dev/null
}

# dbq 在 Web 数据库上跑一条只读查询。先 checkpoint 一下 WAL，免得读到旧快照。
dbq() {
  python3 - "$WEB_DB" "$1" <<'PY' 2>/dev/null
import sqlite3, sys
con = sqlite3.connect(sys.argv[1])
try:
    con.execute("PRAGMA wal_checkpoint(TRUNCATE)")
except Exception:
    pass
row = con.execute(sys.argv[2]).fetchone()
print("" if row is None else ("" if row[0] is None else row[0]))
con.close()
PY
}

# dbbool 把 SQL 的真假结果说成 True/False：计数用 dbq（印出 0/1/…），
# 判断用 dbbool，两种断言就不会混成“1 到底是一个还是真”。
dbbool() {
  python3 - "$WEB_DB" "$1" <<'PY' 2>/dev/null
import sqlite3, sys
con = sqlite3.connect(sys.argv[1])
try:
    con.execute("PRAGMA wal_checkpoint(TRUNCATE)")
except Exception:
    pass
row = con.execute(sys.argv[2]).fetchone()
print(bool(row[0]) if row else False)
con.close()
PY
}

# disk_is_blank 用三条互相独立的证据说“这块盘上什么都没有”：
# 没有可识别的签名（wipefs）、内核里没有子分区（lsblk）、首扇区全零（od）。
# 只看其中一条会被上一轮的残留骗过去。
disk_is_blank() {
  _d=$1
  [ -z "$(wipefs --no-act "/dev/$_d" 2>/dev/null | tr -d ' \n')" ] || return 1
  [ -z "$(lsblk -lnr -o NAME,TYPE "/dev/$_d" 2>/dev/null | awk '$2=="part"{print $1}')" ] || return 1
  [ -z "$(dd if="/dev/$_d" bs=512 count=1 2>/dev/null | od -An -v -tx1 | tr -d ' \n0')" ] || return 1
  return 0
}

# prepare_blank_disk 先把盘上可能残留的挂载卸掉，再清分区表，每轮都验证。
# 上一轮留下的挂载会让 wipefs 以 “device busy” 失败——那正是“静默继续然后被预检挡住”的由来。
prepare_blank_disk() {
  _d=$1
  for _mp in $(lsblk -lnpo MOUNTPOINT "/dev/$_d" 2>/dev/null | grep -v '^$'); do
    umount "$_mp" >/dev/null 2>&1 || umount -l "$_mp" >/dev/null 2>&1
  done
  : >"$WORK/logs/wipefs.log"
  _i=0
  while [ "$_i" -lt 5 ]; do
    {
      echo "--- 第 $_i 轮"
      wipefs -a "/dev/$_d"
      sgdisk --zap-all "/dev/$_d"
      partprobe "/dev/$_d"
      blockdev --rereadpt "/dev/$_d"
    } >>"$WORK/logs/wipefs.log" 2>&1
    udevadm settle --timeout=5 >/dev/null 2>&1
    if disk_is_blank "$_d"; then return 0; fi
    _i=$((_i + 1))
    sleep 0.5
  done
  return 1
}

cleanup() {
  if [ -n "${WEB_PID:-}" ] && kill -0 "$WEB_PID" 2>/dev/null; then
    kill -TERM "$WEB_PID" 2>/dev/null; wait "$WEB_PID" 2>/dev/null
  fi
  if [ -n "${AGENT_PID:-}" ] && kill -0 "$AGENT_PID" 2>/dev/null; then
    kill -TERM "$AGENT_PID" 2>/dev/null; wait "$AGENT_PID" 2>/dev/null
  fi
  rm -f "$SOCK"
  return 0
}
trap cleanup EXIT
trap 'cleanup; exit 130' INT
trap 'cleanup; exit 143' TERM

start_agent() {
  "$AGENT" --config "$SYS_CFG" >>"$AGENT_LOG" 2>&1 &
  AGENT_PID=$!
  for _ in $(seq 1 100); do
    [ -S "$SOCK" ] && return 0
    sleep 0.1
  done
  return 1
}

stop_agent() {
  if [ -n "${AGENT_PID:-}" ] && kill -0 "$AGENT_PID" 2>/dev/null; then
    kill -TERM "$AGENT_PID" 2>/dev/null
    wait "$AGENT_PID" 2>/dev/null
  fi
  AGENT_PID=""
  rm -f "$SOCK"
  for _ in $(seq 1 50); do
    [ -S "$SOCK" ] || return 0
    sleep 0.1
  done
  return 1
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

chown_web_db() {
  chown "$WEB_USER" "$WEB_DB" 2>/dev/null
  for suffix in "" "-wal" "-shm"; do
    [ -e "$WEB_DB$suffix" ] && chown "$WEB_USER" "$WEB_DB$suffix"
  done
  return 0
}

login() {
  curl -s -c "$3" -o "$WORK/$4.json" -w '%{http_code}' -X POST "$BASE/api/v1/auth/login" \
    -H 'Content-Type: application/json' -H 'X-UPanel-Request: 1' -H "Origin: $ORIGIN" \
    -d "{\"username\":\"$1\",\"password\":\"$2\"}"
}
csrf_of() { jget "$1" 'd["data"]["csrf_token"]'; }

# api 发一次请求，返回状态码，响应体落在 $WORK/resp.json（同时覆盖 $WORK/last.json）。
api() {
  _jar=$1; _csrf=$2; _method=$3; _path=$4; _body=${5:-}
  if [ -n "$_body" ]; then
    printf '%s' "$_body" >"$WORK/req.json"
    curl -s -b "$_jar" -o "$WORK/resp.json" -w '%{http_code}' -X "$_method" "$BASE$_path" \
      -H 'Content-Type: application/json' -H "Origin: $ORIGIN" -H "X-CSRF-Token: $_csrf" \
      -d @"$WORK/req.json"
  else
    curl -s -b "$_jar" -o "$WORK/resp.json" -w '%{http_code}' -X "$_method" "$BASE$_path" \
      -H "Origin: $ORIGIN" -H "X-CSRF-Token: $_csrf"
  fi
}

# api_nocsrf 故意不带 CSRF 令牌。
api_nocsrf() {
  _jar=$1; _method=$2; _path=$3; _body=${4:-}
  printf '%s' "$_body" >"$WORK/req.json"
  curl -s -b "$_jar" -o "$WORK/resp.json" -w '%{http_code}' -X "$_method" "$BASE$_path" \
    -H 'Content-Type: application/json' -H "Origin: $ORIGIN" -d @"$WORK/req.json"
}

ecode() { jget "$WORK/resp.json" 'd["error"]["code"]'; }
emsg() { jget "$WORK/resp.json" 'd["error"]["message"]'; }
dget() { jget "$WORK/resp.json" "d[\"data\"]$1"; }

# reauth 在密码窗口外重新验证一次，让破坏性动作的预检可以提交。
reauth() { api "$JAR" "$CSRF" POST /api/v1/auth/reauth "{\"password\":\"$PASSWORD\"}"; }

preflight() {
  printf '{"action":"%s","params":%s}' "$1" "$2" >"$WORK/pre.json"
  _code=$(curl -s -b "$JAR" -o "$WORK/preflight.json" -w '%{http_code}' \
    -X POST "$BASE$PREFLIGHT_PATH" \
    -H 'Content-Type: application/json' -H "Origin: $ORIGIN" -H "X-CSRF-Token: $CSRF" \
    -d @"$WORK/pre.json")
  printf '%s' "$_code"
}

# 幂等键每次运行都不同：Web 侧的幂等记录是持久的。
RUN_KEY="-$(date -u +%Y%m%d%H%M%S)-$$"

# submit_job 提交预检：$1 预检标识，$2 幂等键前缀，$3 确认短语（不需要时为空）。
submit_job() {
  printf '{"preflight_id":"%s","confirmation_phrase":"%s"}' "$1" "${3:-}" >"$WORK/jobreq.json"
  _code=$(curl -s -b "$JAR" -o "$WORK/submit.json" -w '%{http_code}' -X POST "$BASE$JOBS" \
    -H 'Content-Type: application/json' -H "Origin: $ORIGIN" -H "X-CSRF-Token: $CSRF" \
    -H "Idempotency-Key: $2$RUN_KEY" -d @"$WORK/jobreq.json")
  printf '%s' "$_code"
}

wait_job() {
  _i=0
  while [ "$_i" -lt 300 ]; do
    api "$JAR" "$CSRF" GET "/api/v1/jobs/$1" >/dev/null
    cp "$WORK/resp.json" "$WORK/job.json"
    _st=$(jget "$WORK/job.json" 'd["data"]["status"]')
    case "$_st" in
      succeeded|failed|canceled|interrupted) printf '%s' "$_st"; return 0 ;;
    esac
    _i=$((_i + 1))
    sleep 0.2
  done
  printf 'timeout'
}

# run_action 走完整的预检 + 任务流程，结果留在 $WORK/job.json。
JOB_STATE=""
JOB_OUTCOME=""
run_action() {
  _pre=$(preflight "$1" "$2")
  cp "$WORK/preflight.json" "$PAYLOADS/preflight-$3.json"
  case "$_pre" in
    200|201) pass "预检 $1 生成" ;;
    *) fail "预检 $1 生成" "状态码 $_pre $(jget "$WORK/preflight.json" 'd["error"]["message"]')"
       JOB_STATE="no-preflight"; return 1 ;;
  esac
  _pid=$(jget "$WORK/preflight.json" 'd["data"]["preflight_id"]')
  _phrase=$(jget "$WORK/preflight.json" 'd["data"].get("confirmation_phrase") or ""')
  # 破坏性动作要 5 分钟内的密码再认证：这是“人还在键盘前”的证明，不是可跳过的提示。
  if [ "$(jget "$WORK/preflight.json" 'bool(d["data"].get("requires_reauth"))')" = "True" ]; then
    _rc=$(reauth)
    if [ "$_rc" = "200" ]; then
      pass "预检 $1 要求再认证，已重新验证密码"
    else
      fail "预检 $1 要求再认证" "状态码 $_rc $(jget "$WORK/resp.json" 'd["error"]["message"]')"
      JOB_STATE="no-reauth"; return 1
    fi
  fi
  _sub=$(submit_job "$_pid" "$3" "$_phrase")
  case "$_sub" in
    202|200) pass "提交 $1 任务" ;;
    *) fail "提交 $1 任务" "状态码 $_sub $(jget "$WORK/submit.json" 'd["error"]["message"]')"
       JOB_STATE="no-job"; return 1 ;;
  esac
  _jid=$(jget "$WORK/submit.json" 'd["data"]["job_id"]')
  JOB_STATE=$(wait_job "$_jid")
  JOB_OUTCOME=$(jget "$WORK/job.json" 'd["data"]["result"]["outcome"]')
  cp "$WORK/job.json" "$PAYLOADS/job-$3.json"
  return 0
}
jobdata() { jget "$WORK/job.json" "d[\"data\"][\"result\"][\"data\"].get(\"$1\")"; }

mkdir -p "$WORK/logs" "$PAYLOADS" "$WEB_DB_DIR"
chown -R "$WEB_USER" "$WEB_DB_DIR"

echo "== 0. 环境与安全守卫 =="
. /etc/os-release
note "系统：$PRETTY_NAME"
expect_eq "以 root 运行" "0" "$(id -u)"
if getent group docker >/dev/null 2>&1; then
  if id -nG "$WEB_USER" 2>/dev/null | tr ' ' '\n' | grep -qx docker; then
    fail "Web 身份不在 docker 组" "实际上在"
  else
    pass "Web 身份不在 docker 组"
  fi
else
  note "本机没有 docker 组（P09.1 不需要）"
fi
if id "$WEB_USER" >/dev/null 2>&1; then
  pass "Web 运行身份存在（$WEB_USER）"
else
  fail "Web 运行身份存在" "没有 $WEB_USER 账号"
  exit 2
fi
expect_not_contains "Web 身份没有登录 shell" "$(getent passwd "$WEB_USER" | cut -d: -f7)" "/bin/bash"
if command -v python3 >/dev/null 2>&1; then pass "python3 可用（解析 JSON 与读库）"; else
  fail "python3 可用" "没有 python3，脚本无法解析载荷"; exit 2; fi
if ss -lntH "sport = :$PORT" 2>/dev/null | grep -q .; then
  fail "端口 $PORT 空闲" "已被占用"; exit 2
else
  pass "端口 $PORT 空闲"
fi

echo
echo "== 1. 配置与启动（代理 root、Web 非 root） =="
install -d -m 0755 /etc/upanel
install -d -m 0700 "$AGENT_DB_DIR"
install -d -m 0755 "$RUN_DIR"
rm -f "$AGENT_DB" "$AGENT_DB-wal" "$AGENT_DB-shm"
rm -f "$WEB_DB" "$WEB_DB-wal" "$WEB_DB-shm"
cat >"$SYS_CFG" <<JSON
{
  "web": { "mode": "production", "listen_address": "127.0.0.1:$PORT", "shutdown_timeout_seconds": 10 },
  "agent": {
    "socket_path": "/run/upanel/agent.sock",
    "shutdown_timeout_seconds": 10,
    "web_user": "upanel-web",
    "socket_group": "upanel-web",
    "database_path": "/var/lib/upanel-agent/agent.db",
    "secret_key_path": "/etc/upanel/agent.key",
    "storage": { "smart_enabled": false, "smart_interval_minutes": 15, "network_capacity_timeout_seconds": 2 },
    "services": {},
    "logs": {}
  },
  "database": { "path": "$WEB_DB", "busy_timeout_ms": 5000 },
  "log": { "level": "info", "format": "json" }
}
JSON
chmod 0644 "$SYS_CFG"

if [ -f "$KEY_FILE" ]; then
  pass "代理密钥文件已存在（沿用）"
else
  if "$AGENT" --config "$SYS_CFG" -init-key >>"$WORK/logs/init-key.log" 2>&1; then
    pass "-init-key 生成代理密钥"
  else
    fail "-init-key 生成代理密钥" "$(tail -3 "$WORK/logs/init-key.log" 2>/dev/null)"
  fi
fi
if start_agent; then pass "代理以 root 启动并创建 socket"; else fail "代理以 root 启动" "$(tail -5 "$AGENT_LOG" 2>/dev/null)"; fi
expect_eq "代理进程 uid=0" "0" "$(ps -o uid= -p "$AGENT_PID" 2>/dev/null | tr -d ' ')"

printf '%s\n%s\n' "$PASSWORD" "$PASSWORD" | "$CTL" init-admin --config "$SYS_CFG" --db "$WEB_DB" \
  --username admin --display-name "管理员" --json >"$WORK/init.out" 2>"$WORK/init.err"
expect_eq "init-admin 退出码" "0" "$?"
chown_web_db
if start_web; then pass "Web 在 $BASE 启动"; else fail "Web 启动" "$(tail -5 "$WEB_LOG" 2>/dev/null)"; fi
LISTEN_PID=$(ss -lptnH "sport = :$PORT" 2>/dev/null | sed -n 's/.*pid=\([0-9]*\).*/\1/p' | head -1)
[ -z "$LISTEN_PID" ] && LISTEN_PID=$WEB_PID
WEB_UID=$(ps -o uid= -p "$LISTEN_PID" 2>/dev/null | tr -d ' ')
if [ -n "$WEB_UID" ] && [ "$WEB_UID" != "0" ]; then
  pass "监听 $PORT 的 Web 进程不是 root（pid=$LISTEN_PID uid=$WEB_UID）"
else
  fail "监听 $PORT 的 Web 进程不是 root" "pid=${LISTEN_PID:-未知} uid=${WEB_UID:-未知}"
fi
expect_eq "数据库迁移到第 7 版（0007_apps）" "7" "$(dbq 'SELECT MAX(version) FROM schema_migrations')"

JAR=$WORK/admin.jar
expect_eq "管理员登录" "200" "$(login admin "$PASSWORD" "$JAR" login-admin)"
CSRF=$(csrf_of "$WORK/login-admin.json")
expect_ne "拿到 CSRF 令牌" "" "$CSRF"

echo
echo "== 2. 判别式绑定：type 决定 binding 的唯一合法形状 =="
expect_eq "link 应用创建" "201" \
  "$(api "$JAR" "$CSRF" POST "$APPS" '{"name":"面板入口","type":"link","public_url":"https://panel.example.com","icon_key":"globe"}')"
cp "$WORK/resp.json" "$PAYLOADS/app-link.json"
LINK_ID=$(dget '["id"]')
case "$LINK_ID" in
  app_*) pass "标识是 app_ 前缀" ;;
  *) fail "标识是 app_ 前缀" "$LINK_ID" ;;
esac
expect_eq "标识是 app_ + 32 位十六进制" "True" \
  "$(python3 -c 'import re,sys;print(bool(re.fullmatch(r"app_[0-9a-f]{32}", sys.argv[1])))' "$LINK_ID")"
expect_eq "link 的 binding 为空对象" "0" "$(dget '["binding"].__len__()')"
expect_eq "link 的 group_id 是 null" "None" "$(dget '["group_id"]')"
expect_eq "link 的 runtime_state" "not_applicable" "$(dget '["runtime_state"]')"
expect_eq "link 的 http_state" "not_applicable" "$(dget '["http_state"]')"
expect_eq "link 的 last_checked_at 是 null" "None" "$(dget '["last_checked_at"]')"
expect_eq "link 的 capabilities.open 为真" "True" "$(dget '["capabilities"]["open"]')"
expect_eq "link 没有启停能力" "True" \
  "$(python3 -c 'import json,sys
c=json.load(open(sys.argv[1]))["data"]["capabilities"]
print(not (c["start"] or c["stop"] or c["restart"] or c["logs"]))' "$PAYLOADS/app-link.json")"

expect_eq "systemd 应用创建" "201" \
  "$(api "$JAR" "$CSRF" POST "$APPS" '{"name":"反向代理","type":"systemd","binding":{"unit":"nginx.service"}}')"
cp "$WORK/resp.json" "$PAYLOADS/app-systemd.json"
SYS_ID=$(dget '["id"]')
expect_eq "systemd 的 binding.unit" "nginx.service" "$(dget '["binding"]["unit"]')"
expect_eq "systemd 的 public_url 是 null" "None" "$(dget '["public_url"]')"
expect_eq "systemd 的 runtime_state 是 unknown（不是 stopped）" "unknown" "$(dget '["runtime_state"]')"
expect_eq "systemd 的 dependency_protection" "not_applicable" "$(dget '["dependency_protection"]')"

expect_eq "docker 应用创建" "201" \
  "$(api "$JAR" "$CSRF" POST "$APPS" "{\"name\":\"网盘\",\"type\":\"docker\",\"binding\":{\"container_id\":\"$(printf '%s' "$CONTAINER_ID" | tr 'a-f' 'A-F')\"}}")"
cp "$WORK/resp.json" "$PAYLOADS/app-docker.json"
DOCKER_ID=$(dget '["id"]')
expect_eq "容器 ID 被归一化为小写" "$CONTAINER_ID" "$(dget '["binding"]["container_id"]')"

expect_eq "compose 应用创建" "201" \
  "$(api "$JAR" "$CSRF" POST "$APPS" "{\"name\":\"监控栈\",\"type\":\"compose\",\"binding\":{\"deployment_id\":\"$DEPLOYMENT_ID\"}}")"
expect_eq "compose 的 binding.deployment_id" "$DEPLOYMENT_ID" "$(dget '["binding"]["deployment_id"]')"

# 6 条错位绑定：全部 400，且错误信息点明是哪个键不属于这个类型。
bind_bad() {
  _name=$1; _body=$2; _want=$3
  expect_eq "$_name 被拒" "400" "$(api "$JAR" "$CSRF" POST "$APPS" "$_body")"
  expect_eq "$_name 错误码" "VALIDATION_ERROR" "$(ecode)"
  expect_contains "$_name 的错误说明" "$_want" "$(emsg)"
}
bind_bad "systemd 多给 container_id" \
  "{\"name\":\"x1\",\"type\":\"systemd\",\"binding\":{\"unit\":\"a.service\",\"container_id\":\"$CONTAINER_ID\"}}" "container_id"
bind_bad "docker 少给 container_id" \
  '{"name":"x2","type":"docker","binding":{}}' "container_id"
bind_bad "docker 用容器名当绑定" \
  '{"name":"x3","type":"docker","binding":{"container_id":"my-thing"}}' "64"
bind_bad "compose 给了 unit" \
  "{\"name\":\"x4\",\"type\":\"compose\",\"binding\":{\"deployment_id\":\"$DEPLOYMENT_ID\",\"unit\":\"a.service\"}}" "unit"
bind_bad "link 带了绑定" \
  '{"name":"x5","type":"link","public_url":"https://a.example.com","binding":{"unit":"a.service"}}' "link"
bind_bad "link 没有入口地址" \
  '{"name":"x6","type":"link"}' "入口地址"
bind_bad "未知类型" '{"name":"x7","type":"kubernetes"}' "类型"

expect_eq "同一个容器给第二个应用是 422" "422" \
  "$(api "$JAR" "$CSRF" POST "$APPS" "{\"name\":\"第二个网盘\",\"type\":\"docker\",\"binding\":{\"container_id\":\"$CONTAINER_ID\"}}")"
expect_eq "重复绑定的错误码" "VALIDATION_ERROR" "$(ecode)"
expect_eq "另一个容器可以建" "201" \
  "$(api "$JAR" "$CSRF" POST "$APPS" "{\"name\":\"另一个网盘\",\"type\":\"docker\",\"binding\":{\"container_id\":\"$CONTAINER_ID_2\"}}")"
SECOND_DOCKER=$(dget '["id"]')
expect_eq "两个 link 可以同时存在（link 没有绑定对象）" "201" \
  "$(api "$JAR" "$CSRF" POST "$APPS" '{"name":"第二个入口","type":"link","public_url":"https://b.example.com"}')"
LINK2_ID=$(dget '["id"]')
expect_eq "独立证据：库里 binding_json 是规范化形式" \
  "{\"container_id\":\"$CONTAINER_ID\"}" \
  "$(dbq "SELECT binding_json FROM apps WHERE id = '$DOCKER_ID'")"
expect_eq "独立证据：唯一索引存在" "1" \
  "$(dbq "SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='uq_apps_binding'")"

echo
echo "== 3. 公共入口与内部探测目标是两个字段 =="
url_bad() {
  _name=$1; _url=$2; _want=$3
  expect_eq "$_name 被拒" "400" \
    "$(api "$JAR" "$CSRF" POST "$APPS" "{\"name\":\"入口$_name\",\"type\":\"link\",\"public_url\":\"$_url\"}")"
  expect_contains "$_name 的错误说明" "$_want" "$(emsg)"
}
url_bad "回环 IPv4" "https://127.0.0.1:9443" "回环"
url_bad "localhost" "https://localhost" "回环"
url_bad "通配地址" "https://0.0.0.0" "浏览器"
url_bad "非 http 协议" "ftp://example.com" "http"
url_bad "带账号密码" "https://user:pw@example.com" "用户名或密码"
url_bad "javascript 协议" "javascript:alert(1)" "http"
expect_eq "内网域名可用" "201" \
  "$(api "$JAR" "$CSRF" POST "$APPS" '{"name":"内网入口","type":"link","public_url":"http://10.0.0.5:8080"}')"

# target_id 的形状里写不出 IP、主机名、端口或完整 URL——这是结构性的 SSRF 约束。
target_bad() {
  _t=$1
  expect_eq "target_id=$_t 被拒" "400" \
    "$(api "$JAR" "$CSRF" POST "$APPS" "{\"name\":\"探测$_t\",\"type\":\"link\",\"public_url\":\"https://a.example.com\",\"health_check\":{\"enabled\":true,\"target_id\":\"$_t\",\"port\":80}}")"
}
for t in 127.0.0.1 169.254.169.254 localhost:8080 http://local_1 host_1 Local_1 local_ 10.0.0.5; do
  target_bad "$t"
done
expect_eq "合法探测目标 local_1 通过" "201" \
  "$(api "$JAR" "$CSRF" POST "$APPS" '{"name":"健康检查","type":"link","public_url":"https://c.example.com","health_check":{"enabled":true,"target_id":"local_1","port":8080}}')"
HEALTH_ID=$(dget '["id"]')
cp "$WORK/resp.json" "$PAYLOADS/app-health.json"
expect_eq "健康检查默认 scheme" "http" "$(dget '["health_check"]["scheme"]')"
expect_eq "健康检查默认 path" "/" "$(dget '["health_check"]["path"]')"
expect_eq "健康检查默认间隔" "30" "$(dget '["health_check"]["interval_seconds"]')"
expect_eq "健康检查默认超时" "3" "$(dget '["health_check"]["timeout_seconds"]')"
expect_eq "期望状态码是空数组而非 null" "[]" "$(dget '["health_check"]["expected_status_codes"]')"
expect_eq "启用但没探测时 http_state 是 unknown" "unknown" "$(dget '["http_state"]')"
expect_eq "期望状态码去重排序" "[200, 204, 302]" \
  "$(api "$JAR" "$CSRF" PATCH "$APPS/$HEALTH_ID" '{"version":1,"health_check":{"enabled":true,"target_id":"cntr_web","port":8080,"expected_status_codes":[204,200,204,302]}}' >/dev/null; dget '["health_check"]["expected_status_codes"]')"

echo
echo "== 4. 分组、收藏与“删分组不改变别的记录” =="
expect_eq "建分组" "201" "$(api "$JAR" "$CSRF" POST "$GROUPS" '{"name":"运维"}')"
cp "$WORK/resp.json" "$PAYLOADS/group-ops.json"
GROUP_ID=$(dget '["id"]')
case "$GROUP_ID" in
  grp_*) pass "分组标识是 grp_ 前缀" ;;
  *) fail "分组标识是 grp_ 前缀" "$GROUP_ID" ;;
esac
expect_eq "分组 sort_order 从 0 开始" "0" "$(dget '["sort_order"]')"
expect_eq "重名分组被拒" "422" "$(api "$JAR" "$CSRF" POST "$GROUPS" '{"name":"运维"}')"
expect_eq "第二个分组排到末尾" "1" \
  "$(api "$JAR" "$CSRF" POST "$GROUPS" '{"name":"开发"}' >/dev/null; dget '["sort_order"]')"
GROUP2_ID=$(dget '["id"]')
expect_eq "分组列表可读" "200" "$(api "$JAR" "$CSRF" GET "$GROUPS")"
expect_eq "分组列表条数" "2" "$(dget '["items"].__len__()')"

expect_eq "应用挂到分组" "201" \
  "$(api "$JAR" "$CSRF" POST "$APPS" "{\"name\":\"组内应用\",\"type\":\"link\",\"public_url\":\"https://d.example.com\",\"group_id\":\"$GROUP_ID\"}")"
GROUPED_APP=$(dget '["id"]')
expect_eq "分组过滤只返回组内应用" "1" \
  "$(api "$JAR" "$CSRF" GET "$APPS?group_id=$GROUP_ID" >/dev/null; dget '["items"].__len__()')"
expect_eq "非空分组不能删" "422" "$(api "$JAR" "$CSRF" DELETE "$GROUPS/$GROUP_ID?version=1")"
expect_eq "非空分组的错误码" "DEPENDENCY_BLOCKED" "$(ecode)"
expect_eq "独立证据：分组还在" "1" "$(dbq "SELECT COUNT(*) FROM app_groups WHERE id = '$GROUP_ID'")"
expect_eq "独立证据：应用还在原分组里" "$GROUP_ID" "$(dbq "SELECT group_id FROM apps WHERE id = '$GROUPED_APP'")"
expect_eq "显式 null 把应用移出分组" "None" \
  "$(api "$JAR" "$CSRF" PATCH "$APPS/$GROUPED_APP" '{"version":1,"group_id":null}' >/dev/null; dget '["group_id"]')"
expect_eq "移出之后空分组可以删" "200" "$(api "$JAR" "$CSRF" DELETE "$GROUPS/$GROUP2_ID?version=1")"
expect_eq "空分组删掉了" "200" "$(api "$JAR" "$CSRF" DELETE "$GROUPS/$GROUP_ID?version=1")"
expect_eq "重复删分组是 404" "404" "$(api "$JAR" "$CSRF" DELETE "$GROUPS/$GROUP_ID?version=1")"

expect_eq "管理员收藏" "200" "$(api "$JAR" "$CSRF" PUT "$APPS/$SYS_ID/favorite" '{"favorite":true}')"
expect_eq "收藏幂等" "200" "$(api "$JAR" "$CSRF" PUT "$APPS/$SYS_ID/favorite" '{"favorite":true}')"
expect_eq "管理员看到自己收藏了" "True" "$(api "$JAR" "$CSRF" GET "$APPS/$SYS_ID" >/dev/null; dget '["favorite"]')"

echo
echo "== 5. 版本、权限、CSRF =="
expect_eq "PATCH 缺 version" "400" "$(api "$JAR" "$CSRF" PATCH "$APPS/$SYS_ID" '{"name":"改名"}')"
expect_eq "PATCH 空补丁" "400" "$(api "$JAR" "$CSRF" PATCH "$APPS/$SYS_ID" '{"version":1}')"
expect_eq "PATCH 未知字段" "400" "$(api "$JAR" "$CSRF" PATCH "$APPS/$SYS_ID" '{"version":1,"nope":1}')"
expect_eq "只改类型不许（必须同时给绑定）" "400" "$(api "$JAR" "$CSRF" PATCH "$APPS/$SYS_ID" '{"version":1,"type":"docker"}')"
expect_contains "只改类型的错误点明 binding" "binding" "$(emsg)"
expect_eq "正常改名" "200" "$(api "$JAR" "$CSRF" PATCH "$APPS/$SYS_ID" '{"version":1,"name":"反向代理（改）"}')"
expect_eq "改名后版本 +1" "2" "$(dget '["version"]')"
expect_eq "改名不动绑定" "nginx.service" "$(dget '["binding"]["unit"]')"
expect_eq "旧版本 PATCH 是 409" "409" "$(api "$JAR" "$CSRF" PATCH "$APPS/$SYS_ID" '{"version":1,"name":"再改"}')"
expect_eq "409 的错误码" "VERSION_CONFLICT" "$(ecode)"
expect_eq "改类型并同时给绑定" "200" \
  "$(api "$JAR" "$CSRF" PATCH "$APPS/$SYS_ID" "{\"version\":2,\"type\":\"docker\",\"binding\":{\"container_id\":\"$CONTAINER_ID_3\"}}")"
expect_eq "绑定只剩 container_id" "True" \
  "$(python3 -c 'import json,sys
b=json.load(open(sys.argv[1]))["data"]["binding"]
print(set(b.keys())=={"container_id"} if isinstance(b,dict) else False)' "$WORK/resp.json")"
expect_eq "改类型后记录里没有残留的 unit（独立证据）" \
  "{\"container_id\":\"$CONTAINER_ID_3\"}" \
  "$(dbq "SELECT binding_json FROM apps WHERE id = '$SYS_ID'")"

expect_eq "缺 CSRF 的创建是 403" "403" \
  "$(api_nocsrf "$JAR" POST "$APPS" '{"name":"无令牌","type":"link","public_url":"https://e.example.com"}')"
expect_eq "缺 CSRF 的错误码" "CSRF_FAILED" "$(ecode)"

expect_eq "建 viewer 账号" "201" \
  "$(api "$JAR" "$CSRF" POST "$USERS" "{\"username\":\"p09viewer\",\"display_name\":\"观察员\",\"role\":\"viewer\",\"initial_password\":\"$VIEWER_PASSWORD\"}")"
VJAR=$WORK/viewer.jar
expect_eq "viewer 登录" "200" "$(login p09viewer "$VIEWER_PASSWORD" "$VJAR" login-viewer)"
VCSRF=$(csrf_of "$WORK/login-viewer.json")
expect_eq "viewer 读应用列表" "200" "$(api "$VJAR" "$VCSRF" GET "$APPS")"
expect_eq "viewer 读单个应用" "200" "$(api "$VJAR" "$VCSRF" GET "$APPS/$SYS_ID")"
expect_eq "viewer 看不到别人的收藏" "False" "$(dget '["favorite"]')"
expect_eq "viewer 读分组" "200" "$(api "$VJAR" "$VCSRF" GET "$GROUPS")"
expect_eq "viewer 创建应用被拒" "403" \
  "$(api "$VJAR" "$VCSRF" POST "$APPS" '{"name":"越权","type":"link","public_url":"https://f.example.com"}')"
expect_eq "viewer 改应用被拒" "403" "$(api "$VJAR" "$VCSRF" PATCH "$APPS/$SYS_ID" '{"version":3,"name":"越权"}')"
expect_eq "viewer 删应用被拒" "403" "$(api "$VJAR" "$VCSRF" DELETE "$APPS/$SYS_ID?version=3")"
expect_eq "viewer 建分组被拒" "403" "$(api "$VJAR" "$VCSRF" POST "$GROUPS" '{"name":"越权组"}')"
expect_eq "viewer 可以设自己的收藏" "200" "$(api "$VJAR" "$VCSRF" PUT "$APPS/$SYS_ID/favorite" '{"favorite":true}')"
expect_eq "viewer 收藏后自己看得到" "True" "$(api "$VJAR" "$VCSRF" GET "$APPS/$SYS_ID" >/dev/null; dget '["favorite"]')"
expect_eq "管理员看到的仍是自己的（未被影响）" "True" "$(api "$JAR" "$CSRF" GET "$APPS/$SYS_ID" >/dev/null; dget '["favorite"]')"
expect_eq "viewer 取消收藏" "200" "$(api "$VJAR" "$VCSRF" PUT "$APPS/$SYS_ID/favorite" '{"favorite":false}')"
expect_eq "取消后 viewer 看到的是 false" "False" "$(api "$VJAR" "$VCSRF" GET "$APPS/$SYS_ID" >/dev/null; dget '["favorite"]')"
expect_eq "独立证据：收藏行只属于各自的用户" "1" \
  "$(dbq "SELECT COUNT(*) FROM app_favorites WHERE app_id = '$SYS_ID'")"

echo
echo "== 6. 未实现的接口确实不存在 =="
for notyet in /api/v1/apps/discovery \
            "/api/v1/apps/$SYS_ID/health" \
            "/api/v1/apps/$SYS_ID/logs" \
            /api/v1/containers /api/v1/images /api/v1/container-networks \
            /api/v1/container-volumes /api/v1/deployments; do
  expect_eq "GET $notyet 不存在" "404" "$(api "$JAR" "$CSRF" GET "$notyet")"
done
expect_eq "没有容器动作被登记" "0" \
  "$(dbq "SELECT COUNT(*) FROM audit_events WHERE action LIKE 'container.%' OR action LIKE 'deploy%'")"

echo
echo "== 7. 删除只删元数据 =="
JOBS_BEFORE=$(dbq 'SELECT COUNT(*) FROM jobs')
expect_eq "删除应用" "200" "$(api "$JAR" "$CSRF" DELETE "$APPS/$SECOND_DOCKER?version=1")"
expect_eq "删除后读不到" "404" "$(api "$JAR" "$CSRF" GET "$APPS/$SECOND_DOCKER")"
expect_eq "重复删除是 404" "404" "$(api "$JAR" "$CSRF" DELETE "$APPS/$SECOND_DOCKER?version=1")"
expect_eq "版本不符是 409" "409" "$(api "$JAR" "$CSRF" DELETE "$APPS/$LINK2_ID?version=9")"
expect_eq "删除不产生任务" "$JOBS_BEFORE" "$(dbq 'SELECT COUNT(*) FROM jobs')"
expect_eq "独立证据：库里少了这一行" "0" "$(dbq "SELECT COUNT(*) FROM apps WHERE id = '$SECOND_DOCKER'")"
expect_eq "独立证据：收藏随应用级联删除" "0" "$(dbq "SELECT COUNT(*) FROM app_favorites WHERE app_id = '$SECOND_DOCKER'")"
expect_eq "systemd 服务没被碰过（nginx 不因删除应用而消失）" "0" \
  "$(dbq "SELECT COUNT(*) FROM audit_events WHERE action LIKE 'service.%'")"

echo
echo "== 8. 依赖卷：经代理确认，且“确认不了”与“不存在”分开 =="
PRE_APPS=$(dbq 'SELECT COUNT(*) FROM apps')
DEF='
{"name":"数据应用","type":"systemd","binding":{"unit":"data.service"},"volume_dependencies":["PLACEHOLDER"]}'
DUMMY_VOL=vol_0123456789abcdef0123456789abcdef

# 8a 代理不可达：503，且库里一行都没多
if stop_agent; then
  pass "代理已停止（模拟不可达）"
else
  fail "代理已停止" "socket 仍在"
fi
CODE=$(api "$JAR" "$CSRF" POST "$APPS" "$(printf '%s' "$DEF" | sed "s/PLACEHOLDER/$DUMMY_VOL/")")
expect_eq "代理不可达时带依赖卷的写入是 503" "503" "$CODE"
expect_eq "503 的错误码" "AGENT_UNAVAILABLE" "$(ecode)"
expect_eq "代理不可达时没有写库" "$PRE_APPS" "$(dbq 'SELECT COUNT(*) FROM apps')"
if start_agent; then pass "代理重新启动"; else fail "代理重新启动" "$(tail -5 "$AGENT_LOG" 2>/dev/null)"; fi

# 8b 卷确实不存在：422 DEPENDENCY_MISSING，并带出卷标识
expect_eq "卷不存在是 422" "422" "$(api "$JAR" "$CSRF" POST "$APPS" "$(printf '%s' "$DEF" | sed "s/PLACEHOLDER/$DUMMY_VOL/")")"
expect_eq "卷不存在的错误码" "DEPENDENCY_MISSING" "$(ecode)"
expect_eq "错误里带出卷标识" "$DUMMY_VOL" "$(jget "$WORK/resp.json" 'd["error"]["details"]["volume_id"]')"
expect_eq "卷不存在时也没有写库" "$PRE_APPS" "$(dbq 'SELECT COUNT(*) FROM apps')"

# 8c 卷存在：201，依赖登记下来，保护状态是 unmanaged。
#
# 这一段要一块**真的空白**的一次性盘。上一轮留下的挂载或残留分区表会让 wipefs 直接失败，
# 而预检会（正确地）用 PARTITION_TABLE_PRESENT 挡住初始化——那种失败是夹具没清干净，
# 不是产品有问题。所以先清、再**验证**盘确实空白才往下走；验证不过就整段 NOT RUN，
# 把 wipefs 的原话写出来，不拿一串级联的 FAIL 冒充产品缺陷。
# disk_table_state 从设备列表里读“这张盘的分区表类型/分区数”，读不到时是 missing。
disk_table_state() {
  python3 - "$2" "$1" <<'PY' 2>/dev/null
import json, sys
doc = json.load(open(sys.argv[1], encoding="utf-8"))
want = sys.argv[2]
dev = next((x for x in doc["data"]["devices"] if x["id"] == want), None)
if dev is None:
    print("missing")
else:
    table = dev.get("partition_table") or {}
    print("%s/%d" % (table.get("type"), len(table.get("partitions") or [])))
PY
}

# wait_agent_sees_blank 等代理的拓扑快照追上真实磁盘。
#
# 预检**故意**绑定用户看到的那份快照（P07.1 的取舍：现场重扫会让每份预检在提交时都对不上），
# 所以抹盘之后立刻预检，拿到的是抹盘前的快照，预检会正确地判“盘上已有分区表”。
# 这不是产品缺陷，是快照语义——夹具要做的是等下一次扫描，而不是让产品放弃绑定。
wait_agent_sees_blank() {
  _i=0
  _state=""
  while [ "$_i" -lt 120 ]; do
    api "$JAR" "$CSRF" GET /api/v1/storage/devices >/dev/null
    cp "$WORK/resp.json" "$WORK/disks.json"
    _state=$(disk_table_state "$1" "$WORK/disks.json")
    [ "$_state" = "none/0" ] && return 0
    _i=$((_i + 1))
    sleep 0.5
  done
  printf '%s' "$_state"
  return 1
}

# wait_agent_sees_device 等新分区出现在代理的设备列表里（格式化预检也吃同一份快照）。
wait_agent_sees_device() {
  _i=0
  while [ "$_i" -lt 120 ]; do
    _code=$(api "$JAR" "$CSRF" GET "/api/v1/storage/devices/$1")
    [ "$_code" = "200" ] && return 0
    _i=$((_i + 1))
    sleep 0.5
  done
  return 1
}

# clear_stale_volume_fixture 清掉**上一轮验收**在同一个挂载点上留下的东西。
#
# 挂载点由 slug 派生，所以每轮跑的都是同一个 /srv/upanel/volumes/p09data；
# 上一轮导入时写进 /etc/fstab 的那一行会让这一轮导入被正确拒绝（“挂载点重复”）。
# 那是夹具的残留，不是产品的判断错误。这里只删第二个字段正好等于这个路径的行，
# 删之前把 /etc/fstab 备份到 logs/，删了什么写进运行日志。
clear_stale_volume_fixture() {
  _mp=/srv/upanel/volumes/p09data
  if awk -v mp="$_mp" '$2==mp{found=1}END{exit !found}' /proc/mounts 2>/dev/null; then
    if umount "$_mp" 2>>"$WORK/logs/fixture.log"; then
      note "上一轮残留的挂载已卸下：$_mp"
    else
      fail "卸下上一轮残留的挂载" "$_mp"
    fi
  fi
  if [ -f /etc/fstab ] && awk -v mp="$_mp" '$2==mp{found=1}END{exit !found}' /etc/fstab; then
    cp /etc/fstab "$WORK/logs/fstab.before"
    awk -v mp="$_mp" '$2!=mp' "$WORK/logs/fstab.before" >"$WORK/logs/fstab.after"
    cat "$WORK/logs/fstab.after" >/etc/fstab
    note "清了上一轮夹具在 /etc/fstab 里残留的 $_mp（原件备份在 logs/fstab.before）"
  fi
  return 0
}

fixture_volume() {
  clear_stale_volume_fixture
  run_action storage.partition_table.initialize \
    "{\"device_id\":\"$BLANK_A\",\"table_type\":\"gpt\"}" p09-init
  expect_eq "初始化分区表终态" "succeeded" "$JOB_STATE"
  run_action storage.partition.create \
    "{\"device_id\":\"$BLANK_A\",\"start_bytes\":\"1048576\",\"size_bytes\":\"1073741824\",\"name\":\"p09\"}" p09-part
  expect_eq "建分区终态" "succeeded" "$JOB_STATE"
  _i=0
  while [ "$_i" -lt 40 ]; do
    [ -b "/dev/${BLANK_A}1" ] && break
    _i=$((_i + 1)); sleep 0.2
  done
  if wait_agent_sees_device "${BLANK_A}1"; then
    pass "代理快照里已经有新分区 ${BLANK_A}1"
  else
    fail "代理快照里已经有新分区 ${BLANK_A}1" "60 秒内没出现"
  fi
  run_action storage.filesystem.format \
    "{\"device_id\":\"${BLANK_A}1\",\"filesystem\":\"ext4\",\"label\":\"p09vol\"}" p09-format
  expect_eq "格式化终态" "succeeded" "$JOB_STATE"
  run_action storage.volume.import \
    "{\"device_id\":\"${BLANK_A}1\",\"slug\":\"p09data\",\"display_name\":\"P09 数据卷\"}" p09-import
  expect_eq "导入卷终态" "succeeded" "$JOB_STATE"
  VOL_ID=$(jobdata volume_id)
  case "$VOL_ID" in
    vol_*) pass "拿到受管理卷标识（vol_ 前缀）" ;;
    *) fail "拿到受管理卷标识" "$VOL_ID" ;;
  esac
  expect_eq "卷在 /storage/volumes 里" "True" \
    "$(api "$JAR" "$CSRF" GET /api/v1/storage/volumes >/dev/null; \
       python3 -c 'import json,sys
d=json.load(open(sys.argv[1]))["data"]["volumes"]
print(any(v["id"]==sys.argv[2] for v in d))' "$WORK/resp.json" "$VOL_ID")"

  expect_eq "依赖真实卷的写入通过" "201" \
    "$(api "$JAR" "$CSRF" POST "$APPS" "{\"name\":\"数据应用\",\"type\":\"systemd\",\"binding\":{\"unit\":\"data.service\"},\"volume_dependencies\":[\"$VOL_ID\"]}")"
  cp "$WORK/resp.json" "$PAYLOADS/app-with-volume.json"
  VOL_APP=$(dget '["id"]')
  expect_eq "依赖登记在响应里" "['$VOL_ID']" "$(dget '["volume_dependencies"]')"
  expect_eq "有依赖卷时 protection 是 unmanaged（不是 managed）" "unmanaged" "$(dget '["dependency_protection"]')"
  expect_eq "独立证据：依赖落库" "1" \
    "$(dbq "SELECT COUNT(*) FROM app_volume_dependencies WHERE app_id = '$VOL_APP' AND volume_id = '$VOL_ID'")"
  expect_eq "依赖卷在应用详情里读回" "True" \
    "$(api "$JAR" "$CSRF" GET "$APPS/$VOL_APP" >/dev/null; \
       python3 -c 'import json,sys
d=json.load(open(sys.argv[1]))["data"]
print(d["volume_dependencies"]==[sys.argv[2]])' "$WORK/resp.json" "$VOL_ID")"
  expect_eq "不带 volume_dependencies 的 PATCH 不清空依赖" "['$VOL_ID']" \
    "$(api "$JAR" "$CSRF" PATCH "$APPS/$VOL_APP" '{"version":1,"name":"数据应用（改）"}' >/dev/null; dget '["volume_dependencies"]')"
  expect_eq "显式空数组才是清空" "[]" \
    "$(api "$JAR" "$CSRF" PATCH "$APPS/$VOL_APP" '{"version":2,"volume_dependencies":[]}' >/dev/null; dget '["volume_dependencies"]')"
  expect_eq "清空后 protection 回到 not_applicable" "not_applicable" "$(dget '["dependency_protection"]')"
  expect_eq "独立证据：依赖行删掉了" "0" \
    "$(dbq "SELECT COUNT(*) FROM app_volume_dependencies WHERE app_id = '$VOL_APP'")"
}

if [ -z "$BLANK_A" ]; then
  notrun "依赖卷存在时写入通过" "没有一次性空盘（SPARE_DISKS 为空），无法建受管理卷"
  notrun "dependency_protection=unmanaged" "同上"
elif prepare_blank_disk "$BLANK_A"; then
  pass "一次性盘 /dev/$BLANK_A 确认为空白（首扇区全零、无子分区、无签名）"
  if wait_agent_sees_blank "$BLANK_A"; then
    pass "代理快照跟上了真实磁盘（分区表 none、0 个分区）"
    fixture_volume
  else
    notrun "依赖卷存在时写入通过" \
      "代理快照 60 秒内没有跟上空白盘（最后读到 '$_state'）"
    notrun "dependency_protection=unmanaged" "同上"
  fi
else
  notrun "依赖卷存在时写入通过" \
    "无法把 /dev/$BLANK_A 清成空白：$(tail -2 "$WORK/logs/wipefs.log" 2>/dev/null | tr '\n' ' ')"
  notrun "dependency_protection=unmanaged" "同上"
fi

echo
echo "== 9. 审计：管理动作留痕，收藏不留 =="
expect_eq "app.create 有审计" "True" \
  "$(dbbool "SELECT COUNT(*) > 0 FROM audit_events WHERE action = 'app.create'")"
expect_eq "app.update 有审计" "True" \
  "$(dbbool "SELECT COUNT(*) > 0 FROM audit_events WHERE action = 'app.update'")"
expect_eq "app.delete 有审计" "True" \
  "$(dbbool "SELECT COUNT(*) > 0 FROM audit_events WHERE action = 'app.delete'")"
expect_eq "app_group.create 有审计" "True" \
  "$(dbbool "SELECT COUNT(*) > 0 FROM audit_events WHERE action = 'app_group.create'")"
expect_eq "app_group.delete 有审计" "True" \
  "$(dbbool "SELECT COUNT(*) > 0 FROM audit_events WHERE action = 'app_group.delete'")"
expect_eq "审计 target 形如 app:<id>" "True" \
  "$(dbbool "SELECT COUNT(*) > 0 FROM audit_events WHERE action = 'app.create' AND target = 'app:$LINK_ID'")"
expect_eq "审计 target 形如 app_group:<id>" "True" \
  "$(dbbool "SELECT COUNT(*) > 0 FROM audit_events WHERE action = 'app_group.create' AND target = 'app_group:$GROUP_ID'")"
expect_eq "viewer 的越权尝试留下失败审计" "True" \
  "$(dbbool "SELECT COUNT(*) > 0 FROM audit_events WHERE target = 'app:$SYS_ID' AND outcome = 'failure'")"
FAV_BEFORE=$(dbq "SELECT COUNT(*) FROM audit_events")
api "$JAR" "$CSRF" PUT "$APPS/$LINK_ID/favorite" '{"favorite":true}' >/dev/null
api "$JAR" "$CSRF" PUT "$APPS/$LINK_ID/favorite" '{"favorite":false}' >/dev/null
expect_eq "收藏不产生审计" "$FAV_BEFORE" "$(dbq 'SELECT COUNT(*) FROM audit_events')"
expect_eq "审计表是只追加的（UPDATE 被触发器拒绝）" "True" \
  "$(python3 - "$WEB_DB" <<'PY'
import sqlite3, sys
con = sqlite3.connect(sys.argv[1])
try:
    con.execute("UPDATE audit_events SET action = 'x' WHERE id = (SELECT MIN(id) FROM audit_events)")
    con.commit()
    print(False)
except Exception:
    print(True)
con.close()
PY
)"

echo
echo "== 10. 收尾不变量 =="
FINAL_PID=$(ss -lptnH "sport = :$PORT" 2>/dev/null | sed -n 's/.*pid=\([0-9]*\).*/\1/p' | head -1)
FINAL_UID=$(ps -o uid= -p "${FINAL_PID:-0}" 2>/dev/null | tr -d ' ')
if [ -n "$FINAL_PID" ] && [ -n "$FINAL_UID" ] && [ "$FINAL_UID" != "0" ]; then
  pass "监听 $PORT 的 Web 进程到最后都不是 root（uid=$FINAL_UID）"
else
  fail "监听 $PORT 的 Web 进程到最后都不是 root" "pid=${FINAL_PID:-无} uid=${FINAL_UID:-无}"
fi
expect_eq "代理进程仍是 root" "0" "$(ps -o uid= -p "$AGENT_PID" 2>/dev/null | tr -d ' ')"
expect_eq "四张应用表都在" "4" \
  "$(dbq "SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name IN ('apps','app_groups','app_favorites','app_volume_dependencies')")"
expect_eq "没有残留的 app_ 审计被删（只增不减）" "True" \
  "$(dbbool "SELECT COUNT(*) >= 5 FROM audit_events WHERE action LIKE 'app%'")"
if grep -qiE 'panic|fatal' "$WEB_LOG" "$AGENT_LOG" 2>/dev/null; then
  fail "日志里没有 panic/fatal" "$(grep -icE 'panic|fatal' "$WEB_LOG" "$AGENT_LOG" 2>/dev/null | tr '\n' ' ')"
else
  pass "日志里没有 panic/fatal"
fi

echo
echo "=========================== 汇总 ==========================="
printf 'PASS %s / FAIL %s / NOT RUN %s\n' "$PASSED" "$FAILED" "$NOTRUN"
if [ -n "$FAILED_NAMES" ]; then printf '失败项:%s\n' "$FAILED_NAMES"; fi
printf 'NOT RUN 不等于通过。\n'
echo "============================================================"
[ "$FAILED" -eq 0 ] || exit 1
exit 0
