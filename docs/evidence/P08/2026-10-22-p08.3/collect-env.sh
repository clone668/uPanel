#!/bin/sh
# 采集 P08.3 验收环境（在一次性 Ubuntu 测试虚拟机里、验收跑完之后执行）。
set -u
echo "P08.3 验收环境（一次性 Ubuntu 测试虚拟机）"
echo "证据类型：Linux VM integration"
echo "采集时间：$(date -u +%Y-%m-%dT%H:%M:%SZ)（UTC，虚拟机时钟）"
echo
echo "== 虚拟机"
echo "os=$(. /etc/os-release && echo "$PRETTY_NAME")"
echo "kernel=$(uname -sr) $(uname -m)"
echo "nproc=$(nproc)"
echo "vcpu_model=$(awk -F': ' '/^model name/{print $2; exit}' /proc/cpuinfo)"
echo "mem=$(awk '/^MemTotal/{print $2" kB"}' /proc/meminfo)"
echo "ssh -i H:/Code/vms/upanel-2404/id_ed25519 -p 2222 upanel@127.0.0.1"
echo
echo "== 磁盘"
lsblk -o NAME,SIZE,TYPE,FSTYPE,MOUNTPOINTS
echo
echo "== Samba 与 ACL（夹具，不是被测物）"
dpkg-query -W -f='${Package} ${Version}\n' samba samba-common-bin acl 2>/dev/null
echo "smbd --version：$(smbd --version 2>&1 | head -1)"
for t in /usr/sbin/smbd /usr/bin/smbpasswd /usr/bin/pdbedit /usr/bin/testparm \
         /usr/bin/smbcontrol /usr/bin/smbstatus /usr/bin/smbclient \
         /usr/bin/setfacl /usr/bin/getfacl; do
  if [ -x "$t" ]; then echo "$t 存在"; else echo "$t 缺失"; fi
done
echo "smbd 状态：$(systemctl is-active smbd.service smbd.socket 2>/dev/null | tr '\n' ' ')"
echo
echo "== systemd（P08.3 的被测面之一）"
echo "systemd=$(systemctl --version | head -1)"
echo "systemd-escape 存在：$([ -x /usr/bin/systemd-escape ] && echo 是 || echo 否)"
echo "smbd.service 的 drop-in 目录："
systemctl show smbd.service -p DropInPaths
echo
echo "== 被测二进制（sha256，与宿主构建产物比对）"
sha256sum /var/tmp/upanel-bin/upanel-agent /var/tmp/upanel-bin/upanel-web /var/tmp/upanel-bin/upanelctl
echo
echo "== 验收脚本"
sha256sum /var/tmp/p08-3.sh /var/tmp/p08-3-reboot.sh 2>/dev/null
wc -l /var/tmp/p08-3.sh /var/tmp/p08-3-reboot.sh 2>/dev/null
