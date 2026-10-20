# P02.1 验收结果（2026-10-20）

阶段：P02.1 —— 通过 `upanelctl` 创建第一个管理员；密码哈希、登录、退出、会话轮换、CSRF、Origin、
限速；无默认密码。

结论：**已实现并通过验收**。宿主（Windows，真实二进制 + 真实 SQLite 文件）与两台真实 Ubuntu VM
（24.04.5 / 22.04.5，非 root 用户运行）全部通过。systemd 托管形态仍未验证（见“未执行”）。

## 已执行并通过（宿主，Windows + Git Bash）

| 检查 | 命令 | 结果 | 证据文件 |
|---|---|---|---|
| 编译 | `go build ./...` | exit 0 | `go-test.txt`（含构建前置） |
| 静态检查 | `go vet ./...` | exit 0 | `go-vet.txt` |
| 格式 | `gofmt -l cmd internal` | 空输出 | `gofmt.txt` |
| 全部测试 | `go test ./...` | 13 包 ok，exit 0 | `go-test.txt` |
| 库文件权限用例 | `go test ./internal/store -run TestOpenTightensDatabaseFileMode -v` | **SKIP**（Windows 的 chmod 不控制权限位；Linux 侧由 VM 验收覆盖） | `store-filemode-test.txt` |
| 哈希耗时 | `TestDefaultPasswordParamsTiming` | 98.7 ms（同机参数对比见 commands.log） | `argon2-timing.txt` |
| 宿主冒烟 | `bash tests/smoke/p02-1-auth-smoke.sh` | **PASS 38 / FAIL 0** | `host-smoke.txt` |
| 环境 | `go version` / `uname -a` / `go env` | 记录在案 | `env.txt` |

## 已执行并通过（真实 Ubuntu VM，非 root 用户 `upanel` / uid 1000）

| 检查 | 环境 | 结果 | 证据文件 |
|---|---|---|---|
| `tests/integration/p02-1-acceptance.sh` | Ubuntu 24.04.5，内核 6.8.0-147-generic | **PASS 58 / FAIL 0** | `vm-2404-acceptance.txt`、`vm-2404-env.txt` |
| `tests/integration/p02-1-acceptance.sh` | Ubuntu 22.04.5，内核 5.15.0-198-generic | **PASS 58 / FAIL 0** | `vm-2204-acceptance.txt`、`vm-2204-env.txt` |

两台 VM 使用同一份交叉编译二进制（`upanel-web` sha256 `f5ae0052…`）与同一份脚本
（sha256 `ac32e472…`），详见证文件里的 `sha256sum` 输出。

VM 上覆盖的验收点（脚本 10 个分组，58 项断言）：

| 分组 | 内容 | 断言数 |
|---|---|---|
| 0 | 非 root 运行；三个二进制可执行；`upanelctl version`；交付的 systemd 单元 `User=upanel-web`、无 `User=root`、无万能 shell | 8 |
| 1 | `init-admin` 退出码/输出/不含口令；两次输入不一致被拒；已有账号时拒绝并说明原因 | 7 |
| 2 | production 模式在 `127.0.0.1:9443` 启动；健康检查 200；Web 进程 uid=1000；库文件属主为运行用户；库文件权限不向 group/other 开放（0600） | 6 |
| 3 | 登录入口：缺 `X-UPanel-Request` 400、非 JSON 400、陌生 Origin 403、缺 Origin `CSRF_FAILED`、未知字段 `VALIDATION_ERROR`、错误密码 401、错误响应不泄露账号是否存在 | 7 |
| 4 | 正确口令 200；Cookie `__Host-upanel_session`/HttpOnly/Secure/SameSite=Strict/Path=//无 Domain；响应不含口令与哈希；拿到 Cookie 与 CSRF | 10 |
| 5 | GET session 200；CSRF 轮换；`reauth_expires_at` 为 null；无 Cookie 401；伪造 Cookie 401 | 5 |
| 6 | **进程重启后同一会话仍有效**（会话在库里而非内存） | 2 |
| 7 | 缺 CSRF 退出 403；正确 CSRF 200；退出清除 Cookie；退出后同 Cookie 401 | 4 |
| 8 | 连续失败触发 429；响应带 `retry_after_seconds` | 2 |
| 9 | 库文件与 `-wal` 中无明文口令、无明文会话令牌、无明文 CSRF 令牌，只有 Argon2id 哈希 | 4 |
| 10 | Web 日志不含口令与 CSRF 令牌，但仍记录了登录请求 | 3 |

## 未执行（NOT RUN）

- **systemd 托管形态**：单元只做了静态断言（`User=upanel-web`、无 root、无万能 shell），
  没有在 VM 上用 systemd 真实启动 `upanel-web`。单元里的用户/组/`StateDirectory` 与
  `/run/upanel/agent.sock` 一起在 P02.3 落地，届时才有安装级证据。
- **空闲/绝对超时实等**：30 分钟空闲与 12 小时绝对上限只在宿主用可控时钟的单元测试里验证，
  VM 上不等待真实时间。
- 浏览器侧：P02.1 不包含前端登录页（STAGES.md 未要求），无真实浏览器证据；`Secure` Cookie
  在明文回环下被 Firefox/Safari 接受与否未知（DECISIONS Q28）。
- TLS（P10）后的 Cookie/Origin 行为；再认证（P02.2）；TOTP（P14）；审计事件（P02.2）。

## 本轮修复的真实缺陷（均由检查暴露，不是推测）

1. `decodeHash` 把 PHC 的 `v=19` 整段当作数字解析，导致本包自己生成的哈希永远无法校验——
   真实登录会 100% 返回“密码哈希格式无效”。用例：`TestHashPasswordRoundTrip`、
   `TestPasswordParamsParsing`。
2. `Authenticate` 把 TEXT 列（`created_at` / `updated_at`）直接 Scan 进 `time.Time`，
   会话校验必然 500。用例：`TestLoginIssueAndAuthenticate`。
3. **VM 复跑暴露**：SQLite 按 umask 建库，VM 上实测权限为 `0644`，同机其他用户可读到口令哈希
   与会话哈希。现在 `store.Open`（校验 PRAGMA 后）与迁移结束时都把库文件及存在的 `-wal`/`-shm`
   收到 `0600`（DECISIONS Q30）。修复后两台 VM 复跑 FAIL 0。
