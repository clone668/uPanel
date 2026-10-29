#!/bin/sh
# P14.4 Linux 验收脚本：NFS 受管理导出、网段/root_squash、卷依赖、真实客户端读写。
#
# 运行位置：Ubuntu 22.04 验收 VM 内，**必须以 root 运行**。前置夹具是
# nfs-kernel-server + nfs-common（exportfs / showmount / mount.nfs），缺了在第 0 节停下，
# 不把缺包记成产品缺陷。面板不会替任何人装包（PLAN.md 第 10 节）。
#
# 用法（**必须后台起**，理由与 P14.2/P14.3 相同：NFS 挂载在内核里可能挂住很久，
# ssh 管道抖一次就 SIGPIPE 掉整轮验收）：
#   sudo SPARE_DISKS=sdb BIN_DIR=/var/tmp/upanel-bin setsid nohup \
#        sh tests/integration/p14-4-acceptance.sh >/var/tmp/p144-run.log 2>&1 &
#   然后轮询 /var/tmp/upanel-accept-p14-4/RESULT
#
# 退出码：0 全部通过；1 有失败项；2 环境不满足。NOT RUN 是**按设计**列出的范围外项，
# 单独计数、不影响退出码——与 P14.1/P14.2/P14.3 一致。
#
# ---------------------------------------------------------------- 这一轮验什么
#
#   A. **只碰自己那份文件**：面板只写 /etc/exports.d/upanel.exports，整份重写，每行行尾
#      带 `# upanel:<share_id>` 认领标记；/etc/exports 一个字都不改（只有它被手工改过
#      才会出现"读到别人导出"的冲突）。
#   B. **判决靠读回，不靠退出码**：`exportfs -ra` 返回 0 不代表表变了。应用之后用
#      `exportfs -v` 读**实际导出表**逐条比对；比对上才算 verified，`served` 才为真。
#   C. **真实客户端读写**：本机当客户端，`mount -t nfs -o vers=3,proto=tcp` 挂上，
#      **先 findmnt 再动手**（挂载失败时客户端会静默写本地目录、rc 还是 0——这是这一轮
#      最容易骗过自己的地方）。写进去的文件从服务端路径读回，哈希必须一致。
#   D. **root_squash 是真的**：root 通过 NFS 写进去的文件落盘属主是 nobody:nogroup。
#   E. **网段就是访问控制**：把允许网段改成不在名单里的一个，本机客户端立刻连不上；
#      改回来又连得上。没有账号这一层可退。
#   F. **只读共享两道锁**：导出行写 `ro`、目录权限 0755；客户端写被内核拒。
#   G. **卷依赖**：卷在上面时有启用导出 → 面板拒卸载（VOLUME_IN_USE_BY_SHARE）；
#      从外面卸掉 → 撤销循环把导出行摘掉、实际导出表里也没有了、客户端挂不上；
#      卷挂回来不会自己复活，重新应用才恢复。
#   H. **撤干净**：停用/删除之后托管文件里一条 upanel 行都不剩、`exportfs -v` 里没有
#      卷里的路径，而卷里的目录与文件一个不少。
#
# ---------------------------------------------------------------- 明确不验（NOT RUN）
#
#   * 从**另一台机器**挂进来：VM 里只有本机，跨机器还牵涉网卡与防火墙。网段限制这一条
#     用"改名单"的方式在本机验（名单外连不上），但没验过两台机器之间的真实链路。
#   * **Windows / macOS 的 NFS 客户端**：没有这两种测试机。说明里给它们的地址形态由
#     web 侧断言（`web/src/api/shares.test.ts`）。
#   * **NFSv4**：面板不假设 v4（`fsid=0` 的假根在别的地方会挡住直挂，见 DECISIONS）。
#     脚本只跑 `vers=3,proto=tcp` 这条；v4 的尝试结果写进 facts，不算通过项。
#   * **异步/并发写导出文件**：两条导出同时应用时的锁行为由单测
#     `internal/shares/exports_test.go` 覆盖；真机上同时发两个任务会把冲突变成时序问题。
#   * **kerberos（sec=krb5*）**：这一版固定 sec=sys，没有这一层。

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

SPARE_DISKS=${SPARE_DISKS:-}
WORK=${WORK:-/var/tmp/upanel-accept-p14-4}
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
PORT=${PORT:-19479}
WEB_ADDR=127.0.0.1
BASE="http://$WEB_ADDR:$PORT"
ORIGIN="$BASE"
WEB_USER=upanel-web
PASSWORD='vm-p144-admin-91'
JAR=""
CSRF=""
RESULT=$WORK/RESULT

AGENT_LOG=$WORK/logs/agent.log
WEB_LOG=$WORK/logs/web.log
AGENT_PID=""
WEB_PID=""

STORE=/api/v1/storage
SHARES_PATH=/api/v1/shares
PREFLIGHT_PATH=/api/v1/operations/preflight
JOBS=/api/v1/jobs

VOLUME_ROOT=/srv/upanel/volumes
FSTAB=/etc/fstab
SLUG=data
LABEL=p14vol
PART_MAIN=$((2 * 1073741824))
PART_START=1048576

# NFS 的固定路径：与 internal/shares/nfs.go、startupdeps.go 里的常量逐字一致。
EXPORTS_DIR=/etc/exports.d
EXPORTS_FILE=$EXPORTS_DIR/upanel.exports
MAIN_EXPORTS=/etc/exports
EXPORTS_STARTUP_DROPIN=/etc/systemd/system/nfs-server.service.d/upanel-exports.conf

# 名字与相对路径：rw 共享挂在卷根，ro 共享在 ro/ 子目录。
SHARE_RW=media
SHARE_RO=media-ro
REL_RO=ro
# 本机客户端：NFS 没有账号，名单里必须有 127.0.0.0/8 才挂得进来。
CIDR_LOCAL=127.0.0.0/8
# 名单外的一个网段：用来证明"网段是唯一的访问控制"。
CIDR_FOREIGN=192.168.77.0/24

MOUNT_RW=/mnt/nfs-$SHARE_RW
MOUNT_RO=/mnt/nfs-$SHARE_RO

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
# 共享标识必须是 shr_ + 32 位小写十六进制。
#
# 只判"非空"是不够的：jget 取不到时会打印 `ERR:KeyError:'data'` 这种字符串，非空，
# 于是后面每一条断言都以这个垃圾值为输入全部变红——第一轮验收就是这样，一个失败被
# 放大成五十多条噪音，真正的现场（落库被 CHECK 挡住）反而埋掉了。
expect_share_id() {
  if printf '%s' "$2" | grep -Eq '^shr_[0-9a-f]{32}$'; then
    pass "$1"
  else
    fail "$1" "不是 shr_ + 32 位十六进制：'$2'"
  fi
}
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
pget() { jget "$WORK/preflight.json" "$1"; }
jobget() { jget "$WORK/job.json" "$1"; }
pall() { pget '",".join(b["code"] for b in d["data"]["blockers"])'; }
pwarn() { pget '",".join(w["code"] for w in (d["data"].get("warnings") or []))'; }
joutcome() { jobget 'd["data"]["result"]["outcome"]'; }
jobobserved() { jobget "d[\"data\"][\"result\"][\"observed_state\"].get(\"$1\")"; }
jobdata() { jobget "d[\"data\"][\"result\"][\"data\"].get(\"$1\")"; }

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
  chown "$WEB_USER" "$WEB_DB" 2>/dev/null
  for suffix in "" "-wal" "-shm"; do
    [ -e "$WEB_DB$suffix" ] && chown "$WEB_USER" "$WEB_DB$suffix"
  done
  return 0
}

# ---- 清理（**必须自己把客户端挂载卸掉**：它挂着的时候 restorecon/umount 卷都会失败，
#      而且一个挂住的 NFS 会让整个脚本卡在收尾） ----
#
# 为什么不能用 `umount -l`：本机既是 NFS 服务端又是客户端，lazy umount 只是把这个挂载
# 从命名空间里摘掉，内核仍然持有那个文件系统，于是**服务端的 umount 会报 target is busy**。
# 22.04 实测：`umount -l` 客户端之后 `umount /mnt/exp2` 返回 32（target is busy），
# 再试一次还是 32。结果是“从外面把卷卸掉”这一步根本做不到，撤销那一整节全红。
# 顺序因此是硬的（本机环路，不存在网络卡死的风险）：先硬卸，再 `-f`，最后才 `-l`。
unmount_clients() {
  local mp try
  for mp in "$MOUNT_RW" "$MOUNT_RO"; do
    [ -d "$mp" ] || continue
    for try in "umount" "umount -f" "umount -l"; do
      mountpoint -q "$mp" 2>/dev/null || break
      $try "$mp" 2>/dev/null || true
    done
  done
  return 0
}

restore_exports() {
  if [ -f "${MAIN_EXPORTS_ARCHIVE:-}" ]; then
    cp "$MAIN_EXPORTS_ARCHIVE" "$MAIN_EXPORTS" 2>/dev/null || true
  fi
  if [ -d "${EXPORTS_DIR_ARCHIVE:-}" ]; then
    rm -f "$EXPORTS_DIR"/*.exports 2>/dev/null || true
    cp "$EXPORTS_DIR_ARCHIVE"/*.exports "$EXPORTS_DIR"/ 2>/dev/null || true
  fi
  exportfs -ra >/dev/null 2>&1 || true
  return 0
}

cleanup() {
  unmount_clients
  local _mp
  for _mp in "$VOLUME_ROOT"/*; do
    [ -d "$_mp" ] || continue
    umount "$_mp" 2>/dev/null || true
  done
  if [ -n "${WEB_PID:-}" ] && kill -0 "$WEB_PID" 2>/dev/null; then
    kill -TERM "$WEB_PID" 2>/dev/null; wait "$WEB_PID" 2>/dev/null
  fi
  if [ -n "${AGENT_PID:-}" ] && kill -0 "$AGENT_PID" 2>/dev/null; then
    kill -TERM "$AGENT_PID" 2>/dev/null; wait "$AGENT_PID" 2>/dev/null
  fi
  rm -f "$SOCK"
  restore_exports
  return 0
}
trap cleanup EXIT
trap 'write_result; cleanup; exit 130' INT
trap 'write_result; cleanup; exit 143' TERM

# ---- 独立证据 ----

blkid_field() { blkid -p -o value -s "$2" "$1" 2>/dev/null | head -1; }
is_mounted() {
  if [ -n "$(findmnt -rno TARGET --mountpoint "$1" 2>/dev/null)" ]; then echo True; else echo False; fi
}
mount_options() { findmnt -rno OPTIONS --mountpoint "$1" 2>/dev/null; }
dir_mode() { stat -c '%a' "$1" 2>/dev/null; }
# `exportfs -v` 是**实际生效的导出表**，不是任何人的意图。断言一律读它。
#
# 它的形状是这样的（22.04 实测，`cat -A` 看过）：
#
#   /srv/upanel/volumes/data$
#   ^I^I127.0.0.0/8(sync,wdelay,hide,no_subtree_check,sec=sys,rw,secure,root_squash,...)$
#
# **路径单独占一行，后面没有空格**。所以第一轮写的 `grep -F "$MP "`（带尾空格）永远匹配不上，
# 三条"独立证据（核心）"全是假红——而同一份数据在面板自己的读回里是通过的（ParseExportfsV
# 按行解析）。这里改成按行精确匹配路径，再把属于它的客户端行取出来看选项。
exports_table() { exportfs -v 2>/dev/null; }
export_clients_of() { # $1 路径 → 该路径下的客户端选项行（空输出 = 没被导出）
  exports_table | awk -v p="$1" '/^\t/ { if (on) print; next } { on = ($0 == p) }'
}
exports_table_has() {
  if [ -n "$(export_clients_of "$1")" ]; then echo True; else echo False; fi
}
exports_file_text() { cat "$EXPORTS_FILE" 2>/dev/null; }
exports_file_has() {
  if exports_file_text | grep -qF "$1"; then echo True; else echo False; fi
}
# 只数**真正的导出**，不数文件头。
#
# 托管文件开头有三行说明，其中一行原文就含 `` `# upanel:<共享 id>` ``（讲认领标记怎么
# 用）。直接 `grep -c '# upanel:'` 会把它算成一条共享：VM 第一轮就是这么把“两条导出”
# 数成 3、把“一条都不剩”数成 1 的。
managed_line_count() {
  exports_file_text | grep -v '^#' | grep -c '# upanel:' 2>/dev/null | tr -d ' '
}
main_exports_sha() { sha256sum "$MAIN_EXPORTS" 2>/dev/null | cut -d' ' -f1; }

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

login() {
  curl -s -c "$3" -o "$WORK/$4.json" -w '%{http_code}' -X POST "$BASE/api/v1/auth/login" \
    -H 'Content-Type: application/json' -H 'X-UPanel-Request: 1' -H "Origin: $ORIGIN" \
    -d "{\"username\":\"$1\",\"password\":\"$2\"}"
}
csrf_of() { jget "$1" 'd["data"]["csrf_token"]'; }
reauth() { api "$JAR" "$CSRF" POST /api/v1/auth/reauth "{\"password\":\"$PASSWORD\"}"; }

preflight() {
  printf '{"action":"%s","params":%s}' "$1" "$2" >"$TMPD/pre.json"
  _code=$(curl -s -b "$JAR" -o "$TMPD/resp.json" -w '%{http_code}' -X POST "$BASE$PREFLIGHT_PATH" \
    -H 'Content-Type: application/json' -H "Origin: $ORIGIN" -H "X-CSRF-Token: $CSRF" \
    -d @"$TMPD/pre.json")
  cp "$TMPD/resp.json" "$WORK/preflight.json"
  printf '%s' "$_code"
}

submit_job() {
  python3 -c 'import json,sys
d={"preflight_id":sys.argv[1]}
if sys.argv[2]:
    d["confirmation_phrase"]=sys.argv[2]
print(json.dumps(d))' "$1" "${2:-}" >"$TMPD/jobreq.json"
  _code=$(curl -s -b "$JAR" -o "$TMPD/resp.json" -w '%{http_code}' -X POST "$BASE$JOBS" \
    -H 'Content-Type: application/json' -H "Origin: $ORIGIN" -H "X-CSRF-Token: $CSRF" \
    -H "Idempotency-Key: $3" -d @"$TMPD/jobreq.json")
  cp "$TMPD/resp.json" "$WORK/submit.json"
  printf '%s' "$_code"
}

wait_job() {
  _i=0
  while [ "$_i" -lt 400 ]; do
    api "$JAR" "$CSRF" GET "/api/v1/jobs/$1" >/dev/null
    cp "$TMPD/resp.json" "$WORK/job.json"
    _st=$(jobget 'd["data"]["status"]')
    case "$_st" in
      succeeded|failed|canceled|interrupted) printf '%s' "$_st"; return 0 ;;
    esac
    _i=$((_i + 1))
    sleep 0.2
  done
  printf 'timeout'
}

# 幂等键每次运行都不同：Web 侧的幂等记录是持久的（重复的键会判 IDEMPOTENCY_CONFLICT）。
RUN_KEY="-$(date -u +%Y%m%d%H%M%S)-$$"

run_action() {
  PRE_CODE=$(preflight "$1" "$2")
  cp "$WORK/preflight.json" "$PAYLOADS/preflight-$3.json"
  PRE_ALLOWED=$(pget 'd["data"]["allowed"]')
  PRE_ID=$(pget 'd["data"]["preflight_id"]')
  PRE_PHRASE=$(pget 'd["data"].get("confirmation_phrase") or ""')
  PRE_REAUTH=$(pget 'bool(d["data"].get("requires_reauth"))')
  if [ "$PRE_REAUTH" = "True" ]; then
    REAUTH_CODE=$(reauth)
    [ "$REAUTH_CODE" = "200" ] || fail "再认证（$3）" "状态码 $REAUTH_CODE"
  fi
  if [ -z "$PRE_ID" ] || { [ "$PRE_CODE" != "200" ] && [ "$PRE_CODE" != "201" ]; }; then
    fail "预检（$3）给出可提交的计划" "状态码 $PRE_CODE，响应 $(head -c 300 "$WORK/preflight.json")"
    JOB_ID=""
    JOB_STATE="preflight:$PRE_CODE"
    : >"$WORK/job.json"
    return 0
  fi
  JOB_CODE=$(submit_job "$PRE_ID" "$PRE_PHRASE" "$3$RUN_KEY")
  cp "$WORK/submit.json" "$PAYLOADS/submit-$3.json"
  if [ "$JOB_CODE" = "202" ]; then
    JOB_ID=$(jget "$WORK/submit.json" 'd["data"]["job_id"]')
    JOB_STATE=$(wait_job "$JOB_ID")
    cp "$WORK/job.json" "$PAYLOADS/job-$3.json"
  else
    JOB_ID=""
    JOB_STATE="submit:$JOB_CODE"
    : >"$WORK/job.json"
    fail "提交（$3）" "状态码 $JOB_CODE，响应 $(head -c 300 "$WORK/submit.json")"
  fi
}

get_shares() {
  _code=$(api "$JAR" "$CSRF" GET "$SHARES_PATH")
  cp "$TMPD/resp.json" "$WORK/shares-$1.json"
  printf '%s' "$_code"
}
sget() { jget "$WORK/shares-$1.json" "$2"; }
get_share() {
  _code=$(api "$JAR" "$CSRF" GET "$SHARES_PATH/$1")
  cp "$TMPD/resp.json" "$WORK/share-$2.json"
  printf '%s' "$_code"
}
shget() { jget "$WORK/share-$1.json" "$2"; }
volumes() {
  _code=$(api "$JAR" "$CSRF" GET "$STORE/volumes")
  cp "$TMPD/resp.json" "$WORK/volumes-$1.json"
  printf '%s' "$_code"
}
volsget() { jget "$WORK/volumes-$1.json" "$2"; }
current_vol_version() { volumes "v$2" >/dev/null; volsget "v$2" "[v for v in d[\"data\"][\"volumes\"] if v[\"id\"]==\"$1\"][0][\"version\"]"; }
current_share_version() { get_share "$1" "ver$2" >/dev/null; shget "ver$2" 'd["data"]["share"]["version"]'; }

# 一条 NFS 共享的定义（读写/只读、一条网段）。$1 名字 $2 卷 $3 相对路径 $4 ro|rw $5 网段
nfs_definition() {
  printf '{"name":"%s","protocol":"nfs","volume_id":"%s","relative_path":"%s","enabled":true,"read_only":%s,"allowed_cidrs":["%s"],"smb_users":[],"nfs_options":{"root_squash":true,"sync":true}}' \
    "$1" "$2" "$3" "$4" "$5"
}

# ---------------------------------------------------------------------------
echo "== 0. 环境与起点 =="
# ---------------------------------------------------------------------------

for p in "$AGENT" "$WEB" "$CTL"; do
  if [ -x "$p" ]; then pass "二进制存在：$p"; else echo "缺少 $p（先交叉编译到 $BIN_DIR）" >&2; exit 2; fi
done
for tool in exportfs showmount mount.nfs mountpoint findmnt python3 sgdisk losetup blkid; do
  if command -v "$tool" >/dev/null 2>&1; then pass "夹具：有 $tool"; else
    echo "缺少 $tool（nfs-kernel-server / nfs-common / util-linux）——这是环境不满足，不是产品缺陷" >&2
    exit 2
  fi
done
if [ -f /etc/systemd/system/nfs-server.service ] || systemctl list-unit-files nfs-server.service >/dev/null 2>&1; then
  pass "夹具：有 nfs-server.service"
else
  echo "没有 nfs-server.service" >&2; exit 2
fi
if [ "$(systemctl is-active nfs-server 2>/dev/null)" != "active" ]; then
  systemctl start nfs-server >/dev/null 2>&1 || true
  sleep 1
fi
expect_eq "夹具：nfs-server 在运行" "active" "$(systemctl is-active nfs-server 2>/dev/null)"

# 承载 / 的设备：一次性盘绝不能是它。
ROOT_SRC=$(findmnt -rno SOURCE / 2>/dev/null)
ROOT_REAL=$(readlink -f "$ROOT_SRC" 2>/dev/null)
if [ -z "$ROOT_REAL" ] || [ ! -b "$ROOT_REAL" ]; then
  echo "无法确定承载 / 的块设备（findmnt 给出 '$ROOT_SRC'）" >&2; exit 2
fi
ROOT_DISK=$(lsblk -no PKNAME "$ROOT_REAL" 2>/dev/null | head -1)
[ -z "$ROOT_DISK" ] && ROOT_DISK=$(basename "$ROOT_REAL")
pass "承载 / 的设备是 $ROOT_REAL（整盘 $ROOT_DISK）"

BLANK_A=${SPARE_DISKS:-}
BLANK_A=$(printf '%s' "$BLANK_A" | awk '{print $1}')
if [ -z "$BLANK_A" ]; then
  # 没有指定一次性盘时不猜：猜错就是擦掉一台机器。
  echo '需要一块一次性空盘：SPARE_DISKS=sdb（会被格式化）' >&2; exit 2
fi
if [ ! -e "/dev/$BLANK_A" ]; then echo "/dev/$BLANK_A 不存在" >&2; exit 2; fi
if [ "$BLANK_A" = "$ROOT_DISK" ]; then echo "/dev/$BLANK_A 就是承载 / 的整盘，拒绝使用" >&2; exit 2; fi
if [ "$(lsblk -no TYPE "/dev/$BLANK_A" 2>/dev/null | head -1)" != "disk" ]; then
  echo "/dev/$BLANK_A 不是整盘（lsblk type != disk），拒绝使用" >&2; exit 2
fi
if lsblk -no MOUNTPOINTS "/dev/$BLANK_A" 2>/dev/null | grep -q '[^ ]'; then
  echo "/dev/$BLANK_A 上有挂载点，拒绝使用" >&2; exit 2
fi
_size=$(blockdev --getsize64 "/dev/$BLANK_A" 2>/dev/null || echo 0)
if [ "$_size" -lt $((PART_MAIN + 64 * 1048576)) ]; then
  echo "/dev/$BLANK_A 容量 ${_size} 字节，放不下夹具分区" >&2; exit 2
fi
pass "一次性盘 /dev/$BLANK_A 可用（$(numfmt --to=iec "$_size" 2>/dev/null || echo "$_size")）"

rm -rf "$WORK"
mkdir -p "$WORK" "$TMPD" "$FACTS" "$PAYLOADS" "$WORK/logs" "$WEB_DB_DIR"
chmod 0755 "$WORK" "$WORK/logs" "$PAYLOADS" "$FACTS"
chmod 0700 "$TMPD"
chown -R "$WEB_USER" "$WEB_DB_DIR"
rm -f "$RESULT"

# 上一轮的客户端挂载先卸掉：它们挂着的时候下面"起点干净"的断言全是假的。
unmount_clients

# /etc/exports 与 /etc/exports.d 整份留档，之后清成干净起点——这一轮要判断"面板有没有
# 碰别人的导出"，起点里混着上一轮手工实验留下的 fsid=0 假根会让结论无意义。
MAIN_EXPORTS_ARCHIVE=$WORK/logs/exports-initial.txt
EXPORTS_DIR_ARCHIVE=$WORK/logs/exports.d-initial
cp "$MAIN_EXPORTS" "$MAIN_EXPORTS_ARCHIVE" 2>/dev/null || : >"$MAIN_EXPORTS_ARCHIVE"
mkdir -p "$EXPORTS_DIR_ARCHIVE"
cp "$EXPORTS_DIR"/*.exports "$EXPORTS_DIR_ARCHIVE"/ 2>/dev/null || true
note "起点留档：$MAIN_EXPORTS_ARCHIVE、$EXPORTS_DIR_ARCHIVE（收尾时放回）"

install -d -m 0755 "$EXPORTS_DIR"
if [ -s "$MAIN_EXPORTS" ]; then
  : >"$MAIN_EXPORTS"
  note "清空 $MAIN_EXPORTS（内容已留档）"
fi
for f in "$EXPORTS_DIR"/*.exports; do
  [ -e "$f" ] || continue
  rm -f "$f"
  note "移走导出文件 $f（已留档）"
done

# 面板自己写的那两处也要回到"没做过"：开机依赖 drop-in 与 /etc/fstab 里的受管理块。
#
# 它们都是**面板的产物**，上一轮如果没走到"删卷/停用"就失败，就会留在这里。留着会让
# 这一轮的起点断言与卷导入假红——第二轮验收就是这么红的：
#   * drop-in 还在 → "起点：没有开机依赖 drop-in" 直接 FAIL；
#   * fstab 里还剩 `... /srv/upanel/volumes/data ...` → 导入同一块盘撞
#     `写 /etc/fstab 失败：挂载点 "/srv/upanel/volumes/data" 重复`。
# 这不是产品缺陷，是验收脚本自己没有把上一轮的现场清干净（ENV 类假红）。
DROPIN_ARCHIVE=$WORK/logs/startup-dropin-initial.conf
if [ -f "$EXPORTS_STARTUP_DROPIN" ]; then
  cp "$EXPORTS_STARTUP_DROPIN" "$DROPIN_ARCHIVE" 2>/dev/null || true
  rm -f "$EXPORTS_STARTUP_DROPIN"
  rmdir "$(dirname "$EXPORTS_STARTUP_DROPIN")" 2>/dev/null || true
  note "移走上一轮留下的开机依赖 drop-in（已留档）"
fi
if grep -q '^# BEGIN upanel managed volumes' "$FSTAB" 2>/dev/null; then
  cp "$FSTAB" "$WORK/logs/fstab-initial.txt" 2>/dev/null || true
  sed -i '/^# BEGIN upanel managed volumes/,/^# END upanel managed volumes/d' "$FSTAB"
  systemctl daemon-reload 2>/dev/null || true
  note "摘掉上一轮留下的 upanel fstab 块（原文留在 $WORK/logs/fstab-initial.txt）"
fi

exportfs -ra >/dev/null 2>&1 || true
# 基准哈希要在**清完之后**取。第一轮取在了清空之前，于是收尾那条"没人动 /etc/exports"
# 拿清空前的哈希去比清空后的文件，永远不等——一条与产品无关的假红。
MAIN_SHA_BEFORE=$(main_exports_sha)
expect_eq "起点：实际导出表是空的" "" "$(exports_table | tr -d '\n')"
expect_eq "起点：没有 $EXPORTS_FILE" "False" "$(test -f "$EXPORTS_FILE" && echo True || echo False)"
expect_eq "起点：没有开机依赖 drop-in" "False" "$(test -f "$EXPORTS_STARTUP_DROPIN" && echo True || echo False)"

lsblk -o NAME,TYPE,SIZE,FSTYPE,MOUNTPOINTS >"$FACTS/lsblk-initial.txt" 2>&1
wipefs -a "/dev/$BLANK_A" >/dev/null 2>&1
sgdisk --zap-all "/dev/$BLANK_A" >/dev/null 2>&1
blockdev --rereadpt "/dev/$BLANK_A" >/dev/null 2>&1
udevadm settle --timeout=5 >/dev/null 2>&1
pass "一次性盘 /dev/$BLANK_A 清成空白"
rm -rf "$VOLUME_ROOT"/* 2>/dev/null
install -d -m 0755 "$VOLUME_ROOT"

# ---------------------------------------------------------------------------
echo
echo "== 1. 配置与启动（代理 root、Web 非 root） =="
# ---------------------------------------------------------------------------

install -d -m 0755 /etc/upanel
install -d -m 0700 "$AGENT_DB_DIR"
install -d -m 0755 "$RUN_DIR"
rm -f "$AGENT_DB" "$AGENT_DB-wal" "$AGENT_DB-shm"
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
expect_eq "管理员登录" "200" "$(login admin "$PASSWORD" "$JAR" login-admin)"
CSRF=$(csrf_of "$WORK/login-admin.json")
expect_ne "拿到 CSRF 令牌" "" "$CSRF"

expect_eq "页面在 /shares 上有入口（SPA 返回 index.html）" "200" \
  "$(curl -s -o /dev/null -w '%{http_code}' "$BASE/shares")"

# ---------------------------------------------------------------------------
echo
echo "== 2. 夹具：一块卷 =="
# ---------------------------------------------------------------------------

run_action storage.partition_table.initialize \
  "{\"device_id\":\"$BLANK_A\",\"table_type\":\"gpt\"}" init-a
expect_eq "初始化分区表任务终态" "succeeded" "$JOB_STATE"
run_action storage.partition.create \
  "{\"device_id\":\"$BLANK_A\",\"start_bytes\":\"$PART_START\",\"size_bytes\":\"$PART_MAIN\",\"name\":\"data\"}" create-a
expect_eq "建分区任务终态" "succeeded" "$JOB_STATE"
_i=0
while [ "$_i" -lt 40 ]; do
  [ -b "/dev/${BLANK_A}1" ] && break
  _i=$((_i + 1)); sleep 0.2
done
run_action storage.filesystem.format \
  "{\"device_id\":\"${BLANK_A}1\",\"filesystem\":\"ext4\",\"label\":\"$LABEL\"}" format-a
expect_eq "格式化任务终态" "succeeded" "$JOB_STATE"
expect_eq "独立证据：blkid 读出的类型是 ext4" "ext4" "$(blkid_field "/dev/${BLANK_A}1" TYPE)"

run_action storage.volume.import \
  "{\"device_id\":\"${BLANK_A}1\",\"slug\":\"$SLUG\",\"display_name\":\"NFS 数据卷\"}" import-a
expect_eq "导入任务终态" "succeeded" "$JOB_STATE"
VOL_ID=$(jobdata volume_id)
MP=$VOLUME_ROOT/$SLUG
VOL_VER=$(current_vol_version "$VOL_ID" m1)
run_action storage.volume.mount "{\"volume_id\":\"$VOL_ID\",\"version\":\"$VOL_VER\"}" mount-a
expect_eq "挂载任务终态" "succeeded" "$JOB_STATE"
expect_eq "独立证据：卷真的挂上了" "True" "$(is_mounted "$MP")"

# ---------------------------------------------------------------------------
echo
echo "== 3. 能力报告：NFS 那一路是现场探出来的 =="
# ---------------------------------------------------------------------------

expect_eq "共享列表可读" "200" "$(get_shares cap)"
cp "$WORK/shares-cap.json" "$PAYLOADS/capability.json"
expect_eq "capability.nfs.status 是 available" "available" "$(sget cap 'd["data"]["capability"]["nfs"]["status"]')"
expect_eq "capability.nfs.exports_file 指向面板独占的那一份" "$EXPORTS_FILE" \
  "$(sget cap 'd["data"]["capability"]["nfs"]["exports_file"]')"
expect_ne "capability.nfs 报出了 exportfs 的路径（执行层要的就是它）" "" \
  "$(sget cap 'd["data"]["capability"]["nfs"].get("exportfs") or ""')"
expect_eq "capability.nfs.service_state 是 active" "active" \
  "$(sget cap 'd["data"]["capability"]["nfs"]["service_state"]')"
expect_eq "此刻没有面板写的导出" "True" \
  "$(sget cap '(d["data"]["capability"]["nfs"].get("exported_paths") or []) == []')"

# ---------------------------------------------------------------------------
echo
echo "== 4. 应用读写导出：写文件 + 读回实际导出表 =="
# ---------------------------------------------------------------------------

DEF_RW=$(nfs_definition "$SHARE_RW" "$VOL_ID" "" false "$CIDR_LOCAL")
run_action share.apply "{\"share_id\":null,\"version\":null,\"definition\":$DEF_RW}" apply-rw
expect_eq "应用预检允许" "True" "$PRE_ALLOWED"
expect_eq "预检没有阻塞项" "" "$(pall)"
expect_contains "预检告警里说清了'此刻还没进导出表'" "EXPORTS_NOT_APPLIED" "$(pwarn)"
expect_eq "应用任务终态" "succeeded" "$JOB_STATE"
expect_eq "结果 verified（读回比对通过才可能）" "verified" "$(joutcome)"
SHARE_RW_ID=$(jobdata share_id)
expect_share_id "拿到共享标识" "$SHARE_RW_ID"
# 第一个应用是整个脚本的地基：它没成功，后面的挂载、只读、卷依赖全都没有可验的对象。
# 继续跑只会用垃圾参数制造几十条假红，把真正的失败埋掉——所以这里明确收手，
# 并把结果写出来（RESULT 里有 PASS/FAIL 与失败项名字），不把"没跑"当成"通过"。
if ! printf '%s' "$SHARE_RW_ID" | grep -Eq '^shr_[0-9a-f]{32}$'; then
  note "第一个应用任务没有成功（见上一行与 agent 日志）：后续各节以它为前置，未执行"
  write_result
  exit 1
fi
expect_eq "observed exports_file = written" "written" "$(jobobserved exports_file)"
expect_eq "observed reload = done" "done" "$(jobobserved reload)"
expect_eq "observed service = active" "active" "$(jobobserved service)"
expect_eq "observed served = True" "True" "$(jobobserved served)"
expect_eq "observed directory_mode = 0777" "-rwxrwxrwx" "$(jobobserved directory_mode)"

expect_eq "独立证据：托管导出文件存在" "True" "$(test -f "$EXPORTS_FILE" && echo True || echo False)"
expect_eq "独立证据：文件里那一行带 upanel 认领标记" "True" "$(exports_file_has "# upanel:$SHARE_RW_ID")"
expect_eq "独立证据：行里有身份网段" "True" "$(exports_file_has "$CIDR_LOCAL")"
expect_eq "独立证据：行里是 rw" "True" "$(exports_file_has 'rw')"
expect_eq "独立证据：行里没有 no_root_squash / async / insecure / *" "False" \
  "$(exports_file_text | grep -qE 'no_root_squash|async|insecure|[ (]\*' && echo True || echo False)"
expect_eq "独立证据：目录权限是 0777" "777" "$(dir_mode "$MP")"
expect_eq "独立证据（核心）：exportfs -v 的实际导出表里有这个路径" "True" "$(exports_table_has "$MP")"
expect_eq "独立证据：实际导出表里带 root_squash" "True" \
  "$(export_clients_of "$MP" | grep -qF 'root_squash' && echo True || echo False)"
expect_eq "独立证据：实际导出表里带 no_subtree_check" "True" \
  "$(export_clients_of "$MP" | grep -qF 'no_subtree_check' && echo True || echo False)"
expect_eq "开机依赖 drop-in 写好了" "True" "$(test -f "$EXPORTS_STARTUP_DROPIN" && echo True || echo False)"
expect_eq "随机启动之前先要求卷就位（drop-in 里点名了挂载路径）" "True" \
  "$(grep -qF "$MP" "$EXPORTS_STARTUP_DROPIN" && echo True || echo False)"

# ---------------------------------------------------------------------------
echo
echo "== 5. 真实客户端：NFSv3 挂载、先 findmnt、再读写 =="
# ---------------------------------------------------------------------------

# mount 的现场写在哪个文件里。
#
# 不能拼成 `$WORK/mount-$MOUNT_RW.err`：挂载点是 /mnt/... 这种带斜杠的路径，
# 拼出来的文件名里就有了目录分隔符，重定向直接失败（`cannot create …: Directory nonexistent`），
# 于是记住的是**重定向的**退出码，mount 根本没跑——第一轮验收的红就是这么来的。
mount_errfile() { printf '%s/mount-%s.err' "$WORK" "$(basename "$1")"; }

mount_nfs() { # $1 源目录 $2 挂载点 $3 额外选项
  install -d -m 0755 "$2"
  if mount -t nfs -o "vers=3,proto=tcp${3:+,$3}" "127.0.0.1:$1" "$2" 2>"$(mount_errfile "$2")"; then
    printf '0'
  else
    printf '%s' "$?"
  fi
}

MOUNT_RC=$(mount_nfs "$MP" "$MOUNT_RW" "")
printf 'mount 退出码 %s\n' "$MOUNT_RC" >"$FACTS/mount-rw.txt"
expect_eq "客户端挂载的退出码是 0" "0" "$MOUNT_RC"
# 这一条是这一轮的重点：挂载失败时 mount 的退出码**仍然可能是 0**，文件会静默写到本地目录。
# 所以任何写入之前必须先 findmnt 确认真的挂上了。
expect_eq "核心：findmnt 确认它真的挂上了（不是静默写本地）" "True" "$(is_mounted "$MOUNT_RW")"
findmnt -rno TARGET,SOURCE,FSTYPE,OPTIONS --mountpoint "$MOUNT_RW" >"$FACTS/mount-rw-findmnt.txt" 2>&1
expect_contains "挂的是 nfs，源就是服务端路径" "nfs" "$(mount_options "$MOUNT_RW")|$(findmnt -rno FSTYPE --mountpoint "$MOUNT_RW")"

PROBE_LOCAL=$WORK/logs/probe-write.txt
printf 'upanel-nfs-probe %s\n' "$(date -u +%FT%TZ)" >"$PROBE_LOCAL"
PROBE_SHA=$(sha256sum "$PROBE_LOCAL" | cut -d' ' -f1)
if cp "$PROBE_LOCAL" "$MOUNT_RW/probe-write.txt" 2>"$WORK/write-rw.err"; then
  pass "客户端写文件成功（走的是挂载点）"
else
  fail "客户端写文件" "$(cat "$WORK/write-rw.err" 2>/dev/null)"
fi
expect_eq "核心：从**服务端路径**读回的内容与写入一致（数据真的过了 NFS）" "$PROBE_SHA" \
  "$(sha256sum "$MP/probe-write.txt" 2>/dev/null | cut -d' ' -f1)"
# root_squash 的真凭据：客户端是 root，落盘属主却是 nobody。
expect_eq "核心：root_squash 生效——root 写进去的文件落盘属主是 nobody" "nobody" \
  "$(stat -c '%U' "$MP/probe-write.txt" 2>/dev/null)"
expect_eq "核心：组也 squash 成了 nogroup" "nogroup" \
  "$(stat -c '%G' "$MP/probe-write.txt" 2>/dev/null)"

# 名字里带 no_root_squash 的开关根本不存在：想放宽只能改代码（PLAN §10）。
PRE_CODE=$(preflight share.apply "{\"share_id\":\"$SHARE_RW_ID\",\"version\":\"$(current_share_version "$SHARE_RW_ID" bad)\",\"definition\":$(printf '{"name":"%s","protocol":"nfs","volume_id":"%s","relative_path":"","enabled":true,"read_only":false,"allowed_cidrs":["%s"],"smb_users":[],"nfs_options":{"root_squash":false,"sync":true}}' "$SHARE_RW" "$VOL_ID" "$CIDR_LOCAL")}" )
cp "$WORK/preflight.json" "$PAYLOADS/preflight-root-squash-off.json"
expect_eq "root_squash=false 被预检拒（NFS_OPTIONS_INVALID）" "False" "$(pget 'd["data"]["allowed"]')"
expect_contains "拒绝码是 NFS_OPTIONS_INVALID" "NFS_OPTIONS_INVALID" "$(pall)"
PRE_CODE=$(preflight share.apply "{\"share_id\":\"$SHARE_RW_ID\",\"version\":\"$(current_share_version "$SHARE_RW_ID" bad2)\",\"definition\":$(printf '{"name":"%s","protocol":"nfs","volume_id":"%s","relative_path":"","enabled":true,"read_only":false,"allowed_cidrs":["0.0.0.0/0"],"smb_users":[],"nfs_options":{"root_squash":true,"sync":true}}' "$SHARE_RW" "$VOL_ID")}" )
cp "$WORK/preflight.json" "$PAYLOADS/preflight-open-world.json"
expect_contains "0.0.0.0/0 被预检拒（NFS_OPEN_TO_WORLD）" "NFS_OPEN_TO_WORLD" "$(pall)"
PRE_CODE=$(preflight share.apply "{\"share_id\":\"$SHARE_RW_ID\",\"version\":\"$(current_share_version "$SHARE_RW_ID" bad3)\",\"definition\":$(printf '{"name":"%s","protocol":"nfs","volume_id":"%s","relative_path":"","enabled":true,"read_only":false,"allowed_cidrs":[],"smb_users":[],"nfs_options":{"root_squash":true,"sync":true}}' "$SHARE_RW" "$VOL_ID")}" )
cp "$WORK/preflight.json" "$PAYLOADS/preflight-no-cidr.json"
expect_contains "空网段被预检拒（SHARE_NO_CIDRS）" "SHARE_NO_CIDRS" "$(pall)"

# 幂等：同样的定义再应用一次，文件不该重写、也不该刷表。
SHARE_VER=$(current_share_version "$SHARE_RW_ID" idem)
run_action share.apply "{\"share_id\":\"$SHARE_RW_ID\",\"version\":\"$SHARE_VER\",\"definition\":$DEF_RW}" apply-rw-again
expect_eq "幂等重放任务终态" "succeeded" "$JOB_STATE"
expect_eq "幂等重放：文件没重写" "unchanged" "$(jobobserved exports_file)"
expect_eq "幂等重放：没有刷表（内容没变）" "skipped_unchanged" "$(jobobserved reload)"
expect_eq "幂等重放仍然是 verified" "verified" "$(joutcome)"

# ---------------------------------------------------------------------------
echo
echo "== 6. 网段是唯一的访问控制：名单外连不上 =="
# ---------------------------------------------------------------------------

SHARE_VER=$(current_share_version "$SHARE_RW_ID" off)
DEF_FOREIGN=$(nfs_definition "$SHARE_RW" "$VOL_ID" "" false "$CIDR_FOREIGN")
run_action share.apply "{\"share_id\":\"$SHARE_RW_ID\",\"version\":\"$SHARE_VER\",\"definition\":$DEF_FOREIGN}" apply-foreign
expect_eq "改成名单外网段的任务终态" "succeeded" "$JOB_STATE"
expect_eq "独立证据：文件里现在是名单外那个网段" "True" "$(exports_file_has "$CIDR_FOREIGN")"
expect_eq "独立证据：实际导出表里也是它" "True" \
  "$(export_clients_of "$MP" | grep -qF "$CIDR_FOREIGN" && echo True || echo False)"

umount -l "$MOUNT_RW" 2>/dev/null || true
sleep 1
expect_eq "起点：名单外的挂载点已经卸掉" "False" "$(is_mounted "$MOUNT_RW")"
FOREIGN_RC=$(mount_nfs "$MP" "$MOUNT_RW" "")
printf 'mount 退出码 %s\n%s\n' "$FOREIGN_RC" "$(cat "$(mount_errfile "$MOUNT_RW")" 2>/dev/null)" >"$FACTS/mount-foreign-cidr.txt"
expect_ne "核心：名单外的客户端挂不上（挂载非 0 退出）" "0" "$FOREIGN_RC"
# 挂载失败时目录里可能被写进本地文件——所以这里再确认一次“没挂上”而不是“挂上了但空了”。
expect_eq "核心：名单外确实没挂上（findmnt 为空）" "False" "$(is_mounted "$MOUNT_RW")"

SHARE_VER=$(current_share_version "$SHARE_RW_ID" back)
run_action share.apply "{\"share_id\":\"$SHARE_RW_ID\",\"version\":\"$SHARE_VER\",\"definition\":$DEF_RW}" apply-back
expect_eq "改回本机网段的任务终态" "succeeded" "$JOB_STATE"
MOUNT_RC=$(mount_nfs "$MP" "$MOUNT_RW" "")
expect_eq "核心：改回来又能挂上" "0" "$MOUNT_RC"
expect_eq "核心：findmnt 确认挂上了" "True" "$(is_mounted "$MOUNT_RW")"

# ---------------------------------------------------------------------------
echo
echo "== 7. 只读共享：导出行 ro + 目录 0755，客户端写不进去 =="
# ---------------------------------------------------------------------------

DEF_RO=$(nfs_definition "$SHARE_RO" "$VOL_ID" "$REL_RO" true "$CIDR_LOCAL")
run_action share.apply "{\"share_id\":null,\"version\":null,\"definition\":$DEF_RO}" apply-ro
expect_eq "只读导出预检允许" "True" "$PRE_ALLOWED"
expect_eq "只读导出任务终态" "succeeded" "$JOB_STATE"
expect_eq "只读导出结果 verified" "verified" "$(joutcome)"
SHARE_RO_ID=$(jobdata share_id)
expect_share_id "拿到只读共享的标识" "$SHARE_RO_ID"
expect_eq "只读目录权限是 0755" "755" "$(dir_mode "$MP/$REL_RO")"
expect_eq "独立证据：文件里那一行是 ro" "True" \
  "$(exports_file_text | grep -F "$MP/$REL_RO" | grep -qF 'ro,' && echo True || echo False)"
expect_eq "独立证据：实际导出表里是 ro" "True" \
  "$(exports_table | grep -F "$MP/$REL_RO" | grep -qF 'ro' && echo True || echo False)"
expect_eq "两条导出都在托管文件里" "2" "$(managed_line_count)"

printf 'ro-probe %s\n' "$(date -u +%FT%TZ)" >"$MP/$REL_RO/readme.txt"
expect_eq "服务端放一个只读文件" "0" "$?"
# 权限必须显式给：root_squash 把客户端 root 映射成 nobody，而脚本是 root 跑的、
# sudo 带下来的 umask 是 077——不 chmod 就是 0600，客户端连**读**都读不到。
# 第一轮就是这么红的（`readme.txt` 两端都是 -rw-------，grep 读不到内容），
# 而产品行为是对的：服务端没有义务让被 squash 的身份读一个只有 root 能读的文件。
chmod 0644 "$MP/$REL_RO/readme.txt"

MOUNT_RC=$(mount_nfs "$MP/$REL_RO" "$MOUNT_RO" "")
expect_eq "只读共享挂载退出码是 0" "0" "$MOUNT_RC"
expect_eq "findmnt 确认只读共享挂上了" "True" "$(is_mounted "$MOUNT_RO")"
# 只读**不是**客户端挂载选项里的 ro。22.04 裸机实测：服务端导出 `ro`，客户端 `mount -o
# vers=3,proto=tcp` 之后 `findmnt` 的 OPTIONS 照样是 `rw,relatime,vers=3,…`——NFSv3 的
# 只读是服务端逐次拒绝写请求（EROFS），客户端选项里根本看不到 ro。断言“OPTIONS 里有 ro”
# 会永远红，而且红得让人去怀疑产品。真正的三道证据是：导出行是 ro（上面）、目录 0755
# （上面）、内核这次写入被拒且错误是只读（下面）。OPTIONS 只留档，不参与判决。
{
  echo "--- findmnt"
  findmnt -rno TARGET,OPTIONS --mountpoint "$MOUNT_RO"
  echo "--- 客户端看到的内容"
  ls -la "$MOUNT_RO"
  echo "--- 服务端目录"
  ls -la "$MP/$REL_RO"
} >"$FACTS/mount-ro.txt" 2>&1
expect_eq "客户端读得到服务端放进去的文件" "True" \
  "$(grep -qF 'ro-probe' "$MOUNT_RO/readme.txt" 2>/dev/null && echo True || echo False)"
if printf 'x\n' >"$MOUNT_RO/probe-write.txt" 2>"$WORK/write-ro.err"; then
  fail "核心：只读共享上客户端写不进去" "写成功了——ro 没起作用"
else
  pass "核心：只读共享上客户端写不进去（内核拒绝）"
fi
expect_not_contains "拒绝是内核的只读错误，不是别的原因" "No such file" "$(cat "$WORK/write-ro.err" 2>/dev/null)"
expect_eq "只读共享目录里没有多出写入的文件" "False" \
  "$(test -e "$MP/$REL_RO/probe-write.txt" && echo True || echo False)"

# 到这里就把客户端挂载卸掉：本机既是服务端又是客户端，一个还挂着的 NFS 客户端会让
# 第 9 节“从外面把卷卸掉”报 target is busy——而那一节整片断言都建立在“卷真的掉线了”。
unmount_clients
expect_eq "只读共享的客户端挂载已经卸干净" "False" "$(is_mounted "$MOUNT_RO")"

# ---------------------------------------------------------------------------
echo
echo "== 8. 同路径冲突：预检就拦下，不是等 exportfs 报 duplicated =="
# ---------------------------------------------------------------------------

DEF_CONFLICT=$(nfs_definition same-path "$VOL_ID" "" false "$CIDR_LOCAL")
PRE_CODE=$(preflight share.apply "{\"share_id\":null,\"version\":null,\"definition\":$DEF_CONFLICT}")
cp "$WORK/preflight.json" "$PAYLOADS/preflight-conflict.json"
expect_eq "同目录的第二个导出被拦下" "False" "$(pget 'd["data"]["allowed"]')"
expect_contains "阻塞码是 EXPORT_PATH_CONFLICT" "EXPORT_PATH_CONFLICT" "$(pall)"
expect_eq "被拦下时实际导出表没被动" "True" "$(exports_table_has "$MP")"

# ---------------------------------------------------------------------------
echo
echo "== 9. 卷依赖：卸载被拒、掉线撤销、挂回来不自动复活 =="
# ---------------------------------------------------------------------------

VOL_VER=$(current_vol_version "$VOL_ID" b0)
PRE_CODE=$(preflight storage.volume.unmount "{\"volume_id\":\"$VOL_ID\",\"version\":\"$VOL_VER\"}")
cp "$WORK/preflight.json" "$PAYLOADS/preflight-unmount-blocked.json"
expect_eq "有启用导出时，面板拒绝卸载这块卷" "False" "$(pget 'd["data"]["allowed"]')"
expect_contains "拒绝的理由是 VOLUME_IN_USE_BY_SHARE" "VOLUME_IN_USE_BY_SHARE" "$(pall)"

unmount_clients
expect_eq "客户端挂载已卸掉（否则卷也卸不掉）" "False" "$(is_mounted "$MOUNT_RW")"
expect_eq "只读共享的客户端挂载也已卸掉" "False" "$(is_mounted "$MOUNT_RO")"
# 为什么这里是 `umount -l` 而不是 `umount`：**导出活着的卷根本卸不掉**。
#
# 22.04 裸机实测（`fuser -vm` 点名）：只要 knfsd 还持有那个文件系统（导出过、
# 并且有客户端连过一次），普通 `umount` 一定返回 32 / `target is busy`，堆栈里是
# `kernel knfsd`。这一轮前两次验收就是卡在这儿：卷从没真的掉线，后面整片撤销断言
# 跟着全红。反过来，只导出一个 tmpfs、没有客户端连过，`umount` 是能成功的——
# 差别不在产品，在“内核里有没有人拿着它”。
#
# lazy umount 摘的是**命名空间里的这一个挂载**：`findmnt` 立刻为空，面板的探针看到
# 的就是 unmounted，正是要验的那一侧。knfsd 手里那份还会活一会儿，但面板接着会摘
# 掉导出行并 `exportfs -ra`，那才是这里要核的因果。
#
# 没验的一侧：设备级掉线（拔盘、I/O 错误之后 `statfs` 报错）——这台 VM 是
# `-monitor none` 起的，跑的时候没法把 sdb 拔掉。这一条记在结论的 NOT RUN 里。
UMOUNT_ERR=$(umount -l "$MP" 2>&1)
[ -z "$UMOUNT_ERR" ] || note "umount -l $MP 的报错：$UMOUNT_ERR"
expect_eq "独立证据：卷从挂载树里摘掉了" "False" "$(is_mounted "$MP")"

# 撤销循环每 30 秒跑一次：最多等 75 秒。
REVOKE_WAIT=0
while [ "$REVOKE_WAIT" -lt 75 ]; do
  get_shares offline >/dev/null
  if [ "$(sget offline "bool([s for s in d[\"data\"][\"shares\"] if s[\"id\"]==\"$SHARE_RW_ID\"][0].get(\"runtime_revoked\"))")" = "True" ]; then break; fi
  REVOKE_WAIT=$((REVOKE_WAIT + 1))
  sleep 1
done
note "等撤销用了 ${REVOKE_WAIT} 秒"
cp "$WORK/shares-offline.json" "$PAYLOADS/page-shares-offline.json"
SEL_OFF="[s for s in d[\"data\"][\"shares\"] if s[\"id\"]==\"$SHARE_RW_ID\"][0]"
expect_eq "运行时撤销记录出现了" "True" "$(sget offline "bool($SEL_OFF.get(\"runtime_revoked\"))")"
expect_eq "撤销记录里写着当时的卷状态" "unmounted" "$(sget offline "$SEL_OFF[\"runtime_revoked\"][\"volume_state\"]")"
expect_eq "config_applied 跟着变成假" "False" "$(sget offline "bool($SEL_OFF[\"config_applied\"])")"
expect_eq "独立证据（核心）：托管导出文件里已经没有这个路径" "False" "$(exports_file_has "$MP ")"
expect_eq "独立证据（核心）：实际导出表里也没有了" "False" "$(exports_table_has "$MP")"
EXP_OFFLINE=$(mount_nfs "$MP" "$MOUNT_RW" "")
printf 'mount 退出码 %s\n%s\n' "$EXP_OFFLINE" "$(cat "$(mount_errfile "$MOUNT_RW")" 2>/dev/null)" >"$FACTS/mount-after-revoke.txt"
expect_eq "卷掉线后客户端挂不上" "False" "$(is_mounted "$MOUNT_RW")"

MOUNT_BACK_ERR=$(mount "$MP" 2>&1)
[ -z "$MOUNT_BACK_ERR" ] || note "mount $MP 的报错：$MOUNT_BACK_ERR"
expect_eq "独立证据：卷又挂上了" "True" "$(is_mounted "$MP")"
MOUNT_BACK_WAIT=0
while [ "$MOUNT_BACK_WAIT" -lt 20 ]; do
  volumes restored >/dev/null
  if [ "$(volsget restored "[v for v in d[\"data\"][\"volumes\"] if v[\"id\"]==\"$VOL_ID\"][0][\"observed_state\"]")" = "mounted" ]; then break; fi
  MOUNT_BACK_WAIT=$((MOUNT_BACK_WAIT + 1))
  sleep 1
done
get_shares restored >/dev/null
SEL_R="[s for s in d[\"data\"][\"shares\"] if s[\"id\"]==\"$SHARE_RW_ID\"][0]"
expect_eq "卷挂回来之后仍然是撤销状态（不自动恢复）" "True" "$(sget restored "bool($SEL_R.get(\"runtime_revoked\"))")"
expect_eq "卷挂回来之后导出表里依然没有它（撤销只减不增）" "False" "$(exports_table_has "$MP")"

SHARE_VER=$(current_share_version "$SHARE_RW_ID" reap)
run_action share.apply "{\"share_id\":\"$SHARE_RW_ID\",\"version\":\"$SHARE_VER\",\"definition\":$DEF_RW}" reapply-rw
expect_eq "重新应用任务终态" "succeeded" "$JOB_STATE"
expect_eq "重新应用结果 verified" "verified" "$(joutcome)"
expect_eq "重新应用之后撤销记录被清掉" "True" \
  "$(get_shares serving >/dev/null; sget serving "not bool([s for s in d[\"data\"][\"shares\"] if s[\"id\"]==\"$SHARE_RW_ID\"][0].get(\"runtime_revoked\"))")"
expect_eq "独立证据：实际导出表里又有它了" "True" "$(exports_table_has "$MP")"
MOUNT_RC=$(mount_nfs "$MP" "$MOUNT_RW" "")
expect_eq "重新应用之后客户端又能挂上" "0" "$MOUNT_RC"
expect_eq "核心：findmnt 确认真的挂上了" "True" "$(is_mounted "$MOUNT_RW")"
expect_eq "客户端读得到之前写进去的文件" "$PROBE_SHA" \
  "$(sha256sum "$MOUNT_RW/probe-write.txt" 2>/dev/null | cut -d' ' -f1)"

# v4 的尝试只记录，不算通过项（面板不假设 v4）。
install -d -m 0755 /mnt/nfs-v4
if mount -t nfs -o vers=4 "127.0.0.1:$MP" /mnt/nfs-v4 2>"$FACTS/mount-v4.err"; then
  printf 'vers=4 挂载成功\n' >"$FACTS/mount-v4.txt"
else
  printf 'vers=4 挂载失败：%s\n' "$(cat "$FACTS/mount-v4.err" 2>/dev/null)" >"$FACTS/mount-v4.txt"
fi
if mountpoint -q /mnt/nfs-v4 2>/dev/null; then umount -l /mnt/nfs-v4 2>/dev/null || true; fi
note "NFSv4 的尝试结果记在 $FACTS/mount-v4.txt（不作为通过项）"

# ---------------------------------------------------------------------------
echo
echo "== 10. 撤干净：导出行没了，卷里的文件一个不少 =="
# ---------------------------------------------------------------------------

FILES_BEFORE=$(find "$MP" -type f | wc -l | tr -d ' ')
SHA_BEFORE=$(sha256sum "$MP/probe-write.txt" 2>/dev/null | cut -d' ' -f1)

unmount_clients
expect_eq "客户端挂载都卸掉了" "False" "$(is_mounted "$MOUNT_RW")"

SHARE_VER=$(current_share_version "$SHARE_RO_ID" dis)
run_action share.disable "{\"share_id\":\"$SHARE_RO_ID\",\"version\":\"$SHARE_VER\"}" disable-ro
expect_eq "停用只读导出的任务终态" "succeeded" "$JOB_STATE"
expect_eq "独立证据：文件里不再有 ro 那一行" "False" "$(exports_file_has "$MP/$REL_RO")"
expect_eq "独立证据：实际导出表里也没有了" "False" "$(exports_table_has "$MP/$REL_RO")"
expect_eq "独立证据：目录与文件还在" "True" "$(test -f "$MP/$REL_RO/readme.txt" && echo True || echo False)"

SHARE_VER=$(current_share_version "$SHARE_RW_ID" del)
run_action share.delete "{\"share_id\":\"$SHARE_RW_ID\",\"version\":\"$SHARE_VER\"}" delete-rw
expect_eq "删除读写导出的任务终态" "succeeded" "$JOB_STATE"
expect_eq "独立证据：实际导出表里一条面板的导出都不剩" "False" "$(exports_table_has "$MP")"
expect_eq "独立证据：文件里一条 upanel 行都不剩" "0" "$(managed_line_count)"
expect_eq "独立证据：卷里的文件数与删除前一致" "$FILES_BEFORE" "$(find "$MP" -type f | wc -l | tr -d ' ')"
expect_eq "独立证据：内容哈希没变" "$SHA_BEFORE" "$(sha256sum "$MP/probe-write.txt" 2>/dev/null | cut -d' ' -f1)"
expect_eq "独立证据：没人动的 /etc/exports 哈希没变" "$MAIN_SHA_BEFORE" "$(main_exports_sha)"
# 停用**不删记录**（契约 §7(6)：停用/删除只摘导出行，目录与记录都留着）——所以收尾时
# 列表里应当**只剩那条已停用的只读共享**，没有一条是启用的、没有一条还声称配置已应用。
# 第一轮写的是"列表回到空数组"，等于要求 disable 顺手删记录，与契约相反。
get_shares final >/dev/null
expect_eq "收尾只剩一条共享" "1" "$(sget final 'len(d["data"]["shares"])')"
expect_eq "收尾那一条是 media-ro、已停用、没在配置里" "True" \
  "$(sget final 'all(s["definition"]["name"]=="media-ro" and not s["definition"]["enabled"] and not s["config_applied"] for s in d["data"]["shares"])')"

# ---------------------------------------------------------------------------
echo
echo "== 11. 结论 =="
# ---------------------------------------------------------------------------

notrun "从另一台机器挂进来" "VM 里只有本机；网段限制用'改名单'在本机验过，跨机器链路没跑过"
notrun "Windows / macOS 的 NFS 客户端" "没有这两种测试机；地址形态由 web 侧断言（web/src/api/shares.test.ts）"
notrun "NFSv4 直挂" "面板不假设 v4（fsid=0 假根会挡）；脚本只跑 vers=3,proto=tcp，v4 结果只记录"
notrun "两条导出并发写托管文件" "锁行为由 internal/shares/exports_test.go 覆盖；真机并发会把冲突变成时序问题"
notrun "kerberos（sec=krb5*）" "这一版固定 sec=sys，没有这一层"
notrun "设备级掉线（拔盘 / I\\O 错误之后 statfs 报错）" \
  "这台 VM 是 -monitor none 起的，跑的时候没法把 sdb 拔掉；验过的是“挂载点从命名空间里消失之后面板做什么”（lazy umount）"

printf 'PASS=%s FAIL=%s NOT RUN=%s\n' "$PASSED" "$FAILED" "$NTRUN"
write_result
if [ "$FAILED" -ne 0 ]; then
  printf '失败项：%s\n' "$FAILED_NAMES"
  exit 1
fi
exit 0
