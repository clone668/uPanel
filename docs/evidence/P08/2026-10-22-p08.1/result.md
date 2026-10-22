# P08.1 验收结果 — Samba 能力检测、受管理账号、密码秘密通道、POSIX ACL、共享定义与配置版本

证据类型：**Linux VM integration**（一次性 Ubuntu 24.04.5 虚拟机，root 起代理、uid 999 起 Web）
执行时间：2026-10-22（UTC）
脚本：`tests/integration/p08-1-share-acceptance.sh`
环境：见 `env.txt`；宿主检查见 `check.txt`；单元测试见 `native-tests.txt`

---

## 1. 结论

| 项 | 数据 |
|---|---|
| 最终一遍（第五遍，`run.log`）| **PASS=382 / FAIL=0 / NOT RUN=4**（退出码 0）|
| 目录、ACL、账号、秘密通道 | 都在真机上真的做出来了，并用独立命令（`getfacl`、`getent`、`pdbedit`、`sha256sum`）核验 |
| `share.apply` 的 outcome | `accepted`（不是 `verified`）：`observed.samba_config="not_written"`、`served=false`、恒带 `SHARE_CONFIG_NOT_APPLIED` 告警 |
| smb.conf | 整轮结束时**逐字节未变**（`sha256` 比对），本轮确实一个字节都没写 |
| 明文扫描 | Web 日志、代理日志、两个数据库（含 WAL）、全部 `/proc/*/cmdline` 与 `environ`：**0 命中** |

这一遍之所以要跑到第五遍，是因为前几遍在真机上抓出了**四个产品缺陷**（Q214/Q220 与 Q221/Q223 两个同类
的“观察失败被当成操作失败”）与**一个 P05.1 的性能问题**（Q222），加上验收脚本自身的五处缺陷。每一条都在下面
写清了「怎么被发现 → 怎么改 → 用什么证明改对了」。

第五遍是**带着 Q223 的修正**跑的：预检里那次 `getent` 超时降级成 `ACCOUNT_LOOKUP_TIMEOUT` 告警，
执行前复核改用 20 秒自己的预算。这一遍里 `getent` 恰好都在预算内（`warnings:0`），
所以**告警分支本身没有被真机触发**——它由单测 `TestAssessAccountApplyLookupTimeoutIsWarning`
与 `TestCreateAccountRefusesExistingUserOnExit9` 覆盖。真机证明的是另一半：两套预算没有破坏任何一条
正常路径，上一遍死掉的那两处（`提交（acct-create-ro）` 与三次删除预检）这一遍全部通过。

## 2. 范围（做了什么、没做什么）

已做（STAGES §10 第 1 条的全部内容）：

- **能力检测** `internal/shares/capability.go`：`smbd`/`smbpasswd`/`pdbedit`/`setfacl`/`getfacl` 逐个探测
  （固定候选路径、不猜、不落 PATH），30 秒 TTL；缺哪个工具就报哪个阻塞码，`available` 只在这两件事都成立时
  为真；`config_write` 恒 `unsupported`（本阶段不写配置）。
- **受管理账号** `internal/shares/account.go` + `write.go`：`useradd --system --no-create-home
  --home-dir /nonexistent --shell /usr/sbin/nologin --user-group` + `smbpasswd -s -a`（明文只走 stdin），
  三条独立读回（`getent passwd` / `passwd -S` / `pdbedit -Lv`）。
- **密码秘密通道** `POST /api/v1/secrets`：admin + CSRF + 再认证，代理侧 AES-256-GCM 加密，
  返回 `sec_<32hex>`；用途枚举只在代理侧（Q216）。
- **目录与 POSIX ACL** `internal/shares/acl.go`：`setfacl --set` 整份替换（Q211），`getfacl -cp` 读回逐条
  比对并额外核验 `mask`；撤销也是整份替换。
- **共享定义与配置版本** `internal/shares/definition.go`：确定性渲染（账号按名字排序、键序固定），
  `config_hash = sha256(渲染结果)`；`directory_prepared` / `config_applied` / `config_hash` 三个字段回答三个问题。
- 存储：`migrations/agentdb/0005_shares.sql`（`managed_accounts`、`shares`），Web 只读视图（Q217）。

没做（留给后续阶段，本轮用断言反过来证明没做）：

- 不创建 `/etc/samba/upanel.d`、不往 `smb.conf` 加 include、不跑 `testparm`、不重载 smbd（P08.2）。
- 不提供 `GET /shares/:id/connections`（P08.3）。
- 共享此刻**不对外提供任何访问**——这不是“还没测”，是这一阶段的产品事实。

## 3. 逐节覆盖（第五遍真机运行，`run.log`）

| 节 | PASS | FAIL |
|---|---|---|
| 0. 环境与安全守卫 | 33 | 0 |
| 1. 配置与启动（代理 root、Web 非 root） | 18 | 0 |
| 2. 能力检测：面板说的必须与系统上的事实一致 | 23 | 0 |
| 3. 夹具：一块空盘走产品的 GPT → 分区 → ext4 → 导入 → 挂载；另一块只导入 | 38 | 0 |
| 4. 秘密通道：只进不出，明文只经过一次 | 24 | 0 |
| 5. 受管理账号：真的建出来，并用 `getent` / `pdbedit` 两条独立读回 | 79 | 0 |
| 6. 共享定义：目录与 POSIX ACL 真的做出来，但结果只能是 accepted | 51 | 0 |
| 7. 页面依赖的载荷：列表 / 详情 / `config_applied` 恒为 false | 19 | 0 |
| 8. 拒绝路径：该拒的拒，理由写给页面看 | 44 | 0 |
| 9. 禁用 / 改回 / 删除：ACL 撤销、目录保留、记录消失 | 50 | 0 |
| 10. 收尾与不变量 | 3 | 0 |
| **合计** | **382** | **0** |

第 10 节的三条是整轮的不变量，单独列出来因为它最容易被“过程中都通过”掩盖：代理停掉后 socket 已清理、
`smb.conf` 从第一字节到最后一个字节没变（`sha256` 比对首末两份 `logs/smb-conf-{initial,final}.txt`）、
整轮日志与两个数据库（含 WAL）里**零个**明文命中。

NOT RUN 四条（原因写在 `run.log` 末尾，不假装通过）：SMB 客户端连入、写配置/testparm/重载/连接状态、
浏览器真实渲染、能力 blocked 分支。

## 4. 真机上抓出的产品缺陷

| 编号 | 症状 | 根因 | 修法与证据 |
|---|---|---|---|
| Q214 | 新建受管理账号的预检**永远 500**（`缺少目标状态版本`）| `assessAccountApply` 只在更新分支设 `StateVersion`；新建分支没有任何东西可绑，`allowed=true` 却没有版本，被 `validatePlanResult` 判成实现错误。新建共享同洞 | 新建对象的状态版本 = `absent:<名字>`（可复现、执行端重新判决得到同一串）。回归测试 `TestAssessCreateBindsStateVersion`（判决→用判决自己的版本执行→guard 必须放行）、`TestAssessAbsentVersionChangesOnceObjectExists` |
| Q215 | 预检偶发 `503 代理不可用` | 预检里 `getent` 的时限是 30 秒，比 Web 整条链路（`agentclient.DefaultTimeout` 10 秒）还长：NSS 一卡，用户看到的是“代理不可用”，真正的原因连日志都写不完整 | 预检里账号查询单独 `accountLookupTimeout = 3s`；规则：**预检里每一步外部命令的时限都必须显著小于调用方的整体预算** |
| Q220 | 删除账号的预检**永远 500**（`缺少目标身份指纹`）| `assessAccountDelete` 设了 `TargetID`/`DisplayName`/`StateVersion`，漏了 `IdentityFingerprint` | `out.IdentityFingerprint = rec.Name`；回归测试 `TestAssessAccountDeleteBindsTarget`。**这是 Q214/Q190 的第三次同类**：校验器要的东西，每条 allowed 的路径都得给全——三次分别漏在拒绝路径、创建路径、删除路径。本轮顺带把四个判决函数的 allowed 路径全查了一遍（见 §4.1） |
| Q221 | 一次观察超时让整条操作变 500；`Bad_Name` 这条本该必拒的预检也 500 | ① `getent` 超时直接冒成错误；② 名字已经不合法时仍然去问系统有没有同名账号 | ① 超时重试一次，总预算仍 3 秒（拆成两次 1.5 秒），退出码类失败不重试；② `nameValid` 短路——**不要为一个不会改变判决的观察付出等待**。回归测试 `TestSystemUserExistsRetriesOnceOnTimeout`、`TestSystemUserExistsDoesNotRetryOnRealFailure`、`TestAssessAccountApplyInvalidNameSkipsSystemLookup` |
| Q222 | **根因已定论，未根治**（属 P05.1） | 见 §9：隔离实验证明就是服务采集器 | 见 §9 的处置与后续 |
| Q223 | 第四遍仍然倒在同一个地方：`提交（acct-create-ro）` 回 500，代理日志`shares: 查询系统账号失败: syscmd: 命令超出运行时限（/usr/bin/getent，上限 1.5s）` | 同一个问题（系统里有没有这个账号）在**预检**与**执行前复核**两处被写成了同一个预算，而两处的等待能力根本不同：预检在 Web 等着的 10 秒链路上，执行在任务自己的预算里 | ① 预算按阶段分开：预检 3 秒（2×1.5 秒，链路不变），执行 20 秒（2×10 秒）；② 预检查不出结果不再当错误——`systemUserExists` 用尽预算返回包裹 `syscmd.ErrTimeout` 的错误，判决层翻成告警 `ACCOUNT_LOOKUP_TIMEOUT`（新码，CONTRACTS §7.1 已登记），`allowed` 由其它事实决定；③ 安全不靠这条预检：执行阶段长预算复核 + `createAccount` 认 `useradd` 退出码 9（回 409 `DEPENDENCY_BLOCKED` 并点名 `ACCOUNT_NOT_MANAGED`，绝不继续设 Samba 密码）。回归测试 `TestAssessAccountApplyLookupTimeoutIsWarning`、`TestAccountApplyExecutionLooksUpWithItsOwnBudget`、`TestCreateAccountRefusesExistingUserOnExit9` |

### 4.1 allowed 路径的绑定覆盖检查（本轮新增的系统性核对）

四个判决函数里每一条 `Allowed = true` 的路径都必须给出 `IdentityFingerprint` 与 `StateVersion`：

| 判决函数 | 指纹 | 状态版本 |
|---|---|---|
| `assessShareApply` | `dctx.VolumeUUID`（无条件，在 `Allowed` 之前）| 更新 = `rec.Version`；新建 = `absent:<名字>` |
| `assessShareRef`（disable/delete）| `rec.ConfigHash` | `rec.Version` |
| `assessAccountApply` | `p.Name`（无条件）| 更新 = `rec.Version`；新建 = `absent:<名字>` |
| `assessAccountDelete` | `rec.Name`（本轮补）| `rec.Version` |

`docs/CONTRACTS.md` §7.1 补上了这条规则（原文只有存储动作那一段写过），并写明“缺绑定是**实现错误**，
不是可以放宽的检查”。

## 5. 验收脚本自身的缺陷（都会让脚本骗自己）

| # | 缺陷 | 它当时说了什么谎 | 改法 |
|---|---|---|---|
| 1 | 预检失败后没有清空 `job.json` | 预检 503 / 提交 500 之后，后面每一条 job 断言读到的都是**上一轮**的作业，于是“结果是 verified”“带 verified_at”一路通过，失败被推到 §6 才暴露 | `run_action` 在预检失败与提交失败两条路径上都清空 `job.json` 并把状态码写进 `JOB_STATE`；提交失败另外单独记一条 `FAIL`，带上响应体前 300 字节 |
| 2 | 请求体拼接丢了名字后面的闭合引号 | 服务端只回一句 `400 请求体不是合法的 JSON`，真正的原因得靠反推 | 改正拼接；`preflight()` 里加一道自检：请求体解析不了就把坏掉的原文喊到日志 |
| 3 | `$( )` 吃掉渲染结果结尾的换行后再算 sha256 | `config_hash 就是 config_preview 的 sha256` 这条永远红，而产品是对的（`8e09e9…` vs `3258aa…`） | 直接在 Python 里读 JSON 的那个字段再哈希，边界与产品看到的一致 |
| 4 | 空串里“找不到 D”被当成通过 | `pdbedit` 读失败时 `Account Flags` 是空的，`expect_not_contains "D" ""` 照样绿——启用的证据是一句空话 | 先断言 `Account Flags` 非空，再断言没有 `D`；同理给 `note.txt` 加了“摘要是 64 位十六进制”的前置断言（空串与空串相等） |
| 5 | 拿机器上现成的 `upanel` 当“非面板建的账号”夹具 | `upanel*` 是保留名，判决正确地先报 `ACCOUNT_NAME_INVALID`，于是 `ACCOUNT_NOT_MANAGED` 那条断言红得莫名其妙 | 夹具自己造：临时 `useradd` 一个名字合法的系统账号 → 断言 → 删掉 → 核对它真的没了 |

第 5 条是第三遍唯一剩下的失败项，第四遍就是带着这个修正跑的。

## 6. 实际执行的检查

| 检查 | 结果 | 证据 |
|---|---|---|
| 宿主 `sh scripts/check.sh`（Node v22.23.3）| **PASS 13 / FAIL 0 / NOT RUN 0** | `check.txt` |
| 宿主 `go test ./... -count=1` | 见 `native-tests.txt` | `native-tests.txt` |
| `sh -n` 验收脚本 | 通过 | 见本轮提交 |
| 真机验收（一次性 VM）| 见 §1 | `run.log`、`payloads/`、`logs/` |

未执行（NOT RUN）的检查与原因：

- **从另一台机器用 SMB 客户端连上来**：P08.1 不写配置、不重载服务，共享此刻不对外提供服务；
  SMB 客户端 VM（`P00-VM-D`）尚未建立。这是 P08.2/P08.3 的验收项。
- **写配置 / `testparm` / 原子替换 / 重载 / 连接状态**：属于 P08.2 与 P08.3。
- **浏览器里的真实渲染**：本脚本验的是载荷；DOM 由 Web 侧 SSR 回放测试覆盖，浏览器端到端由 smoke 覆盖。
- **能力 `blocked` 分支（缺包环境）**：这台机器装了 samba，走的是 `available`；`blocked` 由
  `internal/shares` 的注入式单测覆盖（不靠卸载真机的包来造）。

## 7. 未验证 / 需要后续阶段才能验证

1. 共享对客户端**实际可用**（`testparm` 通过、`smbd` 重载后能连上、ACL 在真实 SMB 会话里生效）——P08.2/P08.3。
2. `AllowedCIDRs` 在真实连接上的效果（`hosts allow` 只是渲染出来的文本，还没有让 smbd 读过）。
3. 并发两人同时应用同一共享的串行化，在真机上只验了“锁名里有共享名与卷”，没有造真的并发。

## 8. 环境备注

- 虚拟机时钟是 UTC 2026-10-22（与宿主一致）；`smbd` 本来就装着并开着（夹具），P08.1 不碰它。
- 虚拟机里有三块闲置盘（sdb 8G / sdc 4G / sdd 4G），脚本只接受显式给出的
  `SPARE_DISKS="sdb sdc"`，不会因为“看到一块空盘”就去动它；系统盘按 `ROOT_DISK` 校验后拒绝。
- 同一台机器上 `systemctl show`（200 个单元、13 个属性）一次要 **4.49 秒墙钟 / 2.43 秒用户态 CPU**，
  `list-units --all` 要枚举 188 个已装载单元、`list-unit-files` 257 个单元文件，服务采集每 **10 秒** 一轮
  （Q222）。一轮采集在这个 2 vCPU 的机器上要吃掉大约三分之一个核，而且是一段一段的突发。
  这就是 `getent` 偶尔跨秒的外部原因（§9 的隔离实验）。**P08.1 的两套预算只是让它不至于变成 500 或误杀，
  不是根因修复。**

## 9. 已知问题（Q222，未根治）

**Q222 — P05.1 的服务采集器在 2 vCPU 机器上会把观察类命令挤到秒级。**

### 9.1 隔离实验（本案的证据，不是推测）

同一台一次性 VM、同一份代理配置，只切换“代理跑不跑”，`getent passwd <名字>` 各采样一轮
（采样脚本 `/tmp/probe2.sh`，间隔 0.2 秒，记录每次的毫秒数）：

| 阶段 | 采样 | 最大 | 平均 | >1 秒的次数 |
|---|---|---|---|---|
| A. 代理**没跑** | 30 | 29 ms | 25 ms | 0 |
| B. 代理跑着（采集器每 10 秒一轮） | 40 | **2894 ms** | 106 ms | 1 |
| C. 代理停掉之后 | 10 | 29 ms | 23 ms | 0 |

A 与 C 是同一条基线（23—29 ms），B 里出现 2.9 秒的单次延迟，均值涨 4 倍。**这是相关性 + 因果隔离**：
唯一改变的自变量是代理进程，而代理里唯一周期性密集调外部命令的就是服务采集器。

### 9.2 采集器到底花多少

| 测量（同一台 VM）| 数值 |
|---|---|
| `systemctl show -- <200 个单元>`（13 个属性）| 墙钟 **4.49 s**，用户态 CPU **2.43 s** |
| `systemctl show -- <188 个单元>`（只取 `Id`）| **4.01 s** —— 成本按**单元数**算，和属性多少几乎无关 |
| `systemctl show -- '*'`（加 `--all`，483 个单元）| **8.62 s** |
| `systemctl list-units --all --output=json` | 0.08 s（但只有 8 个字段，`MainPID`/`MemoryCurrent`/`NRestarts`/`ActiveEnterTimestamp`/`FragmentPath` 都没有）|
| 采集范围 | `list-unit-files --type=service` 257 + `list-units --all` 188，按 200 个一组分块 |
| 采集间隔 | `DefaultInterval = 10s`（`internal/services/types.go`；**CONTRACTS 第 690 行把 10000 ms 写成了契约**）|

也就是说：每 10 秒有一轮约 6 秒墙钟 / 3 秒 CPU 的突发，占掉这台 2 vCPU 机器大约三分之一个核。
`systemctl show` 的成本是**每个单元一次 D-Bus 往返**（~21 ms），属 systemd 本身的形态，
在 agent 这一侧没有便宜的打法——`systemctl show` 的批量模式（`'*'`、`--all '*'`）反而更慢，
唯一快的 `list-units --output=json` 缺 P05 契约要求的字段。

### 9.3 本轮怎么处置

- **P08.1 侧已经不会因为它出错**：预检那条 3 秒预算的观察超时降级成 `ACCOUNT_LOOKUP_TIMEOUT` 告警，
  执行前复核用 20 秒自己的预算（Q223）。第四遍真机验收 `FAIL=0` 是这个结论的证据。
- **采集器本身没有改**：它属于 P05.1，改调度（降频）要动 CONTRACTS 第 690 行的 10 秒契约，
  改采集方式（增量/只采已装载单元）要重做 P05.1 的 DTO 与验收。这两件事都不该在 P08 里顺手做。
- **它不再挡 P08.2**：P08.2 写配置、跑 `testparm`、重载 smbd，用的都是任务预算里的 30 秒级命令，
  没有一条压在 Web 的 10 秒链路上。真正会持续被它影响的，是任何“在预检里调用外部命令”的新动作——
  规则已经写进 CONTRACTS §7.1：**预检里的每一步外部命令，时限都必须显著小于调用方的整体预算，
  而且超时要能降级成一句人话，不能变成 500**。
- **根治的候选方案**（留给 P05 的性能补强，需先看契约能不能松）：把 `systemctl show` 换成
  `list-units --output=json` + `list-unit-files` 的合并，只对**详情页与预检**现场读那几个贵字段；
  或把采集间隔做成随负载自适应（要有契约改动与 P05 回归证据）。

## 10. 复现

```sh
# 宿主：静态检查与单元测试
PATH="/c/Users/huxi/AppData/Local/nvm/v22.23.3:$PATH" sh scripts/check.sh
go test ./... -count=1

# 一次性虚拟机（Ubuntu 24.04.5）：真机验收
scp -i H:/Code/vms/upanel-2404/id_ed25519 -P 2222 \
  tests/integration/p08-1-share-acceptance.sh upanel@127.0.0.1:/var/tmp/
ssh -i H:/Code/vms/upanel-2404/id_ed25519 -p 2222 upanel@127.0.0.1 \
  'sudo BIN_DIR=/var/tmp/upanel-bin SPARE_DISKS="sdb sdc" sh /var/tmp/p08-1-share-acceptance.sh'
```

代理与 Web 二进制在宿主交叉编译（`GOOS=linux GOARCH=amd64 go build ./cmd/...`），拷贝到
`/var/tmp/upanel-bin` 执行；虚拟机里没有 Go 工具链。
