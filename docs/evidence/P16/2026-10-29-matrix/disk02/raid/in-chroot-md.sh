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
