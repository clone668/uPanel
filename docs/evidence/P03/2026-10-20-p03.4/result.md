# P03.4 验收结果 — Web 与代理重启对账、重复提交、执行结果未知的 interrupted

日期：2026-10-20　　实施者：本项目执行者　　结论：**passed**（代码 + 两台 VM 验收均完成）

本文件只写**实际执行过**的检查。未执行的检查在第 6 节单列（NOT RUN），不计入通过。

---

## 1. 本轮交付内容

| # | 交付物 | 文件 | 对应验收点（STAGES P03.4 / CONTRACTS §4.3） |
|---|---|---|---|
| 1 | 后台对账循环：启动立刻扫一遍，之后每 30 秒一次，批次 100，只查不跑 | `internal/operations/reconcile.go`（新）、`cmd/upanel-web/main.go` | Web 重启后仍能找到原 operation；“启动时检查所有非终态和未同步任务” |
| 2 | 提交在途登记：任务已落库、`CreateOperation` 未返回的窗口不对账 | `internal/operations/service.go`、`jobs.go` | 提交途中不判定，避免把正在执行的破坏性动作写成 failed |
| 3 | `NOT_FOUND` 按 `needs_sync` 分流：`failed`/`unconfirmed` 或 `interrupted`/`unknown_result` | `internal/operations/reconcile.go`（`handleAgentForgets`） | “执行结果未知的 interrupted”；修订 Q63 |
| 4 | 幂等记录清理：只删“已过期且任务已结束”的键，每小时一次 | `internal/operations/reconcile.go` | CONTRACTS §14.2 保留期落地，且不制造第二次执行 |
| 5 | 读接口与后台扫描共用同一份判定（`reconcileOne`） | `internal/operations/jobs.go` | “被读”与“被扫”不会得到不同终态 |
| 6 | 测试注册表约束变成断言 | `internal/agent/acceptance_test.go`（新） | “测试动作只在独立注册表，不放入生产白名单” |
| 7 | 单元测试 11 个 | `internal/operations/reconcile_test.go`（新） | 覆盖排队→完成、提交未到达、结果未知、在途、代理不可用、终态跳过、重启重放、并发竞态、ctx 停止、两种清理 |
| 8 | 黑盒验收脚本 78 项断言 | `tests/integration/p03-4-reconcile-acceptance.sh`（新） | 两代 Ubuntu 上的 11 个段落 |
| 9 | 全量回归调度：日志改到自建 `LOG_DIR`，11 个脚本按身份执行 | `tests/integration/run-all.sh` | Q71 的延续（跑错身份不会报“身份错”） |

产品代码**没有**新增任何写动作：代理的生产登记表仍为空，验收动作只在 `-acceptance-actions`
下存在，且 `p03-4` 第 2 段用不带该参数的代理验证了这一点。

## 2. 宿主证据（开发机：Windows 11 + Git Bash）

| 检查 | 命令 | 结果 | 证据文件 |
|---|---|---|---|
| 构建 | `go build ./...` | exit 0，无输出 | `host-build.txt` |
| 静态检查 | `go vet ./...` | exit 0，无输出 | `go-vet.txt` |
| 格式 | `gofmt -l .` | 无输出 | `gofmt.txt` |
| 单元测试（全量） | `go test -count=1 ./...` | 18 个包 `ok` + 2 个 `no test files`，exit 0 | `go-test.txt` |
| 单元测试（详细） | `go test -count=1 -v` 覆盖 operations / agent / agentstore / api / store / agentclient | **--- PASS 258 / --- FAIL 0**（=== RUN 351，含子测试与表驱动分支） | `go-test-verbose.txt`、`go-test-verbose-summary.txt` |
| 统一闸门 | `sh scripts/check.sh` | **PASS 12 / FAIL 0 / NOT RUN 0** | `check.txt` |
| 契约 lint | `redocly lint --format=summary api/openapi.yaml` | valid，**10 warnings**（`no-unused-components` 8 + `operation-4xx-response` 2，与 P03.3 同类同量） | `openapi-lint.txt` |
| 交叉编译 | `GOOS=linux GOARCH=amd64 go build/build -c` | 三个入口 + 七个测试二进制 | `cross-build-sha256.txt` |

交叉编译产物的 sha256（两台 VM 上逐一核对过，逐字节一致）：

```
0b03b63fb4298e47d7c80b0fd2cbd288dc61c4497eb985cada72ec7467c24976  upanel-agent
89ddb2a7e9c8a13be7275d23eea299c1d85b85cc6a3f7b46dd59dbf7321c1ba0  upanel-web
91880be42ef196a8c0f4ffb7d033ad4ffc922a8531bcf418359fc9d9f5223332  upanelctl
5706c9e3199beed87eeaf5ca17e09f6e171c4fe3b6059d916a5681986f80a9da  tests/agent.test
9c2e734763748697bc1a54dbc2143ae9f16e3114366ceef221df5deac6958774  tests/agentclient.test
f72c84ea38d6406166cbc74f54891d74379ca68cddac2de82de8e19456a831d1  tests/agentstore.test
8cf56ce6020a8136e53fc69bba6677d917eaf01745ba4d4e7b955c11959230b8  tests/api.test
ce78004a5383ab6805a220cee801b22eb294112a4dd46664b637232ff39376f0  tests/operations.test
3fe0d44905571e7b3dcfa83999deefaf25cd241710fbfaf5d244954727d68018  tests/store.test
e1ff7f0f71c8e9763c53c148ec888e78649d001599a2ff5b282b4c95ccfe6b17  tests/syscmd.test
```

## 3. 两台验收 VM

| 检查 | 24.04.5 / 6.8.0-147（端口 2222） | 22.04.5 / 5.15.0-198（端口 2223） |
|---|---|---|
| `p03-4-reconcile-acceptance.sh`（本轮新增） | **通过 78 / 失败 0** | **通过 78 / 失败 0** |
| `p01-1-acceptance.sh` | PASS 29 / FAIL 0 | PASS 29 / FAIL 0 |
| `p01-2-acceptance.sh` | PASS 34 / FAIL 0 | PASS 34 / FAIL 0 |
| `p01-4-acceptance.sh`（含 systemd 真实安装启动） | 通过 114 / 失败 0 | 通过 114 / 失败 0 |
| `p02-1-acceptance.sh` | PASS 58 / FAIL 0 | PASS 58 / FAIL 0 |
| `p02-2-acceptance.sh` | PASS 103 / FAIL 0 | PASS 103 / FAIL 0 |
| `p02-3-agent-acceptance.sh` | PASS 85 / FAIL 0 | PASS 85 / FAIL 0 |
| `p02-4-agent-secrets-acceptance.sh` | PASS 87 / FAIL 0 | PASS 87 / FAIL 0 |
| `p03-1-preflight-acceptance.sh` | 通过 93 / 失败 0 | 通过 93 / 失败 0 |
| `p03-2-jobs-acceptance.sh` | 通过 102 / 失败 0 | 通过 102 / 失败 0 |
| `p03-3-events-acceptance.sh` | 通过 73 / 失败 0 | 通过 73 / 失败 0 |
| VM 内核原生测试二进制（7 个 `.test`） | **TOTAL PASS 360 / FAIL 0** | **TOTAL PASS 360 / FAIL 0** |
| `run-all.sh` 汇总 | 运行 11 / 失败 0 | 运行 11 / 失败 0 |

原生测试二进制的分项（两台一致）：agentstore 72、agent 104、agentclient 24、operations 57、
syscmd 9、store 11、api 83 = **360**（P03.3 为 346，本轮新增 14 个用例：对账 11 + 验收注册表 3）。

**“验收跑的是这份代码”的凭据**：`env-24.txt` 与 `env-22.txt` 记录了两台 VM 上
`/tmp/upanel-bin/*`（三个入口 + 七个测试二进制）与全部验收脚本的 sha256，
与宿主 `cross-build-sha256.txt`、以及本仓库工作区的文件逐行一致；报告正文里没有一处
“大概是这样”。

逐脚本完整日志：`vm-logs-2404/`、`vm-logs-2204/`（各 11 个文件，来自 `/var/tmp/upanel-logs/`）。

## 4. 证明“测试有牙”的变异与静态检查

三条检查都在宿主上实跑，产物在证据目录里，**不是描述**：

1. **摘掉在途守卫**（`if false && s.submitInFlight(job.ID)`）→
   `TestSweepPendingSkipsInFlightSubmit` 与 `TestSubmitRacingSweepDoesNotMarkFalseFailure` 双双
   失败（`Scanned:1 Unconfirmed:1`，期望 `SkippedInflight:1`）。恢复后通过。
   证据：`mutation-1-inflight-guard.txt`。
2. **把 `needs_sync` 分流写死**（`if false`）→ `TestSweepPendingMarksUnconfirmedWhenAgentNeverSawOperation`
   失败（`Unknown:1`，期望 `Unconfirmed:1`）。恢复后通过。证据：`mutation-2-needs-sync-split.txt`。
3. **对账路径没有重新提交的调用点**：`grep -rn CreateOperation internal/operations/reconcile.go`
   只命中两句注释；唯一向代理发起的调用是 `GetOperation`。证据：`static-no-resubmit.txt`。

## 5. 本轮修掉的缺陷（按发现顺序）

| 缺陷 | 表现 | 处理 | 决策 |
|---|---|---|---|
| `NOT_FOUND` 一律写 failed | 代理曾经确认过、后来查不到，被断言成“没执行” | 按 `needs_sync` 分流，`interrupted`/`unknown_result` 并要求人工核对 | Q75（修订 Q63） |
| 验收断言扫整个响应体 | `request_id` 随机串里出现 “4242”，`check.sh` 的 go-test 变红 | 只断言 `error.code` 与 `error.message` 两个字段 | Q78 |
| `p01-4` 不清 systemd 库 | 旧库记着 `0006` 的旧校验和，新二进制按设计拒绝迁移，服务停在 `activating`，连带 5 条失败 | migrate 前删掉该库（现场由脚本自己创建） | Q79 |
| 两处“按时间取最新一条”的断言 | `created_at` 秒级精度，取到哪一行不确定；`p03-4` 那处等于把对象从断言里抹掉 | 改成按幂等键/`operation_id` 指名对象，并补错误码断言 | Q80 |

补记：本轮还关闭了 P03.2 留下的一个欠账——**`idempotency_keys` 的到期清理此前从未实现**
（当时注释写“清理属 P03.4”），现已实现并有两项单元证据（Q76）。

## 6. NOT RUN（本轮没有执行，不能算通过）

- **`go test -race`**：宿主需要 cgo（`-race requires cgo`），本机未启用；VM 内也没有 Go 工具链。
  整轮**没有竞态检测器证据**。并发正确性由 `TestSubmitRacingSweepDoesNotMarkFalseFailure`
  （把“代理调用中”做成确定性窗口）与 `_txlock=immediate` 的排队语义支撑，但那不等于 race detector。
- **幂等记录清理的周期**：`IdempotencyPurgeInterval = 1h`，验收脚本不会等一小时，因此
  “每小时真的跑了一次”**只有单元测试与代码，没有 VM 观测**。
- **真实浏览器**：本轮没有任何浏览器证据；对账与 `interrupted` 的展示沿用 P03.3 的组件，
  没有在 Chromium/Firefox 里打开过。P03.3 的 NOT RUN 项（真实浏览器 SSE、长连接压测、
  日志环容量）在本轮同样仍未执行。
- **代理/Web 同时重启**：脚本覆盖的是“先杀代理再重启”“先杀 Web 再重启”两种顺序，
  **没有**在两侧同时停、同时起的情况下验证收敛；理论上对账会在一轮内收敛，但这是推理不是证据。
- **多 Web 进程**：在途登记是进程内的，systemd 单实例前提下成立；多实例/多副本未验证（Q74 已注明）。
- **长期运行**：没有让 Web 连续跑数小时观察 30 秒循环的累积影响（内存、日志量、WAL 增长）。

## 7. 复现

```sh
# 宿主（Git Bash）
cd /h/Code/uPanel
go build ./... && go vet ./... && gofmt -l .
go test -count=1 ./...
export PATH="/c/Users/huxi/AppData/Local/nvm/v22.23.3:$PATH"
sh scripts/check.sh
web/node_modules/.bin/redocly lint --format=summary api/openapi.yaml

# 交叉编译 + 上传（两台 VM 各做一次，逐文件 chmod 0755，注意 upanelctl 结尾没有连字符）
mkdir -p /h/Code/vms/dist-p034/tests
GOOS=linux GOARCH=amd64 go build -o /h/Code/vms/dist-p034/upanel-agent ./cmd/upanel-agent
GOOS=linux GOARCH=amd64 go build -o /h/Code/vms/dist-p034/upanel-web   ./cmd/upanel-web
GOOS=linux GOARCH=amd64 go build -o /h/Code/vms/dist-p034/upanelctl    ./cmd/upanelctl
GOOS=linux GOARCH=amd64 go test -c -o /h/Code/vms/dist-p034/tests/ ./internal/agentstore/ \
    ./internal/agent/ ./internal/operations/ ./internal/agentclient/ ./internal/syscmd/ \
    ./internal/store/ ./internal/api/

# VM 内（root）
sudo BIN_DIR=/tmp/upanel-bin sh /tmp/upanel-src/tests/run-native-tests.sh
sudo BIN_DIR=/tmp/upanel-bin sh /tmp/upanel-src/tests/integration/run-all.sh
# 逐脚本日志在 /var/tmp/upanel-logs/，可用 LOG_DIR=... 覆盖
```

完整命令序列（含变异测试与 VM 上传）见同目录 `commands.log`。
