# P14.3 验收结果：改防火墙之前，先证明"改完你还进得来"

**日期**：2026-10-27（VM 内时间 2026-10-26T19:06:48Z — 19:13:56Z，UTC）
**机器**：Ubuntu 22.04.5 / kernel 5.15.0-198-generic / systemd 249.11 / ufw 0.36.1，QEMU slirp，`ens4`
**结果**：**PASS 153 / FAIL 0 / NOT RUN 4**，退出码 0（第三轮）
**脚本**：`tests/integration/p14-3-acceptance.sh`，sha256 `ce8c0a05f5aff6f00028bf4aa1448db1fccd777398c00178c4611f862b59aab6`
**二进制**：`upanel-agent 5a321eaa…f889db`、`upanel-web 96e571a1…9ce6e1c`、`upanelctl 190bc152…297fc46`（宿上交叉编译的同一组，VM 内 `sha256sum` 核对一致）

---

## 1. 结论

**P14.3 的四个验收点在真机上成立。**

1. **管理通路是阻塞项**：要启用、但提交的规则集里没有 `from any` 的 22/tcp 放行时，预检
   `allowed=false` 并给出 `FIREWALL_MANAGEMENT_PATH`，**一个字节都没动**（`ufw status` 仍是
   inactive、`ufw show added` 仍是 `(None)`）。把 22/tcp 加回定义之后同一份提交通过。
2. **独立回滚不需要面板**：apply 一份新规则集 → 不确认 → **停掉 Web 与代理** → 到期后规则
   自己退回旧的。journal 里动手的是 `upanel-fwrollback@<token>.service`，进程标识是
   `upanel-agent[162295]`，不是 Web。
3. **只碰带标记的规则**：管理员手写的 `ufw allow 9000/tcp`（没有 comment）跨两次 apply 原样
   存在，`show added` 前后逐字节相同。
4. **撤干净用产品自己**：`{enabled:false, rules:[]}` → `ufw status` 回到 inactive、
   `show added` 回到 `(None)`、默认策略三行前后一致。

**这一轮真正花时间的地方不是"验过了"，是"验的东西本身对不对"**：第一轮 132/13 红，13 条里有
**1 个真缺陷（8 条红）+ 5 条脚本自己的假红/假绿**。两件都记在第 4、5 节。这也是为什么这一轮
跑了三次——第一轮死在环境检查（`grep active` 匹配 `inactive`），第二轮把缺陷暴露出来。

---

## 2. 这一轮验的范围

STAGES 对 P14.3 的要求是「UFW 规则与预检、保留管理通路、独立回滚、实际规则验证」。拆成
10 个阶段、153 条断言：

| 阶段 | 断言 | 验的是什么 |
|---|---|---|
| 0 环境 | 12 | root、三个二进制、python3、`runuser`、ufw 0.36.1；**起点必须 inactive 且没有面板规则**；默认策略先读出来当基线 |
| 1 装配 | 3 | 代理（**不带** `-firewall-writes`）能起、密钥、`init-admin` |
| 2 写路径默认关闭 | 16 | `GET /firewall/config` 与三个动作都返回 400「未登记的查询 / 未登记的动作」；期间 ufw 一动不动、连回滚记录目录都不该出现 |
| 3 打开开关 | 12 | 只加 `-firewall-writes`（**不加** `-network-writes`）：`manager=ufw`、`installed`、`editable`、`rules=0`、给了 `config_hash`、`pending=null`；未登录 401 / viewer 403 |
| 4 预检判决 | 24 | 坏指纹 → `FIREWALL_CONFIG_CHANGED`；四种坏定义 → **解码层 400**；无 pending 时的 confirm/rollback → `FIREWALL_NO_PENDING_ROLLBACK`；**缺 22/tcp → `FIREWALL_MANAGEMENT_PATH`**；停用状态下同一份定义不拦；全程 `ufw show added` 逐字节不变 |
| 5 第一次真改 | 31 | 启用 + 22/tcp + 8443/tcp → `verified`；记录 0600、timer 武装（`NextElapse` 不是 `infinity`）、模板 `ExecStart` 指向本次的代理二进制；`Default:` 三行不变；手工确认 → `confirmed` + timer 停 + 再确认被拒 |
| 6 加缺删多 | 13 | 手写 `9000/tcp` 先放进去 → 定义改成 `ssh+db` → `upanel:web` 消失、`upanel:db` 出现、**9000 原样**；token 不符 → `FIREWALL_TOKEN_MISMATCH`；已有 pending 时不许再 apply |
| 7 独立回滚 | 24 | 见 §3 |
| 8 恢复之后 | 11 | 重起服务、`pending=null`、无残留 timer 单元、日志无 panic、审计 failure/success 计数 |
| 9 撤干净 | 11 | `{enabled:false, rules:[]}` → inactive + 零面板规则；确认之后没有 pending；默认策略仍一致；4 条 NOT RUN |

---

## 3. 断连那一段：把 Web 和代理都停掉，看规则自己回来

阶段 7 的现场（`logs/p143-2204-run3.log` 第 130—160 行，时间取自脚本的 `STAMP`）：

```
（阶段 7 开始）apply：ssh + 8443/tcp → outcome=verified
           token=frb_695fe87aa232cb87fa6310eef902d7b9
           deadline=2026-10-26T19:12:26Z（T0+99s）
19:10:51Z  services-stopped   ← 停掉 Web 与代理，curl health 返回 000
19:11:47Z  t-plus-60         ← 60 秒时 web 那条还在、timer 仍武装
19:12:26Z  deadline
19:12:34Z  systemd 启动 upanel-fwrollback@frb_695f….service（journal）
19:12:42Z  after-deadline     ← 脚本这时的采样：web 没了、db 回来了（脚本算 deadline+14s）
19:12:45Z  upanel-agent[162295] 打出"防火墙回滚 frb_695f… 已处理"；Deactivated / Finished（journal）
19:12:46Z  phase7-done
```

时间戳的来路要说清楚：`STAMP` 那三行是脚本自己 `date -u` 打的，`19:12:34` / `19:12:45` 是
journal 的秒级时间。两个时钟会互相错开几秒——脚本在 `19:12:42` 采样时规则已经退回，而 journal
里"已处理"那行落在 `19:12:45`。所以"回滚什么时候完成"没有一个到毫秒的答案，**能站住的判据是
顺序**：deadline 之后才动、动完之后记录是 `rolled_back`、`ufw` 仍然 active。


journal 原文（`facts/journal-fwrollback.txt`）：

```
Oct 26 19:12:34 upanel-2204 systemd[1]: Starting uPanel firewall rollback (frb_695fe87aa232cb87fa6310eef902d7b9)...
Oct 26 19:12:45 upanel-2204 upanel-agent[162295]: 防火墙回滚 frb_695fe87aa232cb87fa6310eef902d7b9 已处理
Oct 26 19:12:45 upanel-2204 systemd[1]: upanel-fwrollback@frb_695f….service: Deactivated successfully.
Oct 26 19:12:45 upanel-2204 systemd[1]: Finished uPanel firewall rollback (frb_695fe87aa232cb87fa6310eef902d7b9).
```

三件必须同时成立的事，都成立：

- **退回去的是规则集，不是把防火墙关掉**：回滚之后 `ufw status` 仍是 `active`。关掉也能让人
  进来，但那等于用"没有防护"换"能登录"，不是这个功能该做的事。
- **窗口没被提前咬掉**：60 秒时新规则还在、timer 还武装。`deadline-60` 这条断言防的是
  `timer` 被 `daemon-reload` 顶后（Q457 那个坑）——顶后的表现是"提前退回去"，看起来像成功。
- **动手的是代理不是面板**：`upanel-agent[162295]`。Web 进程那时已经不在。

### 规则集的前后对照

```
首次 apply 之后（facts/show-added-after-first-apply.txt）
  ufw allow 22/tcp comment 'upanel:ssh'
  ufw allow 8443/tcp comment 'upanel:web'

回滚之后（facts/show-added-after-rollback.txt）
  ufw allow 22/tcp comment 'upanel:ssh'
  ufw allow 5432/tcp comment 'upanel:db'
```

`web` 换回 `db` — 这正是阶段 6 结束时的那份规则集，也就是"改之前的样子"。

### 两条救命看门狗都醒了，都没动手

这一轮的风险不是"地址没了"而是"规则把 22 挡了"，所以看门狗的判据是
**UFW 启用、且没有任何 `from any` 的规则放行 22/tcp**。它们动手就留 `watchdog-fired-*`，
验收会因为那个标记判 FAIL。

```
19:10:15Z systemd-timer  not-needed: 22/tcp 有放行
19:10:37Z sleep-shell    not-needed: 22/tcp 有放行
```

两条互相独立（一条 systemd `OnCalendar` 绝对时刻、一条 `sleep` 子壳），都按点醒来、都判定
不需要动手。这句话只有在它们**确实跑过**之后才有意义——见 §5 第 3 条。

---

## 4. 真机抓到的产品缺陷：一条拒绝被包装成了 500

### 现场

第一轮（2026-10-26T18:5x）`132 PASS / 13 FAIL`。其中 8 条的失败长这样：

```
FAIL  没有待确认记录时 confirm 预检 200: 期望 '200'，实际 '500'
FAIL  命中 FIREWALL_NO_PENDING_ROLLBACK: 缺少 'FIREWALL_NO_PENDING_ROLLBACK'（实际: ERR:KeyError:'data'）
FAIL  rollback 也命中 FIREWALL_NO_PENDING_ROLLBACK: ... ERR:KeyError:'data'
      （外加 token 不符那一组 4 条，同一形状）
```

`KeyError: 'data'` 的含义是"响应里没有 `data` 字段"——因为它不是预检响应，是错误响应：

```json
{"error":{"code":"INTERNAL_ERROR","message":"服务内部错误"},"request_id":"2596b215…"}
```

web.log 里同一 `request_id` 的原文：

```
{"level":"ERROR","msg":"代理返回未知错误码","error":"agentclient: 代理返回 500 INTERNAL_ERROR: 预检结果不合法"}
```

### 根因

`internal/agent/preflight.go` 在回响应前跑 `validatePlanResult`：

```go
if plan.NormalizedParams == nil {
    return errors.New("缺少规范化参数")
}
```

而 `FirewallEngine` 的三个预检函数里，`Assessment.Normalized` **只在"能执行"那条路上赋值**：

| 函数 | 拒绝分支 | 修复前 `Normalized` |
|---|---|---|
| `AssessConfirm` | `rec == nil`（无 pending） | nil |
| `AssessConfirm` | `token` 不符 | nil |
| `AssessRollback` | `rec == nil` | nil |
| `AssessRollback` | `token` 不符 | nil |
| `Assess` | `ValidateFirewallDefinition` 失败 | nil |

于是"你没有待确认的改动"这条最普通的业务拒绝，被包装成"服务内部错误"。

### 为什么这条链每一段单独看都合理

- 预检器用 `Allowed=false` + `Blockers` 表达业务拒绝 —— 这是全项目的约定；
- `validatePlanResult` 认为"连规范化参数都没有的计划不可能被执行" —— 自洽性检查，没错；
- `NormalizedParams` 的意义是让执行端回放一份**已经洗过**的参数 —— 也没错。

合起来的结果是：用户拿到 500，会去查日志、怀疑代理挂了、重启服务，而真相是"你确认得太早了"。
**危害不在功能（拒绝确实发生了），在于拿到的是错误的话。**

### 修法

`§13.6` 的 `AssessConfirm` 本来就把 `normalized := params` 摆在函数第一行，防火墙这一版漏了。
照抄：

```go
normalized := FirewallConfirmParams{Token: strings.TrimSpace(params.Token)}
out := Assessment{Locks: []string{FirewallLockName}, DisplayName: "确认防火墙改动",
    Normalized: normalized, IdentityFingerprint: "firewall:ufw"}
```

三个函数都补上（`apply` 那条用 `params` 本身）。有记录时后面仍覆盖成 `rec.Token`。

**回归测试**（`internal/network/firewallapply_test.go`）：

- `TestFirewallAssessRefusalsKeepNormalizedParams`：无 pending × confirm/rollback + 定义不合法 × apply，三条拒绝分支都断言 `Normalized != nil`
- `TestFirewallTokenMismatchKeepsNormalizedParams`：有记录但 token 不符

两层都钉：既钉"这条分支不再 nil"，也在契约 §13.7 加了「拒绝分支必须带规范化参数」一节。

### 这不是孤例

同一形状的坑 P07.3 撞过一次（Q190：设备已经消失时，判决正确给出 `DEVICE_NOT_FOUND`，
但"消失的设备"没有东西可以绑定 → 500）。那次的修法是「只在 `Allowed=true` 时要求绑定」。
这次不同：绑定那边放行了，**规范化参数**这边没跟上。两次都是"拒绝路径上的元数据"。

---

## 5. 另外五条是验收脚本自己的错（记在这里，因为方法错误比产品错误更容易复发）

第一轮 13 条红里，除了上面 8 条，其余 5 条**不是产品的问题**。逐条写清楚，因为它们各自代表
一类会让验收说谎的错误：

### 5.1 `grep -qi active` 会匹配 `inactive`（假红，且方向危险）

`ufw status` 未启用时第一行是 `Status: inactive`，而 `inactive` 里含 `active` 这个词。

```sh
# 错的
ufw_active() { "$UFW" status | head -n1 | grep -qi active && echo yes || echo no; }
```

后果：第一轮环境检查直接把"起点干净"判成"起点 ufw 是 active"并退出。**更要紧的是看门狗用
的是同一个正则**——它一旦跑起来就会认为"ufw 启用但 22 没有放行"，然后真的去
`ufw --force disable` 并加一条 22/tcp 规则：既污染现场，又留下 `watchdog-fired-*` 让真正的
断言红得说不清原因。改成 `grep -qiE '^Status: active'`（两处）。

### 5.2 默认策略的判据在 inactive 时是空的（假绿，永远不会红）

原来用 `ufw status verbose | grep '^Default:'` 抓基线，而 **inactive 时 `ufw status verbose`
只打印 `Status: inactive`，不打印 `Default:` 行**（VM 上实测）。于是"改前"和"改后"两头都是
空串，`expect_eq "" ""` 永真——一条**永远不会红**的断言，比没有断言更糟。

改成读 `/etc/default/ufw` 的 `DEFAULT_INPUT/OUTPUT/FORWARD_POLICY`：启用与否都读得到，而且它
才是 ufw 重启后的生效来源。另加一条 `expect_ne "起点读得到默认策略" ""` 防"空比空"。

### 5.3 `write_watchdog` 只定义、从未被调用（假绿）

救命绳脚本 `$WORK/watchdog.sh` 根本不存在，两条看门狗到期时只会各自打一行
`/bin/sh: 0: cannot open …/watchdog.sh: No such file` 然后退出。而"看门狗没有动手"这条断言
查的是 `watchdog-fired-*` —— 不存在，于是 **PASS**。失败的方向恰恰是最坏的那个：真把 22
挡了也没人救，而验收说"没动手"。

修：`arm_watchdogs` 第一行调 `write_watchdog`；并且两条看门狗**缺日志一律 FAIL**
（不给"收尾时它还没到点"留后门——到那一行时武装时刻已过 232 秒）。

### 5.4 `OnActiveSec=120s` 去读了一个不存在的文件（假红）

计时的单元是**每 token 一个** `upanel-fwrollback@<token>.timer`，`OnActiveSec` 写在它里面；
`@.timer` 模板根本不存在（模板只有 `@.service` 那一个）。`cat` 一个不存在的文件得到空串，
`expect_contains` 自然红。

**这条值得单独记**：它说明"红"也要核实原因，不能见红就改产品。我当时先怀疑产品的 timer
模板，去读了 `firewallTimerUnit()` 才发现是逐 token 生成的。

### 5.5 坏定义的断言方向反了（假红）

我以为坏定义会走到"预检 200 + `FIREWALL_DEFINITION_INVALID`"，实际在**代理的参数解码**
就被拒：

```json
{"error":{"code":"VALIDATION_ERROR","message":"agent: 参数不合法: 第 1 条规则：network: 防火墙定义不合法: port 必须在 1—65535 之间，收到 0"}}
```

`newFirewallApplyAction` 的 `Decode` 里先跑一遍 `ValidateFirewallDefinition`，形状不合法在
那一层返回 400 —— 这比阻塞项**更早、更便宜**，而且不需要人再读一份预检报告，是更好的设计。
所以产品不改，改断言：四种坏定义都断言 400 + `VALIDATION_ERROR`，并核对话术（"port 必须在
1—65535 之间"、"allow/deny/reject/limit"）。

顺带一个结论写进契约：`FIREWALL_DEFINITION_INVALID` 这条阻塞项在真机上**不可达**，它是
纵深防御（`Assess` 独立再校验一次，只在"解码放行而预检拒绝"时出现）。已同步到
`docs/CONTRACTS.md` §13.7 的阻塞项表。

### 5.6 还有一条我自己差点漏掉的：`rules` 空时是 `null`

读代码时发现的（不是真机抓的）：`Current()` 里 `Rules: []ManagedRule{}` 的初始化被
`out.Rules = snap.managed` 覆盖，而 `readSnapshot` 在 ufw 读不到时提前 return、`snap.managed`
是 nil → `rules` 序列化成 `null`，违反自己 OpenAPI 写的 `array`，前端 `config.rules.map`
直接崩——而"读不到 ufw"恰恰是最需要看到这张卡片的时候。

改成 `append(out.Rules, snap.managed...)`（空现场恒为 `[]`），前端两处也兜了 `?? []`。
指纹算的是 `snap.managed`，不受影响。

---

## 6. 已知限制（写在这里，不假装已覆盖）

1. **`OnActiveSec` 是相对计时**，会被并发的 `daemon-reload` 顶后。`netrollback` 那边
   （Q457）是同一个限制。防火墙这一轮的 60 秒断言就是在盯它：真顶后了会表现为"提前退回去"。
2. **回滚只恢复"面板管理的规则集 + 启用状态"**。管理员在这一轮里手写的规则不会被退回——
   它们不属于这次改动的"之前"。这是设计（Q459），但在"改完发现手写规则也挡人"的场景下
   不救场。
3. **管理端口认不出来就停下**（`FIREWALL_MANAGEMENT_UNKNOWN`）。SSH 优先按进程名 `sshd` 认，
   认不出来退回 22/tcp。非标端口的 SSH 如果既没有 `sshd` 进程名、又没有面板监听，会被当成
   "管理端口认不全"而不是"22 就够"。
4. **一次只允许一条未确认的改动**。两条叠加会让"退回哪一份"变得没有答案。

---

## 7. NOT RUN（按设计）

| 项 | 为什么没跑 | 由什么覆盖 |
|---|---|---|
| 浏览器里的渲染与交互 | VM 内没有 Playwright（Q21） | `web/src/components/firewall-write.test.ts`（9 条 SSR 渲染断言）+ 宿主 `web-smoke` |
| 回滚自己失败（记录 `state=failed`） | 真机上造它等于把机器弄坏在两条看门狗之外，风险不对等 | 单测 `TestFirewallApplyRollsBackWhenVerifyMismatch` 覆盖判决形状 |
| ufw 根本没装的机器（`FIREWALL_UNSUPPORTED` + `editable=false`） | 卸载 ufw 会连 VM 自己的转发规则一起带走，不是一条能安全回头的用例 | 单测覆盖判决形状；阶段 3 断言了这台机器 `editable=true` 并打印 reasons |
| IPv6 规则、应用配置（`ufw allow 'Nginx Full'`）、端口区间、按接口限定 | 这一版**明确不支持** | 形状由单测覆盖；真机上只验证了"手写 v4 规则不进面板列表、也不被改"（阶段 6 的 9000） |

**没有一项 NOT RUN 是"跑了但没过"。** 153 条断言里 0 条失败。

---

## 8. 复现

```sh
# 1) 宿主交叉编译（不要把 Go 工具链搬进 VM）
cd /h/Code/uPanel
for p in upanel-agent upanel-web upanelctl; do
  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -buildvcs=false -trimpath \
    -o ".build/bin-linux/$p" "./cmd/$p"
done

# 2) 脚本与 JSON 助手放同一目录（脚本按 $0 找助手）
scp -i /h/Code/vms/upanel-2204/id_ed25519 -P 2223 \
  tests/integration/p14-3-acceptance.sh tests/integration/json_helper.py \
  .build/bin-linux/* upanel@127.0.0.1:/tmp/p143/
#    二进制放 /tmp/upanel-bin（root 拥有，覆盖前先 sudo rm）

# 3) 清场：起点必须 inactive、且没有 `upanel:` 规则。脚本自己会判，判不过就以 2 退出
#    并把清理命令打出来（它不替操作者动 ufw —— 那是上一轮的现场）。
sudo ufw --force disable && sudo ufw --force reset

# 4) 起跑。**必须 detached**：阶段 7 停掉 Web 与代理之后要等满 120 秒窗口，
#    前台跑法会在 ssh 抖动时被 SIGPIPE 杀死，而"它是不是自己退回来的"正是被验的那件事。
sudo BIN_DIR=/tmp/upanel-bin setsid nohup sh /tmp/p143/p14-3-acceptance.sh \
  </dev/null >/var/tmp/p143-run.log 2>&1 &

# 5) 轮询结论
until sudo test -f /var/tmp/upanel-accept-p14-3/RESULT; do sleep 10; done
sudo cat /var/tmp/upanel-accept-p14-3/RESULT
```

证据：`logs/p143-2204-run3.log`（完整 153 条）、`logs/RESULT.txt`、`logs/watchdog-*.log`、
`logs/host-check.txt`（宿主全量检查）、`facts/`（50 个现场文件：`env.txt`、`journal-fwrollback.txt`、
`show-added-*.txt`、各阶段 `config-*.json` 与 `preflight-*.json`）。

**三次轮次的账**：第一轮 `132/13/4`（死在 §5.1）、第二轮 `132/13/4`（§4 的缺陷暴露，8 条红）、
第三轮 `153/0/4`。第一、二轮的日志没有留档——它们跑在缺陷被定位之前，留着的只有 `facts/`
里那些 `preflight-confirm-nothing.json` 形式的 500 响应体，以及本文 §4 引用的原文。
