#!/bin/sh
# P08.1 Linux 验收脚本：Samba 能力检测、受管理账号、密码秘密通道、POSIX ACL、
# 共享定义与配置版本。
#
# 与 p07-x 的分工：那几份证明的是“磁盘上的字节与 fstab 真的对上了”。本脚本证明的是
# P08.1 这一阶段真正承诺的六件事，每一条都要有一条**不经过面板**的证据：
#
#   1) **能力检测说的是真话**：面板说 Samba 齐了，就必须是 `smbd/smbpasswd/pdbedit/
#      setfacl/getfacl` 真的在固定绝对路径上存在、smb.conf 真的可读、版本号与
#      `smbd --version` 一致；面板说 config_write=unsupported，就必须一个字节都没写。
#   2) **密码只经过一次，且只经标准输入**：明文不出现在任何进程的 argv/environ、
#      不出现在两边的日志、不以明文出现在两边的数据库、不出现在任何响应体里。
#      这一条用随机密码 + `grep -f 文件`（不是 `grep -F "$PW"`，否则检查者自己的
#      argv 里就有密码了）来验。
#   3) **账号是真的建出来的**：`getent passwd` 与 `pdbedit -Lv` 两条独立读回，
#      uid 落在系统段、shell 是 nologin、home 是 /nonexistent、Linux 密码锁着、
#      passdb 里有 Password last set。
#   4) **目录与 POSIX ACL 是真的做出来的**：`getfacl` 读回逐条对照定义，mask 覆盖
#      授权，world 一点权限都没有。
#   5) **但共享此刻不对外提供访问**——这是本阶段最容易撒谎的地方。所以 share.apply
#      的结果必须是 `accepted` 而**不是** `verified`，`verified_at` 必须为空，
#      `observed.served` 必须是 false，`config_applied` 必须恒为 false，而且
#      `/etc/samba/smb.conf` 的 sha256 在整轮前后必须逐字节不变、`testparm -s`
#      里不许出现这条共享的段名。
#   6) **该拒的拒，且理由写给页面看**：越界路径、符号链接、未挂载的卷、未知账号、
#      空名单、nfs、重名、被共享引用的账号删除——每一条都要有 blocker 码与 message。
#
# 用法：
#   sudo BIN_DIR=/var/tmp/upanel-bin SPARE_DISKS="sdb sdc" \
#        sh tests/integration/p08-1-share-acceptance.sh
#
# 环境变量：PORT（默认 19476，避开 p07-1..5 的端口）、WORK、BIN_DIR、SPARE_DISKS
# （两块一次性空盘，会被格式化——只允许在可丢弃的测试 VM 里跑）。
#
# 前置条件（属于夹具，不属于被测功能）：这台 VM 上已经装好 samba 与 acl。
# 面板**不会**替你装包（PLAN.md 第 10 节禁止静默安装）；缺包时本脚本在第 0 节就
# 以退出码 2 停下，而不是把“缺 smbd”记成产品缺陷。
#
# 退出码：0 全部通过；1 有失败项；2 环境不满足（非 root / 盘不安全 / 缺工具 / 缺 samba）。
#
# 明确不验（NOT RUN，原因写在这里，不在脚本里假装通过）：
#   - **从另一台机器用 SMB 客户端连上来**：P08.1 不写配置、不重载服务，共享此刻
#     根本不对外提供访问，连上去只能证明“连不上”。SMB 客户端 VM（P00-VM-D）尚未建立；
#     这一步从 P08.2 起才是有效验收。
#   - **写入 Samba 配置、testparm 校验、原子替换、重载、共享连接状态**：属于 P08.2
#     与 P08.3（docs/STAGES.md 第 10 节）。本脚本反过来证明“确实没写”。
#   - **浏览器里的真实渲染**：本脚本验的是页面**读到的那份载荷**；把载荷画成 DOM 由
#     web 侧 SSR 回放覆盖，浏览器端到端由 `web/scripts/smoke.mjs` 覆盖。三者不是同一件事。
#   - **能力 blocked 分支（缺包环境）**：本机装了 samba，因此走的是 available。
#     blocked 分支由 `internal/shares` 的单元测试用注入的探针覆盖，不是这里。

set -u

BIN_DIR=${BIN_DIR:-/var/tmp/upanel-bin}

if [ "$(id -u)" -ne 0 ]; then
  printf '本脚本必须以 root 运行：sudo BIN_DIR=%s sh %s\n' "$BIN_DIR" "$0" >&2
  exit 2
fi

AGENT="$BIN_DIR/upanel-agent"
WEB="$BIN_DIR/upanel-web"
CTL="$BIN_DIR/upanelctl"

WORK=${WORK:-/var/tmp/upanel-accept-p08-1}
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
PORT=${PORT:-19476}
BASE="http://127.0.0.1:$PORT"
ORIGIN="$BASE"
WEB_USER=upanel-web
PASSWORD='vm-correct-horse-9'

SPARE_DISKS=${SPARE_DISKS:-"sdb sdc"}
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
FSTAB_ARCHIVE=""

VOLUME_ROOT=/srv/upanel/volumes
FSTAB=/etc/fstab
SMB_CONF=/etc/samba/smb.conf
SMB_INCLUDE_DIR=/etc/samba/upanel.d
GIB=1073741824
PART_MAIN=$((2 * GIB))
PART_ALT=$GIB
PART_START=1048576
SLUG_MAIN=data
SLUG_ALT=logs
LABEL_MAIN=p08vol
LABEL_ALT=p08logs

# 夹具里的名字。共享名与账号名都落在产品自己收紧后的字符集里（小写、数字、-、_）。
SHARE_NAME=p08share
REL_PATH=team/docs
ACCT_RW=smbp08rw
ACCT_RO=smbp08ro
VIEWER_USER=p08viewer
VIEWER_PW='vm-viewer-horse-4'

PASSED=0
FAILED=0
NOTRUN=0
BLK_N=0
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

# jget <文件> <python 表达式，d 为解析后的 JSON>
#
# 求值出错时打印 ERR:<类型>:<消息>：空字符串会让“表达式写错”看起来像
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
  if [ -n "${AGENT_PID:-}" ] && kill -0 "$AGENT_PID" 2>/dev/null; then
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
  if [ -n "${WEB_PID:-}" ] && kill -0 "$WEB_PID" 2>/dev/null; then
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

reauth() { api "$JAR" "$CSRF" POST /api/v1/auth/reauth "{\"password\":\"$PASSWORD\"}"; }

# preflight <动作> <参数 JSON>：预检响应另存一份 $WORK/preflight.json，回显状态码。
preflight() {
  printf '{"action":"%s","params":%s}' "$1" "$2" >"$WORK/pre.json"
  # 请求体自己先过一遍 JSON 解析。真机上见过两次“请求体不是合法的 JSON”的 400：
  # 一次是我在这条命令里把闭合引号写丢了（名字后面的 `"`），服务端只回一句
  # 400，真正的原因得靠反推。这里当场把坏掉的请求体喊到日志里。
  python3 -c 'import json, sys
json.load(open(sys.argv[1], encoding="utf-8"))' "$WORK/pre.json" 2>/dev/null ||
    printf '!!! 预检请求体不是合法 JSON：%s\n' "$(cat "$WORK/pre.json")" >&2
  _code=$(curl -s -b "$JAR" -o "$WORK/resp.json" -w '%{http_code}' \
    -X POST "$BASE$PREFLIGHT_PATH" \
    -H 'Content-Type: application/json' -H "Origin: $ORIGIN" -H "X-CSRF-Token: $CSRF" \
    -d @"$WORK/pre.json")
  cp "$WORK/resp.json" "$WORK/preflight.json"
  printf '%s' "$_code"
}

# submit_job <preflight_id> <短语> <幂等键>：响应存为 $WORK/submit.json，回显状态码。
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

# wait_job <job_id>：轮询到终态，任务视图存为 $WORK/job.json，回显终态或 timeout。
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

# run_action <动作> <参数 JSON> <幂等键>
#
# 预检 →（预检说需要再认证就先再认证）→ 提交 → 等终态。
# 结果放在全局变量里（不放进 $( ) 里：那会开子 shell，变量带不回来）：
#   PRE_CODE / PRE_ALLOWED / PRE_ID / PRE_PHRASE / PRE_REAUTH
#   JOB_ID / JOB_CODE / JOB_STATE
run_action() {
  PRE_CODE=$(preflight "$1" "$2")
  cp "$WORK/preflight.json" "$PAYLOADS/preflight-$3.json"
  PRE_ALLOWED=$(pallowed)
  PRE_ID=$(pget 'd["data"]["preflight_id"]')
  PRE_PHRASE=$(pget 'd["data"].get("confirmation_phrase") or ""')
  PRE_REAUTH=$(preauth)
  if [ "$PRE_REAUTH" = "True" ]; then
    REAUTH_CODE=$(reauth)
    [ "$REAUTH_CODE" = "200" ] || fail "再认证（$3）" "状态码 $REAUTH_CODE"
  fi
  # 预检没有给出可提交的计划时立刻记失败，并把上一轮的 job.json 清空。
  # 不清的话，后面每一条 job 断言都在读**上一轮**的结果：真机 §5 就这么骗过自己——
  # 预检 503、提交 404，而 joutcome/jobobserved 拿到的还是上一轮那份 job.json，
  # 一连串“通过”全是假的。
  # 200 与 201 都算给出了计划：Web 的预检接口回 200（代理那边才是 201）。
  if [ -z "$PRE_ID" ] || { [ "$PRE_CODE" != "200" ] && [ "$PRE_CODE" != "201" ]; }; then
    fail "预检（$3）给出可提交的计划" "状态码 $PRE_CODE，响应 $(head -c 300 "$WORK/preflight.json")"
    JOB_ID=""
    JOB_STATE="preflight:$PRE_CODE"
    : >"$WORK/job.json"
    return 0
  fi
  JOB_CODE=$(submit_job "$PRE_ID" "$PRE_PHRASE" "$3")
  cp "$WORK/submit.json" "$PAYLOADS/submit-$3.json"
  if [ "$JOB_CODE" = "202" ]; then
    JOB_ID=$(jget "$WORK/submit.json" 'd["data"]["job_id"]')
    JOB_STATE=$(wait_job "$JOB_ID")
    cp "$WORK/job.json" "$PAYLOADS/job-$3.json"
  else
    # 提交没拿到任务：一样要清掉上一轮的 job.json。真机 §5 的第二遍就是被这个骗的：
    # 提交回了 500，而后面每一条 job 断言读到的还是上一个账号的任务，于是
    # “结果是 verified”“带 verified_at”一路通过，失败全被藏到 §6 以后去了。
    JOB_ID=""
    JOB_STATE="submit:$JOB_CODE"
    : >"$WORK/job.json"
    fail "提交（$3）" "状态码 $JOB_CODE，响应 $(head -c 300 "$WORK/submit.json")"
  fi
}

jstatus() { jobget 'd["data"]["status"]'; }
joutcome() { jobget 'd["data"]["result"]["outcome"]'; }
jverified() { jobget 'bool(d["data"]["result"].get("verified_at"))'; }
jrecovery() { jobget 'd["data"]["result"].get("recovery_hint") or ""'; }
jobobserved() { jobget "d[\"data\"][\"result\"][\"observed_state\"][\"$1\"]"; }
jobdata() { jobget "d[\"data\"][\"result\"][\"data\"].get(\"$1\")"; }

pallowed() { pget 'd["data"]["allowed"]'; }
pall() { pget '",".join(b["code"] for b in d["data"]["blockers"])'; }
pwhy() { pget '"; ".join(b["message"] for b in d["data"]["blockers"])'; }
pwarn() { pget '",".join(w["code"] for w in (d["data"].get("warnings") or []))'; }
pimpact() { pget '",".join(i["kind"] for i in d["data"]["impacts"])'; }
plocks() { pget '",".join(d["data"]["locks"])'; }
pstate() { pget 'd["data"]["target"]["display_name"]'; }
preauth() { pget 'bool(d["data"].get("requires_reauth"))'; }

# blocked <名字> <预检参数> <期望的 blocker 码>
#
# 一条被拒的预检要三件事同时成立：允许=false、阻塞码里有期望的那一个、每条阻塞都
# 带人读的理由。还要第四件：**它提交不进任务**——预检说“不许”之后，POST /jobs
# 必须照样拒绝（409），不许出现“先拒后放”。
blocked() {
  BLK_N=$((BLK_N + 1))
  preflight "$1" "$2" >/dev/null
  cp "$WORK/preflight.json" "$PAYLOADS/preflight-blocked-$3-$BLK_N.json"
  expect_eq "$1 被拒" "False" "$(pallowed)"
  expect_contains "$1 的阻塞码" "$3" "$(pall)"
  expect_ne "$1 的阻塞理由不是空的" "" "$(pwhy)"
  _pid=$(pget 'd["data"]["preflight_id"]')
  _code=$(submit_job "$_pid" "" "blocked-$BLK_N")
  expect_eq "$1 被拒的预检提交不进任务" "409" "$_code"
}

expect_verified() {
  expect_eq "$1任务终态" "succeeded" "$JOB_STATE"
  expect_eq "$1结果是 verified" "verified" "$(joutcome)"
  expect_eq "$1带 verified_at" "True" "$(jverified)"
}

# accepted 与本阶段的意义：动作真的做了、也核验了，但共享还没有被提供。
expect_accepted() {
  expect_eq "$1任务终态" "succeeded" "$JOB_STATE"
  expect_eq "$1结果是 accepted（不是 verified）" "accepted" "$(joutcome)"
  expect_eq "$1不带 verified_at" "False" "$(jverified)"
  expect_ne "$1给了 recovery_hint 说清还差什么" "" "$(jrecovery)"
}

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

# ---------- 独立证据（不经过产品的解析器） ----------

blkid_field() { blkid -p -o value -s "$2" "$1" 2>/dev/null | head -1; }
fs_type() { blkid_field "$1" TYPE; }
fs_uuid() { blkid_field "$1" UUID; }
fs_label() { blkid_field "$1" LABEL; }
is_mounted() {
  if [ -n "$(findmnt -rno TARGET --mountpoint "$1" 2>/dev/null)" ]; then echo True; else echo False; fi
}

smb_conf_sha() { sha256sum "$SMB_CONF" 2>/dev/null | cut -d' ' -f1; }
# 面板自己说“没写配置”不算数：问 Samba 自己的解析器这张共享段存不存在。
smb_served_sections() {
  testparm -s --suppress-prompt "$SMB_CONF" 2>/dev/null | grep -c "^\[$1\]" || true
}
smb_conf_include_lines() { grep -cF "/etc/samba/upanel.d/" "$SMB_CONF" 2>/dev/null || true; }

passwd_line() { getent passwd "$1" 2>/dev/null | head -1; }
passwd_uid() { passwd_line "$1" | cut -d: -f3; }
passwd_shell() { passwd_line "$1" | cut -d: -f7; }
passwd_home() { passwd_line "$1" | cut -d: -f6; }
# `passwd -S` 的第二列：L=锁定、P=可用、NP=无密码。共享账号只允许前两者里的锁定。
passwd_status() { passwd -S "$1" 2>/dev/null | awk '{print $2}'; }
pdb_names() { pdbedit -L 2>/dev/null | cut -d: -f1; }
pdb_has() {
  if pdb_names | grep -qxF "$1"; then echo True; else echo False; fi
}
# Account Flags 形如 [DU        ]：D=disabled、U=有用户名。只读，不改。
pdb_flags() {
  pdbedit -Lv -u "$1" 2>/dev/null | sed -n 's/^Account Flags: *//p' | head -1
}
pdb_password_set() {
  pdbedit -Lv -u "$1" 2>/dev/null \
    | sed -n 's/^Password last set: *//p' | head -1
}
acl_of() { getfacl -cp "$1" 2>/dev/null; }
acl_entry() {
  acl_of "$1" | awk -F: -v k="user:$2" '$0 ~ "^" k ":" {print $3}' | head -1
}
acl_has_named_users() {
  if acl_of "$1" | grep -q '^user:[^:]'; then echo True; else echo False; fi
}
# acl_field <目录> <键>：键是 "user::" / "group::" / "other::" / "mask::" 之一，
# 打印该行的权限位。用 index() 而不是分隔符：`user::rwx` 里两个冒号是连着的，
# 按 ':' 切会得到空字段。
acl_field() {
  acl_of "$1" | awk -v k="$2" 'index($0, k) == 1 {print substr($0, length(k) + 1); exit}'
}
# 目录对 world 的权限位（mode 的第三位）：必须是 0。
dir_other_bits() { stat -c '%a' "$1" 2>/dev/null | cut -c3; }
dir_mountpoint() { stat -c '%m' "$1" 2>/dev/null; }

# 明文扫描：用 grep -f 从文件里读模式，避免检查者自己的 argv 里出现密码。
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
hits_in_processes() {
  # 扫全部进程的 argv 与 environ。密码只在 smbpasswd 的标准输入上经过，
  # 因此这两处都不该出现它。
  for d in /proc/[0-9]*; do
    cat "$d/cmdline" 2>/dev/null | tr '\0' ' '
    printf '\n'
    cat "$d/environ" 2>/dev/null | tr '\0' ' '
    printf '\n'
  done | grep -a -c -F -f "$PW_FILE" 2>/dev/null || true
}

db_query() {
  python3 -c 'import sqlite3,sys
try:
    con = sqlite3.connect(sys.argv[1])
    rows = con.execute(sys.argv[2]).fetchall()
    print(";".join("|".join("" if c is None else str(c) for c in r) for r in rows))
except Exception as exc:
    print("ERR:%s:%s" % (type(exc).__name__, exc))' "$1" "$2" 2>/dev/null
}

echo "== 0. 环境与安全守卫 =="
for b in "$AGENT" "$WEB" "$CTL"; do
  if [ -x "$b" ]; then pass "二进制存在且可执行 $(basename "$b")"; else fail "二进制存在且可执行 $(basename "$b")" "$b"; fi
done
for t in python3 curl runuser sha256sum findmnt lsblk sgdisk blkid wipefs udevadm \
         blockdev stat awk diff grep shred; do
  if command -v "$t" >/dev/null 2>&1; then pass "依赖命令可用 $t"; else fail "依赖命令可用 $t" "缺少 $t"; fi
done

# Samba 与 acl 是**夹具**，不是被测功能：面板不会替你装包。
MISSING_PKGS=""
for t in /usr/sbin/smbd /usr/bin/smbpasswd /usr/bin/pdbedit /usr/bin/testparm \
         /usr/bin/setfacl /usr/bin/getfacl; do
  if [ -x "$t" ]; then pass "夹具：$t 存在"; else MISSING_PKGS="$MISSING_PKGS $t"; fi
done
if [ -f "$SMB_CONF" ]; then pass "夹具：$SMB_CONF 存在"; else MISSING_PKGS="$MISSING_PKGS $SMB_CONF"; fi
if [ -n "$MISSING_PKGS" ]; then
  printf '这台机器缺少 samba/acl 夹具：%s\n' "$MISSING_PKGS" >&2
  printf '先装好（apt-get install -y samba acl）再来跑验收；面板不负责装包。\n' >&2
  exit 2
fi
note "smbd 版本（独立读取）：$(/usr/sbin/smbd --version 2>&1 | head -1)"

if ! id -u "$WEB_USER" >/dev/null 2>&1; then
  useradd --system --no-create-home --shell /usr/sbin/nologin "$WEB_USER" 2>/dev/null
fi
if id -u "$WEB_USER" >/dev/null 2>&1; then pass "Web 账号 $WEB_USER 存在"; else fail "Web 账号 $WEB_USER 存在" "无法创建"; fi
if ! getent group "$WEB_USER" >/dev/null 2>&1; then groupadd --system "$WEB_USER" 2>/dev/null; fi
if curl -fsS "$BASE/api/v1/health/live" >/dev/null 2>&1; then
  echo "$PORT 上已有服务在监听；先停掉遗留的 upanel-web 再跑验收" >&2
  exit 1
fi

# —— 干净的起点：先把上一轮留下的挂载与托管块清掉，**再**做盘的守卫判断。
#
# 顺序不能反：守卫里那条“盘上不许有挂载点”是给“这盘到底能不能碰”用的，而上一轮
# 中断时留下的挂载点属于本脚本自己的残留。先清残留，守卫判断才是对**当前状态**的
# 判断，而不是对一个没收拾干净的现场报警。
for mp in "$VOLUME_ROOT"/*; do
  [ -d "$mp" ] || continue
  note "卸掉上一轮遗留的挂载 $mp"
  umount "$mp" 2>/dev/null || true
done
if grep -qx '# BEGIN upanel managed volumes' "$FSTAB" 2>/dev/null; then
  # 留档放在 $WORK 之外：$WORK 马上就要被整个删掉重建。
  FSTAB_ARCHIVE=$WORK.fstab-before-reset.txt
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

BLANK_A=$(printf '%s\n' $SPARE_DISKS | sed -n 1p)
BLANK_B=$(printf '%s\n' $SPARE_DISKS | sed -n 2p)
if [ -z "$BLANK_A" ] || [ -z "$BLANK_B" ]; then
  echo "SPARE_DISKS 需要两块盘（当前 '$SPARE_DISKS'）" >&2
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
ROOT_CHAIN=" $(lsblk -sno NAME "$ROOT_REAL" 2>/dev/null | tr '\n' ' ') $(lsblk -no NAME "$ROOT_REAL" 2>/dev/null | tr '\n' ' ') "
pass "承载 / 的设备是 $ROOT_REAL（整盘 $ROOT_DISK）"

for d in $SPARE_DISKS; do
  if [ -e "/dev/$d" ]; then pass "一次性盘 /dev/$d 存在"; else echo "/dev/$d 不存在" >&2; exit 2; fi
  case "$ROOT_CHAIN" in
    *" $d "*) echo "/dev/$d 与根设备同源（链路 $ROOT_CHAIN），拒绝使用" >&2; exit 2 ;;
  esac
  if [ "$d" = "$ROOT_DISK" ]; then
    echo "/dev/$d 就是承载 / 的整盘，拒绝使用" >&2
    exit 2
  fi
  if [ "$(lsblk -no TYPE "/dev/$d" 2>/dev/null | head -1)" != "disk" ]; then
    echo "/dev/$d 不是整盘（lsblk type != disk），拒绝使用" >&2
    exit 2
  fi
  if lsblk -no MOUNTPOINT "/dev/$d" 2>/dev/null | grep -q '[^ ]'; then
    echo "/dev/$d 上有挂载点，拒绝使用" >&2
    exit 2
  fi
  _size=$(blockdev --getsize64 "/dev/$d" 2>/dev/null || echo 0)
  if [ "$_size" -lt $((PART_MAIN + 64 * 1048576)) ]; then
    echo "/dev/$d 容量 ${_size} 字节，放不下夹具分区" >&2
    exit 2
  fi
  note "/dev/$d：$_size 字节，无挂载"
done

# —— 干净的起点（第二部分）：清空工作目录，然后记下起点的证据。
rm -rf "$WORK"
mkdir -p "$WORK/logs" "$PAYLOADS" "$SECRETS_DIR" "$WEB_DB_DIR"
chmod 0755 "$WORK" "$PAYLOADS" "$WORK/logs"
# 秘密只放在 root 读得到的目录里；进程 umask 可能给 0755，所以再显式收紧一次。
chmod 0700 "$SECRETS_DIR"
chown -R "$WEB_USER" "$WEB_DB_DIR"
if [ -f "${FSTAB_ARCHIVE:-}" ]; then
  cp "$FSTAB_ARCHIVE" "$WORK/logs/fstab-before-reset.txt" 2>/dev/null || true
fi
cp "$FSTAB" "$WORK/logs/fstab-initial.txt"
chmod 0644 "$WORK/logs/fstab-initial.txt"
lsblk -o NAME,TYPE,SIZE,FSTYPE,MOUNTPOINTS >"$WORK/logs/lsblk-initial.txt" 2>&1
cp "$SMB_CONF" "$WORK/logs/smb-conf-initial.txt" 2>/dev/null || true
SMB_CONF_SHA0=$(smb_conf_sha)
pass "起点：smb.conf 的 sha256 已记录（$SMB_CONF_SHA0）"

rm -rf "$VOLUME_ROOT"/* 2>/dev/null
install -d -m 0755 "$VOLUME_ROOT"
for d in $SPARE_DISKS; do
  wipefs -a "/dev/$d" >/dev/null 2>&1
  sgdisk --zap-all "/dev/$d" >/dev/null 2>&1
  blockdev --rereadpt "/dev/$d" >/dev/null 2>&1
  udevadm settle --timeout=5 >/dev/null 2>&1
done
pass "两块一次性盘清成空白"

# 上一轮半途死掉会把受管理账号留在系统里：下一轮建同名账号会被 ACCOUNT_NOT_MANAGED
# 挡住（“系统里已经有一个不是本面板创建的账号”），验收就从脏起点开始了。
# 这里只清本脚本自己建的这两个名字，清完立刻核对，清不掉照样算失败。
for n in "$ACCT_RW" "$ACCT_RO" p08sysprobe; do
  pdbedit -x -u "$n" >/dev/null 2>&1 || true
  userdel "$n" >/dev/null 2>&1 || true
done
if getent passwd "$ACCT_RW" >/dev/null 2>&1 || getent passwd "$ACCT_RO" >/dev/null 2>&1 ||
  pdbedit -L 2>/dev/null | grep -qE "^($ACCT_RW|$ACCT_RO):"; then
  fail "起点：受管理账号已清空" "系统里还有 smbp08* 账号残留"
else
  pass "起点：受管理账号已清空"
fi

echo
echo "== 1. 配置与启动（代理 root、Web 非 root） =="
install -d -m 0755 /etc/upanel
install -d -m 0700 "$AGENT_DB_DIR"
install -d -m 0755 "$RUN_DIR"
install -d -m 0755 "$VOLUME_ROOT"
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
grep -q "共享模块已接线" "$AGENT_LOG" && pass "代理日志声明共享模块已接线" \
  || fail "代理日志声明共享模块已接线" "日志里没有这一行（依赖不全？）"
grep -q '"config_write":"unsupported' "$AGENT_LOG" && pass "代理日志明说本阶段不写配置" \
  || fail "代理日志明说本阶段不写配置" "日志里没有 config_write=unsupported 这一项"

runuser -u "$WEB_USER" -- curl -s --unix-socket "$SOCK" \
  http://agent/internal/v1/capabilities >"$PAYLOADS/agent-capabilities.json" 2>/dev/null
AGENT_ACTIONS=$(jget "$PAYLOADS/agent-capabilities.json" \
  '(d.get("data") or d).get("supported_actions") or []')
for a in share.apply share.disable share.delete share.account.apply share.account.delete; do
  case "$AGENT_ACTIONS" in
    *"$a"*) pass "代理把 $a 登记成可执行动作" ;;
    *) fail "代理把 $a 登记成可执行动作" "capabilities.supported_actions 里没有它" ;;
  esac
done

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

# 密码入口的第一道门：再认证。**必须在这里测**——§3 的磁盘动作会顺带把这次会话
# 再认证掉（run_action 会自动补一次），等到 §4 再测就永远测不到“未再认证”这个状态：
# 真机 §5 那一轮就是在这里骗过自己的（403 变成了 201）。
NORE_CODE=$(api "$JAR" "$CSRF" POST "$SECRETS_PATH" '{"purpose":"smb_password","value":"reauth-probe"}')
expect_eq "未再认证时存秘密是 403" "403" "$NORE_CODE"
expect_contains "拒绝码是 REAUTH_REQUIRED" "REAUTH_REQUIRED" "$(jget "$WORK/resp.json" 'd["error"]["code"]')"
expect_eq "被拒之后代理的秘密表仍然是空的" "0" "$(db_query "$AGENT_DB" 'select count(*) from secrets')"

echo
echo "== 2. 能力检测：面板说的必须与系统上的事实一致 =="
# 页面第一眼看的就是这份能力。它答三个问题：Samba 齐不齐、配置读不读得到、
# 本阶段能不能写配置。
expect_eq "共享列表可读" "200" "$(get_shares cap)"
cp "$WORK/shares-cap.json" "$PAYLOADS/page-shares-empty.json"
expect_eq "能力状态是 available" "available" "$(sget cap 'd["data"]["capability"]["status"]')"
expect_eq "available 时没有 reason" "" "$(sget cap 'd["data"]["capability"].get("reason") or ""')"
expect_eq "另外两块能力也在同一份响应里" "True" \
  "$(sget cap 'all(k in d["data"] for k in ("capability","shares"))')"
expect_eq "共享列表此刻是空数组（不是 null）" "True" "$(sget cap 'd["data"]["shares"] == []')"

# 版本号：独立跑一次 smbd --version，去掉 "Version " 前缀后必须逐字相同。
SMB_VER_INDEP=$(/usr/sbin/smbd --version 2>/dev/null | head -1 | sed 's/^Version //')
expect_eq "能力里的 samba_version 与 smbd --version 一致" "$SMB_VER_INDEP" \
  "$(sget cap 'd["data"]["capability"]["samba_version"]')"

# 工具路径：产品报的每一条都必须真的存在且可执行；缺失的不许报。
TOOL_BAD=$(sget cap 'import os
bad=[]
t=d["data"]["capability"].get("tools") or {}
for k,v in t.items():
    if not v or not os.access(v, os.X_OK):
        bad.append(k+"="+str(v))
_=",".join(bad)')
expect_eq "能力里报的每个工具路径都存在且可执行" "" "$TOOL_BAD"
for k in smbd smbpasswd pdbedit setfacl getfacl; do
  expect_ne "必需工具 $k 在能力里（缺了就该 blocked）" "" \
    "$(sget cap "d[\"data\"][\"capability\"][\"tools\"].get(\"$k\") or \"\"")"
done

# 配置：可读性、路径、include 现状——每一条都有独立的对照。
expect_eq "config_path 是固定的绝对路径" "/etc/samba/smb.conf" \
  "$(sget cap 'd["data"]["capability"]["config_path"]')"
expect_eq "config_readable 与 test -r 一致" "$([ -r "$SMB_CONF" ] && echo True || echo False)" \
  "$(sget cap 'bool(d["data"]["capability"]["config_readable"])')"
expect_eq "include_ready 与 test -d 一致" "$([ -d "$SMB_INCLUDE_DIR" ] && echo True || echo False)" \
  "$(sget cap 'bool(d["data"]["capability"]["include_ready"])')"
expect_eq "include_present 与 smb.conf 里那一行一致" \
  "$([ "$(smb_conf_include_lines)" -gt 0 ] && echo True || echo False)" \
  "$(sget cap 'bool(d["data"]["capability"]["include_present"])')"
expect_eq "配置写入在本阶段是 unsupported" "unsupported" \
  "$(sget cap 'd["data"]["capability"]["config_write"]')"

# 提示码也是闭集：不许出现没有 message 的条目。
expect_eq "能力里的 notices 都有 message" "True" \
  "$(sget cap 'all(bool(n.get("message")) for n in (d["data"]["capability"].get("notices") or []))')"
if [ "$([ -d "$SMB_INCLUDE_DIR" ] && echo True || echo False)" = "False" ]; then
  expect_contains "include 目录不存在时给出 INCLUDE_DIR_MISSING" "INCLUDE_DIR_MISSING" \
    "$(sget cap '",".join(n["code"] for n in (d["data"]["capability"].get("notices") or []))')"
else
  notrun "include 目录不存在时给出 INCLUDE_DIR_MISSING" "$SMB_INCLUDE_DIR 已存在（上一轮遗留？）"
fi
expect_eq "能力采集时间是 RFC3339（不是零值）" "True" \
  "$(sget cap 'bool(d["data"]["capability"].get("collected_at")) and not d["data"]["capability"]["collected_at"].startswith("0001")')"

expect_eq "账号列表可读" "200" "$(get_accounts cap-accounts)"
expect_eq "账号列表与共享列表带的是同一份能力" \
  "$(sget cap 'd["data"]["capability"]["status"]')" \
  "$(aget cap-accounts 'd["data"]["capability"]["status"]')"
expect_eq "账号列表此刻是空数组" "True" "$(aget cap-accounts 'd["data"]["accounts"] == []')"

echo
echo "== 3. 夹具：一块空盘走产品的 GPT → 分区 → ext4 → 导入 → 挂载；另一块只导入 =="
# 共享只能建在**已挂载**的受管卷里（未挂载时共享路径会落到系统盘的空目录上，
# 那是 PLAN.md 第 10 节明令禁止的状态）。所以这里必须先有一个真的卷。
PART_NAME=data
run_action storage.partition_table.initialize \
  "{\"device_id\":\"$BLANK_A\",\"table_type\":\"gpt\"}" init-a
expect_eq "初始化分区表预检允许" "True" "$PRE_ALLOWED"
expect_verified "初始化分区表"
run_action storage.partition.create \
  "{\"device_id\":\"$BLANK_A\",\"start_bytes\":\"$PART_START\",\"size_bytes\":\"$PART_MAIN\",\"name\":\"$PART_NAME\"}" create-a
expect_verified "建分区"
sync_ok=0
_i=0
while [ "$_i" -lt 40 ]; do
  [ -b "/dev/${BLANK_A}1" ] && { sync_ok=1; break; }
  _i=$((_i + 1)); sleep 0.2
done
expect_eq "独立证据：内核看到了 ${BLANK_A}1" "1" "$sync_ok"
run_action storage.filesystem.format \
  "{\"device_id\":\"${BLANK_A}1\",\"filesystem\":\"ext4\",\"label\":\"$LABEL_MAIN\"}" format-a
expect_eq "格式化预检允许" "True" "$PRE_ALLOWED"
expect_verified "格式化"
expect_eq "独立证据：blkid 读出的类型是 ext4" "ext4" "$(fs_type "/dev/${BLANK_A}1")"
expect_eq "独立证据：blkid 读出的卷标" "$LABEL_MAIN" "$(fs_label "/dev/${BLANK_A}1")"
UUID_A=$(fs_uuid "/dev/${BLANK_A}1")
expect_ne "独立证据：UUID 非空" "" "$UUID_A"

run_action storage.volume.import \
  "{\"device_id\":\"${BLANK_A}1\",\"slug\":\"$SLUG_MAIN\",\"display_name\":\"数据卷\"}" import-a
expect_eq "导入预检允许" "True" "$PRE_ALLOWED"
expect_verified "导入"
VOL_ID_A=$(jobdata volume_id)
MP_A=$VOLUME_ROOT/$SLUG_MAIN
expect_eq "挂载点由 slug 派生" "$MP_A" "$(jobdata mount_path)"

VOL_VER=$(current_vol_version "$VOL_ID_A" m1)
run_action storage.volume.mount "{\"volume_id\":\"$VOL_ID_A\",\"version\":\"$VOL_VER\"}" mount-a
expect_verified "挂载"
expect_eq "内核视图：卷挂上了" "True" "$(is_mounted "$MP_A")"

# 第二块盘只导入、不挂载：它存在的意义是给“未挂载的卷不能建共享”一条真实的现场。
run_action storage.partition_table.initialize \
  "{\"device_id\":\"$BLANK_B\",\"table_type\":\"gpt\"}" init-b
expect_verified "第二块盘初始化分区表"
run_action storage.partition.create \
  "{\"device_id\":\"$BLANK_B\",\"start_bytes\":\"$PART_START\",\"size_bytes\":\"$PART_ALT\",\"name\":\"logs\"}" create-b
expect_verified "第二块盘建分区"
_i=0
while [ "$_i" -lt 40 ]; do
  [ -b "/dev/${BLANK_B}1" ] && break
  _i=$((_i + 1)); sleep 0.2
done
run_action storage.filesystem.format \
  "{\"device_id\":\"${BLANK_B}1\",\"filesystem\":\"ext4\",\"label\":\"$LABEL_ALT\"}" format-b
expect_verified "第二块盘格式化"
run_action storage.volume.import \
  "{\"device_id\":\"${BLANK_B}1\",\"slug\":\"$SLUG_ALT\",\"display_name\":\"日志卷\"}" import-b
expect_verified "第二个卷导入"
VOL_ID_B=$(jobdata volume_id)
MP_B=$VOLUME_ROOT/$SLUG_ALT
expect_eq "第二个卷确实没挂载（这是故意的）" "False" "$(is_mounted "$MP_B")"
expect_eq "第二个卷的 observed_state 不是 mounted" "True" \
  "$(volume "$VOL_ID_B" b-state >/dev/null; volget b-state 'd["data"]["observed_state"] != "mounted"')"

echo
echo "== 4. 秘密通道：只进不出，明文只经过一次 =="
# 密码在本脚本开头就地随机生成，只落在 root 0600 的文件里。这样“扫描 argv 里有没有
# 密码”这件事本身不会把密码带进 argv（grep 用 -f 文件）。
umask 077
# 两个密码文件：pw.txt 用于第一个账号，pw2.txt 用于第二个。文件名固定，内容随机，
# 目录 0700 —— 明文只活在这两个文件里，收尾时 shred 掉。
PW_FILE=$SECRETS_DIR/pw.txt
PW2_FILE=$SECRETS_DIR/pw2.txt
head -c 24 /dev/urandom | base64 | tr -d '\n' >"$PW_FILE"
head -c 24 /dev/urandom | base64 | tr -d '\n' >"$PW2_FILE"
chmod 600 "$PW_FILE" "$PW2_FILE"
PW_LEN=$(wc -c <"$PW_FILE" | tr -d ' ')
expect_ne "随机密码已生成（长度 $PW_LEN）" "0" "$PW_LEN"

# 权限边界：viewer 一个都读不到，也存不进秘密。
VIEWER_CODE=$(api "$JAR" "$CSRF" POST /api/v1/users \
  "{\"username\":\"$VIEWER_USER\",\"display_name\":\"看客\",\"role\":\"viewer\",\"initial_password\":\"$VIEWER_PW\"}")
expect_eq "创建 viewer 账号" "201" "$VIEWER_CODE"
VJAR=$WORK/viewer.jar
expect_eq "viewer 登录" "200" "$(login "$VIEWER_USER" "$VIEWER_PW" "$VJAR" login-viewer)"
VCSRF=$(csrf_of "$WORK/login-viewer.json")
expect_eq "viewer 读共享列表是 403" "403" "$(api "$VJAR" "$VCSRF" GET "$SHARES_PATH")"
expect_eq "viewer 读账号列表是 403" "403" "$(api "$VJAR" "$VCSRF" GET "$ACCOUNTS_PATH")"
expect_eq "viewer 读共享详情是 403" "403" \
  "$(api "$VJAR" "$VCSRF" GET "$SHARES_PATH/shr_00000000000000000000000000000000")"
expect_eq "viewer 存秘密是 403" "403" \
  "$(api "$VJAR" "$VCSRF" POST "$SECRETS_PATH" "{\"purpose\":\"smb_password\",\"value\":\"x\"}")"
expect_eq "未登录读共享列表是 401" "401" \
  "$(curl -s -o /dev/null -w '%{http_code}' -H "Origin: $ORIGIN" "$BASE$SHARES_PATH")"

# put_secret <用途> <密码文件> <引用>：请求体走文件，明文不进 argv。
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
  # 请求体里的明文用完就抹掉：留档的 payloads 里只放响应。
  shred -u "$SECRETS_DIR/req.json" 2>/dev/null || rm -f "$SECRETS_DIR/req.json"
  printf '%s' "$_code"
}
secget() { jget "$WORK/secret.json" "$1"; }

# “未再认证不能存秘密”那条已经在 §1 测过了（那时会话确实还没有再认证）。
# 这里不能重测：§3 的磁盘动作已经把这次会话再认证掉，现在再发同一个请求只会 201，
# 断言 403 就会失败——而那失败与被测功能无关，是测试顺序的问题。

# 形状先在 Web 层挡住：不合形状的用例连代理都不用问。
expect_eq "用途名不合形状是 400" "400" \
  "$(api "$JAR" "$CSRF" POST "$SECRETS_PATH" '{"purpose":"Bad-Purpose","value":"x"}')"
expect_eq "空内容是 400" "400" \
  "$(api "$JAR" "$CSRF" POST "$SECRETS_PATH" '{"purpose":"smb_password","value":""}')"
expect_eq "多一个字段是 400" "400" \
  "$(api "$JAR" "$CSRF" POST "$SECRETS_PATH" '{"purpose":"smb_password","value":"x","extra":1}')"

expect_eq "再认证" "200" "$(reauth)"
REAUTH_CHECK=$(jget "$WORK/resp.json" 'd["data"]["reauth_until"] if "reauth_until" in d["data"] else True')
note "再认证有效期：$REAUTH_CHECK"

# 闭集之外 / 内部专用的用途，代理必须拒——Web 层不复制枚举，所以这条错误要能从
# 代理原样透传上来（400，不是 500）。
expect_eq "内部用途 totp_seed 被代理拒绝（400 透传）" "400" \
  "$(put_secret totp_seed "$PW_FILE" "")"
expect_eq "闭集外的用途被代理拒绝（400 透传）" "400" \
  "$(put_secret not_a_purpose "$PW_FILE" "")"

expect_eq "存 smb_password 成功" "201" "$(put_secret smb_password "$PW_FILE" "")"
cp "$WORK/secret.json" "$PAYLOADS/secret-create.json"
SECRET_REF=$(secget 'd["data"]["secret_ref"]')
expect_eq "secret_ref 是 sec_ 加 32 位小写十六进制" "True" \
  "$(python3 -c 'import re,sys; print(bool(re.fullmatch(r"sec_[0-9a-f]{32}", sys.argv[1])))' "$SECRET_REF" 2>/dev/null)"
expect_eq "用途回显是 smb_password" "smb_password" "$(secget 'd["data"]["purpose"]')"
expect_eq "临时秘密带 expires_at（10 分钟后自己消失）" "True" \
  "$(secget 'bool(d["data"].get("expires_at"))')"
expect_eq "响应体里没有明文" "0" \
  "$(grep -a -c -F -f "$PW_FILE" "$WORK/secret.json" 2>/dev/null || true)"

# 审计：写秘密这件事要留痕，但**不留内容**。secret_ref 这个键本身也会被替换成
# [redacted]——引用不是密码，可它是“去解哪一份密文”的线索，按秘密处理更稳妥。
AUDIT_SECRET=$(db_query "$WEB_DB" "select action || '|' || target || '|' || outcome || '|' || redacted_details from audit_events where action = 'secret.create' order by id desc limit 1")
expect_contains "审计里有一条成功的 secret.create" "secret.create|secret:smb_password" "$AUDIT_SECRET"
expect_contains "审计详情里记了用途" "smb_password" "$AUDIT_SECRET"
expect_contains "审计详情里的 secret_ref 被替换成 [redacted]" "[redacted]" "$AUDIT_SECRET"
expect_not_contains "审计详情里没有引用原文" "$SECRET_REF" "$AUDIT_SECRET"
expect_eq "审计详情里也没有明文" "0" \
  "$(printf '%s' "$AUDIT_SECRET" | grep -a -c -F -f "$PW_FILE" 2>/dev/null || true)"

echo
echo "== 5. 受管理账号：真的建出来，并用 getent / pdbedit 两条独立读回 =="
# 先钉住“空名单里没有密码字段”：DTO 上没有，数据库里也没有。
expect_eq "managed_accounts 表里没有任何密码/哈希列" "True" \
  "$(python3 -c '
import sqlite3, sys
con = sqlite3.connect(sys.argv[1])
cols = [r[1].lower() for r in con.execute("PRAGMA table_info(managed_accounts)")]
bad = [c for c in cols
       if ("password" in c and c != "has_password") or "hash" in c or "passwd" in c]
print(bad == [])' "$AGENT_DB" 2>/dev/null)"

run_action share.account.apply \
  "{\"account_id\":null,\"name\":\"$ACCT_RW\",\"secret_ref\":\"$SECRET_REF\",\"disabled\":false}" acct-create-rw
expect_eq "建账号预检允许" "True" "$PRE_ALLOWED"
expect_eq "预检目标显示名就是账号名" "$ACCT_RW" "$(pstate)"
expect_eq "建账号要求再认证" "True" "$PRE_REAUTH"
expect_eq "建账号没有确认短语" "" "$PRE_PHRASE"
expect_eq "建账号的影响里有 access_change" "True" \
  "$(case "$(pimpact)" in *access_change*) echo True ;; *) echo False ;; esac)"
expect_eq "锁是 account:<名字>" "account:$ACCT_RW" "$(plocks)"
expect_verified "建受管理账号"
ACCT_RW_ID=$(jobdata account_id)
expect_eq "账号标识是 sac_ 形状" "sac_" "$(printf '%s' "$ACCT_RW_ID" | cut -c1-4)"

# observed_state 逐条：这些数字必须是产品**读回来**的，不是它传进去的期望值。
expect_eq "observed：samba_account 为真" "True" "$(jobobserved samba_account)"
expect_eq "observed：password_set 为真" "True" "$(jobobserved password_set)"
expect_eq "observed：secret 已转成持久引用" "True" "$(jobobserved secret_persisted)"
expect_eq "observed：shell 是 nologin" "/usr/sbin/nologin" "$(jobobserved shell)"
expect_eq "observed：home 是 /nonexistent" "/nonexistent" "$(jobobserved home)"
OBS_UID=$(jobobserved uid)
expect_eq "observed：uid 落在系统段（<1000）" "True" \
  "$(python3 -c 'import sys; print(int(sys.argv[1]) < 1000)' "$OBS_UID" 2>/dev/null)"
expect_contains "observed：Linux 密码是锁定的（L 或 NP）" "L" "$(jobobserved linux_password)"
expect_ne "observed：Password last set 有值" "" "$(jobobserved password_last_set)"

# 独立读回一：getent passwd。
expect_eq "独立证据：getent passwd 里有 $ACCT_RW" "True" \
  "$([ -n "$(passwd_line "$ACCT_RW")" ] && echo True || echo False)"
expect_eq "独立证据：uid 与产品报的一致" "$OBS_UID" "$(passwd_uid "$ACCT_RW")"
expect_eq "独立证据：shell 是 nologin" "/usr/sbin/nologin" "$(passwd_shell "$ACCT_RW")"
expect_eq "独立证据：home 是 /nonexistent" "/nonexistent" "$(passwd_home "$ACCT_RW")"
expect_ne "独立证据：Linux 密码不是可用的（passwd -S 不是 P）" "P" "$(passwd_status "$ACCT_RW")"

# 独立读回二：Samba 自己的工具。
expect_eq "独立证据：pdbedit -L 里有 $ACCT_RW" "True" "$(pdb_has "$ACCT_RW")"
expect_ne "独立证据：passdb 里有 Password last set" "" "$(pdb_password_set "$ACCT_RW")"
expect_not_contains "独立证据：Account Flags 里没有 D（账号是启用的）" "D" "$(pdb_flags "$ACCT_RW")"

# 明文不外泄：四个地方全扫一遍（都用 -f 文件，避免检查者自己带上明文）。
expect_eq "Web 日志里没有明文" "0" "$(hits_in_file "$WEB_LOG")"
expect_eq "代理日志里没有明文" "0" "$(hits_in_file "$AGENT_LOG")"
expect_eq "两个数据库（含 WAL）里没有明文" "0" "$(hits_in_databases)"
expect_eq "任何进程的 argv/environ 里没有明文" "0" "$(hits_in_processes)"

# 第二个账号（只读）：ACL 里要有两种不同的权限位，这是“为什么必须用 ACL”的现场。
expect_eq "存第二个密码成功" "201" "$(put_secret smb_password "$PW2_FILE" "")"
SECRET_REF2=$(secget 'd["data"]["secret_ref"]')
run_action share.account.apply \
  "{\"account_id\":null,\"name\":\"$ACCT_RO\",\"secret_ref\":\"$SECRET_REF2\",\"disabled\":false}" acct-create-ro
expect_verified "建第二个受管理账号"
ACCT_RO_ID=$(jobdata account_id)
expect_eq "独立证据：pdbedit -L 里有 $ACCT_RO" "True" "$(pdb_has "$ACCT_RO")"

# 载荷形状：账号列表里没有 secret_ref、没有密码哈希。
expect_eq "账号列表可读" "200" "$(get_accounts two)"
cp "$WORK/accounts-two.json" "$PAYLOADS/page-accounts-two.json"
expect_eq "账号列表里有两条" "2" "$(aget two 'len(d["data"]["accounts"])')"
expect_eq "账号记录里没有 secret_ref 字段" "False" \
  "$(aget two 'any("secret_ref" in a for a in d["data"]["accounts"])')"
expect_eq "账号记录里没有任何密码/哈希字段" "False" \
  "$(aget two 'any(k in a for a in d["data"]["accounts"] for k in a if "password" in k and k != "has_password")')"
expect_eq "两个账号都报 has_password=true" "True" \
  "$(aget two 'all(bool(a["has_password"]) for a in d["data"]["accounts"])')"
expect_eq "账号记录里没有明文" "0" \
  "$(grep -a -c -F -f "$PW_FILE" "$WORK/accounts-two.json" 2>/dev/null || true)"

# 拒绝路径（账号）。
# 名字已被一条受管理记录占着：secret_ref 给一个**形状合法**的引用
# （形状合法但不存在的秘密只会在执行时被发现），这样阻塞项里只剩“名字已被占用”。
blocked share.account.apply '{"account_id":null,"name":"'"$ACCT_RW"'","secret_ref":"sec_00000000000000000000000000000000","disabled":false}' ACCOUNT_NAME_TAKEN
blocked share.account.apply \
  "{\"account_id\":null,\"name\":\"root\",\"secret_ref\":\"$SECRET_REF\",\"disabled\":false}" ACCOUNT_NAME_INVALID
blocked share.account.apply \
  '{"account_id":null,"name":"Bad_Name","secret_ref":"","disabled":false}' ACCOUNT_NAME_INVALID
# 新建账号不带 secret_ref：没有密码的 Samba 账号无法认证，预检阶段就该拦。
blocked share.account.apply \
  '{"account_id":null,"name":"nopassword","secret_ref":"","disabled":false}' SECRET_INVALID
# secret_ref 直接放明文：这不是“参数不合法”而是“密码走了不该走的路”，
# 必须在参数解码阶段就被挡下来（400），不能一路走到执行器。
expect_eq "secret_ref 里直接放明文是 400" "400" \
  "$(preflight share.account.apply '{"account_id":null,"name":"rawpass","secret_ref":"hunter2","disabled":false}')"
expect_contains "拒绝理由说明它要的是 sec_ 引用" "sec_" \
  "$(jget "$WORK/preflight.json" 'd["error"]["message"]')"
# 系统上已经有一个不是面板建的账号：必须拒绝静默接管。
#
# 夹具要自己造，不能“找一台机器上现成那个 uid≥1000 的账号”：真机上挑中的是
# upanel，而 upanel* 是保留名，判决会（正确地）先报 ACCOUNT_NAME_INVALID，
# 于是一条好好的断言红得莫名其妙。这里临时建一个名字合法、系统里真的存在、
# 但不是面板建的账号，用完就删，删完核对它真的没了。
PROBE_SYS_USER=p08sysprobe
userdel "$PROBE_SYS_USER" >/dev/null 2>&1 || true
if useradd --system --no-create-home --shell /usr/sbin/nologin "$PROBE_SYS_USER" 2>>"$WORK/logs/probe-useradd.err"; then
  pass "夹具：临时系统账号 $PROBE_SYS_USER 已建（不是面板建的）"
  blocked share.account.apply \
    "{\"account_id\":null,\"name\":\"$PROBE_SYS_USER\",\"secret_ref\":\"$SECRET_REF2\",\"disabled\":false}" ACCOUNT_NOT_MANAGED
  userdel "$PROBE_SYS_USER" >/dev/null 2>&1 || true
  expect_eq "夹具：临时系统账号已清掉" "" "$(getent passwd "$PROBE_SYS_USER")"
else
  fail "夹具：临时系统账号 $PROBE_SYS_USER 已建" "$(tail -2 "$WORK/logs/probe-useradd.err" 2>/dev/null)"
  notrun "系统已有同名账号时拒绝接管" "夹具账号建不出来"
fi

# 禁用 / 启用：不用再上传密码，因此要有一条 ACCOUNT_PASSWORD_KEPT 告警。
run_action share.account.apply \
  "{\"account_id\":\"$ACCT_RO_ID\",\"name\":\"$ACCT_RO\",\"secret_ref\":\"\",\"disabled\":true}" acct-disable-ro
expect_eq "改禁用状态要求再认证（改的是认证凭据）" "True" "$PRE_REAUTH"
expect_contains "改禁用状态时提示密码沿用上次的" "ACCOUNT_PASSWORD_KEPT" "$(pwarn)"
expect_verified "禁用受管理账号"
expect_eq "observed：disabled 为真" "True" "$(jobobserved disabled)"
expect_eq "observed：这次没有换密码" "False" "$(jobobserved password_changed)"
expect_contains "独立证据：Account Flags 里出现 D" "D" "$(pdb_flags "$ACCT_RO")"

run_action share.account.apply \
  "{\"account_id\":\"$ACCT_RO_ID\",\"name\":\"$ACCT_RO\",\"secret_ref\":\"\",\"disabled\":false}" acct-enable-ro
expect_verified "启用受管理账号"
# 空串里当然没有 D。先证 pdbedit 这次真的读到了东西，再证 D 不见了，
# 否则“启用成功”的证据就是一句空话。
ACCT_RO_FLAGS=$(pdb_flags "$ACCT_RO")
expect_ne "独立证据：读得到 Account Flags" "" "$ACCT_RO_FLAGS"
expect_not_contains "独立证据：Account Flags 里的 D 没了" "D" "$ACCT_RO_FLAGS"
ACCT_RO_VER=$(jobdata version)

echo
echo "== 6. 共享定义：目录与 POSIX ACL 真的做出来，但结果只能是 accepted =="
DEF_BODY=$(python3 -c '
import json, sys
name, vol, rel, rw, ro = sys.argv[1:6]
print(json.dumps({"share_id": None, "version": None, "definition": {
    "name": name, "protocol": "smb", "volume_id": vol, "relative_path": rel,
    "enabled": True, "read_only": False,
    "allowed_cidrs": ["192.168.56.0/24"],
    "smb_users": [{"account_id": rw, "access": "write"}, {"account_id": ro, "access": "read"}],
}}))' "$SHARE_NAME" "$VOL_ID_A" "$REL_PATH" "$ACCT_RW_ID" "$ACCT_RO_ID")
SHARE_DIR=$MP_A/$REL_PATH

run_action share.apply "$DEF_BODY" share-apply
expect_eq "共享预检允许" "True" "$PRE_ALLOWED"
expect_eq "预检目标显示名是共享名" "$SHARE_NAME" "$(pstate)"
expect_eq "共享预检要求再认证" "True" "$PRE_REAUTH"
expect_eq "共享预检没有确认短语（可逆）" "" "$PRE_PHRASE"
expect_contains "影响里有 config_change" "config_change" "$(pimpact)"
expect_contains "影响里有 access_change" "access_change" "$(pimpact)"
expect_contains "锁里有共享名" "share:$SHARE_NAME" "$(plocks)"
expect_contains "锁里有卷" "volume:$VOL_ID_A" "$(plocks)"
# 最关键的一条告警：本阶段不写配置。它必须每次都在。
expect_contains "预检带 SHARE_CONFIG_NOT_APPLIED 告警" "SHARE_CONFIG_NOT_APPLIED" "$(pwarn)"
expect_eq "预检的阻塞列表是空的（告警不是阻塞）" "" "$(pall)"

expect_accepted "应用共享"
SHARE_ID=$(jobdata share_id)
SHARE_VER=$(jobdata version)
CONFIG_HASH=$(jobdata config_hash)
CONFIG_PREVIEW=$(jobdata config_preview)
expect_eq "共享标识是 shr_ 形状" "shr_" "$(printf '%s' "$SHARE_ID" | cut -c1-4)"
expect_eq "新建共享的版本是 1" "1" "$SHARE_VER"
expect_eq "config_hash 是 64 位十六进制" "True" \
  "$(python3 -c 'import re,sys; print(bool(re.fullmatch(r"[0-9a-f]{64}", sys.argv[1])))' "$CONFIG_HASH" 2>/dev/null)"

# 本阶段的诚实性，逐条钉死。
expect_eq "observed：samba_config = not_written" "not_written" "$(jobobserved samba_config)"
expect_eq "observed：served = false" "False" "$(jobobserved served)"
expect_eq "observed：directory_prepared = true" "True" "$(jobobserved directory_prepared)"
expect_eq "observed：acl_verified = true" "True" "$(jobobserved acl_verified)"
expect_eq "observed：directory 落在卷的挂载点里" "$SHARE_DIR" "$(jobobserved directory)"
expect_eq "observed：mount_path 就是卷的挂载点" "$MP_A" "$(jobobserved mount_path)"
expect_contains "recovery_hint 说清还差什么" "P08.2" "$(jrecovery)"

# 配置版本 = 渲染结果的 sha256。这个等式由脚本自己算一遍，不用产品的代码。
# 注意：**不能**把 config_preview 先取进 shell 变量再算——`$( )` 会吃掉结尾的换行，
# 而渲染结果以换行结尾（sha256 差得干干净净：8e09e9… vs 3258aa…）。所以直接读
# JSON 里那个字段，边界与产品看到的一模一样。
HASH_INDEP=$(python3 -c 'import hashlib, json, sys
with open(sys.argv[1], encoding="utf-8") as f:
    d = json.load(f)
print(hashlib.sha256(d["data"]["result"]["data"]["config_preview"].encode()).hexdigest())' \
  "$WORK/job.json" 2>/dev/null)
expect_eq "config_hash 就是 config_preview 的 sha256（独立算一遍）" "$CONFIG_HASH" "$HASH_INDEP"
cp "$WORK/job.json" "$PAYLOADS/job-share-apply.json"

# 渲染出来的段落逐键核对：这一份是 P08.2 要写进配置的东西，现在只是没写。
for want in "[$SHARE_NAME]" "path = $SHARE_DIR" "guest ok = no" \
            "read only = no" "follow symlinks = no" "wide links = no" \
            "valid users = " "write list = " "hosts allow = 192.168.56.0/24"; do
  expect_contains "配置预览里有 $want" "$want" "$CONFIG_PREVIEW"
done
expect_contains "valid users 里有两个账号" "$ACCT_RW" "$CONFIG_PREVIEW"
expect_contains "valid users 里也有只读账号" "$ACCT_RO" "$CONFIG_PREVIEW"
expect_eq "write list 里只有读写账号（只读账号不在里面）" "$ACCT_RW" \
  "$(printf '%s\n' "$CONFIG_PREVIEW" | sed -n 's/^ *write list = //p')"

# ---------- 独立证据：目录与 ACL ----------
expect_eq "独立证据：共享目录真的建出来了" "True" \
  "$([ -d "$SHARE_DIR" ] && echo True || echo False)"
expect_eq "独立证据：目录在卷的文件系统上（不是系统盘）" "$MP_A" "$(dir_mountpoint "$SHARE_DIR")"
expect_eq "独立证据：目录对 world 没有任何权限" "0" "$(dir_other_bits "$SHARE_DIR")"
ACL_DUMP=$WORK/logs/getfacl-share.txt
acl_of "$SHARE_DIR" >"$ACL_DUMP"
cp "$ACL_DUMP" "$PAYLOADS/getfacl-share.txt"
note "getfacl $SHARE_DIR："
while IFS= read -r l; do note "  $l"; done <"$ACL_DUMP"
expect_eq "独立证据：属主是 rwx" "rwx" "$(acl_field "$SHARE_DIR" 'user::')"
expect_eq "独立证据：组是 ---（不靠同名组放行）" "---" "$(acl_field "$SHARE_DIR" 'group::')"
expect_eq "独立证据：其他人是 ---" "---" "$(acl_field "$SHARE_DIR" 'other::')"
expect_eq "独立证据：读写账号拿到 rwx" "rwx" "$(acl_entry "$SHARE_DIR" "$ACCT_RW")"
expect_eq "独立证据：只读账号拿到 r-x" "r-x" "$(acl_entry "$SHARE_DIR" "$ACCT_RO")"
ACL_MASK=$(acl_field "$SHARE_DIR" 'mask::')
expect_eq "独立证据：mask 覆盖了授权（rwx）" "rwx" "$ACL_MASK"
expect_eq "产品报的 mask 与 getfacl 读回的一致" "$ACL_MASK" "$(jobobserved acl_mask)"
expect_eq "ACL 里只有这两条具名条目（没有上一轮的残留）" "2" \
  "$(acl_of "$SHARE_DIR" | grep -c '^user:[^:]')"

# ---------- 独立证据：配置**没有**被写 ----------
expect_eq "独立证据：smb.conf 的 sha256 逐字节未变" "$SMB_CONF_SHA0" "$(smb_conf_sha)"
expect_eq "独立证据：testparm 不认识这条共享（它确实没被提供）" "0" \
  "$(smb_served_sections "$SHARE_NAME")"
expect_eq "独立证据：smb.conf 里仍然没有托管 include 行" "0" "$(smb_conf_include_lines)"

echo
echo "== 7. 页面依赖的载荷：列表 / 详情 / config_applied 恒为 false =="
expect_eq "共享列表可读" "200" "$(get_shares one)"
cp "$WORK/shares-one.json" "$PAYLOADS/page-shares-one.json"
expect_eq "列表里有一条共享" "1" "$(sget one 'len(d["data"]["shares"])')"
expect_eq "共享名与定义一致" "$SHARE_NAME" "$(sget one 'd["data"]["shares"][0]["definition"]["name"]')"
expect_eq "directory_prepared 为真（目录与 ACL 已核验）" "True" \
  "$(sget one 'bool(d["data"]["shares"][0]["directory_prepared"])')"
expect_eq "config_applied 恒为 false（P08.2 才会变真）" "False" \
  "$(sget one 'bool(d["data"]["shares"][0]["config_applied"])')"
expect_eq "列表里每一条的 config_applied 都是 false" "True" \
  "$(sget one 'all(s["config_applied"] is False for s in d["data"]["shares"])')"
expect_eq "config_hash 与任务结果一致" "$CONFIG_HASH" \
  "$(sget one 'd["data"]["shares"][0]["config_hash"]')"
expect_eq "定义里的 allowed_cidrs 原样带回" "192.168.56.0/24" \
  "$(sget one 'd["data"]["shares"][0]["definition"]["allowed_cidrs"][0]')"
expect_eq "定义里的两条授权都带回" "2" \
  "$(sget one 'len(d["data"]["shares"][0]["definition"]["smb_users"])')"

expect_eq "共享详情可读" "200" "$(get_share "$SHARE_ID" detail)"
cp "$WORK/share-detail.json" "$PAYLOADS/page-share-detail.json"
expect_eq "详情里的 id 与列表一致" "$SHARE_ID" "$(shget detail 'd["data"]["share"]["id"]')"
expect_eq "详情也带能力（页面不必发两次请求）" "available" \
  "$(shget detail 'd["data"]["capability"]["status"]')"
expect_eq "详情的 config_applied 也是 false" "False" \
  "$(shget detail 'bool(d["data"]["share"]["config_applied"])')"

expect_eq "形状不对的共享标识是 400（连代理都不问）" "400" \
  "$(get_share 'not-an-id' bad-shape)"
expect_eq "格式对但不存在的共享是 404" "404" \
  "$(get_share 'shr_00000000000000000000000000000000' missing)"
expect_eq "404 的响应体里带着那个 id" "True" \
  "$(jget "$WORK/share-missing.json" "'shr_00000000000000000000000000000000' in d[\"error\"][\"message\"]")"

# 数据库层的同一件事：没有任何一条记录被标成“配置已写入”。
expect_eq "代理库里没有一条 config_applied=1 的共享" "0" \
  "$(db_query "$AGENT_DB" 'select count(*) from shares where config_applied=1')"
expect_eq "代理库里这条共享的 config_hash 与接口一致" "$CONFIG_HASH" \
  "$(db_query "$AGENT_DB" "select config_hash from shares where name='$SHARE_NAME'")"
expect_eq "代理库里 directory_prepared=1 且 config_applied=0" "1|0" \
  "$(db_query "$AGENT_DB" "select directory_prepared, config_applied from shares where name='$SHARE_NAME'")"

echo
echo "== 8. 拒绝路径：该拒的拒，理由写给页面看 =="
# 每一条都是一个真实的坑。判决必须在**预检**阶段给出，而不是等执行到一半失败。
NFS_BODY=$(printf '%s' "$DEF_BODY" | python3 -c '
import json,sys
d=json.load(sys.stdin); d["definition"]["protocol"]="nfs"; print(json.dumps(d))')
blocked share.apply "$NFS_BODY" PROTOCOL_UNSUPPORTED

DOTDOT_BODY=$(printf '%s' "$DEF_BODY" | python3 -c '
import json,sys
d=json.load(sys.stdin); d["definition"]["relative_path"]="a/../../etc"; print(json.dumps(d))')
blocked share.apply "$DOTDOT_BODY" SHARE_PATH_INVALID

ABS_BODY=$(printf '%s' "$DEF_BODY" | python3 -c '
import json,sys
d=json.load(sys.stdin); d["definition"]["relative_path"]="/etc"; print(json.dumps(d))')
blocked share.apply "$ABS_BODY" SHARE_PATH_INVALID

NOUSER_BODY=$(printf '%s' "$DEF_BODY" | python3 -c '
import json,sys
d=json.load(sys.stdin); d["definition"]["smb_users"]=[]; print(json.dumps(d))')
blocked share.apply "$NOUSER_BODY" SHARE_NO_USERS

MISSING_ACCT_BODY=$(printf '%s' "$DEF_BODY" | python3 -c '
import json,sys
d=json.load(sys.stdin)
d["definition"]["name"]="othershare"
d["definition"]["smb_users"]=[{"account_id":"sac_00000000000000000000000000000000","access":"read"}]
print(json.dumps(d))')
blocked share.apply "$MISSING_ACCT_BODY" ACCOUNT_MISSING

DUPLICATE_BODY=$(printf '%s' "$DEF_BODY" | python3 -c '
import json,sys
d=json.load(sys.stdin); print(json.dumps(d))')
blocked share.apply "$DUPLICATE_BODY" SHARE_NAME_TAKEN

# 未挂载的卷：定义再合法也不能建——否则共享路径会落到系统盘的空目录上。
UNMOUNTED_BODY=$(printf '%s' "$DEF_BODY" | python3 -c '
import json,sys
d=json.load(sys.stdin)
d["definition"]["name"]="unmounted"
d["definition"]["volume_id"]=sys.argv[1]
print(json.dumps(d))' "$VOL_ID_B")
blocked share.apply "$UNMOUNTED_BODY" VOLUME_NOT_MOUNTED

NO_VOL_BODY=$(printf '%s' "$DEF_BODY" | python3 -c '
import json,sys
d=json.load(sys.stdin)
d["definition"]["name"]="novol"
d["definition"]["volume_id"]="vol_00000000000000000000000000000000"
print(json.dumps(d))')
blocked share.apply "$NO_VOL_BODY" VOLUME_NOT_FOUND

# 符号链接：卷里放一个指向 /etc 的链接，共享路径必须被拒（PLAN.md 第 10 节）。
ln -sfn /etc "$MP_A/escape" 2>/dev/null
SYMLINK_BODY=$(printf '%s' "$DEF_BODY" | python3 -c '
import json,sys
d=json.load(sys.stdin)
d["definition"]["name"]="escapes"
d["definition"]["relative_path"]="escape"
print(json.dumps(d))')
blocked share.apply "$SYMLINK_BODY" SHARE_PATH_SYMLINK

# 版本对不上：页面拿着旧版本提交时必须被拒，不许静默覆盖别人的改动。
STALE_BODY=$(printf '%s' "$DEF_BODY" | python3 -c '
import json,sys
d=json.load(sys.stdin)
d["share_id"]=sys.argv[1]; d["version"]="999"
print(json.dumps(d))' "$SHARE_ID")
blocked share.apply "$STALE_BODY" SHARE_VERSION_MISMATCH

# 被共享引用的账号不能删：删掉会让这条共享的 valid users 指向不存在的人。
blocked share.account.delete "{\"account_id\":\"$ACCT_RW_ID\"}" ACCOUNT_HAS_SHARES

echo
echo "== 9. 禁用 / 改回 / 删除：ACL 撤销、目录保留、记录消失 =="
printf '%s\n' "共享里的文件" >"$SHARE_DIR/note.txt" 2>>"$WORK/logs/note-write.err"
NOTE_SHA=$(sha256sum "$SHARE_DIR/note.txt" 2>/dev/null | cut -d' ' -f1)
# 文件必须先真的写进去。空串与空串相等——不加这一条，“文件还在、内容没变”会在
# 共享目录根本不存在的时候也报通过（真机上一遍就是这么骗过自己的：目录没建出来，
# 后面两条 sha256 断言都“通过”了）。
expect_eq "前置：共享目录里能写文件（摘要是 64 位十六进制）" "64" "${#NOTE_SHA}"

run_action share.disable "{\"share_id\":\"$SHARE_ID\",\"version\":\"$SHARE_VER\"}" share-disable
expect_eq "禁用预检允许" "True" "$PRE_ALLOWED"
# 禁用**不**要求再认证：它只撤销授权，可逆（CONTRACTS.md 第 5 节 R 列）。
expect_eq "禁用不要求再认证" "False" "$PRE_REAUTH"
expect_accepted "禁用共享"
# 禁用的结果里 version 在 observed_state（data 只回 share_id 与 directory）：
# 下一次变更必须用这个新版本，否则就是“拿旧版本覆盖别人的改动”。
SHARE_VER=$(jobobserved version)
expect_eq "observed：ACL 已撤销" "True" "$(jobobserved acl_revoked)"
expect_eq "observed：目录被保留" "True" "$(jobobserved directory_kept)"
expect_eq "独立证据：ACL 上没有具名账号了" "False" "$(acl_has_named_users "$SHARE_DIR")"
expect_eq "独立证据：其他人仍然是 ---" "---" "$(acl_field "$SHARE_DIR" 'other::')"
expect_eq "独立证据：目录对 world 仍无权限" "0" "$(dir_other_bits "$SHARE_DIR")"
expect_eq "独立证据：目录里的文件还在，内容没变" "$NOTE_SHA" "$(sha256sum "$SHARE_DIR/note.txt" | cut -d' ' -f1)"
expect_eq "载荷：directory_prepared 回到 false" "False" \
  "$(get_shares dis >/dev/null; sget dis 'bool(d["data"]["shares"][0]["directory_prepared"])')"
expect_eq "载荷：config_applied 仍然是 false" "False" \
  "$(sget dis 'bool(d["data"]["shares"][0]["config_applied"])')"
expect_eq "载荷：定义里的 enabled 已经是 false" "False" \
  "$(sget dis 'bool(d["data"]["shares"][0]["definition"]["enabled"])')"

# 再改回启用：走的是同一条 share.apply 的更新路径（带 share_id + version）。
REENABLE_BODY=$(python3 -c '
import json, sys
name, vol, rel, rw, ro, sid, ver = sys.argv[1:8]
print(json.dumps({"share_id": sid, "version": ver, "definition": {
    "name": name, "protocol": "smb", "volume_id": vol, "relative_path": rel,
    "enabled": True, "read_only": False, "allowed_cidrs": ["192.168.56.0/24"],
    "smb_users": [{"account_id": rw, "access": "write"}, {"account_id": ro, "access": "read"}],
}}))' "$SHARE_NAME" "$VOL_ID_A" "$REL_PATH" "$ACCT_RW_ID" "$ACCT_RO_ID" "$SHARE_ID" "$SHARE_VER")
run_action share.apply "$REENABLE_BODY" share-reenable
expect_eq "改回启用的预检允许" "True" "$PRE_ALLOWED"
expect_accepted "改回启用"
SHARE_VER=$(jobdata version)
expect_eq "版本自增了（1 → 3，两次成功变更）" "3" "$SHARE_VER"
expect_eq "独立证据：ACL 又装回去了" "rwx" "$(acl_entry "$SHARE_DIR" "$ACCT_RW")"
expect_eq "独立证据：只读账号仍然是 r-x" "r-x" "$(acl_entry "$SHARE_DIR" "$ACCT_RO")"
expect_eq "同一份定义两次应用的 config_hash 相同（哈希只看配置内容）" "$CONFIG_HASH" \
  "$(jobdata config_hash)"

run_action share.delete "{\"share_id\":\"$SHARE_ID\",\"version\":\"$SHARE_VER\"}" share-delete
expect_eq "删除预检允许" "True" "$PRE_ALLOWED"
expect_eq "删除要求再认证" "True" "$PRE_REAUTH"
expect_accepted "删除共享"
expect_eq "独立证据：ACL 已被撤销" "False" "$(acl_has_named_users "$SHARE_DIR")"
expect_eq "独立证据：目录没有被删（删共享不删数据）" "True" \
  "$([ -d "$SHARE_DIR" ] && echo True || echo False)"
expect_eq "独立证据：目录里的文件还在" "$NOTE_SHA" "$(sha256sum "$SHARE_DIR/note.txt" | cut -d' ' -f1)"
expect_eq "载荷：共享列表回到空数组" "True" "$(get_shares after-del >/dev/null; sget after-del 'd["data"]["shares"] == []')"
expect_eq "代理库：shares 表里没有这条记录了" "0" \
  "$(db_query "$AGENT_DB" "select count(*) from shares where name='$SHARE_NAME'")"
expect_eq "独立证据：smb.conf 仍然逐字节未变" "$SMB_CONF_SHA0" "$(smb_conf_sha)"

# 现在账号没有引用了，可以删。
run_action share.account.delete "{\"account_id\":\"$ACCT_RW_ID\"}" acct-delete-rw
expect_verified "删除读写账号"
expect_eq "observed：passdb 条目已删" "True" "$(jobobserved samba_removed)"
expect_eq "observed：Linux 账号已删" "True" "$(jobobserved linux_removed)"
expect_eq "独立证据：getent passwd 里没有了" "" "$(passwd_line "$ACCT_RW")"
expect_eq "独立证据：pdbedit -L 里也没有了" "False" "$(pdb_has "$ACCT_RW")"

run_action share.account.delete "{\"account_id\":\"$ACCT_RO_ID\"}" acct-delete-ro
expect_verified "删除只读账号"
expect_eq "独立证据：两个账号都没了" "False True" \
  "$(pdb_has "$ACCT_RO") $([ -z "$(passwd_line "$ACCT_RO")" ] && echo True || echo False)"
expect_eq "载荷：账号列表回到空数组" "True" \
  "$(get_accounts after-del >/dev/null; aget after-del 'd["data"]["accounts"] == []')"
expect_eq "代理库：managed_accounts 表里没有记录了" "0" \
  "$(db_query "$AGENT_DB" 'select count(*) from managed_accounts')"

echo
echo "== 10. 收尾与不变量 =="
stop_web || true
stop_agent || true
if [ ! -S "$SOCK" ]; then pass "代理已停止、socket 已清理"; else fail "代理已停止、socket 已清理" "$SOCK 还在"; fi

# 整轮的不变量：面板从头到尾没有写过 Samba 的配置，也没有创建托管 include 目录。
expect_eq "整轮结束时 smb.conf 仍逐字节未变" "$SMB_CONF_SHA0" "$(smb_conf_sha)"
cp "$SMB_CONF" "$WORK/logs/smb-conf-final.txt" 2>/dev/null || true
cp "$FSTAB" "$WORK/logs/fstab-final.txt"
lsblk -o NAME,SIZE,FSTYPE,LABEL,UUID,MOUNTPOINTS >"$WORK/logs/lsblk-final.txt" 2>&1
acl_of "$SHARE_DIR" >"$WORK/logs/getfacl-final.txt" 2>/dev/null || true
pdbedit -L >"$WORK/logs/pdbedit-final.txt" 2>&1 || true
getent passwd | grep -E "^($ACCT_RW|$ACCT_RO):" >"$WORK/logs/getent-final.txt" 2>&1 || true

# 最后再扫一遍明文：整轮所有日志与两个数据库。
expect_eq "收尾扫描：所有日志与数据库里都没有明文" "0" \
  "$(( $(hits_in_file "$WEB_LOG") + $(hits_in_file "$AGENT_LOG") + $(hits_in_databases) ))"

# 清理落在临时目录里的密码文件。
shred -u "$PW_FILE" "$PW2_FILE" 2>/dev/null || rm -f "$PW_FILE" "$PW2_FILE"

notrun "从另一台机器用 SMB 客户端连上来" \
  "P08.1 不写配置、不重载服务，共享此刻不对外提供访问；SMB 客户端 VM（P00-VM-D）尚未建立"
notrun "写入配置 / testparm 校验 / 原子替换 / 重载 / 共享连接状态" \
  "属于 P08.2 与 P08.3；本脚本反过来证明这一阶段确实没写"
notrun "浏览器里的真实渲染" \
  "本脚本验载荷；DOM 由 web 侧 SSR 回放覆盖，浏览器端到端由 smoke 覆盖"
notrun "能力 blocked 分支（缺包环境）" \
  "本机装了 samba，走的是 available；blocked 由 internal/shares 的注入式单测覆盖"

echo
echo "== 汇总 =="
printf 'PASS=%d FAIL=%d NOT RUN=%d\n' "$PASSED" "$FAILED" "$NOTRUN"
[ "$FAILED" -eq 0 ] || printf 'FAILED:%s\n' "$FAILED_NAMES"
echo
echo "证据留在 $WORK（payloads/ 是页面与任务的原始响应，logs/ 是独立检查的输出）"
[ "$FAILED" -eq 0 ] && exit 0
exit 1
