# P07.5 验收结果 — 磁盘页面完整交互 + 系统外部独立检查 + 存储恢复文档 + openapi 补齐 P07

**证据类型：Linux VM integration（真机验收，独立 Ubuntu 24.04 VM）+ mock integration
（真机载荷回放的 SSR 组件测试）+ unit（Go / Vitest）。不含 browser E2E 与人工观察。**

- 日期：2026-10-22
- 范围：`docs/STAGES.md` §9 P07.5 全句——“磁盘页面完整交互、系统外部独立检查、
  存储恢复文档、`api/openapi.yaml` 补齐 P07 接口。”
- 结论：**PASS 205 / FAIL 0 / NOT RUN 3**（真机验收脚本 EXIT=0）；
  宿主 `scripts/check.sh` **PASS 13 / FAIL 0 / NOT RUN 0**。
- 运行日志：`run.log`（205 条 PASS 逐条在内）
- 本轮真机抓出 **1 个产品缺陷**（Q208，纳入管理的候选永远是空的）与
  **2 个验收脚本自身缺陷**（Q204、Q209）。三条都改在代码里，没有改验收条件。

## 1. 这一轮交付了什么

把 P07.1–P07.4 已经能干活的后端接上页面：九种动作都有入口、每一步都能看到预检为什么放行或
拒绝、任务提交后能看到“到底做了什么、凭什么说成功”；再给这台机器配一把**不经过面板的尺子**
（`storage-external-check.sh`），用 `/etc/fstab`、`findmnt`、`/proc/self/mountinfo`、systemd
单元独立复述同一批事实。

| 文件 | 改动 |
|---|---|
| `web/src/api/volumes.ts`（新增，413 行） | 卷的 DTO、三个只读接口的取数、**导入候选的筛选**（`importCandidates`）、`isManagedFilesystem`、`candidateFilesystemLabel`。候选判据是本轮 Q208 的修复点：类型未知仍然列出，不把“未挂载所以读不到”当成“不合格” |
| `web/src/utils/jobresult.ts`（新增，185 行） | 一次存储写任务的 `result` 翻译成“一句话结论 + 证据行 + 恢复建议”。五个卷动作与四个设备动作结果形状不同但读法一致，集中一处解析；字段缺失时不编造、不显示 `undefined` |
| `web/src/components/VolumeImportForm.vue`（新增，231 行） | 导入表单：候选列表、slug → 挂载点的预览与校验、`automount`/`read_only`/`nodev`/`nosuid` 的勾选、未挂载分区的“类型读不到”如实说明 |
| `web/src/components/VolumeCard.vue`（新增，409 行） | 单个受管理卷：`desired_state` 与 `observed_state` 分开显示（不一致就是页面要提醒的事）、挂载才有容量读数、没挂载写「没有容量读数」而**不是 `0 B`**、五个动作入口按判决层给的状态渲染 |
| `web/src/components/VolumeSection.vue`（新增，115 行） | 卷区块：读到了给结论（卷 / 「还没有受管的卷」），不停在「正在读取受管卷」 |
| `web/src/components/DeviceStorageActions.vue`（新增，442 行） | 设备四个动作（初始化 GPT / 建分区 / 删未使用分区 / 格式化）的入口：只认载荷里的 `capabilities`，`available` 才可点、`blocked` 显示服务端给的理由、`unsupported` 不出现；载荷里没有 `capabilities` 时**不画任何动作**，只留一块版本不一致的说明（Q205） |
| `web/src/components/StorageActionDialog.vue`（新增，401 行） | 动作确认框：两次请求的编排（`/operations/preflight` → `/jobs`，带 `Idempotency-Key`）、预检 120 s 过期后的重新预检、需要短语的动作用户必须原样敲、需要再认证的动作提示重新输入密码、任务终态用 `jobresult` 翻译 |
| `web/src/views/StorageView.vue`、`StorageDeviceView.vue`、`web/src/components/DeviceDetailPanel.vue`（改） | 列表页挂上卷区块；设备详情页把“当前阶段没有可执行的动作”换成「服务端判定的动作」；动作判定全部来自载荷 |
| `web/src/api/storage.ts`（改） | 设备动作判定与 `capabilities` 的 DTO 对齐本次 openapi |
| `web/src/fixtures/p07-5-vm/`（新增，README + 11 份载荷） | 从真机抓回来的页面载荷：拓扑、整盘详情、分区详情（未挂载/已挂载）、卷列表（空/一个挂着）、卷详情（挂着/没挂）、占用列表（忙/闲）、代理能力表。README 写明为什么用真载荷、每份文件是什么、**别把具体 UUID/pid/字节数写死** |
| `web/src/components/realdata-p07-5.test.ts`（新增，16 条） | 真载荷回放：Q208 回归（未挂载、类型读不到的分区必须仍是候选）、容量只在挂着时存在、占用列表显示真实 pid、动作判定三态、GPT 表与内核分区表按 `number` 对齐、`canAct:false` 不给任何入口 |
| `web/src/api/volumes.test.ts`（新增，22 条）、`storage-write.test.ts`（新增，24 条）、`jobresult.test.ts`（新增）、`realdata.test.ts`/`storage.test.ts`/`realdata-storage-write.test.ts`（改） | 单测与 SSR 回放；把“夹具比现实干净”的地方按真机载荷的两种空形态补齐 |
| `web/scripts/smoke.mjs`（改） | 浏览器冒烟补两条：受管卷区块读到了结果（不停在「正在读取受管卷」）、设备详情页有「服务端判定的动作」 |
| `scripts/storage-external-check.sh`（新增，381 行） | **系统外部独立检查**：不经过面板，只读 `/etc/fstab` + `findmnt` + `/proc/self/mountinfo` + systemd 单元 + 备份文件，判断这台机器现在的状态。支持 `--fstab <副本>` 与 `--expect` 做故障注入；退出码 0 = 一致，1 = 有 FAIL |
| `tests/integration/p07-5-storage-page-acceptance.sh`（新增，1098 行，POSIX sh，root 跑） | 本轮真机验收，11 节（含“外部检查抓不抓得到坏 fstab”的五种坏法，全做在副本上） |
| `docs/STORAGE_RECOVERY.md`（新增，263 行） | 存储恢复文档：机器起不来、卷丢了、fstab 坏了、要退出管理、要回滚备份时人该怎么一步步做；§2 第三步用 `findmnt --mountpoint` 而不是 `--target`（Q204） |
| `api/openapi.yaml`（改，+347 行） | 补齐 P07 的卷接口：`GET /api/v1/storage/volumes`、`GET /api/v1/storage/volumes/{id}`、`GET /api/v1/storage/volumes/{id}/blockers` + 五个 schema（`StorageVolume`、`StorageVolumeDependency`、`StorageVolumeList`、`StorageVolumeBlockers`、`StorageBusyProcess`）；把 `capacity`/`observed_state`/`dependencies` 的空值语义写进 description；版本 `0.0.0-p06.3` → `0.0.0-p07.5`，`stage` → `P07.5` |
| `docs/DECISIONS.md` | Q204（`findmnt --target` 判“挂没挂”）、Q205（载荷里没有 `capabilities` 时页面说什么）、Q206（外部检查的故障注入只动 fstab 副本）、Q207（夹具从真机抓回来回放）、Q208（候选永远是空的：产品缺陷）、Q209（“最后一个卷退出后托管块该消失”是想当然，验收脚本缺陷） |

## 2. 环境

见 `env.txt`。要点：Ubuntu 24.04.5 / 内核 6.8.0-147；util-linux 2.39.3、mke2fs 1.47.0、
sgdisk 1.0.10（只做证据与收尾擦盘）、systemd 255、`nproc=2`；sda 40 G 系统盘（全程只出现在
“被拒绝”的用例里）、sdb 8 G / sdc 4 G 是一次性空盘。代理以 root 运行，**Web 以 uid 999 运行**。
宿主 Node 22.23.3（`nvm`，`scripts/check.sh` 要求钉在 PATH 首位）。

夹具仍然由**产品自己的动作**造出来（GPT → 分区 → ext4），不是 `sgdisk` 手搓。

## 3. 逐节结果（断言数）

| 节 | 覆盖 | PASS | FAIL | NOT RUN |
|---|---|---|---|---|
| 0 | 环境与安全守卫（两块一次性盘都与根不同源、代理是 root、Web 账号存在、工具齐全） | 27 | 0 | 0 |
| 1 | 配置与启动（代理 root、监听 19474 的 Web 不是 root） | 9 | 0 | 0 |
| 2 | 页面依赖的载荷：动作判定与空列表语义（空列表是 `[]` 不是 null；形状不对 400、不存在 404） | 10 | 0 | 0 |
| 3 | 夹具：两块空盘走产品的 GPT → 分区 → ext4（独立读回 GPT 与 UUID） | 39 | 0 | 0 |
| 4 | 导入两个卷：fstab 最小修改、备份、挂载点由 slug 派生、重复导入被拒 | 28 | 0 | 0 |
| 5 | 挂载：容量只在真挂着时有，文件也能真写进去 | 18 | 0 | 0 |
| 6 | 占用列表：说得出是谁占着（页面直接显示这个 pid） | 9 | 0 | 0 |
| 7 | 系统外部独立检查：一条不经过面板的命令把同一批事实再读一遍 | 29 | 0 | 0 |
| 8 | 卸载 → 重挂载：文件一致 | 13 | 0 | 0 |
| 9 | 退出管理：托管块回到干净状态 | 22 | 0 | 0 |
| 10 | 收尾（三条明确不验的路径，逐条写原因） | 1 | 0 | 3 |
| **合计** | | **205** | **0** | **3** |

## 4. 关键断言与读数

**未挂载的分区没有文件系统类型，这是契约不是 bug**（第 3 节）：

```
PASS  分区详情：未挂载时文件系统类型按契约为空（不是猜的）
PASS  分区详情：UUID 只能是内核那个，或者读不到
```

**挂载之后同一块分区就说得出来了**（第 5 节，同一份载荷的两个时刻对照）：

```
PASS  挂载后分区详情的类型是 ext4
PASS  挂载后分区详情能看到挂载点
PASS  页面的卷详情：capacity 不是 null
PASS  页面的卷详情：capacity 的字节数大于 0
PASS  页面的卷详情：没有 observed_error
PASS  文件落在卷的挂载点上（stat 与 findmnt 两条路径一致）
```

真载荷（`payloads/page-volume-mounted.json`）里挂着的卷：

```
capacity = { total_bytes: "2040373248", available_bytes: "1916198912", used_percent: 6.1,
             inode_total: "131072", inode_available: "131061", inode_used_percent: 0,
             collected_at: "2026-10-22T08:55:44.421130275Z" }
```

同一卷卸载之后（`payloads/page-volume-unmounted.json`）：`capacity: null`、`desired_state`
与 `observed_state` 都是 `unmounted` —— 页面在这里写「没有容量读数」，**不写 `0 B`**
（SSR 回放 `realdata-p07-5.test.ts` 钉住）。

**外部独立检查对上同一个现场**（`logs/ext-real.txt`，一个卷挂着、一个没挂）：

```
PASS /srv/upanel/volumes/data: UUID cb0c6b10-… 现在是 /dev/sdb1
PASS /srv/upanel/volumes/data: 现在挂着（findmnt --mountpoint source=/dev/sdb1）
PASS /srv/upanel/volumes/data: --target 也指向它自己（两路读数一致）
PASS /srv/upanel/volumes/data: 内核挂载表 /proc/self/mountinfo 里有它
PASS /srv/upanel/volumes/data: 它是一个真正的挂载点（设备号与父目录不同）
PASS /srv/upanel/volumes/logs: 现在没有挂载
PASS /srv/upanel/volumes/data: ext4 的 fsck 顺序是 2
PASS /srv/upanel/volumes/data: systemd 里有 srv-upanel-volumes-data.mount（LoadState=loaded）
INFO 断言执行 28 项，跳过 0 项
RESULT 一致：这台机器的 fstab 托管块、挂载事实与设备身份能互相对上
```

**五种坏 fstab 都抓得到**（第 7 节，全部做在 `--fstab` 的副本上，`/etc/fstab` 一字节不动；
`logs/ext-fault-*.txt` 各一份）：

```
PASS  外部检查对着真实 fstab 的退出码
PASS  外部检查确认这个卷挂着
PASS  外部检查确认另一个卷没挂
PASS  外部检查确认 UUID 指向的设备
PASS  外部检查确认 fsck 顺序
PASS  外部检查确认 nofail
PASS  外部检查确认 systemd 单元
PASS  外部检查没有 FAIL 行
PASS  退出管理之后外部检查仍然一致
PASS  一个卷都没有时外部检查也是一致
```

**空托管块仍然要通过 `findmnt --verify`**（第 9 节，Q209 修完后的断言）：

```
PASS  托管块还在，但里面已经没有条目   （BEGIN=1 END=1，条目 0）
PASS  并且说得清块里是空的
```

`logs/ext-final.txt`：`INFO 托管块内条目: 0` + `RESULT 一致`。块外的行逐字未变（第 9 节 22 条断言里
有专门的“块外内容一字节不变”对照组）。

## 5. 真机抓出的产品缺陷（改产品，不改断言）

### Q208 — 纳入管理的候选**永远是空的**

P07.5 第一次真机跑，第 4 节之前的候选断言**没红**，因为我根本没在那儿断言候选。真正暴露它的是
把第 3 节的设备详情载荷读出来看：一块刚 `mkfs.ext4` 好、还没挂载的分区，`filesystem.type` 是
`null`（`CONTRACTS` §6.1 第 231 行：未挂载时不调 blkid、不猜；`scan.go` 只在 by-uuid 链接存在时
才填，udev 抢跑时整个 `filesystem` 都是 `null`）。而 `importCandidates` 当时的判据是
「`filesystem` 非 null 且 `type` 非 null」+「已挂载的排除」——**这两个条件互为补集**：

- 一块盘要么**挂着**（被“已挂载”排除），
- 要么**没挂**（读不到类型，被“类型必须已知”排除）。

于是导入表单在任何真机上都是空的。前端单测全绿，因为夹具里的未挂载分区被我手写成了
`type: 'ext4'`——**夹具比现实干净，测试就变成了橡皮章**。

修复后的判据三层：类型**已知且不是 ext4/XFS** ⇒ 排除；类型**未知** ⇒ 仍然列出，界面上如实写
「未挂载时页面读不到类型，类型与 UUID 由服务端在导入前直读确认」；已挂载 ⇒ 排除。

锁住它的测试是按**真机的两种空形态**各钉一条（`filesystem: null` 与 `filesystem.type: null`），
再加一条「ntfs 未挂载 ⇒ 不当候选」。反向验证：把 `importCandidates` 改回旧的两条件写法，
恰好多出两条红——不多不少。

`realdata-p07-5.test.ts` 里另外钉了一条组合断言：真实的未挂载分区载荷 + 真实的整盘载荷一起喂给
`importCandidates`，结果必须**只**包含那块分区。

## 6. 验收脚本自身的缺陷（记录，不掩盖）

### Q204 — `findmnt --target` 判“挂没挂”，每个没挂的挂载点都被读成挂着

外部检查第一版用 `findmnt -n -o SOURCE --target "$mountpoint"` 回答“现在挂了吗”。`--target`
的真实语义是「**哪个挂载覆盖了这个路径**」——它对**任何存在的路径**都返回上一层挂载，于是没挂载的
`/srv/upanel/volumes/logs` 读到 `/dev/sda1`（根文件系统），脚本先判“挂着”，再拿 `/dev/sda1` 与
UUID 对应的 `/dev/sdc1` 比，报出一条假 FAIL。

VM 上两条命令就分清：`--target` 输出 `/dev/sda1`、rc=0；`--mountpoint` 无输出、rc=1。

改法：判据换成 `findmnt --mountpoint`，并加**两个独立证人**——`--target` 返回的 `TARGET` 必须
恰好等于这个挂载点本身（两路读数打架就报错），`/proc/self/mountinfo` 第 5 列里必须真有这个路径。
`docs/STORAGE_RECOVERY.md` §2 第三步同步改掉。

### Q209 — “最后一个卷退出管理后托管块应该消失”是想当然

收尾写了三条断言：`BEGIN`/`END` 标记数应为 0、外部检查应该说“没有面板托管块”。真机上全红：
实际 `BEGIN=1 END=1`、`托管块内条目: 0`、`RESULT 一致`——P07.4 的 `forget` 只删**自己那一行**，
标记刻意保留（下一块盘进来时不用重建），`docs/STORAGE_RECOVERY.md` §3.3 就是这么写的。

**断言和文档打架，先去读文档。** 断言改成“块还在、里面 0 条、块外逐字未变”，外部检查那头断言
`托管块内条目: 0`，顺带钉住“空块仍然要通过 `findmnt --verify`”。

## 7. NOT RUN（逐条原因与替代证据）

| NOT RUN | 原因 | 替代证据 |
|---|---|---|
| 浏览器里的真实渲染 | 本脚本验的是**载荷**（服务端实际发出去的东西）。DOM 由 web 侧 SSR 回放覆盖，浏览器端到端由 `web/scripts/smoke.mjs` 覆盖 | `realdata-p07-5.test.ts`（16 条真载荷回放）+ `check.txt` 里 web-smoke 的 10 条 PASS |
| 开机自动挂载 / 跨重启 | P07.4 已用**一次真重启**证明过（Q195：`srv-upanel-volumes-data.mount` 在重启后 `LoadState=loaded` 且真的挂上），本轮不重复烧一次重启 | `docs/evidence/P07/2026-10-22-p07.4/run-postboot.log`；本轮外部检查在每节里都读 `systemd` 单元状态，与它一致 |
| 九种动作的每一次点击 | 设备动作由 P07.2/P07.3 验收覆盖（各一次完整真机执行），本脚本验的是页面要读的载荷形状与卷的完整生命周期 | `docs/evidence/P07/2026-10-22-p07.2/`、`…-p07.3/` |

三条都写在脚本头的“明确不覆盖”里，不是跑完才想起来的。

## 8. 实际执行的检查

| 检查 | 结果 | 证据 |
|---|---|---|
| 真机验收 `tests/integration/p07-5-storage-page-acceptance.sh`（root，`SPARE_DISKS="sdb sdc"`） | **PASS 205 / FAIL 0 / NOT RUN 3**（EXIT=0） | `run.log`、`payloads/`（58 份真实响应 JSON + 4 份外部检查原始输出）、`logs/`（agent.log、web.log、ext-*.txt、fstab-*.txt、kernel.txt、lsblk-*.txt） |
| 宿主 `scripts/check.sh`（Node 22.23.3 钉在 PATH 首位） | **PASS 13 / FAIL 0 / NOT RUN 0** | `check.txt` |
| `go vet ./...`、`gofmt -l cmd internal` | 干净 | `native-tests.txt` |
| `GOOS=linux go build ./...`（交叉编译） | exit 0 | `native-tests.txt` |
| `go test ./... -count=1` | **26 包 ok**（storage 508、agent 278、api 211，与 P07.4 同口径未变——本轮没动 Go 代码） | `native-tests.txt` |
| web 测试 | **35 文件 / 552 条 全通过** | `check.txt` |
| `api/openapi.yaml` lint（Redocly） | **valid，7 warning**（`no-unused-components` 4、`operation-4xx-response` 2、`operation-2xx-response` 1） | `check.txt` |
| 外部检查脚本自身 | 真机三遍真的 `/etc/fstab`（一挂一空、退出管理后、全空）+ 五种坏副本 + 一份“不该报错的干净副本” | `logs/ext-*.txt` |

**本轮没有改 Go 代码**，所以 Go 侧的包数与条目数与 P07.4 一致是预期的，不是漏跑。

## 9. 已知限制

- `apps`/`shares`/`backups` 三个依赖来源仍未实现，`dependencies` 恒为 `[]`——页面显示“没有面板侧
  依赖”在今天是真的，等那些模块做出来要一起改，openapi 里已按“空数组 = 没有依赖，不是没查”写清。
- 只覆盖 ext4 走完整生命周期；XFS 的导入/挂载仍留到有真实需求时再验（fstab 第六列已按 XFS=0）。
- 卷的挂载点由 `slug` 派生且不可指定；`slug` 只允许小写字母、数字与中划线。
- 占用检查依赖 `/proc`；容器化或隐藏 pid 的环境只能 fail-closed（判为忙），这是有意的。
- 验收只在 24.04.5 做过；22.04.5 上没有任何 P07 证据。
- `go test -race` 仍未在真机跑过（验收 VM 没有 Go/cgo 工具链）。
- 外部检查脚本是**只读证人**，不做修复；它说“不一致”之后怎么办写在 `docs/STORAGE_RECOVERY.md` 里。

## 10. 下一子任务

**P08.1** —— `docs/STAGES.md` §10 的第一项。P07 到此收口：写盘、卷生命周期、页面交互、
openapi、恢复文档、外部独立检查全部有真机证据。
