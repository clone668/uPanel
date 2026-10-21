#!/bin/sh
# 临时诊断脚本（不进仓库）：回答两个问题。
#   1. 整盘 mkswap 到底把 SWAPSPACE2 写在哪里？（产品签名识别漏判 swap 的原因）
#   2. dm linear 盖在分区上时，分区的 sysfs holders 是否真的非空？
#      （产品快照里 holders 为空的原因）
set -u

D_SWAP=/dev/sdd
D_DM=/dev/sdc

echo "== 0. 环境 =="
uname -r
blockdev --getsize64 "$D_SWAP" "$D_DM" 2>/dev/null || true
getconf PAGESIZE

echo
echo "== 1. 整盘 mkswap =="
wipefs -a "$D_SWAP" >/dev/null 2>&1
sgdisk --zap-all "$D_SWAP" >/dev/null 2>&1
dd if=/dev/zero of="$D_SWAP" bs=1M count=8 conv=fsync >/dev/null 2>&1
blockdev --rereadpt "$D_SWAP" >/dev/null 2>&1
udevadm settle --timeout=10 >/dev/null 2>&1
mkswap -f -L p07swap "$D_SWAP"
echo "mkswap rc=$?"

python3 - "$D_SWAP" <<'PY'
import sys
path = sys.argv[1]
size = 0
with open("/sys/class/block/%s/size" % path.rsplit("/", 1)[-1]) as f:
    size = int(f.read().strip()) * 512
print("device size:", size)
windows = [("head", 0, min(size, 0x20000)), ("tail", max(0, size - 0x10000), size)]
with open(path, "rb") as f:
    for name, start, end in windows:
        f.seek(start)
        buf = f.read(end - start)
        print("%s window: %d bytes from %d, nonzero=%d" % (name, len(buf), start, sum(1 for b in buf if b)))
        for magic in (b"SWAPSPACE2", b"SWAP-SPACE"):
            idx, hits = 0, []
            while True:
                i = buf.find(magic, idx)
                if i < 0:
                    break
                hits.append(start + i)
                idx = i + 1
            print("  %s at %s" % (magic.decode(), hits or "not found"))
        # 顺带看看头两个扇区里有什么
        if name == "head":
            nz = [i for i, b in enumerate(buf[:4096]) if b]
            print("  first nonzero head offsets (<=16):", nz[:16])
PY

echo "-- blkid / wipefs 的看法"
blkid "$D_SWAP" || echo "blkid: 无输出"
wipefs -n "$D_SWAP" || echo "wipefs: 无输出"
echo "-- 内核是否认这个交换区（swapon 成功即证明魔数有效）"
if swapon "$D_SWAP" 2>&1; then
  swapon --show
  swapoff "$D_SWAP"
  echo "swapon/swapoff OK"
else
  echo "swapon 失败"
fi

echo
echo "== 2. 分区上的 dm holder =="
wipefs -a "$D_DM" >/dev/null 2>&1
sgdisk --zap-all "$D_DM" >/dev/null 2>&1
sgdisk --clear --new=1:2048:+256M "$D_DM" >/dev/null 2>&1
blockdev --rereadpt "$D_DM" >/dev/null 2>&1
udevadm settle --timeout=10 >/dev/null 2>&1
P="${D_DM}1"
ls -l "$P" || echo "分区不存在"
echo "-- 建 dm 之前的 holders"
ls -l "/sys/block/$(basename "$D_DM")/$(basename "$P")/holders" 2>&1
cat "/sys/block/$(basename "$D_DM")/$(basename "$P")/holders/list" 2>&1 || echo "(list 读不到)"

SZ=$(blockdev --getsz "$P")
echo "分区扇区数: $SZ"
if printf '0 %s linear %s 0\n' "$SZ" "$P" | dmsetup create p07dm 2>&1; then
  echo "dmsetup create rc=0"
else
  echo "dmsetup create 失败"
fi
sleep 2
echo "-- 建 dm 之后的 holders"
ls -l "/sys/block/$(basename "$D_DM")/$(basename "$P")/holders" 2>&1
cat "/sys/block/$(basename "$D_DM")/$(basename "$P")/holders/list" 2>&1 || echo "(list 读不到)"
echo "-- /sys/block/dm-*/slaves"
ls -d /sys/block/dm-* 2>/dev/null || echo "没有 dm-* 目录"
for d in /sys/block/dm-*; do
  [ -d "$d" ] || continue
  echo "$d: uevent-name=$(cat "$d/dm/name" 2>/dev/null) slaves=$(cat "$d/slaves/list" 2>/dev/null | tr '\n' ' ')"
done
dmsetup ls 2>&1
echo "-- dmsetup table"
dmsetup table 2>&1
dmsetup remove p07dm 2>&1
sleep 1
echo "-- 移除之后"
cat "/sys/block/$(basename "$D_DM")/$(basename "$P")/holders/list" 2>&1 || echo "(list 读不到)"

echo
echo "== 3. 收尾：两块盘擦回空白 =="
wipefs -a "$D_DM" >/dev/null 2>&1
sgdisk --zap-all "$D_DM" >/dev/null 2>&1
dd if=/dev/zero of="$D_DM" bs=1M count=8 conv=fsync >/dev/null 2>&1
wipefs -a "$D_SWAP" >/dev/null 2>&1
sgdisk --zap-all "$D_SWAP" >/dev/null 2>&1
dd if=/dev/zero of="$D_SWAP" bs=1M count=8 conv=fsync >/dev/null 2>&1
blockdev --rereadpt "$D_DM" >/dev/null 2>&1
blockdev --rereadpt "$D_SWAP" >/dev/null 2>&1
udevadm settle --timeout=10 >/dev/null 2>&1
lsblk -o NAME,SIZE,TYPE,FSTYPE,MOUNTPOINTS
echo "done"
