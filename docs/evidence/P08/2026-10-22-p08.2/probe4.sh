#!/bin/bash
# P08.2 设计探针 4：systemctl 在 syscmd 的固定环境下可用吗；section 名大小写；
# testparm 处理跨文件重复段名的行为。
set -u
sudo -n tee /etc/samba/p08case.conf >/dev/null <<'EOF'
[MixedCase]
   path = /tmp
   valid users = nobody
EOF
echo "=== env -i systemctl is-active"
env -i PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin LANG=C.UTF-8 LC_ALL=C.UTF-8 \
  /usr/bin/systemctl is-active smbd.service smbd.socket; echo "rc=$?"
echo "=== env -i systemctl 单 unit"
env -i PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin /usr/bin/systemctl is-active smbd.service; echo "rc=$?"
echo "=== section 名大小写（候选文件里是 [MixedCase]）"
sudo -n testparm -s /etc/samba/p08case.conf 2>/dev/null | grep -F '['
echo "--按原样查--"; sudo -n testparm -s --section-name=MixedCase --parameter-name=path /etc/samba/p08case.conf 2>&1 | tail -2; echo "rc=$?"
echo "--按小写查--"; sudo -n testparm -s --section-name=mixedcase --parameter-name=path /etc/samba/p08case.conf 2>&1 | tail -2; echo "rc=$?"
echo "=== 同一段名在 include 前后各出现一次时，谁生效"
sudo -n cp -a /root/smb.conf.p08.2-orig /tmp/probe4-base.conf
printf '\n[dup]\n   path = /tmp/first\n' | sudo -n tee -a /tmp/probe4-base.conf >/dev/null
sudo -n tee /etc/samba/p08dup.conf >/dev/null <<'EOF'
[dup]
   path = /tmp/second
EOF
printf '\ninclude = /etc/samba/p08dup.conf\n' | sudo -n tee -a /tmp/probe4-base.conf >/dev/null
sudo -n testparm -s --section-name=dup --parameter-name=path /tmp/probe4-base.conf 2>&1 | tail -2
echo "=== 合并后 map to guest（候选文件写 Never，主文件写 Bad User，候选在后面）"
sudo -n testparm -s --parameter-name="map to guest" /tmp/probe4-base.conf 2>/dev/null
echo "=== 主文件里 include 一行写成注释时会被当成已接入吗"
printf '#include = /etc/samba/p08dup.conf\n' | sudo -n tee -a /tmp/probe4-base.conf >/dev/null
sudo -n testparm -s --parameter-name="map to guest" /tmp/probe4-base.conf 2>/dev/null
sudo -n rm -f /etc/samba/p08case.conf /etc/samba/p08dup.conf /tmp/probe4-base.conf
