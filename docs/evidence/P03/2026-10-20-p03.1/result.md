# P03.1 验收结果 — 预检 DTO、120 秒期限、身份/动作/参数绑定、再认证与确认短语

- 日期：2026-10-20
- 范围：STAGES.md §5 P03.1（预检 DTO、120 秒期限、动作/参数/身份绑定、再认证、确认短语、快照版本），
  契约依据 CONTRACTS.md §4.1（预检）、§5（动作注册表）、§14.1（Web 库 `preflights`）、
  §14.2（代理库 `preflight_plans`）、§15（代理内部协议），
  权限依据 PLAN.md §4（`upanel-web` 非 root、无 sudo；只有 `upanel-agent` 能碰系统）。
- 结论：**通过**。宿主闸门 `PASS 12 / FAIL 0 / NOT RUN 0`；两台 Ubuntu 验收 VM 上
  P03.1 验收脚本 **93 项断言全通过**，P02.1—P02.4 四套回归脚本在同样两台 VM 上仍全绿；
  交叉编译的 agentstore / agent / agentclient / operations / syscmd 测试二进制在 VM 内核上原生 PASS。
- 本子任务**没有**开始任何磁盘写代码（STAGES.md：P03 未通过前禁止），验收用的动作来自
  只在 `-acceptance-actions` 下存在的独立验收注册表，不进入生产动作白名单。

## 1. 本子任务交付了什么

| 交付项 | 实现位置 | 说明 |
|---|---|---|
| 预检计划生成（代理侧） | `internal/agent/preflight.go`、`internal/agent/registry.go` | 动作注册项新增 `Plan`（规划器）与 `ActionPolicy`（`RequiresReauth` / `RequiresPhrase` / `PhraseVerb` / `Cancellable`）；代理负责规范化参数、目标身份指纹、状态版本、影响、阻塞项、锁、确认短语 |
| 计划存储与一次性语义 | `internal/agentstore/plans.go`、`migrations/agentdb/0002_preflight_plans.sql` | `preflight_plans` 表；`phrase_hash` 只存哈希；`markPlanUsed` 的 `UPDATE ... WHERE used_at = 0 AND expires_at > ?` 由数据库层保证一次性；过期计划由扫描器删除 |
| 代理内部接口 | `internal/agent/server.go`、`internal/agent/preflight.go` | `POST /internal/v1/preflights`，`/internal/v1` 前缀 + SO_PEERCRED UID 门；没有 `GET`；缺规划器返回 501 而不是伪造计划 |
| Web 侧编排 | `internal/operations/service.go` | 转发 `actor_context`（用户、会话哈希、再认证时间）、落库绑定（`params_hash`、`target_json`、`state_version`、`agent_ref`、`expires_at`）、把代理计划整理成公开 DTO；**不**自己判断能不能执行 |
| Web 侧预检接口 | `internal/api/preflights.go`、`internal/api/server.go` | `POST /api/v1/operations/preflight`：仅 admin + CSRF；错误映射到契约错误码（400/403/503）；成功写审计 `operation.preflight`；日志与审计不含短语 |
| Web 侧绑定表 | `migrations/0004_preflights.sql` | `preflights`：`user_id`（外键级联）、`session_hash`（重新登录即失效）、`params_hash`、`normalized_params`、`target_json`、`state_version`、`agent_ref`、`allowed`/`requires_reauth`/`requires_phrase`、`created_at`/`expires_at`、`consumed_job_id`（P03.2 用） |
| 代理客户端 | `internal/agentclient/client.go` | `CreatePreflight`；顺带修掉一个真实缺陷：客户端只把 200 当成功，而代理所有创建类接口返回 201 —— 现在接受 2xx，`PutSecret` 也一并覆盖 |
| 验收动作注册表 | `internal/agent/acceptance.go`、`cmd/upanel-agent`（`-acceptance-actions`） | `acceptance.probe`（要求再认证 + 短语 `PROBE`）与 `acceptance.ping`（都不要求）；只在该开关打开时登记，生产表里没有它们 |
| 机器可校验契约 | `api/openapi.yaml` | 补上 `POST /api/v1/operations/preflight` 与 5 个 schema；`version: 0.0.0-p03.1`、`stage: P03.1`；`redocly lint` 通过 |
| 验收脚本 | `tests/integration/p03-1-preflight-acceptance.sh` | 11 节 / 93 条断言，root 运行，两台 VM 各跑一次 |

## 2. 契约要点是如何落地的

| CONTRACTS.md 4.1 要求 | 落地方式 | 验证断言 |
|---|---|---|
| 有效期 120 秒 | 代理签发时 `expires_at = now.Truncate(秒) + 120s`；Web 存**代理给的同一个时间戳**，只做合理性校验（超过 TTL+5 s 视为协议违约） | 响应与实际时间差 115—121 s；代理库 `expires_at - created_at = 120000` ms；Web 库 `expires_at` 与响应逐字符相同 |
| 绑定用户 | `preflights.user_id` 外键；代理库 `user_id` 同样记录，两侧可按 id 对账 | 两侧 `user_id` 相等；未登录/非 admin 一个字节都不落库 |
| 绑定会话 | 两侧都存 `session_hash = SHA-256(会话令牌)` | 两侧 `session_hash` 相等；无会话令牌明文入库 |
| 绑定动作 | 计划里的 `action` 必须等于请求的动作名，否则 500（协议违约，不伪装成业务拒绝） | 动作名回显一致；不一致的计划被拒绝且不落库 |
| 绑定规范化参数 | 代理规范化 → 存 `params_json` + `params_hash = SHA-256(canonical)`；Web 用同一份规范化参数算哈希并落库 | 带多余空白的请求与紧凑请求得到**同一个**哈希；内容不同则不同 |
| 绑定目标状态版本 | 代理 `target_json.state_version` + 单列 `state_version`；Web 也存一份；公开 DTO **不含**它 | Web 库与代理库 `state_version` 相等；响应体里没有 `state_version`、没有 `agent_ref` |
| 危险动作必须短语 | 注册表 `ActionPolicy.RequiresPhrase` + `PhraseVerb`；短语 = `VERB` + 6 位 32 进制码（去掉 I/O/0/1），`phrase_hash` 入库 | `acceptance.probe` 返回短语且匹配 `^PROBE [A-HJ-NP-Z2-9]{6}$`；`acceptance.ping` 返回 `null` |
| 短语只在服务端生成 | 前端不能提交短语；请求 DTO 只有 `action`/`params`，未知字段直接 400 | 夹带 `allowed`/`confirmation_phrase` 等字段 → 400 且不落库 |
| 预检本身不写系统 | 代理只调用动作的 `Plan`（规划器），不碰执行器；验收动作在注册表里**只**登记了规划器，没有执行器，且代理此刻还没有执行路由 | 计划的影响声明为 `none`；`acceptance.probe`/`ping` 的注册项里没有可调用的执行函数，预检路径不存在能改系统的代码分支 |
| 预检不许成为后门 | 未登记动作 → 400；缺规划器 → 501；`GET /internal/v1/preflights` → 405 | 三个分支各有断言 |

## 3. 宿主（开发机）执行过的检查

| 检查 | 命令 | 结果 | 证据 |
|---|---|---|---|
| 编译 | `go build ./...` | exit 0 | `host-build.txt` |
| 静态检查 | `go vet ./...` | exit 0，无输出 | `go-vet.txt` |
| 格式 | `gofmt -l .` | exit 0，0 个文件 | `gofmt.txt` |
| 单元/集成测试 | `go test ./...` | 全部包 ok | `go-test.txt` |
| 项目闸门 | `sh scripts/check.sh` | PASS 12 / FAIL 0 / NOT RUN 0 | `check.txt` |
| 契约校验 | `redocly lint api/openapi.yaml` | valid，11 warnings（与改动前的 11 个同类告警一致：9 个 no-unused-components、2 个 health 端点的 operation-4xx-response） | `check.txt`（openapi-lint 段） |
| 交叉编译哈希 | `sha256sum .tmp-linux-bin/...` | 见文件；两台 VM 内 `sha256sum` 与宿主完全一致 | `cross-build-sha256.txt` |

## 4. Ubuntu 验收 VM 结果

两台 VM：`upanel-2404`（Ubuntu 24.04.5，内核 6.8.0-147）、`upanel-2204`（Ubuntu 22.04.5，内核 5.15.0-198）。
二进制由宿主交叉编译后拷入（VM 内无 Go/Node 工具链，DECISIONS Q9）。

| 脚本 | 运行角色 | 2404 | 2204 | 日志 |
|---|---|---|---|---|
| `p03-1-preflight-acceptance.sh`（本次） | root | 通过 93 / 失败 0 | 通过 93 / 失败 0 | `vm-2404-p031-acceptance.txt`、`vm-2204-p031-acceptance.txt` |
| `p02-1-acceptance.sh`（回归） | `upanel`（非 root） | PASS 58 / FAIL 0 | PASS 58 / FAIL 0 | `vm-2404-p021-regression.txt`、`vm-2204-p021-regression.txt` |
| `p02-2-acceptance.sh`（回归） | `upanel`（非 root） | PASS 103 / FAIL 0 | PASS 103 / FAIL 0 | `vm-2404-p022-regression.txt`、`vm-2204-p022-regression.txt` |
| `p02-3-agent-acceptance.sh`（回归） | root | PASS 84 / FAIL 0 | PASS 84 / FAIL 0 | `vm-2404-p023-regression.txt`、`vm-2204-p023-regression.txt` |
| `p02-4-agent-secrets-acceptance.sh`（回归） | root | PASS 87 / FAIL 0 | PASS 87 / FAIL 0 | `vm-2404-p024-regression.txt`、`vm-2204-p024-regression.txt` |

`p02-4` 的 `schema-version` 断言本轮由 `1` 改成 `2`（代理迁移 0002 已落地）。这是把
“当前应有的版本”更新到事实，不是放宽检查：断言仍是精确相等，并加了注释要求新增代理迁移必须同步更新。

## 5. VM 内核上原生执行的测试二进制（2404，root）

| 包 | 结果 | 说明 |
|---|---|---|
| `agentstore.test` | PASS | 含 `TestOpenRejectsNonRootOwnedDir` 等真实文件属主/权限用例 |
| `agent.test`（preflight/route/选项子集） | PASS | 计划落库、一次性、规范化、过期扫描、路由 404、必填选项 |
| `agentclient.test` | PASS | 契约字段转发、错误分类、2xx 修复回归 |
| `operations.test` | PASS | 绑定落库、到期/时钟偏差校验、参数上限 |
| `syscmd.test` | PASS | 特权命令封装 |

日志：`vm-2404-native-tests.txt`。宿主 `go test ./...` 已覆盖同一批用例，这一轮是补上
“在目标内核与目标权限模型下也通过”的证据（vm 里没有 Go，所以是 `go test -c` 产物）。

## 6. 93 条断言覆盖了什么（逐条见日志）

1. **环境与权限**：脚本以 root 运行；`upanel-web` 账号存在；`/run/upanel/agent.sock` 为
   `root:upanel-web 0660`、目录 `0755 root:root`；Web 以 `runuser -u upanel-web` 起的
   **子进程 uid 不是 0**（`ps -eo uid=,comm=` 判 `upanel-web`），root 名下没有 Web 进程。
2. **计划形状**：200 + `preflight_id` 匹配 `^pf_[0-9a-f]{32}$`；动作名回显；`normalized_params`
   是对象；`target.display_name` 非空、`identity_fingerprint` 非空；`allowed=true`；
   `requires_reauth=true`；`cancellable=false`；`locks` 含 `acceptance:probe`；
   **响应里没有 `state_version`，也没有 `agent_ref`**。
3. **120 秒与两端绑定**：`expires_at` 与当前时间差落在 115—121 s；代理库
   `expires_at - created_at = 120000`；Web 库的 `expires_at` 与响应字符串**完全相同**；
   两侧 `user_id`、`session_hash`、`params_hash`、`state_version` 相等。
4. **参数规范化**：`{"label":" a "}` 与 `{"label": " a "}`（多余空白）得到同一 `params_hash`；
   `{"label":"b"}` 得到不同哈希；Web 库 `params_hash` 等于 `SHA-256(canonical params)`。
5. **策略来自登记表**：`acceptance.ping` 的 `requires_reauth=false`、`confirmation_phrase=null`、
   `cancellable=true`；与 probe 形成对照，证明不是硬编码的“全都要求”。
6. **短语只出现一次**：代理库 `phrase_hash = SHA-256(短语)`；在 `agent.db`/`-wal`/`-shm` 三处
   二进制搜索短语明文 **0 次**；Web 库所有列中唯一带 `phrase` 字样的列是 `requires_phrase`；
   Web 日志与 journal 里搜不到短语。
7. **权限**：未登录 401、viewer 403、缺 `X-CSRF-Token` 403（`CSRF_FAILED`）；三种情况
   代理库与 Web 库都没有新增行（`SELECT COUNT(*)` 不变）。
8. **请求形状**：非 JSON Content-Type 400、坏 JSON 400、`params` 是数组 400、动作名含大写 400、
   夹带未知字段 400；未登记动作 400 且错误信息可读；`GET` 405 + `Allow: POST`。
9. **代理不可用**：停掉代理后 503 + `AGENT_UNAVAILABLE`，且不留下“半份”预检行。
10. **一次性与过期**（单元层）：`used_at` 从 0 变非 0 后触发器拒绝改回；过期未用计划被扫描器删除；
    已用计划不被删除。

## 7. 明确未验证或有边界的部分（不得当作已通过）

| 项目 | 状态 | 原因 |
|---|---|---|
| 预检 → 任务提交的端到端（`POST /jobs`） | 未实现 | P03.2；本子任务只到“生成并绑定预检” |
| 幂等键、`operation_id`、资源排队、取消 | 未实现 | P03.2 / P03.3 |
| SSE 事件流、任务中心界面 | 未实现 | P03.3 |
| 前端预检界面 / TS DTO | 未实现 | STAGES.md 未把前端纳入 P03.1；前端与真实动作一起在 P04 之后的阶段接入，接口已按契约固化 |
| 真实的破坏性动作（格式化、分区、删目录） | 未实现 | STAGES.md 明令 P03 通过前禁止写磁盘代码；本轮验收动作不改系统 |
| 真机磁盘身份、系统盘递归保护 | 未实现 | P04 / P07，且只能在测试 VM 里做 |
| 再认证窗口在预检阶段的强制 | 按契约**不**在预检阶段强制 | 契约要求提交时强制（`REAUTH_REQUIRED`），预检阶段只回显 `requires_reauth`；P03.2 补强制点 |
| 多并发预检的锁竞争 | 未验证 | 需要并发场景与 P03.2 的锁实现；当前锁只作为计划字段返回 |

## 8. 失败与修正（如实记录）

1. 宿主首轮把 `p02-1`/`p02-2` 用 `sudo` 跑，各出现 2 项失败（“验收脚本以非 root 用户运行”、
   “Web 进程不以 root 运行”）。这是脚本的设计意图（它要证明 Web 不是 root），不是产品缺陷；
   按正确角色（`upanel` 直跑）重跑即 58/0 与 103/0。
2. 上述 root 运行还留下 root 属主的 `/var/tmp/upanel-accept-p02-1` 与 root 属主的 Web 库，
   紧接着的非 root 运行出现 31 / 65 项失败（`init-admin` 认为账号已存在、curl 打到残留进程）。
   清理工作目录后重跑全绿。两次失败都没有通过修改断言或放宽条件来“解决”。
3. 实现过程中修掉的两个真实缺陷：
   - `agentstore` 的 `[]byte("[]")` 长度非 0，导致“拒绝却没有阻塞项”的计划被放行 → 引入
     `listEmpty()` 按内容判断；
   - `agentclient` 只接受 HTTP 200，而代理创建类接口返回 201 → 改为接受 2xx，并补了
     端到端用例（真实 `agentclient` ↔ 真实 `agent` 处理器）防回归。
4. 响应与库里的 `expires_at` 曾差几百毫秒（RFC3339 秒级 vs 毫秒级）→ 代理改为
   `now.Truncate(time.Second)` 后再算到期，两处真值合一。
