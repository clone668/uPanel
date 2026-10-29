# P12.4 验收结果：受管理 timer、时区/错过执行/不重叠/超时、内置动作复用、非 root 脚本

- 日期：2026-10-24 起，交付轮 2026-10-25（UTC）
- 分支 `main`，commit `8e52b36`（工作树未提交，按用户要求不提交不推送）
- 验收对象：`schedule.apply` / `schedule.delete` / `schedule.trigger`、`script.apply` /
  `script.run` / `script.delete` 的真实实现，以及"到点自己跑"这条链路
  （代理 → systemd 单元 → `upanectl schedule-trigger` → 任务框架 → 面板对账导入）
- 验收方式：真实 Web（非 root）→ `/run/upanel/agent.sock` → root 代理 → 真实 systemd，
  两台专用 Ubuntu VM 各跑一轮，**同一脚本、同一组二进制、不并发**

## 1. 结论

| VM | 发行版 | systemd | 轮次 | 结果 |
|---|---|---|---|---|
| 2404 | Ubuntu 24.04.5（`6.8.0-147-generic`） | 255（`255.4-1ubuntu8.18`） | 第 5 轮 | **PASS 246 / FAIL 0 / NOT RUN 0** |
| 2204 | Ubuntu 22.04.5（`5.15.0-198-generic`） | 249（`249.11-0ubuntu3.23`） | 第 2 轮 | **PASS 246 / FAIL 0 / NOT RUN 0** |

两台逐节断言数完全相同；把随机 id（`sch_*` / `scr_*`）归一之后，`results.txt` 的断言名单
`diff` 无输出。

**这一轮的红不是"跑不通"，而是两个会静默做错事的缺陷**：单元只写了 `Type=oneshot` 就声称
"不重叠"，实际拦不住第二分钟的唤醒（Q371——长任务会每分钟叠一层）；root 跑的
`schedule-trigger --wait` 读不到它要等的那次操作，一直在空转、靠 `TimeoutStartSec` 才结束
（Q380）。两条都属于"看起来在工作"的那一类，所以本轮判据的重点从"单元是不是 active"
换成了"唤醒撞上运行中的单元时发生了什么"。

**22.04 是第二轮才绿的，首轮 5 条红全是验收工具自己的毛病（Q386、Q387），产品当时是对的**，
两轮都留了记录，见 §6。

## 2. 交付指纹

构建入口唯一：`scripts/build-linux.sh`；同一条命令连跑两次，三个产物逐字节相同。

```
VERSION=0.0.0-dev COMMIT=8e52b36 BUILD_DATE=2026-10-25T07:59:14Z sh scripts/build-linux.sh
```

| 产物 | sha256 |
|---|---|
| `/var/tmp/upanel-bin/upanel-web` | `0a9f0b233a1afdff570a75805a999e7c90e1d7105796a87a16fc8b276488ae7b` |
| `/var/tmp/upanel-bin/upanel-agent` | `61eec0a30a84436ed0ce05f38ce608103cad3053feb6ab2cf930bee1efe08adb` |
| `/var/tmp/upanel-bin/upanelctl` | `ec35b9e4ed058586653021e904bdfa3b4a8f49daedffa2676e8cea38bbc9daae` |
| `tests/integration/p12-4-schedule-acceptance.sh` | `097b04f6491f747f0519da018abd848ff00faecd7203e3c706a6f9a42a524562` |
| `tests/integration/json_helper.py` | `767b9844676a7f570c195b4de1a2e1768bb1f50b5aa7219963c016c1c9d6a837` |

两台 VM 的 `payloads/<vm>/env.txt` 里三份 sha256 与上表逐字相同，版本戳均为
`0.0.0-dev (commit 8e52b36, built 2026-10-25T07:59:14Z, go1.27.3)`。

## 3. 逐节断言数（两台完全相同）

| 节 | 内容 | 断言数 |
|---|---|---|
| §0 | 前置环境、版本、`upanel-shell` 的组与身份 | 18 |
| §1 | 受管理脚本：登记、落盘权限、正文只在详情里 | 22 |
| §2 | 立即运行：以 `upanel-shell` 身份、拿不到 root | 11 |
| §3 | 计划：定义 → 单元 → systemd 的原话 | 43 |
| §4 | 时区：同一表达式、两个时区，是两个时刻 | 8 |
| §5 | 错过执行策略与停用：改的是同一份定义，单元跟着改 | 20 |
| §6 | 触发：稳定 ID、重复判决、只认 root | 16 |
| §7 | 定时操作被面板对账导入任务表 | 14 |
| §8 | 超时：上限是定义里的数 | 16 |
| §8a | `--wait` 真的在看这次操作（而不是卡在某个 403 上） | 4 |
| §8b | 不重叠：上一次还在跑时，定时器不会起第二次 | 19 |
| §9 | 证书续期定时任务的接线（P12.5） | 5 |
| §10 | 备份策略也能被定时（内置动作复用任务框架） | 8 |
| §11 | 代理不在时：503，不是空列表 | 12 |
| §12 | 权限：viewer 读不到计划与脚本 | 6 |
| §13 | 删除：引用保护、单元清理、文件清理 | 25 |
| 合计 | | **246** |

### 关键任务 id（24.04）

取自 `payloads/2404/job-*.json` 的 `data.job_id`，可直接按 id 复核：

| payload | job id |
|---|---|
| `job-schedule-create` | `job_620a3a25bcfe44e2eb6ea4055deb173e` |
| `job-schedule-utc` | `job_91406d532de33acb8462c5beeb695321` |
| `job-schedule-disable` | `job_00b6ad608d603924a7875200cd04f8ef` |
| `job-schedule-timeout` | `job_f7d87c31add2e49ec61b32f45051de3c` |
| `job-schedule-overlap` | `job_a70402602dd36fe94e4b1ffdb15ef899` |
| `job-schedule-backup` | `job_bf9b9ce95d17d08fb7f46a48c3571481` |
| `job-script-run` | `job_ded9c187fcd78d7b541da676543d2ffd` |

22.04 是独立一轮、id 全不同（例如 `job-schedule-create` =
`job_7f07870d1fa1f6d96c7d1da90a148a8d`、`job-schedule-overlap` =
`job_5d5bea442ca2c23c5b06253a28def2d9`），不能拿一机的 id 去另一机查。

## 4. 几条最要紧的判据

**"不重叠"是"两次运行不并行"，不是"总共只触发一次"。** 这两件事被混了很久（Q383）。
systemd 249 与 255 都会把到期时撞上运行中的唤醒**排队补跑**，所以触发数 2 不是缺陷；
真缺陷是同时有两个在跑。现在的判据从 journal 读事实（`logs/<vm>/overlap-unit-sequence.txt`）：

```
overlap-runs-do-not-overlap        序列必须是 S,F,S,F…（空序列单独判 empty，不混成重叠）
overlap-first-run-outlives-the-wake 第一次运行 ≥61 秒，证明 T+60 那次唤醒撞上的是运行中的单元
```

本轮两台各捕到一次运行、77 秒（`单元启动/结束序列 SF，第一次运行 77 秒，触发记录 1 条`）：
脚本在 T+65 抽样之后把定时器停掉，排队的那次没来得及起。**"补跑"的形状证据不来自本轮窗口**，
来自 Q387 定判据时对两台真 journal 的解析——22.04 `sequential 79 SFSF`、24.04
`sequential 82 SFSF`，同一解析器把伪造的 `S S F` 判成 `overlap:SSF`、把空文件判成
`empty`。这里如实写明：本轮窗口只能证明"运行中撞上唤醒、没有并行"，不能证明"补跑也会
各跑各的"。

**`--wait` 必须能读到"这一次"操作。** 单元 `activating` 不等于"在等这次操作"——root 调
`/internal/v1/operations/:id` 曾被 403 挡住，`--wait` 空转、单元一直 active，看起来完全
像"不重叠已实现"（Q380）。所以 §8a 不是读状态码，而是读它等的那条操作
（`overlap-wait-reads-own-operation`、`overlap-wait-no-permission-warnings`），并且
`wait-failed-exit` / `wait-reports-terminal-stage` / `wait-exits-within-deadline` 三条
证明失败时退出码非 0、且不会赖到超时才收手。

**时区是两个时刻，不是两串字符。** §4 用同一条 `on_calendar` 分别在
`Asia/Shanghai` 与 UTC 各建一条计划，断言两个 `next_elapse` 相差 8 小时整
（`timezone-offset-eight-hours-mod-day`），且 wall clock 在两边分别读作本地时间与 UTC
（`schedule-next-elapse-wallclock-asia`、`timezone-wallclock-utc`）。

**超时的上限来自定义，不来自"跑到天荒地老"。** §8 建一条 3 秒上限的计划，操作终态必须是
`failed` 且原因可读——22.04 的原文（`logs/2204/agent.log`）：

```
{"level":"ERROR","msg":"动作执行失败","action":"script.run",
 "error":"agent: 动作执行失败: 脚本 \"p12-4 超时 8edab69b\" 超过 3 秒被终止"}
```

同时 `unit-timeout-from-definition` 证明单元 `TimeoutStartSec` 是这个数 + 收尾余量，
`timeout-no-leftover-sleep` 证明被子进程留下的 `sleep` 没有活过任务。

**脚本的手是绑着的。** §0 断言 `upanel-shell` 不在 sudo / docker / disk 组、不是 root；
§2 用它跑 `id`（`script-run-identity`、`script-run-uid-not-zero`）、让他去写 `/etc`
（`script-run-cannot-write-etc`、`script-run-no-root-file`）。落盘是
`root:upanel-shell 0750`（`script-probe-file-mode`、`script-probe-file-not-world-readable`），
运行前核对 SHA-256（`script-detail-content-matches-hash`）。

**代理不在就是 503，不是空列表。** §11 停掉代理后 `/schedules`、`/scripts`、详情三条都
必须 503 且码是 `AGENT_UNAVAILABLE`（`schedules-agent-down-503`、`scripts-agent-down-code`、
`schedule-detail-agent-down-503`），重启后列表必须"回来且非空"
（`schedules-back-200`、`schedules-back-not-empty`）——空列表会让管理员以为计划都没了。

**触发历史不随计划删除而消失。** 迁移 `0010_schedules.sql` 故意不给
`schedule_triggers.schedule_id` 建外键，并把整张表设成只追加（`BEFORE DELETE … RAISE(ABORT)`）。
§13 删完四条计划后查库，两台都还留着记录（24.04 18 行、22.04 9 行，见
`migration/<vm>/migration-after.txt`），这是设计，不是泄漏。

**读一份列表的代价随条数增长，预算也得跟着定。** 24.04 上一轮整条列表读了 10144 毫秒，
压过 Web 侧 10 秒通用上限，一个健康的代理被报成"代理不可用"（Q385）。改成 4 路有界并发
组装后，本轮同一份列表读 **493 毫秒**（`logs/2404/web.log`，
`"path":"/api/v1/schedules" … "duration_ms":493`）。

但并发只压掉了"逐条相加"，压不掉机器本身的慢：本轮 24.04 上有两次**单条**详情读了
9149 与 8795 毫秒（都发生在长任务跑着的时候，`logs/2404/agent.log` 的
"代理请求耗时偏长"）。所以计划/脚本镜像单独给了 30 秒客户端（`ScheduleQueryTimeout`），
而不是靠"并发之后肯定快"来赌。两台本轮日志里 `AGENT_UNAVAILABLE` 各 3 条，全部来自 §11
主动停代理的那三次（08:11:09—08:11:10），没有一条是超时造成的。

## 5. 真机抓出的产品缺陷（本轮修掉并复验）

| 编号 | 缺陷 | 现象 | 修法 |
|---|---|---|---|
| Q371 | 只写 `Type=oneshot` 就声称"不重叠" | `schedule-trigger` 交完触发就返回，单元几十毫秒结束，长任务每分钟叠一层（第二个操作在资源锁上排 30 秒后 `RESOURCE_BUSY`） | `schedule-trigger --wait` 轮询到操作终态才退出；`TimeoutStartSec` = 上限 + 60 秒收尾 |
| Q374 | `scripts.interpreter` 库里存基名 `sh`，契约写 `/bin/sh` | 同名字两种说法，最后以 `CHECK constraint failed` 的形式炸在真机上 | 统一成绝对路径，`migration-after.txt` 里 `interpreter` 列现在全是 `/bin/sh` |
| Q375 | `systemd-analyze calendar` 的输出形状随宿主机时区变 | UTC 机器上少打一行 `(in UTC)`，于是**每一条计划都被判成"不会触发"** | 只认 `Next elapse:` 那一行，不再依赖时区注解 |
| Q376 | 脚本目录的上级 `/var/lib/upanel-agent` 是 `0700 root:root` | `upanel-shell` 连穿都穿不过去，脚本永远跑不起来 | 每一级上级只补 `other` 的 `x`（`0700` → `0711`：能穿过去、列不出来），脚本自身仍是 `root:upanel-shell 0750` |
| Q377 | 库里的 `UNIQUE(kind, target_id)` / `UNIQUE(name)` 不在预检说话 | 预检 `allowed=true`，提交 409 | 预检读同一份约束，`schedule-target-taken-*` / `schedule-name-taken-*` 三条一组 |
| Q380 | root 跑的 `--wait` 读不到它要等的那次操作 | root 调 `upanel-web` 的路由回 403，`--wait` 空转，单元靠 `TimeoutStartSec` 收场 | 给 root 一条只读"这条计划自己触发的那一次操作"的窄路由；等待余量小于单元余量，超时那一刻先有话说 |
| Q381 | 导入即终态的任务 `needs_sync` 永远停在 1 | 脚本两秒结束、对账三十秒后才导入，条件更新跳过终态行时把这一列也挡住了，扫描每 30 秒再问一遍代理 | `needs_sync` 单独收掉：已读到代理的视图就是核对过了，状态一字节不改 |
| Q382 | `busy_timeout` 5 秒仍然 `database is locked` | 日志里一簇 `SQLITE_BUSY (5)`：不是死锁，是几个写者在 5 秒窗口互相摊薄 | `BEGIN IMMEDIATE` 失败且 `Code()` 是 5/6 时退避重试三次（100ms、300ms），用尽如实报错 |
| Q385 | 一条计划列表读了 10144 毫秒 | 代理逐条串行组装，每条 fork 两次 systemd（`systemd-analyze calendar` 250—330ms），12 条就是十秒量级 | 4 路有界并发（顺序按下标回填、取消时如实报错）+ 计划/脚本镜像单独 30 秒客户端 |

## 6. 验收方法上的修正（不是产品缺陷，但会误导判断）

- **Q370**："socket 文件在"不等于"代理起来了"。Unix socket 是文件系统对象，上一次崩溃会把
  它留在原地；改成"存在 **且** 以 `upanel-web` 身份 `GET /internal/v1/capabilities` 成功"。
- **Q372**：行式 JSON 助手的答复不能带裸换行。一条多行 `content` 会把后面几十条断言读成
  别人的值，表现成"大面积 `helper-mismatch`"。跨行值必须 `json.dumps` 过一遍。
- **Q373**：脚本把 `upanelctl` 装成 `upanectl`，用自己的错拼证明自己没问题——安装后必须核
  哈希（`upanelctl-hash-matches`）。
- **Q378**：探针写不出证据却回 0，三条身份断言拿到空文件、任务却判 `succeeded`。
- **Q379**：数 `--wait` 的出现次数会把单元文件里的注释也数进去。
- **Q383**：见 §4——"一共只触发一次"不是契约。
- **Q384**：读数工具把两种"没有"混成一种（JSON `null` 被印成字符串 `None`，`[ -n ]` 因此
  永远为真），另有 `import_probe` 量到中间态就返回。
- **Q386**：探针把"收敛"定成 `needs_sync==0`。那个标记问的是"抄回来过没有"，不是"跑完了
  没有"——操作还在跑时它也会被清 0（那是对的）。于是探针提前返回，`timeout-operation-failed`
  读到 `running`、原因读到 `{}`，而同一行 28 秒后自己走到 `failed`，错误原文一字不差。
  判据改成**终态**且事件 ≥2。
- **Q387**：见 §4。22.04 首轮三条红都在这里：`journalctl` 的行文在两代 systemd 上不一样
  （249 只写 `Starting uPanel 计划任务：…`，不出现单元名），而 `is-active` 是在 T+65 醒来
  看一眼的抽样——慢机器上醒来时第一次运行已经结束。**同一份真 journal 证明实现是对的，
  错的是判据。**

## 7. 宿主检查

`sh scripts/check.sh`（PATH 前置 `C:\Users\huxi\AppData\Local\nvm\v22.23.3`，按 `.nvmrc`）
最终 **PASS 13 / FAIL 0 / NOT RUN 0**：

```
go-build go-vet go-test go-build-linux go-vet-linux gofmt node-version
web-typecheck web-lint web-test web-build web-smoke openapi-lint
```

- `web-test`：45 个文件、820 个用例全过，`production` 冒烟 10/0/0（未登录、落到登录页、
  无未捕获异常、无失败请求）。
- `openapi-lint`：`api/openapi.yaml` 通过校验，6 条警告（`no-unused-components: 3`、
  `operation-4xx-response: 2`、`operation-2xx-response: 1`），数量与类别和 P12.3 记录相同。
  本轮新增的 `/api/v1/schedules*`、`/api/v1/scripts*` 已进契约文件。
- 日志：`logs/host-check.txt`。

`gofmt -l` 无输出；构建走 `scripts/build-linux.sh`，`BUILD_DATE` 固定后连跑两次逐字节一致
（三个产物的 sha256 见 §2）。

**`go test -race` 未执行**（NOT RUN）：宿主 Windows + Go 工具链无 gcc，`-race requires cgo`，
`CGO_ENABLED=1` 后 `cgo: C compiler "gcc" not found`。这条不折成"并发已验"——
`schedules.Views` 的并发用例只证明顺序回填与取消传播，不等于竞态检测。

## 8. STAGES 的 P12 验收条目对到本轮

`docs/STAGES.md` §14 的"验收"是一整段，P12.4 只承担其中属于 timer 与脚本的部分。逐条对一遍：

| STAGES 条目 | 本轮状态 | 对应判据 |
|---|---|---|
| 同计划重复触发仅运行一次 | **已验（语义已校正）** | §6 `trigger-same-slot`、`trigger-second-duplicate`、`trigger-second-same-operation`、`trigger-second-same-trigger-id`（幂等靠 `UNIQUE(schedule_id, slot_at)`）+ §8b 不并行 |
| 系统时区不同仍按指定时间执行 | **已验** | §4 整节（`timezone-offset-eight-hours-mod-day`、`schedule-next-elapse-wallclock-asia`、`timezone-wallclock-utc`） |
| 脚本不能升级为 root | **已验** | §0 组身份 4 条 + §2 `script-run-uid-not-zero`、`script-run-cannot-write-etc`、`script-run-no-root-file` |
| 错过执行策略 | **已验** | §5 `unit-timer-persistent-true` / `-false`、`unit-timer-misfire-documented` |
| 超时必须真实终止 | **已验** | §8 `timeout-operation-failed`、`timeout-operation-reason`、`unit-timeout-from-definition`、`timeout-no-leftover-sleep` |
| 内置动作复用任务框架 | **已验** | §7 全部 14 条（导入行、系统操作者、`cancellable=0`、事件与预检）+ §10 `job-schedule-backup` |
| 证书续期 timer | **不属本轮** | §9 只验"接线前就 blocked 且指向 P12.5"（`schedule-cert-blocked-says-p12-5`），真正的续期留给 P12.5 |
| 备份后实际恢复并校验文件哈希 / 错误密码 / 主机密钥 / 存储满 / 网络断开 / 清理范围 | 由 P12.1—P12.3 承担，本轮不重复计 | 见 `2026-10-24-p12.3/result.md` |
| 备份与定时任务 UI | **不属本轮** | P12.5 |

## 9. 没做的、以及"做了也不能算数"的

- **`go test -race` 未执行（NOT RUN）**：宿主是 Windows + Go 工具链无 gcc，
  `-race requires cgo`，`CGO_ENABLED=1` 后 `cgo: C compiler "gcc" not found`。竞态相关的
  证据只有：`schedules.Views` 并发组装的用例（`views_parallel_test.go`）在普通模式下验证
  了顺序回填与取消传播，**这不等于 race detector 的结论**。
- **浏览器 UI 完全没验**：P12.4 没有 UI，P12.5 才有。
- **本轮两台各只捕到一次"不重叠"运行**（见 §4），"补跑也各跑各的"来自早前轮次的真 journal。
- 22.04 首轮那份日志（5 FAIL）保留在决策记录里但不作为产品证据——根因是 Q386/Q387。
