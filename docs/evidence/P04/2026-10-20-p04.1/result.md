# P04.1 验收结果 — 系统基础资源采集、轮询缓存与时间戳

日期：2026-10-20　　实施者：本项目执行者　　结论：**passed**（代码 + 两台 VM 验收均完成）

本文件只写**实际执行过**的检查。未执行的检查在第 6 节单列（NOT RUN），不计入通过。

---

## 1. 本轮交付内容

| # | 交付物 | 文件 | 对应验收点（STAGES P04.1 / PLAN §8 / CONTRACTS §3、§15） |
|---|---|---|---|
| 1 | 采集 DTO：段级 `status`/`error`/`source`/`collected_at`，`Summary` 带 `interval_ms`/`stale`/`degraded` | `internal/monitoring/types.go`（新） | “轮询缓存与时间戳”；字节与计数器按 CONTRACTS §1 用十进制字符串 |
| 2 | `/proc` 解析器：`stat`、`meminfo`、`loadavg`、`net/dev`、`os-release`、`uptime`、`cpuinfo` | `internal/monitoring/procfs.go`（新） | “不解析本地化文本、不用零值冒充失败” |
| 3 | 5 秒轮询采集器：`Run`（立即采一次再 ticker）、`Refresh`、`Snapshot`（读取时算 `stale`）、`Summary`（冷启动同步采一次） | `internal/monitoring/collector.go`（新） | “轮询缓存与时间戳”；失败段保留最近有效值与旧时间 |
| 4 | 速率只报区间：相邻两次样本差分 / 时间窗；无前次样本、计数器变小、窗口非正 → `null` | `internal/monitoring/collector.go` | PLAN §8“计数器只报速率，不给累计值当下的假象” |
| 5 | `boot_id` 变化让 CPU 采样窗口失效（换启动不产生假尖峰） | `internal/monitoring/collector.go` | 段状态与数值语义一致性 |
| 6 | 代理侧查询 `system.summary`：零参数、只读快照、`DecodeParams` 拒绝任何字段 | `internal/agent/systemquery.go`（新）、`internal/agent/registry.go` | CONTRACTS §15“结构化只读查询”；**没有万能命令入口** |
| 7 | 能力如实上报：只有注入采集器时才登记 `system.summary` | `internal/agent/registry.go`（`ProductionDeps`/`RegisterProductionQueries`） | 宁可少报也不返回空壳（DECISIONS Q84） |
| 8 | 代理启动时拉起采集循环（信号 ctx 退出） | `cmd/upanel-agent/main.go` | 采集生命周期跟随服务 |
| 9 | Web 侧透传：`GET /api/v1/system/summary`（权限 V） | `internal/api/system.go`（新）、`internal/api/router.go`、`internal/api/server.go` | CONTRACTS §3 权限矩阵；代理不可用 503 `AGENT_UNAVAILABLE`（Q88） |
| 10 | Web 侧代理客户端：`Fetcher`，**不做第二层缓存** | `internal/monitoring/fetch.go`（新）、`cmd/upanel-web/main.go` | 时间戳必须是采集时间（Q81） |
| 11 | 契约：新路径与新 schema，版本 `0.0.0-p04.1` | `api/openapi.yaml` | 只登记已实现的路径 |
| 12 | 夹具：6 份真实 `/proc` 采集 + 7 份合成反例 + 来源记录 | `tests/fixtures/`、`PROVENANCE.md` | 解析器测试用真实样本（Q85） |
| 13 | 黑盒验收脚本 72 项断言 | `tests/integration/p04-1-system-acceptance.sh`（新） | 两台 VM 上执行 |
| 14 | 测试夹具查找：环境变量 / 二进制旁边 / 向上查找三层兜底 | `internal/testsupport/fixtures.go`（新）、`scripts/run-native-tests.sh` | VM 上测试二进制能找到真实样本，找不到就**失败**而不是跳过 |

产品代码**没有**新增任何写动作，也没有引入命令执行：采集全部是文件读取。

## 2. 宿主证据（开发机：Windows 11 + Git Bash）

| 检查 | 命令 | 结果 | 证据文件 |
|---|---|---|---|
| 构建 | `go build ./...` | exit 0，无输出 | `host-build.txt` |
| 静态检查 | `go vet ./...` | exit 0，无输出 | `go-vet.txt` |
| 格式 | `gofmt -l .` | 无输出 | `gofmt.txt` |
| 单元测试（全量） | `go test -count=1 ./...` | 19 个包 `ok`，exit 0 | `go-test.txt` |
| 单元测试（详细） | `go test -count=1 -v ./internal/monitoring/ ./internal/agent/ ./internal/api/` | **--- PASS 228 / --- FAIL 0** | `go-test-verbose.txt` |
| 统一闸门 | `sh scripts/check.sh` | **PASS 12 / FAIL 0 / NOT RUN 0** | `check.txt` |
| 契约 lint | `redocly lint --format=summary api/openapi.yaml` | valid，**9 warnings**（`no-unused-components` 7 + `operation-4xx-response` 2；比 P03.4 少 1 个，因为新 schema 用掉了原有的一个未引用组件） | `openapi-lint.txt` |
| 交叉编译 | `GOOS=linux GOARCH=amd64 go build` / `go test -c` | 三个入口 + 八个测试二进制 | `cross-build-sha256.txt` |

宿主的线格式测试（`internal/monitoring/types_test.go`）直接钉住 JSON：
`"total_bytes":"4009864"`（字符串）、`"usage_percent":12.5`（数字）、
`"rx_bytes_per_second":null`（缺样本时 null 而不是 0）、
`"collected_at":"2026-10-20T08:00:05.123456789Z"`。

## 3. 真实 Linux 证据（两台验收 VM）

| 项 | VM-B | VM-A |
|---|---|---|
| 发行版 | Ubuntu 24.04.5 LTS | Ubuntu 22.04.5 LTS |
| 内核 | 6.8.0-147-generic | 5.15.0-198-generic |
| 规格 | 2 vCPU / 4 GiB / `lo` + `ens4` | 2 vCPU / 4 GiB / `lo` + `ens4` |
| Python（验收脚本用） | 3.12.3 | 3.10.12 |
| Go 工具链 | 未安装（DECISIONS Q9），用宿主交叉编译产物 | 同左 |
| 环境记录 | `env-24.txt` | `env-22.txt` |

二进制逐字节一致：两台 VM 上 `upanel-web` / `upanel-agent` / `upanelctl` 与 8 个测试二进制
（共 11 个文件）的 sha256 与宿主产物**逐个相同**（`vm-sha256-2404.txt` / `vm-sha256-2204.txt`，
对照表 `cross-build-sha256.txt`；核对命令见 `commands.log`）。三个入口二进制带 `-trimpath` 且
`BUILD_DATE` 固定，可在宿主上逐字节重建；测试二进制不带 `-trimpath`，sha256 依赖宿主源码路径，
它的作用是证明“上传到 VM 的就是这里跑过的那一份”，不是跨机器的构建配方——这一点写进了
`cross-build-sha256.txt` 的注释，不藏在口头说明里。

核对过程中发现并纠正一处证据缺陷：22.04 上的 `tests/api.test` 是旧产物
（`8cf56ce6…`，不是宿主当时的 `d688923a…`），其余 10 个文件一致。已把 11 个文件重新同步到两台
VM 并重跑原生测试。P04.1 与 12 脚本回归**不依赖**这个测试二进制（回归脚本跑的是三个入口二进制，
那三个在同步前后字节相同），所以回归结论不受影响；但原生测试的结论以同步后这一轮为准。

### 3.1 P04.1 单脚本验收（72 项断言）

| VM | 命令 | 结果 | 证据 |
|---|---|---|---|
| 24.04 | `sudo BIN_DIR=/tmp/upanel-bin sh tests/integration/p04-1-system-acceptance.sh` | **通过 72 / 失败 0**，exit 0 | `p04-1-24.txt` |
| 22.04 | 同上 | **通过 72 / 失败 0**，exit 0 | `p04-1-22.txt` |

断言覆盖（逐段）：

1. **环境与身份**：root 运行、三个二进制可执行、`upanel-web` 账号存在；代理 uid=0。
2. **能力如实**：`supported_queries == ['system.summary']`，`supported_actions == []`。
3. **与 `/proc` 逐项比对**：内存总量按字节精确相等；`MemAvailable` 与两次读取同量级（±64 MiB，
   因为页缓存会让它真的变动）；`used = total - available`；逻辑核数 = `nproc`；`boot_id`、
   `os_id`、`kernel_release`、主机名、开机时长（相对 `/proc/uptime` 的合理区间）、负载 1 分钟值
   （相对 `/proc/loadavg`，容差 1.0）；五个段 `status=ok`。
4. **时间戳与轮询缓存**：`interval_ms=5000`；`collected_at` 是 10 秒内的快照；`stale=false`；
   `degraded=false`；睡 6 秒后 `collected_at` 前进 4—9 秒。
5. **计数器区间**：`ens4` 的 `rx_bytes` 落在“睡满一个间隔”前后两次 `/proc/net/dev` 读取之间，
   且第二份快照不小于第一份。
6. **使用率与速率**：第二份快照 `usage_percent` 非 null 且在 0—100；`sample_window_ms` 在
   4000—9000；按核使用率数量 = 逻辑核数；至少一块网卡给出非 null 的非负速率。
7. **没有万能入口**：`system.summary` 带 `params.command` → 400；带未知参数字段 → 400；
   `system.nonexistent` → 400“未登记”；`GET /internal/v1/queries` → 405。
8. **Web 透传**：未登录 401 `UNAUTHENTICATED`；admin 200 且数值与 `/proc` 一致、`collected_at`
   是 10 秒内的快照、`interval_ms` 与代理一致；新建 viewer 后 viewer 读同样 200。
9. **采集故障不阻塞登录**：停掉代理后 `socket` 不存在，登录仍 200；`/api/v1/system/summary`
   返回 503 `AGENT_UNAVAILABLE` 且**不返回任何数值**；代理重启后接口自动回到 200 且数值仍是真实值。
10. **日志卫生**：代理日志无 `sh -c` 痕迹、无 `BEGIN PRIVATE KEY`；Web 日志无 `panic`。

### 3.2 VM 内核上的 Go 测试二进制

| VM | 命令 | 结果 | 证据 |
|---|---|---|---|
| 24.04 | `sudo BIN_DIR=/tmp/upanel-bin sh scripts/run-native-tests.sh` | **TOTAL PASS=401 / FAIL=0**（agentstore 72 / agent 108 / agentclient 24 / operations 57 / syscmd 9 / store 11 / api 90 / monitoring 30），exit 0 | `native-24.txt` |
| 22.04 | 同上 | **TOTAL PASS=401 / FAIL=0**（同上分布），exit 0 | `native-22.txt` |

（同步后的最终一轮。此前一轮是 394，差在 `agent` 与 `api` 两包夹具用例补齐后的计数；
两台 VM 现在跑的是同一批 11 个二进制，见 `cross-build-sha256.txt`。）

解析器用例读的是**真实采集样本**（`tests/fixtures/*-2404.txt`），不是手写样例。夹具经
`UPANEL_FIXTURES`（脚本把夹具拷到 `BIN_DIR/fixtures`）或被直接执行测试二进制时按“二进制旁边的
fixtures”找到；找不到时用例**失败**并列出试过的路径，不跳过。

### 3.3 全量回归（12 个脚本）

| VM | 命令 | 结果 | 证据 |
|---|---|---|---|
| 24.04 | `sudo ... sh tests/integration/run-all.sh`（`LOG_DIR=/var/tmp/upanel-logs-p041c`） | **运行 12 / 失败 0** | `runall-24.txt`、`vm-logs-2404/` |
| 22.04 | 同上 | **运行 12 / 失败 0** | `runall-22.txt`、`vm-logs-2204/` |

逐脚本断言数（两台 VM 完全相同）：

| 脚本 | 24.04 | 22.04 |
|---|---|---|
| `p01-1-acceptance` | PASS 29 / FAIL 0 | PASS 29 / FAIL 0 |
| `p01-2-acceptance` | PASS 34 / FAIL 0 | PASS 34 / FAIL 0 |
| `p01-4-acceptance` | 通过 114 / 失败 0 | 通过 114 / 失败 0 |
| `p02-1-acceptance` | PASS 58 / FAIL 0 | PASS 58 / FAIL 0 |
| `p02-2-acceptance` | PASS 103 / FAIL 0 | PASS 103 / FAIL 0 |
| `p02-3-agent-acceptance` | PASS 87 / FAIL 0 | PASS 87 / FAIL 0 |
| `p02-4-agent-secrets-acceptance` | PASS 87 / FAIL 0 | PASS 87 / FAIL 0 |
| `p03-1-preflight-acceptance` | 通过 93 / 失败 0 | 通过 93 / 失败 0 |
| `p03-2-jobs-acceptance` | 通过 102 / 失败 0 | 通过 102 / 失败 0 |
| `p03-3-events-acceptance` | 通过 73 / 失败 0 | 通过 73 / 失败 0 |
| `p03-4-reconcile-acceptance` | 通过 78 / 失败 0 | 通过 78 / 失败 0 |
| `p04-1-system-acceptance` | 通过 72 / 失败 0 | 通过 72 / 失败 0 |
| **合计** | **930 / 0** | **930 / 0** |

`p02-3` 的断言在本轮**收紧而非放宽**：查询表从“必须为空”改成“必须完全等于
`['system.summary']`”，未登记样本换成 `system.nonexistent`，并新增两条（已登记查询 200 且带
`data`），断言数 85 → 87（DECISIONS Q86）。`run-all.sh` 的注册表也把新脚本排在最后
（`ROOT_SCRIPTS`），标题注释同步更新。

### 3.4 环境事实（不记作产品缺陷）

24.04 验收 VM 在本轮开始前第四次内核 panic（`Attempted to kill init! exitcode=0x8b`，
与 P02.4/P03.2 同一现象，四次全部发生在 24.04，22.04 一次没有）。重启后 `/tmp` 被清空、
`/etc/upanel/upanel.json` 再次被截断成 0 字节（`chown` 前解析直接报 EOF）。处理：删掉该文件，
重新上传三个二进制、8 个测试二进制、夹具与验收脚本，逐个核对 sha256 后重跑。**本文件所有 VM
结论都是重启之后重跑得到的**，不是拿重启前的日志凑数。

另外，第一次全量回归曾出现“脚本全过但退出码 2”：`run-all.sh` 早先把逐脚本日志写进
`/tmp/upanel-accept-<name>.log`，`/tmp` 的粘滞位让后一次运行写不进上一次以 root 建的同名文件。
该问题在 P03.4 已修（自建 `LOG_DIR`，默认 `/var/tmp/upanel-logs`，0777），本轮沿用
`LOG_DIR=/var/tmp/upanel-logs-p041c`，两次全量回归都是干净退出。

## 4. 本轮抓到并修掉的问题（都改在产品或验证手段本身，没有放宽断言）

| # | 现象 | 根因 | 修法 | 对应决策 |
|---|---|---|---|---|
| 1 | 计数器在 JSON 里是数字，违反 CONTRACTS §1 | Go 默认把 `uint64` 序列化成数字 | 全部字节/计数器字段加 `json:",string"`，并用 `types_test.go` 钉住线格式 | Q82 |
| 2 | 22.04 上三条时间戳断言判 False，24.04 全绿 | Python 3.10 的 `fromisoformat` 不认 9 位小数秒（Go RFC3339Nano） | 验收脚本内统一截到 6 位；产品时间格式不改 | Q87 |
| 3 | VM 上 `monitoring` / `agent` 夹具用例全部失败 | 测试二进制跑在 `/tmp`，`../../tests/fixtures` 在那里没有意义 | `internal/testsupport` 三层查找 + 运行器把夹具拷到 `BIN_DIR/fixtures`；**找不到就失败** | Q85 补充 |
| 4 | “快照之间只增不减”在 24.04 偶发失败 | 断言传参把两份快照的顺序弄反了（`jgetx` 的第一个参数才是 `d`） | 把第一份快照作为**附加参数**传入，方向固定 | 本条（验收脚本缺陷） |
| 5 | 代理停机后 Web 起不来（`attempt to write a readonly database`） | 验收脚本只把库文件 chown 给 Web 账号，目录还是 root 的，SQLite 建不了 `-wal` | 先把库目录整体 chown 给 Web 账号再启动 | 本条（验收脚本缺陷） |

## 5. 语义与边界（本轮明确写进代码与文档的部分）

- `status` 是**最近一次采集尝试**的结果；值自身带 `collected_at`。失败时保留上一次成功的值
  和它的旧时间，从没成功过则为 `null`（PLAN §8“最近有效值及其时间”）。
- `degraded`（有段不 ok）与 `stale`（快照超过 3 个采集间隔）是**两个不同的问题**，
  分别回答“有没有采错”和“这份数据有多新”。
- 速率只由相邻两次样本给出；没有前一次样本、计数器变小（重置/换设备/换启动）、时间窗非正时为
  `null`（不是 0、也不能是负数）。`boot_id` 变化会让 CPU 窗口失效。
- 采集**不执行任何外部命令**：没有 `exec`、没有 shell、没有可注入参数的接口；
  `system.summary` 的 `DecodeParams` 用 `DisallowUnknownFields` 拒绝任何字段。
- 采集在代理侧，Web 不做第二层缓存：`collected_at` 永远是采集时间，而不是“Web 转发的时间”。

## 6. NOT RUN（未执行，不计入通过）

| 项 | 为什么没做 | 怎么做 |
|---|---|---|
| 真实生产服务器上的采集 | 本项目没有生产服务器；验收只在两台可丢弃 VM 上做 | 部署后按 `docs/LOCAL_START.md` 起服务，观察 `system.summary` 的 `collected_at` 前进 |
| 长时间（小时级）采集的内存与 fd 稳定性 | 本轮只跑了几分钟的验收，没有做长跑观察 | 用 `systemctl show` 的 `MemoryCurrent` 与 `/proc/<pid>/fd` 计数做 24 小时观察 |
| 采集段失败的真实注入（例如 `/proc/meminfo` 不可读） | 只有单测用假 reader 覆盖，VM 上没有制造“某个 `/proc` 文件不可读”的场景（去 `chmod` 真 `/proc` 没有意义也不安全） | 在容器里用受限挂载点验证，或加一个测试专用的路径注入开关后再验 |
| 受限容器/无 `/proc` 环境 | 两台 VM 都是完整系统 | 在容器里跑代理冒烟，确认段状态变 `unavailable` 而不是崩 |
| 前端首页（P04.4） | 属后续子任务 | P04.4 |
| 磁盘拓扑、SMART/NVMe、diskstats 速率 | 属 P04.2/P04.3 | P04.2/P04.3 |
| 采集间隔可配置 | 契约写的是 5 秒（PLAN §8），本轮固定 5000 ms；`interval_ms` 字段已经在响应里，便于前端解释 | 如需要，后续在配置里开放，并在契约里写明范围 |

## 7. 结论

P04.1 的验收条件（CPU / 内存 / 负载 / 网络 / 系统信息采集 + 轮询缓存 + 时间戳）在代码、契约、
两台 Ubuntu VM 的黑盒验收与 VM 内核原生测试上都已完成；采集是纯文件读取，没有引入命令执行面，
也没有启用任何写动作。**P04 整体仍是 in_progress**：P04.2（块设备拓扑与系统盘保护）、
P04.3（SMART/NVMe 与 diskstats）、P04.4（前端首页）未开始。
