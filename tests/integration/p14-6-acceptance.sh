#!/bin/sh
# P14.6 Linux 验收脚本：TOTP、一次性恢复码、启用确认、CLI 本机恢复与会话失效。
#
# 运行位置：Ubuntu 22.04 / 24.04 验收 VM 内，**必须以 root 运行**。这一轮不碰分区、不碰
# 挂载、不碰网卡，只碰两个数据库（面板库 + 代理秘密库）和三条 /auth/totp 接口。
#
# 用法（**必须后台起**，理由与 P14.1–P14.5 相同：这一轮会故意把代理停掉再起，pipe 一抖
# 就 SIGPIPE 掉整轮验收）：
#   sudo BIN_DIR=/var/tmp/upanel-bin setsid nohup \
#        sh tests/integration/p14-6-acceptance.sh >/var/tmp/p146-run.log 2>&1 &
#   然后轮询 /var/tmp/upanel-accept-p14-6/RESULT
#
# 退出码：0 全部通过；1 有失败项；2 环境不满足。NOT RUN 是按设计列出的范围外项，单独
# 计数、不影响退出码——与 P14.1–P14.5 一致。
#
# ---------------------------------------------------------------- 这一轮验什么
#
# STAGES §16 对 P14.6 有两条硬要求，都必须有**真机读数**，不能拿单测折算：
#
#   A. **TOTP 未确认不启用**：
#      ① `POST /auth/totp/setup` 之后 `GET /auth/session` 仍是 `totp_enabled=false`，
#         面板库 `user_totp` 里**一行都没有**（不是 enabled=0，是没有行）；
#      ② 代理库那条种子此刻是 `state='temporary'`（十分钟自毁的临时秘密），不是 persistent；
#      ③ 此时无码登录照常成功——未确认的登记不改变任何登录规则；
#      ④ 确认时码错 → 401，且库里仍然一行都没有；
#      ⑤ 带外读数：`upanelctl totp-reset` 在这个时刻必须明确失败并说"没有启用二次验证"
#         （它读的是同一个库，不是接口返回的布尔值）。
#   B. **恢复码只能使用一次**：
#      ① 用第一枚恢复码关闭时**先把代理停掉**：码在库里被标记 used_at，关闭因此失败（503）；
#      ② 带外读库：那一行的 used_at 已经非空——码被烧掉了，不是"失败了就当没用过"；
#      ③ 把代理起回来，用**同一枚**码再关一次 → 401「恢复码无效（每枚只能用一次）」；
#      ④ 换第二枚 → 200，关闭成功；库里状态行与十枚哈希一起消失。
#      （这一条不能靠"关掉之后再拿旧码试"来验：那时整张表都没了，分不清是"用过"还是
#      "不存在"。真正的判据是"码已标记为用过、而状态仍然存在"那个中间态。）
#
# 另外四件事也在这里读真机：
#
#   C. **码由脚本自己按 RFC 6238 算出来**（python3 标准库，扮演一部 authenticator），
#      不从面板里偷一个。所以这里验的是"面板认不认真码"，不是"面板认不认面板自己"。
#   D. **种子与 otpauth URI 一次都不落盘**：setup 返回的 base32 与整串 URI，在面板库、
#      代理库、以及它们的 -wal 文件里 grep 命中数必须是 0（代理库里是密文）。
#   E. **同一个时间步用两次被拒**（重放）：码对了但已经在 30 秒窗口里用掉 → 401「刚刚用过」。
#      这是"通过一次不等于通过一辈子"的唯一真机证据。
#   F. **CLI 本机恢复**：root 跑 `totp-reset` 时 `agent_status=forbidden`（SO_PEERCRED 只认
#      upanel-web，这是设计不是缺陷），以 upanel-web 身份跑则 `destroyed`；两种情况下
#      面板库都清干净、会话全部作废、审计 actor 是 `local-cli:*` 且 actor_id 为 NULL。
#
# ---------------------------------------------------------------- 明确不验（NOT RUN）
#
#   * **真浏览器里的点击流程**（点"开始登记"→抄 URI→输码→抄恢复码）：这台 VM 上没有
#     浏览器。前端产物本身能验（见第 8 节：bundle 里真有这一块），交互由前端单测与
#     这一轮的接口读数共同覆盖。
#   * **二维码图片**：P00 冻结的依赖集里没有 QR 编码器，界面按文本 + 复制按钮呈现
#     `otpauth_uri`。要不要引 QR 库是独立决策，不在这一轮。
#   * **`GET /auth/totp`（还剩几枚恢复码）**：这一轮不开新接口。余量只能从
#     `upanelctl totp-reset` 的输出与关闭流程看到。
#   * **手机与服务器时钟相差超过 ±1 步**：需要改 VM 时钟（`date -s`），会把 30 秒窗口、
#     会话过期、限速窗口一起搅乱，其它断言就不可信了。±1 步的边界由
#     `internal/totp` 的单测（RFC 6238 附录 B 六组向量）覆盖。
#   * **登录限速打满**（同一账号 15 分钟内 5 次失败）：这一轮刻意只制造 4 次失败，
#     把剩下的额度留给下一轮；打满之后所有断言都会变成 429，验不出别的东西。

set -u

umask 077

BIN_DIR=${BIN_DIR:-/var/tmp/upanel-bin}
if [ "$(id -u)" -ne 0 ]; then
  printf '本脚本必须以 root 运行：sudo BIN_DIR=%s sh %s\n' "$BIN_DIR" "$0" >&2
  exit 2
fi

AGENT="$BIN_DIR/upanel-agent"
WEB="$BIN_DIR/upanel-web"
CTL="$BIN_DIR/upanelctl"

WORK=${WORK:-/var/tmp/upanel-accept-p14-6}
TMPD=$WORK/tmp
FACTS=$WORK/facts
PAYLOADS=$WORK/payloads
RUN_DIR=/run/upanel
SOCK=$RUN_DIR/agent.sock
SYS_CFG=/etc/upanel/upanel.json
AGENT_DB_DIR=/var/lib/upanel-agent
AGENT_DB=$AGENT_DB_DIR/agent.db
KEY_FILE=/etc/upanel/agent.key
WEB_DB_DIR=$WORK/webdata
WEB_DB=$WEB_DB_DIR/upanel.db
PORT=${PORT:-19483}
WEB_ADDR=127.0.0.1
BASE="http://$WEB_ADDR:$PORT"
ORIGIN="$BASE"
WEB_USER=upanel-web
PASSWORD='vm-p146-admin-71'
JAR=""
CSRF=""
RESULT=$WORK/RESULT

AGENT_LOG=$WORK/logs/agent.log
WEB_LOG=$WORK/logs/web.log
AGENT_PID=""
WEB_PID=""

SESSION_COOKIE='__Host-upanel_session'

PASSED=0
FAILED=0
NTRUN=0
FAILED_NAMES=""
NOTRUN_NAMES=""

pass() { PASSED=$((PASSED + 1)); printf 'PASS  %s\n' "$1"; }
fail() { FAILED=$((FAILED + 1)); FAILED_NAMES="$FAILED_NAMES | $1"; printf 'FAIL  %s: %s\n' "$1" "${2:-}"; }
notrun() { NTRUN=$((NTRUN + 1)); NOTRUN_NAMES="$NOTRUN_NAMES | $1"; printf 'NOT RUN  %s: %s\n' "$1" "${2:-}"; }
note() { printf 'NOTE  %s\n' "$1"; }
stamp() { printf 'STAMP %s %s\n' "$(date -u +%FT%TZ)" "$1"; }

expect_eq() { if [ "$2" = "$3" ]; then pass "$1"; else fail "$1" "期望 '$2'，实际 '$3'"; fi; }
expect_ne() { if [ "$2" != "$3" ]; then pass "$1"; else fail "$1" "不该等于 '$3'"; fi; }
expect_contains() {
  case $3 in
    *"$2"*) pass "$1" ;;
    *) fail "$1" "缺少 '$2'（实际: $3）" ;;
  esac
}
expect_not_contains() {
  case $3 in
    *"$2"*) fail "$1" "不该出现 '$2'（实际: $3）" ;;
    *) pass "$1" ;;
  esac
}
# 标识形状（四前缀之一 + 32 位小写十六进制）。取不到值时 jget 会打印 ERR:...，
# 非空但没意义；按形状判能把它当场拦下，而不是让后面每一条都拿垃圾值去比较。
expect_id() {
  if printf '%s' "$2" | grep -Eq '^(sac|ssk|sru|sec|srule)_[0-9a-f]{32}$'; then
    pass "$1"
  else
    fail "$1" "不是 <前缀>_+32 位十六进制：'$2'"
  fi
}
expect_ge() { # $1 标题 $2 下界 $3 实际
  if [ "${3:-x}" -ge "$2" ] 2>/dev/null; then pass "$1"; else fail "$1" "期望 >= $2，实际 '$3'"; fi
}

write_result() {
  {
    printf 'PASS=%s FAIL=%s NOT RUN=%s\n' "$PASSED" "$FAILED" "$NTRUN"
    printf 'FAILED_NAMES=%s\n' "${FAILED_NAMES:- (none)}"
    printf 'NOTRUN_NAMES=%s\n' "${NOTRUN_NAMES:- (none)}"
  } >"$RESULT"
}

# ---- JSON 助手 ----
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

# ---- 数据库的带外读数 ----
#
# 一律读**副本**：直接以 rw 打开面板库会在库里留下 root 拥有的 -wal/-shm，
# 之后 upanel-web 就写不动了（这正是 P14.5 在 init-admin 之后要 chown 的原因）。
# 读副本还有第二个好处：读的那一刻是自洽的，不会撞上正在写的那一笔。
dbq() { # $1 数据库文件, $2 一条 SQL（取第一行第一列）
  python3 - "$1" "$2" <<'PY'
import os, shutil, sqlite3, sys, tempfile
src, sql = sys.argv[1], sys.argv[2]
d = tempfile.mkdtemp(prefix="upanel-ro-")
dst = os.path.join(d, "copy.db")
try:
    shutil.copyfile(src, dst)
    for suffix in ("-wal", "-shm"):
        if os.path.exists(src + suffix):
            shutil.copyfile(src + suffix, dst + suffix)
    con = sqlite3.connect(dst)
    try:
        row = con.execute(sql).fetchone()
        print("" if row is None else row[0])
    finally:
        con.close()
finally:
    shutil.rmtree(d, ignore_errors=True)
PY
}

# 明文命中数：在给定的所有文件（含 -wal）里数一个字符串出现几次。
plain_hits() { # $1 要搜的串, 其余是文件
  # 空串当"要搜的串"时 grep 会匹配每一行——得到的数字跟安全毫无关系，却会伪装成
  # "命中很多"。第一轮就是这么红的（期望 0、实际 496 行）。这里直接返回 -1：
  # 断言照旧会红，但红的原因看得见（Q481）。
  if [ -z "${1:-}" ]; then
    printf '%s' -1
    return 0
  fi
  _needle=$1
  shift
  _n=0
  for _f in "$@"; do
    [ -e "$_f" ] || continue
    _c=$(grep -a -c -F -- "$_needle" "$_f" 2>/dev/null || true)
    [ -n "$_c" ] || _c=0
    _n=$((_n + _c))
  done
  printf '%s' "$_n"
}

# ---- 充当一部 authenticator：按 RFC 6238 自己算码 ----
#
# 与产品里的实现不是同一份代码（那份在 internal/totp，有 RFC 向量对过）：这里用 python3
# 标准库从 base32 种子现算，就是为了让"面板认不认真码"这件事有个独立的对照。
totp_code() { # $1 base32 种子, $2 相对时间步（0=当前，1=下一个）
  python3 - "$1" "$2" <<'PY'
import base64, hashlib, hmac, struct, sys, time
secret = sys.argv[1].strip().upper().replace(" ", "")
offset = int(sys.argv[2])
pad = (8 - len(secret) % 8) % 8
key = base64.b32decode(secret + "=" * pad)
step = int(time.time()) // 30 + offset
digest = hmac.new(key, struct.pack(">Q", step), hashlib.sha1).digest()
o = digest[-1] & 0x0F
value = struct.unpack(">I", digest[o:o + 4])[0] & 0x7FFFFFFF
print("%06d" % (value % 1000000))
PY
}

# 从 otpauth:// URI 里取出 base32 种子。
totp_secret_of() {
  python3 - "$1" <<'PY'
import sys
from urllib.parse import parse_qs, urlsplit
parts = urlsplit(sys.argv[1])
print(parse_qs(parts.query).get("secret", [""])[0])
PY
}

# 恢复码的规范化与哈希（与服务端同一规则：大写、只留字母数字、sha256:<hex>）。
recovery_hash() {
  python3 - "$1" <<'PY'
import hashlib, re, sys
canon = re.sub(r"[^A-Z0-9]", "", sys.argv[1].upper())
print("sha256:" + hashlib.sha256(canon.encode()).hexdigest())
PY
}

# 等到一个 30 秒窗口的开头（最多 30 秒）：给"同一个码连用两次 → 重放"留出余量，
# 不然两次请求跨过窗口边界，第二次会以"码不对"而不是"刚刚用过"被拒。
wait_for_step_edge() {
  _sleep_for=$(( 30 - ($(date +%s) % 30) ))
  [ "$_sleep_for" -gt 0 ] || _sleep_for=1
  sleep "$_sleep_for"
}

# ---- 进程 ----

start_agent() {
  "$AGENT" --config "$SYS_CFG" >>"$AGENT_LOG" 2>&1 &
  AGENT_PID=$!
  for _ in $(seq 1 150); do
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
  runuser -u "$WEB_USER" -- "$WEB" --config "$SYS_CFG" --mode production --db "$WEB_DB" \
    --listen "$WEB_ADDR:$PORT" >>"$WEB_LOG" 2>&1 &
  WEB_PID=$!
  for _ in $(seq 1 250); do
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
}

chown_web_db() {
  for suffix in "" "-wal" "-shm"; do
    [ -e "$WEB_DB$suffix" ] && chown "$WEB_USER" "$WEB_DB$suffix"
  done
  return 0
}

cleanup() {
  stop_web
  stop_agent
  return 0
}
trap cleanup EXIT
trap 'write_result; cleanup; exit 130' INT
trap 'write_result; cleanup; exit 143' TERM

# ---- 面板读的总线 ----

api() {
  _jar=$1; _csrf=$2; _method=$3; _path=$4; _body=${5:-}
  if [ -n "$_body" ]; then
    printf '%s' "$_body" >"$TMPD/req.json"
    curl -s -b "$_jar" -o "$TMPD/resp.json" -w '%{http_code}' -X "$_method" "$BASE$_path" \
      -H 'Content-Type: application/json' -H "Origin: $ORIGIN" -H "X-CSRF-Token: $_csrf" \
      -d @"$TMPD/req.json"
  else
    curl -s -b "$_jar" -o "$TMPD/resp.json" -w '%{http_code}' -X "$_method" "$BASE$_path" \
      -H "Origin: $ORIGIN" -H "X-CSRF-Token: $_csrf"
  fi
}

# 登录：带不带 totp 都由第 5 个参数决定（空 = 这次没带码）。
user_login() { # $1 账号 $2 口令 $3 jar 文件 $4 输出名 $5 码（可空）
  if [ -n "${5:-}" ]; then
    printf '{"username":"%s","password":"%s","totp":"%s"}' "$1" "$2" "$5" >"$TMPD/login.json"
  else
    printf '{"username":"%s","password":"%s"}' "$1" "$2" >"$TMPD/login.json"
  fi
  curl -s -c "$3" -o "$WORK/$4.json" -w '%{http_code}' -X POST "$BASE/api/v1/auth/login" \
    -H 'Content-Type: application/json' -H 'X-UPanel-Request: 1' -H "Origin: $ORIGIN" \
    -d @"$TMPD/login.json"
}
login_get() { jget "$WORK/$1.json" "$2"; }

reauth_with() { # $1 口令 $2 码（可空）
  if [ -n "${2:-}" ]; then
    api "$JAR" "$CSRF" POST /api/v1/auth/reauth "{\"password\":\"$1\",\"totp\":\"$2\"}"
  else
    api "$JAR" "$CSRF" POST /api/v1/auth/reauth "{\"password\":\"$1\"}"
  fi
}

session_get() { # $1 jar 文件 → HTTP 状态码；响应体留在 $TMPD/session.json
  _code=$(api "$1" "" GET /api/v1/auth/session "")
  cp "$TMPD/resp.json" "$TMPD/session.json"
  printf '%s' "$_code"
}
session_field() { # $1 jar 文件 $2 表达式
  session_get "$1" >/dev/null
  jget "$TMPD/session.json" "$2"
}
# **GET /auth/session 会轮换 CSRF 令牌**（服务端每个会话只留哈希）。所以每读一次会话，
# 脚本手里那份旧令牌就作废了。读主会话（$JAR）之后必须调一次这个函数，把令牌换成新的；
# 变量赋值不能写进 $( ) 里——那是在子 shell 里改，父 shell 看不见。
csrf_from_session() {
  _token=$(jget "$TMPD/session.json" 'd["data"]["csrf_token"]')
  case $_token in
    '' | ERR:*) : ;;
    *) CSRF=$_token ;;
  esac
}
session_field() { # $1 jar 文件 $2 表达式
  session_get "$1" >/dev/null
  jget "$TMPD/session.json" "$2"
}

# jar 里有没有会话 Cookie（文件不存在算 0）。用来证明"口令对了但码没过时一个会话都不签发"。
jar_has_session() {
  if [ -f "$1" ] && grep -q "$SESSION_COOKIE" "$1" 2>/dev/null; then echo 1; else echo 0; fi
}

setup_call() { api "$JAR" "$CSRF" POST /api/v1/auth/totp/setup '{}'; }
confirm_call() { # $1 enrollment_id $2 码
  api "$JAR" "$CSRF" POST /api/v1/auth/totp/confirm \
    "{\"enrollment_id\":\"$1\",\"totp\":\"$2\"}"
}
disable_call() { # $1 口令 $2 验证码（可空）$3 恢复码（可空）
  _body=$(python3 -c 'import json,sys
d={"password":sys.argv[1]}
if sys.argv[2]:
    d["totp"]=sys.argv[2]
if sys.argv[3]:
    d["recovery_code"]=sys.argv[3]
print(json.dumps(d))' "$1" "${2:-}" "${3:-}")
  api "$JAR" "$CSRF" DELETE /api/v1/auth/totp "$_body"
}

# 只读那几个会让断言有意义的数字，全从库里取，不看接口说什么。
totp_rows() { dbq "$WEB_DB" "SELECT COUNT(*) FROM user_totp"; }
totp_enabled_rows() { dbq "$WEB_DB" "SELECT COUNT(*) FROM user_totp WHERE enabled = 1"; }
totp_secret_ref() { dbq "$WEB_DB" "SELECT secret_ref FROM user_totp LIMIT 1"; }
totp_last_step() { dbq "$WEB_DB" "SELECT last_step FROM user_totp LIMIT 1"; }

# ---- 取一个"现在就能用、又不是重放"的码 ----
#
# 面板只接受**严格大于** last_step 的时间步，窗口是 ±1。所以能用的最小步是 last_step+1：
#   - last_step 比当前步小（正常情况）→ 用当前步（offset 0）；
#   - last_step 等于当前步（刚刚用掉这一步）→ 用下一步（offset 1，仍在窗口内）；
#   - last_step 比当前步大（刚用过一个超前步）→ 等到时间追上来，再用 offset 1。
#
# 第一轮验收在这里连错三次：脚本原先按 0/1/2/3 一路往上加偏移，而 offset 2、3 已经落到
# ±1 窗口外面，于是"带码再认证""第 2 步用下一个码"以及它们后面的一串断言全成了假红——
# 看起来像产品拒了正确的码，其实是脚本自己送了一个不该被接受的步（Q481）。
current_step() { echo $(( $(date +%s) / 30 )); }

fresh_code() { # $1 base32 种子
  _ls=$(totp_last_step)
  [ -n "$_ls" ] || _ls=-1
  _off=$(( _ls + 1 - $(current_step) ))
  [ "$_off" -ge 0 ] || _off=0
  while [ "$_off" -gt 1 ]; do
    wait_for_step_edge
    _off=$(( _ls + 1 - $(current_step) ))
  done
  totp_code "$1" "$_off"
}
recovery_rows() { dbq "$WEB_DB" "SELECT COUNT(*) FROM user_totp_recovery_codes"; }
recovery_hashed_rows() {
  dbq "$WEB_DB" "SELECT COUNT(*) FROM user_totp_recovery_codes WHERE substr(code_hash,1,7) = 'sha256:'"
}
recovery_used_at() { # $1 明文恢复码 → used_at（空串 = 还没用过 / 没有这一行）
  dbq "$WEB_DB" "SELECT COALESCE(used_at,'') FROM user_totp_recovery_codes WHERE code_hash='$(recovery_hash "$1")'"
}
seed_state() { # $1 代理侧引用 → 那条种子的 state
  # 引用形状先验一遍：上一段若已经红了，这里可能拿到的是 jget 的错误标记
  # （ERR:KeyError:'data'），拼进 SQL 会变成一条语法错，把真正的原因盖掉（Q481）。
  case "${1:-}" in
    sec_*) dbq "$AGENT_DB" "SELECT state FROM secrets WHERE id = '$1'" ;;
    *) printf '' ;;
  esac
}
seed_count_by_state() { # $1 state
  dbq "$AGENT_DB" "SELECT COUNT(*) FROM secrets WHERE purpose = 'totp_seed' AND state = '$1'"
}
audit_count() { dbq "$WEB_DB" "SELECT COUNT(*) FROM audit_events WHERE action = '$1'"; }
audit_last_actor() { dbq "$WEB_DB" "SELECT actor_snapshot FROM audit_events WHERE action = '$1' ORDER BY id DESC LIMIT 1"; }

# ---------------------------------------------------------------------------
echo "== 0. 环境与起点 =="
# ---------------------------------------------------------------------------

for p in "$AGENT" "$WEB" "$CTL"; do
  if [ -x "$p" ]; then pass "二进制存在：$p"; else echo "缺少 $p（先交叉编译到 $BIN_DIR）" >&2; exit 2; fi
done
for tool in python3 curl ss runuser; do
  if command -v "$tool" >/dev/null 2>&1; then pass "夹具：有 $tool"; else
    echo "缺少 $tool —— 这是环境不满足，不是产品缺陷" >&2; exit 2
  fi
done
# python3 要能算 TOTP（base32 + hmac）：缺任何一个模块都算环境不满足。
if python3 -c 'import base64, hashlib, hmac, struct' >/dev/null 2>&1; then
  pass "夹具：python3 能算 HMAC-SHA1（这一轮的码由脚本自己算）"
else
  echo "python3 缺少 hashlib/hmac —— 环境不满足" >&2; exit 2
fi

rm -rf "$WORK"
mkdir -p "$WORK" "$TMPD" "$FACTS" "$PAYLOADS" "$WORK/logs" "$WEB_DB_DIR"
chmod 0755 "$WORK" "$WORK/logs" "$PAYLOADS" "$FACTS"
chmod 0700 "$TMPD"
chown -R "$WEB_USER" "$WEB_DB_DIR"
rm -f "$RESULT"

stamp "起点"

# ---------------------------------------------------------------------------
echo
echo "== 1. 配置与启动（代理 root、Web 非 root） =="
# ---------------------------------------------------------------------------

install -d -m 0755 /etc/upanel
install -d -m 0700 "$AGENT_DB_DIR"
install -d -m 0755 "$RUN_DIR"
# **起点必须是一张没有二次验证状态的面板库**：留着上一轮的 user_totp 行，
# 下面"未确认不启用"的断言就会以别人的现场为输入。
rm -f "$AGENT_DB" "$AGENT_DB-wal" "$AGENT_DB-shm"
rm -f "$WEB_DB" "$WEB_DB-wal" "$WEB_DB-shm"

cat >"$SYS_CFG" <<JSON
{
  "web": { "mode": "production", "listen_address": "$WEB_ADDR:$PORT", "shutdown_timeout_seconds": 10 },
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
if start_web; then pass "Web 在 $BASE 启动"; else
  fail "Web 启动" "$(tail -5 "$WEB_LOG" 2>/dev/null)"
  write_result; exit 1
fi
LISTEN_PID=$(ss -lptnH "sport = :$PORT" 2>/dev/null | sed -n 's/.*pid=\([0-9]*\).*/\1/p' | head -1)
[ -z "$LISTEN_PID" ] && LISTEN_PID=$WEB_PID
WEB_UID=$(ps -o uid= -p "$LISTEN_PID" 2>/dev/null | tr -d ' ')
if [ -n "$WEB_UID" ] && [ "$WEB_UID" != "0" ]; then
  pass "监听 $PORT 的 Web 进程不是 root（pid=$LISTEN_PID uid=$WEB_UID）"
else
  fail "监听 $PORT 的 Web 进程不是 root" "pid=${LISTEN_PID:-未知} uid=${WEB_UID:-未知}"
fi

JAR=$WORK/admin.jar
expect_eq "管理员登录" "200" "$(user_login admin "$PASSWORD" "$JAR" login-admin '')"
CSRF=$(login_get login-admin 'd["data"]["csrf_token"]')
expect_ne "拿到 CSRF 令牌" "" "$CSRF"
expect_eq "登录响应里 totp_enabled 是 false（起点没开二次验证）" "False" \
  "$(login_get login-admin 'd["data"]["totp_enabled"]')"

expect_eq "设置页有入口（SPA 返回 index.html）" "200" \
  "$(curl -s -o /dev/null -w '%{http_code}' "$BASE/settings")"

expect_eq "起点：面板库没有二次验证状态行" "0" "$(totp_rows)"
expect_eq "起点：代理库没有 totp_seed" "0" \
  "$(dbq "$AGENT_DB" "SELECT COUNT(*) FROM secrets WHERE purpose = 'totp_seed'")"

# ---------------------------------------------------------------------------
echo
echo "== 2. 未确认的登记不是启用（STAGES §16 之一） =="
# ---------------------------------------------------------------------------

expect_eq "登记前的再认证（只给口令）" "200" "$(reauth_with "$PASSWORD" '')"
expect_eq "POST /auth/totp/setup" "200" "$(setup_call)"
SETUP_JSON=$TMPD/resp.json
cp "$SETUP_JSON" "$PAYLOADS/totp-setup.json"
ENROLL_ID=$(jget "$SETUP_JSON" 'd["data"]["enrollment_id"]')
URI=$(jget "$SETUP_JSON" 'd["data"]["otpauth_uri"]')
SECRET=$(totp_secret_of "$URI")
expect_id "enrollment_id 是代理的引用（sec_+32 hex）" "$ENROLL_ID"
expect_eq "otpauth_uri 是 totp 类型" "otpauth" "$(printf '%s' "$URI" | cut -d: -f1)"
expect_contains "otpauth_uri 带着发行者与账号" "otpauth://totp/" "$URI"
expect_eq "otpauth_uri 里的 digits 是 6" "6" "$(jget "$SETUP_JSON" 'd["data"]["digits"]')"
expect_eq "otpauth_uri 里的 period 是 30" "30" "$(jget "$SETUP_JSON" 'd["data"]["period"]')"
if printf '%s' "$SECRET" | grep -Eq '^[A-Z2-7]{16,}$'; then
  pass "从 URI 里取出了 base32 种子（长 ${#SECRET}）"
else
  fail "从 URI 里取出 base32 种子" "取到 '$SECRET'"
fi

# ① 面板说"没启用"
expect_eq "setup 之后 session 仍是 totp_enabled=false" "False" \
  "$(session_field "$JAR" 'd["data"]["totp_enabled"]')"
csrf_from_session
# ② 面板库里一行都没有（不是 enabled=0，是没有行）
expect_eq "setup 之后面板库 user_totp 仍是 0 行" "0" "$(totp_rows)"
# ③ 代理库那条种子是"十分钟自毁的临时秘密"
expect_eq "代理库那条种子是 temporary" "temporary" "$(seed_state "$ENROLL_ID")"
expect_eq "代理库没有 persistent 的 totp_seed" "0" "$(seed_count_by_state persistent)"
# ④ 未确认的登记不改变登录规则：无码登录照常成功
expect_eq "未确认时无码登录照样 200" "200" \
  "$(user_login admin "$PASSWORD" "$WORK/unconfirmed.jar" login-unconfirmed '')"
expect_eq "未确认时登录响应 totp_enabled=false" "False" \
  "$(login_get login-unconfirmed 'd["data"]["totp_enabled"]')"
# ⑤ 带上一个码也没用：账号没启用二次验证，那个字段被忽略（不会因此变成启用）
expect_eq "未确认时带码登录也 200（码被忽略，不会顺手启用）" "200" \
  "$(user_login admin "$PASSWORD" "$WORK/unconfirmed2.jar" login-unconfirmed2 "$(totp_code "$SECRET" 0)")"
expect_eq "带码登录之后面板库仍然 0 行" "0" "$(totp_rows)"

# 明文种子不许落盘：面板库、代理库以及它们的 -wal，一个命中都不能有。
expect_eq "面板库里没有明文种子" "0" \
  "$(plain_hits "$SECRET" "$WEB_DB" "$WEB_DB-wal" "$WEB_DB-shm")"
expect_eq "代理库里没有明文种子（只存密文）" "0" \
  "$(plain_hits "$SECRET" "$AGENT_DB" "$AGENT_DB-wal" "$AGENT_DB-shm")"
expect_eq "两个库里都没有 otpauth URI" "0" \
  "$(plain_hits 'otpauth://' "$WEB_DB" "$WEB_DB-wal" "$AGENT_DB" "$AGENT_DB-wal")"

# ⑥ 带外读数：CLI 读同一个库，此刻必须明确失败（"没有启用二次验证"）。
"$CTL" totp-reset --config "$SYS_CFG" --db "$WEB_DB" --username admin --no-agent --json \
  >"$FACTS/ctl-before-confirm.out" 2>"$FACTS/ctl-before-confirm.err"
expect_eq "未确认时 totp-reset 退出码是 1（库里没有启用行）" "1" "$?"
expect_contains "未确认时 totp-reset 说没有启用二次验证" "没有启用二次验证" \
  "$(cat "$FACTS/ctl-before-confirm.err" 2>/dev/null)"
chown_web_db

# ⑦ 码错 → 401，且库里依然一行都没有（不做"半启用"）
WRONG=$(python3 -c 'import sys
c = sys.argv[1]
print("%06d" % ((int(c) + 1) % 1000000))' "$(totp_code "$SECRET" 0)")
expect_eq "确认时码错是 401" "401" "$(confirm_call "$ENROLL_ID" "$WRONG")"
expect_eq "码错时响应码是 UNAUTHENTICATED" "UNAUTHENTICATED" "$(jget "$TMPD/resp.json" 'd["error"]["code"]')"
expect_eq "码错之后面板库仍然 0 行" "0" "$(totp_rows)"
expect_eq "码错之后 session 仍然 totp_enabled=false" "False" \
  "$(session_field "$JAR" 'd["data"]["totp_enabled"]')"
csrf_from_session
expect_eq "码错之后代理库那条种子仍是 temporary" "temporary" "$(seed_state "$ENROLL_ID")"
expect_eq "码错之后没有签发任何恢复码" "0" "$(recovery_rows)"

# ---------------------------------------------------------------------------
echo
echo "== 3. 六位数确认之后才启用，十枚恢复码只出现一次 =="
# ---------------------------------------------------------------------------

wait_for_step_edge
CODE=$(totp_code "$SECRET" 0)
expect_eq "确认用当前时间步的码" "200" "$(confirm_call "$ENROLL_ID" "$CODE")"
CONFIRM_JSON=$TMPD/resp.json
cp "$CONFIRM_JSON" "$PAYLOADS/totp-confirm.json"
expect_eq "确认之后 enabled=true" "True" "$(jget "$CONFIRM_JSON" 'd["data"]["enabled"]')"
expect_eq "一次性恢复码是 10 枚" "10" "$(jget "$CONFIRM_JSON" 'len(d["data"]["recovery_codes"])')"
RC1=$(jget "$CONFIRM_JSON" 'd["data"]["recovery_codes"][0]')
RC2=$(jget "$CONFIRM_JSON" 'd["data"]["recovery_codes"][1]')
if printf '%s' "$RC1" | grep -Eq '^[0-9A-Z]{5}-[0-9A-Z]{5}$'; then
  pass "恢复码形状是 XXXXX-XXXXX"
else
  fail "恢复码形状是 XXXXX-XXXXX" "'$RC1'"
fi
expect_eq "十枚恢复码互不相同" "10" \
  "$(jget "$CONFIRM_JSON" 'len(set(d["data"]["recovery_codes"]))')"
# 0/1/O/I 不许出现在恢复码里：这四组字符是手抄时最容易看错的。
expect_eq "恢复码里没有 0/1/O/I" "0" \
  "$(jget "$CONFIRM_JSON" 'sum(1 for c in d["data"]["recovery_codes"] if set(c) & set("01OI"))')"

expect_eq "确认之后 session totp_enabled=true" "True" \
  "$(session_field "$JAR" 'd["data"]["totp_enabled"]')"
csrf_from_session
expect_eq "面板库 user_totp 变成 1 行 enabled=1" "1" "$(totp_enabled_rows)"
expect_eq "面板库记的 secret_ref 就是登记时那条" "$ENROLL_ID" "$(totp_secret_ref)"
expect_eq "last_step 已经被写过（不是初始的 -1）" "1" \
  "$(dbq "$WEB_DB" "SELECT last_step >= 0 FROM user_totp LIMIT 1")"
# Q480 的回归：确认用的那个码在同一个时间步里不能再拿来登录。
# 第一轮这里读到 last_step=-1——代理验过确认码，面板却没把这一步记下来，于是"刚确认完
# 的那个六位数"还能当登录码用一次。重放被拒不记失败次数，所以这条不占限速额度。
expect_eq "确认用的码立刻拿去登录是 401（确认也算用掉一步）" "401" \
  "$(user_login admin "$PASSWORD" "$WORK/confirmreplay.jar" login-confirmreplay "$CODE")"
expect_contains "确认码重放的拒因是'用过'" "用过" \
  "$(login_get login-confirmreplay 'd["error"]["message"]')"
expect_eq "确认码重放不签发会话" "0" "$(jar_has_session "$WORK/confirmreplay.jar")"
expect_eq "面板库恢复码 10 行" "10" "$(recovery_rows)"
expect_eq "10 行全是以 sha256: 开头的哈希" "10" "$(recovery_hashed_rows)"
expect_eq "第一枚恢复码的明文不在面板库" "0" \
  "$(plain_hits "$(printf '%s' "$RC1" | tr -d '-')" "$WEB_DB" "$WEB_DB-wal")"
expect_eq "代理库那条种子转成了 persistent" "persistent" "$(seed_state "$ENROLL_ID")"
expect_eq "代理库没有临时种子残留" "0" "$(seed_count_by_state temporary)"

# 已启用再登记一次必须被拒：一台机器只留一条种子，否则下次登录到底校验哪一份没有答案。
expect_eq "已启用再调 setup 是 422" "422" "$(setup_call)"
expect_eq "422 的响应码是 VALIDATION_ERROR" "VALIDATION_ERROR" "$(jget "$TMPD/resp.json" 'd["error"]["code"]')"
expect_eq "被拒之后面板库仍是 1 行" "1" "$(totp_rows)"

expect_ge "审计里有 auth.totp.setup" 1 "$(audit_count auth.totp.setup)"
expect_ge "审计里有 auth.totp.enable" 1 "$(audit_count auth.totp.enable)"
expect_eq "审计里没有出现 otpauth URI" "0" \
  "$(plain_hits 'otpauth://' "$WEB_DB" "$WEB_DB-wal")"

# 启用时作废其它会话：S2 里那两条无码登录的会话必须失效，发起启用这一条保留。
expect_eq "启用后旧会话（unconfirmed.jar）被作废" "401" \
  "$(session_get "$WORK/unconfirmed.jar")"
expect_eq "启用后本条会话仍有效" "True" \
  "$(session_field "$JAR" 'd["data"]["totp_enabled"]')"
csrf_from_session

# ---------------------------------------------------------------------------
echo
echo "== 4. 登录变成两步，同一个码不能用第二次 =="
# ---------------------------------------------------------------------------

expect_eq "已启用之后无码登录是 401" "401" \
  "$(user_login admin "$PASSWORD" "$WORK/nocode.jar" login-nocode '')"
expect_eq "缺码时的错误码是 UNAUTHENTICATED" "UNAUTHENTICATED" \
  "$(login_get login-nocode 'd["error"]["code"]')"
expect_eq "缺码时 details.totp_required=true（前端据此亮验证码框）" "True" \
  "$(login_get login-nocode 'd["error"].get("details", {}).get("totp_required")')"
if [ "$(jar_has_session "$WORK/nocode.jar")" = "0" ]; then
  pass "缺码时一个会话都不签发（口令对了也不发）"
else
  fail "缺码时一个会话都不签发" "jar 里竟然有 $SESSION_COOKIE"
fi

expect_eq "码错登录是 401" "401" \
  "$(user_login admin "$PASSWORD" "$WORK/wrongcode.jar" login-wrongcode "$WRONG")"
expect_eq "码错时也回 totp_required=true" "True" \
  "$(login_get login-wrongcode 'd["error"].get("details", {}).get("totp_required")')"

wait_for_step_edge
CODE=$(totp_code "$SECRET" 0)
expect_eq "码对登录是 200" "200" \
  "$(user_login admin "$PASSWORD" "$WORK/code1.jar" login-code1 "$CODE")"
expect_eq "码对登录的响应里 totp_enabled=true" "True" \
  "$(login_get login-code1 'd["data"]["totp_enabled"]')"

# 同一个码立刻再用一次：必须被拒（同一个 30 秒窗口里的第二次）。
expect_eq "同一个码第二次用是 401" "401" \
  "$(user_login admin "$PASSWORD" "$WORK/code1again.jar" login-code1again "$CODE")"
expect_contains "第二次的拒因是'刚刚用过'（重放）" "刚刚用过" \
  "$(login_get login-code1again 'd["error"]["message"]')"
expect_eq "重放被拒时不签发会话" "0" "$(jar_has_session "$WORK/code1again.jar")"

# 下一个时间步的码立刻就能用——这就是界面上"等手机上出现下一个六位数"那句话的依据。
CODE_NEXT=$(totp_code "$SECRET" 1)
expect_eq "下一个时间步的码现在就能用（±1 步窗口）" "200" \
  "$(user_login admin "$PASSWORD" "$WORK/code2.jar" login-code2 "$CODE_NEXT")"

# 再认证也是两步：先不带码 → 401 + totp_required；再带一个新码 → 200。
expect_eq "已启用之后无码再认证是 401" "401" "$(reauth_with "$PASSWORD" '')"
expect_eq "无码再认证也回 totp_required=true" "True" \
  "$(jget "$TMPD/resp.json" 'd["error"].get("details", {}).get("totp_required")')"
CODE_NEXT2=$(fresh_code "$SECRET")
expect_eq "带码再认证是 200" "200" "$(reauth_with "$PASSWORD" "$CODE_NEXT2")"

# ---------------------------------------------------------------------------
echo
echo "== 5. 恢复码只能用一次（STAGES §16 之二） =="
# ---------------------------------------------------------------------------
#
# 判据不是"关掉之后旧码不认了"（那时整张表都没了，分不清是'用过'还是'不存在'），
# 而是那个**中间态**：码在库里被标记 used_at，而状态行还在。造出这个中间态的办法是
# 让关闭在"码已经被消费"之后失败——把代理停掉，第 3 步销毁种子必然不可用。
#
# 停代理前必须先把再认证窗口推到 5 分钟后（停掉代理就再也做不了带码再认证）。

CODE_NEXT3=$(fresh_code "$SECRET")
expect_eq "停代理之前的带码再认证" "200" "$(reauth_with "$PASSWORD" "$CODE_NEXT3")"
expect_eq "RC1 此刻还没被标记用过" "" "$(recovery_used_at "$RC1")"
expect_eq "此刻面板库恢复码仍是 10 行" "10" "$(recovery_rows)"

stop_agent
if [ -S "$SOCK" ]; then fail "代理已停止（socket 应消失）" "socket 还在"; else pass "代理已停止（socket 消失）"; fi

expect_eq "代理不在时用 RC1 关闭 → 503（关闭不可能成功）" "503" \
  "$(disable_call "$PASSWORD" '' "$RC1")"
expect_eq "503 的错误码是 AGENT_UNAVAILABLE" "AGENT_UNAVAILABLE" \
  "$(jget "$TMPD/resp.json" 'd["error"]["code"]')"
# 带外读数：码确实被烧掉了——不是"失败了就当没用过"。
if [ -n "$(recovery_used_at "$RC1")" ]; then
  pass "带外读库：RC1 的 used_at 已经被写上（码一次性，失败也不回滚）"
else
  fail "带外读库：RC1 的 used_at 已经被写上" "used_at 仍为空"
fi
expect_eq "关闭失败之后状态行还在（没有半清）" "1" "$(totp_enabled_rows)"
expect_eq "关闭失败之后恢复码仍是 10 行" "10" "$(recovery_rows)"

if start_agent; then pass "代理重新启动"; else fail "代理重新启动" "$(tail -5 "$AGENT_LOG" 2>/dev/null)"; fi

expect_eq "代理回来后用同一枚 RC1 再关 → 401" "401" "$(disable_call "$PASSWORD" '' "$RC1")"
expect_contains "拒因是'每枚只能用一次'" "只能用一次" "$(jget "$TMPD/resp.json" 'd["error"]["message"]')"
expect_eq "RC1 仍然保持'用过'（没有被复活）" "1" \
  "$(dbq "$WEB_DB" "SELECT used_at IS NOT NULL FROM user_totp_recovery_codes WHERE code_hash='$(recovery_hash "$RC1")'")"
expect_eq "被拒之后恢复码仍是 10 行（用过的不删，只标记）" "10" "$(recovery_rows)"
expect_eq "被拒之后二次验证仍启用" "1" "$(totp_enabled_rows)"

expect_eq "换第二枚 RC2 关闭 → 200" "200" "$(disable_call "$PASSWORD" '' "$RC2")"
expect_eq "关闭响应 enabled=false" "False" "$(jget "$TMPD/resp.json" 'd["data"]["enabled"]')"
expect_eq "关闭响应报出作废的其它会话数 >= 1" "True" \
  "$(jget "$TMPD/resp.json" 'd["data"]["revoked_sessions"] >= 1')"
expect_eq "关闭之后面板库状态行没了" "0" "$(totp_rows)"
expect_eq "关闭之后十枚恢复码一起没了" "0" "$(recovery_rows)"
expect_eq "关闭之后代理库那条种子是 destroyed" "destroyed" "$(seed_state "$ENROLL_ID")"
expect_eq "关闭之后代理库没有 persistent 的 totp_seed" "0" "$(seed_count_by_state persistent)"
expect_eq "关闭之后 session totp_enabled=false" "False" \
  "$(session_field "$JAR" 'd["data"]["totp_enabled"]')"
csrf_from_session
expect_eq "关闭之后无码登录又是 200" "200" \
  "$(user_login admin "$PASSWORD" "$WORK/afterdisable.jar" login-afterdisable '')"
expect_eq "关闭之后那条'另一台设备'的会话被作废" "401" \
  "$(session_get "$WORK/code2.jar")"
expect_eq "关闭之后发起关闭的这一条会话仍有效" "False" \
  "$(session_field "$JAR" 'd["data"]["totp_enabled"]')"
csrf_from_session

# ---------------------------------------------------------------------------
echo
echo "== 6. 关闭时间用验证码那一半（口令 + 当前码） =="
# ---------------------------------------------------------------------------
#
# 恢复码那一半上面验过了；这里验"关了再开、用下一个码关"，顺带把第 1 步再认证与
# 第 2 步关闭都用码走一遍——界面上的两步表单就是照这个顺序写的。

expect_eq "重新登记：再认证" "200" "$(reauth_with "$PASSWORD" '')"
expect_eq "重新登记：setup" "200" "$(setup_call)"
ENROLL2=$(jget "$TMPD/resp.json" 'd["data"]["enrollment_id"]')
SECRET2=$(totp_secret_of "$(jget "$TMPD/resp.json" 'd["data"]["otpauth_uri"]')")
wait_for_step_edge
expect_eq "重新登记：confirm" "200" "$(confirm_call "$ENROLL2" "$(totp_code "$SECRET2" 0)")"
expect_eq "重新登记之后又变成 10 枚恢复码" "10" "$(recovery_rows)"

# 第 1 步：口令 + 当前码（这一个码会被再认证用掉，所以第 2 步只能用下一个）。
CODE_A=$(fresh_code "$SECRET2")
expect_eq "关闭第 1 步：带码再认证" "200" "$(reauth_with "$PASSWORD" "$CODE_A")"
# 第 2 步：同一个码再用一次必须被拒（这正是界面上"请等下一个六位数"的由来）。
expect_eq "关闭第 2 步用刚用过的码 → 401" "401" "$(disable_call "$PASSWORD" "$CODE_A" '')"
expect_contains "拒因是'刚刚用过'" "刚刚用过" "$(jget "$TMPD/resp.json" 'd["error"]["message"]')"
expect_eq "被拒之后二次验证仍启用" "1" "$(totp_enabled_rows)"
CODE_B=$(fresh_code "$SECRET2")
expect_eq "关闭第 2 步用下一个码 → 200" "200" "$(disable_call "$PASSWORD" "$CODE_B" '')"
expect_eq "用码关闭之后状态行没了" "0" "$(totp_rows)"
expect_eq "用码关闭之后代理侧种子 destroyed" "destroyed" "$(seed_state "$ENROLL2")"

# ---------------------------------------------------------------------------
echo
echo "== 7. CLI 本机恢复：唯一的'丢了手机也丢了恢复码'出口 =="
# ---------------------------------------------------------------------------

expect_eq "恢复演练：再认证" "200" "$(reauth_with "$PASSWORD" '')"
expect_eq "恢复演练：setup" "200" "$(setup_call)"
ENROLL3=$(jget "$TMPD/resp.json" 'd["data"]["enrollment_id"]')
SECRET3=$(totp_secret_of "$(jget "$TMPD/resp.json" 'd["data"]["otpauth_uri"]')")
wait_for_step_edge
expect_eq "恢复演练：confirm" "200" "$(confirm_call "$ENROLL3" "$(totp_code "$SECRET3" 0)")"
RC3=$(jget "$TMPD/resp.json" 'd["data"]["recovery_codes"][0]')
expect_eq "恢复演练：库里 1 行状态 + 10 枚码" "1/10" "$(totp_enabled_rows)/$(recovery_rows)"

# root 跑：能清面板库，但代理那一半会被 SO_PEERCRED 拒掉（设计，不是缺陷）。
"$CTL" totp-reset --config "$SYS_CFG" --db "$WEB_DB" --username admin --json \
  >"$FACTS/ctl-reset-root.json" 2>"$FACTS/ctl-reset-root.err"
expect_eq "root 跑 totp-reset 退出码是 0（面板库这一半做成了）" "0" "$?"
chown_web_db
expect_eq "报告 state_cleared=true" "True" "$(jget "$FACTS/ctl-reset-root.json" 'd["state_cleared"]')"
expect_eq "报告清掉了 10 枚恢复码" "10" "$(jget "$FACTS/ctl-reset-root.json" 'd["recovery_codes_cleared"]')"
expect_eq "报告作废了 >= 1 条会话" "True" "$(jget "$FACTS/ctl-reset-root.json" 'd["revoked_sessions"] >= 1')"
expect_eq "root 身份下 agent_status=forbidden" "forbidden" "$(jget "$FACTS/ctl-reset-root.json" 'd["agent_status"]')"
expect_ne "forbidden 时带一句说明（不假装清干净了）" "" "$(jget "$FACTS/ctl-reset-root.json" 'd.get("agent_detail","")')"
expect_eq "CLI 输出里没有口令" "0" "$(plain_hits "$PASSWORD" "$FACTS/ctl-reset-root.json")"
expect_eq "CLI 输出里没有恢复码" "0" "$(plain_hits "$RC3" "$FACTS/ctl-reset-root.json")"
expect_eq "恢复之后面板库状态行没了" "0" "$(totp_rows)"
expect_eq "恢复之后恢复码没了" "0" "$(recovery_rows)"
# 如实记录残留：代理侧那条密文还在（root 被拒），但它已经无人引用。
expect_eq "代理侧种子仍在 persistent（root 被拒，孤儿密文留在那里）" "persistent" \
  "$(seed_state "$ENROLL3")"
expect_eq "全部会话作废：旧 jar 401" "401" "$(session_get "$JAR")"
csrf_from_session
expect_eq "恢复之后无码登录 200（二次验证真的没了）" "200" \
  "$(user_login admin "$PASSWORD" "$JAR" login-afterreset '')"
CSRF=$(login_get login-afterreset 'd["data"]["csrf_token"]')
expect_contains "审计 actor 是本机 CLI（不是某个账号）" "local-cli:" \
  "$(audit_last_actor auth.totp.reset)"
expect_eq "审计里 actor_id 为 NULL（没有人证明过自己是谁）" "1" \
  "$(dbq "$WEB_DB" "SELECT COUNT(*) FROM audit_events WHERE action='auth.totp.reset' AND actor_id IS NULL")"

# 以 upanel-web 身份跑：同一个账号、同一条命令，代理那一半也能做成（destroyed）。
expect_eq "第二轮演练：再认证" "200" "$(reauth_with "$PASSWORD" '')"
expect_eq "第二轮演练：setup" "200" "$(setup_call)"
ENROLL4=$(jget "$TMPD/resp.json" 'd["data"]["enrollment_id"]')
SECRET4=$(totp_secret_of "$(jget "$TMPD/resp.json" 'd["data"]["otpauth_uri"]')")
wait_for_step_edge
expect_eq "第二轮演练：confirm" "200" "$(confirm_call "$ENROLL4" "$(totp_code "$SECRET4" 0)")"
runuser -u "$WEB_USER" -- "$CTL" totp-reset --config "$SYS_CFG" --db "$WEB_DB" --username admin --json \
  >"$FACTS/ctl-reset-webuser.json" 2>"$FACTS/ctl-reset-webuser.err"
expect_eq "以 upanel-web 身份跑 totp-reset 退出码 0" "0" "$?"
chown_web_db
expect_eq "upanel-web 身份下 agent_status=destroyed" "destroyed" "$(jget "$FACTS/ctl-reset-webuser.json" 'd["agent_status"]')"
expect_eq "那条种子在代理库里变成 destroyed" "destroyed" "$(seed_state "$ENROLL4")"
expect_eq "只剩 root 那一轮留下的孤儿密文（persistent=1）" "1" "$(seed_count_by_state persistent)"
expect_eq "孤儿密文里也没有明文种子" "0" \
  "$(plain_hits "$SECRET3" "$AGENT_DB" "$AGENT_DB-wal")"
expect_eq "全部会话再次作废：旧 jar 401" "401" "$(session_get "$JAR")"
csrf_from_session
expect_eq "恢复之后无码登录 200" "200" \
  "$(user_login admin "$PASSWORD" "$JAR" login-afterreset2 '')"
CSRF=$(login_get login-afterreset2 'd["data"]["csrf_token"]')

# 关闭之后 CLI 再跑一次必须失败（没有可恢复的东西）——它不会拿"清空一个空表"当成功。
"$CTL" totp-reset --config "$SYS_CFG" --db "$WEB_DB" --username admin --no-agent --json \
  >"$FACTS/ctl-reset-again.out" 2>"$FACTS/ctl-reset-again.err"
expect_eq "已恢复之后再跑 totp-reset 退出码 1" "1" "$?"
expect_contains "再跑时的说明是'没有启用二次验证'" "没有启用二次验证" \
  "$(cat "$FACTS/ctl-reset-again.err" 2>/dev/null)"
chown_web_db

# ---------------------------------------------------------------------------
echo
echo "== 8. 前端产物里真有这一块（不是占位 dist） =="
# ---------------------------------------------------------------------------

curl -s -o "$FACTS/settings-index.html" "$BASE/settings"
expect_contains "设置页返回的是 SPA 的 index.html" 'id="app"' \
  "$(cat "$FACTS/settings-index.html" 2>/dev/null)"
BUNDLE=$(grep -o '/assets/[A-Za-z0-9._-]*\.js' "$FACTS/settings-index.html" 2>/dev/null | head -1)
if [ -n "$BUNDLE" ]; then
  # 产物是分片的：设置页与 /auth/totp 都不在入口 bundle 里，而是被动态 import 的
  # 独立 chunk（SettingsView-*.js、totp-*.js）。只抓入口那个文件会漏掉要验的东西，
  # 所以从 index.html 出发，把每个 chunk 里引用到的 chunk 名一代代抓下来（最多四轮），
  # 拼成一个文件再断言。抓不到的 chunk 在这里不会静默通过——断言会因为串不在而变红。
  : >"$FACTS/bundle.js"
  _seen=" $BUNDLE "
  _pending="$BUNDLE"
  _round=0
  _chunks=0
  while [ -n "$_pending" ] && [ "$_round" -lt 4 ]; do
    _round=$((_round + 1))
    _next=""
    for _chunk in $_pending; do
      _chunks=$((_chunks + 1))
      curl -s -o "$TMPD/chunk.js" "$BASE$_chunk"
      cat "$TMPD/chunk.js" >>"$FACTS/bundle.js"
      printf '\n/* ---- %s ---- */\n' "$_chunk" >>"$FACTS/bundle.js"
      for _name in $(grep -o 'assets/[A-Za-z0-9._-]*\.js' "$TMPD/chunk.js" 2>/dev/null | sort -u); do
        case "$_seen" in
          *" /$_name "*) ;;
          *)
            _seen="${_seen}/$_name "
            _next="$_next /$_name"
            ;;
        esac
      done
    done
    _pending=$_next
  done
  expect_ge "抓下来的分片不止入口那一个" 2 "$_chunks"
  BUNDLE_BYTES=$(wc -c <"$FACTS/bundle.js" 2>/dev/null | tr -d ' ')
  expect_ge "拿到的前端产物不是空文件" 1000 "$BUNDLE_BYTES"
  expect_ge "前端产物里有「二次验证」" 1 "$(grep -c -F '二次验证' "$FACTS/bundle.js" 2>/dev/null || true)"
  expect_ge "前端产物里有 /auth/totp 的三条路径" 1 "$(grep -c -F '/auth/totp' "$FACTS/bundle.js" 2>/dev/null || true)"
  expect_ge "前端产物里有一次性恢复码的提示" 1 "$(grep -c -F '只显示这一次' "$FACTS/bundle.js" 2>/dev/null || true)"
  expect_ge "前端产物里有 '同一个码不能用第二次' 这类说明" 1 \
    "$(grep -c -F '同一个码不能用第二次' "$FACTS/bundle.js" 2>/dev/null || true)"
else
  notrun "前端产物里有没有二次验证这一块" "二进制里没有 /assets/*.js（dist 是占位文件）——这一轮前端没构建"
fi

# ---------------------------------------------------------------------------
echo
echo "== 9. 结论 =="
# ---------------------------------------------------------------------------

notrun "真浏览器里的登记/关闭点击流程" "这台 VM 上没有浏览器；交互由前端单测 + 本轮的接口读数共同覆盖"
notrun "二维码图片" "P00 冻结的依赖集里没有 QR 编码器，界面按文本 + 复制按钮呈现 otpauth_uri；要不要引 QR 库是独立决策"
notrun "GET /auth/totp（还剩几枚恢复码）" "这一轮不开新接口；余量只能从 totp-reset 的输出与关闭流程看到"
notrun "手机与服务器时钟相差超过 ±1 步" "需要改 VM 时钟，会同时搅乱窗口/过期/限速；±1 步边界由 internal/totp 的 RFC 6238 向量覆盖"
notrun "登录限速打满（15 分钟 5 次）" "这一轮刻意只制造 4 次失败；打满之后所有断言都会变成 429"
notrun "root 跑 totp-reset 之后那条孤儿密文的彻底清理" "代理侧密文仍占一行（已无人引用）；彻底清扫是 P16 的运维项"

ls -la "$WEB_DB"* "$AGENT_DB"* >"$FACTS/databases-final.txt" 2>&1
dbq "$WEB_DB" "SELECT COUNT(*) FROM user_totp" >"$FACTS/final-user_totp.txt" 2>&1
dbq "$AGENT_DB" "SELECT state, COUNT(*) FROM secrets WHERE purpose='totp_seed' GROUP BY state" \
  >"$FACTS/final-totp-seeds.txt" 2>&1

printf 'PASS=%s FAIL=%s NOT RUN=%s\n' "$PASSED" "$FAILED" "$NTRUN"
write_result
if [ "$FAILED" -ne 0 ]; then
  printf '失败项：%s\n' "$FAILED_NAMES"
  exit 1
fi
exit 0
