#!/bin/sh
# P09.3 Linux 验收脚本：Docker 容器/镜像/网络/数据卷/compose 项目的只读查询、容器日志
# 分页与实时流，以及“现有受支持容器”的启停（预检 → 任务 → 读回），危险容器只显示不接管。
#
# 运行位置：Ubuntu 22.04/24.04 验收 VM 内，**必须以 root 运行**（代理只能在 root 下启动，
# 并以 upanel-web 身份核实 socket 边界）。不依赖 Go 工具链：用宿主交叉编译好的 Linux 二进制。
#
# 用法：
#   sudo BIN_DIR=/var/tmp/upanel-bin sh tests/integration/p09-3-container-acceptance.sh
#
# 退出码：0 全部通过；1 有失败项；2 环境不满足（非 root / 二进制缺失）。
#
# 覆盖的验收点（STAGES §11 P09.3、CONTRACTS §8.3、PLAN §12）：
#   1. Web 非 root、代理 root；Web 账号不在 docker 组、不连 docker.sock
#   2. GET /containers：available、状态、端口、挂载、compose 标签、资源、风险与能力
#   3. 危险容器（privileged / host 网络 / 危险 capability / 挂 docker.sock）：
#      risk.level=high、managed=false、三项能力全 false
#   4. 容器环境变量与完整标签从不外泄：夹具里放了唯一口令，全流程响应体里找不到
#   5. 容器身份只认完整 64 位 ID：名字/短 ID/大写一律 400，且不会到达 docker
#   6. 容器日志：从新到旧、tail 边界、游标往返（严格更早、无边界重复）、脱敏
#   7. 镜像 / 网络 / 数据卷 / 部署（compose 只读项目，能力三项固定 false）
#   8. 启停：预检 → 任务 → 读回验证，并与 `docker inspect` 现场对照；
#      退出码 0 不算成功，读回状态一致才算
#   9. 危险容器与状态冲突在预检就被拒；提交被拒的预检得到 409，且容器状态不变
#  10. 预检之后状态变了（外部 restart）→ 任务失败为 PREFLIGHT_STALE，不照旧预检硬上
#  11. 两种“不可用”分开：代理停掉是 AGENT_UNAVAILABLE，dockerd 停掉是 DOCKER_UNAVAILABLE
#  12. viewer 读容器清单被拒、写被拒；审计里有 container.control 的 preflight/job.submit
#
# 明确不覆盖（NOT RUN，原因写在这里，不在脚本里假装通过）：
#   - 浏览器渲染：VM 上没有浏览器。页面级证据在宿主上用真实载荷回放。
#   - pam 之外的其它 Docker 版本/加速器：本机只有 apt 的 docker.io，验收范围就是它。
#   - “docker 返回 0 但状态没变”的路径：需要人为构造一个骗人的 docker，正常 daemon 上
#     做不到，仅由 internal/containers/containers_test.go 的 fake 覆盖（标 NOT RUN）。
#   - 危险容器“执行前复核”拒绝：预检就拒了，要构造“预检后被改成危险容器且 ID 不变”
#     才能在真实 docker 上走到，仅由 internal/agent/containers_test.go 覆盖（标 NOT RUN）。

set -u

BIN_DIR=${BIN_DIR:-/var/tmp/upanel-bin}
HERE=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)

if [ "$(id -u)" -ne 0 ]; then
  printf '本脚本必须以 root 运行：sudo BIN_DIR=%s sh %s\n' "$BIN_DIR" "$0" >&2
  exit 2
fi

AGENT="$BIN_DIR/upanel-agent"
WEB="$BIN_DIR/upanel-web"
CTL="$BIN_DIR/upanelctl"

WORK=${WORK:-/var/tmp/upanel-accept-p09-3}
PAYLOADS=$WORK/payloads
RUN_DIR=/run/upanel
SOCK=$RUN_DIR/agent.sock
SYS_CFG=/etc/upanel/upanel.json
AGENT_DB_DIR=/var/lib/upanel-agent
AGENT_DB=$AGENT_DB_DIR/agent.db
KEY_FILE=/etc/upanel/agent.key
WEB_DB_DIR=$WORK/webdata
WEB_DB=$WEB_DB_DIR/upanel.db
PORT=${PORT:-19493}
BASE="http://127.0.0.1:$PORT"
ORIGIN="$BASE"
WEB_USER=upanel-web
PASSWORD='vm-correct-horse-9'
VIEWER_PASSWORD='vm-viewer-horse-4'

# 夹具。名字全部带 upn-p093- 前缀，收尾时按名字删干净，不在机器上留东西。
CNT_IMAGE=${CNT_IMAGE:-alpine:3.20}
NAME_OK=upn-p093-ok
NAME_STOPPED=upn-p093-stopped
NAME_PRIV=upn-p093-priv
NAME_HOSTNET=upn-p093-hostnet
NAME_SOCK=upn-p093-sock
NAME_CAP=upn-p093-cap
NAME_LOG=upn-p093-log
NET=upn-p093-net
VOL=upn-p093-vol
COMPOSE_PROJECT=upnaccept

# 两个唯一的秘密：一个放在容器环境变量里（必须永不出现在任何响应里），
# 一个写进容器日志（必须被日志脱敏成 [redacted]）。
SECRET_ENV="vm-p093-env-secret-7c41"
SECRET_LOG="vm-p093-log-secret-2b93"

CONTAINERS=/api/v1/containers
IMAGES=/api/v1/images
NETWORKS=/api/v1/container-networks
VOLUMES=/api/v1/container-volumes
DEPLOYMENTS=/api/v1/deployments
PREFLIGHT_PATH=/api/v1/operations/preflight
JOBS_PATH=/api/v1/jobs
USERS=/api/v1/users

AGENT_LOG=$WORK/logs/agent.log
WEB_LOG=$WORK/logs/web.log
AGENT_PID=""
WEB_PID=""
HAS_DOCKER=0
DOCKER_READY=0
WEB_RUNNING=0

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
    *"$2"*) fail "$1" "不该包含 '$2'" ;;
    *) pass "$1" ;;
  esac
}

# jget 用 python3 读 JSON：这台机器上没有 jq（与 P05—P09.2 的取舍一致，不为验收装包）。
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

# dbq 在 Web 数据库上跑一条只读查询（先 checkpoint，避免读到旧 WAL 快照）。
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

# cq <容器 ID> <点分路径>：从 $WORK/containers.json 里取某个容器的字段。
# 布尔值打成 True/False，列表打成长度，找不到容器打 missing。
cq() {
  python3 - "$WORK/containers.json" "$1" "$2" <<'PY'
import json, sys
d = json.load(open(sys.argv[1], encoding="utf-8"))
cid, path = sys.argv[2], sys.argv[3]
for c in d["data"]["containers"]:
    if c["id"] != cid:
        continue
    cur = c
    try:
        for part in path.split("."):
            cur = cur[int(part)] if part.isdigit() else cur[part]
    except Exception:
        print("missing")
        raise SystemExit
    if isinstance(cur, bool):
        print("True" if cur else "False")
    elif isinstance(cur, list):
        print(len(cur))
    elif cur is None:
        print("None")
    else:
        print(cur)
    raise SystemExit
print("missing")
PY
}

# reason <容器 ID> <原因码>：这个容器有没有这条风险原因。
reason() {
  python3 - "$WORK/containers.json" "$1" "$2" <<'PY'
import json, sys
d = json.load(open(sys.argv[1], encoding="utf-8"))
cid, code = sys.argv[2], sys.argv[3]
for c in d["data"]["containers"]:
    if c["id"] == cid:
        print("True" if any(r["code"] == code for r in c["risk"]["reasons"]) else "False")
        raise SystemExit
print("missing")
PY
}

# tskey <RFC3339 文本>：把时间戳变成可比的 (UTC 秒, 纳秒)。
# 不能直接比字符串：RFC3339Nano 会去掉小数尾部的零，".5Z" 逐字符比 ".55Z" 更大，
# 数值上却更小；验收里一旦按字符串判“从新到旧”，就会偶发假失败。
TSKEY_PY='
import datetime, re
def key(s):
    m = re.match(r"^(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2})(?:\.(\d+))?(Z|[+-]\d{2}:\d{2})?$", s.strip())
    if not m:
        return None
    base = datetime.datetime.fromisoformat(m.group(1))
    nanos = int((m.group(2) or "0")[:9].ljust(9, "0"))
    off = m.group(3) or "Z"
    if off != "Z":
        sign = 1 if off[0] == "+" else -1
        base -= sign * datetime.timedelta(hours=int(off[1:3]), minutes=int(off[4:6]))
    return (base, nanos)
'

# logstat <文件> <名字>：打印日志页的派生事实（条数/游标/是否从新到旧）。
logstat() {
  python3 - "$1" "$2" <<PY
import json, sys
$TSKEY_PY
d = json.load(open(sys.argv[1], encoding="utf-8"))["data"]
name = sys.argv[2]
entries = d.get("entries") or []
if name == "count":
    print(len(entries))
elif name == "cursor":
    print(d.get("next_cursor") or "")
elif name == "descending":
    ks = [key(e["timestamp"]) for e in entries]
    ok = len(ks) > 1 and all(k is not None for k in ks) and all(ks[i] > ks[i + 1] for i in range(len(ks) - 1))
    print("True" if ok else "False")
elif name == "lines":
    print(d.get("lines", 0))
elif name == "merged":
    print("True" if d.get("streams_merged") else "False")
else:
    print("ERR:unknown")
PY
}

# all_older <文件> <游标>：这一页每条都严格早于游标（边界不重复）。
all_older() {
  python3 - "$1" "$2" <<PY
import json, sys
$TSKEY_PY
d = json.load(open(sys.argv[1], encoding="utf-8"))["data"]
c = key(sys.argv[2])
entries = d.get("entries") or []
ks = [key(e["timestamp"]) for e in entries]
ok = c is not None and bool(entries) and all(k is not None and k < c for k in ks)
print("True" if ok else "False")
PY
}

# redacted_ok <文件> <原文秘密>：这页里有条目被脱敏，且原文里找不到秘密。
redacted_ok() {
  python3 - "$1" "$2" <<'PY'
import json, sys
raw = open(sys.argv[1], encoding="utf-8").read()
d = json.loads(raw)["data"]
secret = sys.argv[2]
hit = [e for e in (d.get("entries") or []) if e.get("redacted") and "[redacted]" in e["message"]]
ok = bool(hit) and secret not in raw
print("True" if ok else "False")
PY
}

cleanup() {
  for _pid in "${WEB_PID:-}" "${AGENT_PID:-}"; do
    if [ -n "$_pid" ] && kill -0 "$_pid" 2>/dev/null; then
      kill -TERM "$_pid" 2>/dev/null
      wait "$_pid" 2>/dev/null
    fi
  done
  # 容器/网络/卷都是脚本自己造的夹具，收尾时删掉：不留在机器上。
  if [ "$HAS_DOCKER" -eq 1 ] && command -v docker >/dev/null 2>&1; then
    docker rm -f "$NAME_OK" "$NAME_STOPPED" "$NAME_PRIV" "$NAME_HOSTNET" "$NAME_SOCK" \
      "$NAME_CAP" "$NAME_LOG" >/dev/null 2>&1 || true
    docker network rm "$NET" >/dev/null 2>&1 || true
    docker volume rm "$VOL" >/dev/null 2>&1 || true
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
    if curl -fsS "$BASE/api/v1/health/live" >/dev/null 2>&1; then WEB_RUNNING=1; return 0; fi
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

# api <jar> <csrf> <方法> <路径> [body] [idempotency-key]：响应体落在 $WORK/resp.json。
api() {
  _jar=$1; _csrf=$2; _method=$3; _path=$4; _body=${5:-}; _key=${6:-}
  if [ -n "$_body" ]; then
    printf '%s' "$_body" >"$WORK/req.json"
    if [ -n "$_key" ]; then
      curl -s -b "$_jar" -o "$WORK/resp.json" -w '%{http_code}' -X "$_method" "$BASE$_path" \
        -H 'Content-Type: application/json' -H "Origin: $ORIGIN" -H "X-CSRF-Token: $_csrf" \
        -H "Idempotency-Key: $_key" -d @"$WORK/req.json"
    else
      curl -s -b "$_jar" -o "$WORK/resp.json" -w '%{http_code}' -X "$_method" "$BASE$_path" \
        -H 'Content-Type: application/json' -H "Origin: $ORIGIN" -H "X-CSRF-Token: $_csrf" \
        -d @"$WORK/req.json"
    fi
  else
    curl -s -b "$_jar" -o "$WORK/resp.json" -w '%{http_code}' -X "$_method" "$BASE$_path" \
      -H "Origin: $ORIGIN" -H "X-CSRF-Token: $_csrf"
  fi
}

api_anon() {
  curl -s -o "$WORK/resp.json" -w '%{http_code}' -X "$1" "$BASE$2" -H "Origin: $ORIGIN"
}

ecode() { jget "$WORK/resp.json" 'd["error"]["code"]'; }
emsg() { jget "$WORK/resp.json" 'd["error"]["message"]'; }
dget() { jget "$WORK/resp.json" "d[\"data\"]$1"; }

# get_containers：读一次容器清单，落在 $WORK/containers.json（载荷留档）。
get_containers() {
  _code=$(api "$JAR" "$CSRF" GET "$CONTAINERS")
  cp "$WORK/resp.json" "$PAYLOADS/containers.json" 2>/dev/null
  cp "$WORK/resp.json" "$WORK/containers.json" 2>/dev/null
  printf '%s' "$_code"
}

# preflight <载荷名> <body>：响应存成 preflight-<名>.json，回显 HTTP 状态码。
preflight() {
  _code=$(api "$JAR" "$CSRF" POST "$PREFLIGHT_PATH" "$2")
  cp "$WORK/resp.json" "$PAYLOADS/preflight-$1.json" 2>/dev/null
  printf '%s' "$_code"
}

# submit_job <载荷名> <preflight_id>：响应存成 job-submit-<名>.json，回显 HTTP 状态码。
submit_job() {
  _code=$(api "$JAR" "$CSRF" POST "$JOBS_PATH" "{\"preflight_id\":\"$2\"}" "p09-3-$1-$$")
  cp "$WORK/resp.json" "$PAYLOADS/job-submit-$1.json" 2>/dev/null
  printf '%s' "$_code"
}

# wait_terminal_job <job_id> <超时秒>：轮询到终态，回显 status。
wait_terminal_job() {
  _jid=$1; _limit=$(( ${2:-40} * 2 )); _i=0; _st=unknown
  while [ "$_i" -lt "$_limit" ]; do
    api "$JAR" "$CSRF" GET "$JOBS_PATH/$_jid" >/dev/null
    _st=$(jget "$WORK/resp.json" 'd["data"]["status"]')
    case $_st in
      succeeded|failed|canceled|interrupted) printf '%s' "$_st"; return 0 ;;
    esac
    _i=$((_i + 1))
    sleep 0.5
  done
  printf '%s' "${_st:-unknown}"
  return 1
}

# run_control <载荷名> <容器 ID> <操作>：预检 → 提交 → 等到终态，回显任务状态。
# 任何一步 HTTP 不对，回显 http-<码> 并返回 1（调用处的 expect_eq 会把它标成失败）。
run_control() {
  _body=$(printf '{"action":"container.control","params":{"container_id":"%s","operation":"%s"}}' "$2" "$3")
  _code=$(preflight "$1" "$_body")
  [ "$_code" = 200 ] || { printf 'http-%s' "$_code"; return 1; }
  _pf=$(jget "$PAYLOADS/preflight-$1.json" 'd["data"]["preflight_id"]')
  _code=$(submit_job "$1" "$_pf")
  [ "$_code" = 202 ] || { printf 'http-%s' "$_code"; return 1; }
  _jid=$(jget "$PAYLOADS/job-submit-$1.json" 'd["data"]["job_id"]')
  _st=$(wait_terminal_job "$_jid" 40)
  api "$JAR" "$CSRF" GET "$JOBS_PATH/$_jid" >/dev/null
  cp "$WORK/resp.json" "$PAYLOADS/job-$1.json" 2>/dev/null
  printf '%s' "$_st"
}

# live_running <容器名>：用 docker 现场读“在不在跑”（唯一的真值来源）。
live_running() { docker inspect -f '{{.State.Running}}' "$1" 2>/dev/null || printf 'unknown'; }
live_state() { docker inspect -f '{{.State.Status}}' "$1" 2>/dev/null || printf 'unknown'; }
live_id() { docker inspect -f '{{.Id}}' "$1" 2>/dev/null || printf ''; }

echo "== 0. 环境与安全守卫 =="
. /etc/os-release
note "系统：$PRETTY_NAME"
expect_eq "以 root 运行" "0" "$(id -u)"
for _b in "$AGENT" "$WEB" "$CTL"; do
  if [ -x "$_b" ]; then pass "二进制存在且可执行 $(basename "$_b")"; else
    fail "二进制存在且可执行 $(basename "$_b")" "$_b"; exit 2; fi
done
if id "$WEB_USER" >/dev/null 2>&1; then pass "Web 运行身份存在（$WEB_USER）"; else
  fail "Web 运行身份存在" "没有 $WEB_USER 账号"; exit 2; fi
expect_not_contains "Web 身份没有登录 shell" "$(getent passwd "$WEB_USER" | cut -d: -f7)" "/bin/bash"
if command -v python3 >/dev/null 2>&1; then pass "python3 可用（解析 JSON 与读库）"; else
  fail "python3 可用" "没有 python3，脚本无法解析载荷"; exit 2; fi
if ss -lntH "sport = :$PORT" 2>/dev/null | grep -q .; then
  fail "端口 $PORT 空闲" "已被占用"; exit 2
fi
pass "端口 $PORT 空闲"

HAS_DOCKER=0
if command -v docker >/dev/null 2>&1; then
  HAS_DOCKER=1
  note "本机有 docker：$(docker --version 2>/dev/null | head -1)"
else
  note "本机没有 docker：容器相关的全部验收将标记为 NOT RUN"
fi

# Web 账号不该有任何碰 docker 的路子：不在 docker 组，且连不上 socket。
if [ "$HAS_DOCKER" -eq 1 ] && [ -S /var/run/docker.sock ]; then
  DOCKER_READY=1
fi
if [ "$DOCKER_READY" -eq 1 ]; then
  if docker info >/dev/null 2>&1; then
    pass "docker 守护进程可用（$(docker info --format '{{.ServerVersion}}' 2>/dev/null)）"
  else
    fail "docker 守护进程可用" "docker info 失败"
  fi
  if id -nG "$WEB_USER" 2>/dev/null | tr ' ' '\n' | grep -qx docker; then
    fail "Web 账号不在 docker 组" "$WEB_USER 竟然是 docker 组成员"
  else
    pass "Web 账号不在 docker 组（$WEB_USER）"
  fi
  SOCK_MODE=$(stat -c '%U:%G %a' /var/run/docker.sock 2>/dev/null)
  note "docker.sock 属主与权限：$SOCK_MODE"
else
  notrun "Web 账号不在 docker 组" "本机没有可用的 docker"
fi

echo
echo "== 1. 配置与启动（代理 root、Web 非 root） =="
install -d -m 0755 /etc/upanel
install -d -m 0700 "$AGENT_DB_DIR"
install -d -m 0755 "$RUN_DIR"
rm -f "$AGENT_DB" "$AGENT_DB-wal" "$AGENT_DB-shm"
rm -rf "$WORK"
mkdir -p "$WORK/logs" "$PAYLOADS" "$WEB_DB_DIR"
rm -f "$WEB_DB" "$WEB_DB-wal" "$WEB_DB-shm"
chown -R "$WEB_USER" "$WEB_DB_DIR"
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
expect_eq "数据库迁移到第 8 版（0008_app_health）" "8" "$(dbq 'SELECT MAX(version) FROM schema_migrations')"

JAR=$WORK/admin.jar
expect_eq "管理员登录" "200" "$(login admin "$PASSWORD" "$JAR" login-admin)"
CSRF=$(csrf_of "$WORK/login-admin.json")
expect_ne "拿到 CSRF 令牌" "" "$CSRF"

echo
echo "== 2. Docker 夹具（普通容器与四类危险容器） =="
if [ "$DOCKER_READY" -eq 1 ]; then
  if ! docker image inspect "$CNT_IMAGE" >/dev/null 2>&1; then
    note "本机没有 $CNT_IMAGE，尝试拉取（需要外网；失败则后面标 NOT RUN）"
    docker pull "$CNT_IMAGE" >"$WORK/logs/docker-pull.log" 2>&1 || true
  fi
  if docker image inspect "$CNT_IMAGE" >/dev/null 2>&1; then
    docker rm -f "$NAME_OK" "$NAME_STOPPED" "$NAME_PRIV" "$NAME_HOSTNET" "$NAME_SOCK" \
      "$NAME_CAP" "$NAME_LOG" >/dev/null 2>&1 || true
    docker network rm "$NET" >/dev/null 2>&1 || true
    docker volume rm "$VOL" >/dev/null 2>&1 || true
    docker network create "$NET" >"$WORK/logs/docker-net.log" 2>&1 || true
    docker volume create "$VOL" >"$WORK/logs/docker-vol.log" 2>&1 || true

    # 普通容器：跑一个每秒写一行的循环，带 compose 标签、命名卷与一个**唯一口令**环境变量。
    docker run -d --name "$NAME_OK" --network "$NET" -v "$VOL":/data \
      -e "APP_DB_PASSWORD=$SECRET_ENV" \
      -l "com.docker.compose.project=$COMPOSE_PROJECT" \
      -l "com.docker.compose.service=api" \
      -l "com.docker.compose.container-number=1" \
      -l "com.docker.compose.project.working_dir=/srv/$COMPOSE_PROJECT" \
      -l "com.docker.compose.project.config_files=/srv/$COMPOSE_PROJECT/compose.yaml" \
      "$CNT_IMAGE" sh -c 'i=0; while true; do echo "line-$i"; i=$((i+1)); sleep 1; done' \
      >"$WORK/logs/docker-run-ok.log" 2>&1 || true

    # 已停止的容器（created → 面板的 stopped）：用来验 start。
    docker create --name "$NAME_STOPPED" "$CNT_IMAGE" sh -c 'sleep 30' \
      >"$WORK/logs/docker-create-stopped.log" 2>&1 || true

    # 日志夹具：写一行带口令的日志，然后静默——用来验脱敏（这行是最老的，tail 小也能取到）。
    docker run -d --name "$NAME_LOG" "$CNT_IMAGE" \
      sh -c "echo 'boot db password=$SECRET_LOG done'; sleep 3600" \
      >"$WORK/logs/docker-run-log.log" 2>&1 || true

    # 四类危险容器：privileged、host 网络、挂 docker.sock、危险 capability。
    docker run -d --name "$NAME_PRIV" --privileged "$CNT_IMAGE" sh -c 'sleep 3600' \
      >"$WORK/logs/docker-run-priv.log" 2>&1 || true
    docker run -d --name "$NAME_HOSTNET" --network host "$CNT_IMAGE" sh -c 'sleep 3600' \
      >"$WORK/logs/docker-run-hostnet.log" 2>&1 || true
    docker run -d --name "$NAME_SOCK" -v /var/run/docker.sock:/var/run/docker.sock \
      "$CNT_IMAGE" sh -c 'sleep 3600' >"$WORK/logs/docker-run-sock.log" 2>&1 || true
    docker run -d --name "$NAME_CAP" --cap-add SYS_ADMIN "$CNT_IMAGE" sh -c 'sleep 3600' \
      >"$WORK/logs/docker-run-cap.log" 2>&1 || true

    ID_OK=$(live_id "$NAME_OK")
    ID_STOPPED=$(live_id "$NAME_STOPPED")
    ID_PRIV=$(live_id "$NAME_PRIV")
    ID_HOSTNET=$(live_id "$NAME_HOSTNET")
    ID_SOCK=$(live_id "$NAME_SOCK")
    ID_CAP=$(live_id "$NAME_CAP")
    ID_LOG=$(live_id "$NAME_LOG")

    _bad=0
    for _id in "$ID_OK" "$ID_STOPPED" "$ID_PRIV" "$ID_HOSTNET" "$ID_SOCK" "$ID_CAP" "$ID_LOG"; do
      case "$_id" in
        *[!0-9a-f]*|"") _bad=1 ;;
      esac
      [ "${#_id}" -eq 64 ] || _bad=1
    done
    if [ "$_bad" -eq 0 ]; then
      pass "七个夹具容器都起来了（ID 全是 64 位小写十六进制）"
    else
      fail "七个夹具容器都起来了" "$(tail -2 "$WORK"/logs/docker-run-*.log 2>/dev/null | tr '\n' ' ')"
    fi
    expect_eq "普通容器在跑" "true" "$(live_running "$NAME_OK")"
    expect_eq "已停止夹具处于 created（面板读作 stopped）" "created" "$(live_state "$NAME_STOPPED")"

    # 等日志夹具写出那一行（否则后面读到空页会误判成“脱敏失败”）。
    _i=0
    while [ "$_i" -lt 50 ]; do
      _lines=$(docker logs "$NAME_LOG" 2>&1 | wc -l)
      [ "$_lines" -ge 1 ] && break
      _i=$((_i + 1)); sleep 0.1
    done
    expect_eq "日志夹具已经写出一行" "1" "$(docker logs "$NAME_LOG" 2>&1 | wc -l)"

    # 等普通容器积累几行日志。
    _i=0
    while [ "$_i" -lt 50 ]; do
      _lines=$(docker logs "$NAME_OK" 2>&1 | wc -l)
      [ "$_lines" -ge 3 ] && break
      _i=$((_i + 1)); sleep 0.2
    done
    if [ "$_lines" -ge 3 ]; then pass "普通容器已写出多行日志（$_lines 行）"; else
      fail "普通容器已写出多行日志" "只有 $_lines 行"; fi
  else
    note "拉不到 $CNT_IMAGE：容器相关验收全部标 NOT RUN"
  fi
fi

if [ "$HAS_DOCKER" -eq 1 ] && [ "$DOCKER_READY" -eq 1 ] && [ -n "${ID_OK:-}" ] && [ "${#ID_OK}" -eq 64 ]; then
  FIXTURES_OK=1
else
  FIXTURES_OK=0
fi

echo
echo "== 3. 容器清单：只读、字段与风险判决 =="
if [ "$FIXTURES_OK" -eq 1 ]; then
  expect_eq "未登录读容器清单" "401" "$(api_anon GET "$CONTAINERS")"
  # Web 账号连 docker.sock 必须失败（它不在 docker 组）：这里期望“非 0”退出码。
  expect_ne "Web 账号连不上 docker.sock（不在 docker 组）" "0" \
    "$(runuser -u "$WEB_USER" -- docker ps >/dev/null 2>&1; echo $?)"
  # Web 进程自己也不该持有到 docker.sock 的连接：它是 root 代理的活。
  if ss -xp 2>/dev/null | grep 'docker.sock' | grep -q "pid=$LISTEN_PID,"; then
    fail "Web 进程没有连 docker.sock" "pid=$LISTEN_PID 出现在 docker.sock 的连接里"
  else
    pass "Web 进程没有连 docker.sock（pid=$LISTEN_PID）"
  fi

  CODE=$(get_containers)
  expect_eq "admin 读容器清单" "200" "$CODE"
  expect_eq "清单 available" "True" "$(dget '["available"]')"
  expect_eq "清单里能找到普通容器" "True" \
    "$(jget "$WORK/containers.json" "any(c[\"id\"] == \"$ID_OK\" for c in d[\"data\"][\"containers\"])")"
  expect_eq "已停止的容器也在清单里（不只是 running）" "True" \
    "$(jget "$WORK/containers.json" "any(c[\"id\"] == \"$ID_STOPPED\" for c in d[\"data\"][\"containers\"])")"
  expect_eq "普通容器 state" "running" "$(cq "$ID_OK" state)"
  expect_eq "普通容器 state_text 非空" "True" "$([ -n "$(cq "$ID_OK" state_text)" ] && echo True || echo False)"
  expect_eq "普通容器 image 非空" "True" "$([ -n "$(cq "$ID_OK" image)" ] && echo True || echo False)"
  expect_eq "普通容器挂着一个数据卷" "1" "$(cq "$ID_OK" mounts)"
  expect_eq "普通容器的 compose 项目" "$COMPOSE_PROJECT" "$(cq "$ID_OK" compose.project)"
  expect_eq "普通容器的 compose 服务" "api" "$(cq "$ID_OK" compose.service)"
  expect_eq "普通容器 risk.level" "none" "$(cq "$ID_OK" risk.level)"
  expect_eq "普通容器 managed" "True" "$(cq "$ID_OK" capabilities.managed)"
  expect_eq "普通容器可 stop" "True" "$(cq "$ID_OK" capabilities.stop)"
  expect_eq "运行中容器不可 start（状态只能用 restart 改变）" "False" "$(cq "$ID_OK" capabilities.start)"
  expect_eq "已停止容器 state" "stopped" "$(cq "$ID_STOPPED" state)"
  expect_eq "已停止容器可 start" "True" "$(cq "$ID_STOPPED" capabilities.start)"
  expect_eq "已停止容器不可 stop" "False" "$(cq "$ID_STOPPED" capabilities.stop)"

  # 危险容器：risk.level=high、managed=false、三项能力全 false。
  for _pair in "priv:$ID_PRIV:privileged" "hostnet:$ID_HOSTNET:host_network" \
    "sock:$ID_SOCK:docker_sock_mount" "cap:$ID_CAP:dangerous_capability"; do
    _label=${_pair%%:*}; _rest=${_pair#*:}; _id=${_rest%%:*}; _code=${_rest#*:}
    expect_eq "$_label 容器 risk.level=high" "high" "$(cq "$_id" risk.level)"
    expect_eq "$_label 容器 managed=false" "False" "$(cq "$_id" capabilities.managed)"
    expect_eq "$_label 容器三项能力全 false" "FalseFalseFalse" \
      "$(cq "$_id" capabilities.start)$(cq "$_id" capabilities.stop)$(cq "$_id" capabilities.restart)"
    expect_eq "$_label 容器的原因码含 $_code" "True" "$(reason "$_id" "$_code")"
  done
  expect_eq "privileged 容器至少一条风险说明" "True" \
    "$(jget "$WORK/containers.json" "any(r[\"message\"] for c in d[\"data\"][\"containers\"] if c[\"id\"] == \"$ID_PRIV\" for r in c[\"risk\"][\"reasons\"])")"

  # 环境变量与完整标签从不外泄：这一条是“不请求就不存在泄漏”的落地检查。
  expect_not_contains "清单里没有容器环境变量的值" "$SECRET_ENV" "$(cat "$WORK/containers.json")"
  expect_not_contains "清单里没有环境变量名 APP_DB_PASSWORD" "APP_DB_PASSWORD" "$(cat "$WORK/containers.json")"
  expect_eq "普通容器的挂载里有命名卷 $VOL" "True" \
    "$(jget "$WORK/containers.json" "any(m[\"type\"] == \"volume\" and m[\"name\"] == \"$VOL\" for c in d[\"data\"][\"containers\"] if c[\"id\"] == \"$ID_OK\" for m in c[\"mounts\"])")"

  # 详情
  CODE=$(api "$JAR" "$CSRF" GET "$CONTAINERS/$ID_OK")
  cp "$WORK/resp.json" "$PAYLOADS/container-detail.json" 2>/dev/null
  expect_eq "容器详情" "200" "$CODE"
  expect_eq "详情 id 一致" "$ID_OK" "$(dget '["id"]')"
  expect_eq "详情 risk.level" "none" "$(dget '["risk"]["level"]')"
  expect_not_contains "详情里没有容器环境变量的值" "$SECRET_ENV" "$(cat "$WORK/resp.json")"
else
  notrun "容器清单与详情" "没有 docker 或夹具未就绪（见第 2 节）"
fi

echo
echo "== 4. 容器身份：只认完整 64 位 ID =="
if [ "$FIXTURES_OK" -eq 1 ]; then
  SHORT_ID=$(printf '%s' "$ID_OK" | cut -c1-12)
  for _bad in "$NAME_OK" "$SHORT_ID" "$(printf '%s' "$ID_OK" | tr 'a-f' 'A-F')" "${ID_OK}x"; do
    CODE=$(api "$JAR" "$CSRF" GET "$CONTAINERS/$_bad")
    # /bin/sh 没有 ${var:0:14}，截断交给 cut，脚本才能在 dash 下跑完。
    _brief=$(printf '%s' "$_bad" | cut -c1-14)
    expect_eq "非完整 ID 的详情请求被拒（$_brief…）" "400" "$CODE"
    expect_eq "…错误码是 VALIDATION_ERROR" "VALIDATION_ERROR" "$(ecode)"
  done
  CODE=$(api "$JAR" "$CSRF" GET "$CONTAINERS/0000000000000000000000000000000000000000000000000000000000000000")
  expect_eq "不存在的完整 ID → 404" "404" "$CODE"
  expect_eq "…错误码是 CONTAINER_NOT_FOUND" "CONTAINER_NOT_FOUND" "$(ecode)"
  CODE=$(api "$JAR" "$CSRF" GET "$CONTAINERS/$ID_OK/logs?tail=abc")
  expect_eq "tail 非整数 → 400" "400" "$CODE"
  CODE=$(api "$JAR" "$CSRF" GET "$CONTAINERS/$ID_OK/logs?tail=0")
  expect_eq "tail=0 → 400" "400" "$CODE"
  CODE=$(api "$JAR" "$CSRF" GET "$CONTAINERS/$ID_OK/logs?tail=1001")
  expect_eq "tail=1001 → 400" "400" "$CODE"
  expect_eq "注入式容器名没能执行任何东西（容器数量未变）" "0" \
    "$(docker ps -a --format '{{.Names}}' | grep -c 'upn-p093-.*;' || true)"
else
  notrun "容器身份与参数边界" "没有 docker 或夹具未就绪"
fi

echo
echo "== 5. 容器日志：从新到旧、游标往返、脱敏 =="
if [ "$FIXTURES_OK" -eq 1 ]; then
  CODE=$(api "$JAR" "$CSRF" GET "$CONTAINERS/$ID_OK/logs?tail=5")
  cp "$WORK/resp.json" "$WORK/page1.json"
  cp "$WORK/resp.json" "$PAYLOADS/logs-page1.json" 2>/dev/null
  expect_eq "读一页容器日志" "200" "$CODE"
  expect_eq "一页 5 条" "5" "$(logstat "$WORK/page1.json" count)"
  expect_eq "条目从新到旧" "True" "$(logstat "$WORK/page1.json" descending)"
  expect_eq "streams_merged=true（docker 不分 stdout/stderr）" "True" "$(logstat "$WORK/page1.json" merged)"
  CURSOR=$(logstat "$WORK/page1.json" cursor)
  expect_ne "给了 next_cursor（还能往前翻）" "" "$CURSOR"

  CODE=$(api "$JAR" "$CSRF" GET "$CONTAINERS/$ID_OK/logs?tail=5&cursor=$CURSOR")
  cp "$WORK/resp.json" "$WORK/page2.json"
  cp "$WORK/resp.json" "$PAYLOADS/logs-page2.json" 2>/dev/null
  expect_eq "带游标翻到下一页" "200" "$CODE"
  expect_eq "第二页每条都严格早于游标（边界不重复）" "True" "$(all_older "$WORK/page2.json" "$CURSOR")"
  expect_ne "第二页不是空的（游标真的往回走了）" "0" "$(logstat "$WORK/page2.json" count)"

  CODE=$(api "$JAR" "$CSRF" GET "$CONTAINERS/$ID_OK/logs?cursor=not-a-time")
  expect_eq "游标被篡改 → 400（不当成“没给”）" "400" "$CODE"
  CODE=$(api "$JAR" "$CSRF" GET "$CONTAINERS/$ID_OK/logs?since=yesterday")
  expect_eq "since 不是 RFC3339 → 400" "400" "$CODE"
  FUTURE=$(python3 -c 'import datetime; print((datetime.datetime.now(datetime.timezone.utc)+datetime.timedelta(hours=2)).strftime("%Y-%m-%dT%H:%M:%SZ"))')
  CODE=$(api "$JAR" "$CSRF" GET "$CONTAINERS/$ID_OK/logs?until=$FUTURE")
  expect_eq "until 在未来 → 400（未来窗口不该是“这段时间没日志”）" "400" "$CODE"
  CODE=$(api "$JAR" "$CSRF" GET "$CONTAINERS/0000000000000000000000000000000000000000000000000000000000000000/logs")
  expect_eq "不存在的容器的日志 → 404" "404" "$CODE"

  # 脱敏：容器里写的那行口令必须变成 [redacted]，原文不能出现在响应里。
  CODE=$(api "$JAR" "$CSRF" GET "$CONTAINERS/$ID_LOG/logs?tail=10")
  cp "$WORK/resp.json" "$WORK/page-log.json"
  cp "$WORK/resp.json" "$PAYLOADS/logs-redacted.json" 2>/dev/null
  expect_eq "读日志夹具的日志" "200" "$CODE"
  expect_eq "口令被脱敏成 [redacted] 且原文不在响应里" "True" \
    "$(redacted_ok "$WORK/page-log.json" "$SECRET_LOG")"
else
  notrun "容器日志分页与脱敏" "没有 docker 或夹具未就绪"
fi

echo
echo "== 6. 镜像 / 网络 / 数据卷 =="
if [ "$FIXTURES_OK" -eq 1 ]; then
  CODE=$(api "$JAR" "$CSRF" GET "$IMAGES")
  cp "$WORK/resp.json" "$PAYLOADS/images.json" 2>/dev/null
  expect_eq "镜像清单" "200" "$CODE"
  expect_eq "镜像 available" "True" "$(dget '["available"]')"
  expect_ne "镜像清单不是空的" "0" "$(jget "$WORK/resp.json" 'len(d["data"]["images"])')"
  expect_eq "镜像条目都带非空 ID" "True" \
    "$(jget "$WORK/resp.json" 'all(i["id"] for i in d["data"]["images"])')"

  CODE=$(api "$JAR" "$CSRF" GET "$NETWORKS")
  cp "$WORK/resp.json" "$PAYLOADS/networks.json" 2>/dev/null
  expect_eq "网络清单" "200" "$CODE"
  expect_eq "网络 available" "True" "$(dget '["available"]')"
  expect_eq "夹具网络在清单里" "True" \
    "$(jget "$WORK/resp.json" "any(n[\"name\"] == \"$NET\" for n in d[\"data\"][\"networks\"])")"
  expect_eq "夹具网络带子网" "True" \
    "$(jget "$WORK/resp.json" "any(n[\"subnets\"] for n in d[\"data\"][\"networks\"] if n[\"name\"] == \"$NET\")")"
  expect_eq "bridge 网络存在" "True" \
    "$(jget "$WORK/resp.json" 'any(n["name"] == "bridge" for n in d["data"]["networks"])')"

  CODE=$(api "$JAR" "$CSRF" GET "$VOLUMES")
  cp "$WORK/resp.json" "$PAYLOADS/volumes.json" 2>/dev/null
  expect_eq "数据卷清单" "200" "$CODE"
  expect_eq "数据卷 available" "True" "$(dget '["available"]')"
  expect_eq "夹具卷在清单里" "True" \
    "$(jget "$WORK/resp.json" "any(v[\"name\"] == \"$VOL\" for v in d[\"data\"][\"volumes\"])")"
  expect_eq "夹具卷标出被哪个容器占用" "True" \
    "$(jget "$WORK/resp.json" "any(\"$NAME_OK\" in v[\"in_use_by\"] for v in d[\"data\"][\"volumes\"] if v[\"name\"] == \"$VOL\")")"
else
  notrun "镜像 / 网络 / 数据卷" "没有 docker 或夹具未就绪"
fi

echo
echo "== 7. 部署（compose 项目只读视图） =="
if [ "$FIXTURES_OK" -eq 1 ]; then
  CODE=$(api "$JAR" "$CSRF" GET "$DEPLOYMENTS")
  cp "$WORK/resp.json" "$WORK/deployments.json"
  cp "$WORK/resp.json" "$PAYLOADS/deployments.json" 2>/dev/null
  expect_eq "部署清单" "200" "$CODE"
  expect_eq "部署 available" "True" "$(dget '["available"]')"
  DEP_ID=$(jget "$WORK/deployments.json" "next((p[\"id\"] for p in d[\"data\"][\"projects\"] if p[\"name\"] == \"$COMPOSE_PROJECT\"), \"\")")
  expect_ne "按 compose 标签归并出项目 $COMPOSE_PROJECT" "" "$DEP_ID"
  case "$DEP_ID" in
    dep_*) pass "项目标识是派生的 dep_ 前缀（不带项目名原文）" ;;
    *) fail "项目标识是派生的 dep_ 前缀" "实际 '$DEP_ID'" ;;
  esac
  expect_eq "项目 $COMPOSE_PROJECT 三项能力全 false" "FalseFalseFalse" \
    "$(jget "$WORK/deployments.json" "next((str(p[\"capabilities\"][\"apply\"])+str(p[\"capabilities\"][\"update\"])+str(p[\"capabilities\"][\"remove\"]) for p in d[\"data\"][\"projects\"] if p[\"name\"] == \"$COMPOSE_PROJECT\"), \"missing\")")"
  expect_eq "项目里有 api 服务" "True" \
    "$(jget "$WORK/deployments.json" "next((str(\"api\" in p[\"services\"]) for p in d[\"data\"][\"projects\"] if p[\"name\"] == \"$COMPOSE_PROJECT\"), \"missing\")")"
  expect_eq "项目的容器数是 1" "1" \
    "$(jget "$WORK/deployments.json" "next((p[\"total\"] for p in d[\"data\"][\"projects\"] if p[\"name\"] == \"$COMPOSE_PROJECT\"), -1)")"

  CODE=$(api "$JAR" "$CSRF" GET "$DEPLOYMENTS/$DEP_ID")
  cp "$WORK/resp.json" "$PAYLOADS/deployment-detail.json" 2>/dev/null
  expect_eq "部署详情" "200" "$CODE"
  expect_eq "详情里带着项目的容器" "True" \
    "$(jget "$WORK/resp.json" "any(c[\"id\"] == \"$ID_OK\" for c in d[\"data\"][\"containers\"])")"
  expect_eq "详情 capabilities.update=false" "False" "$(dget '["capabilities"]["update"]')"
  CODE=$(api "$JAR" "$CSRF" GET "$DEPLOYMENTS/dep_00000000000000000000000000000000")
  expect_eq "不存在的部署 → 404" "404" "$CODE"
  CODE=$(api "$JAR" "$CSRF" GET "$DEPLOYMENTS/$COMPOSE_PROJECT")
  expect_eq "按项目名查部署 → 404（不模糊匹配）" "404" "$CODE"
else
  notrun "部署（compose 项目）" "没有 docker 或夹具未就绪"
fi

echo
echo "== 8. 启停：预检 → 任务 → 读回验证 =="
if [ "$FIXTURES_OK" -eq 1 ]; then
  # 8.1 stop：普通容器。预检允许、目标身份只由 ID 决定、compose 成员带告警。
  CODE=$(preflight stop-ok "{\"action\":\"container.control\",\"params\":{\"container_id\":\"$ID_OK\",\"operation\":\"stop\"}}")
  expect_eq "stop 预检" "200" "$CODE"
  expect_eq "stop 预检 allowed" "True" "$(jget "$PAYLOADS/preflight-stop-ok.json" 'd["data"]["allowed"]')"
  expect_eq "目标身份指纹只由容器 ID 决定" "container:$ID_OK" \
    "$(jget "$PAYLOADS/preflight-stop-ok.json" 'd["data"]["target"]["identity_fingerprint"]')"
  expect_eq "预检带影响说明" "True" \
    "$(jget "$PAYLOADS/preflight-stop-ok.json" 'bool(d["data"]["impacts"])')"
  expect_eq "compose 成员带 COMPOSE_MEMBER 告警" "COMPOSE_MEMBER" \
    "$(jget "$PAYLOADS/preflight-stop-ok.json" 'd["data"]["warnings"][0]["code"]')"
  expect_eq "启停不要求再认证" "False" "$(jget "$PAYLOADS/preflight-stop-ok.json" 'd["data"]["requires_reauth"]')"
  expect_eq "启停不要求确认短语" "None" "$(jget "$PAYLOADS/preflight-stop-ok.json" 'd["data"]["confirmation_phrase"]')"
  expect_eq "锁是容器自己" "container:$ID_OK" "$(jget "$PAYLOADS/preflight-stop-ok.json" 'd["data"]["locks"][0]')"
  expect_eq "规范化参数只有 ID 与操作" "True" \
    "$(jget "$PAYLOADS/preflight-stop-ok.json" 'sorted(d["data"]["normalized_params"].keys()) == ["container_id", "operation"]')"

  PF=$(jget "$PAYLOADS/preflight-stop-ok.json" 'd["data"]["preflight_id"]')
  expect_eq "提交 stop 任务" "202" "$(submit_job stop-ok "$PF")"
  JID=$(jget "$PAYLOADS/job-submit-stop-ok.json" 'd["data"]["job_id"]')
  expect_eq "stop 任务终态" "succeeded" "$(wait_terminal_job "$JID" 40)"
  api "$JAR" "$CSRF" GET "$JOBS_PATH/$JID" >/dev/null
  cp "$WORK/resp.json" "$PAYLOADS/job-stop-ok.json" 2>/dev/null
  expect_eq "stop 结果 outcome" "verified" "$(jget "$PAYLOADS/job-stop-ok.json" 'd["data"]["result"]["outcome"]')"
  expect_eq "stop 的 observed_state.verified" "True" \
    "$(jget "$PAYLOADS/job-stop-ok.json" 'd["data"]["result"]["observed_state"]["verified"]')"
  expect_eq "stop 读回状态是 stopped" "stopped" \
    "$(jget "$PAYLOADS/job-stop-ok.json" 'd["data"]["result"]["observed_state"]["state"]')"
  expect_eq "stop 记录了操作前状态" "running" \
    "$(jget "$PAYLOADS/job-stop-ok.json" 'd["data"]["result"]["observed_state"]["previous_state"]')"
  expect_eq "docker 现场见证（已停止）" "false" "$(live_running "$NAME_OK")"

  # 8.2 start：把同一个容器再拉起来。
  expect_eq "start 任务终态" "succeeded" "$(run_control start-ok "$ID_OK" start)"
  expect_eq "start 读回状态是 running" "running" \
    "$(jget "$PAYLOADS/job-start-ok.json" 'd["data"]["result"]["observed_state"]["state"]')"
  expect_eq "docker 现场见证（在跑）" "true" "$(live_running "$NAME_OK")"

  # 8.3 stop 之后 start 已停止夹具（created 状态）。
  expect_eq "对已停止容器 start 的终态" "succeeded" "$(run_control start-stopped "$ID_STOPPED" start)"
  expect_eq "已停止夹具被真的拉起来了" "true" "$(live_running "$NAME_STOPPED")"

  # 8.4 restart：状态不变的写操作，也要读回验证。
  expect_eq "restart 任务终态" "succeeded" "$(run_control restart-ok "$ID_OK" restart)"
  expect_eq "restart 读回状态是 running" "running" \
    "$(jget "$PAYLOADS/job-restart-ok.json" 'd["data"]["result"]["observed_state"]["state"]')"
  expect_eq "docker 现场见证（restart 后仍在跑）" "true" "$(live_running "$NAME_OK")"

  echo
  echo "== 9. 拒绝：危险容器、状态冲突、参数注入、陈旧预检 =="
  # 9.1 危险容器：四类都不接管，且提交被拒后状态不变。
  for _pair in "priv:$ID_PRIV" "hostnet:$ID_HOSTNET" "sock:$ID_SOCK" "cap:$ID_CAP"; do
    _label=${_pair%%:*}; _id=${_pair#*:}
    CODE=$(preflight "stop-$_label" "{\"action\":\"container.control\",\"params\":{\"container_id\":\"$_id\",\"operation\":\"stop\"}}")
    expect_eq "$_label 容器 stop 预检被拒（HTTP 200 + allowed=false）" "200" "$CODE"
    expect_eq "$_label 容器 allowed" "False" "$(jget "$PAYLOADS/preflight-stop-$_label.json" 'd["data"]["allowed"]')"
    expect_eq "$_label 容器阻塞码含 CONTAINER_NOT_MANAGED" "True" \
      "$(jget "$PAYLOADS/preflight-stop-$_label.json" 'any(b["code"] == "CONTAINER_NOT_MANAGED" for b in d["data"]["blockers"])')"
  done
  PF=$(jget "$PAYLOADS/preflight-stop-priv.json" 'd["data"]["preflight_id"]')
  expect_eq "提交被拒的预检 → 409" "409" "$(submit_job stop-priv "$PF")"
  expect_eq "…错误码 DEPENDENCY_BLOCKED（Web 在落库时就判掉，不到代理）" "DEPENDENCY_BLOCKED" "$(ecode)"
  expect_eq "privileged 容器没有被停掉" "true" "$(live_running "$NAME_PRIV")"

  # 9.2 状态冲突：对 running 容器 start。
  CODE=$(preflight start-running "{\"action\":\"container.control\",\"params\":{\"container_id\":\"$ID_OK\",\"operation\":\"start\"}}")
  expect_eq "对 running 容器 start 的预检" "200" "$CODE"
  expect_eq "…allowed=false" "False" "$(jget "$PAYLOADS/preflight-start-running.json" 'd["data"]["allowed"]')"
  expect_eq "…阻塞码是 CONTAINER_STATE_CONFLICT" "CONTAINER_STATE_CONFLICT" \
    "$(jget "$PAYLOADS/preflight-start-running.json" 'd["data"]["blockers"][0]["code"]')"

  # 9.3 参数注入：名字、闭集外的操作、多余字段都不行。
  CODE=$(preflight inject-name "{\"action\":\"container.control\",\"params\":{\"container_id\":\"$NAME_OK\",\"operation\":\"stop\"}}")
  expect_eq "用容器名预检 → 400" "400" "$CODE"
  expect_eq "…错误码 VALIDATION_ERROR" "VALIDATION_ERROR" "$(ecode)"
  CODE=$(preflight inject-op "{\"action\":\"container.control\",\"params\":{\"container_id\":\"$ID_OK\",\"operation\":\"rm -f\"}}")
  expect_eq "闭集外的操作 → 400" "400" "$CODE"
  CODE=$(preflight inject-extra "{\"action\":\"container.control\",\"params\":{\"container_id\":\"$ID_OK\",\"operation\":\"stop\",\"args\":[\"-f\"]}}")
  expect_eq "多余参数（args）→ 400" "400" "$CODE"
  CODE=$(preflight inject-flag "{\"action\":\"container.control\",\"params\":{\"container_id\":\"--privileged\",\"operation\":\"stop\"}}")
  expect_eq "把 flag 当容器 ID → 400" "400" "$CODE"
  expect_eq "注入尝试之后普通容器还在跑" "true" "$(live_running "$NAME_OK")"

  # 9.4 不存在的容器：预检是 allowed=false（不是 500）。
  CODE=$(preflight stop-missing "{\"action\":\"container.control\",\"params\":{\"container_id\":\"0000000000000000000000000000000000000000000000000000000000000000\",\"operation\":\"stop\"}}")
  expect_eq "不存在容器的预检" "200" "$CODE"
  expect_eq "…allowed=false" "False" "$(jget "$PAYLOADS/preflight-stop-missing.json" 'd["data"]["allowed"]')"
  expect_eq "…阻塞码 CONTAINER_NOT_FOUND" "CONTAINER_NOT_FOUND" \
    "$(jget "$PAYLOADS/preflight-stop-missing.json" 'd["data"]["blockers"][0]["code"]')"

  # 9.5 陈旧预检：预检之后在外部把容器重启，旧预检必须失效。
  CODE=$(preflight stop-stale "{\"action\":\"container.control\",\"params\":{\"container_id\":\"$ID_OK\",\"operation\":\"stop\"}}")
  expect_eq "陈旧预检用例的预检" "200" "$CODE"
  expect_eq "…此时 allowed" "True" "$(jget "$PAYLOADS/preflight-stop-stale.json" 'd["data"]["allowed"]')"
  docker restart "$NAME_OK" >/dev/null 2>&1
  _i=0
  while [ "$_i" -lt 40 ]; do
    [ "$(live_running "$NAME_OK")" = "true" ] && break
    _i=$((_i + 1)); sleep 0.25
  done
  PF=$(jget "$PAYLOADS/preflight-stop-stale.json" 'd["data"]["preflight_id"]')
  # 状态在预检之后变了：这道门在**受理**时就复核（CONTRACTS §5「提交任务时重算，
  # 不一致是 PREFLIGHT_STALE」），所以结局是 409，任务根本进不了执行队列。
  JOBS_BEFORE=$(dbq "SELECT COUNT(*) FROM jobs")
  expect_eq "提交陈旧预检 → 409（受理时复核就挡下）" "409" "$(submit_job stop-stale "$PF")"
  expect_eq "…错误码 PREFLIGHT_STALE" "PREFLIGHT_STALE" "$(ecode)"
  # 拒绝也要留痕：作业表里多一条 failed，错误码写在结果里（Q159：按内容找，不按位置）。
  JOBS_AFTER=$(dbq "SELECT COUNT(*) FROM jobs")
  expect_eq "…作业表多一条（拒绝也留痕）" "$((JOBS_BEFORE + 1))" "$JOBS_AFTER"
  STALE_JOBS=$(dbq "SELECT COUNT(*) FROM jobs WHERE status = 'failed' AND error_json LIKE '%PREFLIGHT_STALE%'")
  if [ "${STALE_JOBS:-0}" -gt 0 ] 2>/dev/null; then
    pass "…failed 作业里写着 PREFLIGHT_STALE（$STALE_JOBS 条）"
  else
    fail "…failed 作业里写着 PREFLIGHT_STALE" "实际 $STALE_JOBS 条"
  fi
  expect_eq "容器没有被这个旧预检停掉" "true" "$(live_running "$NAME_OK")"
  # 第二道门（受理之后、真正执行 docker 之前再复核）没法在真机上稳定构造：
  # 要在 202 与执行之间改容器状态，得靠可重复的竞态。由单测覆盖，这里如实标 NOT RUN。
  notrun "执行前复核（受理之后再变）" \
    "受理时的复核已经把变化挡在 202 之前；要在受理与执行之间改状态需要可重复的竞态，真机构造不出来，由 internal/agent/containers_test.go 的 TestContainerControlExecuteStaleState 覆盖"

  # 重新预检一次就恢复正常：陈旧预检不是“把容器锁死了”。
  expect_eq "重新预检后 stop 终态" "succeeded" "$(run_control stop-ok2 "$ID_OK" stop)"
  expect_eq "docker 现场见证（第二次已停止）" "false" "$(live_running "$NAME_OK")"
  expect_eq "恢复运行（供日志流使用）" "succeeded" "$(run_control start-ok2 "$ID_OK" start)"

  echo
  echo "== 10. 容器日志实时流（SSE） =="
  # 5 秒是紧的：开流前要先确认容器存在（一次 inspect），代理侧还要申请许可再开流，
  # 真机上这一段就花掉三四秒，留给“跟随”的时间不够夹具写出一行。20 秒是给跟随
  # 留出的窗口，断言本身不变（必须收到 entry 事件）。
  curl -sN -b "$JAR" --max-time 20 -o "$WORK/stream.sse" -w '%{http_code}' \
    "$BASE/api/v1/containers/$ID_OK/logs/stream?tail=5" >"$WORK/stream.code" 2>/dev/null || true
  STREAM_CODE=$(cat "$WORK/stream.code" 2>/dev/null)
  expect_eq "开流状态码" "200" "$STREAM_CODE"
  expect_contains "流里收到了 entry 事件" "event: entry" "$(cat "$WORK/stream.sse")"
  expect_contains "entry 事件带时间戳 id（供 Last-Event-ID 用）" "id: 20" "$(cat "$WORK/stream.sse")"
  cp "$WORK/stream.sse" "$PAYLOADS/stream.sse" 2>/dev/null

  curl -s -b "$JAR" --max-time 5 -o "$WORK/stream404.json" -w '%{http_code}' \
    "$BASE/api/v1/containers/0000000000000000000000000000000000000000000000000000000000000000/logs/stream" \
    >"$WORK/stream404.code" 2>/dev/null || true
  expect_eq "不存在的容器开流 → 404（不是 200 + error 帧）" "404" "$(cat "$WORK/stream404.code")"
  expect_eq "…错误码 CONTAINER_NOT_FOUND" "CONTAINER_NOT_FOUND" \
    "$(jget "$WORK/stream404.json" 'd["error"]["code"]')"
else
  notrun "启停、拒绝与日志流" "没有 docker 或夹具未就绪"
fi

echo
echo "== 11. 两种“不可用”必须分开 =="
if [ "$FIXTURES_OK" -eq 1 ]; then
  # 11.1 代理停掉：AGENT_UNAVAILABLE（等一会儿会好）。
  stop_agent
  CODE=$(api "$JAR" "$CSRF" GET "$CONTAINERS")
  cp "$WORK/resp.json" "$PAYLOADS/containers-agentdown.json" 2>/dev/null
  expect_eq "代理停掉时读容器清单 → 503" "503" "$CODE"
  expect_eq "…错误码是 AGENT_UNAVAILABLE" "AGENT_UNAVAILABLE" "$(ecode)"
  if start_agent; then pass "代理重新启动"; else fail "代理重新启动" "$(tail -3 "$AGENT_LOG")"; fi
  CODE=$(get_containers)
  expect_eq "代理回来后清单恢复 200" "200" "$CODE"

  # 11.2 dockerd 停掉：列表 200 + available=false + notice；详情 503 DOCKER_UNAVAILABLE；
  #      预检是 allowed=false + DOCKER_UNAVAILABLE（不是 500）。
  #
  # 等的是“docker 命令不再能问到守护进程”，不是“套接字文件消失了”：dockerd 被停掉之后
  # socket 文件可能还留在 /var/run 里，而产品（Q271）正是按 docker 自己的
  # “Cannot connect to the Docker daemon” 判定的。
  if command -v systemctl >/dev/null 2>&1; then
    systemctl stop docker.socket docker.service >/dev/null 2>&1 || true
    _i=0
    while [ "$_i" -lt 60 ]; do
      docker info >/dev/null 2>&1 || break
      _i=$((_i + 1)); sleep 0.5
    done
    if docker info >/dev/null 2>&1; then
      notrun "dockerd 停掉后的表现" "docker 守护进程没停下来，无法验证 DOCKER_UNAVAILABLE"
    else
      CODE=$(api "$JAR" "$CSRF" GET "$CONTAINERS")
      cp "$WORK/resp.json" "$PAYLOADS/containers-dockerdown.json" 2>/dev/null
      expect_eq "dockerd 停掉时清单仍是 200（不是 503）" "200" "$CODE"
      expect_eq "…available=false" "False" "$(dget '["available"]')"
      expect_ne "…notice 说清为什么" "" "$(dget '["notice"]')"
      expect_contains "…notice 提到 Docker" "Docker" "$(dget '["notice"]')"
      expect_eq "…容器列表是空数组，不是 null" "[]" "$(dget '["containers"]')"
      CODE=$(api "$JAR" "$CSRF" GET "$CONTAINERS/$ID_OK")
      cp "$WORK/resp.json" "$PAYLOADS/container-dockerdown.json" 2>/dev/null
      expect_eq "dockerd 停掉时读详情 → 503" "503" "$CODE"
      expect_eq "…错误码是 DOCKER_UNAVAILABLE" "DOCKER_UNAVAILABLE" "$(ecode)"
      # 日志走的是同一个判决：代理回 503 DOCKER_UNAVAILABLE，Web 必须原样带出。
      # 旧的 Web 侧只按状态码分类，会把任何 5xx 都改写成 AGENT_UNAVAILABLE
      # （真机上就是这里露的），所以详情和日志各验一条。
      CODE=$(api "$JAR" "$CSRF" GET "$CONTAINERS/$ID_OK/logs?tail=10")
      cp "$WORK/resp.json" "$PAYLOADS/container-logs-dockerdown.json" 2>/dev/null
      expect_eq "dockerd 停掉时读日志 → 503" "503" "$CODE"
      expect_eq "…错误码也是 DOCKER_UNAVAILABLE" "DOCKER_UNAVAILABLE" "$(ecode)"
      CODE=$(preflight stop-dockerdown "{\"action\":\"container.control\",\"params\":{\"container_id\":\"$ID_OK\",\"operation\":\"stop\"}}")
      expect_eq "dockerd 停掉时预检仍是 200" "200" "$CODE"
      expect_eq "…allowed=false" "False" "$(jget "$PAYLOADS/preflight-stop-dockerdown.json" 'd["data"]["allowed"]')"
      expect_eq "…阻塞码 DOCKER_UNAVAILABLE" "DOCKER_UNAVAILABLE" \
        "$(jget "$PAYLOADS/preflight-stop-dockerdown.json" 'd["data"]["blockers"][0]["code"]')"
      PF=$(jget "$PAYLOADS/preflight-stop-dockerdown.json" 'd["data"]["preflight_id"]')
      expect_eq "提交这种预检 → 409" "409" "$(submit_job stop-dockerdown "$PF")"
      expect_eq "…错误码 DEPENDENCY_BLOCKED" "DEPENDENCY_BLOCKED" "$(ecode)"
    fi
    # 把 dockerd 拉回来：验收机器要能继续用，后面的清理也要靠它。
    systemctl start docker.socket docker.service >/dev/null 2>&1 || true
    _i=0
    while [ "$_i" -lt 60 ]; do
      [ -S /var/run/docker.sock ] && docker info >/dev/null 2>&1 && break
      _i=$((_i + 1)); sleep 0.5
    done
    if docker info >/dev/null 2>&1; then
      pass "dockerd 已恢复（$(docker info --format '{{.ServerVersion}}' 2>/dev/null || echo '?')）"
    else
      fail "dockerd 已恢复" "docker 守护进程没有回来，后续清理可能失败"
    fi
  else
    notrun "dockerd 停掉后的表现" "本机没有 systemctl"
  fi
else
  notrun "两种不可用分开" "没有 docker 或夹具未就绪"
fi

echo
echo "== 12. 权限与审计 =="
CODE=$(api "$JAR" "$CSRF" POST "$USERS" \
  "{\"username\":\"p093-viewer\",\"display_name\":\"只读\",\"role\":\"viewer\",\"initial_password\":\"$VIEWER_PASSWORD\"}")
expect_eq "建 viewer 账号" "201" "$CODE"
VJAR=$WORK/viewer.jar
expect_eq "viewer 登录" "200" "$(login p093-viewer "$VIEWER_PASSWORD" "$VJAR" login-viewer)"
VCSRF=$(csrf_of "$WORK/login-viewer.json")
expect_ne "viewer 拿到 CSRF 令牌" "" "$VCSRF"
expect_eq "viewer 读容器清单 → 403（侦察材料）" "403" "$(api "$VJAR" "$VCSRF" GET "$CONTAINERS")"
expect_eq "viewer 读镜像 → 403" "403" "$(api "$VJAR" "$VCSRF" GET "$IMAGES")"
expect_eq "viewer 读部署 → 403" "403" "$(api "$VJAR" "$VCSRF" GET "$DEPLOYMENTS")"
if [ "$FIXTURES_OK" -eq 1 ]; then
  expect_eq "viewer 读容器详情 → 403" "403" "$(api "$VJAR" "$VCSRF" GET "$CONTAINERS/$ID_OK")"
  expect_eq "viewer 发容器预检 → 403" "403" \
    "$(api "$VJAR" "$VCSRF" POST "$PREFLIGHT_PATH" "{\"action\":\"container.control\",\"params\":{\"container_id\":\"$ID_OK\",\"operation\":\"stop\"}}")"
else
  notrun "viewer 读容器详情 / 发预检" "没有 docker 或夹具未就绪"
fi

PF_COUNT=$(dbq "SELECT COUNT(*) FROM audit_events WHERE action = 'operation.preflight' AND target = 'preflight:container.control'")
if [ "${PF_COUNT:-0}" -gt 0 ] 2>/dev/null; then
  pass "审计里有 container.control 的预检记录（$PF_COUNT 条）"
else
  fail "审计里有 container.control 的预检记录" "实际 $PF_COUNT 条"
fi
JOB_COUNT=$(dbq "SELECT COUNT(*) FROM audit_events WHERE action = 'job.submit'")
if [ "${JOB_COUNT:-0}" -gt 0 ] 2>/dev/null; then
  pass "审计里有任务提交记录（$JOB_COUNT 条）"
else
  fail "审计里有任务提交记录" "实际 $JOB_COUNT 条"
fi
DENIED=$(dbq "SELECT COUNT(*) FROM audit_events WHERE action = 'operation.preflight' AND outcome = 'failure'")
if [ "${DENIED:-0}" -gt 0 ] 2>/dev/null; then
  pass "审计里记下了被拒的预检（$DENIED 条）"
else
  fail "审计里记下了被拒的预检" "实际 $DENIED 条"
fi

echo
echo "== 汇总 =="
printf 'PASS %d / FAIL %d / NOT RUN %d\n' "$PASSED" "$FAILED" "$NOTRUN"
if [ "$FAILED" -gt 0 ]; then
  printf '失败项：%s\n' "$FAILED_NAMES"
  exit 1
fi
exit 0
