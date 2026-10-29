# P12.5 验收结果：证书续期 timer 接线、备份页与计划任务页依赖的接口形状

- 日期：2026-10-25（UTC）
- 分支 `main`，commit `8e52b36`（工作树未提交，按用户要求不提交不推送）
- 验收对象：**定时续期这条链路到底跑不跑得通**（面板建计划 → 代理写 systemd 单元 →
  systemd 唤醒 → `upanectl schedule-trigger --wait` → 代理提交 `certificate.renew` →
  certbot 真的被调起来 → 结果与两处事实——盘上那张证书、面板账本里那一行——一致），
  以及备份页与计划任务页依赖的接口形状（快照大小、脚本详情包装、再认证矩阵、
  prune 待删名单由预检算、前端产物里有没有这些动作）
- 验收方式：真实 Web（非 root）→ `/run/upanel/agent.sock` → root 代理 → 真实 systemd
  + 真实 ACME（本机 Pebble，P10.3 留下的夹具）。两台专用 Ubuntu VM **同一份脚本、
  同一组二进制、不并发**。

## 1. 结论

| VM | 发行版 | systemd | certbot | 轮次 | 结果 |
|---|---|---|---|---|---|
| 2404 | Ubuntu 24.04.5（`6.8.0-147-generic`） | 255（`255.4-1ubuntu8.18`） | 2.9.0 | 第 12 轮 | **PASS 214 / FAIL 0 / NOT RUN 0** |
| 2204 | Ubuntu 22.04.5（`5.15.0-198-generic`） | 249（`249.11-0ubuntu3.23`） | 1.21.0 | 第 2 轮 | **PASS 214 / FAIL 0 / NOT RUN 0** |

两台退出码都是 0，逐节断言数完全相同（25 / 20 / 44 / 24 / 30 / 15 / 20 / 10 / 19 / 7），
把随机 id（`sch_*` / `job_*` / 测试域名后缀）归一之后 `logs/*/results.txt` 的断言名单
`diff` 无输出。两台跑的是同一份脚本（md5 `9fd078cbd334f9081b53a0a8f1eecc7b`）和
同一组二进制。

**这一阶段的红几乎都不是"跑不通"，而是"看起来在做事"。** 定时续期最危险的地方是它
发生在凌晨三点、没有人看：预检放行了但绑不上目标、任务显示成功但盘上什么都没换、
失败只留一句"动作执行失败"——这三种都比直接报错更难发现。所以本轮的判据不是
"接口返回 200"，而是"两处事实对得上"：`fingerprint`、`not_after`、`last_error`
在账本、任务表、盘上证书三处必须一致。

## 2. 交付指纹

构建入口唯一：`scripts/build-linux.sh`。

```
VERSION=0.0.0-dev COMMIT=8e52b36 BUILD_DATE=2026-10-25T16:54:19Z sh scripts/build-linux.sh
```

| 产物 | sha256 |
|---|---|
| `/var/tmp/upanel-bin/upanel-web` | `251b4f1da8097713f67a32e70eed2898e88020a77cafe12850fe959c29124054` |
| `/var/tmp/upanel-bin/upanel-agent` | `0be3a87f2572c8c70cebbd38e91aa01d46b3068057a720d790208841cf71da7c` |
| `/var/tmp/upanel-bin/upanelctl` | `9ebc49243c6520816feec87b22c5124f536c05e1303dd0d8d06926efe3afb25f` |
| `tests/integration/p12-5-acceptance.sh` | md5 `9fd078cbd334f9081b53a0a8f1eecc7b` |

两台 VM 上的 `sha256sum` 由脚本自己写进 `payloads/*/env.txt`，与上表逐字一致。

## 3. 这一阶段撞到并修掉的产品缺陷

四条里前三条是"会静默做错事"的那一类，第四条是形状问题。

**(1) 定时续期在预检层被判成"实现错误"。** `AssessIssue` 把空的 `certificate_id`
当成目标身份指纹，而定时触发那一刻根本没有 id 可填（触发发生在 systemd 里，没有
面板可问）。判决是 `allowed=true` 却没有绑定，注册表按 `internal/agent/preflight.go`
的规则回 500 `INTERNAL_ERROR`，systemd 只看到单元失败，人到手的是一个数字。修法：
`IssueParams.targetIdentity()` 在没有 id 时回退成 `domain:<域名>`——盘上
`live/<domain>` 那一份就是唯一候选。`TestAssessIssueBindsTargetIdentityForScheduledRenewal`
钉住三条（scheduled 参数必须绑上身份、换域名身份跟着变、手工路径仍然用证书 id）。

**(2) 失败原因到不了管理员眼前。** certbot 已经在 stderr 里说清了原因，但注册表在动作
失败时只写 `error.message`，于是面板与账本上留下的是"动作执行失败"——一次每晚都在
失败的定时续期，看不出是 80 不通、账号失效还是速率限额。修法：
`certbotFailure` 实现 `FailureReasoner`（结构性的，agent 侧用 `errors.As` 找），
`internal/certificates/ops.go` 走不通的那条路都带上 `oneLine(err.Error(), 300)`。
本轮 `schedule-fail-job-reason-mentions-certbot` 与 `ledger-a-last-error-is-job-reason`
就是这条链路的判据：任务表里那句和账本 `last_error` 必须**同一个字符串**，并且要
出现 certbot 自己的话。

**(3) `Module.Apply` 里 P12.4 留下的"证书续期尚未接线"闸没拆。** 三件事（定义存
`certificate{domain,staging}`、代理按域名查盘上目录、结果按 observed 回写）补齐之后，
闸还立着，于是 `schedule.apply` 一律被拒。闸拆掉后 `schedule-a-submit-202` 才可能绿。

**(4) `normalized_params` 里出现空的 `certificate_id` 与 `email`。** 面板把代理交上来
的定时任务导入任务表时，会按 `IssueParams` 结构体重新编码一遍参数，于是计划里本来
没有的两个字段以 `""` 出现。这不是难看，是**审计行必须长得像它记录的那次操作**：
真机 `schedule-a-job-params-no-certificate-id` 红的就是这个形状。修法是两个字段加
`omitempty`，并由 `TestScheduledRenewalParamsMarshalWithoutEmptyIdentityFields` 钉住。

产品面改动的文件只有两个：`internal/certificates/ops.go`、
`internal/certificates/scheduled_test.go`（新增两条测试）。

## 4. 验收方法的修正（六条，全是"夹具/判据自己在骗人"）

**(a) §3 prune 数的是仓库总条数。** 仓库跨轮次累积快照，而 prune 的
`ListSnapshots` 按策略过滤、快照列表接口不过滤，`prune-left-keep-last` 期望 2 实际 16。
判据改成让**产品再算一次**：新一轮预检的待删名单必须为空（`expected_delete_ids`
长度为 0），且 prune 之后清单减少的条数**精确等于**待删条数。

**(b) §5 的水位与账本 before 取在触发之后。** `wait_job_row` 用
`jobs.rowid > watermark` 找"本轮新导入"的行，水位取晚了就把本轮那行漏在水位以下
（`schedule-a-job-imported-succeeded` 等六条红，而产品是对的）。两处取值都挪到
`systemctl start` 之前。

**(c) §7 关掉校验口 5002 造不出执行期失败。** 第 10 轮六条红全指这里：5002 关掉、
80 照开，任务却 `succeeded`，盘上还换了一张新证书。certbot 日志把原因写死了——
那次订单拿回来的授权是 `"status":"valid","validated":"2026-10-25T17:05:50Z",
"expires":"2026-10-25T18:05:52Z"`：§5 通过的那张 HTTP-01 授权 Pebble 缓存了一小时，
§7 的订单直接 `ready`，certbot 一次校验都没做。只要缓存这条路存在，**"关校验口"
就永远造不出失败**。修法：停的是 ACME 目录本身（14000），失败落在 certbot 自己的
第一次请求上，走的是同一段 `FailureReasoner` 链路；80 仍然开着，预检放行，失败才
落在执行期而不是被拦成 `blocked`。第 10 轮的现场留在
`logs/2404/earlier-round10-section7-fixture-defect.log`。

**(d) §0 从来没有断言 80 与 5002 是空的。** 22.04 第一轮 `http01-webroot-port-80`
第一条红，下游 68 条跟着红——一条真缺陷都没有：P10 验收留下的 nginx 开机就占着 `:80`，
夹具的 `python3 -m http.server` 在 bind 时 `EADDRINUSE` 当场退出，日志里只有一段
traceback，而每条证书断言都以"探针不是 200"的形式连带变红。修法：§0 先用
`ss -ltnH` 断言这两个口是空的，被占了就按头文件写好的语义打一行
`NOT RUN 端口 80 已被其它进程占用` 并退 2——**环境不满足时不产出假红**。
首轮日志留在 `logs/2204/first-round-nginx-blocked.log`。

**(e) §9 拿四十分钟前的会话去读接口。** 22.04 第一轮一轮跑了 38.5 分钟，§9 五条读到
401——红的不是接口，是会话有效期。这一节验的是"页面要的读接口在不在"，不是
"会话能活多久"，所以进 §9 前重新登录一次（`ui-relogin-for-read-checks`）。
24.04 跑 21 分钟时这五条一直是绿的，问题只在慢机上暴露。

**(f) 验收脚本自己的采样器把机器压垮了。** 第 7—9 轮 certbot 三次撞上 120 秒上限，
日志停在 "Storing nonce"，而 Pebble 早就签好了。真凶是采样器：`ps -eo pid,stat,pcpu,wchan
-C certbot` 里 `-e` 覆盖了 `-C`，每 5 秒列出全部进程，`ps` 自己吃 150—185% CPU。
`sar -q` 的历史采样把话说清楚了：13:40 负载 7.34、14:40 8.28（无采样器）、16:10 7.85，
而机器真空闲时 `top` 是 98.5% idle、load 1.09。9 条可运行任务分 2 个 TCG 核再叠数倍
减速，十几秒的活就长到了一百多秒。修法：采样器改成廉价的 `ps -C`，并在四次 certbot
调用前加 `wait_quiet`（1 分钟负载回落到 1.5 以下再继续，最多等 180 秒，等不到就照常
触发、超时如实算失败）。

## 5. 两台机器的差异（值得单独记的两条）

- **`wait_quiet` 在 24.04 上四次都没等到，在 22.04 上四次都等到了。** 24.04 是 TCG
  模拟的 2 vCPU 客户机，验收脚本自己跑着的时候 1 分钟负载就在 3.3—3.8，永远回不到
  1.5；22.04 是硬件虚拟化，四次都落到 1.43—1.48 才继续。这道闸不是摆设，但它的
  阈值是"这台机器能回落到多少"的函数，**别把它当护栏**：24.04 那轮 certbot 没超时，
  是因为宿主那段时间真的空闲，不是因为闸起了作用。
- **同一份脚本，24.04 跑 1290 秒、22.04 跑 660 秒**（21.5 分钟 vs 11 分钟）。
  断言名单不受影响，但"慢到会话过期"这类次生现象只在 24.04 上出现——(e) 就是这么
  被发现的。

## 6. 宿主侧检查

`PATH` 前缀加上 `.nvmrc` 锁定的工具链（`H:\Code\.toolchains\node-v22.23.3-win-x64`）后
`sh scripts/check.sh` → **PASS 13 / FAIL 0 / NOT RUN 0**，与 P12.3/P12.4 同口径
（`logs/host-check.txt`）：

| 步骤 | 结果 |
|---|---|
| `go-build` / `go-vet` / `go-test` / `go-build-linux` / `go-vet-linux` / `gofmt` | PASS（`go test ./...` **43 个包 ok**） |
| `node-version` | PASS（v22.23.3） |
| `web-typecheck` / `web-lint` / `web-test` / `web-build` | PASS（**49 个文件 939 个用例**） |
| `web-smoke` | PASS（Playwright/Chromium，10 项；走首页、`/storage`、`/storage/devices/sda` 与 404 路由） |
| `openapi-lint` | PASS（`valid`，6 warnings：`no-unused-components` 3、`operation-4xx-response` 2、`operation-2xx-response` 1；与 P12.4 记的 10 条不同，本轮 `api/openapi.yaml` 有改动） |

**第一次是拿系统 node 跑的，PASS 11 / FAIL 2**（`logs/host-check-system-node.txt`）：
`node-version` 期望 `v22.23.3` 实际 `v24.15.0`；`openapi-lint` 在校验输出 `valid` 之后
以 `Assertion failed: !(handle->flags & UV_HANDLE_CLOSING), file src\win\async.c` 退出 127。
两条都落在 node 工具链这一层，换成锁定版本后同一份代码、同一条命令全绿——所以
`check.sh` 里那道版本门是**能做事的门**，不是形式。这次也顺手记下来：报告宿主检查时
必须先确认 `node -v`，否则拿到的 11/2 会被误读成产品问题。

## 7. 未验证 / 受限（NOT RUN，如实记录）

1. **浏览器里的真实点击没有跑。** 本轮验的是"前端产物真的打进了二进制、里面带着这些
   动作名与字段名"（§8）以及"页面依赖的读接口活着"（§9），**没有**用浏览器去点备份页
   与计划任务页上的按钮。宿主 `check.sh` 的 `web-smoke`（Playwright/Chromium）走的是
   首页、`/storage`、`/storage/devices/sda` 与 404 路由，**不覆盖这两个页面**。渲染、
   滚动、对话框焦点、提交后的界面反馈一律未验证。
2. **"certbot 超时但证书已经写下来"那条分支没造出现场。** 代码里超时之后先看盘上有没有
   证书（`internal/certificates/certbot.go`），有就按成功处理；本轮没有一次真的超时，
   所以这条分支只有单测覆盖，没有真机证据。
3. **生产 Let's Encrypt 一次都没碰。** 全程 Pebble + `staging:true`，速率限额、真实
   账号、真实 DNS 解析都不在证据范围内。
4. **`go test -race` 仍然未执行**（宿主无 gcc，`-race requires cgo`），与 P12.4 相同。
5. **22.04 的 nginx 是被停掉的**，没有验证"nginx 占着 80 时面板会怎么表现"——那属于
   P10 的范围，本轮把它当前置条件排除了。

## 8. 文件清单

```
result.md                                      本文件
logs/2404/full-run.log                         24.04 第 12 轮完整输出（214 条断言逐条）
logs/2404/results.txt                          24.04 断言名单
logs/2404/load-sampler.log                     24.04 整轮负载与进程采样（10 秒一次）
logs/2404/earlier-round10-section7-fixture-defect.log   §7 六条红的现场（夹具缺陷证据）
logs/2404/{agent,web,pebble,pebble-cert,pebble-restored,http80,http5002,unit-*,certbot-register}.log
logs/2204/full-run.log                         22.04 第 2 轮完整输出
logs/2204/results.txt                          22.04 断言名单
logs/2204/first-round-nginx-blocked.log        22.04 首轮：nginx 占 80，68 条假红
logs/2204/{agent,web,pebble,pebble-cert,pebble-restored,http80,http5002,unit-*,certbot-register}.log
logs/host-check.txt                            宿主 scripts/check.sh（锁定工具链，13/0/0）
logs/host-check-system-node.txt                 同一份代码用系统 node v24.15.0 跑的那次（11/2）
payloads/2404/, payloads/2204/                 每一次请求/响应与 env.txt（含三个二进制 sha256）
```
