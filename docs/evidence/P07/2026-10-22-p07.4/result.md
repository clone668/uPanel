# P07.4 验收结果 — 受管理卷（导入、挂载/卸载、UUID 校验、fstab 最小修改、原子替换/备份/回滚、开机自动挂载、繁忙占用列表）

- 日期：2026-10-22
- 范围：`docs/STAGES.md` §9 P07.4 全句——“导入卷、挂载/卸载、UUID 校验、fstab 最小修改、
  原子替换/备份/回滚、开机自动挂载、繁忙占用列表。”
- 结论：**第一段 PASS 267 / FAIL 0 / NOT RUN 0**；**第二段（重启后）见 §4**。
  两段用的是同一份脚本（`sha256 a4144ef5…`，1463 行），中间隔着一次**真重启**。
- 运行日志：`run.log`（第一段）、`run-postboot.log`（第二段）

## 1. 这一轮交付了什么

把“一块已经有文件系统的分区”变成**面板管理的卷**：登记记录 + `/etc/fstab` 里的托管块 +
`/srv/upanel/volumes/<slug>` 上的挂载点，三者由五个动作维护，每一步都要两源核验，
失败要把原文件逐字节留住。写盘（分区、格式化）仍走 P07.2/P07.3 的执行器，本轮不碰。

| 文件 | 改动 |
|---|---|
| `internal/storage/fstab.go`（新增，760 行）+ `fstab_linux.go`/`fstab_other.go` | fstab 的解析与**最小修改**：托管块标记、条目渲染（固定选项顺序、`nofail` 恒在、`noauto` 随 `automount`、第六列 ext4=2/XFS=0）、块外内容逐字保留；`Write` 的顺序是“读 → 比哈希 → 渲染候选 → 临时文件 → fsync → `findmnt --verify --tab <候选>` → 备份 → rename → fsync 目录”，渲染结果与原文相同时直接返回（不写、不留备份），**禁用 `mount -a`** |
| `internal/storage/volume.go`（新增，431 行） | 卷登记记录、`slug` → 挂载点的派生与校验、`desired_state`/`observed_state` 的推导、乐观并发的 `version` |
| `internal/storage/volumelist.go`（新增，180 行）+ `fetch.go`（改） | 卷列表与详情：把“登记记录 + 采集快照”合成 `capacity`/`observed_error`，不挂载就没有 `capacity` |
| `internal/storage/volumeassess.go`（新增，960 行） | 五个动作的判决层：导入前的签名/分区/系统盘/挂载点/占用保护，挂载前的“UUID 必须两源一致”，卸载前的占用必须查得清，改配置/退出管理的 fstab 前置；**放行的预检都绑定身份指纹与状态版本**（Q199 的跨动作不变量） |
| `internal/storage/volumewrite.go`（新增，878 行） | 五个动作的执行器：`import`（写 fstab → 写记录 → 失败用备份换回）、`mount`/`unmount`（固定模板 `mount <dev> <mp>` / `umount <mp>`，完成后 `findmnt` + 拓扑重扫两源核验）、`configure`（改 fstab 那一行）、`forget`（只删自己那一行，标记保留）；`umount` 非 0 时把**当时的占用者**重扫一遍写进错误消息 |
| `internal/storage/volumeholders.go`（新增，69 行）+ `busy.go`（改）+ `rdev_linux.go`/`rdev_other.go`（新增） | 占用扫描合流：`Scan`（fd 的 `st_rdev`，直接开块设备的进程）与 `ScanMount`（挂载点下文件的 `st_dev`，打开着卷里文件的进程）合并去重；`busy` 不再把“挂着但空闲”算成忙；读不到 `/proc` 一律 fail-closed |
| `internal/storage/collector.go`、`capabilities.go`、`http.go`、`assess.go`（改） | 采集器把卷的 `observed_state` 与容量算进快照；能力表登记五个新动作；查询层新增 `NotFoundError`（“卷不存在”直通 404，不伪装成 500） |
| `internal/agent/volumeaction.go`（新增，254 行）+ `storagequery.go`（改）+ `registry.go`、`storageaction.go`（改）+ `cmd/upanel-agent/main.go`（改） | 五个动作的 Decode/Plan/Execute 登记；三个只读查询（列表、详情、占用）；代理启动装配：`受管理卷已接线` 这一行日志是“依赖齐全”的自证 |
| `internal/agentstore/resources.go`（新增，208 行）+ `migrations/agentdb/0004_managed_resources.sql`（52 行） | 代理侧持久化：`managed_volumes` 表（schema version 4）、`SaveVolume`/`DeleteVolume`/`LoadVolumes`，与操作表同一事务边界 |
| `internal/api/storage.go`（改，269 行）+ `router.go`（改） | Web 侧三个只读接口：`GET /storage/volumes`（包一层 `{"volumes":[…]}`）、`GET /storage/volumes/:id`、`GET /storage/volumes/:id/blockers`（admin）；五个写动作**没有**直连入口，统一走 `/operations/preflight` + `/jobs` |
| `docs/CONTRACTS.md` §6.5（新增） | 五个卷动作的参数/再认证/锁/核验来源、fstab 托管块的形状、写路径的每一步、`VolumeBlockers` 字段语义（`busy` 的定义：挂着但空闲不算忙）、两种占用为什么都要查 |
| `tests/integration/p07-4-storage-volume-acceptance.sh`（新增，1463 行） | 本轮真机验收，13 节 + 第二段（重启后） |
| `tests/integration/run-all.sh`（改） | 补 p07-4，并写明“都要写盘，必须串行” |
| `docs/DECISIONS.md` | Q191（占用只看块设备号会漏掉打开卷内文件的进程）、Q192（`busy` 把挂载当忙）、Q193/Q194（夹具：GPT 吃掉盘头 1 MiB；`sgdisk -p` 不打印 GPT）、Q195（开机自动挂载只能靠一次真重启）、Q196（四条明确不验的路径）、Q197（快照等待要盯分区）、Q198（竞态用例的前提是先安静下来）、Q199（configure 预检漏绑定 ⇒ 面板 500）、Q200（“预检后变忙”在受理时就被挡，客户端看到 400）、Q201（要站到执行端只能冻住产品自己的 `umount`）、Q202（重启后 `/run` 是空的）、Q203（`BIN_DIR` 要扛重启且对 Web 账号可执行） |

## 2. 环境

见 `env.txt`。要点：Ubuntu 24.04.5 / 内核 6.8.0-147；util-linux 2.39.3、mke2fs 1.47.0、
sgdisk 1.0.10（只做证据与收尾擦盘）、systemd 255；代理以 root 运行、**Web 以 uid 999 运行**；
sda 系统盘全程只出现在“被拒绝”的用例里，sdb/sdc 是一次性空盘，夹具（GPT → 分区 → ext4）
全部由**产品自己的动作**造出来，不是 `sgdisk` 手搓。

## 3. 逐节结果（断言数）

第一段（`run.log`）：

| 节 | 覆盖 | PASS | FAIL | NOT RUN |
|---|---|---|---|---|
| 0 | 环境与安全守卫（两块一次性盘都与根不同源、代理是 root、Web 账号存在、工具齐全） | 31 | 0 | 0 |
| 1 | 配置与启动（代理 root、监听 19474 的 Web 不是 root、`受管理卷已接线`） | 9 | 0 | 0 |
| 2 | 能力与只读接口：五个卷动作可执行；空列表是 `[]`；形状不对 400、不存在 404 | 13 | 0 | 0 |
| 3 | 夹具：两块盘走产品的 GPT → 分区 → ext4（`blkid` 独立读回 GPT 与 UUID） | 33 | 0 | 0 |
| 4 | 导入卷：fstab 最小修改、备份、原子替换、重复导入被拒 | 57 | 0 | 0 |
| 5 | 挂载：UUID 两源核验 + 真的能写文件 | 26 | 0 | 0 |
| 6 | 卸载 → 重挂载：文件一致 | 16 | 0 | 0 |
| 7 | 繁忙占用列表：说得出是谁占着（含两道门与执行端 EBUSY） | 31 | 0 | 0 |
| 8 | 开机自动挂载：fstab 里那一行的选项 + `findmnt --verify` | 19 | 0 | 0 |
| 9 | 外部编辑 fstab：旧预检不能覆盖它（`PREFLIGHT_STALE`） | 8 | 0 | 0 |
| 10 | 候选校验失败：原文件一个字节都不动 | 17 | 0 | 0 |
| 11 | 回归：第二块盘上的卷不受影响 | 5 | 0 | 0 |
| 12 | 收尾（第一段）：停面板后卷还挂着 | 2 | 0 | 0 |
| **合计** | | **267** | **0** | **0** |

第二段（`run-postboot.log`，真重启之后）：

| 节 | 覆盖 | PASS | FAIL | NOT RUN |
|---|---|---|---|---|
| A | 重启后的现场（**还没启动任何 upanel 进程**）：自己挂上了没有、UUID 对不对、systemd 单元的时间戳、另一个卷有没有误挂 | 9 | 0 | 0 |
| B | 启动面板：Web 账号能执行二进制、面板看到的是同一个事实（`observed_state`/`capacity`/占用） | 11 | 0 | 0 |
| C | 收尾：卸载 + 退出管理（只减一行、标记保留、留备份、之后 404） | 18 | 0 | 0 |
| **合计** | | **38** | **0** | **0** |

## 4. 关键断言与读数

**导入是“最小修改”（§4）**：第一次导入之后 `/etc/fstab` 的非空行**只多出托管块的四行**
（BEGIN、说明行、条目、END），托管块之外的每一个字节逐字未变；条目与期望逐字一致
（`UUID=… /srv/upanel/volumes/data ext4 rw,nodev,nosuid,nofail,x-systemd.device-timeout=30 0 2`）；
`fstab` 的权限与属主没被顺手改掉；候选临时文件 0 个；备份目录多出的一份**内容就是写之前的
`/etc/fstab`**。导入**不把卷挂上**（不改运行状态），`capacity` 因此是 `null`。

**重复导入被拒**：同一 UUID 再导入一次，预检不允许（阻塞码点名“已经在管理”），提交被拒，
而且 `/etc/fstab` 一个字节没动——被拒的提交不产生副作用。

**挂载的核验不是看退出码（§5）**：`checked_via` 是 `["findmnt","topology_scan"]`；结果里的
UUID 与 `blkid -p` 直接读盘得到的 UUID 一致；挂上之后脚本真的往里写文件并读回来。

**卸载 → 重挂载（§6）**：文件内容一致（UUID 相同、`findmnt` 与 `/proc/self/mountinfo` 两处都在），
说明“卸载不是把数据弄丢再挂回来”。

**两道门是两回事（§7）**——这也是原计划判断错的唯一一处：

- 第一层（预检）：有进程打开着卷里的文件时，卸载预检**不允许**，阻塞码 `VOLUME_BUSY`，
  消息里点名 `pid=N`；脚本再到 `/proc/N/fd` 里核对那个 fd 真的指向这个卷。占用者退出后
  占用信息回到“不忙”。
- 第二层之一（**受理时**）：预检放行之后才出现占用者 → 提交得到 **400**，
  `VALIDATION_ERROR`，消息 `当前状态不允许执行：VOLUME_BUSY`；卷仍挂着、`desired_state`
  仍是 `mounted`，并且**留下一条 `status=failed` / `step=rejected` 的任务**（Q159 的纪律）。
  这是代理在受理时重算计划（第二道门）挡下来的，操作对象根本没被创建（Q200）。
- 第二层之二（**执行时**）：冻住产品自己 fork 的 `umount`（`comm=umount`，`SIGSTOP`），
  此刻第二道门已过、内核还没被调用；此时往挂载点里开文件，再解冻 → `umount` 自己吃
  `rc=32 target is busy`，任务 `failed` + `COMMAND_FAILED`，消息里**同时有退出码、真实输出和
  重扫出来的占用者**：

      umount /srv/upanel/volumes/data 退出码 32：umount: /srv/upanel/volumes/data: target is busy.。
      当前占用：挂载点仍然存在：/srv/upanel/volumes/data；有 1 个进程正打开着
      /srv/upanel/volumes/data：6108/sleep。先停掉它们，或把应用改成从挂载点访问

  `desired_state` 没有被改成 `unmounted`——失败不假装成功。

**开机自动挂载（§8 + 第二段 A）**：`automount=true` 之后条目去掉了 `noauto`，`nofail` 与
`x-systemd.device-timeout=30` 保留（一块数据盘不在，不该让机器起不来）；`findmnt --verify --tab
/etc/fstab` 接受现行文件。真重启之后：

- 开机时间 `07:55:24`，`srv-upanel-volumes-data.mount` 的 `ActiveEnterTimestamp` 是 `07:55:50`
  ——**开机时由 fstab 挂上的**，那之后脚本才启动面板；
- 重启后挂载点的 UUID 与登记一致，`/proc/self/mountinfo` 里有它；
- 标记文件 `persist.txt` 的 sha256 与第一段记下的一致；
- **没开自动挂载的第二个卷重启后没有挂上**（`noauto` 是真的生效，不是写着好看）；
- 重启没有改动 `/etc/fstab`（sha256 与第一段结束时的相同），这个 UUID 的条目仍只有一条；
- `/run` 已被清空（没有遗留 socket）——顺带证明第一段不是“面板在维持那个挂载”。

**外部编辑不会被旧任务覆盖（§9）**：预检之后有人在托管块之外手工加了一行，提交得到
**409 `PREFLIGHT_STALE`**，手加的那一行还在，条目数没变——旧任务不会把文件“改回它以为的样子”。

**候选不合格时目标文件一个字节不动（§10）**：托管块之外塞一行解析不了的垃圾 → `findmnt --verify`
报 parse error → 改配置任务 `failed` + `COMMAND_FAILED`，消息含“候选 fstab 未通过校验”；
`/etc/fstab` 与塞坏行之后**逐字节相同**，备份数没变（校验在换文件之前），没有遗留候选临时文件。
清掉坏行之后同样的改动成功——刚才是候选不合格，不是状态被打坏了。

**退出管理只减一行（第二段 C）**：`forget` 之后托管块里没有这条 UUID，但**BEGIN/END 标记还在**
（下一块盘进来时不用重建），备份留下一份，卷详情变 404、列表里没有它，而另一个卷还在。

## 5. 真机抓出的产品缺陷（改产品，不改断言）

只有一条，但它是从 P07.1 就埋着的洞：

**Q199 — `storage.volume.configure` 的预检没有绑定目标身份指纹 ⇒ 面板 500。**
第 8 节第一次跑 configure 时网页侧 `500 INTERNAL_ERROR`，代理日志
`预检结果不合法: action=storage.volume.configure: 缺少目标身份指纹：预检必须绑定一个可复核的目标`。
判决层放行、阻塞项为空、锁也对，就是漏了 `a.IdentityFingerprint = t.obs.IdentityFingerprint`
——`import`/`mount`/`unmount`/`forget` 都有，唯独 configure 漏了，因为此前从没有真机用例跑过它。

修法：`assessVolumeConfigure` 补上绑定；补一条**跨动作不变量**
`TestAssessVolumeActionsBindTargetIdentity`（遍历五个卷动作，凡放行的预检都必须同时有
`IdentityFingerprint` 与 `StateVersion`）。反向验证过：把修复摘掉，只有 configure 那一格红。
CONTRACTS §6.5 把这条写成硬规定。

另外两个缺陷是**验收脚本自己的**（见 §7），修的是脚本，不是产品。

## 6. 为什么“执行端 EBUSY”要单独立一格（Q200/Q201）

原计划只有一格：预检放行 → 制造占用 → 提交 → 期望执行端 `umount` 失败。真机上提交在
**受理时**就被第二道门挡下（400 `VALIDATION_ERROR`）：`SubmitJob` 用同一个判决函数重算计划，
一变忙就拒。这是**好事**——注定失败的操作连 operation 都不会建。

但它把“执行端撞 EBUSY”这条路遮住了。要站到那一刻，只能冻住产品自己 fork 的 `umount`：
提交前起紧循环读 `/proc/<代理>/task/*/children`，看到 `comm=umount` 立刻 `SIGSTOP`。
先用独立探针验证这条路可达（`probe-umount-ebusy.sh` + `measurements-umount-ebusy.txt`，
第一次就抓住），再写进验收脚本；脚本里最多试 3 次，冻错子进程就 `CONT` 重试，
3 次都没抓住记 NOT RUN 而不是伪造通过。

## 7. 验收脚本自身的缺陷（记录，不掩盖）

1. **快照等待盯错了对象**（Q197）：`wait_mounted` 拿整盘判断“挂上了没有”，永远等不到
   （拓扑只给整盘与**已挂载**分区填 `filesystem`）；姊妹函数 `wait_unmounted` 于是**永远瞬间成立**。
   一真一假两条断言同时污染。修法：所有等待都盯**分区**。
2. **竞态用例的前提没做**（Q198）：第一个占用者还活着就去拿预检，拿到的必然是 `VOLUME_BUSY`，
   后面四条连环失败。修法：先杀掉占用者、断言“回到不忙”，再拿预检。
3. **断言表达式写错**（本轮）：查“被受理拒绝的那次留下了任务”时用多行 Python 表达式，
   `jget` 走的是 `exec` 路径，末行一个裸字符串表达式**不会**进 `_`，于是打印空串、断言失败——
   而载荷里明明是对的。修法：显式 `_ = "…"`，并按 `step=="rejected"` 精确挑行。
   教训记在这里：这类“测试工具自己说谎”比产品缺陷更耗时间（P07.3 的 Q190 也是同一种坑）。
4. **重启后 `/run` 是空的**（Q202）：第二段第 1 轮代理拒绝启动，日志原文
   `socket 目录不可用 /run/upanel（应由安装脚本以 root 创建）`。安装形态下由 systemd 单元
   的 `RuntimeDirectory=upanel` 建立，脚本直接跑二进制所以要自己建。
5. **`BIN_DIR` 放错地方**（Q203）：第 2 轮把二进制放进 `/home/upanel/p07-4-bin`，
   而 `/home/upanel` 是 0750，`runuser -u upanel-web` 报 `Permission denied`，表现成
   “代理启动 PASS、Web 起不来”。修法：统一 `/var/tmp/upanel-bin`（0755，也扛得过重启——
   `/tmp` 重启后是空的），并在第二段开头加一条前置断言
   `runuser -u $WEB_USER -- test -x "$WEB"`。

## 8. NOT RUN（逐条原因与替代证据）

| NOT RUN | 原因 | 替代证据 |
|---|---|---|
| 导入后登记记录写失败的回滚（fstab 已替换、`SaveVolume` 失败 → 用备份换回） | 要让代理的 SQLite 正好在“fstab 已换、记录还没写”这一瞬失败，只能 `chattr +i` 数据库或拔盘，两种都会污染后面所有用例的代理状态 | 单测 `TestVolumeImportRollsBackWhenSaveFails`、`…WhenReloadFails` |
| 目录 fsync 失败（rename 已成功、目录没同步） | 在 ext4 上无法稳定制造 | 单测的 `fsyncDir` 接缝用例 |
| `umount` 之后拓扑重扫失败 | 需要采集器同时坏掉 | 单测 |
| 候选校验失败的其它形态（mount 选项写错、字段缺失） | 本脚本用“托管块之外塞一行解析不了的垃圾”触发 `findmnt --verify` 失败，覆盖的是“候选不合格 → 原文件不动”这条**结论**；其它抱怨没有逐一构造 | §10 的 17 条断言 + `internal/storage/fstab_test.go` |
| XFS 卷的导入/挂载（fstab 第六列 pass=0） | P07.3 已验 XFS 格式化；本脚本只用 ext4 走完整生命周期 | `internal/storage/fstab_test.go` 的 XFS 用例 + P07.3 证据 |

这五条**在脚本头的“明确不覆盖”里逐条写着**，不是跑完才想起来的。

## 9. 实际执行的检查

| 检查 | 结果 | 证据 |
|---|---|---|
| 真机验收第一段 `PHASE=main` | **PASS 267 / FAIL 0 / NOT RUN 0**（EXIT=0） | `run.log`、`payloads/`（真实响应）、`logs/`（agent.log、web.log、fstab 各阶段快照、lsblk、findmnt 校验） |
| 真机验收第二段 `PHASE=post-boot`（真重启之后） | **PASS 38 / FAIL 0 / NOT RUN 0**（EXIT=0） | `run-postboot.log` |
| 探针 `probe-umount-ebusy.sh`（先证明执行端可达） | `FROZEN=27445/umount`、任务 `failed`/`COMMAND_FAILED`、消息含 `6108/sleep` 同类 | `measurements-umount-ebusy.txt` |
| 宿主 `scripts/check.sh`（Node 22.23.3 钉在 PATH 首位） | **PASS 13 / FAIL 0 / NOT RUN 0**（web-smoke 10/0/0） | `check.txt` |
| 宿主 `go test ./... -count=1` | **26 包 ok** | `native-tests.txt` |
| `go vet ./...`、`gofmt -l cmd internal` | 干净 | `native-tests.txt` |
| 单测条目数（`-v` 的顶层 + 子测试 PASS 行） | storage **508**、agent 278、api 211、agentstore 90（P07.3 提交同口径 storage 是 328，本轮 **+180**） | `native-tests.txt` |
| `api/openapi.yaml` | valid，7 warning（与上一轮相同，本轮没有改它——见 §10） | `check.txt` |

## 10. 已知限制

- **`api/openapi.yaml` 还没有 P07 的存储写动作与卷接口**（版本仍写 `0.0.0-p06.3`）。
  本轮的契约写在 `docs/CONTRACTS.md` §6.5 里，前端 DTO 也没有跟上——这是 **P07.5 的第一件事**
  （页面完整交互与 openapi 同步），本轮不假装它已经完成。
- 卷的挂载点由 `slug` 派生且不可指定；`slug` 只允许小写字母、数字与中划线。
- 只覆盖 ext4 走完整生命周期；XFS 的导入/挂载留到有真实需求时再验（fstab 第六列已按 XFS=0）。
- `apps`/`shares`/`backups` 三个依赖来源在 P07.4 时尚未实现，恒为 `[]`——页面会显示“没有面板侧依赖”，
  这在今天是真的，等那些模块做出来要一起改。
- 占用检查依赖 `/proc`；容器化或隐藏 pid 的环境下只能 fail-closed（判为忙），这是有意的。
- 验收只在 24.04.5 做过；22.04.5 上没有任何 P07 证据。
- `go test -race` 仍未在真机跑过（验收 VM 没有 Go/cgo 工具链）。

## 11. 下一子任务

**P07.5** —— 磁盘页面的完整交互（导入、挂载/卸载、改配置、退出管理的前端流程与错误呈现）、
`api/openapi.yaml` 补齐 P07 存储接口、并发与恢复文档更新，以及“系统外部独立检查”
（不看面板，直接从 `/etc/fstab`、`findmnt`、`systemctl` 判断这台机器现在的状态）。
本轮留下的现成材料：`payloads/` 里的真实响应可以直接做前端 fixture 回放，`run.log` 里的
每一条错误文案都是页面上该原样呈现的东西。

