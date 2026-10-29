# P13.4 验收结果：确认动作、投递账读路径、历史曲线的“缺失不是 0”、采集陈旧三态

- 日期：2026-10-26（UTC）
- 分支 `main`，commit `8e52b36`（工作树未提交，按用户要求不提交不推送）
- 验收对象：**“面板怎么把已经发生的事说给读得懂的人听”**——确认动作的语义边界（确认不是
  恢复、幂等、第一个确认人胜出、viewer 不能认领）、投递账的读路径（顶层
  `GET /notification-deliveries`、通道快照、闭集过滤、limit 截断）、历史曲线的三条展示口径
  （缺失不是 0 / 没点不画轴 / null 显示“—”）、采集陈旧三态（未开启 / 没在采集 / 陈旧）
- 验收方式：真实 Web（非 root）→ `/run/upanel/agent.sock` → root 代理 → 真实 Ubuntu 主机；
  受控种入（给不存在的网卡 `accept0` 写 `rx_bytes_per_sec`，5 秒一个桶，值由控制文件决定），
  不压 CPU——“持续 30 秒”是契约尺度，压机器会让断言跟着调度走。两台专用 Ubuntu VM
  **同一份脚本、同一组二进制、一台一跑不并发**。
- 浏览器层为什么不在 VM 上验：VM 里没有 Playwright，前端这一层（历史曲线怎么画、告警行怎么
  排、确认按钮怎么脏）由宿主侧 `vue/server-renderer` 渲染测试与 `web-smoke` 覆盖
  （DECISIONS Q21）。VM 上钉的是**它拿到的事实**，不钉像素。

## 1. 结论

| VM | 发行版 | 结果 | 退出码 | 断言 |
|---|---|---|---|---|
| 2404 | Ubuntu 24.04.5（内核 6.8.0-147，Python 3.12.3，systemd 255，`-Accel tcg,thread=single`） | **PASS** | 0 | 128 通过 / 0 失败 / 0 未执行 |
| 2204 | Ubuntu 22.04.5（内核 5.15.0-198，Python 3.10.12，systemd 249，whpx） | **PASS** | 0 | 128 通过 / 0 失败 / 0 未执行 |

逐节断言数（两台逐字一致）：§0 环境 **7**、§1 装配 **9**、§2 采集基线 **14**、§3 确认的权限
与校验 **14**、§4 确认的语义 **20**、§5 投递账读路径 **31**、§6 缺失不是 0 **12**、§7 已恢复
的事件也能确认 **8**、§8 关掉采集 **10**、§9 卫生 **3**，合计 128。

归一化后的 128 条断言名单跨机 `diff` **没有输出**（这一轮没有任何一行因为量到的数值不同而
不同）。两台唯一的差异在 NOTE 行：`generated_at` 比 `last_sample_at` 晚 **6 秒**（2404）与
**1 秒**（2204）——同一份判定，差在机器速度。

## 2. 交付指纹

| 项 | 值 |
|---|---|
| 代码 commit | `8e52b36`（工作树含本轮改动，未提交） |
| `upanel-agent` | `0d128952eca291e8546a001637b257c1c60adb049fc5d9921c091b7c8c69e671` |
| `upanel-web` | `cef620a74e08c7ddb55616ccb71e92431ea4007ec298ee2a822c065019ad1b14` |
| `upanelctl` | `c66d2c5377305c3cd5c6ff329b7cf387c6e84334ec01f087c51f5dcafadd055b` |
| 验收脚本（最终） | `tests/integration/p13-4-acceptance.sh` sha256 `f86fb06fe8e06d1cdecf4899b5dec2c64355a87439343dd9b456028dac34862c`，LF（`tr -d '\r' \| wc -c` 与 `wc -c` 相等） |
| 验收脚本（上一版） | sha256 `2ce471eed117cf0341223cd6c28048dc76c7c96e02e1d86ddceeba3e1cb5bf78`，被 F-Q429 取代 |
| 二进制来源 | 宿主 `GOOS=linux GOARCH=amd64` 交叉编译，`bin/linux-amd64/`，两台 VM 各自 `sha256sum` 复核一致 |

两台 VM 的发行版与跑分环境见 `logs/2204/env.txt`、`logs/2404/env.txt`；跑分日志是 VM 内
`/tmp/p13-4-run.txt` 原样拷回（22.04 那份 `sha256sum` 前后相同，未经换行转换）。

## 3. 这一轮撞到并修掉的问题（全部是验收侧，产品零改动）

### F-Q429：20 秒新鲜度判据拿宿主 `now` 去量，慢机上假红

24.04 首轮唯一一红：`刚落的样本让健康度落在 20 秒判据内: 期望 'true'，实际 'false'`。

产品没坏，两条证据：同一时刻库里 host 序列 `11:23:00 → 11:25:00` 每 5 秒一条、无缺口；那份
响应自报 `last_sample_at 11:23:37.733` 与 `generated_at 11:23:44.854`，**差 7 秒**，本来就在
`max(3×2s, 20s)` 判据内。

坏的是量尺：断言用宿主 `datetime.now()` 减面板的时间戳，而 24.04 走 TCG 单线程，从取回状态
到算出结果之间隔着一次 sqlite 查询和两次 python3 冷启动（每次三四秒），尺子自己往前走了
十几秒。22.04 走 whpx 快得多，同一条断言一直是绿的——差异全在机器速度上。

改成 `generated_at - last_sample_at ≤ 20`：两个时刻出自同一份响应、同一个进程、同一次快照，
量的正是“面板自己觉得多久没落样本”，这才是契约里那条判据的本义。量到的秒数打成 NOTE
（2404 = 6 秒、2204 = 1 秒），下次判红能一眼分清“数据真陈旧”还是“尺子抖动”。DECISIONS Q429。

### F-Q430：`enabled` 与 `collecting` 被当成一个意思，红了 4 条

第 8 节原本断言“关掉采集 ⇒ `status.enabled=false`”。读代码之后事实相反：
`internal/metrics/reader.go` 的 `Status` 里 `Enabled: true` 是**写死**的（走到这一步就说明
库在），`Collecting: r.recorder != nil`；“查得到旧数据、但不再写新数据”是
`Enabled=true && Collecting=false`，原因进 `Reason`（装配处给的是“配置里关闭了历史采集
（`web.metrics.enabled=false`）：旧数据可查，不再写新数据”）。`enabled=false` 只剩一种来路：
这个进程**没有库**（演示模式/未装配），那时连历史都查不了。

断言与契约一起改，产品一行没动：断言 `collecting=false` + `reason` 含 `web.metrics.enabled` +
旧 series 照样查得到 + 10 秒内 host 行数不涨 + `last_sample_at` 为 null。DECISIONS Q430，
CONTRACTS §13.4 同步重写“两种 enabled 不是一个意思”。

### F-Q431：另外五处验收方法缺陷

| # | 症状 | 真因 | 修法 |
|---|---|---|---|
| 1 | Web 启动失败：`数据库打开失败 / permission denied`，像产品的库坏了 | `install -d -m 0700 $WORK` 配 umask 077 把 `upanel-web` 挡在 `/var/tmp` 下那个目录外 | `$WORK` 自身 0711（能穿过、不能列），里面的日志仍 0700 |
| 2 | 建规则 400，像产品的规则校验坏了 | 请求体里带了 `"scope":"interface"`，而 `alertRuleRequest` 没有这个字段（scope 由 metric + target_id 推出） | 删掉该字段，并加一条正面断言“R1 的 scope 被推成 interface” |
| 3 | “确认前 `acked_at` 是空的”判红 | `SELECT COUNT(*) ... WHERE acked_at IS NULL` 期望写成 0，实际是 1 | 期望值改正 |
| 4 | “缺口不被回填”从 10 变 9 | 窗口右端取 `GAP_END+10` 是**未来时刻**，脚本自己的种入又填了那个桶 | 右端取 `iso_of $GAP_END`（过去的、已定型的时刻），并把 sleep 拉够 |
| 5 | “缺口区间一行原始样本都没有”数出 1 行 | SQL 漏了 `metric` 与 `resolution='raw'`，把落在缺口里的 **minute 聚合行**读成了回填——聚合桶覆盖缺口的一段是聚合的正常产物 | 过滤条件补全，注释写清“聚合行不是回填” |

此外一次判据拆分：第 6 节原先用同一个窗口既问“缺几桶”又问“图上连不连”。定型窗口
（右端 = `GAP_END`）里只有缺口**之前**的点，永远量不到断裂。拆成两个窗口：`gap.json`
（定型，问缺几桶）与 `gap-break.json`（`FROM..GAP_END+30`，问图上的相邻两点是否真的断开）。

## 4. 断言覆盖了什么

- **§0 环境（7）**：root 运行、三个二进制可执行、python3、runuser、服务账号 `upanel-web` 存在。
- **§1 装配（9）**：代理密钥、代理起 socket、`init-admin` 退出码、Web 启动、管理员登录、第二个
  管理员（bob）建号与登录、viewer（alice）建号与登录。
- **§2 采集基线（14）**：`/system/metrics/status` 200 且 `enabled=true`、`collecting=true`、
  `interval_seconds=2`、host 样本开始落库、`last_sample_at` 非空且落在 20 秒判据内、受控序列
  `accept0` 开始写入、两条规则（R1 确认用 / R2 恢复后确认用）建起来、R1 的 `scope` 被推成
  `interface`、受控序列真的把两条事件触发出来并拿到 id。**这一节是后面所有判据的地基**——
  没有真实样本，“陈旧”和“正常”分不开。
- **§3 确认的权限与校验（14）**：未登录 401、viewer 403（只读能看事件、不能认领）、admin 缺
  CSRF 403、`id=0`/`id=abc` 400 且报错点出 id 的规则、未知 id 404 且原文是“告警事件不存在”、
  每一次被拒都落审计且审计里记着错误码。
- **§4 确认的语义（20）**：确认前 `acked_at` 为空；admin 确认 200；`acked_by` 取自**会话**而不是
  提交上来的名字；`acked_at` 非空但 **`state` 仍是 `firing`**、`resolved_at` 没被写；库里记下
  确认人；确认之后事件仍出现在 `state=firing` 列表里；第二个管理员再确认也是 200 且**保留第一个
  确认人、不刷新时间**（幂等 + 首告者胜）；两次成功确认各落一条审计。
- **§5 投递账读路径（31）**：未登录 401、viewer 403、admin 200；行里带着**投递当时**的通道快照
  与事件引用；`state`/`kind` 闭集过滤生效、越界枚举 400；`limit` 正整数校验、超过 200 **按 200
  截断而不报错**；`kind=test` 在没有测试发送时是空；pending/sent/failed 三种状态各自读得对。
- **§6 缺失不是 0（12）**：真停采集 45 秒；缺口区间在 `metric_samples` 里**一行原始样本都没有**
  （不是写了 0）；查询结果的 `missing_points` 数得出缺的桶；相邻两点**真的断开**；缺口里没有
  `value=0` 的假点；重新采集之后缺口**不被回填**。
- **§7 已恢复的事件也能确认（8）**：R2 走完 `firing → ok`，已恢复的事件仍可确认，且确认不改
  `resolved_at`。
- **§8 关掉采集（10）**：以 `web.metrics.enabled=false` 重启后，`enabled` 仍为 true（能回答历史
  问题）、`collecting=false`、`reason` 指向配置里那一项、旧数据照样查得到、10 秒内没有新样本
  落库、`last_sample_at` 为 null。
- **§9 卫生（3）**：代理与 Web 日志无 panic；本脚本没有产生任何 `operation`（确认与查询都不写
  系统）。

## 5. 这一阶段的验收方法修正（写进方法，不写进产品）

1. **判据的两个时刻必须都取自被观测方自己的账本**。Q425 已经定过一次（退避时长），这轮是第二个
   现场（新鲜度，Q429）。规矩现在写死在脚本注释里：凡是要量“被观测方觉得过了多久”，就只许用它
   自己写在同一份快照里的两个时刻相减。
2. **换脚本就必须两台重跑**。脚本从 `2ce471ee…` 改成 `f86fb06f…` 之后，22.04 那份已经跑过的绿
   不能再用——“同一份脚本、同一组二进制”是跨机可比的**前提**，不是修饰。所以最终成绩是：
   22.04 两版各跑一轮（128/0、128/0），24.04 两轮（127/1 → 128/0）。

## 6. 宿主检查

`scripts/check.sh` 在最终工作树上 **PASS 14 / FAIL 0 / NOT RUN 0**（`logs/host-check.txt`）：
line-endings、go-build、go-vet、go-test（46 个包全部 ok）、go-build-linux、go-vet-linux、
gofmt、node-version、web-typecheck、web-lint、web-test（**52 个文件 985 个用例全通过**）、
web-build、web-smoke（production 构建、10 项全 PASS）、openapi-lint（redocly：valid，6 warnings）。

本轮新增的前端测试三个文件 46 个用例：`web/src/api/metrics.test.ts`（分段、边界、单位换算、
`judgeStaleness` 四态、查询串）、`web/src/api/alerts.test.ts`（条件文案、时长/时刻格式、事件摘要、
确认状态、`ackAlertEvent` 打到的路径与方法）、`web/src/components/MetricChart.test.ts`（SSR：
空态、缺口两段、孤立点画方块、刻度与时间轴、零宽区间不出 NaN）。

## 7. NOT RUN（明确未执行，不算通过）

- **浏览器里的真实交互**：VM 无 Playwright；图表缩放/悬停、告警列表翻页、确认按钮的脏状态由宿主
  SSR 渲染测试覆盖，像素与手感未验。
- **真实第三方通道上的确认扩散**：本轮确认不触发通知，通知投递已在 P13.3 验收；“确认后是否该
  发一封”不在本子任务范围（契约未定义）。
- **`go test -race`**：宿主无 gcc，未执行。
- **高负载下采集节奏抖动到判据边界**：本轮用固定 2 秒节奏 + 人为 45 秒停采集造缺口，未在真实
  业务负载下逼近 `max(3×interval, 20s)` 这条线。
- **长时间运行下的聚合与裁剪**（P13.1 已用缩短的保留窗口验过，本轮沿用同一套配置，未重验）。

## 8. 相关文件

- 契约：`docs/CONTRACTS.md` §13.4（两个新端点、确认不是恢复、投递账顶层路径、历史曲线三条展示
  口径、陈旧三态 / 两种 enabled）
- 取舍：`docs/DECISIONS.md` Q427（确认只记“谁知道了”）、Q428（投递账不挂通道下）、Q429（新鲜度
  判据的两个时刻）、Q430（enabled 不是 collecting）、Q431（另五处验收方法缺陷）
- 脚本：`tests/integration/p13-4-acceptance.sh`
- 前端测试：`web/src/api/metrics.test.ts`、`web/src/api/alerts.test.ts`、
  `web/src/components/MetricChart.test.ts`
- 日志：`logs/2204/`、`logs/2404/`（`acceptance.log` + `env.txt`）、`logs/binary-sha256.txt`、
  `logs/host-check.txt`
