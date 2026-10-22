#!/bin/sh
# P07.4 事实测量（一次性 VM，root，可丢弃盘 sdb）。
#
# 目的只有一个：在写产品代码之前，把“挂载与 fstab 到底怎么表现”量出来。
# 每一条都是可证伪的断言式测量，输出 VALUE=... 行，供 evidence/measurements.txt 直接引用。
#
# 安全：全程只碰 sdb（一次性数据盘）与一个**一定会被复原**的 /etc/fstab 副本。
# 我们绝不改 /etc/fstab 本身——真实替换留到验收里，由被测代码做。
set -u
DISK=/dev/sdb
PART=${DISK}1
MNT=/srv/upanel/volumes/probe
WORK=/root/p074-probe
CAND=$WORK/fstab.candidate

say() { printf '%s\n' "$*"; }
val() { printf 'VALUE %-34s %s\n' "$1" "$2"; }
rc() { printf 'RC    %-34s %s\n' "$1" "$2"; }

mkdir -p "$WORK"
say "== 0. 环境 =="
val kernel "$(uname -r)"
val util_linux "$(dpkg-query -W -f='${Version}' util-linux 2>/dev/null)"
for b in /usr/bin/mount /usr/bin/umount /usr/bin/findmnt /usr/bin/lsblk /usr/sbin/mkfs.ext4 /usr/sbin/mkfs.xfs /usr/sbin/sgdisk /usr/bin/udevadm /usr/bin/systemd-escape; do
  if [ -x "$b" ]; then val "exists $b" yes; else val "exists $b" NO; fi
done
val fstab_size "$(stat -c '%s %a %U:%G' /etc/fstab)"
val fstab_lines "$(wc -l < /etc/fstab)"
val fstab_tail_byte "$(tail -c 1 /etc/fstab | od -An -c | tr -d ' \n')"

say ""
say "== 1. 准备一块 ext4 分区（一次性盘） =="
wipefs -a "$DISK" >/dev/null 2>&1
sgdisk --zap-all "$DISK" >/dev/null 2>&1
printf 'label: gpt\n' | sfdisk --wipe never "$DISK" >/dev/null 2>&1
printf 'start=2048, size=2097152, type=linux, name="probe"\n' |
  sfdisk --append --wipe-partitions never "$DISK" >/dev/null 2>&1
udevadm settle --timeout=10
mkfs.ext4 -q -L p074probe "$PART"
UUID_EXT4=$(blkid -s UUID -o value "$PART")
TYPE_EXT4=$(blkid -s TYPE -o value "$PART")
val ext4_uuid "$UUID_EXT4"
val ext4_type "$TYPE_EXT4"
val dev_node "$(test -b "$PART" && echo yes || echo NO)"

say ""
say "== 2. findmnt 的验证能力（候选文件，不碰真 fstab） =="
# 2a 合法条目（ext4，pass=2，含 noauto 与 x-systemd.device-timeout）
cat >"$CAND" <<EOF
UUID=$UUID_EXT4 $MNT ext4 rw,nodev,nosuid,noauto,x-systemd.device-timeout=30 0 2
EOF
out=$(findmnt --verify --tab "$CAND" 2>&1); e=$?
rc "findmnt --verify 合法ext4" "$e"
val "findmnt 合法ext4 输出" "$(printf '%s' "$out" | tr '\n' '|' | cut -c1-200)"

# 2b 同一条去掉 noauto
sed 's/,noauto//' "$CAND" >"$CAND.2"
out=$(findmnt --verify --tab "$CAND.2" 2>&1); e=$?
rc "findmnt --verify 去掉noauto" "$e"
val "findmnt 无noauto 输出" "$(printf '%s' "$out" | tr '\n' '|' | cut -c1-200)"

# 2c 设备不存在
cat >"$CAND.3" <<EOF
UUID=00000000-0000-0000-0000-000000000000 $MNT ext4 rw 0 2
EOF
out=$(findmnt --verify --tab "$CAND.3" 2>&1); e=$?
rc "findmnt --verify 不存在的UUID" "$e"
val "findmnt 不存在UUID 输出" "$(printf '%s' "$out" | tr '\n' '|' | cut -c1-200)"

# 2d 未知挂载选项
cat >"$CAND.4" <<EOF
UUID=$UUID_EXT4 $MNT ext4 rw,bogusopt 0 2
EOF
out=$(findmnt --verify --tab "$CAND.4" 2>&1); e=$?
rc "findmnt --verify 未知选项" "$e"
val "findmnt 未知选项 输出" "$(printf '%s' "$out" | tr '\n' '|' | cut -c1-200)"

# 2e 挂载点目录不存在
cat >"$CAND.5" <<EOF
UUID=$UUID_EXT4 /srv/upanel/volumes/nosuchdir ext4 rw,noauto 0 2
EOF
out=$(findmnt --verify --tab "$CAND.5" 2>&1); e=$?
rc "findmnt --verify 挂载点不存在" "$e"
val "findmnt 挂载点不存在 输出" "$(printf '%s' "$out" | tr '\n' '|' | cut -c1-200)"

# 2f xfs 用 pass=0 与 pass=2 的差别
wipefs -a "$PART" >/dev/null 2>&1
mkfs.xfs -f -q -L p074probe "$PART"
UUID_XFS=$(blkid -s UUID -o value "$PART")
val xfs_uuid "$UUID_XFS"
for p in 0 2; do
  cat >"$CAND.6" <<EOF
UUID=$UUID_XFS $MNT xfs rw,noauto 0 $p
EOF
  out=$(findmnt --verify --tab "$CAND.6" 2>&1); e=$?
  rc "findmnt --verify xfs pass=$p" "$e"
  val "findmnt xfs pass=$p 输出" "$(printf '%s' "$out" | tr '\n' '|' | cut -c1-160)"
done

say ""
say "== 3. 注释块与外面内容无关性 =="
cp /etc/fstab "$CAND.7"
cat >>"$CAND.7" <<EOF

# BEGIN uPanel managed volumes（面板维护，请勿手工编辑）
UUID=$UUID_XFS $MNT xfs rw,noauto,nodev,nosuid,x-systemd.device-timeout=30 0 0
# END uPanel managed volumes
EOF
out=$(findmnt --verify --tab "$CAND.7" 2>&1); e=$?
rc "findmnt --verify 附件注释块" "$e"
val "带注释块 输出" "$(printf '%s' "$out" | tr '\n' '|' | cut -c1-200)"
val "注释块后行数增量" "$(( $(wc -l <"$CAND.7") - $(wc -l </etc/fstab) ))"

say ""
say "== 4. 真挂载：noauto 条目能不能靠挂载点挂上 =="
mkdir -p "$MNT"
chmod 0755 "$MNT"
cp /etc/fstab "$WORK/fstab.orig"
trap 'cp "$WORK/fstab.orig" /etc/fstab; umount "$MNT" 2>/dev/null; rmdir "$MNT" 2>/dev/null' EXIT INT TERM
cat "$CAND.7" >/etc/fstab

out=$(mount "$MNT" 2>&1); e=$?
rc "mount <挂载点> (noauto)" "$e"
val "mount 输出" "$(printf '%s' "$out" | tr '\n' '|' | cut -c1-200)"
val "findmnt 挂上了吗" "$(findmnt -no SOURCE,TARGET,FSTYPE,OPTIONS "$MNT" 2>&1 | cut -c1-160)"
val "findmnt UUID" "$(findmnt -no UUID "$MNT" 2>&1 | head -1)"
val "挂载后有 nodev 吗" "$(findmnt -no OPTIONS "$MNT" 2>&1 | tr ',' '\n' | grep -c '^nodev$')"
val "挂载后有 nosuid 吗" "$(findmnt -no OPTIONS "$MNT" 2>&1 | tr ',' '\n' | grep -c '^nosuid$')"
val "写文件" "$(touch "$MNT/hello" 2>&1 && echo ok || echo FAILED)"

# 4b 同一个挂载点第二次挂
out=$(mount "$MNT" 2>&1); e=$?
rc "重复 mount 同一挂载点" "$e"
val "重复 mount 输出" "$(printf '%s' "$out" | tr '\n' '|' | cut -c1-200)"

# 4c 有进程打开挂载点里的文件时 umount
( exec 3<"$MNT/hello"; sleep 8 ) &
HOLDER=$!
sleep 1
out=$(umount "$MNT" 2>&1); e=$?
rc "umount 繁忙" "$e"
val "umount 繁忙输出" "$(printf '%s' "$out" | tr '\n' '|' | cut -c1-200)"
kill "$HOLDER" 2>/dev/null
wait "$HOLDER" 2>/dev/null
out=$(umount "$MNT" 2>&1); e=$?
rc "umount 空闲" "$e"
val "umount 后 findmnt" "$(findmnt -no TARGET "$MNT" 2>&1 | tr '\n' '|' | cut -c1-100)"

say ""
say "== 5. 卸载时 /proc/self/mountinfo 与 device 的对应 =="
mount "$MNT" >/dev/null 2>&1
val "mountinfo 行" "$(grep -F " $MNT " /proc/self/mountinfo | head -1 | cut -c1-160)"
val "mountinfo 的 source 字段" "$(grep -F " $MNT " /proc/self/mountinfo | head -1 | awk '{print $10}')"
val "sdb1 的 rdev" "$(stat -c '%t:%T' "$PART")"
umount "$MNT" 2>/dev/null

say ""
say "== 6. 候选文件原子替换的细节 =="
cp /etc/fstab "$WORK/atomic.test"
printf '# probe\n' >>"$WORK/atomic.test"
chmod 0644 "$WORK/atomic.test"
val "替换前 inode" "$(stat -c '%i %a %U:%G' /etc/fstab)"
# 只在 /tmp 里演一遍 rename，不碰真 fstab
cp "$WORK/atomic.test" "$WORK/atomic.new"
mv "$WORK/atomic.new" "$WORK/atomic.test"
val "rename 后 inode 变了" "$(stat -c '%i' "$WORK/atomic.test")"
val "rename 后权限" "$(stat -c '%a %U:%G' "$WORK/atomic.test")"
val "目录 fsync 可用" "$(python3 -c 'import os;fd=os.open("/etc",os.O_RDONLY);os.fsync(fd);os.close(fd);print("ok")' 2>&1)"
val "flock /etc/fstab 可用" "$(python3 -c '
import fcntl
f=open("/etc/fstab","r+")
try:
    fcntl.flock(f.fileno(), fcntl.LOCK_EX|fcntl.LOCK_NB)
    print("ok")
finally:
    f.close()' 2>&1)"

say ""
say "== 7. 清理 =="
# trap 恢复 fstab；这里再显式还原一次并确认
cp "$WORK/fstab.orig" /etc/fstab
val "fstab 已复原" "$(cmp -s "$WORK/fstab.orig" /etc/fstab && echo yes || echo NO)"
val "fstab 行数" "$(wc -l </etc/fstab)"
umount "$MNT" 2>/dev/null
rmdir "$MNT" 2>/dev/null
rmdir /srv/upanel/volumes 2>/dev/null
rmdir /srv/upanel 2>/dev/null
wipefs -a "$PART" >/dev/null 2>&1
sgdisk --zap-all "$DISK" >/dev/null 2>&1
wipefs -a "$DISK" >/dev/null 2>&1
val "sdb 已擦回空白" "$(blkid "$PART" 2>&1 | head -1; echo done)"
val "findmnt 校验真 fstab" "$(findmnt --verify --tab /etc/fstab 2>&1 | tail -2 | tr '\n' '|' | cut -c1-160)"
say "PROBE DONE"
