# P06.3 验收结果 — 受限 Web 终端（PTY/upanel-shell、两跳 WebSocket、四道门、许可、超时、会话元数据审计）

- 阶段：P06（文件管理与受限终端）/ 子任务 P06.3
- 日期：2026-10-21
- 结论：**P06.3 passed**。Ubuntu 24.04.5 真机断言 **PASS=282 / FAIL=0 / NOT RUN=0**，
  VM 内核原生套件 **PASS=962 / FAIL=0**，宿主 `check.sh` **PASS 13 / FAIL 0 / NOT RUN 0**，
  前端 **445** 用例全过，`api/openapi.yaml` valid（7 warning，第 7 条是 101-only 操作的
  `operation-2xx-response`）。
- 这一轮交付的是**能力最小、边界最硬的终端**：一个账号最多 1 个会话、全局 2 个，
  单连接、无 tmux 式持久化、无录像、无命令接口（只有字节），PTY 在 `exec` 之前就降权到
  `upanel-shell`，并且**会话结束时 shell 真的会死**（不是只从会话表里消失）。

---

## 1. 本轮做了什么

`STAGES.md` §8 P06.3：“以 `upanel-shell` 启动 PTY、WebSocket、再认证、Origin、连接许可、
窗口调整和超时；审计会话元数据。”

### 1.1 两跳链路

```
浏览器 ──WS(子协议 upanel.term.v1 + upanel.grant.<id>)──► Web ──WS(upanel.term.v1)──► 代理 ──PTY──► /bin/bash -i (upanel-shell)
```

- 浏览器连的是 **Web**，Web 再连**代理**：浏览器永远拿不到代理 socket，也不知道代理的存在。
- Web 不解析终端内容，只在两条 WS 之间搬字节；帧的合法性由**代理**判定（Web 只做长度与形态的
  前置校验，避免把垃圾送进代理）。
- 子协议按契约固定：`Sec-WebSocket-Protocol` 必须**同时**带上 `upanel.term.v1` 与
  `upanel.grant.<32 位 hex>`；许可只从子协议取，不认 URL 参数、不认 Cookie、不回显在 101 响应里。

### 1.2 四道门（全部在 upgrade **之前**，失败是真正的 JSON 信封）

固定顺序，任何一道不过就不升级、并落一条审计：

| 顺序 | 门 | 失败状态 | 审计 stage |
|---|---|---|---|
| 1 | `Origin` 白名单 | 403 `CSRF_FAILED` | `origin` |
| 2 | 子协议/版本 | 400 `VALIDATION_ERROR` | `protocol` |
| 3 | 一次性许可（30 秒、单次） | 403 `FORBIDDEN` | `grant` |
| 4 | 代理附加会话（不存在/已被连/代理不可用） | 404 / 409 / 503 | `agent` |

“先连代理、再升级浏览器”是刻意的：如果先 `101 Switching Protocols` 再发现代理不认这个会话，
浏览器只能看到一个没有原因的断开，用户看到的会是“终端莫名其妙断线”。

### 1.3 会话是会话，不是任务

- 状态只有两个：`detached` / `attached`。`terminal.create` 的任务在会话建好那一刻就 `succeeded`，
  会话此后的生命周期不进任务中心（任务状态机表达不了“一条连接正在被使用”）。
- 一个会话同一时刻只服务**一条**连接：第二条拿 409 `RESOURCE_BUSY`（两条连接同时写同一个 tty，
  输入会交错，且“谁在敲”再也说不清）。
- 断流≠结束：连接掉了 PTY 与 shell 都留着，宽限期内重连接回**同一个 shell**（导出的变量、
  当前目录都在）。宽限期内没有连接时输出照常读走并丢弃，不做无限攒。
- 时限只能收紧不能放宽（`Limits.Validate`）：空闲 60—900 秒（默认 900）、最长 60—3600 秒
  （默认 3600）、宽限 5—60 秒（默认 60）、每账号 1、全局 2。改配置只能变严，写宽直接拒绝启动。

### 1.4 帧与限额

| 方向 | 帧 | 字段 |
|---|---|---|
| C→S | `input` | `data_b64` |
| C→S | `resize` | `cols`、`rows` |
| S→C | `ready` | `session_id`、`user`、`uid`、`cols`、`rows` |
| S→C | `output` | `data_b64` |
| S→C | `exit` | 退出码 / 信号 / 结束原因 |
| S→C | `error` | 契约错误码 |

- 单帧载荷 64 KiB、帧总长 95573 字节、`cols`/`rows` 1—500、输入 256 KiB/s、200 帧/s；
  越界一律 `FRAME_INVALID` / `RATE_LIMITED` 并断开，不做“截断后继续”。
- 关闭码：`1000` 正常、`4001` 帧非法、`4002` 限速；`1005/1006/1015` 由 `sendableCloseCode`
  拦掉（这三个不能由应用自己发出去）。Web 侧只**镜像**代理给的关闭码，不解析帧。

### 1.5 审计

`terminal.attach`（四道门拒绝、连接建立、连接结束）与 `terminal.close`；`target` 固定
`terminal:<session_id>`（中间件层拒绝也走 `denyTarget`，例外只有列表接口——它的 URL 里没有
session id，`target` 只能是 `user`）；`details` 只有元数据（`origin`、`protocol`、`stage`、
`end_side`、`end_reason`、`duration_ms`、`input_bytes`、`output_bytes`）。**终端全文不进日志、
不进审计**（验收里用一个标记串在审计行、代理日志、Web 日志三处都断言“搜不到”）。

### 1.6 能力软关闭

终端账号不可用（不存在、是 root、nologin、home 属主不对或可写、在特权组里）时，代理**照常启动**，
只是不登记 `terminal.create`（动作）与 `terminal.sessions`（查询），日志里写清原因；
Web 启动时探测一次，拿不到能力就给 501 `NOT_IMPLEMENTED`，前端整页显示“当前实例未启用终端”。

---

## 2. 代码与文件

新增：

| 文件 | 内容 |
|---|---|
| `internal/terminal/{manager.go,manager_linux.go,manager_other.go}` | 会话表、限额、超时扫描、PTY 启动与降权核对（`/proc/<pid>/status` 读回 uid/gid/附加组）、显式挂断 |
| `internal/terminal/{frame.go,http.go,types.go,protocol_test.go,manager_linux_test.go}` | 帧编解码与限额、错误→状态码唯一映射、类型与闭集、原生 PTY 用例 |
| `internal/terminal/remote.go,remote_test.go` | Web 侧终端调用端（本地校验、响应自洽性、拒绝翻译） |
| `internal/agent/terminalaction.go,terminalstream.go` | 代理动作 `terminal.create`、查询 `terminal.sessions`、WS 流（`pumpTerminal`） |
| `internal/agentclient/terminal.go` | Web→代理的终端客户端 |
| `internal/api/{terminal.go,terminalgrant.go,terminalws.go,terminal_test.go}` | 四条 HTTP 接口、一次性许可、WS 中继 |
| `web/src/api/terminal.ts,terminal.test.ts`、`web/src/views/TerminalView.vue` | 前端 DTO、页面（xterm）、六种状态 |
| `tests/integration/p06-3-terminal-acceptance.sh`、`p06-3-ws-client.py` | 真机验收脚本与 WS 客户端 |

修改：`internal/agent/{registry,server,streams,systemquery}.go`（登记表、错误码透传）、
`internal/api/{router,server,users}.go`（路由、`reject`/`denyTarget`）、`internal/audit/audit.go`、
`internal/config/config.go`（终端配置与收紧校验）、`cmd/upanel-{agent,web}/main.go`、
`api/openapi.yaml`（四条接口 + 能力）、`web/src/{api/auth.ts,modules/registry.ts,router/routes.ts}`、
`scripts/run-native-tests.sh`（补执行位）。

---

## 3. 契约与决策

- `docs/CONTRACTS.md` 新增 `### 10.4 受限 Web 终端（P06.3 起）`：两跳图、两状态、409、
  收紧规则、字段表、限额表、关闭码、许可规则、四道门表、错误映射表、幂等 DELETE、
  能力软关闭与启动探测、“明确不提供”清单。本轮又补一段
  **“回收”指 shell 进程真的没了**（关 master 不等于挂断，判定标准是 `/proc/<pid>` 消失）。
- `docs/DECISIONS.md` Q160—Q169：再认证范围、时限只能收紧、能力软关闭与探测、许可放在子协议、
  先连代理再升级、三处“同一规则两种说法”的统一、断开时给关闭码、终端环境里不放自家变量、
  审计目标与内容、**Q169 关 master 不会挂断（PTY 回收缺陷）**。

---

## 4. 真机抓到的缺陷（都改产品，不改断言）

| # | 现象 | 根因 | 修法 |
|---|---|---|---|
| 1 | 领许可/关闭别人的会话返回 503 `AGENT_UNAVAILABLE` | 代理查询层把 `ErrNotFound` 当“执行失败→500”，Web 再把 500 翻成 503 | `terminalRejection` 给查询/关闭的每个错误带上判定，三层说同一个码（Q165） |
| 2 | 关闭一个已经不存在的会话返回 404 | `Manager.Close` 把“目标状态已成立”当失败；别人的会话返回 403，等于承认这个 id 存在 | `Close` 幂等；非属主一律 `ErrNotFound`，与 `Get` 同一条规则（Q165） |
| 3 | 帧非法/超速率时浏览器侧 `closed_by_server=False` | 服务端只 `conn.Close()`，从不发 close 帧 | 代理加 `4001`/`4002`，Web 只镜像代理的关闭码（Q166） |
| 4 | 终端环境里能读到 `UPANEL_TERMINAL=1` | `buildEnv` 多塞了一个“标记变量” | 从白名单删除：终端里的进程和一次 SSH 登录的差别不需要靠环境变量表达（Q167） |
| 5 | 会话已结束，`upanel-shell` 进程还活着、代理还留着 master fd | `ptmx.Close()` 对阻塞 fd 只减引用，真正 `close(2)` 要等读泵的 `read` 返回 → tty 从不释放 → 收不到 SIGHUP | `Close` = 关 master + **显式** `kill(-pid, SIGHUP)`，2 秒不退补 `SIGKILL`；发信号前用 `/proc/<pid>/stat` 的 ppid+starttime 确认真是自己的子进程（Q169） |
| 6 | 中间件层拒绝的终端路由审计 `target='user'` | 拒绝发生在拿到会话 id 之前，统一写了固定的 `user` | `denyTarget`：`terminal.attach|close` 且 `:id` 合法时写 `terminal:<id>`（Q168） |

验收脚本自身的缺陷（**不是**产品问题，同样按“断言只增不减”处理）：CRLF 导致 `sh` 报
`set: Illegal option`；上传的二进制是 0644 没有执行位；`/tmp/upanel-bin` 缺 `o+x` 让
`upanel-web` 穿不过；能力断言把 `terminal.sessions` 拿去 `supported_actions` 里找；
限速用例的填充字节（`x`）落在 PTY 行缓冲里污染后续命令；两个账号的会话状态用同一个
cookie jar 采样；“审计 target 以 `terminal:` 开头”这条断言过宽（把列表接口的 `user`
也算失败）——改成三类行分别精确比对。

---

## 5. 实际执行的检查与证据

| 检查 | 结果 | 证据文件 |
|---|---|---|
| VM P06.3 验收脚本（20 节） | **PASS=282 / FAIL=0 / NOT RUN=0** | `p06-3-acceptance-2404.txt` |
| VM 内核原生套件（13 个测试二进制） | **PASS=962 / FAIL=0**（terminal 33 / api 200 / agent 206 / storage 159 …） | `native-tests.txt` |
| 宿主 `go build/vet`（windows + linux） | 全部 exit=0 | `go-gates.txt` |
| 宿主 `gofmt -l cmd internal` | 无输出 | `go-gates.txt` |
| 宿主 `go test ./... -count=1` | **26 个包 ok / 0 FAIL**（另 3 个包无测试文件） | `go-test.txt` |
| 前端 typecheck / lint / build | exit=0，`TerminalView` 346.92 kB | `web-gates.txt` |
| 前端单测 | **29 文件 / 445 用例 passed** | `web-test.txt` |
| `scripts/check.sh`（Node v22.23.3） | **PASS 13 / FAIL 0 / NOT RUN 0** | `check.txt` |
| `redocly lint api/openapi.yaml` | valid，7 warning | `openapi-lint.txt` |

验收里值得单独点出的断言：

- 身份与边界（一节 16 条）：`id -un` 是 `upanel-shell`、`id -nG` 无
  `root/sudo/docker/disk/adm/wheel/shadow`、`sudo -n true` 失败、`/etc/shadow` 与
  `/etc/upanel/agent.key` 读不到、`/etc/upanel` 写不进、`env` 里没有 `UPANEL_*`。
- 宽限到期（第 13 节）：会话从表里消失 **且** `pgrep -u upanel-shell` 归零（这条是本轮
  第 5 号缺陷的回归断言，改成轮询等待仍然必须归零）。
- 空闲/最长（第 14/15 节）：60 秒无输入被 `idle` 回收；一直有活动也会在 120 秒（配置收紧）
  被 `expired` 回收。
- 数量上限（第 16 节）：每账号第二个会话预检即阻塞（`TERMINAL_SESSION_LIMIT`），全局第三个
  任务失败（`RESOURCE_BUSY`），前两个会话不受影响。
- 审计（第 17 节）：48 条终端审计行；四道门的拒绝各有 `stage`；`terminal.close` 幂等；
  标记串在审计与两份日志里都搜不到；单行 `details` 序列化 < 300 字节。

---

## 6. 未验证（NOT RUN）与原因

| 项 | 原因 |
|---|---|
| 22.04.5（VM-A）上的 P06.3 验收 | 本轮按 P06.1/P06.2 同一口径只在 24.04.5 上执行；PTY/`/proc` 行为在 22.04 上应当一致但**没有实测证据** |
| 真实浏览器里手工敲终端（xterm 渲染、输入法、复制粘贴、移动端软键盘） | 前端只有 vitest 单测 + dist 构建 + 冒烟（其余页面）；终端字节链路由 Python WS 客户端验，不代表浏览器 UI 手感受验 |
| 代理进程在会话进行中被 `kill -9` 时浏览器的表现 | 验收只覆盖“代理正常关闭连接”（审计里 `agent_closed`）；代理爆死时浏览器侧的 close 码/重连提示未断言 |
| `/dev/pts` 耗尽、全局 2 个会话之外的并发压力 | 契约上限就是 2，没有做规模化压力测试 |
| 终端页在屏幕阅读器/键盘导航下的可访问性 | 未执行 |
| 22.04/24.04 之外的内核（`openat2` 只是文件模块约束，终端不涉及） | 不在支持范围 |

## 7. 已知限制（已写进契约，不是缺陷）

- 没有 root/sudo 终端、没有 `run` 帧、没有 `POST /exec`：终端只有字节通道。
- 没有 tmux 式持久化：会话最长 1 小时，进程不跨 Web/代理重启存活。
- 没有终端录像/回放、没有多人共享会话、没有 SFTP（文件走第 11 节契约）。
- 断流期间输出丢弃，不补发（实时通道语义）。

## 8. 下一子任务

P06 三个子任务到此全部 passed（阶段结论见 `docs/PROGRESS.md` §3）。下一个子任务是
**P07.1**：存储动作预检、可靠设备身份、整盘锁、系统设备/挂载/swap/holders/依赖保护——
**只验证拒绝逻辑，不执行任何破坏性动作**（P07 的破坏性验收必须用可丢弃的独立 VM 数据盘）。
