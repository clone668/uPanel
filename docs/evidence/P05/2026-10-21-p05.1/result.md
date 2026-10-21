# P05.1 验收结果（2026-10-21）

**范围**：服务列表、详情与保护名单；通过「预检 → 任务」执行允许的启动/停止/重启/
开机启动设置。依据 `docs/STAGES.md` §7 P05.1、`docs/CONTRACTS.md` §10.1/§4、
`docs/PLAN.md` §14、`api/openapi.yaml`（`info.version: 0.0.0-p05.1`，
`x-upanel-contract-status.stage: P05.1`）。

**结论：passed（宿主 + 一台验收 VM `upanel-2404`）。**
VM 侧 **PASS=140 / FAIL=0 / NOT RUN=3**，载荷校验 **PASS=22369 / FAIL=0**，
宿主 `scripts/check.sh` **PASS 13 / FAIL 0 / NOT RUN 0**。
NOT RUN 三项都在本文件末尾逐条写明原因，没有当作通过。

## 做了什么

| 层 | 文件 | 内容 |
|---|---|---|
| 采集 | `internal/services/{systemctl,types,collector,protection,fetch}.go`（新包） | 枚举 service 单元（`list-unit-files` ∪ `list-units --all`，剔掉模板与别名）、按块解析 `systemctl show`、10 秒快照缓存、保护名单判定、`Snapshot`/`Unit` DTO |
| 控制 | `internal/services/control.go` | 预检评估（`AssessControl`）与执行（`ExecuteControl`）：状态冲突、受保护、不可启用、单元身份指纹（`sha256:` + 完整 64 位十六进制）、`systemctl` 真实退出状态与读回验证 |
| 代理 | `internal/agent/{servicesquery,servicesaction}.go`、`registry.go` | `service.list` / `service.get` 查询通道与 `service.control` 生产动作；`cmd/upanel-agent` 注册并在启动时开启采集 |
| API | `internal/api/services.go`（新）、`router.go`、`server.go` | `GET /services`、`GET /services/{unit}`、`POST /operations/preflight`（服务动作）、`POST /jobs`（service.control） |
| 审计 | `internal/audit/audit.go`、`internal/api/users.go` | 新增 `job_id` 列写入与 `JobIDFromTarget`：`job.submit` / `job.cancel` 现在能按任务号回溯 |
| 前端接口层 | `web/src/api/{services,preflights}.ts`（新）、`jobs.ts` | 与 openapi 同源的 DTO、闭集中文标签、`canOperate`/`summarizeUnits`/`groupUnits`/`canSubmitPreflight` 判决函数 |
| 前端页面 | `web/src/views/{ServicesView,ServiceUnitView}.vue`、`web/src/components/{ServiceUnitCard,ServiceUnitPanel,ServiceActionConfirm}.vue`（新）、`modules/registry.ts`、`router/routes.ts` | 列表（摘要 + 分组 + 关键字过滤）、详情（事实面板 + 动作入口）、确认对话框（影响说明 → 预检 → 任务 → 轮询终态）；加载/空/错误/无权限四种状态齐备 |
| 契约 | `api/openapi.yaml`、`docs/CONTRACTS.md` §4.3/§10.1、`docs/DECISIONS.md` | 补 `Job.result` 空对象语义、模板与别名的排除规则（附真实 stderr）、完整性校验规则 |
| 真机夹具 | `internal/services/testdata/*`（7 个，systemd 255 的真实字节）、`web/src/fixtures/p05-1-vm/*`（15 个，VM 真实响应） | 解析与页面的回归底座，数据不是编的 |
| 测试 | `internal/services/*_test.go`、`internal/api/services_test.go`、`internal/audit/audit_test.go`、`internal/agent/services_test.go`、`web/src/api/{services,preflights}.test.ts`、`web/src/components/{services.test.ts,realdata-services.test.ts}` | 含真实字节的解析回归、注入边界、受保护/模板/别名/not-found 分支、SSR 渲染真实载荷 |
| VM 验收 | `tests/integration/p05-1-services-acceptance.sh`、`p05-1-payload-validate.py`（新） | 14 节、140 条断言 + 2.2 万条载荷断言，全部在真 systemd 上执行 |

## 实际执行的检查

| 检查 | 结果 | 证据 |
|---|---|---|
| VM 验收脚本自身断言（14 节） | **PASS=140 FAIL=0 NOT RUN=3** | `p05-1-acceptance-2404.txt` 末行 |
| VM 载荷校验（字段、闭集、null 语义、与 `systemctl` 对照） | **PASS=22369 FAIL=0** | 同文件 §12 |
| 宿主 `sh scripts/check.sh` | **PASS 13 / FAIL 0 / NOT RUN 0** | `check.txt` |
| `go build ./...` / `go vet ./...` | 退出码 0 / 0 | `go-gates.txt` |
| `go test ./... -count=1` | 21 个包全部 ok | `go-test.txt` |
| 前端 `vue-tsc --noEmit` | 0 error | `web-gates.txt` |
| 前端 `eslint .` | 0 problem | `web-gates.txt` |
| 前端 `vitest run` | **252 passed（20 文件）** | `web-gates.txt` |
| 前端 `vite build` | 成功（493 ms） | `web-gates.txt` |
| 页面冒烟（dist 预览，未登录守卫） | **PASS 10 / FAIL 0 / SKIP 0** | `check.txt` |
| 真实载荷回放（`realdata-services.test.ts`） | **22 passed** | `web-gates.txt` |
| 契约 lint（redocly） | valid，9 warning（未使用组件 7 + 4xx 说明 2，历史遗留） | `check.txt` |

VM 侧值得逐条看的关键断言（都在 `p05-1-acceptance-2404.txt` 里）：

- **身份边界**：监听 19449 的是 `upanel-web`（uid 999），代理是 root；`agent.sock` 是
  `root:upanel-web` 且不是 0666。
- **列表是快照**：连续两次 `collected_at` / `snapshot_version` 不变，10 秒后推进。
- **规模与枚举一致**：接口 239 个单元，与 `systemctl` 枚举结果逐项对照；25 个模板名与
  20 个别名都不出现在列表里，别名请求 404 并在提示里要求用规范名，模板请求 404 并提示需要实例。
- **状态是现读的**：14 个抽样单元的 `active_state`/`enablement` 与 `systemctl is-active`/
  `is-enabled` 逐个相同。
- **受保护单元**：`systemd-journald` / `systemd-logind` / `systemd-udevd` / `ssh` 四个的
  `capabilities` 都是空数组，预检 `allowed=false` 且阻塞码 `SERVICE_PROTECTED`，
  现场状态在整轮验收前后没有被改动。
- **真实启停**：`start` 之后现场 `active`、`main_pid=24135`、`started_at=2026-10-21T01:31:23Z`；
  `restart` 后 `started_at` 前进到 `01:31:25Z`；`enable`/`disable` 后 `UnitFileState` 跟随变化，
  且 `disable` 不会顺手停掉正在跑的服务；`stop` 之后 `main_pid` 变 `null`、能力只剩
  `start`/`enable`。每一步都有对应的 `observed_state.verified=true`。
- **重复操作被预检挡住**：重复 `start`、重复 `stop`、未运行时 `restart`、
  无 `[Install]` 段单元 `enable`，四种都是 `allowed=false` 且没有生成任务。
- **失败必须失败**：`ExecStart` 不存在的单元让任务终态为 `failed`，
  `result` 是空对象 `{}`，错误码 `COMMAND_FAILED`，代理日志里保留 `systemctl` 的真实退出状态。
- **viewer 只读**：能读列表与详情，创建预检与提交任务都是 403。
- **审计**：有 `operation.preflight` 与 `job.submit` 记录（后者 `job_id` 非空），
  `redacted_details` 里没有管理员口令。
- **注入边界**：`..%2F..%2Fetc%2Fpasswd`、`cron`、`-o.service`、`a b.service`、`x;y.service`
  五个非法单元名分别被 400/404 拒绝且响应里不含系统文件内容；六种注入式预检参数
  （多余字段、未知操作、旗标形状的单元名）全部 400，没有一个被执行。

## 与 STAGES §7 P05.1 验收条目对应

| 验收条目 | 证据 |
|---|---|
| 操作测试 service 的真实状态发生变化 | §8 整节：`start`/`restart`/`enable`/`disable`/`stop` 五步，每步都读回 `ActiveState`/`UnitFileState`/`main_pid`/`started_at`，并与 `systemctl` 现场对照 |
| 且有审计 | §11：`operation.preflight` + `job.submit`（`job_id` 非空）都在 `audit_events` 里 |
| 不存在的 unit 不被当作命令片段 | §5 + §6：`/services/upanel-doesnotexist.service` 404、预检 `SERVICE_NOT_FOUND`，且现场没有生成任何单元文件；§7 六种注入形态全部 400 |
| 不能停止代理/面板/SSH/核心服务 | §6：`ssh.service`、`systemd-journald/logind/udevd` 受保护、`capabilities=[]`、预检拒绝；面板与代理自身作为单元的判定由 `internal/services/types_test.go` 的 `TestProtect` 覆盖（VM 里它们是普通进程，见 NOT RUN） |
| PID 重用后旧预检不能杀新进程（P05.2） | 不属本子任务；本轮只保证服务的单元身份指纹是 `sha256:` 全 64 位，不靠名字猜 |

## 本轮发现并修掉的问题

1. **282 个单元的机器上列表只有 7 行**（产品缺陷，真机才暴露）。
   `systemctl show` 遇到裸模板名（`foo@.service`）会以
   `Unit name ... is neither a valid invocation ID nor unit name.` 退出 1，
   并且**把这条命令里后面的名字一起丢掉**。旧代码只判断“解析结果非空”，
   于是拿到 7 个就当成功。现在：枚举阶段就剔掉模板；`Show` 分块执行；
   拿到的单元与请求的单元逐项核对，缺的单独重试，仍缺就把整次采集标记失败并保留上一份快照。
   真实字节存在 `internal/services/testdata/`，回归用例
   `TestParseRealMultiUnitOutput` / `TestShowFailsWhenChunkAborts` / `TestShowRetriesMissingNamesIndividually`。
2. **整次采集被一个别名单元搞崩**（产品缺陷）。`dbus-org.freedesktop.ModemManager1.service`
   是 `ModemManager.service` 的别名，`systemctl show` 只回 `Id=ModemManager.service`，
   按 `Id` 匹配就认为“请求的单元没回来”→ 整次采集 `status=error`、0 个单元。
   现在按 `Id` **和** `Names` 建索引，并在枚举阶段剔掉 `UnitFileState=alias` 的条目
   （否则同一个单元会出现两行，别名那行的 `enablement` 还是 `alias`，等于绕过保护名单的读侧对账）。
   契约写进 `docs/CONTRACTS.md` §10.1，回归用例 `TestShowMatchesAliasByName` /
   `TestListUnitNamesExcludesAliases`。
3. **失败任务的 `result` 语义漂移**（契约与实现不一致）。`job-start-fail` 里
   `result` 到底是 `{}` 还是 `null` 一直含糊。按 `api/openapi.yaml` 的
   `Job.result` 是必填对象，定成“没有结果就是 `{}`，失败原因只在 `error` 里”，
   契约描述、验收脚本与载荷校验三处一起改齐。
4. **审计的 `job_id` 从来没写进去**（产品缺陷）。`audit_events.job_id` 列一直存在，
   但 `Record` 没有落值，于是“从审计回溯到任务”这条链是断的。现在 `audit.Event.JobID`
   写入列值（非任务类事件仍为 NULL），`JobIDFromTarget` 负责从 `job:` 目标推出任务号，
   验收 §11 的 `job.submit AND job_id IS NOT NULL` 断言因此成立。

## 无法在本环境覆盖（NOT RUN，逐条原因）

| 项 | 原因 |
|---|---|
| 执行后未收敛（`systemctl` 返回 0 但状态 15 秒内没变） | 需要人为构造“命令成功但状态不变”的单元，正常 systemd 上造不出来。该路径由 `internal/services/collector_test.go` 的 fake 与 `control` 的读回超时分支覆盖，VM 上标 NOT RUN，不冒充通过 |
| 浏览器渲染（真实数据） | VM 上没有浏览器，也没装 Node。页面级证据在宿主用**同一批真实载荷**回放：`web/src/components/realdata-services.test.ts` 读 `web/src/fixtures/p05-1-vm/`（与本目录 `payloads/` 逐字节相同，22 个用例） |
| 面板自身作为 systemd 单元（`upanel-web` / `upanel-agent`） | 验收里两者是直接启动的进程，不是 unit，所以“停掉面板会断掉自己”这条保护判定在 VM 上无从触发；由 `internal/services/types_test.go` 的 `TestProtect`（按单元名与 `MainPID` 两种输入）与
`internal/services/collector_test.go` 的 `TestExecuteControlRechecksProtection`（执行前再查一次）覆盖 |

## 已知限制

- 保护名单把 `systemd-*` 整体计入核心组件（CONTRACTS §10.1 的显式决定），
  这台 VM 因此有 86 个受保护单元。它们全都没有 `capabilities`，界面上不出现按钮。
- 列表是 10 秒快照，`truncated=true` 时（单元数超过 1000）界面会明说“只显示前 N 个”，
  真机上没触发过，只有单测覆盖。
- 别名单元不在列表里（契约如此）：要操作它必须用规范名，这就是详情 404 提示里写的那句话。

## 环境侧记录

一次验收运行期间 VM 的 PID 1（systemd）SIGSEGV（`panic-6` 后内核重启），该次运行作废。
原因未定位，重跑后得到本文记录的结果。`env.txt` 里有完整的环境采集与这条记录。

## 下一步

P05.2：进程列表、PID + starttime 身份、TERM/KILL 分开的流程、禁止终止关键进程。
届时 `service.control` 的预检/任务链路直接复用，不新开一条执行路径。
