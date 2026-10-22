#!/bin/bash
# P08.2 设计探针 3：open_files 的形状；smbd 未运行时的行为。
set -u
PASS='ProbePass123'
U=p08probe
D=/srv/p08probe

cleanup() {
  sudo -n systemctl start smbd 2>/dev/null
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
sudo -n truncate -s 1500M "$D/big.bin"
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

echo "=== 保持一个打开的文件（get 大文件）"
( printf 'get big.bin /dev/null\n'; sleep 25 ) | smbclient "//127.0.0.1/$U" -U "$U%$PASS" >/tmp/hold3.log 2>&1 &
HOLD=$!
sleep 3
echo "=== open_files 形状"
sudo -n smbstatus --json 2>/dev/null | python3 -c 'import json,sys; d=json.load(sys.stdin); print(json.dumps({"sessions":d.get("sessions"),"tcons":d.get("tcons"),"open_files":d.get("open_files")}, indent=1)[:4000])'
echo "=== 原始 open_files"
sudo -n smbstatus --json 2>/dev/null | sed 's/.*"open_files"/"open_files"/'
echo
kill $HOLD 2>/dev/null; wait $HOLD 2>/dev/null

echo "=== smbd 停止后的 smbstatus"
sudo -n systemctl stop smbd
sleep 1
sudo -n smbstatus --json; echo "rc=$?"
sudo -n smbcontrol all reload-config; echo "smbcontrol rc=$?"
sudo -n systemctl is-active smbd; echo "is-active rc=$?"
sudo -n systemctl start smbd
sleep 1
sudo -n systemctl is-active smbd
echo "=== end"
