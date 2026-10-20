# P03.3 验收结果 — SSE 事件、断线重连、日志限额、任务中心、状态机与错误详情

- 日期：2026-10-20
- 结论：**passed**（代码 + 两台 Ubuntu 验收 VM 均通过；未执行项在文末单列，不能算通过）
- 规格依据：`docs/PLAN.md`、`docs/CONTRACTS.md`（§4.3 SSE / §4.2 错误码 / §14 数据模型）、`docs/STAGES.md`（P03.3）
- 对应决策：`docs/DECISIONS.md` Q67（阶段与事件同事务）、Q68（日志环保留）、Q69（SSE 用轮询+对账）、
  Q70（viewer 任务可见性）、Q71（验收脚本运行身份）、Q72（最小登录页，范围补充）

## 1. 这一轮交付了什么

| 面 | 交付物 |
|---|---|
| 后端 | `GET /api/v1/jobs/{id}/events`（SSE，`Last-Event-ID` 续传、`gap` 帧、每用户 4 连接上限）、`GET /api/v1/jobs/{id}/logs`（cursor/limit 分页）、`internal/operations/jobevent.go`（按 `jobs.agent_event_seq` 水位镜像代理事件 + 日志环裁剪）、迁移 `0006_job_event_mirror.sql`（水位列 + 只允许删 `kind='log'` 的触发器口子） |
| 代理 | `internal/agentstore/operations.go`：`MarkRunning` / `Finish` / `RequestCancel` 的“阶段变更 + 对应事件”合进一个事务（缺陷修复，见第 4 节） |
| 前端 | `JobsView.vue`（列表/详情/状态机/取消/日志尾部/结果 JSON）、`api/jobs.ts`（状态机与错误详情纯函数）、`api/jobstream.ts`（可注入 `EventSourceLike` 的流客户端）、`LoginView.vue` + `stores/session.ts` + `router/guard` + `router/redirect.ts`（范围补充） |
| 契约 | `api/openapi.yaml`：两条路径 + `JobEvent` / `JobLogsData` schema，`stage: P03.3`，`implemented_paths` 同步 |
| 测试 | `internal/api/jobstream_test.go`（httptest 真 HTTP，7 组）、`internal/agentstore/operations_test.go` 新增 2 项（原子性注入证明 + 并发不变量）、前端 `jobs.test.ts` 17 项 / `jobstream.test.ts` 19 项 / `redirect.test.ts` 4 项 |
| 验收设施 | `tests/integration/p03-3-events-acceptance.sh`（73 项断言）、`tests/integration/run-all.sh`（按身份调度全部验收脚本）、`scripts/run-native-tests.sh`（VM 内跑原生测试二进制并统计） |

## 2. 宿主证据（`docs/evidence/P03/2026-10-20-p03.3/`）

| 文件 | 内容 | 结果 |
|---|---|---|
| `host-build.txt` | `go build ./...` | exit 0 |
| `go-vet.txt` | `go vet ./...` | exit 0 |
| `gofmt.txt` | `gofmt -l .` | 无输出 |
| `go-test.txt` | `go test -count=1 ./...` | 全 ok，exit 0 |
| `go-test-verbose.txt` / `-summary.txt` | 5 个包的 `-v` 明细 | **228 PASS / 0 FAIL** |
| `check.txt` | `sh scripts/check.sh` | **PASS 12 / FAIL 0 / NOT RUN 0** |
| `openapi-lint.txt` | `redocly lint --format=summary api/openapi.yaml` | valid，10 warnings（8 no-unused-components + 2 operation-4xx-response，与上一阶段同类同量） |
| `cross-build-sha256.txt` | 三个 Linux 二进制的 sha256 | `dfb35bcc…` / `27412850…` / `cd51044d…` |

## 3. VM 证据（两台独立 Ubuntu，均为 root 跑代理侧脚本、非 root 跑 Web 侧脚本）

| 检查 | 24.04.5 / 6.8.0-147 | 22.04.5 / 5.15.0-198 | 证据文件 |
|---|---|---|---|
| `p03-3-events-acceptance.sh` | 通过 73 / 失败 0 | 通过 73 / 失败 0 | `vm-upanel-{2404,2204}-p033-acceptance.txt` |
| `p01-1` | PASS 29 / FAIL 0 | PASS 29 / FAIL 0 | `vm-upanel-*-p011.txt` |
| `p01-2` | PASS 34 / FAIL 0 | PASS 34 / FAIL 0 | `vm-upanel-*-p012.txt` |
| `p01-4` | 通过 114 / 失败 0 | 通过 114 / 失败 0 | `vm-upanel-*-p014.txt` |
| `p02-1` | PASS=58 / FAIL=0 | PASS=58 / FAIL=0 | `vm-upanel-*-p021.txt` |
| `p02-2` | PASS=103 / FAIL=0 | PASS=103 / FAIL=0 | `vm-upanel-*-p022.txt` |
| `p02-3` | PASS 85 / FAIL 0 | PASS 85 / FAIL 0 | `vm-upanel-*-p023.txt` |
| `p02-4` | PASS=87 / FAIL=0 | PASS=87 / FAIL=0 | `vm-upanel-*-p024.txt` |
| `p03-1` | 通过 93 / 失败 0 | 通过 93 / 失败 0 | `vm-upanel-*-p031.txt` |
| `p03-2` | 通过 101 / 失败 0 | 通过 101 / 失败 0 | `vm-upanel-*-p032.txt` |
| 原生测试二进制（agentstore/agent/agentclient/operations/syscmd/store/api） | **346 PASS / 0 FAIL** | **346 PASS / 0 FAIL** | `vm-upanel-*-native-tests.txt` |
| 环境与 sha256 | 见文件 | 见文件 | `vm-upanel-{2404,2204}-env.txt` |

两台 VM 上 `upanel-agent` / `upanel-web` / `upanelctl` 与全部测试二进制的 sha256 与宿主交叉编译产物
逐字节一致，所以“验收跑的是这份代码”有据可查。完整命令序列见 `commands.log`。

`p03-3-events-acceptance.sh` 的 12 个段落：0 环境前置 → 1 起代理与 Web、登录、再认证 →
2 提交探针任务（202 + `events_url`）→ 3 流的帧格式与响应头（含 `agent_kind`/`agent_seq`
证明事件来自代理镜像）→ 4 `Last-Event-ID` 续传只补未送达事件 → 5 校验与权限（坏 `Last-Event-ID` 400、
未登录 401、viewer 403、别人的任务 404、viewer 列表 200 且为空）→ 6 日志分页（`next_cursor`、
坏 cursor 400、`limit` 0/201 都 400）→ 7 日志环被裁后的 `gap` 帧 → 8 追加式触发器
（改状态被拒、删状态事件被拒、删日志允许、终态不回退）→ 9 连接上限（第 5 条 429，关闭后释放）
→ 10 关闭事件流不影响任务继续执行 → 11 取消语义（202/409）→ 12 日志里没有 CSRF 与口令。

## 4. 本轮抓到并修掉的缺陷（改产品，不改测试）

**阶段变更与对应事件不是原子的（22.04 偶发，三次跑出两次失败）**

- 现象：`TestOperationSubmitRunsOnceAndRecordsResult` 报
  `事件太少: [{Seq:1 Kind:accepted …} {Seq:2 Kind:started …}]`，而此时操作阶段已经是 `succeeded`。
- 根因：`MarkRunning` / `Finish` / `RequestCancel` 先 `UPDATE operations`，再用**另一笔写**
  `INSERT operation_events`。两笔写之间存在窗口，读方可以先看到终态、后（甚至永远不）看到终态事件。
- 为什么必须改产品：Web 的 `MirrorAgentEvents` 正是按“阶段可见 ⟹ 对应事件可见”实现的——
  先读操作、再读事件，看到终态后不再追。分两笔写会让终态事件永久落在镜像之外，
  用户看到的是一个没有最后一条日志的任务。
- 修法：三处阶段变更合进一个 `WithTx` 事务；条件更新带 `stage = <读到的阶段>` 守卫，
  `RowsAffected = 0` 时回滚并用 `errStageMoved` 重试（上限 3 次，竞争只来自“取消 vs 执行”）。
- 确定性证明：新增 `TestFinishRollsBackStageWhenEventWriteFails`——用 `BEFORE INSERT` 触发器
  只让该操作的事件写入失败，断言终态一起回滚。**把 `internal/agentstore/operations.go` 换回
  上一个提交的版本，该测试失败**（`事件写入失败后阶段 = "succeeded"，期望仍是 running`）；
  换回修复版本后通过。另加 `TestStageChangeAndEventAreAtomic`：并发读方持续检查不变量。
- 修复后：同一用例在 22.04 上单独 `-test.count=20` 全过，宿主 `-count=40` 全过。

## 5. 本轮暴露的两个流程事故（不是产品缺陷，改的是调度）

1. **用错身份跑验收脚本**：`sudo sh p01-2-acceptance.sh` 让 root 绕过 `0500` 目录权限，
   “数据库目录不可写必须拒绝启动”那条永远等不到退出，脚本挂了 32 分钟。
   → 新增 `tests/integration/run-all.sh`：按脚本头声明的身份分组（`p01-1/p01-2/p01-4/p02-1/p02-2`
   用 `setpriv` 降到 `upanel`；`p02-3/p02-4/p03-1/p03-2/p03-3` 用 `sudo`），跑前清理残留进程、
   `/run/upanel` 与 `/var/tmp/upanel-accept-*`。**断言一条没改。**
2. **通配符漏了 `upanelctl`**：`chmod 0755 /tmp/upanel-bin/upanel-*` 不匹配结尾没有连字符的
   `upanelctl`，24.04 上 `p02-1` 因此刷出 45 条“功能失败”，看起来像产品坏了。
   → `run-all.sh` 跑前检查三个入口是否可执行，缺就 `exit 2` 并打印原因；`DEPLOY_DIR` 结构不对
   也直接报错，不让 `p01-4` 以七条“单元文件不存在”的形式呈现。

## 6. 环境事实（不记作产品缺陷）

- 24.04 VM 本轮第四次中断：15:2x 期间 ssh 开始 banner 超时、登录不进，强制重启后恢复。
  上一次启动的 kernel 日志结尾只有 `journald: File ... corrupted or uncleanly shut down`，
  没有 oops/panic 记录（本轮之前它已 panic 三次，均为 `Attempted to kill init! exitcode=0x8b`）。
  重启后 `/tmp` 清空，二进制与脚本重新上传并逐一核对 sha256，随后所有脚本重跑通过。
- 22.04 VM 全程未中断。

## 7. NOT RUN（未执行，不能算通过）

- `go test -race`：宿主报 `-race requires cgo; enable cgo by setting CGO_ENABLED=1`，未执行；
  VM 内同样未启用 cgo。整轮**没有竞态检测器证据**，并发正确性靠并发用例 +
  `_txlock=immediate` 的排队语义支撑。
- 浏览器端的 SSE 与登录页：只有 node 层逻辑测试（可注入 `EventSourceLike`）与构建产物，
  没有用真实浏览器打开任务中心，没有观察帧刷新 / 重连 / `gap` 的实际表现；Firefox/Safari 未验证。
- 长连接资源占用与压测：没有“连接挂数小时”“4 条连接跑满再反复断线重连”的观测；
  只做了秒级的连接上限与释放断言。
- 日志环裁剪的容量/耗时观测：只验证“超过上限后最旧日志被删、状态事件不受影响”，
  没有真实产出上万行日志。
- 代理与 Web 同时重启时的对账收敛（P03.4 范围）未验证。

## 8. 复现步骤

```sh
# 宿主：构建、单测、闸门、交叉编译
cd /h/Code/uPanel
go build ./... && go vet ./... && gofmt -l . && go test -count=1 ./...
export PATH="/c/Users/huxi/AppData/Local/nvm/v22.23.3:$PATH"
sh scripts/check.sh
mkdir -p /h/Code/vms/dist-p033/tests
GOOS=linux GOARCH=amd64 go build -o /h/Code/vms/dist-p033/upanel-agent ./cmd/upanel-agent
GOOS=linux GOARCH=amd64 go build -o /h/Code/vms/dist-p033/upanel-web   ./cmd/upanel-web
GOOS=linux GOARCH=amd64 go build -o /h/Code/vms/dist-p033/upanelctl    ./cmd/upanelctl
GOOS=linux GOARCH=amd64 go test -c -o /h/Code/vms/dist-p033/tests/ ./internal/agentstore/ \
    ./internal/agent/ ./internal/operations/ ./internal/agentclient/ ./internal/syscmd/ \
    ./internal/store/ ./internal/api/

# VM（两台各一次）：上传 → 核对 sha256 → 原生测试 → 全部验收脚本
scp -i .../id_ed25519 -P 2222 -r /h/Code/vms/dist-p033/{upanel-agent,upanel-web,upanelctl,tests} \
    upanel@127.0.0.1:/tmp/upanel-bin/
scp -i .../id_ed25519 -P 2222 tests/integration/*.sh upanel@127.0.0.1:/tmp/upanel-src/tests/integration/
scp -i .../id_ed25519 -P 2222 -r deploy upanel@127.0.0.1:/tmp/upanel-src/     # 注意别嵌套成 deploy/deploy
ssh ... 'chmod 0755 /tmp/upanel-bin/upanel-agent /tmp/upanel-bin/upanel-web \
         /tmp/upanel-bin/upanelctl /tmp/upanel-bin/tests/*'
ssh ... 'sudo BIN_DIR=/tmp/upanel-bin sh /tmp/upanel-src/tests/run-native-tests.sh'
ssh ... 'BIN_DIR=/tmp/upanel-bin sh /tmp/upanel-src/tests/integration/run-all.sh'
```

## 9. 下一阶段

P03.4（Web/agent 重启对账、重复提交、`interrupted` 后的“未知结果”处理），随后 P04。
本轮已经为它留下两个前提：代理的阶段与事件现在是原子的（读方可以放心按阶段判断是否追事件），
以及 `run-all.sh` 让“全量回归”变成一条命令、身份不会再给错。
