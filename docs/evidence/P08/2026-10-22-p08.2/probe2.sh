#!/bin/bash
# P08.2 设计探针 2：抓一条**活着的** SMB 连接下的 smbstatus --json 形状。
set -u
PASS='ProbePass123'
U=p08probe
D=/srv/p08probe

cleanup() {
  sudo -n cp -a /root/smb.conf.p08.2-orig /etc/samba/smb.conf
  sudo -n rm -f /etc/samba/p08probe.conf
  sudo -n smbcontrol all reload-config 2>/dev/null
  sudo -n smbpasswd -x "$U" >/dev/null 2>&1
  sudo -n userdel "$U" >/dev/null 2>&1
  sudo -n rm -rf "$D"
}
trap cleanup EXIT

sudo -n useradd --system --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin --user-group "$U"
printf '%s\n%s\n' "$PASS" "$PASS" | sudo -n smbpasswd -s -a "$U"
sudo -n mkdir -p "$D"
sudo -n chmod 0700 "$D"
sudo -n setfacl --set u::rwx,g::---,o::---,u:$U:rwx "$D"
sudo -n tee /etc/samba/p08probe.conf >/dev/null <<EOF
[global]
   server min protocol = SMB2
   map to guest = never

[$U]
   path = $D
   browseable = yes
   guest ok = no
   read only = no
   valid users = $U
EOF
printf '\ninclude = /etc/samba/p08probe.conf\n' | sudo -n tee -a /etc/samba/smb.conf >/dev/null
sudo -n smbcontrol all reload-config

echo "=== 打开一条保持的连接（stdin 不关闭）"
mkdir -p /tmp/holddir
printf 'keep\n' > /tmp/holddir/keep.txt
( printf 'ls\n'; sleep 15 ) | smbclient "//127.0.0.1/$U" -U "$U%$PASS" >/tmp/hold.log 2>&1 &
HOLD=$!
sleep 3

echo "=== smbstatus --json"
sudo -n smbstatus --json > /tmp/smbstatus.json 2>/dev/null
cat /tmp/smbstatus.json | python3 -m json.tool 2>/dev/null || cat /tmp/smbstatus.json
echo
echo "=== smbstatus --json 原文（不格式化）"
cat /tmp/smbstatus.json
echo
echo "=== smbstatus -S 文本"
sudo -n smbstatus -S 2>/dev/null
echo "=== smbstatus -p 文本"
sudo -n smbstatus -p 2>/dev/null | head
echo "=== smbstatus 默认输出"
sudo -n smbstatus 2>/dev/null | head -30
wait $HOLD 2>/dev/null
echo "=== 连接关闭后再取一次"
sleep 2
sudo -n smbstatus --json 2>/dev/null
echo
