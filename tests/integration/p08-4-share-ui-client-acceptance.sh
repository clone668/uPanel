#!/bin/sh
# P08.3 之后是 P08.4：共享 UI、用户权限、客户端连接说明的 Linux 验收。
#
# 与前两份的分工：
#   p08-1 证明共享被定义、目录与 ACL 备好，但**没有**对外提供服务；
#   p08-2 证明配置真的写进托管文件、Samba 真的在用、四种客户端身份被正确区分；
#   这一份证明**页面读到的东西**与**页面教人的做法**都成立：
#
#   1) **三个准备状态分开**：config_hash / directory_prepared / config_applied 各答一个
#      问题，载荷里三个字段都在，页面与契约对得上（CONTRACTS §7.1）。
#   2) **页面说明里的命令真的能跑**：客户端说明给的 smbclient 与 mount.cifs 命令，
#      本脚本逐条、原样执行（只填占位符），写完能读回、卸载后挂载点消失。
#      这几行字符串与 web/src/api/shares.test.ts 里钉住的是同一批文本，改一边会红。
#   3) **权限矩阵照说明说的那样**：写用户能写、读用户能读不能写、名单外的账号被拒、
#      匿名被拒——三种拒绝都必须是真的 NT_STATUS_*，不是我们的想象。
#   4) **密码不出现在任何它不该出现的地方**：命令行（/proc/*/cmdline）、日志、
#      面板与代理的数据库、/proc/mounts。凭据文件必须 0600。
#   5) **客户端说明不撒谎**：卷掉线时那条共享在载荷里是 volume_offline、
#      clientHintsUsable 为假（由 web 侧回放断言），脚本这边核对现场字段。
#
# 用法：
#   sudo BIN_DIR=/var/tmp/upanel-bin SPARE_DISKS="sdb" \
#        sh tests/integration/p08-4-share-ui-client-acceptance.sh
#
# 环境变量：PORT（默认 19478）、WORK、BIN_DIR、SPARE_DISKS（一块一次性空盘，会被格式化）。
#
# 前置夹具（不属于被测功能，缺了就在第 0 节停下，不把缺包记成产品缺陷）：
#   samba、samba-common-bin（smbclient/smbstatus/smbcontrol/testparm/pdbedit）、
#   acl、cifs-utils（mount.cifs）。面板**不会**替任何人装包（PLAN.md 第 10 节）。
#
# 退出码：0 全部通过；1 有失败项；2 环境不满足。
#
# 明确不验（NOT RUN，原因写在这里，不在脚本里假装通过）：
#   - **Windows 与 macOS 客户端**：这台机器上没有这两种系统，也没有 P00-VM-D 客户端 VM。
#     说明里给它们的地址（`\\主机\名字` 与 `smb://主机/名字`）由 web 侧断言形状，
#     真实资源管理器/Finder 的连接没有跑过。
#   - **从另一台物理机器连进来**：本机客户端与 smbd 同机，跨机器还牵涉防火墙。
#   - **浏览器里的真实渲染**：脚本验的是页面读到的载荷；DOM 由 vitest 的 SSR 回放覆盖。

set -u

BIN_DIR=${BIN_DIR:-/var/tmp/upanel-bin}

if [ "$(id -u)" -ne 0 ]; then
  printf '本脚本必须以 root 运行：sudo BIN_DIR=%s sh %s\n' "$BIN_DIR" "$0" >&2
  exit 2
fi

AGENT="$BIN_DIR/upanel-agent"
WEB="$BIN_DIR/upanel-web"
CTL="$BIN_DIR/upanelctl"

SPARE_DISKS=${SPARE_DISKS:-}
WORK=${WORK:-/var/tmp/upanel-accept-p08-4}
PAYLOADS=$WORK/payloads
SECRETS_DIR=$WORK/secrets
RUN_DIR=/run/upanel
SOCK=$RUN_DIR/agent.sock
SYS_CFG=/etc/upanel/upanel.json
AGENT_DB_DIR=/var/lib/upanel-agent
AGENT_DB=$AGENT_DB_DIR/agent.db
KEY_FILE=/etc/upanel/agent.key
WEB_DB_DIR=$WORK/webdata
WEB_DB=$WEB_DB_DIR/upanel.db
PORT=${PORT:-19478}
BASE="http://127.0.0.1:$PORT"
ORIGIN="$BASE"
WEB_USER=upanel-web
PASSWORD='vm-correct-horse-9'

STORE=/api/v1/storage
SHARES_PATH=/api/v1/shares
ACCOUNTS_PATH=/api/v1/share-accounts
SECRETS_PATH=/api/v1/secrets
PREFLIGHT_PATH=/api/v1/operations/preflight
JOBS=/api/v1/jobs
AGENT_LOG=$WORK/logs/agent.log
WEB_LOG=$WORK/logs/web.log
AGENT_PID=""
WEB_PID=""
SMB_CONF_ARCHIVE=""

VOLUME_ROOT=/srv/upanel/volumes
FSTAB=/etc/fstab
SMB_CONF=/etc/samba/smb.conf
SMB_INCLUDE_DIR=/etc/samba/upanel.d
MANAGED_CONF=$SMB_INCLUDE_DIR/upanel.conf
BACKUP_DIR=/var/lib/upanel-agent/smb-backups
PART_MAIN=$((2 * 1073741824))
PART_START=1048576
SLUG=data
LABEL=p08vol

# 共享名就叫 data：与 web 侧钉住的那几条命令文本逐字一致（//127.0.0.1/data、
# /root/.smb/data.cred、/mnt/data）。改名会让那份“互相钉住”的断言失去意义。
SHARE_NAME=data
# 空串表示“整个卷根”：ValidateRelativePath 明确允许，且这里的挂载测试因此更干净。
REL_PATH=
ACCT_RW=smbp08rw
ACCT_RO=smbp08ro
ACCT_X=smbp08un
ACCT_RW_PW='vm-share-rw-horse-2'
ACCT_RO_PW='vm-share-ro-horse-3'
ACCT_X_PW='vm-share-none-horse-7'

MOUNT_MAIN=/mnt/data
MOUNT_RO=/mnt/data-ro
CRED_MAIN=/root/.smb/data.cred
CRED_RO=/root/.smb/data-ro.cred

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

unmount_cleanup() {
  for mp in "$MOUNT_MAIN" "$MOUNT_RO"; do
    if mountpoint -q "$mp" 2>/dev/null; then umount -l "$mp" 2>/dev/null || true; fi
  done
}
cleanup() {
  unmount_cleanup
  if [ "${SMBD_STOPPED:-0}" = "1" ]; then systemctl start smbd >/dev/null 2>&1 || true; fi
  if [ -n "${SMB_CONF_ARCHIVE:-}" ] && [ -f "$SMB_CONF_ARCHIVE" ]; then
    cp "$SMB_CONF_ARCHIVE" "$SMB_CONF" 2>/dev/null || true
    smbcontrol all reload-config >/dev/null 2>&1 || true
  fi
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

reauth() { api "$JAR" "$CSRF" POST /api/v1/auth/reauth "{\"password\":\"$PASSWORD\"}"; }

put_secret() {
  python3 -c 'import json,sys
body={"purpose":sys.argv[1],"value":open(sys.argv[2], encoding="utf-8").read()}
if len(sys.argv)>3 and sys.argv[3]:
    body["reference"]=sys.argv[3]
print(json.dumps(body))' "$1" "$2" "${3:-}" >"$SECRETS_DIR/req.json"
  chmod 600 "$SECRETS_DIR/req.json"
  _code=$(curl -s -b "$JAR" -o "$WORK/secret.json" -w '%{http_code}' -X POST "$BASE$SECRETS_PATH" \
    -H 'Content-Type: application/json' -H "Origin: $ORIGIN" -H "X-CSRF-Token: $CSRF" \
    -d @"$SECRETS_DIR/req.json")
  shred -u "$SECRETS_DIR/req.json" 2>/dev/null || rm -f "$SECRETS_DIR/req.json"
  printf '%s' "$_code"
}

preflight() {
  printf '{"action":"%s","params":%s}' "$1" "$2" >"$WORK/pre.json"
  _code=$(curl -s -b "$JAR" -o "$WORK/resp.json" -w '%{http_code}' \
    -X POST "$BASE$PREFLIGHT_PATH" \
    -H 'Content-Type: application/json' -H "Origin: $ORIGIN" -H "X-CSRF-Token: $CSRF" \
    -d @"$WORK/pre.json")
  cp "$WORK/resp.json" "$WORK/preflight.json"
  printf '%s' "$_code"
}

submit_job() {
  python3 -c 'import json,sys
d={"preflight_id":sys.argv[1]}
if sys.argv[2]:
    d["confirmation_phrase"]=sys.argv[2]
print(json.dumps(d))' "$1" "${2:-}" >"$WORK/jobreq-submit.json"
  _code=$(curl -s -b "$JAR" -o "$WORK/resp.json" -w '%{http_code}' -X POST "$BASE$JOBS" \
    -H 'Content-Type: application/json' -H "Origin: $ORIGIN" -H "X-CSRF-Token: $CSRF" \
    -H "Idempotency-Key: $3" -d @"$WORK/jobreq-submit.json")
  cp "$WORK/resp.json" "$WORK/submit.json"
  printf '%s' "$_code"
}

wait_job() {
  _i=0
  while [ "$_i" -lt 300 ]; do
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

# 幂等键必须每次运行都不同：Web 侧的幂等记录是持久的，同一个键第二次提交会被判成
# IDEMPOTENCY_CONFLICT（作业根本没被创建）。键里带时间戳与 pid，run 内仍然稳定。
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

joutcome() { jobget 'd["data"]["result"]["outcome"]'; }
jobobserved() { jobget "d[\"data\"][\"result\"][\"observed_state\"][\"$1\"]"; }
jobdata() { jobget "d[\"data\"][\"result\"][\"data\"].get(\"$1\")"; }
pall() { pget '",".join(b["code"] for b in d["data"]["blockers"])'; }
pwarn() { pget '",".join(w["code"] for w in (d["data"].get("warnings") or []))'; }

# ---------- 页面读的接口 ----------

get_shares() {
  _code=$(api "$JAR" "$CSRF" GET "$SHARES_PATH")
  cp "$WORK/resp.json" "$WORK/shares-$1.json"
  printf '%s' "$_code"
}
sget() { jget "$WORK/shares-$1.json" "$2"; }

get_accounts() {
  _code=$(api "$JAR" "$CSRF" GET "$ACCOUNTS_PATH")
  cp "$WORK/resp.json" "$WORK/accounts-$1.json"
  printf '%s' "$_code"
}
aget() { jget "$WORK/accounts-$1.json" "$2"; }

get_share() {
  _code=$(api "$JAR" "$CSRF" GET "$SHARES_PATH/$1")
  cp "$WORK/resp.json" "$WORK/share-$2.json"
  printf '%s' "$_code"
}
shget() { jget "$WORK/share-$1.json" "$2"; }

get_connections() {
  _code=$(api "$JAR" "$CSRF" GET "$SHARES_PATH/$1/connections")
  cp "$WORK/resp.json" "$WORK/connections-$2.json"
  printf '%s' "$_code"
}
cget() { jget "$WORK/connections-$1.json" "$2"; }

volumes() {
  _code=$(api "$JAR" "$CSRF" GET "$STORE/volumes")
  cp "$WORK/resp.json" "$WORK/volumes-$1.json"
  printf '%s' "$_code"
}
volsget() { jget "$WORK/volumes-$1.json" "$2"; }

volume() {
  _code=$(api "$JAR" "$CSRF" GET "$STORE/volumes/$1")
  cp "$WORK/resp.json" "$WORK/volume-$2.json"
  printf '%s' "$_code"
}
volget() { jget "$WORK/volume-$1.json" "$2"; }
current_vol_version() { volume "$1" "$2" >/dev/null; volget "$2" 'd["data"]["version"]'; }

# ---------- 独立证据 ----------

blkid_field() { blkid -p -o value -s "$2" "$1" 2>/dev/null | head -1; }
is_mounted() {
  if [ -n "$(findmnt -rno TARGET --mountpoint "$1" 2>/dev/null)" ]; then echo True; else echo False; fi
}
managed_sha() { sha256sum "$MANAGED_CONF" 2>/dev/null | cut -d' ' -f1; }
smb_section_exists() {
  if testparm -s --suppress-prompt --section-name="$1" --parameter-name="path" "$SMB_CONF" \
    >/dev/null 2>&1; then echo True; else echo False; fi
}
smb_effective() {
  testparm -s --suppress-prompt --section-name="$1" --parameter-name="$2" \
    "$SMB_CONF" 2>/dev/null | tr -d '\r' | sed '/^$/d' | tr '\n' ' ' | sed 's/ $//'
}
smb_service_state() {
  systemctl is-active smbd.service smbd.socket 2>/dev/null | tr -d '\r' | awk '
    $0=="active" || $0=="reloading" { a=1; k=1; next }
    $0=="inactive" || $0=="failed" || $0=="activating" || $0=="deactivating" ||
    $0=="unknown" || $0=="maintenance" { k=1 }
    END { if (a) print "active"; else if (k) print "inactive"; else print "unknown" }'
}

# 明文扫描：模式从文件读（grep -f），检查者自己的 argv 里不出现密码。
hits_in_file() {
  if [ -f "$1" ]; then grep -a -c -F -f "$PW_FILE" "$1" 2>/dev/null || true; else printf '0'; fi
}
hits_in_databases() {
  _n=0
  for f in "$AGENT_DB" "$AGENT_DB-wal" "$AGENT_DB-shm" "$WEB_DB" "$WEB_DB-wal" "$WEB_DB-shm"; do
    [ -f "$f" ] || continue
    _n=$((_n + $(grep -a -c -F -f "$PW_FILE" "$f" 2>/dev/null || true)))
  done
  printf '%s' "$_n"
}
# 进程表 + 环境块的瞬时快照。采样不是证明，只说明“这一刻没看到”；
# 真正的结构性保证是命令里根本没有密码（见 §4 的静态断言与 web 侧的字符串钉住）。
hits_in_processes() {
  for d in /proc/[0-9]*; do
    cat "$d/cmdline" 2>/dev/null | tr '\0' ' '
    printf '\n'
    cat "$d/environ" 2>/dev/null | tr '\0' ' '
    printf '\n'
  done | grep -a -c -F -f "$PW_FILE" 2>/dev/null || true
}

# ---------- 客户端（说明里教的那几种） ----------

smb_authfile() { # <账号> <密码变量名> -> 认证文件路径
  _f=$SECRETS_DIR/auth-$1
  eval "_pw=\${$2}"
  printf 'username=%s\npassword=%s\ndomain=\n' "$1" "$_pw" >"$_f"
  chmod 0600 "$_f"
  printf '%s' "$_f"
}
smb_ls() { smbclient "//127.0.0.1/$1" -A "$2" -m SMB3 -c 'ls' 2>&1; }
smb_put() { smbclient "//127.0.0.1/$1" -A "$2" -m SMB3 -c "put $3 $4" 2>&1; }
smb_get() { smbclient "//127.0.0.1/$1" -A "$2" -m SMB3 -c "get $3 $4" >/dev/null 2>&1; cat "$4" 2>/dev/null; }
smb_ls_guest() { smbclient "//127.0.0.1/$1" -N -m SMB3 -c 'ls' 2>&1; }

echo "== 0. 环境与安全守卫 =="
for b in "$AGENT" "$WEB" "$CTL"; do
  if [ -x "$b" ]; then pass "二进制存在且可执行 $(basename "$b")"; else fail "二进制存在且可执行 $(basename "$b")" "$b"; fi
done
for t in python3 curl runuser sha256sum findmnt lsblk sgdisk blkid wipefs udevadm \
         blockdev stat awk diff grep systemctl mountpoint; do
  if command -v "$t" >/dev/null 2>&1; then pass "依赖命令可用 $t"; else fail "依赖命令可用 $t" "缺少 $t"; fi
done

MISSING_PKGS=""
for t in /usr/sbin/smbd /usr/bin/smbpasswd /usr/bin/pdbedit /usr/bin/testparm \
         /usr/bin/smbcontrol /usr/bin/smbstatus /usr/bin/smbclient \
         /usr/bin/setfacl /usr/bin/getfacl /usr/sbin/mount.cifs; do
  if [ -x "$t" ]; then pass "夹具：$t 存在"; else MISSING_PKGS="$MISSING_PKGS $t"; fi
done
if [ -f "$SMB_CONF" ]; then pass "夹具：$SMB_CONF 存在"; else MISSING_PKGS="$MISSING_PKGS $SMB_CONF"; fi
if [ -n "$MISSING_PKGS" ]; then
  printf '这台机器缺少夹具：%s\n' "$MISSING_PKGS" >&2
  printf '先装好（apt-get install -y samba samba-common-bin acl cifs-utils）再来跑验收。\n' >&2
  exit 2
fi
note "smbd 版本（独立读取）：$(/usr/sbin/smbd --version 2>&1 | head -1)"
note "mount.cifs 版本：$(/usr/sbin/mount.cifs -V 2>&1 | head -1)"

if ! id -u "$WEB_USER" >/dev/null 2>&1; then
  useradd --system --no-create-home --shell /usr/sbin/nologin "$WEB_USER" 2>/dev/null
fi
if id -u "$WEB_USER" >/dev/null 2>&1; then pass "Web 账号 $WEB_USER 存在"; else fail "Web 账号 $WEB_USER 存在" "无法创建"; fi
if ! getent group "$WEB_USER" >/dev/null 2>&1; then groupadd --system "$WEB_USER" 2>/dev/null; fi
if curl -fsS "$BASE/api/v1/health/live" >/dev/null 2>&1; then
  echo "$PORT 上已有服务在监听；先停掉遗留的 upanel-web 再跑验收" >&2
  exit 1
fi

BLANK_A=$(printf '%s\n' $SPARE_DISKS | sed -n 1p)
if [ -z "$BLANK_A" ]; then
  echo "SPARE_DISKS 需要一块一次性空盘（当前 '$SPARE_DISKS'）" >&2
  exit 2
fi
ROOT_SRC=$(findmnt -no SOURCE / 2>/dev/null)
ROOT_REAL=$(readlink -f "$ROOT_SRC" 2>/dev/null)
if [ -z "$ROOT_REAL" ] || [ ! -b "$ROOT_REAL" ]; then
  echo "无法确定承载 / 的块设备（findmnt 给出 '$ROOT_SRC'）；拒绝在这个环境里跑磁盘验收" >&2
  exit 2
fi
ROOT_DISK=$(lsblk -no PKNAME "$ROOT_REAL" 2>/dev/null | head -1)
if [ -z "$ROOT_DISK" ]; then
  if [ "$(lsblk -no TYPE "$ROOT_REAL" 2>/dev/null | head -1)" = "disk" ]; then
    ROOT_DISK=$(basename "$ROOT_REAL")
  else
    ROOT_DISK=$(lsblk -sno NAME "$ROOT_REAL" 2>/dev/null | tail -1)
  fi
fi
if [ -z "$ROOT_DISK" ]; then
  echo "无法确定承载 / 的整盘；拒绝运行（不能盲目擦盘）" >&2
  exit 2
fi
# 上一轮留下的受管理卷挂载点会落在那块一次性盘上，先卸掉，否则下面的“一次性盘不能有
# 挂载点”守卫会把我们自己留下的东西当成危险物。
for mp in "$VOLUME_ROOT"/*; do
  [ -d "$mp" ] || continue
  umount "$mp" 2>/dev/null || true
done
pass "承载 / 的设备是 $ROOT_REAL（整盘 $ROOT_DISK）"
if [ -e "/dev/$BLANK_A" ]; then pass "一次性盘 /dev/$BLANK_A 存在"; else echo "/dev/$BLANK_A 不存在" >&2; exit 2; fi
if [ "$BLANK_A" = "$ROOT_DISK" ]; then echo "/dev/$BLANK_A 就是承载 / 的整盘，拒绝使用" >&2; exit 2; fi
if [ "$(lsblk -no TYPE "/dev/$BLANK_A" 2>/dev/null | head -1)" != "disk" ]; then
  echo "/dev/$BLANK_A 不是整盘（lsblk type != disk），拒绝使用" >&2
  exit 2
fi
if lsblk -no MOUNTPOINT "/dev/$BLANK_A" 2>/dev/null | grep -q '[^ ]'; then
  echo "/dev/$BLANK_A 上有挂载点，拒绝使用" >&2
  exit 2
fi
_size=$(blockdev --getsize64 "/dev/$BLANK_A" 2>/dev/null || echo 0)
if [ "$_size" -lt $((PART_MAIN + 64 * 1048576)) ]; then
  echo "/dev/$BLANK_A 容量 ${_size} 字节，放不下夹具分区" >&2
  exit 2
fi

unmount_cleanup
if grep -qx '# BEGIN upanel managed volumes' "$FSTAB" 2>/dev/null; then
  FSTAB_ARCHIVE=$WORK.fstab-before-reset.txt
  mkdir -p "$WORK/logs"
  cp "$FSTAB" "$FSTAB_ARCHIVE" 2>/dev/null || true
  note "清掉遗留的托管块（内容留档到 $FSTAB_ARCHIVE）"
  python3 - <<'PY'
p = '/etc/fstab'
lines = open(p, encoding='utf-8').read().splitlines(True)
out, inside = [], False
for l in lines:
    if l.strip() == '# BEGIN upanel managed volumes':
        inside = True
        continue
    if l.strip() == '# END upanel managed volumes':
        inside = False
        continue
    if not inside:
        out.append(l)
open(p, 'w', encoding='utf-8').write(''.join(out))
PY
fi

rm -rf "$WORK"
mkdir -p "$WORK/logs" "$PAYLOADS" "$SECRETS_DIR" "$WEB_DB_DIR"
chmod 0755 "$WORK" "$PAYLOADS" "$WORK/logs"
chmod 0700 "$SECRETS_DIR"
chown -R "$WEB_USER" "$WEB_DB_DIR"
if [ -f "${FSTAB_ARCHIVE:-}" ]; then
  cp "$FSTAB_ARCHIVE" "$WORK/logs/fstab-before-reset.txt" 2>/dev/null || true
fi

cp "$SMB_CONF" "$WORK/logs/smb-conf-initial.txt"
chmod 0644 "$WORK/logs/smb-conf-initial.txt"
SMB_CONF_ARCHIVE=$WORK/logs/smb-conf-initial.txt

if [ -d "$SMB_INCLUDE_DIR" ]; then
  rm -rf "$SMB_INCLUDE_DIR"
  note "清掉上一轮遗留的 $SMB_INCLUDE_DIR"
fi
# 上一次跑完之后 smb.conf 末尾那行 include 还留着（代理只在第一次追加，之后不动它，
# 这是对的行为）。本轮要从“干净的起点”开始，所以把那一行摘掉——摘之前 smb.conf 已经
# 整份留档在 logs/smb-conf-initial.txt，收尾时原样放回去。
if grep -qF "/etc/samba/upanel.d/" "$SMB_CONF" 2>/dev/null; then
  python3 - "$SMB_CONF" <<'PY'
import sys
p = sys.argv[1]
kept = [l for l in open(p, encoding='utf-8').read().splitlines(True)
        if '/etc/samba/upanel.d/' not in l]
open(p, 'w', encoding='utf-8').write(''.join(kept))
PY
  note "摘掉 smb.conf 里残留的 include 行（前一份留档在 logs/smb-conf-initial.txt）"
fi
if grep -qF "/etc/samba/upanel.d/" "$SMB_CONF" 2>/dev/null; then
  fail "起点：smb.conf 里没有残留的 include 行" "仍有指向 $SMB_INCLUDE_DIR 的行"
else
  pass "起点：smb.conf 里没有残留的 include 行"
fi
rm -rf "$BACKUP_DIR"

lsblk -o NAME,TYPE,SIZE,FSTYPE,MOUNTPOINTS >"$WORK/logs/lsblk-initial.txt" 2>&1
rm -rf "$VOLUME_ROOT"/* 2>/dev/null
install -d -m 0755 "$VOLUME_ROOT"
wipefs -a "/dev/$BLANK_A" >/dev/null 2>&1
sgdisk --zap-all "/dev/$BLANK_A" >/dev/null 2>&1
blockdev --rereadpt "/dev/$BLANK_A" >/dev/null 2>&1
udevadm settle --timeout=5 >/dev/null 2>&1
pass "一次性盘 /dev/$BLANK_A 清成空白"

for n in "$ACCT_RW" "$ACCT_RO" "$ACCT_X"; do
  pdbedit -x -u "$n" >/dev/null 2>&1 || true
  userdel "$n" >/dev/null 2>&1 || true
done
if getent passwd "$ACCT_RW" >/dev/null 2>&1 || pdbedit -L 2>/dev/null | grep -qE "^($ACCT_RW|$ACCT_RO|$ACCT_X):"; then
  fail "起点：受管理账号已清空" "系统里还有 smbp08* 账号残留"
else
  pass "起点：受管理账号已清空"
fi

if [ "$(smb_service_state)" != "active" ]; then
  systemctl start smbd >/dev/null 2>&1 || true
  sleep 1
fi
expect_eq "夹具：smbd 在运行" "active" "$(smb_service_state)"

# 起点：挂载点没有残留（上一轮用 -l 卸过也要确认真的没了）
for mp in "$MOUNT_MAIN" "$MOUNT_RO"; do
  expect_eq "起点：$mp 没有挂载" "False" "$(is_mounted "$mp")"
done
rm -rf /root/.smb

echo
echo "== 1. 配置与启动（代理 root、Web 非 root） =="
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

JAR=$WORK/admin.jar
expect_eq "管理员登录" "200" "$(login admin "$PASSWORD" "$JAR" login-admin)"
CSRF=$(csrf_of "$WORK/login-admin.json")
expect_ne "拿到 CSRF 令牌" "" "$CSRF"

PW_FILE=$SECRETS_DIR/passwords.txt
umask 077
printf '%s\n%s\n%s\n%s\n' "$ACCT_RW_PW" "$ACCT_RO_PW" "$ACCT_X_PW" "$PASSWORD" >"$PW_FILE"
chmod 0600 "$PW_FILE"

# 构建进二进制的这一份前端里必须有共享页：这是“页面真的发到了浏览器手上”的机械证据。
# 面板本身不读这个文件，它只是被 go:embed 打进 upanel-web。
if grep -aq "客户端怎么连" "$WEB"; then
  pass "Web 二进制里嵌入了共享页（含“客户端怎么连”这段文案）"
else
  fail "Web 二进制里嵌入了共享页" "在 $WEB 里搜不到共享页的文案，前端产物可能没同步进 embed"
fi
expect_eq "页面在 /shares 上有入口（SPA 会返回 index.html）" "200" \
  "$(curl -s -o /dev/null -w '%{http_code}' "$BASE/shares")"

echo
echo "== 2. 夹具：卷 / 三个账号 / 一条共享 =="
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
  "{\"device_id\":\"${BLANK_A}1\",\"slug\":\"$SLUG\",\"display_name\":\"数据卷\"}" import-a
expect_eq "导入任务终态" "succeeded" "$JOB_STATE"
VOL_ID=$(jobdata volume_id)
MP=$VOLUME_ROOT/$SLUG
VOL_VER=$(current_vol_version "$VOL_ID" m1)
run_action storage.volume.mount "{\"volume_id\":\"$VOL_ID\",\"version\":\"$VOL_VER\"}" mount-a
expect_eq "挂载任务终态" "succeeded" "$JOB_STATE"
expect_eq "独立证据：卷真的挂上了" "True" "$(is_mounted "$MP")"

for pair in "$ACCT_RW:$ACCT_RW_PW" "$ACCT_RO:$ACCT_RO_PW" "$ACCT_X:$ACCT_X_PW"; do
  _name=${pair%%:*}
  _pw=${pair#*:}
  printf '%s' "$_pw" >"$SECRETS_DIR/$_name.pw"
  chmod 0600 "$SECRETS_DIR/$_name.pw"
  _sec_code=$(put_secret smb_password "$SECRETS_DIR/$_name.pw" "accept-p08-4-$_name")
  cp "$WORK/secret.json" "$PAYLOADS/secret-$_name.json"
  expect_eq "存 $_name 的密码走秘密通道" "201" "$_sec_code"
  _ref=$(jget "$WORK/secret.json" 'd["data"]["secret_ref"]')
  case "$_ref" in sec_*) pass "拿得到 $_name 的 secret_ref（sec_ 前缀）" ;; *) fail "拿得到 $_name 的 secret_ref" "$_ref" ;; esac
  run_action share.account.apply \
    "{\"account_id\":null,\"name\":\"$_name\",\"secret_ref\":\"$_ref\",\"disabled\":false}" "acct-$_name"
  expect_eq "建 $_name 任务终态" "succeeded" "$JOB_STATE"
  expect_eq "建 $_name 结果 verified" "verified" "$(joutcome)"
done
ACCT_ID_RW=$(jget "$PAYLOADS/job-acct-$ACCT_RW.json" 'd["data"]["result"]["data"]["account_id"]')
ACCT_ID_RO=$(jget "$PAYLOADS/job-acct-$ACCT_RO.json" 'd["data"]["result"]["data"]["account_id"]')
ACCT_ID_X=$(jget "$PAYLOADS/job-acct-$ACCT_X.json" 'd["data"]["result"]["data"]["account_id"]')
expect_ne "拿到写账号的 id" "" "$ACCT_ID_RW"
expect_ne "拿到读账号的 id" "" "$ACCT_ID_RO"
expect_ne "拿到未授权账号的 id" "" "$ACCT_ID_X"
expect_eq "独立证据：三个账号都在 passdb 里" "3" \
  "$(pdbedit -L 2>/dev/null | cut -d: -f1 | grep -cE "^($ACCT_RW|$ACCT_RO|$ACCT_X)$" | tr -d ' ')"

# 名单里只有 rw 与 ro；un 在 passdb 里但不在名单里——这正是“未授权被拒”的现场。
DEF_MAIN=$(printf '{"name":"%s","protocol":"smb","volume_id":"%s","relative_path":"%s","enabled":true,"read_only":false,"allowed_cidrs":["127.0.0.0/8"],"smb_users":[{"account_id":"%s","access":"write"},{"account_id":"%s","access":"read"}]}' \
  "$SHARE_NAME" "$VOL_ID" "$REL_PATH" "$ACCT_ID_RW" "$ACCT_ID_RO")
run_action share.apply "{\"share_id\":null,\"version\":null,\"definition\":$DEF_MAIN}" apply-main
expect_eq "应用共享预检允许" "True" "$PRE_ALLOWED"
expect_eq "应用共享任务终态" "succeeded" "$JOB_STATE"
expect_eq "应用共享结果 verified" "verified" "$(joutcome)"
SHARE_ID=$(jobdata share_id)
SHARE_VER=$(jobdata version)
expect_ne "拿到共享标识" "" "$SHARE_ID"
expect_eq "独立证据：Samba 读回 valid users 含两个账号" "True" \
  "$(python3 -c 'import sys
v=set(sys.argv[1].replace(","," ").split())
print(v == {sys.argv[2], sys.argv[3]})' "$(smb_effective "$SHARE_NAME" "valid users")" "$ACCT_RW" "$ACCT_RO")"
expect_eq "独立证据：Samba 读回的 path 就是卷根" "$MP" "$(smb_effective "$SHARE_NAME" path)"
expect_eq "独立证据：Samba 读回的 guest ok" "No" "$(smb_effective "$SHARE_NAME" "guest ok")"

echo
echo "== 3. 三个准备状态分开，且与现场一致 =="
expect_eq "共享列表可读" "200" "$(get_shares main)"
cp "$WORK/shares-main.json" "$PAYLOADS/page-shares-main.json"
SEL="[s for s in d[\"data\"][\"shares\"] if s[\"id\"]==\"$SHARE_ID\"][0]"
expect_eq "载荷里有这一条" "1" "$(sget main "len([s for s in d[\"data\"][\"shares\"] if s[\"id\"]==\"$SHARE_ID\"])")"
expect_eq "config_hash 是 64 位十六进制（sha256）" "True"   "$(sget main "len($SEL[\"config_hash\"])==64 and all(c in \"0123456789abcdef\" for c in $SEL[\"config_hash\"])")"
expect_eq "directory_prepared 为真" "True" "$(sget main "bool($SEL[\"directory_prepared\"])")"
expect_eq "config_applied 为真" "True" "$(sget main "bool($SEL[\"config_applied\"])")"
# 三个状态分开是契约（CONTRACTS §7.4 (1)）；这里再加一条：记录里的哈希与任务结果里的哈希
# 是同一份渲染，不是“又算了一遍、结果可能不同”。
expect_eq "记录的 config_hash 与任务结果里的 config_hash 一致" "True"   "$(python3 -c 'import json,sys
rec=json.load(open(sys.argv[1], encoding="utf-8"))
job=json.load(open(sys.argv[2], encoding="utf-8"))
sid=sys.argv[3]
share=[x for x in rec["data"]["shares"] if x["id"]==sid][0]
print(share["config_hash"] == job["data"]["result"]["data"]["config_hash"])'     "$WORK/shares-main.json" "$PAYLOADS/job-apply-main.json" "$SHARE_ID")"
expect_eq "没有 runtime_revoked（这一条没被撤销过）" "True" "$(sget main "not $SEL.get(\"runtime_revoked\")")"
# 共享记录里**不该**有卷状态与开机依赖：卷是外部事实，开机依赖是操作结果。
expect_eq "记录里没有 volume_state/volume_name（卷的状态由页面从卷列表 join）" "True"   "$(sget main "not any(k in $SEL for k in (\"volume_state\", \"volume_name\"))")"
expect_eq "记录里没有 startup_deps/startup_paths（那是任务结果里的东西）" "True"   "$(sget main "not any(k in $SEL for k in (\"startup_deps\", \"startup_paths\"))")"
expect_eq "记录的字段全在 openapi ShareRecord 的闭集里" "True"   "$(sget main "set($SEL) <= {\"id\",\"definition\",\"config_hash\",\"version\",\"directory_prepared\",\"config_applied\",\"runtime_revoked\",\"last_operation_id\",\"created_at\",\"updated_at\"}")"
# 页面要显示“卷此刻挂没挂”，依据是卷那边的观测值，不是共享记录。
expect_eq "卷列表可读" "200" "$(volumes main)"
expect_eq "卷的观测状态是 mounted（卡片上的“观测”就取这个）" "mounted"   "$(volsget main "[v for v in d[\"data\"][\"volumes\"] if v[\"id\"]==\"$VOL_ID\"][0][\"observed_state\"]")"
expect_ne "卷有给人看的名字" ""   "$(volsget main "[v for v in d[\"data\"][\"volumes\"] if v[\"id\"]==\"$VOL_ID\"][0][\"display_name\"]")"
# 开机依赖是**这次操作的结果**，落在任务结果里。
case "$(jget "$PAYLOADS/job-apply-main.json" 'd["data"]["result"]["data"].get("startup_deps")')" in
  written|unchanged|removed|failed|skipped_no_service)
    pass "应用任务结果里的 startup_deps 在闭集里" ;;
  *) fail "应用任务结果里的 startup_deps 在闭集里"        "$(jget "$PAYLOADS/job-apply-main.json" 'd["data"]["result"]["data"].get("startup_deps")')" ;;
esac
expect_eq "开机依赖列的就是这块卷的挂载点" "True"   "$(jget "$PAYLOADS/job-apply-main.json" "\"$MP\" in (d[\"data\"][\"result\"][\"data\"].get(\"startup_paths\") or [])")"

echo "== 4. 页面载荷契约：账号里没有密码，连接状态带 service =="
expect_eq "账号列表可读" "200" "$(get_accounts main)"
cp "$WORK/accounts-main.json" "$PAYLOADS/page-accounts-main.json"
expect_eq "三个受管理账号都在" "3" "$(aget main 'len(d["data"]["accounts"])')"
# has_password 是唯一允许带 "passw" 字样的键：它只回答“设过没有”，是布尔。
expect_eq "账号载荷里没有密码/哈希/引用这类值（has_password 除外）" "0" \
  "$(aget main 'len([k for a in d["data"]["accounts"] for k in a if k != "has_password" and any(t in k.lower() for t in ("passw","hash","secret"))])')"
expect_eq "has_password 是布尔且为真（密码确实经秘密通道设进去了）" "True" \
  "$(aget main 'all(a["has_password"] is True for a in d["data"]["accounts"])')"
expect_eq "账号载荷里没有明文字段（与密码文件比对命中 0）" "0" \
  "$(hits_in_file "$WORK/accounts-main.json")"
expect_eq "每个账号都带 managed 与 disabled 两个布尔" "True" \
  "$(aget main 'all(isinstance(a.get("managed"), bool) and isinstance(a.get("disabled"), bool) for a in d["data"]["accounts"])')"
expect_eq "能力报告与列表一起返回（不是两次请求）" "available" \
  "$(sget main 'd["data"]["capability"]["status"]')"
expect_eq "managed_shares 里就是这条共享名" "True" \
  "$(python3 -c 'import json,sys
d=json.load(open(sys.argv[1], encoding="utf-8"))
print(sys.argv[2] in d["data"]["capability"]["managed_shares"])' "$WORK/shares-main.json" "$SHARE_NAME")"

expect_eq "连接状态可读" "200" "$(get_connections "$SHARE_ID" main)"
cp "$WORK/connections-main.json" "$PAYLOADS/page-connections-main.json"
case "$(cget main 'd["data"]["service"]')" in
  active|inactive|unknown) pass "连接状态的 service 在闭集里" ;;
  *) fail "连接状态的 service 在闭集里" "$(cget main 'd["data"]["service"]')" ;;
esac
expect_eq "此刻没人连着（列表是空数组，不是 null）" "True" \
  "$(cget main 'd["data"]["connections"] == []')"
expect_eq "空列表时 service 仍然报得出（不是“不知道”）" "active" "$(cget main 'd["data"]["service"]')"

echo
echo "== 5. 照页面给的命令，逐条执行 =="
# 这一段故意逐字执行 web 侧钉住的那几行（见 web/src/api/shares.test.ts 末尾）。
AUTH_RW=$(smb_authfile "$ACCT_RW" ACCT_RW_PW)
AUTH_RO=$(smb_authfile "$ACCT_RO" ACCT_RO_PW)
AUTH_X=$(smb_authfile "$ACCT_X" ACCT_X_PW)

# —— smbclient：说明里给的是 `-U <账号>` + 交互输密码。脚本不能交互，
#    所以用 stdin 把密码喂给它，argv 里仍然只有账号名。
CLI_CMD="smbclient //127.0.0.1/$SHARE_NAME -U $ACCT_RW -m SMB3 -c ls"
printf '%s\n' "$CLI_CMD" >"$PAYLOADS/client-commands-run.txt"
CLI_OUT=$(printf '%s\n' "$ACCT_RW_PW" | smbclient "//127.0.0.1/$SHARE_NAME" -U "$ACCT_RW" -m SMB3 -c 'ls' 2>&1)
printf '%s\n' "$CLI_OUT" >"$PAYLOADS/client-smbclient-argv.txt"
expect_not_contains "说明里的 smbclient 命令能连上（密码从标准输入给）" "NT_STATUS_" "$CLI_OUT"
expect_contains "smbclient 列出了共享内容" "blocks of size" "$CLI_OUT"

# —— mount.cifs：说明里是一串六步。逐条执行，只把占位符换成真值。
{
  echo 'sudo install -d -m 0700 /root/.smb'
  echo "# 凭据从标准输入写进只有 root 能读的文件：下面是 here-doc，密码不进 argv"
  echo "sudo sh -c 'umask 077; cat > $CRED_MAIN' <<'CRED'"
  echo "username=$ACCT_RW"
  echo "password=<已填>"
  echo 'CRED'
  echo "sudo chmod 0600 $CRED_MAIN"
  echo "sudo install -d -m 0755 $MOUNT_MAIN"
  echo "sudo mount -t cifs //127.0.0.1/$SHARE_NAME $MOUNT_MAIN -o credentials=$CRED_MAIN,vers=3.0,uid=\$(id -u),gid=\$(id -g)"
  echo "mount | grep $SHARE_NAME"
  echo "sudo umount $MOUNT_MAIN"
} >"$PAYLOADS/client-mount-steps.txt"

sudo install -d -m 0700 /root/.smb
if [ -d /root/.smb ]; then pass "凭据目录 /root/.smb 建好了"; else fail "凭据目录 /root/.smb 建好了" "不存在"; fi
expect_eq "凭据目录权限 0700" "700" "$(stat -c %a /root/.smb)"

# here-doc 的正文从标准输入进 cat，密码不在任何进程的 argv 里。
sudo sh -c "umask 077; cat > $CRED_MAIN" <<CRED
username=$ACCT_RW
password=$ACCT_RW_PW
CRED
sudo chmod 0600 "$CRED_MAIN"
expect_eq "凭据文件存在" "True" "$([ -f "$CRED_MAIN" ] && echo True || echo False)"
expect_eq "凭据文件权限是 0600" "600" "$(stat -c %a "$CRED_MAIN")"
expect_eq "凭据文件里有账号与密码两行" "2" "$(grep -c . "$CRED_MAIN")"

sudo install -d -m 0755 "$MOUNT_MAIN"
PROC_HITS_DURING_MOUNT=0
MOUNT_OUT=$(sudo mount -t cifs "//127.0.0.1/$SHARE_NAME" "$MOUNT_MAIN" \
  -o "credentials=$CRED_MAIN,vers=3.0,uid=$(id -u),gid=$(id -g)" 2>&1)
PROC_HITS_DURING_MOUNT=$((PROC_HITS_DURING_MOUNT + $(hits_in_processes | tr -d '\r')))
printf '%s\n' "$MOUNT_OUT" >"$PAYLOADS/client-mount-out.txt"
expect_eq "mount.cifs 没有报错" "" "$MOUNT_OUT"
expect_eq "独立证据：共享挂到了 $MOUNT_MAIN" "True" "$(is_mounted "$MOUNT_MAIN")"
MOUNT_LINE=$(mount | grep " $MOUNT_MAIN " | head -1)
expect_contains "mount 输出里有这条挂载" "//127.0.0.1/$SHARE_NAME" "$MOUNT_LINE"
expect_contains "挂载参数里是 cifs 与 vers=3.0" "cifs" "$MOUNT_LINE"
expect_not_contains "/proc/mounts 里没有凭据内容" "$ACCT_RW_PW" "$(cat /proc/mounts)"
expect_eq "凭据文件的路径没有出现在 /proc/mounts（mount.cifs 会把它剥掉）" "0" \
  "$(grep -c "credentials=$CRED_MAIN" /proc/mounts 2>/dev/null || true)"

# 通过挂载点写：写用户能写、能读回。
WRITE_THROUGH=$MOUNT_MAIN/ui-probe.txt
printf 'written through the mounted cifs share at %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" >"$WRITE_THROUGH"
expect_eq "写用户通过挂载点能写" "True" "$([ -f "$WRITE_THROUGH" ] && echo True || echo False)"
expect_eq "独立证据：文件落在卷里的位置由 relative_path 决定（这里是卷根）" "True" \
  "$([ -f "$MP/ui-probe.txt" ] && echo True || echo False)"
expect_eq "落盘内容与写下的一致" "$(sha256sum "$WRITE_THROUGH" | cut -d' ' -f1)" \
  "$(sha256sum "$MP/ui-probe.txt" 2>/dev/null | cut -d' ' -f1)"
expect_contains "从挂载点读回内容" "written through the mounted cifs share" "$(cat "$WRITE_THROUGH")"

# 只读身份：同一条命令，换一份凭据、换一个挂载点。
sudo sh -c "umask 077; cat > $CRED_RO" <<CRED
username=$ACCT_RO
password=$ACCT_RO_PW
CRED
sudo chmod 0600 "$CRED_RO"
sudo install -d -m 0755 "$MOUNT_RO"
RO_MOUNT_OUT=$(sudo mount -t cifs "//127.0.0.1/$SHARE_NAME" "$MOUNT_RO" \
  -o "credentials=$CRED_RO,vers=3.0,uid=$(id -u),gid=$(id -g)" 2>&1)
printf '%s\n' "$RO_MOUNT_OUT" >"$PAYLOADS/client-mount-ro-out.txt"
expect_eq "只读身份的挂载没有报错" "" "$RO_MOUNT_OUT"
expect_eq "独立证据：只读身份也挂上了" "True" "$(is_mounted "$MOUNT_RO")"
expect_contains "只读身份能读到写用户留下的文件" "ui-probe.txt" "$(ls "$MOUNT_RO" 2>&1)"
# 用 touch 而不是 printf 重定向：重定向失败时错误是 **shell** 打的，它会绕开
# 命令自己的 2>；touch 的失败信息由 touch 自己写到 fd 2，才捕获得到。
touch "$MOUNT_RO/ro-probe.txt" 2>"$WORK/ro-write.err"
RO_WRITE_RC=$?
RO_WRITE_OUT=$(cat "$WORK/ro-write.err" 2>/dev/null)
expect_ne "只读身份通过挂载点写被拒（shell 拿到非零退出码）" "0" "$RO_WRITE_RC"
case "$RO_WRITE_OUT" in
  *"Permission denied"*|*"Read-only file system"*)
    pass "拒绝是真的系统错误：$RO_WRITE_OUT" ;;
  *) fail "拒绝是真的系统错误" "退出码 $RO_WRITE_RC，stderr '$RO_WRITE_OUT'" ;;
esac
expect_eq "独立证据：卷里没有只读身份建出来的文件" "False" \
  "$([ -f "$MP/ro-probe.txt" ] && echo True || echo False)"

# umount：说明的最后一步，两个挂载点都要干净地卸掉。
UMOUNT_OUT=$(sudo umount "$MOUNT_MAIN" 2>&1)
expect_eq "umount $MOUNT_MAIN 没有报错" "" "$UMOUNT_OUT"
expect_eq "独立证据：$MOUNT_MAIN 已经卸掉" "False" "$(is_mounted "$MOUNT_MAIN")"
sudo umount "$MOUNT_RO" >/dev/null 2>&1
expect_eq "独立证据：$MOUNT_RO 已经卸掉" "False" "$(is_mounted "$MOUNT_RO")"

echo
echo "== 6. 权限矩阵（协议层，身份来自真实秘密通道） =="
REAL_FILE=$WORK/logs/uplink.txt
printf 'written through a real smbclient at %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" >"$REAL_FILE"
PUT_RW=$(smb_put "$SHARE_NAME" "$AUTH_RW" "$REAL_FILE" "probe-write.txt")
printf '%s\n' "$PUT_RW" >"$PAYLOADS/smbclient-put-rw.txt"
expect_contains "write 用户能上传" "putting file" "$PUT_RW"
expect_eq "独立证据：文件真的落在卷里" "True" "$([ -f "$MP/probe-write.txt" ] && echo True || echo False)"

LS_RO=$(smb_ls "$SHARE_NAME" "$AUTH_RO")
printf '%s\n' "$LS_RO" >"$PAYLOADS/smbclient-ls-ro.txt"
expect_not_contains "read 用户能列目录" "NT_STATUS_" "$LS_RO"
expect_contains "read 用户看得到已有文件" "probe-write.txt" "$LS_RO"
PUT_RO=$(smb_put "$SHARE_NAME" "$AUTH_RO" "$REAL_FILE" "ro-should-fail.txt")
printf '%s\n' "$PUT_RO" >"$PAYLOADS/smbclient-put-ro.txt"
expect_contains "read 用户写被拒" "NT_STATUS_" "$PUT_RO"
expect_eq "独立证据：read 用户没有写出任何文件" "False" \
  "$([ -f "$MP/ro-should-fail.txt" ] && echo True || echo False)"

LS_X=$(smb_ls "$SHARE_NAME" "$AUTH_X")
printf '%s\n' "$LS_X" >"$PAYLOADS/smbclient-ls-unauthorized.txt"
expect_contains "名单外的账号（在 passdb 里）被拒" "NT_STATUS_" "$LS_X"
expect_not_contains "名单外的账号看不到内容" "probe-write.txt" "$LS_X"

GUEST_OUT=$(smb_ls_guest "$SHARE_NAME")
printf '%s\n' "$GUEST_OUT" >"$PAYLOADS/smbclient-guest.txt"
expect_contains "匿名（guest）被拒" "NT_STATUS_" "$GUEST_OUT"
expect_not_contains "匿名看不到内容" "probe-write.txt" "$GUEST_OUT"

echo
echo "== 7. 密码卫生：命令行、日志、数据库、挂载表 =="
expect_eq "代理日志里没有密码明文" "0" "$(hits_in_file "$AGENT_LOG")"
expect_eq "Web 日志里没有密码明文" "0" "$(hits_in_file "$WEB_LOG")"
expect_eq "抓回来的载荷里没有密码明文" "0" \
  "$(grep -a -c -F -f "$PW_FILE" "$PAYLOADS"/* 2>/dev/null | awk -F: '{s+=$2} END {print s+0}')"
expect_eq "代理与面板的数据库里没有密码明文" "0" "$(hits_in_databases)"
expect_eq "进程表（cmdline/environ）里没有密码明文" "0" "$(hits_in_processes)"
note "挂载期间对 /proc 采样的命中数：$PROC_HITS_DURING_MOUNT（采样不是证明，命令本身不含密码才是）"
expect_eq "凭据文件之外没有别的文件留着密码（/root/.smb 下只有两份 0600）" "0" \
  "$(find /root/.smb -type f ! -perm 0600 2>/dev/null | wc -l | tr -d ' ')"
expect_eq "提交账号动作时任务参数里带的是 sec_ 引用（不是明文）" "True" \
  "$(python3 - "$PAYLOADS/preflight-acct-$ACCT_RW.json" "$PW_FILE" <<'PY'
import json, sys
doc = json.load(open(sys.argv[1], encoding='utf-8'))
text = json.dumps(doc, ensure_ascii=False)
secrets = [s.strip() for s in open(sys.argv[2], encoding='utf-8') if s.strip()]
print('sec_' in text and not any(s in text for s in secrets))
PY
)"
expect_eq "秘密响应里没有明文" "0" \
  "$(hits_in_file "$PAYLOADS/secret-$ACCT_RW.json")"

echo
echo "== 8. 抓取页面夹具（供 web 侧回放） =="
FIXTURES=$WORK/fixtures
mkdir -p "$FIXTURES"
cp "$WORK/shares-main.json" "$FIXTURES/shares.json"
cp "$WORK/accounts-main.json" "$FIXTURES/accounts.json"
cp "$WORK/connections-main.json" "$FIXTURES/connections.json"
cp "$WORK/share-main.json" "$FIXTURES/share.json" 2>/dev/null || \
  { get_share "$SHARE_ID" main >/dev/null; cp "$WORK/share-main.json" "$FIXTURES/share.json"; }
cp "$PAYLOADS/preflight-apply-main.json" "$FIXTURES/preflight-apply.json"
cp "$PAYLOADS/job-apply-main.json" "$FIXTURES/job-apply.json"
cp "$PAYLOADS/job-acct-$ACCT_RW.json" "$FIXTURES/job-account-apply.json"
cp "$WORK/volumes-main.json" "$FIXTURES/volumes.json" 2>/dev/null || {
  volumes main >/dev/null; cp "$WORK/volumes-main.json" "$FIXTURES/volumes.json"; }
# 页面给出的命令原文，和真的执行了什么（回放用例用它证明“说明里的命令与跑过的命令是同一条”）。
cp "$PAYLOADS/client-mount-steps.txt" "$FIXTURES/client-mount-steps.txt"
cp "$PAYLOADS/client-commands-run.txt" "$FIXTURES/client-commands-run.txt"
for f in "$FIXTURES"/*.json; do
  if python3 -c 'import json,sys' >/dev/null && python3 -m json.tool "$f" >/dev/null 2>&1; then
    pass "夹具是合法 JSON：$(basename "$f")"
  else
    fail "夹具是合法 JSON：$(basename "$f")" "解析失败"
  fi
done
expect_eq "夹具里没有密码明文" "0" \
  "$(grep -a -c -F -f "$PW_FILE" "$FIXTURES"/* 2>/dev/null | awk -F: '{s+=$2} END {print s+0}')"

echo
echo "== 9. 卷掉线：页面说的“连不上”与现场一致 =="
# 面板**不让人**在有启用共享的卷上直接卸载（VOLUME_IN_USE_BY_SHARE，§6.5）。要复现“运行中
# 掉线”，只能像现实里那样从外面卸——拔盘、手滑的 umount、后端存储掉了，面板看到的都是同一件事。
VOL_VER=$(current_vol_version "$VOL_ID" b0)
PRE_CODE=$(preflight storage.volume.unmount "{\"volume_id\":\"$VOL_ID\",\"version\":\"$VOL_VER\"}")
cp "$WORK/preflight.json" "$PAYLOADS/preflight-unmount-blocked.json"
expect_eq "有启用共享时，面板拒绝卸载这块卷" "False" "$(pget 'd["data"]["allowed"]')"
expect_contains "拒绝的理由是 VOLUME_IN_USE_BY_SHARE" "VOLUME_IN_USE_BY_SHARE" "$(pall)"

umount "$MP" 2>"$WORK/umount.err"
expect_eq "独立证据：卷被从外面卸掉了" "False" "$(is_mounted "$MP")"

# 撤销循环每 30 秒跑一次（DefaultShareWatchInterval）：最多等 75 秒，等到了就往下走。
REVOKE_WAIT=0
while [ "$REVOKE_WAIT" -lt 75 ]; do
  get_shares offline >/dev/null
  if [ "$(sget offline "bool([s for s in d[\"data\"][\"shares\"] if s[\"id\"]==\"$SHARE_ID\"][0].get(\"runtime_revoked\"))")" = "True" ]; then break; fi
  REVOKE_WAIT=$((REVOKE_WAIT + 1))
  sleep 1
done
note "等撤销用了 ${REVOKE_WAIT} 秒"
cp "$WORK/shares-offline.json" "$PAYLOADS/page-shares-offline.json"
SEL_OFF="[s for s in d[\"data\"][\"shares\"] if s[\"id\"]==\"$SHARE_ID\"][0]"
expect_eq "运行时撤销记录出现了" "True" "$(sget offline "bool($SEL_OFF.get(\"runtime_revoked\"))")"
# 卷的状态**不在**共享记录里（契约闭合，见 §3 与 Q246）：页面从 /storage/volumes 的
# observed_state join 出来。所以“卷掉线了”要分两处看——卷那边观测到 unmounted，共享这边
# 记下了当时的卷状态。直接读共享记录的 volume_state 只会拿到 None，而 None 不等于 unmounted。
volumes offline >/dev/null
volsget offline "[v for v in d[\"data\"][\"volumes\"] if v[\"id\"]==\"$VOL_ID\"][0][\"observed_state\"]" >/dev/null
expect_eq "卷那边观测到 unmounted（页面 join 的就是这个）" "unmounted" \
  "$(volsget offline "[v for v in d[\"data\"][\"volumes\"] if v[\"id\"]==\"$VOL_ID\"][0][\"observed_state\"]")"
expect_eq "共享记录里依旧没有 volume_state（状态只从卷列表来）" "True" \
  "$(sget offline "not any(k in $SEL_OFF for k in (\"volume_state\", \"volume_name\"))")"
expect_eq "撤销记录里写着当时的卷状态" "unmounted" "$(sget offline "$SEL_OFF[\"runtime_revoked\"][\"volume_state\"]")"
expect_eq "撤销记录里有一句给人读的理由" "True"   "$(sget offline "len($SEL_OFF[\"runtime_revoked\"].get(\"reason\") or \"\") > 0")"
expect_eq "config_applied 跟着变成假（配置里那段被撤了）" "False" "$(sget offline "bool($SEL_OFF[\"config_applied\"])")"
expect_eq "独立证据：托管配置里已经没有这一段" "False" "$(smb_section_exists "$SHARE_NAME")"
OFFLINE_LS=$(smb_ls "$SHARE_NAME" "$AUTH_RW")
printf '%s\n' "$OFFLINE_LS" >"$PAYLOADS/smbclient-volume-offline.txt"
expect_contains "卷掉线时客户端连不上（真的 NT_STATUS）" "NT_STATUS_" "$OFFLINE_LS"

# 卷挂回来也不会自动复活：这是 CONTRACTS 里“撤销只减不增”的那一条。
mount "$MP" 2>"$WORK/mount.err"
expect_eq "独立证据：卷又挂上了（fstab 里那条，noauto）" "True" "$(is_mounted "$MP")"
MOUNT_BACK_WAIT=0
while [ "$MOUNT_BACK_WAIT" -lt 20 ]; do
  get_shares restored >/dev/null
  volumes restored >/dev/null
  if [ "$(volsget restored "[v for v in d[\"data\"][\"volumes\"] if v[\"id\"]==\"$VOL_ID\"][0][\"observed_state\"]")" = "mounted" ]; then break; fi
  MOUNT_BACK_WAIT=$((MOUNT_BACK_WAIT + 1))
  sleep 1
done
cp "$WORK/shares-restored.json" "$PAYLOADS/page-shares-restored.json"
SEL_R="[s for s in d[\"data\"][\"shares\"] if s[\"id\"]==\"$SHARE_ID\"][0]"
expect_eq "卷挂回来之后仍然是撤销状态（不自动恢复）" "True" "$(sget restored "bool($SEL_R.get(\"runtime_revoked\"))")"
RESTORED_LS=$(smb_ls "$SHARE_NAME" "$AUTH_RW")
expect_contains "撤销状态下客户端依然连不上" "NT_STATUS_" "$RESTORED_LS"

# 重新应用才恢复服务：这一条把“页面教你点应用”与“应用真的有用”接上。
SHARE_VER=$(get_share "$SHARE_ID" main >/dev/null; shget main 'd["data"]["share"]["version"]')
run_action share.apply "{\"share_id\":\"$SHARE_ID\",\"version\":\"$SHARE_VER\",\"definition\":$DEF_MAIN}" reapply-main
expect_eq "重新应用任务终态" "succeeded" "$JOB_STATE"
expect_eq "重新应用结果 verified" "verified" "$(joutcome)"
get_shares serving >/dev/null
SEL_S="[s for s in d[\"data\"][\"shares\"] if s[\"id\"]==\"$SHARE_ID\"][0]"
expect_eq "重新应用之后撤销记录被清掉" "False" "$(sget serving "bool($SEL_S.get(\"runtime_revoked\"))")"
expect_eq "config_applied 回到真" "True" "$(sget serving "bool($SEL_S[\"config_applied\"])")"
SERVING_LS=$(smb_ls "$SHARE_NAME" "$AUTH_RW")
printf '%s
' "$SERVING_LS" >"$PAYLOADS/smbclient-after-reapply.txt"
expect_not_contains "重新应用之后客户端又能连上" "NT_STATUS_" "$SERVING_LS"

# §8 里抓的是“一切正常”的现场；掉线与恢复的现场在这里补抓（顺序决定它只能在这儿）。
cp "$PAYLOADS/page-shares-offline.json" "$FIXTURES/shares-offline.json" 2>/dev/null || true
cp "$PAYLOADS/page-shares-restored.json" "$FIXTURES/shares-restored.json" 2>/dev/null || true
cp "$PAYLOADS/smbclient-put-ro.txt" "$FIXTURES/smbclient-put-ro.txt" 2>/dev/null || true

echo "== 10. 收尾不变量：撤掉共享之后卷里的东西一个不少 =="
DIR_BEFORE=$(find "$MP" -type f | wc -l | tr -d ' ')
CONTENT_BEFORE=$(sha256sum "$MP/probe-write.txt" 2>/dev/null | cut -d' ' -f1)
SHARE_VER=$(get_share "$SHARE_ID" main >/dev/null; shget main 'd["data"]["share"]["version"]')
run_action share.disable "{\"share_id\":\"$SHARE_ID\",\"version\":\"$SHARE_VER\"}" disable-main
expect_eq "停用任务终态" "succeeded" "$JOB_STATE"
expect_eq "独立证据：段从配置里消失了" "False" "$(smb_section_exists "$SHARE_NAME")"
DISABLED_LS=$(smb_ls "$SHARE_NAME" "$AUTH_RW")
printf '%s\n' "$DISABLED_LS" >"$PAYLOADS/smbclient-after-disable.txt"
expect_contains "停用之后客户端连不上" "NT_STATUS_" "$DISABLED_LS"
expect_eq "独立证据：文件数与停用前一致" "$DIR_BEFORE" "$(find "$MP" -type f | wc -l | tr -d ' ')"
expect_eq "独立证据：内容哈希没变" "$CONTENT_BEFORE" "$(sha256sum "$MP/probe-write.txt" 2>/dev/null | cut -d' ' -f1)"

# 停用会推进版本号，删除必须拿**新的**版本：拿旧版本会被 SHARE_VERSION_MISMATCH 挡下，
# 那是契约正确、脚本写错。
SHARE_VER=$(get_share "$SHARE_ID" main >/dev/null; shget main 'd["data"]["share"]["version"]')
expect_ne "停用之后拿到新版本号" "" "$SHARE_VER"
run_action share.delete "{\"share_id\":\"$SHARE_ID\",\"version\":\"$SHARE_VER\"}" delete-main
expect_eq "删除任务终态" "succeeded" "$JOB_STATE"
expect_eq "独立证据：托管文件里不再有这一段" "False" \
  "$(grep -qF "[$SHARE_NAME]" "$MANAGED_CONF" 2>/dev/null && echo True || echo False)"
expect_eq "独立证据：目录与文件仍然在" "$DIR_BEFORE" "$(find "$MP" -type f | wc -l | tr -d ' ')"
expect_eq "共享列表回到空数组" "True" "$(get_shares final >/dev/null; sget final 'd["data"]["shares"] == []')"
expect_eq "能力报告的 managed_shares 也回到空" "True" \
  "$(sget final 'd["data"]["capability"]["managed_shares"] == []')"

# 夹具里的秘密文件（认证文件、密码文件）在收尾时删掉：它们不属于证据。
rm -f "$SECRETS_DIR"/auth-* 2>/dev/null
rm -rf /root/.smb

echo
echo "== 11. 结论 =="
# 这些**没跑**，写出来而不是让汇总里的 0 号位空着：没跑的事不能长得像通过。
notrun "Windows 客户端（资源管理器 / net use）" "这台机器上没有 Windows，P00-VM-D 客户端 VM 尚未建立；说明里给它的地址形态由 web 侧断言"
notrun "macOS 客户端（Finder）" "同上，没有 macOS 测试机"
notrun "从另一台物理机器连进来" "本机的客户端与 smbd 同机；跨机器还牵涉防火墙，属于没验过的路径"
notrun "浏览器里的真实渲染" "脚本验的是页面读的载荷；DOM 由 vitest 的 SSR 回放覆盖（realdata-p08-4.test.ts）"

printf 'PASS=%s FAIL=%s NOT RUN=%s\n' "$PASSED" "$FAILED" "$NOTRUN"
if [ "$FAILED" -ne 0 ]; then
  printf '失败项：%s\n' "$FAILED_NAMES"
  exit 1
fi
exit 0
