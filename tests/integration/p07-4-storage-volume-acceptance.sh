#!/bin/sh
# P07.4 Linux 验收脚本：受管理卷（导入、挂载/卸载、UUID 校验、fstab 最小修改、
# 原子替换/备份/回滚、开机自动挂载、繁忙占用列表）。
#
# 与 p07-1 / p07-2 / p07-3 的分工：那三份证明“该拒的拒”“分区表写对了”“文件系统做出来了”；
# 本脚本证明“把它纳管之后，fstab 里那一行、系统里的那个挂载点、开机时的那个行为，
# 与登记记录三者一致，而且卸不掉的时候能把占用它的进程指出来”。
#
# **退出码 0 不算成功**：每个写动作都要求任务终态 succeeded、result 为 verified，
# 并且每一条断言都配一条**独立证据**（不读产品的解析结果）：
#   - 文件系统身份：blkid -p（产品按 CONTRACTS §6 不调用 blkid，所以它是另一个证人）；
#   - fstab 内容：直接读 /etc/fstab 的字节（托管块标记、条目行、块外前缀的原样性）；
#   - 备份与原子替换：备份目录里的文件内容与写之前的 /etc/fstab 逐字节相同，
#     且 /etc/fstab 的 mode/owner 不被顺手改掉、候选临时文件不留痕；
#   - 挂载事实：findmnt 与 /proc/self/mountinfo（内核视图），不是产品自己的报告；
#   - 占用：产品说“pid=N 在用”，脚本就到 /proc/N/fd 里核对那个 fd 真的指向这个卷——
#     两边对得上才算它没编，而且脚本杀的就是产品点名的那个 pid。
#
# 分两段运行（“开机自动挂载”只能靠一次真重启证明）：
#   第一段（默认）：
#     sudo BIN_DIR=/var/tmp/upanel-bin SPARE_DISKS="sdb sdc" \
#          sh tests/integration/p07-4-storage-volume-acceptance.sh
#     结束时卷是“已登记 + automount 打开 + 挂载着 + 有一个标记文件”，
#     脚本把跨重启需要的期望值写进 $WORK/state.env 然后停下。
#   重启后用同一个 WORK 跑第二段：
#     sudo BIN_DIR=/var/tmp/upanel-bin PHASE=post-boot sh tests/integration/p07-4-storage-volume-acceptance.sh
#     第二段**先证明重启后它自己挂上了，再启动任何 upanel 进程**。
#
# 可选环境变量：
#   PHASE=main|post-boot   运行哪一段（默认 main）
#   PORT=19474             Web 监听端口（与 p07-1/2/3 不同，避免撞车）
#   SPARE_DISKS            两块**一次性**空盘设备名（默认 "sdb sdc"）
#   WORK=/var/tmp/...      工作目录（两段必须一致）
#   BIN_DIR                两段必须一致，而且要**扛过一次重启**（/tmp 在重启后是空的），
#                          并且对 Web 账号（upanel-web）可穿越、可执行——放在
#                          /home/<user> 下会被 0750 挡住（Q203）。
#
# 退出码：0 全部通过；1 有失败项；2 环境不满足（非 root / 盘不安全 / 缺工具 / 缺上一段的状态）。
#
# 明确不覆盖（NOT RUN，原因写在这里，不在脚本里假装通过）：
#   - **导入后登记记录写失败的回滚**（fstab 已替换、SaveVolume 失败 → 用备份换回去）：
#     要让代理的 SQLite 正好在“fstab 已换、记录还没写”这一瞬失败，只能 chattr +i
#     数据库或拔盘，两种都会污染后面所有用例的代理状态。单测
#     TestVolumeImportRollsBackWhenSaveFails / …WhenReloadFails 覆盖这两条路径。
#   - **候选校验失败后保留原文件**：本脚本用“托管块之外塞一行解析不了的垃圾”触发
#     findmnt --verify 失败（parse error），覆盖的正是“候选不合格 → 原文件不动”。
#     mount 选项写错、字段缺失这类 findmnt 的其它抱怨没有逐一构造。
#   - **目录 fsync 失败**（rename 已成功、目录没同步）：Error 只是警告，无法在 ext4 上
#     稳定制造；生产行为是“已替换但可能回退”，由单测的 fsyncDir 接缝覆盖。
#   - **umount 之后拓扑重扫失败**：需要采集器同时坏掉，单测覆盖。
#   - XFS 卷的导入/挂载：P07.3 已验 XFS 格式化；本脚本只用 ext4 走完整生命周期，
#     XFS 的 fstab pass=0 由单测与 P07.3 的证据覆盖。

set -u

PHASE=${PHASE:-main}
BIN_DIR=${BIN_DIR:-/var/tmp/upanel-bin}

if [ "$(id -u)" -ne 0 ]; then
  printf '本脚本必须以 root 运行：sudo BIN_DIR=%s sh %s\n' "$BIN_DIR" "$0" >&2
  exit 2
fi

AGENT="$BIN_DIR/upanel-agent"
WEB="$BIN_DIR/upanel-web"
CTL="$BIN_DIR/upanelctl"

WORK=${WORK:-/var/tmp/upanel-accept-p07-4}
PAYLOADS=$WORK/payloads
STATE=$WORK/state.env
RUN_DIR=/run/upanel
SOCK=$RUN_DIR/agent.sock
SYS_CFG=/etc/upanel/upanel.json
AGENT_DB_DIR=/var/lib/upanel-agent
AGENT_DB=$AGENT_DB_DIR/agent.db
BACKUP_DIR=/var/lib/upanel-agent/fstab-backups
KEY_FILE=/etc/upanel/agent.key
WEB_DB_DIR=$WORK/webdata
WEB_DB=$WEB_DB_DIR/upanel.db
PORT=${PORT:-19474}
BASE="http://127.0.0.1:$PORT"
ORIGIN="$BASE"
WEB_USER=upanel-web
PASSWORD='vm-correct-horse-9'

SPARE_DISKS=${SPARE_DISKS:-"sdb sdc"}
STORE=/api/v1/storage
PREFLIGHT_PATH=/api/v1/operations/preflight
JOBS=/api/v1/jobs
AGENT_LOG=$WORK/logs/agent.log
WEB_LOG=$WORK/logs/web.log
AGENT_PID=""
WEB_PID=""

VOLUME_ROOT=/srv/upanel/volumes
FSTAB=/etc/fstab
GIB=1073741824
PART_MAIN=$((2 * GIB))
PART_ALT=$((GIB))
# 分区必须从第一块空闲区里开始：GPT 自己占掉盘头 1 MiB（外加备份表），
# 从 0 开始建分区会被判 REGION_NOT_FREE——那不是 bug，是产品在保护分区表。
PART_START=1048576
SLUG_MAIN=data
SLUG_ALT=logs
LABEL_MAIN=p07vol
LABEL_ALT=p07logs

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
resp_get() { jget "$WORK/resp.json" "$1"; }
pget() { jget "$WORK/preflight.json" "$1"; }
jobget() { jget "$WORK/job.json" "$1"; }
devget() { jget "$WORK/device.json" "$1"; }

cleanup() {
  kill_holder
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
  _code=$(curl -s -b "$JAR" -o "$WORK/resp.json" -w '%{http_code}' \
    -X POST "$BASE$PREFLIGHT_PATH" \
    -H 'Content-Type: application/json' -H "Origin: $ORIGIN" -H "X-CSRF-Token: $CSRF" \
    -d @"$WORK/pre.json")
  cp "$WORK/resp.json" "$WORK/preflight.json"
  printf '%s' "$_code"
}

# submit_job <preflight_id> <短语> <幂等键>：响应存为 $WORK/submit.json，回显状态码。
#
# 短语为空时**不发这个字段**：卷动作没有确认短语，发一个空串会让“没有短语”在
# 日志里看起来像“提交了一个空短语”。
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

# submit_and_wait <preflight_id> <短语> <幂等键>
# 回显任务终态；提交阶段就被拒时回显 "submit:<状态码>"。
submit_and_wait() {
  _code=$(submit_job "$1" "${2:-}" "$3")
  if [ "$_code" != "202" ]; then printf 'submit:%s' "$_code"; return 0; fi
  _id=$(jget "$WORK/submit.json" 'd["data"]["job_id"]')
  wait_job "$_id"
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
  JOB_CODE=$(submit_job "$PRE_ID" "$PRE_PHRASE" "$3")
  cp "$WORK/submit.json" "$PAYLOADS/submit-$3.json"
  if [ "$JOB_CODE" = "202" ]; then
    JOB_ID=$(jget "$WORK/submit.json" 'd["data"]["job_id"]')
    JOB_STATE=$(wait_job "$JOB_ID")
    cp "$WORK/job.json" "$PAYLOADS/job-$3.json"
  else
    JOB_ID=""
    JOB_STATE="submit:$JOB_CODE"
  fi
}

jstatus() { jobget 'd["data"]["status"]'; }
jstep() { jobget 'd["data"]["step"]'; }
joutcome() { jobget 'd["data"]["result"]["outcome"]'; }
jverified() { jobget 'bool(d["data"]["result"].get("verified_at"))'; }
jerr() { jobget 'd["data"]["error"]["code"]'; }
jerrmsg() { jobget 'd["data"]["error"]["message"]'; }
jobobserved() { jobget "d[\"data\"][\"result\"][\"observed_state\"][\"$1\"]"; }
jobdata() { jobget "d[\"data\"][\"result\"][\"data\"].get(\"$1\")"; }
jobobservedcsv() {
  jobget "','.join(d[\"data\"][\"result\"][\"observed_state\"].get(\"$1\") or [])"
}

# jobs_list <后缀>：任务列表存成 $WORK/jobs-<后缀>.json，回显 HTTP 状态码。
#
# 受理时被拒的提交**没有 job_id 返回给客户端**（响应是错误信封），所以这类用例只能按内容
# 到列表里找那条 failed 任务——Q159 的纪律：按内容找，不按位置。
jobs_list() {
  _code=$(api "$JAR" "$CSRF" GET "$JOBS?limit=50")
  cp "$WORK/resp.json" "$WORK/jobs-$1.json"
  printf '%s' "$_code"
}

pallowed() { pget 'd["data"]["allowed"]'; }
pall() { pget '",".join(b["code"] for b in d["data"]["blockers"])'; }
pwhy() { pget '"; ".join(b["message"] for b in d["data"]["blockers"])'; }
pimpact() { pget '",".join(i["kind"] for i in d["data"]["impacts"])'; }
plocks() { pget '",".join(d["data"]["locks"])'; }
pstate() { pget 'd["data"]["target"]["display_name"]'; }
preauth() { pget 'bool(d["data"].get("requires_reauth"))'; }

# 一个写动作是否“真的成了”：终态 succeeded、result=verified、带 verified_at。
expect_verified() {
  expect_eq "$1任务终态" "succeeded" "$JOB_STATE"
  expect_eq "$1结果是 verified" "verified" "$(joutcome)"
  expect_eq "$1带 verified_at" "True" "$(jverified)"
}

# ---------- 设备快照 ----------

# device <设备 id>：把设备详情存成 $WORK/device.json。
device() { api "$JAR" "$CSRF" GET "$STORE/devices/$1" >/dev/null; cp "$WORK/resp.json" "$WORK/device.json"; }

# 快照新鲜度：拓扑每 5 秒重扫，写完盘/挂完载立刻读设备详情会拿到操作之前的快照。
devcollected() {
  jget "$WORK/device.json" 'import datetime
s=str(d["data"]["collected_at"]).replace("Z","+00:00")
try:
    _=datetime.datetime.fromisoformat(s).timestamp()
except Exception:
    _=""'
}
later_than() {
  python3 -c 'import sys
try:
    sys.exit(0 if float(sys.argv[1]) > float(sys.argv[2]) else 1)
except Exception:
    sys.exit(1)' "$1" "$2" 2>/dev/null
}
wait_device() {
  _since=${3:-0}
  _i=0
  while [ "$_i" -lt 60 ]; do
    device "$1"
    if [ "$(devget "$2")" = "True" ] && later_than "$(devcollected)" "$_since"; then
      return 0
    fi
    _i=$((_i + 1))
    sleep 0.5
  done
  return 1
}
settle() {
  _t0=$(date +%s)
  if wait_device "$1" "$2" "$_t0"; then
    pass "快照反映了操作之后的状态（$1）"
  else
    fail "快照反映了操作之后的状态（$1）" "30 秒内没有出现操作之后且满足「$2」的快照"
  fi
}

# sync_topo <设备>：等采集器**重扫一次**再往下走。
#
# 预检绑定的是缓存快照（这是 Assess 的设计：预检必须绑定用户看到的那份拓扑），
# 而拓扑采集间隔是 5 秒。刚改过盘就立刻预检，很可能绑到改之前的快照；提交时第二道门
# 发现版本变了，会判 PREFLIGHT_STALE。那是**正确的拒绝**，但不是这些用例要验的东西。
sync_topo() { settle "$1" 'True'; }

# 挂载状态也必须由**采集器看到之后**才预检：卷动作的 StateVersion 里含现场观察状态，
# 快照还停在“没挂”的时候预检卸载，判出来的是 VOLUME_NOT_MOUNTED（假拒绝）。
#
# 参数是**分区** id（sdb1），不是整盘（sdb）：拓扑只给整盘与已挂载的分区填 filesystem，
# 整盘的 filesystem 恒为 null。第一版传整盘，于是“挂上了没有”这条永远等不到、
# “卸掉了没有”这条永远瞬间成立——一真一假地看起来都像通过（Q197）。
wait_mounted() {
  settle "$1" "any(m[\"path\"]==\"$2\" for m in ((d[\"data\"][\"device\"].get(\"filesystem\") or {}).get(\"mounts\") or []))"
}
wait_unmounted() {
  settle "$1" "not any(m[\"path\"]==\"$2\" for m in ((d[\"data\"][\"device\"].get(\"filesystem\") or {}).get(\"mounts\") or []))"
}

# ---------- 卷的相关接口 ----------

# volumes <后缀>：卷列表存成 $WORK/volumes-<后缀>.json，回显 HTTP 状态码。
volumes() {
  _code=$(api "$JAR" "$CSRF" GET "$STORE/volumes")
  cp "$WORK/resp.json" "$WORK/volumes-$1.json"
  printf '%s' "$_code"
}
volsget() { jget "$WORK/volumes-$1.json" "$2"; }

# volume <id> <后缀>：卷详情存成 $WORK/volume-<后缀>.json，回显状态码。
volume() {
  _code=$(api "$JAR" "$CSRF" GET "$STORE/volumes/$1")
  cp "$WORK/resp.json" "$WORK/volume-$2.json"
  printf '%s' "$_code"
}
volget() { jget "$WORK/volume-$1.json" "$2"; }

# blockers <id> <后缀>：占用信息存成 $WORK/blockers-<后缀>.json，回显状态码。
blockers() {
  _code=$(api "$JAR" "$CSRF" GET "$STORE/volumes/$1/blockers")
  cp "$WORK/resp.json" "$WORK/blockers-$2.json"
  printf '%s' "$_code"
}
blkget() { jget "$WORK/blockers-$1.json" "$2"; }

# current_vol_version <卷 id> <后缀>：读当前 version（乐观并发令牌）。
current_vol_version() {
  volume "$1" "$2" >/dev/null
  volget "$2" 'd["data"]["version"]'
}

# ---------- 独立证据（不经过产品的解析器） ----------

blkid_field() { blkid -p -o value -s "$2" "$1" 2>/dev/null | head -1; }
fs_type() { blkid_field "$1" TYPE; }
fs_uuid() { blkid_field "$1" UUID; }
fs_label() { blkid_field "$1" LABEL; }
kbytes() { lsblk -bno SIZE "$1" 2>/dev/null | tr -d ' '; }

# 挂载事实：findmnt 与 /proc/self/mountinfo（内核视图）。
is_mounted() {
  if [ -n "$(findmnt -rno TARGET --mountpoint "$1" 2>/dev/null)" ]; then echo True; else echo False; fi
}
mountinfo_line() { grep -F " $1 " /proc/self/mountinfo 2>/dev/null | head -1; }
findmnt_uuid() { findmnt -no UUID --mountpoint "$1" 2>/dev/null | head -1; }
findmnt_source() { findmnt -no SOURCE --mountpoint "$1" 2>/dev/null | head -1; }
findmnt_options() { findmnt -no OPTIONS --mountpoint "$1" 2>/dev/null | head -1; }
# 一个文件到底落在哪个挂载点上（stat 与 findmnt 两条独立路径）。
file_mount() { findmnt -rno TARGET --target "$1" 2>/dev/null | head -1; }
file_mount_stat() { stat -c '%m' "$1" 2>/dev/null; }

# fstab 事实：直接读字节。
fstab_sha() { sha256sum "$FSTAB" | cut -d' ' -f1; }
fstab_count() { grep -c . "$FSTAB" 2>/dev/null || true; }
fstab_mode() { stat -c '%a %U:%G' "$FSTAB" 2>/dev/null; }
fstab_entry() { grep -F "UUID=$1 " "$FSTAB" 2>/dev/null | head -1; }
fstab_entry_count() { grep -cF "UUID=$1 " "$FSTAB" 2>/dev/null || true; }
begin_markers() { grep -cx '# BEGIN upanel managed volumes' "$FSTAB" 2>/dev/null || true; }
end_markers() { grep -cx '# END upanel managed volumes' "$FSTAB" 2>/dev/null || true; }
# 托管块里的条目行（不含标记与说明行）。
block_lines() {
  awk '/^# BEGIN upanel managed volumes$/{f=1;next} /^# END upanel managed volumes$/{f=0} f && $0 !~ /^#/ && NF' "$FSTAB"
}
temp_leftovers() { ls /etc/fstab.upanel-new 2>/dev/null | wc -l | tr -d ' '; }
backup_count() { ls -1 "$BACKUP_DIR"/*.fstab 2>/dev/null | wc -l | tr -d ' '; }
# 备份目录里有没有一份内容与给定 sha256 相同的文件。
backup_has_content() {
  _want=$1
  for f in "$BACKUP_DIR"/*.fstab; do
    [ -f "$f" ] || continue
    [ "$(sha256sum "$f" | cut -d' ' -f1)" = "$_want" ] && return 0
  done
  return 1
}

# 占用者核对：产品说 pid=N 在用，脚本到 /proc/N/fd 里看那个 fd 是否真的指向这个卷。
pid_fds_into() {
  _pid=$1; _mp=$2; _hit=0
  for l in /proc/"$_pid"/fd/*; do
    case "$(readlink "$l" 2>/dev/null)" in
      "$_mp"/*) _hit=1 ;;
    esac
  done
  [ "$_hit" = "1" ] && echo True || echo False
}

HOLDER_PID=""
kill_holder() {
  if [ -n "${HOLDER_PID:-}" ] && kill -0 "$HOLDER_PID" 2>/dev/null; then
    kill -9 "$HOLDER_PID" 2>/dev/null
    wait "$HOLDER_PID" 2>/dev/null
  fi
  HOLDER_PID=""
  return 0
}

# hold_file <挂载点> <文件名>：在卷里开一个 fd 并一直握着（内核会因此拒绝 umount）。
hold_file() {
  sh -c "exec 3<>'$1/$2'; exec sleep 600" >/dev/null 2>&1 &
  HOLDER_PID=$!
  sleep 1
}

# ---------- 冻住产品自己 fork 的 umount ----------
#
# 卷动作有两道门：受理时的复核（`internal/agent/operations.go` 的 revalidate 会重算计划）
# 会把“预检之后变忙”的提交直接拒掉，所以执行端那段“umount 撞上 EBUSY → 把占用者列出来”
# 的代码，只有在**受理已经通过之后**才出现占用者时才跑得到。这个窗口只有几毫秒，
# 不冻就等于不验。冻住的瞬间正是要找的那一刻：复核已经过了、内核还没被调用。
#
# 为什么用 Python：umount 一个空闲卷只要几毫秒，dash 每轮循环至少 fork 一次，一轮几十毫秒，
# 整轮都能漏（P07.3 冻 mkfs 时踩过，见那轮的 Q185/Q188）。
#
# 三个坑照旧：① 子进程挂在**发起 exec 的那条线程**的 children 里，所以要把所有线程都读一遍；
# ② 不能用 `[ -s 文件 ]` 判空（procfs 的 st_size 恒为 0）；③ 不能用 `read x < /proc/...`
# （dash 里 read 是特殊内建，重定向失败会打死整个 shell）。
freeze_loop() {
  python3 - "$AGENT_PID" "$1" <<'PY'
import glob
import os
import signal
import sys
import time

agent, out = int(sys.argv[1]), sys.argv[2]
# 先把 pid 报给父 shell：冷启动几十毫秒，不握手就会在 import 期间错过窗口。
with open(out + ".watch", "w", encoding="ascii") as fh:
    fh.write(str(os.getpid()))
want = {"umount"}
traced = set()
deadline = time.time() + 120.0
while time.time() < deadline:
    kids = []
    for path in glob.glob("/proc/%d/task/*/children" % agent):
        try:
            with open(path, encoding="ascii") as fh:
                kids.extend(fh.read().split())
        except OSError:
            continue
    for kid in kids:
        try:
            with open("/proc/%s/comm" % kid, encoding="ascii") as fh:
                name = fh.read().strip()
        except OSError:
            continue
        if kid not in traced:
            traced.add(kid)
            try:
                with open(out + ".trace", "a", encoding="ascii") as fh:
                    fh.write("%s %s\n" % (kid, name))
            except OSError:
                pass
        # 名字不对就下一轮继续看这个 pid：fork 与 exec 之间 comm 还是父进程的名字。
        if name not in want:
            continue
        try:
            os.kill(int(kid), signal.SIGSTOP)
        except OSError:
            continue
        with open(out, "w", encoding="ascii") as fh:
            fh.write(kid)
        sys.exit(0)
    time.sleep(0.0005)
sys.exit(1)
PY
}
start_freeze() {
  rm -f "$1" "$1.watch" "$1.trace"
  freeze_loop "$1" >/dev/null 2>&1 &
  FREEZE_JOB=$!
  FREEZE_WATCH="$1.watch"
  _i=0
  while [ "$_i" -lt 300 ]; do
    [ -s "$1.watch" ] && return 0
    _i=$((_i + 1))
    sleep 0.01
  done
  return 1
}
# stop_freeze 必须按看门狗**自己报的 pid** 杀：`cmd &` 的 `$!` 只是外层子 shell，
# 杀掉它之后 python 会变孤儿继续轮询（P07.3 第 6 轮就这么把后面一轮的 umount 冻住了）。
stop_freeze() {
  if [ -n "${FREEZE_WATCH:-}" ] && [ -s "$FREEZE_WATCH" ]; then
    _wp=$(tr -d ' \n' <"$FREEZE_WATCH" 2>/dev/null)
    [ -n "$_wp" ] && kill -KILL "$_wp" 2>/dev/null
  fi
  if [ -n "${FREEZE_JOB:-}" ] && kill -0 "$FREEZE_JOB" 2>/dev/null; then
    kill -KILL "$FREEZE_JOB" 2>/dev/null
    wait "$FREEZE_JOB" 2>/dev/null
  fi
  FREEZE_JOB=""
  FREEZE_WATCH=""
  return 0
}
wait_frozen() {
  _i=0
  while [ "$_i" -lt 1200 ]; do
    if [ -s "$1" ]; then tr -d ' \n' <"$1"; return 0; fi
    _i=$((_i + 1))
    sleep 0.05
  done
  return 1
}
thaw() {
  if [ -n "${FROZEN_PID:-}" ] && kill -0 "$FROZEN_PID" 2>/dev/null; then
    kill -CONT "$FROZEN_PID" 2>/dev/null
  fi
  FROZEN_PID=""
  return 0
}
kill_frozen() {
  if [ -n "${FROZEN_PID:-}" ] && kill -0 "$FROZEN_PID" 2>/dev/null; then
    kill -CONT "$FROZEN_PID" 2>/dev/null
    kill -KILL "$FROZEN_PID" 2>/dev/null
  fi
  FROZEN_PID=""
  return 0
}

# =====================================================================
# 第二段：重启之后
# =====================================================================
if [ "$PHASE" = "post-boot" ]; then
  echo "== A. 重启后的现场（还没有启动任何 upanel 进程） =="
  if [ ! -f "$STATE" ]; then
    echo "找不到 $STATE：第二段必须在第一段之后、且用同一个 WORK 运行" >&2
    exit 2
  fi
  # shellcheck disable=SC1090
  . "$STATE"
  note "第一段记录的：卷 $VOL_ID，UUID $VOL_UUID，挂载点 $VOL_MP，另一个卷 $VOL_MP_B"

  if [ -S "$SOCK" ]; then
    fail "重启后没有遗留的代理 socket" "$SOCK 还在：上一段没有干净退出"
  else
    pass "重启后没有遗留的代理 socket（/run 已被清空）"
  fi
  UNIT="srv-upanel-volumes-$VOL_SLUG.mount"
  note "systemd 单元 $UNIT 的 ActiveEnterTimestamp：$(systemctl show -p ActiveEnterTimestamp --value "$UNIT" 2>/dev/null)"
  note "本次开机时间：$(uptime -s 2>/dev/null)"

  # 1. 它自己挂上了没有——不看面板，看内核。
  expect_eq "重启后卷挂在面板的挂载点上" "True" "$(is_mounted "$VOL_MP")"
  expect_eq "重启后挂载点的 UUID 与登记一致" "$VOL_UUID" "$(findmnt_uuid "$VOL_MP")"
  expect_contains "重启后挂载点在内核 mountinfo 里" "$VOL_MP" "$(mountinfo_line "$VOL_MP")"
  expect_eq "systemd 单元重启后是 active" "active" \
    "$(systemctl show -p ActiveState --value "$UNIT" 2>/dev/null)"
  TS=$(systemctl show -p ActiveEnterTimestamp --value "$UNIT" 2>/dev/null)
  if [ -n "$TS" ] && [ "$TS" != "n/a" ]; then
    pass "该单元有启动时间戳（是开机时挂的，不是后来手工挂的）"
  else
    fail "该单元有启动时间戳" "拿不到 ActiveEnterTimestamp"
  fi
  # 另一个卷导入时没开自动挂载：重启后**不该**挂上。这条把 noauto 钉在开机行为上。
  expect_eq "没开自动挂载的第二个卷重启后没有挂上" "False" "$(is_mounted "$VOL_MP_B")"

  # 2. 数据与配置都完好。
  if [ -f "$VOL_MP/persist.txt" ]; then
    expect_eq "重启后标记文件内容一致" "$MARK_SHA" "$(sha256sum "$VOL_MP/persist.txt" | cut -d' ' -f1)"
  else
    fail "重启后标记文件存在" "$VOL_MP/persist.txt 不见了"
  fi
  expect_eq "重启没有改动 /etc/fstab" "$FSTAB_SHA" "$(fstab_sha)"
  expect_eq "重启后这个 UUID 的条目仍然只有一条" "1" "$(fstab_entry_count "$VOL_UUID")"

  echo
  echo "== B. 启动代理与 Web，确认面板看到的是同一个事实 =="
  # /run 是 tmpfs，重启后是空的。安装形态下 /run/upanel 由 systemd 单元建立
  # （deploy/systemd/upanel-agent.service：RuntimeDirectory=upanel、
  # RuntimeDirectoryMode=0755），而这段验收直接跑二进制、没有单元文件，所以要
  # 自己扮演那个角色。代理在目录不在时拒绝启动是**对的**（Q39 的 verifySocketDir
  # 自检：目录必须 root 属主、组与其他位不可写），日志原文“应由安装脚本以 root
  # 创建”。第 1 轮就撞在这里——不是产品缺陷，是脚本少做了一步（Q202）。
  install -d -m 0755 "$RUN_DIR"
  # BIN_DIR 还要对 Web 账号可穿越、可执行。第 2 轮把二进制放在 /home/upanel 下，
  # 而 /home/upanel 是 0750，于是 `runuser -u upanel-web -- $WEB` 报 Permission denied，
  # 表现成“代理启动 PASS、Web 起不来”（Q203）。这条提前把它说清楚。
  if runuser -u "$WEB_USER" -- test -x "$WEB" 2>/dev/null; then
    pass "Web 账号能执行 BIN_DIR 里的二进制"
  else
    fail "Web 账号能执行 BIN_DIR 里的二进制" \
      "runuser -u $WEB_USER -- test -x $WEB 失败：目录或文件权限挡住了（BIN_DIR=$BIN_DIR）"
  fi
  if start_agent; then pass "代理启动"; else fail "代理启动" "$(tail -5 "$AGENT_LOG" 2>/dev/null)"; fi
  if start_web; then pass "Web 启动"; else fail "Web 启动" "$(tail -5 "$WEB_LOG" 2>/dev/null)"; fi
  JAR=$WORK/admin.jar
  expect_eq "管理员登录" "200" "$(login admin "$PASSWORD" "$JAR" login-admin)"
  CSRF=$(csrf_of "$WORK/login-admin.json")
  if [ -n "$CSRF" ]; then pass "拿到 CSRF 令牌"; else fail "拿到 CSRF 令牌" "登录响应里没有 csrf_token"; fi

  expect_eq "面板详情可读" "200" "$(volume "$VOL_ID" post)"
  expect_eq "面板详情：observed_state 是 mounted" "mounted" "$(volget post 'd["data"]["observed_state"]')"
  expect_eq "面板详情：desired_state 是 mounted" "mounted" "$(volget post 'd["data"]["desired_state"]')"
  expect_eq "面板详情：capacity 不是 null" "True" "$(volget post 'bool(d["data"].get("capacity"))')"
  expect_eq "占用信息可读" "200" "$(blockers "$VOL_ID" post)"
  expect_eq "占用接口：挂着但没人用" "False" "$(blkget post 'd["data"]["busy"]')"

  echo
  echo "== C. 收尾：卸载 + 退出管理（fstab 回到干净状态） =="
  VOL_VER=$(current_vol_version "$VOL_ID" post2)
  run_action storage.volume.unmount "{\"volume_id\":\"$VOL_ID\",\"version\":\"$VOL_VER\"}" post-unmount
  expect_eq "卸载预检允许" "True" "$PRE_ALLOWED"
  expect_eq "卸载要求再认证" "True" "$PRE_REAUTH"
  expect_verified "卸载"
  expect_eq "卸载：内核视图里没有挂载点了" "False" "$(is_mounted "$VOL_MP")"

  VOL_VER=$(current_vol_version "$VOL_ID" post3)
  FSTAB_BEFORE_FORGET=$(fstab_sha)
  run_action storage.volume.forget "{\"volume_id\":\"$VOL_ID\",\"version\":\"$VOL_VER\"}" post-forget
  expect_eq "退出管理预检允许" "True" "$PRE_ALLOWED"
  expect_eq "退出管理要求再认证" "True" "$PRE_REAUTH"
  expect_verified "退出管理"
  expect_eq "退出管理之后托管块里没有这条 UUID" "0" "$(fstab_entry_count "$VOL_UUID")"
  expect_eq "退出管理没有删掉托管块的标记" "1" "$(begin_markers)"
  expect_eq "退出管理留下了备份" "True" \
    "$(backup_has_content "$FSTAB_BEFORE_FORGET" && echo True || echo False)"
  expect_eq "卷详情现在是 404" "404" "$(volume "$VOL_ID" gone)"
  expect_eq "卷列表里没有它了" "False" \
    "$(volumes post4 >/dev/null; volsget post4 "any(v[\"slug\"]==\"$VOL_SLUG\" for v in d[\"data\"][\"volumes\"])")"
  expect_eq "另一个卷还在（退出管理只减一行）" "True" \
    "$(volsget post4 "any(v[\"slug\"]==\"$VOL_SLUG_B\" for v in d[\"data\"][\"volumes\"])")"

  echo
  echo "== 汇总（post-boot） =="
  printf 'PASS=%d FAIL=%d NOT RUN=%d\n' "$PASSED" "$FAILED" "$NOTRUN"
  [ "$FAILED" -eq 0 ] || printf 'FAILED:%s\n' "$FAILED_NAMES"
  [ "$FAILED" -eq 0 ] && exit 0
  exit 1
fi

# =====================================================================
# 第一段
# =====================================================================
echo "== 0. 环境与安全守卫 =="
for b in "$AGENT" "$WEB" "$CTL"; do
  if [ -x "$b" ]; then pass "二进制存在且可执行 $(basename "$b")"; else fail "二进制存在且可执行 $(basename "$b")" "$b"; fi
done
for t in python3 curl runuser blockdev sha256sum findmnt lsblk sgdisk blkid \
         wipefs udevadm dd ss stat systemctl tr awk diff; do
  if command -v "$t" >/dev/null 2>&1; then pass "依赖命令可用 $t"; else fail "依赖命令可用 $t" "缺少 $t"; fi
done
# 代理里写死的固定路径（storage.DefaultMountPath / UmountPath / FindmntPath /
# DefaultSystemctlPath）。断言的是**代理实际会 exec 的文件**。
for p in /usr/bin/mount /usr/bin/umount /usr/bin/findmnt /usr/bin/systemctl; do
  if [ -x "$p" ]; then pass "卷动作依赖存在且可执行 $p"; else fail "卷动作依赖存在且可执行 $p" "不存在或不可执行"; fi
done
note "findmnt: $(findmnt --version 2>&1 | head -1)"
note "整机 fstab（验收开始时）："
sed -n '1,20p' "$FSTAB" 2>/dev/null | while IFS= read -r l; do note "  $l"; done

if ! id -u "$WEB_USER" >/dev/null 2>&1; then
  note "创建系统账号 $WEB_USER"
  useradd --system --no-create-home --shell /usr/sbin/nologin "$WEB_USER" 2>/dev/null
fi
if id -u "$WEB_USER" >/dev/null 2>&1; then pass "Web 账号 $WEB_USER 存在"; else fail "Web 账号 $WEB_USER 存在" "无法创建"; fi
if ! getent group "$WEB_USER" >/dev/null 2>&1; then groupadd --system "$WEB_USER" 2>/dev/null; fi
if getent group "$WEB_USER" >/dev/null 2>&1; then pass "Web 组 $WEB_USER 存在"; else fail "Web 组 $WEB_USER 存在" "无法创建"; fi
if curl -fsS "$BASE/api/v1/health/live" >/dev/null 2>&1; then
  echo "$PORT 上已有服务在监听；先停掉遗留的 upanel-web 再跑验收" >&2
  exit 1
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
note "根设备链路：$ROOT_CHAIN"

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
  if swapon --show=NAME --noheadings 2>/dev/null | grep -qx "$(readlink -f "/dev/$d")"; then
    echo "/dev/$d 是活动交换区，拒绝使用" >&2
    exit 2
  fi
  _size=$(blockdev --getsize64 "/dev/$d" 2>/dev/null || echo 0)
  if [ "$_size" -lt $((PART_MAIN + 64 * 1048576)) ]; then
    echo "/dev/$d 容量 ${_size} 字节，放不下夹具分区" >&2
    exit 2
  fi
  note "/dev/$d：$_size 字节，无挂载、无 swap"
done
for d in $SPARE_DISKS; do
  wipefs -a "/dev/$d" >/dev/null 2>&1
  sgdisk --zap-all "/dev/$d" >/dev/null 2>&1
  dd if=/dev/zero of="/dev/$d" bs=1M count=4 conv=notrunc >/dev/null 2>&1
  blockdev --rereadpt "/dev/$d" >/dev/null 2>&1
  udevadm settle --timeout=5 >/dev/null 2>&1
done
pass "两块一次性盘清成空白"

rm -rf "$WORK"
mkdir -p "$WORK/logs" "$PAYLOADS" "$WEB_DB_DIR"
chmod 0755 "$WORK" "$PAYLOADS" "$WORK/logs"
chown -R "$WEB_USER" "$WEB_DB_DIR"
# 验收开始时的 fstab 原样留档：后面每一条“块外没被动过”的断言都拿它比。
cp "$FSTAB" "$WORK/logs/fstab-initial.txt"
chmod 0644 "$WORK/logs/fstab-initial.txt"
uname -r >"$WORK/logs/kernel.txt" 2>/dev/null || true
lsblk -o NAME,TYPE,SIZE,FSTYPE,MOUNTPOINTS >"$WORK/logs/lsblk-initial.txt" 2>&1
if [ -d "$BACKUP_DIR" ]; then
  note "清掉验收之前遗留的 fstab 备份（$(backup_count) 份）"
  rm -f "$BACKUP_DIR"/*.fstab
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
grep -q "受管理卷已接线" "$AGENT_LOG" && pass "代理日志声明受管理卷已接线" \
  || fail "代理日志声明受管理卷已接线" "日志里没有这一行（依赖不全？）"

runuser -u "$WEB_USER" -- curl -s --unix-socket "$SOCK" \
  http://agent/internal/v1/capabilities >"$PAYLOADS/agent-capabilities.json" 2>/dev/null

printf '%s\n%s\n' "$PASSWORD" "$PASSWORD" | "$CTL" init-admin --config "$SYS_CFG" --db "$WEB_DB" \
  --username admin --display-name "管理员" --json >"$WORK/init.out" 2>"$WORK/init.err"
INIT_RC=$?
expect_eq "init-admin 退出码" "0" "$INIT_RC"
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

echo
echo "== 2. 能力与只读接口：五个卷动作可执行，卷视图可用 =="
for a in storage.volume.import storage.volume.mount storage.volume.unmount \
         storage.volume.configure storage.volume.forget; do
  expect_eq "capabilities 里有 $a" "True" \
    "$(jget "$PAYLOADS/agent-capabilities.json" "\"$a\" in d[\"data\"][\"supported_actions\"]")"
done
note "代理登记的动作数：$(jget "$PAYLOADS/agent-capabilities.json" 'len(d["data"]["supported_actions"])')"

expect_eq "卷列表可读" "200" "$(volumes empty)"
expect_eq "空列表序列化成 []（不是 null）" "True" "$(volsget empty 'd["data"]["volumes"] == []')"
expect_eq "不存在的卷详情是 404" "404" "$(volume vol_00000000000000000000000000000000 missing)"
expect_eq "不存在的卷详情错误码" "NOT_FOUND" "$(resp_get 'd["error"]["code"]')"
expect_eq "形状不对的卷标识是 400" "400" "$(volume nonsense bad-shape)"
expect_eq "形状不对的错误码" "VALIDATION_ERROR" "$(resp_get 'd["error"]["code"]')"
expect_eq "不存在的卷的占用信息是 404" "404" "$(blockers vol_00000000000000000000000000000000 missing)"
expect_eq "占用信息错误码" "NOT_FOUND" "$(resp_get 'd["error"]["code"]')"

echo
echo "== 3. 夹具：两块盘走产品的 GPT → 分区 → ext4 =="
# 用产品动作造夹具而不是 sgdisk/mkfs：STAGES §9 第 1 条要求的就是这条链
# “新虚拟盘 → GPT → 分区 → ext4 → 导入 → 挂载 → 写文件 → 卸载 → 重挂载”。
run_action storage.partition_table.initialize "{\"device_id\":\"$BLANK_A\",\"table_type\":\"gpt\"}" init-a
expect_verified "初始化 GPT（$BLANK_A）"
sgdisk -p "/dev/$BLANK_A" >"$WORK/logs/sgdisk-$BLANK_A.txt" 2>&1
# sgdisk -p 不会为了“这是 GPT”专门印一行，所以另找一个证人：blkid 直接从盘上读分区表类型。
expect_eq "盘上确实有 GPT（blkid 独立读取）" "gpt" "$(blkid -p -o value -s PTTYPE "/dev/$BLANK_A" 2>/dev/null | head -1)"

run_action storage.partition_table.initialize "{\"device_id\":\"$BLANK_B\",\"table_type\":\"gpt\"}" init-b
expect_verified "初始化 GPT（$BLANK_B）"
expect_eq "盘上确实有 GPT（blkid 独立读取，$BLANK_B）" "gpt" \
  "$(blkid -p -o value -s PTTYPE "/dev/$BLANK_B" 2>/dev/null | head -1)"
sync_topo "$BLANK_A"
sync_topo "$BLANK_B"

run_action storage.partition.create \
  "{\"device_id\":\"$BLANK_A\",\"start_bytes\":\"$PART_START\",\"size_bytes\":\"$PART_MAIN\"}" part-a
expect_verified "建分区（$BLANK_A）"
run_action storage.partition.create \
  "{\"device_id\":\"$BLANK_B\",\"start_bytes\":\"$PART_START\",\"size_bytes\":\"$PART_ALT\"}" part-b
expect_verified "建分区（$BLANK_B）"
for p in "${BLANK_A}1" "${BLANK_B}1"; do
  if [ -b "/dev/$p" ]; then pass "夹具分区 /dev/$p 存在"; else fail "夹具分区 /dev/$p 存在" "不存在"; fi
done
expect_eq "夹具：${BLANK_A}1 的内核大小" "$PART_MAIN" "$(kbytes "/dev/${BLANK_A}1")"
expect_eq "夹具：${BLANK_B}1 的内核大小" "$PART_ALT" "$(kbytes "/dev/${BLANK_B}1")"
sync_topo "$BLANK_A"
sync_topo "$BLANK_B"

run_action storage.filesystem.format \
  "{\"device_id\":\"${BLANK_A}1\",\"filesystem\":\"ext4\",\"label\":\"$LABEL_MAIN\"}" fmt-a
expect_verified "格式化（${BLANK_A}1）"
run_action storage.filesystem.format \
  "{\"device_id\":\"${BLANK_B}1\",\"filesystem\":\"ext4\",\"label\":\"$LABEL_ALT\"}" fmt-b
expect_verified "格式化（${BLANK_B}1）"
UUID_A=$(fs_uuid "/dev/${BLANK_A}1")
UUID_B=$(fs_uuid "/dev/${BLANK_B}1")
expect_eq "独立工具读到 ext4（${BLANK_A}1）" "ext4" "$(fs_type "/dev/${BLANK_A}1")"
expect_eq "独立工具读到标签（${BLANK_A}1）" "$LABEL_MAIN" "$(fs_label "/dev/${BLANK_A}1")"
if [ -n "$UUID_A" ] && [ -n "$UUID_B" ] && [ "$UUID_A" != "$UUID_B" ]; then
  pass "两个文件系统的 UUID 不同（$UUID_A / $UUID_B）"
else
  fail "两个文件系统的 UUID 不同" "读到 '$UUID_A' 与 '$UUID_B'"
fi
sync_topo "$BLANK_A"
sync_topo "$BLANK_B"

echo
echo "== 4. 导入卷：fstab 最小修改、备份、原子替换 =="
MP_A=$VOLUME_ROOT/$SLUG_MAIN
MP_B=$VOLUME_ROOT/$SLUG_ALT
UNIT_A="srv-upanel-volumes-$SLUG_MAIN.mount"
EXPECT_LINE="UUID=$UUID_A $MP_A ext4 rw,nodev,nosuid,nofail,x-systemd.device-timeout=30,noauto 0 2"
FSTAB_BEFORE=$(fstab_sha)
FSTAB_MODE_BEFORE=$(fstab_mode)
FSTAB_COUNT_BEFORE=$(fstab_count)
BACKUPS_BEFORE=$(backup_count)

run_action storage.volume.import \
  "{\"device_id\":\"${BLANK_A}1\",\"slug\":\"$SLUG_MAIN\",\"display_name\":\"数据卷\"}" import-a
expect_eq "导入预检状态码" "200" "$PRE_CODE"
expect_eq "导入预检允许" "True" "$PRE_ALLOWED"
expect_contains "导入的锁有整盘" "disk:$BLANK_A" "$(plocks)"
expect_contains "导入的锁有 fstab" "fstab" "$(plocks)"
expect_eq "导入不需要再认证（只登记，不改开机行为）" "False" "$PRE_REAUTH"
expect_eq "导入没有确认短语" "" "$PRE_PHRASE"
expect_contains "导入的影响里有 config_change" "config_change" "$(pimpact)"
note "目标：$(pstate)"
expect_verified "导入"

# —— 登记的记录 ——
expect_eq "核验来源：fstab 读回 + systemd 单元" "fstab_reread,systemctl_show" "$(jobobservedcsv checked_via)"
expect_eq "写进 fstab 的 UUID 与盘上一致（独立 blkid）" "$UUID_A" "$(jobobserved uuid)"
expect_eq "结果里的条目行与 fstab 实际内容一致" "$EXPECT_LINE" "$(jobobserved fstab_entry)"
expect_eq "systemd 里的挂载单元名" "$UNIT_A" "$(jobobserved systemd_unit)"
VOL_ID_A=$(jobdata volume_id)
VOL_SLUG_A=$SLUG_MAIN
expect_contains "卷标识是 vol_ 形状" "vol_" "$VOL_ID_A"

# —— fstab 的真实字节 ——
expect_eq "fstab 里这个 UUID 只有一条条目" "1" "$(fstab_entry_count "$UUID_A")"
expect_eq "fstab 条目与期望逐字一致" "$EXPECT_LINE" "$(fstab_entry "$UUID_A")"
expect_eq "托管块标记 BEGIN 恰好一个" "1" "$(begin_markers)"
expect_eq "托管块标记 END 恰好一个" "1" "$(end_markers)"
expect_eq "托管块里只有这一条条目" "1" "$(block_lines | wc -l | tr -d ' ')"
expect_eq "没有遗留候选临时文件" "0" "$(temp_leftovers)"
expect_eq "fstab 权限与属主没被顺手改掉" "$FSTAB_MODE_BEFORE" "$(fstab_mode)"
# 块外内容必须逐字不动：把托管块剔掉，与原始文件比对。
awk '/^# BEGIN upanel managed volumes$/{f=1;next} /^# END upanel managed volumes$/{f=0;next} !f' \
  "$FSTAB" >"$WORK/logs/fstab-prefix-now.txt"
if diff -q "$WORK/logs/fstab-initial.txt" "$WORK/logs/fstab-prefix-now.txt" >/dev/null 2>&1; then
  pass "托管块之外的内容逐字未变"
else
  fail "托管块之外的内容逐字未变" \
    "$(diff "$WORK/logs/fstab-initial.txt" "$WORK/logs/fstab-prefix-now.txt" 2>&1 | head -5 | tr '\n' ' ')"
fi
expect_eq "fstab 非空行只多出托管块的四行" "$((FSTAB_COUNT_BEFORE + 4))" "$(fstab_count)"
expect_eq "备份目录里多了一份" "$((BACKUPS_BEFORE + 1))" "$(backup_count)"
expect_eq "备份的内容就是写之前的 fstab" "True" \
  "$(backup_has_content "$FSTAB_BEFORE" && echo True || echo False)"
expect_eq "备份路径出现在结果里" "True" \
  "$([ -f "$(jobdata fstab_backup)" ] && echo True || echo False)"
expect_eq "systemd 认得这个挂载单元" "loaded" \
  "$(systemctl show -p LoadState --value "$UNIT_A" 2>/dev/null)"
expect_eq "挂载点目录已创建" "True" "$([ -d "$MP_A" ] && echo True || echo False)"
expect_eq "导入没把卷挂上（不改运行状态）" "False" "$(is_mounted "$MP_A")"

# —— 只读视图 ——
expect_eq "卷列表：一个卷" "1" "$(volumes a >/dev/null; volsget a 'len(d["data"]["volumes"])')"
expect_eq "卷列表：slug" "$SLUG_MAIN" "$(volsget a 'd["data"]["volumes"][0]["slug"]')"
expect_eq "卷列表：UUID 与 blkid 一致" "$UUID_A" "$(volsget a 'd["data"]["volumes"][0]["filesystem_uuid"]')"
expect_eq "卷列表：observed_state 是 unmounted" "unmounted" \
  "$(volsget a 'd["data"]["volumes"][0]["observed_state"]')"
expect_eq "卷列表：desired_state 是 unmounted" "unmounted" \
  "$(volsget a 'd["data"]["volumes"][0]["desired_state"]')"
expect_eq "卷列表：capacity 是 null（没挂载）" "False" \
  "$(volsget a 'bool(d["data"]["volumes"][0].get("capacity"))')"
expect_eq "卷详情可读" "200" "$(volume "$VOL_ID_A" a)"
expect_eq "卷详情：mount_path" "$MP_A" "$(volget a 'd["data"]["mount_path"]')"
expect_eq "卷详情：automount（导入默认关）" "False" "$(volget a 'd["data"]["automount"]')"
expect_eq "卷详情：nodev/nosuid 默认开" "True True" \
  "$(volget a 'str(d["data"]["nodev"])+" "+str(d["data"]["nosuid"])')"
expect_eq "卷详情：没有 observed_error" "" "$(volget a 'd["data"].get("observed_error") or ""')"
expect_eq "占用信息可读" "200" "$(blockers "$VOL_ID_A" a)"
expect_eq "占用信息：没挂载" "False" "$(blkget a 'd["data"]["mounted"]')"
expect_eq "占用信息：不忙" "False" "$(blkget a 'd["data"]["busy"]')"
expect_eq "占用信息：空列表是 []" "True" \
  "$(blkget a 'd["data"]["processes"] == [] and d["data"]["mounts"] == []')"
expect_eq "占用信息：带检查时间" "True" "$(blkget a 'bool(d["data"]["checked_at"])')"

# 重复导入同一个文件系统必须被拒（两个卷指向同一挂载点会让 fstab 出现两行）。
FSTAB_BEFORE_DUP=$(fstab_sha)
run_action storage.volume.import "{\"device_id\":\"${BLANK_A}1\",\"slug\":\"data2\"}" import-dup
expect_eq "重复导入同一 UUID 的预检不允许" "False" "$PRE_ALLOWED"
expect_contains "重复导入的阻塞码" "VOLUME_UUID_TAKEN" "$(pall)"
expect_eq "重复导入的提交被拒" "submit:409" "$JOB_STATE"
expect_eq "重复导入没有改动 fstab" "$FSTAB_BEFORE_DUP" "$(fstab_sha)"

# 第二个卷：证明两个卷共存、互不干扰、条目按挂载点排序。
run_action storage.volume.import \
  "{\"device_id\":\"${BLANK_B}1\",\"slug\":\"$SLUG_ALT\",\"display_name\":\"日志卷\"}" import-b
expect_verified "第二个卷导入"
VOL_ID_B=$(jobdata volume_id)
expect_eq "第二个卷的 fstab 条目" \
  "UUID=$UUID_B $MP_B ext4 rw,nodev,nosuid,nofail,x-systemd.device-timeout=30,noauto 0 2" \
  "$(fstab_entry "$UUID_B")"
expect_eq "托管块里现在两条条目" "2" "$(block_lines | wc -l | tr -d ' ')"
expect_contains "条目按挂载点排序（data 在 logs 之前）" "$MP_A" "$(block_lines | head -1)"
expect_eq "第一个卷的条目没有被第二个卷改动" "$EXPECT_LINE" "$(fstab_entry "$UUID_A")"

echo
echo "== 5. 挂载：UUID 两源核验 + 真的能写文件 =="
VOL_VER=$(current_vol_version "$VOL_ID_A" b)
run_action storage.volume.mount "{\"volume_id\":\"$VOL_ID_A\",\"version\":\"$VOL_VER\"}" mount-a
expect_eq "挂载预检允许" "True" "$PRE_ALLOWED"
expect_eq "挂载不需要再认证" "False" "$PRE_REAUTH"
expect_eq "挂载没有确认短语" "" "$PRE_PHRASE"
expect_contains "挂载的锁有 volume" "volume:$VOL_ID_A" "$(plocks)"
expect_contains "挂载的锁有整盘" "disk:$BLANK_A" "$(plocks)"
expect_verified "挂载"
expect_eq "核验来源：findmnt + 拓扑重扫" "findmnt,topology_scan" "$(jobobservedcsv checked_via)"
expect_eq "结果里的 UUID 与盘上一致" "$UUID_A" "$(jobobserved uuid)"
expect_eq "结果里的挂载点" "$MP_A" "$(jobobserved mount_path)"
# —— 独立证据：内核视图 ——
expect_eq "内核视图：挂载点上就是这个 UUID" "$UUID_A" "$(findmnt_uuid "$MP_A")"
expect_contains "内核视图：源是这块分区" "${BLANK_A}1" "$(findmnt_source "$MP_A")"
expect_contains "内核视图：nosuid 生效" "nosuid" "$(findmnt_options "$MP_A")"
expect_contains "内核视图：nodev 生效" "nodev" "$(findmnt_options "$MP_A")"
expect_contains "内核视图：可读写（rw）" "rw" "$(findmnt_options "$MP_A")"
expect_eq "内核 mountinfo 里有这个挂载点" "True" \
  "$([ -n "$(mountinfo_line "$MP_A")" ] && echo True || echo False)"
# 真的写一笔：只有真正挂上了，这个文件才会落在这块一次性的虚拟盘上。
printf 'p07-4 验收：%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" >"$MP_A/hello.txt"
HELLO_SHA=$(sha256sum "$MP_A/hello.txt" | cut -d' ' -f1)
expect_eq "写进去的文件属于这个挂载点（findmnt -T）" "$MP_A" "$(file_mount "$MP_A/hello.txt")"
expect_eq "写进去的文件属于这个挂载点（stat %m）" "$MP_A" "$(file_mount_stat "$MP_A/hello.txt")"
wait_mounted "${BLANK_A}1" "$MP_A"
expect_eq "面板详情：observed_state 是 mounted" "mounted" \
  "$(volume "$VOL_ID_A" a >/dev/null; volget a 'd["data"]["observed_state"]')"
expect_eq "面板详情：capacity 不是 null" "True" "$(volget a 'bool(d["data"].get("capacity"))')"
expect_eq "面板详情：容量总数 > 0" "True" "$(volget a 'int(d["data"]["capacity"]["total_bytes"]) > 0')"
expect_eq "面板详情：desired_state 变成 mounted" "mounted" "$(volget a 'd["data"]["desired_state"]')"
expect_eq "占用信息：挂着但没人用" "False" "$(blockers "$VOL_ID_A" a >/dev/null; blkget a 'd["data"]["busy"]')"
expect_eq "占用信息：挂载点列表里有它" "True" \
  "$(blkget a "d[\"data\"][\"mounted\"] and \"$MP_A\" in d[\"data\"][\"mounts\"]")"

echo
echo "== 6. 卸载 → 重挂载：文件一致 =="
VOL_VER=$(current_vol_version "$VOL_ID_A" b)
run_action storage.volume.unmount "{\"volume_id\":\"$VOL_ID_A\",\"version\":\"$VOL_VER\"}" unmount-a
expect_eq "卸载预检允许" "True" "$PRE_ALLOWED"
expect_eq "卸载要求再认证" "True" "$PRE_REAUTH"
expect_verified "卸载"
expect_eq "内核视图：挂载点没了" "False" "$(is_mounted "$MP_A")"
expect_eq "卸载之后 fstab 条目还在（卸载不改开机配置）" "1" "$(fstab_entry_count "$UUID_A")"
wait_unmounted "${BLANK_A}1" "$MP_A"
expect_eq "面板详情：observed_state 回到 unmounted" "unmounted" \
  "$(volume "$VOL_ID_A" a >/dev/null; volget a 'd["data"]["observed_state"]')"
expect_eq "面板详情：capacity 回到 null" "False" "$(volget a 'bool(d["data"].get("capacity"))')"

# 没挂载的卷再卸载一次：预检必须拦住（不是“成功卸载了一个没挂的东西”）。
VOL_VER=$(current_vol_version "$VOL_ID_A" b)
run_action storage.volume.unmount "{\"volume_id\":\"$VOL_ID_A\",\"version\":\"$VOL_VER\"}" unmount-again
expect_eq "重复卸载的预检不允许" "False" "$PRE_ALLOWED"
expect_contains "重复卸载的阻塞码" "VOLUME_NOT_MOUNTED" "$(pall)"

run_action storage.volume.mount "{\"volume_id\":\"$VOL_ID_A\",\"version\":\"$VOL_VER\"}" remount-a
expect_verified "重挂载"
expect_eq "重挂载之后文件内容一致" "$HELLO_SHA" "$(sha256sum "$MP_A/hello.txt" | cut -d' ' -f1)"

echo
echo "== 7. 繁忙占用列表：说得出是谁占着 =="
hold_file "$MP_A" hold.txt
expect_eq "占用信息可读" "200" "$(blockers "$VOL_ID_A" busy)"
expect_eq "占用信息：报告忙" "True" "$(blkget busy 'd["data"]["busy"]')"
expect_eq "占用信息：挂了" "True" "$(blkget busy 'd["data"]["mounted"]')"
BUSY_PIDS=$(blkget busy '" ".join(p["pid"] for p in d["data"]["processes"])')
BUSY_COUNT=$(blkget busy 'len(d["data"]["processes"])')
if [ -n "$BUSY_PIDS" ]; then
  pass "占用信息列出了进程（pid=$BUSY_PIDS，共 $BUSY_COUNT 个）"
else
  fail "占用信息列出了进程" "列表是空的：面板看不见占用者，用户只能看到一句“设备忙”"
fi
HOLDER_REPORTED=$(printf '%s\n' $BUSY_PIDS | head -1)
expect_eq "面板点名的 pid 真的握着这个卷（到 /proc/pid/fd 独立核对）" "True" \
  "$(pid_fds_into "$HOLDER_REPORTED" "$MP_A")"
note "占用者的进程名：$(blkget busy '" ".join(p["comm"] for p in d["data"]["processes"])')"

# 第一层：预检就该拦住——不能把“注定失败”的卸载放行。
VOL_VER=$(current_vol_version "$VOL_ID_A" b)
run_action storage.volume.unmount "{\"volume_id\":\"$VOL_ID_A\",\"version\":\"$VOL_VER\"}" unmount-busy-pre
expect_eq "有进程占用时卸载预检不允许" "False" "$PRE_ALLOWED"
expect_contains "占用阻塞码" "VOLUME_BUSY" "$(pall)"
expect_contains "阻塞消息里点名了占用的 pid" "$HOLDER_REPORTED" "$(pwhy)"
expect_eq "有占用时提交被拒" "submit:409" "$JOB_STATE"
expect_eq "预检拒绝之后卷还挂着（内核视图）" "True" "$(is_mounted "$MP_A")"

# 占用者退出之后，占用信息必须回到“不忙”。这一步既是断言，也是第二层用例的前提：
# 竞态要验的是“预检那一刻还没人用、之后才出现”，所以必须先真的安静下来（Q198）。
kill_holder
sleep 0.5
expect_eq "占用者退出后占用信息回到不忙" "False" \
  "$(blockers "$VOL_ID_A" after >/dev/null; blkget after 'd["data"]["busy"]')"

# 第二层之一：预检放行之后才出现占用者 → **受理时**就被拒。
#
# 这一格不是原计划里的分支。原计划以为会一路走到执行端，让 umount 自己撞 EBUSY；
# 真机上代理在**受理**操作时会重算一遍计划（`internal/agent/operations.go` revalidate），
# 一变忙就回 409 VALIDATION_ERROR，Web 按错误码表返回 400，任务留一条 failed/rejected（Q200）。
# 这是好事：注定失败的操作连 operation 都不会建。断言要按真实行为写，不能按愿望写。
VOL_VER=$(current_vol_version "$VOL_ID_A" b)
PRE_CODE=$(preflight storage.volume.unmount "{\"volume_id\":\"$VOL_ID_A\",\"version\":\"$VOL_VER\"}")
cp "$WORK/preflight.json" "$PAYLOADS/preflight-unmount-race.json"
expect_eq "（竞态用例的准备）预检放行" "True" "$(pallowed)"
RACE_PRE_ID=$(pget 'd["data"]["preflight_id"]')
reauth >/dev/null
hold_file "$MP_A" hold2.txt
RACE_HOLDER=$HOLDER_PID
ACCEPT_CODE=$(submit_job "$RACE_PRE_ID" "" "unmount-race-accept")
cp "$WORK/submit.json" "$PAYLOADS/submit-unmount-race-accept.json"
expect_eq "受理时发现变忙：提交被拒" "400" "$ACCEPT_CODE"
expect_eq "拒绝码是 VALIDATION_ERROR" "VALIDATION_ERROR" \
  "$(jget "$WORK/submit.json" 'd["error"]["code"]')"
expect_contains "拒绝原因说到当前状态不允许（带阻塞码）" "VOLUME_BUSY" \
  "$(jget "$WORK/submit.json" 'd["error"]["message"]')"
expect_eq "受理被拒之后卷还挂着（内核视图）" "True" "$(is_mounted "$MP_A")"
expect_eq "受理被拒之后 desired_state 仍是 mounted" "mounted" \
  "$(volume "$VOL_ID_A" a >/dev/null; volget a 'd["data"]["desired_state"]')"
# Q159 的纪律：受理被拒也要留一条任务，且**按内容找**（同一秒内的顺序由 id 决定）。
expect_eq "被受理拒绝的那次留下 failed/rejected 的任务" "failed|rejected|VALIDATION_ERROR" \
  "$(jobs_list race-accept >/dev/null; jget "$WORK/jobs-race-accept.json" '
j=[x for x in d["data"]["jobs"] if x["action"]=="storage.volume.unmount" and x.get("step")=="rejected"]
x=j[0] if j else {}
_="%s|%s|%s" % (x.get("status"), x.get("step"), (x.get("error") or {}).get("code"))')"

# 第二层之二：受理通过**之后**才出现占用者 → 执行端的 umount 撞上 EBUSY，
# 由执行器自己把占用者再列一遍（这是 CONTRACTS §6.5 那一段的直接证据）。
#
# 制造这个窗口只能冻住产品自己 fork 的 umount：冻住的那一刻，受理复核已经过了、
# 内核还没被调用，正好是“执行时”那一刻（Q201）。最多试 3 次，没冻住就把卷挂回去重来。
kill_holder
sleep 0.5
RACE_EBUSY_OK=0
_attempt=0
while [ "$_attempt" -lt 3 ]; do
  _attempt=$((_attempt + 1))
  VOL_VER=$(current_vol_version "$VOL_ID_A" b)
  PRE_CODE=$(preflight storage.volume.unmount "{\"volume_id\":\"$VOL_ID_A\",\"version\":\"$VOL_VER\"}")
  cp "$WORK/preflight.json" "$PAYLOADS/preflight-unmount-ebusy.json"
  if [ "$(pallowed)" != "True" ]; then
    note "第 $_attempt 次：预检没放行（$(pwhy)）"
    continue
  fi
  EBUSY_PRE_ID=$(pget 'd["data"]["preflight_id"]')
  reauth >/dev/null
  if ! start_freeze "$WORK/frozen-umount.pid"; then
    fail "看门狗就绪" "第 $_attempt 次：Python 没在 3 秒内报就绪（见 frozen-umount.pid.watch）"
    break
  fi
  ECODE=$(submit_job "$EBUSY_PRE_ID" "" "unmount-ebusy")
  if [ "$ECODE" != "202" ]; then
    stop_freeze
    note "第 $_attempt 次：提交没被受理（submit:$ECODE）"
    continue
  fi
  EBUSY_JOB=$(jget "$WORK/submit.json" 'd["data"]["job_id"]')
  FROZEN_PID=$(wait_frozen "$WORK/frozen-umount.pid")
  stop_freeze
  if [ -z "$FROZEN_PID" ]; then
    note "第 $_attempt 次没冻住 umount（诊断：$(tr '\n' ';' <"$WORK/frozen-umount.pid.trace" 2>/dev/null)）"
    _st=$(wait_job "$EBUSY_JOB")
    note "第 $_attempt 次任务结果：$_st"
    kill_holder
    if ! is_mounted "$MP_A"; then
      VOL_VER=$(current_vol_version "$VOL_ID_A" b)
      run_action storage.volume.mount "{\"volume_id\":\"$VOL_ID_A\",\"version\":\"$VOL_VER\"}" mount-after-miss
      wait_mounted "${BLANK_A}1" "$MP_A"
    fi
    continue
  fi
  RACE_EBUSY_OK=1
  break
done

if [ "$RACE_EBUSY_OK" = "1" ]; then
  note "冻住了产品的 umount（pid=$FROZEN_PID，comm=$(cat /proc/"$FROZEN_PID"/comm 2>/dev/null)）"
  hold_file "$MP_A" hold3.txt
  EBUSY_HOLDER=$HOLDER_PID
  kill -CONT "$FROZEN_PID" 2>/dev/null
  FROZEN_PID=""
  EBUSY_STATE=$(wait_job "$EBUSY_JOB")
  cp "$WORK/job.json" "$PAYLOADS/job-unmount-ebusy.json"
  expect_eq "执行时撞上占用 → 任务失败（不是虚假成功）" "failed" "$EBUSY_STATE"
  expect_eq "失败的错误码" "COMMAND_FAILED" "$(jerr)"
  BUSY_MSG=$(jerrmsg)
  expect_contains "失败原因里带上了 umount 的原文" "target is busy" "$BUSY_MSG"
  expect_contains "失败原因里点名了占用的 pid" "$EBUSY_HOLDER" "$BUSY_MSG"
  expect_contains "失败原因里还有一句可读的占用说明" "当前占用" "$BUSY_MSG"
  note "失败消息：$BUSY_MSG"
  expect_eq "失败之后卷还挂着（内核视图）" "True" "$(is_mounted "$MP_A")"
  expect_eq "失败之后 desired_state 仍是 mounted" "mounted" \
    "$(volume "$VOL_ID_A" a >/dev/null; volget a 'd["data"]["desired_state"]')"
  kill_holder
  sleep 0.5
  expect_eq "执行端占用者也退出后占用信息回到不忙" "False" \
    "$(blockers "$VOL_ID_A" after2 >/dev/null; blkget after2 'd["data"]["busy"]')"
else
  kill_frozen
  kill_holder
  notrun "执行时撞上占用 → umount 撞 EBUSY" \
    "三次都没能在 umount 的运行窗口内冻住它；这是测试的竞态，不当作功能缺失，诊断见 frozen-umount.pid.trace"
fi
VOL_VER=$(current_vol_version "$VOL_ID_A" b)
run_action storage.volume.unmount "{\"volume_id\":\"$VOL_ID_A\",\"version\":\"$VOL_VER\"}" unmount-free
expect_verified "占用者退出后卸载"
expect_eq "卸载之后内核视图里没了" "False" "$(is_mounted "$MP_A")"
wait_unmounted "${BLANK_A}1" "$MP_A"

echo
echo "== 8. 开机自动挂载：fstab 里那一行的选项 =="
FSTAB_BEFORE_CONF=$(fstab_sha)
VOL_VER=$(current_vol_version "$VOL_ID_A" b)
run_action storage.volume.configure \
  "{\"volume_id\":\"$VOL_ID_A\",\"version\":\"$VOL_VER\",\"automount\":true}" configure-auto
expect_eq "改配置预检允许" "True" "$PRE_ALLOWED"
expect_eq "改配置要求再认证" "True" "$PRE_REAUTH"
expect_contains "改配置的锁有 fstab" "fstab" "$(plocks)"
expect_verified "改配置"
AUTO_LINE=$(fstab_entry "$UUID_A")
expect_eq "fstab 条目去掉了 noauto" \
  "UUID=$UUID_A $MP_A ext4 rw,nodev,nosuid,nofail,x-systemd.device-timeout=30 0 2" "$AUTO_LINE"
expect_not_contains "条目里没有 noauto" "noauto" "$AUTO_LINE"
expect_contains "条目里保留 nofail（盘不在也不该卡住开机）" "nofail" "$AUTO_LINE"
expect_contains "条目里有固定设备等待时间" "x-systemd.device-timeout=30" "$AUTO_LINE"
expect_eq "改配置留的备份就是改之前那份 fstab" "True" \
  "$(backup_has_content "$FSTAB_BEFORE_CONF" && echo True || echo False)"
expect_eq "托管块里仍然只有两条条目" "2" "$(block_lines | wc -l | tr -d ' ')"
# findmnt 校验现行 fstab：这是“这台机器下次能不能起来”的独立判断。
if findmnt --verify --tab "$FSTAB" >"$WORK/logs/findmnt-verify-after-automount.txt" 2>&1; then
  pass "findmnt --verify 接受当前的 /etc/fstab"
else
  fail "findmnt --verify 接受当前的 /etc/fstab" \
    "$(head -3 "$WORK/logs/findmnt-verify-after-automount.txt" | tr '\n' ' ')"
fi
expect_eq "面板详情：automount 已打开" "True" \
  "$(volume "$VOL_ID_A" a >/dev/null; volget a 'd["data"]["automount"]')"

# 挂上，写一个“重启后必须还在”的标记文件。
VOL_VER=$(current_vol_version "$VOL_ID_A" b)
run_action storage.volume.mount "{\"volume_id\":\"$VOL_ID_A\",\"version\":\"$VOL_VER\"}" mount-for-boot
expect_verified "准备重启前的挂载"
printf 'p07-4 跨重启标记\n' >"$MP_A/persist.txt"
MARK_SHA=$(sha256sum "$MP_A/persist.txt" | cut -d' ' -f1)
expect_eq "标记文件在卷里" "$MARK_SHA" "$(sha256sum "$MP_A/persist.txt" | cut -d' ' -f1)"
wait_mounted "${BLANK_A}1" "$MP_A"

echo
echo "== 9. 外部编辑 fstab：旧预检不能覆盖它 =="
VOL_VER=$(current_vol_version "$VOL_ID_A" b)
run_action storage.volume.configure \
  "{\"volume_id\":\"$VOL_ID_A\",\"version\":\"$VOL_VER\",\"automount\":false}" configure-stale-pre
STALE_PRE_ID=$PRE_ID
expect_eq "（为下一步准备的预检）允许" "True" "$PRE_ALLOWED"
expect_eq "（为下一步准备的预检）要求再认证" "True" "$PRE_REAUTH"
FSTAB_BEFORE_EDIT=$(fstab_sha)
printf '# 验收脚本在预检之后手工加的一行（不得被旧任务覆盖）\n' >>"$FSTAB"
FSTAB_AFTER_EDIT=$(fstab_sha)
expect_ne "外部编辑确实改了文件" "$FSTAB_BEFORE_EDIT" "$FSTAB_AFTER_EDIT"
reauth >/dev/null
STALE_CODE=$(submit_and_wait "$STALE_PRE_ID" "" "stale-submit")
cp "$WORK/submit.json" "$PAYLOADS/submit-stale-fstab.json"
expect_eq "外部编辑后提交旧预检被拒" "submit:409" "$STALE_CODE"
expect_eq "拒绝码是 PREFLIGHT_STALE" "PREFLIGHT_STALE" \
  "$(jget "$WORK/submit.json" 'd["error"]["code"]')"
expect_eq "外部编辑的内容还在（旧任务没有覆盖它）" "$FSTAB_AFTER_EDIT" "$(fstab_sha)"
expect_eq "外部编辑之后这个 UUID 的条目仍只有一条" "1" "$(fstab_entry_count "$UUID_A")"
sed -i '/^# 验收脚本在预检之后手工加的一行/d' "$FSTAB"
expect_eq "手工编辑已还原" "$FSTAB_BEFORE_EDIT" "$(fstab_sha)"

echo
echo "== 10. 候选校验失败：原文件一个字节都不动 =="
# 先把自动挂载真的关掉：Write 在“渲染结果与原内容相同”时会直接返回、根本不校验候选，
# 所以这个用例必须让内容真的会变，否则它证明的是另一件事。
VOL_VER=$(current_vol_version "$VOL_ID_A" b)
run_action storage.volume.configure \
  "{\"volume_id\":\"$VOL_ID_A\",\"version\":\"$VOL_VER\",\"automount\":false}" configure-off
expect_verified "关闭自动挂载（为坏候选用例准备）"
expect_contains "条目重新带上 noauto" "noauto" "$(fstab_entry "$UUID_A")"

FSTAB_BEFORE_BAD=$(fstab_sha)
BACKUPS_BEFORE_BAD=$(backup_count)
# 在托管块**之外**塞一行解析不了的垃圾：产品的块解析不看块外，但 findmnt --verify
# 会报 parse error——这正是“候选不合格”的情形，而机器上的 /etc/fstab 必须原样保留。
printf 'this-line-is-not-a-valid-fstab-entry\n' >>"$FSTAB"
FSTAB_SEEDED=$(fstab_sha)
VOL_VER=$(current_vol_version "$VOL_ID_A" b)
run_action storage.volume.configure \
  "{\"volume_id\":\"$VOL_ID_A\",\"version\":\"$VOL_VER\",\"automount\":true}" configure-bad-candidate
expect_eq "坏行存在时改配置失败" "failed" "$JOB_STATE"
expect_eq "失败的错误码" "COMMAND_FAILED" "$(jerr)"
expect_contains "失败原因指向候选文件校验" "候选 fstab 未通过校验" "$(jerrmsg)"
expect_eq "失败之后 /etc/fstab 与塞坏行之后逐字节相同" "$FSTAB_SEEDED" "$(fstab_sha)"
expect_eq "失败之后没有多出备份（校验在换文件之前）" "$BACKUPS_BEFORE_BAD" "$(backup_count)"
expect_eq "失败之后没有遗留候选临时文件" "0" "$(temp_leftovers)"
sed -i '/^this-line-is-not-a-valid-fstab-entry$/d' "$FSTAB"
expect_eq "移除坏行后文件回到原样" "$FSTAB_BEFORE_BAD" "$(fstab_sha)"
# 坏行清掉之后再改一次：证明刚才是“候选不合格”，不是状态被打坏了。
VOL_VER=$(current_vol_version "$VOL_ID_A" b)
run_action storage.volume.configure \
  "{\"volume_id\":\"$VOL_ID_A\",\"version\":\"$VOL_VER\",\"automount\":true}" configure-after-bad
expect_verified "清除坏行之后同样的改动"
expect_eq "自动挂载又打开了" "True" \
  "$(volume "$VOL_ID_A" a >/dev/null; volget a 'd["data"]["automount"]')"
expect_not_contains "最后的条目里没有 noauto" "noauto" "$(fstab_entry "$UUID_A")"
expect_eq "标记文件仍在" "$MARK_SHA" "$(sha256sum "$MP_A/persist.txt" | cut -d' ' -f1)"

echo
echo "== 11. 回归：第二块盘上的卷不受影响 =="
expect_eq "卷列表里两个卷" "2" "$(volumes final >/dev/null; volsget final 'len(d["data"]["volumes"])')"
expect_eq "卷列表按挂载点排序" "$MP_A,$MP_B" \
  "$(volsget final '",".join(v["mount_path"] for v in d["data"]["volumes"])')"
expect_eq "第二个卷仍是 unmounted" "unmounted" \
  "$(volsget final "next(v[\"observed_state\"] for v in d[\"data\"][\"volumes\"] if v[\"slug\"]==\"$SLUG_ALT\")")"
expect_eq "第二个卷的 fstab 条目没被改动" \
  "UUID=$UUID_B $MP_B ext4 rw,nodev,nosuid,nofail,x-systemd.device-timeout=30,noauto 0 2" \
  "$(fstab_entry "$UUID_B")"
expect_eq "第一个卷现在还挂着" "True" "$(is_mounted "$MP_A")"

# 记下跨重启检查需要的事实。
FSTAB_SHA=$(fstab_sha)
{
  printf 'VOL_ID=%s\n' "$VOL_ID_A"
  printf 'VOL_UUID=%s\n' "$UUID_A"
  printf 'VOL_SLUG=%s\n' "$VOL_SLUG_A"
  printf 'VOL_MP=%s\n' "$MP_A"
  printf 'VOL_MP_B=%s\n' "$MP_B"
  printf 'VOL_SLUG_B=%s\n' "$SLUG_ALT"
  printf 'MARK_SHA=%s\n' "$MARK_SHA"
  printf 'FSTAB_SHA=%s\n' "$FSTAB_SHA"
} >"$STATE"
chmod 0644 "$STATE"
note "状态已写入 $STATE"

echo
echo "== 12. 收尾（第一段） =="
stop_web || true
stop_agent || true
if [ ! -S "$SOCK" ]; then pass "代理已停止、socket 已清理"; else fail "代理已停止、socket 已清理" "$SOCK 还在"; fi
expect_eq "停止面板之后卷还挂着（不是面板在维持它）" "True" "$(is_mounted "$MP_A")"
cp "$FSTAB" "$WORK/logs/fstab-before-reboot.txt"
note "现在的 /etc/fstab："
sed -n '1,30p' "$FSTAB" | while IFS= read -r l; do note "  $l"; done

echo
echo "== 汇总（第一段） =="
printf 'PASS=%d FAIL=%d NOT RUN=%d\n' "$PASSED" "$FAILED" "$NOTRUN"
[ "$FAILED" -eq 0 ] || printf 'FAILED:%s\n' "$FAILED_NAMES"
echo
echo "下一步：重启这台 VM，然后运行第二段："
echo "  sudo BIN_DIR=$BIN_DIR PHASE=post-boot WORK=$WORK sh $0"
echo "第二段会在**启动任何 upanel 进程之前**先看重启后的挂载与标记文件。"
[ "$FAILED" -eq 0 ] && exit 0
exit 1
