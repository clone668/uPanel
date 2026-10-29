# P13.2 验收结果：告警规则、持续时间、恢复阈值、空间双阈值、去重、冷却、维护静默

- 日期：2026-10-26（UTC）
- 分支 `main`，commit `8e52b36`（工作树未提交，按用户要求不提交不推送）
- 验收对象：**"什么时候该响、什么时候不该响"**——三态状态机（`ok` → `pending` →
  `firing` → `ok`）、"持续"必须是区间内每一个样本都在线外、恢复要跨过另一条线并同样
  持续、空间与 inode 的双口径（`any`/`all` 对偶）、一个事件周期只有一行、未变化不刷屏、
  恢复只发一次、恢复后的冷却期、维护静默只压通知不压判定，以及数据缺失时**原地不动**
- 验收方式：真实 Web（非 root）→ `/run/upanel/agent.sock` → root 代理 → 真实 Ubuntu 主机，
  受控种入的一条序列（不存在的网卡 `accept0`，每 5 秒一个 `rx_bytes_per_sec` 样本，值由
  `probe.phase` 决定）驱动时间线 + 真实容量数据驱动空间规则，SQLite 里逐行核对。两台专用
  Ubuntu VM **同一份脚本、同一组二进制、一台一跑不并发**。

## 1. 结论

| VM | 发行版 | 结果 | 退出码 | 断言 |
|---|---|---|---|---|
| 2404 | Ubuntu 24.04.5（内核 6.8.0-147，systemd 255，Python 3.12.3，sqlite 3.45.1） | **PASS** | 0 | 148 通过 / 0 失败 / 0 未执行 |
| 2204 | Ubuntu 22.04.5（内核 5.15.0-198，systemd 249，Python 3.10.12，sqlite 3.37.2） | **PASS** | 0 | 148 通过 / 0 失败 / 0 未执行 |

归一化后的 148 条断言名单跨机 `diff` 无输出（`logs/2404/results.txt` 与
`logs/2204/results.txt`）。逐节断言数：§0 环境 **7**、§1 装配 **9**、§2 规则校验/权限/
乐观并发 **48**、§3 生命周期+空间双阈值+维护静默 **63**、§4 删除规则 **9**、§5 审计 **8**、
§6 日志卫生 **4**。

宿主 `scripts/check.sh`：**PASS 14 / FAIL 0 / NOT RUN 0**，见第 6 节（`logs/host-check.txt`）。

**两台机器都不是一次跑绿，而且 24.04 跑了六轮。** 这不是同一件事重复六遍——每一轮红的
性质都不一样，四轮是产品缺陷或验收方法缺陷，一轮是环境病理，一轮是脚本自己崩。按顺序
记下来是因为"最终全绿"如果不带过程，等于把前面几轮的红都藏起来：

| 24.04 轮次 | 脚本 md5 | 结果 | 红在哪 | 归类 |
|---|---|---|---|---|
| 1 | `ad7d7941…` | 117 / 25，exit 1 | `取到可用的文件系统目标` 等 25 条连锁失败 | **环境病理**：`systemd-logind` 泄漏 7477 个 fd（Q415） |
| 2 | `0c2d6b5f…` | 139 / 3，exit 1 | 三处审计断言 | **脚本 bug**：SQL 列名写成 `details`，真名 `redacted_details`（Q416） |
| 3 | `c11e7f6b…` | **142 / 0**，exit 0 | — | 第一次全绿（当时的产品形状） |
| 4 | `feaa6506…` | 145 / 2，exit 1 | `清除后 starts_at 为空`、`pending 期间还没有任何事件` | **产品缺陷** Q418 修完之后，一条断言反过来红（它照着错的实现写），另一条是竞态（Q419） |
| 5 | `f19158b0…` | 崩在 §3.1，exit 2 | `arithmetic expression: expecting primary` | **脚本 bug**：`state_since` 是 Unix 秒整数，被拿去当 RFC3339 解（Q420） |
| 6 | `c74dd469…` | **148 / 0**，exit 0 | — | 当前结论 |

22.04 跑了两轮：`c11e7f6b…` 上 142/0（`logs/2204/acceptance.round1-142-0.log`），
`c74dd469…` 上 148/0（`logs/2204/acceptance.log`）。

**证据的边界**：第 1 轮（117/25）的完整日志被后续轮次覆盖了（同一个 `/tmp/p13-2-2404.log`
路径），只剩当时读进对话里的失败名单，逐字抄在第 3 节；第 2、3、4、5 轮的原始日志都在
`logs/2404/`。不把已经丢掉的日志假装还留着。

## 2. 交付指纹

宿主交叉编译（`CGO_ENABLED=0`，`-trimpath -ldflags '-s -w'`）：

```
GOOS=linux GOARCH=amd64 go build -o /tmp/upanel-linux/upanel-{agent,web,ctl} ./cmd/upanel-{agent,web,ctl}
```

| 产物 | md5 | sha256 |
|---|---|---|
| `upanel-web` | `082752b33bb2cd9293a2124c71b1e8d2` | `c67a5a89b2801fa8df345046e8129f56fdbf6d95e03f7afca2a044b2ef142c23` |
| `upanel-agent` | `1610950d6b8cce61fc6794e2b4f47c36` | `9504ee8e442054a9d5fb8b5e08700f0d9062878ce2b0359e92a169ed93e40830` |
| `upanelctl` | `e5e4ea6dbf3e2c892a6a13bc304cc682` | `c69631f4d16e3a8ade7c855d7e2b9d521d6ecc9e9a49f8df96207bdaa3b09c3b` |
| `tests/integration/p13-2-acceptance.sh` | `c74dd469e47a7c17aad05e94b628d930` | — |

两台 VM 上 `md5sum /tmp/upanel-bin/* /tmp/p13-2-acceptance.sh` 与上表逐字一致，见
`logs/2404/env.txt` 与 `logs/2204/env.txt`。`upanel-web` 的哈希在第 4 轮之后变过一次
（Q418 修的就是 web 侧 DTO），agent 与 ctl 没变——本轮没动它们的代码。

## 3. 这一阶段撞到并修掉的产品缺陷

### Q414：覆盖率容差与 `duration_seconds` 地板必须按每条序列自己的写入节奏算

首轮 24.04 真机暴露：主机指标每 5 秒一行，但容量序列（`used_percent`、
`inode_used_percent`、`available_bytes`）走 `storage.DefaultCapacityTTL = 30s` 的采集缓存，
**30 秒才有一个点**。评估器当时写死 `coverageSlack=10s`、`staleAfter=20s` 判断"窗口够不够、
数据新不新"，对容量永远不成立：规则建得成功、状态机每轮都跑、`last_reason` 却永远说
"窗口内没有任何样本"，而库里数据明明有。PLAN §18 要的空间/inode 告警会静默失效，且不会在
任何检查里报错。

修法：`metrics.WriteCadence(metric)` 成为"这条序列多久写一行"的唯一答案；新鲜度
`max(3×节奏, 20s)`（主机 20 秒、容量 90 秒），窗口前段余量 `max(2×节奏, 10s)`
（主机 10 秒、容量 60 秒）；`minDurationFor` 再加 `duration_seconds ≥ 2×节奏`
（主机 ≥30 秒、容量 ≥60 秒）。真机上由 §3 那条"容量指标 duration_seconds 低于两个采集
周期 → 400"钉住。

### Q417：`last_reason` 只在"进入 pending"那一轮写，第二轮被覆盖成空串

`decide` 只在**发生迁移**的分支填 `Reason`；`pending` 要连续几轮（30 秒持续 ÷ 15 秒评估），
后几轮落到"没迁移"的默认分支，`Reason` 留空写回库。于是**最需要解释的那一刻恰好什么都不说**，
看的人只能怀疑"它是不是根本没在看数据"——那正好也是 Q414 的症状，两者叠在一起会把排查带偏。
恢复侧同理（状态还是 firing，但已经在往回走）。修法：两个分支的每一轮都写回进度
（`已超限，正在计持续（还需 12s）` / `已回到恢复线内，正在计恢复持续（还需 8s）`）。回归：
`TestDecideKeepsReasonWhileCounting`，真机侧由 §3 的 `pending 的 last_reason 说明在计持续` 钉住。

### Q418：契约说"可空"，实现回的是空串

宿主 `openapi-lint` 关报 7 条 `Property 'nullable' is not expected here`——`nullable` 是
OpenAPI 3.0 的关键字，本文件是 3.1。改的过程中发现其中 4 处不是文档问题而是实现问题：
`formatAlertTime` 把零值时间格式化成 `""`，而 CONTRACTS §13.2 明写"没有窗口时起止为 `null`"、
事件字段"可空 `resolved_at`/可空 `acked_at`"。空串在客户端是另一件事：`new Date("")` 是非法
时间、`x == null` 把空串判成"有值"，于是"这条事件恢复了没有"只能靠 `state` 猜。

修法：`formatAlertTimeOrNil`（零值返回 nil），`ResolvedAt`/`AckedAt`/`StartsAt`/`EndsAt`
改成 `*string`；`openapi.yaml` 的 7 处 `nullable` 改成 3.1 写法（`type: [string, "null"]` /
`[number, "null"]`），`redocly lint` 从 7 error 变成 **有效，0 error / 6 warning**（6 条
warning 是这一轮之前就有的）。回归三处都按**原始 JSON** 判 null 而不是按 Go 侧指针：
`TestAlertSilenceRoundTrip`、新增 `TestAlertEventNullableTimesAreJSONNull`（firing 的行
`resolved_at`/`acked_at` 为 null、resolved 的行是字符串——只造一条的话"全返回 null"也能过）、
新增 `TestAlertRuleLastValueIsJSONNull`。把 helper 临时改回返回空串指针，前两个测试立刻红，
原文在 `logs/regression-before-fix.txt`：

```
--- FAIL: TestAlertSilenceRoundTrip (0.43s)
    alerts_test.go:335: 初始状态不该有静默窗口: {Active:false StartsAt:0x1c8b2da9ee50 ...}
--- FAIL: TestAlertEventNullableTimesAreJSONNull (0.39s)
    alerts_test.go:466: 未恢复事件的 resolved_at 应为 JSON null，实际 ""
```

真机侧加三条：没有窗口时静默接口的起止是 null（两处）、还在 firing 的事件
`resolved_at`/`acked_at` 是 null（新增 `ev_null` 助手，把 null / `<missing>` / 有值三者分开）。

## 4. 验收方法与环境的修正

### Q415：整机 fd 总数是环境前置条件（`systemd-logind` 泄漏 7477 个 fd）

首轮 117/25 的真因。代理每 5 秒扫一次存储拓扑，"设备是否被占用"那一步要把**每个进程的每个
fd** 取一遍设备号（`ProcBusyScanner.walkProc`）。24.04 上整机 7976 个 fd，其中
`systemd-logind` 独占 **7477**。实测对照：

```
storage.topology   24.85 s / 41.08 s      ← 重启 logind 之后 0.027 / 0.023 / 0.307 s
system.summary      0.068 s
lsblk               0.485 s
```

拓扑查询超时 → Web 记"读取存储拓扑失败：代理不可用" → 容量序列一个点也落不下来 →
`status.series` 里连 `filesystem` 这个 scope 都没有 → 所有跟文件系统有关的断言连锁失败。
`agent.log` 里同时有 47 条"代理请求耗时偏长"，代理吃满一个核——上一轮记下的
"agent 103% CPU"之谜同源。首轮失败名单（日志已覆盖，逐字摘）:

```
失败项: | 取到可用的文件系统目标 | 报错说明两个 scope 不在一处 | 报错点出 duration_seconds |
报错说清是"装不下两个点" | 建 R2（空间双口径 any，持续 60 秒）201 | R2 的 scope 由指标反查成
filesystem | 建 R3（空间双口径 all，持续 60 秒）201 | 超限但还没持续够 → pending（不是立刻触发）
| pending 的 last_reason 说明在计持续 | pending 期间还没有任何事件（持续没够就不报） |
R2（any）在真实容量数据上触发（Q414 回归位） | R3（all）在**同一份数据**上没有触发 |
R2 开了一条事件 | R2 的事件也被标成 silenced（静默不拦判定，只标记通知） | R3 一条事件都没有
| R3 在这段时间里确实被评估过 | 真实采集断掉时 R2 没有被判成恢复（容量的容差是 90 秒，
这一轮还不敢下结论） | 删除正在 firing 的 R2 200 | 删规则时把打开的事件收口成 resolved |
被删规则的事件仍留着快照（rule_name 还在） | 被删规则的事件 rule_id 置空（外键 SET NULL，
不是级联删除） | 被删规则的快照阈值还在 | 审计里记下了阈值 | 审计里记下了恢复线 |
PATCH 的审计带改动前后的值
```

（最后三条在环境修好之后仍然红，是另一个原因，见 Q416。）

处置：脚本 §0 自带 fd 体检，`> 4000` 打一行说明并 `exit 2`——**环境不满足时不产出假红**
（与 Q395 同一语义）；`取到可用的文件系统目标` 从"睡 12 秒读一次"改成最多 90 秒的退避轮询，
超时把采集器的 `last_error` 一起印出来。口径写进 `docs/TEST_ENVIRONMENT.md` §8.10。
这一轮实测两台 VM 的 fd 总数：24.04 **532**、22.04 **470**（含正在跑的服务；空载基线 30—50）。

### Q416：查询辅助函数不许吞掉 SQL 错误

第 2 轮三条审计红（`期望 1，实际 ''`）看着像产品没记审计详情。真因：`audit_events` 的列叫
`redacted_details`，脚本写成 `details`，SQLite 报 `no such column` 而 `sqlq` 把 stderr 扔进
`/dev/null`——**错误与"查出来是空"长得一模一样**。改正列名后直接在真库上查：`threshold`
6 行、`recovery_threshold` 6 行、`before` 1 行，三处都齐。`sqlq` 现在出错打印
`<sql-error: …>`。

### Q419：三处判据修正

① `清除后 starts_at 为空` 是照着 Q418 那个**错的实现**写的，产品修正后它反过来红——两个错
的东西互相印证比一个错更牢固，也更难发现。② `pending 期间还没有任何事件` 是竞态：状态在
`:14:16` 读到 pending，事件列表到 `:14:34` 才读，中间 18 秒里规则已经 firing；同时 `R1_T0`
取在"四条规则都建完"的 `:14:07`，而 pending 从 `:14:00` 就开始了，算出"用了 24 秒"，
比 `duration_seconds` 还短，看上去像产品提前触发。③ 改成 `pending_probe`——**同一个读事务**
里取 `state`/`state_since`/`last_reason`/事件数，轮询到 pending 的那个快照同时断言"还在
pending"与"0 条事件"；另加一条不受取样时刻影响的强判据：事件的 `started_at` 必须比
`state_since` 至少晚 `duration_seconds`。第 6 轮这条判据是 **30 秒**（24.04）/ **30 秒**
（22.04），一秒钟的便宜也没占。

### Q420：`$(( ))` 里塞命令替换是地雷

第 5 轮跑到 §3.1 就死了：`arithmetic expression: expecting primary: " 1792996240 -  "`，
退出码 2。前 82 条全绿、一条红都没有——**崩掉比红更难查**：红会告诉你哪条判据不成立，
崩溃只留一行 shell 的语法抱怨。根因是新加的强判据把 `state_since` 当 RFC3339 去解，而它在
`alert_rules` 里是 **Unix 秒整数**（`applyOutcome` 用 `unixOrZero` 写）；`iso_unix` 抛异常、
`2>/dev/null` 吞掉 stderr、`$()` 拿到空串。修法：`pending_probe` 直接按整数印；`iso_unix`
永不返回空串（空输入与解析失败都印 0，末尾再挂 `|| echo 0`）；`PROBE_SINCE` 过一遍纯数字
校验。**验收脚本不许因为一个取值问题整体崩掉**。

## 5. 断言覆盖了什么

- **§2 规则校验/权限/并发（48 条）**：viewer 只读、写要 admin + CSRF、未登录 401；缺
  `duration_seconds`、未知指标、低于下限、恢复线跑到不安全侧、次级指标跨 scope、三个
  `secondary_*` 只给一半、没有次级口径时 `combination` 归一成 `any`——每条都断言**报错
  指出的是缺的那个字段**，不是泛泛的 400；PATCH 的 `version` 语义（缺 400 / 旧 409 / 未知
  404）、`DELETE` 的 `version` 语义、`enabled=false` 生效、新建规则初始 `state=ok`；
  静默窗口的起止必须同进同出、终点早于起点 400、没有窗口时起止是 **null**（Q418）。
- **§3 生命周期/空间双阈值/维护静默（63 条）**：受控种子就位（SQL 数 raw 样本）；容量指标
  30 秒窗口被拒（Q414 回归位）；`pending` 不立刻触发且 `last_reason` 说清在计持续（Q417
  回归位）；持续够才 `firing` 且**只开一条**事件，事件从 `state_since` 起实打实等了 30 秒；
  再评估两轮事件数不涨（未变化不刷屏）；真实同一份容量数据上 `any` 触发、`all` 不触发；
  维护静默里事件 `silenced=true` 但照样记录、规则照样判定；清静默后新开的事件
  `silenced=false`、旧事件不被改写；**受控序列停写 40 秒**时 R1 状态、事件、`last_eval_at`
  都前进而 `last_reason` 说"没有样本"（丢失数据不当成 0）；**真代理停 40 秒**时容量规则
  没有被判成恢复（容差 90 秒）；回落后要恢复持续够才 `resolved`，事件原地收口、带出实际
  `duration_seconds` 与覆盖全区间的 `peak_value`、有 `resolved_at`，并进入冷却；冷却期内
  再次超限不重新起算、`last_reason` 说清在等冷却。
- **§4 删除正在报警的规则（9 条）**：事件被收口成 `resolved`、`rule_id` 变 0、快照字段还在、
  库里不剩任何 `firing` 的孤儿行。
- **§5 审计（8 条）**：`alert.rule.create/update/delete` 与 `alert.silence.update` 都落库，
  且**记下阈值、恢复线与 PATCH 前后的值**——只记改后值等于把一半事实丢了；被拒的请求也留痕。
- **§6 日志卫生（4 条）**：两个口令都不出现在 web 日志里、没有 panic、日志非空。

## 6. 宿主检查

```
PATH="/h/Code/.toolchains/node-v22.23.3-win-x64:$PATH" sh scripts/check.sh
```

**PASS 14 / FAIL 0 / NOT RUN 0**（`logs/host-check.txt`）。`go test ./...` 43 包 ok，其中
`internal/api` 实打实跑了 **193.1 秒**（338 个用例，每个建号用例都要做一次 Argon2id，
之前几轮它一直是 `cached` 所以从没露过这个耗时）；web **49 文件 939 用例**；`web-smoke`
10/0/0；`openapi-lint` **有效，0 error / 6 warnings**（6 条 warning 本轮之前就有）。

同一份代码在修 Q418 **之前**跑过一次是 **13/1**，唯一红的正是 `openapi-lint`：

```
error   struct: 7
warning no-unused-components: 3
warning operation-4xx-response: 2
warning operation-2xx-response: 1

❌ Validation failed with 7 errors and 6 warnings.
<== FAIL  openapi-lint (exit 1)
```

两次输出都留档（`logs/host-check-before-fix.txt` 与 `logs/host-check.txt`）——只留绿的那次，
等于把"这个缺陷是谁挡住的"从记录里抹掉。

## 7. NOT RUN（明确未执行，不算通过）

1. **通知渠道（P13.3）**：`alert_events.silenced` 已经落库，但**没有任何东西会把它发出去**。
   "维护静默只压通知"这句话目前在实现上是"只标记、不发送"，因为还没有发送方。
2. **告警桌面的前端（P13.4）**：规则列表、事件列表、静默设置都只有 HTTP 端点，前端页面、
   历史图表与 `POST /alerts/events/:id/ack`（确认动作，列已备好）都在 P13.4。
3. **`go test -race`**：宿主无 gcc，一直未执行，与 P12 各阶段一致。
4. **告警在真实故障下的表现**：本轮的超限由受控种入与真实容量阈值驱动，没有做"真的把磁盘
   写满"这类破坏性验收（也不该在共享验收机上做）。

## 8. 相关文件

- 实现：`internal/alerts/{alerts,evaluator,store}.go`、`internal/api/alerts.go`、
  `internal/metrics/cadence.go`（`WriteCadence`）、`migrations/0012_alerts.sql`
- 契约：`docs/CONTRACTS.md` §13.2、`api/openapi.yaml`（`AlertRule`/`AlertEvent`/`AlertSilence`）
- 决策：Q414（节奏）、Q415（fd 体检）、Q416（SQL 错误不许吞）、Q417（`last_reason`）、
  Q418（null 不是空串）、Q419（判据修正）、Q420（算术里的命令替换）
- 脚本：`tests/integration/p13-2-acceptance.sh`（md5 `c74dd469…`）
- 环境：`docs/TEST_ENVIRONMENT.md` §8.10（fd 前置条件）
