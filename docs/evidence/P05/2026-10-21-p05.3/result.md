# P05.3 验收结果：journal 结构化查询、筛选、游标、实时流、权限与资源回收

- 阶段：P05（服务、进程与日志）→ P05.3
- 验收日期：2026-10-21
- 验收环境：`upanel-2404`（Ubuntu 24.04.5，systemd/journald 255，2 vCPU），详见 `env.txt`
- 结论：**passed**。代码、单元测试、契约、前端真实载荷回放、VM 真机验收、跨阶段回归均已完成；
  未验证项逐条列在 §6，并注明原因，不当作通过。

## 1. 本轮范围

`docs/STAGES.md` §7：

> P05.3：journal 结构化查询、筛选、游标和实时流，日志权限、截断和资源回收。

§7 的验收条款里与本轮有关的那一条：

> **结束日志订阅不会残留无限 journalctl 子进程。**

| 条款 | 结论 | 证据位置 |
|---|---|---|
| **结束日志订阅不残留 journalctl 子进程** | 本轮验证：流在跑时 `journalctl` 子进程存在，客户端断开后 15 秒内归零；空闲流 60 秒后收到 `end{reason:"idle_timeout"}` 并同样归零 | `p05-3-acceptance-2404.txt` §13、§15 |
| 按 unit / 时间 / 级别 / 游标查询（PLAN §14） | 逐项验证，含跨页每个标记恰好出现一次 | 同上 §3、§4、§7 |
| 原始日志只有 admin 可读（PLAN §14） | 未登录 401、viewer 403（查询与流两条路径）、admin 200；能力表里没有日志相关动作 | 同上 §1、§10 |
| 秘密过滤是尽力措施 | `password=` / `token=` 被抹成 `[redacted]` 且 `redacted=true`，原文不在载荷里 | 同上 §8 |
| 条数 / 字节 / 时间范围上限 | 1000 条、1 MiB、24 小时三个上限都有拒绝用例；单页字节上限真的以 `truncated_reason=page_bytes` 生效 | 同上 §5、§6 |
| 流损坏/结束必须释放子进程与订阅资源 | 断开、并发第 5 条 429、空闲超时三条都验了子进程归零 | 同上 §13、§14、§15 |

契约：`docs/CONTRACTS.md` §10.3（新增）、§15（`/internal/v1/streams` 的 logs.stream 用途）、
`api/openapi.yaml`（`version: 0.0.0-p05.3`、`stage: P05.3`、`GET /api/v1/logs`、`GET /api/v1/logs/stream`）。
实施决策见 `docs/DECISIONS.md` Q118—Q134。

## 2. 交付物

| 文件 | 内容 |
|---|---|
| `internal/journal/`（新包，7 文件约 2000 行含测试） | `params.go` 参数规范化与全部边界（数据源白名单、unit 形状、级别闭集、游标字符集、1—1000 条、24 小时跨度、未来 skew）；`entry.go` journald JSON → DTO（`binary`/`truncated`/`redacted` 三种“不完整”各自显式）；`query.go` 键集翻页（降序、`next_cursor`、`page_bytes` 截断、窗口本地过滤）；`stream.go` 跟随流（`journalctl -f`、游标去重、十条元数据字段、`StreamEnd` 六种结束原因）；`syscmd_adapter.go` 只把“跑一个进程、收尾、可中断”接进 `internal/syscmd` |
| `internal/logs/fetch.go`（217 行） | Web 侧只做三件事：参数交给代理的 `logs.query`、把结果按契约校验成 `journal.Page`（缺 `cursor`/`timestamp`、空 `next_cursor` 都算代理违约）、把代理错误翻译成 503/400 |
| `internal/agent/{logsquery,streams}.go`、`registry.go` | 查询 `logs.query`；`/internal/v1/streams` 的单次许可（30 秒过期、只用一次、未登记用途 501）；NDJSON 流、空闲/时长/条数/字节四种自我收流 |
| `internal/agentclient/streams.go`（162 行） | 流客户端：独立的 `Transport`（`ResponseHeaderTimeout=0`，拨号器与普通请求相同）、`Last-Event-ID` 续传由调用方传入 |
| `internal/api/logs.go`（254 行）、`router.go` | `GET /api/v1/logs`、`GET /api/v1/logs/stream`，**admin**；查询串解析失败 400（未转义的 `;` 不会被静默丢掉） |
| `web/src/api/logs.ts`（517 行）、`logs.test.ts` | 与 openapi 同源的 DTO、级别/数据源闭集中文标签、限额常量、`parseLogStreamFrame`（帧类型来自事件名）、`openLogStream`（`Last-Event-ID` 续传交给 EventSource）、`summarizeLogPage`、`entryPlainText` |
| `web/src/views/LogsView.vue`（524 行）、`components/LogEntryRow.vue`（127 行） | 查询表单与窗口说明、翻页、加载/空/错误/无权限四态、实时流开关与结束原因；条目行把 `binary`/`truncated`/`redacted` 三种状态各画各的 |
| `web/src/fixtures/p05-3-vm/`（26 份）、`components/realdata-logs.test.ts`（24 例） | VM 真实响应与真实 SSE 帧，与 `payloads/`、`*.frames` 逐字节相同（`cmp` 核过）；SSR 回放 + 帧解析 |
| `tests/fixtures/journal/`、`internal/journal/journal_test.go` | 3 份真实 journalctl 字节（正序、逆序 3 条、warning 级），从 `internal/journal/testdata/` 迁到 `tests/fixtures/`（Q133） |
| `tests/integration/p05-3-logs-acceptance.sh`（871 行，17 节）、`p05-3-payload-validate.py`（463 行）、`p05-3-emit-markers.sh`（79 行） | 真机验收、载荷校验、可自证的日志写入器 |
| `docs/CONTRACTS.md` §10.3、`docs/DECISIONS.md` Q118—Q134 | 契约补登 + 本轮全部判断与缺陷记录 |

刻意不做：全文检索/正则搜索 journal、跨机日志、把日志导出成文件、任何日志写动作、
viewer 的“脱敏版日志”（过滤是尽力措施，不能当成可以开放的理由）。

## 3. 执行的检查

| 检查 | 结果 | 证据 |
|---|---|---|
| `go build ./...` / `go vet ./...` | 退出码 0 / 0 | `go-gates.txt` |
| `go test ./... -count=1` | **24 个包全部 ok**（`internal/journal` 40 例、`internal/logs` 22 例在内） | `go-test.txt` |
| 交叉编译（linux/amd64）与 `go vet` | 退出码 0 / 0 | `check.txt` |
| `gofmt -l .` | 无输出（本轮修掉 8 个未格式化文件） | `check.txt` |
| 前端 `vue-tsc --noEmit` / `eslint .` | 0 error / 0 problem | `web-gates.txt` |
| 前端 `vitest run` | **347 passed（25 文件）**，其中日志真实载荷与真实 SSE 帧回放 24 例 | `web-gates.txt` |
| 前端 `vite build` | 成功 | `web-gates.txt` |
| 页面冒烟（`dist` 预览，未登录守卫） | PASS 10 / FAIL 0 / SKIP 0 | `check.txt` |
| 契约 lint（redocly） | valid，9 warning（历史遗留，与上一阶段同量） | `check.txt` |
| 宿主 `sh scripts/check.sh` | **PASS 13 / FAIL 0 / NOT RUN 0** | `check.txt` |
| **VM 真机验收脚本**（17 节） | **PASS=132 / FAIL=0 / NOT RUN=1** | `p05-3-acceptance-2404.txt` §17 |
| VM 载荷校验（字段、闭集、null 语义、与 journalctl 对照） | **PASS=6466 / FAIL=0 / SKIP=0** | 同文件 §16 |
| VM 原生测试（Linux 上跑同一份 Go 代码） | 见 §4 | `native-tests.txt` |
| **跨阶段回归**（17 个验收脚本） | **1520 项断言；15 个脚本一次通过，另 2 个因 0 字节脏配置失败、清掉现场后 run B 全绿（148/0）** | `regression-p05-3.txt`、`vm-logs/` |

VM 上的关键断言：监听 19451 的是 `upanel-web`（uid 999）、代理是 root、`agent.sock` 为
`root:upanel-web` 且非 0666；30 条标记写入同一个单元后，按 unit + 窗口查询返回 36 行且每个标记
恰好一次；`level=error` 只回 error 及更严重（没有一条 `marker-i=`），`level=info` 同时含 error 行；
`level=unknown`、`unit=ssh*`、`unit=../etc/passwd`、`source=syslog`、`limit=0`、`limit=1001`、
`limit=abc`、`from>to`、跨度 30 小时、未来的 `to`、手写日期、被 journalctl 拒绝的游标、未转义的游标
全部 400；`limit=5` 的页给出 5 条、`truncated=true`、`next_cursor` 正好是本页最后一条，翻完 8 页
每个标记跨页只出现一次、每页游标互不重复、最后一页 `next_cursor=null`；单页字节上限在 303 条里
截到 261 条、1046028 字节（< 1 MiB），`truncated_reason=page_bytes`；`password=hunter2secret` 变成
`password=[redacted]` 且 `redacted=true`，原文不在载荷里；内核日志（`-k`）能按 `source=kernel` 查出注入的
kmsg 行；流的第一帧之后 `id:` 就是条目游标、事件名是 `entry`，`Last-Event-ID` 续传从下一行开始
（不重复投递），空闲 60 秒收到 `end{reason:"idle_timeout"}`，同一账号第 5 条流 429 且前 4 条不受影响，
客户端断开后 15 秒内 `journalctl` 子进程归零。

## 4. 回归与原生测试

### 4.1 VM 原生测试（同一份 Go 代码在目标平台跑）

```
sudo BIN_DIR=/tmp/upanel-bin sh /tmp/upanel-bin/run-native-tests.sh
```

| 包 | rc | PASS | FAIL |
|---|---|---|---|
| agentstore | 0 | 72 | 0 |
| agent | 0 | 171 | 0 |
| agentclient | 0 | 39 | 0 |
| operations | 0 | 57 | 0 |
| syscmd | 0 | 13 | 0 |
| store | 0 | 11 | 0 |
| api | 0 | 135 | 0 |
| monitoring | 0 | 30 | 0 |
| storage | 0 | 159 | 0 |
| **journal** | 0 | **40** | 0 |
| **logs** | 0 | **22** | 0 |
| **合计** | — | **749** | **0** |

`journal` 与 `logs` 的用例全是合成输入（fake runner、假代理），不需要 journald；
在 Linux 上跑通只证明**同一份代码在目标平台行为一致**，不等于真机验收——那在 §3。
证据：`native-tests.txt`（本文件即 VM 上的原始输出）。

### 4.2 跨阶段全量回归（17 个验收脚本）

```
sudo BIN_DIR=/tmp/upanel-bin WAIT_QUARANTINE=0 sh /tmp/upanel-bin/tests/integration/run-all.sh
```

`WAIT_QUARANTINE=0`：p04-3 的“隔离到期后容量自愈”要等 5 分钟隔离期，本机内核有随机 panic
病史（9 次，uptime 8—70 分钟不定，见 `env.txt`），把它如实记为 NOT RUN，其余断言照跑；
那一项由 `docs/evidence/P04/2026-10-21-p04.3/` 自己的验收覆盖。

| 脚本 | 身份 | run A | run B（同机重跑） |
|---|---|---|---|
| p01-1-acceptance | 非 root | PASS 31 / FAIL 0 | — |
| p01-2-acceptance | 非 root | **PASS 32 / FAIL 2** | **PASS 34 / FAIL 0** |
| p01-4-acceptance | 非 root | **通过 113 / 失败 1 / 未执行 0** | **通过 114 / 失败 0 / 未执行 0** |
| p02-1-acceptance | 非 root | PASS=58 FAIL=0 | — |
| p02-2-acceptance | 非 root | PASS=103 FAIL=0 | — |
| p02-3-agent-acceptance | root | PASS 88 / FAIL 0 | — |
| p02-4-agent-secrets-acceptance | root | PASS=87 FAIL=0 | — |
| p03-1-preflight-acceptance | root | 通过 93 / 失败 0 | — |
| p03-2-jobs-acceptance | root | 通过 102 / 失败 0 | — |
| p03-3-events-acceptance | root | 通过 73 / 失败 0 | — |
| p03-4-reconcile-acceptance | root | 通过 78 / 失败 0 | — |
| p04-1-system-acceptance | root | 通过 72 / 失败 0 | — |
| p04-2-storage-acceptance | root | 通过 110 / 失败 0 / 未执行 0 | — |
| p04-3-storage-health-acceptance | root | 通过 77 / 失败 0 / **未执行 3** | — |
| p05-1-services-acceptance | root | PASS=140 FAIL=0 **NOT RUN=3** | — |
| p05-2-processes-acceptance | root | PASS=128 FAIL=0 NOT RUN=0 | — |
| p05-3-logs-acceptance | root | PASS=132 FAIL=0 **NOT RUN=1** | — |
| **合计** | — | **1520 项断言，2 个脚本失败** | **那 2 个脚本 148 项断言，0 失败** |

run A：`运行脚本数 17 / 失败脚本数 2`，退出码 1，耗时 04:57:45Z→05:04:10Z（约 6.5 分钟），
本次 boot 未发生 panic。

### 4.3 run A 那两条失败：0 字节的默认配置，不是产品缺陷

三处失败（p01-2 两条、p01-4 一条）的实际输出是同一条：

```
配置错误: 解析配置文件失败 /etc/upanel/upanel.json: EOF
```

成因链，每一环都有据可查：

1. 这三条断言测的是**默认配置路径**行为——`upanelctl migrate --db <path>` 与 mock 段都不传
   `--config`，于是读 `/etc/upanel/upanel.json`。文件不存在时走内置默认值（正常）。
2. VM 上那份文件在 run A 开跑时是 **0 字节**。所有 root 脚本都用 `cat > "$SYS_CFG" <<JSON`
   （先截断再写），任何一次中断都可能留下空文件；而 `/etc` 不像 `/tmp` 那样被清空，于是它
   活过了这台机器 9 次内核 panic（`/tmp/upanel-bin` 这次就被清空了，harness 只好重传）。
3. run A 里 p01-2、p01-4 之前没有任何脚本会写这个路径，所以它们读到的就是那份残留；
   p01-4 自己写系统配置的代码在**它后面**的 systemd 段，救不了前面的 mock 断言。
4. 后续脚本（p02-3—p05-3）各自把它重写成合法配置，所以 run A 结束时它已是 314 字节的
   合法 JSON——run B 直接重跑这两个脚本就全绿，**没有改一行产品代码**。

产品行为不改：0 字节配置报“解析配置文件失败”是对的，静默用默认值会把一次失败的安装
伪装成正常运行（DECISIONS Q135）。改的是验收前置检查，`run-all.sh` 开跑时若发现该路径
存在但为 0 字节就明确警告，不替人删现场：

```
警告: /etc/upanel/upanel.json 存在但为 0 字节（多半是上一次安装或被中断的残留）。
      默认配置路径的断言会报“解析配置文件失败 /etc/upanel/upanel.json: EOF”；
      确认是脏现场后再删除它，别把它记成产品缺陷。
```

守卫自检：把该文件临时截成 0 字节、用不存在的脚本名调 `run-all.sh`（不跑任何验收），
上面三行如期出现，随后把 314 字节的原文件恢复回去。自检与两轮回归的完整 stdout 都在
`regression-p05-3.txt`；逐脚本日志在 `vm-logs/runA/`（17 份）与 `vm-logs/runB/`（2 份）。


## 5. 本轮发现并修掉的问题

本轮的缺陷几乎全是**真机**才暴露的：单测与手写样本都对得上，一进真实 journald 就出问题。
按发现顺序，每条都写了修法与钉住它的断言。

1. **`--cursor` 与 `--since/--until` 不能同时给**（Q126）。真实 journalctl 直接退出 1
   （`Please specify only one of --since=, --cursor=, and --after-cursor=.`），于是带窗口的翻页
   每页都 503，前端表现为“每页都是第一页”。修法：带游标时不拼时间参数，窗口在 Go 侧过滤；
   `next_cursor` 在“整页都被窗口过滤掉”时指向本页扫到的最老游标，否则翻页会丢条或原地打转。
   钉住：`TestQueryCursorAppliesWindowLocally`、`TestQueryCursorAdvancesWhenPageFallsOutsideWindow`。
2. **`journalctl --kernel` 不存在**（Q127）。只有 `-k`，内核日志查询与内核日志流都 500/503。
   钉住：`TestQueryKernelArgs` 断言 argv 里有 `-k`、没有 `--kernel`。
3. **未转义的 `;` 让游标被静默丢掉**（Q128）。Go 1.17+ 的 `url.ParseQuery` 遇到裸 `;` 会丢弃
   整对参数并返回错误，而 `c.Query` 只看解析结果——服务端于是收到“没给游标”，乖乖回了第一页。
   修法：查询串解析失败 400 并说清楚要 percent-encode。钉住：`TestLogsRejectsBadParams`
   与真机载荷 `err-cursor-raw.json`。
4. **执行阶段的参数错误被报成 503**（Q129）。`to` 在未来、游标被 journalctl 拒绝只有 Run 阶段才知道，
   原来统一走 500 → Web 翻成 `AGENT_UNAVAILABLE`，用户看到“代理不可用”而真正要改的是参数。
   修法：映射成 400 `VALIDATION_ERROR`。钉住：`TestLogsQueryParamErrorsFromQueryAre400`
   与 `TestLogsQueryInternalErrorsStay500`（两个方向都钉，避免一刀切）。
5. **`limit=0` 被当成“没给”**（Q130）。契约是 1—1000。修法：Web 解析层直接拒绝 `n < 1`；
   钉住四条（`0`/`-1`/`1001`/`abc`，真机与单测各一份）。
6. **空闲流活不到空闲超时**（Q131）。代理 `WriteHeader(200)` 后没有 flush，Web 的流客户端又和普通请求
   共用一个带 10 秒 `ResponseHeaderTimeout` 的 `Transport`，安静系统上第一条日志要等几十秒，
   客户端先把一条完全正常的流掐死（`timeout awaiting response headers`）。修法：开流立即 flush +
   流客户端用零 `ResponseHeaderTimeout` 的克隆 `Transport`（拨号器相同）。钉住：
   `TestStreamLogsFlushesHeadersBeforeFirstEntry`、`TestStreamClientHasNoOverallTimeout`。
7. **参数错误的消息套了三层前缀**（Q134）。`journal: 查询参数不合法: agent: 参数不合法: journal: …`
   ——每一跳都用 `%w: %v` 又写了一遍自己的说法，把一句“改参数”说成代理层故障。修法：两处换成
   “只标记、不改写消息”的包装类型。钉住：`TestLogsQueryParamErrorsFromQueryAre400`、
   `TestQueryMapsAgentRejection`（逐字比对代理原文）、`realdata-logs.test.ts`（真机载荷）。
8. **验收脚本自己的三处错**（Q132、Q133，不是产品缺陷，但不修就会把“脚本错”记成“功能错”）：
   ① 逐条 `logger` 写入会丢 `_SYSTEMD_UNIT` 归属（30 条里 3 条查不到），改成常驻 `systemd-cat`
   一次写多行 + 回读自证 + 只补写缺失项；② systemd 自己写的 `Starting/Finished` 行 `_SYSTEMD_UNIT`
   是 `init.scope`，断言相应放宽为“允许 init.scope 且正文提到该单元”，并在 CONTRACTS §10.3 写明
   服务端如实回传、不替 journald 猜归属；③ `internal/journal` 的夹具原本用 `testdata/` 相对路径，
   测试二进制在 `/tmp` 下跑就找不到，迁到 `tests/fixtures/journal/` 并走 `testsupport.Fixture`。
9. **`gofmt` 有 8 个文件未格式化**（`check.sh` 抓到）：`cmd/upanel-{agent,web}/main.go`、
   `internal/agent/{server,streams,streams_test}.go`、`internal/api/jobstream.go`、
   `internal/journal/journal_test.go`、`internal/syscmd/syscmd_test.go`。纯空白/对齐问题，已 `gofmt -w`，
   `check.sh` 由 FAIL 1 转为 PASS 13 / FAIL 0。
10. **契约状态表漏登两个已实现路径**：`api/openapi.yaml` 的 `version` 已是 `0.0.0-p05.3`，但
    `x-upanel-contract-status.stage` 还停在 `P05.2`，`implemented_paths` 里也没有
    `get /api/v1/logs` 与 `get /api/v1/logs/stream`。已补齐——能力清单写错比功能写错更难发现。

## 6. NOT RUN（未验证，含原因）

- **单条消息 8 KiB 截断（`truncated=true`）**：真实 journald 给不出来。实测 systemd 255 对超过
  4086 字节的 `MESSAGE` 直接返回 null（字节数组），我们的代码把它标成 `binary=true`、`message=""`——
  这条在真机上有断言；而“8 KiB 以上的字符串被截断并标 truncated”只能由 `internal/journal`
  的合成输入覆盖（`TestQueryTruncatesLongMessage`）。不把合成输入的通过写成真机通过。
- **浏览器渲染**：VM 上没有浏览器。页面级证据在宿主用**同一批载荷与真实 SSE 帧**做 SSR 回放
  （`realdata-logs.test.ts`，24 例），夹具与证据目录逐字节相同。
- **journal 目录不可写 / 被删**：需要故意破坏系统日志，不属于本阶段验收范围。
- **多机/异构 systemd**：本轮只有一台 24.04（systemd 255）。22.04（systemd 249）的 `journalctl`
  参数差异没有在本轮验证。
