# P16 补：全量竞态检测（`go test -race`）的真机读数（2026-10-28）

## 0. 这条补的是什么洞

账本里从 P03 起就有一条一直空着的"未验证"：**`go test -race`**。它在 P04.3 那轮有过一次读数
（22.04 VM 上装 gcc 与 Go，`-race` 跑 19 个包，0 条 `DATA RACE`），但那之后代码量涨了三倍，
包数从 19 涨到 54，**再没有在带 cgo 的环境里跑过一次**。所以这一轮把它补上。

顺带说清一件容易混的事：**`-race` 证明的东西很窄**——它只证明"这次执行路径上没触发竞态检测器"。
它不证明没有竞态，不证明逻辑对，也不代替真机功能验收。

## 1. 为什么在 VM 里跑，不在宿主

宿主是 Windows：`go env CGO_ENABLED` = `0`、`command -v gcc` 什么都没有。`-race` 要 cgo，
宿主上根本起不来（P03 的记录里就写着 `-race requires cgo`）。就算在 Windows 上装个 mingw 跑起来，
跑的也只是 `windows` 构建标签下的代码，而产品发出去的是 linux 二进制——真正要查竞态的
`*_linux.go` 在那里根本不会被编译进去。

所以：**源码送到验收 VM 里，在 linux 上编、在 linux 上跑。**

## 2. 源码与交付字节的一致性

Q510 的规矩是"读数只对它自己被跑的那份字节有效"。这一次的读数属于**源码**，不属于某个 tar.gz，
所以要把"这份源码和交付包是不是同一份"讲清楚：

- 源码取自工作树（`8e52b36-dirty`）。**注意：工作树里有 224 个未跟踪的 `.go`**，所以
  `git ls-files | tar` 这条路是错的（第一版就是这么打出来的，连 `tests/fixtures/` 都漏了，
  见第 4 节的"乙"轮）。最终打包命令是可复现的显式目录清单：
  `tar czf /tmp/upanel-src-v4.tar.gz go.mod go.sum api cmd internal migrations deploy scripts tests`
  → 1628 条、750 个 `.go`、542 条 `tests/fixtures/`，sha256 `9fcc148eb0450c3b87dfa58d00d0280716c001eaeb94327ed65304a8d529586e`，4.1 MB。
  （不含 `web/`、`dist/`、`docs/`、`.git`；Go 测试用不到它们，`web/` 在前一轮能正常编译也印证了这点。）
- 交付包 `3f3b40c1…daf0` 的 `build_date` 是 `2026-10-28T05:23:36Z`（宿主本地 13:23:36）。
- 构建之后**只有一处 `.go` 被改过，就是本轮这个测试文件**，可复核：
  `find cmd internal api migrations -name '*.go' -newermt '2026-10-28 13:23:36'`
  → 只有 `18:37 internal/agent/scriptaction_test.go`。它是 `_test.go`，不进产品二进制；
  改它的原因是第一轮 `-race` 在这里报了一条真实的数据竞争（见第 5 节）。

结论：**产品代码与交付包是同一份**；本轮唯一的产品侧字节变化为零，测试侧只动了这一个文件。
读数仍然只写在源码名下，不冒充"对 tar.gz 的验收"。

## 3. 环境

```
宿主        Windows + Git Bash；go1.27.3 windows/amd64；CGO_ENABLED=0；无 C 编译器
客机        Ubuntu 24.04.5 LTS / 6.8.0-147-generic / systemd 255 (255.4-1ubuntu8.18) / 2 vCPU / 3915 MB
虚拟化      QEMU -accel tcg,thread=multi -smp 2 -m 4096（无 KVM/WHPX，纯软件模拟）
C 工具链    gcc (Ubuntu 13.3.0-6ubuntu2~24.04.1) 13.3.0，apt 装的
Go          go1.27.3 linux/amd64（go.dev 官方 tarball，解到 /usr/local/go）
cgo         CGO_ENABLED=1，CC=gcc（`go env` 实测，见 `facts-env.txt`）
源码        工作树 tarball（`9fcc148e…586e`）→ /tmp/upanel-src（54 个包，51 个带测试）
```

`facts-env.txt` 是这一节的原始输出。

## 4. 怎么跑的（三轮，前两轮作废，理由都记着）

**甲（作废）：以 `upanel` 普通用户跑。** 输出 `race-run1-user.log`。
跑到第 12 个包时停手，因为红的形状不对：`internal/agentstore`、`internal/agent`、
`cmd/upanel-agent`、`cmd/upanelctl`、`internal/api` 里 **307 处**都是同一句

```
打开代理数据库失败: agentstore: 代理数据目录不可用:
目录 /tmp/TestXxx123/001 的属主必须是 root，实际为 1000
```

这不是竞态，是**产品自己的安全校验**：代理数据目录必须归 root。所以 `-race` 必须以 root 跑
（P04.3 / P04.3-H2 那两次也是 root）。这一轮不算读数，但留了日志——它证明"普通用户跑这条命令
会得到一片假红"，也顺手抓到了下面那条真实的数据竞争。

**乙（作废）：改 root 重跑，但源码包打错了。** 用 `git ls-files` 打包，漏掉未跟踪文件，
`storagequery_test.go:45` 报"定位夹具目录 storage-2404 失败"。作废，重新按目录清单打包（见第 2 节）。

**丙（正式读数）：root + 完整源码 + 已修好的测试。** 脚本全文见 `race-run2-root.sh`：

```sh
# 客机里，root
cd /tmp/upanel-src
export PATH=/usr/local/go/bin:$PATH CGO_ENABLED=1
export GOMODCACHE=/tmp/gomodcache GOCACHE=/tmp/gocache
go test -race -count=1 -timeout=90m ./...      # → /tmp/race-root.body.log
```

两处开关的理由：

- `-count=1`：默认的测试结果缓存会让"这一轮到底跑没跑"说不清。
- `-timeout=90m`：`go test` **每包默认 10 分钟**。P04.3 在 KVM 上 `-race` 跑 `internal/api`
  已经用了 273 秒，这台是 2 个 TCG vCPU（慢一个量级），不放大就必然撞成
  `panic: test timed out`——那是环境的读数上限，不是竞态。

## 5. 全量那一轮的读数

**跑的是**：`/tmp/upanel-src`（`9fcc148e…586e`，全量轮次用的那份），root，`-race -count=1 -timeout=90m ./...`。
**起止**：`2026-10-28T10:45:24Z` → `2026-10-28T14:42:23Z`，墙钟 **3 小时 57 分**，退出码 **1**。
原始输出 `race-full.body.log`，外壳输出 `race-full.wrapper.log`。

| 项 | 数 |
|---|---|
| 包总数（`go list ./...`） | 54 |
| 带测试文件的包 | 51 |
| 没有测试文件的包 | 3（`internal/testsupport`、`migrations`、`migrations/agentdb`） |
| `ok` | **44** |
| `FAIL` | **7** |
| `WARNING: DATA RACE` | **1** |
| 撞 `-timeout=90m` 的包 | **3** |

### 5.1 逐包（时间是该包在插桩下的用时，不是产品延迟）

| 包 | 结果 | 用时 |
|---|---|---|
| `upanel/cmd/upanel-agent` | ok | 8.971s |
| `upanel/cmd/upanel-web` | ok | 43.536s |
| `upanel/cmd/upanelctl` | **FAIL** | 1037.218s |
| `upanel/internal/accounts` | ok | 3.973s |
| `upanel/internal/agent` | **FAIL（超时）** | 5403.041s |
| `upanel/internal/agentclient` | ok | 11.127s |
| `upanel/internal/agentstore` | ok | 2473.769s |
| `upanel/internal/alerts` | ok | 129.884s |
| `upanel/internal/api` | **FAIL（超时）** | 5406.840s |
| `upanel/internal/apphealth` | ok | 94.955s |
| `upanel/internal/appruntime` | ok | 7.587s |
| `upanel/internal/apps` | ok | 435.389s |
| `upanel/internal/apt` | ok | 5.432s |
| `upanel/internal/audit` | ok | 168.136s |
| `upanel/internal/auth` | **FAIL（超时）** | 5404.532s |
| `upanel/internal/backup` | **FAIL** | 35.676s |
| `upanel/internal/certificates` | ok | 218.908s |
| `upanel/internal/config` | ok | 6.095s |
| `upanel/internal/containers` | ok | 5.708s |
| `upanel/internal/debversion` | ok | 2.274s |
| `upanel/internal/deploy` | ok | 29.164s |
| `upanel/internal/discovery` | ok | 6.656s |
| `upanel/internal/files` | ok | 24.100s |
| `upanel/internal/hostcaps` | ok | 2.530s |
| `upanel/internal/httpx` | ok | 3.194s |
| `upanel/internal/journal` | **FAIL** | 152.191s |
| `upanel/internal/logging` | ok | 3.674s |
| `upanel/internal/logs` | ok | 9.146s |
| `upanel/internal/metrics` | ok | 265.431s |
| `upanel/internal/monitoring` | ok | 6.167s |
| `upanel/internal/network` | ok | 13.746s |
| `upanel/internal/nginx` | ok | 3.569s |
| `upanel/internal/notifications` | ok | 369.627s |
| `upanel/internal/operations` | ok | 1518.821s |
| `upanel/internal/packages` | ok | 4.337s |
| `upanel/internal/platform` | ok | 1.959s |
| `upanel/internal/power` | ok | 2.456s |
| `upanel/internal/processes` | ok | 5.124s |
| `upanel/internal/proxies` | ok | 509.147s |
| `upanel/internal/schedules` | ok | 4.399s |
| `upanel/internal/scripts` | ok | 3.031s |
| `upanel/internal/services` | ok | 7.869s |
| `upanel/internal/shares` | ok | 19.379s |
| `upanel/internal/storage` | **FAIL** | 109.773s |
| `upanel/internal/store` | ok | 346.005s |
| `upanel/internal/syscmd` | ok | 22.749s |
| `upanel/internal/terminal` | ok | 27.829s |
| `upanel/internal/tlscert` | ok | 5.079s |
| `upanel/internal/totp` | ok | 2.747s |
| `upanel/internal/version` | ok | 2.751s |
| `upanel/internal/webui` | ok | 2.211s |
| `upanel/internal/testsupport` | 无测试文件 | — |
| `upanel/migrations` | 无测试文件 | — |
| `upanel/migrations/agentdb` | 无测试文件 | — |

### 5.2 那 7 条红，逐条定性（没有一条是产品缺陷）

| 包 | 红的样子 | 定性 |
|---|---|---|
| `internal/journal` | `WARNING: DATA RACE`：`TestStreamClientClosedStops` 在测试自己的协程里轮询 `streamer.stream`，而 `fakeStreamer.Start` 在 `Stream` 的协程里写它 | **测试侧的数据竞争**，真货，已修（加锁的 `started()`）；报告与改法见 `race-fix-notes.txt` |
| `cmd/upanelctl` | `TestCheckConfigPrintsTLSFingerprint` 断言人类可读输出里有允许源，而产品从不打印 | **测试侧缺陷**（Q517），已修 |
| `internal/backup` | ① `TestResticRecoveryArgumentsAreFixedAndLiteral`：`复制恢复目录句柄失败: bad file descriptor`<br>② `TestDialRetriesOnceWhenHandshakeStalls`：第一轮卡住、第二轮也没连上 | ① **测试侧缺陷**（Q518）：硬编码 `/proc/self/fd/8`，Windows 上 `dupRestoreFD` 是桩所以从没暴露，Linux 上必然 EBADF；已修<br>② 判据实验见 §6 |
| `internal/storage` | `TestOSCapacityReaderNetworkTimeoutQuarantines`：`隔离期内还等了 9.207787ms`（上界 5ms） | **墙钟上界被插桩+TCG 的调度延迟打穿**。功能三条全过：没派新子进程、错误是 `ErrCapacityTimeout`、报文写"不再重试"。判据实验见 §6 |
| `internal/agent` | 撞 `-timeout=90m`（5403s） | **环境上限，不是死锁**：panic 栈显示超时那一刻在跑 `TestFileActionsRejectUnknownFields`（该用例已跑了 50 秒，正常推进）。补跑见 §6 |
| `internal/api` | 撞 `-timeout=90m`（5407s） | 同上：超时那一刻在跑 `TestAppLogsSystemdRejectsContainerParams`（已跑 3 分 29 秒） |
| `internal/auth` | 撞 `-timeout=90m`（5405s） | 同上：超时那一刻在跑 `TestLoginIssueAndAuthenticate`；根因是 `-race` 把 Argon2id 单次校验放大到 **1 分 31 秒**（用例自己的输出：`单次 Argon2id 校验耗时 1m30.976442106s`），而登录类用例每条都要校验一次 |

那唯一一条 `WARNING: DATA RACE` 的完整报告在 `race-full.body.log`，摘要：

```
Read at 0x00c0000984f0 by goroutine 50:
  upanel/internal/journal.TestStreamClientClosedStops()
      /tmp/upanel-src/internal/journal/journal_test.go:689
Previous write at 0x00c0000984f0 by goroutine 51:
  upanel/internal/journal.(*fakeStreamer).Start()
      /tmp/upanel-src/internal/journal/journal_test.go:92
```

### 5.3 这一轮**不能**拿来当"没有竞态"的证据

51 个带测试的包里，**44 个在插桩下跑完了、0 条 `DATA RACE`**；但 `internal/agent`、`internal/api`、
`internal/auth` 三个包**根本没跑完**（撞 90 分钟上限）。这三个包覆盖的正是代理动作面、HTTP
接口层与会话/口令——恰恰是最该查并发的三块。所以本轮能说的话只有一句：
**跑完的 44 个包里没有触发竞态检测器**；那三个包的结论要等 §6 的补跑。

## 6. 补跑与判据实验

全量那一轮留了两个欠账：三个大包撞在 `-timeout=90m` 上没跑完；四条红是测试自身的问题。
补跑分四轮把它们结掉。源码树是 `/tmp/upanel-src-fix`（交付字节那一份 + 四处 `_test.go` 修正，
tarball `d1eb81d2…5e6d`；产品 `.go` 一个字节没动，见 §2）。原始输出 `rerun.log`、`race-re-*.body.log`、
`verify3.log`、`verify4.log`，数字汇总在 `facts-rerun.txt`。

### 6.1 补跑的读数

**起止** `2026-10-28T14:44:46Z` → `23:47:55Z`，墙钟 **9 小时 03 分 09 秒**。启动方式
`sudo -n setsid nohup sh /tmp/race-rerun.sh`，`-race -count=1 -timeout=0`，root 身份。
agent/api/auth 三包**并行**（各占一条 vCPU 的量），其余七个包串行。

| 包 | 结果 | 用时 | FAIL 用例 | `WARNING: DATA RACE` |
|---|---|---|---|---|
| `internal/api` | **ok** | **28929.178s**（8 h 02 m）| 0 | **0** |
| `internal/auth` | FAIL | 11767.531s（3 h 16 m）| 1 | 0 |
| `internal/agent` | FAIL | 16579.372s（4 h 36 m）| 2 | 0 |
| `cmd/upanelctl` | **ok** | 310.760s | 0 | 0 |
| `internal/journal` | **ok** | 27.336s | 0 | 0 |
| `internal/backup` | **ok** | 11.673s | 0 | 0 |
| `internal/storage` | **ok** | 17.025s | 0 | 0 |

`internal/api` 8 小时零 2 分是本轮最硬的数字：399 条测试逐个在插桩下跑，**0 条竞态、0 条失败**。
全量那一轮它连一半都没跑完（5407s 撞上限），所以这个包的并发结论**只有这一份读数**。

中途做了一件事，写在 `idle-panel-cpu.txt`：15:23:35Z 停掉了常驻的 `upanel-web` / `upanel-agent`
（空闲状态在吃 0.25—0.46 个核）。**这不改变任何 pass/fail，只影响墙钟**——少一份负载只会更容易过。

### 6.2 全量那一轮 7 条红的最终处置

| 包 | 红的样子 | 定性 | 处置与依据 |
|---|---|---|---|
| `cmd/upanelctl` | 断言人类可读输出里有允许源 | 测试缺陷（Q517） | 已修 → 补跑 **ok 310.760s** |
| `internal/journal` | `DATA RACE`（测试自己的轮询） | 测试侧真竞争（Q519②） | 已修 → 补跑 **ok 27.336s** |
| `internal/backup` ① | `复制恢复目录句柄失败: bad file descriptor` | 测试缺陷（Q518） | 已修 → 补跑 **ok 11.673s** |
| `internal/backup` ② | SSH 重试第二轮没连上 | 待定 | §6.4 判据实验：带 / 不带 `-race` **都过** |
| `internal/storage` | `隔离期内还等了 9.207787ms`（上界 5ms） | 待定 | §6.3 判据实验：10 次隔离重跑 **全过** |
| `internal/agent` | 撞 90 分钟 | 环境上限 | 补跑换成 `-timeout=0`，跑完 4h36m，红了**另外两条**，见 §6.5 |
| `internal/api` | 撞 90 分钟 | 环境上限 | 补跑 **ok 28929.178s**，0 竞态 |
| `internal/auth` | 撞 90 分钟 | 环境上限 | 补跑跑完 3h16m，红 1 条（计时用例），见 §6.6 |

**四条红里三条是测试自身的缺陷，已修；另外两条（backup ②、storage）经过判据实验都不复现，
上界一个没动。**

### 6.3 判据实验一：`internal/storage` 的 5 毫秒墙钟上界

`TestOSCapacityReaderNetworkTimeoutQuarantines` 断言"隔离期内不许再等超过 5 毫秒"。全量那一轮
它在三包并行的机器上量到 9.207787 ms。**不改上界**，改为在同一台机器上连跑 5 次、带与不带 `-race` 各一组：

| 组 | 次数 | 结果 | 单次耗时 | 包用时 |
|---|---|---|---|---|
| `-race -count=5` | 5 | **5/5 PASS** | 0.03 / 0.02 / 0.02 / 0.02 / 0.02 s | 1.689s |
| `-count=5`（无检测器） | 5 | **5/5 PASS** | 0.03 / 0.02 / 0.02 / 0.02 / 0.02 s | 0.352s |

**10 次里一次都没复现**。结论：那 9.2 ms 是当时机器被三条重包挤出来的调度延迟，不是产品在隔离期内又等了。
上界保留 5 ms。

### 6.4 判据实验二：`internal/backup` 的 600 毫秒握手预算

`TestDialRetriesOnceWhenHandshakeStalls` 断言"第一次握手卡住之后，第二轮必须在 600 毫秒预算内连上"。

| 组 | 结果 | 用例用时 | 包用时 |
|---|---|---|---|
| `-race` | **PASS** | 1.25s | 2.750s |
| 无 `-race` | **PASS** | 0.98s | 1.196s |

带检测器也过。全量那一轮的红同样是并行争抢的产物，预算不动。

### 6.5 判据实验三：`TestDefaultPasswordParamsTiming` 的红不是检测器造成的

这一条我原来记错了方向。原判断是"`-race` 把 Argon2id 放大到 1.79—2.03 秒，所以撞 1 秒上界"。
补跑最后单独跑了一次**不带 `-race`** 的对照：

```
=== RUN   TestDefaultPasswordParamsTiming
    password_test.go:173: 单次 Argon2id 校验耗时 3.061838022s
    password_test.go:178: 校验耗时 3.061838022s 过长，需重新基准
--- FAIL: TestDefaultPasswordParamsTiming (6.07s)
```

**不带检测器也红。** 四组读数摆在一起：

| 机器 | 带 `-race` | 不带 `-race` | 判据（上界 1 秒）|
|---|---|---|---|
| KVM 22.04（P04.3） | 1.79—2.03 s → 红 | **0.6 s → 过** | 真机口径 |
| TCG 24.04（本轮） | 54.26 s → 红 | **3.06 s → 红** | 模拟机慢一个数量级 |

所以这条量的是**机器速度**，不是参数变重，也不只是检测器开销。产品的 Argon2id 参数一个没动，
契约区间仍是真机口径的 100—300 ms。**上界 1 秒不改**——它是"参数别被偷偷调重"的守卫，
为了让模拟机变绿而放宽它，就是把守卫拆了。

### 6.6 修好之后的确认，与两条"新面孔"红

三个测试缺陷（Q517/Q518/Q519）落地后，那四个包在插桩下**全绿**（见 §6.1 表）。
`internal/agent` 跑完之后露出来两条新的红，两条都不是全量那一轮见过的：

| 用例 | 红的样子 | 为什么不是产品缺陷 |
|---|---|---|
| `TestWebhookSelfSignedCertIsRejected` | 期望"自签证书 → 不可重试"，实得 `TLS handshake timeout`（被判可重试） | 服务端是测试自己在**本机**起的 `httptest.NewTLSServer`，握手本该毫秒级；拿到握手超时只可能是服务端的 goroutine 没被调度到 |
| `TestOperationExpiredPreflightRejected` | 期望 409 `PREFLIGHT_EXPIRED`，实得 201 | 见下 |

第二条要展开说，因为它看起来像"产品接受了过期预检"。读代码不是：`internal/agent/operations.go`
里 `now` 是**在读库之前**采样的——

```go
now := s.nowUTC().Truncate(time.Second)          // 171  先采样
plan, err := s.opts.Plans.Get(r.Context(), …)    // 175  后读库
```

而测试夹具是在 `Get` 里按**读的那一刻**造过期：`p.ExpiresAt = time.Now().UTC().Add(-time.Second)`。
于是只要这次 `Get` 的耗时超过 1 秒，`Get 时刻 − 1 秒`就落到 `now` 之后，夹具想造的"已过期"
反而变成"还没到期"。

产品侧确实留了一个可记的语义细节：`now` 采样在读库之前，所以"过期"判定的窗口会被这次读的
延迟撑长（真机上是微秒，这里是秒）。但那是读数延迟，不是"接受过期预检"。

**隔离重跑（第四轮，`verify3.log`）把这两条单独跑，各 `-race -count=3`：**

```
webhook-isolated   PASS 1.63s / 0.13s / 0.14s     （服务端日志：remote error: tls: bad certificate —— 正是该有的样子）
expired-isolated   PASS 16.74s / 15.53s / 14.34s
```

**两条都是 3/3 全过。** 隔离条件下 `expired` 那条本身要跑 15 秒（建库、迁移），所以它不便宜；
全量补跑里它在三包并行下变成 32.25 秒 —— 慢了一倍，`Get` 也就跟着越过了 1 秒余量。

同一轮还做了一棵**探针树**（只把夹具那一行的 `-time.Second` 改成 `-time.Minute`，断言一个字不改）：
也是 3/3 过。**但这棵探针树什么也没证明**——未改的那份在隔离条件下同样全过，所以"1 秒余量太窄"
这个因根本没有被区分开。设计得不对，如实记在这里，不当证据用。

### 6.7 判据实验四：把"三包并行挤爆 2 条 vCPU"这个因人为造出来

隔离 3/3 全过只说明这两条**不常在**，不说明它们**为什么**红。第四轮的设计（换夹具余量）没起作用，
换成直接造负载：同样的源码、同样的命令、`-count=3`，旁边加 4 个忙循环打满 2 条 vCPU，
对照组就是第四轮 A 段（无负载，3/3 PASS）。

**第五轮（`verify4.log`）：4 个忙循环打满 2 条 vCPU，两条测试各 `-race -count=3`。**

```
loaded-webhook   run1  --- FAIL 6.75s   net/http: TLS handshake timeout   （服务端日志：handshake error …: EOF）
                 run2  --- PASS 0.45s   remote error: tls: bad certificate
                 run3  --- PASS 0.44s   同上
loaded-expired   3/3 PASS，单次 68.53s / 59.26s / 59.06s（无负载时 14—17s）
```

| 用例 | 无负载 | 有负载 | 判读 |
|---|---|---|---|
| `TestWebhookSelfSignedCertIsRejected` | 3/3 PASS（0.13—0.16 s）| **1/3 FAIL** | **复现了**。而且服务端自己记的是 `handshake error …: EOF`——不是证书被拒，是握手根本没走完。机制清楚：本机 TLS 握手本该毫秒级，机器被挤到服务端 goroutine 排不上队，产品的 5 秒握手预算先到，于是信封里装的是"超时（可重试）"而不是"证书（不可重试）" |
| `TestOperationExpiredPreflightRejected` | 3/3 PASS（14—17 s）| 3/3 PASS（59—68 s）| **没复现**，而且它是在比那 32 秒更慢的状况下过的 |

**所以第二条红到目前为止仍然没有定性。** 我原来那个"读库超过 1 秒"的解释被这组读数顶回来了：
68 秒都过，32 秒凭什么红。**不能拿一个不可复现的机理去结一条红**，§6.8 换了确定性实验。

### 6.8 判据实验五：把"采样 `now` 与读库之间被拖住 1.5 秒"焊进探针

不去赌偶发。只改测试文件里的 `mutatingPlanStore.Get`，在它读 `inner` 之前先睡 1500 毫秒，
精确制造 `now`（`operations.go:171`）与夹具改写（`Get` 内部）之间的 1.5 秒间隔。
判据**先写下来再跑**：

| 组 | 夹具余量 | 延迟 | 预期 |
|---|---|---|---|
| A | `-time.Second`（1 秒）| 1.5 秒 | **必须红，且红成 201** |
| B | `-time.Minute`（60 秒）| 1.5 秒 | **必须绿** |

<!-- 第六轮读数待填 -->

**第六轮（`verify5.log`）：两组都按预期走。**

```
A 组（1 秒余量 + 1.5 秒延迟）
  operations_test.go:459: 状态码 = 201，期望 409；响应 {…"preflight_ref":"pf_0abfe963…","stage":"accepted"…}
  --- FAIL: TestOperationExpiredPreflightRejected (18.00s)

B 组（60 秒余量 + 1.5 秒延迟）
  --- PASS: TestOperationExpiredPreflightRejected (16.78s)
```

**机理成立**：夹具的余量只要小于"采样 `now` 到读库"之间的停顿，那条用例就在测调度而不是在测判定；
把余量放到 60 秒，同样的 1.5 秒延迟就翻不动它。

### 6.9 处置：改夹具余量，断言一个字不动（Q520）

`internal/agent/operations_test.go` 里那一行从 `Add(-time.Second)` 改成 `Add(-time.Minute)`，
原地写了为什么（读库前的 `now`、1 秒余量在慢机器上会翻、判据实验的读数在哪）。
**断言仍然是 409 + `PREFLIGHT_EXPIRED`，一个字没改。** 改的是"夹具想造的那个状态到不到位"。

用第七轮确认（`verify6.log`）：把工作树重新打成干净的包（tarball `1fc5ec5b…f6599`）传进客机，
解到 `/tmp/upanel-src-final`，**root** 身份 `-race -count=3 -run '^TestOperationExpiredPreflightRejected$'`：

```
458:		p.ExpiresAt = time.Now().UTC().Add(-time.Minute)     ← 夹具那一行
（产品 .go 里不含这条改动：grep -rn "Add(-time.Minute)" --include=*.go | grep -v _test.go → 空）

--- PASS: TestOperationExpiredPreflightRejected (15.78s)
--- PASS: TestOperationExpiredPreflightRejected (14.27s)
--- PASS: TestOperationExpiredPreflightRejected (13.79s)
PASS
ok  	upanel/internal/agent	45.581s
```

**一条弯路也记下来**：第七轮我第一次是用 `upanel` 身份跑的，三条全红在
`代理数据目录不可用: … 属主必须是 root，实际为 1000` 上——正是 §4 甲轮那 307 条假红的同一个来源。
**"必须 root"不是建议，是这份套件的前置条件**；这次是我自己又踩了一遍。

### 6.10 这一轮一共动了什么

| 文件 | 事由 | 产品字节 |
|---|---|---|
| `cmd/upanelctl/main_test.go` | Q517：删掉一条产品从不满足的断言（同义的 JSON 断言仍在，且从此第一次在 Linux 上被执行） | 零变化 |
| `internal/backup/retention_test.go` | Q518：硬编码 `/proc/self/fd/8` → 真开一个目录取 fd | 零变化 |
| `internal/agent/scriptaction_test.go` | Q519①：轮询加锁 | 零变化 |
| `internal/journal/journal_test.go` | Q519②：轮询加锁 | 零变化 |
| `internal/agent/operations_test.go` | Q520：夹具余量 1 秒 → 60 秒 | 零变化 |

`find cmd internal api migrations -name '*.go' -newermt '2026-10-28 13:23:36'`（13:23:36 = 交付包的构建时刻）
只命中这五个 `_test.go`，**非 `_test.go` 的改动 = 0 个**。所以这五个改动都不进交付包里的两个二进制。

**注意 `/tmp/upanel-src-fix` 已经是脏的**：第六轮往它的 `operations_test.go` 里插了 1.5 秒延迟探针，
它此后不再是"干净的那份补跑树"。所有归档读数都在插入之前取的；要复现请用 `race-rerun.sh` 从头解包，
或者用 tarball `1fc5ec5b…f6599` 那棵（`/tmp/upanel-src-final`）。

### 6.11 收尾：宿主 `scripts/check.sh` 三次读数（第一次 13/2，后两次 15/0）

`-race` 这一轮改动的五个 `_test.go` 最终要落回仓库，落回去之后宿主的全量检查必须再跑一遍。
第一次跑出来是 **PASS 13 / FAIL 2 / NOT RUN 0**，红的是 `node-version` 和 `openapi-lint`。
三次读数都归档在 `facts-host-check.txt`，包括失败那一次的原文。

| | PATH 首位 node | node-version | openapi-lint | 汇总 |
|---|---|---|---|---|
| 第一次 | `C:\Program Files\nodejs\node.exe` v24.15.0 | FAIL | FAIL（exit 127） | 13 / 2 / 0 |
| 第二次 | `C:\Users\huxi\tools\node-v22.23.3-win-x64` v22.23.3 | PASS | PASS | **15 / 0 / 0** |
| 第三次 | `/h/Code/.toolchains/node-v22.23.3-win-x64` v22.23.3 | PASS | PASS | **15 / 0 / 0** |

第三次是补的：第二次用的是一个复查时才发现的**另一份拷贝**（`C:\Users\huxi\tools\`）。仓库外约定的
固定工具链目录其实是 `/h/Code/.toolchains/`（即 `C:\…\Code\.toolchains`，**仓库的上一级**，
与 `docs/evidence/P16/2026-10-28-close/README.md` 里记的那条路径一致），用它能跑出同样的 15/0，
所以归档以第三次为准。

两项都不在代码上，但**处理方式不同**，所以分开写：

- **`node-version` 是工具链没在 PATH 里，判断标准不变**。`.nvmrc` 和 `.node-version` 都写 `22.23.3`，
  而 PATH 首位是系统装的 v24.15.0。`scripts/check.sh:207` 拿 `"v$EXPECTED_NODE"` 和 `node -v` 做等值比较，
  不匹配就红——这个行为是对的，**没有为了让它变绿去放宽比较**。
  顺带纠一个我自己的错判：我原先以为固定版工具链在**仓库里**的 `.toolchains/`，于是在仓库根下
  `ls -d .toolchains/*` 什么都没有、`.gitignore` 里也没有这一条，一度以为它丢了。它其实在
  **仓库的上一级** `/h/Code/.toolchains/`（同目录还有 `qemu/`、两个 `.zip`），仓库这份工作树里确实没有。
  所以"按 .nvmrc 切工具链"这一步**不是 clone 下来自带的**，换机器得自己装一遍再前置到 PATH；
  也不该指望 `git status` 能提醒你它不在。
- **`openapi-lint` 是运行时把进程打死在退出路径上，不是 spec 不合格**。redocly 已经把
  `api/openapi.yaml` 判成 `Woohoo! Your API description is valid.`（6 个 warning，全是既有的
  `no-unused-components: 3` / `operation-4xx-response: 2` / `operation-2xx-response: 1`），
  然后 node 24 在退出时撞 `Assertion failed: !(handle->flags & UV_HANDLE_CLOSING), file src\win\async.c, line 76`，
  exit 127。换 node 22 之后同一条命令干净退出。**结论是运行时版本不对，spec 没动、也没为了绕开它去改 spec。**

第三次读数（以 `/h/Code/.toolchains/` 为准）的逐项：`line-endings / sh-syntax / go-build / go-vet / go-test / go-build-linux / go-vet-linux /
gofmt / node-version / web-typecheck / web-lint / web-test / web-build / web-smoke / openapi-lint` 全 PASS。

**顺序上要交代一句**：`docs/PROGRESS.md` 里"宿主检查 15/0/0"那句话是在这一节之前就写进账本的，
而实际跑出来的第一份读数是 13/2。也就是说账本曾经**比读数早**地写着通过——后来把工具链放对了
才补上 15/0 这一份。两份原文都留在这个目录里，先错后对的过程不删。

这里要讲清楚一件事：**宿主上这 15 项 PASS 不等于这一轮的 `-race` 结论被独立复现了**。宿主 `CGO_ENABLED=0`
且没有 C 编译器，`scripts/check.sh` 里的 `go-test` 是**不带** `-race` 的。它证明的是"这套改动没把宿主
上原有的检查搞坏"，仅此而已；竞态读数仍然只有 24.04 客机那一份。



## 7. 这组读数**没有**证明什么

- **不证明"没有竞态"**。竞态检测器只对**实际执行到的那条路径**说话。跑完的 51 个包没报，等于
  "这次执行顺序下没触发"，不等于"代码没有竞态"。真正的价值在反面：它把**两处测试轮询**的
  真实竞争抓出来了（Q519），产品代码零条。
- **两份源码树，别混着读**。全量那一轮的 44 个 `ok` 用的是**工作树原样**（`/tmp/upanel-src`）；
  补跑的 7 个包用的是**改了四处 `_test.go`** 的那份（`/tmp/upanel-src-fix`）。两棵树的产品 `.go`
  逐个相同（§2 有 `find -newermt` 的读数：非 `_test.go` 改动 = 0 个），但"51 个包 0 竞态"
  这个总数是跨两棵树拼出来的，说的时候要带上这个前提。
- **墙钟数字不是性能**。TCG 是纯软件模拟，`-race` 又把每行内存访问都插了桩。这些秒数只用来
  解释"为什么撞超时"和"为什么那条 1 秒上界过不去"，不能拿去当"产品慢不慢"的证据。
  同一条 Argon2id 校验：KVM 22.04 不带检测器 **0.6 秒**，这台 TCG 24.04 不带检测器 **3.06 秒**。
- **`internal/api` 的 8 小时只跑了一遍**。它是本轮最硬的数字（399 条测试、0 竞态、0 失败），
  但只有一次读数，没有第二次独立复现。
- **只覆盖 amd64**。arm64 的包能打、能起，但这一轮没有任何 arm64 上的 `-race` 读数。
- **只覆盖测试进程内的并发**。面板真实跑起来之后（gin 路由 + 代理的轮询协程 + 定时任务）
  的并发形态，测试进程模拟不到全部；那部分仍然只有真机上的功能验收与日志观察。
  这一条现在有了一个具体的旁证：常驻的 `upanel-web` 空闲时在吃 0.25—0.46 个核（`idle-panel-cpu.txt`），
  **测试进程里查不到这种行为**，只有把它当进程看才看得见。
- **宿主上仍然没有 `-race` 证据**，这一轮的读数全部来自 24.04 客机。


