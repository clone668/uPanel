#!/bin/sh
# P07.3 实测：mkfs.ext4 / mkfs.xfs 的真实行为（只在验收 VM 的备用盘 sdc 上跑）。
#
# 要回答的是四件事，每件都必须有输出为证，不能靠猜：
#   1. ext4 覆盖“已有 ext4 签名”要不要 -F（P07.2 已测一次，这里复测）。
#   2. ext4 覆盖“别的文件系统签名”（xfs）要不要 -F —— 决定 -F 是按状态加还是常量加。
#   3. ext4 覆盖“认不出来的非零内容”要不要 -F（判决层只警告不阻塞这种情况）。
#   4. 标签落盘的确切位置与截断行为（16 字节上限的边界、17 字节时盘上是什么）。
# 另外测：已挂载分区上 mkfs 的退出码与消息（错误退出验收要用），以及单次 mkfs 的耗时
# （决定“进程中断”测试要怎么做才能稳定命中 running 阶段）。
set -u

DEV=/dev/sdc
MNT=/var/tmp/upanel-mkfs-probe
SB_EXT4=$((0x400))   # ext4 超级块偏移
SB_NAME=$((0x400 + 0x78))
SB_UUID=$((0x400 + 0x68))
SB_XFS_NAME=$((0x6c))

say() { printf '\n===== %s =====\n' "$*"; }

cleanup() {
    umount "$MNT" 2>/dev/null
    rmdir "$MNT" 2>/dev/null
}
trap cleanup EXIT

# 每个用例开始前把两条分区都清掉签名，保证起点确定。
wipe_fs() {
    wipefs -a "$1" >/dev/null 2>&1
}

show_sb() { # $1=分区 $2=文件系统类型
    if [ "$2" = ext4 ]; then
        echo -n "  s_volume_name (off 0x478,16B): "
        dd if="$1" bs=1 skip=$SB_NAME count=16 2>/dev/null | od -An -c | head -2
        echo -n "  s_uuid        (off 0x468,16B): "
        dd if="$1" bs=1 skip=$SB_UUID count=16 2>/dev/null | od -An -tx1 | head -2
        echo -n "  magic         (off 0x438,2B) : "
        dd if="$1" bs=1 skip=$((0x438)) count=2 2>/dev/null | od -An -tx1
    else
        echo -n "  sb_fname      (off 0x06c,12B): "
        dd if="$1" bs=1 skip=$SB_XFS_NAME count=12 2>/dev/null | od -An -c | head -2
        echo -n "  sb_magicnum   (off 0x000,4B) : "
        dd if="$1" bs=1 skip=0 count=4 2>/dev/null | od -An -c
        echo -n "  sb_uuid       (off 0x020,16B): "
        dd if="$1" bs=1 skip=$((0x20)) count=16 2>/dev/null | od -An -tx1 | head -2
    fi
}

run_mkfs() { # $1=描述 $2..=命令；输出 rc、stdout、stderr
    desc=$1; shift
    printf '\n--- %s\n' "$desc"
    printf '  cmd: %s\n' "$*"
    out=$(timeout 60 "$@" </dev/null 2>&1); rc=$?
    printf '  rc=%d\n' "$rc"
    printf '%s\n' "$out" | sed 's/^/  | /'
}

say "0. 环境"
dpkg -l e2fsprogs xfsprogs 2>/dev/null | grep -E '^ii' | awk '{print "  "$2" "$3}'
printf '  kernel: %s\n' "$(uname -r)"
printf '  %s: %s\n' "$DEV" "$(lsblk -dno SIZE,MODEL "$DEV" 2>&1)"
printf '  serial: %s\n' "$(cat /sys/block/$(basename "$DEV")/device/serial 2>/dev/null)"

say "1. 准备：GPT + 两个 1 GiB 分区"
umount "$DEV"1 2>/dev/null
umount "$DEV"2 2>/dev/null
wipefs -a "$DEV" >/dev/null 2>&1
sfdisk --wipe never "$DEV" </dev/null >/dev/null 2>&1 <<EOF
label: gpt
start=2048, size=2097152, type=linux, name="probe-a"
start=2101248, size=2097152, type=linux, name="probe-b"
EOF
udevadm settle --timeout=10
lsblk -no NAME,SIZE,TYPE "$DEV"
P1=${DEV}1
P2=${DEV}2

say "2. ext4 覆盖已有 ext4 签名（不加 -F，stdin 为空）"
wipe_fs "$P1"
run_mkfs "先做一次 ext4（无标签）" mkfs.ext4 "$P1"
run_mkfs "再一次 ext4，同一分区，不加 -F" mkfs.ext4 "$P1"

say "3. ext4 覆盖 xfs 签名（不加 -F）"
wipe_fs "$P1"
run_mkfs "先做 xfs 带标签" mkfs.xfs -f -L XFSLBL "$P1"
show_sb "$P1" xfs
run_mkfs "ext4 覆盖 xfs 签名，不加 -F" mkfs.ext4 "$P1"
show_sb "$P1" ext4

say "4. ext4 覆盖认不出来的非零内容（不加 -F）"
wipe_fs "$P2"
dd if=/dev/urandom of="$P2" bs=4096 count=8 conv=notrunc 2>/dev/null
run_mkfs "ext4 覆盖随机内容，不加 -F" mkfs.ext4 "$P2"
show_sb "$P2" ext4

say "5. 标签边界：ext4 -L 16 字节 / 17 字节"
wipe_fs "$P2"
run_mkfs "ext4 -L 16 字节" mkfs.ext4 -L 0123456789abcdef "$P2"
show_sb "$P2" ext4
wipe_fs "$P2"
run_mkfs "ext4 -L 17 字节" mkfs.ext4 -L 0123456789abcdefg "$P2"
show_sb "$P2" ext4

say "6. 标签边界：xfs -L 12 字节 / 13 字节"
wipe_fs "$P1"
run_mkfs "xfs -f -L 12 字节" mkfs.xfs -f -L 0123456789ab "$P1"
show_sb "$P1" xfs
wipe_fs "$P1"
run_mkfs "xfs -f -L 13 字节" mkfs.xfs -f -L 0123456789abc "$P1"
show_sb "$P1" xfs

say "7. xfs 覆盖已有 ext4 签名：不加 -f"
wipe_fs "$P1"
run_mkfs "先做 ext4" mkfs.ext4 "$P1"
run_mkfs "xfs 覆盖 ext4，不加 -f" mkfs.xfs "$P1"

say "8. 已挂载分区上 mkfs（错误退出验收用）"
wipe_fs "$P2"
mkfs.ext4 "$P2" >/dev/null 2>&1
mkdir -p "$MNT"
mount "$P2" "$MNT" && echo "  mounted $P2 -> $MNT"
run_mkfs "mkfs.ext4 覆盖已挂载的 ext4" mkfs.ext4 "$P2"
run_mkfs "mkfs.ext4 -F 覆盖已挂载的 ext4" mkfs.ext4 -F "$P2"
run_mkfs "mkfs.xfs -f 覆盖已挂载的分区" mkfs.xfs -f -L MOUNTED "$P2"
umount "$MNT"

say "9. 耗时（决定中断测试怎么做）"
for i in 1 2 3; do
    wipe_fs "$P1"
    s=$(date +%s%N)
    mkfs.ext4 -q "$P1" </dev/null >/dev/null 2>&1
    e=$(date +%s%N)
    printf '  mkfs.ext4 1GiB #%d: %d ms\n' "$i" $(( (e - s) / 1000000 ))
done
for i in 1 2 3; do
    wipe_fs "$P1"
    s=$(date +%s%N)
    mkfs.xfs -f -q "$P1" </dev/null >/dev/null 2>&1
    e=$(date +%s%N)
    printf '  mkfs.xfs  1GiB #%d: %d ms\n' "$i" $(( (e - s) / 1000000 ))
done

say "10. 收尾"
wipefs -a "$DEV" >/dev/null 2>&1
sfdisk --wipe never --delete "$DEV" >/dev/null 2>&1 </dev/null
wipefs -a "$DEV" >/dev/null 2>&1
udevadm settle --timeout=10
lsblk -no NAME,SIZE,TYPE "$DEV"
echo "PROBE_DONE"
