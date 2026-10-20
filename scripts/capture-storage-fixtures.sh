#!/bin/sh
# 采集存储拓扑验收夹具（P04.2）。必须在目标 Ubuntu 机器上以 root 执行：
#
#   sudo sh scripts/capture-storage-fixtures.sh /tmp/storage-fixture-2404
#
# 采出来的目录是一个**缩小版的 /sys**，只含解析器真正会读的文件，外加：
#   proc/self/mountinfo、proc/mounts、proc/swaps   —— 挂载与交换区
#   dev/disk/by-{id,uuid,label,partuuid}/<名字>     —— 稳定设备链接（符号链接目标写进文件）
#   disks/<名字>.bin                                —— 整盘前 34 个扇区（512B/个）：MBR + GPT 头 + 条目表
#                                                   （跳过的盘写 .skip 并说明原因）
#   MANIFEST.txt                                    —— 采集主机、时间、命令、shasum
#
# 为什么单独做脚本：夹具必须是真实采集而不是手写样例（DECISIONS Q85）。手写的
# sysfs 树会把“我以为内核长这样”固化成测试前提，真机上就不成立。
#
# 唯一的外部命令是 hexdump/dd/sh 这些采集工具本身；解析器运行时不执行任何命令。
set -eu

OUT=${1:-}
if [ -z "$OUT" ]; then
    echo "用法: $0 <输出目录>" >&2
    exit 2
fi
if [ "$(id -u)" -ne 0 ]; then
    echo "错误: 需要 root（读 /dev/<disk> 前 34 个扇区）" >&2
    exit 2
fi

SYS=${SYS_ROOT:-/sys}
BLK="$SYS/block"

# take <源文件> <目标文件>：读不到就**不建**目标文件。
# 不用 `[ -e ] && cat`：有些属性文件存在但读起来报 EINVAL（例如空 wwid），
# 在 set -e 下会把整个采集打断。
take() {
    if [ -e "$1" ]; then
        cat "$1" > "$2" 2>/dev/null || rm -f "$2"
    fi
}

rm -rf "$OUT"
mkdir -p "$OUT/sys/block" "$OUT/proc/self" "$OUT/dev/disk" "$OUT/disks"

# ---- 每个块设备：解析器会读的属性 + 分区的 start/size/partition
for d in "$BLK"/*; do
    name=$(basename "$d")
    dest="$OUT/sys/block/$name"
    mkdir -p "$dest/queue" "$dest/device" "$dest/holders" "$dest/slaves"

    # 顶层属性：读不到就跳过（不写默认值，缺文件本身就是要测的情况）
    for f in size ro removable dev; do
        take "$d/$f" "$dest/$f"
    done
    for f in logical_block_size physical_block_size rotational; do
        take "$d/queue/$f" "$dest/queue/$f"
    done
    # SCSI/NVMe/virtio 的设备属性；NVMe 的型号在 device/model，序列号在 device/serial
    for f in vendor model rev serial wwid state type; do
        take "$d/device/$f" "$dest/device/$f"
    done
    # 休眠判断用：runtime_status 是 suspended 时不去读盘（不强制唤醒）
    take "$d/device/power/runtime_status" "$dest/device/power-status"
    take "$d/device/power/control" "$dest/device/power-control"
    # 传输类型靠 sysfs 真实路径判断（virtio / nvme / usb / scsi）。只留 /sys 之后的相对路径，
    # 夹具因此不依赖采集主机的路径。
    if [ -L "$d/device" ]; then
        readlink -f "$d/device" 2>/dev/null | sed "s|^$SYS/||" > "$dest/device/realpath" || rm -f "$dest/device/realpath"
    fi
    # 设备类型标记：dm（LVM/crypt）与 md（软 RAID）只看这几个属性，不去猜名字。
    # 路径与真机 sysfs 保持一致（/sys/block/dm-0/dm/uuid、/sys/block/md0/md/level）：
    # 夹具如果按别的形状存，解析代码在夹具上过了、到真机上就错。
    take "$d/dm/name" "$dest/dm/name"
    take "$d/dm/uuid" "$dest/dm/uuid"
    take "$d/md/level" "$dest/md/level"
    take "$d/md/array_state" "$dest/md/array_state"
    if [ -d "$d/md" ] && [ ! -e "$dest/md/level" ]; then
        # 有 md 目录但 level 读不到：留一个标记，仍然能认出这是 md 设备。
        : > "$dest/md/.present"
    fi

    # holders/slaves：只留名字（dm/md 的父子关系）
    for h in "$d/holders"/*; do
        if [ -e "$h" ]; then basename "$h" >> "$dest/holders/list"; fi
    done
    for s in "$d/slaves"/*; do
        if [ -e "$s" ]; then basename "$s" >> "$dest/slaves/list"; fi
    done

    # 分区：目录名以设备名开头且不是设备自己的那些
    for p in "$d"/*; do
        [ -d "$p" ] || continue
        pn=$(basename "$p")
        [ "$pn" = queue ] && continue
        [ "$pn" = device ] && continue
        [ "$pn" = holders ] && continue
        [ "$pn" = slaves ] && continue
        [ "$pn" = power ] && continue
        case "$pn" in
            "$name"|"$name"[0-9]*) ;;
            *) continue ;;
        esac
        [ "$pn" = "$name" ] && continue
        pdest="$dest/$pn"
        mkdir -p "$pdest"
        for f in start size partition dev ro; do
            take "$p/$f" "$pdest/$f"
        done
        # 分区上的 holders（LVM 在分区上时这里非空）
        for h in "$p/holders"/*; do
            if [ -e "$h" ]; then basename "$h" >> "$pdest/holders"; fi
        done
    done
done

# ---- 挂载、交换区
cat /proc/self/mountinfo > "$OUT/proc/self/mountinfo"
cat /proc/mounts > "$OUT/proc/mounts"
cat /proc/swaps > "$OUT/proc/swaps"

# ---- 稳定设备链接：把符号链接的**目标**写成普通文件，避免夹具依赖宿主路径
mkdir -p "$OUT/dev/disk"
for kind in id uuid label partuuid; do
    mkdir -p "$OUT/dev/disk/by-$kind"
    for l in /dev/disk/by-"$kind"/*; do
        [ -e "$l" ] || continue
        readlink -f "$l" > "$OUT/dev/disk/by-$kind/$(basename "$l")" 2>/dev/null || true
    done
done

# ---- 整盘前 34 个扇区（MBR 在 0，GPT 头在 1，条目表 2..33），存成二进制 .bin
for d in "$BLK"/*; do
    name=$(basename "$d")
    case "$name" in
        loop*|ram*|fd*) continue ;;   # loop/fd 没有分区表，也不该去读
    esac
    # 睡眠盘不读：读 /dev 会把它唤醒，这正是不允许的行为。
    # runtime_status 的取值是 active/suspended（active = 供电中），device/state
    # 在 SCSI 上是 running、在 NVMe 上是 live、在 ATA 上可能是 active。
    rs=$(cat "$d/device/power/runtime_status" 2>/dev/null || echo active)
    st=$(cat "$d/device/state" 2>/dev/null || echo running)
    if [ "$rs" = "suspended" ]; then
        echo "skip=power-suspended" > "$OUT/disks/$name.skip"
        continue
    fi
    case "$st" in
        running|active|live) ;;
        *) echo "skip=state-$st" > "$OUT/disks/$name.skip"; continue ;;
    esac
    if dd if="/dev/$name" bs=512 count=34 of="$OUT/disks/$name.bin" 2>/dev/null; then
        :
    else
        rm -f "$OUT/disks/$name.bin"
        echo "skip=read-error" > "$OUT/disks/$name.skip"
    fi
done

# ---- 清单
{
    echo "# 存储夹具采集清单"
    echo "host=$(hostname)"
    echo "os=$(. /etc/os-release; echo "$PRETTY_NAME")"
    echo "kernel=$(uname -r)"
    echo "time_utc=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    echo "collected_by=capture-storage-fixtures.sh"
    echo
    echo "## lsblk -b"
    lsblk -b -o NAME,KNAME,MAJ:MIN,TYPE,SIZE,RO,MOUNTPOINT,FSTYPE,UUID 2>/dev/null || true
    echo
    echo "## 文件清单与 sha256"
    find "$OUT" -type f | sort | while read -r f; do
        printf '%s  %s\n' "$(sha256sum "$f" | cut -d' ' -f1)" "${f#"$OUT"/}"
    done
} > "$OUT/MANIFEST.txt"

echo "已采集到 $OUT"
find "$OUT" -type f | wc -l
