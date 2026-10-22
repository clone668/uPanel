#!/bin/sh
# 采集 P09.3 验收环境（在测试虚拟机内以 root 运行）。
# 只读：不改任何配置，不安装任何东西（docker 与夹具镜像在本轮之前就装好了）。
# 输出直接作为 env.txt。

echo "P09.3 验收环境（一次性 Ubuntu 测试虚拟机）"
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
echo "本轮不做任何磁盘操作：P09.3 只读容器/镜像/网络/卷，并对容器本身做 start/stop/restart。"
echo "卷是 docker 的命名卷（/var/lib/docker/volumes 下），不是 sdb1/sdc1/sdd。"
echo
echo "== 运行身份"
echo "upanel-web: $(getent passwd upanel-web)"
echo "upanel-web 的组: $(id -nG upanel-web 2>/dev/null)"
echo "该账号没有登录 shell，也不在 docker 组；验收脚本直接用 docker 试过（会被拒）。"
echo
echo "== 容器运行时（被测对象）"
echo "docker: $(command -v docker || echo 缺失)"
echo "docker 版本: $(docker version --format '{{.Client.Version}} / server {{.Server.Version}}' 2>/dev/null || echo '读不到（守护进程没跑）')"
echo "/var/run/docker.sock: $(ls -l /var/run/docker.sock 2>/dev/null || echo 不存在)"
echo "compose 插件: $(docker compose version 2>/dev/null || echo '未安装（本项目也不用它）')"
echo "镜像: $(docker images --format '{{.Repository}}:{{.Tag}}' 2>/dev/null | tr '\n' ' ')"
echo "容器: $(docker ps -a --format '{{.Names}}:{{.State}}' 2>/dev/null | tr '\n' ' ')"
echo "日志驱动（默认）: $(docker info --format '{{.LoggingDriver}}' 2>/dev/null || echo 读不到)"
echo
echo "docker.io 是测试机上的一次性依赖（Noble 仓库，29.1.3），不是产品的一部分："
echo "产品只通过固定 argv 调用 docker CLI，没有 docker 时相关接口报的是 unavailable/503。"
echo
echo "== 依赖工具（验收脚本自己用的，不是被测物）"
for t in python3 curl ss runuser getent stat sha256sum; do
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
echo "== 端口占用（验收脚本用 19493，要求开始前空闲）"
for p in 19493; do
  printf '%s: %s\n' "$p" "$(ss -lntH "sport = :$p" | wc -l) 个监听"
done
echo
echo "== 版本"
echo "仓库：https://github.com/clone668/uPanel，分支 main"
echo "Go 工具链：go 1.27.3（go.mod 固定）"
echo
echo "== 一次性说明"
echo "这台虚拟机只用于测试。P09.3 的写操作只有三类：docker start/stop/restart（作用于脚本"
echo "自己创建的 7 个夹具容器）、脚本收尾时删除夹具、以及验收“两种不可用”时对 docker.service"
echo "与 docker.socket 做的一次 stop/start。没有碰系统盘、网络配置或其它服务。"
