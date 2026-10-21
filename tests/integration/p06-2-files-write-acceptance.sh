#!/bin/sh
# P06.2 Linux 验收脚本：流式暂存上传、落位提交、编辑备份、建目录、复制/移动/删除、
# 权限/所有者受限修改、版本冲突与配额——在一台真的 Ubuntu 上逐条验证。
#
# 运行位置：Ubuntu 24.04 验收 VM 内，**必须以 root 运行**
# （代理只能在 root 下启动；要造只读根、符号链接、固定权限位的文件，还要能在
#  根外直接改文件来模拟“别人同时改了它”）。
# 不依赖 Go 工具链：用宿主交叉编译好的 Linux 二进制。
#
# 用法：
#   sudo BIN_DIR=/tmp/upanel-bin sh tests/integration/p06-2-files-write-acceptance.sh
#
# 可选环境变量：
#   PORT=19462      Web 监听端口
#
# 退出码：0 全部通过；1 有失败项；2 环境不满足（非 root）。
#
# 覆盖的验收点（STAGES §8 P06.2、CONTRACTS §11.2、§4、§5、PLAN §15）：
#   1. 能力表登记了八个 file.* 动作（少报等于隐藏功能）
#   2. 暂存上传：create → PUT 原始字节 → 目标的**一个字节都没变** → commit_upload
#      预检 → 任务 → 读回 sha256 一致、上传记录消失（上传 ≠ 落地）
#   3. 上传边界：声明超 max_upload_bytes 是 413 且不留暂存；Content-Length 不符 400；
#      无 Content-Length 400；state=received 再 PUT 是 409；DELETE 后 GET 是 404；
#      目标为目录 400；只读根 403
#   4. file.write_text：新建/覆盖都读回验证；覆盖留备份（旧内容真的在备份里）；
#      目标已存在却没给版本是 VERSION_CONFLICT 阻塞；旧版本再写同样阻塞；
#      备份份数上限 10，超出从最旧开始清理
#   5. 外部改文件之后再提交任务 → 任务 failed 且 code=PREFLIGHT_STALE（不是静默覆盖）
#   6. file.mkdir：建目录、重复是 TARGET_EXISTS、父目录不存在是 PARENT_MISSING
#   7. file.copy / file.move：同根/跨根；目标已存在阻塞；含符号链接的树 TREE_UNSUPPORTED；
#      跨根 move 的 impacts 带 not_atomic，同根带 rename
#   8. file.delete：非空目录非递归是 DIRECTORY_NOT_EMPTY；未再认证提交是 403 REAUTH_REQUIRED
#      且**没有烧掉预检**；递归删除的短语首词是 DELETE，错短语是 CONFIRMATION_MISMATCH；
#      删除后读回不存在，条目数与字节数如实
#   9. file.chmod：0640 生效并读回；4755（setuid）在参数层拒绝；没变化有 NO_CHANGE 告警；
#      未再认证 403
#  10. file.chown：账号模块未落地时预检就 MANAGED_ACCOUNTS_UNAVAILABLE（不接受任意 uid/gid）
#  11. 路径形状：../、绝对路径、`.upanel` 在参数层拒绝；指向根外的符号链接是 TARGET_NOT_REGULAR
#  12. 只读根（writable=false）上的写动作全是 ROOT_READ_ONLY
#  13. 任务中心：GET /jobs 能看到本轮的动作与终态；终态任务不可取消（409）
#  14. 权限：未登录 401、viewer 403（上传、预检、任务三处）
#  15. 回归：概览/存储/服务/日志/文件列表在加了写动作之后仍然正常
#
# 明确不覆盖（NOT RUN，原因写在这里，不在脚本里假装通过）：
#   - 浏览器渲染与文件页交互：VM 上没有浏览器，页面级证据在宿主上用真实载荷回放。
#   - 磁盘满（ENOSPC）：要造一个真的满的卷，且会把 VM 的根盘写满；ENOSPC 的判决由
#     internal/files 的单元用例覆盖，这里不伪造。
#   - 24 小时暂存过期与定期清理：验收不可能等 24 小时。
#   - 500 MiB 备份总量上限：要写 500 MiB 才能触发，收益不抵用时；份数上限（10）已验。
#   - 5 万项目录树的递归删除/复制上限：P06.1 已验过 50 001 项的列举截断，
#     树的拒绝路径由单元用例覆盖。
#   - 并发真竞态（两个请求同时改同一个文件）：单机顺序脚本无法可靠制造。

set -u

BIN_DIR=${BIN_DIR:-/tmp/upanel-bin}
HERE=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)

if [ "$(id -u)" -ne 0 ]; then
  printf '本脚本必须以 root 运行：sudo BIN_DIR=%s sh %s\n' "$BIN_DIR" "$0" >&2
  exit 2
fi

AGENT="$BIN_DIR/upanel-agent"
WEB="$BIN_DIR/upanel-web"
CTL="$BIN_DIR/upanelctl"

WORK=${WORK:-/var/tmp/upanel-accept-p06-2}
PAYLOADS=$WORK/payloads
RUN_DIR=/run/upanel
SOCK=$RUN_DIR/agent.sock
SYS_CFG=/etc/upanel/upanel.json
AGENT_DB_DIR=/var/lib/upanel-agent
AGENT_DB=$AGENT_DB_DIR/agent.db
KEY_FILE=/etc/upanel/agent.key
WEB_DB_DIR=$WORK/webdata
WEB_DB=$WEB_DB_DIR/upanel.db
PORT=${PORT:-19462}
BASE="http://127.0.0.1:$PORT"
ORIGIN="$BASE"
WEB_USER=upanel-web
PASSWORD='vm-correct-horse-9'
VIEWER_PASSWORD='vm-correct-horse-8'

ROOT_RW=$WORK/rw            # 可写根：本轮所有写动作都在这里
ROOT_RW2=$WORK/rw2          # 第二个可写根：跨根 move 的目标
ROOT_RO=$WORK/ro            # 只读根：writable=false
UPLOAD_BYTES=1572864        # 1.5 MiB：超过 Web 曾经那个 1 MiB 的全局请求体上限
MAX_UPLOAD=4194304          # 4 MiB：agent.files.max_upload_bytes
STAGING=$ROOT_RW/.upanel/staging
BACKUPS=$ROOT_RW/.upanel/backups
PAYLOAD=$WORK/payload.bin

FILES=/api/v1/files
AGENT_LOG=$WORK/logs/agent.log
WEB_LOG=$WORK/logs/web.log
AGENT_PID=""
WEB_PID=""

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

# jget <文件> <python 表达式，d 为解析后的 JSON>
#
# 先按**单个表达式**求值；不是表达式（多语句、赋值）就按语句块执行，打印 `_`。
# 断言里需要临时变量的地方因此不用为了“能 eval”把判断挤成一行看不懂的东西。
#
# 求值本身出错时打印 ERR:<类型>:<消息>：空字符串会让一次“表达式写错”看起来像
# “响应里没这个字段”，而这两件事的下一步完全不同。
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
resp_get() { jget "$WORK/resp.json" "$1"; }
pget() { jget "$WORK/preflight.json" "$1"; }
jobget() { jget "$WORK/job.json" "$1"; }

cleanup() {
  stop_web
  stop_agent
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

stop_web() {
  if [ -n "$WEB_PID" ] && kill -0 "$WEB_PID" 2>/dev/null; then
    kill -TERM "$WEB_PID" 2>/dev/null
    wait "$WEB_PID" 2>/dev/null
  fi
  WEB_PID=""
  for _ in $(seq 1 100); do
    curl -fsS "$BASE/api/v1/health/live" >/dev/null 2>&1 || return 0
    sleep 0.1
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

# api <jar> <csrf> <方法> <路径> [body]：响应写到 $WORK/resp.json，回显状态码。
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

# preflight <动作> <参数 JSON>：预检响应另存为 $WORK/preflight.json，回显状态码。
# 单独存一份是为了能在同一节里既断言状态码又多次读结论，不被后续请求冲掉。
preflight() {
  printf '{"action":"%s","params":%s}' "$1" "$2" >"$WORK/pre.json"
  _code=$(curl -s -b "$JAR" -o "$WORK/resp.json" -w '%{http_code}' \
    -X POST "$BASE/api/v1/operations/preflight" \
    -H 'Content-Type: application/json' -H "Origin: $ORIGIN" -H "X-CSRF-Token: $CSRF" \
    -d @"$WORK/pre.json")
  cp "$WORK/resp.json" "$WORK/preflight.json"
  printf '%s' "$_code"
}

# submit_job <preflight_id> <短语> <幂等键>：响应存为 $WORK/submit.json，回显状态码。
submit_job() {
  python3 - "$1" "${2:-}" >"$WORK/jobreq.json" <<'PY'
import json, sys
print(json.dumps({"preflight_id": sys.argv[1], "confirmation_phrase": sys.argv[2]}))
PY
  _code=$(curl -s -b "$JAR" -o "$WORK/resp.json" -w '%{http_code}' -X POST "$BASE/api/v1/jobs" \
    -H 'Content-Type: application/json' -H "Origin: $ORIGIN" -H "X-CSRF-Token: $CSRF" \
    -H "Idempotency-Key: $3" -d @"$WORK/jobreq.json")
  cp "$WORK/resp.json" "$WORK/submit.json"
  printf '%s' "$_code"
}

# wait_job <job_id>：轮询到终态，任务视图存为 $WORK/job.json，回显终态或 timeout。
wait_job() {
  _i=0
  while [ "$_i" -lt 150 ]; do
    api "$JAR" "$CSRF" GET "/api/v1/jobs/$1" >/dev/null
    cp "$WORK/resp.json" "$WORK/job.json"
    _st=$(jobget 'd["data"]["status"]')
    case "$_st" in
      succeeded|failed|canceled|interrupted) printf '%s' "$_st"; return 0 ;;
    esac
    _i=$((_i + 1))
    sleep 0.2
  done
  printf 'timeout'
}

# submit_and_wait <preflight_id> <短语> <幂等键>
# 回显任务终态；提交阶段就被拒时回显 "submit:<状态码>"（那样断言里一眼看得出
# 是“没提交上去”还是“执行失败”）。任务视图始终在 $WORK/job.json / submit.json。
submit_and_wait() {
  _code=$(submit_job "$1" "${2:-}" "$3")
  if [ "$_code" != "202" ]; then printf 'submit:%s' "$_code"; return 0; fi
  _id=$(jget "$WORK/submit.json" 'd["data"]["job_id"]')
  wait_job "$_id"
}

# run_action <动作> <参数> <短语> <幂等键>：预检 → 任务 → 等终态，回显终态。
run_action() {
  _pf_code=$(preflight "$1" "$2")
  if [ "$_pf_code" != "200" ]; then printf 'preflight:%s' "$_pf_code"; return 0; fi
  _pf=$(pget 'd["data"]["preflight_id"]')
  submit_and_wait "$_pf" "${3:-}" "$4"
}

# J <python 表达式>：在当前任务视图/预检视图里取字段的便捷别名（保持断言短）。
jstatus() { jobget 'd["data"]["status"]'; }
joutcome() { jobget 'd["data"]["result"]["outcome"]'; }
jerr() { jobget 'd["data"]["error"]["code"]'; }
jobserved() { jobget "d[\"data\"][\"result\"][\"observed_state\"][\"$1\"]"; }
jdata() { jobget "d[\"data\"][\"result\"][\"data\"].get(\"$1\")"; }

# pcode <n>：预检结论里第 n 个阻塞项/告警项的码。
pblocker() { pget "d[\"data\"][\"blockers\"][$1][\"code\"]"; }
pwarn() { pget "d[\"data\"][\"warnings\"][$1][\"code\"]"; }
pimpact() { pget "d[\"data\"][\"impacts\"][$1][\"kind\"]"; }

urlenc() { python3 -c 'import sys,urllib.parse;print(urllib.parse.quote(sys.argv[1],safe=""))' "$1"; }
sha_of() { sha256sum "$1" 2>/dev/null | cut -d' ' -f1; }
# file_version <路径>：用文本接口读当前 version（写动作的 expected_version 来源）。
file_version() {
  api "$JAR" "$CSRF" GET "$FILES/text?root_id=rw&path=$(urlenc "$1")" >/dev/null
  resp_get 'd["data"]["version"]'
}
# staging_count：暂存目录里还留着几条上传记录（.json 是记录，.part 是数据）。
staging_count() {
  find "$STAGING" -maxdepth 1 -name 'up_*.json' 2>/dev/null | wc -l | tr -d ' '
}

echo "== 0. 环境 =="
for b in "$AGENT" "$WEB" "$CTL"; do
  if [ -x "$b" ]; then pass "二进制存在且可执行 $(basename "$b")"; else fail "二进制存在且可执行 $(basename "$b")" "$b"; fi
done
for t in python3 curl runuser stat sha256sum find; do
  if command -v "$t" >/dev/null 2>&1; then pass "依赖命令可用 $t"; else fail "依赖命令可用 $t" "缺少 $t"; fi
done
if ! id -u "$WEB_USER" >/dev/null 2>&1; then
  note "创建系统账号 $WEB_USER"
  useradd --system --no-create-home --shell /usr/sbin/nologin "$WEB_USER" 2>/dev/null
fi
if id -u "$WEB_USER" >/dev/null 2>&1; then pass "Web 账号 $WEB_USER 存在"; else fail "Web 账号 $WEB_USER 存在" "无法创建"; fi
if curl -fsS "$BASE/api/v1/health/live" >/dev/null 2>&1; then
  echo "$PORT 上已有服务在监听；先停掉遗留的 upanel-web 再跑验收" >&2
  exit 1
fi

rm -rf "$WORK"
mkdir -p "$WORK/logs" "$PAYLOADS" "$WEB_DB_DIR"
chmod 0755 "$WORK" "$PAYLOADS" "$WORK/logs"
chown -R "$WEB_USER" "$WEB_DB_DIR"
uname -r >"$WORK/logs/kernel.txt" 2>/dev/null || true

echo
echo "== 1. 造现场（可写根 / 第二可写根 / 只读根 / 上传载荷） =="
mkdir -p "$ROOT_RW/uploads" "$ROOT_RW/adir" "$ROOT_RW/docs/nested" "$ROOT_RW/tree-link" \
         "$ROOT_RW2/target" "$ROOT_RO"
printf 'v1\n' >"$ROOT_RW/notes.txt"
printf 'old\n' >"$ROOT_RW/old.conf"
printf 'c1\n' >"$ROOT_RW/conflict.conf"
printf 'm\n' >"$ROOT_RW/modes.conf"
chmod 0644 "$ROOT_RW/modes.conf"
printf 'a\n' >"$ROOT_RW/docs/a.txt"
printf 'b\n' >"$ROOT_RW/docs/nested/b.txt"
# 根外的秘密：写动作的任何一次越界都会把它暴露出来。
mkdir -p "$WORK/outside"
printf 'do-not-leak\n' >"$WORK/outside/secret.txt"
ln -sf "$WORK/outside/secret.txt" "$ROOT_RW/link-out"
# 树里有符号链接：递归复制/移动必须整棵拒绝，而不是“跳过它继续”。
ln -sf "$WORK/outside/secret.txt" "$ROOT_RW/tree-link/inside-link"
printf 't\n' >"$ROOT_RW/tree-link/t.txt"
printf 'ro\n' >"$ROOT_RO/ro.txt"
# 载荷：1.5 MiB 确定性字节。用 python 写，避免 POSIX sh 的 printf 在
# 大数量与二进制字节上的各种不确定行为。
python3 - "$PAYLOAD" "$UPLOAD_BYTES" <<'PY'
import sys
path, size = sys.argv[1], int(sys.argv[2])
buf = bytearray(size)
for i in range(size):
    buf[i] = (i * 37 + 11) % 251
open(path, "wb").write(bytes(buf))
PY
PAYLOAD_SHA=$(sha_of "$PAYLOAD")
expect_eq "载荷字节数" "$UPLOAD_BYTES" "$(stat -c '%s' "$PAYLOAD")"
note "载荷 sha256：$PAYLOAD_SHA"
cp "$ROOT_RW/notes.txt" "$PAYLOADS/notes-initial.txt" 2>/dev/null
ls -la "$ROOT_RW" >"$WORK/logs/root-listing.txt" 2>&1

echo
echo "== 2. 配置与启动（代理 root、Web 非 root） =="
install -d -m 0755 /etc/upanel
install -d -m 0700 "$AGENT_DB_DIR"
install -d -m 0755 "$RUN_DIR"
rm -f "$AGENT_DB" "$AGENT_DB-wal" "$AGENT_DB-shm"
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
    "logs": {},
    "files": {
      "max_upload_bytes": $MAX_UPLOAD,
      "roots": [
        { "id": "rw",  "name": "可写根",   "path": "$ROOT_RW",  "writable": true },
        { "id": "rw2", "name": "第二可写根", "path": "$ROOT_RW2", "writable": true },
        { "id": "ro",  "name": "只读根",   "path": "$ROOT_RO",  "writable": false }
      ]
    }
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
expect_eq "agent.sock 不是 0666" "no" \
  "$(stat -c '%a' "$SOCK" 2>/dev/null | grep -qx 666 && echo yes || echo no)"

# 能力表：八个 file.* 动作一个不少地登记了。少报等于隐藏功能，多报等于编造。
runuser -u "$WEB_USER" -- curl -s --unix-socket "$SOCK" \
  http://localhost/internal/v1/capabilities >"$PAYLOADS/agent-capabilities.json" 2>/dev/null
expect_eq "能力表登记了八个 file.* 动作" "True" \
  "$(jget "$PAYLOADS/agent-capabilities.json" '
_ = {"file.mkdir","file.write_text","file.commit_upload","file.copy","file.move",
     "file.delete","file.chmod","file.chown"} <= set(d["data"]["supported_actions"])')"
note "代理登记的动作：$(jget "$PAYLOADS/agent-capabilities.json" 'sorted(a for a in d["data"]["supported_actions"] if a.startswith("file."))')"

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

JAR=$WORK/admin.jar
expect_eq "管理员登录" "200" "$(login admin "$PASSWORD" "$JAR" login-admin)"
CSRF=$(csrf_of "$WORK/login-admin.json")
if [ -n "$CSRF" ]; then pass "拿到 CSRF 令牌"; else fail "拿到 CSRF 令牌" "登录响应里没有 csrf_token"; fi

api "$JAR" "$CSRF" POST /api/v1/users \
  "{\"username\":\"viewer\",\"display_name\":\"只读\",\"role\":\"viewer\",\"initial_password\":\"$VIEWER_PASSWORD\"}" \
  >"$WORK/viewer-code.txt"
expect_eq "创建 viewer 账号" "201" "$(cat "$WORK/viewer-code.txt")"
chown_web_db
VJAR=$WORK/viewer.jar
expect_eq "viewer 登录" "200" "$(login viewer "$VIEWER_PASSWORD" "$VJAR" login-viewer)"

echo
echo "== 3. 暂存上传：声明 → 传字节 → 目标仍未变 → 提交落位 =="
expect_eq "创建暂存上传" "201" \
  "$(api "$JAR" "$CSRF" POST "$FILES/uploads" \
     "{\"root_id\":\"rw\",\"path\":\"uploads/blob.bin\",\"size_bytes\":\"$UPLOAD_BYTES\"}")"
cp "$WORK/resp.json" "$PAYLOADS/upload-created.json"
UP1=$(resp_get 'd["data"]["upload_id"]')
expect_eq "upload_id 形状是 up_ + 32 位十六进制" "True" \
  "$(resp_get '__import__("re").fullmatch(r"up_[0-9a-f]{32}", d["data"]["upload_id"]) is not None')"
expect_eq "创建时 state 是 open" "open" "$(resp_get 'd["data"]["state"]')"
expect_eq "创建时 received_bytes 是 \"0\"" '"0"' "$(resp_get 'json.dumps(d["data"]["received_bytes"])')"
expect_eq "创建时不产生目标文件" "no" "$([ -e "$ROOT_RW/uploads/blob.bin" ] && echo yes || echo no)"
expect_eq "创建时暂存里有一条记录" "1" "$(staging_count)"

# PUT 原始字节：Content-Type 是 octet-stream，不是 JSON。
PUT_CODE=$(curl -s -b "$JAR" -o "$WORK/resp.json" -w '%{http_code}' -X PUT \
  "$BASE$FILES/uploads/$UP1/content" -H 'Content-Type: application/octet-stream' \
  -H "Origin: $ORIGIN" -H "X-CSRF-Token: $CSRF" --data-binary @"$PAYLOAD")
expect_eq "PUT 上传内容" "200" "$PUT_CODE"
cp "$WORK/resp.json" "$PAYLOADS/upload-received.json"
expect_eq "收完后 state 是 received" "received" "$(resp_get 'd["data"]["state"]')"
expect_eq "received_bytes 等于声明字节数" "$UPLOAD_BYTES" "$(resp_get 'int(d["data"]["received_bytes"])')"
expect_eq "收完之后目标**仍然**不存在（上传不等于落地）" "no" \
  "$([ -e "$ROOT_RW/uploads/blob.bin" ] && echo yes || echo no)"
expect_eq "GET 上传视图" "200" "$(api "$JAR" "$CSRF" GET "$FILES/uploads/$UP1")"
cp "$WORK/resp.json" "$PAYLOADS/upload-get.json"

expect_eq "commit_upload 预检" "200" \
  "$(preflight file.commit_upload "{\"upload_id\":\"$UP1\",\"overwrite\":false}")"
cp "$WORK/preflight.json" "$PAYLOADS/preflight-commit.json"
expect_eq "commit 预检允许" "True" "$(pget 'd["data"]["allowed"]')"
expect_eq "commit 的 impact 是 create" "create" "$(pimpact 0)"
expect_eq "commit 不需要再认证（overwrite=false）" "False" "$(pget 'd["data"]["requires_reauth"]')"
expect_eq "commit 的确认短语是 null（不要求短语）" "True" "$(pget 'd["data"]["confirmation_phrase"] is None')"
UP_PF=$(pget 'd["data"]["preflight_id"]')
expect_eq "提交落位任务并成功" "succeeded" "$(submit_and_wait "$UP_PF" "" "k-commit-1")"
cp "$WORK/job.json" "$PAYLOADS/job-commit.json"
expect_eq "落位 outcome 是 verified" "verified" "$(joutcome)"
expect_eq "落位后读回的大小" "$UPLOAD_BYTES" "$(jobserved size_bytes)"
expect_eq "落位后读回的路径" "uploads/blob.bin" "$(jobserved path)"
expect_eq "落位后的文件 sha256 与载荷一致" "$PAYLOAD_SHA" "$(sha_of "$ROOT_RW/uploads/blob.bin")"
expect_eq "新建文件不留备份" "None" "$(jdata backup_path)"
expect_eq "落位后暂存记录消失" "0" "$(staging_count)"
expect_eq "落位后 GET 上传是 404（记录已随落位消失）" "404" \
  "$(api "$JAR" "$CSRF" GET "$FILES/uploads/$UP1")"
expect_eq "落位后的 404 错误码" "NOT_FOUND" "$(resp_get 'd["error"]["code"]')"

echo
echo "== 4. 上传的边界与配额 =="
BASE_STAGING=$(staging_count)
expect_eq "声明超过 max_upload_bytes 是 413" "413" \
  "$(api "$JAR" "$CSRF" POST "$FILES/uploads" \
     "{\"root_id\":\"rw\",\"path\":\"uploads/too-big.bin\",\"size_bytes\":\"$((MAX_UPLOAD + 1))\"}")"
cp "$WORK/resp.json" "$PAYLOADS/upload-too-large.json"
expect_eq "超限的错误码是 VALIDATION_ERROR（413 上的体积错）" "VALIDATION_ERROR" "$(resp_get 'd["error"]["code"]')"
expect_eq "超限不留暂存" "$BASE_STAGING" "$(staging_count)"

expect_eq "上传到目录上是 400" "400" \
  "$(api "$JAR" "$CSRF" POST "$FILES/uploads" \
     "{\"root_id\":\"rw\",\"path\":\"adir\",\"size_bytes\":\"16\"}")"
expect_eq "目标是目录的错误码" "VALIDATION_ERROR" "$(resp_get 'd["error"]["code"]')"

expect_eq "上传到只读根是 403" "403" \
  "$(api "$JAR" "$CSRF" POST "$FILES/uploads" \
     "{\"root_id\":\"ro\",\"path\":\"new.bin\",\"size_bytes\":\"16\"}")"
expect_eq "只读根的错误码" "FORBIDDEN" "$(resp_get 'd["error"]["code"]')"

expect_eq "未登记的根是 404" "404" \
  "$(api "$JAR" "$CSRF" POST "$FILES/uploads" \
     "{\"root_id\":\"nope\",\"path\":\"new.bin\",\"size_bytes\":\"16\"}")"
expect_eq "未登记根的错误码" "NOT_FOUND" "$(resp_get 'd["error"]["code"]')"

# Content-Length 与声明不符：入口就 400，不先传一遍。
expect_eq "创建 100 字节的暂存" "201" \
  "$(api "$JAR" "$CSRF" POST "$FILES/uploads" \
     "{\"root_id\":\"rw\",\"path\":\"uploads/short.bin\",\"size_bytes\":\"100\"}")"
UP_SHORT=$(resp_get 'd["data"]["upload_id"]')
python3 -c "open('$WORK/short.bin','wb').write(b'x'*99)"
expect_eq "Content-Length 与声明不符是 400" "400" \
  "$(curl -s -b "$JAR" -o "$WORK/resp.json" -w '%{http_code}' -X PUT \
     "$BASE$FILES/uploads/$UP_SHORT/content" -H 'Content-Type: application/octet-stream' \
     -H "Origin: $ORIGIN" -H "X-CSRF-Token: $CSRF" --data-binary @"$WORK/short.bin")"
expect_eq "长度不符的错误码" "VALIDATION_ERROR" "$(resp_get 'd["error"]["code"]')"

# 没有 Content-Length（分块传输）：不再猜“传完了没有”。
expect_eq "无 Content-Length 是 400" "400" \
  "$(cat "$WORK/short.bin" | curl -s -b "$JAR" -o "$WORK/resp.json" -w '%{http_code}' -X PUT \
     "$BASE$FILES/uploads/$UP_SHORT/content" -H 'Content-Type: application/octet-stream' \
     -H "Origin: $ORIGIN" -H "X-CSRF-Token: $CSRF" -T -)"

# 收完之后再传一次：状态已经不是 open。
#
# 注意不能拿 $UP1 来验：它已经 commit 落位，记录随之消失，再 PUT 得到的是 404
# （“没有这个上传”），而不是 409（“上传状态不对”）。这两件事不一样，所以这里
# 专门开一个新上传，只收一次、再来一次。
expect_eq "创建第三个暂存（验重复 PUT）" "201" \
  "$(api "$JAR" "$CSRF" POST "$FILES/uploads" \
     "{\"root_id\":\"rw\",\"path\":\"uploads/rep.bin\",\"size_bytes\":\"32\"}")"
UP_REP=$(resp_get 'd["data"]["upload_id"]')
python3 -c "open('$WORK/rep.bin','wb').write(b'r'*32)"
expect_eq "第一次 PUT 到新暂存" "200" \
  "$(curl -s -b "$JAR" -o "$WORK/resp.json" -w '%{http_code}' -X PUT \
     "$BASE$FILES/uploads/$UP_REP/content" -H 'Content-Type: application/octet-stream' \
     -H "Origin: $ORIGIN" -H "X-CSRF-Token: $CSRF" --data-binary @"$WORK/rep.bin")"
expect_eq "received 之后再 PUT 是 409" "409" \
  "$(curl -s -b "$JAR" -o "$WORK/resp.json" -w '%{http_code}' -X PUT \
     "$BASE$FILES/uploads/$UP_REP/content" -H 'Content-Type: application/octet-stream' \
     -H "Origin: $ORIGIN" -H "X-CSRF-Token: $CSRF" --data-binary @"$WORK/rep.bin")"
expect_eq "重复 PUT 的错误码是 VERSION_CONFLICT（状态不一致，不是参数错）" "VERSION_CONFLICT" \
  "$(resp_get 'd["error"]["code"]')"
# 收干净：这条上传只为验状态机，不留到后面的用例里。
expect_eq "收掉这条暂存" "200" "$(api "$JAR" "$CSRF" DELETE "$FILES/uploads/$UP_REP")"

# 放弃上传：只删暂存，不碰目标。
expect_eq "创建第二个暂存" "201" \
  "$(api "$JAR" "$CSRF" POST "$FILES/uploads" \
     "{\"root_id\":\"rw\",\"path\":\"uploads/drop.bin\",\"size_bytes\":\"32\"}")"
UP_DROP=$(resp_get 'd["data"]["upload_id"]')
expect_eq "DELETE 暂存" "200" "$(api "$JAR" "$CSRF" DELETE "$FILES/uploads/$UP_DROP")"
expect_eq "DELETE 之后 GET 是 404" "404" "$(api "$JAR" "$CSRF" GET "$FILES/uploads/$UP_DROP")"
expect_eq "放弃上传不会创建目标文件" "no" \
  "$([ -e "$ROOT_RW/uploads/drop.bin" ] && echo yes || echo no)"

# 目标已存在时：不 overwrite 是 TARGET_EXISTS，overwrite 但版本不对是 VERSION_CONFLICT。
expect_eq "创建指向已有文件的暂存" "201" \
  "$(api "$JAR" "$CSRF" POST "$FILES/uploads" \
     "{\"root_id\":\"rw\",\"path\":\"old.conf\",\"size_bytes\":\"5\"}")"
UP_OVER=$(resp_get 'd["data"]["upload_id"]')
printf 'five\n' >"$WORK/five.txt"
printf 'newbz' >"$WORK/newbz.bin"
expect_eq "PUT 到已有文件的暂存" "200" \
  "$(curl -s -b "$JAR" -o "$WORK/resp.json" -w '%{http_code}' -X PUT \
     "$BASE$FILES/uploads/$UP_OVER/content" -H 'Content-Type: application/octet-stream' \
     -H "Origin: $ORIGIN" -H "X-CSRF-Token: $CSRF" --data-binary @"$WORK/newbz.bin")"
expect_eq "目标已存在且不覆盖：预检阻塞" "200" \
  "$(preflight file.commit_upload "{\"upload_id\":\"$UP_OVER\",\"overwrite\":false}")"
expect_eq "阻塞码是 TARGET_EXISTS" "TARGET_EXISTS" "$(pblocker 0)"
expect_eq "被阻塞的预检 allowed=false" "False" "$(pget 'd["data"]["allowed"]')"
expect_eq "覆盖但版本不对也是 VERSION_CONFLICT" "200" \
  "$(preflight file.commit_upload \
     "{\"upload_id\":\"$UP_OVER\",\"overwrite\":true,\"expected_version\":\"v1-0000-0000-0-0\"}")"
expect_eq "覆盖的版本阻塞码" "VERSION_CONFLICT" "$(pblocker 0)"

echo
echo "== 5. file.write_text：新建、覆盖、备份与版本 =="
expect_eq "写新文件（目标不存在，不带版本）" "succeeded" \
  "$(run_action file.write_text '{"root_id":"rw","relative_path":"created.conf","content":"hello\n"}' "" "k-wt-new")"
cp "$WORK/job.json" "$PAYLOADS/job-write-new.json"
expect_eq "新建 outcome 是 verified" "verified" "$(joutcome)"
expect_eq "新建后读回路径" "created.conf" "$(jobserved path)"
expect_eq "新建后读回大小" "6" "$(jobserved size_bytes)"
expect_eq "文件确实写到了盘上" "hello" "$(cat "$ROOT_RW/created.conf")"
api "$JAR" "$CSRF" GET "$FILES/text?root_id=rw&path=created.conf" >/dev/null
expect_eq "文本接口读回内容一致" "True" "$(resp_get 'd["data"]["content"] == "hello\n"')"

# 覆盖：目标已存在却没给版本 = 拒绝（不接受“谁先写完算谁”）。
expect_eq "覆盖已有文件但不给版本：预检阻塞" "200" \
  "$(preflight file.write_text '{"root_id":"rw","relative_path":"notes.txt","content":"x\n"}')"
expect_eq "不给版本的阻塞码" "VERSION_CONFLICT" "$(pblocker 0)"

V_NOTES=$(file_version notes.txt)
note "notes.txt 当前 version：$V_NOTES"
expect_eq "带正确版本覆盖" "succeeded" \
  "$(run_action file.write_text \
     "{\"root_id\":\"rw\",\"relative_path\":\"notes.txt\",\"expected_version\":\"$V_NOTES\",\"content\":\"edited\n\"}" \
     "" "k-wt-over")"
cp "$WORK/job.json" "$PAYLOADS/job-write-overwrite.json"
expect_eq "覆盖 outcome 是 verified" "verified" "$(joutcome)"
expect_eq "覆盖后盘上内容" "edited" "$(cat "$ROOT_RW/notes.txt")"
BK=$(jdata backup_path)
expect_eq "覆盖留下备份（backup_path 非空）" "True" "$([ -n "$BK" ] && [ "$BK" != "None" ] && echo True || echo False)"
expect_eq "备份里是覆盖前的旧内容" "v1" "$(cat "$ROOT_RW/$BK" 2>/dev/null)"
expect_eq "备份份数是 1" "1" "$(jdata backup_count)"
note "备份路径：$BK"

# 旧版本再写：并发编辑的语义——手里那份已经被别人改过了。
expect_eq "用旧版本再写是阻塞" "200" \
  "$(preflight file.write_text \
     "{\"root_id\":\"rw\",\"relative_path\":\"notes.txt\",\"expected_version\":\"$V_NOTES\",\"content\":\"again\n\"}")"
expect_eq "旧版本的阻塞码" "VERSION_CONFLICT" "$(pblocker 0)"
expect_eq "旧版本没有改动文件" "edited" "$(cat "$ROOT_RW/notes.txt")"

# 备份份数上限 10：连写 11 次，最旧的那份被清掉。
_i=0
_ok=1
while [ "$_i" -lt 11 ]; do
  _i=$((_i + 1))
  _v=$(file_version old.conf)
  _st=$(run_action file.write_text \
    "{\"root_id\":\"rw\",\"relative_path\":\"old.conf\",\"expected_version\":\"$_v\",\"content\":\"gen$_i\n\"}" \
    "" "k-wt-prune-$_i")
  if [ "$_st" != "succeeded" ]; then
    fail "连写第 $_i 次成功" "$_st"
    _ok=0
    break
  fi
done
if [ "$_ok" -eq 1 ]; then
  pass "连续覆盖 11 次全部 verified"
  expect_eq "备份份数被限制在 10" "10" "$(jdata backup_count)"
  # 份数要按**这个文件**的备份目录数，不是数整个 .upanel/backups：
  # 那里还放着 notes.txt 的备份，混在一起数出来的 11 份不是任何东西的证据。
  _bk=$(jdata backup_path)
  _bkdir="$ROOT_RW/$(dirname "$_bk")"
  note "old.conf 的备份目录：$_bkdir"
  expect_eq "这个文件的备份目录里确实只有 10 份" "10" \
    "$(find "$_bkdir" -maxdepth 1 -type f 2>/dev/null | wc -l | tr -d ' ')"
  expect_eq "备份里不再有最早的 old 内容" "0" \
    "$(grep -rl '^old$' "$_bkdir" 2>/dev/null | wc -l | tr -d ' ')"
fi

# 内容上限：1 MiB 之内可以，超了在参数层拒绝。
#
# 请求体必须走**文件**（--data-binary @file）：1 MiB 的 JSON 内联进 argv 会撞上
# 内核单参数 128 KiB 的上限（MAX_ARG_STRLEN），curl 根本跑不起来，于是拿到的
# 状态码是空字符串——那不是“服务端拒绝了”，那是“请求没发出去”。
python3 - "$WORK/big-text.json" "$WORK/big-text-body.json" <<'PY'
import json, sys
params = {"root_id": "rw", "relative_path": "big.txt", "content": "a" * (1024 * 1024 + 1)}
json.dump(params, open(sys.argv[1], "w", encoding="utf-8"))
json.dump({"action": "file.write_text", "params": params},
          open(sys.argv[2], "w", encoding="utf-8"))
PY
pf_big=$(curl -s -b "$JAR" -o "$WORK/resp.json" -w '%{http_code}' \
  -X POST "$BASE/api/v1/operations/preflight" -H 'Content-Type: application/json' \
  -H "Origin: $ORIGIN" -H "X-CSRF-Token: $CSRF" --data-binary @"$WORK/big-text-body.json")
expect_eq "超过 1 MiB 的文本内容被拒（400）" "400" "$pf_big"
expect_eq "超文本上限的错误码" "VALIDATION_ERROR" "$(resp_get 'd["error"]["code"]')"

echo
echo "== 6. 外部改文件之后再提交：PREFLIGHT_STALE，而不是静默覆盖 =="
V_CONFLICT=$(file_version conflict.conf)
expect_eq "conflict.conf 预检允许" "200" \
  "$(preflight file.write_text \
     "{\"root_id\":\"rw\",\"relative_path\":\"conflict.conf\",\"expected_version\":\"$V_CONFLICT\",\"content\":\"mine\n\"}")"
expect_eq "预检结论是 allowed" "True" "$(pget 'd["data"]["allowed"]')"
PF_CONFLICT=$(pget 'd["data"]["preflight_id"]')
JOBS_BEFORE=$(api "$JAR" "$CSRF" GET "/api/v1/jobs?limit=200" >/dev/null; resp_get 'len(d["data"]["jobs"])')
# 预检之后、提交之前，别人（这里是 shell）把文件改了。
printf 'theirs\n' >"$ROOT_RW/conflict.conf"
# 代理在**受理**这一刻就会重新验证一次（internal/agent/operations.go），所以过期
# 预检收到的是 409 + PREFLIGHT_STALE，作业根本没有被创建——不是“建了作业再失败”。
expect_eq "预检过期后提交被拒（409）" "submit:409" "$(submit_and_wait "$PF_CONFLICT" "" "k-stale-1")"
cp "$WORK/submit.json" "$PAYLOADS/submit-stale.json"
expect_eq "拒收的错误码是 PREFLIGHT_STALE" "PREFLIGHT_STALE" \
  "$(jget "$WORK/submit.json" 'd["error"]["code"]')"
JOBS_AFTER=$(api "$JAR" "$CSRF" GET "/api/v1/jobs?limit=200" >/dev/null; resp_get 'len(d["data"]["jobs"])')
# 作业**是**落库的：提交先写 jobs（queued），再向代理登记操作；代理在受理时验不过，
# 任务就地变成 failed，HTTP 回 409。这条失败任务必须留在任务中心里可追溯——
# 静默丢弃才是错的。代理那侧没有建立 operation（没有第二次执行的可能）。
expect_eq "过期预检留下一条失败任务（不是静默丢弃）" "$((JOBS_BEFORE + 1))" "$JOBS_AFTER"
# 不按“第一条”取：列表按 created_at 秒级排序，同一秒里的两条任务先后由 id 决定。
# 断言要认的是这条任务的内容，不是它的位置。
expect_eq "这条任务的终态是 failed、步骤是 rejected（代理在受理时拒收）" "True" \
  "$(resp_get 'any(j["status"]=="failed" and j["step"]=="rejected" and j["error"].get("code")=="PREFLIGHT_STALE" for j in d["data"]["jobs"])')"
expect_eq "别人的修改没有被覆盖" "theirs" "$(cat "$ROOT_RW/conflict.conf")"
note "runner 内的 revalidate 步骤（第二道拦截）要真实竞态才能触发，本轮未执行"

echo
echo "== 7. file.mkdir =="
expect_eq "建目录" "succeeded" \
  "$(run_action file.mkdir '{"root_id":"rw","relative_path":"newdir"}' "" "k-mkdir-1")"
cp "$WORK/job.json" "$PAYLOADS/job-mkdir.json"
expect_eq "mkdir outcome 是 verified" "verified" "$(joutcome)"
expect_eq "mkdir 读回类型是 dir" "dir" "$(jobserved type)"
expect_eq "目录真的在盘上" "yes" "$([ -d "$ROOT_RW/newdir" ] && echo yes || echo no)"
# 锁按资源命名：同一条路径上的动作互斥，不同路径不互斥。
expect_eq "mkdir 的锁是 file:rw:/newdir" "['file:rw:/newdir']" \
  "$( { preflight file.mkdir '{"root_id":"rw","relative_path":"newdir"}' >/dev/null; pget 'd["data"]["locks"]'; } )"

expect_eq "重复建同名目录：预检阻塞" "200" \
  "$(preflight file.mkdir '{"root_id":"rw","relative_path":"newdir"}')"
cp "$WORK/preflight.json" "$PAYLOADS/preflight-mkdir-exists.json"
expect_eq "重复建的阻塞码是 TARGET_EXISTS" "TARGET_EXISTS" "$(pblocker 0)"
expect_eq "被阻塞的预检提交任务会被拒（409）" "submit:409" \
  "$(submit_and_wait "$(pget 'd["data"]["preflight_id"]')" "" "k-mkdir-dup")"
expect_eq "被拒的错误码是 DEPENDENCY_BLOCKED" "DEPENDENCY_BLOCKED" \
  "$(jget "$WORK/submit.json" 'd["error"]["code"]')"

expect_eq "父目录不存在：预检阻塞" "200" \
  "$(preflight file.mkdir '{"root_id":"rw","relative_path":"ghost/child"}')"
cp "$WORK/preflight.json" "$PAYLOADS/preflight-mkdir-parent.json"
expect_eq "父目录缺失的阻塞码是 PARENT_MISSING" "PARENT_MISSING" "$(pblocker 0)"

echo
echo "== 8. file.copy / file.move =="
expect_eq "同根复制" "succeeded" \
  "$(run_action file.copy \
     '{"source_root_id":"rw","source_path":"notes.txt","target_root_id":"rw","target_path":"notes-copy.txt"}' \
     "" "k-copy-1")"
cp "$WORK/job.json" "$PAYLOADS/job-copy.json"
expect_eq "复制 outcome 是 verified" "verified" "$(joutcome)"
expect_eq "复制后目标内容一致" "$(cat "$ROOT_RW/notes.txt")" "$(cat "$ROOT_RW/notes-copy.txt" 2>/dev/null)"
expect_eq "复制后源还在" "yes" "$([ -e "$ROOT_RW/notes.txt" ] && echo yes || echo no)"

expect_eq "目标已存在：复制阻塞" "200" \
  "$(preflight file.copy \
     '{"source_root_id":"rw","source_path":"notes.txt","target_root_id":"rw","target_path":"notes-copy.txt"}')"
cp "$WORK/preflight.json" "$PAYLOADS/preflight-copy-exists.json"
expect_eq "复制到已存在目标的阻塞码" "TARGET_EXISTS" "$(pblocker 0)"

expect_eq "源不存在：复制阻塞" "200" \
  "$(preflight file.copy \
     '{"source_root_id":"rw","source_path":"ghost.txt","target_root_id":"rw","target_path":"x.txt"}')"
expect_eq "复制源缺失的阻塞码" "TARGET_MISSING" "$(pblocker 0)"

expect_eq "含符号链接的目录：整棵拒绝" "200" \
  "$(preflight file.copy \
     '{"source_root_id":"rw","source_path":"tree-link","target_root_id":"rw","target_path":"tree-copy"}')"
cp "$WORK/preflight.json" "$PAYLOADS/preflight-copy-tree.json"
expect_eq "树里有符号链接的阻塞码" "TREE_UNSUPPORTED" "$(pblocker 0)"
expect_eq "被拒之后目标没有被创建" "no" "$([ -e "$ROOT_RW/tree-copy" ] && echo yes || echo no)"

expect_eq "同根 move 的预检" "200" \
  "$(preflight file.move \
     '{"source_root_id":"rw","source_path":"notes-copy.txt","target_root_id":"rw","target_path":"notes-moved.txt"}')"
expect_eq "同根 move 的 impact 是 rename（原子改名）" "rename" "$(pimpact 0)"
expect_eq "同根 move 执行成功" "succeeded" \
  "$(submit_and_wait "$(pget 'd["data"]["preflight_id"]')" "" "k-move-1")"
expect_eq "move 后源消失" "no" "$([ -e "$ROOT_RW/notes-copy.txt" ] && echo yes || echo no)"
expect_eq "move 后目标在" "yes" "$([ -e "$ROOT_RW/notes-moved.txt" ] && echo yes || echo no)"

expect_eq "跨根 move 的预检" "200" \
  "$(preflight file.move \
     '{"source_root_id":"rw","source_path":"notes-moved.txt","target_root_id":"rw2","target_path":"target/arrived.txt"}')"
cp "$WORK/preflight.json" "$PAYLOADS/preflight-move-cross.json"
expect_eq "跨根 move 的 impact 是 not_atomic" "not_atomic" "$(pimpact 0)"
expect_eq "跨根 move 执行成功" "succeeded" \
  "$(submit_and_wait "$(pget 'd["data"]["preflight_id"]')" "" "k-move-2")"
cp "$WORK/job.json" "$PAYLOADS/job-move-cross.json"
expect_eq "跨根 move 后源消失" "no" "$([ -e "$ROOT_RW/notes-moved.txt" ] && echo yes || echo no)"
expect_eq "跨根 move 后目标在另一个根里" "edited" "$(cat "$ROOT_RW2/target/arrived.txt" 2>/dev/null)"
expect_eq "跨根 move 的结果里带 source" "rw:notes-moved.txt" "$(jdata source)"

expect_eq "移动到只读根：阻塞" "200" \
  "$(preflight file.move \
     '{"source_root_id":"rw","source_path":"modes.conf","target_root_id":"ro","target_path":"moved.conf"}')"
expect_eq "只读根的阻塞码是 ROOT_READ_ONLY" "ROOT_READ_ONLY" "$(pblocker 0)"

echo
echo "== 9. file.delete：再认证、递归短语与读回 =="
# 非空目录不递归：先拦住，而不是删一半。
expect_eq "非空目录非递归：预检阻塞" "200" \
  "$(preflight file.delete '{"root_id":"rw","relative_path":"docs","recursive":false}')"
cp "$WORK/preflight.json" "$PAYLOADS/preflight-delete-nonempty.json"
expect_eq "非空目录的阻塞码" "DIRECTORY_NOT_EMPTY" "$(pblocker 0)"

# 单文件删除需要再认证。此时会话里还没有再认证。
expect_eq "单文件删除预检" "200" \
  "$(preflight file.delete '{"root_id":"rw","relative_path":"created.conf","recursive":false}')"
expect_eq "delete 要求再认证" "True" "$(pget 'd["data"]["requires_reauth"]')"
PF_DEL=$(pget 'd["data"]["preflight_id"]')
expect_eq "未再认证提交删除：403" "submit:403" "$(submit_and_wait "$PF_DEL" "" "k-del-noauth")"
expect_eq "未再认证的错误码" "REAUTH_REQUIRED" "$(jget "$WORK/submit.json" 'd["error"]["code"]')"

# 再认证：密码进 reauth，不是再登录一次。
expect_eq "再认证" "200" \
  "$(api "$JAR" "$CSRF" POST /api/v1/auth/reauth "{\"password\":\"$PASSWORD\"}")"
cp "$WORK/resp.json" "$PAYLOADS/reauth.json"
expect_eq "再认证给出有效期" "True" "$(resp_get 'bool(d["data"]["reauth_expires_at"])')"
# 同一份预检还能用：被拒的提交没有烧掉它，也没有产生一次执行。
expect_eq "再认证后同一份预检可以执行" "succeeded" "$(submit_and_wait "$PF_DEL" "" "k-del-ok")"
cp "$WORK/job.json" "$PAYLOADS/job-delete-file.json"
expect_eq "删除 outcome 是 verified" "verified" "$(joutcome)"
expect_eq "删除后读回 exists=false" "False" "$(jobserved exists)"
expect_eq "删除后文件真的不在了" "no" "$([ -e "$ROOT_RW/created.conf" ] && echo yes || echo no)"
expect_eq "删单个文件的条目数是 1" "1" "$(jobserved entries)"

# 递归删除：短语首词 DELETE，错短语不算数。
expect_eq "递归删除预检" "200" \
  "$(preflight file.delete '{"root_id":"rw","relative_path":"docs","recursive":true}')"
cp "$WORK/preflight.json" "$PAYLOADS/preflight-delete-recursive.json"
expect_eq "递归删除带 RECURSIVE_DELETE 告警" "RECURSIVE_DELETE" "$(pwarn 0)"
expect_eq "递归删除要求短语" "True" "$(pget 'd["data"]["confirmation_phrase"] is not None')"
PHRASE=$(pget 'd["data"]["confirmation_phrase"]')
expect_eq "短语首词是 DELETE" "True" "$(case "$PHRASE" in DELETE\ *) echo True;; *) echo False;; esac)"
note "确认短语：$PHRASE"
PF_RD=$(pget 'd["data"]["preflight_id"]')
expect_eq "错短语提交：400" "submit:400" "$(submit_and_wait "$PF_RD" "DELETE 000000" "k-del-bad")"
expect_eq "错短语的错误码" "CONFIRMATION_MISMATCH" "$(jget "$WORK/submit.json" 'd["error"]["code"]')"
api "$JAR" "$CSRF" GET '/api/v1/jobs?limit=5' >/dev/null
# 同一秒里的两条任务顺序由 id 决定，所以按内容找，不按位置找。
expect_eq "错短语的任务在任务中心里是 failed（步骤 rejected）" "True" \
  "$(resp_get 'any(j["status"]=="failed" and j["step"]=="rejected" and j["error"].get("code")=="CONFIRMATION_MISMATCH" for j in d["data"]["jobs"])')"
expect_eq "错短语之后目录还在" "yes" "$([ -d "$ROOT_RW/docs" ] && echo yes || echo no)"

expect_eq "重新预检（错短语烧掉了上一份）" "200" \
  "$(preflight file.delete '{"root_id":"rw","relative_path":"docs","recursive":true}')"
PHRASE2=$(pget 'd["data"]["confirmation_phrase"]')
expect_eq "正确短语递归删除" "succeeded" \
  "$(submit_and_wait "$(pget 'd["data"]["preflight_id"]')" "$PHRASE2" "k-del-recursive")"
cp "$WORK/job.json" "$PAYLOADS/job-delete-recursive.json"
expect_eq "递归删除 outcome 是 verified" "verified" "$(joutcome)"
expect_eq "递归删除后目录不在" "no" "$([ -e "$ROOT_RW/docs" ] && echo yes || echo no)"
expect_eq "递归删除的条目数（1 文件 + 1 子目录 + 子目录里 1 文件 + 目录自身）" "4" "$(jobserved entries)"
expect_eq "递归删除的字节数是 4" "4" "$(jobserved bytes)"

echo
echo "== 10. file.chmod / file.chown =="
# 再认证的有效期是 5 分钟；这一节的所有动作都要求它，先续一次，
# 免得断言因为脚本跑得慢而变成“有时通过”。重复再认证本身也是合法操作。
expect_eq "再认证（为 chmod/chown 续期）" "200" \
  "$(api "$JAR" "$CSRF" POST /api/v1/auth/reauth "{\"password\":\"$PASSWORD\"}")"
expect_eq "chmod 0640" "succeeded" \
  "$(run_action file.chmod '{"root_id":"rw","relative_path":"modes.conf","mode":"0640"}' "" "k-chmod-1")"
cp "$WORK/job.json" "$PAYLOADS/job-chmod.json"
expect_eq "chmod outcome 是 verified" "verified" "$(joutcome)"
expect_eq "chmod 后盘上是 0640" "640" "$(stat -c '%a' "$ROOT_RW/modes.conf")"
expect_eq "chmod 读回的 mode" "0640" "$(jobserved mode)"

expect_eq "再改成同一个权限：带 NO_CHANGE 告警" "200" \
  "$(preflight file.chmod '{"root_id":"rw","relative_path":"modes.conf","mode":"0640"}')"
cp "$WORK/preflight.json" "$PAYLOADS/preflight-chmod-noop.json"
expect_eq "无变化的告警码" "NO_CHANGE" "$(pwarn 0)"

expect_eq "setuid 权限在参数层拒绝（400）" "400" \
  "$(preflight file.chmod '{"root_id":"rw","relative_path":"modes.conf","mode":"4755"}')"
cp "$WORK/resp.json" "$PAYLOADS/preflight-chmod-setuid.json"
expect_eq "setuid 的错误码" "VALIDATION_ERROR" "$(resp_get 'd["error"]["code"]')"
expect_eq "被拒之后权限没变" "640" "$(stat -c '%a' "$ROOT_RW/modes.conf")"

expect_eq "chown 在预检就阻塞" "200" \
  "$(preflight file.chown '{"root_id":"rw","relative_path":"modes.conf","managed_account_id":3}')"
cp "$WORK/preflight.json" "$PAYLOADS/preflight-chown.json"
expect_eq "chown 的阻塞码" "MANAGED_ACCOUNTS_UNAVAILABLE" "$(pblocker 0)"
note "$(pget 'd["data"]["blockers"][0]["message"]')"

echo
echo "== 11. 路径形状与越界（写动作） =="
expect_eq "'../escape' 被参数层拒绝" "400" \
  "$(preflight file.mkdir '{"root_id":"rw","relative_path":"../escape"}')"
cp "$WORK/resp.json" "$PAYLOADS/preflight-path-dotdot.json"
expect_eq "'../escape' 的错误码" "VALIDATION_ERROR" "$(resp_get 'd["error"]["code"]')"
expect_eq "绝对路径被参数层拒绝" "400" \
  "$(preflight file.mkdir '{"root_id":"rw","relative_path":"/etc/shadow"}')"
expect_eq "绝对路径的错误码" "VALIDATION_ERROR" "$(resp_get 'd["error"]["code"]')"
expect_eq "保留目录 .upanel 被拒绝" "400" \
  "$(preflight file.mkdir '{"root_id":"rw","relative_path":".upanel/evil"}')"
expect_eq "保留目录的错误码" "VALIDATION_ERROR" "$(resp_get 'd["error"]["code"]')"
expect_eq "越界没有被执行" "no" "$([ -e "$ROOT_RW/.upanel/evil" ] && echo yes || echo no)"
expect_eq "指向根外的符号链接：写文本阻塞" "200" \
  "$(preflight file.write_text '{"root_id":"rw","relative_path":"link-out","content":"pwn\n"}')"
cp "$WORK/preflight.json" "$PAYLOADS/preflight-write-symlink.json"
expect_eq "符号链接的阻塞码" "TARGET_NOT_REGULAR" "$(pblocker 0)"
expect_eq "根外的秘密没有被写" "do-not-leak" "$(cat "$WORK/outside/secret.txt")"

echo
echo "== 12. 只读根：所有写动作都是 ROOT_READ_ONLY =="
expect_eq "只读根建目录" "200" "$(preflight file.mkdir '{"root_id":"ro","relative_path":"x"}')"
expect_eq "只读根 mkdir 的阻塞码" "ROOT_READ_ONLY" "$(pblocker 0)"
expect_eq "只读根写文本" "200" \
  "$(preflight file.write_text '{"root_id":"ro","relative_path":"ro.txt","content":"x\n"}')"
expect_eq "只读根 write_text 的阻塞码" "ROOT_READ_ONLY" "$(pblocker 0)"
expect_eq "只读根 chmod（已再认证）" "200" \
  "$(preflight file.chmod '{"root_id":"ro","relative_path":"ro.txt","mode":"0600"}')"
expect_eq "只读根 chmod 的阻塞码" "ROOT_READ_ONLY" "$(pblocker 0)"
expect_eq "只读根 chown" "200" \
  "$(preflight file.chown '{"root_id":"ro","relative_path":"ro.txt","managed_account_id":3}')"
# 账号模块的闸门在根只读检查之前：chown 现在根本没有“改谁”的答案，
# 所以它先报 MANAGED_ACCOUNTS_UNAVAILABLE，而不是 ROOT_READ_ONLY。
expect_eq "只读根 chown 的阻塞码（账号闸门更靠前）" "MANAGED_ACCOUNTS_UNAVAILABLE" "$(pblocker 0)"
expect_eq "只读根上的文件没被改动" "ro" "$(cat "$ROOT_RO/ro.txt")"

echo
echo "== 13. 任务中心 =="
expect_eq "任务列表" "200" "$(api "$JAR" "$CSRF" GET '/api/v1/jobs?limit=100')"
cp "$WORK/resp.json" "$PAYLOADS/jobs-list.json"
expect_eq "本轮七类动作都在任务中心里" "True" \
  "$(resp_get '{"file.mkdir","file.write_text","file.commit_upload","file.copy","file.move","file.delete","file.chmod"} <= set(j["action"] for j in d["data"]["jobs"])')"
expect_eq "每条任务都有 job_id 与状态" "True" \
  "$(resp_get 'all(j["job_id"] and j["status"] for j in d["data"]["jobs"])')"
expect_eq "失败的任务都带错误码" "True" \
  "$(resp_get 'all((j["error"] or {}).get("code") for j in d["data"]["jobs"] if j["status"] == "failed")')"
DONE_JOB=$(jobget 'd["data"]["job_id"]')
expect_eq "终态任务不可取消（409）" "409" \
  "$(api "$JAR" "$CSRF" POST "/api/v1/jobs/$DONE_JOB/cancel" '{}')"
expect_eq "不可取消的错误码" "VERSION_CONFLICT" "$(resp_get 'd["error"]["code"]')"

echo
echo "== 14. 权限：未登录 / viewer / admin =="
expect_eq "未登录创建上传是 401" "401" \
  "$(curl -s -o "$PAYLOADS/err-noauth-upload.json" -w '%{http_code}' \
     -X POST "$BASE$FILES/uploads" -H 'Content-Type: application/json' \
     -d '{"root_id":"rw","path":"x","size_bytes":"1"}')"
expect_eq "未登录的错误码" "UNAUTHENTICATED" \
  "$(jget "$PAYLOADS/err-noauth-upload.json" 'd["error"]["code"]')"
expect_eq "未登录预检是 401" "401" \
  "$(curl -s -o "$PAYLOADS/err-noauth-preflight.json" -w '%{http_code}' \
     -X POST "$BASE/api/v1/operations/preflight" -H 'Content-Type: application/json' \
     -d '{"action":"file.mkdir","params":{"root_id":"rw","relative_path":"x"}}')"
expect_eq "viewer 创建上传是 403" "403" \
  "$(curl -s -b "$VJAR" -o "$PAYLOADS/err-viewer-upload.json" -w '%{http_code}' \
     -X POST "$BASE$FILES/uploads" -H 'Content-Type: application/json' \
     -d '{"root_id":"rw","path":"x","size_bytes":"1"}')"
expect_eq "viewer 的错误码" "FORBIDDEN" "$(jget "$PAYLOADS/err-viewer-upload.json" 'd["error"]["code"]')"
expect_eq "viewer 预检是 403" "403" \
  "$(curl -s -b "$VJAR" -o "$PAYLOADS/err-viewer-preflight.json" -w '%{http_code}' \
     -X POST "$BASE/api/v1/operations/preflight" -H 'Content-Type: application/json' \
     -d '{"action":"file.mkdir","params":{"root_id":"rw","relative_path":"x"}}')"
expect_eq "viewer 提交任务是 403" "403" \
  "$(curl -s -b "$VJAR" -o "$PAYLOADS/err-viewer-job.json" -w '%{http_code}' \
     -X POST "$BASE/api/v1/jobs" -H 'Content-Type: application/json' \
     -H 'Idempotency-Key: k-viewer' -d '{"preflight_id":"pf_x"}')"
expect_eq "viewer 查上传也是 403（整组 admin）" "403" \
  "$(curl -s -b "$VJAR" -o "$PAYLOADS/err-viewer-getupload.json" -w '%{http_code}' \
     "$BASE$FILES/uploads/$UP1")"

echo
echo "== 15. 回归：写动作之外仍然正常 =="
expect_eq "健康检查" "200" "$(curl -s -o /dev/null -w '%{http_code}' "$BASE/api/v1/health/live")"
expect_eq "系统概要" "200" "$(api "$JAR" "$CSRF" GET /api/v1/system/summary)"
expect_eq "日志查询（P05.3）" "200" "$(api "$JAR" "$CSRF" GET '/api/v1/logs?limit=10')"
expect_eq "存储设备列表（P04）" "200" "$(api "$JAR" "$CSRF" GET /api/v1/storage/devices)"
expect_eq "服务列表（P05.1）" "200" "$(api "$JAR" "$CSRF" GET /api/v1/services)"
expect_eq "进程列表（P05.2）" "200" "$(api "$JAR" "$CSRF" GET '/api/v1/processes?limit=5')"
expect_eq "文件根列表（P06.1）" "200" "$(api "$JAR" "$CSRF" GET "$FILES/roots")"
cp "$WORK/resp.json" "$PAYLOADS/files-roots.json"
expect_eq "三个根都在" "['ro', 'rw', 'rw2']" \
  "$(resp_get 'sorted(r["root_id"] for r in d["data"]["roots"])')"
expect_eq "只有两个根可写" "['rw', 'rw2']" \
  "$(resp_get 'sorted(r["root_id"] for r in d["data"]["roots"] if r["writable"])')"
expect_eq "列举可写根" "200" "$(api "$JAR" "$CSRF" GET "$FILES/list?root_id=rw&path=")"
cp "$WORK/resp.json" "$PAYLOADS/files-list.json"
expect_eq "保留目录不出现在列表里" "False" \
  "$(resp_get 'any(e["name"] == ".upanel" for e in d["data"]["entries"])')"
expect_eq "根里的普通文件仍然可读（P06.1）" "200" \
  "$(api "$JAR" "$CSRF" GET "$FILES/text?root_id=rw&path=notes.txt")"
expect_eq "读回的内容是本轮写进去的" "True" "$(resp_get 'd["data"]["content"] == "edited\n"')"

echo
echo "== 16. 载荷归档 =="
cp "$WORK/preflight.json" "$PAYLOADS/final-preflight.json" 2>/dev/null
cp "$WORK/job.json" "$PAYLOADS/final-job.json" 2>/dev/null
install -m 0644 "$PAYLOAD" "$PAYLOADS/upload-payload.bin" 2>/dev/null
python3 - "$PAYLOADS" "$WORK/logs/agent.log" "$WORK/logs/web.log" <<'PY'
import json, os, sys
out, agent_log, web_log = sys.argv[1], sys.argv[2], sys.argv[3]
summary = {"payloads": sorted(os.listdir(out))}
for name, path in (("agent.log", agent_log), ("web.log", web_log)):
    try:
        lines = open(path, encoding="utf-8", errors="replace").read().splitlines()
    except OSError:
        lines = []
    keys = ("已创建上传暂存", "暂存上传收完", "已提交上传", "已删除文件", "已移动文件",
            "操作完成", "拒绝", "error", "ERROR")
    summary[name] = [line for line in lines if any(k in line for k in keys)][-60:]
json.dump(summary, open(os.path.join(out, "log-summary.json"), "w", encoding="utf-8"),
          ensure_ascii=False, indent=2)
print("载荷 %d 个，日志摘要 %d 行 / %d 行" %
      (len(summary["payloads"]), len(summary["agent.log"]), len(summary["web.log"])))
PY
if [ -f "$PAYLOADS/log-summary.json" ]; then
  pass "载荷与日志摘要已归档（$PAYLOADS）"
else
  fail "载荷与日志摘要已归档" "log-summary.json 没生成"
fi

echo
printf '通过 %d 项，失败 %d 项，未执行 %d 项\n' "$PASSED" "$FAILED" "$NOTRUN"
if [ "$FAILED" -ne 0 ]; then
  printf '失败项：%s\n' "$FAILED_NAMES"
  exit 1
fi
exit 0
