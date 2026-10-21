# P07.1 验收结果 — 存储写动作的拒绝逻辑（预检判决、可靠设备身份、整盘锁、系统设备/挂载/swap/holders/依赖保护）

- 阶段：P07（分区、格式化、挂载）/ 子任务 P07.1
- 日期：2026-10-21
- 结论：**P07.1 passed**。Ubuntu 24.04.5 真机断言 **PASS=189 / FAIL=0 / NOT RUN=1**（唯一 NOT RUN 是
  `PREFLIGHT_STALE`，见 §6），
  VM 内核原生 **587 个顶层用例 PASS / 0 FAIL / 1 SKIP**（storage 162 / agent 161 / api 161 /
  operations 45 / files 58），宿主 `go test -count=1 ./...` **26 包 ok**、
  `check.sh` **PASS 13 / FAIL 0 / NOT RUN 0**、前端 **29 文件 / 445 用例**、`api/openapi.yaml` valid（7 warning）、
  同机定向回归 `p04-2-storage-acceptance.sh` **112 / 0 / 0**（改动过 `scan.go`，必须证明只读拓扑没被带坏）。
- 这一轮**不写盘**：四个存储动作只登记参数解码与预检，`Execute` 为 nil，提交必 501 `NOT_IMPLEMENTED`。
  交付的是“该拒的都拒了”的真机证据，以及被真机改掉的**两个**产品缺陷（交换区魔数位置、holders/slaves 的读取方式）。

---

## 1. 本轮做了什么

`STAGES.md` §8 P07.1：“存储动作预检、可靠设备身份、整盘锁、系统设备/挂载/swap/holders/依赖保护；
只验证拒绝逻辑。”

### 1.1 判决层（`internal/storage/assess.go`）

给定“一份拓扑快照 + 目标设备 + 动作”，回答四件事：能不能做、为什么不能、会影响什么、锁哪块盘。
三条硬规则：

1. **拿不准就不放行**——身份、签名、占用、拓扑任何一项判定失败都变成阻塞项，不是默认 `false`。
2. **整盘保护沿依赖递归**——目标自己的保护标记不算数，沿 `partition_of`/`depends_on` 追到物理盘。
3. **未知即拒绝**——不认识的设备类型、读不出来的签名、只从 sysfs 拿到的分区表，都给“不能做”。

阻塞码是固定枚举（给前端分类用，不是 HTTP 码）：
`TOPOLOGY_UNAVAILABLE`、`DEVICE_NOT_FOUND`、`SYSTEM_DISK_PROTECTED`、`DEVICE_MOUNTED`、`SWAP_ACTIVE`、
`DEVICE_HAS_HOLDERS`、`DEVICE_BUSY`、`DEVICE_OFFLINE`、`DEVICE_READ_ONLY`、`UNSUPPORTED_DEVICE_KIND`、
`IDENTITY_UNKNOWN`、`IDENTITY_CONFLICT`、`PARTITION_TABLE_PRESENT`、`PARTITION_TABLE_UNKNOWN`、
`PARTITION_TABLE_NOT_GPT`、`PARTITION_TABLE_UNREADABLE`、`PARTITION_NOT_FOUND`、
`FILESYSTEM_SIGNATURE_PRESENT`、`SIGNATURE_UNREADABLE`、`DEVICE_IS_STACKED`、`REGION_UNALIGNED`、
`REGION_NOT_FREE`、`REGION_TOO_SMALL`、`NO_FREE_REGION`、`PARENT_DISK_UNKNOWN`、`LABEL_INVALID`；
告警码 `IDENTITY_LOW_CONFIDENCE`、`PARTITION_TABLE_WARNING`、`EXISTING_DATA_WILL_BE_LOST`。

### 1.2 四个动作的策略（`internal/agent/storageaction.go`）

| 动作 | 再认证 | 短语 | 锁 | 可取消 | 执行器 |
|---|---|---|---|---|---|
| `storage.partition_table.initialize` | 是 | `INIT xxxxxx` | `disk:<整盘>` | 否 | 无（501） |
| `storage.partition.create` | 是 | 无（只在空闲区加条目，不覆盖数据） | `disk:<整盘>` | 否 | 无（501） |
| `storage.partition.delete` | 是 | `DELETE xxxxxx` | `disk:<整盘>` | 否 | 无（501） |
| `storage.filesystem.format` | 是 | `FORMAT xxxxxx` | `disk:<整盘>` | 否 | 无（501） |

标签上限按文件系统各自的真实限制：ext4 16 字节、XFS 12 字节（通用 255 字符是错的）；
分区名 36 字符 ASCII；起点与大小按 1 MiB 对齐且 ≥1 MiB。

### 1.3 能力投影（`internal/storage/capabilities.go`）

`available`/`blocked`/`unsupported` 三态 + `details` 带阻塞码，由判决层投影而来，**不手写**；
缓存 30 秒、键是 `(设备身份, snapshot_version)`。`storage.partition.create` 的投影不带区间，
所以它的 `available` 只表示“结构上能做”（有 GPT、有空闲区、名字合法）。

### 1.4 签名与占用

- 签名只读**头部 68 KiB + 尾部 16 KiB**，认 ext(0xEF53@1080)、XFS、Btrfs、swap v1/v0、
  LUKS1/LUKS2、LVM2、md 1.0/0.90 的固定魔数；全零才是 `blank`，认不出来的非零内容是 `unknown`。
  读失败 → `SIGNATURE_UNREADABLE`（不唤醒休眠设备）。
- 占用检查遍历 `/proc/<pid>/fd`，按 **`st_rdev` 设备号**比对（不是路径）；读不到 `/proc` 就报错，
  由判决层拒绝——**“看不见”不能当成“没人用”**。

### 1.5 分区身份

`"sha256:" + hex(sha256("upanel-partition-v1|" + 整盘指纹 + "|" + 盘ID + "|" + 起点))`，
继承整盘的 `identity_confidence`。预检 DTO 只暴露 `display_name` 与 `identity_fingerprint`，
不暴露内部绑定用的 `state_version`。

---

## 2. 真机验收覆盖（`tests/integration/p07-1-storage-guard-acceptance.sh`，13 节）

| 节 | 覆盖 |
|---|---|
| 0 | 安全守卫：先算承载 `/` 的整盘，任何与它同源的一次性盘 → 拒绝运行（exit 2）；三块盘 wipefs + zap-all + 首尾 4 MiB 归零 |
| 1 | 代理 root + Web 非 root（uid=999）、init-admin、登录、CSRF、viewer 账号 |
| 2 | 能力真实性：`supported_actions` 里**没有**任何 `storage.*`；四个动作都能预检；空白盘四项能力 = available/blocked/unsupported/unsupported；系统盘初始化 = blocked 且理由含“系统” |
| 3 | 系统盘与它的 **4 个分区**（sda1/sda14/sda15/sda16）的初始化/删除/格式化一律 `SYSTEM_DISK_PROTECTED`，锁是整盘 |
| 4 | 空白盘初始化：`allowed=true`、影响含“未检测到已知签名不代表盘上绝无原始数据”与“覆盖盘上的分区元数据”、短语首词 `INIT`、要求再认证、整盘锁、首尾 4 MiB 哈希不变 |
| 5 | 提交的两道门：未再认证 → 403 `REAUTH_REQUIRED`（盘上字节不变、无任务）；再认证后 → 501 `NOT_IMPLEMENTED`，任务表只多一条 `failed`/`rejected`（Q159），无任何非终态任务 |
| 6 | 已有 GPT：初始化 `PARTITION_TABLE_PRESENT`；建分区的对齐/太小/越界/落在表内/名字过长/字节写成数字/`mbr` 表类型/带路径分隔符的设备 id |
| 7 | 签名：整盘 `mkfs.ext4`（LBA0 全零）、整盘 `mkswap`（魔数在 4096−10）、LUKS2 头 → 全部 `FILESYSTEM_SIGNATURE_PRESENT` |
| 8 | 四类占用：挂载中 `DEVICE_MOUNTED`、活动交换区 `SWAP_ACTIVE`、dm holder `DEVICE_HAS_HOLDERS`、进程打开 `DEVICE_BUSY`（理由带 pid）；整盘被打开时它上面的分区动作也拒 |
| 9 | 身份：分区指纹是 `sha256:` 形态、两次预检一致、拓扑变化后不变、不同分区不同 |
| 10 | 权限：匿名 401、viewer 403（预检与提交），viewer 仍能读设备详情 |
| 11 | 回归与日志卫生：拓扑/设备详情/系统概览/任务列表仍正常；代理与 Web 日志里没有 `sgdisk/mkfs/wipefs/mkswap/dmsetup create`，没有明文口令 |
| 12 | 收尾：擦掉夹具后初始化能力回到 `available`、再次允许；无遗留 swap、无遗留 dm |

脚本**不做**任何产品未授权的事：所有分区/文件系统/swap/dm 现场都是测试手工具（sgdisk/mkfs/mkswap/dmsetup）
在**一次性空盘**上造的。

---

## 3. 本轮被真机抓出的产品缺陷（全部改产品，不改断言）

### 3.1 交换区魔数读错位置（DECISIONS Q171）

签名层原本只在盘尾 16 KiB 找 `SWAPSPACE2`。真机上 `mkswap -L p07swap /dev/sdd` 之后，
整盘交换区被报成 `unknown`。实测（`probe-swap-holder.sh`，2026-10-21）：

- `SWAPSPACE2` 在偏移 **4086 = 4096−10**（第一个页面的末 10 字节）；
- `wipefs` 报的偏移也是 `0xff6`；
- 盘尾 64 KiB **全零**；
- `swapon` 接受该设备。

改成按 4 KiB/64 KiB 页两种边界精确比对，盘尾扫描保留（旧工具与文件交换副本会写在那里）。
补反例用例：「盘中间出现字面量 `SWAPSPACE2`」必须仍是 `unknown`。

### 3.2 `holders`/`slaves` 不是 `list` 文件（DECISIONS Q172）

原实现只读 `/sys/block/sdc/sdc1/holders/list`，而 6.8 内核对**分区不挂这个属性**
（`No such file or directory`）：`holders/`、`slaves/` 是符号链接目录。后果是
“分区正被 dm/md 占用”完全看不见，`DEVICE_HAS_HOLDERS`/`DEVICE_IS_STACKED` 在真机上永远不触发。
改成 `linkNames()` 以**目录项**为事实来源（跳过汇总用的 `list` 条目），`list` 只作退路，四处调用点统一。

---

## 4. 验收脚本自身的缺陷（记录，不掩盖；断言只增不减）

1. §3 用 `lsblk -no NAME` 枚举根盘分区，输出带 `├─` 树形前缀 → 一条也匹配不上 → 这一节**什么都没断言
   却报了 NOT RUN**。改成 `-l`；`-r` 与 `-l` 互斥，不能叠。修好后 4 个分区各一条断言。
2. §5 直接提交存储动作 → 403 `REAUTH_REQUIRED`（四个动作都要求 5 分钟内再认证）。改成两道门各自断言。
3. §5 原先断言“提交后任务数不变”，与 Q159 矛盾（受理即拒的作业保留一条 failed/rejected 用于追溯）。
   真机上确实多了一条 `failed`/`rejected`/`NOT_IMPLEMENTED`。改成“只多一条 + 停在 rejected + 无终态外任务”。
4. §7 的 LUKS 现场等 `partition_table.type == "none"` 永远等不到：LUKS 头在 LBA0，真实值是 `unknown`（Q176）。
5. §8 的 `FREE_START_C` 从**分区**详情里取整盘空闲区 → `ERR:TypeError` 被当数值塞进 `start_bytes` → 400
   → 后续断言全变成 `KeyError:'data'`。改成先等整盘 GPT 与空闲区进快照、再读整盘详情，并对
   `''/None/ERR:*` 显式失败（Q175）。
6. 快照新旧与能力 TTL：`settle` 必须同时满足“`collected_at` 晚于夹具开始时刻”与期望值；能力断言轮询 40 秒
   （能力投影 30 秒 TTL）；空闲区取**最大**的那块，不取 `free_regions[0]`（那是 GPT 之后 34 扇区 → 1 MiB
   的未对齐缝隙）（Q175）。
7. **原生测试的采集脚本**（`collect-native.sh`）第一次是以 `upanel` 用户跑的，于是
   storage 37 FAIL、agent 132 FAIL、files 13 FAIL、api 直接 10 分钟超时——192 条“失败”里没有一条是产品问题：
   agent/api 的用例要建 **root 属主 0700** 的代理数据目录（`agentstore` 会校验，报“目录的属主必须是 root，
   实际为 1000”），storage/monitoring 的用例要读 `tests/fixtures` 里的真机字节（报“定位夹具目录失败 …
   可用 `UPANEL_FIXTURES` 指向 tests/fixtures”），而 api 包在 TCG 客机上跑 argon2id 登录本来就慢，
   `-test.timeout 10m` 不够。修法：脚本加两条前置检查（不是 root 就 `exit 2`、没有夹具就 `exit 2`），
   显式 `export UPANEL_FIXTURES`，超时放到 1 小时，并把 `tests/fixtures` 传到 VM 的 `/tmp/fixtures`。
   **这条记在这里是因为它差点把“环境没配对”写成“产品坏了”。**
8. **`p04-2-storage-acceptance.sh` 里有四处陈旧断言和一处脆弱断言**（同机回归时暴露，105 通过 / 5 失败）：
   - 能力表被写死成 P04 时代的两条动作、六条查询；P05 加了服务/进程、P06 加了文件/终端、P07.1 加了
     设备能力投影，于是“完全等于”的断言必然红。**改钉跨阶段不变量**：`storage.topology` 在表里、
     **没有任何 `storage.*` 写动作被登记成可执行**（这正是 Q170 的不变量）、查询表有序无重复。
     写死清单只会让每个阶段都“改期望值把灯变绿”，那是橡皮图章。
   - “`health.checked_at` 一律是 null”在 P04.3 之后就不成立（物理盘启动即检查一次）。改成诚实性
     不变量：**状态不是 unknown 就必须有检查时间**——“说健康就得有检查时间”。
   - “`capabilities` 一律是空数组”在 P07.1 之后不成立。改成：设备能力只能引用 `storage.*` 动作、
     状态必须在 `available/blocked/unsupported` 闭集内。
   - §9「`collected_at` 在一个间隔之后前进了」拿的是 §2 那份快照的 `T1`，而 §3—§8（回环挂载、
     statvfs、卸载）在 TCG 客机上要跑几分钟，于是 `T3−T1` 早就超过断言里的 30 秒上限——它其实在
     断言“前面几节跑得快不快”。改成**本节现取基线 T2** 再等 6 秒比较。这一条本轮确实假红过一次。

   修完后同机重跑 **112 / 0 / 0**（断言从 110 条增加到 112 条，只增不减），
   证明 `holders`/`slaves` 读取方式的改动没有带坏只读拓扑。

第一轮真机跑在 §5（提交门）、§7（LUKS 快照）、§8（整盘忙时建分区）三处报失败，逐条定位后全部落在
上面 1—5 上；两条产品缺陷（§3.1、§3.2）也在这轮被抓出。修掉 §4 的第 1—5 条后只剩第 3 条（与 Q159 的语义
冲突），第三条那一轮的真机读数已随 `/tmp` 覆盖不可查，因此这里不写具体数字；最终读数见 §5 表格与
`p07-1-acceptance-2404.txt`（`PASS 189 / FAIL 0 / NOT RUN 1`）。

---

## 5. 实际执行的检查与证据

| 检查 | 结果 | 文件 |
|---|---|---|
| 真机验收（Ubuntu 24.04.5，13 节） | **PASS=189 / FAIL=0 / NOT RUN=1** | `p07-1-acceptance-2404.txt` |
| VM 内核原生（五个交叉编译测试二进制） | **587 顶层用例 PASS / 0 FAIL / 1 SKIP** | `native-tests.txt`、`native-tests-summary.txt`、`native-<包>.txt` |
| 宿主 `go test -count=1 ./...` | 26 包 ok（3 个包无测试文件） | `go-test.txt` |
| 宿主 `gofmt -l` / `go vet` | 无输出 / 退出码 0 | `gofmt-vet.txt` |
| 宿主 `check.sh` | **PASS 13 / FAIL 0 / NOT RUN 0**（Node 22.23.3 固定工具链） | `check.txt` |
| 前端 vitest | 29 文件 / 445 用例 | `web-test.txt` |
| 契约 lint | valid，7 warning（本轮未改 `api/openapi.yaml`） | `check.txt`（openapi-lint 段） |
| 验收载荷 | 逐份存档 | `payloads/` |
| 环境事实 | 内核、磁盘布局、二进制 sha256、工具版本 | `env.txt` |
| 实测脚本（交换区魔数、dm holders） | 见 §3 | `probe-swap-holder.sh`、`probe-holder.sh` |
| 排查脚本（整盘忙时建分区预检） | 见 §4 第 5 条 | `repro-create-busy.sh` |
| 定向回归：`p04-2-storage-acceptance.sh`（存储只读拓扑，被本轮改过 `scan.go`） | **通过 112 / 失败 0 / 未执行 0**（同机重跑；第一轮 105/5 全部是脚本自身的陈旧/脆弱断言，见 §4 第 8 条） | `regression-p04-2.txt`、`regression-p04-2-first.txt` |

---

## 6. 未验证（NOT RUN）与原因

| 项 | 为什么没验 |
|---|---|
| 执行前复核 `PREFLIGHT_STALE` | 四个动作没有执行器，提交在 `revalidate` 之前就 501，真机上触不到这条路径。P07.2 登记执行器后补 |
| 真实的写盘、分区、格式化 | 本轮范围就是拒绝逻辑；没有任何一次产品写操作发生过 |
| RAID（md）成员的 holders | 验收 VM 没有 mdadm 现场；holders 用 dm（同一条 sysfs 路径）证明，md 只在单测夹具里覆盖 |
| 离线/只读设备 | 要热拔盘或真光盘；P04.3 已用 sysfs 夹具与 loop 覆盖只读判定 |
| 应用/共享/备份依赖（PLAN §9.1 第 5—7 项） | 到 P08/P11/P17 才有对象 |
| 两块盘并发抢锁的真实争用 | 本阶段没有执行器；存储动作的锁字符串在 §3/§4/§6 里断言，锁机制本身 P03.2 已验 |
| 22.04.5 | 本轮只在 24.04.5（TCG 客机）上跑过 |
| `go test -race` | 验收 VM 没有 Go/cgo 工具链，交叉编译的测试二进制不带 `-race` |
| 前端行为 | 本轮没有前端改动；设备页不渲染动作按钮，写操作界面等 P07.2 |

未验证项一律不计入通过项。

---

## 7. 环境备注（复现用）

- 验收 VM：Ubuntu 24.04.5，QEMU 用户态 NAT，`-Accel tcg,thread=multi`
  （**whpx 会让该客机 panic**，`console.panic-*.log` 存档在 VM 目录下）。
- ssh：`ssh -i H:/Code/vms/upanel-2404/id_ed25519 -p 2222 upanel@127.0.0.1`，然后 `sudo -n`
  （cloud-init 建的是 **upanel** 用户 + NOPASSWD sudo；root 直登没有配置）。
- 磁盘：sda 40 GiB（sda1 `/`、sda14、sda15 `/boot/efi`、sda16 `/boot`），三块一次性数据盘 sdb 8 GiB、
  sdc 4 GiB、sdd 4 GiB。
- 二进制由宿主交叉编译：`GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o /tmp/upanel-bin/… ./cmd/…`，
  `chmod 755` 后 `scp` 到 `/tmp/`，再 `sudo mv` 到 `/tmp/upanel-bin`。
- 原生测试二进制：`GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go test -c -o /tmp/<pkg>.test ./internal/<pkg>`。
