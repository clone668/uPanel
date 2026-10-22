#!/bin/sh
# P08.3 Linux 验收脚本：共享与卷的绑定、开机依赖、运行时脱盘、卸载/格式化的依赖阻断。
#
# 与 p08-2 的分工：那一份证明“配置写得对、Samba 在用、客户端能访问”。这一份问的是
# 另一件事——**共享目录背后的文件系统还在不在**。同一串路径在卷挂着与没挂着的时候
# 指向两个不同的东西，而 smbd 不区分它们。所以这一阶段的每一条证据都必须是
# “客户端此刻看到什么”，而不是“我们的配置里写了什么”：
#
#   1) 开机依赖真的装上了：`/etc/systemd/system/smbd.service.d/upanel-shares.conf`
#      里有 `RequiresMountsFor=`，而且 **systemd 自己**读到了它
#      （`systemctl show smbd.service -p DropinsPaths/RequiresMountsFor/After`），
#      不是“我们写了个文件”。
#   2) 运行中脱盘会被收缩暴露面：外部 `umount` 之后一个监视周期内，共享的段从托管
#      配置里消失、客户端拿到 `NT_STATUS_BAD_NETWORK_NAME`、登记表里留下撤销现场。
#      判定用**真实 smbclient**，不用 testparm 通过冒充“共享可用”。
#   3) 撤销时不会共享系统盘上的空目录：`umount` 之后在本该是共享目录的路径上（此时
#      它属于根分区）放一个标记文件；客户端既连不上这份共享，也就看不到那个标记。
#   4) 只撤不恢复：卷挂回来之后不会自动写回配置；人重新应用一次才回来，而且回来之后
#      客户端看到的是**卷里**的内容，不是根分区上那个标记文件。
#   5) 撤销是那次唯一的无人工确认写入：`smb.conf`（既有第三方共享）逐字节不变
#      （前缀比对），只有面板独占的托管文件被重写。
#   6) 卸载/格式化/退出管理三个动作的阈值不同：启用中的共享挡卸载（IN_USE_BY_SHARE），
#      任何共享（含已禁用）挡格式化与退出管理，没有依赖时才放行。
#
# 用法：
#   sudo BIN_DIR=/var/tmp/upanel-bin SPARE_DISKS="sdb" \
#        sh tests/integration/p08-3-share-volume-binding-acceptance.sh
#
# 环境变量：PORT（默认 19478，避开 p08-1 的 19476 与 p08-2 的 19477）、WORK、BIN_DIR、
# SPARE_DISKS（一块一次性空盘，会被格式化——只允许在可丢弃的测试 VM 里跑）。
#
# 退出码：0 全部通过；1 有失败项；2 环境不满足（非 root / 盘不安全 / 缺工具 / 缺 samba）。
#
# 明确不验（NOT RUN，原因写在这里，不在脚本里假装通过）：
#   - **真的重启一次去看开机窗口**：本脚本中途不会被 `reboot` 打断，所以这里只验
#     “systemd 收到了这条顺序要求”。真正的重启由同目录的
#     `p08-3-reboot-order.sh` 分两阶段做（重启前布置 + 重启后核对），
#     证据在 `docs/evidence/P08/<date>-p08.3/logs/reboot-*`。
#   - **观测不明（SHARE_VOLUME_STATE_UNKNOWN）与依赖查询失败
#     （VOLUME_DEPENDENCIES_UNKNOWN）**：真机上要造出“卷挂着但读不出来”与
#     “登记表读不出来”得先把存储采集器弄坏，那会把后面所有节一起弄坏。
#     这两条由单测覆盖（`internal/shares/{reconcile,startupdeps}_test.go`、
#     `internal/storage/deps_test.go`）。
#   - **从另一台物理机器连进来**：本机 `smbclient` 是真实 SMB 客户端，但与 smbd 同机；
#     `P00-VM-D`（SMB 客户端 VM）尚未建立。
#   - **浏览器里的真实渲染**：本脚本验的是页面读到的载荷。

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
WORK=${WORK:-/var/tmp/upanel-accept-p08-3}
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
SMBD_STOPPED=0

VOLUME_ROOT=/srv/upanel/volumes
FSTAB=/etc/fstab
SMB_CONF=/etc/samba/smb.conf
SMB_INCLUDE_DIR=/etc/samba/upanel.d
MANAGED_CONF=$SMB_INCLUDE_DIR/upanel.conf
BACKUP_DIR=/var/lib/upanel-agent/smb-backups
STARTUP_DROPIN=/etc/systemd/system/smbd.service.d/upanel-shares.conf
STARTUP_DROPIN_DIR=/etc/systemd/system/smbd.service.d
PART_MAIN=$((1 * 1073741824))
PART_START=1048576
SLUG=p08v3
LABEL=p08v3

SHARE_NAME=p08sh3
REL_PATH=team/docs
ACCT_RW=smbp08b3
ACCT_RW_PW='vm-bind-rw-horse-5'

# 监视间隔（代理内置 30 秒）。轮询上限留出“一个周期 + 一轮存储扫描（5 秒）”的余量。
WATCH_WAIT=${WATCH_WAIT:-75}

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
  # 函数在下面才定义：脚本在更早的地方退出时它还不存在，别让清理步骤自己报错。
  if command -v stop_smbd_restore >/dev/null 2>&1; then stop_smbd_restore; fi
  if command -v stop_web >/dev/null 2>&1; then stop_web; fi
  if command -v stop_agent >/dev/null 2>&1; then stop_agent; fi
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

# stop_smbd_restore 在退出时把 smbd 与 smb.conf 拉回原样。
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
# 不清的话后面每条 job 断言读到的都是上一轮的作业。
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
jobstartup() { jobget '",".join(d["data"]["result"]["data"].get("startup_paths") or [])'; }

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
}

# ---------- 页面读的接口 ----------

get_shares() {
  _code=$(api "$JAR" "$CSRF" GET "$SHARES_PATH")
  cp "$WORK/resp.json" "$WORK/shares-$1.json"
  printf '%s' "$_code"
}
sget() { jget "$WORK/shares-$1.json" "$2"; }

# blockers <卷 id> <后缀>：占用详情存成 $WORK/blockers-<后缀>.json，回显状态码。
#
# busy 与面板侧依赖要分开问：列表里的 dependencies 是给人看的摘要，而
# “为什么卸载不了”的结论在 /blockers 上（它还包含进程、dm/md、交换区）。
blockers() {
  _code=$(api "$JAR" "$CSRF" GET "$STORE/volumes/$1/blockers")
  cp "$WORK/resp.json" "$WORK/blockers-$2.json"
  printf '%s' "$_code"
}
blkget() { jget "$WORK/blockers-$1.json" "$2"; }

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
share_version() { get_share "$1" "$2" >/dev/null; shget "$2" 'd["data"]["share"]["version"]'; }

volumes() {
  _code=$(api "$JAR" "$CSRF" GET "$STORE/volumes")
  cp "$WORK/resp.json" "$WORK/volumes-$1.json"
  printf '%s' "$_code"
}
volsget() { jget "$WORK/volumes-$1.json" "$2"; }
# 取出我们那块卷在列表里的下标（列表里可能还有别的卷）。
vol_index() {
  volsget "$1" "next((i for i,v in enumerate(d[\"data\"][\"volumes\"]) if v[\"id\"]==\"$2\"), -1)"
}
# 取列表里第 $2 个卷的某个字段。下标越界（含 vol_index 给出的 -1）会打印 ERR:IndexError
# 而不是悄悄取到最后一个卷——Python 的负下标在这里正好是最坏的默认值。
vol_field() {
  volsget "$1" "d[\"data\"][\"volumes\"][$2 if 0 <= $2 < len(d[\"data\"][\"volumes\"]) else 10**9].get(\"$3\")"
}

volume() {
  _code=$(api "$JAR" "$CSRF" GET "$STORE/volumes/$1")
  cp "$WORK/resp.json" "$WORK/volume-$2.json"
  printf '%s' "$_code"
}
volget() { jget "$WORK/volume-$1.json" "$2"; }
current_vol_version() { volume "$1" "$2" >/dev/null; volget "$2" 'd["data"]["version"]'; }

# wait_revoked <share_id> <标签>：等监视循环把这条共享撤掉。打印等待秒数（超时打印 timeout）。
#
# 轮询的是页面读到的同一个接口，不是日志：日志里有这行只说明代理写了日志，
# 而“登记表上真的记下了撤销、并且 served 归了 false”才是要证明的东西。
wait_revoked() {
  _t0=$(date +%s)
  _deadline=$((_t0 + WATCH_WAIT))
  while [ "$(date +%s)" -lt "$_deadline" ]; do
    get_share "$1" "$2" >/dev/null
    if [ "$(shget "$2" 'bool(d["data"]["share"].get("runtime_revoked"))')" = "True" ] &&
       [ "$(shget "$2" 'd["data"]["share"]["config_applied"]')" = "False" ]; then
      printf '%s' "$(( $(date +%s) - _t0 ))"
      return 0
    fi
    sleep 2
  done
  printf 'timeout'
}

# ---------- 独立证据（不经过产品的解析器） ----------

blkid_field() { blkid -p -o value -s "$2" "$1" 2>/dev/null | head -1; }
fs_type() { blkid_field "$1" TYPE; }
is_mounted() {
  if [ -n "$(findmnt -rno TARGET --mountpoint "$1" 2>/dev/null)" ]; then echo True; else echo False; fi
}

smb_conf_sha() { sha256sum "$SMB_CONF" 2>/dev/null | cut -d' ' -f1; }
managed_sha() { sha256sum "$MANAGED_CONF" 2>/dev/null | cut -d' ' -f1; }
managed_mtime() { stat -c %Y "$MANAGED_CONF" 2>/dev/null || echo 0; }
# 进程的 umask 只能从 /proc 读。它的重要性见第 6 节与第 11 节：文件权限会被它掩掉。
proc_umask() { sed -n 's/^Umask:[[:space:]]*//p' "/proc/$1/status" 2>/dev/null | tr -d ' \r'; }
smb_conf_include_lines() { grep -cF "/etc/samba/upanel.d/" "$SMB_CONF" 2>/dev/null || true; }
smb_conf_sections() { grep '^\[' "$SMB_CONF" 2>/dev/null | tr -d '[]' | sort; }

# 托管文件里有没有这一段（段名不区分大小写，这里按 Samba 的归一化用 grep -i）。
managed_has_section() {
  if grep -qi "^\[$1\]" "$MANAGED_CONF" 2>/dev/null; then echo True; else echo False; fi
}

# 独立问 Samba：这份共享的段现在存不存在、它的键值是什么。
# 用 --parameter-name 而不是 grep 我们自己写的文本——“文件里写了什么”不等于
# “Samba 读出来是什么”。段不存在时 testparm 的退出码不为 0。
smb_effective() {
  testparm -s --suppress-prompt --section-name="$1" --parameter-name="$2" \
    "$SMB_CONF" 2>/dev/null | tr -d '\r' | sed '/^$/d' | tr '\n' ' ' | sed 's/ $//'
}
smb_section_exists() {
  if testparm -s --suppress-prompt --section-name="$1" --parameter-name="path" "$SMB_CONF" \
    >/dev/null 2>&1; then echo True; else echo False; fi
}
smb_service_state() {
  systemctl is-active smbd.service smbd.socket 2>/dev/null | tr -d '\r' | awk '
    $0=="active" || $0=="reloading" { a=1; k=1; next }
    $0=="inactive" || $0=="failed" || $0=="activating" || $0=="deactivating" ||
    $0=="unknown" || $0=="maintenance" { k=1 }
    END { if (a) print "active"; else if (k) print "inactive"; else print "unknown" }'
}
# smbstatus_tcons <共享名>：Samba 自己说有几种树连接接到这份共享上。
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

# —— 开机依赖：问 systemd 自己，而不是读我们写的那份文件 ——
dropin_exists() { if [ -f "$STARTUP_DROPIN" ]; then echo True; else echo False; fi; }
dropin_requires() { sed -n 's/^RequiresMountsFor=//p' "$STARTUP_DROPIN" 2>/dev/null | tr -d '\r'; }
systemd_requires() { systemctl show smbd.service -p RequiresMountsFor 2>/dev/null | sed -n 's/^RequiresMountsFor=//p'; }
systemd_after() { systemctl show smbd.service -p After 2>/dev/null | sed -n 's/^After=//p'; }
systemd_dropins() { systemctl show smbd.service -p DropInPaths 2>/dev/null | sed -n 's/^DropInPaths=//p'; }
mount_unit_of() { systemd-escape -p --suffix=mount "$1" 2>/dev/null; }

passwd_line() { getent passwd "$1" 2>/dev/null | head -1; }
pdb_has() {
  if pdbedit -L 2>/dev/null | cut -d: -f1 | grep -qxF "$1"; then echo True; else echo False; fi
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

# smbclient 的调用：认证走 `-A 认证文件`，不走 `-U 账号%密码`（后者会把密码放进 argv）。
smb_authfile() { # <账号> <密码变量名> -> 打印认证文件路径
  _f=$SECRETS_DIR/auth-$1
  eval "_pw=\${$2}"
  printf 'username=%s\npassword=%s\ndomain=\n' "$1" "$_pw" >"$_f"
  chmod 0600 "$_f"
  printf '%s' "$_f"
}
smb_ls() { smbclient "//127.0.0.1/$1" -A "$2" -m SMB3 -c 'ls' 2>&1; }
smb_put() { smbclient "//127.0.0.1/$1" -A "$2" -m SMB3 -c "put $3 $4" 2>&1; }

echo "== 0. 环境与安全守卫 =="
for b in "$AGENT" "$WEB" "$CTL"; do
  if [ -x "$b" ]; then pass "二进制存在且可执行 $(basename "$b")"; else fail "二进制存在且可执行 $(basename "$b")" "$b"; fi
done
for t in python3 curl runuser sha256sum findmnt lsblk sgdisk blkid wipefs udevadm \
         blockdev stat awk diff grep systemctl systemd-escape mount umount; do
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

# 开机依赖的 drop-in 也是上一轮的残留：这一轮要从“没有它”开始，
# 否则 §4 证明不了它是这次被写出来的。
if [ -f "$STARTUP_DROPIN" ]; then
  rm -f "$STARTUP_DROPIN"
  systemctl daemon-reload >/dev/null 2>&1 || true
  note "清掉上一轮遗留的 $STARTUP_DROPIN"
fi
if [ -e "$STARTUP_DROPIN" ]; then
  fail "起点：开机依赖 drop-in 不存在" "$STARTUP_DROPIN 还在"
else
  pass "起点：开机依赖 drop-in 不存在"
fi
expect_eq "起点：systemd 里 smbd 没有 RequiresMountsFor" "" "$(systemd_requires)"

lsblk -o NAME,TYPE,SIZE,FSTYPE,MOUNTPOINTS >"$WORK/logs/lsblk-initial.txt" 2>&1
rm -rf "$VOLUME_ROOT"/* 2>/dev/null
install -d -m 0755 "$VOLUME_ROOT"
wipefs -a "/dev/$BLANK_A" >/dev/null 2>&1
sgdisk --zap-all "/dev/$BLANK_A" >/dev/null 2>&1
blockdev --rereadpt "/dev/$BLANK_A" >/dev/null 2>&1
udevadm settle --timeout=5 >/dev/null 2>&1
pass "一次性盘 /dev/$BLANK_A 清成空白"

pdbedit -x -u "$ACCT_RW" >/dev/null 2>&1 || true
userdel "$ACCT_RW" >/dev/null 2>&1 || true
if getent passwd "$ACCT_RW" >/dev/null 2>&1 || pdbedit -L 2>/dev/null | grep -qE "^($ACCT_RW):"; then
  fail "起点：受管理账号已清空" "系统里还有 $ACCT_RW 残留"
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
# 监视循环必须在启动时就存在，并且把“不自动恢复”这件事写在日志里。
WATCH_LINE=$(grep -a '已启用共享卷监视' "$AGENT_LOG" | head -1)
expect_ne "代理日志声明启用共享卷监视" "" "$WATCH_LINE"
expect_contains "监视日志写明锚定的是同一把配置锁" "samba:config" "$WATCH_LINE"
expect_contains "监视日志写明不自动恢复" "不自动恢复" "$WATCH_LINE"

runuser -u "$WEB_USER" -- curl -s --unix-socket "$SOCK" \
  http://agent/internal/v1/capabilities >"$PAYLOADS/agent-capabilities.json" 2>/dev/null
AGENT_ACTIONS=$(jget "$PAYLOADS/agent-capabilities.json" \
  '(d.get("data") or d).get("supported_actions") or []')
for a in share.apply share.disable share.delete storage.volume.unmount storage.volume.forget \
         storage.filesystem.format; do
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

PW_FILE=$SECRETS_DIR/passwords.txt
umask 077
printf '%s\n%s\n' "$ACCT_RW_PW" "$PASSWORD" >"$PW_FILE"
chmod 0600 "$PW_FILE"

echo
echo "== 2. 夹具：空盘 → GPT → 分区 → ext4 → 导入 → 挂载；受管理账号 =="
PART_NAME=data
run_action storage.partition_table.initialize \
  "{\"device_id\":\"$BLANK_A\",\"table_type\":\"gpt\"}" init-a
expect_eq "初始化分区表预检允许" "True" "$PRE_ALLOWED"
expect_eq "初始化分区表任务终态" "succeeded" "$JOB_STATE"
run_action storage.partition.create \
  "{\"device_id\":\"$BLANK_A\",\"start_bytes\":\"$PART_START\",\"size_bytes\":\"$PART_MAIN\",\"name\":\"$PART_NAME\"}" create-a
expect_eq "建分区任务终态" "succeeded" "$JOB_STATE"
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

printf '%s' "$ACCT_RW_PW" >"$SECRETS_DIR/$ACCT_RW.pw"
chmod 0600 "$SECRETS_DIR/$ACCT_RW.pw"
_sec_code=$(put_secret smb_password "$SECRETS_DIR/$ACCT_RW.pw" "accept-p08-3-$ACCT_RW")
cp "$WORK/secret.json" "$PAYLOADS/secret-$ACCT_RW.json"
expect_eq "存 $ACCT_RW 的密码走秘密通道" "201" "$_sec_code"
SEC_RW=$(jget "$PAYLOADS/secret-$ACCT_RW.json" 'd["data"]["secret_ref"]')
expect_ne "拿得到 $ACCT_RW 的 secret_ref" "" "$SEC_RW"
run_action share.account.apply \
  "{\"account_id\":null,\"name\":\"$ACCT_RW\",\"secret_ref\":\"$SEC_RW\",\"disabled\":false}" acct-rw
expect_eq "建 $ACCT_RW 任务终态" "succeeded" "$JOB_STATE"
ACCT_ID_RW=$(jobdata account_id)
expect_eq "独立证据：账号在 passdb 里" "True" "$(pdb_has "$ACCT_RW")"
AUTH_RW=$(smb_authfile "$ACCT_RW" ACCT_RW_PW)

echo
echo "== 3. 基线：共享应用成功，客户端真的能访问 =="
DEF_RW=$(printf '{"name":"%s","protocol":"smb","volume_id":"%s","relative_path":"%s","enabled":true,"read_only":false,"allowed_cidrs":["127.0.0.0/8"],"smb_users":[{"account_id":"%s","access":"write"}]}' \
  "$SHARE_NAME" "$VOL_ID" "$REL_PATH" "$ACCT_ID_RW")
run_action share.apply "{\"share_id\":null,\"version\":null,\"definition\":$DEF_RW}" apply-rw
expect_eq "应用共享预检允许" "True" "$PRE_ALLOWED"
expect_verified_served "应用共享"
SHARE_ID=$(jobdata share_id)
SHARE_VER=$(jobdata version)
expect_ne "拿到共享标识" "" "$SHARE_ID"
expect_eq "独立证据：托管文件里有这一段" "True" "$(managed_has_section "$SHARE_NAME")"
expect_eq "独立证据：Samba 读回的 path" "$MP/$REL_PATH" "$(smb_effective "$SHARE_NAME" path)"

printf 'p08-3 real content\n' >"$WORK/probe.txt"
PUT_OUT=$(smb_put "$SHARE_NAME" "$AUTH_RW" "$WORK/probe.txt" "probe.txt")
printf '%s\n' "$PUT_OUT" >"$PAYLOADS/smbclient-put.txt"
expect_contains "客户端能上传（共享真的在服务）" "putting file" "$PUT_OUT"
LS_OUT=$(smb_ls "$SHARE_NAME" "$AUTH_RW")
printf '%s\n' "$LS_OUT" >"$PAYLOADS/smbclient-ls.txt"
expect_contains "客户端能列出刚上传的文件" "probe.txt" "$LS_OUT"

# 基线哈希：之后每一次“无人工确认的写入”（运行时撤销）都要拿它来比对。
SMB_SHA_BASE=$(smb_conf_sha)
MANAGED_SHA_BASE=$(managed_sha)
NOTE_LINE=$(printf 'smb.conf 基线 sha256 = %s\n托管文件基线 sha256 = %s\n' "$SMB_SHA_BASE" "$MANAGED_SHA_BASE")
printf '%s\n' "$NOTE_LINE" >"$WORK/logs/baseline-hashes.txt"

echo
echo "== 4. 开机依赖：drop-in 写出来，而且 systemd 自己认了这条顺序 =="
expect_eq "作业里 startup_deps 是 written" "written" "$(jobobserved startup_deps)"
expect_eq "作业里 startup_paths 就是卷的挂载点" "$MP" "$(jobstartup)"
expect_eq "独立证据：drop-in 文件存在" "True" "$(dropin_exists)"
expect_eq "独立证据：drop-in 目录权限 0755" "755" "$(stat -c '%a' "$STARTUP_DROPIN_DIR" 2>/dev/null)"
expect_eq "独立证据：drop-in 权限 0644" "644" "$(stat -c '%a' "$STARTUP_DROPIN" 2>/dev/null)"
cp "$STARTUP_DROPIN" "$PAYLOADS/startup-dropin.conf" 2>/dev/null
expect_contains "独立证据：drop-in 是 [Unit] 片段" "[Unit]" "$(cat "$STARTUP_DROPIN" 2>/dev/null)"
expect_eq "独立证据：drop-in 里的 RequiresMountsFor" "$MP" "$(dropin_requires)"
expect_eq "独立证据：drop-in 里只有一行 RequiresMountsFor" "1" \
  "$(grep -c '^RequiresMountsFor=' "$STARTUP_DROPIN" 2>/dev/null | tr -d ' ')"

# 这一条是本节的核心：不是“我们写了文件”，而是“systemd 加载了它并算出了顺序”。
expect_contains "systemd 的 DropInPaths 里有这份 drop-in" "$STARTUP_DROPIN" "$(systemd_dropins)"
expect_eq "systemd 解析出的 RequiresMountsFor" "$MP" "$(systemd_requires)"
MOUNT_UNIT=$(mount_unit_of "$MP")
expect_ne "挂载单元名由路径派生" "" "$MOUNT_UNIT"
expect_contains "systemd 的 After 里出现该挂载单元（顺序约束真的生效）" "$MOUNT_UNIT" "$(systemd_after)"
printf 'drop-in:\n%s\n\nDropInPaths: %s\nRequiresMountsFor: %s\nAfter: %s\n' \
  "$(cat "$STARTUP_DROPIN" 2>/dev/null)" "$(systemd_dropins)" "$(systemd_requires)" \
  "$(systemd_after)" >"$WORK/logs/startup-deps.txt"

echo
echo "== 5. 运行中脱盘：外部 umount → 一个监视周期内撤销 =="
# 外部 umount：不经过面板的任何接口。这正是“虚拟盘被摘掉 / 有人手工卸载”的形状。
sync
umount "$MP" 2>&1 | sed 's/^/      umount: /'
expect_eq "独立证据：findmnt 说这个挂载点没挂" "False" "$(is_mounted "$MP")"

# 在“本该是共享目录”的那条路径上造一个属于根分区的目录与标记文件。
# 卷挂着的时候那条路径在卷里；现在它是根分区上的空目录——正是产品必须拒绝共享的东西。
mkdir -p "$MP/$REL_PATH"
printf 'this file lives on the root filesystem\n' >"$MP/$REL_PATH/rootfs-marker.txt"
expect_eq "独立证据：根分区上的标记文件确实存在（空目录不是空的）" "True" \
  "$([ -f "$MP/$REL_PATH/rootfs-marker.txt" ] && echo True || echo False)"
note "此时 $MP/$REL_PATH 属于根分区，不是那块卷"

WATCH_ELAPSED=$(wait_revoked "$SHARE_ID" revoke)
cp "$WORK/share-revoke.json" "$PAYLOADS/share-after-revoke.json"
note "从 umount 到登记表记录撤销：${WATCH_ELAPSED}s（监视间隔 30s，存储扫描 5s）"
case "$WATCH_ELAPSED" in
  timeout) fail "监视循环在一个周期内撤销了共享" "等了 ${WATCH_WAIT}s 仍没有 runtime_revoked" ;;
  *) pass "监视循环在一个周期内撤销了共享（${WATCH_ELAPSED}s）" ;;
esac

expect_eq "登记表：config_applied 归 false" "False" "$(shget revoke 'd["data"]["share"]["config_applied"]')"
expect_eq "撤销现场记的是这块卷" "$VOL_ID" "$(shget revoke 'd["data"]["share"]["runtime_revoked"]["volume_id"]')"
expect_eq "撤销现场记的是卷名" "数据卷" "$(shget revoke 'd["data"]["share"]["runtime_revoked"]["volume_name"]')"
expect_eq "撤销现场记的是 unmounted" "unmounted" \
  "$(shget revoke 'd["data"]["share"]["runtime_revoked"]["volume_state"]')"
expect_ne "撤销现场带时间" "" "$(shget revoke 'd["data"]["share"]["runtime_revoked"]["at"]')"
expect_contains "撤销原因说清是挂载状态" "没有挂载" \
  "$(shget revoke 'd["data"]["share"]["runtime_revoked"]["reason"]')"

# —— 独立证据：配置里没有了、Samba 不知道这段、客户端看到的是“共享不存在”
expect_eq "独立证据：托管文件里这段已经没了" "False" "$(managed_has_section "$SHARE_NAME")"
expect_eq "独立证据：Samba 里这一段不存在" "False" "$(smb_section_exists "$SHARE_NAME")"
REVOKED_LS=$(smb_ls "$SHARE_NAME" "$AUTH_RW")
printf '%s\n' "$REVOKED_LS" >"$PAYLOADS/smbclient-after-revoke.txt"
expect_contains "客户端拿到的是“共享不存在”" "NT_STATUS_BAD_NETWORK_NAME" "$REVOKED_LS"
# 这一条才是“不会共享系统盘空目录”的机器判据：客户端没有看到那个空目录里的标记文件。
expect_not_contains "客户端没有看到根分区上的标记文件" "rootfs-marker.txt" "$REVOKED_LS"
expect_not_contains "客户端也没列出一个空目录" "blocks of size" "$REVOKED_LS"
expect_eq "独立证据：Samba 自己说这份共享上的树连接是 0" "0" "$(smbstatus_tcons "$SHARE_NAME")"

# —— 撤销是那次唯一的无人工确认写入：只动面板独占的文件
expect_ne "撤销确实重写了托管文件" "$MANAGED_SHA_BASE" "$(managed_sha)"
expect_eq "撤销没有动 smb.conf（第三方共享逐字节不变）" "$SMB_SHA_BASE" "$(smb_conf_sha)"
PREFIX_OK=$(python3 - "$SMB_CONF" "$WORK/logs/smb-conf-initial.txt" <<'PY'
import sys
new = open(sys.argv[1], 'rb').read()
old = open(sys.argv[2], 'rb').read()
print("True" if new.startswith(old) else "False")
PY
)
expect_eq "独立证据：smb.conf 仍是初始内容的前缀延伸" "True" "$PREFIX_OK"
expect_eq "独立证据：include 行仍然只有一行" "1" "$(smb_conf_include_lines)"
# 撤销之后没有共享需要等挂载点，开机依赖随之收回（这正是 §4 那条路径的逆操作）。
expect_eq "撤销之后 drop-in 被收回（没有共享要等这个挂载点了）" "False" "$(dropin_exists)"
expect_eq "撤销之后 systemd 里不再要求 RequiresMountsFor" "" "$(systemd_requires)"
expect_contains "代理日志记下这次撤销" "卷离线，相关共享已从托管配置撤销" \
  "$(grep -a '卷离线' "$AGENT_LOG" | tail -1)"
expect_contains "代理日志写明不会自动写回" "不会自动写回" \
  "$(grep -a '卷离线' "$AGENT_LOG" | tail -1)"

echo
echo "== 6. 撤销之后再来一轮：不写、不重载、不刷新时间（用重启走“启动首轮”） =="
MANAGED_SHA_R1=$(managed_sha)
MANAGED_MTIME_R1=$(managed_mtime)
REVOKED_AT_R1=$(shget revoke 'd["data"]["share"]["runtime_revoked"]["at"]')
UPDATED_AT_R1=$(shget revoke 'd["data"]["share"]["updated_at"]')
# 重启代理：监视循环的第一轮立刻跑（不等 30 秒），这同时证明“开机时盘就没挂上”
# 的路径也走同一套判定。
stop_agent
if start_agent; then pass "代理重启（走启动首轮的对账）"; else fail "代理重启" "$(tail -5 "$AGENT_LOG")"; fi
# 这一次重启是**故意的**：脚本在上面设了 umask 077，与生产的代理单元 UMask=0077 一致。
# 真机缺陷就藏在这里——`os.OpenFile(..., 0644)` 的权限位会被 umask 掩掉，于是托管配置与
# 开机依赖都落成 0600（Q243）。第 11 节那条 0644 断言得在 umask 077 下才有意义。
expect_eq "这次重启起的代理 umask 是 0077（与生产单元 UMask=0077 一致）" "0077" \
  "$(proc_umask "$AGENT_PID")"
sleep 4
get_share "$SHARE_ID" revoke2 >/dev/null
expect_eq "第二轮之后托管文件哈希不变" "$MANAGED_SHA_R1" "$(managed_sha)"
expect_eq "第二轮之后托管文件 mtime 不变" "$MANAGED_MTIME_R1" "$(managed_mtime)"
expect_eq "第二轮之后撤销时间仍是第一次的" "$REVOKED_AT_R1" \
  "$(shget revoke2 'd["data"]["share"]["runtime_revoked"]["at"]')"
expect_eq "第二轮之后 updated_at 没有被推到当下" "$UPDATED_AT_R1" \
  "$(shget revoke2 'd["data"]["share"]["updated_at"]')"
expect_eq "第二轮之后这一段仍然不在配置里" "False" "$(managed_has_section "$SHARE_NAME")"
expect_eq "第二轮之后 config_applied 仍然是 false" "False" \
  "$(shget revoke2 'd["data"]["share"]["config_applied"]')"

echo
echo "== 7. 卷没挂的时候再点“应用”：预检必须挡下 =="
SHARE_VER=$(share_version "$SHARE_ID" now)
PRE_CODE=$(preflight share.apply "{\"share_id\":\"$SHARE_ID\",\"version\":\"$SHARE_VER\",\"definition\":$DEF_RW}")
cp "$WORK/preflight.json" "$PAYLOADS/preflight-apply-offline.json"
expect_eq "卷没挂时应用共享的预检被拒" "False" "$(pallowed)"
expect_contains "挡下它的码是 VOLUME_NOT_MOUNTED" "VOLUME_NOT_MOUNTED" "$(pall)"
expect_contains "理由说清挂载点此时是系统盘上的空目录" "空目录" "$(pwhy)"
note "预检没有放行，所以这一节不提交任何作业（提交被挡下的预检本身就该失败）"

echo
echo "== 8. 卷回来不自动恢复；人再应用一次才回来（而且服务的是卷里的内容） =="
# 按设备挂回来，不走 fstab 的按目标挂载：导入时 automount=false，fstab 那一行带
# noauto（开机不由 local-fs 拉起来），所以这里必须显式给设备。
mount "/dev/${BLANK_A}1" "$MP" 2>&1 | sed 's/^/      mount: /'
expect_eq "独立证据：卷又挂上了" "True" "$(is_mounted "$MP")"
# 等一个完整的监视周期：如果对账会自动写回配置，这一段就该重新出现。
note "等一个监视周期（$WATCH_WAIT 秒内）确认它不会自动恢复……"
MANAGED_SHA_R2=$(managed_sha)
sleep 35
get_share "$SHARE_ID" after-remount >/dev/null
expect_eq "一个周期之后这一段仍然不在配置里（不自动恢复）" "False" "$(managed_has_section "$SHARE_NAME")"
expect_eq "一个周期之后托管文件哈希不变（没有静默写回）" "$MANAGED_SHA_R2" "$(managed_sha)"
expect_eq "一个周期之后撤销现场仍然保留" "True" \
  "$(shget after-remount 'bool(d["data"]["share"].get("runtime_revoked"))')"
expect_eq "一个周期之后 shared 的 config_applied 仍是 false" "False" \
  "$(shget after-remount 'd["data"]["share"]["config_applied"]')"
expect_eq "独立证据：这期间 Samba 里仍然没有这一段" "False" "$(smb_section_exists "$SHARE_NAME")"

SHARE_VER=$(share_version "$SHARE_ID" now)
run_action share.apply "{\"share_id\":\"$SHARE_ID\",\"version\":\"$SHARE_VER\",\"definition\":$DEF_RW}" apply-back
expect_eq "人重新应用：预检允许" "True" "$PRE_ALLOWED"
expect_verified_served "人重新应用"
expect_eq "重新应用之后 startup_deps 重新是 written（drop-in 装回来）" "written" \
  "$(jobobserved startup_deps)"
expect_eq "独立证据：drop-in 又在了" "True" "$(dropin_exists)"
expect_eq "独立证据：systemd 又重新要求 RequiresMountsFor" "$MP" "$(systemd_requires)"
LS_BACK=$(smb_ls "$SHARE_NAME" "$AUTH_RW")
printf '%s\n' "$LS_BACK" >"$PAYLOADS/smbclient-after-reapply.txt"
expect_contains "客户端又连得上了，并且看到卷里那个文件" "probe.txt" "$LS_BACK"
# 关键：看到的是**卷里**的内容，不是根分区上那个标记文件（它被挂载遮住了）。
expect_not_contains "客户端没有看到根分区上的标记文件（服务的是卷）" "rootfs-marker.txt" "$LS_BACK"
expect_eq "独立证据：标记文件此刻被挂载遮住（但确实还在根分区上）" "False" \
  "$([ -f "$MP/$REL_PATH/rootfs-marker.txt" ] && echo True || echo False)"

echo
echo "== 9. 依赖阻断的三个阈值：卸载、格式化、退出管理 =="
volumes list >/dev/null
VIDX=$(vol_index list "$VOL_ID")
expect_ne "卷在列表里" "-1" "$VIDX"
expect_eq "列表里这块卷的 id" "$VOL_ID" "$(vol_field list "$VIDX" "id")"
expect_eq "列表里这块卷此刻是 mounted" "mounted" "$(vol_field list "$VIDX" "observed_state")"
expect_contains "列表里的 dependencies 点出了依赖它的共享" "share:$SHARE_NAME" \
  "$(volsget list '",".join(x["kind"]+":"+x["name"] for x in (d["data"]["volumes"]['"$VIDX"'].get("dependencies") or []))')"

VOL_VER=$(current_vol_version "$VOL_ID" u1)
PRE_CODE=$(preflight storage.volume.unmount "{\"volume_id\":\"$VOL_ID\",\"version\":\"$VOL_VER\"}")
cp "$WORK/preflight.json" "$PAYLOADS/preflight-unmount-blocked.json"
expect_eq "有启用共享时卸载预检被拒" "False" "$(pallowed)"
expect_contains "挡下它的是 VOLUME_IN_USE_BY_SHARE" "VOLUME_IN_USE_BY_SHARE" "$(pall)"
expect_contains "理由里点了共享的名字" "$SHARE_NAME" "$(pwhy)"

PRE_CODE=$(preflight storage.filesystem.format \
  "{\"device_id\":\"${BLANK_A}1\",\"filesystem\":\"ext4\",\"label\":\"$LABEL\"}")
cp "$WORK/preflight.json" "$PAYLOADS/preflight-format-blocked.json"
expect_eq "有共享时格式化预检被拒" "False" "$(pallowed)"
expect_contains "挡下格式化的是 VOLUME_IN_USE_BY_SHARE" "VOLUME_IN_USE_BY_SHARE" "$(pall)"

PRE_CODE=$(preflight storage.volume.forget "{\"volume_id\":\"$VOL_ID\",\"version\":\"$VOL_VER\"}")
cp "$WORK/preflight.json" "$PAYLOADS/preflight-forget-blocked.json"
expect_eq "有共享时退出管理预检被拒" "False" "$(pallowed)"
expect_contains "挡下退出管理的是 VOLUME_HAS_DEPENDENCIES" "VOLUME_HAS_DEPENDENCIES" "$(pall)"

volumes busy >/dev/null
VIDX=$(vol_index busy "$VOL_ID")
expect_eq "卷的占用详情可读" "200" "$(blockers "$VOL_ID" busy)"
expect_eq "占用详情里 busy=true" "True" "$(blkget busy 'bool(d["data"]["busy"])')"
expect_contains "占用详情里 shares 点出了共享名" "$SHARE_NAME" \
  "$(blkget busy '",".join(s.get("name","") for s in (d["data"]["shares"] or []))')"
expect_eq "列表里这条依赖仍然是 share 类" "share" \
  "$(volsget busy 'next((x["kind"] for x in (d["data"]["volumes"]['"$VIDX"'].get("dependencies") or [])), "")')"

# —— 关掉共享之后：卸载放行（只挡“启用中”），退出管理仍然被挡（任何共享都算）
SHARE_VER=$(share_version "$SHARE_ID" now)
run_action share.disable "{\"share_id\":\"$SHARE_ID\",\"version\":\"$SHARE_VER\"}" disable-rw
expect_eq "禁用共享任务终态" "succeeded" "$JOB_STATE"
expect_eq "禁用之后 served=false" "False" "$(jobobserved served)"
expect_eq "禁用之后 startup_deps 是 removed（没有共享要等挂载点了）" "removed" \
  "$(jobobserved startup_deps)"
expect_eq "独立证据：drop-in 被删掉" "False" "$(dropin_exists)"
expect_eq "独立证据：systemd 里不再要求 RequiresMountsFor" "" "$(systemd_requires)"
expect_eq "独立证据：Samba 里这一段没了" "False" "$(smb_section_exists "$SHARE_NAME")"

VOL_VER=$(current_vol_version "$VOL_ID" u2)
PRE_CODE=$(preflight storage.volume.unmount "{\"volume_id\":\"$VOL_ID\",\"version\":\"$VOL_VER\"}")
cp "$WORK/preflight.json" "$PAYLOADS/preflight-unmount-disabled.json"
expect_eq "共享只被禁用（没删）时卸载预检放行" "True" "$(pallowed)"
PRE_CODE=$(preflight storage.volume.forget "{\"volume_id\":\"$VOL_ID\",\"version\":\"$VOL_VER\"}")
cp "$WORK/preflight.json" "$PAYLOADS/preflight-forget-disabled.json"
expect_eq "禁用中的共享仍然挡住退出管理" "False" "$(pallowed)"
expect_contains "挡住退出管理的仍是 VOLUME_HAS_DEPENDENCIES" "VOLUME_HAS_DEPENDENCIES" "$(pall)"
PRE_CODE=$(preflight storage.filesystem.format \
  "{\"device_id\":\"${BLANK_A}1\",\"filesystem\":\"ext4\",\"label\":\"$LABEL\"}")
cp "$WORK/preflight.json" "$PAYLOADS/preflight-format-disabled.json"
expect_eq "禁用中的共享仍然挡住格式化" "False" "$(pallowed)"
expect_contains "挡住格式化的仍是 VOLUME_IN_USE_BY_SHARE" "VOLUME_IN_USE_BY_SHARE" "$(pall)"

echo
echo "== 10. 重新启用把开机依赖装回来；删掉最后一条共享之后依赖清单清零 =="
SHARE_VER=$(share_version "$SHARE_ID" now)
run_action share.apply "{\"share_id\":\"$SHARE_ID\",\"version\":\"$SHARE_VER\",\"definition\":$DEF_RW}" reenable-rw
expect_verified_served "重新启用"
expect_eq "重新启用之后 startup_deps 是 written" "written" "$(jobobserved startup_deps)"
expect_eq "独立证据：drop-in 回来了" "True" "$(dropin_exists)"
# drop-in 是在 umask 0077 的代理进程里写出来的（第 6 节那次重启），权限必须是 0644：
# 契约里写的就是 0644，而权限被 umask 掩成 0600 是这个缺陷的第二种形态（Q243）。
expect_eq "drop-in 权限是 0644（没被 umask 掩成 0600）" "644" \
  "$(stat -c '%a' "$STARTUP_DROPIN" 2>/dev/null)"
expect_eq "独立证据：systemd 里又要求 RequiresMountsFor" "$MP" "$(systemd_requires)"

SHARE_VER=$(share_version "$SHARE_ID" now)
run_action share.delete "{\"share_id\":\"$SHARE_ID\",\"version\":\"$SHARE_VER\"}" delete-rw
expect_eq "删除任务终态" "succeeded" "$JOB_STATE"
expect_eq "删除之后 startup_deps 是 removed" "removed" "$(jobobserved startup_deps)"
expect_eq "删除之后共享详情是 404" "404" "$(get_share "$SHARE_ID" gone)"
expect_eq "独立证据：drop-in 不在了" "False" "$(dropin_exists)"
expect_eq "独立证据：systemd 里 RequiresMountsFor 空了" "" "$(systemd_requires)"
expect_eq "共享列表回到空" "0" "$(get_shares final >/dev/null; sget final 'len(d["data"]["shares"])')"

volumes clear >/dev/null
VIDX=$(vol_index clear "$VOL_ID")
expect_ne "依赖清零之后卷仍在列表里" "-1" "$VIDX"
expect_eq "依赖清零之后列表里的 dependencies 是空的" "0" \
  "$(volsget clear 'len(d["data"]["volumes"]['"$VIDX"'].get("dependencies") or [])')"
expect_eq "依赖清零之后占用详情可读" "200" "$(blockers "$VOL_ID" clear)"
expect_eq "依赖清零之后占用详情里 busy=false（挂着但没人用不算忙）" "False" \
  "$(blkget clear 'bool(d["data"]["busy"])')"
expect_eq "依赖清零之后占用详情里 shares 是空数组" "0" \
  "$(blkget clear 'len(d["data"]["shares"] or [])')"

# 断开了依赖，卸载就该能走完：这不是“永远挡住”，是“有依赖时挡住”。
VOL_VER=$(current_vol_version "$VOL_ID" u3)
run_action storage.volume.unmount "{\"volume_id\":\"$VOL_ID\",\"version\":\"$VOL_VER\"}" unmount-final
expect_eq "没有依赖时卸载预检允许" "True" "$PRE_ALLOWED"
expect_eq "卸载任务终态" "succeeded" "$JOB_STATE"
expect_eq "独立证据：卷已经卸下来了" "False" "$(is_mounted "$MP")"

# 收尾：把根分区上造出来的标记文件删掉（它本来就是本脚本的夹具，不该留在机器上）。
rm -f "$MP/$REL_PATH/rootfs-marker.txt" 2>/dev/null
rmdir "$MP/$REL_PATH" "$MP/team" 2>/dev/null
note "已清理根分区上的夹具目录 $MP/$REL_PATH"

echo
echo "== 11. 收尾不变量 =="
expect_eq "整轮下来 include 仍然只有一行" "1" "$(smb_conf_include_lines)"
CUR_SECTIONS=$(smb_conf_sections)
MISSING=0
for s in $SMB_SECTIONS0; do
  case "$CUR_SECTIONS" in
    *"$s"*) : ;;
    *) MISSING=$((MISSING + 1)) ;;
  esac
done
expect_eq "整轮下来 smb.conf 原有段名一个都没少" "0" "$MISSING"
PREFIX_OK=$(python3 - "$SMB_CONF" "$WORK/logs/smb-conf-initial.txt" <<'PY'
import sys
new = open(sys.argv[1], 'rb').read()
old = open(sys.argv[2], 'rb').read()
print("True" if new.startswith(old) else "False")
PY
)
expect_eq "整轮下来 smb.conf 仍是初始内容的前缀延伸（逐字节）" "True" "$PREFIX_OK"
expect_eq "托管 include 目录权限是 0755" "755" "$(stat -c '%a' "$SMB_INCLUDE_DIR" 2>/dev/null)"
# 与第 6 节呼应：代理是带着 umask 0077 跑的，权限必须仍是 0644（Q243）。
expect_eq "托管文件权限是 0644（没被 umask 掩成 0600）" "644" "$(stat -c '%a' "$MANAGED_CONF" 2>/dev/null)"

LOG_HITS=$(( $(hits_in_file "$WEB_LOG") + $(hits_in_file "$AGENT_LOG") ))
expect_eq "两个日志里没有共享密码明文" "0" "$LOG_HITS"
expect_eq "两个数据库（含 WAL）里没有共享密码明文" "0" "$(hits_in_databases)"
expect_eq "任何进程的 argv/environ 里没有共享密码明文" "0" "$(hits_in_processes)"

if kill -0 "$AGENT_PID" 2>/dev/null; then pass "代理整轮没有退出"; else fail "代理整轮没有退出" "进程没了"; fi
if [ -S "$SOCK" ]; then pass "socket 仍然在"; else fail "socket 仍然在" "$SOCK 消失了"; fi
expect_eq "收尾：smbd 在运行（本脚本没把它停在停着的状态）" "active" "$(smb_service_state)"

echo
echo "== 12. 明确不验（NOT RUN） =="
notrun "真的重启一次看开机窗口" "本脚本中途不会被 reboot 打断；由 p08-3-reboot-order.sh 分两阶段验收"
notrun "观测不明（SHARE_VOLUME_STATE_UNKNOWN）" "真机要造出“卷挂着但读不出来”得先弄坏采集器；单测覆盖"
notrun "依赖查询失败（VOLUME_DEPENDENCIES_UNKNOWN）" "代理总是接了这个查询；单测覆盖 internal/storage/deps_test.go"
notrun "跨机器 SMB 客户端" "P00-VM-D（客户端 VM）尚未建立"
notrun "浏览器里的真实渲染" "本脚本验的是页面读到的载荷"

echo
echo "== 13. 结论 =="
printf 'PASS=%s FAIL=%s NOT RUN=%s\n' "$PASSED" "$FAILED" "$NOTRUN"
if [ "$FAILED" -ne 0 ]; then
  printf '失败项：%s\n' "$FAILED_NAMES"
  exit 1
fi
exit 0
