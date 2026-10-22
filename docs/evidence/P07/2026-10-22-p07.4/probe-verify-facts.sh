#!/bin/sh
# P07.4 事实测量，第二轮：findmnt --verify 的判定边界与 systemd 视角。
# 第一轮（probe-volume-facts.sh）已经确认了工具存在、noauto 条目能靠挂载点挂上、
# 繁忙卸载退出码 32、findmnt 能给出 UUID。这一轮只补“候选文件到底怎么才算通过”。
set -u
DISK=/dev/sdb
PART=${DISK}1
MNT=/srv/upanel/volumes/probe
MNT2=/srv/upanel/volumes/probe2
WORK=/root/p074-probe2
cd "$WORK" 2>/dev/null || { mkdir -p "$WORK"; cd "$WORK"; }

val() { printf 'VALUE %-40s %s\n' "$1" "$2"; }
rc() { printf 'RC    %-40s %s\n' "$1" "$2"; }
ver() { # ver <名称> <文件> ; 输出 rc 与摘要
  out=$(findmnt --verify --tab "$2" 2>&1); e=$?
  rc "verify $1" "$e"
  val "verify $1 输出" "$(printf '%s' "$out" | tr '\n' '|' | cut -c1-220)"
}

mkdir -p /srv/upanel/volumes
wipefs -a "$DISK" >/dev/null 2>&1
sgdisk --zap-all "$DISK" >/dev/null 2>&1
printf 'label: gpt\n' | sfdisk --wipe never "$DISK" >/dev/null 2>&1
printf 'start=2048, size=2097152, type=linux, name="probe"\n' |
  sfdisk --append --wipe-partitions never "$DISK" >/dev/null 2>&1
udevadm settle --timeout=10
mkfs.ext4 -q -L p074probe "$PART"
UUID=$(blkid -s UUID -o value "$PART")
val uuid "$UUID"

# 挂载点目录先建好：产品在写 fstab 之前就会 mkdir，所以“目录不存在”不构成本用例的输入。
mkdir -p "$MNT"
chmod 0755 "$MNT"

echo ""
echo "== A. 目录存在 + 设备在位 =="
printf 'UUID=%s %s ext4 rw,nodev,nosuid,nofail,x-systemd.device-timeout=30,noauto 0 2\n' "$UUID" "$MNT" >c1
ver "noauto+在位" c1
printf 'UUID=%s %s ext4 rw,nodev,nosuid,nofail,x-systemd.device-timeout=30 0 2\n' "$UUID" "$MNT" >c2
ver "自动挂载+在位" c2

echo ""
echo "== B. 设备不在位（UUID 不存在） =="
BAD=11111111-2222-3333-4444-555555555555
printf 'UUID=%s %s ext4 rw,nodev,nosuid,nofail,x-systemd.device-timeout=30,noauto 0 2\n' "$BAD" "$MNT" >c3
ver "noauto+不在位" c3
printf 'UUID=%s %s ext4 rw,nodev,nosuid,nofail,x-systemd.device-timeout=30 0 2\n' "$BAD" "$MNT" >c4
ver "自动挂载+不在位(有nofail)" c4
printf 'UUID=%s %s ext4 rw,nodev,nosuid,x-systemd.device-timeout=30 0 2\n' "$BAD" "$MNT" >c5
ver "自动挂载+不在位(无nofail)" c5

echo ""
echo "== C. 未知选项能不能被抓到 =="
printf 'UUID=%s %s ext4 rw,nodev,nosuid,nofail,bogusopt 0 2\n' "$UUID" "$MNT" >c6
ver "未知选项bogusopt" c6
printf 'UUID=%s %s ext4 rw,nodev,nosuid,nofail,dev,suid 0 2\n' "$UUID" "$MNT" >c7
ver "合法但少见的选项" c7
printf 'UUID=%s %s ext4 0 2\n' "$UUID" "$MNT" >c8
ver "没有选项列（结构错）" c8
printf 'UUID=%s %s zfs defaults 0 0\n' "$UUID" "$MNT" >c9
ver "未知文件系统类型" c9
printf 'UUID=%s /srv/upanel/volumes/a /srv/upanel/volumes/b ext4 rw 0 2\n' "$UUID" >c10
ver "字段错位" c10

echo ""
echo "== D. 同一挂载点两行 / 同一 UUID 两行 =="
printf 'UUID=%s %s ext4 rw,nofail,noauto 0 2\nUUID=%s %s ext4 rw,nofail,noauto 0 2\n' \
  "$UUID" "$MNT" "$BAD" "$MNT" >c11
ver "同一挂载点两行" c11
printf 'UUID=%s %s ext4 rw,nofail,noauto 0 2\nUUID=%s %s ext4 rw,nofail,noauto 0 2\n' \
  "$UUID" "$MNT" "$UUID" "$MNT2" >c12
ver "同一UUID两行" c12

echo ""
echo "== E. 真挂载/卸载的边界 =="
cp /etc/fstab /root/fstab.orig2
trap 'cp /root/fstab.orig2 /etc/fstab; umount "$MNT" 2>/dev/null; systemctl daemon-reload; rmdir "$MNT" 2>/dev/null' EXIT INT TERM
cp c2 /etc/fstab           # 自动挂载条目（无 noauto）
systemctl daemon-reload >/dev/null 2>&1
unit=$(systemd-escape -p --suffix=mount "$MNT")
val "挂载单元名" "$unit"
val "单元已生成吗" "$(systemctl show -p LoadState --value "$unit" 2>&1)"
out=$(mount "$MNT" 2>&1); e=$?
rc "mount 自动挂载条目" "$e"
val "挂载后 findmnt UUID" "$(findmnt -no UUID "$MNT" 2>&1)"
val "单 元活跃状态" "$(systemctl is-active "$unit" 2>&1)"
out=$(umount "$MNT" 2>&1); e=$?
rc "umount 已挂载" "$e"
out=$(umount "$MNT" 2>&1); e=$?
rc "umount 未挂载" "$e"
val "umount 未挂载 输出" "$(printf '%s' "$out" | tr '\n' '|' | cut -c1-160)"
val "单元变为" "$(systemctl is-active "$unit" 2>&1)"

echo ""
echo "== F. 没有 fstab 条目时按挂载点挂 =="
cp /root/fstab.orig2 /etc/fstab
systemctl daemon-reload >/dev/null 2>&1
out=$(mount "$MNT" 2>&1); e=$?
rc "mount 无条目" "$e"
val "mount 无条目 输出" "$(printf '%s' "$out" | tr '\n' '|' | cut -c1-160)"

echo ""
echo "== G. 目录不存在时按挂载点挂（产品会先 mkdir） =="
rmdir "$MNT" 2>/dev/null
out=$(mount "$MNT" 2>&1); e=$?
rc "mount 目录不存在" "$e"
val "mount 目录不存在 输出" "$(printf '%s' "$out" | tr '\n' '|' | cut -c1-160)"
mkdir -p "$MNT"; chmod 0755 "$MNT"

echo ""
echo "== H. daemon-reload 是否会清掉 that 'fstab 已修改' 警告 =="
cp c2 /etc/fstab
val "reload 前" "$(findmnt --verify --tab /etc/fstab 2>&1 | grep -c 'modified' )"
systemctl daemon-reload >/dev/null 2>&1
val "reload 后" "$(findmnt --verify --tab /etc/fstab 2>&1 | grep -c 'modified' )"
cp /root/fstab.orig2 /etc/fstab
systemctl daemon-reload >/dev/null 2>&1
val "fstab 已复原" "$(cmp -s /root/fstab.orig2 /etc/fstab && echo yes || echo NO)"

echo ""
echo "== I. 清理 =="
umount "$MNT" 2>/dev/null
rmdir "$MNT" 2>/dev/null
rmdir /srv/upanel/volumes 2>/dev/null
wipefs -a "$PART" >/dev/null 2>&1
sgdisk --zap-all "$DISK" >/dev/null 2>&1
wipefs -a "$DISK" >/dev/null 2>&1
val "sdb 擦回空白" "$(sgdisk -p "$DISK" 2>&1 | tail -1)"
echo "PROBE2 DONE"
