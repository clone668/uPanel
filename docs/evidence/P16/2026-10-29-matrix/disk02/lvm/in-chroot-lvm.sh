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
grep -n -E '^[[:space:]]*(linux|search)' /boot/grub/grub.cfg | head -20
