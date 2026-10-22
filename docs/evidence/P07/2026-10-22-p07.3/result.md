# P07.3 验收结果 — ext4/XFS 格式化、标签校验、确认短语、禁止取消、执行前二次身份检查

- 日期：2026-10-22
- 范围：`docs/STAGES.md` §9 P07.3 全句——“ext4/XFS 格式化、标签校验、确认短语、禁止取消、
  执行前二次身份检查；测试错误退出和进程中断。”
- 结论：**PASS 204 / FAIL 0 / NOT RUN 1**（真机 Ubuntu 24.04.5，三块一次性空盘）。
  唯一 NOT RUN 是“内核与盘上条目不一致”那一路，属真实竞态，有单测覆盖（见 §8）。
- 验收脚本：`tests/integration/p07-3-storage-format-acceptance.sh`（1296 行，
  sha256 `3d70edcc6e5671df354c4cfda0878622a62c851287bf34c7201c9bd10f13b1e2`）
- 运行日志：`run.log`（第 9 轮，也就是上面这个 hash 跑出来的那一轮）

## 1. 这一轮交付了什么

格式化动作接上执行器：`storage.filesystem.format` 从“只登记 + 预检”变成真的写盘。写盘仍然只有
固定模板一条路——命令行里只出现**已校验的文件系统名**（`ext4` / `xfs`）与**已校验的设备路径**，
卷标经第三道闸（`formatCommand` 的 `ErrUnsafeWrite`）确认只能进 `-L` 的位置。

| 文件 | 改动 |
|---|---|
| `internal/storage/format.go`（新增，448 行） | 格式化执行器：`executeFormat`（再读设备 → 比对预检绑定的身份与状态版本 → 拼固定模板 → 经 `syscmd` 执行 → 探测超级块 → 两源核验 → 出 `WriteReport`）；`compareFormatIdentity`；`formatObservedState`；`signatureKindFor`；进程中断的 `interruptedWrite`/`interruptedRun` |
| `internal/storage/fsprobe.go`（新增） | 直接读超级块做独立核验：ext4 在偏移 1024（`0x438` 的 `53 ef` 魔数、`s_uuid`、`s_volume_name`），XFS 在偏移 0（`XFSB`、`sb_uuid`、`sb_fname`）。这是“退出码 0 不算验证”的落点 |
| `internal/storage/syscmd_adapter.go`、`write.go`（改） | 把格式化接到写原语上；写前复核与写后核验共用一条错误出口 |
| `internal/agent/storageaction.go`、`registry.go`、`cmd/upanel-agent/main.go`（改） | 格式化登记执行器；`Plan` 里补标签上限判定（`storage.ValidateLabel`） |
| `internal/agent/preflight.go`（改） | 目标绑定只在 `allowed=true` 时是硬性要求（Q190，见 §6） |
| `internal/storage/assess.go`（改） | 拒绝路径的注释写明“设备不在了就没有身份可绑”，并断言 `display_name` 仍保留（Q190） |
| `docs/CONTRACTS.md` §4.1、§6.4 | 新增两条边界：**被拒绝的预检没有目标可绑定**（Q190）；**快照版本只覆盖结构，不覆盖未挂载分区的签名**（Q189） |
| `api/openapi.yaml` | `PreflightTarget.identity_fingerprint` 的说明补上 `allowed=false` 时允许为空 |
| `tests/integration/p07-3-storage-format-acceptance.sh`（新增，14 节） | 本轮真机验收 |
| `tests/integration/p07-1-storage-guard-acceptance.sh`（改） | P07.3 起格式化也有执行器，“过门之后必然 501”这种现场在真机上不存在了，改成显式 NOT RUN + 一条跨阶段不变量（四个存储动作都已可执行） |
| `tests/integration/run-all.sh`（改） | 补 p07-3，并写明三个脚本的先后（都要写盘，必须串行） |
| `docs/DECISIONS.md` | Q184（ext4 17 字节标签 rc=0 但被静默截断）、Q185（从 `/proc` 冻住正在跑的 mkfs）、Q186（第二道门比的是采集快照版本）、Q187（标签超限是 400 不是阻塞）、Q188（先查再认证再比短语）、Q189（快照版本只覆盖结构）、Q190（不存在设备的预检曾经是 500） |
| `internal/storage/format_test.go`、`fsprobe_test.go`、`assess_test.go`、`internal/agent/storageaction_test.go`（新增/改） | 执行器与探测器的原生用例；Q190 的回归用例 |

## 2. 环境

见 `env.txt`。要点：Ubuntu 24.04.5 / 内核 6.8.0-147；mke2fs 1.47.0、mkfs.xfs 6.6.0、
util-linux 2.39.3、sgdisk 1.0.10；代理以 root 运行、**Web 以 uid 999 运行**；sda 系统盘
（UPANEL-SYS-40G）全程只出现在“被拒绝”的用例里，sdb/sdc/sdd 是一次性空盘。

## 3. 逐节结果（断言数）

| 节 | 覆盖 | PASS | FAIL | NOT RUN |
|---|---|---|---|---|
| 0 | 环境与安全守卫（三块一次性盘都与根不同源、代理硬编码的 `mkfs.ext4`/`mkfs.xfs` 真在、Web 非 root） | 27 | 0 | 0 |
| 1 | 配置与启动 | 8 | 0 | 0 |
| 2 | 能力：格式化现在是可执行动作 | 4 | 0 | 0 |
| 3 | 夹具：`sgdisk` 造 GPT + 三个分区（sdb1 1 GiB / sdc1 64 MiB / sdd1 2 GiB） | 10 | 0 | 0 |
| 4 | ext4 格式化：标签、两源核验、独立工具复核 | 6 | 0 | 0 |
| 5 | 确认短语：错的短语被拒，且这份预检就此作废 | 37 | 0 | 0 |
| 6 | 同一分区改成 XFS：UUID 必须换、`-f` 在 argv 里 | 18 | 0 | 0 |
| 7 | 标签上限：超限在**预检之前**就被拒（400），既不落任务也不碰盘 | 12 | 0 | 0 |
| 8 | 禁止取消：mkfs 跑着时取消 → 409，任务不被标成 canceled | 14 | 0 | 0 |
| 9 | 错误退出：预检允许、mkfs 以非 0 结束（64 MiB 上做 xfs） | 12 | 0 | 0 |
| 10 | 执行前二次身份检查：预检之后有人动了盘 | 25 | 0 | 1 |
| 11 | 进程中断：mkfs 跑着时代理被 SIGKILL | 12 | 0 | 0 |
| 12 | 回归与日志卫生 | 9 | 0 | 0 |
| 13 | 收尾：擦掉夹具 + 不存在的设备的预检 | 10 | 0 | 0 |
| **合计** | | **204** | **0** | **1** |

## 4. 关键断言与读数

**确认短语（§5）**：错短语是 400 `CONFIRMATION_MISMATCH`，而且**这份预检就此作废**——用对的短语
再提交一次仍然被拒（`PREFLIGHT_TRANSFERRED`/作废理由），盘上字节一个都没动。也就是说“猜短语”
不是一个可以重试的接口，猜错一次就要重新预检。

**标签上限（§7）**：ext4 17 字节、xfs 13 字节在**预检之前**就是 400 `VALIDATION_ERROR`，
不落任务、不碰盘。真机实测的前提（`measurements.txt` §5/§6）：`mkfs.ext4 -L 17B` 会 rc=0 并把
标签**静默截断**成 16 字节，`mkfs.xfs -L 13B` 会 rc=1。所以这个上限不能靠 mkfs 自己把住。

**错误退出（§9）**：64 MiB 分区上做 xfs，预检允许（判决层只看最小分区与签名），mkfs 以 1 退出。
任务是 `failed` + `COMMAND_FAILED`，消息里同时有**退出码**、**mkfs 的真实输出**
（`larger than 300MB`）、**“盘上现在是”**（读回的实际状态）和“重新提交”的提醒；分区首 4 MiB
的哈希前后一致——命令失败不等于盘被写脏。

**禁止取消（§8）**：在 `/proc` 里盯代理的直接子进程，看到 `mkfs.ext4` 就 `SIGSTOP`（冻住时
`comm=mkfs.ext4`、`stat=T`，任务停在 `running/executing`）。此时 `POST /jobs/:id/cancel` 是
**409 `VALIDATION_ERROR`**，理由说明这个动作不能安全取消；任务既没有变成 `canceled`，
`cancellable` 也是 `false`。解冻之后它跑完并 `verified`——取消被拒没有破坏执行。

**进程中断（§11）**：同样冻住 `mkfs.xfs`，然后 `SIGKILL` 代理。重启后 reconcile 把遗留操作标成
`interrupted` + `OPERATION_INTERRUPTED`，消息说“实际状态未知”；**没有自动重跑**（任务数不变，
同一分区上没有第二条成功任务）。代理死了 mkfs 不会自己退出，脚本先收掉再重启——这一步在注释里
写明是测试的责任，不是产品的。

**执行前二次身份检查（§10）**：两道门，用法不同。

- 10a（版本门）：预检之后在**同一块盘上新建一个分区**（`sgdisk --new=2:0:+256M`）——目标分区的
  位置、身份、大小、文件系统全不变，变的只有盘的结构。快照版本随即从
  `sha256:2683d3fa…` 变成 `sha256:23d53538…`，提交得到 **409 `PREFLIGHT_STALE`**，
  目标分区首 4 MiB 哈希、ext4 签名、UUID 全部不变。
- 10b（身份门）：把分区删掉再在同一块盘的别处建一个**同号**分区（编号还是 `sdc1`，字节不是了），
  提交得到 **409 `DEVICE_IDENTITY_CHANGED`**（见 §8 说明为什么这一路只做了 NOT RUN）。

## 5. 为什么 10a 换成“结构变了”（Q189）

§10a 第一版是“预检之后把目标分区的签名 `wipefs` 掉”。第 4、5 两轮都失败：`wipefs -a` 之后
设备详情里的 `snapshot_version` **逐字节相同**（`sha256:576972147d41b6c5…`），于是预检仍然有效，
那次本该被拒的提交把盘格式化了。

原因在 `storage.scan`：`filesystem` 只给整盘、已挂载分区（以及有 by-uuid/by-label 链接的顶层设备）
填，**未挂载分区上的签名、UUID、卷标根本不进拓扑**，自然也不进 `snapshotVersion`。
这不是产品缺陷，是边界：第二道门的版本比较管“结构变了”，签名变化由写动作动手前的**设备直读**
承担（对 `format` 来说签名变了本来也不该拒绝——它的工作就是覆盖那个签名）。

所以验收改成制造结构变化，并加了两条硬断言：版本**必须**变、被拒之后字节/UUID/文件系统**必须**不动。
CONTRACTS §6.4 记下这条边界，按设备版本化列为后续改进项（会牵动 p04-2/p07-1 的快照语义）。

## 6. 真机抓出的产品缺陷（改产品，不改断言）

**Q190 — 对不存在的设备做预检回 500，本该是一条拒绝。**

第 7 轮跑完时，§13 那条“只作记录”的收尾探针打印出 `allowed=ERR:KeyError:'data'`。翻响应：

    {"error":{"code":"INTERNAL_ERROR","message":"服务内部错误"},...}

代理日志把原因写得很直白：`预检结果不合法: 缺少目标身份指纹：预检必须绑定一个可复核的目标`。

链路是：判决层**正确**地给出 `allowed=false` + `DEVICE_NOT_FOUND`（设备不在了，没有任何东西可以
绑定，指纹留空是对的），但 `validatePlanResult` **无条件**要求目标指纹与状态版本，于是把这条最普通
的拒绝判成了实现错误。这个洞从 P07.1 就在，只是前两轮的验收从没对未知设备做过预检（只做过
`GET /storage/devices/nosuchdisk` → 404）。

修法：**目标绑定只在 `allowed=true` 时是硬性要求**。绑定的唯一用途是让执行端复核“要动的还是预检时
那个东西”，而提交端对 `!allowed` 直接 409（`operations.go`），被拒的预检根本不可能被执行。
反向约束一条没放松：放行的预检仍必须同时有指纹与状态版本，`不允许执行却没有给出阻塞项` 仍报错。

补的测试：
- `internal/agent.TestStoragePreflightForMissingDeviceIsNot500`——走完整 HTTP 路径：201（Web 映射
  200）、`allowed=false`、`DEVICE_NOT_FOUND`、`display_name` 保留、`identity_fingerprint` 为空、
  预检落库（一条被拒记录）、提交仍 409。
- `internal/storage.TestAssessDeviceNotFound` 新增三条断言：不编造指纹 / 不允许执行 / 保留设备名。

验收侧把那条“只作记录”的探针**升级成断言**（不存在的设备预检不是 500、`DEVICE_NOT_FOUND`、
不编造指纹、仍说得清问的是谁）——这条就是该缺陷的回归锁。擦盘之后 `sdb1` 的现场改成纯记录项，
输出从 `ERR:KeyError` 变成 `allowed=False blockers=DEVICE_NOT_FOUND`。

## 7. 验收脚本自身的缺陷（记录，不掩盖）

1. **冻不住 mkfs（四种原因，逐条修）**。要测“取消被拒”与“进程被杀”，必须在 mkfs 跑着的那几十
   毫秒里下手，所以第一版用 shell 循环盯 `/proc`——一轮 20—40 ms，而 `mkfs.ext4` 1 GiB 只要
   43—53 ms，整轮都能漏（第 4 轮）。
2. **换成 Python 之后仍然漏**：`cmd &` 的 `$!` 只是外层子 shell，`stop_freeze` 杀掉它之后 Python
   变成孤儿继续轮询，第 6 轮它把 §9 那次 `mkfs.xfs` 冻住了，任务再也结束不了。改成**按 Python
   自己报的 pid 杀**，并用 `*.watch` 文件做启动握手（冷启动几十毫秒，不握手就会在 import 期间
   错过窗口）。
3. **第 12 轮（run8）又漏一次，这次原因不同**：`seen` 集合让每个 pid 只看一次，而 fork 与 exec
   之间 `comm` 还是父进程的名字——0.5 ms 一次的采样有相当概率落在这个几十微秒的缝里，那个 pid
   从此被永久跳过。表现具有误导性：2 GiB 的 `mkfs.xfs`（窗口更宽）漏了，1 GiB 的 `mkfs.ext4`
   抓到了。修法是**每轮都重新看当前子进程，`seen` 只用于写诊断轨迹**。
4. **窗口没抓住时不再一次定生死**：§8/§11 各自重试最多 3 次（每次用新的预检），失败的那一次让
   任务跑完再试。断言只在成功的那次上做，条数因此稳定；三次都没抓住才记 NOT RUN，并打印
   `frozen*.pid.trace`（每个子进程 pid + 当时的 comm）作为诊断。

另外（第 7 轮）：§13 的收尾探针用 `pallowed`/`pall` 直接取 `d["data"]`，遇到错误信封只会打印
`ERR:KeyError`——**测试工具自己把 500 吞成了一句看不懂的话**。这正是 Q190 差点被漏掉的原因，
所以第 9 轮把它拆成了显式断言 + 一条容错的记录项。

## 8. NOT RUN（逐条原因与替代证据）

| NOT RUN | 原因 | 替代证据 |
|---|---|---|
| `DEVICE_IDENTITY_CHANGED` 的内核/盘不一致那一路（§10b 的“换位”分支） | 要在提交前的一瞬制造“内核分区表与盘上 GPT 条目不一致”，在真机上是竞态，无法稳定复现 | 单测 `TestFormatRefusesPartitionThatMoved`（假扫描器构造内核/盘上不一致）；§10b 的**身份指纹变化**这一路仍由 `DEVICE_IDENTITY_CHANGED` 的单元测试与 p07-1 的分区指纹用例覆盖 |

进程中断原本也在 NOT RUN 名单里（第 8 轮漏掉了窗口），修掉第 7 节第 3 条之后第 9 轮**在第一次
尝试里就冻住了**，因此不再 NOT RUN。

## 9. 实际执行的检查

| 检查 | 结果 | 证据 |
|---|---|---|
| 真机验收 `p07-3-storage-format-acceptance.sh` | **PASS 204 / FAIL 0 / NOT RUN 1** | `run.log`、`payloads/`（20 份真实响应）、`logs/`（agent.log、web.log、lsblk） |
| 同机回归 `p07-1-storage-guard-acceptance.sh` | **PASS 184 / FAIL 0 / NOT RUN 2**（两条 NOT RUN 都是脚本里写明的“现场在真机上不存在”，见该文件的 `== 5.` 与 `== 10.` 段） | `regression-p07-1.txt` |
| 同机回归 `p04-2-storage-acceptance.sh` | **通过 114 / 失败 0 / 未执行 0**（只读拓扑没被格式化执行器带坏） | `regression-p04-2.txt` |
| 宿主 `scripts/check.sh`（Node 22.23.3） | **PASS 13 / FAIL 0 / NOT RUN 0** | `check.txt` |
| 宿主 `go test ./... -count=1` | **26 包 ok，EXIT=0** | `native-tests.txt` |
| `go vet ./...` | 干净 | 同上 |
| `gofmt -l internal/ cmd/` | 干净 | check.sh 的 gofmt 项 |
| `api/openapi.yaml` | valid，7 warning（与上一轮相同） | `check.txt` |
| storage 包单测条目数 | **PASS 328 / FAIL 0**（212 顶层 + 116 子测试）；同一口径对同一个包比：P07.2 提交 `882b6d5` 是 291，本轮 **+37**。新增的 `format_test.go`（20 条）/`fsprobe_test.go`（17 条）覆盖执行器、超级块探测与格式生命周期 | 本轮统计：本工作树 `go test ./internal/storage/ -count=1 -v` 对 `git worktree` 里的 `882b6d5` 同跑（口径见 §9） |

## 10. 已知限制

- 只覆盖 ext4 与 XFS；btrfs/f2fs 等不在本阶段范围（STAGES §9 只点名这两个）。
- 标签字符集是“可打印 ASCII、无 `/`”，没有做 UTF-8 标签（ext4 支持 `-E encoding`，本轮不涉及）。
- 格式化不做 `-m`/`-i`/`-b`/条带等参数，模板是固定的：本轮证明的是“按固定模板写对”。
- 三个门（标签、短语、二次身份检查）都在代理侧；Web 侧只做展示，不做二次判断——
  这与 CONTRACTS §4 的分工一致，但意味着 Web 被绕过时安全性仍然依赖代理。
- 22.04.5 上没有任何 P07 证据（验收只在 24.04.5 做过）。
- `go test -race` 仍未在真机跑过（验收 VM 没有 Go/cgo 工具链，交叉编译的测试二进制不带 `-race`）。

## 11. 下一子任务

**P07.4** —— 卷导入、挂载与 fstab：识别已有文件系统的分区并纳入管理、挂载点校验、`fstab`
条目的写入与回滚、挂载失败的错误退出，以及“挂载前必须有可复核的设备身份”这条与 P07.3 相同的
纪律。前置已就位：P07.3 的超级块直读（`fsprobe.go`）与文件系统身份、P07.2 的写盘链路与两源核验、
P07.1 的挂载/占用/系统盘保护。
