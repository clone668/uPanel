P11.3 验收结果（2026-10-23，第 2 轮）
====================================

范围
----
STAGES §13 第 3 项：**重启/关机预检、再认证、倒计时与连接状态；重启 boot_id 确认**。

验收条件（原文）：**关键任务运行时拒绝电源操作；关机界面不伪称已验证**。

这一段要做的产品面：
1. 代理侧新增 `system.power` 动作（`reboot` / `poweroff` 两个操作），走既有的
   预检 → 再认证 → 提交 → 执行 → 审计链路，不自建旁路。
2. 硬条件：**有别的写操作正在执行时，电源操作一律拒绝**，并且拒绝要说清是谁在挡。
3. 倒计时：下发之后先等一段时间（默认 30 秒，可配），期间任务 `running`、命令还没发出去、
   可以取消；取消之后**机器不能重启**（这是"受理 ≠ 发生"的正面证据）。
4. 重启之后必须能回答"那次重启真的发生过吗"——唯一站得住的证据是 `boot_id` 变了；
   在这之前只能说 `accepted`，`verified_at` 必须是空。
5. 关机之后**什么都证明不了**：代理死了，面板看不到电源。所以措辞只能是
   "已下发、连接已断开、最终状态无法确认，需要现场确认"；不允许出现"关机成功"。
6. 前端：电源卡片走写操作对话框（预检 → 展示影响面 → 再认证 → 提交 → 轮询），
   任务详情里多一节 `power_check`（四个状态：confirmed / unchanged / unreachable / unknown）。

**这一段没有新增表、没有新增接口路径**：电源动作复用 `POST /operations/preflight`、
`POST /jobs`、`GET /jobs/{id}`、`POST /jobs/{id}/cancel`；新增的只有
`GET /system/power-state` 的既有契约（P11.1 就有了）与 `job.power_check` 派生字段。

代码清单（两个提交，跑验收时 HEAD = 7d4039e）
---------------------------------------------
abbca9b  功能：电源动作全链路
新增
    internal/power/power.go            377 行  参数与闭集、校验（delay 边界、操作名、禁用短语）、
                                               Assessment/Execution 类型、错误码、锁名
    internal/power/exec.go             351 行  预检与执行：在跑任务闸门、锁、倒计时、sysctl 调用、
                                               boot_id 前后记账、取消
    internal/power/exec_test.go        602 行  注入式（Paths/Run/Now/Sleep/ReadFile/Stat/InFlight），
                                               含本轮真机缺陷的回归测试
    internal/agent/poweraction.go      179 行  `system.power` 的 Action：Decode/Plan/Execute/Policy
    internal/agent/poweraction_test.go 341 行  参数映射、执行前复核把 OperationID 传给 power、
                                               结果形状与 failure reason
    internal/agent/powerdeps.go         53 行  真依赖装配（systemctl、/proc、文件）
    cmd/upanel-agent/power.go           75 行  把 power 执行器接进 registry
    internal/packages/powercheck.go    198 行  **Web 侧**的核对：confirmed/unchanged/unreachable/unknown
                                               与四段措辞（"只能由现场回答"）
    internal/packages/powercheck_test.go 227 行  四种状态与措辞边界
    web/src/api/power.ts               173 行  预检/提交/取消/轮询的电源专用封装
    web/src/components/PowerControlCard.vue   132 行  电源卡片 + 写操作对话框
    web/src/components/PowerCheckPanel.vue     88 行  任务详情里的核对面板
    web/src/api/power.test.ts / components/power.test.ts
    tests/integration/p11-3-power-acceptance.sh 859 行  四阶段真机验收
修改
    api/openapi.yaml                    `job.power_check` 字段、PowerCheck schema、措辞说明
    cmd/upanel-agent/main.go            registry 注册电源依赖
    internal/agent/registry.go          `PlanInput.OperationID`
    internal/agent/operations.go        `revalidate` 带 operationID；接受路径传空串
    internal/agent/runner.go            执行前复核传 `op.ID`
    internal/agent/systemquery.go       power-state 的 boot_id/重启标志来源
    internal/api/jobs.go                单个任务读回时现场核对（列表**不**核对）
    internal/api/packages.go            把核对接到 job 视图
    internal/api/{jobs,packages}_test.go
    web/src/api/jobs.ts / utils/jobresult.ts / utils/jobresult.test.ts
    web/src/views/JobsView.vue / UpdatesView.vue
    web/src/components/StorageActionDialog.vue  写操作对话框按动作放宽超时（电源类更慢）

7d4039e  修复：第 1 轮真机上抓到的"代理被抢 socket"
    internal/agent/server.go             +49 行  `prepareSocketPath` 先探测再决定删不删：
                                               连得上 → 拒绝启动、不碰文件；ECONNREFUSED/
                                               ENOENT → 陈旧，照旧清理；说不清 → 按有人处理
    internal/agent/socketprobe_linux.go   20 行  平台侧错误分类（syscall.ECONNREFUSED/ENOENT）
    internal/agent/socketprobe_other.go   13 行  非 Linux 平台的等价实现（沿用 peercred 的写法）
    internal/agent/server_test.go        +84 行  两个回归测试：活 socket 必须拒绝启动且原监听者
                                               不受影响；陈旧 socket 仍能清理（否则崩一次就起不来）
    tests/integration/p11-3-power-acceptance.sh +42 行  `guard_foreign_units`：外来
                                               `upanel*.service` 停用 + mask（mask 失败则改名挪走），
                                               断言一个都不剩，否则 exit 2；不再 rm -f socket，
                                               陈旧/占用交给代理自己判断

逐节断言（两台 VM 完全相同，按 `== 节标题 ==` 从日志数出，非估算）
------------------------------------------------------------------
    节                                                        22.04.5          24.04.5
    0   这台机器的现场                                        2 / 0 / 0        2 / 0 / 0
    0.1 装配：配置、代理密钥、管理员与 viewer                  7 / 0 / 0        7 / 0 / 0
    1   拒收面：形状不对的电源请求一步都不许走                 11 / 0 / 0       11 / 0 / 0
    2   预检说的是真话（要再认证、不要短语、可取消、锁 global） 11 / 0 / 0       11 / 0 / 0
    3   关键任务运行时拒绝（STAGES §13）                       11 / 0 / 0       11 / 0 / 0
    4   倒计时取消：命令压根没发出去                           8 / 0 / 0        8 / 0 / 0
    5   下发真实重启（本阶段到此为止）                         7 / 0 / 1N       7 / 0 / 1N
    6   重启之后：boot_id 说了什么                             8 / 0 / 0        8 / 0 / 0
    7   任务记录跨过一次重启还在，而且说的是老实话             12 / 0 / 0       12 / 0 / 0
    8   下发关机（本阶段到此为止）                             12 / 0 / 1N      12 / 0 / 1N
    9   关机之后：能说什么、不能说什么                         15 / 0 / 0       15 / 0 / 0
    10  审计：三次下发、一次取消、一次被拒都在账上             4 / 0 / 0        4 / 0 / 0
    11  三条电源任务各自说清了什么                             1 / 0 / 0        1 / 0 / 0
    --------------------------------------------------------------------------------
    合计（`--phase report`）                                  109 / 0 / 2      109 / 0 / 2

§0 这一轮的 2 条 PASS 是 `env-systemd-and-boot-id` 与新增的 `env-no-foreign-units`
（外来 upanel 单元必须不存在——第 1 轮的 6 条 FAIL 就是它们造成的，见下）。

两条 NOT RUN 不是漏跑，是**位置问题**，且都在下一阶段被断言：
    reboot-terminal-state-in-phase-pre        → 阶段二 §7 断言（reboot-after: accepted / 无
                                                verified_at / 命令与退出码 / power_check=confirmed）
    poweroff-terminal-state-in-phase-post     → 阶段三 §9 断言（poweroff-after: accepted / 无
                                                verified_at / "人工开机"提示 / 核对消息拒绝声称）
也就是说：**2 条 NOT RUN 各自对应下一阶段同一事实的 PASS**，不是"少验了两条"。
另外 §7 里有一条 `phase-pre-had-no-failures`：阶段一的失败数写进 `/var/tmp` 跨过一次重启读
回来必须是 0——重启不会让失败"消失"。

真机上真正发生的事（两台各一遍）
--------------------------------
- 22.04 真的重启了：`591a2533` → `5389231a`（阶段二读到时 uptime 79 秒）。
- 24.04 也换了 boot_id：`97057b04` → `22486dd5`（uptime 24 秒），但**那一次是宿主冷启动**
  完成的——QEMU 11.1.0 + WHPX 在这台宿主上无法完成客户机 reset（4 次尝试全挂，
  串口停在 `reboot: Restarting system`，stderr `WHPX: Unexpected VP exit code 4`）。
  guest 已经到达 `reboot.target`（文件系统已 sync）之后宿主才 kill + 冷启动，
  所以 boot_id 变化是真的，**"客户机自己回来"这一环在 24.04 上没有验到**。limitations ②。
- 真的断电了：24.04 关机后 QEMU 进程消失（串口日志结尾 `reboot: Power down`，
  logs/2404-console-poweroff.log），重新上电后 boot_id 又是新的 `813e082b`；
  22.04 同（`4a25a825`）。三个 id 互不相同，所以"重启过一次"与"断电后又上电"分得开。
- 关键任务闸门：先跑一个写任务，再对重启做预检 → `allowed=false`、`blocker` 点出那个
  正在跑的 job id；提交 → **409** + `POWER_JOBS_RUNNING`，机器没重启（boot_id 不变）。
  这不是看返回码，是看"它没动"。
- 取消：倒计时里提交取消 → 任务终态 `canceled`、`outcome` 为空、**没有 power_check**
  （没发生的事没有核对）、boot_id 不变。
- 关机措辞（原文抓自任务详情）：
  「机器已经回来过：boot_id 从 5389231a 变成 4a25a825。这说明那次关机之后它重新启动过；
  但电源当时是否真的切断，面板给不出证据，只能由现场回答。」
  断言同时钉死了"不许说什么"：不含"关机成功"、`outcome != verified`、`verified_at` 为空。

第 1 轮那 6 条 FAIL 的根因（不是"环境抖了一下"，是两个真问题）
--------------------------------------------------------------
第 1 轮 22.04 的阶段二一开始就红了 6 条：`power-state-200` 拿到 503、
`power-state-boot-id` 是 `KeyError: 'data'`、四条 `reboot-power-check-*` 全是 `unreachable`。
顺线查下去：

    Web 日志 18:14:59  代理不可用: dial unix /run/upanel/agent.sock: connect: no such file or directory
    代理日志           18:14:35 起就在 listen，18:15:11 还正常答过一次预检

也就是：**进程活着、socket 没了**。原因是两件事叠在一起：

① 环境：前几轮留下的 `upanel-agent.service` / `upanel-web.service`（`ExecStart` 指向已删除
   的 `/usr/local/lib/upanel/upanel-agent`）在 `Restart=on-failure` 下每 3 秒重生一次
   （`203/EXEC`，`NRestarts=116`）。它们带 `RuntimeDirectory=upanel`，**每停一次就删掉
   /run/upanel**——里正是脚本手启的代理的 socket。
② 产品：`prepareSocketPath` 对已存在的 socket 一律 `os.Remove` 再 bind——"连得上"和
   "连不上"在它眼里一样。于是每个重生周期都在删别人正在用的 socket；抢输的那个随即退出，
   退出时又把路径删了。最后：进程活着，路径没了，面板只能说"代理不可用"。

②是产品缺陷，在 `7d4039e` 里修掉了，判据是"探测"而不是"猜"：连得上→拒绝启动且不碰文件；
`ECONNREFUSED`/`ENOENT`→陈旧，照旧清理；说不清（超时等）→按有人处理，同样不碰。
两个回归测试分别钉住"活的不许抢"和"陈旧的必须能清"（后者少了，代理崩一次就再也起不来）。
①是环境，脚本现在每阶段 `guard_foreign_units` 断言为 0。

第 1 轮的日志曾经用来证明这两个缺陷是真的；**本目录里的日志已整份换成第 2 轮**，
第 1 轮不再作为任何 PASS 的依据。

本轮修掉的产品缺陷：**每次重启都自己拒自己**（功能提交 abbca9b 内）
--------------------------------------------------------------
现象：真机上 `system.power` 的每个任务都停在执行前复核（`stage=failed, step=revalidate`，
`started_at=0`），错误 `POWER_JOBS_RUNNING`；而它前面刚跑的预检是 `allowed=true`。
表面上像"预检放行、提交必失败"，也容易被误读成"关键任务闸门太严"。

根因：`revalidate` 重算计划时不带操作 id，`power.Assess` 固定调 `InFlight("")`，
于是电源操作在"在跑的任务"里撞见了**自己**——闸门的判据本身是对的，
错的是问法：它问的是"有没有写操作在跑"，却没把自己排除掉。

修法（在接缝处修，不动闸门）：
    PlanInput.OperationID → power.AssessRequest.ExcludeOperationID → InFlight(exclude)
预检阶段传空串（那时还没有操作 id，谁在跑都算）；执行前复核传 `op.ID`。
回归测试两层：
    internal/power/exec_test.go   TestAssessExcludesSelfWhenRevalidating
                                  三支——预检拦自己、复核放行自己、别人照样拦
    internal/agent/poweraction_test.go  TestPowerPlanPassesOperationIDToPower
                                  断言 PlanInput 的 id 一路传到 AssessRequest

验收脚本自己抓出的问题（都不是产品缺陷但不修就会产出假证据）
------------------------------------------------------------
① **把 Windows PE 推给 Linux**：`scripts/build.sh` 在宿主上产出的是 PE，推到 VM 上
   `Exec format error`，脚本起不来、日志一行没有。修法：手工构 `bin/linux-amd64/`，
   并在脚本环境自检里加 **ELF magic 守卫**（不是 `7f454c46` 就 exit 2）。
② **倒计时与自己抢命**：30 秒倒计时和脚本 25 秒观察窗挤在一起。修法：倒计时改 60 秒，
   终态断言移到下一阶段并如实记 NOT RUN。
③ **trap 杀掉自己要验的东西**：阶段一/二退出时 `cleanup()` 优雅停掉代理，倒计时里的
   reboot 变成 `interrupted`。修法：这两阶段收尾 `KEEP_SERVICES=1`。
④ **`jget` 的裸变量名让断言假通过**：表达式写成 `boot_before`（只定义了 `d`），
   `NameError` 被吞成 `ERR:...` 字符串，`expect_ne` 于是"通过"。修法：改成 `d["boot_before"]`，
   并加 `require_value()`——读不到就当场 exit 2。
⑤ **列表信封读错**：`data["items"]` → 实际是 `data.jobs`。
⑥ **`fs.protected_regular` 让"日志"变成上一轮的日志**：重定向由外层 upanel 用户执行，
   root 在 sticky 目录里 truncate 不了别人的文件，ssh 立刻退出，而等待器读的是**上一轮**
   同名日志，于是"阶段二 11 条 FAIL"全是假的。修法：日志写进 root 自己的目录，
   并且**先看 mtime 与行数再信内容**。
⑦ **WHPX 下重启挂死**：`WHPX: Unexpected VP exit code 4`。22.04 换 `-Accel tcg`；
   24.04 换不了（tcg 下开不了机），见 limitations ②。
⑧ **§10 的"审计不含明文口令"是假的通过**（最该记的一条）：查询写成
   `SELECT group_concat(redacted_details)`，**漏了 `FROM audit_events`**；SQLite 在空作用域
   里找列直接报 `no such column`，dbq 的 stdout 是空的、stderr 进了日志，`grep` 从空串里
   当然找不到口令 → **PASS**。没查成被当成了清白。修法：补 FROM，并让「查询失败」本身
   判失败；在 22.04 上用坏 SQL / 好 SQL / 含口令列三支证明这个闸门是 fail-closed。
⑨ **外来的 systemd 单元**（第 2 轮新增，见上文根因①）：脚本现在每阶段停用 + mask
   （mask 失败则改名挪走）并断言为 0；不这么做，任何"代理不可用"的结论都不可信。
⑩ **证据文件会被自己的下一次启动覆盖**：`-serial file:` 是 truncate 打开，22.04 的断电
   串口日志就是这么丢的。修法：每次宿主干预之前先 `cp console.log console.<阶段>.log`。
   本轮 24.04 的做到了（logs/2404-console-poweroff.log、2404-console-reboot-hang.log），
   22.04 的没赶上——如实记在 env.txt 环境干预③。

宿主检查（logs/host-check.txt）
------------------------------
`scripts/check.sh` **PASS 13 / FAIL 0 / NOT RUN 0**（go-build / go-vet / go-test /
go-build-linux / go-vet-linux / gofmt / node-version / web-typecheck / web-lint /
web-test / web-build / web-smoke / openapi-lint），web **45 文件 820 用例**全过，
openapi 校验通过（6 条 warning 是既有的：3 未用组件、2 个缺 4xx、1 个缺 2xx）。
这一份是**修完之后**在最终代码树上重跑的。

限制（如实列出，不当成已验）
----------------------------
① **没有浏览器渲染验证**：验收 VM 上没有浏览器。`PowerControlCard` / `PowerCheckPanel` /
   `JobsView` 只有类型检查、组件逻辑单测与真机 JSON 回放，没人在浏览器里点过倒计时按钮。
② **24.04 的客户机热重启没有验到**：QEMU + WHPX 在这台宿主上无法完成 reset（4/4 挂住，
   串口原因见 logs/2404-console-reboot-hang.log）。boot_id 变化是真的，但那一次是宿主
   冷启动完成的。所以"客户机自己重启回来"只在 22.04 上验到；24.04 上的产品断言照跑，
   **环境那一环不许读成通过**。
③ **"电源真的切断"面板证明不了，也不声称**：真机能证明的是"机器回来过且 boot_id 变了"。
   QEMU 进程消失是**宿主侧**的观察，不是产品能力——这正是措辞里说"只能由现场回答"的原因。
④ **关机阶段需要人（或宿主脚本）重新上电**：`--phase final` 无法自证，两条 NOT RUN 的
   根源就在这里。
⑤ **电源操作只有一把全局锁**：`power-global` 串行化是对的，但只验了"有写操作在跑→拒绝"
   与"同一预检只能提交一次"，**没有**验两个管理员同时抢电源时的真实竞争。
⑥ **systemd inhibitor 未验**：如果别的进程（比如 unattended-upgrades、某个 `shutdown -r`）
   挡住重启，`systemctl` 会退出非 0，本轮只验了正常路径的 `exit_code=0`。
⑦ **倒计时边界只验了校验层**：`delay_seconds` 的 0 / 太短 / 太长 / 未知字段 / 多余参数都被
   拒收（§1），但**没有**真的跑一次 1 秒倒计时去看它在真机上的表现。
⑧ **"关键任务"的口径是通用的**：判据是"有没有别的写操作在执行"，不是包管理器/磁盘任务
   单独的特权规则；apt 锁、磁盘任务与电源的关系没有单独验。
⑨ **socket 守卫只验了单机语义**：活 socket 拒绝启动、陈旧 socket 能清，这两个都有单测；
   但"两个代理同时起、谁赢"只在真机上偶然复现过（第 1 轮），没有做成一台机器的专门实验。

结论
----
**P11.3 passed**：两台专用虚机各 **PASS 109 / FAIL 0 / NOT RUN 2**（2 条 NOT RUN 均在下一
阶段被同一事实的 PASS 覆盖），宿主 **PASS 13 / FAIL 0**；关键任务闸门、倒计时取消、
`boot_id` 前后确认、关机措辞四件事都有真机证据。真机抓出并修掉 **2 个产品缺陷**
（电源操作把自己算成关键任务；代理抢走正在服务的 socket）与 **1 条假通过**（审计查询漏
FROM），另有 10 处验收脚本/流程缺陷。24.04 的客户机热重启是本轮唯一没验到的环节，
写在限制②里，不折算成通过。

下一子任务：**P12.1**（restic 本地/SFTP 仓库、秘密引用、SFTP 主机密钥验证、仓库初始化
与容量检测）。
