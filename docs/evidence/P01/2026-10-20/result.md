# P01.1 验收结果（2026-10-20）

子任务：**P01.1 — 目录结构、两个服务入口与 CLI 入口；配置解析、结构化日志、版本、
live health；Linux 与 mock 实现分离**（STAGES.md 第 34 行）。

判定：**P01.1 完成**。P01 阶段整体仍为 `in_progress`（P01.2/P01.3/P01.4 未做）。

复现脚本（均已入库）：
- `tests/integration/p01-1-acceptance.sh` — 在 Ubuntu VM 内运行，26 项断言。
- `tests/integration/p01-1-host-refusal.sh` — 在 Windows 宿主运行，16 项断言（拒绝与契约）。

---

## 1. 已实现（代码已写好，且在本记录第 2 节的范围内跑过）

| 组件 | 文件 | 行为 |
|---|---|---|
| 版本 | `internal/version/version.go` | `0.0.0-dev` / `unknown` 占位，可被 `-ldflags -X` 注入；`Info` 输出 snake_case JSON |
| 平台分离 | `internal/platform/{platform,platform_linux,platform_other}.go` | 构建标签区分；非 Linux 上 `CheckMode(production)` 返回 `ErrProductionUnsupported`；`CheckAgent()` 要求 `GOOS=linux` 且 euid 0 |
| 日志 | `internal/logging/logging.go` | `log/slog`，json/text 两种 handler；未知级别或格式直接报错，不降级 |
| HTTP 契约 | `internal/httpx/httpx.go` | `{"data":…,"request_id":…}` / `{"error":{"code","message","details"},"request_id":…}`；合法入站 `X-Request-ID` 回显，否则随机 16 字节 hex；`no-store`、`nosniff` |
| 配置 | `internal/config/config.go` + `defaults_linux.go` + `defaults_other.go` | JSON 严格解码（未知字段、尾部多余内容均报错）；默认 `127.0.0.1:9443` 与 `/run/upanel/agent.sock`；mock 强制 `127.0.0.1:18881`；shutdown 1—120 秒 |
| Web 入口 | `internal/api/server.go` + `cmd/upanel-web/main.go` | `GET/HEAD /api/v1/health/live`；其余方法 405 + `Allow`；未知路径 JSON 404；`net.Listen` 成功后才报"已开始监听"；优雅停止；**TLS 就绪前 production 拒绝非回环绑定** |
| 代理入口 | `internal/agent/server.go` | root 校验；socket 目录必须已存在；同名非 socket 文件拒绝覆盖；`0660`；仅 `GET /internal/v1/capabilities`，两个数组为空；停止时删除 socket |
| 可执行程序 | `cmd/upanel-web` / `cmd/upanel-agent` / `cmd/upanelctl` | 退出码 0 成功 / 1 拒绝或运行错误 / 2 配置或参数错误 / 3 CLI 未实现；`upanelctl` 提供 `version`、`check-config`，其余子命令报"尚未实现（计划阶段 …）" |
| 构建边界 | `web/go.mod` | 阻止根模块 `go build ./...` 递归扫描 `web/node_modules` 中的第三方 Go 源码 |

第三方 Go 模块：**0 个**（P01.1 全程标准库）。Gin 与 SQLite 按计划在 P01.2 引入。

## 2. 已验证（有真实命令输出与退出码）

证据文件全部在 `docs/evidence/P01/2026-10-20/`。

### 2.1 开发宿主（Windows 11 build 26300）

`sh tests/integration/p01-1-host-refusal.sh` → **PASS 16 / FAIL 0，退出码 0**：

- `--mode production` 退出码 1，消息明确"production 模式仅支持 Linux…请显式使用 --mode mock"。
- `--mode production --listen 0.0.0.0:9443` 退出码 2（TLS 前禁止明文暴露，见第 5 节第 1 条）。
- `upanel-agent.exe` 退出码 1；`upanel-agent.exe --mode mock` 退出码 2（代理无 mode 参数）。
- mock：health 200 + 契约信封；`X-Request-ID` 同时出现在响应头与 body；`/` 与 `/index.html`
  均 404 JSON；POST 405；`--listen 0.0.0.0:18881` 退出码 2；SIGTERM 退出码 143，端口已释放。
- `upanelctl check-config` 正常配置退出码 0；显式路径不存在退出码 1。

静态检查：`go build` / `go vet` / `go test`（10 个包）/ `gofmt -l` 全部 0 或无输出；
`GOOS=linux GOARCH=amd64 go build ./...` 与 `go vet ./...` 均为 0（静态链接 ELF）；
`sh scripts/check.sh`（PATH 前置 .nvmrc 的 Node 22.23.3）→ **PASS 8 / FAIL 0 / NOT RUN 4**，退出码 0
（NOT RUN 的四项是 web-typecheck/lint/test/build，P01.3 才建立脚本）。

### 2.2 Ubuntu 22.04.5（内核 5.15.0-198）与 Ubuntu 24.04.5（内核 6.8.0-147）

同一份脚本 `tests/integration/p01-1-acceptance.sh`，两份独立输出，
**两台均 PASS 26 / FAIL 0，脚本退出码 0**：

| 检查组 | 断言 |
|---|---|
| CLI | `upanelctl version`；`check-config` 读出 9443 与 `/run/upanel/agent.sock`；含未知字段的配置退出码 1 |
| mock | 只监听 `127.0.0.1:18881`；health 契约信封；`/` 404 JSON；SIGTERM 退出码 0；`--listen 0.0.0.0:18881` 退出码 2 |
| production | 以 uid 1000(upanel) 运行、监听 `127.0.0.1:9443`；`X-Request-ID: acceptance-rid-1` 原样回显；POST 405；SIGTERM 退出码 0；`--listen 0.0.0.0:9443` 退出码 2 |
| 代理 | 非 root 运行退出码 1；root 运行后 socket 为 `socket srw-rw----`；`ss -ltnp` 中代理 TCP 监听数为 0；capabilities 含 `"supported_actions":[]` 与 `"mode":"production"`；未知内部路径 404；SIGTERM 后 socket 被删除；同名普通文件存在时退出码 1 且文件保留 |

VM 内 `/tmp` 下的三个二进制与仓库 `bin/linux-amd64` 逐一 sha256 比对一致（见 `env.txt`）。

### 2.3 阶段验收条款对照（STAGES.md P01）

| 条款 | 结果 | 依据 |
|---|---|---|
| production 不得连到 mock | 已验证 | 配置与 CLI 都无回退路径；mock 端口有硬校验 |
| 非 Linux 的 production 必须显式拒绝启动 | 已验证 | 宿主退出码 1 + 明确消息；单元测试 `main_nonlinux_test.go` |
| mock 只绑 127.0.0.1:18881 | 已验证 | 两台 VM `ss -ltn` 实读；非回环地址退出码 2 |
| 未知 API 不得返回 index.html 或 200 | 已验证 | `/`、`/index.html`、`/api/v1/unknown`、`/api/v2/health/live` 全部 404 JSON |
| 依赖缺失不得伪装成正常 | 已实现，**未验证** | 当前无外部依赖；P01.2 引入 SQLite 后才具备可验证点 |

## 3. 未验证（NOT RUN，不声明通过）

- **真实部署形态**：未写 systemd 单元、未创建 `upanel-web` 系统用户、未接 Nginx / TLS（P01.4、P10）。
- **TLS**：Web 只提供回环明文 HTTP；production 已拒绝非回环绑定，TLS 在 P10 接入。
- **`/api/v1/health/live` 之外的路由**：尚不存在，P01.2 起建立。
- **代理的 SO_PEERCRED 校验、动作注册、鉴权**：未实现（P02.3）。
- **前端**：typecheck / lint / test / build 全部 NOT RUN（P01.3）。
- **CI**：仓库未初始化 Git，工作流未在远端执行（DECISIONS Q6）。
- **VM 内构建**：VM 无 Go/Node，验收使用交叉编译产物（DECISIONS Q9）。
- **并发/长时间运行**：只做启动、请求、停止冒烟，无 soak 与压测（P16 范围）。
- 宿主结果只证明拒绝逻辑与契约，不能代替 Ubuntu 可用性证据。

## 4. 受阻

无。P01.1 不依赖任何尚未具备的环境。

## 5. 本轮发现并处理的问题

1. **TLS 未接入时的明文暴露风险**：首版只对非回环监听打警告。改为在 production 下
   **拒绝非回环地址（退出码 2）**，message 指向 P10。已补单元测试
   `TestProductionRefusesNonLoopbackBeforeTLS`，并在两台 VM 与宿主各加了一条断言。
2. **验收脚本首轮误用配置**：mock 段复用了带 `listen_address: 127.0.0.1:9443` 的配置文件，
   被 `ResolveListen` 按设计拒绝（退出码 2），首轮出现 5 项 FAIL。已改为 mock 使用独立配置。
   这不是产品缺陷，而是"mock 不得静默改写端口"的正确表现。
3. **`go.mod` 注释与事实不符**：原注释称"业务依赖在 P01.1 首次导入"，实际零第三方依赖。
   已改为"P01.1 只用标准库，P01.2 首次导入"。
4. **宿主 Node 版本闸门**：PATH 中 `C:\Program Files\nodejs` 是独立安装的 v24.15.0，
   nvm-windows 无法接管该路径。本轮 `nvm install 22.23.3` 并在运行 check.sh 时把它前置到 PATH；
   未前置时 check.sh 如实 FAIL，说明闸门有效（DECISIONS Q14）。
5. **`X-Request-ID` 响应头大小写**：Go 把响应头名规范化为 `X-Request-Id`。保持 `Set()` 行为
   （HTTP 头名大小写不敏感），在代码注释里记录取舍，不为字面一致去直接写 map 破坏标准库查找。
6. **VM 内 `chmod +x` 遗漏**：一轮分发后 `upanelctl` 缺执行位，脚本如实报"缺少可执行文件"。
   重新赋权后复跑通过；这属于分发步骤疏漏，不影响产品代码。

## 6. 证据清单

| 文件 | 内容 |
|---|---|
| `env.txt` | 宿主与两台 VM 的系统/内核/CPU/内存/块设备序列号、交叉编译产物 sha256 |
| `host-verification.txt` | 宿主 16 项断言逐条 PASS（拒绝启动、mock 契约、CLI），退出码 0 |
| `vm-2404-acceptance.txt` | Ubuntu 24.04.5 上 26 项断言逐条 PASS，退出码 0 |
| `vm-2204-acceptance.txt` | Ubuntu 22.04.5 上 26 项断言逐条 PASS，退出码 0 |
| `check-run.txt` | `sh scripts/check.sh` 全量输出，PASS 8 / FAIL 0 / NOT RUN 4，退出码 0 |
| `commands.log` | 实际执行过的命令与退出码位置 |

清理：两台 VM `systemctl poweroff` 退出码 0；宿主 qemu 进程数 0；seed 服务已停止；
端口 2222/2223/8899/8900 全部关闭。
