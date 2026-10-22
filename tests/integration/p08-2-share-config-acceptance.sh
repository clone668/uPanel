#!/bin/sh
# P08.2 Linux 验收脚本：最小 include 接入、testparm、原子更新、重载、共享连接状态。
#
# 与 p08-1 的分工：那一份证明“共享被定义、目录与 ACL 备好了，但**没有**对外提供服务”。
# 这一份证明相反的那一半——配置真的写下去了，而且 Samba 真的在用，并且每一条都有一条
# **不经过面板**的证据：
#
#   1) **写进的是面板独占的文件**：共享段出现在 `/etc/samba/upanel.d/upanel.conf`，
#      `smb.conf` 只多了“一行注释 + 一行 include”，原有内容逐字节保留（用前缀比对，
#      不是“看起来还在”）。既有第三方共享一个都没少。
#   2) **候选与合并都过了 testparm**：不管是作业里报的那两个值，还是独立跑
#      `testparm -s --section-name=… --parameter-name=…` 读回来的有效值。
#   3) **真实客户端说得对**：write 用户能上传并读回内容；read 用户能列不能写；
#      在 passdb 里但不在名单里的账号被拒；匿名（guest）被拒；`hosts allow` 真的生效
#      （不在允许网段里的客户端连不上）。这一步用本机 `smbclient`，是 SMB 协议层的
#      真实客户端，不是“面板说它生效了”。
#   4) **幂等重放不写、不重载**：同一份定义再应用一次，文件哈希与 mtime 不变，
#      `samba_config=unchanged`、`reload=skipped_unchanged`。
#   5) **smbd 没在跑时说真话**：服务停着的时候应用一条改动，结果必须是 `accepted`
#      而不是 `verified`，`served=false`，并且带 `SMB_SERVICE_INACTIVE`——配置写对了
#      也不等于有人能访问。
#   6) **撤得干净**：disable/delete 之后段从配置里消失、客户端连不上
#      （`NT_STATUS_BAD_NETWORK_NAME`）、ACL 撤销、目录内容一个字节不少。
#   7) **连接状态读的是 Samba 自己的会话表**：一条保持着的连接必须出现在
#      `GET /shares/:id/connections` 里（账号、机器、方言、签名），断开后消失；
#      并且响应里必带 `service`，因为空列表有两种含义。
#
# 用法：
#   sudo BIN_DIR=/var/tmp/upanel-bin SPARE_DISKS="sdb" \
#        sh tests/integration/p08-2-share-config-acceptance.sh
#
# 环境变量：PORT（默认 19477，避开 p08-1 的 19476）、WORK、BIN_DIR、SPARE_DISKS
# （一块一次性空盘，会被格式化——只允许在可丢弃的测试 VM 里跑）。
#
# 前置条件（属于夹具，不属于被测功能）：这台 VM 上已经装好 samba、samba-common-bin
# （`smbclient`/`smbstatus`/`smbcontrol`/`testparm`）与 acl。面板**不会**替你装包。
# 缺东西时本脚本在第 0 节就以退出码 2 停下，而不是把“缺 smbd”记成产品缺陷。
#
# 退出码：0 全部通过；1 有失败项；2 环境不满足（非 root / 盘不安全 / 缺工具 / 缺 samba）。
#
# 明确不验（NOT RUN，原因写在这里，不在脚本里假装通过）：
#   - **从另一台物理机器连进来**：本机 `smbclient` 是真实 SMB 客户端，但它与 smbd 同机；
#     跨机器还牵涉防火墙与网络，而 `P00-VM-D`（SMB 客户端 VM）尚未建立。这不是“测过了”。
#   - **浏览器里的真实渲染**：本脚本验的是页面读到的载荷；DOM 由 web 侧回放覆盖。
#   - **缺 testparm/smbcontrol 时的“只删不增”分支**：要拆掉本机 Samba 工具才能造出这个
#     环境，那会把夹具弄坏、后面所有节都跑不了。它由 `internal/shares` 的单测
#     （`TestApplyManagedConfigRemovalOnlyWithoutTestparm`）覆盖。
#   - **写失败的回滚（磁盘满、权限被抢）**：真机上造这两个要先动根文件系统；单测用注入
#     的文件系统桩覆盖（`TestExecuteShareApplyRollsBackOnMergedFailure`）。
#   - **掉电后的持久性**：`fsync` 目录这一步在开发宿主上是 `Access is denied`，
#     真机这里只证明它被调用过（单测计数桩），断电场景需要额外的注入手段。

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
WORK=${WORK:-/var/tmp/upanel-accept-p08-2}
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
PORT=${PORT:-19477}
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
SMBD_STOPPED=0

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

SHARE_NAME=p08share
SHARE_ALLOWED=p08cidr
REL_PATH=team/docs
ACCT_RW=smbp08rw
ACCT_RO=smbp08ro
ACCT_RW_PW='vm-share-rw-horse-2'
ACCT_RO_PW='vm-share-ro-horse-3'
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
  # 函数在 §6 才定义：脚本在更早的地方退出时它还不存在，别让清理步骤自己报错。
  if command -v teardown_netns >/dev/null 2>&1; then teardown_netns; fi
  stop_smbd_restore
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

# stop_smbd_restore 在退出时把 smbd 拉回运行状态。
#
# 第 10 节会主动停掉 smbd（那是被验的场景），但脚本必须把它恢复：否则下一条脚本
# 一起来就看到一台“Samba 不在跑”的机器，而它没有任何办法知道这是上一轮留下的。
stop_smbd_restore() {
  if [ "${SMBD_STOPPED:-0}" = "1" ]; then
    systemctl start smbd >/dev/null 2>&1 || true
    SMBD_STOPPED=0
  fi
  if [ -n "${SMB_CONF_ARCHIVE:-}" ] && [ -f "$SMB_CONF_ARCHIVE" ]; then
    cp "$SMB_CONF_ARCHIVE" "$SMB_CONF" 2>/dev/null || true
    smbcontrol all reload-config >/dev/null 2>&1 || true
  fi
  return 0
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

# put_secret <用途> <密码文件> <引用>
#
# 明文从文件读进请求体，请求体走 `-d @file`——这样密码既不在 argv 里（/proc 可见），
# 也不会印在终端上。请求体用完即 shred。
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
secget() { jget "$WORK/secret.json" "$1"; }

# preflight <动作> <参数 JSON>：预检响应另存一份 $WORK/preflight.json，回显状态码。
preflight() {
  printf '{"action":"%s","params":%s}' "$1" "$2" >"$WORK/pre.json"
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
# 预检 →（需要再认证就先再认证）→ 提交 → 等终态。结果放在全局变量里（不放进 $( )：
# 那会开子 shell，变量带不回来）。预检/提交失败时**清空** $WORK/job.json——
# 不清的话后面每条 job 断言读到的都是上一轮的作业，p08-1 就在这上面骗过自己。
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
jobnotices() {
  jobget '",".join((d["data"]["result"]["observed_state"].get("config_notices") or []))'
}

pallowed() { pget 'd["data"]["allowed"]'; }
pall() { pget '",".join(b["code"] for b in d["data"]["blockers"])'; }
pwhy() { pget '"; ".join(b["message"] for b in d["data"]["blockers"])'; }
pwarn() { pget '",".join(w["code"] for w in (d["data"].get("warnings") or []))'; }
pimpact() { pget '",".join(i["kind"] for i in d["data"]["impacts"])'; }
plocks() { pget '",".join(d["data"]["locks"])'; }
preauth() { pget 'bool(d["data"].get("requires_reauth"))'; }

expect_verified_served() {
  expect_eq "$1任务终态" "succeeded" "$JOB_STATE"
  expect_eq "$1结果是 verified" "verified" "$(joutcome)"
  expect_eq "$1带 verified_at" "True" "$(jverified)"
  expect_eq "$1observed.served" "True" "$(jobobserved served)"
  expect_eq "$1recovery_hint 说明已在提供服务" "True" \
    "$(jget "$WORK/job.json" 'd["data"]["result"]["recovery_hint"].find("正在提供") >= 0')"
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

# ---------- 独立证据（不经过产品的解析器） ----------

blkid_field() { blkid -p -o value -s "$2" "$1" 2>/dev/null | head -1; }
fs_type() { blkid_field "$1" TYPE; }
fs_uuid() { blkid_field "$1" UUID; }
is_mounted() {
  if [ -n "$(findmnt -rno TARGET --mountpoint "$1" 2>/dev/null)" ]; then echo True; else echo False; fi
}

smb_conf_sha() { sha256sum "$SMB_CONF" 2>/dev/null | cut -d' ' -f1; }
managed_sha() { sha256sum "$MANAGED_CONF" 2>/dev/null | cut -d' ' -f1; }
smb_conf_include_lines() { grep -cF "/etc/samba/upanel.d/" "$SMB_CONF" 2>/dev/null || true; }
smb_conf_section_lines() { grep -c '^\[' "$SMB_CONF" 2>/dev/null || true; }
smb_conf_sections() { grep '^\[' "$SMB_CONF" 2>/dev/null | tr -d '[]' | sort; }

# 独立问 Samba：这份共享的段现在存不存在、它的键值是什么。
# 用 --parameter-name 而不是 grep 我们自己写的文本——“文件里写了什么”不等于
# “Samba 读出来是什么”。段不存在时 testparm 的退出码不为 0。
# 那个值只出现在 stdout 上（横幅与 “Loaded services file OK.” 都在 stderr，见
# docs/evidence/P08/2026-10-22-p08.2/probe-output.txt 第 4 节）。
smb_effective() {
  testparm -s --suppress-prompt --section-name="$1" --parameter-name="$2" \
    "$SMB_CONF" 2>/dev/null | tr -d '\r' | sed '/^$/d' | tr '\n' ' ' | sed 's/ $//'
}
smb_section_exists() {
  if testparm -s --suppress-prompt --section-name="$1" --parameter-name="path" "$SMB_CONF" \
    >/dev/null 2>&1; then echo True; else echo False; fi
}

# smb_service_state 与产品里 probeServiceState 的判定保持一致：
# 问的是 smbd.service 与 smbd.socket 两个 unit，任意一个 active 就算在服务。
# 用 awk 逐行精确比，因为 "inactive" 里也含 "active"。
smb_service_state() {
  systemctl is-active smbd.service smbd.socket 2>/dev/null | tr -d '\r' | awk '
    $0=="active" || $0=="reloading" { a=1; k=1; next }
    $0=="inactive" || $0=="failed" || $0=="activating" || $0=="deactivating" ||
    $0=="unknown" || $0=="maintenance" { k=1 }
    END { if (a) print "active"; else if (k) print "inactive"; else print "unknown" }'
}

# smbstatus_tcons <共享名>：Samba 自己说有几种树连接接到这份共享上。
# 不经过面板：这是“面板转述得对不对”的对账边。
smbstatus_tcons() {
  smbstatus --json 2>/dev/null | python3 -c '
import json, sys
name = sys.argv[1].lower()
try:
    doc = json.load(sys.stdin)
except Exception:
    print("ERR")
    raise SystemExit
tcons = doc.get("tcons") or {}
print(len([t for t in tcons.values() if str(t.get("service", "")).lower() == name]))' "$1"
}

passwd_line() { getent passwd "$1" 2>/dev/null | head -1; }
pdb_has() {
  if pdbedit -L 2>/dev/null | cut -d: -f1 | grep -qxF "$1"; then echo True; else echo False; fi
}
acl_of() { getfacl -cp "$1" 2>/dev/null; }
acl_entry() {
  acl_of "$1" | awk -F: -v k="user:$2" '$0 ~ "^" k ":" {print $3}' | head -1
}
acl_has_named_users() {
  if acl_of "$1" | grep -q '^user:[^:]'; then echo True; else echo False; fi
}
acl_field() {
  acl_of "$1" | awk -v k="$2" 'index($0, k) == 1 {print substr($0, length(k) + 1); exit}'
}

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

# smbclient 的调用：连接 / 列目录 / 上传 / 下载。失败信息里带 NT_STATUS_*。
#
# 认证走 `-A 认证文件`，不走 `-U 账号%密码`：后者会把密码放进 smbclient 的 argv，
# 同机器上任何一个能读 /proc 的进程都看得到——那正是本阶段禁止的事。认证文件 0600，
# 用完由 §14 的进程表扫描来证明“确实没走 argv”。
smb_authfile() { # <账号> <密码变量名> -> 打印认证文件路径
  _f=$SECRETS_DIR/auth-$1
  eval "_pw=\${$2}"
  printf 'username=%s\npassword=%s\ndomain=\n' "$1" "$_pw" >"$_f"
  chmod 0600 "$_f"
  printf '%s' "$_f"
}
smb_ls() { # <共享> <认证文件>
  smbclient "//127.0.0.1/$1" -A "$2" -m SMB3 -c 'ls' 2>&1
}
smb_put() { # <共享> <认证文件> <本地文件> <远端名>
  smbclient "//127.0.0.1/$1" -A "$2" -m SMB3 -c "put $3 $4" 2>&1
}
smb_get() { # <共享> <认证文件> <远端名> <落地路径>
  smbclient "//127.0.0.1/$1" -A "$2" -m SMB3 -c "get $3 $4" >/dev/null 2>&1
  cat "$4" 2>/dev/null
}
smb_ls_guest() { smbclient "//127.0.0.1/$1" -N -m SMB3 -c 'ls' 2>&1; }

echo "== 0. 环境与安全守卫 =="
for b in "$AGENT" "$WEB" "$CTL"; do
  if [ -x "$b" ]; then pass "二进制存在且可执行 $(basename "$b")"; else fail "二进制存在且可执行 $(basename "$b")" "$b"; fi
done
for t in python3 curl runuser sha256sum findmnt lsblk sgdisk blkid wipefs udevadm \
         blockdev stat awk diff grep systemctl; do
  if command -v "$t" >/dev/null 2>&1; then pass "依赖命令可用 $t"; else fail "依赖命令可用 $t" "缺少 $t"; fi
done

MISSING_PKGS=""
for t in /usr/sbin/smbd /usr/bin/smbpasswd /usr/bin/pdbedit /usr/bin/testparm \
         /usr/bin/smbcontrol /usr/bin/smbstatus /usr/bin/smbclient \
         /usr/bin/setfacl /usr/bin/getfacl; do
  if [ -x "$t" ]; then pass "夹具：$t 存在"; else MISSING_PKGS="$MISSING_PKGS $t"; fi
done
if [ -f "$SMB_CONF" ]; then pass "夹具：$SMB_CONF 存在"; else MISSING_PKGS="$MISSING_PKGS $SMB_CONF"; fi
if [ -n "$MISSING_PKGS" ]; then
  printf '这台机器缺少 samba/acl 夹具：%s\n' "$MISSING_PKGS" >&2
  printf '先装好（apt-get install -y samba samba-common-bin acl）再来跑验收；面板不负责装包。\n' >&2
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

# —— 干净的起点（第一部分）：把上一轮的残留清掉，**再**做盘的守卫判断。
# 顺序不能反：守卫里“盘上不许有挂载点”是给“这盘能不能碰”用的，而上一轮中断留下的
# 挂载点属于本脚本自己的残留，先清掉，守卫判断才是对当前状态的判断。
for mp in "$VOLUME_ROOT"/*; do
  [ -d "$mp" ] || continue
  note "卸掉上一轮遗留的挂载 $mp"
  umount "$mp" 2>/dev/null || true
done
if grep -qx '# BEGIN upanel managed volumes' "$FSTAB" 2>/dev/null; then
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

# —— 干净的起点（第二部分）：清空工作目录，记下起点证据。
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
SMB_CONF_SHA0=$(smb_conf_sha)
SMB_SECTIONS0=$(smb_conf_sections)
printf '%s\n' "$SMB_SECTIONS0" >"$WORK/logs/smb-sections-initial.txt"
pass "起点：smb.conf 的 sha256 已记录（$SMB_CONF_SHA0）"
note "起点：smb.conf 里有 $(printf '%s\n' "$SMB_SECTIONS0" | grep -c . || true) 个段，其中既有第三方共享必须保留"

# 上一轮的托管文件与 include 目录：这一轮从“没接过 include”的干净状态开始。
if [ -d "$SMB_INCLUDE_DIR" ]; then
  rm -rf "$SMB_INCLUDE_DIR"
  note "清掉上一轮遗留的 $SMB_INCLUDE_DIR"
fi
if grep -qF "/etc/samba/upanel.d/" "$SMB_CONF" 2>/dev/null; then
  fail "起点：smb.conf 里没有残留的 include 行" "仍有指向 $SMB_INCLUDE_DIR 的行"
else
  pass "起点：smb.conf 里没有残留的 include 行"
fi
rm -rf "$BACKUP_DIR"
pass "起点：备份目录已清空（$BACKUP_DIR）"

lsblk -o NAME,TYPE,SIZE,FSTYPE,MOUNTPOINTS >"$WORK/logs/lsblk-initial.txt" 2>&1
rm -rf "$VOLUME_ROOT"/* 2>/dev/null
install -d -m 0755 "$VOLUME_ROOT"
wipefs -a "/dev/$BLANK_A" >/dev/null 2>&1
sgdisk --zap-all "/dev/$BLANK_A" >/dev/null 2>&1
blockdev --rereadpt "/dev/$BLANK_A" >/dev/null 2>&1
udevadm settle --timeout=5 >/dev/null 2>&1
pass "一次性盘 /dev/$BLANK_A 清成空白"

for n in "$ACCT_RW" "$ACCT_RO"; do
  pdbedit -x -u "$n" >/dev/null 2>&1 || true
  userdel "$n" >/dev/null 2>&1 || true
done
if getent passwd "$ACCT_RW" >/dev/null 2>&1 || pdbedit -L 2>/dev/null | grep -qE "^($ACCT_RW|$ACCT_RO):"; then
  fail "起点：受管理账号已清空" "系统里还有 smbp08* 账号残留"
else
  pass "起点：受管理账号已清空"
fi

if [ "$(smb_service_state)" != "active" ]; then
  systemctl start smbd >/dev/null 2>&1 || true
  sleep 1
fi
expect_eq "夹具：smbd 在运行" "active" "$(smb_service_state)"

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

# 三个口令写进秘密文件，供最后的明文扫描用（grep -f，检查者自己的 argv 里没有密码）。
# 每个账号另存一份**不带换行**的值文件，作为秘密通道的输入。
PW_FILE=$SECRETS_DIR/passwords.txt
umask 077
printf '%s\n%s\n%s\n' "$ACCT_RW_PW" "$ACCT_RO_PW" "$PASSWORD" >"$PW_FILE"
chmod 0600 "$PW_FILE"

echo
echo "== 2. 能力检测：这一阶段起「配置能写」必须是真的 =="
expect_eq "共享列表可读" "200" "$(get_shares cap)"
cp "$WORK/shares-cap.json" "$PAYLOADS/page-shares-empty.json"
expect_eq "能力状态是 available" "available" "$(sget cap 'd["data"]["capability"]["status"]')"
expect_eq "共享列表此刻是空数组（不是 null）" "True" "$(sget cap 'd["data"]["shares"] == []')"

SMB_VER_INDEP=$(/usr/sbin/smbd --version 2>/dev/null | head -1 | sed 's/^Version //')
expect_eq "能力里的 samba_version 与 smbd --version 一致" "$SMB_VER_INDEP" \
  "$(sget cap 'd["data"]["capability"]["samba_version"]')"

TOOL_BAD=$(sget cap 'import os
bad=[]
t=d["data"]["capability"].get("tools") or {}
for k,v in t.items():
    if not v or not os.access(v, os.X_OK):
        bad.append(k+"="+str(v))
_=",".join(bad)')
expect_eq "能力里报的每个工具路径都存在且可执行" "" "$TOOL_BAD"
for k in smbd smbpasswd pdbedit testparm smbcontrol smbstatus setfacl getfacl; do
  expect_ne "必需工具 $k 在能力里（缺了就该 unsupported）" "" \
    "$(sget cap "d[\"data\"][\"capability\"][\"tools\"].get(\"$k\") or \"\"")"
done

expect_eq "配置写入从本阶段起是 available" "available" \
  "$(sget cap 'd["data"]["capability"]["config_write"]')"
# 启动日志不能再说“本阶段不写配置”：那句话从 P08.2 起就是假的，会把排障的人
# 往“共享建好了但客户端连不上是正常的”这条错路上带。
WIRING_LINE=$(grep -a '共享模块已接线' "$AGENT_LOG" | head -1)
expect_contains "启动日志记下了托管配置文件路径" "$MANAGED_CONF" "$WIRING_LINE"
expect_not_contains "启动日志不再声称本阶段不写配置" "unsupported（P08.1" "$WIRING_LINE"
expect_eq "service_state 与 systemctl 的判定一致（smbd.service + smbd.socket）" "$(smb_service_state)" \
  "$(sget cap 'd["data"]["capability"]["service_state"]')"
expect_eq "service_units 报的是 smbd.service 与 smbd.socket" "smbd.service,smbd.socket" \
  "$(sget cap '",".join(d["data"]["capability"]["service_units"])')"
expect_eq "managed_config 指向面板独占的那份文件" "$MANAGED_CONF" \
  "$(sget cap 'd["data"]["capability"]["managed_config"]')"
expect_eq "managed_config_present 与文件是否存在一致" \
  "$([ -f "$MANAGED_CONF" ] && echo True || echo False)" \
  "$(sget cap 'bool(d["data"]["capability"]["managed_config_present"])')"
expect_eq "include_present 与 smb.conf 里那一行一致" \
  "$([ "$(smb_conf_include_lines)" -gt 0 ] && echo True || echo False)" \
  "$(sget cap 'bool(d["data"]["capability"]["include_present"])')"
expect_eq "起点 include_present 是 false（还没接）" "False" \
  "$(sget cap 'bool(d["data"]["capability"]["include_present"])')"
expect_eq "notices 都有 message" "True" \
  "$(sget cap 'all(bool(n.get("message")) for n in (d["data"]["capability"].get("notices") or []))')"

expect_eq "账号列表可读" "200" "$(get_accounts cap-accounts)"
expect_eq "账号列表此刻是空数组" "True" "$(aget cap-accounts 'd["data"]["accounts"] == []')"

echo
echo "== 3. 夹具：空盘走产品的 GPT → 分区 → ext4 → 导入 → 挂载 =="
PART_NAME=data
run_action storage.partition_table.initialize \
  "{\"device_id\":\"$BLANK_A\",\"table_type\":\"gpt\"}" init-a
expect_eq "初始化分区表预检允许" "True" "$PRE_ALLOWED"
expect_eq "初始化分区表任务终态" "succeeded" "$JOB_STATE"
expect_eq "初始化分区表结果 verified" "verified" "$(joutcome)"
run_action storage.partition.create \
  "{\"device_id\":\"$BLANK_A\",\"start_bytes\":\"$PART_START\",\"size_bytes\":\"$PART_MAIN\",\"name\":\"$PART_NAME\"}" create-a
expect_eq "建分区任务终态" "succeeded" "$JOB_STATE"
expect_eq "建分区结果 verified" "verified" "$(joutcome)"
sync_ok=0
_i=0
while [ "$_i" -lt 40 ]; do
  [ -b "/dev/${BLANK_A}1" ] && { sync_ok=1; break; }
  _i=$((_i + 1)); sleep 0.2
done
expect_eq "独立证据：内核看到了 ${BLANK_A}1" "1" "$sync_ok"
run_action storage.filesystem.format \
  "{\"device_id\":\"${BLANK_A}1\",\"filesystem\":\"ext4\",\"label\":\"$LABEL\"}" format-a
expect_eq "格式化任务终态" "succeeded" "$JOB_STATE"
expect_eq "独立证据：blkid 读出的类型是 ext4" "ext4" "$(fs_type "/dev/${BLANK_A}1")"
expect_eq "独立证据：blkid 读出的卷标" "$LABEL" "$(blkid_field "/dev/${BLANK_A}1" LABEL)"

run_action storage.volume.import \
  "{\"device_id\":\"${BLANK_A}1\",\"slug\":\"$SLUG\",\"display_name\":\"数据卷\"}" import-a
expect_eq "导入预检允许" "True" "$PRE_ALLOWED"
expect_eq "导入任务终态" "succeeded" "$JOB_STATE"
VOL_ID=$(jobdata volume_id)
MP=$VOLUME_ROOT/$SLUG
expect_eq "挂载点由 slug 派生" "$MP" "$(jobdata mount_path)"

VOL_VER=$(current_vol_version "$VOL_ID" m1)
run_action storage.volume.mount "{\"volume_id\":\"$VOL_ID\",\"version\":\"$VOL_VER\"}" mount-a
expect_eq "挂载任务终态" "succeeded" "$JOB_STATE"
expect_eq "独立证据：卷真的挂上了" "True" "$(is_mounted "$MP")"

echo
echo "== 4. 受管理账号 + 秘密通道（共享名单要用真实账号） =="
for pair in "$ACCT_RW:$ACCT_RW_PW" "$ACCT_RO:$ACCT_RO_PW"; do
  _name=${pair%%:*}
  _pw=${pair#*:}
  printf '%s' "$_pw" >"$SECRETS_DIR/$_name.pw"
  chmod 0600 "$SECRETS_DIR/$_name.pw"
  _sec_code=$(put_secret smb_password "$SECRETS_DIR/$_name.pw" "accept-p08-2-$_name")
  cp "$WORK/secret.json" "$PAYLOADS/secret-$_name.json"
  expect_eq "存 $_name 的密码走秘密通道" "201" "$_sec_code"
done
SEC_RW=$(jget "$PAYLOADS/secret-$ACCT_RW.json" 'd["data"]["secret_ref"]')
SEC_RO=$(jget "$PAYLOADS/secret-$ACCT_RO.json" 'd["data"]["secret_ref"]')
expect_ne "拿得到 $ACCT_RW 的 secret_ref" "" "$SEC_RW"
expect_ne "拿得到 $ACCT_RO 的 secret_ref" "" "$SEC_RO"
expect_eq "秘密响应里没有明文" "0" \
  "$(grep -a -c -F -f "$PW_FILE" "$PAYLOADS/secret-$ACCT_RW.json" 2>/dev/null || true)"

run_action share.account.apply \
  "{\"account_id\":null,\"name\":\"$ACCT_RW\",\"secret_ref\":\"$SEC_RW\",\"disabled\":false}" acct-rw
expect_eq "建 $ACCT_RW 任务终态" "succeeded" "$JOB_STATE"
expect_eq "建 $ACCT_RW 结果 verified" "verified" "$(joutcome)"
ACCT_ID_RW=$(jobdata account_id)
run_action share.account.apply \
  "{\"account_id\":null,\"name\":\"$ACCT_RO\",\"secret_ref\":\"$SEC_RO\",\"disabled\":false}" acct-ro
expect_eq "建 $ACCT_RO 任务终态" "succeeded" "$JOB_STATE"
ACCT_ID_RO=$(jobdata account_id)
expect_eq "独立证据：两个账号都在 passdb 里" "True" "$(pdb_has "$ACCT_RW")"
expect_eq "独立证据：$ACCT_RO 已在 passdb 里（它此刻还不在共享名单里）" "True" "$(pdb_has "$ACCT_RO")"

echo
echo "== 5. share.apply：配置真的写进去，而且 Samba 真的在用 =="
DEF_RW=$(printf '{"name":"%s","protocol":"smb","volume_id":"%s","relative_path":"%s","enabled":true,"read_only":false,"allowed_cidrs":["127.0.0.0/8"],"smb_users":[{"account_id":"%s","access":"write"}]}' \
  "$SHARE_NAME" "$VOL_ID" "$REL_PATH" "$ACCT_ID_RW")
run_action share.apply "{\"share_id\":null,\"version\":null,\"definition\":$DEF_RW}" apply-rw
expect_eq "应用共享预检允许" "True" "$PRE_ALLOWED"
expect_verified_served "应用共享"
SHARE_ID=$(jobdata share_id)
SHARE_VER=$(jobdata version)
expect_ne "拿到共享标识" "" "$SHARE_ID"
expect_eq "申请写配置这次确实重写了文件" "written" "$(jobobserved samba_config)"
expect_eq "include 这一行是这次接上的" "installed" "$(jobobserved include)"
expect_eq "重载被执行" "done" "$(jobobserved reload)"
expect_eq "候选与合并都过了 testparm" "merged-ok" "$(jobobserved config_testparm)"
expect_eq "服务状态是 active" "active" "$(jobobserved service)"
expect_ne "给了备份路径（改配置前先备份）" "" "$(jobobserved config_backups)"

# —— 独立证据一：文件层面的三件事（位置、必须最小、既有内容逐字节保留）
expect_eq "独立证据：托管文件存在" "True" "$([ -f "$MANAGED_CONF" ] && echo True || echo False)"
expect_contains "独立证据：托管文件里有这一段" "[$SHARE_NAME]" "$(cat "$MANAGED_CONF" 2>/dev/null)"
expect_contains "独立证据：托管文件里写的是卷内的真实目录" "path = $MP/$REL_PATH" \
  "$(cat "$MANAGED_CONF" 2>/dev/null)"
expect_eq "独立证据：smb.conf 里指向托管目录的行只有一行" "1" "$(smb_conf_include_lines)"
PREFIX_OK=$(python3 - "$SMB_CONF" "$WORK/logs/smb-conf-initial.txt" <<'PY'
import sys
new = open(sys.argv[1], 'rb').read()
old = open(sys.argv[2], 'rb').read()
rest = new[len(old):]
print("True" if new.startswith(old) else "False")
print(rest.decode('utf-8', 'replace'))
PY
)
expect_eq "独立证据：smb.conf 原有内容逐字节保留（新内容是旧内容的前缀延伸）" "True" \
  "$(printf '%s\n' "$PREFIX_OK" | sed -n 1p)"
APPENDED_LINES=$(printf '%s\n' "$PREFIX_OK" | sed -n '2,$p' | sed '/^$/d' | wc -l | tr -d ' ')
expect_eq "独立证据：追加的行只有两行（一行说明注释 + 一行 include）" "2" "$APPENDED_LINES"
CUR_SECTIONS=$(smb_conf_sections)
MISSING=0
for s in $SMB_SECTIONS0; do
  case "$CUR_SECTIONS" in
    *"$s"*) : ;;
    *) MISSING=$((MISSING + 1)) ;;
  esac
done
expect_eq "独立证据：smb.conf 原有段名全都还在" "0" "$MISSING"

# —— 独立证据二：问 Samba 自己（不是读我们自己写的文本）
expect_eq "独立证据：Samba 读得出这一段存在" "True" "$(smb_section_exists "$SHARE_NAME")"
expect_eq "独立证据：Samba 读回的 path" "$MP/$REL_PATH" "$(smb_effective "$SHARE_NAME" path)"
expect_eq "独立证据：Samba 读回的 valid users" "$ACCT_RW" "$(smb_effective "$SHARE_NAME" "valid users")"
expect_eq "独立证据：Samba 读回的 read only" "No" "$(smb_effective "$SHARE_NAME" "read only")"
expect_eq "独立证据：Samba 读回的 guest ok" "No" "$(smb_effective "$SHARE_NAME" "guest ok")"
expect_eq "独立证据：Samba 读回的 hosts allow" "127.0.0.0/8" "$(smb_effective "$SHARE_NAME" "hosts allow")"

# —— 独立证据：共享账号能不能真的走进这个目录
#
# 这一条是 Q233 的机器判据。第二遍真机运行在这里丢了 7 条断言：目录自身的 ACL 一条不差、
# testparm 读回的 path 一个字不差，客户端却在 put 时拿到 NT_STATUS_ACCESS_DENIED——因为
# 上级目录 team 被上一版按 0700 建出来，共享账号既不是它的属主也不在它的属组里，连门都进不去。
# 所以判据不是“我们的 ACL 写了什么”，而是“用共享账号的身份，能不能穿过去”。
ANCESTOR_DIR=$(dirname "$MP/$REL_PATH")
expect_eq "独立证据：共享账号能走进共享目录" "True" \
  "$(sudo -u "$ACCT_RW" test -x "$MP/$REL_PATH" && echo True || echo False)"
expect_eq "独立证据：共享账号能穿过每一级上级目录" "True" \
  "$(sudo -u "$ACCT_RW" test -x "$ANCESTOR_DIR" && echo True || echo False)"
expect_eq "独立证据：上级目录对 other 只开了 x（可穿越、不可列出）" "True" \
  "$(python3 -c 'import os,sys
m = os.stat(sys.argv[1]).st_mode
print(bool(m & 0o001) and not bool(m & 0o004))' "$ANCESTOR_DIR")"

# —— 备份：文件在、权限紧、内容就是改之前那一份
BK_DIR_COUNT=$(ls -1 "$BACKUP_DIR" 2>/dev/null | wc -l | tr -d ' ')
if [ "$BK_DIR_COUNT" -ge 1 ]; then pass "备份目录里产生了备份（$BK_DIR_COUNT 个）"; \
  else fail "备份目录里产生了备份" "$BACKUP_DIR 是空的"; fi
BK_PERM=$(find "$BACKUP_DIR" -maxdepth 1 -type f ! -perm 0600 2>/dev/null | wc -l | tr -d ' ')
expect_eq "备份文件权限都是 0600" "0" "$BK_PERM"
BK_MATCH=$(grep -rlF "[$SHARE_NAME]" "$BACKUP_DIR" 2>/dev/null | wc -l | tr -d ' ')
expect_eq "改之前的备份里没有这一段（备份的是旧的空配置）" "0" "$BK_MATCH"

# —— 共享记录：意图与事实都成立
expect_eq "共享详情可读" "200" "$(get_share "$SHARE_ID" rw)"
expect_eq "记录里 config_applied 是 true" "True" "$(shget rw 'bool(d["data"]["share"]["config_applied"])')"
expect_eq "记录里 directory_prepared 是 true" "True" \
  "$(shget rw 'bool(d["data"]["share"]["directory_prepared"])')"
expect_eq "共享列表里能读到这条" "1" \
  "$(get_shares list >/dev/null; sget list "len([s for s in d[\"data\"][\"shares\"] if s[\"id\"]==\"$SHARE_ID\"])")"

echo
echo "== 6. 真实 SMB 客户端：读写权限、未授权、匿名、来源网段 =="
AUTH_RW=$(smb_authfile "$ACCT_RW" ACCT_RW_PW)
AUTH_RO=$(smb_authfile "$ACCT_RO" ACCT_RO_PW)
REAL_FILE=$WORK/logs/upload.txt
printf 'written through a real smbclient at %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" >"$REAL_FILE"
PUT_OUT=$(smb_put "$SHARE_NAME" "$AUTH_RW" "$REAL_FILE" "probe-write.txt")
cp "$REAL_FILE" "$PAYLOADS/upload-source.txt"
printf '%s\n' "$PUT_OUT" >"$PAYLOADS/smbclient-put.txt"
expect_contains "write 用户能上传" "putting file" "$PUT_OUT"
expect_eq "独立证据：文件真的落在卷里" "True" \
  "$([ -f "$MP/$REL_PATH/probe-write.txt" ] && echo True || echo False)"
expect_eq "独立证据：落盘内容与源文件一致" "$(sha256sum "$REAL_FILE" | cut -d' ' -f1)" \
  "$(sha256sum "$MP/$REL_PATH/probe-write.txt" 2>/dev/null | cut -d' ' -f1)"
GET_BACK=$(smb_get "$SHARE_NAME" "$AUTH_RW" "probe-write.txt" "$WORK/logs/readback.txt")
expect_contains "write 用户能读回内容" "written through a real smbclient" "$GET_BACK"

# 只读账号此刻不在名单里：它必须被拒（这是“未授权账号被拒”的真机证据）。
RO_LIST=$(smb_ls "$SHARE_NAME" "$AUTH_RO")
printf '%s\n' "$RO_LIST" >"$PAYLOADS/smbclient-unauthorized.txt"
expect_contains "未授权的账号（在 passdb 里但不在名单里）被拒" "NT_STATUS_" "$RO_LIST"
expect_not_contains "未授权的账号看不到目录内容" "probe-write.txt" "$RO_LIST"

GUEST_OUT=$(smb_ls_guest "$SHARE_NAME")
printf '%s\n' "$GUEST_OUT" >"$PAYLOADS/smbclient-guest.txt"
expect_contains "匿名（guest）被拒" "NT_STATUS_" "$GUEST_OUT"
expect_not_contains "匿名看不到目录内容" "probe-write.txt" "$GUEST_OUT"

# 第二条共享：允许网段写成 10.0.0.0/24。
#
# **不能拿 127.0.0.1 来验“不在允许网段里会被拒”**：Samba 文档（man 5 smb.conf，
# hosts allow 一节）明写 "the localhost address 127.0.0.1 will always be allowed access
# unless specifically denied by a hosts deny option"。第一遍真机上这条断言就是这么倒的——
# 错的是断言，不是产品（见 DECISIONS Q234）。要验这个控制，必须从一个**真的不属于
# 允许网段**的地址发起连接：用 network namespace 造一个 10.99.0.2 当客户端，
# 宿主侧 10.99.0.1 让 smbd 可达。同一内核、不同源地址，够证明 hosts allow 按来源地址过滤。
DEF_CIDR=$(printf '{"name":"%s","protocol":"smb","volume_id":"%s","relative_path":"%s","enabled":true,"read_only":false,"allowed_cidrs":["10.0.0.0/24"],"smb_users":[{"account_id":"%s","access":"write"}]}' \
  "$SHARE_ALLOWED" "$VOL_ID" "team/cidr" "$ACCT_ID_RW")
run_action share.apply "{\"share_id\":null,\"version\":null,\"definition\":$DEF_CIDR}" apply-cidr
expect_eq "第二条共享任务终态" "succeeded" "$JOB_STATE"
SHARE_ALLOWED_ID=$(jobdata share_id)
expect_eq "独立证据：Samba 读回的 hosts allow" "10.0.0.0/24" "$(smb_effective "$SHARE_ALLOWED" "hosts allow")"
# 名单没覆盖环回时必须有一条告警：不能让界面暗示“本机也被挡住了”。
expect_contains "预检告警：名单管不住本机（Samba 对环回永远放行）" "SHARE_HOSTS_ALLOW_LOOPBACK" "$(pwarn)"

# 本机连得上：这是 Samba 的文档行为，把它当事实钉住，而不是当成产品缺陷。
CIDR_LO=$(smb_ls "$SHARE_ALLOWED" "$AUTH_RW")
printf '%s\n' "$CIDR_LO" >"$PAYLOADS/smbclient-cidr-loopback.txt"
expect_not_contains "本机（127.0.0.1）能连上：Samba 对环回永远放行（文档行为）" "NT_STATUS_" "$CIDR_LO"

# —— 从一个真的不在允许网段里的地址连进来
NETNS=p08client
VETH_HOST=p08veth0
VETH_GUEST=p08veth1
NS_HOST_IP=10.99.0.1
NS_CLIENT_IP=10.99.0.2
setup_netns() {
  ip netns add "$NETNS" 2>/dev/null || return 1
  ip link add "$VETH_HOST" type veth peer name "$VETH_GUEST" 2>/dev/null || return 1
  ip link set "$VETH_GUEST" netns "$NETNS" 2>/dev/null || return 1
  ip addr add "$NS_HOST_IP/24" dev "$VETH_HOST" 2>/dev/null || return 1
  ip link set "$VETH_HOST" up 2>/dev/null || return 1
  ip netns exec "$NETNS" ip addr add "$NS_CLIENT_IP/24" dev "$VETH_GUEST" 2>/dev/null || return 1
  ip netns exec "$NETNS" ip link set "$VETH_GUEST" up 2>/dev/null || return 1
  ip netns exec "$NETNS" ip link set lo up 2>/dev/null || return 1
  return 0
}
teardown_netns() {
  ip netns del "$NETNS" >/dev/null 2>&1 || true
  ip link del "$VETH_HOST" >/dev/null 2>&1 || true
}
smb_ls_ns() { ip netns exec "$NETNS" smbclient "//$NS_HOST_IP/$1" -A "$2" -m SMB3 -c 'ls' 2>&1; }

if setup_netns; then
  NETNS_READY=1
  expect_eq "夹具：netns 客户端地址" "$NS_CLIENT_IP" \
    "$(ip netns exec "$NETNS" ip -4 -o addr show dev "$VETH_GUEST" 2>/dev/null | awk '{print $4}' | cut -d/ -f1)"
  NS_DENY=$(smb_ls_ns "$SHARE_ALLOWED" "$AUTH_RW")
  printf '%s\n' "$NS_DENY" >"$PAYLOADS/smbclient-cidr-netns-denied.txt"
  expect_contains "不在允许网段里的客户端（10.99.0.2）被拒" "NT_STATUS_" "$NS_DENY"

  # 正向对照：把允许网段改成客户端所在的那一段，同一个客户端就该连得上。
  # 没有这一条，“被拒”也可能只是“客户端根本连不到 445”。
  SHARE_ALLOWED_VER=$(get_share "$SHARE_ALLOWED_ID" cidr >/dev/null; \
    shget cidr 'd["data"]["share"]["version"]')
  DEF_CIDR2=$(printf '{"name":"%s","protocol":"smb","volume_id":"%s","relative_path":"%s","enabled":true,"read_only":false,"allowed_cidrs":["10.99.0.0/24"],"smb_users":[{"account_id":"%s","access":"write"}]}' \
    "$SHARE_ALLOWED" "$VOL_ID" "team/cidr" "$ACCT_ID_RW")
  run_action share.apply \
    "{\"share_id\":\"$SHARE_ALLOWED_ID\",\"version\":\"$SHARE_ALLOWED_VER\",\"definition\":$DEF_CIDR2}" \
    apply-cidr-open
  expect_verified_served "允许网段改成客户端所在网段"
  expect_eq "独立证据：Samba 读回的新 hosts allow" "10.99.0.0/24" \
    "$(smb_effective "$SHARE_ALLOWED" "hosts allow")"
  NS_ALLOW=$(smb_ls_ns "$SHARE_ALLOWED" "$AUTH_RW")
  printf '%s\n' "$NS_ALLOW" >"$PAYLOADS/smbclient-cidr-netns-allowed.txt"
  expect_not_contains "网段放行之后同一个客户端能连上" "NT_STATUS_" "$NS_ALLOW"
else
  NETNS_READY=0
  notrun "不在允许网段里的客户端被拒" "这台机器建不出 network namespace/veth：无法从非环回地址发起连接"
  notrun "网段放行之后同一个客户端能连上" "同上"
fi

echo
echo "== 7. 共享连接状态：读的是 Samba 自己的会话表 =="
# 先用一个保持着的连接造出真实现场。用一个 FIFO 喂 stdin：写端一直握着，
# smbclient 就不会看到 EOF，连接就一直挂着。这样 $! 就是 smbclient 自己的 PID
# （管道的话拿到的是子 shell，kill 掉它不一定会结束客户端）。
HOLD_FIFO=$WORK/logs/hold.fifo
rm -f "$HOLD_FIFO"
mkfifo -m 0600 "$HOLD_FIFO"
smbclient "//127.0.0.1/$SHARE_NAME" -A "$AUTH_RW" -m SMB3 \
  <"$HOLD_FIFO" >"$WORK/logs/hold.log" 2>&1 &
HOLD_PID=$!
exec 9>"$HOLD_FIFO"
printf 'ls\n' >&9
sleep 3
expect_eq "连接状态可读" "200" "$(get_connections "$SHARE_ID" live)"
cp "$WORK/connections-live.json" "$PAYLOADS/page-connections-live.json"
expect_eq "报告里的 service 是 active" "active" "$(cget live 'd["data"]["service"]')"
expect_eq "报告里的共享名" "$SHARE_NAME" "$(cget live 'd["data"]["share_name"]')"
CONN_N=$(cget live 'len(d["data"]["connections"])')
if [ "$CONN_N" -ge 1 ] 2>/dev/null; then
  pass "至少看到一条连接（实际 $CONN_N）"
  expect_contains "连接里的账号是 write 用户" "$ACCT_RW" \
    "$(cget live '",".join(c["username"] for c in d["data"]["connections"])')"
  expect_contains "连接里的机器是 127.0.0.1" "127.0.0.1" \
    "$(cget live '",".join(c["machine"] for c in d["data"]["connections"])')"
  expect_contains "连接里有协商出的方言" "SMB" \
    "$(cget live '",".join(c["dialect"] for c in d["data"]["connections"])')"
  expect_ne "连接里有签名状态（不是空串）" "" \
    "$(cget live 'd["data"]["connections"][0]["signing"]')"
  expect_ne "连接里有建立时间" "" "$(cget live 'd["data"]["connections"][0]["connected_at"]')"
else
  fail "至少看到一条连接" "连接数 = $CONN_N（保持中的客户端没被 smbstatus 看到）"
fi
# 与独立读到的 smbstatus 对账：面板说有几条，Samba 自己也说有几条。
INDEP_TCONS=$(smbstatus_tcons "$SHARE_NAME")
expect_eq "面板的连接数与 smbstatus 自己报的一致" "$INDEP_TCONS" "$CONN_N"
# 关掉 FIFO 写端让 smbclient 看到 EOF 自己退出；不退再 kill。两种都要收干净，
# 否则 §14 的进程表扫描会把这条测试自己留下的 smbclient 当成“面板把密码写进了 argv”。
exec 9>&-
_i=0
while [ "$_i" -lt 20 ] && kill -0 "$HOLD_PID" 2>/dev/null; do _i=$((_i + 1)); sleep 0.2; done
kill "$HOLD_PID" 2>/dev/null
wait "$HOLD_PID" 2>/dev/null
rm -f "$HOLD_FIFO"
sleep 2
expect_eq "连接断开后列表回到 0" "0" "$(get_connections "$SHARE_ID" idle >/dev/null; cget idle 'len(d["data"]["connections"])')"
expect_eq "空闲时 service 仍然报 active（空列表不是“服务停了”）" "active" \
  "$(cget idle 'd["data"]["service"]')"

echo
echo "== 8. 幂等重放：内容没变就不写、不重载 =="
SHA_BEFORE=$(managed_sha)
SMB_SHA_BEFORE=$(smb_conf_sha)
BK_BEFORE=$(ls -1 "$BACKUP_DIR" 2>/dev/null | wc -l | tr -d ' ')
# 顺手把上级目录改回上一版建出来的形状（0700）：重放必须把权限修好，而配置一个字节都不动。
# 这条是“修好旧状态”的真机证据——单元测试能证明会调 chmod，只有这里能证明修完真的能用。
chmod 0700 "$ANCESTOR_DIR"
expect_eq "夹具：上级目录已改成 0700" "700" "$(stat -c %a "$ANCESTOR_DIR")"
expect_eq "夹具：改回 0700 之后共享账号进不去" "False" \
  "$(sudo -u "$ACCT_RW" test -x "$ANCESTOR_DIR" && echo True || echo False)"
run_action share.apply "{\"share_id\":\"$SHARE_ID\",\"version\":\"$SHARE_VER\",\"definition\":$DEF_RW}" apply-rw-again
expect_eq "重放任务终态" "succeeded" "$JOB_STATE"
expect_eq "重放结果仍然是 verified" "verified" "$(joutcome)"
expect_eq "重放时文件内容没变" "unchanged" "$(jobobserved samba_config)"
expect_eq "重放时不重载" "skipped_unchanged" "$(jobobserved reload)"
expect_eq "重放时 include 本来就在" "present" "$(jobobserved include)"
expect_eq "重放之后托管文件哈希不变" "$SHA_BEFORE" "$(managed_sha)"
expect_eq "重放之后 smb.conf 哈希不变" "$SMB_SHA_BEFORE" "$(smb_conf_sha)"
expect_eq "重放没有产生新备份" "$BK_BEFORE" "$(ls -1 "$BACKUP_DIR" 2>/dev/null | wc -l | tr -d ' ')"
expect_eq "重放之后 served 仍然是 true" "True" "$(jobobserved served)"
# 修权限这件事必须说出来：改了机器上哪些目录的权限，是必须能回答的问题。
expect_eq "重放结果点名了修过的上级目录" "$ANCESTOR_DIR" \
  "$(jobget '",".join(d["data"]["result"]["observed_state"].get("path_traversal_repaired") or [])')"
expect_eq "独立证据：上级目录被修成可穿越" "True" \
  "$(sudo -u "$ACCT_RW" test -x "$ANCESTOR_DIR" && echo True || echo False)"
RE_PUT=$(smb_put "$SHARE_NAME" "$AUTH_RW" "$REAL_FILE" "probe-write-repaired.txt")
printf '%s\n' "$RE_PUT" >"$PAYLOADS/smbclient-put-repaired.txt"
expect_contains "修好之后 write 用户又能上传" "putting file" "$RE_PUT"

echo
echo "== 9. 名单变更：加一个只读账号，读用户真的能读、不能写 =="
SHARE_VER=$(get_share "$SHARE_ID" rw >/dev/null; shget rw 'd["data"]["share"]["version"]')
DEF_RW2=$(printf '{"name":"%s","protocol":"smb","volume_id":"%s","relative_path":"%s","enabled":true,"read_only":false,"allowed_cidrs":["127.0.0.0/8"],"smb_users":[{"account_id":"%s","access":"write"},{"account_id":"%s","access":"read"}]}' \
  "$SHARE_NAME" "$VOL_ID" "$REL_PATH" "$ACCT_ID_RW" "$ACCT_ID_RO")
run_action share.apply "{\"share_id\":\"$SHARE_ID\",\"version\":\"$SHARE_VER\",\"definition\":$DEF_RW2}" apply-rw-ro
expect_eq "加只读账号任务终态" "succeeded" "$JOB_STATE"
expect_verified_served "加只读账号"
expect_eq "独立证据：Samba 读回的 valid users 含两个账号" "True" \
  "$(smb_effective "$SHARE_NAME" "valid users" | grep -q "$ACCT_RO" && echo True || echo False)"
expect_eq "独立证据：write list 只有写用户" "$ACCT_RW" "$(smb_effective "$SHARE_NAME" "write list")"

RO_LIST=$(smb_ls "$SHARE_NAME" "$AUTH_RO")
printf '%s\n' "$RO_LIST" >"$PAYLOADS/smbclient-ro-list.txt"
expect_contains "只读用户能列目录" "probe-write.txt" "$RO_LIST"
RO_PUT=$(smb_put "$SHARE_NAME" "$AUTH_RO" "$REAL_FILE" "ro-should-fail.txt")
printf '%s\n' "$RO_PUT" >"$PAYLOADS/smbclient-ro-put.txt"
# 拒的具体码在不同版本上可能是 ACCESS_DENIED 或 MEDIA_WRITE_PROTECTED，
# 所以判据是“报了 NT_STATUS_ 且没有一句 putting file”——后者才是成功的样子。
expect_contains "只读用户上传被拒（有 NT_STATUS_ 报错）" "NT_STATUS_" "$RO_PUT"
expect_not_contains "只读用户上传没有成功（没有 putting file）" "putting file" "$RO_PUT"
expect_eq "独立证据：只读用户没在卷里留下文件" "False" \
  "$([ -f "$MP/$REL_PATH/ro-should-fail.txt" ] && echo True || echo False)"
expect_eq "独立证据：只读账号的 ACL 是 r-x" "r-x" "$(acl_entry "$MP/$REL_PATH" "$ACCT_RO")"
expect_eq "独立证据：写账号的 ACL 是 rwx" "rwx" "$(acl_entry "$MP/$REL_PATH" "$ACCT_RW")"

echo
echo "== 10. smbd 没在跑的时候：配置照写，但不许说“已生效” =="
# 两个 unit 都要停：只停 smbd.service 的话，若 smbd.socket 还活着（socket 激活的
# 部署就是这样），产品会如实报 active，而这一节想验的“服务没在跑”就没造出来。
systemctl stop smbd.socket smbd.service >/dev/null 2>&1
systemctl stop smbd >/dev/null 2>&1
SMBD_STOPPED=1
sleep 1
expect_eq "夹具：smbd 已停" "inactive" "$(smb_service_state)"
SHARE_VER=$(get_share "$SHARE_ID" rw >/dev/null; shget rw 'd["data"]["share"]["version"]')
DEF_RO=$(printf '{"name":"%s","protocol":"smb","volume_id":"%s","relative_path":"%s","enabled":true,"read_only":true,"allowed_cidrs":["127.0.0.0/8"],"smb_users":[{"account_id":"%s","access":"write"},{"account_id":"%s","access":"read"}]}' \
  "$SHARE_NAME" "$VOL_ID" "$REL_PATH" "$ACCT_ID_RW" "$ACCT_ID_RO")
run_action share.apply "{\"share_id\":\"$SHARE_ID\",\"version\":\"$SHARE_VER\",\"definition\":$DEF_RO}" apply-service-down
expect_eq "服务停着时任务仍然 success" "succeeded" "$JOB_STATE"
expect_eq "服务停着时结果是 accepted（不是 verified）" "accepted" "$(joutcome)"
expect_eq "服务停着时不带 verified_at" "False" "$(jverified)"
expect_eq "服务停着时 served=false" "False" "$(jobobserved served)"
expect_eq "服务停着时不重载（不装作做了）" "skipped_service_inactive" "$(jobobserved reload)"
expect_contains "服务停着时说清原因" "SMB_SERVICE_INACTIVE" "$(jobnotices)"
expect_contains "recovery_hint 指向服务没在跑" "smbd 不在运行" "$(jrecovery)"
expect_eq "但配置确实写下去了（read only 改成了 yes）" "Yes" "$(smb_effective "$SHARE_NAME" "read only")"
expect_eq "连接状态此时报 inactive" "inactive" \
  "$(get_connections "$SHARE_ID" down >/dev/null; cget down 'd["data"]["service"]')"
expect_contains "连接状态为 inactive 时给出 SMB_SERVICE_INACTIVE" "SMB_SERVICE_INACTIVE" \
  "$(cget down '",".join(n["code"] for n in (d["data"].get("notices") or []))')"
systemctl start smbd >/dev/null 2>&1
SMBD_STOPPED=0
sleep 2
expect_eq "夹具：smbd 已恢复" "active" "$(smb_service_state)"
# 服务回来之后，同一个共享的连接查询必须不再报 inactive：否则页面会一直显示
# 一个“服务没在跑”的旧结论。
expect_eq "服务恢复后连接状态报 active" "active" \
  "$(get_connections "$SHARE_ID" up >/dev/null; cget up 'd["data"]["service"]')"

echo
echo "== 11. share.disable：段消失、客户端连不上、ACL 撤销、目录不删 =="
SHARE_VER=$(get_share "$SHARE_ID" rw >/dev/null; shget rw 'd["data"]["share"]["version"]')
run_action share.disable "{\"share_id\":\"$SHARE_ID\",\"version\":\"$SHARE_VER\"}" disable-rw
expect_eq "禁用任务终态" "succeeded" "$JOB_STATE"
expect_eq "禁用后 served 必须是 false" "False" "$(jobobserved served)"
expect_eq "独立证据：Samba 里这一段没了" "False" "$(smb_section_exists "$SHARE_NAME")"
expect_eq "独立证据：托管文件里也没了" "False" \
  "$(grep -qF "[$SHARE_NAME]" "$MANAGED_CONF" 2>/dev/null && echo True || echo False)"
DIS_OUT=$(smb_ls "$SHARE_NAME" "$AUTH_RW")
printf '%s\n' "$DIS_OUT" >"$PAYLOADS/smbclient-disabled.txt"
expect_contains "禁用后客户端连不上" "NT_STATUS_BAD_NETWORK_NAME" "$DIS_OUT"
expect_eq "独立证据：命名 ACL 已撤销" "False" "$(acl_has_named_users "$MP/$REL_PATH")"
expect_eq "独立证据：目录还在" "True" "$([ -d "$MP/$REL_PATH" ] && echo True || echo False)"
expect_eq "独立证据：目录里的文件一个都没删" "True" \
  "$([ -f "$MP/$REL_PATH/probe-write.txt" ] && echo True || echo False)"
expect_eq "记录里 config_applied 回到 false" "False" \
  "$(get_share "$SHARE_ID" disc >/dev/null; shget disc 'bool(d["data"]["share"]["config_applied"])')"

echo
echo "== 12. share.delete：记录没了、段没了、目录内容仍然不删 =="
SHARE_VER=$(get_share "$SHARE_ID" disc >/dev/null; shget disc 'd["data"]["share"]["version"]')
run_action share.delete "{\"share_id\":\"$SHARE_ID\",\"version\":\"$SHARE_VER\"}" delete-rw
expect_eq "删除任务终态" "succeeded" "$JOB_STATE"
expect_eq "删除后共享详情是 404" "404" "$(get_share "$SHARE_ID" gone)"
expect_eq "独立证据：Samba 里没有这一段" "False" "$(smb_section_exists "$SHARE_NAME")"
expect_eq "独立证据：目录与内容都还在" "True" \
  "$([ -f "$MP/$REL_PATH/probe-write.txt" ] && echo True || echo False)"

echo
echo "== 13. 第二条共享的收尾：撤掉它，并确认托管文件回到只剩 global =="
SHARE_VER=$(get_share "$SHARE_ALLOWED_ID" cidr >/dev/null; shget cidr 'd["data"]["share"]["version"]')
run_action share.delete "{\"share_id\":\"$SHARE_ALLOWED_ID\",\"version\":\"$SHARE_VER\"}" delete-cidr
expect_eq "删除第二条共享任务终态" "succeeded" "$JOB_STATE"
expect_eq "独立证据：Samba 里没有第二条段" "False" "$(smb_section_exists "$SHARE_ALLOWED")"
expect_eq "托管文件里没有残留的段（只剩 global）" "0" \
  "$(grep -c '^\[' "$MANAGED_CONF" 2>/dev/null | awk '{print ($1>0)?($1-1):0}')"
expect_eq "共享列表回到空" "0" \
  "$(get_shares final >/dev/null; sget final 'len(d["data"]["shares"])')"

echo
echo "== 14. 收尾不变量 =="
# include 仍然是恰好一行：反复的写/删不该把它变成两行。
expect_eq "整轮下来 include 仍然只有一行" "1" "$(smb_conf_include_lines)"
# 既有第三方共享从头到尾没人动过。
CUR_SECTIONS=$(smb_conf_sections)
MISSING=0
for s in $SMB_SECTIONS0; do
  case "$CUR_SECTIONS" in
    *"$s"*) : ;;
    *) MISSING=$((MISSING + 1)) ;;
  esac
done
expect_eq "整轮下来 smb.conf 原有段名一个都没少" "0" "$MISSING"
expect_eq "托管 include 目录权限是 0755" "755" "$(stat -c '%a' "$SMB_INCLUDE_DIR" 2>/dev/null)"
expect_eq "托管文件权限是 0644" "644" "$(stat -c '%a' "$MANAGED_CONF" 2>/dev/null)"

# 明文扫描：两个共享密码不许出现在日志、数据库、进程表里。
LOG_HITS=$(( $(hits_in_file "$WEB_LOG") + $(hits_in_file "$AGENT_LOG") ))
expect_eq "两个日志里没有共享密码明文" "0" "$LOG_HITS"
expect_eq "两个数据库（含 WAL）里没有共享密码明文" "0" "$(hits_in_databases)"
expect_eq "任何进程的 argv/environ 里没有共享密码明文" "0" "$(hits_in_processes)"

# 代理仍然活着：整轮没有把它弄崩。
if kill -0 "$AGENT_PID" 2>/dev/null; then pass "代理整轮没有退出"; else fail "代理整轮没有退出" "进程没了"; fi
if [ -S "$SOCK" ]; then pass "socket 仍然在"; else fail "socket 仍然在" "$SOCK 消失了"; fi

echo
echo "== 15. 结论 =="
printf 'PASS=%s FAIL=%s NOT RUN=%s\n' "$PASSED" "$FAILED" "$NOTRUN"
if [ "$FAILED" -ne 0 ]; then
  printf '失败项：%s\n' "$FAILED_NAMES"
  exit 1
fi
exit 0
