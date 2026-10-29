#!/bin/bash
# Migrate the running VM-C root filesystem onto /dev/sdb inside LVM.
# Target layout (legacy BIOS boot, no ESP):
#   /dev/sdb1  1 MiB  bios_grub   (grub core.img, holds no filesystem)
#   /dev/sdb2  rest   PV for VG upanel-vg -> LV root -> ext4 -> /
# /boot lives inside the LV (no separate partition), so the ONLY thing on the
# physical disk outside LVM is a 1 MiB unmounted bios_grub partition.
set -euo pipefail

VG=upanel-vg
LV=root
DISK=/dev/sdb
TARGET=/mnt/root

say() { echo "### $*"; }
die() { echo "!!! $*" >&2; exit 1; }

[ "$(id -u)" = 0 ] || die "must run as root"
[ -b "$DISK" ] || die "$DISK missing"
SECTORS=$(blockdev --getsz "$DISK")
say "disk=$DISK sectors=$SECTORS ($((SECTORS/2/1024/1024)) GiB)"
[ "$SECTORS" -gt 40000000 ] || die "unexpected disk size"
findmnt -no SOURCE / | grep -q "$DISK" && die "$DISK is the running root, refusing"

say "wiping $DISK"
# 前一次跑到一半留下的 VG/LV 必须先拆掉，否则 vgcreate 会撞同名。
vgchange -an "$VG" >/dev/null 2>&1 || true
lvremove -y -f "$VG/$LV" >/dev/null 2>&1 || true
vgremove -y -f "$VG" >/dev/null 2>&1 || true
pvremove -y -ff "$DISK"2 >/dev/null 2>&1 || true
wipefs -a "$DISK" >/dev/null 2>&1 || true
sgdisk --zap-all "$DISK" >/dev/null
sgdisk -n 1:2048:+1M -t 1:ef02 -c 1:bios_grub "$DISK" >/dev/null
sgdisk -n 2:0:0     -t 2:8e00 -c 2:lvm       "$DISK" >/dev/null
partprobe "$DISK"
udevadm settle
sleep 1
lsblk -o NAME,SIZE,TYPE,PARTTYPENAME "$DISK"

say "pvcreate / vgcreate / lvcreate"
pvcreate -y -ff "$DISK"2
vgcreate -y "$VG" "$DISK"2
lvcreate -y -l 100%FREE -n "$LV" "$VG"
udevadm settle
LVDEV="/dev/$VG/$LV"
say "LVDEV=$LVDEV"
[ -b "$LVDEV" ] || die "LV device $LVDEV not present"

mkfs.ext4 -q -L upanel-root "$LVDEV"
mkdir -p "$TARGET"
mount "$LVDEV" "$TARGET"

say "rsync / -> $TARGET (skips other mounts)"
rsync -aHAXx --numeric-ids --info=stats2 / "$TARGET"/
say "rsync /boot -> $TARGET/boot (skips /boot/efi)"
rsync -aHAXx --numeric-ids /boot/ "$TARGET"/boot/

for d in dev proc sys run; do
  mkdir -p "$TARGET/$d"
  mount --bind "/$d" "$TARGET/$d"
done
mount --bind /dev/pts "$TARGET/dev/pts"

say "chroot: fstab / initramfs hook / grub"
cat > "$TARGET/tmp/in-chroot.sh" <<'INCHROOT'
#!/bin/bash
set -euo pipefail
VG=upanel-vg
LV=root
LVDEV="/dev/$VG/$LV"
UUID=$(blkid -s UUID -o value "$LVDEV")
echo "LVDEV=$LVDEV UUID=$UUID"

# Only / on the LV. No /boot partition, no ESP.
cat > /etc/fstab <<EOF
UUID=$UUID / ext4 discard,commit=30,errors=remount-ro 0 1
EOF
cat /etc/fstab

rm -rf /boot/efi
mkdir -p /boot

# no swap to resume from
if [ -f /etc/initramfs-tools/conf.d/resume ]; then
  echo 'RESUME=none' > /etc/initramfs-tools/conf.d/resume
fi

# deterministic VG activation before root resolution
cat > /etc/initramfs-tools/scripts/local-top/zz-lvm-activate <<'EOS'
#!/bin/sh
PREREQ=""
prereqs() { echo "$PREREQ"; }
case "$1" in
  prereqs) prereqs; exit 0 ;;
esac
modprobe -q dm-mod 2>/dev/null || true
if command -v vgchange >/dev/null 2>&1; then
  vgchange -ay --sysinit >/dev/null 2>&1 || true
elif [ -x /sbin/lvm ]; then
  /sbin/lvm vgchange -ay --sysinit >/dev/null 2>&1 || true
fi
exit 0
EOS
chmod 0755 /etc/initramfs-tools/scripts/local-top/zz-lvm-activate

sed -i 's|^GRUB_CMDLINE_LINUX=.*|GRUB_CMDLINE_LINUX="rd.lvm.vg='"$VG"' rd.lvm.lv='"$VG/$LV"'"|' /etc/default/grub
grep '^GRUB_CMDLINE_LINUX=' /etc/default/grub

export DEBIAN_FRONTEND=noninteractive
update-initramfs -u -k all
lsinitramfs /boot/initrd.img-$(ls /boot/vmlinuz-* | sed 's|.*vmlinuz-||' | sort -V | tail -1) \
  | grep -E 'sbin/lvm|local-top/zz-lvm-activate' || echo "WARN: lvm bits not in initrd"

update-grub
grub-install --target=i386-pc --recheck --boot-directory=/boot /dev/sdb
echo "--- grub.cfg linux lines ---"
grep -n -E '^\s*(linux|search)' /boot/grub/grub.cfg | head -20
INCHROOT
chmod 0755 "$TARGET/tmp/in-chroot.sh"
chroot "$TARGET" /tmp/in-chroot.sh

say "cleanup"
rm -f "$TARGET/tmp/in-chroot.sh"
for d in dev/pts dev proc sys run; do umount "$TARGET/$d" 2>/dev/null || true; done
sync
umount "$TARGET"
vgchange -an "$VG" || true
say "DONE"
