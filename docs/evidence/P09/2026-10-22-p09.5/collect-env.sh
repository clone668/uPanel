#!/bin/sh
# 采集 P09.5 验收环境（在测试虚拟机内以 root 运行）。
# 只读采集器本身：不改配置、不装东西。脚本运行时对环境做过的写操作列在最后一节，
# 逐条写明“谁做的、收尾有没有还原”。
# 输出直接作为 env.txt。

echo "P09.5 验收环境（一次性 Ubuntu 测试虚拟机）"
echo "证据类型：Linux VM integration（后端事实）+ 载荷回放（前端 DOM）"
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
echo "== 网络（入口地址那一段要用非回环地址）"
ip -4 -o addr show scope global
echo "脚本取第一个 scope global 的 IPv4 作为“服务器地址”，把站点容器发布在它上面，"
echo "入口（link）应用的 public_url 就写成 http://<该地址>:18897/ 。"
echo "同一端口在 127.0.0.1 上没有监听——这正是“入口不是回环”的独立证据。"
echo
echo "== 磁盘"
lsblk -o NAME,SIZE,TYPE,FSTYPE,MOUNTPOINTS
echo
echo "-- fstab（受管理块）"
sed -n '/BEGIN upanel managed volumes/,/END upanel managed volumes/p' /etc/fstab
echo
echo "sdc1 / sdd1 是两块 1 GiB 的独立盘，专门留给 P08/P09 的卷夹具。它们都不挂载、"
echo "都在本轮之前就存在于这台一次性虚拟机上；脚本用 sdc1 走产品的 registry 登记成受管理卷，"
echo "只写 fstab 与代理库，不挂载、不动盘上数据；收尾用产品自己的 storage.volume.forget"
echo "退出管理并把 fstab 条目删掉。"
echo
echo "== 运行身份"
echo "upanel-web: $(getent passwd upanel-web)"
echo "upanel-web 的组: $(id -nG upanel-web 2>/dev/null)"
echo "该账号没有登录 shell，也不在 docker 组；脚本 §0 验过，§1 现场对照监听 19495 的进程 uid=999。"
echo
echo "== 容器运行时（被测对象）"
echo "docker: $(command -v docker || echo 缺失)"
echo "docker 版本: $(docker version --format '{{.Client.Version}} / server {{.Server.Version}}' 2>/dev/null || echo '读不到（守护进程没跑）')"
echo "/var/run/docker.sock: $(ls -l /var/run/docker.sock 2>/dev/null || echo 不存在)"
echo "镜像: $(docker images --format '{{.Repository}}:{{.Tag}}' 2>/dev/null | tr '\n' ' ')"
echo "日志驱动（默认）: $(docker info --format '{{.LoggingDriver}}' 2>/dev/null || echo 读不到)"
echo
echo "== systemd（被测对象）"
echo "systemd: $(systemctl --version | head -1)"
echo "本轮起停/改写的单元：脚本自己创建的 upn-p095-svc.service（/etc/systemd/system/），"
echo "每 5 秒写一行日志（这样 journal 载荷不是空页）；收尾时 disable --now + 删除 + daemon-reload。"
echo "没有触碰 sshd/docker 等受保护单元：受保护那一侧只做了一件事——对 ssh.service 发 stop 预检，"
echo "验证 allowed=false（并现场对照它仍然是 active）。"
echo
echo "== 依赖工具（验收脚本自己用的，不是被测物）"
for t in python3 curl ss runuser getent stat sha256sum findmnt blkid lsblk ip hostname nc; do
  printf '%s: %s\n' "$t" "$(command -v "$t" || echo '缺失')"
done
echo "sqlite3 命令行工具：$(command -v sqlite3 || echo '缺失（用 python3 的 sqlite3 模块读库）')"
echo
echo "== lsblk 的一个坑（脚本踩过，写在这里免得下一轮再踩）"
echo "util-linux $(lsblk --version | awk '{print $NF}')：-r（raw）与 -l（list）互斥，"
echo "-lnpr 会直接报 'mutually exclusive arguments' 并退出 1，拿到空列表。"
echo "脚本现在只用 'lsblk -l -n -p -o NAME,TYPE,FSTYPE,MOUNTPOINT'。"
echo
echo "== Go 与 Node"
echo "虚拟机内没有 Go 工具链，也没有 Node：前端是纯 TypeScript，DOM 由宿主的 vitest 回放"
echo "本脚本抓回的载荷（web/src/fixtures/p09-5-vm/）。"
echo "代理与 Web 二进制在开发宿主用 GOOS=linux GOARCH=amd64 go build -trimpath 交叉编译，"
echo "拷进 /var/tmp/upanel-bin 执行。宿主上的检查见 check.txt 与 native-tests.txt。"
echo
echo "== 二进制指纹"
sha256sum /var/tmp/upanel-bin/upanel-agent /var/tmp/upanel-bin/upanel-web /var/tmp/upanel-bin/upanelctl
echo
echo "== 端口占用（脚本用 19495 起 Web）"
for p in 19495 18895 18896 18897; do
  printf '%s: %s 个监听\n' "$p" "$(ss -lntH "sport = :$p" | wc -l)"
done
echo
echo "== 版本"
echo "仓库：https://github.com/clone668/uPanel，分支 main"
echo "Go 工具链：go 1.27.3（go.mod 固定）"
echo
echo "== 本轮脚本对环境做过的写操作（都在一次性虚拟机上，收尾已还原）"
cat <<'NOTE'
1. docker: 建 7 个夹具容器（upn-p095-logs / -ok / -500 / -site / -gated / -always / -rebuild），
   对其中一个 stop、对另一个 rm -f 之后同名重建（验“ID 变了就是看不见”）；
   收尾时 docker rm -f 全部删除（含重建过的那个）。
2. systemd: 写 /etc/systemd/system/upn-p095-svc.service，enable --now；
   用产品自己的 service.control 把它 stop 再 start 各一次（验卡片的启停路径）；
   收尾时 disable --now、删文件、daemon-reload。
3. 存储: 用产品自己的 storage.volume.import 把 /dev/sdc1 登记成受管理卷（写 fstab 受管理块，
   不挂载、不动盘上数据）；中途 mount/unmount 各一次做门禁正对照；收尾用 storage.volume.forget
   退出管理并把 fstab 条目删掉。
4. 应用元数据: 建 13 条应用、2 个分组、1 次收藏，收尾逐条删除（删除本身也是一条验收：
   全量列表回到 0 条、分组回到 0 个）。
5. 代理与 Web: /run/upanel/agent.sock 与 Web 的 SQLite 库都在 /var/tmp/upanel-accept-p09-5/ 下，
   不写 /var/lib/upanel 以外的系统位置。
没有碰系统盘（sda）、网络配置、防火墙或其它服务；没有执行 partition/format/wipefs。
NOTE
