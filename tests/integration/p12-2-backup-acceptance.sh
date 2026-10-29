#!/bin/sh
# P12.2 Linux 验收脚本：备份策略、运行、快照列表、一致性口径、停受管理应用备份、
# 面板 SQLite 在线备份。
#
# 要证明的不是"接口返回 200"，而是下面这些**很容易被糊过去**的事实：
#
#   1) **快照是读回来的，不是命令退出 0**：任务结果里 verified_by = restic stats，
#      snapshot_id 必须与脚本自己用同样口令跑 `restic snapshots --json` 读到的那条
#      逐字一致；再用 restic 自己把这份快照 restore 到临时目录，逐文件 sha256 与源比对
#      ——这一步用的是**独立工具**，不是产品的恢复接口（产品恢复是 P12.3）。
#   2) **备份定义只有一份真相**：面板不建 backup_* 表，/backup/* 整组是代理镜像的
#      编辑器；代理停掉之后整组回 503，不是"先登记回头同步"。
#   3) **版本令牌不是装饰**：PATCH 缺 version 面板先 400，版本不符代理 409，
#      backup.run 预检带旧 version 是 BACKUP_POLICY_VERSION_STALE。
#   4) **apps 由面板展开，代理双向核对**：调用方自己给 apps 直接 400；策略要停的应用
#      少给一个运行对象就 BACKUP_APP_UNAVAILABLE（不降级成停机不全的备份）；多给一个
#      就是扩大授权，同样拒。
#   5) **停 → 备份 → 拉起在真机上发生**：被停的 systemd 单元在备份结束后是 active，
#      且 ActiveEnterTimestamp 变过（说明它真的被停过一次又起来了，不是"一直在跑"）。
#      observed_state 里每个应用的 stop/start 都有 done 与 verified。
#   6) **面板库副本包含 WAL 里已提交的事务**：备份前 -wal 非空、备份后从快照里取回
#      的副本用 sqlite3 做 integrity_check，并且能看到备份前最后一次写库留下的行。
#      直接 os.Copy 主库文件做不到这一条。
#   7) **快照列表不谎报**：列表 size_included=false 且没有 size_bytes 字段；仓库没登记
#      是 registered=false（200），不是 404；快照不在是 found=false（200）。
#
# 用法（root，在验收虚拟机上）：
#   sudo BIN_DIR=/var/tmp/upanel-bin sh tests/integration/p12-2-backup-acceptance.sh
#
# 环境变量：BIN_DIR（默认 /var/tmp/upanel-bin）、WORK（默认 /var/tmp/upanel-accept-p12-2）、
# PORT（默认 19522）。
#
# 退出码：0 全部通过；1 有失败项；2 环境不满足。
#
#   8) **失败与取消不许假成功**：SFTP 真的写一份快照；跑到一半杀掉本次 restic 的
#      SFTP 客户端连接；独立 64 MiB tmpfs 仓库被真正写满；可取消任务最终落在 canceled。
#      三种现场都核对源文件哈希不变，已存在的快照仍能读，失败/取消不写成 verified。
#
# 明确不覆盖（NOT RUN，原因写在这里，不假装通过）：
#   - **产品的恢复接口**：恢复到新目录、覆盖恢复预检是 P12.3。本脚本里的 restore 是 restic
#     自己干的独立核对，不是产品功能验收。
#   - **保留策略真的清理**：retention 本阶段只登记不执行，清理是 P12.3 的 backup.prune。
#   - **浏览器渲染**：VM 上没有浏览器，备份 UI 属 P12.5。
#
# 证据卫生：跑完把 payloads 里含秘密明文的文件就地替换成说明；口令只经环境变量给 restic。

set -u

BIN_DIR=${BIN_DIR:-/var/tmp/upanel-bin}
WORK=${WORK:-/var/tmp/upanel-accept-p12-2}
PORT=${PORT:-19522}
AGENT="$BIN_DIR/upanel-agent"
WEB="$BIN_DIR/upanel-web"
CTL="$BIN_DIR/upanelctl"
WEB_USER=upanel-web
RUN_DIR=/run/upanel
SOCK=$RUN_DIR/agent.sock
SYS_CFG=/etc/upanel/upanel.json
KEY_FILE=/etc/upanel/agent.key
AGENT_DB_DIR=/var/lib/upanel-agent
BASE="http://127.0.0.1:$PORT"
ORIGIN="$BASE"
PASSWORD='p12-2-admin-horse-9'
VIEWER_PASSWORD='p12-2-viewer-horse-4'
REPO_PASSWORD='p12-2-repo-horse-7'
ROOT_PATH=$WORK/data-root
LOCAL_REL=backups/p12-2
REPO_PATH=$ROOT_PATH/$LOCAL_REL
RESTORE_DIR=$WORK/restore
PANEL_COPIES=$WORK/panel-copies
APP_UNIT=p12-2-backup-app.service
APP_UNIT_FILE=/etc/systemd/system/$APP_UNIT
SFTP_USER=p12sftp
SFTP_HOME=/home/$SFTP_USER
SFTP_REMOTE=$SFTP_HOME/p12-2-repo
SSH_DIR=$WORK/ssh
FULL_REPO=$ROOT_PATH/full-repo
FULL_SOURCE=$ROOT_PATH/data-full
FULL_MOUNTED=0
TMPD=/tmp/p12-2.$$
SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
SESSION_HASH=$(openssl rand -hex 32)

# 常驻 JSON 助手的通道（见 jget 上面的说明）。
JSON_TAB=$(printf '\t')
JSON_SEQ=0
JSON_IN=$TMPD/json.in
JSON_OUT=$TMPD/json.out
JSON_PID=""

PASSED=0
FAILED=0
NOTRUN=0
FAILED_NAMES=""
AGENT_PID=""
WEB_PID=""

pass() { PASSED=$((PASSED + 1)); printf 'PASS  %s\n' "$1"; printf 'PASS\t%s\n' "$1" >>"$WORK/logs/results.txt"; }
fail() {
  FAILED=$((FAILED + 1))
  FAILED_NAMES="$FAILED_NAMES | $1"
  printf 'FAIL  %s: %s\n' "$1" "${2:-}"
  printf 'FAIL\t%s\t%s\n' "$1" "${2:-}" >>"$WORK/logs/results.txt"
}
notrun() { NOTRUN=$((NOTRUN + 1)); printf 'NOT RUN  %s：%s\n' "$1" "$2"; printf 'NOTRUN\t%s\t%s\n' "$1" "$2" >>"$WORK/logs/results.txt"; }
note() { printf '      %s\n' "$1"; }
expect_eq() { if [ "$2" = "$3" ]; then pass "$1"; else fail "$1" "期望 '$2'，实际 '$3'"; fi; }
expect_contains() {
  case "$3" in
    *"$2"*) pass "$1" ;;
    *) fail "$1" "期望包含 '$2'，实际 '$3'" ;;
  esac
}
expect_ge() { if [ "$2" -ge "$3" ] 2>/dev/null; then pass "$1"; else fail "$1" "期望 >= $3，实际 '$2'"; fi; }

# stamp <标签>：往 logs/stamps.txt 打一个秒表点。
#
# 为什么需要它：这台 24.04 虚拟机是 QEMU TCG 纯软件模拟（无 KVM），预检要二十多秒、
# 再认证（Argon2id，128 MiB、t=3）要将近一分钟。预检有效期按契约是 120 秒，
# 脚本自己的每一步开销和产品每一步开销必须分得开看，否则分不清"产品慢"和"脚本慢"。
STAMP_T0=$(date -u +%s)
stamp() {
  printf '%s\t%s\t+%ss\n' "$(date -u +%H:%M:%S)" "$1" "$(( $(date -u +%s) - STAMP_T0 ))" \
    >>"$WORK/logs/stamps.txt"
}

# jget <文件> <python 表达式，d 为解析后的 JSON>
#
# 表达式出错时打印 ERR:<类型>:<消息>：空字符串会让"表达式写错"看起来像"响应里没这个字段"。
#
# 解释器是**常驻**的（tests/integration/json_helper.py），不是每次 `python3 -c`：
# 那台 QEMU TCG 软模拟机器上一次解释器启动要 8.6—17.7 秒（实测，负载 8.9），
# 而这一轮验收要问几百次 JSON。每次都付一次启动开销，两个后果：整轮拖慢一个数量级，
# 以及**把 120 秒的预检窗口吃掉**——真机实测「预检 21.9 秒 + 再认证 57.9 秒 +
# 脚本自身 45 秒」越界，提交因此 409。表达式语义一个字没改，改的只是谁来解释它。
jget() {
  JSON_SEQ=$((JSON_SEQ + 1))
  _seq=$JSON_SEQ
  # fd 7 = 助手的 stdin，fd 8 = 它的 stdout，在启动助手之后由 exec 打开。
  # 不能用路径：`<"$JSON_OUT"` 每次 read 都重新打开一次 fifo，dash 里的
  # `>&"$JSON_IN"` 更是直接报 Bad fd number（它要的是 fd 号，不是文件名）。
  printf '%s\tget\t%s\t%s\n' "$_seq" "$1" "$2" >&7
  IFS=$JSON_TAB read -r _got _val <&8
  if [ "${_got:-}" != "$_seq" ]; then printf 'ERR:helper-mismatch'; return 0; fi
  printf '%s' "$_val"
}

# jdumps <json 文本>：规范化成紧凑 JSON（原来那条 `python3 -c json.dumps`）。
jdumps() {
  JSON_SEQ=$((JSON_SEQ + 1))
  _seq=$JSON_SEQ
  printf '%s\tdumps\t%s\n' "$_seq" "$1" >&7
  IFS=$JSON_TAB read -r _got _val <&8
  if [ "${_got:-}" != "$_seq" ]; then printf 'ERR:helper-mismatch'; return 0; fi
  printf '%s' "$_val"
}

cleanup() {
  if [ -n "${JSON_PID:-}" ] && kill -0 "$JSON_PID" 2>/dev/null; then
    kill -TERM "$JSON_PID" 2>/dev/null
  fi
  if [ -n "${WEB_PID:-}" ] && kill -0 "$WEB_PID" 2>/dev/null; then
    kill -TERM "$WEB_PID" 2>/dev/null
    wait "$WEB_PID" 2>/dev/null
  fi
  if [ -n "${AGENT_PID:-}" ] && kill -0 "$AGENT_PID" 2>/dev/null; then
    kill -TERM "$AGENT_PID" 2>/dev/null
    wait "$AGENT_PID" 2>/dev/null
  fi
  systemctl stop "$APP_UNIT" >/dev/null 2>&1
  rm -f "$APP_UNIT_FILE"
  systemctl daemon-reload >/dev/null 2>&1
  if [ "$FULL_MOUNTED" -eq 1 ]; then
    umount "$FULL_REPO" >/dev/null 2>&1
    FULL_MOUNTED=0
  fi
  rm -rf "$TMPD"
  return 0
}
trap cleanup EXIT
trap 'cleanup; exit 130' INT
trap 'cleanup; exit 143' TERM

# ---------------------------------------------------------------- 环境检查
if [ "$(id -u)" -ne 0 ]; then
  printf '本脚本必须以 root 运行：sudo BIN_DIR=%s sh %s\n' "$BIN_DIR" "$0" >&2
  exit 2
fi
for f in "$AGENT" "$WEB" "$CTL"; do
  if [ ! -x "$f" ]; then printf '缺少可执行文件：%s\n' "$f" >&2; exit 2; fi
  # 四字节魔术数字：把 Windows 编出来的 PE 推上来时，症状是十几个连锁失败，全在别处。
  if [ "$(head -c 4 "$f" | od -An -tx1 | tr -d ' \n')" != "7f454c46" ]; then
    printf '%s 不是本机的 ELF 可执行文件\n' "$f" >&2
    exit 2
  fi
done
if [ ! -x /usr/bin/restic ]; then
  echo '本机没有 /usr/bin/restic（apt install restic），P12.2 的验收无法进行' >&2
  exit 2
fi
for c in ssh ssh-keygen ssh-keyscan; do
  if ! command -v "$c" >/dev/null 2>&1; then
    echo "本机没有 $c，SFTP 备份与中途断线验收无法进行" >&2
    exit 2
  fi
done
if ! ss -ltnH 2>/dev/null | grep -q ':22 '; then
  echo '本机 sshd 没有监听 22，SFTP 备份与中途断线验收无法进行' >&2
  exit 2
fi
if ! id -u "$WEB_USER" >/dev/null 2>&1; then
  printf '本机没有 %s 账号（先装一遍 0.1 包）\n' "$WEB_USER" >&2
  exit 2
fi
if ! command -v systemctl >/dev/null 2>&1; then
  echo '本机没有 systemctl，停受管理应用的验收无法进行' >&2
  exit 2
fi
if pgrep -x upanel-agent >/dev/null 2>&1 || pgrep -x upanel-web >/dev/null 2>&1; then
  printf '这台机器上已经有 upanel 进程在跑，先停掉再验收\n' >&2
  exit 2
fi

rm -rf "$WORK"
mkdir -p "$WORK/logs" "$WORK/payloads" "$ROOT_PATH/data" "$RESTORE_DIR"
# 代理库与 Web 库都用**本次工作目录下**的：策略与仓库定义是持久状态，沿用上一轮的库
# 会让"名字已被占用"这种连锁失败出现在第三段，看上去像是创建接口坏了。
install -d -m 0700 "$WORK/agent"
AGENT_DB=$WORK/agent/agent.db
WEB_DB_DIR=$WORK/webdata
WEB_DB=$WEB_DB_DIR/upanel.db
install -d -m 0755 "$WEB_DB_DIR"
chmod 0755 "$WORK"
mkdir -p "$TMPD"
chmod 1777 "$TMPD"   # runuser 要以 upanel-web 的身份在里面写响应文件

# 常驻 JSON 助手：解释器只启动一次，之后一问一答。
# 放在这里而不是第一次 jget 时，是为了让那 8—18 秒的启动开销发生在等待里，
# 而不是发生在预检与提交之间那 120 秒的窗口里。
rm -f "$JSON_IN" "$JSON_OUT"
mkfifo "$JSON_IN" "$JSON_OUT"
python3 "$SCRIPT_DIR/json_helper.py" <"$JSON_IN" >"$JSON_OUT" 2>>"$TMPD/json_helper.log" &
JSON_PID=$!
# 打开一次，之后整轮都走这两个 fd。打开顺序有讲究：fifo 的读端与写端要配成对才
# 返回，先 7 后 8 正好与助手的 <stdin >stdout 对上，谁先谁后都不会死锁。
exec 7>"$JSON_IN" 8<"$JSON_OUT"

RESULTS=$WORK/logs/results.txt
AGENT_LOG=$WORK/logs/agent.log
WEB_LOG=$WORK/logs/web.log
PAYLOADS=$WORK/payloads
: >"$RESULTS"
: >"$AGENT_LOG"
: >"$WEB_LOG"

echo "== 0. 这台机器上的 restic / systemd 现场 =="
note "restic：$(restic version 2>&1 | head -1)"
note "systemd：$(systemctl --version 2>&1 | head -1)"
{
  echo "# restic version"; restic version 2>&1
  echo; echo "# systemctl --version"; systemctl --version 2>&1 | head -2
  echo; echo "# sha256sum of binaries"
  sha256sum "$AGENT" "$WEB" "$CTL"
} >"$PAYLOADS/env.txt" 2>&1
pass "env-records-versions"

# ---------------------------------------------------------------- 代理侧调用（以 upanel-web 的身份）
web_curl() {
  _m=$1; _p=$2; _o=$3; _b=${4:-}
  _tmp=$TMPD/out.json
  rm -f "$_tmp"
  if [ -n "$_b" ]; then
    CODE=$(runuser -u "$WEB_USER" -- curl -s -o "$_tmp" -w '%{http_code}' -X "$_m" \
      --unix-socket "$SOCK" -H 'Content-Type: application/json' \
      --data-binary "@$_b" "http://agent$_p" 2>/dev/null)
  else
    CODE=$(runuser -u "$WEB_USER" -- curl -s -o "$_tmp" -w '%{http_code}' -X "$_m" \
      --unix-socket "$SOCK" "http://agent$_p" 2>/dev/null)
  fi
  if [ -f "$_tmp" ]; then cp "$_tmp" "$_o" 2>/dev/null; fi
  [ -f "$_o" ] || : >"$_o"
  printf '%s' "$CODE"
}

json() {
  _path=$TMPD/$1.json
  printf '%s' "$2" >"$_path"
  chmod 0644 "$_path"
  cp "$_path" "$PAYLOADS/$1.json" 2>/dev/null
  printf '%s' "$_path"
}

# agent_pf <名字> <动作> <参数 JSON>：代理侧预检。
agent_pf() {
  _body=$(printf '{"action":"%s","params":%s,"actor_context":{"user_id":1,"session_id_hash":"%s","request_id":"p12-2-%s","reauth_at":"%s"}}' \
    "$2" "$3" "$SESSION_HASH" "$1" "$(date -u +%Y-%m-%dT%H:%M:%SZ)")
  _code=$(web_curl POST /internal/v1/preflights "$PAYLOADS/preflight-$1.json" "$(json preflight-$1 "$_body")")
  printf '%s' "$_code"
}
apget() { jget "$PAYLOADS/preflight-$1.json" "$2"; }
apblockers() { apget "$1" 'json.dumps([b["code"] for b in d["data"]["blockers"]])'; }
apmessages() { apget "$1" 'json.dumps([b["message"] for b in d["data"]["blockers"]], ensure_ascii=False)'; }

run_op() {
  _opid="op_$(openssl rand -hex 16)"
  _body=$(printf '{"operation_id":"%s","preflight_ref":"%s","confirmation":"","actor_context":{"user_id":1,"session_id_hash":"%s","request_id":"p12-2-%s-op","reauth_at":"%s"}}' \
    "$_opid" "$2" "$SESSION_HASH" "$1" "$(date -u +%Y-%m-%dT%H:%M:%SZ)")
  _code=$(web_curl POST /internal/v1/operations "$PAYLOADS/operation-$1.json" "$(json operation-submit-$1 "$_body")")
  if [ "$_code" != "200" ] && [ "$_code" != "201" ] && [ "$_code" != "202" ]; then
    printf 'submit:%s' "$_code"; return 0
  fi
  _stage=""
  for _ in $(seq 1 240); do
    web_curl GET "/internal/v1/operations/$_opid" "$PAYLOADS/operation-$1.json" >/dev/null
    _stage=$(opget "$1" 'd["data"]["stage"]')
    case "$_stage" in
      succeeded|failed|canceled|interrupted) break ;;
    esac
    sleep 0.5
  done
  printf '%s' "${_stage:-timeout}"
}
opget() { jget "$PAYLOADS/operation-$1.json" "$2"; }

put_secret() { # put_secret <名字> <用途> <值>
  _req=$TMPD/secret.req.json
  # 走常驻助手而不是 `python3 -c`：这台机器上一次解释器启动要 8—17 秒。
  # 值是脚本里写死的测试口令（不含引号与反斜杠），所以直接拼 JSON 文本。
  jdumps "{\"purpose\": \"$2\", \"value\": \"$3\", \"reference\": \"p12.2\"}" >"$_req"
  chmod 0644 "$_req"
  _code=$(web_curl POST /internal/v1/secrets "$PAYLOADS/secret-$1.json" "$_req")
  rm -f "$_req"
  printf '%s' "$_code"
}
secret_of() { jget "$PAYLOADS/secret-$1.json" 'd["data"]["secret_ref"]'; }

# put_secret_file <名字> <用途> <文件>：多行秘密（如 PEM）不能塞进 jdump 的一行协议。
# 这是设置阶段的一次性 Python 调用，不在预检窗口里；明文只进临时请求体，发出后即删。
put_secret_file() {
  _req=$TMPD/secret.req.json
  python3 -c 'import json,sys
with open(sys.argv[2], encoding="utf-8") as fh:
    value = fh.read()
print(json.dumps({"purpose": sys.argv[1], "value": value, "reference": "p12.2"}, ensure_ascii=False))' \
    "$2" "$3" >"$_req"
  chmod 0644 "$_req"
  _code=$(web_curl POST /internal/v1/secrets "$PAYLOADS/secret-$1.json" "$_req")
  rm -f "$_req"
  printf '%s' "$_code"
}

# restic_q <参数…>：用环境变量给口令（不进 argv），输出留给调用方。
restic_q() {
  RESTIC_PASSWORD="$REPO_PASSWORD" restic -r "$REPO_PATH" --no-cache "$@"
}

# ---------------------------------------------------------------- Web 侧调用
login() {
  curl -s -c "$3" -o "$WORK/$4.json" -w '%{http_code}' -X POST "$BASE/api/v1/auth/login" \
    -H 'Content-Type: application/json' -H 'X-UPanel-Request: 1' -H "Origin: $ORIGIN" \
    -d "{\"username\":\"$1\",\"password\":\"$2\"}"
}
csrf_of() { jget "$1" 'd["data"]["csrf_token"]'; }

# api <jar> <csrf> <方法> <路径> [body] [响应留档名]
api() {
  _jar=$1; _csrf=$2; _method=$3; _path=$4; _body=${5:-}; _keep=${6:-}
  _code=""
  if [ -n "$_body" ]; then
    printf '%s' "$_body" >"$WORK/req.json"
    _code=$(curl -s -b "$_jar" -o "$WORK/resp.json" -w '%{http_code}' -X "$_method" "$BASE$_path" \
      -H 'Content-Type: application/json' -H "Origin: $ORIGIN" -H "X-CSRF-Token: $_csrf" \
      -d @"$WORK/req.json")
  else
    _code=$(curl -s -b "$_jar" -o "$WORK/resp.json" -w '%{http_code}' -X "$_method" "$BASE$_path" \
      -H "Origin: $ORIGIN" -H "X-CSRF-Token: $_csrf")
  fi
  if [ -n "$_keep" ]; then cp "$WORK/resp.json" "$PAYLOADS/$_keep.json" 2>/dev/null; fi
  printf '%s' "$_code"
}
rget() { jget "$WORK/resp.json" "$1"; }

# preflight <动作> <参数 JSON> [留档名]：Web 侧预检（展开发生在这里）。
preflight() {
  printf '{"action":"%s","params":%s}' "$1" "$2" >"$WORK/pre.json"
  _code=$(curl -s -b "$JAR" -o "$WORK/resp.json" -w '%{http_code}' -X POST \
    "$BASE/api/v1/operations/preflight" \
    -H 'Content-Type: application/json' -H "Origin: $ORIGIN" -H "X-CSRF-Token: $CSRF" \
    -d @"$WORK/pre.json")
  if [ -n "${3:-}" ]; then cp "$WORK/resp.json" "$PAYLOADS/$3.json" 2>/dev/null; fi
  cp "$WORK/resp.json" "$WORK/preflight.json"
  printf '%s' "$_code"
}
pget() { jget "$WORK/preflight.json" "$1"; }
pblockers() { pget 'json.dumps([b["code"] for b in (d["data"].get("blockers") or [])])'; }
pwarnings() { pget 'json.dumps([w["code"] for w in (d["data"].get("warnings") or [])])'; }

submit_job() {
  # preflight_id 是 `pf_` + 十六进制，直接拼；原来那条 `python3 -c json.dumps`
  # 在这台慢机器上要 8—17 秒，而它就落在预检与提交之间的窗口里。
  printf '{"preflight_id": "%s"}' "$1" >"$WORK/jobreq.json"
  _code=$(curl -s -b "$JAR" -o "$WORK/resp.json" -w '%{http_code}' -X POST "$BASE/api/v1/jobs" \
    -H 'Content-Type: application/json' -H "Origin: $ORIGIN" -H "X-CSRF-Token: $CSRF" \
    -H "Idempotency-Key: $2" -d @"$WORK/jobreq.json")
  cp "$WORK/resp.json" "$WORK/submit.json"
  printf '%s' "$_code"
}
wait_job() {
  if [ -z "${1:-}" ]; then printf 'no-job-id'; return 0; fi
  _i=0
  while [ "$_i" -lt 600 ]; do
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
# job_id_of <文件>：取任务 id，取不到就返回空串。
#
# 不能直接用 jget：jget 在表达式出错时返回 "ERR:KeyError:'data'"，而调用方会把这串
# 当成任务 id 拼进 URL——提交失败之后脚本就对着一个不存在的任务空转几百次，
# 看上去像"任务卡住"，其实是提交那一步就失败了。
job_id_of() {
  _v=$(jget "$1" 'd["data"]["job_id"]')
  case "$_v" in
    ERR:*) printf '' ;;
    *) printf '%s' "$_v" ;;
  esac
}
jobget() { jget "$WORK/job.json" "$1"; }

# begin_backup_job <策略 id> <版本> <留档名>
# 只负责“再认证 → 预检 → 提交”，不等待终态；断网、写满和取消都要在这个窗口里动手。
# 结果放在 JOB_* 全局变量里，调用方逐条断言，不能把空 id 当成任务继续等。
begin_backup_job() {
  _policy=$1
  _version=$2
  _name=$3
  JOB_REAUTH_CODE=$(api "$JAR" "$CSRF" POST /api/v1/auth/reauth "{\"password\":\"$PASSWORD\"}")
  JOB_PREFLIGHT_CODE=$(preflight backup.run \
    "{\"backup_policy_id\":\"$_policy\",\"version\":$_version}" "preflight-$_name")
  JOB_ALLOWED=$(pget 'd["data"]["allowed"]')
  _pref=$(pget 'd["data"]["preflight_id"]')
  JOB_SUBMIT_CODE=$(submit_job "$_pref" "p12-2-$_name-$(openssl rand -hex 8)")
  cp "$WORK/submit.json" "$PAYLOADS/submit-$_name.json"
  JOB_ID=$(job_id_of "$WORK/submit.json")
}

file_sha() { sha256sum "$1" 2>/dev/null | awk '{print $1}'; }

# ---------------------------------------------------------------- 1. 起服务
echo
echo "== 1. 启动代理（root）与 Web（非 root） =="
install -d -m 0755 /etc/upanel
install -d -m 0700 "$AGENT_DB_DIR"
install -d -m 0755 "$RUN_DIR"
cat >"$SYS_CFG" <<JSON
{
  "web": { "mode": "production", "listen_address": "127.0.0.1:$PORT", "shutdown_timeout_seconds": 10 },
  "agent": {
    "socket_path": "$SOCK",
    "shutdown_timeout_seconds": 10,
    "web_user": "$WEB_USER",
    "socket_group": "$WEB_USER",
    "database_path": "$AGENT_DB",
    "secret_key_path": "$KEY_FILE",
    "storage": { "smart_enabled": false, "smart_interval_minutes": 15, "network_capacity_timeout_seconds": 2 },
    "services": {},
    "logs": {},
    "files": { "roots": [ { "id": "root1", "name": "验收数据", "path": "$ROOT_PATH", "writable": true } ] },
    "backup": { "panel_database_path": "$WEB_DB", "panel_copy_dir": "$PANEL_COPIES" }
  },
  "database": { "path": "$WEB_DB", "busy_timeout_ms": 5000 },
  "log": { "level": "info", "format": "json" }
}
JSON
chmod 0644 "$SYS_CFG"

if [ -f "$KEY_FILE" ]; then
  pass "agent-key-present"
elif "$AGENT" --config "$SYS_CFG" -init-key >>"$WORK/logs/init-key.log" 2>&1; then
  pass "agent-key-created"
else
  fail "agent-key-created" "$(tail -3 "$WORK/logs/init-key.log" 2>/dev/null)"
fi

# 不 rm -f socket：陈旧/占用由代理自己判断（Q324）。
"$AGENT" --config "$SYS_CFG" >>"$AGENT_LOG" 2>&1 &
AGENT_PID=$!
for _ in $(seq 1 100); do
  if [ -S "$SOCK" ] && runuser -u "$WEB_USER" -- curl -fsS -o /dev/null \
       --unix-socket "$SOCK" http://agent/internal/v1/capabilities 2>/dev/null; then
    break
  fi
  sleep 0.1
done
if runuser -u "$WEB_USER" -- curl -fsS -o /dev/null --unix-socket "$SOCK" \
     http://agent/internal/v1/capabilities 2>/dev/null; then
  pass "agent-started"
else
  fail "agent-started" "代理没起来（见 $AGENT_LOG）"
fi
expect_eq "agent-runs-as-root" "0" "$(ps -o uid= -p "$AGENT_PID" 2>/dev/null | tr -d ' ')"
expect_contains "agent-logs-backup-run-enabled" "已启用备份仓库与备份运行" "$(cat "$AGENT_LOG")"

CAP=$PAYLOADS/capabilities.json
CODE=$(web_curl GET /internal/v1/capabilities "$CAP")
expect_eq "capabilities-200" "200" "$CODE"
expect_eq "capabilities-has-backup-run" "yes" \
  "$(jget "$CAP" '"yes" if "backup.run" in d["data"]["supported_actions"] else "no"')"
expect_eq "capabilities-has-snapshots" "yes" \
  "$(jget "$CAP" '"yes" if "backup.snapshots" in d["data"]["supported_queries"] else "no"')"

printf '%s\n%s\n' "$PASSWORD" "$PASSWORD" | "$CTL" init-admin --config "$SYS_CFG" --db "$WEB_DB" \
  --username admin --display-name "管理员" --json >"$WORK/init.out" 2>"$WORK/init.err"
expect_eq "init-admin" "0" "$?"
chown -R "$WEB_USER:$WEB_USER" "$WEB_DB_DIR"

runuser -u "$WEB_USER" -- "$WEB" --mode production --db "$WEB_DB" --listen "127.0.0.1:$PORT" \
  >>"$WEB_LOG" 2>&1 &
WEB_PID=$!
WEB_UP=no
for _ in $(seq 1 100); do
  if curl -fsS "$BASE/api/v1/health/live" >/dev/null 2>&1; then WEB_UP=yes; break; fi
  sleep 0.2
done
expect_eq "web-started" "yes" "$WEB_UP"
WEB_UID=$(ps -eo uid=,comm= | awk '$2=="upanel-web" {print $1; exit}')
if [ -n "$WEB_UID" ] && [ "$WEB_UID" != "0" ]; then
  pass "web-not-root"
else
  # WEB_PID 是 runuser 的 pid（root），要看真正监听的那个进程。
  fail "web-not-root" "uid=${WEB_UID:-找不到 upanel-web 进程}"
fi

JAR=$WORK/admin.cookies
CODE=$(login admin "$PASSWORD" "$JAR" login-admin)
expect_eq "login-admin" "200" "$CODE"
CSRF=$(csrf_of "$WORK/login-admin.json")
cp "$WORK/login-admin.json" "$PAYLOADS/login-admin.json" 2>/dev/null

# viewer：备份这一组整组 admin，viewer 必须是 403，不是"只读可看"。
CODE=$(api "$JAR" "$CSRF" POST /api/v1/users \
  "{\"username\":\"p12watcher\",\"display_name\":\"p12watcher\",\"role\":\"viewer\",\"initial_password\":\"$VIEWER_PASSWORD\"}")
expect_eq "create-viewer" "201" "$CODE"
VIEWER_JAR=$WORK/viewer.cookies
CODE=$(login p12watcher "$VIEWER_PASSWORD" "$VIEWER_JAR" login-viewer)
expect_eq "login-viewer" "200" "$CODE"
VIEWER_CSRF=$(csrf_of "$WORK/login-viewer.json")

echo
echo "== 2. 秘密与仓库定义（仓库写操作要再认证） =="
expect_eq "secret-repo-password" "201" "$(put_secret repopass backup_repository_password "$REPO_PASSWORD")"
SEC_PASS=$(secret_of repopass)
expect_contains "secret-ref-shape" "sec_" "$SEC_PASS"

REPO_DEF="{\"name\":\"p12-2 本地仓库\",\"backend\":\"local\",\"location\":{\"root_id\":\"root1\",\"relative_path\":\"$LOCAL_REL\"},\"password_secret_ref\":\"$SEC_PASS\"}"
# 没再认证就写仓库：403。这条必须在 201 之前，否则"再认证"只是文档上的一句话。
CODE=$(api "$JAR" "$CSRF" POST /api/v1/backup/repositories "$REPO_DEF" repo-no-reauth)
expect_eq "repo-create-requires-reauth" "403" "$CODE"
CODE=$(api "$JAR" "$CSRF" POST /api/v1/auth/reauth "{\"password\":\"$PASSWORD\"}")
expect_eq "reauth-admin" "200" "$CODE"
CODE=$(api "$JAR" "$CSRF" POST /api/v1/backup/repositories "$REPO_DEF" repo-create)
expect_eq "repo-create-201" "201" "$CODE"
REP=$(jget "$PAYLOADS/repo-create.json" 'd["data"]["id"]')
expect_contains "repo-id-shape" "rep_" "$REP"
expect_eq "repo-create-viewer-forbidden" "403" \
  "$(api "$VIEWER_JAR" "$VIEWER_CSRF" POST /api/v1/backup/repositories "$REPO_DEF")"
expect_eq "repo-list-viewer-forbidden" "403" \
  "$(api "$VIEWER_JAR" "$VIEWER_CSRF" GET /api/v1/backup/repositories)"
CODE=$(api "$JAR" "$CSRF" GET /api/v1/backup/repositories "" repo-list)
expect_eq "repo-list-200" "200" "$CODE"
expect_eq "repo-list-state-query" "backup.repository.state" \
  "$(jget "$PAYLOADS/repo-list.json" 'd["data"]["repository_state_query"]')"
# local 后端带 SFTP 字段：400，不是"忽略多余的"。
CODE=$(api "$JAR" "$CSRF" POST /api/v1/backup/repositories \
  "{\"name\":\"bad\",\"backend\":\"local\",\"location\":{\"root_id\":\"root1\",\"relative_path\":\"a\",\"host\":\"h\"},\"password_secret_ref\":\"$SEC_PASS\"}")
expect_eq "repo-local-rejects-sftp-fields" "400" "$CODE"

# 初始化真实仓库：走代理动作（Web 侧没有初始化按钮，属 P12.1 的动作）。
CODE=$(agent_pf init backup.repository.initialize "{\"repository_id\":\"$REP\",\"version\":1}")
expect_eq "init-preflight-201" "201" "$CODE"
expect_eq "init-preflight-allowed" "True" "$(apget init 'd["data"]["allowed"]')"
PREF=$(apget init 'd["data"]["preflight_ref"]')
expect_eq "init-run-succeeded" "succeeded" "$(run_op init "$PREF")"
REPO_ID=$(opget init 'd["data"]["result"]["data"]["repo_id"]')
expect_eq "init-repo-id-64hex" "64" "$(printf '%s' "$REPO_ID" | wc -c)"

# 独立核对：用同样的口令自己读一次 config。两边都是 restic 读的，但读取路径不同。
restic_q cat config >"$PAYLOADS/independent-cat-config.json" 2>"$PAYLOADS/independent-cat-config.stderr"
INDEP_ID=$(jget "$PAYLOADS/independent-cat-config.json" 'd["id"]')
if [ -n "$REPO_ID" ] && [ "$REPO_ID" = "$INDEP_ID" ]; then
  pass "independent-repo-id-matches"
else
  fail "independent-repo-id-matches" "任务报的 $REPO_ID，独立读的是 $INDEP_ID"
fi

echo
echo "== 3. 策略：CRUD、版本令牌、口径闭集 =="
POL_BODY="{\"name\":\"p12-2 数据目录\",\"repository_id\":\"$REP\",\"sources\":[{\"kind\":\"root\",\"root_id\":\"root1\",\"relative_path\":\"data\"}],\"consistency_mode\":\"files_only\",\"retention\":{\"keep_last\":7}}"
CODE=$(api "$JAR" "$CSRF" POST /api/v1/backup/policies "$POL_BODY" policy-create)
expect_eq "policy-create-201" "201" "$CODE"
POL=$(jget "$PAYLOADS/policy-create.json" 'd["data"]["id"]')
expect_contains "policy-id-shape" "pol_" "$POL"
expect_eq "policy-create-viewer-forbidden" "403" \
  "$(api "$VIEWER_JAR" "$VIEWER_CSRF" POST /api/v1/backup/policies "$POL_BODY")"
# 名字重复：400（请求本身不合法），不是 409。
CODE=$(api "$JAR" "$CSRF" POST /api/v1/backup/policies "$POL_BODY")
expect_eq "policy-duplicate-name-400" "400" "$CODE"
# 口径与 app_ids 互相约束：两个方向都是 400。
CODE=$(api "$JAR" "$CSRF" POST /api/v1/backup/policies \
  "{\"name\":\"bad1\",\"repository_id\":\"$REP\",\"sources\":[{\"root_id\":\"root1\",\"relative_path\":\"data\"}],\"consistency_mode\":\"stop_managed_apps\",\"retention\":{}}")
expect_eq "policy-stop-apps-required" "400" "$CODE"
CODE=$(api "$JAR" "$CSRF" POST /api/v1/backup/policies \
  "{\"name\":\"bad2\",\"repository_id\":\"$REP\",\"sources\":[{\"root_id\":\"root1\",\"relative_path\":\"data\"}],\"consistency_mode\":\"files_only\",\"app_ids\":[\"app_0123456789abcdef0123456789abcdef\"],\"retention\":{}}")
expect_eq "policy-files-only-rejects-apps" "400" "$CODE"
# PATCH：缺 version 面板先 400；带旧 version 代理 409。
CODE=$(api "$JAR" "$CSRF" PATCH "/api/v1/backup/policies/$POL" "$POL_BODY")
expect_eq "policy-patch-missing-version-400" "400" "$CODE"
CODE=$(api "$JAR" "$CSRF" PATCH "/api/v1/backup/policies/$POL" \
  "{\"name\":\"p12-2 数据目录\",\"repository_id\":\"$REP\",\"sources\":[{\"kind\":\"root\",\"root_id\":\"root1\",\"relative_path\":\"data\"}],\"consistency_mode\":\"files_only\",\"retention\":{\"keep_last\":7},\"version\":99}")
expect_eq "policy-patch-stale-version-409" "409" "$CODE"
CODE=$(api "$JAR" "$CSRF" PATCH "/api/v1/backup/policies/$POL" \
  "{\"name\":\"p12-2 数据目录 v2\",\"repository_id\":\"$REP\",\"sources\":[{\"kind\":\"root\",\"root_id\":\"root1\",\"relative_path\":\"data\"}],\"consistency_mode\":\"files_only\",\"retention\":{\"keep_last\":7},\"version\":1}" policy-patch)
expect_eq "policy-patch-200" "200" "$CODE"
expect_eq "policy-patch-version-bumped" "2" \
  "$(jget "$PAYLOADS/policy-patch.json" 'd["data"]["version"]')"
# 删除只删策略：回 200 + 空对象（统一信封），仓库还在。
CODE=$(api "$JAR" "$CSRF" POST /api/v1/backup/policies \
  "{\"name\":\"p12-2 待删\",\"repository_id\":\"$REP\",\"sources\":[{\"root_id\":\"root1\",\"relative_path\":\"data\"}],\"consistency_mode\":\"files_only\",\"retention\":{}}" policy-doomed)
POL_DOOMED=$(jget "$PAYLOADS/policy-doomed.json" 'd["data"]["id"]')
CODE=$(api "$JAR" "$CSRF" DELETE "/api/v1/backup/policies/$POL_DOOMED" "" policy-delete)
expect_eq "policy-delete-200" "200" "$CODE"
expect_eq "policy-delete-envelope-empty" "{}" \
  "$(jget "$PAYLOADS/policy-delete.json" 'json.dumps(d["data"])')"
CODE=$(api "$JAR" "$CSRF" GET "/api/v1/backup/repositories/$REP")
expect_eq "repo-survives-policy-delete" "200" "$CODE"

# 代理镜像的默认拒绝：root 也不能碰。
CODE=$(curl -s -o "$TMPD/root.json" -w '%{http_code}' --unix-socket "$SOCK" \
  http://agent/internal/v1/backup/policies 2>/dev/null)
expect_eq "policy-mirror-denies-root" "403" "$CODE"

echo
echo "== 4. backup.run 的参数与预检（不真跑的那些） =="
# 版本不符：BACKUP_POLICY_VERSION_STALE。
CODE=$(agent_pf stale backup.run "{\"backup_policy_id\":\"$POL\",\"version\":1}")
expect_eq "run-stale-preflight-201" "201" "$CODE"
expect_contains "run-stale-version-blocked" "BACKUP_POLICY_VERSION_STALE" "$(apblockers stale)"
# 策略不存在。
CODE=$(agent_pf ghost backup.run "{\"backup_policy_id\":\"pol_00000000000000000000000000000000\",\"version\":1}")
expect_contains "run-unknown-policy-blocked" "BACKUP_POLICY_NOT_FOUND" "$(apblockers ghost)"
# 调用方自己给 apps：Web 严格解码直接 400。
CODE=$(preflight backup.run "{\"backup_policy_id\":\"$POL\",\"version\":2,\"apps\":[]}" preflight-apps-supplied)
expect_eq "run-caller-apps-400" "400" "$CODE"
# 源不存在：另建一条指向不存在的相对路径的策略（目录还没造），预检必须挡住。
CODE=$(api "$JAR" "$CSRF" POST /api/v1/backup/policies \
  "{\"name\":\"p12-2 不存在源\",\"repository_id\":\"$REP\",\"sources\":[{\"kind\":\"root\",\"root_id\":\"root1\",\"relative_path\":\"data/no-such-dir\"}],\"consistency_mode\":\"files_only\",\"retention\":{}}" policy-missing-src)
POL_MISSING=$(jget "$PAYLOADS/policy-missing-src.json" 'd["data"]["id"]')
CODE=$(agent_pf missingsrc backup.run "{\"backup_policy_id\":\"$POL_MISSING\",\"version\":1}")
expect_contains "run-missing-source-blocked" "BACKUP_SOURCE" "$(apblockers missingsrc)"

echo
echo "== 5. 真跑一次 files_only 备份，并用 restic 独立恢复核对 =="
# 造一份内容确定的源：二进制 + 文本 + 子目录，哈希先算好。
mkdir -p "$ROOT_PATH/data/sub"
printf 'p12-2 line one\n' >"$ROOT_PATH/data/a.txt"
printf 'p12-2 nested\n' >"$ROOT_PATH/data/sub/b.txt"
head -c 4096 /dev/urandom >"$ROOT_PATH/data/blob.bin"
sha256sum "$ROOT_PATH/data/a.txt" "$ROOT_PATH/data/sub/b.txt" "$ROOT_PATH/data/blob.bin" \
  >"$PAYLOADS/source-hashes.txt"
note "源哈希：$(cat "$PAYLOADS/source-hashes.txt" | tr '\n' ' ')"

# 再认证放在预检**之前**。理由是实测出来的：预检有效期按契约是 120 秒（Q53），
# 而这台软模拟机器上一次 Argon2id 再认证要 57.9 秒。把再认证放在窗口里，
# 窗口就只剩一半留给"看计划 → 提交"，慢机器上必然 409。契约要求的是
# "提交前五分钟内完成过再认证"（PLAN 安全条款），先认证后预检同样满足；
# 界面上的对应顺序是"打开备份页 → 验证身份 → 生成计划 → 确认执行"。
stamp "s5-reauth-start"
CODE=$(api "$JAR" "$CSRF" POST /api/v1/auth/reauth "{\"password\":\"$PASSWORD\"}")
expect_eq "reauth-before-run" "200" "$CODE"
stamp "s5-reauth-done"
CODE=$(preflight backup.run "{\"backup_policy_id\":\"$POL\",\"version\":2}" preflight-run-files)
expect_eq "run-files-preflight-200" "200" "$CODE"
stamp "s5-preflight-done"
expect_eq "run-files-preflight-allowed" "True" "$(pget 'd["data"]["allowed"]')"
# files_only 必须说出"不保证正在写入的数据库一致"，不能只报"可以跑"。
expect_contains "run-files-warns-consistency" "BACKUP" "$(pwarnings)"
expect_eq "run-files-apps-empty" "[]" \
  "$(pget 'json.dumps(d["data"]["normalized_params"].get("apps"))')"
PREF_ID=$(pget 'd["data"]["preflight_id"]')
stamp "s5-preflight-id-read"
CODE=$(submit_job "$PREF_ID" "p12-2-run-files-$(openssl rand -hex 8)")
stamp "s5-submit-done"
expect_eq "run-files-submit-202" "202" "$CODE"
JOB_FILES=$(job_id_of "$WORK/submit.json")
expect_eq "run-files-job-succeeded" "succeeded" "$(wait_job "$JOB_FILES")"
cp "$WORK/job.json" "$PAYLOADS/job-files.json"
SNAP=$(jobget 'd["data"]["result"]["data"]["snapshot_id"]' 2>/dev/null)
if [ -z "$SNAP" ]; then SNAP=$(jget "$PAYLOADS/job-files.json" 'd["data"]["result"]["data"]["snapshot_id"]'); fi
expect_eq "run-files-snapshot-64hex" "64" "$(printf '%s' "$SNAP" | wc -c)"
expect_eq "run-files-verified-by-stats" "restic stats" \
  "$(jget "$PAYLOADS/job-files.json" 'd["data"]["result"]["observed_state"]["verified_by"]')"
expect_eq "run-files-consistency-label" "files_only" \
  "$(jget "$PAYLOADS/job-files.json" 'd["data"]["result"]["observed_state"]["consistency_mode"]')"

restic_q snapshots --json >"$PAYLOADS/restic-snapshots.json" 2>"$PAYLOADS/restic-snapshots.stderr"
INDEP_SNAP=$(jget "$PAYLOADS/restic-snapshots.json" 'json.dumps([s["id"] for s in d])')
expect_contains "snapshot-visible-to-independent-restic" "$SNAP" "$INDEP_SNAP"
# 独立恢复核对：restic 自己 restore，逐文件 sha256。这不是产品的恢复接口（P12.3）。
restic_q restore "$SNAP" --target "$RESTORE_DIR" >"$PAYLOADS/restic-restore.stdout" 2>"$PAYLOADS/restic-restore.stderr"
RESTORED=$RESTORE_DIR$ROOT_PATH/data
if [ -f "$RESTORED/a.txt" ] && [ -f "$RESTORED/blob.bin" ]; then
  pass "independent-restore-has-files"
else
  fail "independent-restore-has-files" "恢复目录里没有源文件：$(find "$RESTORE_DIR" -maxdepth 4 -type f 2>/dev/null | head -3)"
fi
HASH_MATCH=$(python3 -c 'import hashlib,sys
ok=True
for line in open(sys.argv[1], encoding="utf-8"):
    want, src = line.split()
    rel = src.split("/data/", 1)[-1]
    got = hashlib.sha256(open(sys.argv[2] + "/" + rel, "rb").read()).hexdigest()
    if got != want:
        ok = False
        print("mismatch", rel, want, got)
print("yes" if ok else "no")' "$PAYLOADS/source-hashes.txt" "$RESTORED" 2>/dev/null | tail -1)
expect_eq "independent-restore-hashes-match" "yes" "$HASH_MATCH"

echo
echo "== 6. 快照列表与详情 =="
CODE=$(api "$JAR" "$CSRF" GET "/api/v1/backup/snapshots?repository_id=$REP" "" snapshots-list)
expect_eq "snapshots-list-200" "200" "$CODE"
expect_eq "snapshots-list-registered" "True" \
  "$(jget "$PAYLOADS/snapshots-list.json" 'd["data"]["registered"]')"
expect_eq "snapshots-list-size-not-included" "False" \
  "$(jget "$PAYLOADS/snapshots-list.json" 'd["data"]["size_included"]')"
expect_contains "snapshots-list-has-our-snapshot" "$SNAP" \
  "$(jget "$PAYLOADS/snapshots-list.json" 'json.dumps([s["snapshot_id"] for s in d["data"]["items"]])')"
# 列表里不许出现 size_bytes：那条要单独跑一次 restic stats，"没读"不能写成 0。
if grep -q '"size_bytes"' "$PAYLOADS/snapshots-list.json"; then
  fail "snapshots-list-has-no-size" "列表里出现了 size_bytes"
else
  pass "snapshots-list-has-no-size"
fi
CODE=$(api "$JAR" "$CSRF" GET "/api/v1/backup/snapshots?repository_id=$REP&policy_id=$POL" "" snapshots-filtered)
expect_eq "snapshots-filtered-200" "200" "$CODE"
expect_ge "snapshots-filtered-count" \
  "$(jget "$PAYLOADS/snapshots-filtered.json" 'd["data"]["count"]')" 1
CODE=$(api "$JAR" "$CSRF" GET "/api/v1/backup/snapshots/$SNAP?repository_id=$REP" "" snapshot-detail)
expect_eq "snapshot-detail-200" "200" "$CODE"
expect_eq "snapshot-detail-found" "True" \
  "$(jget "$PAYLOADS/snapshot-detail.json" 'd["data"]["found"]')"
expect_ge "snapshot-detail-size-positive" \
  "$(jget "$PAYLOADS/snapshot-detail.json" 'int(d["data"]["snapshot"]["size_bytes"])')" 1
expect_eq "snapshot-detail-restore-unverified" "None" \
  "$(jget "$PAYLOADS/snapshot-detail.json" 'd["data"]["snapshot"]["restore_verified_at"]')"
# 没登记的仓库：200 + registered=false，不是 404。
CODE=$(api "$JAR" "$CSRF" GET "/api/v1/backup/snapshots?repository_id=rep_00000000000000000000000000000000" "" snapshots-unregistered)
expect_eq "snapshots-unregistered-200" "200" "$CODE"
expect_eq "snapshots-unregistered-flag" "False" \
  "$(jget "$PAYLOADS/snapshots-unregistered.json" 'd["data"]["registered"]')"
CODE=$(api "$JAR" "$CSRF" GET "/api/v1/backup/snapshots/0000000000000000000000000000000000000000000000000000000000000000?repository_id=$REP" "" snapshot-notfound)
expect_eq "snapshot-missing-200-found-false" "200" "$CODE"
expect_eq "snapshot-missing-found-false" "False" \
  "$(jget "$PAYLOADS/snapshot-notfound.json" 'd["data"]["found"]')"
CODE=$(api "$JAR" "$CSRF" GET "/api/v1/backup/snapshots/$SNAP")
expect_eq "snapshot-detail-requires-repository" "400" "$CODE"
CODE=$(api "$JAR" "$CSRF" GET "/api/v1/backup/snapshots")
expect_eq "snapshots-list-requires-repository" "400" "$CODE"
CODE=$(api "$VIEWER_JAR" "$VIEWER_CSRF" GET "/api/v1/backup/snapshots?repository_id=$REP")
expect_eq "snapshots-viewer-forbidden" "403" "$CODE"

echo
echo "== 7. 停受管理应用：真的停过、真的拉起来 =="
# 造一个会一直写文件的服务：它停没停，看心跳文件与 ActiveEnterTimestamp 就知道。
cat >"$APP_UNIT_FILE" <<UNIT
[Unit]
Description=P12.2 验收用的受管理应用
[Service]
Type=simple
ExecStart=/bin/sh -c 'while :; do date +%%s >> $ROOT_PATH/data/heartbeat.log; sleep 1; done'
Restart=no
[Install]
WantedBy=multi-user.target
UNIT
systemctl daemon-reload >/dev/null 2>&1
systemctl start "$APP_UNIT" >/dev/null 2>&1
expect_eq "test-app-started" "active" "$(systemctl is-active "$APP_UNIT" 2>/dev/null)"
ACTIVE_ENTER_BEFORE=$(systemctl show -p ActiveEnterTimestamp --value "$APP_UNIT" 2>/dev/null)
note "启动时间（备份前）：$ACTIVE_ENTER_BEFORE"

CODE=$(api "$JAR" "$CSRF" POST /api/v1/apps \
  "{\"name\":\"p12-2 心跳应用\",\"type\":\"systemd\",\"binding\":{\"unit\":\"$APP_UNIT\"}}" app-create)
expect_eq "app-create-201" "201" "$CODE"
APP_ID=$(jget "$PAYLOADS/app-create.json" 'd["data"]["id"]')
expect_contains "app-id-shape" "app_" "$APP_ID"

CODE=$(api "$JAR" "$CSRF" POST /api/v1/backup/policies \
  "{\"name\":\"p12-2 停机备份\",\"repository_id\":\"$REP\",\"sources\":[{\"kind\":\"root\",\"root_id\":\"root1\",\"relative_path\":\"data\"}],\"consistency_mode\":\"stop_managed_apps\",\"app_ids\":[\"$APP_ID\"],\"retention\":{\"keep_last\":3}}" policy-stop)
expect_eq "policy-stop-create-201" "201" "$CODE"
POL_STOP=$(jget "$PAYLOADS/policy-stop.json" 'd["data"]["id"]')

# 代理侧少给运行对象：必须阻塞（停机不全的备份不能降级通过）。
CODE=$(agent_pf stop-noapp backup.run "{\"backup_policy_id\":\"$POL_STOP\",\"version\":1,\"apps\":[]}")
expect_contains "run-stop-missing-app-blocked" "BACKUP_APP_UNAVAILABLE" "$(apblockers stop-noapp)"
# 多给一个策略里没有的应用：扩大授权，同样拒。
CODE=$(agent_pf stop-extra backup.run \
  "{\"backup_policy_id\":\"$POL_STOP\",\"version\":1,\"apps\":[{\"app_id\":\"$APP_ID\",\"kind\":\"systemd\",\"object\":\"$APP_UNIT\"},{\"app_id\":\"app_11111111111111111111111111111111\",\"kind\":\"systemd\",\"object\":\"cron.service\"}]}")
expect_eq "run-stop-extra-app-not-allowed" "False" "$(apget stop-extra 'd["data"]["allowed"]')"

# Web 侧展开：不存在的 app_id 回 422，并点名是哪一个。
CODE=$(api "$JAR" "$CSRF" POST /api/v1/backup/policies \
  "{\"name\":\"p12-2 停机缺失\",\"repository_id\":\"$REP\",\"sources\":[{\"kind\":\"root\",\"root_id\":\"root1\",\"relative_path\":\"data\"}],\"consistency_mode\":\"stop_managed_apps\",\"app_ids\":[\"app_22222222222222222222222222222222\"],\"retention\":{}}" policy-stop-ghost)
POL_STOP_GHOST=$(jget "$PAYLOADS/policy-stop-ghost.json" 'd["data"]["id"]')
CODE=$(preflight backup.run "{\"backup_policy_id\":\"$POL_STOP_GHOST\",\"version\":1}" preflight-stop-ghost)
expect_eq "run-stop-ghost-422" "422" "$CODE"
expect_contains "run-stop-ghost-names-app" "app_22222222222222222222222222222222" \
  "$(cat "$PAYLOADS/preflight-stop-ghost.json")"

stamp "s7-reauth-start"
CODE=$(api "$JAR" "$CSRF" POST /api/v1/auth/reauth "{\"password\":\"$PASSWORD\"}")
expect_eq "reauth-before-stop-run" "200" "$CODE"
stamp "s7-reauth-done"
CODE=$(preflight backup.run "{\"backup_policy_id\":\"$POL_STOP\",\"version\":1}" preflight-run-stop)
expect_eq "run-stop-preflight-200" "200" "$CODE"
stamp "s7-preflight-done"
expect_eq "run-stop-preflight-allowed" "True" "$(pget 'd["data"]["allowed"]')"
expect_contains "run-stop-apps-expanded" "$APP_UNIT" \
  "$(pget 'json.dumps(d["data"]["normalized_params"].get("apps"))')"
expect_contains "run-stop-shows-stop-impact" "停掉" \
  "$(pget 'json.dumps(d["data"].get("impacts"), ensure_ascii=False)')"
PREF_ID=$(pget 'd["data"]["preflight_id"]')
stamp "s7-preflight-id-read"
CODE=$(submit_job "$PREF_ID" "p12-2-run-stop-$(openssl rand -hex 8)")
stamp "s7-submit-done"
expect_eq "run-stop-submit-202" "202" "$CODE"
JOB_STOP=$(job_id_of "$WORK/submit.json")
expect_eq "run-stop-job-succeeded" "succeeded" "$(wait_job "$JOB_STOP")"
cp "$WORK/job.json" "$PAYLOADS/job-stop.json"
expect_contains "run-stop-observed-stop" "\"operation\": \"stop\"" \
  "$(jget "$PAYLOADS/job-stop.json" 'json.dumps(d["data"]["result"]["observed_state"].get("apps"), ensure_ascii=False)')"
expect_contains "run-stop-observed-start" "\"operation\": \"start\"" \
  "$(jget "$PAYLOADS/job-stop.json" 'json.dumps(d["data"]["result"]["observed_state"].get("apps"), ensure_ascii=False)')"
expect_eq "run-stop-app-active-after" "active" "$(systemctl is-active "$APP_UNIT" 2>/dev/null)"
ACTIVE_ENTER_AFTER=$(systemctl show -p ActiveEnterTimestamp --value "$APP_UNIT" 2>/dev/null)
note "启动时间（备份后）：$ACTIVE_ENTER_AFTER"
if [ -n "$ACTIVE_ENTER_BEFORE" ] && [ "$ACTIVE_ENTER_BEFORE" != "$ACTIVE_ENTER_AFTER" ]; then
  pass "run-stop-app-really-restarted"
else
  fail "run-stop-app-really-restarted" "ActiveEnterTimestamp 没变：它可能根本没被停过"
fi
expect_eq "run-stop-apps-restored-flag" "True" \
  "$(jget "$PAYLOADS/job-stop.json" 'd["data"]["result"]["observed_state"]["apps_restored"]')"

echo
echo "== 8. 面板 SQLite 在线备份：WAL 里已提交的行必须在副本里 =="
# 备份前保证 -wal 里有东西：Web 一直在写（审计/会话），真没东西就如实记 NOT RUN。
if [ -s "$WEB_DB-wal" ]; then
  pass "panel-wal-present-before-backup"
else
  notrun "panel-wal-present-before-backup" "备份前 -wal 为空，WAL 那一半的证明这次造不出来"
fi
WAL_SIZE=$(stat -c %s "$WEB_DB-wal" 2>/dev/null || printf 0)
note "备份前 -wal 大小：$WAL_SIZE 字节"
# 备份前最后一次写库：一条审计（下面的策略创建就会写）。先记一个时间锚点。
CODE=$(api "$JAR" "$CSRF" POST /api/v1/backup/policies \
  "{\"name\":\"p12-2 面板库\",\"repository_id\":\"$REP\",\"sources\":[{\"kind\":\"panel_database\"}],\"consistency_mode\":\"files_only\",\"retention\":{}}" policy-panel)
expect_eq "policy-panel-create-201" "201" "$CODE"
POL_PANEL=$(jget "$PAYLOADS/policy-panel.json" 'd["data"]["id"]')
CODE=$(agent_pf panel backup.run "{\"backup_policy_id\":\"$POL_PANEL\",\"version\":1}")
expect_eq "run-panel-preflight-allowed" "True" "$(apget panel 'd["data"]["allowed"]')"
PREF=$(apget panel 'd["data"]["preflight_ref"]')
expect_eq "run-panel-succeeded" "succeeded" "$(run_op panel "$PREF")"
expect_eq "run-panel-integrity-ok" "ok" \
  "$(opget panel 'd["data"]["result"]["observed_state"]["panel_database"]["integrity"]')"
expect_ge "run-panel-copy-size-positive" \
  "$(opget panel 'd["data"]["result"]["observed_state"]["panel_database"]["size_bytes"]')" 1
SNAP_PANEL=$(opget panel 'd["data"]["result"]["data"]["snapshot_id"]')
expect_eq "run-panel-snapshot-64hex" "64" "$(printf '%s' "$SNAP_PANEL" | wc -c)"
# 临时副本跑完必须清掉：里面有口令哈希与会话。
if [ -d "$PANEL_COPIES" ] && [ -n "$(find "$PANEL_COPIES" -type f 2>/dev/null | head -1)" ]; then
  fail "panel-copy-cleaned-up" "副本目录里还有文件：$(find "$PANEL_COPIES" -type f | head -2)"
else
  pass "panel-copy-cleaned-up"
fi
# 从快照里把副本取回来，用 sqlite3 独立验证（不是面板自己说它好）。
restic_q ls "$SNAP_PANEL" >"$PAYLOADS/panel-snapshot-files.txt" 2>/dev/null
PANEL_IN_SNAP=$(awk '/upanel.*\.db$|panel.*\.db$|\.db$/ {print; exit}' "$PAYLOADS/panel-snapshot-files.txt")
note "快照里的面板库副本路径：$PANEL_IN_SNAP"
if [ -n "$PANEL_IN_SNAP" ]; then
  pass "panel-copy-present-in-snapshot"
else
  fail "panel-copy-present-in-snapshot" "快照里没有 .db 文件：$(head -5 "$PAYLOADS/panel-snapshot-files.txt")"
fi
if [ -n "$PANEL_IN_SNAP" ]; then
  restic_q dump "$SNAP_PANEL" "$PANEL_IN_SNAP" >"$WORK/panel-restored.db" 2>"$PAYLOADS/panel-dump.stderr"
  PANEL_CHECK=$(python3 -c 'import sqlite3,sys
try:
    con = sqlite3.connect("file:%s?mode=ro" % sys.argv[1], uri=True)
    integrity = con.execute("PRAGMA integrity_check").fetchone()[0]
    users = con.execute("select count(*) from users").fetchone()[0]
    audits = con.execute("select count(*) from audit_events").fetchone()[0]
    print("integrity=%s users=%d audits=%d" % (integrity, users, audits))
except Exception as exc:
    print("ERR:%s" % exc)' "$WORK/panel-restored.db" 2>/dev/null)
  note "取回的副本：$PANEL_CHECK"
  expect_contains "panel-restored-integrity-ok" "integrity=ok" "$PANEL_CHECK"
  # users >= 1：init-admin 是在备份前做的；读不到用户说明副本是旧的/空的。
  USERS_N=$(printf '%s' "$PANEL_CHECK" | sed -n 's/.*users=\([0-9]*\).*/\1/p')
  expect_ge "panel-restored-has-admin" "${USERS_N:-0}" 1
  AUDITS_N=$(printf '%s' "$PANEL_CHECK" | sed -n 's/.*audits=\([0-9]*\).*/\1/p')
  expect_ge "panel-restored-has-audit" "${AUDITS_N:-0}" 1
fi

echo
echo "== 9. SFTP 实跑、中途断线、存储满与取消 =="
# SFTP 目标账号与 P12.1 相同：不能用 nologin 的 upanel-web。私钥只进秘密存储，
# 独立证据里不留私钥；远端目录是本轮专用路径，不碰已有仓库。
if id -u "$SFTP_USER" >/dev/null 2>&1; then
  pass "sftp-target-account-present"
else
  if useradd --create-home --shell /bin/bash "$SFTP_USER" >>"$WORK/logs/setup.log" 2>&1; then
    pass "sftp-target-account-created"
  else
    fail "sftp-target-account-created" "$(tail -3 "$WORK/logs/setup.log" 2>/dev/null)"
    exit 2
  fi
fi
rm -rf "$SSH_DIR" "$SFTP_REMOTE"
mkdir -p "$SSH_DIR"
ssh-keygen -q -t ed25519 -N '' -C 'p12-2-backup-run' -f "$SSH_DIR/id_ed25519" >/dev/null 2>&1
install -d -m 0700 -o "$SFTP_USER" -g "$SFTP_USER" "$SFTP_HOME/.ssh"
touch "$SFTP_HOME/.ssh/authorized_keys"
chown "$SFTP_USER:$SFTP_USER" "$SFTP_HOME/.ssh/authorized_keys"
chmod 0600 "$SFTP_HOME/.ssh/authorized_keys"
if ! grep -qF "$(cat "$SSH_DIR/id_ed25519.pub")" "$SFTP_HOME/.ssh/authorized_keys" 2>/dev/null; then
  cat "$SSH_DIR/id_ed25519.pub" >>"$SFTP_HOME/.ssh/authorized_keys"
fi
HOSTKEY_FP=$(ssh-keyscan -p 22 -t ed25519 127.0.0.1 2>/dev/null | ssh-keygen -lf - 2>/dev/null | awk '{print $2}')
expect_contains "sftp-host-key-scanned" "SHA256:" "$HOSTKEY_FP"
expect_eq "secret-sftp-private-key" "201" \
  "$(put_secret_file sftpkey sftp_private_key "$SSH_DIR/id_ed25519")"
SEC_SFTP_KEY=$(secret_of sftpkey)
CODE=$(api "$JAR" "$CSRF" POST /api/v1/auth/reauth "{\"password\":\"$PASSWORD\"}")
expect_eq "sftp-repo-reauth-200" "200" "$CODE"
CODE=$(api "$JAR" "$CSRF" POST /api/v1/backup/repositories \
  "{\"name\":\"p12-2 SFTP 运行\",\"backend\":\"sftp\",\"location\":{\"host\":\"127.0.0.1\",\"port\":22,\"username\":\"$SFTP_USER\",\"remote_path\":\"$SFTP_REMOTE\",\"host_key_fingerprint\":\"$HOSTKEY_FP\"},\"password_secret_ref\":\"$SEC_PASS\",\"credential_secret_ref\":\"$SEC_SFTP_KEY\"}" repo-sftp)
expect_eq "sftp-repo-create-201" "201" "$CODE"
REP_SFTP=$(jget "$PAYLOADS/repo-sftp.json" 'd["data"]["id"]')
CODE=$(agent_pf sftp-init backup.repository.initialize "{\"repository_id\":\"$REP_SFTP\",\"version\":1}")
expect_eq "sftp-init-preflight-allowed" "True" "$(apget sftp-init 'd["data"]["allowed"]')"
PREF=$(apget sftp-init 'd["data"]["preflight_ref"]')
expect_eq "sftp-init-succeeded" "succeeded" "$(run_op sftp-init "$PREF")"

CODE=$(api "$JAR" "$CSRF" POST /api/v1/backup/policies \
  "{\"name\":\"p12-2 SFTP 策略\",\"repository_id\":\"$REP_SFTP\",\"sources\":[{\"kind\":\"root\",\"root_id\":\"root1\",\"relative_path\":\"data\"}],\"consistency_mode\":\"files_only\",\"retention\":{}}" policy-sftp)
expect_eq "sftp-policy-create-201" "201" "$CODE"
POL_SFTP=$(jget "$PAYLOADS/policy-sftp.json" 'd["data"]["id"]')
begin_backup_job "$POL_SFTP" 1 run-sftp
expect_eq "run-sftp-reauth-200" "200" "$JOB_REAUTH_CODE"
expect_eq "run-sftp-preflight-allowed" "True" "$JOB_ALLOWED"
expect_eq "run-sftp-submit-202" "202" "$JOB_SUBMIT_CODE"
JOB_SFTP=$JOB_ID
expect_eq "run-sftp-job-succeeded" "succeeded" "$(wait_job "$JOB_SFTP")"
cp "$WORK/job.json" "$PAYLOADS/job-sftp.json"
SNAP_SFTP=$(jget "$PAYLOADS/job-sftp.json" 'd["data"]["result"]["data"]["snapshot_id"]')
expect_eq "run-sftp-snapshot-64hex" "64" "$(printf '%s' "$SNAP_SFTP" | wc -c)"
expect_eq "run-sftp-verified-by-stats" "restic stats" \
  "$(jget "$PAYLOADS/job-sftp.json" 'd["data"]["result"]["observed_state"]["verified_by"]')"
CODE=$(api "$JAR" "$CSRF" GET "/api/v1/backup/snapshots?repository_id=$REP_SFTP" "" snapshots-sftp)
expect_eq "run-sftp-snapshot-listed" "True" \
  "$(jget "$PAYLOADS/snapshots-sftp.json" 'any(s["snapshot_id"]=="'"$SNAP_SFTP"'" for s in d["data"]["items"])')"
if [ -n "$(find "$SFTP_REMOTE" -type f 2>/dev/null | head -1)" ]; then
  pass "run-sftp-remote-files-present"
else
  fail "run-sftp-remote-files-present" "$SFTP_REMOTE 下没有仓库文件"
fi

# 中途断网：只杀本次 restic 为 SFTP 拉起的 ssh 客户端，不断宿主到 VM 的管理连接，
# 也不改防火墙。256 MiB 随机数据避免去重把这一轮缩成瞬时完成。
mkdir -p "$ROOT_PATH/data-network"
dd if=/dev/urandom of="$ROOT_PATH/data-network/random.bin" bs=1M count=256 status=none
NETWORK_SHA_BEFORE=$(file_sha "$ROOT_PATH/data-network/random.bin")
CODE=$(api "$JAR" "$CSRF" POST /api/v1/backup/policies \
  "{\"name\":\"p12-2 断线策略\",\"repository_id\":\"$REP_SFTP\",\"sources\":[{\"kind\":\"root\",\"root_id\":\"root1\",\"relative_path\":\"data-network\"}],\"consistency_mode\":\"files_only\",\"retention\":{}}" policy-network-cut)
expect_eq "network-policy-create-201" "201" "$CODE"
POL_NETWORK=$(jget "$PAYLOADS/policy-network-cut.json" 'd["data"]["id"]')
begin_backup_job "$POL_NETWORK" 1 network-cut
expect_eq "network-cut-preflight-allowed" "True" "$JOB_ALLOWED"
expect_eq "network-cut-submit-202" "202" "$JOB_SUBMIT_CODE"
JOB_NETWORK=$JOB_ID
SSH_PID=""
_i=0
while [ "$_i" -lt 200 ] && [ -z "$SSH_PID" ]; do
  for _pid in $(pgrep -x ssh 2>/dev/null); do
    _args=$(tr '\000' ' ' <"/proc/$_pid/cmdline" 2>/dev/null)
    if printf '%s' "$_args" | grep -q 'p12sftp' && printf '%s' "$_args" | grep -q 'sftp'; then
      SSH_PID=$_pid
      break
    fi
  done
  [ -n "$SSH_PID" ] && break
  _i=$((_i + 1))
  sleep 0.1
done
if [ -n "$SSH_PID" ]; then
  printf 'restic SFTP ssh pid=%s\n' "$SSH_PID" >"$PAYLOADS/network-cut.txt"
  kill -TERM "$SSH_PID" 2>/dev/null
  pass "network-cut-restic-ssh-killed"
else
  fail "network-cut-restic-ssh-killed" "任务结束之前没有找到本次 restic 的 SFTP ssh 进程"
fi
expect_eq "network-cut-job-failed" "failed" "$(wait_job "$JOB_NETWORK")"
cp "$WORK/job.json" "$PAYLOADS/job-network-cut.json"
expect_eq "network-cut-error-code" "COMMAND_FAILED" \
  "$(jget "$PAYLOADS/job-network-cut.json" 'd["data"]["error"]["code"]')"
expect_eq "network-cut-source-unchanged" "$NETWORK_SHA_BEFORE" \
  "$(file_sha "$ROOT_PATH/data-network/random.bin")"
CODE=$(api "$JAR" "$CSRF" GET "/api/v1/backup/snapshots?repository_id=$REP_SFTP" "" snapshots-sftp-after-cut)
expect_eq "network-cut-old-snapshot-still-readable" "True" \
  "$(jget "$PAYLOADS/snapshots-sftp-after-cut.json" 'any(s["snapshot_id"]=="'"$SNAP_SFTP"'" for s in d["data"]["items"])')"

# 存储满：独立 64 MiB tmpfs，不填系统盘。先放一份小快照，再把可用空间压到约 1 MiB；
# 预检仍能读仓库，提交后继续写到 ENOSPC。事后原小快照必须还能 dump 出来。
install -d -m 0755 "$FULL_REPO" "$FULL_SOURCE"
printf 'p12-2 baseline survives ENOSPC\n' >"$FULL_SOURCE/baseline.txt"
if mount -t tmpfs -o size=64m tmpfs "$FULL_REPO"; then
  FULL_MOUNTED=1
  pass "disk-full-isolated-tmpfs-mounted"
else
  fail "disk-full-isolated-tmpfs-mounted" "独立 tmpfs 挂载失败；停止，绝不改写系统盘"
  exit 2
fi
if RESTIC_PASSWORD="$REPO_PASSWORD" restic -r "$FULL_REPO" --no-cache init \
     >"$PAYLOADS/disk-full-init.log" 2>&1; then
  pass "disk-full-repo-initialized"
else
  fail "disk-full-repo-initialized" "$(tail -3 "$PAYLOADS/disk-full-init.log")"
fi
if RESTIC_PASSWORD="$REPO_PASSWORD" restic -r "$FULL_REPO" --no-cache backup "$FULL_SOURCE" --json \
     >"$PAYLOADS/disk-full-baseline.json" 2>"$PAYLOADS/disk-full-baseline.stderr"; then
  pass "disk-full-baseline-snapshot-created"
else
  fail "disk-full-baseline-snapshot-created" "$(tail -3 "$PAYLOADS/disk-full-baseline.stderr")"
fi
dd if=/dev/urandom of="$FULL_SOURCE/random.bin" bs=1M count=64 status=none
RESTIC_PASSWORD="$REPO_PASSWORD" restic -r "$FULL_REPO" --no-cache snapshots --json \
  >"$PAYLOADS/disk-full-baseline-snapshots.json" 2>"$PAYLOADS/disk-full-baseline-snapshots.stderr"
FULL_BASE_SNAP=$(jget "$PAYLOADS/disk-full-baseline-snapshots.json" 'd[-1]["id"]')
FULL_SHA_BEFORE=$(file_sha "$FULL_SOURCE/random.bin")
AVAIL_KB=$(df -Pk "$FULL_REPO" | awk 'NR==2 {print $4}')
FILL_COUNT=$((AVAIL_KB / 1024 - 1))
if [ "$FILL_COUNT" -gt 0 ]; then
  dd if=/dev/zero of="$FULL_REPO/filler" bs=1M count="$FILL_COUNT" status=none
fi
df -Pk "$FULL_REPO" >"$PAYLOADS/disk-full-before-run-df.txt"
CODE=$(api "$JAR" "$CSRF" POST /api/v1/auth/reauth "{\"password\":\"$PASSWORD\"}")
expect_eq "disk-full-repo-reauth-200" "200" "$CODE"
CODE=$(api "$JAR" "$CSRF" POST /api/v1/backup/repositories \
  "{\"name\":\"p12-2 写满仓库\",\"backend\":\"local\",\"location\":{\"root_id\":\"root1\",\"relative_path\":\"full-repo\"},\"password_secret_ref\":\"$SEC_PASS\"}" repo-disk-full)
expect_eq "disk-full-repo-create-201" "201" "$CODE"
REP_FULL=$(jget "$PAYLOADS/repo-disk-full.json" 'd["data"]["id"]')
CODE=$(api "$JAR" "$CSRF" POST /api/v1/backup/policies \
  "{\"name\":\"p12-2 写满策略\",\"repository_id\":\"$REP_FULL\",\"sources\":[{\"kind\":\"root\",\"root_id\":\"root1\",\"relative_path\":\"data-full\"}],\"consistency_mode\":\"files_only\",\"retention\":{}}" policy-disk-full)
expect_eq "disk-full-policy-create-201" "201" "$CODE"
POL_FULL=$(jget "$PAYLOADS/policy-disk-full.json" 'd["data"]["id"]')
begin_backup_job "$POL_FULL" 1 disk-full
expect_eq "disk-full-preflight-allowed" "True" "$JOB_ALLOWED"
expect_eq "disk-full-submit-202" "202" "$JOB_SUBMIT_CODE"
JOB_FULL=$JOB_ID
dd if=/dev/zero of="$FULL_REPO/filler" bs=1M count=16 oflag=append conv=notrunc status=none \
  2>"$PAYLOADS/disk-full-filler.stderr"
FILL_RC=$?
if [ "$FILL_RC" -ne 0 ]; then
  pass "disk-full-filler-hit-enospc"
else
  fail "disk-full-filler-hit-enospc" "填充命令没有遇到 ENOSPC"
fi
FULL_LEFT=$(df -Pk "$FULL_REPO" | awk 'NR==2 {print $4}')
if [ "$FULL_LEFT" -lt 1024 ]; then
  pass "disk-full-less-than-1mib-free"
else
  fail "disk-full-less-than-1mib-free" "仍有 ${FULL_LEFT} KiB 可用"
fi
expect_eq "disk-full-job-failed" "failed" "$(wait_job "$JOB_FULL")"
cp "$WORK/job.json" "$PAYLOADS/job-disk-full.json"
expect_eq "disk-full-error-code" "COMMAND_FAILED" \
  "$(jget "$PAYLOADS/job-disk-full.json" 'd["data"]["error"]["code"]')"
expect_eq "disk-full-source-unchanged" "$FULL_SHA_BEFORE" "$(file_sha "$FULL_SOURCE/random.bin")"
# 任务已终止且没有并发写入；--no-lock 让独立只读核对本身不再向已满仓库写锁文件。
# 两个冻结验收版本 restic 0.12.1 / 0.16.4 的 dump 全局参数均已实地核对支持该选项。
if RESTIC_PASSWORD="$REPO_PASSWORD" restic -r "$FULL_REPO" --no-cache --no-lock dump "$FULL_BASE_SNAP" "$FULL_SOURCE/baseline.txt" \
     >"$WORK/full-baseline-restored.txt" 2>"$PAYLOADS/disk-full-restore.stderr" && \
   cmp -s "$FULL_SOURCE/baseline.txt" "$WORK/full-baseline-restored.txt"; then
  pass "disk-full-old-snapshot-still-readable"
else
  fail "disk-full-old-snapshot-still-readable" "$(tail -3 "$PAYLOADS/disk-full-restore.stderr")"
fi
if [ "$FULL_MOUNTED" -eq 1 ] && umount "$FULL_REPO"; then
  FULL_MOUNTED=0
  pass "disk-full-tmpfs-unmounted"
else
  fail "disk-full-tmpfs-unmounted" "tmpfs 卸载失败"
fi

# 取消：同样用 256 MiB 新随机数据，提交后先确认任务把它标成可取消，再发取消。
# 终态必须是 canceled；源不变，主仓库里更早的快照仍由 restic 独立读到。
mkdir -p "$ROOT_PATH/data-cancel"
dd if=/dev/urandom of="$ROOT_PATH/data-cancel/random.bin" bs=1M count=256 status=none
CANCEL_SHA_BEFORE=$(file_sha "$ROOT_PATH/data-cancel/random.bin")
CODE=$(api "$JAR" "$CSRF" POST /api/v1/backup/policies \
  "{\"name\":\"p12-2 取消策略\",\"repository_id\":\"$REP\",\"sources\":[{\"kind\":\"root\",\"root_id\":\"root1\",\"relative_path\":\"data-cancel\"}],\"consistency_mode\":\"files_only\",\"retention\":{}}" policy-cancel)
expect_eq "cancel-policy-create-201" "201" "$CODE"
POL_CANCEL=$(jget "$PAYLOADS/policy-cancel.json" 'd["data"]["id"]')
begin_backup_job "$POL_CANCEL" 1 cancel
expect_eq "cancel-preflight-allowed" "True" "$JOB_ALLOWED"
expect_eq "cancel-submit-202" "202" "$JOB_SUBMIT_CODE"
JOB_CANCEL=$JOB_ID
CODE=$(api "$JAR" "$CSRF" GET "/api/v1/jobs/$JOB_CANCEL" "" job-cancel-before)
expect_eq "cancel-job-cancellable" "True" \
  "$(jget "$PAYLOADS/job-cancel-before.json" 'd["data"]["cancellable"]')"
CODE=$(api "$JAR" "$CSRF" POST "/api/v1/jobs/$JOB_CANCEL/cancel" '{}' job-cancel-request)
expect_eq "cancel-request-202" "202" "$CODE"
expect_eq "cancel-request-recorded" "True" \
  "$(jget "$PAYLOADS/job-cancel-request.json" 'd["data"]["cancellation_requested"]')"
expect_eq "cancel-job-terminal-canceled" "canceled" "$(wait_job "$JOB_CANCEL")"
cp "$WORK/job.json" "$PAYLOADS/job-cancel.json"
expect_eq "cancel-source-unchanged" "$CANCEL_SHA_BEFORE" "$(file_sha "$ROOT_PATH/data-cancel/random.bin")"
restic_q snapshots --json >"$PAYLOADS/restic-snapshots-after-cancel.json" \
  2>"$PAYLOADS/restic-snapshots-after-cancel.stderr"
expect_eq "cancel-old-snapshot-still-readable" "True" \
  "$(jget "$PAYLOADS/restic-snapshots-after-cancel.json" 'any(s["id"]=="'"$SNAP"'" for s in d)')"

echo
echo "== 10. 仓库没初始化时不能备份；代理停掉整组 503 =="
expect_eq "secret-second-password" "201" "$(put_secret secondpass backup_repository_password "$REPO_PASSWORD")"
SEC_SECOND=$(secret_of secondpass)
CODE=$(api "$JAR" "$CSRF" POST /api/v1/auth/reauth "{\"password\":\"$PASSWORD\"}")
expect_eq "repo-uninit-reauth-200" "200" "$CODE"
CODE=$(api "$JAR" "$CSRF" POST /api/v1/backup/repositories \
  "{\"name\":\"p12-2 未初始化\",\"backend\":\"local\",\"location\":{\"root_id\":\"root1\",\"relative_path\":\"backups/not-initialized\"},\"password_secret_ref\":\"$SEC_SECOND\"}" repo-uninit)
expect_eq "repo-uninit-create-201" "201" "$CODE"
REP_UNINIT=$(jget "$PAYLOADS/repo-uninit.json" 'd["data"]["id"]')
CODE=$(api "$JAR" "$CSRF" POST /api/v1/backup/policies \
  "{\"name\":\"p12-2 未初始化策略\",\"repository_id\":\"$REP_UNINIT\",\"sources\":[{\"kind\":\"root\",\"root_id\":\"root1\",\"relative_path\":\"data\"}],\"consistency_mode\":\"files_only\",\"retention\":{}}" policy-uninit)
POL_UNINIT=$(jget "$PAYLOADS/policy-uninit.json" 'd["data"]["id"]')
CODE=$(agent_pf uninit backup.run "{\"backup_policy_id\":\"$POL_UNINIT\",\"version\":1}")
expect_contains "run-uninitialized-repo-blocked" "BACKUP_REPOSITORY" "$(apblockers uninit)"

# 代理停掉：整组 503（面板没有本地副本），不是空列表、不是"先登记"。
kill -TERM "$AGENT_PID" 2>/dev/null
wait "$AGENT_PID" 2>/dev/null
AGENT_PID=""
CODE=$(api "$JAR" "$CSRF" GET /api/v1/backup/repositories "" repos-agent-down)
expect_eq "repos-agent-down-503" "503" "$CODE"
expect_eq "repos-agent-down-code" "AGENT_UNAVAILABLE" \
  "$(jget "$PAYLOADS/repos-agent-down.json" 'd["error"]["code"]')"
CODE=$(api "$JAR" "$CSRF" GET /api/v1/backup/policies)
expect_eq "policies-agent-down-503" "503" "$CODE"
CODE=$(api "$JAR" "$CSRF" GET "/api/v1/backup/snapshots?repository_id=$REP")
expect_eq "snapshots-agent-down-503" "503" "$CODE"

echo
echo "== 11. NOT RUN 与证据卫生 =="
notrun "product-restore-api" "产品恢复接口是 P12.3；本脚本的 restore 是 restic 独立核对"
notrun "retention-prune" "retention 本阶段只登记不执行，清理是 P12.3 的 backup.prune"
notrun "browser-ui" "VM 上没有浏览器；备份 UI 属 P12.5"

# 秘密不进日志：代理与 Web 的日志里都不能出现仓库口令与面板口令。
for secret in "$REPO_PASSWORD" "$PASSWORD"; do
  if grep -qF "$secret" "$AGENT_LOG" "$WEB_LOG" 2>/dev/null; then
    fail "no-secret-in-logs" "日志里出现了秘密明文"
  else
    pass "no-secret-in-logs"
  fi
done
# payloads 里凡是可能带秘密明文的请求体，就地替换成说明。
for f in "$PAYLOADS"/*.json; do
  if [ -f "$f" ] && grep -qF "$REPO_PASSWORD" "$f" 2>/dev/null; then
    printf '{"redacted":"原文件含仓库口令明文，已就地替换；产品侧秘密只经 stdin/环境变量，不进 argv 与日志"}\n' >"$f"
  fi
  if [ -f "$f" ] && grep -qF "$PASSWORD" "$f" 2>/dev/null; then
    printf '{"redacted":"原文件含面板口令明文，已就地替换"}\n' >"$f"
  fi
done
# init.out / login 响应里也不留口令（login 响应只有 token，但这里统一扫一遍）。
for f in "$WORK"/login-*.json "$WORK"/resp.json "$WORK"/req.json; do
  if [ -f "$f" ] && grep -qF "$PASSWORD" "$f" 2>/dev/null; then
    printf '{"redacted":"含口令，已替换"}\n' >"$f"
  fi
done
rm -f "$WORK/admin.cookies" "$WORK/viewer.cookies"

echo
echo "=================================================="
printf 'PASS=%d FAIL=%d NOT RUN=%d\n' "$PASSED" "$FAILED" "$NOTRUN"
if [ "$FAILED" -gt 0 ]; then
  printf '失败项：%s\n' "$FAILED_NAMES"
fi
printf '证据目录：%s\n' "$WORK"
[ "$FAILED" -eq 0 ]
