#!/bin/sh
# P16.5 收尾：**DISK-02 —— 根盘在 LVM / RAID 下的递归保护**（真机读数）。
#
# STAGES 条款：| DISK-02 | 根盘在 LVM/RAID 下 | 递归保护，不能破坏任何底层盘 | P04/P07 |
#
# 这条从 P00 起一直只是"部分"：矩阵里已有的结论全部出自"根在普通分区上"的机器
# （sda1 → sda），"根在**叠层**设备上"这个现场一次都没造过。本脚本要求根文件系统
# 真的落在 LVM 的 LV 或软 RAID 的 md 设备上，否则 exit 2 —— 不在分区根的机器上冒充读数。
#
# 两个现场（都在 legacy BIOS 启动、`/boot` 在根设备里、没有独立 ESP）：
#   - LVM（P00-VM-C）：sda1 = 1 MiB bios_grub（不装文件系统），sda2 = VG upanel-vg 的 PV，
#                     LV root 挂 /；物理盘上除 bios_grub 之外什么都没有。
#   - RAID（P00-VM-D）：sda1 / sdb1 = 各 1 MiB bios_grub，sda2 + sdb2 = md0（RAID1）挂 /。
# 两者共同点：**底层整盘自己既没有文件系统，也没有挂载点**。它之所以受保护，只可能
# 是从根设备沿 partition_of / depends_on 递归上去的 —— 这正是 DISK-02 要证明的事。
#
# 断言分四层：
#   1. 现场自证：/ 在 dm(LVM)/md(RAID) 上，底层是"分区 → 整盘"，/boot 不在单独分区；
#   2. 拓扑递归：根设备、每个成员分区、每块物理盘都 system_disk=true，且
#      **它们的 protection.sources 只有根设备**（标记是往上递的，不是谁自己挂了什么）；
#   3. 拒绝贯穿：整盘初始化、成员分区删除/格式化、根设备格式化四条预检全部
#      SYSTEM_DISK_PROTECTED，锁是**每一块**底层盘（RAID 时两把，不是一把）；
#   4. 收窄验证：一块真正空白的备用盘仍然 available —— 递归没有把整台机全标红。
#   另外用前后哈希证明这一轮**一个字节都没写**，并且写原语确实接上了（拒绝不是缺工具）。
#
# 用法（root）：
#   sudo BIN_DIR=/var/tmp/upanel-bin SPARE_DISK=sdb sh tests/integration/p16-5-root-disk-recursion.sh
#
# 环境变量：PORT（默认 19495）、WORK、BIN_DIR、SPARE_DISK（默认 sdb，必须是一块
# 与根链无关的空整盘；用来做"递归没有过宽"的反面对照）。
#
# 退出码：0 全部通过；1 有失败项；2 环境不满足（非 root / 根不在 LVM 或 md 上 / 备用盘不安全）。
#
# 明确不验（NOT RUN，不在这里假装通过）：
#   - **降级/重建中的 RAID**：成员盘 faulted、rebuild 进行中的现场没造。递归逻辑不看
#     阵列健康，但没有真机读数就不算验过。
#   - **根在 LVM/RAID 上写满（ENOSPC）**：P16.4 的 ENOSPC 现场是独立 tmpfs；把根写满
#     会伤到这台机的系统盘，不做。
#   - **执行期复核（PREFLIGHT_STALE / DEVICE_IDENTITY_CHANGED）**：预检就拒了，提交门
#     到不了；那两条由 P07.2 在一次性盘上验，与根在不在叠层设备上无关。

set -u

BIN_DIR=${BIN_DIR:-/var/tmp/upanel-bin}

if [ "$(id -u)" -ne 0 ]; then
  printf '本脚本必须以 root 运行：sudo BIN_DIR=%s sh %s\n' "$BIN_DIR" "$0" >&2
  exit 2
fi

AGENT="$BIN_DIR/upanel-agent"
WEB="$BIN_DIR/upanel-web"
CTL="$BIN_DIR/upanelctl"

for f in "$AGENT" "$WEB" "$CTL"; do
  if [ ! -x "$f" ]; then
    printf '缺少可执行文件：%s\n' "$f" >&2
    exit 2
  fi
done

WORK=${WORK:-/var/tmp/upanel-accept-p16-5}
LOGS=$WORK/logs
RUN_DIR=/run/upanel
SOCK=$RUN_DIR/agent.sock
SYS_CFG=/etc/upanel/upanel.json
AGENT_DB_DIR=/var/lib/upanel-agent
AGENT_DB=$AGENT_DB_DIR/agent.db
KEY_FILE=/etc/upanel/agent.key
WEB_DB_DIR=$WORK/webdata
WEB_DB=$WEB_DB_DIR/upanel.db
RW_DIR=$WORK/rw
PORT=${PORT:-19495}
BASE="http://127.0.0.1:$PORT"
ORIGIN="$BASE"
WEB_USER=upanel-web
PASSWORD='vm-correct-horse-9'
SPARE_DISK=${SPARE_DISK:-sdb}
STORE=/api/v1/storage
PREFLIGHT_PATH=/api/v1/operations/preflight
JOBS=/api/v1/jobs
AGENT_LOG=$LOGS/agent.log
WEB_LOG=$LOGS/web.log
AGENT_PID=""
WEB_PID=""
UNITS_WERE=""

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

resp_get() { jget "$WORK/resp.json" "$1"; }
topo_get() { jget "$WORK/topo.json" "$1"; }
dev_get() { jget "$WORK/device.json" "$1"; }
pre_get() { jget "$WORK/preflight.json" "$1"; }
sha_of() { sha256sum "$1" 2>/dev/null | cut -d' ' -f1; }

cleanup() {
  for _pid in "${WEB_PID:-}" "${AGENT_PID:-}"; do
    if [ -n "$_pid" ] && kill -0 "$_pid" 2>/dev/null; then
      kill -TERM "$_pid" 2>/dev/null
      wait "$_pid" 2>/dev/null
    fi
  done
  rm -f "$SOCK"
  case "$UNITS_WERE" in
    *agent*) systemctl start upanel-agent >/dev/null 2>&1 ;;
  esac
  case "$UNITS_WERE" in
    *web*) systemctl start upanel-web >/dev/null 2>&1 ;;
  esac
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

# device_detail <id>：把设备详情落到 device.json（判定必须来自代理的同一份快照）。
device_detail() {
  api "$JAR" "$CSRF" GET "$STORE/devices/$1" >/dev/null
  cp "$WORK/resp.json" "$WORK/device.json"
}

# preflight <action> <params-json>：回显状态码，响应落进 preflight.json。
preflight() {
  printf '{"action":"%s","params":%s}' "$1" "$2" >"$WORK/pre.json"
  _code=$(curl -s -b "$JAR" -o "$WORK/resp.json" -w '%{http_code}' \
    -X POST "$BASE$PREFLIGHT_PATH" \
    -H 'Content-Type: application/json' -H "Origin: $ORIGIN" -H "X-CSRF-Token: $CSRF" \
    -d @"$WORK/pre.json")
  cp "$WORK/resp.json" "$WORK/preflight.json"
  printf '%s' "$_code"
}
pall() { pre_get '",".join(b["code"] for b in d["data"]["blockers"])'; }
pmsg() { pre_get '";".join(b["message"] for b in d["data"]["blockers"])'; }
plocks() { pre_get '",".join(sorted(d["data"]["locks"]))'; }
pallowed() { pre_get 'd["data"]["allowed"]'; }
# lock_for <device_id>：按 CONTRACTS「锁沿 depends_on/partition_of 从目标自己追」算出的
# 期望锁集合。整盘 → 它自己一把；分区 → 它所在整盘一把。**不是**"整台机所有物理盘"——
# 锁是互斥用的，不是保护用的；保护看 SYSTEM_DISK_PROTECTED。2026-10-29 在 RAID 根盘
# 上量出来的（见 evidence 的 raid/README.md §3）：目标 sda 只锁 disk:sda，目标 md0
# 才锁 disk:sda,disk:sdb。LVM 只有一根 PV，两种读法在那一场上恰好同值，所以没暴露。
lock_for() {
  if [ -e "/sys/class/block/$1/partition" ]; then
    printf 'disk:%s' "$(basename "$(dirname "$(readlink -f "/sys/class/block/$1")")")"
  else
    printf 'disk:%s' "$1"
  fi
}

# cap_state / cap_reason <id> <action>
cap_state() {
  dev_get "next((c[\"state\"] for c in d[\"data\"][\"device\"][\"capabilities\"] if c[\"action\"]==\"$2\"),\"<无此动作>\")"
}
cap_reason() {
  dev_get "next((c.get(\"reason\") or \"\" for c in d[\"data\"][\"device\"][\"capabilities\"] if c[\"action\"]==\"$2\"),\"\")"
}

node_field() {
  topo_get "next((n for n in d[\"data\"][\"nodes\"] if n[\"id\"]==\"$1\"),{}).get(\"$2\")"
}
node_prot() {
  topo_get "next((n for n in d[\"data\"][\"nodes\"] if n[\"id\"]==\"$1\"),{}).get(\"protection\",{}).get(\"$2\")"
}
node_sources() {
  topo_get "\",\".join(next((n for n in d[\"data\"][\"nodes\"] if n[\"id\"]==\"$1\"),{}).get(\"protection\",{}).get(\"sources\") or [])"
}
edge_has() {
  topo_get "any(e[\"from_device_id\"]==\"$1\" and e[\"to_device_id\"]==\"$2\" and e[\"relation\"]==\"$3\" for e in d[\"data\"][\"edges\"])"
}

# 前 1 MiB 与末 1 MiB 的哈希：产品一旦真动过盘，这两块必有一块变。
head_hash() { dd if="$1" bs=1M count=1 2>/dev/null | sha256sum | cut -d' ' -f1; }
tail_hash() {
  _bytes=$(blockdev --getsize64 "$1" 2>/dev/null || echo 0)
  _skip=$(( _bytes / 1048576 - 1 ))
  [ "$_skip" -lt 0 ] && _skip=0
  dd if="$1" bs=1M skip="$_skip" count=1 2>/dev/null | sha256sum | cut -d' ' -f1
}

echo "== 0. 环境与「根在叠层设备上」的现场自证 =="
. /etc/os-release
note "系统：$PRETTY_NAME"
note "内核：$(uname -r)"
expect_eq "以 root 运行" "0" "$(id -u)"
for t in python3 curl runuser lsblk blkid blockdev dd sha256sum; do
  if command -v "$t" >/dev/null 2>&1; then pass "依赖命令可用 $t"; else fail "依赖命令可用 $t" "缺少"; exit 2; fi
done

# --- 现场自证：不是 LVM/md 根就退出，绝不在这里假装叠层读数。 ---
ROOT_SRC=$(findmnt -no SOURCE / 2>/dev/null)
ROOT_REAL=$(readlink -f "$ROOT_SRC" 2>/dev/null)
ROOT_ID=$(basename "${ROOT_REAL:-none}")
note "/ 的来源：$ROOT_SRC -> ${ROOT_REAL:-?}（设备 id $ROOT_ID）"
SCENE=""
ROOT_KIND=""
case "$ROOT_ID" in
  dm-*)
    DM_UUID=$(cat "/sys/block/$ROOT_ID/dm/uuid" 2>/dev/null)
    case "$DM_UUID" in
      LVM-*)
        SCENE=lvm; ROOT_KIND=lvm
        pass "/ 落在 device-mapper 设备 $ROOT_ID 上，dm/uuid 是 LVM"
        note "dm 名：$(cat "/sys/block/$ROOT_ID/dm/name" 2>/dev/null)（udev 写成 /dev/mapper/…）"
        expect_eq "根文件系统类型" "ext4" "$(findmnt -no FSTYPE / 2>/dev/null)"
        ;;
      *)
        printf '/sys/block/%s/dm/uuid = %s，不是 LVM；本脚本只验 LVM 与 md 根，退出\n' \
          "$ROOT_ID" "${DM_UUID:-空}" >&2
        exit 2
        ;;
    esac
    ;;
  md*)
    if [ -d "/sys/block/$ROOT_ID/md" ]; then
      SCENE=raid; ROOT_KIND=raid
      pass "/ 落在软 RAID 设备 $ROOT_ID 上（/sys/block/$ROOT_ID/md 存在）"
      MD_LEVEL=$(cat "/sys/block/$ROOT_ID/md/level" 2>/dev/null)
      MD_STATE=$(cat "/sys/block/$ROOT_ID/md/array_state" 2>/dev/null)
      note "阵列级别 $MD_LEVEL，状态 $MD_STATE"
      expect_contains "阵列级别是 RAID" "raid" "$MD_LEVEL"
      expect_eq "阵列状态是 clean" "clean" "$MD_STATE"
      expect_eq "根文件系统类型" "ext4" "$(findmnt -no FSTYPE / 2>/dev/null)"
    else
      printf '/sys/block/%s/md 不存在；这个 / 不在 md 阵列上，退出\n' "$ROOT_ID" >&2
      exit 2
    fi
    ;;
  *)
    printf '这台的 / 既不在 dm/LVM 也不在 md/RAID 上（来源 %s）；DISK-02 不在分区根上冒充，退出\n' \
      "$ROOT_SRC" >&2
    exit 2
    ;;
esac
note "场景：$SCENE（根设备 $ROOT_ID，拓扑里应显示为 $ROOT_KIND）"

# --- 链路：根设备 → 成员分区 → 物理盘。从 sysfs 读，不从 lsblk 的显示文本里抠：
#     `lsblk -s` 会打出树形字符（└─），而且对 dm 设备显示的是 dm 名（upanel--vg-root）。 ---
SLAVES=$(ls "/sys/block/$ROOT_ID/slaves/" 2>/dev/null | tr '\n' ' ' | sed 's/ *$//')
note "根设备的成员（sysfs slaves）：${SLAVES:-空}"
if [ -z "$SLAVES" ]; then
  printf '根设备 %s 没有 slaves；这个现场不满足 DISK-02 的前提，退出\n' "$ROOT_ID" >&2
  exit 2
fi
DISKS=""
for s in $SLAVES; do
  [ -e "/sys/class/block/$s" ] || {
    printf '成员 %s 在 sysfs 里找不到；退出\n' "$s" >&2; exit 2; }
  _parent=$(basename "$(dirname "$(readlink -f "/sys/class/block/$s")")")
  expect_eq "成员 $s 是分区（type）" "part" "$(lsblk -dno TYPE "/dev/$s" 2>/dev/null)"
  expect_eq "成员 $s 挂在整盘 $_parent（type）" "disk" "$(lsblk -dno TYPE "/dev/$_parent" 2>/dev/null)"
  case " $DISKS " in
    *" $_parent "*) ;;
    *) DISKS="$DISKS $_parent" ;;
  esac
done
DISKS=$(printf '%s\n' $DISKS | sort | tr '\n' ' ' | sed 's/ *$//')
note "物理盘：$DISKS（共 $(printf '%s\n' $DISKS | wc -l | tr -d ' ') 块）"
for d in $DISKS; do
  if [ "$d" = "$SPARE_DISK" ]; then
    printf '/dev/%s 是承载 / 的物理盘，不能同时当备用盘；退出\n' "$SPARE_DISK" >&2; exit 2
  fi
done
# 成员盘的签名：LVM 是 LVM2_member，RAID 是 linux_raid_member。
if [ "$SCENE" = "lvm" ]; then
  for s in $SLAVES; do
    expect_eq "成员 $s 的 PV 签名" "LVM2_member" "$(blkid -s TYPE -o value "/dev/$s" 2>/dev/null)"
  done
else
  for s in $SLAVES; do
    expect_eq "成员 $s 的 RAID 签名" "linux_raid_member" "$(blkid -s TYPE -o value "/dev/$s" 2>/dev/null)"
    expect_contains "成员 $s 在阵列里的状态" "in_sync" "$(cat "/sys/block/$ROOT_ID/md/dev-$s/state" 2>/dev/null)"
  done
fi
# /boot 必须在根设备里面：否则物理盘受保护可能来自 /boot 分区，递归就证明不成立。
expect_eq "/boot 不是独立挂载点（在根设备里面）" "" "$(findmnt -no SOURCE /boot 2>/dev/null)"
expect_eq "没有独立的 ESP（/boot/efi 不挂在任何块设备上）" "" \
  "$(findmnt -no SOURCE /boot/efi 2>/dev/null)"
for d in $DISKS; do
  expect_eq "整盘 $d 自身没有挂载点（MOUNTPOINT 空）" "" \
    "$(lsblk -dno MOUNTPOINT "/dev/$d" 2>/dev/null)"
  expect_eq "整盘 $d 自身没有文件系统（blkid TYPE 空）" "" \
    "$(blkid -s TYPE -o value "/dev/$d" 2>/dev/null)"
done
note "每块物理盘自己都不承载文件系统 —— 它们受保护只可能来自递归"

# --- 备用盘：反面对照，必须与根链无关。 ---
if [ ! -b "/dev/$SPARE_DISK" ]; then
  printf '备用盘 /dev/%s 不存在；需要一个空整盘做"递归没有过宽"的对照，退出\n' "$SPARE_DISK" >&2
  exit 2
fi
for d in $ROOT_ID $SLAVES $DISKS; do
  if [ "$d" = "$SPARE_DISK" ]; then
    printf '/dev/%s 在根链路里，不能当备用盘；退出\n' "$SPARE_DISK" >&2; exit 2
  fi
done
expect_eq "备用盘 /dev/$SPARE_DISK 是整盘" "disk" "$(lsblk -dno TYPE "/dev/$SPARE_DISK" 2>/dev/null)"
expect_eq "备用盘没有挂载点" "" "$(lsblk -dno MOUNTPOINT "/dev/$SPARE_DISK" 2>/dev/null)"
expect_eq "备用盘是活动交换区吗" "" "$(swapon --show=NAME --noheadings 2>/dev/null | grep -x "/dev/$SPARE_DISK" || :)"
expect_eq "备用盘是 LVM/dm 成员吗" "" "$(dmsetup ls 2>/dev/null | grep -w "$SPARE_DISK" || :)"
SPARE_SIZE=$(blockdev --getsize64 "/dev/$SPARE_DISK" 2>/dev/null || echo 0)
note "备用盘 /dev/$SPARE_DISK 大小 $((SPARE_SIZE / 1024 / 1024)) MiB"

# 期望的锁：每一块底层盘一把。
EXPECT_LOCKS=$(for d in $DISKS; do printf 'disk:%s\n' "$d"; done | sort | paste -sd, -)
note "期望的锁：$EXPECT_LOCKS"

if ! id -u "$WEB_USER" >/dev/null 2>&1; then
  useradd --system --no-create-home --shell /usr/sbin/nologin "$WEB_USER" 2>/dev/null
fi
if id -u "$WEB_USER" >/dev/null 2>&1; then pass "Web 账号 $WEB_USER 存在"; else
  fail "Web 账号 $WEB_USER 存在" "无法创建"; exit 2; fi
if ! getent group "$WEB_USER" >/dev/null 2>&1; then groupadd --system "$WEB_USER" 2>/dev/null; fi
if getent group "$WEB_USER" >/dev/null 2>&1; then pass "Web 组 $WEB_USER 存在"; else
  fail "Web 组 $WEB_USER 存在" "无法创建"; exit 2; fi

if curl -fsS "$BASE/api/v1/health/live" >/dev/null 2>&1; then
  echo "$PORT 上已有服务在监听；先停掉遗留的 upanel-web 再跑验收" >&2
  exit 1
fi

for u in upanel-agent upanel-web; do
  if systemctl is-active --quiet "$u" 2>/dev/null; then
    UNITS_WERE="$UNITS_WERE $(printf '%s' "$u" | sed 's/^upanel-//')"
    systemctl stop "$u" >/dev/null 2>&1 || true
  fi
done
[ -n "$UNITS_WERE" ] && note "停掉并将在收尾还原的单元：$UNITS_WERE"
_i=0
while [ "$_i" -lt 40 ]; do
  if [ "$(systemctl is-active upanel-agent 2>/dev/null)" = "inactive" ] &&
     [ "$(systemctl is-active upanel-web 2>/dev/null)" = "inactive" ]; then
    break
  fi
  _i=$((_i + 1)); sleep 0.5
done
expect_eq "代理单元已停（否则 socket 被占）" "inactive" "$(systemctl is-active upanel-agent 2>/dev/null || :)"
expect_eq "Web 单元已停" "inactive" "$(systemctl is-active upanel-web 2>/dev/null || :)"
expect_eq "端口 $PORT 空闲" "" "$(ss -lntH "sport = :$PORT" 2>/dev/null)"

echo
echo "== 1. 配置与启动（代理 root、Web 非 root） =="
rm -rf "$WEB_DB_DIR" "$RW_DIR" "$LOGS"
mkdir -p "$LOGS" "$WEB_DB_DIR" "$RW_DIR"
chown -R "$WEB_USER" "$WEB_DB_DIR"
chmod 0755 "$WORK"
install -d -m 0755 /etc/upanel
install -d -m 0700 "$AGENT_DB_DIR"
install -d -m 0755 "$RUN_DIR"
rm -f "$AGENT_DB" "$AGENT_DB-wal" "$AGENT_DB-shm"
rm -f "$WEB_DB" "$WEB_DB-wal" "$WEB_DB-shm"
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
    "files": {
      "max_upload_bytes": 1073741824,
      "roots": [
        { "id": "rw", "name": "可写根", "path": "$RW_DIR", "writable": true }
      ]
    },
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
  if "$AGENT" --config "$SYS_CFG" -init-key >>"$LOGS/init-key.log" 2>&1; then
    pass "-init-key 生成代理密钥"
  else
    fail "-init-key 生成代理密钥" "$(tail -3 "$LOGS/init-key.log" 2>/dev/null)"; exit 2
  fi
fi
if start_agent; then pass "代理以 root 启动并创建 socket"; else
  fail "代理以 root 启动" "$(tail -5 "$AGENT_LOG" 2>/dev/null)"; exit 2; fi
expect_eq "代理进程 uid=0" "0" "$(ps -o uid= -p "$AGENT_PID" 2>/dev/null | tr -d ' ')"

printf '%s\n%s\n' "$PASSWORD" "$PASSWORD" | "$CTL" init-admin --config "$SYS_CFG" --db "$WEB_DB" \
  --username admin --display-name "管理员" --json >"$WORK/init.out" 2>"$WORK/init.err"
expect_eq "init-admin 退出码" "0" "$?"
chown_web_db
if start_web; then pass "Web 在 $BASE 启动"; else
  fail "Web 启动" "$(tail -5 "$WEB_LOG" 2>/dev/null)"; exit 2; fi
LISTEN_PID=$(ss -lntpH "sport = :$PORT" 2>/dev/null | sed -n 's/.*pid=\([0-9]*\).*/\1/p' | head -1)
REAL_UID=$(ps -o uid= -p "${LISTEN_PID:-0}" 2>/dev/null | tr -d ' ')
if [ -n "$REAL_UID" ] && [ "$REAL_UID" != "0" ]; then
  pass "监听 $PORT 的 Web 进程不是 root（pid=$LISTEN_PID uid=$REAL_UID）"
else
  fail "监听 $PORT 的 Web 进程不是 root" "uid=${REAL_UID:-未知}"
fi

JAR=$WORK/admin.jar
expect_eq "管理员登录" "200" "$(login admin "$PASSWORD" "$JAR" login-admin)"
CSRF=$(csrf_of "$WORK/login-admin.json")
expect_ne "拿到 CSRF 令牌" "" "$CSRF"

echo
echo "== 2. 拓扑：根设备 / 成员分区 / 每块物理盘都要受保护 =="
expect_eq "读拓扑" "200" "$(api "$JAR" "$CSRF" GET "$STORE/topology")"
cp "$WORK/resp.json" "$WORK/topo.json"
expect_eq "拓扑状态" "ok" "$(topo_get 'd["data"]["status"]')"

expect_eq "拓扑里认下 / 归属 $ROOT_ID" "$ROOT_ID" \
  "$(topo_get 'next((n["id"] for n in d["data"]["nodes"] if n.get("filesystem") and any(m["path"]=="/" for m in (n["filesystem"].get("mounts") or []))),"")')"
expect_eq "根设备在拓扑里的类型 = $ROOT_KIND" "$ROOT_KIND" "$(node_field "$ROOT_ID" kind)"
expect_eq "根设备受保护" "True" "$(node_prot "$ROOT_ID" system_disk)"
expect_contains "根设备的理由里说得出是根文件系统" "根文件系统" "$(node_prot "$ROOT_ID" reasons)"
for s in $SLAVES; do
  expect_eq "成员分区 $s 受保护" "True" "$(node_prot "$s" system_disk)"
  expect_eq "成员分区 $s 的 protection.sources 只有根设备" "$ROOT_ID" "$(node_sources "$s")"
  expect_eq "拓扑里有 $ROOT_ID --depends_on--> $s" "True" "$(edge_has "$ROOT_ID" "$s" depends_on)"
done
for d in $DISKS; do
  expect_eq "物理盘 $d 受保护" "True" "$(node_prot "$d" system_disk)"
  expect_eq "物理盘 $d 在拓扑里的类型是 disk" "disk" "$(node_field "$d" kind)"
  # 关键读数：整盘自己没有任何文件系统/挂载点，它的来源只有根设备。
  expect_eq "物理盘 $d 自己没有文件系统（filesystem 为 null）" "None" "$(node_field "$d" filesystem)"
  expect_eq "物理盘 $d 的 protection.sources 只有根设备（标记是递归上来的）" "$ROOT_ID" \
    "$(node_sources "$d")"
  for s in $SLAVES; do
    _p=$(basename "$(dirname "$(readlink -f "/sys/class/block/$s")")")
    if [ "$_p" = "$d" ]; then
      expect_eq "拓扑里有 $s --partition_of--> $d" "True" "$(edge_has "$s" "$d" partition_of)"
    fi
  done
done
expect_eq "备用盘 $SPARE_DISK 不受保护" "False" "$(node_prot "$SPARE_DISK" system_disk)"

echo
echo "== 3. 四条写动作预检：叠层各层都拒，锁按目标自己的依赖链追 =="
FIRST_SLAVE=$(printf '%s\n' $SLAVES | head -1)
FIRST_DISK=$(printf '%s\n' $DISKS | head -1)

expect_eq "整盘 $FIRST_DISK 初始化预检被拒" "False" \
  "$(preflight storage.partition_table.initialize "{\"device_id\":\"$FIRST_DISK\",\"table_type\":\"gpt\"}" >/dev/null; pallowed)"
expect_contains "整盘初始化的阻塞码" "SYSTEM_DISK_PROTECTED" "$(pall)"
expect_contains "整盘初始化的理由说明是系统数据" "系统数据" "$(pmsg)"
expect_eq "整盘 $FIRST_DISK 初始化的锁只有它自己" "$(lock_for "$FIRST_DISK")" "$(plocks)"

for d in $DISKS; do
  expect_eq "整盘 $d 初始化预检被拒" "False" \
    "$(preflight storage.partition_table.initialize "{\"device_id\":\"$d\",\"table_type\":\"gpt\"}" >/dev/null; pallowed)"
  expect_contains "整盘 $d 初始化的阻塞码" "SYSTEM_DISK_PROTECTED" "$(pall)"
  expect_eq "整盘 $d 初始化的锁只有它自己" "$(lock_for "$d")" "$(plocks)"
done

# 多块物理盘时才做这一条：过宽的锁会让两块无关的盘上的动作互相排队。
# 一块盘的拓扑（LVM）上它恒真，写出来只会变成一个不量任何东西的绿点。
if [ "$(printf '%s\n' $DISKS | wc -l | tr -d ' ')" -gt 1 ]; then
  preflight storage.partition_table.initialize "{\"device_id\":\"$FIRST_DISK\",\"table_type\":\"gpt\"}" >/dev/null
  for d in $DISKS; do
    [ "$d" = "$FIRST_DISK" ] && continue
    expect_not_contains "整盘 $FIRST_DISK 的锁不含 $d（锁没有过宽）" "disk:$d" "$(plocks)"
  done
fi

expect_eq "成员分区 $FIRST_SLAVE 删除预检被拒" "False" \
  "$(preflight storage.partition.delete "{\"device_id\":\"$FIRST_SLAVE\"}" >/dev/null; pallowed)"
expect_contains "成员分区删除的阻塞码" "SYSTEM_DISK_PROTECTED" "$(pall)"
expect_eq "成员分区删除的锁是它所在的那块整盘" "$(lock_for "$FIRST_SLAVE")" "$(plocks)"

expect_eq "成员分区 $FIRST_SLAVE 格式化预检被拒" "False" \
  "$(preflight storage.filesystem.format "{\"device_id\":\"$FIRST_SLAVE\",\"filesystem\":\"ext4\"}" >/dev/null; pallowed)"
expect_contains "成员分区格式化的阻塞码" "SYSTEM_DISK_PROTECTED" "$(pall)"
expect_eq "成员分区格式化的锁是它所在的那块整盘" "$(lock_for "$FIRST_SLAVE")" "$(plocks)"

expect_eq "根设备 $ROOT_ID 格式化预检被拒" "False" \
  "$(preflight storage.filesystem.format "{\"device_id\":\"$ROOT_ID\",\"filesystem\":\"ext4\"}" >/dev/null; pallowed)"
expect_contains "根设备格式化的阻塞码" "SYSTEM_DISK_PROTECTED" "$(pall)"
# 这一条才是"每一块底层盘"：目标是叠层设备，依赖链把每一块成员盘都追出来。
expect_eq "根设备格式化的锁覆盖每一块底层盘" "$EXPECT_LOCKS" "$(plocks)"

echo
echo "== 4. 设备详情的能力视图与预检判决一致 =="
device_detail "$FIRST_DISK"
expect_eq "整盘详情：受保护" "True" "$(dev_get 'd["data"]["device"]["protection"]["system_disk"]')"
expect_eq "整盘详情：初始化能力 blocked" "blocked" "$(cap_state "$FIRST_DISK" storage.partition_table.initialize)"
expect_contains "整盘能力理由说明是系统数据" "系统" "$(cap_reason "$FIRST_DISK" storage.partition_table.initialize)"
device_detail "$FIRST_SLAVE"
expect_eq "成员分区详情：受保护" "True" "$(dev_get 'd["data"]["device"]["protection"]["system_disk"]')"
expect_eq "成员分区详情：删除能力 blocked" "blocked" "$(cap_state "$FIRST_SLAVE" storage.partition.delete)"
expect_contains "成员分区删除理由说明是系统数据" "系统" "$(cap_reason "$FIRST_SLAVE" storage.partition.delete)"
device_detail "$ROOT_ID"
expect_eq "根设备详情：受保护" "True" "$(dev_get 'd["data"]["device"]["protection"]["system_disk"]')"

echo
echo "== 5. 反面对照：备用盘没有跟着被标红 =="
device_detail "$SPARE_DISK"
expect_eq "备用盘详情：不受保护" "False" "$(dev_get 'd["data"]["device"]["protection"]["system_disk"]')"
expect_eq "备用盘详情：初始化能力 available" "available" \
  "$(cap_state "$SPARE_DISK" storage.partition_table.initialize)"
expect_eq "备用盘初始化预检 allowed=true（递归没有把整台机锁死）" "True" \
  "$(preflight storage.partition_table.initialize "{\"device_id\":\"$SPARE_DISK\",\"table_type\":\"gpt\"}" >/dev/null; pallowed)"
expect_eq "备用盘初始化的锁只锁它自己" "disk:$SPARE_DISK" "$(plocks)"
expect_not_contains "备用盘预检里没有系统盘阻塞" "SYSTEM_DISK_PROTECTED" "$(pall)"

echo
echo "== 6. 一个字节都没写：前后哈希 + 没有非终态任务 =="
ROOT_UUID_HASH=$(sha_of "/sys/block/$ROOT_ID")
HEAD_BEFORE=""
TAIL_BEFORE=""
HEAD_AFTER=""
TAIL_AFTER=""
for d in $DISKS; do
  HEAD_BEFORE="$HEAD_BEFORE $(head_hash "/dev/$d")"
  TAIL_BEFORE="$TAIL_BEFORE $(tail_hash "/dev/$d")"
done
# 再把四条预检跑一遍，之后立刻比对（让"哈希"夹住整轮拒绝）。
preflight storage.partition_table.initialize "{\"device_id\":\"$FIRST_DISK\",\"table_type\":\"gpt\"}" >/dev/null
preflight storage.partition.delete "{\"device_id\":\"$FIRST_SLAVE\"}" >/dev/null
preflight storage.filesystem.format "{\"device_id\":\"$FIRST_SLAVE\",\"filesystem\":\"ext4\"}" >/dev/null
preflight storage.filesystem.format "{\"device_id\":\"$ROOT_ID\",\"filesystem\":\"ext4\"}" >/dev/null
for d in $DISKS; do
  HEAD_AFTER="$HEAD_AFTER $(head_hash "/dev/$d")"
  TAIL_AFTER="$TAIL_AFTER $(tail_hash "/dev/$d")"
done
expect_ne "整盘首 1 MiB 的哈希可读" "" "$(printf '%s\n' $HEAD_BEFORE | head -1)"
expect_eq "每块整盘首 1 MiB 都未被写" "$(printf '%s\n' $HEAD_BEFORE)" "$(printf '%s\n' $HEAD_AFTER)"
expect_eq "每块整盘末 1 MiB 都未被写" "$(printf '%s\n' $TAIL_BEFORE)" "$(printf '%s\n' $TAIL_AFTER)"
for s in $SLAVES; do
  if [ "$SCENE" = "lvm" ]; then
    expect_eq "成员 $s 的 PV 签名还在" "LVM2_member" "$(blkid -s TYPE -o value "/dev/$s" 2>/dev/null)"
  else
    expect_eq "成员 $s 的 RAID 签名还在" "linux_raid_member" "$(blkid -s TYPE -o value "/dev/$s" 2>/dev/null)"
  fi
done
expect_eq "根设备的 sysfs 身份没变" "$ROOT_UUID_HASH" "$(sha_of "/sys/block/$ROOT_ID")"
expect_eq "根仍在本轮开始时的那个设备上" "$ROOT_ID" \
  "$(basename "$(readlink -f "$(findmnt -no SOURCE /)")")"
api "$JAR" "$CSRF" GET "$JOBS" >/dev/null
expect_eq "没有非终态任务" "0" \
  "$(resp_get 'len([j for j in d["data"]["jobs"] if j["status"] not in ("failed","succeeded","canceled")])')"
expect_eq "没有任何存储写任务被执行" "0" \
  "$(resp_get 'len([j for j in d["data"]["jobs"] if str(j.get("action","")).startswith("storage.")])')"

echo
echo "== 7. 日志：写原语确实接上了，但一次都没被执行 =="
# 先证明"拒绝"不是因为缺工具。代理启动时会把接上的写原语打出来；这一行不在，
# 下面那些拒绝就可能只是"没有 sfdisk/mkfs"——那就证明不了递归保护。
if grep -q '磁盘写原语已接线' "$AGENT_LOG" 2>/dev/null; then
  pass "代理启动日志确认写原语已接线（拒绝不是缺工具造成的）"
  note "$(grep -m1 '磁盘写原语已接线' "$AGENT_LOG" 2>/dev/null | cut -c1-220)"
else
  fail "代理启动日志确认写原语已接线" "没找到这一行；本轮的拒绝无法与“缺依赖”区分"
fi
# "接线"是能力公告，"执行"才是动作。写动作一旦真的跑到执行器，会留下这几条日志之一。
for pat in '动作执行失败' '执行前复核被拒' '动作缺少执行实现'; do
  _n=$(cat "$AGENT_LOG" "$WEB_LOG" 2>/dev/null | grep -c "$pat")
  expect_eq "日志里没有「$pat」" "0" "${_n:-0}"
done
expect_not_contains "日志里没有明文口令" "$PASSWORD" "$(cat "$AGENT_LOG" "$WEB_LOG" 2>/dev/null)"

echo
echo "== 8. 明确不验（记录在案，不假装通过） =="
notrun "降级/重建中的 RAID" \
  "成员 faulted、rebuild 进行中的现场没造；递归不看阵列健康，但没有真机读数就不算验过"
notrun "根在 LVM/RAID 上写满（ENOSPC）" \
  "P16.4 的 ENOSPC 现场是独立 tmpfs；把根写满会伤到这台机的系统盘，不做"
notrun "执行期复核（PREFLIGHT_STALE / DEVICE_IDENTITY_CHANGED）" \
  "预检就拒了，提交门到不了；那两条由 P07.2 在一次性盘上验，与根在不在叠层设备上无关"

echo
printf '场景 %s：根设备 %s / 成员 %s / 物理盘 %s\n' "$SCENE" "$ROOT_ID" "$SLAVES" "$DISKS"
printf 'PASS %d / FAIL %d / NOT RUN %d\n' "$PASSED" "$FAILED" "$NOTRUN"
[ -n "$FAILED_NAMES" ] && note "失败项：$FAILED_NAMES" | tr '|' '\n'
[ "$FAILED" -eq 0 ]
