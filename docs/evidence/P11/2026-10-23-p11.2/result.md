# P11.2 结果：软件包写事务（刷新索引、安装、卸载、升级）

日期 2026-10-23 · 提交 `0babda4`（代码）· 验收脚本 `675b0eee`（+ `c5a9974`）· 版本标注 `0.0.0-p11.2`
（环境、二进制哈希、虚机清单、实测 apt 成本见同目录 `env.txt`）

## 这一轮做了什么

STAGES §13 第 2 条：**写**。装、卸、升、刷新索引。这是整个 P11 里唯一会改变系统状态的地方，
也是面板第一次伸手去改 `/var/lib/dpkg`。

四件事决定了它的形状：

**一、模拟即准入。** 预检不猜"删掉 A 大概会带走 B"——它跑一次 `apt-get -s`，把 apt 自己算出来的
计划（`Inst` / `Remv` / `Conf` 行、保留包及其原因、`E:` 行原文）当准入依据。面板不复刻依赖解析器。

**二、模拟的结论就是验收标准。** 执行完不看退出码，而是再读一遍 dpkg 登记，与计划逐条比对：
装上的版本是不是 `-s` 写的那一版、删掉的包是不是真的没了、有没有哪个包变成 `iF`/`iU`。
对不上就是 `partial`，并点名"期望什么、实际是什么"。这一条把"命令成功了"和"系统真的变成了
说的那样"分开——两者不是一回事。

**三、锁只探测、从不删除。** 探测方式是"自己也去 `flock` 一次，拿不到就说明有人占着"，
拿到立即释放。外部 apt 正在跑时预检直接 `APT_BUSY`，执行前再复核一次。删锁文件既不会让
那个人停下来，还会让下一个进程以为没人占用——两个 apt 同时写 dpkg 数据库的结果是一台
需要人工救援的机器。

**四、非交互是事先说好的。** 所有写命令都带 `DEBIAN_FRONTEND=noninteractive`、
`Dpkg::Options::=--force-confdef` 与 `--force-confold`、`Dpkg::Use-Pty=0`、
`APT_LISTCHANGES_FRONTEND=none`、`NEEDRESTART_MODE=l`：没有终端可问，配置文件冲突的处置
就必须在执行之前定死。本轮把这一条从"argv 里的一串字"变成了真凭据（见下文）。

权限策略按动作分档，且**在预检之前就算出来**（那时还没跑 `-s`，不知道会连带删几个包，
所以卸载一律要短语，与要删几个包无关）：

| 动作 | 再认证 | 确认短语 | 可取消 |
| --- | --- | --- | --- |
| `package.refresh` | 不要 | 不要 | 不要（不可取消） |
| `package.install` | 要 | 不要 | 不要 |
| `package.upgrade` | 要 | 不要 | 不要 |
| `package.remove` | 要 | **要** | 不要 |

**保护名单在代码里。** `openssh-server`、`netplan.io`、`ca-certificates`、`bash` 等命中即拦，
不接受配置、不接受 API 写入、不接受临时豁免（与 `services/protection.go` 同一个道理）。
表里刻意不做前缀猜（`lib*` 之类）：猜出来的保护会让正常的库升级也过不去。

**中断之后不自动修。** apt 在事务中途被杀（或超时被终止），面板不跑 `dpkg --configure -a`：
自动修复会把"apt 为什么中断"这条线索抹掉，而且用户可能正想看看现场。它做的事是
**如实说出是谁停的**，然后把决定权交回去。

## 代码清单

| 位置 | 内容 |
| --- | --- |
| `internal/apt/simulate.go` | `apt-get -s` 的解析：`Inst`/`Remv`/`Conf`、保留包与原因、`E:` 行原文；宁可认不出来，也不错认 |
| `internal/apt/protection.go` | 删除保护名单（代码内固定，不提供关闭开关） |
| `internal/apt/locks.go` + `probelock_unix.go` / `probelock_other.go` | 四把 apt/dpkg 锁的探测：拿到立刻放，`ENOENT` 算空闲，读不到如实报错 |
| `internal/apt/writer.go` | 写事务的形状与四个动作的固定参数（`--only-upgrade`，永不 `dist-upgrade`） |
| `internal/apt/writer_assess.go` | 预检：探测锁 → 读 dpkg → 跑模拟 → 出结论（能不能做、为什么不能、会发生什么） |
| `internal/apt/writer_exec.go` | 执行：复核锁 → 跑 apt-get → 重读 dpkg → 与计划逐条核对；失败时点名是谁停的 |
| `internal/agent/packageaction.go` | 四个动作登记：参数校验（包名形状、数量上限、拒绝选项样子的名字）、预检结论 → `Plan`、`FailureReasoner` |
| `internal/syscmd` | `Result.Signal`：被信号杀死时把信号名传出来（退出码 -1 什么也没说） |
| `internal/api/jobs.go` / `preflights.go` | 两条写路由的超时按动作放宽（慢机器上一次再模拟 38.7 秒） |
| `internal/operations/service.go` | Web 侧的预检绑定与提交窗口（`slowOperationWriteBudget`） |
| `web/src/api/packageactions.ts` | 冻结的动作名与参数形状、包名的前端形状校验、准入门槛的静态表 |
| `web/src/views/UpdatesView.vue` | 四个写动作接上 `StorageActionDialog`；按钮只认载荷里的 `capabilities` |
| `web/src/components/StorageActionDialog.vue` | 包类动作的提交超时与轮询窗口按动作放宽 |
| `web/src/utils/jobresult.ts` | 任务的 `observed_state` 翻译成"做了什么、凭什么说成功了" |
| `tests/integration/p11-2-package-write-acceptance.sh` | 真机验收 1103 行，16 节 |
| `internal/apt/testdata/2404-sim-*.txt` | 24.04 上真实抓取的 12 份 `apt-get -s` 输出（含连带删除、unmet、already-installed） |

## 验收结果

两台专用虚机，同一份脚本（md5 `675b0eee`），同一批二进制（构建自 `0babda4`）：

    22.04.5（apt 2.4.14）  PASS 169  FAIL 0  NOT RUN 4   exit=0
    24.04.5（apt 2.8.3）   PASS 169  FAIL 0  NOT RUN 4   exit=0

逐节断言数（两台机器**逐节完全相同**，下表的数字由 `logs/{2204,2404}-p11-2-acceptance.log`
按 `== 节标题 ==` 分段、数 `PASS`/`FAIL`/`NOT RUN` 行得到，不是估的）：

| 节 | 内容 | PASS | FAIL | NOT RUN |
| --- | --- | --- | --- | --- |
| 0 | 这台机器的发行版与包管理现场（含升级/conffile 夹具） | 9 | 0 | 0 |
| 1 | 启动代理（root）与 Web（非 root） | 7 | 0 | 0 |
| 2 | 能力表报告四个写动作 | 5 | 0 | 0 |
| 3 | 登录与权限 | 4 | 0 | 0 |
| 4 | 参数边界：这七种请求必须被拒，且不留任务 | 11 | 0 | 0 |
| 5 | 保护名单：apt 愿意删的，面板必须拦（openssh-server） | 13 | 0 | 0 |
| 6 | 连带删除：预检必须点名，且不提交（只读证据） | 5 | 0 | 0 |
| 7 | 不成立的操作：找不到的包、没装的包 | 7 | 0 | 0 |
| 8 | 资源锁：别人占着 dpkg 前端的锁时，预检必须拦下且一步不做 | 7 | 0 | 0 |
| 9 | 真装：cowsay + sl（两个包，一次预检） | 23 | 0 | 0 |
| 10 | 不取消：不可取消的动作，取消请求必须被拒 | 1 | 0 | 0 |
| 11 | 真升：只升点名的包，不做发行版升级 | 13 | 0 | 0 |
| 12 | 刷新索引：正常一次，坏源一次 | 19 | 0 | 0 |
| 13 | 真卸：短语是硬门槛 | 19 | 0 | 0 |
| 13.5 | 硬杀：apt 在事务中途被杀，面板必须说实话、不自动修复、不重试 | 12 | 0 | 0 |
| 14 | 审计：预检、提交、被拒都留痕，且不含口令 | 5 | 0 | 0 |
| 15 | 收尾：机器回到此前的样子 | 9 | 0 | 0 |
| 16 | 明确未覆盖 | 0 | 0 | 4 |

两台机器的断言数一样，不代表跑的是同一件事：24.04 的 apt 慢得多（一次 `apt-get -s` 模拟
10.8—47.2 秒 vs 22.04 的亚秒级），同一批断言在那边是靠把预算与超时按实测放宽之后才全绿的。

`NOT RUN` 的 4 项，两台一致：`APT_LOCK_UNREADABLE`（root 造不出 EACCES，挪锁文件会破坏
验收机的 dpkg；分支由 `internal/apt/locks_test.go` 覆盖）、重启执行与 `boot_id` 前后确认
（P11.3）、浏览器渲染（VM 上没有浏览器，载荷见 `payloads/`）、Windows/macOS 宿主（0.1 不支持）。

**这一轮的 conffile 冲突不再是 NOT RUN。** 上一版把它写成"验收机上没有这种包"，理由是自造
一个需要自建源、会污染现场。现在升级路径本来就用了脚本自建的 `file:` 源（见下），
于是顺手把 conffile 一起做了：两个夹具包各带一个 `conffiles`，装 1.0 之后本地改一行，
2.0 里那一行不一样，升级时 dpkg 真的面对"本地改过、包也改了"。升级完断言的是文件里
**还剩什么**：本地那一行还在、2.0 的版本行没进来、新的那份落在 `.dpkg-dist`——这才是
`--force-confold` 生效的样子。

## 真机抓出的缺陷

**一、被信号停住的 apt，只说"退出码 -1"。** 硬杀验收里把 `apt-get` 杀掉之后，任务错误写的是
`退出码 -1：Reading package lists...`——退出码 -1 既不是 apt 自己的判断，也没说谁停的，
管理员读到的是一句什么都没说的话。信号名本来就摆在 `ProcessState` 里，只是没传出来。
修法：`syscmd.Result` 增加 `Signal`（`SIGKILL`/`SIGTERM`/…，认不出就给"信号 N"），
`internal/apt/writer_exec.go` 用 `stoppedNote()` 把它变成一句人话：
`apt-get install 被信号 SIGKILL 终止（退出码 -1），事务可能停在中间；先核对 dpkg 状态，
再决定是否重试`。超时与信号是两件事：超时由本包自己终止进程组，也走 SIGKILL，
所以区分"谁停的"要看 `TimedOut`，不看信号。

**二、代理的 socket 尸体被当成就绪。** 上一次被 SIGKILL 的代理会留下
`/run/upanel/agent.sock`，而 `[ -S ]` 对尸体也返回真。于是验收脚本在**新代理还没写第一行日志**
的时候就去 grep 日志，报了一条假的 `agent-logs-apt-write-enabled` FAIL。修法：起代理前先删
socket，并且要求**真的**有人应答——用 Web 账号 `curl` 一次 `capabilities`，拿到 200 才算就绪。
这条值得记住的地方是：失败的姿态看起来像"产品没写那行日志"，而实际上是"测试抢跑了"。

**三、apt 锁探测的单测与实现矛盾，而且一直没跑过。** `scripts/check.sh` 在 Windows 宿主上跑，
`//go:build unix` 的测试文件一直被排除，所以 `internal/apt` 的锁探测、`internal/syscmd` 的信号、
`internal/processes` 的信号终止这三包从来没在 Linux 上执行过。把它们交叉编译成 linux/amd64
测试二进制丢到 22.04 上跑（24 / 77 / 41 个用例），第一条就红了：`TestProbeLocksNeverRemoves`
的断言与实现里写明的语义（unix 上 `ENOENT` 算"空闲"，不是"找不到"）冲突。按平台拆开：
unix 侧断言"缺文件 → 空闲且不报错"，非 unix 侧断言"不支持"；"读不到"那条用超长路径造出来
（root 也能触发），这样验收脚本里 `APT_LOCK_UNREADABLE` 的 NOT RUN 说明才站得住。

**四、前端把 38.7 秒的再模拟当成 20 秒就该回来的请求。** `submitJob` 里写死 20 秒超时，
写操作对话框的轮询上限是 120 次（约 2 分钟）。慢机器上光提交时的再模拟就要 38.7 秒，
`package.install` 真装 cowsay + sl 跑了 6 分半——于是页面会在任务还在正常跑的时候宣布
"提交超时"和"等太久了"。修法：`submitTimeoutFor(action)` 让包类动作走 95 秒，
`jobPollLimitFor(action)` 让轮询窗口是 21 分钟（比代理给 apt 的 20 分钟写超时多一分钟）。

**五、验收脚本的"300 秒"其实是 400 多秒。** `wait_job` 数的是循环次数不是秒，`sleep 0.5`
加轮询开销，一节就漂出几分钟；而它又比代理先放弃。改成按墙钟算（`date +%s` 截止），
写类预算 21 分钟，并且不比代理先放弃。

**六、夹具被上一轮吃掉了——"恰好有个可升级的包"不是夹具，是运气。** 第一次完整跑完，
22.04 上冒出一条 `env-has-upgradable-package`：这台机器没有任何可升级的包。因为上一次验收
把它升到了最新。24.04 那边更糟：`apt-get -s upgrade | head -1` 挑中的 `libglib2.0-data`
真升一次要跑十几分钟，把"只升点名的包"埋进噪声里。改成脚本自己造两个 8 KB 的包（a 与 b），
各两个版本，索引按 `.deb` 的实际字节算（24.04 上没有 `dpkg-scanpackages`，也不为此装一套
工具链），`file:` 源 + `[trusted=yes]`：装 1.0、候选停在 2.0、**只升 a**。差集里只该有 a，
而 b 就摆在旁边一动不动——这是 `--only-upgrade` 与"顺手把能升的都升了"的分水岭。
收尾时包、源、仓库目录一起删掉，下一轮还是从 1.0 开始。

顺带三个自己踩出来的坑，都记在脚本注释里：`DEBIAN/conffile` 少了个 s（dpkg 读的是
`conffiles`，写成单数就被当成普通文件、升级时直接覆盖）；`grep -c` 返回的是数字不是 `yes`；
以及启动器把 `$?` 写成了字面量 `\0`，于是日志末尾那个 `exit=0` 是假的——退出码现在由
`/root/run-p11-2.sh` 捕获，FAIL 一定伴随 `exit=1`。

## 已知限制（本轮没解决的，写在这里）

1. **`APT_LOCK_UNREADABLE` 仍然只能在单测里验。** root 拿 dpkg 的锁文件不会被权限拦
   （`CAP_DAC_OVERRIDE`），而把锁文件挪走造 EACCES 会破坏验收机的包管理器。脚本里那条
   NOT RUN 说的是实话。
2. **"读不到 dpkg" 也没有真机反例。** 让 `dpkg-query` 失败要动 `/var/lib/dpkg`，
   同样只走了单测。
3. **刷新索引的"网络真的断了"没验。** 负例用的是一个不可达的源（`file:` 路径不存在），
   网络层中断不在本轮。
4. **升级只验了单包与包内 conffile**，没有验"一次升多个包"与"依赖被连带升级"的差集语义
   （连带删除那一侧验的是 `remove`）。
5. **重启执行与 `boot_id` 前后确认是 P11.3**，本轮不涉及。
6. **浏览器渲染没跑。** 包写动作的页面（`UpdatesView` 的四个按钮 + `StorageActionDialog`）
   只有类型检查与逻辑单测，没有在浏览器里点过；VM 上没有浏览器，载荷抓在 `payloads/`。
7. **`StorageActionDialog` 没有组件测试。** 它被存储与软件包两处共用，超时/轮询的改动由
   `web/src/api/preflights.test.ts` 覆盖到函数一级，组件本身只经过类型检查。
8. **Windows / macOS 宿主不在 0.1 范围**，这两台上的行为未验证。

## 下一子任务

**P11.3**（STAGES §13 第 3 条）：重启与关机的预检、再认证、倒计时与连接状态，重启后
`boot_id` 前后确认。写路径的形状已经在 P07.3/P09.3 立好，本轮又多了一条"不可取消"的先例；
新东西是**操作会把自己杀掉**——倒计时怎么对外说、连接怎么优雅地断、重启回来之后怎么
证明"这就是同一台机器重启了"而不是"换了一台"。
