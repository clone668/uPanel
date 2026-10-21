#!/bin/sh
# P07.1 Linux 验收脚本：存储写动作的**拒绝逻辑**——预检判决、可靠设备身份、
# 整盘锁、系统设备/挂载/swap/holders/依赖保护。
#
# 运行位置：Ubuntu 24.04 验收 VM 内，**必须以 root 运行**（代理只能在 root 下启动；
# 占用检查要读 /proc/*/fd，挂载/swap/dm 夹具也都要特权）。
# 不依赖 Go 工具链：用宿主交叉编译好的 Linux 二进制。
#
# 用法：
#   sudo BIN_DIR=/tmp/upanel-bin SPARE_DISKS="sdb sdc sdd" \
#        sh tests/integration/p07-1-storage-guard-acceptance.sh
#
# 可选环境变量：
#   PORT=19471         Web 监听端口
#   SPARE_DISKS        三块**一次性**空盘的设备名（默认 "sdb sdc sdd"）
#
# 退出码：0 全部通过；1 有失败项；2 环境不满足（非 root / 盘不安全 / 缺工具）。
#
# 本脚本**不会**让产品写任何东西：
#   - 四个存储动作只登记了参数解码与预检，Execute 为 nil，提交必 501；
#   - 脚本里的分区表/文件系统/swap/dm 夹具都是测试手工具（sgdisk/mkfs/mkswap/dmsetup）
#     在一次性空盘上造的，不是产品动作；
#   - §4 与 §5 用前后哈希证明“允许的预检”与“501 的提交”连一个字节都没写。
#
# 覆盖的验收点（STAGES §8 P07.1、PLAN §9.1/§9.2、CONTRACTS §5/§4.1/§6/§13）：
#   1. 安全守卫：先算出承载 / 的整盘，拒绝把任何与它同源的设备当一次性盘
#   2. 能力真实性：capabilities.supported_actions 里**没有**这四个动作（没有执行器就
#      不能宣传）；但预检入口在，且设备 capabilities 的四项状态与判决一致
#   3. 系统盘：整盘初始化、根分区/同盘分区的删除与格式化一律 SYSTEM_DISK_PROTECTED，
#      锁是整盘 disk:<id>
#   4. 空白盘初始化：allowed=true、影响里带“未检测到已知签名不代表盘上绝无原始数据”、
#      影响项 data_loss+partition_table_change、短语首词 INIT、要求再认证、整盘锁
#   5. 提交：未再认证 403 REAUTH_REQUIRED（门在代理之前，盘上字节不变）；再认证后
#      仍是 501 NOT_IMPLEMENTED，任务表只多一条 failed/rejected（Q159 留痕），
#      且没有任何非终态任务
#   6. 已有分区表 → PARTITION_TABLE_PRESENT；建分区的对齐/最小/越界/落在表内/名称过长
#   7. 签名：整盘 mkfs.ext4（首扇区全零但盘上有文件系统）与整盘 mkswap（魔数在第一个
#      页面的末 10 字节）都被拒；LUKS 头（有 cryptsetup 时）
#   8. 占用的四类拒绝：挂载、活动交换区、dm holders、进程打开设备；整盘被打开时
#      分区动作也拒（父盘占用要传递）
#   9. 身份：分区指纹是 sha256 且由整盘身份 + 起点决定，拓扑变化后不变，不同分区不同
#  10. 权限：匿名 401、viewer 403（预检与提交两处）
#  11. 回归：拓扑/设备详情/概览/任务列表在加了存储动作之后仍然正常；日志里没有产品
#      调用 sgdisk/mkfs/wipefs/mkswap 的痕迹，也没有明文口令
#  12. 收尾：擦掉夹具后设备能力回到 available（能力跟着状态变，不是一次性结论）
#
# 明确不覆盖（NOT RUN，原因写在这里，不在脚本里假装通过）：
#   - **执行前复核（PREFLIGHT_STALE）**：四个动作没有执行器，提交在复核之前就 501 了，
#     所以“预检过期 / 目标变了”这条路径本轮无法在真机上触发。P07.2 登记执行器后补。
#   - 离线 / 只读设备：要热拔盘或拿真光盘；P04.x 已用 sysfs 夹具与 loop 覆盖只读判定。
#   - RAID（md）成员：VM 没有 mdadm 现场；holders 用 dm 现场覆盖（同一条 sysfs 路径）。
#   - 并发真竞态（两个预检同时抢同一块盘）：本阶段还没有执行器，锁的真实争用在 P03.2
#     已验；存储动作的锁字符串在 §3/§4/§6 里断言。
#   - 应用/共享/备份依赖（PLAN §9.1 第 5—7 项）：要到 P08/P11/P17 才有对象。

set -u

BIN_DIR=${BIN_DIR:-/tmp/upanel-bin}

if [ "$(id -u)" -ne 0 ]; then
  printf '本脚本必须以 root 运行：sudo BIN_DIR=%s sh %s\n' "$BIN_DIR" "$0" >&2
  exit 2
fi

AGENT="$BIN_DIR/upanel-agent"
WEB="$BIN_DIR/upanel-web"
CTL="$BIN_DIR/upanelctl"

WORK=${WORK:-/var/tmp/upanel-accept-p07-1}
PAYLOADS=$WORK/payloads
RUN_DIR=/run/upanel
SOCK=$RUN_DIR/agent.sock
SYS_CFG=/etc/upanel/upanel.json
AGENT_DB_DIR=/var/lib/upanel-agent
AGENT_DB=$AGENT_DB_DIR/agent.db
KEY_FILE=/etc/upanel/agent.key
WEB_DB_DIR=$WORK/webdata
WEB_DB=$WEB_DB_DIR/upanel.db
PORT=${PORT:-19471}
BASE="http://127.0.0.1:$PORT"
ORIGIN="$BASE"
WEB_USER=upanel-web
PASSWORD='vm-correct-horse-9'
VIEWER_PASSWORD='vm-correct-horse-8'

SPARE_DISKS=${SPARE_DISKS:-"sdb sdc sdd"}
STORE=/api/v1/storage
PREFLIGHT_PATH=/api/v1/operations/preflight
JOBS=/api/v1/jobs
AGENT_LOG=$WORK/logs/agent.log
WEB_LOG=$WORK/logs/web.log
AGENT_PID=""
WEB_PID=""
BUSY_PID=""
MNT_POINT=""
SWAP_DEV=""
DM_NAME=""

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

cleanup() {
  release_fixtures
  stop_web
  stop_agent
  return 0
}
trap cleanup EXIT
trap 'cleanup; exit 130' INT
trap 'cleanup; exit 143' TERM

release_fixtures() {
  if [ -n "$MNT_POINT" ] && mountpoint -q "$MNT_POINT" 2>/dev/null; then
    umount "$MNT_POINT" 2>/dev/null
  fi
  if [ -n "$SWAP_DEV" ]; then
    swapoff "$SWAP_DEV" 2>/dev/null
  fi
  if [ -n "$DM_NAME" ]; then
    dmsetup remove "$DM_NAME" 2>/dev/null
  fi
  if [ -n "$BUSY_PID" ] && kill -0 "$BUSY_PID" 2>/dev/null; then
    kill "$BUSY_PID" 2>/dev/null
    wait "$BUSY_PID" 2>/dev/null
  fi
  BUSY_PID=""
  return 0
}

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

# preflight <动作> <参数 JSON>：预检响应另存一份 $WORK/preflight.json，回显状态码。
# 单独存一份是为了在同一节里既断言状态码又多次读结论，不被后续请求冲掉。
preflight() {
  printf '{"action":"%s","params":%s}' "$1" "$2" >"$WORK/pre.json"
  _code=$(curl -s -b "$JAR" -o "$WORK/resp.json" -w '%{http_code}' \
    -X POST "$BASE$PREFLIGHT_PATH" \
    -H 'Content-Type: application/json' -H "Origin: $ORIGIN" -H "X-CSRF-Token: $CSRF" \
    -d @"$WORK/pre.json")
  cp "$WORK/resp.json" "$WORK/preflight.json"
  printf '%s' "$_code"
}

# device <设备 id>：把设备详情存成 $WORK/device.json。
device() { api "$JAR" "$CSRF" GET "$STORE/devices/$1" >/dev/null; cp "$WORK/resp.json" "$WORK/device.json"; }

# wait_device <设备> <python 表达式> [起始时间]：轮询设备详情，直到表达式为 True，
# **并且** 这份快照是在 <起始时间> 之后采集的（最多 30 秒）。
#
# 拓扑每 5 秒重扫一次，造完夹具立刻读会拿到夹具之前的快照；那些“本来就成立”的
# 表达式（例如 partition_table.type == "none"）会立刻为真，于是断言跑在旧快照上——
# 看起来是产品没生效，其实是测试读早了。
# devcollected：设备详情里 collected_at 的 unix 时间（浮点）。解析不出来就打印空串。
devcollected() {
  jget "$WORK/device.json" 'import datetime
s=str(d["data"]["collected_at"]).replace("Z","+00:00")
try:
    _=datetime.datetime.fromisoformat(s).timestamp()
except Exception:
    _=""'
}

# later_than <a> <b>：a 严格晚于 b 时退出码 0。
#
# 用 python 比浮点，不比字符串：Go 序列化 time 时会去掉小数秒的尾零，
# “:01.5Z” 与 “:01Z” 的字典序和真实时间序相反。
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

# settle <设备> <表达式>：造完夹具后等一份**夹具之后采集**、且满足表达式的快照，
# 失败时记成一条明确的失败断言。
settle() {
  _t0=$(date +%s)
  if wait_device "$1" "$2" "$_t0"; then
    pass "快照反映了现场（$1，采集于夹具之后）"
  else
    fail "快照反映了现场（$1）" "30 秒内没有出现夹具之后且满足「$2」的快照"
  fi
}

# 阻塞项/告警项/影响的便捷别名。
pallowed() { pget 'd["data"]["allowed"]'; }
pall() { pget '",".join(b["code"] for b in d["data"]["blockers"])'; }
pwhy() { pget '"; ".join(b["message"] for b in d["data"]["blockers"])'; }
pimpact() { pget '",".join(i["kind"] for i in d["data"]["impacts"])'; }
pimpacttext() { pget '"; ".join(i["description"] for i in d["data"]["impacts"])'; }
plocks() { pget '",".join(d["data"]["locks"])'; }
pstate() { pget 'd["data"]["target"]["display_name"]'; }
pfp() { pget 'd["data"]["target"]["identity_fingerprint"]'; }
pphrase() { pget 'd["data"]["confirmation_phrase"]'; }

# cap_state / cap_reason <设备> <动作>：设备详情里该动作的能力状态与理由。
cap_state() { devget "next(c[\"state\"] for c in d[\"data\"][\"device\"][\"capabilities\"] if c[\"action\"] == \"$2\")"; }
cap_reason() { devget "next((c.get(\"reason\") or \"\") for c in d[\"data\"][\"device\"][\"capabilities\"] if c[\"action\"] == \"$2\")"; }

# wait_cap / expect_cap <名字> <设备> <动作> <期望状态>：等能力视图收敛到期望状态。
#
# 能力视图有 30 秒缓存，键是 (设备, 快照版本)。盘上的内容变了而拓扑版本没变
# （例如整盘 mkfs 不改变分区表、签名又不在快照里）时，缓存到期前会给出旧结论。
# 这不是缺陷，是“别每 5 秒重算一遍全部判决”的代价——测试要等它过期，而不是
# 拿一次瞬时读数当结论。
wait_cap() {
  _i=0
  while [ "$_i" -lt 80 ]; do
    device "$2"
    if [ "$(cap_state "$2" "$3")" = "$4" ]; then return 0; fi
    _i=$((_i + 1))
    sleep 0.5
  done
  return 1
}
expect_cap() {
  if wait_cap "$@"; then
    pass "$1"
  else
    fail "$1" "期望 $4，实际 $(cap_state "$2" "$3")"
  fi
}

# disk_digest <设备>：头部 4 MiB + 尾部 4 MiB 的 sha256。
#
# 用它证明“允许的预检 / 501 的提交一个字节都没写”。只看头部不够：GPT 备份表在盘尾，
# swap 的魔数也在盘尾——只查头部会让一次尾部写入漏过去。
disk_digest() {
  _size=$(blockdev --getsize64 "$1" 2>/dev/null || echo 0)
  _skip=$(( _size / 1048576 - 4 ))
  [ "$_skip" -lt 0 ] && _skip=0
  { dd if="$1" bs=1M count=4 2>/dev/null; dd if="$1" bs=1M skip="$_skip" count=4 2>/dev/null; } \
    | sha256sum | cut -d' ' -f1
}

# wipe_disk <设备>：把一次性盘清成真正的空白（表、签名、首尾都清）。
#
# 只对 §0 校验过的 SPARE_DISKS 调用。清尾部是必须的：GPT 备份表与 swap 魔数都在盘尾，
# 只抹头部的“空白盘”会让本轮的签名检查变成假通过。
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
         wipefs mkswap swapon swapoff dmsetup mountpoint udevadm dd; do
  if command -v "$t" >/dev/null 2>&1; then pass "依赖命令可用 $t"; else fail "依赖命令可用 $t" "缺少 $t"; fi
done
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

SPARE_COUNT=$(printf '%s\n' $SPARE_DISKS | wc -l | tr -d ' ')
if [ "$SPARE_COUNT" -ne 3 ]; then
  echo "SPARE_DISKS 需要正好三块盘（当前 '$SPARE_DISKS'）；脚本按三块分工使用" >&2
  exit 2
fi
BLANK_A=$(printf '%s\n' $SPARE_DISKS | sed -n 1p)
BLANK_B=$(printf '%s\n' $SPARE_DISKS | sed -n 2p)
BLANK_C=$(printf '%s\n' $SPARE_DISKS | sed -n 3p)

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
  # 根直接在整盘上（没有分区层）：整盘自己就是根设备。
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
# 根设备的整条链路（向上到底、向下到子设备）；一次性盘一旦出现在里面就退出。
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

api "$JAR" "$CSRF" POST /api/v1/users \
  "{\"username\":\"viewer\",\"display_name\":\"只读\",\"role\":\"viewer\",\"initial_password\":\"$VIEWER_PASSWORD\"}" \
  >"$WORK/viewer-code.txt"
expect_eq "创建 viewer 账号" "201" "$(cat "$WORK/viewer-code.txt")"
chown_web_db
VJAR=$WORK/viewer.jar
expect_eq "viewer 登录" "200" "$(login viewer "$VIEWER_PASSWORD" "$VJAR" login-viewer)"
VCSRF=$(csrf_of "$WORK/login-viewer.json")

echo
echo "== 2. 能力真实性：能预检，但不在 supported_actions 里 =="
# 四个动作登记了参数解码与预检，但没有执行器。把它们写进 supported_actions 就是在
# 告诉前端“这个能做”，而提交一定 501——那是能力撒谎。
STORAGE_ACTIONS="storage.partition_table.initialize,storage.partition.create,storage.partition.delete,storage.filesystem.format"
expect_eq "supported_actions 里没有任何 storage.* 动作" "[]" \
  "$(jget "$PAYLOADS/agent-capabilities.json" 'sorted(a for a in d["data"]["supported_actions"] if a.startswith("storage."))')"
note "代理登记的可执行动作：$(jget "$PAYLOADS/agent-capabilities.json" 'd["data"]["supported_actions"]')"
# 但预检入口必须在：不登记就连接不到预检，用户就看不到“为什么不能碰这块盘”。
expect_eq "四个存储动作都能预检（不是未登记动作）" "200" \
  "$(preflight storage.filesystem.format "{\"device_id\":\"$ROOT_DISK\",\"filesystem\":\"ext4\"}")"

# 设备能力：四项、顺序固定、状态与判决一致。
device "$BLANK_A"
expect_eq "空白盘能力项数" "4" "$(devget 'len(d["data"]["device"]["capabilities"])')"
expect_eq "空白盘能力动作顺序" "$STORAGE_ACTIONS" \
  "$(devget '",".join(c["action"] for c in d["data"]["device"]["capabilities"])')"
expect_cap "空白盘初始化 = available" "$BLANK_A" storage.partition_table.initialize available
expect_cap "空白盘建分区 = blocked（还没有 GPT）" "$BLANK_A" storage.partition.create blocked
expect_cap "空白盘删除分区 = unsupported" "$BLANK_A" storage.partition.delete unsupported
expect_cap "空白盘整盘格式化 = unsupported" "$BLANK_A" storage.filesystem.format unsupported
expect_eq "空白盘系统盘保护为 false" "False" "$(devget 'd["data"]["device"]["protection"]["system_disk"]')"

device "$ROOT_DISK"
expect_cap "系统盘初始化 = blocked" "$ROOT_DISK" storage.partition_table.initialize blocked
expect_eq "系统盘保护为 true" "True" "$(devget 'd["data"]["device"]["protection"]["system_disk"]')"
expect_contains "系统盘能力理由里说明是系统数据" "系统" "$(cap_reason "$ROOT_DISK" storage.partition_table.initialize)"

echo
echo "== 3. 系统盘与它的分区：一律拒绝 =="
expect_eq "系统盘初始化的预检状态码" "200" \
  "$(preflight storage.partition_table.initialize "{\"device_id\":\"$ROOT_DISK\",\"table_type\":\"gpt\"}")"
expect_eq "系统盘初始化 allowed" "False" "$(pallowed)"
expect_contains "系统盘初始化的阻塞码" "SYSTEM_DISK_PROTECTED" "$(pall)"
expect_contains "系统盘初始化的理由说清承载系统数据" "系统数据" "$(pwhy)"
expect_eq "系统盘初始化的锁是整盘" "disk:$ROOT_DISK" "$(plocks)"
cp "$WORK/preflight.json" "$PAYLOADS/preflight-system-disk.json"

# 根分区（以及同盘的 /boot 等分区）上的格式化与删除同样拒绝。
ROOT_PART=$(basename "$ROOT_REAL")
if [ "$ROOT_PART" != "$ROOT_DISK" ]; then
  expect_eq "根分区格式化的预检状态码" "200" \
    "$(preflight storage.filesystem.format "{\"device_id\":\"$ROOT_PART\",\"filesystem\":\"ext4\",\"label\":\"root\"}")"
  expect_eq "根分区格式化 allowed" "False" "$(pallowed)"
  expect_contains "根分区格式化的阻塞码" "SYSTEM_DISK_PROTECTED" "$(pall)"
  expect_eq "根分区格式化的锁仍是整盘" "disk:$ROOT_DISK" "$(plocks)"

  expect_eq "根分区删除的预检状态码" "200" \
    "$(preflight storage.partition.delete "{\"device_id\":\"$ROOT_PART\"}")"
  expect_eq "根分区删除 allowed" "False" "$(pallowed)"
  expect_contains "根分区删除的阻塞码" "SYSTEM_DISK_PROTECTED" "$(pall)"
else
  note "根在整盘上，没有分区层可测"
fi
# 同一块盘上的每个分区都要被保护（保护沿 partition_of 向上递归）。
# 这里必须加 -l（列表模式）去掉树形前缀：默认输出会在 NAME 列前面画 “├─”，
# 于是 `^sda[0-9]+$` 一条也匹配不上——整节会“什么都没断言就报通过”。
# 注意 -r 与 -l 互斥（`lsblk: mutually exclusive arguments`），别叠着写。
PROTECTED_PARTS=0
for _p in $(lsblk -lno NAME,TYPE "/dev/$ROOT_DISK" 2>/dev/null | awk '$2 == "part" {print $1}'); do
  _p=$(basename "$_p")
  preflight storage.partition.delete "{\"device_id\":\"$_p\"}" >/dev/null
  expect_contains "同盘分区 $_p 的删除被保护" "SYSTEM_DISK_PROTECTED" "$(pall)"
  PROTECTED_PARTS=$((PROTECTED_PARTS + 1))
done
if [ "$PROTECTED_PARTS" -gt 0 ]; then
  pass "系统盘上一共断言了 $PROTECTED_PARTS 个分区的保护"
else
  notrun "系统盘上各分区的保护" "这块盘上没有分区（整盘承载 /），无从断言"
fi

echo
echo "== 4. 空白盘初始化：允许、影响、整盘锁、短语 =="
BEFORE=$(disk_digest "/dev/$BLANK_A")
expect_eq "空白盘初始化的预检状态码" "200" \
  "$(preflight storage.partition_table.initialize "{\"device_id\":\"$BLANK_A\",\"table_type\":\"gpt\"}")"
expect_eq "空白盘初始化 allowed" "True" "$(pallowed)"
expect_eq "空白盘初始化没有阻塞项" "" "$(pall)"
expect_eq "空白盘初始化的锁" "disk:$BLANK_A" "$(plocks)"
expect_eq "空白盘初始化的影响" "data_loss,partition_table_change" "$(pimpact)"
expect_contains "影响里写明未检测到签名不代表没有数据" "未检测到已知签名不代表盘上绝无原始数据" "$(pimpacttext)"
expect_contains "影响里写明覆盖分区元数据" "覆盖盘上的分区元数据" "$(pimpacttext)"
expect_eq "空白盘初始化要求再认证" "True" "$(pget 'd["data"]["requires_reauth"]')"
expect_eq "空白盘初始化不可取消" "False" "$(pget 'd["data"]["cancellable"]')"
expect_contains "短语首词是 INIT" "INIT " "$(pphrase)"
_pfp=$(pfp)
if [ -n "$_pfp" ] && [ "$_pfp" != "None" ]; then pass "预检绑定了非空身份指纹"; else fail "预检绑定了非空身份指纹" "$_pfp"; fi
note "目标：$(pstate)"
note "指纹：$_pfp"
cp "$WORK/preflight.json" "$PAYLOADS/preflight-blank-allow.json"
AFTER=$(disk_digest "/dev/$BLANK_A")
expect_eq "允许的预检没有写盘（首尾 4 MiB 哈希不变）" "$BEFORE" "$AFTER"

echo
echo "== 5. 提交：未再认证 403、再认证后 501（只留一条 failed 痕迹） =="
PF_ALLOWED=$(pget 'd["data"]["preflight_id"]')
PHRASE_ALLOWED=$(pphrase)
api "$JAR" "$CSRF" GET "$JOBS" >/dev/null
JOBS_BEFORE=$(resp_get 'len(d["data"]["jobs"])')
printf '{"preflight_id":"%s","confirmation_phrase":"%s"}' "$PF_ALLOWED" "$PHRASE_ALLOWED" >"$WORK/jobreq.json"

# 5.1 门在代理之前：存储动作一律要求 5 分钟内再认证。这条拒绝由 Web 判定，
#     所以盘上的字节一个都不能变——也顺带证明“允许的预检”不会被一次提交变成写操作。
SUB_NOREAUTH=$(curl -s -b "$JAR" -o "$WORK/resp.json" -w '%{http_code}' -X POST "$BASE$JOBS" \
  -H 'Content-Type: application/json' -H "Origin: $ORIGIN" -H "X-CSRF-Token: $CSRF" \
  -H "Idempotency-Key: p07-1-submit-0" -d @"$WORK/jobreq.json")
cp "$WORK/resp.json" "$PAYLOADS/job-reauth-required.json"
expect_eq "未再认证就提交 → 403" "403" "$SUB_NOREAUTH"
expect_eq "未再认证的错误码" "REAUTH_REQUIRED" "$(resp_get 'd["error"]["code"]')"
api "$JAR" "$CSRF" GET "$JOBS" >/dev/null
expect_eq "再认证拒绝没有留下任务" "$JOBS_BEFORE" "$(resp_get 'len(d["data"]["jobs"])')"
expect_eq "再认证拒绝没有写盘" "$BEFORE" "$(disk_digest "/dev/$BLANK_A")"

# 5.2 再认证之后同一个预检可以直接用：仍然 501（没有执行器），
#     不能因为过了门就悄悄变成一次真实的写操作。
expect_eq "再认证" "200" "$(api "$JAR" "$CSRF" POST /api/v1/auth/reauth "{\"password\":\"$PASSWORD\"}")"
SUB_CODE=$(curl -s -b "$JAR" -o "$WORK/resp.json" -w '%{http_code}' -X POST "$BASE$JOBS" \
  -H 'Content-Type: application/json' -H "Origin: $ORIGIN" -H "X-CSRF-Token: $CSRF" \
  -H "Idempotency-Key: p07-1-submit-1" -d @"$WORK/jobreq.json")
cp "$WORK/resp.json" "$PAYLOADS/job-501.json"
expect_eq "提交存储动作的状态码" "501" "$SUB_CODE"
expect_eq "提交的错误码" "NOT_IMPLEMENTED" "$(resp_get 'd["error"]["code"]')"
api "$JAR" "$CSRF" GET "$JOBS" >/dev/null
cp "$WORK/resp.json" "$PAYLOADS/jobs-after-501.json"
# Q159：受理即拒的提交**保留**一条 failed/rejected 任务（那是“刚才那次点击到底发生了什么”
# 的唯一凭据）。所以这里断言的不是“没有任务”，而是“只多了一条、而且它已经是终态”——
# 绝不允许留下一条永远跑不起来、却在排队等执行的任务。
expect_eq "被拒的提交只新增一条任务" "$((JOBS_BEFORE + 1))" "$(resp_get 'len(d["data"]["jobs"])')"
expect_eq "这条任务的 action 是初始化" "storage.partition_table.initialize" \
  "$(resp_get '[j for j in d["data"]["jobs"] if j["action"]=="storage.partition_table.initialize"][0]["action"]')"
expect_eq "这条任务停在 rejected 阶段" "rejected" \
  "$(resp_get '[j for j in d["data"]["jobs"] if j["action"]=="storage.partition_table.initialize"][0]["step"]')"
expect_eq "这条任务的状态是 failed" "failed" \
  "$(resp_get '[j for j in d["data"]["jobs"] if j["action"]=="storage.partition_table.initialize"][0]["status"]')"
expect_eq "这条任务的错误码是 NOT_IMPLEMENTED" "NOT_IMPLEMENTED" \
  "$(resp_get '[j for j in d["data"]["jobs"] if j["action"]=="storage.partition_table.initialize"][0]["error"]["code"]')"
expect_eq "没有非终态任务（不留永远跑不起来的排队）" "0" \
  "$(resp_get 'len([j for j in d["data"]["jobs"] if j["status"] not in ("failed","succeeded","canceled")])')"
expect_eq "磁盘内容在 501 之后仍未变" "$BEFORE" "$(disk_digest "/dev/$BLANK_A")"

echo
echo "== 6. 有分区表的盘：初始化拒绝、建分区的对齐与越界 =="
sgdisk --clear --new=1:0:+512M "/dev/$BLANK_A" >/dev/null 2>&1
udevadm settle --timeout=5 >/dev/null 2>&1
settle "$BLANK_A" 'd["data"]["device"]["partition_table"]["type"] == "gpt"'
expect_cap "有 GPT 时初始化 = blocked" "$BLANK_A" storage.partition_table.initialize blocked
expect_cap "有 GPT 时建分区 = available" "$BLANK_A" storage.partition.create available
expect_eq "有 GPT 时初始化预检状态码" "200" \
  "$(preflight storage.partition_table.initialize "{\"device_id\":\"$BLANK_A\",\"table_type\":\"gpt\"}")"
expect_contains "已有分区表的阻塞码" "PARTITION_TABLE_PRESENT" "$(pall)"
expect_eq "已有分区表时 allowed" "False" "$(pallowed)"

# 空闲区起点：取设备详情里**最大**的那块空闲区。
# 不能取 free_regions[0]：GPT 之后第一块空闲区常常是 34 扇区之后到 1 MiB 对齐之间的
# 那道缝（约 1 MiB - 17 KiB），起点没有对齐、也放不下一个分区。取它会把“本来就该被拒”
# 的区间当成“合法区间”，测试就成了假失败。
FREE_START=$(devget 'max(d["data"]["device"]["partition_table"]["free_regions"], key=lambda r: int(r["size_bytes"]))["start_bytes"]')
FREE_SIZE=$(devget 'max(d["data"]["device"]["partition_table"]["free_regions"], key=lambda r: int(r["size_bytes"]))["size_bytes"]')
case "$FREE_START" in
  '' | None | ERR:*)
    # 空、None、ERR: 都不能当数值往下传：把 "ERR:TypeError:…" 塞进 start_bytes
    # 只会换来一个 400，然后后续断言全部变成看不懂的 KeyError。
    fail "读到空闲区" "设备详情里没有最大空闲区（${FREE_START:-空}）"
    FREE_START=1048576
    ;;
  *)
    pass "读到最大的空闲区（起点 $FREE_START，大小 $FREE_SIZE）"
    ;;
esac

GOOD_SIZE=1073741824
preflight storage.partition.create \
  "{\"device_id\":\"$BLANK_A\",\"start_bytes\":\"$FREE_START\",\"size_bytes\":\"$GOOD_SIZE\",\"name\":\"data\"}" >/dev/null
expect_eq "合法区间 allowed" "True" "$(pallowed)"
expect_eq "合法区间没有阻塞项" "" "$(pall)"
cp "$WORK/preflight.json" "$PAYLOADS/preflight-create-allow.json"

BAD_START=$(( FREE_START + 512 ))
preflight storage.partition.create \
  "{\"device_id\":\"$BLANK_A\",\"start_bytes\":\"$BAD_START\",\"size_bytes\":\"$GOOD_SIZE\"}" >/dev/null
expect_contains "起点没对齐的阻塞码" "REGION_UNALIGNED" "$(pall)"
preflight storage.partition.create \
  "{\"device_id\":\"$BLANK_A\",\"start_bytes\":\"$FREE_START\",\"size_bytes\":\"524288\"}" >/dev/null
expect_contains "小于最小分区的阻塞码" "REGION_TOO_SMALL" "$(pall)"
preflight storage.partition.create \
  "{\"device_id\":\"$BLANK_A\",\"start_bytes\":\"$FREE_START\",\"size_bytes\":\"68719476736\"}" >/dev/null
expect_contains "超出容量的阻塞码" "REGION_NOT_FREE" "$(pall)"
# 与已有分区重叠：从 0 开始必然落在分区表区，不在任何空闲区里。
preflight storage.partition.create \
  "{\"device_id\":\"$BLANK_A\",\"start_bytes\":\"0\",\"size_bytes\":\"1048576\"}" >/dev/null
expect_contains "落在分区表区的阻塞码" "REGION_NOT_FREE" "$(pall)"
expect_eq "落在分区表区 allowed" "False" "$(pallowed)"
# 名称过长在解码层就被拒（400），不是预检结论。
LONGNAME=$(python3 -c 'print("x"*37)')
expect_eq "分区名过长在参数层被拒" "400" \
  "$(preflight storage.partition.create "{\"device_id\":\"$BLANK_A\",\"start_bytes\":\"$FREE_START\",\"size_bytes\":\"$GOOD_SIZE\",\"name\":\"$LONGNAME\"}")"
expect_eq "分区名过长的错误码" "VALIDATION_ERROR" "$(resp_get 'd["error"]["code"]')"
# 字节数写成 JSON number 也在解码层被拒：契约是十进制字符串（超过 2^53 会丢位）。
expect_eq "字节数写成数字被拒" "400" \
  "$(preflight storage.partition.create "{\"device_id\":\"$BLANK_A\",\"start_bytes\":$FREE_START,\"size_bytes\":\"1048576\"}")"
expect_eq "表类型 mbr 在参数层被拒" "400" \
  "$(preflight storage.partition_table.initialize "{\"device_id\":\"$BLANK_B\",\"table_type\":\"mbr\"}")"

# 删除与格式化分区：空白分区允许（这也是“该放的放了”的证据）。
PART_A="${BLANK_A}1"
if [ -b "/dev/$PART_A" ]; then
  settle "$PART_A" 'd["data"]["device"]["kind"] == "partition"'
  preflight storage.partition.delete "{\"device_id\":\"$PART_A\"}" >/dev/null
  expect_eq "空白分区删除 allowed" "True" "$(pallowed)"
  expect_eq "空白分区删除的锁是整盘" "disk:$BLANK_A" "$(plocks)"
  preflight storage.filesystem.format "{\"device_id\":\"$PART_A\",\"filesystem\":\"ext4\",\"label\":\"data\"}" >/dev/null
  expect_eq "空白分区格式化 allowed" "True" "$(pallowed)"
  # 标签按文件系统分别校验：ext4 16 字节、xfs 12 字节，超出在参数层拒绝。
  expect_eq "ext4 13 字节标签可以" "200" \
    "$(preflight storage.filesystem.format "{\"device_id\":\"$PART_A\",\"filesystem\":\"ext4\",\"label\":\"1234567890123\"}")"
  expect_eq "xfs 13 字节标签被拒" "400" \
    "$(preflight storage.filesystem.format "{\"device_id\":\"$PART_A\",\"filesystem\":\"xfs\",\"label\":\"1234567890123\"}")"
  expect_eq "支持之外的文件系统被拒" "400" \
    "$(preflight storage.filesystem.format "{\"device_id\":\"$PART_A\",\"filesystem\":\"btrfs\"}")"
  expect_eq "设备 id 带路径分隔符被拒" "400" \
    "$(preflight storage.partition.delete '{"device_id":"../etc/passwd"}')"
else
  fail "GPT 上建出了 ${PART_A}" "分区设备不存在，后续分区断言无法进行"
fi

echo
echo "== 7. 签名：首扇区全零的整盘 ext4，与写在第一页末 10 字节的 swap =="
# 直接在整盘上 mkfs：LBA0 是全零（没有分区表），但 1 KiB 处有 ext 超级块。
# 只看首扇区会把它当空白盘——这正是本轮新增签名读取的原因。
mkfs.ext4 -q -F -L p07whole "/dev/$BLANK_B" >/dev/null 2>&1
udevadm settle --timeout=5 >/dev/null 2>&1
settle "$BLANK_B" 'd["data"]["device"]["partition_table"]["type"] == "none"'
expect_eq "整盘 ext4 的初始化预检状态码" "200" \
  "$(preflight storage.partition_table.initialize "{\"device_id\":\"$BLANK_B\",\"table_type\":\"gpt\"}")"
expect_eq "整盘 ext4 的初始化 allowed" "False" "$(pallowed)"
expect_contains "整盘 ext4 的阻塞码" "FILESYSTEM_SIGNATURE_PRESENT" "$(pall)"
expect_contains "理由里点出 ext 系列签名" "ext 系列文件系统签名" "$(pwhy)"
expect_cap "整盘 ext4 时初始化能力 = blocked" "$BLANK_B" storage.partition_table.initialize blocked
cp "$WORK/preflight.json" "$PAYLOADS/preflight-signature-ext4.json"

# swap 的魔数在**第一个页面的末 10 字节**（4096-10 = 4086），不是盘尾：
# 2026-10-21 在验收机上实测 mkswap 之后 SWAPSPACE2 在偏移 4086、盘尾 64 KiB 全零，
# wipefs 报的偏移也是 0xff6。只看盘尾会把交换区当成“无法识别的签名”。
mkswap -f -L p07swap "/dev/$BLANK_C" >/dev/null 2>&1
udevadm settle --timeout=5 >/dev/null 2>&1
settle "$BLANK_C" 'd["data"]["device"]["partition_table"]["type"] == "none"'
expect_eq "整盘 swap 的初始化预检状态码" "200" \
  "$(preflight storage.partition_table.initialize "{\"device_id\":\"$BLANK_C\",\"table_type\":\"gpt\"}")"
expect_eq "整盘 swap 的初始化 allowed" "False" "$(pallowed)"
expect_contains "整盘 swap 的阻塞码" "FILESYSTEM_SIGNATURE_PRESENT" "$(pall)"
expect_contains "理由里点出交换区签名" "交换区签名" "$(pwhy)"
cp "$WORK/preflight.json" "$PAYLOADS/preflight-signature-swap.json"

if command -v cryptsetup >/dev/null 2>&1; then
  wipe_disk "/dev/$BLANK_B"
  if printf 'p07-not-a-secret\n' | cryptsetup luksFormat --batch-mode --type luks2 -q \
       --pbkdf pbkdf2 --pbkdf-force-iterations 1000 --key-file - "/dev/$BLANK_B" >/dev/null 2>&1; then
    udevadm settle --timeout=5 >/dev/null 2>&1
    # LUKS 头就在 LBA0 开头（"LUKS\xba\xbe"），所以首扇区既不是 GPT/MBR、也不是全零，
    # 分区表类型如实变成 unknown——这正是本条要等的“夹具之后的新快照”。
    settle "$BLANK_B" 'd["data"]["device"]["partition_table"]["type"] == "unknown"'
    preflight storage.partition_table.initialize "{\"device_id\":\"$BLANK_B\",\"table_type\":\"gpt\"}" >/dev/null
    expect_eq "LUKS 头的初始化 allowed" "False" "$(pallowed)"
    expect_contains "LUKS 头的阻塞码" "FILESYSTEM_SIGNATURE_PRESENT" "$(pall)"
    expect_contains "理由里点出 LUKS" "LUKS" "$(pwhy)"
    cp "$WORK/preflight.json" "$PAYLOADS/preflight-signature-luks.json"
  else
    notrun "LUKS 头拒绝" "cryptsetup 存在但 luksFormat 失败，没造出真实的 LUKS 现场"
  fi
else
  notrun "LUKS 头拒绝" "VM 上没有 cryptsetup（单元用例覆盖 LUKS1/2 魔数，真机现场未造）"
fi

echo
echo "== 8. 四类占用：挂载、活动交换区、dm holders、进程打开 =="
wipe_disk "/dev/$BLANK_B"
wipe_disk "/dev/$BLANK_C"
sgdisk --clear --new=1:0:+1024M --new=2:0:+512M "/dev/$BLANK_C" >/dev/null 2>&1
udevadm settle --timeout=5 >/dev/null 2>&1
PART_C1="${BLANK_C}1"
PART_C2="${BLANK_C}2"
# 两级等待：先等**整盘**的分区表与空闲区出现在快照里，再等分区本身出现。
# 只等 kind == "partition" 会在“分区已可见、但整盘的分区表还是造 GPT 之前那份”的
# 瞬间返回，于是下面从 device.json 取空闲区取到的是上一份快照（甚至取错设备）。
settle "$BLANK_C" 'd["data"]["device"]["partition_table"]["type"] == "gpt" and len(d["data"]["device"]["partition_table"]["free_regions"] or []) > 0'
settle "$PART_C1" 'd["data"]["device"]["kind"] == "partition"'
# 同样取最大的空闲区：GPT 之后那道未对齐的缝不是可用的建分区起点。
# 注意这里要重新读**整盘**的详情：上一句 device.json 里是分区。
device "$BLANK_C"
FREE_START_C=$(devget 'max(d["data"]["device"]["partition_table"]["free_regions"], key=lambda r: int(r["size_bytes"]))["start_bytes"]')
case "$FREE_START_C" in
  '' | None | ERR:*)
    fail "读到整盘的最大空闲区" "设备详情里没有可用空闲区（${FREE_START_C:-空}）"
    FREE_START_C=""
    ;;
esac

# (1) 挂载
mkfs.ext4 -q -F "/dev/$PART_C1" >/dev/null 2>&1
MNT_POINT=$WORK/mnt
mkdir -p "$MNT_POINT"
if mount "/dev/$PART_C1" "$MNT_POINT" 2>/dev/null; then
  # filesystem 可能整体为 null（设备没有挂载点、没有文件系统信息时是 null），
  # 直接 d["filesystem"]["mounts"] 会抛 TypeError，把“还没扫到”误当成“扫到了”。
  settle "$PART_C1" 'len((d["data"]["device"].get("filesystem") or {}).get("mounts") or []) > 0'
  preflight storage.filesystem.format "{\"device_id\":\"$PART_C1\",\"filesystem\":\"xfs\"}" >/dev/null
  expect_eq "挂载中的分区格式化 allowed" "False" "$(pallowed)"
  expect_contains "挂载中的阻塞码" "DEVICE_MOUNTED" "$(pall)"
  expect_contains "阻塞理由带上挂载点" "$MNT_POINT" "$(pwhy)"
  cp "$WORK/preflight.json" "$PAYLOADS/preflight-mounted.json"
  if umount "$MNT_POINT"; then pass "卸载 $MNT_POINT"; else fail "卸载 $MNT_POINT" "umount 失败，后面的断言会基于挂载状态"; fi
  MNT_POINT=""
  settle "$PART_C1" 'len((d["data"]["device"].get("filesystem") or {}).get("mounts") or []) == 0'
  preflight storage.filesystem.format "{\"device_id\":\"$PART_C1\",\"filesystem\":\"xfs\"}" >/dev/null
  expect_eq "卸载后格式化 allowed" "True" "$(pallowed)"
else
  fail "挂载 $PART_C1" "mount 失败，挂载类断言无法进行"
  MNT_POINT=""
fi

# (2) 活动交换区
SWAP_DEV="/dev/$PART_C2"
mkswap -f -L p07cnt "$SWAP_DEV" >/dev/null 2>&1
if swapon "$SWAP_DEV" 2>/dev/null; then
  settle "$PART_C2" 'bool(d["data"]["device"].get("swap"))'
  preflight storage.partition.delete "{\"device_id\":\"$PART_C2\"}" >/dev/null
  expect_eq "活动交换区的删除 allowed" "False" "$(pallowed)"
  expect_contains "活动交换区的阻塞码" "SWAP_ACTIVE" "$(pall)"
  cp "$WORK/preflight.json" "$PAYLOADS/preflight-swap.json"
  swapoff "$SWAP_DEV"
  SWAP_DEV=""
  settle "$PART_C2" 'not d["data"]["device"].get("swap")'
  preflight storage.partition.delete "{\"device_id\":\"$PART_C2\"}" >/dev/null
  expect_eq "swapoff 之后删除 allowed" "True" "$(pallowed)"
else
  fail "swapon $SWAP_DEV" "swapon 失败，交换区断言无法进行"
  SWAP_DEV=""
fi

# (3) dm holders：在 sdc1 上盖一层 dm，sdc1 的 sysfs holders 会非空。
DM_NAME=upanel-p07-holder
DM_SIZE=$(blockdev --getsz "/dev/$PART_C1" 2>/dev/null || echo 0)
if [ "$DM_SIZE" -gt 0 ] && \
   printf '0 %s linear /dev/%s 0\n' "$DM_SIZE" "$PART_C1" | dmsetup create "$DM_NAME" 2>/dev/null; then
  settle "$PART_C1" 'len(d["data"]["device"].get("holders") or []) > 0'
  preflight storage.filesystem.format "{\"device_id\":\"$PART_C1\",\"filesystem\":\"ext4\"}" >/dev/null
  expect_eq "有 dm holder 的分区格式化 allowed" "False" "$(pallowed)"
  expect_contains "有 dm holder 的阻塞码" "DEVICE_HAS_HOLDERS" "$(pall)"
  cp "$WORK/preflight.json" "$PAYLOADS/preflight-holders.json"
  dmsetup remove "$DM_NAME"
  DM_NAME=""
  settle "$PART_C1" 'len(d["data"]["device"].get("holders") or []) == 0'
else
  fail "dmsetup 造 holder" "无法在 $PART_C1 上创建 dm 设备"
  DM_NAME=""
fi

# (4) 进程打开设备：占用检查按设备号读 /proc/*/fd，不是比路径。
python3 - "/dev/$PART_C1" <<'PY' &
import sys, time
f = open(sys.argv[1], "rb", buffering=0)
time.sleep(60)
PY
BUSY_PID=$!
for _ in $(seq 1 40); do
  [ -n "$(ls -l "/proc/$BUSY_PID/fd" 2>/dev/null | grep -F "/dev/$PART_C1")" ] && break
  sleep 0.25
done
if [ -n "$(ls -l "/proc/$BUSY_PID/fd" 2>/dev/null | grep -F "/dev/$PART_C1")" ]; then
  pass "夹具进程已打开 /dev/$PART_C1（pid=$BUSY_PID）"
  preflight storage.filesystem.format "{\"device_id\":\"$PART_C1\",\"filesystem\":\"ext4\"}" >/dev/null
  expect_eq "被进程打开的分区格式化 allowed" "False" "$(pallowed)"
  expect_contains "被进程打开时的阻塞码" "DEVICE_BUSY" "$(pall)"
  expect_contains "阻塞理由带上 pid" "$BUSY_PID" "$(pwhy)"
  cp "$WORK/preflight.json" "$PAYLOADS/preflight-busy.json"
  kill "$BUSY_PID" 2>/dev/null
  wait "$BUSY_PID" 2>/dev/null
  BUSY_PID=""
  preflight storage.filesystem.format "{\"device_id\":\"$PART_C1\",\"filesystem\":\"ext4\"}" >/dev/null
  expect_eq "进程退出后格式化 allowed" "True" "$(pallowed)"
else
  fail "夹具进程打开 /dev/$PART_C1" "没能确认 fd 指向设备"
  kill "$BUSY_PID" 2>/dev/null
  BUSY_PID=""
fi

# 整盘被打开时，分区动作也要拒：分区表的写入落在整盘上，父盘占用必须传递。
python3 - "/dev/$BLANK_C" <<'PY' &
import sys, time
f = open(sys.argv[1], "rb", buffering=0)
time.sleep(60)
PY
BUSY_PID=$!
for _ in $(seq 1 40); do
  [ -n "$(ls -l "/proc/$BUSY_PID/fd" 2>/dev/null | grep -F "/dev/$BLANK_C")" ] && break
  sleep 0.25
done
if [ -n "$(ls -l "/proc/$BUSY_PID/fd" 2>/dev/null | grep -F "/dev/$BLANK_C")" ]; then
  pass "夹具进程已打开整盘 /dev/$BLANK_C（pid=$BUSY_PID）"
  preflight storage.filesystem.format "{\"device_id\":\"$PART_C1\",\"filesystem\":\"ext4\"}" >/dev/null
  expect_eq "整盘被打开时对其分区的格式化 allowed" "False" "$(pallowed)"
  expect_contains "整盘占用传递到分区的阻塞码" "DEVICE_BUSY" "$(pall)"
  cp "$WORK/preflight.json" "$PAYLOADS/preflight-parent-busy.json"
  if [ -n "$FREE_START_C" ] && [ "$FREE_START_C" != "None" ]; then
    preflight storage.partition.create \
      "{\"device_id\":\"$BLANK_C\",\"start_bytes\":\"$FREE_START_C\",\"size_bytes\":\"1048576\"}" >/dev/null
    expect_eq "整盘被打开时建分区 allowed" "False" "$(pallowed)"
    expect_contains "整盘被打开时建分区的阻塞码" "DEVICE_BUSY" "$(pall)"
  else
    notrun "整盘被打开时建分区" "这块盘上没有空闲区可以发起建分区请求"
  fi
  kill "$BUSY_PID" 2>/dev/null
  wait "$BUSY_PID" 2>/dev/null
  BUSY_PID=""
else
  fail "夹具进程打开整盘 /dev/$BLANK_C" "没能确认 fd 指向设备"
  kill "$BUSY_PID" 2>/dev/null
  BUSY_PID=""
fi

echo
echo "== 9. 设备身份：分区指纹由整盘身份 + 起点决定 =="
expect_eq "分区格式化预检状态码" "200" \
  "$(preflight storage.filesystem.format "{\"device_id\":\"$PART_C1\",\"filesystem\":\"ext4\"}")"
PF_FP1=$(pfp)
case "$PF_FP1" in
  sha256:*) pass "分区身份指纹是 sha256: 形态" ;;
  *) fail "分区身份指纹是 sha256: 形态" "$PF_FP1" ;;
esac
cp "$WORK/preflight.json" "$PAYLOADS/preflight-identity-1.json"

preflight storage.filesystem.format "{\"device_id\":\"$PART_C1\",\"filesystem\":\"ext4\"}" >/dev/null
expect_eq "同一台设备两次预检的身份指纹一致" "$PF_FP1" "$(pfp)"

# 同一个整盘上的两个分区必须指纹不同：否则“删除 sdc1”和“删除 sdc2”绑的是同一个身份。
preflight storage.partition.delete "{\"device_id\":\"$PART_C2\"}" >/dev/null
PF_FP_C2=$(pfp)
if [ "$PF_FP_C2" != "$PF_FP1" ]; then
  pass "同盘不同分区的身份指纹不同"
else
  fail "同盘不同分区的身份指纹不同" "两个分区都得到 $PF_FP1"
fi

# 拓扑变化：在整盘上再加一个分区（宿主侧夹具）。分区 1 的起点没变 → 指纹必须不变，
# 否则“动了隔壁分区”就会让一个正当的预检失效。
sgdisk --new=3:0:+256M "/dev/$BLANK_C" >/dev/null 2>&1
udevadm settle --timeout=5 >/dev/null 2>&1
settle "$BLANK_C" 'len(d["data"]["device"]["partition_table"]["partitions"]) == 3'
preflight storage.filesystem.format "{\"device_id\":\"$PART_C1\",\"filesystem\":\"ext4\"}" >/dev/null
cp "$WORK/preflight.json" "$PAYLOADS/preflight-identity-2.json"
expect_eq "拓扑变化后分区身份指纹不变" "$PF_FP1" "$(pfp)"
note "预检响应不向前端暴露 state_version（内部绑定信息），这里只能验证身份稳定；"
note "“版本变了 → 提交时 PREFLIGHT_STALE”要等 P07.2 有了执行器才能验。"
notrun "执行前复核（PREFLIGHT_STALE）" "四个动作没有执行器，提交在复核之前就 501；P07.2 登记执行器后补"

echo
echo "== 10. 权限：匿名 401、viewer 403 =="
expect_eq "匿名预检 401" "401" \
  "$(curl -s -o "$WORK/resp.json" -w '%{http_code}' -X POST "$BASE$PREFLIGHT_PATH" \
     -H 'Content-Type: application/json' -H "Origin: $ORIGIN" \
     -d "{\"action\":\"storage.partition.delete\",\"params\":{\"device_id\":\"$PART_C1\"}}")"
expect_eq "匿名提交 401" "401" \
  "$(curl -s -o "$WORK/resp.json" -w '%{http_code}' -X POST "$BASE$JOBS" \
     -H 'Content-Type: application/json' -H "Origin: $ORIGIN" \
     -H "Idempotency-Key: p07-1-anon" -d '{"preflight_id":"pf_00000000000000000000000000000000"}')"
printf '{"action":"%s","params":%s}' storage.partition.delete \
  "{\"device_id\":\"$PART_C1\"}" >"$WORK/pre.json"
expect_eq "viewer 预检 403" "403" \
  "$(curl -s -b "$VJAR" -o "$WORK/resp.json" -w '%{http_code}' -X POST "$BASE$PREFLIGHT_PATH" \
     -H 'Content-Type: application/json' -H "Origin: $ORIGIN" -H "X-CSRF-Token: $VCSRF" \
     -d @"$WORK/pre.json")"
expect_eq "viewer 预检的错误码" "FORBIDDEN" "$(resp_get 'd["error"]["code"]')"
expect_eq "viewer 提交 403" "403" \
  "$(curl -s -b "$VJAR" -o "$WORK/resp.json" -w '%{http_code}' -X POST "$BASE$JOBS" \
     -H 'Content-Type: application/json' -H "Origin: $ORIGIN" -H "X-CSRF-Token: $VCSRF" \
     -H "Idempotency-Key: p07-1-viewer" -d '{"preflight_id":"pf_00000000000000000000000000000000"}')"
# viewer 能看设备树（权限 V），只是不能预检写动作。
expect_eq "viewer 能读设备详情" "200" \
  "$(curl -s -b "$VJAR" -o "$WORK/resp.json" -w '%{http_code}' "$BASE$STORE/devices/$BLANK_A" -H "Origin: $ORIGIN")"

echo
echo "== 11. 回归与日志卫生 =="
expect_eq "存储拓扑仍可读" "200" "$(api "$JAR" "$CSRF" GET "$STORE/topology")"
expect_eq "拓扑里设备数 > 0" "True" "$(resp_get 'len(d["data"]["nodes"]) > 0')"
expect_eq "设备详情 200" "200" "$(api "$JAR" "$CSRF" GET "$STORE/devices/$BLANK_A")"
expect_eq "未知设备 404" "404" "$(api "$JAR" "$CSRF" GET "$STORE/devices/nosuchdisk")"
expect_eq "系统概览仍可读" "200" "$(api "$JAR" "$CSRF" GET /api/v1/system/summary)"
expect_eq "任务列表仍可读" "200" "$(api "$JAR" "$CSRF" GET "$JOBS")"

# 日志卫生：产品的预检不应该调用任何分区/格式化命令。夹具命令是脚本自己跑的，
# 所以这里查的是“代理与 Web 的日志里没有出现这些命令名”。
if grep -Eq 'sgdisk|mkfs\.|wipefs|mkswap|dmsetup create' "$AGENT_LOG" 2>/dev/null; then
  fail "代理日志里没有磁盘命令" "$(grep -Eo 'sgdisk|mkfs\.[a-z0-9]+|wipefs|mkswap|dmsetup create' "$AGENT_LOG" | head -3 | tr '\n' ' ')"
else
  pass "代理日志里没有磁盘命令"
fi
if grep -Eq 'sgdisk|mkfs\.|wipefs|mkswap' "$WEB_LOG" 2>/dev/null; then
  fail "Web 日志里没有磁盘命令" "$(grep -Eo 'sgdisk|mkfs\.[a-z0-9]+|wipefs|mkswap' "$WEB_LOG" | head -3 | tr '\n' ' ')"
else
  pass "Web 日志里没有磁盘命令"
fi
for f in "$AGENT_LOG" "$WEB_LOG"; do
  if grep -q "$PASSWORD" "$f" 2>/dev/null; then
    fail "日志里没有明文口令 $(basename "$f")" "found"
  else
    pass "日志里没有明文口令 $(basename "$f")"
  fi
done

echo
echo "== 12. 收尾：擦掉夹具，能力跟着状态回到 available =="
release_fixtures
wipe_disk "/dev/$BLANK_C"
wipe_disk "/dev/$BLANK_B"
wipe_disk "/dev/$BLANK_A"
settle "$BLANK_A" 'd["data"]["device"]["partition_table"]["type"] == "none"'
expect_cap "擦回空白后初始化能力 = available" "$BLANK_A" storage.partition_table.initialize available
expect_eq "擦回空白后再次允许初始化" "200" \
  "$(preflight storage.partition_table.initialize "{\"device_id\":\"$BLANK_A\",\"table_type\":\"gpt\"}")"
expect_eq "擦回空白后 allowed" "True" "$(pallowed)"
lsblk -o NAME,TYPE,SIZE,FSTYPE,MOUNTPOINTS >"$WORK/logs/lsblk-final.txt" 2>&1
swapon --show >"$WORK/logs/swapon-final.txt" 2>&1
dmsetup ls >"$WORK/logs/dmsetup-final.txt" 2>&1
LEFTOVER_SWAP=""
for d in $SPARE_DISKS; do
  if grep -q "/dev/$d" "$WORK/logs/swapon-final.txt" 2>/dev/null; then LEFTOVER_SWAP="$LEFTOVER_SWAP $d"; fi
done
if [ -z "$LEFTOVER_SWAP" ]; then
  pass "收尾后没有遗留 swap"
else
  fail "收尾后没有遗留 swap" "$LEFTOVER_SWAP"
fi
if grep -qE "upanel-p07-holder" "$WORK/logs/dmsetup-final.txt" 2>/dev/null; then
  fail "收尾后没有遗留 dm 设备" "见 $WORK/logs/dmsetup-final.txt"
else
  pass "收尾后没有遗留 dm 设备"
fi

echo
echo "== 汇总 =="
printf 'PASS %s / FAIL %s / NOT RUN %s\n' "$PASSED" "$FAILED" "$NOTRUN"
[ -z "$FAILED_NAMES" ] || printf '失败项：%s\n' "$FAILED_NAMES"
[ "$FAILED" -eq 0 ] || exit 1
exit 0
