# P03.2 验收结果（2026-10-20）

**阶段**：P03.2 — 持久 jobs、`Idempotency-Key` 幂等链、`operation_id` 去重、代理执行记录与事件、
资源锁与排队、取消规则、按 `operation_id` 对账。

**结论：passed。** 代码与两台 Ubuntu 验收 VM 的行为验收均完成；宿主闸门全绿；
STAGES §5 为 P03 规定的六条验收全部有真实系统证据。P03 整体仍为 in_progress（P03.3/P03.4 未开始）。

- 验收日期：2026-10-20
- 被测代码：工作区当前状态（`version: 0.0.0-p03.2`、`stage: P03.2`）
- VM：`upanel-2404`（`127.0.0.1:2222`，Ubuntu 24.04.5 LTS / 6.8.0-147-generic / 2 vCPU / 3915 MiB）
  与 `upanel-2204`（`127.0.0.1:2223`，Ubuntu 22.04.5 LTS / 5.15.0-198-generic / 2 vCPU / 3915 MiB）

## 1. 交付清单

Web 侧（`upanel-web`，非 root，无 sudo）：

| 文件 | 内容 |
|---|---|
| `internal/api/jobs.go` | `POST /api/v1/jobs`、`GET /api/v1/jobs`、`GET /api/v1/jobs/{id}`、`POST /api/v1/jobs/{id}/cancel` |
| `internal/operations/jobs.go` | 提交编排（Service）：幂等键 → 条件消费预检 → 落 job + 首条事件（**同一事务**）→ 才调代理；对账 `syncFromAgent` |
| `migrations/0005_jobs.sql` | `jobs`、`job_events`、`idempotency_keys`、`preflights.cancellable`/`consumed_job_id` |
| `internal/agentclient/` | `CreateOperation`、`GetOperation`、`CancelOperation`（2xx 语义，真实 handler 端到端用例） |

代理侧（`upanel-agent`，root，仅 Unix socket）：

| 文件 | 内容 |
|---|---|
| `internal/agent/operations.go` | `/internal/v1/operations`（创建/查询/取消）、`operations/{id}/events` 的处理 |
| `internal/agent/runner.go` | 后台执行器：`context.Background`（浏览器关/Web 死都不取消）、执行前再算一次计划复核状态版本、可中断点响应 `ErrCanceled` |
| `internal/agent/locks.go` | 进程内资源锁：锁名排序获取（不死锁）、30 s 排队上限（超时 `RESOURCE_BUSY`） |
| `internal/agentstore/operations.go` | `operations`/`operation_events` 只追加；`operation_id UNIQUE` + `request_hash` 去重；预检一次性条件消费 |

契约：`api/openapi.yaml` 新增 jobs 四条路径与 `Job`/`JobEvent`/`CreateJobRequest` 等 schema。

## 2. 宿主侧结果

| 检查 | 命令 | 结果 | 证据 |
|---|---|---|---|
| 构建 | `go build ./...` | exit 0 | `host-build.txt` |
| 静态检查 | `go vet ./...` | exit 0 | `go-vet.txt` |
| 格式 | `gofmt -l .` | 无输出 | `gofmt.txt` |
| 单元/集成 | `go test -count=1 ./...` | 全 ok | `go-test.txt` |
| 用例计数 | `go test -v ...` | **170 PASS / 0 FAIL** | `go-test-verbose.txt`、`go-test-verbose-summary.txt` |
| 统一闸门 | `sh scripts/check.sh` | **PASS 12 / FAIL 0 / NOT RUN 0** | `check.txt` |
| 契约 lint | `redocly lint api/openapi.yaml` | valid，10 warnings（与改动前同类同量） | `openapi-lint.txt` |

## 3. VM 侧结果（两台完全一致）

| 脚本 | 24.04.5 | 22.04.5 | 证据 |
|---|---|---|---|
| `p03-2-jobs-acceptance.sh` | 通过 101 / 失败 0 | 通过 101 / 失败 0 | `vm-upanel-{2404,2204}-p032-acceptance.txt` |
| `p01-1-acceptance.sh` | 29 / 0 | 29 / 0 | `vm-upanel-*-p011.txt` |
| `p01-2-acceptance.sh` | 34 / 0 | 34 / 0 | `vm-upanel-*-p012.txt` |
| `p01-4-acceptance.sh` | 114 / 0 | 114 / 0 | `vm-upanel-*-p014.txt` |
| `p02-1-acceptance.sh` | 58 / 0 | 58 / 0 | `vm-upanel-*-p021.txt` |
| `p02-2-acceptance.sh` | 103 / 0 | 103 / 0 | `vm-upanel-*-p022.txt` |
| `p02-3-agent-acceptance.sh` | 85 / 0 | 85 / 0 | `vm-upanel-*-p023.txt` |
| `p02-4-agent-secrets-acceptance.sh` | 87 / 0 | 87 / 0 | `vm-upanel-*-p024.txt` |
| `p03-1-preflight-acceptance.sh` | 93 / 0 | 93 / 0 | `vm-upanel-*-p031.txt` |
| 内核原生测试二进制 ×5 | 169 PASS / 0 FAIL | 169 PASS / 0 FAIL | `vm-upanel-*-native-tests.txt` |

`p03-2` 的 12 个实质段落（`== 0` 环境 … `== 11` 终态/日志卫生，`== 12` 汇总）：

- `== 0` 环境：root、端口 19444、`/run/upanel` 0755 root:root、清理旧接受目录
- `== 1` 启动代理与 Web：socket 属主/权限、Web 进程非 root、`capabilities`
- `== 2` 再认证与提交主流程：预检 → `POST /jobs`（`Idempotency-Key` 必填）→ `job_id`/`status`/`events_url`，
  预检 `consumed_job_id` 指向该 job，幂等键 TTL ≥ 24 h，代理侧 `succeeded`、`progress_percent` 为 `null`
- `== 3` 同一请求提交十次：一个 job、一次代理执行、一条 `accepted` 事件
- `== 4` 幂等冲突（同键不同 body → 409 `IDEMPOTENCY_CONFLICT`）与预检一次性（换键复用同一预检 → 409 `PREFLIGHT_STALE`）
- `== 5` 预检过期被拒
- `== 6` 再认证缺失 → 403 `REAUTH_REQUIRED`；短语不符 → 400 `CONFIRMATION_MISMATCH`
- `== 7` Web 进程被杀后任务继续执行；Web 重启后按 `jobs.operation_id` 找到原 operation，`operation_id` 不变
- `== 8` 代理 `kill -9`：job 变 `interrupted`、`finished_at` 有值、**不自动重跑**
- `== 9` 取消规则：`acceptance.probe` 不可取消（取消 → 409），`acceptance.ping` 可取消（202 → `canceled` + `cancellation_requested`）
- `== 10` 权限与请求形状：未登录 401、非 admin 403、缺 CSRF 403、夹带未知字段 400
- `== 11` 终态不可回退（`succeeded` 不能再变 `queued`）、日志卫生（`journal`/`state`/`-wal` 里搜不到短语）

**二进制一致性**：两台 VM 上 `/tmp/upanel-bin/upanel-agent`、`upanel-web` 的 sha256 与宿主
交叉编译产物逐字节相同（`vm-upanel-*-env.txt` vs `cross-build-sha256.txt`），所以“VM 跑的就是这份代码”可查。

## 4. 验收抓到的产品缺陷（均改产品，未改断言）

### 4.1 `CONFIRMATION_MISMATCH` 映射成 409（应为 400）

- 症状：`短语不符返回 400: 期望 '400'，实际 '409'`
- 根因：`internal/httpx.StatusForCode` 把它归进 409 组，而 CONTRACTS §4.2、`api/openapi.yaml`
  与代理侧都是 400。资源状态没变，是请求内容不对。
- 修法：改归 `CodeValidationError` 同组（400），并在表上注释分组依据。
- 证据：重跑 `== 6` 段通过；决策 DECISIONS Q64。

### 4.2 SQLite WAL 下 DEFERRED 事务升级失败不排队（`SQLITE_BUSY`）

- 症状：`ping 任务提交 202: 期望 '202'，实际 '500'`，六项连锁失败；
  代理日志 `agentstore: 消费预检失败: database is locked (5) (SQLITE_BUSY)`，只在前一次恢复性写入后约 350 ms 内出现。
- 根因：默认 `_txlock` 为 DEFERRED，事务先取读快照、第一次写时才升级写锁；**升级失败时 busy handler
  不会被调用**，立即返回 `SQLITE_BUSY`，`busy_timeout(5000)` 形同不存在。
- 修法：`internal/store/store.go` 的 `dsn()` 加 `_txlock=immediate`。
- 回归防线：新增 `TestWithTxQueuesBehindAnotherWriter`（A 持写锁、B 排队），
  **把 `_txlock=immediate` 去掉后该测试确实失败**：
  `并发写事务应排队等待而不是返回 SQLITE_BUSY，实际: database is locked (5) (SQLITE_BUSY)`。
- 决策 DECISIONS Q65。

## 5. 验收脚本的修正（只按已文档化的现行行为，断言只增不减）

| 脚本 | 原症状 | 原因（均已文档化） | 处理 |
|---|---|---|---|
| `p03-2` | `管理员登录 200 期望 200 实际 400`，随后 `PF_ID: parameter not set` 以 exit 2 退出 | `login()` 缺 `X-UPanel-Request: 1`（p03-1 里有）；未设变量让 exit 2 冒充“环境不满足” | 补头；登录失败即早退 **exit 1**；`PF_*` 全部加 `:-` 兜底 |
| `p01-1` | 19 / 7 | P01.4 的 SPA fallback 让未知路径返回 index.html；P02.3 要求 `/run/upanel` 为 0755 root:root（0750 会被代理拒绝并记日志）；P02.4 缺 key 需 `-init-key`；P02.3 普通路由对 root UID 返回 403 | 断言改成 SPA 语义 + API 404 为 JSON；目录 0755；缺 key 时引导；新增 `agent-root-forbidden`。**26 → 29 条** |
| `p01-2` | 2 失败 | 断言写的还是 `schema_version: 1`，现行为 5（0001—0005） | 改 5，并注明“当前应有的版本，新增迁移必须同步抬”（Q57 同一约定） |
| `p01-4` | 7 项 systemd 失败 | `DEPLOY_DIR` 未 stage | 指向 `/tmp/upanel-src/deploy` |

harness 侧的错也如实记一笔：从 `/tmp` 而不是 `/tmp/upanel-src` 跑，导致 `p02-3` 33/42、
`p02-4` 44/41、`p01-1` 19/7；换到正确布局后全部通过。**那是跑法错，不是产品问题。**

## 6. 未执行 / 未验证（NOT RUN）

| 项 | 状态 | 原因 |
|---|---|---|
| 资源锁排队打满 30 秒 → `RESOURCE_BUSY` 的 VM 验证 | **NOT RUN** | 验收动作太快，VM 上没有跑到超时边界；该分支只有宿主单测 |
| 真实状态漂移下的“执行前复核” | **NOT RUN** | 验收动作的状态版本是构造常量，没有真实设备/配置可漂移 |
| 跨进程 / 持久资源锁（重启后仍生效） | 不在范围内 | P03.2 的锁是进程内的（DECISIONS Q58—Q65 记录边界） |
| SSE 事件推流（`events_url` 是否可用） | 未实现 | 属 P03.3；当前只能轮询 `GET /api/v1/jobs/{id}` |
| 浏览器真实交互（任务中心页面） | 未实现 | 属 P03.3 |
| 生产动作的提交与执行 | 无动作可执行 | 生产登记表为空（STAGES 规定 P03 通过前不写磁盘代码） |

## 7. 环境事件（不记作产品缺陷）

- 24.04 验收 VM 本轮内核 panic 两次：`Attempted to kill init! exitcode=0x0000008b`
  （约 4058 s 与约 528 s uptime，`H:\Code\vms\upanel-2404\console.log` 可查）。两次都重启后重跑全部验收并通过。
  22.04 全程未 panic。与 P02.4 期间同现象，按“宿主 WHPX 长跑不稳”处理。
- 其中一次 panic 留下被截断（内容为空）的 `/etc/upanel/upanel.json`，导致三项断言报
  `解析配置文件失败 /etc/upanel/upanel.json: EOF`；删除该文件后 34/0 与 114/0。
- 宿主 PowerShell 的 `ssh` 不认这两把 VM 私钥（`UNPROTECTED PRIVATE KEY FILE`），
  全部 VM 操作用 Git Bash 的 `ssh` 完成。
- 一次 PowerShell 强杀报 `无法将类型"System.Object[]"值转换为…Int32`，随后确认只有
  pid 1832（挂住的 2404）与 23164（2204）存在，**未出现 qcow2 双挂载**；显式 kill 1832 后重启。

## 8. 复现命令

```sh
# 宿主
cd /h/Code/uPanel
go build ./... && go vet ./... && gofmt -l . && go test -count=1 ./...
export PATH="/c/Users/huxi/AppData/Local/nvm/v22.23.3:$PATH"
sh scripts/check.sh

# 交叉编译（VM 内不需要 Go 工具链）
GOOS=linux GOARCH=amd64 go build -o /h/Code/vms/dist-p032/upanel-agent ./cmd/upanel-agent
GOOS=linux GOARCH=amd64 go build -o /h/Code/vms/dist-p032/upanel-web   ./cmd/upanel-web
GOOS=linux GOARCH=amd64 go build -o /h/Code/vms/dist-p032/upanelctl    ./cmd/upanelctl
go test -c -o /h/Code/vms/dist-p032/tests/ ./internal/agentstore/ ./internal/agent/ \
    ./internal/operations/ ./internal/agentclient/ ./internal/syscmd/

# VM（先上传 dist-p032 → /tmp/upanel-bin/，源码树 → /tmp/upanel-src/，核对 sha256）
sudo rm -rf /var/tmp/upanel-accept-p03-2
sudo BIN_DIR=/tmp/upanel-bin sh /tmp/upanel-src/tests/integration/p03-2-jobs-acceptance.sh
cd /tmp/upanel-src
BIN_DIR=/tmp/upanel-bin sh tests/integration/p01-1-acceptance.sh
BIN_DIR=/tmp/upanel-bin sh tests/integration/p01-2-acceptance.sh
BIN_DIR=/tmp/upanel-bin DEPLOY_DIR=/tmp/upanel-src/deploy sh tests/integration/p01-4-acceptance.sh
BIN_DIR=/tmp/upanel-bin sh tests/integration/p02-1-acceptance.sh
BIN_DIR=/tmp/upanel-bin sh tests/integration/p02-2-acceptance.sh
sudo BIN_DIR=/tmp/upanel-bin sh tests/integration/p02-3-agent-acceptance.sh
sudo BIN_DIR=/tmp/upanel-bin sh tests/integration/p02-4-agent-secrets-acceptance.sh
sudo BIN_DIR=/tmp/upanel-bin sh tests/integration/p03-1-preflight-acceptance.sh
for t in agentstore agent operations agentclient syscmd; do
  echo "=== $t.test"; sudo /tmp/upanel-bin/tests/$t.test -test.v; done
```
