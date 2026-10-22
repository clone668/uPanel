# P07.2 验收结果 — 真的写盘：初始化 GPT、创建分区、删除未使用分区

| 项 | 值 |
|---|---|
| 子任务 | STAGES §9 P07.2：“在隔离 VM 的空白虚拟盘上初始化 GPT、创建分区、删除未使用分区；固定模板、对齐、udev 等待、真实结果验证。” |
| 日期 | 2026-10-22 |
| 机器 | `upanel-2404`（Ubuntu 24.04.5 LTS，内核 6.8.0-147-generic，QEMU 客机） |
| 工具链 | Go 1.27.3（宿主交叉编译 `GOOS=linux GOARCH=amd64 CGO_ENABLED=0`）、util-linux 2.39.3（sfdisk/udevadm）、gdisk 1.0.10（sgdisk）、e2fsprogs 1.47.0、Python 3.12.3 |
| 一次性盘 | sdb 8 GiB、sdc 4 GiB、sdd 4 GiB（串口序列号 `UPANEL-DATA-A-8G` / `-B-4G` / `-C-4G`），**都不是系统盘**；系统盘 sda 40 GiB 全程只做“被拒绝”的用例 |
| 验收脚本 | `tests/integration/p07-2-storage-write-acceptance.sh`（13 节，sha256 `ae1cfafb…fe426d`） |
| 结果 | **PASS 177 / FAIL 0 / NOT RUN 1**（`REMOTE_EXIT=0`），原始输出 `run.log` |
| 原生测试 | `native-tests.txt`（13 个包，结果见 §7） |
| 证据 | 本目录：`run.log`、`env.txt`、`native-tests.txt`、`payloads/`（14 份真实响应）、`logs/`（代理与 Web 的日志） |

一句话结论：**三个分区动作在真机上真的写了盘**，写下去的是代码里的固定模板、位置按 1 MiB 对齐、
写完等 udev 稳定，然后按“盘上的字节 + 内核/sysfs”两个来源分别读回、两边对上才算 `verified`。
零退出码不算验证，任务里的 `outcome` 必须是 `verified` 才算成功。

---

## 1. 本轮做了什么

### 1.1 写盘执行器（`internal/storage/write.go`，943 行）

- **固定模板，不拼命令行。** `initialize` 的脚本只有一个字面量 `label: gpt\n`
  （`initializeScript()`），`create` 是 `start=<sectors>, size=<sectors>, type=linux, name="<n>"`
  加 `--append --wipe-partitions never`，`delete` 是 `--delete <dev> <n>`。
  唯一进入命令行的变量是**已校验的数字**（起点/大小换算成 512 字节扇区，必须是 2048 的整数倍）
  和**已校验的分区名**（可打印 ASCII、≤36 字节、不含 `"` 与 `\`）。
- **只经标准输入。** util-linux 2.39.3 的 `sfdisk` 用法里没有“脚本文件”参数（实测），
  所以模板走 `syscmd.Spec.Stdin`；那不是命令通道，是一个 64 KiB 上限的字节通道，
  而且注释里写明“只能由代码里的固定模板填充，不得来自 HTTP 参数”。
- **写之前再看一眼。** 执行器在 `Execute` 里重新扫描拓扑、重算判决，比对预检绑定的
  `identity_fingerprint` 与 `state_version`，再核对“现在这块盘的表类型是不是我以为的”
  （`initialize` 要求现在是 `none`，其余两个要求现在是 `gpt`）。对不上就是
  `DEVICE_IDENTITY_CHANGED` / `PREFLIGHT_STALE`，一个字节都不写。
- **delete 的分区号从盘上的条目表反查。** 参数里给的是分区设备名（`sdb2`），
  槽位号取自 `disk_entries`，不从设备名里猜数字。
- **写之后等三件事，然后两源核验：**
  1. `udevadm settle --timeout=N`（`observed_state.udev_settled`）；
  2. 内核/sysfs 里出现预期的分区集合，`/dev/<part>` 节点存在（`waitForKernel`）；
  3. 重新读盘上的 GPT 头与条目表，与“写之前就按意图算好的期望形态”（`expectedState`）逐条比对
     （`compareEntries`）。
  任何一步不满足都是**失败**，并且失败文案写“先核实实际状态，不要直接重新提交”；
  命令返回 0 但上下文已经结束（超时/停机）时，明确报“实际状态未知”，不猜成功。
- **报告。** `observed_state`：`device_id`、`table_type`、`disk_guid`、`disk_entries`、
  `kernel_partitions`、`udev_settled`；`data`：`device_id`、`table_type`、`partitions`、
  `checked_via=["disk_bytes","kernel_sysfs"]`，create 另带 `created_partition`，
  delete 另带 `deleted_device_id`（CONTRACTS §6.3）。

### 1.2 `syscmd` 增加有上限的标准输入（`internal/syscmd/syscmd.go`）

`Spec.Stdin []byte` + `MaxStdinBytes = 64 KiB` + `ErrStdinTooLarge`。仍然逐项传参、
绝对路径、白名单环境、不经过 `sh -c`；空 `Stdin` 时子进程拿到的仍是空 stdin（不是父进程的）。
`syscmd_test.go` 补了三类用例：模板原样到达子进程、超限被拒、空 stdin 不继承父进程。

### 1.3 登记（`internal/agent/registry.go`、`storageaction.go`、`cmd/upanel-agent/main.go`）

三个分区动作从“只解码 + `Plan`”变成“解码 + `Plan` + `Execute`”；
`storage.filesystem.format` **仍然没有执行器**（P07.3），提交照样 501 `NOT_IMPLEMENTED`。
执行器的依赖（`sfdisk`、`udevadm`、拓扑扫描器、命令执行器）在启动时装配，
缺任何一个都在执行时返回 `DEPENDENCY_MISSING`，而不是假装成功。

### 1.4 契约：`partition_table` 多两个字段（P07.2 起）

`disk_guid` 与 `disk_entries`（只读主 GPT，LBA 1—33）。写动作的结果核验要求
“盘上字节”与“内核视图”两边都对上，因此这两个字段是**验收的一部分**，不是展示用的补充。
CONTRACTS §6.3 写了字段表与规则，`api/openapi.yaml` 加了 `StorageGPTEntry`，
前端 `web/src/api/storage.ts` 同步了 DTO，并用真机载荷回放（§7）。

### 1.5 一个真机才看得见的产品缺陷

`disk_entries` 第一版是 `[]GPTEntry` 加 `omitempty`：一张**刚初始化、还没有分区**的 GPT
条目表是空数组，被省略；而“没读到表”的设备本来就没有这个键。于是“读到了，零条”与
“没读到”在 JSON 里变成同一件事，而验收正是用“零条”证明初始化真的清空了条目表（Q177）。
改成 `*[]GPTEntry`，并让 `tableStateOf` 在“读到了 GPT 却拿不到条目表”时直接报错，
不拿 nil 当零条。

---

## 2. 真机验收覆盖（13 节，逐节断言数）

| 节 | 内容 | PASS |
|---|---|---|
| 0 | 环境与安全守卫（root、工具存在、三块一次性盘都不是系统盘、代理硬编码的 `/usr/sbin/sfdisk` 与 `/usr/bin/udevadm` 真的在） | 25 |
| 1 | 配置与启动（代理以 root、Web 以 `upanel-web` 非 root） | 8 |
| 2 | 三个分区动作已登记为可执行，格式化仍未登记 | 4 |
| 3 | 空白盘初始化 GPT：真的写盘、模板固定、两源核验、幂等重放不写第二次 | 29 |
| 4 | 创建分区：1 MiB 对齐、条目表与内核两边对上、`/dev` 节点当场就在 | 27 |
| 5 | 第二个分区带名字：名字真的落进条目表（用 `sgdisk` 这个**别的工具**读回来） | 16 |
| 6 | 删除未使用分区：条目消失、节点消失、**前面的槽号没有重排** | 23 |
| 7 | 执行前复核：预检之后用宿主工具改盘 → 409 `PREFLIGHT_STALE`，一个字节没多写 | 11 |
| 8 | 系统盘：预检 `allowed=false` → 提交在 Web 侧 409 `DEPENDENCY_BLOCKED`，不产生任务 | 10 |
| 9 | 写动作不改变设备身份指纹（否则写完之后自己的预检立刻失效） | 3（+1 NOT RUN） |
| 10 | 并发：同一块盘两个提交只允许一个成功 | 8 |
| 11 | 回归与日志卫生（拓扑/详情/任务列表仍可读、Web 日志里没有分区命令、日志里没有明文口令） | 9 |
| 12 | 收尾：擦回空白后设备能力回到 `available` | 4 |

每一节都不是“接口返回 200 就算过”：写盘相关的断言都配一份**独立证据**——
GPT 头用 Python 直接读 LBA1（不经过产品的解析器）、分区存在性用 `sgdisk -p` 的表格行、
内核大小用 `lsblk --bytes`、盘上字节变化用首尾扇区的 sha256。

---

## 3. 只有真机才知道的事实（都写进了代码注释）

1. **`sfdisk` 只从标准输入读脚本**（util-linux 2.39.3 的用法里没有脚本文件参数）。
2. `label: gpt` 写入后 `FirstUsableLBA = 2048`（= 1 MiB），盘尾的备份头同时存在。
3. `sfdisk --delete <dev> <n>` **不重排**：删掉 2 号之后 1 号仍然是 1 号（`sgdisk -p` 可见）。
4. 分区名里放一个 `"` 会让 sfdisk 报 `line 1: unsupported command`（rc=1）；
   `\` 会被转义处理吃掉。所以名字校验直接拒绝这两个字符，而不是“尽力转义”。
5. `sgdisk -i N` 在分区不存在时 **仍然 rc=0**（只在 stdout 打印 `Partition #N does not exist.`）。
6. `udevadm settle` 返回之后 `/dev/<part>` 与 `/sys/class/block/<part>` 当场就在，
   不需要额外 sleep——验收是“任务刚结束就查”，不是“等两秒再查”。

---

## 4. 真机抓出的产品缺陷

### 4.1 “读到零条”与“没读到”在 JSON 里是同一件事（Q177）

- 现象：`设备详情里盘上条目数是 0: ERR:KeyError:'disk_entries'`。
- 成因：`[]GPTEntry` + `omitempty`，空数组被省略。
- 处理：字段改成 `*[]GPTEntry`；`table.go` 读到 GPT 时归一成空数组并取地址；
  `write.go` 的 `tableStateOf` 反过来要求“读到 GPT 就必须有条目表”，否则报错。
- 结论：**改产品，不改断言**。这条断言正是“初始化真的清空了条目表”的唯一证据。

---

## 5. 验收脚本自身的缺陷（记录，不掩盖）

| # | 缺陷 | 处理 |
|---|---|---|
| 1 | 把 `confirmation_phrase` 的 `None` 当短语发出去，任务照样成功——服务端对不需要短语的动作不校验（Q178） | 断言 `is None`，提交传空串 |
| 2 | 删除用例在**空白**分区上断言 `data_loss`，产品只给 `partition_table_change`（Q179） | 产品是对的：先用 `mkfs.ext4` 造真签名、`blkid` 断言签名在，再断言影响 |
| 3 | 用 `sgdisk -i 2` 的退出码判断分区是否消失，真机上 0 号也不变（Q180） | 改成读 `sgdisk -p` 的表格行，另断言输出里有 `does not exist` |
| 4 | 能力列表写死成固定动作名，P06 起就已经过期（Q181） | 改成跨阶段不变量（闭集、无重复、写动作不出现在查询里） |
| 5 | `scripts/run-native-tests.sh` 只数 `--- FAIL` 行算失败。包超时 panic / 崩溃时一条 `--- FAIL` 都不打印，于是 `rc≠0` 却被算成“0 失败”，整个脚本在最该报警的时候报绿 | 加 `rc≠0 且无 --- FAIL` 计一次失败并打印尾部 25 行；每个包加 `-test.timeout`（默认 3600s，`UPANEL_TEST_TIMEOUT` 可覆盖）——TCG 模拟的 VM 上 api 包默认 10 分钟会先到点 |

这几条都是**我的断言/工具错了，不是产品错了**。把断言改松是不允许的，所以每条都换成了
“更难假通过”的写法（换工具读、换数据源读、换成不变量、把“没打印失败”也算失败）。

---

## 6. 机会性证据：`runner` 里的第二次 `revalidate`（Q182）

`p07-1` 把 `step: revalidate` 记为 NOT RUN——没有执行器时提交在复核之前就 501，
撞不到“受理之后、执行之前有人改盘”。本章 §10 的两个并发提交（同一块盘、两个预检）
在两次运行里都撞到了：

```json
{"job_id":"job_52969ae842d682b0e46d85fd8ea27c37","action":"storage.partition.create",
 "status":"failed","step":"revalidate",
 "error":{"code":"PREFLIGHT_STALE","message":"目标状态在预检之后发生变化，请重新预检",
          "recovery_hint":"重新预检后再提交"}}
```

两个提交都被受理（202），输掉的那个执行前复核时发现盘已经变了 → 失败，盘上只多了一个分区。
这是**机会性**证据：脚本把实际发生的闸门（`rejected` 还是 `revalidate`）打印出来，
但不要求每次必然出现。

---

## 7. 实际执行的检查与证据

| 检查 | 命令 | 结果 | 证据文件 |
|---|---|---|---|
| 宿主全套检查 | `sh scripts/check.sh` | PASS 13 / FAIL 0 / NOT RUN 0 | `check.txt` |
| Go 构建/静态检查 | `go build ./...`、`go vet ./...`、`gofmt -l .` | 干净（`GO_VET_EXIT=0`、`GOFMT_EXIT=0`） | `go-test.txt`、`check.txt` |
| 宿主单元测试 | `go test -count=1 ./...` | 26 个包 ok / 3 个包无测试文件（不吃缓存） | `go-test.txt` |
| 前端 | `npm run typecheck` / `npm test` | 30 文件 / 450 用例全过（新增 5 例真机载荷回放） | `check.txt` |
| OpenAPI | `redocly lint api/openapi.yaml` | valid（7 warnings，均为既有） | `check.txt` |
| 真机验收 P07.2 | `sh p07-2-storage-write-acceptance.sh` | **PASS 177 / FAIL 0 / NOT RUN 1** | `run.log` |
| 真机原生测试 | `sh run-native-tests.sh`（13 个包） | **TOTAL PASS=1134 / FAIL=0**，13 个包全 `rc=0` | `native-tests.txt` |
| 回归 P07.1 | `sh p07-1-storage-guard-acceptance.sh`（三块一次性盘） | **PASS 194 / FAIL 0 / NOT RUN 1**（`REMOTE_EXIT=0`；唯一 NOT RUN 仍是 `PREFLIGHT_STALE`，本轮由 P07.2 验收覆盖） | `regression-p07-1.txt` |
| 回归 P04.2 | `sh p04-2-storage-acceptance.sh` | **通过 114 / 失败 0 / 未执行 0** | `regression-p04-2.txt` |

P07.1 回归是 **194** 而不是当时的 **189**，差 5 条全部来自脚本自己这轮的改动，不是现场抖动：
§2 从 12 条变 13 条（能力表断言换成跨阶段不变量，Q181），§5 从 14 条变 18 条——**§5 必须改，
否则这条回归会写盘**：P07.1 当时“再认证之后仍然 501”用的是空白盘初始化，P07.2 起初始化有了
执行器，再提交这条预检就真的把盘写成 GPT 了。所以 §5 换成仍无执行器的格式化（P07.3），
断言“501 `NOT_IMPLEMENTED` + 只多一条终态任务 + 盘上字节不变”。主语句没变：验的是门，不是写。

原生测试跑了两遍，两遍都是 **TOTAL PASS=1134 / FAIL=0**：第一遍用改之前的
`run-native-tests.sh`（只数 `--- FAIL`，输出留底 `native-tests-run1-unbounded.txt`），
第二遍用补上 `-test.timeout` 与“`rc≠0` 且无 `--- FAIL` 记一次失败”之后的脚本（`native-tests.txt`，
VM 上脚本 sha256 `8736e22f…e84cc5eb`）。两遍一致说明那处修改没有改变任何判定，只补上了
“超时/崩溃会被算成 0 失败”这个洞。

---

## 8. 未验证（NOT RUN）与原因

| 项 | 原因 |
|---|---|
| `DEVICE_IDENTITY_CHANGED`（执行前身份变了） | 身份指纹来自序列号/WWN/型号+拓扑地址，VM 里没法在不换硬件的前提下改它。`write_test.go` 的 `TestWriteRejectsIdentityChange` 用假扫描器覆盖了这条分支；真机要热插拔一块同型号盘才可能触发。 |
| 备份 GPT 表的一致性 | 产品的条目表读取只读主表（LBA 1—33），盘尾的备份表不解、不比对。验收用独立读取证明备份头**存在**，但“主备不一致时怎么办”没有验。这是已记录的限制（PROGRESS §8）。 |
| 格式化（ext4/XFS） | P07.3。本轮 `storage.filesystem.format` 仍然没有执行器。 |
| 损坏的 GPT（CRC 错、主备不一致） | 没有安全的制造方法又不影响后续用例。 |
| 写盘过程中断电/进程被杀 | P07.3 的验收点（“测试错误退出和进程中断”）。本轮只验了“命令返回后上下文已结束 → 报实际状态未知”。 |

---

## 9. 与 P07.1 的差异

| 动作 | P07.1 | P07.2 |
|---|---|---|
| `storage.partition_table.initialize` | 只 `Plan`，`Execute` 为 nil，提交 501 | **真的写盘**，两源核验后 `verified` |
| `storage.partition.create` | 同上 | **真的写盘**（`--append`，不覆盖已有条目） |
| `storage.partition.delete` | 同上 | **真的写盘**（按槽位删，不重排） |
| `storage.filesystem.format` | 501 | 501（未变，P07.3） |

`p07-1` 里“过门之后必然 501”的那一段因此改成用格式化动作来证明——**它仍然一个字节都不写**。

---

## 10. 环境备注（复现用）

- 二进制与脚本上传后核过 sha256（`env.txt`）。
- `/tmp/upanel-bin` 必须是 0755：以 `upanel-web` 身份跑的 Web 进程要穿得过这个目录。
- 上传的二进制落地是 0644，执行位要 `sudo install -m 0755` 或显式 `chmod`；
  `chmod +x /tmp/upanel-bin/upanel-*` **匹配不到 `upanelctl`**（没有连字符），
  要用显式文件名。
- 验收跑之前确认 `sdb/sdc/sdd` 上没有分区、没有挂载、没有 swap：脚本自己会先算一遍，
  不满足就退出码 2，不会“顺手擦掉”。
- 本轮 `upanel-2404` 全程未重启，`Attempted to kill init` 计数为 0（`env.txt`）。
