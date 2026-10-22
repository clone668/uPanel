# P09.3 验收结果 — Docker 只读查询、容器日志与现有受支持容器的启停

证据类型：**Linux VM integration**（一次性 Ubuntu 24.04.5 虚拟机，root 起代理、uid 999 起 Web）
执行时间：2026-10-22（UTC，虚拟机时钟）
脚本：`tests/integration/p09-3-container-acceptance.sh`
环境：见 `env.txt`；宿主检查见 `check.txt`；单元测试见 `native-tests.txt`

---

## 1. 结论

| 项 | 数据 |
|---|---|
| 最终一遍（第五遍，`run.log`） | **PASS=217 / FAIL=0 / NOT RUN=1**（退出码 0） |
| 唯一 NOT RUN | 「执行前复核（受理之后再变）」——受理时的复核已经把状态变化挡在 202 之前；要在受理与执行之间改状态需要可重复的竞态，真机构造不出来。由 `internal/agent/containers_test.go` 的 `TestContainerControlExecuteStaleState` 覆盖 |
| 真机发现的产品缺陷 | **3 条**（Q270 docker `--tail` 与 `--until` 的执行顺序让往回翻页永远只能拿到边界一条、Q271 套接字还在但守护进程死了被当成“命令失败”甚至 500、Q272 代理说的 5xx 域内码在 Web 层被改写成 `AGENT_UNAVAILABLE`），全部修完重验 |
| 危险容器 | 四类（privileged / host 网络 / 危险 capability / 挂 docker.sock）在真机上分别起容器：`risk.level=high`、`managed=false`、三项能力全 false，预检就拒（`CONTAINER_NOT_MANAGED`），提交被拒的预检是 409 且容器**真的没被动过** |
| 容器身份 | 只认完整 64 位小写十六进制 ID；名字、短 ID、大写、把 `--last` 当 ID 一律 400，且注入尝试之后容器数量未变 |
| 秘密 | 容器环境变量**从不被请求**（`Config.Env` 不在 inspect 的格式串里，清单里搜不到 `APP_DB_PASSWORD` 这个变量名）；日志里的口令被 `logging.Redact` 成 `[redacted]` |
| 启停 | stop / start / restart 三条都走“预检 → 任务 → 读回”，并每条都用 `docker inspect` 现场对照：**退出码 0 不算成功，读回状态一致才算** |
| 两种不可用 | 代理停掉是 503 `AGENT_UNAVAILABLE`；dockerd 停掉是列表 200 + `available=false` + `notice`、详情与日志 503 `DOCKER_UNAVAILABLE`、预检 `allowed=false` + 阻塞码 `DOCKER_UNAVAILABLE` |

这一轮改的**都是产品代码**，不是把断言改松。三条缺陷都是真机跑出来、能说清“谁会被这句话误导”的问题。
前几遍跑出来的失败里有 6 条是脚本自己的毛病（见 §5），也逐条写清了。

## 2. 范围（做了什么、没做什么）

已做（STAGES §11 第 3 条的全部内容 + PLAN §12 的容器部分）：

- **只读查询**（`internal/containers`）：容器（`docker ps -a --no-trunc` + 逐条 `inspect --format`）、
  镜像、网络、数据卷、compose 部署视图。全部走**固定的 docker 子命令**，Web 进程从不碰
  `docker.sock`、不执行任何命令——真机上两条都验了（Web 账号不在 `docker` 组、`docker ps`
  被拒；`/proc/<pid>/fd` 里没有 docker.sock）。
- **容器身份**：`^[0-9a-f]{64}$`。Web 与代理各校验一次，因为 ID 是唯一会进 argv 的可写字符串。
  短 ID 也不行——短 ID 可以靠猜，全 ID 不行。
- **风险判定**（闭集）：privileged、host 网络/pid/ipc、危险 capability、挂 docker.sock、
  挂宿主 `/`。命中即 `risk.level=high` + `managed=false` + 三项能力全 false，并在**两处**拒绝：
  预检（`CONTAINER_NOT_MANAGED`）与执行前复核（`controlBlockedError` → 409 `PREFLIGHT_STALE`）。
- **不请求不该要的东西**：inspect 格式串里没有 `Config.Env`、没有完整标签表（只取六个
  `com.docker.compose.*`）、没有 raw inspect 的输出。环境变量里通常就是数据库口令。
- **日志**：从新到旧一页（`--tail`）、时间键集往回翻页（见 Q270）、每账号 4 条并发流、
  流上限 10 分钟 / 空闲 60 秒 / 5000 条 / 8 MiB。响应里带 `from`/`to` 说明“看的是哪一段”。
- **启停**：`container.control`（stop/start/restart）走既有的预检 → 任务 → 幂等 → 资源锁 → 审计
  通道；锁是容器自己（同一个容器同时只能有一个任务）；读回验证写在结果里（`observed_state`）。
- **compose 只读**：项目由 `com.docker.compose.project` 标签归并，`id = "dep_"+sha256(项目名)[:32]`，
  三项能力固定 false；**只做精确匹配的 GET**，按项目名模糊查是 404。外部 Compose YAML 不执行、
  不解析、不落盘——面板只是“看见”它。

没做（属后续子任务）：

- 应用启停/日志与依赖卷的联动（P09.4）——所以本轮的“受管理”只到容器这一层。
- 前端页面（P09.5）——本轮只有 HTTP 接口。

## 3. 逐节断言（`run.log`）

| 节 | 内容 | PASS |
|---|---|---|
| 0 | 环境与安全守卫（root、二进制、Web 身份无 shell、端口空闲、docker 29.1.3、Web 账号不在 docker 组） | 10 |
| 1 | 配置与启动（代理 root 建 socket、Web 监听进程 uid=999、迁移到第 8 版、登录与 CSRF） | 9 |
| 2 | Docker 夹具（七个容器：普通、已停止、日志、privileged、host 网络、危险 cap、挂 sock） | 5 |
| 3 | 容器清单：字段、风险判决、四类危险容器、挂载、环境变量不外泄 | 44 |
| 4 | 容器身份只认 64 位 ID；名字/短 ID/大写/tail 越界 400；注入无效果 | 14 |
| 5 | 日志：从新到旧、游标往返（严格更早、无边界重复）、12 种参数拒绝、脱敏 | 14 |
| 6 | 镜像 / 网络 / 数据卷（含“这个卷被谁占着”） | 13 |
| 7 | 部署：compose 项目只读视图、能力全 false、项目名不模糊匹配 | 12 |
| 8 | 启停：stop / start / restart 各一轮预检→任务→读回 + `docker inspect` 现场对照 | 24 |
| 9 | 拒绝：四类危险容器、状态冲突、参数注入（名字/flag/多余参数/闭集外操作）、陈旧预检 409 + `PREFLIGHT_STALE` + 作业留痕 | 37 |
| 10 | SSE：能收到 entry 事件、事件 id 是时间戳、不存在的容器是 404 而不是 error 帧 | 5 |
| 11 | 两种“不可用”分开：代理停 / dockerd 停 | 19 |
| 12 | 权限（viewer 全拒）与审计（`container.control` 预检、任务提交、被拒预检） | 11 |
| — | 合计 | **217** |

## 4. 真机抓出的产品缺陷（3 条，均已修并重验）

### Q270 — `docker logs --tail N --until 游标`：往回翻页永远只拿到边界一条

第四遍跑到 §5「第二页每条都严格早于游标」「第二页不是空的」两条失败。手工在 VM 上复现：
容器有 21 行日志，取最新 5 行拿到游标，再 `docker logs --tail 5 --until <游标>`——**只回一行**，
正是游标那条本身（再被去重逻辑丢掉，于是第二页是空的）。

原因是 docker 的执行顺序：先取“整个流的最后 N 行”，**然后**才按 `--until` 过滤。
比游标早的行在过滤之前就已经被 tail 丢光了。这不是参数没写对，是 `--tail` 与 `--until`
组合起来的语义缺陷。

修法：往回翻页改成时间窗 `--since … --until 游标`，窗口初始 15 分钟，没凑满就放大 8 倍，
放大到铺住**容器创建时间**（一次 `docker inspect --format {{.Created}}` 取到，是“更早的确实
没有了”的可靠下限）或 30 天上限；单次输出撞上 8 MiB 上限就缩小 4 倍（丢掉的是窗口靠后、
最接近游标的那段，缩窗才是对的）。单次翻页最多问 docker 8 次，不收敛就明确报错而不是给一份
缺了要紧部分的页。

`truncated` 的语义也跟着钉死了：**只有**“满页”“撞字节上限”或“窗口还没铺到容器创建时间”才是
`true`；窗口铺到创建时间就是 `false`——不让“可能还有”冒充“确实没有了”。

### Q271 — 套接字还在、守护进程死了：被当成“命令失败”甚至 500

同一遍里 §11.2 落到 NOT RUN：脚本在等 `/var/run/docker.sock` 消失，而 `systemctl stop docker`
之后 socket 文件**可能留在原地**。回头看发现是两层问题叠在一起，其中一层是产品的：

产品只按“套接字文件在不在”判断 Docker 可用。于是 dockerd 半路死掉（`systemctl stop docker`、
OOM 杀掉、升级中重启——运维最常遇到的三种）时，列表接口会回 503 而不是契约要求的
200 + `available=false`，`container.control` 预检则直接 500。

修法：判据改成 **docker 自己那句话**（`Cannot connect to the Docker daemon` /
`Is the docker daemon running?` / `connect: connection refused`），一律翻成
`UnavailableError`；清单类接口（容器/镜像/网络/卷）遇到它降级为 200 + `available=false` + `notice`；
预检遇到它写成 `allowed=false` + `DOCKER_UNAVAILABLE` 阻塞项（`DOCKER_UNAVAILABLE` 是**拒绝理由**，
不是“预检算错了”）。

### Q272 — 代理说“Docker 不可用”，Web 改写成“代理不可用”

第四遍剩下的唯一失败：dockerd 停掉后读 `/containers/:id`，状态码 503 正确，`code` 却是
`AGENT_UNAVAILABLE`。根因在 Web 侧 `containers.Fetcher.mapError`：它只按状态码分类，
把代理的任何 5xx 都当成“代理自己不可用”。

这条契约（CONTRACTS §8.3(5)）因此在 Web 这一层被抹掉，而它是会误导处置方向的：
页面说“等代理一会儿”，实际要做的是把 dockerd 起起来。

修法：带域内码的 5xx 原样透出（`503 DOCKER_UNAVAILABLE`、`409 PREFLIGHT_STALE`…），
只有通用码（空 / `AGENT_UNAVAILABLE` / `INTERNAL_ERROR`）和连不上才翻成 `AGENT_UNAVAILABLE`。
`agentNotDomainCode` 一处定义这个判断；新增 `TestFetchKeepsDockerUnavailable`、
`TestFetchKeepsPreflightStale`、`TestFetchMapsAgentGeneric5xxToUnavailable`、
`TestContainerDetailDockerUnavailable`，并在验收脚本里给**日志**补一条同款断言
（同一个判决的两条路径都走一遍，不然修了详情漏了日志没人知道）。

## 5. 验收脚本自身的缺陷（6 条，与本轮产品代码无关）

| # | 现象 | 根因 | 修法 |
|---|---|---|---|
| 1 | §5「条目从新到旧」偶发失败 | 直接按字符串比 RFC3339Nano：小数尾零被去掉后 `".5Z"` 逐字符比 `".55Z"` 更大，数值上却更小 | 时间比较改成先解析成 `(UTC 秒, 纳秒)` 再比（`tskey`） |
| 2 | 整段脚本突然中断（「Bad substitution」） | `${_bad:0:14}` 是 bash 专有写法，而脚本 shebang 是 `sh`（Ubuntu 上是 dash） | 改成 `cut -c1-14` |
| 3 | §3「Web 账号连不上 docker.sock」失败 | 断言写成了 `expect_eq … "0"`，但这里要的恰恰是**非 0** | 改成 `expect_ne` |
| 4 | §9「提交陈旧预检」期望 202 得到 409 | 脚本假设“受理后执行时才失败”；契约 §5/§1536 规定**受理时**就复核一次，变了就是 409 `PREFLIGHT_STALE`，并留一条 `failed` 作业 | 断言改成 409 + `PREFLIGHT_STALE` + 作业表多一条且 `error_json` 里有这个码；执行前复核标 NOT RUN 并指名单测 |
| 5 | §10 SSE 只收到 `: connected` | 开流前有“容器存在性预检 + 许可 + 打开”三步，吃掉 curl 5 秒预算 | `--max-time 20`（断言不变，只放宽等待） |
| 6 | §11.2 落到 NOT RUN | 等的是 socket 文件消失，而文件会残留 | 改成等 `docker info` 失败（顺带把产品缺陷 Q271 逼了出来） |

## 6. NOT RUN（1 条，原因写在这里，不在脚本里假装通过）

| 项 | 原因 | 替代证据 |
|---|---|---|
| 执行前复核（受理之后再变） | 受理时的复核已经把状态变化挡在 202 之前。要在“受理”与“执行”之间改容器状态，需要一个可重复的竞态窗口（毫秒级），真机上构造不出来；硬造出来也只是“碰巧赢了一次” | `internal/agent/containers_test.go` 的 `TestContainerControlExecuteStaleState`：直接调执行入口，断言 409 `PREFLIGHT_STALE`，并断言容器没有被停 |

脚本头部另有三条**声明式**不覆盖（不是执行后才发现）：浏览器渲染（VM 无浏览器，页面级证据在宿主
回放）、其它 Docker 版本/加速器（本机只有 apt 的 `docker.io`，验收范围就是它）、“docker 返回 0 但
状态没变”（需要一个人为骗人的 docker，正常 daemon 上做不到，由 `internal/containers` 的 fake 覆盖）。

## 7. 已实现 / 已验证 / 未验证 / 受阻

**已实现（本轮新增）**

- `internal/containers`：`types.go`（视图与风险闭集）、`parse.go`（inspect 格式串与解析）、
  `docker.go`（固定子命令 + 不可用分类）、`logs.go`（分页与流）、`control.go`（预检判决与执行前复核）、
  `http.go`（判决 → HTTP 的唯一映射）、`fetch.go`（Web 侧读取端，含 Q272 的 5xx 分类）。
- 代理侧：`internal/agent/containerquery.go`（7 个查询）、`containeraction.go`（`container.control`）、
  `registry.go`（能力由注册表推导）、`streams.go`（流许可与并发额度）。
- Web 侧：`internal/api/containers.go`（7 个 GET + 1 个 SSE），全部 admin。

**已验证（真机 + 单测）**

- 上面 13 节 217 条断言，`FAIL=0`。
- `go test ./...` 全绿；容器相关单测 57 个用例（`internal/containers` 26、`internal/agent` 15、
  `internal/api` 13、`internal/containers/fetch_test.go` 3）。
- 宿主 `scripts/check.sh`：**PASS 13 / FAIL 0 / NOT RUN 0**（含 gofmt、Linux 交叉编译、前端四项、
  openapi lint 7 warning）。

**未验证**

- 前端页面（P09.5 才做）。
- 除 `docker.io` 29.1.3 之外的 Docker 版本（本机只有它）。
- 危险容器的“执行前复核”拒绝（预检就拒了，见 §6）。

**受阻**：无。

## 8. 已知限制

- **日志往回翻页是按时间窗猜的**：安静的容器要多问几次 docker（最多 8 次），话多的容器要缩窗。
  这是 docker 没有游标的直接后果，不是实现偷懒；页面上没有“翻得多慢”的承诺。
- **`truncated=false` 只在窗口铺到容器创建时间时给出**：更早的确实没有了。容器 `Created` 之后的
  时间窗若被 8 MiB 上限截断，宁可缩窗重问，也不接受一份缺了靠游标那段的页。
- **compose 项目只读**：`dep_` id 是派生的，不带项目名原文（项目名可能是内部信息）；按名字查
  是 404，因为“模糊匹配一个只读视图”没有用处却会误导人以为可以按名字操作。
- **容器名字不进 argv**：所有子命令的目标只有完整 ID，名字只在输出里出现。

## 9. 下一子任务

**P09.4**（STAGES §11 第 4 条）：关联 systemd/容器的启停、日志、状态和依赖卷；受管理/未接管
依赖保护明确显示。P09.3 已经把“容器这一层的启停与读回”和“依赖卷的身份（`volume_id`）”
备好，P09.4 要把两者与应用元数据（P09.1）和依赖卷保护（P08）接起来。
