# uPanel 开发进度账本

最后更新：2026-10-20。

当前事实：**P00、P01 已通过**。P01.1 交付 Go 最小可运行骨架
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


## 1. 当前任务

- 当前阶段：P03（预检、任务、幂等、对账）—— **in_progress**（P03.1、P03.2 均已 passed；P03.3—P03.4 未开始）。
- 已完成子任务：P00.1—P00.3、**P01.1**—**P01.4**、**P02.1**（首个管理员、密码哈希、登录、
  退出、会话轮换、CSRF、Origin、限速；无默认密码）、**P02.2**（admin/viewer、账号管理、
  最后管理员保护、再认证、脱敏 DTO、审计事件起步）、**P02.3**（代理 Unix socket + SO_PEERCRED
  UID 边界、固定动作/查询登记表、结构化只读查询、命令执行器超时/输出上限）、**P02.4**（审计事务化、
  日志秘密脱敏、代理独立数据库、AES-256-GCM 秘密存储与到期销毁）、**P03.1**（预检计划由代理生成、
  120 秒期限、用户/会话/动作/规范化参数/状态版本绑定、再认证与确认短语策略、公开预检 DTO）、
  **P03.2**（持久 jobs、`Idempotency-Key` 幂等链、`operation_id` 去重、代理执行记录与事件、
  资源锁与排队上限、取消规则、先落库再提交与按 `operation_id` 对账；两台 VM 各 101 项断言通过，
  VM 内核原生 169 用例全绿）。
- 下一子任务：**P03.3**（SSE 事件流、断线重连、日志限额、任务中心、状态机和错误详情）。
  P03.2 只返回了 `events_url` 地址，流式推送本身还没实现，P03.3 要把它做成真的可用。
- 下游是否可继续：**是**，但任何系统写操作仍需等到 P03（预检/任务/幂等/对账）整体通过之后；
  P03.1/P03.2 只交付了“预检 + 接受并追踪任务”，**生产登记表仍为空，一个真实写动作都没有**。
- 当前版本：未发布。
- 当前无阻塞。遗留（不阻塞）：VM-C（LVM 根，P04 前需要）、VM-D（SMB 客户端，P08/P14 前需要）；
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
| P03 | 预检、任务、幂等、对账 | in_progress | P03.1 **passed**：预检由代理生成（规范化参数、目标身份指纹、状态版本、影响、阻塞项、锁、再认证与短语策略），Web 只保存绑定并返回公开 DTO；有效期 120 秒且两端同一时间戳；用户/会话/动作/`params_hash`/`state_version` 双侧一致；短语只回一次明文、代理库只存哈希；未登记动作 400、缺规划器 501、`GET` 405。证据 `docs/evidence/P03/2026-10-20-p03.1/`（两台 VM 各 93 项断言 + VM 内核原生 5 个测试二进制，P02.1—P02.4 回归各 58/103/84/87 项）。P03.2 **passed**：持久 jobs、幂等链、`operation_id` 去重、代理执行记录与事件、资源锁与 30 秒排队、取消规则、先落库再提交、按 `operation_id` 对账；证据 `docs/evidence/P03/2026-10-20-p03.2/`（两台 VM 各 101 项断言 + VM 内核原生 169 用例 0 FAIL + p01-1/p01-2/p01-4/p02-1—p02-4/p03-1 回归各 29/34/114/58/103/85/87/93；宿主 check.sh PASS 12/0/NOT RUN 0）。验收期间抓到并修掉两个真缺陷：`CONFIRMATION_MISMATCH` 应为 400（DECISIONS Q64）、SQLite WAL DEFERRED 事务升级不调 busy handler → `_txlock=immediate`（DECISIONS Q65）。P03.3—P03.4 未开始（SSE/任务中心/重启对账） |
| P04 | 首页和存储只读 | not_started | 需 VM 真实块设备 |
| P05 | 服务、进程、日志 | not_started | — |
| P06 | 文件和受限终端 | not_started | — |
| P07 | 分区、格式化、挂载 | not_started | 需独立 VM 数据盘 |
| P08 | SMB 与卷依赖 | not_started | 需真实客户端 |
| P09 | 现有 Web 应用与 Docker | not_started | — |
| P10 | 反向代理、HTTPS、0.1 发布 | not_started | — |
| P11 | 软件更新与电源 | not_started | — |
| P12 | 备份恢复与计划任务 | not_started | 需恢复校验 |
| P13 | 历史监控和告警 | not_started | — |
| P14 | 网络、NFS、账号、TOTP | not_started | 需独立网络回滚验收 |
| P15 | 应用部署与更新 | not_started | — |
| P16 | 集成、恢复与 1.0 发布 | not_started | — |

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

**环境事实（不记作产品缺陷）**：24.04 VM 本轮 panic 两次（约 4058 s 与约 528 s uptime，
同一条 `Attempted to kill init! exitcode=0x8b`），重启重跑后全绿；其中一次留下被截断的
`/etc/upanel/upanel.json`（内容为空），三项断言报 `解析配置文件失败 … EOF`，删文件后恢复。
22.04 VM 全程未 panic。

## 6. 未决问题与范围变更

未批准任何范围变更。DECISIONS.md 第 8 节记录 Q1—Q66：

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
| P00-VM-C（LVM 根） | P00/P04 | **NOT RUN**（未创建） | 需在 P04 前补齐 |
| P00-VM-D（SMB 客户端） | P00/P08 | **NOT RUN**（未创建） | 需在 P08 前补齐 |

核心验收矩阵（SEC/JOB/DISK/FILE/TERM/SHARE/APP/PROXY/PKG/BACKUP/TIMER/ALERT/NET/NFS/DEPLOY/RELEASE）
全部尚未开始。其中 JOB-02（预检到期/换参数/目标变化 → 拒绝且不执行）在 P03.2 的提交路径上
**已有宿主测试 + 两台 VM 的真实系统证据**（`p03-2-jobs-acceptance.sh` 的 `== 4`/`== 5` 段：
过期、换参数、他人预检、被阻塞、再认证超窗各自拒绝且不落 job）。JOB 组的其余条目同样是
“机制在 P03.2 验过、真实动作尚未登记”——P03.2 的验收动作是 `acceptance.probe`/`acceptance.ping`，
不是磁盘/服务/网络动作。所以 JOB 矩阵整体仍为 not_started：**验的是机制，不是业务动作**。

## 8. 已知限制

- 已有能力限于：P01 的骨架/持久层/前端/打包部署形态，P02.1 的认证与会话，P02.2 的账号管理与
  审计事件，P02.3 的 Web↔代理通道与执行器边界，P02.4 的审计事务化/日志脱敏/代理独立库与秘密存储，
  P03.1 的预检生成与绑定，P03.2 的持久任务/幂等/去重/资源锁/取消与对账（**任务机制完整，但仍无
  生产动作可执行**）。viewer 目前的“只读”只体现在账号管理与任务查询接口上
  （写操作一律 403）；代理的生产登记表为空，**没有任何业务数据接口**，也没有首页/磁盘/服务/文件页面。
  P03 及之后的高风险能力（磁盘写、共享、网络回滚、备份恢复）只有约定，没有实现，
  更没有 Linux 证据。
- 预检与任务之间已经接上（P03.2：`POST /api/v1/jobs` 消费预检并落持久 job），但**能执行的动作
  仍然只有 `-acceptance-actions` 下的验收动作**：`acceptance.probe`/`acceptance.ping` 只在开关打开
  时登记，生产登记表为空。所以“从预检到执行”这条链路的**代码是对的、动作是假的**——这是
  STAGES 明令 P03 通过前不许写磁盘代码的结果，不是缺陷。真实动作随 P04/P05/P07 逐项登记。
- 任务执行结果目前没有事件推流：`POST /api/v1/jobs` 返回的 `events_url` 是个**还不可用的地址**
  （SSE 属 P03.3）。现在要跟踪进度只能轮询 `GET /api/v1/jobs/{id}`，且该接口只读库、不主动对账
  ——对账发生在 `GetJob`/`CancelJob` 拿到代理响应的路径与 `needs_sync` 行上。
- 状态版本的“执行前复核”在代理侧做了（接受时一次、真正开始执行前再算一次计划），但**没有在 VM 上
  用真实状态漂移验证过**（例如排队等锁期间设备被换掉）——验收动作的状态版本是构造出来的常量，
  没有真实设备可换。宿主测试只覆盖“计划不一致就拒”的分支，VM 验收覆盖的是“计划一致才执行”。
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
