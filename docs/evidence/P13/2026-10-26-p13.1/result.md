# P13.1 验收结果：历史监控的采集、三层聚合、保留清理与查询点数上限

- 日期：2026-10-26（UTC）
- 分支 `main`，commit `8e52b36`（工作树未提交，按用户要求不提交不推送）
- 验收对象：**历史到底有没有被如实记下来、能不能被如实读出来**——10 个登记指标按代理的
  5 秒节奏落到 `metric_samples`、同一份快照不写两次、分钟/小时两层聚合在桶结束后出现、
  超出保留窗口的样本被裁掉、缺口不补零、查询按跨度选层且点数不超过 1440
- 验收方式：真实 Web（非 root）→ `/run/upanel/agent.sock` → root 代理 → 真实 Ubuntu 主机
  的 CPU/内存/网络/文件系统事实，SQLite 里逐行核对。两台专用 Ubuntu VM **同一份脚本、
  同一组二进制、不并发**。

## 1. 结论

| VM | 发行版 | 结果 | 退出码 | 断言 |
|---|---|---|---|---|
| 2204 | Ubuntu 22.04.5（内核 5.15.0-198，systemd 249，Python 3.10.12，sqlite 3.37.2） | **PASS** | 0 | 88 通过 / 0 失败 / 0 未执行 |
| 2404 | Ubuntu 24.04.5（内核 6.8.0-147，systemd 255，Python 3.12.3，sqlite 3.45.1） | **PASS** | 0 | 88 通过 / 0 失败 / 0 未执行 |

归一化后的 88 条断言名单跨机 `diff` 无输出（`logs/2204/results.txt` 与
`logs/2404/results.txt`）。宿主 `scripts/check.sh` **PASS 14 / FAIL 0 / NOT RUN 0**
（`logs/host-check.txt`），按 `.nvmrc` 使用 `H:\Code\.toolchains\node-v22.23.3-win-x64`；
同一份代码先用系统 node v24.15.0 跑过一次是 13/1，红在工具链层（`node-version` 期望
v22.23.3、实际 v24.15.0），换回锁定版本后全绿。

两台机器都不是一次跑绿。红过的几轮按"夹具 → 判据 → 产品"逐层剥开：先是 CRLF 让脚本在
VM 上 `set: Illegal option -` 与 `gofmt` 关 FAIL，再是 `scp` 推上去的二进制没有执行位，
再是按粒度而不是按 scope 读 series、恒真的 socket 断言、缺口判据窗口的右端没让桶宽，
最后一层才是产品缺陷（加权平均的浮点误差撞 CHECK）。这一点写清楚，是因为"最终全绿"如果
不带过程，等于把前几轮的红藏起来。

**证据的边界**：第 4 轮（24.04 上三项红那次）的 `web.log` 与 `acceptance.log` 没有随证据
保留——复跑时同一路径被覆盖了。那一轮的关键原话逐字引用在第 3 节，另外把"修之前"的状态
固化成了可重跑的回归测试（`logs/regression-before-fix.txt`），任何人可以从当前源码重放。
不把已经丢掉的日志假装还留着。

## 2. 交付指纹

构建入口唯一：`scripts/build-linux.sh`。

```
VERSION=0.0.0-dev COMMIT=8e52b36 BUILD_DATE=2026-10-25T21:04:14Z sh scripts/build-linux.sh
```

| 产物 | sha256 |
|---|---|
| `upanel-web` | `c99f56c38f4b203fbcdb025145196da9fd111f47cc4d53c6b470e7e4087ad017` |
| `upanel-agent` | `b469732d864abd28dc677025de9bf51d30555f3b1d57af30bc1b4354ec44d644` |
| `upanelctl` | `5d4a1bbfb08a676cba773f6275b396ea34768031c497d6ed77695cf490bfc1f6` |
| `tests/integration/p13-1-acceptance.sh` | md5 `d08964c5385a5156308386001c2190bf` |

两台 VM 上 `sha256sum /tmp/upanel-bin/*` 与 `md5sum /tmp/p13-1-acceptance.sh` 的结果
逐字一致，见 `logs/2204/env.txt` 与 `logs/2404/env.txt`。

`upanel-web` 的哈希在修复 Q404 之后变了（修复前 `4f67b8cd…`），agent/ctl 的哈希没变
（本轮没改它们的代码），这是预期内的：换行符与代码都属于构建输入（Q406）。

## 3. 这一阶段撞到并修掉的产品缺陷

### Q404：加权平均的浮点误差让整个小时桶永久写不进去

**现象。** 24.04 第 4 轮，`== 4. C：保留与启动补算` 三项红：`samples.hour` 为 0；
种进上一个整小时的那 60 个分钟桶既没被聚成 hour 行，也查不到。

**现场。** 24.04 第 4 轮的 `web.log`（该轮日志未随证据保留，原因见第 1 节末）里，代理侧
的那行原话是：

```
stage=aggregate-hour
store: 聚合 hour 桶 1792958400 失败: constraint failed:
CHECK constraint failed: min_value <= value AND value <= max_value (275)
```

之后每分钟重试一次，每次都一样。

**根因。** 把那个桶的分钟行按 `AggregateMetricBucket` 里同一句 GROUP BY 在库里摊开，
18 组里有 2 组越界：

| scope | target_id | metric | 加权平均 | min_value | max_value | 样本数 |
|---|---|---|---|---|---|---|
| filesystem | `/` | `inode_used_percent` | `1.8999999999999997` | `1.9` | `1.9` | 3 |
| filesystem | `/boot` | `used_percent` | `14.199999999999998` | `14.2` | `14.2` | 3 |

三个样本的值全等于 1.9，SQL 算的是 `SUM(1.9*12)/SUM(12)`，双精度下不等于 1.9，比
`MIN(min_value)` 小一个 ulp。表上的 `CHECK (min_value <= value AND value <= max_value)`
因此拒绝了整条 INSERT。

**后果不是"少一个点"。** INSERT 是整桶一条语句，任何一组越界就是整桶失败——那一小时
**所有**指标（CPU、负载、内存、网卡、三个挂载点）的 hour 行都没写成，而自愈机制每轮
重试都失败、失败原因只在日志里出现一次（`logInterval` 一分钟一条），缺口永久存在。

**修法。** 聚合与查询两处都把加权平均夹回 `[MIN(min_value), MAX(max_value)]`：

```sql
min(max(SUM(value * sample_count) / SUM(sample_count), MIN(min_value)), MAX(max_value))
```

一组数的加权平均在数学上必然落在它们的最小值与最大值之间，落到区间外的只可能是浮点
误差，所以夹回去是还原事实，**不是放宽不变量**——`CHECK` 原样保留。查询那处
（`QueryMetricSeries` 按步长再卷一次）必须一起夹：桶宽等于步长时那条 GROUP BY 只合并
一行，误差就出在这一行里，不夹的话越界值会原样返回给页面。

**判据。** 回归测试 `TestAggregateMetricBucketEqualValuesStayInRange` 用 1.9 与 14.2
复现。去掉夹取时它报出的错误与真机一字不差：

```
--- FAIL: TestAggregateMetricBucketEqualValuesStayInRange (0.03s)
    metricstore_test.go:200: 值相同时也必须聚合成功（浮点误差不能撞 CHECK）:
    store: 聚合 hour 桶 1792918800 失败: constraint failed:
    CHECK constraint failed: min_value <= value AND value <= max_value (275)
```

加回夹取即通过。这份"修之前"的输出留在 `logs/regression-before-fix.txt`，是真机红现场
被固化成可重跑的东西。

**复验。** 修后两台 VM 各 88/0；`启动补算出了小时行（samples.hour > 0）`在内 24.04 由
红转绿。

## 4. 验收方法修正

前三条是"看着在验、其实什么都没证"，第四条是判据窗口边界算错。四条都不放宽任何东西。

1. **Q405 缺口判据窗口的右端也要让一个桶宽。** 24.04 第 3 轮
   `代理停机期间库里也没有行` 期望 0 实际 4，四行是 `load1/load5/load15/memory_used_percent`，
   `ts=1792961060`，而 `GAP_START=1792961048`——看上去是"代理死后第 12 秒仍在写"，我一度
   按这个方向去查产品。翻 `agent.log` 才看清：`"系统采集恢复","collected_at":"…20:44:24.847"`，
   代理是 20:44:24.847 才恢复采集的；`InsertMetricSamples` 里 `alignUnix(s.TS, width)` 把
   时间戳向下对齐到 5 秒桶，20:44:24.847 落桶成 20:44:20，比 `GAP_END`（20:44:24）还小，
   于是重启后的第一份快照被算进了"停机窗口"。那四行的形状也是证据：没有
   `cpu_usage_percent`、没有网卡速率——差分指标在进程重启后的第一份快照里必然缺失。
   脚本原本只在左端收了一个桶宽（注释写着"两端各收一个桶宽"，代码只做了一半）。修法：
   新增 `GAP_IN_TO=$((GAP_END - 5))`，判据点与 SQL 计数都用它；停机时长从 `sleep 15`
   加到 `sleep 25`，让两端各收 5 秒之后判定区间还有 16 秒（三个桶）。这是**加强**：
   原来名义上判 10 秒、实际判到重启边界，现在明确判 16 秒。
   为此专门写了一个探针（起 agent + web、`kill -TERM` 代理后逐 5 秒打印 `MAX(ts)`）在
   24.04 上跑对照：`MAX(ts)` 在代理死后停在停机前 8 秒，44 秒内一格没前进，证明产品没有
   "死后还写"。探针用完即删。
2. **Q407 恒真的 socket 断言。** `stop_agent` 自己 `rm -f "$SOCK"`，所以"停机后 socket
   不存在"永远成立、什么都证明不了。改成拿住 PID 后 `kill -0` 判"进程已退出"。
3. **Q407 按粒度而不是按 scope 读 series。** 断言期望 `status.series` 里有 `raw` 这一组，
   实现是按 **scope**（`host`/`interface`/`filesystem`）分组。产品是对的，错的是断言；
   改成按 scope 判，并补一条"每条都带 scope/target_id/metric"。
4. **Q407 基准取样点取早了。** 关闭采集后"最新 raw 时间戳没有前进"的基准 `BEFORE_MAX`
   原本在 `stop_web` **之前**取：采集周期 2 秒，取完到进程真正退出之间还可能落进一份新
   快照，MAX 合法地前进一格，断言就成了偶发失败。移到 `stop_web` 之后取。

另有两处是**工程卫生**（Q406），不是产品也不是判据：编辑工具在 Windows 上把 5 个文件写成
CRLF，导致 `gofmt` 关 FAIL（`check.sh` 因此新增第 14 关 `line-endings`，用字节数比对而不是
`grep '\r'`——Git Bash 的 grep 把 CRLF 当行尾，查不出来）；`scp` 推上去的二进制没有执行位，
三个都 `Permission denied`，补 `chmod 0755`。

## 5. 逐节断言数

| 节 | 内容 | 断言数 |
|---|---|---|
| 0 | 环境（工具、二进制、脚本指纹、UTC、权限） | 6 |
| 1 | 装配：配置（历史监控打开，保留窗口压缩）+ 代理 + 管理员 + Web | 5 |
| 2 | A：真实采集（5 秒节奏、不重复写、分钟聚合出现） | 22 |
| 3 | B：缺口（停代理期间不写样本、不补零） | 9 |
| 4 | C：保留与启动补算（种入历史行，重启后先聚合再裁剪） | 10 |
| 5 | D：查询规则（跨度选层、点数上限、参数校验、权限） | 23 |
| 6 | E：关闭采集（旧数据仍可查，status 说清原因） | 8 |
| 7 | F：日志卫生（口令、私钥、panic） | 5 |
| | **合计** | **88** |

两台机器的逐节数与断言名单逐字一致。

## 6. 未执行（NOT RUN）

1. **前端历史图表**：属 P13.4，本子任务只交付后端端点与三层存储。曲线渲染、缩放、跨度
   切换、本地时区展示一律未验。
2. **`go test -race`**：宿主 Windows 无 gcc、`-race requires cgo`，与 P12.3/P12.4/P12.5 同。
3. **"365 天真实老化"与行数上限裁剪**：验收把三层保留窗口压到 raw 3 分钟 / minute 2 小时 /
   hour 4 小时，验的是行为等价；默认配置（24h / 30d / 365d）下的实际耗时与磁盘占用，以及
   `MaxRows` 超限后"丢最老整桶"的路径，只有单测覆盖，真机没跑到那个量级。
4. **跨小时的长停机补算**：验过的缺口是 26 秒；代理停两小时后重启，跨小时的
   `PendingMetricBuckets` 自愈补算没有真机样本。

`NOT RUN` 不等于通过；这几条记在 `docs/PROGRESS.md` 第 8 节"P13.1 的七条限制"里。
