# uPanel 开发日志（P00—P16）

这里保存 uPanel 从 P00 到 P16 的开发过程记录，原文是 0.1 时期的 README 正文。
它是"当时做到了哪一步"的顺序叙述，不是使用说明，也不是当前能力清单——能力与边界以
[SUPPORT_MATRIX.md](SUPPORT_MATRIX.md) 和 [KNOWN_LIMITATIONS.md](KNOWN_LIMITATIONS.md) 为准，
逐阶段的账本、读数与证据见 [PROGRESS.md](PROGRESS.md) 与 [EVIDENCE_INDEX.md](EVIDENCE_INDEX.md)。

保留它的理由：每个阶段的叙述里都写着"为什么这样定"和"当时踩到的坑"（哪条契约改过、
哪个缺陷是 VM 才抓到的），这些在 DECISIONS 的编号条目里是索引，在这里是上下文。

---

Go 侧已有最小可运行骨架
——两个服务入口（`upanel-web` 非 root / `upanel-agent` root 仅 Unix socket）加一个 CLI
（`upanelctl`），带严格 JSON 配置、结构化日志、版本信息、`/api/v1/health/live` 与代理
`capabilities`，Linux 与 mock 实现分离；持久层已有 SQLite（WAL / foreign_keys / busy_timeout
逐项校验）、显式 SQL 迁移与校验和锁定、事务工具、封闭错误码与统一错误信封、request_id；
前端已有 Vue 3 + TypeScript 骨架：路由、Naive UI 中文导航（只显示已实现模块）、统一 API 客户端、
七状态组件、错误组件与不可关闭的 mock 横幅，构建产物在编译期嵌入 `upanel-web`（`//go:embed`），
并带安全响应头、CSP nonce、SPA 回退与“未知 API 恒为 JSON 404”的边界；
`deploy/` 提供 production/mock 配置样例与两个 systemd 单元（Web 非 root / 代理 root），
`scripts/build.sh` 一条命令完成
前端构建 → 产物同步 → Go 二进制构建。**P02.1 起有真实认证**：`users` / `sessions` 两张业务表、
Argon2id 口令哈希、`__Host-` 会话 cookie（只存哈希）、`/auth/login|session|logout`、
CSRF 双提交、Origin 校验、登录失败限速与 `upanelctl init-admin` 唯一的建号入口
（**没有默认密码**）。Cookie 带 `Secure`，所以面板必须经 HTTPS 访问（本机 loopback 明文除外，见
DECISIONS Q28）。**P02.2 起有权限模型与账号管理**：admin 独占 `/users` 增删改查（字段级脱敏 DTO、
`limit`/`cursor` 分页、乐观并发 `version`）、viewer 访问账号管理一律 403；危险改动要求 5 分钟内
密码再认证（`POST /auth/reauth`）、不能自我提权/自我禁用/删除自己、不得移除最后一个可用管理员；
每次改动写入追加式 `audit_events`（数据库触发器禁止 UPDATE/DELETE），口令从不出现在响应、
日志与审计里。**P02.3 起 Web 与特权代理之间有了真实通道**：`/run/upanel/agent.sock`
（`root:upanel-web 0660`、目录 0755）、内核 `SO_PEERCRED` 的 UID 判断（普通接口只认 `upanel-web`，
root 与其他 UID 一律 403）、固定动作/查询登记表（生产表**故意为空**，`capabilities` 如实报
`[]`/`[]`）、结构化只读查询（只认登记过的名字与字段，没有 `command` 字段）、以及命令执行器
（绝对程序路径 + 字符白名单、参数逐字传递、固定环境、超时按进程组杀、输出上限）。
**P02.4 补齐审计、日志与秘密三件事**：审计事件改为跟随业务事务（`ctx` 里有事务就写进同一事务，
业务回滚则审计一起消失，不再存在“业务生效但审计丢失”的窗口）；日志脱敏是**记录器自身**的属性
（按键名片段 + 值形状，`password=…`、`token: …` 这类内嵌文本也会被打码），因此元数据字段用
`key_path`/`key_fingerprint`/`ref` 这类不含敏感词的命名。代理侧有了**独立的 root-only SQLite 库**
`/var/lib/upanel-agent/agent.db`（目录 0700、文件 0600，代理启动时自校验，Web 账号既读不到也列不出）
和**秘密加密存储**：AES-256-GCM、每条秘密独立随机 nonce、AAD 绑定引用与用途、密钥在
`/etc/upanel/agent.key`（`root:root 0600`，`upanel-agent -init-key` 生成且绝不覆盖）；未引用的临时
秘密 10 分钟后销毁（密文与 nonce 清零、留墓碑行），被引用即转持久；**没有读取明文的 HTTP 路由**。
两台 Ubuntu VM 上 P02.4 各 87 项断言通过，并在 VM 内核上原生跑通 AEAD/到期销毁/权限用例。
**P03.1 打开了系统写操作的“前半程”——预检**：`POST /api/v1/operations/preflight`（仅 admin + CSRF）
把动作名与参数连同身份上下文转给代理，由**代理**生成计划——规范化参数、目标身份指纹、
目标状态版本快照、影响、阻塞项、资源锁、是否需要再认证与确认短语；Web 只保存绑定信息
（用户、会话哈希、`params_hash`、`state_version`、代理侧计划引用、到期时间）并返回公开 DTO。
预检有效期 **120 秒**，到期时间由代理签发、两端存同一字符串；确认短语由服务端生成，
只在响应里出现**一次明文**，代理库里只有它的哈希，Web 侧一个字节都不存，也不进日志/审计/journal。
两台 Ubuntu VM 上 P03.1 各 93 项断言通过（含“Web 进程不是 root”“库里搜不到短语明文”
“两端绑定字段逐一相等”），P02.1—P02.4 回归各 58/103/84/87 项仍全绿。
**P03.2 让被接受的任务真的落地并被追踪**：`POST /api/v1/jobs`（仅 admin + CSRF，`Idempotency-Key`
必填）在**一个事务里**核对幂等键、条件消费预检（`consumed_job_id` 一次性）、写入 `job_` 与
`op_` 两个身份、写第一条状态事件，然后才去调代理——先落库再提交，崩在中间也不会出现
“执行了但没记录”。代理侧 `operations`/`operation_events` 只追加、按 `operation_id` 去重
（同一 id 重复提交只回已有记录），后台执行器用 `context.Background` 跑，所以浏览器关掉、
Web 掉线都不影响任务；资源锁按锁名排序获取避免死锁，30 秒排队上限后返回 `RESOURCE_BUSY`。
三类失败严格分开：代理拒单 → `failed` + 契约错误码**不重试**；代理不可达 → 留 `queued` 等对账、
按同一个 `operation_id` 收敛、**不重发**；对账回 `NOT_FOUND` → `failed`/`unconfirmed`。
`progress_percent` 是 `NULL` 就不编进度，`cancellation_requested` 不等于 `canceled`，
不可取消的动作不会给出一个按下去必然失败的取消按钮。
**两台 Ubuntu VM 上 P03.2 验收已通过（24.04.5 与 22.04.5 各 101 项断言 / 0 失败；VM 内核上原生
跑 169 个用例 0 FAIL；宿主 `check.sh` PASS 12 / FAIL 0 / NOT RUN 0）。** 验收抓到两个宿主测试没抓到的
真缺陷并已修复：`CONFIRMATION_MISMATCH` 曾错映射成 409（契约要求 400，DECISIONS Q64）；
SQLite WAL 下 DEFERRED 事务升级失败不排队导致偶发 `SQLITE_BUSY`，改用 `_txlock=immediate`
并加了能拦住回归的测试（DECISIONS Q65）。
**P03.3 让任务“看得见”**：`GET /api/v1/jobs/{id}/events` 是真正的 SSE 流（`id:`/`event:`/`data:`
三行帧，首帧 `: connected`，每 15 秒一帧 `: keepalive`，每次写前设写超时而不是全局 `WriteTimeout`），
支持 `Last-Event-ID` 断线续传；客户端要的位置已经被日志环裁掉时先发 `event: gap`（`seq` 为 0、
不带 id 行，所以不会把水位往回推），前端收到就整体重读而不是猜补。事件的唯一生产者仍是代理，
Web 按 `jobs.agent_event_seq` 水位把代理事件镜像进 `job_events`，**先写事件再写状态**，
客户端不会先看到 `succeeded` 再等 `finished`。日志有容量上限（每任务最近 1000 行，只允许删
`kind='log'`，状态类事件一条不删，DECISIONS Q68），并有 `GET /jobs/{id}/logs` 分页。
前端补上任务中心（列表/详情/状态机/取消/日志尾部/结果 JSON）与**最小登录页 + 路由守卫**——
后者是范围补充：任务中心只有 admin 能用，而 P02.1 只有登录接口没有登录界面（DECISIONS Q72）。
`progress_percent` 是 `NULL` 就显示“未上报”，`cancellation_requested` 仍不等于 `canceled`。
**本轮还修了一个只在 VM 上偶发的真缺陷**：代理把“阶段变更”和“对应事件”分成两笔写，
读方可能看到终态却读不到终态事件；已改为同一事务，并用注入事件写失败的测试证明终态会跟着回滚
（DECISIONS Q67）。
**P03.4 让任务在重启与崩溃之后仍然说得清**：Web 启动时立刻、之后每 30 秒对账一次非终态/待对账
任务（`operations.RunReconciler`），**只查不跑**——对账路径里没有 `CreateOperation` 调用点，
永远不会重发一个可能是格式化的动作（DECISIONS Q73）；提交还在代理调用的路上时，内存里的在途
登记让对账拒绝下任何结论，免得把正在执行的动作写成 `failed`（Q74）；代理说“没这条操作”时按
`needs_sync` 分流——从没同步成功过 → 提交没到达，`failed`/`unconfirmed`；同步过 → 记录丢了、
执行与否未知，`interrupted`/`unknown_result` 并要求先核对系统实际状态（Q75，修订 Q63）；
幂等键到期的清理只删“关联任务已结束”的记录，不制造第二次执行（Q76）；验收动作表与生产表
的隔离从注释变成断言（Q77，`internal/agent/acceptance_test.go`）。本轮**没有前端改动**：
P03.3 的七状态组件已经把它显示为“结果未知（已中断）”，页面也不提供重试入口（接口同样没有）。
**P04.1 开始接真实系统数据，但只读**：代理侧每 5 秒采样一次 `/proc`（CPU 合计/按核、内存与 swap、
负载、网卡计数器）与主机信息（主机名、发行版、内核、架构、boot_id、开机时长），结果是一份内存快照，
`system.summary` 只读这份快照，Web 通过 `GET /api/v1/system/summary` 透传（权限 V，未登录 401，
代理不可用 503 `AGENT_UNAVAILABLE`）。三条硬规则写进了 `internal/monitoring` 的包注释：段级 `status`
说明“最近一次采集有没有成功”，值自带 `collected_at` 说明“这份数是什么时候的”（失败时保留最近有效值
及其旧时间，绝不用 0 冒充），计数器只报**区间**（相邻两次样本、速率没有前一次样本时为 `null` 而不是 0），
`boot_id` 变化会让 CPU 采样窗口失效。字节与计数器在 JSON 里是十进制字符串（CONTRACTS §1，DECISIONS Q82）。
采集全部走文件读取，**不执行任何外部命令**，也就不存在命令注入面。两台 VM 上各 72 项断言通过
（与 `/proc` 逐项比对、时间戳前进、参数夹带被拒、代理停机时登录仍然成功而首页返回 503）。
**仍然没有任何系统写操作**：代理的生产动作表为空（验收动作只在 `-acceptance-actions` 下存在）；
磁盘拓扑与 SMART 属 P04.2/P04.3，前端首页属 P04.4。Ubuntu 22.04/24.04 验收 VM 上骨架、持久层、
打包形态、认证链路、账号管理、代理边界、秘密存储、预检绑定、任务提交、对账与系统采集均有实跑证据
（见 [开发进度账本](PROGRESS.md) 与 `docs/evidence/`）。
规格基准日期：2026-10-20。

P00 建立的基线文件（P01.1 起开始叠加代码）：

- [实施决策记录](DECISIONS.md)：三个执行身份的边界、范围、技术栈、路径、未决问题。
- [依赖核实与锁定](DEPENDENCIES.md)：Go 1.27.3 / Node 22.23.3、Go 模块与 npm 精确版本。
- [验收测试环境](TEST_ENVIRONMENT.md)：VM 规格、虚拟数据盘序列号与隔离要求、
  已创建环境的复现步骤（QEMU 11.1.0 + WHPX，无管理员权限）。
- [公开契约](api/openapi.yaml)：信封、错误码、安全方案；只登记已实现并通过验收的路径。
- [本地启动与安装](LOCAL_START.md)：构建、本地运行、前端开发、systemd 安装与故障排查。
- `deploy/`：production/mock 配置样例与 systemd 单元模板（`deploy/systemd/`：`upanel-web.service`、
  `upanel-agent.service`）。
- `scripts/check.sh`：统一检查；未具备的检查输出 `NOT RUN`，不作为通过。
- `tests/fixtures/`、`docs/evidence/`：夹具与证据约定。

本地检查：

    # 需要 Go 1.27.3 与 Node 22.23.3（见 .nvmrc）
    npm --prefix web ci --ignore-scripts
    sh scripts/check.sh

宿主上若 PATH 里的 Node 不是 22.23.3（例如另有独立安装的 v24），先把它前置到 PATH，
否则 `check.sh` 会按设计 FAIL 在 `node-version`（见 DECISIONS Q14）。

前端（P01.3 / P01.4）：

    npm --prefix web run dev          # Vite 开发服务器，5173；不做跨域代理，需自行指向后端
    npm --prefix web run typecheck    # vue-tsc，含 .vue 模板类型
    npm --prefix web run lint         # ESLint 10（只覆盖 .ts；依赖里没有 eslint-plugin-vue）
    npm --prefix web run test         # vitest（50 个用例，node 环境 + SSR 断言）
    npm --prefix web run build        # 产物在 web/dist，由 scripts/build.sh 同步并嵌入 Go 二进制
    npm --prefix web run smoke        # 真实 Chromium 冒烟，16 项（需 npx playwright install chromium）
    npm --prefix web run smoke:mock   # 以 VITE_UPANEL_MODE=mock 重新构建并检查横幅，17 项
    node web/scripts/smoke.mjs --base http://127.0.0.1:18881 --expect mock
                                      # 直接对运行中的二进制做冒烟，19 项

构建、运行、安装与排障见 [本地启动与安装](LOCAL_START.md)：

    sh scripts/build.sh               # 构建前端 → 同步 internal/webui/dist → 构建三个二进制到 bin/
    bin/upanel-web --mode mock --db "$PWD/bin/dev-mock.db"   # 页面 + 同源 API，127.0.0.1:18881

mock 横幅说明：面板处于 mock 模式时，页面顶部**始终**显示“演示模式：数据为演示数据，未操作服务器”，
没有关闭按钮。模式来源优先级为 `window.__UPANEL_MODE__`（P01.4 起由服务端在入口页注入）→
`VITE_UPANEL_MODE` → `production`；P02 提供 `GET /meta` 后改为以服务端应答为准（DECISIONS Q21）。

构建与运行（开发用，非部署形态；完整步骤见 [docs/LOCAL_START.md](LOCAL_START.md)）：

    sh scripts/build.sh                                       # 前端构建 + 嵌入 + 三个二进制
    go build -o bin/upanel-web ./cmd/upanel-web               # 单独构建（需先同步过前端产物）
    go build -o bin/upanel-agent ./cmd/upanel-agent
    go build -o bin/upanelctl ./cmd/upanelctl

    # P01.2 起数据库是硬依赖：非 Linux 没有默认路径，必须显式 --db
    bin/upanelctl migrate --db "$PWD/bin/upanel.db"          # 幂等；失败以非 0 退出
    bin/upanel-web --mode mock --db "$PWD/bin/upanel.db"     # 只绑 127.0.0.1:18881，演示数据
    bin/upanel-web --mode production --db /var/lib/upanel/upanel.db
                                                            # 仅 Linux；默认 127.0.0.1:9443，明文 HTTP
    bin/upanelctl version
    bin/upanelctl check-config --config /etc/upanel/upanel.json   # 含 database 段

    # P02.1 起账号只由本机 CLI 创建，没有默认密码，也没有 --password 参数（密码两次输入不回显）
    bin/upanelctl init-admin --config /etc/upanel/upanel.json --username admin --display-name "管理员"
    sh tests/smoke/p02-1-auth-smoke.sh    # 认证链路冒烟：临时库/临时端口，38 项断言，无需浏览器
    sh tests/smoke/p02-2-account-smoke.sh # 账号管理/再认证/审计冒烟：81 项断言，无需浏览器

生产安装（Ubuntu + systemd）：Web 二进制装到 `/usr/local/bin`、配置 `/etc/upanel/upanel.json`、
数据库 `/var/lib/upanel/upanel.db`，服务用户 `upanel-web` 非 root 无 sudo，单元模板
`deploy/systemd/upanel-web.service`；特权代理装到 `/usr/local/lib/upanel/upanel-agent`，以 root 运行、
只监听 `/run/upanel/agent.sock`（`root:upanel-web 0660`），单元模板 `deploy/systemd/upanel-agent.service`。
步骤与故障排查见 [docs/LOCAL_START.md](LOCAL_START.md) 第 6、7 节。

Linux 验收脚本（在 Ubuntu VM 内运行，不需要 VM 内装 Go/Node）：

    BIN_DIR=/tmp DEPLOY_DIR=/tmp/deploy sh tests/integration/p01-4-acceptance.sh
                                                            # 114 项断言：嵌入/头/404 边界/systemd
    BIN_DIR=/tmp sh tests/integration/p01-1-acceptance.sh   # 29 项断言
    BIN_DIR=/tmp sh tests/integration/p01-2-acceptance.sh   # 34 项断言（迁移/PRAGMA/启动拒绝）
    BIN_DIR=/tmp/upanel-bin sh tests/integration/p02-1-acceptance.sh
                                                            # 58 项断言（认证链路，非 root 运行）
    BIN_DIR=/tmp/upanel-bin sh tests/integration/p02-2-acceptance.sh
                                                            # 103 项断言（账号管理/再认证/审计，非 root）
    sudo BIN_DIR=/tmp/upanel-bin sh tests/integration/p02-3-agent-acceptance.sh
                                                            # 85 项断言（代理 socket/UID 边界/登记表/
                                                            # 执行器；root 运行，含 systemd 真实托管）
    sudo BIN_DIR=/tmp/upanel-bin sh tests/integration/p02-4-agent-secrets-acceptance.sh
                                                            # 87 项断言（密钥生命周期/独立代理库/密文落盘/
                                                            # 到期销毁/无明文读取；root 运行）
    sudo BIN_DIR=/tmp/upanel-bin sh tests/integration/p03-1-preflight-acceptance.sh
                                                            # 93 项断言（预检生成/120 秒期限/两端绑定一致/
                                                            # 短语只回一次明文/拒绝路径不落库；root 运行）
    sudo BIN_DIR=/tmp/upanel-bin sh tests/integration/p03-2-jobs-acceptance.sh
                                                            # 101 项断言（0—11 共 12 个段落 + 汇总：提交/幂等/
                                                            # 预检一次性/Web 被杀重启/代理 kill -9 → interrupted/
                                                            # 取消规则；ubuntu 24.04.5 与 22.04.5 均已通过）
    sh tests/integration/p01-1-host-refusal.sh              # 宿主侧拒绝与契约核验，16 项断言

---

## 交付给开发模型的文件

请将整个目录交给开发模型，按以下顺序阅读：

1. [产品与技术开发计划](PLAN.md)：目标、范围、架构、各模块行为和安全边界。
2. [接口与数据契约](CONTRACTS.md)：统一数据类型、API、状态机、数据库和特权代理协议。
3. [阶段任务与验收](STAGES.md)：P00—P16 的实施顺序、产出、测试及停止条件。
4. [DeepSeek 执行说明](DEEPSEEK_HANDOFF.md)：可直接复制的首轮提示词与续接规则。
5. [开发进度账本](PROGRESS.md)：跨对话保存进度、证据、限制和下一步。

推荐一次只让开发模型实施一个阶段。通过该阶段验收后，再进入下一阶段。

三个交付层次：

- 0.1 核心单机版：P00—P10 完成，具备受保护的磁盘操作、SMB、现有应用管理、反向代理等核心能力。
- 1.0 完整单机版：P00—P16 完成，补齐更新、备份、告警、网络、NFS、用户与受约束的应用部署。
- 后续扩展：多服务器、LVM/RAID 写操作、磁盘加密、分区缩容等，不属于上述版本。

“完整单机版”不等于没有边界。各模块明确列出的限制属于产品定义，界面需要如实展示。

## 文档维护规则

- PLAN.md 决定产品范围、技术栈和不可突破的边界。
- CONTRACTS.md 是接口、字段、状态与数据表的唯一契约来源。
- STAGES.md 决定实施次序和验收条件。
- DEEPSEEK_HANDOFF.md 决定开发模型的执行方式。
- PROGRESS.md 记录事实，不得用计划内容代替已完成内容。
- 如果文档之间发生实质冲突，登记阻塞项并指出冲突位置，不得自行扩大权限或降低保护要求。

不要仅发送聊天摘要给开发模型。至少提供上述五份文档，或者让模型直接读取此仓库。
