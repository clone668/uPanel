# P01.2 结果报告（2026-10-20）

子任务：SQLite 迁移机制、事务工具、错误响应、request_id、中间件与 API 路由组织。

结论：**已实现并已验证**（宿主 + Ubuntu 22.04/24.04）。无阻塞项。
以下按“已实现 / 已验证 / 未验证 / 受阻”分开写，不把未跑的东西算进通过。

## 1. 已实现（代码在仓库，能构建、能运行）

- **数据库打开**：`database/sql` + `modernc.org/sqlite`（纯 Go，无 cgo）。打开时从连接**读回**
  `PRAGMA journal_mode` / `foreign_keys` / `busy_timeout` 并逐项校验，不符即失败；日志写的是读回值。
- **路径规则**：绝对路径、不含 `?`/`#`、数据目录必须已存在（不由代码创建）。
- **迁移**：`migrations/NNNN_name.sql` 经 `embed.FS` 编进二进制；文件名格式、重复序号、空文件都校验；
  每条迁移一个事务、升序应用；已应用迁移的 SHA-256 存入 `schema_migrations` 并在每次启动时复核；
  迁移文件被改或删即拒绝；数据库版本高于程序内置版本即拒绝（只前滚）。
- **事务**：`store.WithTx`，错误与 panic 都回滚，panic 继续上抛。
- **错误响应**：24 个契约错误码为封闭枚举 + 状态码映射；未知码按 500，不猜 400；
  统一信封与 `X-Request-ID`、`Cache-Control: no-store`、`X-Content-Type-Options: nosniff`。
- **request_id**：中间件入口决定一次（合法入站沿用，非法丢弃重生成），上下文传递，
  响应头 / 响应体 / 访问日志同值。
- **中间件链**：requestID → accessLog → recovery → bodyLimit(1 MiB)。panic → 500 `INTERNAL_ERROR`，
  堆栈只进日志；超限体可由 `IsBodyTooLarge` 映射 413。
- **路由（Gin）**：关闭尾斜杠与 fixed-path 重定向；NoRoute → JSON 404；NoMethod → 405 + `Allow`；
  不预埋 501 空路由。
- **CLI**：`upanelctl migrate [--config|--db|--json]`，退出码 0/1/2 语义明确；
  `check-config` 增加 `database_path`、`database_busy_timeout_ms`。
- **启动拒绝**：Web 在数据库打不开或迁移失败时以退出码 1 拒绝启动，且不进入监听。
- **配置**：新增 `database` 段（`path`、`busy_timeout_ms` 100—60000，默认 5000），Linux 默认
  `/var/lib/upanel/upanel.db`，非 Linux 无默认值。
- **契约文档**：`api/openapi.yaml` 登记 `GET/HEAD /api/v1/health/live`；
  `INTERNAL_ERROR` 作为枚举外例外在描述中写明。

## 2. 已验证（有可复现的证据，证据文件同目录）

| 项目 | 结果 | 证据 |
|---|---|---|
| `go build ./...`、`go vet ./...`、`gofmt -l` | 退出码 0 / 输出为空 | commands.log |
| `go test ./... -count=1` | 11 个包 ok，`upanel/migrations` 无测试 | go-test.txt |
| 新增测试逐项 | 63 PASS / 0 FAIL | go-test-verbose.txt |
| `scripts/check.sh` | PASS 8 / FAIL 0 / NOT RUN 4（NOT RUN = 前端四项，P01.3 才有脚本） | check-run.txt |
| 宿主拒绝/契约回归 | PASS 16 / FAIL 0 | host-refusal.txt |
| `go mod download` / `go mod verify` | 退出码 0 / all modules verified | dependency-resolution.txt |
| Ubuntu 24.04.5 P01.2 | PASS 34 / FAIL 0 | vm-2404-p01-2.txt |
| Ubuntu 22.04.5 P01.2 | PASS 34 / FAIL 0 | vm-2204-p01-2.txt |
| Ubuntu 24.04.5 P01.1 回归 | PASS 26 / FAIL 0 | vm-2404-p01-1-regression.txt |
| Ubuntu 22.04.5 P01.1 回归 | PASS 26 / FAIL 0 | vm-2204-p01-1-regression.txt |
| 二进制一致性 | 两台 VM 内 `/tmp` 三个文件 sha256 与 `dist/p01-2/` 完全相同 | env.txt |

VM 上真实证明到的行为：真实文件建库与 SQLite 文件头、数据库文件归属运行用户（非 root）、
再次迁移不重复应用、启动日志 `journal_mode=wal` / `foreign_keys=true` / `busy_timeout_ms=5000`、
迁移状态跨进程重启保留（`applied_versions=[]`）、`-wal` 文件存在、目录缺失与目录不可写
（root 0500）都退出码 1 且不监听、非 SQLite 随机文件迁移失败且不输出成功信息、
相对路径 / 空路径按用法错误处理。

## 3. 未验证（明确不声明通过）

- 真实版本升级路径：当前只有 `0001`，多步序列与校验和异常由单元测试用临时迁移集证明，
  未在 VM 上用“发布 0002 后再升级”验证。
- 迁移中途被 SIGKILL / 断电的恢复：只验证了失败迁移整条回滚，没做杀进程实验。
- 多进程并发写同一数据库时的 `busy_timeout` 实际行为：未压测。
- 磁盘满、只读文件系统、NFS/overlay 等非 ext4 位置上的 WAL 行为：未测。
- systemd 单元与 `upanel-web` 系统用户的真实部署（P01.4）：VM 内是普通用户直接跑二进制。
- Nginx 与 TLS（P10）：Web 仍是回环明文 HTTP。
- 前端 typecheck / lint / test / build（P01.3）。
- CI 远端执行（仓库未初始化 Git）。
- VM 内构建（VM 无 Go/Node，用交叉编译产物）。

## 4. 受阻

无。

## 5. 本轮发现的问题与处理

1. `ErrAlreadyWritten` 定义后从未使用 → 删除，避免“看起来有机制其实是死代码”。
2. `store.Open` 日志原本写死 `journal_mode=wal` / `foreign_keys=true` → 改为记录**读回的实际值**，
   让日志能当证据用（校验失败时根本不会走到这行日志）。
3. P01.1 两个验收脚本在数据库成为硬依赖后会失败（Web 按设计拒绝启动）→ 脚本补 `--db` 并注明原因；
   重跑回归 26/26、16/16。产品行为未变。
4. 新脚本首轮 1 项 FAIL 是断言写法错误（JSON 里的 `[` 被转义）→ 修正后重跑，最终证据 34/34。
5. `go mod tidy` 解析出的传递依赖 `golang.org/x/crypto v0.48.0`、`x/sys v0.48.0` 低于
   DEPENDENCIES.md 冻结表的 v0.58.0 / v0.49.0 → 不改 `go.sum`、不写 `replace`，
   在 DEPENDENCIES §2 与 DECISIONS Q19 如实记录，首次直接 import 时再抬高。
6. `INTERNAL_ERROR` 与 405/500 不在 CONTRACTS 的状态码枚举里 → 登记为 DECISIONS Q17/Q18，
   并在 openapi.yaml 写明，不擅自扩张契约。

## 6. 下一步

P01.3：前端骨架（Vue 3 + TypeScript 严格、Vite、Vue Router、Pinia、Naive UI、中文导航、
统一 API 客户端、错误展示组件、mock 模式持续显示的“演示数据”横幅）。本轮不开始。
