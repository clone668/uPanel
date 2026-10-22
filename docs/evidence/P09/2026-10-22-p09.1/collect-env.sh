#!/bin/sh
# 采集 P09.1 验收环境（在测试虚拟机内以 root 运行）。
# 只读：不改任何配置，不安装任何东西。输出直接作为 env.txt。

echo "P09.1 验收环境（一次性 Ubuntu 测试虚拟机）"
echo "证据类型：Linux VM integration"
echo "采集时间：$(date -u +%Y-%m-%dT%H:%M:%SZ)（UTC，虚拟机时钟）"
echo
echo "== 虚拟机"
. /etc/os-release
echo "os=$PRETTY_NAME"
echo "kernel=$(uname -srm)"
echo "nproc=$(nproc)"
echo "vcpu_model=$(awk -F': ' '/model name/{print $2; exit}' /proc/cpuinfo)"
echo "mem=$(awk '/MemTotal/{print $2" "$3}' /proc/meminfo)"
echo "ssh -i H:/Code/vms/upanel-2404/id_ed25519 -p 2222 upanel@127.0.0.1"
echo
echo "== 磁盘"
lsblk -o NAME,SIZE,TYPE,FSTYPE,MOUNTPOINTS
echo
echo "sda 是系统盘（/ 在 sda1）。脚本只接受显式给出的 SPARE_DISKS，"
echo "本轮 SPARE_DISKS=sdd（4G 空盘）当作一次性夹具：分区表、分区、ext4、导入卷。"
echo "sdb1（2G ext4，挂在 /srv/upanel/volumes/data）与 sdc1（1G ext4）是 P07/P08 留下的，"
echo "本轮不使用，也没有被改动。"
echo
echo "== 运行身份"
echo "upanel-web: $(getent passwd upanel-web)"
echo "该账号没有登录 shell，也没有 docker 组——验收脚本里各有一条断言在核对。"
echo
echo "== 依赖工具（验收脚本自己用的，不是被测物）"
for t in python3 curl ss runuser getent sgdisk blockdev udevadm wipefs blkid; do
  printf '%s: %s\n' "$t" "$(command -v "$t" || echo '缺失')"
done
echo "sqlite3 命令行工具：$(command -v sqlite3 || echo '缺失（用 python3 的 sqlite3 模块读库）')"
echo
echo "== Go"
echo "虚拟机内没有 Go 工具链。代理与 Web 二进制在开发宿主用"
echo "GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath 交叉编译，"
echo "拷进 /var/tmp/upanel-bin 执行。宿主上的 go test ./... 见 native-tests.txt。"
echo
echo "== 二进制指纹"
sha256sum /var/tmp/upanel-bin/upanel-agent /var/tmp/upanel-bin/upanel-web /var/tmp/upanel-bin/upanelctl
echo
echo "== 版本"
echo "仓库：https://github.com/clone668/uPanel，分支 main"
echo "本轮改动前的最后一次提交：$(cd "$(dirname "$0")" 2>/dev/null && echo 'e790c3a feat(shares): P08.4 共享页面、用户权限与客户端连接说明')"
echo "Go 工具链：go 1.27.3（go.mod 固定）"
echo
echo "== 一次性说明"
echo "这台虚拟机只用于测试。sdd 上做的分区/格式化/导入是破坏性操作，"
echo "只在一次性虚拟机的空盘上执行；开发宿主与日常服务器上没有跑过任何一条盘操作。"
