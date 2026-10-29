# P14.1 验收结果：网络现状的四条只读端点与“能不能改”的判定

- 日期：2026-10-26（UTC）
- 分支 `main`，commit `8e52b36`（工作树含本轮改动，按用户要求不提交不推送）
- 验收对象：**“这台机器现在是什么样”与“这份现状能不能被本面板安全地改”**——网卡与地址、
  路由与默认网关、监听端口与持有者、防火墙管理者与规则的四条只读端点，以及
  `capability`（netplan + networkd + 单物理网卡 + 无 NetworkManager）与 `editable`
  （只有 ufw 独占时才开）两份判定。
- 验收方式：真实 Web（非 root，`upanel-web`）→ `/run/upanel/agent.sock` → root 代理 →
  真实 Ubuntu 主机。**每条关键断言都配一条现场独立读数**：网卡类型与物理性对照 sysfs、
  配置归属对照 `/run` 下的渲染产物、监听集合对照 `/proc/net/{tcp,tcp6}`、防火墙对照
  `ufw status verbose/numbered`，并对判定本身在脚本里用现场事实**独立重算一遍**再比对。
  两台专用 Ubuntu VM，**同一份脚本、同一组二进制、一台一跑不并发**。
- 为什么不靠 mock：mock 只能证明 handler 把代理给的结构原样吐出去（那部分由
  `internal/api/network_test.go` 覆盖）。这一层要证明的是代理从内核与文件系统读出来的
  东西是对的，所以两边对不上就判红——不管哪边看起来更合理。
- 浏览器层为什么不在 VM 上验：VM 里没有 Playwright，页面（四张卡、能不能改的横幅、
  issues 提示）由宿主侧 `vue/server-renderer` 渲染测试与 `web-smoke` 覆盖（DECISIONS Q21）。
  VM 上钉的是**页面拿到的事实**，不钉像素。

## 1. 结论

| VM | 发行版 | 结果 | 退出码 | 断言 |
|---|---|---|---|---|
| 2204 | Ubuntu 22.04.5（内核 5.15.0-198，Python 3.10.12，systemd 249，whpx） | **PASS** | 0 | 112 通过 / 0 失败 / 0 未执行 |
| 2404 | Ubuntu 24.04.5（内核 6.8.0-147，Python 3.12.3，systemd 255，`-Accel tcg,thread=single`） | **PASS** | 0 | 116 通过 / 0 失败 / 0 未执行 |

逐节断言数：

| 节 | 内容 | 2204 | 2404 |
|---|---|---|---|
| §0 | 环境（root、二进制、python3、runuser、服务账号） | 7 | 7 |
| §1 | 装配（代理、管理员、viewer、Web） | 7 | 7 |
| §2 | 权限与“没有写入口”（401/403/200，POST 同路径 404/405） | 14 | 14 |
| §3 | 网卡形状与逐张 sysfs 对照 | 20 | 25 |
| §4 | 能不能改：现场独立重算后比对 | 7 | 6 |
| §5 | 路由与默认网关 | 10 | 10 |
| §6 | 监听端口与 `/proc/net` 双向比对 | 13 | 13 |
| §7 | 防火墙与 `ufw status` | 9 | 9 |
| §8 | `issues` 语义（四条都查） | 16 | 16 |
| §9 | 只读：四条请求不碰 `/etc/netplan`、`/etc/ufw`、`/etc/systemd/network` | 4 | 4 |
| §10 | 卫生（无 panic、两进程仍在、审计只记被拒的读） | 5 | 5 |
| 合计 | | **112** | **116** |

两台差 4 条的**原因不是**某台少验了一项，而是 24.04 上多一张网卡：P09 的容器验收留下
`docker0` 网桥，§3 对每张网卡有 5 条 sysfs 对照（存在 / physical / kind / mtu / carrier），
多一张就多 5 条；§4 走的是 `editable=false` 分支（原因见 §4.2），比 `editable=true` 分支少
一条 target 对照，净差 +4。

宿主侧（最终工作树、同一组源码）：`scripts/check.sh` **PASS 14 / FAIL 0 / NOT RUN 0**，
`go vet ./...` 干净，go 47 包 ok + 3 个 no test files，`gofmt` 干净，Linux 交叉构建过，
web 53 文件 **999** 用例、`web-smoke` 10/0/0、`openapi-lint` **valid + 6 warnings**（6 条
warning 是本轮之前就有的）。重跑 `check.sh` 交叉编译出的三个二进制 sha256 与两台 VM 上跑的
**逐字节一致**（`92795acb…`/`252a7ed6…`/`6841cb3c…`）——这一条比脚本里的 `sha256sum` 复核更强：
它证明 VM 跑的就是当前这棵树。

## 2. 两台机器落在**不同的判定分支**上（这一轮最有价值的一点）

`capability` 的两条分支都在真机上走到了，不是靠单测构造出来的：

| VM | backend | editable | target | 原因 |
|---|---|---|---|---|
| 2204 | `netplan-networkd` | **true** | `ens4` | 一份 `50-cloud-init.yaml`，渲染出 `10-netplan-ens4.network`；只有一张物理网卡 `ens4`；没有网桥/bond/VLAN/Wi-Fi；`/run/NetworkManager` 不存在 |
| 2404 | `netplan-networkd` | **false** | — | 同上，但存在网桥 `docker0`（P09 容器验收的残留）：`本机存在网桥接口（docker0）：PLAN §19 规定 1.0 只改单个物理网卡的 IPv4，其余先只读` |

也就是说“能改”的那个绿色按钮与“灰掉并给出原因”的那条路径都被真机验过。判定在脚本里
是用现场事实**独立重算**的（`/etc/netplan` 有配置 + `/run/systemd/network/10-netplan-*.network`
存在 → `netplan-networkd`；`physical_eths` 数出物理以太网卡数；`complex_kinds` 数出
网桥/bond/Wi-Fi/VLAN；`/run/NetworkManager` 是否在），再与响应逐项比对——不是拿产品的答案
去验产品。

## 3. 现场事实（响应原文摘要，完整见 `logs/*/facts.txt`）

两个 VM 的 `collected_at`、网卡、路由、防火墙：

```
2204  collected_at = 2026-10-26T12:51:58Z
iface ens4  kind=ethernet phys=True  state=up      mtu=1500  carrier=True  addrs=10.0.2.15/24(ipv4), fec0::5054:ff:fe12:3456/64(ipv6), fe80::5054:ff:fe12:3456/64(ipv6)
iface lo    kind=loopback phys=False state=unknown mtu=65536 carrier=True  addrs=127.0.0.1/8(ipv4), ::1/128(ipv6)

2404  collected_at = 2026-10-26T12:50:38Z
iface docker0 kind=bridge   phys=False state=down    mtu=1500  carrier=False addrs=172.17.0.1/16(ipv4)
iface ens4    kind=ethernet phys=True  state=up      mtu=1500  carrier=True  addrs=10.0.2.15/24(ipv4), …
iface lo      kind=loopback phys=False state=unknown mtu=65536 carrier=True  addrs=127.0.0.1/8(ipv4), ::1/128(ipv6)

两台相同：
routes     8 条，默认路由 v4 0.0.0.0/0 via 10.0.2.2 dev ens4 与 v6 ::/0 via fe80::2 dev ens4
gateways   ipv4 10.0.2.2(ens4), ipv6 fe80::2(ens4)
firewall   manager=ufw installed=true enabled=false editable=true rules=0
           manager_reasons=[发现 ufw，且没有别的防火墙管理器在] reasons=[] issues=[]
```

监听端口（2204 33 条 / 2404 19 条，均 `truncated=false`），其中几条值得留档的：

| 条目 | 现场含义 |
|---|---|
| `tcp 0.0.0.0:22 pid=737 proc=sshd unit=ssh.service` | 22.04 由 sshd 自己听 |
| `tcp 0.0.0.0:111 pid=1 proc=systemd unit=init.scope` | 24.04 的 rpcbind 走 socket activation，持有者是 PID 1 |
| `tcp 0.0.0.0:2049 pid=0 proc= unit=` | NFS 内核线程持有，`/proc/<pid>/fd` 里没有它——`pid=0` 的语义是**读不到**，不是“没有进程” |
| `tcp 127.0.0.1:19471 pid=<web> proc=upanel-web` | 面板自己的端口出现在自己报的表里，pid 与 runuser 的子进程号一致，`/proc/<pid>/comm` 也是 `upanel-web` |

## 4. 这一轮撞到的问题

### 4.1 F-Q438（产品缺陷）：主机路由的 `destination` 不带前缀，同一个字段两种写法

22.04 首轮红在 `目标一律写成网段（含前缀长度）: 期望 '[]'，实际 '["10.0.2.2", "10.0.2.3", "::1"]'`。

`ip -j route` 对主机路由只给地址（`{"dst":"10.0.2.2"}`），对网段给 CIDR（`{"dst":"10.0.2.0/24"}`）。
产品原来把 `dst` 原样搬进 `Route.Destination`，于是同一个字段一会儿是 `10.0.2.2`、一会儿是
`10.0.2.0/24`——读的人得先记住“没有斜杠时按单地址解释”，而那正是 CONTRACTS §13.5 写着
“default 也写成网段”想避免的事。修法是解析时就补齐：无斜杠的 v4 加 `/32`、v6 加 `/128`。
单测 `TestRoutesNormalizeHostRoutesToPrefix` 用现场这三条形状钉住（`0.0.0.0/0`、`10.0.2.0/24`、
`10.0.2.2/32`、`::1/128` 的排序与取值）。

### 4.2 F-Q439（产品缺陷）：`/etc/nftables.conf` 每台 Ubuntu 都有，被判成“ufw 与 nftables 同存”

22.04 **第二轮**红在 `ufw 与别的管理器同存 → manager=unknown: 期望 'unknown'，实际 'ufw'`——
这一条红得很有意思：产品报 `ufw`，脚本（按产品第一版的口径写的）期望 `unknown`。查现场：

```
/etc/nftables.conf 存在（228 字节，nftables 包自带的默认规则集）
systemctl is-enabled nftables → disabled
/etc/systemd/system/*.wants/nftables.service → 不存在
```

也就是说第一版把“有那个文件”当成“nftables 在管”，而**每一台 Ubuntu 都装 nftables 包、
都带这个文件**。后果不是显示不好看：`manager` 变成 `unknown` → `editable=false` →
P14.3 的写路径在一台最标准的 Ubuntu 上**永远打不开**。判据改成“它会不会在开机时把这份
规则装进内核”：`/etc/nftables.conf` 存在**并且** `/etc/systemd/system/*.wants/nftables.service`
存在。单测两条（`TestFirewallUbuntuDefaultNftablesConfIsNotAManager`、
`TestFirewallNftablesEnabledCountsAsForeignManager`）分别钉住两侧；已知局限写进
CONTRACTS §13.5（手工 `nft -f` 载入但没 enable 服务的机器看不到证据）。

### 4.3 F-Q440～F-Q445（验收方法，产品零改动）

| 编号 | 现象 | 真因与改法 |
|---|---|---|
| F-Q440 | `占用 19471 的 pid 就是 Web 进程: 期望 '127119'，实际 '127157'` | `$WEB_PID` 是 `runuser` 的壳，真正监听的是它 fork 的子进程。改成读 `/proc/$WEB_PID/task/$WEB_PID/children` 取子进程号，并**加一条不依赖记账的断言**：报出来的 pid 的 `/proc/<pid>/comm` 必须是 `upanel-web` |
| F-Q441 | `loopback 标记只出现在回环地址上: 期望 '[]'`，实际是 `127.0.0.53:53`（systemd-resolved） | 断言只允许 `127.0.0.1`/`::1`，而 127.0.0.0/8 整段都是回环。改成 `startswith('127.')` 或 `== '::1'` |
| F-Q442 | `期望 '['127.0.0.1']'，实际 '["127.0.0.1"]'` | 期望值写成了 Python `str(list)` 的形状，响应是 `json.dumps` 的形状。凡比结构的断言一律走 `jbd`（json.dumps 出口），不吃 `jget` 的 `str()` |
| F-Q443 | `响应里有 capability: 缺少 'capability'` | 用 `head -c 400` 找键，而长响应前 400 字节只有网卡数组。改成 `jbd "…'capability' in d['data']…"`，找键不做字符串截断 |
| F-Q444 | 24.04 **首轮**：`firewall` 的 issues 里有一条 `ufw/timeout`，而脚本却“验过”了规则条数 `0 == 0` | 产品是对的（如实记了超时），假绿出在脚本：读不到时 0 条期望与 0 条实际被算成一致。改成**重读一次**（读到才对照，并把“第一次超时”留在日志里），仍读不到就记 `NOT RUN` 而不是 PASS；规则 action/raw 这两条只依赖响应形状，留在外面照查 |
| F-Q445 | 24.04：`22 端口反查到了持有者进程名: 期望 'sshd'，实际 'systemd'` | 24.04 用 `ssh.socket` 由 PID 1 持有监听、收到连接再交给 sshd，进程名写死 `sshd` 是把 22.04 的形状当成了全部。改成“报出来的进程名与内核里那个 pid 的 comm 一致”——判据不再依赖我事先知道该是哪个进程 |

另有一条只影响本轮取证、不进脚本的教训：远端 `pkill -f "upanel-agent --config"` 会连
**执行它的那层 shell** 一起杀掉（那层 shell 的 argv 里就有这个字符串），表现是整条 ssh
命令“零输出、零退出码”。用 `[u]panel-agent` 这种自避让写法。

## 5. 只读的证明方式

`/etc/netplan`、`/etc/ufw`、`/etc/systemd/network` 三个目录的**文件清单 + 每个文件的
sha256** 在四条请求前后逐字节比对（`hash_tree`），三处都无变化。刻意**不**去比 `ip addr`
的输出：DHCP 租约的 `valid_lft` 每秒都在变，那种红是尺子在动，不是产品在写。

另附一条“不是缓存”的旁证：两次独立采集的 `collected_at` 不同（同一秒内完成则记 NOTE，
不判红）。

## 6. 未验证 / NOT RUN（明确区分）

| 项 | 状态 | 说明与补做方法 |
|---|---|---|
| Wi-Fi / bond / VLAN / 多物理网卡 / NetworkManager 在跑 这几条 `editable=false` 的**具体**原因分支 | **仅有单测** | 真机上造这些要么要写系统配置（P14.1 只读，不允许），要么要一张无线卡。单测 `TestCapability*` 覆盖五条原因；真机上只走到“网桥 `docker0`”这一条（24.04） |
| `netplan-networkmanager`、`networkmanager`、`unknown` 三种 backend | **仅有单测** | 两台 VM 都是 `netplan-networkd`。改渲染器要写 `/etc/netplan`，属于 P14.2 的预检场景 |
| `manager=firewalld` / `nftables(enable)` / `iptables-persistent` / `none` | **仅有单测** | 真机上装第二套防火墙会改变这台机器的规则集，且会影响后续阶段；`none` 需要卸掉 ufw |
| `ip` 不在、`/proc/net` 读不到、`ufw` 未安装时的 `issues` | **仅有单测** | 真机上是标准安装；`ip` 缺失要靠改 PATH/删程序，属于破坏现场 |
| `issues.code` 里的 `permission_denied` / `unparsable` / `failed` | **未在真机触发** | 只有 `timeout` 在 24.04 首轮自然出现过一次（见 F-Q444）。伪造这些要在 sysfs 里放畸形内容或改文件权限，属于造现场 |
| 页面（`NetworkView.vue`）在浏览器里的渲染 | **NOT RUN（VM 内不可执行）** | 由宿主侧渲染测试与 `web-smoke` 覆盖，见 DECISIONS Q21 |

## 7. 复现

```sh
# 宿主：交叉编译 + 推文件（换端口与密钥即为另一台 VM）
sh scripts/build-linux.sh
scp -i H:/Code/vms/upanel-2404/id_ed25519 -P 2222 bin/linux-amd64/upanel-{agent,web,ctl} \
    upanel@127.0.0.1:/tmp/upanel-bin/
scp -i H:/Code/vms/upanel-2404/id_ed25519 -P 2222 \
    tests/integration/p14-1-acceptance.sh tests/integration/json_helper.py \
    upanel@127.0.0.1:/tmp/upanel-src/tests/integration/

# VM 内（必须 root）
sudo pkill -f "[u]panel-agent --config"; sudo pkill -f "[u]panel-web --config"
sudo rm -rf /run/upanel
sudo BIN_DIR=/tmp/upanel-bin sh /tmp/upanel-src/tests/integration/p14-1-acceptance.sh
```

## 8. 交付指纹

| 项 | 值 |
|---|---|
| 代码 commit | `8e52b36`（工作树含本轮改动，未提交） |
| `upanel-agent` | `92795acbffbba7881cc5ea364d0b1fec7d97d3628485c0bec5c3a708c7cd1824` |
| `upanel-web` | `252a7ed6ba974cc10266b0424ad4af79d72adfa824d8e99a2bf0a921f709c2a6` |
| `upanelctl` | `6841cb3c070a29fbb6570cf9f03a2821b52093a4c567ee2f0c815ee427416649` |
| 验收脚本 | `tests/integration/p14-1-acceptance.sh` sha256 `147b7a93d248bd1d622b6c91f2717daf5246c9cd4d580945baeac1fe30f2c16a` |
| JSON 助手 | `tests/integration/json_helper.py` sha256 `767b9844676a7f570c195b4de1a2e1768bb1f50b5aa7219963c016c1c9d6a837` |

两台 VM 各自 `sha256sum` 复核，与宿主 `bin/linux-amd64/` 逐字节一致（见 `logs/*/env.txt`）。
两个 VM 的发行版、内核、Python、systemd、netplan 渲染产物、物理网卡、nftables/ufw 现场，
同样在 `logs/*/env.txt`。
