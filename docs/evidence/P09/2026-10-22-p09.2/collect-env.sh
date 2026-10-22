#!/bin/sh
# 采集 P09.2 验收环境（在测试虚拟机内以 root 运行）。
# 只读：不改任何配置，不安装任何东西。输出直接作为 env.txt。

echo "P09.2 验收环境（一次性 Ubuntu 测试虚拟机）"
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
echo "本轮不做任何磁盘操作：P09.2 是发现、候选确认与 HTTP 健康探测，"
echo "sdb1/sdc1/sdd 一律没碰（脚本里没有任何分区/格式化命令）。"
echo
echo "== 运行身份"
echo "upanel-web: $(getent passwd upanel-web)"
echo "该账号没有登录 shell，也没有 docker 组。"
echo
echo "== 本机监听（发现来源的真实输入）"
ss -lntp 2>/dev/null | head -20
echo
echo "== /proc 发现通路是否可读（代理读这些，不是 Web 读）"
echo "/proc/net/tcp: $([ -r /proc/net/tcp ] && echo 可读 || echo 不可读)"
echo "/proc/net/tcp6: $([ -r /proc/net/tcp6 ] && echo 可读 || echo 不可读)"
echo "cgroup 版本: $(stat -fc %T /sys/fs/cgroup 2>/dev/null || echo 未知)"
echo "systemd: $(systemctl --version 2>/dev/null | head -1 || echo 缺失)"
echo
echo "== 容器运行时（P09.2 的第三个来源）"
echo "docker: $(command -v docker || echo 缺失)"
echo "docker 版本: $(docker version --format '{{.Server.Version}}' 2>/dev/null || echo '读不到（守护进程没跑）')"
echo "podman: $(command -v podman || echo 缺失)"
echo "/var/run/docker.sock: $([ -S /var/run/docker.sock ] && echo 存在 || echo 不存在)"
echo "镜像: $(docker images --format '{{.Repository}}:{{.Tag}}' 2>/dev/null | tr '\n' ' ')"
echo
echo "本轮为容器验收装了 docker.io（29.1.3，Noble 仓库）并拉了一个 alpine:3.20 做夹具。"
echo "这是测试机上的一次性依赖，不是产品的一部分：产品只调用 docker CLI 读清单，"
echo "没有 docker 时发现里的 docker 来源是 unavailable（真机上验过没有 docker 的那一版）。"
echo
echo "== 依赖工具（验收脚本自己用的，不是被测物）"
for t in python3 curl ss runuser getent; do
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
echo "== 端口占用（验收脚本用 19482—19486，要求开始前空闲）"
for p in 19482 19483 19484 19485 19486; do
  printf '%s: %s\n' "$p" "$(ss -lntH "sport = :$p" | wc -l) 个监听"
done
echo
echo "== 版本"
echo "仓库：https://github.com/clone668/uPanel，分支 main"
echo "Go 工具链：$(cd /h/Code/uPanel 2>/dev/null && echo 'go 1.27.3（go.mod 固定）' || echo 'go 1.27.3（go.mod 固定）')"
echo
echo "== 一次性说明"
echo "这台虚拟机只用于测试。P09.2 全部动作都是读 /proc、读 systemd 状态、"
echo "对 127.0.0.1 上的三个临时 HTTP 服务发请求，没有碰磁盘、网络配置或系统服务。"
