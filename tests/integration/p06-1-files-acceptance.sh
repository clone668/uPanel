#!/bin/sh
# P06.1 Linux 验收脚本：受管理文件根、句柄级路径约束、列表/文本/下载，
# 以及路径穿越与符号链接的逃逸测试——在一台真的 Ubuntu 上逐条验证。
#
# 运行位置：Ubuntu 24.04 验收 VM 内，**必须以 root 运行**
# （代理只能在 root 下启动；符号链接、硬链接、FIFO、子挂载都要有人造它们）。
# 不依赖 Go 工具链：用宿主交叉编译好的 Linux 二进制。
#
# 用法：
#   sudo BIN_DIR=/tmp/upanel-bin sh tests/integration/p06-1-files-acceptance.sh
#
# 可选环境变量：
#   PORT=19461      Web 监听端口
#   BIGDIR=50010    truncated 用例的目录项数（必须大于 MaxListScan=50000）
#
# 退出码：0 全部通过；1 有失败项；2 环境不满足（非 root）。
#
# 覆盖的验收点（STAGES §8 P06.1、CONTRACTS §11、PLAN §15）：
#   1. 根只来自配置：files.roots 只回登记的根，未登记 root_id 是 404
#   2. 配置里 path="/" 的根被拒：接口与界面都无法变成“可管理整个服务器”
#   3. `../..`、绝对路径、空段、'.' 段、超长路径：形状层拒绝（422/400），不进入内核解析
#   4. 符号链接不跟随：列表里标 symlink，列举/读取/下载一律 422 PATH_OUTSIDE_ROOT
#   5. magic link 跳板（根内指向 /proc/self/fd 的链接）同样 422
#   6. 硬链接（nlink>1）拒绝：422 PATH_OUTSIDE_ROOT
#   7. 特殊文件（FIFO）与目录：不是普通文件，400 VALIDATION_ERROR
#      ——FIFO 那一行同时证明“打开根内非普通文件不会把代理挂住”
#   8. 文本：内容/大小/version 自洽；>1 MiB 是 413；非 UTF-8 或含 NUL 是 422 FILE_NOT_TEXT
#   9. expected_version 不一致是 409 VERSION_CONFLICT（版本在**已打开句柄**上核验）
#  10. 下载：octet-stream + nosniff + no-store + Content-Disposition（ASCII + RFC 5987）
#      + Content-Length，字节与原文件 sha256 一致
#  11. 分页：页大小 200、两页无重复、最后一页 next_cursor 为 null（不是空串）
#  12. 超大目录 truncated=true（如实标记，不静默截断）
#  13. 根内子挂载（tmpfs）被 RESOLVE_NO_XDEV 拒绝：422
#  14. 根被删掉重建（dev/inode 变了）→ 409 ROOT_UNAVAILABLE，不退化成“新目录照用”
#  15. 权限：未登录 401、viewer 403（含下载）、admin 200
#  16. 回归：概览/存储/服务/日志在加了文件模块之后仍然正常
#
# 明确不覆盖（NOT RUN，原因写在这里，不在脚本里假装通过）：
#   - 浏览器渲染与文件页交互：VM 上没有浏览器。页面级证据在宿主上用真实载荷回放
#     （web/src/components/files.test.ts 读 web/src/fixtures/p06-1-vm/）。
#   - URL 里无法表达的 NUL 路径：由 internal/files 的单元用例覆盖。
#   - 内核 < 5.6（没有 openat2）时的 501 回退：验收内核是 6.8，造不出来。

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
VALIDATE="$HERE/p06-1-payload-validate.py"

WORK=${WORK:-/var/tmp/upanel-accept-p06-1}
PAYLOADS=$WORK/payloads
RUN_DIR=/run/upanel
SOCK=$RUN_DIR/agent.sock
SYS_CFG=/etc/upanel/upanel.json
AGENT_DB_DIR=/var/lib/upanel-agent
AGENT_DB=$AGENT_DB_DIR/agent.db
KEY_FILE=/etc/upanel/agent.key
WEB_DB_DIR=$WORK/webdata
WEB_DB=$WEB_DB_DIR/upanel.db
PORT=${PORT:-19461}
BASE="http://127.0.0.1:$PORT"
ORIGIN="$BASE"
WEB_USER=upanel-web
PASSWORD='vm-correct-horse-9'
VIEWER_PASSWORD='vm-correct-horse-8'

ROOT=$WORK/root               # 主根：普通文件、符号链接、FIFO、硬链接
ROOT_FLIP=$WORK/flip          # 验证“根被删掉重建”
ROOT_SHM=/dev/shm/upanel-p061 # 大目录与子挂载用例（tmpfs 上造 5 万个文件才够快）
BIGDIR=${BIGDIR:-50010}
PAGE_EXPECT=200

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
jget() {
  python3 -c 'import json,sys
try:
    d=json.load(open(sys.argv[1], encoding="utf-8"))
except Exception:
    print("")
    raise SystemExit
print(eval(sys.argv[2]))' "$1" "$2" 2>/dev/null
}
resp_get() { jget "$WORK/resp.json" "$1"; }

cleanup() {
  stop_web
  stop_agent
  umount "$ROOT_SHM/submount" 2>/dev/null
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

# get <载荷名> <端点> <查询串>：GET 并按名字保存载荷（校验程序按名字找文件，
# 缺一个就是 FAIL）。端点是 /roots、/list、/text 之一——**必须显式写**，这个脚本
# 曾经在这里漏掉端点，结果每一行都命中 /files 本身、全部报 404，而断言恰好
# 都期望非 200，差点把“接口不存在”当成“越界被拒绝”。
get() {
  _code=$(api "$JAR" "$CSRF" GET "$FILES$2$3")
  cp "$WORK/resp.json" "$PAYLOADS/$1" 2>/dev/null
  printf '%s' "$_code"
}

# get_anon <载荷名> <端点> <查询串>：不带 Cookie。
get_anon() {
  _code=$(curl -s -o "$WORK/resp.json" -w '%{http_code}' "$BASE$FILES$2$3")
  cp "$WORK/resp.json" "$PAYLOADS/$1" 2>/dev/null
  printf '%s' "$_code"
}

# get_viewer <载荷名> <端点> <查询串>：用只读账号。
get_viewer() {
  _code=$(curl -s -b "$VJAR" -o "$WORK/resp.json" -w '%{http_code}' "$BASE$FILES$2$3")
  cp "$WORK/resp.json" "$PAYLOADS/$1" 2>/dev/null
  printf '%s' "$_code"
}

# dl <载荷前缀> <查询串>：下载，头部进 <前缀>.headers，字节进 <前缀>.bin。
dl() {
  curl -s -b "$JAR" -D "$PAYLOADS/$1.headers" -o "$PAYLOADS/$1.bin" -w '%{http_code}' \
    "$BASE$FILES/download$2"
}

urlenc() { python3 -c 'import sys,urllib.parse;print(urllib.parse.quote(sys.argv[1],safe=""))' "$1"; }
# head_of <载荷前缀> <头部名>：大小写不敏感地取最后一个同名头的值（去掉 CR）。
head_of() {
  grep -i "^[[:space:]]*$2:" "$PAYLOADS/$1.headers" 2>/dev/null | tail -1 \
    | sed 's/^[^:]*:[[:space:]]*//' | tr -d '\r'
}
sha_of() { sha256sum "$1" 2>/dev/null | cut -d' ' -f1; }

echo "== 0. 环境 =="
for b in "$AGENT" "$WEB" "$CTL"; do
  if [ -x "$b" ]; then pass "二进制存在且可执行 $(basename "$b")"; else fail "二进制存在且可执行 $(basename "$b")" "$b"; fi
done
if [ -f "$VALIDATE" ]; then pass "载荷校验程序存在"; else fail "载荷校验程序存在" "$VALIDATE"; fi
for t in python3 curl runuser ss stat sha256sum mount mkfifo install; do
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

rm -rf "$WORK" "$ROOT_SHM"
mkdir -p "$WORK/logs" "$PAYLOADS" "$WEB_DB_DIR"
chmod 0755 "$WORK" "$PAYLOADS" "$WORK/logs"
chown -R "$WEB_USER" "$WEB_DB_DIR"
uname -r >"$WORK/logs/kernel.txt" 2>/dev/null || true

echo
echo "== 1. 造真实对象（文件、符号链接、硬链接、FIFO、大文件、超大目录） =="
install -d -m 0755 /etc/upanel
install -d -m 0700 "$AGENT_DB_DIR"
install -d -m 0755 "$RUN_DIR"
mkdir -p "$ROOT/sub" "$ROOT_FLIP" "$ROOT_SHM"
printf 'Port 22\nPermitRootLogin no\n' >"$ROOT/sub/sshd_config"
printf 'upanel\n' >"$ROOT/hostname"
printf 'old\n' >"$ROOT/versioned.conf"
printf 'flip\n' >"$ROOT_FLIP/a.txt"
# 根外的秘密：任何路径穿越或跟随符号链接的漏洞都会把它暴露出来。
mkdir -p "$WORK/outside"
printf 'do-not-leak\n' >"$WORK/outside/secret.txt"
ln -sf "$WORK/outside/secret.txt" "$ROOT/link-secret"
ln -sfn "$WORK/outside" "$ROOT/link-dir"
ln -sfn /proc/self/fd "$ROOT/link-magic"
mkfifo "$ROOT/fifo"
# 硬链接：nlink=2 的普通文件必须被拒（这个 inode 在根外可能还有名字）。
printf 'shared\n' >"$ROOT/hard-a.conf"
ln "$ROOT/hard-a.conf" "$ROOT/hard-b.conf"
# 1 MiB + 1 字节：文本读取上限之外。
python3 -c "open('$ROOT/big.log','w').write('a'*(1024*1024+1))"
# 用 python 写非法 UTF-8 与含 NUL 的字节：POSIX sh 的 printf 不解释 \x 转义
# （dash 会把反斜杠原样写进文件，那样文件其实是合法 UTF-8，用例永远测不到拒绝）。
python3 -c "
import os
root = '$ROOT'
open(os.path.join(root, 'binary.dat'), 'wb').write(b'\xff\xfe\x00binary')
open(os.path.join(root, 'nul.dat'), 'wb').write(b'abc\x00def')
"
# 分页：pages/ 里放 250 个空文件（页大小 200，所以是满一页 + 半页）。
# 刻意放在子目录里：根目录本身留着十几个名字，界面首屏就是它，
# 把 250 个文件堆在根上会让“根里有什么”这条断言看不出东西。
python3 -c "
import os
root = os.path.join('$ROOT', 'pages')
os.makedirs(root, exist_ok=True)
for i in range(250):
    open(os.path.join(root, 'page-%03d.txt' % i), 'w').close()
"
# 超大目录：必须超过 internal/files 的 MaxListScan。
python3 -c "
import os
for i in range($BIGDIR):
    open(os.path.join('$ROOT_SHM','e%05d' % i),'w').close()
"
expect_eq "根内准备了 250 个分页文件" "250" "$(find "$ROOT/pages" -maxdepth 1 -name 'page-*.txt' | wc -l | tr -d ' ')"
note "超大目录项数：$(find "$ROOT_SHM" -maxdepth 1 -type f | wc -l | tr -d ' ')"
ls -la "$ROOT" >"$WORK/logs/root-listing.txt" 2>&1

echo
echo "== 2. 配置与启动（代理 root、Web 非 root） =="
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
      "roots": [
        { "id": "work", "name": "验收根", "path": "$ROOT", "writable": false },
        { "id": "flip", "name": "可替换根", "path": "$ROOT_FLIP", "writable": false },
        { "id": "shm",  "name": "大目录根", "path": "$ROOT_SHM", "writable": false }
      ]
    }
  },
  "database": { "path": "$WEB_DB", "busy_timeout_ms": 5000 },
  "log": { "level": "info", "format": "json" }
}
JSON
chmod 0644 "$SYS_CFG"

# 配置里出现 path="/" 的根必须被拒：那是“可管理整个服务器所有目录”的唯一入口。
# 从刚写好的配置派生（只改 files.roots），这样这一条失败只可能是文件根引起的，
# 而不是“别的必填字段没写”。
python3 - "$SYS_CFG" "$WORK/root-slash.json" <<'PY'
import json, sys
cfg = json.load(open(sys.argv[1], encoding="utf-8"))
cfg["agent"]["socket_path"] = sys.argv[2].replace("root-slash.json", "slash.sock")
cfg["agent"]["database_path"] = sys.argv[2].replace("root-slash.json", "slash.db")
cfg["agent"]["files"] = {"roots": [{"id": "all", "name": "全部", "path": "/"}]}
json.dump(cfg, open(sys.argv[2], "w", encoding="utf-8"), ensure_ascii=False)
PY
"$AGENT" --config "$WORK/root-slash.json" >"$WORK/slash.out" 2>&1
SLASH_RC=$?
cp "$WORK/slash.out" "$PAYLOADS/root-slash-rejected.txt" 2>/dev/null
expect_eq "配置拒绝 path=\"/\" 的文件根（无法管理整个服务器）" "2" "$SLASH_RC"
note "$(tail -1 "$WORK/slash.out" 2>/dev/null)"

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

# 能力表：给什么就报什么。少报等于隐藏功能，多报等于编造。
runuser -u "$WEB_USER" -- curl -s --unix-socket "$SOCK" \
  http://localhost/internal/v1/capabilities >"$PAYLOADS/agent-capabilities.json" 2>/dev/null
expect_eq "能力表登记了 files.roots" "True" \
  "$(jget "$PAYLOADS/agent-capabilities.json" '"files.roots" in d["data"]["supported_queries"]')"
expect_eq "能力表登记了 files.list" "True" \
  "$(jget "$PAYLOADS/agent-capabilities.json" '"files.list" in d["data"]["supported_queries"]')"
expect_eq "能力表登记了 files.text" "True" \
  "$(jget "$PAYLOADS/agent-capabilities.json" '"files.text" in d["data"]["supported_queries"]')"
note "代理登记的查询：$(jget "$PAYLOADS/agent-capabilities.json" 'sorted(d["data"]["supported_queries"])')"

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
expect_eq "agent.sock 归 root:$WEB_USER" "root:$WEB_USER" "$(stat -c '%U:%G' "$SOCK" 2>/dev/null)"
expect_eq "agent.sock 不是 0666" "no" \
  "$(stat -c '%a' "$SOCK" 2>/dev/null | grep -qx 666 && echo yes || echo no)"

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
echo "== 3. 根只来自配置 =="
expect_eq "GET /files/roots" "200" "$(get files-roots.json /roots "")"
expect_eq "roots 里恰好三个登记根" "['flip', 'shm', 'work']" \
  "$(resp_get 'sorted(r["root_id"] for r in d["data"]["roots"])')"
expect_eq "登记根都可用" "True" "$(resp_get 'all(r["available"] for r in d["data"]["roots"])')"
expect_eq "roots 没有 path 为 / 的项" "False" "$(resp_get 'any(r["path"] == "/" for r in d["data"]["roots"])')"
expect_eq "roots 带挂载点与文件系统类型" "True" \
  "$(resp_get 'all(r.get("mount_point") and r.get("fs_type") for r in d["data"]["roots"])')"
expect_eq "未登记的 root_id 是 404 NOT_FOUND" "404" "$(get err-unknown-root.json /list "?root_id=nope&path=")"
expect_eq "未登记根的错误码" "NOT_FOUND" "$(resp_get 'd["error"]["code"]')"

echo
echo "== 4. 路径穿越与形状校验 =="
expect_eq "'../../etc/shadow' 是 422" "422" "$(get err-path-dotdot.json /list "?root_id=work&path=$(urlenc '../../etc/shadow')")"
expect_eq "'../../etc/shadow' 错误码 PATH_OUTSIDE_ROOT" "PATH_OUTSIDE_ROOT" "$(resp_get 'd["error"]["code"]')"
expect_eq "绝对路径是 422" "422" "$(get err-path-absolute.json /list "?root_id=work&path=$(urlenc '/etc/passwd')")"
expect_eq "绝对路径错误码 PATH_OUTSIDE_ROOT" "PATH_OUTSIDE_ROOT" "$(resp_get 'd["error"]["code"]')"
expect_eq "中段 '..' 是 422" "422" "$(get err-path-mid.json /list "?root_id=work&path=$(urlenc 'sub/../../etc')")"
expect_eq "空段（a//b）是 400" "400" "$(get err-path-empty-segment.json /list "?root_id=work&path=$(urlenc 'sub//sshd_config')")"
expect_eq "'.' 段是 400" "400" "$(get err-path-dot.json /list "?root_id=work&path=$(urlenc './hostname')")"
expect_eq "超长路径是 400" "400" \
  "$(get err-path-toolong.json /list "?root_id=work&path=$(urlenc "$(python3 -c 'print("a"*5000)')")")"
expect_eq "文本接口空路径是 400" "400" "$(get err-text-empty.json /text "?root_id=work&path=")"

echo
echo "== 5. 符号链接 / magic link / 硬链接 / 特殊文件 =="
expect_eq "列出根目录" "200" "$(get files-list-root.json /list "?root_id=work&path=")"
expect_eq "符号链接在列表里标为 symlink" "True" \
  "$(resp_get 'all(e["type"] == "symlink" for e in d["data"]["entries"] if e["name"] in ("link-secret","link-dir","link-magic"))')"
expect_eq "FIFO 在列表里标为 other" "other" \
  "$(resp_get '[e["type"] for e in d["data"]["entries"] if e["name"] == "fifo"][0]')"
expect_eq "符号链接指向根外的文件：读取是 422" "422" \
  "$(get err-symlink-text.json /text "?root_id=work&path=link-secret")"
expect_eq "符号链接错误码 PATH_OUTSIDE_ROOT" "PATH_OUTSIDE_ROOT" "$(resp_get 'd["error"]["code"]')"
expect_eq "符号链接指向根外的目录：列举是 422" "422" \
  "$(get err-symlink-list.json /list "?root_id=work&path=link-dir")"
expect_eq "magic link 跳板（/proc/self/fd）：列举是 422" "422" \
  "$(get err-magic-list.json /list "?root_id=work&path=$(urlenc 'link-magic/0')")"
expect_eq "硬链接（nlink=2）：读取是 422" "422" "$(get err-hardlink-text.json /text "?root_id=work&path=hard-a.conf")"
expect_eq "硬链接错误码 PATH_OUTSIDE_ROOT" "PATH_OUTSIDE_ROOT" "$(resp_get 'd["error"]["code"]')"
expect_eq "FIFO 读取是 400（不是普通文件，且不阻塞）" "400" "$(get err-fifo-text.json /text "?root_id=work&path=fifo")"
expect_eq "目录当文件读是 400" "400" "$(get err-dir-text.json /text "?root_id=work&path=sub")"
expect_eq "根自身不能当文件读" "400" "$(get err-root-text.json /text "?root_id=work&path=")"

echo
echo "== 6. 文本：内容、版本与上限 =="
expect_eq "读子目录里的文本" "200" "$(get files-text-ok.json /text "?root_id=work&path=sub/sshd_config")"
expect_eq "文本内容正确" "Port 22\nPermitRootLogin no\n" \
  "$(resp_get 'd["data"]["content"].replace("\n","\\n")')"
expect_eq "文本路径是根内相对路径" "sub/sshd_config" "$(resp_get 'd["data"]["path"]')"
expect_eq "文本带 version" "True" "$(resp_get 'd["data"]["version"].startswith("v1-")')"
expect_eq "文本大小是字符串" "True" "$(resp_get 'isinstance(d["data"]["size_bytes"], str)')"
VERSION=$(resp_get 'd["data"]["version"]')
expect_eq "带同一版本重读仍然成功" "200" \
  "$(get files-text-version-ok.json /text "?root_id=work&path=sub/sshd_config&expected_version=$VERSION")"
printf 'changed\n' >"$ROOT/sub/sshd_config"
expect_eq "文件变了之后旧版本是 409" "409" \
  "$(get err-version-conflict.json /text "?root_id=work&path=sub/sshd_config&expected_version=$VERSION")"
expect_eq "版本冲突错误码" "VERSION_CONFLICT" "$(resp_get 'd["error"]["code"]')"
printf 'Port 22\nPermitRootLogin no\n' >"$ROOT/sub/sshd_config"
expect_eq "超过 1 MiB 的文本是 413" "413" "$(get err-too-large.json /text "?root_id=work&path=big.log")"
expect_eq "非 UTF-8 是 422 FILE_NOT_TEXT" "422" "$(get err-not-text.json /text "?root_id=work&path=binary.dat")"
expect_eq "非 UTF-8 错误码" "FILE_NOT_TEXT" "$(resp_get 'd["error"]["code"]')"
expect_eq "含 NUL 的文本也是 FILE_NOT_TEXT" "422" "$(get err-nul-text.json /text "?root_id=work&path=nul.dat")"

echo
echo "== 7. 分页与超大目录 =="
expect_eq "第一页" "200" "$(get files-list-page1.json /list "?root_id=work&path=pages")"
expect_eq "第一页条数是一页 200" "$PAGE_EXPECT" "$(resp_get 'len(d["data"]["entries"])')"
CURSOR=$(resp_get 'd["data"]["next_cursor"]')
expect_eq "第一页给出 next_cursor" "True" "$(resp_get 'bool(d["data"]["next_cursor"])')"
expect_eq "第二页" "200"   "$(get files-list-page2.json /list "?root_id=work&path=pages&cursor=$(urlenc "$CURSOR")")"
expect_eq "250 项正好是 200 + 50，第二页是尾页" "50" "$(resp_get 'len(d["data"]["entries"])')"
expect_eq "尾页 next_cursor 为空（不是空串）" "True" "$(resp_get 'd["data"]["next_cursor"] is None')"
expect_eq "两页无重复" "True" "$(python3 - "$PAYLOADS" <<'PY'
import json, sys, os
p = sys.argv[1]
a = json.load(open(os.path.join(p, "files-list-page1.json"), encoding="utf-8"))["data"]["entries"]
b = json.load(open(os.path.join(p, "files-list-page2.json"), encoding="utf-8"))["data"]["entries"]
names = [e["name"] for e in a] + [e["name"] for e in b]
print(all(names[i] != names[i - 1] for i in range(1, len(names))) and len(set(names)) == len(names))
PY
)"
expect_eq "超大目录如实标记 truncated" "True" \
  "$(get files-truncated.json /list "?root_id=shm&path=" >/dev/null; resp_get 'd["data"]["truncated"]')"

echo
echo "== 8. 根内子挂载（RESOLVE_NO_XDEV） =="
mkdir -p "$ROOT_SHM/submount"
if mount -t tmpfs tmpfs "$ROOT_SHM/submount" 2>"$WORK/logs/mount.err"; then
  printf 'inner\n' >"$ROOT_SHM/submount/inner.txt"
  expect_eq "根内子挂载上的路径是 422" "422" "$(get err-xdev.json /list "?root_id=shm&path=submount/inner.txt")"
  expect_eq "子挂载错误码 PATH_OUTSIDE_ROOT" "PATH_OUTSIDE_ROOT" "$(resp_get 'd["error"]["code"]')"
  umount "$ROOT_SHM/submount" 2>/dev/null
else
  notrun "根内子挂载 (NO_XDEV)" "mount 失败：$(tail -1 "$WORK/logs/mount.err" 2>/dev/null)"
fi

echo
echo "== 9. 根身份变化 =="
expect_eq "替换之前 flip 根可列举" "200" "$(get files-flip-before.json /list "?root_id=flip&path=")"
# 用 rename 换掉根，而不是“删掉再 mkdir 同名目录”：文件系统可能把刚释放的 inode
# 立刻复用给新目录，那样 dev/inode 与登记时一致，这条用例就成了假通过。
mkdir -p "$WORK/decoy"
printf 'impostor\n' >"$WORK/decoy/impostor.txt"
rm -rf "$ROOT_FLIP"
mv "$WORK/decoy" "$ROOT_FLIP"
expect_eq "根被删掉重建后列举是 409" "409" "$(get err-root-unavailable.json /list "?root_id=flip&path=")"
expect_eq "根不可用错误码" "ROOT_UNAVAILABLE" "$(resp_get 'd["error"]["code"]')"
expect_eq "重建的根没有被当成原来的根（响应里没有任何目录项）" "True" \
  "$(resp_get 'not (d.get("data") or {}).get("entries")')"
expect_eq "根不可用后 roots 里标出不可用" "True" \
  "$(get files-roots-after.json /roots "" >/dev/null; \
     resp_get '[r["available"] for r in d["data"]["roots"] if r["root_id"]=="flip"][0] == False')"

echo
echo "== 10. 下载 =="
expect_eq "下载成功" "200" "$(dl files-download "?root_id=work&path=sub/sshd_config")"
expect_eq "Content-Type 是 octet-stream" "application/octet-stream" "$(head_of files-download Content-Type)"
expect_eq "带 nosniff" "nosniff" "$(head_of files-download X-Content-Type-Options)"
expect_eq "带 no-store" "True" \
  "$(case "$(head_of files-download Cache-Control)" in *no-store*) echo True;; *) echo False;; esac)"
expect_eq "Content-Length 与文件一致" "$(stat -c '%s' "$ROOT/sub/sshd_config")" "$(head_of files-download Content-Length)"
expect_eq "带 X-UPanel-File-Version" "True" \
  "$(case "$(head_of files-download X-UPanel-File-Version)" in v1-*) echo True;; *) echo False;; esac)"
expect_eq "Content-Disposition 是 attachment" "True" \
  "$(case "$(head_of files-download Content-Disposition)" in attachment*) echo True;; *) echo False;; esac)"
expect_eq "下载字节与原文件一致" "$(sha_of "$ROOT/sub/sshd_config")" "$(sha_of "$PAYLOADS/files-download.bin")"

expect_eq "符号链接下载是 422" "422" "$(dl err-download-symlink "?root_id=work&path=link-secret")"
cp "$PAYLOADS/err-download-symlink.bin" "$PAYLOADS/err-download-symlink.json" 2>/dev/null
expect_eq "越界下载的 body 是 JSON 错误信封" "json" \
  "$(case "$(head_of err-download-symlink Content-Type)" in application/json*) echo json;; *) echo no;; esac)"
expect_eq "越界下载没有 octet-stream" "False" \
  "$(case "$(head_of err-download-symlink Content-Type)" in *octet-stream*) echo True;; *) echo False;; esac)"
expect_eq "越界下载的错误码" "PATH_OUTSIDE_ROOT" \
  "$(jget "$PAYLOADS/err-download-symlink.json" 'd["error"]["code"]')"
expect_eq "越界下载没有泄漏根外内容" "False" \
  "$(jget "$PAYLOADS/err-download-symlink.json" '"do-not-leak" in json.dumps(d)')"

echo
echo "== 11. 权限：未登录 / viewer / admin =="
expect_eq "未登录 roots 是 401" "401" "$(get_anon err-no-auth-roots.json /roots "")"
expect_eq "未登录错误码" "UNAUTHENTICATED" "$(resp_get 'd["error"]["code"]')"
expect_eq "未登录 list 是 401" "401" "$(get_anon err-no-auth-list.json /list "?root_id=work&path=")"
expect_eq "未登录 download 是 401" "401" \
  "$(curl -s -o "$PAYLOADS/err-no-auth-download.json" -w '%{http_code}' "$BASE$FILES/download?root_id=work&path=hostname")"
expect_eq "viewer roots 是 403" "403" "$(get_viewer err-viewer-roots.json /roots "")"
expect_eq "viewer 错误码" "FORBIDDEN" "$(resp_get 'd["error"]["code"]')"
expect_eq "viewer list 是 403" "403" "$(get_viewer err-viewer-list.json /list "?root_id=work&path=")"
expect_eq "viewer text 是 403" "403" "$(get_viewer err-viewer-text.json /text "?root_id=work&path=hostname")"
curl -s -b "$VJAR" -o "$PAYLOADS/err-viewer-download.json" -w '%{http_code}' \
  "$BASE$FILES/download?root_id=work&path=hostname" >"$WORK/viewer-dl-code.txt"
expect_eq "viewer download 是 403" "403" "$(cat "$WORK/viewer-dl-code.txt")"
expect_eq "viewer 下载拿不到文件内容" "False" \
  "$(grep -q 'upanel' "$PAYLOADS/err-viewer-download.json" && echo True || echo False)"

echo
echo "== 12. 回归：文件模块之外仍然正常 =="
expect_eq "健康检查" "200" "$(curl -s -o /dev/null -w '%{http_code}' "$BASE/api/v1/health/live")"
expect_eq "系统概要" "200" "$(api "$JAR" "$CSRF" GET /api/v1/system/summary)"
expect_eq "日志查询（P05.3）" "200" "$(api "$JAR" "$CSRF" GET '/api/v1/logs?limit=10')"
expect_eq "存储设备列表（P04）" "200" "$(api "$JAR" "$CSRF" GET /api/v1/storage/devices)"
expect_eq "服务列表（P05.1）" "200" "$(api "$JAR" "$CSRF" GET /api/v1/services)"

echo
echo "== 13. 载荷校验 =="
if [ -f "$VALIDATE" ]; then
  python3 "$VALIDATE" "$PAYLOADS" >"$WORK/logs/payload-validate.txt" 2>&1
  _rc=$?
  sed 's/^/      /' "$WORK/logs/payload-validate.txt"
  if [ "$_rc" -eq 0 ]; then
    pass "载荷校验（$PAYLOADS）"
  else
    fail "载荷校验" "$(grep -c '^FAIL' "$WORK/logs/payload-validate.txt" 2>/dev/null) 项失败"
  fi
else
  fail "载荷校验脚本存在" "$VALIDATE 不存在"
fi

echo
printf '通过 %d 项，失败 %d 项，未执行 %d 项\n' "$PASSED" "$FAILED" "$NOTRUN"
if [ "$FAILED" -ne 0 ]; then
  printf '失败项：%s\n' "$FAILED_NAMES"
  exit 1
fi
exit 0
