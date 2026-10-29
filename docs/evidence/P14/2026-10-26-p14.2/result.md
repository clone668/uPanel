# P14.2 验收结果：把地址改坏之后，机器怎么自己回来

- 日期：2026-10-26（UTC）
- 分支 `main`，工作树含本轮改动（按用户要求不提交不推送）
- 验收对象：**`network.apply` / `network.confirm` / `network.rollback` 这三个写动作**——单物理
  网卡的 IPv4 受限修改、独立于面板的 systemd 回滚 timer、候选地址确认、以及"改坏了怎么办"。
- 验收方式：真实 Web（非 root，`upanel-web`）→ `/run/upanel/agent.sock` → root 代理 → 真实
  Ubuntu 主机。**把 ens4 改成一个不可达地址（`10.99.99.99`）**，停掉 Web 与代理，让"能救这台
  机器的只剩 systemd timer"，然后等 120 秒看它自己回来。
- 为什么不靠 mock：mock 能证明 handler 把代理的判决原样吐出去（那部分由
  `internal/api/networkexpand_test.go` 与 `internal/network/applyengine_test.go` 覆盖）。
  这一层要证明的是"**内核里真的发生了**"——地址真的换了、真的断了、timer 真的在没有任何
  upanel 进程时把它退回来了。这些只有真机能给。
- 浏览器层为什么不在 VM 上验：VM 里没有 Playwright（DECISIONS Q21），页面由宿主侧
  `vue/server-renderer` 渲染测试与 `web-smoke` 覆盖；VM 上钉的是**页面拿到的事实**。

## 1. 结论

**STAGES §16 的四条硬条件，这一轮全部拿到了现场证据。**

| §16 要求 | 现场 | 证据 |
|---|---|---|
| 故意配置不可达地址/规则 | `10.99.99.99/24`，`outcome=verified`，`ip` 读回就是它，旧 DHCP 地址与默认路由都消失了 | `logs/p142-2204-run5.log` 阶段 7 前 10 条；`facts/job-apply-unreachable.json` |
| 断开浏览器并停止 Web 进程 | Web 端口关闭、`/run/upanel/agent.sock` 消失、`upanel-agent` 进程不在——**此后除了 systemd 没有任何 upanel 进程** | 同上 `stop-services` 段；`journal-netrollback.txt` 里 15:03:41 那行 `upanel-agent[148818]` 是新拉起的 oneshot，不是常驻代理 |
| 120 秒后仍能自动恢复 | 记录自己写的窗口是 `applied_at=15:01:12Z → deadline=15:03:12Z`；timer 15:03:32Z 起、15:03:41Z 完；地址在 `deadline+28s` 回来 | `facts/rollback-record-unreachable.json`、`facts/journal-netrollback.txt` |
| 旧地址不能绕过新地址验证 | 回环地址（旧地址一侧）确认被 `CONFIRM_WRONG_ADDRESS` 拒，理由里点名到达地址与候选地址；新地址一侧确认 `outcome=verified` 后记录转 `confirmed` | 日志 109–115 行、117–123 行；`facts/preflight-preflight-confirm-oldaddr.json` |

另外两条 §16 里点名要的：**复杂 netplan 返回 `unsupported`**（阶段 5：写进 `/etc/netplan`
的多文件/网桥构造，产品拒绝并原样说明，**没有 apply**）；**旧地址确认被拒后 timer 仍然在跑、
记录仍然 `pending`**（拒绝不等于取消，谁都没被顺手清理掉）。

**轮次与最终数字**：

- 最终轮（第五轮）：`PASS 186 / FAIL 0 / NOT RUN 3`，退出码 **0**，
  `STAMP 2026-10-26T14:58:14Z start` → `2026-10-26T15:05:11Z done`。
- 验收脚本 sha256 `6a3a4ed8259461d7c6a53ea647ea21011fb0647ef824fa25f6186e0f92f95b95`
  （宿主与 VM 上 `/opt/upanel-accept/p14-2-acceptance.sh` 一致）。
- 二进制：`upanel-agent e19852f8…caf8`、`upanel-web 6be39c17…0325`、
  `upanelctl a0ca7219…f580`（`facts/env.txt`）。
- 环境：Ubuntu 22.04.5 LTS，kernel `5.15.0-198-generic`，systemd `249.11-0ubuntu3.23`，
  ens4 走 slirp DHCP `10.0.2.15/24`，网关 `10.0.2.2`。

**这一轮之前崩过三次，原因都记下来，因为它们比数字有用**：

1. 第一、二轮：阶段 6.2 要绑新地址，产品按 `config.CheckTransport` 拒绝明文非回环监听，
   `start_web_on` 起不来，其后十几条全是连锁假红（详见 §2）。
2. 第三轮：7.4 按"轮询 14 次"收工，而 deadline 还没到，四条断言在**到期之前**采样 →
   假红；同一轮还抓到真产品缺陷（切回 DHCP 的核验判据是反的，§4）。
3. 第四轮：`PASS 181 / FAIL 2`，两条红都是我写错的断言——systemd 249 的 journal 里
   **没有** `ExecStart` 行、也**不写** `status=0/SUCCESS`，我去问了一件它不记的事。
   改成问"进程标识是 `upanel-agent[`"、"模板里的 `netrollback --token %i`"、"`Finished`
   + `Deactivated successfully` + 不含 `Failed with result`"之后，第五轮绿。

**结论：P14.2 passed。** 按 §16 的硬条件，**"无法真实验证独立网络回滚"这个前提已经不成立了**
——证据在 §3，地址是 systemd timer 在 `deadline+28s` 自己退回来的，当时 Web 与代理都不在。

但 `-network-writes` 的默认值**仍然是 `false`**，理由换了一条：改网卡是会把机器从网络上摘掉的
操作，"要不要给面板这个权力"是装机时由人决定的事（P16.1 的服务单元、P16.4 的手册），
不该由一次验收顺手替所有既有部署决定。开关的说明文字已经按这个新理由改过（`cmd/upanel-agent/main.go`），
不再写"仅用于测试 VM / 尚未验收"——那句话现在是假的。见 Q458。

`GET /network/config` 在没有开关时依旧返回 `capability.writable=false` 且**不带 `config_hash`**，
所以面板那边看到的是一个"能读不能改"的入口，不是一个可以点的按钮。

## 2. 这一轮为什么全程 HTTPS

面板在阶段 6.2 要**绑到新地址上**（`10.0.2.15`）去验"从新地址确认"，而产品的传输策略是硬的：
`config.CheckTransport` 拒绝"非回环监听 + 明文"，理由不是洁癖——会话 Cookie 带 `__Host-`
前缀，只在 HTTPS 下回传，明文监听等于把会话交给链路。

第一、二轮就是在这里崩的：`start_web_on "$CUR_ADDR"` 起不来，产品报
`监听地址 10.0.2.15:19473 不是回环地址，必须配置 web.tls_cert_path 与 web.tls_key_path`。
当时脚本还"记一条 fail 就继续"，后面十几条断言全是连锁假红（其中"journal 里没有回滚单元
输出"甚至是在真回滚发生**之前**取的）。

所以这一轮：脚本自己签一张证书（SAN 含 `127.0.0.1`、`10.0.2.15`、`10.99.99.99`，私钥
`root:upanel-web 0640`），`allowed_origins` 给三个 https 源，`curl` 统一 `-k`，另加
`must_start_web_on`——起不来就留证并 `exit 1`，不再拖着一串必然红的断言往下跑。

## 3. 断连重连这一段到底发生了什么

阶段 7 的目标只有一个：让"能把这台机器救回来的"只剩 systemd。所以脚本先改地址，再把 Web
和代理**都杀掉**，然后什么都不做，等。

时间线全部来自 `facts/` 与 `logs/p142-2204-run5.log`，没有一处是推算的：

| UTC | 事件 | 出处 |
|---|---|---|
| 15:00:48 | 阶段 7 开始，`ens4` 还是 `10.0.2.15/24`（DHCP） | 日志 `phase7-start` |
| 15:01:1x | `network.apply` 提交 → 预检 `allowed=true`，影响说明里写了 120 秒与自动退回 | 日志 |
| 15:01:12 | 记录落盘：`applied_at=15:01:12Z`、`deadline=15:03:12Z`、`state=pending` | `facts/rollback-record-unreachable.json` |
| 15:01:16 | `Started uPanel network rollback timer (nrb_bfc428…)` | `facts/journal-netrollback.txt` |
| 15:01:36 | 任务返回 `outcome=verified`，`observed_state.address=10.99.99.99` | `facts/job-apply-unreachable.json` |
| 15:01:39 | **停 Web、停代理**：端口关闭、`/run/upanel/agent.sock` 消失、`upanel-agent` 进程不在 | 日志 `stop-services` |
| 15:02:02 | 停掉之后 60 秒：仍停在 `10.99.99.99`，timer 仍 `active`，记录仍 `pending` | 日志 `mid-window`（窗口没被提前咬掉） |
| 15:03:12 | **deadline**（记录自己写的，不是脚本算的） | 记录 |
| 15:03:32 | `Starting uPanel network rollback (nrb_bfc428…)` —— deadline + 20s | journal |
| 15:03:38 | `upanel-netrollback@….timer: Deactivated successfully` / `Stopped`（回滚自己把 timer 停了） | journal |
| 15:03:11→41 | 地址回到 `10.0.2.15/24 dynamic`，默认网关回来 —— **deadline + 28s** | 日志 + `facts/addr-after.txt` |
| 15:03:41 | `upanel-agent[148818]: 回滚 nrb_bfc428… 已处理`、`Finished`、`Deactivated successfully`、`Consumed 3.013s CPU time` | journal |
| 15:04:19 | 备用看门狗（systemd 绝对时刻）：`not-needed: ens4 上是 10.0.2.15/24` | `facts/watchdog-systemd-timer.log` |
| 15:04:37 | 备用看门狗（sleep 子壳 +215s）：同上 | `facts/watchdog-sleep-shell.log` |

三个可以直接看的结论：

1. **回滚不是面板做的。** 15:01:39 之后到 15:03:32 之间，这台机器上没有一个 upanel 进程在跑。
   拉起回滚的是 systemd（`Starting uPanel network rollback (…)` 是 systemd 自己写的行），
   干活的是 `upanel-agent netrollback --token %i` 这个 oneshot——journal 里的进程标识是
   `upanel-agent[148818]`，而常驻代理在 15:01:39 就已经不在了，所以这是**新起的一个进程**。
2. **窗口没被提前咬掉，也没被拖到窗口之外。** 60 秒时还在不可达地址上；地址在
   `deadline+28s` 回来，落在"deadline 之后、deadline+45s 之内"这两条断言之间。
3. **两条看门狗都没动手。** 它们本来是用来在"产品自己没回来"时报告失败的救命绳；它们各写了
   一行 `not-needed`，正好反证回滚是产品自己完成的。看门狗用的是**绝对时刻**（`OnCalendar=`），
   不是相对计时——理由在 §5.1。

`deadline + 20s` 才触发，不是 systemd 慢了 20 秒的锅，也不是"窗口被延长了"：记录里的
`deadline` 是 15:03:12，timer 武装时按 `OnActiveSec=120s` 从**武装那一刻**起算，而武装那一刻
（15:01:16）比 `applied_at`（15:01:12）晚 4 秒；剩下的十几秒是 `daemon-reload` 把相对计时器
重新起算的已知行为（Q457）。所以真实的"从改完到退回来"是 15:01:36（任务返回）→ 15:03:41（退回），
**125 秒**；从 deadline 算，晚了 28 秒。这两句话都不好听，但都是真的，写在这里比藏起来有用。

## 4. 真机抓到的产品缺陷：切回 DHCP 的核验判据是反的

**这是本轮唯一一个"单测全绿、真机必红"的缺陷**，所以单独一节，写给后来的人当样本。

**现场**（第三轮，22.04 VM）：同一轮里先把 `ens4` 从 DHCP 改成静态 `10.0.2.15/24`
（面板自己那份 `90-upanel-ens4.yaml`），再从静态**改回 DHCP**。任务跑完，`outcome`
读出来是 `rolled_back`，回滚记录里写着"切到 DHCP 之后旧地址 `10.0.2.15` 还在网卡上"。
而那一刻 `ip` 读回来的是 `10.0.2.15/24 … dynamic`——**机器上其实已经是 DHCP 了**。
页面给用户看的是"改回 DHCP 失败"，回滚把一件成功的事退回去了。

第三轮的完整日志**没有留下来**：我在拉取证据之前先 `rm -rf` 了那个工作目录。这一条记为
本轮的方法失误，不掩饰。上面这几句现场的出处是当时会话里的读取；这一轮（第五轮）重新跑
同一条路径，日志在 `logs/p142-2204-run5.log` 第 129–133 行：

```
PASS  改回 DHCP：allowed=true
PASS  改回 DHCP：任务 succeeded
PASS  改回 DHCP：outcome=verified
PASS  DHCP 记录里没有候选地址（DHCP 拿到的地址是随机的，不能做凭据）
PASS  DHCP 记录的 mode 是 dhcp
```

**根因有两层，第二层才是真凶**：

1. DHCP 分支调的是 `waitForAddress(exact=false)`——它只要求"这张网卡上有全局 IPv4"。
   切到 DHCP 之后，**旧静态地址一直在**，这个条件从第一个采样就成立，于是那段"等租约"
   是**空转**的：一秒都没等。
2. 紧接着的判据是"旧地址必须消失"（`hasGlobalIPv4(prev.Address)` 必须为假）。在 slirp 里
   DHCP 恒发同一个地址 `10.0.2.15`，所以"旧地址还在"永远成立 → 判失败 → 立刻回滚。
   **"地址还在"恰恰就是成功的样子。**

**改法**：DHCP 的核验改成等"这张网卡上**恰好一个**全局 IPv4"（`waitForSingleAddress`），
窗口 30 秒（静态仍 15 秒，`dhcpVerifyWindow`）。这个判据一次覆盖两件事：

- 交接过渡：旧的在、新的还没来 → 0 个或 2 个，继续等它收敛；
- 原来那条检查真正想抓的：**别的 netplan 文件也在给这张网卡配地址**——那个地址不会被
  networkd 摘掉，会一直停在两个，等满窗口后报"2 个全局 IPv4"并回滚。

`waitForSingleAddress` 的两条失败分支都是可读的句子，不是布尔值：0 个报"DHCP 没有发下租约"，
多个报"多个说明别的配置文件也在给这张网卡配地址"。窗口抽成 `ApplyEngine.dhcpWait` 字段
是为了让单测能压成 0——否则"等不到"那条分支每条用例要真睡 30 秒，那种用例不会有人跑。

**已知抓不到**（写在 §5.2，不假装覆盖了）：别的配置文件把旧地址**原地声明成同名静态地址**时，
它与"DHCP 又发了同一个地址"在地址层面无法区分。

`waitForAddress` 同时去掉了 `exact` 参数，只服务静态地址。留着一个"看起来能配 DHCP"的参数，
合用的那天一定有人把 DHCP 传进来，然后"旧静态地址还在"又被当成"新地址到了"——同一个坑，
第二次掉进去。

## 5. 已知限制（写在这里，不假装已覆盖）

### 5.1 回滚 timer 用的是相对计时（`OnActiveSec`），会被并发的 `daemon-reload` 顶后

见 DECISIONS Q457。方向是**单一**的：窗口只会被拉长，不会被提前咬掉；而且记录里的
`deadline` 是绝对时刻，代理下次启动时 `Reconcile` 拿它比对，过期就直接回滚，所以偏差不会
跨重启累积。改法（`OnCalendar=<deadline>`）已经想清楚，但会动 CONTRACTS §13.6 与三处单测，
本轮**不做**——验收的核心判据（"120 秒后仍能自动恢复"）在现机制下已经真机通过，换个机制
再验一轮不该塞进同一个子任务。

### 5.2 别的配置文件把旧地址原地声明成同名静态地址时，判不出来

DHCP 模式下核验的是"这张网卡上恰好一个全局地址"。如果另一份 netplan 文件声明的地址**正好
等于** DHCP 发回来的那个，地址层面无法与"这个地址是租来的"区分。抓住的是另一种：
别的文件配了**不同**的地址 → 网卡上两个全局地址 → 不通过。这一条写进 Q456 的"已知抓不到"。

## 6. NOT RUN（按设计）

脚本自己报了 3 条（`logs/p142-2204-run5.log` 末尾），我另外补 4 条——**脚本没报不等于验过**：

**脚本里的 3 条**：

1. **浏览器里的渲染与断线重连交互** —— VM 里没有 Playwright（Q21）。页面层由宿主侧
   `vue/server-renderer` 的 SSR 渲染测试与 `web-smoke` 覆盖；VM 上钉的是"页面拿到的事实"
   （`GET /network/config` 在断连前后的 `pending` 状态、倒计时该读的 `deadline`）。
2. **网卡被改名/拔掉（`NETWORK_TARGET_CHANGED` 的另一半）** —— 改网卡名要重启，重启会把
   timer 的语义一起冲掉（`OnActiveSec` 从开机起算），那就不是"验回滚"而是"验另一件事"了。
   单测覆盖了判决形状。
3. **回滚自己失败（`failed` 分支）** —— 真机上造它等于把机器弄坏到看门狗之外，风险不对等。
   这一条永远只能是"设计上可读"（`unknown` + 明确的 `recovery_hint`），不是"验证过"。

**我补的 4 条**：

4. **`network.rollback` 的"立刻退回"真机路径没跑。** 脚本只在写开关关闭时验了它的预检
   400（日志第 37–38 行），真正"提交 rollback、不等 timer、立刻退回"这条路只有单测
   （`internal/network` 的 `rollback` 组）。这一条是我这一轮**主动没做**：它要占用一次
   "现场改成不可达地址 + 再立刻退回"的完整往返，而阶段 7 已经把同一条恢复链路（含
   `netplan apply`、`verifyRestored`、`markRecord`、`disarmRollback`）走过一遍；再加一轮
   只会多一份重复证据，不会多一个覆盖。记在这里，不含糊。
5. **`Reconcile`（开机对账）在真机上没验**。阶段 8 只是重启代理后断言"没有待确认记录"，
   那是"没有记录时的行为"，不是"有一条过期记录时它会退回去"。那条路只有单测
   （过期即回滚、未过期按剩余时间重新武装）。
6. **24.04 上没跑 P14.2 验收**。本轮全部在 22.04.5（systemd 249）上。24.04 的 systemd 是
   255，timer/journal 行为有差异（`OnCalendar=` 与 `AccuracySec` 的实现细节不同）。
   双平台覆盖是 P16.2 的事，这里不预先声称。
7. **真实物理网卡 / 真实 DHCP 服务器没验**。slirp 的 DHCP 恒发 `10.0.2.15`，这既是
   §4 那个缺陷暴露出来的原因，也意味着"租约换了一个地址"这条最常见的正常路径在真机上
   **一次都没出现过**。它只被单测覆盖：`TestApplyDHCPKeepsOwnershipAndDropsOldAddress`
   （旧的被摘掉、新的到了 → `verified`）、`TestApplyDHCPVerifiedWhenServerReissuesSameAddress`
   （同名地址回来 → `verified`）、`TestApplyDHCPRollsBackWhenAnotherFileAlsoConfigures`
   （两个地址 → `rolled_back`）。三面都有人管，但没有一面是内核给的。

## 7. 复现

**前置**：一台独立 Ubuntu VM（本轮是 22.04.5，QEMU + slirp，网卡 `ens4` 走 DHCP，
宿主 `hostfwd` 到 guest `:22`），root 可登录，有 `python3`/`netplan`/`runuser`/`openssl`。
**不要在任何在用的机器上跑这个脚本**——它会把那张网卡改成一个不可达地址。

```sh
# 1) 宿主交叉编译（不要把 Go 工具链搬进 VM）
cd <repo>
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 sh -c '
  for c in upanel-agent upanel-web upanelctl; do
    go build -buildvcs=false -trimpath -o "/tmp/upanel-bin/$c" "./cmd/$c" || exit 1
  done'
scp -i <vm-key> -P <port> /tmp/upanel-bin/* upanel@<vm>:/tmp/upanel-bin/

# 2) 脚本与 JSON 助手放同一目录（脚本按 $0 找助手）
scp -i <vm-key> -P <port> \
  tests/integration/p14-2-acceptance.sh tests/integration/json_helper.py \
  upanel@<vm>:/opt/upanel-accept/

# 3) 清场：上一轮的面板文件、回滚记录、残留 timer 与**模板**都要清掉。
#    模板（upanel-netrollback@.service）留着会让"模板里 ExecStart 跑的是 netrollback"
#    这条断言假绿——它认识的是上一轮那份文件。
sudo rm -rf /var/tmp/upanel-accept-p14-2 /var/lib/upanel-agent/network
sudo rm -f  /etc/netplan/90-upanel-*.yaml /etc/systemd/system/upanel-netrollback@*
sudo systemctl daemon-reload && sudo netplan generate && sudo netplan apply
ip -4 -o addr show ens4        # 应当是 dynamic，且不在 10.99.99.0/24 里

# 4) 起跑。**必须 detached**：阶段 7 会把 ssh 真的掐断，前台跑法会被 SIGPIPE 杀死。
sudo BIN_DIR=/tmp/upanel-bin setsid nohup sh /opt/upanel-accept/p14-2-acceptance.sh \
     >/var/log/p142-run.log 2>&1 &
# 结论写 $WORK/RESULT 与 /var/log/p142-run.log，退出码写在哪里由调用方决定，例如：
#   ... >/var/log/p142-run.log 2>&1; echo $? >/var/log/p142-run.rc
```

**预期**：阶段 7 会断线 2—3 分钟（地址换到 `10.99.99.99`，slirp 的端口转发随之失效）。
线回来之后 `grep -E '^(== |PASS [0-9]|FAIL )' /var/log/p142-run.log` 应当看到
`PASS 186 / FAIL 0 / NOT RUN 3`，退出码 0。

**这一轮用的二进制与本轮最终工作树的差异**：只有 `cmd/upanel-agent/main.go` 里
`-network-writes` 的**说明文字**与 `internal/agent/systemquery.go` 里 `NetworkWrites`
的一段**注释**（Q458 改的口径）。二者都不参与任何被验行为——验收是显式带 `-network-writes`
跑的，flag 说明字符串不进决策、不进返回值、不进任何断言。记在这里，不假装二进制逐字节相同。
