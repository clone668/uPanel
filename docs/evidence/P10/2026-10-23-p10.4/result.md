# P10.4 验收结果（2026-10-23）

STAGES §12 第 4 项：**0.1 安装包、校验和、systemd 单元、初始用户、HTTPS/SSH 隧道使用说明、
基本恢复说明；执行完整核心场景**。另有 0.1 放行条件「22.04 和 24.04 都有安装/重启/核心运维证据，
README 明确尚未实现的 1.0 功能」。

本文件只写**实际跑出来的结果**。没跑的检查在最后一节点名，不写成通过。

## 一、怎么复现

```
# 0) 先提交再打包再验收（DECISIONS Q317）：本目录的一切数字都来自 ac9e4a0 打出的这颗包
git commit …                                   # ac9e4a0

# 1) 宿主机：打 0.1 包（amd64 + arm64），自带 SHA256SUMS
VERSION=0.1.0 sh scripts/release.sh             # → d00bada6… / dfc9cfb5…

# 2) 两台专用测试虚机（22.04 / 24.04）上，按顺序跑
sudo PACKAGE=<包根目录> sh p10-4-install-acceptance.sh    # 第一部分：安装/校验和/单元/HTTPS/恢复
sudo sh p10-4-core-scenario.sh                            # 第二部分：完整核心场景
sudo sh p10-4-installer-group-branches.sh                 # 安装器「组在、账号不在」两支（22.04 上跑）

# 3) 回到宿主机（隧道必须从外面进，虚机内部自己连自己证明不了转发）
sh p10-4-ssh-tunnel.sh 22.04 2223 <22.04 的 ssh 私钥>
sh p10-4-ssh-tunnel.sh 24.04 2222 <24.04 的 ssh 私钥>
```

脚本会改动被测机器（装/卸、systemd 单元、证书、/etc/hosts、iptables、信任库），收尾逐个还原；
`p10-4-install-acceptance.sh` 的头部把改动与**不覆盖的项**都列了出来。

## 二、证据清单（本目录）

| 文件 | 内容 |
|---|---|
| `env.txt` | 产物 sha256、两台虚机的事实（发行版/内核/nginx/certbot/python 信任锚）、`systemctl is-active` 耗时分布 |
| `check.txt` | 宿主 `scripts/check.sh` 完整输出（前端 10 项 + Go 13 项） |
| `native-tests.txt` / `native-tests-raw.txt` | 宿主 `gofmt` / `go vet` / `go test ./...` 摘要与原始输出 |
| `logs/2204-install-acceptance.log` | 22.04：安装验收第一部分完整输出 |
| `logs/2204-core-scenario.log` | 22.04：核心场景第二部分完整输出 |
| `logs/2204-installer-group-branches.log` | 22.04：安装器组状态两支的完整输出 |
| `logs/2404-install-acceptance.log` | 24.04：安装验收第一部分完整输出 |
| `logs/2404-core-scenario.log` | 24.04：核心场景第二部分完整输出 |
| `logs/2204-ssh-tunnel.log` | 22.04：从开发机经 SSH 隧道访问面板（`tests/integration/p10-4-ssh-tunnel.sh`，在宿主机上跑） |
| `logs/2404-ssh-tunnel.log` | 24.04：同上 |
| `payloads/2204/`、`payloads/2404/` | 核心场景每步的原始 JSON：预检、提交、作业终态、证书详情（含 `reload`/`served`/`unit_state`/`unit_state_attempts`/`readback`） |

每个日志的末尾都有 `PASS= / FAIL= / NOT RUN=` 汇总，日志本身是完整记录，不是摘抄。

## 三、结果

被测产物：`upanel-0.1.0-linux-amd64.tar.gz`，sha256
`d00bada6d4698c30500b94cc8dd8abce4a6e3f9232e28a802727854b74230644`，由提交 `ac9e4a0` 打出
（`VERSION=0.1.0 sh scripts/release.sh`）。**下面表里的每一个数字都产生于这颗包**：
先提交、再打包、再用这个包把四个脚本全跑一遍（DECISIONS Q317）。二进制自报
`0.1.0 (commit ac9e4a0, built 2026-10-23T08:06:20Z, go1.27.3)`。

两台虚机上的落盘核对（跑完之后取的）：`~/p104/*.tar.gz` 都是 `d00bada6…`；
`/usr/local/bin/upanel-web` `147d327f…`、`/usr/local/bin/upanelctl` `3f084ec4…`、
`/usr/local/lib/upanel/upanel-agent` `f64651e4…`，两台同值，与包内 `bin/` 同值。
核心场景日志第 0 节也把这三个 sha256 打了出来，两处可以对照。

| 脚本 | 22.04.5 | 24.04.5 |
|---|---|---|
| `p10-4-install-acceptance.sh`（安装、校验和、单元、HTTPS、登录、重入、恢复口令、重启） | **PASS=136 / FAIL=0 / NOT RUN=0** | **PASS=136 / FAIL=0 / NOT RUN=0** |
| `p10-4-core-scenario.sh`（完整核心场景） | **PASS=105 / FAIL=0 / NOT RUN=0** | **PASS=105 / FAIL=0 / NOT RUN=0** |
| `p10-4-installer-group-branches.sh`（「组在、账号不在」两支） | **PASS=15 / FAIL=0** | 未跑（该缺陷是 22.04 上抓的；脚本设计为在 22.04 上取证） |
| `p10-4-ssh-tunnel.sh`（**在宿主机上跑**：从外面 `ssh -N -L` 进来访问面板） | **PASS=9 / FAIL=0** | **PASS=9 / FAIL=0** |

核心场景两台各 105 条断言，两台的内容相同：多出来的那一条是「测试 ACME 的随机拒绝比例（5%）」——
它存在的理由见下面 Q316，也提醒读日志的人“偶发一次 badNonce 是夹具的既定随机性”。

四个脚本的 sha256 记在 `env.txt` 第七节：**日志与脚本一一对应**，不是"用某个版本跑完、再用另一个版本发"。
这两台这次签发都是第 1 次就成功（日志里的「签发尝试次数：1」），badNonce 重试路径**这次没有触发**；
它之所以存在于脚本里，是因为 24.04 上的上一轮被 Pebble 的 5% 随机拒签打中过两次并要求重跑（Q316）。
换句话说：这条路有证据（上一轮的日志与 Q316 的记录），但**本目录这两份日志证明不了它**——
这里如实写成"未触发"，不写成"已验证"。

宿主（Windows，Go 1.27.3，node v22.23.3）：`gofmt -l` 无输出、`go vet ./...` 退出 0、
`go test ./... -count=1` **36 个包 ok / 0 失败**、`scripts/check.sh` **PASS 10 / FAIL 0**
（前端）+ **PASS 13 / FAIL 0 / NOT RUN 0**（Go）。这四项是在 `ac9e4a0` 提交之后对着发出去的
那份源码又跑了一遍才写进 `check.txt` / `native-tests-raw.txt` 的，不是提交前的输出。

### 这一轮真机抓出并修掉的 3 个产品缺陷

1. **Q309（最贵的那个）**：24.04 上核心场景 86/16，第一条红是「启用 TLS 之后 80 口仍然是 200
   （期望 301）」。报告写明 `reload=skipped_service_inactive`、`unit_state=""`、`outcome=accepted`。
   根因是测量：`systemctl is-active nginx` 在这台 2 vCPU 虚机上单次可耗时 **11,420ms**
   （另一轮再取样 40 次，max **16,489ms**），超过 10 秒预算 → 状态读到空串 → 代码把「读不到」
   当成「没在跑」→ 配置写下去却**从不 reload**。修法：探测最多 3 次（间隔 300ms）；
   三次都读不到时如实报 `unit_state_error`（每次的耗时与原因）+ `served=false` + 一行
   `systemctl reload nginx` 的下一步；单元确实没在跑时仍然不 reload、不 restart（Q287 不动）。
   `observed` 新增 `unit_state_attempts`（实际探测次数）。验收侧补一条盯根因的断言：
   绑定证书那次应用必须 `reload=done` / `served=true`。
2. **Q305**：22.04 上第 4 步死在 `useradd: group upanel-web exists`——`userdel` 只在组里没有
   其他成员时才删组，「卸过、组还在」是随时会出现的半截状态，而安装器一边宣称幂等一边被它卡死。
   修法：空组 `-g` 复用；组里有成员则停下点名 + 给一行 `gpasswd -d`；卸载补一次 `groupdel`。
   两支都有专门的验收脚本（`p10-4-installer-group-branches.sh`，15/0）。
3. **certbot 失败丢原因**：`syscmd.Runner` 对非零退出返回 `*ExitError`，签发路径把输出整段丢了，
   用户只看到「以退出状态 1 结束」。改用 `errors.As` 分出「certbot 拒绝了」与「跑不起来」，
   把 stderr（没有就看 stdout）的尾部带进错误；`certbot_test.go` 两条用例钉住。

### 找到但**本轮不改**的一处粗糙面

签发失败时，作业接口只回「动作执行失败」（`COMMAND_FAILED`），CA 的具体原因（例如
`JWS has an invalid anti-replay nonce`）**只在 agent 的 journal 里**。P10.3 那次修的是
「把 certbot 的输出带进错误」，它进了日志，但没进 API 的 `error.message`——界面上的用户
仍然只知道“失败了”。改这条要动 `CONTRACTS.md` 的错误体与界面文案，属于计划外改动，
所以这里只点名、不动契约（`DECISIONS` Q316），下一轮做 P11 之前需要的人可以据此提单。

### 验收脚本自身修掉的 9 处（DECISIONS Q306—Q316）

`RUN_TAG` 每轮换域名与幂等键（否则第二次跑必然撞 409/429，而那两条都是产品正确行为）；
收尾走文档路径（`enabled=false` → apply → `DELETE`，不被 409 挡住就当没看见）；
把「域名访问到指定上游」从瞬时请求改成有界轮询（优雅 reload 有 6—111ms 窗口，Q308）；
CSRF 令牌按 cookie jar 存（它是会话的，不是一个全局变量；`$(login …)` 是子 shell，赋值传不出来）；
wss 显式传 ACME 根（22.04 镜像里 python 的系统信任锚是 0 个，24.04 是 120 个——系统信任链由
同节的 curl 断言单独证明）。

后四处是这一轮反复重跑时挖出来的，都不是产品问题，但每一条都曾把一次绿跑成红：

- **Q313 测试 ACME 夹具每轮从零开始**：原来“14000 口有响应就直接用”，沿用的可能是上一轮留下的
  Pebble（账号与 nonce 都在它内存里），而 certbot 缓存着账号 URL。24.04 上红过一次。
- **Q314 reload 之后的断言要等收敛**：「80 是 301」原来只问一次，问到的可能是还没交班的旧 worker
  （`Content-Length: 157` 的上游响应）。22.04 上红过一次，同轮 443/wss/重启后全绿。
  顺带踩到一个更基础的坑：wait 辅助函数的提示打到 stdout，被 `$(…)` 一起捕获，
  `expect_eq` 拿“提示 + 301”去比“301”，两台同时红——提示一律 `>&2`。
- **Q315 脚本必须是 LF**：在 Windows 上写盘留下 CRLF，git-bash 的 `sh -n` 不吭声，
  拷到 Ubuntu 上 dash 在 `set -u` 处直接退出（`set: Illegal option -`）。
- **Q316 只对 badNonce 重试**：测试 ACME 故意随机拒 5% 的合法 nonce（它自己日志里写着），
  certbot 2.9 在取证书那条路径上不重试，于是一次坏运气就把签发打成 failed。
  重试仅限 `anti-replay nonce`、最多 3 次、幂等键每次不同、每次失败另存并记录。

全过程的完整输出见 `logs/`，每步的原始 JSON（预检、提交、作业终态、证书详情）见 `payloads/`。

## 四、没跑的检查（NOT RUN，不算通过）

这一节是上一句「不写成通过」的兑现清单，与 `env.txt` 第六节一致：

- **真实公网域名与真实 Let's Encrypt**：验收全程用 Pebble（本地测试 ACME，`127.0.0.1:14000`），
  不碰正式环境的速率限额。所以「向 LE 申请真证书」这条**没有证据**，只有「ACME 流程本身跑通」。
- **浏览器渲染与地址栏**：两台虚机没有浏览器。HTTPS 用 `curl --cacert`、`openssl s_client`、
  python `ssl` 的语义结果验证（证书链、SAN、协议版本、HSTS 头），**没有截图**，
  也**没有**说「地址栏变绿」这种只有人眼能确认的事。
- **SSH 隧道的人工操作**：`docs/ACCESS.md` 的隧道步骤是**文档交付物**（写清楚了 `ssh -L` 命令、
  为什么浏览器看到的是 `https://127.0.0.1:9443`、以及什么时候该换掉自签名证书）。
  隧道的机械部分在本目录有单独取证（`logs/*-ssh-tunnel.log`），但那是在 Linux 上从 `sshd`
  转发的回环连接，**不等于**在 Windows 终端和手机上按文档点过一遍。
- **`--install-deps` 的 apt 安装**：安装脚本 `--install-deps` 会跑 `apt-get update` 装依赖；
  验收机上只在「联网可用」这一层取证，**没有**逐个核对装上的版本是否满足最低要求。
- **arm64 包的真机运行**：只验证了它能打包、`SHA256SUMS` 正确、包结构与 amd64 同形；
  真机运行与安装只发生在 amd64 上。
- **分区/格式化/网络断连**：按约束只能在一次性的独立测试虚机里做，且不属于 P10 范围；
  本轮**一次都没做**。
- **`p10-4-installer-group-branches.sh` 在 24.04 上**：设计上就在 22.04 取证（缺陷是在 22.04 抓的），
  24.04 上**未跑**。
- **badNonce 重试路径（本目录这两份日志）**：两台这次都是第 1 次签发就成功，重试分支没进去，
  所以这两份日志**不构成**对它的验证；它的证据在 Q316 记录的那一轮。
- **运维动作的人工入口**：核心场景全部走 REST（`curl` 打预检 → 提交 → 轮询终态），
  面板页面上的点击、浏览器里的实际交互**没有**在虚机上跑（前端只有宿主的 `scripts/check.sh`）。
- **证书续期**：核心场景只走「首次签发」（`certificates.issue` + `proxy.apply`），
  续期路径（`renew` 与到期信息刷新）本轮**没有在真机上跑过**；也**没有**等过一次真实到期。
  脚本里关于 301 与 `/.well-known` 的注释是设计依据，不是运行证据。

> 一句话：本目录证明的是**在 22.04.5 与 24.04.5 上，0.1 包的安装、校验和、systemd 单元、
> 初始用户、HTTPS 访问、口令恢复、重启后可用、以及代理/证书/共享的核心运维动作都按文档工作**。
> 它不证明上面这几条，也不替它们作保。

