# P05.2 验收结果：进程列表、PID+starttime 身份、TERM/KILL 分开的流程、禁止终止关键进程

- 阶段：P05（服务、进程与日志）→ P05.2
- 验收日期：2026-10-21
- 验收环境：`upanel-2404`（Ubuntu 24.04.5，kernel 6.8.0-147-generic，systemd 255，2 vCPU，MemTotal 4009852 kB），
  详见 `env.txt`
- 结论：**passed**。代码、单元测试、契约、前端真实载荷回放、VM 真机验收、跨阶段回归均已完成；
  未验证项逐条列在下面，并注明原因，不当作通过。

## 1. 本轮范围

`docs/STAGES.md` §7：

> P05.2：进程列表、PID+starttime 身份、TERM/KILL 分开的流程；禁止终止关键进程。

§7 的验收条款里与本轮有关的四条：

| 条款 | 结论 | 证据位置 |
|---|---|---|
| 操作测试 service 的真实状态发生变化且有审计 | P05.1 已验；本轮回归重跑 **PASS=140 / FAIL=0 / NOT RUN=3** | `regression-p05-2.txt` §第一轮 |
| 不存在的 unit 不被当作命令片段 | P05.1 已验（同上回归） | 同上 |
| **不能停止代理/面板/SSH/核心服务** | 本轮验证：进程级保护 + 服务级保护双层；PID 1 / 内核线程 / 代理自身 / SSH 的预检全部 `allowed=false`，提交 409 且进程未受影响 | `p05-2-acceptance-2404.txt` §7 |
| **PID 重用后旧预检不能杀新进程** | 本轮验证：用 `ns_last_pid` 强制复用，旧预检提交 409 `DEVICE_IDENTITY_CHANGED`，**新进程毫发无伤** | `p05-2-acceptance-2404.txt` §9 |
| 结束日志订阅不会残留无限 journalctl 子进程 | **不属于 P05.2**，属 P05.3 | — |

契约：`docs/CONTRACTS.md` §10.2（新增）、`api/openapi.yaml`（`version: 0.0.0-p05.2`、`stage: P05.2`、
`GET /api/v1/processes`）。实施决策见 `docs/DECISIONS.md` Q112—Q117。

## 2. 交付物

| 文件 | 内容 |
|---|---|
| `internal/processes/`（新包，18 个文件，2631 行含测试） | `procfs.go`（逐字段读 `/proc/<pid>/{stat,status,cmdline,cgroup}`，不依赖 `ps`）、`collector.go`（5 秒快照、2000 上限、PID 升序、失败保留上一份）、`protection.go`（7 类固定保护表）、`sanitize.go`（命令摘要脱敏）、`signal.go`（预检结论 + 执行 + 读回验证）、`types.go`（DTO、闭集、指纹与状态版本）、`kill_unix.go`/`kill_other.go`（只保留 `kill(2)`，不拼 shell） |
| `internal/processes/testdata/`（30 个文件） | 真机 `/proc` 字节：PID 1、内核线程、sshd、sleep、带秘密参数的命令 × `stat`/`status`/`cmdline`/`cgroup`，另有 `passwd.txt` |
| `internal/agent/{processesquery,processaction}.go`、`registry.go`、`preflight.go`、`systemquery.go` | 查询 `processes.list`、生产动作 `process.signal`；登记条件按依赖是否为 nil（生产表在依赖齐备时才出现，验收表仍只有 `acceptance.*`） |
| `internal/api/processes.go`（新）、`router.go`、`server.go` | `GET /api/v1/processes`，**admin**；viewer 403 `FORBIDDEN` |
| `web/src/api/processes.ts`（278 行）、`processes.test.ts`（12 例） | 与 openapi 同源的 DTO、封闭集合中文标签、`signalNeedsPhrase`/`signalParams`/`sameProcess`/`filterProcesses`/`summarizeProcesses`——判决只读服务端给的 `capabilities` |
| `web/src/views/ProcessesView.vue`（596 行）、`components/ProcessSignalConfirm.vue`（217 行） | 列表（摘要 + 筛选 + 加载/空/错误/无权限）、确认卡（TERM 与 KILL 两套门槛、受保护/已被拒/过期四种形态、`不会自动升级` 文案） |
| `web/src/fixtures/p05-2-vm/`（22 份）、`components/realdata-processes.test.ts`（31 例） | VM 真实响应，与 `payloads/` 逐字节相同（`cmp` 核过）；SSR 回放 |
| `tests/integration/p05-2-processes-acceptance.sh`（995 行，12 节）、`p05-2-payload-validate.py`（578 行） | 真机验收脚本与载荷校验（字段、闭集、null 语义、与 `/proc` 对照） |
| `docs/CONTRACTS.md` §10.2、`api/openapi.yaml`、`docs/DECISIONS.md` Q112—Q117 | 身份定义、状态闭集、cpu/memory 的 null 语义、保护类别、阻塞项与告警项代码、TERM/KILL 门槛、不自动升级 |

刻意不做：按名字/正则/属主/前缀匹配目标，批量发送，列表接口上直接终止，任何“万能命令”入口，
任何自动 TERM→KILL 升级，受保护进程上的动作按钮。

## 3. 执行的检查

| 检查 | 结果 | 证据 |
|---|---|---|
| `go build ./...` / `go vet ./...` | 退出码 0 / 0 | `go-gates.txt` |
| `go test ./... -count=1` | **22 个包全部 ok**（`internal/processes` 0.438s、`internal/agent` 6.406s、`internal/api` 37.736s） | `go-test.txt`、`check.txt` |
| 交叉编译（linux/amd64）与 `go vet` | 退出码 0 / 0 | `check.txt` |
| `gofmt` | 无差异 | `check.txt` |
| 前端 `vue-tsc --noEmit` / `eslint .` | 0 error / 0 problem | `web-gates.txt` |
| 前端 `vitest run` | **295 passed（22 文件）**，其中进程真实载荷回放 31 例 | `web-gates.txt`、`check.txt` |
| 前端 `vite build` | 成功（522 ms） | `web-gates.txt` |
| 页面冒烟（`dist` 预览，未登录守卫） | **PASS 10 / FAIL 0 / SKIP 0** | `check.txt` |
| 契约 lint（redocly） | valid，9 warning（历史遗留） | `check.txt` |
| 宿主 `sh scripts/check.sh` | **PASS 13 / FAIL 0 / NOT RUN 0** | `check.txt` |
| **VM 真机验收脚本**（12 节） | **PASS=128 / FAIL=0 / NOT RUN=0** | `p05-2-acceptance-2404.txt` §12 |
| **VM 载荷校验** | **PASS=9250 / FAIL=0 / SKIP=1** | 同上，行 8941/9412 |
| **跨阶段定向回归**（p01-1、p02-3、p03-1、p03-2、p04-1、p04-2、p04-3、p05-1、p05-2） | 全部 0 失败 | `regression-p05-2.txt` |

回归明细（全部在 `upanel-2404` 上真机执行）：

| 脚本 | 结果 |
|---|---|
| p01-1（非 root） | PASS 30 / FAIL 0 |
| p02-3 | PASS 87 / FAIL 0 |
| p03-1 | 93 通过 / 0 失败 |
| p03-2 | 102 通过 / 0 失败 |
| p04-1 | 72 通过 / 0 失败 |
| p04-2 | 110 通过 / 0 失败 |
| p04-3 | 77 通过 / 0 失败 / 3 未执行（USB 盘、NVMe 真实健康、隔离到期自愈） |
| p05-1 | PASS=140 / FAIL=0 / NOT RUN=3 |
| p05-2 | PASS=128 / FAIL=0 / NOT RUN=0 |

## 4. VM 上的关键断言（原文摘录）

快照与采集（`p05-2-acceptance-2404.txt` §2—§4）：第一份快照 137 个进程（sleeping 66 + idle 71），
111 个受保护、26 个可操作；随后两份快照 138（多出僵尸 1）与 139（僵尸 1、running 1、可操作 27），
间隔都是 `interval_ms=5000`，`collected_at` 相隔 5000 ms；僵尸（pid 4936）与内核线程的
`memory_bytes` 为 `null`（内核线程没有 VmRSS）；`yes`（pid 5041）`cpu_percent=100`；
新出现的进程第一份快照里 `cpu_percent` 为 `null`（没有基线就是 null，不是 0）。

TERM（§5）：

```
PASS  TERM 不需要再认证
PASS  TERM 不需要确认短语
PASS  TERM 的资源锁是 process:<pid>:<starttime>
PASS  TERM 任务终态是 succeeded
PASS  TERM 之后进程真的消失（跨过面板自己说的那一层）
```

KILL（§6）：

```
PASS  没有再认证时进程没有被碰过（pid=5276 仍在）
PASS  短语不符时进程没有被碰过（pid=5276 仍在）
PASS  KILL 任务终态是 succeeded
PASS  同一份预检第二次提交被拒（409）
PASS  重放的拒绝码是 PREFLIGHT_STALE
```

保护（§7）：

```
PASS  PID 1 的 TERM 预检被拒
PASS  PID 1 仍然活着
PASS  被拒的 KILL 预检仍要求再认证
PASS  被拒的 KILL 预检没有被消费
PASS  代理进程仍然活着
PASS  内核线程的 TERM 预检被拒
PASS  SSH 的 KILL 预检被拒
PASS  SSH 进程仍然活着
```

PID 重用（§9，本轮核心条款）：

```
PASS  内核把同一个 PID 分给了新进程（pid=5463，starttime 从 30039 变成 30049）
PASS  旧预检的身份指纹不是新进程的
PASS  拿旧预检提交被拒绝（409）
PASS  拒绝码是 DEVICE_IDENTITY_CHANGED
PASS  被重用的 PID 上的新进程毫发无伤（这正是本子任务的核心验收点）
PASS  对新进程重新预检才是允许的
PASS  新身份的任务成功
```

权限与审计（§10）：viewer 读列表与预检都是 403；审计里能查到对应的 `operation.preflight` 与
`job.submit`（`job_id` 非空），且没有明文口令。

## 5. 本轮修掉的问题

**没有发现新的产品缺陷。** 这一轮暴露的问题全部在验收手段与跨阶段断言上：

1. **验收读数错位（8 条脚本断言 + 3 条载荷断言，第一轮）**：列表是 5 秒缓存快照，脚本起进程后
   立刻断言“它在列表里”、杀掉后立刻断言“它不在”，断言落在两次采集之间。改成
   `wait_in_snapshot`（每 0.5 秒取一次，直到目标出现且 `cpu_percent` 有基线）后消失。僵尸的父进程
   存活窗口从 8 秒提到 20 秒；“忙进程 CPU 100%”允许目标已被清理时记 **SKIP** 而不是 PASS。
   记录在 `docs/DECISIONS.md` Q117。
2. **两条断言本身写错了**（同一轮改对，不是放宽）：被拒的 KILL 预检**仍然**带再认证与短语
   （Q114，“拦住它的是 `allowed=false`，不是把门槛藏起来”）；viewer 读进程列表是 **403** 而不是 200
   （Q115）。
3. **五处跨阶段陈旧断言**（本轮回归发现）：P05.1/P05.2 登记了新的查询与动作，`p01-1`、`p02-3`、
   `p04-1`、`p04-2`、`p04-3` 里“写动作为空”“查询只有两项”的断言随之失效。按 Q86 的规则改成断言
   **完整闭集** `['processes.list','services.list','services.unit','storage.topology','system.summary']` /
   `['process.signal','service.control']`——少报是隐藏能力，多报是编造能力。没有删除、没有放宽。
4. **`p04-2` 的 `health.status 一律 unknown` 不成立**（P04.3 起分区/回环/逻辑设备如实报
   `unsupported`）。改成三条更严格的不变量：状态在闭集内、非物理盘必须 `unsupported`、物理盘没真的
   检查过（`checked_at=null`）必须 `unknown`。
5. **VM 上传布局（不是产品问题）**：`p02-3` 按 `DEPLOY_DIR="$ROOT/deploy"` 找模板，而 `deploy/` 当时被
   放在 `/tmp/deploy`；`agent.test` 缺 `tests/fixtures/`（30 个真机字节文件）。补齐后 45 条失败清零。

## 6. 状态分类

**已实现**：`processes.list` 查询与 `GET /api/v1/processes`、`process.signal` 生产动作（预检 → 任务 →
幂等 → 锁 → 现场复核 → `kill(2)` → 读回）、7 类保护、TERM/KILL 两套门槛、前端列表与确认卡、
契约 §10.2、openapi、验收脚本与载荷校验、真实载荷夹具与回放测试。

**已验证**（真机 Ubuntu 24.04 / kernel 6.8 / systemd 255）：上面第 3、4 节的全部内容——列表契约与
5 秒快照、与 `/proc`/`ps` 的对照、CPU 来自两次采样、TERM 与 KILL 各自的门槛与真实退出、
四类保护拒绝且进程未受影响、参数边界、**PID 重用后旧预检不能杀新进程**、viewer 403、审计链。

**未验证（NOT RUN）**：

| 项 | 原因 |
|---|---|
| 浏览器里的实际渲染 | VM 上没有浏览器。页面级证据在宿主用**同一批载荷**做 SSR 回放（`realdata-processes.test.ts`，31 例），夹具与本目录 `payloads/` 逐字节相同 |
| TERM 超时（进程忽略 SIGTERM） | 正常内核上造不出“被 TERM 且不退出”的可控进程；这条路径由 `internal/processes/signal_test.go` 的 fake 覆盖 |
| KILL 后仍存在（D 状态，不可中断睡眠） | 需要内核模块或坏盘才能稳定造出 D 状态，VM 上做不到；同样由 fake 覆盖 |
| 结束日志订阅不残留 journalctl 子进程 | 属 P05.3 范围 |
| p04-3 的三项 | USB 盘与 NVMe 在 VM 里不存在；`WAIT_QUARANTINE=0`（20 分钟隔离期不计入本轮回归），隔离到期自愈未重跑 |
| 多机/异构内核 | 本轮只有一台 VM，PID 重用、保护名单、竞态都在这台机器上验证 |

**受阻**：无。VM 在本轮期间发生过一次内核 panic（见 §7），重启后重传二进制与脚本重跑，最终结果
是在干净启动上取得的。

## 7. 环境事故（不记作产品缺陷）

`upanel-2404` 本轮之前共 7 次 `Kernel panic - not syncing: Attempted to kill init! exitcode=0x0000008b`
（`CPU: 1 PID: 1 Comm: systemd`，日志见 `env.txt` 的表与 `console.panic-*.log`）。已排除两个方向：
不是镜像共享（每次都是独立覆盖层），也不是宿主内存不足（宿主同期有 32.4 GB 可用）。本轮最终
验收在第七次 panic 之后的新启动上完成，作为第二轮的读数只有那一份被采用。

## 8. 复现

见 `commands.log`（编译与上传、两轮 VM 验收、证据回收与 `cmp` 校验、宿主闸门、前端回放、定向回归）。
一句话版本：

```sh
# 宿主
go build ./... && go vet ./... && go test ./... -count=1
cd web && npm run typecheck && npm run lint && npm test && npm run build && cd ..
sh scripts/check.sh

# VM（upanel-2404）
sudo env BIN_DIR=/tmp/upanel-bin sh /tmp/upanel-bin/tests/integration/p05-2-processes-acceptance.sh
```
