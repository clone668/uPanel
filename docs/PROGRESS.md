# uPanel 开发进度账本

最后更新：2026-10-30。

**核心验收矩阵 31 条 = 已验 31 / 部分 0 / 未验 0。** 最后一条"部分"（DISK-02 根盘在 LVM/RAID 下）
于 2026-10-29 关掉：在客机里迁出两台根在叠层设备上的 Ubuntu 24.04（LVM `91/0/3`、RAID1 `113/0/3`），
证明靠的是"物理盘自己没有文件系统却被标成系统盘"，不是"预检会拒"；顺带抓出一条写错的锁断言
（Q522，脚本错、产品零改动）。**"一条不剩"不等于"什么都验过了"**——臂 64 端到端、掉电演练、
裸机开机到可用、0.1→1.0 升级、降级/重建中的阵列等仍登记在 `docs/KNOWN_LIMITATIONS.md`。

**P16.2 双平台端到端验收（读数都取在最终交付字节 `3f3b40c1…daf0` 上）：22.04 七相位 155 PASS /
0 FAIL / 3 NOT RUN，24.04 七相位同样是 155 / 0 / 3、逐相位相同（47 / 14 / 14 / 22 / 21 / 23 / 14）；
页面链路另行补测 43 PASS / 0 FAIL**。两个真机缺陷（Q499 22.04 上
`/logs` 全 400、Q500 坏包升级伪成功）都是在真机上抓出来并修掉、在两台上各验一次。24.04 上首轮 `failup`
红过一条，查清是断言自己的采样竞态——`Type=simple` 的单元 `Started` 一打就进 active，而"主进程 exit 1"
要 0.23~0.40 秒后才记账，active 占空比约一成（Q515）；收紧断言（判 socket + 跨一轮重启采两次）后在同一份
字节上重跑得 21/0/0，**判据不是放松而是换成不吃竞态的**，首轮读数留在证据目录的 `run1-pre-tighten/`。
2026-10-27 那轮 24.04 的"开机到 Web active 152s > 120s"属于另一套 QEMU 参数（`tcg,thread=single`），
这一轮用 `thread=multi` 没复现，**因此不能说那条已修好**，裸机读数仍记为未验证。P16.1 那批"只有 `sh -n`"
的部署脚本（安装/升级/卸载/备份/恢复）在这一轮全部在真实 Ubuntu 上跑过。详见 §5 的「P16.2」小节与
`docs/evidence/P16/` 下的证据目录（含两次作废轮、三次换字节重跑与 24.04 那一轮收紧断言前读数的分层说明）。

**P16.4 完成 → P00—P16 全部完成，1.0 的文档集与发布包就位**。六份交付文档（用户手册、恢复手册、
支持矩阵、已知限制、发布说明、证据索引）与重写后的 `README.md`（1.0 入口；原阶段流水账迁到
`docs/DEVLOG.md`）一起进包：`release.sh` 的文档列表 6 → 10 份、**缺一份即 `exit 2`**，
`install.sh` 装机落 **13 个文件**，install 相位新增 7 条断言盯着这件事。**在冻结的发布字节上重跑七相位
155 PASS / 0 FAIL / 3 NOT RUN**（amd64 包 `751a274d…`）。P16.3 那条 fsync 修复（Q505）本轮补了真机读数：
生产写路径在真机 ext4 上 `fsync(目录 fd) = 0`，`strace` 序列在证据里。**掉电语义、0.1→1.0 升级、
arm64 端到端、裸机读数仍是未验证**。详见 §5 的「P16.4」小节与 `docs/evidence/P16/2026-10-28-p16.4/`。

**P16.1 已实现、宿主检查通过、真机已验（由 P16.2 补上）**：`internal/hostcaps` 是唯一的能力探测实现，三个出口
分别是 `upanelctl host-caps`（安装器与"面板起不来"时的排障）、代理
`GET /internal/v1/capabilities` 的 `host_caps`、面板 `GET /api/v1/system/capabilities`（admin，
只读快照，不触发新探测）。四条诚实性规则：只在 `/usr/sbin`、`/sbin`、`/usr/bin`、`/bin` 里找
（**不走 `PATH`**）；每条只有 `present`/`absent`/`unknown` 三态、`unknown` 必须带原因；版本只在
真读到才填；`host_caps: null` 是"还没探完"，**不等于空表**。21 条规格（systemd/journalctl/
docker/nginx/restic/ssh/ufw/nfs/samba/apt/certbot/smartctl/sgdisk/mkfs/findmnt/shadow/sudo…）每条都
带 `feature`（它能支撑面板的哪个功能）与 `hint`。`--line` 是六列 TSV、无表头、空字段表示"没有"、
禁用 `-`/`n/a` 占位符，给 POSIX sh 用（安装器里没有 jq）。第二半是"安装/升级/卸载 + 两库与密钥
备份 + 失败可重入"：`deploy/scripts/backup-state.sh`（**先停服务再拷**——SQLite 的 `-wal`/`-shm`
是活的；`/etc/upanel`（含 `agent.key`）与两库**同一份**备份，丢了密文永久不可解；`.incomplete`
标记区分"跑一半"）、`install.sh` 第 9 步在动库之前自动备份（失败即停，`--no-backup` 才跳过）且
第 3 步的依赖事实改读同一份 `host-caps --line`、`uninstall.sh` 在 `--purge-*` 之前默认先备份
（缺 `backup-state.sh` 即中止）、`release.sh` 打包带上它、`check.sh` 新增 `sh-syntax`。
宿主读数：`sh scripts/check.sh` **PASS 15 / FAIL 0 / NOT RUN 0**（`sh-syntax` 在最终工作树上单跑：
77 个脚本 `sh -n` 全部通过，解析器 `/usr/bin/sh`）、go 全包 test 全绿、前端 **62 文件 / 1175 用例**。
**三个部署脚本在这台 Windows 宿主上一次都跑不起来**，只有 `sh -n` 这一个读数（且这里的 `sh` 是
bash，抓不到 bashism）；备份/恢复/升级在真实 Ubuntu 上未验，能力探测页没有真机读数、没有
Playwright 用例，24.04/22.04 差异未验。

当前事实：**P00—P14 全部 passed**（P14.6 于 2026-10-27 在真实 Ubuntu 22.04 VM 上通过：
`PASS 163 / FAIL 0 / NOT RUN 6`、退出码 0，证据 `docs/evidence/P14/2026-10-27-p14.6/`），
**P15.3 已实现、宿主检查通过、真机验收通过**：2026-10-27 在 VM-A（Ubuntu 22.04.5 / docker 29.1.3 /
compose 2.40.3 / restic 0.12.1）上 `PHASE=main` **229 PASS / 0 FAIL / 1 NOT RUN**，退出码 0，
证据 `docs/evidence/P15/2026-10-27-p15.3/`；唯一 NOT RUN 是夹具盘 `/dev/sdd` 上已有 GPT、按上一轮
夹具复用（`sgdisk`/`mkfs` 三步没重跑，卷的导入/挂载/文件系统 UUID 全部真读过）。真机这一轮**没有
暴露新的产品缺陷**，改掉的 13 条全是验收脚本把断言对齐到产品代码时发现的问题（见 §5 的
「P15.3 真机验收」小节与证据 README）。P15.3（`deployment.update`、
镜像 digest 记录、按选择备份、健康探测与回切）交付 `internal/deploy` 的 `record.go`/`update.go`/
`probe.go` 与代理侧的 `deployment.update` 动作、更新前备份适配层；接线阶段先写测试让缺陷现形，
抓到 **3 个产品缺陷**（Q493）；P15.3 那一轮的 `sh scripts/check.sh` 是 **PASS 14 / FAIL 0 / NOT RUN 0**。
**P15.4 已实现、宿主检查通过、真机验收通过（2026-10-28）**：部署页（`web/src/api/deployments.ts` +
`web/src/views/DeploymentsView.vue`，入口 `/apps/deployments`）只读管理面，没有原始 Compose YAML、
没有危险宿主挂载、没有特权选项——那三样在数据模型里写不出来，所以页面上连输入框都没有；创建/更新/
删除走既有 `/operations/preflight` + `/jobs`，**没有新增写接口**，也没有“直接部署”按钮。宿主读数：
前端 61 文件 / 1161 用例、`vue-tsc` 干净、`vite build` 成功、`sh scripts/check.sh` **PASS 14 / FAIL 0 /
NOT RUN 0**（首跑 gofmt 抓到 5 个未格式化文件，`gofmt -w` 后重跑；`git diff -w` 为空，只有空白改动）。
**真机读数（2026-10-28，Ubuntu 22.04.5 / docker 29.1.3 / compose 2.40.3，43 条断言全过）**：跑之前先修掉一个**阻断项**——三条部署动作都是 R=是，而登录不开再认证窗口，页面只把“这一步要求再认证”写成文案，真机上按确认必然 403 `REAUTH_REQUIRED`，页面链路一次都走不完（Q511）；跑的时候又抓到两处页面缺陷：`StatePanel` 的空状态把页面传的标题丢了、删除成功后还去读那份已删的部署（注定 404）（Q513）。修完在真机上走完 新建 → 预检 → 再认证 → 任务 `verified` → 详情 → 换镜像 →删除，三次任务全部 `outcome: verified`，面板侧 57 条请求全 200/202（`docs/evidence/P15/2026-10-28-p15.4/`）。**仍未验证**：200 份截断 / `skipped` / `config_drift` 三种现场只有单测；“删除没动卷数据”这次只有文案证据（部署没挂卷）；24.04 未跑；任务执行中读现场可能顶到 10 秒预算（Q512、KNOWN_LIMITATIONS C12）。下一个动作是 P16.1（统一能力探测与安装/升级/卸载）。P15.2（systemd 包装单元、挂载依赖、落地与删除、
代理登记表接线）**已实现、宿主检查通过、真机验收通过**：2026-10-27 在 VM-A（Ubuntu 22.04.5）上
`PHASE=main` **199 PASS / 0 FAIL / 2 NOT RUN**、`PHASE=boot` **51 PASS / 0 FAIL / 0 NOT RUN**，
证据 `docs/evidence/P15/2026-10-27-p15.2/`。真机首跑暴露并修掉 **4 个产品缺陷**（Q484 新建预检
缺版本令牌 → 100% 500；带 `deployment_id` 的预检漏填规范化参数 → 500；Q485 apply 落在已有的
active 单元上时 `start` 是空操作、容器一个都没起；Q486 `is-enabled` 的退出码 1 被当成执行失败、
`deployment.remove` 在真机上永远删不掉）与 **5 类验收脚本自身的错误**（Q487 的两条 NOT RUN 理由
也在那里）。P15.1（受限部署定义、校验与 Compose 渲染）**已实现、宿主检查通过**。P14.6 交付 TOTP 二次验证
（设置页登记 → 六位数确认 → 十枚一次性恢复码 → 两步关闭）、两步登录与四个再认证点、
`upanelctl totp-reset` 本机恢复；真机抓到并修掉 **1 个产品缺陷**（Q480：确认用的那个码
还能当登录码再用一次）与 **4 类验收脚本自身的断言错误**（Q481）。P13 交付历史监控（告警规则、持续时间、恢复阈值、
空间双阈值、去重、冷却、维护静默；通知渠道与投递账；确认动作、投递账读接口、历史图表与告警
列表、陈旧与缺失曲线）。P14.1—P14.4 交付网络/路由/UFW 只读与冲突检测、`network.apply` 的
断连自动回滚、`firewall.apply` 的管理通路保护与不确认自动回滚、以及 NFS 受管理导出（详见
§1 与 P14 小节）。
P13.4 两台 Ubuntu VM（24.04、22.04）各 PASS 128 / FAIL 0 / NOT RUN 0，退出码 0；断言名单
跨机逐字一致（归一后 `diff` 无输出），同一份脚本（sha256 `f86fb06f…`）同一组二进制；
宿主检查 14/0/0。P13.2 两台 Ubuntu VM（24.04、22.04）各 PASS 148 / FAIL 0 / NOT RUN 0，
退出码 0；断言名单跨机逐字一致（归一后 `diff` 无输出），同一份脚本（md5 `c74dd469…`）
同一组二进制；宿主检查 14/0/0。P13.1 两台 Ubuntu VM（24.04、22.04）各 PASS 88 / FAIL 0 /
NOT RUN 0，退出码 0；断言名单跨机逐字一致（归一后 `diff` 无输出），同一份脚本（md5
`d08964c5…`）同一组二进制；宿主检查 14/0/0。P12.5 最终两台 Ubuntu VM（24.04 第 12 轮、
22.04 第 2 轮）各 PASS 214 / FAIL 0 / NOT RUN 0，断言名单跨机逐字一致（归一后 `diff` 无输出），
宿主检查 13/0/0（权威状态见 §1 与 §3；下面这段是交付历程摘要，按轮次追加，个别中间轮次
没写进来）。P01.1 交付 Go 最小可运行骨架
（两个服务入口 + CLI、严格 JSON 配置、结构化日志、`/api/v1/health/live`、代理 `capabilities`、
Linux/mock 分离）；P01.2 交付持久层基础（SQLite 打开与 PRAGMA 校验、显式 SQL 迁移与校验和锁定、
事务工具、封闭错误码与统一错误信封、request_id、Gin 路由与中间件组织、`upanelctl migrate`），
并让 Web 在数据库打不开或迁移失败时拒绝启动。两台 Ubuntu 验收 VM 上 P01.2 各 34 项断言、
P01.1 回归各 26 项断言全部通过。P01.3 交付前端骨架（Vue 3 + TS 严格、路由、Naive UI 中文导航、
统一 API 客户端、七状态组件、mock 横幅、未实现模块隐藏），typecheck / lint / 50 个单元用例 /
生产构建 / 真实浏览器冒烟（production 16 项、mock 17 项）全部通过，`scripts/check.sh` 首次
PASS 12 / FAIL 0 / NOT RUN 0。P01.4 把前端产物嵌入 Go Web 二进制（`//go:embed`），补齐安全响应头与
CSP nonce、SPA 回退与未知 API 的 404 边界、production/非回环拒绝、systemd 单元模板与本地启动文档；
两台 VM 上无工具链也能跑，P01.4 验收脚本各 114 项断言全部通过，并真实安装 systemd 单元、
以 `upanel-web` 用户启动服务后停掉清理。
**P02.1 起有了第一块真正的业务能力**：账号、Argon2id 密码哈希、登录/退出、会话轮换、
CSRF、Origin 白名单与登录限速（`users`/`sessions` 两张真实表，`upanelctl init-admin`
创建首个管理员，无默认密码）。宿主冒烟 38 项断言、两台 Ubuntu VM 各 58 项断言全通过。
**P02.2 把权限与账号管理补齐**：admin/viewer 边界、账号增删改查（脱敏 DTO、limit/cursor 分页、
乐观并发 version）、最后可用管理员保护、5 分钟再认证窗口、追加式 `audit_events`（触发器禁止
UPDATE/DELETE）。宿主冒烟 81 项断言、两台 Ubuntu VM 各 99 项断言全通过，P02.1 回归各 58 项仍通过。
**P02.3 让 Web 与特权代理之间第一次有了真实的通道**：Unix socket `/run/upanel/agent.sock`
（`root:upanel-web 0660`，目录 `0755`）、内核 `SO_PEERCRED` 的 UID 判断（普通接口只认
`upanel-web`，root 与其他 UID 一律 403）、固定动作/查询登记表（生产表**故意为空**，
`capabilities` 如实报告 `[]`）、结构化只读查询通道（只认登记过的名字与字段，无 `command` 字段）、
以及命令执行器（绝对程序路径、字符白名单、参数逐字传递、固定环境、超时杀进程组、输出上限）。
两台 Ubuntu VM 各 **84 项断言通过**，并在 VM 内核上原生跑通 28 个代理用例与 9 个命令执行器用例；
P02.2 回归各 99 项仍通过。
**P02.4 补齐审计、日志与秘密三件事**：审计改为**跟随业务事务**（`ctx` 里有事务就写进同一事务，
业务回滚则审计一起消失，被拒绝的请求独立落一条 `outcome=failure`）；日志脱敏做进记录器本身
（键名片段 + 值形状，`password=…` 这类内嵌文本也打码），元数据字段统一改用 `key_path`/
`key_fingerprint`/`ref` 这类不含敏感词的命名。代理侧有了**独立的 root-only SQLite 库**
（`/var/lib/upanel-agent/agent.db`，目录 0700、文件 0600，代理启动时自校验，Web 账号读不到也列不出）
和**秘密加密存储**：AES-256-GCM、每条秘密独立随机 nonce、AAD 绑定引用与用途、密钥在
`/etc/upanel/agent.key`（`root:root 0600`，`upanel-agent -init-key` 生成且绝不覆盖）；未引用的
临时秘密 10 分钟后销毁（密文与 nonce 清零、留墓碑行），被引用即转持久；**没有任何读取明文的
HTTP 路由**。两台 Ubuntu VM 上 P02.4 各 **87 项断言通过**，并在 VM 内核上原生跑通 32 + 45 个
AEAD/到期销毁/权限用例；P02.1/P02.2/P02.3 回归各 58 / 103 / 84 项仍通过。
**P03.1 打开了系统写操作的“前半程”——预检，但一个写动作都还不能执行**：管理员提交动作名与参数，
Web 把它连同身份上下文转给代理，由**代理**生成计划（规范化参数、目标身份指纹、目标状态版本快照、
影响、阻塞项、锁、是否需要再认证与确认短语），Web 只负责保存绑定信息并返回公开 DTO。
预检有效期 120 秒，绑定用户/会话/动作/规范化参数/状态版本；确认短语由服务端生成、只在响应里出现
一次明文，代理库里只有它的哈希。`POST /api/v1/operations/preflight` 已进 `api/openapi.yaml`。
两台 Ubuntu VM 上 P03.1 各 **93 项断言通过**（含“Web 进程不是 root”“短语在库/日志/journal 里搜不到”
“两端 `user_id`/`session_hash`/`params_hash`/`state_version` 一致”），P02.1—P02.4 回归各
58 / 103 / 84 / 87 项仍全绿，VM 内核上原生跑通 agentstore/agent/agentclient/operations/syscmd 测试二进制。
**仍无系统数据采集与任何系统写操作**：没有首页指标、没有磁盘/服务/文件接口；代理的生产登记表为空，
只开放 `capabilities`、`queries` 与 `preflights` 的机制（验收动作只在 `-acceptance-actions` 下存在），
真实只读查询随 P04/P05 逐项登记，真实写动作随 P07 之后逐项登记。
**P03.2 让“接受的任务”第一次真的落地并被追踪**：Web 侧有了持久 jobs（`job_` + `op_` 两个身份、
`Idempotency-Key` → `idempotency_keys` → `jobs.id` 的幂等链、先落库再提交、`preflights.consumed_job_id`
一次性消费），代理侧有了执行记录（`operations`/`operation_events` 只追加、`operation_id` 去重、
进程内资源锁按锁名排序避免死锁、30 秒排队上限）、后台执行器（用 `context.Background` 跑，浏览器
关闭或 Web 掉线都不影响）和取消规则（不可取消的动作没有取消信号，可取消的才在可中断点返回
`canceled`）。三类失败严格分开：代理拒单 → `failed` + 契约码不重试；代理不可达 → 留 `queued` 等
对账、按同一个 `operation_id` 收敛、绝不重发；对账 `NOT_FOUND` → `failed`/`unconfirmed`。
`progress_percent` 是 `NULL` 就不编进度，`cancellation_requested` 不等于 `canceled`。
**两台 Ubuntu VM 上的 P03.2 验收已执行并通过：24.04.5 与 22.04.5 各 通过 101 / 失败 0**
（十次同键提交只执行一次、同键换请求体 409、预检过期与状态变化拒绝、Web 被杀后任务继续、
代理 kill -9 → 重启后 `interrupted` 且不自动重试、不可取消动作无取消信号），
回归 p01-1/p01-2/p01-4/p02-1/p02-2/p02-3/p02-4/p03-1 两台均为 29/34/114/58/103/85/87/93 全绿，
VM 内核原生测试二进制 169 个用例 0 FAIL，二进制 sha256 与宿主交叉编译产物逐字节一致。
宿主 `check.sh` PASS 12 / FAIL 0 / NOT RUN 0。

这一轮 VM 验收抓到了两个**宿主测试没抓到**的真缺陷，都改的是产品而不是断言：
① `CONFIRMATION_MISMATCH` 被 `StatusForCode` 归进 409 组，而 CONTRACTS §4.2、`api/openapi.yaml`
和代理侧都是 400 —— 公开 API 与契约差一个状态码（DECISIONS Q64）；
② SQLite 默认 DEFERRED 事务在 WAL 下“先读快照再升级为写”，**升级失败时 busy handler 不会被调用**，
直接返回 `SQLITE_BUSY`，表现为“任务提交 500”+ 代理日志 `消费预检失败: database is locked`
（DECISIONS Q65）。修法 `_txlock=immediate`，并新增 `TestWithTxQueuesBehindAnotherWriter`——
把修复去掉后该测试确实失败（`database is locked (5) (SQLITE_BUSY)`），说明它真的能拦住回归。
另外修了四份**断言过期**的验收脚本（p01-1、p01-2、p01-4、p03-2）：只按已文档化的现行行为更新，
断言条数只增不减（p01-1 26 → 29，新增 `agent-root-forbidden`）。

**P04.2 把“这台服务器上有什么盘、哪块不能碰”变成了只读事实**：`internal/storage` 走 `/sys/block` +
`/sys/class/block` 枚举整盘/分区/LVM/RAID/加密/loop/rom，边只有 `partition_of` 与 `depends_on`（from=子→to=父）；
设备身份按 `wwid` → `serial` → 型号+容量的强弱顺序算 `sha256:` 指纹并给出 `identity_confidence`（读不到就如实
`medium`，两台 QEMU VM 都是 `medium`）；系统盘保护从 `/proc/self/mountinfo` 与 `/proc/swaps` 出发，
沿边**递归向上**把底层盘一起标记，空白盘不保护、临时挂载的 loop 镜像也不因“被挂载”变成系统盘；
分区表读盘头部只用来补类型与 `free_regions`，与内核不一致时保留内核结果并给 `warning`；挂载容量 30 秒缓存、
`available_bytes` 取 `f_bavail`；`snapshot_version` 只由内容决定（与时间、容量无关），写动作预检才有意义。
代理查询 `storage.topology`，Web 侧 `GET /api/v1/storage/{topology,devices,devices/:id}`（权限 V）只透传，
不重排边、不改保护判定。**没有** `lsblk`/`blkid`/`udevadm` 调用，没有任何写动作，`health` 一律 `unknown`、
`capabilities` 一律 `[]`（SMART 属 P04.3）。
**两台 Ubuntu VM 上的 P04.2 验收已执行并通过：各 通过 108 / 失败 0 / 未执行 0；VM 内核原生 473 用例 0 FAIL；
13 个验收脚本全量回归 1038 断言 0 失败。**
这一轮抓到两个**只有真机才能发现**的缺陷：① `/dev/disk/by-*` 是指向块设备的符号链接，代码用
`os.ReadFile` 读它，等于把整块盘读进内存，代理在 24.04 上启动约 20 秒后被 OOM 杀掉（Q89，夹具里这些条目
是普通文件，永远测不出来）；② `snapshot_version` 把 `collected_at` 和容量也哈希进去，每 5 秒就变，
违反 CONTRACTS §6 的预检语义（Q90）。另外把 by-id 链接目标收紧成“只认本次扫描枚举到的设备”（Q91）。



**P04.3 让“盘还健康吗、现在多忙、那块 NFS 还活着吗”第一次有了真实读数**：`internal/storage` 新增
`smart.go`（`smartctl -j -H -A -i` 一次拿全，**按退出状态位掩码**判读：失效/预失效 → `critical`，
历史位/日志位 → `warning`，命令失败/打开失败 → `unknown`；温度 ≤ 0 当作没有读数给 `null`，绝不显示 0 °C）、
`diskstats.go`（`/proc/diskstats` 每台设备**只取自己那一行**，分区/整盘/dm 从不相加；累计计数转十进制字符串，
速率由相邻两次采样差分，窗口异常或计数变小给 `null`，平均延迟只在完成次数 > 0 时给，扇区固定 512 字节）、
`capacity*.go`（网络文件系统的 `statvfs` 在代理自己的 `--statfs-helper` 子进程里查，并发 2、默认 10 秒超时、
失败后**隔离 5 分钟不再重试**）。健康采集是**独立 goroutine**、默认 15 分钟（可配 5—60），
慢盘只拖住它自己那次探测；**不为读健康唤醒休眠盘**（先读 sysfs 电源状态，`sleeping` 直接跳过，
非 NVMe 再加 `-n standby`）。拓扑新增 `Device.io`、`Mount.capacity_error` 和 `network_mounts`
（无块设备的网络挂载单独成表，`reachable` 三值：true 读到 / false 确定失败 / null 还没查过），
`io` 与网络盘容量**不进** `snapshot_version`（Q100、Q101）。**三台 VM 上的验收已执行并通过**：
24.04.5 与 22.04.5 各 **77** 项、`upanel-smart`（QEMU NVMe）**62** 项，0 失败（未执行项各 1/1/2，均写明原因）；
NVMe 上是真实 `healthy` 且温度 > 0，并与机器自己的 `smartctl -j` 逐盘对账；**把 NFS 服务端停掉、让 `statfs`
挂在内核里时，登录仍然 332 ms / 271 ms 返回 200**，同一时刻该挂载仍是 `reachable=false` 且整个停服期间
最多 1 个 statfs 子进程；5 分钟隔离到期后容量自动回来。原生存量测试二进制 `storage.test` 三台 VM **103/0**。
顺带补上了**长期缺失的竞态证据**：在 22.04 VM 上装了 gcc 与 Go，`go test -race -count=1 ./...` → 19 包 ok、
**0 条 `WARNING: DATA RACE`**，并因此抓到 Q103（`cmd/upanel-web` 的用例会读机器上的 `/etc/upanel/upanel.json`，
测试结果取决于环境——用例自己补空配置，产品代码不改）。**补强轮（第二轮）**按 `STAGES.md` §6
回对夹具清单，补上 QEMU USB 盘的两个真实反例（“桥不认识”“`-d sat` 打不开”）、一条真实 `mount --bind`
的多挂载点用例，并把 `smartctl` 自己的诊断原话透进 `health.details`（Q104）——三台 VM 重跑
**77 / 77 / 63** 项、0 失败，原生 `storage.test` **106/0**（详见 §5「P04.3 补强」）。

**P05（服务、进程、日志）三个子任务全部交付并 passed**：P05.1 服务列表、详情与保护名单，
启停/重启/开机启动走预检 → 任务 → 锁 → 读回；P05.2 进程列表与信号，身份是 pid + starttime，
TERM 与 KILL 是两次独立操作，关键进程零能力；P05.3 journal 结构化查询、级别/单元/数据源筛选、
游标翻页、实时流，以及日志权限、截断与**子进程回收**（结束订阅后 `journalctl` 归零）。三者
在 VM 上分别为 140/0/3、128/0/0、132/0/1。跨阶段全量回归 17 个脚本、1520 项断言：15 个脚本
一次通过，另 2 个的 3 条失败是 `/etc/upanel/upanel.json` 为 0 字节的脏现场，清掉后 run B 全绿
（产品代码未改，Q135）。细节见 §5 的 2026-10-21 三个小节。

**P06.1 打开了文件管理：只读，但边界是真的**：代理在根句柄上用一次 `openat2` 完成路径解析（`RESOLVE_BENEATH|RESOLVE_NO_MAGICLINKS|RESOLVE_NO_XDEV|RESOLVE_NO_SYMLINKS` + `O_NOFOLLOW`），打开后对同一个句柄 `fstat` 核对普通文件 / nlink==1 / dev 与根相同 / 大小上限，版本也取自这次 stat；根的身份（dev/ino + 挂载点 + fstype）在每次请求上重新核对，变了就是 `ROOT_UNAVAILABLE`；配置里**不允许**把 `/` 登记成文件根，所以“能管理什么”在数据上就只有一份根列表。新增 `GET /api/v1/files/roots|list|text|download` 四条 admin 接口，下载是原始字节流（octet-stream + nosniff + no-store + RFC 5987 文件名），失败时回 JSON 信封。VM 真机验收 **PASS=115 / FAIL=0 / NOT RUN=0**、载荷校验 **6379 / 0**；真机抓到 3 个宿主抓不到的缺陷（`O_PATH` 的 fd 上 `getdents64` 是 EBADF、`O_RDONLY` 打开 FIFO 会永久阻塞、符号链接目录被内核报 ENOTDIR 从而被误归成 400），另修了 `next_cursor` 空串与 `ApiErrorPanel` 的 `title` 失效。

**P06.2 让文件管理第一次真的会写**，而且写入路径没有一条旁路：七个写动作
（`file.mkdir`/`file.write_text`/`file.copy`/`file.move`/`file.delete`/`file.chmod`/`file.chown`）
加上单次许可的上传三件套（声明 → 流式 `PUT` → `commit_upload`），全部走
“预检 → 任务 → 幂等 → 资源锁 → 结果读回 → 审计”同一条链路。字节先落在根内的
`.upanel/staging`，声明即预留、收满即归还，**传输期间目标文件一个字节都不动**，
提交时才用 `renameat2` 原子换位；覆盖前留 10 份 / 500 MiB 上限的编辑备份。
版本冲突不是“写失败”，而是预检就拒：外部改过文件后提交 → 409 `PREFLIGHT_STALE`，
内容原样（作业表里留一条 `failed`/`rejected` 可追溯，Q159）。
真机 **PASS=228 / FAIL=0 / NOT RUN=0**、VM 内核原生 **909/0**、前端 **428** 用例、
宿主 `check.sh` PASS 13/0/0。第一次真机跑几乎每条都红，而原因全在产品：`O_PATH` 的 fd 上
`getdents64` 再次 EBADF（连带目录删除、目录复制、备份清理三处，11 次覆盖留下 12 份备份）、
`PARENT_MISSING` 在真实 files 层不可达、`.upanel` 在列举里看得见、预留计数在无容量读取器时
恒为 0 且过期清理不归还（Q155—Q158）。细节见 §5「P06.2」。

**P07（分区、格式化、挂载）五个子任务全部 passed，磁盘是第一条真正高风险的能力**：
P07.1 先把“该拒的都拒”证完（判决层、可靠身份、整盘锁、系统盘/挂载/swap/holders/依赖保护，
四个动作的 `Execute` 为 nil，**一整轮没有写过一个字节**）；P07.2 才接上真的写盘（GPT → 建分区 →
删未使用分区，固定模板经 stdin 通道，写完盘上字节与内核两源比对）；P07.3 接上格式化（ext4/XFS，
写完**直接读超级块**核验，错短语作废预检，取消与中断各有明确语义）；P07.4 把已有文件系统的分区
纳管成受管理卷（fstab 只做最小修改 + 原子替换 + 备份 + `findmnt --verify`，**禁用 `mount -a`**，
开机自动挂载由一次**真重启**证明）；P07.5 把九种动作接上页面、补齐 `api/openapi.yaml`、写下
`docs/STORAGE_RECOVERY.md`，并给这台机器配一条**不经过面板**的独立检查脚本（只读 `/etc/fstab`、
`findmnt`、`mountinfo`、systemd 单元）。五轮真机断言 189 / 177 / 204 / 267+38 / 205，全 0 FAIL；
真机抓出并修掉 **6 个**产品缺陷（Q171/Q172/Q177/Q190/Q199/Q208，含 P07.5 的
“纳入管理的候选永远是空的”）。细节见 §5 的五个小节。

## 1. 当前任务

- 当前阶段：**P16（完整单机版集成与发布）passed，也是 STAGES 的最后一个阶段。**
  1.0 的功能面全部实现并真机验收过：双平台七相位 `155/0/3`（冻结字节 `3f3b40c1…daf0`）、
  浏览器主流程两平台补齐（22.04 35/0/0、24.04 冒烟 35/0/0 + 写链路 43/0/0）、
  `-race` 全量跑完且**产品代码零竞态**（问题全在测试侧，Q517—Q520）、宿主 `sh scripts/check.sh`
  **PASS 15 / FAIL 0 / NOT RUN 0**。
- 本轮工作（2026-10-30，**不是 STAGES 的子任务**）：负责人要求重做界面，做成「像群晕或飞牛那种 NAS
  桌面版」，随后一句「继续，不要停下来，直接全部完成能交付给我测试」。四轮下来：①「桌面式外壳 + 设计
  系统」重写 `web/`；②23 个视图逐页收口，**25 张图全看过、改了 15 处排版缺陷**（按钮塌成 11px、蓝块盖住
  文件名、白带、双标题、看不见的进度轨道……）；③负责人第三次纠正「我说了是那种桌面版，你能理解什么是
  桌面版吗」——判上一版外壳**只是带图标的仪表盘**，重做成**真桌面**：`/desk` 是首页（图标阵列 + 常驻坞），
  打开模块是**在桌面上开一个窗口**（标题栏 / 拖动 / 缩角 / 双击铺满 / 关闭回桌面）；④重新打字节、
  装进 Ubuntu 24.04.5 客机、经 SSH 隧道真登录、**7 张真机截图（真 /proc、229 个 systemd 单元、真块设备）
  `pageerror` 0**。
  **后端、契约、路由表、模块口径零改动**，所以 `3f3b40c1…daf0` 那份交付物与两台 VM 上的读数仍然
  成立（Q510）——但**重新打的包是另一份字节**（amd64 `72997c0f…634a`），拿它做的看图读数只对这份字节有效。
  宿主读数：66 文件 / 1219 用例全过、`vue-tsc` 0、`eslint` 0、`npm run build` 通过、
  `sh scripts/check.sh` **15 / 0 / 0**。
  **这些图仍然不是界面验收**：25 张宿主图是 Chromium 回放夹具，7 张真机图是截图、无断言，31 条矩阵
  一条都没动。**9 个页面在真机上仍是空态**（这台机器没装 samba/nfs、没有任务与备份），空态对、非空态未知。
  见 §5「界面重做（三）」、`docs/evidence/UI/2026-10-30-pages/` 与 `docs/evidence/UI/2026-10-30-desk{,-live}/`。
  下一步：**本地已经没有可做的事**——剩下的每一条都要求真机上的非空数据（装包、建共享、跑备份、改网络），
  在验收 VM 上按 `--install-deps` 补齐后才能看。
- 本轮子任务（2026-10-29）：**核心验收矩阵逐项对照建立 + 三条"部分"逐条去补 + 最后一条"部分"关掉**——
  新增 `docs/ACCEPTANCE_MATRIX.md`，STAGES §19 的 31 条逐行给状态与"目录 + 节"级证据指针，
  45 条指针用脚本核过存在性、0 缺失。建矩阵那一轮**没有产生任何新的真机读数**（索引与定档工作）。
  同一天接着按 `gaps/` 那一轮**补现场**：FILE-02（ENOSPC 真撞、断流、2 GiB 真推）与
  APP-02（真 302 上游 + 金丝雀）从「部分」改成「已验」，读数 `PASS 80 / FAIL 0 / NOT RUN 0`
  （`P16/2026-10-29-matrix/gaps/`，字节绑定见 `gaps/binaries.txt`）。
  最后把 DISK-02 也关掉：在客机里迁出**两台根在叠层设备上的 Ubuntu 24.04**（LVM `91/0/3`、
  RAID1 `113/0/3`，`P16/2026-10-29-matrix/disk02/`），递归保护在"根盘在 LVM/RAID 下"这一层
  第一次有真机读数，顺带抓出一条写错的锁断言（Q522，脚本错、产品零改动）。
  现在合计 **已验 31 / 部分 0 / 未验 0**；PROXY-02 是 10-30 复核出来的**引错证据**，不是补读数。
- **下一步：没有下一个子任务——P00—P16 全部完成，矩阵 31 条一条"部分"不剩。**
  仍然未验证的东西全部登记在 `docs/KNOWN_LIMITATIONS.md`（arm64 端到端、掉电演练、裸机开机到可用、
  0.1→1.0 升级、真并发竞态、降级/重建中的 RAID、根在叠层设备上写满……）。
  **它们不阻塞"1.0 已实现并验收"这个结论，但也不能被读成已验。**
- 上一阶段（保留 P15.3 收尾时的记录原样）：**P15（受约束应用部署与更新）passed——P15.1—P15.4
  全部实现、宿主检查通过、真机验收通过。** P15.3 交付：
  - `record.go`：`<name>.deployment.json`（`0640`，规范化定义 + 镜像记录，只有 `sec_` 引用）、
    `<name>.compose.yaml.prev`（`0600`，上一版原样字节）；`requireRecord` 把"盘上没有"与"有但
    不是合法 JSON"分开报，两种都拒绝更新。
  - `update.go`：`AssessUpdate` 只读预检（记录 → 盘上摘要 → 宿主事实 → 卷依赖 → 渲染 → 备份
    策略）、`ExecuteUpdate` 执行（停单元 → `files_only` 备份 → 拉镜像 → digest 核对 → 写 `.prev`
    → 写新配置并 `restart` → 探测 → 失败回切原样字节并再探）；`UpdateBudget` 一组常量把 10 分钟
    硬线切成段，每段**动手前**判断够不够。
  - `probe.go`：探宿主公开入口（通配地址换 `127.0.0.1`），`http` 判 `200—399`、`tcp` 判能建连，
    不跟随重定向、禁 keep-alive，120 秒预算且 `Truncated` 不算通，启动宽限期内失败不算失败；
    `Checked` 与 `Healthy` 分开（"没探成"≠"探不通"）。
  - `applier.go`：`ApplyRequest.ReplaceExisting`（换掉盘上那份不同内容的配置）与 `MustRun`
    （动词必须真的执行）分开两个字段，失败路径的"起回来"走 `Applier.RestartUnit`（只 restart、
    不碰文件）。
  - 代理侧：`deployment.update` 进动作表（五种 impact、锁含 `backup-policy:`/`backup-repo:`、
    进度步骤 `stopping`/`backup`/`pulling`/`writing`/`probing`）；两个"已回切"的拒绝码翻成
    `rolled_back` **且 error 为 nil**，读回值因此进得了审计；`deploybackup.go` 只收 `files_only`
    策略。`deployment.apply` 同时接上健康探测，`health_probe_pending` 告警删除。
  **已验证（宿主）**：`go test ./internal/agent/ ./internal/deploy/ ./cmd/...` 全绿、
  `sh scripts/check.sh` **PASS 14 / FAIL 0 / NOT RUN 0**。**已验证（真机）**：2026-10-27 VM-A
  （Ubuntu 22.04.5、docker 29.1.3 / compose 2.40.3 / restic 0.12.1）`PHASE=main`
  **229 PASS / 0 FAIL / 1 NOT RUN**、退出码 0（`docs/evidence/P15/2026-10-27-p15.3/`）：
  换镜像后 digest 与 `docker image inspect` 逐字相同、健康探测失败 104 秒后回切成
  `rolled_back` 且 `error` 为空、按选择备份先停单元再备份且快照能被独立 `restic` 读到、
  五种拒绝（`backup_policy_needs_apps` / `stale` / `record_mismatch` / `no_definition_record` /
  `update_not_implemented` + `deployment_exists`）之后盘上配置与容器一个字节没动、拉不到镜像
  失败后服务仍在应答。唯一 NOT RUN 是夹具盘 `/dev/sdd` 已有 GPT、按上一轮复用
  （`sgdisk`/`mkfs` 没重跑）。细节见 §5 的 P15.3 两个小节与 CONTRACTS §8.8、DECISIONS Q488—Q494。
- 上上个子任务：**P15.2（systemd 包装单元、挂载依赖、落地与删除）已实现、宿主检查通过、真机
  验收通过。** P15.2 交付 `internal/deploy` 的三个文件与
  代理侧接线：
  - `unit.go`：确定性包装单元 `upanel-deploy-<name>.service`（`Type=oneshot` +
    `RemainAfterExit=yes`、`ExecStart` = 固定的 `docker compose … up -d --remove-orphans
    --no-build`、`ExecStop` = `down`、`Requires=docker.service`、`[Install]
    WantedBy=multi-user.target`）、逐卷 `RequiresMountsFor` + `BindsTo`/`After`、
    mount unit 名按 `systemd-escape --path` 逐字符实现（段内 `-` → `\x2d`）、
    `ComposeArgv`（argv 里没有可写字符串）。
  - `applier.go`：只碰单元与配置两个路径；临时文件 → fsync → chmod → rename → 目录 fsync；
    配置 0600、单元 0644；**版本 = 配置字节的 `sha256:<hex>`**；同内容重放幂等、异内容
    `ErrDeploymentExists`、单元被改过则整篇重写；成功与否看 `is-enabled`/`is-active` 的读回
    （这两个动词的退出码是答案不是失败）；盘上已有单元而这次又写了新内容时启动动词是
    `restart`（`unit_restarted=true`），因为 `start` 对 active 的 oneshot 是空操作；
    删除先 `disable --now`，仍 active 就 `ErrUnitStillActive` 不删文件，数据卷与业务目录不动。
  - `service.go`：`deployment.apply`（只创建；带 `deployment_id` 或 `version` 的更新路径明确
    返回 `update_not_implemented`）与 `deployment.remove` 的预检/执行两阶段，执行前把预检的
    每一步重跑一遍（秘密轮换、卷卸载、单元漂移都在这里被看见）；版本令牌
    `deploymentStateVersion` 四态非空。
  - 代理侧：`agent/deployaction.go` 把两条动作登记进生产表，`deployfacts.go` 供宿主事实
    （受管理卷 → `VolumeFacts`、文件根按最长挂载点前缀定卷、每次重新问地址表），密钥环解析
    `app_env` 的 `secret_ref`；单元起来之后再按 compose 项目做一次容器交叉核对，读到 0 个容器
    → `partial` 而不是 `verified`。
  **已验证（真机）**：2026-10-27 VM-A（Ubuntu 22.04.5、docker 29.1.3 / compose 2.40.3）
  `PHASE=main` **199 PASS / 0 FAIL / 2 NOT RUN**、`PHASE=boot` **51 PASS / 0 FAIL / 0 NOT RUN**
  （`docs/evidence/P15/2026-10-27-p15.2/`）。STAGES §17 的三条验收（真实部署、卷缺失重启不绕过、
  删除保留数据）通过。**未验证（P15.2 范围外）**：`/deployments` 写接口与页面（P15.4）、24.04
  差异、句柄级路径核对；`deployment.update` 等 P15.3 内容见上一条（已实现，真机已通过）。细节见
  §5 的 P15.2 两个小节与 CONTRACTS §8.7。
- 上一阶段：**P14（网络、NFS、账号、TOTP）已全部 passed**。P14.6（TOTP、一次性恢复码、
  启用确认、CLI 本机恢复与会话失效）**已实现、
  宿主检查通过，并在真实 Ubuntu 22.04 VM 上验收通过**：第二轮（修完 Q480/Q481 之后）
  `PASS 163 / FAIL 0 / NOT RUN 6`、退出码 0（`docs/evidence/P14/2026-10-27-p14.6/`）。
  STAGES §16 的两条硬要求都有真机读数：**TOTP 未确认不启用**（接口报 `totp_enabled=false`、
  库里 `user_totp` 0 行、代理侧种子 `temporary`、无码登录照旧 200、带外 `totp-reset` 明确
  失败并说"没有启用二次验证"、明文种子与 `otpauth://` 在五个库文件里命中 0）；**恢复码只能
  用一次**（造出中间态：停代理 → 用 RC1 关闭失败 503，带外读到 `used_at` 已写 → 起回代理后
  同一枚 401「每枚只能用一次」→ 换 RC2 才成功）。另外读到：码由脚本自己按 RFC 6238 算、
  同一时间步第二次 401「刚刚用过」且不发 Cookie、下一个时间步立刻可用、确认用的码也算用掉
  一步、`totp-reset` 在 root 下 `agent_status=forbidden`（`SO_PEERCRED` 只认 `upanel-web`）
  而在 `upanel-web` 身份下 `destroyed`、审计 actor 是 `local-cli:<who>` 且 `actor_id` 为 NULL。
  真机抓到 **1 个产品缺陷**（Q480：`EnableTotp` 把 `last_step` 写死 -1，确认用的那个码在同
  一个 30 秒窗口里还能登录一次）与 **4 类验收脚本自身的断言错误**（Q481：脚本把偏移加到 ±1
  窗口外面、空串当 needle、`jget` 的失败标记拼进 SQL）。
  P14.5（受管理 Linux 账号 / SSH 公钥 / 受限 sudo 模板）**已实现、宿主检查通过，并在真实
  Ubuntu 22.04 VM 上验收通过**：第三轮 `PASS 168 / FAIL 0 / NOT RUN 6`、退出码 0
  （`docs/evidence/P14/2026-10-27-p14.5/`）。现场读数：`passwd -S` 读回 `P`（口令只经
  `chpasswd` 的 stdin）、uid 由系统分配且落在 1000–59999、`authorized_keys` 里人手写的外来
  钥匙在加/换/删之后逐字节不变、`sudo -l -U` 恰好一条且 `runuser` 换过去真的执行了模板
  命令而模板外命令被 sudo 拒、**真机制造 visudo 失败后旧规则逐字节保住**、禁用是换壳
  （`passwd -S` 仍 `P`）、key-only 账号能登而禁用后同一把钥匙被拒。真机抓到 **2 个产品
  缺陷**（Q477：`Definition` 缺 json 标签，`display_name` 直接 400；Q478：`GET /accounts`
  拿 Samba 能力当账号判据）与 **1 条与真机相反的文档断言**（Q479："没口令就登不上"）。P14.4（NFS 受管理导出：只维护
  `/etc/exports.d/upanel.exports` 一份文件、行尾 `# upanel:<id>` 认领、网段是唯一访问控制、
  只读靠导出行 `ro` + 目录 `0755`、卷掉线时运行时撤销、真实 NFSv3 客户端读写）**已实现、
  单测/宿主检查已过、并在真实 Ubuntu 22.04 VM 上验收通过**：第七轮
  `PASS 141 / FAIL 0 / NOT RUN 6`，退出码 0（`docs/evidence/P14/2026-10-27-p14.4/`）。四条硬性
  现场都成立：`exportfs -v` 的实际导出表里有这条路径且带 `root_squash`；把允许网段换成名单外
  之后**同一台机器**挂不上、改回来立刻又能挂上；只读共享上客户端写入被内核以 `Read-only file
  system` 拒绝且服务端没多出文件；卷从挂载树里摘掉后 **13 秒**内撤销循环把导出行摘掉、
  `runtime_revoked.volume_state=unmounted`、`config_applied=false`，卷挂回来**不自动复活**、
  重新应用才恢复。真机抓到 **2 个产品缺陷**（Q471：0005 的 `protocol IN ('smb')` 让导出写下去了
  却在落库时被 CHECK 挡住，`fakeStore` 完全不跑 SQL 所以单测掩盖了它；Q473：同一个目录的第二条
  共享在预检被放行，`ExportConflicts` 只比"别人的导出文件"这一半）与 **1 条同类未修**
  （SMB 路径在配置生效后落库失败同样不回滚，见下方 P14.4 小节）。P14.3（`firewall.apply` /
  `firewall.confirm` / `firewall.rollback`
  三个写动作 + `GET /firewall/config` 入口状态 + 前端改写卡片 + 独立 systemd 回滚 timer）
  **已实现、单测/宿主检查已过、并在真实 Ubuntu 22.04 VM 上验收通过**：第三轮
  `PASS 153 / FAIL 0 / NOT RUN 4`，退出码 0（`docs/evidence/P14/2026-10-27-p14.3/`）。两条硬性
  用例都成立：**挡住管理通路会被拦在预检**（要启用、但规则集里没有 `from any` 的 22/tcp 放行 →
  `allowed=false` + `FIREWALL_MANAGEMENT_PATH`，`ufw status` 仍 inactive、`show added` 仍
  `(None)`；停用状态下同一份定义不拦）；**改完不确认会自己退回来**（apply 后停掉 Web 与代理 →
  60 秒时新规则仍在、timer 仍武装 → `deadline` 后 systemd 拉起
  `upanel-fwrollback@<token>.service` → 规则换回旧集、`ufw` 仍 active，journal 里动手的是
  `upanel-agent[162295]` 不是 Web）。规则身份靠 comment `upanel:<id>`，只加缺的、删多的，
  **手写的 `ufw allow 9000/tcp` 跨两次 apply 原样保留**；`-firewall-writes` 与
  `-network-writes` 分开、默认都关。真机抓到 **1 个产品缺陷**（Q464：三个 `Assess` 的拒绝分支
  没带 `Normalized`，`validatePlanResult` 判"预检结果不合法"，于是"你没有待确认的改动"被包装成
  500）与 **1 处契约违反**（`rules` 在 ufw 读不到时会序列化成 `null`，违反自家 OpenAPI 的
  `array`、前端 `.map` 直接崩），另有 **5 条验收方法修正**（Q465，最险的一条：看门狗用
  `grep -qi active` 会匹配 `inactive`，把"未启用"永远读成"已启用"）。P14.2（`network.apply` /
  `network.confirm` / `network.rollback` 三个写动作 + `GET /network/config` 入口状态 +
  前端改写卡片 + 独立 systemd 回滚 timer）**已实现、单测/宿主检查已过、并在真实 Ubuntu 22.04 VM
  上验收通过**：第五轮 `PASS 186 / FAIL 0 / NOT RUN 3`，退出码 0
  （`docs/evidence/P14/2026-10-26-p14.2/`）。核心现场：把 `ens4` 改成不可达的
  `10.99.99.99/24`（`outcome=verified`，旧地址与默认路由都消失）→ **停掉 Web 与代理**
  → 60 秒时仍在不可达地址（窗口没被提前咬掉）→ `deadline+28s` 时地址自己回到
  `10.0.2.15/24`，记录变 `rolled_back`，拉起回滚的是 systemd
  （`upanel-netrollback@<token>.service`，journal 里有 `Starting …` / `upanel-agent[pid]: 回滚 … 已处理`
  / `Finished …`），两条救命看门狗都写了 `not-needed`。**STAGES §16 的硬条件因此解除。**
  写路径默认**仍是关闭**，但理由换了（Q458）：不再因为"没验过"，而是因为"改网卡等于把机器从
  网络上摘掉，这个权力该在装机时由人给"（P16.1 服务单元 + P16.4 手册），验收不该顺手替所有
  既有部署改掉一个安全默认值。本轮实现要点：执行顺序是**渲染 →
  `netplan generate` → 先武装 timer → `netplan apply` → 读回内核事实**（顺序即安全：timer 装不上
  就不改，apply 失败或核验不符立刻按记录回滚，不等 timer）；outcome 闭集 `verified`/
  `rolled_back`/`unknown`，**没有 `failed`**——"跑完了但改动没留下"与"失败"是两件事；
  `network.confirm` 的 `arrived_on` 由服务端从连接里取（`http.LocalAddrContextKey`，去 IPv6
  zone 后 `Unmap()`，所以 `::ffff:192.168.1.50` 与 `192.168.1.50` 是同一个地址），调用方给的一律
  忽略——这是"旧地址不能绕过新地址验证"的实现方式；回滚记录落成
  `/var/lib/upanel-agent/network/rollback/<token>.json`（root 0600），不是面板数据库。
  宿主检查第一轮还撞到一个**与本轮无关但真实的偶发缺陷**（Q446：`readAtTimeout` 对已取消的
  context 抛硬币，`internal/storage` 有一条测试在全量并行时红），已按"已取消就不读盘"修掉。
  P14.1 两台专用
  Ubuntu VM 最终各 **PASS / FAIL 0 / NOT RUN 0**（22.04 112、24.04 116），退出码均 0；同一份
  脚本（sha256 `147b7a93…`）同一组二进制；两台差 4 条已核实为 24.04 多一张 `docker0` 网桥
  （§3 每卡 5 条 sysfs 对照多 5 条、§4 走 `editable=false` 分支少 1 条 target 对照），不是少验。
  **P14.1（网卡与地址、路由与默认网关、监听端口与持有者、防火墙管理者与规则四条只读端点，
  外加"这份现状能不能被本面板安全地改"的判定）已实现且已验**：四条端点全部 admin 且全部只读，
  **写入口一条都没开**（验收里 POST 四条同路径与 `/network/apply` 必须 404/405）；查不到不等于
  没有——`Issue{source,code,detail}` 的 code 是闭集 missing/permission_denied/timeout/
  unparsable/failed，`ip` 整个不可用时仍从 sysfs 列网卡并另记一条 `ip/missing`，而不是退化成
  空表；不猜——类型看 sysfs（`br0` 名字里有 br 但没有 bridge 目录就是 ethernet）、配置归属看
  `/run` 下的渲染产物，不解析 netplan YAML；只读也不许慢——总 8 秒、命令 6 秒，超时如实记
  `timeout` 并让其余来源照常产出。`editable=true` 只在 netplan+networkd、无 Wi-Fi/网桥/bond/
  VLAN、物理以太网卡恰好一张、`/run/NetworkManager` 不存在时才成立，`false` 必带 `reasons`
  （`unknown` 与 `none` 不合并）；审计**只记被拒的读**——成功读不落库，否则每刷一次页面就四条。
  真机撞到并修掉 **2 个产品缺陷**（Q438 主机路由的 `destination` 不带前缀导致同一字段两种写法、
  Q439 每台 Ubuntu 都带的 `/etc/nftables.conf` 被判成"ufw 与 nftables 同存"会让 P14.3 在标准
  机器上永远打不开），另修正 **6 处验收方法**（Q440 runuser 壳与它 fork 的子进程、Q441 回环是
  127.0.0.0/8、Q442 比结构一律走 `json.dumps`、Q443 截断响应找键、Q444 读不到时的 `0 == 0`
  假绿、Q445 24.04 的 `ssh.socket` 让 22 端口持有者是 systemd）。**两台恰好走到判定的两个
  分支**：22.04 `editable=true, target=ens4`，24.04 因遗留 `docker0` 网桥 `editable=false`
  且理由点名网桥。见第 5 节 P14.1 小节与 `docs/evidence/P14/2026-10-26-p14.1/result.md`。
  P13.4 两台专用
  Ubuntu VM 最终各 **PASS 128 / FAIL 0 / NOT RUN 0**，退出码均 0；逐节断言数
  （7/9/14/14/20/31/12/8/10/3）与断言名单跨机逐字一致（归一后 `diff` 无输出），同一份脚本
  （sha256 `f86fb06f…`）同一组二进制；宿主最终检查 **14/0/0**。**P13.4（确认动作、投递账读
  接口、历史图表与告警列表、陈旧与缺失曲线）已实现且已验**：确认只写"谁知道了"——`acked_at`/
  `acked_by` 从不碰状态机，`state` 仍是 `firing`，第二个管理员再确认时时间与人都不变（幂等 +
  `WHERE ... AND acked_at IS NULL` 首告者胜）；投递账读路径是顶层
  `GET /notification-deliveries`（不挂通道下，因为一行账带的是**投递当时**的通道快照，且收件箱
  要的是跨通道排序），行形状与通道端点回执共用同一个 DTO；历史曲线的三条展示口径写进契约
  （缺失不是 0 / 没点不画轴 / null 显示"—"）；"陈旧"与"没开"分开说——`enabled` 说的是这个进程
  答不答得了历史问题，`collecting` 才是在不在记，`enabled=false` 只剩"没有库"一种来路。
  验收侧撞到并修掉 3 个问题，**全部在脚本这一侧**（Q429 20 秒新鲜度判据拿宿主 `now` 去量在
  TCG 单线程上假红、Q430 把 `enabled` 与 `collecting` 当成一个意思红了 4 条、Q431 另五处装配与
  判据缺陷），产品缺陷 0 个。见第 5 节 P13.4 小节和
  `docs/evidence/P13/2026-10-26-p13.4/result.md`。P13.3 两台专用 Ubuntu VM 最终各
  **PASS 211 / FAIL 0 / NOT RUN 0**，
  退出码均 0；逐节断言数（13/11/48/12/44/34/35/14）与断言名单跨机逐字一致（只有两行量到
  的数值不同，且都是 PASS）。**P13.3（通知通道、投递账、由代理发送）已实现且已验**：
  发送发生在**代理**里——面板只知道"发到哪、说什么"，从头到尾拿不到明文，因为要发就得给
  面板开一个"把 `secret_ref` 解成明文"的接口，而那会把所有用途的秘密（备份仓库口令、
  TLS 私钥、SFTP 私钥）一起漏出去；`secret_ref` 的用途由**代理自己**核对，否则"不能读
  明文"被绕成"把明文发给我"。投递失败一律回 **200**（`accepted`/`status`/`detail`/
  `retryable`），因为"代理按你的要求发了、被对端拒绝"与"代理没理你"是两件事；`retryable`
  只有代理能判（它才知道那是 4xx 临时拒绝、超时还是 5xx 永久错误）。投递入队与告警事件
  **同一笔事务**，静默事件一条都不入队，`(event_id, channel_id, kind)` 部分唯一索引去重，
  退避 `30s × 2^(n-1)`、10 分钟封顶、`max_attempts` 建在通道上（默认 3、上限 10）；
  通道被停用/删除时 pending 判 `failed` 且**不计入 attempts**（一次都没发出去的东西不该
  看起来像试过）。验收侧撞到并修掉 3 个问题，**全部在脚本这一侧**（Q424 接收器的 TLS 升级
  在 Python 3.12 上必挂、Q425 退避不能用"我们到达时还剩多少秒"来量、Q426 空版本号拼进
  PATCH 的 body 把产品合法的 400 读成了"停用没生效"），产品缺陷 0 个。见第 5 节 P13.3 小节。
  P13.2 两台专用 Ubuntu VM 最终各 **PASS 148 / FAIL 0 / NOT RUN 0**，退出码均 0；逐节断言数
  （7/9/48/63/9/8/4）与断言名单跨机逐字一致，宿主最终检查 **14/0/0**。
  **P13.2（告警规则）已实现且已验**：见第 5 节 P13.2 小节。
  P13.1 两台专用 Ubuntu VM 最终各
  **PASS 88 / FAIL 0 / NOT RUN 0**，退出码均 0；逐节断言数与断言名单跨机逐字一致，宿主最终检查
  **14/0/0**。**P13.1（历史监控）已实现且已验**：见第 5 节 P13.1 小节。
  **P12.5（证书续期 timer 接入、备份页与计划任务页）已实现且已验**：P12.5 两台专用 Ubuntu VM
  最终各 **PASS 214 / FAIL 0 / NOT RUN 0**，退出码均 0；逐节断言数与断言名单跨机逐字一致，
  宿主最终检查 **13/0/0**。见第 5 节 P12.5 小节。
  **P12.4（受管理 timer、时区/错过执行/不重叠/超时、内置动作复用任务框架、非 root 脚本）
  已实现且已验**：计划与脚本只存在代理库，Web 侧是代理镜像直读（代理不在就 503
  `AGENT_UNAVAILABLE`，不回空列表）；单元文件把时区、`Persistent`、最长运行、目标服务写死，
  `systemd-analyze calendar` 算下次到期、`systemctl show` 读 systemd 的原话，
  `installed`/`unit_file_state`/`active_state` 三种事实分开写。**"不重叠"靠
  `schedule-trigger --wait` 把单元撑到操作终态**（systemd 的既有语义因此变成产品承诺），
  触发放行只认 root、只接受已批准的定义、槽位折到 UTC 分钟、`UNIQUE(schedule_id, slot_at)`
  保证重放不新增操作；定时操作每 30 秒被对账导入任务表，行为面板里的惰性"系统操作者"
  （`role=viewer`、`disabled=1`、登录回答与"没有这个账号"同码同文案）。脚本落盘
  `root:upanel-shell 0750`、解释器固定 `/bin/sh`、运行身份 `upanel-shell`（不在
  sudo/docker/disk 组）、运行前核对 SHA-256、被计划引用时删不掉（`SCRIPT_IN_USE`）。
  真机抓出并修掉 9 个产品缺陷（Q371 只写 `Type=oneshot` 就声称不重叠、Q374 解释器基名与
  契约不符、Q375 `systemd-analyze calendar` 输出形状随时区变、Q376 脚本目录上级不可穿、
  Q377 库约束不在预检说话、Q380 root 的 `--wait` 读不到自己要等的操作、Q381 导入即终态的
  `needs_sync` 永不为 0、Q382 `busy_timeout` 5 秒仍 `database is locked`、Q385 一条列表读
  10144 毫秒压过 Web 侧 10 秒上限）；验收方法修正 9 条（Q370、Q372、Q373、Q378、Q379、
  Q383、Q384、Q386、Q387）。

  **P12.3（`backup.restore` / `backup.verify` / `backup.prune`：恢复到新目录、覆盖恢复、
  恢复验证、显式保留清理与失败处理）已实现且已验**：恢复先证明"目标是谁"（执行前重比
  身份指纹与版本，不通过就 `DEVICE_IDENTITY_CHANGED`）；默认不覆盖，覆盖不是"先删再写"——
  同父目录随机暂存树 → `RENAME_EXCHANGE` / `RENAME_NOREPLACE` → 发布后重新遍历核对 → 才删
  旧树；写满、断连、restic 非零退出、抽样不一致都在"换"之前失败，暂存树清掉、原目标不动、
  被停的应用照样拉起。`backup.verify` 两态自我说明（`sample_restore=false` 只做结构检查，
  结果里 `restore_verified=False`）；`backup.prune` 预检冻结 `expected_delete_ids`，集合一变
  就 `PREFLIGHT_STALE` 且不删任何东西，清理后重新读 SFTP 仓库证明"只动了指定仓库"。
  真机抓出并修掉 4 个产品缺陷（Q356 借用 fd 被 GC 关掉、Q357 会话滑动但 Cookie 不续发、
  Q359 仓库失效锁让 verify/prune 永久失败、Q360/Q363 失败消息只剩"有 10 个错误"且补原因
  只认 `error:`），另修正 5 条验收方法（Q355/Q358/Q361/Q362/Q364，含"并发验收会把失败判给
  产品"与"gofmt 未清零就不构建、部署逐文件核哈希"）。宿主 13/0/0，node 按 `.nvmrc` 用
  v22.23.3。证据见 `docs/evidence/P12/2026-10-24-p12.3/result.md`。
  **P12.2（策略、运行、快照列表、文件一致性说明、停受管理应用备份、面板 SQLite 在线备份）
  已实现**：仓库与策略在面板侧只有**一份真相**——代理里的镜像，`/backup/*` 整组是它的编辑器
  （代理不在时整组 503，不搞"先登记回头同步"）；`backup.run` 的参数按契约是
  `backup_policy_id` + `version`，`apps` 由面板**展开**（systemd 单元 / 容器 id），调用方自己
  给 apps 直接 400，代理侧再双向核对（多给 = 扩大授权、少给 = 停机不全，都拒）；一致性口径
  闭集 `files_only` / `stop_managed_apps`，`stop_managed_apps` 少解析一个应用就整轮阻塞
  （`BACKUP_APP_UNAVAILABLE` 点名是哪一个），compose 部署不降级、回 422 并说明理由；执行顺序
  固定为 读现状 → 停（只认"刚才真在跑的"）→ 面板库在线副本 → `restic backup` → `restic stats`
  回读快照 id → 拉起（失败路径也用 `WithoutCancel` 拉起）→ 清临时副本；面板库副本用
  `VACUUM INTO` + `mode=ro`（不是 `os.Copy`，`-wal` 里的已提交事务不能丢），副本 0700/0600、
  跑完即删、只在 `observed_state` 留摘要；快照列表**不带大小**（`size_included:false`，
  `size_bytes` 字段不出现而不是写 0），`registered` / `found` 是状态字段不是 404。
  宿主验证：`go build ./...`、`go vet ./...` 无输出，`go test ./...` 全包通过（`internal/api`
  117.6 秒），`redocly lint api/openapi.yaml` 有效（6 条警告全部是改动前就有的）；
  新增/改动的用例：`internal/api` 14 条、`internal/agent` 的 `backuprun_test.go` 3 条、
  `internal/backup` 的 `policy_test.go` / `run_test.go` / `paneldb_test.go`、
  `internal/agentstore` 的 `policystore_test.go`。真机又补齐 SFTP 写快照、断网、独立 tmpfs
  存储满和运行中取消：失败场景不只核对任务终态，也核对源哈希不变、旧快照仍可读；取消为
  `canceled`，断网/写满为 `failed + COMMAND_FAILED`。最终两台均 170/0/3，三条 NOT RUN
  仅产品恢复、保留清理、浏览器 UI，分别属于 P12.3/P12.5。真机抓出并修掉 4 个产品缺陷
  （Q343 短快照 id、Q344 认证写超时、Q345 进度堵管道、Q347 上层客户端提前取消慢查询），
  证据见 `docs/evidence/P12/2026-10-24-p12.2/result.md`。
  **P12.1（restic 本地/SFTP 仓库、秘密引用、SFTP 主机密钥验证、仓库初始化与容量检测）
  passed**：这一段只做"仓库"这一层——仓库真的建起来了（`restic cat config` 读回的 64 位
  id 与脚本自己再读一次**逐字节相同**）、只建新仓库不覆盖已有数据、主机密钥由我们自己
  核对（指纹只认登记的那把，known_hosts 核对通过才写 0600）、秘密按用途把关、容量读不到
  就是 `supported=false` 而不是 0、口令错要说得出口令不对、root 碰镜像路由直接 403。
  两台 VM 各 **PASS 133 / FAIL 0 / NOT RUN 5**、`EXIT=0`，逐节断言数两台完全相同；仓库 id
  22.04 `5b2ac62d…`、24.04 `6044228b…`，两台都独立核对过。真机抓出并修掉 **3 个产品缺陷**：
  ①主机密钥协商按服务器偏好先递 ECDSA，登记的是 ed25519 指纹 → 正确的那份也报不匹配
  （改成不匹配时按算法逐个重握手，只有指纹相等才算通过）；②一次握手读超时（15 秒）就把
  "连不上"定死，而同机手动重放 6 次全在数百毫秒内连上（改成网络层失败整轮再来一次，
  两轮封顶）；③只读查询的 30 秒预算耗尽被写成"restic 以退出状态 -1 结束"——把"我们自己
  收的工"说成"restic 崩了"，且那台慢机器上整串动作本来就要 30 秒（分出
  `ErrBudgetExhausted`、预算提到 45 秒）。另有 3 条假绿与 3 条脚本缺陷被脚本自己抓出。
  宿主 `check.sh` PASS 13 / FAIL 0（web 45 文件 820 用例）。**备份/恢复本身是 P12.2/P12.3**：
  BACKUP-01 的"备份后恢复"本轮没有对象可验，写进 NOT RUN。
  **P11.3（重启/关机：预检、再认证、倒计时与连接状态、`boot_id` 确认）passed**：电源是唯一
  “执行者与被观察者是同一个”的动作——命令交出去，代理就随机器一起消失，所以全部功夫在
  “能说什么、不能说什么”。结果只到 `accepted`，`verified_at` 为空；唯一站得住的证据是
  `boot_id` 变了（`confirmed` / `unchanged` / `unreachable` / `unknown` 四个闭集，没有第五个），
  关机则只承认“回来过”，电源是否真切断**只能由现场回答**（断言同时钉死“不许出现关机成功”）。
  关键任务闸门：有写操作在跑 → 预检 `allowed=false` + 点名 blocker、提交 409
  `POWER_JOBS_RUNNING`、机器不动；倒计时取消 → `canceled`、无 `power_check`、`boot_id` 不变
  （“受理 ≠ 发生”的正面证据）。两台专用虚机各 **PASS 109 / FAIL 0 / NOT RUN 2**（多出的一条是
  新增的 `env-no-foreign-units`；两条 NOT RUN 是“脚本与被测机器同一台”的位置问题，各自在
  下一阶段被同一事实的 PASS 覆盖），逐节断言数两台完全相同；真机 `boot_id` 22.04
  `591a2533→5389231a→4a25a825`、24.04 `97057b04→22486dd5→813e082b`（三个 id 互不相同，
  才分得开“重启一次”与“断电后又上电”）。
  真机抓出并修掉 **2 个产品缺陷**：①电源操作把自己算成在跑的关键任务，每个 `system.power`
  都停在 `revalidate` 报 `POWER_JOBS_RUNNING`——修法是把 `OperationID` 从 `PlanInput` 递到
  `InFlight(exclude)`，闸门判据一字未动；②代理起服时无条件 `os.Remove` 已存在的 socket，
  把**正在服务**的那个代理的路径删掉（第 2 轮 22.04 上 6 条 503 的根因），修法是先探测再决定
  ——连得上就拒绝启动且不碰文件，`ECONNREFUSED`/`ENOENT` 才算陈旧，说不清按有人处理。
  另有 **1 条假绿**：审计查询漏了 `FROM audit_events`，
  SQLite 报错被吞、`grep` 从空串里找不到口令就 PASS（“没查成 ≠ 清白”），已改成查询失败也算
  失败并重跑阶段三。宿主 `check.sh` **PASS 13 / FAIL 0**（web 45 文件 820 用例，修完后重跑）。
  证据 `docs/evidence/P11/2026-10-23-p11.3/`。24.04 的**客户机热重启本轮没验到**（QEMU 11.1
  + WHPX 在这台宿主上 4/4 挂住，`WHPX: Unexpected VP exit code 4`；TCG 下 24.04 开不了机），
  那次是 guest 已到 `reboot.target` 之后由宿主冷启动完成的——写在证据的限制②里，不折算成通过。
  **P11.2（刷新索引/安装/卸载/升级）passed**：模拟即准入（`apt-get -s` 的计划当准入依据）、
  执行后重读 dpkg 与计划逐条核对（`observed_state.verified`，退出码 0 不是证据）、锁只探测
  从不删除、非交互参数事先定死、四个动作全部不可取消、`remove` 额外要 `REMOVE` 短语。
  两台专用虚机各 **PASS 169 / FAIL 0 / NOT RUN 4**，两次 `exit=0`；宿主 `check.sh`
  **PASS 13 / FAIL 0**（web 43 文件 791 用例）；`//go:build unix` 的三个包第一次在 Linux
  上真跑（24+1 / 41 / 77 全过），并因此抓出一条与本包语义矛盾的锁探测断言。真机顺带抓出
  **6 个缺陷**，其中三个产品缺陷：被信号停住的 apt 只说“退出码 -1”、写操作对话框把 38.7 秒
  的再模拟当成 20 秒就该回来的请求、`--only-upgrade` 的夹具靠“恰好有个可升级的包”这种运气。
  证据 `docs/evidence/P11/2026-10-23-p11.2/`。**P11.1（软件包与更新只读页、包名与版本展示）passed**：代理出事实（`apt.packages`
  给 dpkg 登记 + `apt-cache policy` 候选/套件/优先级，`system.power_state` 给重启标志与
  `boot_id`），Web 算判断（`candidate_relation`/`upgradable`/`security`/`pockets`/
  `counts`/分页/筛选），版本比较走 `internal/debversion`。两台专用虚机各
  **PASS 64 / FAIL 0 / NOT RUN 7**，与 `dpkg`/`apt` 逐条对账 676 与 731 条全查、
  **0 失配**；宿主 `check.sh` **PASS 13 / FAIL 0**（web 42 文件 759 用例）。
  真机抓出并修掉 **1 个产品缺陷**：`dpkg-query` 的 `${binary:Package}` 带架构限定
  （`bind9-libs:amd64`），而 `apt-cache policy` 的块头被 apt 剥成 `bind9-libs:`，
  按请求名索引必然落空——整类包的候选被谎报成“读不到”，页面不报错也不告警。修法是
  基础名回退 + **同名双架构时拒绝回退**（如实报 `unknown`）；对账脚本最初复刻了同一个
  bug（两边都说 null 于是“一致”），改成另一种解析之后才有资格当对账。顺带钉死
  **24.04 的 apt 比 22.04 慢 28 倍**（731 名 26.6s vs 676 名 384ms），于是超时分成
  45s/60s/120s/30s 四层，并且**删掉了启动预热**（开机争抢下 26s→62s，反而把健康检查
  逼成 `AGENT_UNAVAILABLE`）。能力表里没有任何 `package.*` 动作，验收有断言。
  证据 `docs/evidence/P11/2026-10-23-p11.1/`，前端回放夹具 `web/src/fixtures/p11-1-vm/`。
  **P10.4（0.1 安装包、校验和、systemd 单元、初始用户、HTTPS/SSH 隧道使用说明、基本恢复说明、
  完整核心场景）passed**：两台专用虚机跑四个脚本——安装验收 22.04 与 24.04 各
  **136/0/0**；核心场景 22.04 与 24.04 各 **105/0/0**；安装器「组在、账号不在」两支
  **15/0**（22.04）；SSH 隧道两条 **9/0**（在宿主机上跑，从外面 `ssh -N -L` 进来）。真机抓出并修掉 **3 个产品缺陷**：**Q309** 单元探测一次超时被当成
  “服务没在跑”，于是配置写下去了却**从不 reload**（24.04 上 `systemctl is-active nginx`
  偶发 11.4s / 16.5s，超过 10 秒预算；修法是“读不到”与“没在跑”彻底分开：重试 3 次 + 报告
  `unit_state_error` + 不谎报 `skipped_service_inactive`），**Q305** 22.04 上 `useradd` 被
  “组在、账号不在”卡死（空组复用、有成员点名拒绝、卸载补 `groupdel`），**certbot 失败丢原因**
  （`errors.As` 分出“certbot 拒绝”与“跑不起来”，把 stderr 尾部带进错误）。验收脚本自身也修了
  九处（Q306—Q308、Q310、Q311、Q313—Q316：RUN_TAG、文档路径收尾、reload 窗口、按 cookie jar
  的 CSRF、22.04 的 python 信任锚为 0、测试 ACME 夹具每轮从零、reload 后断言等收敛、
  脚本必须 LF、只对 badNonce 重试）。证据 `docs/evidence/P10/2026-10-23-p10.4/`。
  **P10.3（证书：Certbot staging/正式签发、手动导入、到期信息、私钥保护、续期动作）passed**：
  四件事各钉一个位置。**staging 必须显式选**（`staging *bool` 必填，缺了 400 —— 默认值就是"不小心
  调用正式 ACME"）；`--server` **只由 `staging` 推导**，调用方给不了地址，正式地址只能由 root 拥有的
  代理配置覆盖且必须 `https://`；签发前先做 HTTP-01 可达性检查，不通就 `ACME_HTTP01_UNREACHABLE`，
  **不拿限流去试**。**账本只有一个写入点**（`OnJobFinished`），`crt_` 标识在预检展开期定死、按域名唯一，
  所以"续期"不换记录标识。**导入拆成两条通道**：公钥走 `POST /certificates/uploads`（multipart，1 MiB，
  10 分钟一次性、绑会话，正文里带私钥直接 400），私钥走 `POST /secrets`（purpose `tls_private_key`）——
  **秘密不进 argv、不进日志、不进库**，落地时先 `Take` 再验 `CoversDomain`。续期读 jobs 表做同域名
  **1 小时冷却**（429 `RESOURCE_BUSY`，失败的尝试也算一次），只做 HTTP-01、不做通配符、不用 certbot 的
  nginx 插件。**真机抓出一个真正的产品缺口**：续期只换盘上的文件，而"要不要 reload"当时只看配置字节，
  字节没变就 `skipped_unchanged` —— 于是**续期在面板上没有任何一条路径能让新证书真正生效**。
  修法是去**测**而不是记状态：绑了证书、字节又没变时，用 SNI 对 `127.0.0.1:443` 握手取叶证书指纹，
  与盘上比，不一致就 reload 并写 `reload_reason=cert_rotated`，reload 后再测，给 12 × 250 ms 的 settle
  预算，仍不一致才报 `PROXY_TLS_SERVED_STALE`（告警不阻断）；探测失败不改变结论但进 observed。
  这条 settle 预算是被真机逼出来的：`nginx -s reload` 只把 HUP 发出去就返回，紧接着的握手拿到的还是
  旧 worker 的旧证书（实测 `c202…` → 新 `f1d4…`）。VM 上 **PASS=176 / FAIL=0 / NOT RUN=7**（四遍才到 0：
  114/37 → 165/2 → 172/4 → 176/0，前两遍的失败一半是脚本自身的毛病，一半是上面那条缺口）。
  证据 `docs/evidence/P10/2026-10-23-p10.3/`。
  **P10.2（配置生成、原子写入、`nginx -t` 门禁、reload 与失败恢复）passed**：
  一条站点记录第一次变成真在服务的 Nginx 站点。流水线固定成五步：重新评估（不吃预检的旧结论）→
  账号文件先落 → 配置写同目录临时文件并 `fsync` → `rename` 覆盖 + `fsync` 目录 → `nginx -t` 整棵树当门禁；
  失败就回滚，**回滚之后再跑一次 `nginx -t`** 作为证据，失败路径绝不 reload。
  三处容易写错的地方这一轮钉住了：**渲染是纯函数**（apr1 盐由域名派生，所以"再应用一次"是
  幂等重放：不写文件、不备份、不 reload，`file_state=unchanged` / `reload=skipped_unchanged`）、
  **`applied` 是算出来的不是存下来的**（`applied`/`stale`/`not_applied`/`unknown` 四态，库迁移仍是第 9 版，
  Q281 里"P10.2 再加 `applied_config_hash` 列"被 Q286 推翻）、**`proxy.apply` 的参数由 Web 展开**
  （请求体只有一个 `site_id`，定义取自库、`panel_port` 取自真实监听地址，Q282）。
  账号文件权限按 nginx 的 worker 用户定（`root:<worker 组> 0640`，读不到才退回 0644 并报告）。
  删除记录的门禁也在这一轮补上：托管配置还在盘上时 `DELETE /proxies/:id` 是 409
  `DEPENDENCY_BLOCKED`，先删记录会留下一个面板不再管理却仍在服务的 `server` 块。
  VM 上 **PASS=153 / FAIL=0 / NOT RUN=7**；真机抓出并修掉 **1 个**产品缺陷（Q290：预检偶发
  6.7–9.3 秒，一次顶到 10 秒上限变成 503"代理不可用"——nginx 现状采集里的单元探测每次都现场问
  `systemctl show`，而后台服务采集正扫着 503 个单元；进程内复现 **6.389 秒 vs 读快照 0.17 秒**，
  修后三轮最慢预检 224ms / 299ms / 1698ms——最后那一轮紧跟在一次 `proxy.apply` 之后，代理侧 0 条
  慢请求日志，属 2 vCPU 上的瞬时时延，没再退回到现场问 systemd）。同时把两件事变成永久设施：代理对
  超过 500ms 的请求记一条日志，
  验收 §13b 断言"预检不许被代理拖慢"。宿主 `go test ./...` 34 个包全绿、`gofmt`/`go vet` 干净。
  收尾前按最终源码重编了三个二进制并**整份重跑一遍**，`env.txt` 的 sha256 即仓库这版源码。
  证据 `docs/evidence/P10/2026-10-23-p10.2/`。
  **上一子任务 P10.1（Nginx 发现、配置冲突、单域名单 upstream、WebSocket、请求限制、访问策略）passed**：
  这一轮**只登记与判断，一个字节都没写 `/etc/nginx`**——配置生成与 reload 是 P10.2。分成两个包：
  `internal/nginx` 解析配置并吐**事实**（`nginx.conf`、`conf.d/*.conf`、`sites-enabled/*`（含符号链接
  按真实路径去重）、每个 `server` 块的 `server_name` 与行号、`listen`、`include`、`nginx -v`、单元状态），
  `internal/proxies` 出**判断**（`JudgeDomain`）——事实要 root、判断要数据库（面板自己有没有同名记录），
  两边不互相猜（Q278）。代理只登记一条查询 `nginx.state`（参数 `{fresh}`）；**展示走 5 秒快照，写入门禁
  走直读**：缓存窗口里正好可能有人手工加了一条 `server_name`，而那正是唯一需要判准的时刻。
  冲突三态 **`clear` / `taken` / `unknown`，`unknown` 与 `taken` 一样阻断写入**（409 + 证据：
  文件、`server_name` 所在行、原文、`broken_files`）——因为 `nginx reload` 不会因为 `server_name`
  撞车而失败，它只会安静地用第一个块。读不到主机事实是 503 `AGENT_UNAVAILABLE`，不是“没有冲突”。
  域名按 IDNA 归一（A-label、小写、去尾点，禁通配符/协议/端口/路径）；**上游端口不得等于面板自己真正
  监听的端口**（取自运行时监听地址，不是写死的 9443）；默认值与 nginx 对齐（`1m`/`60s`）；
  访问模式与附加字段必须一致（多给也 400，不静默丢弃）；`secret_ref` 只进不出。
  `enabled` 只是**意图**：没有 `applied_*` 字段，审计明细固定 `"applied": false`（Q280）。
  VM 上 **PASS=100 / FAIL=0 / NOT RUN=0**；真机抓出并修掉 **1 个**产品缺陷（Q279：`nginx -v` 的时限
  写成无单位的 `5`，被当成 5 纳秒，命令还没启动就超时——宿主的单测在 Windows 上 `t.Skip`，全绿证明不了
  它）。宿主 `check.sh` **PASS 13 / FAIL 0 / NOT RUN 0**、`go test ./...` 37 个包全绿。
  证据 `docs/evidence/P10/2026-10-22-p10.1/`。
  **上一子任务 P09.5（应用卡片、搜索、分组、一键打开、状态区分和常用入口首页集成）passed**：
  这一轮**只动前端**，没有新增任何接口——`GET /apps`、`/app-groups`、`/apps/discovery`、
  `/apps/{id}/health`、`/apps/{id}/logs` 与既有的 `POST/PATCH/DELETE /apps`、`PUT /apps/{id}/favorite`、
  `/app-groups` 全部是 P09.1—P09.4 就有的。卡片把**四件事分开画**：`runtime_state`、`http_state`、
  `dependency_protection`、`runtime_note`——「容器在跑但回 500」于是同时写着“运行中”与“入口异常”，
  而不是一句“状态良好”。按钮不是按类型画的，是按服务器给的 `capabilities` 画的：link 三项全 false
  只有 `open`，受保护单元只剩 `logs`，看不见的绑定（重建过的容器、不存在的单元）一个按钮都不给。
  **`open` 是锚点不是请求**（`target="_blank"`、`rel="noopener noreferrer"`），地址是服务器地址——
  真机上入口写的是 `http://10.0.2.15:18897/`，那个地址真回 200，**同一端口在 127.0.0.1 上什么都没有**。
  三条容易写错的地方这一轮钉住了：**启停请求只能落成 `service.control` / `container.control`**
  （`controlRequest` 对 link 返回 `null`，`apps.ts` 的注释写明为什么不引入 `app.control`——那条路会
  绕过 `internal/appruntime/gate.go` 的依赖卷门禁）、**收藏是按用户存的**（viewer 收藏后 admin 的列表里
  仍未收藏）、**搜索在页面不在服务端**（`GET /apps?q=` 仍是 200 且结果集不变，契约里没有这个参数）。
  分组删除有守卫：非空分组 422 `DEPENDENCY_BLOCKED` 且分组里的应用没有被顺手移走。
  VM 上 **PASS=211 / FAIL=0 / NOT RUN=0**；这一轮**真机没抓出产品缺陷**，4 处失败是 1 处契约文字
  写错（Q277：`group_id` 形状不对该是 400，描述原来写成“当作没有这个分组”）+ 3 处脚本自己的毛病
  （`appget` 接不了整表达式、拿“刚发完动作”的旧快照断言新状态、删分组的循环被 `resp.json` 覆盖），
  逐条写在证据 result.md §5。前端 **40 文件 / 720 用例**全绿、`typecheck`/`lint` 干净，宿主
  `check.sh` PASS 13/0/0，真机载荷回放 51 条断言（`web/src/fixtures/p09-5-vm/`）。证据
  `docs/evidence/P09/2026-10-22-p09.5/`。
  **上一子任务 P09.4（关联 systemd/容器的启停、日志、状态和依赖卷；受管理/未接管依赖保护明确显示）passed**：
  新包 `internal/appruntime` 把「状态、能力、依赖保护」三件事合成一次判断——**摘要来自观测**，
  **能力来自底层对象自己的能力集合**（不按“它是个 systemd 服务”猜），**保护的含义被限死**。
  三处容易写错的地方这一轮都钉住了：**`unknown` 不是 `stopped`**（绑在不存在的单元/被重建的容器上
  报 `unknown` + 「请重新确认绑定」，不给一个点了必然失败的启动按钮）、**受保护单元仍有日志能力**
  （`ssh.service` 三项生命周期全 false、`logs` 为 true——读日志不是写操作）、**门禁只拦启动**
  （`stop` 永不拦：卷掉线时最想做的就是停机，拦它只会把人逼回命令行、绕过审计）。启动门禁
  `internal/appruntime/gate.go` 跑在 `operations.CreatePreflight` 已有判决**之后**，只能把
  `allowed` 从 true 改成 false（Q273）：未挂载的依赖卷 → `VOLUME_OFFLINE`，卷身份不符 →
  `VOLUME_IDENTITY_CHANGED`，读不到卷清单 → `VOLUME_CHECK_UNAVAILABLE`（fail closed），
  提交被钉住的预检是 409 `DEPENDENCY_BLOCKED`，真机现场用 `docker inspect` 对照过容器确实没动。
  `managed` 只给「docker + `restart` 是 no/空 + 有依赖卷」一种（Q274）——它说的是“经面板发起的
  启动会被检查”，不是“依赖被接管了”：手工 `docker start`、开机自启、`systemctl enable` 过的单元、
  `restart: always` 这四条路都绕过面板，所以那几种一律 `unmanaged` 并在 note 里点名是谁绕开了。
  `GET /api/v1/apps/{id}/logs` 按绑定判别 journal / docker，两种 `page` 形状各自与 `/logs`、
  `/containers/{id}/logs` 完全一致，`unit`/`source` 这类“想改对象”的参数一律 400（对象来自绑定）。
  两条**没做**的明写着而不是拿门禁冒充（Q276、Q275）：运行时卷失联自动停止要代理侧常驻协调者，
  本轮不做（已经跑着的容器不会因卷掉线自动停）；docker 的 `failed` 这一版不区分（DTO 里没有
  ExitCode，非 running 一律 `stopped`，退出码在 `state_text` 里）。VM 上 **PASS=165 / FAIL=0 /
  NOT RUN=0**；这一轮**真机没抓出产品缺陷**，10 处失败全是脚本自己的毛病（子壳变量、拿受保护单元
  当夹具、`/storage/volumes` 读成 `data.items`、`lsblk -r` 与 `-l` 在 util-linux 2.39 互斥、
  拿旧服务快照断言新单元、把「改完之后的版本号」当乐观锁、`pfget` 拼路径去跑表达式），逐条写在
  证据的 result.md §5。宿主 `check.sh` PASS 13/0/0、`go test -count=1 ./...` 全绿。证据
  `docs/evidence/P09/2026-10-22-p09.4/`。
  **上一子任务 P09.3（Docker 只读查询、容器日志与现有受支持容器的启停）passed**：
  容器/镜像/网络/数据卷/部署的只读查询、容器日志（时间键集分页 + SSE 实时流，每账号 4 条并发）、
  以及现有受支持容器的 start/stop/restart 全部就位；Web 进程从不碰 `docker.sock`（真机验了
  “Web 账号不在 docker 组、`docker ps` 被拒、`/proc/<pid>/fd` 里没有 docker.sock”）。三个当时
  必须定下来的东西：**容器身份只认完整 64 位小写十六进制**（全 ID 猜不出来；它是唯一会进 argv
  的可写字符串）、**风险是闭集**（privileged / host 网络 / 危险 capability / 挂 docker.sock /
  挂宿主 `/` → `risk.level=high` + `managed=false` + 三项能力全 false，预检与执行前复核各拒一次）、
  **不该要的东西不请求**（inspect 格式串里没有 `Config.Env`、没有完整标签表，真机放了唯一口令，
  清单/详情/日志/SSE/代理日志里都搜不到）。VM 上 **PASS=217 / FAIL=0 / NOT RUN=1**（第五遍；
  唯一 NOT RUN 是“执行前复核（受理之后再变）”，需要毫秒级可重复竞态，真机构造不出来，由
  `TestContainerControlExecuteStaleState` 覆盖）。真机抓出并修掉 **3 个**产品缺陷——Q270
  （`docker logs --tail N --until 游标` 里 `--tail` 先作用于整个流、`--until` 之后才过滤，往回翻页
  永远只剩边界一条；改成时间窗 `--since … --until`，可加宽可缩窗，下限是容器创建时间）、
  Q271（套接字还在但守护进程死了被当成“命令失败”甚至 500；判据改成 docker 自己那句
  `Cannot connect to the Docker daemon`，清单降级 200 + `available=false`、预检
  `allowed=false` + `DOCKER_UNAVAILABLE`）、Q272（代理回的 5xx 域内码在 Web 层被改写成
  `AGENT_UNAVAILABLE`，页面因此把用户指错方向）。宿主 `check.sh` PASS 13/0/0、`go test ./...`
  全绿、容器相关单测 57 个用例。证据 `docs/evidence/P09/2026-10-22-p09.3/`。
  **上一子任务 P09.2（本机端口/服务/容器发现、管理员确认候选、普通健康检查与 SSRF 约束）passed**：
  三路来源读出本机正在对外提供什么——`/proc/net/tcp{,6}` 的 LISTEN（st=0A）→ inode →
  `/proc/<pid>/fd` → cgroup v2 → `.service` 单元、裸监听、以及 `docker ps -a --no-trunc` ——
  候选 `target_id` 只有 `local_…`/`cntr_…` 两种形状。**SSRF 不是过滤而是写不出来**：探测地址一律由
  代理拼成 `http(s)://127.0.0.1:<port><path>`，`Proxy: nil`、不跟重定向、TLS 校验开、正文 64 KiB 截断，
  调用方没有任何位置可以放主机名、IP 或完整 URL。确认候选＝`POST /apps`（不另开接口，否则“创建应用”
  有两条代码路径）。健康结论只存进 `app_health_results`（迁移 `0008`），定义仍在 `apps.health_json`；
  调度 5 秒一拍、一轮 60 秒、每应用 30—300 秒、最小间隔 5 秒，**代理不可用就写一条 `unknown` 并结束
  本轮**。这一轮把两件诚实性写成了规矩：**“没看”不能变成“没有”**——探测侧 `unknown` vs `error`
  （Q257），绑定合并侧“来源这一轮没采成就不进 `missing_bindings`”（Q260）；**没发出去的探测不填
  address/url**，否则一条 `error` 看起来像真连过。为了容器那三条验收在测试机上装了
  `docker.io` 29.1.3 + `alpine:3.20` **夹具**（不是依赖）。VM 上 **PASS=120 / FAIL=0 / NOT RUN=1**
  （六遍；唯一 NOT RUN 是“卷离线禁止启动受管理依赖应用”，属 P09.4）。真机抓出并修掉 **3 个**
  产品缺陷——Q258（`ssh.socket` 的端口在候选里出现两次：pid 1 代持的监听该归给真正认领的进程，
  pid 1 **独占**的仍要保留）、Q259（有 docker 时同一个容器出现两次：docker-proxy 跑在
  `docker.service` 的 cgroup 里，抑制必须在单元分类**之前**）、Q260（停掉 dockerd 后容器应用
  立刻被说成“可能被重建过，需要重新确认”）；另有一条语义修正 Q257（目标从发现结果里消失记
  `error`，否则同一个“站点停了”会随时点不同在“异常”和“等一下就好”之间跳）。宿主
  `check.sh` PASS 13/0/0、`go test ./...` 全部包 ok。证据 `docs/evidence/P09/2026-10-22-p09.2/`。
  **上一子任务 P09.1（应用/分组/收藏元数据、link/systemd/docker/compose 判别模型、公共入口与内部目标分离）passed**：
  四张表（`apps`、`app_groups`、`app_favorites`、`app_volume_dependencies`，迁移 `0007_apps.sql`），
  标识 `app_`/`grp_` + 32 位十六进制（Q250）。`type` 决定 `binding` 里唯一合法的键，规范化 JSON 让
  `uq_apps_binding`（`WHERE type <> 'link'`）成为唯一性的真正来源，另有
  `CHECK (type <> 'link' OR binding_json = '{}')`——重复绑定是数据库拒绝，不是“先查后写”。
  `public_url` 是给浏览器点的（生产模式禁回环/通配，Q249），健康检查的 `target_id` 是给面板自己探测的，
  形状 `^(local|cntr)_[a-z0-9][a-z0-9_-]{0,47}$`——IP、主机名、端口、URL 在语法层面写不出来，
  `169.254.169.254` 那条在真机上专门试过。摘要四个字段只填“不知道”（link 是 `not_applicable`，
  其余 `unknown` + `last_checked_at=null` + `dependency_protection=unmanaged`），`capabilities` 只声明
  此刻真能做到的事（只有 `open`），于是 P09.5 画不出死按钮。依赖卷只存代理侧标识、不建外键（Q248），
  写入时经代理确认，三种结局互不混淆：通过 / 422 `DEPENDENCY_MISSING` / 503 `AGENT_UNAVAILABLE`（**不写库**）。
  删除只删元数据（`jobs` 行数不变、`service.*` 审计为 0）；收藏按 (app, user) 存、级联删除、**不写审计**。
  VM 上 **PASS=210 / FAIL=0 / NOT RUN=0**（第八遍；第七遍是紧接着的复跑，同样 210/0/0，验证了夹具在
  脏盘上也能自清）。真机**没有发现产品缺陷**；前几遍的 23 个 FAIL 全部是验收脚本自己的问题，
  逐条记在证据第 5 节：4 处断言写错（`error.details` 不存在、拿已占用的容器 ID 去改绑、破坏性动作漏了
  再认证）与 1 处夹具没清干净（上一轮 `/etc/fstab` 里 `p09data` 那行让这一轮导入被**正确**拒绝）。
  另外把一条产品语义钉进了脚本：**预检故意绑定快照**（`Collector.Assess` 用 `Topology` 而非现场重扫），
  所以抹盘之后要等代理下一次扫描再预检，否则预检会正确地判“盘上已有分区表”——这不是缺陷，
  脚本原来没等。宿主 `check.sh` PASS 13/0/0、`go test ./...` 全部包 ok。证据
  `docs/evidence/P09/2026-10-22-p09.1/`。
  **上一子任务 P08.4（共享 UI、用户权限与客户端连接说明）passed**：
  页面把 §7.1–7.3 已经算出来的东西如实摆出来，并教人怎么连——**不加接口、不改字段**。
  三个准备状态（`config_hash` / `directory_prepared` / `config_applied`）分开显示；
  **卷的状态与开机依赖都不在共享记录里**（`ShareRecord` 只有十个字段）：卷的挂没挂由页面按
  `volume_id` 从 `/storage/volumes` 的 `observed_state` join，取不到就少显示一行、不把“不知道”
  画成“没挂”，而状态是 `unknown`/`error`/`mismatch` 时说“卷状态读不出来”而不是“没挂”；
  开机依赖在 `share.apply` 的 `result.data` 里。客户端说明给 Windows/macOS/Linux-`mount.cifs`/
  Linux-`smbclient` 四种，地址取自 `window.location.hostname`，命令里**永远没有密码**
  （`credentials=` 文件、`0600`、here-doc 从标准输入写，禁 `sh -c 'printf "password=…" > f'`）。
  VM 上 **PASS=190 / FAIL=0 / NOT RUN=4**（四遍），第 5 节逐字执行说明里的 11 行命令
  （真 `smbclient` + 真 `mount.cifs` 挂载、通过挂载点写入读回、`umount`），第 6 节四种身份
  全部拿到真实 `NT_STATUS_*`，第 7 节 58 份载荷 / 两个数据库 / `/proc/*/cmdline` 与 `environ` /
  `/proc/mounts` 的密码命中数全为 0。真机抓出并修掉 **2 个**产品缺陷——
  Q246（前端 DTO 里写着后端从不发送的 `volume_state`/`volume_name`/`startup_deps`/`startup_paths`，
  于是“卷没挂”永远亮不了）、Q247（“重新应用”这条唯一的出路走不通：`runtime_revoked` 从来没被清过，
  页面永远显示“已撤销”）；另记 **5 处**验收脚本缺陷（起点 include 残留、只读写探针被 shell 绕开、
  第 9 节按“面板自己卸卷”设计、又去读不存在的 `volume_state`、二进制早于前端产物同步）。
  宿主 `check.sh` PASS 13/0/0、`go test ./...` 全部包 ok、前端 637 条用例全绿。
  证据 `docs/evidence/P08/2026-10-23-p08.4/`；回放夹具 `web/src/fixtures/p08-4-vm/`。
  **上一子任务 P08.3 passed（卸载/格式化依赖阻断这一块）**：
  共享只认“卷真的挂着”才渲染——`storage.VolumeStateMounted` 之外的任何状态（含 `unknown`、空串）
  一律不写那一段，于是“卷没挂时共享的是系统盘上的空目录”这件事在配置层面就不成立；运行时脱盘由
  一个 30 秒一轮的**只撤不恢复**监视循环处理，撤销现场（卷 id、卷名、`unmounted`、时间、原因）
  落在共享记录上，卷回来**不会**自动写回，要人再应用一次。开机依赖写 systemd drop-in
  `/etc/systemd/system/smbd.service.d/upanel-shares.conf`（`0644`，`RequiresMountsFor=` 一行四个路径、
  排序去重、原子写、必须 `daemon-reload`、路径清空时整份删掉），**开机拉起挂载的是它而不是 fstab**
  （面板写的 fstab 条目带 `noauto`）。存储侧的反向约束按 `purpose` 分档：卸载只被**启用中**的共享
  挡住，格式化与退出管理被**任何**共享挡住，依赖查不出来一律 `VOLUME_DEPENDENCIES_UNKNOWN` 阻塞。
  VM 上 **PASS=207 / FAIL=0 / NOT RUN=5**（三遍）；重启顺序另用一个三阶段脚本连同一次真 `reboot`
  验完（pre 18 / post 11 / clean 5，各 0 失败）。真机抓出并修掉 **2 个**产品缺陷——
  Q240（`shares` 表少一列、`runtime_revoked` 从来没落过盘，还让幂等判据永远失效）、
  Q243（托管配置与 drop-in 的 `0644` 被进程 umask 掩成 `0600`，契约里那条一直没成立过）；
  记下 **2 处**验收脚本缺陷（Q241 按猜测的 JSON 形状断言、把 `-1` 当合法下标、把变量当命令；
  Q244 固定幂等键让第二遍跑不起来，并且把提交失败记成“作业超时”）与 1 处契约补记（Q242）。
  宿主 `check.sh` PASS 13/0/0、`go test ./...` 全部包 ok。
  证据 `docs/evidence/P08/2026-10-22-p08.3/`。
  **下一阶段 P09**：现有 Web 应用与 Docker 接入，下一个子任务 **P09.1**（应用/分组/收藏元数据，
  link/systemd/docker/compose 判别模型，公共入口与内部目标分离）。
  **上一子任务 P08.2 passed**：
  **P08.2（最小 include 接入、`testparm`、原子更新、重载、共享连接状态；已有配置保留）passed**：
  面板独占 `/etc/samba/upanel.d/upanel.conf`（每次整份重写），`smb.conf` 末尾**只追加两行**
  （一行说明注释 + 一行 `include`），原有内容逐字节保留。写链路是
  渲染 → `.upanel-new` + fsync → 候选 `testparm -s` → 备份（`/var/lib/upanel-agent/smb-backups/`，0600，
  留 40 份）→ rename → 目录 fsync → 接 include → 合并后 `testparm -s` → `smbcontrol all reload-config`
  （**只在 `smbd` 是 active 时**）→ 逐参数 `testparm --section-name=X --parameter-name=Y` 读回。
  报告值 `samba_config`/`include`/`reload`/`service`/`served`/`outcome` 各说各的话：
  服务停着时配置照写但结果只能是 `accepted`、`served=false`、不带 `verified_at`。连接状态读的是
  `smbstatus` 自己的会话表（账号、机器、方言、签名、建立时间），与面板报数交叉核对。
  VM 上 **PASS=237 / FAIL=0 / NOT RUN=0**（四遍才走到这里）；真机抓出 **1 个**产品缺陷
  （Q233：`MkdirAll(0700)` 把上级目录也建成 0700，共享账号连“走进去”的 `x` 都没有——7 条断言因此失败，
  但只有 1 个根因，记账时分开记）、**1 个**契约事实（Q234：`hosts allow` 对 `127.0.0.1` 永远放行，
  “从本机连、期望被拒”这条断言本身是错的），记下 2 处验收脚本缺陷（`set -u` 下引用函数局部变量；
  用环回测 `hosts allow`）。宿主 `check.sh` PASS 13/0/0、`go test ./...` 28 个包全 ok。
  证据 `docs/evidence/P08/2026-10-22-p08.2/`。
  **上一子任务 P08.1（Samba 能力检测、受管理账号、密码秘密通道、POSIX ACL、共享定义与配置版本）passed**：
  `internal/shares`（15 个文件：11 个生产文件 3681 行 + 4 个测试文件 2251 行）+
  `internal/agent/shareaction.go|sharequery.go` +
  `internal/agentstore/sharestore.go` + `migrations/agentdb/0005_shares.sql` 把五个动作
  `share.apply|disable|delete`、`share.account.apply|delete` 接上“预检 → 任务 → 幂等 → 资源锁 → 读回 → 审计”。
  五项承诺逐条落地：能力探针只认**固定候选绝对路径**（不落 PATH、不可配置，缺哪个报哪个，`config_write`
  恒 `unsupported`）；密码明文**只经过一次、只走 `smbpasswd` 的标准输入**（日志、数据库、argv、environ、
  响应体五处都扫过）；账号三条独立读回（`getent passwd` / `passwd -S` / `pdbedit -Lv`）；ACL 用
  `setfacl --set` **整份替换**并用 `getfacl -cp` 读回核验 `mask` 覆盖；`share.apply` 的 outcome 是
  `accepted` 而**不是** `verified`，`observed.samba_config="not_written"`、`served=false`、恒带
  `SHARE_CONFIG_NOT_APPLIED` 告警——这一阶段真的没写 `smb.conf`（首末 `sha256` 逐字节相同）。
  VM 上 **PASS=382 / FAIL=0 / NOT RUN=4**（五遍才走到这里）；真机抓出并修掉 **4 个**产品缺陷
  （Q214 新建预检没有版本、Q220 删除预检没有指纹、Q221 观察超时变 500、Q223 预检与执行共用同一个观察预算），
  钉死 **1 个** P05.1 的性能问题（Q222，隔离实验：代理停/跑/停 → 23—29 ms / 平均 106 ms 最长 2894 ms /
  23—29 ms），记下 5 处验收脚本缺陷。宿主 `check.sh` PASS 13/0/0、`go test ./...` 全部 ok、
  `internal/shares` 54 个用例 0 FAIL。证据 `docs/evidence/P08/2026-10-22-p08.1/`。
- 上一阶段：**P07（分区、格式化、挂载）passed（P07.1—P07.5 全部）**。P06.1—P06.3 全部 passed。P07.1（存储动作预检、可靠设备身份、整盘锁、系统设备/挂载/swap/holders/依赖保护；**只验证拒绝逻辑**）**passed**：四个存储动作只登记解码 + `Plan`、`Execute` 为 nil，提交必 501 `NOT_IMPLEMENTED`，本轮**没有任何一次写盘**；判决层对“读不出来”的一律阻塞（签名、`/proc/*/fd`、分区表、设备类型），保护沿 `partition_of`/`depends_on` 递归向上，能力投影由判决层算出并带 30 秒 TTL。VM 上 **PASS=189 / FAIL=0 / NOT RUN=1**（唯一 NOT RUN 是 `PREFLIGHT_STALE`：没有执行器时提交在复核之前就 501）；真机抓出并修掉 **2 个**产品缺陷（Q171 交换区魔数在 4096−10 而不是盘尾；Q172 `holders`/`slaves` 是符号链接目录、没有 `list` 文件，导致 dm/md 占用在真机上永远看不见）。宿主 `check.sh` PASS 13/0/0、前端 445 用例、openapi valid。
  **P07.2（真的写盘：初始化 GPT、建分区、删未使用分区）passed**：`internal/storage/write.go`（943 行）
  只用代码里的**固定模板**，模板经 `syscmd` 新开的**有上限的 stdin 通道**（64 KiB，不得来自 HTTP 参数）
  交给 `sfdisk`——命令行里只有已校验的数字（换算成 512 字节扇区、必须 2048 的整数倍）与已校验的名字
  （可打印 ASCII、≤36、不含 `"` 与 `\`）。写之前重算判决、比对预检绑定的身份指纹与状态版本、核对表类型与
  目标槽位；写完 `udevadm settle`、等内核与 `/dev` 节点、再读盘上的 GPT 头与条目表与“写前算好的期望形态”
  逐条比对，**两源（盘上字节 + 内核/sysfs）都对上才算 `verified`**——退出码 0 不算验证。格式化仍属 P07.3，
  `storage.filesystem.format` 依旧 501。VM 上 **PASS=177 / FAIL=0 / NOT RUN=1**（唯一 NOT RUN 是
  `DEVICE_IDENTITY_CHANGED`）；真机抓出并修掉 **1 个**产品缺陷（Q177：`disk_entries` 的“空数组”与
  “没读到”在 JSON 里撞车，`omitempty` 把两者说成同一件事）、记下 4 处验收脚本缺陷（Q178—Q181），
  §10 的并发还**撞到了 `runner` 里的第二次 `revalidate`**（Q182，P07.1 那条 NOT RUN 由此补上机会性证据）。
  宿主 `check.sh` PASS 13/0/0、前端 **450** 用例（新增 5 例真机载荷回放）、openapi valid。
  **P07.3（ext4/XFS 格式化）passed**：`internal/storage/format.go`（448 行）+ `fsprobe.go` 把
  `storage.filesystem.format` 接上执行器——固定模板（只有已校验的文件系统名、设备路径与卷标），
  卷标经第三道闸；写完**直接读超级块**（ext4 偏移 1024 的 `53 ef`/`s_uuid`/`s_volume_name`，
  XFS 偏移 0 的 `XFSB`/`sb_uuid`/`sb_fname`）做两源核验，退出码 0 不算验证。四条纪律落地：
  确认短语猜错一次就作废整份预检；标签超限在**预检之前**就是 400（ext4 的 17 字节标签会 rc=0
  并被**静默截断**，见 Q184）；mkfs 跑着时取消是 409 且任务不会被标成 canceled；代理被 SIGKILL
  后操作落 `interrupted` + `OPERATION_INTERRUPTED` 且**不自动重跑**。VM 上
  **PASS=204 / FAIL=0 / NOT RUN=1**（唯一 NOT RUN 是内核/盘上条目不一致的真实竞态，单测覆盖）；
  真机抓出并修掉 **1 个**产品缺陷（Q190：对已经不存在的设备做预检回 500——拒绝路径没有目标可绑定，
  预检层却无条件要求绑定），记下 Q184—Q190。
  **P07.4（受管理卷）passed**：`internal/storage/fstab.go`（760 行）+ `volume.go`（431）+
  `volumeassess.go`（960）+ `volumewrite.go`（878）+ `volumelist.go`/`volumeholders.go` 把
  “一块已经有文件系统的分区”变成面板管理的卷——登记记录 + `/etc/fstab` 托管块 +
  `/srv/upanel/volumes/<slug>` 三者由五个动作（`import`/`mount`/`unmount`/`configure`/`forget`）
  维护。fstab 只做**最小修改**：块外逐字保留、选项顺序固定（`nofail` 恒在、`noauto` 随
  `automount`、第六列 ext4=2/XFS=0）、写路径固定为“比哈希 → 渲染候选 → 临时文件 → fsync →
  `findmnt --verify --tab <候选>` → 备份 → rename → fsync 目录”，候选不合格时目标文件一个字节不动，
  渲染结果与原文相同则直接返回；**禁用 `mount -a`**。占用列表把两种来源合流
  （fd 的 `st_rdev` 与挂载点下文件的 `st_dev`），“挂着但空闲”不再算忙，读不到 `/proc` 一律
  fail-closed。VM 上分两段跑（中间隔着一次**真重启**）：第一段
  **PASS=267 / FAIL=0 / NOT RUN=0**，第二段 **PASS=38 / FAIL=0 / NOT RUN=0**——重启后
  `srv-upanel-volumes-data.mount` 在开机 26 秒时由 fstab 自己挂上（那之后才启动面板）、
  UUID 与登记一致、标记文件 sha 不变、`noauto` 的那个卷**没有**挂上。真机抓出并修掉
  **1 个**产品缺陷（Q199：configure 的预检没绑定目标身份指纹 ⇒ 面板 500），记下 Q191—Q203
  （含 Q191/Q192 两个占用口径缺陷与 Q200/Q201 两道门的澄清）。
  **P07.5（磁盘页面完整交互 + 系统外部独立检查 + 存储恢复文档 + openapi 补齐 P07）passed**：
  前端五个新组件接上九种动作（`DeviceStorageActions`、`VolumeImportForm`、`VolumeCard`、
  `VolumeSection`、`StorageActionDialog`）+ `web/src/api/volumes.ts`、`web/src/utils/jobresult.ts`；
  判定只认载荷里的 `capabilities`，`capacity` 只在真挂着时存在、没挂写「没有容量读数」而不是 `0 B`；
  `api/openapi.yaml` 补三条卷接口 + 五个 schema（版本 `0.0.0-p07.5`）；
  `scripts/storage-external-check.sh`（381 行）做**不经过面板的独立检查**（只读 `/etc/fstab`、
  `findmnt --mountpoint`、`/proc/self/mountinfo`、systemd 单元、备份权限，退出码 0=一致）；
  `docs/STORAGE_RECOVERY.md` 写清人怎么恢复。真机验收 **PASS=205 / FAIL=0 / NOT RUN=3**，
  外部检查在真机上跑三遍（一挂一空 / 退出管理后 / 全空）并做五种坏 fstab 注入（全在副本上）。
  真机抓出并修掉 **1 个**产品缺陷（Q208：把“未挂载所以读不到类型”当成“不合格”，
  于是纳入管理的候选在任何真机上都是空的——夹具比现实干净，测试就成了橡皮章），
  记下两个验收脚本缺陷（Q204 `findmnt --target` 的语义、Q209 想当然的收尾期望）与
  Q205/Q206/Q207。宿主 `check.sh` PASS 13/0/0、web 35 文件 / 552 用例、openapi valid。
  下一个子任务 **P08.1**（`docs/STAGES.md` §10 第一项）未开始。
- 已完成子任务：P00.1—P00.3、**P01.1**—**P01.4**、**P02.1**（首个管理员、密码哈希、登录、
  退出、会话轮换、CSRF、Origin、限速；无默认密码）、**P02.2**（admin/viewer、账号管理、
  最后管理员保护、再认证、脱敏 DTO、审计事件起步）、**P02.3**（代理 Unix socket + SO_PEERCRED
  UID 边界、固定动作/查询登记表、结构化只读查询、命令执行器超时/输出上限）、**P02.4**（审计事务化、
  日志秘密脱敏、代理独立数据库、AES-256-GCM 秘密存储与到期销毁）、**P03.1**（预检计划由代理生成、
  120 秒期限、用户/会话/动作/规范化参数/状态版本绑定、再认证与确认短语策略、公开预检 DTO）、
  **P03.2**（持久 jobs、`Idempotency-Key` 幂等链、`operation_id` 去重、代理执行记录与事件、
  资源锁与排队上限、取消规则、先落库再提交与按 `operation_id` 对账；两台 VM 各 101 项断言通过，
  VM 内核原生 169 用例全绿）、**P03.3**（SSE 事件流与断线续传、`event: gap`、日志环容量、
  代理事件镜像、任务中心与错误详情、最小登录页与路由守卫；两台 VM 各 73 项断言，
  抓并修掉 Q67 的原子性缺陷）、**P03.4**（启动 + 每 30 秒的对账循环、只查不跑、提交在途不判定、
  `NOT_FOUND` 按 `needs_sync` 分流为 `failed`/`unconfirmed` 与 `interrupted`/`unknown_result`、
  幂等记录到期清理、验收注册表约束变成断言；两台 VM 各 78 项断言，原生测试 360 用例全绿）、
  **P04.1**（系统信息采集：`internal/monitoring` 读 `/proc`（stat/meminfo/loadavg/net/dev/
  os-release/uptime）与 `syscall.Uname`，代理侧 5 秒轮询缓存 + 单调时钟驱动 + `boot_id` 失效，
  每次采集带 `collected_at`、`interval_ms`、`stale`、`degraded` 与**分节状态**；字节/计数器
  一律非负十进制字符串，算不出来的速率是 `null` 而不是 `0`；查询登记表新增 `system.summary`
  （唯一查询），`GET /api/v1/system/summary` 走 Web 透传、需要 V 权限、代理不可用时 503
  `AGENT_UNAVAILABLE` 且不影响登录；两台 VM 各 72 项断言、原生测试 394 用例全绿、
  12 个验收脚本全量回归 0 失败）、**P04.2**（存储拓扑只读：`internal/storage` 走 `/sys/block` +
  `/sys/class/block` 枚举块设备树（整盘/分区/LVM/RAID/加密/loop/rom），边只有 `partition_of`/`depends_on`
  且 from=子→to=父；设备身份按 WWD→序列号→型号+容量的强弱顺序算 `sha256:` 指纹并给出
  `identity_confidence`（读不到就如实 `medium`）；系统盘保护从 `/proc/self/mountinfo` + `/proc/swaps`
  出发、沿边**递归向上**标记，空白盘不保护、临时挂载的 loop 镜像也不保护；分区表读盘头部只用来补类型与
  `free_regions`，与内核不一致时保留内核结果并给 warning；容量 30 秒缓存、`available_bytes` 取 `f_bavail`；
  `snapshot_version` 只由内容决定（与时间、容量无关）；查询登记表新增 `storage.topology`，
  Web 侧 `GET /api/v1/storage/{topology,devices,devices/:id}`（权限 V）只透传；两台 VM 各 108 项断言、
  原生测试 473 用例全绿、13 个验收脚本全量回归 0 失败。**本轮抓到并修掉两个只有真机才能发现的缺陷**：
  块设备被当普通文件读导致代理被 OOM 杀死（Q89）、`snapshot_version` 依赖时间（Q90））、
  **P04.3**（SMART/NVMe 健康与 diskstats：`smartctl -j -H -A -i` 一次拿全、按退出状态**位掩码**判读
  （失效/预失效 → `critical`，历史位/日志位 → `warning`，命令/打开失败 → `unknown`），温度 ≤ 0 当没有读数；
  先读 sysfs 电源状态、非 NVMe 再加 `-n standby`，**不为读健康唤醒休眠盘**；`/proc/diskstats` 每台设备只取
  自己那一行、计数转十进制字符串，速率由相邻两次采样差分（归零/回绕/窗口异常 → `null`）、平均延迟仅在
  完成次数 > 0 时给、扇区固定 512 字节；网络文件系统的容量在代理自己的 `--statfs-helper` 子进程里查
  （并发 2、默认 10 秒超时、失败后隔离 5 分钟），拓扑新增 `network_mounts`/`Device.io`/`Mount.capacity_error`，
  `io` 与网络盘容量不进 `snapshot_version`；健康采集是独立 goroutine、默认 15 分钟（可配 5—60）；
  两台 VM 各 77 项、`upanel-smart`（QEMU NVMe）62 项断言通过，原生 `storage.test` 三台 VM 103/0，
  **statfs 挂住时登录 332 ms / 271 ms**）、**P04.4**（首页、磁盘列表、拓扑/分区详情、健康与容量卡片的浏览器呈现：
  前端 DTO 与 `api/openapi.yaml` 同源、七状态组件 + 空/错误/依赖未安装、`null` 一律显示 `—`、
  容量/百分比/速率照实渲染、`capabilities` 为空就明说“当前阶段没有可执行的动作”而不是摆按钮；
  宿主 Playwright 冒烟 31/0/5（真实 `upanel-web --mode mock`）+ dist 预览 10/10、前端 **192** 个用例、
  VM 上四份真实响应的载荷校验 **856/0** 与脚本 **31/0**；本轮修掉 Q105/Q106，详见 §5）。
  **P05.1**（服务列表、详情与保护名单，以及通过预检 + 任务执行允许的启动/停止/重启/开机启动：
  `internal/services` 从 `list-unit-files` ∪ `list-units --all` 枚举并剔掉模板与别名，按块解析
  `systemctl show` 并对缺失的名字逐个重试，10 秒快照缓存；受保护单元（面板自身、SSH、systemd 核心、
  网络基础、其他模块）`capabilities` 恒为空；单元身份指纹是 `sha256:` 全 64 位；执行前再次校验保护与
  状态，`systemctl` 返回 0 也要读回验证，不收敛即失败；失败任务 `result` 为 `{}`、原因只在 `error` 里；
  审计补上 `job_id` 落库。VM 上 **PASS=140 / FAIL=0 / NOT RUN=3**、载荷校验 **22369/0**，
  前端新增真实载荷回放 22 例，宿主 `check.sh` PASS 13/0/0。**本轮抓到并修掉两个只有真机才能发现的缺陷**：
  模板名让 `systemctl show` 整条中断（282 个单元的机器只显示 7 行）、别名单元让整次采集失败
  （详见 §5 与 CONTRACTS §10.1））、
  **P05.2**（进程列表与控制：`internal/processes` 纯 `/proc` 采集（`pid` + `start_time_ticks` 身份、
  10 个状态与 7 类保护的闭集、命令摘要脱敏、5 秒 2000 条快照）、代理侧生产动作 `process.signal`
  （**TERM 与 KILL 是两次独立操作**，KILL 要 5 分钟内再认证 + 一次性短语 `KILL XXXXXX`，
  `allowed=false` 时**照样**按信号给出策略、真正拦它的是 409 `DEPENDENCY_BLOCKED`）、
  执行前现场重读 `/proc` 核对身份/保护/是否已退出后再发信号，发完每 100 ms 读回
  （TERM 10 秒 / KILL 5 秒），只有进程确实不在了才 `succeeded`；受保护进程（PID 1、内核线程、
  面板与代理自身、SSH、systemd 核心、网络基础、其他模块）`capabilities` 恒为空；PID 重用由
  `ns_last_pid` 在真机上造出来验证（旧预检 409 `DEVICE_IDENTITY_CHANGED`，新进程毫发无伤）。
  VM 上 **PASS=128 / FAIL=0 / NOT RUN=0**、载荷校验 **9250/0（1 SKIP）**，前端新增真实载荷回放
  31 例、全量 295 例，宿主 `check.sh` PASS 13/0/0。**本轮改对了两条写错的断言**（被拒的 KILL 仍带短语、
  viewer 读进程列表是 403），并把 harness 从“睡固定秒数”改成“等快照里出现再断言”——
  详见 §5 与 DECISIONS Q112—Q117）、**P05.3**（日志：`internal/journal` 真实 `journalctl` 结构化查询与
  跟随流、`internal/logs` 契约校验、`logs.query` 查询 + `/internal/v1/streams` 单次许可 NDJSON 流、
  `GET /api/v1/logs` 与 `/logs/stream` 两条 admin 接口、前端查询页与实时流；VM 上 132/0/1，
  9 个真机缺陷全部修掉，结束订阅后 `journalctl` 子进程归零）、**P06.1**（受管理文件根与只读文件接口：
  `openat2` 单次解析 + 同句柄 `fstat`、根身份每次核对、配置拒绝 `/`，`files.roots|list|text|download`；
  VM 上 115/0/0、载荷校验 6379/0，真机抓到 `O_PATH` 上 `getdents64` 等 3 个缺陷）、
  **P06.2**（流式暂存上传与受控写入：七个写动作 + 上传三件套全部走同一条预检→任务→幂等→锁→读回→审计链路，
  提交时 `renameat2` 原子换位、覆盖前编辑备份（10 份 / 500 MiB）、版本冲突在预检就拒；
  VM 上 228/0/0、VM 内核原生 909/0、前端 428 例，真机抓到 `O_PATH` 重犯、`PARENT_MISSING` 不可达、
  `.upanel` 可见、预留计数两处泄漏共 4 类 6 个缺陷）、
  **P06.3**（受限 Web 终端：`upanel-shell` 的 PTY 在 `exec` 前降权并从 `/proc/<pid>/status` 读回身份核对，
  浏览器→Web→代理两跳 WebSocket，四道门（Origin → 子协议 → 一次性许可 → 代理附加）全在 upgrade 之前，
  帧字段闭集 + 单个 64 KiB 载荷 + 256 KiB/s 与 200 帧/s 上限 + 关闭码 1000/4001/4002，
  会话两状态、单连接（第二条 409）、断流宽限内重连接回同一个 shell，时限与数量只能收紧，
  审计只记元数据（终端全文不进日志/审计）；VM 上 282/0/0、VM 内核原生 962/0、前端 445 例，
  真机抓到 6 类缺陷——最硬的一条是“关 master 不会挂断”，会话已结束而 shell 还在跑（Q169））、
  **P07.1**（存储动作预检、可靠设备身份、整盘锁、系统设备/挂载/swap/holders/依赖保护；**只验证拒绝逻辑**，
  四个动作 `Execute` 为 nil、提交必 501，VM 上 189/0/1，真机抓到 Q171/Q172 两个缺陷）、
  **P07.2**（真的写盘：固定模板初始化 GPT、建分区、删未使用分区，模板只经 `syscmd` 的有上限 stdin 通道，
  写前重算判决+比对身份与版本，写后 `udevadm settle` 再两源读回核对，VM 上 177/0/1，抓到 Q177）、
  **P07.3**（ext4/XFS 格式化：固定模板 + 超级块直读的两源核验、标签上限在预检前 400、错短语作废整份预检、
  禁止取消、代理被 SIGKILL 后 `interrupted` 且不自动重跑、执行前二次身份检查；VM 上 204/0/1，
  抓到 Q190——“对不存在的设备做预检回 500”）。
- 已完成子任务（续）：**P07.4**（受管理卷：导入、挂载/卸载、UUID 校验、fstab 最小修改、
  原子替换/备份/回滚、开机自动挂载、繁忙占用列表；VM 上分两段跑——第一段 267/0/0、第二段（真重启后）
  38/0/0，抓到 Q199“configure 的预检漏绑定目标身份指纹 ⇒ 面板 500”，另有 Q191/Q192 两个占用口径缺陷
  与 Q200/Q201“两道门”的澄清）。
- 本轮已完成：**P08.4**（共享 UI、用户权限与客户端连接说明）。共享页把三个准备状态分开显示、
  卷的状态由页面按 `volume_id` 从卷列表 join、客户端说明四种且命令里没有密码；真机 **190/0/4**，
  抓到并修掉两个产品缺陷（Q246 前端 DTO 写着后端从不发送的四个字段、Q247 “重新应用”走不通），
  并把 13 份真机载荷回灌成前端回归夹具。
- 下一子任务：**P09.1**（`docs/STAGES.md` §11 第一项：应用/分组/收藏元数据，link/systemd/docker/compose
  判别模型，公共入口与内部目标分离）。
  前置注意：P09 需要真实的 Docker 与既有 Web 应用（24.04 VM 上已装 docker，P09.3 才需要容器夹具）；
  P07.4/P07.5 里 `dependencies` 的三个来源中 `shares` 已按卷依赖接上（P08.3），`apps` 与 `backups`
  仍恒为 `[]`，分别在 P09/P12 接。
  **破坏性验收只能用可丢弃的独立 VM 数据盘（sdb/sdc/sdd），
  验收脚本必须先算出承载 `/` 的整盘并拒绝任何与它同源的盘**。
  P05 结束时面板已有四类只读查询（系统/存储/服务/进程/日志）、受控写动作（服务启停与进程信号）
  和一条完整的“预检 → 任务 → 幂等 → 资源锁 → 读回验证 → 审计”链路；P06 的文件操作与 P07 的全部
  存储动作都复用同一条链路，没有新开执行路径。
- 下游是否可继续：**是**；但任何系统写操作必须走 P03 建成的链路（预检 → 任务 → 幂等 → 对账），
  不允许绕过。P04.1 的 `system.summary` 是**查询**，不产生任务，也不落写审计。
- 当前版本：未发布。
- 当前无阻塞。遗留（不阻塞）：VM-C（LVM 根，P04 前需要）、VM-D（SMB 客户端，P14 前需要；
  P08 用同机真实 `smbclient` + 真 `mount.cifs` 代替，跨机器访问仍记 NOT RUN）；
  验收 VM 内未安装 Go/Node 工具链（DECISIONS Q9）；systemd 真实托管形态已在 P02.3 的两台 VM
  验收中落地（代理单元安装、启动、socket 权限、停止清理均有断言），见第 7 节。
- 环境事实（P02.4 期间）：24.04 验收 VM 在回归途中内核 panic（`Attempted to kill init!`
  exitcode=0x8b，systemd/PID 1 段错误，`console.log` 可查），随后重启该 VM 并重跑全部验收；
  `/tmp` 随重启清空，二进制与脚本重新上传并核对 sha256。**这是测试环境问题，不是产品行为**；
  但“宿主侧 WHPX 长时间高负载下 VM 可能挂”记在这里，后续阶段连续跑长验收时要留意。
- 环境事实（P03.2 期间）：24.04 验收 VM 又 panic 两次（同一条 `Attempted to kill init!`，
  exitcode=0x8b，分别在约 4058 s 与约 528 s uptime），两次都重启后重跑全部验收并通过；
  其中一次 panic 留下了一份**被截断的 `/etc/upanel/upanel.json`**（内容只剩空），导致三项断言
  报 `解析配置文件失败 … EOF`——删掉该文件后 34/0 与 114/0。三次 panic 全部出现在 24.04 上，
  22.04 从未发生，仍按“宿主 WHPX 长跑不稳”处理，不记作产品缺陷。
  另外：宿主 PowerShell 的 `ssh` 不认这两把 VM 私钥（`UNPROTECTED PRIVATE KEY FILE`），
  所有 VM 操作用 Git Bash 的 `ssh` 完成。
- 环境事实（P04.1 期间）：24.04 验收 VM 第四次 panic（同一条 `Attempted to kill init!`，
  exitcode=0x8b），重启后 `/tmp` 清空、`/etc/upanel/upanel.json` 再次被截断成 0 字节；
  删除该文件并重新上传二进制/脚本/夹具后全量回归通过。四次全在 24.04，22.04 一次没有。
  结论不变：宿主侧 WHPX 长跑不稳，不是产品缺陷。**但这也意味着任何一次“跑完就过”的结论
  必须能在重启后再复现一遍**——P04.1 的两台 VM 最终结果都是重启后重跑得到的。
- 环境事实（P04.2 期间）：24.04 验收 VM 第五次 panic（同一条 `Attempted to kill init!`，exitcode=0x8b），
  这次发生在全量回归跑到一半时。重启、重传二进制/测试/夹具/脚本、核 sha256（12/12 一致）后，
  依次跑原生测试（473/0）、p04-2（108/0）、全量回归（13 脚本 0 失败）。五次全在 24.04，22.04 一次没有。
  另一件事**不是环境问题而是我自己的操作缺陷**：`chmod +x /tmp/upanel-bin/upanel-*` 匹配不到
  `upanelctl`（没有连字符），p04-2 因此报了 4 项失败（`init-admin` 退出码 126）；显式列三个文件后 108/0。
- 环境事实（P05.1 期间）：24.04 验收 VM 又出现一次 `Attempted to kill init!`（exitcode=0x8b），
  形态与以往几次一致，发生在一次 P05.1 验收运行期间；该次运行作废，重启 VM、重传二进制与脚本后
  重跑才得到最终结果（`docs/evidence/P05/2026-10-21-p05.1/env.txt` 记了这一条，不逐个累计次数）。
  处理方式与前几轮相同：`/tmp` 会被清空、必要时删掉被截断的 `/etc/upanel/upanel.json`。
- 环境事实（P05.2 期间）：`upanel-2404` 至本轮累计 7 次 `Kernel panic - not syncing: Attempted to
  kill init! exitcode=0x0000008b`（`CPU: 1 PID: 1 Comm: systemd`，`segfault … error 44/46 in libc.so.6`），
  本轮验收期间又发生一次；已排除镜像共享与宿主内存不足（同期宿主可用 32.4 GB）。最终读数取自
  第七次 panic 之后重启的新启动（QEMU 26908，宿主 10:21:37 启动），时间线、7 份 console 日志与
  两次被排除的假设记在 `docs/evidence/P05/2026-10-21-p05.2/env.txt`。
- 环境事实（P06.2 期间）：`upanel-2404` 又崩两次（第十、十一次 `Attempted to kill init! exitcode=0x8b`，
  console 存为 `console.panic-10`/`console.panic-11`），一次在 P06.2 验收跑到一半、一次在收尾。
  处理与前几轮相同：重启、重传二进制/测试/脚本、必要时删掉被截断的 `/etc/upanel/upanel.json`，
  **重启后重跑原生套件（909/0）与验收（228/0/0）**，结论与崩前一致。不再逐个累计次数，
  只记“这一轮又发生过、最终读数取自重启后”。本轮另外两次 **ssh 退出码 255** 不是 VM 的问题：
  `pkill -f upanel-agent` 与 `pkill -f p06-2-files-write…` 匹配到了 ssh 自己的远端命令行，
  把自己杀了；改用 `pkill -x upanel-agent` 与 `pkill -f "files-write-accept[a]nce"` 后正常。
- 环境事实（P06.3 期间）：`upanel-2404` 本轮**没有** panic，全程未重启（`uptime` 与
  `journalctl -k | grep -c 'Attempted to kill init'` 在 env.txt 里）。两次 ssh 退出码 255 仍是
  `pkill -f` 匹配到 ssh 自己的远端命令行（`pkill -f "/tmp/upanel-bin/upanel"` 把远端 shell 杀了），
  改方括号写法（`"[u]panel-bin/upanel"`、`"[b]ash -i"`）并拆成独立调用后正常。
  另外两件事按“环境事实”记：① 上传到 VM 的二进制与测试二进制都是 0644，执行位要显式补
  （验收脚本里 `chmod +x`，`run-native-tests.sh` 里也补了一句，否则每条都判成“缺少测试二进制”）；
  ② `/tmp/upanel-bin` 必须是 0755，否则以 `upanel-web` 身份跑的 Web 进程穿不过去。

- **P14.5 已实现、宿主已验证、真机验收已通过（2026-10-27）。**
  **已实现**：`internal/accounts` 整层（`definition.go`/`sshkey.go`/`sudo.go`/`parse.go`/
  `commands.go`/`assess.go`/`execute.go`/`errors.go`/`notices.go`/`types.go`/`system.go`/
  `fs.go`/`fetch.go`）——判决（六个动作的预检、阻塞码闭集、执行前复核绑身份与状态版本）
  与执行（`useradd --create-home --shell <闭集壳> --user-group`、`usermod --shell`、
  `userdel` **不带 `-r`**、口令只经 `chpasswd` 的 stdin 且用完清零、authorized_keys 托管区
  整块重写且区外逐字节不动、sudoers 走「候选 `visudo -c -f` → rename → `visudo -c` →
  解析读回 → `sudo -l -U` 读回」五道门，任何一道不过都把文件恢复成逐字节相同的旧版本）；
  迁移 `migrations/agentdb/0014_accounts_ssh_sudo.sql`（表重建补 `display_name`/
  `shell_profile` 两列 + `managed_ssh_keys`/`managed_sudo_rules` 两张表，`ON DELETE CASCADE`）；
  `internal/agentstore/accountstore.go`；代理侧六个动作注册（`internal/agent/accountaction.go`）
  与只读查询 `accounts.ssh_keys`（`accountquery.go`）；`GET /accounts`、
  `GET /accounts/:id/ssh-keys`（admin）；CONTRACTS §13.8；Web（`api/accounts.ts`、
  `AccountsView.vue`、`/accounts` 路由、SharesView 入口、`summarizeAccountJob`，以及账号页
  **自带的 5 分钟再认证**——`POST /secrets` 有自己的 reauth 窗口，第一次点"新建"必然撞上它，
  把它留成"去别处先验一次"就是页面最糟的形态）；真机验收脚本
  `tests/integration/p14-5-acceptance.sh`（1084 行 13 节，已在真机上跑了三轮）。
  **已验证（仅宿主）**：`go build ./...` 干净、`gofmt -l internal cmd` 干净、
  `go test -count=1 ./internal/accounts/ ./internal/agent/ ./internal/agentstore/
  ./internal/api/ ./internal/shares/` 五个包全 ok（`internal/accounts` **37** 个顶层用例）；
  前端 `typecheck`/`lint` 干净、`npm --prefix web run test` **57 文件 1116 用例**全绿
  （`api/accounts.test.ts` 23 个）。其中 `engine_test.go`（**7** 个）是这一轮补上的：
  用假 Store / 假命令执行器 / 假文件系统跑 `ExecuteWrite`，锁住四条最容易静默退化的不变量
  ——口令只出现在 `chpasswd` 的 stdin 且用完全零、`useradd` 的 argv 逐项（没有 uid/gid/
  groups）、`useradd` 退出码 9 拒绝接管既有账号、**门禁二失败时旧 sudoers 逐字节恢复且
  无临时文件残留**、`authorized_keys` 托管区外的行一字不动、删账号不带 `-r` 且连带撤
  sudoers、禁用是换壳不是 `usermod -L`、状态版本过期不动手。这四条还用变异核过：
  去掉 `wipe(stdin)`、掐掉门禁二的恢复分支、给 `userdel` 加 `-r`、把 `ApplyKeys` 改成整份
  重写——四刀全部被对应用例抓红，所以它们不是"写了就算"的绿灯。依据是 `types.go` 里
  `Paths` 的那句注释：这条不变量"必须在单元测试里能反复验证，不能只靠一次虚拟机运行来担保"。
  **真机已验证（2026-10-27，Ubuntu 22.04.5 / kernel 5.15.0-198，VM-A）**：第三轮
  **PASS 168 / FAIL 0 / NOT RUN 6**、退出码 0（`docs/evidence/P14/2026-10-27-p14.5/`）。
  STAGES §16 的四个点全部有真机读数：① 口令只经 `chpasswd` 的 stdin，任务参数里只有
  `sec_` 引用，`passwd -S` 读回 `P`；② uid 由系统分配（`getent` 读回 1002，落在
  1000–59999）、home 在 `/home` 下、登录壳是 `/bin/rbash`；③ `authorized_keys` 里人手写的
  那条外来钥匙在加/换/删三次之后**逐字节不变**，文件 `0600` / 目录 `0700`，`ssh-keygen -lf`
  的指纹与登记表一致；④ sudo 五道门全走了一遍：`0440` + 认领标记 + 无通配符、整份
  `visudo -c` 通过、`sudo -l -U` 里恰好一条授权、`runuser` 换到 alice 之后 `sudo -n`
  **真的执行**了模板命令而模板外的命令被 sudo 自己拒；**真机制造一次 visudo 失败**（候选
  文件自己合格、rename 已发生、整份校验失败）时旧规则文件哈希与内容**逐字节不变**、没有
  残留候选临时文件、失败原因带 `SUDO_VISUDO_FAILED`，移走坏文件后同一操作成功；手改成没有
  认领标记的规则文件被 `SUDO_FILE_DAMAGED` 拒且逐字节保留；禁用是换壳（`passwd -S` 仍是
  `P`，没用 `-L`）；`userdel` 不带 `-r`。sshd 那一面验的是「钥匙能用、口令不是门槛、壳才是
  门槛」——key-only 账号能登，面板禁用后同一把钥匙被拒（sshd 原文
  `This account is currently not available.`），再启用又能登，撤掉公钥后登不上。
  真机抓出并修掉 **2 个产品缺陷**：Q477（`accounts.Definition` 一个 json 标签都没写，
  `display_name` 直接 400——所有单测只构造 Go 结构体、从不走线格式，所以全绿也看不见）、
  Q478（`GET /accounts` 把 Samba 的能力当账号判据，没装 Samba 的机器上账号功能整个不可用）;
  另修掉 **1 条与真机相反的文档断言** Q479（"没口令就登不上"——publickey 认证不看 shadow 的
  口令字段）与两条脚本缺陷，环境那条（`/etc/sudoers.d/` 里非 `0440` 的文件让整份
  `visudo -c` 失败）记进 `docs/TEST_ENVIRONMENT.md` §8.12。
  **NOT RUN（6 条，全部在脚本里列明理由）**：共享账号与 Samba passdb 两半（这台 VM 没装
  `samba-common-bin`，不为一条验收去装包）、两条公钥动作并发写同一份 `authorized_keys`
  （锁由单测覆盖）、第 11 把公钥被拒（上限判定在判决层，单测覆盖）、home 在别处/是符号链接
  的账号（面板拒绝写，只在单测里造过）、面板自己的 TOTP 与登录策略（属于 P14.6）。
  **受阻**：无。
  设计取舍已写进 Q475（`account.apply` 遇 `type=share` 转给 `share.account.apply`）与
  Q476（sudo 模板为什么不带单元参数——sudoers 的 `*` 跨参数）。

- **P14.6（TOTP、一次性恢复码、启用确认、CLI 本机恢复与会话失效）**已实现（六层：纯逻辑、
  秘密存储、代理接口、Web 侧、CLI 恢复、Web UI）、宿主检查全绿，**并在真实 Ubuntu 22.04 VM 上
  验收通过**（第二轮 `PASS 163 / FAIL 0 / NOT RUN 6`、退出码 0，证据
  `docs/evidence/P14/2026-10-27-p14.6/`；真机抓到 1 个产品缺陷 Q480 与 4 类脚本断言错误 Q481）。
  **已实现（第一层）**：`internal/totp`——RFC 6238 的 HOTP/TOTP（HMAC-SHA1、动态截断、6 位）、
  ±1 时间步窗口、`subtle.ConstantTimeCompare` 比较、`MatchStep` 返回匹配到的时间步（给
  重放防护用）、`otpauth://` URI（标签按 unreserved 字符集转义）、一次性恢复码生成/规范化/
  哈希（`sha256:` 前缀 + 常数时间比较）。
  **已实现（第二层）**：`agentstore.Secrets` 的三个方法——`PutInternal`（写入内部用途
  `totp_seed`，跳过"可上传"检查但保留其余校验）、`Binding`（一次查询读用途与引用，
  **不解密**，供归属核对）、`Destroy`（清空密文与 nonce 留墓碑，重复调用幂等）。
  **已实现（第三层）**：`internal/agent/totp.go` 的四条专用路由
  `POST /internal/v1/totp/{enroll,confirm,verify,disable}`——`enroll` 生成种子并以**临时**
  秘密写入（10 分钟自毁）＋回 `secret_ref` 与一次性 `otpauth://` URI；`confirm` 验码通过后
  `Reference` 成持久秘密（引用 `totp:<user_id>`）；`verify` 先 `Binding` 核对用途与归属
  再解密验码，回匹配到的时间步；`disable` 销毁并**幂等**（已销毁回 200 + `destroyed:false`，
  不制造存在性探针）。未接线时四条路由 404，root 与陌生 UID 一律 403（gate 层面）。
  接线在 `cmd/upanel-agent/main.go`（`Totp: secrets`）；契约 §15 的四个表行与一段
  "未确认的登记是临时秘密 / 归属单独核对 / 码不对是结果不是错误"的说明已补齐。
  **已实现（第四层，Web 侧）**：迁移 `0014_user_totp.sql`——`user_totp`（`user_id`
  PRIMARY KEY + FK `ON DELETE CASCADE`、`secret_ref`、`enabled`、`last_step`、时间戳）与
  `user_totp_recovery_codes`（`code_hash` 带 `sha256:` 的 `CHECK`、`used_at`、
  `UNIQUE(user_id, code_hash)`）；**没有明文种子列**，种子只在代理里。
  `internal/auth/totp.go` 是纯状态层：`TotpState`、`EnableTotp`（十枚码与状态行同一笔事务）、
  `DisableTotp`（删行而不是置 `enabled=0`）、`AcceptTotpStep`（一条带 `last_step < ?` 的
  UPDATE 同时完成判断与写入，并发下只有一个成功）、`ConsumeRecoveryCode`（`used_at IS NULL`
  的条件 UPDATE，返回剩余枚数）、`RecoveryCodesRemaining`。`internal/agentclient/totp.go`
  四个方法；`OK && Step <= 0` 视为协议走形（没有步就没法防重放）。
  `internal/api/totp.go` 三条端点 + 共用的 `verifySecondFactor` 门；`internal/api/auth.go`
  的登录拆成 `VerifyCredentials` → `verifySecondFactor` → `FinishLogin`（口令过了但码没过
  时**一个会话都不签发**），`handleReauth` 同样两步（`VerifyReauthPassword` →
  `MarkReauthenticated`），失败计数只在成功时清零（否则"口令已知、码乱猜"会得到一条不受
  限速约束的六位数爆破通道）。**失败关闭**：账号启用了二次验证而 `opts.Totp == nil` 时
  登录与再认证一律 503——一个"代理挂了所以先让他进去"的分支等于给攻击者一个"把代理弄挂"
  的绕过方式。启用/关闭后作废该账号其余会话（保留发起本次操作的那条）。
  **已实现（第五层，CLI 本机恢复）**：`upanelctl totp-reset`——丢了 authenticator 与全部
  恢复码时唯一的入口（那时 Web 上没有任何合法路径）。清状态行与恢复码、作废该账号全部会话、
  记 `auth.totp.reset`（actor 为 `local-cli:<who>` 快照，`actor_id` 为 NULL），并尽力让代理
  销毁种子；代理那一步失败**不阻断恢复**，但输出如实报 `agent_status`
  （`destroyed`/`already_gone`/`unreachable`/`forbidden`/`skipped`）。契约 §14.1 补了这一段。
  **已实现（第六层，Web UI）**：`api/totp.ts`（三条接口 + `secondFactorRequired` 只认
  `details.totp_required === true`）、`components/secondfactor.ts`（`sanitizeCode` 只留数字
  且截六位、`isCodeComplete`、`isRecoveryCodeShape`）、`SecondFactorField.vue`（六个页面共
  用，免得哪一处忘了带码或忘了清空）、`TotpSettingsCard.vue`（登记 → 确认 → 十枚恢复码
  一次性展示 → 两步关闭）、`views/SettingsView.vue` 与 `/settings` 路由（`registry` 里
  `settings` 从"未实现"改成 `P14.6`）、`LoginView.vue` 改两步（401 + `totp_required` 时
  亮出验证码框且**不清已输入的口令**）、`api/auth.ts` + `stores/session.ts` 带上
  `totp_enabled` 与 `signIn(..., totp?)`。四个既有再认证点（`StorageActionDialog`、
  `AccountsView`、`BackupView`、`TerminalView`）全部改成按 `session.totpEnabled` 多带一个
  码，成功后两个字段一起清空。**没有 QR 库**（P00 冻结的依赖集里没有编码器）——`otpauth_uri`
  以文本 + 复制按钮呈现，理由写在组件注释里；扩依赖是独立决策，不在这一层顺手做。
  关闭的两步照服务端约束写进文案：第 1 步的再认证已经用掉当前时间步，所以第 2 步只能用
  **恢复码**或**下一个**六位数。
  **已验证（仅宿主）**：`internal/totp` **14** 个用例全绿，其中
  `TestCodeMatchesRFC6238Vectors` 用的是 RFC 6238 附录 B 那六组向量
  （`94287082`/`07081804`/`14050471`/`89005924`/`69279037`/`65353130`），另有一条从向量
  推出的 6 位读数（8 位值末六位）；`internal/agent` 的 TOTP 用例 **24** 条断言（含子用例）
  全绿，覆盖"响应里除了 URI 没有第二个地方能拿到种子"、信封字段逐键核对、未确认就 verify
  被拒、他人引用在 verify/confirm/disable 三处都 403、拿普通秘密的引用冒充种子 400、
  临时种子过期后要求重新登记、`disable` 两次分别回 `destroyed:true`/`false`、未接线 404、
  方法/Content-Type/对端 UID 三道门。写测试时抓到自己的一个真实缺陷：`url.PathEscape` 按
  path-segment 规则**不转义** `:` 与 `@`，`admin@example.com` 会被 authenticator 应用当成
  "issuer=admin, account=example.com"——改成只放行 unreserved 字符的 `escapeLabel`。
  **已验证（仅宿主）·第四、五层**：新增 **22** 个用例全绿——`internal/api` **9** 个
  （登记全流程含"码错时库里一枚恢复码都没有"、已启用再登记 422、三条路由 admin-only、
  `Totp=nil` 时三条入口 503 且已启用账号登录 503 失败关闭、代理不可用 503、登记过期 409、
  带码登录/缺码 401 + `totp_required`/码错 401/**同一时间步第二次 401 重放**/下一步放行、
  再认证带码、关闭时口令错 403 与码错 401 都不动状态也不去销毁种子、恢复码关闭与二次关闭 404、
  关闭只作废**其它**会话），`internal/auth` **4** 个（十枚码入库全为哈希、重复启用被拒、
  同一/更早时间步被拒且未启用账号不会被建出行、恢复码一次性与大小写/连字符不敏感、
  别人的码无效、关闭连恢复码一起删且可重新登记），`internal/agentclient` **6** 个
  （四条调用的线格式、缺 `secret_ref`/`otpauth_uri` 与 `OK` 无 `step` 判为协议走形、
  `destroyed:false` 不被说成 true、代理 403 保留为 `*Error`），`cmd/upanelctl` **3** 个
  （清状态 + 恢复码 + 作废会话 + 审计 `auth.totp.reset` 且 `actor_id` 为 NULL、
  输出里没有口令与恢复码、代理不可达时恢复仍成功但如实标注、未启用/不存在的账号明确失败）。
  连同此前两层：`internal/totp` **14** 例、`internal/agent` **24** 条断言。
  宿主上两轮全绿：`go test ./internal/api/ ./internal/auth/ ./internal/store/
  ./internal/agentclient/ ./internal/agent/ -count=1`（api 包 166s、agent 包 46s）
  与 `go test ./cmd/upanelctl/ ./internal/totp/ ./internal/audit/ -count=1`。
  **已验证（仅前端单测）·第六层**：新增 **21** 个用例全绿——`src/api/totp.test.ts` **7** 个
  （三条请求的方法/路径/字段名、`disable` 只带调用方真的给了的因子、码错时错误体不加工、
  `secondFactorRequired` 只认 `details.totp_required === true`，口令错误不被说成"请输验证码"）、
  `src/components/secondfactor.test.ts` **8** 个（字母全角符号被洗掉、超六位截断、
  六位才发请求、恢复码大小写与连字符不敏感）、`src/components/totp-card.test.ts` **6** 个
  （SSR：未启用时页面上没有 `otpauth://` 也没有恢复码、已启用时第 2 步的输入框还不出现、
  文案里必须有"下一个"与"作废其它会话"、设置页从会话取 `totp_enabled` 而不凭空显示已启用）。
  连同这六层的前端改动：`npm run typecheck` 无输出、`eslint .` 无告警、
  `npm run test` **60 个文件 / 1137 个用例全绿**（23.5s）。
  **已验证（真机）**：2026-10-27，Ubuntu 22.04.5（kernel 5.15.0-198），第二轮
  `PASS 163 / FAIL 0 / NOT RUN 6`、退出码 0（`docs/evidence/P14/2026-10-27-p14.6/`；同一组
  二进制 agent `0040ed6b…` / web `179ccfa0…` / ctl `8da5b176…`，脚本 `f3c6f442…`）。第一轮
  `PASS 149 / FAIL 11`，11 条红里 1 条是产品缺陷（Q480）、10 条是脚本自己的断言错误（Q481），
  修完再跑才全绿。STAGES §16 两条硬要求的现场读数：
  ① **TOTP 未确认不启用**——setup 之后 session 仍 `totp_enabled=false`、面板库 `user_totp`
  **0 行**（不是 `enabled=0`）、代理侧种子 `state='temporary'`、无码登录 200、带码登录也 200
  （码被忽略，不会顺手启用）、码错 confirm 401 且仍 0 行/0 枚恢复码、带外
  `upanelctl totp-reset` 退出码 1 并明说"没有启用二次验证"、明文种子与 `otpauth://` 在面板库/
  代理库及它们的 `-wal`/`-shm` 五处命中 **0**。
  ② **恢复码只能使用一次**——判据是那个**中间态**：先带码再认证把窗口推到 5 分钟 → 停代理 →
  用 RC1 关闭得 503 `AGENT_UNAVAILABLE`，带外读到该行 `used_at` **已写**（失败也不回滚）、
  状态行与 10 枚码都还在 → 起回代理 → **同一枚** RC1 401「每枚只能用一次」（没有复活）→
  换 RC2 才 200，状态行与十枚哈希一起消失、种子 `destroyed`、另一台设备的会话 401 而发起
  关闭的这一条保留。（不能靠"关掉之后旧码不认"来验：那时整张表都没了，分不清"用过"还是
  "不存在"。）
  另外读到：码由脚本按 RFC 6238 用 python 标准库自己算（独立于产品实现，验的是"面板认不认
  真码"）、同一时间步第二次 401「刚刚用过」且不签发 Cookie、下一个时间步立刻可用（±1 窗口）、
  **确认用的码也算用掉一步**（Q480 的回归断言）、root 跑 `totp-reset` 得 `agent_status=forbidden`
  而 `runuser -u upanel-web` 得 `destroyed`（两种情况下面板库都清干净、会话全废、审计 actor
  是 `local-cli:*` 且 `actor_id` 为 NULL、输出里没有口令也没有恢复码）、前端产物里
  `二次验证`/`/auth/totp`/`只显示这一次`/`同一个码不能用第二次` 四处文案都在。
  修完 Q480 之后宿主再跑一遍 `sh scripts/check.sh`：**PASS 14 / FAIL 0 / NOT RUN 0**
  （line-endings、go-build、go-vet、go-test、go-build-linux、go-vet-linux、gofmt、
  node-version、web-typecheck、web-lint、web-test、web-build、web-smoke、openapi-lint 全绿）。
  **未实现**：账号页上的"还剩几枚恢复码"——需要一条 `GET /auth/totp` 只读端点，
  本轮不开新接口，恢复码余量仍只能靠 `upanelctl totp-reset` 的输出与关闭流程看到。
  **未验证（真机范围外，逐条有理由，均为 NOT RUN）**：真浏览器里从登记点到登录的交互
  （这台 VM 上没有浏览器；由前端 21 例 SSR/请求形状单测 + 本轮接口读数共同覆盖）、
  二维码图片（P00 冻结依赖集里没有 QR 编码器，界面按文本 + 复制按钮呈现 `otpauth_uri`）、
  手机与服务器时钟相差超过 ±1 步（要改 VM 时钟，会同时搅乱窗口/过期/限速；边界由
  `internal/totp` 的 RFC 6238 向量覆盖）、登录限速打满（本轮刻意只制造 4 次失败，打满之后
  所有断言都会变成 429）、24.04 那台 VM（本轮只跑 22.04：P14.6 的对象全在账号/代理层，
  与发行版无关；全套回归由 P16.2 补上——24.04 上七相位 147/1/3（2026-10-27 那轮，唯一那条红是模拟器引导慢），最终交付字节上是 155/0/3；页面链路也在同一份字节上补齐（22.04 43/43、24.04 35/35 + 43/43），见 §5 的「P16.2」「P16.2 补记」与「P16.2 补记之二」）。
  **已知残留（写进账本，不装作没有）**：以 root 运行 `totp-reset` 时代理侧那条密文会留下
  （SO_PEERCRED 只认 `upanel-web`，root 被拒）。它已无人引用、任何登录都不会再校验它，
  但密文仍占着代理秘密库的一行；彻底清理要么以 `upanel-web` 身份重跑，要么留给代理做一次
  孤儿 `totp_seed` 清扫（P16 的运维项，尚未实现）。真机第 7 节读到的 `persistent=1` 就是它，
  并且顺手验过：那条孤儿密文里没有明文种子（`grep` 命中 0）。

## 2. 状态定义

- not_started：未开始。
- in_progress：正在实施，验收未全部通过。
- blocked：缺少条件或存在必须先解决的问题。
- passed：该阶段全部子任务与验收完成，证据可查。

“代码已写完但未跑真实系统测试”不是 passed。发现阻断性回归后必须撤销相关 passed 状态。

## 3. 阶段状态

| 阶段 | 内容 | 状态 | 证据/备注 |
|---|---|---|---|
| P00 | 基线和验收环境 | **passed** | P00.1/P00.2/P00.3 完成；VM-A(22.04)/VM-B(24.04) 已创建并核验，序列号与隔离证据齐全。`docs/evidence/P00/2026-10-20/`（VM-C/VM-D 未建，不属 P00 验收项） |
| P01 | 最小可运行骨架 | **passed** | P01.1 骨架/配置/日志/健康检查、P01.2 SQLite 迁移/事务/错误码/request_id/Gin 路由、P01.3 前端骨架/路由/中文导航/统一 API 客户端/七状态组件/mock 横幅、P01.4 前端嵌入二进制/mock 与 production 配置/systemd 单元/本地启动文档，均有宿主 + 两台 VM 证据；P01.4 各 VM 114 项断言通过。证据 `docs/evidence/P01/2026-10-20*/` |
| P02 | 认证、权限、审计、代理 | **passed** | P02.1 **passed**：users/sessions 表、Argon2id、登录/退出/会话轮换/CSRF/Origin/限速、`upanelctl init-admin`（无默认密码），证据 `docs/evidence/P02/2026-10-20-p02.1/`（宿主 38 项 + 两台 VM 各 58 项断言）。P02.2 **passed**：admin/viewer 边界、账号增删改查（脱敏 DTO + 分页 + 乐观并发）、最后管理员保护、5 分钟再认证、追加式审计表，证据 `docs/evidence/P02/2026-10-20-p02.2/`（宿主 81 项 + 两台 VM 各 99 项断言，P02.1 回归各 58 项）。P02.3 **passed**：代理 socket 与 SO_PEERCRED UID 边界、固定登记表（生产为空）、结构化只读查询、命令执行器超时/输出上限、systemd 真实托管，证据 `docs/evidence/P02/2026-10-20-p02.3/`（两台 VM 各 84 项断言 + VM 内核原生 28 + 9 个用例，P02.2 回归各 99 项）。P02.4 **passed**：审计跟随业务事务（被拒绝请求独立记 failure 行）、日志脱敏做进记录器、代理独立 root-only 库（`/var/lib/upanel-agent/agent.db`，目录 0700 / 文件 0600，启动自校验）、AES-256-GCM 秘密存储（独立 nonce、AAD 绑引用与用途、`/etc/upanel/agent.key` 0600 且 `-init-key` 不覆盖）、临时秘密 10 分钟销毁与墓碑行、无明文读取路由，证据 `docs/evidence/P02/2026-10-20-p02.4/`（两台 VM 各 87 项断言 + VM 内核原生 agentstore 32 / agent 45 个用例，P02.1/P02.2/P02.3 回归各 58 / 103 / 84 项）。**阶段结论：P02 全部子任务完成** |
| P03 | 预检、任务、幂等、对账 | **passed** | P03.1 **passed**：预检由代理生成（规范化参数、目标身份指纹、状态版本、影响、阻塞项、锁、再认证与短语策略），Web 只保存绑定并返回公开 DTO；有效期 120 秒且两端同一时间戳；用户/会话/动作/`params_hash`/`state_version` 双侧一致；短语只回一次明文、代理库只存哈希；未登记动作 400、缺规划器 501、`GET` 405。证据 `docs/evidence/P03/2026-10-20-p03.1/`（两台 VM 各 93 项断言 + VM 内核原生 5 个测试二进制，P02.1—P02.4 回归各 58/103/84/87 项）。P03.2 **passed**：持久 jobs、幂等链、`operation_id` 去重、代理执行记录与事件、资源锁与 30 秒排队、取消规则、先落库再提交、按 `operation_id` 对账；证据 `docs/evidence/P03/2026-10-20-p03.2/`（两台 VM 各 101 项断言 + VM 内核原生 169 用例 0 FAIL + p01-1/p01-2/p01-4/p02-1—p02-4/p03-1 回归各 29/34/114/58/103/85/87/93；宿主 check.sh PASS 12/0/NOT RUN 0）。验收期间抓到并修掉两个真缺陷：`CONFIRMATION_MISMATCH` 应为 400（DECISIONS Q64）、SQLite WAL DEFERRED 事务升级不调 busy handler → `_txlock=immediate`（DECISIONS Q65）。P03.3 **passed**：SSE 事件流（`id:`/`event:`/`data:`、`: connected`、15 秒 keepalive、`Last-Event-ID` 续传、日志环被裁后先发 `event: gap`）、代理事件镜像（先事件后状态）、日志分页与每任务 1000 行上限、任务中心与错误详情、最小登录页与路由守卫；证据 `docs/evidence/P03/2026-10-20-p03.3/`（两台 VM 各 73 项断言 + 原生 346 用例 0 FAIL + P03 前序回归）。验收期间抓到并修掉代理侧“阶段与事件分两笔写”的原子性缺陷（DECISIONS Q67）。P03.4 **passed**：Web 启动立刻 + 每 30 秒的对账循环（只查不跑，`grep CreateOperation` 在 `reconcile.go` 无调用点）、提交在途不判定、`NOT_FOUND` 按 `needs_sync` 分流（`failed`/`unconfirmed` vs `interrupted`/`unknown_result`）、幂等记录到期清理、验收注册表约束变成断言；证据 `docs/evidence/P03/2026-10-20-p03.4/`（两台 VM 各 78 项断言 + 原生 360 用例 0 FAIL + 全量 11 脚本 0 失败）。**阶段结论：P03 全部子任务完成，生产登记表仍为空（无真实写动作）** |
| P04 | 首页和存储只读 | **passed** | P04.1 **passed**：`internal/monitoring` 纯文件读取采集（`/proc/stat`、`meminfo`、`loadavg`、`net/dev`、`os-release`、`uptime`、`cpuinfo` + `uname`），代理侧 5 秒轮询缓存（`collected_at`/`interval_ms`/`stale`/`degraded` + 分节 `status`），速率只由相邻样本差分给出、算不出来为 `null`，`boot_id` 变化让 CPU 窗口失效；查询登记表新增唯一查询 `system.summary`（`DecodeParams` 拒绝任何字段），Web 侧 `GET /api/v1/system/summary`（权限 V）只透传、不做第二层缓存，代理不可用 503 `AGENT_UNAVAILABLE` 且不影响登录；能力只上报真实存在的东西。证据 `docs/evidence/P04/2026-10-20-p04.1/`（两台 VM 各 **72** 项断言 + VM 内核原生 **401** 用例 0 FAIL + 12 脚本全量回归 930/0；宿主 check.sh PASS 12/0/NOT RUN 0，redocly valid 9 warnings）。**生产登记表仍为空，本轮没有新增任何写动作**。P04.2 **passed**：`internal/storage` 只读块设备拓扑（`/sys/block` + `/sys/class/block`，整盘/分区/lvm/raid/crypt/loop/rom/other）、边 `partition_of`/`depends_on`（from=子→to=父）、设备身份 `sha256:` 指纹 + 诚实的 `identity_confidence`、系统盘保护从 mountinfo/swaps 出发沿边递归向上、分区表读盘头部只补类型与 free_regions（内核结果优先）、容量 30 秒缓存取 `f_bavail`、`snapshot_version` 只由内容决定；代理查询 `storage.topology`、Web `GET /api/v1/storage/{topology,devices,devices/:id}`（V 权限、只透传）；`health.status` 一律 `unknown`、`capabilities` 一律 `[]`。证据 `docs/evidence/P04/2026-10-20-p04.2/`（两台 VM 各 **108** 项断言 + VM 内核原生 **473** 用例 0 FAIL + 13 脚本全量回归 1038/0；宿主 check.sh PASS 12/0/NOT RUN 0）。本轮修掉 Q89（块设备被当普通文件读 → 代理 OOM）、Q90（快照版本依赖时间）、Q91（by-id 链接缺已知设备约束）、Q96（一个测试抖动）。P04.3 **passed**（SMART/NVMe 健康与 diskstats：15 分钟采集节奏、睡眠盘不唤醒）——`internal/storage` 新增 `smart.go`/`diskstats.go`/`capacity*.go`：smartctl `-j -H -A -i` 按**退出状态位掩码**判读（失效/预失效 → `critical`，历史位/日志位 → `warning`，命令失败/打开失败 → `unknown`），先读 sysfs 电源状态、非 NVMe 再加 `-n standby`，**不为读健康唤醒休眠盘**；`/proc/diskstats` 每台设备只取自己那一行、计数转十进制字符串，速率由相邻两次采样差分（归零/回绕/窗口异常 → `null`），平均延迟只在完成次数 > 0 时给，扇区固定 512 字节；网络文件系统容量在代理自己的 `--statfs-helper` 子进程里查（并发 2、默认 10 秒超时、失败后隔离 5 分钟不再重试），拓扑新增 `network_mounts`（无块设备的网络挂载单独成表）、`Device.io`、`Mount.capacity_error`，`io` 不进 `snapshot_version`。证据 `docs/evidence/P04/2026-10-21-p04.3/`：24.04.5 与 22.04.5 各 **77** 项通过、`upanel-smart`（QEMU NVMe）**62** 项通过，0 失败（未执行项各 1/1/2，均写明原因）；**statfs 挂在内核里时登录仍 332 ms / 271 ms 返回 200**；原生 `storage.test` 三台 VM **103/0**；22.04 VM 内 `go test -race ./...` 19 包 ok、**0 条 DATA RACE**。P04.3 补强（第二轮）：QEMU USB 盘的两个真实反例（“桥不认识”“`-d sat` 打不开”）+ 真实 `mount --bind` 多挂载点用例 + `smartctl` 原话透进 `health.details`（Q104），三台 VM 重跑 77/77/**63**、0 失败，原生 `storage.test` **106/0**、`-race` 19 包 ok / 0 DATA RACE。P04.4 **passed**（首页、磁盘列表、拓扑/分区详情、健康与容量卡片；**写操作能力此时不启用**）：前端 DTO 与 `api/openapi.yaml` 同源，页面把 `null` 一律显示 `—`、容量与百分比照实、`unsupported/unknown` 不挂告警、`capabilities` 为空就明说没有动作；Playwright 冒烟 31/0/5 + dist 预览 10/10，前端 **192** 用例；VM 上四份真实响应载荷校验 **856/0**、脚本 **31/0 NOT RUN 0**；证据 `docs/evidence/P04/2026-10-21-p04.4/`。**P04 到此全部 passed** |
| P05 | 服务、进程、日志 | **passed** | P05.1 **passed**：服务列表/详情/保护名单 + 预检→任务→读回的执行链路（`internal/services`、`internal/api/services.go`、`internal/agent/service*`、前端列表与详情页）；VM 上 **PASS=140 / FAIL=0 / NOT RUN=3**、载荷校验 **22369/0**，前端真实载荷回放 22 例、全量 252 例，宿主 `check.sh` PASS 13/0/0；证据 `docs/evidence/P05/2026-10-21-p05.1/`。P05.2 **passed**：进程列表与信号（`internal/processes` 纯 `/proc` 采集、`pid`+`start_time_ticks` 身份、5 秒 2000 条快照、10 状态 7 类保护闭集、命令摘要脱敏；`process.signal` 生产动作走同一条预检→任务→锁→读回链路，TERM 与 KILL 两次独立操作、KILL 再认证 + 一次性短语、受保护进程零能力、执行前现场重读 `/proc` 核对身份与保护）；VM 上 **PASS=128 / FAIL=0 / NOT RUN=0**、载荷校验 **9250/0（1 SKIP）**，PID 重用由 `ns_last_pid` 真机造出并验证旧预检被拒、新进程安然无恙；前端真实载荷回放 31 例、全量 295 例，宿主 `check.sh` PASS 13/0/0；证据 `docs/evidence/P05/2026-10-21-p05.2/`。P05.3 **passed**：日志（`internal/journal` 真实 journalctl 结构化查询与跟随流、`internal/logs` Web 侧契约校验、`logs.query` 代理查询 + `/internal/v1/streams` 单次许可的 NDJSON 流、`GET /api/v1/logs` 与 `GET /api/v1/logs/stream` 两条 admin 接口，前端查询页/条目行/实时流）；VM 上 **PASS=132 / FAIL=0 / NOT RUN=1**、载荷校验 **6466/0/0**，9 个真实缺陷被真机抓出并修掉（游标与 `--since` 冲突、`--kernel`、未转义 `;` 被静默丢弃、参数错误 500→503、`limit=0`、流不 flush 头、错误消息三层前缀），**结束订阅不残留 journalctl 子进程**与空闲超时收流都在真机上验过；前端真实载荷回放 24 例、全量 347 例，宿主 `check.sh` PASS 13/0/0；证据 `docs/evidence/P05/2026-10-21-p05.3/`。**P05 到此全部 passed** |
| P06 | 文件和受限终端 | **passed** | P06.1 **passed**：受管理文件根 + 句柄级路径约束 + 列表/文本/下载（`internal/files` 双角色一套 DTO、`openat2` 单次解析 + 同句柄 `fstat`、根身份每次核对、配置拒绝 `/`；`files.roots\|list\|text` 三个查询 + `files.download` 单次许可流；`GET /api/v1/files/{roots,list,text,download}` 四条 admin 接口；前端文件页/条目行四态与“不跟随、不读取”说明）。VM 上 **PASS=115 / FAIL=0 / NOT RUN=0**、载荷校验 **6379/0**，前端真实载荷回放 27 例、全量 406 例，宿主 `check.sh` PASS 13/0/0；证据 `docs/evidence/P06/2026-10-21-p06.1/`。P06.2 **passed**：流式暂存上传与受控写入（`internal/files/staging.go` 声明即预留/收满即归还、根内 `.upanel/staging`、TTL 24 h；`write.go` 编辑备份 10 份 + 500 MiB；`StatTree` 树统计与 `TREE_TOO_LARGE`/`TREE_UNSUPPORTED`；七个写动作 `file.mkdir\|write_text\|copy\|move\|delete\|chmod\|chown` + `commit_upload` 全部走预检→任务→幂等→资源锁→`renameat2` 原子换位→同句柄 `fstat` 读回→审计；上传三件套与四条 `/api/v1/files/uploads*` 接口，字节经代理单次许可通道、不进 job 表；前端上传/新建/重命名/复制/删除/编辑/改权限改所有者与确认短语弹窗）。VM 上 **PASS=228 / FAIL=0 / NOT RUN=0**、VM 内核原生 **909/0**、宿主 `check.sh` PASS 13/0/0、前端 428 例、openapi valid；真机抓出并修掉 4 类 6 个缺陷（Q155—Q158）；未验证项 8 条逐条写在 result.md §5；证据 `docs/evidence/P06/2026-10-21-p06.2/`。P06.3 **passed**：受限 Web 终端（`internal/terminal` 会话表 + 限额收紧 + Linux PTY 降权启动与身份读回 + 显式挂断；`internal/agent/terminalaction.go\|terminalstream.go` 动作 `terminal.create`、查询 `terminal.sessions`、WS 流；`internal/api/terminal*.go` 四条接口 + 一次性许可 + WS 中继；前端 `TerminalView.vue` 六态与“不是 root shell”边界提示）。浏览器→Web→代理两跳 WS，四道门（Origin/子协议/许可/代理附加）全在 upgrade 前，帧闭集 + 64 KiB 载荷 + 256 KiB/s 与 200 帧/s 上限 + 关闭码 1000/4001/4002，两状态 + 单连接 409 + 宽限重连同壳，审计只记元数据。VM 上 **PASS=282 / FAIL=0 / NOT RUN=0**、VM 内核原生 **962/0**、宿主 `check.sh` PASS 13/0/0、前端 445 例、openapi valid；真机抓出并修掉 6 类缺陷（Q165—Q169，其中 Q169“关 master 不会挂断”会话结束后 shell 残留）；NOT RUN 5 条见 result.md §6；证据 `docs/evidence/P06/2026-10-21-p06.3/`。**阶段结论：P06 三个子任务全部完成** |
| P07 | 分区、格式化、挂载 | **passed** | P07.1 **passed**：存储写动作的**拒绝逻辑**（预检判决、可靠设备身份、整盘锁、系统设备/挂载/swap/holders/依赖保护）。四个动作 `storage.partition_table.initialize\|partition.create\|partition.delete\|filesystem.format` 只登记解码 + `Plan`，`Execute` 为 nil，提交必 501 `NOT_IMPLEMENTED`（Q170），因此**这一轮没有任何一次写盘**。判决层 `internal/storage/assess.go` 以“读不出来不等于可以写”为第一原则（签名读不到/`/proc/*/fd` 扫不动/分区表只有 sysfs/设备类型未知 → 一律阻塞），保护沿 `partition_of`/`depends_on` 递归向上，阻塞码是闭集；能力投影由判决层算出、30 秒 TTL；签名识别 ext/XFS/Btrfs/swap/LUKS/LVM2/md 固定魔数，占用按 `st_rdev` 比对。VM 上 **PASS=189 / FAIL=0 / NOT RUN=1**（`PREFLIGHT_STALE` 无法验证——提交在复核之前就 501）；真机抓出并修掉 **2 个**产品缺陷：交换区魔数在第一个页面的末 10 字节（4096−10）而不是盘尾、`holders/`/`slaves/` 是符号链接目录没有 `list` 属性（导致 dm/md 占用在真机上永远看不见），见 Q171/Q172。证据 `docs/evidence/P07/2026-10-21-p07.1/`。**P07.2 passed**：三个分区动作接上执行器并在 VM 的三块一次性空盘上**真的写盘**——固定模板（`label: gpt` / `--append --wipe-partitions never start=… size=… type=linux name="…"` / `--delete <dev> <n>`）、模板只经 `syscmd` 的有上限 stdin 通道、命令行里只有已校验的数字与名字；写前重算判决并比对身份指纹/状态版本，写后 `udevadm settle` + 等内核与 `/dev` 节点 + 读回盘上 GPT 头与条目表与期望形态逐条比对，两源都对上才 `verified`（退出码 0 不算）。契约新增 `partition_table.disk_guid`/`disk_entries`（CONTRACTS §6.3、openapi `StorageGPTEntry`、前端 DTO 与真机载荷回放）。VM 上 **PASS=177 / FAIL=0 / NOT RUN=1**，真机抓出并修掉 Q177（`disk_entries` 空数组与“没读到”撞车），记下 Q178—Q182；宿主 `check.sh` PASS 13/0/0、前端 450 用例。证据 `docs/evidence/P07/2026-10-22-p07.2/`。**P07.3 passed**：`storage.filesystem.format` 接上执行器（`internal/storage/format.go` 448 行 + `fsprobe.go`）——固定模板里只有已校验的文件系统名、设备路径与卷标（卷标另有第三道 `ErrUnsafeWrite` 闸），写完**直接读超级块**做两源核验（ext4 偏移 1024 的 `53 ef`/`s_uuid`/`s_volume_name`，XFS 偏移 0 的 `XFSB`/`sb_uuid`/`sb_fname`），退出码 0 不算验证。四条纪律落地：错短语 400 且**整份预检就此作废**；标签超限在预检之前就是 400（ext4 的 17 字节标签会 rc=0 并被静默截断，Q184）；mkfs 跑着时取消是 409 且任务不会被标成 canceled；代理被 SIGKILL 后操作落 `interrupted` + `OPERATION_INTERRUPTED` 且**不自动重跑**；执行前二次身份检查用两道门——版本比对（结构变了 → 409 `PREFLIGHT_STALE`）与设备直读（字节换了 → 409 `DEVICE_IDENTITY_CHANGED`）。VM 上 **PASS=204 / FAIL=0 / NOT RUN=1**（唯一 NOT RUN 是内核/盘上条目不一致的真实竞态，单测 `TestFormatRefusesPartitionThatMoved` 覆盖），真机抓出并修掉 **1 个**产品缺陷 Q190（对已经不存在的设备做预检回 500：拒绝路径没有目标可绑定，预检层却无条件要求绑定），记下 Q184—Q190；宿主 `check.sh` PASS 13/0/0、`go test ./... -count=1` 26 包 ok、storage 单测 PASS 328 / FAIL 0（P07.2 提交同口径 291，+37）。证据 `docs/evidence/P07/2026-10-22-p07.3/`。**P07.4 passed**：受管理卷——`internal/storage/fstab.go`（760 行）/`volume.go`/`volumeassess.go`（960）/`volumewrite.go`（878）/`volumeholders.go` 把“一块已有文件系统的分区”变成面板管理的卷，五个动作 `storage.volume.import\|mount\|unmount\|configure\|forget` 全部接上执行器（**都不要确认短语**：短语只留给“写下去就找不回来”的动作）。fstab 只做**最小修改**（块外逐字保留、选项顺序固定、`nofail` 恒在、`noauto` 随 automount、第六列 ext4=2/XFS=0），写路径“比哈希 → 渲染候选 → 临时文件 → fsync → `findmnt --verify` → 备份 → rename → fsync 目录”，候选不合格目标文件一个字节不动，渲染结果与原文相同则直接返回；**禁用 `mount -a`**。占用列表把 fd 的 `st_rdev` 与挂载点下文件的 `st_dev` 合流，“挂着但空闲”不算忙，读不到 `/proc` 一律 fail-closed。VM 上分两段（中间隔一次真重启）：第一段 **PASS=267 / FAIL=0 / NOT RUN=0**，第二段 **PASS=38 / FAIL=0 / NOT RUN=0**——开机 26 秒时 `srv-upanel-volumes-data.mount` 由 fstab 自己挂上（之后才启动面板）、UUID 一致、标记文件 sha 不变、`noauto` 的卷没有挂上。真机抓出并修掉 **1 个**产品缺陷 Q199（configure 预检漏绑定目标身份指纹 ⇒ 500），记下 Q191—Q203。证据 `docs/evidence/P07/2026-10-22-p07.4/`。**P07.5 passed**：磁盘页面完整交互 + 系统外部独立检查 + 存储恢复文档 + openapi 补齐 P07。前端五个新组件（`DeviceStorageActions`/`VolumeImportForm`/`VolumeCard`/`VolumeSection`/`StorageActionDialog`）+ `api/volumes.ts`、`utils/jobresult.ts` 把九种动作接上页面：判定只认载荷里的 `capabilities`（`available` 才可点、`blocked` 给服务端理由、`unsupported` 不出现、载荷无 `capabilities` 时一个动作都不画，Q205），`capacity` 只在真挂着时存在（没挂写「没有容量读数」而不是 `0 B`），每次动作走 `/operations/preflight` → `/jobs`（带幂等键），预检 120 秒过期要重新预检、要短语的必须原样敲、要再认证的提示重新输密码。`api/openapi.yaml` 补 `GET /storage/volumes`、`/storage/volumes/{id}`、`/storage/volumes/{id}/blockers` + 五个 schema，版本 `0.0.0-p07.5`，`capacity`/`observed_state`/`dependencies` 的空值语义写进 description。`scripts/storage-external-check.sh`（381 行）是**不经过面板的独立证人**（只读 `/etc/fstab`、`findmnt --mountpoint`、`/proc/self/mountinfo`、systemd 单元、备份权限，退出码 0=一致），真机上跑三遍真的 `/etc/fstab` + 五种坏副本；`docs/STORAGE_RECOVERY.md` 写清人怎么恢复（§2 第三步用 `--mountpoint`，Q204）。夹具从真机抓回 `web/src/fixtures/p07-5-vm/`（11 份载荷 + README），由 `realdata-p07-5.test.ts` 回放。VM 上 **PASS=205 / FAIL=0 / NOT RUN=3**（浏览器渲染 / 跨重启 / 九种动作逐一点击，逐条写原因与替代证据），真机抓出并修掉 **1 个**产品缺陷 Q208（把“未挂载所以读不到类型”当成“不合格”，于是纳入管理的候选在任何真机上都是空的——**夹具比现实干净，测试就变成了橡皮章**），记下 Q204/Q209 两个验收脚本缺陷与 Q205—Q207；宿主 `check.sh` PASS 13/0/0、web 35 文件 / 552 用例、openapi valid（7 warning）、`go test ./...` 26 包 ok（本轮没改 Go 代码，条目数与 P07.4 一致）。证据 `docs/evidence/P07/2026-10-22-p07.5/`。**阶段结论：P07 五个子任务全部完成** |
| P08 | SMB 与卷依赖 | **已完成**（P08.1—P08.4 全部 passed）| P08.1 **passed**：Samba 能力检测、受管理账号、密码秘密通道、POSIX ACL、共享定义与配置版本。VM 上 **PASS=382 / FAIL=0 / NOT RUN=4**；`share.apply` 只到 `accepted`（不写 `smb.conf`、不重载，`sha256` 首末一致），密码明文只走 `smbpasswd` 的 stdin（五处扫描 0 命中），账号与 ACL 均用系统自己的命令读回核验。真机修掉 4 个产品缺陷（Q214/Q220/Q221/Q223），钉死 P05.1 采集器性能问题（Q222，未根治），记下 5 处验收脚本缺陷；证据 `docs/evidence/P08/2026-10-22-p08.1/`。**P08.2 passed**：托管 `upanel.conf` 整份重写 + `smb.conf` 末尾只追加两行（原有内容逐字节前缀比对保留），候选/合并两次 `testparm`、备份 0600 留 40 份、rename + 目录 fsync 的原子更新、`smbd` 活着才 `smbcontrol all reload-config`、逐参数读回、真实 `smbclient` 读写与拒绝、`smbstatus` 会话表连接状态。VM 上 **PASS=237 / FAIL=0 / NOT RUN=0**（四遍）；真机抓出 **1 个**产品缺陷（Q233 上级目录 0700 导致真实客户端 `ACCESS_DENIED`，7 条断言 1 个根因）+ **1 个**契约事实（Q234 `hosts allow` 对环回永远放行），2 处验收脚本缺陷；证据 `docs/evidence/P08/2026-10-22-p08.2/`。**P08.3 passed**：共享与 `volume_id` 绑定、启用检查、启动依赖、运行时脱盘、卸载/格式化依赖阻断。挂载门（只有 `volume_state=mounted` 才渲染，`unknown`/空按没挂处理）、30 秒一轮**只撤不恢复**的监视循环（撤销现场 `runtime_revoked{at,volume_id,volume_name,volume_state,reason}` 落在共享记录上，人再应用才回来）、systemd drop-in `/etc/systemd/system/smbd.service.d/upanel-shares.conf`（`RequiresMountsFor=` 一行四路径、排序去重、原子写、`daemon-reload` 必做、清空即整份删除）、存储侧按 `purpose` 分档的反向约束（卸载只看启用中的共享，格式化/退出管理看任何共享，查不出依赖一律阻塞）。VM 上 **PASS=207 / FAIL=0 / NOT RUN=5**（三遍：184/14 → 203/2 → 207/0），另用三阶段脚本连同一次真 `reboot` 验完重启顺序（pre 18 / post 11 / clean 5，各 0 失败，挂载单元比 `smbd` 早 57.2 秒进入 active，`smb.conf` 重启前后逐字节相同）。真机抓出 **2 个**产品缺陷（Q240 少一列导致 `runtime_revoked` 从不落盘且幂等判据永远失效；Q243 `0644` 被 umask 掩成 `0600`），**2 处**脚本缺陷（Q241、Q244），1 处契约补记（Q242）；证据 `docs/evidence/P08/2026-10-22-p08.3/`。**P08.4 passed**：共享 UI、用户权限与客户端连接说明（不加接口、不改字段）。三个准备状态分开显示；卷的状态与开机依赖**都不在共享记录里**——卷的挂没挂由页面按 `volume_id` 从 `/storage/volumes` 的 `observed_state` join（取不到就少显示一行；`unknown`/`error`/`mismatch` 说“读不出来”而不是“没挂”），开机依赖在 `share.apply` 的 `result.data`；客户端说明四种（Windows/macOS/`mount.cifs`/`smbclient`），地址取自 `window.location.hostname`，命令里永远没有密码（`credentials=` + `0600` + here-doc 走 stdin）。VM 上 **PASS=190 / FAIL=0 / NOT RUN=4**（四遍：167/20 → 187/2 → 190/0），第 5 节逐字执行说明里的 11 行（真 `smbclient` + 真 `mount.cifs` 挂载、写入读回、`umount`），第 6 节四种身份全拿真实 `NT_STATUS_*`，第 7 节 58 份载荷 / 两个库 / `/proc` 两处 / `/proc/mounts` 命中 0。真机抓出 **2 个**产品缺陷（Q246 前端 DTO 写着后端从不发送的四个字段，“卷没挂”永远亮不了；Q247 “重新应用”这条唯一的出路走不通，`runtime_revoked` 从来没被清过），**5 处**验收脚本缺陷；宿主 `check.sh` PASS 13/0/0、前端 637 条用例全绿；证据 `docs/evidence/P08/2026-10-23-p08.4/`、回放夹具 `web/src/fixtures/p08-4-vm/`。`P00-VM-D`（独立 SMB 客户端 VM）仍未建立，跨机器真实网段访问属 **NOT RUN** |
| P09 | 现有 Web 应用与 Docker | **已完成**（P09.1—P09.5 全部 passed）| P09.5 **passed**：应用卡片、搜索、分组、一键打开、状态区分与常用入口首页集成——**只动前端，不新增接口**。卡片四件事分开画（`runtime_state` / `http_state` / `dependency_protection` / `runtime_note`），「容器在跑但回 500」同时写“运行中”与“入口异常”；按钮按服务器给的 `capabilities` 画（link 只有 `open`、受保护单元只剩 `logs`、看不见的绑定一个都不给）；`open` 是锚点（`target="_blank"`、`noopener noreferrer`）不是请求，地址是服务器地址——真机入口 `http://10.0.2.15:18897/` 回 200 而同端口回环 000，回环/`localhost`/带凭据/`javascript:` 的入口在写入口就 400；启停只落 `service.control`/`container.control`（不引入 `app.control`，那条路会绕过依赖卷门禁）；收藏按用户、幂等；搜索在页面（`GET /apps?q=` 结果集不变）；非空分组删不掉且成员不被顺手移走；`group_id` 形状对但不存在 → 200 + 空、**形状不对 → 400**（Q277，改的是 openapi 的描述不是代码）。VM 上 **PASS=211 / FAIL=0 / NOT RUN=0**，真机**无产品缺陷**（4 处失败：1 处契约文字 + 3 处脚本毛病）；前端 40 文件 / 720 用例、真机载荷回放 51 条断言、宿主 `check.sh` PASS 13/0/0；证据 `docs/evidence/P09/2026-10-22-p09.5/`、回放夹具 `web/src/fixtures/p09-5-vm/`。P09.4 **passed**：应用摘要里的状态/按钮/日志/依赖保护全部**来自底层对象的观测**，并在启动路径上加一道**只能收紧**的依赖卷门禁。`internal/appruntime` 一次请求内合成三件事：状态逐来源枚举（systemd `active`/`reloading`→`running`、`failed`→`failed`、`activating`/`deactivating`→`unknown`、单元不在列表→`unknown`；docker `running`→`running`、`stopped`→`stopped`、其余→`unknown`）、**能力来自对象自己的能力集合**（不再由 `type` 推导）、保护判定。三处容易写错的钉死了：**`unknown` 不是 `stopped`**（绑在不存在的单元/被重建的容器上要人重新确认绑定，而不是给一个点下去必然失败的按钮）、**受保护单元仍有日志能力**（`ssh.service` 三项生命周期全 false、`logs` 为 true）、**门禁只拦启动**（`stop` 永不拦——卷掉线时最想做的就是停机，拦它只会把人逼回命令行、绕过审计）。门禁 `internal/appruntime/gate.go` 跑在 `CreatePreflight` 已有判决**之后**，只能把 `allowed` 从 true 改成 false（Q273；依赖关系只在 Web 库里，代理不该知道应用元数据）：未挂载→`VOLUME_OFFLINE`、身份不符→`VOLUME_IDENTITY_CHANGED`、读不到卷清单→`VOLUME_CHECK_UNAVAILABLE`（fail closed），提交被钉住的预检是 409 `DEPENDENCY_BLOCKED`，真机用 `docker inspect` 现场对照容器确实没动。`managed` 只给「docker + `restart` 是 no/空 + 有依赖卷」（Q274）——它说的是“经面板发起的启动会被检查”，不是“依赖被接管了”；手工 `docker start`、开机自启、`systemctl enable` 过的单元、`restart: always` 四条绕过路径都在 note 里点名，这几种一律 `unmanaged`。`GET /api/v1/apps/{id}/logs` 按绑定判别 journal / docker，两种 `page` 形状各自与 `/logs`、`/containers/{id}/logs` 一致；`unit`/`source` 这类“想改对象”的参数一律 400（对象来自绑定）。两条**没做**的明写着而不拿门禁冒充（Q276、Q275）：运行时卷失联自动停止要代理侧常驻协调者，本轮不做；docker 的 `failed` 这一版不区分（DTO 里没有 ExitCode，非 running 一律 `stopped`）。VM 上 **PASS=165 / FAIL=0 / NOT RUN=0**（11 节），**真机没抓出产品缺陷**，10 处失败全是脚本自己的毛病（子壳变量、拿受保护单元当夹具、`/storage/volumes` 读成 `data.items`、`lsblk -r` 与 `-l` 在 util-linux 2.39 互斥、拿旧服务快照断言新单元、把“改完之后的版本号”当乐观锁、`pfget` 拼路径去跑表达式）；宿主 `check.sh` PASS 13/0/0、`go test -count=1 ./...` 全绿；证据 `docs/evidence/P09/2026-10-22-p09.4/`。P09.3 **passed**：Docker 只读查询（容器/镜像/网络/数据卷/部署）、容器日志（时间键集分页 + SSE 流，每账号 4 条并发）、现有受支持容器的 start/stop/restart。**Web 进程从不碰 `docker.sock`**——所有查询与动作在代理里用固定子命令完成，真机验了 Web 账号不在 docker 组、`docker ps` 被拒、`/proc/<pid>/fd` 里没有 sock。**容器身份只认完整 64 位小写十六进制**（唯一会进 argv 的可写字符串，名字只在输出里出现；短 ID 可猜，全 ID 不行），`^[0-9a-f]{64}$` 在 Web 与代理各校验一次。**风险是闭集而不是启发式**：privileged、host 网络/pid/ipc、危险 capability、挂 `docker.sock`、挂宿主 `/` → `risk.level=high` + `managed=false` + 三项能力全 false，并在预检（`CONTAINER_NOT_MANAGED`）与执行前复核（`controlBlockedError` → 409 `PREFLIGHT_STALE`）**两处**拒绝。**不该要的东西不请求**：inspect 格式串里没有 `Config.Env`、没有完整标签表（只取六个 `com.docker.compose.*`），真机放了唯一口令，清单/详情/日志/SSE/代理日志里都搜不到，日志正文经 `logging.Redact` 成 `[redacted]`。启停每条都“预检 → 任务 → 读回”并与 `docker inspect` 现场对照：**退出码 0 不算成功，读回状态一致才算**（`observed_state.verified`）。compose 项目是派生身份 `dep_`+sha256 前缀，只做精确匹配 GET，外部 YAML 不执行不解析不落盘。VM 上 **PASS=217 / FAIL=0 / NOT RUN=1**（五遍；唯一 NOT RUN 是“执行前复核（受理之后再变）”，需毫秒级可重复竞态，由 `TestContainerControlExecuteStaleState` 覆盖）；真机抓出并修掉 **3 个**产品缺陷（Q270 `--tail`/`--until` 顺序让往回翻页只剩边界一条 → 时间窗分页；Q271 套接字还在但守护进程死了被当成命令失败/500 → 按 `Cannot connect to the Docker daemon` 判 `UnavailableError`，清单降级 200 + `available=false`、预检 `allowed=false` + `DOCKER_UNAVAILABLE`；Q272 代理的 5xx 域内码被 Web 改写成 `AGENT_UNAVAILABLE`）；宿主 `check.sh` PASS 13/0/0、`go test ./...` 全绿；证据 `docs/evidence/P09/2026-10-22-p09.3/`。P09.2 **passed**：本机端口/服务/容器发现、管理员确认候选、普通健康检查与 SSRF 约束。三路来源（`/proc/net/tcp{,6}` LISTEN → inode → `/proc/<pid>/fd` → cgroup v2 → `.service`、裸监听、`docker ps -a --no-trunc`），候选 `target_id` 只有 `local_…`/`cntr_…` 两种形状；**SSRF 不是过滤而是写不出来**——探测地址一律由代理拼成 `http(s)://127.0.0.1:<port><path>`（`Proxy: nil`、不跟重定向、TLS 校验开、正文 64 KiB 截断），调用方没有任何位置能放主机名/IP/完整 URL；确认候选＝`POST /apps`（不另开接口）；结论只进 `app_health_results`（`0008`），定义仍在 `apps.health_json`；调度 5 秒一拍/一轮 60 秒/每应用 30—300 秒/最小间隔 5 秒，代理不可用写一条 `unknown` 即结束本轮。两条诚实性规矩：**“没看”不能变成“没有”**（探测侧 `unknown` vs `error`，入 Q257；绑定合并侧来源没采成不进 `missing_bindings`，Q260），**没发出去的探测不填 address/url**。为三条容器验收在测试机上装 `docker.io` 29.1.3 + `alpine:3.20` **夹具**（非依赖；无 docker 的机器上那三条仍是 NOT RUN）。VM 上 **PASS=120 / FAIL=0 / NOT RUN=1**（六遍 93/0 → 120/0；唯一 NOT RUN 属 P09.4）；真机抓出并修掉 **3 个**产品缺陷（Q258 pid 1 代持的 `.socket` 监听让端口出现两次、Q259 docker-proxy 在 `docker.service` cgroup 里让容器出现两次且抑制必须在单元分类之前、Q260 停 dockerd 后容器应用被误判“需重新确认”）+ 一条语义修正 Q257；宿主 `check.sh` PASS 13/0/0、`go test ./...` 全绿；证据 `docs/evidence/P09/2026-10-22-p09.2/`。**下一子任务 P09.3**（Docker 只读查询、日志、现有受支持容器的启停）。P09.1 **passed**：应用/分组/收藏元数据、link/systemd/docker/compose 判别模型、公共入口与内部目标分离。四张表（`apps`/`app_groups`/`app_favorites`/`app_volume_dependencies`，`0007_apps.sql`）；`type` 决定 `binding` 的唯一合法键，规范化 JSON + `uq_apps_binding`（`WHERE type <> 'link'`）+ `CHECK` 让唯一性由数据库保证；`public_url` 只受理浏览器能打开的地址（生产禁回环/通配），健康检查 `target_id` 形状写不出 IP/主机名/URL，`169.254.169.254` 真机试过；摘要字段只填“不知道”（link `not_applicable`，其余 `unknown` + `last_checked_at=null` + `dependency_protection=unmanaged`），`capabilities` 只有 `open`；依赖卷不建外键（Q248），写入经代理确认，503 不写库、422 带 `volume_id`；删除只删元数据，收藏按用户隔离且不写审计。VM 上 **PASS=210 / FAIL=0 / NOT RUN=0**（第八遍；第七遍复跑同样 210/0/0）。真机**无产品缺陷**，前几遍 23 个 FAIL 全为脚本自身问题（断言写错 4 处、破坏性动作漏再认证、夹具没清干净 1 处），并把“预检绑定快照”这条产品语义钉进脚本；宿主 `check.sh` PASS 13/0/0、`go test ./...` 全绿；证据 `docs/evidence/P09/2026-10-22-p09.1/`。|
| P10 | 反向代理、HTTPS、0.1 发布 | **passed**（P10.1—P10.4 全部完成）| P10.3 **passed**：证书四件事——staging 必填（`staging *bool` 缺失即 400，默认值就等于"不小心调用正式 ACME"）、`--server` 只由 staging 推导（调用方给不了地址）、签发前先做 HTTP-01 可达性检查；手动导入拆两条通道（公钥走一次性上传，1 MiB、10 分钟、绑会话；私钥走 `POST /secrets`），私钥不进 argv/日志/库，落地前 `Take` + `CoversDomain`；账本按域名唯一、`crt_` 在预检展开期定死、**唯一写入点 `OnJobFinished`**；到期信息 `valid`/`expiring`（21 天窗）/`expired` 现场读盘；续期读 jobs 表做同域名 1 小时冷却（429 `RESOURCE_BUSY`）。**续期与生效分家**：续期只换盘上文件，生效靠下一次 `proxy.apply` 的轮换**实测**（SNI 握手取叶证书指纹，与盘上不一致就 reload，`reload_reason=cert_rotated`，12 × 250 ms settle 预算，仍不一致报 `PROXY_TLS_SERVED_STALE` 告警）。VM 上 **PASS=176 / FAIL=0 / NOT RUN=7**（四遍收敛 114/37 → 165/2 → 172/4 → 176/0），真机抓出 **1 个**产品缺口（续期之后新证书在任何路径上都不生效）。证据 `docs/evidence/P10/2026-10-23-p10.3/`。P10.2 **passed**：配置生成（纯函数渲染、确定性 apr1 盐）、原子写入（临时文件 + fsync + rename + fsync 目录）、`nginx -t` 整棵树门禁、失败回滚与回滚复验、`nginx -s reload`（只 reload 不 restart，单元没在跑就 skip）、`applied` 四态推导（不落库）、改名与停用的文件清理（先备份）、账号文件权限按 worker 用户定、删除记录的孤儿配置门禁 409、`proxy.apply` 参数服务端展开。VM 上 153 项断言全过，真机抓出 1 个缺陷（Q290 预检偶发 8 秒 → 单元探测排在采集扫掠后面，6.389s vs 0.17s）。证据 `docs/evidence/P10/2026-10-23-p10.2/`。P10.1 **passed**：Nginx 发现、域名冲突判断、站点定义登记（单域名单 upstream、WebSocket 开关、请求限制、访问策略）。事实与判断分家：代理登记 `nginx.state`（`{fresh}`）只给事实，`internal/proxies` 出判断；**展示用 5 秒快照、写入门禁用直读**（缓存窗口里刚手工加的 `server_name` 正是唯一需要判准的时刻）；冲突三态 `clear`/`taken`/`unknown`，**`unknown` 与 `taken` 一样 409 阻断**（`nginx reload` 不因 `server_name` 撞车失败，只会安静地用第一个块），证据带文件 + `server_name` 所在行 + 原文 + `broken_files`；读不到主机事实是 503 而不是“没有冲突”。符号链接按真实路径去重（`sites-enabled/default` 只算一次）；域名 IDNA 归一并禁通配符/协议/端口/路径；**上游端口不得等于面板真正监听的端口**（取自监听地址，不写死 9443）；默认值与 nginx 一致（`1m`/`60s`）；访问模式与附加字段双向一致（多给也 400）；`secret_ref` 只进不出。**P10.1 不写配置**：没有 `applied_*` 字段、审计固定 `"applied": false`、`/etc/nginx` 全部文件内容摘要前后相等（Q280）。VM 上 **PASS=100 / FAIL=0 / NOT RUN=0**，真机抓出 **1 个**产品缺陷（Q279：`nginx -v` 时限写成无单位 `5` → 5 纳秒，Windows 上被 `t.Skip` 的单测证明不了）；证据 `docs/evidence/P10/2026-10-22-p10.1/` |
| P11 | 软件更新与电源 | **已完成**（P11.1、P11.2、P11.3 全部 passed）| P11.3 **passed**：`system.power`（`reboot`/`poweroff`）走既有预检→再认证→提交→执行→审计链路——结果只到 `accepted`、`verified_at` 为空，唯一站得住的证据是 `boot_id` 变了（`confirmed`/`unchanged`/`unreachable`/`unknown` 四个闭集，没有第五个）；**关机只承认“回来过”，电源是否真切断“只能由现场回答”**，断言同时钉死“不许出现关机成功 / 不许 `outcome=verified`”；关键任务闸门（有写操作在跑 → 预检 `allowed=false` 并点名 blocker、提交 409 `POWER_JOBS_RUNNING`、`boot_id` 不动）与倒计时取消（终态 `canceled`、无 `power_check`、`boot_id` 不变）互为正反面；`power-global` 锁串行化。两台 VM 各 **PASS 109 / FAIL 0 / NOT RUN 2**（多出的一条是新增的 `env-no-foreign-units`；两条 NOT RUN 是“脚本与被测机器同一台”的位置问题，各自在下一阶段被同一事实的 PASS 覆盖），逐节断言数两台完全相同；真机 `boot_id` `591a2533→5389231a→4a25a825`（22.04）与 `97057b04→22486dd5→813e082b`（24.04），24.04 关机后 QEMU 进程从宿主消失（真断电而非假关机；串口原文 `reboot: Power down` 留在证据里）；宿主 `check.sh` PASS 13 / FAIL 0（web 45 文件 820 用例，修完后在最终代码上重跑）。真机抓出并修掉 **2 个产品缺陷**：电源操作把自己算成在跑的关键任务（每个 `system.power` 停在 `revalidate` 报 `POWER_JOBS_RUNNING`，而前一步预检是 `allowed`）——修法是 `OperationID` 从 `PlanInput` 递到 `InFlight(exclude)`，闸门判据一字未动，两层回归测试钉住；**代理起服时抢走正在服务的 socket**（第 2 轮 22.04 的 6 条 503 根因：外层 systemd 单元每 3 秒重生并清 `/run`，`prepareSocketPath` 又无条件 `os.Remove`，删掉的是还活着的代理的路径）——修法是先探测：连得上拒绝启动、`ECONNREFUSED`/`ENOENT` 才算陈旧、说不清按有人处理，两个回归测试钉住“活的不许抢”和“陈旧的必须能清”；另有 **1 条假绿**：审计查询漏了 `FROM audit_events`，SQLite 报错被吞、空串里找不到口令就 PASS（“没查成 ≠ 清白”），改成查询失败也判失败并重跑阶段三；十条脚本与流程缺陷、九条限制见证据（其中第二条是 24.04 的客户机热重启本轮没验到）。证据 `docs/evidence/P11/2026-10-23-p11.3/`。P11.2 **passed**：四个写动作（`package.refresh`/`install`/`remove`/`upgrade`）——模拟即准入（`apt-get -s` 的 `Inst`/`Remv`/`Conf` 与保留原因当依据）、执行后重读 dpkg 与计划逐条核对（`observed_state.verified`；退出码 0 不是证据）、四把 apt/dpkg 锁只探测从不删、非交互六件套（`noninteractive`/`--force-confdef`/`--force-confold`/`Use-Pty=0`/`LISTCHANGES=none`/`NEEDRESTART_MODE=l`）、四个动作全部不可取消、`remove` 额外要 `REMOVE` 短语、`upgrade` 只走 `--only-upgrade` 永不 `dist-upgrade`、删除保护名单硬编码在代码里。两台 VM 各 **PASS 169 / FAIL 0 / NOT RUN 4**、两次 `exit=0`；宿主 `check.sh` PASS 13 / FAIL 0（web 43 文件 791 用例）；`//go:build unix` 三个包首次在 Linux 真跑（`syscmd` 24+1 SKIP / `processes` 41 / `apt` 77）。真机抓出 **6 个缺陷**：退出码 -1 不说谁停的（`syscmd.Result.Signal`）、socket 尸体被当就绪（脚本抢跑）、锁探测单测与实现矛盾（分平台断言）、38.7s 再模拟被前端 20s 掐断、`wait_job` 按次数算的“300 秒”实际 400+、升级夹具靠“恰好有个可升级的包”（改自建 `file:` 源 + conffile 真冲突）。证据 `docs/evidence/P11/2026-10-23-p11.2/`。P11.1 **passed**：软件包与更新只读页——代理出事实（dpkg 登记 + `apt-cache policy` 候选 + 重启标志/`boot_id`）、Web 算判断（`candidate_relation`/`upgradable`/`security`/`pockets`/`counts`/分页/筛选），`internal/debversion` 做版本比较。真机抓出并修掉 **1 个产品缺陷**：arch 限定名（`bind9-libs:amd64`）在 `apt-cache policy` 里块头被 apt 剥成 `bind9-libs:`，按请求名索引必然落空 → 整类包的候选被谎报成“读不到”；修法是基础名回退 + 同名双架构时**不回退**（如实报 `unknown`），对账脚本自己也复刻了同一个 bug，改法不同之后才敢当对账。24.04 的 apt 比 22.04 慢 28 倍（26.6s vs 384ms / 731 与 676 个包名），超时分成 45s/60s/120s/30s 四层，**启动预热删掉**（26s→62s 反而误报不可用）。两台 VM 各 **PASS 64 / FAIL 0 / NOT RUN 7**，对账 0 失配；宿主 `check.sh` PASS 13 / FAIL 0。证据 `docs/evidence/P11/2026-10-23-p11.1/` |
| P12 | 备份恢复与计划任务 | **已完成**（P12.1、P12.2、P12.3、P12.4、P12.5 全部 passed）| P12.5 **passed**：证书续期 timer 接入（面板按账本展开 `certificate{domain,staging}`、代理按域名查盘上 `live/<domain>`、触发只发 `{domain,staging,scheduled:true}` 于是 `--keep-until-expiring` 天然生效、结果按 observed 回写、面板按域名对账导入任务表）与备份页/计划任务页依赖的接口形状（快照大小、`GET /scripts/:id` 的 `{script:…}` 包装、`needsReauth` 从代理 `ActionPolicy` 派生、prune 待删名单由预检算、前端产物里带这些动作）。两台 VM（24.04/systemd 255/certbot 2.9.0、22.04/systemd 249/certbot 1.21.0）最终各 **PASS 214 / FAIL 0 / NOT RUN 0**、exit 0，逐节断言数（25/20/44/24/30/15/20/10/19/7）与归一化后的断言名单跨机逐字一致，同一份脚本（md5 `9fd078cb…`）同一组二进制；全程真实 Pebble ACME 走完 HTTP-01、换证、失败留原因三段。真机抓出并修掉 4 个产品缺陷（Q390 定时续期预检绑不上目标身份、Q391 失败原因到不了任务与账本、Q392 P12.4 的"尚未接线"闸没拆、Q393 审计行里的空 `certificate_id`/`email`），方法修正 5 条（Q394 Pebble 缓存已通过的授权一小时、Q395 §0 不断言 80/5002 空闲、Q396 §9 用整轮开始时的会话、Q397 prune 数总条数与水位取在触发之后、Q398 采样器 `ps -e -C` 自己压垮 2 vCPU 客户机）；24.04 跑 12 轮、22.04 跑 2 轮才全绿。宿主 `check.sh` 最终 13/0/0。证据 `docs/evidence/P12/2026-10-25-p12.5/`。P12.4 **passed**：受管理 timer（单元写死时区/`Persistent`/最长运行/目标服务）、计划与脚本只存在代理库且 Web 直读镜像（代理不在 503，不回空列表）、`schedule-trigger --wait` 让"单元 active"等于"这次操作还在跑"（不重叠）、触发只认 root 且槽位折到 UTC 分钟幂等、定时操作被对账导入任务表并挂一个登录不了的系统操作者、非 root 脚本（`root:upanel-shell 0750`、`/bin/sh`、运行前核 SHA-256、被引用时删不掉）。两台 VM（24.04/systemd 255、22.04/systemd 249）最终各 **PASS 246 / FAIL 0 / NOT RUN 0**、exit 0，逐节断言数与名单跨机逐字一致；真机抓出并修掉 9 个产品缺陷（Q371/Q374/Q375/Q376/Q377/Q380/Q381/Q382/Q385），方法修正 9 条（Q370/Q372/Q373/Q378/Q379/Q383/Q384/Q386/Q387）；宿主 `check.sh` 最终 13/0/0。证据 `docs/evidence/P12/2026-10-24-p12.4/`。P12.3 **passed**：`backup.restore`（新目录/覆盖/失败不发布/停拉应用）、`backup.verify`（结构检查与真实抽样两态自我说明）、`backup.prune`（冻结 `expected_delete_ids`、`PREFLIGHT_STALE`、只删本策略、清残留锁）。两台 VM（24.04/restic 0.16.4、22.04/restic 0.12.1）最终各 **PASS 117 / FAIL 0 / NOT RUN 0**、exit 0，断言名单跨机逐字一致；写满（inode 耗尽）与 SFTP 断连都核对源/旧目标哈希不变、无暂存残留、应用仍 active。真机抓出并修掉 Q356/Q357/Q359/Q360+Q363 四个产品缺陷（失败消息那条是两次修到位的，记作一个），方法修正 Q355/Q358/Q361/Q362/Q364；宿主 `check.sh` 最终 13/0/0（node 按 `.nvmrc` 用 v22.23.3）。证据 `docs/evidence/P12/2026-10-24-p12.3/`。P12.2 **passed**：代理镜像唯一真相、`backup.run` 预检与执行、面板展开 `apps` 并由代理双向核对、停受管理应用、`VACUUM INTO` 面板库在线副本、完整快照 id + `restic stats` 回读、快照列表/详情。两台 VM 最终各 **PASS 170 / FAIL 0 / NOT RUN 3**、exit 0，逐条结果一致；SFTP 成功、断网、独立 tmpfs 写满、运行中取消都有真机证据，失败场景同时核对源不变和旧快照可读。真机抓出并修掉 Q343/Q344/Q345/Q347 四个产品缺陷；宿主 `check.sh` 最终 13/0/0。证据 `docs/evidence/P12/2026-10-24-p12.2/`。P12.1 **passed**：本地/SFTP 仓库、秘密引用、主机密钥核对、初始化、容量与现状三态；两台各 133/0/5，证据 `docs/evidence/P12/2026-10-23-p12.1/`。**BACKUP-01 的"备份后恢复并校验"现在有产品侧真机证据**（P12.3）；timer 触发与证书续期都有真机证据（P12.4/P12.5）；剩下的 BACKUP 相关缺口只有**浏览器里对备份页与计划任务页的真实点击**——P12.5 验的是产物与接口形状，不是交互（见第 8 节） |
| P13 | 历史监控和告警 | **passed**（P13.1—P13.4 全部 passed）| P13.4 **passed**：确认动作只写"谁知道了"（`acked_at`/`acked_by` 不碰状态机，确认后 `state` 仍是 `firing`、`resolved_at` 不被写；幂等且首告者胜——`WHERE id=? AND acked_at IS NULL` 让第二个管理员再点也保留第一个人的时间与人；已恢复的事件也能确认；viewer 能看事件但不能认领）；投递账读路径是顶层 `GET /notification-deliveries`（一行账带的是**投递当时**的通道快照，收件箱要的是跨通道排序，挂通道下就成了 N+1），行形状与通道端点回执共用同一个 DTO，`limit` 超过 200 按 200 截断而不报错；历史曲线三条展示口径（缺失不是 0——缺口里没有行、`missing_points` 是唯一证据、minute/hour 聚合桶落进缺口区间是聚合的正常产物不是回填；没点不画轴；null 显示"—"）；"陈旧"与"没开"分开说：`enabled` 说的是这个进程答不答得了历史问题，`collecting` 才是在不在记，关掉采集是 `enabled=true + collecting=false + reason`，`enabled=false` 只剩"没有库"一种来路。两台 VM（24.04/systemd 255/TCG 单线程、22.04/systemd 249/whpx）各 **PASS 128 / FAIL 0 / NOT RUN 0**、exit 0，逐节断言数（7/9/14/14/20/31/12/8/10/3）与归一化后的断言名单跨机逐字一致（`diff` 无输出），同一份脚本（sha256 `f86fb06f…`）同一组二进制；产品缺陷 0 个，验收方法修正 3 组（Q429 新鲜度判据的两个时刻必须都取自同一份响应、Q430 `enabled` 不是 `collecting`、Q431 另五处装配与判据缺陷）；宿主 `check.sh` **14/0/0**。24.04 两轮（127/1 → 128/0），22.04 两轮（换脚本前后各一轮 128/0）。见第 5 节 P13.4 小节与 `docs/evidence/P13/2026-10-26-p13.4/`。P13.3 **passed**：通知由**代理**投递（面板拿不到明文）、`secret_ref` 用途由代理自己核对、投递失败仍回 200 并把 `retryable` 交给代理判、入队与事件同事务、`(event_id, channel_id, kind)` 去重、30 秒退避与 10 分钟封顶、`max_attempts` 建在通道上、停用/删除判死且不计 attempts；两台 VM 各 211/0/0，见第 5 节 P13.3 小节与 `docs/evidence/P13/2026-10-26-p13.3/`。P13.2 **passed**：三态状态机（`ok`→`pending`→`firing`→`ok`，落在规则行上、CAS 加版本，数据不足时原地不动、没有第四态）、"持续"= 窗口内**每一个**样本都在线外、恢复要跨过 `recovery_threshold` 死区并同样持续 `duration_seconds`、`any`/`all` 双口径对偶（同一份真实容量数据上 `any` 触发、`all` 不触发）、一个事件周期只有一行（触发 INSERT / 恢复原地 UPDATE，`UNIQUE INDEX ... WHERE state='firing'` 把"未变化不刷屏、恢复只发一次"变成结构保证）、事件存规则快照且删除规则时 `rule_id` 置空不级联、维护静默只标记 `silenced` 不压判定、"没有数据不是 0"。两台 VM（24.04/systemd 255、22.04/systemd 249）各 **PASS 148 / FAIL 0 / NOT RUN 0**、exit 0，逐节断言数（7/9/48/63/9/8/4）与归一化后的断言名单跨机逐字一致，同一份脚本（md5 `c74dd469…`）同一组二进制。真机抓出并修掉 3 个产品缺陷（Q414 覆盖率容差与 `duration_seconds` 地板要按每条序列自己的写入节奏算、Q417 `last_reason` 每轮都要写、Q418 可空字段回 `null` 不回空串）、验收方法修正 6 条（Q415 整机 fd 是验收前置条件、Q416 查询辅助不许吞 SQL 错误、Q419 三处判据修正、Q420 算术里不许塞命令替换）；宿主 `check.sh` **14/0/0**。24.04 跑了六轮（117/25 → 139/3 → 142/0 → 145/2 → 崩溃 → 148/0），22.04 两轮（142/0 → 148/0）。证据 `docs/evidence/P13/2026-10-26-p13.2/`。P13.1 **passed**：三层粒度（raw 5 秒留 24h / minute 留 30d / hour 留 365d）、长格式表 + `min_value`/`max_value`/`sample_count` + `WITHOUT ROWID` 复合主键、`INSERT OR IGNORE` 去重、只卷已结束的桶（按样本数加权、先聚合后裁剪、留两个桶宽、启动自愈补算）、读哪一层由"时间跨度 × 实际保留窗口"决定而不是步长、缺失就是没有行（`missing_points` 计数、永不补零）、`GET /system/metrics` 与 `/system/metrics/status` 两条 V 权限端点。两台 VM（24.04/systemd 255、22.04/systemd 249）各 **PASS 88 / FAIL 0 / NOT RUN 0**、exit 0，逐节断言数（6/5/22/9/10/23/8/5）与归一化后的断言名单跨机逐字一致，同一份脚本（md5 `d08964c5…`）同一组二进制。真机抓出并修掉 1 个产品缺陷（Q404 加权平均的浮点误差撞 `CHECK(min<=value<=max)`，整个小时桶永久写不进去，那一小时所有指标的 hour 行缺失）、方法修正 4 条（Q405 缺口判据窗口右端也要让一个桶宽、Q406 换行符与执行位属于构建输入、Q407 三处恒真/错期望/取样点早的断言）；宿主 `check.sh` **14/0/0**。证据 `docs/evidence/P13/2026-10-26-p13.1/`。 |
| P14 | 网络、NFS、账号、TOTP | **passed**（P14.1—P14.6 全部 passed。P14.6 **真机通过**：2026-10-27 Ubuntu 22.04.5，第二轮 `PASS 163 / FAIL 0 / NOT RUN 6`、退出码 0，证据 `docs/evidence/P14/2026-10-27-p14.6/`；第一轮 `149 / FAIL 11` 抓到 **1 个产品缺陷**（Q480：`EnableTotp` 把 `last_step` 写死 -1，确认用的那个码在同一个 30 秒窗口里还能登录一次）与 **4 类脚本断言错误**（Q481：偏移加到 ±1 窗口外面、空串当 needle、`jget` 的失败标记拼进 SQL）。STAGES §16 两条硬要求都有现场读数——**未确认不启用**（库 0 行 + 代理种子 `temporary` + 带外 CLI 明确失败）与**恢复码只能一次**（停代理造出中间态：`used_at` 已写而状态行还在，起回代理后同一枚 401）。分层实现：`internal/totp` 通过 RFC 6238 向量、代理侧四条 `/internal/v1/totp/*` 24 条断言、Web 侧（迁移 0014、状态层、`/auth/totp/*` 三端点、登录与再认证两步、失败关闭）与 `upanelctl totp-reset` 合计 **22** 个新用例全绿、Web UI（设置页 + 两步登录 + 四个再认证点）**21** 个新用例全绿且 `typecheck`/`lint`/全量 1137 例通过；**真机第二轮 `PASS 163 / FAIL 0 / NOT RUN 6`、退出码 0**）| P14.1 **passed**：四条只读端点（`/network/interfaces`、`/network/routes`、`/network/listeners`、`/firewall`，全 admin）、`capability`/`editable` 判定与"查不到也要说清楚"的 `Issue` 闭集。**没有开任何写入口**（POST 四条同路径与 `/network/apply` 在真机上必须 404/405）。两台专用 Ubuntu VM 各 PASS 112（22.04）/ 116（24.04）、FAIL 0、NOT RUN 0，退出码 0；同一份脚本（sha256 `147b7a93…`）同一组二进制；`/etc/netplan`、`/etc/ufw`、`/etc/systemd/network` 三处前后哈希不变证只读。**两台走到判定的两个分支**：22.04 `editable=true, target=ens4`，24.04 因遗留 `docker0` 网桥 `editable=false` 且理由点名网桥。真机抓出并修掉 2 个产品缺陷（Q438 主机路由 `destination` 无前缀、Q439 `/etc/nftables.conf` 每台 Ubuntu 都有 → 误判 nftables 同存），验收方法修正 6 条（Q440—Q445）。证据 `docs/evidence/P14/2026-10-26-p14.1/`。**P14.2 `network.apply` passed**（22.04 VM 第五轮 `PASS 186 / FAIL 0 / NOT RUN 3`、退出码 0，证据 `docs/evidence/P14/2026-10-26-p14.2/`：地址被改成不可达的 `10.99.99.99/24` → 停 Web 与代理 → `deadline+28s` 由 `upanel-netrollback@<token>.service` 自己退回来，两条看门狗均 `not-needed`；旧地址确认被 `CONFIRM_WRONG_ADDRESS` 拒；复杂 netplan 返回 `NETWORK_UNSUPPORTED`）——执行顺序"渲染 → `netplan generate` → 先武装 timer → `netplan apply` → 读回内核事实"，outcome 闭集 `verified`/`rolled_back`/`unknown`（**无 `failed`**），`network.confirm` 的 `arrived_on` 由服务端从连接取（v4-unmap 归一化），回滚记录落成 root 0600 的文件。**STAGES §16 的硬条件因此解除**；但 `-network-writes` 仍默认关闭，理由换成"改网卡等于把机器从网络上摘掉，这个权力该在装机时由人给"（Q458，落到 P16.1 服务单元与 P16.4 手册）。**P14.3 `firewall.apply` passed**（22.04 VM 第三轮 `PASS 153 / FAIL 0 / NOT RUN 4`、退出码 0，证据 `docs/evidence/P14/2026-10-27-p14.3/`：启用 + 规则集里没有 `from any` 的 22/tcp 放行 → 预检 `FIREWALL_MANAGEMENT_PATH` 拦下且现场一字节没动；apply 后**停掉 Web 与代理** → 60 秒时新规则仍在 → `deadline` 后 `upanel-fwrollback@<token>.service` 使规则自己换回旧集且 `ufw` 仍 active，journal 里动手的是 `upanel-agent[pid]`；手写 `ufw allow 9000/tcp` 跨两次 apply 原样保留）——规则身份靠 comment `upanel:<id>`，apply 只加缺的删多的、**不整体重建**（整体 reset 会连手写规则一起清掉），指纹只算"启用状态 + 带标记的规则"；管理通路判据是"启用状态下每个对外可达管理端口都要有 `from any` 的 tcp 放行"（`ufw allow from 10.0.0.5 to any port 22` 不算），认不全管理端口时 `FIREWALL_MANAGEMENT_UNKNOWN` 并停下；`-firewall-writes` 与 `-network-writes` 是两个开关、默认都关。真机抓出并修掉 **1 个产品缺陷**（Q464 三个 `Assess` 的拒绝分支没带 `Normalized` → "没有待确认的改动"被包装成 500）、**1 处契约违反**（`rules` 在 ufw 读不到时序列化成 `null`，违反 OpenAPI 的 `array`）、**5 条验收方法修正**（Q465）。**P14.4 `share.apply` / `share.disable` / `share.delete` 的 NFS 那一半 passed**（22.04 VM 第七轮 `PASS 141 / FAIL 0 / NOT RUN 6`、退出码 0，证据 `docs/evidence/P14/2026-10-27-p14.4/`：只维护 `/etc/exports.d/upanel.exports` 一份文件、行尾 `# upanel:<id>` 认领、`/etc/exports` 前后 sha256 不变；`exportfs -v` 的实际导出表里路径在、`root_squash` 在、没有 `no_root_squash`/`async`/`insecure`/`*`；把允许网段换成 `192.168.77.0/24` 后**同一台机器**挂不上、改回 `127.0.0.0/8` 立刻又能挂上；只读共享导出行 `ro` + 目录 `0755`，客户端写入被内核以 `Read-only file system` 拒绝、服务端没多出文件；卷摘出挂载树后 **13 秒**内撤销循环摘掉导出行并 `exportfs -ra`，`runtime_revoked.volume_state=unmounted` + `config_applied=false`，卷挂回来**不自动复活**、重新应用才恢复，客户端随后又能挂上并读到之前写进去的文件；停用/删除只摘导出行，目录与文件一个不少、内容哈希不变，收尾只剩一条已停用的共享记录）——判据一律读 `exportfs -v` 的实际导出表（`exportfs -ra` 的退出码不是证据），只读的两道锁是"导出行 `ro` + 目录 `0755`"（NFSv3 客户端的 `findmnt` OPTIONS 永远报 `rw`，只读是服务端回 EROFS），`nfs:exports` 与 `samba:config` 是两把锁。真机抓出并修掉 **2 个产品缺陷**（Q471：0005 的 `CHECK (protocol IN ('smb'))` 让导出写下去了却在落库时被挡，`fakeStore` 不跑 SQL 所以单测掩盖了它 → 0013 表重建迁移；Q473：同一个目录的第二条共享在预检被放行 → `SameDirShares` + `sharesSharingDir`，预检与执行同一条判据）、**1 条同类未修**（SMB 路径在配置生效后落库失败不回滚），**7 类验收方法缺陷**（Q474，最险的一条：`"$(PRE_ALLOWED)"` 少一个 `$` 把变量当命令执行，一条语法错带红第 7—10 节；另有 `umount -l` 让 knfsd 持有文件系统导致"卸卷"永远 busy）。**P14.5 账号 / SSH 公钥 / 受限 sudo 模板 passed**（2026-10-27，Ubuntu 22.04.5 / kernel 5.15.0-198，第三轮 `PASS 168 / FAIL 0 / NOT RUN 6`、退出码 0，证据 `docs/evidence/P14/2026-10-27-p14.5/`）：口令只经 `chpasswd` 的 stdin（任务参数里只有 `sec_` 引用，`passwd -S` 读回 `P`）；uid 由系统分配（`getent` 读回 1002，落在 1000–59999）、home 在 `/home` 下、登录壳 `/bin/rbash`；`authorized_keys` 托管区里人手写的外来钥匙在加/换/删三次之后**逐字节不变**、文件 `0600` / 目录 `0700`、`ssh-keygen -lf` 的指纹与登记表一致；sudo 五道门全走一遍——`0440` + 认领标记 + 无通配符、整份 `visudo -c` 通过、`sudo -l -U` 恰好一条、`runuser` 换到 alice 之后 `sudo -n` **真的执行**了模板命令而模板外命令被 sudo 自己拒；**真机制造 visudo 失败**（候选合格、rename 已发生、整份校验失败）时旧规则文件哈希与内容逐字节保住、没有残留候选临时文件、失败原因带 `SUDO_VISUDO_FAILED`；手改成没有认领标记的规则文件被 `SUDO_FILE_DAMAGED` 拒且逐字节保留；禁用是换壳（`passwd -S` 仍是 `P`，没用 `-L`）；`userdel` 不带 `-r`；sshd 那一面验的是「钥匙能用、口令不是门槛、壳才是门槛」（key-only 账号能登 → 面板禁用后同一把钥匙被拒，sshd 原文 `This account is currently not available.` → 启用又能登 → 撤掉公钥后登不上）。真机抓出并修掉 **2 个产品缺陷**（Q477：`accounts.Definition` 一个 json 标签都没写，`display_name` 直接 400——所有单测只构造 Go 结构体、从不走线格式，所以全绿也看不见；Q478：`GET /accounts` 把 Samba 的能力当账号判据，没装 Samba 的机器上账号功能整个不可用）与 **1 条与真机相反的文档断言**（Q479："没口令就登不上"——publickey 认证不看 shadow 的口令字段） |
| P15 | 应用部署与更新 | **passed**（P15.1—P15.4 全部实现并真机验收通过；P15.4 的页面链路 2026-10-28 在 22.04 上 43/43）| P15.4 **已实现、宿主检查通过、真机未验**：部署页只读管理面（`/etc/upanel/deploy` 的三个文件 + systemd 的两个回答），**没有新增写接口**——创建/更新/删除走既有 `/operations/preflight` + `/jobs`（`deployment.apply`/`update`/`remove`），页面上没有“直接部署”；`compose_yaml`/`privileged`/`devices`/`network_mode`/`cap_add` 在数据模型里没有落点，所以连输入框都不存在（留框再拒等于暗示那件事可行）。交付 `web/src/api/deployments.ts`（形状常量、两条查询、只读纯函数、三份预检参数、草稿校验）、`jobresult.ts` 的 `summarizeDeploymentJob`（`rolled_back` 不许说“已更新”、`containers_checked=false` 必须写“没有独立读数”）、`DeploymentsView.vue`（列表/详情/新建/换镜像/删除，轮询 2s×300=10 分钟对上 `UpdateBudget=9min`，预检 120 秒过期由 ticker 关确认）、路由 `/apps/deployments` 与应用页入口（**不去顶“容器”那一格**，`routes.test.ts` 断言 `containers.implementedIn` 仍为 `null`，Q495）、CONTRACTS §8.9 与 Q495/Q496。宿主读数：前端 **61 文件 / 1161 用例**、`vue-tsc` 干净、`vite build` 成功（`DeploymentsView` chunk 36.23 kB）、`routes.test.ts` 11 条。**未验证（NOT RUN）**：本轮**没有任何真机读数**，页面链路没被人点过、也没有 Playwright 用例（`web-smoke` 只走“未登录落到登录页”）；`truncated`/`skipped`/`config_drift` 三种现场只在单测里造过；从这一页发起的三个动作一次都没跑过（动作本身在 P15.2/P15.3 验过，走的是脚本）；24.04 差异未验。自查抓到 3 个实现缺陷（`64 << 30` 被截成 0、空行过滤与校验冲突、`getManagedDeployment` 改成 `async`），都不是真机发现的。P15.3 **已实现、宿主检查通过、真机验收通过**：`internal/deploy` 新增 `record.go`/`update.go`/`probe.go`——`<name>.deployment.json`（`0640`，规范化定义 + 镜像记录，无明文秘密）、`<name>.compose.yaml.prev`（`0600`，上一版原样字节）、`deployment.update` 的预检与执行（停单元 → `files_only` 备份 → 拉镜像 → digest 核对 → 写 `.prev` → 写新配置并 `restart` → 健康探测 → 失败回切原样字节并再探）、面板侧健康探测（探宿主公开入口、`http` 判 `200—399`、`tcp` 判能建连、不跟随重定向、禁 keep-alive、120 秒预算且截断不算通、启动宽限期内失败不算失败）、时间预算切段（`UpdateBudget=9min` / `UpdateBackupBudget=360s` / `UpdateRollbackReserve=120s` / `UpdateMinStep=5s`，每段动手前判断够不够）。代理侧 `internal/agent/deployaction.go` 登记 `deployment.update`（五种 impact、锁含 `backup-policy:`/`backup-repo:`、进度步骤 `stopping`/`backup`/`pulling`/`writing`/`probing`），`internal/agent/deploybackup.go` 是更新前备份的适配层（只收 `files_only`，预检与执行两处都拒 `stop_managed_apps`）。`deployment.apply` 同时接上健康探测（`ApplyOutcome.Health`，探测失败把 `verified` 降成 `partial`），`AssessApply` 的 `health_probe_pending` 告警删除。`go test ./internal/agent/ ./internal/deploy/ ./cmd/...` 全绿（`update_test.go` 13 个用例），`sh scripts/check.sh` **PASS 14 / FAIL 0 / NOT RUN 0**。接线阶段先写测试让缺陷现形，抓到 **3 个产品缺陷**（`ErrDeploymentExists` 挡住更新、配置只在 `!exists` 时写、`restartQuietly` 静默失败），细节见 DECISIONS Q493。**已验证（真机，2026-10-27，VM-A Ubuntu 22.04.5 / docker 29.1.3 / compose 2.40.3 / restic 0.12.1）**：`PHASE=main` **229 PASS / 0 FAIL / 1 NOT RUN**、退出码 0（`docs/evidence/P15/2026-10-27-p15.3/`）——换镜像后记录里的 digest 与 `docker image inspect -f '{{.Id}}'` 逐字相同、健康探测失败 104 秒后回切成 `rolled_back` 且 `error` 为空（`.prev` 与记录都没被动）、按选择备份先停单元再备份且快照能被独立 `restic snapshots` 读到、六种拒绝之后盘上配置与容器一个字节没动、钉一个拉不到的 digest 失败后服务仍在应答；唯一 NOT RUN 是夹具盘 `/dev/sdd` 已有 GPT、按上一轮复用（`sgdisk`/`mkfs` 没重跑，卷的导入/挂载/UUID 全部真读过）。真机这一轮**没有暴露新的产品缺陷**，改掉的 13 条是验收脚本把断言对齐到产品代码时发现的问题（见 §5 的「P15.3 真机验收」小节与证据 README）。P15.2 **已实现、宿主检查通过、接线完成、真机验收通过**：`internal/deploy` 新增 `unit.go`/`applier.go`/`service.go`——包装单元 `upanel-deploy-<name>.service`（`Type=oneshot`+`RemainAfterExit=yes`、`ExecStart` 固定 `docker compose … up -d --remove-orphans --no-build`、`ExecStop` = `down`、`Requires=docker.service`、`[Install] WantedBy=multi-user.target`，逐卷 `RequiresMountsFor` 与 `BindsTo`/`After`，mount unit 名按 `systemd-escape --path` 逐字符实现）、`Applier`（配置 0600/单元 0644、临时文件+fsync+rename、**版本=配置字节 sha256**、同内容幂等重放、异内容 `ErrDeploymentExists`、单元漂移重写、成功与否看 `is-enabled`/`is-active` 读回、删除先 `disable --now` 且仍 active 则 `ErrUnitStillActive` 不删文件、数据卷与业务目录不动）、`Service`（`deployment.apply` 只创建 + `deployment.remove`，预检与执行两阶段，执行前重跑全部判据）。`HostEnv.Volumes` 由集合换成 `VolumeFacts{MountPoint, FilesystemUUID, Mounted, MountedUUID}`，“文件根必须落在它登记的卷挂载点之内”与“启动前 UUID 核对”（`CheckVolumesReady`）因此成立。`go test ./internal/deploy/` **51 个顶层用例全绿**，`sh scripts/check.sh` **PASS 14 / FAIL 0 / NOT RUN 0**。**已验证（真机，2026-10-27，VM-A Ubuntu 22.04.5 / docker 29.1.3 / compose 2.40.3）**：`PHASE=main` **199 PASS / 0 FAIL / 2 NOT RUN**、`PHASE=boot` **51 PASS / 0 FAIL / 0 NOT RUN**（`docs/evidence/P15/2026-10-27-p15.2/`）——真实 `docker compose` 部署起来、宿主端口 HTTP 200、重放不重写不重启、masked mount unit 让 `systemctl start` 失败且没有替我们建目录、重启后单元 `enabled` 但 `inactive` 且系统盘没多出数据、解除 mask 挂回卷后自己起来、删除后单元与配置消失而数据逐字节还在。**未验证**：`/deployments` 写接口与页面（P15.4）、24.04 差异、bind mount 前的句柄级路径核对（`deployment.update`/镜像 digest/按选择备份/健康探测与回切已在 P15.3 真机验收通过）。P15.1 **已实现、宿主检查通过、未接线**：`internal/deploy` 交付受约束 `DeploymentDefinition`（十字段闭集，`privileged`/`devices`/`network_mode`/`cap_add`/`compose_yaml` 等写不出来，请求里出现即 400 `unknown_field`）、八个稳定拒绝码、三段校验（形状 → 宿主事实 → 秘密）、确定性 Compose 渲染（`restart` 恒 `"no"`、长语法挂载 + `bind.create_host_path: false`、`$`→`$$` 防 compose 插值、健康检查不进容器、不写顶层 `name`）、配置摘要与秘密只经 `app_env` 解出；挂载必须落在部署数据根内、所在卷必须写进 `volume_dependencies`（否则依赖卷门禁查不到它）。`go test ./internal/deploy/` 20 个顶层用例 / 100 条 PASS 全绿（含 §17 恶意输入逐条）。**未验证**：无真机读数、无调用方（两个入口尚未 import）、渲染结果未被真实 `docker compose config` 读回；STAGES §17 的三条验收（真实部署/升级、卷缺失重启不绕过、健康失败显示失败）一条都还没有真机证据。P15.2 起接线。证据：CONTRACTS §8.6、DECISIONS Q482、`go test` 输出（无独立证据目录——本子任务没有可跑的现场） |
| P16 | 集成、恢复与 1.0 发布 | **passed**（P16.1 已实现、宿主检查通过、**真机已验**；P16.2 双平台端到端（2026-10-27 首轮）**22.04 148/0/3、24.04 147/1/3**，那条红已归因为单线程 TCG、换了 QEMU 参数后未复现（不等于修好）；P16.3 七维度复核完成、补了一条 fsync 缺陷（Q505）；P16.4 文档集与发布包完成，**在冻结的 1.0 发布字节上重跑七相位 155/0/3**，Q505 的写路径另做了真机 ext4 探针；P15.4 的页面修复与 Q514 的错误分类修复之后字节又重打两次，最终字节 `3f3b40c1…` 上七相位仍是 **155/0/3**（`P16/2026-10-28-p16.2-final/`），24.04 在同一份字节上也 **155/0/3**（`P16/2026-10-28-p16.2-2404-final/`；首轮 `failup` 有一条红查清为断言采样竞态，收紧断言后重跑 21/0/0，Q515）。**从浏览器完成主要用户流程**这一条也在同一份字节上补齐了两平台：22.04 写链路 43/43，24.04 冒烟 35/0/0 + 写链路 43/0/0（`P16/2026-10-28-p16.2-2404-browser/`；首轮卡在"装面板之后才补 docker、代理没重启"的启动时门，Q516，重启代理后全绿）。**掉电演练全程 NOT RUN**；**amd64 全模块 `-race` 已跑完（这是从 P03 记到 P16 的那句"未执行"的结案）**：全量 3h57m 得 44 ok / 7 FAIL / 3 撞超时 / 1 条 `DATA RACE`，补跑 9h03m 把三个大包跑完（`internal/api` 8h02m、0 竞态、0 失败），**产品代码零条数据竞争**，爆出来的问题全在测试侧（Q517—Q520 五处只动 `_test.go` 的修正，产品字节零变化）。**核心验收矩阵已逐项对照**：STAGES §19 的 31 条**已验 31 / 部分 0 / 未验 0**，见 `docs/ACCEPTANCE_MATRIX.md`（PROXY-02 于 10-30 复核为引错证据，从"部分"改成"已验"；FILE-02 与 APP-02 于 10-29 用 `P16/2026-10-29-matrix/gaps/` 补上真机现场，`PASS 80 / FAIL 0 / NOT RUN 0`；DISK-02 的 LVM/RAID 根盘同日由 `P16/2026-10-29-matrix/disk02/` 两台客机补上——LVM `PASS 91 / FAIL 0 / NOT RUN 3`、RAID1 `PASS 113 / FAIL 0 / NOT RUN 3`，最后一条"部分"就这么关掉了））| P16.1 **已实现、宿主检查通过、真机未验**：一份能力探测实现、三种问法（`upanelctl host-caps` / 代理 `host_caps` / 面板 `GET /system/capabilities`），四条诚实性规则（只在四个系统目录找、三态且 `unknown` 必须带原因、版本只在真读到才填、`null` ≠ 空表），21 条规格每条带 `feature` 与 `hint`；`--line` 六列 TSV 给安装脚本用。第二半：`backup-state.sh`（停服务 → 拷两库含 `-wal`/`-shm` 与 `/etc/upanel` 含 `agent.key` → 起回服务 → `MANIFEST.sha256`，`.incomplete` 标记区分跑一半）、`install.sh` 第 9 步动库前自动备份 + 第 3 步改读 `host-caps --line`、`uninstall.sh` 在 `--purge-*` 前默认先备份、`release.sh` 打包、`check.sh` 新增 `sh-syntax`。宿主 `sh scripts/check.sh` **PASS 15 / FAIL 0 / NOT RUN 0**、前端 62 文件 / 1175 用例；**部署脚本只拿到 `sh -n` 读数，没有一次真实运行；备份/恢复/升级与探测页在 Ubuntu 上一次都没跑过** |

## 4. 已冻结决策

完整清单见 `docs/DECISIONS.md`。要点：

- 单台 Ubuntu；先 amd64、22.04/24.04 LTS。
- Go + Vue 3 + TypeScript + Naive UI + SQLite；不换栈。
- 非 root Web（`upanel-web`）、root 受限代理（`upanel-agent`，仅 Unix socket）、
  独立非 root 终端账号（`upanel-shell`）。
- 所有系统写操作采用预检 + 持久任务 + 幂等 + 锁 + 结果核验 + 审计，不提供万能命令执行器。
- 所有物理盘分层监控；系统盘与不支持的复杂设备禁止破坏性操作。
- 0.1 为 P00—P10，1.0 为 P00—P16。
- 工具链与依赖精确版本已核实并锁定，见 `docs/DEPENDENCIES.md`。

## 5. 子任务记录

### P00.1（2026-10-20）— 阅读规格、范围摘要、决策记录

    子任务：P00.1
    状态：完成
    修改文件：docs/DECISIONS.md（新建）
    完成行为：固定三个执行身份的权限边界；固定 0.1/1.0/后续范围与排除项；
              固定技术栈、仓库布局与运行路径；整理不可突破的安全边界与契约约定；
              登记 7 条未决/环境问题（Q1—Q7），其中 Q3 为 P00.3 的 VM 阻塞。
    API/数据库变化：无
    测试环境：开发宿主 Windows 10.0.26300（Git Bash）
    实际检查：无自动化检查（文档产出）；check.sh 的结构检查覆盖该文件是否存在，退出码 0
    证据路径：docs/evidence/P00/2026-10-20/check-run.txt
    未运行检查：无（本子任务不涉及代码执行）
    已知问题/阻塞：无新增阻塞
    下一步：P00.2

### P00.2（2026-10-20）— 依赖核实、版本约束、锁文件、契约骨架、CI 与检查脚本

    子任务：P00.2
    状态：完成（前端运行时兼容性未验证，见下）
    修改文件：go.mod（新建）、.nvmrc（新建）、.node-version（新建）、
              web/package.json（新建）、web/package-lock.json（生成，267 条目）、
              api/openapi.yaml（新建）、redocly.yaml（新建）、
              scripts/check.sh（新建）、.github/workflows/ci.yml（新建）、
              .gitignore（新建）、docs/DEPENDENCIES.md（新建）
    完成行为：
      - 核实并冻结 Go 1.27.3、Node 22.23.3（npm 10.9.9），写入 go.mod / .nvmrc / .node-version。
      - 核实 7 个 Go 模块与 19 个 npm 包的最新版本、发布时间与 go/peer 约束；
        TypeScript 固定 5.9.3（typescript-eslint peer 上限 <6.1.0）。
      - 生成 package-lock.json（lockfileVersion 3，全部来自 registry.npmjs.org 且带 integrity），
        显式包含 linux-x64 的 rolldown/lightningcss 可选二进制。
      - api/openapi.yaml 骨架：信封、错误码枚举、安全方案（会话 Cookie + CSRF）、
        公共参数/头/响应；paths 为空（刻意），x-upanel-contract-status 标注阶段。
      - scripts/check.sh：仓库结构、工具链版本闸门、Go build/vet/test/gofmt、
        前端 typecheck/lint/test/build（脚本存在才运行）、OpenAPI lint；
        未具备的项输出 NOT RUN，FAIL 影响退出码。
      - CI 工作流：checkout@v7.0.1、setup-go@v7.0.0（1.27.3）、setup-node@v7.1.0（.nvmrc）、
        npm ci 后执行 sh scripts/check.sh。
    API/数据库变化：无业务 API；新增契约骨架文件（不含任何已实现端点）
    测试环境：开发宿主；Go 经 GOTOOLCHAIN 使用 1.27.3；Node 使用 22.23.3（仓库外安装）
    实际检查命令、退出码和结果：
      - GOTOOLCHAIN=go1.27.3 go version -> 0，go1.27.3
      - curl go.dev / proxy.golang.org / nodejs.org / registry.npmjs.org -> 0，版本见证据
      - npm install --package-lock-only --ignore-scripts -> 0
      - npm ci --ignore-scripts -> 0（added 241 packages）
      - npm ls --depth=0 -> 0（19 个依赖全部解析）
      - redocly lint --format=summary api/openapi.yaml -> 0（20 warnings: no-unused-components）
      - sh scripts/check.sh（Node 22.23.3）-> 0（PASS 2 / FAIL 0 / NOT RUN 8）
      - sh scripts/check.sh（宿主 Node 24.15.0）-> 1（node-version FAIL，闸门有效）
    证据路径：docs/evidence/P00/2026-10-20/{env.txt,dependency-resolution.txt,
              check-run.txt,check-run-host-node.txt,result.md}
    未运行检查：
      - Go build/vet/test/gofmt：仓库尚未有 Go 源码（P01.1）
      - 前端 typecheck/lint/test/build：尚无应用代码与 scripts（P01.3）
      - vue-router 5 / pinia 4 / vite 8 / vitest 5 / xterm 6 的运行时兼容性：
        仅按 registry 元数据与 peer 范围核实，未构建过应用 -> NOT RUN
      - Linux 上的 npm ci / Go 编译 / Node 22 ABI：无 Linux 环境 -> NOT RUN
      - Playwright 浏览器二进制：未安装 -> NOT RUN
      - CI 工作流：仓库未初始化 Git、无远端 -> NOT RUN
    已知问题/阻塞：无新增阻塞；依赖兼容性风险登记为 DECISIONS Q4/Q5
    下一步：P00.3（文档部分）

### P00.3（2026-10-20）— VM 验收说明、夹具与证据约定

    子任务：P00.3
    状态：部分完成 -> 阶段阻塞（VM 未创建）
    修改文件：docs/TEST_ENVIRONMENT.md（新建）、tests/fixtures/README.md（新建）、
              docs/evidence/README.md（新建）、docs/evidence/P00/2026-10-20/*（证据）
    完成行为：
      - 定义 VM-A（22.04）/VM-B（24.04）/VM-C（LVM 根）/VM-D（SMB 客户端）规格，
        数据盘 data-a/b/c 的容量与**显式序列号**约定、隔离硬性要求
        （禁止宿主盘透传/USB passthrough/读写共享目录）、快照与数据盘重建流程、
        VM 内环境核验脚本、证据采集约定、各阶段环境对应表。
      - 建立 tests/fixtures 规则（真实采集、脱敏、合成样本需标注）与各阶段最少夹具清单。
      - 建立 docs/evidence 目录约定与 P00 证据。
    实际检查：
      - hypervisor 探测：VBoxManage / vmrun / qemu-system-x86_64 / virsh 均未安装
      - Unix 环境：Windows 宿主 + Git Bash，无 /dev 块设备
    证据路径：docs/evidence/P00/2026-10-20/env.txt
    未运行检查（NOT RUN）：
      - 创建 Ubuntu 22.04/24.04 VM
      - 创建并核对可丢弃虚拟数据盘序列号（UPANEL-DATA-A-8G 等）
      - 验证数据盘与宿主磁盘隔离
      - 原因：无可用 hypervisor，安装虚拟化软件需要外部授权
    已知问题/阻塞：**P00.3 验收未通过**，P00 不能标记 passed，P01 不得开始
    下一步：获得 hypervisor 授权后执行 TEST_ENVIRONMENT.md 第 1—3 节，
              把 VM 核验输出存入 docs/evidence/P00/<新日期>/，再重新验收 P00

### P00.3 续（2026-10-20）— 验收 VM 实际创建与核验（解除上一记录的阻塞）

    子任务：P00.3
    状态：完成（本记录覆盖上一条 blocked 记录；上一条保留为历史，不删除）
    修改文件：docs/TEST_ENVIRONMENT.md（状态、第 3 节工具链说明、第 4 节重建流程、第 6—8 节重写/新增）、
              docs/DECISIONS.md（Q3 改为已解决，新增 Q8/Q9）、
              docs/DEPENDENCIES.md（宿主偏差表补虚拟化与环境事实）、
              README.md（P00 状态与 TEST_ENVIRONMENT 描述）、
              docs/PROGRESS.md（本文件）、
              docs/evidence/P00/2026-10-20/{vm-2404-verification.txt,vm-2204-verification.txt,
              host-isolation.txt,commands.log,check-run-post-p00.txt,result.md}（重写/刷新/新增）
              仓库之外：H:\Code\vms\launch-vm.ps1、H:\Code\vms\*\serve-seed.js、
              H:\Code\vms\upanel-2404\、H:\Code\vms\upanel-2204\（VM 磁盘与 cloud-init seed，
              不在仓库内，不入库）
    完成行为：
      - 在无管理员权限前提下解决 hypervisor 缺失：确认 Windows Hypervisor Platform 已启用
        （HypervisorPresent=True、WinHvPlatform.dll 存在），采用用户级便携 QEMU 11.1.0
        （H:\Code\.toolchains\qemu）配 -accel whpx；无系统级安装、无服务、删除目录即可回滚。
      - 使用官方 Ubuntu cloud image（22.04.5 / 24.04.5），下载后校验 SHA256SUMS 通过；
        原始镜像保留在仓库之外。
      - cloud-init NoCloud 因无法可靠生成 CIDATA ISO，改为经 HTTP 提供 seed：
        宿主回环只读白名单 HTTP 服务 + -smbios type=1,serial=ds=nocloud;s=http://10.0.2.2:PORT/。
      - 两台 VM 均为 2 vCPU / 4096 MB；virtio-scsi 挂 40 GiB 系统盘 + 三块数据盘，
        每块盘显式 serial=（UPANEL-SYS-40G / UPANEL-DATA-A-8G / UPANEL-DATA-B-4G /
        UPANEL-DATA-C-4G），符合 TEST_ENVIRONMENT.md 第 1 节。
      - 修正上一轮缺陷：VM-B 首次启动时系统盘漏配 serial，已关机后按统一参数重启并复验。
      - 修正上一轮缺陷：两台 VM 曾以 3072 MB 启动（低于文档 4 GiB 规格），
        关机后以 -m 4096 重启并复验；本条记录的核验输出全部来自 4096 MB 配置。
    实际检查命令、退出码和结果（全部在 VM 内实读，输出见证据文件）：
      - VM-B：lsb_release -a -> Ubuntu 24.04.5 LTS；uname -r -> 6.8.0-147-generic -> 0
      - VM-A：lsb_release -a -> Ubuntu 22.04.5 LTS；uname -r -> 5.15.0-198-generic -> 0
      - nproc -> 2 / 2；free -m -> Mem total 3915 MB / 3911 MB（-m 4096 扣除内核保留）-> 0
      - lsblk -d -o NAME,SIZE,TYPE,TRAN,VENDOR,MODEL,SERIAL,WWN ->
        sda 40G / sdb 8G / sdc 4G / sdd 4G，VENDOR=QEMU，MODEL=QEMU HARDDISK，
        SERIAL 与第 1 节表格逐项一致（两台 VM 相同）-> 0
      - ls -l /dev/disk/by-id/ -> scsi-0QEMU_QEMU_HARDDISK_UPANEL-{SYS-40G,DATA-A-8G,DATA-B-4G,DATA-C-4G} -> 0
      - udevadm ID_SERIAL_SHORT -> 与 lsblk SERIAL 一致（/dev/sda..sdd）-> 0
      - findmnt -no SOURCE,TARGET,FSTYPE,SIZE,USED,AVAIL / -> /dev/sda1 / ext4 37.7G（24.04）、
        38.6G（22.04）-> 0；lsblk 拓扑显示数据盘 sdb/sdc/sdd 无分区、无挂载，根在 sda
      - findmnt -t 9p,virtiofs,cifs,nfs,nfs4 -> 空（无宿主共享混入）-> 0
      - 阳性对照 findmnt -t ext4 -> 列出 /（24.04 另列 /boot），证明探测命令本身有效 -> 0
      - swapon --show -> 空（未启用 swap，避免干扰内存类验收）-> 0
      - 宿主侧 Get-Disk -> 物理盘 ST2000DM006(Z4Z8X457)、ST4000VN008(S0I6L66G)、
        Lexar NVMe、Samsung NVMe，均未出现在任何 -drive 参数中 -> 0
      - 宿主侧 QEMU 命令行实读：-drive 只引用 H:\Code\vms\ 下的 qcow2；
        hostfwd 仅绑 127.0.0.1:2222/2223；仓库内 *.qcow2/*.vdi/*.vmdk/*.iso 数量为 0 -> 0
      - qemu-img info --backing-chain system.qcow2 -> virtual size 40 GiB，后备链指向 cloud image -> 0
      - sh scripts/check.sh（Node 22.23.3，文档更新后复跑）-> 0（PASS 2 / FAIL 0 / NOT RUN 8），
        见 check-run-post-p00.txt
      - VM 关机与清理：systemctl poweroff 两台；宿主 QEMU 进程全部退出；
        端口 2222/2223/8899/8900 全部关闭 -> 0
    证据路径：docs/evidence/P00/2026-10-20/{vm-2404-verification.txt,vm-2204-verification.txt,
              host-isolation.txt,commands.log,check-run-post-p00.txt,result.md}
    未运行检查（NOT RUN，明确不声明通过）：
      - VM 内 Go/Node 工具链版本核验：VM 仅装基础系统，未安装工具链（P00 不要求；见 DECISIONS Q9）
      - smartctl / NVMe SMART 读取：virtio 盘无真实 SMART 数据（P04.3 前需另行确认）
      - 破坏性磁盘操作（分区/格式化）：P07 才做，且每用例前回快照并重建数据盘
      - VM-C（LVM 根）、VM-D（SMB 客户端）：未创建，P04 / P08 前补齐
      - VM 快照机制：当前为文件级重建（保留原始 cloud image 副本），未使用 qemu 内部快照
    已知问题/阻塞：无。P00 三项子任务全部完成，验收条件满足。
    下一步：P01.1

### P01.1（2026-10-20）— 目录结构、两个服务入口与 CLI、配置、日志、版本、live health、Linux/mock 分离

    子任务：P01.1（STAGES.md 第 34 行）
    状态：完成
    修改文件：
      cmd/upanel-web/main.go、cmd/upanel-agent/main.go、cmd/upanelctl/main.go（+ 各自 _test.go）
      cmd/upanel-web/main_nonlinux_test.go（//go:build !linux，断言 production 拒绝启动）
      internal/version/version.go、internal/platform/{platform.go,platform_linux.go,platform_other.go}
      internal/logging/logging.go、internal/httpx/httpx.go、internal/config/config.go
      internal/config/{defaults_linux.go,defaults_other.go}
      internal/api/server.go、internal/agent/server.go
      tests/integration/p01-1-acceptance.sh（新建，入库的 Linux 验收脚本，26 项断言）
      tests/integration/p01-1-host-refusal.sh（新建，入库的宿主拒绝/契约核验脚本，16 项断言）
      web/go.mod（新建，构建边界，非依赖文件）
      go.mod（仅改注释：P01.1 零第三方依赖，P01.2 首次导入）
      scripts/check.sh（新增 go-build-linux / go-vet-linux 两个槽位）
      docs/evidence/P01/2026-10-20/*（新建 7 个证据文件）
    完成行为：
      - 三个入口：upanel-web（默认 127.0.0.1:9443，非 root，无 sudo）、
        upanel-agent（root，仅 /run/upanel/agent.sock，0660）、upanelctl（version / check-config；
        init-admin、reset-password、migrate、scheduler 明确报“尚未实现（计划阶段 …）”，退出码 3）。
      - 配置：JSON 严格解码（未知字段、尾部多余内容均报错）；显式 --config 路径不存在即失败；
        默认路径 /etc/upanel/upanel.json 缺失时按内置默认值运行；CLI 覆盖配置文件。
      - TLS 未接入前的保护（PLAN.md 第 6 节要求面板自身监听 HTTPS 9443，TLS 属 P10）：
        production 绑定非回环地址直接以退出码 2 拒绝，不做“先明文跑起来再说”。
      - mock 模式只能绑 127.0.0.1:18881，显式指定其他地址直接退出码 2，不做静默回退。
      - 日志：log/slog，json/text，未知级别或格式报错而不是降级；启动日志记录监听地址、
        network、mode、loopback_only、version、pid、started_at。
      - HTTP 契约：成功/错误信封与 request_id 一致（同一 rid 同时写入 body 与 X-Request-ID 头）；
        支持 HEAD；405 带 Allow；未知路径返回 JSON 404，绝不返回页面或 200。
      - 代理：非 root 拒绝启动；socket 目录必须预先存在（不在代码里创建）；
        同名普通文件/目录拒绝覆盖且保留原文件；停止时删除 socket。
      - 平台分离：非 Linux 上 production 显式拒绝（退出码 1），mock 可在宿主运行。
    接口/数据库变化：
      - 新增公开接口 GET/HEAD /api/v1/health/live（返回 {"status":"ok"}，不含版本、路径、主机信息）。
      - 新增代理内部接口 GET /internal/v1/capabilities（version/commit/build_date/go_version/mode/
        os/arch/started_at/supported_actions[]/dependencies[]，当前两个数组为空）。
      - 数据库：无（P01.2 引入）。
    实际检查命令、退出码与结果：
      - go build ./... / go vet ./... / go test ./... -> 0（10 个包 ok）；gofmt -l -> 空
      - GOOS=linux GOARCH=amd64 go build ./... 与 go vet ./... -> 0（静态链接 ELF）
      - sh scripts/check.sh（PATH 前置 nvm 22.23.3）-> 0，PASS 8 / FAIL 0 / NOT RUN 4
        （NOT RUN = web typecheck/lint/test/build，P01.3 才有脚本）
      - 同一命令在未前置 PATH 时 -> 1（node-version FAIL，证明版本闸门有效）
      - 宿主 sh tests/integration/p01-1-host-refusal.sh -> 0（PASS 16 / FAIL 0）
      - Windows 宿主：--mode production -> 1；--mode production --listen 0.0.0.0:9443 -> 2；
        upanel-agent.exe -> 1；upanel-agent.exe --mode mock -> 2；
        mock 冒烟（health 200 / / 与 /index.html 404 JSON / POST 405 + Allow）全部符合契约；
        kill -TERM -> 143，端口无残留
      - Ubuntu 24.04.5 与 22.04.5 各跑 tests/integration/p01-1-acceptance.sh -> 0，PASS 26 / FAIL 0
        （CLI、mock 绑定、production 以 uid 1000 运行、request_id 回显、405、production 与 mock
        各自拒绝非回环地址、代理非 root 拒绝、socket 0660、代理无 TCP 监听、capabilities 空数组、
        未知内部路径 404、socket 清理、拒绝覆盖非 socket 文件）
      - VM 内 /tmp 二进制 sha256 与仓库 bin/linux-amd64 完全一致
      - 清理：两台 VM systemctl poweroff -> 0；宿主 qemu 进程数 0；
        端口 2222/2223/8899/8900 全部关闭；seed 服务进程已停止
    证据路径：docs/evidence/P01/2026-10-20/{env.txt,host-verification.txt,vm-2404-acceptance.txt,
              vm-2204-acceptance.txt,check-run.txt,commands.log,result.md}
    未运行检查（NOT RUN，不声明通过）：
      - systemd 单元、upanel-web 系统用户的真实部署（P01.4）
      - Nginx 与 TLS（P10）；当前 Web 只有回环明文 HTTP
      - 代理 SO_PEERCRED 校验、动作注册、鉴权（P02.3）
      - 前端 typecheck/lint/test/build（P01.3）
      - CI 远端执行（仓库未初始化 Git）
      - VM 内构建（VM 无 Go/Node，使用交叉编译产物）
      - 并发/长时间运行压测（P16 范围）
    已知问题/阻塞：无阻塞。已在 DECISIONS 登记 Q10—Q15（配置格式与严格解码、mock 绑定规则、
              web/go.mod 构建边界、TLS 未接入且 production 拒绝非回环、宿主 Node 双版本、
              零第三方 Go 依赖）。
    下一步：P01.2

### P01.2（2026-10-20）— SQLite 迁移机制、事务工具、错误响应、request_id、中间件与 API 路由组织

    子任务：P01.2（STAGES.md 第 34 行）
    状态：完成
    修改文件：
      internal/httpx/httpx.go（重写：24 个契约错误码 + 状态码映射、request_id、信封、响应头）
      internal/api/server.go、internal/api/middleware.go、internal/api/router.go（改用 Gin）
      internal/api/middleware_test.go（新建）
      internal/store/store.go、internal/store/migrate.go（新建包）
      internal/store/store_test.go、internal/store/migrate_test.go（新建）
      migrations/migrations.go（新建，embed.FS）、migrations/0001_settings.sql（新建）
      internal/config/config.go（新增 database 段与校验）
      internal/config/defaults_linux.go、defaults_other.go（数据库默认路径）
      internal/config/config_test.go、internal/config/defaults_linux_test.go（新建/扩充）
      cmd/upanel-web/main.go（--db、打开数据库、迁移、失败即拒绝启动）、main_test.go（扩充）
      cmd/upanelctl/main.go（实现 migrate 子命令；check-config 报告数据库配置）、main_test.go（扩充）
      go.mod、go.sum（首次写入第三方依赖：Gin v1.12.0、modernc.org/sqlite v1.60.1）
      api/openapi.yaml（登记 GET/HEAD /api/v1/health/live；说明 INTERNAL_ERROR 例外）
      tests/integration/p01-2-acceptance.sh（新建，34 项断言）
      tests/integration/p01-1-acceptance.sh、p01-1-host-refusal.sh（数据库成为硬依赖后补 --db）
      docs/evidence/P01/2026-10-20-p01.2/*（新建 13 个证据文件）
    完成行为：
      - 数据库：`database/sql` + `modernc.org/sqlite`（纯 Go，无 cgo）。打开时从连接读回 PRAGMA
        并逐项校验 journal_mode=wal、foreign_keys=1、busy_timeout=配置值；任一不符直接失败，
        拒绝在“以为开了保护、实际没开”的状态下运行。日志记录的是读回的实际值。
      - 路径规则：必须是绝对路径、不得含 `?`/`#`；数据目录必须已存在（不在代码里创建，
        目录归属由安装步骤决定）；打开失败、迁移失败都让 Web 以退出码 1 拒绝启动。
      - 迁移：显式 SQL 文件（`migrations/NNNN_name.sql`，embed 进二进制），文件名格式校验、
        重复序号与空文件检测；每条迁移独立事务、按版本升序应用；已应用迁移的 SHA-256 写入
        schema_migrations 并在每次启动时校验（改文件或删文件都拒绝执行）；数据库版本高于程序
        内置版本时拒绝（只前滚，不降级）。
      - 事务：`store.WithTx` 在错误与 panic 时回滚（panic 继续向上抛），成功才提交。
      - 错误响应：24 个契约错误码为封闭枚举，`StatusForCode` 映射到 400/401/403/404/409/422/501/503；
        未知错误码按 500 处理而不是猜一个 400。信封 `{"error":{code,message,details},request_id}`，
        响应头 X-Request-ID、Cache-Control: no-store、X-Content-Type-Options: nosniff。
      - request_id：中间件在入口决定一次（合法入站值沿用，非法值丢弃重新生成），写入上下文；
        响应头、响应体、访问日志三处取同一个值。
      - 中间件顺序：requestID → accessLog → recovery → bodyLimit（1 MiB 上限）。
        panic 转 500 `INTERNAL_ERROR`，堆栈只进日志、不进响应体；超限请求体可通过
        `IsBodyTooLarge`（`*http.MaxBytesError`）映射为 413。
      - 路由：Gin，关闭尾斜杠与 fixed-path 重定向（避免 301 被当成成功），
        NoRoute → JSON 404，NoMethod → 405 + Allow；不预埋“返回 501 的空路由”。
      - CLI：`upanelctl migrate [--config PATH] [--db PATH] [--json]`，退出码 0 成功、
        1 打开或迁移失败、2 用法错误（例如未给数据库路径）；`check-config` 增加
        `database_path` 与 `database_busy_timeout_ms`。
    接口/数据库变化：
      - 数据库：新增表 `settings(key PK, value_json, version, created_at, updated_at)` 与
        `idx_settings_updated_at`；新增迁移跟踪表 `schema_migrations(version PK, name, checksum, applied_at)`。
      - 公开接口：GET/HEAD /api/v1/health/live 写入 openapi.yaml（形状不变：data.status="ok"）。
      - 错误码枚举未扩张（仍 24 个）；`INTERNAL_ERROR` 只用于程序故障（panic、序列化失败），
        不属于业务枚举，已在 openapi.yaml 与 DECISIONS Q17 明确。
    实际检查命令、退出码与结果：
      - go build ./... / go vet ./... / go test ./... -count=1 -> 0（11 个包 ok，
        upanel/migrations 无测试文件）；gofmt -l -> 空
      - 新增测试覆盖：PRAGMA 生效与非法路径、事务提交/回滚/panic 回滚、数据跨重开保留、
        真实迁移文件建表+索引、二次执行为空操作、校验和不匹配/迁移文件被删/版本超前、
        失败迁移整条回滚、中间件（request_id 生成与沿用、未知路径 JSON 404、405+Allow、
        HEAD 无正文、panic→500 且不泄漏堆栈、请求体超限）、config database 校验、
        upanelctl migrate（新库、幂等、文本/JSON、路径非法、数据库版本超前）、
        Web（--db 相对路径、未配置路径、目录缺失、迁移失败 → 拒绝启动）
      - 详细逐项结果：go test ./internal/api ./internal/store ./internal/config ./cmd/upanelctl
        ./cmd/upanel-web -v -> 63 项 PASS / 0 FAIL
      - sh scripts/check.sh（PATH 前置 nvm 22.23.3）-> 0，PASS 8 / FAIL 0 / NOT RUN 4
        （NOT RUN = web typecheck/lint/test/build，P01.3 才有脚本）
      - go mod download -> 0；go mod verify -> 全部模块校验通过；go.sum 134 行
      - 宿主 sh tests/integration/p01-1-host-refusal.sh -> 0（PASS 16 / FAIL 0，回归）
      - Ubuntu 24.04.5 跑 tests/integration/p01-2-acceptance.sh -> 0，PASS 34 / FAIL 0
      - Ubuntu 22.04.5 跑 tests/integration/p01-2-acceptance.sh -> 0，PASS 34 / FAIL 0
      - 两台 VM 各复跑 tests/integration/p01-1-acceptance.sh -> 0，PASS 26 / FAIL 0（回归）
      - 覆盖的 Linux 真实行为：真实文件建库、SQLite 文件头校验、数据库文件归属运行用户、
        重复迁移不再应用、Web 启动日志里 journal_mode=wal / foreign_keys=true / busy_timeout_ms=5000、
        迁移状态跨进程重启保留（重启后 applied_versions=[]）、-wal 文件存在、
        目录缺失与目录不可写（root 0500）都让 Web 退出码 1 且不开始监听、
        非 SQLite 随机文件迁移失败且不输出成功信息、相对路径与空路径按用法错误处理
      - VM 内 /tmp 三件二进制 sha256 与仓库 dist/p01-2/ 完全一致
      - 清理：两台 VM systemctl poweroff -> 0；宿主 qemu 进程数 0；
        2222/2223/8899/8900 无 LISTENING 残留（只剩本机客户端侧 TIME_WAIT）；seed 服务进程已结束
    证据路径：docs/evidence/P01/2026-10-20-p01.2/{env.txt,vm-2404-p01-2.txt,vm-2204-p01-2.txt,
              vm-2404-p01-1-regression.txt,vm-2204-p01-1-regression.txt,host-refusal.txt,
              go-test.txt,go-test-verbose.txt,dependency-resolution.txt,check-run.txt,
              cleanup.txt,commands.log,result.md}
    未运行检查（NOT RUN，不声明通过）：
      - 迁移的“真实升级路径”（0002 及以后的文件）在 Ubuntu 上执行：当前只有 0001，
        多步序列与校验和异常由单元测试用临时迁移集证明，未在 VM 上用真实新版本升级
      - 迁移过程中断电/被 SIGKILL 的崩溃恢复：只验证了单条迁移失败回滚，未做杀进程实验
      - 多进程并发写同一数据库的 busy_timeout 行为：未压测（P02 起有并发写再补）
      - 磁盘满、只读文件系统、NFS/overlay 等非 ext4 位置上的 WAL 行为：未测
      - systemd 单元与 upanel-web 系统用户的真实部署（P01.4）；当前 VM 用普通用户直接跑二进制
      - Nginx 与 TLS（P10）；Web 仍只有回环明文 HTTP
      - 前端 typecheck/lint/test/build（P01.3）
      - CI 远端执行（仓库未初始化 Git）
      - VM 内构建（VM 无 Go/Node，使用交叉编译产物）
    已知问题/阻塞：无阻塞。已在 DECISIONS 登记 Q16—Q20（database 配置段与路径规则、
              Gin 只用于 Web API 而代理保持 net/http、迁移校验和与禁止降级、
              INTERNAL_ERROR 在契约枚举之外、传递依赖版本低于冻结表）。
              P01.1 的两个验收脚本在数据库成为硬依赖后补了 `--db`（测试夹具更新，
              不是产品行为变化）；p01-2 脚本首轮有 1 项断言写法错误（把 JSON 里的 `[`
              转义了），修正后重跑才作为最终证据。
    下一步：P01.3

### P01.3（2026-10-20）— Vue 页面骨架、路由、Naive UI、中文导航、统一 API 客户端、错误组件、mock 横幅

    子任务：P01.3（STAGES.md 第 36 行）
    状态：完成
    新增文件（全部在 web/ 下）：
      index.html（lang=zh-CN、referrer=no-referrer、内联 SVG 图标、模式注入说明）
      vite.config.ts、vitest.config.ts、tsconfig.json、env.d.ts、eslint.config.js
      scripts/smoke.mjs（真实 Chromium 冒烟，production 与 mock 两种产物）
      src/main.ts、src/App.vue、src/styles/global.css
      src/api/types.ts（24 个契约错误码 + 3 个客户端码 + 信封类型）
      src/api/client.ts（统一 fetch 封装：同源、信封解包、request_id、CSRF/幂等头、超时、不自动重试）
      src/api/errors.ts（code → 标题/建议/可重试性/是否需重新登录）
      src/stores/mode.ts（production/mock 解析与 Pinia store）
      src/modules/registry.ts（PLAN 第 7 节 16 项导航，唯一来源；未实现项 implementedIn=null）
      src/router/routes.ts（纯路由数据）、src/router/index.ts（createWebHistory + 标题）
      src/components/AppShell.vue（品牌 + 只渲染可见模块的 NMenu + 内容区）
      src/components/MockBanner.vue（不可关闭的演示模式横幅）
      src/components/StatePanel.vue（七种状态统一组件，状态用文字标签而非仅颜色）
      src/components/ApiErrorPanel.vue（message + 错误码 + request_id + 条件重试按钮）
      src/views/OverviewView.vue（只验证 /health/live 与客户端链路，不编造系统数据）
      src/views/NotFoundView.vue（404）
      测试：src/api/client.test.ts（12）、src/api/errors.test.ts（5）、src/stores/mode.test.ts（5）、
            src/modules/registry.test.ts（7）、src/router/routes.test.ts（4）、
            src/components/panels.test.ts（17，SSR 渲染断言）
    修改文件：
      web/package.json（新增 scripts：dev/build/preview/typecheck/lint/test/test:watch/smoke/smoke:mock）
      .gitignore（忽略 /web/dist-mock/）
    未修改：Go 侧任何源码、api/openapi.yaml、依赖版本（package-lock.json 未变，sha256 见证据 env.txt）

    已实现：
      - 导航唯一数据源 = modules/registry.ts：16 项按 PLAN 顺序登记，未实现项 implementedIn=null，
        visibleModules() 过滤后菜单只有“概览”；未实现模块也不进路由表，直接访问落到 404 页面，
        不存在“可点击但无效”的入口。
      - 统一 API 客户端：仅同源 /api/v1；GET/HEAD 禁止 body 且不带写头；写请求带
        Content-Type、X-UPanel-Request: 1、可选 X-CSRF-Token 与 Idempotency-Key；
        credentials=same-origin；AbortSignal.timeout + any 组合超时；成功必须 {data, request_id}
        齐全，失败必须是契约内的 code，否则判 INVALID_RESPONSE（不猜业务原因）。
      - 错误语义分离：source=client（NETWORK_ERROR / INVALID_RESPONSE / ABORTED）与
        source=server（24 码）；未知服务端码不隐藏，按 status≥500 决定是否可重试。
        客户端不做任何自动重试（写操作由服务端幂等键决定）。
      - mock 横幅：mock 模式在布局最上方持续显示“演示数据，未操作服务器”，无关闭按钮；
        概览页额外用 NTag 标注当前模式。
      - 七状态表达：StatePanel 覆盖 加载/空数据/无权限/依赖未安装/数据不支持/请求失败/部分数据可用，
        每个状态都有中文标签 + 标题 + 说明，部分可用时列出缺失项，错误时可带 code 与 request_id。
      - 模式来源：window.__UPANEL_MODE__ → VITE_UPANEL_MODE → production（默认）。
      - 冒烟脚本证明的关键点：预览环境没有后端时页面显示 INVALID_RESPONSE + 错误码 + 说明，
        不把 200/404 的 HTML 当作成功数据、不停留在加载中；mock 产物切换路由后横幅仍在。

    已验证（均可复跑，证据见 docs/evidence/P01/2026-10-20-p01.3/）：
      - npm --prefix web run typecheck：exit 0（strict、exactOptionalPropertyTypes、noUncheckedIndexedAccess）
      - npm --prefix web run lint：exit 0（ESLint 10 flat config + typescript-eslint，src/**/*.ts）
      - npm --prefix web run test：6 文件 / 50 用例全部通过
      - npm --prefix web run build：exit 0，2697 模块，dist/index.html 1.28 kB + 主包 413.53 kB（gzip 131.25 kB）
      - npm --prefix web run smoke：16 / 16（production 产物，真实 Chromium 无头）
      - npm --prefix web run smoke:mock：17 / 17（VITE_UPANEL_MODE=mock 构建 → dist-mock）
      - sh scripts/check.sh：PASS 12 / FAIL 0 / NOT RUN 0（Go 6 项 + node-version + 前端 4 项 + openapi-lint）
    未验证（不计为通过）：
      - 同源 API + 静态页面由 Go Web 二进制提供（P01.4）
      - mock 服务端绑定 127.0.0.1:18881、production 永不连 mock（mock 服务端尚未实现）
      - 页面与真实后端的成功路径：当前唯一可访问的 /health/live 在预览环境不可达，
        因此“成功信封渲染”只有单元测试（stub fetch）证据，没有真实后端证据
      - Ubuntu 虚拟机上的浏览器访问（P01.3 无系统写操作、无 Linux 依赖；P01.4 起才需要 VM）
      - 浏览器交互（点击/键盘/焦点/可访问性工具扫描）：仓库未落地 e2e 用例集
      - .vue 文件的 ESLint 检查（依赖清单无 eslint-plugin-vue，由 vue-tsc + SSR 断言覆盖）
      - 依赖大版本的浏览器兼容性矩阵（只在 Chromium 156 上跑过）
    已知问题/阻塞：无阻塞。DECISIONS 新增 Q21（node-only vitest + SSR 断言、ESLint 不覆盖 .vue、
              模式来源与 P02 /meta 缺口）。症状与处置：
              - vue-tsc 在 exactOptionalPropertyTypes 下拒绝 withDefaults 传 undefined 的默认值，
                改为显式 `?: string | undefined`（不是放宽编译选项）。
              - eslint 首轮扫到 dist-mock 里的压缩产物（905 个 error），把 dist-mock 加入 ignores，
                不是关规则。
              - Playwright 首次运行缺 chromium 1248（本机只有 1223），装匹配浏览器后通过。
              - SSR 断言里 naive-ui 组件会带 data-v-* 与 n-button 包装结构，断言改为正则/`>重试<`。
    下一步：P01.4（前端构建嵌入 Go Web 二进制、开发/生产配置、初版 systemd 单元模板、本地启动文档）

### P01.4（2026-10-20）— 前端嵌入 Go Web 二进制、开发/生产配置、systemd 单元模板、本地启动文档

    子任务：P01.4（STAGES.md 第 37 行）；P01 整体验收在同轮完成
    状态：完成
    新增文件：
      internal/webui/webui.go（`//go:embed all:dist`、入口页注入运行模式、CSP nonce、
        静态资源缓存策略、SPA 回退、405/503/404 边界）
      internal/webui/webui_test.go（14 个用例）
      internal/webui/dist/.keep（占位：真实产物由 scripts/build.sh 同步，不提交）
      internal/api/frontend_test.go（5 个用例：前端委托与未知 API 边界）
      scripts/build.sh（前端构建 → 同步 dist → Go 构建三个二进制 → 打印内嵌大小）
      deploy/systemd/upanel-web.service（User=upanel-web + 加固项 + StateDirectory）
      deploy/upanel.example.json（production：127.0.0.1:9443）
      deploy/upanel.mock.example.json（mock：127.0.0.1:18881 + 独立数据库文件）
      tests/integration/p01-4-acceptance.sh（Linux 验收：114 项断言）
      docs/LOCAL_START.md（构建、本地运行、前端开发、检查、systemd 安装、故障排查）
      docs/evidence/P01/2026-10-20-p01.4/（env、build、交叉编译、host 检查、浏览器冒烟、
        503 行为、非 Linux 拒绝、两台 VM 验收、check-run、commands.log、result.md）
    修改文件：
      internal/api/server.go（Options 新增 Frontend http.Handler）
      internal/api/router.go（新增 handleNoRoute/isAPIPath：API 前缀先判，命中即 JSON 404；
        405 处理器只回 JSON，去掉不可达的前端分支）
      cmd/upanel-web/main.go（初始化 webui handler 并注入模式；缺资源时告警而非静默）
      .gitignore（忽略 /internal/webui/dist/* 与 /web/dist-mock/，保留 .keep）
      web/scripts/smoke.mjs（支持 --base/--expect 直接打真实二进制，不再只能对 vite preview）
      README.md（状态、构建与运行、验收脚本清单、LOCAL_START 入口）
    未修改：技术栈与依赖版本（package-lock.json、go.mod 未变）、api/openapi.yaml（无新接口）、
            PLAN/CONTRACTS/STAGES 三份规格文档

    已实现：
      - 前端产物编译期嵌入：仓库只提交 dist/.keep；scripts/build.sh 先构建 web 再同步到
        internal/webui/dist 再 go build（顺序错就等于没嵌入）。没有产物时启动打 WARN，
        页面请求一律 503 + 指向构建脚本的说明，绝不返回“看起来正常”的空页面。
      - 运行模式由服务端注入：index.html 的 `</head>` 前插入
        `<script nonce="…">window.__UPANEL_MODE__="…"</script>`，值经 json.Marshal 转义，
        被污染也无法闭合 script 标签；入口页 Cache-Control: no-store，避免 mock/production 串台。
      - 安全响应头：nosniff、Referrer-Policy: no-referrer、X-Frame-Options: DENY、
        COOP: same-origin；CSP 为 `default-src 'self'`、`script-src 'self' 'nonce-<每响应随机>'`、
        `style-src 'self' 'unsafe-inline'`（Naive UI 运行时注入样式，架构决定）、`object-src 'none'`、
        `frame-ancestors 'none'` 等。nonce 每次响应重新生成（16 字节 crypto/rand）。
      - 静态资源策略：`/assets/<name>-<hash>.<ext>` 走 `public, max-age=31536000, immutable`，
        其他静态文件 1 小时；内容类型用固定映射表，不依赖宿主注册表。
      - 路径边界：只对无扩展名的页面路径回退到 index.html；带扩展名的缺失资源返回 404；
        路径先 Clean 再拼到 embed FS，`../` 穿越既读不到宿主文件，也不会被当成资源。
      - API 边界：`/api` 与 `/api/*` 在 NoRoute 里优先判为 JSON 404 `NOT_FOUND`，
        无论 GET/POST 都不会落到静态回退，也就不可能返回 index.html 或 200。
      - 运行模式准入：mock 只允许 `127.0.0.1:18881`（命令行或配置文件写错地址都退出码 2）；
        production 在 TLS（P10）之前只允许回环地址；非 Linux 上 production 退出码 1 并说明原因。
      - systemd：`User=upanel-web`、NoNewPrivileges/ProtectSystem=strict/ProtectHome 等加固，
        `StateDirectory=upanel` 提供数据库目录，`ConfigurationDirectory=upanel` 提供配置目录，
        `ExecStartPre=upanelctl check-config` 在配置非法时直接失败。代理单元留给 P02.3。

    已验证（均可复跑，证据见 docs/evidence/P01/2026-10-20-p01.4/）：
      - gofmt -l cmd internal：空输出；go vet ./...：exit 0
      - go test ./... -count=1：12 个包全部 ok；webui + api 33 个用例 PASS / 0 FAIL
      - sh scripts/build.sh：exit 0，`bin/upanel-web` 28.7 MB，内嵌入口页 1287 字节
      - 交叉编译 bin/linux-amd64（ELF、静态链接）：upanel-web 19.4 MB（未嵌入时 6.8 MB）
      - 宿主 curl 检查（mock 二进制）：页面 200 + nonce CSP + 注入 mock；hashed 资源
        text/javascript + immutable；`/api/v1/health/live` JSON 信封 + X-Request-ID；
        `/api`、`/api/`、`/api/v1/unknown`、`/api/v2/health/live` 均为 JSON 404 NOT_FOUND；
        `/storage` SPA 200；`POST /` 405 + Allow: GET；`--mode mock --listen 0.0.0.0:18881` 退出码 2
      - 真实 Chromium 冒烟（对二进制，非同源 mock）：19 / 19 PASS
      - 无嵌入资源的二进制（临时移除 dist/index.html 后构建）：启动 WARN + 页面 503 +
        `/api/v1/health/live` 仍 200，证明“缺依赖不伪装成正常”
      - Windows 宿主 `--mode production`：退出码 1，"production 模式仅支持 Linux"
      - Ubuntu 24.04.5（VM-B）：p01-4 验收 114 PASS / 0 FAIL / 0 NOT RUN，exit 0
      - Ubuntu 22.04.5（VM-A）：同上 114 PASS / 0 FAIL / 0 NOT RUN，exit 0
      - 两台 VM 均真实执行 `systemd-analyze verify` + 安装单元 + `systemctl start`：
        is-active=active、页面 200 且注入 production、MainPID 属 `upanel-web`（非 root、非 PID 1）、
        `/var/lib/upanel` 0750 属服务用户、停止后端口释放；验完 stop + 删除单元
      - sh scripts/check.sh：PASS 12 / FAIL 0 / NOT RUN 0（含 go-build-linux / go-vet-linux）
    未验证（不计为通过）：
      - 真实生产服务器安装（本仓库只有验收 VM，没有生产机；部署动作本身未执行）
      - TLS/HTTPS（P10）；9443 目前是明文回环，因此 production 只允许回环地址
      - 认证与 `GET /meta`（P02），前端运行模式仍完全依赖服务端注入
      - Firefox/Safari/移动端浏览器（仍只有 Chromium 156 证据）
      - 代理 systemd 单元与 socket 属主/权限（P02.3）
      - 配置热重载（未实现，改配置需重启）
    已知问题/阻塞：无阻塞。DECISIONS 新增 Q22（缺前端产物走 503 + 告警而不是拒绝启动；
              代理单元延后到 P02.3；静态资源缓存策略与 CSP 取舍）。

    P01 整体验收（STAGES.md 第 39 行）逐条：
      - production 不连接 mock：**通过**——二进制没有 mock 数据通路，production 页面注入
        production、读真实 SQLite（独立库文件），mock 端口不监听。注意：有真实系统数据的接口
        要到 P04 才出现，届时需重新核对“production 不连接 mock”的实现。
      - 非 Linux production 明确拒绝：**通过**（Windows 宿主实跑，退出码 1）。
      - mock 仅绑定本地 18881：**通过**（两台 VM 的 ss 断言 + 写错地址退出码 2）。
      - 同源 API 和静态页可加载：**通过**（两台 VM 页面 200 + 同源 /api/v1 信封）。
      - 未知 API 不返回 index.html 或 200：**通过**（5 个未知 API 路径 + POST，均 JSON 404）。
      - 无依赖时不伪装成正常：**通过**（缺前端 503 + 告警；缺数据库拒绝启动，见 P01.2）。
    下一步：P02.1（管理员创建、登录/退出、会话轮换、CSRF、Origin、限速）

### P02.1（2026-10-20）— 首个管理员、密码哈希、登录/退出、会话轮换、CSRF、Origin、限速

    子任务：P02.1（STAGES.md：通过 upanelctl 创建第一个管理员；密码哈希、登录、退出、
      会话轮换、CSRF、Origin、限速；无默认密码）
    状态：passed（宿主 38 项 + 两台 Ubuntu VM 各 58 项断言全通过；未验证项见「未验证」）
    新增文件：
      migrations/0002_users_sessions.sql（users：username COLLATE NOCASE UNIQUE、password_hash、
        role CHECK(admin|viewer)、disabled、version、时间戳；sessions：id_hash UNIQUE、
        user_id FK ON DELETE CASCADE、csrf_hash、created_at、last_seen_at、expires_at、
        reauth_until；索引 user_id / expires_at）
      internal/auth/password.go（Argon2id PHC 哈希、参数解析防御上限、策略校验、令牌生成与哈希）
      internal/auth/service.go（Service/Options、默认 30 分钟空闲 + 12 小时绝对超时、时钟注入）
      internal/auth/users.go（Role、用户名/显示名校验、CreateUser、CreateFirstAdmin、凭据读取）
      internal/auth/session.go（Login 编排、Issue/Authenticate/Revoke/RevokeAllForUser/
        RotateCSRF/PurgeExpired、滑动超时节流、禁用账号即时失效、防账号枚举的等价耗时校验）
      internal/auth/ratelimit.go（进程内固定窗口限速：用户名与来源 IP 双维度、窗口到期自动恢复、
        成功登录只清用户名维度）
      internal/httpx/origin.go（NormalizeOrigin、OriginSet：逐字比较、不做通配）
      internal/api/auth.go（login/session/logout 处理器、requireSession/requireCSRF/originGuard、
        Cookie 读写、remoteIP 不解析代理头）
      internal/api/auth_test.go（14 组 HTTP 级用例）；internal/api/testsupport_test.go（真实临时库）
      internal/auth/{password,users,session,ratelimit}_test.go；internal/httpx/origin_test.go
      cmd/upanelctl/initadmin.go（init-admin：终端无回显两次输入 / 管道两行一致、拒绝重复初始化）
      tests/smoke/p02-1-auth-smoke.sh（宿主冒烟 38 项断言）
      docs/evidence/P02/2026-10-20-p02.1/（env、go-test、go-vet、gofmt、store-filemode-test、
        argon2-timing、host-smoke、vm-2404-env、vm-2404-acceptance、vm-2204-env、
        vm-2204-acceptance、commands.log、result.md）
    修改文件：
      internal/store/store.go（`tightenFileModes`：库文件与 -wal/-shm 收到 0600，Open 时应用）
      internal/store/migrate.go（迁移结束再收紧一次：WAL/SHM 可能到写库后才出现）
      internal/store/store_test.go（TestOpenTightensDatabaseFileMode；Windows 上 SKIP）
      tests/integration/p02-1-acceptance.sh（新建，58 项断言，VM 内非 root 运行）
      internal/config/config.go（web.allowed_origins + 校验 + ResolveAllowedOrigins 跨字段规则）
      internal/api/server.go（Options 新增必需项 Auth 与 AllowedOrigins；Server 持有 OriginSet）
      internal/api/router.go（/auth 三个路由 + /api/v1 级 originGuard）
      cmd/upanel-web/main.go（构造认证服务、启动与每 15 分钟清理过期会话、无账号时 WARN、
        解析允许源）
      cmd/upanelctl/main.go（注册 init-admin；check-config 输出 allowed_origins 并做同一判定）
      api/openapi.yaml（/auth/login、/auth/session、/auth/logout 与 User/LoginRequest/LoginData/
        SessionData/EmptyData、RequestMarker/RequestOrigin 参数；x-upanel-contract-status → P02.1）
      docs/DECISIONS.md（Q23—Q29）、docs/DEPENDENCIES.md（x/crypto v0.58.0、x/term v0.47.0 成为
        直接依赖；解析版本表）、
      README.md（状态段改为“P00、P01 已通过，P02 进行中”，写入认证能力、无默认密码、
        Secure cookie、init-admin/认证冒烟命令与 Linux 验收脚本清单）、
      docs/LOCAL_START.md（新增 3.1 首次创建管理员；§5 增加 p02-1-auth-smoke.sh 用法）
      internal/store/migrate_test.go、cmd/upanelctl/main_test.go、internal/api/{server,frontend,
        middleware}_test.go（迁移数量与必需选项的断言从“写死 1 条”改为“连续升序”）
    未修改：技术栈、冻结依赖版本（只新增 x/crypto、x/term 两个已冻结/登记的模块）、
            PLAN/CONTRACTS/STAGES 三份规格文档

    已实现：
      - 无默认密码：`upanelctl init-admin` 是本机唯一入口。终端下两次无回显输入；非终端读两行
        且必须一致（只给一行即失败）；已有任何账号时在提示输入密码之前就拒绝；输出只有用户名、
        角色、数据库路径、schema 版本，从不输出密码或哈希。
      - 密码存储：Argon2id，参数 m=128 MiB / t=3 / p=2，随 PHC 字符串入库；解析时对 m/t/p/salt/key
        设防御上限；`NeedsRehash` 在登录成功后就地升级旧参数哈希（不需要迁移）。
      - 会话：`sessions` 只存令牌与 CSRF 的 SHA-256；Cookie `__Host-upanel_session`
        （HttpOnly/Secure/SameSite=Strict/Path=/、无 Domain、32 字节随机）；30 分钟空闲滑动 +
        12 小时绝对上限（绝对上限不可通过滑动续期突破）；滑动写库按 1 分钟节流；
        登录时若携带旧 Cookie 则一并作废；退出即删会话；账号被禁用时其会话下一次校验即失效并被删除。
      - CSRF：非 GET/HEAD 需 `X-CSRF-Token`，与会话里保存的哈希常量时间比较；
        `GET /auth/session` 每次调用轮换令牌（服务端仍只存哈希，见 Q25）。
      - Origin：登录要求 JSON Content-Type + `X-UPanel-Request: 1` + 与本地配置精确匹配的 Origin；
        `/api/v1` 下所有非 GET/HEAD 请求再过一层 originGuard；期望值只来自本机配置，
        不由 Host / X-Forwarded-* 推导；监听非回环时必须显式配置允许源，否则拒绝启动。
      - 限速：用户名 5 次/15 分钟、来源 IP 20 次/15 分钟，进程内固定窗口；触发后 429 +
        `VALIDATION_ERROR` + `details.retry_after_seconds`；窗口到期自动恢复，无永久锁定。
      - 账号枚举防护：不存在的账号与错误密码返回同一错误，且对不存在账号执行一次等价开销的
        哈希校验，抹平响应时间差。
      - 错误映射：错误密码 401 UNAUTHENTICATED；禁用账号 403 FORBIDDEN；缺失/不符 Origin 或 CSRF
        403 CSRF_FAILED；未登录 401；限速 429；请求体超限 413；其余 400/500 按契约。
      - 日志边界：访问日志只记方法/路径/状态/耗时/来源地址，请求体与 Cookie 不进日志
        （冒烟断言 web 日志不含密码与 CSRF 令牌）。

    已验证（宿主，证据 docs/evidence/P02/2026-10-20-p02.1/）：
      - gofmt -l cmd internal：空输出；go vet ./...：exit 0；go test ./...：13 个包全部 ok
        （新增 auth 包 26 个用例、api 包 14 组认证用例、httpx origin 5 个用例、config 4 个用例、
        upanelctl init-admin 4 个用例、迁移断言更新后仍全绿）
      - 单次 Argon2id 校验实测 98.7—131.2 ms（同机 5 次取最优的不同参数对比见 Q23）
      - tests/smoke/p02-1-auth-smoke.sh：PASS 38 / FAIL 0 —— 覆盖 init-admin（含重复初始化被拒）、
        登录四道入口检查、错误密码 401、Cookie 五条属性、session 轮换 CSRF、无 Cookie/伪造 Cookie
        401、缺 CSRF 退出 403、退出后同 Cookie 401、连续失败 429 + retry_after_seconds、
        日志不含密码与 CSRF 令牌
      - 测试过程中发现并修复两个真实缺陷：① PHC 解析把 `v=19` 整段当数字解析，导致本包
        自己生成的哈希永远校验失败（登录会 100% 报“哈希格式无效”）；② `Authenticate` 把 TEXT
        列扫进 time.Time，任何会话校验都会 500。两者都由新增用例在宿主上暴露，修复后复跑全绿

    已验证（真实 Ubuntu VM，证据同名目录 vm-2404-*.txt / vm-2204-*.txt）：
      - tests/integration/p02-1-acceptance.sh 在 **Ubuntu 24.04.5（内核 6.8.0-147）与
        Ubuntu 22.04.5（内核 5.15.0-198）** 两台 VM 上以非 root 用户（upanel, uid 1000）执行：
        PASS 58 / FAIL 0（两台一致，二进制 sha256 相同，脚本 sha256 相同）
      - VM 上验证到的点：production 模式在 127.0.0.1:9443 明文启动；Web 进程 uid=1000（非 root）；
        库文件属主为运行用户且权限 0600；init-admin 非终端两行输入必须一致、已有账号即拒绝；
        登录四道入口检查；Cookie 五条属性；CSRF 轮换；无 Cookie/伪造 Cookie 均 401；
        **进程重启后同一会话仍有效**（会话在库里而不在内存）；缺 CSRF 退出 403、退出后同 Cookie
        401；连续失败 429 + retry_after_seconds；库文件与 -wal 里**没有明文口令、明文会话令牌、
        明文 CSRF 令牌**，只有 Argon2id 哈希；Web 日志不含口令与令牌
      - 交付的 systemd 单元静态检查：`User=upanel-web`、无 `User=root`、无万能 shell
      - VM 复跑发现并修复第三个真实缺陷：SQLite 按 umask 建库得到 0644，同机其他用户可读到
        口令哈希与会话哈希。现在是 Open 时与迁移后都把库及 -wal/-shm 收到 0600（Q30），
        修复后两台 VM 复跑均为 FAIL 0

    未验证（明确 NOT RUN）：
      - systemd 单元**只做了静态检查**，没有在 VM 上用 systemd 真实托管 upanel-web：
        单元里的用户/组/StateDirectory 在 P02.3 与 agent socket 一起落地，届时才有安装级证据。
      - 会话的 30 分钟空闲与 12 小时绝对超时只在宿主用可控时钟的单元测试验证，
        VM 上没有做“等 30 分钟”的实跑（时间成本不可接受，且单元测试覆盖了同一逻辑）。
      - 浏览器侧：P02.1 没有前端登录页（STAGES.md 未要求），因此没有真实浏览器证据；
        `Secure` Cookie 在明文回环下被 Firefox/Safari 接受与否未知（DECISIONS Q28）。
      - TLS（P10）后的 Cookie/Origin 行为、再认证（P02.2）、TOTP（P14）、审计事件（P02.2）未实现。


### P02.2（2026-10-20）— admin/viewer 权限落地、账号管理、最后管理员保护、再认证、脱敏 DTO、审计事件起步

    子任务：P02.2（STAGES.md：admin/viewer、账号管理、最后管理员保护、再认证；实现字段级脱敏 DTO）
    状态：passed（宿主冒烟 81 项 + 两台 Ubuntu VM 各 99 项断言全通过；未验证项见「未验证」）
    新增文件：
      migrations/0003_audit_events.sql（audit_events：actor_id 无外键、actor_snapshot、action、
        target、request_id、job_id、outcome CHECK(success|failure)、redacted_details、created_at；
        三个索引；UPDATE/DELETE 由 BEFORE 触发器 RAISE(ABORT) 拒绝——追加式写死）
      internal/audit/audit.go（Event/Writer、Record 校验 action 与 outcome、按关键字黑名单把
        password/secret/token/csrf/hash 等键的明细替换为 [redacted]、时间按 RFC3339 UTC 入库）
      internal/audit/audit_test.go（4 个用例：脱敏、拒绝未知事件、账号删除后审计仍在、
        UPDATE/DELETE 被触发器拒绝）
      internal/auth/manage.go（ListUsers 带 limit/cursor 分页、GetUser、UpdateUser、
        SetPassword、DeleteUser、countEnabledAdmins；ErrVersionConflict / ErrLastAdmin /
        ErrSelfModification / ErrInvalidLimit / ErrInvalidCursor）
      internal/auth/manage_test.go（9 个用例：分页、version 冲突、自我提权/自我禁用拒绝、
        最后管理员保护、禁用撤会话、改密撤会话并换口令、口令策略、删除版本与自删、级联）
      internal/auth/reauth.go（Reauthenticate 复用登录限速计数、不存在的账号做等价耗时校验、
        成功后把 sessions.reauth_until 推到 now+5m；ReauthValid 用注入时钟判断）
      internal/auth/reauth_test.go（4 个用例：5 分钟窗口边界 4m 有效 / 6m 失效、错误口令不产生窗口、
        连续失败进入限速、账号不存在与已删除同样返回凭据无效）
      internal/api/users.go（adminUserDTO 脱敏 DTO、GET/POST/PATCH/DELETE /users、
        POST /users/:id/password、POST /auth/reauth、planLimit 分页入参、
        writeUserError/writeReauthError/requireAdmin/requireReauth、recordAudit）
      internal/api/users_test.go（7 组用例：admin/viewer 边界与再认证入口、列表脱敏与分页、
        创建与登录、再认证 + version 并发 + 自我提权、改密撤会话、删除与审计留痕、写操作必须带 CSRF）
      tests/smoke/p02-2-account-smoke.sh（宿主冒烟 81 项断言）
      tests/integration/p02-2-acceptance.sh（Linux VM 验收 99 项断言，非 root 运行）
      docs/evidence/P02/2026-10-20-p02.2/（host-build、go-test、check、host-smoke、
        host-smoke-web.log、vm-2404-env、vm-2404-acceptance、vm-2204-env、vm-2204-acceptance、
        vm-2404-p021-regression、vm-2204-p021-regression、vm-env-host-build、upanel-web.service、
        commands.log、result.md）
    修改文件：
      internal/auth/users.go（新增 InputError 与 invalidInput：创建账号的用户名/显示名/角色/口令
        校验失败一律包成“输入错误”，接口层映射 400 而不是 500）
      internal/api/router.go（/auth/reauth 与 /users 路由；整组 requireSession + requireAdmin，
        写操作再叠 requireCSRF + requireReauth）
      internal/api/server.go（Options 新增必需项 Audit；nil 时拒绝构造，避免“审计没接上却照常上线”）
      cmd/upanel-web/main.go（构造审计 Writer，失败单独报错退出）
      internal/api/testsupport_test.go（newTestServices 返回 (*auth.Service, *audit.Writer)，
        另提供 newTestServicesWithDB 供需要直接查表的用例使用）
      internal/api/{server,frontend,middleware}_test.go（跟随 Options.Audit 的签名变化）
      cmd/upanelctl/main_test.go（迁移条数断言改为按 migrations.FS 实际文件计算，
        不再写死“2 条”）
      api/openapi.yaml（/auth/reauth、/users、/users/{id}、/users/{id}/password 四个路径；
        AdminUser/UserListData/CreateUserRequest/UpdateUserRequest/SetPasswordRequest/
        DeleteUserRequest/ReauthRequest/ReauthData/UserId 组件；x-upanel-contract-status → P02.2）
      docs/DECISIONS.md（Q31—Q36：错误映射、再认证窗口、最后管理员不变式、审计策略与
        append-only、分页与游标、CSRF 缺陷修复）
      README.md（状态段、账号管理能力段、新增冒烟与验收脚本命令）
      docs/LOCAL_START.md（§5 增加 p02-2-account-smoke.sh 与 VM 验收脚本）
      docs/PROGRESS.md（本记录、§1、§3、§7、§8）
    未修改：技术栈、依赖清单（本子任务没有新增第三方依赖）、PLAN/CONTRACTS/STAGES 三份规格文档

    已实现：
      - 权限模型：admin 独占账号管理；viewer 访问 `/users` 的任何方法（含 `GET`）一律 403，
        `/auth/reauth` 也只对 admin 开放。未知身份按未登录处理（401），不做“看起来能点但请求失败”。
      - 账号管理：`GET /users`（脱敏 DTO + limit/cursor 分页）、`POST /users`（201）、
        `PATCH /users/:id`（display_name/role/disabled，指针字段区分“没提供”与“显式设空”）、
        `POST /users/:id/password`（直接设新口令并作废目标全部会话）、`DELETE /users/:id`
        （version 优先取请求体，其次取查询参数）。初始口令与新口令都不回显、不入日志、不写审计。
      - 脱敏 DTO：只返回 id/username/display_name/role/disabled/version/created_at/updated_at；
        password_hash、会话、CSRF、再认证时间一律不出现。冒烟脚本按字符串断言响应里不含
        `password`、`argon2`、`csrf`、`reauth`、`session_hash`。
      - 最后可用管理员保护：降级、禁用、删除都在同一事务内检查“还剩几个 enabled admin”；
        管理员不能改自己的角色或启用状态，也不能删除自己。接口层因此先撞 403（自我修改），
        422 分支是服务层不变式，由单测直接覆盖（Q33）。
      - 再认证：`POST /auth/reauth` 校验口令后写 `sessions.reauth_until = now + 5m`；
        写操作经 `requireReauth` 检查，过期返回 403 REAUTH_REQUIRED；口令错误 403 FORBIDDEN
        且不产生窗口；失败计数与登录共用限速，触发即 429 + retry_after_seconds。
      - 乐观并发：所有改动带 `version`，成功后 `version + 1`；不匹配 409 VERSION_CONFLICT；
        删除缺 version 400。
      - 审计：创建/修改/改密/删除各写一条 `audit_events`（actor_snapshot 形如
        `admin(admin)#1`、target `user:<id>`、request_id 取自中间件）；表只追加，
        UPDATE/DELETE 被触发器拒绝；审计写入失败只打 ERROR 日志，业务变更保持生效（Q34）。
      - 输入边界：严格 JSON（未知字段、尾部内容、空请求体各有明确报错）、用户名格式、
        角色枚举、口令策略；SQL 注入形状的用户名（`Admin'--`）在参数化查询下既进不去也不会误判。

    已验证（宿主，证据 docs/evidence/P02/2026-10-20-p02.2/）：
      - go build ./... / go vet ./... / gofmt -l cmd internal：全部干净（host-build.txt）
      - go test ./... -count=1：14 个包 ok（go-test.txt）。新增 api 34 个顶层用例、auth 44 个、audit 4 个
      - sh scripts/check.sh：PASS 12 / FAIL 0 / NOT RUN 0（check.txt；含前端 4 项与 openapi-lint）
      - tests/smoke/p02-2-account-smoke.sh（mock 模式真实二进制）：PASS 81 / FAIL 0（host-smoke.txt）
      - 冒烟覆盖：未登录 401、viewer 五种接口 403、创建/重复 422/SQL 注入形状 400/未知字段 400/
        未知角色 400/弱口令 400/缺 CSRF 403、列表脱敏与 limit-cursor 翻页（含游标不含明文用户名、
        坏游标 400）、未再认证 403 REAUTH_REQUIRED、错误口令再认证 403 且不获窗口、正确口令 200、
        totp 400、PATCH version 自增与过期 409、改自己角色/禁用自己 403、改密使旧会话 401、
        旧口令 401 新口令 200、删账号使会话 401、重复删除 404、审计四类动作齐全且无口令、
        UPDATE/DELETE 被拒、库与日志无明文口令
      - 冒烟脚本发现并修复一个真实缺陷：`POST /api/v1/users` 漏了 `requireCSRF`（Q36），
        修复前“管理员会话 + 正确 Origin 但不带 CSRF”能一路走到业务层。

    已验证（真实 Ubuntu VM，证据同名目录 vm-2404-*.txt / vm-2204-*.txt）：
      - tests/integration/p02-2-acceptance.sh 在 **Ubuntu 24.04.5（内核 6.8.0-147）与
        Ubuntu 22.04.5（内核 5.15.0-198）** 两台 VM 上以非 root 用户（upanel, uid 1000）执行：
        **两台均 PASS 99 / FAIL 0**，二进制与脚本 sha256 相同（vm-env-host-build.txt）
      - VM 上验证到的点：production 在 127.0.0.1:9443 启动且 Web 进程 uid=1000；账号创建、
        脱敏列表（不含 password/argon2/csrf/reauth/session_hash）、`limit=1` 翻页与坏游标 400；
        viewer 六条越权路径全 403；再认证窗口三段（缺窗口 403、错口令 403、对口令 200）；
        PATCH 版本自增与 409；自我修改三条 403；重置口令后目标会话 401 且旧口令失效；
        删除后会话 401、重复删除 404；审计四类动作齐全、`APPEND_ONLY=YES`（真实触发器拒绝
        UPDATE 与 DELETE）；库与 `-wal`/`-shm` 权限 0600 且属主是运行用户；库与日志无明文口令
      - P02.1 回归：`p02-1-acceptance.sh` 在两台 VM 上各 **PASS 58 / FAIL 0**，
        新增路由与审计表没有破坏原有认证链路

    未验证（明确 NOT RUN）：
      - 前端：本子任务没有账号管理页面（STAGES.md P02.2 未要求前端），因此没有浏览器证据；
        页面、加载/空/无权限状态与再认证弹窗要等对应的 UI 子任务。
      - systemd 托管：仍然只有静态检查，真实托管要等 P02.3 与 agent socket 一起落地。
      - 再认证窗口的时间语义（5 分钟有效期、跨重启、跨会话）只在宿主的可控时钟单测里验证；
        VM 上等 5 分钟不现实，没有实跑到期。
      - 多管理员并发写：两个 admin 同时改同一个账号的 409 只有单测级的顺序模拟，
        没有在 VM 上做真实并发压测。
      - 审计与业务的事务一致性：当前审计写入失败不回滚业务（Q34），两者进同一事务是 P02.4。
      - 日志脱敏只覆盖了“口令/令牌不进日志”的断言；P02.4 才做系统性秘密脱敏与
        agent 独立库、秘密加密存储。

### P02.3（2026-10-20）— 代理 Unix socket、SO_PEERCRED UID 边界、固定登记表、结构化只读查询、命令执行器

    子任务：P02.3（STAGES.md：代理 Unix socket、UID 校验、固定动作注册、结构化只读查询、
            命令执行器超时/输出上限；CONTRACTS §15 私有协议；PLAN §4 执行器边界）
    状态：passed（两台 Ubuntu VM 各 84 项断言 + VM 内核原生 28/9 个用例；未验证项见「未验证」）
    新增文件：
      internal/syscmd/syscmd.go（Spec/Limits/Result、Run：exec.CommandContext + 固定环境
        PATH/LANG/LC_ALL、Stdin=nil、输出按字节上限截断并继续 drain、退出码是结果而不是“成功”、
        ErrEmptyProgram/ErrProgramNotAbsolute/ErrProgramInvalid/ErrTimeout/ErrOutputLimit、
        validateProgram 字符白名单）
      internal/syscmd/procattr_unix.go（Setpgid + 超时按进程组 SIGKILL，2s WaitDelay 兜底）
      internal/syscmd/procattr_other.go（非 unix 平台的等价空实现，不做假动作）
      internal/syscmd/syscmd_test.go（辅助进程通过 -test.run 复用自身：退出码、参数逐字、
        固定环境、输出截断、300ms 超时、父上下文取消、Limits 默认值）
      internal/agent/registry.go（固定登记表：Action/Query、名字形态 ^[a-z][a-z0-9]*(\.[a-z][a-z0-9_]*)+$、
        params 上限 64 KiB、DecodeParams 严格解析；NewProductionRegistry 返回空表）
      internal/agent/registry_test.go
      internal/agent/peer.go（PeerPolicy/ResolvePeerPolicy 用 os/user 解析 uid/gid 并拒绝 0；
        rootOnlyPrefixes 为空；gate 中间件在进入任何 handler 前判 UID）
      internal/agent/peers_test.go（gate 对 root / 同组其他 UID / 未知身份 / 未知路径的判定）
      internal/agent/peercred_linux.go（SO_PEERCRED via SyscallConn；verifySocketOwnership 0660 root:gid；
        verifySocketDir：目录必须 root 属主、组与其他位不可写、可被 socket 组或其他人穿越）
      internal/agent/peercred_other.go（非 Linux 一律拒绝，不假装有对端身份）
      internal/agent/listener.go（peerListener 在 Accept 时读 UID 并注入 request context）
      internal/agent/queries.go（POST /internal/v1/queries：405 + Allow、415、413、400 严格解析、
        未登记 query 拒绝、内部错误只进日志）
      internal/agent/queries_test.go（测试专用登记表 test.echo，覆盖四类拒绝与成功路径）
      internal/agentclient/client.go（http.Client over unix、path.IsAbs 校验、Capabilities/Query、
        *Error（业务拒绝）与 *UnavailableError（依赖不可用）两类错误、8 MiB 响应上限）
      internal/agentclient/client_test.go（httptest + 注入 dialer，覆盖超时与坏信封）
      deploy/systemd/upanel-agent.service（User=root、RuntimeDirectory=upanel + 0755、
        ExecStartPre 校验配置、UMask=0077、NoNewPrivileges/ProtectHome/ProtectKernel*/Restrict*
        等硬化项，并注释说明为何**不**开 PrivateMounts/PrivateTmp/ProtectSystem=strict）
      tests/integration/p02-3-agent-acceptance.sh（Linux VM 验收 84 项断言，必须以 root 运行）
      docs/evidence/P02/2026-10-20-p02.3/（host-build、go-vet、gofmt、go-test、check、
        cross-build-sha256、两台 VM 的 p023-acceptance / agent-test / syscmd-test /
        systemd-analyze-verify / journal / env、两台 VM 的 p022-regression、commands.log、result.md）
    修改文件：
      internal/agent/server.go（Options 新增 Peer 与 Registry；路由 /internal/v1/capabilities 与
        /internal/v1/queries；Handler 包 gate；ConnContext 注入对端身份；Serve 增加
        verifySocketDir + peerListener + secureSocket 回读校验；capabilities 增加
        supported_actions/supported_queries；启动日志记录 allowed_web_user/uid/socket_gid/
        registered_actions/registered_queries）
      internal/agent/server_test.go（重写：以注入 withPeerInfo 模拟不同 UID；
        /internal/v1/preflights 取代已实现的 /queries 进入 404 清单）
      internal/config/config.go + config_test.go（Agent 新增 web_user/socket_group 与
        validateAccountName：拒绝空、非法字符、纯数字 UID）
      cmd/upanel-agent/main.go（启动前 ResolvePeerPolicy，失败即拒绝启动并说明原因）
      deploy/upanel.example.json（agent.web_user / agent.socket_group）
      .gitignore（.tmp-linux-bin/：VM 交叉编译产物）
      docs/DECISIONS.md（Q37—Q41）、README.md、docs/LOCAL_START.md、docs/PROGRESS.md（本记录）
    未修改：技术栈与依赖清单（本子任务零新增第三方依赖）、PLAN/CONTRACTS/STAGES、
             Web 公开 API 与 api/openapi.yaml（私有协议不进公开契约）

    已实现：
      - 通道：代理只监听 Unix socket（无任何 TCP），socket root:upanel-web 0660，
        /run/upanel 0755 root:root（可穿越不可写），启动时自检目录与 socket 权限并回读确认。
      - 身份：一条连接一个身份，UID 来自内核 SO_PEERCRED。普通接口只允许配置的 web_user；
        root 与任何其他 UID 一律 403 FORBIDDEN，且响应不回显允许的 UID；root 的专用路由表
        当前为空（P03 登记 schedule trigger 后才非空）。无身份信息时 403，不“默认放行”。
      - 登记表：动作与查询都是固定、具名、可枚举的；名字形态与 params 大小在入口校验；
        生产表为空，capabilities 如实报告 []/[]（Q37）。不存在“转发任意 query”或通用 command 通道。
      - 查询通道：POST /internal/v1/queries 只认登记过的名字与字段；未知字段、非 JSON、
        空 body、超长 body、GET 各有明确拒绝（400/413/405 + Allow）。
      - 执行器边界：绝对程序路径 + 字符白名单（拒绝命令字符串）、参数逐项传递不经 shell、
        固定环境白名单、超时后按进程组 SIGKILL、stdout/stderr 字节上限截断、
        非零退出码作为 Result 返回（不是“成功”）。
      - systemd：代理单元以 root 运行、RuntimeDirectory 0755、ExecStartPre 用 upanelctl
        校验配置；硬化项按“不破坏必要能力”取舍并写明原因。

    已验证（宿主，证据 docs/evidence/P02/2026-10-20-p02.3/）：
      - go build ./... / go vet ./... / gofmt -l .：全部干净（host-build.txt / go-vet.txt / gofmt.txt 0 行）
      - go test ./... -count=1：16 个包 ok（含新增 agent / agentclient / syscmd）
      - sh scripts/check.sh：PASS 12 / FAIL 0 / NOT RUN 0（含 go-build-linux / go-vet-linux
        交叉检查与 redocly lint）

    已验证（真实 Ubuntu VM，两台一致）：
      - tests/integration/p02-3-agent-acceptance.sh 在 Ubuntu 24.04.5（6.8.0-147）与
        Ubuntu 22.04.5（5.15.0-198）上以 root 执行：**两台均 PASS 84 / FAIL 0**；
        二进制与测试二进制的 sha256 与宿主交叉编译产物逐字一致（cross-build-sha256.txt）。
      - 覆盖：单元安装与 systemd 真实启动、socket 属主/组/模式、目录 0755 且其他人不可写、
        代理无 TCP 监听、capabilities 只对 Web 账号 200（root 403、同组其他 UID 403 且不泄露
        允许 UID、nobody 连接失败 000）、未登记 query/命令形状字段/未知字段/非 JSON/空 body/
        超长 body/GET 的拒绝、未知路径的 404（Web）与 403（root）、启动期四类拒绝
        （账号不存在、数字 UID、非 root、socket 路径被普通文件占据且该文件保留）、
        两台 VM 上原生执行 syscmd.test（9 PASS + 1 SKIP）与 agent.test（28 PASS）、
        停止后 socket 清理与服务 inactive、journal 里有监听与拒绝记录且不含 password/secret。
      - P02.2 回归：p02-2-acceptance.sh 在两台 VM 上各 `结果: PASS=99 FAIL=0`，
        新增 web_user/socket_group 配置与 server.go 重写没有破坏账号、再认证、审计链路。

    未验证（明确 NOT RUN）：
      - 真实系统数据采集：生产登记表为空，capabilities 报 []/[]；磁盘/服务/进程等只读查询
        随 P04/P05 逐项登记并单独验收。
      - 写操作路径（/preflights、/operations、/schedules/:id/trigger）：按 PLAN 属 P03/P11，
        当前返回 404，未实现也未测试。
      - 审计与业务同事务、日志系统性脱敏、代理独立数据库与秘密加密存储：P02.4。
      - 并发：同一 socket 上多连接并发、长时间运行的 fd/goroutine 泄漏没有压测。
      - 非 Linux 平台：peercred_other.go 一律拒绝，未在 macOS 上跑过验收（也不打算把代理
        跑在非 Linux 上）。
      - 22.04 上 systemd-analyze 对系统自带 snapd.service 的告警不属于本仓库，未修复，
        已在脚本里限定为“只判本单元自身的告警”，原文留在日志里。

### P02.4（2026-10-20）— 审计事务化、日志秘密脱敏、代理独立数据库与秘密加密存储

交付（代码）：

- 审计跟随业务事务：`internal/audit/audit.go` 的 `Record` 改为“`ctx` 里有 `store.TxFromContext`
  的事务就写同一事务，没有就独立提交”。`internal/api/users.go` 新增 `recordAuditInTx`（成功审计与
  业务同一事务，写失败即回滚）、`recordDenied`（`outcome=failure`，details 只有
  `stage`/`error_code`/方法/路径）、`failUserAction`（事务内失败 → 拒绝或 500，两种都留痕）、
  `auditRoute`/`reject`（中间件层拒绝也带 action 名，如 CSRF/admin/reauth）。
- 日志秘密脱敏：`internal/logging/redact.go` 补齐值形状匹配（`password=…`、`password="…"`、
  `token: …`），脱敏是记录器自身的属性而不是调用点的自觉；元数据统一用 `key_path`/`key_version`/
  `key_fingerprint`/`ref`/`purpose`/`state` 这类不含敏感片段的键名（DECISIONS Q46）。
- 代理独立数据库：`internal/agentstore`（`store.go` 打开与自校验、`perms_linux.go`/`perms_other.go`
  的 root-only 目录/文件判定、`migrations/agentdb/0001_secrets.sql` 与 `agentdb.FS`）。
  目录 `0700 root`、库文件 `0600 root:root`，打开前校验目录、打开后校验文件，不合规即拒绝启动。
  本阶段只建 `secrets` 一张表；§14.2 其余表随定义它们的阶段落地（DECISIONS Q43）。
- 秘密加密存储：`internal/agentstore/keyring.go`（AES-256-GCM、每条独立 12 字节 nonce、
  AAD `"upanel/agent/secret/v1|"+ref+"|"+purpose`、密钥文件两行文本、`-init-key` 用
  `O_CREATE|O_EXCL` 永不覆盖、输出 SHA-256 前 8 字节指纹）、`purposes.go`（用途闭集，
  `totp_seed` 仅内部）、`secrets.go`（`Put`/`Resolve`/`Reference`/`DestroyExpired`，
  时间列用 INTEGER Unix 毫秒）。
- 秘密接口：`internal/agent/secrets.go` 只提供 `POST /internal/v1/secrets`（无读取明文路由），
  启动即扫一次过期秘密 + 每 30 s 扫描；`internal/agentclient` 增加 `PutSecret`。
- 配置与部署：`agent.database_path`、`agent.secret_key_path`（Linux 默认
  `/var/lib/upanel-agent/agent.db`、`/etc/upanel/agent.key`，非 Linux 无默认值）；
  `deploy/systemd/upanel-agent.service` 增加 `StateDirectory=upanel-agent`（0700）与
  `-init-key` 首次部署说明；`deploy/upanel.example.json` 同步。

接口/数据库变更：

- 新增内部接口 `POST /internal/v1/secrets`（用途闭集、值大小上限 4/16/32 KiB、返回
  `secret_ref`/`purpose`/`state`/`expires_at`，201）。无读取路由；`GET` 返回 405 + `Allow: POST`。
- 新增代理库 `secrets` 表（`id`/`purpose`/`ciphertext`/`nonce`/`key_version`/`state`/`reference`/
  `created_at`/`expires_at`/`referenced_at`/`destroyed_at` + 一组 CHECK 约束与两个索引）。
- Web 库结构未变；`audit_events` 的写入路径变为“同事务或独立提交”两种。
- 公开 API（`/api/v1`）未新增路径，`api/openapi.yaml` 无需变更。

已验证（宿主，证据 `docs/evidence/P02/2026-10-20-p02.4/`）：

- `go build ./...`、`go vet ./...`、`gofmt -l .` 全部干净；`go test ./...` 全部包 ok；
  `sh scripts/check.sh` → PASS 12 / FAIL 0 / NOT RUN 0。
- 新增单元/接口测试：密钥文件生成-加载-拒覆盖-格式错误、Seal/Open 往返与 nonce 唯一、
  错误 AAD/篡改/截断/错密钥全部解密失败、Put/Resolve/Reference/DestroyExpired、
  `TestCiphertextNeverLeaksPlaintextToDisk`（在 db/wal/shm 里搜明文标记）、
  HTTP 201/405/404/400/413/415/403 各拒绝路径、`TestSweepExpiredSecrets`（短 TTL 真实执行）、
  日志脱敏双向回归（元数据可见 / `secret_ref` 必被打码）。

已验证（真实 Ubuntu VM，两台一致）：

- `tests/integration/p02-4-agent-secrets-acceptance.sh` 在 24.04.5（6.8.0-147）与 22.04.5
  （5.15.0-198）上以 root 执行：**两台均 PASS 87 / FAIL 0**（日志见 `vm-<name>-p024-acceptance.txt`）。
- 覆盖：无密钥启动被拒并提示 `-init-key`、启动不隐式生成密钥、`-init-key` 输出 16 位指纹、
  文件两行/头正确/内容 32 字节/`root:root 0600`、再次生成被拒且密钥未变；目录 0700、
  `agent.db` 与 WAL/SHM 0600、`schema_version=1`、迁移名 `0001_secrets`、`secrets` 表存在、
  `upanel-web` 读不到库/密钥/列不出目录；POST 201 + `sec_` 引用 + `temporary` + 540—660 s 到期、
  响应无明文、带 reference 直接持久；GET 405 + Allow、`/secrets/<ref>` 与 `/secrets/<ref>/value`
  404、未知与内部用途 400、空值/超限/未知字段 400、root 与同组其他 UID 403、nobody 000；
  落盘两行状态正确、密文 ≥16 字节、nonce 长 12 且互不相同、`key_version=1`、
  库里搜不到两段明文标记；重启后行仍在；journal 无明文且含 `key_fingerprint`/schema/库路径。
- VM 内核原生执行交叉编译的测试二进制：`agentstore.test` 32 个用例 / 0 FAIL、
  `agent.test` 45 个用例 / 0 FAIL（`vm-<name>-unit-tests.txt`）。
- 回归：`p02-1` 各 58 / 0、`p02-2` 各 103 / 0、`p02-3` 各 84 / 0（两台一致）。

未验证（明确 NOT RUN）：

- 服务进程上的 10 分钟挂钟到期销毁（到期逻辑已在短 TTL 下用真实 SQLite + 真实 AEAD 执行）。
- 密钥轮换（`key_version` 只落列与校验，轮换流程未实现，契约未要求）。
- 真实业务写入秘密的调用方（SMB/备份/证书随 P08/P12/P10 落地）。
- `totp_seed` 写入路径（P14 注册流程，本阶段明确拒绝上传）。
- 长时间运行下的大日志量脱敏压力。

环境事故（如实记录）：24.04 验收 VM 在回归途中内核 panic（`Attempted to kill init!`
exitcode=0x8b，systemd/PID 1 段错误），重启 VM、重传二进制（sha256 复核一致）后整套重跑；
最终结果取自重启后的干净运行。属测试环境问题，不是产品行为。

### P03.1（2026-10-20）— 预检由代理生成、120 秒期限、绑定与确认短语、公开 DTO

交付（代码）：

- 代理侧计划生成：`internal/agent/preflight.go`（`POST /internal/v1/preflights` 处理器：校验
  `actor_context`、调用动作规划器、核对计划自洽性、落库、返回一次性短语）、`internal/agent/registry.go`
  （`Action.Plan` 规划器 + `ActionPolicy{RequiresReauth, RequiresPhrase, PhraseVerb, Cancellable}`，
  短语动词 `^[A-Z][A-Z0-9]{1,15}$` 在登记时校验）、`internal/agent/server.go`（路由与
  `Options.Plans` 必填——不允许启动一个签不出预检的代理）。
- 计划存储：`internal/agentstore/plans.go` + `migrations/agentdb/0002_preflight_plans.sql`。
  `phrase_hash` 只存哈希；一次性由 `UPDATE ... WHERE used_at = 0 AND expires_at > ?` 保证；
  过期未用计划由扫描器删除；`listEmpty()` 修掉了“`[]byte("[]")` 长度非 0”导致
  “拒绝却没有阻塞项”的计划被放行的真实缺陷。
- Web 侧编排：`internal/operations/service.go`（转发 + 算 `params_hash`（canonical JSON，
  `json.Number` 解码后重排键）+ 落库绑定 + 只做合理性校验，不自己判断 allowed）、
  `internal/api/preflights.go`（`POST /api/v1/operations/preflight`，仅 admin + CSRF，
  错误映射 400/403/503/500，成功写审计 `operation.preflight`，日志与审计不含短语）、
  `migrations/0004_preflights.sql`（`preflights` 表，含 `agent_ref`/`state_version`/
  `consumed_job_id`，`session_hash` CHECK 长度 64）。
- 代理客户端：`internal/agentclient` 新增 `CreatePreflight`；同时修掉“只把 200 当成功、
  而代理所有创建接口返回 201”的真实缺陷（现在接受 2xx），并补真实 client ↔ 真实 agent 的
  端到端用例防回归。
- 验收用动作表：`internal/agent/acceptance.go` 的 `acceptance.probe`（要求再认证 + 短语 `PROBE`）
  与 `acceptance.ping`（都不要求，作对照），只在 `-acceptance-actions` 打开时登记，生产表仍为空。
- 契约：`api/openapi.yaml` 新增 `POST /api/v1/operations/preflight` 与 5 个 schema
  （`PreflightRequest`/`PreflightTarget`/`PreflightNotice`/`PreflightImpact`/`PreflightData`），
  `version: 0.0.0-p03.1`、`stage: P03.1`，并把该路径加入 `implemented_paths`。

接口/数据库变更：

- 新增公开接口 `POST /api/v1/operations/preflight`（200，仅 admin，需 CSRF）。响应字段与
  CONTRACTS §4.1 逐项对齐；`confirmation_phrase` 不要求短语时为 `null` 而不是省略；
  **不返回** `state_version` 与 `agent_ref`（内部绑定信息）。
- 新增内部接口 `POST /internal/v1/preflights`（201；只认 `upanel-web` UID；无 `GET` → 405；
  缺规划器 → 501；动作未登记 → 400）。
- 新增 Web 库 `preflights` 表（迁移 0004）；新增代理库 `preflight_plans` 表（代理迁移 0002，
  代理 schema_version 由 1 → 2，`p02-4` 验收脚本的版本断言同步改为 2 并注明“当前应有的版本”）。

已验证（宿主，证据 `docs/evidence/P03/2026-10-20-p03.1/`）：

- `go build ./...`、`go vet ./...`、`gofmt -l .` 干净；`go test ./...` 全部包 ok；
  `sh scripts/check.sh` → PASS 12 / FAIL 0 / NOT RUN 0；`redocly lint api/openapi.yaml` valid
  （11 warnings，与改动前同类同量：9 个未引用组件 + 2 个 health 端点无 4xx）。
- 新增单元/接口测试：计划往返与 DB CHECK、一次性触发器、`DeleteExpired` 只删“未用且过期”、
  11 种不一致计划的拒绝表、规范化哈希对空白不敏感/对内容敏感、短语字母表熵、请求形状 9 条拒绝、
  `RequiresPhrase` 对照、两端 `expires_at` 一致（含 `now.Truncate(time.Second)` 修复）、
  审计 `target=preflight:<action>` 且 details 不含短语、Web 库无短语明文（`LIKE '%PROBE ABCD23%'` = 0）。

已验证（真实 Ubuntu VM，两台一致）：

- `tests/integration/p03-1-preflight-acceptance.sh` 以 root 在 24.04.5（6.8.0-147）与 22.04.5
  （5.15.0-198）执行：**两台均 通过 93 / 失败 0**（`vm-<name>-p031-acceptance.txt`）。
- 覆盖：socket `root:upanel-web 0660` 与目录 `0755 root:root`；Web 子进程 uid = `upanel-web`
  且 root 名下无 Web 进程；预检 200 与完整字段（`allowed`/`requires_reauth`/`cancellable`/
  `blockers`/`locks`/`warnings`/`impacts`）；响应不含 `state_version` 与 `agent_ref`；
  `expires_at` 距今 115—121 s 且代理库 `expires_at - created_at = 120000` ms、Web 库与响应字符串相同；
  两侧 `user_id`/`session_hash`/`params_hash`/`state_version` 一致；带空白参数与紧凑参数同哈希、
  不同内容不同哈希；`acceptance.ping` 无短语、可取消；短语匹配 `^PROBE [A-HJ-NP-Z2-9]{6}$`、
  代理库 `phrase_hash = SHA-256(短语)`、在 `agent.db`/`-wal`/`-shm` 搜明文 0 次、Web 库唯一含
  `phrase` 字样的列是 `requires_phrase`、日志与 journal 无短语；未登录 401 / viewer 403 /
  缺 CSRF 403 且两侧都不落库；非 JSON、坏 JSON、数组参数、非法动作名、未知字段 → 400；
  `GET` → 405 + `Allow: POST`；代理停止 → 503 `AGENT_UNAVAILABLE` 且不留半份记录。
- VM 内核原生执行交叉编译测试二进制（2404，root）：`agentstore.test`、`agent.test`
  （预检/路由/必填选项子集）、`agentclient.test`、`operations.test`、`syscmd.test` 全部 PASS
  （`vm-2404-native-tests.txt`）。
- 回归：两台 VM 上 `p02-1` 各 58 / 0、`p02-2` 各 103 / 0、`p02-3` 各 84 / 0、`p02-4` 各 87 / 0。

未验证（明确 NOT RUN）：

- 预检 → 任务提交的端到端（`POST /jobs` 消费预检、`Idempotency-Key`、`operation_id`）：P03.2。
- 预检到时后的拒绝、执行前状态版本复核、锁排队的真实行为：需要 P03.2 的执行路径。
- 再认证窗口在**提交**阶段的强制（`REAUTH_REQUIRED`）：P03.1 只按契约回显 `requires_reauth`，
  不在预检阶段拦截；强制点在 P03.2。
- 前端预检界面与 TS DTO：STAGES.md P03.1 未要求前端，页面随真实动作的 UI 阶段落地。
- 真实破坏性动作（格式化/分区/删目录）：STAGES.md 明令 P03 通过前禁止写磁盘代码，
  本轮验收动作不改系统（影响声明 `none`）。

环境与执行注意（如实记录）：`p02-1`/`p02-2` 必须以非 root 的 `upanel` 用户直接运行
（脚本自己 sudo 起服务，并断言“Web 进程不是 root”）；本轮先用 sudo 跑导致各 2 项失败，
且残留的 root 属主工作目录又让随后的非 root 运行大面积失败；清理 `/var/tmp/upanel-accept-*`
后按正确角色重跑即全绿。两次失败都没有通过改断言或放宽条件来“解决”。

### P03.2（2026-10-20）— 持久 jobs、幂等键、operation_id、代理执行记录、资源锁、排队与取消

交付（代码）：

- 代理侧执行面：`internal/agent/operations.go`（`POST /internal/v1/operations`、
  `GET /internal/v1/operations/{id}`、`GET .../events`、`POST .../cancel`）、
  `internal/agent/runner.go`（接受后在后台执行，用 `context.Background` 而不是请求 ctx
  ——“浏览器关闭任务继续”就靠这里；执行前重算计划并复核目标身份与状态版本）、
  `internal/agent/locks.go`（资源锁：锁名先排序再获取，靠顺序避免死锁；30 秒拿不到
  返回 `RESOURCE_BUSY`，不无限占 worker）、`migrations/agentdb/0003_operations.sql`
  （`operations` + `operation_events` 追加式触发）。代理 schema_version 2 → 3。
- 去重锚点在**代理**：`operation_id` 由 Web 生成，代理侧 `UNIQUE(operation_id)` +
  `request_hash` 存证；同一个 `operation_id` 重复提交只回已有记录（`duplicate=true`），
  不会二次执行。预检的“一次性”消费点在提交阶段，且**重复提交先命中 operation 去重，
  再消费预检**——崩溃窗口里重放不会把预检烧掉。
- Web 侧编排 `internal/operations/jobs.go`：`SubmitJob` / `GetJob` / `ListJobs` /
  `CancelJob`、`syncFromAgent`、`reconcile`、`markJobFailed`。
- 提交顺序（先落库，再提交）：校验 → 造 `job_`/`op_` → 事务内（幂等表查询并比对
  `body_hash` → 读预检绑定 → `UPDATE preflights SET consumed_job_id=? WHERE id=?
  AND consumed_job_id IS NULL` 条件消费 → INSERT job（`queued` + `needs_sync=1`）→
  INSERT `idempotency_keys` → 写 status 事件）→ 事务外 `agent.CreateOperation`。
- 三种失败严格分开，不互相冒充：①代理业务拒绝 → job `failed` + 契约错误码，**不重试**；
  ②代理不可达 → job 留 `queued` + `needs_sync=1`，之后按 `operation_id` 对账，**不重发**；
  ③对账回 `NOT_FOUND` → `failed`/`unconfirmed` + `AGENT_UNAVAILABLE`，不做“再试一次
  也许就好了”。
- 状态诚实：`statusForStage` 把代理 `accepted` 映射成公开 `queued`；DB 触发器
  `jobs_terminal_is_final` 挡住终态回退；`progress_percent` 为 `NULL` 就是“不知道”，
  不编进度条；`cancellation_requested` ≠ `canceled`。
- `syncFromAgent` 只在**状态真的变了**时写事件（更新前先读旧 `status`/`step`），
  否则对账轮询会把 `job_events` 灌成回声。
- `Service.Agent` 接口从 1 个方法扩到 4 个（`CreatePreflight`/`CreateOperation`/
  `GetOperation`/`CancelOperation`），注释写明“每个方法都必须有真实实现”。第一版
  `jobs.go` 里出现过 `cancellableOf` 返回常量的桩和占位 owner，按“生产代码不得使用
  空实现”整份删掉重写，并补 `preflights.cancellable` 列取代猜值。
- 公开 API `internal/api/jobs.go`：`POST /api/v1/jobs`（202 + `job_id`/`status`/
  `events_url`）、`GET /api/v1/jobs`（limit 1—200）、`GET /api/v1/jobs/{id}`、
  `POST /api/v1/jobs/{id}/cancel`（202）。`Idempotency-Key` 必填（缺失 400
  `VALIDATION_ERROR`，长度/字符非法 400）；同键重放返回同一个 `job_id` 且只写一次审计。
- 路由与权限：jobs 组挂在 `audit.ActionJobManage` 下，组内全部 `requireSession`，
  写操作叠 `requireAdmin` + `requireCSRF`；viewer 只能列/看自己的 job，别人的 job
  一律 404（不区分“不存在”与“不是你的”）。
- 审计新增 `job.submit`/`job.cancel`/`job.manage`。
- 契约：`api/openapi.yaml` 新增 4 条路径与 6 个 schema（`JobSubmitRequest`/
  `JobSubmitData`/`JobStatus`/`Job`/`JobData`/`JobListData`）+ `JobId` 路径参数
  （`^job_[0-9a-f]{32}$`），`version: 0.0.0-p03.2`、`stage: P03.2`，`implemented_paths` 同步。
- 验收脚本 `tests/integration/p03-2-jobs-acceptance.sh`（root，端口 19444，
  工作目录 `/var/tmp/upanel-accept-p03-2`；脚本内 `== 0` 环境 … `== 11` 终态/日志卫生
  共 12 个实质段落，`== 12` 为汇总；`sh -n` 通过、可执行位已设）。
- 被新路由打旧的断言同步修正（不让旧脚本误报）：`p01-1` 的未知路径探针改
  `/internal/v1/streams`；`p02-3` 的“操作接口 404”改成 `GET /internal/v1/streams` → 404
  加 `POST /internal/v1/operations` 未知字段 → 400；`p02-4` 的代理 `schema-version`
  断言 2 → 3 并注明“当前应有的版本”。

接口/数据库变更：

- 新增公开接口 4 条（见上），均为异步语义 202（除 `GET`）；新增内部接口
  `POST/GET /internal/v1/operations[...]`（只认 `upanel-web` UID）。
- Web 库：迁移 `0005_jobs.sql` 追加 `preflights.cancellable`
  （`NOT NULL DEFAULT 0`、`CHECK (cancellable IN (0,1))`）。默认 0 是刻意的保守值：
  没声明可取消的动作按不可取消处理，前端不显示取消按钮。
- 代理库：迁移 `0003_operations.sql` 新增 `operations`/`operation_events`，
  `schema_version` 2 → 3。

已验证（宿主，本轮实际执行）：

- `sh scripts/check.sh` → **PASS 12 / FAIL 0 / NOT RUN 0**
  （`go-build`/`go-vet`/`go-test`/`go-build-linux`/`go-vet-linux`/`gofmt`/`node-version`/
  `web-typecheck`/`web-lint`/`web-test`/`web-build`/`openapi-lint` 全 PASS）。
- `go test -count=1 ./internal/operations/ ./internal/api/` → ok，
  **84 个用例 PASS / 0 FAIL**（`operations` 18 个测试函数、`api` 14 个测试函数）。
- `go test -count=1 ./internal/agent/` → **76 个用例 PASS / 0 FAIL**。
- `go vet ./...` 退出码 0；`gofmt -l .` 无输出。
- `redocly lint api/openapi.yaml` → valid，10 warnings（8 个未引用组件 + 2 个 health
  端点无 4xx，与改动前同类同量，未新增）。
- 测试打的是产品路径不是桩：真实 SQLite 文件 + 真实迁移 + 真实路由。覆盖：十次同键
  提交只产生 1 个 job / 1 次代理调用 / 1 条幂等记录；同键不同 body → 409；不同键同一
  预检 → 409 `PREFLIGHT_STALE`；预检过期 / 会话变更 / 他人预检 / 被阻塞 / 再认证超窗
  各自拒绝；代理不可达 → job 留 `queued`，对账用**同一个** `operation_id` 收敛到
  succeeded 且 `CreateOperation` 只被调 1 次；代理拒单 → `failed` + `error_json` 里的
  契约码；代理回 `NOT_FOUND` → `failed`/`unconfirmed` 且不重试；终态 job 不再碰代理；
  不可取消动作的取消请求 409 且不调代理；可取消动作取消 → `canceled`；viewer 边界、
  未知 job 404、终态触发器拒绝回退。

未验证（明确 NOT RUN）：

- **两台 Ubuntu VM 上的 `tests/integration/p03-2-jobs-acceptance.sh` 本轮没有执行。**
  因此下列验收项一条真实系统证据都还没有：真实 socket 权限下的提交、Web 进程被杀后
  任务继续、Web 重启后按 `operation_id` 找到原 operation、代理 `kill -9` 后操作变
  `interrupted`、真实 systemd 环境里 `operation_events` 只追加、真实库里的幂等 TTL。
- `p01-1`/`p02-3`/`p02-4` 的回归本轮**未重跑**（本轮改了这三条脚本的断言，改动后是否
  仍全绿未在 VM 上验证）。
- VM 内核上的原生测试二进制（`agent.test`/`operations.test`/`agentstore.test` 等）
  本轮**未重跑**。
- SSE/`events_url`：目前只返回了地址，流式推送本身属 P03.3，未实现。P03.3 之前
  `events_url` 指向的路径不可用。
- 过期未消费的 `preflights` 行没有清理作业，表会缓慢增长。
- `GET /api/v1/jobs` 只读库、不逐行对账；对账由 `GetJob`/`CancelJob` 与后续调度器触发。

已知限制与判断（同步写进 DECISIONS Q58—Q63；验收阶段又补了 Q64—Q66）：

- 不可取消动作收到取消请求用 HTTP **409 + `VALIDATION_ERROR`**：契约 24 码里没有
  “不可取消”这一项，409 表达“请求与资源当前状态冲突”最准确，不新造错误码。
- 代理侧资源锁是**进程内**的，只覆盖同一个代理里的并发；外部 apt/systemctl/配置
  编辑器不认这把锁，真实动作从 P04 起还要查系统锁与配置哈希。机制先在位，边界写明。

下一步：**补齐 P03.2 自身的验收**——在两台 VM 上跑 `p03-2-jobs-acceptance.sh` 与
`p01-1`/`p02-3`/`p02-4` 回归、重跑 VM 内核原生测试二进制，把证据落到
`docs/evidence/P03/<date>-p03.2/`。**在这之前不得进入 P03.3**（前置验收未通过不跳阶段）。

环境事实（如实记录，非产品行为）：本轮结束时 24.04 验收 VM（`127.0.0.1:2222`）又进了
`Attempted to kill init! exitcode=0x8b` 的内核 panic（`H:\Code\vms\upanel-2404\console.log`
尾部可见 `end Kernel panic - not syncing`），SSH 停在 banner exchange 超时；22.04 VM
（`127.0.0.1:2223`）正常，`upanel@127.0.0.1` 可登录、`5.15.0-198-generic`、`Ubuntu 22.04.5 LTS`。
这与 P02.4 期间那次是同一现象，按测试环境问题处理：先重启 2404，再跑 P03.2 验收；
重启后 `/tmp` 会清空，二进制与脚本必须重传并核对 sha256。

### P03.2 验收结果（同上日，验收回填）

**结论：passed。** 代码 + 两台 VM 验收均完成，证据在 `docs/evidence/P03/2026-10-20-p03.2/`。

| 检查 | 24.04.5 / 6.8.0-147 | 22.04.5 / 5.15.0-198 |
|---|---|---|
| `p03-2-jobs-acceptance.sh` | 通过 101 / 失败 0 | 通过 101 / 失败 0 |
| `p01-1` | PASS 29 / FAIL 0 | PASS 29 / FAIL 0 |
| `p01-2` | PASS 34 / FAIL 0 | PASS 34 / FAIL 0 |
| `p01-4` | 通过 114 / 失败 0 | 通过 114 / 失败 0 |
| `p02-1` | PASS=58 / FAIL=0 | PASS=58 / FAIL=0 |
| `p02-2` | PASS=103 / FAIL=0 | PASS=103 / FAIL=0 |
| `p02-3` | PASS 85 / FAIL 0 | PASS 85 / FAIL 0 |
| `p02-4` | PASS=87 / FAIL=0 | PASS=87 / FAIL=0 |
| `p03-1` | 通过 93 / 失败 0 | 通过 93 / 失败 0 |
| VM 内核原生测试二进制（agentstore/agent/operations/agentclient/syscmd） | 169 用例 PASS / 0 FAIL | 169 用例 PASS / 0 FAIL |

两台 VM 上 `/tmp/upanel-bin/upanel-agent` 与 `upanel-web` 的 sha256 与宿主交叉编译产物
（`cross-build-sha256.txt`）逐字节一致，所以“验收跑的是这份代码”有据可查。
宿主侧：`go build ./...` 退出 0、`go vet ./...` 退出 0、`gofmt -l .` 无输出、
`go test ./...` 全 ok（`-v` 计数 170 PASS / 0 FAIL）、`check.sh` PASS 12 / FAIL 0 / NOT RUN 0、
`redocly lint api/openapi.yaml` valid（10 warnings，与改动前同类同量）。

**验收抓到的两个真缺陷（改产品，不改断言）**

1. **`CONFIRMATION_MISMATCH` 的状态码错了**：`internal/httpx.StatusForCode` 把它放在 409 那组，
   但 CONTRACTS §4.2 与 `api/openapi.yaml` 都写 400，代理侧也直接回 400。后果是公开 API 与
   契约差一个状态码，前端按契约写的 400 分支永远收不到。VM 上表现为
   `短语不符返回 400: 期望 '400'，实际 '409'`。已改为 400 并归入 `CodeValidationError` 同组
   （DECISIONS Q64）。
2. **SQLite WAL 下 DEFERRED 事务升级失败不排队**：默认 `_txlock` 是 DEFERRED，事务先取读快照、
   等到第一次写才升级成写锁；**升级失败时 busy handler 不会被调用**，直接返回 `SQLITE_BUSY`。
   表现是“ping 任务提交 202 期望、实际 500”，代理日志
   `agentstore: 消费预检失败: database is locked (5) (SQLITE_BUSY)`，且只在前一个恢复性写入
   之后 350 ms 内出现。改为 `_txlock=immediate`（`internal/store/store.go` 的 `dsn()`），
   BEGIN 时就拿写锁、冲突交给 `busy_timeout` 排队（DECISIONS Q65），并新增
   `TestWithTxQueuesBehindAnotherWriter`；**把 `_txlock=immediate` 去掉后该测试确实失败**
   （`并发写事务应排队等待而不是返回 SQLITE_BUSY，实际: database is locked (5) (SQLITE_BUSY)`），
   证明它拦得住回归。

**修的四份过期验收脚本（只按已文档化的现行行为更新，断言只增不减）**

- `p01-1`（26 → 29 条）：SPA fallback 让“未知路径 404”变成 200 返回 index.html（P01.4）；
  `/run/upanel` 权限 0750 被代理拒绝，需 0755 root:root（P02.3，代理日志有原文）；
  缺 `/etc/upanel/agent.key` 时需 `-init-key` 引导（P02.4）；普通路由对 root UID 返回 403
  （P02.3），新增 `agent-root-forbidden` 断言。
- `p01-2`：`schema_version` 断言从 1 改成 **5**（0001—0005），并写明“这是对当前期望版本的精确
  断言，每加一份迁移必须同步抬”。
- `p01-4`：`DEPLOY_DIR` 未 stage 导致 7 项 systemd 断言失败，改指向 `/tmp/upanel-src/deploy`。
- `p03-2`：`login()` 漏了 `X-UPanel-Request: 1`（p03-1 里有）；补上，并加“登录失败即早退
  （exit 1，不是 exit 2）”，再给所有 `PF_*` 变量加 `:-` 兜底，避免未设变量让脚本以 exit 2
  冒充“环境不满足”。

另外 `p02-3` 的断言数从 84 变成 **85**，不是放宽也不是新增功能：代码阶段（上一节第 40—41 行）
把“操作接口未实现 404”改成了“未知字段 400”，并新增了 `unknown-path-404-for-web` 一条，
改名 + 新增后净增一条。两台 VM 上 85/0 与脚本一致。

**交付去向**：本轮结束时仓库已纳入 git 并推送到 <https://github.com/clone668/uPanel>
（`main`，首个提交 `d76e339`，379 个文件 / 56812 行；`.gitignore` 排除 `web/node_modules`、
`bin/`、`dist/`、`*.db`、`*.test`、`*.key`、磁盘镜像）。推完核对 `git ls-remote origin` 的
`refs/heads/main` 与本地 `HEAD` 同一个 sha，工作区干净。

**环境事实（不记作产品缺陷）**：24.04 VM 本轮 panic 两次（约 4058 s 与约 528 s uptime，
同一条 `Attempted to kill init! exitcode=0x8b`），重启重跑后全绿；其中一次留下被截断的
`/etc/upanel/upanel.json`（内容为空），三项断言报 `解析配置文件失败 … EOF`，删文件后恢复。
22.04 VM 全程未 panic。

### P03.3（2026-10-20）— SSE 事件、断线重连、日志限额、任务中心、状态机与错误详情

交付（代码）：

- SSE 端点 `GET /api/v1/jobs/{id}/events`（`internal/api/jobstream.go`）。帧格式照 CONTRACTS §4.3：
  `id: <seq>` + `event: status|progress|log|gap` + `data: {seq,kind,payload,created_at}`；
  连接建立先发一帧 `: connected`（注释帧，不推进水位），之后每 15 秒一帧 `: keepalive`。
  响应头 `Content-Type: text/event-stream`、`Cache-Control: no-store`、`X-Accel-Buffering: no`。
  每条帧写之前 `SetWriteDeadline(10s)` —— 用绝对 `WriteTimeout` 会连正常的长连接一起杀掉，
  这是 SSE 最容易踩的坑。
- 断线重连靠 `Last-Event-ID` 请求头（也接受 `last_event_id` 查询参数，方便 curl 调试），
  格式非法直接 400 `VALIDATION_ERROR`。补发范围是 `job_events.seq > after`；如果客户端要的位置
  早于保留窗口（`bounds.MinSeq`），先发一帧 `event: gap`（`seq` 恒为 0、**不带 id 行**，因此
  不会把水位往回推），再补发窗口内的事件。前端收到 `gap` 就不做增量猜补，直接重读详情 + 日志
  并 `stream.restart()`。
- 每用户最多 4 条并发连接（`sseMaxConnsPerUser`），第 5 条 429；连接断开即释放配额
  （用 `acquire`/`release` 包住整个处理函数，不是靠 GC）。
- 事件镜像 `internal/operations/jobevent.go`：代理是**唯一**的事件生产者，Web 只抄。
  水位是 `jobs.agent_event_seq`，只抄 `seq > 水位` 的事件，并在**同一个事务**里把水位推上去
  （要么事件与水位一起落地，要么都不落地）。写入顺序是**先事件、后状态**——客户端不会出现
  “先看到 succeeded、再等 finished”的顺序错乱。
- 日志限额（迁移 `0006_job_event_mirror.sql`）：`job_events` 的 `UPDATE` 永远被触发器拒绝；
  `DELETE` 只开给 `kind='log'`，每个任务保留最近 1000 行，状态/进度/gap 事件一条都不删。
  这是对 P03.2“绝对只追加”的一次**有意识的降级**，理由与边界写在迁移注释和 DECISIONS Q68 里。
- 日志分页 `GET /api/v1/jobs/{id}/logs?cursor=&limit=`：默认 100，上限 200（`MaxJobEventPage`），
  `next_cursor` 可为 `null`（到底了），坏 cursor 400。
- 前端状态机与错误详情 `web/src/api/jobs.ts`：`canCancel`（admin + 可取消 + 未请求取消 + 非终态
  四个条件缺一不可）、`progressText`（`null` → “未上报”，不编进度）、`jobErrorMessage`
  （`AGENT_UNAVAILABLE` / `OPERATION_INTERRUPTED` 各带一条“先确认真实状态再决定”的 hint）、
  `mergeLogs`（按 seq 去重升序）、`trimLogs(500)`。
- 任务中心 `web/src/views/JobsView.vue`：列表 5 秒轮询、详情、SSE 状态标签（连接中/已连接/重连中/
  已断开）、`gap` 触发整体重读、日志尾部合并、取消（失败时显示错误面板，不吞错）、viewer 只读提示、
  结果 JSON 折叠块。
- 流式客户端 `web/src/api/jobstream.ts`：`openJobStream(jobId, handlers, factory)` 接受可注入的
  `EventSourceLike`，所以流逻辑能在 node 里测（不引 jsdom，DECISIONS Q21 的约定延续）；
  `readyState === 2` 报 `closed`，其余报 `reconnecting`，不把“断了”说成“还在连”。
- 登录页与路由守卫（**范围补充**，DECISIONS Q72）：`web/src/views/LoginView.vue`、
  `web/src/stores/session.ts`（`unknown|anonymous|authenticated`，401 当正常结果处理）、
  `web/src/router/index.ts` 的 `installAuthGuard`、`web/src/router/redirect.ts` 的 `safeRedirect`
  （拒绝非字符串、非 `/` 开头、`//`、`/\`、`/login`，防开放重定向与登录页自环）。
- 契约：`api/openapi.yaml` 新增 `/api/v1/jobs/{id}/events`（SSE，200/400/401/403/404/429，含样例流）
  与 `/api/v1/jobs/{id}/logs`（cursor/limit，200/400/401/403/404）；新增 schema `JobEvent`
  （`seq >= 0`、`kind` 闭枚举、`payload`、`created_at`）与 `JobLogsData`
  （`next_cursor` 用 `type: [string, 'null']` —— OpenAPI 3.1 里 `nullable: true` 无效，
  redocly 会直接报错）；`stage: P03.3`，`implemented_paths` 同步。
- 测试：`internal/api/jobstream_test.go` 用 `httptest.NewServer` 走真实 HTTP（帧格式、resume、
  坏 `Last-Event-ID`、权限、`gap`、连接上限、日志分页各一组；`ResponseRecorder` 无法断言
  chunked 刷新与断连，所以不用它）；前端新增 `jobs.test.ts` 17 项、`jobstream.test.ts` 19 项、
  `redirect.test.ts` 4 项。
- 代理侧修复（本轮 22.04 偶发失败暴露，**改产品不改测试**）：`internal/agentstore/operations.go`
  的 `MarkRunning` / `Finish` / `RequestCancel` 把“阶段变更”和“对应事件”合进同一个事务，
  条件更新带阶段守卫、未命中则回滚重试。根因与证据见 DECISIONS Q67 与下面的验收记录。

接口/数据库变更：

- 新增公开接口 2 条（SSE 事件流、日志分页）；均为只读，`requireSession` + `requireAdmin`。
- Web 库迁移 `0006_job_event_mirror.sql`：`jobs.agent_event_seq`（镜像水位）+
  `job_events` 的删除口子（只允许 `kind='log'`）。schema_version 5 → 6。
- `p01-2` 的 `schema_version` 断言 5 → 6（同一约定：精确断言当前版本）。

验证方式变更：

- 新增 `tests/integration/run-all.sh` 与 `scripts/run-native-tests.sh`：前者按脚本头声明的身份
  （root / 非 root）分组调度并在跑之前检查三个二进制是否可执行，后者在 VM 内跑原生测试二进制并
  统计 `--- PASS` / `--- FAIL`。起因是我用 `sudo` 跑了本该以非 root 运行的 `p01-2`，
  root 绕过目录权限导致脚本挂起 32 分钟（DECISIONS Q71）。

### P03.3 验收结果（同上日，验收回填）

**结论：passed。** 代码 + 两台 VM 验收均完成，证据在 `docs/evidence/P03/2026-10-20-p03.3/`。

| 检查 | 24.04.5 / 6.8.0-147 | 22.04.5 / 5.15.0-198 |
|---|---|---|
| `p03-3-events-acceptance.sh` | 通过 73 / 失败 0 | 通过 73 / 失败 0 |
| `p01-1` | PASS 29 / FAIL 0 | PASS 29 / FAIL 0 |
| `p01-2` | PASS 34 / FAIL 0 | PASS 34 / FAIL 0 |
| `p01-4` | 通过 114 / 失败 0 | 通过 114 / 失败 0 |
| `p02-1` | PASS=58 / FAIL=0 | PASS=58 / FAIL=0 |
| `p02-2` | PASS=103 / FAIL=0 | PASS=103 / FAIL=0 |
| `p02-3` | PASS 85 / FAIL 0 | PASS 85 / FAIL 0 |
| `p02-4` | PASS=87 / FAIL=0 | PASS=87 / FAIL=0 |
| `p03-1` | 通过 93 / 失败 0 | 通过 93 / 失败 0 |
| `p03-2` | 通过 101 / 失败 0 | 通过 101 / 失败 0 |
| VM 内核原生测试二进制（agentstore/agent/agentclient/operations/syscmd/store/api） | TOTAL PASS=346 / FAIL=0 | TOTAL PASS=346 / FAIL=0 |

两台 VM 上 `/tmp/upanel-bin/{upanel-agent,upanel-web,upanelctl}` 与测试二进制的 sha256 与宿主
交叉编译产物（`cross-build-sha256.txt`、`vm-*-env.txt`）逐字节一致，所以“验收跑的是这份代码”
有据可查。宿主侧：`go build ./...` 退出 0、`go vet ./...` 退出 0、`gofmt -l .` 无输出、
`go test ./...` 全 ok（`-v` 计数 228 PASS / 0 FAIL）、`check.sh` PASS 12 / FAIL 0 / NOT RUN 0、
`redocly lint api/openapi.yaml` valid（10 warnings，与改动前同类同量）。

**验收抓到的真缺陷（22.04 上偶发，改产品不改测试）**

`TestOperationSubmitRunsOnceAndRecordsResult` 在 22.04 上三次跑出两次失败：
`事件太少: [{Seq:1 Kind:accepted} {Seq:2 Kind:started}]`，而此时操作阶段已经是 `succeeded`。
根因是代理的 `MarkRunning` / `Finish` / `RequestCancel` 先 `UPDATE operations` 再
`INSERT operation_events`，两笔独立的写之间存在窗口，读方可以先看到终态、后（甚至永远不）
看到终态事件。这不是“测试假设了一个代码没承诺的顺序”：Web 的 `MirrorAgentEvents` 正是按
“阶段可见 ⟹ 对应事件可见”写的（先读操作、再读事件，看到终态后不再追），分两笔写会让终态事件
永久丢失在镜像之外。**修在产品侧**：三处阶段变更合进一个事务，条件更新带阶段守卫，
未命中就回滚重试（DECISIONS Q67）。**确定性证明**：新增
`TestFinishRollsBackStageWhenEventWriteFails`——用 `BEFORE INSERT` 触发器只让该操作的事件写入失败，
断言终态一起回滚；把 `internal/agentstore/operations.go` 换回上一个提交的版本时该测试失败
（`事件写入失败后阶段 = "succeeded"，期望仍是 running`），换回修复版本通过。另有
`TestStageChangeAndEventAreAtomic` 用并发读方持续检查不变量。修复后同一用例在 22.04 上
连续 20 次通过、在宿主上 40 次通过。

**本轮暴露的两个流程问题（改调度，不改断言；DECISIONS Q71）**

1. 我用 `sudo` 跑了本该以非 root 运行的 `p01-2`：root 绕过 `0500` 目录权限，
   “数据库目录不可写必须拒绝启动”那条永远等不到退出，脚本挂了 32 分钟。改成
   `tests/integration/run-all.sh` 按脚本头声明的身份调度（非 root 用 `setpriv` 降到 `upanel`）。
2. 上传后 `chmod 0755 /tmp/upanel-bin/upanel-*` 漏掉了 `upanelctl`（结尾没有连字符），
   24.04 上 `p02-1` 因此刷出 45 条“功能失败”，看起来像产品坏了。`run-all.sh` 现在先检查三个
   入口是否可执行，缺文件直接 exit 2 并打印原因。

**环境事实（不记作产品缺陷）**：24.04 VM 本轮第四次中断——15:2x 期间 ssh 开始 banner 超时、
登录不进，强制重启后恢复；上一次启动的 kernel 日志结尾只有
`journald: File ... corrupted or uncleanly shut down`，没有 oops/panic 记录（本轮之前它已
panic 三次，都是 `Attempted to kill init! exitcode=0x8b`）。重启后 `/tmp` 被清空，
二进制与脚本重新上传并逐一核对 sha256 后全部重跑通过。22.04 VM 全程未中断。

**NOT RUN（本轮没有执行，不能算通过）**

- `go test -race`：宿主 `go test -race` 需要 cgo，本机报
  `-race requires cgo; enable cgo by setting CGO_ENABLED=1`，未执行；VM 里同样没有启用 cgo，
  因此整轮**没有**竞态检测器的证据，并发正确性只由 `TestStageChangeAndEventAreAtomic`
  这类并发读写用例 + `_txlock=immediate` 的排队语义支撑。
- 浏览器端的 SSE 与登录页：只有 node 环境下的逻辑测试（可注入 `EventSourceLike`）与构建产物；
  没有用真实浏览器打开任务中心、没有在 Chromium 里观察帧刷新/重连/gap 的实际表现，
  Firefox/Safari 更没验证（DECISIONS Q21、Q72）。
- SSE 长连接下的资源占用：没有做“连接挂 1 小时”“4 条连接跑满再断线重连”的长时间压测；
  连接上限与释放只做了短时断言。
- 日志环裁剪只验证到“超过上限后最旧的日志被删、状态事件不受影响”，没有在真实任务里
  产出上万行日志做容量与性能观测。
- 代理/Web 同时重启时的对账收敛（P03.4 的范围）尚未验证。

**交付去向**：代码与证据已推送到 <https://github.com/clone668/uPanel>，`refs/heads/main` =
本地 `HEAD` 同一个 sha，工作区干净。P03.3 的代码与证据提交是
`a396881735d614d4f4ca7a2302dd3da1cb75cde6`（74 个文件 / +7758 −100），本行随后的文档补充提交
把 `main` 推到 `22d3f30`，两者都已核对。本轮新增的验收设施
（`tests/integration/run-all.sh`、`scripts/run-native-tests.sh`）随代码一起入库，
换机后重建一次 VM 就能一键重跑全量回归。

### P03.4（2026-10-20）— Web 与代理重启对账、重复提交、执行结果未知的 interrupted

交付（代码）：

- **后台对账**（`internal/operations/reconcile.go`，新文件）。`Service.RunReconciler` 由
  `cmd/upanel-web` 以 goroutine 启动：**启动立刻扫一遍**，之后每 `ReconcileInterval = 30s` 一遍；
  `ReconcileBatch = 100`，按 `updated_at` 升序取 `needs_sync = 1 OR status NOT IN (终态)`。
  扫描只调 `GET /internal/v1/operations/:id`——**代码里没有任何 CreateOperation 调用点**，
  对账永远不会重发（DECISIONS Q73）。
- **提交在途登记**（`Service.inflight` + `beginSubmit/endSubmit`）。任务已落库、代理调用还没返回的
  窗口里，对账不下任何结论（`outcomeInflight`，计入 `skipped_inflight`）。没有它，一个正在执行的
  破坏性动作会被写成 `failed`，而代理随后照样执行（DECISIONS Q74）。
- **`NOT_FOUND` 分流**（修订 Q63）：`needs_sync=1` → `failed`/`unconfirmed`/`AGENT_UNAVAILABLE`
  （提交没到达）；`needs_sync=0` → `interrupted`/`unknown_result`/`OPERATION_INTERRUPTED`、
  `cancellable=0`、错误体要求先核对实际状态（DECISIONS Q75）。读接口与后台扫描共用同一份判定
  （`reconcileOne`），不会因为“被读”还是“被扫”得到不同终态。
- **幂等记录清理**（`PurgeExpiredIdempotencyKeys`，`RunReconciler` 里每小时一次）：
  只删“已过期且任务已结束”的记录，未结束任务的键必须留着，否则重试就等于第二次执行
  （DECISIONS Q76）。
- **验收脚本** `tests/integration/p03-4-reconcile-acceptance.sh`（78 项断言）：生产登记表为空、
  十次/并发同键提交、同键不同请求 409、Web 重启后重放、提交未到达、代理 kill -9 后的
  interrupted、代理库丢失后的 unknown_result、终态不回退、没有重试接口。
- **调度修正** `tests/integration/run-all.sh`：日志改写到自建的 `LOG_DIR`（默认
  `/var/tmp/upanel-logs`）。24.04 上出现过“脚本全通过、重定向失败、退出码被读成 2”的假失败——
  `/tmp` 的粘滞位让后一次运行写不进上一次以 root 建的同名文件。
- **测试注册表约束变成断言**（`internal/agent/acceptance_test.go`）：生产表为空、验收动作全部带
  `acceptance.` 前缀且不在生产表、`Decode/Plan/Execute` 齐全、计划必带 `acceptance_only` 告警、
  参数解码拒绝未知字段（DECISIONS Q77）。
- **代理侧**沿用 P03.2 的 `reconcileInterrupted`（启动时把遗留非终态操作标 interrupted，
  不自动重跑）：这一轮用 `kill -9` 在真实 VM 上验证了它与 Web 对账的衔接。

### P03.4 验收结果（同上日，验收回填）

**结论：passed。** 代码 + 两台 VM 验收均完成，证据在 `docs/evidence/P03/2026-10-20-p03.4/`。

| 检查 | 24.04.5 / 6.8.0-147 | 22.04.5 / 5.15.0-198 |
|---|---|---|
| `p03-4-reconcile-acceptance.sh`（新增，78 项断言） | 通过 78 / 失败 0 | 通过 78 / 失败 0 |
| `p01-1` | PASS 29 / FAIL 0 | PASS 29 / FAIL 0 |
| `p01-2` | PASS 34 / FAIL 0 | PASS 34 / FAIL 0 |
| `p01-4` | 通过 114 / 失败 0 | 通过 114 / 失败 0 |
| `p02-1` | PASS=58 / FAIL=0 | PASS=58 / FAIL=0 |
| `p02-2` | PASS=103 / FAIL=0 | PASS=103 / FAIL=0 |
| `p02-3` | PASS 85 / FAIL 0 | PASS 85 / FAIL 0 |
| `p02-4` | PASS=87 / FAIL=0 | PASS=87 / FAIL=0 |
| `p03-1` | 通过 93 / 失败 0 | 通过 93 / 失败 0 |
| `p03-2` | 通过 102 / 失败 0 | 通过 102 / 失败 0 |
| `p03-3` | 通过 73 / 失败 0 | 通过 73 / 失败 0 |
| VM 内核原生测试二进制（7 个） | TOTAL PASS 360 / FAIL 0 | TOTAL PASS 360 / FAIL 0 |
| `run-all.sh` 汇总 | 运行 11 / 失败 0 | 运行 11 / 失败 0 |

宿主侧：`go build ./...`、`go vet ./...` 退出 0，`gofmt -l .` 无输出，`go test ./...` 18 包 ok
（`-v` 计数 258 PASS / 0 FAIL），`check.sh` PASS 12 / FAIL 0 / NOT RUN 0，`redocly lint` valid
（10 warnings），交叉编译三个入口的 sha256 与两台 VM 上逐字节一致。

**p03-4 脚本验证到的行为**（11 个段落，全部黑盒：curl + 查库 + 杀进程）

1. 生产登记表为空：不带 `-acceptance-actions` 的代理对 `acceptance.probe` 预检回 400“未登记的动作”。
2. 同一个幂等键提交十次、并发提交十次：都只有一个 job、一次代理执行。
3. 同键不同请求 → 409 `IDEMPOTENCY_CONFLICT`（`curl --max-time 60` 兜住每个请求）。
4. **Web 重启后重放同一个键**：拿回原 `job_id`，代理操作数不变——不重新执行。
5. **提交没到达**（代理从未见过这条操作，`needs_sync=1`）：启动扫描判 `failed`/`unconfirmed`/
   `AGENT_UNAVAILABLE`。
6. **代理 `kill -9`**：重启后遗留操作被代理标 `interrupted`，Web 启动扫描同步为 `interrupted`，
   `cancellable=0`，代理操作数不变（没有重放）。
7. **代理侧记录丢失**（`needs_sync=0`）：等后台 30 秒扫描（不读页面、不重启 Web），任务变
   `interrupted`/`unknown_result`/`OPERATION_INTERRUPTED`——**不是** `failed`。
8. 终态不回退、`interrupted` 再取消 → 409、`POST /jobs/{id}/retry` → 404（没有重试接口）。
9. 启动对账在 Web 日志里留痕（`开始任务对账` + `任务对账扫描完成` 的七个计数），
   且日志不含口令与 CSRF。

**证明“测试有牙”**：摘掉在途守卫 → 两个用例失败（`Unconfirmed:1`，期望 `SkippedInflight:1`）；
把 `needs_sync` 分流写死 → `TestSweepPendingMarksUnconfirmedWhenAgentNeverSawOperation` 失败；
`grep CreateOperation internal/operations/reconcile.go` 只命中注释（对账没有重新提交的调用点）。
三份输出都在证据目录（`mutation-1-*`、`mutation-2-*`、`static-no-resubmit.txt`）。

**本轮修掉的四个缺陷**

| 缺陷 | 处理 | 决策 |
|---|---|---|
| `NOT_FOUND` 一律写 failed（Q63 的状态选错） | 按 `needs_sync` 分流：没到达 → failed；曾经到达 → interrupted/结果未知 | Q75 |
| `TestGateDeniesOtherUIDs` 扫整个响应体，随机 `request_id` 里撞上 “4242” | 只断言 `error.code` 与 `error.message` | Q78 |
| `p01-4` 不清 `/var/lib/upanel/upanel.db`，旧迁移校验和让服务按设计拒绝启动 | migrate 前清库（现场由脚本自建） | Q79 |
| 两处“按时间取最新一条”的断言 | 改成按幂等键 / `operation_id` 指名对象，并加错误码断言 | Q80 |

顺带关闭了 P03.2 的欠账：`idempotency_keys` 的到期清理（当时注释写“属 P03.4”）现已实现，
且只删“任务已结束”的记录，不会把重试变成第二次执行（Q76）。

**环境事实（不记作产品缺陷）**：24.04 验收 VM 本轮开始前刚经历过一次 panic
（`Attempted to kill init! exitcode=0x8b`），`/tmp` 随重启清空，二进制与脚本重新上传并逐个
核对 sha256。运行期间两台 VM 都没有中断。宿主 `check.sh` 的一次 go-test 变红是 Q78 的
随机串问题，不是产品回归。**harness 事故**：`run-all.sh` 早先把逐脚本日志写进
`/tmp/upanel-accept-<name>.log`，`/tmp` 的粘滞位让后一次运行写不进上一次以 root 建的同名文件，
于是 10 个脚本“全通过但退出码 2”——已改为自建 `LOG_DIR`（默认 `/var/tmp/upanel-logs`，0777）。

**NOT RUN（本轮没有执行，不能算通过）**

- `go test -race`：宿主与 VM 都没有启用 cgo，整轮没有竞态检测器证据。
- 幂等记录清理的 1 小时周期：验收脚本不会等一小时，“每小时真的跑了一次”只有单元测试，
  **没有** VM 观测。
- 真实浏览器：本轮没有任何浏览器证据；`interrupted` 的展示沿用 P03.3 的组件，未在浏览器里看过。
  P03.3 的其他 NOT RUN（SSE 长连接压测、日志环容量、Firefox/Safari）同样仍未执行。
- 代理与 Web **同时**重启的收敛：脚本只覆盖“先杀代理”“先杀 Web”两种顺序。
- 多 Web 进程/多副本：在途登记是进程内的（Q74），systemd 单实例前提之外未验证。
- 连续运行数小时的累积影响（内存、日志量、WAL 增长）未观测。

**交付去向**：代码与证据已推送到 <https://github.com/clone668/uPanel>，`refs/heads/main` =
本地 `HEAD` 同一个 sha `6db9b35fb8e0665a48ae1950d3eb0e1ad2cd9e22`（58 个文件 / +5407 −61），
工作区干净。证据目录 `docs/evidence/P03/2026-10-20-p03.4/` 含宿主闸门输出、两台 VM 的环境与
sha256、`runall-*.txt`、逐脚本日志（`vm-logs-2404/`、`vm-logs-2204/`）、变异测试输出、
`commands.log` 与 `result.md`。本轮新增的 `tests/integration/p03-4-reconcile-acceptance.sh`
与 `run-all.sh` 的 `LOG_DIR` 改动随代码一起入库，换台机器重建一次验收环境就能一键重跑。

### P04.1（2026-10-20）— 系统基础资源采集、5 秒轮询缓存与时间戳

**做了什么**

| 层 | 文件 | 内容 |
|---|---|---|
| 采集 | `internal/monitoring/types.go`（新） | DTO：`Summary{cpu,memory,load,network,host,interval_ms,stale,degraded,collected_at}`，每段带 `status`/`error`/`source`/`collected_at`；字节与计数器一律 `json:",string"`（CONTRACTS §1） |
| 采集 | `internal/monitoring/procfs.go`（新） | `/proc/stat`、`meminfo`、`loadavg`、`net/dev`、`os-release`、`uptime`、`cpuinfo` 解析；`syscall.Uname` 取内核与主机名。只读文件，不执行命令 |
| 采集 | `internal/monitoring/collector.go`（新） | 5 秒 ticker（先立即采一次）、`Refresh`、`Snapshot`（读取时判 `stale`：超过 3 个间隔）、`Summary`（冷启动同步采一次）；某段失败保留上次有效值与它的旧时间，从未成功则 `null` |
| 采集 | `internal/monitoring/collector.go` | 速率 = 相邻两次样本差分 / 时间窗；无前次样本、计数器回退、窗口非正 → `null`；`boot_id` 变化让 CPU 采样窗口失效 |
| 代理 | `internal/agent/systemquery.go`（新） | 查询 `system.summary`：零参数，`DecodeParams` 用 `DisallowUnknownFields` 拒绝任何字段（`params.command` 也拒），返回采集器快照并附 `collected_at` |
| 代理 | `internal/agent/registry.go` | `ProductionDeps.Collector != nil` 时才登记该查询；未注入采集器就不上报这条能力（Q84） |
| 代理 | `cmd/upanel-agent/main.go` | 启动采集循环，随信号 ctx 退出 |
| Web | `internal/api/system.go`（新）、`router.go`、`server.go` | `GET /api/v1/system/summary`，权限 V；代理不可用 → 503 `AGENT_UNAVAILABLE`（Q88），登录与其余接口不受影响 |
| Web | `internal/monitoring/fetch.go`（新）、`cmd/upanel-web/main.go` | 代理客户端 `Fetcher`，**不做第二层缓存**：`collected_at` 永远是采集时间（Q81） |
| 契约 | `api/openapi.yaml` | 新路径 + 新 schema，`info.version: 0.0.0-p04.1`；只登记已实现的路径 |
| 测试 | `internal/monitoring/*_test.go`、`internal/agent/systemquery_test.go`、`internal/api/system_test.go` | 解析器用真实 `/proc` 采集样本 + 合成反例；线格式用例钉住“字符串字节 / `null` 速率” |
| 夹具 | `tests/fixtures/`（6 份真实采集 + 7 份 `*.synthetic.*` 反例）、`PROVENANCE.md` | 来源与采集命令写清楚（Q85） |
| 夹具查找 | `internal/testsupport/fixtures.go`（新）、`scripts/run-native-tests.sh` | 环境变量 → 二进制旁边 → 向上查找三层；找不到**失败并列出试过的路径**，不跳过 |
| 验收 | `tests/integration/p04-1-system-acceptance.sh`（新） | 72 项断言，root 运行；`run-all.sh` 注册为第 12 个脚本 |

**接口与数据变更**：新增 `GET /api/v1/system/summary`（V 权限）与代理内部查询 `system.summary`；
**没有**数据库迁移、没有新表、没有新写动作，生产动作登记表仍为空。

**没有引入的东西**（本轮刻意）：没有 `exec`、没有 shell、没有可注入参数的采集接口、没有让
采集跟随请求即时读盘（否则“轮询缓存”名不副实）、没有第二个缓存层。

### P04.1 验收结果（同上日，验收回填）

**结论：passed。** 代码 + 两台 VM 验收均完成，证据在 `docs/evidence/P04/2026-10-20-p04.1/`。

| 检查 | 24.04.5 / 6.8.0-147 | 22.04.5 / 5.15.0-198 |
|---|---|---|
| `p04-1-system-acceptance.sh`（新增，72 项断言） | 通过 72 / 失败 0 | 通过 72 / 失败 0 |
| VM 内核原生测试二进制（8 个） | TOTAL PASS **401** / FAIL 0 | TOTAL PASS **401** / FAIL 0 |
| 12 脚本全量回归 | 运行 12 / 失败 0（合计 930 断言） | 运行 12 / 失败 0（合计 930 断言） |

宿主侧：`go build ./...`、`go vet ./...` exit 0，`gofmt -l .` 无输出，`go test ./...` 19 包 ok
（`-v` 计数 228 PASS / 0 FAIL），`check.sh` PASS 12 / FAIL 0 / NOT RUN 0，`redocly lint` valid
（9 warnings）。三个入口二进制与 8 个测试二进制在两台 VM 上 sha256 逐个一致（11/11）。

**p04-1 脚本验到的行为**（10 段，全部黑盒：curl + 读 `/proc` + 杀进程）

1. 身份：root 运行、三个二进制可执行、`upanel-web` 账号存在、代理 uid=0。
2. 能力如实：`supported_queries == ['system.summary']`、`supported_actions == []`。
3. 与 `/proc` 逐项比对：`MemTotal` 按字节精确相等；`MemAvailable` 与前后两次读取同量级
   （±64 MiB，页缓存让它在毫秒级就变）；`used = total - available`；逻辑核数 = `nproc`；
   `boot_id`、`os_id`、`kernel_release`、主机名、开机时长、1 分钟负载（容差 1.0）、五个段
   `status=ok`。
4. 时间戳与缓存：`interval_ms=5000`；`collected_at` 在 10 秒内；`stale=false`、`degraded=false`；
   睡 6 秒后 `collected_at` 前进 4—9 秒（不是每次请求都变，也不是不动）。
5. 计数器区间：`ens4` 的 `rx_bytes` 落在“睡满一个间隔”前后两次 `/proc/net/dev` 之间，
   第二份不小于第一份。
6. 使用率与速率：第二份快照 `usage_percent` 非 null 且在 0—100，`sample_window_ms` 在 4000—9000，
   按核使用率数量 = 逻辑核数，至少一块网卡给出非 null 非负速率。
7. 没有万能入口：`params.command` → 400、未知参数 → 400、`system.nonexistent` → 400“未登记”、
   `GET /internal/v1/queries` → 405。
8. Web 透传：未登录 401；admin 200 且数值与 `/proc` 一致；新建 viewer 后 viewer 同样 200。
9. 采集故障不阻塞登录：停代理 → 登录仍 200，`/api/v1/system/summary` 503 `AGENT_UNAVAILABLE`
   且**不返回任何数值**；代理重启后自动回到 200 且是真实值。
10. 日志卫生：代理日志无 `sh -c`、无 `BEGIN PRIVATE KEY`，Web 日志无 `panic`。

**证明“测试有牙”**：`types_test.go` 钉住 `"total_bytes":"4009864"`（字符串）与
`"rx_bytes_per_second":null`；把字段上的 `json:",string"` 去掉该用例立刻失败。夹具用例在
VM 上找不到 `tests/fixtures` 时会**失败并打印试过的路径**（P02.3 就是这样先红后绿的），
不是 skip。

**本轮修掉的问题**（改产品/改验证手段，没有放宽任何断言）

| 问题 | 处理 | 决策 |
|---|---|---|
| 字节/计数器被序列化成 JSON 数字 | 全部加 `json:",string"` + 线格式用例 | Q82 |
| 22.04 的三条时间戳断言判 False（24.04 全绿） | Python 3.10 `fromisoformat` 不认 9 位小数秒 → 脚本内截到 6 位；**产品格式不改** | Q87 |
| VM 上夹具用例全挂 | 测试二进制跑在 `/tmp`，`../../tests/fixtures` 没有意义 → 三层查找 + 运行器拷夹具，找不到就失败 | Q85 补充 |
| “快照只增不减”在 24.04 偶发失败 | 断言把两份快照顺序弄反（`jgetx` 的第一个参数才是 `d`），修正方向 | 验收脚本缺陷 |
| 代理停机后 Web 起不来（`readonly database`） | 只 chown 了库文件没 chown 目录，SQLite 建不了 `-wal` → 先 chown 目录 | 验收脚本缺陷 |
| 22.04 的 `tests/api.test` 是旧产物（`8cf56ce6…`） | 重新同步 11 个文件并重跑原生测试；回归不受影响（回归只用三个入口二进制，那几个字节相同） | 证据缺陷 |

**环境事实（不记作产品缺陷）**：24.04 本轮开始前第四次 panic（`Attempted to kill init!`
exitcode=0x8b），重启后 `/tmp` 清空、`/etc/upanel/upanel.json` 又被截断成 0 字节；删文件、
重传二进制/夹具/脚本、逐个核 sha256 后重跑。**所有 VM 结论都来自重启之后的那一轮。**

**NOT RUN**：真实生产服务器；小时级长跑的内存/fd 稳定性；段失败的真实注入（只有单测用假
reader 覆盖）；受限容器/无 `/proc` 环境；`go test -race`（宿主与 VM 都没开 cgo）；浏览器证据
（首页属 P04.4）；采集间隔可配置（本轮固定 5000 ms，`interval_ms` 已随响应返回）。

**交付去向**：代码与证据已推送到 <https://github.com/clone668/uPanel>，代码提交
`32ca37a2ed4bff7fdbac1a81a19bb08fca6dd0ea`（90 个文件 / +7771 −35），本条记录随紧随其后的
文档提交入库。证据目录 `docs/evidence/P04/2026-10-20-p04.1/` 含宿主闸门输出、两台 VM 环境、
二进制 sha256 对照（`cross-build-sha256.txt`、`vm-sha256-2404.txt`、`vm-sha256-2204.txt`）、
`runall-*.txt`、`p04-1-*.txt`、`native-*.txt`、逐脚本日志（`vm-logs-2404/`、`vm-logs-2204/`）、
`commands.log` 与 `result.md`。新增的 `tests/integration/p04-1-system-acceptance.sh`、
`tests/fixtures/` 与 `scripts/run-native-tests.sh` 的夹具逻辑随代码一起入库，换台机器重建一次
验收环境就能一键重跑。

### P04.2（2026-10-20）— 存储拓扑只读：块设备树、设备身份校验、系统盘保护识别

**做了什么**

| 层 | 文件 | 内容 |
|---|---|---|
| 采集 | `internal/storage/types.go`（新） | DTO：`Topology{nodes,edges,snapshot_version,collected_at,interval_ms,stale}`、`Device`、`PartitionTable`/`Partition`/`FreeRegion`、`Filesystem`/`Mount`/`Capacity`、`Health`/`Capability`、`Protection`、`Swap`、`Edge`。字节量与计数器一律 `json:",string"`；读不到的字段是 `null`，不是 `0` |
| 采集 | `internal/storage/scan.go`（新） | 走 `/sys/block`（含 `/sys/class/block` 下的分区）枚举设备；`dev`/`size`/`ro`/`removable`/`holders`/`slaves`/`device/{model,vendor,serial,wwid}`；`partition` 属性区分整盘与分区；`by-id/by-uuid/by-label` 稳定链接；`/proc/self/mountinfo` 与 `/proc/swaps` 关联挂载与交换区。**没有 exec、没有 shell、没有 lsblk/blkid** |
| 采集 | `internal/storage/table.go`（新） | 分区表探测：只读盘头部若干 KiB 解析 MBR/GPT，用来补 `type` 与 `free_regions`；与 sysfs 不一致时保留内核结果并给 `warning`；`source=kernel` 表示没去读盘 |
| 采集 | `internal/storage/identity.go`（新） | 身份指纹：按 `wwid` → `serial` → 型号+容量等强弱顺序取可用信息算 `sha256:`，同时给出 `identity_confidence`（high/medium/low）。信息不够就如实 `medium`——两台验收 VM 的 QEMU 盘都读不到 serial/wwid，指纹仍然稳定且两盘不撞车 |
| 采集 | `internal/storage/protect.go`（新） | 系统盘保护：关键路径（`/`、`/boot`、EFI、面板数据目录、面板配置目录、活动交换区）→ 挂载点 → 设备，再沿 `partition_of`/`depends_on` **递归向上**标记底层盘；理由去重固定排序，`sources` 记来源设备 |
| 采集 | `internal/storage/collector.go`（新） | 5 秒拓扑扫描 + 30 秒容量缓存（`statvfs`，跟随挂载点）；扫描失败保留上一份成功快照并置 `stale`，**不清空节点**（读不到不等于没有盘） |
| 代理 | `internal/agent/storagequery.go`（新）、`registry.go` | 查询 `storage.topology`：零参数，`DecodeParams` 拒绝任何字段（含 `params.command`、`params.path`），登记进生产表；`capabilities` 如实报 `queries: ['storage.topology','system.summary']`、`actions: []` |
| Web | `internal/api/storage.go`（新）、`router.go`、`server.go` | `GET /api/v1/storage/{topology,devices,devices/:id}`，权限 V；只透传，不重排边、不改保护判定；未知设备 404；代理不可用 503 `AGENT_UNAVAILABLE` |
| Web | `internal/storage/fetch.go`（新） | 代理客户端 `Fetcher`，同样不做第二层缓存 |
| 契约 | `docs/CONTRACTS.md` §6.1（新）、`api/openapi.yaml` | 新增 15 个 storage schema，`info.version: 0.0.0-p04.2`；§6.1 把“保护沿边递归向上、capabilities 恒空数组、内核分区结果优先、未挂载时 type/uuid/label 为 null、休眠盘不唤醒”写成契约 |
| 测试 | `internal/storage/*_test.go`（8 个文件）、`internal/agent/storagequery_test.go`、`internal/api/storage_test.go` | 用两台 VM 的真实采集夹具（`tests/fixtures/storage-2404/`、`storage-2204/`）当输入，断言设备集合、边方向、指纹稳定、保护递归、分区表解析、`snapshot_version` 与时间无关；合成反例覆盖坏 MBR/GPT 与坏 sysfs 字段 |
| 夹具 | `scripts/capture-storage-fixtures.sh`（新）、`tests/fixtures/storage-{2404,2204}/`、`MANIFEST.txt` | 夹具是**在验收 VM 上原样采集**的（`/sys/block` 子树、`/proc/self/mountinfo`、`/proc/swaps`、盘头部 1 MiB 切片），来源与采集命令写在 MANIFEST 里 |
| 验收 | `tests/integration/p04-2-storage-acceptance.sh`（新） | 15 段 108 项断言，root 运行；`run-all.sh` 注册为第 13 个脚本 |

**接口与数据变更**：新增 `GET /api/v1/storage/topology`、`/storage/devices`、`/storage/devices/:id`（均为 V 权限）
与代理内部查询 `storage.topology`；**没有**数据库迁移、没有新表、没有新写动作，生产动作登记表仍为空。

**没有引入的东西**（本轮刻意）：没有 `lsblk`/`blkid`/`udevadm` 调用（全部读文件）；没有为探测分区表
唤醒休眠盘；没有 SMART（P04.3）；没有编造 `health`（一律 `unknown`、温度 `null`）；没有给出任何
`capabilities` 动作入口。


### P04.2 验收结果（同上日，验收回填）

**结论：passed。** 代码 + 宿主闸门 + 两台 VM 验收全部完成，证据在
`docs/evidence/P04/2026-10-20-p04.2/`，详见其中的 `result.md`。

| 检查 | 24.04.5 / 6.8.0-147 | 22.04.5 / 5.15.0-198 |
|---|---|---|
| `p04-2-storage-acceptance.sh`（新增，108 项断言） | 通过 108 / 失败 0 / 未执行 0 | 通过 108 / 失败 0 / 未执行 0 |
| VM 内核原生测试二进制（9 个） | TOTAL PASS **473** / FAIL 0 | TOTAL PASS **473** / FAIL 0 |
| 13 脚本全量回归 | 运行 13 / 失败 0（合计 **1038** 断言） | 运行 13 / 失败 0（合计 **1038** 断言） |

宿主侧：`go build ./...`、`go vet ./...` exit 0，`gofmt -l .` 无输出，
`go test ./... -count=1 -v` 计 **648** 个 `--- PASS` / 0 个 `--- FAIL`（4 个平台相关 SKIP），
`check.sh` PASS 12 / FAIL 0 / NOT RUN 0，`redocly lint` valid（9 warnings）。
12 个交叉编译产物（3 个入口 + 9 个测试二进制）在宿主与两台 VM 上 sha256 **逐个一致**；
入口二进制用固定 `BUILD_DATE` 重复构建 3/3 逐字节相同。

**p04-2 脚本验到的行为**（15 段，全部黑盒）

1. 环境：root、三个二进制可执行、`upanel-web` 账号、python3。
2. 代理启动：socket、`uid=0`、10 秒内开始应答；`capabilities` 如实报
   `['storage.topology','system.summary']` 且 `actions == []`。
3. 快照形态：`status=ok`、`interval_ms=5000`、`collected_at` 在 15 秒内、`snapshot_version` 是 sha256。
4. 设备集合与 `/sys/block` + 分区完全一致（24.04 上 17 台）；`size_bytes`/`read_only` 与 sysfs 精确一致；
   `kind=partition` 与内核 `partition` 属性一一对应；扇区大小关系成立。
5. 边：两端已知、`relation` 封闭、无重复、无自环；sysfs 认的每个分区都有 `partition_of` 指回整盘且不多出。
6. 身份：只有真读到 WWN/序列号才报 `high`（QEMU 上如实 `medium`）；不同设备不撞车、同型号同容量不撞车；
   内容不变时两次查询版本不变、逐台指纹一致。
7. 诚实性：`health.status` 一律 `unknown`、温度与 `checked_at` 是 `null`、每台设备 `capabilities == []`、
   读不到的字段是 `null` 不是空串。
8. 系统盘保护：脚本从 `/proc/self/mountinfo` **独立重算**承载关键路径的设备（24.04：sda1/sda15/sda16），
   逐台核对被标为系统盘；理由写明承载根文件系统；保护沿边递归向上；空白盘 sdb/sdc/sdd 不被保护。
9. 临时 loop 镜像（64 MiB ext4）：`kind=loop`、挂载点出现、**不因被挂载而成为系统盘**；
   `total_bytes` 与 statvfs 精确一致，`available_bytes` 与空闲镜像 `f_bavail` **精确相等**
   （`f_bfree` 差 4 694 016 字节 → 这条断言真能区分取错字段）；挂载出现/消失都让 `snapshot_version` 变，
   且消失后回到原值（指纹只由内容决定）。
10. 节奏：一个间隔后 `collected_at` 前进 4—30 秒。
11. 容量：`/` 的 `total_bytes` 精确相等；`available_bytes` 落在实时 `f_bavail` 的 0.5% 内
    （30 秒采集窗口内会漂移，脚本注明本机 `f_bfree - f_bavail` 仅 16 MiB、区分不了 Bavail/Bfree，
    严格对账放在第 9 段）；`used_percent` 在 0—100；容量自带 `collected_at`。
12. 边界：不接受任何参数（含 `command`/`path`）、未登记查询名 400 且说明“未登记”、信封夹带未知字段 400。
13. Web 透传：未登录 401；admin 与 viewer 都 200（V）；Web 与代理给出同一个 `snapshot_version`
    且不重排边、不改保护判定；详情只带相关边；未知设备 404；`sda;id`、`..%2f..%2fetc%2fshadow`、
    `sda%20--force` 都不是命令入口。
14. 代理停机：`/storage/topology` 与 `/storage/devices` 503 且不返回任何设备；恢复后 200 且仍是同一份真实拓扑。
15. 日志卫生：无 `sh -c` 痕迹、无私钥泄漏、无 panic。

**证明“测试有牙”**：`TestReadDiskLinksUsesSymlinkTarget` 在 VM 上真跑（Windows 上因缺符号链接权限 SKIP），
`storage-regression-*.txt` 有 `--- PASS`；`TestSnapshotVersionIgnoresTimeAndCapacity` 钉住“相隔 37 秒指纹相同、
补容量不变、改路径才变”；第 9 段的精确相等 + 4.69 MB 保留块差是“取错字段立刻红”的硬证据；
第 8 段的保护判定由脚本独立重算，不是拿被测代码验证被测代码。

**本轮修掉的问题**

| 问题 | 处理 | 决策 |
|---|---|---|
| 代理在真实 24.04 上约 20 秒后被 OOM 杀掉（夹具单测全绿）：`/dev/disk/by-*` 是指向块设备的符号链接，`os.ReadFile` 把整块盘读进内存 | `linkTarget` 先 `Lstat` 再 `Readlink`，普通文件分支只认 `/dev/` 绝对路径；新增符号链接回归用例 | Q89 |
| `snapshot_version` 每 5 秒就变（违反 CONTRACTS §6 预检语义） | 哈希前归一化 `collected_at`/`checked_at`/容量；新增“时间无关”用例 | Q90 |
| `/dev/disk/by-*` 指向非设备文件时被当成设备名 | 只对本次扫描枚举到的设备名生效；链接目标按 `devDir` 相对化 | Q91 |
| `capabilities` 断言过期（`p02-3`/`p04-1` 只列 `system.summary`） | 改成精确列表并注明少报多报都算失败 | Q92 |
| 验收脚本 `jget`/`jgetx` 把 `None` 也打印、`NameError` 变空串静默通过 | 抽出 `_probe` 截 stdout：语句块用它的 print、单表达式用返回值；命名空间补 `json`/`os` | Q95 |
| `TestSweepExpiredSecrets` 高负载下偶发失败（在 50ms TTL 上做边界断言） | 拆成“长 TTL 断言不误杀 + 短 TTL 断言会销毁”，语义不变 | Q96 |

**环境事实（不记作产品缺陷）**：24.04 本轮第五次 `Attempted to kill init!` panic（exitcode=0x8b），
发生在全量回归途中。重启后重新上传二进制/测试/夹具/脚本、核 sha256（12/12 一致），
再依次跑原生测试（473/0）、p04-2（108/0）、全量回归（13 脚本 0 失败）——本文所有 24.04 结论
都来自重启后的那一轮。另有一次**验收环境缺陷**：`chmod +x /tmp/upanel-bin/upanel-*` 匹配不到
`upanelctl`（无连字符），导致 p04-2 报 4 项失败（`init-admin` 退出码 126）；显式列三个文件后 108/0，
已记进 `commands.log` 避免重复踩。

**NOT RUN**：真实生产服务器；LVM/LUKS/软 RAID/NVMe/多路径的真实拓扑（两台 VM 只有 SCSI 盘与 loop）；
休眠盘不唤醒的行为（验收机没有可休眠的盘，仅单测覆盖）；SMART 与温度（P04.3）；`go test -race`；
前端页面渲染（P04.4）。

**交付去向**：代码与证据已推送到 <https://github.com/clone668/uPanel>，代码提交
`85f5cedbeffbcde5c6c63394a1f6c4fb5c7a356b`（407 个文件 / +8901 −32，含两台 VM 的存储夹具），
本条记录随紧随其后的文档提交入库。证据目录 `docs/evidence/P04/2026-10-20-p04.2/` 含宿主闸门输出
（`check.txt`、`openapi-lint.txt`）、两台 VM 环境（`vm-2404-env.txt`、`vm-2204-env.txt`）、
二进制 sha256 对照（`cross-build-sha256.txt`、`vm-sha256-2404.txt`、`vm-sha256-2204.txt`）、
`runall-2404.txt`/`runall-2204.txt`、`p04-2-2404.txt`/`p04-2-2204.txt`、
`native-2404.txt`/`native-2204.txt`、`storage-regression-2404.txt`/`storage-regression-2204.txt`、
`commands.log` 与 `result.md`。新增的 `tests/integration/p04-2-storage-acceptance.sh`、
`tests/fixtures/storage-{2404,2204}/`、`scripts/capture-storage-fixtures.sh` 随代码入库，
换台机器重建一次验收环境就能一键重跑（步骤见 `commands.log`）。


### P04.3（2026-10-21）— SMART/NVMe 健康、diskstats 速率、不可达网络挂载隔离

**做了什么**

| 层 | 文件 | 内容 |
|---|---|---|
| 配置 | `internal/config/config.go`、`deploy/upanel.example.json` | 新增 `agent.storage`：`smart_enabled`（默认 true）、`smart_interval_minutes`（默认 15，**范围 5—60，越界是启动错误**）、`smartctl_path`（默认空=在 `/usr/sbin`、`/usr/bin`、`/sbin`、`/bin` 里找；显式写错不退回自动查找）、`network_capacity_timeout_seconds`（默认 10，范围 1—60） |
| 采集 | `internal/storage/smart.go`（新） | `smartctl -j -H -A -i` 的解析：`smart_status.passed`、NVMe 健康日志与 `critical_warning`、温度（`current` ≤ 0 视为没有读数 → `null`）、ATA 属性表。smartctl 退出状态是**位掩码**：1 命令行、2 打开失败/低功耗、4 SMART 命令失败、8 DISK FAILING、16 预失效当前、32 预失效历史、64 错误日志、128 自检日志；失效/预失效 → `critical`，历史位/日志位 → `warning`，命令失败/打开失败 → `unknown`。**不为读健康唤醒盘**：先读 sysfs 电源状态（`sleeping` 直接跳过，`source=sysfs`），非休眠的非 NVMe 再加 `-n standby` 让 smartctl 最后一刻放弃 |
| 采集 | `internal/storage/diskstats.go`（新） | `/proc/diskstats` 解析：每台设备**只取自己那一行**（分区/整盘/dm 从不相加），累计量转十进制字符串；速率由相邻两次采样差分，窗口为 0/负、超过采集间隔 3 倍、任一计数变小（内核重启、换盘、回绕、同名新设备）→ `rate=null`；`rate=0` 是真结论；平均延迟只在对应完成次数 > 0 时给，扇区固定按 512 字节 |
| 采集 | `internal/storage/capacity.go`、`capacity_linux.go`、`capacity_other.go`（新） | `statvfs` 容量（`f_bavail`）与**网络挂载隔离**：网络文件系统的容量在代理自己的 `--statfs-helper <绝对路径>` 子进程里查，并发上限 2、超时 1—60 秒（默认 10），超时/报错后该挂载点**隔离 5 分钟不再重试**，`capacity=null`、`reachable=false`、`capacity_error` 写明“超时”“多久之后才重试” |
| 采集 | `internal/storage/collector.go` | 拓扑仍是 5 秒一循环；健康采集是**独立 goroutine**（15 分钟一次），慢盘只拖住它自己那次探测；容量 30 秒缓存 + 网络挂载目标表（设备挂载点与 `network_mounts` 共用一套机制）；`io` 在 `snapshot_version` 计算**之后**才挂上去 |
| 采集 | `internal/storage/types.go` | `Device.io`（`sampled_at`/累计计数/`rate`）、`Health`（`status`/`source`/`temperature_celsius`/`checked_at`/`details`）、`Topology.network_mounts`（`path`/`source`/`fstype`/`read_only`/`reachable`/`capacity`/`capacity_error`）、`Mount.capacity_error` |
| 采集 | `internal/storage/scan.go` | `selectNetworkMounts`：没有块设备支撑 + `fstype` 属于网络文件系统的挂载点（tmpfs/proc/cgroup 不进）单独成表，按 path 去重排序；身份字段进 `snapshot_version`，容量/可达性不进 |
| 代理 | `cmd/upanel-agent/main.go` | `--statfs-helper <绝对路径>` 模式（只 statfs、输出一行 JSON、参数不是恰好一个绝对路径退出码 2）；`agent.storage` 接线；健康采集随拓扑启动 |
| 契约 | `docs/CONTRACTS.md` §6.2、`api/openapi.yaml` | io/health 的取值与位掩码、`network_mounts` 的语义（`reachable` 三值：true 读到、false 确定失败、null 还没查过）、`capacity_error`；openapi 新增 `StorageNetworkMount`，`StorageTopology.required` 加 `network_mounts`，`info.version: 0.0.0-p04.3` |
| 测试 | `internal/storage/{smart,diskstats,capacity,collector_p043,network_mount}_test.go`（新）、`collector_test.go`、`scan_test.go` | 真实采集夹具（VM-E 的 NVMe/SCSI `smartctl -j`、三台 VM 的 `/proc/diskstats`）+ 合成输入覆盖位掩码、坏 JSON、空数组、丢失字段；`sleeping` 的两段式探测只有单测（见 NOT RUN） |
| 夹具 | `tests/fixtures/smart/`、`tests/fixtures/diskstats-{2204,2404,smart-2404}.txt`、`PROVENANCE.md` | 都是**在验收 VM 上原样采集**的，采集命令与日期写在 PROVENANCE |
| 验收 | `tests/integration/p04-3-storage-health-acceptance.sh`（新，1058 行） | 16 段、三台 VM 上分别 77/77/62 项断言，root 运行；`run-all.sh` 注册为第 14 个脚本（整脚本约 20 分钟，`WAIT_QUARANTINE=0` 可跳过 5 分钟隔离期并把该项记 NOT RUN） |

**接口与数据变更**：没有新的 HTTP 路由、没有数据库迁移、没有新写动作，生产动作登记表仍为空。
`GET /api/v1/storage/topology` 的响应多了 `network_mounts`，`/storage/*` 的节点多了 `io` 与
`filesystem.mounts[].capacity_error`；配置多了 `agent.storage.*`。`--statfs-helper` 是**进程内部模式**
（只接受一个绝对路径、只做 statfs），不是命令执行入口，也不在代理的动作/查询登记表里。

**没有引入的东西**（本轮刻意）：没有 `smartctl` 以外的外部命令；没有为读健康唤醒休眠盘；
没有把分区与整盘的 I/O 相加；没有在 SMART 缺失时编造 `healthy`；没有把网络挂载塞进设备列表
（它们没有块设备）；没有做任何写操作。

**本轮修掉的问题**：Q101（网络挂载在拓扑里根本不可见 → 新增 `network_mounts`）、
Q102（验收要等结论而不是等固定秒数）、Q103（`cmd/upanel-web` 的用例会读机器上的
`/etc/upanel/upanel.json`，测试结果取决于环境）；另修 `smart_test.go` 与 `CONTRACTS.md` 的 CRLF 行尾。


### P04.3 验收结果（2026-10-21，验收回填）

**结论：passed。** 代码 + 宿主闸门 + 三台 VM 验收完成，证据在
`docs/evidence/P04/2026-10-21-p04.3/`，详见其中的 `result.md`。

| 检查 | 24.04.5 / 6.8.0-147 | 22.04.5 / 5.15.0-198 | 24.04.5 + QEMU NVMe（upanel-smart） |
|---|---|---|---|
| `p04-3-storage-health-acceptance.sh` | 通过 **77** / 失败 0 / 未执行 1 | 通过 **77** / 失败 0 / 未执行 1 | 通过 **62** / 失败 0 / 未执行 2 |
| 未执行项 | NVMe 真实 healthy（本机只有 SCSI 盘） | 同左 | 网络挂载隔离、NFS 卡住时登录（本机无 `nfs-kernel-server`） |
| 原生 `storage.test` | PASS **103** / FAIL 0 | PASS **103** / FAIL 0 | PASS **103** / FAIL 0 |

宿主：`go build ./...`、`go vet ./...`、`gofmt -l .` 干净；`go test ./... -count=1 -v` 计
**568** 个顶层 `--- PASS`（含子测试 744）/ 0 FAIL / 6 SKIP / 20 包 ok；
`sh scripts/check.sh` **PASS 12 / FAIL 0 / NOT RUN 0**，`redocly lint` valid（9 warnings）。
竞态：VM（22.04，gcc 11.4）内 `go test -race -count=1 ./...` → 19 包 ok、**0 条 `WARNING: DATA RACE`**，
唯一失败是 `TestDefaultPasswordParamsTiming`（`-race` 把 Argon2id 单次校验放大到 1.79—2.03 秒，
用例上界 1 秒；不带 `-race` 时 0.6 秒通过）。这次是在**装过面板的机器**上跑的，`cmd/upanel-web` 全绿，
反证 Q103 的修复有效。

**p04-3 脚本验到的行为**（16 段，全部黑盒）

1. 配置：3/61 分钟的 SMART 间隔、0/61 秒的网络超时、相对路径或带 `?` 的 `smartctl_path` 都拒绝启动并指出范围。
2. 代理启动：socket、`uid=0`、10 秒内应答；`capabilities` 仍不报任何写动作。
3. health 闭集与来源；分区/loop/rom 恒 `unsupported` 且带理由；全表没有 0 °C。
4. **与机器自己的 `smartctl` 逐盘交叉验证**：脚本另跑一次 `smartctl -j -H -A -i`，比 `smart_status.passed`、温度、NVMe `critical_warning`。
   `upanel-smart` 上 NVMe 是真实 `healthy` 且温度 > 0；两台只有 SCSI 盘的 VM 上 4 块盘全是 `unknown` 且理由一致（`smart_support.available=false`）。
5. 节奏：13 秒内 `collected_at` 前进、`checked_at` 不变，期间没有任何 `smartctl` 进程被拉起。
6. diskstats：先等到快照的 `sampled_at` ≥ 本机 t0，再在前后各读一次 `/proc/diskstats`，逐字段落在区间内且是十进制字符串。
7. 速率：第二次采样后出现；`null` 与 `0` 的语义在单测里钉住（归零/回绕/窗口异常）。
8. `snapshot_version`：相隔 11 秒两次取快照版本相同，而期间计数确实变了（io 不参与指纹）。
9. 网络挂载：NFS 停服后容量在缓存过期后变 `null`、`reachable=false`、`capacity_error` 说明超时与“不再重试”；
   5 秒扫描不受影响；整个停服期间最多 1 个 statfs 子进程。
10. 登录不受采集拖累：**在 `statfs` 挂在内核里的那一刻**掐表登录，2404 **332 ms**、2204 **271 ms** 返回 200，
    同一时刻该挂载仍是 `reachable=false`（快不是因为卡点过去了）。
11. 隔离到期（5 分钟）后服务恢复，容量自动回来。
12. `--statfs-helper` 的边界：空、相对路径、两个参数、带空格都退出码 2。
13. Web 透传：未登录 401、admin/viewer 200（V），io 字段可为 null、字节字段是字符串、health 未被改写，
    拓扑里 `network_mounts` 是数组且字段完整。
14. 没有 smartctl（`smartctl_path=/nonexistent/...`）时物理盘 `unknown` 且带说明，分区仍 `unsupported`，恢复配置后代理正常重启。
15. 重复 `init-admin` 被拒绝（不覆盖已有管理员）。
16. 日志卫生：无 `sh -c` 痕迹、无私钥泄漏、无 panic。

**证明“测试有牙”**：温度断言排除了 0 °C（QEMU SCSI 真的会输出 `temperature.current = 0`）；
diskstats 的区间对账是真读 `/proc/diskstats` 而不是复用被测输出；网络挂载那一段是**真的把 NFS 服务停掉**，
让 `statfs` 在内核里挂住，再用“最多 1 个 statfs 子进程”和“登录 271—332 毫秒”证明隔离生效；
`smartctl` 交叉验证是拿系统工具的输出与被测代码对账，不是拿被测代码验证被测代码。

**NOT RUN**：真实生产服务器；休眠盘不唤醒的真机行为（QEMU 的 SCSI/NVMe 进不了 standby，
`smartctl -s standby,now` 之后 sysfs 仍是 `running`，只有单测）；ATA/SATA 的真实 `smartctl` 输出
（`-device ide-hd` 在这套 QEMU 上让 guest 卡在固件阶段）；NVMe 真实健康在只有 SCSI 盘的两台 VM 上不适用
（由 `upanel-smart` 覆盖）；网络挂载隔离在 `upanel-smart` 上不适用（无 NFS 服务端，由另两台覆盖）；
真实 SATA/USB/RAID/LVM 拓扑；前端页面渲染（P04.4）。

**环境事故（不记作产品缺陷）**：`upanel-2404` 与 `upanel-smart` 本轮各 panic 一次
（`Attempted to kill init! exitcode=0x0000008b`，与之前几次同款）。重启后 `/tmp` 被清空，
二进制/脚本/夹具全部重传并核 sha256（三台 VM 上三个入口二进制哈希逐个相同），再从干净状态重跑。

**交付去向**：代码与证据已推送到 <https://github.com/clone668/uPanel>，提交
`9a1c089c27218ee1ab8c96e1df8c391095aa8ef1`（54 个文件 / +8912 −128），本条记录随紧随其后的文档提交入库。
证据目录 `docs/evidence/P04/2026-10-21-p04.3/` 含宿主闸门（`host-gates.txt`、`go-test.txt`、
`go-test-verbose.txt`、`check.txt`）、三台 VM 环境与验收输出（`env-*.txt`、`p04-3-*.txt`）、
原生 `storage.test`（`native-storage-*.txt`）、`-race` 两轮（`race-2204.txt` 为被污染的首次运行、
`race-final-2204.txt` 为修复 Q103 后的复跑）、`commands.log` 与 `result.md`。
浏览器渲染仍无证据：页面属 P04.4。


### P04.3 补强（2026-10-21，第二轮）

**为什么补**：passed 之后按 `STAGES.md` §6 的夹具与用例清单逐条回对，发现三处“文档写了、代码与证据
没盖住”的地方——①拓扑里没有一块 **USB 盘**的真实 `smartctl` 输出；②`/proc/self/mountinfo` 里
“同一设备两个挂载点”没有用例（`scan.go` 的 mount 去重/排序没人守）；③USB 桥不认识时
`smartctl` 自己的诊断被吞掉，`details` 只剩“退出状态位 0”，运维看不出为什么没读到。
①②是证据/测试缺口，③是去抓真实 USB 输出时才撞上的**产品缺陷**（不是先想到再补的测试）。

| 文件 | 改动 |
|---|---|
| `internal/storage/smart.go` | 新增 `smartctlMessage()`：取 `smartctl.messages` 里 `severity=error` 的那条（没有就取第一条非空的）；位 0 分支、打开失败兜底、最终 `unknown` 兜底都改成“中文原因 + `：` + smartctl 原话” |
| `internal/storage/smart_test.go` | 新增两个真实捕获用例：USB 桥不认识（exit 1 → `unknown`、`details` 含 `Unknown USB bridge`、温度 `nil`）、`-d sat` 打不开（exit 2 → `unknown`、含“无法打开设备”、温度 `nil`）；位 0 的断言文案改成“退出状态位 0”（位 0 也覆盖“设备类型无法判定”，只写“命令行被拒绝”会解释错） |
| `tests/fixtures/smart/upanel-smart-usb-{unknown-bridge,sat}.json` | VM-E 上按产品自己的参数原样采集（`a47cc17b…` / `759d891b…`），命令与 sha256 写进 `PROVENANCE.md` |
| `internal/storage/scan_test.go` | 新增 `TestDeviceWithTwoMountPointsKeepsBoth`：真实 `mount --bind` 的 `mountinfo` 行（`85 28 8:1 … /dev/sda1`，与 `/` 同为 `8:1`）→ 两个 `Mount` 条目都保留、按 path 排序、`sda` 仍是 `Protection.SystemDisk` |
| `tests/integration/p04-3-storage-health-acceptance.sh` | 头注释加第 14/15 条；交叉验证加反向规则“API 说 `healthy` 而 `smartctl` 没给通过结论 → 判失败”；新增 USB 可见性断言（`transport=usb`，没有则记 NOT RUN） |
| `docs/CONTRACTS.md` §6.2 | `details` 必须带 `smartctl.messages` 原话（优先 `severity=error`）；USB 桥类磁盘只能是 `unknown`，不是 `unsupported` 也不是 `healthy` |
| `docs/DECISIONS.md` Q104 | 记录判据：`healthy` 必须由 smartctl 自己的通过结论背书（`smart_status.passed=true` 或 NVMe 健康日志），否则 `unknown` |
| `docs/TEST_ENVIRONMENT.md` | VM-E 磁盘布局补 `sdb`（`-WithUsbDisk` 才有的 2 GiB USB 盘）与真实输出说明 |

**产品行为的变化**（VM-E 真实响应，`docs/evidence/P04/2026-10-21-p04.3/usb-sdb-and-nvme-health.jsonl`）：
补强前同一块 U 盘是 `unknown`，理由止于“退出状态位 0”；现在是
`…本次结果不可信：/dev/sdb: Unknown USB bridge [0x46f4:0x0001 (0x000)]`。
同一份响应里 `nvme0n1` 真实 `healthy` + 温度 50、`sda` 是 `unsupported` + `smart_support.available=false`，
三种结论各自有据。

**本轮验收（三台 VM 全部冷启动后重跑，二进制与脚本先核 sha256：三台上 `upanel-web 639293af…`、
`upanel-agent b417e53f…`、`upanelctl c81c73eb…`、`storage.test ea3b2e8f…`、脚本 `6807eb3b…` 逐个相同）**

| 检查 | 24.04.5 / 6.8.0-147 | 22.04.5 / 5.15.0-198 | 24.04.5 + QEMU NVMe + QEMU USB |
|---|---|---|---|
| 验收脚本 | 通过 **77** / 失败 0 / 未执行 2 | 通过 **77** / 失败 0 / 未执行 2 | 通过 **63** / 失败 0 / 未执行 2 |
| 未执行项 | USB 盘在拓扑里可见、NVMe 真实 healthy | 同左 | 网络挂载隔离实验、NFS 卡住时登录（无 `nfs-kernel-server`） |
| 原生 `storage.test` | 顶层 **106** / FAIL 0 | **106** / FAIL 0 | **106** / FAIL 0 |

VM-E 从 62 涨到 63，多的就是“USB 盘在拓扑里可见（transport=usb：sdb），并纳入逐盘对账”，
且 `API 的健康结论与 smartctl 自身输出逐盘一致` 仍然 PASS。宿主：`go build`/`go vet`/`gofmt -l .` 干净；
`go test ./... -v` **571** 个顶层 PASS（含子测试 176，合计 747）/ **0 FAIL** / 6 SKIP；
`sh scripts/check.sh` **PASS 12 / FAIL 0 / NOT RUN 0**。竞态：VM-2204 以 root 重跑
`go test -race ./...` → **19 包 ok、0 条 `WARNING: DATA RACE`**，唯一失败仍是
`TestDefaultPasswordParamsTiming`（`-race` 下 Argon2id 单次 2.39 秒 vs 用例上界 1 秒）。

**顺带修掉的记录错误**：§1 的状态行原写“P04.3/P04.4 未开始”，与 §3 的“P04.3 passed”自相矛盾，
改为 P04.3 已 passed、P04.4 未开始。

**环境事故（不记作产品缺陷）**：`upanel-2404` 与 `upanel-2204` 执行 `reboot` 后**在 WHPX 上挂死**
（console 停在 `reboot: Restarting system`、QEMU 进程与端口转发都在但不引导、SSH banner 超时），
冷启动后立刻正常。6.8.0-147 的两个 guest 当天各 panic 过：形态一致——
`Kernel panic - not syncing: Attempted to kill init! exitcode=0x0000008b`、`PID: 1 Comm: systemd`、
前面没有 `BUG:`/`Oops`，即 PID 1 自己吃了 SIGSEGV。`upanel-smart` 在 uptime 2738 s 时（console 存为
`console.panic-2026-10-21.log`），`upanel-2404` 在 uptime 2802 s 时**正好撞在本轮重跑期间**
（日志截断，console 存为 `console.panic-4-2026-10-21.log`，冷启动后重跑才拿到有效结果）；
5.15.0-198 的 `upanel-2204` 当天没 panic 过。两次都在 45—47 分钟附近，但 `upanel-smart` 之后连续跑了
50 分钟以上不复发，所以不是定时器，只能记成“同一族偶发”。

**NOT RUN（本轮仍未验证）**：真实 U 盘（桥被 smartctl 认识、能读 SMART 与温度）——这轮拿到的是两个
真实**反例**；睡眠盘不唤醒的真机行为（QEMU 进不了 standby）；ATA/SATA 真实输出（`-device ide-hd`
让 guest 卡在固件阶段）；真实 SATA/USB-RAID/LVM 拓扑；真实生产服务器；前端渲染（P04.4）。
详见 `docs/evidence/P04/2026-10-21-p04.3/hardening.md`。

**交付去向**：补强提交 `3591cfd0c5aeb5932d3bace33bef1323388848f4`（23 个文件 / +3574 −10）
已推送到 <https://github.com/clone668/uPanel>；本条记录随紧随其后的文档提交入库。
新增证据文件：`p04-3-{2404,2204,smart}-h2.txt`、`native-storage-{2404,2204,smart}-h2.txt`、
`race-h2-2204.txt`、`host-gates-h2.txt`、`go-test-verbose-h2.txt`、`check-h2.txt`、
`usb-sdb-and-nvme-health.jsonl`、`hardening.md`。


### P04.4（2026-10-21）— 首页、磁盘列表、拓扑/分区详情、健康与容量卡片（只读）

**范围**：`STAGES.md` §6 P04.4。**写操作能力此时不启用**——`capabilities` 仍是空数组，界面上没有一个
动作按钮，只在空位写“当前阶段没有可执行的动作”，绝不预告 P07 才登记的能力。服务端代码本轮
**零改动**：这一阶段全部是浏览器里的呈现与验证，接口与 DTO 沿用 P04.1—P04.3 已验过的那一套。

| 文件 | 改动 |
|---|---|
| `web/src/views/OverviewView.vue`、`StorageView.vue`（新）、`StorageDeviceView.vue`（新） | 概览（主机/CPU/内存/负载/网络 + 磁盘概要）、存储（整盘与分区、loop 背景区、不可达网络挂载）、设备详情；每页都有加载 / 空 / 错误 / 依赖未安装四种状态，采集时间与刷新节奏照实显示 |
| `web/src/components/{SystemOverviewPanel,StatTile,CapacityBar,HealthBadge,DeviceCard,IoStats,DeviceDetailPanel}.vue`（新） | 纯展示，只接收 DTO，不自己造数据；字节走十进制字符串 + `BigInt`，`null` 一律渲染成 `—`（不写成 0、不写成“未知 0 B”）；七状态里只有 `warning/critical` 会挂告警样式 |
| `web/src/api/system.ts`、`storage.ts`（新） | 与 `api/openapi.yaml` 同源的 DTO 与闭集标签；`healthNeedsAttention()` 只对 `warning/critical` 为真 |
| `web/src/utils/{format,polling,viewstate}.ts`（新） | 字节/百分比/速率格式化（K/M/G 与精确字节分开）；轮询只有一份实现——按服务端 `interval_ms` 调度、不叠请求、页面隐藏时停、重新可见立刻补一次；错误码 → 页面状态的唯一映射（不看文案） |
| `web/src/router/routes.ts`、`modules/registry.ts`、`components/AppShell.vue` | `/storage`、`/storage/devices/:id([A-Za-z0-9._-]+)`（非法 id 直接落 404）；子页面高亮所属模块；概览与存储的模块说明从“未实现”改成已实现 |
| `api/openapi.yaml` | 版本 `0.0.0-p04.4`、`x-upanel-contract-status.stage: P04.4`，补登四个已实现路径；`StorageDevice.io` 进 `required`（P04.3 起就已返回、契约漏登记）；`health` 说明改为“由 SMART/NVMe 填写，读不到只能是 unknown/sleeping” |
| `web/src/{api,utils,components}/*.test.ts` | 16 个文件 / **192** 个用例。vitest 环境是 node，没有 jsdom，组件用 `vue/server-renderer` 的 `renderToString` 渲染纯函数式组件；数据获取在视图里，所以这里验的是“同一份 DTO → 同一段 DOM” |
| `web/src/fixtures/p04-4-vm/{summary,devices,topology,detail-sda}.json`、`README.md`（新） | VM 上抓的四份真实响应，**逐字节**保存（与 `docs/evidence/P04/2026-10-21-p04.4/payloads/` 相同，`sha256:fd6bdf50…`、`collected_at 2026-10-21T00:19:31Z`）；README 写明采集命令、同一快照规则、“不要手改”与 NOT RUN 边界 |
| `web/src/components/realdata.test.ts`（新，17 个用例） | 用上面四份真实载荷跑 SSR：四份响应自洽（同一 `snapshot_version`/`collected_at`、拓扑节点 = 设备 id、边端点存在）、系统概况照实、设备卡片照实（40 GiB 与精确字节、未挂载、系统盘受保护、`unsupported` 不挂告警、不出现 `0 °C`）、详情页照实（GPT、分区表读自磁盘、保护理由与来源、`capabilities` 为空 → “当前阶段没有可执行的动作”、页面上没有 `<button>`） |
| `web/src/components/storage.test.ts`（24 个用例） | 新增两条：两块容量完全相同的盘（`sdc`/`sdd` 同 4 GiB、`serial` 都 `null`）各自显示自己的名字/序列号/稳定标识前缀，不互相顶替；dm/LVM 盘与其下分区各显示自己的 931.51 GiB，界面上不出现 1.82 TiB |
| `web/scripts/smoke.mjs`（重写）、`tests/smoke/p04-4-ui-smoke.sh`（新） | 真浏览器（`@playwright/test` 自带 chromium，没引入新框架）：未登录 `/` 与 `/storage` 都落到登录页、登录后回原地址、导航只列已实现模块、概览页与存储页逐段断言、依赖不可用时显示“依赖未安装”而不是 0、深链接 `/storage/devices/sda` 不落 404 而显示依赖未安装、未知路径 404 且外壳完整、无未捕获异常、无多余失败请求 |
| `scripts/check.sh` | 新增 `web-smoke` 一步（13 步）：找不到 Playwright 浏览器就 `SKIP` 并给出 `npx playwright install chromium`，不假装通过 |
| `tests/integration/p04-4-ui-acceptance.sh`、`tests/integration/p04-4-payload-validate.py`（新） | 真 Ubuntu 上起 root 代理 + **非 root** Web，抓四份真实响应；先等首次 SMART 采集落地；按契约 + `lsblk -P`/`df -B1`/`smartctl -j -i -H -A`/`/proc/diskstats` 逐项交叉校验 |
| `docs/DECISIONS.md` Q105 / Q106 | Q105：验收必须先等 `checked_at`，且必须用**产品自己的** `smartctl` 参数做对照（`available=false → unsupported` 是断言，不是豁免）；Q106：`unknown/unsupported` 不挂“健康未通过”，测试断言“健康判定的解释文案不存在”，而不是断言“正常”二字不存在 |

**页面里刻意不做的事**：不摆任何动作按钮（写操作 P07 才有）；`unsupported/unknown` 不用红色，
只说明理由；容量与百分比不做四舍五入美化（40 GiB 的盘就写 40 GiB，精确字节放 `title`）；
`null` 不当 0——速率 `null` 时写“没有可用采样窗口，不是 0”，温度 `null` 写 `—`；
不接受任何设备 id 之外的参数拼进接口路径。

**本轮验收**

| 检查 | 结果 | 证据 |
|---|---|---|
| VM 载荷校验（契约 + `lsblk`/`df`/`smartctl`/`diskstats`） | **PASS=856 FAIL=0** | `docs/evidence/P04/2026-10-21-p04.4/p04-4-acceptance-2404.txt` |
| VM 验收脚本自身断言（身份、socket、抓取、遍历式 id、日志卫生） | **PASS=31 / FAIL=0 / NOT RUN=0** | 同上 |
| 宿主 `sh scripts/check.sh` | **PASS 13 / FAIL 0 / NOT RUN 0** | `.../check.txt` |
| 前端 `vue-tsc -b` / `eslint` / `vitest run` | 0 error / 0 problem / **192 passed（16 文件）** | `.../web-gates.txt`、`.../check.txt` |
| 页面冒烟：真实 `upanel-web --mode mock` + Playwright（已登录） | **PASS 31 / FAIL 0 / SKIP 5**（SKIP 全是“mock 二进制没有 agent socket，没有设备可渲染”，如实记 SKIP 而不是通过） | `.../p04-4-ui-smoke.txt` |
| 页面冒烟：`dist` 预览（未登录守卫） | **PASS 10 / FAIL 0 / SKIP 0** | `.../check.txt` 的 `web-smoke` 段 |
| 契约 lint（redocly） | valid，9 warning（未使用组件 7 + 4xx 说明 2，历史遗留、与上一阶段同量） | `.../check.txt` |

VM 上的关键断言（可逐条对照）：监听 19448 的进程是 `upanel-web`（**uid 999**），代理是 root，
`agent.sock` 为 `root:upanel-web` 且非 0666；四份响应结构与闭集一致、字节全是十进制字符串、
未知值一律 `null`；`lsblk -P` 的 17 台块设备与 DTO 一一对应；`/`、`/boot`、`/boot/efi` 容量与
`df -B1` 相同；16 台设备的 `io.reads_total` 都不超过 `/proc/diskstats` 对应行；4 块盘的 `health`
与 `smartctl -j -i -H -A` 的结论一致（`smart_support.available=false → unsupported`，`checked_at` 非空）；
`/storage/devices/..%2F..%2Fetc%2Fshadow` 返回 404 且响应体里没有 `root:`；`web.log`/`agent.log`
里没有管理员明文密码。

**本轮发现并修掉的问题**

1. **界面把“未知/不支持”说成“健康未通过”**（产品缺陷）：`DeviceCard` 原先对 `unknown` 与
   `unsupported` 都挂“健康未通过”，于是每一个分区、每一个 loop 设备都在报警。改成只对
   `warning/critical` 挂“健康需要处理”，并补了断言（Q106）。
2. **验收脚本抓得太早**（harness 缺陷）：首次健康探测在独立 goroutine 里，脚本启动 Web 后立刻抓，
   拿到的是 `"SMART 采集尚未完成"` 的占位，整块健康校验空转；旧日志还把 SKIP 原因写成
   “本机没有能被 smartctl 读到的盘”，不是真实原因。现在先等所有整盘都有 `checked_at`
   （`HEALTH_WAIT`，默认 120 秒，**实测 5 秒**），超时如实记 NOT RUN。
3. **健康对照两边参数不一致**（harness 缺陷）：旧校验用 `smartctl -H -j`，产品用 `-j -i -H -A -n standby`。
   同一块 QEMU 盘，前者 `smart_status.passed=true`，后者 `smart_support.available=false`、没有
   `smart_status`——按旧写法会把“产品正确判为不支持”误报成失败。现在用产品同一组参数，
   并把 `available=false → unsupported` 变成断言（4 块盘全部 PASS，见 Q105）。

**顺带补齐的记录**：`StorageDevice.io` 从 P04.3 起就已在响应里，契约一直漏登记 `required`，
本轮补上（前端也因此能在类型层面拿到 `io`，而不是当可选字段猜）。

**NOT RUN（未验证，含原因）**

- **VM 上的浏览器渲染**：验收 VM 没装浏览器，像素级证据只有宿主上的 Playwright 冒烟
  （真实 `upanel-web --mode mock`，已登录，31 项）。VM 上只证明“前端拿到的数据是真的”。
- **真机 SATA / NVMe / USB / 睡眠盘的健康显示**：这台 VM 的盘全是 QEMU 虚拟盘，`smartctl` 说它们
  不支持 SMART，所以真实数据里只有 `unsupported` 一条分支；`healthy`/`warning` 的显示由合成夹具
  覆盖，真实 `healthy` 的采集证据在 P04.3 的 `upanel-smart` 验收里（不属于本阶段）。
- **真实 LVM/RAID/dm 拓扑**：“多层不重复累计”只有合成用例；验收 VM 单盘 + 三个空盘，没有 dm/md 设备
  （P00-VM-C 仍未创建，见 §7 索引表末尾两行）。
- **写操作**：按计划不启用，界面上没有按钮、`capabilities` 恒空——这是本阶段的验收条件，不是未完成项。
- **`viewer` 角色看这些页面**：本轮只用 `admin` 登录；读取接口对两种角色都开放，
  没有单独的“无权限”界面可验。

**环境事故（不记作产品缺陷）**：`upanel-2404` 在 uptime 3096 s 时再次
`Kernel panic - not syncing: Attempted to kill init! exitcode=0x0000008b`（PID 1 自己吃 SIGSEGV，
与 P04.3 记录的形态一致），`upanel-smart` 在 3200 s 时同样。冷启动后立即恢复，本轮数据是在冷启动后的
干净实例上采集的，console 存为 `console.panic-5-2026-10-21.log`。

**交付去向**：随本次提交入库（前端页面、组件、DTO、192 个用例、四份真实夹具、页面冒烟脚本、
VM 验收脚本与校验程序、`api/openapi.yaml` 补登、`check.sh` 的 `web-smoke`、Q105/Q106、§8.7 测试环境）。
证据目录 `docs/evidence/P04/2026-10-21-p04.4/`：`p04-4-acceptance-2404.txt`、`payloads/`（8 份响应）、
`vm-env-2404.txt`、`web-gates.txt`、`check.txt`、`p04-4-ui-smoke.txt`、`commands.log`、`result.md`。
`result.md` 里有逐条对应 STAGES §6 P04.4 的验收条目表、三个缺陷的修法与复核命令。


### P05.1（2026-10-21）— 服务列表、详情、保护名单与受控的启停/重启/开机启动

**范围**：`STAGES.md` §7 P05.1。这一阶段第一次让面板**真的改变系统状态**——但只走 P03 建成的
「预检 → 任务 → 幂等 → 读回验证 → 审计」链路，没有第二条执行路径，也没有任何“直接执行”的接口。

| 文件 | 改动 |
|---|---|
| `internal/services/{systemctl,types,collector,protection,fetch}.go`（新包） | 枚举（`list-unit-files` ∪ `list-units --all`，剔掉模板与别名）、按块解析 `systemctl show`、10 秒快照缓存、保护名单判定、`Snapshot`/`Unit` DTO 与状态版本 |
| `internal/services/control.go` | `AssessControl`（预检结论：受保护 / 不存在 / 状态冲突 / 不可启用，锁 `service:<unit>`，`identity_fingerprint` 用全 64 位 `sha256:`）与 `ExecuteControl`（执行前再查保护与状态，跑完读回验证） |
| `internal/agent/{servicesquery,servicesaction}.go`、`registry.go`、`systemquery.go` | 代理侧查询 `service.list`/`service.unit` 与**生产动作** `service.control`（登记表里第一次出现真实写动作） |
| `internal/api/services.go`（新）、`router.go`、`server.go` | `GET /api/v1/services`、`GET /api/v1/services/{unit}`；模板单元与别名单元在详情上一律 404 并说明该用什么名字 |
| `internal/audit/audit.go`、`internal/api/users.go` | `job_id` 真正落库（此前列存在但从没写过值），`JobIDFromTarget` 从 `job:` 目标推任务号 |
| `web/src/api/{services,preflights}.ts`（新）、`jobs.ts` | 与 openapi 同源的 DTO、封闭集合的中文标签、`canOperate`/`summarizeUnits`/`groupUnits`/`canSubmitPreflight` 判决函数（判决只读服务端给的 `capabilities`，不自己推） |
| `web/src/views/{ServicesView,ServiceUnitView}.vue`、`components/{ServiceUnitCard,ServiceUnitPanel,ServiceActionConfirm}.vue`（新） | 列表（摘要 + 分组 + 关键字过滤 + 加载/空/错误/无权限）、详情（事实面板：受保护分类与理由逐条、开机启动与运行状态分开写、能力为空时给出**具体**原因）、确认框（影响说明 → 预检 → 提交 → 轮询终态） |
| `api/openapi.yaml`、`docs/CONTRACTS.md` §4.3/§10.1、`docs/DECISIONS.md` | 版本 `0.0.0-p05.1`、`stage: P05.1`、补登两个已实现路径；`Job.result` 明确“没有结果就是 `{}`”；模板与别名的排除规则（附 `systemctl` 真实 stderr）、完整性校验规则（按 `Id` 或 `Names` 匹配、缺的逐个重试、仍缺就整次失败并保留上一份快照） |
| `internal/services/testdata/*`（7 个，systemd 255 真实字节） | `show` 三单元/单单元/not-found/别名/模板被拒的 stderr、`list-unit-files`、`list-units` |
| `web/src/fixtures/p05-1-vm/*`（15 个，VM 真实响应） | 列表快照、六种详情/预检、成功与失败任务、两种 404；`realdata-services.test.ts` 用它做 SSR 回放 |
| `tests/integration/p05-1-services-acceptance.sh`、`p05-1-payload-validate.py`（新） | 14 节 140 条断言 + 2.2 万条载荷断言，全程在真 systemd 上跑，跑完清理验收单元 |

**这一阶段刻意不做的事**：不给任何“直接执行”按钮（一律先预检）；不受保护单元一个按钮都不摆，
空位写清原因（受保护 / 模板 / not-found / 当前状态无动作）；不按名字前缀猜保护
（唯一例外是 `systemd-*` 整体计入核心组件，CONTRACTS §10.1 有明确记录）；不把 `systemctl` 的
退出码当业务结果（要读回状态并比对）；不把“列表非空”当通过（要与枚举逐项对照）。

**本轮验收**

| 检查 | 结果 | 证据 |
|---|---|---|
| VM 验收脚本自身断言（14 节） | **PASS=140 / FAIL=0 / NOT RUN=3** | `docs/evidence/P05/2026-10-21-p05.1/p05-1-acceptance-2404.txt` |
| VM 载荷校验（字段、闭集、null 语义、与 `systemctl` 对照） | **PASS=22369 / FAIL=0** | 同文件 §12 |
| 宿主 `sh scripts/check.sh` | **PASS 13 / FAIL 0 / NOT RUN 0** | `.../check.txt` |
| `go build` / `go vet` / `go test ./... -count=1` | 退出码 0 / 0 / **21 包全部 ok** | `.../go-gates.txt`、`.../go-test.txt` |
| 前端 `vue-tsc --noEmit` / `eslint .` | 0 error / 0 problem | `.../web-gates.txt` |
| 前端 `vitest run` | **252 passed（20 文件）**，其中真实载荷回放 22 例 | `.../web-gates.txt` |
| 前端 `vite build` | 成功（493 ms） | `.../web-gates.txt` |
| 页面冒烟（`dist` 预览，未登录守卫） | **PASS 10 / FAIL 0 / SKIP 0** | `.../check.txt` |
| 契约 lint（redocly） | valid，9 warning（未使用组件 7 + 4xx 说明 2，历史遗留） | `.../check.txt` |

VM 上的关键断言：监听 19449 的是 `upanel-web`（uid 999）、代理是 root、`agent.sock` 为
`root:upanel-web` 且非 0666；列表是 10 秒快照（连续两次 `collected_at` 不变，之后推进），
239 个单元与 `systemctl` 枚举逐项对照；25 个模板名与 20 个别名都不进列表，别名请求 404 要求用规范名，
模板请求 404 要求给实例；14 个抽样单元的状态与 `is-active`/`is-enabled` 相同；
`start`/`restart`/`enable`/`disable`/`stop` 五步都读回验证（`started_at` 从 `01:31:23Z` 前进到
`01:31:25Z`，`stop` 后 `main_pid` 变 `null`、能力只剩 `start`/`enable`）；重复 `start`、重复 `stop`、
未运行时 `restart`、无 `[Install]` 段的 `enable` 四种都由预检挡住且不产生任务；
`ExecStart` 不存在的单元让任务 `failed` 且 `result` 为 `{}`；viewer 能读不能写；
审计里有 `operation.preflight` 与 `job.submit`（`job_id` 非空）且没有明文口令。

**本轮发现并修掉的问题**

1. **282 个单元的机器上列表只有 7 行**（产品缺陷，只有真机能暴露）。`systemctl show` 遇到裸模板名
   `foo@.service` 会以 `Unit name ... is neither a valid invocation ID nor unit name.` 退出 1，
   并且**把这次调用里后面的名字一起丢掉**；旧代码只判“解析结果非空”，于是 7 个就当成功。
   现在枚举阶段剔掉模板，`Show` 分块执行，拿到的单元与请求的逐项核对，缺的单独重试，
   仍缺就把整次采集标记失败并保留上一份快照。真实字节入库，回归用例
   `TestParseRealMultiUnitOutput`、`TestShowFailsWhenChunkAborts`、`TestShowRetriesMissingNamesIndividually`。
2. **一个别名单元让整次采集失败**（产品缺陷）。`dbus-org.freedesktop.ModemManager1.service` 是
   `ModemManager.service` 的别名，`systemctl show` 只回 `Id=ModemManager.service`，按 `Id` 匹配就认为
   “请求的单元没回来”→ `status=error`、0 个单元。现在按 `Id` **和** `Names` 建索引，枚举阶段剔掉
   `UnitFileState=alias` 的条目（否则同一单元出现两行，别名那行 `enablement=alias`，等于从读侧绕过
   保护名单对账）。回归用例 `TestShowMatchesAliasByName`、`TestListUnitNamesExcludesAliases`。
3. **失败任务的 `result` 语义漂移**（契约与实现不一致）。`Job.result` 在 openapi 里是必填对象，
   定成“没有结果就是 `{}`，失败原因只在 `error` 里”，契约描述、验收脚本、载荷校验三处一起对齐。
4. **审计的 `job_id` 从来没写过**（产品缺陷）。列一直存在，`Record` 却没落值，“从审计回溯到任务”
   这条链是断的。现在写入列值（非任务事件仍为 NULL），验收 §11 的
   `job.submit AND job_id IS NOT NULL` 断言因此成立。

**NOT RUN（未验证，含原因）**

- **执行后未收敛**（`systemctl` 返回 0 但 15 秒内状态没变）：正常 systemd 上造不出这种单元，
  由 `internal/services/collector_test.go` 的 fake 与 `control` 的读回超时分支覆盖。
- **浏览器渲染**：VM 上没有浏览器，像素证据在宿主用**同一批载荷**回放
  （`web/src/components/realdata-services.test.ts`，22 例；夹具与本目录 `payloads/` 逐字节相同）。
- **面板自身作为 systemd 单元**：验收里两者是直接启动的进程，不是 unit；
  “停掉面板会断掉自己”这条判定由 `internal/services/types_test.go` 的 `TestProtect` 与
  `collector_test.go` 的 `TestExecuteControlRechecksProtection` 覆盖。

**环境事故（不记作产品缺陷）**：`upanel-2404` 在一次 P05.1 验收运行期间又一次
`Kernel panic - not syncing: Attempted to kill init! exitcode=0x0000008b`（uptime 2835 s，
console 存为 `console.panic-6-2026-10-21.log`），该次运行作废，重启后重传二进制与脚本再跑得到最终结果。

**交付去向**：随本次提交入库（`internal/services` 新包、代理动作、API、前端页面与 DTO、
真实字节夹具与 15 份真实响应、验收脚本与校验程序、openapi 补登、§8.8 测试环境说明）。
证据目录 `docs/evidence/P05/2026-10-21-p05.1/`：`result.md`、`commands.log`、`env.txt`、
`p05-1-acceptance-2404.txt`、`payloads/`（65 份响应）、`vm-logs-2404/`、`go-gates.txt`、`go-test.txt`、
`web-gates.txt`、`check.txt`。

### P05.2（2026-10-21）— 进程列表、PID+starttime 身份、TERM/KILL 分开的流程、禁止终止关键进程

**范围**：`STAGES.md` §7 P05.2。这一阶段把“改变系统状态”的第二个入口接进同一条链路，
并且第一次引入**身份**这个概念：只凭 PID 杀进程会被内核的 PID 重用咬到，所以身份是
`pid` + `start_time_ticks`（`/proc/<pid>/stat` 第 22 字段），指纹
`sha256("process/v1|<pid>|<start>")`，状态版本 `sha256("process-state/v1|<pid>|<start>")`
（刻意不含 CPU/内存/state）。

| 文件 | 改动 |
|---|---|
| `internal/processes/`（新包，18 文件 2631 行含测试） | `procfs.go` 逐字段读 `/proc/<pid>/{stat,status,cmdline,cgroup}`（不调 `ps`）；`collector.go` 5 秒快照、`MaxProcesses=2000`、PID 升序、采集失败保留上一份；`protection.go` 7 类固定保护（init / kernel_thread / self / ssh / systemd_core / network / other_module）；`sanitize.go` 命令摘要脱敏；`signal.go` 预检结论 + 执行 + 读回；`kill_unix.go`/`kill_other.go` 只保留 `kill(2)`，不拼 shell |
| `internal/processes/testdata/`（30 个真机字节） | PID 1、内核线程、sshd、sleep、带秘密参数命令的 `stat`/`status`/`cmdline`/`cgroup`，另加 `passwd.txt` |
| `internal/agent/{processesquery,processaction}.go`、`registry.go`、`preflight.go`、`systemquery.go`、`cmd/upanel-agent/main.go` | 查询 `processes.list`、生产动作 `process.signal`；登记仍按“依赖非 nil 才登记”，验收表只有 `acceptance.*` |
| `internal/api/processes.go`（新）、`router.go`、`server.go`、`cmd/upanel-web/main.go` | `GET /api/v1/processes`，**admin**（viewer 403 `FORBIDDEN`），只透传代理快照 |
| `web/src/api/processes.ts`（278 行）、`processes.test.ts`（12 例） | openapi 同源 DTO、闭集中文标签、`isExited`/`signalNeedsPhrase`/`signalParams`/`sameProcess`/`filterProcesses`/`summarizeProcesses`/`formatCPUPercent`；判决只读服务端 `capabilities` |
| `web/src/views/ProcessesView.vue`（596 行）、`components/ProcessSignalConfirm.vue`（217 行） | 列表（摘要 + 筛选 + 加载/空/错误/无权限，渲染上限 200）、确认卡（TERM 一套门槛、KILL 另一套；受保护、已被拒、过期三种形态；写明“不会自动升级”） |
| `web/src/fixtures/p05-2-vm/`（22 份 VM 真实响应）、`components/realdata-processes.test.ts`（31 例） | 与证据目录 `payloads/` 逐字节相同（`cmp` 核过），SSR 回放覆盖快照契约、规模、保护、筛选、预检与任务、错误信封、确认卡四种形态 |
| `tests/integration/p05-2-processes-acceptance.sh`（995 行，12 节）、`p05-2-payload-validate.py`（578 行）、`run-all.sh` | 真机验收与载荷校验；`run-all.sh` 把 p05-2 加进 root 脚本清单 |
| `api/openapi.yaml`、`docs/CONTRACTS.md` §10.2、`docs/DECISIONS.md` Q112—Q117 | 版本 `0.0.0-p05.2`、`stage: P05.2`、`GET /api/v1/processes`；契约写清身份、状态闭集、cpu/memory 的 null 语义、保护类别、阻塞项与告警项、TERM/KILL 门槛、不自动升级 |
| `.gitignore` | 加 `/.build/`（交叉编译与 `go test -c` 产物落在那里） |

**这一阶段刻意不做的事**：不接受按名字/正则/属主/前缀选目标，不做批量，不在列表接口上直接终止，
不提供任何“万能命令”入口；TERM 超时**不自动升级**为 KILL（那等于把普通动作偷偷变成强制动作，
还绕过 KILL 的再认证与短语）；受保护进程与 `zombie`/`dead` 的 `capabilities` 恒为空数组，
前端只按它渲染，不自己推“这个应该能杀”；命令摘要只用于识别，不做审计全文。

**本轮验收**

| 检查 | 结果 | 证据 |
|---|---|---|
| VM 验收脚本自身断言（12 节） | **PASS=128 / FAIL=0 / NOT RUN=0** | `docs/evidence/P05/2026-10-21-p05.2/p05-2-acceptance-2404.txt` |
| VM 载荷校验（字段、闭集、null 语义、与 `/proc` 对照） | **PASS=9250 / FAIL=0 / SKIP=1** | 同文件 §11（SKIP 是“高 CPU 进程校验时已被清理”，写明原因） |
| 宿主 `sh scripts/check.sh` | **PASS 13 / FAIL 0 / NOT RUN 0** | `.../check.txt` |
| `go build` / `go vet` / `go test ./... -count=1` | 退出码 0 / 0 / **22 包全部 ok** | `.../go-gates.txt`、`.../go-test.txt` |
| 前端 `vue-tsc --noEmit` / `eslint .` | 0 error / 0 problem | `.../web-gates.txt` |
| 前端 `vitest run` | **295 passed（22 文件）**，其中进程真实载荷回放 31 例 | `.../web-gates.txt` |
| 前端 `vite build` | 成功（522 ms） | `.../web-gates.txt` |
| 契约 lint（redocly） | valid，9 warning（历史遗留） | `.../check.txt` |
| **跨阶段定向回归**（p01-1、p02-3、p03-1、p03-2、p04-1、p04-2、p04-3、p05-1） | 全部 0 失败：30/87/93/102/72/110/77（+3 NOT RUN）/140 | `.../regression-p05-2.txt` |

VM 上的关键断言：监听 19450 的是 `upanel-web`（uid 999）、代理是 root、`agent.sock` 为
`root:upanel-web` 且非 0666；第一份快照 137 个进程（sleeping 66 + idle 71）、111 个受保护、
26 个可操作，`interval_ms=5000` 且相邻两份相隔 5000 ms；僵尸（pid 4936）与内核线程的
`memory_bytes` 为 `null`，`yes`（pid 5041）`cpu_percent=100`，新出现的进程第一份快照里 `cpu_percent`
是 `null` 而不是 `0`；TERM 不要再认证、不要短语、锁是 `process:<pid>:<starttime>`，任务 `succeeded`
且进程真的消失；KILL 要求 5 分钟内再认证加一次性短语，没再认证（403）与短语错（400）时
**pid 5276 没有被碰过**，预检没被消费，重复提交 409 `PREFLIGHT_STALE`；PID 1 的 TERM、
内核线程、代理自身、SSH 的预检全部 `allowed=false`，被拒的 KILL 预检**仍带**再认证与短语，
提交 409 `DEPENDENCY_BLOCKED` 且预检未消费，四个进程都还活着；PID 重用用
`/proc/sys/kernel/ns_last_pid` 强制造出（pid 5463，starttime 30039 → 30049），拿旧预检提交 409
`DEVICE_IDENTITY_CHANGED`，**被重用的 PID 上的新进程毫发无伤**，按新身份重新预检才允许；
viewer 读列表与预检都是 403；审计里有对应的 `operation.preflight` 与 `job.submit`（`job_id` 非空）。

**本轮发现并修掉的问题**

1. **验收读数错位**（8 条脚本断言 + 3 条载荷断言，第一轮）：列表是 5 秒缓存快照，脚本起进程后
   立刻断言“在列表里”、杀掉后立刻断言“不在”，落在两次采集之间。改成 `wait_in_snapshot`
   （每 0.5 秒取一次，直到目标出现且 `cpu_percent` 有基线，上限 30 秒）；僵尸的父进程存活从 8 秒
   提到 20 秒；忙进程 CPU 允许目标已被清理时记 **SKIP** 并写明原因（记 PASS 就是编证据）。见 Q117。
2. **两条断言本身写错了**（同轮改对，不是放宽语义）：被拒的 KILL 预检**仍然**带再认证与短语
   （Q114）；viewer 读进程列表是 **403** 而不是 200（Q115）。
3. **五处跨阶段陈旧断言**（定向回归发现）：P05.1/P05.2 登记了新查询与新动作，`p01-1`、`p02-3`、
   `p04-1`、`p04-2`、`p04-3` 里“写动作为空”“查询只有两项”的断言失效。按 Q86 改成断言**完整闭集**
   `['processes.list','services.list','services.unit','storage.topology','system.summary']` /
   `['process.signal','service.control']`，并做成脚本顶部的常量，下一阶段加登记项时改一行即可。
   没有删除、没有放宽。`p04-2` 另有一处 `health.status 一律 unknown` 在 P04.3 引入 `unsupported` 后
   不成立，改成三条更严格的不变量（闭集、非物理盘必须 `unsupported`、没检查过的物理盘必须 `unknown`）。
4. **VM 上传布局（不是产品缺陷）**：`p02-3` 按 `DEPLOY_DIR="$ROOT/deploy"` 找模板而 `deploy/` 被放在
   `/tmp/deploy`；`agent.test` 缺 `tests/fixtures/`。补齐后 p02-3 的 45 条失败清零（32→87 通过）。

**NOT RUN（未验证，含原因）**

- **浏览器渲染**：VM 上没有浏览器。页面级证据在宿主用**同一批载荷**做 SSR 回放
  （`realdata-processes.test.ts`，31 例），夹具与 `payloads/` 逐字节相同。
- **TERM 超时（进程忽略 SIGTERM）与 KILL 后仍存在（D 状态）**：正常内核上造不出可控的
  “忽略 TERM 且不退出”与“停在不可中断睡眠”的进程，这两条只由 `internal/processes/signal_test.go`
  的 fake 覆盖。
- **结束日志订阅不残留 journalctl 子进程**：属 P05.3。
- **多机/异构内核**：本轮只有 `upanel-2404` 一台，PID 重用、保护名单、竞态都在这台上验证。

**环境事故（不记作产品缺陷）**：`upanel-2404` 至今 7 次 `Kernel panic - not syncing: Attempted to
kill init! exitcode=0x0000008b`（`CPU: 1 PID: 1 Comm: systemd`），本轮之前最近一次发生在 P05.2
验收途中；已排除镜像共享与宿主内存（同期宿主有 32.4 GB 可用）。最终读数取自第七次 panic 之后
重启的新启动，`env.txt` 记了完整时间线，`console.panic-*.log` 可查。

**交付去向**：随本次提交入库（`internal/processes` 新包、代理查询与动作、API、前端页面与 DTO、
30 份真机 `/proc` 夹具与 22 份真实响应、验收脚本与校验程序、openapi 补登与 CONTRACTS §10.2、
Q112—Q117、五个旧脚本的断言收紧）。证据目录 `docs/evidence/P05/2026-10-21-p05.2/`：
`result.md`、`commands.log`、`env.txt`、`p05-2-acceptance-2404.txt`、`payloads/`（31 份响应）、
`vm-logs-2404/`、`go-gates.txt`、`go-test.txt`、`web-gates.txt`、`check.txt`、`regression-p05-2.txt`。


### P05.3（2026-10-21）— journal 结构化查询、筛选、游标、实时流与资源回收

**范围**：`STAGES.md` §7 P05.3。日志是这一阶段唯一**只读**的子系统，但它比前两个都难：
数据来自一个按行输出、格式随版本微调、还会因为“窗口”与“游标”互斥而拒绝执行的外部命令，
而产品要求的四件事（精确筛选、可续传、可限额、不泄漏秘密）都压在这一层上。

| 文件 | 改动 |
|---|---|
| `internal/journal/`（新包，7 文件约 2000 行含测试） | `params.go` 规范化与全部边界（数据源白名单、unit 形状、级别闭集、游标字符集、1—1000、24 小时、未来 2 分钟 skew）；`entry.go` 逐字段解析 journald JSON（`binary`/`truncated`/`redacted` 三种“不完整”各自显式，绝不把空消息当“空日志”）；`query.go` 键集翻页（降序、`next_cursor`、`page_bytes`、带游标时窗口本地过滤）；`stream.go` 跟随流（`-f`、游标去重、六种结束原因、空闲/时长/条数/字节四道自限）；`syscmd_adapter.go` 只把“跑一个进程、收尾、可中断”接进 `internal/syscmd` |
| `internal/logs/fetch.go`（217 行） | Web 侧三件事：把参数交给代理的 `logs.query`、把结果按契约校验成 `journal.Page`、把代理错误翻译成 503/400；参数错误用“只标记不改写消息”的包装（Q134） |
| `internal/agent/{logsquery,streams}.go`、`registry.go` | 查询 `logs.query`；`/internal/v1/streams` 单次许可（绑定用途与参数、30 秒过期、只用一次、未登记用途 501）；NDJSON 流式响应，开流立即 flush 头（Q131） |
| `internal/agentclient/streams.go`（162 行） | 流客户端：克隆 `Transport` 且 `ResponseHeaderTimeout=0`，拨号器与普通请求相同（Q131） |
| `internal/api/logs.go`（254 行）、`router.go` | `GET /api/v1/logs` 与 `GET /api/v1/logs/stream`，**admin**；查询串解析失败直接 400（Q128） |
| `web/src/api/logs.ts`（517 行）、`logs.test.ts` | openapi 同源 DTO、级别/数据源中文标签、限额常量、`parseLogStreamFrame`（帧类型**来自事件名**）、`openLogStream`（续传交给 EventSource 的 `Last-Event-ID`）、`summarizeLogPage`、`entryPlainText` |
| `web/src/views/LogsView.vue`（524 行）、`components/LogEntryRow.vue`（127 行） | 查询表单 + 窗口说明 + 翻页 + 四态；条目行把 `binary`/`truncated`/`redacted` 各画各的，未知级别原样显示 |
| `web/src/fixtures/p05-3-vm/`（26 份）、`components/realdata-logs.test.ts`（24 例） | 真实日志页 + 真实 SSE 帧，与证据目录逐字节相同；SSR 回放覆盖 DTO、翻页链、错误信封、三种结束形态 |
| `tests/integration/p05-3-logs-acceptance.sh`（871 行，17 节）、`p05-3-payload-validate.py`（463 行）、`p05-3-emit-markers.sh`（79 行） | 真机验收；写入器用常驻 `systemd-cat` + 回读自证（Q132） |
| `tests/fixtures/journal/`、`internal/journal/journal_test.go` | 3 份真实 journalctl 字节；夹具改走 `testsupport.Fixture`（Q133） |
| `api/openapi.yaml`、`docs/CONTRACTS.md` §10.3/§15、`docs/DECISIONS.md` Q118—Q134 | 版本 `0.0.0-p05.3`、`stage: P05.3`、补登两条已实现路径；契约写清级别=下限语义、游标与窗口互斥（服务端本地过滤 + 前进的 `next_cursor`）、未转义 `;` 是 400、`limit=0` 是 400、`unit` 如实回传 `_SYSTEMD_UNIT`（含 `init.scope` 生命周期行）、流请求不等响应头 |

**这一阶段刻意不做的事**：不做全文检索/正则/journalctl 之外的日志源，不导出日志到文件，
不对日志做任何写操作（vacuum/rotate 都不做），不给 viewer “脱敏版日志”——过滤是尽力措施，
不能作为开放的理由；流不允许自定义 `journalctl` 参数，数据源是固定白名单。

**本轮验收**

| 检查 | 结果 | 证据 |
|---|---|---|
| VM 验收脚本自身断言（17 节） | **PASS=132 / FAIL=0 / NOT RUN=1** | `docs/evidence/P05/2026-10-21-p05.3/p05-3-acceptance-2404.txt` |
| VM 载荷校验（字段、闭集、null 语义、与 journalctl 对照） | **PASS=6466 / FAIL=0 / SKIP=0** | 同文件 §16 |
| 宿主 `sh scripts/check.sh` | **PASS 13 / FAIL 0 / NOT RUN 0** | `.../check.txt` |
| `go build` / `go vet` / `go test ./... -count=1` | 退出码 0 / 0 / **24 包全部 ok** | `.../go-gates.txt`、`.../go-test.txt` |
| 前端 `vue-tsc --noEmit` / `eslint .` | 0 error / 0 problem | `.../web-gates.txt` |
| 前端 `vitest run` | **347 passed（25 文件）**，其中日志真实载荷与真实帧回放 24 例 | `.../web-gates.txt` |
| 前端 `vite build` | 成功 | `.../web-gates.txt` |
| 契约 lint（redocly） | valid，9 warning（历史遗留） | `.../check.txt` |
| **跨阶段回归**（全部 17 个验收脚本） | **17 个脚本、1520 项断言：15 个脚本一次通过；`p01-2`/`p01-4` 的 3 条失败源于 `/etc/upanel/upanel.json` 被上一次被中断的运行留成 0 字节（默认配置路径 `EOF`），清掉残留后 run B 两脚本 148/0，产品代码未改（Q135）** | `.../regression-p05-3.txt` |

VM 上的关键断言：30 条标记写入同一单元后 `unit` + 窗口查到 36 行（30 标记 + 3 特殊 + systemd 的
Starting/Finished），每个标记恰好一次；`level=error` 只回 error 及更严重、`level=info` 同时含 error；
`limit=5` 的页 `truncated=true` 且 `next_cursor` 正好是本页最后一条，8 页翻完游标无重复、最后一页
`next_cursor=null`，且**最后一页 `scanned=2` 只给出 1 条、`truncated=false`** —— 窗口是在本地按时间过滤的
（Q126）；1 MiB 上限把 303 条截到 261 条 / 1046028 字节并标 `truncated_reason=page_bytes`；
`password=hunter2secret` → `password=[redacted]` 且 `redacted=true`，原文不在载荷里；`source=kernel`
用 `-k` 查出注入的 kmsg 行（Q127）；流的 `id:` 就是条目游标、事件名 `entry`、`Last-Event-ID` 续传不重复；
**客户端断开后 15 秒内 `journalctl` 子进程归零**，空闲 60 秒收到 `end{reason:"idle_timeout"}`，
同账号第 5 条流 429 而前 4 条不受影响。

**环境（与产品无关，但影响证据的可信度，故一并记录）**：本机内核本轮 panic 9 次，全是同一条
`Attempted to kill init! exitcode=0x0000008b`（PID 1 收到 SIGSEGV），uptime 8—70 分钟不定；
处置是 kill QEMU + `launch-vm.ps1` 冷启动。冷启动会清空 `/tmp`，所以 harness 整套重传、
二进制按同一份源码重新交叉编译后再跑，验收（132/0/1）与原生测试（749/0）的数字与前一轮一致。
全量回归还撞到一条**脏现场**：`/etc/upanel/upanel.json` 被上一次被中断的运行留成 0 字节，
`p01-2`/`p01-4` 三条默认配置路径断言因此报 `解析配置文件失败: EOF`；清掉残留后重跑全绿，
产品代码一行未改，`run-all.sh` 加了开跑前的 0 字节警告（Q135）。

**本轮发现并修掉的问题**（完整的十条见 `result.md` §5，每条都附钉住它的测试名）

1. **`--cursor` 与 `--since/--until` 互斥**（Q126）：真实 journalctl 直接退出 1，带窗口的翻页每页
   503。改成带游标时不拼时间参数、窗口在 Go 侧过滤，并让 `next_cursor` 在“整页被过滤掉”时仍前进。
2. **`--kernel` 不存在**（Q127）：只有 `-k`。
3. **未转义的 `;` 让游标被静默丢掉**（Q128）：Go 的 `url.ParseQuery` 丢弃整对参数，服务端以为“没给游标”。
   现在查询串解析失败就是 400。
4. **参数错误被报成 503**（Q129）：`to` 在未来、游标被拒这类只有 Run 阶段能判的错误映射成 400。
5. **`limit=0` 被当成缺省**（Q130）。
6. **空闲流活不到空闲超时**（Q131）：代理不开头 + 流客户端带 10 秒响应头超时，安静系统上先被客户端掐死。
7. **参数错误消息三层前缀套娃**（Q134）：`journal: …: agent: …: journal: …`，把“改参数”说成代理故障。
8. **验收脚本自己的三处错**（Q132、Q133）：`logger` 丢 `_SYSTEMD_UNIT`、systemd 生命周期行是
   `init.scope`、`internal/journal` 夹具相对路径在 `/tmp` 下找不到。
9. `gofmt` 8 个文件未格式化（`check.sh` 抓到）。
10. `openapi.yaml` 的 `stage` 与 `implemented_paths` 漏登日志两条路径。

**NOT RUN（未验证，含原因）**

- **单条消息 8 KiB 截断（`truncated=true`）**：真实 journald 对超过 4086 字节的 `MESSAGE` 直接给 null
  （走 `binary` 分支，真机上有断言），字符串截断只能由 `internal/journal` 的合成输入覆盖
  （`TestQueryTruncatesLongMessage`）。
- **浏览器渲染**：VM 上没有浏览器。页面证据在宿主用同一批载荷与真实 SSE 帧做 SSR 回放（24 例）。
- **journal 目录不可写/被删**：需要故意破坏系统日志，不属本阶段。
- **异构 systemd（22.04 / systemd 249）**：本轮只有一台 24.04，`journalctl` 参数差异未验证。

**交付去向**：随本次提交入库（`internal/journal` 新包、`internal/logs`、代理查询与流接口、API、
前端页面与 DTO、26 份真实响应/帧夹具、3 份 journalctl 字节、验收脚本与校验程序、openapi 补登与
CONTRACTS §10.3/§15、Q118—Q134）。证据目录 `docs/evidence/P05/2026-10-21-p05.3/`：
`result.md`、`commands.log`、`env.txt`、`p05-3-acceptance-2404.txt`、`payloads/`（43 份响应）、
`*.frames`（7 份原始 SSE）、`go-gates.txt`、`go-test.txt`、`web-gates.txt`、`check.txt`、
`regression-p05-3.txt`、`native-tests.txt`。


### P06.1（2026-10-21）— 受管理文件根、句柄级路径约束、列表/文本/下载

**范围**：`STAGES.md` §8 P06.1。这一轮只做**读**：列根、列目录、读文本、下载普通文件，
以及把“路径逃不出去”这件事做成可以验证的机制。写（上传/提交/编辑/删除/改名/权限）属于 P06.2，
终端属于 P06.3，页面上不留任何假入口。

| 文件 | 改动 |
|---|---|
| `internal/files/types.go`（369 行） | 两个角色共用的 DTO 与错误哨兵：`RootInfo`/`Entry`/`ListPage`/`TextFile`/`RootsResult`/`DownloadHeader`；`ValidateRelPath` 只做**形状**校验（绝对路径、`..` 段、空段、`.` 段、NUL、非法 UTF-8、PATH_MAX），边界校验交给内核；限额写死为常量（`MaxTextBytes=1<<20`、`MaxPathBytes=4096`、`ListPageSize=200`、`MaxListScan=50_000`） |
| `internal/files/manager_linux.go`（743 行） | 代理侧实现：登记时记下根的身份（dev/ino + 挂载点 + fstype），每次请求重新核对，变了就是 `ROOT_UNAVAILABLE`；解析只用 `openat2` 一次系统调用（`RESOLVE_BENEATH|RESOLVE_NO_MAGICLINKS|RESOLVE_NO_XDEV|RESOLVE_NO_SYMLINKS` + `O_NOFOLLOW`），没有“先检查再打开”的窗口；打开后对**同一个句柄** `fstat`（普通文件、nlink==1、dev 与根相同、大小上限），版本也取自这次 stat |
| `internal/files/mount_linux.go`（152 行） | 最小 mountinfo 解析（挂载点、fstype、转义 `\040`），只为本模块服务；与 `internal/storage` 的解析刻意不合并（Q141） |
| `internal/files/fetch.go`（252 行）、`http.go`（102 行） | Web 侧只做转发与翻译；`HTTPStatusOf` 是**唯一**的错误判决表，代理查询、代理流、Web 公开接口共用（Q143） |
| `internal/files/manager_other.go`（109 行） | 非 Linux 平台一律 `NOT_IMPLEMENTED`，不退化成一串字符串比较 |
| `internal/agent/filesquery.go`（140 行）、`registry.go`、`streams.go` | 登记 `files.roots`/`files.list`/`files.text` 三个查询与 `files.download` 流用途；流沿用 P05.3 的单次许可（30 秒、只用一次、未登记用途 501），首行 JSON 头 + 原始字节（Q144） |
| `internal/api/files.go`（258 行）、`router.go` | 四条 `GET` 全 **admin**；下载不走 JSON（octet-stream + nosniff + no-store + RFC 5987 文件名 + `X-UPanel-File-Version`），被拒绝时回 JSON 信封；本地形状校验与代理判决走同一张映射表（越界是 422，不是 400） |
| `internal/config/config.go` | `agent.files.roots[]` 校验：必须是绝对路径、存在、是目录、**不能是 `/`**（Q145）；错误消息指向具体字段 |
| `web/src/api/files.ts`（335 行）、`files.test.ts`（360 行） | 与 openapi 同源的 DTO；`downloadFile` 用 `fetch` 先读响应再落盘（错误信封不会被存成文件）；`breadcrumbs`/`parentRelative`/`joinRelative` 只拼路径、不归一化 `..` |
| `web/src/views/FilesView.vue`（721 行）、`components/FileEntryRow.vue`（111 行） | 根选择器 + 面包屑 + 条目表 + 文本预览 + 下载；加载/空/错误/无权限四态；符号链接与特殊文件在行内写明“不跟随/不读取”；超过 1 MiB 给“只能下载”；页面文案只说“服务端登记的文件根”，**不出现“整个服务器”** |
| `web/src/components/ApiErrorPanel.vue` | 新增可选 `title`：同一错误码在“列目录”和“读文件”两个动作里说法不同（此前 FilesView 传的 title 落成了 attr，静默失效） |
| `web/src/fixtures/p06-1-vm/`（22 份）、`components/realdata-files.test.ts`（27 例） | 真机响应原样回放：根列表（含 tmpfs 根与 `/` 的容量不同）、目录项（含 symlink/FIFO/硬链接/>1 MiB）、分页链、文本、11 个错误信封、下载响应头、能力表；与证据目录逐字节相同 |
| `tests/integration/p06-1-files-acceptance.sh`（597 行，§0—§13 共 14 节）、`p06-1-payload-validate.py`（456 行） | 真机验收与载荷校验；现场自建（普通文件、符号链接、magic link、硬链接、FIFO、>1 MiB、250 个分页文件、50 010 项的 tmpfs 目录 + 其下嵌套挂载、被替换的根） |
| `api/openapi.yaml`、`docs/CONTRACTS.md` §11.1（新增）、`docs/DECISIONS.md` Q136—Q146 | 版本 `0.0.0-p06.1`、`stage: P06.1`、四条路径 + 5 个参数 / 2 个头 / 8 个 schema；契约补登字段、游标语义（末项名字、页大小 200、`next_cursor: null`）、version 形状、下载响应头、错误判决表 |

**刻意不做的事**：不做任何写（上传、提交、编辑、删除、改名、权限、所有者），不递归列目录，
不跟随符号链接（也不报告它的目标），不读特殊文件，不做全文搜索/编辑器，不提供“输入任意路径”
的入口，不把根的 `path` 渲染成可点击的服务器目录。

**本轮验收**

| 检查 | 结果 | 证据 |
|---|---|---|
| VM 验收脚本自身断言（§0—§12） | **PASS=115 / FAIL=0 / NOT RUN=0** | `docs/evidence/P06/2026-10-21-p06.1/p06-1-acceptance-2404.txt` |
| VM 载荷校验（字段闭集、`next_cursor` null 语义、根 `path != "/"`、错误码与文案） | **PASS=6379 / FAIL=0** | 同文件 §13 |
| 宿主 `sh scripts/check.sh` | **PASS 13 / FAIL 0 / NOT RUN 0** | `.../check.txt` |
| `go build` / `go vet`（宿主与 linux 交叉） / `gofmt -l` | 退出码 0 / 0 / 无输出 | `.../go-gates.txt` |
| `go test ./... -count=1` | **25 个包全部 ok** | `.../go-test.txt` |
| VM 内核原生测试二进制 | `files.test` **54 个 PASS**、`api.test` PASS | `.../native-tests.txt` |
| 前端 `vue-tsc --noEmit` / `eslint .` | 0 error / 0 problem | `.../web-gates.txt` |
| 前端 `vitest run` | **406 passed（28 文件）**，其中 P06.1 真实载荷回放 27 例、手写形态 32 例（`api/files.test.ts` 22 + `components/files.test.ts` 10） | `.../web-gates.txt` |
| 前端 `vite build` | 成功 | `.../web-gates.txt` |
| 契约 lint（redocly） | valid，6 warning（`IfMatchVersion`/`DataEnvelope`/`ListEnvelope`/`TooManyRequests` 未使用 + 2 条操作缺 4xx，均为历史遗留；本轮新增的组件全部被引用） | `.../openapi-lint.txt` |

VM 上的关键断言：`../../etc/shadow`、`/etc/shadow`、中段 `..`、`.` 段、`a//b`、超长路径、
文本接口空路径共九条形状全部被拒且**错误码正确**（越界 422 `PATH_OUTSIDE_ROOT`、形状 400；
宿主 `files.test` 另覆盖十种拒绝形状，含 NUL、结尾斜杠、单段 256 字节、超过 PATH_MAX、非法 UTF-8）；
根里的符号链接（指向根外文件、根外目录）与 `/proc/self/fd` 这类 magic link、硬链接（nlink=2）、
FIFO 全部拿不到内容，且没有一种状态码是 200，FIFO 用例同时证明“打开根内非普通文件不会把代理挂住”；
根内嵌套 tmpfs（`RESOLVE_NO_XDEV`）同样 422；根被 `mv` 替换之后回 409 `ROOT_UNAVAILABLE`，
`roots` 里标出该根不可用，“重建的根没有被当成原来的根（响应里没有任何目录项）”；配置里把根改成 `/` 时 `upanel-agent` 以退出码 2 拒绝启动，消息里出现“文件根”与 `/`；
列表页 200 项、游标为末项名字、250 个文件翻两页无重叠无跳过、尾页 `next_cursor=null`、
50 010 项的目录 `truncated=true`；文本与磁盘逐字节一致（27 字节）、`size_bytes` 与 content 字节数相等、
`expected_version` 不匹配 409 `VERSION_CONFLICT`；下载 `Content-Length` 与 `stat` 一致、
sha256 与磁盘一致、`X-UPanel-File-Version` 合法、越界下载回的是 JSON 而不是 octet-stream；
未登录 401（roots/list/download）与 viewer 403（四条路径各一条，且下载拿不到内容）；admin 的四条 200 由成功用例给出；能力表只有 `files.roots\|list\|text`，没有任何写动作。

**本轮被真机抓出的缺陷（全部改产品，不改断言）**

1. `List` 用 `O_PATH|O_DIRECTORY` 打开目录再 `getdents64` → **EBADF**，每个列举都 500（Q136）。
   `go:build linux` 的包在 Windows 宿主上根本没跑过，所以只有 VM 能抓到。
2. `Text`/下载用 `O_RDONLY` 打开 FIFO 会**永久阻塞**，一个 `?path=fifo` 就钉死一个协程（Q137）。
   改 `O_RDONLY|O_NONBLOCK`。
3. 根内“指向目录的符号链接”被内核报 ENOTDIR，被我归成 400；契约要求 422（Q138）。
   补一次 `O_PATH|O_NOFOLLOW` 重解 + `fstat` 判定。
4. 列表 `next_cursor` 尾页返回 `""`，与 `/jobs`、`/logs`、`/users` 的 `null` 不一致（Q140）。
   Go 结构体改 `*string`，前端 DTO 同步。
5. 前端 `ApiErrorPanel` 的 `title` 从未声明为 prop，三个页面传的标题静默失效。

**流程事故（记录，不掩盖）**：第一轮验收脚本全部请求漏拼端点（`/list`、`/text`、`/roots`），
59 条失败但真正的原因是“全部打到了 `/api/v1/files`”，而断言大多只检查“非 200”，
所以失败看起来像产品问题（Q139）。改成端点必填的取数函数后重跑，才看到真实的产品缺陷。
另一条：`TestRootIdentityChangeIsUnavailable` 曾经**假通过**——先删临时目录再建诱饵，
内核把刚释放的 inode 又分给了诱饵，dev/ino 与登记时相同（Q146）。改成“先造诱饵、再删原根、再 mv 到位”。

**环境（与产品无关，但影响证据可信度，故一并记录）**：本轮开始时 VM 处于 boot 0（05:34:25 起），
上一轮 boot -1 的最后一条记录是 SSH 会话关闭（05:16:11），**没有任何 shutdown/reboot target 记录**，
即第 10 次非正常终止（panic 文本未落盘，故不引用原文）。本轮 3 次完整验收、2 次原生测试期间未再发生。

### P06.2（2026-10-21）— 流式暂存上传、提交、编辑备份、目录创建、复制/移动/删除、权限/所有者受限修改、版本冲突与配额

**范围**：`STAGES.md` §8 P06.2。这一轮把 P06.1 的只读文件管理变成**会写**，并且写入**没有一条旁路**：
七个写动作加上传提交走的是 P03 建成的同一条“预检 → 任务 → 幂等 → 资源锁 → 读回验证 → 审计”链路，
和 P05.1 的服务启停、P05.2 的进程信号共用同一套执行器。字节流走 P05.3 建成的“单次许可”通道形态，
不进 job 表、不随上传大小涨 Web 内存。终端（PTY）属于 P06.3，页面上不留假入口。

| 文件 | 改动 |
|---|---|
| `internal/files/types.go`（+420 行） | 写入侧 DTO 与错误哨兵：`PathStat`（大小/模式/uid/gid/mtime/inode/`nlink` 版本）、`TreeStat`（`TreeEntries`/`TreeFiles`/`TreeDirs`/`TreeSymlinks`/`TreeBytes`/`TreeTruncated`）、`Upload`/`UploadState`、`BackupInfo`；限额常量 `DefaultMaxUploadBytes=2 GiB`、`UploadTTL=24h`、`MaxEditBackups=10`、`MaxEditBackupBytes=500 MiB`、`MaxTreeEntries=50_000`、`ReservedDirName=".upanel"` |
| `internal/files/staging.go`（新增） | 暂存区：`.upanel/staging/<sha256(rel)[:8]>/<UnixNano>.part`；**声明即预留**（`reserveSpace` 无论有没有容量读取器都记账，`INSUFFICIENT_SPACE` 在预检阶段给出）、**收满即归还**、任何删记录的地方统一在 `dropUpload` 归还（Q158）；`SweepStaging` 10 分钟一轮按 TTL 清理 |
| `internal/files/write.go`（新增） | 写入原语：`WriteText`（长度/UTF-8/末尾换行约定）、`Mkdir`、`Copy`（先拼暂存再换位，失败不留半截）、`Move`（同根 rename、跨根 copy+delete）、`Delete`（递归需显式）、`Chmod`/`Chown`；`StatTree`（不限深度地数条目，超上限截断并置 `TreeTruncated`）、`walkTree`（遇到符号链接即 `TREE_UNSUPPORTED`，不复制一半）、`backupFile`/`pruneBackups`（10 份 + 500 MiB 两级上限） |
| `internal/files/manager_linux.go`、`manager_other.go` | Linux 侧补齐写入实现（`renameat2` 原子换位、`O_NOFOLLOW` 打开每个父目录、同句柄 `fstat` 读回）；非 Linux 一律 `NOT_IMPLEMENTED`，不退化成一串字符串比较 |
| `internal/files/http.go` | 错误判决表扩到写入：`ErrUploadState`/`ErrAlreadyExists`/`ErrVersionConflict` → 409，`ErrNotFound`/`ErrUploadNotFound` → 404，`ErrNoSpace` → 413 带 `VALIDATION_ERROR`；`CONFIRMATION_MISMATCH` 仍是 400 |
| `internal/files/upload_fetch.go`（新增）、`internal/api/files_upload.go`（新增） | Web 侧上传编排（声明 → 分片写 → 提交）与四条 `/api/v1/files/uploads*` 接口；`Content-Length` 必须等于声明大小，可续传偏移，全部 admin |
| `internal/agent/filesaction.go`（新增，~900 行） | 七个写动作的 `Action`：`Plan` 生成 `FileActionPlan`（`normalized_params`、`target`、`allowed`、`blockers`、`warnings`、`impacts`、`locks`），`Execute` 只在代理内做真实写；`parentNotices`/`missingNotice` 把“父目录缺失”与“目标缺失”分开（Q156）；动作锁按目标路径的规范化名排序，`file.delete` 的 `Entries = TreeEntries + 1` |
| `internal/agent/uploads.go`（新增）、`agentclient/uploads.go`（新增） | 代理侧单次许可的上传通道（`files.upload.declare|write|commit|drop`），与 P05.3 的流同形态：30 秒许可、只用一次、未登记用途 501 |
| `internal/agent/registry.go`、`runner.go`、`preflight.go`、`server.go`、`cmd/upanel-agent/main.go` | 生产登记表**第一次有写动作**（七个动作 + `commit_upload`）；计划与执行共用同一份 `files.Writer`；预检渲染带上写入的阻塞项与警告 |
| `internal/api/router.go`、`server.go`、`middleware.go`、`preflights.go`、`users.go`、`operations/service.go`、`internal/config/config.go` | 四条上传路由（admin）、请求体上限（`MaxUploadBytes` 来自配置，`413` 走 `VALIDATION_ERROR`）、再认证窗口与上传路由的 CSRF、`agent.files.max_upload_bytes` 等新配置项与校验 |
| `web/src/api/files.ts`（+300 行）、`files.test.ts`（+361 行）、`client.ts` | 与 openapi 同源的写入 DTO；上传用 `fetch` + `ReadableStream` 分片、进度、取消；`VERSION_CONFLICT`/`PREFLIGHT_STALE`/`INSUFFICIENT_SPACE` 各有专门的用户文案 |
| `web/src/views/FilesView.vue`（+1119 行）、`components/FileEntryRow.vue`、`FileActionConfirm.vue`（新增） | 上传、新建目录、重命名/移动、复制、删除（递归要点确认）、编辑文本（带备份提示）、改权限/改所有者；加载/空/错误/无权限/只读根五态；确认短语弹窗逐字匹配、过期整条重来 |
| `tests/integration/p06-2-files-write-acceptance.sh`（§0—§16）、`scripts/run-native-tests.sh` | 真机验收 228 项断言；现场自建（可写根 / 第二可写根 / 只读根 / 上传载荷），跨 16 节覆盖上传、配额、写入、备份、并发、路径、权限边界 |
| `api/openapi.yaml`、`docs/CONTRACTS.md`（写入章节）、`docs/DECISIONS.md` Q155—Q159 | 版本 `0.0.0-p06.2`、`stage: P06.2`、四条上传路径 + 请求体上限 + 写入 Plan/Result 字段；契约补登备份上限、暂存语义、`chown` 只收名字、`file.delete` 的 `Entries` 定义 |

**刻意不做的事**：不做分片合并的上传（单次 `PUT` 有偏移续传，但不拼多段）、不做目录递归复制到另一个根、
不跟随任何符号链接（复制时遇到就是 `TREE_UNSUPPORTED`）、不允许 `chown root`、不接受数字 uid、
不提供“把文件拖到任意路径”的入口、不做并发编辑的锁（只做版本冲突检测，冲突就让人重来）、
不做回收站（删除即删，备份只针对覆盖写）。

**本轮验收**

| 检查 | 结果 | 证据 |
|---|---|---|
| VM 验收脚本自身断言（§0—§15） | **通过 228 项 / 失败 0 项 / 未执行 0 项** | `docs/evidence/P06/2026-10-21-p06.2/p06-2-acceptance-2404.txt` |
| 载荷归档（真实响应体） | 43 份 | `.../payloads/` |
| VM 内核原生测试套件（12 个包） | **TOTAL PASS=909 / FAIL=0** | `.../native-tests.txt` |
| 宿主 `sh scripts/check.sh` | **PASS 13 / FAIL 0 / NOT RUN 0** | `.../check.txt` |
| `go build` / `go vet`（宿主与 linux 交叉） / `gofmt -l` | 退出码 0 / 0 / 无输出 | `.../go-gates.txt` |
| `go test ./... -count=1` | **25 个包全部 ok** | `.../go-test.txt` |
| 前端 `vue-tsc --noEmit` / `eslint .` / `vite build` | 0 error / 0 problem / 构建成功 | `.../web-gates.txt` |
| 前端 `vitest run` | **428 passed（28 文件）** | `.../web-test.txt` |
| 契约 lint（redocly） | valid，6 warning（与 P06.1 同一批历史遗留；本轮新增组件全部被引用） | `.../openapi-lint.txt` |

VM 上的关键断言：`POST /uploads` 声明后目标文件**一字节未动**（`GET /files/text` 仍是旧内容旧版本）、
暂存区只有一份 `.part`、`PUT` 之后 `received_bytes` 精确等于声明值、`commit_upload` 之后 `staging` 归零、
内容与提交字节逐字节一致；`Content-Length` 与声明不符 400、重复 `PUT` 第二次 409 `VERSION_CONFLICT`、
放弃上传后 `reserved` 归零；声明超过剩余空间 → 预检 200 + `INSUFFICIENT_SPACE` 阻塞（不是提交时 500）；
外部 `echo >> app.conf` 之后提交 → **409 + `PREFLIGHT_STALE`，文件内容没被覆盖**，作业表里多一条
`failed`/`step=rejected` 的作业（Q159）；三次覆盖后该文件自己的备份目录里 3 份、内容分别是上一版、
`backup_count` 如实返回；`file.mkdir` 已存在 → `TARGET_EXISTS`、父缺失 → `PARENT_MISSING`、
嵌套只建一层；目录复制带子目录、目标已存在 → `TARGET_EXISTS`、树里有符号链接 → `TREE_UNSUPPORTED`
**且目标未创建**；非空目录不递归 → `DIRECTORY_NOT_EMPTY`、递归删除要再认证 + 带路径的短语、
条目数 4（`docs/a.txt` + `docs/nested/` + `docs/nested/b.txt` + 目录自身）；`chmod 0640` 生效并读回、
`4755`/非八进制 → 400、`chown` 只收受管理账号名且读回 uid/gid 一致；只读根上七个动作全是
`ROOT_READ_ONLY` 阻塞；写动作的路径形状与 P06.1 同一套判决（越界 422、形状 400），
**没有字节被写进暂存区**；viewer 能列目录但七个动作全 403 `FORBIDDEN`。

**本轮被真机抓出的缺陷（全部改产品，不改断言）**

1. `StatTree` / `walkTree` / 备份目录用 `O_PATH|O_DIRECTORY` 再 `getdents64` → **EBADF**（Q155）。
   三个功能同时坏：目录删除预检恒 `PATH_REJECTED`、目录复制预检 500、
   `pruneBackups` 每次都失败——而失败只写一条 warning，于是 **11 次覆盖留下 12 份备份、`backup_count` 恒为 0**。
   这是 Q136 的**重犯**，所以修复配了会真的走进子目录的用例（`TestStatTreeCountsDirectories`、
   `TestBackupPruneKeepsTen`）。
2. 目标缺一级父目录时，files 层在 `openParent` 就返回错误（不是 `Exists=false`），
   `blockerFor(ErrNotFound)` 把它翻成 `TARGET_MISSING`，`parentNotices` 根本没机会跑——
   契约里的 `PARENT_MISSING` 在真实层**不可达**（Q156）。
3. `List` 只做了路径层拒绝，名字表照原样返回，于是 `.upanel` 在列举里**看得见**：
   一个既列不出内容、又删不掉的条目，而它恰恰是面板自己的内部目录（Q157）。
4. 预留计数在两处不成立（Q158）：没配容量读取器时 `reserveSpace` 直接 return，`ReservedBytes` 恒 0；
   `SweepStaging` 走过期清理不释放预留——盘上没数据、账上一直占着。两条都靠
   `TestUploadLifecycleHappyPath` 与 `TestUploadExpirySweep` 兜住。

**流程事故（记录，不掩盖）**

1. `chmod +x /tmp/upanel-bin/upanel-*` **不匹配** `upanelctl`（没有连字符），`init-admin` 以 126 退出，
   整份验收全是 401 与空响应。P04.2 已经犯过一次，这次又犯——所以写进了 commands.log 与 result.md §3.5。
2. `jget` 之前用 `python3 -c '…' "$1"` 取参数，`sys.argv` 里拿不到——改成 `eval` + `_` 约定，
   异常统一打印 `ERR:<Type>:<msg>`，命名空间里补上 `json`。
3. 1 MiB 的预检请求体用 `--data` 直接拼进参数会撞 `MAX_ARG_STRLEN=128 KiB`，改 `--data-binary @"文件"`。
4. 「按位置取 `jobs[0]`」不稳定：`created_at` 是**秒级**，同一秒内顺序由 `id DESC` 决定，
   断言必须按内容匹配（`any(...)`）。
5. 两次 ssh 退出码 255 是 `pkill -f` 匹配到 ssh 自己的远端命令行；改 `pkill -x` 与方括号打断自匹配。

**未验证（NOT RUN，逐条原因见 result.md §5）**：浏览器里真实渲染这套新界面、真实 ENOSPC、
24 小时自然过期、500 MiB 备份总量上限、50 000 条目树、多客户端真并发赛跑、
代理 `runner` 的再验证步骤、跨文件系统移动。未验证项一律不计入通过项。

### P06.3（2026-10-21）— 受限 Web 终端：upanel-shell 的 PTY、两跳 WebSocket、四道门、一次性许可、超时与元数据审计

**范围**：`STAGES.md` §8 P06.3。这一轮给面板装上第一个**交互通道**，同时把它钉成全场权限最硬的地方：
PTY 在 `agent` 进程里 `fork` 之后、`exec` 之前就降权到 `upanel-shell`（`setgroups` 清空 → `setgid` → `setuid`，
再从 `/proc/<pid>/status` 读回真实/有效 uid、gid 与附加组核对，任何一项不符就杀掉子进程让创建失败）；
浏览器连的是 Web，Web 再把 WS 接到代理，浏览器从头到尾不知道代理 socket 存在，也拿不到任何“执行命令”的入口——
只有字节，没有 `run` 帧、没有 `POST /exec`、没有 sudo 密码框。

| 文件 | 改动 |
|---|---|
| `internal/terminal/manager.go`（新增） | 会话表（一把锁护住整张表，创建/计数/回收/超时扫描都在同一份真相上判定）、`Limits` 收紧校验、`sweepLocked` + `Run` 每秒扫描、`Attachment`（单连接、`Read/Write/Resize/Info/Done/Exit`、输出块满即丢）、结束原因闭集 `user|exit|idle|expired|disconnect|shutdown`；`Close` 幂等且“别人的会话”对外一律 `ErrNotFound` |
| `internal/terminal/manager_linux.go`（新增） | `ResolveAccount`（账号必须存在、非 root、home 属主正确且 `o-w`、shell 不能是 nologin、不在 `sudo/docker/disk/adm/kvm/lxd/wheel` 等特权组）、`startPTY`（`Setsid+Setctty`、`Credential{NoSetGroups:false}`、`pty.StartWithAttrs`）、`buildEnv` 白名单（8 个变量，**没有**任何面板自己的标记）、`verifyChildAccount`、**显式挂断**（`kill(-pid, SIGHUP)` → 2 秒不退补 `SIGKILL`，发信号前用 `/proc/<pid>/stat` 的 ppid+starttime 确认身份） |
| `internal/terminal/frame.go`、`http.go`、`types.go`（新增） | 帧严格解码（字段闭集）、`InputLimiter`（256 KiB/s、200 帧/s）、关闭码 `1000/4001/4002`、错误→状态码唯一映射 `HTTPStatusOf` |
| `internal/terminal/remote.go`（新增） | Web 侧调用端：本地先校验形态（错的 `session_id` 不占一次 IPC）、响应自洽性校验、代理拒绝翻成 Web 错误（404/409/403/503 各归各位） |
| `internal/agent/terminalaction.go`、`terminalstream.go`（新增） | 动作 `terminal.create`、查询 `terminal.sessions`、WS 流 `pumpTerminal`（一个 `closeWith` 保证只发一次 close 帧；`statusCarrier` 把拒绝提升成代理错误码） |
| `internal/agentclient/terminal.go`（新增） | Web→代理的终端客户端（连接、流、关闭） |
| `internal/api/terminal.go`、`terminalgrant.go`、`terminalws.go`（新增） | 四条 HTTP 接口（列表/领许可/关闭/连接）、一次性许可（32 hex、30 秒、只用一次、只从子协议取、不回显）、WS 中继（镜像代理的关闭码，`sendableCloseCode` 拦掉 1005/1006/1015） |
| `internal/agent/{registry,server,streams,systemquery}.go`、`internal/api/{router,server,users}.go`、`internal/audit/audit.go` | 登记表在账号可用时才登记两个能力；`/internal/v1/streams` 支持终端用途；中间件拒绝时 `denyTarget` 把 `target` 记成 `terminal:<id>`；审计动作 `terminal.attach`/`terminal.close` + 五个 stage |
| `internal/config/config.go`、`cmd/upanel-{agent,web}/main.go` | 终端账号/home/shell、三个时限与两个上限（只能收紧）、Web 侧启动探测能力 |
| `web/src/api/terminal.ts`、`terminal.test.ts`、`views/TerminalView.vue`（新增） | 与 openapi 同源的 DTO；xterm 页面：连接中/已连接/已断开/空/错误/无权限六态，固定提示“这是受限终端，不是 root shell”，`capabilities` 为空时整页说明未启用 |
| `tests/integration/p06-3-terminal-acceptance.sh`、`p06-3-ws-client.py`（新增） | 真机验收 282 项断言 20 节；WS 客户端把“输入计划 → 期望帧”写成 JSON，由脚本驱动 |
| `api/openapi.yaml`、`docs/CONTRACTS.md`（新增 §10.4）、`docs/DECISIONS.md` Q160—Q169 | 四条接口、`stage: P06.3`；契约写清两跳、两状态、409、收紧规则、帧表与限额、关闭码、许可规则、四道门表、错误映射、幂等 DELETE、能力软关闭与“明确不提供”清单，以及“回收 = 进程真的没了”的判定标准 |

**刻意不做的事**：不做 root/sudo 终端、不做 `run` 帧或命令接口、不做 tmux 式持久化（会话最长 1 小时、
进程不跨 Web/代理重启存活）、不做录像/回放、不做多人共享同一会话、不做 SFTP（文件走第 11 节契约）、
断流期间的输出直接丢弃不补发（实时通道语义）。

**本轮被真机抓出的缺陷（全部改产品，不改断言）**

1. 领许可 / 关闭别人的会话返回 503 `AGENT_UNAVAILABLE`：代理查询层把 `ErrNotFound` 当执行失败 → 500，
   Web 再翻成 503。`terminalRejection` 给每个查询/关闭错误带上判定，三层说同一个码（Q165）。
2. 关闭一个已经不存在的会话返回 404，关闭别人的会话返回 403：`Close` 既不幂等、又承认了“这个 id 存在”。
   改成幂等、非属主一律 `ErrNotFound`，与 `Get` 同一规则（Q165）。
3. 帧非法/超速率时浏览器侧 `closed_by_server=False`：服务端只 `conn.Close()` 从不发 close 帧。
   加 `4001`/`4002`，Web 只镜像代理给的码（Q166）。
4. 终端环境里能读到 `UPANEL_TERMINAL=1`：`buildEnv` 多塞了标记变量，被 `env | grep -c UPANEL` 抓到。
   从白名单删除——终端里的进程和一次 SSH 登录的差别不需要靠环境变量表达（Q167）。
5. **会话已结束，`upanel-shell` 还在跑，代理里 master fd 也还开着**（第 13 节）
   。根因不是 shell 忽略 SIGHUP（`SigIgn` 显示没忽略），而是 `os.File.Close` 对阻塞 fd 只做“标记 + 减引用”，
   真正的 `close(2)` 要等读泵那次 `read` 返回 → tty 从不释放 → 挂断信号从未发出、`Wait` 永不返回。
   改成“关 master + 显式 `kill(-pid, SIGHUP)`，2 秒不退补 `SIGKILL`”，配
   `TestManagerGraceExpiryReapsShell` / `TestManagerCloseReapsShell` 两个原生用例断言 `/proc/<pid>` 消失（Q169）。
6. 中间件层拒绝的终端路由审计 `target='user'`：拒绝发生在拿到会话 id 之前。`denyTarget` 按动作与
   `:id` 形态给出 `terminal:<id>`，只有列表接口（URL 里没有 id）仍记 `user`（Q168）。

**验收脚本自身的缺陷（记录，不掩盖）**：脚本以 CRLF 落地导致 `sh` 报 `set: Illegal option`；
上传的二进制是 0644 没有执行位（`chmod +x` 且 `chmod 755 /tmp/upanel-bin` 才能让 `upanel-web` 穿过去）；
能力断言把 `terminal.sessions` 拿去 `supported_actions` 里找；限速用例的填充字节 `x` 落进 PTY 行缓冲，
把后续 `export` 污染成 `command not found`（改发 `\n`）；两个账号的会话状态用同一个 cookie jar 采样
（改按 jar 分别 `wait_state_as`）；“审计 target 以 `terminal:` 开头”这条断言过宽，把列表接口的 `user`
也算失败——改成“三类行分别精确比对”（脚本断言只增不减，282 > 277）。

**实际执行的检查与证据**：VM 24.04.5 上 **PASS=282 / FAIL=0 / NOT RUN=0**；VM 内核原生 **962 用例 0 FAIL**
（terminal 33 / api 200 / agent 206 / storage 159 / agentstore 72 / agentclient 39 / operations 57 /
files 80 / monitoring 30 / journal 40 / logs 22 / store 11 / syscmd 13）；宿主 `go test ./...` 26 包 ok（3 个包无测试文件）、
`gofmt` 无输出、`check.sh` **PASS 13 / FAIL 0 / NOT RUN 0**；前端 **29 文件 / 445 用例**；openapi valid 7 warning。
证据 `docs/evidence/P06/2026-10-21-p06.3/`（含 18 份载荷）。

**未验证（NOT RUN，逐条原因见 result.md §6）**：22.04.5 上的终端验收、真实浏览器里手工敲终端
（xterm 渲染/输入法/粘贴/移动端键盘）、代理被 `kill -9` 时浏览器侧的表现、`/dev/pts` 耗尽与并发压力、
终端页的可访问性。未验证项一律不计入通过项。

### P07.1（2026-10-21）— 存储写动作的拒绝逻辑：预检判决、可靠身份、整盘锁、系统设备/挂载/swap/holders/依赖保护

**范围**：`STAGES.md` §8 P07.1、“只验证拒绝逻辑”。这一轮**不写盘**：四个存储动作只登记参数解码与预检，
`Execute` 为 nil，提交必 501 `NOT_IMPLEMENTED`。换来的是把“什么情况下必须拒绝”这件事在真实设备上钉死：
系统盘及其所有分区、已有分区表、已有文件系统/LUKS/交换区签名、挂载中、活动交换区、dm/md holders、
被进程打开、身份不可靠、区间不对齐/越界/太小/落在表内、文件名与标签超限——每一条都有阻塞码、都有一条真机断言。

判决层的第一原则是**读不出来不等于可以写**：签名读不到 → `SIGNATURE_UNREADABLE`；`/proc/*/fd` 扫不动 →
`DEVICE_BUSY` 而不是“没人用”；分区表只拿到 sysfs 而没读盘 → `PARTITION_TABLE_UNREADABLE`；
未知设备类型 → `UNSUPPORTED_DEVICE_KIND`。所有判断失败都变成阻塞项，没有一条默认放行。

| 文件 | 改动 |
|---|---|
| `internal/storage/assess.go`（新增，1080 行） | 判决层：`Assess(action, params)` 返回 `Assessment`（allowed / blockers / warnings / impacts / locks / 身份指纹 / 状态版本）。四个动作各自的规则函数：初始化只允许无分区表且无签名的空白整盘；建分区只允许 GPT 且区间落在空闲区内（1 MiB 对齐、≥1 MiB、不越界、不与已有分区重叠）；删除分区要求父盘可见、分区仍在父盘的分区列表里、无挂载/swap/holders/打开者；格式化只允许普通数据分区且未挂载。保护**沿 `partition_of`/`depends_on` 递归向上**：目标是 LV 时 LV 分区、PV 分区、物理盘三者必须同时被拒 |
| `internal/storage/capabilities.go`（新增） | 设备能力投影：`available`/`blocked`/`unsupported` 三态 + `details` 里带阻塞码；30 秒 TTL、键是 `(设备身份, snapshot_version)`；`storage.partition.create` 的投影**不带区间**，因此它的 `available` 只表示“结构上能做” |
| `internal/storage/signature.go`、`signature_device.go`（新增） | 签名识别：只读头部 68 KiB 与尾部 16 KiB，认 ext（0xEF53@1080）、XFS、Btrfs、swap v1/v0、LUKS1/LUKS2、LVM2、md 1.0/0.90 的固定魔数；认不出来的非零内容报 `unknown`（“有东西但我不认识”），**不报 blank**；全零才是 `blank`。读失败返回错误，由判决层记成 `SIGNATURE_UNREADABLE`。不唤醒休眠设备 |
| `internal/storage/busy.go`（新增）、`rdev_linux.go`、`rdev_other.go`（新增） | 占用检查：遍历 `/proc/<pid>/fd`，按 **`st_rdev` 设备号**与目标比对（不是路径——`/dev/sdb` 会被改名、被 `ln`、dm/md 的设备号与名字无关）；非 Linux 平台返回 `errPlatformUnsupported`，判决层据此拒绝，而不是返回“没人用” |
| `internal/storage/scan.go`、`root.go`、`collector.go`（改） | `linkNames()` 以**目录项**为事实来源读取 `holders/`、`slaves/`（Q172）；collector 把签名探针与占用检查接进预检输入 |
| `internal/agent/storageaction.go`（新增）、`registry.go`、`server.go`、`cmd/upanel-agent/main.go`（改） | 登记四个动作的解码 + `Plan`：全部 `RequiresReauth`、`Cancellable=false`，短语动词 `INIT`/`FORMAT`/`DELETE`（`create` 不要短语），锁一律 `disk:<整盘 id>`；`ExecutableActionNames()` 只导出有执行器的动作，因此四个名字**不进** `supported_actions` |
| `internal/storage/{assess,capabilities,busy,signature,scan}_test.go`（新增/改） | 判决、能力、占用、签名、拓扑关系读取的原生用例 |
| `tests/integration/p07-1-storage-guard-acceptance.sh`（新增，13 节） | 真机验收：先算出承载 `/` 的整盘并拒绝任何与它同源的一次性盘；能力真实性；系统盘与它 4 个分区的拒绝；空白盘允许的预检（影响、整盘锁、短语、再认证）且前后哈希不变；提交的两道门（未再认证 403、再认证后 501）与任务表形态；已有 GPT 时的拒绝与建分区的五种区间错误；整盘 ext4 / 整盘 swap / LUKS 头三种签名；挂载、活动交换区、dm holders、进程打开四类占用（含父盘占用传递到分区）；分区身份指纹的稳定性；权限；日志卫生；收尾后能力回到 `available` |
| `scripts/capture-storage-fixtures.sh`、`tests/integration/run-all.sh`（改） | 分区 holders 的捕获形状改成 `holders/list`（与真实形状一致）；`run-all.sh` 的 root 脚本清单补上 p06-1/p06-2/p06-3/p07-1（P06 三个脚本此前不在清单里，全量回归从来没跑到它们） |
| `tests/integration/p04-2-storage-acceptance.sh`（改） | 同机回归暴露的 4 处陈旧断言 + 1 处脆弱断言改成跨阶段不变量（详见下文“验收脚本自身的缺陷”第 8 条），断言数从 110 增加到 112 |
| `docs/DECISIONS.md`（Q170—Q176） | 决策与缺陷记录；`docs/CONTRACTS.md` 本轮**未改**——四个动作、锁、短语与再认证策略在 P03.1/P04.2 已写进契约（§5 表、§4.1），本轮的实现与之一致 |

**刻意不做的事**：不登记任何执行器（P07.2/P07.3）；不做挂载/卷管理（P07.4）；不实现 MBR 分区表修改；
不做应用/共享/备份依赖判断（P08/P11/P17）；不动 SMART 与采集节奏（P04.3 已定）。

**本轮被真机抓出的产品缺陷（全部改产品，不改断言）**

1. **交换区魔数读错位置**：签名层只在盘尾 16 KiB 找 `SWAPSPACE2`，真机上 `mkswap` 出来的整盘交换区被报成
   `unknown`。实测（`probe-swap-holder.sh`）魔数在**第一个页面的末 10 字节**：偏移 **4086 = 4096−10**、
   `wipefs` 报 `0xff6`、盘尾 64 KiB 全零、`swapon` 接受该设备。改成按 4 KiB/64 KiB 页两种边界精确比对，
   盘尾扫描保留；并补反例用例——“盘中间出现字面量 `SWAPSPACE2`”必须仍是 `unknown`（Q171）。
2. **`holders`/`slaves` 不是 `list` 文件**：原实现只读 `/sys/block/<part>/holders/list`，而 6.8 内核上
   **没有这个属性**（`No such file or directory`），`holders/`、`slaves/` 是符号链接目录。后果是
   “分区正被 dm/md 占用”完全看不见，`DEVICE_HAS_HOLDERS`/`DEVICE_IS_STACKED` 在真机上永远不触发。
   改成以目录项为事实来源（跳过汇总用的 `list` 条目），四处调用点统一（Q172）。

**验收脚本自身的缺陷（记录，不掩盖；断言只增不减）**

1. §3 用 `lsblk -no NAME` 枚举根盘分区，默认输出带 `├─` 树形前缀 → 一条也匹配不上 → 这一节**什么都没断言
   却报了 NOT RUN**（“这块盘上没有分区”）。改成 `-l`（注意 `-r` 与 `-l` 互斥），修好后 4 个分区各一条断言。
2. §5 直接提交存储动作，拿到 403 `REAUTH_REQUIRED` 而不是 501——四个动作都要求 5 分钟内再认证（契约如此）。
   改成两道门各自断言：未再认证 → 403 且盘上字节不变；再认证后 → 501。
3. §5 原先断言“提交后任务数不变”，与 Q159（受理即拒的作业**保留**一条 failed/rejected 用于追溯）
   直接矛盾；真机上确实多了一条 `failed`/`rejected`/`NOT_IMPLEMENTED` 的任务。改成“只多一条、停在 rejected、
   错误码正确、没有任何非终态任务”——这才是“不留永远跑不起来的任务”的正确表述。
4. §7 的 LUKS 现场等 `partition_table.type == "none"` 永远等不到：LUKS 头就在 LBA0，首扇区既不是 GPT/MBR
   也不是全零，真实值是 `unknown`（Q176）。
5. §8 的 `FREE_START_C` 从**分区**的详情里取整盘的空闲区（上一句 `device` 读的是分区）→ `ERR:TypeError`，
   把 `ERR:…` 当数值塞进 `start_bytes` → 400 → 后续断言全变成 `KeyError:'data'`。改成先等整盘 GPT 与空闲区
   进入快照、再读整盘详情，并对 `''/None/ERR:*` 一律显式失败（Q175）。
6. 快照新旧与能力 TTL：`settle` 现在必须同时满足“`collected_at` 晚于夹具开始时刻”与期望值；能力断言轮询到
   40 秒（能力投影 30 秒 TTL）；空闲区一律取**最大**的那块，不取 `free_regions[0]`（那是 GPT 之后 34 扇区
   → 1 MiB 的未对齐缝隙）（Q175）。
7. **原生测试采集脚本**第一次以 `upanel` 用户跑，报出 192 条“失败”——没有一条是产品问题：agent/api 的用例
   要 root 属主 0700 的代理数据目录，storage/monitoring 的用例要 `UPANEL_FIXTURES` 指向 `tests/fixtures`，
   api 包在 TCG 客机上跑 argon2id 登录超过 `-test.timeout 10m`。改成“不是 root / 没有夹具就 `exit 2`”、
   显式导出 `UPANEL_FIXTURES`、超时 1 小时，并从 root 重跑才得到 587/0/1。**记在这里是因为它差点把
   “环境没配对”写成“产品坏了”。**
8. **`p04-2-storage-acceptance.sh` 的四处陈旧断言与一处脆弱断言**（同机定向回归 105/5 暴露）：
   能力表被写死成 P04 时代的两条动作六条查询（P05/P06/P07.1 各自加过东西）、
   “`health.checked_at` 一律是 null”（P04.3 起物理盘启动即检查）、“`capabilities` 一律是空数组”
   （P07.1 起有设备能力投影）、§9 的 `collected_at` 用 §2 的旧基线（§3—§8 在 TCG 上要跑几分钟，
   `T3−T1` 必然超过 30 秒上限，断言变成了“前面几节跑得快不快”）。全部改成**跨阶段不变量**：
   存储查询在表里、**没有任何 `storage.*` 写动作被登记成可执行**（Q170 的不变量）、
   查询表有序无重复、“说健康就必须有检查时间”、设备能力只引用 `storage.*` 且状态在闭集内、
   §9 现取基线再等 6 秒比较。改完同机重跑 **112/0/0**（断言只增不减），
   证明 `holders`/`slaves` 读取方式的改动没有带坏只读拓扑。

**实际执行的检查与证据**：VM 24.04.5 上 **PASS=189 / FAIL=0 / NOT RUN=1**（唯一 NOT RUN 是
`PREFLIGHT_STALE`：没有执行器时提交在复核之前就 501）；VM 内核原生
**587 个顶层用例 PASS / 0 FAIL / 1 SKIP**（storage 162 / agent 161 / api 161 / operations 45 / files 58；
SKIP 是 `TestNestedMountRejectedByXDEV`——“需要 root 执行 mount，由 P06.1 验收脚本在 VM 上覆盖”，
该路径在 P06.1 的真机验收里已经跑过；五个交叉编译的测试二进制）；
宿主 `go test -count=1 ./...` **26 包 ok**、`gofmt -l` 无输出、`go vet ./...` 退出码 0、
`check.sh` **PASS 13 / FAIL 0 / NOT RUN 0**（Node 22.23.3 固定工具链）、前端 **29 文件 / 445 用例**、
`api/openapi.yaml` valid（7 warning，本轮未改契约文件）；同机定向回归 `p04-2-storage-acceptance.sh`
**112 / 0 / 0**（改动过 `scan.go`，必须证明只读拓扑没被带坏）。证据 `docs/evidence/P07/2026-10-21-p07.1/`。

**未验证（NOT RUN，逐条原因见 result.md §6）**：执行前复核（`PREFLIGHT_STALE`，没有执行器时提交在复核之前
就 501）、RAID（md）成员的 holders 现场、离线/只读设备、应用/共享/备份依赖、并发抢同一块盘、
22.04.5 VM、`go test -race`（本轮验收 VM 没有 Go/cgo 工具链，交叉编译的测试二进制也不带 `-race`）。

### P07.2（2026-10-22）— 真的写盘：初始化 GPT、创建分区、删除未使用分区（固定模板、对齐、udev 等待、两源核验）

**范围**：`STAGES.md` §9 P07.2。P07.1 证明“该拒的都拒”，这一轮证明“该写的写对了”：三个分区动作接上
执行器，在隔离 VM 的三块**一次性空盘**上真的初始化 GPT、建两个分区（第二个带名字）、删掉其中一个未使用
分区。格式化仍属 P07.3，`storage.filesystem.format` 依旧 501。

写盘的第一原则是**只用固定模板**：`label: gpt`、`--append --wipe-partitions never start=… size=…
type=linux name="…"`、`--delete <dev> <n>` 全是代码里的字面量；能进命令行的只有**已校验的数字**
（换算成 512 字节扇区、必须是 2048 的整数倍）和**已校验的名字**（可打印 ASCII、≤36 字节、不含 `"` 与 `\`）。
`sfdisk` 没有“脚本文件”参数（实测 util-linux 2.39.3），所以模板走 `syscmd` 新开的**有上限的 stdin 通道**
（64 KiB，注释里写明只能由固定模板填充、不得来自 HTTP 参数），而不是 `sh -c "echo … | sfdisk"`。

第二原则是**退出码不是验证**：写完先 `udevadm settle`，再等内核与 `/dev` 节点出现，然后重新读盘上的
GPT 头与条目表，与“写之前就按意图算好的期望形态”逐条比对，并且要求 sysfs 那份视图也对得上。
少一样就是失败，失败文案写“先核实实际状态，不要直接重新提交”。

| 文件 | 改动 |
|---|---|
| `internal/storage/write.go`（新增，943 行） | 写原语：`ExecuteWrite`（再扫拓扑 → 重算判决 → 比对身份指纹与状态版本 → 校验表类型与目标槽位 → 拼模板 → 执行 → udev settle → 等内核 → 两源核验）；`prepareWrite` 的参数校验；`expectedState`/`compareEntries` 的期望形态比对；`commandFailed`/`preflightStale`/`identityChanged`/`dependencyMissing` 四类错误 |
| `internal/storage/syscmd_adapter.go`、`http.go`（新增） | 把 `syscmd` 与 `HTTPError` 接到写原语上；错误码到 HTTP 状态的映射（`DEVICE_IDENTITY_CHANGED`/`PREFLIGHT_STALE` → 409、`DEPENDENCY_MISSING` → 503） |
| `internal/syscmd/syscmd.go`、`syscmd_test.go`（改） | `Spec.Stdin`（默认空，唯一例外是固定模板）、`MaxStdinBytes = 64 KiB`、`ErrStdinTooLarge`；补“模板原样到达子进程 / 超限被拒 / 空 stdin 不继承父进程”三类用例 |
| `internal/storage/types.go`、`table.go`、`collector.go`（改） | `partition_table` 新增 `disk_guid` 与 `disk_entries`；`DiskEntries` 用 `*[]GPTEntry`（Q177）；解析器补主 GPT 条目表（只读 LBA 1—33） |
| `internal/storage/write_test.go`（新增，727 行） | 假扫描器 + 假命令执行器的原生用例：模板字节、对齐拒绝、身份变化、状态版本变化、写后读回不一致、内核没跟上、命令失败文案、`DEPENDENCY_MISSING` |
| `internal/agent/storageaction.go`、`registry.go`、`runner.go`、`cmd/upanel-agent/main.go`（改） | 三个动作接上执行器；`storageSource` 把“能判决”和“会写”合成一个接口（漏实现是**编译错误**，不是运行时 501）；执行器把预检绑定的身份与状态版本原样传下去，错误按 `statusCarrier` 映射成能自己处理的原因 |
| `api/openapi.yaml`、`docs/CONTRACTS.md` §6.3、`web/src/api/storage.ts` | 契约：`StorageGPTEntry`、`disk_guid`/`disk_entries`；写动作的 `observed_state`/`data` 字段；前端 DTO 同步 |
| `tests/integration/p07-2-storage-write-acceptance.sh`（新增，13 节） | 真机验收（覆盖点见下） |
| `tests/integration/p07-1-storage-guard-acceptance.sh`（改） | P07.2 起三个分区动作会真的写盘，所以“过门之后必然 501”改到**仍然没有执行器**的格式化上，并把目标换成由测试手工具造出来的空白分区；能力断言改成跨阶段不变量（Q181） |
| `tests/integration/p04-2-storage-acceptance.sh`（改） | 能力表断言改成跨阶段不变量（Q181） |
| `tests/integration/run-all.sh`（改） | root 脚本清单补上 p07-2，并写明它必须排在 p07-1 之后（p07-1 需要空白盘，p07-2 会把盘写脏） |
| `web/src/components/realdata-storage-write.test.ts`、`web/src/fixtures/p07-2-vm/`（新增） | 用真机抓回的设备详情回放：条目表与内核分区按槽位一一对应、字节数是十进制字符串、起点 1 MiB 对齐、名字读得回来、格式化不以 `available` 出现 |
| `docs/DECISIONS.md`（Q177—Q182） | 缺陷与验收方法记录 |

**真机验收覆盖（13 节，178 条断言 + 1 NOT RUN）**：环境与安全守卫（三块一次性盘都不是系统盘、代理硬编码的
`/usr/sbin/sfdisk` 与 `/usr/bin/udevadm` 真的在）→ 代理 root / Web 非 root → 三个分区动作已可执行、格式化
仍不可执行 → 空白盘初始化 GPT（模板、两源核验、幂等重放不写第二次）→ 建分区（1 MiB 对齐、`/dev` 与 sysfs
当场就在）→ 第二个分区带名字（用 `sgdisk` 这个别的工具读回来）→ 删除（槽位不重排）→ 预检之后改盘
（409 `PREFLIGHT_STALE`，字节不变）→ 系统盘（409 `DEPENDENCY_BLOCKED`，不产生任务）→ 身份指纹稳定
→ 并发（只允许一个成功）→ 回归与日志卫生 → 收尾擦盘后能力回到 `available`。

**刻意不做的事**：不做格式化（P07.3）、不做挂载/导入卷/fstab（P07.4）、不做分区改名/改类型/调整大小、
不写 MBR、不做前端写操作交互（P07.5）。

**本轮被真机抓出的产品缺陷（改产品，不改断言）**

1. **“读到零条”与“没读到”在 JSON 里是同一件事**（Q177）：`disk_entries` 第一版是 `[]GPTEntry` 加
   `omitempty`，一张刚初始化、还没有分区的盘的条目表是空数组 → 被省略；而“没读到表”的设备本来就没这个键。
   验收正是用“盘上条目数是 0”来证明初始化清空了条目表，于是撞上 `KeyError:'disk_entries'`。
   改成 `*[]GPTEntry`，并让 `tableStateOf` 在“读到 GPT 却拿不到条目表”时直接报错——**不拿 nil 当零条**。

**验收脚本自身的缺陷（记录，不掩盖；每条都换成更难假通过的写法）**

1. 把 `confirmation_phrase` 的 `None` 当短语发出去，任务照样成功——服务端对**不需要**短语的动作根本不校验
   这一项，断言是绿的却什么也没验（Q178）。改成断言 `is None`，提交时传空串。
2. 删除用例在**空白**分区上断言 `data_loss`，产品只给 `partition_table_change`（Q179）。**产品是对的**：
   影响按现场状态算，先用 `mkfs.ext4` 造一个真签名、`blkid` 确认签名在，再断言 `data_loss` 与告警。
3. 用 `sgdisk -i 2` 的退出码判断分区是否消失，真机上删完仍然 rc=0（只在 stdout 打印
   `Partition #2 does not exist.`）（Q180）。改成读 `sgdisk -p` 的表格行 + 断言输出里出现 `does not exist`。
4. 能力表写死成固定动作名，P06 起就已经过期（Q181）。改成跨阶段不变量：闭集、无重复、写动作不出现在
   `supported_queries` 里、本阶段该可执行的恰好可执行。
5. **测试工具自己在最该报警的时候报绿**：`scripts/run-native-tests.sh` 只数 `--- FAIL` 行算失败，包超时
   panic / 崩溃时一条 `--- FAIL` 都不打印，于是 `rc≠0` 被算成“0 失败”、脚本退出码 0。改成“rc≠0 且一条
   `--- FAIL` 都没有”记一次失败并打印尾部 25 行，同时给每个包加 `-test.timeout`（默认 3600s，`UPANEL_TEST_TIMEOUT`
   可覆盖）——测试二进制被直接执行时 `-test.timeout` 的默认值是 0（无限），10 分钟那个默认值属于 `go test`。

**实际执行的检查与证据**：VM 24.04.5 上 **PASS=177 / FAIL=0 / NOT RUN=1**（唯一 NOT RUN 是
`DEVICE_IDENTITY_CHANGED`：身份来自序列号/WWN，VM 里造不出来，单测 `TestWriteRejectsIdentityChange`
用假扫描器覆盖）。关键读数：初始化后盘上 `FirstUsableLBA=2048`、`LastUsableLBA=16777182`、盘尾备份头存在、
首尾 4 MiB 哈希变了；建分区起点 1048576（1 MiB 对齐）、大小与请求一致、`/dev/<part>` 与 sysfs 当场就在、
`lsblk --bytes` 的内核大小一致；第二个分区的名字 `data` 由 `sgdisk -i 2` 这个**别的工具**读回；删除后槽位
不重排（1 号仍是 1 号）；幂等重放返回同一条任务且没有写第二次盘；预检之后用宿主工具改盘 → 409
`PREFLIGHT_STALE` 且盘上字节不变；系统盘 → 409 `DEPENDENCY_BLOCKED` 且不产生任务；全程 Web 日志里没有
`sfdisk|sgdisk|wipefs`，两份日志里都没有明文口令。**§10 的并发还撞到了 `runner` 里的第二次 `revalidate`**
（两个提交都被受理，输掉的那个停在 `step: revalidate`、`code: PREFLIGHT_STALE`），把 P07.1 里那条 NOT RUN
补上了机会性证据（Q182）。原生测试（VM 内核上跑 13 个交叉编译的测试二进制）**PASS=1134 / FAIL=0**、13 个包
`rc=0`（agentstore 72 / agent 240 / agentclient 39 / operations 57 / syscmd 16 / store 11 / api 200 / files 80 /
terminal 33 / monitoring 30 / storage **294** / journal 40 / logs 22；storage 从 P07.1 的 162 涨到 294，新增的
`write_test.go` 覆盖写盘执行器）。同机回归：`p04-2-storage-acceptance.sh` **通过 114 / 失败 0 /
未执行 0**、`p07-1-storage-guard-acceptance.sh` **PASS 194 / FAIL 0 / NOT RUN 1**（P07.1 当时是 189，差的 5 条
全是脚本自己这轮的改动：§2 +1 是能力断言换成跨阶段不变量，§5 +4 是**必须**换动作——P07.1 用“再认证后初始化
仍然 501”证明门，P07.2 起初始化有执行器了，再提交就真的写盘，所以改用仍无执行器的格式化）。
宿主 `check.sh` **PASS 13 / FAIL 0 / NOT RUN 0**、`go test -count=1 ./...` 26 包 ok（不吃缓存）、`go vet` 与
`gofmt -l` 干净、前端 **30 文件 / 450 用例**（新增 5 例真机载荷回放）、`api/openapi.yaml` valid（7 warning）。
证据 `docs/evidence/P07/2026-10-22-p07.2/`（`run.log`、`env.txt`、`check.txt`、`go-test.txt`、
`native-tests.txt`、`regression-p04-2.txt`、`regression-p07-1.txt`、14 份真实响应 payload、代理与 Web 日志、
`result.md`）。

**未验证（NOT RUN，逐条原因见 result.md §8）**：`DEVICE_IDENTITY_CHANGED`（要换硬件）、备份 GPT 表的一致性
（产品只读主表）、格式化（P07.3）、损坏的 GPT、写盘过程中的断电/进程被杀（P07.3 的验收点）、22.04.5 上
无任何 P07 证据、`go test -race`（验收 VM 没有 Go/cgo 工具链，交叉编译的测试二进制不带 `-race`）。

### P07.3（2026-10-22）— ext4/XFS 格式化：固定模板、超级块直读、错短语作废预检、禁止取消、二次身份检查

**范围**：`docs/STAGES.md` §9 P07.3 全句。P07.1 证明“该拒的都拒”，P07.2 证明“分区该写的写对了”，
这一轮把最后一个写动作接上执行器：`storage.filesystem.format` 不再是 501，它真的在一次性空盘上
`mkfs.ext4` / `mkfs.xfs`，并且**每一件事都要能被第二个人独立复核**。

第一原则仍然是固定模板：命令行里只有**已校验的文件系统名**（`ext4`/`xfs`）、**已校验的设备路径**
与**已校验的卷标**；卷标另有一道 `formatCommand` 的 `ErrUnsafeWrite` 闸，确认它只能出现在 `-L`
的位置。`mkfs.ext4` 不加 `-F`（真机实测：覆盖 ext4/xfs 签名、覆盖随机内容都不需要它），
`mkfs.xfs` 加 `-f`（不加会被已有签名拒掉）。

第二原则是**退出码 0 不算验证**：写完直接读超级块——ext4 在偏移 1024（`0x438` 处的 `53 ef`、
`s_uuid`、`s_volume_name`），XFS 在偏移 0（`XFSB`、`sb_uuid`、`sb_fname`）——再与内核侧的
`/dev` 与 `blkid` 这个**独立工具**的读数对一遍。产品自己不调用 `blkid`：它是验收的证人。

| 文件 | 改动 |
|---|---|
| `internal/storage/format.go`（新增，448 行） | 格式化执行器：`executeFormat`（再读设备 → 比对预检绑定的身份与状态版本 → 拼模板 → 经 `syscmd` 执行 → 探测超级块 → 两源核验 → `WriteReport`）、`compareFormatIdentity`、`formatObservedState`、`formatReportData`、`signatureKindFor`、进程中断的 `interruptedWrite`/`interruptedRun` |
| `internal/storage/fsprobe.go`（新增） | 超级块直读：`FilesystemIdentity`（类型、UUID、卷标、签名种类），不依赖 `blkid`。P07.4 的 UUID 校验与卷导入可以直接用它 |
| `internal/storage/syscmd_adapter.go`、`write.go`（改） | 格式化走同一条写原语与同一条错误出口 |
| `internal/agent/storageaction.go`、`registry.go`、`cmd/upanel-agent/main.go`（改） | 格式化登记执行器；标签上限判定进入预检；启动时记录接线的写原语路径（可审计） |
| `internal/agent/preflight.go`（改） | 目标绑定（身份指纹 + 状态版本）只在 `allowed=true` 时是硬性要求（Q190） |
| `internal/storage/assess.go`（改） | 拒绝路径写清“设备不在了就没有身份可绑”，`display_name` 始终保留 |
| `internal/storage/format_test.go`、`fsprobe_test.go`（新增）、`assess_test.go`、`internal/agent/storageaction_test.go`（改） | 执行器、超级块探测、标签边界、Q190 的回归用例 |
| `docs/CONTRACTS.md` §4.1、§6.4 | 两条新边界：**被拒绝的预检没有目标可绑定**（Q190）；**快照版本只覆盖结构，不覆盖未挂载分区的签名**（Q189） |
| `api/openapi.yaml` | `PreflightTarget.identity_fingerprint` 在 `allowed=false` 时允许为空 |
| `tests/integration/p07-3-storage-format-acceptance.sh`（新增，14 节 1296 行） | 本轮真机验收 |
| `tests/integration/p07-1-storage-guard-acceptance.sh`（改） | ① 格式化也有执行器之后，“过门之后必然 501”这种现场在真机上不存在了，改成显式 NOT RUN + 一条跨阶段不变量（四个存储动作都已可执行）；② 日志卫生断言排除代理启动时那行“磁盘写原语已接线”（它记录的是接线的路径，不是跑了命令——P07.3 验收跑过之后再跑 P07.1，这条第一次报假失败） |
| `tests/integration/run-all.sh`（改） | 补 p07-3，并写明 p07-1 → p07-2 → p07-3 必须串行（都写盘） |
| `docs/DECISIONS.md` | Q184—Q190 |

**真机验收覆盖（14 节 204 条断言 + 1 NOT RUN）**：环境与安全守卫（三块一次性盘都与承载 `/` 的整盘
不同源、代理硬编码的 `mkfs.ext4`/`mkfs.xfs` 真在、Web 非 root）→ 能力表里格式化已可执行 →
夹具（`sgdisk` 造 GPT + sdb1 1 GiB / sdc1 64 MiB / sdd1 2 GiB）→ ext4 格式化并两源核验 →
**确认短语**（错短语 400 且整份预检作废，盘上一个字节都没写）→ 同一分区改成 XFS（UUID 必须换、
`-f` 在 argv 里）→ **标签上限**（ext4 17 字节 / xfs 13 字节在预检之前就 400，不落任务不碰盘）→
**禁止取消**（冻住 mkfs → 取消 409 → 任务不是 `canceled`、`cancellable=false` → 解冻后跑完 `verified`）→
**错误退出**（64 MiB 上做 xfs：预检允许、mkfs 退出 1 → `COMMAND_FAILED`，消息带退出码、真实输出、
“盘上现在是”与“重新提交”提醒，分区首 4 MiB 哈希不变）→ **执行前二次身份检查**（结构变了 → 409
`PREFLIGHT_STALE` 且目标分区字节/UUID/文件系统全不变；换位 → 409 `DEVICE_IDENTITY_CHANGED`）→
**进程中断**（冻住 mkfs → `SIGKILL` 代理 → 重启后 reconcile 把操作标 `interrupted` +
`OPERATION_INTERRUPTED`、“实际状态未知”、**不自动重跑**）→ 回归与日志卫生（Web 日志里没有
`mkfs\|blkid`、两份日志里没有明文口令）→ 收尾（擦盘、无残留分区、不存在的设备的预检不是 500）。

**刻意不做的事**：不导入卷、不挂载、不写 fstab（P07.4）；不做 btrfs/f2fs；不做 `-m`/`-i`/条带等
格式参数；不做分区改名/改类型/调整大小；不做前端写操作交互（P07.5）。

**本轮被真机抓出的产品缺陷（改产品，不改断言）**

1. **对不存在的设备做预检回 500**（Q190）：第 7 轮 §13 的收尾探针打印 `allowed=ERR:KeyError:'data'`，
   翻响应是 `500 INTERNAL_ERROR`，代理日志写着 `预检结果不合法: 缺少目标身份指纹`。链路是：
   判决层**正确**地给出 `allowed=false` + `DEVICE_NOT_FOUND`（设备不在了就没有东西可以绑定，指纹留空
   是对的），而 `validatePlanResult` 无条件要求目标指纹与状态版本，于是把这条最普通的拒绝判成了实现
   错误。这个洞从 P07.1 就在，只是前两轮的验收从没对未知设备做过预检。修法是**绑定只在 `allowed=true`
   时是硬性要求**：绑定的唯一用途是让执行端复核“要动的还是预检时那个东西”，而提交端对 `!allowed`
   直接 409，被拒的预检根本不可能被执行。反向约束一条没放松（放行的预检仍必须有指纹与状态版本；
   “不允许执行却没有给出阻塞项”仍报错），并补了 `TestStoragePreflightForMissingDeviceIsNot500`
   与 `TestAssessDeviceNotFound` 的三条断言。

**验收脚本自身的缺陷（记录，不掩盖；每条都换成更难假通过的写法）**

1. **冻不住 mkfs，四次翻车**。要测“取消被拒”与“进程被杀”，必须在 mkfs 跑着的那几十毫秒里下手：
   ① 第一版用 shell 循环盯 `/proc`，一轮 20—40 ms，而 `mkfs.ext4` 1 GiB 只要 43—53 ms，整轮都能漏；
   ② 换 Python 之后 `$!` 只是外层子 shell，杀它会把 Python 留成孤儿继续轮询，第 6 轮它把 §9 那次
   `mkfs.xfs` 冻住、任务再也结束不了——改成按 Python 自己报的 pid 杀，并用 `*.watch` 文件做启动握手
   （冷启动几十毫秒，不握手就会在 import 期间错过窗口）；③ 第 12 轮（run8）又漏一次，原因是 `seen`
   让每个 pid 只看一次，而 fork 与 exec 之间 `comm` 还是父进程的名字，0.5 ms 的采样有相当概率落在
   这个几十微秒的缝里——表现极具误导性（窗口更宽的 2 GiB `mkfs.xfs` 漏了，1 GiB 的 `mkfs.ext4` 抓到了）；
   ④ 现在每轮都重新看当前子进程（`seen` 只用于写诊断轨迹），并且 §8/§11 各自最多试 3 次（每次都
   用新的预检；断言只在成功的那次上做，条数因此稳定），三次都没抓住才记 NOT RUN 并打印
   `frozen*.pid.trace`（pid + 当时的 comm）。
2. **测试工具自己把 500 吞成一句看不懂的话**：§13 的收尾探针用 `pallowed`/`pall` 直接取 `d["data"]`，
   遇到错误信封只会打印 `ERR:KeyError`——Q190 差点因此被漏掉。现在拆成显式断言（不存在的设备预检
   不是 500、阻塞码 `DEVICE_NOT_FOUND`、不编造指纹、说得清问的是谁）+ 一条容错的记录项。
3. **“预检之后 `wipefs` 掉签名”不能用来验第二道门**（Q189）：第 4、5 两轮的真机实测发现，
   `wipefs -a` 之后设备详情里的 `snapshot_version` **逐字节相同**（`sha256:576972147d41b6c5…`），
   于是预检仍然有效，该被拒的提交把盘格式化了。原因是 `storage.scan` 只给整盘与**已挂载**分区填
   `filesystem`，未挂载分区上的签名/UUID/卷标不进拓扑，也就不进版本。改成制造**结构**变化
   （同一块盘上新建一个分区）并加两条硬断言：版本**必须**变、被拒之后字节/UUID/文件系统**必须**不动。

**实际执行的检查与证据**：VM 24.04.5 上 **PASS=204 / FAIL=0 / NOT RUN=1**（唯一 NOT RUN 是
“内核与盘上条目不一致”那一路，属真实竞态，单测 `TestFormatRefusesPartitionThatMoved` 覆盖）。
关键读数：10a 的结构变化让快照版本从 `sha256:2683d3fa…` 变成 `sha256:23d53538…`，被拒之后目标分区
首 4 MiB 哈希、`ext4` 签名、UUID 全部不变；冻结现场 `comm=mkfs.ext4`、`stat=T`、任务停在
`running/executing`，此时取消是 409 且 `cancellable=false`；解冻后任务 `succeeded`/`verified`，
UUID 与上一次不同；SIGKILL 代理后操作落 `interrupted`/`OPERATION_INTERRUPTED`，任务数不变、
同一分区上没有第二条成功任务；64 MiB 上做 xfs 的失败消息里同时有退出码、`larger than 300MB`、
“盘上现在是”与“重新提交”。同机回归：`p07-1-storage-guard-acceptance.sh` 与
`p04-2-storage-acceptance.sh`（数字见证据目录的 `regression-*.txt`）。宿主 `check.sh`
**PASS 13 / FAIL 0 / NOT RUN 0**（Node 22.23.3）、`go test ./... -count=1` **26 包 ok**、`go vet` 与
`gofmt -l` 干净、storage 包单测 **PASS 328 / FAIL 0**（对 P07.2 提交 `882b6d5` 同口径是 291，+37）、`api/openapi.yaml` valid（7 warning）。
证据 `docs/evidence/P07/2026-10-22-p07.3/`（`run.log`、`env.txt`、`check.txt`、`native-tests.txt`、
`measurements.txt`、`measurements-mkfs.txt`、20 份真实响应 payload、代理与 Web 日志、`lsblk`、
两个探针脚本、`result.md`）。

**未验证（NOT RUN，逐条原因见 result.md §8）**：`DEVICE_IDENTITY_CHANGED` 的“内核与盘上条目不一致”
那一路（真实竞态，单测覆盖）、22.04.5 上无任何 P07 证据、`go test -race`（验收 VM 没有 Go/cgo 工具链）。

**遗留改进项（不在本轮改）**：把状态版本按设备粒度化（Q189 的后续），让“未挂载分区的签名变化”也能
进入版本；这会牵动 p04-2/p07-1 的快照语义，需要单独一轮。

### P07.4（2026-10-22）— 受管理卷：导入、挂载/卸载、fstab 最小修改、原子替换/备份/回滚、开机自动挂载、繁忙占用列表

**范围**：`docs/STAGES.md` §9 P07.4 全句。P07.1 证明“该拒的都拒”，P07.2/P07.3 证明“盘该写的写对了”，
这一轮把**已经存在文件系统的分区**纳管：登记记录 + `/etc/fstab` 托管块 + `/srv/upanel/volumes/<slug>`
三者由五个动作维护，每一步都要两源核验，失败要把原文件逐字节留住。

| 文件 | 改动 |
|---|---|
| `internal/storage/fstab.go`（新增，760 行）+ `fstab_linux.go`/`fstab_other.go` | fstab 解析与**最小修改**：托管块标记、条目渲染（固定选项顺序、`nofail` 恒在、`noauto` 随 `automount`、第六列 ext4=2/XFS=0）、块外内容逐字保留；`Write` = 比哈希 → 渲染候选 → 临时文件 → fsync → `findmnt --verify --tab <候选>` → 备份 → rename → fsync 目录；渲染结果与原文相同则直接返回（不写、不留备份）；**禁用 `mount -a`** |
| `internal/storage/volume.go`（新增，431 行）、`volumelist.go`（180）、`fetch.go`（改） | 卷记录、`slug` → 挂载点派生与校验、`desired_state`/`observed_state` 推导、乐观并发 `version`；列表/详情把“记录 + 采集快照”合成 `capacity`/`observed_error`（没挂载就没有 `capacity`） |
| `internal/storage/volumeassess.go`（新增，960 行） | 五个动作的判决层：导入前签名/分区/系统盘/挂载点/占用保护，挂载前“UUID 必须两源一致”，卸载前占用必须查得清，改配置/退出管理的 fstab 前置；**放行的预检都绑定身份指纹与状态版本**（Q199 的跨动作不变量） |
| `internal/storage/volumewrite.go`（新增，878 行） | 五个动作的执行器：`import`（写 fstab → 写记录 → 失败用备份换回）、`mount`/`unmount`（固定模板 `mount <dev> <mp>` / `umount <mp>`，完成后 `findmnt` + 拓扑重扫两源核验）、`configure`（只改 fstab 那一行）、`forget`（只删自己那一行，标记保留）；`umount` 非 0 时把**当时的占用者**重扫一遍写进错误消息 |
| `internal/storage/volumeholders.go`（新增，69 行）+ `busy.go`（改）+ `rdev_linux.go`/`rdev_other.go`（新增） | 占用扫描合流：`Scan`（fd 的 `st_rdev`，直接开块设备的进程）与 `ScanMount`（挂载点下文件的 `st_dev`，打开卷内文件的进程）合并去重；`busy` 不再把“挂着但空闲”算成忙；读不到 `/proc` 一律 fail-closed |
| `internal/storage/collector.go`、`capabilities.go`、`http.go`、`assess.go`（改） | 采集器把卷的 `observed_state` 与容量算进快照；能力表登记五个新动作；查询层新增 `NotFoundError`（“卷不存在”直通 404，不伪装成 500） |
| `internal/agent/volumeaction.go`（新增，254 行）+ `storagequery.go`（改）+ `registry.go`、`storageaction.go`（改）+ `cmd/upanel-agent/main.go`（改） | 五个动作的 Decode/Plan/Execute 登记；三个只读查询（列表、详情、占用）；装配完成时日志打 `受管理卷已接线` |
| `internal/agentstore/resources.go`（新增，208 行）+ `migrations/agentdb/0004_managed_resources.sql`（52 行） | 代理侧持久化：`managed_volumes` 表（schema version 4）、`SaveVolume`/`DeleteVolume`/`LoadVolumes` |
| `internal/api/storage.go`（改，269 行）+ `router.go`（改） | 三个只读接口：`GET /storage/volumes`（包一层 `{"volumes":[…]}`）、`GET /storage/volumes/:id`、`GET /storage/volumes/:id/blockers`（admin）；五个写动作**没有**直连入口，统一走 `/operations/preflight` + `/jobs` |
| `docs/CONTRACTS.md` §6.5（新增） | 五个卷动作的参数/再认证/锁/核验来源、fstab 托管块的形状、写路径每一步、`VolumeBlockers` 字段语义（`busy`：挂着但空闲不算忙）、两种占用为什么都要查 |
| `tests/integration/p07-4-storage-volume-acceptance.sh`（新增，1463 行，13 节 + 第二段） | 本轮真机验收（详见下） |
| `tests/integration/run-all.sh`（改） | 补 p07-4，并写明“都要写盘，必须串行” |
| `docs/DECISIONS.md`（Q191—Q203） | 两个占用口径缺陷、两条夹具事实、两段式验收与四条明确不验、快照等待盯分区、竞态前提、configure 漏绑定、两道门的真实行为、冻住 umount、重启后 `/run` 为空、`BIN_DIR` 的两个坑 |
| `internal/storage/{volume,volumeassess,volumewrite,volumelist,fstab,busy,volumefakes}_test.go`、`internal/agent/volumeaction_test.go`、`internal/api/storagevolumes_test.go`、`internal/agentstore/resources_test.go`（新增） | 判决层/执行器/fstab 渲染与写入/占用合流/查询与持久化的原生用例；Q191/Q192/Q199 的回归锁 |

**真机验收覆盖（第一段 13 节 267 条断言 + 第二段 38 条，全部通过）**：环境与安全守卫（两块一次性盘
都与根不同源、代理是 root、Web 账号存在）→ 配置与启动（监听 19474 的 Web uid=999、`受管理卷已接线`）→
能力与只读接口（五个卷动作可执行，空列表是 `[]`，形状不对 400、不存在 404）→ 夹具**全部由产品自己的
动作**造（GPT → 分区 → ext4，`blkid` 独立读回）→ 导入（条目逐字、非空行只多四行、块外逐字未变、
权限属主未动、备份内容等于写之前、无临时残留、导入不挂载、重复导入被拒且 fstab 不动）→ 挂载
（`findmnt` + 拓扑重扫两源、UUID 与 `blkid` 一致、真写文件）→ 卸载 → 重挂载（文件一致）→
占用（预检 `VOLUME_BUSY` 点名 pid 并与 `/proc/N/fd` 对照；**受理时**变忙回 400 `VALIDATION_ERROR`
且留一条 `failed/rejected` 任务；**执行时**冻住产品自己的 `umount` 制造 `target is busy`，
消息里带重扫的占用者）→ 开机自动挂载的 fstab 行与 `findmnt --verify` → 外部编辑导致 `PREFLIGHT_STALE`
且手工内容不被覆盖 → 候选不合格时目标文件逐字节不动 → 第二个卷不受影响 → 停面板后卷仍挂着 →
**真重启** → 第二段先证明“开机 26 秒时由 fstab 自己挂上”（`ActiveEnterTimestamp`）、UUID 一致、
标记文件 sha 不变、`noauto` 的卷没挂上、fstab 未被重启改动、`/run` 已清空，**然后**才启动面板，
确认面板看到同一事实，最后卸载 + 退出管理（只减一行、标记保留、留备份、之后 404）。

**刻意不做的事**：不做前端交互与页面（P07.5）；不改 `api/openapi.yaml`（P07.5 的第一件事——
版本仍写 `0.0.0-p06.3`，P07 的存储接口与新字段都还没进去）；不做 XFS 卷的完整生命周期
（P07.3 已验 XFS 格式化，fstab 第六列已按 XFS=0 实现并有单测）；不做 MBR 卷、LVM、加密卷、
网络文件系统；不做“面板侧依赖”（`apps`/`shares`/`backups` 三个来源尚未实现，恒为 `[]`）。

**本轮被真机抓出的产品缺陷（改产品，不改断言）**

**Q199 — `storage.volume.configure` 的预检没有绑定目标身份指纹 ⇒ 面板 500。** 第 8 节第一次跑
configure 拿到 `500 INTERNAL_ERROR`，代理日志 `预检结果不合法: … 缺少目标身份指纹：预检必须绑定一个
可复核的目标`。判决层放行、阻塞项为空、锁也对，就是漏了 `a.IdentityFingerprint = t.obs.IdentityFingerprint`
——另外四个动作都有，唯独 configure 漏了，因为此前从没有真机用例跑过它。修法：补上绑定 +
一条**跨动作不变量** `TestAssessVolumeActionsBindTargetIdentity`（遍历五个动作，凡放行的预检都必须同时有
指纹与状态版本；新增卷动作会被这个循环自动带上）。反向验证过：摘掉修复只有 configure 那一格红。
另外两个“缺陷”在 P07.4 之前就已修掉，但都是本轮写验收时发现的：Q191（占用只看 fd 的 `st_rdev`，
看不见打开卷内文件的进程）与 Q192（`busy` 把“有挂载点”算成忙，与预检自相矛盾）。

**验收脚本自身的缺陷（记录，不掩盖）**：① 快照等待盯整盘而不是分区（Q197，一真一假两条断言同时污染）；
② 竞态用例在第一个占用者还活着时就去拿预检（Q198，四条连环失败）；③ 查任务行的 Python 表达式
用多行形式但末行是裸字符串，`jget` 的 `exec` 路径不会把它放进 `_`，于是断言拿到空串——**测试工具
自己说谎**，比产品缺陷更耗时间；④ 第二段第一轮没建 `/run/upanel`（重启后 `/run` 是空的，
安装形态下由 systemd 的 `RuntimeDirectory` 建立，脚本直接跑二进制所以要自己建，Q202）；
⑤ 第二轮把 `BIN_DIR` 放进 `/home/upanel`（0750），`runuser -u upanel-web` 报 Permission denied，
表现成“代理启动 PASS、Web 起不来”——修法是统一 `/var/tmp/upanel-bin`（也扛得过重启，`/tmp` 重启后是空的）
并在第二段开头加一条 `runuser -u $WEB_USER -- test -x "$WEB"` 的前置断言（Q203）。

**实际执行的检查与证据**：VM 24.04.5 上第一段 **PASS=267 / FAIL=0 / NOT RUN=0**（EXIT=0），
第二段（真重启之后）**PASS=38 / FAIL=0 / NOT RUN=0**（EXIT=0），两段同一份脚本
（sha256 `a4144ef5…`，1463 行）。关键读数：开机 `07:55:24`、`srv-upanel-volumes-data.mount` 的
`ActiveEnterTimestamp` 是 `07:55:50`（由 fstab 自己挂上，那之后才启动面板）；导入后 `/etc/fstab`
非空行只多出托管块四行、块外逐字未变、备份内容等于写之前；受理时变忙是 **400 `VALIDATION_ERROR`**
（`当前状态不允许执行：VOLUME_BUSY`）且留一条 `failed/rejected` 任务；执行端冻住 `umount` 后吃到
`rc=32 target is busy`，消息含 `6108/sleep` 与“当前占用”。宿主 `check.sh` **PASS 13 / FAIL 0 /
NOT RUN 0**（Node 22.23.3 钉在 PATH 首位；不钉时唯一失败项是 node-version 的环境漂移）、
`go test ./... -count=1` **26 包 ok**、`go vet` 与 `gofmt -l` 干净、storage 包单测 **508**（P07.3 同口径 328，+180）、
`api/openapi.yaml` valid（7 warning，本轮未改它）。证据 `docs/evidence/P07/2026-10-22-p07.4/`
（`run.log`、`run-postboot.log`、`env.txt`、`check.txt`、`native-tests.txt`、
`measurements-umount-ebusy.txt`、真实响应 payload、代理与 Web 日志、fstab 各阶段快照、`lsblk`、
三个探针脚本、两次第二段失败日志、`result.md`）。

**未验证（NOT RUN，逐条原因见 result.md §8）**：导入后登记记录写失败的回滚、目录 fsync 失败、
`umount` 之后拓扑重扫失败、候选校验失败的其它形态、XFS 卷的导入/挂载（前四条都要“正好在这一瞬坏掉”
的环境，强行造会污染后续用例，由单测覆盖；XFS 由 P07.3 + fstab 单测覆盖）。
另外 22.04.5 上无任何 P07 证据、`go test -race`（验收 VM 没有 Go/cgo 工具链）。

**遗留改进项（不在本轮改）**：`api/openapi.yaml` 与前端 DTO 跟上 P07 的卷接口（P07.5）；
`apps`/`shares`/`backups` 三个依赖来源做出来之后，`busy` 的结论要跟着改。

### P07.5（2026-10-22）— 磁盘页面完整交互 + 系统外部独立检查 + 存储恢复文档 + openapi 补齐 P07

**范围**：`docs/STAGES.md` §9 P07.5 全句。P07.1—P07.4 已经让后端“该拒的拒、该写的写对、
卷能挂能卸”，但页面还只会只读展示。这一轮把九种动作接上页面，并给这台机器配一把**不经过面板的尺子**。

| 文件 | 改动 |
|---|---|
| `web/src/api/volumes.ts`（新增，413 行） | 卷 DTO、三个只读接口取数、`importCandidates`（**Q208 的修复点**：类型未知仍然列出）、`isManagedFilesystem`、`candidateFilesystemLabel` |
| `web/src/utils/jobresult.ts`（新增，185 行） | 一次存储写任务的 `result` 翻译成“一句话结论 + 证据行 + 恢复建议”；四个设备动作与五个卷动作结果形状不同但读法一致，集中一处解析；字段缺失不编造、不显示 `undefined` |
| `web/src/components/DeviceStorageActions.vue`（新增，442 行） | 设备四个动作入口：只认载荷里的 `capabilities`——`available` 才可点、`blocked` 显示服务端给的理由、`unsupported` 不出现；载荷里没有 `capabilities` 时**一个动作都不画**，只留版本不一致的说明（Q205） |
| `web/src/components/VolumeImportForm.vue`（新增，231 行） | 导入表单：候选、slug → 挂载点预览与校验、四个挂载选项、未挂载分区“类型读不到”的如实说明（`data-testid="import-unknown-type"`） |
| `web/src/components/VolumeCard.vue`（新增，409 行） | 单卷卡片：`desired_state` 与 `observed_state` 分开显示、挂载才有容量读数、没挂写「没有容量读数」而**不是 `0 B`**、五个动作入口按判决状态渲染 |
| `web/src/components/VolumeSection.vue`（新增，115 行） | 卷区块：读到了给结论（卷 / 「还没有受管的卷」），不停在「正在读取受管卷」 |
| `web/src/components/StorageActionDialog.vue`（新增，401 行） | 确认框：`/operations/preflight` → `/jobs`（带 `Idempotency-Key`）的编排、预检 120 秒过期后重新预检、要短语的必须原样敲、要再认证的提示重新输密码、终态用 `jobresult` 翻译 |
| `web/src/views/StorageView.vue`、`StorageDeviceView.vue`、`web/src/components/DeviceDetailPanel.vue`、`web/src/api/storage.ts`（改） | 列表页挂上卷区块；设备详情页把“当前阶段没有可执行的动作”换成「服务端判定的动作」；判定全部来自载荷 |
| `web/src/fixtures/p07-5-vm/`（新增，README + 11 份载荷） | 真机抓回的页面载荷：拓扑、整盘详情、分区详情（未挂载/已挂载）、卷列表（空/一个挂着）、卷详情（挂着/没挂）、占用列表（忙/闲）、代理能力表；README 写明“别把具体 UUID/pid/字节数写死” |
| `web/src/components/realdata-p07-5.test.ts`（新增，16 条）+ `volumes.test.ts`（22）、`storage-write.test.ts`（24）、`jobresult.test.ts`（新增）+ `realdata.test.ts`/`storage.test.ts`/`realdata-storage-write.test.ts`（改） | 真载荷回放与单测；把“夹具比现实干净”的地方按真机的两种空形态补齐 |
| `web/scripts/smoke.mjs`（改） | 浏览器冒烟补两条：受管卷区块读到了结果、设备详情页有「服务端判定的动作」 |
| `scripts/storage-external-check.sh`（新增，381 行） | **系统外部独立检查**：只读 `/etc/fstab` + `findmnt --mountpoint` + `/proc/self/mountinfo` + systemd 单元 + 备份权限；支持 `--fstab <副本>` 与 `--expect` 做故障注入；退出码 0 = 一致、1 = 有 FAIL |
| `tests/integration/p07-5-storage-page-acceptance.sh`（新增，1098 行，POSIX sh，root 跑） | 本轮真机验收，11 节（详见下） |
| `docs/STORAGE_RECOVERY.md`（新增，263 行） | 恢复文档：机器起不来、卷丢了、fstab 坏了、退出管理、回滚备份时人怎么一步步做；§2 第三步用 `--mountpoint`（Q204） |
| `api/openapi.yaml`（改，+347 行） | `GET /api/v1/storage/volumes`、`/storage/volumes/{id}`、`/storage/volumes/{id}/blockers` + 五个 schema（`StorageVolume`、`StorageVolumeDependency`、`StorageVolumeList`、`StorageVolumeBlockers`、`StorageBusyProcess`）；`capacity`/`observed_state`/`dependencies` 的空值语义写进 description；版本 `0.0.0-p06.3` → `0.0.0-p07.5`、`stage` → `P07.5` |
| `docs/DECISIONS.md`（Q204—Q209） | `findmnt --target` 的语义、`capabilities` 为空时页面说什么、外部检查的故障注入只动副本、夹具从真机抓回、**Q208 产品缺陷**、Q209 想当然的收尾期望 |

**真机验收覆盖（11 节 205 条断言，全部通过）**：环境与安全守卫（两块一次性盘与根不同源、代理 root、
Web 非 root）→ 配置与启动 → 页面依赖的载荷（动作判定闭集、空列表是 `[]`、形状不对 400、不存在 404）→
夹具由产品自己的动作造（GPT → 分区 → ext4）→ 导入两个卷（fstab 最小修改、备份、挂载点由 slug 派生、
重复导入被拒）→ 挂载（容量只在真挂着时有、文件真写进去）→ 占用列表（页面直接显示真实 pid）→
**外部独立检查**（一条不经过面板的命令把同一批事实再读一遍：UUID → 设备、`--mountpoint` 与
`--target` 两路一致、`/proc/self/mountinfo`、真正是挂载点、fsck 顺序、`nofail`、systemd 单元、
备份 0600；再在**副本**上注入五种坏法各抓一条，并附“干净副本退出码 0”）→ 卸载 → 重挂载（文件一致）→
退出管理（托管块还在但 0 条、块外逐字未变、外部检查仍一致）。

**本轮被真机抓出的产品缺陷（改产品，不改断言）**

**Q208 — 纳入管理的候选永远是空的。** 第一次真机跑，候选断言没红是因为我根本没在那儿断言候选；
真正暴露它的是把第 3 节的设备详情载荷读出来看：刚 `mkfs.ext4`、还没挂载的分区 `filesystem.type` 是
`null`（`CONTRACTS` §6.1 第 231 行：未挂载不调 blkid、不猜），而 `importCandidates` 当时的判据是
“`filesystem` 非 null 且 `type` 非 null” + “已挂载的排除”——**两个条件互为补集**，一块盘要么挂着
（被排除）要么没挂（读不到类型，被排除），于是导入表单在任何真机上都是空的。前端单测全绿，因为夹具里
的未挂载分区被我手写成了 `type: 'ext4'`。修法：类型已知且不是 ext4/XFS ⇒ 排除；类型未知 ⇒ 仍然列出
并如实说明；已挂载 ⇒ 排除。锁它的测试按真机的两种空形态各钉一条，反向验证过（改回旧写法恰好多两条红）。
**教训：一个筛选条件如果不是拿真机载荷回放，它测的是我脑子里的设备，不是机器上的设备。**

**验收脚本自身的缺陷（记录，不掩盖）**：① 外部检查第一版用 `findmnt --target` 判“挂没挂”——
`--target` 的语义是“哪个挂载**覆盖**了这个路径”，对任何存在的路径都返回上一层挂载，于是没挂的挂载点
被读成 `/dev/sda1` 并报出一条假 FAIL；改成 `--mountpoint` + `--target` 的 `TARGET` 必须等于它自己 +
`/proc/self/mountinfo` 两个独立证人（Q204）。② 收尾断言“最后一个卷退出管理后托管块应该消失”是想当然，
真机是 `BEGIN=1 END=1` + 条目 0（P07.4 的 `forget` 只删自己那一行，标记刻意保留，`STORAGE_RECOVERY` §3.3
就是这么写的）——**断言和文档打架，先去读文档**，断言改成“块还在、里面 0 条、块外逐字未变”（Q209）。
③ 脚本第一版是 CRLF，`scp` 到 VM 上直接 `18: : not found` / `19: set: Illegal option -`（28 条 FAIL 全是这个的连带），转 LF 后才看到真结果。

**实际执行的检查与证据**：VM 24.04.5 上 **PASS=205 / FAIL=0 / NOT RUN=3**（EXIT=0）。关键读数：
挂载后卷详情 `capacity.total_bytes=2040373248`、`used_percent=6.1`；同一卷卸载后 `capacity: null`；
未挂载分区详情 `filesystem.type is None`；外部检查在真 `/etc/fstab` 上跑三遍（一挂一空 `28 项断言跳过 0`、
退出管理后、全空）每遍 `RESULT 一致`；五种坏副本各 `RESULT` 不一致且退出码 1，干净副本退出码 0。
宿主 `check.sh` **PASS 13 / FAIL 0 / NOT RUN 0**、web **35 文件 / 552 用例**、
`go test ./... -count=1` **26 包 ok**（本轮**没改 Go 代码**，storage 508 / agent 278 / api 211 与 P07.4 同口径、
一致是预期的不是漏跑）、`go vet` 与 `gofmt -l` 干净、`GOOS=linux go build` exit 0、
`api/openapi.yaml` valid（7 warning：`no-unused-components` 4、`operation-4xx-response` 2、`operation-2xx-response` 1）。
证据 `docs/evidence/P07/2026-10-22-p07.5/`（`run.log`、`env.txt`、`check.txt`、`native-tests.txt`、
`payloads/` 58 份真实响应 JSON + 4 份外部检查原始输出、`logs/` 里的 agent.log/web.log/`ext-*.txt`/fstab 快照/lsblk/kernel、`result.md`）。

**未验证（NOT RUN，逐条原因见 result.md §7）**：浏览器里的真实渲染（本脚本验载荷；DOM 由 SSR 回放 +
`smoke.mjs` 覆盖）、开机自动挂载 / 跨重启（P07.4 已用一次真重启证明，本轮不重复烧重启）、
九种动作的每一次点击（设备动作由 P07.2/P07.3 覆盖，本脚本验的是载荷形状与卷的完整生命周期）。
另外 22.04.5 上无任何 P07 证据、`go test -race`（验收 VM 没有 Go/cgo 工具链）。

**遗留改进项（不在本轮改）**：`apps`/`shares`/`backups` 三个依赖来源做出来之后 `dependencies` 与
`busy` 的结论要跟着改；XFS 卷的完整生命周期；外部检查脚本是只读证人，不做修复（修复步骤在
`docs/STORAGE_RECOVERY.md`）。

### P08.1（2026-10-22）— Samba 能力检测、受管理账号、密码秘密通道、POSIX ACL、共享定义与配置版本

**范围**：`docs/STAGES.md` §10 第 1 条全句。这一阶段的承诺很具体：面板要说真话（Samba 到底在不在）、
密码只经过一次且只走标准输入、账号与目录权限要真的做出来并能用**不经过面板的命令**读回来，
而共享**此刻还不对外提供访问**——最后这条是本阶段最容易撒谎的地方，所以它由“反过来证明没做”来验。

| 文件 | 改动 |
|---|---|
| `internal/shares/types.go`（356 行） | DTO、动作名、阻塞/告警码闭集（本轮新增 `ACCOUNT_LOOKUP_TIMEOUT`）、`Assessment`/`Store` 接口 |
| `internal/shares/capability.go`（271） | 能力探针：`smbd`/`smbpasswd`/`pdbedit`/`setfacl`/`getfacl` 逐个走**固定候选绝对路径**（不落 PATH、不可配置），30 秒 TTL；`config_write` 恒 `unsupported` |
| `internal/shares/commands.go`（145） | 全部外部命令的 argv 模板；**密码只出现在一处**（`smbpasswd` 的 stdin）；预检 3 秒 / 执行 20 秒两套观察预算（Q223） |
| `internal/shares/account.go`（249） | `getent passwd`、`passwd -S`、`pdbedit -Lv` 的解析与读回模型 |
| `internal/shares/acl.go`（266） | `setfacl --set` 整份替换 + `getfacl -cp` 读回 + `mask` 覆盖判定 + `DiffACL` |
| `internal/shares/definition.go`（314） | 确定性渲染（账号按名字排序、键序固定）、`config_hash = sha256(渲染结果)` |
| `internal/shares/assess.go`（742） | 五个动作的判决：能力、卷与挂载、路径形状/越界/符号链接、名单非空、重名、被引用、系统里同名账号 |
| `internal/shares/write.go`（825） | 五个动作的执行器：`useradd` → 三条读回 → `smbpasswd -s -a` → 读回 → 落库；ACL 写完读回；失败只回滚自己造的东西 |
| `internal/shares/fetch.go`/`system.go`/`http.go` | 查询取数、系统读回接缝、带判决的错误（409 `DEPENDENCY_BLOCKED` / 400 `VALIDATION_ERROR`） |
| `internal/agent/shareaction.go`（292）、`sharequery.go`（123） | 五个动作与三条查询登记进代理的固定登记表 |
| `internal/agentstore/sharestore.go`（384） | `shares` 与 `managed_accounts` 两张表的读写，版本自增 |
| `internal/api/shares.go`（270） | Web 侧账号/共享的读视图与 `POST /api/v1/secrets` |
| `migrations/agentdb/0005_shares.sql`（82） | 两张表；**没有密码列、没有哈希列** |
| `api/openapi.yaml`（改） | 账号、共享、秘密、能力四组接口与 schema；版本 `0.0.0-p08.1`、`stage: P08.1` |
| `docs/CONTRACTS.md` §7.1（改） | 判据补齐：allowed 预检必须绑定目标身份指纹与状态版本、告警码 `ACCOUNT_LOOKUP_TIMEOUT`、观察预算按阶段分 |
| `docs/DECISIONS.md`（Q214—Q223） | 能力探针不猜路径、秘密用途枚举只在代理、ACL 整份替换、渲染确定性、**Q214/Q220/Q221/Q223 四个产品缺陷**、Q222 采集器性能问题 |
| `tests/integration/p08-1-share-acceptance.sh`（1442 行，POSIX sh，root 跑） | 本轮真机验收，11 节 |

**真机验收覆盖（第五遍 382 条断言，全部通过，`FAIL=0 NOT RUN=4`）**：环境与安全守卫（一次性盘与根不同源、
代理 root、Web uid 999）33 条；配置与启动 18 条；能力检测逐项与系统事实对照（版本号 vs `smbd --version`、
路径真的存在、`config_write=unsupported` 且一个字节没写）23 条；夹具（产品自己的 GPT → 分区 → ext4 →
导入 → 挂载）38 条；秘密通道 24 条；受管理账号 79 条；共享定义与 ACL 51 条；页面载荷 19 条；
拒绝路径 44 条；禁用/改回/删除 50 条；收尾不变量 3 条。**第 10 节的 3 条是整轮的不变量**：
代理停掉后 socket 已清理、`smb.conf` 首末 `sha256` 逐字节相同、日志与两个数据库（含 WAL）零明文命中。

**本轮被真机抓出的产品缺陷（改产品，不改断言）**

1. **新建受管理账号的预检永远 500**（Q214）：`assessAccountApply` 只在更新分支设 `StateVersion`，
   新建分支没有任何东西可绑，`allowed=true` 却没有版本，被 `validatePlanResult` 判成实现错误。
   修法是给新建绑定一个**可复现**的版本：`absent:<名字>`——它说的正是“此刻还没有它”，
   执行端重新判决会得到同一串。新建共享同洞，一起修。
2. **删除账号的预检永远 500**（Q220）：`assessAccountDelete` 设了 `TargetID`/`DisplayName`/`StateVersion`，
   漏了 `IdentityFingerprint`。这是 Q214/Q190 的**第三次同类**：校验器要的东西，每条 allowed 的路径
   都得给全。本轮因此把四个判决函数的 allowed 路径逐条查了一遍并写进 result.md §4.1。
3. **一次观察超时把整条操作变成 500**（Q221）：`getent` 超时直接冒成错误；名字已经不合法时还去问系统。
   修法两件：超时重试一次（预检总预算仍 3 秒，拆成两次 1.5 秒；退出码类失败不重试）、
   名字不合法时**短路跳过这次观察**（不会改变判决的等待，一秒都不该付）。
4. **两处观察用了同一个预算**（Q223，第四遍仍然倒在这里）：预检在 Web 等着的 10 秒链路上，执行前复核
   在任务自己的预算里，两处的等待能力根本不同却写了同一个数。修法：预算按阶段分开（预检 3 秒、
   执行 20 秒），并且**预检查不出结果不再当错误**——降级成 `ACCOUNT_LOOKUP_TIMEOUT` 告警，
   因为“不接管既有账号”这条不变量由执行阶段兜底：长预算复核一次，`useradd` 对已存在的名字以退出码 9
   失败，`createAccount` 据此回 409 `DEPENDENCY_BLOCKED` 并点名 `ACCOUNT_NOT_MANAGED`，
   **绝不继续设置 Samba 密码**。三条回归测试把这三件事各钉一条。

**已知问题（未根治，明确记录）**：Q222 —— P05.1 的服务采集器在 2 vCPU 机器上每 10 秒有一轮约
6 秒墙钟 / 3 秒 CPU 的 `systemctl show` 突发（每个单元一次 D-Bus 往返，~21 ms，`systemctl` 侧没有便宜的
批量打法），把 `getent` 挤到过 4.7/7.3/7.5 秒。本轮做了隔离实验（代理停/跑/停，23—29 ms → 平均 106 ms、
最长 2894 ms → 23—29 ms）把因果钉死，但**没有改采集器**：降频要动 CONTRACTS 第 690 行的 10 秒契约，
换采集方式要重做 P05.1 的 DTO 与验收。P08.1 侧已经不会因为它出错（见 Q223），它也不再挡 P08.2。

**验收脚本自身的缺陷（记录，不掩盖）**：五条，都会让脚本骗自己——预检/提交失败后不清
`job.json`（于是后面每条 job 断言读到的都是上一轮的作业）、请求体拼接丢闭合引号、`$( )` 吃掉渲染结果
结尾换行后算 sha256、空串里“找不到 D”被当成通过、拿机器上现成的保留名 `upanel` 当“非面板建的账号”夹具。
每条的改法见 result.md §5。

**未验证（NOT RUN，逐条写原因）**：从另一台机器用 SMB 客户端连上来（P08.1 不写配置、共享不对外提供访问；
`P00-VM-D` 尚未建立）、写配置/`testparm`/原子替换/重载/连接状态（P08.2/P08.3）、浏览器里的真实渲染
（本脚本验载荷，DOM 由 SSR 回放与 smoke 覆盖）、能力 blocked 分支（本机装了 samba，blocked 由注入式单测覆盖）。

证据：`docs/evidence/P08/2026-10-22-p08.1/`（`run.log` 与 `payloads/`、`logs/` 是真机原始产物，
`check.txt` 是宿主 `check.sh`、`native-tests.txt` 是 `go test ./...` 与 `internal/shares` 的 54 个用例）。
宿主 `sh scripts/check.sh` **PASS 13 / FAIL 0 / NOT RUN 0**；`go test ./... -count=1` 全部 ok。

### P08.2（2026-10-22）— 最小 include 接入、`testparm`、原子更新、重载、共享连接状态

**做了什么**

| 文件 | 内容 |
| --- | --- |
| `internal/shares/config.go`（400） | 托管配置的渲染：`[global]`（`server min protocol = SMB2`、`map to guest = never`、`usershare allow guests = no`）+ 每个启用共享一段；整份重写，不做增量拼接 |
| `internal/shares/configwrite.go`（746） | 写链路：`.upanel-new` + fsync → 候选 `testparm -s` → 备份 0600（留 40 份）→ rename → 目录 fsync → 接 include → 合并 `testparm -s` → `smbcontrol all reload-config` → 逐参数读回；全局锁 `samba:config` |
| `internal/shares/assessconfig.go`（181） | 配置写入的预检判决（候选能否通过 `testparm`、include 现状、服务状态、目录前提） |
| `internal/shares/service.go`（80） | `smbd` 状态与重载判定（`active|inactive|unknown`；停着就不重载，且不许说“已生效”） |
| `internal/shares/connections.go`（261） | `smbstatus` 会话表解析（id 是字符串、方言/签名/建立时间）、与面板报数交叉核对、`SMB_SERVICE_INACTIVE` |
| `internal/shares/write.go` | `ensureShareDir` 重写 + `ensureAncestorsTraversable`（见 Q233） |
| `internal/shares/assess.go`、`definition.go`、`types.go` | 穿越权限告警 `SHARE_PATH_NOT_TRAVERSABLE`、环回告警 `SHARE_HOSTS_ALLOW_LOOPBACK`、`cidrsCoverLoopback` |
| `internal/shares/system.go` | `Options.Chmod`（可注入，叶子/上级目录权限与测试用同一套语义） |
| `internal/agent/shareaction.go`、`sharequery.go`、`cmd/upanel-agent/main.go` | 动作/查询接线；启动日志不再硬编码 `config_write`，改由能力探针判定 |
| `internal/api/shares.go`、`router.go`、`api/openapi.yaml` | 共享应用/停用/删除、连接状态接口；契约状态 `stage: P08.2` |
| `docs/CONTRACTS.md`、`docs/DECISIONS.md` | 两个新告警码；两块新条文（目录权限的两种语义、`allowed_cidrs` 的真实边界）；Q232–Q234 |
| `tests/integration/p08-2-share-config-acceptance.sh`（1346 行） | 15 节 237 条断言的真机验收 |

**契约落地要点**

- `smb.conf` 只多**两行**（一行注释 + 一行 `include = /etc/samba/upanel.d/upanel.conf`），
  原有内容逐字节保留——断言是“新文件是旧文件的**前缀延伸**”，不是“包含”这种会被改写骗过的判据。
- 报告的六个值是分开的：`samba_config=written|unchanged`、`include=installed|present`、
  `reload=done|skipped_service_inactive|failed|skipped_unchanged`、`service=active|inactive|unknown`、
  `served`、`outcome`。服务停着时配置照写，但 `outcome=accepted`、`served=false`、不带 `verified_at`。
- 重放（内容没变）**不写文件、不重载、不产生新备份**，两个哈希都不变。
- 权限两套语义：叶子共享目录 `0700` + `setfacl`（真相在 ACL 上），上级目录 `0711`（可穿越、不可列出）。

**真机抓出的产品缺陷（改产品，不改断言）**

1. **上级目录 `0700` 让真实客户端 `ACCESS_DENIED`**（Q233）：`MkdirAll(dir, 0700)` 把缺失的父目录一起建成
   `0700`，共享账号既不是 owner 也不是 group 成员，连 `x` 都没有。`namei -l` 直接看到的
   `/srv/upanel/volumes/data/team` 是 `drwx------ root root`。7 条断言失败背后**只有 1 个根因**，
   记账时两者分开写（这是 Q233 定下的记账规则）。修法：叶子 `0700` 不变，**上级 `0711`**；
   对历史 `0700` 上级做 `perm|0o011` 修复并上报 `observed.path_traversal_repaired`。
2. **`hosts allow` 对环回永远放行**（Q234，契约事实 + 脚本缺陷）：`man 5 smb.conf` 写明 `127.0.0.1`
   始终被放行，所以“从本机连、期望被拒”的断言本身是错的。脚本改成用 veth + network namespace 造真实
   非环回客户端 `10.99.0.2`，并补一条正对照（放行后**同一个**客户端必须连得上）；产品侧新增非阻断告警
   `SHARE_HOSTS_ALLOW_LOOPBACK`，**不**偷偷替用户加 `hosts deny`。

**四遍运行**：#1 脚本在 §4 中止（`set -u` 下引用函数局部变量 `_ACCT_RW`）；
#2 **PASS=211 / FAIL=7**（Q233）；#3 **PASS=225 / FAIL=1**（Q234）；#4 **PASS=237 / FAIL=0 / NOT RUN=0**。
四份日志都留在 VM 的 `/var/tmp/p08-2-run{,-1,-2,-3}.log`，最终那份进了证据目录。

**逐节断言数（共 237）**：环境与安全守卫 38、配置与启动 14、能力检测 25、夹具（空盘→GPT→ext4→导入→挂载）14、
受管理账号与秘密通道 10、`share.apply` 36、真实 SMB 客户端 21、连接状态 12、幂等重放 14、名单变更 14、
`smbd` 停着 13、`share.disable` 9、`share.delete` 4、第二条共享收尾 4、收尾不变量 9。

**未验证（NOT RUN，逐条写原因）**：`P00-VM-D`（独立 SMB 客户端 VM）仍未建立——SMB 客户端行为改用同机
真实 `smbclient` + netns 客户端验证，覆盖同一批系统调用与同一条 SMB 路径，但**不是**跨机器真实网络路径；
Windows/macOS 客户端兼容性未测；`P08.3`（volume 绑定、卸载/格式化依赖阻断、运行时脱盘）与 `P08.4`
（共享 UI）未开始；`internal/storage` 的 `TestOSCapacityReaderNetworkTimeoutQuarantines` 早前失败过一次，
随后多次重跑（含本轮全量）均 ok，原因未定位，记录在案。

证据：`docs/evidence/P08/2026-10-22-p08.2/`（`run.log`+`payloads/`+`logs/` 为真机原始产物，
`env.txt` 为环境与二进制/脚本 `sha256`，`check.txt` 为宿主 `check.sh`，`native-tests.txt` 为
`go test ./...` 与四条新测试，`result.md` 为逐节结论与两个缺陷的完整复盘，`probe5.sh`/`probe5-output.txt`
为 Q234 的可复现记录）。脚本 `sha256` `6cbe92db…` 与仓库中那一份一致——跑的就是提交的这份。

### P08.3（2026-10-22）— 共享与 `volume_id` 绑定、启用检查、启动依赖、运行时脱盘、依赖阻断

**做了什么**

| 文件 | 内容 |
| --- | --- |
| `internal/shares/mountgate.go` | 挂载门：只有 `storage.VolumeStateMounted` 才渲染那一段；`unknown`、空串、`unmounted` 一律不渲染（不对称是刻意的：少一份共享可见可修，多一份指向空目录的共享是静默误导） |
| `internal/shares/reconcile.go` | 运行时脱盘的**只撤不恢复**循环：30 秒一轮、首轮立刻跑、持有 `samba:config` 锁；记 `RuntimeRevocation{at,volume_id,volume_name,volume_state,reason}`，保留第一次的现场，重放不写文件、不重载、不动 `updated_at` |
| `internal/shares/startupdeps.go` | 开机依赖：drop-in `/etc/systemd/system/smbd.service.d/upanel-shares.conf`（目录 `0755`、文件 `0644`），`RequiresMountsFor=` 一行四个路径、排序去重、原子写、`daemon-reload` 必做；列表清空时整份删除并再 reload；报告 `startup_deps=written\|unchanged\|removed\|failed\|skipped` |
| `internal/shares/types.go`、`write.go`、`assess.go`、`assessconfig.go` | `RuntimeRevoked`、`startup_deps`/`startup_paths` 字段；应用与撤销两条路都更新 drop-in；预检按挂载门判决 |
| `internal/agent/sharewatch.go` | 监视循环接线（启动日志写明“卷离线时撤销共享配置；不自动恢复”） |
| `internal/agent/sharedeps.go` | 存储侧对共享的反向约束：卸载只看**启用中**的共享，格式化与退出管理看**任何**共享；prober 为 nil 或出错 → `VOLUME_DEPENDENCIES_UNKNOWN`（阻塞） |
| `internal/storage/assess.go`、`volumeassess.go`、`volumelist.go`、`volume.go` | 依赖阻断码与 `/blockers` 的 `busy`/`shares[]`；`dependencies[]` 清零后 `busy=false` |
| `internal/agentstore/sharestore.go` + `migrations/agentdb/0006_share_runtime_revoked.sql` | 撤销现场落盘（Q240）：迁移加列 `runtime_revoked_json`、列常量增列、`SaveShare` 序列化、`scanShare` 反序列化并在读不懂时报错而不是猜空 |
| `internal/shares/configwrite.go` | `writeFileSync` 写完显式 `Chmod(perm)`（Q243）：权限由代码钉住，不由启动它的 shell 决定 |
| `docs/CONTRACTS.md` §7.3 | 三处补记：托管配置与 drop-in 都是 `0644` 且与 umask 无关；drop-in 与托管配置同生同死；开机拉起挂载的是 `RequiresMountsFor` 而不是 fstab |
| `tests/integration/p08-3-share-volume-binding-acceptance.sh`（1222 行） | 14 节 207 条断言的真机验收 |
| `tests/integration/p08-3-reboot-order.sh`（395 行） | 三阶段（`pre`/`post`/`clean`）重启顺序验收，中间是一次真 `reboot` |

**契约落地要点**

- **“卷没挂”不是一个提示，是一个不渲染的理由**：卷不在 `mounted` 状态时共享段根本不出现，
  再点“应用”会被预检 `VOLUME_NOT_MOUNTED` 挡下，理由写明“此时共享目录只会落在一个系统盘上的空目录里”。
- **运行时只撤不恢复**：卷回来之后那一段仍然缺席，页面提示重新应用。自动写回会把一次外部故障
  变成一次静默配置变更。
- **撤销现场要能回答“为什么不见了”**：`runtime_revoked` 带卷 id、卷名、当时的卷状态、时间与原因。
- **开机依赖与共享同生同死**：最后一条共享被撤销/禁用/删除时 drop-in 整份撤走，否则一块坏盘会让
  `smbd` 从此起不来。
- **`noauto` 与 `RequiresMountsFor=` 是分工，不是矛盾**：面板写的 fstab 条目带 `noauto`（导入卷
  `automount=false`），`local-fs.target` 不会去挂它；开机把它拉起来的是 drop-in 生成的 `.mount` 依赖。

**真机抓出的产品缺陷（改产品，不改断言）**

1. **结构体有字段、表里没有列**（Q240）：`shares` 表只有 15 列，`SaveShare` 带着 `RuntimeRevoked`
   但没有任何迁移为它加列，于是 `runtime_revoked` **从来没落过盘**（§5 的 7 条断言失败）。更隐蔽的是
   幂等判据失效——“记录上已经记过这次撤销”永远为假，每 30 秒重写一次、`updated_at` 被推到当下。
   修法：迁移 0006 + 列常量 + `SaveShare` + `scanShare`，补两条单测；另做迁移**升级路径**探针
   （把 16 列的库 DROP 回 15 列、删掉最高版本那条迁移记录，再起代理——列补回来、旧行取到空串、
   其余迁移没有重跑）。
2. **权限被 umask 掩掉**（Q243）：`os.OpenFile(..., 0644)` 的 `perm` 先被 umask 掩一遍才生效，
   生产的代理单元带 `UMask=0077`，于是托管配置与 drop-in 在真机上一直是 `0600`，而契约写着 `0644`。
   修法：写完显式 `Chmod`；补 `TestWriteFileSyncEnforcesMode`；验收侧先断言**代理进程自己的 umask
   是 0077**，再断言两个文件是 `0644`——三条合起来才说明问题，否则在 umask 022 的进程里跑那条断言
   通过也证明不了什么。

**三遍运行**：#1 **PASS=184 / FAIL=14**（Q240 七条 + Q241 六条 + Q243 一条）；#2 **PASS=203 / FAIL=2**
（Q243 一条 + Q241 第三处一条）；#3 **PASS=207 / FAIL=0 / NOT RUN=5**。三份日志原样留在
`docs/evidence/P08/2026-10-22-p08.3/runs/`。

**重启顺序**：`p08-3-reboot-order.sh pre|post|clean` 中间夹一次真 `systemctl reboot`，
三段 **18 / 11 / 5，各 0 失败**。`post` 的核心断言是顺序而不是结果：两个单元各自
`ActiveEnterTimestampMonotonic` 由 systemd 给出，挂载单元比 `smbd` 早 **57.2 秒** 进入 active；
另有 `smb.conf` **重启前后逐字节相同**这一条，钉死“重启不是一次隐式配置变更”。
这一遍是第二遍跑——第一遍三阶段也全绿，重跑时因为固定的幂等键撞了 `IDEMPOTENCY_CONFLICT`（Q244），
而脚本又把提交失败当成“作业超时”报了 60 秒假轮询；修掉之后才是上面这组数字。

**逐节断言数（共 207）**：环境与安全守卫 42、配置与启动 18、夹具（空盘→GPT→分区→ext4→导入→挂载）15、
基线（共享真的能用）10、开机依赖 12、运行中脱盘 23、幂等（走启动首轮）8、卷没挂时再应用 3、
卷回来不自动恢复 17、依赖阻断的三个阈值 26、重新启用 + 依赖清零 22、收尾不变量 11。

**未验证（NOT RUN，逐条写原因）**：真的重启一次看开机窗口（本脚本中途不能被 `reboot` 打断，
由 `p08-3-reboot-order.sh` 补上，见上）；`SHARE_VOLUME_STATE_UNKNOWN`（真机要造出“卷挂着但读不出来”
得先弄坏采集器，单测覆盖）；`VOLUME_DEPENDENCIES_UNKNOWN`（代理总是接了这个查询，单测
`internal/storage/deps_test.go` 覆盖）；`P00-VM-D` 跨机器 SMB 客户端（客户端 VM 未建立，改用同机
真实 `smbclient`）；浏览器里的真实渲染（验的是页面读到的载荷，没有 DOM 断言）。

证据：`docs/evidence/P08/2026-10-22-p08.3/`（`run.log` 与 `runs/run1.log`/`runs/run2.log`、
`runs/reboot-pre-before-q244-fix.txt`、`payloads/`、`logs/`（含 `reboot-pre|post|clean.txt`、
`reboot-journal.txt`、`reboot-systemd-{before,after}.txt`）、`env.txt`、`check.txt`、`native-tests.txt`、
`probe-migration-upgrade.{sh,txt}`、`result.md`）。主脚本 `sha256` `a4ade937…`、重启脚本 `e8dec05d…`，
与仓库中那两份一致（`env.txt` 末尾的 `sha256sum` 是 VM 上同一份的输出）。

### P08.4（2026-10-23）— 共享 UI、用户权限与客户端连接说明

**做了什么**（本阶段**不加接口、不改字段**；`api/openapi.yaml` 只把版本推到 `0.0.0-p08.4`）

| 文件 | 内容 |
| --- | --- |
| `web/src/views/SharesView.vue` | 共享页：列表 + 能力横幅 + 应用/停用/删除 + 账号面板 + 连接状态；每轮轮询同时取 `/storage/volumes`，按 `definition.volume_id` 把卷 join 成 `ShareVolumeInfo` 传给卡片 |
| `web/src/api/shares.ts` | `ShareRecord` 收敛到契约那十个字段；新增 `ShareVolumeInfo`；`summarizeShareState(share, volume?)` / `clientHintsUsable(share, volume?)`；`clientHints(share, host)` 四种客户端的说明（命令里没有密码） |
| `web/src/components/ShareCard.vue` | 一条共享：三状态分开、卷名与观测值、撤销块、连接数、客户端说明折叠面板；取不到卷时**不显示**观测那一行 |
| `web/src/components/ShareAccountPanel.vue`、`ShareForm.vue`、`ShareClientHelp.vue` | 受管理账号（设密码走秘密通道，拿到 `sec_` 后立刻清空输入框）、共享定义表单、客户端说明折叠块 |
| `web/src/components/shares.ts` | 表单值与 `ShareDefinition` 的互转、校验（与 `internal/shares` 的定义校验同口径） |
| `web/src/api/shares.test.ts`、`components/shares.test.ts`、`components/realdata-p08-4.test.ts` | 50 条手写夹具用例 + **27 条真机载荷回放**用例 |
| `web/src/fixtures/p08-4-vm/`（13 个文件 + README） | 真机载荷原样留档，README 写明来源与“别手改”的理由 |
| `internal/shares/write.go` | `executeShareApply` 成功后清空 `RuntimeRevoked`（Q247） |
| `docs/SHARES_CLIENT.md`（新） | 面向使用者的手册：三状态、谁能连、四种客户端怎么连、连不上先看哪里、密码经过哪些地方 |
| `docs/CONTRACTS.md` §7.3、§7.4 | 「重新应用即作废」；§7.4 新增 P08.4 呈现条款（含“卷状态读不出来 ≠ 没挂”） |
| `tests/integration/p08-4-share-ui-client-acceptance.sh`（1145 行） | 11 节 190 条断言的真机验收 |
| `web/src/modules/registry.test.ts` | 导航清单加上 `shares`（模块真的上线了才加） |

**契约落地要点**

- **页面只能显示服务端真的发的东西**：`ShareRecord` 就是 openapi 那十个字段。卷的状态属于卷
  （`/storage/volumes` 的 `observed_state`），开机依赖属于那次操作（`result.data`）。
- **“卷没挂”与“卷状态读不出来”是两句话**：渲染层对两者一视同仁（都不写配置段），但给人看的话
  必须分开——一句话决定他是去看盘，还是去看观测链路。
- **客户端说明的硬规矩**：地址取自 `window.location.hostname`（代理没有地址表，不猜）；命令里永远
  没有密码，`mount.cifs` 用 `credentials=<0600 文件>`，凭据文件用 here-doc 从标准输入写
  （禁 `sh -c 'printf "password=…" > f'`，Q245）；说明**不是**“已可用”的证明，`config_applied`
  为假或已撤销时第一条提示必须写明；`allowed_cidrs` 非空时要说“Samba 对 127.0.0.1 永远放行”。
- **出路必须真的通**：撤销块旁边点“重新应用”之后，撤销现场必须消失（Q247）。

**逐节断言数（共 190）**：环境与安全守卫 40、配置与启动 10、夹具 30、三状态 15、载荷契约 12、
逐条执行说明里的命令 26、权限矩阵 10、密码卫生 8、夹具抓取 9、卷掉线与恢复 19、收尾不变量 11。

**真机抓出的产品缺陷（改产品，不改断言）**

1. **前端 DTO 里写着后端从不发送的字段**（Q246）：`volume_state`、`volume_name`、`startup_deps`、
   `startup_paths` 在真机载荷里全是 `None`/不存在。后果不是“多显示一个空”——`summarizeShareState`
   的“卷没挂”分支判 `volume_state !== 'mounted'`，字段恒为空串，**卷真掉线时页面仍显示“已写入配置”**。
   能活下来是因为单测夹具是自己写的、带着这些字段，而断言全是渲染断言——**没有一条问过“服务端真的发吗”**。
   修法：删掉四个凭空字段、卷由页面 join、开机依赖那行从卡片上删掉（属任务结果），验收脚本反过来钉死
   “记录的键必须是契约的子集”。
2. **“重新应用”走不通**（Q247）：`executeShareApply` 更新分支继承 `existing` 的 `RuntimeRevoked`，
   全仓库没有一处把它写回 `nil`，于是重新应用之后配置回真、客户端能读写，页面却永远显示“已撤销”、
   `clientHintsUsable` 恒假。修法：成功应用后清空；撤销只由 30 秒对账重新产生；配
   `TestExecuteShareApplyClearsRuntimeRevocation`；契约 §7.3 与 §7.4(2) 各补一条。

**验收脚本自身的缺陷（改脚本，不改产品）**

1. 起点不干净就断言“干净”（上一遍留下的 include 行）→ 先留档再摘掉，收尾原样放回。
2. 只读写探针用 `printf > f`，失败时错误由 **shell** 打、绕开命令自己的 `2>`，拿到空 stderr →
   改 `touch … 2>f`。
3. 第 9 节按“面板自己卸卷”设计，而面板正确地拦住了（`VOLUME_IN_USE_BY_SHARE`），整段 13 条连锁红 →
   改为“断言拒绝 → 从外面 `umount` → 轮询 ≤75 秒等对账 → 从外面挂回 → 仍是撤销 → 重新应用恢复”。
4. Q246 修完之后第 9 节仍去读共享记录的 `volume_state`（拿到 `None`）→ 改读卷列表的 `observed_state`，
   并额外断言“共享记录里依旧没有这个键”。
5. `bin/linux-amd64` 里的二进制早于 `internal/webui/dist` 同步，二进制里没有共享页 → 顺序固定为
   前端构建 → 同步 dist → `GOOS=linux go build`。

**四遍运行**：#1 **PASS=167 / FAIL=20**（Q246 四条 + 构建顺序一条 + 三处脚本缺陷十五条）；#2
**PASS=187 / FAIL=2**（Q247 一条 + 一处残留的 `volume_state` 读法）；#3 **PASS=190 / FAIL=0 / NOT RUN=4**。
三份日志原样留在 `docs/evidence/P08/2026-10-23-p08.4/runs/`。宿主 `check.sh` PASS 13/0/0，
`go test ./...` 全部包 ok，前端 637 条用例全绿（含 27 条真机载荷回放）。

**未验证（NOT RUN，逐条写原因）**：Windows 客户端（资源管理器 / `net use`）、macOS 客户端（Finder）
——没有这两种测试机、`P00-VM-D` 仍未建立，说明里给它们的地址形状由 web 侧断言；从另一台物理机器
连进来（本机客户端与 `smbd` 同机，跨机器还牵涉防火墙）；浏览器里的真实渲染（脚本验的是页面读到的
载荷，DOM 由 vitest 的 SSR 回放覆盖）。

**已知限制**：`shares-offline.json` 与 `shares-restored.json` 逐字节相同（差别在卷那一侧），回放用例
因此显式传入“挂载中的卷”；`web-smoke` 只覆盖生产/mock 两种模式的冒烟，不覆盖浏览器交互。

证据：`docs/evidence/P08/2026-10-23-p08.4/`（`run.log`、`runs/run1.log`、`runs/run2.log`、`payloads/`（58 份）、
`logs/`、`env.txt`、`check.txt`、`native-tests.txt`、`result.md`）。主脚本 `sha256` `9b5b6a08…`
（VM 上与仓库里同一份，见 `env.txt`）。

### P09.1（2026-10-22）— 应用/分组/收藏元数据、判别式绑定、公共入口与内部目标分离

**范围**（STAGES §11 第 1 条）：登记“这台机器上有什么应用”的**元数据**，以及一个应用与
systemd 单元 / 容器 / compose 项目之间的绑定关系。本子任务**不碰**运行态：不看容器、不启停服务、
不做健康探测、不读日志——那些是 P09.2–P09.4。前端是 P09.5。

**做了什么**

- `migrations/0007_apps.sql`：`apps`、`app_groups`、`app_favorites`、`app_volume_dependencies`。
- `internal/apps/`：`Definition`/`Patch`/`App`/`Group` 类型、校验与规范化（`validate.go`）、
  SQL（`store.go`，含乐观锁、游标分页、`InTransaction`）、卷存在性确认（`volumes.go`）。
- `internal/api/apps.go`：投递与错误映射；`internal/api/router.go` 里挂上 `/apps`、
  `/apps/{id}`、`/apps/{id}/favorite`、`/app-groups`、`/app-groups/{id}`。
- `api/openapi.yaml`：新增 6 个路径、2 个 id 参数、11 个 schema；`version: 0.0.0-p09.1`。
- `docs/CONTRACTS.md` §8.1：把本子任务的边界逐条写清，并列出**没有实现**的接口，
  免得下一阶段的人以为它们已经在了。
- `docs/DECISIONS.md`：Q248（依赖只存卷标识、不建外键，503 与 422 分开）、
  Q249（生产模式禁回环/通配 `public_url`，mock 模式放宽）、
  Q250（`app_`/`grp_` 沿用前缀 + 32 位十六进制，偏离 §1 的“UUID v4”字面）。

**几个当时必须定下来的取舍**

- **判别式绑定落到数据库**：`binding_json` 先规范化（键序固定、只留该类型的那一个键），
  再把唯一性交给 `CREATE UNIQUE INDEX uq_apps_binding ON apps (type, binding_json) WHERE type <> 'link'`。
  这样“同一个容器被登记两次”不是一次先查后写的竞态，而是一条约束。
- **入口与目标分开管**：`public_url` 是“浏览器点得开”、`target_id` 是“面板自己探”。
  把两者混成一个字段，就会在“面板能连、用户点不开”或者“用户点得开、面板探到别人家”之间二选一。
  `target_id` 用形状约束（`local_…` / `cntr_…`）从语法上排除 IP、主机名、端口与 URL——
  SSRF 在这里不是靠黑名单拦的，是**写不出来**。
- **摘要字段只能填“不知道”**：`runtime_state`/`http_state`/`last_checked_at`/
  `dependency_protection` 在 P09.1 没有任何探测器，于是它们只有 `not_applicable` 与 `unknown`
  两种诚实值；`capabilities` 只声明此刻真的能做到的事（只有 `open`），
  于是 P09.5 不会画出一排按下去没反应的按钮。
- **依赖卷不建外键**（Q248）：卷在代理的库里，Web 库里再存一份就是两份事实。这里只存
  `vol_` 标识（形状 `CHECK` 约束），写入时通过代理的卷列表确认；代理不可达是 503 且**不写库**，
  卷确实不存在是 422。把“确认不了”说成“不存在”会让网络抖动变成数据损坏。

**验收**

- 真机脚本 `tests/integration/p09-1-apps-metadata-acceptance.sh`，第八遍 **PASS=210 / FAIL=0 / NOT RUN=0**，
  第七遍（紧接着复跑）同样 210/0/0——复跑这一遍是**故意**在脏盘上跑的，用来证明夹具能自己清干净。
- 覆盖：四种类型的绑定形状（含 7 条错位绑定）、唯一性（含 `sqlite_master` 里的索引）、
  `public_url` 6 类非法值、`target_id` 8 个非法形状、健康检查默认值、分组与
  `DEPENDENCY_BLOCKED`、收藏按用户隔离、版本冲突 409、viewer 403、缺 CSRF 403、
  8 个未实现接口确实 404、删除不产生任务、依赖卷三种结局、审计动作与目标、
  审计表只追加、收尾不变量（Web 非 root、代理 root、四张表都在、日志无 panic）。
- 宿主：`scripts/check.sh` PASS 13 / FAIL 0 / NOT RUN 0（含 gofmt、Linux 交叉编译、`go vet`、
  前端四项、openapi lint 7 条 warning），`go test ./...` 全部包 ok。

**真机没抓出产品缺陷，但抓出了验收脚本自己的 5 类问题**

1. 断言了 `error.details.field`，而 400 的响应体里没有 `details`（那句话在 `message` 里）。
2. 拿**已经被占用**的容器 ID 去改绑，得到 422——那是产品的正确拒绝，是脚本选错了夹具值。
3. 破坏性动作（初始化分区表/格式化/导入卷）要求 5 分钟内的再认证，脚本没先 `POST /auth/reauth`。
4. 上一轮 `/etc/fstab` 里 `p09data` 那行让这一轮导入被正确拒绝（挂载点重复），
   而 `wipefs` 在盘被挂着时会失败、脚本把输出丢进 `/dev/null` 就当中没这回事。
   现在：`disk_is_blank`（签名 + 子分区 + 首扇区三条独立证据）+ `prepare_blank_disk`
   （先卸挂载、再清、最多 5 轮、每轮验证），清不干净就整段 NOT RUN 并把 `wipefs` 原话写出来。
5. **预检故意绑定快照**（`Collector.Assess` 用 `Topology` 而不是现场重扫，理由写在代码注释里：
   现场重扫会让每份预检在提交时都对不上）。抹盘之后立刻预检，拿到的是抹盘前的快照，
   于是预检正确地判“盘上已有分区表”。现在脚本用 `wait_agent_sees_blank` /
   `wait_agent_sees_device` 等快照追上真实磁盘；等不到就 NOT RUN，不把快照语义记成缺陷。

**未验证（NOT RUN，逐条写原因）**：`/apps/discovery`、健康探测、日志（P09.2–P09.3，接口还不存在）；
容器/镜像/网络/卷/部署/ compose 栈（P09.3–P09.4）；前端渲染与交互（P09.5）；
列表跨页游标、并发写冲突、分组深翻页、多人同时收藏的竞争（都只在单元测试里覆盖）；
真实浏览器里的 CSRF/SameSite 行为（真机用 curl + cookie jar + `X-CSRF-Token` 验协议行为）。

**已知限制**：`app_volume_dependencies` 里的卷标识不做外键，所以卷在代理侧被删掉之后，
应用记录上的依赖会变成一条“指不到卷”的标识——P09.4 的“依赖阻断”要按这个事实设计，
不能假设依赖总能被解析回一个卷。列表分页用 `(name, id)` 与 `(sort_order, id)` 游标，
改名会改变游标位置（记录在契约里）。

证据：`docs/evidence/P09/2026-10-22-p09.1/`（`run.log`、`payloads/`（14 份）、`logs/`、
`env.txt`、`check.txt`、`native-tests.txt`、`result.md`、`collect-env.sh`）。

### P09.2（2026-10-22）— 本机端口/服务/容器发现、管理员确认候选、健康检查与 SSRF 约束

**范围**（STAGES §11 第 2 条）：把“这台机器上正在对外提供什么”读出来，让人确认之后登记成应用，
并对登记过的入口做**普通 HTTP 健康探测**。这一轮仍然不做启停、不读日志、不接管 compose。

**做了什么**

- `internal/discovery/`：三路来源（`/proc/net/tcp{,6}` 的 LISTEN → inode → 持有进程 → cgroup v2
  → `.service` 单元；裸监听；`docker ps -a --no-trunc --format`），候选与 `target_id` 的生成，
  15 秒快照缓存，探测客户端（`Proxy: nil`、不跟随重定向、TLS 校验开、正文 64 KiB 截断）。
- `internal/apphealth/`：Web 侧调度器（5 秒一拍、一轮 60 秒、每应用 30—300 秒节流、最小间隔 5 秒），
  代理不可用写一条 `unknown` 并结束本轮。
- `migrations/0008_app_health.sql`：`app_health_results` 一行一个应用，只存**结论**；
  定义仍在 `apps.health_json`。
- `internal/api/apps_discovery.go` + `internal/agent/discoveryquery.go`：`GET /apps/discovery`（admin）、
  `GET /apps/:id/health`（V），代理侧的发现/探测查询通道。
- `api/openapi.yaml` 两个新路径与 6 个 schema；`docs/CONTRACTS.md` §8.2；
  `docs/DECISIONS.md` Q251—Q260。

**几个当时必须定下来的取舍**

- **SSRF 不是“过滤”，是“写不出来”**：探测地址永远由代理拼成 `http(s)://127.0.0.1:<port><path>`，
  调用方只能给 `local_…`/`cntr_…` 形状的 `target_id`、端口与 path。`169.254.169.254`、内网主机、
  完整 URL 在这条路径上**没有出现的位置**。
- **“确认候选”不另开接口**：确认就是 `POST /apps`。再开一个 `/apps/discovery/:id/confirm` 等于
  让“创建一个应用”有两条代码路径，其中一条迟早会和另一条不一致。
- **“没看”和“没有”必须分开**：探测侧是 `unknown` vs `error`，绑定合并侧是“来源没采成就不进
  `missing_bindings`”。这两处是同一条道理的两次落地（Q253/Q257/Q260）。
- **候选的身份只看 id**：systemd 用单元名、docker 用完整 64 位容器 ID，绝不按名字模糊匹配——
  否则容器重建之后，一个没人确认过的新容器会被悄悄接到旧应用的入口上（Q255）。
- **同一个服务只出现一次**：pid 1 代持的 `.socket` 监听（Q258）与 docker-proxy 的发布端口监听
  （Q259）都归给真正的对象，不各占一条候选。

**验收**

- 真机脚本 `tests/integration/p09-2-app-discovery-acceptance.sh`，最终一遍
  **PASS=120 / FAIL=0 / NOT RUN=1**（唯一 NOT RUN 是“卷离线禁止启动受管理依赖应用”，属 P09.4）。
- 覆盖：候选来自本机（自建监听出现、无人监听的端口不出现）、`target_id` 形状、
  viewer/anon 权限、确认候选＝`POST /apps`、幽灵目标 422 + `details.target_id`、
  非法目标形状 400、三种 unknown、真实 200/500/超时三种结论、服务停掉后变 `error`、
  代理不可用 503 且不写库、**真容器**：候选与发布端口、running 但 HTTP 500 明确异常、
  重建 ID 变化后旧绑定要求重新确认、docker 停掉不把“没看”写成“不在”、
  收尾不变量（Web 非 root、代理 root、库里没有非回环地址、日志无 panic、无云元数据请求）。
- 这一轮为容器条款在测试机上装了 `docker.io` 29.1.3 + `alpine:3.20` 夹具；没有 docker 的机器上
  那三条仍是 NOT RUN（脚本里按 `command -v docker` 分支，不把“没跑”说成“通过”）。
- 宿主：`scripts/check.sh` PASS 13 / FAIL 0 / NOT RUN 0，`go test ./...` 全部包 ok。

**真机抓出并修掉 3 个产品缺陷**

1. **Q258** 候选里 22 端口出现两次：`ssh.socket` 的套接字由 pid 1 持有，sshd 自己也持有同一个
   端口。现在同一个（地址, 端口, 协议）只归有人认领的进程，pid 1 **独占**的监听仍然是候选
   （“套接字开着、服务还没被激活”是要看见的状态）。
2. **Q259** 有 docker 时同一个容器出现两次：docker-proxy 跑在 `docker.service` 的 cgroup 里，
   原来的抑制逻辑只作用在“裸监听”分支，先按单元分类就变成了“docker.service 在监听 19487”。
   现在“摘掉发布端口上的监听”挪到单元分类之前，`dockerd` 自己的 2375 仍然保留。
3. **Q260** 停掉 dockerd 之后，那个容器应用立刻出现在 `missing_bindings`（“可能被重建过，
   需要重新确认”）。容器并没有被重建，只是这一轮没人去看。现在 `missing_bindings` 以
   “对应来源这一轮 `status=ok`”为前提；systemd 侧同理（代理刚启动、服务快照还没有时，
   所有 systemd 绑定都会被说成“当前没有在监听任何端口”）。

另有一条语义修正在验收第 7 节被抓出来：**目标从发现结果里消失时记 `error` 而不是 `unknown`**
（Q257）。否则同一个物理事实——站点停了——会因为 15 秒缓存的时点不同，一会儿显示“异常”、
一会儿显示“暂不确定”，而后者会让一个挂掉的站点永远看起来“等一下就好”。

**未验证（NOT RUN，逐条写原因）**：卷离线禁止启动受管理依赖应用（P09.4，本轮 `capabilities.start`
为 `false`）；`/apps/:id/logs`、`/containers*`、`/images`、`/container-networks`、
`/container-volumes`、`/deployments*`（P09.3—P09.4）；前端渲染与交互（P09.5）；
并发确认同一候选、多人同时操作（单测覆盖）；podman 等其他运行时（不在 P09 范围）；
小时级调度 soak（只验了到点会探、间隔内不探、代理掉线写 unknown）。

**已知限制**：代理刚启动的第一个采集周期内服务快照还没有，`systemd` 来源会如实报“服务状态
尚未完成第一次采集”，那一小段时间里单元候选为零（快照 `partial`）——这是 P05.1 定下的语义
（还没采过 ≠ 本机没有服务），Q260 保证它不会被误读成“绑定失效”。容器身份用一个 64 位 ID，
`docker ps` 的输出格式是解析约定（`|` 分隔、端口列最后），docker CLI 改格式时会在
`ErrDockerOutput` 上明确报错，不会静默给出半份清单。

证据：`docs/evidence/P09/2026-10-22-p09.2/`（`run.log`、`payloads/`（13 份）、`logs/`、
`env.txt`、`check.txt`、`native-tests.txt`、`result.md`、`collect-env.sh`）。

### P09.3（2026-10-22）— Docker 只读查询、容器日志与现有受支持容器的启停

**范围**（STAGES §11 第 3 条）：容器/镜像/网络/数据卷/部署的只读查询、容器日志（分页与实时流）、
以及**现有受支持容器**的 start/stop/restart。危险容器只显示不接管；外部 Compose YAML 不执行、
不解析、不落盘，只按 `com.docker.compose.project` 标签归并出一个只读视图。这一轮不做应用级的
启停联动（P09.4），也不做前端（P09.5）。

**做了什么**

- `internal/containers/`：`types.go`（视图 DTO 与风险闭集）、`parse.go`（inspect 格式串与解析）、
  `docker.go`（固定的 docker 子命令、不可用分类）、`logs.go`（时间键集分页与实时流）、
  `control.go`（预检判决、执行前复核）、`http.go`（判决 → HTTP 的唯一映射）、`fetch.go`
  （Web 侧读取端，进程内不碰 `docker.sock`）。
- `internal/agent/containerquery.go`（7 个查询：清单/详情/镜像/网络/卷/部署/部署详情）、
  `containeraction.go`（`container.control`）、`streams.go`（流许可与每账号 4 条并发额度）、
  `registry.go`（能力由注册表推导，不手写）。
- `internal/api/containers.go`：7 个 GET + 1 个 SSE（`/containers/:id/logs/stream`），**整组 admin**
  ——容器清单等于“这台机器跑了什么”，属于侦察材料。
- `api/openapi.yaml` 八个路径与对应 schema；`docs/CONTRACTS.md` §8.3；`docs/DECISIONS.md`
  Q261—Q272。

**几个当时必须定下来的取舍**

- **容器身份只认完整 64 位小写十六进制**：全 ID 猜不出来，短 ID 可以。ID 是这个模块里**唯一**
  会进入 argv 的可写字符串，Web 与代理各校验一次；名字只在输出里出现。
- **风险是一个闭集，不是启发式**：privileged、host 网络/pid/ipc、危险 capability、挂
  `docker.sock`、挂宿主 `/`。命中即 `risk.level=high` + `managed=false` + 三项能力全 false，
  并在**两处**拒绝（预检 + 执行前复核）——只在预检拒绝，等于相信“预检之后世界不变”。
- **不该要的东西不请求**：inspect 的格式串里没有 `Config.Env`、没有完整标签表（只取六个
  `com.docker.compose.*`）、不取 raw inspect。环境变量里通常就是数据库口令与令牌，
  “先取回来再过滤”是迟早会漏的做法。真机上放了唯一口令验证：清单、详情、日志、SSE 的响应体
  和代理日志里都搜不到它。
- **两种“不可用”必须分开**：代理不可用（503 `AGENT_UNAVAILABLE`）与这台机器上的 Docker 不可用
  （列表 200 + `available=false` + `notice`；详情与日志 503 `DOCKER_UNAVAILABLE`；预检
  `allowed=false` + 阻塞码）——合成一句“容器不可用”会让人去修错的东西（Q271/Q272）
- **日志往回翻不能靠 `--tail`**：docker 先取整个流的最后 N 行、再按 `--until` 过滤，于是
  比游标早的行在过滤前就被丢光（Q270）。往回翻用时间窗，窗口能加宽也能缩窄。
- **compose 项目是派生身份**：`id = "dep_"+sha256("compose:"+项目名)[:32]`，不带项目名原文；
  只做精确匹配的 GET，按名字模糊查是 404。

**验收**

- 真机脚本 `tests/integration/p09-3-container-acceptance.sh`，最终一遍（第五遍）
  **PASS=217 / FAIL=0 / NOT RUN=1**。13 节：环境守卫 10、启动身份 9、夹具 5、容器清单 44、
  身份 14、日志 14、镜像/网络/卷 13、部署 12、启停 24、拒绝 37、SSE 5、两种不可用 19、权限审计 11。
- 唯一 NOT RUN 是「执行前复核（受理之后再变）」：受理时的复核已经把变化挡在 202 之前，
  要在受理与执行之间改状态需要可重复的竞态，真机构造不出来；由
  `internal/agent/containers_test.go` 的 `TestContainerControlExecuteStaleState` 覆盖。
- 启停三条（stop/start/restart）每条都“预检 → 任务 → 读回”，并与 `docker inspect` 现场对照：
  **退出码 0 不算成功，读回状态一致才算**。四类危险容器各自起真容器，预检被拒后现场确认它
  **真的没被动过**。
- 宿主：`scripts/check.sh` PASS 13 / FAIL 0 / NOT RUN 0；`go test ./...` 全部包 ok；
  容器相关单测 57 个用例。

**真机抓出并修掉 3 个产品缺陷**

1. **Q270 日志往回翻页永远只能拿到边界一条**：`docker logs --tail N --until <游标>` 里
   `--tail` 先作用于**整个流**、`--until` 之后才过滤，所以比游标早的行在过滤前就被丢光了，
   第二页只剩游标那条本身（再被去重掉就成了空页）。这不是参数写错，是组合语义的缺陷。
   改成时间窗 `--since … --until 游标`：初始 15 分钟、没凑满放大 8 倍、铺到**容器创建时间**
   （`docker inspect --format {{.Created}}`）为止或 30 天上限；撞 8 MiB 上限就缩窗 4 倍（丢的是
   最接近游标那段，缩窗才对）。`truncated` 的语义同时钉死：铺到创建时间就是 `false`，
   不让“可能还有”冒充“确实没有了”。
2. **Q271 套接字还在、守护进程死了**：产品只按套接字文件判断可用性，于是 `systemctl stop docker`
   / OOM / 升级重启这三种最常见的状况下，列表回 503（契约要 200 + `available=false`），预检直接 500。
   判据改成 **docker 自己那句** `Cannot connect to the Docker daemon`；清单类接口降级为
   200 + `available=false` + `notice`，预检写成 `allowed=false` + `DOCKER_UNAVAILABLE`
   （那是**拒绝理由**，不是“预检算错了”）。
3. **Q272 代理说“Docker 不可用”，Web 改写成“代理不可用”**：Web 侧 `mapError` 只按状态码分类，
   把代理的任何 5xx 都当成“代理自己不可用”，于是详情/日志的 503 带着 `AGENT_UNAVAILABLE` 出去。
   带域内码的 5xx 现在原样透出，只有通用码（空 / `AGENT_UNAVAILABLE` / `INTERNAL_ERROR`）与连不上
   才翻成 `AGENT_UNAVAILABLE`。这一条最值得记：它不会让任何测试变红，只会让页面把用户指错方向。

**验收脚本自身的问题（6 条，与产品代码无关）**：RFC3339Nano 按字符串比大小（`.5Z` > `.55Z`
数值上却更小）、`${_bad:0:14}` 是 bash 专有写法而脚本跑在 dash 上、一条断言的极性写反、
“陈旧预检”误以为是执行期失败（契约是**受理时**复核 → 409 + `PREFLIGHT_STALE` + 一条 `failed`
作业）、SSE 用例给的 5 秒不够开流的三步开销、§11.2 等的是 socket 文件消失而不是 `docker info` 失败。
逐条见 `result.md` §5。

**未验证**：前端页面（P09.5）；除 apt 的 `docker.io` 29.1.3 之外的 Docker 版本；“docker 返回 0
但状态没变”（需要一个人为骗人的 docker，正常 daemon 上做不到，由 `internal/containers` 的 fake
覆盖）；危险容器的执行前复核拒绝（预检就拒了，单测覆盖）。**受阻**：无。

**已知限制**：往回翻页是按时间窗猜的，安静的容器可能要多问 docker 几次（上限 8 次），这是
docker 没有游标的直接后果；`docker ps` 的输出格式是解析约定（`|` 分隔、端口列在最后），
CLI 改格式时会在 `ErrDockerOutput` 上明确报错，不会静默给出半份清单。

证据：`docs/evidence/P09/2026-10-22-p09.3/`（`run.log`、`payloads/`（48 份）、`logs/`
（agent/web + 9 份 docker 夹具输出）、`env.txt`、`check.txt`、`native-tests.txt`、`result.md`、
`collect-env.sh`）。

### P09.4（2026-10-22）— 应用关联 systemd/容器的状态、日志、启停与依赖卷保护

**范围**（STAGES §11 第 4 条）：把 P09.1 的应用元数据、P09.2 的发现与健康、P09.3 的容器启停、
P08 的受管理卷接起来——应用摘要里的状态、按钮、日志、依赖保护全部**来自底层对象的观测**，
并在启动路径上加一道**只能收紧**的依赖卷门禁。不做前端（P09.5），不做 compose 部署生命周期
（P10 起），不做运行时卷失联自动停止。

**做了什么**

- `internal/appruntime/appruntime.go`（355 行，新）：一次请求内合成「状态 / 能力 / 依赖保护」。
  状态映射是**逐来源枚举**的，不是猜：systemd `active`+`reloading` → `running`、`failed` →
  `failed`、`activating`/`deactivating` → `unknown`、单元不在列表 → `unknown`；docker `running` →
  `running`、`stopped` → `stopped`、其余 → `unknown`。每次给出非 `running` 的结论时都写一句
  `runtime_note` 说清是哪一种“不是 running”。
- `internal/appruntime/gate.go`（183 行，新）：启动门禁。只对 `service.control`、`container.control`
  的 `start`/`restart` 生效；`stop` 永不拦；没登记成应用的容器不拦；卷 `mounted` 放行、
  `mismatch` → `VOLUME_IDENTITY_CHANGED`、其余非 mounted → `VOLUME_OFFLINE`/`VOLUME_MISSING`、
  读不到卷清单 → `VOLUME_CHECK_UNAVAILABLE`（fail closed）。
- `internal/api/applogs.go`（156 行，新）：`GET /api/v1/apps/{id}/logs`，按绑定判别 journal / docker。
- `internal/apps/target.go`（41 行，新）：绑定 → 「该看谁」的一处判别，三处共用。
- 改：`internal/apps/apps.go`（删掉“按类型推能力”）、`internal/api/apps.go`（DTO 增两个可空
  note 字段，四项改成现算）、`internal/operations/service.go`（预检接门禁，`Gate` 为 nil 时行为不变）、
  `internal/api/router.go`、`internal/api/server.go`、`cmd/upanel-web/main.go`。
- `api/openapi.yaml`：`/api/v1/apps/{id}/logs` 与 `AppJournalLogsData`/`AppContainerLogsData`，
  三处既有字段的说明重写（`runtime_state`、`dependency_protection`、`AppCapabilities`）。
- `docs/CONTRACTS.md` §8.4（8 段）；`docs/DECISIONS.md` Q273—Q276。

**几个当时必须定下来的取舍**

- **`unknown` 不是 `stopped`**（Q276 的邻居）：绑在一个不存在的单元、或绑在被重建过的容器 ID 上，
  最省事的写法是回 `stopped`（“没在跑嘛”），但那会给用户一个点下去必然失败的启动按钮，
  而且把“我看不见”伪装成“它停了”。真机上断言的就是 `unknown` + note 要求重新确认绑定。
- **能力来自对象，不来自类型**：`AppCapabilities` 不再由 `type` 推导。受保护单元（`ssh.service`）
  的三项生命周期能力全 false，但 `logs` 是 true——读日志不是写操作，把“受保护”翻成
  “什么都不能做”是错的。
- **门禁放在 Web 的预检之后，不放进代理的动作表**（Q273）：依赖关系只在 Web 库里，代理不该知道
  应用元数据（否则同一份事实存两处，早晚不一致）。门禁是 `CreatePreflight` 里的一个接口，
  拿到已有判决后**只能把 `allowed` 从 true 改成 false**。它也因此只影响“经面板发起的启动”。
- **`managed` 的含义被限死**（Q274）：只给「docker + `restart_policy ∈ {"", "no"}` + 有依赖卷」。
  它说的是“经面板发起的启动会被依赖卷检查”，**不是**“依赖被接管了”。绕过面板的四条路
  （手工 `docker start`、开机自启、`systemctl enable` 过的单元、`restart: always`）都在 note 里
  点名，这几种一律 `unmanaged`。
- **docker 的 `failed` 这一版不区分**（Q275）：DTO 里没有 ExitCode，造一个 `failed` 只能靠
  “不是 running 就 failed”，会把正常停机的容器说成故障。非 running 一律 `stopped`，
  退出码留在 `state_text` 里。
- **运行时卷失联自动停止推迟，不拿门禁冒充**（Q276）：那需要代理侧常驻协调者。
  CONTRACTS §8.4(7) 写的是当前真实行为——门禁拦的是“下一次经面板的启动”，已经跑着的不会被停。

**验收**

- 真机脚本 `tests/integration/p09-4-app-runtime-acceptance.sh`（1017 行），最终一遍
  **PASS=165 / FAIL=0 / NOT RUN=0**。11 节：环境守卫 7、启动身份 9、夹具 18、摘要 32、
  running+500 8、依赖保护 12、启动门禁 24、重建 8、日志 35、代理停掉 7、收尾 5。
- 门禁那 24 条里有三条是对照组，缺一条就会把“拦住”说成“有依赖就一律拦”：`stop` 不被拦
  （同一台机器、同一个未挂载的卷）、没登记成应用的容器不被拦、**把卷挂起来之后同一份 start
  预检 `allowed=true`**（挂载 → 预检 → 卸载都走产品自己的卷接口）。被拦住的那次提交之后，
  脚本用 `docker inspect` 现场对照：容器**真的没被启动**。
- 夹具都是脚本自己造的：6 个容器、1 个自有 systemd 单元（`upn-p094-svc.service`，不碰受保护单元；
  受保护规则另开一组断言用 `ssh.service` 验）、1 个走产品 `storage.volume.import` 登记出来的
  受管理卷（写 fstab 受管理块、不挂载、不动盘上数据）。收尾用 `storage.volume.forget` 退出管理，
  并双向读回（fstab 里没有那个 UUID、卷列表里没有那个 `vol_`）。
- 宿主：`scripts/check.sh` PASS 13 / FAIL 0 / NOT RUN 0；`go test -count=1 ./...` 全绿；
  新增 `internal/appruntime/appruntime_test.go`（481 行）与 `internal/api/applogs_test.go`（371 行）。

**这一轮真机没抓出产品缺陷**。10 处失败全是脚本自己的毛病：

1. `create_app` 在 `$(...)` 里跑，子壳里设的变量出不来 → 应用标识一直是空（标识写进文件，用 `app_id` 读）。
2. 拿 `ssh.service` 当“可起停的单元”夹具 → 它是受保护单元，能力按契约就是空的（自建单元）。
3. `/storage/volumes` 读成 `data.items` → 实际是 `data.volumes`。
4. 预检标识前缀是 `pf_` 不是 `pre_`。
5. 机器上没有受管理卷 → 卷两段整段 NOT RUN（改成走产品登记路径造夹具，顺带把正对照做成真的）。
6. 服务列表是**采集成快照之后缓存的**，`enable --now` 完 1 秒内建应用读的是旧快照 → 加 `wait_unit`
   轮询 `/api/v1/services` 等单元出现（**产品没错，是脚本拿旧快照断言新事实**）。
7. `lsblk -lnpr`：util-linux 2.39 里 `-r`（raw）与 `-l`（list）**互斥**，命令直接退出 1、拿到空列表
   → 去掉 `-r`。这条坑写进了 `env.txt`。
8. PATCH 应用的 `version` 是**乐观锁的当前版本**，脚本给的是“改完之后的版本号” → 先读再提交。
9. `pfget` 是把参数拼在 `d["data"]` 后面的，跑表达式 `any(...)` 会拼成语法错误 → 新增 `pfexpr`。
10. 「卷挂着时 protection 仍是 managed」失败是第 8 条的连带（PATCH 没成功，应用根本没带上依赖卷）。

逐条见 `result.md` §5。

**未验证**：前端页面与“入口在远程浏览器打开服务器地址”的页面级证据（P09.5）；systemd/docker
控制的**执行**本身（P05.2 / P09.3 已验，本轮验的是“门禁有没有拦住”与“拦住之后东西有没有动”）；
运行时卷失联自动停止（未实现，Q276）。**受阻**：无。

**已知限制**：`managed` 不等于依赖被接管（四条绕过路径写在 note 里）；docker 非 running 一律
`stopped`（`failed` 这一版不区分）；门禁只拦下一次经面板的启动，运行期卷掉线不会自动停机。

证据：`docs/evidence/P09/2026-10-22-p09.4/`（`run.log`、`payloads/`（29 份）、`logs/`
（agent/web）、`env.txt`（**验收跑完之后**采集，fstab 与卷列表都已还原）、`check.txt`、
`native-tests.txt`、`result.md`、`collect-env.sh`）。

### P09.5（2026-10-22）— 应用卡片、搜索、分组、一键打开、状态区分与常用入口首页集成

**范围**（STAGES §11 第 5 条）：把 P09.1—P09.4 的接口做成能用的页面。**只动前端**：
`web/src/api/apps.ts`（DTO + 纯函数）、`AppCard.vue`、`AppForm.vue`、`AppLogPanel.vue`、
`AppDiscoveryPanel.vue`、`AppsView.vue`、`OverviewView.vue`（常用入口）、路由与模块注册表。
没有新增任何后端接口；`api/openapi.yaml` 只改了一处**描述**（Q277）。

**做了什么**

- `web/src/api/apps.ts`（新）：DTO 与 openapi 同源（`App`、`AppGroup`、`AppDiscoveryData`、
  `AppHealthState`、`AppLogsData`、`AppCapabilities`），加一组纯函数把页面规则从组件里拿出来——
  `availableActions` / `primaryAction` / `controlRequest` / `openTarget` / `matchesAppQuery` /
  `bucketApps` / `sortApps` / `frequentEntries` / `mapAppError`。纯函数是刻意的：这些规则要被
  真机载荷直接回放，塞在 `<template>` 里就只能靠“看着像对”。
- `AppCard.vue`（新）：一张卡画四件事，各带自己的标签——运行时（`RUNTIME_STATE_LABELS`）、
  入口（`HTTP_STATE_LABELS`）、依赖保护（`managed`/`unmanaged` + note）、运行时说明。
  按钮按 `capabilities` 画：`open` 渲染成 `<a href target="_blank" rel="noopener noreferrer">`，
  `start`/`stop`/`restart` 渲染成发请求的按钮，`logs` 打开日志面板；名字下面是入口地址原文。
- `AppForm.vue`（新）：建/改应用。`type` 决定出现哪个绑定字段（link → `public_url`；
  systemd → `unit`；docker → `container_id`；compose → `compose_project`），
  切换类型就把另一组的键从提交载荷里去掉——否则 `binding` 里会带着一个不属于该类型的键。
- `AppLogPanel.vue`（新）：按 `binding.kind` 判别 journal / docker，两种 `page` 形状各自渲染，
  分页游标只在前一页拿得到时出现。
- `AppDiscoveryPanel.vue`（新）：本机候选（`target_id` 是闭集 ID）+ `missing_bindings`
  要求重新确认绑定；**不按名字自动接手**（那正是 P09.4 要避免的“看不见的绑定”）。
- `AppsView.vue`（新）：搜索框 + 分组过滤 + 分桶渲染（收藏格在最前、未分组在最后）+ 新建入口。
- `OverviewView.vue`（改）：首页常用入口区，调 `frequentEntries`（只取 `capabilities.open &&
  public_url`，收藏在前、其次按最近更新、最多 8 条）。
- `web/src/utils/jobresult.ts`（改）：`summarizeAppJob` 读 `service.control`/`container.control`
  的 `observed_state`（`previous_state` → `active_state`、`operation`、`verified`），
  不拿“任务退出码 0”当成功。
- `api/openapi.yaml`：`GET /api/v1/apps` 的 `group_id` 描述重写（Q277）。

**几个当时必须定下来的取舍**

- **不引入 `app.control`**：启停必须落成既有的 `service.control` / `container.control`。
  多一条“应用控制”的动作看起来更干净，但它会绕过 `internal/appruntime/gate.go`——那条门禁
  挂在**这两个动作名**上。这条现在写成 `apps.ts` 的注释 + 一条回归断言
  （`controlRequest` 对 link 必须返回 `null`），免得以后有人“顺手加一条捷径”。
- **`open` 是锚点不是请求**：一键打开如果做成 `POST`，服务器地址对浏览者就没有意义了。
  `openTarget` 返回 `{href, reason}`：`href === ''` 时卡片写 `reason`（例如“没登记入口地址”），
  而不是画一个点了没反应的按钮。
- **形状不对的过滤器是 400，不是空列表**（Q277）：代码本来就是对的，错的是 openapi 的描述。
  被默默忽略的过滤器与「读不到不能变成没有」是同一类不诚实，而且更隐蔽——返回的还是 200。
- **能力会晚一拍，页面不替采集器猜**：`runtime_state` 来自 services 采集器的快照（默认 10 秒一轮），
  刚发完动作时摘要还是上一轮的事实。页面照着请求回来的那一份画，不自己“乐观更新”；
  验收脚本用 `wait_app` 轮询，而不是立刻断言（这条写进了夹具 README，免得下一个人以为是 bug）。
- **收藏按用户存**：`PUT /apps/{id}/favorite` 改的是 `app_favorites` 里当前用户那一行，
  所以 viewer 收藏完，admin 的列表里它仍未收藏。页面不给“全局收藏”这种错觉。

**验收**

- 真机脚本 `tests/integration/p09-5-apps-ui-acceptance.sh`（约 790 行），最终一遍
  **PASS=211 / FAIL=0 / NOT RUN=0**。13 节：环境守卫 9、启动身份 8、夹具 9、应用矩阵 36、
  状态区分 16、重建 9、入口与 SSRF 15、分组与收藏 35、启停 17、依赖卷门禁 29、权限 13、
  抓取载荷 9、收尾 6。
- 验收行逐条落成可执行的断言：link 没有启停（§3.1 + SQLite 复核绑定）；running + 500 同时
  写“运行中”与“入口异常”并仍给得出 stop（§4）；重建后旧绑定 `unknown` + note + 四能力全 false
  + 发现结果 `missing_bindings`（§5）；入口是 `10.0.2.15:18897` 且那里真回 200、同端口回环 000
  （§6）；云元数据/主机名/完整 URL 三种 `target_id` 全 400（§6）；卷离线 → `VOLUME_OFFLINE` +
  409 + `docker inspect` 仍 `exited`，stop 不拦、卷挂上后 `allowed=true`（§9）。
- 三组对照，缺一组就会把结论说过头：**卷挂着时同一份 start 预检 `allowed=true`**（门禁读的是
  这一次实测）、**stop 不被拦**（同一台机器、同一个未挂载的卷）、**viewer 收藏之后 admin 的列表
  里仍未收藏**（收藏是按用户的）。
- 夹具自己造：7 个容器（含 `--restart always` 与“会被重建”的那一个）、1 个自有 systemd 单元
  （每 5 秒写一行日志）、1 个走产品 `storage.volume.import` 登记的受管理卷；收尾用
  `storage.volume.forget` 退出管理，13 条应用与 2 个分组全部删净（全量列表回到 0 条）。
- 前端侧：真机载荷 `web/src/fixtures/p09-5-vm/`（16 份 + README）由
  `realdata-p09-5.test.ts` 回放，**51 条断言**；`npm run test` **40 文件 / 720 用例**全绿，
  `typecheck` / `lint` 干净；宿主 `scripts/check.sh` **PASS 13 / FAIL 0 / NOT RUN 0**。

**这一轮真机没抓出产品缺陷**。第一遍 **PASS=200 / FAIL=4**，4 处逐条查清：

1. **契约文字写错**（唯一的仓库改动）：openapi 说 `group_id` 形状不对“按没有这个分组过滤，
   不报错”，服务端给的是 400 `VALIDATION_ERROR`。判定代码是对的，改描述（Q277）。
2. 「消息点名 `group_id`」：服务端消息是「分组标识必须是 grp_ 加 32 位小写十六进制」，
   中文点名，不含字面量 `group_id` → 断言改匹配「分组标识」。
3. / 4. 「停机之后卡片上应出现 start」「启动之后应出现 stop」：`appget` 只能接字段后缀，
   接整表达式是语法错误（新增 `appexpr`）；改对之后仍红，因为**能力会晚一拍**——
   摘要来自 10 秒一轮的服务快照，刚发完 `service.control` 还是旧事实 → 新增 `wait_app`
   轮询（最长 60 s，读到就返回），并把这件事写进夹具 README 与 result.md。

另有两处是第一次失败后补的（为了让断言更贴验收行，不是为了让红变绿）：多建两个**真能打开**的
入口应用并当场 `curl` 出 200（常用入口那几条才有东西可测）、把图库放进「媒体」分组让那个分组
真有两条成员。

**未验证**：**真实浏览器里点**（本轮页面证据是 `renderToString` + 断言，Q246 的约定；没有
Playwright 截图也没有人工点检）；跨浏览器与移动端布局；并发（两人同时收藏同一条、两个管理员
同时改同一个分组）；大数量下的分页与虚拟滚动（本轮最多 13 条应用）。**受阻**：无。

**已知限制**：能力有一拍延迟（`runtime_state` 来自 10 秒一轮的采集快照，动作完成后页面不会
“乐观更新”）；搜索是页面侧的，条数很多时是全量在内存里筛（分页仍由服务端游标负责，
两件事互不影响）；`app.control` 不存在也不该存在——所有启停都走既有两个动作名。

证据：`docs/evidence/P09/2026-10-22-p09.5/`（`run.log`、`payloads/`（52 份）、`logs/`
（agent/web）、`env.txt`（**验收跑完之后**采集）、`check.txt`、`native-tests.txt`、`result.md`、
`collect-env.sh`），回放夹具 `web/src/fixtures/p09-5-vm/`。

### P10.1（2026-10-22）— Nginx 发现、配置冲突、单域名单 upstream、WebSocket、请求限制、访问策略

**范围**（STAGES §12 第 1 条）：让面板能回答“这台机器的 Nginx 现在长什么样”和“这个域名我能不能用”，
并把“我要这样代理”登记下来。**不生成任何配置**：写文件、`nginx -t`、reload 是 P10.2，证书是 P10.3。

**做了什么**

- `internal/nginx`（新）：配置解析与事实采集。只扫三处（`nginx.conf`、`conf.d/*.conf`、
  `sites-enabled/*`，与默认 `include` 一致，不跟随其它 include），按**解析后的真实路径**去重——
  `sites-enabled/default` 是符号链接，不去重就会把同一份 `server_name` 数两遍、证据指两次。
  每个 `server` 块留下 `line`（块起始）与 `names_line`（`server_name` 所在行）、`listen`、`names`。
  `ServerName.Verdict` 五类写法：精确、`*.x`、`www.*`、`x.*`、占位 `_`（不声明任何域名）、
  正则/其它（`Undecidable`）。`nginx -v` 与 `nginx.service` 状态只读，读不到只降级成 warning，
  **不会**因此说“这台机器上没有 Nginx”。
- `internal/proxies`（新）：`NormalizeDomain`（IDNA `MapForLookup`+`StrictDomainName`，A-label、小写、
  去一个尾点；通配符/协议/路径/查询串/端口/IP 一律报错而不是悄悄剪掉）、`Definition.Normalize`
  （默认值、闭集、模式与附加字段双向一致）、`JudgeDomain`（纯函数，不 I/O）、`Service`
  （事务 + 乐观锁 + 审计）、`store`（SQLite）。
- `internal/proxies/agentfacts.go`（新）：`Snapshot`（代理侧 5 秒快照）/ `Collect`（直读）/
  `Invalidate`（只设“下次必须 `fresh`”，Web 侧不存第二份缓存）。
- `internal/agent/nginxquery.go`（新）：登记查询 `nginx.state`，参数 `{fresh}`；`NginxVersionProbe`
  读 `nginx -v` 的 **stderr**（nginx 就写在那里）。
- `internal/api/proxies.go`（新）：7 条路由，全部 admin；写操作带 CSRF；审计 `proxy.create/update/delete`
  与业务同事务，明细固定 `"applied": false`。
- `migrations/0009_proxies.sql`：`proxies` 表 + `uq_proxies_domain` 唯一索引 +
  `CHECK`（模式与名单一致性、上游目标前缀、数值范围）。
- 接线：`cmd/upanel-agent/main.go` 探测 `nginx` 可执行文件（常见五处）与 `nginx.service`；
  `cmd/upanel-web/main.go` 从**真实监听地址**取面板端口传给校验（不是写死 9443）。
- `api/openapi.yaml`：`0.0.0-p10.1`，4 条 path、12 个 schema；`docs/CONTRACTS.md` §9.1。

**几个当时必须定下来的取舍**

- **事实归代理、判断归 Web**（Q278）：事实要 root，判断要数据库（面板自己有没有同名记录）。
  两边都不替对方猜，代理只会登记 `nginx.state` 一条查询，`internal/api` 不直接读配置。
- **缓存分两档**：页面上用 5 秒快照（同一次请求里前后两个域名依据同一时刻），写操作前的冲突判断
  用直读。理由很具体：缓存窗口里正好可能有人手工加了一条 `server_name`，而那是唯一需要判准的时刻。
- **`unknown` 也阻断**：`nginx reload` 不会因为 `server_name` 撞车而失败，它只会安静地用第一个块。
  所以“有正则 `server_name`”“有文件解析失败”“读不到配置”都必须拒绝写入，而不是当成“没找到冲突”。
  验收里专门验了正对照：删掉那份正则/坏文件之后，同一个域名立刻恢复 `clear`。
- **读不到 ≠ 没有**：代理不可用时写入 503 `AGENT_UNAVAILABLE`，列表仍 200 且
  `nginx.read_error=agent_unavailable`、`config_missing=false`；`config_missing` 只在“主机上确实
  没有 Nginx 配置目录”时为真，那种情况下冲突结论才是 `clear`。
- **面板端口是禁区且来自真实端口**：`PanelPort` 取自运行时 `--listen` 的端口，验收里被拒绝的正是
  这一次跑的 19495。写死 9443 会让这台机器上的规则形同虚设。
- **`enabled` 是意图**（Q280）：没有 `applied_*`、`config_path`、`config_hash` 字段；审计里写
  `"applied": false`；`managed_file_count` 在 P10.1 必然是 0。
- **`secret_ref` 只进不出**：创建响应、列表、详情、审计里都只有账号名。代价是**修改账号必须重传
  `secret_ref`**（只给 `name` 会按“密码引用非法”400），这条写进 CONTRACTS §9.1(5)。
- **定义展平进列，不进 `definition_json`**（Q281）：`definition_json` 装不下“cidr 模式必须有网段、
  其它模式必须没有”这类约束，而展平之后四条 `CHECK` 与域名唯一索引都能由数据库守。代价是
  一个 11 字段的表，换来的是并发提交在库里撞一下就是 409，而不是“先查后写”中间那条缝。
  `applied_config_hash` 这一列**不存在**：P10.1 不写配置，它永远是空，加它等于留一个能误读的位置。
  当时 Q281 留了一句“P10.2 再加这一列”——**P10.2 没有加**，改成了每次读时推导（Q286）。

**验收**

- 真机脚本 `tests/integration/p10-1-proxy-acceptance.sh`（630 行，7 节），最终一遍
  **PASS=100 / FAIL=0 / NOT RUN=0**（退出码 0）。节：环境与启动 9、Nginx 现状 12、冲突判断 21、
  登记校验 22、修改与删除 12、权限与“没有代理” 15、没动 Nginx 4。
- 夹具是真的 nginx 配置：`/etc/nginx/conf.d/p101-taken.conf`（`server_name` 在第 3 行）、
  `p101-wild.conf`（`*.p101wild.example.com`）、临时 `p101-regex.conf`（正则）、临时
  `p101-broken.conf`（未闭合的 `server` 块）。**正则与坏文件必须分段放**：正则会让**所有**域名的
  结论变成 `unknown`，放一起会把 clear/taken 断言全染成 unknown，看起来像崩了。
- 逐条钉住的证据：`line == 3`（取 `names_line` 不是块起始行）；`*.x` 占子域但**不占上级域**；
  库里 `SELECT COUNT(*)` 证明被拒绝的写入没留记录；审计 `redacted_details LIKE '%"applied":false%'`
  计数；`/etc/nginx` 全部文件内容摘要**前后相等** + `nginx -t` 通过 + `conf.d` 无残留；
  停代理后写 503 且库不变、重启后同一请求 201。

**这一轮真机抓出 1 个产品缺陷**（Q279）：`maxNginxVersionTimeout = 5` 无单位，赋给
`syscmd.Limits.Timeout`（`time.Duration`）就是 5 纳秒，`nginx -v` 还没 fork 就超时，版本号永远为空。
宿主的 `go test ./...` 全绿是因为那条探针的单测在 Windows 上 `t.Skip`。修成 `5 * time.Second` 后
`version=1.24.0`，`warnings` 清空。脚本自身也错过两遍：第一遍 Web 起不来（脚本把库目录留给 root，
sqlite 报 `attempt to write a readonly database`）、第二遍 4 处期望写错（审计应为 2 条、`taken` 夹具
在 §4 之前已被删）——都记在证据 `run-attempt-1.log` / `run-attempt-2.log` 里。

**未验证**：配置生成、原子写、`nginx -t` 门禁、reload 与失败恢复（P10.2）；HTTPS 与证书（P10.3）；
`basic_auth` 口令的实际生效（要等 P10.2 生成 htpasswd）；前端页面（~~P10.4~~ **订正（P10.3 记）**：
STAGES 里没有代理/证书的页面条目，P10.4 只写“0.1 安装包、校验和、systemd 单元、初始用户、
HTTPS/SSH 隧道使用说明、基本恢复说明、执行完整核心场景”，所以这不是“欠到 P10.4”，而是 STAGES
没要求的范围外事项；要做须在 §6 登记）；多机/远程 Nginx
（PLAN 限定单机）；22.04.5 上的 Nginx（本轮全部证据在 24.04.5 + nginx 1.24.0）。**受阻**：无。

**已知限制**：`nginx -v` 与 `nginx.service` 读不到时只降级成 warning（配置照样读、冲突照样判，
但页面上版本号是空的）；只解析三处配置，其它 `include` 进来的 `server_name` **看不见**——
如果有人在 `nginx.conf` 里 include 了自定义目录，那里的域名不会被判成 `taken`（这条写进
`internal/nginx/collector.go` 的注释，P10.2 生成配置时也不能依赖它）；
`proxies` 表按 CONTRACTS §14.1 用了表名但没有照抄 `definition_json` / `applied_config_hash`
（Q281），§14.1 那一行已改成实际形态。

证据：`docs/evidence/P10/2026-10-22-p10.1/`（`run.log`、`run-attempt-1.log`、`run-attempt-2.log`、
`payloads/nginx-facts.json`、`logs/`（agent/web/nginx-t）、`env.txt`、`native-tests.txt`、`result.md`）。

### P10.2（2026-10-23）— 配置生成、原子写入、`nginx -t` 门禁、reload 与失败恢复

**已实现**：一条站点记录变成真在服务的 Nginx 站点，五步固定流水线——重新评估（重新采集现状、
重新判域名冲突并忽略自己那份文件，不吃预检的旧结论，Q288）→ 账号文件先落 → 配置写同目录临时文件
并 `fsync` → `rename` 覆盖 + `fsync` 目录 → `nginx -t` 整棵树当门禁；失败回滚、**回滚后再测一次**
`nginx -t` 作为证据、失败路径不 reload（Q285）。reload 只 `nginx -s reload` 不 restart，执行前现场问
`systemctl is-active nginx`，没在跑就 `skipped_service_inactive` 而不是谎称已生效（Q287）。
渲染是纯函数、apr1 盐由域名派生，所以重放幂等：不写盘、不备份、不 reload（Q283）。账号文件落在
`/etc/nginx/upanel-auth/<domain>.htpasswd`，权限按主配置里的 `user` 定成 `root:<worker 组> 0640`，
读不到 worker 用户才退回 0644 并在任务里报 `PROXY_AUTH_FILE_WORLD_READABLE`（Q284）。`applied` 四态
（`applied`/`stale`/`not_applied`/`unknown`）由"此刻渲染的字节 vs 盘上那份文件的字节"算出来，
**库迁移仍是第 9 版**，没有 `applied_config_hash` 列（Q286，推翻 Q281 的承诺）。`proxy.apply` 的参数
由 Web 展开（请求体只有 `site_id`；定义取自库、`panel_port` 取自真实监听地址，执行层再校验一次，
Q282）。所有拼进配置内容的路径走 `path` 不走 `filepath`（Q289）。托管配置还在盘上时删除记录是
409 `DEPENDENCY_BLOCKED`，读不到事实是 503（不是"当作没有"）。

**已验证**（真机，`docs/evidence/P10/2026-10-23-p10.2/`）：VM 上 **PASS=153 / FAIL=0 / NOT RUN=7**。
写下去的文件 0644、首行托管标记、`nginx -t` 通过、worker 换一代（reload 真发生）、`Host:` 头穿过
nginx 拿到上游页面；重放不产生新备份也不 reload；`p102_no_such_directive on;` 夹具走完
"写 → 门禁 → 回滚"（逐字节还原、worker 没换、站点仍在服务、面板照常健康）；手工改文件变 `stale`、
再应用回 `applied` 且被改的那份进了备份；改名移除旧文件、停用移除文件但留备份、`served=false`；
basic auth 的 apr1 被真 nginx 认（401/200/401）；`/etc/nginx` 里非面板文件摘要前后一致。
`gofmt`/`go vet` 干净，`go test ./...` 34 个包全绿。

**未验证**：443 与证书签发/续期（P10.3）；面板 HTTPS 与 SSH 隧道文档、0.1 打包（P10.4）；
WebSocket 握手实测（测试上游不支持 Upgrade）；真实 DNS 与外部访问（VM 用 `Host` 头代替）；
密码哈希参数的跨机器标定（Q292）。**受阻**：无。

**已知限制**：
- 预检偶发慢的根因已修（Q290），但同类风险还在别处：`services.unit` 查询
  （`internal/agent/servicesquery.go`）仍然**现场**问 `systemctl show`，在后台采集扫掠期间同样可能
  等上数秒。P05.1 的详情页本来就要求"现在能不能 stop 必须看现在"，所以不能简单改成读快照；
  这条留给 P05 的后续处理，记在这里而不是假装没有。
- 服务采集在 503 个单元的机器上一轮要 20 秒（按 200 个一段、`showTimeout` 10 秒、`listTimeout` 20 秒），
  快照因此可能旧十几秒。单元状态用于展示与告警够用，reload 决策另有现场探测（Q287）。
- 只解析三处配置（`nginx.conf`、`conf.d/*.conf`、`sites-enabled/*`）：别人 `include` 进来的目录里
  的 `server_name` 看不见，P10.1 的限制在这里照旧成立。
- 密码哈希在 2 vCPU 虚机上单次 1.4–3.9 秒，与 DECISIONS Q23 记的 100–300ms 不符；登录/再认证
  因此 4–6 秒。属 P02 参数标定，不在 P10.2 顺手改（Q292）。

证据：`docs/evidence/P10/2026-10-23-p10.2/`（`run.log`、`payloads/`（渲染结果、`htpasswd-site-c`、
`permissions.txt`、`nginx-T.txt`、预检与任务 JSON）、`logs/`（agent/web/upstream）、
`probe-unit-timing.txt`（Q290 的复现数据）、`argon2-timing.txt`（Q292）、`check.txt`、
`native-tests.txt`、`env.txt`、`result.md`）。

### P10.3（2026-10-23）— 证书签发、续期、手动导入、到期信息与私钥保护

**已实现**：`internal/certificates` 一整套（领域模型、签发/续期/导入的 Decode→Plan→Execute、账本、
certbot 调用、上传暂存、现场读盘、HTTP-01 预检），`internal/api` 五个端点（列表、详情、签发、续期、
上传），`internal/agent` 三个固定动作，`migrations/0010_certificates.sql`。
`staging` 是**必填指针**（缺了 400），`--server` 只能由它推导；正式地址只能由 root 拥有的代理配置
覆盖且必须 `https://`（Q293）。**账本只有一个写入点** `OnJobFinished`，`crt_` 标识在预检展开期定死、
按域名唯一，所以续期不换标识（Q295）。冷却不用新表，读 jobs 表的签发尝试（Q294）。
导入拆两条通道：公钥走 `POST /certificates/uploads`（multipart 字段 `certificate`，1 MiB，10 分钟，
一次性、绑会话，正文里出现私钥直接 400），私钥走 `POST /secrets` 拿 `secret_ref`，落地时先 `Take`
再验 `CoversDomain`（Q296）。路径权限：手动 `/etc/ssl/upanel` 0700、`<domain>.pem` 0644、
`<domain>.key` 0600 root。
**续期与生效分家**（Q297）：续期只换盘上文件；`proxy.apply` 在"绑了证书且配置字节没变"时用 SNI 对
`127.0.0.1:443` 握手取叶证书指纹（`internal/proxies/servedcert.go`），与盘上不一致就 reload 并写
`reload_reason=cert_rotated`，reload 之后再测、给 12 × 250 ms 的 settle 预算，仍不一致记
`PROXY_TLS_SERVED_STALE` 告警；探测失败不改结论但把 `served_cert_probe=failed: …` 记进 observed。
不加强制参数、不落新列、不记"上次 reload 时是哪张证书"——每次算，算不出来就说算不出来。
`PROXY_TLS_*` 十个码在 CONTRACTS §9.3(10) 里分阻断（5 个）与告警（5 个）。

**已验证**（真机，`docs/evidence/P10/2026-10-23-p10.3/`）：VM 上 **PASS=176 / FAIL=0 / NOT RUN=7**，
Pebble 当 ACME 测试服务（HTTP-01 经 nat 从 80 转发到 5002）。签发走通并 verified：`live/` 0700、
`live/<域名>` 里只有 4 个符号链接加 certbot 自带的 `README`、`privkey.pem` root:root、
**uid 999 既读不到私钥也进不去目录**；绑定后 443 带系统信任链取到上游页面。
续期：1 小时内第二次被 429 挡住且不产生任务；续期后指纹变、标识不变、绑定不变、到期刷新；
**续期之后、重新应用之前 443 仍在服务旧证书**（这就是"续期不自动生效"，本轮把它变成已知行为而不是缺口）；
重新应用时 `file_state=unchanged` 但 `reload=done`、`reload_reason=cert_rotated`，
`served_cert_fingerprint=a185e5be…`（旧）与 `served_cert_after_reload=2b8e7891…`（新，等于盘上指纹），
`served_cert_probe_attempts=2`；第三轮应用又回到 `skipped_unchanged`（没有把幂等重放弄坏）。
手动导入：公钥上传 + 私钥秘密通道，站点 B 在 443 上服务的就是导入的那张证书；过期证书绑定被挡；
`expiring` 卡在 21 天窗上。私钥保护逐出口搜索 22 条（三份日志、五个表、预检参数、任务结果、所有响应体），
接口不吐 `key_path`/`cert_path`/`key_secret_ref`。宿主 `gofmt`/`go vet` 干净、`go test ./...` 36 个包 ok，
`check.sh` PASS 12 / FAIL 1（仅 node 版本，宿主 v24.15.0 与 `.nvmrc` 的 22.23.3 不一致，未切 nvm）。

**未验证**：真实 Let's Encrypt 签发与正式地址（速率限额 + 需要公网域名与 80 端口，NOT RUN 第 1、2 条）；
真实公网 DNS 与外部可达性（用 `/etc/hosts` + nat 代替）；浏览器里的信任表现（VM 无浏览器）；
DNS-01/通配符（产品不做）；吊销与 OCSP（不在范围内）；timer 自动续期（P12）；代理/证书前端页面
（STAGES 未列）。**受阻**：无。

**已知限制**：见 §8「P10.3 的六条限制」——最要紧的是第 ① 条：**P12 的 timer 必须做两步**，
续期之后对绑定了这张证书的站点各应用一次，否则新证书不会上线。

证据：`docs/evidence/P10/2026-10-23-p10.3/`（`run.log`（176/0/7 的那一遍，与 `env.txt` 里的二进制
sha256 对应）、`payloads/`（44 份预检/任务/证书 DTO）、`logs/`（agent/web/upstream）、`check.txt`、
`native-tests.txt`、`env.txt`、`result.md`（含四遍运行的收敛过程与四个失败根因））。

### P10.4（2026-10-23）— 0.1 安装包、安装/卸载脚本、访问与恢复说明、完整核心场景

STAGES §12 第 4 项：**0.1 安装包、校验和、systemd 单元、初始用户、HTTPS/SSH 隧道使用说明、
基本恢复说明；执行完整核心场景**。这一节连同 0.1 放行条件「22.04 和 24.04 都有安装/重启/核心运维
证据，README 明确尚未实现的 1.0 功能」一起验。

**交付物**：`scripts/release.sh` 打 0.1 包（amd64 + arm64，`SHA256SUMS` 随包，包内逐文件校验和）；
`deploy/scripts/install.sh` / `uninstall.sh` 是可审阅的脚本而不是一行安装器；`docs/INSTALL.md`
（校验 → 解包 → 读脚本 → 依赖按功能装 → 安装 → 装完自检 → 重入/升级/卸载）、`docs/ACCESS.md`
（HTTPS 与 SSH 隧道两种访问方式、证书指纹核对）、`docs/RECOVERY.md`（忘记口令、面板证书、代理不工作、
库损坏、fstab 之下的 0.1 无路可走清单）。

**验收（四个脚本，两台专用虚机，最终包 `d00bada6…`，由提交 `ac9e4a0` 打出，二进制 sha256 见证据 `env.txt`）**：
下面的数字**全部是在这个已提交、已打包的产物上重跑出来的**（先提交、再打包、再验收，DECISIONS Q317；
早先那份 `333fdf81…` 是"修复还没提交"时打的包，已作废）。两台虚机落盘的
`upanel-web` / `upanelctl` / `upanel-agent` 与包内 `bin/` 逐字节同值，二进制自报
`0.1.0 (commit ac9e4a0, built 2026-10-23T08:06:20Z, go1.27.3)`：
- 安装验收（`p10-4-install-acceptance.sh`）：22.04 **PASS=136 / FAIL=0 / NOT RUN=0**、
  24.04 **PASS=136 / FAIL=0 / NOT RUN=0**。校验和是真在检查（改一个字节必须失败）、卸载先列后删
  且保留 `/srv/upanel`、账号无登录 shell 且不在特权组、目录与文件权限逐项核对、面板只经 HTTPS
  对外（明文请求拿不到业务响应、HSTS 只在 TLS 下、TLS 1.2 能连 1.1 连不上）、非回环缺 TLS 拒绝
  启动、Cookie 是 `__Host-` + `Secure` + `HttpOnly` + `SameSite=Strict`、重跑安装器保留全部状态、
  `reset-password` 这条本机恢复路径。
- 核心场景（`p10-4-core-scenario.sh`）：22.04 **PASS=105 / FAIL=0 / NOT RUN=0**、
  24.04 **PASS=105 / FAIL=0 / NOT RUN=0**。域名访问到指定上游（不是默认站点）、WebSocket 穿过
  nginx 12 条推流不断、同域名与端口冲突被拒、staging 签发与 443、私钥在文件/接口/库/日志里都
  取不到、坏 nginx 配置不破坏已有站点且面板照常登录、重启两个单元之后 443 与 wss 仍然成立、
  收尾按文档路径（enabled=false → apply → DELETE）移除。
- 安装器组状态两支（`p10-4-installer-group-branches.sh`，22.04）：**PASS=15 / FAIL=0**。
  「组在、账号不在」时：组里有成员 → 拒绝并点名 + 给一行修复命令；组为空 → 复用组补建账号，
  装完两单元 active、HTTPS 200、管理员能登录。
- SSH 隧道（`p10-4-ssh-tunnel.sh`，**在宿主机上跑**，从外面进来）：22.04 **PASS=9 / FAIL=0**、
  24.04 **PASS=9 / FAIL=0**。隧道未建时本机端口不可达 → `ssh -N -L` 转发后经 `https://127.0.0.1:19443`
  取到 health 200、证书链与主机名校验通过（未加 `-k`）、响应带 HSTS、明文说话拿不到业务响应 →
  关掉隧道端口重新不可达。
- 宿主：`gofmt` 干净、`go vet` 退出 0、`go test ./...` 36 个包 ok、`scripts/check.sh`
  前端 10 项 + Go 13 项全过（node 固定 v22.23.3）。

**真机抓出并修掉的 3 个产品缺陷**：
1. **Q309（这一轮最贵的一个）**：`systemctl is-active nginx` 在 2 vCPU 虚机上单次可耗时
   11.4 秒 / 16.5 秒，超过 10 秒预算，`unitState()` 返回空串，而执行路径把空串当成“服务没在跑”，
   于是**配置写进去了却从不 reload**，报告还写 `skipped_service_inactive`：80 口仍是旧的 HTTP 块
   （期望 301 得到 200）、443 根本没开，一路把 wss、重启后、收尾等 16 条断言带红。修法是把
   “读不到”与“没在跑”彻底分开：探测最多问 3 次（间隔 300ms），拿到答案照常 reload；三次都读不到
   才如实报告 `unit_state=""` + `unit_state_error`（每次的耗时与原因）+ `served=false`，并给出
   `systemctl reload nginx` 的下一步。**单元确实没在跑时仍然不 reload、也仍然不 restart**（Q287 不动）。
   验收侧补了一条盯根因的断言：绑定证书那次应用必须报 `reload=done` / `served=true`。
2. **Q305**：22.04 上 `useradd` 被“组在、账号不在”卡死（`userdel` 只在组无其他成员时才删组），
   安装器一边宣称幂等、一边被这个状态挡住。修法是分两支：空组复用（`-g`）、有成员则停下点名并
   给出 `gpasswd -d` 的一行修复；卸载时补一次 `groupdel`。两支都有专门的验收脚本。
3. **certbot 失败丢原因**：`syscmd.Runner` 对非零退出返回 `*ExitError`，签发路径把整段输出丢了，
   用户只看到“以退出状态 1 结束”。修法是用 `errors.As` 区分“certbot 拒绝了”与“跑不起来”，
   把 stderr（没有就看 stdout）的尾部带进错误信息；`certbot_test.go` 两条用例钉住。

**验收脚本自身修掉的缺陷**（每一条都记在 DECISIONS，因为“红在夹具上”和“红在产品上”必须分得开）：
Q306 每轮换 `RUN_TAG` 域名与幂等键（否则第二次跑必然撞 409/429，而那两条都是产品正确行为）、
Q307 收尾走文档路径（`DELETE` 被孤儿配置门禁 409 挡住时不许 `rm` 了事）、Q308 `verified` 不等于
“这一刻由新配置应答”（优雅 reload 有 6—111ms 窗口，断言改成有界轮询）、Q310 CSRF 令牌绑会话
（按 cookie jar 存；`$(login …)` 是子 shell，赋值传不出来）、Q311 22.04 镜像上 python 的系统信任锚
是 0 个（24.04 是 120 个）——wss 断言显式传 Pebble 根，“系统信任链”由 curl 单独证明）。
后四处是反复重跑才挖出来的，都不是产品问题、但每一条都曾把一次绿跑成红：Q313 测试 ACME 夹具
每轮从零开始（沿用一个上一轮留下的 Pebble，账号与 nonce 就对不上）、Q314 reload 之后的断言要
等收敛（“80 是 301”只问一次会问到还没交班的旧 worker；顺带发现 wait 辅助函数的提示打到 stdout
被 `$(…)` 一起捕获，两台同时红）、Q315 仓库里的 `*.sh` 必须 LF（Windows 上写盘留 CRLF，
dash 在 `set -u` 处直接退出）、Q316 只对 `badNonce` 重试（测试 ACME 故意随机拒 5% 的合法
nonce，而 certbot 2.9 在取证书那条路径上不重试；重试仅限这条原因、最多 3 次、每轮换幂等键、
失败那份作业另存）。

**结果**：0.1 的两个架构都打得出、校验和可自证；两台发行版的安装、重启与核心运维都有原始日志；
`README.md` 开头写明当前状态与“尚未实现的 1.0 功能”在 §8「已知限制」里逐条列出。
证据 `docs/evidence/P10/2026-10-23-p10.4/`（`result.md` 是结果汇总，`logs/` 是完整输出，
`payloads/` 是每一步的原始 JSON，`env.txt` 是产物与机器事实）。

### P11.1（2026-10-23）— 软件包与更新只读页：dpkg 事实、apt 候选、安全更新与重启标志

**读的时候最容易犯的错，是把“读不到”说成“没有”。** 这一轮的写法把事实与判断切开：
代理只出事实（`apt.packages` 给 dpkg 的登记 + `apt-cache policy` 的候选、候选套件、
优先级；`system.power_state` 给 `/run/reboot-required*` 与内核 `boot_id`），Web 算判断
（`candidate_relation` / `upgradable` / `security` / `pockets` / `counts` / 分页 / 筛选）。
版本比较用 `internal/debversion` 而不是字符串或 semver；`counts` 是全量统计，不随筛选和
翻页变化，`matched` 是“筛选后、翻页前”——两个数字并排放，页面才能同时说“共 731 个包”
和“筛出 1 条”。

**真机抓出一个缺陷：arch 限定名的候选全被谎报成“读不到”。** `dpkg-query` 的
`${binary:Package}` 给的是 `bind9-libs:amd64`，而 `apt-cache policy bind9-libs:amd64`
打出来的块头是 `bind9-libs:`（apt 自己剥了 `:arch`），按请求名索引必然落空，于是这类包的
`candidate_version` 全是 `null`、`candidate_relation` 是 `unknown`，页面显示“候选读不到”，
不报错也不告警。值得记住的是它**怎么被抓住的**：第一次跑完，Web 说 `null`、对账脚本说
`None`，两边“一致”，而验收脚本判失配——因为对账脚本也按请求名索引，它复刻了同一个 bug；
真正独立的判据是肉眼在 VM 上看 `apt-cache policy` 的输出。修法是基础名回退 + 歧义保护
（同名双架构同时装着就**不回退**，如实报 `unknown`：宁可说读不到，也不猜）。修完两台机器
731/676 条全查 0 失配；夹具里留了 `packages-bind9.json`，回放测试断言它的候选不是 `null`。
顺带一条展示规则：`name` 本身就带 `:arch`，页面不能再拼一次（否则 `bind9-libs:amd64:amd64`）。

**24.04 的 apt 慢 28 倍，超时被迫分层。** 同一段代码：22.04（apt 2.4.14）一次
`apt-cache policy` 676 个包名 384ms、首次调用到响应 411ms；24.04（apt 2.8.3）731 个包名
冷缓存 26.6s（≈36ms/名）、首次调用到响应 11.6—15.6s。于是 `agentclient` 的 10s、Web 的
30s 写超时、前端的 15s 都不够用，现在的分层是 45s（`packages.AgentQueryTimeout`）→ 60s
（Web 单请求写超时）→ 120s（代理写超时）→ 30s（快照 TTL），前端 60s。**启动预热被删掉**：
开机就采一次，实测在开机争抢下把 26s 拖成 62s，反而让健康检查误报 `AGENT_UNAVAILABLE`；
现在第一次请求自己付这笔钱，之后命中 30s 快照，候选缓存键是“已装版本 + 索引 mtime”。
能力表里**没有任何 `package.*` 动作**（只读就是只读，装/卸/升级在 P11.2，重启在 P11.3），
验收里有一条直接断言这一点。

**结果**：两台专用虚机、同一份二进制（`agent ac17bec7…` / `web d40ad6c1…` /
`ctl bf29426f…`，即提交 `6551ff9`）、同一份脚本
（`p11-1-apt-acceptance.sh fb57f279…`），各 **PASS 64 / FAIL 0 / NOT RUN 7**，
对账 `MISMATCH=0`（22.04 676 条、24.04 731 条），首次采集 411ms / 11611ms。宿主
`scripts/check.sh` **PASS 13 / FAIL 0**（web 42 文件 759 用例）。`NOT RUN` 的 7 项写进 §8；
`p02-3` 的期望清单改了字符串但**没重跑**，也记在 §8。证据
`docs/evidence/P11/2026-10-23-p11.1/`（`env.txt` 机器与产物事实、`result.md` 结果与限制、
`logs/` 两台机器的完整输出与宿主检查、`payloads/` 每一步的原始 JSON）。
前端回放夹具 `web/src/fixtures/p11-1-vm/`（7 份，真机原样抓取，`README.md` 里有清单）。

### P11.2（2026-10-23）— 软件包写事务：模拟即准入、非交互事先定死、执行后逐包核对

**写之前先模拟，写之后不比退出码比现场。** 预检跑一次 `apt-get -s`，把 apt 自己算出来的
计划（`Inst`/`Remv`/`Conf`、保留包及原因、`E:` 行原文）当准入依据——面板不复刻依赖解析器，
也不猜“删掉 A 大概会带走 B”。执行完**不看退出码**，再读一遍 dpkg 登记与计划逐条比对：
装上的版本是不是 `-s` 写的那一版、该没的是不是真没了、有没有包变成 `iF`/`iU`；对不上就是
`partial`，并点名“期望什么、实际是什么”。这一条把“命令成功了”和“系统真的变成了说的那样”
分开——退出码 0 从来不是证据。

**四个动作的策略在预检之前就定死**：`refresh` 只要锁、`install`/`upgrade` 要再认证、
`remove` 额外要 `REMOVE` 短语（写死在代码里，与“这一次会删几个包”无关——预检之前根本还
不知道连带删除有多大），四个全部**不可取消**：apt 拿到锁开始写 dpkg 之后，“取消”只会
把一个包停在半配置状态。非交互也提前定死：`DEBIAN_FRONTEND=noninteractive` +
`--force-confdef` + `--force-confold` + `Dpkg::Use-Pty=0` + `APT_LISTCHANGES_FRONTEND=none`
+ `NEEDRESTART_MODE=l`。锁只**探测**（自己也 `flock` 一次、拿到立刻放），从不删除：
删锁不会让正在跑的那个人停下来，还会让下一个进程以为没人占用。

**中断之后不自动修。** apt 被信号停住或超时被终止，面板不跑 `dpkg --configure -a`——那会
把“为什么中断”这条线索抹掉。它做的是如实说出是谁停的。这条是被真机逼出来的：第一次硬杀
之后，任务错误写的是 `退出码 -1：Reading package lists...`，等于什么都没说；于是
`syscmd.Result` 多了一个 `Signal` 字段，`writer_exec.go` 把它变成
`apt-get install 被信号 SIGKILL 终止（退出码 -1），事务可能停在中间；先核对 dpkg 状态，
再决定是否重试`。超时也走 SIGKILL，所以区分“谁停的”要看 `TimedOut` 而不是看信号。

**`--force-confold` 从“argv 里的一串字”变成了文件里还剩什么。** 上一版把 conffile 冲突写成
NOT RUN，理由是“验收机上没有这种包，自造要建源、会污染现场”。这一轮升级路径本来就改用
脚本自建的 `file:` 源（两个 8 KB 的夹具包 a/b，各 1.0 与 2.0，`Packages` 索引按 `.deb` 的
实际字节算出大小/md5/sha256，不装 `dpkg-dev`）——**因为发行版里“恰好有个可升级的包”是
上一轮验收消费掉的运气，不是夹具**。于是顺手把 conffile 一起做真了：装 1.0、本地改一行、
升到 2.0，断言的是文件里**还剩什么**（本地那行还在、2.0 的版本行没进来、新的一份落在
`.dpkg-dist`）。第一次做时 `DEBIAN/conffile` 少了个 s，dpkg 一声不响地当普通文件处理，
升级直接覆盖——是断言把它抓出来的，不是人眼。

**结果**：两台专用虚机、同一份二进制（`agent 511fd08e…` / `web 18d0fe00…` /
`ctl 5ebfd18c…`，即提交 `0babda4`）、同一份脚本
（`p11-2-package-write-acceptance.sh 675b0eee…`，1103 行，16 节），22.04.5 与 24.04.5
各 **PASS 169 / FAIL 0 / NOT RUN 4**，两次 `exit=0`（退出码由启动器捕获，不再是字面量）。
宿主 `scripts/check.sh` **PASS 13 / FAIL 0**（web 43 文件 791 用例）；`//go:build unix` 的
三个包第一次在 Linux 上真跑：`syscmd` 24(+1 SKIP) / `processes` 41 / `apt` 77，全过——
正是这一步抓出 `TestProbeLocksNeverRemoves` 与本包写明的语义冲突（unix 上 `ENOENT` 算空闲）。
`NOT RUN` 的 4 项与七条限制写进 §8。证据
`docs/evidence/P11/2026-10-23-p11.2/`（`env.txt` 机器/二进制/实测 apt 成本，`result.md`
结果与限制、六条真机缺陷，`logs/` 两台机器完整输出 + 宿主检查 + Linux-only 单测，
`payloads/{2204,2404}/` 每一步的原始 JSON，`run-launcher.sh` 启动器原文）。

### P11.3（2026-10-23）— 电源：唯一没人能旁观的动作，只能靠 `boot_id` 与"不吹"

**重启和关机是这个面板里唯一"执行者与被观察者是同一个"的动作。** 别的写操作都能在事后
把结果读回来对账（装了什么包、写了哪个文件、挂载点有没有真挂上），电源不行：命令交出去
之后代理进程就随着机器一起消失，没有任何一方还在看着现场。所以这一段的全部功夫都花在
"**能说什么、不能说什么**"上——预检说清影响面与需要什么（再认证、不要短语、可取消、
占 `power-global` 锁），倒计时期间任务 `running`、命令还没发出去、随时可撤销，执行结果是
`accepted`（命令已交出），`verified_at` 必须是空。

**唯一站得住的证据是 `boot_id`。** 内核每次开机换一个，所以任务被读回时（只读单个任务时
才现场核对，列表不核对——列表上每个任务都去问一次代理，等于让页面替代理发并发）面板去问
"你此刻的 boot_id 是哪个"，与下发前记下的那个比：不同 → `confirmed`（这次重启确实发生过）；
相同 → `unchanged`（受理了，但还在原来那次开机上，可能还在倒计时、可能被取消，**不猜是哪
一种**）；连不上 → `unreachable`（关机后会这样，但**不构成关机成功的证据**）；读不出来 →
`unknown`。四个值就是全部能站住的结论，没有第五个"应该差不多"。

**关机这一侧只承认自己不知道。** 面板连电源灯都看不到，所以措辞是：机器已经回来过、
`boot_id` 从谁变成谁、这说明关机之后它重新启动过，**但电源当时是否真的切断，面板给不出
证据，只能由现场回答**。断言同时钉死"不许说什么"：不许出现"关机成功"、`outcome` 不许是
`verified`、`verified_at` 不许有值。这一条不是措辞洁癖——是验收条件里"关机界面不伪称
已验证"的原文落地。

**关键任务闸门**照 STAGES §13 的原样实现：有别的写操作正在执行时，电源操作预检
`allowed=false`、`blocker` 点出那个 job id，提交 409 + `POWER_JOBS_RUNNING`，机器不动
（判据是 `boot_id` 没变，不是看返回码）。倒计时里取消则是这一条的反面证据：任务终态
`canceled`、`outcome` 为空、**没有 `power_check`**（没发生的事没有核对）、`boot_id` 不变。

**真机抓出的产品缺陷：每个重启都自己拒自己。** 现象是 `system.power` 的每个任务都停在
执行前复核（`stage=failed, step=revalidate`，`started_at=0`），错误 `POWER_JOBS_RUNNING`，
而它前面刚跑的预检是 `allowed=true`——表面像"预检放行、提交必失败"，也很容易被误读成
"闸门太严"。根因在问法：`revalidate` 重算计划时不带操作 id，`power.Assess` 固定调
`InFlight("")`，闸门于是把发起者**自己**算成了在跑的关键任务。修法是在接缝处把 id 递下去
（`PlanInput.OperationID → AssessRequest.ExcludeOperationID → InFlight(exclude)`，
预检传空串、执行前复核传 `op.ID`），闸门判据一个字没动。回归测试两层：power 层三支
（预检拦自己、复核放行自己、别人照样拦）、agent 层一支（id 一路传到 `AssessRequest`）。

**验收脚本自己抓出八条问题，其中一条是"假绿"。** 最该记的是 §10 的「审计不含明文口令」：
查询写成 `SELECT group_concat(redacted_details)`，**漏了 `FROM audit_events`**，SQLite 在空
作用域里找列直接报 `no such column`；dbq 的 stdout 是空的、stderr 只进了日志，`grep` 从空串
里当然找不到口令，于是 **PASS**——没查成被当成了清白。修法是补 `FROM` 并让"查询失败"本身
判失败（在 22.04 上用坏 SQL / 好 SQL / 含口令列三支证明这个闸门 fail-closed），两台 VM 的
阶段三用修好的脚本重跑。两版脚本的差异是可验证的：把交付版那段换回第一版，md5 复算
= `28eaba97…`，与 pre/post 实跑的那一版逐字节相同。其余七条：把 Windows PE 推给 Linux
（`Exec format error`，脚本起不来、日志一行没有 → 手工构 `bin/linux-amd64/` + ELF magic
守卫，起跑线拒跑）；30 秒倒计时与脚本自己的 25 秒观察窗抢命（改 60 秒、终态断言移到下一
阶段并如实记 NOT RUN）；`cleanup()` trap 把代理优雅停掉、倒计时里的 reboot 变 `interrupted`
（两阶段收尾 `KEEP_SERVICES=1`）；`jget` 的裸变量名 `NameError` 被吞成 `ERR:` 字符串让
`expect_ne` 假通过（改 `d["boot_id"]` + `require_value`）；`fs.protected_regular` 让 root 也
truncate 不了 upanel 在 sticky 目录里的文件，等待器于是读到**上一轮**日志、"阶段二 11 条
FAIL"全是假的（日志写进 root 自己的目录，先看 mtime 与行数再信内容）；WHPX 下重启挂死
（`WHPX: Unexpected VP exit code 4`，`reboot: Restarting system` 之后再也不回来 → `-Accel tcg`）。

**重跑的第 2 轮抓出了真正的根因：一个产品缺陷在删别人正在用的 socket。** 22.04 的阶段二
一开始就红了 6 条，全是 `GET /system/power-state` 返回 503（`AGENT_UNAVAILABLE`）。顺线查：
Web 日志说 `dial unix /run/upanel/agent.sock: connect: no such file or directory`，而代理
18:14:35 起就在 listen、18:15:11 还正常答过预检——**进程活着，socket 没了**。两个原因叠在
一起：①环境里留着早前安装验收的 `upanel-agent.service` / `upanel-web.service`，`ExecStart`
指向已被删除的二进制，`Restart=on-failure` 下每 3 秒重生一次（`203/EXEC`、`NRestarts=116`），
它们的 `RuntimeDirectory=upanel` **每停一次就删掉 `/run/upanel`**；②产品侧
`prepareSocketPath` 对已存在的 socket 一律 `os.Remove` 再 bind——"连得上"和"连不上"在它眼里
一样，于是每个重生周期都在删还活着的代理的路径，抢输的那个退出时又把路径删了。修法是让它
先问一句：连得上 → 已经有一个代理在服务，**拒绝启动且不碰文件**；`ECONNREFUSED`/`ENOENT`
→ 陈旧，照旧清理；说不清（超时等）→ 按有人处理，同样不碰——**分不清就不删，删掉的可能是
一个活着的代理**。两个回归测试分别钉住"活的不许抢"（原监听者拒绝启动后仍然可连）与"陈旧
的必须能清"（否则代理崩一次就再也起不来）。环境那一半由脚本的 `guard_foreign_units` 兜住：
每阶段停用 + mask 外来 `upanel*.service`（`/etc` 里的真实单元文件 mask 会失败，改为改名
挪走），断言一个都不剩，停不掉就 exit 2——这就是 §0 新增的 `env-no-foreign-units`。
第 1 轮的日志本来只用来证明这两个缺陷是真的，确认之后整份作废，两台从干净状态按
"提交 → 构建 → 跑"的顺序重来（`7d4039e`）。

**结果**：两台专用虚机、同一份二进制（`agent 6ce5726f…` / `web 876ce0a3…` /
`ctl 48c360ae…`，自报 `0.0.0-p11.3 (commit 7d4039e)`）、同一份脚本（md5 `93e7251c…`
四阶段全程一致），22.04.5 与 24.04.5 各 **PASS 109 / FAIL 0 / NOT RUN 2**，
逐节断言数两台完全相同（§0 2、§0.1 7、§1 11、§2 11、§3 11、§4 8、§5 7+1N、§6 8、§7 12、
§8 12+1N、§9 15、§10 4、§11 1）。两条 NOT RUN 不是漏跑，是**位置问题**：reboot 的终态只能
在重启之后读、poweroff 的终态只能在上电之后读，各自在下一阶段被同一事实的 PASS 覆盖
（`reboot-after` / `poweroff-after`：`accepted`、无 `verified_at`、命令与退出码、"人工开机"
提示、核对消息拒绝声称）。真机事实：22.04 `591a2533 → 5389231a → 4a25a825`，24.04
`97057b04 → 22486dd5 → 813e082b`——三个 id 互不相同，所以"重启过一次"与"断电后又上电"
分得开；24.04 关机后 QEMU 进程从宿主上消失（串口 `reboot: Power down` 留在证据里），
是真断电而不是假关机。宿主 `scripts/check.sh` **PASS 13 / FAIL 0**（web 45 文件 820 用例，
修完之后在最终代码上重跑）。
九条限制写进 §8（无浏览器渲染、**24.04 的客户机热重启没验到**、电源切断只能由现场回答、
final 阶段必须人/宿主上电、`power-global` 的并发竞争未验、systemd inhibitor 未验、
倒计时边界只验了校验层、"关键任务"口径是通用的、socket 守卫只验了单机语义）。证据
`docs/evidence/P11/2026-10-23-p11.3/`（`env.txt` 构建/指纹/两台虚机的 boot_id 链与三处
环境干预、`result.md` 结果与限制、`logs/` 四阶段完整输出 + 宿主检查 + 版本指纹 +
24.04 的断电与重启挂死串口原文、`payloads/{2204,2404}/` 每一步原始 JSON）。

### P12.1（2026-10-24）— 备份仓库：先证明"仓库是读回来的"，再谈备份

**这一段只做"仓库"这一层，不做备份本身。** 写快照、还原、校验一致是 P12.2/P12.3，
所以 BACKUP-01 的"备份后恢复"本轮**没有对象可验**——它进 NOT RUN，不冒充通过。
能证明的是七件事：仓库真的建起来了（`restic cat config` 读回的 64 位 id 与脚本自己再读一次
**逐字节相同**）、只建新仓库不覆盖已有数据（已存在就 `BACKUP_REPOSITORY_EXISTS` 拦住）、
主机密钥是我们自己核对的（指纹只认登记的、known_hosts 由我们核对通过后写 0600、
`StrictHostKeyChecking=no` 全链路一次不出现）、秘密按用途把关（TLS 私钥当口令 →
`BACKUP_SECRET_PURPOSE`）、容量是读出来的不是 0、口令错要说得出口令不对、默认拒绝
（root 碰镜像路由 → 403 `拒绝对端 UID 不在允许范围`）。

产品面：`internal/backup`（定义、指纹、拨号核验、restic 调用、容量、判决、执行）、
代理侧一个动作 `backup.repository.initialize` + 一个查询 `backup.repository.state` +
**仓库定义镜像路由**（配置写入不该产生 job、不该要再认证；PATCH 带 `version` 做乐观并发，
DELETE 只删定义不删数据），秘密用途闭集加两条。契约见 CONTRACTS §15。

**真机抓出三个产品缺陷，一个比一个隐蔽：**

① **A：登记的是指纹，服务器先递的是另一把。** 24.04 上第一遍 **PASS 88 / FAIL 33**，
指纹**对**的仓库也报 `BACKUP_HOST_KEY_MISMATCH`（登记 `SHA256:R7OL…` 的 ed25519，
握手递过来的却是 `ecdsa-sha2-nistp256`）。根因是 x/crypto 的 `defaultHostKeyAlgos`
把 ECDSA 排在 Ed25519 **前面**，而 `HostKeyCallback` 只能对协商出的那一把表态，没有
"换下一把"的钩子；管理员抄的 `ssh-keygen -lf` 第三列只有摘要没有类型，契约里表达不出
"按类型钉死"。修法（`d99998c`）：不匹配时把 `HostKeyAlgorithms` 钉成一种算法逐个重握手，
见过的类型不再试，**只有指纹相等才算通过**。

② **B：一次握手抖动就把"连不上"定死。** `1ae4e39`（把"执行前复核被拒"的原因写进日志）
先给了真原因：`BACKUP_SFTP_UNREACHABLE` + `read tcp 127.0.0.1:56442->127.0.0.1:22: i/o timeout`
——连接建起来了，是握手读超时吃满 15 秒；紧接着在同一台机器上手动重放 6 次全部数百毫秒
内连上，sshd DEBUG2 无鉴权失败、每秒连接采样无 ≥3 秒断层（机器没冻住）。修法（`de3cef8`）：
`Dial` 拆成"网络层重试外壳 + 算法轮"，只对 `isNetworkFailure` 再走一轮（两轮封顶、
间隔 250 ms），认证失败与指纹不符不重试。原文见证据 `logs/2204-dial-timeout-round.txt`。

③ **C："我们没等完"被写成"restic 出错"。** 24.04 上用 `de3cef8` 跑是 **PASS 132 / FAIL 1**，
唯一失败是口令错的现状查询，原因原文 `restic cat config 失败：backup: /usr/bin/restic
以退出状态 -1 结束`。查下去是**只读查询自己的 30 秒预算先用尽**，上层 ctx 到期把整组
进程一起杀掉，而 `runOutcome` 只判了"restic 自己的超时"，父 ctx 到期掉进 `ExitError`
分支。同机量到：ssh 握手 3—9 秒、SFTP 逐次 stat 各一往返、`restic cat config` 5—12 秒，
整串 30 秒上下——**30 秒的预算刚好卡在边界上**，把一次正常的查询判成超时。修法
（`659ba02`）：`runOutcome` 分出 `ErrBudgetExhausted`（"这次没有结论，不等于仓库有问题"）
与 `ErrExecTimeout`，`ExitError` 带上信号名，预算 30 → 45 秒（预检本来就是 40 秒）。
**这不是把验收条件放宽**：脚本那条断言一字未改，改的是"到底谁把进程停掉的"这句话。

另有**三条假绿**被脚本自己抓出来（`空等于空` 的 id 对账、上一轮留下的 known_hosts
让"是我们写的"无条件成立、`run_op` 的响应被请求体覆盖），加上 `PREFLIGHT_STALE`
每轮必现（`session_id_hash` 各随机一次）、`preflight-malformed-id` 断言错对象
（形状校验属于镜像路由）、上一轮的定义连锁打红，六条都在 `d99998c` 里修掉。

**结果**：22.04.5（`tcg,thread=multi`）与 24.04.5（`tcg,thread=single`）各
**PASS 133 / FAIL 0 / NOT RUN 5**、`EXIT=0`，逐节断言数两台完全相同（§0 2、§1 4、§2 3、
§3 25、§4 7、§5 19、§6 11、§7 13、§8 5、§9 5、§10 6、§10b 6、§11 13、§12 7、§13 6、
证据卫生 1）。仓库 id 22.04 `5b2ac62d…`、24.04 `6044228b…`（两台的 `repo_version` 分别是
1 与 2——restic 0.16 默认写 version 2，我们如实转述读到的值）。宿主 `scripts/check.sh`
**PASS 13 / FAIL 0**（web 45 文件 820 用例）。五条 NOT RUN 写进 §8（备份与恢复、
备份中途断网、存储满、目标不是目录、两个 restic 版本"行为一致"），另有三样本阶段根本
没有的东西：Web 侧 `/api/v1/backup/*` 与页面（P12.2—P12.5）、定时任务（P12.4/P12.5）、
`go test -race`（无 cgo）。证据 `docs/evidence/P12/2026-10-23-p12.1/`
（`env.txt` 两台环境与二进制指纹、`result.md` 结果与三个缺陷、`logs/` 两台的完整输出与
宿主检查、`payloads/{2204,2404}/` 各 42 个原始 JSON）。

### P12.2（2026-10-24）— 策略、运行、快照与面板库副本：先定"谁说了算"

**这一段的第一个决定不是代码，是"定义存在哪儿"。** 仓库与策略在面板侧**不建表**：
代理里那份镜像就是唯一的真相，`/api/v1/backup/*` 整组是它的编辑器。理由不是省事——
预检计划里的 `normalized_params` 是冻结的，执行时再向 Web 要一遍定义就等于让"被冻结的
目标"可替换；而如果面板另存一份，两者迟早漂移，"这条策略绑的是哪个仓库"就会有两个答案。
代价写在明处：代理不在时整组 **503**（`AGENT_UNAVAILABLE`），不是"先登记、回头同步"——
那个"回头"从来没有发生过一次。CONTRACTS §14.1 的 `backup_repositories` / `backup_policies`
两行因此仍属逻辑模型，本轮**没有**对应迁移。

**产品面**（`internal/backup` → `internal/agent` → `internal/agentstore` → `internal/agentclient`
→ `internal/api`）：

- 仓库定义 CRUD（无 DELETE：契约里没有，且删定义会让"这些快照属于谁"无从回答）与策略 CRUD
  （可以删：删策略不动仓库、不动快照，审计里明写 `cascaded=false` / `kept:[repository,snapshots]`）。
  权限按"这条定义引用了什么"分层：仓库引用口令与私钥 → CSRF + 再认证；策略不引用秘密 → CSRF。
- `backup.run` 的预检与执行。参数按契约 §5 是 `backup_policy_id` + `version`，第三个字段
  `apps` 是**服务端展开**结果：`apps.TypeSystemd` → `systemd/<unit>`、`apps.TypeDocker` →
  `container/<id>`。调用方在 `/preflights` 里自己给 `apps` 直接 400（严格解码，不是忽略）。
  代理侧 `appsVsPolicy` **双向**核对：多给 = 扩大授权（拒）；少给 = 停机不全（阻塞，
  `BACKUP_APP_UNAVAILABLE`），消息点名是哪个 `app_id`、为什么不降级。
- 一致性口径闭集 `files_only` / `stop_managed_apps`，同时是快照标签
  （`upanel:consistency:<mode>`）。`files_only` 会在预检里告警"正在写入的数据库在快照里
  可能不一致"；`stop_managed_apps` 要求策略里**每一个** app_id 都能解析出可启停对象，
  `compose`、只登记入口的链接型、已删应用三种情况回 422 `DEPENDENCY_BLOCKED` 并说明理由。
- 执行顺序：读现状（只认"刚才真在跑的"）→ 停（逐条 `done/verified/note`）→ 面板库在线副本
  → `restic backup` → `restic stats` 回读快照 id（`Verified` 只在读回来之后成立）→ 拉起
  （`context.WithoutCancel`，**失败路径也要拉起**）→ 清临时副本。
- 面板库副本用 `VACUUM INTO` + `mode=ro`，不用 `os.Copy`：直接拷 `.db` 会漏掉 `-wal` 里
  已提交的事务，得到一份看起来正常、实际更旧的库。副本目录 0700 / 文件 0600，只在这次运行
  期间存在，`observed_state` 里只留大小、`user_version`、`integrity_check` 与时间。
- 快照查询两条：`backup.snapshots`（列表，`size_included:false`、`size_bytes` 字段**缺省**
  而不是 0——每条要单独跑一次 `restic stats`）与 `backup.snapshot`（详情，带 stats 与
  `restore_verified_at`）。`registered` / `found` 是**状态字段**，不回 404：404 会被读成
  "这条路由不存在"。字节数按公共契约用十进制字符串输出，不用 Number。

**实现初期的一处包测试红是判据错，不是产品错**（记进 DECISIONS Q342；后续真机缺陷另见下文）：`TestUpdateRepositoryVersionConflict`
期望 409 拿到 400——用例自己的 PATCH body 里 `location` 少了 `relative_path`，400 出在
`Normalize()`、在调用代理之前；结尾那条 `updatedV != 0` 读的又是前两次调用留下的痕迹。
改判据（补全 body、断言前清零），不放宽断言。

**另修一处不一致**：删策略原本回 `204`，而这份 API 的每个响应都带 `request_id` 信封——
一个 204 会让前端那条通用解析路径在这里断掉。改成 `200` + 空对象，与站点、应用、分组一致。

**验证（宿主）**：`go build ./...`、`go vet ./...` 无输出；`go test ./...` 全包通过
（`internal/api` 117.6 秒）；`redocly lint api/openapi.yaml` 有效（6 条警告全部是改动前
就有的：health 两条无 4XX、terminal connect 无 2XX、三个未被引用的通用组件）。
新增用例：`internal/api/backup_test.go` 14 条（权限 401/403、无调用端 503、列表形状、
create 要再认证 + 审计、版本冲突 409 / 记录不在 404 / 缺 version 400、策略 CRUD + 审计、
`stop_managed_apps` 缺 app_ids 400、快照列表必须给 `repository_id`、列表无 size、
详情带 size 且 `restore_verified_at` 为 null、调用方给 apps 400、策略不在 404、
解析不出 422 并点名、compose 422、files_only 空数组），`internal/agent/backuprun_test.go`
3 条（策略应用少给就阻塞、files_only 不带 apps、预检计划带评估），
`internal/backup` 的 `policy_test.go` / `run_test.go` / `paneldb_test.go`，
`internal/agentstore/policystore_test.go`。

**验证（最终真机）**：同一最终脚本（sha256 `d7028b17…40c78`）与同一组二进制在
Ubuntu 22.04 / restic 0.12.1、Ubuntu 24.04 / restic 0.16.4 上分别完整执行，均为
**PASS 170 / FAIL 0 / NOT RUN 3，exit 0**。宿主逐字比较两台 `results.txt`，名称、数量、
顺序一致；并结构化复核四个新增任务：SFTP succeeded，断网与写满 failed + COMMAND_FAILED，
取消 canceled；SFTP 快照为完整 64 位 id、`verified_by=restic stats`，断线前后列表均包含它。
断网只杀本次 restic 的 SFTP ssh；写满只使用独立 64 MiB tmpfs 且 filler 真撞 ENOSPC。
两类失败的源哈希均未变，baseline 快照用完整 id 独立回读一致；写满后的只读核对使用
`--no-cache --no-lock`，避免核对工具自己向已满仓库写锁。

真机共抓出并修复 4 个产品缺陷：Q343（0.12 短快照 id 唯一解析）、Q344（Web 写超时容纳
慢机器 Argon2id）、Q345（进度回调堵管道与 ErrWaitDelay 分类）、Q347（备份专用 100 秒
agentclient 晚于代理 90 秒查询预算）。Q348 记录故障注入、PEM 编码、追加 filler、只读核对
和证据边界的修正。最终宿主 `scripts/check.sh` 为 13/0/0，日志与所有失败现场均保留。

**结论：P12.2 passed。** 剩余 NOT RUN 只有产品恢复、保留清理、浏览器 UI，分别属于
P12.3/P12.5；不折算成通过。最终证据在远程与宿主两侧扫描测试口令和私钥标记均零命中。
完整结果、二进制/脚本指纹、任务 id 和历史失败现场索引见
`docs/evidence/P12/2026-10-24-p12.2/result.md`。

### P12.3（2026-10-24/25）— 恢复：先证明"目标是谁"，再谈"把文件放进去"

**这一段最难的地方不是 restic，是"替换一个目录"这件事本身。** 备份只要读，恢复要写，
而且写在别人的数据上。所以恢复的顺序被定成一条链，每一环都不许跳过：

1. **先证明目标是谁**。`target_root_id` 指的是面板登记过的根，执行前重新比对它的身份指纹
   与版本；根被换成软链、被替换、被改写，`revalidate` 直接给 `DEVICE_IDENTITY_CHANGED`，
   恢复不开始。这一条与磁盘动作共用同一个 ophand：**预检放行不等于执行时还放行**。
2. **默认不覆盖**。目标必须不存在或为空；非空且没带 `overwrite` 时预检就 `allowed=false`。
   要覆盖也不是"先删再写"，那是把旧数据当赌注：先在同父目录建随机暂存树，restic 经
   `F_DUPFD_CLOEXEC` 复制的独立目录描述符写进 `/proc/self/fd/3`（口令仍只从
   `RESTIC_PASSWORD` 走，私钥不落地），**写入目标与真实目标同一文件系统**，最后一步才换。
3. **换是原子的**。目标已存在用 `RENAME_EXCHANGE`，不存在用 `RENAME_NOREPLACE`（宁可失败
   也不覆盖别人刚建的东西）。换完重新遍历核对整棵树，通过之后才删旧树；核对不过，旧树还在。
4. **失败不发布**。写满、断连、restic 非零退出、抽样不一致——任何一条都在"换"之前失败，
   暂存树清掉、原目标一个字节都没动、**被停的应用照样拉起**（`defer` 走 `WithoutCancel`，
   失败路径也执行）。停应用这一步只停"刚才真的在跑"的，跑完只拉起停过的那些。

**验证分两态，而且必须自己说清楚是哪一态。** `backup.verify` 的 `sample_restore=false` 只做
仓库结构检查（`restic check`），结果里 `restore_verified=False`——它不是"备份能恢复"的证明；
`sample_restore=true` 才真抽样恢复到临时目录并核对内容。这个字段就是给 UI 和巡检看的，
免得把"结构没坏"读成"能恢复"。

**保留清理只删自己冻结的那一批。** `backup.prune` 预检把 `expected_delete_ids` 冻进计划，
执行时集合一变就 `PREFLIGHT_STALE`，不删任何东西（`prune-stale-deleted-nothing-*` 两条分别
覆盖本策略与第二策略）。清理只作用于指定仓库：本地 prune 之后重新读 SFTP 仓库的快照列表
（**清理后**读，不复用清理前的响应，Q355），另一个仓库逐字未变。

**两件跟"运维到底能不能用"有关的事。** 一是仓库里的失效锁：`check` 与 `forget --prune`
要独占锁，主机崩一次就永久卡住，而 restic 那句"可以用 unlock 命令"在面板上等于没说——
把口令只经标准输入、私钥只在 tmpfs 里短暂存在的产品，运维手上没有能跑的 restic。所以产品
自己清（Q359）：命中残留锁句式先 `restic unlock`（不带 `--remove-all`，只删 restic 认定的
失效锁）再重试一次。二是失败消息：`Fatal: There were 10 errors` 对运维没有任何信息量，
写满磁盘、没权限、路径不存在在那一行里长成一个样子；补第一条具体错误（Q360），而具体错误
的真实形状是 `ignoring error for <path>: <err>`，不是 `error:`（Q363，这一条是我自己第一版
没修对、被真机打回来的）。

产品面：`internal/backup/{restore,restore_types,prune,retention,exec,restic,restic_fd_unix,restic_fd_other}.go`、
`internal/agent/{backuprecovery,appstop,backupquery,backupaction}.go`、
`internal/agentstore/{verificationstore,backupstore}.go`、`internal/agentclient/backup.go`、
`internal/api/{backup,backupexpand}.go`（`internal/backup` 里没有 `recovery.go`：恢复的核心
在 `restore.go`，跨包串联在 `internal/agent/backuprecovery.go`）。契约 §12.3 补了失效锁与
失败消息两条不可让的行为。

**真机抓出并修掉 4 个产品缺陷**：Q356（`os.NewFile` 包装借用 fd，GC 把恢复目录句柄关掉，
长恢复必踩）、Q357（会话在库里滑动续期、Cookie 按登录时刻到期，长任务跑到 30 分钟全 401）、
Q359（失效锁）、Q360/Q363（失败消息只剩汇总，且补原因只认 `error:`）。验收方法上修正
5 条：Q355（清理后必须重读现场）、Q358（按字节填满会被预检正确拦下）、Q361（并发验收会
把失败判给产品，两份日志作废）、Q362（改用 inode 耗尽夹具，测"写到一半失败"）、
Q364（`gofmt` 未清零就不构建；部署用 `install` 逐文件覆盖并逐文件核哈希，`mv` 通配曾静默
留下旧的 `upanelctl`）。

**结论：P12.3 passed。** 两台 VM（24.04/restic 0.16.4、22.04/restic 0.12.1）各
**PASS 117 / FAIL 0 / NOT RUN 0**，退出码 0，逐节断言数与断言名单跨机逐字一致；宿主
`check.sh` 13/0/0（node 必须按 `.nvmrc` 用 v22.23.3，默认的 v24 会被闸门拦下），
`//go:build linux` 的三条用例另在两台 VM 上各实跑一遍全 PASS（Windows 宿主编译不到它们）。
失败路径不只核对任务终态，还核对源/旧目标哈希不变、无暂存残留、应用仍 active。
剩余 NOT RUN：浏览器恢复/清理 UI（P12.5）、timer 与错过执行策略/时区/非 root 脚本（P12.4，
**已由 P12.4 解除**，见下一小节）。
完整结果见 `docs/evidence/P12/2026-10-24-p12.3/result.md`。

### P12.4（2026-10-25）— 受管理 timer 与受管理脚本：让"到点自己跑"变成可验证的事实

**这一段的难点不是 cron 表达式，是"谁在替你做事、你凭什么相信它做了"。** 定时任务跑的时候
没有人在看，出错也不会有人当场发现。所以整段的形状是——定义只有一份（代理里）、单元是可读的
（时区/错过执行/最长运行/不重叠都写在文件里）、触发是幂等的（同一分钟重放不新增操作）、
结果会被对账导入（任务表里看得到，也知道它从哪来）。

1. **两份真相会立刻对不上。** 计划与脚本只存在代理库，Web 的 `/schedules*`、`/scripts*`
   是代理镜像直读；代理不在就 503 `AGENT_UNAVAILABLE`，不回空列表——空列表会让管理员以为
   "计划都没了"，然后照着记忆重建一遍。id 形状（`sch_`/`scr_` + 32 位十六进制）在打代理
   之前就 400。
2. **意图与事实分开写。** `enabled`/`version`/`timer_ref` 是面板要的，`installed`/
   `unit_file_state`/`active_state` 是 systemd 的原话；`next_elapse` 读不到时字段缺席，
   不编时间；`blockers` 永不为 `null`。
3. **不重叠不是注释，是 `--wait`。** 单元初版只写了 `Type=oneshot` 并声称"上一次没跑完就
   不会起第二次"——这句话是错的：`schedule-trigger` 把触发交给代理就返回，真活跑在代理
   后台，单元几十毫秒就结束。真机上这会让长任务每分钟叠一层（第二个操作排队 30 秒后
   `RESOURCE_BUSY`，journal 每分钟一条红）。修法是把"单元 active"和"这次操作还在跑"绑成
   一件事（Q371）：`schedule-trigger --wait` 轮询到终态才退出，`TimeoutStartSec` 取计划
   上限 + 60 秒收尾。
4. **触发只认 root，输入只有触发 ID。** `POST /internal/v1/schedules/:id/trigger` 按路径
   形状单独放行 UID 0，其它内部路由仍只接受 `upanel-web`；动作来自代理里已批准的定义，
   调用方给不了 action/params。槽位折到 UTC 分钟、`TriggerID` 幂等、伪造槽 400，判决闭集
   `triggered|blocked|duplicate|failed`（`blocked` 是策略在起作用，不是故障）。
5. **系统操作者在库里有一行，但登录不了。** 保留 id `1<<62`，面板惰性建 `scheduler` 行
   （`role='viewer'`、`disabled=1`、哈希前缀 `!`），登录回答与"没有这个账号"同码同文案，
   用户列表里不出现。定时操作每 30 秒被对账导入 `jobs`（`cancellable=0`、`needs_sync=1`，
   预检引用如实抄写），水位取 `jobs.created_at` 而不是导入时刻。
6. **脚本是非 root 的。** 落盘 `root:upanel-shell 0750`、解释器固定 `/bin/sh`、运行身份
   `upanel-shell`（不在 sudo/docker/disk 组里，写不了 `/etc`）、运行前核对 SHA-256、
   被计划引用时删不掉（`SCRIPT_IN_USE`）、超时被终止并把原因写进任务。

产品面：`internal/schedules/{module,installer,units,schedule,params,record}.go`、
`internal/scripts/{module,scripts,params}.go`、`internal/agentstore/{schedulestore,scriptstore}.go`、
`internal/agent/{scheduleaction,scheduleservice,scheduletrigger,scriptaction}.go`、
`cmd/upanel-agent/scheduletargets.go`、`cmd/upanelctl/schedule.go`、
`internal/agentclient/{schedule,client}.go`、`internal/api/{schedule,router,server}.go`。
契约 §12.4 补了若干条不可让的行为，其中第（12）条是"读一份计划列表的代价随条数增长，
调用上限也按这个定"。

**真机抓出并修掉 9 个产品缺陷**：Q371（不重叠没被实现）、Q374（`interpreter` 库里存基名
`sh`、契约写 `/bin/sh`，最后以 `CHECK constraint failed` 炸在真机上）、Q375
（`systemd-analyze calendar` 在 UTC 机器上少打一行 `(in UTC)`，每条计划都被判成"不会触发"）、
Q376（脚本目录的上级 `0700 root:root`，`upanel-shell` 连穿都穿不过）、Q377
（`UNIQUE(kind, target_id)`/`UNIQUE(name)` 不在预检说话，预检放行、提交 409）、Q380
（root 跑的 `--wait` 读不到它要等的那次操作，一直在空转）、Q381（导入即终态的任务
`needs_sync` 永远停在 1，扫描每 30 秒再问一遍代理）、Q382（`busy_timeout` 5 秒仍
`database is locked`）、Q385（一条列表读 10144 毫秒，压过 Web 侧 10 秒上限，健康的代理被
报成"代理不可用"）。验收方法修正 9 条：Q370、Q372（行式 JSON 助手的答复不能带裸换行）、
Q373、Q378、Q379、Q383（"一共只触发一次"不是"不重叠"）、Q384（两种"没有"被混成一种）、
Q386（探针把"收敛"定成 `needs_sync==0`）、Q387（"不重叠"的判据认了 systemd 的行文与
T+65 那一刻的抽样）。

**结论：P12.4 passed。** 两台 VM（24.04/systemd 255、22.04/systemd 249）各
**PASS 246 / FAIL 0 / NOT RUN 0**，退出码 0，逐节断言数与断言名单跨机逐字一致；宿主
`check.sh` 13/0/0（web 45 文件 820 用例）。**22.04 是第二轮才绿的，首轮 5 条红全是验收
工具自己的毛病（Q386、Q387），产品当时是对的**——同一份真 journal（`S F S F`，79 秒）
证明实现没问题、判据错了，两轮都留了记录。剩余 NOT RUN：浏览器备份/定时任务 UI（P12.5）、
证书续期的真实执行（P12.5，本轮只验"接线前就 blocked 且指向 P12.5"）、`go test -race`
（宿主无 gcc，`-race requires cgo`）。
完整结果见 `docs/evidence/P12/2026-10-24-p12.4/result.md`。

### P12.5（2026-10-25）— 证书续期 timer 接入、备份页与计划任务页：没有人在看的时候，凭什么相信它做对了

**这一段难的不是接线，是"凌晨三点没有人看"。** 定时续期跑在 systemd 里，触发那一刻面板
可能关着、代理可能重启过、证书可能早就换掉，所以链路每一段都必须自己说明：定义由面板
按账本展开（调用方填的域名一律不信）、单元由代理写、触发只认 root 且幂等、结果按
observed 回写、面板再按域名对账导入任务表。判据也不是"接口返回 200"，而是**三处事实必须
对得上**：账本的 `fingerprint` / `not_after` / `last_error`、任务表那一行、盘上
`live/<domain>/cert.pem` 的真实指纹。

1. **计划里存的是域名与 staging，不是证书 id。** 续期真正需要的只有两件事——域名和
   要不要 staging——它们本身就是落盘事实，不是引用（Q388）。执行那一刻再去查证书记录，
   账本可能已经对不上：行被删了、域名被改了、ACME 环境换了，凌晨三点就会对着一个不存在的
   目标跑。所以 `schedule.apply` 只收调用方给的 `certificate_id`（或什么都不给），域名与
   `staging` 由面板按账本展开填进去，调用方自己填的域名一律不信——否则就是一条
   "给任意域名签证书"的旁路。
2. **代理按域名回答"这张证书还在不在"。** `TargetLookup` 的入参从证书 id 改成域名，
   查的是 `/etc/letsencrypt/live/<domain>` 这个盘上目录：账本是面板的镜像，盘上那个才是事实，
   两者不一致时信盘上那个（`cmd/upanel-agent/scheduletargets.go`）。
3. **签发与续期是同一条动作，靠 `scheduled` 分开语义。** 定时触发只发
   `{domain, staging, scheduled:true}`，不带证书 id、不带 `force`，于是
   `--keep-until-expiring` 天然生效——一张还没到期的证书不会被无谓地重签，这是速率限额的
   用法；要"现在就重签"只有人在面板上点续期那一下（`forceRenewal` 认的正是这个条件）。
4. **失败必须留下原因，而不是一句"动作执行失败"。** 注册表在动作失败时只写
   `error.message`，失败任务的 `result` 按契约是空对象，certbot 的 `stderr_tail` 到不了
   管理员眼前；一次每晚都在失败的续期，界面上分不出是 80 不通、账号失效还是速率限额。
   `certbotFailure` 实现 `FailureReasoner`（Q391），判据是**任务表那句必须等于账本
   `last_error`，并且必须出现 certbot 自己的话**。
5. **预检要绑得上目标。** 判决说 `allowed=true` 却没有绑定，注册表按"实现错误"回 500，
   systemd 只把单元的失败告诉你——人到手的是一个数字（Q390）。定时续期没有证书 id 可绑，
   目标就是那个域名本身。
6. **页面不自己判断"要不要再认证"。** 备份页与计划任务页的 `needsReauth` 从代理的
   `ActionPolicy` 派生，页面不按"这个动作看起来危不危险"自己判：`backup.run/restore/prune`、
   `certificate.issue/renew`、`schedule.*`、`script.*` 都要再认证，`backup.verify` 是唯一
   不需要的那条。判错的后果很难看——对话框说"确认即可"，提交时服务端回 403
   `REAUTH_REQUIRED`，管理员以为面板坏了。同理，prune 的待删名单不许页面拼：它由预检算出、
   代理写进 `normalized_params.expected_delete_ids`、提交时按同一份复核（Q389）。

产品面：`internal/schedules/{module,schedule}.go`、`cmd/upanel-agent/scheduletargets.go`、
`internal/certificates/{ops,ledger}.go`、`internal/agent/{certificateaction,preflight}.go`、
`internal/api/{schedule,scheduleexpand,preflightexpand}.go`、
`web/src/views/{BackupView,SchedulesView}.vue`、`web/src/api/{backup,schedules,preflights}.ts`、
`web/src/components/{backup,schedules}.ts`、`web/src/utils/jobresult.ts`、
`web/src/modules/registry.ts`、`web/src/router/routes.ts`。契约 §12.5 补了若干条不可让的
行为，其中第（7）条是"再认证标志从代理抄、参数只带该带的字段、`GET /scripts/:id` 外面包了
一层 `{script:…}`"。

**真机抓出并修掉 4 个产品缺陷**（Q390 定时续期预检绑不上目标身份、Q391 失败原因到不了任务
与账本、Q392 `Module.Apply` 里 P12.4 的"尚未接线"闸没拆、Q393 审计行按解码它的结构体长
——空的 `certificate_id`/`email`）；**验收方法修正 5 条**（Q394 关校验口造不出失败、Pebble
把通过过的授权缓存一小时；Q395 断言 80 与 5002 是空的；Q396 §9 拿整轮开始时的会话去读接口；
Q397 prune 数仓库总条数与水位取在触发之后；Q398 采样器 `ps -e -C` 自己把 2 vCPU 客户机
压到运行队列 9 条）。24.04 一共跑了 12 轮、22.04 跑了 2 轮才全绿，前 11 轮的红按
"夹具 → 判据 → 产品"的顺序一层层剥开。

**结论：P12.5 passed。** 两台 VM（24.04/systemd 255/certbot 2.9.0、22.04/systemd 249/
certbot 1.21.0）各 **PASS 214 / FAIL 0 / NOT RUN 0**，退出码 0；逐节断言数（25/20/44/24/30/
15/20/10/19/7）与归一化后的断言名单跨机逐字一致；同一份脚本（md5 `9fd078cb…`）同一组
二进制。宿主 `check.sh` **PASS 13 / FAIL 0 / NOT RUN 0**（按 `.nvmrc` 用
`H:\Code\.toolchains\node-v22.23.3-win-x64`；同一个 commit 先拿系统 node v24.15.0 跑过一次是
11/2，两条红都在工具链层——`node-version` 与 `openapi-lint` 的 libuv 收尾断言，换回锁定版本
后同一份代码全绿，两次输出都留在证据里）。
剩余 NOT RUN：浏览器里真实点击两个页面（本轮验的是"产物打进了二进制、里面带着这些动作"
与"页面依赖的读接口活着"，不是渲染与交互；`web-smoke` 走的是首页/存储页，不覆盖这两个
页面）、`go test -race`（宿主无 gcc）。
完整结果见 `docs/evidence/P12/2026-10-25-p12.5/result.md`。

### P13.1（2026-10-26）— 历史监控：先决定"什么时候该没有数据"，再谈怎么存

**这一段难的不是存点，是回答"为什么这里没有点"。** 同一个"过去一小时 CPU 怎么走的"，
一分钟前看和三个月后看读的不是同一张表；代理停过十分钟，那十分钟就**必须**是空的，
不能补零、不能插值、不能拿上一次的值填——补过的曲线会让"当时机器是不是卡住了"这个问题
永远问不出答案。所以先定三件事：时间戳是谁的、缺的时候留什么、读的时候从哪一层取。

1. **时间戳是样本自己的采集时刻，向下对齐到桶边界，存成 INTEGER Unix 秒。** 快照里
   每一段（CPU/负载/内存/网卡/容量）都有自己的 `collected_at`，用它而不是请求时刻：
   采集循环卡住时，值的时间比请求时间旧，历史必须记那个旧时间。落库时 `alignUnix(TS, 桶宽)`
   向下对齐，主键 `(scope,target_id,metric,resolution,ts)` 复合 + `WITHOUT ROWID`，
   同一份快照重写不产生第二行（`INSERT OR IGNORE`）（Q399）。
2. **读哪一层由"时间跨度 × 实际保留窗口"决定，不是由步长决定。** 初版按步长从细到粗试，
   谁先塞进 1440 点就用谁——那是错的：保留窗口被配短了，raw 里根本没有三天前的点，
   按步长选层会选中一个空层，页面得到"过去三天什么都没发生"。现在的规则是跨度落在哪个
   窗口里就读哪一层（raw ≤24h / minute ≤30d / hour ≤365d），步长再在这个层上按
   1440 点上限加粗并向上对齐层宽，响应回报 `resolution`/`step_seconds`/`max_points`（Q400）。
3. **2 秒轮询代理快照、按 `collected_at` 去重、缺口就是没有行。** 轮询比代理的 5 秒采集
   快一档，是为了不踩点（踩上就丢一整份快照）；去重靠 `lastSummaryAt` 与库层主键两道。
   段状态不是 `ok`、值为 `null`、代理连不上——三种情况都不写行，`missing_points` 如实计数，
   永不补零（Q401）。
4. **聚合窗口=源粒度的保留窗口，裁剪点留两个桶宽，先聚合后裁剪。** 先裁后卷会让最老的
   那个桶永远卷不上，所以顺序固定；`PendingMetricBuckets` 只认"源有数据、目标还没有行"的
   桶，重启、崩溃、时钟回拨都能自愈补算（200 一批 / 每轮 4000 桶）（Q402）。
5. **差分指标在进程重启后的第一份快照里必然缺失。** CPU 使用率与网卡速率是相邻两次
   `/proc` 采样的差，代理重启后没有"上一次"，所以那四个 `load1/load5/load15/
   memory_used_percent` 有四行、`cpu_usage_percent` 一行没有——这是正确行为，不是丢数据。
   这条在验收里被反过来当过两次证据（见 Q405）。
6. **状态接口只说本进程知道的事。** `GET /system/metrics/status` 的 `inserted/aggregated/
   pruned` 是这个进程启动以来的计数，行数是查出来的；`collecting` 与 `enabled` 分开报，
   采集关掉时 `reason` 说明是配置关的而不是"代理坏了"。

产品面：`internal/metrics/{recorder,series,query,reader}.go`、`internal/store/metricstore.go`、
`migrations/0011_metric_samples.sql`、`internal/api/metrics.go`、
`cmd/upanel-web/main.go`（装配采集循环）。契约 §13.1 新增"P13.1 已实现范围"、§3 补
`/system/metrics/status`、§14.1 重写 `metric_samples` 行。

**真机抓出并修掉 1 个产品缺陷**：Q404——加权平均的浮点误差（一整桶样本值全等于 1.9 时
`SUM(1.9*12)/SUM(12)` 算出 `1.8999999999999997`）撞上表上的
`CHECK (min_value <= value AND value <= max_value)`，整个小时桶写不进去、每轮重试都失败，
那一小时**所有**指标的 hour 行永久缺失。修法是把平均值夹回 `[MIN(min_value), MAX(max_value)]`
（一组数的加权平均必然落在区间内，落到区间外的只可能是浮点误差，所以这是还原事实、不是
放宽不变量），聚合与查询两处都夹，并留下用 1.9 / 14.2 复现的回归测试。
**验收方法修正 4 条**：Q405 缺口判据窗口的右端也要让一个桶宽（重启后第一份快照的
`collected_at` 落桶后会掉到 `GAP_END` 之前，被误判成"停机期间仍在写"）；Q406 换行符与
执行位属于构建输入（`check.sh` 加第 14 关 `line-endings`，`scp` 的二进制要 `chmod 0755`）；
Q407 三条"看着在验、其实什么都没证"的断言（恒真的 socket 断言、按粒度而不是按 scope 读
series、关闭采集后的基准取样点在 `stop_web` 之前）。

**结论：P13.1 passed。** 两台 VM（24.04 的 systemd 255 / Python 3.12.3 / sqlite 3.45.1、
22.04 的 systemd 249 / Python 3.10.12 / sqlite 3.37.2）各 **PASS 88 / FAIL 0 / NOT RUN 0**，
退出码 0；逐节断言数（6/5/22/9/10/23/8/5）与归一化后的断言名单跨机逐字一致（`diff` 无输出）；
同一份脚本（md5 `d08964c5385a5156308386001c2190bf`）与同一组二进制（web `c99f56c3…`、
agent `b469732d…`、ctl `5d4a1bbf…`）。宿主 `check.sh` **PASS 14 / FAIL 0 / NOT RUN 0**
（按 `.nvmrc` 用 `H:\Code\.toolchains\node-v22.23.3-win-x64`）。
剩余 NOT RUN：前端历史图表（属 P13.4，本子任务只交付后端端点与存储）、`go test -race`
（宿主无 gcc，与 P12.5 同一理由）、"365 天真实老化"没有等真实时间（验收把三层保留窗口压到
3m/2h/4h 验证等价行为；50 万行的行数上限裁剪路径只有单测覆盖，真机没跑到那个量）。
完整结果见 `docs/evidence/P13/2026-10-26-p13.1/result.md`。

### P13.2（2026-10-26）— 告警：难的不是"超了就叫"，是"凭什么说它一直在超"

**这一个子任务真正要回答的是三个问题，代码量反而是最小的一部分。** 第一，什么叫"持续"——
区间平均会把"冲高 30 秒又落回来"抹平，而那正是最需要看见的东西，所以判据是窗口
`(now - duration, now]` 内**每一个**样本都在线外，有一个落回线内就掉回 `ok`。第二，什么叫
"好了"——`recovery_threshold` 与 `threshold` 之间那条死区就是防刷屏的全部机制，恢复同样要
持续 `duration_seconds`，跨不过死区就不算恢复。第三，什么叫"不知道"——窗口里没有样本、
最新样本超过容差没更新、窗口前段还没攒够，这三种情况状态机**原地不动**，只把原因写进
`last_reason`；采集停掉十分钟就自动"恢复"一次的告警，比没有告警更坏。已 firing 的规则遇到
数据中断继续开着，"不知道"不能得出"已经好了"（P13.1 定下的"缺就是没有行、永不补零"这条
口径，在告警这一层第二次被当成判据用）。

1. **三态状态机落在规则行上，CAS 加版本。** `ok --超限--> pending --超限持续够--> firing
   --恢复持续够--> ok`，`WHERE id=? AND version=? AND state=?`。数据不足时状态不变，因此
   没有第四态；"unknown"不是状态，是这一轮没做判断。评估每 15 秒一轮，`pending` 的持续判断
   因此从来不是一次算出来的（这一条直接生出了 Q417）。
2. **一个事件周期只有一行。** 触发 INSERT `firing`，恢复原地 UPDATE 成 `resolved` 并写上
   `resolved_at`、实际 `duration_seconds` 与 `peak_value`（按 `[started_at, resolved_at]`
   重算的极值，不是触发那一刻的值）。"恢复只发一次、未变化不刷屏"是**结构**保证：
   `UNIQUE INDEX idx_alert_events_open (rule_id) WHERE state='firing'`。规则删除时事件
   不级联删除（`rule_id` 是 `ON DELETE SET NULL`，变 0），快照字段照旧，历史仍然说得清
   "当时是什么条件"；删规则或停用前先把打开的事件收口，否则那行会变成永远 firing 的孤儿。
3. **双口径是对偶的。** `any`：任一条件满足即超限，恢复要求两个都回到安全侧；`all`：全部
   满足才超限，恢复只要一个回到安全侧。验收在同一份真实容量数据上同时验了这两面——
   `any` 触发、`all` 不触发，一条判据证两面。
4. **维护静默只压通知，不压判定。** 落在静默窗口内的事件记 `silenced=1`，事件照记、状态机
   照跑。机器确实超限了，这是事实，不该因为"正在维护"就假装没发生。
5. **可空字段回 `null` 而不是空串。** 这一条看着像文档活，实际是客户端语义：`new Date("")`
   是非法时间，`x == null` 把空串判成"有值"，于是"这条事件恢复了没有"只能靠 `state` 猜
   （Q418）。

产品面：`internal/alerts/{alerts,evaluator,store}.go`、`internal/api/alerts.go`、
`internal/metrics/cadence.go`（`WriteCadence`）、`migrations/0012_alerts.sql`。契约 §13.2
新增"判定口径"八条、事件形状与去重、`silenced` 的边界、审计动作清单；`api/openapi.yaml`
的 `AlertRule`/`AlertEvent`/`AlertSilence` 改成 OpenAPI 3.1 的可空写法。前端页面属 P13.4，
通知渠道属 P13.3——本子任务只交付后端端点与存储，`silenced` 现在**只标记、不发送**，
因为还没有发送方。

**真机抓出并修掉 3 个产品缺陷**：Q414——覆盖率容差与 `duration_seconds` 地板按写死的 5 秒
判，而容量序列走 `storage` 的 30 秒缓存，结果一条空间规则的表象是"配好了永远不响"、
`last_reason` 却写"窗口内没有任何样本"（这会让 PLAN §18 要的空间/inode 告警静默失效，
且不在任何检查里报错）；修法是 `metrics.WriteCadence` 成为唯一答案，新鲜度
`max(3×节奏, 20s)`、窗口前段余量 `max(2×节奏, 10s)`、`duration_seconds ≥ 2×节奏`。
Q417——`last_reason` 只在"进入 pending"那一轮写，后几轮落进"没迁移"的默认分支被覆盖成空串，
**最需要解释的那一刻恰好什么都不说**，而这个症状与 Q414 长得一样、会把排查带偏；修法是从此
每一轮都写进度（`已超限，正在计持续（还需 12s）`）。Q418——契约说可空、实现回空串，
4 处 `*string` 修正加 3 个按**原始 JSON** 判 null 的回归测试。

**验收方法修正 6 条**：Q415 整机 fd 总数是环境前置条件（24.04 上 `systemd-logind` 泄漏
7477 个 fd，`storage.topology` 从 0.027 秒变成 25—41 秒，容量序列一个点也落不下来，25 条
连锁失败；顺带解开了上一轮"agent 103% CPU"之谜）；Q416 查询辅助函数不许吞掉 SQL 错误
（列名写成 `details`、真名 `redacted_details`，错误被 `2>/dev/null` 吞掉后与"查出来是空"
长得一模一样）；Q419 三处判据修正（断言照着错的实现写、`R1_T0` 取晚 13 秒、把跨两次读的
断言当原子事实——改成一个读事务的 `pending_probe`，另加一条不受取样时刻影响的强判据：
事件的 `started_at` 必须比 `state_since` 至少晚 `duration_seconds`）；Q420 `$(( ))` 里塞
命令替换等于给脚本埋地雷（一个解不出来的 Unix 秒整数就把整轮验收崩掉，红都不红）。

**24.04 跑了六轮，每一轮红的性质都不一样**：117/25（环境病理，Q415）→ 139/3（脚本 bug，
Q416）→ **142/0**（第一次全绿）→ 145/2（Q418 修完后一条断言反过来红 + 一处竞态，Q419）→
崩在 §3.1、exit 2（脚本 bug，Q420）→ **148/0**。这不是同一件事重复六遍，而是四轮各自撞到
不同层面的问题；22.04 跑了二轮（142/0 → 148/0）。第 1 轮的完整日志被后续轮次覆盖了，
只留失败名单原文，已在 result.md 里如实注明——不把已经丢掉的日志假装还留着。

**结论：P13.2 passed。** 两台 VM 各 **PASS 148 / FAIL 0 / NOT RUN 0**，退出码 0；逐节断言数
（7/9/48/63/9/8/4）与归一化后的断言名单跨机逐字一致（`diff` 无输出）；同一份脚本
（md5 `c74dd469e47a7c17aad05e94b628d930`）与同一组二进制（web `082752b3…`、agent
`1610950d…`、ctl `e5e4ea6d…`）。宿主 `check.sh` **PASS 14 / FAIL 0 / NOT RUN 0**——这一轮
它的意义在 `openapi-lint` 那关：Q418 那条契约漂移就是它挡住的（修之前 13/1，7 条
`error struct: 7`），改成 OpenAPI 3.1 写法并通过 DTO 修正之后回到 valid + 6 warnings。
剩余 NOT RUN：通知渠道（属 P13.3，`silenced` 目前没有任何发送方）、告警桌面与
`POST /alerts/events/:id/ack`（属 P13.4，列已备好）、`go test -race`（宿主无 gcc）、
"真把磁盘写满"这类破坏性场景没有在共享验收机上做。
完整结果见 `docs/evidence/P13/2026-10-26-p13.2/result.md`。


### P13.3（2026-10-26）— 通知：谁去发这封信，以及"发不出去"该怎么记账

**这一个子任务真正的难点不在 SMTP 也不在 HTTPS，而在两个边界问题上。** 第一，**谁持有明文**：
SMTP 口令与 webhook 令牌存在代理的加密库里，让面板去发就得给面板一个"把 `secret_ref` 解成
明文"的接口——那是 CONTRACTS 从 P02.4 起就明确拒绝开的洞，而一旦开了，流出去的不只是 SMTP
口令，而是**所有**用途的秘密。所以发送放进代理，明文从头到尾没有离开持有它的进程，面板只
知道"发到哪、说什么"；代价是代理多了一项主动出网的能力，这是真实的风险增量，不靠"反正只有
Web 账号能调"糊过去。第二，**"投递失败"是什么**：它不是接口失败，所以投递路由**一律回 200**，
结果放在 `data` 里（`accepted`/`status`/`detail`/`retryable`），失败原因是对端原话
（"550 5.1.1 user unknown"）而不是我们猜的"发送失败"；`retryable` 也只由代理判——只有它
知道那是一次 SMTP 451 临时拒绝、一次 5xx 永久拒绝，还是一次超时，面板再猜一次就是第二套
与事实无关的规则。

1. **秘密用途由代理自己核对。** `secret_ref` 必须指向用途为 `notification_password` 的秘密，
   不对就 400。不核对的话，面板只要把备份仓库口令的引用填进一条自己控制的 SMTP 通道，代理
   就会把仓库口令当 AUTH 口令发到攻击者的服务器上——"不能读明文"被绕成"把明文发给我"，
   正好绕过了第①条的全部意义。用途不对与秘密不存在回同一个 400，不区分，免得变成探测用途
   的接口。
2. **代理侧的硬限制不是"配置项"。** webhook 只 https、拒 URL 里的用户名口令与 `#片段`、
   **不跟随重定向**（跟随等于让对端把告警正文引到我们没配过的地址上）、响应只读 4 KiB；
   SMTP 只 `starttls`/`implicit`（**没有明文模式**）、TLS 最低 1.2、AUTH 只 PLAIN；单次投递
   总时长 **8 秒**，比面板侧 10 秒的调用上限更短，免得面板先把代理掐掉、留下"不知道发没发
   出去"的一行。
3. **入队与事件同一笔事务。** 投递入队走 `alerts.Options{Notifier}`，在 `applyOutcome` 的
   同一笔事务里执行；否则中间崩一次就留下"事件在库里、谁也不知道它发生过"，而告警最不能
   容忍的正是"发生了但没人知道"。静默事件一条都不入队（"维护静默只压通知"在这层才真的
   成立），`(event_id, channel_id, kind)` 部分唯一索引把去重变成结构保证。
4. **退避与上限。** `30s × 2^(n-1)`、`maxRetryDelay = 10m` 封顶（不封顶的话 `max_attempts=10`
   的通道最后一次尝试会排到几小时之后，那时人早就以为它在正常工作了）；`max_attempts` 建在
   通道上（默认 3、上限 10），因为"重试几次"是"这条通道有多重要"的一部分。
5. **判死要分清三种。** 对端永久拒绝 → `failed` 且带对端原话；次数用尽 → `failed` 且
   `attempts == max_attempts`；**通道被停用或删除** → `failed`，但 `attempts` **一次都不加**
   （一次都没发出去的东西不该看起来像试过），原因直接写"通道已停用，未发送"。
6. **保存配置不发送。** 建好/改好一条通道之后，外面一个请求都不该收到、投递账 0 行——"保存
   一次就往外面喷一封邮件"是取消不掉的行为。要发就显式点测试发送，那是唯一一次单发。

产品面：`internal/notifications/{notifications,channels,deliveries,dispatch,agentsender}.go`、
`internal/agent/notify.go`、`internal/api/notifications.go`、
`migrations/0013_notification_channels.sql`。契约 §13.3 新增"回执形状"、"投递账"列语义、
硬限制清单与一条**已知限制**（审计红字化按**键名片段**匹配，`secret_ref` 命中 `secret` 也被
抹成 `[redacted]`，于是"凭据从 A 换成 B"在审计里看不出来——Q423，不在 P13.3 顺手改，
因为那会动到 P08 共享、P12 备份已有的审计语义）。投递账的**读接口**属 P13.4。

**真机验收抓到 0 个产品缺陷，抓到 3 个验收侧问题。** 这不等于"产品一次就对了"，只等于这一轮
红的地方都在脚本这一侧，而且其中一条差点把产品判成错：

- **Q424 接收器的 STARTTLS 在 Python 3.12 上必挂。** 首轮 27 条红里绝大多数是同一件事的连锁：
  代理调 SMTP，`net/smtp` 卡在 STARTTLS 满 8 秒报 `use of closed network connection`，而
  `openssl s_client -starttls smtp` 对同一个接收器**全程畅通**——两个客户端一个过一个不过，
  最容易得出"产品的 SMTP 客户端坏了"。真因：`loop.start_tls` 的第二个参数决定握手解密后的
  应用数据交给谁，3.12 上对端握手后的第一句 `EHLO` 会在 `await` 返回前几毫秒抵达、被当成
  升级前的明文丢掉。定位靠三步：Go 探针复现 → 接收器打出 `dropped-while-paused
  hex=45484c4f…`（正是明文 `EHLO`）→ `tcpdump -i lo -A` 证明线上发的是 ClientHello。修法
  是加一层 `TLSBridge`（握手期间只进缓冲、返回后重放），并显式 `server_side=True`、接管新
  transport、丢掉升级前的旧缓冲。
- **Q425 退避不能用"我们到达时还剩多少秒"来量。** 第 3 轮 `此刻还剩 19 秒` 判红，产品一秒
  不差：`next = now.Add(retryDelay(attempt))` 里的 `now` 取自 `DispatchOnce` 开头，而调度节拍
  `dispatchInterval = 5s`，投递行入队即到期，所以首次尝试一定落在入队后的第一个节拍上——
  `next_attempt_at - created_at ∈ [30, 35]`，区间宽度就是节拍本身。改成账本内两个时刻相减，
  并把这段推导连同三个常量写进注释；两台机器实测 35 / 31，正好是节拍相位的两端。
- **Q426 空版本号拼进 PATCH 的 body，把产品合法的 400 读成了"停用没生效"。** 第 3 轮那两条红
  （`failed|2` + 原因 `boom`）看上去像产品把"停用判死"做成了"用尽上限判死"，还互相印证；
  真因是 `chan_version` 多传了一个参数，版本号取成空串、请求体拼成
  `{"enabled":false,"version":}`，产品回 `VALIDATION_ERROR`（audit 58、`stage=validation`），
  停用从未发生。修法：调用改对，且这一步自己有四条断言、非数字版本号直接不发——**空值不再
  能穿过断言链**；没赶上停用窗口时如实报"没赶上"，不再把"用尽上限"说成"停用判死"。

**24.04 跑了六轮**：153/27（Q424）→ 203/5（种入权限、断言读错文件、超时措辞过窄）→
204/3（Q426 + Q425）→ 210/1（Q425 的后半段）→ **211/0**（旧二进制）→ **211/0**（从当前
工作树重新构建的二进制）。第 6 轮重新构建，是因为第 5 轮全绿时 `/tmp/upanel-bin` 里那批
二进制的来源无法从仓库证明；第 5 轮仍留在履历里，不因为它是旧二进制就抹掉。22.04 一轮
211/0，与第 6 轮同一份脚本、同一组二进制。

**结论：P13.3 passed。** 两台 VM 各 **PASS 211 / FAIL 0 / NOT RUN 0**，退出码 0；逐节断言数
（13/11/48/12/44/34/35/14）与归一化后的断言名单跨机逐字一致（`diff` 只有两行不同，差别在
括号里量到的秒数，且两行都是 PASS）；同一份脚本（md5 `f8f6984d0f5e302970b161899a90ff28`）
与同一组二进制（agent `f7acfd27…`、web `8c8b258d…`、ctl `45fb01be…`）。宿主 `check.sh`
**PASS 14 / FAIL 0 / NOT RUN 0**。剩余 NOT RUN：真实第三方 SMTP 与公网 webhook、
`30s→…→10m` 封顶只在单测、投递账读接口属 P13.4、多收件人/大正文、`retryable` 字段形状只有
单测、`go test -race`（宿主无 gcc）。完整结果见
`docs/evidence/P13/2026-10-26-p13.3/result.md`。

### P13.4（2026-10-26）— 确认与陈旧：把"谁知道了"和"系统好了"分成两件事

**这一层的难点不是画图，是让面板"别说谎"。** 三条边界各自都能悄悄说错话：把"有人点了知道了"
说成"故障已恢复"；把"这段时间没有数据"画成"这段时间是 0"；把"配置里关掉了采集"说成"没有
数据"，或者反过来把"这个进程没有库"说成"采集关掉了"。

1. **确认不是恢复。** `POST /alerts/events/:id/ack` 只写 `acked_at`/`acked_by`，**不碰状态机**：
   确认之后事件仍是 `firing`、`resolved_at` 仍是空，恢复由评估循环按 `recovery_threshold` 判。
   这是有意的——"有人看到了"和"机器好了"是两种事实，合并成一个字段，人就再也没法知道那条告警
   是被人按掉的还是自己好的。幂等与首告者胜由 SQL 保证（`WHERE id=? AND acked_at IS NULL`），
   第二次确认原样返回第一次的时间与人；确认人取自**会话**而不是请求体里的名字（否则谁都能替
   别人签收）；admin 才能确认，viewer 能看事件但不能认领；已恢复的事件也能确认（"这条你知道
   了吗"与"它现在什么状态"无关）。Q427。
2. **投递账的读接口是顶层 `GET /notification-deliveries`。** 挂在 `/notification-channels/:id/`
   下读起来更 REST，但它会把一行投递说成"这个通道的属性"，而事实相反：一行账带着的是**投递
   当时**的通道快照，通道改名、停用、删除都不该改写历史。更实际的理由是收件箱要回答"最近发
   出去过什么"——那是跨通道的排序，挂通道下就得先列通道再逐个查再合并，一次查询变 N+1。
   行的形状与通道端点里的投递回执**共用同一个 DTO**。`limit` 超过 200 按 200 截断而不报错
   （读账读不出危险，截断比 400 对调用方更友好），非正整数才 400。Q428。
3. **缺失不是 0。** `metric_samples` 的缺口**没有行**，不是写了 `value=0`；`missing_points` 是
   缺口存在过的唯一证据；前端按 `step × 3` 切段，缺口两侧画成两条线。有一个容易读错的正常
   产物：minute/hour **聚合桶可以落进缺口区间**——聚合覆盖的是整分钟，缺口只占它的一段，那是
   聚合的正常产物，不是回填。契约里写清这一点，是因为验收自己就被它骗过一次（Q431 第 5 条）。
4. **"陈旧"与"没开"分开说。** `/system/metrics/status` 的 `enabled` 说的是**这个进程答不答得了
   历史问题**（库在就是 true），`collecting` 才是采集循环在不在这个进程里跑；关掉采集是
   `enabled=true + collecting=false + reason`（"配置里关闭了历史采集
   （`web.metrics.enabled=false`）：旧数据可查，不再写新数据"），此 `enabled=false` 只剩一种
   来路：没有库（演示模式/未装配）。三态判据：未开启 / 没在采集 / 陈旧
   （`collecting=true` 且 `last_sample_at` 距此刻超过 `max(3×interval, 20s)`）。Q430。

**验收侧撞到并修掉的问题，全部在脚本这一侧（产品缺陷 0 个）：** Q429（20 秒新鲜度判据拿宿主
`datetime.now()` 去量，24.04 走 TCG 单线程时从取回状态到算出结果过了十几秒，把新鲜样本量成
陈旧；改成同一份响应里 `generated_at - last_sample_at`，两台实测 6 秒 / 1 秒）、Q430（把
`enabled` 与 `collecting` 当成一个意思，第 8 节一次红 4 条；改断言与契约，产品一行没动）、
Q431（目录 0700 把 `upanel-web` 挡在门外、请求体塞了契约之外的 `scope`、`COUNT(*) ... IS NULL`
期望写反、缺口窗口右端取未来时刻被自己的种入填上、SQL 漏了 `metric`/`resolution='raw'` 把聚合
行读成回填）。外加一次判据拆分：缺口"缺几桶"用定型窗口（右端 = 缺口结束时刻），"图上连不连"
用延伸到缺口之后 30 秒的窗口——同一个窗口问不了两个问题。

**24.04 跑了两轮**：127/1（Q429）→ **128/0**。**22.04 跑了两轮**：128/0（脚本 `2ce471ee…`）→
**128/0**（脚本 `f86fb06f…`）。22.04 的第一次全绿不算最终成绩——脚本改过就必须两台重跑，
"同一份脚本、同一组二进制"是跨机可比的前提。

**结论：P13.4 passed。** 两台 VM 各 **PASS 128 / FAIL 0 / NOT RUN 0**，退出码 0；逐节断言数
（7/9/14/14/20/31/12/8/10/3）与归一化后的断言名单跨机逐字一致（`diff` 无输出）；同一份脚本
（sha256 `f86fb06f…`）与同一组二进制（agent `0d128952…`、web `cef620a7…`、ctl `c66d2c53…`）。
宿主 `check.sh` **PASS 14 / FAIL 0 / NOT RUN 0**（go 46 个包、web 52 个文件 985 个用例、
web-smoke 10 项、openapi valid 6 warnings）。剩余 NOT RUN：浏览器里的真实交互（VM 无
Playwright，由 SSR 渲染测试与 `web-smoke` 覆盖）、确认之后要不要发一封通知（契约未定义，不在
本子任务）、`go test -race`（宿主无 gcc）、真实业务负载下逼近
`max(3×interval, 20s)` 这条线、长时间运行下的聚合与裁剪（沿用 P13.1 的缩短窗口，未重验）。
完整结果见 `docs/evidence/P13/2026-10-26-p13.4/result.md`。

### P14.1（2026-10-26）— 网络现状：先如实说出"现在是什么样"，再谈"能不能改"

**范围**：`GET /network/interfaces`、`GET /network/routes`、`GET /network/listeners`、
`GET /firewall` 四条只读端点（全部 admin，全部无副作用），以及给 P14.2/P14.3 用的两份判定：
`capability`（这份 netplan 现状能不能被面板安全地改）与 `editable`（防火墙是不是 ufw 独占）。
**本轮不开任何写入口**——POST 同路径与 `/network/apply` 在真机上必须 404/405，这条本身就是
验收项。

**三条贯穿口径**（写进 CONTRACTS §13.5）：

- **查不到不等于没有**：每个来源都可能缺席，缺席时用 `Issue{source,code,detail}` 说出来，
  `code` 是闭集 `missing`/`permission_denied`/`timeout`/`unparsable`/`failed`。`ip` 整条不可用时
  仍从 sysfs 列出网卡（名字、MTU、MAC、载波、物理性都在，只是没有地址）并另记 `ip/missing`。
  空数组和"读不到"在页面上长得一样，但一个是"这台机器没有网卡"，一个是"取数工具丢了"——
  把后者画成前者，等于让人拿错误的现状去做决定。
- **不猜**：网卡类型看 sysfs（有 `bridge` 目录才是网桥，`br0` 只是名字），物理性看
  `device` 目录 + `type 1` + 没有 `wireless`/`phy80211`/`bonding`/VLAN 标记，配置归属看
  `/run/systemd/network/` 与 `/run/NetworkManager/` 下的**渲染产物**。**不解析 netplan YAML**：
  解析它意味着把"我以为的 netplan 语法"当成事实，而真正生效的是渲染结果。
- **只读也不许慢**：一次采集总预算 8 秒、单条命令 6 秒。超时如实记 `timeout` 并让其余来源
  照常产出——部分真相加一条说明，比一个不返回的请求有用。

**`capability` 判定**：`backend` 闭集 `netplan-networkd`/`netplan-networkmanager`/
`networkmanager`/`other`/`unknown`；`editable=true` 只在 `netplan-networkd` + 无 Wi-Fi/网桥/
bond/VLAN + 物理以太网卡恰好一张 + `/run/NetworkManager` 不存在时成立，`false` 必带
`reasons`（人话，能贴到页面上），`unknown` 与 `none` 不合并——"不知道"和"没有"是两件事。

**防火墙管理者证据链**：firewalld = 程序在 或 `/run/firewalld` 在；nftables = `/etc/nftables.conf`
存在**且** `*.wants/nftables.service` 被 enable；iptables-persistent = `/etc/ufw` 之外另有
`/etc/iptables/rules.v{4,6}`。`editable=true` 只在 ufw 独占时给出。

**审计口径**：只记**被拒的读**。`auditRoute` 只是把动作名放进 context，`s.reject` 才落库；
成功读不写审计——否则一次页面刷新就是四条记录，把审计表灌成访问日志（与 §9 收藏不写审计
同源）。验收断言因此是"failure ≥ 4 且 success == 0"，不是"有审计行"。

**交付**：

- 代理侧：`internal/network/`（`deps.go` 超时与 `Issue` 合成、`interfaces.go` 的
  `detectKind`/`enrichFromSysfs`/`readType`、`routes.go`、`listeners.go`（读
  `/proc/net/{tcp,tcp6,udp,udp6}`，inode 只用于反查、出口前清零、反查不到 `pid=0` 且
  `process` 为空）、`firewall.go` 的 `nftablesManaged`/`parseUFWOutput`、`capability.go` 的
  `rendererArtifacts`/`netplanConfigs`/`detectCapability`、`collector.go` 组装）；四个查询在
  `internal/agent/networkquery.go` 登记（`network.interfaces`/`network.routes`/
  `network.listeners`/`firewall.state`，全部无参数），**未接线时 503 而不是空表**。
- Web 侧：`internal/api/network.go` 四个 handler + `internal/agentclient/network.go` 四个客户端
  方法，全部走 `auditRoute` + admin 门；`api/openapi.yaml` 补齐四条路径与 `capability`/
  `firewall` 两个对象；前端 `web/src/modules/network*` 与页面卡片（数据来自这四条，页面渲染
  本身由 SSR 渲染测试覆盖）。
- 单测 22 个顶层用例在 `internal/network/network_test.go`（含 `ip` 缺失回退、`ufw` 未安装不是
  `none`、网桥阻断、多物理卡阻断、NetworkManager 在跑阻断、netplan 读不到判 `unknown`、
  `/proc/net` 不可读、监听表截断要自报）。

**真机验收**（`tests/integration/p14-1-acceptance.sh`，约 800 行，root 在 VM 内跑）：

- 每条关键断言都配一条**现场独立读数**：逐卡对照 sysfs（physical/kind/mtu/carrier 三态）、
  路由的现场默认网关与出接口、监听集合与 `/proc/net` **双向**比对（响应多一条少一条都红）、
  防火墙与 `ufw status verbose|numbered` 对照、`capability` 在脚本里用现场事实**独立重算一遍**
  再比对（不是拿产品的答案验产品）。
- 只读的证明方式：`/etc/netplan`、`/etc/ufw`、`/etc/systemd/network` 三处的**文件清单 + 每个
  文件 sha256** 在四条请求前后逐字节比对。刻意**不**比 `ip addr` 的输出——DHCP 租约的
  `valid_lft` 每秒都在变，那种红是尺子在动，不是产品在写。
- **两台走到判定的两个分支**：22.04 `backend=netplan-networkd, editable=true, target=ens4`；
  24.04 同样的 backend，但 P09 容器验收留下的 `docker0` 网桥让 `editable=false`，`reasons` 点名
  "本机存在网桥接口（docker0）"。绿色按钮与"灰掉并给出原因"两条路径都是真机验的。
- 成绩：22.04 **PASS 112 / FAIL 0 / NOT RUN 0**（7/7/5/14/20/7/10/13/9/16/4），24.04
  **PASS 116 / FAIL 0 / NOT RUN 0**（7/7/5/14/25/6/10/13/9/16/4），退出码均 0，同一份脚本
  （sha256 `147b7a93…`）、同一组二进制（agent `92795acb…`、web `252a7ed6…`、ctl `6841cb3c…`）。

**真机撞到并修掉的问题**：

1. **Q438 产品缺陷**：主机路由的 `destination` 不带前缀（`ip -j route` 对网段给 `10.0.2.0/24`、
   对主机路由只给 `10.0.2.2`），同一个字段两种写法，前端每处都得写两条分支、写漏一处就是
   静默的错误比较。解析时补齐 `/32`、`/128`，单测 `TestRoutesNormalizeHostRoutesToPrefix` 钉住。
2. **Q439 产品缺陷**：`/etc/nftables.conf` 每台 Ubuntu 都带（nftables 包的默认规则集），第一版
   把"有文件"当成"nftables 在管"→ `manager=unknown` → `editable=false` → **P14.3 在一台最标准
   的 Ubuntu 上永远打不开**。判据改成"它会不会在开机时把规则装进内核"：有 conf **且**
   `*.wants/nftables.service` 在。单测两侧各一条，已知局限（手工 `nft -f` 载入没 enable 的
   机器看不到证据）写进契约。
3. **验收方法 6 条**：Q440 `$WEB_PID` 是 `runuser` 的壳（改读 `children`，并加一条不依赖脚本
   记账的断言——报出的 pid 的 `/proc/<pid>/comm` 必须是 `upanel-web`）；Q441 回环是
   127.0.0.0/8（`127.0.0.53:53` 是 systemd-resolved，原断言只放行 `127.0.0.1`）；Q442 比结构
   一律走 `json.dumps`（`str(list)` 的引号差异一路假红）；Q443 别用 `head -c 400` 找键（长响应
   前 400 字节只有网卡数组，且这个错误会随数据量变化）；Q444 读不到时的 `0 == 0` 是**假绿**
   （24.04 上 `ufw status` 撞 6 秒上限，产品如实记 `ufw/timeout`，脚本却把"没读到"验成"没有
   规则"——改成重读一次，仍读不到记 NOT RUN）；Q445 24.04 用 `ssh.socket` 由 PID 1 持有 22
   端口（断言从"应该叫 sshd"改成"报出的进程名与内核里那个 pid 的 comm 一致"）。

**NOT RUN（明确未验，不含糊）**：Wi-Fi/bond/VLAN/多物理卡/NetworkManager 在跑 这几条
`editable=false` 的**具体**原因分支、三种非 `netplan-networkd` 的 backend、
`manager=firewalld`/`nftables(enable)`/`iptables-persistent`/`none`、`ip` 与 `ufw` 缺失、
`issues.code` 里的 `permission_denied`/`unparsable`/`failed`——**全部只有单测**，真机上造这些
要写系统配置或删程序，属于破坏现场。页面在浏览器里的真实渲染 VM 内不可执行（无 Playwright），
由 SSR 渲染测试与 `web-smoke` 覆盖（Q21）。

**结论：P14.1 passed。** 完整结果见 `docs/evidence/P14/2026-10-26-p14.1/result.md`。

### P14.2（2026-10-26）— 改地址：先让机器自己能把网络退回来，再谈能不能改

**范围**：三个写动作 `network.apply`（单物理网卡 DHCP/静态 IPv4）、`network.confirm`（停回滚
timer、保留新配置）、`network.rollback`（立刻退回），加上入口状态 `GET /network/config`、
独立 systemd 回滚 timer、开机对账，以及前端那张改写卡片。**写路径默认关闭**，只认
`-network-writes` 显式开关。

**状态：passed（22.04 VM 第五轮，2026-10-26）。** 真机把 ens4 改成不可达的
`10.99.99.99/24`，停掉 Web 与代理，地址在 `deadline+28s` 自己回来了——**STAGES §16 的硬条件
因此解除**。开关仍然默认 `false`，但理由换了（Q458）：改网卡等于把机器从网络上摘掉，这个权力
是装机时由人决定的事（P16.1 的服务单元、P16.4 的手册），不该由一次验收顺手替所有既有部署
决定。开关的说明文字也按新理由改过——原来那句"仅用于测试 VM / 尚未验收"在第五轮之后是假的。

**执行顺序就是安全模型**（`internal/network/applyengine.go` 的 `write`）：

1. 渲染 `90-upanel-<iface>.yaml`（0600，原子替换；先写临时文件再 `Rename`）；
2. `netplan generate` ——只渲染不生效。语法/语义过不去就**把面板文件恢复原状、不做 apply**，
   现场一个字节没动；
3. 写回滚记录、**先武装** systemd timer，然后才 `netplan apply`。timer 装不上就**不许改**：
   一条改完之后没人能把它退回来的路，不该存在；
4. `netplan apply` 失败 → 立刻按记录回滚，不等 timer 到期；
5. **不信退出码**，读回内核事实（地址/前缀/网关/DHCP 拿到的地址）核验；不符 → 立刻回滚。

`netplan apply` 返回 0 但内核没换地址是真实存在的（改名、竞态、`networkd` 没重载），所以
第 5 步是判决，不是装饰。outcome 闭集 `verified`/`rolled_back`/`unknown`，**没有 `failed`**：
它确实**跑完了**，只是改动没留下——说成"失败"会让人以为机器现在坏了，说成"成功"会让人以为
新地址生效了，两句都不对。恢复也失败时是 `unknown`（"不知道现在什么样"），那时唯一正确的
下一步是去看现场，所以 `RecoveryHint` 直接写这句。

**`network.confirm` 的凭据不是 token，是"这次请求到达的地址"**：判据由服务端从连接里取
（`http.LocalAddrContextKey`），调用方传的 `arrived_on` 一律忽略（`internal/api/networkexpand.go`
在转发前注入，取不到连接地址就 500 且**不转发**）。归一化顺序是 `SplitHostPort` → 去 IPv6
zone → `netip.ParseAddr().Unmap()`，所以 `::ffff:192.168.1.50` 与 `192.168.1.50` 会落到同一个
字符串上——代理做的只是字符串相等比较，归一化漏一步，"从新地址确认"就会被判成从旧地址。
这条正是"旧地址不能绕过新地址验证"的实现方式。

**回滚记录是文件，不是库**：`/var/lib/upanel-agent/network/rollback/<token>.json`（root 0600，
`nrb_` + 32 hex）。理由在 CONTRACTS §14.2 里：开机对账发生在数据库就绪之前，而且这份记录要
在"面板进程自己被掐断"之后仍然存在——它属于 systemd 那一侧，不属于应用那一侧。

**交付**：

- 代理侧：`internal/network/applyengine.go`（`write`/`confirm`/`rollback`/`Reconcile`/
  `ExecutePendingRollback`、`capturePrevious`/`restorePanelFile`/`verifyApplied`、
  `armRollback`/`disarmRollback`）、`applytypes.go`（`RollbackRecord`/`ConfigState`/
  `RollbackView`/`RollbackWindow=120s`/`ValidRollbackToken`）、`netplaninspect.go`（只看
  `/etc/netplan` 与 `/run` 的渲染产物，**不解析别人的 YAML**）；`internal/agent/networkaction.go`
  登记三个写动作（`Policy` 只放 admin、`network.apply` 要 `RECONFIGURE XXXXXX` 短语）；
  `cmd/upanel-agent/main.go` 的 `-network-writes` 开关与开机 `Reconcile`。
- Web 侧：`internal/api/networkexpand.go`（`arrived_on` 注入）、`internal/api/network.go` 的
  `handleGetNetworkConfig`、`internal/agentclient/network.go` 的 `NetworkConfig`（缺
  `collected_at`/`capability.backend`，或"说可以改却没有 `config_hash`"都按残缺响应拒）、
  `api/openapi.yaml` 补 `/api/v1/network/config` 与 5 个 schema。
- 前端：`web/src/api/network.ts` 的写路径纯函数（`buildDefinition`/`buildApplyParams`/
  `rollbackSecondsLeft`/`countdownText`/`pendingHeadline`）、`preflights.ts` 的 `network.` 超时
  分档（30 秒，代理那一段预算是 20 秒，前端必须比它宽）、`jobresult.ts` 的
  `summarizeNetworkJob`（结论看 `outcome` 不看 `status`）、`NetworkApplyCard.vue`（倒计时只在
  actionable 时起 1 秒 ticker）、`NetworkView.vue` 的第五条并行查询（写路径失败**不进**页面顶部
  的"有一项没能读到"，它是另一件事）。

**检查（已执行）**：

- `go build ./...`、`go vet ./...` 干净；`internal/network` **39** 个顶层用例（其中
  `applyengine_test.go` **17** 个：写完先武装 timer 再 apply、generate 失败不动现场、核验不符立刻
  回滚、timer 装不上就不改、待确认记录挡住第二次写、`confirm` 要候选地址与窗口、开机对账
  过期即回滚且未过期按剩余时间重新武装、DHCP 保留归属并丢掉旧地址；**Q456 补的两条**锁住
  DHCP 新判据的两个方向——同名地址回来是 `verified`、另一个文件也在配这张网卡是 `rolled_back`）；
  `internal/agent`
  网络动作 13 个（判决原样过桥、兜底 blocker、三个 outcome、未知 outcome 报错）；
  `internal/api` 的 `networkexpand_test.go` 6 个（含 `::ffff:` 与 zone 归一化、"取不到连接
  地址就不转发"）+ 网络 handler 7 个。
- 前端：`npm --prefix web run test` **54 文件 1031 用例全过**（新增 32 条：写路径纯逻辑 12、
  `network.` 超时分档 2、`summarizeNetworkJob` 8、`NetworkApplyCard` 的 SSR 渲染 9、以及
  `/network/config` 的请求路径），`npm run typecheck`（vue-tsc）通过。
- 宿主 `scripts/check.sh` 在**最终工作树**上 **PASS 14 / FAIL 0 / NOT RUN 0**
  （`docs/evidence/P14/2026-10-26-p14.2/logs/host-check.txt`）：go 47 包 ok + 3 个
  no test files、gofmt 干净、Linux 交叉构建过、web 54 文件 1031 用例、`web-smoke` 10/0/0、
  `openapi-lint` valid + 6 warnings（与基线一致）。
- 第一轮 check 是 **FAIL 1**（`go-test`）：`internal/storage` 的
  `TestDeviceSuperblockProberCancelledContext` 红了。查下去是本轮无关但真实的偶发缺陷——
  `readAtTimeout` 对**已经取消**的 context 抛硬币（`ctx.Done()` 与读完成的 `done` 同时就绪时
  均匀随机）。修法是进 goroutine 之前先判 `ctx.Err()`。见 Q446。

**真机验收（22.04 VM，第五轮）：`PASS 186 / FAIL 0 / NOT RUN 3`，退出码 0。** 前四次都没绿，
原因逐条记在 `docs/evidence/P14/2026-10-26-p14.2/result.md` §1：一、二轮崩在"非回环监听必须
TLS"（面板绑新地址时起不来，后续断言全是连锁假红）；第三轮 7.4 按轮询次数收工、deadline 还没到
就采样（假红），同一轮抓到 DHCP 核验判据的真缺陷（Q456）；第四轮两条红是我问错了 systemd 249
的 journal——它不记 `ExecStart`、也不写 `status=0/SUCCESS`。

**"已验证"的部分**（每一条都有现场文件，见 `docs/evidence/P14/2026-10-26-p14.2/facts/`）：

- **不可达地址 → 停 Web 与代理 → 自己回来**：15:01:12 落记录（`deadline=15:03:12Z`）→
  15:01:16 timer 武装 → 15:01:39 停 Web/代理（此后**没有任何 upanel 进程**）→ 15:02:02 仍在
  不可达地址 → 15:03:32 systemd 拉起 `upanel-netrollback@<token>.service` → 15:03:41 退回完成、
  `Consumed 3.013s CPU time` → 地址在 `deadline+28s` 回到 `10.0.2.15/24 dynamic`。
- **旧地址不能绕过新地址验证**：从回环地址确认被 `CONFIRM_WRONG_ADDRESS` 拒（理由里点名到达
  地址与候选地址），被拒后 timer 仍在、记录仍 `pending`；从新地址确认 `outcome=verified`、
  记录转 `confirmed`、timer 停掉且单元文件删除；再确认一次是 `NO_PENDING_ROLLBACK`。
- **复杂 netplan 返回 `unsupported`**：加一份把 ens4 当成员的桥接配置 → `GET /network/config`
  仍 200 但 `unsupported` 点名那个文件、预检 `UNSUPPORTED`、提交 409 `DEPENDENCY_BLOCKED`
  且**没有落下任务**；删掉后恢复可改。
- **写路径默认关闭**：不带 `-network-writes` 时 `GET /network/config` 与三个写动作都是 400，
  带开关后才出现（Q452 的真机证明，阶段 2 共 12 条断言）。

**NOT RUN（明确未验，不含糊）**——脚本自己报 3 条，我另补 4 条：

1. 浏览器里的真实渲染与断连重连交互（VM 内无 Playwright，由 SSR 渲染测试与 `web-smoke`
   覆盖，Q21）。
2. 网卡被改名/拔掉（`NETWORK_TARGET_CHANGED` 的另一半）：改网卡名要重启，重启会把这次
   timer 的语义一起冲掉。
3. 回滚自己失败（`failed` 分支）：真机上造它等于把机器弄坏到看门狗之外，风险不对等；这条
   永远只能是"设计上可读"，不是"验证过"。
4. **`network.rollback` 的"立刻退回"真机路径**——只有单测。主动没做：阶段 7 已经把同一条恢复
   链路（`netplan apply` / `verifyRestored` / `markRecord` / `disarmRollback`）走过一遍，
   再加一轮只多一份重复证据。
5. **`Reconcile`（开机对账）在真机上没验**：阶段 8 只验了"没有待确认记录时的行为"，不是
   "有一条过期记录时它会退回去"。单测覆盖过期即回滚、未过期按剩余时间重新武装。
6. **24.04 上没跑 P14.2**：本轮全在 22.04.5（systemd 249）；24.04 是 255，timer/journal 行为
   有差异。双平台是 P16.2 的事。
7. **真实物理网卡 / 真实 DHCP 服务器没验**：slirp 的 DHCP 恒发 `10.0.2.15`，"租约换了一个
   地址"这条最常见的正常路径在真机上一次都没出现过，只有三条单测。

**已知限制**（写进 DECISIONS 而不是藏着）：Q457 —— 回滚 timer 用 `OnActiveSec=120s`，会被并发
的 `daemon-reload` 重新起算。第五轮就撞上了：deadline 15:03:12、timer 15:03:32 才触发。方向是
单一的（只会晚，不会提前咬掉窗口），开机 `Reconcile` 兜底；改法是 `OnCalendar=<deadline>`，
但 CONTRACTS §13.6 写死了 `OnActiveSec=120s`，第二轮再改契约。

**结论：P14.2 passed。** 代码已实现、单测与宿主检查已过、真机验收通过（186/0/3，退出码 0）。
写路径默认关闭，理由是 Q458 那条装机决策，不是"还没验"。

### P14.3（2026-10-27）— 改规则之前，先证明"改完你还进得来"

**范围**：三个写动作 `firewall.apply`（`{enabled, rules[]}`）、`firewall.confirm`（撤 timer、
保留新规则集）、`firewall.rollback`（立刻退回），加上入口状态 `GET /firewall/config`、独立
systemd 回滚 timer、开机对账，以及前端那张改写卡片。**写路径默认关闭**，只认
`-firewall-writes`——与 `-network-writes` 是**两个开关**：能把机器从网络上摘掉和能改它的规则
是两个不同量级的权力，绑在一起会让"只想让人改 ufw"的部署被迫也交出发改网卡的能力。

**状态：passed（22.04 VM 第三轮，2026-10-27）。** 153 条断言 0 失败；两条硬性用例都在真机上
成立：挡住管理通路被预检拦下、改完不确认时规则自己退回来（Web 与代理全程是停的）。

**设计：只加缺的、删多的，不整体重建。** 定义是 `{enabled, rules[]}`，规则的身份不是位置而是
comment ——每条面板规则都写成 `ufw allow 22/tcp comment 'upanel:ssh'`，`ufw show added` 打印的是
规范命令行，于是"这条是我管的吗"变成一个可读的事实而不是一次猜测。apply 做的事是：读现状 →
按 `upanel:` 前缀筛出自己管的那些 → 与定义比 → 加缺的、删多的 → 读回核对。**不整体重建**是
因为 `ufw --force reset` 会连管理员手写的规则一起清掉，而"我只想加一条 5432"不该有那个副作用。
阶段 6 专门验了这件事：先手写 `ufw allow 9000/tcp`（没有 comment），两次 apply 之后它还在，
`show added` 逐字节相同。指纹 `firewallConfigHash` 只算"启用状态 + 带标记的规则"，所以手写规则
不会让面板永远说"现场已被别人改过"。

**执行顺序就是安全模型**（`internal/network/firewallapply.go` 的 `write`）：

1. 按定义算出要加/要删的规则（纯计算，不动现场）；
2. 落回滚记录（root 0600），**先武装** systemd timer，然后才动 ufw。timer 装不上就**不许改**；
3. 逐条加/删，任何一步失败 → 立刻按记录回滚，不等 timer 到期；
4. **不信退出码**，重读 `ufw show added` 与启用状态核验；不符 → 立刻回滚。

outcome 闭集 `verified`/`rolled_back`/`unknown`，**没有 `failed`**——同 P14.2 的理由，与 §13.6 一致。

**管理通路的判据不是"22 端口有没有开"，是"启用状态下每个对外可达的管理端口都要有 `from any`
的 tcp 放行"。** 这条差别的现实意义：`ufw allow from 10.0.0.5 to any port 22` 看起来像放行了
22，但它只对那一个来源成立，管理员换台机器就进不去——面板不该把这种配置当成"你还能登录"。
判据实现里 SSH 优先按进程名 `sshd` 认（24.04 的 `ssh.socket` 让端口持有者是 systemd，见
Q445），认不出来退回 22/tcp；连管理端口都认不全时是 `FIREWALL_MANAGEMENT_UNKNOWN` 并**停下**
（不知道保护什么就不能声称保护了）。`FIREWALL_MANAGEMENT_PATH` 是**阻塞项**，不是提示。

**回滚记录是文件，不是库**：`/var/lib/upanel-agent/firewall/rollback/<token>.json`（root 0600，
`frb_` + 32 hex）。理由同 P14.2（CONTRACTS §14.2）：开机对账发生在数据库就绪之前，而且这份
记录要在"面板进程自己被掐断"之后仍然存在。

**交付**：

- 代理侧：`internal/network/firewallapply.go`（`Assess`/`Current`/`ExecuteWrite`/`Reconcile`/
  `ExecutePendingRollback`/`AssessConfirm`/`AssessRollback`、`armRollback`/`disarmRollback`）、
  `firewalltypes.go`（`ManagedRule.spec()`、9 个阻塞码、`firewallConfigHash`、
  `RollbackWindow=120s`、`frb_` token）、`internal/agent/firewallaction.go` 登记三个写动作
  （只放 admin、`RequiresReauth` + `RECONFIGURE XXXXXXXX` 短语，`Decode` 里先跑
  `ValidateFirewallDefinition`）、`cmd/upanel-agent/main.go` 的 `-firewall-writes` 开关、
  `cmd/upanel-agent/fwrollback.go`（`fwrollback` 子命令，在解析配置之前处理）与
  `upanel-fwrollback@.service` 模板。
- Web 侧：`internal/api` 的 `GET /firewall/config` 与三个写动作转发、`api/openapi.yaml` 的
  两个 schema 与新端点。
- 前端：`web/src/api/firewall.ts` 的写路径纯函数（`buildDefinition`/`buildApplyParams`/
  `describeFirewallChange`/`rollbackSecondsLeft`）、`FirewallApplyCard.vue`（倒计时只在
  actionable 时起 ticker）。

**检查（已执行）**：

- `go build ./...`、`gofmt -l`（空）、`go vet ./...` 干净；`internal/network` **58** 个顶层用例
  （`applyengine_test.go` 17 + `firewallapply_test.go` 19 + `network_test.go` 22），其中
  `firewallapply_test.go` 本轮新增两条回归：`TestFirewallAssessRefusalsKeepNormalizedParams`
  与 `TestFirewallTokenMismatchKeepsNormalizedParams`（Q464）；`internal/agent` 防火墙动作 9 个。
- 前端：`npm --prefix web run test` **56 文件 1065 用例全过**（新增 34 条：`firewall.test.ts` 25 +
  `firewall-write.test.ts` 9），`vue-tsc` 与 `web-lint` 通过。
- 宿主 `scripts/check.sh` 在**最终工作树**上 **PASS 14 / FAIL 0 / NOT RUN 0**（node v22.23.3）：
  go 47 包 ok + 3 个 no test files、gofmt 干净、Linux 交叉构建与 vet 过、web 56 文件 1065 用例、
  `web-smoke` 10/0/0、`openapi-lint` valid + 6 warnings（与基线一致）。

**真机验收（22.04 VM，第三轮）：`PASS 153 / FAIL 0 / NOT RUN 4`，退出码 0**，`19:06:48Z start`
→ `19:13:56Z done`。逐节断言数 12/3/16/12/24/31/13/24/11/7 = 153（第 9 节含 4 条 NOT RUN）。
前两轮都没绿，账见 `docs/evidence/P14/2026-10-27-p14.3/result.md` §4、§5：第一轮死在
`grep -qi active` 匹配 `inactive`，第二轮把 Q464 那个 500 暴露出来。

**"已验证"的部分**（现场文件在 `docs/evidence/P14/2026-10-27-p14.3/facts/`，50 个）：

- **管理通路拦截**：启用 + 缺 22/tcp 的定义 → 预检 `allowed=false` +
  `FIREWALL_MANAGEMENT_PATH`，理由里点名 SSH；期间 `ufw status` 仍 inactive、
  `ufw show added` 仍 `(None)`；把 22 加回同一份提交通过。停用状态下同一份定义**不**拦
  （关着就没有断连风险）。
- **独立回滚**：apply → `outcome=verified`，`token=frb_695fe87aa232cb87fa6310eef902d7b9`、
  `deadline=19:12:26Z` → `19:10:51Z` 停 Web 与代理（`curl` 返回 `000`）→ `19:11:47Z` 60 秒检查
  新规则仍在、timer 仍武装 → `19:12:34Z` systemd 拉起
  `upanel-fwrollback@frb_695f….service` → `19:12:45Z` `upanel-agent[162295]` 打完
  `防火墙回滚 frb_… 已处理`、`Deactivated successfully` / `Finished` → 规则换回旧集
  （`upanel:web` 消失、`upanel:db` 回来）、`ufw` 仍 active。
- **只碰带标记的规则**：手写 `ufw allow 9000/tcp` 在阶段 4—8 全程原样；apply 前 `upanel:ssh` +
  8443 出现、回滚后变成 `upanel:ssh` + 5432。
- **两处反复确认的负向**：token 不符 → `FIREWALL_TOKEN_MISMATCH` 且不动现场；已有 pending 时
  再 apply 被拒；schema 坏定义在**解码层**就是 400（比阻塞项更早返回）。
- **撤干净**：`{enabled:false, rules:[]}` → `ufw status` 回 inactive、`show added` 回 `(None)`、
  `/etc/default/ufw` 三行策略前后一致。
- **两条救命看门狗都真醒过**：`19:10:15Z`（systemd timer）与 `19:10:37Z`（sleep 子壳）各写一条
  `not-needed: 22/tcp 有放行`。这一条是刻意收紧的：脚本第一版从没调用 `write_watchdog`，
  脚本不存在 → 看门狗不会开火 → "看门狗没有动手"成了**永远不会红**的断言（Q465）。

**NOT RUN（明确未验，不含糊）**——脚本自己报 4 条：

1. 浏览器里的真实渲染与交互（VM 内无 Playwright，Q21），由 SSR 渲染测试与 `web-smoke` 覆盖。
2. 回滚自己失败（记录 `state=failed`）：真机上造它等于把机器弄坏到两条看门狗之外，风险不对等。
3. ufw 根本没装的机器（`FIREWALL_UNSUPPORTED` + `editable=false`）：卸载 ufw 会连 VM 自己的
   转发规则一起带走，不是一条能安全回头的用例。
4. IPv6 规则、应用配置（`ufw allow 'Nginx Full'`）、端口区间、按接口限定：这一版**明确不支持**。

**已知限制**（写进 DECISIONS 而不是藏着）：① `OnActiveSec=120s` 会被并发的 `daemon-reload`
顶后（同 Q457，防火墙这一轮没撞上，60 秒那条断言就是在盯它）；② 回滚只恢复"面板管理的规则集 +
启用状态"，管理员这一轮里手写的规则不会被退回（不属于这次改动的"之前"，Q459）；
③ 非标端口 SSH 若既无 `sshd` 进程名又无面板监听，会被当成"管理端口认不全"而不是"22 就够"；
④ 一次只允许一条未确认的改动（两条叠加会让"退回哪一份"没有答案）。

**结论：P14.3 passed。** 代码已实现、单测与宿主检查已过、真机验收通过（153/0/4，退出码 0）。
两个写开关仍默认关闭；`-firewall-writes` 打开后也只动带 `upanel:` 标记的规则。

---

### P14.4（2026-10-27）— NFS：网段是唯一的访问控制，`exportfs -ra` 的退出码不是证据

STAGES 的原文是「NFS 受管理导出、网段/root_squash、卷依赖、真实客户端读写验收」。这一轮
只做 NFS 那一半的 `share.apply` / `share.disable` / `share.delete`。

**实现侧已经写完的（代码 + 单测）**：`internal/shares/nfs.go` 负责渲染与解析导出文件，
`exports.go` 负责"写—读回—失败还原"，`assess.go` 的 `exportsAssessment` 负责预检；
`internal/agent/sharewatch.go` 的撤销循环（30 秒一轮）负责卷掉了之后摘导出行。判据链与
SMB 那一套同形：执行前重新判决 → 写完读回 → 失败说清在哪一步并只回滚自己造的东西。

**真机验到的四件硬事**（都是在 22.04 VM 上跑出来的，不是推的）：

1. **只维护一份文件，认领靠行尾标记。** 导出全部写进 `/etc/exports.d/upanel.exports`，
   行的结尾是 `# upanel:<share_id>`；`/etc/exports`（管理员那份，VM 里放着
   `/srv/upanel-nfs *(ro,async,…,fsid=0)`）整轮 sha256 前后逐字节相同。别人的 `.exports`
   文件只读来做冲突检测，不改写、不导入成可编辑共享。停用/删除之后托管文件正文一条都不剩
   （只有三行说明），目录与里面的文件一个不少、内容哈希不变。
2. **网段是唯一的访问控制，而且它是真的在挡。** 把允许网段从 `127.0.0.0/8` 改成
   `192.168.77.0/24` 之后，**同一台机器**挂不上（`mount` 非 0 退出、`findmnt` 空）；
   改回来立刻又能挂上。中间没有账号、ACL、密码参与。
3. **只读是两道锁：导出行 `ro` + 目录 `0755`。** 客户端把文件写进只读共享，被内核以
   `Read-only file system` 拒绝，服务端目录里没有多出这个文件。这里有个坑必须记下来：
   **NFSv3 客户端的 `findmnt` OPTIONS 永远报 `rw`**，只读表现为服务端回 EROFS——所以
   "挂载选项里是 ro"这种断言根本不成立（Q466）。
4. **卷依赖与运行时撤销只减不增。** 卷还在挂载树里时面板拒卸载（`VOLUME_IN_USE_BY_SHARE`）；
   卷摘出挂载树之后，撤销循环摘掉导出行、重新 `exportfs -ra`、写
   `runtime_revoked.volume_state=unmounted` 与 `config_applied=false`，实测 **13 秒**完成；
   卷挂回来**不自动复活**，重新 `share.apply` 才恢复，客户端随后又能挂上并读到之前写进去的
   文件。

**判据一律读回 `exportfs -v` 的实际导出表**，按路径整行匹配再取它缩进的客户端行——
`exportfs -ra` 的退出码不作为证据。理由是真机撞出来的：目录不存在时 `exportfs -ra` 报
`Failed to stat` 并以 1 退出，但它仍然把导出注册上了；反过来同路径导出两次会让
`exportfs` 报 `duplicated export entries` 并以 1 退出，**整份**导出表刷新失败，连别人的
导出一起停摆（Q466）。

**真机抓出来的 2 个产品缺陷（都是真机先红、修完再验）**：

| # | 真机现象 | 根因 | 修法 |
|---|---|---|---|
| 1 | 导出写下去了、`exportfs` 也吃下去了，最后在 `SaveShare` 被 SQL 挡住：`shares.protocol CHECK (protocol IN ('smb'))` | 0005 迁移把协议钉死成 smb，新协议没有对应迁移 | 新增 `0013_shares_protocol_nfs.sql`：SQLite 不能就地改 CHECK，只能表重建（建新表→拷数据→删旧表→改名→重建两个索引）；并把"旧 schema 真的拒绝 nfs"写成断言 `TestMigration0013RebuildsSharesAndKeepsRows`。`fakeStore` 不跑 SQL，所以单测一直是绿的（Q471） |
| 2 | 同一个目录的第二条共享，预检 `allowed=true` | `ExportConflicts` 只比"别人的导出文件"，面板自己的两条共享指向同一个目录没人拦 | 新增纯函数 `SameDirShares` + `sharesSharingDir`（预检），执行期 `applyExports` 里再拦一次；预检与执行用同一个函数，不靠"合并两行和稀泥"（两条共享的网段、只读标记可以不同）。`CONTRACTS.md` §7(5) 补上两个来源（Q473） |

**另有一条同类未修，必须写明**：SMB 路径（`internal/shares/write.go:230`）在配置生效之后
落库失败同样不回滚。修它需要把 `configState` 带出 `applyManagedConfig`，并在还原之后
reload `smbd`——那是 P08 的代码路径，本轮不动。已登记在 DECISIONS（Q472），不是"忘了"。

**验收方法自身的缺陷 7 类**（单独列表，因为它们全都伪装成产品故障）：最险的是脚本里写成
`"$(PRE_ALLOWED)"`（少一个 `$`），把变量当命令执行，一条语法错把第 7—10 节六十几条断言
全带红了；其次是 `grep -c '# upanel:'` 把文件头那句示例注释也数进去，导致"两条导出"数成
3；再次是 `umount -l` 只摘命名空间、本机 `knfsd` 仍持有文件系统（`fuser -vm` 点名
`kernel knfsd`），让"从外面卸卷"永远 `target is busy`——改成硬卸优先，并且"设备级掉线"
明确登记为 NOT RUN 而不是拿 lazy umount 冒充。其余四类（`ro` 断言不成立、夹具没给
`0644` 导致被 squash 的 `nobody` 读不到、"disable 顺手删记录"这种与契约相反的断言、
前置失败后六十多条断言用垃圾值变红）逐条记在 Q474 与 `result.md` 第 5 节。

**结论：P14.4 passed。** 22.04 VM 第七轮 `PASS 141 / FAIL 0 / NOT RUN 6`，退出码 0；
证据目录 `docs/evidence/P14/2026-10-27-p14.4/`。6 条 NOT RUN 逐条写明为什么没验——
跨机器挂载、Windows/macOS 客户端、NFSv4 直挂、两条导出并发写托管文件、kerberos、
设备级掉线（这台 VM 是 `-monitor none` 起的，跑的时候拔不了盘）。

### P14.5（2026-10-27）— 账号：禁用是换壳（不是 `-L`），sudo 的判据是 sudo 自己

STAGES §16 对 P14.5 的要求是「受管理 Linux 账号、SSH 公钥、受限 sudo 模板；禁止修改
root/系统账号和面板身份提权」。四条设计决定支撑它：

- **两种账号、三处必须分开显示**：`regular`（人能登录）与 `share`（只给 Samba）。
  页面上 `shell`（此刻的壳）、`shell_profile`（期望档）、`has_password`（设过没有）
  是三个独立字段——把它们合成一个"状态"就会说出与磁盘不符的话。
- **禁用靠换壳**：`disabled` 档把登录壳换成 `/usr/sbin/nologin`，`restricted` 档是
  `/bin/rbash`。**不用 `usermod -L`**：`-L` 会动 shadow 的口令字段，等于替用户改了别的
  事实，启用时还得记着原来的哈希；换壳只改一行、可逆、读回一眼就能看见。
- **`authorized_keys` 托管区**：整块由面板重写，区外逐字节不动。标记是
  `# >>> upanel 托管区（整块由面板重写，手改会被覆盖）>>>` / `# <<< upanel 托管区 <<<`，
  文件 `0600`、目录 `0700`、上限 10 把。区外一旦被碰，"面板不会覆盖你的东西"这句话就没了。
- **sudo 的五道门**：候选文件 `visudo -c -f` → rename → 整份 `visudo -c` → 解析读回 →
  `sudo -l -U` 读回；**判据是 sudo 自己**，不是面板说"我写了"。模板是闭集
  （`system_status` = `/usr/bin/systemctl status --no-pager`、`journal_recent` =
  `/usr/bin/journalctl -n 200 --no-pager`），零通配符——sudoers 的 `*` 是按单个参数做
  glob 的，`journalctl -u *` 实际允许 `--vacuum-size=1G`（Q476）。文件 `0440`、带认领标记
  `# upanel:`，手改过的文件一律 `SUDO_FILE_DAMAGED` 拒绝、一个字不动。

**真机三轮才绿：真正暴露产品缺陷的只有第一轮**。

| 轮次 | 结果 | 红的根因 |
|---|---|---|
| 第一轮 | 63 PASS / 111 FAIL | ① 环境：`/etc/sudoers.d/upanel` 是 `0644`，整份 `visudo -c` rc=1；② 产品 Q477：`Definition` 无 json 标签 → `display_name` 400；③ 产品 Q478：`GET /accounts` 用 Samba 的能力当账号判据 |
| 第二轮 | 162 / 3 | 全是脚本与口径：`password_changed` 只在改账号时出现；`ssh-keygen -lf` 一行一把被当单值比；§11 断言"没口令就登不上"与真机相反 |
| 第三轮 | **168 / 0 / 6** | — |

**真机抓出来的 2 个产品缺陷（都是 111 条红里挖出来的，单测全绿）**：

| # | 真机现象 | 根因 | 修法 |
|---|---|---|---|
| 1 | 第 3 节起全线红：预检就回 `json: unknown field "display_name"` | `accounts.Definition` 一个 json 标签都没写；代理用 `DisallowUnknownFields`，而 encoding/json 的大小写不敏感只对整词有效（`name`→`Name` 行，`display_name` 不行）。所有单测只构造 Go 结构体，从不走**线格式** | 六个字段全补标签；新增 `internal/agent/accountaction_test.go`，用例一律从 JSON 字符串出发走真实 `Decode`；变异核验：去掉 `display_name` 标签，用例复现真机那句话（Q477） |
| 2 | 账号页显示"这台机器不支持账号管理"、新建按钮是灰的 | 账号与 Samba 是**不相交**的两个工具集，`GET /accounts` 却直接返回 `shares.Capability`（判据是 `smbd`/`smbpasswd`/`pdbedit`/`setfacl`） | 照 P14.4 给 NFS 单开判定的成例：新增 `accounts.Capability` 与只读查询 `accounts.capability`；Web 侧 `AccountCapability` / `ShareCapability` 两个类型。回归含"有 smbd 没 useradd 必须 blocked"这条缺陷正身（Q478） |

**另有一条不是产品缺陷、但比缺陷更该记的：文档说反了。** 四处文案（`api/accounts.ts`、
`AccountsView.vue` 三处、`utils/jobresult.ts`、`CONTRACTS.md` §13）都写着"没有口令的账号
即使配了公钥也登不上"，并且自认"未验证"。真机 §11 直接把它否了：publickey 认证不看
shadow 的口令字段，**key-only 账号能登**。这不是缺陷——key-only 是服务器上很常见的用法。
真正拦人的是**壳**。所以脚本 §11 改成四面（能登 / 禁用后被拒 / 启用后又能登 / 撤钥匙后
被拒），把"登不上"归因到壳上而不是 sshd 配置；中文案全部改成"只能用已配的公钥登录 /
要拦人就禁用"，并给 `accounts.test.ts`、`jobresult.test.ts` 加了钉住的用例（Q479）。

**验收方法自身的两条缺陷**：`password_changed` 只在改账号时出现在结果里，建号读的是
`password_state`/`has_password`；`ssh-keygen -lf` 对多把钥匙一行一把，原来拿它跟单值比，
新增 `fp_listed()` 做包含判断。另外环境那条（`/etc/sudoers.d/` 里任何非 `0440` 的文件都
让整份 `visudo -c` 失败）记进 `docs/TEST_ENVIRONMENT.md` §8.12，坏文件留档在
`/var/tmp/p145-envfix/upanel.badmode`。

**结论：P14.5 passed。** 22.04 VM 第三轮 `PASS 168 / FAIL 0 / NOT RUN 6`、退出码 0；
证据目录 `docs/evidence/P14/2026-10-27-p14.5/`（全量 `run.log`、四条 ssh 读数、sudo 真跑
输出、宿主检查）。6 条 NOT RUN 逐条写明为什么没验：共享账号与 Samba passdb 两半（这台 VM
没装 `samba-common-bin`，不为一条验收去装包）、两条公钥动作并发写同一份 `authorized_keys`
（锁由单测覆盖）、第 11 把公钥被拒（上限在判决层）、home 在别处/是符号链接（面板拒绝写，
只在单测造过）、面板自己的 TOTP 与登录策略（P14.6）。

### P14.6（2026-10-27）— TOTP：确认过的那个码不能再用第二次

STAGES §16 的两条硬要求是「TOTP 未确认不启用」与「恢复码只能使用一次」。这一轮的现场读数
与逐条断言写在 §1，证据在 `docs/evidence/P14/2026-10-27-p14.6/`；这里只记方法与结论。

- **重放防护靠一条 SQL**：`AcceptTotpStep` 用 `UPDATE user_totp SET last_step=? WHERE user_id=?
  AND last_step < ?` 同时完成“判断”与“写入”，零行即 `ErrTotpReplay`。判据在库里而不是在
  进程内存里，因此重启、多会话、CLI 与 Web 并发都走同一条线。
- **“只能用一次”必须造中间态**：关掉 TOTP 之后旧恢复码本来就不认，那证明不了“用过”。
  现场的造法是：先带码认证把再认证窗口推到 5 分钟 → 停代理 → 用 RC1 关闭得到 503，带外
  读到 `used_at` 已写而状态行还在 → 起回代理 → 同一枚 401，换 RC2 才成功。
- **前端产物要爬分片**：`/auth/totp` 在 `totp-*.js`、`只显示这一次` 在 `SettingsView-*.js`，
  只抓入口 bundle 的断言是假绿。第 8 节从 `index.html` 出发逐代爬 chunk（4 轮、55 个分片、
  1,625,696 字节），并断言分片数 ≥ 2。
- **真机抓到 1 个产品缺陷（Q480）**：`EnableTotp` 不搬确认时匹配到的 `Step`，`last_step`
  写死 -1，于是**确认用的那个六位数在同一个 30 秒窗口里还能当登录码用一次**。单测当时把
  缺陷写成了期望值（断言 `LastStep == -1`），变异核验才暴露出来。修法是签名加 `lastStep`、
  `< 0` 拒绝启用、INSERT 写入该步；三处调用点与两条新用例钉住。
- **验收脚本自身 10 条假红（Q481）**：偏移加到 ±1 窗口外面、空串当 needle（`grep -F ''`
  匹配每一行）、`jget` 的失败标记被原样拼进 SQL。三条都改成“先证明取到了再拼”。

**结论：P14.6 passed。** 22.04 VM 第二轮 `PASS 163 / FAIL 0 / NOT RUN 6`、退出码 0。
6 条 NOT RUN：两枚恢复码在同一个时间步被并发消费（唯一的原子性证据是 `used_at` 的
`UPDATE … WHERE used_at IS NULL`，未做真并发）、时钟回拨与 NTP 跳变、>`±1` 窗口的时间步、
`otpauth://` 在真实手机 App 里扫码（无设备）、`upanelctl totp-reset` 在代理停止时的行为
（P14.6 只验了 root 下 403 与 `upanel-web` 下成功）、24.04 机器上的同一套脚本（本轮只跑
22.04，24.04 的差异在 P16.2 双平台端到端里一并覆盖）。

### P15.1（2026-10-27）— 部署定义：先把“不能表达什么”定死，再谈怎么渲染

STAGES §17 对 P15.1 的要求是「受限 `DeploymentDefinition`、镜像/端口/资源/目录校验、秘密
环境变量、Compose 配置渲染」，验收那一侧要求「恶意挂载 / `docker.sock` / devices /
privileged / host network **全部无法表达或被拒绝**」。这一轮只做**纯函数层**：
`internal/deploy`（定义、校验、确定性渲染），没有接线（接口、库表、执行分别是 P15.2/P15.4）。

- **已实现**：`Definition` 十个字段的闭集（JSON 严格解码，未知字段 400 `unknown_field` 并
  点名）；`Normalize` 管形状（名字/镜像/端口/IP 字面量/环境变量/挂载/资源/健康检查/依赖卷，
  八个稳定拒绝码，只报第一条）；`CheckHost` 管宿主事实（地址是不是本机此刻的、文件根在不
  在、根所在的**受管理卷**在不在、那个卷有没有写进 `volume_dependencies`）；`Render` 产出
  确定性 Compose 字节 + 配置摘要 + 挂载来源表 + 秘密引用表（不解秘密就渲染不出来）。
  渲染在**代理侧**，因为秘密只在代理的密钥环里（`Resolver` 只收 `app_env` 用途）。
  五处写法是功能不是风格：`restart` 恒为字符串 `"no"`、挂载用长语法且
  `bind.create_host_path: false`、所有引号标量的 `$` 转义成 `$$`、健康检查**不**进容器、
  不写顶层 `name`（项目名由执行时 `-p` 传）。依据见 DECISIONS Q482 与 CONTRACTS §8.6。
- **已验证（宿主）**：`go build` / `go vet` / `gofmt` 干净；`go test ./internal/deploy/`
  20 个顶层用例、100 条 PASS 记录全绿，其中 §17 点名的恶意输入逐条成表（`privileged`、
  `devices`、`network_mode`、`cap_add`、`compose_yaml`、`..`、绝对宿主路径、`/proc`、
  `/sys`、`/dev`、`docker.sock`、绑定通配地址、只读根读写挂载、健康检查探非容器端口）；
  黄金用例逐字比对整份渲染结果（含 `$` 双重转义与缩进）。
- **未验证**：**没有在任何真实 Ubuntu 虚拟机上跑过**，也没有被任何生产代码路径调用——
  `upanel-agent`/`upanel-web` 都还没 import 这个包（P15.2 才接线）。因此「正常镜像可部署/
  升级」「卷缺失时 VM 重启也不绕过」「健康失败显示失败」这三条 §17 验收**一条都还没有真机
  读数**。渲染出的 YAML 也**没有**被真实的 `docker compose config` 解析过：字段名与键序是按
  Compose spec 写的，没有一个 `docker compose` 的读回证明。
- **未验证（范围外，按计划留给后面）**：定义与渲染结果的持久化、`POST /deployments` 一类写
  接口、配置 0600 落盘、包装 systemd 单元、执行 compose、镜像 digest 回读、更新/回切与
  页面（P15.2—P15.4）。`GET /deployments` 仍是只读观测（§8.3）。
- **受阻**：无。下一个子任务 P15.2（systemd 包装、挂载依赖、容器 `restart=no`、部署/停止/
  删除保留数据），入口就是这一层的 `Render` 结果与 `Rendered.MountedSources`。

### P15.2 前半程（2026-10-28）— 包装单元与落地：先让“卷没挂上”真的能挡住启动

STAGES §17 对 P15.2 的要求是「systemd 包装、挂载依赖、容器 `restart=no`、部署/停止/删除保留数据」。
这一轮把 `internal/deploy` 从“能渲染一份配置”推进到“能在宿主上落地一个受管部署”，**只做创建与
删除**：`deployment.update` 是 P15.3 的事，在 P15.2 里被明确拒绝而不是悄悄覆盖。

- **已实现**：`unit.go`（包装单元 + mount unit 转义 + compose argv）、`applier.go`（写盘、读回、
  删除）、`service.go`（两个动作的预检与执行两阶段）；`HostEnv.Volumes` 换成携带
  `MountPoint`/`FilesystemUUID`/`Mounted`/`MountedUUID` 的 `VolumeFacts`，新增两条判据与
  `CheckVolumesReady`（PLAN §11 规则 2/4 的启动前 UUID 核对）。
- **为什么 `restart: no` 必须配一个包装单元**：`no` 只说明“没人再拉它起来”——包括开机。
  没有单元时，机器重启后应用永远回不来而面板上一片绿；有单元时，开机由 systemd 起、运行中
  由 systemd 守、停止由 systemd 收，容器因此永远只有一条生命周期路径，也就不存在“docker 在
  卷挂载之前先把容器拉起来”这个洞（规则 4）。
- **为什么单元要 `BindsTo` mount unit 而不是只写 `RequiresMountsFor`**：前者在**运行中**卷失联
  时也会停掉单元（规则 5 的落点），后者只管启动顺序。两条一起写，开机与运行中两个窗口都盖住。
- **为什么版本是配置的 sha256**：它与 §8.6 的 `ConfigDigest`、以及 Web 侧库表里的
  `applied_digest` 是同一个值。用它做 `deployment.remove` 的乐观并发令牌，意味着“你删的是你看到
  的那一份”，而这份判断只需要读一个文件就能复核。
- **已验证（宿主）**：`go build`/`go vet`/`gofmt` 干净；`go test ./internal/deploy/` **51 个顶层
  用例全绿**，其中钉住的有：单元黄金样本逐字比对、mount unit 名逐字符（`/srv/upanel/volumes/
  my-app` → `srv-upanel-volumes-my-app.mount`）、`systemctl` 调用序列（`daemon-reload` →
  `enable` → `start` → `is-enabled` → `is-active`）、幂等重放不重写文件（mtime 不变）、异内容被
  `ErrDeploymentExists` 拒绝且磁盘不变、`compose up` 返回 0 但单元非 active 时失败并给出
  `journalctl`（消息里带上它，测试断言这一点）、单元被手工改过则整篇重写、删除后业务目录与
  其它部署的单元都还在、`disable --now` 之后仍 active 时**不删文件**、版本令牌不符时一个字节
  都不动。`sh scripts/check.sh` **PASS 14 / FAIL 0 / NOT RUN 0**。
- **未验证**：`deployment.apply` / `deployment.remove` **还没有登记进代理的动作表**——`Service`
  需要的宿主事实（文件根 → 受管理卷的最长前缀映射、卷的 `Mounted`/`MountedUUID`）与密钥环
  解析器在代理侧还没接起来；`cmd/upanel-agent` 也还没构造 `Applier`。**没有在真实 Ubuntu 上跑过
  一次 `docker compose`**，`systemctl` 的调用全部来自 fake runner。因此 §17 的三条验收
  （真实部署与升级、卷缺失重启不绕过、健康失败显示失败）仍然**未验证**。
- **未验证（范围外）**：`deployment.update`、镜像 digest 记录、按选择备份、健康探测与回切
  （P15.3）；`/deployments` 写接口与页面（P15.4）。
- **受阻**：无。下一个动作是 P15.2 的接线：把这两条动作登记进 `NewProductionRegistry`，在代理侧
  用文件根表与卷表构造 `HostEnv`（含最长前缀映射与 UUID 读回）、用密钥环实现 `Resolver`，
  然后在 VM 上跑一次真实部署与一次“卷缺失后重启”。

### P15.2 真机验收（2026-10-27）— 第一次真的把容器跑起来：四个产品缺陷，全在真机上

STAGES §17 对 P15.2 的要求是「systemd 包装、挂载依赖、容器 `restart=no`、部署/停止/删除保留数据」。
接线完成后在 VM-A（Ubuntu 22.04.5、docker 29.1.3 / compose 2.40.3、夹具盘 `/dev/sdd`）上跑
`tests/integration/p15-2-acceptance.sh` 两段：`PHASE=main` **199 PASS / 0 FAIL / 2 NOT RUN**、
`PHASE=boot` **51 PASS / 0 FAIL / 0 NOT RUN**。证据 `docs/evidence/P15/2026-10-27-p15.2/`
（`RESULT`、`RESULT-boot`、两段控制台、`facts/`、`logs/`、`payloads/`）。

- **已实现（本轮补的）**：`agent/deployaction.go` 的接线与两处判决修正；`Applier.Apply` 的启动
  动词按现场选择；`systemctlAnswer`（退出码是答案）；`deploy.deploymentStateVersion`（四个非空
  令牌）。宿主侧新增 `TestAssessApplyBindsStateVersionOnCreate`、
  `TestAssessRemoveBindsVersionWhenConfigMissing`、`TestDeployApplyUpdateRefusalIsAPlanNotAnError`、
  `TestDeployRemoveRefusalIsAPlanNotAnError`、`TestApplyRestartsWhenUnitAlreadyOnDisk`、
  `TestApplyStartsFreshUnit`、`TestInspectToleratesDisabledAnswer`；假 runner 改成与真实 `syscmd`
  同形（非零退出 → `*syscmd.ExitError`，`enable`/`disable` 真的翻状态）。
- **已验证（真机）**：部署真的起来（`verified`、容器 1 个在跑、宿主端口 HTTP 200、应答里有
  `Host` 头）；包装单元逐字符合契约（`facts/unit.service`）；幂等重放不重写、不重启、容器 ID 与
  `StartedAt` 都没变；判决层（`update_not_implemented` / `deployment_exists`）在真机上给出 200 +
  blocker，而不是 500；masked mount unit 让 `systemctl start` 失败（`Unit … is masked.`）、
  卷没挂上、marker 没落到系统盘、预检给 `volume_not_ready`；重启后单元 `enabled` 但 `inactive`、
  系统盘无数据、journal 无条目（masked 依赖让事务在启动前就被拒）；解除 mask + 挂回卷后单元与
  容器都回来；删除后单元与配置消失、`systemd` 报 `not-found`，业务目录与 `marker.txt` 逐字节还在。
- **四类产品缺陷（真机才暴露，全部已修）**：① 新建部署预检 100% 500（缺版本令牌，Q484）；
  ② 带 `deployment_id` 的预检 500（`deployPlanFromError` 漏填 `NormalizedParams`）；
  ③ apply 落在**已有的 active 单元**上时 `start` 是空操作，配置写了但容器一个没起（Q485）；
  ④ `deployment.remove` 永远删不掉——`is-enabled` 对 `disabled` 退出 1，被当成执行失败（Q486）。
  其中 ③④ 的共同根因是**假件与真机形状对不上**：假 runner 不会以非零退出返回错误，也不会在
  `disable` 之后翻 `enabled` 状态，于是"真机上唯一会走的那条路"在宿主测试里根本不存在。
- **五类验收脚本自身的错误（不是产品缺陷，已修）**：起点没清干净（上一轮的 active 单元、fstab
  条目、残留容器都留在盘外）；`findmnt --mountpoint` 对子目录返回空；`runuser` 写不进 0700 的
  root 目录导致 facts 缺文件；删除后仍用 `-f <已删掉的配置>` 数容器（compose 的报错行被数成
  1 个容器）；以及一处**假绿**——容器 ID 与 `StartedAt` 两侧都为空也算"没变"。
- **NOT RUN（两条，理由在 Q487）**：`容器里读到的 marker`（`traefik/whoami` 是 scratch 镜像，
  容器内没有 `cat`/`sh`，挂载改由 `docker inspect` + `findmnt` 交叉核对）；
  `运行中卷失联 → systemd 自动停单元`（容器把卷 bind 进自己的 mount namespace，`umount -l`
  造的是假现场）。
- **未验证（范围外）**：`deployment.update`、镜像 digest、按选择备份、健康探测与回切（P15.3）；
  `/deployments` 写接口与页面（P15.4）；24.04 上的 systemd/docker 差异；bind mount 前的句柄级
  路径核对。
- **受阻**：无。下一个子任务 P15.3：`deployment.update`（拉镜像并记录实际 digest）、按选择备份、
  启动后健康探测与失败可见、回切边界。

### P15.3（2026-10-29）— 更新与健康检查：先让"回切"有据可依，再谈换镜像

STAGES §17 对 P15.3 的要求是「`deployment.update`（拉镜像并记录实际 digest）、按选择备份、启动后
健康探测与失败可见、回切边界」。库层（`internal/deploy`）与代理接线（`internal/agent`）先做完，
随后在 VM-A 上跑通真机验收（见下一小节）。

- **已实现（`internal/deploy`）**
  - `record.go`：`<name>.deployment.json`（`0640`）记规范化定义与镜像记录，`<name>.compose.yaml.prev`
    （`0600`）记上一版原样字节。`requireRecord` 把"盘上没有"与"有但不是合法 JSON"分开报
    （`ErrRecordMissing`），两种情况都拒绝更新——没有记录就当场补一份是编造。为什么定义必须落盘见
    Q488。
  - `update.go`：`AssessUpdate`（只读：形状 → 记录 → 盘上摘要 → 宿主事实 → 卷依赖 → 渲染 → 备份
    策略）与 `ExecuteUpdate`（停单元 → 备份 → 拉镜像 → digest 核对 → 写 `.prev` → 写新配置并
    `restart` → 探测 → 失败回切）。`UpdateBudget` 一组常量把 10 分钟硬线切成段，每段动手前判断够
    不够（Q491）。
  - `probe.go`：面板侧探测探**宿主公开入口**（通配地址换 `127.0.0.1`），`http` 判 `200—399`、`tcp`
    判能建连，不跟随重定向、禁 keep-alive，120 秒预算且 `Truncated` 不算通，启动宽限期内失败不算
    失败。`Checked`/`Healthy` 是两个字段：**"没探成"不等于"探不通"**。
  - 回切写回 `.prev` 的**原样字节**并 `restart`，再探一次；**数据不跟着回切**，这句话逐字进了错误
    消息（Q489）。
  - `applier.go`：新增 `ApplyRequest.ReplaceExisting`（"那份不同内容的配置就是要被换掉"，只有
    update 传）、`ApplyRequest.MustRun`（"这个动词必须真的执行"）与 `Applier.RestartUnit`（只
    restart、不碰文件）。两个字段分开不合并的理由见 Q492。
  - `service.go`：`ApplyOutcome.Health`，`ExecuteApply` 在 `started` 之后探测（`probing` 步骤），
    探测失败把原本 `verified` 的判决降成 `partial`；`AssessApply` 的 `health_probe_pending` 告警
    删除——CONTRACTS §8.7 那条"这一版不探测 HTTP"随本轮作废。
- **已实现（`internal/agent`）**
  - `deployaction.go`：`deployment.update` 登记进动作表（`DeploySource` 增 `AssessUpdate` /
    `ExecuteUpdate`），五种 impact、锁含 `backup-policy:`/`backup-repo:`，进度步骤
    `stopping`/`backup`/`pulling`/`writing`/`probing`。**回切成功不是失败**：两个"已回切"的拒绝码翻成
    `OutcomeRolledBack` 且 error 为 nil，读回值（回切后摘要、单元状态、`rollback_health_*`）因此进得
    了审计；回切本身失败才是错误（Q490）。
  - `deploybackup.go`：更新前备份的适配层，只收 `files_only`；`stop_managed_apps` 在预检与执行两处
    都拒（预检与执行之间策略可能被改）。
- **已验证（宿主）**：`go build ./...` 干净；`go test ./internal/agent/ ./internal/deploy/ ./cmd/...`
  全绿（`update_test.go` 13 个用例、`record_test.go`、`probe_test.go`、`deploybackup_test.go`）；
  `sh scripts/check.sh` **PASS 14 / FAIL 0 / NOT RUN 0**（含 `gofmt`——首轮 7 个文件未格式化，已修）。
- **接线阶段抓到的三个产品缺陷（都是产品缺陷，不是测试问题，Q493）**：`ErrDeploymentExists` 把更新
  挡在门外；`Apply` 只在 `!exists` 时写配置，于是更新"成功但版本没变"；`restartQuietly` 走 `Apply`
  被判据挡住，而错误被吞掉——"备份失败后把单元起回来"这一步静默地没发生。
- **已验证（真机）**：2026-10-27 VM-A（Ubuntu 22.04.5、docker 29.1.3 / compose 2.40.3、
  restic 0.12.1）`PHASE=main` **229 PASS / 0 FAIL / 1 NOT RUN**、退出码 0
  （`docs/evidence/P15/2026-10-27-p15.3/`）。读数与边界见下面「P15.3 真机验收」小节。
- **受阻**：无。下一个动作是 P15.4（`/deployments` 写接口与更新 UI）。

### P15.3 真机验收（2026-10-27）— 回切在真机上是一次能读回的事实，不是一句承诺

脚本 `tests/integration/p15-3-acceptance.sh`（单段 `PHASE=main`，七分半钟）铺好卷、文件根、备份
仓库与策略之后，把 §6—§12 七段走完：建站 → 正常更新 → 同镜像重放 → 探测失败回切 → 带备份的
更新 → 只收 `files_only` → 四种拒绝。**229 PASS / 0 FAIL / 1 NOT RUN**。

- **已验证（建站与记录）**：记录 `0640`；记录里的 `image.digest` 与
  `docker image inspect -f '{{.Id}}'` 逐字相同（`sha256:43a68d10…`）；记录里除 `sec_` 引用外没有
  秘密残留；第一次建站**没有 `.prev`**（这一条本身是断言，不是注释）。
- **已验证（正常更新）**：`ab9215e4…` → `e406bf01…` 两个版本对得上；`previous_config_digest`
  就是更新前那个；`config_changed=true` / `idempotent=false` / `unit_restarted=true`；新 digest
  `sha256:65645c7b…`；盘上三个文件 0600 / 0600 / 0640，`.prev` 的 sha256 与字节数与更新前**逐字
  相同**；容器运行时读回新镜像、项目标签 `webapp`、宿主端口 HTTP 200。
- **已验证（同镜像重放）**：`config_changed=false` 但 **`unit_restarted=true`**——Q485 那条
  “`start` 对 active 的 oneshot 是空操作”在更新路上同样成立；预检给的是 `config_unchanged`
  告警（预检 `warnings[]`，不是执行期 `observed_state.warnings`）。
- **已验证（回切）**：换成 `busybox:1.36.1` 之后 **104 秒**（`12:49:46Z` → `12:51:30Z`，4 次探测
  尝试真等满超时）；终态仍 `succeeded`、`outcome=rolled_back`、**`error` 为空**；`health_ok=false`、
  `rollback_verified=true`、`rollback_health_ok=true`；盘上配置 sha256 回到更新前那份、`.prev` 没被
  回切改掉、容器回到上一版镜像、HTTP 又通；**记录没被改**（新版本从来没被认下来过）；恢复提示
  把"数据没有回切"逐字写进了消息。
- **已验证（按选择备份）**：`unit_stopped=true`（先停单元再备份）、`backup_verified=true`、
  策略与仓库标识都进观测；快照 `d5458d51…` 在任务观测与**独立 `restic snapshots`** 两处都能读到，
  标签含 `upanel:consistency:files_only`。
- **已验证（拒绝与失败）**：`backup_policy_needs_apps`（并指路去单独跑 `backup.run`）、`stale`、
  `record_mismatch`、`no_definition_record`、`update_not_implemented`、`deployment_exists`——每一次
  之后盘上配置与容器一个字节没动；钉一个拉不到的 digest（`…@sha256:1111…1111`）预检仍允许，
  执行期 `failed`（`failed to resolve reference … not found`），配置与 `.prev` 没动、容器还在跑、
  单元仍 active、服务仍在应答——**失败没有把应用留在停掉的状态**。
- **NOT RUN（一条）**：`初始化 GPT / 建分区 / 格式化`——夹具盘 `/dev/sdd` 上已有 GPT，按上一轮
  夹具复用（要重跑请换一块空盘）。卷这一侧没有打折：独立工具读 UUID、导入、挂载、`findmnt`
  按 UUID 核对、marker 落在卷上，全部真跑。
- **本轮修掉的东西**：真机这一轮**没有暴露新的产品缺陷**（0 FAIL）。改掉的 13 条全在验收脚本里，
  都是"按记忆写就会假绿或假红"的地方：`$WORK/logs` 还没建就让重定向失败看起来像拉不到镜像、
  `update_params` 读了一个从未赋值的版本串于是 `stale` 分支永远不触发、`previous` 猜成
  `image.previous`、把 `config_unchanged` 当执行期告警读、一条自比自的假绿断言、
  `health_ok` 在"根本没探"时键不存在而 `jget` 抛 `KeyError`（新增 `expect_falsy`）、§12 拿"同名
  同内容 apply"当创建语义的判据（那份定义渲染出的字节与盘上一模一样，会走幂等路径）、
  `$TMPD` 0700 让 `web_curl` 全部拿到 000、`wait_job` 180s 上限短于 9 分钟更新预算、夹具卷里残留
  上一轮的数据会混进快照、§11 拿旧时代的 sha 比对、restic 各版本 `snapshots --json` 的 `id` 长度
  不一致、`RESULT` 0600 宿主读不到（取回时用 `sudo`）。逐条原因见证据目录 README 的表。
- **仍然未验证**：`image_digest_mismatch`（注册表按 digest 寻址，钉错 digest 会让 `docker pull`
  先失败，真机造不出那个现场，只有单测读数）；更新预算切段把时间耗到边界上逼出 `partial`；
  连续两次失败的回切；24.04 差异。
- **受阻**：无。下一个子任务 P15.4（`/deployments` 写接口与更新 UI）。

### P15.4（2026-10-30）— 部署页：先把“管理面与观测面是两张表”写进页面，再谈按钮

STAGES §17 这一条只有一句：“部署和更新 UI，明确禁止原始 Compose YAML、危险宿主挂载和特权选项。”
后端管理面（`GET /deployments/managed`、`/deployments/managed/:name`、代理侧两条查询、`api` 层、
openapi、CONTRACTS §8.9 的接口部分）上一轮已经接完，这一轮只做前端与文档：API 层、结果翻译、
部署页、路由入口。

- **没有新增写接口，这是设计不是欠账**：创建、更新、删除一律走既有的
  `/operations/preflight` + `/jobs`（`deployment.apply` / `deployment.update` / `deployment.remove`），
  页面上没有“直接部署”这个按钮。§17 说的“部署和更新 UI”在这里指**把受管部署管理起来的那一页**，
  不是一套 `/deployments` 的 POST/PUT/DELETE。
- **危险选项不是被拦住的，是写不出来的**：数据模型里没有 `compose_yaml`、`privileged`、`devices`、
  `network_mode`、`cap_add` 的落点，代理侧 `DisallowUnknownFields` 让它们连请求都进不来（400
  `unknown_field`）。所以页面连一个“高级 YAML”输入框都没有——留一个框再把内容拒掉，比不留更糟：
  它暗示那件事可能做到。
- **交付（API 层）**：`web/src/api/deployments.ts`——形状常量与正则（部署名 63 位、`dep_`/`vol_`/`sec_`
  三种 id、文件根 id、CPU 小数、环境变量名）、`ManagedDeployment*` 类型、两条查询、只读纯函数
  （`deploymentPhase` 只读 `unit_state`，**容器在跑不会把管理面说成 running**；`deploymentPhaseDetail`
  在 `known=false` 时必须给出两条可能原因；`updateHint`/`removeHint` 只解释已知的拦路条件）、三份
  预检参数（`applyParams`/`updateParams`/`removeParams`，版本令牌只来自服务端、没选备份时显式写
  `backup_policy_id: null`）、草稿与校验（`validateDeploymentDraft` 返回 `{field, message}`，`field`
  与 JSON 同形如 `environment[0].name`；**整行全空才是占位行，填了一半必须报错**——否则那一半会被
  静默丢掉）。两条实现细节记在这里：内存上限用乘法不用位移（`64 << 30` 在 JS 里是 32 位运算，截成 0
  会让校验报“最多 0 MiB”）；`BLOCKED_CONTAINER_PATHS` 十条把 `/proc`、`/sys`、`/dev`、`/boot`、
  `/lib/modules` 与三个容器运行时 socket 列成常量，校验只做提醒，真正的判决在代理预检。
- **交付（结果翻译）**：`web/src/utils/jobresult.ts` 追加 `summarizeDeploymentJob`——逐条读
  `observed`（配置/单元、镜像与 digest 是否真有读数、备份快照与校验、健康探测的四件事、回切是否
  复验、容器有无独立读数、删除后数据是否保留）。三条口径：`outcome=rolled_back` 是**这次更新没有
  成功**（`succeeded` + 回切），话术不许说“已更新”；`containers_checked=false` 那行必须带“没有独立
  读数”，不能把“没查”画成“没有”；`partial` 的话是“落地了但读回有一项不对”。
- **交付（页面）**：`web/src/views/DeploymentsView.vue` 只做四件事——列出现场、展开详情看记录、
  新建、换镜像、删除（后三件一律预检 → 任务 → 重新读现场）。任务轮询 2 秒 × 300 次 = 10 分钟，
  对应代理的 `UpdateBudget=9min`。依赖数据（文件根、卷、备份策略）在 admin 下 best-effort 取，
  取不到只让表单少选项、不阻塞列表；备份策略**只列 `files_only`**（`stop_managed_apps` 会被代理预检
  拒），文件根只列 `writable` 的。预检 120 秒过期有 ticker 管，过期后确认按钮关掉。
- **交付（入口）**：路由 `/apps/deployments`（`meta.moduleKey: 'apps'`）+ 应用页上的“受管部署”按钮。
  **不去顶导航里“容器”那一格**：那一格是“容器与镜像”，这一页读的是受管部署，顶上去等于对外说
  “容器页做好了”。`routes.test.ts` 断言 `containers.implementedIn` 仍是 `null`，防止被改回去（Q495）。
- **交付（文档）**：CONTRACTS §8.9（两张表的对照、接口形状与错误映射、`version` ≠ `config_digest`、
  `unit_state.known` 必须带 reason、读取上限 200 与 `skipped`/`truncated`、本轮已验证/未验证清单）、
  DECISIONS Q495/Q496（管理面与观测面分两张表；代理侧读写分成 `DeploySource` 与 `DeployInspector`
  两个字段，nil 时不登记查询——能力表不撒谎）。
- **已验证（宿主）**：`web/src/api/deployments.test.ts` **19** 个用例（阶段只读 `unit_state`、非法部署名
  抛 `RangeError` 且一个请求都没发、两条查询打到 `/api/v1/deployments/managed`、预检参数、定义闭集
  断言 keys 里没有 `privileged`/`compose_yaml`/`network_mode`/`cap_add`/`devices`/`user`、MiB→字节、
  秘密与公开值二选一、草稿校验逐条、定义⇄草稿往返）；`jobresult.test.ts` 新增 **5** 个（全文件 73）；
  `routes.test.ts` 11 条（含新增那条“不去点亮容器格”）；`npx vitest run` 全量 **61 文件 / 1161 用例
  通过**，`vue-tsc --noEmit` 干净，`vite build` 成功（`DeploymentsView` 单独 chunk 36.23 kB）。
  `sh scripts/check.sh` 在本轮最终代码上重跑：**PASS 14 / FAIL 0 / NOT RUN 0**。
- **未验证（NOT RUN）**：这一轮**没有任何真机读数**——上面每一条都是宿主单测与构建。页面链路
  **没有被人点过**，也没有 Playwright 用例（`web-smoke` 只走“未登录落到登录页”一条）。三种现场只在
  单测里造过：200 份以上（`truncated`）、`skipped` 非空、`config_drift` 为真。从这一页发起的
  `deployment.apply` / `update` / `remove` 一次都没跑过（动作本身在 P15.2/P15.3 真机验过，走的是脚本）。
  24.04 差异未验。
- **本轮抓到的实现缺陷**：`64 << 30` 截成 0（内存上限变成“最多 0 MiB”）；空行过滤与校验冲突（定了
  “整行全空才是占位行”这条产品规则）；`getManagedDeployment` 从同步抛错改成 `async`（非法名应当是
  被拒绝的 Promise，调用方一处 `try/catch` 能接住两条路径）。都是宿主阶段自查出来的，不是真机发现的。
- **受阻**：无。下一个子任务 P16.1（统一能力探测与安装/升级/卸载）。

### P15.4 真机验收（2026-10-28，VM 时钟）— 跑之前先修掉“门”，跑的时候再抓两处页面缺陷

CONTRACTS §8.9 当时留了一句“页面链路要等 P16 的统一验收轮（或单独一轮 VM 验收）才谈得上验收
通过”。这一轮就是那次单独验收：在一台干净的 Ubuntu 22.04.5（2 vCPU / 单线程 TCG）上装 docker.io
29.1.3 + compose 2.40.3，装 1.0 的 amd64 包，宿主上用 headless Chromium 经 SSH 隧道点这一页。

**先修阻断项（Q511）：这一页原先在真机上根本点不通。** `deployment.apply` / `update` / `remove`
三条都是 R=是，而**登录不开再认证窗口**——只有 `POST /auth/reauth` 把它推到 `now+5min`。页面原先把
“这一步要求再认证”只写在确认卡上，于是按确认必然 403 `REAUTH_REQUIRED`，链路一次都走不完。
P15.2/P15.3 验的是脚本直接打接口，所以这个洞一直活着。修法：预检说 `requires_reauth` 且
`session.reauthExpiresAt` 不在有效期内时摆出再认证卡（面板密码 + 开了 TOTP 才有的验证码），
`reauthPassword()` → `session.restore()` → 把被打断的那一步接着提交；口令只走 `/auth/reauth`，
不进任务参数、不进日志。真机上 `POST /auth/reauth` 200（3082ms）→ `POST /jobs` 202，5 分钟窗口内
后两次写入没有再要密码。

**第一轮 31 PASS / 12 FAIL，12 条红的成因必须先分类再动手**：

| 条数 | 定性 | 说明 |
|---|---|---|
| 1 | **产品缺陷** | `StatePanel` 的 empty 分支只把 `description` 交给 `NEmpty`，页面传的 `title` 被丢掉——空状态只剩“暂无数据”，部署页的“这台机器上还没有受管部署”看不见 |
| 8 | 脚本时序 | 详情卡先渲染“正在读取…”，脚本对着这三个字做了六次断言；改成等面板真读出内容再断言 |
| 1 | 同上两件事 | “删除后回到空状态”既撞空状态缺陷，也撞时序 |
| 1 | **产品缺陷 + 观察项** | 删除成功后仍 `reloadDetail()`，读一个刚被删掉的部署注定 404（日志 WARN + 控制台错误）；另有两条 503 见下 |
| 1 | 脚本的错 | 把登录前 `/api/v1/auth/session` 的 401 当成错误（那是预期探测） |

**两处页面缺陷（Q513）**：空状态丢标题（改成标题与说明都落成 `<p>`，`panels.test.ts` 补回归用例）；
删除分支不再明知故问去读已删的部署。**观察项（Q512）**：任务执行中两条 `GET /deployments/managed*`
在 10008 / 10025 ms 被面板掐断（代理侧同一时刻 `status 200 duration_ms 10020 / 10037`——代理答了，
只是答在 10 秒之后），报成 `AGENT_UNAVAILABLE` 是措辞不实；第二次跑（镜像已在本地）没有再出现。
预算不改，记进 KNOWN_LIMITATIONS C12。

**第二轮在最终字节上 43/43 全过**（`docs/evidence/P15/2026-10-28-p15.4/`）：登录 → 空状态 → 新建
（`nginx:1.27-alpine`，`127.0.0.1:18089`→80，128 MiB / 0.5 核）→ 预检 → 再认证 → 任务 → 列表
（`dep_…`、阶段读 `unit_state`）→ 详情（`sha256:…` 版本令牌、闭集、资源上限、上一版）→ 换镜像
（`nginx:1.28-alpine`，带回切）→ 详情刷新出新镜像并留着上一版 → 删除 → 回空状态。三次任务
`outcome: verified`（26 / 31 / 18 秒），面板侧 **57 条请求全是 200/202**，三次预检的请求体里没有
`compose_yaml` / `privileged` / `network_mode` / `cap_add` / `devices`。

**交付**：`web/scripts/deploy-smoke.mjs`（43 条断言；口令只从环境变量来；`UPANEL_SMOKE_TIMEOUT_SCALE`
只放大等待、不放宽断言；“已知非 2xx”单列打印、不算通过）。

**未验证**：200 份截断 / `skipped` / `config_drift` 三种现场仍只有单测；**“删除没动卷数据”这次只有
文案证据**（那份部署没挂卷，没有实物可证）；24.04 未跑；掉电语义仍 NOT RUN。

**收尾时又抓到一处日志误判（Q514）**：把读数固定在冻结字节上的过程中，面板日志里出现两条
`ERROR 会话校验失败 error="auth: 读取会话失败: context canceled"`（对外 500 `INTERNAL_ERROR`）与两条
`WARN …：代理不可用 error="… context canceled"`（对外 503 `AGENT_UNAVAILABLE`）。三个响应**没有人收到**——
登录后 SPA 并发拉三个接口，随即导航到部署页，浏览器把它们 abort 了；可日志写的是“服务端坏了”“代理不可用”。
排除过进程崩溃（`NRestarts=0`，同期无 panic）、代理预热（重启代理后 t+1s/t+2s 的 503 走的是另一条分支，
t+3s 起 45 次探测全 200）、稳定故障（同一会话 12 次全 200）。修了会话校验那一层
（`clientGone()` 只看取消、不看 `DeadlineExceeded`；取消时记 `WARN 客户端已断开，会话校验中止` 并写 499），
读查询那一层的“取消 vs 不可用”没动（判定散在 `agentclient` 与十个领域包里），记进 KNOWN_LIMITATIONS C13。
**修了代码就得重打字节，重打就得重跑**：七相位与页面链路都在重打后的最终字节 `3f3b40c1…` 上重取读数，
`b1d913e7…` 上那一轮的 155/0/3 与 43/43 留作可重复性旁证（Q510 的口径）。

- **受阻**：无。下一个子任务 P16.1（统一能力探测与安装/升级/卸载），已在下一节。

### P16.1（2026-10-30）— 能力探测只说它真读到的事；备份必须在动库之前

这一子任务有两半，STAGES §18 把它们写成一句话（"统一能力探测、安装/升级/卸载、两数据库和密钥
备份、失败可重入；兼容已有软件"），但两半的共同点才是重点：**面板说"这台机器能做什么"和面板
说"我备份好了"是同一种话——只能转述事实，不能替事实表态**。

**交付（一份实现、三种问法）**：`internal/hostcaps` 是唯一探测实现，三个出口读的是同一份事实——
`upanelctl host-caps`（安装器与"面板起不来"时的排障，`--json` / `--line` / `--timeout`）、代理
`GET /internal/v1/capabilities` 的 `host_caps`、面板 `GET /api/v1/system/capabilities`（admin，
只读快照）。探测**不在请求里跑**：代理启动探一次、每 10 分钟重探（`hostcaps.DefaultInterval`），
端点只读快照——一次 HTTP 请求不该去 fork 二十个 `--version`。

**交付（四条诚实性规则）**：
1. 只在这四个目录里找：`/usr/sbin`、`/sbin`、`/usr/bin`、`/bin`。**不走 `PATH`**——`PATH` 里可能是
   用户自己的 `apt` 包装、可能是 `~/bin/nginx`，那不是"这台机器装了 nginx"。
2. 每条只有 `present`/`absent`/`unknown` 三态，`unknown` **必须**带 `reason`（没权限、超时、退出码
   非零、读到的东西看不懂）。找不到不算 `absent`，除非真的确认四个目录都没有。
3. 版本只在**真读到**才填。`nginx -v` 把版本打在 stderr、`docker --version` 带前后缀——解析不出来
   就留空，不猜。
4. `host_caps: null` 是"还没探完"，**不等于空表**。前端 `hostcaps.ts` 里没有任何把 `null` 补成 `[]`
   的代码，也没有任何"可以/不可以"的判断函数——这一层只转述，不判决。
顺带删掉了 `capabilities.dependencies` 那个永远为空的字段：一个从来不会有内容的字段比没有更坏，
它会让人以为"没显示就是没装"。

**交付（规格表）**：21 条——systemd、journalctl、systemd-analyze、docker、docker.compose、nginx、
restic、ssh、ufw、ip、nfs、samba、apt、certbot、smartctl、sgdisk、mkfs.ext4、mkfs.xfs、findmnt、
shadow、sudo。每条都带 `feature`（它能支撑面板的哪个功能）与 `hint`（`apt` 提示）。登记标准是
"面板真的有代码路径依赖它"，不是"这台机器上可能有"。

**交付（`--line` 六列契约）**：`id <TAB> state <TAB> version <TAB> path <TAB> feature <TAB> hint`，
无表头、空字段表示"没有"、**禁用 `-`/`n/a` 占位符**，与 `--json` 互斥（同时给就 `exitUsage`）。
用途很具体：安装脚本跑在 POSIX sh 里，**没有 jq**。`install.sh` 第 3 步就是拿它当唯一依赖事实源
（`dep_field` 用 `awk` 按 tab 切，五个分支：present / absent / unknown / 空 / 意外值），
`installer_need` 只剩 `openssl`、`curl` 这两个安装器自用工具——它们不在 host-caps 表里，因为面板
本身不依赖它们。退出码只表示"探测有没有跑完"，**不表示"缺东西"**——缺席不是失败。

**交付（B 面：备份与可重入）**：`deploy/scripts/backup-state.sh`。三个技术必然决定了它的形状：
- SQLite 的 `-wal`/`-shm` 是活的，**必须停服务再拷**，否则拷到的是一份"看起来完整"的旧状态。
  脚本只停它在跑的服务，结束时（含中途失败，`trap ... EXIT INT TERM` → `restore_services`）起回去。
- `agent.key` 与两个库**必须同一份备份**：单个库拿出来是不可解的密文，分开存等于两份都不完整。
- 库位置从 `check-config --json` 读，**不写死路径**——为此新增了 `agent_database_path` 字段。配置里
  挪过库，备份就得跟着挪。
七步：前置检查 → 读生效路径 → 停服务 → `cp -a` 配置/证书/密钥（再 `chmod -R go-rwx`）→ 拷两库
（主库 + `-wal` + `-shm`）→ 起回服务 → `MANIFEST.sha256`（有 `sqlite3` 才顺手 `PRAGMA
integrity_check`）。可重入语义：默认 `/var/backups/upanel/<UTC>`（带时间戳，本来就不会撞上）、
目标非空即拒绝、**上次跑一半的目录（有 `.incomplete`、没有 `MANIFEST.sha256`）明确拒绝**并告诉人
`rm -rf` 或换目录——`die()` 的文案也会点名"换一个新目录重跑"。`.incomplete` 走到最后一步才删，
中途失败时留着，让下一个人一眼看出"这份不完整"。它**不进 MANIFEST**，否则清单里会有一个随后被
删掉的文件、`sha256sum -c` 会报缺失。

**交付（接线）**：`install.sh` 12 → 13 步（第 9 步在动库之前自动备份，失败即停；`--backup-dir`、
`--no-backup`）；`uninstall.sh` 在 `--purge-config` / `--purge-data` 之前**默认先备份**，缺
`backup-state.sh` 就中止（`exit 2`），没有任何可备份状态时明说"不备份"继续；`release.sh` 打包带上
它并自检执行位；`check.sh` 新增 `sh-syntax` 一关（对 `git ls-files` 里 `scripts/*.sh`、
`deploy/scripts/*.sh`、`tests/integration/*.sh` 逐个 `sh -n`）。卸载器不再说"删了就没了"，改成
"安装器与卸载器都会在动数据库之前备份"。

**交付（文档与页面）**：`api/openapi.yaml`（`GET /system/capabilities` 路径 + `SystemCapabilities` /
`HostCapsReport` / `HostCapsItem` 三个 schema，redocly lint 通过、6 个既有 warning）、CONTRACTS §3.1
（三出口表格、四条规则、缓存理由、21 条登记标准、`--line` 六列契约、退出码语义）、INSTALL（13 步表
+ "第 3 步为什么改成问 host-caps" + 升级/卸载重写）、RECOVERY §4.3/§7（恢复路径与新备份方式）、
DECISIONS Q497/Q498。页面：`web/src/views/HostCapabilitiesView.vue`（路由 `/settings/host`）+ 设置页
入口。页面上**没有任何安装/卸载按钮**——这一轮只交"探测"，不交"装东西"；刷新按钮写的是"只重读
代理已经探好的快照，不会触发新的探测"。

**已验证（宿主）**：`sh scripts/check.sh` 在最终工作树上 **PASS 15 / FAIL 0 / NOT RUN 0**（新增的
`sh-syntax` 一关 PASS，解析器 `/usr/bin/sh`）；因为 `.incomplete` 那几处是在全量 check 启动之后改的，
收尾又在最终工作树上把 `sh-syntax` 单跑了一遍：**77 个脚本、0 失败**（`git ls-files` 的同一份名单）。
`go test ./cmd/upanelctl/ ./internal/hostcaps/ ./internal/api/` 全绿（`api` 单包 159.97s），go 全包
test 全绿；`npx vitest run` 全量 **62 文件 / 1175 用例通过**，`vue-tsc --noEmit` 干净，`vite build`
成功，`web-smoke` 10/10。**构建与打包是真跑的**：`sh scripts/build.sh` → `VERSION=0.1.0-dev.1
ARCHES=amd64 sh scripts/release.sh` 产出 `dist/upanel-0.1.0-dev.1-linux-amd64.tar.gz`（sha256
`17ff6518…`、44135630 字节），`tar tzvf` 里 `install.sh` 34404 B、`uninstall.sh` 10943 B、
**`backup-state.sh` 12345 B 且三者都是 `0755`**——这条读数覆盖"打包真的带上了备份脚本、执行位在包内
没丢"，不覆盖"脚本在 Ubuntu 上跑得通"。宿主读数全部留档在
`docs/evidence/P16/2026-10-30-p16.1/HOST-READINGS.md`（含第 4 节 8 条 NOT RUN）。CLI 侧 6 个用例（`--json` 用 `DisallowUnknownFields` 验字段、`--line` 六列
无重复、三态与"禁占位符"、“无表头”用"第一行第一列等于 `hostcaps.ItemIDs()[0]`"断言而不是扫字符串、
`--json --line` 互斥、`--timeout 0` 拒绝）；前端 13 个用例（三态词表、`countHostCaps` 不把 `unknown`
并进"未安装"、空清单四个 0、`describeHostCapsItem` 四种情形、`hostCapsHeadline` 四种情形、
`host_caps: null` 不被补成 `[]`）。

**未验证（NOT RUN）**：这一轮**没有任何 Ubuntu 读数**。
- **三个部署脚本在这台 Windows 宿主上一次都跑不起来**，只有 `sh -n` 这一个读数。而且这里的 `sh`
  是 bash，**抓不到 bashism**——真机上如果某个脚本用了 `[[ ]]`、数组、`local -n`，`sh -n` 也会过。
  `check.sh` 的输出里已经注明了这句，不靠人记。
- `backup-state.sh` 的备份、恢复、可重入三条路径**一次都没真跑过**：停服务/起回服务、`-wal` 是否
  真的跟着走、`MANIFEST.sha256` 能不能 `-c` 过、`.incomplete` 在真实中断下留没留下。
- `install.sh` 第 9 步（升级前自动备份）与 `uninstall.sh` 的默认备份**没跑过**；`install.sh` 第 3 步
  改成读 `host-caps --line` 之后，在**装了东西的旧机器**上是否仍然放行（`unknown` 分支会不会误伤）未验
  ——这正是"兼容已有软件"那半句要求的东西。
- 能力探测页**没有真机读数、没有被人点过**，也没有 Playwright 用例（`web-smoke` 只走"未登录落到
  登录页"）。
- 24.04 / 22.04 的差异未验：`mkfs.xfs` 在 24.04 默认不装、`ufw` 在两台的包名相同、`samba` 的二进制
  名（`smbd` vs `samba`）——这些都只在单测里用假文件系统走过。

**本轮抓到的实现缺陷**（全是宿主自查，不是真机发现的）：`cmd/upanelctl/hostcaps.go` 里 `"os"` 导入
写了却没用到（`_ = os.Stdout` 那行删掉才编过）；`TestHostCapsLine` 最初用"输出里不含'状态'"断言
无表头，结果命中了 `feature` 列里的"防火墙只读状态与规则写入口"——改成断言第一行第一列；前端两处
`exactOptionalPropertyTypes` 违规（`version: undefined` 不能显式传、`new ApiError` 的 `code` 必须属
枚举）；`gofmt -l` 抓到 `internal/hostcaps/hostcaps.go` 未格式化（check.sh 的 gofmt 关抓到的）。
另外 `backup-state.sh` 的 MANIFEST 一开始没排除 `.incomplete`，会在成功瞬间生成一份"少一个文件"的
清单——这条是自己回头看出来的，没有留到 `sha256sum -c` 才炸。

- **受阻**：无。下一个子任务 P16.2（双平台端到端验收与资源/延迟测量）——注意 P16.1 的"未验证"里
  有一半（三个部署脚本、备份/恢复）必须在 P16.2 的真机上补，不能靠 `sh -n` 折算。

### P16.2（2026-10-27）— 七个相位在真机上走通；两个伪成功/版本差异缺陷是这一轮唯一的收获

**已实现**：这一轮没有新功能，只有两个**产品缺陷的修复**与一个**验收脚本/驱动**（都是被真机逼出来的）：

* **Q499**：`internal/journal/query.go` 的 `queryArgs` 用 `time.RFC3339Nano` 拼 `journalctl --since/--until`。
  systemd **249**（Ubuntu 22.04）不认 ISO 8601 的 `T` 分隔符（251 才认），于是 22.04 上**每一条**
  `/logs` 查询都 400——被拒的是服务端自算的默认窗口，日志页一条都读不出来；24.04（systemd 255）上一切正常。
  新增 `journalTimestamp()`：`2006-01-02 15:04:05.000000 UTC`（空格分隔、微秒精度、显式 UTC，避免客户机
  非 UTC 时区把窗口整体挪位），回归测试 `TestQueryArgsTimeIsJournalctlCompatible` 钉住字串形状。
* **Q500**：`deploy/scripts/install.sh` 第 13 步原来只 `warn` "代理 socket 不存在"，而代理单元是 `Type=simple`
  ——`systemctl restart` 在 fork 后就返回 0，一个立刻 `exit 1` 的二进制照样让第 12 步打印"已 enable 并启动"，
  于是坏包升级**打出 `==== 安装完成 ====`、退出码 0**。改法：第 13 步等 `agent.sock` + 单元 active 最多 30 秒，
  等不到就贴代理 journal 并 `die`（退出码 2）。`docs/INSTALL.md` 第 13 步同步说明。
* **验收驱动**：`tests/integration/p16-2-run-on-2204.sh`（宿主侧）——把包与脚本送进 VM、VM 内
  `sha256sum -c` 核对、按相位跑、`boot` 之前真重启、最后把 `facts/`/`logs/`/`RESULT-ALL` 取回；
  Q501 记的三个驱动缺陷（重启竞态、相位前不等 ssh、`run.log` 被截断）在这一轮修掉。
* **验收脚本**：`tests/integration/p16-2-acceptance.sh` 七相位（install / measure / boot / upgrade /
  failup / uninstall / purge），每相位写 `RESULT-<phase>` 并追加 `RESULT-ALL`；`measure` 顺带落一份
  `growth-files.txt`（60 秒空闲前后各一次文件清单），因为 22.04 那轮只留了目录总大小、答不出"是哪个文件在长"。

**已验证（真机，Ubuntu 22.04.5 / kernel 5.15.0-198 / systemd 249 / 2 vCPU / 3911 MB / 39 G，`-accel tcg`）**：

| 相位 | PASS | FAIL | NOT RUN |
|---|---|---|---|
| install | 40 | 0 | 0 |
| measure | 14 | 0 | 3 |
| boot（真重启之后） | 14 | 0 | 0 |
| upgrade | 22 | 0 | 0 |
| failup | 21 | 0 | 0 |
| uninstall | 23 | 0 | 0 |
| purge | 14 | 0 | 0 |
| **合计** | **148** | **0** | **3** |

真实读数（不是 mock）：安装 **30.0 s**；重启 → Web active **60 s**；同包重装（=升级）**49 s**；
RSS **web 288 MB / agent 25 MB**；`health` p50 **46 ms** / p95 53 ms，`system/summary` p50 **52 ms** / p95 63 ms（单线程 TCG，属保守读数）；
请求体 >1 MiB → **413**；`limit=5000` → **400**、`limit=1000` → **200**（单页 76 条 / 21039 字节）；
第 5 条并发 SSE → **429**，断开后名额释放；60 秒空闲里 journal **155 → 155** 行、代理库目录不增长。
`failup` 七条断言把 Q500 的修法钉死：坏包退出码非 0、打出`安装没有完成`、失败点在第 13 步
（`代理 30 秒内没有起来`）、点名 `AGENT_UNAVAILABLE` 且贴出 `journalctl -u upanel-agent`、
**没有**打出 `==== 安装完成 ====`；失败前备份已完成且 `MANIFEST.sha256` 可校验，重跑好包救回、
数据与密钥不变。

**浏览器那条 NOT RUN 已补测（两台）**：干净 cloud image 里没有浏览器，所以 `measure` 里记为 NOT RUN
（理由写在相位日志）。宿主机经 `ssh -L 9443:127.0.0.1:9443` 对着**同一台 VM、同一份包**跑 Playwright，
22.04 **PASS 35 / FAIL 0 / SKIP 0**、24.04 **同样 35 / 0 / 0**（登录闸门、登录后回到被拦截地址、
导航与 `implementedIn` 一致、概览真实快照、存储页设备卡片与详情深链接、无未捕获异常、
除 401/404/503 外无失败请求）。证据 `docs/evidence/P16/2026-10-27-p16.2-browser/`，
边界写在那个目录的 README，包括 24.04 上四次失败的成因：一次是我把隧道开在 9444 导致跨源 **403**
（产品行为正确），两次是登录超过前端 `timeoutMs: 15_000` —— 单线程 TCG 下 `argon2id m=128MiB`
一次登录烧 **9.5s CPU**（宿主 M3 上 128ms），于是卡在"产品给 15 秒、模拟器要 17 秒"的缝里。
这条**不进 P16.2 的通过项**：24.04 的登录时延要在硬件虚拟化/裸机上重跑才算验过（进 P16.4 已知限制）。

**已验证（真机，Ubuntu 24.04.5 / kernel 6.8.0-147 / **systemd 255** / 2 vCPU / 3914 MB / 38 G，`-accel 'tcg,thread=single' -cpu max`）**：
同一份发布包（sha256 `5216cb31…`）、同一份脚本、同一套断言，只换客户机。

| 相位 | PASS | FAIL | NOT RUN |
|---|---|---|---|
| install | 40 | 0 | 0 |
| measure | 14 | 0 | 3 |
| boot | 13 | **1** | 0 |
| upgrade | 22 | 0 | 0 |
| failup | 21 | 0 | 0 |
| uninstall | 23 | 0 | 0 |
| purge | 14 | 0 | 0 |
| **合计** | **147** | **1** | **3** |

两台的对照：安装 30.0 s → **56.3 s**、升级 49 s → **67 s**、`health` p50 46 ms → **81 ms**、
RSS web 288 MB → **290 MB**、日志单页 76 条 → **79 条**（`limit=1000` 都 200）、第 5 条 SSE 两台都 **429**、
60 秒空闲 journal 两台都不长（155→155 / 161→161）。**Q499 的修法在 systemd 255 上同样成立**——
那次修不是"给 249 打补丁"，而是写成了两个版本都认的形式；**Q500 的七条 failup 断言在 24.04 上也全绿**。

**那一条红（`boot`: `开机到 Web 可用不超过 120s: 152s`）的归因**：额外做了一次探针重启，
`btime → Web active` = **97 s**，其中 `upanel-web` 自身 `blame` = **6.255 s**、`upanel-agent` 9.0 s；
整机 kernel 17.3 s + userspace 131 s，头号慢项是 `snapd.seeded` 42.8 s、`snapd` 39.5 s、`cloud-init` 16.3 s。
即 152 s 里 96% 是**单线程 TCG 下的客户机引导**，`After=network-online.target` 只等了 2.1 s。
**判据不改、这条红照留**（120 s 对裸机是合理上限），定性为"判据在这种加速后端上不成立"，
裸机的"开机到可用"记为**未验证**并进 P16.4 的支持矩阵与已知限制。§18 要的"重启"本身是过的：
自启 + `enabled`、socket 回来、健康 200、证书/配置/密钥未被改写、管理员仍能登录、代理链路通——13 条全绿。
收尾给 `phase_boot` 加了一条 `NOTE`（打印两个单元各自的 `blame` 耗时，**只加读数**），
但上述读数不是它产出的，是探针单独测的。

**宿主检查（在改完五个文件之后的最终工作树上跑，2026-10-28 00:12）**：`sh scripts/check.sh` →
**PASS 15 / FAIL 0 / NOT RUN 0**。明细：`line-endings` / `sh-syntax` / `go-build` / `go-vet` / `go-test` /
`go-build-linux` / `go-vet-linux` / `gofmt` / `node-version` / `web-typecheck` / `web-lint` / `web-test` /
`web-build` / `web-smoke`（production 10 项）/ `openapi-lint`。`openapi-lint` 的 6 条 warning 与 P15
同一批，本轮未新增。**这不等于真机验收**：上面两个平台的读数各自有独立的证据目录。

**未验证（NOT RUN，逐条说明而不是折算）**：

- `measure` 里的**上传 2 GiB** 与**文本编辑 1 MiB**——属 P06 范围，本相位不造 2 GiB 文件，保留 NOT RUN 并指向 P06；
- **裸机的"开机到可用"**——单线程 TCG 下这个判据不成立（上面那条红的归因），真实硬件上的读数未取得；
- **空闲写放大**：22.04 那轮只留下"整目录 60 秒多出 313 KB、未查明"；24.04 这轮加了
  `facts/growth-files.txt`（空闲前后各一份目录清单）后才看清——**是 `upanel.db-wal` 在长**
  （490312 → 877592 B，主库文件 364544 B 一页没变、`-shm` 没变、代理库目录没变、journal 161→161）。
  也就是有周期写入者每 30 秒一轮（当时最像 `internal/operations` 的对账循环），本轮**没有抓到是谁写的**。
  不是泄漏（WAL 到 1000 页会检查点），但"空闲还在写"值得较真，列进 **P16.3**：空闲 5 分钟看 WAL 是否
  停在 1000 页内、`PRAGMA wal_checkpoint` 后是否回落、以及这些写有没有必要。
  **P16.3 已查明**：写的是 `upanel-web` 的 `metric_samples`（P13.1 的原始样本，5 秒一个点、每点 10 个指标），
  不是对账循环；WAL 停在 1002 帧（4.1 MB）不再长，`PRAGMA wal_checkpoint(TRUNCATE)` 能归零。
  见 `docs/evidence/P16/2026-10-27-p16.3/README.md` 第 2 节；`upanel-agent` 那份库在空闲时**一帧都没写**；
- 破坏性/大体积项（分区、格式化、网络断连、上传 2 GiB）不在这一轮，各有自己的阶段。

**未决**：`docs/TEST_ENVIRONMENT.md` §8.13 新增三条重启类约束 + 一条 seed server 约束（漏起 seed 会让
客户机带 `DataSourceNone` 白跑一整轮开机，24.04 第一次起机就这么废了 987 秒）；证据目录里三轮的分层
（`attempt1-scriptbug` / `attempt2-contaminated` / 正式那一轮）各自写了作废理由，不算"通过"。

- **受阻**：无。

### P16.2 补记（2026-10-28）— 最终字节上的双平台读数（各 155/0/3），以及一条假红的来路

字节这一环先交代清楚（Q510）：这一天包被换了四次，每次改动都重新取读数。最后一版是 `3f3b40c1…daf0`
（Q514 修了会话层的错误分类），**22.04 与 24.04 两台机器跑的都是这一个包**，两台都从校验过的 cloud image
重建系统盘、从零开始走七个相位。两份读数逐相位完全相同：install 47、measure 14、boot 14、upgrade 22、
failup 21、uninstall 23、purge 14 —— **155 PASS / 0 FAIL / 3 NOT RUN**（3 条 NOT RUN 按设计：上传 2 GiB、
文本编辑 1 MiB、浏览器主流程）。22.04 那一份在 `P16/2026-10-28-p16.2-final/`，24.04 在
`P16/2026-10-28-p16.2-2404-final/`。

**已实现**：产品代码在这一轮**一行未动**；动的是验收脚本里的一条断言（Q515）。24.04 首轮 `failup` 红了
`坏包让代理服务起不来（这正是失败现场）: 不该等于 'active'`，而同相位另外六条（退出码非 0、第 13 步 die、
点名 AGENT_UNAVAILABLE、贴出 journal、没有完成横幅、不是校验和失败）全 PASS —— 说明安装器判得没错。
从客机 journal 按微秒取那段窗口才看清：坏包是 `#!/bin/sh` + `exit 1`，进程活不过几毫秒，但单元是 `Type=simple`，
`Started` 一打就进 active，而"主进程 exit 1"要 **0.234~0.400 秒**之后才被记账（11 轮实测）；`Restart=on-failure` +
`RestartSec=3s` 又让单元每约 3.4 秒重来一轮 → **active 占空比约一成**，单次 `systemctl is-active` 有约 10% 概率正好
落在里面。改法不是放宽标准，而是换成两件不吃竞态的事：① `/run/upanel/agent.sock` 不存在（"面板用不了代理"的
直接判据，坏包永远造不出它）；② 隔 1.2 秒采两次，只有**连续两次**都是 active 才算真活着。断言条数不变（failup
仍 21），跨平台的逐相位数字才对得上。收紧后在同一份字节上重跑 `install,failup`：47/0/0 + **21/0/0**，那条输出
`socket 不在，is-active 采样 'activating'/'activating'`（两次都采在自动重启的间隙里，正是这个单元该有的样子）。
首轮读数没删，留在证据目录的 `run1-pre-tighten/`。

**已验证**：两台机器、同一份字节、七相位各 155/0/3；`boot` 相位的真重启在 24.04 上是 `boot_to_web_seconds=75`、
`upanel-agent` 6.160s / `upanel-web` 5.021s（`facts/boot.txt`）；安装耗时 22.04 为 29065ms、24.04 为 29764ms（后者是收紧断言后那次 `install` 相位的读数）。

**未验证**：

- **arm64 端到端仍 NOT RUN**：只打包，没有第二台 arm 机器。
- **掉电语义仍 NOT RUN**：`boot` 验的是"重启后单元能起来"，不是断电一致性；P16 的整条掉电演练没做。
- **裸机的"开机到可用"仍无读数**，并且 2026-10-27 那轮 24.04 的"152s > 120s"这一轮**没有复现**——两轮的 QEMU
  参数不同（那轮 `-accel 'tcg,thread=single' -cpu max`，这轮 `-accel tcg,thread=multi -smp 2 -m 4096`）。
  **因此不能说那条红已经修好**，只能说这套参数下没出现；判据不改，裸机读数继续记为未验证。
- **页面链路已在 24.04 上补齐**（2026-10-28 同日补跑，`P16/2026-10-28-p16.2-2404-browser/`）：在这台干净盘上
  装了同一份最终字节，`smoke.mjs` **35/0/0**、`deploy-smoke.mjs` **43/0/0**（首轮卡在预检，是"装面板之后才补
  docker、代理没重启"的启动时门，Q516；重启代理后同一份字节上全绿）。此前 43/43 的浏览器读数只取自 22.04。

- **受阻**：无。

### P16.2 补记之二（2026-10-28）— 24.04 的页面链路补到最终字节上（47/0/0 + 35/0/0 + 43/0/0），顺带抓到一个"装了 docker 却不生效"

补的是上一节末尾那条"未验证"：**从浏览器完成主要用户流程**（STAGES §18）在 24.04 上没有跑在最终字节上
（43/43 只在 22.04 有；35/35 那两份是 2026-10-27 的字节）。这一轮把两个平台这一格对齐，**没有重打包**——
包还是 `3f3b40c1…daf0`，逐字节相同，读数只属于它（Q510）。

**已实现**：产品代码本轮**一行未动**；`tests/integration/*.sh` 与 `web/scripts/*.mjs` 也一行未动。

**已验证（真机，24.04.5 / systemd 255 / 单线程 TCG，盘从基底镜像重建）**：

- 安装相位 47/0/0（`RESULT-install`）；
- 页面冒烟 `smoke.mjs` **35/0/0**（`smoke.log`）；`UPANEL_SMOKE_TIMEOUT_SCALE=4` 只放大等待，断言不放宽；
- 部署写链路 `deploy-smoke.mjs` **43/0/0**、`exit=0`（`deploy-smoke.log`）：新建 → 预检 → 再认证 → 任务 →
  详情 → 换镜像（1.27 → 1.28，`上一版镜像` 留着）→ 删除，跑完 `docker ps -a` 0 个容器、`docker compose ls`
  0 个项目、`/etc/upanel/deploy` 0 个文件、`upanel-deploy*` 0 个单元；
- 旁证：整轮跑完 `会话校验失败` 0 次、`panic` 0 次，`upanel-agent`/`upanel-web` 仍 active。

**抓到的问题（已登记，本轮不修）**：写链路第一次跑**红了**，红在 10 项之后的"获取预检"——客机 journal 是
`WARN 拒绝未登记动作的预检 action=deployment.apply` + `preflight status=400`。成因不是脚本：三条部署写动作
在代理**启动时**按 `firstExistingFile(deploy.DockerBinary)` 决定登记与否（`cmd/upanel-agent/main.go` 911—975），
而这台机器先装面板（`docker：absent`）后装 docker。**有意的启动时门**，装依赖正确顺序不踩；但同一处代码里
`hostCaps` 每 10 分钟重探、注释写着"装完一个包不必重启代理"，写动作的登记**不跟着重探**，于是能力页先说
docker 有了、预检才撞 400，日志 hint 也没写"装完重启代理"。修它要重打字节 → 按 Q510 两台 VM 全部重跑，
所以本轮只登记（Q516），现场处理是 `systemctl restart upanel-agent` 后在同一份字节上重跑 → 43/0/0。
首轮日志留档 `deploy-smoke-run1-docker-after-install.log`。

**未验证**：arm64 端到端、掉电语义、裸机"开机到可用"、`go test -race`——四条与上一节一样，本轮没有变化。
另外这一轮**没有**证明"面板装好之后再补 docker 能不经重启就可用"，恰恰证明它不能。

**受阻**：无。

### P16.3（2026-10-27）— 七个维度专项复核：一条"原子写"漏了 fsync（掉电后可能是全零文件），补上；另加一道整张路由表的权限守卫

**已实现**：

* **路由守卫用例**（新文件 `internal/api/router_guard_test.go`，127 条路由）。原先每个域都有"这条路由权限对"
  的用例，但**没有任何东西**保证"下一条新加的路由也带了中间件"——这类回归的后果是 200 而不是 403。
  三条表级断言：① 除 3 条公开入口（`GET|HEAD /api/v1/health/live`、`POST /api/v1/auth/login`）外，
  124 条路由匿名一律 **401 `UNAUTHENTICATED`**；② 所有非 GET/HEAD/OPTIONS 路由带会话但缺 `X-CSRF-Token`
  一律 **403 `CSRF_FAILED`**（并要求至少核到 20 条写路由）；③ 路由名不得出现 `exec/shell/command/sudo/raw/eval/sql`。
  公开入口写成显式白名单：加一条就要在评审里说清理由。
* **Q505：`internal/network` 两份 `writeFileAtomic` 补 `SyncDir` 与文件 fsync**。原来只有"临时文件 + rename"，
  内容与父目录都没刷盘。全仓其余写盘点（`accounts`/`schedules`/`scripts`/`shares`/`proxies`/`certificates`/
  `deploy`/`files`）全都 fsync，network 这一份是漏的而不是另一种风格。ext4 `data=ordered` 下 rename 进日志
  而内容还在页缓存时掉电，重启后可能看到**长度正确、内容全零**的 netplan 文件——远程管理的服务器掉电后
  起不来网，而回滚 timer 挂在"起网之后"那一侧，救不了这种。**八个调用点里还有回滚记录、unit 模板与 timer
  文件**，也就是安全网自己也走这条路：零长度的记录/Timer 正好是 P14.2 声称不存在的那种"改坏了没人管"。
  改法：`WriteFileSystem` 增 `SyncDir(dir)`、
  `osWriteFS.WriteFile` 改为 open/write/**Sync**/close、两份 `writeFileAtomic` rename 后刷父目录且**失败即报错**。
* **验收探针** `tests/integration/p16-3-wal-probe.sh`（客户机侧，只读）：从 `/proc/*/fd` 找持有库文件的进程、
  按表点行数、30 秒一个样本记四个文件体积，最后 `PRAGMA wal_checkpoint(TRUNCATE)` 与两个 pragma 读数。

**已验证**：

* **Q504 空闲写放大结案**（P16.2 移交）。22.04 真机、空闲 90 分钟的稳态上取样 11 次（5.5 分钟）：
  持有者是 `upanel-web` pid 769；`metric_samples` 913 → 1687（**+774**），其余表一行没变；`-wal` 长到
  **4,128,272 B 后停住**（= 32 + 1002×4120，`wal_autocheckpoint=1000` 的稳态上限），`TRUNCATE` 检查点
  返回 `(0, 1002, 1002)` 归零；`upanel-agent` 的库空闲时一帧没写。**P16.2 那句"最像对账循环"的猜测被推翻**，
  已在上一小节就地更正。容量折算留给 P16.4：约 140 行/分钟、约 30 MB/天原始样本，被 24h 保留窗口截住。
* **路由守卫会红**：两次变异（摘掉 `st.Use(s.requireSession())` / 改公开白名单）都让对应用例失败，恢复后转绿；
  `go test ./internal/api/` 全包 167.9 s 通过。
* **fsync 修复的路径验证**：`TestWriteFileAtomicSyncsDirectoryAfterRename`——正常时断言 rename 后刷过一次父目录；
  注入 `syncDirErr` 时必须在错误里带上目录名。变异验证：把 `SyncDir` 调用摘掉 → `FAIL … 实际刷了 []`，恢复转绿；
  `go test ./internal/network/` ok 0.770s，`go build ./...` 通过。
* **真机读数**（22.04.5，不是 httptest）：CSP/HSTS/XFO/nosniff/Referrer-Policy/COOP 六个头、单元 `User=upanel-web`
  与七条 hardening 指令、`agent.sock` 权限边界（普通账号直连 EACCES、`upanel-web` 连上）、匿名 401、
  未知 `/api` 路径 404、错方法 405、带外站 `Origin` 的 POST 403、缺 `X-UPanel-Request` 的登录 400。
* **七个维度逐条过完，除 Q505 外无阻断项**。四条**有意**的残余风险写进证据 README 第 7 节：
  登录限速是进程内计数（PLAN.md 第 5 节取舍）、SSRF 不做内网地址黑名单（管理员配的就是内网）、
  `metric_samples` ~30 MB/天、老内核无 `openat2` 时**不降级**（宁可功能不可用）。

* **宿主检查（改完 `internal/network` 与两份文档之后的最终工作树）**：`sh scripts/check.sh` →
  **PASS 15 / FAIL 0 / NOT RUN 0**；明细 `line-endings sh-syntax go-build go-vet go-test go-build-linux
  go-vet-linux gofmt node-version web-typecheck web-lint web-test web-build web-smoke openapi-lint` 全 PASS。
  其中 `go-test` 的 `internal/api` **152.875 s**（含新的 127 条路由守卫）、`internal/network` 通过；
  前端 62 文件 / 1175 用例；`openapi-lint` 那 6 条 warning 与 P15 同源，不是本轮引入。
  **这不是真机验收**——它只证明这台 Windows 宿主上的构建、静态检查与单测成立。

**未验证**：

* **掉电语义 NOT RUN**：fsync 修复只验证了"代码路径被走到、失败会被报出来"，没做真机断电演练（要拔电源）。
* 回滚 timer（`upanel-netrollback@`/`upanel-fwrollback`）的到期行为**本轮未重做**，读数用 P14.2/P14.3 的真机演练。
* SSRF 五处外连的"响应体上限"只做了静态阅读（`io.LimitReader` 三处）与超时/不跟随重定向的代码核对，
  **没有搭恶意对端做实测**。

**受阻**：无。

### P16.4（2026-10-28）— 1.0 的文档集与发布包：十份文档必须随包落地；顺手把 P16.3 的 fsync 修复在真机 ext4 上走了一遍

**已实现**：

* **六份交付文档**：`docs/USER_GUIDE.md`（新写：两种角色、四个阶段、每个模块怎么点、常见任务、
  出问题先看哪里）、`docs/RECOVERY.md`（改掉回滚的触发方式——**`start` 那个 `.service`，不是 `.timer`**——
  与记录字段名 `panel_file_content`）、`docs/SUPPORT_MATRIX.md`（六档证据分级、21 条 `host-caps`、
  btrfs 行点名 `FILESYSTEM_UNSUPPORTED` 的代码位置）、`docs/KNOWN_LIMITATIONS.md`（新增"0.1 → 1.0 升级
  未验证"一行；"面板没有用户管理页"补上"也**没有改密码入口**"）、`docs/RELEASE_NOTES.md`（§5 增一段
  证据状态：升级相位升的是 1.0 → 1.0）、`docs/EVIDENCE_INDEX.md`（P16 各行补齐）。
  P16.3 留下的"指标增长容量折算"落进 `KNOWN_LIMITATIONS`：5 秒一个原始点、24 小时保留 → **约 30 MB/天**。
* **README 交回"入口文档"的位置**：原来 255 行里有 190 行是 P00→P10 的阶段叙述加一整节"0.1 尚未实现的功能"，
  而 `release.sh` 会把它拷进每一个发布包。正文**按原样**（含当时的判断）迁到 `docs/DEVLOG.md`，前面加一段
  "这是历史，不是能力清单"；README 重写成 1.0 的入口（三条边界、装机器四步、十份文档索引、验收状态、
  开发命令）。同时修掉随包文档里指向**包外文件**的死链（`EVIDENCE_INDEX`/`PLAN`/`CONTRACTS`/`STAGES`/
  `DECISIONS`），明文标成"在仓库里，不随包发出"。
* **发布链严格化（Q507）**：`scripts/release.sh` 的文档列表 6 → **10 份**，缺任何一份 `exit 2`
  （原来是"有就拷，没有就算了"）；工作树有未提交改动时 `COMMIT` 追加 `-dirty`
  （包里的代码不等于那个 commit，按 commit 对源码会对不上）；`deploy/scripts/install.sh` 装机文档
  13 个文件（README + VERSION + 配置样例 + 10 份 `docs/`）、拒绝消息不再自称 0.1；
  三个部署脚本的头部注释同步。
* **验收断言**：`tests/integration/p16-2-acceptance.sh` 的 install 相位新增 7 条——六份关键文档必须在
  `/usr/local/share/doc/upanel` 里、且份数 = **13**。缺文档的包在这一步就红。
* **真机探针** `internal/network/fsync_real_test.go`（`//go:build linux`，默认 `t.Skip`，只在给了
  `UPANEL_REAL_FS_DIR` 时跑）：用生产的 `osWriteFS` 与 `ApplyEngine.writeFileAtomic`，在真机文件系统上
  验"rename 之后 `fsync` 目录 fd"这一步（Q505 加的正是它）。

**已验证**：

* **端到端七相位，跑在冻结的发布字节上**（amd64 包 sha256 `751a274d…95fba`、VERSION `1.0.0` /
  `commit: 8e52b36-dirty` / `build_date: 2026-10-28T03:59:13Z`；Ubuntu 22.04.5 / kernel 5.15.0-198 /
  QEMU TCG）：install **47/0/0**、measure **14/0/3**、boot **14/0/0**（`boot_to_web_seconds=56`，判据 120 s）、
  upgrade **22/0/0**、failup **21/0/0**、uninstall **23/0/0**、purge **14/0/0**，合计
  **155 PASS / 0 FAIL / 3 NOT RUN**，驱动退出码 0。证据 `docs/evidence/P16/2026-10-28-p16.4/`。
* **文档集真的落到机器上**：新增的 7 条断言全 PASS（六份点名 + 份数 13），这条链是"包里带文档 → 装到
  `/usr/local/share/doc/upanel` → README 说去看它"三处同时成立才算数。
* **发布包事实**：amd64 `751a274d…95fba`（44,149,944 B）、arm64 `c94ed4d5…352c6db`（40,945,025 B）；
  包内 `sha256sum -c SHA256SUMS` **21/21 OK**；
  tar 自己记录的权限是 `bin/*` 与 `deploy/scripts/*` **0755**、其余 0644（宿主是 Windows，解出来的
  `stat` 看不到执行位，这一栏必须以 `tar tzvf` 为准——本轮就差点被它骗过去）。
* **fsync 真机读数（补 P16.3 的缺口）**：`strace` 里看得到完整序列
  `openat(tmp,0600) → fsync(tmp) = 0 → renameat = 0 → openat(dir,O_RDONLY) → fsync(dirfd) = 0`，
  覆盖写再走一遍同样的四条，无残留 `.upanel-tmp`，内容逐字节一致、权限 `0600`，测试 `PASS`（0.13 s）。
  文件系统 `/dev/sda1 ext4`。**这证明写路径在真机 ext4 上走得通、目录 fsync 不返回 EINVAL/EBADF，不证明掉电语义。**
* **宿主检查（所有改动落定后的工作树）**：`sh scripts/check.sh` → **PASS 15 / FAIL 0 / NOT RUN 0**。
  `go-test` 里 `internal/network` 显示 `(cached)` 是正常的——新加的探针带 `//go:build linux`，在 Windows
  宿主上被构建约束排除，包的输入没变；这也意味着**它在宿主上永远不会被算成"通过"**。
* **第一轮（包 `fbad9fc9…`）也是 155/0/3 全绿**，但那之后文档复核抓出五处事实错误（Q509），字节重打，
  所以第一轮归档成 `prelim-docdelta/`，**只当可重复性旁证，不当最终读数**；两轮的差异只有 5 份文档的正文
  （三个二进制的字节数逐个相同）。

**未验证**：

* **掉电语义 NOT RUN**：目录 fsync 生效只等于"内核收到了这条命令"，不等于"掉电后一定是新内容"。拔电源演练仍没做。
* **0.1 → 1.0 升级未验证**：upgrade 相位升的是 **1.0 → 1.0**（同版本重装，覆盖备份/停服务/换二进制/迁移/
  起回/数据还在整条链）；从 0.1 的库升上来只有迁移脚本的单测。
* **arm64 只有打包与校验和**，端到端全在 amd64 上。
* measure 的三条 NOT RUN 未变（2 GiB 上传、1 MiB 文本编辑、VM 内的浏览器主流程）；**裸机读数仍然没有**。

**受阻**：无。P16.4 是 P16 的最后一个子任务，**P00—P16 到此全部完成**。

### P16 收尾补强（2026-10-28 → 10-29）— `-race` 全量第一次在 24.04 上跑完：产品代码零竞态，问题全在测试侧

**为什么补这件事**：从 P03 到 P16，账本里每一条"未验证"都写着同一句——**宿主没有 `go test -race`**
（Windows、`CGO_ENABLED=0`、没有 gcc）。P04.3 曾在另一台 KVM 机器上跑过整模块 `-race`
（19 包 ok），但那是 P04.3 的一小部分代码，之后又长了两百多轮。**1.0 定版之前必须有这一份读数。**

**已实现**：五个只动 `_test.go` 的修正，产品字节零变化。

* **Q517** `cmd/upanelctl/main_test.go`：删掉一条**产品从不满足**的断言（它断言人类可读输出里有
  "允许源"这一行，而产品只在 `--json` 的 `allowed_origins` 里给；这条用例带 Windows Skip，
  于是"唯一有人跑测试的平台上永远跳过、唯一能跑到它的平台上永远红"，从 P10.4 `5f29b0b` 起就红）。
  同义的 JSON 断言原样保留，而且它从此**第一次**在 Linux 上被执行。
* **Q518** `internal/backup/retention_test.go`：`/proc/self/fd/8` 硬编码 → `restoreTarget(t)` 真开一个目录取 fd。
  非 Unix 上 `dupRestoreFD` 是桩，所以这条用例在 Windows 上永远绿、在 Linux 上必然 EBADF。
* **Q519** 两处**测试侧**数据竞争：`scriptaction_test.go` 轮询 `src.ran`、`journal_test.go` 轮询
  `streamer.stream`，而被轮询的字段在另一个协程里写。各加锁 + 快照访问器，断言一条没动。
* **Q520** `internal/agent/operations_test.go`：预检过期夹具的余量 1 秒 → 60 秒。**断言一个字没改**
  （仍是 409 `PREFLIGHT_EXPIRED`）；依据是确定性判据实验（见下）。

**已验证**：

* **全量那一轮**（`-race -count=1 -timeout=90m ./...`，root，24.04 / 2 vCPU / TCG，墙钟 **3 小时 57 分**）：
  54 个包（51 个带测试）**44 `ok` / 7 `FAIL` / 3 撞超时**，**1 条 `WARNING: DATA RACE`**，
  撞超时的三个包是 `internal/agent`（5403s）/ `internal/api`（5407s）/ `internal/auth`（5405s）——
  panic 栈显示三处都在正常推进，**不是死锁**。
* **补跑**（`-timeout=0`，三包并行，墙钟 **9 小时 03 分**）：
  `internal/api` **`ok` 28929.178s（8 小时 02 分）、0 竞态、0 失败**；`internal/auth` 红 1 条、`internal/agent` 红 2 条；
  修过的那四个包 `cmd/upanelctl` / `internal/journal` / `internal/backup` / `internal/storage`
  **全部 `ok`**（310.760s / 27.336s / 11.673s / 17.025s）。
* **结论：产品代码零条数据竞争。** 全量那一轮唯一的 `DATA RACE`、以及补跑里爆出的每一条红，
  全部落在测试侧或被机器速度放大——**没有一条需要改产品**。
* **五个判据实验，全部写在证据目录里，上界一个没改**：
  ① `storage` 的 5 毫秒墙钟上界（全量轮量到 9.207787 ms）→ 带/不带 `-race` 各连跑 5 次，**10 次全过**，不复现；
  ② `backup` 的 600 毫秒握手预算 → 带 `-race` 1.25s、不带 0.98s，**都过**；
  ③ `TestDefaultPasswordParamsTiming` → **不带 `-race` 也红**（这台 TCG 机 3.061838022s，上界 1 秒；
   KVM 22.04 不带检测器是 0.6 秒），**判据量的是机器速度，不是参数变重**；
  ④ `TestWebhookSelfSignedCertIsRejected` → 隔离 3/3 过；人为加 4 个忙循环打满 2 条 vCPU 时 **1/3 复现**
  握手超时（服务端日志 `handshake error …: EOF`），机制坐实为"本机 TLS 握手被挤过 5 秒预算"；
  ⑤ Q520 的机理 → 在 `mutatingPlanStore.Get` 里人为插 1.5 秒延迟，**1 秒余量必红成 201、60 秒余量必绿**，
  预期先写死在脚本里再跑，与预期一致。改正后 root `-race -count=3` **3/3 全过**。
* **宿主检查**：`sh scripts/check.sh` **PASS 15 / FAIL 0 / NOT RUN 0**（见 §7 `P16-RACE-HOST`）。
  这一句写进账本时实际只跑出 **13 / 2**（系统 `node` v24 在 PATH 首位），把钉住的 `node 22.23.3`
  前置后重跑才是 15/0；两份原文都在 `docs/evidence/P16/2026-10-28-race/facts-host-check.txt`，
  先错后对的过程在证据 README §6.11 里没有删。

**未验证**：

* **arm64 上没有任何 `-race` 读数**（同 §8 那条：包能打能起，没有真机端到端）。
* **`-race` 只覆盖测试进程内的并发**。面板真跑起来之后（gin 路由 + 采集协程 + 对账协程）的并发形态
  模拟不到；这一条本轮反而抓到一个具体的旁证：空闲的 `upanel-web` 在这台模拟机上吃 **0.86 个核**，
  SIGQUIT 抓栈定位到历史监控的聚合回合（每 2 秒一次 `Pass`，含一次 `PendingMetricBuckets` 查询）。
  **真机上一个回合占多少 CPU，本轮没有读数**——不能把 0.86 个核说成产品常态，也不能反过来说
  "真机上肯定是毫秒级"。
* **`internal/api` 的 8 小时只有一次读数**，没有第二次独立复现。
* 这一轮的读数跨了两棵树（全量的 44 个 `ok` 用工作树原样，补跑的 7 个包用改了五处 `_test.go` 的那份）；
  两棵树的产品 `.go` 逐个相同（`find -newermt '2026-10-28 13:23:36'`：命中 5 个 `_test.go`、
  **非 `_test.go` 改动 = 0 个**），但"合计 0 竞态"这句话要带上这个前提。

**受阻**：无。

**这一条同时把账本里连续十几轮的那句"`go test -race` 仍未执行（NOT RUN）"关掉了**——见 §8 末条。

### P16 收尾补强（2026-10-29）— 核心验收矩阵逐项对照建立：31 条，已验 30 / 部分 1 / 未验 0

**为什么补这件事**：`docs/PROGRESS.md` §7 末尾挂着「矩阵编号与已有断言的对应关系仍未建立（这是账本的
一个缺口，不是产品的缺口）」。STAGES §19 有 31 条验收编号，§7 是一张按**子任务**编号的索引——
两套编号之间没有桥。后果很具体：说"1.0 完整交付"时，**没有任何一页能回答"§19 这 31 条分别靠什么撑着"**，
只能靠人翻 77 个证据目录。§1.6（PROGRESS 必须记录命令、环境、结果、证据路径）与 P16.4 的
"完整证据索引"都要求有这座桥。**缺的从来不是产品能力，是索引。**

**已实现**：新增 `docs/ACCEPTANCE_MATRIX.md`（产品代码零改动、零新读数、零新断言）。

* 31 行，一行一条 STAGES §19 编号，字段是「场景（照抄 STAGES）/ 状态 / 证据等级 / 证据位置（**目录 + 节**）
  / 关键读数」。**证据必须指到节**，写"见 P07"不算。
* 状态只有三档，写死：**已验**（该行全部子句都有真机读数）/ **部分**（至少一个子句只有单测或代码属性，
  或现场没造出来）/ **未验**。**按最弱子句定档**，不允许"验了两件、把第三件放进括号"。
* 总表之后是三条"部分"的展开：缺哪个子句、账本哪一行自己已经承认、要造什么现场才能补；
  外加 §2.4 一条**更正**（PROXY-02 起初被判成部分，复核是引错证据）。
* RELEASE-01 单独展开一节，把 P16 七相位逐个对到 STAGES §18 的五个验收动作
  （install 47 / boot 14 / upgrade 22 / **failup 21** / uninstall 23 / purge 14 / measure 14，
  合计 **155 / 0 / 3**）。
* 末节写清"证明了什么、没证明什么"：不证明 31 条就是全部风险；三条"部分"不能加总成已验；
  **跨阶段的读数不是同一份二进制**（P02—P15 各自当时的字节，只有 P16 那批是冻结的 `3f3b40c1…`）。

**已验证**：

* **45 条证据指针逐个用脚本核过存在性，0 缺失**（建表时的条数；`gaps/` 那一轮补了两条指针，重跑是 47 条、仍 0 缺失）（含 `{2204,2404}` 花括号展开、`RESULT-*` 通配、
  `:93` 行号后缀与 `…/` 省略写法的解析）。
* 每个引用到的 `§N` 都是从对应文件的 `== N. 标题 ==` 里读出来的，不是凭记忆写的节号。
* 关键读数逐个回原文件核过：P02.1 `PASS=58`、P02.2 `PASS=99`、P02.3 `PASS 84/0`、
  P03.2 `通过 101/0`、P04.2 `108/0/0`、P04.3 `77/0/1`、P06.1 `115/0/0`、P06.2 `228/0/0`、
  P06.3 `282/0/0`、P07.1 `189/0/1`、P07.2 `177/0/1`、P07.3 `204/0/1`、P07.4 `267/0/0`、
  P07.5 `205/0/3`、P08.1 `382/0/4`、P08.2 `237/0/0`、P08.3 `207/0/5`、P08.4 `190/0/4`、
  P09.2 `120/0/1`、P09.3 `217/0/1`、P09.4 `165/0/0`、P10.1 `100/0/0`、P10.2 `153/0/7`、
  P10.3 `176/0/7`、P11.2 两台各 `169/0/4`、P12.2 `170/0/3`、P12.3 `117/0/0`、
  P12.4 两台各 `246/0/0`、P12.5 两台各 `214/0/0`、P13.2 `148/0`、P14.2 `186/0/3`、
  P14.4 `141/0/6`、P15.2 `199/0/2`+`51/0/0`、P15.3 `229/0/1`、P16 七相位 `155/0/3`。

**这一轮抓出来的五件事（四件是账本以前写宽了，第五件是账本自己写坏了）**：

1. **DISK-02 不是已验，是部分**。"根盘在 LVM/RAID 下"的现场**从来没有造出来过**：
   `P04/2026-10-20-p04.2/result.md:93` 写着"两台 VM 只有 SCSI 盘与 loop，`dmsetup`/`mdadm` 未构造"，
   `P07/2026-10-21-p07.1/result.md:190` 写着"md 只在单测夹具里覆盖"。已验的是**系统盘及其分区一律拒绝**
   与**dm holders 能说清占用**——这两件事都成立，但都不等于"LVM 根盘上的递归保护"。根因是 `P00-VM-C`
   （LVM 根）从 P00 起就是 NOT RUN，STAGES 说"最晚 P04"，到 P16 结束仍未建。
2. **FILE-02 缺 ENOSPC**。`P06/2026-10-21-p06.2/result.md:233-234` 原话承认"没有真机证据"，
   理由是 VM 的 root 是 overlay、塞满会挂住宿主。超大上传那半也只是**按声明拒 413**，
   真推 2 GiB 在 `RESULT-measure` 里也是 NOT RUN。
3. **APP-02 没有真机 302**。metadata 地址拒过（`非 target 形状被拒（169.254.169.254）`），
   "候选只来自本机"是结构性的；但**重定向那一半只有"客户端不跟随"这条代码属性**，
   在 P09.2 的日志里搜不到 `302` 也搜不到 `Location:`。
4. **PROXY-02 起初被判成"部分"，是引错了证据（2026-10-30 更正为已验）**。当时依据是
   `P10/2026-10-23-p10.2/run.log` 的 NOT RUN 段原话「测试上游（python http.server）不支持 Upgrade」。
   那句话是**关于 P10.2 那个脚本**的，而 **P10.4 换了上游，在两台机器上都跑完了真握手**：
   `P10/2026-10-23-p10.4/logs/{2204,2404}-core-scenario.log` §4 各有
   `PASS WS 握手返回 101（Upgrade 穿过了 nginx）`，§6 wss 也是 101 + 12 条推流 + echo +
   TLS ≥ 1.2 + 对端指纹与账本一致，§8 重启之后 wss 仍 12 条。私钥那半仍是 P10.3 §10。
   **教训**：NOT RUN 是一张待办，不是一条定论——只查"有没有 NOT RUN 段"就会漏掉后来补上的读数。

5. **顺带发现账本自己的一处坏格式（已登记 Q521，未修）**。`docs/DECISIONS.md` 第 8 节的
   Q 表：表头（第 132 行）是 5 列，把 Q521 自身写进去之后共 **521 条** Q 行，按 GFM 规则
   （反斜杠转义的竖线算字面字符）逐行重算，渲染后 **5 格 458 条、4 格 1 条、6 格 54 条、
   7 格 4 条、8 格 1 条、9 格 1 条、10 格 2 条**——即 **458 条对得上、63 条对不上**。63 条分三类：
   ①**行内代码里的竖线没转义**的 16 条、共 33 处（`Q40`、`Q44`、`Q113`、`Q136`、`Q137`、
   `Q138`、`Q140`、`Q155`、`Q159`、`Q233`、`Q298`、`Q300`、`Q315`、`Q364`、`Q378`、`Q420`，
   写的是位或、枚举与管道命令）；②**有意写成 6 字段**的 46 条（`Q446`—`Q483`、`Q511`—`Q520`，
   P14.5 起把「内容」拆成 现象 / 判定 / 处理），表头一直没跟着改；③`Q14`（第 148 行）只有 4 格。
   GFM 按表头列数截断 → 那 46 条的**最后一格（处理 / 已修）在预览里被静默丢掉**，被劈开的
   16 条整段错位；原始 markdown 里字一个没少。**不修的理由**：先把列型定死再逐行转义，等于把
   521 行重排一遍，属于文档标准化任务，不在本轮子任务里；**修之前引用任何 Q 号都得读原文，
   别只信渲染结果**。本轮新写的 Q521 按表头的 5 列写。读数与脚本：
   `docs/evidence/P16/2026-10-29-matrix/check-decisions-table.py` → `facts-decisions-table.txt`。

**未验证**：这一轮**没有产生任何新的真机读数**——它是索引与定档工作，不是验收工作。
上面三条"部分"的缺口原样保留，一条都没有因为写进表就变成已验。（PROXY-02 不在这三条里：它是 10-30 复核出的**引错证据**，改的是判定，不是读数。）

**受阻**：无。（四条缺口各有明确的补法：建 VM-C 造 LVM 根、拿独立 tmpfs/loop 塞满试 ENOSPC、
起一个 302 上游、换一个支持 Upgrade 的上游。它们都不需要新代码能力，只需要现场。）

**这一条把账本里挂了十几轮的"矩阵未逐项对照"关掉了**——§7 那句"仍未建立"改成已建立并保留历史原话。

### P16 收尾补强（二）（2026-10-29）— 把"部分"逐条去补：两条补成已验，一条仍是部分

**为什么补这件事**：上一节把矩阵建起来了，账本因此第一次指明了三条"部分"各自的缺口是什么、要造什么现场。
缺口既然能一句话说清，就不该再挂着——**去造现场，不写新的解释。**

**已实现**：新增验收脚本 `tests/integration/p16-4-matrix-gaps.sh`（可重跑）。
**产品代码与 `_test.go` 零变化**——这一轮只加了一个验收脚本。

**已验证（真机，`P16/2026-10-29-matrix/gaps/2404-matrix-gaps.log`，`PASS 80 / FAIL 0 / NOT RUN 0`）**：

* **APP-02｜302 不跟随**。三个真上游：19491 回 `302 → 127.0.0.1:19493/stolen`、19492 回
  `302 → 169.254.169.254/latest/meta-data/…`、19493 是金丝雀（每次命中写一行）。
  读数：`http_state=ok`、`status_code=302`、`address=127.0.0.1`；**金丝雀命中 0 次**；
  两个上游各被探 17 次（30 秒一拍，探了整场）。指向 metadata 的那条记录里连 `169.254` 都搜不到。
* **FILE-02｜ENOSPC 真的撞上了**。128 MiB tmpfs 当可写根 → 声明 8 MiB（当时可用 131064 KiB）→
  填到只剩 512 KiB → PUT → **413 `VALIDATION_ERROR`「files: 空间不足: 磁盘空间不足，暂存已删除」**；
  `.part` 被删、记录留 `failed`、`received_bytes` 归零、**已有文件 sha256 未变**、代理日志有 WARN。
* **FILE-02｜断流**。`curl --limit-rate 1M --max-time 3` 掐在 3 秒：上传进 `failed`、暂存清掉、
  目标不存在；代理记「传输中断（已写入 3145728 字节）：unexpected EOF」——**喂了多少字节是读数，不是估的**。
* **FILE-02｜2 GiB 真推一遍**。`max_upload_bytes` 设 3 GiB，推 2 GiB：`200`、
  `received_bytes=2147483648`、**峰值 RSS 28740 KiB**（0.5 秒采样取峰值，判据 < 2 GiB 的 1/4）、
  落位 `outcome=verified`、落盘 sha256 与载荷一致、记录随落位消失、再查是 404；
  `max_upload_bytes+1` 仍是 413。

**这一轮抓出来的四件事**：

1. **`uploadReserveMargin = 64 MiB`**（`internal/files/staging.go:52`）。容量预检是
   `available < already + want + 64MiB`，所以**小于 64 MiB 的文件根永远声明不了上传**。
   第一版脚本用 16 MiB 的 tmpfs，在声明阶段就被 413 掉，根本走不到写入——**测不到 ENOSPC**。
   这不是 bug，是"预检说够不代表刚好够"的取舍；但它决定了 ENOSPC 现场的最小尺寸，记下来省下一个人的时间。
2. **2 GiB 不能用 `curl --data-binary @file`**：curl 会先把它读进内存，直接 `out of memory`。
   `-T file` 才是流式。更要紧的是——用 `--data-binary` 的测试**根本测不出服务端是不是流式**，
   因为它自己先不流式了。
3. **判据"值非空"会让断言变绿而不是变红**。第一版健康轮询写成"`checked_at` 非空且不是 None"，
   而取值函数在缺键时返回的是 `ERR:KeyError:'data'` —— 一个非空字符串，于是 404 被当成"探到了"。
   现在的判据显式排除 `ERR:` 前缀。**这类缺陷的危险方向是反的：它不制造红，它制造假绿。**
4. **302 记成 `ok` 不是漏判**。`302` 在默认成功集合里，含义是"这站点能连上、用 302 回答"；
   面板如实记状态码，不替人判断跳转目标好不好。APP-02 期望的"拒绝"拒绝的是
   **向 `Location` 里的地址发出请求**，不是"拒绝承认 302 存在"。两件事混起来就会把正确行为读成缺陷。

**未验证 / NOT RUN（与本轮通过项分开记，别混着读）**：

* **DISK-02 的 LVM/RAID 根盘：脚本明确不覆盖，矩阵里仍是"部分"。** 它是唯一还欠着的一条。
* **真并发竞态**：FILE-02 的"并发编辑"是用强制交错复现的（外部改 → 提交 → `PREFLIGHT_STALE`），
  不是真竞态。STAGES 的期望里没有这一条，但它仍在 `docs/KNOWN_LIMITATIONS.md`。
* **根文件系统上的 ENOSPC**：只在专用 tmpfs 上压过。`P06.2` 当初不压根盘的理由（"塞满会把宿主一起挂住"）
  仍然成立，没有被这一轮推翻。
* **性能**：292 秒传 2 GiB 是单线程 TCG 的数字，不作任何性能结论。

**受阻**：无（本轮）。DISK-02 的下一步有明确的阻碍点：`H:\Code\vms\images\` 只有 cloud image、
**没有 Ubuntu server installer ISO**，`layout: lvm` 那条 subiquity 安装路径走不通；
要造 VM-C 就得在客机里把根迁到 LVM（第二块盘 → `pvcreate`/`vgcreate`/`lvcreate` → mkfs → rsync →
chroot `grub-install` → 改 fstab → 用 LV 盘当系统盘重启），再跑 P07 式的回避/递归保护验收。
这条路没有走过，**不预判它一定成**。

**这一轮之后**：矩阵 31 条 = **已验 30 / 部分 1 / 未验 0**。（这是建表当时的样子；同一天的下一节
把最后那一条也关掉了，现在的合计是 **31 / 0 / 0**。）

### P16 收尾补强（三）（2026-10-29）— 把最后一条"部分"关掉：两台根在叠层设备上的客机

**为什么补这件事**：上一节的结尾只剩 DISK-02 一条"部分"，缺的不是代码，是**现场**——"根盘在
LVM/RAID 下"这件事从 P00 起就没造出来过。缺口既然一句话能说清，就去造现场。

**已实现**：新增验收脚本 `tests/integration/p16-5-root-disk-recursion.sh`（可重跑，**同一份脚本认两种
拓扑**：读 `dm/uuid` 前缀判 LVM、读 `/sys/block/<id>/md/` 判 RAID，期望的锁从拓扑算出来）。
**产品代码与 `_test.go` 零变化。**

**现场是在客机里迁出来的，不是装出来的。** `H:\Code\vms\images\` 只有 Ubuntu cloud image、没有
server installer ISO，subiquity 的 `layout: lvm|raid` 走不通。两台的造法都是：第二/三块虚盘 →
`pvcreate`/`vgcreate`/`lvcreate` 或 `mdadm --create` → `mkfs.ext4` → `rsync` 整个根 → chroot
写 fstab / `update-initramfs` / `update-grub` / `grub-install` → **摘掉系统盘重启**。

| 场 | 客机 | 根设备 | 成员 | 物理盘 | 读数 |
|---|---|---|---|---|---|
| LVM | `upanel-lvm` | `/dev/mapper/upanel--vg-root`（`dm-0`） | `sda2` | 1 块 | **91 PASS / 0 FAIL / 3 NOT RUN** |
| RAID1 | `upanel-md` | `/dev/md0` | `sda2`+`sdb2` | 2 块 | **113 PASS / 0 FAIL / 3 NOT RUN** |

**已验证（真机，`P16/2026-10-29-matrix/disk02/`）**——这一条到底证明了什么，拆成四个能读的子句：

* **递归真的发生了。** 每块物理盘 `protection.sources` **只有**根设备的 id，而它自己 `filesystem`
  是 `null`（不是挂载点、不是交换区、不承载文件系统）：LVM 是 `sda → ["dm-0"]`，RAID 是
  `sda`/`sdb → ["md0"]`。**那个 `system_disk=true` 不可能是"它挂了 /"得来的。**
* **反证把"沿链递归"和"按盘染红"分开了。** `sda1`（LVM 场）与 `sda1`/`sdb1`（RAID 场）都是同一块盘上的
  bios_grub 分区、同样没有文件系统，但**不在根链路上，所以 `false`**。
* **叠层各层都被拒。** 整盘 / 成员分区 / 根设备本身四条写动作预检全 `allowed=false` +
  `SYSTEM_DISK_PROTECTED`，理由里说得出"系统数据"。
* **一个字节都没写，而且不是因为缺工具。** 整盘首末 1 MiB 哈希前后一致、`LVM2_member` /
  `linux_raid_member` 签名还在、sysfs 身份没变、零存储写任务；代理启动日志里有「磁盘写原语已接线」
  （`sfdisk`/`udevadm`/`mkfs.ext4`/`mkfs.xfs`），且没有「动作执行失败」「执行前复核被拒」
  「动作缺少执行实现」。
* **递归没有过宽。** 4 GiB 空白备用盘（LVM 是 `sdb`，RAID 是 `sdc`）：不受保护、`initialize` 能力
  `available`、预检 `allowed=true`、锁只有它自己。

**这一轮抓出来的四件事**：

1. **一台单盘机验证不出"锁该锁谁"。** 第一版脚本把锁断言写成"每一块底层盘"。LVM 场只有一根 PV，
   「锁目标自己那块盘」与「锁住整台机所有物理盘」**在每一条断言上恰好同值**，所以 `91/0/3` 一路绿。
   RAID1 两块成员盘上一跑立刻红 5 条：期望 `disk:sda,disk:sdb`、实际 `disk:sda`。查代码不是产品错——
   `internal/storage/assess.go:761` 的 `chain()` 从**目标**出发沿 `parents` 往上追，CONTRACTS:279 写的
   也是"沿 `depends_on`/`partition_of` 递归追踪"，CONTRACTS:424 对单动作写的就是「整盘
   （`disk:<disk_id>`）」。**锁是互斥用的，保护看 `SYSTEM_DISK_PROTECTED`；混成一件事就会把正确行为
   读成缺陷。**（Q522）
2. **改法是换断言，不是删断言。** 5 条改成按目标自己的链算（新增 `lock_for()`，走 sysfs 判分区还是
   整盘），另**加一条更严的**：物理盘多于一块时，单盘目标的锁**不许**含别的盘——太窄和太宽都夹住。
   第一场那份红读数留在 `disk02/raid/runs1-pre-fix/`，不删。
3. **`/boot` 必须留在叠层设备里面。** 如果 `/boot` 单独占一个分区，物理盘受保护就可能来自"它挂着
   `/boot`"，递归就没被证明、只被绕过去了。两台都让 `/boot` 跟着根走，物理盘上唯一不属于 LVM/RAID
   的东西是那 1 MiB bios_grub。
4. **RAID1 的 MBR 不是冗余的。** 阵列的引导扇区不在阵列里，在两块盘各自的 MBR 上——`grub-install`
   必须对**每一块**成员盘各跑一次，只装一块的话那块盘坏了就开不了机，而 RAID1 的全部意义就是
   "那块盘坏了还能开"。

**宿主检查（改完脚本后重跑，不是建表那次）**：`sh scripts/check.sh` → **PASS 15 / FAIL 0 / NOT RUN 0**
（`docs/evidence/P16/2026-10-29-matrix/facts-check.txt` 已换成这一份）。与建表那次比只有两个**计数**变了：
`line-endings` 157 → 162（扫全仓 `*.sh`/`*.sql`/`*.service` 且含未跟踪文件，`disk02/` 归档的 4 个探针
脚本算进去了）、`sh-syntax` 81 → 82（现在 7 + 3 + 72 = 82 个全过 `sh -n`）。前端仍 62 文件 / 1176 用例。
**建表那次 check 跑在改脚本之前，按 Q510 那份读数绑的是旧字节，所以整跑一遍覆盖掉，不拿旧读数配新字节。**

**未验证 / NOT RUN（与通过项分开记）**：

* **降级 / 重建中的 RAID**（成员 `faulted`、`rebuild` 进行中）：现场没造。递归不看阵列健康，
  但没有真机读数就不算验过。
* **根在叠层设备上写满（ENOSPC）**：把根写满会伤到这台机的系统盘，不做。P16.4 的 ENOSPC 现场是
  独立 tmpfs。
* **执行期复核**（`PREFLIGHT_STALE` / `DEVICE_IDENTITY_CHANGED`）：预检就拒了，提交门到不了；
  那两条由 P07.2 在一次性盘上验过，与根在不在叠层设备上无关。

**受阻**：无。

**这一轮之后**：矩阵 31 条 = **已验 31 / 部分 0 / 未验 0**。P00—P16 全部完成。

### 补强（四）（2026-10-30）— 清单里一行旧话被后来的证据推翻，改对

**起因**：回答"能不能用了"时逐条核 `KNOWN_LIMITATIONS.md`，发现一行自相矛盾——A 节写着
「24.04 的 systemd 事务与 compose 措辞差异 | **未验证** | 部署相关的真机读数全部来自 22.04」，
而 `docs/EVIDENCE_INDEX.md` 里 24.04 那两轮明明有部署读数。

**成因不是笔误，是时间错位**：那句话是 P15.2 当轮写的（当时确实只有 22.04.5 一台机器的
部署读数，见 §5 P15.2 小节的第 ④ 条）。后来的 P16.2 24.04 轮把七相位和浏览器写链路都补上了，
没有任何人去回头改这句。

**已修**：

* `docs/KNOWN_LIMITATIONS.md` 那一行改成 **已补（真机，2026-10-28）**，并把读数写全：七相位
  `155 / 0 / 3`（install 47 / measure 14 / boot 14 / upgrade 22 / failup 21 / uninstall 23 / purge 14，
  `P16/2026-10-28-p16.2-2404-final/`）、部署写链路 `43 / 0 / 0`（`…-2404-browser/`），客机 systemd
  `255.4-1ubuntu8.18`、docker 29.1.3、docker-compose-v2 2.40.3。**docker 与 compose 两平台同版本**，
  所以"compose 版本差异"这个担心没有对象。24.04 上仍没做的写清楚：部署的故障注入（`systemctl mask`
  造"卷不可用 → 起不来"、钉错 digest、容器刚起就崩），那些在 22.04 上也是单开一轮做的。
* §5 P15.2 小节第 ④ 条**原文不改**，末尾加一句前向指针，说明它说的是当轮口径、后来的 24.04 轮
  补上了什么。历史段不重写，只加路标。
* `docs/DECISIONS.md` 记 **Q523**（文档缺陷：清单条目与证据集合不一致；改法与"不动历史段"的取舍）。

**验证**：`python docs/evidence/P16/2026-10-29-matrix/verify-pointers.py` → `检查 51 条路径；缺失 0`；
改过的四份文档（本账本、`DECISIONS.md`、`KNOWN_LIMITATIONS.md`、矩阵 README）都是纯 LF；
`check-decisions-table.py` 重跑：**523 条 Q 行、5 格 460 / 不是 5 格 63**，新加的 `Q522`、`Q523`
都是 5 格（脚本里写死的"521 条"也修了，见 Q521 末段）。**没有重打字节**——这一轮一个字没碰
`cmd/`、`internal/`、`web/src/` 与 `deploy/`，所以 `3f3b40c1…daf0` 那份交付物与两台 VM 上的读数
仍然成立（Q510）。

**未验证 / NOT RUN**：部署故障注入在 24.04 上（同上，那类现场连 22.04 都只做过一轮，
登记在 `KNOWN_LIMITATIONS.md` A 节「部分真机造不出的现场」）。**受阻**：无。

### 界面重做（2026-10-30）— 桌面式外壳 + 设计系统（纯前端重写，后端与契约零改动）

**起因**：负责人三句话——「UI不行，太简单了」、「能不能设计成像群晕或飞牛等 NAS 系统那种桌面版，这样
操作对新手来说更简单」、「那你开始设计这个 UI 系统吧」。**这一轮不是 STAGES 的子任务**（P00—P16 已
全部完成），是负责人直接下的前端工作，所以不计入矩阵、不改验收状态。

**边界先说清**：`cmd/`、`internal/`、`api/openapi.yaml`、路由表、DTO、状态机、动作注册表**一行没动**；
16 个模块的可实现口径按 PLAN §7 不变（`containers` 仍然不出现在 dock 与启动台里，因为 `implementedIn`
是 `null`）。改的全部在 `web/src/`。

**已实现**：

| 文件 | 作用 |
|---|---|
| `web/src/shell/icons.ts` | 16 个模块的 24×24 描边路径表 + 兜底图标；`moduleIconPath()` 未知键走兜底 |
| `web/src/shell/nav.ts` | 从旧 AppShell 里抽出来的高亮判定：`meta.moduleKey` 优先，回落到路径前缀匹配 |
| `web/src/shell/DesktopBar.vue` | 顶栏：品牌按钮（开启动台）、当前模块面包屑、时钟、主题三态、账号与退出 |
| `web/src/shell/AppDock.vue` | 底部固定程序坞：全部 + `visibleModules()`，选中态是 2px 主轴色线 |
| `web/src/shell/AppLauncher.vue` | 启动台：`role="dialog"` 遮罩层，卡片网格带模块摘要与「当前」标记，Esc / 点遮罩关闭 |
| `web/src/shell/AppShell.vue` | 组合根：桌布（点阵 + `background-attachment: fixed`）→ 顶栏 → 单个内容窗口 → 程序坞 → 启动台 |
| `web/src/ui/ModuleIcon.vue` | 图标组件，`aria-hidden`、`focusable="false"` |
| `web/src/components/StatTile.vue` | 换成 token 化的纸面样式，数字用 `tabular-nums`（**不用 mono**，否则 CJK 值会掉字体） |
| `web/src/components/SystemOverviewPanel.vue` | `NCard`/`NTag` 换成 `UPanel`/`UTag` + 细线分段；`data-testid` 与全部错误分支文案逐字保留 |
| `web/src/components/StatePanel.vue` | 新增 `compact`（**纯样式开关**，文案一字不改；与标题重复的「状态：…」行在其下隐藏） |
| `web/src/components/ApiErrorPanel.vue` | `NAlert` 换成本地红框（左边框 3px）；`role="alert"`、`data-testid="api-error"`、code/request_id 全保留 |
| `web/src/views/OverviewView.vue` | 4 张 KPI（CPU / 内存 / 整盘容量 / 需要处理）+ 1.75fr:1fr 双栏；数据逻辑**逐字未改** |
| `web/src/App.vue` | 一行：`import AppShell from '@/shell/AppShell.vue'` |
| `web/scripts/preview-shell.mjs` | 只给宿主看图的预览脚本：拦 `**/api/v1/**`，回放 P04.4 真机抓下来的 `summary`/`devices` 夹具 |

**这一轮做的三个判断，都记进了 `docs/DECISIONS.md`**：

* **Q524 不做假窗口。**「像 NAS 桌面版」最容易走偏的一条是画一堆带最小化/关闭的窗框。仓库里 23 个视图
  全都依赖 `useRoute()`/`useRouter()`，而 `<RouterView>` 只能挂一个；真要并排多个窗口，就得给每个窗口造
  一份伪路由上下文——那是**改视图契约**。所以选**单内容窗口 + 启动台 + 程序坞**：新手要的是「点得到、
  看得懂在哪」，不是能拖动的框。这轮没有一个装饰性的最小化/关闭按钮。
* **Q525 颜色存在两份，靠测试夹住。** `tokens.css` 与 `PALETTE` 各有一份色值，`src/ui/tokens.test.ts`
  断言「暗色值必须恰好出现两次」当防漂移闸；同时记下已知脆弱点：外部强写 `<html data-theme>` 会绕过控制器。
* **Q526 `StatePanel compact` 是开关不是新组件**，避免出现两套会各自漂移的文案。**Q527 预览脚本回放夹具，
  明确不算验收**，`scripts/` 不进 `dist`。

**已验证（宿主，不是 Ubuntu 验收）**：`.toolchains/node-v22.23.3`（系统 node 是 v24，会出假红，仓外那份
才是钉住的），`docs/evidence/UI/2026-10-30-shell/`：

* `npx vitest run` → **66 文件 / 1219 用例全过**（含新加的 `nav.test.ts` 10 条、`icons.test.ts` 4 条、
  `panels.test.ts` 里新增的紧凑版 2 条）。
* `npx vue-tsc --noEmit` → `exit=0`；`npx eslint .` → `exit=0`（**注意**：本仓 ESLint 只配了 `.ts`/`.mjs`，
  `.vue` 是 `File ignored because no matching configuration was supplied`，模板里的错误类型检查抓不到）。
* `npm run build` → 成功，`dist/` 只有 `index.html` + `assets`（`design.html` 与预览脚本都没进产物）。
* 五张图：`shell-overview` / `shell-launcher` / `shell-dark` / `shell-storage` / `shell-mobile`，
  1440×900 与 400×820、明暗两套，`pageerror` 零、`scrollW == clientW`（无横向溢出）。
* `sh scripts/check.sh` 整跑 → **PASS 15 / FAIL 0 / NOT RUN 0**（`facts-check.txt`，两次独立跑同值）。
  **别把它读大**：其中的 `web-smoke` 走的是**未登录**路径、断言落在登录页，**没有覆盖**新的顶栏 /
  程序坞 / 启动台 / 概览页——那部分只有夹具截图。"冒烟过了"不等于"外壳验过了"。

**未验证 / NOT RUN（与上面那串分开记）**：

* **没有一个字节跑在 Ubuntu 真机上。** 这轮的图全是**宿主 Chromium 回放抓下来的夹具**，不是活着的面板。
  按老规矩：**夹具截图不算界面验收**。要算，得在验收 VM 上装好、真登录、真点一遍。
* **只看了 Chromium（Playwright 1.64.1）**。Firefox / Safari / Edge 一次没开；`backdrop-filter`、
  `100dvh`、`background-attachment: fixed` 三处最可能在别家浏览器上不一样，没有读数。
* **无障碍只做了两处手工动作**：启动台焦点移入 + Esc 关闭、图标 `aria-hidden`。**没跑 axe**，
  没有屏幕阅读器实测，对比度没量（暗色模式下 `--ink-3` 在 `--surface` 上的比值是多少，这轮没有算）。
* **没碰过触摸屏 / 平板**。400px 只是把窗口缩窄，和真手指是两件事；程序坞 `overflow-x: auto` 在触屏上
  的惯性滚动没试。
* **其余约 20 个视图还是 naive-ui 原件**，只是跟着 token 换了配色，没有逐个改成 `U*` 组件。这一轮只把
  概览页一条链路（含它用到的 5 个组件）走完，剩下的是**外观不完全统一**，不是坏。

**API / 数据库变化**：无。没有新接口、没有新字段、没有迁移。

**已知问题·阻塞**：无阻塞。已知问题全部在上面的「未验证」里，性质是「覆盖面不够」，不是「做错了」。

**下一步**：按同一套组件把其余视图逐页替换（建议顺序：存储 → 备份 → 服务，这三页信息密度最高、
最容易暴露排版问题）；真机 UI 验收要等下一版重打字节后一起做（Q510）。

### 界面重做（续）（2026-10-30）— 25 页逐页收口（纯前端，后端与契约仍然零改动）

**起因**：上一轮只走完概览页一条链路，其余约 20 个视图只是跟着 token 换了配色。负责人一句
「继续，不要停下来，直接全部完成能交付给我测试」——把剩下的页全部截一遍、逐张看、改到能看。
**仍然不是 STAGES 的子任务**，不计入矩阵、不改任何验收状态。

**边界**：`cmd/`、`internal/`、`api/openapi.yaml`、路由表、DTO、状态机、动作注册表**一行没动**；
PLAN §7 的模块可见性口径不变。改的全部在 `web/src/` 与 `web/scripts/`。

**已实现**：

| 文件 | 作用 |
|---|---|
| `web/src/styles/naive-skin.css` | 整份重写。**只用直接属性**（`border-radius`/`padding`/`font-size`/`letter-spacing`），一条 `!important` 的 `--n-*` 覆盖都不留；按钮那一格**故意空着**并注明原因 |
| `web/src/ui/theme.ts` | 按钮几何（26/28px 身高、`0 12px` 内边距、2px 方角）从 CSS 移进来；新增 `Input` 的圆角/边框/聚焦环；`Progress.railColor` 换成 `--rule` |
| `web/src/styles/views.css` | 页面骨架选择器收窄成 `.win > section, .win > main`（原 `.win section` 连 `UPanel` 的 `<section>` 根一起套上了）；新增页头操作按钮右对齐规则 |
| `web/src/ui/UKpi.vue` | 新增 `extra` 插槽，独占一行；`foot` 与 `extra` 不再互相挤 |
| `web/src/components/ServiceUnitCard.vue` | 头部改列布局：名称一行、标签一行、描述一行（`--fs-sm`/`--ink-3`） |
| `web/src/components/ServiceUnitPanel.vue` | 描述与名称之间的间距；muted 规格；修 `ssh.serviceOpenBSD Secure Shell server` 连成一行 |
| `web/src/views/ServiceUnitView.vue` | 去掉死的 `max-width: 1000px` |
| `web/src/views/StorageDeviceView.vue` / `web/src/components/DeviceDetailPanel.vue` | 去掉死的 `max-width: 1100px`；详情项的首列与值之间加 6px |
| `web/src/components/CapacityBar.vue` | 说明文字降到 `--fs-sm` / `--ink-3` |
| `web/src/views/SettingsView.vue` | 换真页头（`h1` + 说明），两栏自适应网格；`.settings__lead` 包一层才换得成行 |
| `web/src/views/JobsView.vue` | 换真页头，删掉与卡片标题重复的写法；`.jobs__lead` 同上 |
| `web/src/views/AlertsView.vue` | 根改 `<section>`（留在 `.win > section` 覆盖内），去死的内边距，页头重排 |
| `web/src/views/NotFoundView.vue` | 去掉 `NResult` 的彩色 emoji，换 `<span class="nf__code u-num">404</span>`；按钮改主色实心 |
| `web/src/views/UpdatesView.vue` | 删掉与卡片标题重复的 `powerLine` 一行；`.updates__tiles` 交回全局网格 |
| `web/src/components/PowerControlCard.vue` | 重启标志的说明重写（**测试断言的子串逐字保留**） |
| `web/src/views/BackupView.vue` / `web/src/views/SchedulesView.vue` | 页头第二个主按钮降为 `secondary` |
| `web/src/views/OverviewView.vue` | 仪表从 84px 定宽改 `display: block` |
| `web/scripts/preview-pages.mjs` | 删掉死路由 `/host/capabilities`；补 `/health/live`（**注释注明是照 handler 抄的，不是抓的**）；把没夹具的 9 页写进文件头 |

**这一轮做的五个判断，都记进了 `docs/DECISIONS.md`**（Q528—Q532）：

* **Q528 naive 把颜色与几何存在元素的内联 `style` 上。** 外部 CSS 碰 `--n-*` 只有
  `!important` 生效，而 `!important` 会同时打死所有变体。所以**这些变量一律不在外部 CSS 里碰**，
  入口只有 `themeOverrides`；提高特异性用双类选择器（0-2-1）。
* **Q529 `themeOverrides` 是几何的唯一来源。** 身高、内边距、方角写在那儿，CSS 那边**一个字节都
  不写**——文字按钮正是靠把 `--n-height` 设成 `initial` 才贴着行内文字走。
* **Q530 `.win > section, .win > main` 只套页面根。** 原来那条选到所有后代，把 `UPanel` 自己的
  `<section>` 也变成了 16px 的 gap 盒子。
* **Q531 `flex: 1 0 100%` 换不了行。** flexbox 会先用 `max-width` 夹住 hypothetical main size；
  要换行就得加一层容器。
* **Q532 夹具覆盖有洞，且这个洞不能用"编一个成功响应"来补。** 9 个页面没有夹具，预览里就该显示
  「读不到」；它们正常的样子只能在 Ubuntu 真机上看。

**已验证（宿主，不是 Ubuntu 验收）**：仓外 `node-v22.23.3`，Chromium **156.0.8078.4**（Playwright
1.64.1），`docs/evidence/UI/2026-10-30-pages/`：

* **25 张图**（1440×900 明 / 暗、400×820 窄屏），`pageerror` **0**、横向溢出 **0**，逐页读数在
  `facts-preview.json` 与 `result.md` 的表里。
* `npx vue-tsc --noEmit` → exit 0；`npx vitest run` → **66 文件 / 1219 用例全过**；
  `npx eslint .` → exit 0（**只覆盖 `.ts`/`.mjs`，`.vue` 被忽略**）；`npm run build` → ✓ 9.11 s。
* `sh scripts/check.sh` 整跑 → **PASS 15 / FAIL 0 / NOT RUN 0**，跑在最终字节上。
  **别把它读大**：`web-smoke` 走的是未登录路径、断言落在登录页，**没覆盖这一轮改的任何一页**；
  15 条里没有一条能把"这一页看着对不对"变成红绿。**版面是看图看的。**

**未验证 / NOT RUN（与上面那串分开记）**：

* **零个字节跑在 Ubuntu 真机上。** 25 张全是宿主 Chromium 回放夹具。
* **只看了 Chromium。** Firefox / Safari / Edge 一次没开；`backdrop-filter`、`100dvh`、
  `background-attachment: fixed` 三处最可能在别家不一样。
* **无障碍没有读数**：没跑 axe、没做屏幕阅读器实测、对比度没量（暗色下 `--ink-3` 在 `--surface`
  上的比值依然未知）。
* **没碰过触摸屏。** 400px 只是把窗口缩窄。
* **9 个页面的正常状态完全没看过**：`/jobs`、`/backup`、`/alerts`、`/schedules`、`/network`、
  `/settings/host`、`/terminal`、`/apps/deployments`、`/accounts`。预览里它们只显示"读不到"。
* **内容窗口内部的横向溢出没有断言**，这一轮只在 `documentElement` 上量。

**API / 数据库变化**：无。

**已知问题·阻塞**：无阻塞。已知问题全是"覆盖面不够"，不是"做错了"。

**下一步**：本地能做的前端工作到此为止。剩下的每一条都要求真机：在验收 VM 上装好、真登录、
把那 9 个页面点一遍。按 Q510 要等下一版重打字节（改文档措辞不重打）。

### 界面重做（三）（2026-10-30）— 从"带图标的仪表盘"改成真正的桌面（纯前端 + 一次重打包真机自看）

**起因**：负责人第三次纠正一句「我说了是那种桌面版，你能理解什么是桌面版吗」。把上一版外壳当桌面读：
**没有桌面**（`/overview` 就是首页，一进来已经在某个功能页里，图标阵列只是快捷入口）、**没有窗口**
（内容区与屏幕同宽同学，滚动的是整页而不是一个窗格）、**关掉没处可回**（没有"桌面"这个落地页）。
三件事是同一个问题：外壳只包了一层皮，没承起"桌面 → 开窗 → 关窗回桌面"这条动线。**仍然不是
STAGES 的子任务**，不计入矩阵、不改任何验收状态。

**边界**：`cmd/`、`internal/`、`api/openapi.yaml`、DTO、状态机、动作注册表、`modules/registry.ts` 的
16 项顺序与 `implementedIn` 规则**一行没动**。改的全部在 `web/src/`（加一次 `scripts/build.sh`/
`release.sh` 重打包，因为要看真机）。

**已实现**：

| 文件 | 作用 |
|---|---|
| `web/src/shell/DeskWindow.vue` | 新。窗口壳：标题栏（两个真 `<button>`：关闭 / 铺满）、拖动、右下角缩角、双击铺满；`MIN_W 620 / MIN_H 420`，默认占视口 **82%×78%**；`@media (max-width:820px)` 直接铺满、隐藏把手 |
| `web/src/shell/DeskIcons.vue` | 新。桌面图标阵列，`grid-auto-flow: column` 列流、每格 96px、`<button role="listitem">`，激活态反色 |
| `web/src/views/DeskView.vue` | 新，**空组件**（`display:none`）。桌面装修挂在 AppShell 上；这条路由存在的唯一理由是**让 URL 与屏幕一致**（刷新、后退、关窗都诚实）。理由写在文件头 |
| `web/src/router/routes.ts` | 加 `/desk`（`meta.moduleKey: null`），放在 `/overview` 前 |
| `web/src/router/redirect.ts` | `HOME_PATH` 改 `/desk`；白名单逻辑不变（拒绝 `//`、`/\`、`/login`） |
| `web/src/router/redirect.test.ts` | 断言跟着改成 `/desk`，每个兜底用例都用它 |
| `web/src/shell/AppShell.vue` | 重写：`.desk__wall`（点阵壁纸）+ `DesktopBar` + `DeskIcons` + `DeskWindow(v-if="!onDesk")` 里包 `<RouterView />` + `AppDock` + `AppLauncher`；`closeWindow()` → 回 `/desk` |
| `web/src/shell/DesktopBar.vue` | 加 `onDesk` prop；在桌面上时面包屑写「桌面」而不是某个模块名 |
| `web/src/App.vue` | 删掉 `AppShell` 插槽里那段死的 `<RouterView />`（历史上它和 shell 内部的重复） |
| `web/src/views/LoginView.vue` | 登录页挪到同一张壁纸上，垂直居中，`box-shadow: var(--shadow-win)` |
| `web/src/styles/views.css` | Q530 那条 `.win > section, .win > main` **改成 `.win__body > section/main/div`**——页面根节点被塞进 `.win__body` 了，旧选择器一条都匹配不上；意图不变（只认直接子元素，管不到面板内部） |
| `web/scripts/preview-pages.mjs` | 加 `page-desk` |
| `web/scripts/shoot-live.mjs` | 新（工具，不进 `dist`）：真机登录 → 7 页截图；登录改成 `waitForURL(60s)`（TCG 下固定等 2.5 秒会误判"没跳转"） |

**这一轮的判断，记进了 `docs/DECISIONS.md` Q533**：关闭与铺满这次要给——单窗格模型里"关闭"唯一诚实的
语义是**回到桌面**，是真导航；**最小化仍然不给**（没有窗口栈，最小化只能变成"藏起来"，假语义）。
Q524 那句"不做最小化/关闭/还原这类按钮"原文不改，记为被 Q533 **部分修订**。

**已验证（宿主）**：仓外 `node-v22.23.3`、Chromium 156.0.8078.4 / Playwright 1.64.1。
`npx vue-tsc --noEmit` → 0；`npx vitest run` → **66 文件 / 1219 用例全过**；`npx eslint .` → 0；
`npm run build` → ✓ 1.84 s；`sh scripts/check.sh` 整跑 → **PASS 15 / FAIL 0 / NOT RUN 0**。
**别读大**：`web-smoke` 仍走未登录路径落在登录页，这 15 条里没有一条能判"这一页看着像不像桌面"。
截图 5 张（`/desk`、`/`、`/services` 1440×900 + `/desk`、`/` 400×820）：**`pageerror` 0、未覆盖接口 0、
横向溢出 0**，窄屏两张 `scrollH` 恰为 820（窗口铺满、没把整页撑长），逐张读数在
`docs/evidence/UI/2026-10-30-desk/result.md` 第一节。

**已验证（真机，但只是截图不是验收）**：Ubuntu 24.04.5（`6.8.0-147-generic`，4 vCPU `tcg,thread=multi`）。
重打包 amd64 `upanel-0.0.0-8e52b36-linux-amd64.tar.gz`（44176855 字节，
`sha256 72997c0f82f0dbecaae064611c82347302ed42151ecacda7a4fe0fb51d46634a`），客机内 `sha256sum -c` 先核字节
→ OK → 解包 → `install.sh --source …`（**没加** `--write-config`：沿用已有配置与代理密钥，安装器自报
「管理员账号: (已存在的账号)」）。装完 `upanelctl 0.0.0-8e52b36 (commit 8e52b36-dirty, built
2026-10-29T16:22:54Z, go1.27.3)`、两服务 `active`、Web uid=999。经 `ssh -N -L 8443:127.0.0.1:9443`
在宿主浏览器打开：**登录后落在 `/desk`**，7 次导航 **`pageerror` 0**。图上看到的是真数据——概览
CPU 53.8% / 4 逻辑核（`QEMU TCG CPU`）、内存 645.71 MiB / 3.82 GiB、运行 25 分钟；服务与进程 **229 个单元**
（运行 59 / 未运行 168 / 失败 0 / 受保护 90 / 不在本机 22）；存储真块设备 `sda` 40 GiB（系统盘受保护）/
`sdb` 8 GiB / `sdc` 4 GiB / `sdd` 4 GiB，健康一律「未知」或「不支持检测」并写原因；共享页给的是
**能力缺失的诚实态**（`Samba 不可用`、`NFS 不可用`，逐条列缺什么、为什么因此不许写配置）；
应用页 `0 个应用 / 待确认候选 4`（`containerd.service` 在跑、监听 `127.0.0.1:40215`）。读数与复现命令见
`docs/evidence/UI/2026-10-30-desk/result.md`（含宿主 3 张与真机 7 张的分档说明）。

**未验证 / NOT RUN**：

* **窄屏只在宿主夹具上补了两张**（`/desk` 与 `/`，400×820，横向溢出 0、`scrollH` 恰 820——铺满分支与
  「隐藏标题栏 / 隐藏把手」都按预期走）。**真实数据下的窄屏、窄屏上的那 9 页，一次都没看过。**
* **只开过一个浏览器**（Chromium 156）：Firefox / Safari / Edge 没开。
* **窗口拖动 / 缩放 / 双击铺满没有自动化**：靠手点，没有断言，也没有键盘可达性读数（焦点环、Tab 顺序）。
* **窗口内横向溢出没量**：只量了 `documentElement`。
* **无障碍零读数**：没跑 axe、没量对比度、没做屏幕阅读器实测。
* **真机那 9 个页面只看到空态**：这台机器没装 samba/nfs/certbot、没有任务、没有备份仓库、没改过网络。
  **空态对，非空态未知**——要按 `install.sh --install-deps nginx,certbot,samba,smartmontools` 补（TCG 下
  十分钟以上）之后才能看。
* **这不是界面验收**：31 条矩阵一条没动；真机图是截图，无断言。

**API / 数据库变化**：无。

**已知问题·阻塞**：无阻塞。已知问题全是"覆盖面不够"。

**下一步**：界面侧本地已经没事可做。要看非空态得先在验收 VM 上补齐依赖包，再点那 9 个页面。

## 6. 未决问题与范围变更


未批准任何范围变更。DECISIONS.md 第 8 节记录 Q1—Q479（P14.5 收尾时为 479 条）；P03.3 新增六条：Q67（阶段与事件必须同事务，
缺陷修复）、Q68（日志环保留 vs 只追加）、Q69（SSE 用轮询+对账，不做进程内广播中心）、
Q70（viewer 任务可见性 = per-user 隔离，别人的任务 404）、Q71（验收脚本的运行身份必须由调度器
强制）、Q72（最小登录页与路由守卫，范围补充，待确认）。P03.4 新增八条：Q73（对账用
30 秒后台循环 + 启动扫描，只查不跑）、Q74（在途提交不判定）、Q75（`NOT_FOUND` 按 `needs_sync`
分流，修订 Q63）、Q76（幂等记录只删已结束任务的）、Q77（测试注册表约束变成断言）、
Q78（断言读字段，不扫整个响应体——随机 `request_id` 曾撞上 “4242”）、
Q79（`p01-4` 必须自清 systemd 库；旧库里的旧迁移校验和会让服务按设计拒绝启动）、
Q80（断言必须指名对象，不能按秒级 `created_at` 取“最新一条”）。P04.1 新增八条：
Q81（采集缓存只在代理侧，Web 只透传）、Q82（字节与计数器是十进制字符串）、
Q83（段级 `status` 与值自带 `collected_at`；`degraded` 与 `stale` 是两个问题；速率算不出来是
`null`；`boot_id` 变化让窗口失效）、Q84（能力只登记真实存在的东西）、
Q85（夹具必须是真实采集 + 标注反例）、Q86（p02-3 断言收紧 85→87，只增不减）、
Q87（Python 3.10 与 RFC3339Nano；验收脚本截断小数秒，产品格式不改）、
Q88（代理不可用是 503 `AGENT_UNAVAILABLE`，不是 500；采集失败不阻塞登录）。
P04.2 新增八条（Q89—Q96，见阶段记录）。P04.3 新增七条：Q97（SMART 用 `smartctl -j`，
**退出状态是位掩码**，失效/预失效 → `critical`、历史位/日志位 → `warning`、命令或打开失败 →
`unknown`；温度 ≤ 0 视为没有读数）、Q98（**不为读健康唤醒休眠盘**：先读 sysfs 电源状态，
`sleeping` 直接跳过并标 `source=sysfs`，非 NVMe 再加 `-n standby`）、Q99（`/proc/diskstats`
**每台设备只取自己那一行**，分区与整盘、dm 与底层盘从不相加）、Q100（速率由相邻两次采样差分，
窗口异常或计数变小 → `null`，扇区固定 512 字节，`io` 不进 `snapshot_version`）、
Q101（**网络挂载在拓扑里必须可见**：无块设备的网络文件系统单独进 `network_mounts`，
容量/可达性不进版本号）、Q102（验收**等结论不等秒数**：轮询字段直到目标状态，而不是 `sleep N`）、
Q103（`cmd/upanel-web` 的用例会读机器上的 `/etc/upanel/upanel.json`，测试结果取决于环境——
用例自己补空配置，产品代码不改）。**Q104 以后的条目（P04.3 补强至 P07.5，共 Q104—Q209）没有
在这里逐条复述，权威清单见 `docs/DECISIONS.md`**；P07.5 新增 Q204—Q209：
`findmnt --target` 的语义与改法、`capabilities` 为空时页面说什么、外部检查的故障注入只动 fstab 副本、
夹具从真机抓回、**Q208 产品缺陷**（把“未挂载所以读不到”当成“不合格”）、Q209（想当然的收尾期望）。

- Q1 Go module 路径暂定 `upanel`（P01 前如需 canonical path 需一次性确定）。
- Q2 开发宿主工具链与冻结版本不一致，已用 GOTOOLCHAIN 与仓库外 Node 22.23.3 解决。
- Q3 ~~阻塞~~ → **已解决（2026-10-20）**：便携 QEMU + WHPX 创建 VM-A/VM-B，序列号与隔离已核验。
- Q4 TypeScript 固定 5.9.3（typescript-eslint peer 上限）。
- Q5 前端大版本运行时兼容性：**部分验证（2026-10-20，P01.3）**——Vue 3.5.44 / Router 5.4.0 /
      Pinia 4.1.0 / Naive UI 2.45.3 / Vite 8.3.4 组合可构建、可渲染（Chromium 156 无头）；
      Firefox/Safari 与真机移动端仍未验证。
- Q6 仓库未初始化 Git，CI 未运行。
- Q7 Playwright 浏览器：**已解决（2026-10-20）**——本机安装 chromium 1248（匹配 @playwright/test 1.64.1），
      `npm --prefix web run smoke` 可跑；浏览器二进制不在仓库内，换机需执行 `npx playwright install chromium`。
- Q8 cloud-init seed 经宿主回环 HTTP 提供（无管理员权限，无法可靠生成 CIDATA ISO）。
- Q9 验收 VM 内未装 Go/Node 工具链（P00 不要求；P01 起如需要再补装核验）。
- Q10 配置为严格 JSON（未知字段/尾部多余内容报错）；显式 `--config` 缺失即失败，默认路径缺失用内置默认值。
- Q11 mock 只能说 `127.0.0.1:18881`，其他地址一律退出码 2，不做自动纠正。
- Q12 `web/go.mod` 是构建边界（挡住 `node_modules` 里的 Go 源码），不是依赖清单。
- Q13 TLS 未接入（P10）；在此之前 production 拒绝非回环地址。TLS 行为本身未验证。
- Q14 宿主存在两个 Node；跑前端/检查命令前必须把 nvm 的 22.23.3 前置到 PATH。
- Q15 P01.1 零第三方 Go 依赖；Gin/SQLite 在 P01.2 按锁定的精确版本一次加入。
- Q16 新增 `database` 配置段（`path`、`busy_timeout_ms`）：Linux 默认 `/var/lib/upanel/upanel.db`，
      非 Linux 无默认值，必须显式 `--db`；路径必须是绝对路径且不含 `?`/`#`，数据目录不由代码创建。
- Q17 panic 与响应序列化失败返回 500 + `INTERNAL_ERROR`；该码在契约 24 码之外，业务逻辑永不使用，
      前端按未知错误处理。已在 openapi.yaml 注明。
- Q18 405 与 500 都不在 CONTRACTS 的“状态码”枚举内：405 用 `VALIDATION_ERROR` 并带 Allow 头；
      两者都是 HTTP 语义补全，不改契约枚举，前端需能处理。
- Q19 `go mod tidy` 解析出的传递依赖 `golang.org/x/crypto v0.48.0`、`golang.org/x/sys v0.48.0`
      低于冻结表写的 v0.58.0 / v0.49.0。按 MVS 保留现状，首次直接 import 时再抬到冻结版本，
      不写 replace、不伪造版本。详见 DEPENDENCIES §2。
- Q20 Gin 只用于 Web 公开 API；特权代理继续用标准库 net/http + Unix socket，
      不把 Web 框架引入权限边界更高的一侧。
- Q21 前端测试与运行模式（P01.3）：
      ① 冻结依赖里没有 jsdom/happy-dom，组件测试用 `vue/server-renderer` 做 SSR 字符串断言，
         vitest 环境固定 `node`；不新增 DOM 环境依赖。
      ② 依赖里没有 eslint-plugin-vue，`.vue` 不经过 ESLint；SFC 类型/模板由 vue-tsc 覆盖，
         渲染分支由 SSR 断言覆盖。需要 `.vue` 规则时按“新增依赖”流程单独决策。
      ③ 运行模式来源优先级 `window.__UPANEL_MODE__` → `VITE_UPANEL_MODE` → `production`；
         P01.4 起二进制运行由服务端注入，缺口只剩浏览器侧自证（`GET /meta` 属 P02，需要登录）。
- Q22 前端产物与部署形态（P01.4）：
      ① 二进制没有嵌入前端资源时不拒绝启动，而是启动打 WARN、页面请求返回 503 并提示跑
         `scripts/build.sh`。理由：这类二进制仍可用于只调 API 与排查，拒绝启动会让“构建顺序错了”
         和“配置错了”混成同一个错误；503 保证不会把缺资源伪装成正常页面。
      ② 静态资源缓存：`assets/` 下带哈希的产物用 `immutable` 长缓存，`index.html` 用 `no-store`
         （入口页含服务端注入的运行模式，缓存会造成 mock/production 串台）。
      ③ CSP 的 `style-src` 保留 `'unsafe-inline'`：Naive UI 在运行时注入 `<style>`，在不换组件库
         的前提下无法用 nonce 覆盖；`script-src` 仍然只允许带 nonce 的内联脚本。
      ④ 代理 systemd 单元推迟到 P02.3，因为 `/run/upanel/agent.sock` 的属主与 0660 权限
         属于 P02.3 的交付内容，先写单元等于先编造权限模型。

- Q37 代理生产登记表**故意为空**（P02.3）：`NewProductionRegistry()` 返回空表，capabilities 如实报
      `actions=[]`、`queries=[]`，启动日志记录 `registered_actions=0`；登记机制由测试专用登记表覆盖，
      真实只读查询随 P04/P05 逐项登记。
- Q38 UID 边界（P02.3）：普通接口只认内核 SO_PEERCRED 报告的 `web_user` UID；root 走普通接口一律
      403，root 专用路由表当前为空；同组其他 UID 同样 403 且响应不回显允许的 UID。
- Q39 `/run/upanel` 固定 0755 root:root（可穿越不可写），并由代理启动时 `verifySocketDir` 自检，
      不满足即拒绝启动；socket 本身仍是 0660 root:upanel-web。
- Q40 执行器在 `filepath.IsAbs` **之前**做程序路径字符白名单，拒绝命令字符串（`;`、空格、`$( )`、
      反引号、`|`、`&&`、引号）；该缺陷只有在目标平台原生跑测试才暴露。
- Q41 `internal/agentclient` 区分 `*Error`（代理明确拒绝，按契约码处理）与 `*UnavailableError`
      （拨号失败/超时/坏信封，上层必须当依赖不可用，不能显示成业务结论）。
- Q42 审计跟随业务事务（P02.4）：`audit.Record` 发现 `ctx` 带事务就写同一事务，业务回滚则审计一起消失；
      被拒绝的请求没有业务事务，独立提交一条 `outcome=failure`。
- Q43 代理数据库独立且自证权限（P02.4）：`/var/lib/upanel-agent/agent.db`，目录 0700 root、文件 0600，
      打开前校验目录、打开后校验文件；用自己的迁移集 `migrations/agentdb`；本阶段只建 `secrets` 表，
      §14.2 其余表随定义它们的阶段落地（不猜列名）。
- Q44 AEAD 选型与密钥文件（P02.4）：标准库 AES-256-GCM（不引 x/crypto，避免 Q19 的版本抬升）、
      每条独立随机 nonce 单独成列、AAD 绑 `ref`+`purpose`；密钥两行文本 `root:root 0600`，
      `-init-key` 用 `O_CREATE|O_EXCL` 永不覆盖，指纹为 SHA-256 前 8 字节。
- Q45 秘密生命周期（P02.4）：用途闭集（`totp_seed` 仅内部）、只有 POST 没有读取明文路由、
      引用可创建时给也可事后提升（同引用幂等、拒绝改绑、过期不可提升）、到期销毁清空密文与 nonce
      但保留墓碑行、时间列统一 INTEGER Unix 毫秒。
- Q46 日志脱敏按字段名片段命中，因此元数据字段改名（P02.4）：`key_path`/`key_version`/
      `key_fingerprint`/`ref`；不放宽脱敏、不加放行名单，两个方向的回归测试固定这条约定。
- Q47 审计断言改成精确三条（P02.4）：四个业务动作各至少一条 success、failure 行必须带结构化
      `stage`+`error_code` 且无口令键、业务动作 target 为 `user` 且 failure target 只允许 `user`/`session`。
      “全部 success”这种写法本身是假断言（P02.4 起被拒绝请求也会记账）。
- Q48 验收脚本的 journal 断言必须限定时间窗（P02.4）：两套脚本都改用 `--since "$JOURNAL_SINCE"`，
      既避免历史行造成误报，也避免旧记录让“含某串”断言误过。
- Q49 换行符属于构建输入（P02.4）：`.go`/`.sh` 被写成 CRLF 会让 `gofmt` 闸门翻红、脚本在 VM 上崩在
      `set: Illegal option -`；新增 `.gitattributes` 强制 LF，源文件改动后必须重新交叉编译并重跑 VM 验收
      （`upanel-agent` 哈希 43ca7e78… → 32c5e305… 即因此而来）。
- Q50 预检计划只能由代理生成（P03.1）：Web 不复制动作表、不算 allowed；Web 若也能造计划，
      状态版本/目标指纹/锁就可以被单方面编出来，执行前复核会变成自证。Web 只保存绑定字段并做合理性校验。
- Q51 **契约差异待确认**（P03.1）：CONTRACTS §4.1 示例短语是 `"FORMAT ABC123 P1"`（三段），实现是
      `"FORMAT K7M2QX"`（动词 + 6 位去歧义码）。契约正文未规定格式，`P1` 语义未说明；在 CONTRACTS 写清
      之前不改契约、也不假装符合示例，P03.2 按整串精确比对。
- Q52 确认短语只存哈希（P03.1）：代理库 `phrase_hash`，明文只在预检响应出现一次；Web 连哈希都不存
      （没有比对需求），短语不进日志/审计/journal；验收脚本在 db/wal/shm 三处二进制搜索明文为 0 次。
- Q53 预检到期由代理给（P03.1）：`expires_at = now.Truncate(秒) + 120s`，Web 存同一字符串，只拒绝
      已过期或超出 TTL+5 s 偏差的计划；截断到秒修掉了“两处毫秒/秒级真值差几百毫秒”的缺陷。
- Q54 验收动作表用 `-acceptance-actions` 开关（P03.1）：`acceptance.probe`（再认证 + 短语）与
      `acceptance.ping`（都不要求，作对照）只在开关打开时登记，生产表仍为空；两者**只登记规划器**，
      预检路径上不存在能改系统的代码分支（STAGES 明令 P03 通过前禁止写磁盘代码）。
- Q55 `agentclient` 只认 200 是真实缺陷（P03.1 修复）：代理创建类接口返回 201，两侧单测各用假对端
      因此“各自正确、合起来错误”；改为接受 2xx，并补真 client 打真 handler 的端到端用例。
- Q56 公开预检 DTO 不暴露 `state_version` 与 `agent_ref`（P03.1）：它们是服务端绑定信息，
      暴露只会诱使前端自己判断状态是否变化；契约示例也没有这两个字段。
- Q57 代理库 schema 版本随代理迁移前进（P03.1）：新增 `0002_preflight_plans` 后版本 1 → 2，
      `p02-4` 的精确断言同步改为“当前应有的版本 2”并注明规则；不放宽成 `≥1`，两条迁移序列互不影响。
- Q58 `operation_id` 由 Web 生成、代理按它去重（P03.2）：Web 造 `op_`+32 hex，代理 `UNIQUE(operation_id)`
      + `request_hash`；先落库再提交，崩溃窗口里重放只回已有记录、不二次执行。
- Q59 重复提交先命中去重、后消费预检（P03.2）：顺序反了的话，一次网络重试就会把预检烧掉而任务没建。
- Q60 `preflights.cancellable` 默认 0（P03.2）：没声明可取消的动作按不可取消处理，保守默认宁可少一个
      取消按钮，也不给一个按下去会被 409 拒绝的按钮。
- Q61 `syncFromAgent` 只在状态真的变了时写事件（P03.2）：否则对账轮询会把 `job_events` 灌成回声，
      历史失去“发生过什么”的意义。
- Q62 不可取消动作的取消请求 = 409 + `VALIDATION_ERROR`（P03.2）：契约 24 码无此项，不新造错误码。
- Q63 代理回 `NOT_FOUND` → `failed`/`unconfirmed` + `AGENT_UNAVAILABLE`（P03.2）：状态未知不等于成功，
      也不自动重跑破坏性动作；同一 `operation_id` 留给人工/后续对账。

- Q89/Q90/Q91 是 P04.2 在真实 VM 上抓到的三个缺陷（块设备被当普通文件读导致 OOM、快照版本依赖时间、
      by-id 链接缺“已知设备”约束），修法与影响见 DECISIONS。它们的共同点：夹具单测全绿，只有真机会炸。
- Q92 起 `capabilities` 里出现第二个查询 `storage.topology`（仍无任何动作）：`p02-3`/`p04-1` 里写死的
      “只有 system.summary”断言已同步收紧。**少报和多报都算失败**，不允许为了让老断言过而隐瞒新能力。
- **待确认（P04.2 提出）**：`docs/CONTRACTS.md` §6.1 是本轮新增的（保护递归、`capabilities` 恒空、
      内核分区结果优先、未挂载时 `filesystem.type/uuid/label` 为 null、休眠盘不唤醒、容量 30 秒 TTL）。
      它是对 §6 的补充，不改变既有字段语义；如果负责人希望换一种写法（例如把 `protection.sources`
      去掉、或让 `capabilities` 永远不出现），要在 P04.3 开始前说清楚，否则前端会按现在的形状实现。
- **未决（P04.2 记录）**：VM-C（LVM 根）仍未创建。保护递归在 LVM/RAID/加密上的逻辑只有构造节点与
      边的单测，没有真机样本。P07（分区/格式化/挂载）之前必须补一台 LVM 根的验收 VM，
      否则“写操作前能不能正确识别系统盘”这一点在真实叠层上仍属未验证。

- Q155—Q159 是 P06.2 的记录（Q155 是 Q136 的**重犯**：`O_PATH` 的 fd 上 `getdents64` 是 EBADF，
      这次同时打断了目录删除预检、目录复制和备份清理；Q156 `PARENT_MISSING` 在真实 files 层不可达；
      Q157 保留目录 `.upanel` 在列举里可见；Q158 预留计数在无容量读取器时恒 0 且过期清理不释放；
      Q159 决定“受理即拒”的作业**保留**为 `failed`/`rejected` 而不是删掉）。共同点仍然是：
      Windows 宿主上 `go:build linux` 的路径根本没跑过，夹具单测全绿，只有真机会炸。
- **待确认（P06.2 记录，不阻塞 P06.3）**：暂存区与备份区放在被管理根内部的 `.upanel`，
      因此它们占用该根的配额，且对“根被整个 `rm -rf`”没有保护。P07/P12 之前需要定：
      是继续放在根内（实现简单、跨盘不需要额外配置），还是改成 `/var/lib/upanel-agent/staging`
      （受保护、但跨文件系统提交就不能用 `renameat2` 原子换位，得退化成 copy+rename）。
      现在的实现选了前者，理由是“提交必须在同一文件系统内一次原子完成”。

后续按“日期、问题/变更、依据、影响文件、决策人、处理结果”记录。
不要把模型个人偏好当作项目负责人批准。

## 7. 验收证据索引

| 编号 | 阶段 | 状态 | 证据 |
|---|---|---|---|
| P00-TOOLCHAIN | P00 | 已执行（宿主） | docs/evidence/P00/2026-10-20/{env.txt,check-run.txt,check-run-host-node.txt,dependency-resolution.txt} |
| P00-VM | P00 | **已执行并通过（VM-A 22.04 / VM-B 24.04）** | docs/evidence/P00/2026-10-20/{vm-2404-verification.txt,vm-2204-verification.txt,host-isolation.txt,commands.log} |
| P00-CHECK-POST | P00 | 已执行通过（退出码 0） | docs/evidence/P00/2026-10-20/check-run-post-p00.txt |
| P01.1-LINUX | P01 | **已执行并通过（VM-A 22.04 / VM-B 24.04，各 26 项断言）** | docs/evidence/P01/2026-10-20/{vm-2404-acceptance.txt,vm-2204-acceptance.txt,env.txt} |
| P01.1-HOST | P01 | 已执行通过（宿主 16 项断言，退出码 0） | docs/evidence/P01/2026-10-20/host-verification.txt |
| P01.1-CHECK | P01 | 已执行通过（PASS 8 / FAIL 0 / NOT RUN 4，退出码 0） | docs/evidence/P01/2026-10-20/check-run.txt |
| P01.2-LINUX | P01 | **已执行并通过（VM-A 22.04 / VM-B 24.04，各 34 项断言；P01.1 回归各 26 项）** | docs/evidence/P01/2026-10-20-p01.2/{vm-2404-p01-2.txt,vm-2204-p01-2.txt,vm-2404-p01-1-regression.txt,vm-2204-p01-1-regression.txt,env.txt} |
| P01.2-HOST | P01 | 已执行通过（宿主 16 项断言，退出码 0；数据库成为硬依赖后回归） | docs/evidence/P01/2026-10-20-p01.2/host-refusal.txt |
| P01.2-UNIT | P01 | 已执行通过（63 项 verbose PASS / 0 FAIL；go test ./... 全部 ok） | docs/evidence/P01/2026-10-20-p01.2/{go-test.txt,go-test-verbose.txt} |
| P01.2-DEPS | P01 | 已执行通过（go mod download / verify 退出码 0，go.sum 134 行） | docs/evidence/P01/2026-10-20-p01.2/dependency-resolution.txt |
| P01.2-CHECK | P01 | 已执行通过（PASS 8 / FAIL 0 / NOT RUN 4，退出码 0） | docs/evidence/P01/2026-10-20-p01.2/check-run.txt |
| P01.3-FRONT | P01 | 已执行通过（typecheck/lint/build 退出码 0；6 文件 50 用例全绿） | docs/evidence/P01/2026-10-20-p01.3/{typecheck.txt,lint.txt,test.txt,build.txt} |
| P01.3-SMOKE | P01 | 已执行通过（真实 Chromium：production 16/16，mock 17/17） | docs/evidence/P01/2026-10-20-p01.3/{smoke-production.txt,smoke-mock.txt} |
| P01.3-CHECK | P01 | 已执行通过（PASS 12 / FAIL 0 / NOT RUN 0，退出码 0） | docs/evidence/P01/2026-10-20-p01.3/check-run.txt |
| P01.3-VM | P01 | 已补验（同源 API + 静态页面在 P01.4 的两台 VM 验收中覆盖） | docs/evidence/P01/2026-10-20-p01.4/vm-*-acceptance.txt |
| P01.4-HOST | P01 | 已执行通过（构建 + curl 边界检查 + 非 Linux 拒绝 + 缺资源 503） | docs/evidence/P01/2026-10-20-p01.4/{build.txt,linux-cross-build.txt,host-checks.txt,host-nonlinux-production-refusal.txt,host-missing-assets-503.txt} |
| P01.4-UNIT | P01 | 已执行通过（go test ./... 12 包 ok；webui+api 33 PASS / 0 FAIL） | docs/evidence/P01/2026-10-20-p01.4/{go-test.txt,go-test-webui-api.txt,go-vet.txt,gofmt.txt} |
| P01.4-SMOKE | P01 | 已执行通过（真实 Chromium 对二进制：19/19） | docs/evidence/P01/2026-10-20-p01.4/host-smoke-mock.txt |
| P01.4-VM | P01 | **已执行并通过（VM-A 22.04 / VM-B 24.04，各 114 项断言，含 systemd 真实安装启动）** | docs/evidence/P01/2026-10-20-p01.4/{vm-2404-acceptance.txt,vm-2204-acceptance.txt,vm-2404-env.txt,vm-2204-env.txt} |
| P01.4-CHECK | P01 | 已执行通过（PASS 12 / FAIL 0 / NOT RUN 0，退出码 0） | docs/evidence/P01/2026-10-20-p01.4/check-run.txt |
| P02.1-UNIT | P02 | 已执行通过（go test ./... 13 包 ok；auth 26 + api 认证 14 组 + httpx origin 5 + config 4 + init-admin 4 用例） | docs/evidence/P02/2026-10-20-p02.1/{go-test.txt,go-vet.txt,gofmt.txt} |
| P02.1-TIMING | P02 | 已执行通过（Argon2id 单次校验 98.7—131.2 ms，落在契约 100—300 ms 区间） | docs/evidence/P02/2026-10-20-p02.1/argon2-timing.txt |
| P02.1-SMOKE | P02 | 已执行通过（宿主 mock 模式真实二进制：PASS 38 / FAIL 0） | docs/evidence/P02/2026-10-20-p02.1/host-smoke.txt |
| P02.1-VM | P02 | 已执行通过（Ubuntu 24.04.5 + 22.04.5，非 root 运行，两台均 PASS 58 / FAIL 0；库文件 0600、重启后会话有效、库内无明文秘密） | docs/evidence/P02/2026-10-20-p02.1/{vm-2404-env.txt,vm-2404-acceptance.txt,vm-2204-env.txt,vm-2204-acceptance.txt} |
| P02.1-FILEMODE | P02 | 部分执行（宿主 Windows 上 SKIP；真实权限在 VM 验收的“数据库文件权限”断言里通过） | docs/evidence/P02/2026-10-20-p02.1/{store-filemode-test.txt,vm-2404-acceptance.txt} |
| P02.2-UNIT | P02 | 已执行通过（go test ./... 14 包 ok；api 34 + auth 44 + audit 4 顶层用例） | docs/evidence/P02/2026-10-20-p02.2/{go-test.txt,host-build.txt,check.txt} |
| P02.2-SMOKE | P02 | 已执行通过（宿主 mock 模式真实二进制：PASS 81 / FAIL 0） | docs/evidence/P02/2026-10-20-p02.2/{host-smoke.txt,host-smoke-web.log} |
| P02.2-VM | P02 | **已执行并通过（Ubuntu 24.04.5 + 22.04.5，非 root 运行，两台均 PASS 99 / FAIL 0）** | docs/evidence/P02/2026-10-20-p02.2/{vm-2404-env.txt,vm-2404-acceptance.txt,vm-2204-env.txt,vm-2204-acceptance.txt,vm-env-host-build.txt} |
| P02.2-REGRESSION | P02 | 已执行通过（P02.1 验收脚本在两台 VM 各 PASS 58 / FAIL 0） | docs/evidence/P02/2026-10-20-p02.2/{vm-2404-p021-regression.txt,vm-2204-p021-regression.txt} |
| P02.2-AUDIT-APPEND | P02 | 已执行通过（VM 上用 sqlite3 直连尝试 UPDATE/DELETE，均被触发器拒绝：APPEND_ONLY=YES） | docs/evidence/P02/2026-10-20-p02.2/{vm-2404-acceptance.txt,vm-2204-acceptance.txt} |
| P02.2-FRONTEND | P02 | **NOT RUN**（本子任务不含账号管理页面；STAGES.md P02.2 未要求前端） | 页面与再认证交互随对应 UI 子任务 |
| P02.3-VM | P02 | **已执行并通过（Ubuntu 24.04.5 + 22.04.5，root 运行，两台均 PASS 84 / FAIL 0；含 systemd 真实托管代理、SO_PEERCRED 边界、启动期拒绝、停止清理）** | docs/evidence/P02/2026-10-20-p02.3/{vm-2404-p023-acceptance.txt,vm-2204-p023-acceptance.txt,vm-2404-env.txt,vm-2204-env.txt,cross-build-sha256.txt} |
| P02.3-UNIT-VM | P02 | 已执行通过（VM 内核上原生跑测试二进制：agent.test 28 PASS、syscmd.test 9 PASS + 1 SKIP） | docs/evidence/P02/2026-10-20-p02.3/{vm-2404-agent-test.txt,vm-2204-agent-test.txt,vm-2404-syscmd-test.txt,vm-2204-syscmd-test.txt} |
| P02.3-UNIT | P02 | 已执行通过（go test ./... 16 包 ok；宿主 build/vet/gofmt 干净；check.sh PASS 12 / FAIL 0 / NOT RUN 0） | docs/evidence/P02/2026-10-20-p02.3/{host-build.txt,go-vet.txt,gofmt.txt,go-test.txt,check.txt} |
| P02.3-JOURNAL | P02 | 已执行通过（journal 有启动字段与 UID/未登记查询拒绝记录，无 password/secret，停止为优雅停止） | docs/evidence/P02/2026-10-20-p02.3/{vm-2404-journal.txt,vm-2204-journal.txt} |
| P02.3-REGRESSION | P02 | 已执行通过（P02.2 验收脚本在两台 VM 各 `PASS=99 FAIL=0`） | docs/evidence/P02/2026-10-20-p02.3/{vm-2404-p022-regression.txt,vm-2204-p022-regression.txt} |
| P02.3-QUERIES | P02 | **部分执行**：查询通道的四类拒绝与空表报告已在 VM 上验证；**真实只读查询（磁盘/服务/进程）NOT RUN**——生产登记表为空，随 P04/P05 登记 | docs/evidence/P02/2026-10-20-p02.3/vm-2404-p023-acceptance.txt |
| P02.4-VM | P02 | **已执行并通过（Ubuntu 24.04.5 + 22.04.5，root 运行，两台均 PASS 87 / FAIL 0；含密钥生命周期、独立代理库权限、密文落盘、无明文读取路由、重启持久性）** | docs/evidence/P02/2026-10-20-p02.4/{vm-2404-p024-acceptance.txt,vm-2204-p024-acceptance.txt,env-2404.txt,env-2204.txt,cross-build-sha256.txt} |
| P02.4-UNIT-VM | P02 | 已执行通过（VM 内核上原生跑交叉编译测试二进制：agentstore 32 用例 / 0 FAIL、agent 45 用例 / 0 FAIL） | docs/evidence/P02/2026-10-20-p02.4/{vm-2404-unit-tests.txt,vm-2204-unit-tests.txt} |
| P02.4-REGRESSION | P02 | 已执行通过（两台 VM：p02-1 各 58 / 0、p02-2 各 103 / 0、p02-3 各 84 / 0） | docs/evidence/P02/2026-10-20-p02.4/{vm-2404-p021-acceptance.txt,vm-2404-p022-acceptance.txt,vm-2404-p023-acceptance.txt,vm-2204-p021-acceptance.txt,vm-2204-p022-acceptance.txt,vm-2204-p023-acceptance.txt} |
| P02.4-JOURNAL | P02 | 已执行通过（root 视角 journal：启动字段含 key_path/key_version/key_fingerprint 与 schema、优雅停止；无提交的明文、无密钥本体） | docs/evidence/P02/2026-10-20-p02.4/{vm-2404-journal.txt,vm-2204-journal.txt} |
| P02.4-UNIT | P02 | 已执行通过（宿主 build/vet/gofmt 干净、go test ./... 全部 ok、check.sh PASS 12 / FAIL 0 / NOT RUN 0） | docs/evidence/P02/2026-10-20-p02.4/{host-build.txt,go-vet.txt,gofmt.txt,go-test.txt,check.txt} |
| P02.4-NOTRUN | P02 | **NOT RUN**：服务上 10 分钟挂钟到期销毁、密钥轮换、真实业务写入秘密的调用方、totp_seed 写入路径 | 见 result.md 第 4 节与 PROGRESS 第 5 节 P02.4 记录的“未验证” |
| P02.4-RESULT | P02 | 已产出（交付清单、宿主与 VM 证据、未执行项、本轮修复的问题、复现命令） | docs/evidence/P02/2026-10-20-p02.4/{result.md,commands.log} |
| P03.1-VM | P03 | **已执行并通过（Ubuntu 24.04.5 + 22.04.5，root 运行，两台均 通过 93 / 失败 0；含 socket 权限、Web 非 root、120 s 期限、两端绑定一致、短语只在响应出现一次、拒绝路径不落库、代理停止 503）** | docs/evidence/P03/2026-10-20-p03.1/{vm-2404-p031-acceptance.txt,vm-2204-p031-acceptance.txt,env.txt,cross-build-sha256.txt} |
| P03.1-UNIT-VM | P03 | 已执行通过（VM 内核上原生跑交叉编译测试二进制：agentstore / agent 预检子集 / agentclient / operations / syscmd 全部 PASS） | docs/evidence/P03/2026-10-20-p03.1/vm-2404-native-tests.txt |
| P03.1-REGRESSION | P03 | 已执行通过（两台 VM：p02-1 各 58 / 0、p02-2 各 103 / 0、p02-3 各 84 / 0、p02-4 各 87 / 0） | docs/evidence/P03/2026-10-20-p03.1/{vm-2404-p021-regression.txt,vm-2404-p022-regression.txt,vm-2404-p023-regression.txt,vm-2404-p024-regression.txt,vm-2204-p021-regression.txt,vm-2204-p022-regression.txt,vm-2204-p023-regression.txt,vm-2204-p024-regression.txt} |
| P03.1-UNIT | P03 | 已执行通过（宿主 build/vet/gofmt 干净、go test ./... 全部 ok、check.sh PASS 12 / FAIL 0 / NOT RUN 0） | docs/evidence/P03/2026-10-20-p03.1/{host-build.txt,go-vet.txt,gofmt.txt,go-test.txt,check.txt} |
| P03.1-CONTRACT | P03 | 已执行通过（`api/openapi.yaml` 新增预检路径与 5 个 schema；redocly lint valid，11 warnings 与改动前同类同量） | docs/evidence/P03/2026-10-20-p03.1/check.txt（openapi-lint 段）、`api/openapi.yaml` |
| P03.1-FRONTEND | P03 | **NOT RUN**（STAGES.md P03.1 未要求前端；预检界面随真实动作的 UI 阶段落地） | — |
| P03.1-NOTRUN | P03 | **NOT RUN**：`POST /jobs` 消费预检、幂等键与 `operation_id`、提交阶段的再认证强制、状态版本复核与锁排队 | 见 result.md 第 7 节与 P03.1 记录的“未验证” |
| P03.1-RESULT | P03 | 已产出（交付清单、契约落地对照、宿主与 VM 证据、未执行项、失败与修正、复现命令） | docs/evidence/P03/2026-10-20-p03.1/{result.md,commands.log} |
| P03.2-UNIT | P03 | 已执行通过（宿主 `go test -count=1 ./...` 全 ok，`-v` 计数 **170 PASS / 0 FAIL**；`go vet ./...` 退出码 0、`gofmt -l .` 无输出、`go build ./...` 退出码 0） | docs/evidence/P03/2026-10-20-p03.2/{host-build.txt,go-vet.txt,gofmt.txt,go-test.txt,go-test-verbose.txt,go-test-verbose-summary.txt} |
| P03.2-CHECK | P03 | 已执行通过（`sh scripts/check.sh` → PASS 12 / FAIL 0 / NOT RUN 0；`redocly lint api/openapi.yaml` valid，10 warnings 与改动前同类同量——8 个 no-unused-components + 2 个 operation-4xx-response） | docs/evidence/P03/2026-10-20-p03.2/{check.txt,openapi-lint.txt} |
| P03.2-VM | P03 | **已执行并通过（Ubuntu 24.04.5 与 22.04.5，root 运行，两台均 通过 101 / 失败 0）**。覆盖 STAGES §5 六条：十次同键提交只执行一次（`operation_id` 唯一）、同键不同请求体 409 `IDEMPOTENCY_CONFLICT`、预检过期与状态变化拒绝、Web 进程被杀后任务继续、代理 kill -9 重启后 `interrupted` 且不自动重试、对账按同一个 `operation_id` 收敛、不可取消动作无取消信号 | docs/evidence/P03/2026-10-20-p03.2/{vm-upanel-2404-p032-acceptance.txt,vm-upanel-2204-p032-acceptance.txt,vm-upanel-2404-env.txt,vm-upanel-2204-env.txt,cross-build-sha256.txt} |
| P03.2-UNIT-VM | P03 | 已执行通过（VM 内核上原生跑交叉编译测试二进制：agentstore / agent / operations / agentclient / syscmd，两台均 **169 用例 PASS / 0 FAIL**） | docs/evidence/P03/2026-10-20-p03.2/{vm-upanel-2404-native-tests.txt,vm-upanel-2204-native-tests.txt} |
| P03.2-REGRESSION | P03 | 已执行通过（两台 VM：p01-1 各 29 / 0、p01-2 各 34 / 0、p01-4 各 114 / 0、p02-1 各 58 / 0、p02-2 各 103 / 0、p02-3 各 85 / 0、p02-4 各 87 / 0、p03-1 各 93 / 0） | docs/evidence/P03/2026-10-20-p03.2/vm-upanel-{2404,2204}-p{011,012,014,021,022,023,024,031}.txt |
| P03.2-RESULT | P03 | 已产出（交付清单、宿主与 VM 证据、验收抓到的两个缺陷与修法、过期脚本修正、未执行项、复现命令） | docs/evidence/P03/2026-10-20-p03.2/{result.md,commands.log} |
| P03.3-UNIT | P03 | 已执行通过（宿主 `go test -count=1 ./...` 全 ok，`-v` 计数 **228 PASS / 0 FAIL**；`go vet ./...` 退出码 0、`gofmt -l .` 无输出、`go build ./...` 退出码 0） | docs/evidence/P03/2026-10-20-p03.3/{host-build.txt,go-vet.txt,gofmt.txt,go-test.txt,go-test-verbose.txt,go-test-verbose-summary.txt} |
| P03.3-CHECK | P03 | 已执行通过（`sh scripts/check.sh` → PASS 12 / FAIL 0 / NOT RUN 0；`redocly lint api/openapi.yaml` valid，10 warnings 与上一阶段同类同量） | docs/evidence/P03/2026-10-20-p03.3/{check.txt,openapi-lint.txt,cross-build-sha256.txt} |
| P03.3-VM | P03 | **已执行并通过（Ubuntu 24.04.5 与 22.04.5，root 运行，两台均 通过 73 / 失败 0）**。覆盖 SSE 帧格式与响应头、`: connected`、`id` 单调、`Last-Event-ID` 续传、坏值 400、未登录 401 / viewer 403 / 别人的任务 404、日志分页与坏 cursor、`gap` 帧、追加式触发器（改状态被拒 / 删日志允许 / 终态不回退）、每用户 4 条连接上限与释放、关闭流不影响任务执行、取消 202/409、日志无 CSRF 与口令 | docs/evidence/P03/2026-10-20-p03.3/{vm-upanel-2404-p033-acceptance.txt,vm-upanel-2204-p033-acceptance.txt,vm-upanel-2404-env.txt,vm-upanel-2204-env.txt,cross-build-sha256.txt} |
| P03.3-UNIT-VM | P03 | 已执行通过（VM 内核上原生跑交叉编译测试二进制：agentstore / agent / agentclient / operations / syscmd / store / api，两台均 **346 用例 PASS / 0 FAIL**；修复前会偶发的 `TestOperationSubmitRunsOnceAndRecordsResult` 在 22.04 上单独复跑 20 次全过） | docs/evidence/P03/2026-10-20-p03.3/{vm-upanel-2404-native-tests.txt,vm-upanel-2204-native-tests.txt} |
| P03.3-REGRESSION | P03 | 已执行通过（两台 VM 各跑完整 `run-all.sh`：p01-1 29 / 0、p01-2 34 / 0、p01-4 114 / 0、p02-1 58 / 0、p02-2 103 / 0、p02-3 85 / 0、p02-4 87 / 0、p03-1 93 / 0、p03-2 101 / 0、p03-3 73 / 0，**失败脚本数 0**） | docs/evidence/P03/2026-10-20-p03.3/vm-upanel-{2404,2204}-p{011,012,014,021,022,023,024,031,032,033-acceptance}.txt |
| P03.3-RESULT | P03 | 已产出（交付清单、宿主与 VM 证据、验收抓到的阶段/事件非原子缺陷与修法、两起流程事故、未执行项、复现命令） | docs/evidence/P03/2026-10-20-p03.3/result.md |
| P03.4-UNIT | P03 | 已执行通过（宿主 `go test -count=1 ./...` 18 包 ok，`-v` 计数 **258 PASS / 0 FAIL**；新增对账 11 个用例 + 验收注册表 3 个） | docs/evidence/P03/2026-10-20-p03.4/{go-test.txt,go-test-verbose.txt,go-test-verbose-summary.txt,host-build.txt,go-vet.txt,gofmt.txt} |
| P03.4-CHECK | P03 | 已执行通过（`sh scripts/check.sh` → PASS 12 / FAIL 0 / NOT RUN 0；`redocly lint api/openapi.yaml` valid + 10 warnings） | docs/evidence/P03/2026-10-20-p03.4/{check.txt,openapi-lint.txt} |
| P03.4-MUTATION | P03 | 已执行（摘掉在途守卫 → 2 个用例失败；写死 `needs_sync` 分流 → 1 个用例失败；`reconcile.go` 无 `CreateOperation` 调用点） | docs/evidence/P03/2026-10-20-p03.4/{mutation-1-inflight-guard.txt,mutation-2-needs-sync-split.txt,static-no-resubmit.txt} |
| P03.4-VM | P03 | **已执行并通过（Ubuntu 24.04.5 与 22.04.5，root 运行，两台均 通过 78 / 失败 0；含 Web 重启重放、代理 kill -9、代理库被清空后的结果未知）** | docs/evidence/P03/2026-10-20-p03.4/{runall-24.txt,runall-22.txt,vm-logs-2404/,vm-logs-2204/} |
| P03.4-UNIT-VM | P03 | 已执行通过（VM 内核上原生跑交叉编译测试二进制：7 个包 **TOTAL PASS 360 / FAIL 0**） | docs/evidence/P03/2026-10-20-p03.4/{native-24.txt,native-22.txt,env-24.txt,env-22.txt} |
| P03.4-REGRESSION | P03 | 已执行通过（两台 VM 各跑完整 `run-all.sh`：11 个脚本 / 0 失败；p01-1 29、p01-2 34、p01-4 114、p02-1 58、p02-2 103、p02-3 85、p02-4 87、p03-1 93、p03-2 102、p03-3 73、p03-4 78） | docs/evidence/P03/2026-10-20-p03.4/{runall-24.txt,runall-22.txt} |
| P03.4-RESULT | P03 | 已产出（交付清单、宿主与 VM 证据、变异与静态检查、四个缺陷与修法、NOT RUN 清单、复现命令） | docs/evidence/P03/2026-10-20-p03.4/result.md |
| P03.3-RESULT（补记） | P03 | 已产出（交付清单、宿主与 VM 证据、验收抓到的阶段/事件非原子缺陷与修法、两起流程事故、未执行项、复现命令）。P04.1 期间发现本表上一行曾被误粘成 P03.3 的路径碎片，已拆回两行 | docs/evidence/P03/2026-10-20-p03.3/{result.md,commands.log} |
| P04.1-UNIT | P04 | 已执行通过（宿主 `go build`/`go vet`/`gofmt` 干净，`go test ./...` **19 包 ok**，指定包 `-v` 计数 **228 PASS / 0 FAIL**；`monitoring` 30 个用例含线格式与解析器反例） | docs/evidence/P04/2026-10-20-p04.1/{host-build.txt,go-vet.txt,gofmt.txt,go-test.txt,go-test-verbose.txt} |
| P04.1-CHECK | P04 | 已执行通过（`sh scripts/check.sh` → PASS 12 / FAIL 0 / NOT RUN 0；`redocly lint api/openapi.yaml` valid + 9 warnings） | docs/evidence/P04/2026-10-20-p04.1/{check.txt,openapi-lint.txt} |
| P04.1-VM | P04 | **已执行并通过（Ubuntu 24.04.5 与 22.04.5，root 运行，两台均 通过 72 / 失败 0；含 `/proc` 逐项比对、时间戳前进、计数器区间、无万能入口、Web 透传、代理停机不阻塞登录）** | docs/evidence/P04/2026-10-20-p04.1/{p04-1-24.txt,p04-1-22.txt,env-24.txt,env-22.txt} |
| P04.1-UNIT-VM | P04 | 已执行通过（VM 内核上原生跑交叉编译测试二进制：8 个包 **TOTAL PASS 401 / FAIL 0**） | docs/evidence/P04/2026-10-20-p04.1/{native-24.txt,native-22.txt} |
| P04.1-REGRESSION | P04 | 已执行通过（两台 VM 各跑完整 `run-all.sh`：**12 个脚本 / 0 失败**，合计 930 断言；p02-3 收紧到 87、p04-1 新增 72） | docs/evidence/P04/2026-10-20-p04.1/{runall-24.txt,runall-22.txt,vm-logs-2404/,vm-logs-2204/} |
| P04.1-BINARY | P04 | 已执行通过（11 个交叉编译产物在宿主与两台 VM 上 sha256 逐个一致；入口二进制用固定 `BUILD_DATE` 可重建） | docs/evidence/P04/2026-10-20-p04.1/{cross-build-sha256.txt,vm-sha256-2404.txt,vm-sha256-2204.txt} |
| P04.1-RESULT | P04 | 已产出（交付清单、宿主与 VM 证据、六个缺陷与修法、NOT RUN 清单、环境事故记录） | docs/evidence/P04/2026-10-20-p04.1/{result.md,commands.log} |
| P04.2-CHECK | P04 | 已执行通过（`sh scripts/check.sh` → PASS 12 / FAIL 0 / NOT RUN 0；`redocly lint api/openapi.yaml` valid + 9 warnings） | docs/evidence/P04/2026-10-20-p04.2/{check.txt,openapi-lint.txt} |
| P04.2-VM | P04 | **已执行并通过（Ubuntu 24.04.5 与 22.04.5，root 运行，两台均 通过 108 / 失败 0 / 未执行 0；含设备集合与 sysfs 全等、边方向与封闭性、身份稳定不撞车、系统盘保护独立重算、临时 loop 镜像不因挂载受保护、容量对 Bavail 精确对账、参数边界、Web 透传与 viewer 权限、代理停机 503）** | docs/evidence/P04/2026-10-20-p04.2/{p04-2-2404.txt,p04-2-2204.txt,vm-2404-env.txt,vm-2204-env.txt,storage-regression-2404.txt,storage-regression-2204.txt} |
| P04.2-UNIT-VM | P04 | 已执行通过（VM 内核上原生跑交叉编译测试二进制：9 个包 **TOTAL PASS 473 / FAIL 0**；`TestReadDiskLinksUsesSymlinkTarget` 在 Linux 上真实执行，不是 SKIP） | docs/evidence/P04/2026-10-20-p04.2/{native-2404.txt,native-2204.txt} |
| P04.2-REGRESSION | P04 | 已执行通过（两台 VM 各跑完整 `run-all.sh`：**13 个脚本 / 0 失败**，合计 1038 断言） | docs/evidence/P04/2026-10-20-p04.2/{runall-2404.txt,runall-2204.txt} |
| P04.2-BINARY | P04 | 已执行通过（12 个交叉编译产物在宿主与两台 VM 上 sha256 逐个一致；入口二进制重复构建 3/3 逐字节相同） | docs/evidence/P04/2026-10-20-p04.2/{cross-build-sha256.txt,vm-sha256-2404.txt,vm-sha256-2204.txt} |
| P04.2-RESULT | P04 | 已产出（结论、逐段验到的行为、测试有牙的证据、六个缺陷与修法、NOT RUN 清单、环境事故与操作缺陷记录、复现命令） | docs/evidence/P04/2026-10-20-p04.2/{result.md,commands.log} |
| P04.3-CHECK | P04 | 已执行通过：宿主 `go build` / `go vet` / `gofmt -l .` 干净；`go test ./... -count=1 -v` 计 568 个顶层 PASS（含子测试 744）/ 0 FAIL / 6 SKIP / 20 包 ok；`sh scripts/check.sh` **PASS 12 / FAIL 0 / NOT RUN 0**（含 openapi-lint：valid + 9 warnings）。 | docs/evidence/P04/2026-10-21-p04.3/{host-gates.txt,go-test.txt,go-test-verbose.txt,check.txt} |
| P04.3-RACE | P04 | **已执行通过**：22.04 验收 VM（gcc 11.4 + Go 1.27.3）内 `go test -race -count=1 ./...` → 19 包 ok、**0 条 `WARNING: DATA RACE`**；唯一失败 `TestDefaultPasswordParamsTiming` 是 `-race` 把 Argon2id 放大到 1.79—2.03 秒（用例上界 1 秒），不带 `-race` 时 0.6 秒通过。首次跑被 `/etc/upanel/upanel.json` 污染（暴露 Q103），修复后在该机器上复跑全绿。 | docs/evidence/P04/2026-10-21-p04.3/{race-2204.txt,race-final-2204.txt} |
| P04.3-VM | P04 | **已执行通过**：`p04-3-storage-health-acceptance.sh` 在 24.04.5 与 22.04.5 上 root 运行 **通过 77 / 失败 0 / 未执行 1**（NVMe 真实健康，本机只有 SCSI）；**statfs 卡住期间登录 332 ms / 271 ms 返回 200**，同一时刻该网络挂载仍 `reachable=false`；NFS 停服 → 容量 null + 隔离 5 分钟 → 自动恢复；`smartctl` 逐盘交叉验证；13 秒内无 `smartctl` 进程被拉起；`snapshot_version` 不随 I/O 计数变化。 | docs/evidence/P04/2026-10-21-p04.3/{p04-3-2404.txt,p04-3-2204.txt,env-2404.txt,env-2204.txt} |
| P04.3-VM-NVME | P04 | **已执行通过**：`upanel-smart`（24.04.5 + QEMU NVMe 4 GiB）**通过 62 / 失败 0 / 未执行 2**（网络挂载隔离、NFS 卡住时登录——本机无 `nfs-kernel-server`）；NVMe 真实 `healthy`、温度 > 0 且与机器自己的 `smartctl -j` 一致。 | docs/evidence/P04/2026-10-21-p04.3/{p04-3-smart-2404.txt,env-smart.txt} |
| P04.3-UNIT-VM | P04 | 已执行通过：在 22.04 验收 VM 内以 Linux 原生方式运行 `storage.test`（夹具放在 `<bin>/fixtures`），三台 VM 均 **TOTAL PASS 103 / FAIL 0**；`sleeping` 两段式探测、位掩码、速率归零/回绕只在单测覆盖（真机 standby 进不去）。 | docs/evidence/P04/2026-10-21-p04.3/{native-storage-2404.txt,native-storage-2204.txt,native-storage-smart.txt} |
| P04.3-RESULT | P04 | 已补：结论（**passed**）、16 段验收矩阵、三台 VM 结果表、关键证据摘录、本轮修复表（Q101/Q102/Q103/CRLF/脚本缺陷）、**NOT RUN 清单**（真实生产硬件、休眠盘唤醒、ATA 真实输出、SCSI-only 主机上的 NVMe、VM-E 上的 NFS、真实 SATA/USB/RAID/LVM、前端渲染 = P04.4）与重做命令。 | docs/evidence/P04/2026-10-21-p04.3/{result.md,commands.log} |
| P04.3-H2-VM | P04 | **已执行通过**（补强轮，三台 VM 冷启动后重跑、二进制与脚本先核 sha256）：24.04.5 **77** / 22.04.5 **77** / `upanel-smart`（NVMe + QEMU USB）**63** 项通过，**0 失败**（未执行项 2/2/2，均写明原因）；VM-E 上 `USB 盘在拓扑里可见（transport=usb：sdb）` 为 PASS，`sdb` 与另两块盘一起过了逐盘交叉验证。 | docs/evidence/P04/2026-10-21-p04.3/{p04-3-2404-h2.txt,p04-3-2204-h2.txt,p04-3-smart-h2.txt,usb-sdb-and-nvme-health.jsonl} |
| P04.3-H2-UNIT-VM | P04 | 已执行通过：三台 VM 原生 `storage.test` 均 **106 个顶层用例 PASS / FAIL 0**（第一轮 103，新增 USB 桥/`-d sat`/多挂载点三条）；宿主 `go test ./... -v` 计 571 个顶层 PASS（含子测试 176，合计 747）/ 0 FAIL / 6 SKIP。 | docs/evidence/P04/2026-10-21-p04.3/{native-storage-2404-h2.txt,native-storage-2204-h2.txt,native-storage-smart-h2.txt,go-test-verbose-h2.txt} |
| P04.3-H2-RESULT | P04 | 已产出：为什么补（三个缺口）、改了什么（逐文件）、真实捕获命令与原话、产品行为变化前后的 `details` 对照、本轮结果表、环境事故（两台 NFS 机 `reboot` 在 WHPX 挂死、VM-E 又一次 `exitcode=0x0000008b`）、NOT RUN（真实可读 USB 盘、休眠盘、ATA）与复现命令。 | docs/evidence/P04/2026-10-21-p04.3/{hardening.md,check-h2.txt,host-gates-h2.txt} |
| P04.3-H2-RACE | P04 | **已执行通过**：VM-2204（22.04.5，gcc 11.4，Go 1.27.3）内**以 root** `go test -race -count=1 ./...` → **19 包 ok、0 条 `WARNING: DATA RACE`**；唯一失败 `TestDefaultPasswordParamsTiming` 是 `-race` 把 Argon2id 单次校验放大到 **2.39 秒**（用例上界 1 秒），与第一轮同一原因。普通用户跑会在 `internal/agentstore` 因“代理数据目录属主必须是 root”失败（预期行为）。 | docs/evidence/P04/2026-10-21-p04.3/{race-h2-2204.txt} |
| P04.4-UNIT | P04 | 已执行通过：前端 `vue-tsc -b` 0 error、`eslint .` 0 problem、`vitest run` **16 文件 / 192 用例全 passed**（新增 `realdata.test.ts` 17 个真实载荷 SSR 用例、`storage.test.ts` 的两条同容量/多层容量用例）。vitest 环境是 node，没有 jsdom：组件走 `renderToString`，`onMounted` 不执行，所以验的是“同一份 DTO → 同一段 DOM”。 | docs/evidence/P04/2026-10-21-p04.4/{web-gates.txt,check.txt} |
| P04.4-CHECK | P04 | 已执行通过：`sh scripts/check.sh` → **PASS 13 / FAIL 0 / NOT RUN 0**（新增第 13 步 `web-smoke`）；`redocly lint api/openapi.yaml` valid + 9 warnings（与上一阶段同类同量）。第一次跑时 `web-typecheck` 失败过一次：`realdata.test.ts(138,12): error TS18048: 'summary.cpu.per_cpu' is possibly 'undefined'`——只有 `npm run typecheck`（`vue-tsc -b`）能抓到，`vue-tsc --noEmit` 抓不到；改成 `per_cpu?.length` 后通过。 | docs/evidence/P04/2026-10-21-p04.4/{check.txt,web-gates.txt,commands.log} |
| P04.4-SMOKE | P04 | **已执行通过（宿主，真浏览器）**：真实 `upanel-web --mode mock` + Playwright（已登录）→ **PASS 31 / FAIL 0 / SKIP 5**，SKIP 全是“mock 二进制没有 agent socket，没有设备可渲染”，如实记 SKIP；`dist` 预览（未登录守卫）→ **PASS 10 / FAIL 0 / SKIP 0**。覆盖未登录拦截、登录回跳、导航只列已实现模块、概览/存储逐段断言、`AGENT_UNAVAILABLE` → “依赖未安装”而非 0、深链接不落 404、未知路径 404、无未捕获异常与多余失败请求。 | docs/evidence/P04/2026-10-21-p04.4/{p04-4-ui-smoke.txt,check.txt} |
| P04.4-VM | P04 | **已执行通过（Ubuntu 24.04.5 / 6.8.0-147，root 起代理、非 root 起 Web）**：`p04-4-ui-acceptance.sh` **PASS=31 / FAIL=0 / NOT RUN=0**，载荷校验 `p04-4-payload-validate.py` **PASS=856 / FAIL=0**。含 uid 999 的 Web 进程、`agent.sock` 为 `root:upanel-web` 且非 0666、四份响应同快照自洽、17 台块设备与 `lsblk -P` 一一对应、容量与 `df -B1` 相同、16 台设备 `io.reads_total` 不超 `/proc/diskstats`、4 块盘健康与 `smartctl -j -i -H -A` 一致（`available=false → unsupported`）、遍历式 id → 404 且无 `root:`、日志无明文口令。 | docs/evidence/P04/2026-10-21-p04.4/{p04-4-acceptance-2404.txt,payloads/,vm-env-2404.txt} |
| P04.4-DATA | P04 | 已产出：VM 上抓的 8 份真实响应（summary/topology/devices/detail-sda..sdd/traversal）逐字节存入证据目录，其中四份同时放进 `web/src/fixtures/p04-4-vm/` 作前端回归夹具（同一 `snapshot_version: sha256:fd6bdf50…`、`collected_at 2026-10-21T00:19:31Z`），README 写明采集命令、同快照规则、“不要手改”与 NOT RUN 边界。 | docs/evidence/P04/2026-10-21-p04.4/payloads/ 与 web/src/fixtures/p04-4-vm/{README.md,*.json} |
| P04.4-RESULT | P04 | 已产出：范围与逐文件改动、实际执行的检查表、**与 STAGES §6 P04.4 验收条目逐条对应表**、三个缺陷（DeviceCard 把 unknown/unsupported 挂成“健康未通过”；验收脚本早于首次 SMART 采集；校验程序与产品用的 smartctl 参数不一致）与修法、NOT RUN 清单（VM 无浏览器、真实 SATA/NVMe/USB/睡眠盘、真实 dm/LVM/RAID、写操作按计划不启用、viewer 角色）、环境事故（2404/smart 各 panic 一次）与复核命令。 | docs/evidence/P04/2026-10-21-p04.4/{result.md,commands.log} |
| P05.1-UNIT | P05 | 已执行通过：`go build ./...`、`go vet ./...` 退出码 0；`go test ./... -count=1` **21 个包全部 ok**（`internal/services` 新包含 7 份 systemd 255 真实字节的解析回归）。 | docs/evidence/P05/2026-10-21-p05.1/{go-gates.txt,go-test.txt} |
| P05.1-FRONT | P05 | 已执行通过：`vue-tsc --noEmit` 0 error、`eslint .` 0 problem、`vitest run` **20 文件 / 252 用例全 passed**（新增 `realdata-services.test.ts` 22 个真实载荷 SSR 用例）、`vite build` 成功。 | docs/evidence/P05/2026-10-21-p05.1/web-gates.txt |
| P05.1-CHECK | P05 | 已执行通过：`sh scripts/check.sh` → **PASS 13 / FAIL 0 / NOT RUN 0**；`redocly lint api/openapi.yaml` valid + 9 warnings（与上一阶段同类同量）。必须在 `.nvmrc` 的 Node v22.23.3 下跑，系统 Node v24.15.0 会让 `node-version` 项 FAIL。 | docs/evidence/P05/2026-10-21-p05.1/check.txt |
| P05.1-VM | P05 | **已执行通过（Ubuntu 24.04.5 / 6.8.0-147 / systemd 255.4-1ubuntu8.18，root 起代理、非 root 起 Web）**：`p05-1-services-acceptance.sh` **PASS=140 / FAIL=0 / NOT RUN=3**，载荷校验 `p05-1-payload-validate.py` **PASS=22369 / FAIL=0**。含 uid 999 的 Web 进程、`agent.sock` 为 `root:upanel-web` 且非 0666、列表快照 10 秒、239 个单元与枚举逐项对照、模板/别名排除与两种 404、14 个抽样单元状态与 `systemctl` 相同、五步真实启停与读回、四种重复/冲突操作被预检挡住、`ExecStart` 失败 → 任务 `failed` 且 `result={}`、viewer 只读、审计含 `job.submit` 且 `job_id` 非空。 | docs/evidence/P05/2026-10-21-p05.1/{p05-1-acceptance-2404.txt,payloads/,vm-logs-2404/,env.txt} |
| P05.1-DATA | P05 | 已产出：65 份 VM 真实响应逐字节存入证据目录，其中 15 份同时放进 `web/src/fixtures/p05-1-vm/` 作前端回归夹具（`services.json` 239 个单元、`snapshot_version: sha256:700d9c14…`）；`internal/services/testdata/` 另存 7 份 systemd 255 的真实输出（含模板被拒的 stderr 与别名单元的 `Names=`）。 | docs/evidence/P05/2026-10-21-p05.1/payloads/ 与 web/src/fixtures/p05-1-vm/、internal/services/testdata/ |
| P05.1-RESULT | P05 | 已产出：范围与逐文件改动、实际执行的检查表、**与 STAGES §7 P05.1 验收条目逐条对应表**、四个缺陷（模板名中断 `systemctl show` 导致列表只剩 7 行；别名单元让整次采集失败；失败任务 `result` 语义漂移；审计 `job_id` 从未落库）与修法、NOT RUN 清单、已知限制、环境事故与复核命令。 | docs/evidence/P05/2026-10-21-p05.1/{result.md,commands.log} |
| P05.2-UNIT | P05 | 已执行通过：`go build ./...`、`go vet ./...` 退出码 0；`go test ./... -count=1` **22 个包全部 ok**（`internal/processes` 新包含 `/proc` 字节解析、保护判定、脱敏、信号与读回的回归）。 | docs/evidence/P05/2026-10-21-p05.2/{go-gates.txt,go-test.txt} |
| P05.2-FRONT | P05 | 已执行通过：`vue-tsc --noEmit` 0 error、`eslint .` 0 problem、`vitest run` **22 文件 / 295 用例全 passed**（新增 `realdata-processes.test.ts` 31 个真实载荷 SSR 用例）、`vite build` 成功。 | docs/evidence/P05/2026-10-21-p05.2/web-gates.txt |
| P05.2-CHECK | P05 | 已执行通过：`sh scripts/check.sh` → **PASS 13 / FAIL 0 / NOT RUN 0**；`redocly lint` valid + 9 warnings（同类同量）。必须在 `.nvmrc` 的 Node v22.23.3 下跑。 | docs/evidence/P05/2026-10-21-p05.2/check.txt |
| P05.2-VM | P05 | **已执行通过（Ubuntu 24.04.5 / 6.8.0-147 / systemd 255.4-1ubuntu8.18，root 起代理、非 root 起 Web）**：`p05-2-processes-acceptance.sh` **PASS=128 / FAIL=0 / NOT RUN=0**，载荷校验 `p05-2-payload-validate.py` **PASS=9250 / FAIL=0 / SKIP=1**。含 uid 999 的 Web、5 秒快照与 5000 ms 间隔、`cpu_percent` 的 null 语义、TERM/KILL 两套门槛与真实退出、四类保护拒绝且进程未动、**`ns_last_pid` 强制造出的 PID 重用（旧预检 409 `DEVICE_IDENTITY_CHANGED`，新进程毫发无伤）**、viewer 403、审计链。 | docs/evidence/P05/2026-10-21-p05.2/{p05-2-acceptance-2404.txt,payloads/,vm-logs-2404/,env.txt} |
| P05.2-DATA | P05 | 已产出：31 份 VM 真实响应逐字节存入证据目录，其中 22 份同时放进 `web/src/fixtures/p05-2-vm/` 作前端回归夹具（`processes-cpu.json` 139 条、`processes-zombie.json` 138 条、`processes-first.json` 137 条，另有预检/任务/403 信封）；`internal/processes/testdata/` 另存 30 份真机 `/proc` 字节。 | docs/evidence/P05/2026-10-21-p05.2/payloads/ 与 web/src/fixtures/p05-2-vm/、internal/processes/testdata/ |
| P05.2-REGRESSION | P05 | 已执行通过（同机定向回归）：p01-1 30/0、p02-3 87/0、p03-1 93/0、p03-2 102/0、p04-1 72/0、p04-2 110/0、p04-3 77/0（3 NOT RUN：USB、NVMe、隔离到期自愈）、p05-1 PASS=140/0、p05-2 PASS=128/0。第一轮的失败（p04-1 两条陈旧断言、p02-3 上传布局）与修法都在这份文件里。 | docs/evidence/P05/2026-10-21-p05.2/regression-p05-2.txt |
| P05.2-RESULT | P05 | 已产出：范围与逐文件改动、**与 STAGES §7 验收条目逐条对应表**、实际执行的检查表、关键断言原文、本轮修掉的问题（验收读数错位、两条写错的断言、五处跨阶段陈旧断言、VM 上传布局）、NOT RUN 清单、七次 panic 的环境事故与复核命令。 | docs/evidence/P05/2026-10-21-p05.2/{result.md,commands.log} |
| P05.3-UNIT | P05 | 已执行通过：`go build ./...`、`go vet ./...` 退出码 0；`go test ./... -count=1` **24 个包全部 ok**（`internal/journal` 40 例：journald 真实字节解析、参数边界、游标翻页与本地窗口、8 KiB 截断、`page_bytes`；`internal/logs` 22 例：契约校验与错误映射）。 | docs/evidence/P05/2026-10-21-p05.3/{go-gates.txt,go-test.txt} |
| P05.3-FRONT | P05 | 已执行通过：`vue-tsc --noEmit` 0 error、`eslint .` 0 problem、`vitest run` **25 文件 / 347 用例全 passed**（新增 `realdata-logs.test.ts` 24 例：真实载荷 + 真实 SSE 帧）、`vite build` 成功。 | docs/evidence/P05/2026-10-21-p05.3/web-gates.txt |
| P05.3-CHECK | P05 | 已执行通过：`sh scripts/check.sh` → **PASS 13 / FAIL 0 / NOT RUN 0**；`redocly lint` valid + 9 warning。本轮首次跑出 `gofmt:FAIL`（8 个文件未格式化），`gofmt -w` 后转绿。必须在 `.nvmrc` 的 Node v22.23.3 下跑。 | docs/evidence/P05/2026-10-21-p05.3/check.txt |
| P05.3-VM | P05 | **已执行通过（Ubuntu 24.04.5 / systemd 255.4-1ubuntu8.18，root 起代理、非 root 起 Web）**：`p05-3-logs-acceptance.sh` **PASS=132 / FAIL=0 / NOT RUN=1**，载荷校验 **PASS=6466 / FAIL=0 / SKIP=0**。含 uid 999 的 Web、`agent.sock` 为 `root:upanel-web`、30 条标记跨 8 页每标记恰好一次、`level=` 下限语义、五种窗口/参数拒绝、1000 条与 1 MiB 上限（`page_bytes` 截到 261 条 1046028 字节）、秘密抹除、`source=kernel`、401/403/200 三条权限路径、SSE `id:`=游标与 `Last-Event-ID` 续传、**断开后 15 秒内 `journalctl` 子进程归零**、第 5 条流 429、空闲 60 秒 `end(idle_timeout)`。 | docs/evidence/P05/2026-10-21-p05.3/{p05-3-acceptance-2404.txt,payloads/,env.txt} |
| P05.3-DATA | P05 | 已产出：43 份 VM 真实响应（日志页、错误信封、能力表）+ 7 份 curl 原始 SSE 帧逐字节存入证据目录，其中 26 份同时放进 `web/src/fixtures/p05-3-vm/` 作前端回归夹具（`cmp` 核过逐字节相同）；`tests/fixtures/journal/` 另存 3 份真实 journalctl 字节。 | docs/evidence/P05/2026-10-21-p05.3/ 与 web/src/fixtures/p05-3-vm/、tests/fixtures/journal/ |
| P05.3-REGRESSION | P05 | 已执行通过（同机全量 `run-all.sh` 17 个脚本、**1520 项断言**：p01-1 31/0、p01-2 32/2→run B 34/0、p01-4 113/1→run B 114/0、p02-1 58/0、p02-2 103/0、p02-3 88/0、p02-4 87/0、p03-1 93/0、p03-2 102/0、p03-3 73/0、p03-4 78/0、p04-1 72/0、p04-2 110/0/0、p04-3 77/0/3（USB、NVMe、隔离到期自愈 NOT RUN）、p05-1 140/0/3、p05-2 128/0/0、p05-3 132/0/1。run A 那 3 条失败的真实输出都是 `配置错误: 解析配置文件失败 /etc/upanel/upanel.json: EOF`：默认配置路径上留着一份 0 字节残留（所有 root 脚本都用 `cat > $SYS_CFG` 先截断再写，`/etc` 又不随重启清理），不是产品缺陷；`run-all.sh` 已加开跑前警告（Q135）。同批 VM 原生测试 `TOTAL PASS=749 / FAIL=0`） | docs/evidence/P05/2026-10-21-p05.3/{regression-p05-3.txt,vm-logs/,native-tests.txt} |
| P05.3-RESULT | P05 | 已产出：范围与逐文件改动、**与 STAGES §7 验收条目逐条对应表**、实际执行的检查表、关键断言原文、9 个缺陷与修法（含每条钉住它的测试名）、NOT RUN 清单与复核命令。 | docs/evidence/P05/2026-10-21-p05.3/{result.md,commands.log} |
| P06.1-UNIT | P06 | 已执行通过：`go build ./...`、`go vet ./...`（宿主与 linux/amd64 交叉）退出码 0，`gofmt -l ./cmd ./internal` 无输出；`go test ./... -count=1` **25 个包全部 ok**（`internal/files` 新增 54 个用例：路径形状 15 种、符号链接/magic link/硬链接/FIFO/设备拒绝、根身份变化、游标翻页、超限截断、错误判决表；`internal/api` 用假 reader 证明四条路由的 DTO 与 503 分支） | docs/evidence/P06/2026-10-21-p06.1/{go-gates.txt,go-test.txt} |
| P06.1-FRONT | P06 | 已执行通过：`vue-tsc --noEmit` 0 error、`eslint .` 0 problem、`vitest run` **28 文件 / 406 用例全 passed**（新增 `realdata-files.test.ts` 27 例真实载荷回放、`api/files.test.ts` 22 + `components/files.test.ts` 10 共 32 例手写形态）、`vite build` 成功 | docs/evidence/P06/2026-10-21-p06.1/web-gates.txt |
| P06.1-CHECK | P06 | 已执行通过：`sh scripts/check.sh` → **PASS 13 / FAIL 0 / NOT RUN 0**；`redocly lint api/openapi.yaml` valid + 6 warning（历史遗留，本轮新增组件全被引用）。第一次跑 `gofmt:FAIL`（`cmd/upanel-web/main.go` 导入顺序），`gofmt -w` 后转绿。必须在 `.nvmrc` 的 Node v22.23.3 下跑（本机默认是 v24.15.0） | docs/evidence/P06/2026-10-21-p06.1/{check.txt,openapi-lint.txt} |
| P06.1-VM | P06 | **已执行通过（Ubuntu 24.04.5 / 6.8.0-147，root 起代理、uid 999 起 Web）**：`p06-1-files-acceptance.sh` **PASS=115 / FAIL=0 / NOT RUN=0**，载荷校验 **PASS=6379 / FAIL=0**。含九条路径形状断言（越界 422 / 形状 400，宿主 `files.test` 另覆盖 10 种拒绝形状）、符号链接/magic link/硬链接/FIFO 全部拿不到内容且不发生阻塞、根内嵌套 tmpfs 被 `RESOLVE_NO_XDEV` 拒、根被替换 → 409 且重建的根没有被当成原来的根、配置登记 `/` 时代理以退出码 2 拒绝启动、250 文件两页无重叠无跳过且尾页 `next_cursor=null`、50 010 项目录 `truncated=true`、文本与磁盘逐字节一致、`expected_version` 不匹配 409、下载 sha256 与 `stat` 一致且越界下载回 JSON 而非 octet-stream、未登录 401 / viewer 403 各路径断言（admin 的四条 200 由成功用例给出）、能力表无写动作 | docs/evidence/P06/2026-10-21-p06.1/{p06-1-acceptance-2404.txt,payloads/,env.txt} |
| P06.1-UNIT-VM | P06 | 已执行通过：VM 内核上原生跑交叉编译测试二进制，`files.test` **54 个 PASS / 0 FAIL**（`//go:build linux` 的用例第一次真的在 Linux 上执行，正是它抓出了 O_PATH/getdents 与 FIFO 阻塞两条缺陷）、`api.test` PASS | docs/evidence/P06/2026-10-21-p06.1/native-tests.txt |
| P06.1-DATA | P06 | 已产出：44 份 VM 真实响应（根列表、目录页、文本、11 个错误信封、下载头、能力表、配置拒绝输出）逐字节存入证据目录，其中 22 份同时放进 `web/src/fixtures/p06-1-vm/` 作前端回归夹具 | docs/evidence/P06/2026-10-21-p06.1/payloads/ 与 web/src/fixtures/p06-1-vm/ |
| P06.1-RESULT | P06 | 已产出：范围与逐文件改动、**与 STAGES §8 P06.1 验收条目逐条对应表**、实际执行的检查表、5 个缺陷与修法（O_PATH 不能 getdents、FIFO 阻塞、ENOTDIR 误分类、`next_cursor` 空串、ApiErrorPanel 的 title 失效）、2 起流程事故（验收脚本漏端点、inode 复用导致假通过）、NOT RUN 清单、环境事故与复核命令 | docs/evidence/P06/2026-10-21-p06.1/result.md |
| P06.2-UNIT | P06 | 已执行通过：`go build ./...`、`go vet ./...`（宿主与 linux/amd64 交叉）退出码 0，`gofmt -l ./cmd ./internal` 无输出；`go test ./... -count=1` **25 个包全部 ok**（`internal/files` 新增暂存生命周期、预留计数、树统计、复制、备份清理用例；`internal/agent` 新增七个动作的预检阻塞表与 `PARENT_MISSING` 的真实分支） | docs/evidence/P06/2026-10-21-p06.2/{go-gates.txt,go-test.txt} |
| P06.2-UNIT-VM | P06 | 已执行通过：VM 内核上原生跑 12 个包的交叉编译测试二进制，**TOTAL PASS=909 / FAIL=0**。`TestStatTreeCountsDirectories`（entry 4 / 文件 2 / 子目录 1 / 链接 1 / 字节 3）、`TestCopyDirectoryCopiesTree`（含符号链接 → `TREE_UNSUPPORTED` 且目标未创建）、`TestBackupPruneKeepsTen`（12 次覆盖留 10 份、最新 v11）、`TestUploadLifecycleHappyPath`（声明后预留 5 字节）、`TestUploadExpirySweep`（过期后预留归零）——这五个就是本轮四条缺陷的照妖镜 | docs/evidence/P06/2026-10-21-p06.2/native-tests.txt |
| P06.2-FRONT | P06 | 已执行通过：`vue-tsc --noEmit` 0 error、`eslint .` 0 problem、`vitest run` **28 文件 / 428 用例全 passed**、`vite build` 成功 | docs/evidence/P06/2026-10-21-p06.2/{web-gates.txt,web-test.txt} |
| P06.2-CHECK | P06 | 已执行通过：`sh scripts/check.sh` → **PASS 13 / FAIL 0 / NOT RUN 0**；`redocly lint api/openapi.yaml` valid + 6 warning（与 P06.1 同一批历史遗留）。必须在 `.nvmrc` 的 Node v22.23.3 下跑 | docs/evidence/P06/2026-10-21-p06.2/{check.txt,openapi-lint.txt} |
| P06.2-VM | P06 | **已执行通过（Ubuntu 24.04.5 / 6.8.0-147，root 起代理、uid 999 起 Web）**：`p06-2-files-write-acceptance.sh` **通过 228 / 失败 0 / 未执行 0**（§0—§16）。含：声明后目标文件一字节未动、暂存区只有 `.part`、`commit_upload` 后 `staging` 归零且内容逐字节一致、`Content-Length` 不符 400、重复 `PUT` 409 `VERSION_CONFLICT`、放弃后 `reserved` 归零、超配额在预检就 `INSUFFICIENT_SPACE`、外部改文件后提交 **409 `PREFLIGHT_STALE` 且内容未被覆盖**（作业表留 `failed`/`rejected`）、三次覆盖 3 份备份且 `backup_count` 如实、`mkdir` 三种结果、目录复制与 `TREE_UNSUPPORTED`、非空目录不递归 `DIRECTORY_NOT_EMPTY`、递归删除再认证 + 带路径短语 + 条目数 4、`chmod 0640` 读回、`chown` 只收受管理账号名、只读根七动作全 `ROOT_READ_ONLY`、写动作路径形状与 P06.1 同判决且无字节入暂存、viewer 七动作全 403 | docs/evidence/P06/2026-10-21-p06.2/{p06-2-acceptance-2404.txt,payloads/,env.txt} |
| P06.2-RESULT | P06 | 已产出：范围与逐文件改动、**与 STAGES §8 P06.2 验收条目逐条对应表**、实际执行的检查表、关键断言原文、4 类 6 个缺陷与修法（含每条钉住它的测试名）、5 起流程事故、8 条 NOT RUN 与复核命令、已知限制 | docs/evidence/P06/2026-10-21-p06.2/{result.md,commands.log} |
| P06.3-VM | P06 | **已执行通过（Ubuntu 24.04.5 / 6.8.0-147，root 起代理、upanel-web 起 Web）**：`p06-3-terminal-acceptance.sh` **通过 282 / 失败 0 / 未执行 0**（§0—§20）。含：能力表登记 `terminal.create`（动作）与 `terminal.sessions`（查询）且没有任何 exec/shell/command 类能力；匿名 401、viewer 403（四条接口）；`terminal.create` 未再认证被拒 → 再认证 → 任务 `succeeded`；许可只能用一次、30 秒过期、不放在 URL 里、不回显在 101；四道门各自的状态码与审计 stage（无 Origin → 403 `CSRF_FAILED`、错子协议 → 400 `VALIDATION_ERROR`、坏许可 → 403 `FORBIDDEN`、别人/不存在的会话 → 404/409/503 且不泄露存在性）；终端里 `id -un` 是 `upanel-shell`、无特权组、`sudo -n` 失败、`/etc/shadow` 与代理密钥读不到、`/etc/upanel` 写不进、`env` 里没有 `UPANEL_*`；`stty size` 读回 43x132；帧闭集越界一律 `4001`、超速率 `4002` 且浏览器侧真的收到 close 帧；已连接时再连 → 409 `RESOURCE_BUSY`；断流宽限内重连接回同一个 shell（导出变量还在）；宽限到期会话消失且 `pgrep -u upanel-shell` **归零**；60 秒空闲 → `idle`、120 秒（收紧配置）→ `expired`；每账号第 2 个会话预检 `TERMINAL_SESSION_LIMIT`、全局第 3 个任务 `RESOURCE_BUSY`；关闭幂等、非法 `reason` 400、关闭必须带 CSRF；48 条终端审计行、四道门各有 stage、标记串在审计与两份日志里都搜不到、单行 `details` < 300 字节；账号改成 nologin 后代理仍能启动、不登记能力、接口 501；终端之外（系统/存储/服务/进程/日志/文件）回归正常 | docs/evidence/P06/2026-10-21-p06.3/{p06-3-acceptance-2404.txt,payloads/,env.txt} |
| P06.3-UNIT-VM | P06 | 已执行通过：VM 内核上 13 个包的交叉编译测试二进制，**TOTAL PASS=962 / FAIL=0**（terminal 33 / api 200 / agent 206 / storage 159 / agentstore 72 / agentclient 39 / operations 57 / files 80 / monitoring 30 / journal 40 / logs 22 / store 11 / syscmd 13）。其中 `TestManagerGraceExpiryReapsShell` 与 `TestManagerCloseReapsShell` 是本轮 PTY 回收缺陷的照妖镜（断言 `/proc/<pid>` 在 5 秒内消失）；`TestManagerDetachKeepsPTY` 反过来钉住“断开不杀 PTY”；`TestLimitsCannotBeLoosened`、`TestDecodeClientFrame*`、`TestSendableCloseCode`、`TestDenyTargetPointsAtTerminalSession` 各钉住一条契约 | docs/evidence/P06/2026-10-21-p06.3/native-tests.txt |
| P06.3-CHECK | P06 | 已执行通过：`sh scripts/check.sh`（Node v22.23.3）→ **PASS 13 / FAIL 0 / NOT RUN 0**；`redocly lint api/openapi.yaml` valid + 7 warning（第 7 条是只有 101 的 `connect` 操作被判 `operation-2xx-response`，属预期） | docs/evidence/P06/2026-10-21-p06.3/{check.txt,openapi-lint.txt} |
| P06.3-FRONT | P06 | 已执行通过：`vue-tsc --noEmit` 0 error、`eslint .` 0 problem、`vitest run` **29 文件 / 445 用例全 passed**（新增 `terminal.test.ts`）、`vite build` 成功（`TerminalView` 346.92 kB） | docs/evidence/P06/2026-10-21-p06.3/{web-gates.txt,web-test.txt} |
| P06.3-RESULT | P06 | 已产出：范围与逐文件改动、四道门与帧限额表、6 类真机缺陷与修法（Q165—Q169）、验收脚本自身的 7 处缺陷、实际执行的检查表、关键断言原文、5 条 NOT RUN 与原因、已知限制、下一子任务 P07.1 | docs/evidence/P06/2026-10-21-p06.3/{result.md,commands.log} |
| P07.1-VM | P07 | **已执行通过（Ubuntu 24.04.5 / 6.8.0-147，root 起代理、uid 999 起 Web，三块一次性数据盘 sdb/sdc/sdd）**：`p07-1-storage-guard-acceptance.sh` **PASS=189 / FAIL=0 / NOT RUN=1**（唯一 NOT RUN 是 `PREFLIGHT_STALE`，见 result.md §6）。含：先算承载 `/` 的整盘并拒绝任何同源的一次性盘；`supported_actions` 里没有任何 `storage.*` 但四个动作都能预检；系统盘与它的 **4 个分区**（sda1/sda14/sda15/sda16）初始化/删除/格式化一律 `SYSTEM_DISK_PROTECTED` 且锁是整盘；空白盘初始化 `allowed=true` + 影响含“未检测到已知签名不代表盘上绝无原始数据” + 短语首词 `INIT` + 要求再认证 + 前后首尾 4 MiB 哈希不变；提交两道门（未再认证 403 `REAUTH_REQUIRED` 且盘上字节不变、无任务；再认证后 501 `NOT_IMPLEMENTED`，**只多一条** `failed`/`rejected` 的任务、无非终态任务）；已有 GPT 的初始化拒绝与建分区的五种区间错误；整盘 ext4 / 整盘 swap（魔数在第一页末 10 字节）/ LUKS2 头三种签名全部 `FILESYSTEM_SIGNATURE_PRESENT`；挂载中、活动交换区、dm holder、进程打开四类占用各自阻塞且理由带挂载点/pid，**整盘被打开时它上面分区与建分区同样拒**；分区指纹 `sha256:` 形态、两次一致、拓扑变化后不变、同盘不同分区不同；匿名 401 / viewer 403 / viewer 仍能读设备详情；两份日志里没有 `sgdisk/mkfs/wipefs/mkswap/dmsetup create`、没有明文口令；擦回空白后能力回到 `available`、无遗留 swap 与 dm | docs/evidence/P07/2026-10-21-p07.1/{p07-1-acceptance-2404.txt,payloads/,env.txt} |
| P07.1-DATA | P07 | 已产出：17 份 VM 真实预检响应与错误信封逐字节存入证据目录（空白盘允许、系统盘拒绝、GPT 拒绝、五种区间错误、ext4/swap/LUKS 签名、挂载/swap/holders/进程打开/父盘忙、身份两次、能力表、403 与 501 任务形态） | docs/evidence/P07/2026-10-21-p07.1/payloads/ |
| P07.1-CHECK | P07 | 已执行通过：`sh scripts/check.sh`（Node v22.23.3）→ **PASS 13 / FAIL 0 / NOT RUN 0**；`redocly lint api/openapi.yaml` valid + 7 warning（本轮未改契约文件）；宿主 `go test -count=1 ./...` **26 个包 ok**（3 个包无测试文件）、`gofmt -l ./cmd ./internal` 无输出、`go vet ./...` 退出码 0 | docs/evidence/P07/2026-10-21-p07.1/{check.txt,go-test.txt,gofmt-vet.txt} |
| P07.1-FRONT | P07 | 已执行通过（本轮**没有前端改动**，跑的是回归）：`vue-tsc --noEmit` 0 error、`eslint .` 0 problem、`vitest run` **29 文件 / 445 用例全 passed**。设备页仍不渲染任何动作按钮（`StorageDeviceView` 无 capability 渲染），写操作界面等 P07.2 | docs/evidence/P07/2026-10-21-p07.1/{web-test.txt,check.txt} |
| P07.1-RESULT | P07 | 已产出：范围与逐文件改动、13 节验收覆盖表、2 个产品缺陷（交换区魔数在 4096−10 而非盘尾、`holders`/`slaves` 是符号链接目录没有 `list`）、验收脚本自身的 6 处缺陷（含 `lsblk` 树形前缀导致整节“什么都没断言”）、实际执行的检查表、9 条 NOT RUN 与原因、环境备注、复现命令 | docs/evidence/P07/2026-10-21-p07.1/{result.md,probe-swap-holder.sh,probe-holder.sh,repro-create-busy.sh,collect-native.sh} |
| P07.1-UNIT-VM | P07 | 已执行通过：VM 内核上跑五个交叉编译的测试二进制，**587 个顶层用例 PASS / 0 FAIL / 1 SKIP**（storage 162 / agent 161 / api 161 / operations 45 / files 58；SKIP 是 `TestNestedMountRejectedByXDEV`——“需要 root 执行 mount，由 P06.1 验收脚本在 VM 上覆盖”）。本轮两条缺陷的照妖镜：`TestClassifySignature` 的交换区用例与「盘中间出现字面量 `SWAPSPACE2` 必须仍是 unknown」反例（Q171）、`TestLinkNamesReadsDirectoryEntries`（Q172）。采集脚本有两条前置检查：不是 root 就 `exit 2`（`agentstore` 校验数据目录属主 root 0700）、`UPANEL_FIXTURES` 找不到 `storage-2404` 就 `exit 2` | docs/evidence/P07/2026-10-21-p07.1/{native-tests.txt,native-tests-summary.txt,native-storage.txt,native-agent.txt,native-api.txt,native-operations.txt,native-files.txt,env.txt} |
| P07.1-REGRESSION | P07 | 已执行通过（同机定向回归）：`p04-2-storage-acceptance.sh` **通过 112 / 失败 0 / 未执行 0**。第一轮 105/5，五条失败全部是脚本自身的陈旧/脆弱断言（能力表写死、`health.checked_at` 一律 null、`capabilities` 一律空数组、§9 用 §2 的旧基线算间距），改法见 result.md §4 第 8 条；改完断言从 110 增加到 112。这一轮要证明的是：**`linkNames()` 改成读目录项之后，只读拓扑没有被带坏**（系统盘保护、`snapshot_version` 只由内容决定、容量与 statvfs 一致、回环挂载的进出都在） | docs/evidence/P07/2026-10-21-p07.1/{regression-p04-2.txt,regression-p04-2-first.txt} |
| P07.2-VM | P07 | **已执行通过（Ubuntu 24.04.5 / 6.8.0-147，root 起代理、uid 999 起 Web，三块一次性数据盘 sdb/sdc/sdd）**：`p07-2-storage-write-acceptance.sh` **PASS=177 / FAIL=0 / NOT RUN=1**（`REMOTE_EXIT=0`；唯一 NOT RUN 是 `DEVICE_IDENTITY_CHANGED`，见 result.md §8）。含：**真的写了盘**——空白盘初始化 GPT（`label: gpt` 只走 stdin，模板固定，`FirstUsableLBA=2048`/`LastUsableLBA=16777182`、盘尾备份头在位、首尾 4 MiB 哈希变了）、建分区起点 1048576（1 MiB 对齐）且 `/dev/<part>` 与 sysfs 当场就在、第二个分区名字 `data` 用 `sgdisk -i 2` 这个**别的工具**读回、删除后槽位不重排；写完 `udevadm settle` + 等内核节点，再按“盘上 GPT 头/条目表”和“内核/sysfs 分区”两源逐条比对，**都对上才算 `verified`**（零退出码不算）；幂等重放返回同一条任务且不写第二次；预检之后用宿主工具改盘 → 409 `PREFLIGHT_STALE` 且盘上字节不变；系统盘 → 409 `DEPENDENCY_BLOCKED` 且不产生任务；格式化仍 501；并发只允许一个成功，输掉的那个停 `step: revalidate`、`code: PREFLIGHT_STALE`；两份日志里没有 `sfdisk/sgdisk/wipefs`、没有明文口令；擦回空白后能力回到 `available` | docs/evidence/P07/2026-10-22-p07.2/{run.log,env.txt,logs/,payloads/} |
| P07.2-DATA | P07 | 已产出：14 份 VM 真实响应逐字节存入 `payloads/`（初始化/建分区/删除三类任务的 `verified` 结果与 `report_data`、设备详情里 `disk_entries` 与 `partition_table.partitions` 的对应关系、`PREFLIGHT_STALE` 与 `DEPENDENCY_BLOCKED` 两种拒绝信封、并发里输掉的那一个的 `job-conc-a.json`/`job-conc-b.json`）；其中 `device-two-partitions.json` 直接用作前端回放夹具（Q85：不手写夹具） | docs/evidence/P07/2026-10-22-p07.2/payloads/ |
| P07.2-CHECK | P07 | 已执行通过：`sh scripts/check.sh`（Node v22.23.3、Go 1.27.3）→ **PASS 13 / FAIL 0 / NOT RUN 0**（含 go-build/go-vet/go-test/go-build-linux/go-vet-linux/gofmt/node-version/web-typecheck/web-lint/web-test/web-build/web-smoke/openapi-lint）；`redocly lint api/openapi.yaml` valid + 7 warning（本轮未改契约文件）；另单独跑 `go test -count=1 ./...` **26 个包 ok / 3 个包无测试文件**、`gofmt -l ./cmd ./internal` 无输出、`go vet ./...` 退出码 0（不吃 check.sh 的缓存） | docs/evidence/P07/2026-10-22-p07.2/{check.txt,go-test.txt} |
| P07.2-FRONT | P07 | 已执行通过（本轮前端只做 DTO 与回放测试，**没有写操作界面**，界面等 P07.5）：`vue-tsc --noEmit` 0 error、`eslint .` 0 problem、`vitest run` **30 文件 / 450 用例全 passed**（新增 `realdata-storage-write.test.ts` 5 例：条目表与内核分区按槽位一一对应、字节数是十进制字符串、起点 1 MiB 对齐、`type_guid` 是 Linux data、名字读得回来、`storage.filesystem.format` 不以 `available` 出现）、`vite build` 成功 | docs/evidence/P07/2026-10-22-p07.2/{check.txt} |
| P07.2-RESULT | P07 | 已产出：范围与逐文件改动、13 节覆盖表与逐节断言数、4 条“只有真机才知道的事实”、1 个产品缺陷（Q177）、4 处验收脚本自身缺陷（Q178—Q181）与 1 处测试工具缺陷（`run-native-tests.sh` 把 `rc≠0` 的超时/崩溃算成 0 失败）、机会性证据 Q182（`runner` 第二次 `revalidate` 在真实竞态里确实触发）、实际执行的检查表、逐条 NOT RUN 与原因、与 P07.1 的差异、复现命令与 sha256 | docs/evidence/P07/2026-10-22-p07.2/{result.md} |
| P07.2-UNIT-VM | P07 | 已执行通过：VM 内核上跑 13 个交叉编译的测试二进制，**TOTAL PASS=1134 / FAIL=0**、13 个包全 `rc=0`（agentstore 72 / agent 240 / agentclient 39 / operations 57 / syscmd 16 / store 11 / api 200 / files 80 / terminal 33 / monitoring 30 / **storage 294** / journal 40 / logs 22）。storage 从 P07.1 的 162 涨到 294，新增覆盖即写盘执行器（模板校验、重算判决、身份/状态版本比对、两源核验、幂等重放、身份变化拒绝）。跑了两遍：改脚本前后各一遍，两遍数字一致（第一遍留底 `native-tests-run1-unbounded.txt`） | docs/evidence/P07/2026-10-22-p07.2/{native-tests.txt,native-tests-run1-unbounded.txt,env.txt} |
| P07.2-REGRESSION | P07 | 已执行通过（同机定向回归，同一批一次性盘）：①`p04-2-storage-acceptance.sh` **通过 114 / 失败 0 / 未执行 0**（只读拓扑没被写盘执行器带坏：系统盘保护、`snapshot_version` 只由内容决定、容量与 statvfs 一致）；②`p07-1-storage-guard-acceptance.sh` **PASS 194 / FAIL 0 / NOT RUN 1**（唯一 NOT RUN 仍是 `PREFLIGHT_STALE`，本轮由 P07.2 验收补上）。p07-1 从 189 变 194 是脚本自己这轮的改动（§2 +1 跨阶段不变量、§5 +4：**§5 必须换动作**，否则“再认证后提交初始化”在 P07.2 起会真的写盘） | docs/evidence/P07/2026-10-22-p07.2/{regression-p04-2.txt,regression-p07-1.txt} |
| P07.3-VM | P07 | **已执行通过（Ubuntu 24.04.5 / 6.8.0-147，root 起代理、uid 999 起 Web，三块一次性盘 sdb/sdc/sdd）**：`p07-3-storage-format-acceptance.sh`（sha256 `3d70edcc…13b1e2`，1296 行）**PASS=204 / FAIL=0 / NOT RUN=1**（`REMOTE_EXIT=0`；唯一 NOT RUN 是 `DEVICE_IDENTITY_CHANGED`，见 result.md §8）。含：环境与安全守卫（三块盘都与承载 `/` 的整盘不同源、硬编码的 `mkfs.ext4`/`mkfs.xfs` 真在、版本可追溯）→ 能力表里格式化已可执行 → ext4 格式化并**两源核验**（超级块直读 + `blkid` 这个独立证人；零退出码不算）→ **确认短语错** 400 `CONFIRMATION_MISMATCH` 且整份预检作废、盘上一个字节没写 → 同分区改 XFS（UUID 换、`-f` 在 argv）→ **标签超限**（ext4 17B / xfs 13B）在预检之前就 400 且不落任务（§5，字节数来自 mkfs 实测）→ **禁止取消**（冻住 `mkfs.ext4` 现场 `comm=mkfs.ext4`/`stat=T`/任务 `running/executing` → 取消 409 `VALIDATION_ERROR`、不是 `canceled`、`cancellable=false` → 解冻后 `succeeded`/`verified`）→ **错误退出**（64 MiB 做 xfs：预检允许、mkfs 退出码 1 → `COMMAND_FAILED`，消息带退出码 + `larger than 300MB` + “盘上现在是” + 重新提交提醒，分区首 4 MiB 哈希不变）→ **执行前二次身份检查**（结构变化：版本 `sha256:2683d3fa…`→`sha256:23d53538…`，409 `PREFLIGHT_STALE`，目标分区字节/UUID/ext4 签名全不变；换位：409 `DEVICE_IDENTITY_CHANGED`）→ **进程中断**（冻住 `mkfs.xfs` 后 `SIGKILL` 代理 → 重启 reconcile 落 `interrupted` + `OPERATION_INTERRUPTED` + “实际状态未知”，任务数不变、不自动重跑）→ 日志卫生（Web 日志无 `mkfs\|blkid`、两份日志无明文口令）→ 收尾（擦盘无残留；不存在的设备的预检是 `allowed=false` + `DEVICE_NOT_FOUND`，**不是 500**） | docs/evidence/P07/2026-10-22-p07.3/run.log |
| P07.3-DATA | P07 | 已产出：20 份 VM 真实响应逐字节存入 `payloads/`（ext4/xfs 格式化任务的 `verified` 结果与 `report_data`、错短语 403/400 与作废后重放、标签超限两份、小分区命令失败、结构变化被拒、换位被拒、中断任务、不存在的设备的预检、格式化能力表），以及 `logs/`（agent.log、web.log、`lsblk-initial/final.txt`） | docs/evidence/P07/2026-10-22-p07.3/{payloads/,logs/} |
| P07.3-CHECK | P07 | 已执行通过：`sh scripts/check.sh`（Node v22.23.3、Go 1.27.3）→ **PASS 13 / FAIL 0 / NOT RUN 0**（go-build/go-vet/go-test/go-build-linux/go-vet-linux/gofmt/node-version/web-typecheck/web-lint/web-test/web-build/web-smoke/openapi-lint）；另单独跑 `go test ./... -count=1` **26 个包 ok**（EXIT=0，不吃缓存）、`gofmt -l internal/ cmd/` 无输出、`redocly lint api/openapi.yaml` valid + 7 warning（与上一轮同一批） | docs/evidence/P07/2026-10-22-p07.3/{check.txt,native-tests.txt} |
| P07.3-MEASURE | P07 | 已产出（真机实测，决定验收怎么写）：`mkfs.ext4` 1 GiB 43/53/46 ms、`mkfs.xfs` 1 GiB 89/64/103 ms、3 GiB ext4 166 ms / xfs 132 ms（**所以取消用例必须先把进程冻住**，否则窗口来不及）；ext4 不加 `-F` 能覆盖 ext4/xfs/随机内容；xfs 不加 `-f` 被已有签名拒；ext4 `-L` 给 17 字节 rc=0 但**静默截断**成 16、xfs `-L` 给 13 字节 rc=1；两者都拒已挂载分区；xfs 拒 <300 MB（§9“错误退出”用例的来源） | docs/evidence/P07/2026-10-22-p07.3/{measurements.txt,measurements-mkfs.txt} |
| P07.3-UNIT | P07 | 已执行通过：宿主 `go test ./... -count=1` 26 包；`internal/storage` 包 **PASS 328 / FAIL 0**（同一口径对同一个包：P07.2 提交 `882b6d5` 是 291，本轮 +37，即 212 顶层 + 116 子测试）：新增 `format_test.go` 覆盖 argv 组装（`-F` 不出现、`-f` 出现、标签/`-L` 位置）、标签字节数上限（ext4 16 / xfs 12，按字节不是字符）、模板校验与身份/状态版本比对、`COMMAND_FAILED` 消息四要素、`fsprobe` 的 UUID/签名/超级块直读；另新增 `TestStoragePreflightForMissingDeviceIsNot500`（Q190 回归锁） | docs/evidence/P07/2026-10-22-p07.3/native-tests.txt |
| P07.3-REGRESSION | P07 | 已执行通过（同机定向回归，同一批一次性盘，串行）：①`p07-1-storage-guard-acceptance.sh` **PASS 184 / FAIL 0 / NOT RUN 2**（NOT RUN 两条：§5.2“过门之后仍然 501”这个现场在 P07.3 之后已不存在、由 `TestStorageSubmitWithoutExecutorIs501` 顶上；§10 `PREFLIGHT_STALE` 由 p07-2/p07-3 在一次性盘上验。§5 从 18 条降到 8 条是因为那段原本对**四个动作各跑一遍**“再认证后仍然 501”，现在四个动作都真的可执行）；②`p04-2-storage-acceptance.sh` **通过 114 / 失败 0 / 未执行 0**（只读拓扑没被格式化执行器带坏） | docs/evidence/P07/2026-10-22-p07.3/{regression-p07-1.txt,regression-p04-2.txt} |
| P07.3-RESULT | P07 | 已产出：范围与逐文件改动、14 节覆盖表与逐节断言数（27/8/4/10/6/37/18/12/14/12/25/12/9/10）、关键断言原文与读数（版本前后值、冻结现场、失败消息四要素）、1 个产品缺陷（Q190：对不存在的设备做预检回 500）与 3 类验收脚本缺陷（冻结机制四次翻车、收尾探针吞 500、§10a 设计错）、1 条 NOT RUN 与替代证据、实际执行的检查表、已知限制、下一子任务 P07.4 | docs/evidence/P07/2026-10-22-p07.3/result.md |
| P07.4-VM | P07 | **已执行通过（Ubuntu 24.04.5 / 6.8.0-147，root 起代理、uid 999 起 Web，两块一次性盘 sdb/sdc，夹具全部由产品自己的动作造）**：`p07-4-storage-volume-acceptance.sh`（sha256 `a4144ef5…`，1463 行）第一段 **PASS=267 / FAIL=0 / NOT RUN=0**（EXIT=0）。含：环境与安全守卫 → 代理 root / Web 非 root / `受管理卷已接线` → 五个卷动作可执行、空列表 `[]`、形状不对 400、不存在 404 → 夹具（产品的 GPT → 分区 → ext4，`blkid` 独立读回）→ **导入的最小修改**（条目逐字、非空行只多四行、块外逐字节未变、权限属主未动、备份内容等于写之前、无临时残留、导入不挂载、重复导入被拒且 fstab 不动）→ **挂载两源核验**（`findmnt` + 拓扑重扫、UUID 与 `blkid` 一致、真写文件）→ 卸载 → 重挂载文件一致 → **占用两道门**（预检 `VOLUME_BUSY` 点名 pid 并与 `/proc/N/fd` 对照；受理时变忙回 **400 `VALIDATION_ERROR`** 且留 `failed/rejected` 任务；执行时冻住产品自己的 `umount` 吃 `rc=32 target is busy`，消息含重扫出的 `6108/sleep` 与“当前占用”）→ 开机自动挂载的 fstab 行与 `findmnt --verify` → 外部编辑导致 `PREFLIGHT_STALE` 且手工内容不被覆盖 → 候选不合格时目标文件逐字节不动 → 第二个卷不受影响 → 停面板后卷仍挂着 | docs/evidence/P07/2026-10-22-p07.4/run.log |
| P07.4-POSTBOOT | P07 | **已执行通过（中间隔着一次真重启 `shutdown -r now`）**：第二段 `PHASE=post-boot` **PASS=38 / FAIL=0 / NOT RUN=0**（EXIT=0）。**先证明、后启动任何 upanel 进程**：开机 `07:55:24`、`srv-upanel-volumes-data.mount` 的 `ActiveEnterTimestamp` 是 `07:55:50`（由 fstab 自己挂上）、挂载点 UUID 与登记一致、`/proc/self/mountinfo` 里有它、systemd 单元 `active`、标记文件 sha256 不变、**没开自动挂载的第二个卷没有挂上**、重启没有改动 fstab、`/run` 已清空（没有遗留 socket）；然后启动面板确认 `observed_state=mounted`/`capacity` 非空/挂着但没人用，最后卸载 + 退出管理（只减一行、BEGIN/END 标记保留、留备份、之后 404、另一个卷还在） | docs/evidence/P07/2026-10-22-p07.4/run-postboot.log |
| P07.4-DATA | P07 | 已产出：VM 真实响应 payload（导入/挂载/卸载/改配置/退出管理的任务与预检、`jobs-race-accept.json`、占用三态、卷列表与详情、错误形状）、`logs/`（agent.log、web.log、`fstab-initial/fstab-before-reboot/fstab-prefix-now`、`findmnt-verify-after-automount.txt`、`lsblk-initial.txt`、`kernel.txt`）、`state/`（`frozen-umount.pid.trace` 等抓窗口诊断）、`state.env`（跨重启期望值），以及**两轮失败的第二段日志**（`logs/postboot-attempt1.log` 没建 `/run/upanel`、Q202；attempt2 的 `BIN_DIR` 权限，Q203） | docs/evidence/P07/2026-10-22-p07.4/{payloads/,logs/,state/,state.env} |
| P07.4-CHECK | P07 | 已执行通过：`sh scripts/check.sh`（Node v22.23.3 钉在 PATH 首位、Go 1.27.3）→ **PASS 13 / FAIL 0 / NOT RUN 0**（web-smoke 10/0/0）；不钉 PATH 时唯一失败项是 `node-version`（宿主 Node v24.15.0 vs `.nvmrc` 22.23.3，环境漂移不是代码），已如实记录在 evidence `env.txt`。另 `go test ./... -count=1` **26 包 ok**、`gofmt -l`/`go vet` 干净、`redocly lint api/openapi.yaml` valid + 7 warning（本轮未改 openapi） | docs/evidence/P07/2026-10-22-p07.4/{check.txt,native-tests.txt} |
| P07.4-UNIT | P07 | 已执行通过：`internal/storage` **508** 条（同口径 P07.3 是 328，**+180**，即 `-v` 的顶层 + 子测试 PASS 行）、`internal/agent` 278、`internal/api` 211、`internal/agentstore` 90。新增：fstab 渲染与写入（选项顺序、`nofail` 恒在、`noauto` 随 automount、第六列 ext4=2/XFS=0、块外逐字保留、渲染相同则早退、候选不合格不动原文件）、五个动作的判决与执行器（导入回滚两条、卸载 EBUSY 文案、退出管理只删自己一行）、占用合流（`ScanMount` 找开文件者、挂载点不存在、读不到 `/proc` fail-closed、挂着但空闲不算忙）、以及 Q199 的**跨动作不变量** `TestAssessVolumeActionsBindTargetIdentity` | docs/evidence/P07/2026-10-22-p07.4/native-tests.txt |
| P07.4-MEASURE | P07 | 已产出（真机实测，决定验收怎么写）：先写独立探针 `probe-umount-ebusy.sh` 证明“冻住产品自己 fork 的 `umount` 之后，执行端的 EBUSY 分支真的可达”——第一次就抓住（`FROZEN=27445 comm=umount`、任务 `failed`/`COMMAND_FAILED`、消息含 `27450/sleep`），再把机制搬进脚本（Q201）；另有两个事实探针 `probe-volume-facts.sh`、`probe-verify-facts.sh` 记录 fstab/挂载/UUID 的现场 | docs/evidence/P07/2026-10-22-p07.4/{measurements-umount-ebusy.txt,probe-umount-ebusy.sh,probe-volume-facts.sh,probe-verify-facts.sh} |
| P07.4-RESULT | P07 | 已产出：范围与逐文件改动、两段覆盖表与逐节断言数（31/9/13/33/57/26/16/31/19/8/17/5/2 + A9/B11/C18）、关键断言原文与读数（导入后的 fstab 差异、受理时 400 与任务行、执行端 `target is busy` 全文、开机时间戳与 sha）、1 个产品缺陷（Q199）与 5 条验收脚本自身缺陷（Q197/Q198/断言表达式/Q202/Q203）、5 条 NOT RUN 与替代证据、实际执行的检查表、已知限制（含 `api/openapi.yaml` 尚未跟上）、下一子任务 P07.5 | docs/evidence/P07/2026-10-22-p07.4/result.md |
| P07.5-VM | P07 | **已执行并通过**：Ubuntu 24.04.5 真机，`tests/integration/p07-5-storage-page-acceptance.sh`（1098 行，root，`SPARE_DISKS="sdb sdc"`）**PASS=205 / FAIL=0 / NOT RUN=3**（EXIT=0），11 节 | docs/evidence/P07/2026-10-22-p07.5/{run.log,env.txt} |
| P07.5-EXTERNAL | P07 | 已执行通过（真机，**不经过面板**）：`scripts/storage-external-check.sh` 对真 `/etc/fstab` 跑三遍（一挂一空 28 项断言 / 退出管理后 / 全空）每遍 `RESULT 一致`；五种坏副本（缺 `nofail`、fsck 顺序 0、UUID 不存在、重复挂载点、BEGIN/END 不配对）各 `RESULT` 不一致且退出码 1，干净副本退出码 0——**注入只动副本，`/etc/fstab` 一字节不动** | docs/evidence/P07/2026-10-22-p07.5/logs/{ext-real.txt,ext-after-forget.txt,ext-final.txt,ext-fault-*.txt,ext-expect-wrong.txt} |
| P07.5-PAYLOAD | P07 | 已产出：页面真正要读的响应与任务链原样落盘（拓扑、整盘/分区详情、卷列表/详情、占用列表、代理能力表、fstab 各阶段快照、lsblk、kernel 视图）+ 58 份真实响应 JSON + 4 份外部检查原始输出 | docs/evidence/P07/2026-10-22-p07.5/{payloads/,logs/} |
| P07.5-WEB | P07 | 已执行通过（mock integration，真机载荷回放）：`web/src/fixtures/p07-5-vm/` 11 份载荷由 `realdata-p07-5.test.ts`（16 条）回放；web **35 文件 / 552 用例**全绿；浏览器冒烟新增两条（受管卷区块读到了结果、设备详情页有「服务端判定的动作」） | docs/evidence/P07/2026-10-22-p07.5/check.txt；web/src/fixtures/p07-5-vm/README.md |
| P07.5-UNIT | P07 | 已执行通过：`go test ./... -count=1` **26 包 ok**（本轮**没改 Go 代码**，storage 508 / agent 278 / api 211 与 P07.4 同口径，一致是预期的）、`go vet`/`gofmt` 干净、`GOOS=linux go build` exit 0 | docs/evidence/P07/2026-10-22-p07.5/native-tests.txt |
| P07.5-CHECK | P07 | 已执行通过：宿主 `scripts/check.sh` **PASS 13 / FAIL 0 / NOT RUN 0**；`api/openapi.yaml` lint **valid，7 warning** | docs/evidence/P07/2026-10-22-p07.5/check.txt |
| P07.5-RESULT | P07 | 已产出：逐文件改动、逐节断言数（27/9/10/39/28/18/9/29/13/22 + 收尾 1）、关键断言原文与读数（挂载容量 `2040373248` / 卸载后 `capacity: null`、外部检查 28 项全文、空托管块 `条目 0`）、**1 个产品缺陷（Q208）**、3 条验收脚本自身缺陷（Q204/Q209/CRLF）、3 条 NOT RUN 与替代证据、实际执行的检查表、已知限制、下一子任务 P08.1 | docs/evidence/P07/2026-10-22-p07.5/result.md |
| P08.1-VM | P08 | **已执行并通过（24.04.5 一次性 VM，382 项断言，`FAIL=0`）** | docs/evidence/P08/2026-10-22-p08.1/{run.log,payloads/,logs/} |
| P08.1-HOST | P08 | 已执行通过（`check.sh` PASS 13 / FAIL 0 / NOT RUN 0） | docs/evidence/P08/2026-10-22-p08.1/check.txt |
| P08.1-UNIT | P08 | 已执行通过（`go test ./...` 全部 ok；`internal/shares` 54 用例 0 FAIL） | docs/evidence/P08/2026-10-22-p08.1/native-tests.txt |
| P08.1-RESULT | P08 | 已执行（含 4 个产品缺陷、5 处脚本缺陷、Q222 隔离实验） | docs/evidence/P08/2026-10-22-p08.1/{result.md,env.txt} |
| P08.2-VM | P08 | **已执行并通过（24.04.5 一次性 VM，15 节 237 项断言，`FAIL=0 / NOT RUN=0`；四遍，前三遍的红是真缺陷）** | docs/evidence/P08/2026-10-22-p08.2/{run.log,payloads/,logs/} |
| P08.2-HOST | P08 | 已执行通过（`check.sh` PASS 13 / FAIL 0 / NOT RUN 0；openapi valid，7 warning） | docs/evidence/P08/2026-10-22-p08.2/check.txt |
| P08.2-UNIT | P08 | 已执行通过（`go test ./...` 28 包全 ok；本轮 4 条新测试全 PASS） | docs/evidence/P08/2026-10-22-p08.2/native-tests.txt |
| P08.2-RESULT | P08 | 已产出（含 Q233 产品缺陷与 Q234 契约事实的复盘、逐节断言表、NOT RUN 清单、已知限制） | docs/evidence/P08/2026-10-22-p08.2/{result.md,env.txt} |
| P08.2-PROBE | P08 | 已执行（`hosts allow` 对环回永远放行的可复现记录；跑完 `smb.conf` `sha256` 与跑前一致） | docs/evidence/P08/2026-10-22-p08.2/{probe5.sh,probe5-output.txt} |
| P08.3-VM | P08 | **已执行并通过（24.04.5 一次性 VM，14 节 207 项断言，`FAIL=0 / NOT RUN=5`；三遍，前两遍的红是 2 个真缺陷 + 2 处脚本缺陷）** | docs/evidence/P08/2026-10-22-p08.3/{run.log,runs/,payloads/,logs/} |
| P08.3-REBOOT | P08 | 已执行通过（三阶段 + 一次真 `reboot`：pre 18 / post 11 / clean 5，各 0 失败；挂载单元比 `smbd` 早 57.2 秒，`smb.conf` 重启前后逐字节相同） | docs/evidence/P08/2026-10-22-p08.3/logs/reboot-{pre,post,clean}.txt、logs/reboot-{journal,systemd-before,systemd-after}.txt |
| P08.3-HOST | P08 | 已执行通过（`check.sh` PASS 13 / FAIL 0 / NOT RUN 0） | docs/evidence/P08/2026-10-22-p08.3/check.txt |
| P08.3-UNIT | P08 | 已执行通过（`go test ./...` 全部包 ok；本轮 3 条新测试，其中 `TestWriteFileSyncEnforcesMode` 在 Windows 上 SKIP、由真机 0644 断言补上） | docs/evidence/P08/2026-10-22-p08.3/native-tests.txt |
| P08.3-MIGRATE | P08 | 已执行通过（升级路径探针：16 列库 DROP 回 15 列 + 删最高版本迁移记录 → 起代理后列补回、旧行取空串、其余迁移不重跑） | docs/evidence/P08/2026-10-22-p08.3/{probe-migration-upgrade.sh,probe-migration-upgrade.txt} |
| P08.3-RESULT | P08 | 已产出（三遍运行的逐条失败与根因、Q240/Q243 产品缺陷与 Q241/Q244 脚本缺陷复盘、逐节断言表、NOT RUN 清单、已知限制） | docs/evidence/P08/2026-10-22-p08.3/{result.md,env.txt} |
| P08.4-VM | P08 | **已执行并通过（24.04.5 一次性 VM，11 节 190 项断言，`FAIL=0 / NOT RUN=4`；四遍，前三遍的红是 2 个真缺陷 + 5 处脚本缺陷）** | docs/evidence/P08/2026-10-23-p08.4/{run.log,runs/,payloads/,logs/} |
| P08.4-CLIENT | P08 | 已执行通过（逐字执行页面说明里的 11 行：真 `smbclient` 连上并列出内容；`mount.cifs` + `0600` 凭据文件挂到 `/mnt/data`、通过挂载点写入读回 `sha256` 一致、`umount` 干净；四种身份全拿真实 `NT_STATUS_*`） | docs/evidence/P08/2026-10-23-p08.4/{logs/run.log 第 5、6 节,payloads/client-*,payloads/smbclient-*} |
| P08.4-HYGIENE | P08 | 已执行通过（代理/Web 日志、58 份载荷、两个数据库含 `-wal`/`-shm`、`/proc/*/cmdline` 与 `environ`、`/proc/mounts` 的密码命中数全 0；凭据文件 `0600`、目录 `0700`） | docs/evidence/P08/2026-10-23-p08.4/run.log 第 7 节 |
| P08.4-FIXTURE | P08 | 已执行通过（13 份真机载荷回灌 `web/src/fixtures/p08-4-vm/`，27 条回放断言：记录键闭集、三状态分开、卷 join、撤销块五字段、账号无秘密、说明命令与真机跑过的是同一字符串） | web/src/fixtures/p08-4-vm/、web/src/components/realdata-p08-4.test.ts |
| P08.4-HOST | P08 | 已执行通过（`check.sh` PASS 13 / FAIL 0 / NOT RUN 0；前端 637 条用例全绿） | docs/evidence/P08/2026-10-23-p08.4/check.txt |
| P08.4-UNIT | P08 | 已执行通过（`go test ./...` 全部包 ok；本轮新增 `TestExecuteShareApplyClearsRuntimeRevocation`） | docs/evidence/P08/2026-10-23-p08.4/native-tests.txt |
| P08.4-RESULT | P08 | 已产出（四遍运行的逐条失败与根因、Q246/Q247 产品缺陷与 5 处脚本缺陷复盘、逐节断言表、NOT RUN 清单、已知限制、下一子任务 P09.1） | docs/evidence/P08/2026-10-23-p08.4/{result.md,env.txt} |
| P09.1-VM | P09 | **已执行并通过（24.04.5 一次性 VM，11 节 210 项断言，`FAIL=0 / NOT RUN=0`；第八遍为最终一遍，第七遍是紧接着的脏盘复跑，同样 210/0/0）** | docs/evidence/P09/2026-10-22-p09.1/{run.log,payloads/,logs/} |
| P09.1-DB | P09 | 已执行通过（独立读库：`schema_migrations` 最高版本 7、`binding_json` 规范化形式、`uq_apps_binding` 索引存在、分组关联前后各读一次、收藏按用户计数、依赖行落库/清除、`jobs` 行数不变、`audit_events` 动作与 `target='app:…'`、UPDATE 被触发器 ABORT） | docs/evidence/P09/2026-10-22-p09.1/run.log 第 2、4、7、8、9 节 |
| P09.1-DEP | P09 | 已执行通过（依赖卷三种结局：代理停 → 503 `AGENT_UNAVAILABLE` 且 `apps` 行数不变；卷不在列表 → 422 `DEPENDENCY_MISSING` + `details.volume_id`；真卷 → 201 + `unmanaged`，并在 sdd 上真做分区/格式化/导入） | docs/evidence/P09/2026-10-22-p09.1/{run.log 第 8 节,payloads/job-p09-*,logs/wipefs.log,logs/fstab.before} |
| P09.1-HOST | P09 | 已执行通过（`check.sh` PASS 13 / FAIL 0 / NOT RUN 0，含 gofmt、Linux 交叉编译、前端四项、openapi lint 7 warning） | docs/evidence/P09/2026-10-22-p09.1/check.txt |
| P09.1-UNIT | P09 | 已执行通过（`go test ./...` 全部包 ok；本轮新增 `internal/apps`（store/validate）与 `internal/api/apps_test.go`） | docs/evidence/P09/2026-10-22-p09.1/native-tests.txt |
| P09.1-RESULT | P09 | 已产出（八遍运行里 23 个 FAIL 的逐条根因——全部是脚本自身问题、产品语义“预检绑定快照”的记法、逐节断言表、NOT RUN 清单、已知限制、下一子任务 P09.2） | docs/evidence/P09/2026-10-22-p09.1/{result.md,env.txt,collect-env.sh} |
| P09.2-VM | P09 | **已执行并通过（24.04.5 一次性 VM，10 节 120 项断言，`FAIL=0 / NOT RUN=1`；六遍 93/0 → 120/0，最终一遍留档；唯一 NOT RUN 是“卷离线禁止启动受管理依赖应用”，属 P09.4）** | docs/evidence/P09/2026-10-22-p09.2/{run.log,payloads/,logs/} |
| P09.2-DOCKER | P09 | 已执行通过（真 `docker.io` 29.1.3 + `alpine:3.20` 夹具：running 但 HTTP 500 → `error` + `status_code=500`；同名重建后 ID 变化 → 旧绑定进 `missing_bindings` 且要求重新确认、新容器未被自动绑定、旧 ID 不再是候选；`systemctl stop docker.socket docker.service` → 来源非 ok、容器应用**不**进 `missing_bindings`、快照整体 `partial`；docker-proxy 发布端口不再产生重复候选） | docs/evidence/P09/2026-10-22-p09.2/{run.log 第 3b、8b 节,logs/docker-run-*.log,payloads/discovery-*} |
| P09.2-SSRF | P09 | 已执行通过（代理侧探测只拼 `127.0.0.1`，`Proxy: nil`；非回环地址/完整 URL 在 `target_id` 语法层面写不出来；全轮抓包与日志里云元数据地址命中 0；代理不可用 503 且不写库） | docs/evidence/P09/2026-10-22-p09.2/{run.log 第 1、4、5、6、9 节,logs/agent.log,logs/web.log} |
| P09.2-HOST | P09 | 已执行通过（`check.sh` PASS 13 / FAIL 0 / NOT RUN 0，含 gofmt、Linux 交叉编译、前端四项、openapi lint） | docs/evidence/P09/2026-10-22-p09.2/check.txt |
| P09.2-UNIT | P09 | 已执行通过（`go test ./...` 全部包 ok；本轮新增 `internal/discovery`、`internal/apphealth`、`internal/api/apps_discovery_test.go`，含 `TestProbeMissingTargetVerdict` / `TestCollectDropsShadowedSocketActivation` / `TestCollectDropsDockerProxyInsideDockerUnit` / `TestAppDiscoverySkipsIssuesWhenSourceFailed`） | docs/evidence/P09/2026-10-22-p09.2/native-tests.txt |
| P09.2-RESULT | P09 | 已产出（逐节断言表、3 个产品缺陷与 1 条语义修正的根因、5 处验收脚本缺陷、NOT RUN 清单与原因、systemd 首轮采集窗口的观察记录、复现步骤、下一子任务 P09.3） | docs/evidence/P09/2026-10-22-p09.2/{result.md,env.txt,collect-env.sh} |
| P09.3-VM | P09 | **已执行并通过（24.04.5 一次性 VM，13 节 217 项断言，`FAIL=0 / NOT RUN=1`；第五遍为最终一遍，四遍 197/6 → 214/1 → 217/0 收敛；唯一 NOT RUN 是“执行前复核（受理之后再变）”，需毫秒级可重复竞态，由 `TestContainerControlExecuteStaleState` 覆盖）** | docs/evidence/P09/2026-10-22-p09.3/{run.log,payloads/,logs/} |
| P09.3-READONLY | P09 | 已执行通过（容器/镜像/网络/数据卷/部署五类只读视图；Web 账号不在 docker 组、`docker ps` 被拒、Web 进程 `/proc/<pid>/fd` 里没有 docker.sock；inspect 从不请求 `Config.Env` 与完整标签表，唯一口令 `vm-p093-env-secret-7c41` 在全部响应体与日志里命中 0） | docs/evidence/P09/2026-10-22-p09.3/{run.log 第 3、6、7 节,payloads/containers.json,payloads/container-detail.json} |
| P09.3-RISK | P09 | 已执行通过（四类危险容器各起真容器：privileged / host 网络 / 危险 capability / 挂 docker.sock，全部 `risk.level=high` + `managed=false` + 三项能力 false + 对应原因码；预检 `CONTAINER_NOT_MANAGED`；提交被拒的预检 409 + `DEPENDENCY_BLOCKED`，现场确认 privileged 容器**没被停掉**） | docs/evidence/P09/2026-10-22-p09.3/{run.log 第 3、9 节,payloads/preflight-stop-{priv,hostnet,sock,cap}.json} |
| P09.3-CONTROL | P09 | 已执行通过（stop/start/restart 各一轮：预检含目标指纹（只由 ID 决定）、影响说明、`COMPOSE_MEMBER` 告警、锁=容器自身、规范化参数只有 ID 与操作；任务终态 + `observed_state.verified` + 读回状态 + `docker inspect` 现场对照；状态冲突是 `CONTAINER_STATE_CONFLICT`，陈旧预检 409 `PREFLIGHT_STALE` 且作业表留一条 `failed`） | docs/evidence/P09/2026-10-22-p09.3/{run.log 第 8、9 节,payloads/preflight-*,payloads/job-*} |
| P09.3-LOGS | P09 | 已执行通过（一页从新到旧、`next_cursor`=最旧时间戳、往回翻页每条严格早于游标且非空、游标被篡改/`since` 形状错/`until` 在未来 全部 400、不存在容器 404、口令 `vm-p093-log-secret-2b93` 脱敏成 `[redacted]`；SSE 收到 entry 事件且事件 id 是时间戳、不存在的容器开流是 404 不是 error 帧） | docs/evidence/P09/2026-10-22-p09.3/{run.log 第 5、10 节,payloads/logs-*.json,payloads/stream.sse} |
| P09.3-UNAVAIL | P09 | 已执行通过（代理停掉 → 503 `AGENT_UNAVAILABLE`；dockerd 停掉 → 列表 200 + `available=false` + `notice`（提到 Docker）+ 空数组，详情与日志 503 `DOCKER_UNAVAILABLE`，预检 200 + `allowed=false` + 阻塞码 `DOCKER_UNAVAILABLE`，提交 409；恢复后 `docker info` 正常） | docs/evidence/P09/2026-10-22-p09.3/{run.log 第 11 节,payloads/containers-agentdown.json,payloads/containers-dockerdown.json,payloads/container-dockerdown.json,payloads/container-logs-dockerdown.json} |
| P09.3-HOST | P09 | 已执行通过（`check.sh` PASS 13 / FAIL 0 / NOT RUN 0，含 gofmt、Linux 交叉编译、前端四项、openapi lint 7 warning） | docs/evidence/P09/2026-10-22-p09.3/check.txt |
| P09.3-UNIT | P09 | 已执行通过（`go test ./...` 全部包 ok；本轮新增 `internal/containers`（types/parse/docker/logs/control/http/fetch）、`internal/agent/containers_test.go`、`internal/api/containers_test.go`，容器相关 57 个用例，含 `TestLogsOlderPageUsesWindowWithoutTail` / `TestLogsOlderPageWidensWhenWindowIsTooQuiet` / `TestLogsOlderPageShrinksWhenOutputTruncated` / `TestDaemonDownIsUnavailable` / `TestListDegradesWhenDaemonDies` / `TestAssessControlDaemonDiesMidwayIsBlocked` / `TestFetchKeepsDockerUnavailable` / `TestContainerControlExecuteStaleState`） | docs/evidence/P09/2026-10-22-p09.3/native-tests.txt |
| P09.3-RESULT | P09 | 已产出（逐节断言表、3 个产品缺陷（Q270/Q271/Q272）的根因与复现、6 处验收脚本缺陷、NOT RUN 清单与原因、已知限制、下一子任务 P09.4） | docs/evidence/P09/2026-10-22-p09.3/{result.md,env.txt,collect-env.sh} |
| P09.4-VM | P09 | **已执行并通过（24.04.5 一次性 VM，11 节 165 项断言，`FAIL=0 / NOT RUN=0`，退出码 0）** | docs/evidence/P09/2026-10-22-p09.4/{run.log,payloads/,logs/} |
| P09.4-STATE | P09 | 已执行通过（摘要来自观测：link `not_applicable`；systemd 的 `active` 与 `reloading` 都归 `running`；**绑在不存在的单元 → `unknown` 而不是 `stopped`** 且 note 要求重新确认绑定；docker running/stopped 两种各给对应能力；容器被重建后旧 ID → `unknown` + 重新确认绑定 + 不给 start 也不给 logs；库里 `binding_json` 仍是旧 ID，名字相同也不自动接手） | docs/evidence/P09/2026-10-22-p09.4/{run.log 第 3、7 节,payloads/app-sysd-missing.json,payloads/app-docker-rebuild.json} |
| P09.4-PROTECTION | P09 | 已执行通过（无依赖卷 → `not_applicable` + note 为 null；docker `restart=no` + 依赖卷 → `managed` 且 note 说“不会自己启动”；`restart=always` + 依赖卷 → `unmanaged` 且 note 点名 restart 策略；systemd + 依赖卷 → `unmanaged` 且 note 说面板没改写 `RequiresMountsFor/BindsTo`） | docs/evidence/P09/2026-10-22-p09.4/{run.log 第 5 节,payloads/app-docker-gated.json,payloads/app-docker-always.json,payloads/app-sysd.json} |
| P09.4-GATE | P09 | 已执行通过（未挂载的依赖卷 → 预检 200 + `allowed=false` + `VOLUME_OFFLINE` + 阻塞项带卷标识；提交 409 `DEPENDENCY_BLOCKED` 且 `docker inspect` 现场对照容器仍是 `exited`；**对照三组**：stop 不被拦、未绑定容器不被拦、把卷挂起来之后同一份 start 预检 `allowed=true`；挂载/卸载都走产品自己的卷接口） | docs/evidence/P09/2026-10-22-p09.4/{run.log 第 6 节,payloads/preflight-start-gated.json,payloads/job-submit-gated.json,payloads/preflight-start-mounted.json} |
| P09.4-LOGS | P09 | 已执行通过（`/apps/{id}/logs` 按绑定判别：systemd 应用 `source=journal` + `page` 与 `/logs` 同形且没有 `container_id`；docker 应用 `source=docker` + `page` 与 `/containers/{id}/logs` 同形且没有 `unit`/`level`；`unit`/`source`/`level`/`from`/`tail` 跨来源一律 400；link 422、compose 501 `NOT_IMPLEMENTED`；viewer 403、匿名 401 而摘要仍可读） | docs/evidence/P09/2026-10-22-p09.4/{run.log 第 8 节,payloads/applogs-journal.json,payloads/applogs-docker.json} |
| P09.4-500 | P09 | 已执行通过（容器 running 但 HTTP 500：从发现结果取到 `cntr_upn-p094-500` 与端口，探测得 `http_state=error` + `status_code=500` + `runtime_state=running` 三者并存，`last_checked_at` 有值；独立证据 `curl` 直接打夹具端口也是 500） | docs/evidence/P09/2026-10-22-p09.4/{run.log 第 2、4 节,payloads/health-500.json,payloads/app-docker-500.json} |
| P09.4-UNAVAIL | P09 | 已执行通过（代理停掉：`/apps` 仍 200、docker 应用 `runtime_state=unknown` + note「这一轮没读到」、protection 同时降级成 `unmanaged`、link 不受影响、应用日志 503；重启代理后恢复） | docs/evidence/P09/2026-10-22-p09.4/{run.log 第 9 节,payloads/apps-agent-down.json} |
| P09.4-HOST | P09 | 已执行通过（`check.sh` PASS 13 / FAIL 0 / NOT RUN 0，含 gofmt、Linux 交叉编译、前端四项、openapi lint 7 warning；`gofmt` 修掉了 `internal/api/apps.go` 的 import 顺序后才全绿——修完重新交叉编译并**重跑**了真机验收，`env.txt` 里的二进制指纹与最终 `run.log` 是同一份） | docs/evidence/P09/2026-10-22-p09.4/{check.txt,env.txt} |
| P09.4-UNIT | P09 | 已执行通过（`go test -count=1 ./...` 全部包 ok；本轮新增 `internal/appruntime/appruntime_test.go`（481 行，状态映射/能力/保护/门禁）、`internal/api/applogs_test.go`（371 行，判别与参数拒绝）、`internal/apps` 的 target 判别） | docs/evidence/P09/2026-10-22-p09.4/native-tests.txt |
| P09.4-RESULT | P09 | 已产出（逐节断言表、10 处验收脚本缺陷的根因、0 个产品缺陷、NOT RUN=0 与三条声明式不覆盖、已知限制、下一子任务 P09.5） | docs/evidence/P09/2026-10-22-p09.4/{result.md,env.txt,collect-env.sh} |
| P09.5-VM | P09 | **已执行并通过（24.04.5 一次性 VM，13 节 211 项断言，`FAIL=0 / NOT RUN=0`，退出码 0）** | docs/evidence/P09/2026-10-22-p09.5/{run.log,payloads/,logs/} |
| P09.5-CARD | P09 | 已执行通过（卡片画四件事且互不吞：`runtime_state` + `http_state` + `dependency_protection` + `runtime_note`；**容器 running 但 HTTP 500** 时三条同时成立 `running`/`error`/`status_code=500`，且仍给得出 stop；重建后的旧绑定四能力全 false、note 要求重新确认、发现结果把它列进 `missing_bindings`；受保护单元只剩 `logs`；link 只有 `open`） | docs/evidence/P09/2026-10-22-p09.5/{run.log 第 3—5 节,payloads/apps.json,payloads/app-docker-500.json,payloads/health-error.json} |
| P09.5-ENTRY | P09 | 已执行通过（入口是**服务器地址**：`http://10.0.2.15:18897/` 真回 200，而**同一端口在 127.0.0.1 上是 000**；回环 / `localhost` / 带用户名密码 / `javascript:` 四种 `public_url` 在写入口就 400；`target_id` 填 `169.254.169.254`、主机名、完整 URL 三种形状全 400，`path` 塞完整 URL 也 400——SSRF 是结构性的，没有位置能放主机） | docs/evidence/P09/2026-10-22-p09.5/{run.log 第 6 节,payloads/app-link-files.json,payloads/app-link-media.json} |
| P09.5-GROUP | P09 | 已执行通过（`group_id` 过滤：形状对但不存在 → 200 + 空、**形状不对 → 400 `VALIDATION_ERROR`** 且消息点名分组标识（Q277）；`q` 不是服务端参数（`GET /apps?q=…` 结果集不变，搜索在页面）；非空分组删除 → 422 `DEPENDENCY_BLOCKED` 且成员没被顺手移走；收藏幂等、按用户存） | docs/evidence/P09/2026-10-22-p09.5/{run.log 第 7 节,payloads/app-groups.json} |
| P09.5-CONTROL | P09 | 已执行通过（卡片上的 stop/start 落成 **`service.control`**（`{unit, operation}`）并走预检 → 任务 → `observed_state` 核验：`previous_state=active` → `active_state=inactive`、`verified=true`；停机后能力翻成 `start`、启动后翻回 `stop`；`ssh.service` 预检 `allowed=false` 且**没有提交出任务**，现场仍在跑） | docs/evidence/P09/2026-10-22-p09.5/{run.log 第 8 节,payloads/job-appstop.json,payloads/job-appstart.json,payloads/preflight-sshstop.json} |
| P09.5-GATE | P09 | 已执行通过（卷未挂载 → 启动预检 `allowed=false` + `VOLUME_OFFLINE`、提交 409 `DEPENDENCY_BLOCKED`、`docker inspect` 现场仍是 `exited`；**对照**：stop 不被拦、卷挂上之后同一份 start `allowed=true`、`always` 容器是 `unmanaged` 且 note 点名 restart 策略） | docs/evidence/P09/2026-10-22-p09.5/{run.log 第 9 节,payloads/preflight-start-gated.json,payloads/preflight-start-mounted.json} |
| P09.5-PERM | P09 | 已执行通过（viewer 读应用/分组 200，读发现、建应用、建分组、读应用日志全 403；viewer 收藏 200 且**admin 的列表里它仍未收藏**——收藏按用户存；匿名读/收藏 401） | docs/evidence/P09/2026-10-22-p09.5/{run.log 第 10 节} |
| P09.5-REPLAY | P09 | 已执行通过（真机载荷 16 份取自 VM，`realdata-p09-5.test.ts` 回放 **51 条断言**：字段闭集、三状态分开、能力来自对象、`open` 是锚点不是请求、分桶规则、启停动作名、任务 `observed_state`） | web/src/fixtures/p09-5-vm/、web/src/components/realdata-p09-5.test.ts |
| P09.5-HOST | P09 | 已执行通过（`check.sh` PASS 13 / FAIL 0 / NOT RUN 0，含 gofmt、Linux 交叉编译、前端四项、openapi lint 7 warning（与 P08 以来同口径）；`go test -count=1 ./...` 全部包 ok；前端 40 文件 / 720 用例；`typecheck`/`lint` 干净） | docs/evidence/P09/2026-10-22-p09.5/{check.txt,native-tests.txt} |
| P09.5-RESULT | P09 | 已产出（逐节断言表、4 处失败的根因（1 处契约文字 + 3 处脚本毛病）、0 个产品缺陷、NOT RUN=0 与四条声明式不覆盖、已知限制、下一子任务 P10） | docs/evidence/P09/2026-10-22-p09.5/{result.md,env.txt,collect-env.sh} |
| P10.1-VM | P10 | **已执行并通过（24.04.5 一次性 VM + nginx 1.24.0，7 节 100 项断言，`FAIL=0 / NOT RUN=0`，退出码 0）** | docs/evidence/P10/2026-10-22-p10.1/{run.log,logs/,env.txt} |
| P10.1-FACTS | P10 | 已执行通过（`GET /proxies/nginx`：`version=1.24.0`、`unit_state=active`、`config_root=/etc/nginx`、4 份配置；`sites-enabled/default` 是符号链接且**只登记一次**（按真实路径去重）；`names_line=3` 与 `46` 如实带出；`warnings=[]`） | docs/evidence/P10/2026-10-22-p10.1/{payloads/nginx-facts.json,run.log 第 2 节} |
| P10.1-CONFLICT | P10 | 已执行通过（`clear` / `taken`（证据指到 `server_name` 行 + 文件 + 原文）/ `unknown`（正则、坏文件）三态；`*.x` 占子域不占上级域；**`unknown` 连无关域名也一并阻断**（409）且库里无记录；删掉夹具后同一域名恢复 `clear` 的正对照） | docs/evidence/P10/2026-10-22-p10.1/run.log 第 3 节 |
| P10.1-VALIDATE | P10 | 已执行通过（面板端口 = 真实监听端口 19495 → 400；通配符/带协议/非法目标/`cidr` 空名单/`public` 带网段/重复账号全 400 且库里 0 条；`P101-OK.Example.COM.` → `p101-ok.example.com`；默认 1048576/60/public/http；`secret_ref` 不在响应也不在审计） | docs/evidence/P10/2026-10-22-p10.1/run.log 第 4 节 |
| P10.1-NOWRITE | P10 | 已执行通过（`/etc/nginx` 全部文件内容摘要**前后相等** + `nginx -t` 仍通过 + `conf.d` 无 `p101-*` 残留 + `managed_file_count=0`；审计两条 `proxy.create` 均含 `"applied":false`） | docs/evidence/P10/2026-10-22-p10.1/{run.log 第 7 节,logs/nginx-t.log} |
| P10.1-UNAVAIL | P10 | 已执行通过（停代理：列表仍 200 + `nginx.read_error=agent_unavailable` + `config_missing=false`；登记与冲突判断 503 `AGENT_UNAVAILABLE` 且库不变；重启后同一请求 201；viewer 三条 403、未登录 401） | docs/evidence/P10/2026-10-22-p10.1/run.log 第 6 节 |
| P10.1-ATTEMPTS | P10 | 已保存（第 1 遍 78 FAIL：脚本把库目录留给 root → `attempt to write a readonly database`；第 2 遍 6 FAIL：**1 个产品缺陷**（Q279，`nginx -v` 5ns）+ 4 处脚本期望写错 + 1 处连锁） | docs/evidence/P10/2026-10-22-p10.1/{run-attempt-1.log,run-attempt-2.log} |
| P10.1-HOST | P10 | 已执行通过（`check.sh` PASS 13 / FAIL 0 / NOT RUN 0，含 gofmt、Linux 交叉编译、前端四项、openapi lint 7 warning（与 P08 以来同口径）；`go test ./...` 37 个包全部 ok；首跑 gofmt 红在 `internal/api/proxies_test.go` 已修） | docs/evidence/P10/2026-10-22-p10.1/{check.txt,native-tests.txt} |
| P10.1-RESULT | P10 | 已产出（逐节断言表、1 个产品缺陷的根因与复现、2 遍脚本失败的根因、NOT RUN 五条声明式不覆盖、已知限制、下一子任务 P10.2） | docs/evidence/P10/2026-10-22-p10.1/result.md |
| P10.2-VM | P10 | **已执行并通过**（VM 24.04.5 + nginx 1.24.0：PASS=153 / FAIL=0 / NOT RUN=7，退出码 0；写入/reload/幂等/回滚/`applied` 推导/改名/停用/删除门禁/basic auth 全部真机断言；16 次预检 14 次 5–433ms、最慢 1698ms（紧随一次 apply），2 秒门禁通过；**这一遍是换过二进制之后重跑的，与提交的源码对应**） | docs/evidence/P10/2026-10-23-p10.2/{run.log,logs/,payloads/,env.txt} |
| P10.2-PROBE | P10 | 已执行（Q290 复现：后台服务采集扫掠期间现场问 `systemctl show nginx.service` 6.389s vs 读快照 113–252ms；另附 argon2id 单次 1.4–3.9s 的实测，Q292） | docs/evidence/P10/2026-10-23-p10.2/{probe-unit-timing.txt,argon2-timing.txt} |
| P10.2-HOST | P10 | 已执行通过（`gofmt -l` 无输出、`go vet ./...` 通过、Linux 交叉编译三个二进制成功、`go test ./...` 34 个包全部 ok） | docs/evidence/P10/2026-10-23-p10.2/{check.txt,native-tests.txt} |
| P10.2-RESULT | P10 | 已产出（范围、产品代码清单、检查结果、Q290 的三次定位过程含一次错误的"反证"、脚本自身的 4 处毛病、NOT RUN 七条、机器收尾状态） | docs/evidence/P10/2026-10-23-p10.2/result.md |
| P10.3-VM | P10 | **已执行并通过**（VM 24.04.5 + nginx 1.24.0 + certbot 2.9.0 + Pebble：PASS=176 / FAIL=0 / NOT RUN=7，退出码 0；staging 必填、签发落地与目录权限、uid 999 读不到私钥、绑定后 443 真服务上游、冷却 429 且不产生任务、续期换指纹不换标识、重新应用因证书轮换而 reload、手动导入走通、`expiring`/`expired`、22 条私钥出口搜索、审计留痕；**这一遍是换过二进制之后重跑的**，三个二进制 sha256 见 `env.txt` 且与 `check.txt` 交叉编译产物一致） | docs/evidence/P10/2026-10-23-p10.3/{run.log,logs/,payloads/,env.txt} |
| P10.3-ROTATE | P10 | 已执行通过（`job-apply-a3.json`：`file_state=unchanged` + `reload=done` + `reload_reason=cert_rotated` + `served_cert_fingerprint=a185e5be…`（旧）+ `served_cert_after_reload=2b8e7891…`（新，等于盘上指纹）+ `served_cert_probe_attempts=2`，无陈旧告警；`job-apply-a4.json` 回到 `skipped_unchanged` 且 reload 理由为空——幂等重放没被弄坏） | docs/evidence/P10/2026-10-23-p10.3/payloads/ |
| P10.3-KEY | P10 | 已执行通过（拿一把真实私钥的正文当搜索词逐出口搜：web/agent/init 三份日志、certificates/preflights/jobs/job_events/audit_events 五个表、预检参数、任务结果、全部响应体；证书接口不吐 `key_path`/`cert_path`/`key_secret_ref`；`/etc/letsencrypt/live` 0700 且 uid 999 读不到 privkey） | docs/evidence/P10/2026-10-23-p10.3/run.log（第 5、10 段） |
| P10.3-HOST | P10 | 已执行（`gofmt -l` 无输出、`go vet ./...` 通过、Linux 交叉编译三个二进制成功且 sha256 与 VM 上安装的一致、`go test ./...` 36 个包 ok；`check.sh` PASS 12 / FAIL 1 = `node-version`，宿主 v24.15.0 vs `.nvmrc` 22.23.3，未切 nvm，也没记成通过） | docs/evidence/P10/2026-10-23-p10.3/{check.txt,native-tests.txt} |
| P10.3-ATTEMPTS | P10 | 已保存（四遍收敛 114/37/8 → 165/2/7 → 172/4/7 → 176/0/7；第 1 遍 37 个失败归 7 个根因、其中 4 处是脚本自身；第 2 遍抓到产品缺口"续期之后新证书不生效"；第 3 遍 2 个产品 + 2 个脚本都源于 `nginx -s reload` 的异步性） | docs/evidence/P10/2026-10-23-p10.3/result.md（§5） |
| P10.3-RESULT | P10 | 已产出（范围表、产品代码清单与行数、检查结果、四遍收敛与四个失败根因、NOT RUN 七条、限制六条、机器收尾状态） | docs/evidence/P10/2026-10-23-p10.3/result.md |
| P10.4-PKG | P10 | **已执行通过**（`release.sh` 打 amd64 + arm64；`SHA256SUMS` 覆盖两个包，包内另有一份逐文件校验和；把包内某个文件改一个字节，同一个核对必须失败） | docs/evidence/P10/2026-10-23-p10.4/{env.txt,logs/2204-install-acceptance.log,logs/2404-install-acceptance.log} |
| P10.4-INSTALL | P10 | **已执行并通过（22.04.5 与 24.04.5 各 PASS=136 / FAIL=0 / NOT RUN=0，退出码 0）** | docs/evidence/P10/2026-10-23-p10.4/logs/{2204-install-acceptance.log,2404-install-acceptance.log} |
| P10.4-CORE | P10 | **已执行并通过（22.04.5 与 24.04.5 各 PASS=105 / FAIL=0 / NOT RUN=0，退出码 0）** | docs/evidence/P10/2026-10-23-p10.4/logs/{2204-core-scenario.log,2404-core-scenario.log} |
| P10.4-GROUP | P10 | 已执行通过（22.04：PASS=15 / FAIL=0；「组有成员」拒绝并点名 + 给 `gpasswd -d`，「组为空」复用并补建账号，装完两单元 active、HTTPS 200、能登录） | docs/evidence/P10/2026-10-23-p10.4/logs/2204-installer-group-branches.log |
| P10.4-RELOAD | P10 | 已执行（Q309 的正面证据：`job-apply-a-tls.json` 两台都是 `reload=done` / `served=true` / `unit_state=active`，并记下 `unit_state_attempts` 与 `unit_state_error`） | docs/evidence/P10/2026-10-23-p10.4/payloads/{2204,2404}/job-apply-a-tls.json |
| P10.4-PROBE | P10 | 已执行（`systemctl is-active nginx` 单次耗时取样：22.04 空闲 n=40 max=813ms；24.04 验收期间 n=40 max=16489ms，另有修复前一次 11420ms） | docs/evidence/P10/2026-10-23-p10.4/env.txt |
| P10.4-HOST | P10 | 已执行（`gofmt -l` 无输出、`go vet ./...` 退出 0、`go test ./...` 36 个包 ok、`check.sh` 前端 10 + Go 13 全过，node v22.23.3） | docs/evidence/P10/2026-10-23-p10.4/{check.txt,native-tests.txt,native-tests-raw.txt} |
| P10.4-TUNNEL | P10 | 已执行通过（22.04 与 24.04 各 PASS=9 / FAIL=0；**在宿主机上跑**：隧道未建时本机端口不可达 → `ssh -N -L` 转发后 HTTPS 200、证书链与主机名校验通过（未加 `-k`）、带 HSTS、明文说话拿不到业务响应 → 关隧道端口重新不可达） | docs/evidence/P10/2026-10-23-p10.4/logs/{2204-ssh-tunnel.log,2404-ssh-tunnel.log} |
| P10.4-RESULT | P10 | 已产出（结果汇总、按节 PASS/FAIL/NOT RUN、3 个产品缺陷与 9 处验收脚本缺陷、1 处只点名不改的产品粗糙面、NOT RUN 清单） | docs/evidence/P10/2026-10-23-p10.4/result.md |
| P11.1-VM | P11 | **已执行并通过（22.04.5 与 24.04.5 各 PASS=64 / FAIL=0 / NOT RUN=7，退出码 0）**；逐条对账 `SCOPE=installed ENTRIES=676/731 CHECKED=676/731 MISMATCH=0`；`bind9-libs:amd64` 的候选两侧都读得到（`1:9.18.39-0ubuntu0.22.04.7` / `…0.24.04.8`）；首次采集 411ms / 11611ms | docs/evidence/P11/2026-10-23-p11.1/logs/{2204,2404}-p11-1-acceptance.log |
| P11.1-VERIFY | P11 | 已执行通过（`tests/integration/p11-1-packages-verify.py` 不信 Web 的实现，自己跑 `dpkg-query`/`apt-cache policy` 再比；修正前它对 arch 限定名复刻了同一个 bug，改法不同之后才敢当对账） | docs/evidence/P11/2026-10-23-p11.1/payloads/{2204,2404}/verify.txt |
| P11.1-REPLAY | P11 | 已执行通过（真机载荷回放 19 项：24.04 的 7 份夹具原样抓取） | web/src/fixtures/p11-1-vm/（`README.md` 有清单与抓取时刻）、web/src/components/realdata-p11-1.test.ts |
| P11.1-HOST | P11 | 已执行（`scripts/check.sh` PASS 13 / FAIL 0 / NOT RUN 0：`go vet`、`gofmt` 无输出、`go test ./...` ok、web typecheck/lint/build/smoke、web 42 文件 759 用例、openapi lint 通过；node v22.23.3） | docs/evidence/P11/2026-10-23-p11.1/logs/host-check.txt |
| P11.1-PROBE | P11 | 已执行（`apt-cache policy` 耗时：22.04 676 名 384ms、24.04 731 名冷缓存 26.6s；首次调用到响应 411ms / 11.6—15.6s；启动预热实测把 26s 拖成 62s 并触发 `AGENT_UNAVAILABLE`，已删除） | docs/evidence/P11/2026-10-23-p11.1/env.txt |
| P11.1-RESULT | P11 | 已产出（范围、代码清单、按节 PASS/FAIL/NOT RUN、arch 限定名缺陷的真机证据与歧义保护、apt 成本实测与超时分层、5 条限制） | docs/evidence/P11/2026-10-23-p11.1/{env.txt,result.md} |
| P11.2-VM | P11 | **已执行并通过（22.04.5 与 24.04.5 各 PASS=169 / FAIL=0 / NOT RUN=4，两次 exit=0）**；逐节断言数两台完全相同，按 `== 节标题 ==` 分段从日志数出（非估算）；真装 cowsay+sl、真升夹具包（conffile 本地改动保留、2.0 未覆盖、`.dpkg-dist` 落盘）、真卸、真硬杀 apt（`dpkg --audit` 空、之后 apt 可用）、收尾机器回到原样 | docs/evidence/P11/2026-10-23-p11.2/logs/{2204,2404}-p11-2-acceptance.log |
| P11.2-HOST | P11 | 已执行（`scripts/check.sh` PASS 13 / FAIL 0 / NOT RUN 0：`go vet`、`gofmt` 无输出、`go test ./...` ok、web typecheck/lint/build/smoke、web 43 文件 791 用例、openapi lint 通过） | docs/evidence/P11/2026-10-23-p11.2/logs/host-check.txt |
| P11.2-UNIT | P11 | 已执行（`//go:build unix` 的三个包交叉编译成 linux/amd64 测试二进制，在 22.04 上各自源码根目录（cwd 带 `testdata/`）原生跑：`syscmd` 24 通过 +1 SKIP、`processes` 41 通过、`apt` 77 通过；**首次在 Linux 上执行**，抓出 `TestProbeLocksNeverRemoves` 与本包语义矛盾的断言） | docs/evidence/P11/2026-10-23-p11.2/logs/linux-unit-tests.txt |
| P11.2-PROBE | P11 | 已执行（24.04 上 `apt-get -s` 预检 34.6/13.0/47.2/10.8/40.4s，提交时再模拟 38.7/33.9/29.0s，装 cowsay+sl ≈6 分半，升 `zlib1g` >6 分钟未终态；22.04 同批秒级、`figlet` 安装 <1.2s。据此定下 75s/120s/90s/100s/95s/21 分钟六层等待） | docs/evidence/P11/2026-10-23-p11.2/env.txt |
| P11.2-SCRIPT | P11 | 已执行（`tests/integration/p11-2-package-write-acceptance.sh`，1103 行 16 节，md5 675b0eee…；脚本自建 `file:` 源夹具 + 两个 conffile 包；退出码由 `/root/run-p11-2.sh` 捕获，原文见 run-launcher.sh） | docs/evidence/P11/2026-10-23-p11.2/run-launcher.sh、tests/integration/p11-2-package-write-acceptance.sh |
| P11.2-RESULT | P11 | 已产出（范围、代码清单、逐节 PASS/FAIL/NOT RUN、六条真机缺陷、8 条限制） | docs/evidence/P11/2026-10-23-p11.2/{env.txt,result.md} |
| P11.3-VM | P11 | **已执行并通过（22.04.5 与 24.04.5 各 PASS=109 / FAIL=0 / NOT RUN=2，四阶段 exit=0）**；22.04 跨一次**真重启**（`591a2533→5389231a`，阶段二读到时 uptime 79 秒）与一次**真断电**（`4a25a825`）；24.04 的 boot_id 也换了（`97057b04→22486dd5→813e082b`），但**重启那一跳是宿主冷启动**——QEMU 11.1 + WHPX 4/4 挂住（`WHPX: Unexpected VP exit code 4`），TCG 下 24.04 开不了机（`IO-APIC + timer doesn't work!`），已写在证据限制②；24.04 关机后 QEMU 进程从宿主消失（串口 `reboot: Power down`）；逐节断言数两台完全相同（按 `== 节标题 ==` 分段从日志数出，非估算）；两条 NOT RUN 分别在阶段二 §7 与阶段三 §9 由同一事实的 PASS 覆盖 | docs/evidence/P11/2026-10-23-p11.3/logs/{2204,2404}-{phase-pre,phase-post,phase-final,results}.txt |
| P11.3-FIX | P11 | 已执行（产品缺陷 A：电源操作把自己算成在跑的关键任务 → `POWER_JOBS_RUNNING`；修法 `PlanInput.OperationID → AssessRequest.ExcludeOperationID → InFlight(exclude)`，闸门判据不动。产品缺陷 B：`prepareSocketPath` 无条件删除已存在的 socket，把正在服务的代理的路径删掉（第 1 轮 22.04 的 6 条 503）——改成先探测后决定，`socketprobe_{linux,other}.go` 承载平台错误分类。回归：`TestAssessExcludesSelfWhenRevalidating` 三支、`TestPowerPlanPassesOperationIDToPower`、`TestPrepareSocketPathRefusesLiveSocket`、`TestPrepareSocketPathReplacesStaleSocket`。另修一条**假绿**：审计查询漏 `FROM audit_events`，改成查询失败也判失败，并在 22.04 上用坏 SQL / 好 SQL / 含口令列三支实测 fail-closed） | internal/power/{exec.go,exec_test.go}、internal/agent/{registry.go,operations.go,runner.go,poweraction.go,poweraction_test.go,server.go,server_test.go,socketprobe_linux.go,socketprobe_other.go}、tests/integration/p11-3-power-acceptance.sh |
| P11.3-SCRIPT | P11 | 已执行（`tests/integration/p11-3-power-acceptance.sh`，909 行 11 节四阶段，本轮**两台四阶段全程同一版 md5 93e7251c…**；新增 `guard_foreign_units`：外来 `upanel*.service` 停用 + mask（`/etc` 里的真实单元文件 mask 会失败，改为改名挪走）并断言为 0，停不掉就 exit 2；不再 `rm -f` socket，陈旧/占用交给代理判断；ELF magic 守卫挡住“把 PE 推给 Linux”） | tests/integration/p11-3-power-acceptance.sh、docs/evidence/P11/2026-10-23-p11.3/env.txt |
| P11.3-HOST | P11 | 已执行（`scripts/check.sh` PASS 13 / FAIL 0 / NOT RUN 0：`go vet`、`gofmt` 无输出、`go test ./...` ok、web typecheck/lint/build/smoke、web **45 文件 820 用例**、openapi lint 通过；node v22.23.3；这一份是修完之后在最终代码上重跑的） | docs/evidence/P11/2026-10-23-p11.3/logs/host-check.txt |
| P11.3-RESULT | P11 | 已产出（范围、代码清单、逐节 PASS/FAIL/NOT RUN、2 个产品缺陷、1 条假绿、10 条脚本与流程缺陷、9 条限制——含“24.04 客户机热重启未验到”） | docs/evidence/P11/2026-10-23-p11.3/{env.txt,result.md} |
| P12.1-RESULT | P12 | 已产出（范围、代码清单、三个产品缺陷的原文与定位过程、3 条假绿 + 3 条脚本缺陷、逐节 PASS/FAIL/NOT RUN、5 条 NOT RUN、环境事故） | docs/evidence/P12/2026-10-23-p12.1/{env.txt,result.md} |
| P12.1-VM | P12 | **已执行并通过（22.04.5 与 24.04.5 各 PASS=133 / FAIL=0 / NOT RUN=5，`EXIT=0`）**；逐节断言数两台完全相同（§0 2、§1 4、§2 3、§3 25、§4 7、§5 19、§6 11、§7 13、§8 5、§9 5、§10 6、§10b 6、§11 13、§12 7、§13 6、证据卫生 1）；仓库 id 两台独立核对（`5b2ac62d…` / `6044228b…`，`independent-cat-config.stderr` 为空且两边都非空才比）；一台一跑、不并发 | docs/evidence/P12/2026-10-23-p12.1/logs/{2204,2404}-{run.log,results.txt,agent.log}、payloads/{2204,2404}/*.json |
| P12.1-FIX | P12 | 已执行（产品缺陷 A：主机密钥按算法逐个重握手（`d99998c`）；B：网络层失败整轮重试一次（`de3cef8`）；C：`ErrBudgetExhausted` + 信号名 + 只读查询预算 30→45 秒（`659ba02`）。回归：`TestDialAcceptsRegisteredKeyRegardlessOfNegotiatedPreference`、`TestDialHandshakeTimeoutIsNotAuthFailure`、`TestDialRetriesOnceWhenHandshakeStalls`、`TestRunOutcomeClassifiesWhoEndedTheCall`、`TestResticFailureSaysTimeInsteadOfWrongPassword`。另修观测缺口：`90a669b`（503 文案点名模块）、`1ae4e39`（执行前复核被拒不写明原因）） | internal/backup/{ssh.go,ssh_test.go,exec.go,exec_test.go,restic.go}、internal/agent/{backupquery.go,operations.go}、internal/api/packages.go |
| P12.1-HOST | P12 | 已执行（`scripts/check.sh` PASS 13 / FAIL 0 / NOT RUN 0；web **45 文件 820 用例**；node v22.23.3 —— 默认 v24.15.0 会被版本闸门拦下，不是跳过） | docs/evidence/P12/2026-10-23-p12.1/logs/host-check.txt |
| P12.2-VM | P12 | **已执行并通过（22.04/restic 0.12.1 与 24.04/restic 0.16.4 各 PASS=170 / FAIL=0 / NOT RUN=3，exit=0）**；同一脚本与二进制，跨机 results.txt 逐字一致；SFTP 成功、断网、独立 tmpfs 写满、取消均验证终态与独立事实，三条 NOT RUN 均属 P12.3/P12.5 | docs/evidence/P12/2026-10-24-p12.2/logs/{2204,2404}-run.log、logs/{2204,2404}/results.txt、payloads/{2204,2404}/ |
| P12.2-FIX | P12 | 已修复并复验 4 个真机产品缺陷（Q343/Q344/Q345/Q347）；最终客户端 100 秒晚于代理 90 秒，修复前 10 秒取消现场与修复后 11.313/19.728 秒成功查询均保留。脚本/方法修正见 Q346/Q348 | docs/evidence/P12/2026-10-24-p12.2/{result.md,logs/2404-client-timeout-run.log,logs/2404-before-fix/}、internal/agentclient/{backup.go,client_test.go}、internal/backup/{exec.go,run.go} |
| P12.2-HOST | P12 | 已执行（最终 `scripts/check.sh` PASS 13 / FAIL 0 / NOT RUN 0，web 45 文件 820 用例；修复前与 gofmt 失败日志均保留，不以局部测试替代最终全量结果） | docs/evidence/P12/2026-10-24-p12.2/logs/{host-check.txt,host-check-before-client-timeout.txt} |
| P12.2-RESULT | P12 | 已产出（范围、最终脚本与二进制指纹、两台 170/0/3、四个产品缺陷、脚本修正、四个任务 id、三条后续阶段 NOT RUN、历史失败证据、秘密扫描）；跨机结果/任务/指纹与宿主秘密扫描已复核 | docs/evidence/P12/2026-10-24-p12.2/result.md |
| P12.3-VM | P12 | **已执行并通过（24.04/restic 0.16.4 与 22.04/restic 0.12.1 各 PASS=117 / FAIL=0 / NOT RUN=0，exit=0）**；同一脚本同一组二进制、一台一跑不并发；逐节断言数（§0 11、§1 14、§2 24、§3 13、§4 15、§5 10、§6 30）与 `results.txt` 断言名单跨机逐字一致；恢复/覆盖/验证/清理/写满/SFTP 断连都有真机证据，失败场景同时核对旧目标哈希、暂存残留与应用存活 | docs/evidence/P12/2026-10-24-p12.3/logs/{2404-run.log,2204-run.log}、logs/{2404,2204}/results.txt、payloads/{2404,2204}/ |
| P12.3-FIX | P12 | 已修复并复验 4 个产品缺陷（Q356 借用 fd 被 GC 关掉、Q357 会话滑动但 Cookie 不续发、Q359 失效锁、Q360+Q363 失败消息与真实错误行形状）；每一条都有真机现场与回归用例。历史轮次（116/1/0、被中断轮、并发作废轮）日志全部保留 | internal/backup/{restic.go,restic_retry_test.go,restic_linux_test.go}、internal/auth/{session.go,session_test.go}、internal/api/{auth.go,auth_test.go}、docs/evidence/P12/2026-10-24-p12.3/logs/{2404-run-round-enospc-message.log,2404-run-aborted-rebuild.log,2404-single-thread-attempt/} |
| P12.3-HOST | P12 | 已执行（最终 `scripts/check.sh` PASS 13 / FAIL 0 / NOT RUN 0，web 45 文件 820 用例，web-smoke 10/0/0；第一次 12/1/0 红在 `node-version`（默认 v24.15.0），改用 nvm 的 v22.23.3 后重跑全量，两份日志都保留。**`//go:build linux` 的三条用例另在两台 VM 上实跑**：`TestSlowProgressCallbackDoesNotFailTheCommand`、`TestResticRestoreThroughInheritedDirectoryFD`（Q356 回归）、`TestRecoveryCoreOnRealRestic`，两台各全 PASS） | docs/evidence/P12/2026-10-24-p12.3/logs/{host-check.txt,host-check-wrong-node.txt,2404-linux-tests.txt,2204-linux-tests.txt} |
| P12.3-RESULT | P12 | 已产出（范围、三版二进制指纹与作废原因、两台 117/0/0、逐节断言数、4 个产品缺陷、5 条方法修正、任务 id、STAGES 条目对应表、明确区分已实现/已验证/未验证、秘密扫描零命中） | docs/evidence/P12/2026-10-24-p12.3/result.md |
| P12.4-VM | P12 | **已执行并通过（24.04/systemd 255 与 22.04/systemd 249 各 PASS=246 / FAIL=0 / NOT RUN=0，退出码 0；断言名单跨机逐字一致）** | docs/evidence/P12/2026-10-24-p12.4/{logs/2404,logs/2204,payloads/2404,payloads/2204,migration/2404,migration/2204} |
| P12.4-FIX | P12 | 已修复并复验 9 个产品缺陷（Q371 不重叠未实现、Q374 解释器基名、Q375 calendar 输出形状、Q376 脚本目录不可穿、Q377 库约束不在预检、Q380 root 的 `--wait` 空转、Q381 `needs_sync` 永不为 0、Q382 `database is locked`、Q385 列表 10144ms 压过 10 秒上限）；每一条都有真机现场与回归用例。22.04 首轮 5 条红（Q386/Q387）的日志保留但**不作为产品证据** | internal/schedules/{module.go,views_parallel_test.go}、internal/agentclient/schedule.go、cmd/upanel-web/main.go、internal/operations/jobs.go、internal/schedules/units.go |
| P12.4-HOST | P12 | 已执行（最终 `scripts/check.sh` PASS 13 / FAIL 0 / NOT RUN 0，web 45 文件 820 用例，`production` 冒烟 10/0/0，openapi 6 条既有警告；`go test -race` **NOT RUN**：宿主无 gcc） | docs/evidence/P12/2026-10-24-p12.4/logs/host-check.txt |
| P12.4-RESULT | P12 | 已产出（范围、二进制与脚本指纹、两台 246/0/0、逐节断言数、9 个产品缺陷与 9 条方法修正、"不重叠"判据的诚实边界、任务 id、STAGES 条目对应表、明确区分已实现/已验证/未验证、秘密扫描零命中） | docs/evidence/P12/2026-10-24-p12.4/result.md |
| P12.5-VM | P12 | **已执行并通过（24.04/certbot 2.9.0 第 12 轮、22.04/certbot 1.21.0 第 2 轮，各 PASS=214 / FAIL=0 / NOT RUN=0，退出码 0；逐节断言数 25/20/44/24/30/15/20/10/19/7，归一化后断言名单跨机 diff 无输出；同一脚本 md5 `9fd078cb…` 与同一组二进制）**。覆盖：面板建续期计划 → 代理写单元 → systemd 唤醒 → 代理提交 `certificate.renew` → 真实 Pebble 的 HTTP-01 校验 → 盘上与账本两处事实一致；`--keep-until-expiring` 在"还没到期"时不重签、在"三天后到期"时真的换证；失败场景下任务 `error.message` 与账本 `last_error` 是同一个字符串且含 certbot 自己的话，盘上证书不动 | docs/evidence/P12/2026-10-25-p12.5/{logs/2404/,logs/2204/,payloads/2404/,payloads/2204/} |
| P12.5-FIX | P12 | 已修复并复验 4 个产品缺陷（Q390 定时续期预检绑不上目标身份 → 500 `INTERNAL_ERROR`；Q391 失败原因到不了任务与账本；Q392 P12.4 的"证书续期尚未接线"闸没拆；Q393 `normalized_params` 里空的 `certificate_id`/`email`）与 5 条验收方法修正（Q394—Q398：Pebble 缓存已通过的授权一小时、§0 不断言 80/5002 空闲、§9 用整轮开始时的会话、prune 数总条数与水位取在触发之后、采样器 `ps -e -C` 自己压垮 2 vCPU 客户机）。每条都有"现象 → 根因 → 判据怎么改"，第 10 轮与 22.04 首轮两份红现场留在证据里 | docs/evidence/P12/2026-10-25-p12.5/{result.md,logs/2404/earlier-round10-section7-fixture-defect.log,logs/2204/first-round-nginx-blocked.log} |
| P12.5-HOST | P12 | 已执行（`scripts/check.sh` PASS 13 / FAIL 0 / NOT RUN 0，按 `.nvmrc` 用 `node-v22.23.3-win-x64`；`go test ./...` 43 包 ok，web **49 文件 939 用例**，`web-smoke` 10/0/0，`openapi-lint` valid + 6 warnings）。同一 commit 用系统 node v24.15.0 跑过一次是 11/2（`node-version` 与 `openapi-lint` 的 libuv 收尾断言），两次输出都留档 | docs/evidence/P12/2026-10-25-p12.5/{logs/host-check.txt,logs/host-check-system-node.txt} |
| P12.5-RESULT | P12 | 已产出（范围、二进制与脚本指纹、两台 214/0/0、逐节断言数、4 个产品缺陷与 5 条方法修正、两台机器的差异、"夹具闸不是护栏"的诚实边界、NOT RUN 清单、文件清单） | docs/evidence/P12/2026-10-25-p12.5/result.md |
| P13.1-VM | P13 | **已执行并通过（24.04 与 22.04 各 PASS=88 / FAIL=0 / NOT RUN=0，退出码 0）**；同一份脚本（md5 `d08964c5385a5156308386001c2190bf`）、同一组二进制（web `c99f56c3…`、agent `b469732d…`、ctl `5d4a1bbf…`），归一化后的 88 条断言名单跨机 `diff` 无输出；逐节断言数 6/5/22/9/10/23/8/5 | docs/evidence/P13/2026-10-26-p13.1/{logs/2404/,logs/2204/} |
| P13.1-FIX | P13 | 已修复并复验 1 个产品缺陷（Q404：一整桶样本值全等于 1.9 时 `SUM(1.9*12)/SUM(12)` 算出 `1.8999999999999997`，撞 `CHECK (min_value <= value AND value <= max_value)`，整个小时桶写不进去且每轮重试都失败——那一小时所有指标的 hour 行永久缺失。修法是聚合与查询两处都把加权平均夹回 `[MIN(min_value), MAX(max_value)]`，`CHECK` 原样保留）与 4 条验收方法修正（Q405 缺口判据窗口右端也要让一个桶宽、停机时长 15→25 秒；Q406 `check.sh` 加 `line-endings` 第 14 关、scp 的二进制要 `chmod 0755`；Q407 恒真的 socket 断言、按粒度而非按 scope 读 series、`BEFORE_MAX` 取样点在 `stop_web` 之前）。去掉夹取时回归测试报出与真机一字不差的 `CHECK constraint failed ... (275)` | docs/evidence/P13/2026-10-26-p13.1/logs/regression-before-fix.txt |
| P13.1-HOST | P13 | 已执行（`scripts/check.sh` PASS 14 / FAIL 0 / NOT RUN 0，按 `.nvmrc` 用 `H:\Code\.toolchains\node-v22.23.3-win-x64`；用系统 node v24.15.0 跑过一次是 13/1，红在工具链层 `node-version`，换回锁定版本后同一份代码全绿）。新增第 14 关 `line-endings` 干跑 `checked=122 bad=0` | docs/evidence/P13/2026-10-26-p13.1/logs/host-check.txt |
| P13.1-RESULT | P13 | 已产出（范围、二进制与脚本指纹、两台 88/0/0、逐节断言数、1 个产品缺陷与 4 条方法修正、NOT RUN 三项） | docs/evidence/P13/2026-10-26-p13.1/result.md |
| P13.2-VM | P13 | **已执行并通过（24.04 第 6 轮与 22.04 第 2 轮，各 PASS=148 / FAIL=0 / NOT RUN=0，退出码 0）**；同一份脚本（md5 `c74dd469e47a7c17aad05e94b628d930`）、同一组二进制（web `082752b3…`、agent `1610950d…`、ctl `e5e4ea6d…`），归一化后的 148 条断言名单跨机 `diff` 无输出；逐节断言数 7/9/48/63/9/8/4。覆盖：`duration_seconds` 低于两条采集周期 400；`pending` 不立刻触发且 `last_reason` 说清在计持续；事件从 `state_since` 起实打实等满 30 秒才开、只开一条、再评估两轮不涨；同一份真实容量数据上 `any` 触发而 `all` 不触发；维护静默里事件 `silenced=1` 但照记照判；受控序列停写 40 秒与真代理停 40 秒都不被判成恢复；回落后恢复持续够了才 `resolved` 并带出实际时长与复查的 `peak_value`；冷却期内不重起算；删除正在 firing 的规则把事件收口成 `resolved` 且 `rule_id` 置空 | docs/evidence/P13/2026-10-26-p13.2/{logs/2404/,logs/2204/} |
| P13.2-FIX | P13 | 已修复并复验 3 个产品缺陷（Q414 覆盖率容差与 `duration_seconds` 地板按写死的 5 秒判，而容量序列 30 秒才落一行，空间规则的表象是"配好了永远不响"、`last_reason` 却写"窗口内没有任何样本"——修法是 `metrics.WriteCadence` 成为唯一答案，新鲜度 `max(3×节奏,20s)`、窗口前段余量 `max(2×节奏,10s)`、`duration_seconds ≥ 2×节奏`；Q417 `last_reason` 只在进入 pending 那一轮写、后几轮被覆盖成空串，改成每一轮都写进度；Q418 `resolved_at`/`acked_at`/静默起止回空串而不是 null，4 处 `*string` + openapi 3.1 写法 + 3 个按原始 JSON 判 null 的回归测试）与 6 条验收方法修正（Q415 整机 fd 超 4000 直接退 2——24.04 上 `systemd-logind` 泄漏 7477 个 fd 让 `storage.topology` 从 0.027 秒变成 25—41 秒；Q416 `sqlq` 不许吞 SQL 错误、列名是 `redacted_details`；Q419 三处判据修正并加一条不受取样时刻影响的 `EV_DELAY ≥ duration_seconds` 强判据；Q420 `state_since` 是 Unix 秒整数、`$(( ))` 里塞命令替换会让整轮崩溃）。24.04 六轮履历 117/25 → 139/3 → 142/0 → 145/2 → 崩溃 → 148/0，第 1 轮完整日志被覆盖只留失败名单原文，已在 result.md 注明 | docs/evidence/P13/2026-10-26-p13.2/{result.md,logs/regression-before-fix.txt,logs/2404/acceptance.round5-crash.log} |
| P13.2-HOST | P13 | 已执行（`scripts/check.sh` PASS 14 / FAIL 0 / NOT RUN 0，按 `.nvmrc` 用 `H:\Code\.toolchains\node-v22.23.3-win-x64`；`go test ./...` 43 包 ok，其中 `internal/api` **193.1 秒**（338 个用例，含 Argon2id），web **49 文件 939 用例**，`web-smoke` 10/0/0，`openapi-lint` **valid + 6 warnings**）。同一份代码在修 Q418 **之前**跑过一次是 **13/1**，红在 `openapi-lint` 7 条 `error struct: 7`（`nullable` 是 3.0 关键字、本文件是 3.1），两次输出都留档 | docs/evidence/P13/2026-10-26-p13.2/{logs/host-check.txt,logs/host-check-before-fix.txt} |
| P13.2-RESULT | P13 | 已产出（范围、二进制与脚本指纹、两台 148/0/0、逐节断言数、24.04 六轮履历、3 个产品缺陷与 6 条方法修正、通知/前端/race/破坏性场景四类 NOT RUN、第 1 轮日志丢失的诚实边界） | docs/evidence/P13/2026-10-26-p13.2/result.md |
| P13.3-VM | P13 | **已执行并通过（24.04 第 6 轮与 22.04 第 1 轮，各 PASS=211 / FAIL=0 / NOT RUN=0，退出码 0）**；同一份脚本（md5 `f8f6984d0f5e302970b161899a90ff28`）、同一组二进制（agent `f7acfd27…`、web `8c8b258d…`、ctl `45fb01be…`，从当前工作树交叉编译），归一化后的 211 条断言名单跨机 `diff` 只有两行不同（`投递在 8 秒上限上结束` 实测 9/8 秒、`退避时间是 30 秒` 实测 35/31 秒），两行都是 PASS。逐节断言数 13/11/48/12/44/34/35/14。覆盖：viewer/未登录读不到带收件邮箱与回调地址的通道列表、写要 admin+CSRF；in_app 只有一条且不可删；smtp 没有明文模式、用户名与口令同进同出；webhook 只 https、拒 userinfo 与 `#片段`；`POST /secrets` 要再认证且响应无明文；**保存配置一个请求都不发、投递账 0 行**；SMTP 显式/隐式 TLS 各收一封信且正文 sha256 与契约逐字一致、535 时回 200 + 对端原话 + 只连一次；webhook 200/500/302/超时四种回执各按契约判、302 不跟随、令牌只以 sha256 出现在日志里；用途不对的 `secret_ref` 由**代理**拒绝且连接根本没建起来；告警链路上静默不入队、去重、30 秒退避（`next_attempt_at - created_at` ∈ [30,35]）、先坏后好第二次成功、用尽上限判死、**停用判死且 attempts 一次都不加**、停用后不再入队 | docs/evidence/P13/2026-10-26-p13.3/{logs/2404/,logs/2204/} |
| P13.3-FIX | P13 | 真机验收抓到 **0 个产品缺陷**、3 个验收侧问题（Q424 接收器的 `start_tls` 在 Python 3.12 上把握手后的应用数据丢掉，表现为"Go 的 net/smtp 一句都过不去而 `openssl s_client` 畅通"，加 `TLSBridge` 缓冲重放并显式 `server_side=True`、接管新 transport；Q425 退避不能用"我们到达时还剩多少秒"量，改成 `next_attempt_at - created_at` 且区间宽度 = 5 秒调度节拍，附带 `strftime` 不认 ISO 尾巴 `Z`、`next_attempt_at` 在终局是 0 两个取值坑；Q426 `chan_version` 多传一个参数导致版本号空串、请求体拼成非法 JSON、产品合法的 400 被读成"停用没生效"，改成这一步自己四断言、非数字版本号直接不发）。另修 6 条脚本缺陷（`phase` 写 0600 种入读不到、断言读错响应文件、超时措辞过窄、`$(( ))` 撞非数字整轮崩、`sqlq` 把"没有这一行"打成错误、`MAIL FROM` 没切 ESMTP 参数） | docs/evidence/P13/2026-10-26-p13.3/logs/2404/round3-disable-400.txt |
| P13.3-HOST | P13 | 已执行（`scripts/check.sh` PASS 14 / FAIL 0 / NOT RUN 0，按 `.nvmrc` 用 `H:\Code\.toolchains\node-v22.23.3-win-x64`；`go test ./...` 46 包 ok + 3 个 no test files，多数命中缓存、`internal/api` 这轮显示 `(cached)`；web 49 文件 939 用例；`web-smoke` 10/0/0；`openapi-lint` valid + 6 warnings） | docs/evidence/P13/2026-10-26-p13.3/logs/host-check.txt |
| P13.3-RESULT | P13 | 已产出（范围、二进制与脚本指纹、两台 211/0/0、逐节断言数、24.04 六轮履历与"第 6 轮为何重新构建"、3 个验收侧问题逐条现场、7 类 NOT RUN、第 1—2 轮日志丢失的诚实边界） | docs/evidence/P13/2026-10-26-p13.3/result.md |
| P13.4-VM | P13 | **已执行并通过（24.04 第 2 轮与 22.04 第 2 轮，各 PASS=128 / FAIL=0 / NOT RUN=0，退出码 0）**；同一份脚本（sha256 `f86fb06fe8e06d1cdecf4899b5dec2c64355a87439343dd9b456028dac34862c`）、同一组二进制（agent `0d128952…`、web `cef620a7…`、ctl `c66d2c53…`，从当前工作树交叉编译），归一化后的 128 条断言名单跨机 `diff` **无输出**，两台的差别只有 NOTE 里量到的新鲜度秒数（6 秒 / 1 秒，都是 PASS）。逐节断言数 7/9/14/14/20/31/12/8/10/3。覆盖：未登录 401 / viewer 403 / admin 缺 CSRF 403 / `id` 非正整数 400 / 未知 id 404 且原文"告警事件不存在"、每次被拒都落审计且带错误码；确认人取自会话而不是请求体、确认后 `state` 仍是 `firing` 且 `resolved_at` 不被写、第二个管理员再确认保留第一个人的时间与人、两边各落一条审计、已恢复的事件也能确认；投递账未登录 401 / viewer 403 / admin 200、行带投递当时的通道快照、`state`/`kind` 闭集过滤、越界枚举 400、`limit` 非正整数 400 且 >200 截断不报错、`kind=test` 无人测试时为空；真停采集 45 秒造缺口，缺口里**一行原始样本都没有**（不是写了 0）、`missing_points` 数得出缺的桶、相邻两点真的断开、缺口里没有 `value=0` 的假点、恢复采集后缺口不被回填；关掉采集后 `enabled` 仍 true 而 `collecting=false`、`reason` 指向 `web.metrics.enabled`、旧数据照样查得到、10 秒内没有新样本落库、`last_sample_at` 为 null；日志无 panic、本脚本没有产生任何 `operation` | docs/evidence/P13/2026-10-26-p13.4/{logs/2404/,logs/2204/} |
| P13.4-FIX | P13 | 真机验收抓到 **0 个产品缺陷**、3 组验收侧问题（Q429 20 秒新鲜度判据拿宿主 `datetime.now()` 去量，24.04 走 TCG 单线程时从取回状态到算出结果过了十几秒，把新鲜样本量成陈旧——库里 host 序列每 5 秒一条无缺口、那份响应自报只差 7 秒，改成同响应内 `generated_at - last_sample_at`；Q430 把 `enabled` 与 `collecting` 当成一个意思，第 8 节一次红 4 条，改断言与 CONTRACTS §13.4，产品一行没动；Q431 另五处：`$WORK` 0700 把 `upanel-web` 挡在门外、请求体塞了契约之外的 `scope`、`COUNT(*) ... IS NULL` 期望写反、缺口窗口右端取未来时刻被自己的种入填上、SQL 漏 `metric`/`resolution='raw'` 把聚合行读成回填）。另加一次判据拆分：缺口"缺几桶"用定型窗口、"图上连不连"用延伸到缺口后 30 秒的窗口 | docs/evidence/P13/2026-10-26-p13.4/logs/2404/acceptance.log |
| P13.4-HOST | P13 | 已执行（`scripts/check.sh` 在**最终工作树**上 PASS 14 / FAIL 0 / NOT RUN 0，按 `.nvmrc` 用 `H:\Code\.toolchains\node-v22.23.3-win-x64`；`go test ./...` 46 包 ok、web 52 文件 985 用例、`web-smoke` 10/0/0、`openapi-lint` valid + 6 warnings。第一次跑漏了 Node 工具链所以 `node-version` 判红，重跑前先把 PATH 补上——那一遍的输出没有留作证据） | docs/evidence/P13/2026-10-26-p13.4/logs/host-check.txt |
| P13.4-RESULT | P13 | 已产出（范围、二进制与脚本指纹（含被 Q429 取代的上一版脚本哈希）、两台 128/0/0、逐节断言数、24.04 两轮 / 22.04 两轮的履历与"换脚本必须两台重跑"、3 组验收侧问题逐条现场、5 类 NOT RUN） | docs/evidence/P13/2026-10-26-p13.4/result.md |
| P14.1-VM | P14 | **已执行并通过（22.04 PASS=112 / FAIL=0 / NOT RUN=0；24.04 PASS=116 / FAIL=0 / NOT RUN=0，退出码均 0）**；同一份脚本（sha256 `147b7a93d248bd1d622b6c91f2717daf5246c9cd4d580945baeac1fe30f2c16a`）、同一组二进制（agent `92795acb…`、web `252a7ed6…`、ctl `6841cb3c…`）。逐节断言数 2204 = 7/7/5/14/20/7/10/13/9/16/4，2404 = 7/7/5/14/25/6/10/13/9/16/4，差 4 条已核实为 24.04 多一张 `docker0`（§3 每卡 5 条、§4 少 1 条 target 对照）。覆盖：未登录 401 / viewer 403 / admin 200、**POST 四条同路径与 `/network/apply` 全部 404/405**、逐卡 sysfs 三态对照、`capability` 现场独立重算、路由家族闭集与 `destination` 含 `/`、监听与 `/proc/net` 双向比对（含本面板端口 pid==Web 子进程且 comm==`upanel-web`）、防火墙与 `ufw status verbose|numbered` 对照、`issues` 闭集、三目录前后哈希不变（只读证明）、审计 failure ≥ 4 且 success == 0 | docs/evidence/P14/2026-10-26-p14.1/logs/{2204,2404}/{run.log,env.txt,facts.txt} |
| P14.1-FIX | P14 | 真机验收抓到 **2 个产品缺陷**（Q438 主机路由 `destination` 无前缀 → 同一字段两种写法，补齐 `/32`/`/128`；Q439 `/etc/nftables.conf` 每台 Ubuntu 都有 → 被误判 nftables 同存 → `editable=false` → P14.3 在标准机器上永远打不开，判据改为"有 conf 且 `*.wants/nftables.service` 被 enable"），**6 条验收方法修正**（Q440—Q445：runuser 壳与子进程、127.0.0.0/8、`json.dumps` 比结构、截断找键、`ufw` 超时后的 `0 == 0` 假绿、24.04 `ssh.socket`） | docs/DECISIONS.md Q438—Q445；`docs/evidence/P14/2026-10-26-p14.1/result.md` §4 |
| P14.1-UNIT | P14 | 已执行（`internal/network/network_test.go` 22 个顶层用例全绿，其中 Q438/Q439 各一条回归；`go vet ./...` 干净；`go test ./...` 全量输出留档） | docs/evidence/P14/2026-10-26-p14.1/logs/{host-gotest.txt,host-network-tests.txt} |
| P14.1-HOST | P14 | 已执行（`scripts/check.sh` 在**最终工作树**上 PASS 14 / FAIL 0 / NOT RUN 0，按 `.nvmrc` 用 `H:\Code\.toolchains\node-v22.23.3-win-x64`：go 47 包 ok + 3 个 no test files、gofmt 干净、Linux 交叉构建过、web 53 文件 999 用例、`web-smoke` 10/0/0、`openapi-lint` valid + 6 warnings） | docs/evidence/P14/2026-10-26-p14.1/logs/host-check.txt |
| P14.1-RESULT | P14 | 已产出（范围与"为什么不靠 mock"、两台 112/116、逐节断言数、两台分别落在 `editable=true`/`false` 两个分支的现场事实、2 个产品缺陷 + 6 条验收方法修正逐条现场、只读的证明方式、7 类 NOT RUN、复现命令与交付指纹） | docs/evidence/P14/2026-10-26-p14.1/result.md |
| P14.2-UNIT | P14 | 已执行（`internal/network` **39** 个顶层用例，其中 `applyengine_test.go` **17** 个：先武装 timer 再 apply、generate 失败不动现场、核验不符立刻回滚、timer 装不上就不改、待确认记录挡住第二次写、`confirm` 要候选地址与窗口、对账过期即回滚/未过期重新武装、DHCP 保留归属并丢掉旧地址；**Q456 修完补的两条**：`TestApplyDHCPVerifiedWhenServerReissuesSameAddress`（DHCP 发回同名地址 → `verified`，且 `netplan apply` 只调一次）与 `TestApplyDHCPRollsBackWhenAnotherFileAlsoConfigures`（两个全局地址 → `rolled_back`，`observed["verify_error"]` 含“2 个全局 IPv4”，apply 调两次），另有 `useAddrs(…)` fixture 造多地址的 `ip -j addr show` 回复。`internal/agent` 网络动作 **13** 个（判决原样过桥、兜底 blocker、三个 outcome、未知 outcome 报错）；`internal/api` 的 `networkexpand_test.go` **6** 个（含 `::ffff:` 与 zone 归一化、“取不到连接地址就不转发”）+ 网络 handler **7** 个。`go test` 全量输出留档） | docs/evidence/P14/2026-10-26-p14.2/logs/host-check.txt |
| P14.2-WEB | P14 | 已执行（`npm --prefix web run test` **54 文件 1031 用例全过**——新增 32 条：写路径纯逻辑 12、`network.` 超时分档 2、`summarizeNetworkJob` 8、`NetworkApplyCard` 的 SSR 渲染 9、`/network/config` 请求路径 1；`npm run typecheck`（vue-tsc）与 `web-lint` 通过。**浏览器里的真实渲染与断线重连交互仍未验**（VM 无 Playwright，Q21），这一行不等于那一条） | docs/evidence/P14/2026-10-26-p14.2/logs/host-check.txt |
| P14.2-HOST | P14 | 已执行（`scripts/check.sh` 在**最终工作树**上 **PASS 14 / FAIL 0 / NOT RUN 0**、退出码 0，按 `.nvmrc` 用 `H:/Code/.toolchains/node-v22.23.3-win-x64`：go **47 包 ok + 3 个 no test files**、gofmt 干净、Linux 交叉构建与 vet 过、web **54 文件 1031 用例**、`web-smoke` 过、`openapi-lint` valid + 6 warnings 与基线一致。**跑了两次**：一次在 Q458 改完开关说明文字之前（同样 14/0/0），一次在最终树上；文件里留的是最终树那一份。更早一轮曾 **FAIL 1**（`internal/storage` 的 `TestDeviceSuperblockProberCancelledContext`，与本轮无关的真实偶发缺陷，Q446 修掉） | docs/evidence/P14/2026-10-26-p14.2/logs/host-check.txt |
| P14.2-VM | P14 | **已执行，passed**（Ubuntu 22.04.5 / kernel 5.15.0-198 / systemd 249.11，QEMU slirp；第五轮 `PASS 186 / FAIL 0 / NOT RUN 3`，退出码 0，`STAMP 2026-10-26T14:58:14Z start` → `15:05:11Z done`。脚本 sha256 `6a3a4ed8…95b95`；二进制 `upanel-agent e19852f8…caf8`、`upanel-web 6be39c17…0325`、`upanelctl a0ca7219…f580`。核心现场：改成不可达 `10.99.99.99/24` → 停 Web 与代理 → `deadline+28s` 由 `upanel-netrollback@<token>.service` 退回；旧地址确认被 `CONFIRM_WRONG_ADDRESS` 拒；复杂 netplan 返回 `NETWORK_UNSUPPORTED`；两条救命看门狗均 `not-needed`。前四轮为何没绿见 result.md §1） | docs/evidence/P14/2026-10-26-p14.2/logs/p142-2204-run5.log 与 facts/ |
| P14.2-RESULT | P14 | 已产出（§1 结论与四条 §16 硬条件的现场对照 + 五轮为什么没绿的账；§2 为什么全程 HTTPS；§3 断连重连的逐秒时间线与 journal 原文；§4 DHCP 核验判据缺陷的现场、根因两层、改法与已知抓不到；§5 两条已知限制 Q457/Q456；§6 3 条脚本 NOT RUN + 我补的 4 条；§7 复现命令与“二进制与最终工作树的差异”声明） | docs/evidence/P14/2026-10-26-p14.2/result.md |
| P14.3-UNIT | P14 | 已执行（`internal/network` **58** 个顶层用例：`applyengine_test.go` 17 + `firewallapply_test.go` **19** + `network_test.go` 22；`firewallapply_test.go` 里本轮新增两条回归——`TestFirewallAssessRefusalsKeepNormalizedParams`（无 pending × confirm/rollback + 定义不合法 × apply 三条拒绝分支都断言 `Normalized != nil`，Q464）与 `TestFirewallTokenMismatchKeepsNormalizedParams`（有记录但 token 不符）。另有 `internal/agent/firewallaction_test.go` **9** 个（判决原样过桥、兜底 blocker、`RECONFIGURE` 短语、解码层校验、outcome 闭集、未知 outcome 报错）。`go build ./...`、`gofmt -l`（空）、`go vet ./...` 干净；`go test ./internal/network/` 0.901s、`./internal/agent/` 32.587s 全绿） | docs/evidence/P14/2026-10-27-p14.3/logs/host-check.txt |
| P14.3-WEB | P14 | 已执行（`npm --prefix web run test` **56 文件 1065 用例全过**——新增 34 条：`web/src/api/firewall.test.ts` **25**（`buildDefinition`/`buildApplyParams`/`describeFirewallChange`/`rollbackSecondsLeft`/`firewall.` 超时分档）+ `web/src/components/firewall-write.test.ts` **9**（`FirewallApplyCard` 的 SSR 渲染：倒计时只在 actionable 时起、`rules` 缺字段不崩、变化摘要文案）。`npm run typecheck`（vue-tsc）与 `web-lint` 通过。**浏览器里的真实渲染与交互仍未验**（VM 无 Playwright，Q21），这一行不等于那一条） | docs/evidence/P14/2026-10-27-p14.3/logs/host-check.txt |
| P14.3-HOST | P14 | 已执行（`scripts/check.sh` 在**最终工作树**上 **PASS 14 / FAIL 0 / NOT RUN 0**、退出码 0，node v22.23.3：go **47 包 ok + 3 个 no test files**、gofmt 干净、Linux 交叉构建与 vet 过、web **56 文件 1065 用例**、`web-smoke` **10/0/0**、`openapi-lint` valid + **6 warnings**（与基线一致）；跑在本轮所有产品与脚本改动之后，工作树此后未再改动） | docs/evidence/P14/2026-10-27-p14.3/logs/host-check.txt |
| P14.3-VM | P14 | **已执行，passed**（Ubuntu 22.04.5 / kernel 5.15.0-198 / systemd 249.11 / **ufw 0.36.1**，QEMU slirp；**第三轮** `PASS 153 / FAIL 0 / NOT RUN 4`，退出码 0，`STAMP 19:06:48Z start` → `19:13:56Z done`；逐节断言数 12/3/16/12/24/31/13/24/11/7。脚本 sha256 `ce8c0a05…b59aab6`；二进制 `upanel-agent 5a321eaa…f889db`、`upanel-web 96e571a1…9ce6e1c`、`upanelctl 190bc152…297fc46`。核心现场：缺 22/tcp → `FIREWALL_MANAGEMENT_PATH` 且 `show added` 仍 `(None)`；apply `frb_695fe8…`（deadline `19:12:26Z`）→ 停 Web 与代理 → 60 秒时新规则仍在 → `19:12:34Z` systemd 起 `upanel-fwrollback@…`、`19:12:45Z` `upanel-agent[162295]` 打完“已处理” → 规则换回旧集且 `ufw` 仍 active；手写 `ufw allow 9000/tcp` 跨两次 apply 原样保留；两条救命看门狗 `19:10:15Z`/`19:10:37Z` 各留 `not-needed`。前两轮为何没绿见 result.md §4、§5） | docs/evidence/P14/2026-10-27-p14.3/logs/p143-2204-run3.log 与 facts/（50 个） |
| P14.3-RESULT | P14 | 已产出（§1 结论与两条硬性用例的现场；§2 十个阶段各验了什么与断言数；§3 断连那一段的逐秒时间线、journal 原文与规则集前后对照；§4 Q464 的现场（`132/13`、`ERR:KeyError:'data'`、web.log 的 `预检结果不合法`）、根因、为什么每段单独看都对、修法与两条回归；§5 五条验收方法缺陷逐条（`grep -qi active` 匹配 `inactive`、inactive 时 `status verbose` 无 `Default:` 行导致永真、`write_watchdog` 从未调用、timer 是每 token 一个不是模板、坏定义在解码层就 400）+ `rules` 为 `null` 的契约违反；§6 四条已知限制；§7 4 条 NOT RUN 及覆盖方式；§8 复现命令与三次轮次的账） | docs/evidence/P14/2026-10-27-p14.3/result.md |
| P14.4-UNIT | P14 | 已执行（`internal/shares` **122** 个顶层用例、`internal/agentstore` **87** 个，全部 ok——`go test ./internal/shares/ ./internal/agentstore/` 通过、`go vet ./...` 干净、`gofmt -l` 空。本轮新增三条回归：`TestSameDirShares`（`internal/shares/nfs_test.go`，含"子目录不算同目录"与"同一条共享多行不算冲突"）、`TestApplyExportsRefusesDuplicateDirs`（直接喂两份同目录 `ManagedExport` 进 `applyExports`，断言错误带 `EXPORT_PATH_CONFLICT` 且托管文件已还原）、`TestAssessShareApplyNFSBlocksSameDirAsAnotherShare`（先应用一条 `RelativePath:""` 的共享，再预检同目录的新共享 → `allowed=false` 且消息点名两条共享）；`internal/agentstore/migrations_test.go` 的 `TestMigration0013RebuildsSharesAndKeepsRows` 把"旧 schema 真的拒绝 nfs"写成断言（Q471） | docs/evidence/P14/2026-10-27-p14.4/logs/host-gotest.txt |
| P14.4-WEB | P14 | 已执行（`npm --prefix web run test` **56 文件 1091 用例全过**，P14.3 是 56/1065，本轮净增 **26** 条：NFS 共享记录与导出表客户端的渲染、`firstList` 的空列表语义、`ClientKind` 三种 NFS 来源、多协议共享列表的请求路径；`npm run typecheck`（vue-tsc）与 `web-lint` 通过。修的是 Q470 两处"把 NFS 当不存在"：`firstList(...)` 用 `??` 串联时**空数组不是 falsy**，第一个空列表就把 `startup_paths`/`exported_paths`/`volume_offline_shares` 短路成空——页面不报错、只是那一块永远是空的；`ClientKind` 只写了 SMB 那套，导出表读回来的客户端被显示成"未知"。**浏览器里的真实渲染与交互仍未验**（VM 无 Playwright，Q21），这一行不等于那一条） | docs/evidence/P14/2026-10-27-p14.4/logs/host-check.txt |
| P14.4-HOST | P14 | 已执行（`scripts/check.sh` 在**最终工作树**上 **PASS 14 / FAIL 0 / NOT RUN 0**、退出码 0，node v22.23.3 / go1.27.3：line-endings 过、go **47 包 ok + 3 个 no test files**、gofmt 干净、Linux 交叉构建与 vet 过、web **56 文件 1091 用例**、`web-smoke` **10/0/0**、`openapi-lint` valid + **6 warnings**（与基线一致）。跑在 P14.4 全部产品与脚本改动之后（此后只改了 `docs/` 与证据文件；P14.5 的新代码是在这份日志之后才开始写的，它有自己的 HOST 行） | docs/evidence/P14/2026-10-27-p14.4/logs/host-check.txt |
| P14.4-VM | P14 | **已执行，passed**（Ubuntu 22.04.5 / kernel 5.15.0-198-generic / nfs-kernel-server 与 nfs-common `1:2.6.1-1ubuntu1.2`，QEMU slirp、`-monitor none`；**第七轮** `PASS 141 / FAIL 0 / NOT RUN 6`，退出码 0，VM 内 `20:40:27Z` → `20:44:24Z`；逐节断言数 24/6/8/8/21/12/9/14/3/21/13/6。脚本 sha256 `8c732998bd0b1e68730f136cc821dd1c69bbfbec3bc152dd560a7b86b26aa1af`；二进制 `upanel-agent 3a0caa42…cfc56de9`、`upanel-web e269a391…4102e159`、`upanelctl fb39b0c1…089ff6b0a5`，VM 内 `sha256sum` 与宿主交叉构建的同一组核对一致。核心现场：托管导出文件只有 `/etc/exports.d/upanel.exports` 一份、`/etc/exports` 整轮 sha256 不变；`exportfs -v` 实际导出表里路径在且带 `root_squash`、没有 `no_root_squash`/`async`/`insecure`/`*`；允许网段换成 `192.168.77.0/24` 后同一台机器挂不上（`mount` 非 0、`findmnt` 空）、改回 `127.0.0.0/8` 立刻又能挂上；只读共享客户端写入被内核以 `Read-only file system` 拒绝、服务端目录没多出文件；卷摘出挂载树后 **13 秒**内撤销循环摘导出行 + `exportfs -ra`，`runtime_revoked.volume_state=unmounted`、`config_applied=false`，卷挂回来五项一样不变、重新应用才恢复；停用 + 删除后导出表与托管文件都清空、目录与文件还在、内容哈希不变、只剩一条已停用的记录。前六轮为何没绿见 result.md §4、§5） | docs/evidence/P14/2026-10-27-p14.4/logs/run.log（177 行）与 facts/（8 份） |
| P14.4-FIX | P14 | 真机抓出并修掉 **2 个产品缺陷**（Q471：`shares.protocol` 的 `CHECK (protocol IN ('smb'))` 让 NFS 导出写下去了却在 `SaveShare` 被挡——`fakeStore` 不跑 SQL，单测一直是绿的；修法是 `0013_shares_protocol_nfs.sql` 表重建 + 一条把"旧 schema 真的拒绝 nfs"写成断言的回归。Q473：同一个目录的第二条共享在预检被放行——`ExportConflicts` 只比"别人的导出文件"这一半；修法是纯函数 `SameDirShares` + `sharesSharingDir`，预检与执行同一条判据，执行期再兜一次，`CONTRACTS.md` §7(5) 补上两个来源），web 侧 **2 处同类**（Q470），**7 类验收方法缺陷**（Q474：`"$(PRE_ALLOWED)"` 少一个 `$` 把变量当命令执行、`grep -c '# upanel:'` 数进文件头示例注释、`umount -l` 让 `knfsd` 持有文件系统、"客户端 OPTIONS 是 ro"这条断言本身不成立、夹具 `0600` 被 root_squash 成 `nobody` 读不到、断言"delete 顺手删记录"与契约相反、前置失败后六十多条断言用垃圾值变红），**1 条同类未修**（SMB 路径 `internal/shares/write.go:230` 在配置生效后落库失败不回滚，属于 P08 代码路径） | docs/DECISIONS.md Q470—Q474；docs/evidence/P14/2026-10-27-p14.4/result.md §4、§5 |
| P14.4-RESULT | P14 | 已产出（§1 结论与四个验收点的现场；§2 十二个阶段各验了什么与断言数；§3 真机验到的四件硬事——认领标记是唯一判据 / `exportfs -ra` 的退出码不是证据、判决一律读回 `exportfs -v` / `root_squash` 是行为不是配置项 / 撤销只减不增；§4 两个产品缺陷的真机现场、根因、修法与一条同类未修；§5 七类验收方法缺陷逐条；§6 六条 NOT RUN 及为什么没验；§7 文件清单） | docs/evidence/P14/2026-10-27-p14.4/result.md |
| P14.5-UNIT | P14 | 已执行（`internal/accounts` **37** 个顶层用例：`capability_test.go` 8、`definition_test.go` 7、`engine_test.go` 7、`sshkey_test.go` 7、`sudo_test.go` 8；`internal/agent` 侧新增 `accountaction_test.go` 5（一律从**线格式**出发，Q477 的回归）与 `accountquery_test.go` 6；`go test -count=1 ./internal/accounts/ ./internal/agent/ ./internal/agentstore/ ./internal/api/ ./internal/shares/` 五个包全 `ok`） | docs/evidence/P14/2026-10-27-p14.5/logs/host-check.txt |
| P14.5-WEB | P14 | 已执行（`npm --prefix web run test` **57 文件 1116 用例全过**；`typecheck` 与 `lint` 干净。P14.4 是 56/1091，本轮净增 **3** 条：`accounts.test.ts` 的"缺 visudo 只关掉 sudo 那一半"（23 个）、`jobresult.test.ts` 新增 2 条账号结果翻译。改写的两条断言都对着 Q479：`expect(detail).not.toContain('配了公钥也登不上')`） | docs/evidence/P14/2026-10-27-p14.5/logs/host-check.txt |
| P14.5-HOST | P14 | 已执行（`scripts/check.sh` 在**最终工作树**上 **PASS 14 / FAIL 0 / NOT RUN 0**、退出码 0，go1.27.3 / node v22.23.3：line-endings **134** 个脚本/迁移文件全 LF、go **48 包 ok + 3 个 no test files**、gofmt 干净、Linux 交叉构建与 vet 过、web **57 文件 1116 用例**、`web-smoke` **10/0/0**、`openapi-lint` valid + **6 warnings**（与基线一致）） | docs/evidence/P14/2026-10-27-p14.5/logs/check-run.txt |
| P14.5-VM | P14 | **已执行，passed**（Ubuntu 22.04.5 LTS / kernel 5.15.0-198-generic / sshd active，QEMU，宿主 SSH 转发 `127.0.0.1:2223`；**第三轮** `PASS 168 / FAIL 0 / NOT RUN 6`、退出码 0；脚本 sha256 `c2a5236a32c7b6672b140768685fcec65105a2bb3ab374f1be26798a6a45e6ab`，二进制 `upanel-agent 402564380d…7bbc5f9b9a` / `upanel-web 5edd2bbb04…227a7f24ff` / `upanelctl ef69fa7362…258245df3f44`，VM 内 `sha256sum` 核对一致） | docs/evidence/P14/2026-10-27-p14.5/{RESULT,logs/run.log,facts/} |
| P14.5-FIX | P14 | 真机抓出并修掉 **2 个产品缺陷**（Q477：`accounts.Definition` 一个 json 标签都没写 → `display_name` 400，单测只构造 Go 结构体所以全绿；Q478：`GET /accounts` 把 Samba 的能力当账号判据 → 没装 Samba 的机器上账号功能整个不可用）、**1 条与真机相反的文档断言**（Q479："没口令就登不上"——publickey 认证不看 shadow 的口令字段，key-only 账号能登；四处文案 + CONTRACTS §13 已改，脚本 §11 改成"钥匙 / 口令 / 壳"四面）、**2 条脚本缺陷**（`password_changed` 只在改账号时出现；`ssh-keygen -lf` 多把钥匙一行一把被当单值比）；环境那条（`/etc/sudoers.d/` 里非 `0440` 的文件让整份 `visudo -c` 失败）记进 `docs/TEST_ENVIRONMENT.md` §8.12，坏文件留档 `/var/tmp/p145-envfix/upanel.badmode` | docs/evidence/P14/2026-10-27-p14.5/result.md §4、docs/DECISIONS.md Q477/Q478/Q479 |
| P14.5-RESULT | P14 | 已产出（§1 结论对 STAGES §16 的四条验收点逐条给现场读数；§2 十三节 168 条断言的分布；§3 三轮 63/111 → 162/3 → 168/0 的红账；§4 逐条根因（环境 / 产品 / 脚本 / 口径）；§5 六条 NOT RUN 与为什么；§6 证据文件清单） | docs/evidence/P14/2026-10-27-p14.5/result.md |
| P15.2-HOST | P15 | 已执行（`sh scripts/check.sh` 在**最终工作树**上 **PASS 14 / FAIL 0 / NOT RUN 0**、退出码 0；`go test ./internal/deploy/ ./internal/agent/` 全绿，本轮新增 7 个回归用例） | 无独立文件（真机证据见下两行） |
| P15.2-VM | P15 | **已执行，passed**（VM-A Ubuntu 22.04.5 / kernel 5.15.0-198 / docker 29.1.3 / compose 2.40.3，夹具盘 `/dev/sdd` 4 GiB；`PHASE=main` **199 PASS / 0 FAIL / 2 NOT RUN**，`PHASE=boot` **51 PASS / 0 FAIL / 0 NOT RUN**） | `docs/evidence/P15/2026-10-27-p15.2/{RESULT,RESULT-boot,p152-main.log,p152-boot.log,state.env,facts/,logs/,payloads/,README.md}` |
| P15.3-HOST | P15 | 已执行（`go build ./...` 干净；`go test ./internal/agent/ ./internal/deploy/ ./cmd/...` 全绿，`update_test.go` 13 个用例 + `record_test.go` + `probe_test.go` + `deploybackup_test.go`；`sh scripts/check.sh` **PASS 14 / FAIL 0 / NOT RUN 0**、退出码 0） | 无独立文件（本子任务没有可跑的现场；真机读数见下一行 P15.3-VM） |
| P15.3-VM | P15 | **已执行，passed**（VM-A Ubuntu 22.04.5 / kernel 5.15.0-198 / docker 29.1.3 / compose 2.40.3 / restic 0.12.1，夹具盘 `/dev/sdd` 沿用上一轮 GPT；`PHASE=main` **229 PASS / 0 FAIL / 1 NOT RUN**、退出码 0。NOT RUN 只有 `初始化 GPT / 建分区 / 格式化`——盘上已有 GPT 按上一轮复用；产物三份二进制的 sha256 与宿主 `GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath` 重编结果逐字相同） | `docs/evidence/P15/2026-10-27-p15.3/{RESULT,p153-main.log,state.env,facts/,logs/,payloads/,README.md}` |
| P15.3-FIX | P15 | 真机这一轮**没有产品缺陷**；改掉的是验收脚本自身的 **13 条**会把读数读错的写法：`$WORK/logs` 未建导致重定向失败被当成拉不到镜像、`update_params` 读未赋值变量让 `stale` 分支永不触发、`previous` 猜成 `image.previous`、把 `config_unchanged` 当执行期告警、一条自比自的假绿断言、`health_ok` 在未探测时键不存在而 `jget` 抛 `KeyError`、§12 拿“同名同内容 apply”当创建语义判据、`$TMPD` 0700 让 `web_curl` 全部 000、`wait_job` 180s 短于 9 分钟预算、夹具卷残留上一轮数据、§11 拿旧时代的 sha 比对、restic 各版本 `snapshots --json` 的 `id` 长度不一致、`RESULT` 0600 宿主读不到 | 同上（逐条原因见 `docs/evidence/P15/2026-10-27-p15.3/README.md` 的表；口径见 DECISIONS Q494） |
| P15.4-HOST | P15 | 已执行（`npx vitest run` **62 文件 / 1176 用例全过**；`npx vue-tsc --noEmit` 干净；`npx eslint .` 干净；`sh scripts/check.sh` 在本轮最终工作树上重跑，见 §7 的 P15.4-CHECK 行） | 无独立文件（真机读数见下一行） |
| P15.4-VM | P15 | **已执行，passed**（VM Ubuntu 22.04.5 / kernel 5.15.0-198 / 2 vCPU / 单线程 TCG / docker 29.1.3 / compose 2.40.3；跑在发布字节 `af6d9102…14c1` 上：`web/scripts/deploy-smoke.mjs` **43 PASS / 0 FAIL**，登录 → 空状态 → 新建 → 预检 → 再认证 → 任务 → 详情 → 换镜像 → 删除全部经页面完成；三次任务 `outcome: verified`（26/31/18 秒），面板侧 **57 条请求全 200/202**，预检请求体无 `compose_yaml`/`privileged`/`network_mode`/`cap_add`/`devices`；第一轮 31/12 的成因分类见 §5） | `docs/evidence/P15/2026-10-28-p15.4/{README.md,smoke-run2-final.log,smoke-run1-superseded.log,jobs-timeline.txt,web-requests.txt,facts-package.txt,facts-vm.txt}` |
| P15.4-FIX | P15 | 真机前修掉 **1 个阻断项**（三条部署动作 R=是、登录不开再认证窗口、页面没有那扇门 → 真机上按确认必然 403 `REAUTH_REQUIRED`，Q511），真机上又抓出 **2 个页面缺陷**（`StatePanel` 空状态丢标题；删除成功后仍读已删部署 → 注定 404，Q513）与 **1 个观察项**（任务执行中只读查询顶到 10 秒预算却报 `AGENT_UNAVAILABLE`，Q512，不改预算） | 同上；代理侧 10020/10037ms 与面板侧 10008/10025ms 的对照在 README §5 |
| P15.4-RUN4 | P15 | 已执行（**最终字节 `3f3b40c1…daf0`** 上的第四次跑：页面链路 **43 PASS / 0 FAIL**，`会话校验失败` 计数 **0**，会话层两条改成 `WARN 客户端已断开，会话校验中止` + 499；同窗口仍剩一条 `GET /api/v1/apps` 500——那是库读层，Q514 明确未修，见 §8） | `docs/evidence/P15/2026-10-28-p15.4/`（`smoke-run4-final-bytes.log`、`web-requests-run4.txt`、`jobs-timeline-run4.txt`、`client-canceled-misclassified.txt`） |
| P15.2-FIX | P15 | 真机抓出并修掉 **4 个产品缺陷**（Q484 新建部署预检缺版本令牌 → 100% 500；`deployPlanFromError` 漏填 `NormalizedParams` → 带 `deployment_id` 的预检 500；Q485 `start` 对 active 的 oneshot 是空操作 → 配置写了、容器一个都没起；Q486 `is-enabled` 的退出码 1 被当成执行失败 → `deployment.remove` 永远删不掉）与 **5 类验收脚本自身的错误**（起点未清干净、`findmnt --mountpoint`、`runuser` 写不进 0700 目录、删除后用已删的配置数容器、容器 ID 与 `StartedAt` 空对空的假绿） | 同上；原始报错在 `docs/evidence/P15/2026-10-27-p15.2/logs/agent.log` 与 `p152-main.log`/`p152-boot.log` |
| P16.1-HOST | P16 | 已执行（**最终工作树**上 `sh scripts/check.sh` **PASS 15 / FAIL 0 / NOT RUN 0**、退出码 0；因 `.incomplete` 改动在 check 启动之后落地，收尾又单跑了 `sh-syntax`：77 个脚本 `sh -n` 全部通过、解析器 `/usr/bin/sh`；`go test ./cmd/upanelctl/ ./internal/hostcaps/ ./internal/api/` 全绿，go 全包 test 全绿；前端 62 文件 / 1175 用例、`vue-tsc` 干净、`vite build` 成功、`web-smoke` 10/10、`openapi-lint` 通过；**构建与打包是真跑的**：`sh scripts/build.sh` → `sh scripts/release.sh` 产出 `upanel-0.1.0-dev.1-linux-amd64.tar.gz`（sha256 `17ff6518…`、44135630 字节），包内 `install.sh` 34404 B / `uninstall.sh` 10943 B / `backup-state.sh` 12345 B 且三个都是 `0755`）| `docs/evidence/P16/2026-10-30-p16.1/HOST-READINGS.md`（**三个部署脚本在这台 Windows 宿主上一次都没跑起来，只有 `sh -n` 这一个读数，且宿主 `sh` 是 bash、抓不到 bashism**；备份/恢复/升级与能力探测页的真机读数留给 P16.2，8 条 NOT RUN 逐条列在该文件第 4 节） |
| P16.2-VM-2204 | P16 | 已执行（Ubuntu **22.04.5** / kernel 5.15.0-198 / **systemd 249** / 2 vCPU / 3911 MB / `-accel tcg`：七相位 install 40/0/0、measure 14/0/3、boot 14/0/0、upgrade 22/0/0、failup 21/0/0、uninstall 23/0/0、purge 14/0/0，**合计 148 PASS / 0 FAIL / 3 NOT RUN**、驱动退出码 0；真机抓出并修掉 **Q499**（22.04 上 `/logs` 全 400）与 **Q500**（坏包升级伪成功），两条都在本机复验） | `docs/evidence/P16/2026-10-27-p16.2/`（另有 `…-attempt1-scriptbug/`、`…-attempt2-contaminated/` 两轮作废，各自写了理由） |
| P16.2-FINAL | P16 | 已执行（**最终字节 `3f3b40c1…daf0`**（Q514 修复后重打），22.04.5 / kernel 5.15.0-198 / systemd 249 / QEMU TCG，干净盘从零开始：install 47/0/0、measure 14/0/3、boot 14/0/0（含一次真重启）、upgrade 22/0/0、failup 21/0/0、uninstall 23/0/0、purge 14/0/0，**合计 155 PASS / 0 FAIL / 3 NOT RUN**、驱动退出码 0；同一份字节的页面链路 43/43。Q510 口径：读数只属于这四个字节，之前三轮（`fbad9fc9…`/`af6d9102…`/`b1d913e7…`）各自作废或留作旁证） | `docs/evidence/P16/2026-10-28-p16.2-final/` |
| P16.2-2404-FINAL | P16 | 已执行（**同一份最终字节 `3f3b40c1…daf0`** 上的 Ubuntu **24.04.5** / kernel 6.8.0-147 / **systemd 255** / `-accel tcg,thread=multi -smp 2 -m 4096`，干净盘从零开始：install 47/0/0、measure 14/0/3、boot 14/0/0（含一次真重启，`boot_to_web_seconds=75`）、upgrade 22/0/0、failup **首轮 20/1**、uninstall 23/0/0、purge 14/0/0；唯一那条红查清为**断言采样竞态**——`Type=simple` 单元 `Started` 即 active，主进程 exit 1 要 0.23~0.40 秒后才记账，active 占空比约一成（11 轮 journal 微秒实测，Q515）。断言换成「socket 不存在 + 隔 1.2 秒采两次」后在同一份字节上重跑 `install,failup` → 47/0/0 + **21/0/0**，故七相位合计 **155 PASS / 0 FAIL / 3 NOT RUN**，与 22.04 逐相位相同；首轮读数留在 `run1-pre-tighten/`） | `docs/evidence/P16/2026-10-28-p16.2-2404-final/`（`journal-restart-cycle.txt`、`active-window-analysis.txt`、`run1-pre-tighten/`） |
| P16.2-VM-2404 | P16 | 已执行（Ubuntu **24.04.5** / kernel 6.8.0-147 / **systemd 255** / `-accel 'tcg,thread=single' -cpu max`：**147 PASS / 1 FAIL / 3 NOT RUN**；唯一红是 `boot` 相位 `开机到 Web active 152s > 120s`，探针实测面板自身 6.3s、96% 为单线程 TCG 下的客户机引导，判据不改、红保留、裸机读数记未验证） | `docs/evidence/P16/2026-10-27-p16.2-2404/`（含 `facts/boot-attribute.txt` 与 `console.noDatasource-run.log`） |
| P16.2-BROWSER | P16 | 已执行（同机同包，宿主机 Playwright 经 `ssh -L 9443:127.0.0.1:9443` 打真面板：**35 PASS / 0 FAIL / 0 SKIP**；补上 `measure` 里"浏览器完成主要用户流程"那条 NOT RUN） | `docs/evidence/P16/2026-10-27-p16.2-browser/` |
| P16.3-WAL | P16 | 已执行（22.04 真机、空闲 90 分钟稳态，客户机侧**只读**探针 11 个样本 / 5.5 分钟：持有者是 `upanel-web` pid 769；`metric_samples` 913→1687（+774），其余表 0 变化；`-wal` 停在 4,128,272 B 不再长；`PRAGMA wal_checkpoint(TRUNCATE)` → `(0, 1002, 1002)` 归零；代理库空闲一帧未写） | `docs/evidence/P16/2026-10-27-p16.3/`（`facts/sizes.txt`、`facts/tables-before.txt`、`facts/tables-after.txt`、`facts/report.txt`、`facts/wal-probe.tgz`） |
| P16.3-REVIEW | P16 | 已执行（七维度专项复核 + 真机抽样：六个安全响应头、单元 `User=upanel-web` 与七条 hardening、`agent.sock` EACCES 边界、匿名 401 / 未知路径 404 / 错方法 405 / 带外站 Origin 403 / 缺 `X-UPanel-Request` 400；**除 Q505（fsync 缺失，已修并有变异验证）外无阻断项**；表级守护用例 127 条路由，`go test ./internal/api/` 167.9 s 通过） | `docs/evidence/P16/2026-10-27-p16.3/README.md` §3—§7、`facts/http-and-unit.txt`、`facts/socket-boundary.txt`、`facts/live-routes.txt` |
| P16.4-VM-2204 | P16 | 已执行（Ubuntu **22.04.5** / kernel 5.15.0-198 / QEMU TCG，**跑在冻结的发布字节上**：amd64 包 sha256 `751a274d…95fba`、VERSION `1.0.0` / `8e52b36-dirty` / `2026-10-28T03:59:13Z`；七相位 install 47/0/0、measure 14/0/3、boot 14/0/0（`boot_to_web_seconds=56`）、upgrade 22/0/0、failup 21/0/0、uninstall 23/0/0、purge 14/0/0 = **155 PASS / 0 FAIL / 3 NOT RUN**、驱动退出码 0；install 相位新增的 7 条文档断言全 PASS〔六份关键文档 + 份数 13〕） | `docs/evidence/P16/2026-10-28-p16.4/`（`RESULT-ALL`、`phase-*.log`、`facts/packages.txt`；另有 `prelim-docdelta/` 存第一轮 `fbad9fc9…` 的同套文件，作废理由见 README §4） |
| P16.4-FSYNC | P16 | 已执行（真机 ext4 `/dev/sda1`：生产 `osWriteFS` + `writeFileAtomic`，交叉编译成测试二进制由 root 跑、外套 `strace`——`openat(tmp,0600)` → `fsync(tmp)=0` → `renameat=0` → `openat(dir,O_RDONLY)` → `fsync(dirfd)=0`，覆盖写同路径、无残留 `.upanel-tmp`、内容与权限 `0600` 对、`PASS` 0.13 s。**证明写路径在真机 ext4 上走得通，不证明掉电语义**） | `docs/evidence/P16/2026-10-28-p16.4/facts/fsync-probe.txt`（含二进制/源码 sha256 与完整 strace 序列） |
| P16.4-HOST | P16 | 已执行（所有改动落定后的工作树 `sh scripts/check.sh` → **PASS 15 / FAIL 0 / NOT RUN 0**，十五项全 PASS；`go-test` 中 `internal/network` 显示 `(cached)` 属正常：新探针带 `//go:build linux`，宿主被构建约束排除，**宿主永远不算它通过**） | 见 `docs/evidence/P16/2026-10-28-p16.4/README.md` §6 |
| CLOSE-HOST | P16 | 已执行（Q514/Q515 与文档更正落定后的工作树，工具链钉在 `node 22.23.3` + `go1.27.3` 并前置到 PATH：`sh scripts/check.sh` → **PASS 15 / FAIL 0 / NOT RUN 0**，十五项全绿；同日先跑过一次没前置的，`node-version` 红在系统 `node v24.15.0`，那是环境不是代码，见 §8 末条） | `docs/evidence/P16/2026-10-28-close/`（`check-sh.log` + 环境说明） |
| P16.2-2404-BROWSER | P16 | 已执行（**最终字节 `3f3b40c1…daf0`** 上的 Ubuntu 24.04.5 / systemd 255 / 单线程 TCG，系统盘从基底镜像重建、包里一个字节没改：安装相位 **47/0/0**、`smoke.mjs` **35/0/0**、`deploy-smoke.mjs` **43/0/0**（exit 0）；`UPANEL_SMOKE_TIMEOUT_SCALE=4` 只放大等待、断言不放宽；跑完 0 容器 / 0 compose 项目 / 0 部署单元，`会话校验失败` 0、panic 0） | `docs/evidence/P16/2026-10-28-p16.2-2404-browser/`（`smoke.log`、`deploy-smoke.log`、`facts-vm.txt`） |
| P16.2-2404-DOCKER-GATE | P16 | 已执行（**同一份字节**上写链路首轮就红：`deploy-smoke.mjs` 10 项后卡 8 分钟，客机 journal 是 `拒绝未登记动作的预检 action=deployment.apply` + `preflight status=400`。查清为启动时门——写动作按`firstExistingFile(deploy.DockerBinary)` 在代理启动时登记，而这台机器先装面板后装 docker。重启机器也能好（代理会重起），但不必：`systemctl restart upanel-agent` 之后在**同一份字节同一台机器**上重跑就是 43/0/0。同一处代码里 `hostCaps` 每 10 分钟重探、写动作登记不跟着重探，且 hint 没写"装完重启代理"——**已登记 Q516，本轮不修**（修它要重打字节 → Q510 两台全重跑） | `docs/evidence/P16/2026-10-28-p16.2-2404-browser/deploy-smoke-run1-docker-after-install.log` |
| P16-RACE-FULL | P16 | 已执行（**产品代码零竞态**。24.04.5 / kernel 6.8.0-147 / 2 vCPU / `-accel tcg,thread=multi` / root：全量 `-race -count=1 -timeout=90m ./...` 墙钟 **3h57m**，54 包（51 带测试）**44 ok / 7 FAIL / 3 撞超时 / 1 条 `DATA RACE`**；补跑改 `-timeout=0` 再 **9h03m** 把三个大包跑完——`internal/api` **28929.178s（8h02m）0 竞态 0 失败**，`cmd/upanelctl`/`journal`/`backup`/`storage` 全 `ok`。唯一那条 `DATA RACE` 与后面爆出的红**全部落在测试侧**，五个只动 `_test.go` 的修正 Q517/Q518/Q519①②/Q520 之后复跑全绿；判据实验五条，上界一个没改） | `docs/evidence/P16/2026-10-28-race/`（`README.md` §5 全量逐包 54 行、§6 补跑与五个判据实验、§7 没证明什么；`rerun.log`、`race-re-*.body.log`、`verify3.log`—`verify6.log`、`facts-env.txt`、`facts-rerun.txt`、`panic-dump.txt`、`idle-panel-cpu.txt`） |
| P16-MATRIX | P16 | 已产出（STAGES §19 的 **31 条**逐项对照：建表当时 **已验 30 / 部分 1 / 未验 0**，每条给"目录 + 节"级指针与关键读数；当时唯一剩下的"部分"是 DISK-02 的 LVM/RAID 根盘从未造过（`P00-VM-C` 仍 NOT RUN）——**同一天 `disk02/` 那一轮把它补成已验，现在合计 31 / 0 / 0**；**FILE-02 的 ENOSPC 与 APP-02 的 302 于同日用 `gaps/` 补上真机现场后改成已验**（`PASS 80 / FAIL 0 / NOT RUN 0`）；**PROXY-02 经复核是引错证据，已改成已验**，见矩阵 §2.4）。文中证据指针用脚本逐个核过存在性（建表时 45 条，`gaps/` 补两条后 47 条，`disk02/` 再补四条后 **51 条**），0 缺失；另把 RELEASE-01 按 P16 七相位展开对到 STAGES §18 的五个验收动作）| `docs/ACCEPTANCE_MATRIX.md`；本轮读数 `docs/evidence/P16/2026-10-29-matrix/`（`verify-pointers.py`、`facts-pointers.txt`、`facts-check.txt`；另 `check-decisions-table.py` 与 `facts-decisions-table.txt` 量 Q 表列数，见 Q521） |
| P16-RACE-HOST | P16 | 已执行（五个 `_test.go` 修正落定后的工作树：`sh scripts/check.sh` → **PASS 15 / FAIL 0 / NOT RUN 0**；**先在系统 node 24 上跑出 13/2，两项红都是工具链不在 PATH（固定版 node 22.23.3 在这台机器的 `C:\Users\huxi\tools\` 下、不在仓库里）**，换对工具链后 15/0（`~/tools` 与 `/h/Code/.toolchains` 各跑一次），三份读数都归档；`find … -newermt '2026-10-28 13:23:36'` 命中 5 个 `_test.go`、**非 `_test.go` 改动 = 0 个**，即产品字节与交付包 `3f3b40c1…daf0` 仍相同） | `docs/evidence/P16/2026-10-28-race/README.md` §6.11 + `facts-host-check.txt` |
| P16-MATRIX-GAPS | P16 | 已执行（把矩阵三条"部分"逐条去补：**APP-02 与 FILE-02 补成已验**，`PASS 80 / FAIL 0 / NOT RUN 0`；DISK-02 的 LVM 根盘脚本不覆盖、仍是部分。真机读数：302 金丝雀命中 0 次；ENOSPC 写入时撞满 → 413「空间不足」+ `.part` 清 + 记录留 failed + 目标未改；2 GiB 真推 200、峰值 RSS 28740 KiB、落位 sha256 一致。三次运行，前两次是脚本自身缺陷，缺陷与修法写在 `gaps/README.md` §5）| `docs/evidence/P16/2026-10-29-matrix/gaps/`（`2404-matrix-gaps.log`、`binaries.txt`、`payloads/`）；脚本 `tests/integration/p16-4-matrix-gaps.sh` |
| P16-DISK02 | P16 | 已执行（把矩阵最后一条"部分"关掉：在客机里迁出两台**根在叠层设备上的** Ubuntu 24.04——LVM 根 `91/0/3`、RAID1 根 `113/0/3`，产品代码与 `_test.go` 零变化）。递归的证明是「物理盘自己没有挂载点、没有文件系统，却 `system_disk=true` 且 `protection.sources` 只有根设备」（`sda`/`sdb` 的 `filesystem` 都是 `null`），同盘 bios_grub 分区仍是 `false`——沿链而非按盘染红。四条写动作全 `SYSTEM_DISK_PROTECTED`、整盘首末 1 MiB 哈希前后一致、写原语已接线。**RAID 第一轮红 5 条锁断言，查明是脚本期望写错（Q522，产品零改动）**，改后重跑全绿；pre-fix 读数留在 `runs1-pre-fix/` | `docs/evidence/P16/2026-10-29-matrix/disk02/`（`README.md`、`binaries.txt`、`lvm/`、`raid/`，两份读数 + 原始 `payloads/` + 迁移脚本）；脚本 `tests/integration/p16-5-root-disk-recursion.sh` |
| P00-VM-C（LVM 根） | P00/P04 | **已建并已验**（2026-10-29）——`upanel-lvm`：根在 `/dev/mapper/upanel--vg-root`（`dm-0`，PV 是 `sda2`）。读数 `91 PASS / 0 FAIL / 3 NOT RUN`，见 `docs/evidence/P16/2026-10-29-matrix/disk02/lvm/2404-lvm-root-guard.log`。**它是 STAGES 说"最晚 P04"、实际一直缺到 P16 的那个现场**；补上之后 DISK-02 才从"部分"变成"已验" | 已补齐（P04 时缺，P16 收尾补上）|
| P00-VM-D（SMB 客户端） | P00/P08 | **NOT RUN**（未创建；P08.2 用同机真实 `smbclient` + netns 客户端替代，跨机器网络路径仍缺） | 需在 P08 前补齐 |

核心验收矩阵（SEC/JOB/DISK/FILE/TERM/SHARE/APP/PROXY/PKG/BACKUP/TIMER/ALERT/NET/NFS/DEPLOY/RELEASE）
**已按编号逐项建成对照表 → `docs/ACCEPTANCE_MATRIX.md`（2026-10-29）**：31 条逐行给状态与
"目录 + 节"级证据指针，**建表当时 已验 30 / 部分 1 / 未验 0**；同一天补完，现在合计
**已验 31 / 部分 0 / 未验 0**。四条曾经判"部分"的：三条补上了现场、一条是引错了证据：

* **PROXY-02**（10-30 更正）：P10.4 在两台机器上都跑出 WS/wss 的 101，初版只引了 P10.2 的 NOT RUN。
* **FILE-02**（10-29 补）：ENOSPC 真撞（413 `VALIDATION_ERROR`「空间不足」、`.part` 清、记录留 failed、
  目标未改）、断流（代理记「传输中断（已写入 3145728 字节）」）、**2 GiB 真推一遍**（`200`、
  `received_bytes=2147483648`、峰值 RSS 28740 KiB、落位 sha256 一致）。`P16/2026-10-29-matrix/gaps/`。
* **APP-02**（10-29 补）：两个真 302 上游（→回环金丝雀、→`169.254.169.254`），
  **金丝雀命中 0 次**证明没有跟随重定向，记录里 `address` 仍是 `127.0.0.1`。同一目录。

* **DISK-02**（10-29 补）：在客机里迁出两台根在叠层设备上的 Ubuntu 24.04——LVM `91/0/3`、
  RAID1 `113/0/3`。证明靠的是"物理盘自己没有挂载点、没有文件系统，却 `system_disk=true` 且
  `protection.sources` 只有根设备"，同盘 bios_grub 分区仍是 `false`（沿链而非按盘染红）。
  `P16/2026-10-29-matrix/disk02/`。这一轮还抓出一条写错的锁断言（Q522，脚本错、产品零改动）。

**所以现在 31 条里一条"部分"都不剩。** 但"一条不剩"不等于"什么都验过了"：子句之外的 NOT RUN
（臂 64 端到端、掉电演练、裸机开机到可用、0.1→1.0 升级、降级/重建中的阵列、根在叠层设备上写满……）
仍逐条登记在 `docs/KNOWN_LIMITATIONS.md`。
表里每个数字都来自它指的那个文件，指针逐个用脚本核过存在性（建表 45 条，补 `gaps/` 两条后 47 条，
补 `disk02/` 四条后 51 条，均 0 缺失）。

下面这段历史原话保留（它记的是缺口建立之前的实况，不再代表现在）：

“全部尚未开始。其中 JOB-02（预检到期/换参数/目标变化 →
拒绝且不执行）在 P03.2 的提交路径上 **已有宿主测试 + 两台 VM 的真实系统证据**（`p03-2-jobs-acceptance.sh`
的 `== 4`/`== 5` 段：过期、换参数、他人预检、被阻塞、再认证超窗各自拒绝且不落 job）。”
**但这句到今天已经不准了**：JOB 组现在有 `service.control`/`process.signal`/七个文件动作/
`terminal.create`/四个磁盘动作/五个卷动作这些**真实业务动作**在执行（见 §8 的“已解除的历史限制”）；
DISK 与 FILE、TERM 的实质内容分别在 P07 与 P06.1/P06.2、P06.3 有真机证据；SHARE/APP/PROXY
在 P08/P09/P10 有真机证据，**PKG 的只读与写动作**在 P11.1/P11.2 有真机证据，**电源
（重启/关机的闸门、倒计时取消、`boot_id` 确认与“不伪称已验证”）**在 P11.3 有真机证据。
真正连只读都还没有的矩阵条目是
ALERT/NET/NFS/DEPLOY/RELEASE。**BACKUP 现在有仓库、运行、恢复与清理的真机证据**
（P12.1/P12.2/P12.3：本地/SFTP 仓库、策略、快照、stats 回读、停应用、面板库在线副本、
断网/写满/取消，以及产品侧恢复到新目录、覆盖恢复、两态验证、保留清理）。**BACKUP-01
（备份后恢复、错误密码、网络断开 → 校验一致、失败不假成功）的三条现在都有产品侧证据**：
恢复后哈希一致（P12.3 §2/§3）、错误口令由 P12.1 的 `state-wrongpass-says-password-wrong`
覆盖、网络断开标失败且不动原数据（P12.2 运行断网 + P12.3 §5 恢复断网）。仍然缺的是
**timer 触发**（P12.4：不重叠、错过执行、时区）**现在也有真机证据**（两台各 246/0/0，
`docs/evidence/P12/2026-10-24-p12.4/`）；仍然缺的是**浏览器 UI**（P12.5），不涉及 timer 的
部分不能提前记为整条矩阵通过。TIMER 同理，P12.1/P12.2/P12.3 都不涉及定时器
（P12.4 才接 timer，证书续期 timer 的真实执行在 P12.5）。
**矩阵编号与已有断言的对应关系未建立过**这件事，曾经在这行挂了很多轮（"这是账本的一个缺口，
不是产品的缺口"）。**2026-10-29 补完，缺口关闭**——对照表在 `docs/ACCEPTANCE_MATRIX.md`，
状态口径写死成三档（已验 / 部分 / 未验），"部分"的四条逐条写了缺哪个子句、要什么现场才能补。

## 8. 已知限制

- **P15.2 的六条限制（真机刚刚开始有读数，限制比读数多）**：
  ① **`unit_restarted` 说的是"我怎么交的班"，不是"容器真的重起了"**：它为 `true` 只表示这一轮
  用的是 `restart` 而不是 `start`（单元本来就在盘上、这次又写了新内容）。容器是否真换了一个，
  只能靠 `docker inspect` 的 ID 与 `StartedAt` 另证——重放那一轮 `written=false`，
  所以 `unit_restarted=false`，单元却一直是 active。把这两件事混成一件会得出"重放重启了单元"的
  错结论（Q485）。
  ② **"退出码是答案"这条依赖 `syscmd.ExitError` 这个类型**：`systemctlAnswer` 只把
  `*syscmd.ExitError` 当答案，其它错误仍算失败。假 runner 已改成同形（非零退出返回
  `*syscmd.ExitError`、`enable`/`disable` 真翻状态，`Q486`），但"假件比真机宽松"这类风险不会
  因为补了一次就消失：以后任何新动词的假件都要按真机形状写，否则回归测试挡不住真机。
  ③ **两条 NOT RUN 是真机造不出的现场，不是没跑**：`容器里读到的 marker`——镜像是
  `traefik/whoami`（scratch），容器里没有 `cat`/`sh`，挂载只能由 `docker inspect` 的 `Source`
  与 `findmnt` 交叉核对；`运行中卷失联 → systemd 自动停单元`——容器已把卷 bind 进自己的 mount
  namespace，只能靠 `umount -l` 造假现场，那样的读数不当证据。"卷不可用 → 起不来"这一半用
  `systemctl mask <mount unit>` 的故障注入验了，两件事不互相顶替。
  ④ **只有 22.04.5 一台机器的读数**：kernel 5.15.0-198、docker 29.1.3、compose 2.40.3。
  24.04 的 systemd 事务行为、compose 版本差异（尤其 `up -d --no-build` 的措辞与 `down`
  的 `--remove-orphans`）都没有读数，不外推。
  （**这一条后来补上了，原文不改**：P16.2 的 24.04 轮给出了七相位 `155 / 0 / 3` 与部署写链路
  `43 / 0 / 0`，见 `docs/evidence/P16/2026-10-28-p16.2-2404-final/` 与 `…-2404-browser/`。
  它说的是 **P15.2 当轮**的口径，不是今天的状态。）
  ⑤ **更新这条路在 P15.2 时是空的（P15.3 已补上实现并跑通真机）**：P15.2 那轮的预检回
  `update_not_implemented` 并指向 P15.3。P15.3 已经把 `deployment.update`、镜像 digest 记录、
  按选择备份、启动后健康探测与失败可见、回切边界都实现并真机验收了（229 PASS / 0 FAIL / 1 NOT RUN，
  见 §5 的「P15.3 真机验收」小节）。
  ⑥ **句柄级路径核对仍缺（P15.2 前半程那条限制继续生效）**：容器看见的目录是靠
  `Source` 与 `findmnt` 交叉核对，没有从容器进程的 `/proc/<pid>/root` 逐级 `stat` 比对；
  另外"单元 active 但项目里 0 个容器 → 任务降级成 `partial` + 给恢复提示"这条有单测
  （`TestDeployApplyExecutePartialWhenProjectEmpty`），真机上也**偶然**触发过一次——就是 Q485
  那个 `start` 空操作的现场；但**没有安排一个会立刻退出的镜像**去做专门验收，所以
  "容器刚起就崩溃"这条路只有单测读数。
- **P15.3 的七条限制（实现齐了，真机七段已跑通）**：
  ① **真机读数已有，但不是全覆盖**：正常更新、健康失败回切、按选择备份、digest 记录与六种拒绝
  都在 VM-A 上跑通了（229 PASS / 0 FAIL / 1 NOT RUN，`docs/evidence/P15/2026-10-27-p15.3/`）。
  仍然只有单测读数的是：`image_digest_mismatch`（注册表按 digest 寻址，钉错 digest 会让
  `docker pull` 先失败，真机造不出那个现场）、时间预算切段把时间耗到边界上逼出 `partial`、
  连续两次失败的回切。单测里的 docker/systemctl/备份/探测仍是假件——它们证明调用顺序与判据，
  真机读数证明"按我们写的字节真的起来了"，两者不互相顶替。
  ② **健康探测证明的是"宿主端口能应答"，不是"应用正确"**：`http` 只判状态码落在 `200—399`，
  一个返回 200 的错误页算通，一个返回 500 的可用首页算不通。这是健康检查的固有边界，不是缺陷，
  但页面上写"健康"时要知道它说的是哪一件事。
  ③ **只支持 `http` / `tcp` 两种 kind**：`exec` 类容器内健康检查不在契约里（面板不知道镜像里
  有什么命令，塞一条 `CMD-SHELL` 进去既是注入面又会误报）。
  ④ **回切只回切配置与镜像，不回切数据**：有数据库迁移的应用，新版本动过的数据旧版本读不了。
  这条写进了错误消息，但**没有任何检测手段**能提前发现"这次更新会动数据"——这一版明确不做，
  也不假装做。
  ⑤ **更新前备份只收 `files_only`**：`stop_managed_apps` 需要 Web 侧展开的"应用运行对象"，
  `deployment.update` 的参数里没有。要停多个应用的备份策略在预检就被拒
  （`backup_policy_needs_apps`）。
  ⑥ **备份预算 360 秒是刻意的窄口**：数据量大的应用几乎一定超预算，那就得先单独 `backup.run`
  再更新。这一条会被人当成"限制"抱怨，但反过来（让更新安静地跑四十分钟、最后把应用留在停止
  状态）更糟。
  ⑦ **`.prev` 只有一份**：连续两次更新之后回切目标只有"上一版"，回不到上上版；`.prev` 也被
  `deployment.remove` 一并删掉。要留多版得另做快照，不在这一版的范围里。
- **P11.1 的五条限制**：
  ① **`rc`（仅剩配置）与 `security` 筛选的边界只在单测里覆盖过**：两台验收机上没有
  `rc` 包，`packages-default-excludes-rc` / `packages-installed-false-includes-rc` 是
  **NOT RUN**，不是 PASS。要真验得先在一台一次性 VM 上卸一个包并保留配置（`dpkg -r`），
  那会改动验收机现场，所以没做。
  ② **`apt-cache` 失败时的降级只走了单测**：真让 `apt-cache` 失败会破坏验收机的 apt。
  降级路径是“候选全 `unknown` + `errors` 里写原因”，页面显示“读不到”而不是“已是最新”，
  但这一条在真机上是空白的。
  ③ **`upgradable` 用的是 `candidate > installed`，不是 apt 自己的分阶段推送结论**：
  真升级时 apt 可能因为依赖保留少装几个（`apt-get -s upgrade` 的“被保留”），这个差别要
  到 P11.2 才判。现在页面上的“可升级 9 个”是候选更新的个数。
  ④ **`GET /packages` 每次最多 200 条**，731 个包要翻 15 页（实测 15 页、0 重复）。没有
  一次导出全量，也没有服务端搜索索引；24.04 上一次冷采集 26.6s、首次调用 11.6—15.6s，
  之后命中 30 秒快照与候选缓存（键 = 已装版本 + `/var/lib/apt/lists` mtime）。**启动预热
  是故意没有的**：开机就采一次实测把 26s 拖成 62s，健康检查因此误报 `AGENT_UNAVAILABLE`。
  ⑤ **`p02-3-agent-acceptance.sh` 的期望清单改了字符串但没重跑**：
  `storage.partition.table.initialize` → `storage.partition_table.initialize`（脚本写错了，
  真实能力表里是后者）。这一条记为 **NOT RUN**，不是“已通过”。另外
  `tests/integration/run-all.sh` 的 `ROOT_SCRIPTS` 只列到 p07-4，P08 之后的脚本要单独跑。
- **P10.4 的五条限制**：
  ① **`systemctl is-active` 的时延是这台机器的，不是产品的**：2 vCPU 虚机上实测单次
  11.4s / 16.5s（40 次采样）。探测现在最多问 3 次、每次 10 秒预算，三次都读不到时**不会**reload
  （`served=false` + `unit_state_error`），报告会要求人工 `systemctl reload nginx`——这是如实
  说明“没有证据”，不是“配置错了”。
  ② **优雅 reload 有毫秒级窗口**：`nginx -s reload` 返回后 6—111ms 内新配置才开始应答，
  连续 reload 时 20 次里有 7 次仍被旧 worker 应答（Q308）。所以 `verified` 的含义是“落盘 + 读回
  hash 一致 + 单元 active + reload 完成”，**不是**“这一刻的新连接一定由新配置应答”。
  ③ **22.04 镜像里的 python 读不到系统信任锚**（0 个，24.04 是 120 个，Q311）。验收的 wss 检查
  显式给 `--ca`，“系统信任链”由 curl 单独证明；这两件事不互相顶替，也不代表 22.04 上的浏览器
  有问题——那是 python 的 ca 目录行为。
  ④ **登录与再认证在这两台虚机上要数秒**（Argon2id，Q292；CONTRACTS 写的是 100–300ms）。
  参数标定不在 P10 范围，本轮只如实记录。
  ⑤ **arm64 包只验证了能打包与校验和正确**，真机验收全部在 amd64 上（这是 0.1 的证据范围）。

- **P09.5 的三条限制写在前面**：
  ① **能力会晚一拍**：`runtime_state` 来自 services 采集器的快照（默认 10 秒一轮），刚发完
  `service.control` / `container.control`，摘要还会说着上一轮的事实。这不是缓存出错——摘要如实
  描述它读到的那一轮，页面照着请求回来的那一份画、不自己“乐观更新”。要更快的反馈得改采集节奏，
  那会影响整台机器的 systemctl 调用频率，不在本轮范围。
  ② **搜索是页面侧的**：`GET /apps?q=` 不是契约参数（传了也只当没看见，结果集不变），筛的是
  当前已经取到内存里的那一份。应用条数很多时要靠服务端的游标分页把数据取全，两件事互不替代。
  ③ **没有真实浏览器证据**：页面级证据是 `renderToString` + 断言（DECISIONS Q246 的约定），
  没有 Playwright 截图、没有人工点检、跨浏览器与移动端布局未验证；并发（两人同时收藏同一条、
  两个管理员同时改同一个分组）与大数量下的分页/虚拟滚动也未验证。
- **`api/openapi.yaml` 当前版本 `0.0.0-p09.5`**（`stage: P09.5`）：本轮**没有新增接口**，
  只重写了 `GET /api/v1/apps` 的 `group_id` 参数说明（Q277：形状不对是 400，不是“当作没有这个分组”）。
  上一轮（`0.0.0-p09.4`）新增 `/api/v1/apps/{id}/logs` 与 `AppJournalLogsData`/`AppContainerLogsData`，
  并重写了 `runtime_state`、`dependency_protection`、`AppCapabilities` 三处说明。lint 仍是
  **7 条 warning**（`no-unused-components` 4、`operation-4xx-response` 2、`operation-2xx-response` 1），
  与 P07 起同数同源。
- **P09.4 的三条限制仍然生效（前两条看起来像 bug 但不是）**：
  ① **`managed` 不等于依赖被接管**——它只说“经面板发起的启动会被依赖卷门禁检查”。手工
  `docker start`、开机自启、`systemctl enable` 过的单元、`restart: always` 这四条路都绕过面板，
  所以这几种一律标 `unmanaged` 并在 `dependency_protection_note` 里点名是谁绕开了（Q274）。
  ② **运行期卷掉线不会自动停机**——门禁拦的是“下一次经面板的启动”，已经跑着的容器不会被停；
  要做这件事需要代理侧常驻协调者，本轮明确推迟，不拿门禁冒充（Q276）。
  ③ **docker 的 `failed` 这一版不区分**——DTO 里没有 ExitCode，非 running 一律 `stopped`，
  退出码留在 `state_text` 里（Q275）；这一条是“少说一句”，不是“说错一句”。
- **`api/openapi.yaml` 的 lint 计数从 P07.5 起就没动过**：那一轮（版本 `0.0.0-p07.5`）补了三条卷接口
  （`GET /storage/volumes`、`/storage/volumes/{id}`、`/storage/volumes/{id}/blockers`）与五个 schema，
  `capacity` / `observed_state` / `dependencies` 的空值语义写进 description。之后每一轮只推版本号与
  新增路径，那 **7 条 warning**（`no-unused-components` 4、`operation-4xx-response` 2、
  `operation-2xx-response` 1）一直同数同源——它们不是“新接口没写进去”，是历史遗留的组件与响应声明，
  未清理（当前版本见本节第一条）。
- P03.3 的 SSE 只做了短时验收：**没有**长时间压测（连接挂机数小时、4 条连接跑满再反复断线重连、
  代理频繁重启时的重连风暴），连接上限与释放只有秒级断言；日志环裁剪也没有在“一个任务产出
  上万行日志”的真实场景下观测过容量与耗时。
- SSE 的推送方式是**每连接 1 秒轮询 + 对账**（DECISIONS Q69），不是进程内广播中心：
  单机面板够用，但 N 条连接就是 N 次/秒的本地查询，且这个方案不跨实例（多实例部署要换机制）。
- 任务中心的浏览器证据只有 node 层的逻辑测试（可注入 `EventSourceLike`）与构建产物，
  **没有**在真实浏览器里打开过任务中心、没有观察过帧刷新/重连/gap 的实际表现；
  Firefox/Safari 完全未验证。
- 登录页是 P03.3 的范围补充（DECISIONS Q72，待确认）：只做登录/退出/会话恢复/守卫/回跳校验，
  没有注册、找回密码、MFA，也没有账号管理界面（P02.2 的接口仍然只能靠 API/脚本使用）。
  会话 cookie 带 `Secure`，浏览器直连明文回环之外的场景要等 P10 的 TLS。
- viewer 看不到别人的任务（DECISIONS Q70）：多管理员协同时没有“全局任务视图”，
  P04 概览也不提供；要改必须先改 CONTRACTS。
- `go test -race` 已能执行（P04.3）：在 22.04 验收 VM 上装了 gcc 11.4 与 Go 1.27.3，
  整模块 `go test -race -count=1 ./...` → 19 包 ok、**0 条 `WARNING: DATA RACE`**。
  宿主（无 cgo/gcc）仍执行不了 `-race`，所以**宿主上的竞态证据仍然没有**；
  唯一在 `-race` 下失败的是 `TestDefaultPasswordParamsTiming`（Argon2id 被检测器放大到
  1.79—2.03 秒，用例上界 1 秒），不带 `-race` 时 0.6 秒通过——这是检测器开销，不是数据竞争。
- 代理/Web 同时重启时的任务对账收敛、重复提交与“未知结果”的人工处理属 P03.4，本轮未验证。

- 已有能力限于：P01 的骨架/持久层/前端/打包部署形态，P02.1 的认证与会话，P02.2 的账号管理与
  审计事件，P02.3 的 Web↔代理通道与执行器边界，P02.4 的审计事务化/日志脱敏/代理独立库与秘密存储，
  P03.1 的预检生成与绑定，P03.2 的持久任务/幂等/去重/资源锁/取消与对账，P03.3 的事件流（SSE、
  断线续传、gap、日志环）与任务中心（**任务机制完整，但仍无
  生产动作可执行**），P04.1 的系统基础资源**只读**采集（CPU/内存/负载/网络/主机信息，
  代理侧 5 秒轮询缓存，`GET /api/v1/system/summary`），P04.2 的存储拓扑**只读**视图（块设备树、
  边、设备身份指纹、系统盘保护、分区表类型与空闲区间、挂载点容量；`GET /api/v1/storage/topology`
  与 `/storage/devices[/:id]`），P04.4 的首页/磁盘/拓扑详情页面，P05.1 的服务列表、详情与保护名单与
  `service.control`，P05.2 的进程列表与 `process.signal`（TERM/KILL 两次独立操作），P05.3 的日志
  查询与实时流，P06.1/P06.2 的受管理文件根/列表/文本/下载与七个写动作 + 上传，P06.3 的受限 Web 终端
  （`upanel-shell` 的 PTY、两跳 WS、四道门、一次性许可、时限与元数据审计）。
  P07.1—P07.5 的磁盘写能力（分区表初始化、建/删分区、ext4/XFS 格式化、五个受管理卷动作，
  页面九种动作入口、`docs/STORAGE_RECOVERY.md` 与不经过面板的外部独立检查脚本）。
  P08.1—P08.4 的 SMB 共享（受管理账号、密码只走标准输入、POSIX ACL、`volume_id` 绑定、
  启动依赖与运行时脱盘、共享页面与客户端连接说明）。P09.1—P09.5 的**应用视图**（应用/分组/收藏
  元数据与判别式绑定、本机发现与管理员确认候选、健康检查与 SSRF 约束、容器只读查询与日志、
  现有受支持容器的 start/stop/restart、应用摘要的运行时状态/能力/依赖保护、`/apps/{id}/logs`
  与依赖卷启动门禁、以及 P09.5 的页面：卡片四件事分开画、搜索、分组、一键打开、常用入口首页）
  ——注意 P09 的“写”只有两件事：`service.control`/`container.control`
  这两条既有动作被门禁检查，**没有**新的写接口，也没有“执行任意配置”；P09.5 一行后端代码都没改。
  viewer 目前的“只读”体现在账号管理、任务查询与各只读接口上（写操作一律 403）。
  **代理的生产登记表从 P05.1 起不再是空的**：`service.control` 是第一个真实写动作，
  但它只做“单个服务单元的启停/重启/开机启动”，仍然没有万能命令执行器；
  P06.2 的七个文件动作同样只作用于**受管理根**内的路径，P06.3 的终端只有字节通道（无 `run` 帧、
  无 `POST /exec`、无 sudo）。
  磁盘动作只对**非系统盘**生效，系统盘与不支持的复杂设备一律由判决层拒绝；
  反向代理、网络回滚、备份恢复这些高风险能力仍然只有约定，没有实现，更没有 Linux 证据
  （共享从 P08 起已有实现与真机证据，故不在此列）。
- **已解除的历史限制（P03.2 时代写的，这里保留原话以免“悄悄改账”）**：
  ① “能执行的动作仍然只有 `-acceptance-actions` 下的验收动作、生产登记表为空”——从 P05.1 起生产登记表
  依次登记了 `service.control`（P05.1）、`process.signal`（P05.2）、七个文件写动作（P06.2）、
  `terminal.create`（P06.3）、四个磁盘动作（P07.2/P07.3）与五个卷动作（P07.4）。
  ② “`POST /api/v1/jobs` 返回的 `events_url` 还不可用”——SSE 在 P03.3 就接通了（断线续传、`event: gap`、
  日志环）。
  ③ “状态版本/设备身份的执行前复核没有在真机上用真实漂移验证过”——P07.2 用真实设备消失证明了
  `DEVICE_IDENTITY_CHANGED`（该条在 P07.2 记 NOT RUN、P07.3 用二次身份检查补上机会性证据）、
  P07.3 用真实字节变化证明了第二道门、P07.4 用外部编辑 fstab 证明了 `PREFLIGHT_STALE`。
- 代理资源锁是**进程内**的（P03.2，DECISIONS Q58—Q65 一并记录）：只覆盖同一个代理进程里的并发，
  外部 apt/systemctl/配置编辑器不认这把锁。30 秒排队上限与 `RESOURCE_BUSY` 已有单测；
  两台 VM 上验过的排队是“同锁两个任务顺序执行、按锁名排序不死锁”，**真正打满 30 秒超时的
  排队未验证**（验收里没有跑到那个边界，因为验收动作太快）。跨进程/持久锁（重启后仍生效）
  不在 P03.2 范围内。
- 取消只在“可中断点”生效：可取消动作在等待锁与执行器的检查点之间响应 `ErrCanceled`，
  已经进入不可中断的单个系统调用的动作取消不了（记 `cancellation_requested=true` 而不是 `canceled`）。
  这是刻意的诚实，但也意味着“点了取消”不等于“没发生”——前端必须如实展示。
- 短语字母表与长度（32 进制 6 位，约 30 位熵）是按“手抄一次 + 120 秒 + 一次性”选的；
  **提交接口上没有针对短语的限速**：`POST /api/v1/jobs` 只受会话与 admin 约束，一个已登录管理员
  可以连续猜短语。当前威胁模型里管理员本身是可信的（能猜到自己预检的短语不是攻击），
  但外部暴力入口的限速仍待补，登记为未验证项而非已解决。
- 预检记录会一直留在 Web 库里直到账号被删除（外键级联）；已消费的行保留 `consumed_job_id`
  作为一次性证据，**未消费且过期的行没有清理任务**——P03.2 决定“先保留”，因为它们是审计对照物；
  保留策略与清理作业属后续阶段（表会缓慢增长，已在已知限制里登记）。
- 账号管理没有前端页面：P02.2 只交付接口（STAGES.md 未要求 UI），因此加载/空/无权限状态与
  再认证弹窗都还没有浏览器证据，操作只能通过 API/脚本完成。
- 再认证窗口的 5 分钟有效期、跨会话与跨重启行为只在宿主的可控时钟单测里验证；VM 上没有实等到期。
- 审计已与业务同一事务（P02.4，DECISIONS Q42）：业务回滚则审计一起消失，不再有“业务生效但审计丢失”
  的窗口。仍存在的限制：被拒绝的请求（没有业务事务）独立提交，写失败只留 ERROR 日志——因为那时
  已经没有任何事务可以回滚。
- 审计的 `redacted_details` 与日志脱敏都是**按键名/值形状**命中，非口令类自由文本（例如显示名里
  塞秘密、或一段自然语言描述里夹着口令）不在保护范围内；真正的防线仍是调用点不传秘密。
- 秘密存储的密钥 `/etc/upanel/agent.key` 只有一份，没有托管/轮换流程；密钥丢失等于已存秘密永久
  失效（只能重新录入），密钥泄露等于秘密泄露。密钥轮换（`key_version` 升版）与备份策略属后续阶段。
- 代理秘密的到期销毁在服务里由 30 秒一次的扫描器执行；**10 分钟挂钟到期没有在 VM 上实等验证**
  （到期逻辑用短 TTL 在真实 SQLite + 真实 AEAD 上执行过）。服务长时间停机期间不会销毁任何东西，
  下次启动时会先扫一遍。
- 多管理员并发改同一账号只做了顺序化的 409 断言，没有真实并发压测。
- P02.1 的认证链路已在两台 Ubuntu VM 上以非 root 用户跑通 58 项断言；systemd 托管形态在 P01.4
  （Web 单元）与 P02.3（代理单元，含 socket 权限与停止清理）里各有真实证据，但**真实生产服务器上
  从未安装过**。会话空闲/绝对超时只在宿主的可控时钟单元测试里验证，VM 上没有实等。
- 库文件权限（0600）在 Open 与迁移后强制收紧；目录权限依赖部署（StateDirectory）。如果管理员
  手工把库放到一个 group 可读目录并另建副本，本代码管不到。
- 会话限速计数在进程内，服务重启即清零；单实例部署下成立，多实例（当前不支持）会失效。
- 会话 Cookie 带 `Secure` 但 P10 之前走明文回环；Firefox/Safari 是否接受未验证（DECISIONS Q28）。
- 迁移有 0001（settings）与 0002（users/sessions）两条；真实版本升级路径、迁移中崩溃恢复、
  并发写竞争、磁盘满/只读文件系统等场景都还没在 Linux 上验证过（见 P01.2 的 NOT RUN 清单）。
- 触发 panic 会返回契约枚举之外的 `INTERNAL_ERROR`，前端必须把它当未知错误处理（DECISIONS Q17）。
- 面板当前以**明文 HTTP** 监听回环地址；TLS（P10）之前 production 拒绝非回环绑定，
  因此现在不能用浏览器直连 9443 之外的场景做验收。
- 代理的 UID 边界已在代码层落地（P02.3：SO_PEERCRED + `gate`），但**代理的生产动作/查询登记表为空**：
  现在连代理只能拿到 `capabilities` 与明确的 400，没有任何真实系统数据。Web 侧也还没有调用代理的
  业务路径（首页指标属 P04），`internal/agentclient` 只有单测与 httptest 证据，尚未在 VM 上被真实
  业务链路调用过。
- 代理只有 Linux 实现：`peercred_other.go` 在非 Linux 上一律拒绝连接（不假装能确认对端身份），
  因此开发宿主上无法联调代理链路，所有代理验收都在 VM 内完成。
- 执行器的“超时杀进程组”在 VM 内核上验证过直接子进程；若被调程序自己 `setsid` 脱离进程组，
  孙进程不受此约束——当前没有任何生产动作会调用这种程序，登记真实动作时必须逐条确认。
- 代理 socket 一次连接一个身份，连接复用由调用方负责；没有连接数上限或过载保护，
  并发压力与长时间运行的 fd/goroutine 泄漏未测（P02.3 NOT RUN 清单）。
- 验收 VM 已可用（VM-A/VM-B），但 VM-C（LVM 根）与 VM-D（SMB 客户端）尚未创建，
  因此 P04 的 LVM 场景与 P08/P14 的真实客户端验收在该 VM 补齐前不得开始。
- 验收 VM 内未安装 Go/Node 工具链；P01 起若需要 VM 内构建，须先补装并核验版本。
- 前端依赖的大版本组合只在 Chromium 156 无头环境验证过（构建、渲染、路由、错误态、mock 横幅）；
  Firefox/Safari、移动端与真实提示交互（消息/对话框）没有浏览器证据。
- 前端“成功路径”已有真实证据（P01.4：对真实二进制跑 Chromium 冒烟，页面拿到服务端给的
  request_id 并渲染 mock 横幅），但仍只覆盖 `/api/v1/health/live` 一个接口；业务接口的前端
  渲染要等 P02 起逐阶段验证。
- mock 横幅依赖服务端注入 `window.__UPANEL_MODE__`（P01.4 已实现），因此“服务端是 mock，
  页面却显示 production”这类误判只能靠入口页注入保证；浏览器侧仍无法自证（`GET /meta` 属 P02，
  DECISIONS Q21 ③）。
- 生产形态尚未在任何真实服务器上安装过：systemd 单元与安装步骤只在两台验收 VM 里跑过，
  宿主的 Windows 检查结果不能代替 Linux 证据，VM 证据也不能代替真实生产环境验证。
- Windows 宿主上的检查结果不能作为 Ubuntu 可用性的证据；VM 结果也不能代替 P07 之后的
  真实磁盘/文件系统行为验收。
- 便携 QEMU、VM 镜像、cloud-init seed 全部位于仓库之外（`H:\Code\vms`、`H:\Code\.toolchains`），
  不入库；换机需按 TEST_ENVIRONMENT.md 第 8 节复现。
- P04.1 的采集是“5 秒轮询 + 快照”，不是实时：`collected_at` 永远是上一次采集尝试的时间，
  两次采集之间发生的瞬时峰值看不到；`stale` 的门槛是 3 个间隔（15 秒），采集连续失败也不会
  立刻变红，要等快照超过门槛。前端不要把 `stale=false` 读成“数据刚刚生成”。
- 采集段失败只有**假 reader 的单测**覆盖（`/proc/meminfo` 读失败、内容截断、列数不对等），
  两台 VM 上没有制造过真实段失败（去 chmod 真 `/proc` 没有意义也不安全）。所以
  “失败时保留上次有效值 + 旧时间 + `status=error`”这条在真实系统上**未观测**。
- 采集间隔固定 5000 ms，没有配置项；`interval_ms` 只用于让前端解释时间戳，不驱动行为。
  改动间隔需要同时改契约与验收脚本（PLAN §8 写的是 5 秒）。
- 速率只在相邻两次样本之间成立：代理刚启动的 5 秒内所有 `_per_sec` 是 `null`；
  `boot_id` 变化会丢弃 CPU 窗口，但网卡计数器的回退只能靠“变小就报 null”兜住，
  没有按网卡身份重建对称性（换 veth 重名时可能短暂显示 null）。
- 桥接/虚拟网卡（`veth*`、`docker0`、`br-*`）目前**照常列出**，没有过滤也没有分组；
  两台 VM 上只有 `lo` + `ens4`，所以“网卡很多的机器上响应有多大”没有证据。
- CPU 使用率是整机与按逻辑核两种口径，按核数组长度 = 逻辑核数；**没有**按核频率、
  温度、功耗，也没有 cgroup 口径（容器里看宿主数据的问题未处理）。
- `host` 段只有 `os_id`/`os_name`/`kernel_release`/`hostname`/`uptime` 一类只读信息；
  发行版名称取自 `/etc/os-release` 的 `PRETTY_NAME`，没有做版本语义化。
- P04.1 在当轮没有首页（页面属 P04.4）。P04.4 补上之后，取值的证据是 **SSR 渲染真实载荷**
  （`realdata.test.ts`，17 个用例）与**宿主上的 Playwright 冒烟**（mock，已登录）；
  验收 VM 上没有装浏览器，所以“真实数据在真浏览器里渲染”的像素级证据仍然没有。
- 宿主上**没有** `go test -race` 证据（本机无 cgo/gcc）；VM 上的 `-race` 全绿见上一条。
  5 秒 ticker 与 15 分钟健康采集两个 goroutine 的并发读写在本轮 `-race` 下没有报竞争，
  但长时间（小时级）运行的内存/fd 稳定性仍未观测。
- P04.2 的存储拓扑只有**SCSI 盘 + loop** 的真机证据（两台 VM：sda 40 GiB 带分区、sdb 8 GiB、
  sdc/sdd 4 GiB、8 个 loop、fd0）。LVM、LUKS、软 RAID、NVMe、多路径、iSCSI 都没有真机样本：
  这些形态的边与保护递归只用构造的节点/边单测覆盖过。VM-C（LVM 根）仍是未建的缺口。
- 睡眠盘不唤醒（`health.status=sleeping` 时跳过分区表探测）**只有单测覆盖**：验收 VM 上没有
  可休眠的盘（QEMU 的 SCSI 盘没有指望进入 sleep）。真实磁盘休眠时的行为未观测。
- `health` 在 P04.2 一律是 `unknown`、温度与 `checked_at` 是 `null`：这是诚实的占位，不是缺陷。
  （P04.3 已接上真实采集；在没有 SMART 支撑的盘上（QEMU SCSI）仍是 `unknown` 且带理由，
  不是编造 `healthy`。）
- 容量按 30 秒 TTL 缓存（PLAN §8），两次刷新之间可能比拓扑旧；`available_bytes` 取 `f_bavail`，
  所以快照里的可用空间与实时 `statvfs` 之间有采集窗口内的漂移（验收里 `/` 上观察到约 4.5 MB/40 GiB，
  空闲的 loop 镜像上精确相等）。
- `snapshot_version` **不含**容量与时间，只由拓扑内容决定（Q90）：因此“只剩容量变化”不会让版本变。
  写动作预检若要关心容量变化，必须自己比对容量，不能指望版本号。
- `filesystem.uuid`/`label` 依赖 `/dev/disk/by-*` 已由 udev 生成；没有 udev 的精简系统或容器里
  这两项会是 `null`（诚实降级，不猜、不调 `blkid`）。
- 设备身份在读不到 WWN/序列号时只能给 `medium`（两台 QEMU VM 都是如此）：指纹在**同一台机器内**
  稳定且不撞车，但没有验证过“同一块盘换槽位/换机器后仍认得出”，不能据此做去重或迁移判断。
- 分区表探测只读盘头部有限字节，MBR 扩展分区链过深、坏表、非 512/4096 逻辑扇区的盘只会给
  `warning` 或 `source=kernel`，不做修复也不抛错；`free_regions` 只是“探测到的空闲区间”，
  不能直接当作分区建议。
- 存储拓扑在当轮没有浏览器证据。P04.4 之后：真实载荷的 SSR 回归 + 宿主 mock 冒烟的依赖缺失/空状态
  都有；**真实拓扑在真浏览器里渲染**仍无证据（VM 无浏览器）。
- P04.3 的健康采集是**独立 goroutine**（默认 15 分钟），因此 `health.checked_at` 与
  `collected_at` 通常相差几分钟：页面必须用 `checked_at` 讲“健康数据有多旧”，不能拿
  `collected_at` 顶替；代理刚启动时（第一轮采集还没回来）`health` 的 `status` 是 `unknown`、
  `source` 是 `null`。
- 15 分钟是**默认值**，可配 5—60 分钟；配置越界启动即失败。本轮验收只验了“13 秒内不重复拉起
  `smartctl`”与配置校验，**没有**等过一整个 15 分钟周期去看第二次采集（脚本按时间与 `checked_at`
  判断，不按固定秒数）。小时级漂移、`smartctl` 长时间挂住（某些 USB 桥）未观测。
- 真实 SCSI/SATA 上 `smartctl` 的**退出码语义**只用合成输入与 VM 上真实输出覆盖；`-d sat`、
  `-d megaraid,N`、USB 桥接这些需要 `-d` 参数的形态完全没有样本，代码不猜设备类型。
- ATA/SATA 的 `smartctl -j` 真实输出**未验证**：QEMU 的 `-device ide-hd` 在这套环境里会让
  guest 卡在固件阶段，所以 ATA 分支只有夹具与单测。现有 VM 上 4 块 QEMU SCSI 盘都是
  `smart_support.available=false` → 诚实 `unknown`。
- **休眠盘不唤醒没有真机证据**：QEMU 的 SCSI/NVMe 进不了 standby（`smartctl -s standby,now`
  之后 sysfs 仍是 `running`），两段式探测（先看 sysfs、再加 `-n standby`）只有单测覆盖；
  真实磁盘（尤其是 USB 与 SATA 的 power management）是否会因采集被唤醒，未验证。
- 同一个 15 分钟周期里对同一块盘的 `smartctl` 是**单次调用**（`-H -A -i` 一次拿全），不是三次；
  但盘多时仍是串行探测，几十块盘的机器上单轮采集耗时没有测过。
- `io` 的速率只在相邻两次采样之间成立：代理刚启动后的第一个快照 `rate` 是 `null`；
  窗口超过 3 倍采集间隔、任一计数变小（内核重启、换盘、回绕、同名新设备）都给 `null`。
  平均延迟只在对应完成次数 > 0 时给，所以空闲盘是 `null` 而不是 0。
- `io` **不进** `snapshot_version`（Q90 的延伸）：靠版本号判断“有没有变化”的调用方看不到 I/O 变化。
- 设备 I/O 只取**该设备自己那一行** `/proc/diskstats`，分区与整盘、dm 与底层盘从不相加：
  这与很多人对 `iostat` 的直觉不同，是刻意的（相加会重复计数）。
- 网络挂载的容量查询在代理自己的 `--statfs-helper` 子进程里做：并发上限 2、默认 10 秒超时、
  失败后**隔离 5 分钟**。代价是失败后最多 5 分钟拿不到容量（`capacity=null` +
  `capacity_error` 说明还要等多久），换来的是登录与 5 秒扫描不被 `statfs` 拖死
  （VM 上实测：卡住时登录 332 ms / 271 ms）。
- 网络挂载只覆盖**没有块设备**且 `fstype` 属于网络文件系统的挂载点；iSCSI/NVMe-oF 这类
  “有块设备的网络存储”走设备路径，`reachable` 语义不适用于它们（未验证）。
- 网络挂载隔离的真机证据是**停掉 NFS 服务端**制造的（`statfs` 在内核里挂住），不是拔网线、
  也不是服务端直接重启；SMB/CIFS 客户端、只读网络盘、多个网络盘同时卡住都没有样本。
- `network_mounts[].capacity` 与 `reachable` **不进** `snapshot_version`，只有 path/source/fstype/
  read_only 这些身份字段进；写动作预检若要关心网络盘可用空间，必须自己比对。
- USB 盘的证据是**反例**，不是“能读 SMART 的 U 盘”：QEMU 的 `usb-storage` 桥（`0x46f4:0x0001`）
  不在 smartctl 的桥表里，只能证明“桥不认识 → `unknown` + 原话”；真实 U 盘（桥被识别、能读
  SMART 与温度）与 USB-ATA 桥的 SAT 透传都没有样本。
- “同一设备多个挂载点”只有一条真实 `mount --bind` 的 `mountinfo` 行（同为 `8:1`、ext4）；
  同一设备跨不同文件系统类型、NFS 上再 bind 的两级叠加都没验。
- P04.3 当轮没有任何页面：`io`/健康/网络挂载都只有接口与 curl 证据。P04.4 起 `io`、健康、
  不可达网络挂载都已进入页面并有 SSR 真实载荷断言；但真实 `healthy`/`sleeping` 状态的
  浏览器呈现仍只有合成夹具（见 §5 P04.4 的 NOT RUN）。
- P05.1 的服务列表是**10 秒快照**，详情与预检是现场读取：两者可能短暂不一致（列表显示运行中、
  点开详情已是 inactive）。这是设计（PLAN §14 的轮询节奏），但界面没有做“列表与详情对不上”的提示，
  只保证每个动作前都会重新预检一次。
- `truncated=true`（单元数超过 1000）这条路径**只有单测**，真机上最多见到 239 个单元；
  超限后界面写“只显示前 N 个”，但没有分页或按单元名搜索服务端的实现（筛选完全在浏览器里做）。
- 服务页面的浏览器证据是 **SSR 回放**（`realdata-services.test.ts` 拿 15 份真实响应渲染），
  **没有**在真实浏览器里点过确认框、没有观察过任务轮询的帧刷新与错误态切换；
  Playwright 冒烟目前只覆盖到未登录守卫与导航，没有覆盖服务详情页的交互。
- 保护名单把 `systemd-*` 整体计入核心组件，24.04 上因此有 86 个受保护单元（含一些本身无害的）。
  这是 CONTRACTS §10.1 的显式取舍：宁可多保护，也不按名字去猜哪些 `systemd-*` 安全。
- 别名单元不在列表里、请求它一律 404（要求用规范名）：这是契约决定的，但意味着管理员从
  `systemctl` 输出里看到一个别名时，面板不会替他翻译。
- P05.2 的进程列表同样是**5 秒快照**，预检、提交复核与执行前复核全部现场读 `/proc`：列表与预检
  可能短暂对不上（列表里还在的进程，点开预检已经 `PROCESS_NOT_FOUND`）。这是 Q112 的设计取舍，
  但界面**没有**做“列表已经旧了”的提示，只保证每个动作前重新预检。
- 列表上限 2000 条的 `truncated=true` 路径**只有单测**：真机最多见到 139 个进程。超限时界面写
  “共 N 个、只显示前 2000 个”，但没有服务端分页、没有按 PID 区间翻页。
- 进程页面的浏览器证据是 **SSR 回放**（`realdata-processes.test.ts` 拿 22 份真实响应渲染 31 例），
  **没有**在真实浏览器里输入确认短语、点过 KILL、观察过任务轮询与读回阶段；
  Playwright 冒烟仍只覆盖未登录守卫与导航。
- **TERM 超时（进程忽略 SIGTERM）与 KILL 后仍存在（D 状态）没有真机样本**：正常内核上造不出
  可控的“忽略 TERM 且不退出”与“停在不可中断睡眠”的进程，这两条只有 `internal/processes`
  的 fake 覆盖。VM 上的读回窗口（TERM 10 秒 / KILL 5 秒）因此只被“进程会退出”这一侧验证过。
- PID 重用的证据是一台 VM 上用 `/proc/sys/kernel/ns_last_pid` **强制**造出的（pid 5463，
  starttime 30039 → 30049）。自然发生的重用（尤其是高频 fork 的机器上 windows 内连号）没有样本，
  “旧预检 + 新进程”在**同一秒**内发生的竞态只有单测覆盖。
- 保护名单是代码里的固定表，只按进程的 cgroup/单元归属与 PID 判定：**没有被保护的自建服务进程**
  （例如管理员自己 `nohup` 起的东西）可以照常 TERM/KILL，这是预期行为，但界面除了保护类别之外
  不会额外提示“这可能是某人的业务进程”。
- 命令摘要做了脱敏（疑似秘密的参数值替换成 `***`），但脱敏是**模式匹配**：写在 `--opt=value` 里、
  且不像秘密的参数仍会原样显示；通过 `execve` 改掉 argv 的进程显示的是改写后的内容。
- 进程列表与 `process.signal` 都只对 admin 开放（Q115）：viewer 连只读的进程列表都看不到。
  这是有意的边界（列表含属主与命令行），但意味着排障时 viewer 账号无法自助看“谁占着 CPU”。
- P06.2 的写入界面**没有在真实浏览器里点过**：只有 `vitest` 组件级 428 例与
  `check.sh` 的 web-smoke，上传进度、递归删除确认框、改所有者这几处交互的
  真实手感与键盘可达性未验证。
- 上传的**真实 ENOSPC**（写到一半磁盘满）、**24 小时自然过期**、**500 MiB 备份总量上限**、
  **50 000 条目树**、**多客户端真并发提交**、**跨文件系统移动**六条都只有单测或
  近似断言，没有真机现场（原因逐条写在 `docs/evidence/P06/2026-10-21-p06.2/result.md` §5）。
- 编辑备份是**整文件复制**，不是写时复制：改一个 400 MiB 的文件就要再写 400 MiB；
  10 份 / 500 MiB 两级上限意味着“大文件的编辑历史只有一两代”。
- 暂存区与备份区放在被管理根内的 `.upanel`，因此占用该根配额，且对“根被整个 `rm -rf`”
  没有保护（§6 已列为待确认项，P07/P12 之前要定）。
- 并发编辑只做**版本冲突检测**，不做锁：两个管理员同时改同一个文件，后提交的会拿到
  `PREFLIGHT_STALE` 并被要求重来，界面不提供合并。
- `file.chown` 只接受受管理账号表里的名字，不接受数字 uid，也不接受 `root`；
  需要改属主为系统账号的场景目前无解，这是刻意的边界。
- P06.3 的终端页**没有在真实浏览器里敲过**：`vitest` 用例覆盖的是 DTO 与状态机，xterm 的
  渲染、输入法、复制粘贴、移动端软键盘都只有构建产物，没有人工现场；Firefox/Safari 未验证。
- 终端只在 **24.04.5** 上做过真机验收（P06.1/P06.2 同一口径）；22.04.5 上没有任何终端证据。
- 终端会话的上限就是“每人 1 个、全局 2 个、单连接”，因此**没有**做 `/dev/pts` 耗尽、
  多会话并发、代理被 `kill -9` 时浏览器表现的压力与故障注入验证；
  `kill -9` 那条只覆盖了“代理正常关闭连接”的审计（`agent_closed`）。
- 断流期间的输出直接丢弃（实时通道语义），没有回滚缓冲：这是一条写进契约的限制，
  不是缺陷，但用户在网络抖动后会看到输出缺口。
- 终端身份是系统账号 `upanel-shell`：**所有管理员共用同一个系统身份**，因此文件权限层面
  无法区分“哪个面板账号在 shell 里干了什么”（能区分的是审计里的 `terminal.attach/close`）。
  需要按人隔离时要改成每账号一个系统身份，那是新决策，不在 P06 范围内。
- P07.1 **没有执行器**：四个存储动作能预检、能被拒，提交必 501。所以“预检过期 / 目标在两分钟内变了 →
  `PREFLIGHT_STALE`”这条路径**本轮无法在真机上触发**（提交在 `revalidate` 之前就 501），
  也没有任何一次真实写盘、分区、格式化的证据——本轮证明的是“该拒的都拒了”，不是“该做的能做对”。
- 占用检查只扫 `/proc/<pid>/fd`：进程若 `mmap` 了一块设备再关掉 fd，映射还在、fd 却没了，
  这种占用看不见。要补需要解析 `/proc/<pid>/maps`，属 P07.2 执行前复核的加强项。
- 签名只认固定魔数（ext/XFS/Btrfs/swap/LUKS/LVM2/md），认不出来的非零内容报 `unknown` 并阻塞——
  这是 fail-closed 的取舍，不是“能识别所有格式”。exFAT、NTFS 之外的少数格式会落到 `unknown`。
- 设备能力投影有 **30 秒 TTL**：界面上的可点状态可能比真实状态旧最多半分钟；预检判决永远是新鲜的。
  `storage.partition.create` 的能力**不带区间**，`available` 只表示“结构上能做”，不代表任何具体区间合法。
- P07.1 的真机验收只在 **24.04.5**（TCG 客机）上做过；22.04.5 没有任何 P07 证据。
- 本轮**没有** `go test -race`：验收 VM 内没有 Go/cgo 工具链，交叉编译出来的测试二进制不带
  `-race`（P04.3 曾在另一台装了 gcc+Go 的 22.04 VM 上做过全模块 `-race`）。
- RAID（md）成员关系只在单测里用夹具覆盖：验收 VM 没有 mdadm 现场，holders 用 dm 现场（同一条 sysfs
  路径）证明。离线设备、只读设备、应用/共享/备份依赖同理——都要等到有真实现场的阶段。
- 前端本轮**未改**：`StorageDeviceView` 不渲染任何动作按钮（P04.4 就只在“没有能力”时说明一句），
  因此不存在“点了必然 501 的按钮”；写操作的界面要等 P07.2 有执行器时再做。
- P07.2 起三个分区动作**有执行器**，但**前端仍然没有写操作入口**：界面只按 `capabilities` 显示
  “能/不能”，没有初始化、建分区、删分区的按钮与确认流程。完整交互属 P07.5——本轮用 curl 能走完整条链路，
  浏览器里不能，这是明确的边界面而不是漏做。
- 分区表只读**主** GPT（LBA 1—33）：盘尾的备份表不解、不比对。因此“主备不一致”这种损坏形态
  既看不见也报不出来；验收只证明了备份头**存在**。
- 本轮只验了 `sfdisk` 固定模板能写出来的三件事（初始化、建分区、删分区）：**没有**分区改名、改类型、
  调整大小。参数校验覆盖到“起点 1 MiB 对齐、大小 ≥1 MiB、落在空闲区、名字字符集与长度”。
- 写盘过程中的**断电 / 进程被杀**没有验（属 P07.3 的验收点）。本轮只验到“命令返回但任务上下文已结束 →
  报实际状态未知、不猜成功、不自动重试”。
- 并发只有“两个提交打同一块盘”这一种形态（验收 §10），且只跑过有限次数；更多并发、排队上限、
  多块盘同时写都没有压过。
- 分区写动作的真机证据仍然只在 **24.04.5**（TCG 客机）上；22.04.5 没有任何 P07 证据。
- **P08.1/P08.2 的真机证据只在 24.04.5（TCG 客机）上**，22.04.5 没有任何 P08 证据。
- **共享从 P08.2 起真的对外提供服务**：托管 `/etc/samba/upanel.d/upanel.conf` 整份重写、`smb.conf` 末尾
  接一行 include、`testparm` 候选与合并两次、`smbd` 活着才重载。配置真的生效这件事现在是**逐参数读回**
  核验的（`testparm --section-name --parameter-name`），不再依赖“我们写了所以它生效”。
- **`hosts allow` 管不住本机**（Q234，Samba 文档行为）：`allowed_cidrs` 不覆盖 `127.0.0.0/8` 时面板给
  `SHARE_HOSTS_ALLOW_LOOPBACK` 告警，但 `127.0.0.1` 仍然连得上，面板不改写这个行为。真实网段验证用的是
  netns 里的 `10.99.0.2`；**跨机器的真实网络路径仍未验证**（`P00-VM-D` 未建立）。
- **上级目录 `0711` 是新语义**：叶子共享目录 `0700` + `setfacl`，上级目录 `0711`（可穿越、不可列出）。
  历史遗留的 `0700` 上级在下次应用时被修成 `0711` 并在 `observed.path_traversal_repaired` 里点名——
  这是**改动了面板自己先前创建的目录权限**，范围限定在共享挂载点之下，挂载点本身不动。
- **重载粒度是整个 Samba**：`smbcontrol all reload-config` 会重读全部配置，不是“只热更新这一段”。
  单机面板场景可接受，多实例或高并发写入时要换策略（当前靠全局锁 `samba:config` 串行化）。
- **观察命令在 2 vCPU 机器上会被 P05.1 的服务采集器挤到秒级**（Q222）：每 10 秒一轮 `systemctl show`
  在这台 VM 上是 6 秒墙钟 / 3 秒 CPU。P08.1 用“预检 3 秒 / 执行 20 秒”两套预算 + 超时降级成告警
  （Q223）让它不再变成 500 或误杀，但**采集器本身没有改**（降频要动 CONTRACTS 第 690 行的 10 秒契约，
  换采集方式要重做 P05.1 的 DTO 与验收）。任何“在预检里调用外部命令”的新动作都要按同一条规则设计。
- **`internal/storage` 有一个未定位的偶发失败**：`TestOSCapacityReaderNetworkTimeoutQuarantines` 在早前
  一次全量 `go test ./...` 里红过一次，随后多次重跑（含 P08.2 本轮全量）都是 ok。没找到确定原因，
  不排除是测试里对超时窗口的时序假设偏紧。记录在案，不当成“通过”。
- 并发两人同时应用同一共享只验到“锁名里有共享名与卷”，没有造真的并发；两个账号并发创建同理。
- 秘密的**轮换与过期**没有验：本轮只验了“临时秘密 → 持久秘密”的一次转移，没有验过期删除的时间行为
  （那一条由 P02.4 的单测与 `agentstore` 用例覆盖，不是真机）。
- 共享定义的 `protocol` 目前只有 `smb`；`nfs` 在判决层就是 `PROTOCOL_UNSUPPORTED`，没有实现。
- **运行时脱盘只撤不恢复**（P08.3）：卷离线 → 一段共享被摘掉并记下撤销现场；卷回来**不会**自动写回，
  页面提示重新应用一次。这是刻意的：自动写回会把一次外部故障变成一次静默配置变更。
- **`volume_state` 读不出来时按“没挂”处理**（P08.3）：`unknown`、空串、观测失败一律不渲染共享段。
  不对称是刻意的——少一份共享是可见、可重放、可修的；多一份指向系统盘空目录的共享是静默误导。
  **给人看的措辞要分开**（P08.4）：`unmounted`/`missing` 说“卷此刻没挂”，
  `unknown`/`error`/`mismatch` 说“卷状态读不出来”——渲染决定相同，查故障的方向不同。
- **卷的状态不在共享记录里**（P08.4）：页面每轮轮询都取 `/storage/volumes` 并按 `volume_id` join；
  取不到就少显示一行观测值，不把“不知道”画成“没挂”（Q246）。
- **“重新应用”会清空撤销现场**（P08.4/Q247）：撤销只由 30 秒对账产生；人应用一次就换了一版配置，
  旧记录作废。卷要是还在离线，下一轮对账会记一笔新现场。
- **前端回放夹具是某一次运行的快照**（P08.4）：`web/src/fixtures/p08-4-vm/` 里的 13 份文件是从真机原样
  抓回来的，改一个字节就等于把 Q246 那类“自洽的假象”请回来；要更新就重跑验收脚本再取回。
- **开机拉起挂载的是 drop-in，不是 fstab**（P08.3）：面板写的 fstab 条目带 `noauto`，`local-fs.target`
  不会去挂它；`RequiresMountsFor=` 生成的 `.mount` 依赖才是 `smbd` 前置条件。如果手工删掉 drop-in，
  重启后卷不会自己挂上——这是这个设计要付的代价，`docs/STORAGE_RECOVERY.md` 的恢复路径仍然可用。
- **一块坏盘会拖住 `smbd` 的启动**：drop-in 里的路径只要有一个挂不上，systemd 不会启动 `smbd`。
  撤销/禁用/删除最后一条共享会把整份 drop-in 撤走，但“卷坏了而共享还没被撤销”的那段时间属于
  设计内行为（宁可 `smbd` 不起，也不要起在空目录上）。
- **托管配置与 drop-in 的 `0644` 与 umask 无关**（Q243）：写完显式 `Chmod`。历史遗留的 `0600`
  会在下一次重写时被纠正。
- **迁移漏列不会编译失败**（Q240）：给 `shares` 加字段要同时改四处（迁移、`sharesColumns`、
  `SaveShare`、`scanShare`）。这条已写进 `sharestore.go` 的注释里。
- **P10.2 的五条限制**（前三条是“能力晚一拍”，最后一条是账**没还**）：
  ① **`services.unit` 仍会现场问 systemd**：`internal/agent/servicesquery.go:71` 的 `Unit` 闭包是
  活探针，如果它正好撞上 20 秒一轮的服务扫掠，会排队几秒（Q290 实测 6.389 秒）。P10.2 只修了自己
  这一条路径（`nginx.state` 改读快照），`services.unit` 属于 P05 的动作面，**本轮没有动它**——
  谁在服务采集扫掠进行时调 `service.unit`，仍可能等几秒。这是已登记的口子，不是没发现。
  ② **服务快照最多旧 10 秒**（扫掠 20 秒一轮 + 5 秒快照缓存）：reload 的“单元是否 active”判断基于
  快照。若有人在 10 秒内把 `nginx.service` 停了，面板仍会尝试 reload 并拿到失败，落成
  `reload=failed` 而不是 `skipped_service_inactive`——**结论不会错（失败就是失败），只是原因标签
  会不一样**。
  ③ **`nginx -t` 与 reload 之间有缝**：门禁通过后、reload 前，别人改了配置树，reload 会加载别人的
  版本。P10.2 只做“执行前重新判断自己的文件有没有被抢”，不锁整棵配置树——锁别人的配置不属于本阶段。
  ④ **只解析三处配置的假设仍在**（承接 P10.1）：`nginx -t` 是整棵树的，所以门禁比解析可靠；
  但“冲突判断”仍只看 `nginx.conf`/`conf.d`/`sites-enabled`。自定义 include 目录里的 `server_name`
  既不会判成 `taken`，也不会在改名/删除时被发现。
  ⑤ **登录 4–6 秒，账没还**（Q292）：验收机实测 argon2id 单次 1.4–3.9 秒，与 Q23 记的 100–300ms
  差一个数量级。这是安全参数标定，属 P02 范围，**P10.2 明确不改**（改了就是在本轮偷偷放松口令强度）。
  证据在 `docs/evidence/P10/2026-10-23-p10.2/argon2-timing.txt`。
- **Nginx 只解析三处配置**（P10.1）：`nginx.conf`、`conf.d/*.conf`、`sites-enabled/*`，与 nginx 的默认
  `include` 一致，**不跟随其它 include**。如果有人在 `nginx.conf` 里 include 了自定义目录，那里的
  `server_name` 不会被判成 `taken`。P10.2 生成配置时也不能依赖“扫描覆盖了全部 server 块”这个假设。
- **`nginx -v` / `nginx.service` 读不到只降级成 warning**（P10.1）：配置照样读、冲突照样判，但页面上
  版本号为空、单元状态为未知。**不能**把这种情况写成“这台机器没有 Nginx”——那会让人以为可以随便占域名。
- **`0009_proxies.sql` 在提交前被改写过一次**（P10.1）：本轮真机验收之后，表名按 CONTRACTS §14.1
  对齐成 `proxies`（原来是 `proxy_sites`），并改正了两处与实现不符的注释（说标识是 `prx_`、说
  `enabled` 只由 P10.2 置 1）。文件内容变了，校验和也就变了——`store/migrate.go` 会拒绝校验和不符的
  已有库。它尚未在任何长期库上应用过（验收库每轮重建），因此这次改写没有影响；但如果有人在本轮
  中途拷走过一份 v9 的 Web 库，那个库只能重建。
- **`enabled=true` 不代表已生效**（Q280）：P10.1 一个字都没写进 `/etc/nginx`。要让站点真的生效必须
  等 P10.2 的写配置 + `nginx -t` + reload。**P10.2 已交付这条链路**，并且按 Q286 明确：库里
  **没有** `applied_config_hash` 列，`applied` 是**每次读的时候算出来的推导值**（`applied`/`stale`/
  `not_applied`/`unknown` + `reason`），不存任何“上一个结论”。Q281 里“P10.2 再加一列”的说法被
  Q286 推翻，这里保留原话以免悄悄改账。
- **修改 `basic_auth_users` 必须重传 `secret_ref`**（P10.1）：响应里只回账号名，接口拿不到引用；
  只给 `name` 的 patch 会 400（“密码引用非法”）。这是“秘密只进不出”的代价，不是缺陷。
- **P10.3 的六条限制（第 1 条最要紧：它决定了 P12 怎么写）**：
  ① **续期不 reload，让新证书生效靠下一次 `proxy.apply`**。`certificate.renew` 只换盘上的文件
  （Nginx 的 worker 把证书读进内存，换了文件它不重读）；接上这一棒的是 `proxy.apply` 的**证书轮换
  检测**（配置字节没变时用 SNI 问一次 443 是哪张证书，不一致就 reload，Q297）。所以 **P12 的 timer
  必须做两步**：续期 → 对绑定了这张证书的站点各应用一次。只续期不应用，结果会是“任务全绿、
  `last_error` 清空、到期提醒不再响，而浏览器拿到的还是旧证书”。
  ② **只有 HTTP-01，不支持通配符与 DNS-01**（PLAN §13 的决定）：证书只覆盖单域名，且 80 端口必须
  从公网可达。想要 `*.example.com` 得把 DNS 凭据交给面板，0.1 不做。
  ③ **“443 上现在用哪张证书”是测量出来的**：`proxy.apply` 用 SNI 向 `127.0.0.1:443` 握手取叶证书
  指纹。如果这台机器的 443 前面还有一层别的代理，量到的是那一层——那时会报
  `PROXY_TLS_SERVED_STALE`，**这是有意的**：面板确实管不到外面那一层。
  ④ **正式签发与速率限额的真实行为未验证**：验收只连本地 Pebble。1 小时限流是产品自己的一次性
  闸门（查任务表，Q294），Let's Encrypt 那一侧的周限额没有实测，也不该拿真域名去试。
  ⑤ **没有自动续期调度**：本阶段没有 timer、没有后台重试，续期只有“人点”一条路（P12 接 timer）。
  ⑥ **前端没有代理与证书界面**：P10.1—P10.3 都只有后端与 API（与 P10.1/P10.2 同一条口径）。
  STAGES 里也**没有**列过这一项——P12.5 只写了“证书续期 timer、备份与定时任务 UI”。
  所以这一步现在只能靠 API 或 `curl` 走通；若 0.1 需要页面，那是范围变更，按规矩要在 §6 登记并
  得到批准，本轮不擅自加。
- **P11.2 的八条限制**：
  ① **`APT_LOCK_UNREADABLE` 与“读不到 dpkg”只在单测里验**：root 拿 dpkg 的锁文件不会被权限拦
  （`CAP_DAC_OVERRIDE`），把锁文件挪走造 EACCES 会破坏验收机的包管理器；让 `dpkg-query` 失败
  要动 `/var/lib/dpkg`。这两条在脚本里是 NOT RUN，不是 PASS。
  ② **刷新索引的“网络真的断了”没验**：负例用的是不可达的 `file:` 源（路径不存在），
  网络层中断不在本轮。
  ③ **升级只验了单包与包内 conffile**：没有验“一次升多个包”，也没有验“依赖被连带升级”的差集
  语义（连带删除那一侧验的是 `remove`）。
  ④ **重启执行与 `boot_id` 前后确认是 P11.3**，本轮不涉及。
  ⑤ **浏览器渲染没跑**：包写动作的页面（`UpdatesView` 的四个按钮 + `StorageActionDialog`）
  只有类型检查与逻辑单测，没在浏览器里点过；VM 上没有浏览器，每一步的原始响应在
  `payloads/`。
  ⑥ **`StorageActionDialog` 没有组件测试**：它被存储与软件包两处共用，超时与轮询窗口的改动
  由 `web/src/api/preflights.test.ts` 覆盖到函数一级，组件本身只经过 `vue-tsc`。
  ⑦ **升级路径用的是脚本自建的 `file:` 源**（两个 1.0/2.0 的夹具包）：发行版里“恰好有个可升级
  的包”会被上一轮验收消费掉，是运气不是夹具。这一条不是缺陷，但它意味着**没验过真实发行版包
  的升级**——24.04 上真升一次（`zlib1g`）实测超过 6 分钟仍未终态，被排除在自动化之外。
  ⑧ **Windows / macOS 宿主不在 0.1 范围**，这两台上的行为未验证。
- **P11.3 的九条限制**：
  ① **没有浏览器渲染验证**：验收 VM 上没有浏览器，`PowerControlCard` / `PowerCheckPanel` /
  `JobsView` 只有 `vue-tsc`、组件逻辑单测与真机 JSON 回放，没人在浏览器里点过倒计时按钮。
  ② **24.04 的客户机热重启本轮没验到**：QEMU 11.1 + WHPX 在这台宿主上无法完成 reset
  （4/4 挂住，串口停在 `reboot: Restarting system`，stderr `WHPX: Unexpected VP exit code 4`），
  TCG 下 24.04 又开不了机（6.8 内核 `IO-APIC + timer doesn't work!`，cpu 默认/`max`、
  `hpet=on`、smp 1/2 都试过）。那一次 boot_id 变化是真的，但**是宿主在 guest 已到
  `reboot.target` 之后冷启动完成的**——“客户机自己回来”只在 22.04 上验到。环境那一环
  不许读成通过，串口原文在证据里。
  ③ **“电源真的切断”面板证明不了，也不声称**：真机能证明的是“机器回来过且 `boot_id` 变了”；
  24.04 关机后 QEMU 进程消失是**宿主侧**的观察，不是产品能力。所以措辞写的是“只能由现场
  回答”——验收记录也不拿宿主观察冒充产品证据，两边分开写。
  ④ **阶段三必须由人（或宿主脚本）重新上电**：`--phase final` 无法自证，两条 NOT RUN 的
  根源在这里，是被测对象决定的，不是脚本偷懒。
  ⑤ **电源操作只有一把全局锁**：`power-global` 串行化是对的，但只验了“有写操作在跑 → 拒绝”
  与“同一预检只能提交一次”，**没有**验两个管理员同时抢电源时的真实竞争。
  ⑥ **systemd inhibitor 未验**：别的进程（unattended-upgrades、外来的 `shutdown -r`）挡住
  重启时 `systemctl` 会退出非 0，本轮只验了正常路径的 `exit_code=0`。
  ⑦ **倒计时边界只验了校验层**：`delay_seconds` 的 0 / 太短 / 太长 / 未知字段 / 多余参数都被
  拒收，但**没有**真跑一次 1 秒倒计时去看它在真机上的表现。
  ⑧ **“关键任务”的口径是通用的**：判据是“有没有别的写操作在执行”，不是包管理器/磁盘任务
  单独的特权规则；apt 锁、磁盘任务与电源的关系没有单独验。
  ⑨ **socket 守卫只验了单机语义**：活 socket 拒绝启动、陈旧 socket 能清，这两条都有单测；
  但“两个代理同时起、谁赢”只在第 1 轮真机上偶然复现过，没有做成一台机器的专门实验。
  （版本号问题已在第 2 轮消除：二进制自报 `0.0.0-p11.3 (commit 7d4039e)`，代码指纹仍按
  sha256 对：`agent 6ce5726f…` / `web 876ce0a3…` / `ctl 48c360ae…`，两台一致。）
- **P12.1 的九条限制**：
  ① **仓库层有证据，“备份”还没有**：本轮证明的是仓库能建、能读回、口径不假（id 独立核对、
  容量前后两次读数、`repo_version` 如实转述）。**没有跑过一次真备份**，也没有快照、没有还原，
  BACKUP-01 里“备份后恢复”那半句要 P12.2/P12.3 才有对象。
  ② **定时器完全没动**：TIMER-01 本轮没有 systemd timer、没有时间表、没验“到点自己跑”。
  它跟 P12.1 排在同一阶段，但验收行要 P12.4/P12.5 才闭上。
  ③ **“中途断网”不在本轮**：验到的是“连不上 / 指纹不对 / 口令错 → 预检拒绝且原因不编”。
  备份跑到一半链路断掉、restic 自己重连，属于 P12.2 的执行阶段。
  ④ **容量边界靠单测判决**：存储真满、目标路径存在但不是目录，这两条在真机上**没有**制造出来
  （真填满会连 apt/journal 一起弄坏，挂 loop 设备要改 `/etc/fstab`），结论来自
  `capacityVerdict` 的表驱动用例——是逻辑证据，不是环境证据。
  ⑤ **两个 restic 版本不等于“两种行为一致”**：22.04 是 restic 0.12.1、24.04 是 0.16.4，
  两台新建仓库的 `repo_version` 一个是 1、一个是 2。只证明这两台机器上这两条路径成立；
  不声称各版本行为一致，也不声称我们兼容所有版本（版本原文记在 `payloads/*/restic-versions.txt`）。
  ⑥ **只有 agent 侧，没有 Web 侧和 UI**：`/api/v1/backup/*`、备份页面、策略与快照列表
  属 P12.2—P12.5。本轮在真机上能点到的只有 `upanel-ctl` 与 `/run/upanel/agent.sock`。
  ⑦ **没有 `go test -race`**：宿主与两台 VM 都没有启用 cgo（`-race requires cgo`），
  并发相关的结论来自单测的逻辑判定（全局锁、串行化）与真机的串行观察，不是 race detector 给的。
  ⑧ **拨号重试是有限兜底，不是“总能连上”**：网络层失败整轮重来一次（`dialRounds = 2`，
  两轮封顶，最坏 30.25 秒）。指纹不匹配、认证失败**不**重试。这条是取舍（Q334），
  代价是“半开的慢网络”会让人等满两轮才拿到拒绝。
  ⑨ **45 秒预算是按最慢现场定的**：它不能更短，是因为 24.04 在单线程 TCG 下整条只读查询
  实测 30 秒上下（ssh 握手 3—9 秒、每次 `stat` 一个往返、`restic cat config` 5—12 秒）。
  代价写在明面上：真出问题时用户最多等 45 秒才看到“这次没有结论”（Q335）。
- **P12.2 的限制**：
  ① **`stop_managed_apps` 只覆盖两种对象**：systemd 单元与 docker 容器。`compose` 部署
  与“只登记入口”的链接型应用解析不出**一个**可启停对象，因此回 422 `DEPENDENCY_BLOCKED`
  并点名——不降级成“不用停”（那样快照标签会撒谎）。要支持 compose 得先给代理一条
  “停整组”的受控通道，不在本阶段。
  ② **保留策略只存不做**：`retention` 里的 `keep_last/keep_daily/keep_weekly` 记在策略上，
  本阶段**不执行任何删除**；清理是 P12.3 的 `backup.prune`，必须由管理员显式发起。
  现在看到“保留 7 份”只代表这条策略登记了这个意图。
  ③ **`restore_verified_at` 恒为 `null`**：恢复演练不在本阶段，字段存在但没有值——
  “没验证过”与“验证过但时间是空的”是两件事，界面上要分开。
  ④ **快照列表不带大小**：每条要单独跑一次 `restic stats`，列表回 `size_included:false`、
  `size_bytes` 字段缺省（不是 0）。看大小要点进详情；列表一次最多 500 条。
  ⑤ **面板库副本只覆盖面板的 SQLite**：代理自己的库（`agent.db`，含加密后的秘密与镜像）
  不在任何 `source` 里，秘密库的密钥 `agent.key` 更不在——那是灾难恢复文档要求单独保管的东西。
  备份面板库不等于备份了“面板能解开秘密”这件事。
  ⑥ **仓库定义没有 DELETE**：契约里没有这条，面板也不提供。建错的仓库只能改（改位置、改
  口令引用），不能从镜像里退役；要退役得等契约给出语义（删定义时那些快照怎么算）。
  ⑦ **一次运行的上限是硬的**：`restic backup` 6 小时；代理快照查询预算 90 秒，备份专用
  Web 客户端 100 秒，Web 写超时 120 秒（Q347）。超时不会“接着跑”，下一次是一轮新的。
  慢盘上几十 GB 的首次全量如果超 6 小时，该改的是源的范围，不是把上限加大。
  ⑧ **SFTP 与故障注入只覆盖冻结现场**：两台已真验 SFTP 写快照、只断本次 ssh、独立 tmpfs
  写满与运行中取消；不声称覆盖所有网络故障、所有存储后端或任意容量耗尽形态。存储满没有
  填系统盘，旧快照只读核对在任务终态后用 `--no-lock`，不能据此推断并发写时也可无锁操作。
  ⑨ **独立恢复不等于产品恢复**（P12.2 时的状态，**已由 P12.3 解除**，原文保留以便追溯）：
  当时用 restic 独立恢复核对源哈希、面板库完整性和旧快照可读性；`backup.restore` /
  `backup.verify` / `backup.prune` 那时均未实现，留 P12.3。**P12.3 已实现并两面真机验证**
  （两台各 117/0/0，见 §5 的 P12.3 小节）。浏览器 UI 仍未执行，留 P12.5。
  ⑩ **认证散列在慢机器上要几十秒**：24.04 上一次 Argon2id 校验实测 22—30 秒（Q344）。
  这不是 bug，是硬件慢；但它逼着 Web 的写超时必须大于最坏认证耗时（已从 30 秒提到 2 分钟），
  否则慢机器上的正常登录会表现为“连接被断开”。**没有**因此调低 Argon2id 参数——那是单独的
  安全取舍。
- **P12.4 的限制**：
  ① **同一对象只能挂一条计划**：库里的 `UNIQUE(kind, target_id)` 意味着一个备份策略、一个
  脚本或一张证书只能有一条计划。想"每天 2 点全量、每小时增量"这种多时间表，现在表达不了；
  预检会如实拒绝（`schedule-target-taken-blocked`），不是静默覆盖。要不要放开、怎么放开
  （复合键还是计划里放多条日历）得先定语义，留给 P12.5 之后。
  ② **触发记录只追加、没有清理策略**：`schedule_triggers` 设计成不可删（迁移 0010 用
  `BEFORE DELETE … RAISE(ABORT)` 挡住），所以删掉计划也不清历史。好处是"昨晚到底跑没跑"
  删不掉；代价是每分钟触发的计划一年会攒五十万行量级。轮转策略没做，也别在业务代码里
  私自删——这张表的语义就是不许删。
  ③ **面板上没有读触发历史的接口**：审计只能查代理库或 journal。契约没给这条路由，本轮
  也没造一个"看起来像"的列表。
  ④ **"不重叠"的证据边界**：本轮两台各自只捕到一次运行（77 秒，序列 `SF`），能证明"唤醒
  撞上运行中的单元时没有并行"，**不能**证明"排队补跑也会各跑各的"——后者的形状取自 Q387
  定判据时对两台真 journal 的解析（`sequential 79 SFSF` / `sequential 82 SFSF`）。
  ⑤ **余量窗口没有覆盖**：`--wait` 的截止时间早于单元 `TimeoutStartSec`，而操作本身的
  上限又是另一个数；这三个数之间的窗口里会不会叠一次，本轮没有真机样本。不写成"绝不会"。
  ⑥ **时区只验了两个**：`Asia/Shanghai` 与 UTC（差 8 小时整）。DST 切换、半小时时区
  （如 `Asia/Kolkata`）没有真机样本。
  ⑦ **`go test -race` 未执行（NOT RUN）**：宿主 Windows 无 gcc、`-race requires cgo`。
  `schedules.Views` 的并发用例只证明顺序回填与取消传播，不等于竞态检测结论。
  ⑧ **读列表的预算按条数估算**：计划/脚本镜像单独 30 秒（`ScheduleQueryTimeout`），按现场
  0.25 秒/条估约 130 条上限，超过就该分页；本轮真机观察到慢机器上**单条**详情在长任务
  跑着时能读 8.8—9.1 秒，所以并发只压掉"逐条相加"，不保证"一定快"。
  ⑨ **证书续期只做了接线**：§9 只验"接线前就 `blocked` 且原因指向 P12.5"，真实的续期执行、
  失败重试与通知都在 P12.5。
  ⑩ **浏览器 UI 完全没验**：P12.4 没有 UI，留给 P12.5。
- **P12.5 的九条限制**：
  ① **浏览器里对备份页与计划任务页的真实点击没验**：§8 只证明前端产物真的打进了二进制、
  里面带着这些动作名与字段名（`backup.run`、`certificate_renewal`、`requires_reauth`、
  `size_included` 等），§9 只证明页面依赖的读接口活着。渲染、滚动、对话框焦点、提交后的
  界面反馈一律空白。宿主 `check.sh` 的 `web-smoke`（Playwright/Chromium）走的是首页、
  `/storage`、`/storage/devices/sda` 与 404 路由，**不覆盖这两个页面**。
  ② **"certbot 超时但证书已经写下来"那条分支只有单测**：`internal/certificates/certbot.go`
  超时之后先看盘上有没有证书、有就按成功处理，这一轮一次真的超时都没有，所以没有真机样本。
  ③ **生产 Let's Encrypt 一次都没碰**：全程 Pebble + `staging:true`。速率限额、真实 ACME
  账号、真实 DNS 解析都不在证据范围内。
  ④ **续期失败只验到"原因落账"**：这一阶段没有重试与退避策略（每次触发跑一次，失败就等
  下一次触发），也**没有任何通知渠道**（P13.3 才做）。账本上有 `last_error` 不等于有人会
  知道——这一条要等到 P13 才谈得上。
  ⑤ **`wait_quiet` 不是护栏**：24.04 是 TCG 模拟的 2 vCPU，验收脚本跑着的时候 1 分钟负载
  就在 3.3—3.8，四次都没等到 1.5；22.04 是硬件虚拟化，四次都落到 1.43—1.48。阈值是
  "这台机器能回落到多少"的函数，真正的护栏是**宿主在 VM 验收期间不跑重活**（Q398）。
  ⑥ **两台机器的 certbot 差一个大版本（2.9.0 / 1.21.0），但只交叉验了
  `certonly --webroot` 这一条用法**：`--keep-until-expiring`、`--webroot`、`--server`
  三个参数在两个版本上行为一致（含失败原文的形状），别的参数组合没有交叉验。
  ⑦ **`go test -race` 仍未执行（NOT RUN）**：宿主 Windows 无 gcc、`-race requires cgo`，
  与 P12.3/P12.4 同。
  ⑧ **22.04 上的 nginx 是被停掉的**：没有验证"nginx 占着 80 时面板会怎么表现"——那属于
  P10 的范围，本轮把它当成 HTTP-01 夹具的前置条件排除了（Q395 的 `NOT RUN` 退 2 就是为此）。
  ⑨ **同一域名只能有一条续期计划**：建计划时按域名去重（Q388），理由是两条计划会在同一个
  凌晨抢同一把锁。"每天 2 点全量 + 每小时增量"这种多时间表仍然表达不了，与 P12.4 ①同源。

- **P13.1 的七条限制**：
  ① **前端历史图表没验（属 P13.4）**：本子任务只交付 `GET /system/metrics` 与
  `/system/metrics/status` 两条后端端点与三层存储，页面上的曲线渲染、缩放、跨度切换
  一律还没做，因此"跨机断言逐字一致"证明的是接口与库，不是图形。
  ② **"365 天真实老化"没有等真实时间**：验收把三层保留窗口压到 raw 3 分钟 / minute 2 小时 /
  hour 4 小时，验的是**行为等价**（过期就裁、先聚合后裁、聚合窗口=源保留窗口），不是
  一年的数据量。默认配置（24h / 30d / 365d）下三层的实际耗时与磁盘占用没有真机样本。
  ③ **行数上限裁剪（`MaxRows`）真机没跑到**：验收把上限设成 50 万行，一轮验收写不到那个
  量级，所以"超限就丢最老整桶、先 raw 再 minute 再 hour"这条路径只有单测
  （`TestDeleteOldestMetricBuckets`）与代码阅读，没有真机样本。正常部署下它会先撞哪一层，
  现在只能按行数估算。
  ④ **长时间停机的补算没在真机观察过**：验收的缺口是 26 秒，跨小时的补算
  （`PendingMetricBuckets` 200 一批 / 每轮 4000 桶）只有单测。代理停两小时后重启，
  页面上那两小时是空的还是要等补算填上，真机没有样本。
  ⑤ **代理采集慢于轮询时的行为没测**：轮询 2 秒、代理 5 秒采集，"踩点丢快照"是靠去重而不是
  靠错开时间解决的；代理在重负载下采集间隔被拉长时，历史会稀疏而不是错位——这一条推理成立
  但只有单测支撑，没有在把代理压到很慢的真机上验过。
  ⑥ **时区与夏令时不在证据范围**：两台 VM 都是 UTC，时间戳一律 UTC Unix 秒并对齐到桶边界；
  本地时区的展示与"跨夏令时那一小时"的读法属前端（P13.4），未验。
  ⑦ **`go test -race` 仍未执行（NOT RUN）**：宿主 Windows 无 gcc、`-race requires cgo`，
  与 P12.3/P12.4/P12.5 同。

- **P13.2 的六条限制**：
  ① **通知还没有发送方（属 P13.3）**：`alert_events.silenced` 已经落库，但没有任何东西会
  把它发出去。"维护静默只压通知"目前在实现上是"只标记、不发送"，因为发送方还不存在。
  P13.3 接线之前，"告警会通知到人"这句话一句也不成立。
  ② **告警桌面与确认动作没做（属 P13.4）**：规则列表、事件列表、静默设置都只有 HTTP 端点；
  前端页面、历史图表与 `POST /alerts/events/:id/ack` 都在 P13.4，`acked_at`/`acked_by`
  两列已备好但没有任何代码写它们。
  ③ **没有"真把磁盘写满"这类破坏性验收**：超限由受控种入（对不存在的网卡 `accept0` 每 5 秒
  写一个 `rx_bytes_per_sec`）与真实容量阈值驱动，没有做会让共享验收机不可用的破坏性场景。
  因此"空间真的耗尽时面板表现如何"没有真机样本——只有阈值比较这一层的证据。
  ④ **告警的时间尺度被压缩过**：`duration_seconds` 用 30 秒、评估 15 秒、冷却按秒计，验的是
  行为等价（持续够了才响、恢复够了才收、冷却期内不重起算），不是在真机上等一小时。
  默认配置（比如 5 分钟持续、300 秒冷却）下的实际时序没有真机样本。
  ⑤ **多规则并发只到"互不干扰"这一层**：四条规则同时在场（一条主机、两条空间双口径、一条
  静默中的）已经验过，但没有做几十条规则同轮次的压力样本；评估器的顺序与开销只在真机上
  看到过四条的量级。
  ⑥ **`go test -race` 仍未执行（NOT RUN）**：理由同上，与 P12.3—P12.5、P13.1 同。

- **P13.3 的七条限制**：
  ① **没有向任何真实第三方发过信**：收件方与 webhook 都是脚本自带的本地接收器，这是
  STAGES P13.3 划的边界。真实服务商的限速、SPF/DKIM/DMARC 反馈、退信（bounce/DSN）解析都
  没有样本——产品也没有实现退信解析，"信发出去了"只等于"对端 SMTP 回了 250"。
  ② **真实公网回调不在证据范围**：只有本地 HTTPS 接收器与本地 CA。公网 webhook、真实 429/5xx
  的节奏、代理链、对端证书轮换都没有样本。
  ③ **长周期退避只验到第二次尝试**：真机上 `max_attempts=2`/`3`，看到的是 30 秒 → 60 秒；
  `30s × 2^(n-1)` 一路涨到 `maxRetryDelay = 10m` 封顶那条只有单测
  （`TestRetryBackoffAndGiveUp`），没有在真机上等到十分钟量级。
  ④ **投递账的读接口属 P13.4**：CONTRACTS §13.3 明确读路径在 P13.4，本轮断言一律按事实
  直读 `webdata/upanel.db`（只读连接 + `busy_timeout=15s`），**不是**在验接口。
  ⑤ **`retryable`/`accepted`/`detail` 的字段形状只有单测**：它们是代理回给面板的内部回执，
  公开的测试发送接口回的是投递行。"可重试 vs 判死"的效果由真机间接证明（退避重试成功、
  302 与 500 一次判死），字段本身没有真机断言。
  ⑥ **多收件人、大正文、附件没有样本**：SMTP 通道的 `to` 只有一个地址、正文是纯文本，
  `BODY=8BITMIME` 交给 `net/smtp` 自己加；字符集边界（中文以外）与大正文未验。
  ⑦ **`go test -race` 仍未执行（NOT RUN）**：宿主 Windows 无 gcc、`-race requires cgo`，
  与 P12.3—P12.5、P13.1、P13.2 同。
- **P13.4 的五条限制**：
  ① **浏览器里的真实交互未验**：VM 上跑的是二进制，没有 Playwright。历史曲线怎么缩放、悬停
  提示怎么排、告警列表翻页、确认按钮点下去之后的脏状态与回滚，全部由宿主侧
  `vue/server-renderer` 渲染测试与 `web-smoke` 覆盖（DECISIONS Q21），**像素与手感没有证据**。
  ② **"确认之后要不要通知"没有定义**：本轮确认不触发任何投递，`POST .../ack` 只写审计
  （`alert.event.ack`）。契约里没有这条语义，产品也没实现——它到底是"确认后通知值班群"还是
  "确认即静音后续投递"，要等需求明确，现在不做假设。
  ③ **陈旧判据没有在真实业务负载下逼近边界**：本轮用固定 2 秒采集节奏 + 人为停采集 45 秒造
  缺口，`max(3×interval, 20s)` 这条线是**跨过去的**，不是**擦着过**的。采样在负载下抖动到
  判据附近时三态会怎样翻转，没有样本。
  ④ **长时间运行的聚合与裁剪沿用 P13.1 的缩短窗口**：把 raw/minute/hour 的保留窗口压到小时
  量级来跑，真实 24h/30d/365d 的裁剪路径与磁盘占用没有在本轮重验。
  ⑤ **`go test -race` 仍未执行（NOT RUN）**：宿主 Windows 无 gcc，与前面各子任务同。
- **P14.1 的六条限制**：
  ① **`editable=false` 的多数原因分支只在单测里走过**：真机上只走到"存在网桥 `docker0`"这一条
  （24.04）。Wi-Fi、bond、VLAN、多物理网卡、`/run/NetworkManager` 存在这五条要么需要一张无线
  卡，要么需要写 `/etc/netplan`——后者属于 P14.2 的预检场景，P14.1 只读，不许造现场。
  ② **三种非 `netplan-networkd` 的 backend 未在真机出现**（`netplan-networkmanager`、
  `networkmanager`、`other`/`unknown`）：两台 VM 都是 `netplan-networkd`。换渲染器要写
  netplan 配置。
  ③ **`manager=firewalld` / `nftables`(enable) / `iptables-persistent` / `none` 未在真机验证**：
  装第二套防火墙会改变验收机的规则集并影响后续阶段，卸 ufw 会破坏现场。`none` 与
  `unknown` 的区分（Q435）只有单测。
  ④ **`issues.code` 里只有 `timeout` 在真机上自然出现过一次**（24.04 首轮的 `ufw/timeout`，
  见 Q444）。`permission_denied`、`unparsable`、`failed` 要伪造得靠改文件权限或往 sysfs 放
  畸形内容，属于造现场，全部只有单测。
  ⑤ **`ip` 缺失与 `ufw` 未安装时的降级路径只有单测**：真机是标准安装。降级行为本身是"不退化成
  空表、另记一条 issue"，但"页面在只有 sysfs 事实时怎么显示"没有肉眼证据。
  ⑥ **`go test -race` 仍未执行（NOT RUN）**：宿主 Windows 无 gcc，与 P12.3—P13.4 同。
  另：页面（四张卡、能不能改的横幅、`issues` 提示）在 VM 内不可执行——无 Playwright，由宿主侧
  `vue/server-renderer` 渲染测试与 `web-smoke` 覆盖（Q21），**像素与手感没有证据**。
- **P15.1 的六条限制**：
  ① **没有任何真机读数（NOT RUN）**：STAGES §17 的三条验收——「正常镜像可部署/升级」「卷缺失时
  VM 重启也不绕过保护」「健康失败显示失败、业务目录保留」——一条都还没验，因为这一轮只交付
  纯函数层：不执行 `docker compose`、不写单元文件、不落盘。
  ② **渲染出的 YAML 没被真实的 `docker compose config` 读回**：字段名、键序、`mem_limit`/
  `cpus` 的写法是按 Compose spec 写的，`bind.create_host_path` 与长语法 `ports` 也是；正确性
  目前只有“我们写出来的字节长这样”这一条证据，没有“compose 解释成了这个”的证据。P15.2 接线
  时必须先跑一次 `docker compose config` 并把它当作门禁——如果某个键在目标版本上不被接受，
  应该在那里红，而不是在容器起不来时才发现。
  ③ **没有调用方（死代码）**：`upanel-agent` 与 `upanel-web` 都还没 import 这个包，唯一的执行
  路径是 `go test`。按账本口径这是“已实现、未验证”，不是“已交付能力”。P15.2 的预检必须先
  把它接上，否则这一段代码会一直只活在单测里。
  ④ **`source_root_id` 与部署数据根的口径是推出来的，不是现场读出来的**：本包要求挂载来源
  落在 `HostEnv.DataBase` 之内（默认应为受管理卷根 `/srv/upanel/volumes`），并要求根所在的卷
  同时登记在 `volume_dependencies` 里。但**代理侧还没有把“文件根 → 卷”这条映射算出来**——
  `RootFacts.VolumeID` 现在由调用方填，P15.2 要按最长前缀在卷挂载点表上算，这一步没写过。
  ⑤ **路径只做到字符串层**：`..`、绝对路径、`.` 段与格式都在形状层拒了，但**软链接逃逸**
  （根内某个目录指到根外）只有 `openat2` 或同等的句柄级核对能挡。P15.2 在真正 bind mount 之前
  必须核一次文件身份，否则这一条就是纸面上的。
  ⑥ **`go test -race` 仍未执行（NOT RUN）**：宿主 Windows 无 gcc，与 P12.3—P14.6 同。
- **P15.2 前半程的五条限制**：
  ① **两条动作还没登记进代理**：`Service` 与 `Applier` 都只在 `go test` 里被调用过。
  `deployment.apply` / `deployment.remove` 在 CONTRACTS §5 里仍是契约行，不是已登记动作；
  `capabilities` 也不会报告它们。这不是“顺手接一下”的活：需要的宿主事实来源（根 → 卷的最长
  前缀映射、卷的 `Mounted`/`MountedUUID`）与密钥环 `Resolver` 都得先在代理侧构造出来。
  ② **`systemctl` 与 `docker compose` 一次都没真跑过**：`Applier` 的全部行为由 fake runner
  验证（调用序列、退出码、`is-active` 的回答），这证明的是“我们按这个顺序调了、按这个读数判了”，
  不是“systemd 与 compose 真的这么动”。真机上最可能打脸的两处：mount unit 名转义与
  `RequiresMountsFor` 的解析、`Type=oneshot` + `RemainAfterExit` 下 `disable --now` 是否真的
  跑完 `ExecStop`。
  ③ **`compose up` 的内容仍未被 `docker compose config` 读回**（P15.1 的 ② 未消）：单元里
  写的是 `-f <配置>`，配置本身从没被真实 compose 解析过。接线后的第一次真机部署必须先把
  `docker compose config` 当门禁跑一遍。
  ④ **句柄级路径核对仍然没有**（P15.1 的 ⑤ 未消）：`Applier` 写盘前用的是字符串判据；
  bind source 的软链接替换发生在 compose 那一侧，本包看不到。
  ⑤ **`go test -race` 仍未执行（NOT RUN）**，同 ⑥。
- **浏览器放弃请求时的日志分级（Q514）：修了一层，还有一层，且 C13 的文字少写了一层**。1.0 修的是
  会话校验层（`internal/api/auth.go` 的 `clientGone`，取消 → `WARN 客户端已断开，会话校验中止` + 499）。
  未修的是读层，而读层实际是**两个**：代理读（`agentclient` → 503 `AGENT_UNAVAILABLE`，存储/系统信息/
  容器那几条）与库读（`mapAppError` 落到 `internal` → 500 `INTERNAL_ERROR`，`GET /api/v1/apps` 那一条）。
  包内 `KNOWN_LIMITATIONS.md` C13 只写了代理读那一层，最终字节 `3f3b40c1…` 上仍能读到一条 `/api/v1/apps`
  的 500（`docs/evidence/P15/2026-10-28-p15.4/web-requests-run4.txt`，05:40:47.643）。
  **不为这处措辞重打字节**：`BuildDate` 每次构建都变，改一行包内文档就等于换交付字节，按 Q510 得把两台 VM 的
  七相位与页面链路全部重跑；功能与数据不受影响，代价与收益不成比例。更正记在 DECISIONS Q514 与本条，
  下一版重打字节时一并改 C13 的文字。
- **宿主检查要跑在钉住的工具链上（`node 22.23.3` / `go1.27.3`），否则会得到一条"看起来是代码问题"的红**。
  2026-10-28 收尾那次 `sh scripts/check.sh` 只把系统 `node`（v24.15.0）放在 PATH 前面，`node-version`
  就 FAIL（期望 `v22.23.3`），其余 14 项全 PASS；把 `.toolchains/node-v22.23.3-win-x64` 与
  `.toolchains/go/bin` 前置后重跑才全绿（**PASS 15 / FAIL 0 / NOT RUN 0**，读数在 `docs/evidence/P16/2026-10-28-close/`）。工具链不在 PATH 里**不能当通过**，也不该被读成产品缺陷——
  如实记：这一项是环境，不是代码。
  2026-10-29 又在同一台机器上撞了一次，并读全了代价：系统 `node` v24 那一跑是 **13 PASS / 2 FAIL**
  （`node-version` 与 `openapi-lint` 一起红），而 `openapi-lint` 在 node 24 上是**进程退出时**撞
  `Assertion failed: !(handle->flags & UV_HANDLE_CLOSING)` 掉成 exit 127——redocly 本体已经把 spec 判成
  valid，所以这条红跟 `api/openapi.yaml` 无关。**`.toolchains` 在仓库的上一级**（`/h/Code/.toolchains/`），
  不在仓库里，所以 `git status` 不会提示它缺了；换机器 clone 下来要自己按 `.nvmrc` 装。三份读数：
  `docs/evidence/P16/2026-10-28-race/facts-host-check.txt`（13/2 一份，15/0 两份）。
- **装了 docker 不等于部署能用（Q516）**：三条部署写动作在代理**启动时**按 `firstExistingFile(deploy.DockerBinary)`
  决定登记与否，`hostCaps` 却每 10 分钟重探一次（注释还写着"装完一个包不必重启代理"）。于是"面板装好之后才补
  docker"的机器上，能力页会说 docker 有了、部署页照样给新建表单，点"获取预检"才撞 400 `未登记的动作`。
  现场判据：`journalctl -u upanel-agent` 里 `拒绝未登记动作的预检 action=deployment.apply`；处理：`systemctl restart
  upanel-agent`，之后 `已启用 Compose 部署` 出现即正常。这一条本轮**只登记不修**——修它要动二进制、重打包，按 Q510
  得把两台 VM 的七相位与页面链路全部重跑；下一版重打字节时一起改（写动作随重探、hint 补"装完重启代理"、
  `KNOWN_LIMITATIONS.md` 加一条）。证据：`docs/evidence/P16/2026-10-28-p16.2-2404-browser/`。
- **随包文档在仓库里改了、包里的副本没跟着改（同 Q510 的口径，下一版一起发）**：这一轮把
  `docs/SUPPORT_MATRIX.md` 的 24.04 行与部署页那一行换成了最终字节上的读数（155/0/3、35/35、43/43）。
  交付包 `3f3b40c1…` 里那份仍是 2026-10-27 的字（24.04 写成 147/1/3、部署页标"43 条，22.04"）——
  那是**低估**，不是错报，但也确实和仓库不一致。**不为这一处重打字节**：理由与 Q514 的更正相同
  （`BuildDate` 每次注入 → 重打包就得把两台 VM 的七相位与页面链路全部重跑）。下一版打包时一并带上。
- **`go test -race` 不再是"未执行"（本条关掉从 P03 一路记到 P16 的那句 NOT RUN）**。宿主仍然跑不了
  （Windows、`CGO_ENABLED=0`、没有 gcc），但 **24.04 验收 VM 上已经有一份完整的读数**：
  amd64 全模块 `-race`，产品代码**零条数据竞争**。代价和口径一并记在这里——
  ① **它是"一夜"，不是"几分钟"**：全量 3 小时 57 分撞了三个 90 分钟上限，补跑再 9 小时 03 分，
    其中 `internal/api` 一个包就 **8 小时 02 分**；② **不能用 mock 测试的口径去读它**，它证明的是
    "测试进程内实际跑到的那些路径没触发检测器"，不证明"产品没有竞态"；
  ③ 爆出来的问题**全在测试侧**（Q517 ctl 一条永不成立的断言、Q518 硬编码 fd 8、Q519 两处轮询竞争、
    Q520 一处夹具余量），产品**一个字节没改**——这也是为什么这一轮不需要重打交付包；
  ④ 两条红**保留不改**：`TestDefaultPasswordParamsTiming`（判据量的是机器速度——这台模拟机不带
    检测器也要 3.06 秒，上界 1 秒；KVM 上 0.6 秒）与 `TestWebhookSelfSignedCertRejected`
    （本机 TLS 握手被挤爆时才会红，隔离 3/3 过）。**为了让模拟机变绿去放宽上界，就是把验收标准改小。**
  ⑤ 顺带抓到一条**不是缺陷但值得记**的事：空闲的 `upanel-web` 在这台模拟机上吃 **0.86 个核**，
    SIGQUIT 抓栈定位到历史监控的聚合回合（每 2 秒一次 `Pass`；`time.NewTicker` 会把没跑完那轮
    的 tick 丢掉，所以不堆积、不刷日志）。**真机上一个回合占多少 CPU 本轮没有读数**，
    既不能说"产品常态就这么贵"，也不能反过来说"真机上肯定是毫秒级"。
- **界面那条线只在宿主 Chromium 上看过，覆盖面比它看起来窄（Q524—Q533）**。把话说全：
  ① **宿主那 25 张一张都不是真机**——截图是宿主 Playwright 拦截 `**/api/v1/**`、回放真机抓下来的夹具
  画出来的，不是活着的面板；按老规矩**夹具截图不算界面验收**。
  ② **只开了 Chromium**——Firefox / Safari / Edge 一次没开，`backdrop-filter`、`100dvh`、
  `background-attachment: fixed` 这三处最可能在别家不一样。
  ③ **无障碍只有两处手工动作**（启动台焦点移入 + Esc 关闭、图标 `aria-hidden`）：没跑 axe、没有屏幕
  阅读器实测、暗色模式 `--ink-3` 对 `--surface` 的对比度没量。**窗口拖动、缩角、双击铺满全靠手点，
  没有断言，也没有键盘可达性读数。**
  ④ **没碰触摸屏**：400px 只是把窗口缩窄，程序坞 `overflow-x: auto` 的触屏惯性滚动没试；
  **桌面化之后窄屏补截了 2 张**（`/desk`、`/`，400×820，横向溢出 0、`scrollH` 恰 820），
  但**真实数据下的窄屏仍然没看过**。
  ⑤ **25 页看过，但其中 9 页只看到"读不到"**：`/jobs`、`/backup`、`/alerts`、`/schedules`、`/network`、
  `/settings/host`、`/terminal`、`/apps/deployments`、`/accounts` 没有夹具，预览里一律回 404。
  **2026-10-30 补**：这 9 页在真机上点进去了，但这台客机**没装 samba/nfs/certbot、没有任务、没有备份
  仓库、没改过网络**，看到的仍多是空态——**空态对，非空态仍然没人看过**。
  ⑥ **`scrollH` 只量 `documentElement`**：内容窗口是内部滚动容器，页面没有把桌布撑长，
  但窗口**内部**的横向溢出这一轮没有断言。
  ⑦ **"冒烟过了"不等于"界面验过了"**：`check.sh` 的 `web-smoke` 走未登录路径、断言落在登录页，
  15 条里没有一条能对版面变红或变绿；版面是靠人看图看的。
  ⑧ **2026-10-30 起有 7 张真机截图**（真 `/proc`、229 个 systemd 单元、真块设备、真能力缺失态，
  `pageerror` 0），**但它们仍是截图、没有断言**，31 条矩阵一条没动。而且它们只对**重新打的那份字节**
  （amd64 `72997c0f…634a`）有效——`3f3b40c1…daf0` 那份交付物的读数不能被它们继承（Q510）。
