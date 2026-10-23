# P11.1 结果：软件包与更新只读页

日期 2026-10-23 · 提交 `6551ff9` · 版本标注 `0.0.0-p11.1`
（环境、二进制哈希、虚机清单见同目录 `env.txt`）

## 这一轮做了什么

STAGES §13 第 1 条：**读**。装了哪些包、有哪些候选、哪些是安全更新、要不要重启。
一个读的页面，最难的部分不是搬字段，是**别把“读不到”说成“没有”**。

事实与判断分开：

- **代理出事实**。`apt.packages` 返回 dpkg 的登记（`${binary:Package}`、状态缩写、
  已安装版本、架构）加上 `apt-cache policy` 给出的候选版本、候选所在套件、优先级。
  `system.power_state` 返回重启标志文件、`/run/reboot-required.pkgs`、内核 `boot_id`。
- **Web 算判断**。`candidate_relation`（`newer` / `same` / `older` / `unknown`）、
  `upgradable`、`security`、`pockets`（把 `noble-security` 这样的套件名归到
  `security` / `updates` / `backports` / `release` 四类）、`counts`、分页、筛选。
  版本比较用 `internal/debversion`，不是字符串比较，也不是 semver。

`counts` 是**全量统计**，不随筛选和翻页变化；`matched` 是“筛选之后、翻页之前”的条数。
两个数字放在同一个响应里，就是为了让页面能同时说“共 731 个包”和“筛出 1 条”。

## 代码清单

| 位置 | 内容 |
| --- | --- |
| `internal/apt/parse.go` | `dpkg-query`、`apt-cache policy`、`/run/reboot-required*` 的解析；`***` 那条是**已安装**、候选套件只从 `Candidate:` 对应块的版本行取 |
| `internal/apt/collector.go` | 现场采集：一次 `dpkg-query` 出一份包名表，一次 `apt-cache policy` 批量问候选；30s 快照 + 单飞 + 候选缓存（键 = 已装版本 + `/var/lib/apt/lists` 的 mtime） |
| `internal/debversion` | dpkg 的版本比较规则（epoch / 上游版 / Debian 修订、`~`、数字与字母分段） |
| `internal/packages` | 事实 → 视图：关系、可升级、安全、来源归类、筛选、分页、`counts` |
| `internal/api/packages.go` | `GET /api/v1/packages`、`GET /api/v1/packages/updates`、`GET /api/v1/system/power-state`；参数校验（`limit` 1–200、`installed`/`upgradable`/`security` 只收 true/false、`cursor` 必须是 base64、`query` 拒控制字符） |
| `internal/agent/aptquery.go` | `apt.packages` / `system.power_state` 两条查询，以及它们自己的写超时 |
| `web/src/api/packages.ts` | 类型 + 展示规则（`candidateVersionText` / `entryStatusLabel` / `entryPocketText` / `entryDisplayName`） |
| `web/src/views/UpdatesView.vue` | “软件包”与“更新”两个标签页、筛选、翻页、空态、错误态 |
| `tests/integration/p11-1-apt-acceptance.sh` | 真机验收 64 项 |
| `tests/integration/p11-1-packages-verify.py` | 独立对账：不信 Web 的实现，自己跑 `dpkg`/`apt-cache` 再比 |
| `web/src/components/realdata-p11-1.test.ts` | 真机载荷回放 19 项 |

**能力表里没有任何 `package.*` 动作。** 这是本轮唯一的“写”保护：只读就是只读，
装/卸/升级/刷新索引在 P11.2，重启执行在 P11.3。验收里有一条直接断言
`supported_actions` 里 `package.*` 为空。

## 验收结果

两台专用虚机、同一份二进制（`agent ac17bec7…` / `web d40ad6c1…` / `ctl bf29426f…`）、
同一份脚本（`p11-1-apt-acceptance.sh fb57f279…`）：

| 机器 | 发行版 | 包数 | 首次采集 | 对账 | 结果 |
| --- | --- | --- | --- | --- | --- |
| VM-A | 22.04.5 / apt 2.4.14 | 676 | 411ms | `MISMATCH=0` | **PASS 64 / FAIL 0 / NOT RUN 7** |
| VM-B | 24.04.5 / apt 2.8.3 | 731 | 11611ms | `MISMATCH=0` | **PASS 64 / FAIL 0 / NOT RUN 7** |

（脚本自己的汇总行也算一行 `PASS`，所以 64 = 63 项检查 + 汇总行。）

分节（两台机器逐节一致）：

| 节 | 内容 | PASS |
| --- | --- | --- |
| 0 | 发行版与包管理现场 | 1 |
| 1 | 启动代理（root）与 Web（非 root） | 7 |
| 2 | 能力表如实报告两条查询、且没有 `package.*` 动作 | 4 |
| 3 | 登录与权限（未登录 401 / viewer 403 / admin 200） | 12 |
| 3b | 首次采集只等不判断 | 1 |
| 4 | `GET /packages`：字段、`counts`、筛选、翻页、30s 快照 | 13 |
| 5 | 与 `dpkg`/`apt` 逐条对账（731 条全查） | 2（+2 NOT RUN） |
| 6 | `GET /packages/updates` | 5 |
| 7 | `GET /system/power-state`（`boot_id` 与内核一致） | 4 |
| 8 | 只读性：`/var/lib/dpkg/status` 的 sha256 与 apt 索引 mtime 未变 | 2 |
| 9 | 参数拒绝 + 筛选生效 + arch 限定名 | 13 |
| 10 | 明确未覆盖 | 5 NOT RUN |

`NOT RUN` 的 7 项：这台机器没有 `rc`（仅剩配置）的包，两条 `rc` 检查无法执行；
`apt-cache` 失败降级（要让 `apt-cache` 真失败会破坏验收机的 apt，该分支单测覆盖）；
安装/卸载/升级/刷新索引（P11.2）；分阶段推送与被保留包（P11.2）；重启执行与
`boot_id` 前后确认（P11.3）；浏览器渲染（VM 上没有浏览器，页面数据由真机夹具回放）。

## 真机抓出的缺陷：arch 限定名的候选全被谎报成“读不到”

`dpkg-query -W -f='${binary:Package}'` 对带架构限定的包给的是 **`bind9-libs:amd64`**；
而 `apt-cache policy bind9-libs:amd64` 打出来的块头是 **`bind9-libs:`**——apt 自己把
`:arch` 剥了。按请求名去索引这份输出，键永远对不上，于是所有这类包的
`candidate_version` 都是 `null`、`candidate_relation` 是 `unknown`。页面上就是
“装着的包，候选读不到”，而且不报错、不警告，看起来像机器本身没有候选。

这一条是**在对账时抓到的**，而且抓到的姿态很值得记住：第一次跑出来的结果里，
Web 说 `null`、对账脚本说 `None`——两边一致，验收脚本却判 `MISMATCH`，因为对账脚本
自己也是按请求名索引的，它复刻了同一个 bug。真正独立的判据是肉眼在 VM 上看
`apt-cache policy bind9-libs:amd64` 的输出：表头 `bind9-libs:`，`Candidate:` 有值。
**只有把对账脚本改成另一种解析方式，它才有资格当对账脚本。**

修法：查表先按请求名，落空再按基础名回退，但回退受一张**歧义表**保护——同一个基础名
装了两个架构（`bash:amd64` 与 `bash:i386` 同时存在）时，表头分不清是哪一边的候选，
那就**不回退**，如实报 `unknown`。宁可说“读不到”，也不猜。

修完两边都是 0 失配（731/676 条全查）。夹具里留了 `packages-bind9.json`，
`bind9-libs:amd64` 的候选是 `1:9.18.39-0ubuntu0.24.04.8`；回放测试里有一条专门断言
它**不是** `null`——这条回归是被真机抓出来的，不能再悄悄退回去。

顺带一处展示规则：`name` 本身就是 `${binary:Package}` 的原文，已经带着 `:amd64`，
页面上再拼一次就成了 `bind9-libs:amd64:amd64`。`entryDisplayName()` 按
“`name` 是否已经以 `:arch` 结尾”判断，`all` 架构不加后缀。

## 性能：24.04 的 apt 慢 28 倍，超时得跟着分层

同一段代码在两台机器上量出来：

| | 22.04 / apt 2.4.14 | 24.04 / apt 2.8.3 |
| --- | --- | --- |
| 一次 `apt-cache policy`（676/731 个包名） | 384ms | 26.6s（≈36ms/名） |
| 首次调用到响应返回 | 411–552ms | 11.6s / 12.5s / 15.6s |

后果是一串默认值都不够用：`agentclient` 的 10s、Web 的 30s 写超时、前端的 15s。
现在的分层是 **45s（`packages.AgentQueryTimeout`）→ 60s（Web 单请求写超时）→ 120s
（代理侧写超时）→ 30s（快照 TTL）**，前端 `PACKAGE_QUERY_TIMEOUT_MS = 60_000`。

**启动预热被删掉了。** 曾经在 `main` 里起一个 goroutine 开机就采一次，实测在开机
争抢下把 26s 拖成 62s，反而让健康检查误报 `AGENT_UNAVAILABLE`。现在第一次请求自己付
这笔钱，第二次起命中 30s 快照；候选缓存的键是“已装版本 + 索引 mtime”，索引没动就不
重问 apt。

## 已知限制（本轮没解决的，写在这里）

1. **`rc`（仅剩配置）与 `security` 筛选的边界只在单测里覆盖过。** 两台验收机上
   没有 `rc` 包，这两项是 NOT RUN，不是 PASS。
2. **`apt-cache` 失败时的降级只走了单测。** 真让 `apt-cache` 失败会破坏验收机的
   apt，所以真机上没做；降级路径是“候选全 `unknown` + `errors` 里写原因”，页面上
   显示“读不到”而不是“已是最新”。
3. **`apt-get -s upgrade` 的分阶段推送还没读。** 现在 `upgradable` 来自
   `candidate > installed`，而 apt 真升级时可能因为依赖保留而少装几个；这个差别要到
   P11.2 才判。
4. **`GET /packages` 每次最多 200 条**，全量 731 条要翻 15 页（实测 15 页、0 重复）。
   没有做“一次导出全部”，因为页面不需要，也没有做服务端搜索索引。
5. **`p02-3-agent-acceptance.sh` 的期望清单改了但没重跑**：`storage.partition.table.initialize`
   → `storage.partition_table.initialize`（脚本里写错了，真实能力表里是后者）。
   本轮只改了字符串，脚本未执行，记为 NOT RUN。

## 下一子任务

**P11.2**：刷新索引、安装、卸载、升级（含 `apt-get -s` 模拟、保护名单、资源锁、
非交互配置、不可取消阶段、结果校验）。这一轮才开始有写路径，权限、预检、任务、
幂等和审计都要跟着接上。
