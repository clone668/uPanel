#!/bin/sh
# P07.3 Linux 验收脚本：格式化（ext4 / XFS）。
#
# 逐条验证 STAGES §9 P07.3 的六件事：ext4/XFS 格式化、标签校验、确认短语、
# 禁止取消、执行前二次身份检查、错误退出与进程中断。
#
# 与 p07-1 / p07-2 的关系：那两份证明“该拒的都拒”“分区表该写的写对了”；
# 本脚本证明“文件系统真的按请求做了出来，而且读回来对得上”——**退出码 0 不算成功**，
# 任务的 result.outcome 必须是 verified。
#
# 运行位置：Ubuntu 24.04 验收 VM 内，必须以 root 运行（代理只能在 root 下启动，
# mkfs 也由代理以 root 执行）。不依赖 Go 工具链：用宿主交叉编译好的 Linux 二进制。
#
# 用法：
#   sudo BIN_DIR=/tmp/upanel-bin SPARE_DISKS="sdb sdc sdd" \
#        sh tests/integration/p07-3-storage-format-acceptance.sh
#
# 可选环境变量：
#   PORT=19473         Web 监听端口（与 p07-1 / p07-2 不同，避免撞车）
#   SPARE_DISKS        三块**一次性**空盘的设备名（默认 "sdb sdc sdd"）
#
# 退出码：0 全部通过；1 有失败项；2 环境不满足（非 root / 盘不安全 / 缺工具）。
#
# 每条写断言都配一条**独立证据**（不读产品的解析结果）：
#   - 文件系统身份：blkid -p（产品按 CONTRACTS §6 不调用 blkid，所以它是另一个证人）；
#   - 容量与块大小：lsblk -bno SIZE（内核属性）与 blkid 的 BLOCK_SIZE；
#   - 真的写了盘：分区首 4 MiB 的 sha256 在格式化前后不同；
#   - 固定参数：任务 result.data.command 的 argv 逐项断言（ext4 无 -F，XFS 固定带 -f）；
#   - 取消与中断：SIGSTOP 冻住代理正在跑的 mkfs，从 /proc 读到的**真实进程**，
#     不是脚本自己 fork 出来的替身。
#
# 明确不覆盖（NOT RUN，原因写在这里，不在脚本里假装通过）：
#   - **DEVICE_IDENTITY_CHANGED**（分区身份那一路）：要让内核视图与盘上条目在
#     “提交前的一瞬”不一致，需要真实竞态；脚本改盘之后状态版本先变，闸门落在
#     PREFLIGHT_STALE 上。单元测试 TestFormatRefusesPartitionThatMoved 覆盖另一路。
#   - **磁盘写满 / ENOSPC**：VM 的盘是固定的，制造 ENOSPC 会污染后续用例，
#     而这条错误路径与 COMMAND_FAILED 同一条（describeFormatFailure）。
#   - **4Kn 扇区盘**：VM 拿不到 4Kn 设备（checkWritableSectors 的拒绝路径由单测覆盖）。
#   - 挂载 / fstab 相关的格式化后动作：P07.4。

set -u

BIN_DIR=${BIN_DIR:-/tmp/upanel-bin}

if [ "$(id -u)" -ne 0 ]; then
  printf '本脚本必须以 root 运行：sudo BIN_DIR=%s sh %s\n' "$BIN_DIR" "$0" >&2
  exit 2
fi

AGENT="$BIN_DIR/upanel-agent"
WEB="$BIN_DIR/upanel-web"
CTL="$BIN_DIR/upanelctl"

WORK=${WORK:-/var/tmp/upanel-accept-p07-3}
PAYLOADS=$WORK/payloads
RUN_DIR=/run/upanel
SOCK=$RUN_DIR/agent.sock
SYS_CFG=/etc/upanel/upanel.json
AGENT_DB_DIR=/var/lib/upanel-agent
AGENT_DB=$AGENT_DB_DIR/agent.db
KEY_FILE=/etc/upanel/agent.key
WEB_DB_DIR=$WORK/webdata
WEB_DB=$WEB_DB_DIR/upanel.db
PORT=${PORT:-19473}
BASE="http://127.0.0.1:$PORT"
ORIGIN="$BASE"
WEB_USER=upanel-web
PASSWORD='vm-correct-horse-9'

SPARE_DISKS=${SPARE_DISKS:-"sdb sdc sdd"}
STORE=/api/v1/storage
PREFLIGHT_PATH=/api/v1/operations/preflight
JOBS=/api/v1/jobs
AGENT_LOG=$WORK/logs/agent.log
WEB_LOG=$WORK/logs/web.log
AGENT_PID=""
WEB_PID=""
FROZEN_PID=""

# 格式化夹具的尺寸。XFS 实测要求 “Filesystem must be larger than 300MB”，
# 所以主用例给 1 GiB，错误退出用例故意给 64 MiB（预检允许、mkfs 拒绝）。
GIB=1073741824
PART_MAIN=$((GIB))          # sdb1：ext4 / xfs 主用例
PART_SMALL=$((64 * 1048576)) # sdc1：mkfs.xfs 会以 rc=1 拒绝
PART_BIG=$((2 * GIB))       # sdd1：进程中断用例
LABEL_MAIN=data-vol
LABEL_XFS=xfsvol

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
devget() { jget "$WORK/device.json" "$1"; }
jobget() { jget "$WORK/job.json" "$1"; }

cleanup() {
  stop_freeze
  thaw_mkfs
  kill_frozen
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

reauth() {
  api "$JAR" "$CSRF" POST /api/v1/auth/reauth "{\"password\":\"$PASSWORD\"}"
}

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
print(json.dumps({"preflight_id": sys.argv[1], "confirmation_phrase": sys.argv[2]}))' \
    "$1" "${2:-}" >"$WORK/jobreq-submit.json"
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

jstatus() { jobget 'd["data"]["status"]'; }
jstep() { jobget 'd["data"]["step"]'; }
joutcome() { jobget 'd["data"]["result"]["outcome"]'; }
jverified() { jobget 'bool(d["data"]["result"].get("verified_at"))'; }
jerr() { jobget 'd["data"]["error"]["code"]'; }
jerrmsg() { jobget 'd["data"]["error"]["message"]'; }
jobobserved() { jobget "d[\"data\"][\"result\"][\"observed_state\"][\"$1\"]"; }
jobdata() { jobget "d[\"data\"][\"result\"][\"data\"].get(\"$1\")"; }
jobargv() { jobget '" ".join(d["data"]["result"]["data"].get("command") or [])'; }

pallowed() { pget 'd["data"]["allowed"]'; }
pall() { pget '",".join(b["code"] for b in d["data"]["blockers"])'; }
pwhy() { pget '"; ".join(b["message"] for b in d["data"]["blockers"])'; }
pimpact() { pget '",".join(i["kind"] for i in d["data"]["impacts"])'; }
plocks() { pget '",".join(d["data"]["locks"])'; }
pstate() { pget 'd["data"]["target"]["display_name"]'; }
pfp() { pget 'd["data"]["target"]["identity_fingerprint"]'; }
pphrase() { pget 'd["data"]["confirmation_phrase"]'; }
preauth() { pget 'd["data"]["requires_reauth"]'; }
pwarncodes() { pget '",".join(w["code"] for w in d["data"]["warnings"])'; }

# device <设备 id>：把设备详情存成 $WORK/device.json。
device() { api "$JAR" "$CSRF" GET "$STORE/devices/$1" >/dev/null; cp "$WORK/resp.json" "$WORK/device.json"; }

# job_count：任务总数（分页上限内）。
job_count() { api "$JAR" "$CSRF" GET "/api/v1/jobs?limit=200" >/dev/null; resp_get 'len(d["data"]["jobs"])'; }

# disk_jobs_succeeded <设备 id>：任务列表里“格式化这台设备并且成功”的条数。
# 用来证明“中断之后没有自动重跑”——如果代理偷偷重跑了一次，这里会变成 1。
disk_jobs_succeeded() {
  api "$JAR" "$CSRF" GET "/api/v1/jobs?limit=200" >/dev/null
  cp "$WORK/resp.json" "$WORK/jobs.json"
  python3 -c 'import json,sys
try:
    d=json.load(open(sys.argv[1],encoding="utf-8"))
except Exception:
    print(-1); raise SystemExit
dev=sys.argv[2]
jobs=(d.get("data") or {}).get("jobs") or []
print(len([j for j in jobs
           if j.get("status")=="succeeded"
           and (((j.get("result") or {}).get("data") or {}).get("device_id")==dev)]))' \
    "$WORK/jobs.json" "$1"
}

# 快照新鲜度：拓扑每 5 秒重扫，写完盘立刻读设备详情会拿到写之前的快照。
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
# 而拓扑采集间隔是 5 秒。刚用 sgdisk / wipefs 改过盘就立刻预检，很可能绑到改之前的
# 快照；提交时第二道门发现版本变了，会判 PREFLIGHT_STALE。那是**正确的拒绝**，
# 但不是这些用例要验的东西——要先让快照跟上现状，才谈得上“预检之后有人动了盘”。
sync_topo() { settle "$1" 'True'; }

# ---------- 独立证据（不经过产品的解析器） ----------

# blkid_field <设备> <TYPE|LABEL|UUID|BLOCK_SIZE>：-p 直接探测，绕开 blkid 的缓存。
blkid_field() { blkid -p -o value -s "$2" "$1" 2>/dev/null | head -1; }
fs_type() { blkid_field "$1" TYPE; }
fs_uuid() { blkid_field "$1" UUID; }
fs_label() { blkid_field "$1" LABEL; }
kbytes() { lsblk -bno SIZE "$1" 2>/dev/null | tr -d ' '; }

# part_digest <分区>：分区首 4 MiB 的 sha256（文件系统身份落在这里）。
part_digest() { dd if="$1" bs=1M count=4 2>/dev/null | sha256sum | cut -d' ' -f1; }

# disk_digest <设备>：头部 4 MiB + 尾部 4 MiB 的 sha256（分区表两处都覆盖）。
disk_digest() {
  _size=$(blockdev --getsize64 "$1" 2>/dev/null || echo 0)
  _skip=$(( _size / 1048576 - 4 ))
  [ "$_skip" -lt 0 ] && _skip=0
  { dd if="$1" bs=1M count=4 2>/dev/null; dd if="$1" bs=1M skip="$_skip" count=4 2>/dev/null; } \
    | sha256sum | cut -d' ' -f1
}

wipe_disk() {
  wipefs -a "$1" >/dev/null 2>&1
  sgdisk --zap-all "$1" >/dev/null 2>&1
  dd if=/dev/zero of="$1" bs=1M count=4 conv=notrunc >/dev/null 2>&1
  _size=$(blockdev --getsize64 "$1" 2>/dev/null || echo 0)
  _skip=$(( _size / 1048576 - 4 ))
  [ "$_skip" -lt 0 ] && _skip=0
  dd if=/dev/zero of="$1" bs=1M seek="$_skip" count=4 conv=notrunc >/dev/null 2>&1
  blockdev --rereadpt "$1" >/dev/null 2>&1
  udevadm settle --timeout=5 >/dev/null 2>&1
  return 0
}

# ---------- 冻住代理正在跑的 mkfs（“禁止取消”与“进程中断”用例的窗口） ----------

# first_line <文件>：读第一行（不在紧循环里用了，所以允许 fork；只用于打印现场）。
# 曾经用 shell 内建 `read x < /proc/...` 省 fork——那是个坑，见 freeze_loop 注释第 3 条。
first_line() { head -n1 "$1" 2>/dev/null; }

# freeze_loop <输出文件>：在 /proc 里盯着代理的子进程，看到 mkfs 家族就 SIGSTOP，
# 把 pid 写进文件。mkfs 是代理自己 fork+exec 的，所以冻住的是产品在跑的那个进程，
# 不是脚本造的替身。
#
# 为什么用 Python 而不是 shell 循环：mkfs.ext4 在 1 GiB 上只要 43—53 ms（真机实测，
# 见 docs/evidence/P07/2026-10-22-p07.3/measurements.txt），而 dash 每轮循环至少
# fork 一次 cat，实测一轮 20—40 ms——窗口窄的时候整轮都能漏掉（第 4 轮就是这样：
# xfs 2 GiB 抓到了，ext4 1 GiB 没抓到，那条用例记了 NOT RUN）。Python 轮询一轮
# 几十微秒，稳定落在窗口内。
#
# 三个踩过的坑：
# 1) 只读 /proc/<pid>/task/<主线程>/children 抓不到：子进程挂在**发起 exec 的那条
#    线程**的 children 里（真机实测：代理主线程的 children 是空的）。所以把所有
#    线程的 children 都读一遍。
# 2) 不能用 [ -s 文件 ] 判空：procfs 的 st_size 恒为 0，这个测试对任何 /proc 文件
#    都为假——循环会“跑得很快但永远看不见东西”。
# 3) 不能用 read x < /proc/...：read 在 dash 里是特殊内建，线程退出导致重定向失败
#    会直接把整个 shell 打死（表现就是“怎么都冻不住”）。
# 4) 不能“看过一个 pid 就不再看了”：fork 与 exec 之间 comm 还是父进程的名字，
#    采样撞进这个缝里就会把 mkfs 判成“不是它”，而且永不复查（第 12 轮漏因，见下）。
freeze_loop() {
  python3 - "$AGENT_PID" "$1" <<'PY'
import glob
import os
import signal
import sys
import time

agent, out = int(sys.argv[1]), sys.argv[2]
# 先把自己的 pid 报给父 shell：父 shell 据此确认“导入做完了、循环已经起来了”再提交。
# 冷启动要几十毫秒，而 mkfs.ext4 只要 43—53 ms——不等这一下，提交得早的那几轮会在
# 解释器还在 import 的时候就把 mkfs 跑完了（第 4、6 轮各漏一次）。
with open(out + ".watch", "w", encoding="ascii") as fh:
    fh.write(str(os.getpid()))
want = {"mkfs.ext4", "mkfs.xfs", "mke2fs"}
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
        # 每个 pid 第一次露面时记一行（pid + 当时的 comm），只用于事后诊断，不参与判断。
        if kid not in traced:
            traced.add(kid)
            try:
                with open(out + ".trace", "a", encoding="ascii") as fh:
                    fh.write("%s %s\n" % (kid, name))
            except OSError:
                pass
        # 名字不对就**下一轮继续看这个 pid**，不能记成“看过了”就再也不看。
        #
        # 这是第 12 轮（run8）的漏因：fork 与 exec 之间 comm 还是父进程的名字
        # （upanel-agent），而 0.5 ms 一次的采样有相当大的概率正好落在这个几十微秒的
        # 缝里。旧的 `seen` 集合把那次采样当成定论，那个 pid 从此被永久跳过——于是
        # 2 GiB 的 mkfs.xfs（窗口比 1 GiB 的 ext4 还宽）漏了，而更短的 mkfs.ext4 抓到了。
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
# start_freeze <pid 文件>：起一个盯着代理子进程的看门狗，**等它真的热起来**才返回。
# 热起来的标志是它自己写下的 `*.watch` 文件（导入已完成、轮询就绪）。
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
# stop_freeze：收掉看门狗。必须按**它自己报的 pid** 杀：`cmd &` 的 `$!` 只是外层子
# shell，杀掉它之后 python 会变成孤儿继续轮询——第 6 轮就漏了一个在后台，把 §9 那次
# mkfs 冻住了：§8 记 NOT RUN，§9 的任务卡在那里再也没结束。
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

# wait_frozen <文件>：等冻住的结果，回显 pid（失败回显空串）。
# 窗口给到 60 秒：从受理到真正 exec mkfs 之间还有复核、加锁、读旧超级块几步，
# 10 秒太紧（第 6 轮就是在窗口关掉之后才把 mkfs 冻上，任务于是挂在那里）。
wait_frozen() {
  _i=0
  while [ "$_i" -lt 1200 ]; do
    if [ -s "$1" ]; then tr -d ' \n' <"$1"; return 0; fi
    _i=$((_i + 1))
    sleep 0.05
  done
  return 1
}

thaw_mkfs() {
  if [ -n "$FROZEN_PID" ] && kill -0 "$FROZEN_PID" 2>/dev/null; then
    kill -CONT "$FROZEN_PID" 2>/dev/null
  fi
  FROZEN_PID=""
  return 0
}

kill_frozen() {
  if [ -n "$FROZEN_PID" ] && kill -0 "$FROZEN_PID" 2>/dev/null; then
    kill -CONT "$FROZEN_PID" 2>/dev/null
    kill -KILL "$FROZEN_PID" 2>/dev/null
  fi
  FROZEN_PID=""
  return 0
}

echo "== 0. 环境与安全守卫 =="
for b in "$AGENT" "$WEB" "$CTL"; do
  if [ -x "$b" ]; then pass "二进制存在且可执行 $(basename "$b")"; else fail "二进制存在且可执行 $(basename "$b")" "$b"; fi
done
for t in python3 curl runuser blockdev sha256sum findmnt lsblk sgdisk blkid \
         wipefs udevadm dd ss; do
  if command -v "$t" >/dev/null 2>&1; then pass "依赖命令可用 $t"; else fail "依赖命令可用 $t" "缺少 $t"; fi
done
# 代理里写死的就是这四个路径（storage.DefaultSfdiskPath / DefaultUdevadmPath /
# DefaultMkfsExt4Path / DefaultMkfsXFS）。断言的是**代理实际会 exec 的那个文件**。
for p in /usr/sbin/sfdisk /usr/bin/udevadm /usr/sbin/mkfs.ext4 /usr/sbin/mkfs.xfs; do
  if [ -x "$p" ]; then pass "写原语依赖存在且可执行 $p"; else fail "写原语依赖存在且可执行 $p" "不存在或不可执行"; fi
done
note "sfdisk: $(/usr/sbin/sfdisk --version 2>&1 | head -1)"
note "mkfs.ext4: $(/usr/sbin/mkfs.ext4 -V 2>&1 | head -1)"
note "mkfs.xfs: $(/usr/sbin/mkfs.xfs -V 2>&1 | head -1)"

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
BLANK_C=$(printf '%s\n' $SPARE_DISKS | sed -n 3p)
if [ -z "$BLANK_A" ] || [ -z "$BLANK_B" ] || [ -z "$BLANK_C" ]; then
  echo "SPARE_DISKS 需要三块盘（当前 '$SPARE_DISKS'）" >&2
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
  if [ -e "/dev/$d" ]; then pass "一次性盘 /dev/$d 存在"; else fail "一次性盘 /dev/$d 存在" "不存在"; exit 1; fi
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
  if [ "$_size" -lt "$PART_BIG" ]; then
    echo "/dev/$d 容量 ${_size} 字节，放不下 ${PART_BIG} 字节的夹具分区" >&2
    exit 2
  fi
  note "/dev/$d：$_size 字节，无挂载、无 swap"
  wipe_disk "/dev/$d"
done
pass "三块一次性盘全部清成空白（wipefs + zap-all + 首尾 4 MiB 归零）"

rm -rf "$WORK"
mkdir -p "$WORK/logs" "$PAYLOADS" "$WEB_DB_DIR"
chmod 0755 "$WORK" "$PAYLOADS" "$WORK/logs"
chown -R "$WEB_USER" "$WEB_DB_DIR"
uname -r >"$WORK/logs/kernel.txt" 2>/dev/null || true
lsblk -o NAME,TYPE,SIZE,FSTYPE,MOUNTPOINTS >"$WORK/logs/lsblk-initial.txt" 2>&1

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
echo "== 2. 能力：格式化现在是可执行动作 =="
for a in storage.partition_table.initialize storage.partition.create storage.partition.delete \
         storage.filesystem.format; do
  expect_eq "capabilities 里有 $a" "True" \
    "$(jget "$PAYLOADS/agent-capabilities.json" "\"$a\" in d[\"data\"][\"supported_actions\"]")"
done
note "代理登记的动作数：$(jget "$PAYLOADS/agent-capabilities.json" 'len(d["data"]["supported_actions"])')"

echo
echo "== 3. 夹具：GPT + 三个分区（用 sgdisk，不占用产品动作） =="
# 夹具用 sgdisk 造而不是走产品的 create 动作：create 已在 p07-2 验过，这里要验的是
# 格式化。分区起点都是 1 MiB（2048 扇区）。
sgdisk --new=1:2048:+"$((PART_MAIN / 1048576))"MiB "/dev/$BLANK_A" >/dev/null 2>&1
sgdisk --new=1:2048:+"$((PART_SMALL / 1048576))"MiB "/dev/$BLANK_B" >/dev/null 2>&1
sgdisk --new=1:2048:+"$((PART_BIG / 1048576))"MiB "/dev/$BLANK_C" >/dev/null 2>&1
partprobe "/dev/$BLANK_A" "/dev/$BLANK_B" "/dev/$BLANK_C" 2>/dev/null
udevadm settle --timeout=10 >/dev/null 2>&1
for p in "${BLANK_A}1" "${BLANK_B}1" "${BLANK_C}1"; do
  if [ -b "/dev/$p" ]; then pass "夹具分区 /dev/$p 存在"; else fail "夹具分区 /dev/$p 存在" "不存在"; fi
done
expect_eq "夹具：${BLANK_A}1 的内核大小" "$PART_MAIN" "$(kbytes "/dev/${BLANK_A}1")"
expect_eq "夹具：${BLANK_B}1 的内核大小" "$PART_SMALL" "$(kbytes "/dev/${BLANK_B}1")"
expect_eq "夹具：${BLANK_C}1 的内核大小" "$PART_BIG" "$(kbytes "/dev/${BLANK_C}1")"
expect_eq "夹具：${BLANK_A}1 上还没有文件系统" "" "$(fs_type "/dev/${BLANK_A}1")"
# 夹具刚造好，先等采集器重扫一次：否则预检会绑到“还没有这三个分区”的快照。
sync_topo "$BLANK_A"
sync_topo "$BLANK_B"
sync_topo "$BLANK_C"

echo
echo "== 4. ext4 格式化：标签、两源核验、独立工具复核 =="
DIGEST_BEFORE=$(part_digest "/dev/${BLANK_A}1")
expect_eq "ext4 预检状态码" "200" \
  "$(preflight storage.filesystem.format \
     "{\"device_id\":\"${BLANK_A}1\",\"filesystem\":\"ext4\",\"label\":\"$LABEL_MAIN\"}")"
cp "$WORK/preflight.json" "$PAYLOADS/preflight-format-ext4.json"
expect_eq "ext4 预检允许" "True" "$(pallowed)"
# 锁必须是**整盘**：格式化 sdb1 期间不能有人在同一块盘上建分区。
expect_eq "格式化的锁是整盘" "disk:$BLANK_A" "$(plocks)"
expect_eq "格式化要求再认证" "True" "$(preauth)"
expect_contains "短语首词是 FORMAT" "FORMAT " "$(pphrase)"
expect_contains "影响里有数据丢失" "data_loss" "$(pimpact)"
note "目标：$(pstate)"
PF_E=$(pget 'd["data"]["preflight_id"]')
PH_E=$(pphrase)

echo
echo "== 5. 确认短语：错的短语被拒，且这份预检就此作废 =="
JOBS_BEFORE_PHRASE=$(job_count)
# 先再认证：提交这条路上，再认证窗口是**先查**的一道门（Web 侧先看 reauth_until，
# 再转发给代理比短语），所以没有再认证时看到的会是 403 REAUTH_REQUIRED，连短语都轮不到比。
# 契约没有规定这两道门的先后；脚本按实现的顺序走，并把这个顺序当事实记下来。
expect_eq "再认证" "200" "$(reauth)"
expect_eq "错短语提交被拒（400）" "submit:400" "$(submit_and_wait "$PF_E" "FORMAT WRONG1" "k-badphrase")"
cp "$WORK/submit.json" "$PAYLOADS/submit-bad-phrase.json"
expect_eq "错短语的错误码" "CONFIRMATION_MISMATCH" \
  "$(jget "$WORK/submit.json" 'd["error"]["code"]')"
expect_eq "错短语留下一条被拒记录（不是静默丢弃）" "$((JOBS_BEFORE_PHRASE + 1))" "$(job_count)"
expect_eq "被拒的那条停在 failed/rejected" "True" \
  "$(resp_get 'any(j["status"]=="failed" and j["step"]=="rejected" and j["error"].get("code")=="CONFIRMATION_MISMATCH" for j in d["data"]["jobs"])')"
expect_eq "错短语之后分区上还是空白（一个字节都没写）" "$DIGEST_BEFORE" \
  "$(part_digest "/dev/${BLANK_A}1")"
# 这份预检已经被这次提交用掉了：Web 先把预检标记为已消费、落一条 queued 任务，**然后**
# 才把短语交给代理比对。所以错短语的代价是“预检作废、重新预检”，而不是“可以无限试短语”。
# 这一条顺带证明“一次预检只能提交一次”在真实链路上成立。
expect_eq "同一份预检不能再用（哪怕短语是对的）" "submit:409" \
  "$(submit_and_wait "$PF_E" "$PH_E" "k-format-ext4-replay")"
expect_contains "再用时被判预检已作废" "PREFLIGHT_STALE" \
  "$(jget "$WORK/submit.json" 'd["error"]["code"]')"
cp "$WORK/submit.json" "$PAYLOADS/submit-replay-preflight.json"
expect_eq "作废之后分区上还是空白" "$DIGEST_BEFORE" "$(part_digest "/dev/${BLANK_A}1")"

# 重新预检，用正确的短语正式做一次 ext4。
expect_eq "重新预检（ext4）" "200" \
  "$(preflight storage.filesystem.format \
     "{\"device_id\":\"${BLANK_A}1\",\"filesystem\":\"ext4\",\"label\":\"$LABEL_MAIN\"}")"
cp "$WORK/preflight.json" "$PAYLOADS/preflight-format-ext4-2.json"
expect_eq "重新预检允许" "True" "$(pallowed)"
PF_E=$(pget 'd["data"]["preflight_id"]')
PH_E=$(pphrase)
expect_eq "再认证" "200" "$(reauth)"
expect_eq "ext4 任务终态" "succeeded" "$(submit_and_wait "$PF_E" "$PH_E" "k-format-ext4")"
cp "$WORK/job.json" "$PAYLOADS/job-format-ext4.json"
expect_eq "任务状态 succeeded" "succeeded" "$(jstatus)"
# 退出码 0 不算验证：写任务的 result.outcome 必须是 verified 并带 verified_at。
expect_eq "结果是 verified" "verified" "$(joutcome)"
expect_eq "结果带 verified_at" "True" "$(jverified)"
expect_eq "observed_state 的文件系统" "ext4" "$(jobobserved filesystem)"
expect_eq "observed_state 的标签" "$LABEL_MAIN" "$(jobobserved label)"
expect_eq "observed_state 的设备 id" "${BLANK_A}1" "$(jobobserved device_id)"
expect_eq "observed_state 说 udev 已稳定" "True" "$(jobobserved udev_settled)"
expect_eq "observed_state 说节点在" "True" "$(jobobserved node_present)"
expect_eq "observed_state 的签名分类器结论" "ext" "$(jobobserved signature)"
expect_eq "observed_state 的块大小" "4096" "$(jobobserved block_size_bytes)"
expect_eq "observed_state 的容量等于分区大小" "$PART_MAIN" "$(jobobserved size_bytes)"
expect_eq "observed_state 的分区大小" "$PART_MAIN" "$(jobobserved partition_size_bytes)"
expect_eq "核验走了两条读路径" "['superblock', 'signature_probe']" "$(jobdata checked_via)"
expect_eq "data 里的文件系统" "ext4" "$(jobdata filesystem)"
expect_eq "data 里的标签" "$LABEL_MAIN" "$(jobdata label)"
expect_eq "data 里的 previous_kind 是空（原来没有文件系统）" "" "$(jobdata previous_kind)"
# 固定参数：ext4 **不带** -F（实测不需要，而且 -F 也解不开 mkfs 自己的“已挂载”检查）。
expect_eq "argv 里的程序是 /usr/sbin/mkfs.ext4" "/usr/sbin/mkfs.ext4" \
  "$(jobget 'd["data"]["result"]["data"]["command"][0]')"
expect_eq "argv 里没有 -F" "False" \
  "$(jobget '"-F" in (d["data"]["result"]["data"]["command"] or [])')"
expect_eq "argv 里的标签是单独一项" "$LABEL_MAIN" \
  "$(jobget 'd["data"]["result"]["data"]["command"][2]')"
note "实际 argv：$(jobargv)"
UUID_EXT4=$(jobobserved uuid)

# 独立工具复核：产品按 CONTRACTS §6 不调用 blkid，所以 blkid 是另一个证人。
expect_eq "blkid 认出的类型是 ext4" "ext4" "$(fs_type "/dev/${BLANK_A}1")"
expect_eq "blkid 读到的标签" "$LABEL_MAIN" "$(fs_label "/dev/${BLANK_A}1")"
expect_eq "blkid 读到的 UUID 与任务观察值一致" "$UUID_EXT4" "$(fs_uuid "/dev/${BLANK_A}1")"
expect_eq "blkid 读到的块大小" "4096" "$(blkid_field "/dev/${BLANK_A}1" BLOCK_SIZE)"
expect_ne "格式化真的写了盘（分区首 4 MiB 哈希变了）" "$DIGEST_BEFORE" \
  "$(part_digest "/dev/${BLANK_A}1")"

echo
echo "== 6. 同一个分区改成 XFS：UUID 必须换，-f 在 argv 里 =="
DIGEST_BEFORE_XFS=$(part_digest "/dev/${BLANK_A}1")
expect_eq "xfs 预检状态码" "200" \
  "$(preflight storage.filesystem.format \
     "{\"device_id\":\"${BLANK_A}1\",\"filesystem\":\"xfs\",\"label\":\"$LABEL_XFS\"}")"
cp "$WORK/preflight.json" "$PAYLOADS/preflight-format-xfs.json"
expect_eq "xfs 预检允许" "True" "$(pallowed)"
# 已经有 ext4 数据：预检必须说清“这块数据会没”。
expect_contains "xfs 预检的告警里有 EXISTING_DATA_WILL_BE_LOST" "EXISTING_DATA_WILL_BE_LOST" \
  "$(pwarncodes)"
PF_X=$(pget 'd["data"]["preflight_id"]')
PH_X=$(pphrase)
expect_eq "再认证" "200" "$(reauth)"
expect_eq "xfs 任务终态" "succeeded" "$(submit_and_wait "$PF_X" "$PH_X" "k-format-xfs")"
cp "$WORK/job.json" "$PAYLOADS/job-format-xfs.json"
expect_eq "结果是 verified" "verified" "$(joutcome)"
expect_eq "observed_state 的文件系统" "xfs" "$(jobobserved filesystem)"
expect_eq "observed_state 的标签" "$LABEL_XFS" "$(jobobserved label)"
expect_eq "observed_state 的签名分类器结论" "xfs" "$(jobobserved signature)"
expect_eq "data 里的 previous_kind 是 ext4" "ext4" "$(jobdata previous_kind)"
expect_eq "data 里的 previous_uuid 是上一份 UUID" "$UUID_EXT4" "$(jobdata previous_uuid)"
# -f 是固定参数集合的一部分：不带它 mkfs.xfs 在任何有签名的分区上都以 rc=1 结束。
expect_eq "argv 里的程序是 /usr/sbin/mkfs.xfs" "/usr/sbin/mkfs.xfs" \
  "$(jobget 'd["data"]["result"]["data"]["command"][0]')"
expect_eq "argv 里带 -f" "True" "$(jobget '"-f" in (d["data"]["result"]["data"]["command"] or [])')"
note "实际 argv：$(jobargv)"
UUID_XFS=$(jobobserved uuid)
expect_ne "文件系统 UUID 换了（不是“命令说成功、盘没变”）" "$UUID_EXT4" "$UUID_XFS"
expect_eq "blkid 认出的类型是 xfs" "xfs" "$(fs_type "/dev/${BLANK_A}1")"
expect_eq "blkid 读到的标签" "$LABEL_XFS" "$(fs_label "/dev/${BLANK_A}1")"
expect_eq "blkid 读到的 UUID 与任务观察值一致" "$UUID_XFS" "$(fs_uuid "/dev/${BLANK_A}1")"
expect_ne "改成 xfs 真的写了盘" "$DIGEST_BEFORE_XFS" "$(part_digest "/dev/${BLANK_A}1")"

echo
echo "== 7. 标签上限：超限在**预检之前**就被拒（400），既不落任务也不碰盘 =="
# ext4 的标签上限是 16 字节，XFS 是 12。17 字节尤其危险：实测 mkfs.ext4 打一行
# Warning、**截断**并 rc=0——只看退出码就会把半个标签当成功。
#
# 闸门位置：代理在解码参数时就按文件系统校验标签（storage.ValidateLabel），
# 所以超限的请求连预检都不生成，得到 400 VALIDATION_ERROR，消息里带真实上限。
# 这是**比预检更早**的一道门：一个物理上不存在的标签不是“业务条件不允许”，
# 是参数错。assess 里另有一份 LABEL_INVALID 阻塞（纵深防御），由单测
# TestFormatRejectsLabelOverLimit 覆盖；formatCommand 里还有第三道 ErrUnsafeWrite。
LONG_E=0123456789abcdefg   # 17 字节
LONG_X=0123456789abcde     # 13 字节
JOBS_BEFORE_LABEL=$(job_count)
expect_eq "ext4 17 字节标签的预检被拒（400）" "400" \
  "$(preflight storage.filesystem.format \
     "{\"device_id\":\"${BLANK_A}1\",\"filesystem\":\"ext4\",\"label\":\"$LONG_E\"}")"
cp "$WORK/preflight.json" "$PAYLOADS/preflight-label-too-long-ext4.json"
expect_eq "ext4 超限的错误码是 VALIDATION_ERROR" "VALIDATION_ERROR" \
  "$(jget "$WORK/preflight.json" 'd["error"]["code"]')"
expect_contains "理由里说明了 ext4 的字节上限" "16" \
  "$(jget "$WORK/preflight.json" 'd["error"]["message"]')"
expect_contains "理由里说了标签" "标签" \
  "$(jget "$WORK/preflight.json" 'd["error"]["message"]')"
# 没有预检就没有 preflight_id：拿空串去提交只会得到 404（预检不存在），
# 所以这里直接断言“没有生成预检”，而不是假装还能往下提交。
expect_eq "超限的请求没有生成预检" "False" \
  "$(jget "$WORK/preflight.json" '"preflight_id" in (d.get("data") or {})')"

expect_eq "xfs 13 字节标签的预检被拒（400）" "400" \
  "$(preflight storage.filesystem.format \
     "{\"device_id\":\"${BLANK_A}1\",\"filesystem\":\"xfs\",\"label\":\"$LONG_X\"}")"
cp "$WORK/preflight.json" "$PAYLOADS/preflight-label-too-long-xfs.json"
expect_eq "xfs 超限的错误码是 VALIDATION_ERROR" "VALIDATION_ERROR" \
  "$(jget "$WORK/preflight.json" 'd["error"]["code"]')"
expect_contains "理由里说明了 xfs 的字节上限" "12" \
  "$(jget "$WORK/preflight.json" 'd["error"]["message"]')"
expect_eq "超限的请求没有生成预检（xfs）" "False" \
  "$(jget "$WORK/preflight.json" '"preflight_id" in (d.get("data") or {})')"
expect_eq "两次被拒都没有留下任务" "$JOBS_BEFORE_LABEL" "$(job_count)"
expect_eq "两次被拒之后分区上的文件系统没变" "xfs" "$(fs_type "/dev/${BLANK_A}1")"
expect_eq "UUID 也没变" "$UUID_XFS" "$(fs_uuid "/dev/${BLANK_A}1")"

echo
echo "== 8. 禁止取消：mkfs 跑着时取消 → 409，任务不被标成 canceled =="
# mkfs 只有几十到几百毫秒，正常轮询抢不到窗口。这里在 /proc 里盯代理的**直接子进程**，
# 一旦看到 mkfs 就 SIGSTOP 冻住它——冻住的是产品真的在跑的那个进程。
sync_topo "$BLANK_A"
# 最多试 3 次：每次都用**新的预检**提交一个格式化任务，同时在 /proc 里盯代理的直接子进程，
# 看到 mkfs 就 SIGSTOP 冻住它。没冻住的那一次让任务正常跑完，再试下一次。
#
# 为什么允许重试：验收要证明的是“mkfs 跑着的时候取消被拒、任务不被标成 canceled”，
# 而不是“测试脚本一次就抓住几十毫秒的窗口”。窗口是测试自己的竞态，失败的那次不产生
# 任何产品结论（它既没证明能取消，也没证明不能）。断言只在最后一次上做，条数因此稳定。
FROZEN_PID=""
CODE_C=""
JOB_C=""
PF_C=""
PRE_C=""
REAUTH_C=""
_attempt=0
while [ "$_attempt" -lt 3 ] && [ -z "$FROZEN_PID" ]; do
  _attempt=$((_attempt + 1))
  PRE_C=$(preflight storage.filesystem.format \
    "{\"device_id\":\"${BLANK_A}1\",\"filesystem\":\"ext4\",\"label\":\"$LABEL_MAIN\"}")
  PF_C=$(pget 'd["data"]["preflight_id"]')
  PH_C=$(pphrase)
  REAUTH_C=$(reauth)
  if ! start_freeze "$WORK/frozen.pid"; then
    fail "看门狗就绪" "第 $_attempt 次：Python 没在 3 秒内报就绪（见 frozen.pid.watch）"
    break
  fi
  CODE_C=$(submit_job "$PF_C" "$PH_C" "k-cancel-format-$_attempt")
  JOB_C=$(jget "$WORK/submit.json" 'd["data"]["job_id"]')
  FROZEN_PID=$(wait_frozen "$WORK/frozen.pid")
  stop_freeze
  if [ -z "$FROZEN_PID" ] || ! kill -0 "$FROZEN_PID" 2>/dev/null; then
    FROZEN_PID=""
    # 没抓住：让这次任务跑完（它会 succeeded），然后用新的预检重试。
    [ -n "$JOB_C" ] && wait_job "$JOB_C" >/dev/null
    note "第 $_attempt 次没冻住 mkfs（诊断：$(tr '\n' ';' <"$WORK/frozen.pid.trace" 2>/dev/null)）"
  fi
done
expect_eq "ext4 预检（取消用例）" "200" "$PRE_C"
expect_eq "再认证" "200" "$REAUTH_C"
expect_eq "提交被受理（202）" "202" "$CODE_C"
if [ -n "$FROZEN_PID" ] && kill -0 "$FROZEN_PID" 2>/dev/null; then
  pass "冻住了代理正在跑的 mkfs（pid=$FROZEN_PID）"
  note "冻住时进程名：$(first_line "/proc/$FROZEN_PID/comm") $(first_line "/proc/$FROZEN_PID/stat" | cut -d' ' -f3)"
  # 任务此刻应该还在 running/mkfs。这一步只是把现场固定下来，不是断言项。
  api "$JAR" "$CSRF" GET "/api/v1/jobs/$JOB_C" >/dev/null
  note "取消前的任务状态：$(resp_get 'd["data"]["status"]') / $(resp_get 'd["data"]["step"]')"
  CANCEL_CODE=$(api "$JAR" "$CSRF" POST "/api/v1/jobs/$JOB_C/cancel")
  cp "$WORK/resp.json" "$PAYLOADS/cancel-format-refused.json"
  expect_eq "取消请求被拒（409）" "409" "$CANCEL_CODE"
  expect_eq "拒收的错误码是 VALIDATION_ERROR" "VALIDATION_ERROR" "$(resp_get 'd["error"]["code"]')"
  case "$(resp_get 'd["error"]["message"]')" in
    *不支持安全取消* | *不可取消*)
      pass "理由说明这个动作不能取消" ;;
    *)
      fail "理由说明这个动作不能取消" "$(resp_get 'd["error"]["message"]')" ;;
  esac
  # 取消被拒之后任务必须还在跑：既没有 canceled，也没有被“顺手”标成 failed。
  api "$JAR" "$CSRF" GET "/api/v1/jobs/$JOB_C" >/dev/null
  expect_eq "任务没有被标成 canceled" "False" \
    "$(resp_get 'd["data"]["status"] == "canceled"')"
  expect_eq "任务的 cancellable 是 false" "False" "$(resp_get 'bool(d["data"].get("cancellable"))')"
  # 解冻，让它跑完：这是“取消没有破坏执行”的证据。
  thaw_mkfs
  expect_eq "解冻后任务终态" "succeeded" "$(wait_job "$JOB_C")"
  cp "$WORK/job.json" "$PAYLOADS/job-format-cancel-refused.json"
  expect_eq "解冻后结果是 verified" "verified" "$(joutcome)"
  expect_eq "解冻后文件系统是 ext4" "ext4" "$(jobobserved filesystem)"
  UUID_AFTER_CANCEL=$(jobobserved uuid)
  expect_ne "解冻后 UUID 与之前那次 ext4 不同" "$UUID_EXT4" "$UUID_AFTER_CANCEL"
else
  kill_frozen
  notrun "禁止取消（mkfs 期间取消 → 409）" \
    "三次都没能在 mkfs 的运行窗口内冻住它（任务已跑完）；这是测试的竞态，不当作功能缺失，诊断见 frozen.pid.trace"
  api "$JAR" "$CSRF" GET "/api/v1/jobs/$JOB_C" >/dev/null
  note "没冻住时任务的现场：$(resp_get 'd["data"]["status"]') / $(resp_get 'd["data"]["step"]')"
  wait_job "$JOB_C" >/dev/null
fi

echo
echo "== 9. 错误退出：预检允许、mkfs 以非 0 结束 =="
# 64 MiB 的分区上 mkfs.xfs 会拒绝（实测原文 "Filesystem must be larger than 300MB."）。
# 预检只按最小分区与签名判断，因此这个目标是**合法**的——错误从命令这一层冒上来，
# 正好用来验 COMMAND_FAILED 的文案与“盘上现在是”的读回。
expect_eq "小分区 xfs 预检状态码" "200" \
  "$(preflight storage.filesystem.format \
     "{\"device_id\":\"${BLANK_B}1\",\"filesystem\":\"xfs\",\"label\":\"small\"}")"
cp "$WORK/preflight.json" "$PAYLOADS/preflight-format-small-xfs.json"
expect_eq "小分区 xfs 预检允许（错误是命令层的，不是判决层的）" "True" "$(pallowed)"
PF_S=$(pget 'd["data"]["preflight_id"]')
PH_S=$(pphrase)
DIGEST_SMALL=$(part_digest "/dev/${BLANK_B}1")
expect_eq "再认证" "200" "$(reauth)"
expect_eq "小分区 xfs 任务终态" "failed" "$(submit_and_wait "$PF_S" "$PH_S" "k-format-small-xfs")"
cp "$WORK/job.json" "$PAYLOADS/job-format-small-xfs.json"
expect_eq "任务状态是 failed" "failed" "$(jstatus)"
expect_eq "错误码是 COMMAND_FAILED" "COMMAND_FAILED" "$(jerr)"
ERRMSG=$(jerrmsg)
expect_contains "失败消息带退出码" "退出码 1" "$ERRMSG"
expect_contains "失败消息带 mkfs 的真实输出" "larger than 300MB" "$ERRMSG"
expect_contains "失败消息带“盘上现在是”（读回的实际状态）" "盘上现在是" "$ERRMSG"
expect_contains "失败消息提醒先核实再重新提交" "重新提交" "$ERRMSG"
# mkfs 拒绝时不会碰盘：分区首 4 MiB 的哈希必须一个字节都没变。
expect_eq "mkfs 失败后分区上什么都没写" "$DIGEST_SMALL" "$(part_digest "/dev/${BLANK_B}1")"
expect_eq "mkfs 失败后分区上仍然没有文件系统" "" "$(fs_type "/dev/${BLANK_B}1")"

echo
echo "== 10. 执行前二次身份检查：预检之后有人动了盘 =="
# 两道门：提交时 Web 侧查一次，执行前代理再查一次；两次都用“同一个判决函数重算、
# 逐字比较”的方式——指纹变了是 DEVICE_IDENTITY_CHANGED，版本变了是 PREFLIGHT_STALE。
# 这里先把快照收敛到现状，**再**制造变化：否则测的是“缓存还没跟上”，不是这两道门。
sync_topo "$BLANK_A"
# 10a 的起点必须是一个**确定有文件系统**的分区：前面几节的成败决定 sdb1 上现在是 ext4、
# xfs 还是空的。先按正常路径把它格式化成 ext4（顺带再验证一次写盘），拿到一个确定的
# UUID 与首 4 MiB 摘要，作为“一个字节都没动”的对照物。
expect_eq "10a 起点预检" "200" \
  "$(preflight storage.filesystem.format \
     "{\"device_id\":\"${BLANK_A}1\",\"filesystem\":\"ext4\",\"label\":\"$LABEL_MAIN\"}")"
PF_SEED=$(pget 'd["data"]["preflight_id"]')
PH_SEED=$(pphrase)
expect_eq "再认证（10a 起点）" "200" "$(reauth)"
expect_eq "10a 起点任务" "succeeded" \
  "$(submit_and_wait "$PF_SEED" "$PH_SEED" "k-seed-format")"
expect_eq "10a 起点：${BLANK_A}1 上是 ext4" "ext4" "$(fs_type "/dev/${BLANK_A}1")"
sync_topo "$BLANK_A"
UUID_SEED=$(fs_uuid "/dev/${BLANK_A}1")

# 10a：预检之后**这块盘的结构**被改了（多出来一个分区）→ 提交时复核就拒。
#
# 为什么用“新建分区”而不是“擦掉目标分区的签名”：擦签名改不动状态版本。snapshotVersion
# 只把**结构**算进指纹（分区表、挂载、身份、容量），未挂载分区的文件系统不在里面；
# 真机第 5 轮实测：wipefs 之后版本一字不变，于是“该被拒”的提交把盘格式化了（两条 FAIL）。
# 这对 format 也是合理的设计——它的工作就是覆盖掉那个签名，签名变了不构成拒绝理由。
# 真正该拒的是“盘的结构变了、这份预检已经对不上现场”（10a）与“目标不是同一块字节”（10b）。
expect_eq "预检（待被作废）" "200" \
  "$(preflight storage.filesystem.format \
     "{\"device_id\":\"${BLANK_A}1\",\"filesystem\":\"xfs\",\"label\":\"stale\"}")"
expect_eq "这条预检当时允许" "True" "$(pallowed)"
PF_STALE=$(pget 'd["data"]["preflight_id"]')
PH_STALE=$(pphrase)
note "预检绑定的身份指纹：$(pfp)"
# 状态版本不发给浏览器（它是预检的绑定字段，不是展示字段），所以用设备详情里的
# snapshot_version 做前后对比：两者来自同一份拓扑快照，是同一个东西。
device "$BLANK_A"
SV_BEFORE=$(devget 'd["data"].get("snapshot_version")')
DIGEST_STALE=$(part_digest "/dev/${BLANK_A}1")
JOBS_BEFORE_STALE=$(job_count)

# 10b：预检之后把目标分区换到别的位置（编号相同、字节不同）。
expect_eq "预检（待被换位）" "200" \
  "$(preflight storage.filesystem.format \
     "{\"device_id\":\"${BLANK_B}1\",\"filesystem\":\"ext4\",\"label\":\"moved\"}")"
PF_MOVE=$(pget 'd["data"]["preflight_id"]')
PH_MOVE=$(pphrase)

# 在**同一块盘**上新建一个 256 MiB 分区：目标分区的位置、身份、大小、文件系统全不变，
# 变的只有盘的结构。这样被拒的原因只可能是版本，不可能是身份。
sgdisk --new=2:0:+256M "/dev/$BLANK_A" >/dev/null 2>&1
partprobe "/dev/$BLANK_A" 2>/dev/null
udevadm settle --timeout=5 >/dev/null 2>&1
if [ -b "/dev/${BLANK_A}2" ]; then
  pass "夹具：${BLANK_A} 上多出一个 ${BLANK_A}2"
else
  fail "夹具：${BLANK_A} 上多出一个 ${BLANK_A}2" "新分区没有出现在内核里"
fi
# 改完要让采集器看见（见 sync_topo 的注释）：预检之后的“变化”必须进入快照，
# 版本比较才有意义。
sync_topo "$BLANK_A"
device "$BLANK_A"
SV_AFTER=$(devget 'd["data"].get("snapshot_version")')
note "新建分区之后的快照版本：$SV_AFTER（之前是 $SV_BEFORE）"
if [ -n "$SV_BEFORE" ] && [ -n "$SV_AFTER" ] && [ "$SV_BEFORE" != "$SV_AFTER" ]; then
  pass "盘的结构变了，快照版本也跟着变（预检该失效的原因）"
else
  fail "盘的结构变了，快照版本也跟着变" "之前 '$SV_BEFORE'，之后 '$SV_AFTER'"
fi
expect_eq "再认证" "200" "$(reauth)"
CODE_STALE=$(submit_and_wait "$PF_STALE" "$PH_STALE" "k-stale-format")
cp "$WORK/submit.json" "$PAYLOADS/submit-stale-format.json"
cp "$WORK/job.json" "$PAYLOADS/job-stale-format.json" 2>/dev/null
case "$CODE_STALE" in
  submit:409)
    pass "盘的结构变了之后提交被拒（409）"
    expect_contains "拒收理由指向预检过期" "PREFLIGHT_STALE" \
      "$(jget "$WORK/submit.json" 'd["error"]["code"]')"
    expect_eq "被拒的提交留下一条失败任务" "$((JOBS_BEFORE_STALE + 1))" "$(job_count)"
    ;;
  failed)
    pass "任务在执行前的复核里被判失败"
    expect_contains "失败原因指向预检过期/身份变化" "PREFLIGHT_STALE" "$(jerr)"
    ;;
  *)
    fail "盘的结构变了之后不该执行" "终态 '$CODE_STALE'"
    ;;
esac
expect_eq "被拒之后目标分区上没多写一个字节" "$DIGEST_STALE" "$(part_digest "/dev/${BLANK_A}1")"
expect_eq "被拒之后目标分区上仍然是 ext4" "ext4" "$(fs_type "/dev/${BLANK_A}1")"
expect_eq "被拒之后目标分区的 UUID 没变" "$UUID_SEED" "$(fs_uuid "/dev/${BLANK_A}1")"

# 10b：删掉分区再在同一块盘的别处建一个同号分区——编号还是 sdb1，字节不是了。
sd_start=$(sgdisk -i 1 "/dev/$BLANK_B" 2>/dev/null | sed -n 's/^First sector: \([0-9]*\).*/\1/p' | head -1)
sgdisk --delete 1 "/dev/$BLANK_B" >/dev/null 2>&1
sgdisk --new=1:$((sd_start + 2048)):+"$((PART_SMALL / 1048576))"MiB "/dev/$BLANK_B" >/dev/null 2>&1
partprobe "/dev/$BLANK_B" 2>/dev/null
udevadm settle --timeout=5 >/dev/null 2>&1
if [ -b "/dev/${BLANK_B}1" ]; then
  pass "夹具：${BLANK_B}1 换到新起点后仍然是 /dev/${BLANK_B}1"
  note "新起点扇区：$(sgdisk -i 1 "/dev/$BLANK_B" 2>/dev/null | sed -n 's/^First sector: //p' | head -1)（预检时是 ${sd_start}）"
  DIGEST_MOVED=$(part_digest "/dev/${BLANK_B}1")
  sync_topo "$BLANK_B"
  CODE_MOVE=$(submit_and_wait "$PF_MOVE" "$PH_MOVE" "k-moved-format")
  cp "$WORK/submit.json" "$PAYLOADS/submit-moved-format.json"
  cp "$WORK/job.json" "$PAYLOADS/job-moved-format.json" 2>/dev/null
  # 两条路都是“拒绝”，只是罪名不同：Web 侧先比版本可能先判 PREFLIGHT_STALE，
  # 代理手里的身份指纹先变就是 DEVICE_IDENTITY_CHANGED。断言“拒绝了、理由属于这两类”，
  # 不去猜是哪一层先开口。
  case "$CODE_MOVE" in
    submit:409)
      pass "分区被换位之后提交被拒（409）"
      MOVE_CODE=$(jget "$WORK/submit.json" 'd["error"]["code"]')
      case "$MOVE_CODE" in
        PREFLIGHT_STALE | DEVICE_IDENTITY_CHANGED)
          pass "拒收理由指向预检过期或身份变化（$MOVE_CODE）" ;;
        *) fail "拒收理由指向预检过期或身份变化" "$MOVE_CODE" ;;
      esac
      ;;
    failed)
      pass "任务在执行前的复核里被判失败"
      MOVE_CODE=$(jerr)
      case "$MOVE_CODE" in
        PREFLIGHT_STALE | DEVICE_IDENTITY_CHANGED)
          pass "失败原因指向预检过期或身份变化（$MOVE_CODE）" ;;
        *) fail "失败原因指向预检过期或身份变化" "$MOVE_CODE" ;;
      esac
      ;;
    *)
      fail "分区被换位之后不该执行" "终态 '$CODE_MOVE'"
      ;;
  esac
  expect_eq "被拒之后新位置的分区上没多写一个字节" "$DIGEST_MOVED" \
    "$(part_digest "/dev/${BLANK_B}1")"
  expect_eq "被拒之后新位置的分区上仍然没有文件系统" "" "$(fs_type "/dev/${BLANK_B}1")"
else
  notrun "分区换位（PREFLIGHT_STALE / DEVICE_IDENTITY_CHANGED）" "换位之后 /dev/${BLANK_B}1 没回来"
fi
notrun "DEVICE_IDENTITY_CHANGED 的内核/盘不一致那一路" \
  "要在提交前的一瞬制造内核与盘上条目的不一致，属真实竞态；单测 TestFormatRefusesPartitionThatMoved 覆盖"

echo
echo "== 11. 进程中断：mkfs 跑着时代理被 SIGKILL =="
sync_topo "$BLANK_C"
# 与 §8 同样的重试：没抓住窗口的那一次让任务跑完，再用新的预检试一次（最多 3 次）。
FROZEN_PID=""
JOB_K=""
PF_K=""
PRE_K=""
REAUTH_K=""
CODE_K=""
_attempt=0
while [ "$_attempt" -lt 3 ] && [ -z "$FROZEN_PID" ]; do
  _attempt=$((_attempt + 1))
  PRE_K=$(preflight storage.filesystem.format \
    "{\"device_id\":\"${BLANK_C}1\",\"filesystem\":\"xfs\",\"label\":\"interrupt\"}")
  PF_K=$(pget 'd["data"]["preflight_id"]')
  PH_K=$(pphrase)
  REAUTH_K=$(reauth)
  if ! start_freeze "$WORK/frozen2.pid"; then
    fail "看门狗就绪（中断用例）" "第 $_attempt 次：Python 没在 3 秒内报就绪"
    break
  fi
  CODE_K=$(submit_job "$PF_K" "$PH_K" "k-interrupt-format-$_attempt")
  JOB_K=$(jget "$WORK/submit.json" 'd["data"]["job_id"]')
  FROZEN_PID=$(wait_frozen "$WORK/frozen2.pid")
  stop_freeze
  if [ -z "$FROZEN_PID" ] || ! kill -0 "$FROZEN_PID" 2>/dev/null; then
    FROZEN_PID=""
    [ -n "$JOB_K" ] && wait_job "$JOB_K" >/dev/null
    note "第 $_attempt 次没冻住 mkfs（诊断：$(tr '\n' ';' <"$WORK/frozen2.pid.trace" 2>/dev/null)）"
  fi
done
expect_eq "中断用例的预检" "200" "$PRE_K"
expect_eq "再认证" "200" "$REAUTH_K"
expect_eq "提交被受理（202）" "202" "$CODE_K"
JOBS_BEFORE_KILL=$(job_count)
if [ -n "$FROZEN_PID" ] && kill -0 "$FROZEN_PID" 2>/dev/null; then
  pass "冻住了代理正在跑的 mkfs（pid=$FROZEN_PID）"
  # 代理被 SIGKILL：没有机会写终态，磁盘写到哪一步没人知道。
  kill -KILL "$AGENT_PID" 2>/dev/null
  wait "$AGENT_PID" 2>/dev/null
  AGENT_PID=""
  rm -f "$SOCK"
  pass "代理被 SIGKILL（模拟进程中断）"
  sleep 0.5
  # mkfs 是代理的子进程，代理死了它不会自己退出（还冻着）：先收掉，再重启代理。
  kill_frozen
  if start_agent; then pass "代理重启并重新创建 socket"; else fail "代理重启" "$(tail -5 "$AGENT_LOG" 2>/dev/null)"; fi
  # 重启时的 reconcile 把遗留的非终态操作标成 interrupted，**不重跑**。
  _i=0
  _st=""
  while [ "$_i" -lt 100 ]; do
    api "$JAR" "$CSRF" GET "/api/v1/jobs/$JOB_K" >/dev/null
    cp "$WORK/resp.json" "$WORK/job.json"
    _st=$(jstatus)
    case "$_st" in succeeded|failed|canceled|interrupted) break ;; esac
    _i=$((_i + 1))
    sleep 0.2
  done
  cp "$WORK/job.json" "$PAYLOADS/job-format-interrupted.json"
  expect_eq "任务终态是 interrupted" "interrupted" "$_st"
  expect_eq "错误码是 OPERATION_INTERRUPTED" "OPERATION_INTERRUPTED" "$(jerr)"
  expect_contains "错误说明实际状态未知" "实际状态未知" \
    "$(python3 -c 'import json,sys
d=json.load(open(sys.argv[1],encoding="utf-8"))
e=(d.get("data") or {}).get("error") or {}
print(e.get("message") or "")' "$WORK/job.json" 2>/dev/null)"
  # 不自动重跑：任务数不变，且没有新的 succeeded 任务出现在 ${BLANK_C}1 上。
  expect_eq "中断没有引出自动重跑（任务数不变）" "$JOBS_BEFORE_KILL" "$(job_count)"
  expect_eq "中断之后没有第二条针对同一分区的成功任务" "0" \
    "$(disk_jobs_succeeded "${BLANK_C}1")"
else
  kill_frozen
  notrun "进程中断（SIGKILL 代理 → interrupted）" \
    "三次都没能在 mkfs 的运行窗口内冻住它；这是测试的竞态，不当作功能缺失，诊断见 frozen2.pid.trace"
  [ -n "$JOB_K" ] && wait_job "$JOB_K" >/dev/null
fi

echo
echo "== 12. 回归与日志卫生 =="
expect_eq "存储拓扑仍可读" "200" "$(api "$JAR" "$CSRF" GET "$STORE/topology")"
expect_eq "拓扑里设备数 > 0" "True" "$(resp_get 'len(d["data"]["nodes"]) > 0')"
expect_eq "设备详情 200" "200" "$(api "$JAR" "$CSRF" GET "$STORE/devices/$BLANK_A")"
expect_eq "未知设备 404" "404" "$(api "$JAR" "$CSRF" GET "$STORE/devices/nosuchdisk")"
expect_eq "任务列表仍可读" "200" "$(api "$JAR" "$CSRF" GET "$JOBS")"
expect_eq "没有非终态任务" "0" \
  "$(resp_get 'len([j for j in d["data"]["jobs"] if j["status"] not in ("failed","succeeded","canceled","interrupted")])')"

# Web 一侧永远不该自己调 mkfs/blkid：格式化是代理以 root 执行的，Web 只转发意图。
if grep -Eq 'mkfs\.|mke2fs|blkid' "$WEB_LOG" 2>/dev/null; then
  fail "Web 日志里没有 mkfs/blkid" "$(grep -Eo 'mkfs\.|mke2fs|blkid' "$WEB_LOG" | head -3 | tr '\n' ' ')"
else
  pass "Web 日志里没有 mkfs/blkid"
fi
for f in "$AGENT_LOG" "$WEB_LOG"; do
  if grep -q "$PASSWORD" "$f" 2>/dev/null; then
    fail "日志里没有明文口令 $(basename "$f")" "found"
  else
    pass "日志里没有明文口令 $(basename "$f")"
  fi
done

echo
echo "== 13. 收尾：擦掉夹具 =="
for d in $SPARE_DISKS; do
  wipe_disk "/dev/$d"
done
lsblk -o NAME,TYPE,SIZE,FSTYPE,MOUNTPOINTS >"$WORK/logs/lsblk-final.txt" 2>&1
LEFTOVER=""
for d in $SPARE_DISKS; do
  _kids=$(lsblk -lno NAME "/dev/$d" 2>/dev/null | wc -l | tr -d ' ')
  [ "$_kids" -gt 1 ] && LEFTOVER="$LEFTOVER $d"
done
if [ -z "$LEFTOVER" ]; then
  pass "收尾后一次性盘上没有残留分区"
else
  fail "收尾后一次性盘上没有残留分区" "$LEFTOVER"
fi
for d in $SPARE_DISKS; do
  expect_eq "收尾后 /dev/${d}1 上没有文件系统" "" "$(fs_type "/dev/${d}1")"
done
settle "$BLANK_A" 'd["data"]["device"]["partition_table"]["type"] == "none"'
# 预检一个**根本不存在**的设备：这是判决层正确的拒绝（DEVICE_NOT_FOUND），不是内部错误。
# 曾经是 500（Q190）：拒绝路径没有目标可以绑定，预检层却无条件要求“目标身份指纹”，
# 于是把这条最普通的拒绝判成“预检结果不合法”。这条断言是那个缺陷的回归锁。
CODE_MISSING=$(preflight storage.filesystem.format \
  '{"device_id":"nosuchdev1","filesystem":"ext4"}')
cp "$WORK/preflight.json" "$PAYLOADS/preflight-format-missing-device.json"
expect_eq "不存在的设备：预检不是 500" "200" "$CODE_MISSING"
expect_eq "不存在的设备：不允许执行" "False" "$(pallowed)"
expect_eq "不存在的设备：阻塞码是 DEVICE_NOT_FOUND" "DEVICE_NOT_FOUND" "$(pall)"
expect_eq "不存在的设备：拒绝的理由说得清问的是谁" "True" \
  "$(pget 'd["data"]["target"]["display_name"] == "nosuchdev1"')"
expect_eq "不存在的设备：不编造身份指纹" "<空>" \
  "$(pget 'd["data"]["target"]["identity_fingerprint"] or "<空>"')"
# 擦盘之后同一个 id 已经指向不存在的分区：现场与上面那条是同一个分支，只作记录。
preflight storage.filesystem.format "{\"device_id\":\"${BLANK_A}1\",\"filesystem\":\"ext4\"}" >/dev/null
note "擦盘之后对 ${BLANK_A}1 的预检（记录项，不当作断言）：allowed=$(pget 'd.get("data",{}).get("allowed")') blockers=$(pget '",".join(b["code"] for b in d.get("data",{}).get("blockers",[])) or "-"')"

echo
echo "== 汇总 =="
printf 'PASS %s / FAIL %s / NOT RUN %s\n' "$PASSED" "$FAILED" "$NOTRUN"
[ -z "$FAILED_NAMES" ] || printf '失败项：%s\n' "$FAILED_NAMES"
[ "$FAILED" -eq 0 ] || exit 1
exit 0
