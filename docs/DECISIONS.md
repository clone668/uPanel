# uPanel 实施决策记录

本文件是 P00.1 的产出：把 PLAN.md / CONTRACTS.md / STAGES.md 中已经冻结、不允许实施时再讨论的
内容固定下来，并记录实施环境中真实存在的未决问题。

维护规则：

- 本文件只记录**决策与事实**，不记录计划中的功能。功能范围以 PLAN.md 为准。
- 与规格冲突的内容不得写进这里“顺手改掉”；必须按第 8 节登记冲突。
- 环境事实（工具链、版本、缺失条件）必须写实际核实的值，见 DEPENDENCIES.md 与 TEST_ENVIRONMENT.md。
- 新增决策追加在对应分节末尾，标注日期；不覆盖历史条目。

日期：2026-10-20。依据文档版本：PLAN 1.0、CONTRACTS 1.0、STAGES（P00—P16）。

---

## 1. 三个执行身份（不可合并）

| 身份 | 运行内容 | 权限边界 | 关键约束 |
|---|---|---|---|
| `upanel-web` | Web 进程（认证、权限、元数据、任务编排、页面） | 无 sudo、不加入 `docker` 组、无登录 shell | 非 root 常驻；只监听管理端口；只能通过 `/run/upanel/agent.sock` 请求特权动作；永远不能直接执行系统命令或直连 Docker |
| `upanel-agent` | 特权代理，root，独立 systemd 服务 | root，但只做白名单动作 | 只监听 Unix socket；核验 `SO_PEERCRED` 实际 UID；重新校验参数、设备身份、路径与锁；不提供 `execute(command)` / 任意 argv / 任意文件写入 |
| `upanel-shell` | 受限交互终端的 shell 身份 | 无 sudo、无特权组、独立 home | PTY 在启动 shell **之前**降权到该 UID/GID 并清空环境；Web 进程不能通过该通道获得 root |

补充事实：

- socket 权限 `root:upanel-web 0660`，`/run/upanel/agent.sock`。
- `POST /internal/v1/schedules/:id/trigger` 只接受 UID=0 的本机 CLI；普通路由只接受 `upanel-web`。
- 面板账号与 Linux/Samba 账号是两套身份，不共用密码数据库。
- 终端默认 15 分钟无输入关闭、最长 1 小时、每个管理员 1 个会话、全局 2 个；0.1 不做 tmux 式持久终端。
- 三种身份互相之间的调用方向是单向的：浏览器 → Web → 代理；终端不经过 Web 的 root 能力。

## 2. 版本范围

### 2.1 0.1 核心单机版（P00—P10 全通过后交付）

登录与权限、审计、任务中心、首页、基础监控、磁盘监控、分区创建/删除、GPT 初始化、
ext4/XFS 格式化、受管理卷挂载/卸载、SMB、服务/进程/日志、文件管理、受限终端、
现有应用接入、现有容器生命周期、域名反向代理与 HTTPS。

### 2.2 1.0 完整单机版（P11—P16）

软件更新与电源、备份恢复与计划任务、历史监控与告警、受约束的网络变更、NFS、
系统账号管理、受约束的应用部署与更新、完整安装升级恢复流程。

### 2.3 明确不做（不得用“高级模式”偷偷引入）

多服务器/集群/K8s/虚拟机管理/应用商店；LVM/mdraid/硬件 RAID/ZFS/Btrfs/LUKS 的写操作（可只读识别）；
调整已有分区大小、文件系统缩容、整盘 mkfs、安全擦除；跨发行版升级、内核调优、自动 fsck；
任意 root 终端或 root 命令、任意 Compose YAML 在线运行；任意系统配置文件在线编辑；
任意现有网络结构自动迁移；数据库一致性备份的自动承诺、任意应用升级回滚、第三方 SSO。

## 3. 技术栈冻结

| 层次 | 选择 | 备注 |
|---|---|---|
| 后端 | Go + Gin + database/sql + modernc.org/sqlite | 显式 SQL 迁移，不用 ORM |
| 前端 | Vue 3 + TypeScript strict + Vite + Vue Router + Pinia + Naive UI | 单一 UI 框架、单一路由方案 |
| 图表/终端 | Apache ECharts、xterm.js（`@xterm/xterm`） | PC 主要目标，手机看状态 |
| 数据库 | SQLite WAL，`foreign_keys=ON`，`busy_timeout` | 数据库只放本机文件系统 |
| 实时 | 轮询 + SSE（任务/日志）；仅交互终端用 WebSocket（gorilla/websocket） | 不引入消息队列 |
| 特权 | 独立 `upanel-agent` + Unix socket + 固定动作白名单 | 不提供万能执行器 |
| 部署 | 两个 Go 二进制 + 前端嵌入 Web 二进制 + systemd | 生产机不装 Node.js |
| 反向代理/证书 | Nginx（只管理 `upanel-*.conf`）+ Certbot HTTP-01 / 手动导入 | 0.1 只支持单域名单上游 |
| 共享 | Samba；P14 引入 nfs-kernel-server | — |
| 测试 | Go testing、Vitest、Playwright、Ubuntu 隔离 VM 集成 | mock 通过 ≠ Linux 可用 |

禁止：Python/Node 后端、微服务、Redis、PostgreSQL、K8s、第二套 UI 框架或 ORM、两套路由。
具体依赖版本见 `docs/DEPENDENCIES.md`（P00 核实并锁定）。

## 4. 固定路径

| 用途 | 路径 | 属主/权限 |
|---|---|---|
| 代理 socket | `/run/upanel/agent.sock` | `root:upanel-web` 0660 |
| Web 数据库 | `/var/lib/upanel/upanel.db` | `upanel-web`，本服务可写子目录 |
| 代理数据 | `/var/lib/upanel-agent/`（`agent.db`、操作记录、配置备份） | root 独占 |
| 代理密钥 | `/etc/upanel/agent.key` | `root:root` 0600，Web 不可读 |
| 系统配置 | `/etc/upanel/` | root 所有；Web 只读必要的非秘密配置 |
| Web 程序 | `/usr/local/lib/upanel/upanel-web` | root 所有，Web 用户不可修改 |
| 代理程序 | `/usr/local/lib/upanel/upanel-agent` | root 所有 |
| 管理卷挂载点 | `/srv/upanel/volumes/<slug>` | 只按 UUID 写入 fstab 管理块 |
| 应用目录 | `/srv/upanel/apps/<slug>` | 与卷通过依赖绑定 |
| 终端 home | `/var/lib/upanel-shell` | `upanel-shell` 所有 |
| 证据 | `docs/evidence/<阶段>/<日期>/` | 不含秘密与大文件 |

仓库布局按 PLAN.md 第 22 节：`cmd/upanel-web`、`cmd/upanel-agent`、`cmd/upanelctl`、
`internal/{api,auth,store,jobs,agentclient,agent,platform,storage,apps,monitoring,config}`、
`web/`、`api/openapi.yaml`、`migrations/`、`agent-migrations/`、`deploy/{systemd,scripts}`、
`tests/{fixtures,integration}`、`docs/`。

## 5. 不可突破的边界

1. Web 进程不得以 root 运行；不得 API 暴露原始 root shell、任意命令、任意 argv、任意文件写入。
2. 所有系统写操作必须走：读取状态 → 预检 → 展示影响 → 确认 → 持久任务 → 代理复核 → 执行 → 读取真实结果 → 审计。
3. 预检绑定用户/会话/动作/规范化参数/目标指纹，120 秒有效，一次性；执行前状态变化必须拒绝。
4. 提交用 `Idempotency-Key`，`operation_id` 唯一约束；重复提交不得造成二次执行。
5. 破坏性动作（GPT 初始化、格式化、删除分区、递归删除、覆盖恢复、仓库清理）需要服务端生成的确认短语。
6. 磁盘目标只用不透明 `device_id`；执行时重新解析并核对 WWN / by-id / 序列号 / 容量 / major:minor；
   身份不足或冲突的设备只读。
7. 系统盘保护沿 `partition_of` / `depends_on` 递归追踪；`/`、`/boot`、`/boot/efi`、`/usr`、`/var`、
   面板自身文件系统所在的整块盘都受保护；活动 swap、未知 holders、LVM/RAID/crypt 关系禁止写。
8. 同一物理盘上的操作持排他锁；多个资源按规范字符串排序加锁。
9. 崩溃/重启后不重放非幂等操作；无法确认结果标 `interrupted`，不显示为成功。
10. 所有破坏性磁盘/网络验收只在独立 Ubuntu VM 的可丢弃虚拟数据盘执行；开发宿主与日常服务器不得作为目标。
11. 未实现模块隐藏或返回 `NOT_IMPLEMENTED`，不返回模拟成功；mock 模式只绑定 `127.0.0.1:18881` 且页面持续标识“演示数据”。
12. viewer 只看脱敏状态/监控/应用入口/任务摘要；原始日志、文件、配置、终端、秘密、系统写操作一律 403。

## 6. 契约约定（与 CONTRACTS.md 第 1 节一致）

- 前缀 `/api/v1`，JSON 字段统一 snake_case，前端 DTO 不做隐式驼峰转换。
- 响应信封：`{"data":...,"request_id":"..."}`；列表 `{"data":{"items":[],"next_cursor":null},"request_id":"..."}`；
  错误 `{"error":{"code","message","details"},"request_id":"..."}`；响应头带 `X-Request-ID`。
- 时间 RFC 3339 UTC；未知值为 `null`，不用 `""`、`0`、`"unknown"` 代替。
- 大计数器（字节、扇区）是非负十进制字符串；前端用 BigInt 或格式化工具，不用 Number 做精确运算。
- 百分比 0—100；比率 `_per_sec`，时延 `_ms`，温度 `_celsius`；不可用的温度是 `null`，不是 0。
- 列表默认 `limit=50`、最大 200，`cursor` 不透明，排序稳定。
- 异步写任务创建返回 **202**，不是业务成功；只有真实结果被验证后任务才 `succeeded`。
- 错误码枚举固定在 `api/openapi.yaml` 的 `ErrorCode`，新增必须先改契约。
- `api/openapi.yaml` 只登记**已实现**的接口。P00 的 `paths` 为空是刻意的。

## 7. 工程与阶段纪律

- 文档优先级：PLAN（范围/栈/边界）> CONTRACTS（接口/字段/状态）> STAGES（顺序/验收）> PROGRESS（事实）。
- 一次只做一个子任务；前置验收未通过不得进入下一阶段；`P03` 未通过不得开始磁盘写代码。
- 未执行的检查一律写 `NOT RUN` 并给出补做方法；mock 通过、编译通过、页面可点开都不等于 Linux 验收。
- 禁止为让测试通过而删除测试、放宽校验、关闭 CSRF、忽略错误、硬编码成功。
- 每个已通过子任务保留最小可验收提交；不提交秘密、VM 镜像、磁盘镜像或内存转储。

## 8. 未决问题与冲突

| 编号 | 日期 | 类型 | 内容 | 状态 / 处理 |
|---|---|---|---|---|
| Q1 | 2026-10-20 | 待定 | Go module 路径暂定 `upanel`（无外部导入需求）。如将来需要 canonical import path，须在 P01 写入业务导入前一次性确定。 | 已决策（暂定），不阻塞 |
| Q2 | 2026-10-20 | 环境事实 | 开发宿主为 Windows + Git Bash；系统 Go 为 1.25.6（低于冻结的 1.27.3），系统 Node 为 24.15.0（冻结为 22.23.3）。 | 已处理：Go 通过 `GOTOOLCHAIN` 自动获取 1.27.3 并实测；Node 22.23.3 已下载到仓库之外使用。`scripts/check.sh` 对 Node 版本不符直接 FAIL。见 DEPENDENCIES.md |
| Q3 | 2026-10-20 | ~~阻塞（P00.3 验收）~~ → **已解决（2026-10-20）** | 本机没有可用的 hypervisor，也没有获得安装虚拟化软件的外部授权，无法创建 Ubuntu 22.04/24.04 验收 VM 与可丢弃虚拟数据盘。 | **已解决**：宿主已启用 Windows Hypervisor Platform，改用**用户级便携 QEMU 11.1.0 + `-accel whpx`**（`H:\Code\.toolchains\qemu`，无管理员权限、无系统级安装、删除目录即可回滚）。VM-A（22.04.5）/VM-B（24.04.5）已创建，2 vCPU / 4096 MB / 40 GiB 系统盘 + 三块数据盘，序列号 `UPANEL-SYS-40G`、`UPANEL-DATA-A-8G`、`UPANEL-DATA-B-4G`、`UPANEL-DATA-C-4G` 在 VM 内实读一致，宿主物理磁盘未透传。证据：`docs/evidence/P00/2026-10-20/vm-2404-verification.txt`、`vm-2204-verification.txt`、`host-isolation.txt`；复现步骤见 TEST_ENVIRONMENT.md 第 8 节。**遗留**：VM-C（LVM 根）与 VM-D（SMB 客户端）未创建，P04/P08/P14 前补齐 |
| Q8 | 2026-10-20 | 环境事实 | 验收 VM 由 cloud-init NoCloud **经 HTTP** 注入配置（`-smbios ...ds=nocloud;s=http://10.0.2.2:PORT/`），因为宿主无管理员权限、无法可靠生成 CIDATA ISO。 | 已决策：seed 由宿主回环端口上的只读白名单 HTTP 服务提供，仅监听 `127.0.0.1`；VM 网络为 QEMU user NAT，宿主侧只做 `hostfwd=tcp:127.0.0.1:<port>-:22`。VM 内 `upanel` 用户拥有 NOPASSWD sudo 用于验收操作，**与产品权限边界无关**，`upanel-web` 等产品身份仍按 PLAN 禁止 sudo |
| Q9 | 2026-10-20 | 未验证 | 验收 VM 内未安装 Go/Node 工具链（仅基础系统）。 | 未验证：P00 只要求磁盘身份与隔离，已完成；P01 起若需 VM 内构建，补装并核验到与 DEPENDENCIES.md 完全一致后才可标记 |
| Q4 | 2026-10-20 | 依赖约束 | `typescript@7.0.2` 与 `typescript-eslint@8.72.0` 的 peer 范围 `>=4.8.4 <6.1.0` 冲突。 | 已决策：冻结 `typescript@5.9.3`。升级 TS 前必须确认 lint 工具链支持范围 |
| Q5 | 2026-10-20 | 未验证 → **部分验证（2026-10-20，P01.3）** | `vue-router@5.4.0`、`pinia@4.1.0`、`vite@8.3.4`、`vitest@5.0.5`、`@xterm/xterm@6.0.0` 等大版本的运行时兼容性，只按 registry 元数据与 peer 范围核实，未真正构建过应用（P00 无应用代码）。 | **部分验证**：P01.3 用真实工具链跑通 vue-tsc typecheck、eslint、vitest（50 用例）、vite build 与 Chromium 156 无头渲染；`@xterm/xterm@6.0.0` 尚未被任何代码引入，其兼容性仍属 P06 验证范围。Firefox/Safari 未验证 |
| Q6 | 2026-10-20 | 环境事实 | 仓库尚未初始化 Git，CI 工作流未在任何远端执行过。 | 事实记录：`.github/workflows/ci.yml` 为待执行状态，见 PROGRESS.md |
| Q7 | 2026-10-20 | 环境事实 | Playwright 浏览器未安装（`npm ci --ignore-scripts`），P16 前的 E2E 需要显式 `npx playwright install` 与独立步骤。 | **已解决（P01.3）**：执行 `npx playwright install chromium` 安装匹配 `@playwright/test@1.64.1` 的 chromium 1248，`npm --prefix web run smoke` 可运行。浏览器二进制仍在仓库之外（`%LOCALAPPDATA%\ms-playwright`），换机需重装；`scripts/check.sh` 不含冒烟槽位，冒烟是显式步骤 |

| Q10 | 2026-10-20 | 实施决策 | 配置文件格式与容错策略（P01.1）。 | 已决策：JSON；`json.Decoder` 开启 `DisallowUnknownFields`，并拒绝首个对象之后的任何内容（防止“改了两个对象只生效一个”）。`--config` 显式指定的路径不存在 → 退出码 2；Linux 默认路径 `/etc/upanel/upanel.json` 不存在 → 按内置默认值运行（首次安装不该要求先写配置文件）。非 Linux 无默认路径，避免开发宿主读到宿主机的系统配置 |
| Q11 | 2026-10-20 | 实施决策 | mock 模式的端口约束（P01.1）。 | 已决策：mock 只能说 `127.0.0.1:18881`。显式配置/传入其他地址（含 `0.0.0.0:18881`）→ 退出码 2，不做“自动纠正”。副作用：同一份配置文件里写了 `listen_address: 127.0.0.1:9443` 时不能再加 `--mode mock`，必须另写一份 mock 配置；这是有意的，避免演示数据随配置漂移到网络上。验收脚本已按此拆成两份配置 |
| Q12 | 2026-10-20 | 实施决策 | `web/go.mod` 的性质（P01.1）。 | 已决策：它是**构建边界**，不是依赖清单。npm 依赖里可能夹带 Go 源码（例如 flatted 的 golang 实现），若 `web/` 不构成独立模块，根模块的 `go build ./...` 会递归扫到 `web/node_modules`。该文件不参与任何构建，仓库中也没有对应的 `web/*.go` 代码 |
| Q13 | 2026-10-20 | 实施决策 + 未验证 | TLS 尚未接入（P01.1）。PLAN.md 第 6 节要求面板自身监听 HTTPS 9443，TLS 属 P10。 | 已决策：TLS 就绪前，production 绑定非回环地址**直接拒绝启动**（退出码 2），不接受“先明文跑起来”的临时状态；mock 已由 Q11 限制在回环。**未验证**：证书生成、TLS 握手、Nginx 前置与 9443 的实际 HTTPS 行为全部属于 P10，本阶段没有任何 TLS 证据 |
| Q14 | 2026-10-20 | 环境事实 | 开发宿主同时存在两个 Node：`C:\Program Files
odejs
ode.exe`（v24.15.0，独立安装）与 nvm-windows 的 `v22.23.3`。`nvm use` 无法接管前者（目录是真实目录、非符号链接，改它需要管理员权限）。 | 已处理：本轮 `nvm install 22.23.3`，并在跑 `scripts/check.sh` 时把 nvm 版本目录前置到 PATH，PASS 8 / FAIL 0。未前置时 check.sh 如实 FAIL（node-version），说明闸门有效。CI 不受影响（setup-node 读 .nvmrc）。**注意**：任何在宿主上跑前端命令的人都必须先前置 PATH，否则会用到 v24 |
| Q15 | 2026-10-20 | 实施决策 | P01.1 引入的第三方 Go 依赖为零。 | 已决策：骨架、配置、日志、HTTP 信封、版本、Unix socket 全部用标准库实现；Gin 与 SQLite 只在 P01.2 真正需要时按 DEPENDENCIES.md 的精确版本一次性加入，避免先引入再删的噪声 |
| Q16 | 2026-10-20 | 实施决策 | 数据库路径与 busy_timeout 的配置方式（P01.2）。 | 已决策：新增 `database` 段（`path`、`busy_timeout_ms`，默认 5000，范围 100—60000）。Linux 默认 `/var/lib/upanel/upanel.db`；非 Linux 无默认值，必须 `--db` 显式指定，避免开发宿主静默写系统目录。`path` 必须是 POSIX 绝对路径且不含 `?`/`#`（DSN 分隔符歧义）。**数据目录不由代码创建**：目录归属与权限属于安装步骤（PLAN.md 第 4 节），缺目录即拒绝启动，不“顺手 mkdir” |
| Q17 | 2026-10-20 | 契约补充 | 程序故障（panic、响应序列化失败）必须返回某种错误，但 CONTRACTS.md 的 24 码枚举里没有合适的码。 | 已决策：返回 500 + `INTERNAL_ERROR`，并明确这是枚举外的唯一例外：业务逻辑永不主动返回它，堆栈只进日志、绝不进响应体；前端按未知错误处理并提示重试。已在 `api/openapi.yaml` 的 ErrorCode 描述里写明，不擅自把 `INTERNAL_ERROR` 加进契约枚举（那会变成未批准的范围变更） |
| Q18 | 2026-10-20 | 契约补充 | CONTRACTS.md 第 1 节列出的状态码不含 405 与 500，但 HTTP 语义需要它们。 | 已决策：405 返回 `VALIDATION_ERROR` 并带 `Allow` 头；500 见 Q17。两者都不改契约枚举，也不把“方法不支持”含糊成 404（那会让客户端把接线错误当资源不存在）。P01 验收要求的“未知 API 不返回 200 或页面”由 JSON 404 保证 |
| Q19 | 2026-10-20 | 依赖事实 | P01.2 `go mod tidy` 解析出的传递依赖 `golang.org/x/crypto v0.48.0`、`golang.org/x/sys v0.48.0` 低于 DEPENDENCIES.md 冻结表写的 v0.58.0 / v0.49.0。 | 事实记录 + 已决策：当前没有代码直接 import 这两个模块，它们只是 Gin/SQLite 的传递依赖，按 MVS 取所需最小版本。**不写 `replace`、不手改 go.sum、不把版本凑成表里的数字**。首次直接 import（Argon2id、openat2、SO_PEERCRED）时用 `go get <module>@<冻结版本>` 抬上去并重新验收。冻结表本身不改（它描述的是直接依赖的目标版本），差异在 DEPENDENCIES §2 显式列出 |
| Q20 | 2026-10-20 | 实施决策 | 是否让特权代理也用 Gin（P01.2）。 | 已决策：不用。Gin 只服务 Web 公开 API（路由分组、中间件、404/405 语义）；`upanel-agent` 继续用标准库 `net/http` + Unix socket，权限边界更高的一侧不引入额外框架与它的传递依赖面 |
| Q21 | 2026-10-20 | 实施决策 + 未验证 | 前端测试环境与运行模式来源（P01.3）。冻结依赖清单里只有 `vitest`，没有 jsdom / happy-dom / eslint-plugin-vue；同时 CONTRACTS 第 3 节要求 mock 模式“持续显示演示数据”，但当时没有任何接口能告诉前端服务端处于哪种模式。 | 已决策（三条）：① 组件测试用 `vue/server-renderer` 做 SSR 字符串断言，vitest 环境固定为 `node`，**不新增 DOM 环境依赖**；覆盖模板分支、props 与文案，不覆盖点击/焦点/动画。② `.vue` 文件不经过 ESLint（无插件），SFC 类型与模板错误由 `vue-tsc` 覆盖、渲染分支由 SSR 断言覆盖；需要 `.vue` 规则时按新增依赖流程单独决策。③ 模式来源优先级 `window.__UPANEL_MODE__`（P01.4 嵌入时注入）→ `VITE_UPANEL_MODE`（开发/冒烟）→ `production`。**未验证/已知缺口**：没有公开接口返回服务端模式（`GET /meta` 属 P02，需登录），所以“服务端是 mock、前端误判为 production”在 P01.3 无法自动发现；P02 落地 `/meta` 时补上，并让 mock 横幅以服务端应答为准 |
| Q22 | 2026-10-20 | 实施决策 | 前端产物与部署形态（P01.4）。涉及三个取舍：缺前端产物时的启动策略、静态资源缓存、CSP 的 `style-src`。 | 已决策：① 二进制未嵌入前端资源时**不拒绝启动**，启动打 WARN、页面请求返回 503 并提示执行 `scripts/build.sh`；理由是这样仍可只调 API 排查，而且 503 不会把缺资源伪装成正常页面。② `index.html` 用 `Cache-Control: no-store`（入口页含服务端注入的运行模式，缓存会导致 mock/production 串台）；`/assets/<name>-<hash>.<ext>` 用 `immutable` 长缓存，其余静态文件 1 小时。③ CSP 的 `script-src` 只允许 `self` 与本次响应的 nonce，但 `style-src` 保留 `'unsafe-inline'`——Naive UI 在运行时注入 `<style>`，不换组件库无法用 nonce 覆盖。④ 代理 systemd 单元推迟到 P02.3：`/run/upanel/agent.sock` 的属主与 0660 权限属于 P02.3 的交付内容，先写单元等于先编造权限模型 |

| Q23 | 2026-10-20 | 实施决策 + 实测 | Argon2id 参数取值（P02.1）。CONTRACTS.md 第 2 节要求“参数以实施环境基准设定并记录，目标校验约 100—300 ms”。 | 已决策并实测：`m=131072 KiB (128 MiB), t=3, p=2, salt=16B, key=32B`，单次校验在开发宿主（M3 Air / Windows）实测 **98.7—131.2 ms**（同机对比：64 MiB/t=3 ≈ 44.6 ms 偏快，64 MiB/t=5 ≈ 74.1 ms，128 MiB/t=3 ≈ 109.5 ms，128 MiB/t=4 ≈ 131.2 ms）。选 128 MiB/t=3：落在 100—300 ms 区间中段，真实服务器通常更慢只会更慢不会失效。参数随哈希存于 PHC 字符串（`$argon2id$v=19$m=131072,t=3,p=2$salt$hash`），改默认值不需要迁移旧哈希，登录成功时按 `NeedsRehash` 就地升级。解析哈希时对 m/t/p/salt/key 设防御上限（1 GiB / 64 / 64 / 64B / 128B），防止被篡改的哈希串让进程申请巨量内存。证据：`docs/evidence/P02/2026-10-20-p02.1/argon2-timing.txt` |
| Q24 | 2026-10-20 | 契约补充 | 登录限速（429）用什么错误码：CONTRACTS.md 的 24 码枚举里没有“限速”专用码。 | 已决策（与 Q18 同一思路）：状态码 429 表达“被限速”，响应体 `error.code` 用 `VALIDATION_ERROR`，并在 `error.details.retry_after_seconds` 给出建议等待秒数。不改契约枚举、不生造新码。限速是**进程内**固定窗口计数（用户名 5 次/15 分钟、来源 IP 20 次/15 分钟），不落库：单实例部署下足够，且天然满足“不做永久锁定”——服务重启即清零。成功登录只清用户名维度、**不清**来源维度，否则一个已知正确账号就能把按 IP 的限速重置成摆设 |
| Q25 | 2026-10-20 | 契约补充 | CONTRACTS.md 要求 `GET /auth/session` 返回 `csrf_token`，同时要求“服务端只保存哈希”。 | 已决策：`GET /auth/session` 每次调用都**轮换** CSRF 令牌（生成新令牌、存新哈希、返回明文），因此明文只在响应里出现一次，库里始终只有哈希。副作用：轮换后旧令牌立即失效，前端必须在启动/刷新后先取 session 再发写请求；正在进行中的写请求可能一次性收到 403 CSRF_FAILED（重试即可）。`reauth_expires_at` 从未再认证时返回 `null`（契约要求空值用 null，不用空串） |
| Q26 | 2026-10-20 | 实施决策 | `web.allowed_origins` 的默认值（P02.1）：契约要求“Origin 白名单来自本机配置，不由任意 Host 或 X-Forwarded-* 生成”。 | 已决策：配置项留空时，**仅当最终监听地址是回环地址**才从监听地址推导源（`http://127.0.0.1:<port>`、`http://localhost:<port>`，监听 `::1` 时追加 `http://[::1]:<port>`）；监听非回环地址且未显式配置 → 启动失败（退出码 2），不给“猜一个源”的机会。跨字段规则放在 `config.ResolveAllowedOrigins`，`upanel-web` 与 `upanelctl check-config` 共用同一判定，避免两处结论不一致。每一项都会规范化（scheme/host 小写、默认端口去掉、拒绝路径/查询/userinfo），比较是逐字相等，不做通配 |
| Q27 | 2026-10-20 | 实施决策 | 用户名形态（P02.1）。 | 已决策：用户名统一小写（`NormalizeUsername`），正则 `^[a-z][a-z0-9._-]{2,31}$`，数据库列用 `COLLATE NOCASE UNIQUE`（双保险：即使绕过 Go 侧校验，`ADMIN` 也插不进去）。登录时同样先归一化再比对，所以输入 `Admin` 能登录 `admin`。显示名（`display_name`）是独立字段，允许中文，最长 64 |
| Q28 | 2026-10-20 | 未验证 / 已知限制 | 会话 Cookie 带 `Secure`，但 P10 之前面板在回环上走明文 HTTP。 | 事实记录：`__Host-` 前缀强制要求 `Secure`，本实现照契约保留；Chrome/Edge 把 `http://127.0.0.1` 与 `http://localhost` 视为可信上下文，因此本地能用。**未验证**：Firefox/Safari 在明文回环下是否接受该 Cookie，以及 P10 接入 TLS 后的完整行为。若某个浏览器拒收，症状是登录返回 200 但后续请求 401 —— 属于已知限制，不是权限缺陷；P10 后消失 |
| Q29 | 2026-10-20 | 实施决策 | `upanelctl init-admin` 如何取密码（不允许默认密码、不得进日志）。 | 已决策：不提供 `--password` 参数（会进 shell 历史与进程列表）。终端下用 `x/term` 两次无回显输入；非终端（脚本/管道）读**两行**且必须一致——只给一行视为失败，避免把误触的回车当成密码。已存在任何账号时先拒绝（且在提示输入密码之前就拒绝）。输出只含用户名、角色、数据库路径与 schema 版本，从不输出密码或其哈希 |
| Q30 | 2026-10-20 | 实施决策 + 缺陷修复 | 数据库文件权限：SQLite 按 umask 建库，VM 上实测为 0644，同机其他用户能读到口令与会话哈希。 | 已决策：`store.Open` 在打开并校验 PRAGMA 之后，把库文件与存在的 `-wal`/`-shm` 收到 0600；迁移结束再收紧一次，因为 WAL/SHM 可能到写库时才出现。只收紧、不放开，已经 0600 的不动，附属文件不存在即跳过；非 POSIX 平台 chmod 不控制权限位，函数不因此报错也不假装生效（该断言在 Linux VM 上验，Windows 上 SKIP）。目录权限仍由部署负责（systemd `StateDirectory`，P02.3） |

规格之间未发现实质冲突。若发现，按 PROGRESS.md 第 6 节格式登记冲突位置（文件 + 条款），并停止依赖该条款的实施。
| Q31 | 2026-10-20 | 实施决策 | 账号管理接口的状态码与错误码怎么映射（契约只给了状态码语义，没指定用哪个 code）。 | 已决策：400 `VALIDATION_ERROR`（JSON 语法/未知字段/字段格式/缺 version/非法 limit 与 cursor）；403 `FORBIDDEN`（非 admin、自我提权或自我禁用、删除自己、再认证口令错误）；403 `REAUTH_REQUIRED`（写操作缺再认证窗口）；403 `CSRF_FAILED`（缺/错 CSRF）；404 `NOT_FOUND`；409 `VERSION_CONFLICT`（只用于乐观并发）；422 `VALIDATION_ERROR`（用户名占用、会失去最后一个可用管理员）。422 复用 `VALIDATION_ERROR` 是因为错误码枚举是封闭的（CONTRACTS §1），不能为两处业务规则新增枚举值；前端按 HTTP 状态码区分“输入错”与“业务条件不允许”，`message` 给出具体原因 |
| Q32 | 2026-10-20 | 实施决策 | 再认证窗口存在哪里、多久、怎么限速。 | 已决策：窗口写在会话行（`sessions.reauth_until`），不在内存、不做独立 token，因此重启后仍准确，退出/改密/删号随会话一起消失。窗口 5 分钟（PLAN.md 第 98 行“最近五分钟内完成密码再认证”）。`POST /auth/reauth` 只对 admin 开放，校验口令后把 `reauth_until = now + 5m` 并返回 `reauth_expires_at`；失败与登录共用同一套限速计数（按 `user:` 与 `ip:` 双维度），所以反复用错口令既拿不到窗口，也会被 429 拦住。口令错误的响应是 403 而不是 401：会话本身仍然有效，只是没拿到再认证授权。`totp` 字段在 P14 之前提交即 400，不做“假装验证” |
| Q33 | 2026-10-20 | 实施决策 | “不得删除最后一个可用管理员”放在哪一层，覆盖哪些操作。 | 已决策：在 `auth` 服务层、与写操作同一个事务内计数（`SELECT COUNT(*) FROM users WHERE role='admin' AND disabled=0`），降级、禁用、删除三类操作都检查；同时禁止管理员改动自己的角色与启用状态（`ErrSelfModification` → 403），这既是防自我提权也是防自我锁死。注意接口层实际先被“自我修改”拦住：actor 必然是可用管理员，所以让系统失去最后管理员的 422 分支在单会话模型下走不到，它是给 CLI/后续多身份路径的防御性不变式，由服务层单测直接覆盖（不靠接口凑用例）。禁用一个 admin 只有在还存在另一个可用管理员时才允许 |
| Q34 | 2026-10-20 | 实施决策 | 审计表结构、追加式保证，以及审计失败时业务怎么办。 | 已决策：`audit_events` 只追加，`UPDATE`/`DELETE` 由 SQLite 触发器 `RAISE(ABORT)` 拒绝（应用层也没有任何改/删接口，两道保险）；`actor_id` 故意不建外键——账号被删后审计必须仍然保留，`actor_snapshot` 冗余记录当时身份。`redacted_details` 写入前按键名黑名单（password/passwd/secret/token/csrf/hash…）替换为 `[redacted]`，这是兜底，不是主要防线（主要防线是调用点根本不传秘密，口令类字段一律不进 details）。审计写入失败不回滚已经提交的业务变更，但必须打 ERROR 日志并在日志里写明“业务变更已生效”；P02.4 会把审计与业务放进同一事务，届时这条限制消失 |
| Q35 | 2026-10-20 | 实施决策 | 账号列表的分页形态与游标编码。 | 已决策：`GET /users` 返回 `{"items":[…],"next_cursor":…}`（CONTRACTS §1 列表统一形态），`next_cursor` 无下一页时显式 `null`，不省略字段。`limit` 默认 50、上限 200，越界或非数字一律 400。游标是 `base64url("user:"+上一个用户名)`，按用户名升序稳定翻页；不加密但不可解析依赖，客户端不得解读。游标解码失败或缺少前缀返回 400 `VALIDATION_ERROR`，不当作第一页静默重来（否则翻页会悄悄重复数据） |
| Q36 | 2026-10-20 | 缺陷修复 | 冒烟脚本抓到的真实缺陷：`POST /api/v1/users` 注册路由时漏了 `requireCSRF`，管理员会话 + 正确 Origin 就能不带 CSRF 令牌创建账号（违反“所有非 GET/HEAD 请求必须带 CSRF”）。 | 已修复：路由补上 `s.requireCSRF()`，并新增 `TestUserWritesRequireCSRF`（创建/修改/改密/删除四条路径都断言 403 `CSRF_FAILED`）。修复前该请求返回 422（走到业务层用户名冲突），修复后返回 403。这个缺陷是宿主冒烟脚本发现的，不是单测发现的——记在这里提醒：权限检查要靠“按契约逐条断言”的脚本兜，不能只靠功能路径单测 |

| Q37 | 2026-10-20 | 实施决策 + 未验证 | 代理的生产动作/查询登记表里到底放什么（P02.3）。STAGES.md P03.4 已定过“测试用无害动作由独立测试登记表提供，不放入生产白名单”，但 P02.3 本身要交付“固定动作注册”。 | 已决策：`agent.NewProductionRegistry()` 返回**空表**，`GET /internal/v1/capabilities` 如实报告 `actions: []`、`queries: []`（新增 `supported_queries` 字段与 `supported_actions` 并列），启动日志记录 `registered_actions=0`、`registered_queries=0`。理由：P02.3 的交付物是**机制与边界**，不是采集内容；把还没有真实实现的查询塞进白名单，等于用“能返回点什么”冒充功能完成。登记机制本身由 `registry_test` / `queries_test` 用测试专用登记表覆盖（`test.echo`），生产表仍空。**未验证**：真实只读查询（磁盘/服务/进程）逐项随 P04/P05 登记并单独验收 |
| Q38 | 2026-10-20 | 实施决策 | 代理的 UID 边界：root 能不能调普通接口？其他同组用户能不能？ | 已决策：**普通接口只允许配置里的 `web_user`（默认 `upanel-web`）**，UID 来自内核 `SO_PEERCRED`（`Accept` 时读取，一条连接一个身份，不可伪造也不可随请求头改变）。root 调用普通接口返回 **403 `FORBIDDEN`**，root 只能走专用路由，而专用路由表 `rootOnlyPrefixes` 当前为**空**（P03 登记 schedule trigger 后才非空）——所以现在 root 到处被拒。同组但非 Web 账号的 UID 同样 403，且响应**不回显允许的 UID**（否则等于给攻击者一份探测清单）。文件系统层另有一道：socket `0660` 意味着无关用户连连接都建立不起来（验收里 `nobody` 拿到 000）。两条断言分别覆盖：`upanel-probe`（同组）证明 403 来自 SO_PEERCRED，`nobody` 证明文件权限也生效 |
| Q39 | 2026-10-20 | 实施决策 + 缺陷修复 | `/run/upanel` 的目录权限（P02.3）。首版 systemd 单元只写了 `RuntimeDirectory=upanel`，VM 实测目录为 `0750 root:root`，Web 账号无法穿越，所有调用变成 HTTP 000。 | 已决策：目录固定 **0755 root:root**——可穿越、不可写（Web 账号能走到 socket，但不能在目录里新建或替换文件），socket 本身仍是 `0660 root:upanel-web`。并且代理**启动时自检**（`verifySocketDir`）：目录必须 root 属主、组与其他位不可写、并且能被 socket 组或其他人穿越，否则**拒绝启动**。这样“目录被收紧到打不开”会变成一条明确的启动失败，而不是运行期一片 000。副作用：任何把 `/run/upanel` 改成 0700 的加固都会让代理拒绝启动——这是有意的，因为那种加固之后 Web 根本用不了这个 socket |
| Q40 | 2026-10-20 | 缺陷修复 | 命令执行器把命令字符串当成了程序路径：POSIX 下 `"/bin/sh; rm -rf /tmp/x"` 满足 `filepath.IsAbs`，会走到 exec 阶段，报错语义完全错（“文件不存在”而不是“这不是一个程序”）。 | 已修复：新增 `ErrProgramInvalid` 与 `validateProgram()`，在 `IsAbs` **之前**按字符白名单校验（只允许 `[A-Za-z0-9/._\-+:@~\]`），命中其他字符即拒绝，错误信息写明“程序路径只能是一个文件名，不能是命令字符串”。新增 `TestRunRejectsCommandStrings` 覆盖 `;`、空格、换行、`$( )`、反引号、`|`、`&&`、引号八种形状。**这个缺陷是在验收 VM 上原生跑测试二进制才暴露的**，Windows 宿主上 `filepath.IsAbs` 的语义不同，一直没报——记在这里：平台相关判定必须在目标平台上验证 |
| Q41 | 2026-10-20 | 实施决策 | Web 侧调用代理的客户端（`internal/agentclient`）怎么区分“业务拒绝”与“代理不可用”。 | 已决策：两类错误分开。代理明确回了 4xx/5xx 信封 → `*agentclient.Error`（带 `Status`、`Code`、`Message`），上层按契约错误码处理，可以原样映射成 API 错误；拨号失败、超时、响应无法解析、socket 不存在 → `*agentclient.UnavailableError`，上层必须当作**依赖不可用**而不是业务结论（例如首页指标应显示“采集不可用”，不能显示“磁盘 0 块”）。响应体上限 8 MiB，默认超时 120 s。客户端只接受 POSIX 绝对路径（`path.IsAbs`，不用 `filepath.IsAbs`：后者在 Windows 上把 `/run/...` 判成相对路径，见 Q40 同一类问题） |

| Q42 | 2026-10-20 | 实施决策 | 审计事件与业务变更的事务归属（P02.4 兑现 Q34 里的“P02.4 会把审计放进同一事务”）。 | 已决策：`audit.Writer.Record` 不自己开事务，而是检查 `ctx` 里有没有 `store.TxFromContext` 给的 `*sql.Tx`：有就写进该事务，没有就独立提交。因此业务成功提交则审计必然存在、业务回滚则审计一起消失，**不再有“业务生效但审计没写进去”的窗口**；被拒绝的请求（没有业务事务）仍独立落一条 `outcome=failure`，因为“请求被拒绝”本身就是需要留存的事实。注意这不是“审计失败就回滚业务”——两者现在是同一事务，本来就没有先后。`audit_events` 的 `UPDATE`/`DELETE` 仍由 SQLite 触发器拒绝（Q34） |
| Q43 | 2026-10-20 | 实施决策 | 代理侧的独立数据库、目录权限与迁移集（P02.4）。CONTRACTS §14.2 要求代理有独立数据库、Web 不得访问。 | 已决策：代理使用**独立 SQLite 文件** `/var/lib/upanel-agent/agent.db`（目录 `0700 root:root`、文件 `0600 root:root`），有**自己的迁移目录** `migrations/agentdb` 和自己的 `schema_version`，不复用 Web 库的迁移序列（否则 Web 一升级 schema 版本就把代理库当旧库迁移）。权限不只依赖 systemd 的 `StateDirectory`：`agentstore.Open` 在打开前校验目录（root 属主、组/其他不可写），打开后校验库文件（root 属主、`0600`），**代码层面自证**，因为“库文件放在 Web 可写目录里”等于把秘密交给 Web。本阶段只定义 `secrets` 一张表；§14.2 其余表的列要等定义它们的阶段（P04 磁盘、P05 服务、P09 代理/计划任务…）——现在猜列名等于编造契约 |
| Q44 | 2026-10-20 | 实施决策 | 秘密的加密方案与密钥文件形态（P02.4，CONTRACTS §14.2“维护中的 AEAD 实现，独立随机 nonce”）。 | 已决策：**AES-256-GCM，标准库 `crypto/aes` + `cipher.NewGCM`**，不引 `golang.org/x/crypto/chacha20poly1305`——后者会把 `x/crypto` 直接依赖抬到冻结版本（Q19），而 AES-GCM 在 amd64/arm64 上有硬件加速，标准库实现已足够且不增加依赖面。每条秘密**独立随机 12 字节 nonce**，单独成列（不拼在密文前缀里，便于验收脚本直接量长度）；AAD 绑定 `"upanel/agent/secret/v1|"+ref+"|"+purpose`，因此把密文挪到另一行或改用途都会解密失败。密钥文件 `/etc/upanel/agent.key` 为 `root:root 0600`，两行文本：`upanel/agent-secret-key/v1` + 32 字节 base64；`upanel-agent -init-key` 用 `O_CREATE|O_EXCL` 生成，**已存在就失败、绝不覆盖**（覆盖等于让所有已存秘密永久无法解密）。指纹取密钥 SHA-256 前 8 字节十六进制，只用于运维核对“这台机器的密钥是不是同一把”，不泄露密钥。`key_version` 列现在恒为 1，为将来轮换留位 |
| Q45 | 2026-10-20 | 实施决策 | 秘密的生命周期与接口形态（P02.4）。契约要求“未引用临时秘密 10 分钟销毁、被引用后转持久”，且不给 Web 读取明文的路径。 | 已决策：① 用途是**封闭集合**（smb_password / account_password / proxy_basic_password / backup_repository_password / notification_password / tls_private_key / sftp_private_key / app_env），`totp_seed` 标记为 internal-only（只走 P14 的注册流程，不接受上传），未知用途一律 400。② 只有 `POST /internal/v1/secrets`，**没有** `GET`/读取路由，`/secrets/<ref>` 与 `/secrets/<ref>/value` 返回 404（不是 405，避免暗示“有个资源只是方法不对”）；引用形如 `sec_` + 32 位小写十六进制。③ 引用（reference）可以在创建时给（直接落 persistent），也可以事后由 `Reference()` 提升；同一 reference 重复提升是幂等的，改绑到另一个 reference 被拒绝，已过期的临时秘密不能再被提升。④ 到期销毁 = 单条 `UPDATE ... state='destroyed', ciphertext=X'', nonce=X''`，**留墓碑行**（否则无法区分“引用错了”和“曾经有过但已销毁”，也无法防止 ref 被复用）。⑤ 时间列一律 `INTEGER` Unix 毫秒：RFC3339 文本带小数秒时时序比较不可靠，而到期判定必须能直接比大小。⑥ 明文只在代理进程内以 `[]byte` 存在，落库前加密、响应体不回显明文，请求体与解密结果用完 `wipe` |
| Q46 | 2026-10-20 | 缺陷修复 + 约定 | 日志脱敏的“按键名片段命中”策略会连**非秘密的元数据**一起打掉：VM 上 `journal-reports-key-fingerprint` 断言失败，`secret_key_fingerprint` 的值变成了 `[redacted]`。 | 已决策：不改脱敏规则（放行名单是漏洞，早晚会有人把真秘密塞进放行名单），改为**改日志字段名**：`key_path` / `key_version` / `key_fingerprint` / `ref` / `purpose` / `state`，都不含 secret/token/password/key_file 之类片段。同时补两个方向的回归测试：`TestKeepsNonSecretMetadataVisible`（这几个键必须原样可见）与 `TestRedactsSecretRefKey`（`secret_ref` 会被打码，用来固定这条约定）。这条规则适用于后续所有阶段：**要写进日志的元数据，字段名先过一遍脱敏词表**。这个缺陷是验收脚本在目标系统上发现的，宿主单测当时全绿 |

| Q47 | 2026-10-20 | 缺陷修复（验收脚本） | P02.4 回归时 `p02-2` 的审计断言失败：`审计结果均为 success` 与 `审计 target 带对象类型`。原因不是产品回归，而是**断言口径错了**——P02.4 把“被拒绝/失败的高风险请求也记审计”接上（PLAN.md 第 332 行明确要求），脚本自己制造的 403/409/422 因此留下了 `outcome=failure` 行，`OUTCOMES` 不再是单一 `success`；`TARGETS` 里也多了 `session`（再认证成功记在会话对象上）。 | 已修复：把笼统的“全部 success”换成三条精确断言——① 四个业务动作（`user.create/update/password.reset/delete`）**每个都至少有一条 `success` 审计**（`BUSINESS_SUCCESS` 必须恰好是这四个）——业务动作有 `failure` 行是正常的（被拒绝的写入本来就要留记录），真正的缺陷是“只记失败、没有一条成功”；② 被拒绝的请求必须有 `failure` 行，且 `failure` 行的 `redacted_details` 必须是含 `stage`+`error_code` 的结构化信息、不得出现任何 password/token 键（`DENIED_DETAILS`）；③ 业务动作的 `target` 前缀必须是 `user`（`BUSINESS_TARGETS`），`failure` 行的 target 只允许 `user`/`session`（`FAILURE_TARGETS_UNEXPECTED` 必须为空）。**断言变多也变严**，没有任何一条被删掉。教训：`expect_contains "OUTCOMES=success"` 这种“拿集合的一个可能取值当子串”的写法本身就是假断言，它只在数据刚好全绿时通过 |
| Q48 | 2026-10-20 | 缺陷修复（验收脚本） | P02.3 回归时两条日志断言失败（`journal-has-no-password` / `journal-has-no-secret`）：`journalctl -u upanel-agent` 是**全量历史**，把同一台机器上前一次 P02.4 运行的日志也算进来了（那里会记录 `purpose=smb_password` 这类用途名，不是秘密值，但含 `password` 字样）。反过来，P02.4 的 `journal-reports-key-fingerprint` 用全量日志去搜指纹，会让断言在**旧指纹**上误过——这是更危险的方向。 | 已修复：两个脚本都在启动服务前取 `JOURNAL_SINCE=$(date …)`，日志断言一律加 `--since "$JOURNAL_SINCE"`，只针对本次运行产生的行。“不含某串”的断言从此不会把历史行算到这次头上，“含有某串”的断言也不会被旧记录蒙过去。附带一条通用规则：**验收脚本里凡是用 journal 做断言，必须限定时间窗**，否则既不可复现也方向不定 |

| Q49 | 2026-10-20 | 缺陷修复（仓库卫生） | 编辑工具在 Windows 上把 `cmd/upanel-agent/main.go` 与 `internal/agentstore/secrets.go` 写成了 CRLF，`scripts/check.sh` 的 `gofmt` 槽位从 PASS 翻成 FAIL（`gofmt -l` 列出这两个文件）。同一轮里 `tests/integration/p02-3`、`p02-4` 也曾因 CRLF 在 VM 上直接崩在 `set: Illegal option -`。 | 已修复：全部 `.go` 与 `tests/integration/*.sh` 统一转 LF；新增仓库根 `.gitattributes`（`*.sh`/`*.service`/`*.sql`/`*.go` 强制 `eol=lf`，`*.ps1` 保持 CRLF）；两个验收脚本在开头明确写出“本文件必须是 LF 换行”。**连带影响**：源文件换行变化后 `upanel-agent` 的交叉编译产物哈希从 `43ca7e78…` 变为 `32c5e305…`，于是两台 VM 换上新二进制重跑 P02.3/P02.4，证据文件与哈希表同步更新（`docs/evidence/P02/2026-10-20-p02.4/`）。教训：换行符属于构建输入，改动源文件后必须重新交叉编译并重跑目标平台验收，不能拿旧二进制配新源码当证据 |

| Q50 | 2026-10-20 | 实施决策 | 预检计划由谁生成（P03.1）。可以直接让 Web 自己算一份“看起来合法”的计划，省掉一次 socket 往返。 | 已决策：**计划只能由代理生成**，Web 不复制动作表、不自己算 allowed。理由不是洁癖：执行前复核的价值取决于“计划是那个能真正执行的一方写下的”。Web 若也能造计划，则状态版本、目标指纹、锁都可以被 Web 单方面编出来，复核就变成自证。代价是 Web 必须把 `actor_context`（用户 id、会话哈希、再认证时间）一路带到代理，且代理要多一张 `preflight_plans` 表（迁移 0002）——接受了。Web 侧只保存**绑定用**的字段（`params_hash`、`target_json`、`state_version`、`agent_ref`、`expires_at`），并只做合理性校验（明显不合规的到期时间视为协议违约） |
| Q51 | 2026-10-20 | 契约差异（待确认） | CONTRACTS §4.1 的响应示例里确认短语是 `"FORMAT ABC123 P1"`（三段：动词 + 6 位码 + `P<n>`）；实现生成的是 `"FORMAT K7M2QX"`（动词 + 6 位码，无第三段）。 | 已决策（保持实现形态）并记录差异：契约正文只规定“危险确认短语由服务端生成，不能由前端决定”，**没有**规定格式，示例里的 `P1` 也没有解释；短语是一次性 + 120 秒 + 只回一次，多一段后缀不增加安全性，反而增加手抄出错概率。实现取 `phraseVerbPattern = ^[A-Z][A-Z0-9]{1,15}$` + 去歧义字母表 `ABCDEFGHJKLMNPQRSTUVWXYZ23456789` 的 6 位码（约 30 位熵）。**待确认**：如果 `P1` 原本是想表达“该计划的对象/序号”，需要在 CONTRACTS 里写清语义，否则 P03.2 的提交校验只能按整串精确比对（现在就是整串比对）。在这条被确认前不改契约，也不假装实现符合示例 |
| Q52 | 2026-10-20 | 实施决策 | 确认短语存在哪里（P03.1）。最省事的做法是 Web 和代理各存一份明文。 | 已决策：**只存哈希**，代理库 `phrase_hash = SHA-256(短语)`，明文只出现在预检响应的那一次；Web 侧一个字节都不存（连哈希也不存——Web 没有比对需求，比对发生在代理）。验收脚本在三处（`agent.db`、`-wal`、`-shm`）二进制搜索短语明文，并断言 Web 库里唯一含 `phrase` 字样的列是 `requires_phrase`。附带约定：短语不进日志、不进审计 `redacted_details`、不进 journal |
| Q53 | 2026-10-20 | 实施决策 + 缺陷修复 | 预检到期时间由谁定（P03.1）。 | 已决策：**代理定**，`expires_at = now.Truncate(time.Second) + 120s`；Web 存代理给的**同一个字符串**，只拒绝“已过期”或“超过 TTL + 5 秒时钟偏差”的计划（`ErrInvalidPlan`）。落库前不改写、不四舍五入。**修掉的缺陷**：之前代理按毫秒精度算到期、线上按 RFC3339 秒级下发，Web 存到的到期时间比代理库里的早几百毫秒，边界上会出现两个“真值”——改为先截断到秒再算。验收断言两端字符串逐字符相同（不是“相差 < 1s”） |
| Q54 | 2026-10-20 | 实施决策 | 预检在 P03.1 就需要真实动作，但 STAGES 明令 P03 通过前禁止写磁盘代码。 | 已决策：加一张**只在 `-acceptance-actions` 时登记**的验收动作表（`acceptance.probe` 要求再认证 + 短语 `PROBE`；`acceptance.ping` 都不要求、可取消，作为“策略来自登记表而不是硬编码”的对照）。生产登记表仍为空，`capabilities` 在 VM 上报告 `[]`。验收动作只登记规划器，没有执行器——预检路径上不存在能改系统的代码分支。不使用“mock 一个计划”的方式验收，因为那样验的是测试桩，不是产品路径 |
| Q55 | 2026-10-20 | 缺陷修复 | `internal/agentclient` 只把 HTTP 200 当成功，而代理所有创建类接口返回 201（`POST /internal/v1/secrets` 已如此）。P02.3/P02.4 的单测用 httptest 各自造响应，两边都“自洽”，因此一直没暴露。 | 已修复：客户端接受 **2xx**；新增真实 `agentclient` ↔ 真实 `agent` 处理器的端到端用例（经 `httptest` 转发到真实 `Handler()`，带 `peerInfo` 注入），覆盖 `CreatePreflight`（201 解析成功、业务错误仍是 `*agentclient.Error`、响应缺字段是 `*UnavailableError`）与 `PutSecret`（201 回归）。教训：**假对端会让两侧各自正确、合起来错误**；跨进程边界的功能必须至少有一条真 client 打真 handler 的用例 |
| Q56 | 2026-10-20 | 实施决策 | 公开预检 DTO 是否暴露 `state_version` 与 `agent_ref`（P03.1）。 | 已决策：**都不暴露**。它们是服务端绑定信息：前端拿到 `state_version` 只会诱使前端自己做“状态变没变”的判断（那不是前端的职责，也不可靠）；`agent_ref` 是内部引用，暴露后前端可能自己拼“用它去调代理”的路径。契约 §4.1 的示例也没有这两个字段。验收脚本对响应体做 `state_version` 与 `agent_ref` 值的双向搜索断言 |
| Q57 | 2026-10-20 | 约定 | 代理库 schema 版本随代理自己的迁移前进（P03.1 新增 `0002_preflight_plans`，版本 1 → 2），而 `p02-4` 验收脚本里有一条精确断言的 `schema-version=1`。 | 已决策：把断言更新为**当前应有的版本**（`2`），并在脚本里写明“这是当前应有的版本，不是上限；新增代理迁移时必须同步更新这一行”。这不是放宽检查——断言仍是精确相等，也没有改成“≥1”；代理库与 Web 库的迁移序列相互独立（Q43），两条线各自推进各自的版本号 |
| Q58 | 2026-10-20 | 实施决策 | `operation_id` 由哪一侧生成、去重锚点放在哪（P03.2）。可以让代理自己生成 id 再回传，Web 只存——少一次“先造 id”的动作。 | 已决策：**Web 生成 `op_`+32 hex，代理按 `operation_id` 去重**。理由：Web 要在调用代理**之前**就把 job 落库（先落库再提交），落库那一行必须已经有稳定身份，否则崩在“已提交、未记录”的窗口里就没法找回。代理侧 `operations.operation_id` 有 `UNIQUE` 约束，连同 `request_hash` 一起存；同一 `operation_id` 重复提交返回已有记录（`duplicate=true`）而**不再执行一次**。验收要求“Web 在提交后掉线仍能找到原 operation”就是靠这个锚点：Web 重启后拿 `jobs.operation_id` 去 `GET /internal/v1/operations/{id}`，不靠猜也不靠重发 |
| Q59 | 2026-10-20 | 实施决策 | 重复提交时，去重和“消费预检”谁先（P03.2）。 | 已决策：**先命中去重，再消费预检**。代理侧先按 `operation_id` 查已有操作，命中就直接回记录；Web 侧也同样先查 `idempotency_keys`。顺序反过来的后果很具体：一次网络重试（请求到了、响应丢了）会让预检被烧掉而任务没建成，用户看到“预检已用”却没有任何 job——把一个可恢复的抖动变成不可恢复的失败。消费预检本身是条件更新（`WHERE consumed_job_id IS NULL`），因此即便两个请求同时穿过前面的检查，也只有一个能拿到预检 |
| Q60 | 2026-10-20 | 实施决策 | `preflights.cancellable` 的默认值（P03.2）。P03.1 的预检表漏了这一列，提交时无从判断“这个动作能不能取消”。 | 已决策：**补列，`NOT NULL DEFAULT 0` + `CHECK (cancellable IN (0,1))`，默认不可取消**。保守默认的代价是用户少看到一个取消按钮；反过来的代价是给了一个按下去必然被拒的按钮，或者更糟——真的发了一个取消信号给不可取消的动作。可取消性只由代理的动作策略（`ActionPolicy.Cancellable`）声明，Web 不推断、不按动作名猜。**没有**在代码里写“cancellableOf 恒返回 false”这类占位实现来绕过补迁移（第一版草稿就是这么错的，整份删掉重写了） |
| Q61 | 2026-10-20 | 缺陷修复 | 对账（`syncFromAgent`）每轮都写一条 `job_events`，导致“十次提交一次执行”的验收里事件数对不上；不改的话轮询会把事件表灌满同一状态的回声。 | 已修复：写事件前先读旧 `status`/`step`，**只有真的变了才追加事件**，事件附带 `from` 字段。这不是为了让测试变绿而放宽断言——事件表的语义本来就是“发生过什么变化”，记录“我刚刚又确认了一次还是这个状态”会让历史失去意义，也会让后续的 SSE 推送（P03.3）每次都重放。修的是产品，测试的期望值保持严格（`[queued, succeeded]` 两条） |
| Q62 | 2026-10-20 | 契约差异（待确认） | 不可取消的动作收到取消请求，HTTP 状态码与错误码该是什么（P03.2）。CONTRACTS 第 5 节的 24 个错误码里没有“不可取消”这一项。 | 已决策（暂定）：**409 + `VALIDATION_ERROR`**，消息说明“该动作开始执行后不可取消”。409 表达“请求与资源当前状态冲突”，比 400（请求本身格式错）和 405（方法不允许）都准确；不新造错误码，因为 24 码是封闭枚举，扩枚举要改契约。**待确认**：如果 CONTRACTS 想单独区分“动作不可取消”和“动作已经开始”，需要新增一个错误码（例如 `OPERATION_NOT_CANCELABLE`），那时本决策随之调整。前端当前按 409 分支处理并展示服务端消息 |
| Q63 | 2026-10-20 | 实施决策 | 代理回 `NOT_FOUND`（操作记录不存在，例如代理库被重建）时，Web 该怎么办（P03.2）。最省事的做法是标记失败后“重新提交一次”。 | 已决策：**`failed`/`unconfirmed` + `AGENT_UNAVAILABLE`，绝不自动重发**。状态未知不等于成功，也不等于可以重来——如果是 `disk.format` 这类动作，“再试一次”意味着可能是第二次格式化。`error_json` 里写明 `unconfirmed` 与原因，把“我们不知道它有没有发生”如实交给人判断；`operation_id` 保留在 job 上，人工/后续对账仍可追踪。同理，代理不可达时 job 停在 `queued` + `needs_sync=1`，只对账不重发（STAGES §5“代理崩溃不自动重试破坏性动作”） |
| Q64 | 2026-10-20 | 缺陷修复 | 确认短语不匹配必须是 400，但 `internal/httpx.StatusForCode` 把 `CONFIRMATION_MISMATCH` 归在 409 那一组（P03.2 VM 验收暴露）。 | 已修复：改为 **400**，与 `CodeValidationError` 同组。依据是契约而不是测试的期望值——CONTRACTS §4.2 与 `api/openapi.yaml` 都写 400 `CONFIRMATION_MISMATCH`，代理侧也直接回 400。错在分类：把它当成“状态冲突”，而它其实是“请求内容不对”（用户抄错了短语），资源状态没变。后果比一个数字难看：前端按契约写的 400 分支永远收不到，只能落到通用错误处理。教训同 Q55——宿主单测没覆盖“每个码 → 状态码”的完整映射，是 VM 上按契约逐条断言的脚本抓到的；已在 `httpx` 的表上补齐注释说明分组依据 |
| Q65 | 2026-10-20 | 缺陷修复 | 代理侧“消费预检”偶发 `database is locked (5) (SQLITE_BUSY)`，表现为任务提交 500；只在前一次恢复性写入之后几百毫秒内出现，重试即好（P03.2 VM 验收暴露）。 | 已修复：DSN 加 **`_txlock=immediate`**。根因不是 busy_timeout 太短，而是**默认 DEFERRED 事务在 WAL 下不会用上 busy handler**：DEFERRED 先取读快照、到第一次写才尝试升级成写锁，升级冲突时 SQLite 立即返回 SQLITE_BUSY 并跳过 busy handler，所以 `busy_timeout(5000)` 根本没机会排队。IMMEDIATE 在 BEGIN 时就拿写锁，冲突交给 busy handler 按时限等待。附带好处是事务内的读也见不到“升级前的世界”，少一类隐蔽的读后写竞争。代价是写事务串行化程度更高——对单机面板的写入量无所谓。新增回归测试 `TestWithTxQueuesBehindAnotherWriter`：把 `_txlock=immediate` 去掉后它确实失败（`database is locked (5) (SQLITE_BUSY)`），保留时通过；不写“模拟 BUSY”的假测试 |
| Q66 | 2026-10-20 | 约定 | 验收脚本会随产品演进而过期（P03.2 回归时 p01-1 19/7、p01-2 2 失败、p01-4 7 失败）。过期时改脚本还是改产品？ | 已决策：**先判断“是产品错了还是断言错了”，两者处理方式不同且都不许放宽**。① 产品与契约不符（Q64、Q65 那种）→ 改产品，测试期望值不动；② 断言写的是**曾经的**行为、而现行行为已被后续阶段明确定义并写进文档（P01.4 的 SPA fallback、P02.3 的 `/run/upanel` 0755、P02.4 的 `-init-key`、迁移版本 1→5）→ 更新断言到现行行为，并在脚本里注释“这是当前应有的值 + 依据的阶段”。**断言只增不减**：p01-1 从 26 条变 29 条（新增 `agent-root-forbidden` 等），没有任何一条被删掉或改成“跳过”。③ 分不清的时候按产品缺陷处理——把脚本改绿最省事，但那正是把验收变成装饰的做法 |
| Q67 | 2026-10-20 | 缺陷修复 | 代理写“阶段”和写“对应事件”是两笔独立的写：`MarkRunning` / `Finish` 先 `UPDATE operations`，再 `INSERT operation_events`。读方可能先看到 `running`/`succeeded`、后（甚至永远不）看到 `started`/`finished`。P03.3 在 22.04 VM 上暴露为偶发失败：`TestOperationSubmitRunsOnceAndRecordsResult` 三次跑出两次 `事件太少: [{accepted} {started}]`，而阶段已经是 `succeeded`。 | 已修复（改产品，不改测试）：三处阶段变更（`MarkRunning`、`Finish`、`RequestCancel` 的两个分支）全部改成**一个 `WithTx` 事务里完成 UPDATE + INSERT**，条件更新带上 `stage = <读到的阶段>` 守卫，`RowsAffected = 0` 时回滚并通过 `errStageMoved` 重试（上限 3 次，竞争只来自“取消 vs 执行”两个写者）。**为什么必须在产品侧修**：Web 的 `MirrorAgentEvents` 依赖“阶段可见 ⟹ 对应事件可见”这条不变量——它先读操作再读事件，看到终态后就不再追事件，分两笔写会让终态事件永久丢失（任务日志少最后一条，`gap` 也补不回来）。**确定性证明**：新增 `TestFinishRollsBackStageWhenEventWriteFails`，用 `BEFORE INSERT` 触发器只让该操作的事件写入失败，断言阶段回滚到 `running`（`MarkRunning` 同理回滚到 `accepted`）；把 `internal/agentstore/operations.go` 换回 HEAD 版本时该测试失败（`阶段 = "succeeded"`），换回修复版本通过。另加 `TestStageChangeAndEventAreAtomic`：并发读方在写方推进期间任意时刻读到终态，都必须能读到终态事件 |
| Q68 | 2026-10-20 | 实施决策 | `job_events` 是只追加的（UPDATE 永远禁止），但日志这类高频事件需要按容量裁剪，否则一条长任务的日志会把库撑大（P03.3）。 | 已决策：**触发器按 `kind` 开口子**——`UPDATE` 任何时候都拒绝；`DELETE` 只允许 `kind = 'log'`（迁移 `0006`）。裁剪在 `MirrorAgentEvents` 之后执行，单任务保留最近 1000 行（`jobLogRingCapacity`）。为什么不放到应用层“写满了就不写”：那会让“日志丢了”变成静默行为，且并发镜像时无法原子判断；触发器 + `DELETE ... WHERE kind='log' ORDER BY id LIMIT` 是数据库自己保证的边界。**状态类事件（`status`/`progress`/`gap`）一条都不删**：它们是任务历史，也是 SSE 断线重连的水位依据 |
| Q69 | 2026-10-20 | 实施决策 | SSE 事件流的推送方式：进程内广播中心（job 状态变化时 fan-out 给所有订阅者），还是每连接轮询 + 按需对账（P03.3）。 | 已决策：**每连接轮询**——每 1 秒一轮：先 `GetJob` 对账（必要时 `syncFromAgent`），再从 `job_events` 按 `seq > after` 拉增量，15 秒一次 `: keepalive` 注释帧，单用户最多 4 条连接（第 5 条 429）。理由：① 事件的**唯一生产者是代理**，Web 只是镜像，广播中心省不掉那次对账，只省一次本地查询；② 广播中心要处理订阅者生命周期、慢消费者背压、跨 goroutine 的关闭竞态，这些都是新缺陷面；③ 断线重连的正确性本来就靠 `Last-Event-ID` + `jobs.agent_event_seq` 水位，不靠内存里的最后一条。代价是 N 条连接 = N 次/秒的本地查询（单机面板，可接受）。**何时该换**：多实例部署或连接数上千时，再引入 `LISTEN/NOTIFY` 之类的进程外广播 |
| Q70 | 2026-10-20 | 实施决策 | viewer 能不能看到别人的任务（P03.3 的任务中心）。P03.2 已定“别人的 `operation_id` 一律 404”，但任务中心是列表页，列表要不要按人过滤？ | 已决策：**per-user 隔离，沿用到列表**——viewer 的 `GET /api/v1/jobs` 只返回自己的任务，别人的任务详情/事件/日志一律 404（不是 403，不泄露存在性）。理由：CONTRACTS 里 job 没有“共享/公开”概念，先按最小可见面实现；404 让 IDOR 探测拿不到信号。**已知限制**：多管理员协同时看不到对方正在做什么（运维上确实会想要“全局任务视图”），P04 概览也不做跨用户任务列表——要改就得先在 CONTRACTS 里定义“谁能看谁的任务”，属于契约变更，不在本阶段擅自定义 |
| Q71 | 2026-10-20 | 约定 | 验收脚本的运行身份（root / 非 root）没有任何强制，靠执行者记得。P03.3 回归时我用 `sudo sh p01-2-acceptance.sh` 跑本该以**非 root** 运行的脚本，root 绕过 `0500` 目录权限，“数据库目录不可写必须拒绝启动”那条永远等不到退出，脚本挂了 32 分钟，并且前两条断言判失败。 | 已修复（改调度，不改断言）：新增 `tests/integration/run-all.sh`，按脚本头声明的身份分组——`p01-1/p01-2/p01-4/p02-1/p02-2` 用 `setpriv` 降到 `upanel` 账号跑，`p02-3/p02-4/p03-1/p03-2/p03-3` 用 `sudo` 跑，逐个顺序执行并把完整日志留下（`/tmp/upanel-accept-<name>.log`），最后打印失败脚本清单。**没有改动任何断言的期望值**，改的是“用哪个身份去跑”。教训写进脚本头：跑错身份不会报“身份错”，只会得到看似通过或诡异挂起的结果——p03.2 时期 p01-2 记录的 34/0 是当时用 `upanel` 用户跑的，不是 root |
| Q72 | 2026-10-20 | 范围补充（待确认） | P03.3 交付“任务中心”，但 STAGES 指定的验收对象（任务列表/详情/事件流/取消）全部是 **admin 权限**，而 P02.1 只做了登录接口、**没有前端登录页**。没有登录入口，任务中心在浏览器里永远打不开。 | 已决策（暂定）：**补一个最小登录页 + 路由守卫**（`web/src/views/LoginView.vue`、`web/src/stores/session.ts`、`web/src/router/index.ts` 的 `installAuthGuard`、`safeRedirect`）。范围边界严格限定：只做“登录 / 退出 / 会话恢复 / 未登录跳转 / 回跳地址合法性校验”，**不做**注册、找回密码、MFA、账号管理界面（那是 P02.2 的接口，UI 仍未交付）。**待确认**：如果 STAGES 计划把登录界面放到更后面的阶段，本项应从 P03.3 移出，只保留守卫（那样任务中心只能靠 API 脚本验收）。登录页的浏览器证据只有 Chromium 无头冒烟，未在 Firefox/Safari 验证 |
| Q73 | 2026-10-20 | 实施决策 | Web 侧“启动时检查所有非终态和未同步任务”（CONTRACTS §4.3）没有触发点：P03.2/P03.3 只在用户读任务时才向代理对账，没人打开页面就永远不同步。 | 已决策（P03.4）：`operations.Service.RunReconciler` 在 `cmd/upanel-web` 里以 goroutine 启动，**启动立刻扫一遍**，之后每 `ReconcileInterval = 30s` 一遍；`ReconcileBatch = 100`，按 `updated_at` 升序（最久没人管的最先处理）。间隔取 30s 的理由：待对账任务通常是零到几条，一轮就是几条 socket 调用；更长会让“页面停在运行中”的时间变长，更短只是白耗。扫描只调 `GET /internal/v1/operations/:id`，**没有任何 CreateOperation 调用点**——对账永远不重发。日志记 `scanned/advanced/deferred/unconfirmed/unknown/write_failed/skipped_inflight` 七个计数 | 
| Q74 | 2026-10-20 | 缺陷预防 | 对账与“提交还在途中”会撞车：任务已落库、`CreateOperation` 还没返回的几秒里，代理确实查不到这条操作。若据此下结论，一个正在执行的 `disk.format` 会被写成 `failed`，而代理随后照样执行——面板说了谎，且是最危险的那种。时间宽限期（例如“60 秒内不判定”）只是把窗口糊住，重启后无法区分。 | 已决策：**在内存里登记在途任务**（`Service.inflight`，`SubmitJob` 在调用代理前后 `beginSubmit/endSubmit`），对账遇到在途任务返回 `outcomeInflight` 并计入 `skipped_inflight`，既不下结论也不发查询。读接口与后台扫描共用同一份判定（`reconcileOne`），所以“被读”与“被扫”不会给出不同终态。**同一时刻只允许一个 Web 进程**（systemd 单实例），跨进程的在途状态不在覆盖范围内，这一点写在代码注释里 |
| Q75 | 2026-10-20 | 缺陷修复（修订 Q63） | Q63 把所有 `NOT_FOUND` 都写成 `failed`/`unconfirmed` + `AGENT_UNAVAILABLE`，理由是“状态未知不能当成成功，也不能重来”。这条禁令是对的，但**状态选错了**：对一个代理曾经确认过、正在跑的任务，写 `failed` 等于断言“没有执行”，而实际是“不知道”。P03.4 要求区分“执行结果未知”。 | 已决策：按 `needs_sync` 分流。`needs_sync=1`（Web 从未成功同步过）→ 代理的记录是持久化的，查不到就是没落地：`failed` + `unconfirmed` + `AGENT_UNAVAILABLE`（与 Q63 一致）。`needs_sync=0`（曾经同步成功过）→ 代理侧记录丢失，执行与否未知：`interrupted` + `unknown_result` + `OPERATION_INTERRUPTED`，`cancellable=0`，错误体要求“先核对系统实际状态，再决定是否重新预检”。两条路都**不重试提交**。拦截证据：`TestSweepPendingMarksUnknownResultInterrupted`（状态、步骤、错误码、不可取消、终态不再被扫）；把在途登记去掉时 `TestSweepPendingSkipsInFlightSubmit` 与 `TestSubmitRacingSweepDoesNotMarkFalseFailure` 均失败（实测） |
| Q76 | 2026-10-20 | 实施决策 | `idempotency_keys` 的 TTL 只写了 `expires_at` 从不清理（P03.2 注释“清理属 P03.4”）。直接删到期记录会制造重复执行：一个还在跑的任务，键一删，用户重试就会得到第二个 operation。 | 已决策：`PurgeExpiredIdempotencyKeys` 只删 `expires_at <= now` **且** 关联任务已在终态的记录；`RunReconciler` 每 `IdempotencyPurgeInterval = 1h` 跑一次。清理不动 `jobs`（执行历史不能因为清理而消失，CONTRACTS §14.2）。单元证据：`TestPurgeExpiredIdempotencyKeysRemovesFinishedJobs`、`...KeepsUnfinishedJobs`。**VM 未验证**：1 小时周期无法在验收脚本里等待，只能靠单元测试 |
| Q77 | 2026-10-20 | 约定 | “测试用无害动作由独立测试注册表提供，不放入生产白名单”（STAGES P03.4）此前只有注释与启动参数保证。 | 已决策：把约束变成断言——`internal/agent/acceptance_test.go` 要求生产登记表为空、验收动作全部带 `acceptance.` 前缀且不出现在生产表里、每个验收动作 `Decode/Plan/Execute` 齐全、预检计划必带 `acceptance_only` 告警且 `Impacts` 只有 `none`、参数解码拒绝未知字段（不给字符串命令留入口）。黑盒侧：`p03-4` 用**不带** `-acceptance-actions` 的代理预检 `acceptance.probe`，必须 400 + “未登记的动作” |
| Q78 | 2026-10-20 | 缺陷修复 | `TestGateDeniesOtherUIDs` 断言“错误响应不能泄漏允许的 UID”，做法是 `strings.Contains(rec.Body.String(), "4242")`——扫的是**整个响应体**，里面有随机生成的 `request_id`。某次 `request_id` 恰好是 `9bc265424233c38067f88efba4b446f6`，其中含 “4242”，测试失败并让 `check.sh` 的 go-test 变红（P03.4 证据采集时实发）。 | 已修复：新增 `decodeErrorEnvelope` 解析错误信封，**只断言 `error.code` 与 `error.message` 两个字段**——`code` 必须是 `FORBIDDEN`，`message` 不得包含允许的 UID（`4242`）或调用方 UID。泄漏与否跟随机串无关，断言也不该被随机串左右。同一文件里另两处 `rec.Body.String()` 只用于失败时的诊断输出，不是判定依据，不动。**没有**改成“重试到不撞上为止”或“把 4242 换成别的值”——那只是把概率挪走，概率还在（每个请求 32 个 hex 字符约有 29 个位置，命中概率 ≈ 0.04%）。教训：判定性断言必须先解析再比对字段，不能对整块报文做子串匹配 |
| Q79 | 2026-10-20 | 缺陷修复（测试现场） | `p01-4-acceptance.sh` 的 systemd 段把服务库放在 `/var/lib/upanel/upanel.db`，但从不清理上一轮留下的库。P03.4 回放时 24.04 与 22.04 同时在“migrate 以服务账号执行 exit=0”上失败（实际 1），随后 `systemd-service-active` 看到 `activating`、`serves-index-200` 看到 `000`，连带 4 条失败。真实原因：旧库是中间版本建的，`schema_migrations` 里记着 `0006_job_event_mirror` 的**旧校验和**，新二进制按设计拒绝启动——`迁移 0006 的内容与已应用版本不一致（校验和不同）；已应用的迁移不可修改，请新增一条迁移`。 | 已修复（改测试现场，不改产品）：systemd 段在 migrate 之前删掉 `/var/lib/upanel/upanel.db{,-wal,-shm}`，并注释说明这份库由脚本自己创建。**产品行为是对的**：迁移校验和就是为了拦住“悄悄改已应用的迁移”——那会让两台机器的库结构静默分叉。教训：验收脚本必须自己负责现场清理；依赖“上一轮留下的状态恰好兼容”会让同一份代码今天通过明天失败，而且看起来像产品坏了 |
| Q80 | 2026-10-20 | 缺陷修复（断言精度） | 两处断言用“按时间取最新一条”来指认对象：`p03-2` 的“短语不符留下一条 failed 任务”用 `SELECT status FROM jobs ORDER BY created_at DESC LIMIT 1`，`p03-4` 的“代理侧遗留操作被标 interrupted”用 `...(SELECT operation_id FROM operations ORDER BY created_at DESC LIMIT 1)`。`created_at` 只有秒级精度，同一秒里有多条记录时取到哪一条不确定；后者更糟，它等于“取最新操作的状态”，把要指认的对象从断言里抹掉了。P03.4 回归时 p03-2 那条判出过一次 `queued`（同一份代码在 24.04 与随后的重跑里都是 failed）。 | 已修复：**断言必须指名对象**。p03-2 改成 `idempotency_keys.key='wrong-phrase-key' → job_id → status`，并补一条“该 job 的 `error_json` 含 `CONFIRMATION_MISMATCH`”，确保查的就是被拒的那一条；p03-4 改成用 Web 库里的 `jobs.operation_id` 去查代理库的 `operations.operation_id`。**没有放宽任何期望值**，两条都变严了（多了“键确实指向该任务”“错误码落在该任务上”）。修完后两台 VM 上 p03-2 各连跑 2 次（共 4 次）全过，其后全量回归再各跑 1 次也全过，未再出现该失败。**未查明**：第一次失败时被判为 `queued` 的那一行究竟是哪条任务——现场已被脚本清理，我不会把“猜到的解释”写成结论；能确定的是断言取行不确定这一事实本身，以及它已不再可能发生 |
| Q81 | 2026-10-20 | 实施决策 | 系统基础资源在哪一侧采集：Web 每次请求现读 `/proc`、浏览器轮询代理，还是代理侧定时采样缓存（P04.1）。 | 已决策：**代理侧每 5 秒轮询一次，结果放内存快照，`system.summary` 只读快照，Web 只透传**。理由有三条：① `/proc` 是机器级的，一次读取应当服务所有阅读者——每个浏览器各自触发一次采集，等于把“多少人开着首页”变成“CPU 负载”；② 时间戳必须是真的：`collected_at` 是采集时间，如果 Web 每次现采，时间戳就变成“请求时间”，前端再也无法区分“数据是新的”和“数据是刚才编的”；③ 计数器速率天生需要相邻两次样本，采样点必须由服务端掌握，不能让“访问频率”决定速率是否可得。**Web 侧刻意不做第二层缓存**：多一层缓存就多一个“多久之前的数”的谎，代理的 `collected_at` 会被 Web 的时间盖住。 |
| Q82 | 2026-10-20 | 实施决策 | 内存字节数与网络计数器在 JSON 里是数字还是字符串（P04.1）。Go 默认把 `uint64` 序列化成数字，前端 `JSON.parse` 会转成 `Number`，2^53 之后悄悄丢位。 | 已决策：**按 CONTRACTS §1 用十进制字符串**（`TotalBytes *uint64 \`json:",string"\`` 一族，共 8 个内存字段 + 8 个计数器字段）；百分比、负载、逻辑核数、采样窗口毫秒这些量级安全的仍是数字。`internal/monitoring/types_test.go` 直接钉住线格式：`"total_bytes":"4009864"`、`"usage_percent":12.5`、`"rx_bytes_per_second":null`。这条最初是错的（计数器序列化成了数字），宿主的线格式测试把它抓了回来——契约里写清楚的东西，不能靠“反正现在是小数”侥幸。 |
| Q83 | 2026-10-20 | 实施决策 | 采集段失败时前端该看到什么（P04.1）。最容易的做法是“失败就把值清 0”，前端不用处理缺值；但那会让一次读取失败伪装成“内存真的只有 0 字节”。 | 已决策：**段级 `status` 与值的时间分开表达**：`status` 是**最近一次采集尝试**的结果（`ok`/`error`/`unavailable`），值自身带 `collected_at`；失败时**保留上一次成功的值并保留它的旧时间**（PLAN §8“监控展示最近有效值及其时间”），从没成功过就是 null。`degraded`（有段不 ok）与 `stale`（快照超过 3 个采集间隔）在 `Summary` 层单独给，别让前端从段状态猜整体是否可信。速率类字段没有前一次样本、计数器变小、时间窗不为正时一律 **null 而不是 0**（DECISIONS 早期就冻结过“不用零值冒充失败”）。`boot_id` 变化会让 CPU 采样窗口失效，换内核不产生一个假的使用率尖峰。 |
| Q84 | 2026-10-20 | 实施决策 | `capabilities` 报什么（P04.1 起代理第一次有真实查询）。图省事可以在注册表里永远登记 `system.summary`，没装采集器时返回 501/空结构。 | 已决策：**只登记真实存在的查询**。`system.summary` 仅在 `ProductionDeps.Collector != nil` 时登记，否则 `capabilities` 里没有它、查询返回 400“未登记”。宁可少报（Web 立刻知道这条读取路径不存在），也不要让 Web 相信一个会返回空壳的通道——空结构等于骗人，501 则把“部署没接上”变成“功能坏了”。验收模式同理：动作表用验收表，**查询表仍用生产实现**（`RegisterProductionQueries`），这样验收走的是真实采集路径。 |
| Q85 | 2026-10-20 | 约定 | `/proc` 解析的单测夹具从哪来（P04.1）。手写一份“看起来像 `/proc/stat`”的文本，等于用自己脑子里的格式去测自己的解析器。 | 已约定：**真实采集 + 合成异常样本两类，都登记来源**。真实样本（`stat-cpu-2404.txt`、`meminfo-2404.txt`、`loadavg-2404.txt`、`netdev-2404.txt`、`os-release-2404.txt`、`uptime-2404.txt`）来自 24.04 验收 VM 的 `cat`，命令、退出码、是否脱敏写在 `tests/fixtures/PROVENANCE.md`；异常样本（截断、缺合计行、缺 MemAvailable、列数不对、空网卡表、缺 `ID`、坏 loadavg）**明确标注为合成**，因为它们描述的是内核不会发生的形态，只能人工构造。合成样本永远不许冒充真实采集。 |
| Q86 | 2026-10-20 | 缺陷修复（验收现场，断言过期） | `p02-3-agent-acceptance.sh` 断言 `supported_queries == []` 且 `{"query":"system.summary"}` 返回 400“未登记”。P04.1 一登记这条查询，两条断言必然失败。 | 已修复：**改断言，不改产品**。查询表那条从“为空”改成“**完全等于**已登记项”（`['system.summary']`）——少报是隐藏能力，多报是编造能力，两种都算失败；未登记样本换成 `system.nonexistent`；并把 `system.summary` 当成“已登记但拒绝夹带参数”的样本，**新增**两条断言（200 且带 `data`）。p02-3 从 84 项涨到 86 项，不是缩减。这与 Q66 同一条规则：产品行为按已文档化的现行契约演进时，验收脚本跟着收紧，而不是删掉。 |
| Q87 | 2026-10-20 | 缺陷修复（验收现场，工具缺陷） | `p04-1` 的时间戳断言在 24.04 全绿、在 22.04 全红（三项判 False）。原因是 22.04 的 Python 3.10 `datetime.fromisoformat` 只认 3/6 位小数秒，而 Go 的 RFC3339Nano 给的是 9 位。 | 已修复：**改验收脚本，不改产品**。契约要求 RFC3339，纳秒小数合法，Go 的默认输出没有问题；是脚本用了只在新版 Python 上成立的解析。修法是把时间比较收进 `ts_age_lt` / `ts_gap_in` 两个函数，内部先把小数秒截到 6 位（毫秒以内对“是不是最近 5 秒”没有意义）。教训与 Q86 互补：断言失败先分清是**产品错了**还是**验证手段过时**，两者都要改对，但不能互相顶账。 |
| Q88 | 2026-10-20 | 实施决策 | 代理不可用时，Web 的系统信息接口该返回什么（P04.1）。采集失败既可能是“代理没起”，也可能是“代理起了但读 `/proc` 出错”。 | 已决策：**分两种**——`agentclient.UnavailableError` → 503 `AGENT_UNAVAILABLE`（“代理不可用，暂时读不到系统信息”，此时 `data` 为空，不给任何数字）；其余读取失败 → 500 `COMMAND_FAILED`。并且**采集不在登录/会话路径上**：代理停机时登录照常 200（VM 验收有断言），首页指标坏掉不该让人进不了面板。代理恢复不需要重启 Web，接口自动恢复 200（同一条验收的第 7 段）。 |
| Q89 | 2026-10-20 | 缺陷修复（真实 VM，产品缺陷） | 代理在 24.04 上启动约 20 秒后被 `Out of memory` 杀掉（VMA 1.4 TB / RSS 3.7 GB），夹具单测全绿。`cmd/storagedump` 的 goroutine 栈指到 `storage.readFile ← storage.readDiskLinks ← storage.Scan`，单次读 229 MB。根因：`/dev/disk/by-*/<name>` 在真实系统上是指向**块设备**的符号链接，代码用 `os.ReadFile` 读它，等于把整块盘读进内存（4 GiB 的盘就是 4 GiB 内存）。 | 已修复：新增 `linkTarget`——先 `os.Lstat` 判断是不是符号链接，是就 `os.Readlink`（相对目标相对链接目录解析）；只有普通文件才读内容，并且只接受 `/dev/` 开头的绝对路径（夹具形态）。新增 `TestReadDiskLinksUsesSymlinkTarget`（真符号链接 `../../sda1` 必须解析成 `sda1`；指向 1 MiB 普通文件的链接必须被忽略），Windows 上因无符号链接权限 `t.Skip`。修复后同一台 VM 上 `storage.Scan` 21 ms / 堆 1 MiB。**这个缺陷只有真机验收能发现**：夹具里这些条目是普通文件，永远不会触发整盘读取 |
| Q90 | 2026-10-20 | 缺陷修复（契约违反） | `snapshot_version` 每 5 秒就变，违反 CONTRACTS §6“写动作预检用它与执行前版本比对”。原因：指纹把 `collected_at`（每次扫描都变）和挂载点容量（Collector 后填的 statfs 结果）一起哈希进去了。 | 已修复：`snapshotVersion` 哈希前先归一化——清掉每个节点的 `collected_at`、`health.checked_at`，清掉挂载点 `capacity`。新增 `TestSnapshotVersionIgnoresTimeAndCapacity`（相隔 37 秒的两次扫描指纹相同；补上容量后仍相同；改挂载路径后不同）。验收里对应“内容不变时 snapshot_version 不变”“挂载消失后回到原值”两条断言 |
| Q91 | 2026-10-20 | 实施决策 | `/dev/disk/by-*` 的链接目标怎么算“是设备”。可以按名字长相（`sd[a-z]+`、`nvme\d+n\d+`…）过滤，也可以不过滤（消费者按设备名查表，查不到自然忽略）。 | 已决策：按**本次扫描枚举到的设备名集合**过滤（`readDiskLinks(devDir, known, kind)`，`known` 来自 sysfs 枚举，含分区）。理由：`/dev` 下任何文件都可能被链接指向（例如 loop 镜像文件），只有 sysfs 认得的才是块设备；按名字长相过滤会在 `md`/`dm-`/`mmcblk`/`xvd` 这类命名上漏。链接目标先按 `devDir` 相对化（`devNameIn`），真实系统上 `devDir=/dev` 与原来等价，夹具里 `devDir=<采集目录>/dev` 也能得到同一语义。验收核对：24.04 / 22.04 上 `sda1`/`sda15`/`sda16` 的 uuid 与 label 仍然解析出来（cloudimg-rootfs、UEFI、BOOT），没有因为过滤而丢 |
| Q92 | 2026-10-20 | 实施决策 | `capabilities` 在 P04.2 报什么。存储只读阶段没有页面动作，但也不能报“什么都没做”。 | 已决策：`queries: ['storage.topology', 'system.summary']`、`actions: []`；每台设备的 `capabilities` 在 P04.2 恒为 `[]`（CONTRACTS §6.1）。空数组是“还没有可用动作”，不是“这个设备不支持任何操作”——动作登记随 P07 写路径一起出现。`p02-3` / `p04-1` 里写死的 `capabilities` 断言（原本只列 `system.summary`）同步更新，并加注释说明少报多报都算失败 |
| Q93 | 2026-10-20 | 实施决策 | 系统盘保护（`protection.system_disk`）怎么判、保护沿什么扩散。按设备名猜（`sda` 就保护）显然不对；只看挂载也不行（RAID/LVM/加密叠起来时承载 `/` 的是最底层那块盘）。 | 已决策：从 `/proc/self/mountinfo` 找出承载 `/`、`/boot`、`/boot/efi` 等关键挂载点的设备（`longest_mount` + `mnt_device`，按 major:minor 对到设备名），标 `system_disk=true` 并写明理由；然后**沿拓扑边向上递归**（`partition_of` / `depends_on`），凡依赖受保护设备的父设备也受保护。验收在 VM 上用**独立实现**重算一遍（shell + mountinfo），逐台比对，防止“用被测代码验证被测代码”。空白磁盘不保护；临时挂载的 loop 镜像不因“被挂载了”而变成系统盘（第 8 段实验） |
| Q94 | 2026-10-20 | 实施决策 | 挂载点容量的采集节奏与一致性要求。容量随写入实时变，若每次 JSON 都重算，拓扑指纹和采集节奏都会被拖乱。 | 已决策：容量按 PLAN §8 单独采集，30 秒 TTL 缓存，随挂载点带 `collected_at` 说明它可能比拓扑旧；`available_bytes` 取 `statvfs.f_bavail`（非 root 可用块），不是 `f_bfree`。验收分两处：`/` 上 `total_bytes` 精确相等、`available_bytes` 落在实时 `f_bavail` 的 0.5% 内（30 秒窗口内会漂移，容差写进脚本注释）；严格对账放在空闲的 64 MiB loop 镜像上——那里 `available_bytes` 与 `f_bavail` **精确相等**，而 `f_bfree` 差 4.69 MB，所以这条断言真的能区分两种取法 |
| Q95 | 2026-10-20 | 缺陷修复（验收脚本工具缺陷） | `p04-2` 的 `jget`/`jgetx` 用 `print(eval(...))` 执行探针，多语句块被 `eval` 拒后走 `exec`，结果把 `None` 也打印出来（断言值变成 `"False\nNone"`），而 `NameError` 被当成空串静默失败（`ns` 里没有 `json`）。 | 已修复：抽出公共执行体 `_probe`，把 `stdout` 用 `contextlib.redirect_stdout` 截下来——语句块 print 了什么就用什么，没 print 才用 `eval` 的返回值；命名空间补上 `json`/`os`；Python 报错照旧进验收日志不吞。教训：验收脚本本身的断言逻辑也要当真代码看，否则“通过”和“失败”都可能没意义 |
| Q96 | 2026-10-20 | 缺陷修复（测试不稳定，宿主机负载下暴露） | P02.4 的 `TestSweepExpiredSecrets` 在高负载下偶发失败（本轮 `go test ./... -v` 出现一次，耗时 1.66 s 而不是 0.14 s）。“未到期不销毁”和“到期后销毁”写在同一台 TTL=50ms 的服务器上，机器一忙，POST 到第一次扫描之间就超过 50ms，秘密已到期，第一条断言翻成 false。 | 已修复：拆成两台服务器——TTL=1 小时的服务器断言“未到期不销毁、仍能解密”（差几毫秒不会翻转），TTL=50ms 的服务器等满 80ms 后断言“销毁 1 条、解密失败、重复扫描 0 条”。语义没放宽（两个方向都还在，而且都用真实时间），只是把“断言边界落在 50ms 上”这个必然抖动的写法去掉。抽查 `-count=5` 连续通过 |
| Q97 | 2026-10-21 | 实施决策 | `io` 要不要参与 `snapshot_version`（P07 预检靠它判断“状态有没有变”）。放进去最省事：io 就在 Device 上，哈希整个节点自然带上。 | 已决策：**不参与**。速率每 5 秒都在变，放进去会让预检永远过期——一次没有改变任何东西的写操作也会被判成“拓扑已变”，人要反复重试。实现上 `attachIO` 放在 `snapshotVersion` 之后调用，`snapshotVersion` 里再清一遍 `io`（双保险），并新增 `TestSnapshotVersionIgnoresTimeAndCapacity` 的 io 分支与验收断言“11 秒后版本不变、期间计数确实变了”。代价：只看版本号看不出 I/O 变化，前端要自己比计数；这正是想要的语义——预检关心的是拓扑，不是负载 |
| Q98 | 2026-10-21 | 实施决策 | smartctl 的“退出状态”怎么解释。`smartctl -j` 既在 JSON 里给 `smartctl.exit_status`，进程本身也有退出码，两者都可能是 0—255 的位掩码。按“非 0 就算失败”处理会把“历史自检记录非空”（128）这种纯提示报成 critical。 | 已决策：**按位解释**，1=命令行、2=打开失败/低功耗、4=SMART 命令失败、8=磁盘失效、16=预失效当前、32=预失效历史、64=错误日志、128=自检日志；失效与预失效为 `critical`，历史位与日志位为 `warning`，4/2 与“位 0 命令行被拒”为 `unknown`；位 0 命中时**立即**返回 unknown，因为命令行都没被接受，后面的字段不可信。JSON 里的 exit_status 与进程退出码不一致时以进程为准，并把这条差异写进 details（曾因为 reasons 被覆盖而丢过，现在是显式放在最前面）。单测覆盖 1/2/4/8/16/32/64/128 与 `8 \| 64` 组合 |
| Q99 | 2026-10-21 | 实施决策 | 温度没有读数时填什么。QEMU 的 SCSI 盘在 `smartctl -j` 里给出 `"temperature": {"current": 0}`，直接透传会出现 0 °C。 | 已决策：`current <= 0` 一律当作**没有读数**，`temperature_celsius` 为 `null`（CONTRACTS §6“不要把 unavailable 的温度转成 0°C”）。验收里对每条断言都是“null 或 > 0”，并额外用本机 `smartctl` 的输出交叉比对：smartctl 报 50 °C 时 API 必须给出 50（容差 10），smartctl 没读数时 API 必须是 null。真实 NVMe（VM-E）是唯一给出真实温度样本的机器 |
| Q100 | 2026-10-21 | 实施决策 | 怎么做到“不为读健康而唤醒磁盘”，以及休眠盘该报什么状态。 | 已决策：**两段式**。先读 sysfs（`/sys/class/block/<dev>/device/power/runtime_status` 与 `power/control`），能判定为休眠就立刻返回 `sleeping`、`source=sysfs`，**根本不启动 smartctl**；判不了才对非 NVMe 设备加 `-n standby`（NVMe 没有这个语义，加了会被 smartctl 拒绝），让 smartctl 在最后一刻自己放弃（退出位 2），此时同样报 `sleeping` 而不是 unknown——休眠不是故障。`sleeping` 与 `unknown` 的区别很重要：前者是“盘在睡觉，没病”，后者是“我们看不到”。这条路径**只有单测**（`TestSmartProberSleepingDeviceNoSpawn` 断言零次 spawn、`TestSmartProberArgs` 断言参数里带 `-n standby`）：两台 QEMU VM 的盘进不了 standby（SCSI 忽略 `-s standby,now`，NVMe 的 `runtime_status` 是 `unsupported`），真机证据拿不到，验收脚本里标 NOT RUN |
| Q101 | 2026-10-21 | 实施决策 | 网络挂载（NFS/CIFS）在拓扑里放哪儿。P04.2 的 `filesystem.mounts` 只挂在“有块设备”的节点上，而 NFS 的 major:minor 是伪设备号，`/sys/dev/block` 里查不到。验收时发现这意味着隔离代码永远没人调用——`fillCapacities` 里那条网络分支在真机上是死代码。 | 已决策：`Topology` 增加与 `nodes`/`edges` 平级的 `network_mounts`（CONTRACTS §6.2 已补），只收没有块设备且 `fstype` 属于网络文件系统的挂载（tmpfs/proc/cgroup 不进），按路径去重排序。`reachable` 是三值：`true` 读到容量、`false` 明确失败、`null` 还没查过——用 `false` 表示“不知道”会让刚挂上的盘显示成断开。身份字段进指纹，`reachable`/`capacity`/`capacity_error` 不进。验收在 VM-B 上真做了一遍：`nfs-kernel-server` 自导出 + `hard,timeo=600` 挂载 → 停服 → 容量在缓存过期后变 null、`reachable=false`、`capacity_error` 同时说明超时与“多久之后不再重试”、5 秒扫描不受影响、期间只有一个 statfs 子进程（隔离生效）→ 重启服务后隔离到期自动恢复 |
| Q102 | 2026-10-21 | 验收方法 | 网络挂载的失败要等多久才看得见。容量有 30 秒缓存（PLAN §8），停服后不会立刻变红；第一版验收在停服 20 秒后就断言“容量为 null”，因此在 VM 上失败了一次（缓存还没过期，读到的是旧数字，但 `reachable` 仍是 true）。 | 已决策：验收改成**等结论**而不是等固定秒数——每 3 秒取一次快照，直到 `capacity` 变 null（上限 120 秒，实际约 32 秒），再断言；同时把子进程计数器改成“整个停服期间最多 1 个”的窗口式采样（用标志文件收尾，不再按固定 45 秒）。这条不是产品缺陷，是验收脚本自己的错误假设，记在这里防止下次又写“睡 20 秒就够了” |
| Q103 | 2026-10-21 | 缺陷修复 | `cmd/upanel-web` 的用例在装过面板的机器上失败。不传 `--config` 时 `run` 会读平台默认路径（Linux: `/etc/upanel/upanel.json`），而验收脚本会在真实机器上写这个文件（监听 19447）。于是同一份代码在宿主上全绿、在 VM 上`TestDBFlagMustBeAbsolute` / `TestDatabaseOpenFailureRefusesStart` / `TestMigrationFailureRefusesStart` 三条变红，stderr 里是“mock 模式只能绑定 127.0.0.1:18881，当前配置为 127.0.0.1:19447”。这是测试依赖机器状态，不是产品缺陷，但会让 `go test ./...` 的结论不可信。 | 已决策：测试里加 `testRun` 助手——用例自己没给 `--config` 时补一份 `t.TempDir()` 下的 `{}` 空配置，让每条用例与宿主环境无关。**产品代码一行没改**（默认路径行为是对的：装好的机器就该读系统配置）。补上之后 VM 上 `go test -race ./...` 只剩 `TestDefaultPasswordParamsTiming` 因检测器放大 Argon2id 耗时失败，0 条 `WARNING: DATA RACE` |
