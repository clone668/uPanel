#!/bin/bash
# Migrate the running VM-D root filesystem onto md0 = RAID1(/dev/sdb2, /dev/sdc2).
# Target layout (legacy BIOS boot, no ESP), on EACH member disk:
#   /dev/sdX1  1 MiB  bios_grub   (grub core.img, no filesystem)
#   /dev/sdX2  rest   linux_raid_member
# /dev/md0 = RAID1 of the two members -> ext4 -> /
# /boot lives inside the array, so the ONLY thing on each physical disk outside
# the array is a 1 MiB unmounted bios_grub partition.
set -euo pipefail

A=/dev/sdb
B=/dev/sdc
MD=/dev/md0
ARRAY_UUID=1a2b3c4d:5e6f7a8b:9c0d1e2f:3a4b5c6d
TARGET=/mnt/root

say() { echo "### $*"; }
die() { echo "!!! $*" >&2; exit 1; }

[ "$(id -u)" = 0 ] || die "must run as root"
[ -b "$A" ] || die "$A missing"
[ -b "$B" ] || die "$B missing"
findmnt -no SOURCE / | grep -qE "$A|$B" && die "a target disk backs the running root, refusing"

say "wipe $A $B"
mdadm --stop "$MD" >/dev/null 2>&1 || true
mdadm --stop /dev/md127 >/dev/null 2>&1 || true
for d in "$A" "$B"; do
  wipefs -a "$d" >/dev/null 2>&1 || true
  sgdisk --zap-all "$d" >/dev/null
  sgdisk -n 1:2048:+1M -t 1:ef02 -c 1:bios_grub "$d" >/dev/null
  sgdisk -n 2:0:0     -t 2:fd00 -c 2:raid      "$d" >/dev/null
done
partprobe "$A" "$B"
udevadm settle
sleep 1
lsblk -o NAME,SIZE,TYPE,PARTTYPENAME "$A" "$B"

say "mdadm --create RAID1"
mdadm --create "$MD" --level=1 --raid-devices=2 --metadata=1.2 \
  --uuid="$ARRAY_UUID" --name=root --homehost=any --run "$A"2 "$B"2
udevadm settle
for _ in $(seq 1 60); do
  state=$(cat "/sys/block/$(basename "$MD")/md/array_state" 2>/dev/null || echo "")
  [ "$state" = "clean" ] && break
  sleep 1
done
say "array_state=$(cat "/sys/block/$(basename "$MD")/md/array_state" 2>/dev/null)"
lsblk -o NAME,SIZE,TYPE,MOUNTPOINT "$A" "$B"
cat /proc/mdstat

mkfs.ext4 -q -L upanel-md-root "$MD"
mkdir -p "$TARGET"
mount "$MD" "$TARGET"

say "rsync / -> $TARGET (skips other mounts)"
rsync -aHAXx --numeric-ids --info=stats1 / "$TARGET"/
say "rsync /boot -> $TARGET/boot (skips /boot/efi)"
rsync -aHAXx --numeric-ids /boot/ "$TARGET"/boot/

for d in dev proc sys run; do
  mkdir -p "$TARGET/$d"
  mount --bind "/$d" "$TARGET/$d"
done
mount --bind /dev/pts "$TARGET/dev/pts"

say "chroot: fstab / mdadm.conf / initramfs / grub"
cat > "$TARGET/tmp/in-chroot-md.sh" <<'INCHROOT'
#!/bin/bash
set -euo pipefail
MD=/dev/md0
ARRAY_UUID=1a2b3c4d:5e6f7a8b:9c0d1e2f:3a4b5c6d
MD_UUID=$(blkid -s UUID -o value "$MD")
echo "MD=$MD fs_uuid=$MD_UUID"

cat > /etc/fstab <<EOF
UUID=$MD_UUID / ext4 discard,commit=30,errors=remount-ro 0 1
EOF
cat /etc/fstab

rm -rf /boot/efi
mkdir -p /boot
[ -f /etc/initramfs-tools/conf.d/resume ] && echo 'RESUME=none' > /etc/initramfs-tools/conf.d/resume

# mdadm 的配置要让 initramfs 里也能按 UUID 自动装配。
install -d -m 0755 /etc/mdadm
if ! grep -q "$ARRAY_UUID" /etc/mdadm/mdadm.conf 2>/dev/null; then
  { echo 'HOMEHOST <system>'
    mdadm --detail --scan
  } > /etc/mdadm/mdadm.conf
fi
cat /etc/mdadm/mdadm.conf

cat > /etc/initramfs-tools/scripts/local-top/zz-md-assemble <<'EOS'
#!/bin/sh
PREREQ=""
prereqs() { echo "$PREREQ"; }
case "$1" in
  prereqs) prereqs; exit 0 ;;
esac
modprobe -q raid1 2>/dev/null || true
modprobe -q md_mod 2>/dev/null || true
if command -v mdadm >/dev/null 2>&1; then
  mdadm --assemble --scan --run >/dev/null 2>&1 || true
fi
udevadm settle --timeout=30 2>/dev/null || true
exit 0
EOS
chmod 0755 /etc/initramfs-tools/scripts/local-top/zz-md-assemble

# grub 自己会写 root=UUID=<md0 的文件系统 uuid>
sed -i 's|^GRUB_CMDLINE_LINUX=.*|GRUB_CMDLINE_LINUX=""|' /etc/default/grub

export DEBIAN_FRONTEND=noninteractive
update-initramfs -u -k all
lsinitramfs /boot/initrd.img-$(ls /boot/vmlinuz-* | sed 's|.*vmlinuz-||' | sort -V | tail -1) \
  | grep -E 'mdadm|zz-md-assemble' | head -5 || echo "WARN: md bits not in initrd"

update-grub
grub-install --target=i386-pc --recheck --boot-directory=/boot /dev/sdb
grub-install --target=i386-pc --recheck --boot-directory=/boot /dev/sdc
echo "--- grub.cfg linux lines ---"
grep -n -E '^[[:space:]]*(linux|search)' /boot/grub/grub.cfg | head -8
INCHROOT
chmod 0755 "$TARGET/tmp/in-chroot-md.sh"
chroot "$TARGET" /tmp/in-chroot-md.sh

say "cleanup"
rm -f "$TARGET/tmp/in-chroot-md.sh"
for d in dev/pts dev proc sys run; do umount "$TARGET/$d" 2>/dev/null || true; done
sync
umount "$TARGET" 2>/dev/null || umount -l "$TARGET" 2>/dev/null || true
say "DONE"
