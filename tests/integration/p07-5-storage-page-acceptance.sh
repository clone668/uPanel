#!/bin/sh
# P07.5 Linux 验收脚本：磁盘页面完整交互 + 系统外部独立检查。
#
# 与 p07-1 … p07-4 的分工：那四份证明“该拒的拒”“分区表写对了”“文件系统做出来了”
# “卷能纳管、fstab 改动是原子的、跨重启会自动挂”。本脚本证明另外两件事：
#
#   1) **页面依赖的那份载荷是真的**：设备上四个动作判定（available/blocked/unsupported
#      与理由）、卷对象的 observed_state 与 capacity、占用列表里的 pid，都是服务端判决层
#      拍出来的；页面只按这些字段决定按钮与文案。这一节逐条钉住“页面看到什么”。
#   2) **一条不经过面板的命令**能把同一批事实再读一遍（`scripts/storage-external-check.sh`）：
#      fstab 托管块、findmnt 与内核挂载表、blkid、systemd 单元、备份权限。面板自己说
#      “一致”只是它的说法；两边对不上就是有一边在说假话。
#
# 这一份**重新走一遍写路径**（GPT → 分区 → ext4 → 导入 → 挂载 → 配置 → 卸载 → 退出管理），
# 不是为了重复 P07.2/3/4 的断言，而是为了让第 7 节的外部检查对着**由当前构建刚写出来的**
# 托管块运行——拿一份夹具 fstab 去检查，证明不了这台机器现在能对上。
#
# 用法：
#   sudo BIN_DIR=/var/tmp/upanel-bin SPARE_DISKS="sdb sdc" \
#        sh tests/integration/p07-5-storage-page-acceptance.sh
#
# 环境变量：PORT（默认 19475，避开 p07-1..4 的端口）、WORK、BIN_DIR、SPARE_DISKS
# （两块一次性空盘，会被清空——只允许在可丢弃的测试 VM 里跑）。
#
# 退出码：0 全部通过；1 有失败项；2 环境不满足（非 root / 盘不安全 / 缺工具）。
#
# 明确不验（NOT RUN，原因写在这里，不在脚本里假装通过）：
#   - **浏览器里的真实渲染**：本脚本验的是页面**读到的载荷**与它依赖的字段；把真实载荷
#     画成 DOM 由 web 侧的 SSR 回放覆盖（`web/src/fixtures/p07-5-vm/` + vitest），
#     浏览器端到端由 `web/scripts/smoke.mjs` 覆盖。三者不是同一件事，报告里分开写。
#   - **开机自动挂载 / 跨重启**：P07.4 已用一次真重启证明（Q195），本脚本不重复。
#   - **页面里九种动作的每一次点击**：设备动作已在 P07.2/P07.3 逐个验过，本脚本只重走
#     “为了拿到一个受管卷”必须的那几步，其余动作验的是**载荷形状**（第 2 节）。

set -u

BIN_DIR=${BIN_DIR:-/var/tmp/upanel-bin}

if [ "$(id -u)" -ne 0 ]; then
  printf '本脚本必须以 root 运行：sudo BIN_DIR=%s sh %s\n' "$BIN_DIR" "$0" >&2
  exit 2
fi

AGENT="$BIN_DIR/upanel-agent"
WEB="$BIN_DIR/upanel-web"
CTL="$BIN_DIR/upanelctl"

WORK=${WORK:-/var/tmp/upanel-accept-p07-5}
PAYLOADS=$WORK/payloads
RUN_DIR=/run/upanel
SOCK=$RUN_DIR/agent.sock
SYS_CFG=/etc/upanel/upanel.json
AGENT_DB_DIR=/var/lib/upanel-agent
AGENT_DB=$AGENT_DB_DIR/agent.db
BACKUP_DIR=$AGENT_DB_DIR/fstab-backups
KEY_FILE=/etc/upanel/agent.key
WEB_DB_DIR=$WORK/webdata
WEB_DB=$WEB_DB_DIR/upanel.db
PORT=${PORT:-19475}
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
CHECK_SH=${CHECK_SH:-/var/tmp/storage-external-check.sh}
GIB=1073741824
PART_MAIN=$((2 * GIB))
PART_ALT=$GIB
# 分区必须从第一块空闲区里开始：GPT 自己占掉盘头 1 MiB（外加备份表），
# 从 0 开始建分区会被判 REGION_NOT_FREE——那不是 bug，是产品在保护分区表（Q193）。
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
pget() { jget "$WORK/preflight.json" "$1"; }
jobget() { jget "$WORK/job.json" "$1"; }
devget() { jget "$WORK/device.json" "$1"; }
topoget() { jget "$WORK/topology.json" "$1"; }

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
joutcome() { jobget 'd["data"]["result"]["outcome"]'; }
jverified() { jobget 'bool(d["data"]["result"].get("verified_at"))'; }
jobobserved() { jobget "d[\"data\"][\"result\"][\"observed_state\"][\"$1\"]"; }
jobobservedcsv() {
  jobget "','.join(d[\"data\"][\"result\"][\"observed_state\"].get(\"$1\") or [])"
}
jobdata() { jobget "d[\"data\"][\"result\"][\"data\"].get(\"$1\")"; }

pallowed() { pget 'd["data"]["allowed"]'; }
pall() { pget '",".join(b["code"] for b in d["data"]["blockers"])'; }
pwhy() { pget '"; ".join(b["message"] for b in d["data"]["blockers"])'; }
pimpact() { pget '",".join(i["kind"] for i in d["data"]["impacts"])'; }
plocks() { pget '",".join(d["data"]["locks"])'; }
pstate() { pget 'd["data"]["target"]["display_name"]'; }
preauth() { pget 'bool(d["data"].get("requires_reauth"))'; }

expect_verified() {
  expect_eq "$1任务终态" "succeeded" "$JOB_STATE"
  expect_eq "$1结果是 verified" "verified" "$(joutcome)"
  expect_eq "$1带 verified_at" "True" "$(jverified)"
}

# ---------- 设备快照 ----------

# device <设备 id>：设备详情存成 $WORK/device.json，回显状态码。
device() {
  _code=$(api "$JAR" "$CSRF" GET "$STORE/devices/$1")
  cp "$WORK/resp.json" "$WORK/device.json"
  printf '%s' "$_code"
}
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
    device "$1" >/dev/null
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
sync_topo() { settle "$1" 'True'; }

# 参数是**分区** id（sdb1），不是整盘：拓扑只给整盘与已挂载的分区填 filesystem，
# 整盘的恒为 null，拿整盘等挂载永远等不到、等卸载永远瞬间成立（Q197）。
wait_mounted() {
  settle "$1" "any(m[\"path\"]==\"$2\" for m in ((d[\"data\"][\"device\"].get(\"filesystem\") or {}).get(\"mounts\") or []))"
}
wait_unmounted() {
  settle "$1" "not any(m[\"path\"]==\"$2\" for m in ((d[\"data\"][\"device\"].get(\"filesystem\") or {}).get(\"mounts\") or []))"
}

# ---------- 页面读的三个接口 ----------

topology() {
  _code=$(api "$JAR" "$CSRF" GET "$STORE/topology")
  cp "$WORK/resp.json" "$WORK/topology-$1.json"
  printf '%s' "$_code"
}
tget() { jget "$WORK/topology-$1.json" "$2"; }

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

blockers() {
  _code=$(api "$JAR" "$CSRF" GET "$STORE/volumes/$1/blockers")
  cp "$WORK/resp.json" "$WORK/blockers-$2.json"
  printf '%s' "$_code"
}
blkget() { jget "$WORK/blockers-$1.json" "$2"; }

current_vol_version() { volume "$1" "$2" >/dev/null; volget "$2" 'd["data"]["version"]'; }

# ---------- 独立证据（不经过产品的解析器） ----------

blkid_field() { blkid -p -o value -s "$2" "$1" 2>/dev/null | head -1; }
fs_type() { blkid_field "$1" TYPE; }
fs_uuid() { blkid_field "$1" UUID; }
fs_label() { blkid_field "$1" LABEL; }

is_mounted() {
  if [ -n "$(findmnt -rno TARGET --mountpoint "$1" 2>/dev/null)" ]; then echo True; else echo False; fi
}
mountinfo_line() { grep -F " $1 " /proc/self/mountinfo 2>/dev/null | head -1; }
findmnt_uuid() { findmnt -no UUID --mountpoint "$1" 2>/dev/null | head -1; }

fstab_sha() { sha256sum "$FSTAB" | cut -d' ' -f1; }
fstab_count() { grep -c . "$FSTAB" 2>/dev/null || true; }
fstab_mode() { stat -c '%a %U:%G' "$FSTAB" 2>/dev/null; }
fstab_entry() { grep -F "UUID=$1 " "$FSTAB" 2>/dev/null | head -1; }
fstab_entry_count() { grep -cF "UUID=$1 " "$FSTAB" 2>/dev/null || true; }
begin_markers() { grep -cx '# BEGIN upanel managed volumes' "$FSTAB" 2>/dev/null || true; }
end_markers() { grep -cx '# END upanel managed volumes' "$FSTAB" 2>/dev/null || true; }
block_lines() {
  awk '/^# BEGIN upanel managed volumes$/{f=1;next} /^# END upanel managed volumes$/{f=0} f && $0 !~ /^#/ && NF' "$FSTAB"
}
temp_leftovers() { ls /etc/fstab.upanel-new 2>/dev/null | wc -l | tr -d ' '; }
backup_count() { ls -1 "$BACKUP_DIR"/*.fstab 2>/dev/null | wc -l | tr -d ' '; }
backup_has_content() {
  _want=$1
  for f in "$BACKUP_DIR"/*.fstab; do
    [ -f "$f" ] || continue
    [ "$(sha256sum "$f" | cut -d' ' -f1)" = "$_want" ] && return 0
  done
  return 1
}
pid_fds_into() {
  _pid=$1; _mp=$2; _hit=0
  for l in /proc/"$_pid"/fd/*; do
    case "$(readlink "$l" 2>/dev/null)" in
      "$_mp"/*) _hit=1 ;;
    esac
  done
  [ "$_hit" = "1" ] && echo True || echo False
}
# 卷里有没有 pid 打开的 fd：直接扫全部 /proc，不依赖产品的扫描器。
any_pid_into() {
  _mp=$1; _out=""
  for d in /proc/[0-9]*; do
    _p=${d#/proc/}
    [ "$(pid_fds_into "$_p" "$_mp")" = "True" ] && _out="$_out $_p"
  done
  printf '%s' "${_out# }"
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
hold_file() {
  sh -c "exec 3<>'$1/$2'; exec sleep 600" >/dev/null 2>&1 &
  HOLDER_PID=$!
  sleep 1
}

# run_check <输出文件> [参数…]：跑外部独立检查脚本，回显退出码。
run_check() {
  _out=$1; shift
  sh "$CHECK_SH" "$@" >"$_out" 2>&1
  printf '%s' "$?"
}
check_has() { grep -qxF "$2" "$1" 2>/dev/null && echo True || echo False; }

echo "== 0. 环境与安全守卫 =="
for b in "$AGENT" "$WEB" "$CTL"; do
  if [ -x "$b" ]; then pass "二进制存在且可执行 $(basename "$b")"; else fail "二进制存在且可执行 $(basename "$b")" "$b"; fi
done
for t in python3 curl runuser sha256sum findmnt lsblk sgdisk blkid \
         wipefs udevadm blockdev systemd-escape stat awk diff grep; do
  if command -v "$t" >/dev/null 2>&1; then pass "依赖命令可用 $t"; else fail "依赖命令可用 $t" "缺少 $t"; fi
done
if [ -f "$CHECK_SH" ]; then pass "外部检查脚本存在 $CHECK_SH"; else
  fail "外部检查脚本存在 $CHECK_SH" "把 scripts/storage-external-check.sh 拷到 $CHECK_SH"
fi
note "findmnt: $(findmnt --version 2>&1 | head -1)"
note "整机 fstab（验收开始时）："
sed -n '1,20p' "$FSTAB" 2>/dev/null | while IFS= read -r l; do note "  $l"; done

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

# —— 干净的起点：上一轮（P07.4）留下的托管块、登记库、挂载点目录都清掉。
# 这一步不“顺便修东西”，它只让本脚本的断言对着一个已知的起点：没有托管块、
# 没有登记记录、卷根目录是空的。
for mp in "$VOLUME_ROOT"/*; do
  [ -d "$mp" ] || continue
  umount "$mp" 2>/dev/null || true
done
if [ "$(begin_markers)" != "0" ] || [ "$(end_markers)" != "0" ]; then
  note "清掉遗留的托管块（内容留档到 $WORK/logs/fstab-before-reset.txt）"
  mkdir -p "$WORK/logs" 2>/dev/null
  cp "$FSTAB" "$WORK/logs/fstab-before-reset.txt" 2>/dev/null || true
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
mkdir -p "$WORK/logs" "$PAYLOADS" "$WEB_DB_DIR"
chmod 0755 "$WORK" "$PAYLOADS" "$WORK/logs"
chown -R "$WEB_USER" "$WEB_DB_DIR"
cp "$FSTAB" "$WORK/logs/fstab-initial.txt"
chmod 0644 "$WORK/logs/fstab-initial.txt"
uname -r >"$WORK/logs/kernel.txt" 2>/dev/null || true
lsblk -o NAME,TYPE,SIZE,FSTYPE,MOUNTPOINTS >"$WORK/logs/lsblk-initial.txt" 2>&1

expect_eq "起点：fstab 里没有托管块" "0 0" "$(begin_markers) $(end_markers)"
expect_eq "起点：fstab 本身能通过 findmnt --verify" "0" \
  "$(findmnt --verify --tab "$FSTAB" >/dev/null 2>&1; echo $?)"
rm -rf "$VOLUME_ROOT"/* 2>/dev/null
install -d -m 0755 "$VOLUME_ROOT"
for d in $SPARE_DISKS; do
  wipefs -a "/dev/$d" >/dev/null 2>&1
  sgdisk --zap-all "/dev/$d" >/dev/null 2>&1
  blockdev --rereadpt "/dev/$d" >/dev/null 2>&1
  udevadm settle --timeout=5 >/dev/null 2>&1
done
pass "两块一次性盘清成空白"

echo
echo "== 1. 配置与启动（代理 root、Web 非 root） =="
install -d -m 0755 /etc/upanel
install -d -m 0700 "$AGENT_DB_DIR"
install -d -m 0755 "$BACKUP_DIR"
install -d -m 0755 "$RUN_DIR"
install -d -m 0755 "$VOLUME_ROOT"
rm -f "$AGENT_DB" "$AGENT_DB-wal" "$AGENT_DB-shm"
rm -f "$BACKUP_DIR"/*.fstab 2>/dev/null
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

echo
echo "== 2. 页面依赖的载荷：动作判定与空列表语义 =="
# 页面在这里读三样东西：拓扑（设备列表与每个设备上的四个动作判定）、卷列表、卷详情。
# 这一节钉的不是“产品能干什么”，而是“页面拿到的东西能不能据以画按钮”——
# 缺字段、多字段、状态越界都会让页面要么误报“没有可做的事”，要么摆出一个点不动的按钮。
expect_eq "拓扑可读" "200" "$(topology page)"
cp "$WORK/topology-page.json" "$PAYLOADS/page-topology.json"
expect_eq "拓扑 status 是 ok" "ok" "$(tget page 'd["data"]["status"]')"
expect_eq "拓扑里所有设备都有四个动作判定" "True" \
  "$(tget page 'all(len(n["capabilities"])>=4 for n in d["data"]["nodes"] if n["kind"]=="disk")')"
expect_eq "动作判定里的状态都落在闭集里" "True" \
  "$(tget page 'all(c["state"] in ("available","blocked","unsupported") for n in d["data"]["nodes"] for c in n["capabilities"])')"
expect_eq "blocked 的判定都带理由（页面直接显示这句）" "True" \
  "$(tget page 'all(bool(c.get("reason")) for n in d["data"]["nodes"] for c in n["capabilities"] if c["state"]=="blocked")')"
expect_eq "available 的判定不带阻塞理由" "False" \
  "$(tget page 'any(bool(c.get("reason")) for n in d["data"]["nodes"] for c in n["capabilities"] if c["state"]=="available")')"
# 系统盘：四个动作一个都不能是 available（否则页面会画出能点的按钮）。
expect_eq "系统盘上的写动作全部不可执行" "True" \
  "$(tget page "all(c[\"state\"]!=\"available\" for n in d[\"data\"][\"nodes\"] if n[\"name\"]==\"${ROOT_DISK}\" for c in n[\"capabilities\"])")"
# 空白整盘：初始化分区表可执行，而“对整盘格式化”“在整盘上建分区”是 unsupported。
expect_eq "空白盘上初始化分区表可执行" "available" \
  "$(tget page "next((c[\"state\"] for n in d[\"data\"][\"nodes\"] if n[\"name\"]==\"$BLANK_A\" for c in n[\"capabilities\"] if c[\"action\"]==\"storage.partition_table.initialize\"), \"missing\")")"
expect_eq "对整盘格式化是 unsupported" "unsupported" \
  "$(tget page "next((c[\"state\"] for n in d[\"data\"][\"nodes\"] if n[\"name\"]==\"$BLANK_A\" for c in n[\"capabilities\"] if c[\"action\"]==\"storage.filesystem.format\"), \"missing\")")"
expect_eq "volumes 列表在没有任何卷时是空数组" "True" \
  "$(volumes page-empty >/dev/null; volsget page-empty 'd["data"]["volumes"] == []')"
cp "$WORK/volumes-page-empty.json" "$PAYLOADS/page-volumes-empty.json"

echo
echo "== 3. 夹具：两块空盘走产品的 GPT → 分区 → ext4 =="
PART_NAME=data
run_action storage.partition_table.initialize \
  "{\"device_id\":\"$BLANK_A\",\"table_type\":\"gpt\"}" init-a
expect_eq "初始化分区表预检允许" "True" "$PRE_ALLOWED"
expect_verified "初始化分区表"
expect_eq "初始化的短语动词是 INIT" "True" \
  "$(pget 'd["data"].get("confirmation_phrase","").startswith("INIT")')"
sync_topo "$BLANK_A"
expect_eq "独立证据：blkid 读出的分区表类型是 gpt" "gpt" \
  "$(blkid -p -o value -s PTTYPE "/dev/$BLANK_A" 2>/dev/null | head -1)"

run_action storage.partition.create \
  "{\"device_id\":\"$BLANK_A\",\"start_bytes\":\"$PART_START\",\"size_bytes\":\"$PART_MAIN\",\"name\":\"$PART_NAME\"}" create-a
expect_eq "建分区预检允许" "True" "$PRE_ALLOWED"
expect_verified "建分区"
sync_topo "$BLANK_A"
expect_eq "独立证据：内核看到了 ${BLANK_A}1" "True" \
  "$([ -b "/dev/${BLANK_A}1" ] && echo True || echo False)"
run_action storage.filesystem.format \
  "{\"device_id\":\"${BLANK_A}1\",\"filesystem\":\"ext4\",\"label\":\"$LABEL_MAIN\"}" format-a
expect_eq "格式化预检允许" "True" "$PRE_ALLOWED"
expect_verified "格式化"
expect_eq "独立证据：blkid 读出的类型是 ext4" "ext4" "$(fs_type "/dev/${BLANK_A}1")"
expect_eq "独立证据：blkid 读出的卷标" "$LABEL_MAIN" "$(fs_label "/dev/${BLANK_A}1")"
UUID_A=$(fs_uuid "/dev/${BLANK_A}1")
expect_ne "独立证据：UUID 非空" "" "$UUID_A"
# 设备详情页拿到的就是这份载荷：分区上要有文件系统与 UUID（导入表单靠它列出候选），
# 整盘上要有分区表条目（页面按“盘上的字节”画表格）。等采集器重扫一次再读。
sync_topo "$BLANK_A"
expect_eq "整盘详情可读" "200" "$(device "$BLANK_A")"
cp "$WORK/device.json" "$PAYLOADS/page-device-disk.json"
expect_eq "整盘详情：分区表条目里能看到刚建的分区" "True" \
  "$(devget 'any(str(e["number"])=="1" for e in (d["data"]["device"]["partition_table"].get("disk_entries") or []))')"
expect_eq "整盘详情：四个动作判定都在" "4" \
  "$(devget 'len([c for c in d["data"]["device"]["capabilities"]])')"
device "${BLANK_A}1" >/dev/null
cp "$WORK/device.json" "$PAYLOADS/page-device-partition.json"
# 这里**不能**断言分区详情里的 type=="ext4"。分区此刻没挂载，按 CONTRACTS 第 231 行，
# 未挂载分区上的 filesystem.type 就是 null——产品不调用 blkid、不靠猜。UUID 也不是
# 必然读得到：scan.go 只在 by-uuid 链接存在时才填，udev 抢跑时整个 filesystem 会是 null。
# 能断言的是不变量——读到的东西只能是内核那个值，或者读不到，绝不会是编的。
expect_eq "分区详情：未挂载时文件系统类型按契约为空（不是猜的）" "True" \
  "$(devget '((d["data"]["device"].get("filesystem") or {}).get("type")) is None')"
expect_eq "分区详情：UUID 只能是内核那个，或者读不到" "True" \
  "$(devget '((d["data"]["device"].get("filesystem") or {}).get("uuid")) in (None, "'"$UUID_A"'")')"

run_action storage.partition.create \
  "{\"device_id\":\"$BLANK_B\",\"start_bytes\":\"$PART_START\",\"size_bytes\":\"$PART_ALT\",\"name\":\"logs\"}" create-b-pre
# 第二块盘要先有分区表；上面那条会因为没有 GPT 被拒——这正是“拒绝要有理由”的现场。
expect_eq "没有分区表时建分区被拒" "False" "$PRE_ALLOWED"
expect_contains "拒绝的阻塞码" "PARTITION_TABLE" "$(pall)"
run_action storage.partition_table.initialize \
  "{\"device_id\":\"$BLANK_B\",\"table_type\":\"gpt\"}" init-b
expect_verified "第二块盘初始化分区表"
run_action storage.partition.create \
  "{\"device_id\":\"$BLANK_B\",\"start_bytes\":\"$PART_START\",\"size_bytes\":\"$PART_ALT\",\"name\":\"logs\"}" create-b
expect_verified "第二块盘建分区"
sync_topo "$BLANK_B"
run_action storage.filesystem.format \
  "{\"device_id\":\"${BLANK_B}1\",\"filesystem\":\"ext4\",\"label\":\"$LABEL_ALT\"}" format-b
expect_verified "第二块盘格式化"
UUID_B=$(fs_uuid "/dev/${BLANK_B}1")
expect_ne "独立证据：第二块盘的 UUID 非空" "" "$UUID_B"

echo
echo "== 4. 导入两个卷：fstab 最小修改、备份、挂载点由 slug 派生 =="
MP_A=$VOLUME_ROOT/$SLUG_MAIN
MP_B=$VOLUME_ROOT/$SLUG_ALT
FSTAB_BEFORE=$(fstab_sha)
FSTAB_MODE_BEFORE=$(fstab_mode)
BACKUPS_BEFORE=$(backup_count)
run_action storage.volume.import \
  "{\"device_id\":\"${BLANK_A}1\",\"slug\":\"$SLUG_MAIN\",\"display_name\":\"数据卷\"}" import-a
expect_eq "导入预检允许" "True" "$PRE_ALLOWED"
expect_eq "导入不需要再认证" "False" "$PRE_REAUTH"
expect_eq "导入没有确认短语" "" "$PRE_PHRASE"
expect_contains "导入的锁里有整盘" "disk:$BLANK_A" "$(plocks)"
expect_verified "导入"
VOL_ID_A=$(jobdata volume_id)
expect_eq "卷标识是 vol_ 形状" "vol_" "$(printf '%s' "$VOL_ID_A" | cut -c1-4)"
expect_eq "备份目录里多了一份" "$((BACKUPS_BEFORE + 1))" "$(backup_count)"
expect_eq "备份的内容就是写之前的 fstab" "True" \
  "$(backup_has_content "$FSTAB_BEFORE" && echo True || echo False)"
expect_eq "fstab 权限与属主没被顺手改掉" "$FSTAB_MODE_BEFORE" "$(fstab_mode)"
expect_eq "挂载点由 slug 派生" "$MP_A" "$(jobdata mount_path)"
expect_eq "挂载点目录已创建" "True" "$([ -d "$MP_A" ] && echo True || echo False)"
expect_eq "导入没把卷挂上（不改运行状态）" "False" "$(is_mounted "$MP_A")"
expect_eq "写入的条目带 noauto（导入默认不开自动挂载）" "noauto" \
  "$(case "$(fstab_entry "$UUID_A")" in *noauto*) echo noauto ;; *) echo none ;; esac)"

run_action storage.volume.import \
  "{\"device_id\":\"${BLANK_B}1\",\"slug\":\"$SLUG_ALT\",\"display_name\":\"日志卷\"}" import-b
expect_verified "第二个卷导入"
VOL_ID_B=$(jobdata volume_id)
expect_eq "托管块里现在两条条目" "2" "$(block_lines | wc -l | tr -d ' ')"
expect_contains "条目按挂载点排序" "$MP_A" "$(block_lines | head -1)"

# 页面的 configure 只提交**真的改了的**项（automount）。开着自动挂载之后 noauto 必须消失。
VOL_VER=$(current_vol_version "$VOL_ID_A" c1)
FSTAB_BEFORE_CFG=$(fstab_sha)
run_action storage.volume.configure \
  "{\"volume_id\":\"$VOL_ID_A\",\"version\":\"$VOL_VER\",\"automount\":true}" configure-on
expect_eq "配置预检允许" "True" "$PRE_ALLOWED"
expect_eq "配置要求再认证" "True" "$PRE_REAUTH"
expect_verified "配置（开自动挂载）"
expect_not_contains "条目里不再有 noauto" "noauto" "$(fstab_entry "$UUID_A")"
expect_eq "配置也留了备份" "True" \
  "$(backup_has_content "$FSTAB_BEFORE_CFG" && echo True || echo False)"
expect_eq "另一个卷的条目没被改动" "1" "$(fstab_entry_count "$UUID_B")"

echo
echo "== 5. 挂载：容量只在真挂着时有，文件也能真写进去 =="
VOL_VER=$(current_vol_version "$VOL_ID_A" m1)
run_action storage.volume.mount "{\"volume_id\":\"$VOL_ID_A\",\"version\":\"$VOL_VER\"}" mount-a
expect_eq "挂载预检允许" "True" "$PRE_ALLOWED"
# 挂载**不**要求再认证：它不改持久配置，只是把已经登记的东西挂起来（改配置/卸载/退出管理才要）。
expect_eq "挂载不要求再认证" "False" "$PRE_REAUTH"
expect_verified "挂载"
expect_eq "内核视图：挂上了" "True" "$(is_mounted "$MP_A")"
expect_eq "内核视图：挂载来源的 UUID 就是登记的那个" "$UUID_A" "$(findmnt_uuid "$MP_A")"
expect_contains "内核挂载表里有它" "$MP_A" "$(mountinfo_line "$MP_A")"
wait_mounted "${BLANK_A}1" "$MP_A"

# 同一个分区，挂上之后页面就读得到类型和挂载点了——跟第 3 节“没挂载时为空”对照着看，
# 那个空不是实现偷懒，是契约。
device "${BLANK_A}1" >/dev/null
cp "$WORK/device.json" "$PAYLOADS/page-device-partition-mounted.json"
expect_eq "挂载后分区详情的类型是 ext4" "ext4" "$(devget 'd["data"]["device"]["filesystem"]["type"]')"
expect_eq "挂载后分区详情能看到挂载点" "True" \
  "$(devget 'any(m["path"]=="'"$MP_A"'" for m in (d["data"]["device"]["filesystem"]["mounts"] or []))')"

volume "$VOL_ID_A" mounted >/dev/null
cp "$WORK/volume-mounted.json" "$PAYLOADS/page-volume-mounted.json"
volumes mounted >/dev/null
cp "$WORK/volumes-mounted.json" "$PAYLOADS/page-volumes-one-mounted.json"
expect_eq "页面的卷详情：observed_state=mounted" "mounted" "$(volget mounted 'd["data"]["observed_state"]')"
expect_eq "页面的卷详情：desired_state=mounted" "mounted" "$(volget mounted 'd["data"]["desired_state"]')"
expect_eq "页面的卷详情：capacity 不是 null" "True" "$(volget mounted 'bool(d["data"].get("capacity"))')"
expect_eq "页面的卷详情：capacity 的字节数大于 0" "True" \
  "$(volget mounted 'int(d["data"]["capacity"]["total_bytes"]) > 0')"
expect_eq "页面的卷详情：没有 observed_error" "" "$(volget mounted 'd["data"].get("observed_error") or ""')"

printf '%s\n' "P07.5 标记文件" >"$MP_A/persist.txt"
MARK_SHA=$(sha256sum "$MP_A/persist.txt" | cut -d' ' -f1)
expect_eq "文件落在卷的挂载点上（stat 与 findmnt 两条路径一致）" "$MP_A" "$(stat -c '%m' "$MP_A/persist.txt")"
expect_eq "另一个卷仍然没挂" "False" "$(is_mounted "$MP_B")"

echo
echo "== 6. 占用列表：说得出是谁占着（页面直接显示这个 pid） =="
hold_file "$MP_A" hold.txt
DIRECT_PIDS=$(any_pid_into "$MP_A")
expect_ne "独立证据：确实有进程开着卷里的文件" "" "$DIRECT_PIDS"
expect_eq "占用接口可读" "200" "$(blockers "$VOL_ID_A" busy)"
cp "$WORK/blockers-busy.json" "$PAYLOADS/page-blockers-busy.json"
expect_eq "占用接口：mounted=true" "True" "$(blkget busy 'd["data"]["mounted"]')"
expect_eq "占用接口：busy=true" "True" "$(blkget busy 'd["data"]["busy"]')"
LISTED_PIDS=$(blkget busy '",".join(p["pid"] for p in d["data"]["processes"])')
expect_ne "占用列表里列出了进程" "" "$LISTED_PIDS"
# 逐个核对：产品点名的每个 pid 都真的开着这个卷里的文件（不许编）。
BAD_PIDS=""
for p in $(printf '%s' "$LISTED_PIDS" | tr ',' ' '); do
  [ "$(pid_fds_into "$p" "$MP_A")" = "True" ] || BAD_PIDS="$BAD_PIDS $p"
done
expect_eq "产品点名的每个 pid 都真的开着这个卷里的文件" "" "$BAD_PIDS"
# 反向：脚本自己扫出来的那个占用者，产品也必须点名。
SELF_PID=$(printf '%s' "$DIRECT_PIDS" | tr ' ' '\n' | grep -x "$HOLDER_PID" | head -1)
if [ -n "$SELF_PID" ]; then
  expect_contains "脚本自己开的那个 fd 也在列表里" "$SELF_PID" "$(printf '%s' "$LISTED_PIDS" | tr ',' ' ')"
else
  notrun "脚本自己开的那个 fd 也在列表里" "occupier pid $HOLDER_PID 没扫到它自己的 fd（可能已被回收）"
fi
expect_eq "占用者的 comm 来自 /proc（不是编的名字）" "sleep" \
  "$(blkget busy "next((p[\"comm\"] for p in d[\"data\"][\"processes\"] if p[\"pid\"]==\"$HOLDER_PID\"), \"\")")"
kill_holder
sleep 1
blockers "$VOL_ID_A" idle >/dev/null
expect_eq "占用者退出后回到不忙" "False" "$(blkget idle 'd["data"]["busy"]')"
cp "$WORK/blockers-idle.json" "$PAYLOADS/page-blockers-idle.json"

echo
echo "== 7. 系统外部独立检查：一条不经过面板的命令把同一批事实再读一遍 =="
EXPECT_ENTRY_A="UUID=$UUID_A $MP_A ext4 rw,nodev,nosuid,nofail,x-systemd.device-timeout=30 0 2"
expect_eq "fstab 条目与期望逐字一致（自动挂载已打开，所以没有 noauto）" \
  "$EXPECT_ENTRY_A" "$(fstab_entry "$UUID_A")"
note "现在的托管块："
block_lines | while IFS= read -r l; do note "  $l"; done

if [ ! -f "$CHECK_SH" ]; then
  notrun "外部独立检查" "缺少 $CHECK_SH"
else
  # 7.1 正例：真实 /etc/fstab，一个卷挂着、一个卷没挂。
  RC=$(run_check "$WORK/logs/ext-real.txt" --expect "$MP_A=mounted" --expect "$MP_B=unmounted")
  cp "$WORK/logs/ext-real.txt" "$PAYLOADS/external-check-real.txt"
  expect_eq "外部检查对着真实 fstab 的退出码" "0" "$RC"
  expect_contains "外部检查的结论" "RESULT 一致" "$(cat "$WORK/logs/ext-real.txt")"
  expect_contains "外部检查认得托管块结构" "托管块结构完整" "$(cat "$WORK/logs/ext-real.txt")"
  expect_contains "外部检查确认这个卷挂着" "$MP_A: 现在挂着" "$(cat "$WORK/logs/ext-real.txt")"
  expect_contains "外部检查确认另一个卷没挂" "$MP_B: 现在没有挂载" "$(cat "$WORK/logs/ext-real.txt")"
  expect_contains "外部检查确认 UUID 指向的设备" "$MP_A: UUID $UUID_A 现在是 /dev/${BLANK_A}1" \
    "$(cat "$WORK/logs/ext-real.txt")"
  expect_contains "外部检查确认 fsck 顺序" "$MP_A: ext4 的 fsck 顺序是 2" "$(cat "$WORK/logs/ext-real.txt")"
  expect_contains "外部检查确认 nofail" "$MP_A: 选项里有 nofail" "$(cat "$WORK/logs/ext-real.txt")"
  expect_contains "外部检查确认 systemd 单元" "$MP_A: systemd 里有 srv-upanel-volumes-$SLUG_MAIN.mount" \
    "$(cat "$WORK/logs/ext-real.txt")"
  expect_contains "外部检查确认备份权限" "最新的备份权限是 0600" "$(cat "$WORK/logs/ext-real.txt")"
  expect_eq "外部检查没有 FAIL 行" "0" "$(grep -c '^FAIL' "$WORK/logs/ext-real.txt" || true)"

  # 7.2 交叉核对：脚本说的“挂着”“UUID 指向谁”，脚本之外的命令再各读一遍。
  expect_eq "交叉核对：findmnt 的挂载点是 $MP_A" "True" "$(is_mounted "$MP_A")"
  expect_eq "交叉核对：blkid -U 指到 /dev/${BLANK_A}1" "/dev/${BLANK_A}1" "$(blkid -U "$UUID_A" 2>/dev/null)"
  expect_eq "交叉核对：systemd 认得挂载单元" "loaded" \
    "$(systemctl show -p LoadState --value "srv-upanel-volumes-$SLUG_MAIN.mount" 2>/dev/null | head -1)"
  expect_eq "交叉核对：挂载点目录的设备号与父目录不同" "True" \
    "$([ "$(stat -c %d "$MP_A")" != "$(stat -c %d "$VOLUME_ROOT")" ] && echo True || echo False)"

  # 7.3 --expect 的方向也要能说“不对”：把挂着的那条断言成没挂。
  RC=$(run_check "$WORK/logs/ext-expect-wrong.txt" --expect "$MP_A=unmounted")
  cp "$WORK/logs/ext-expect-wrong.txt" "$PAYLOADS/external-check-expect-wrong.txt"
  expect_eq "把挂着的卷断言成没挂：退出码非 0" "1" "$RC"
  expect_contains "并且说清是哪一条对不上" "$MP_A: 期望 unmounted，实际 mounted" \
    "$(cat "$WORK/logs/ext-expect-wrong.txt")"

  # 7.4 故障注入：在 fstab 的**副本**上制造五种典型坏法，检查脚本必须各自抓住。
  # 副本不碰 /etc/fstab，所以不会把机器弄坏，也不用担心 id。
  FAULT=$WORK/fixtures/fault.fstab
  mkdir -p "$WORK/fixtures"
  # 5a 缺 nofail（把这一行选项里的 nofail 去掉，别的字节一个不动）
  python3 - "$FSTAB" "$FAULT.nofail" "$UUID_A" <<'PY'
import sys
src, dst, uuid = sys.argv[1], sys.argv[2], sys.argv[3]
out = []
for line in open(src, encoding='utf-8'):
    if line.startswith('UUID=%s ' % uuid):
        cols = line.rstrip('\n').split(' ')
        cols[3] = ','.join(o for o in cols[3].split(',') if o != 'nofail')
        line = ' '.join(cols) + '\n'
    out.append(line)
open(dst, 'w', encoding='utf-8').write(''.join(out))
PY
  RC=$(run_check "$WORK/logs/ext-fault-nofail.txt" --fstab "$FAULT.nofail")
  expect_eq "缺 nofail 的副本：退出码非 0" "1" "$RC"
  expect_contains "缺 nofail 的副本：说清是哪一条" "$MP_A: 选项里没有 nofail" \
    "$(cat "$WORK/logs/ext-fault-nofail.txt")"

  # 5b ext4 的 fsck 顺序写成 0
  python3 - "$FSTAB" "$FAULT.pass" "$UUID_A" <<'PY'
import sys
src, dst, uuid = sys.argv[1], sys.argv[2], sys.argv[3]
out = []
for line in open(src, encoding='utf-8'):
    if line.startswith('UUID=%s ' % uuid):
        cols = line.rstrip('\n').split(' ')
        cols[5] = '0'
        line = ' '.join(cols) + '\n'
    out.append(line)
open(dst, 'w', encoding='utf-8').write(''.join(out))
PY
  RC=$(run_check "$WORK/logs/ext-fault-pass.txt" --fstab "$FAULT.pass")
  expect_eq "fsck 顺序写错的副本：退出码非 0" "1" "$RC"
  expect_contains "fsck 顺序写错的副本：说清是哪一条" "$MP_A: ext4 的 fsck 顺序是 0（应为 2）" \
    "$(cat "$WORK/logs/ext-fault-pass.txt")"

  # 5c UUID 在这台机器上不存在（盘被拔了/分区被换了）
  GHOST_UUID=11111111-2222-3333-4444-555555555555
  sed "s/^UUID=$UUID_B /UUID=$GHOST_UUID /" "$FSTAB" >"$FAULT.ghost"
  RC=$(run_check "$WORK/logs/ext-fault-ghost.txt" --fstab "$FAULT.ghost")
  expect_eq "UUID 不存在的副本：退出码非 0" "1" "$RC"
  expect_contains "UUID 不存在的副本：说清是哪一条" "$MP_B: 系统里找不到 UUID=$GHOST_UUID 的设备" \
    "$(cat "$WORK/logs/ext-fault-ghost.txt")"

  # 5d 两个条目指向同一个挂载点
  printf 'UUID=%s %s ext4 rw,nodev,nosuid,nofail,x-systemd.device-timeout=30 0 2\n' \
    "$UUID_B" "$MP_A" >"$WORK/fixtures/dup.extra"
  cp "$FSTAB" "$FAULT.dup"
  python3 - "$FAULT.dup" "$WORK/fixtures/dup.extra" <<'PY'
import sys
tab, extra = sys.argv[1], sys.argv[2]
text = open(tab, encoding='utf-8').read().splitlines(True)
add = open(extra, encoding='utf-8').read()
out = []
for line in text:
    if line.strip() == '# END upanel managed volumes':
        out.append(add)
    out.append(line)
open(tab, 'w', encoding='utf-8').write(''.join(out))
PY
  RC=$(run_check "$WORK/logs/ext-fault-dup.txt" --fstab "$FAULT.dup")
  expect_eq "重复挂载点的副本：退出码非 0" "1" "$RC"
  expect_contains "重复挂载点的副本：说清是哪一条" "托管块内出现重复挂载点" \
    "$(cat "$WORK/logs/ext-fault-dup.txt")"

  # 5e BEGIN/END 不配对（有人手工动过托管块）
  grep -v -x -F '# END upanel managed volumes' "$FSTAB" >"$FAULT.pair"
  RC=$(run_check "$WORK/logs/ext-fault-pair.txt" --fstab "$FAULT.pair")
  expect_eq "托管块不配对的副本：退出码非 0" "1" "$RC"
  expect_contains "托管块不配对的副本：说清是哪一条" "BEGIN/END 不配对" \
    "$(cat "$WORK/logs/ext-fault-pair.txt")"

  # 5f 一个正常副本：证明确实是“注入的那一处”被抓，不是脚本见谁都报错。
  cp "$FSTAB" "$FAULT.clean"
  RC=$(run_check "$WORK/logs/ext-fault-clean.txt" --fstab "$FAULT.clean")
  expect_eq "干净的副本：退出码 0（注入的坏法才是坏法）" "0" "$RC"
fi

echo
echo "== 8. 卸载 → 重挂载：文件一致 =="
VOL_VER=$(current_vol_version "$VOL_ID_A" u1)
run_action storage.volume.unmount "{\"volume_id\":\"$VOL_ID_A\",\"version\":\"$VOL_VER\"}" unmount-a
expect_eq "卸载预检允许" "True" "$PRE_ALLOWED"
expect_verified "卸载"
expect_eq "内核视图：没有挂载点了" "False" "$(is_mounted "$MP_A")"
wait_unmounted "${BLANK_A}1" "$MP_A"
volume "$VOL_ID_A" unmounted >/dev/null
cp "$WORK/volume-unmounted.json" "$PAYLOADS/page-volume-unmounted.json"
expect_eq "页面的卷详情：observed_state=unmounted" "unmounted" \
  "$(volget unmounted 'd["data"]["observed_state"]')"
expect_eq "页面的卷详情：capacity 是 null（不是 0）" "False" \
  "$(volget unmounted 'bool(d["data"].get("capacity"))')"

VOL_VER=$(current_vol_version "$VOL_ID_A" m2)
run_action storage.volume.mount "{\"volume_id\":\"$VOL_ID_A\",\"version\":\"$VOL_VER\"}" remount-a
expect_verified "重挂载"
expect_eq "重挂载之后文件内容一致" "$MARK_SHA" "$(sha256sum "$MP_A/persist.txt" | cut -d' ' -f1)"
expect_eq "重挂载之后 UUID 仍然对得上" "$UUID_A" "$(findmnt_uuid "$MP_A")"

echo
echo "== 9. 退出管理：托管块回到干净状态 =="
VOL_VER=$(current_vol_version "$VOL_ID_A" u2)
run_action storage.volume.unmount "{\"volume_id\":\"$VOL_ID_A\",\"version\":\"$VOL_VER\"}" unmount-a2
expect_verified "退出管理之前的卸载"
VOL_VER=$(current_vol_version "$VOL_ID_A" f1)
FSTAB_BEFORE_FORGET=$(fstab_sha)
run_action storage.volume.forget "{\"volume_id\":\"$VOL_ID_A\",\"version\":\"$VOL_VER\"}" forget-a
expect_eq "退出管理要求再认证" "True" "$PRE_REAUTH"
expect_verified "退出管理"
expect_eq "托管块里没有这条 UUID 了" "0" "$(fstab_entry_count "$UUID_A")"
expect_eq "退出管理没有把另一个卷弄丢" "1" "$(fstab_entry_count "$UUID_B")"
expect_eq "退出管理留了备份" "True" \
  "$(backup_has_content "$FSTAB_BEFORE_FORGET" && echo True || echo False)"
expect_eq "卷详情现在是 404" "404" "$(volume "$VOL_ID_A" gone)"
expect_eq "卷列表里只剩一个卷" "1" "$(volumes after-forget >/dev/null; volsget after-forget 'len(d["data"]["volumes"])')"

if [ -f "$CHECK_SH" ]; then
  RC=$(run_check "$WORK/logs/ext-after-forget.txt")
  cp "$WORK/logs/ext-after-forget.txt" "$PAYLOADS/external-check-after-forget.txt"
  expect_eq "退出管理之后外部检查仍然一致" "0" "$RC"
  expect_contains "并且知道剩下的那一条还在" "$MP_B" "$(cat "$WORK/logs/ext-after-forget.txt")"
fi

# 收尾：第二个卷也退出管理，机器回到没有托管块的状态。
VOL_VER=$(current_vol_version "$VOL_ID_B" f2)
run_action storage.volume.forget "{\"volume_id\":\"$VOL_ID_B\",\"version\":\"$VOL_VER\"}" forget-b
expect_verified "第二个卷退出管理"
# 退出管理只删**自己那一行**，BEGIN/END 标记留着（P07.4 起就是这个行为，写进了
# docs/STORAGE_RECOVERY.md §3.3：下一块盘进来时不用重建托管块）。
# 所以这里不能断言“标记没了”——那是把愿望当成契约。要断言的是“块里空了、块外没动”。
expect_eq "托管块还在，但里面已经没有条目" "1 1 0" \
  "$(begin_markers) $(end_markers) $(block_lines | wc -l | tr -d ' ')"
expect_eq "块外的内容逐字未变" "True" \
  "$(awk '/^# BEGIN upanel managed volumes$/{f=1;next} /^# END upanel managed volumes$/{f=0;next} !f' \
       "$FSTAB" >"$WORK/logs/fstab-prefix-now.txt"; \
     cmp -s "$WORK/logs/fstab-initial.txt" "$WORK/logs/fstab-prefix-now.txt" && echo True || echo False)"
expect_eq "fstab 仍能通过 findmnt --verify" "0" \
  "$(findmnt --verify --tab "$FSTAB" >/dev/null 2>&1; echo $?)"
if [ -f "$CHECK_SH" ]; then
  RC=$(run_check "$WORK/logs/ext-final.txt")
  cp "$WORK/logs/ext-final.txt" "$PAYLOADS/external-check-final.txt"
  expect_eq "一个卷都没有时外部检查也是一致" "0" "$RC"
  expect_contains "并且说得清块里是空的" "托管块内条目: 0" "$(cat "$WORK/logs/ext-final.txt")"
fi

echo
echo "== 10. 收尾 =="
stop_web || true
stop_agent || true
if [ ! -S "$SOCK" ]; then pass "代理已停止、socket 已清理"; else fail "代理已停止、socket 已清理" "$SOCK 还在"; fi
cp "$FSTAB" "$WORK/logs/fstab-final.txt"
lsblk -o NAME,SIZE,FSTYPE,LABEL,UUID,MOUNTPOINTS >"$WORK/logs/lsblk-final.txt" 2>&1
notrun "浏览器里的真实渲染" "本脚本验载荷；DOM 由 web 侧 SSR 回放覆盖，浏览器端到端由 smoke 覆盖"
notrun "开机自动挂载 / 跨重启" "P07.4 已用一次真重启证明（Q195），本脚本不重复"
notrun "九种动作的每一次点击" "设备动作由 P07.2/P07.3 验收覆盖；本脚本验的是载荷形状"

echo
echo "== 汇总 =="
printf 'PASS=%d FAIL=%d NOT RUN=%d\n' "$PASSED" "$FAILED" "$NOTRUN"
[ "$FAILED" -eq 0 ] || printf 'FAILED:%s\n' "$FAILED_NAMES"
echo
echo "证据留在 $WORK（payloads/ 是页面与任务的原始响应，logs/ 是外部检查的输出）"
[ "$FAILED" -eq 0 ] && exit 0
exit 1
