#!/bin/sh
# 一次性诊断：dm holder 在 sysfs 里的可见性。
set -u
sgdisk --clear --new=1:0:+1024M --new=2:0:+512M /dev/sdb >/dev/null 2>&1
udevadm settle --timeout=5
SZ=$(blockdev --getsz /dev/sdb1)
echo "sdb1 sectors: $SZ"
printf '0 %s linear /dev/sdb1 0\n' "$SZ" > /tmp/probe-dm.table
dmsetup create probe-holder --table "$(cat /tmp/probe-dm.table)"
echo "dmsetup: $?"
udevadm settle --timeout=5
echo "sdb1 holders dir: [$(ls /sys/block/sdb1/holders 2>&1)]"
echo "dm-0 slaves: [$(cat /sys/block/dm-0/slaves 2>/dev/null)]"
echo "dmsetup ls: $(dmsetup ls 2>&1)"
dmsetup remove probe-holder 2>/dev/null
echo "after remove: [$(ls /sys/block/sdb1/holders 2>&1)]"
