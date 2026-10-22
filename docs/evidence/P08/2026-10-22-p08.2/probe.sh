#!/bin/bash
# P08.2 设计探针：在真机上确认 smbstatus --json 的形状、testparm 读回的能力与 reload 的效果。
# 只在这台一次性测试 VM 上跑；结束时会把自己造的东西全部拆掉。
set -u
PASS='ProbePass123'
U=p08probe
D=/srv/p08probe

cleanup() {
  sudo -n sed -i '/^# p08probe include$/,$d' /etc/samba/smb.conf 2>/dev/null
  sudo -n cp -a /root/smb.conf.p08.2-orig /etc/samba/smb.conf
  sudo -n rm -f /etc/samba/p08probe.conf
  sudo -n smbcontrol all reload-config 2>/dev/null
  sudo -n smbpasswd -x "$U" >/dev/null 2>&1
  sudo -n userdel "$U" >/dev/null 2>&1
  sudo -n rm -rf "$D"
}
trap cleanup EXIT

echo "=== 1 prepare user + dir"
sudo -n useradd --system --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin --user-group "$U"
printf '%s\n%s\n' "$PASS" "$PASS" | sudo -n smbpasswd -s -a "$U"
sudo -n mkdir -p "$D"
sudo -n chmod 0700 "$D"
sudo -n setfacl --set u::rwx,g::---,o::---,u:$U:rwx "$D"

echo "=== 2 probe config"
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
printf '\n# p08probe include\ninclude = /etc/samba/p08probe.conf\n' | sudo -n tee -a /etc/samba/smb.conf >/dev/null

echo "=== 3 testparm on candidate (valid)"
sudo -n testparm -s /etc/samba/p08probe.conf >/tmp/tp.out 2>/tmp/tp.err; echo "rc=$?"
echo "--out--"; cat /tmp/tp.out; echo "--err--"; cat /tmp/tp.err

echo "=== 4 testparm merged"
sudo -n testparm -s /etc/samba/smb.conf >/tmp/tp2.out 2>/tmp/tp2.err; echo "rc=$?"
echo "--err--"; cat /tmp/tp2.err
echo "--map to guest--"; sudo -n testparm -s --parameter-name="map to guest" /etc/samba/smb.conf 2>/dev/null
echo "--min protocol--"; sudo -n testparm -s --parameter-name="server min protocol" /etc/samba/smb.conf 2>/dev/null
echo "--path of share--"; sudo -n testparm -s --section-name="$U" --parameter-name="path" /etc/samba/smb.conf 2>/dev/null
echo "--valid users--"; sudo -n testparm -s --section-name="$U" --parameter-name="valid users" /etc/samba/smb.conf 2>/dev/null

echo "=== 5 reload"
sudo -n smbcontrol all reload-config; echo "rc=$?"
sudo -n systemctl is-active smbd

echo "=== 6 real client: list + write"
smbclient "//127.0.0.1/$U" -U "$U%$PASS" -c 'ls' 2>&1 | head -20
echo "rc=$?"
printf 'hello from p08.2 probe\n' > /tmp/probe.txt
smbclient "//127.0.0.1/$U" -U "$U%$PASS" -c 'put /tmp/probe.txt probe.txt; ls' 2>&1 | head -20
echo "rc=$?"

echo "=== 7 smbstatus --json with a live tree connection"
# 用一条长连接保持 tcon，同时在另一端读 smbstatus
smbclient "//127.0.0.1/$U" -U "$U%$PASS" -c 'prompt; ls' >/dev/null 2>&1 &
sleep 2
smbclient "//127.0.0.1/$U" -U "$U%$PASS" -c 'ls' >/dev/null 2>&1 &
sleep 3
sudo -n smbstatus --json > /tmp/smbstatus.json 2>/dev/null
echo "--json--"; cat /tmp/smbstatus.json
echo
echo "--text -S--"; sudo -n smbstatus -S 2>/dev/null | head -20
wait

echo "=== 8 remove section from managed file, reload, check client rejected"
sudo -n tee /etc/samba/p08probe.conf >/dev/null <<'EOF'
[global]
   server min protocol = SMB2
   map to guest = never
EOF
sudo -n smbcontrol all reload-config
smbclient "//127.0.0.1/$U" -U "$U%$PASS" -c 'ls' 2>&1 | head -5
echo "rc=$?"
echo "--section gone?--"; sudo -n testparm -s --section-name="$U" --parameter-name="path" /etc/samba/smb.conf >/dev/null 2>&1; echo "rc=$?"

echo "=== 9 bad candidate exit codes"
printf '[global]\n  bogus_key = 1\n[x]\n path = /tmp\n' > /tmp/bad1.conf
sudo -n testparm -s /tmp/bad1.conf >/tmp/bad1.out 2>/tmp/bad1.err; echo "unknown-key rc=$?"
printf '# comment only\n[global\n' > /tmp/bad2.conf
sudo -n testparm -s /tmp/bad2.conf >/tmp/bad2.out 2>/tmp/bad2.err; echo "bad-section rc=$?"; echo "--err--"; cat /tmp/bad2.err
echo "=== 10 done"
