# P02.2 证据：admin/viewer、账号管理、最后管理员保护、再认证、审计事件起步

日期：2026-10-20
子任务：P02.2（STAGES.md §4）
结论：**通过**。宿主冒烟 81 项、两台 Ubuntu VM 验收各 99 项、P02.1 回归各 58 项全部通过。

## 环境

| 项目 | 值 |
|---|---|
| 开发宿主 | Windows + Go 1.27.3、Node 22.23.3（PATH 前置 nvm 目录） |
| VM-B | Ubuntu 24.04.5 LTS，内核 6.8.0-147，SSH `127.0.0.1:2222`，用户 `upanel`（uid 1000，passwordless sudo，验收脚本以非 root 运行） |
| VM-A | Ubuntu 22.04.5 LTS，内核 5.15.0-198，SSH `127.0.0.1:2223`，用户同上 |
| 二进制 | 宿主交叉编译 `GOOS=linux GOARCH=amd64`，两台 VM 使用同一份（sha256 见 `vm-env-host-build.txt`） |

## 文件清单

| 文件 | 内容 |
|---|---|
| `host-build.txt` | go build / go vet / gofmt 结果（全部干净） |
| `go-test.txt` | `go test ./... -count=1`：14 个包 ok（api 34 / auth 44 / audit 4 顶层用例） |
| `check.txt` | `sh scripts/check.sh`：PASS 12 / FAIL 0 / NOT RUN 0 |
| `host-smoke.txt` | `tests/smoke/p02-2-account-smoke.sh`：PASS 81 / FAIL 0 |
| `host-smoke-web.log` | 同一次冒烟的 Web 访问日志（用于“日志不含秘密”断言） |
| `vm-2404-env.txt` / `vm-2204-env.txt` | os-release、内核、python3/curl 版本、二进制与脚本 sha256、`upanelctl version` |
| `vm-2404-acceptance.txt` / `vm-2204-acceptance.txt` | `tests/integration/p02-2-acceptance.sh` 完整输出：两台均 PASS 99 / FAIL 0 |
| `vm-2404-p021-regression.txt` / `vm-2204-p021-regression.txt` | P02.1 验收脚本回归：两台均 PASS 58 / FAIL 0 |
| `vm-env-host-build.txt` | 宿主侧交叉编译产物与两个脚本的 sha256（与 VM 上 `sha256sum` 一致） |
| `upanel-web.service` | 验收时静态检查的 systemd 单元副本（`User=upanel-web`、无 `User=root`） |
| `commands.log` | 实际执行过的命令序列 |

## 已验证（摘要）

宿主：

- 构建/静态检查/全量单测/`scripts/check.sh` 全绿；`openapi.yaml` 通过 redocly lint。
- 冒烟脚本按契约逐条断言：未登录 401；viewer 对 `/users` 五条路径与 `/auth/reauth` 全 403；
  创建 201 且不回显口令；重复用户名 422；SQL 注入形状用户名、未知字段、未知角色、弱口令 400；
  缺 CSRF 403；列表脱敏（无 password/argon2/csrf/reauth/session_hash）且 `next_cursor` 显式为 null；
  `limit=1` 翻页 + 游标不含明文用户名 + 坏游标 400；未再认证 PATCH 403 `REAUTH_REQUIRED`；
  错误口令再认证 403 且不产生窗口；正确口令 200 并返回 `reauth_expires_at`；totp 400；
  PATCH 后 version 自增、过期 version 409、无字段 400；改自己角色/禁用自己 403；
  改密后目标旧会话 401、旧口令 401、新口令 200；删除后会话 401、重复删除 404；
  审计四类动作齐全、`outcome=success`、target 带对象类型、明细无口令；
  `UPDATE`/`DELETE` 被触发器拒绝；库文件与日志无明文口令。

真实 Ubuntu VM（两台一致）：

- 非 root 运行（uid 1000）；production 在 `127.0.0.1:9443` 启动；库与 `-wal`/`-shm` 权限 0600、
  属主为运行用户。
- 99 项断言覆盖上面全部接口行为，另加：`display_name` 中文原样保存（UTF-8 往返）、
  最后一个可用管理员在自我修改被拒后仍然可用、审计 `APPEND_ONLY=YES`
  （用 python3 sqlite3 直连执行 UPDATE 与 DELETE 均被 `audit_events` 触发器以“只追加”拒绝）。
- P02.1 的 58 项认证断言在新增路由与审计表之后仍全部通过（回归）。

## 过程中发现并修复的缺陷

1. **`POST /api/v1/users` 缺 CSRF 校验**（真实缺陷，Q36）。路由注册时漏了 `requireCSRF`，
   导致“管理员会话 + 正确 Origin + 不带 CSRF 令牌”能一路走到业务层（返回 422 而不是 403）。
   已补上并新增 `TestUserWritesRequireCSRF`，冒烟脚本里“缺 CSRF → 403”这条断言从此是回归护栏。
2. **账号列表缺 `next_cursor` 且不分页**（契约不一致）。CONTRACTS §1 规定列表统一为
   `{"items":[…],"next_cursor":…}`、`limit` 默认 50 上限 200、cursor 不透明；实现补齐了
   `limit`/`cursor` 与显式 `next_cursor`（Q35）。

两次修复之后两台 VM 复跑均为 FAIL 0。

## 未验证（NOT RUN）

- 前端账号管理页面：本子任务不含 UI（STAGES.md P02.2 未要求），因此没有浏览器证据；
  加载/空/无权限状态与再认证弹窗随对应 UI 子任务补齐。
- systemd 真实托管：仍只有静态检查，真实托管与 agent socket 一起在 P02.3 落地。
- 再认证窗口的时间到期：只在宿主可控时钟单测里验证（5 分钟窗口的边界），VM 上没有实等。
- 多管理员真实并发写：409 只有顺序化的单测断言，没有并发压测。
- 审计与业务同事务、系统性日志脱敏：P02.4。

## 复现

```sh
# 宿主
sh scripts/check.sh
sh tests/smoke/p02-2-account-smoke.sh

# VM 内（先把交叉编译产物放到 /tmp/upanel-bin）
BIN_DIR=/tmp/upanel-bin sh tests/integration/p02-2-acceptance.sh
BIN_DIR=/tmp/upanel-bin sh tests/integration/p02-1-acceptance.sh   # 回归
```
