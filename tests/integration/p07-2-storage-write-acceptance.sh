#!/bin/sh
# P07.2 Linux 验收脚本：**真的写盘**的那一半。
#
# 在隔离 VM 的空白虚拟盘上初始化 GPT、创建分区、删除未使用分区，逐条验证
# STAGES §9 P07.2 要求的四件事：固定模板、对齐、udev 等待、真实结果验证。
#
# 与 p07-1 的分工（两个脚本合起来才是 P07 的完整验收）：
#   p07-1 证明“该拒的都拒，而且一个字节都不写”；
#   本脚本证明“该写的写对了”——写下去的是固定模板、位置对齐、内核看见了、
#   盘上的字节与内核的视图两边都对得上。
#
# 运行位置：Ubuntu 24.04 验收 VM 内，**必须以 root 运行**（代理只能在 root 下启动，
# 写动作也是代理以 root 执行的）。
# 不依赖 Go 工具链：用宿主交叉编译好的 Linux 二进制。
#
# 用法：
#   sudo BIN_DIR=/tmp/upanel-bin SPARE_DISKS="sdb sdc sdd" \
#        sh tests/integration/p07-2-storage-write-acceptance.sh
#
# 可选环境变量：
#   PORT=19472         Web 监听端口（与 p07-1 不同，避免撞车）
#   SPARE_DISKS        三块**一次性**空盘的设备名（默认 "sdb sdc sdd"）
#
# 退出码：0 全部通过；1 有失败项；2 环境不满足（非 root / 盘不安全 / 缺工具）。
#
# 本脚本**会让产品真的写盘**，因此每一条写断言都配一条独立证据：
#   - 固定模板：不读产品的解析结果，直接用 python 读 LBA1 的 GPT 头，检查
#     FirstUsableLBA == 2048（1 MiB）、盘尾备份头存在；
#   - 对齐：分区起点字节数 % 1048576 == 0，且等于预检里请求的那个起点；
#   - udev 等待：任务结束时 observed_state.udev_settled == true，且 /dev 节点与
#     /sys/class/block 里的条目**当场**就在（不 sleep 之后再查）；
#   - 真实结果验证：任务的 result.outcome 必须是 verified（退出码 0 不算验证），
#     并且盘上条目表（disk_entries）与内核视图（kernel_partitions）两边都对上。
#
# 覆盖的验收点（STAGES §9 P07.2、PLAN §9.1/§9.2、CONTRACTS §5/§6/§13）：
#   1. 安全守卫：先算出承载 / 的整盘，拒绝把任何与它同源的设备当一次性盘
#   2. 能力真实性：三个分区动作登记成可执行，格式化（P07.3）仍未登记
#   3. 写原语依赖：代理硬编码的 /usr/sbin/sfdisk、/usr/bin/udevadm 真的在、可执行
#   4. 初始化 GPT：allowed → 再认证 → 任务 succeeded/verified；盘上 FirstUsableLBA
#      是 2048，盘尾备份头在，没有 /dev 分区节点，首尾哈希变了
#   5. 创建分区：起点 1 MiB 对齐、大小与请求一致、条目表与内核两边都对上、
#      /dev/<part> 立刻存在；名字写进条目表（用 sgdisk 这个**别的工具**读回来）
#   6. 删除未使用分区：条目消失、内核节点消失、前面的分区槽号没有被重排
#   7. 真实结果验证的两处：result.outcome=verified、observed_state 两源一致；
#      幂等重放同一个键不会写第二次
#   8. 执行前复核：预检之后用宿主工具改了盘 → 提交 409 PREFLIGHT_STALE，
#      盘上一个字节都没多写，任务中心留下一条 failed/rejected
#   9. 系统盘：预检不允许 → 提交被拒，写动作根本到不了执行器
#  10. 身份稳定：写动作不改变设备身份指纹（否则写完之后自己的预检立刻失效）
#  11. 并发：同一块盘上两个提交只允许一个成功，盘上不多出第二个分区
#  12. 回归与日志卫生、收尾：擦盘后设备能力回到 available
#
# 明确不覆盖（NOT RUN，原因写在这里，不在脚本里假装通过）：
#   - **DEVICE_IDENTITY_CHANGED**：身份指纹来自序列号/WWN/型号+拓扑地址，VM 里
#     没法在不换硬件的前提下改变它。真机上要热插拔一块同型号盘才可能触发。
#     单元测试 TestWriteRejectsIdentityChange 用假扫描器覆盖了这条分支。
#   - **runner 内的第二次 revalidate**：要在“代理受理之后、执行之前”正好有人改盘。
#     这是真实竞态，脚本不能稳定制造；§10 的两个并发提交偶尔会撞到它（输掉的那个
#     任务停在 step=revalidate，而不是受理阶段就被拒），脚本把实际发生的闸门打印
#     出来，但不要求它必然触发——没触发不等于功能缺失。
#   - **备份 GPT 表不可读**：产品的条目表读取只读主表（LBA 1..33），盘尾的备份表
#     不解。本脚本用独立读取证明备份头**存在**，但产品不做一致性比对——这是已记录
#     的限制，不是这条断言的替身。
#   - 格式化（ext4/xfs）与文件系统校验：P07.3。
#   - 损坏的 GPT（CRC 错、主备不一致）：没有安全的制造方法又不影响后续用例。

set -u

BIN_DIR=${BIN_DIR:-/tmp/upanel-bin}

if [ "$(id -u)" -ne 0 ]; then
  printf '本脚本必须以 root 运行：sudo BIN_DIR=%s sh %s\n' "$BIN_DIR" "$0" >&2
  exit 2
fi

AGENT="$BIN_DIR/upanel-agent"
WEB="$BIN_DIR/upanel-web"
CTL="$BIN_DIR/upanelctl"

WORK=${WORK:-/var/tmp/upanel-accept-p07-2}
PAYLOADS=$WORK/payloads
RUN_DIR=/run/upanel
SOCK=$RUN_DIR/agent.sock
SYS_CFG=/etc/upanel/upanel.json
AGENT_DB_DIR=/var/lib/upanel-agent
AGENT_DB=$AGENT_DB_DIR/agent.db
KEY_FILE=/etc/upanel/agent.key
WEB_DB_DIR=$WORK/webdata
WEB_DB=$WEB_DB_DIR/upanel.db
PORT=${PORT:-19472}
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

# 写动作用的固定尺寸（都是 1 MiB 的整数倍，便于断言对齐）。
PART1_SIZE=67108864   # 64 MiB
PART2_SIZE=33554432   # 32 MiB
ALIGN=1048576

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
# 请求体留在 $WORK/jobreq-submit.json，幂等重放要用同一份字节。
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
  while [ "$_i" -lt 200 ]; do
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
# 回显任务终态；提交阶段就被拒时回显 "submit:<状态码>"（断言里一眼看得出是
# “没提交上去”还是“执行失败”）。任务视图始终在 $WORK/job.json。
submit_and_wait() {
  _code=$(submit_job "$1" "${2:-}" "$3")
  if [ "$_code" != "202" ]; then printf 'submit:%s' "$_code"; return 0; fi
  _id=$(jget "$WORK/submit.json" 'd["data"]["job_id"]')
  wait_job "$_id"
}

jstatus() { jobget 'd["data"]["status"]'; }
joutcome() { jobget 'd["data"]["result"]["outcome"]'; }
jverified() { jobget 'bool(d["data"]["result"].get("verified_at"))'; }
jerr() { jobget 'd["data"]["error"]["code"]'; }
jobobserved() { jobget "d[\"data\"][\"result\"][\"observed_state\"][\"$1\"]"; }
jobdata() { jobget "d[\"data\"][\"result\"][\"data\"].get(\"$1\")"; }

# conc_outcome <conc-a|conc-b>：把 §10 两个并发提交的两种响应形状归一成 code|step。
# 受理后的失败任务在 data.error；提交阶段被拒的只有 error（没有任务，step 记 submit）。
conc_outcome() {
  python3 - "$WORK/job-$1.json" <<'PY' 2>/dev/null || printf 'PARSE|failed'
import json, sys
try:
    d = json.load(open(sys.argv[1], encoding="utf-8"))
except Exception:
    print("PARSE|failed"); raise SystemExit
data = d.get("data") or {}
err = (data.get("error") or (d.get("error") or {}))
code = err.get("code") or ""
step = data.get("step") or ("submit" if not data else "")
print("%s|%s" % (code, step))
PY
}

pallowed() { pget 'd["data"]["allowed"]'; }
pall() { pget '",".join(b["code"] for b in d["data"]["blockers"])'; }
pwhy() { pget '"; ".join(b["message"] for b in d["data"]["blockers"])'; }
pimpact() { pget '",".join(i["kind"] for i in d["data"]["impacts"])'; }
plocks() { pget '",".join(d["data"]["locks"])'; }
pstate() { pget 'd["data"]["target"]["display_name"]'; }
pfp() { pget 'd["data"]["target"]["identity_fingerprint"]'; }
pphrase() { pget 'd["data"]["confirmation_phrase"]'; }
preauth() { pget 'd["data"]["requires_reauth"]'; }
pimpacttext() { pget '"; ".join(i["description"] for i in d["data"]["impacts"])'; }
pwarncodes() { pget '",".join(w["code"] for w in d["data"]["warnings"])'; }
pwarnwhy() { pget '"; ".join(w["message"] for w in d["data"]["warnings"])'; }

# sgdisk_parts <设备>：sgdisk -p 列出的槽位号，逗号分隔（空表就是空串）。
# 不要用 sgdisk -i 的退出码判断条目在不在——条目不存在时它照样 rc=0（实测）。
sgdisk_parts() {
  sgdisk -p "$1" 2>/dev/null | awk '/^Number/{seen=1; next} seen==1 && NF>0 {print $1}' | paste -sd, -
}

# device <设备 id>：把设备详情存成 $WORK/device.json。
device() { api "$JAR" "$CSRF" GET "$STORE/devices/$1" >/dev/null; cp "$WORK/resp.json" "$WORK/device.json"; }

# free_starts <n>：设备详情里按大小降序的第 n 个空闲区起点（字节）。
free_starts() {
  devget "int(sorted(d[\"data\"][\"device\"][\"partition_table\"][\"free_regions\"], key=lambda r: int(r[\"size_bytes\"]), reverse=True)[$1][\"start_bytes\"])"
}
# entry_field <下标> <字段>：设备详情里盘上条目表的第 i 条（从 0 起）。
entry_field() { devget "d[\"data\"][\"device\"][\"partition_table\"][\"disk_entries\"][$1][\"$2\"]"; }
entry_count() { devget 'len(d["data"]["device"]["partition_table"]["disk_entries"] or [])'; }

# job_count：任务总数（分页上限内）。
job_count() { api "$JAR" "$CSRF" GET "/api/v1/jobs?limit=200" >/dev/null; resp_get 'len(d["data"]["jobs"])'; }

# 快照新鲜度：拓扑每 5 秒重扫，写完盘立刻读设备详情会拿到写之前的快照。
# 下面两个函数复制自 p07-1：只认“在夹具/写动作之后采集、且满足表达式”的快照。
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
    pass "快照反映了写之后的状态（$1）"
  else
    fail "快照反映了写之后的状态（$1）" "30 秒内没有出现写之后且满足「$2」的快照"
  fi
}

# disk_digest <设备>：头部 4 MiB + 尾部 4 MiB 的 sha256。
# GPT 主表在头部、备份表在尾部，两处都要覆盖，否则“尾部没写”会漏过去。
disk_digest() {
  _size=$(blockdev --getsize64 "$1" 2>/dev/null || echo 0)
  _skip=$(( _size / 1048576 - 4 ))
  [ "$_skip" -lt 0 ] && _skip=0
  { dd if="$1" bs=1M count=4 2>/dev/null; dd if="$1" bs=1M skip="$_skip" count=4 2>/dev/null; } \
    | sha256sum | cut -d' ' -f1
}

# gpt_field <设备> <first|last|backup>
#
# **独立读取**盘上的字节，不经过产品自己的解析器：这是“固定模板真的写下去了”的
# 唯一证据。first/last 是第 1 个 LBA 里 GPT 头的 FirstUsableLBA / LastUsableLBA
# （偏移 40 起的两个 uint64 LE）；backup 检查最后一个 LBA 是不是 GPT 备份头。
gpt_field() {
  python3 - "$1" "$2" <<'PY' 2>/dev/null
import struct, sys
dev, what = sys.argv[1], sys.argv[2]
with open(dev, "rb") as f:
    f.seek(512)
    hdr = f.read(92)
    if what in ("first", "last"):
        if hdr[0:8] != b"EFI PART":
            print("NOHEADER")
            raise SystemExit
        print(struct.unpack_from("<QQ", hdr, 40)[0 if what == "first" else 1])
    else:
        f.seek(0, 2)
        size = f.tell()
        f.seek((size // 512 - 1) * 512)
        print("yes" if f.read(8) == b"EFI PART" else "no")
PY
}

# part_absent <设备名>：/dev 节点与 sysfs 条目都不在时才为真。
part_absent() {
  [ ! -e "/dev/$1" ] && [ ! -e "/sys/class/block/$1" ] && echo yes || echo no
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

echo "== 0. 环境与安全守卫 =="
for b in "$AGENT" "$WEB" "$CTL"; do
  if [ -x "$b" ]; then pass "二进制存在且可执行 $(basename "$b")"; else fail "二进制存在且可执行 $(basename "$b")" "$b"; fi
done
for t in python3 curl runuser blockdev sha256sum findmnt lsblk sgdisk blkid \
         wipefs udevadm dd ss; do
  if command -v "$t" >/dev/null 2>&1; then pass "依赖命令可用 $t"; else fail "依赖命令可用 $t" "缺少 $t"; fi
done
# 代理里写死的就是这两个路径（storage.DefaultSfdiskPath / DefaultUdevadmPath）。
# 这里断言的是**代理实际会 exec 的那个文件**，不是 PATH 里随便一个同名程序。
for p in /usr/sbin/sfdisk /usr/bin/udevadm; do
  if [ -x "$p" ]; then pass "写原语依赖存在且可执行 $p"; else fail "写原语依赖存在且可执行 $p" "不存在或不可执行"; fi
done
note "sfdisk: $(/usr/sbin/sfdisk --version 2>&1 | head -1)"
note "udevadm: $(/usr/bin/udevadm --version 2>&1 | head -1)"

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

# 承载 / 的设备链：先算出整盘，再拒绝任何与它同源的一次性盘。
# 这一步失败就退出（exit 2）：宁可拒绝运行，也不能拿一块可能放着系统的盘做“验收”。
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
  if dmsetup ls 2>/dev/null | grep -qw "$d"; then
    echo "/dev/$d 被 device-mapper 引用，拒绝使用" >&2
    exit 2
  fi
  _size=$(blockdev --getsize64 "/dev/$d" 2>/dev/null || echo 0)
  # 建两个分区（64+32 MiB）之后还要留出两块空闲区给并发用例，2 GiB 足够。
  if [ "$_size" -lt 2147483648 ]; then
    echo "/dev/$d 容量 ${_size} 字节，小于 2 GiB，无法验证空闲区" >&2
    exit 2
  fi
  note "/dev/$d：$_size 字节，无挂载、无 swap、无 dm 引用"
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
echo "== 2. 三个分区动作是可执行的，格式化还不是 =="
# 这一条和 p07-1 §2 是同一个取舍的两面：p07-1 断言“没有计划外的名字”，
# 这里断言“这一轮该有的名字真的有了”。两边都不钉死整张清单。
for a in storage.partition_table.initialize storage.partition.create storage.partition.delete; do
  expect_eq "capabilities 里有 $a" "True" \
    "$(jget "$PAYLOADS/agent-capabilities.json" "\"$a\" in d[\"data\"][\"supported_actions\"]")"
done
expect_eq "格式化还没有执行器（P07.3）" "False" \
  "$(jget "$PAYLOADS/agent-capabilities.json" '"storage.filesystem.format" in d["data"]["supported_actions"]')"
note "代理登记的动作数：$(jget "$PAYLOADS/agent-capabilities.json" 'len(d["data"]["supported_actions"])')"

echo
echo "== 3. 空白盘初始化 GPT：真的写盘，模板固定，结果两源核验 =="
BEFORE_A=$(disk_digest "/dev/$BLANK_A")
expect_eq "初始化预检状态码" "200" \
  "$(preflight storage.partition_table.initialize "{\"device_id\":\"$BLANK_A\",\"table_type\":\"gpt\"}")"
cp "$WORK/preflight.json" "$PAYLOADS/preflight-init.json"
expect_eq "初始化预检允许" "True" "$(pallowed)"
expect_eq "初始化的锁是整盘" "disk:$BLANK_A" "$(plocks)"
expect_eq "初始化要求再认证" "True" "$(preauth)"
expect_contains "短语首词是 INIT" "INIT " "$(pphrase)"
FP_DISK=$(pfp)
note "目标：$(pstate)　指纹：$FP_DISK"

PF_INIT=$(pget 'd["data"]["preflight_id"]')
PHRASE_INIT=$(pphrase)
expect_eq "再认证" "200" "$(reauth)"
expect_eq "初始化任务成功" "succeeded" "$(submit_and_wait "$PF_INIT" "$PHRASE_INIT" "k-init-1")"
cp "$WORK/job.json" "$PAYLOADS/job-init.json"
JOB_INIT=$(jobget 'd["data"]["job_id"]')
expect_eq "任务终态是 succeeded" "succeeded" "$(jstatus)"
# 退出码 0 不算验证：写任务的 result.outcome 必须是 verified，且带 verified_at。
expect_eq "结果是 verified（不是“命令跑完了”）" "verified" "$(joutcome)"
expect_eq "结果带 verified_at" "True" "$(jverified)"
expect_eq "observed_state 里的表类型" "gpt" "$(jobobserved table_type)"
expect_eq "observed_state 说 udev 已稳定" "True" "$(jobobserved udev_settled)"
expect_eq "初始化之后没有内核分区" "[]" "$(jobobserved kernel_partitions)"
expect_eq "observed_state 里的设备 id" "$BLANK_A" "$(jobobserved device_id)"
expect_eq "observed_state 带盘上 GUID（非空）" "True" "$(jobget 'bool(d["data"]["result"]["observed_state"]["disk_guid"])')"
expect_eq "核验走了两个来源" "['disk_bytes', 'kernel_sysfs']" "$(jobdata checked_via)"
expect_eq "data 里的分区数是 0" "0" "$(jobdata partitions)"
# 写下去的是固定模板：直接读 LBA1 的 GPT 头，不看产品自己的解析结果。
expect_eq "盘上 GPT 头的 FirstUsableLBA 是 2048（1 MiB）" "2048" "$(gpt_field "/dev/$BLANK_A" first)"
# LastUsableLBA 由盘大小决定，只断言它落在盘的最后一个 LBA 之内且非零。
LAST_USABLE=$(gpt_field "/dev/$BLANK_A" last)
case "$LAST_USABLE" in
  '' | 0 | NOHEADER | ERR:*) fail "LastUsableLBA 合法" "$LAST_USABLE" ;;
  *) pass "LastUsableLBA 合法（$LAST_USABLE）" ;;
esac
expect_eq "盘尾的 GPT 备份头存在" "yes" "$(gpt_field "/dev/$BLANK_A" backup)"
expect_eq "初始化后没有 /dev/${BLANK_A}1 节点" "yes" "$(part_absent "${BLANK_A}1")"
AFTER_A=$(disk_digest "/dev/$BLANK_A")
if [ "$BEFORE_A" != "$AFTER_A" ]; then
  pass "初始化真的写了盘（首尾 4 MiB 哈希变了）"
else
  fail "初始化真的写了盘" "首尾哈希没变（$BEFORE_A）"
fi
note "写前 $BEFORE_A"
note "写后 $AFTER_A"

# 幂等：同一个键 + 同一份请求体重放，返回原任务，不写第二次。
JOBS_BEFORE_REPLAY=$(job_count)
DIGEST_AFTER_INIT=$(disk_digest "/dev/$BLANK_A")
expect_eq "同一键重放提交返回 202" "202" "$(submit_job "$PF_INIT" "$PHRASE_INIT" "k-init-1")"
expect_eq "重放返回原来那条任务" "$JOB_INIT" "$(resp_get 'd["data"]["job_id"]')"
expect_eq "重放没有新增任务" "$JOBS_BEFORE_REPLAY" "$(job_count)"
expect_eq "重放没有写第二次盘" "$DIGEST_AFTER_INIT" "$(disk_digest "/dev/$BLANK_A")"

settle "$BLANK_A" 'd["data"]["device"]["partition_table"]["type"] == "gpt"'
expect_eq "设备详情里盘上条目数是 0" "0" "$(entry_count)"
expect_eq "设备详情里的盘 GUID 与任务观察值一致" "$(jobobserved disk_guid)" \
  "$(devget 'd["data"]["device"]["partition_table"].get("disk_guid")')"

echo
echo "== 4. 创建分区：1 MiB 对齐、条目表与内核两边都对上 =="
START1=$(free_starts 0)
case "$START1" in
  '' | ERR:*) fail "读到最大空闲区起点" "$START1"; START1=1048576 ;;
  *) pass "读到最大空闲区起点（$START1 字节）" ;;
esac
# 对齐：起点必须是 1 MiB 的整数倍。这是 PLAN §9.1“只在 1 MiB 边界上建分区”的直接证据。
expect_eq "空闲区起点是 1 MiB 对齐（${START1} % ${ALIGN}）" "0" "$((START1 % ALIGN))"
expect_eq "第一个分区的起点就是 1 MiB" "$ALIGN" "$START1"

expect_eq "建分区预检状态码" "200" \
  "$(preflight storage.partition.create \
     "{\"device_id\":\"$BLANK_A\",\"start_bytes\":\"$START1\",\"size_bytes\":\"$PART1_SIZE\"}")"
cp "$WORK/preflight.json" "$PAYLOADS/preflight-create-1.json"
expect_eq "建分区预检允许" "True" "$(pallowed)"
expect_eq "建分区的锁是整盘" "disk:$BLANK_A" "$(plocks)"
expect_eq "建分区要求再认证" "True" "$(preauth)"
expect_contains "建分区的影响里有分区表变更" "partition_table_change" "$(pimpact)"
PF_C1=$(pget 'd["data"]["preflight_id"]')
# create 不需要确认短语（P07.1 定的：只在空闲区加一条条目，不覆盖任何数据）。
# 这里断言的是“没有短语”，提交时就必须带空串——把 "None" 当短语发出去只是碰巧能过。
expect_eq "建分区不要求确认短语" "True" "$(pget 'd["data"]["confirmation_phrase"] is None')"
expect_eq "再认证" "200" "$(reauth)"
expect_eq "建分区任务成功" "succeeded" "$(submit_and_wait "$PF_C1" "" "k-create-1")"
cp "$WORK/job.json" "$PAYLOADS/job-create-1.json"
expect_eq "建分区的结果是 verified" "verified" "$(joutcome)"
expect_eq "udev 已稳定" "True" "$(jobobserved udev_settled)"
expect_eq "内核里有 $BLANK_A-1 这一项" "['${BLANK_A}1']" "$(jobobserved kernel_partitions)"
expect_eq "data.created_partition 的槽位号" "1" \
  "$(jobget 'd["data"]["result"]["data"]["created_partition"]["number"]')"
expect_eq "data.created_partition 的起点与请求一致" "$START1" \
  "$(jobget 'd["data"]["result"]["data"]["created_partition"]["start_bytes"]')"
expect_eq "data.created_partition 的大小与请求一致" "$PART1_SIZE" \
  "$(jobget 'd["data"]["result"]["data"]["created_partition"]["size_bytes"]')"
expect_eq "data.created_partition 的内核设备名" "${BLANK_A}1" \
  "$(jobget 'd["data"]["result"]["data"]["created_partition"]["device_id"]')"
# udev 等待的证据：任务刚结束就查，节点已经在（不是 sleep 之后再查）。
expect_eq "/dev/${BLANK_A}1 当场就存在" "yes" "$([ -b "/dev/${BLANK_A}1" ] && echo yes || echo no)"
expect_eq "sysfs 里也有 ${BLANK_A}1" "yes" "$([ -e "/sys/class/block/${BLANK_A}1" ] && echo yes || echo no)"
# 内核看到的字节数与请求一致（lsblk --bytes 读的是内核的大小属性）。
expect_eq "内核报告的 ${BLANK_A}1 字节数" "$PART1_SIZE" \
  "$(lsblk -bno SIZE "/dev/${BLANK_A}1" 2>/dev/null | tr -d ' ')"

settle "$BLANK_A" 'len(d["data"]["device"]["partition_table"]["disk_entries"] or []) == 1'
expect_eq "盘上的条目数是 1" "1" "$(entry_count)"
expect_eq "盘上条目的槽位号" "1" "$(entry_field 0 number)"
expect_eq "盘上条目的起点与请求一致" "$START1" "$(entry_field 0 start_bytes)"
expect_eq "盘上条目的大小与请求一致" "$PART1_SIZE" "$(entry_field 0 size_bytes)"
# 起点换算成扇区，必须是 2048 的整数倍（1 MiB = 2048 × 512 字节）。
expect_eq "盘上条目的起点扇区是 2048 的整数倍" "0" "$(( $(entry_field 0 start_bytes) / 512 % 2048 ))"

echo
echo "== 5. 第二个分区带上名字：名字要真的落进条目表 =="
START2=$(free_starts 0)
case "$START2" in
  '' | ERR:*) fail "读到第二个空闲区起点" "$START2"; START2=0 ;;
  *) pass "读到第二个空闲区起点（$START2 字节）" ;;
esac
if [ "$START2" -gt 0 ]; then
  expect_eq "第二个起点也是 1 MiB 对齐" "0" "$((START2 % ALIGN))"
  expect_eq "第二个空区起点与第一个不重叠" "yes" \
    "$([ "$START2" -ge "$((START1 + PART1_SIZE))" ] && echo yes || echo no)"
  expect_eq "建第二个分区预检" "200" \
    "$(preflight storage.partition.create \
       "{\"device_id\":\"$BLANK_A\",\"start_bytes\":\"$START2\",\"size_bytes\":\"$PART2_SIZE\",\"name\":\"data\"}")"
  cp "$WORK/preflight.json" "$PAYLOADS/preflight-create-2.json"
  expect_eq "建第二个分区预检允许" "True" "$(pallowed)"
  PF_C2=$(pget 'd["data"]["preflight_id"]')
  expect_eq "再认证" "200" "$(reauth)"
  expect_eq "建第二个分区任务成功" "succeeded" "$(submit_and_wait "$PF_C2" "" "k-create-2")"
  cp "$WORK/job.json" "$PAYLOADS/job-create-2.json"
  expect_eq "第二个分区的结果是 verified" "verified" "$(joutcome)"
  expect_eq "第二个分区的槽位号" "2" \
    "$(jobget 'd["data"]["result"]["data"]["created_partition"]["number"]')"
  expect_eq "第二个分区的名字写进了结果" "data" \
    "$(jobget 'd["data"]["result"]["data"]["created_partition"]["name"]')"
  expect_eq "/dev/${BLANK_A}2 存在" "yes" "$([ -b "/dev/${BLANK_A}2" ] && echo yes || echo no)"
  settle "$BLANK_A" 'len(d["data"]["device"]["partition_table"]["disk_entries"] or []) == 2'
  expect_eq "盘上条目 2 的名字" "data" "$(entry_field 1 name)"
  # 用**另一个工具**把名字读回来：产品的解析器不该是唯一的证人。
  expect_contains "sgdisk 从盘上读回分区 2 的名字" "data" \
    "$(sgdisk -i 2 "/dev/$BLANK_A" 2>/dev/null | tr -d '\r')"
  # 留一份真实响应给前端做 DTO 夹具：这是“两块分区、其中一块有名字”的设备详情，
  # 同时含 disk_guid 与 disk_entries（P07.2 新增的两个字段）。前端测试直接回放它，
  # 而不是我手写一份理想 JSON——手写夹具会把“我以为接口长这样”变成测试前提。
  api "$JAR" "$CSRF" GET "/api/v1/storage/devices/$BLANK_A" >/dev/null
  cp "$WORK/resp.json" "$PAYLOADS/device-two-partitions.json"
  expect_eq "夹具：设备详情里的盘上条目是 2 条" "2" \
    "$(jget "$PAYLOADS/device-two-partitions.json" 'len(d["data"]["device"]["partition_table"]["disk_entries"])')"
  expect_eq "夹具：设备详情里有 disk_guid" "True" \
    "$(jget "$PAYLOADS/device-two-partitions.json" 'bool(d["data"]["device"]["partition_table"]["disk_guid"])')"
else
  notrun "建第二个分区" "这块盘上只剩一块空闲区，造不出第二个分区"
fi

echo
echo "== 6. 删除未使用分区：条目消失、节点消失、前面的槽号不重排 =="
# 先给这个分区放一个文件系统签名（测试手工具，不是产品动作）。影响列表是按**签名**认定
# 数据的：一块空白分区被删只是因为“盘上有一条条目”而不可逆，一块有 ext4 的分区被删才是
# 真正的数据丢失。两条都要看，才知道 product 不是在背一句固定台词。
mkfs.ext4 -q -F "/dev/${BLANK_A}2" >/dev/null 2>&1
expect_eq "夹具：${BLANK_A}2 上有 ext4 签名" "ext4" \
  "$(blkid -o value -s TYPE "/dev/${BLANK_A}2" 2>/dev/null)"
DIGEST_BEFORE_DEL=$(disk_digest "/dev/$BLANK_A")
expect_eq "删除预检状态码" "200" \
  "$(preflight storage.partition.delete "{\"device_id\":\"${BLANK_A}2\"}")"
cp "$WORK/preflight.json" "$PAYLOADS/preflight-delete-1.json"
expect_eq "删除预检允许" "True" "$(pallowed)"
expect_eq "删除的锁是整盘（不是分区自己）" "disk:$BLANK_A" "$(plocks)"
expect_eq "删除要求再认证" "True" "$(preauth)"
expect_contains "删除的影响里有数据丢失" "data_loss" "$(pimpact)"
expect_contains "删除的影响里有分区表变更" "partition_table_change" "$(pimpact)"
expect_contains "告警里说这块数据会无法访问" "EXISTING_DATA_WILL_BE_LOST" "$(pwarncodes)"
expect_contains "告警里点出 ext 系列签名" "ext 系列文件系统签名" "$(pwarnwhy)"
PF_D1=$(pget 'd["data"]["preflight_id"]')
PH_D1=$(pphrase)
expect_eq "再认证" "200" "$(reauth)"
expect_eq "删除任务成功" "succeeded" "$(submit_and_wait "$PF_D1" "$PH_D1" "k-delete-1")"
cp "$WORK/job.json" "$PAYLOADS/job-delete-1.json"
expect_eq "删除的结果是 verified" "verified" "$(joutcome)"
expect_eq "data 里记下的被删设备" "${BLANK_A}2" "$(jobdata deleted_device_id)"
expect_eq "删完之后的分区数" "1" "$(jobdata partitions)"
expect_eq "删除后内核只剩 ${BLANK_A}1" "['${BLANK_A}1']" "$(jobobserved kernel_partitions)"
expect_eq "/dev/${BLANK_A}2 与 sysfs 条目都不在了" "yes" "$(part_absent "${BLANK_A}2")"
expect_eq "删除真的写了盘" "yes" \
  "$([ "$DIGEST_BEFORE_DEL" != "$(disk_digest "/dev/$BLANK_A")" ] && echo yes || echo no)"
settle "$BLANK_A" 'len(d["data"]["device"]["partition_table"]["disk_entries"] or []) == 1'
expect_eq "盘上只剩 1 条条目" "1" "$(entry_count)"
# 槽号不重排：删掉第 2 条，第 1 条还是 1 号，内核节点也没改名。
expect_eq "第 1 条的槽位号还是 1" "1" "$(entry_field 0 number)"
expect_eq "/dev/${BLANK_A}1 还在" "yes" "$([ -b "/dev/${BLANK_A}1" ] && echo yes || echo no)"
# 用别的工具当第二个证人。注意 sgdisk -i 的**退出码不能用来判断条目在不在**：
# 条目不存在时它打印 "Partition #2 does not exist." 然后仍然 rc=0（实测）。
# 所以看 sgdisk -p 列出的槽位号，不看退出码。
expect_eq "sgdisk 列出的槽位号只剩 1" "1" "$(sgdisk_parts "/dev/$BLANK_A")"
expect_contains "sgdisk 说分区 2 不存在" "does not exist" \
  "$(sgdisk -i 2 "/dev/$BLANK_A" 2>&1 | tr -d '\r')"

echo
echo "== 7. 执行前复核：预检之后有人动了盘 =="
START3=$(free_starts 0)
expect_eq "外部改动前的建分区预检" "200" \
  "$(preflight storage.partition.create \
     "{\"device_id\":\"$BLANK_A\",\"start_bytes\":\"$START3\",\"size_bytes\":\"$PART2_SIZE\"}")"
expect_eq "这条预检当时是允许的" "True" "$(pallowed)"
PF_STALE=$(pget 'd["data"]["preflight_id"]')
PH_STALE=$(pphrase)
# 预检之后、提交之前，别人（这里是脚本里的 sgdisk）在盘上又加了一个分区。
if sgdisk --new=4:0:+8M "/dev/$BLANK_A" >/dev/null 2>&1; then
  pass "夹具：在盘上加了第 4 号分区"
  udevadm settle --timeout=5 >/dev/null 2>&1
  settle "$BLANK_A" 'len(d["data"]["device"]["partition_table"]["disk_entries"] or []) == 2'
  DIGEST_BEFORE_STALE=$(disk_digest "/dev/$BLANK_A")
  JOBS_BEFORE_STALE=$(job_count)
  expect_eq "再认证" "200" "$(reauth)"
  # 代理在**受理**这一刻用同一个判决函数重算计划，状态版本变了就拒收。
  # 所以这里是 409，而不是“建了任务再失败”。
  expect_eq "过期预检提交被拒（409）" "submit:409" "$(submit_and_wait "$PF_STALE" "$PH_STALE" "k-stale-1")"
  cp "$WORK/submit.json" "$PAYLOADS/submit-stale.json"
  expect_eq "拒收的错误码是 PREFLIGHT_STALE" "PREFLIGHT_STALE" \
    "$(jget "$WORK/submit.json" 'd["error"]["code"]')"
  expect_eq "过期预检留下一条失败任务（不是静默丢弃）" "$((JOBS_BEFORE_STALE + 1))" "$(job_count)"
  expect_eq "这条任务停在 failed/rejected、码是 PREFLIGHT_STALE" "True" \
    "$(resp_get 'any(j["status"]=="failed" and j["step"]=="rejected" and j["error"].get("code")=="PREFLIGHT_STALE" for j in d["data"]["jobs"])')"
  expect_eq "被拒之后盘上没多写一个字节" "$DIGEST_BEFORE_STALE" "$(disk_digest "/dev/$BLANK_A")"
  # 回收夹具：把第 4 号分区删掉，后面的用例从头数分区。
  sgdisk --delete 4 "/dev/$BLANK_A" >/dev/null 2>&1
  udevadm settle --timeout=5 >/dev/null 2>&1
  settle "$BLANK_A" 'len(d["data"]["device"]["partition_table"]["disk_entries"] or []) == 1'
else
  notrun "预检过期（PREFLIGHT_STALE）" "夹具没能用 sgdisk 在盘上加分区"
fi
note "受理那一刻的 revalidate 已验到；runner 里的第二次复核只能靠真实竞态，见 §10"

echo
echo "== 8. 系统盘：预检不允许，提交在 Web 侧就被拦住 =="
expect_eq "系统盘初始化预检状态码" "200" \
  "$(preflight storage.partition_table.initialize "{\"device_id\":\"$ROOT_DISK\",\"table_type\":\"gpt\"}")"
expect_eq "系统盘初始化不允许" "False" "$(pallowed)"
expect_contains "系统盘的阻塞码" "SYSTEM_DISK_PROTECTED" "$(pall)"
expect_contains "理由里点出承载系统数据" "系统数据" "$(pwhy)"
PF_ROOT=$(pget 'd["data"]["preflight_id"]')
PH_ROOT=$(pphrase)
ROOT_PARTS_BEFORE=$(lsblk -lno NAME "/dev/$ROOT_DISK" 2>/dev/null | wc -l | tr -d ' ')
JOBS_BEFORE_ROOT=$(job_count)
expect_eq "再认证" "200" "$(reauth)"
# 不允许的预检在**落库之前**就被拒：连一条 failed 任务都不会有（没有意图可执行）。
expect_eq "提交被拒（409）" "submit:409" "$(submit_and_wait "$PF_ROOT" "$PH_ROOT" "k-root-1")"
cp "$WORK/submit.json" "$PAYLOADS/submit-root.json"
expect_eq "拒收的错误码是 DEPENDENCY_BLOCKED" "DEPENDENCY_BLOCKED" \
  "$(jget "$WORK/submit.json" 'd["error"]["code"]')"
expect_eq "被拒的提交没有留下任务" "$JOBS_BEFORE_ROOT" "$(job_count)"
expect_eq "系统盘的分区数没变" "$ROOT_PARTS_BEFORE" \
  "$(lsblk -lno NAME "/dev/$ROOT_DISK" 2>/dev/null | wc -l | tr -d ' ')"
expect_eq "系统盘仍然承载 /" "yes" "$([ -n "$(findmnt -no SOURCE / 2>/dev/null)" ] && echo yes || echo no)"

echo
echo "== 9. 写动作不改变设备身份指纹 =="
# 身份来自序列号/WWN/型号+拓扑地址，与分区表无关。如果写完一次自己的预检就失效，
# 那么“允许过一次写”就等于要重做预检——这条断言守住它。
preflight storage.partition_table.initialize "{\"device_id\":\"$BLANK_A\",\"table_type\":\"gpt\"}" >/dev/null
FP_AFTER=$(pfp)
# 这块盘上已经有 GPT，所以判决当然变成 rejected——判决跟着状态走，身份不该跟着变。
expect_contains "写完之后初始化判决变成已有分区表" "PARTITION_TABLE_PRESENT" "$(pall)"
if [ "$FP_AFTER" = "$FP_DISK" ]; then
  pass "写动作之后设备身份指纹不变"
else
  fail "写动作之后设备身份指纹不变" "预检时 $FP_DISK，写完之后 $FP_AFTER"
fi
preflight storage.partition.delete "{\"device_id\":\"${BLANK_A}1\"}" >/dev/null
FP_PART1=$(pfp)
preflight storage.partition.delete "{\"device_id\":\"${BLANK_A}1\"}" >/dev/null
expect_eq "同一分区的两次预检指纹一致" "$FP_PART1" "$(pfp)"
notrun "DEVICE_IDENTITY_CHANGED" "身份来自序列号/WWN，VM 里改不了；单元测试用假扫描器覆盖"

echo
echo "== 10. 并发：同一块盘上的两个提交只允许一个成功 =="
# 两个预检都基于同一份快照：起点分别是最大空闲区的头和它 32 MiB 之后。
device "$BLANK_A"
START_C1=$(free_starts 0)
START_C2=$((START_C1 + PART2_SIZE))
REGION_MAX=$(devget "int(sorted(d[\"data\"][\"device\"][\"partition_table\"][\"free_regions\"], key=lambda r: int(r[\"size_bytes\"]), reverse=True)[0][\"size_bytes\"])")
if [ "$REGION_MAX" -ge $((PART2_SIZE * 2)) ]; then
  preflight storage.partition.create \
    "{\"device_id\":\"$BLANK_A\",\"start_bytes\":\"$START_C1\",\"size_bytes\":\"$PART2_SIZE\"}" >/dev/null
  PF_A=$(pget 'd["data"]["preflight_id"]')
  PH_A=$(pphrase)
  preflight storage.partition.create \
    "{\"device_id\":\"$BLANK_A\",\"start_bytes\":\"$START_C2\",\"size_bytes\":\"$PART2_SIZE\"}" >/dev/null
  PF_B=$(pget 'd["data"]["preflight_id"]')
  PH_B=$(pphrase)
  expect_eq "再认证" "200" "$(reauth)"
  PARTS_BEFORE=$(entry_count)
  DIGEST_BEFORE_CONC=$(disk_digest "/dev/$BLANK_A")
  # 两个提交同时发。curl 各自把响应写到自己的文件里——共用一个文件会互相覆盖，
  # 那就变成“测试自己的竞态”，不是产品的。
  curl -s -b "$JAR" -o "$WORK/conc-a.json" -w '%{http_code}' -X POST "$BASE$JOBS" \
    -H 'Content-Type: application/json' -H "Origin: $ORIGIN" -H "X-CSRF-Token: $CSRF" \
    -H 'Idempotency-Key: k-conc-a' \
    -d "$(python3 -c 'import json,sys;print(json.dumps({"preflight_id":sys.argv[1],"confirmation_phrase":sys.argv[2]}))' "$PF_A" "$PH_A")" \
    >"$WORK/conc-a.code" &
  PIDA=$!
  curl -s -b "$JAR" -o "$WORK/conc-b.json" -w '%{http_code}' -X POST "$BASE$JOBS" \
    -H 'Content-Type: application/json' -H "Origin: $ORIGIN" -H "X-CSRF-Token: $CSRF" \
    -H 'Idempotency-Key: k-conc-b' \
    -d "$(python3 -c 'import json,sys;print(json.dumps({"preflight_id":sys.argv[1],"confirmation_phrase":sys.argv[2]}))' "$PF_B" "$PH_B")" \
    >"$WORK/conc-b.code" &
  PIDB=$!
  wait "$PIDA" "$PIDB"
  CODE_A=$(cat "$WORK/conc-a.code")
  CODE_B=$(cat "$WORK/conc-b.code")
  note "并发提交状态码：A=$CODE_A B=$CODE_B"
  # 两个提交都可能被受理（202），也可能一个在受理时就被 409 拒掉：
  # 前者说明第二道拦截在工作，后者说明第一道在工作。两种都是健康结果。
  for pair in "A:$CODE_A" "B:$CODE_B"; do
    case "${pair#*:}" in
      202 | 409) pass "并发提交 ${pair%%:*} 的状态码在 {202,409} 之内（${pair#*:}）" ;;
      *) fail "并发提交 ${pair%%:*} 的状态码在 {202,409} 之内" "${pair#*:}" ;;
    esac
  done
  ST_A=""; ST_B=""
  if [ "$CODE_A" = "202" ]; then
    ID_A=$(jget "$WORK/conc-a.json" 'd["data"]["job_id"]')
    ST_A=$(wait_job "$ID_A")
    cp "$WORK/job.json" "$WORK/job-conc-a.json"
    cp "$WORK/job.json" "$PAYLOADS/job-conc-a.json"
  else
    ST_A="rejected:$(jget "$WORK/conc-a.json" 'd["error"]["code"]')"
    cp "$WORK/conc-a.json" "$WORK/job-conc-a.json"
    cp "$WORK/conc-a.json" "$PAYLOADS/job-conc-a.json"
  fi
  if [ "$CODE_B" = "202" ]; then
    ID_B=$(jget "$WORK/conc-b.json" 'd["data"]["job_id"]')
    ST_B=$(wait_job "$ID_B")
    cp "$WORK/job.json" "$WORK/job-conc-b.json"
    cp "$WORK/job.json" "$PAYLOADS/job-conc-b.json"
  else
    ST_B="rejected:$(jget "$WORK/conc-b.json" 'd["error"]["code"]')"
    cp "$WORK/conc-b.json" "$WORK/job-conc-b.json"
    cp "$WORK/conc-b.json" "$PAYLOADS/job-conc-b.json"
  fi
  note "并发任务终态：A=$ST_A B=$ST_B"
  SUCCEEDED=0
  [ "$ST_A" = "succeeded" ] && SUCCEEDED=$((SUCCEEDED + 1))
  [ "$ST_B" = "succeeded" ] && SUCCEEDED=$((SUCCEEDED + 1))
  expect_eq "两个并发提交里只有一个成功" "1" "$SUCCEEDED"
  # 输掉的那一个必须说清原因：不能是 500，也不能静默消失。
  # 两个闸门都可能拦下它——受理时复核（任务停在 rejected），或者 runner 执行前的
  # **第二次**复核（任务停在 revalidate）。后者正是“受理之后、执行之前有人改盘”的
  # 真实竞态，本用例是整份脚本里唯一能撞到它的地方。
  if [ "$ST_A" = "succeeded" ]; then LOST=conc-b; else LOST=conc-a; fi
  LOST_OUT=$(conc_outcome "$LOST")
  LOST_CODE=${LOST_OUT%%|*}
  LOST_STEP=${LOST_OUT#*|}
  note "并发里失败的那一个：code=$LOST_CODE step=$LOST_STEP"
  case "$LOST_CODE" in
    PREFLIGHT_STALE | RESOURCE_BUSY | VALIDATION_ERROR)
      pass "并发里失败的那一个给出了明确原因（$LOST_CODE，step=$LOST_STEP）" ;;
    *) fail "并发里失败的那一个给出了明确原因" "$LOST_OUT" ;;
  esac
  settle "$BLANK_A" "len(d[\"data\"][\"device\"][\"partition_table\"][\"disk_entries\"] or []) >= $((PARTS_BEFORE + 1))"
  expect_eq "盘上只多了一个分区（成功的那个）" "$((PARTS_BEFORE + 1))" "$(entry_count)"
  RACE_KILLED=$(disk_digest "/dev/$BLANK_A")
  if [ "$DIGEST_BEFORE_CONC" != "$RACE_KILLED" ]; then
    pass "并发写之后盘确实变了（成功的那一次写下去了）"
  else
    fail "并发写之后盘确实变了" "首尾哈希没变"
  fi
else
  notrun "并发提交" "最大空闲区只有 $REGION_MAX 字节，放不下两个 ${PART2_SIZE} 的分区"
fi

echo
echo "== 11. 回归与日志卫生 =="
expect_eq "存储拓扑仍可读" "200" "$(api "$JAR" "$CSRF" GET "$STORE/topology")"
expect_eq "拓扑里设备数 > 0" "True" "$(resp_get 'len(d["data"]["nodes"]) > 0')"
expect_eq "设备详情 200" "200" "$(api "$JAR" "$CSRF" GET "$STORE/devices/$BLANK_A")"
expect_eq "未知设备 404" "404" "$(api "$JAR" "$CSRF" GET "$STORE/devices/nosuchdisk")"
expect_eq "任务列表仍可读" "200" "$(api "$JAR" "$CSRF" GET "$JOBS")"
expect_eq "没有非终态任务（写动作都收敛了）" "0" \
  "$(resp_get 'len([j for j in d["data"]["jobs"] if j["status"] not in ("failed","succeeded","canceled")])')"

# Web 一侧永远不该自己调分区工具：写动作是代理以 root 执行的，Web 只转发意图。
if grep -Eq 'sfdisk|sgdisk|wipefs' "$WEB_LOG" 2>/dev/null; then
  fail "Web 日志里没有分区命令" "$(grep -Eo 'sfdisk|sgdisk|wipefs' "$WEB_LOG" | head -3 | tr '\n' ' ')"
else
  pass "Web 日志里没有分区命令"
fi
for f in "$AGENT_LOG" "$WEB_LOG"; do
  if grep -q "$PASSWORD" "$f" 2>/dev/null; then
    fail "日志里没有明文口令 $(basename "$f")" "found"
  else
    pass "日志里没有明文口令 $(basename "$f")"
  fi
done

echo
echo "== 12. 收尾：擦掉夹具 =="
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
settle "$BLANK_A" 'd["data"]["device"]["partition_table"]["type"] == "none"'
expect_eq "擦回空白后初始化预检仍然允许" "200" \
  "$(preflight storage.partition_table.initialize "{\"device_id\":\"$BLANK_A\",\"table_type\":\"gpt\"}")"
expect_eq "擦回空白后 allowed" "True" "$(pallowed)"

echo
echo "== 汇总 =="
printf 'PASS %s / FAIL %s / NOT RUN %s\n' "$PASSED" "$FAILED" "$NOTRUN"
[ -z "$FAILED_NAMES" ] || printf '失败项：%s\n' "$FAILED_NAMES"
[ "$FAILED" -eq 0 ] || exit 1
exit 0
