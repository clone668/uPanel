# P09.5 验收结果 — 应用卡片、搜索、分组、一键打开、状态区分与常用入口首页集成

证据类型：**Linux VM integration**（一次性 Ubuntu 24.04.5 虚拟机，root 起代理、uid 999 起 Web）
执行时间：2026-10-22（UTC，虚拟机时钟）
脚本：`tests/integration/p09-5-apps-ui-acceptance.sh`
环境：见 `env.txt`；宿主检查见 `check.txt`；单元测试见 `native-tests.txt`

---

## 1. 结论

| 项 | 数据 |
|---|---|
| 最终一遍（`run.log`） | **PASS=211 / FAIL=0 / NOT RUN=0**（退出码 0） |
| 真机发现的产品缺陷 | **0 条**（本轮产品代码在真机上一次通过；4 处失败全是脚本自己的毛病或一处契约文字写错，见 §5） |
| 卡片四件事分开写 | 一条应用同时给出 `runtime_state` / `http_state` / `dependency_protection` / `runtime_note`；**容器在跑但回 500** 时 `running` 与 `error` 同时成立，不是一句“状态良好” |
| 能力来自底层对象 | link 三项生命周期全 false；running 的单元只有 stop/restart；受保护单元（`ssh.service`）三项全 false 但**仍能读日志**；容器重建后旧绑定连 `logs` 都没有 |
| 停机再启动 | 同一条应用的前后两拍：`id`/`binding`/`version` 不变，`stopped+start` 与 `running+stop` 互换，`logs` 两拍都在 |
| 一键打开是服务器地址 | `public_url` 用 VM 的非回环地址 `10.0.2.15`，容器在那个地址上真回 200，**同一端口在 127.0.0.1 上什么都没有**；回环 / localhost / 带凭据 / `javascript:` 的入口在写入口就被 400 拒掉 |
| SSRF 是结构性的 | `target_id` 是闭集 ID（`cntr_`/`loc_` 前缀 + 字符集），云元数据地址、主机名、完整 URL 三种形状全 400；`path` 里塞完整 URL 也 400；探测只打 `127.0.0.1:port` |
| 分组与搜索 | 过滤按 `group_id`；形状对但分组不存在 → 200 + 空；**形状不对 → 400**（不静默变成“全部”）；`q` 不是服务端参数，搜索在页面（结果集大小不变）；非空分组删不掉（422 `DEPENDENCY_BLOCKED`），分组里的应用没有被顺手移走 |
| 常用入口 | 收藏是**按用户**存的（viewer 收藏后 admin 的列表里仍未收藏）；重复收藏幂等；首页常用入口只取 `capabilities.open && public_url` 的应用 |
| 启停走真路径 | 卡片上的 stop/start 发的是 **`service.control`**（`{unit, operation}`），走预检 → 任务 → `observed_state` 核验；受保护单元预检 `allowed=false`、不产生任务、`ssh.service` 现场仍在跑 |
| 依赖卷门禁 | 卷未挂载 → 预检 `allowed=false` + `VOLUME_OFFLINE`，提交 409，**容器真的没被启动**（`docker inspect` 仍是 `exited`）；stop 不被拦；卷挂上后同一份 start 预检 `allowed=true`（同一条应用的正对照） |
| 机器还原 | 收尾用产品自己的 `storage.volume.forget` 退出管理：fstab 里不再有那个 UUID，卷列表里不再有它；13 条应用、2 个分组全部删净（全量列表回到 0 条、分组回到 0 个） |

这一轮**没有**改松任何断言，也**没有**为了让脚本过而改产品代码。唯一改动到仓库契约文字的是
`api/openapi.yaml` 里 `group_id` 那一段描述（详见 §5 与 DECISIONS **Q277**）：代码本来就是对的，
写错的是描述。

## 2. 范围（做了什么、没做什么）

已做（STAGES §11 第 5 条的全整内容）：

- 应用卡片：一张卡同时画出运行时、入口、依赖保护、运行时说明四件事，各带自己的标签与颜色语义。
- 搜索：页面侧匹配名字 / 描述 / 绑定对象（unit、container_id），**不匹配 id**，也不会把 `undefined` 当成可匹配文本。
- 分组：`sort_order` 升序 + 同名按名字；非空分组不可删；分组过滤走服务端 `group_id`。
- 一键打开：`open` 是**锚点**（`target="_blank"`、`rel="noopener noreferrer"`），不是发请求的按钮；目标地址是服务器地址。
- 状态区分：`runtime_state`（11 值/4 枚举）、`http_state`、`dependency_protection` 三者独立，任何一格读不到就写 unknown 并带理由。
- 常用入口首页集成：收藏按用户、幂等；首页只取能打开的应用，收藏在前、其次按最近更新，最多 8 条。
- 本机候选与失效绑定：发现面板列出候选（`cntr_`/`loc_`），`missing_bindings` 点名要求重新确认，**不按名字自动接手**。

没做（明确不在本轮）：

- **没有**新增任何后端接口。P09.5 全是 `web/src` 的渲染与交互；`api/openapi.yaml` 只改了一处描述。
- **没有**引入 `app.control` 这条动作。启停必须落到 `service.control` / `container.control`——绕过它们等于绕过 `internal/appruntime/gate.go` 里的依赖卷门禁。这条现在同时写在 `web/src/api/apps.ts` 的注释与断言里（回归锁：`controlRequest` 对 link 返回 `null`）。
- **没有**做服务端搜索。`GET /apps?q=` 仍返回 200 且结果集不变——契约里没有这个参数，页面自己筛。
- **没有**在页面里做任何真实系统写操作。页面只发 HTTP；写操作仍然过预检、幂等、锁、审计。

## 3. 验收行对应（STAGES §11 第 5 条）

| 验收行 | 证据（`run.log`） |
|---|---|
| link 没有启停按钮 | §3.1：link 的 `start/stop/restart/logs` 全 false，只有 `open=true`；§3 又逐项用 SQLite 复核绑定 |
| 容器 running 但 HTTP 500 时明确异常 | §4：同一条应用 `runtime_state=running` + `http_state=error` + `status_code=500` + `last_checked_at` 有值，且**仍给得出 stop** |
| 容器重建 ID 变化后旧绑定要求重新确认 | §5：旧绑定 `unknown`、`runtime_note` 含“重新确认绑定”、四个能力全 false、库里仍是旧 ID、发现结果把它列进 `missing_bindings` |
| 入口在远程浏览器打开服务器地址 | §6：入口是 `http://10.0.2.15:18897/`，该地址真回 200；同端口回环 000；三种危险 `public_url` 全 400 |
| 不可探测云元数据 / 其他主机 | §6：`target_id` 填 `169.254.169.254`、主机名、完整 URL 全 400；`path` 塞 URL 也 400；形状合法但不在发现结果里 → 422 |
| 卷离线禁止启动受管理依赖应用 | §9：`VOLUME_OFFLINE` 预检 + 409 提交 + `docker inspect` 仍 `exited`；stop 不拦；卷挂上后 `allowed=true`（正对照） |
| 外部应用不能保证启动依赖时必须标记未接管 | §9：`restart=always` 与 systemd 一律 `unmanaged`，note 点名是哪条路径绕开了面板；`managed` 只留给「docker + restart 是 no/空 + 有依赖卷」 |

> 脚注要求「不要为了达到"支持已有项目"而开放任意配置执行」：本轮所有写操作仍然只有既有动作名
> （`service.control` / `container.control` / `apps.*` / `app-groups.*` / `storage.volume.*`），
> 参数是闭集结构（`unit` 是单元名、`container_id` 是容器 ID、`target_id` 是发现结果里的 ID），
> 没有任何地方接受自由命令或自由路径。

## 4. 前端侧证据（真机载荷回放）

`web/src/fixtures/p09-5-vm/` 里的 16 份载荷是从这台 VM 上原样取回的，
`web/src/components/realdata-p09-5.test.ts` 用 `renderToString` 回放，**51 条断言**：

- `App` 的键必须是 openapi 闭集的子集（多一个都会红）。
- 三状态分开：`runtime_state` / `http_state` / `dependency_protection` 各自成组，`HTTP_STATE_LABELS` 与 `RUNTIME_STATE_LABELS` 全覆盖。
- 能力来自对象：受保护单元只剩 `logs`；`rebuilt` 四个能力全 false；`link` 没有生命周期。
- 入口：`href` 就是服务器地址，`target="_blank"`、`rel="noopener noreferrer"`；没登记入口的应用 `openTarget().href === ''` 并给出理由。
- 卡片：500 那一张同时出现“运行中”与“入口异常”；依赖保护那一张写着“面板是唯一的自动启动者”。
- 分组：收藏格是额外的一格（不重复计数），搜索时隐藏收藏格，未分组在最后。
- 启停：`controlRequest` 出的是 `service.control` / `container.control`，参数逐字段相等；link 返回 `null`。
- 任务：`summarizeAppJob` 读的是 `observed_state` 里的真实读数（`previous_state=active` → `active_state=inactive`）。

`npm run test` 全量 **40 files / 720 tests passed**，`typecheck` / `lint` 干净（`native-tests.txt`、`check.txt`）。

## 5. 本轮修的是什么（4 处失败）

第一遍真机跑下来 **PASS=200 / FAIL=4 / NOT RUN=0**。逐条查清，没有一条是产品代码的缺陷：

| 失败 | 原因 | 处理 |
|---|---|---|
| 形状不对的 `group_id` 期望 200、实际 400 | **契约文字写错**：openapi 描述说“形状不对按没有这个分组过滤，不报错”，而 `internal/apps/store.go` 返回 400 | 判定**代码是对的**：被默默忽略的过滤器与「读不到不能变成没有」是同一类不诚实——调用方以为在看某个分组，实际在看全部。改 `api/openapi.yaml` 的描述（DECISIONS **Q277**），脚本改为断言 400 + `VALIDATION_ERROR` + 消息点名分组标识 |
| `…消息点名 group_id` | 服务端消息是「分组标识必须是 grp_ 加 32 位小写十六进制」，中文点名，不含字面量 `group_id` | 断言改成匹配「分组标识」（消息本身没有要改的地方） |
| 停机之后卡片上应该出现 start | `appget` 只能接字段后缀，把它用在整表达式上拼出的是语法错误 | 新增 `appexpr`（整表达式入口）与 `wait_app`（轮询到快照跟上） |
| 启动之后卡片上应该出现 stop | 同一条：**能力会晚一拍**——`runtime_state` 来自 services 采集器的快照，默认 10 秒一轮，刚发完 `service.control` 摘要还说着上一轮的事实 | 不是缓存出错，是「摘要如实描述它读到的那一轮」。断言改成轮询（最长 60 s，读到就返回），页面的行为也写进夹具 README |
| 夹具分组已全部删除（剩 1 个） | 删除循环每次都覆盖 `resp.json`，第二个分组的 `version` 读成了上一次删除响应里的 0 | 先把分组列表整份存下来再逐条删 |

另外顺手在第一次失败后补了两件事（都是为了让断言更贴验收行，不是为了让红变绿）：

- 多建两个真能打开的入口应用（下载站、图库），并当场 `curl` 它们各自的地址证明 200——常用入口那几条
  才有“能打开”的东西可测，而不是三个 URL 形状的字符串。
- 把图库放进「媒体」分组，让那个分组里真有两条成员（分组过滤、`group_id` 与分桶才有意义）。

## 6. 未验证 / 不在本证据内

- **没有**在真实浏览器里点：本轮页面证据是 SSR 渲染 + 断言（DECISIONS **Q246** 的约定），
  没有 Chrome/Playwright 截图，也没有人工点检。
- **没有**跨浏览器与移动端布局验证。
- **没有**做并发压力：两份请求同时收藏同一条应用、两个管理员同时改同一个分组，本轮没测。
- **没有**测大数量：应用条数最多 13 条，分页与虚拟滚动的表现不在本轮范围。
- P09.5 **不涉及**分区/格式化/网络断连，所以没有对应的破坏性测试；脚本也没有留下任何系统盘写入。

## 7. 文件清单

| 文件 | 内容 |
|---|---|
| `env.txt` | VM 环境（内核、发行版、docker/systemd 版本、磁盘拓扑、身份、端口、二进制 sha256） |
| `check.txt` | 宿主侧 `sh scripts/check.sh`：**PASS 13 / FAIL 0 / NOT RUN 0** |
| `native-tests.txt` | `go test -count=1 ./...` + `go vet` + `npm run test`（720 条） |
| `run.log` | 真机验收全文（13 节，211 条 PASS） |
| `payloads/` | 抓回来的载荷（应用/分组/发现/健康/日志/预检/任务/卷） |
| `logs/agent.log`、`logs/web.log` | 代理与 Web 在这次运行里的日志 |
| `web/src/fixtures/p09-5-vm/` | 回放用的 16 份载荷 + README（为什么必须是真的） |
