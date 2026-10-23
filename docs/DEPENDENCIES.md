# 依赖核实与锁定

本文是 P00.2 的产出。所有版本都是**实际查询**得到的，不是推测。查询日期：2026-10-20（UTC）。

规则：

1. 表内版本即冻结版本。升级必须先按 `docs/DECISIONS.md` 记录理由、影响面和重新验收的证据。
2. 禁止 `latest`、`^` 浮动范围进入 `package.json`；`package-lock.json` 提交入库，CI 只用 `npm ci`。
3. Go 依赖在首次真正导入时按本表精确版本写入 `go.mod`，并提交 `go.sum`（P01.2 已执行）。
4. 系统软件包（Samba、Nginx、Certbot、restic、smartmontools 等）不在这里虚构版本；
   必须在引入该模块的阶段记录目标发行版的 `apt-cache policy <pkg>` 实际输出。
5. 本表未列出的依赖不得为了“方便”临时加入；需要新增时先记录再实现。

---

## 1. 语言与运行时

| 项目 | 冻结版本 | 依据 | 核实方式 |
|---|---|---|---|
| Go | **1.27.3** | go.dev 当前稳定补丁版本（同时维护 1.27.3 与 1.26.10） | `curl https://go.dev/dl/?mode=json` 返回 `go1.27.3 stable`、`go1.26.10 stable` |
| Node.js | **22.23.3**（Jod LTS，2026-09-23） | PLAN 第 3 节要求 Node 22 LTS 的补丁版本 | `https://nodejs.org/dist/index.json` 过滤 `v22.*` 的最新项 |
| npm | **10.9.9**（Node 22.23.3 自带） | 与 Node 绑定，不单独升级 | 该版本 Node 的 `npm --version` 输出 |

Go 下限由依赖决定，均低于冻结版本：`modernc.org/sqlite@v1.60.1` 要求 `go 1.26.0`，
`golang.org/x/crypto@v0.58.0` 要求 `go 1.26.0`，`gin@v1.12.0` 要求 `go 1.25.0`。

`go.mod` 写入 `go 1.27.3`。开发宿主的系统 Go 为 1.25.6，低于该约束；
已实测 `GOTOOLCHAIN=go1.27.3 go version` 会下载并使用 1.27.3，输出
`go version go1.27.3 windows/amd64`。CI 与验收 VM 直接安装 1.27.3，不依赖自动下载。

## 2. Go 模块（P01.2 起写入 go.mod）

**P01.1 实际引入 0 个第三方模块**：骨架、配置、结构化日志、HTTP 信封、版本、
Unix socket 全部用标准库实现（决策见 DECISIONS Q15）。

**P01.2 已按本表精确版本首次写入 `go.mod` + `go.sum`**（`go.sum` 134 行，
`go mod verify` 全部校验通过，证据 `docs/evidence/P01/2026-10-20-p01.2/dependency-resolution.txt`）：

- 直接依赖 2 个：`github.com/gin-gonic/gin v1.12.0`、`modernc.org/sqlite v1.60.1`。
- 其余 34 行为 Gin/SQLite 的传递依赖（`// indirect`），包括 `quic-go`、`mongo-driver`、
  `go-playground/validator` 等 Gin 的可选特性依赖。这不是本项目挑选的清单，
  是 `go mod tidy` 按 Gin 的 import 图算出来的结果；记录在此以便供应链审计。

**P02.1 起直接依赖增加到 4 个**（认证与会话）：新增 `golang.org/x/crypto v0.58.0`
（Argon2id）与 `golang.org/x/term v0.47.0`（`init-admin` 的无回显密码输入），
两者都按冻结版本 `go get <module>@<version>` 引入，未使用 `replace`。

**解析结果（2026-10-20，`go list -m all` 实读）**：
`golang.org/x/crypto v0.58.0`、`golang.org/x/term v0.47.0`、`golang.org/x/sys v0.49.0`、
`golang.org/x/net v0.60.0`、`golang.org/x/text v0.43.0`。

**与 Q19 的差异已消除**：Q19 记录过 x/crypto 与 x/sys 因“仅传递依赖”停留在 v0.48.0；
P02.1 直接 import 后按 MVS 抬到表中的 v0.58.0 / v0.49.0，x/net 与 x/text 也随之升到
v0.60.0 / v0.43.0。`golang.org/x/term` 冻结表里此前没有条目，本次作为**新增直接依赖**
登记（用途：终端无回显读密码），版本 v0.47.0 由 `go get golang.org/x/term@v0.47.0` 确定。

**P10.1 起直接依赖增加到 5 个**（反向代理的域名归一）：`golang.org/x/net v0.60.0`
由传递依赖升为**直接依赖**，只用到 `golang.org/x/net/idna`。版本没有变化——它本来就是
Gin 图里的 v0.60.0，本次只是 `go.mod` 里去掉 `// indirect` 标记，`go.sum` 未新增条目。
理由：域名要落成文件名与 Nginx 配置（`upanel-<domain>.conf`），必须做 IDNA 到 A-label
的归一，这件事没有标准库实现；自己写 Punycode 属于自造密码学风险的同类错误。

下表标注为已引入的模块已写入 `go.mod`，其余模块尚未引入，仍属计划。

| 模块 | 冻结版本 | 上游发布时间 | 用途 |
|---|---|---|---|
| github.com/gin-gonic/gin | v1.12.0 | 2026-02-28 | HTTP 框架 |
| modernc.org/sqlite | v1.60.1 | 2026-09-29 | 纯 Go SQLite 驱动（不用 cgo） |
| github.com/gorilla/websocket | v1.5.3 | 2024-06-14 | 终端 WebSocket |
| github.com/creack/pty | v1.1.24 | 2024-10-31 | PTY |
| golang.org/x/crypto | v0.58.0 | 2026-10-09 | Argon2id（`crypto/argon2` 之外不自行实现密码学） |
| golang.org/x/sys | v0.49.0 | 2026-10-06 | `openat2` / `SO_PEERCRED` 等系统调用 |
| golang.org/x/term | v0.47.0 | 2026-10-08 | `upanelctl init-admin` 终端无回显读密码（P02.1 新增直接依赖） |
| golang.org/x/net | v0.60.0 | 2026-10-08 | 域名 IDNA 归一（`golang.org/x/net/idna`，P10.1 升为直接依赖） |
| github.com/google/uuid | v1.6.0 | 2024-01-23 | UUID v4 主键 |

核实方式：`GET https://proxy.golang.org/<module>/@latest` 与 `@v/<version>.mod` 读取 `go` 指令。

**`go.sum` 状态：已生成（2026-10-20，P01.2）**。策略：`go get <module>@<本表版本>` +
`go mod tidy`，提交 `go.mod` + `go.sum`；此后构建默认 `-mod=readonly`，`go.sum` 缺失即构建失败，
不允许用 `GOFLAGS=-mod=mod` 绕过。`go.mod` 中不得出现 `replace` / `exclude` / 手写 `toolchain`。

## 3. 前端依赖（已写入 web/package.json 并生成锁文件）

生产依赖：

| 包 | 冻结版本 | 上游发布时间 | 用途 |
|---|---|---|---|
| vue | 3.5.44 | 2026-10-13 | 前端框架 |
| vue-router | 5.4.0 | 2026-10-07 | 路由 |
| pinia | 4.1.0 | 2026-10-14 | 状态 |
| naive-ui | 2.45.3 | 2026-08-27 | 唯一 UI 组件库 |
| echarts | 6.1.0 | 2026-05-19 | 监控图表 |
| @xterm/xterm | 6.0.0 | 2025-12-22 | 终端渲染 |
| @xterm/addon-fit | 0.11.0 | 2025-12-22 | 终端尺寸适配 |

开发依赖：

| 包 | 冻结版本 | 上游发布时间 | 用途 |
|---|---|---|---|
| vite | 8.3.4 | 2026-10-08 | 构建/开发服务器 |
| @vitejs/plugin-vue | 6.0.10 | 2026-10-10 | SFC 编译 |
| typescript | 5.9.3 | 2025-09-30 | 类型检查（见下方说明） |
| vue-tsc | 3.3.13 | 2026-10-10 | Vue 类型检查 |
| vitest | 5.0.5 | 2026-10-15 | 单元测试 |
| @vue/test-utils | 2.5.1 | 2026-09-16 | 组件测试 |
| @playwright/test | 1.64.1 | 2026-10-14 | 浏览器 E2E |
| @types/node | 22.20.5 | 2026-10-01 | 与 Node 22 主版本对齐 |
| eslint | 10.13.0 | 2026-10-16 | Lint |
| typescript-eslint | 8.72.0 | 2026-10-19 | TS 规则 |
| prettier | 3.9.10 | 2026-10-10 | 格式化 |
| @redocly/cli | 2.65.0 | 2026-10-19 | OpenAPI 校验 |

**TypeScript 不用最新的 7.0.2**：`typescript-eslint@8.72.0` 的 peer 范围是
`typescript >=4.8.4 <6.1.0`，与 7.x 不兼容。冻结 5.9.3（5.x 最新），
`vue-tsc@3.3.13`（peer `>=5.0.0`）与 `pinia@4.1.0`（peer `>=5.6.0`）均满足。
升级 TypeScript 前必须先确认 lint 工具链支持范围，见 DECISIONS Q4。

传递依赖中值得记录的两项：`pinia@4.1.0` 运行时依赖 `nostics@^1.2.0`（已核实为
`vercel-labs/nostics`，维护者 `danielroe`、`posva`、`antfu`，非可疑包），
以及 peer `@vue/devtools-api@8.2.1`（lock 内已解析）。

## 4. 锁文件策略与已核实结果

- 锁文件：`web/package-lock.json`，`lockfileVersion: 3`，267 个条目，全部来自
  `https://registry.npmjs.org/` 且带 `integrity`。
- 生成方式：Node 22.23.3 / npm 10.9.9，`npm install --package-lock-only --ignore-scripts`。
- 安装方式：`npm ci --ignore-scripts`（CI 与验收环境一致），禁止 `npm install` 改锁。
- 交叉平台可选二进制已包含：`@rolldown/binding-linux-x64-gnu`、`lightningcss-linux-x64-gnu`
  同时存在（vite 8 使用 rolldown + lightningcss）。
- 变更锁文件的唯一合法途径：修改 `web/package.json` 的精确版本 → 本文件登记 → 重新生成并提交。

实际执行与结果（2026-10-20，开发宿主 Windows）：

| 命令 | 结果 |
|---|---|
| `npm install --package-lock-only --ignore-scripts` | exit 0，生成/更新锁文件 |
| `npm ci --ignore-scripts` | exit 0，added 241 packages |
| `npm ls --depth=0` | exit 0，19 个声明依赖全部解析，无 missing/extraneous |
| `redocly lint --format=summary api/openapi.yaml` | exit 0，契约有效；warnings 仅为骨架期未引用的组件 |

## 5. 未验证项（不得当作已验证）

| 项目 | 状态 | 补做方式 |
|---|---|---|
| 在 Ubuntu 22.04/24.04 上执行 `npm ci` 与前端构建 | NOT RUN | P01.3 起在 VM/CI 中执行，记录退出码 |
| 第三方 Go 模块下载与 `go.sum` 生成 | 已验证（2026-10-20） | `go mod download` 退出码 0、`go mod verify` 输出 all modules verified、`go.sum` 134 行；证据 `docs/evidence/P01/2026-10-20-p01.2/dependency-resolution.txt` |
| 交叉编译与 Linux 运行（1.27.3 → linux/amd64） | 已验证（2026-10-20） | 宿主 `GOOS=linux GOARCH=amd64 go build/vet ./...` 退出码 0；产物在 Ubuntu 22.04/24.04 VM 内实际运行（证据 `docs/evidence/P01/2026-10-20/`） |
| VM 内 `go build` / `go test` | NOT RUN | VM 未装 Go（DECISIONS Q9）；需要时补装到 1.27.3 后再记录 |
| 依赖之间的大版本运行时兼容性（vue-router 5 / pinia 4 / vite 8 / vitest 5 / xterm 6） | NOT RUN | 只按 registry 元数据与 peer 范围核实；必须由 P01.3 的真实 typecheck/build 证明 |
| Playwright 浏览器二进制 | NOT RUN | 需要明确执行 `npx playwright install`（CI 与 VM 分开记录） |
| 系统软件包版本（Nginx/Samba/Certbot/restic/smartmontools 等） | NOT RUN | 对应阶段记录 `apt-cache policy` 实际输出 |
| Node 22.23.3 在 Linux 上的 ABI 兼容 | NOT RUN | VM 内使用官方二进制包，安装后 `node -v` 记录 |

## 6. 开发宿主偏差（事实记录）

| 项目 | 宿主实际 | 冻结要求 | 处理 |
|---|---|---|---|
| 操作系统 | Windows 10.0.26300（Git Bash / MSYS） | Ubuntu 22.04/24.04 | 只用于代码与单元测试；Linux 功能验收在 VM |
| Go | 系统安装 1.25.6；`go version` 实读 **1.27.3**（`GOTOOLCHAIN=auto` 获取） | 1.27.3 | 已实测：`go version` → `go1.27.3 windows/amd64`；CI/VM 直接安装 1.27.3 |
| Node | PATH 中的 `C:\Program Files\nodejs` 是 v24.15.0（独立安装）；nvm-windows 另装 v22.23.3 于 `C:\Users\huxi\AppData\Local\nvm\v22.23.3` | 22.23.3 | `nvm use` 无法接管 `C:\Program Files\nodejs`（真实目录，改动需管理员权限）；跑检查时把 nvm 目录前置到 PATH，`scripts/check.sh` 对版本不符直接 FAIL（DECISIONS Q14） |
| npm | 11.12.1（系统） | 10.9.9 | 使用 Node 22.23.3 自带的 npm |
| 虚拟化 | Windows Hypervisor Platform 已启用；便携 QEMU 11.1.0 位于 `H:\Code\.toolchains\qemu`（用户级、免安装、无管理员权限） | 两块可丢弃虚拟数据盘 | **已解决**：使用 `-accel whpx` 创建 VM-A/VM-B，数据盘序列号见 TEST_ENVIRONMENT.md；证据在 `docs/evidence/P00/2026-10-20/` |
| 验收 VM 基础镜像 | Ubuntu cloud image（22.04.5 / 24.04.5），SHA256 校验通过，存于 `H:\Code\vms\images`（仓库外） | Ubuntu 22.04/24.04 server | 已使用；VM 内未装 Go/Node 工具链（P00 不需要，见 DECISIONS Q9） |
