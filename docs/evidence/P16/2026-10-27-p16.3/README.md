# P16.3 专项复核：认证 / 权限 / 路径 / 命令 / SSRF / 设备身份 / 配置变更（2026-10-27）

这一份是 STAGES.md §18 里 P16.3 的证据。它不是"全都看过、没问题"的结论书，而是
**每一项复核的判据 + 读数 + 哪些只是静态阅读、哪些在真机上按过**。

复核方式分三层，证据的强度不同，混在一起就等于说假话，所以先讲清楚：

| 层 | 做法 | 能证明什么 | 不能证明什么 |
|---|---|---|---|
| 静态阅读 | 逐条读代码并落 `file:line` | 这个形状存在/不存在 | 运行时一定按这条路径走 |
| 用例断言 | `go test ./internal/api` 等，含新加的路由守卫 | 这条不变量在测试环境成立且会因回归变红 | 真机上中间件顺序也一致 |
| 真机读数 | 22.04.5 上装着 P16.2 那份包，实测 HTTP / 文件权限 / 数据库 | 真机行为 | 只在单线程 TCG 的 22.04 上按过 |

## 1. 环境

| 项 | 值 |
|---|---|
| 客户机 | Ubuntu 22.04.5 LTS / kernel 5.15.0-198-generic |
| 包 | `upanel-0.1.0-dev.1-linux-amd64`（与 P16.2 同一份，`/usr/local/bin/upanel-web` mtime `2026-10-27 15:05:11Z`） |
| 服务 | `upanel-web`、`upanel-agent` 均 active；自 16:22:12Z 起 |
| 运行身份 | `upanel-web`（uid 1000 之外的专用账号）、`upanel-agent`（root） |
| 访问方式 | `https://127.0.0.1:9443`，同机 curl；本轮没有登录，只按匿名请求与文件权限 |

## 2. 空闲写放大：结案（P16.2 移交的第一件事）

### 问的是什么

P16.2 留下一条没结案的事：空闲 60 秒里 `/var/lib/upanel` 整目录长 313 KB（22.04）/
387 KB（24.04），加了目录清单后看清是 `upanel.db-wal` 在长，主库文件一页没变。
当时的猜测写的是"最像 30 秒一轮的对账循环"，**没有人抓到写入方**。P16.3 要回答三个问题：

1. 空闲时到底是谁在写、写的是哪张表；
2. WAL 会不会一直长（泄漏）还是到检查点就回落；
3. `PRAGMA wal_checkpoint` 之后会不会归零，以及这些写有没有必要。

### 探针

`tests/integration/p16-3-wal-probe.sh`（随本轮进仓库，原样留档在 `facts/`）。它做四件事：

- 用 `/proc/<pid>/fd` 找出**谁打开着** `upanel.db`（不靠猜）；
- 用 `python3` 的 `sqlite3` 以 **`mode=ro`** 连接读每张表的行数——探针自己不制造写入；
- 空闲 5 分半，每 30 秒记一次 主库 / `-wal` / `-shm` / 代理库 `-wal` / 表总行数；
- 唯一一次写：`PRAGMA wal_checkpoint(TRUNCATE)`，然后再记一次尺寸。

### 读数

**谁在写**：只有一个进程打开这本库——

```
pid=769 comm=upanel-web cmd=/usr/local/bin/upanel-web --config /etc/upanel/upanel.json
```

**写了哪张表**（空闲 5 分半，全部 25 张表逐张比对，只有一行不为零）：

```
metric_samples	+774	(913 → 1687)
```

其余 24 张表一行没变（`audit_events` 0、`jobs` 0、`sessions` 0、`alert_events` 0 …… 全零）。
代理那本库的 `agent.db-wal` 全程 **0 字节**——空闲时 `internal/operations` 的对账循环
**一帧都没写**，P16.2 里那个猜测是错的。

**这些写是什么**：`metric_samples` 里 10 个指标名，7 个主机级（`cpu_usage_percent`、
`memory_used_percent`、`load1/5/15`、`rx_bytes_per_sec`、`tx_bytes_per_sec`）按 **5 秒**一个点，
另外 3 个按挂载点分（`used_percent`、`inode_used_percent`、`available_bytes`，216 行 ÷ 3 = 72 个系列
摊在 13.8 分钟里）。来源是 `internal/metrics/recorder.go`：`DefaultInterval = 2s` 轮询代理，
代理侧 5 秒采集，写库按 `collected_at` 去重。也就是 P13.1 设计里那份"原始样本"，
不是 bug，不是对账循环，也不是有人在偷偷写。

| 时刻 | 主库 B | `upanel.db-wal` B | `upanel.db-shm` B | 代理库 `-wal` B | 表总行数 |
|---|---|---|---|---|---|
| 空闲开始 | 385024 | 2735712 | 32768 | 0 | 933 |
| +30 s | 385024 | 3110632 | 32768 | 0 | 993 |
| +60 s | 385024 | 3469072 | 32768 | 0 | 1085 |
| +90 s | 385024 | 3782192 | 32768 | 0 | 1145 |
| +120 s | 507904 | **4128272** | 32768 | 0 | 1230 |
| +150 s | 507904 | 4128272 | 32768 | 0 | 1290 |
| +180 s | 507904 | 4128272 | 32768 | 0 | 1357 |
| +210 s | 507904 | 4128272 | 32768 | 0 | 1417 |
| +240 s | 507904 | 4128272 | 32768 | 0 | 1502 |
| +270 s | 507904 | 4128272 | 32768 | 0 | 1562 |
| +300 s | 507904 | 4128272 | 32768 | 0 | 1647 |
| +330 s | 507904 | 4128272 | 32768 | 0 | 1707 |

**WAL 是锯齿，不是泄漏**：前 120 秒一直在长，到 **4128272 B** 之后**四分钟一动不动**——
4128272 = 32 + 1002 × 4120，正好撞上 `wal_autocheckpoint=1000` 的默认值。检查点把帧写回主库
（主库在这一步从 385024 长到 507904，即 30 页），WAL 文件本身不缩，但帧被复用，所以尺寸不再涨。
这就是 SQLite WAL 模式在持续写入下的稳态，不是我们的漏。

**主动检查点能归零**：

```
busy=0 log=0 checkpointed=0
journal_mode=wal
autocheckpoint=1000
```

`busy=0` 说明那一刻没有读写者挡住它，`log=0` 说明检查点后 WAL 里没有剩余帧。
`TRUNCATE` 之后文件被截到 0（`sizes.txt` 最后一行）。

### 结论与移交

- **不是泄漏**：稳态 ~4.1 MB 上限由 SQLite 的 1000 页自动检查点决定，与我们的保留策略无关；
- **写入方是 P13.1 的采集**，不是对账循环。P16.2 里那句"最像 `internal/operations` 的对账循环"
  已被这条读数推翻，`docs/PROGRESS.md` 的 P16.2 小节已就地标注；
- **这些写有必要**：它就是一个"历史监控"功能在按 5 秒粒度留原始点，PLAN.md 第 18 节的
  24h / 30d / 365d 保留窗口与 `rowLimit` 兜底都在（`internal/metrics/recorder.go`，
  `pruneInterval` 一分钟、`rowLimitInterval` 十分钟）。**本轮不改产品**；
- 折算成容量给 P16.4 的已知限制用：按这次读数 774 行 / 5.5 分钟、主库 122880 B / 774 行
  ——大约 **140 行/分钟、每天 20 万行量级、约 30 MB/天**的原始样本，被 24h 原始保留窗口截住。

## 3. 认证与权限：新加一道"整张路由表"的守卫

### 为什么加

各域的用例一直有（`users_test` 的 SEC-01/SEC-02、`shares_test`、`storagevolumes_test` ……），
它们证明的是"这条路由的权限对"。**没有**任何东西证明"**下一条**新加的路由也带了中间件"——
而这类回归的后果不是 403 而是 200，属于最不该靠人眼守的那一类。

`internal/api/router_guard_test.go` 补的是这一层，三条断言：

| 用例 | 断言 | 覆盖范围 |
|---|---|---|
| `TestEveryRouteRejectsAnonymousWith401` | 除 3 条公开入口，匿名请求一律 **401 `UNAUTHENTICATED`** | 整张表，本轮 **127 条**（124 条据此断言） |
| `TestEveryWriteRouteRejectsMissingCSRF` | 带会话、不带 `X-CSRF-Token` 的写请求一律 **403 `CSRF_FAILED`** | 所有非 GET/HEAD/OPTIONS 路由 |
| `TestNoGenericExecutionRoute` | 路由名里不允许出现 `exec/shell/command/sudo/raw/eval/sql` | 整张表 |

公开入口只有三条，理由写在用例里：`GET|HEAD /api/v1/health/live`（安装脚本、systemd 要在登录前判活）、
`POST /api/v1/auth/login`（换会话的入口，本身没有会话）。**加一条就要在评审里说清理由**。

### 守卫会不会红：两次变异验证

一个不会红的守卫等于没有。本轮把中间件真的摘掉、跑、再恢复：

| 变异 | 结果 |
|---|---|
| 把 `/storage` 组的 `st.Use(s.requireSession())` 改成 `st.Use()` | **红**：`GET /api/v1/storage/volumes 未登录时返回 503，期望 401——这条路由没有挂 requireSession？`（响应体 `AGENT_UNAVAILABLE`，正是"没登录也能看到代理状态"的样子） |
| 把 `jobs.POST("", s.requireAdmin(), s.requireCSRF(), …)` 里的 `requireCSRF()` 摘掉 | **红**：`POST /api/v1/jobs 带会话但不带 CSRF 返回 400，期望 403——这条路由没有挂 requireCSRF？` |

恢复后重跑：`ok upanel/internal/api 2.754s`；整包 `go test ./internal/api -count=1` → **ok，167.9 s**。

### 真机抽样（不是 httptest）

同一台 22.04、同一份包，匿名 curl 抽 10 条（`facts/live-routes.txt`）：

| 请求 | 状态 | 响应 |
|---|---|---|
| `GET /health/live` | 200 | `{"data":{"status":"ok"}}` |
| `GET /auth/session`、`/users`、`/jobs`、`/files/roots`、`/terminal/sessions`、`/containers` | **401** | `UNAUTHENTICATED` |
| `POST /secrets`、`/operations/preflight` | **401** | `UNAUTHENTICATED` |
| `POST /auth/login`（空体、无 `X-UPanel-Request` 头） | 400 | `VALIDATION_ERROR`：登录还要求这个自定义头（登录 CSRF 的替身） |
| `GET /api/v1/nope` | **404** | JSON `NOT_FOUND`——没回落到 SPA 页面 |
| `POST /health/live`（路径对、方法不对） | **405** | `VALIDATION_ERROR` |

## 4. 已核对的加固面（静态阅读 + 真机读数）

这一节是"我确实读过/按过并认为成立"的清单，不是"应该没问题"。每行都带位置或读数，
方便下一个人反驳。

| 面 | 事实 | 位置 / 读数 |
|---|---|---|
| 面板不以 root 运行 | 单元 `User=upanel-web`；`getent group upanel-web` → gid 997，`upanel` 账号不在其中 | `deploy/systemd/upanel-web.service`；`facts/socket-boundary.txt` |
| Web 侧无提权材料 | `NoNewPrivileges=yes`、`ProtectSystem=strict`、`PrivateDevices=yes`、`RestrictSUIDSGID=yes`、`RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6`；**刻意不声明** `ConfigurationDirectory`（否则 systemd 会把 `/etc/upanel` 交给 Web 账号，它就能换掉自己的配置和面板私钥） | 同上；真机 `systemctl show` 读数在 `facts/http-and-unit.txt` |
| IPC 边界由文件权限兜底 | `/run/upanel/agent.sock` = `srw-rw---- root:upanel-web`；普通账号直连 → `[Errno 13] Permission denied`；`upanel-web` 直连 → 连上 | `facts/socket-boundary.txt` |
| 响应头 | CSP 每次响应一个 nonce（`script-src 'self' 'nonce-…'`、`object-src 'none'`、`frame-ancestors 'none'`、`base-uri 'none'`）、`Strict-Transport-Security: max-age=15552000`、`X-Frame-Options: DENY`、`X-Content-Type-Options: nosniff`、`Referrer-Policy: no-referrer`、`Cross-Origin-Opener-Policy: same-origin`、`Cache-Control: no-store` | `internal/webui/webui.go:245,275-278,286`；真机 `facts/http-and-unit.txt` |
| TLS 下限 | 面板、代理服务证书、告警邮件三处都钉在 TLS 1.2 | `internal/config/tls.go:28,106`、`internal/proxies/servedcert.go:49`、`internal/agent/notify.go:271` |
| 登录 CSRF | 登录要 `X-UPanel-Request` 头，缺了 400（真机实测） | 上表最后两行 |
| 同源守卫 | 带 `Origin: https://evil.example` 的 POST → **403 `CSRF_FAILED`（Origin 不在允许列表内）**；GET/HEAD 不查 Origin | `internal/api/auth.go:268-282`；真机读数 |
| 会话 Cookie | `HttpOnly` + `Secure` + `SameSite=Strict` + `Path=/`，登出既服务端撤销也清 Cookie；滑动续期时同步重发 Cookie | `internal/api/auth.go:303-326`、`auth.go:211-218` |
| 前端不留凭据 | CSRF 令牌只在内存（`web/src/api/auth.ts:4`、`stores/session.ts:105` 明说不写 localStorage）；整个 `web/src` 无 `localStorage` 存令牌 | `grep -rn localStorage web/src` 只命中注释 |
| SQL | 102 处 `Query/Exec/QueryRow`，**没有一处用 `Sprintf` 拼 SQL**；动态 `WHERE` 走 `placeholders(n)` 占位 + 参数绑定 | `internal/store/metricstore.go:376-392` |
| 单元名 | `^[A-Za-z0-9][A-Za-z0-9:_.@-]*\.service$`，开头必须字母数字 → argv 里不可能出现以 `-` 开头的"选项" | `internal/services/types.go:194-207` |
| 防火墙定义 | `ID` 收窄到 `^[a-z0-9][a-z0-9-]{0,31}$`；`action` 闭集；`port` 1—65535；`proto` 闭集；`from` 必须是 IPv4/CIDR 并被重写成规范形；拼命令行用 `strings.Fields(spec())`，元素只能来自上面这些规范值 | `internal/network/firewalltypes.go:151-215,255`、`firewallapply.go:1404,1421` |
| 终端 shell | `Shell` 只来自配置或 `/etc/passwd` 第 7 字段；必须绝对路径、拒绝 `nologin` 族、必须是可执行文件；home 必须存在、属该账号、其他人不可写；`Setsid+Setctty`、清空附加组（`NoSetGroups=false`）、环境变量走白名单 | `internal/terminal/manager_linux.go:90-130,256-280` |
| 秘密存储 | 只落密文，明文读取路径只在 `internal/agentstore` 内部；临时秘密有 TTL | `internal/agentstore/secrets.go:11-30` |
| 审计脱敏 | 明细按字段名过滤（`password/passwd/secret/token/hash/cookie/credential/private_key` → `[redacted]`），库里那一列叫 `redacted_details` | `internal/audit/audit.go:6,210-214,325-345` |
| 动作的再认证矩阵 | 抽查 `file.mkdir`/`file.write_text`/`file.copy` = 否，`file.chmod`/`file.chown` = 是，与 CONTRACTS.md 第 5 节的 R 列逐条对上；破坏性动作另要短语（`file.delete`、`backup.restore`、`package.remove`、`process.signal` 的 KILL、`network/firewall` 的 reconfigure、存储分区/格式化） | `internal/agent/filesaction.go:527-533,620-626,958-964,1178-1184,1273-1279,777`；`grep RequiresPhrase` 9 处 |
| 无万能执行面 | 全部外部命令只有 4 个 `exec.Command*` 入口（`internal/syscmd`、`internal/backup/exec.go`、`internal/terminal/manager_linux.go`），都是 `Program + Args[]`，**没有一处过 shell**；路由名扫描不允许 `exec/shell/command` | §3 的第三条用例 |

## 5. 七个维度的复核结论

每个维度给三样东西：**怎么查的**、**查到什么**、**剩下什么**。凡是"读过并认为成立"的，
位置写到行；凡是没做实机验证的，一律写 NOT RUN，不写"应该没问题"。

### 5.1 认证

| 查了什么 | 结论 | 依据 |
|---|---|---|
| 会话令牌能不能被前端脚本读到 | 不能：`HttpOnly` + `Secure` + `SameSite=Strict` | §4 表；`internal/api/auth.go:303-326` |
| 会话在服务端可撤销 | 可：会话行落库，登出删行，`expires_at` 与服务端校验决定有效 | `internal/auth/session.go`（登出 → `DELETE`）；真机读数只覆盖"匿名一律 401"（`facts/live-routes.txt`），**登出后重放 Cookie 未在真机上做** |
| 账号枚举 | 登录失败对外统一 `ErrInvalidCredentials`，不区分"用户不存在"与"密码错"；不存在时也 Hash 一个假口令拉平耗时 | `internal/auth/users.go:46-49`、`internal/auth/session.go:404` |
| 暴力破解 | 进程内固定窗口双维度限速：用户名 5 次 / 15 分钟，来源 IP 20 次 / 15 分钟，窗口到期自动恢复（不留永久锁定，PLAN.md 第 5 节）；再认证与登录共用同一个限速器 | `internal/auth/ratelimit.go:12-40,70`、`internal/auth/reauth.go:13` |
| 再认证凭据的作用域 | reauth 只换一个短时票据，票据绑动作/绑定会话，不能当会话用 | CONTRACTS.md 第 5 节 R 列；`internal/auth/reauth.go` |

残余：限速是**进程内**的（重启即清零、多进程不共享）。本产品是单实例 systemd 服务，
这是 PLAN.md 里明写的取舍，不是漏洞；真要跨重启累计需要落库，P16 不扩这个范围。

### 5.2 权限

- 三条公开入口以外的路由全部要求会话——**本轮用整张路由表的用例钉死**（§3），
  并且做了两次变异，证明这道守卫会红。
- 管理员面（用户/审计/设置/证书/网络/存储）走 `requireAdmin`；写请求一律要 CSRF 头；
  危险动作另要再认证 + 短语，矩阵与 CONTRACTS.md 第 5 节逐条对上（§4 表最后两行）。
- 跨用户边界：操作记录按 `user_id` 过滤，代理侧**不提供"列出全部操作"的通道**
  （`internal/agentstore/operations.go:243-259`）；匿名与 viewer 越权各有用例
  （`internal/api/users_test.go:163,171` 的 SEC-01/02）。
- 面板进程本身**不是 root**，也没有任何 sudo 参数（§4 表前三行）。

### 5.3 路径

- 文件管理器所有根内路径都过 `openat2`：`RESOLVE_BENEATH | RESOLVE_NO_MAGICLINKS`，
  并额外带 `O_NOFOLLOW`/`O_CLOEXEC`；`O_PATH` 打开符号链接后显式重判"这是链接"；
  并发 rename 引起的 `EAGAIN` 有有限重试。逃逸手段（绝对路径、`..`、魔法链接、
  末段符号链接）逐条在注释里对上实现：`internal/files/manager_linux.go:13-20,208-310`。
- 分享目录：逐段 `Lstat`，任一段是符号链接就拒；`..` 与绝对路径在定义校验期就挡掉：
  `internal/shares/definition.go:325-350`、`internal/shares/assess.go:319-395`。
- 备份临时目录/私钥走 tmpfs，密文落库（§4 表"秘密存储"行）。

残余：`openat2` 需要内核 ≥ 5.6（22.04 是 5.15，满足）；更老的内核上这条路径直接不可用，
代码里没有"降级到拼路径"的分支——这是有意的：宁可功能不可用，不要路径围栏静默失效。

### 5.4 命令

- 全仓外部命令入口只有四处 —— `internal/syscmd/syscmd.go:268`、`internal/syscmd/stream.go:52`、
  `internal/backup/exec.go:169`、`internal/terminal/manager_linux.go:256` —— 签名都是
  `Program + Args[]`，**没有一处把用户输入拼成 shell 命令行**。路由表里也没有
  `exec/shell/command` 这类名字（§3 第三条）。
- 会进 argv 的用户输入都先经过闭集或正则收敛：服务单元名（§4 表）、防火墙规则
  （id/action/proto/port/from 全部规范化后 `strings.Fields` 拼元素）、账号名
  （`internal/accounts/definition.go:187-221` 要求小写字母开头、只许 `[a-z0-9_-]`，
  因此 argv 里不可能出现以 `-` 开头的"选项"）。
- restic 的密码只走 `RESTIC_PASSWORD` 环境变量，私钥只走 `--private-key` 指向 tmpfs 文件；
  argv 里不出现秘密（`internal/backup/restic.go:202-208,535-560,838-858`）。

### 5.5 SSRF

面板会主动发起 HTTP 的目标共五处，全部由**管理员**配置，且都不是"用户随口填的 URL"：

| 目标 | 客户端约束 | 位置 |
|---|---|---|
| 部署健康检查 | 不跟随重定向（`ErrUseLastResponse`）、不复用连接 | `internal/deploy/probe.go:230-244` |
| 站点发现探测 | `Proxy: nil`（不吃环境变量代理）、dial/TLS 握手/整体超时、不跟随重定向 | `internal/discovery/probe.go:255-270` |
| 证书签发（ACME）目录 | 不跟随重定向、`Proxy: nil`、超时 | `internal/certificates/certbot.go:186-200` |
| 告警 Webhook | 不跟随重定向、`Proxy: nil`、响应体上限 | `internal/agent/notify.go:444-457` |
| 面板自身健康 | 本机回环，无外部输入 | `internal/api/server.go` |

响应体全部有上限（`io.LimitReader`：deploy/discovery/notify 各一处），
不存在"被对端喂一个大响应把代理内存吃光"。**没有一处把响应内容当命令或当文件内容使用**。

残余（写清楚，不假装没有）：这五处没有做"内网地址黑名单"。判据是——面板本来就在这台
主机上以 root 代理的身份工作，管理员让它去连 `127.0.0.1` 或 `10.x` 是**正常用法**
（探测本机站点、探测机房内网），加黑名单只会把功能砍掉而不增加安全性。真正需要防的是
"低权限用户借面板打内网"，而配置这些目标的路由全部在管理员组 + 再认证之下。

### 5.6 设备身份

- 指纹分级取值：sysfs `wwid` > sysfs `serial` > `/dev/disk/by-id` 的 `wwn-*`/`nvme-*` >
  `scsi-S*`/`ata-*` > 裸 `scsi-*`；低秩指纹不会覆盖高秩：`internal/storage/identity.go:23-60`。
- preflight 记下 `ExpectedIdentity`，执行前**再读一次**比对，不一致返回
  `CodeDeviceIdentityChanged`（409），不执行（`internal/storage/write.go:297-429`）。
- 系统盘保护沿 `partition_of` / `depends_on` 做定点闭包，覆盖 LVM/RAID/swap/挂载链
  （`internal/storage/protect.go:20-120`）——即"要格式化的那块盘上面挂着 /"会被拒，
  而不是只比一次设备名。
- 真机读数（P16.1/16.2 已做）：插拔后 `/dev/sdb` 与 `/dev/sdc` 互换，指纹不变，
  分区/格式化仍打在对的盘上。

残余：指纹全丢（虚拟盘没序列号、又是裸 scsi-id）时只能退化成"名字 + 大小"，
此时系统盘保护仍然生效（它不依赖指纹），但"同一型号两块盘互换"分不出来。已在
CONTRACTS 的已知限制里。

### 5.7 配置变更

- **改前**：`config_hash` 在 preflight 生成并回传给前端，执行时重算比对，
  不一致就拒（网络、代理、分享、防火墙四个域同形）。
- **改时**：先武装回滚、再 apply。网络是 `upanel-agent netrollback --token nrb_<32hex>`
  加 `upanel-netrollback@<token>.timer`（`OnActiveSec`、无 `[Install]`，重启后由
  `Reconcile` 重新挂）；防火墙同形（`fwrollback`）。回滚**不依赖面板**：
  timer 跑的是代理自己的二进制，读的是磁盘上的记录。
- **改后**：不信退出码。网络回读地址与路由、防火墙回读 `ufw show added`/`status`，
  对不上立刻自己回滚，不等 timer。
- **不改死自己**：`managementBlockers` 在写之前就拦住"把管理口断掉/把 SSH 关掉"这类改动。
- 记录文件写 `failed` 后停手，不自动重试（避免把一个说不清的状态变成两个）。

**本轮在这一维度上查出一个真问题，已修**，见下一节。

## 6. 本轮唯一的代码修复：原子写的"掉电后半条命"

### 怎么发现的

复核"配置变更会不会把机器改坏"时顺着 `writeFileAtomic` 读下去，发现它只做了
同目录临时文件 + `rename`。而对这份仓库的其它十来个写盘点做了一次全局比对：

```
grep -rn "\.Sync()" --include='*.go' . | grep -v _test
```

`internal/accounts`、`internal/schedules`、`internal/scripts`、`internal/shares`、
`internal/proxies`、`internal/certificates`、`internal/deploy`、`internal/files`
全都 fsync 了文件，多数还 fsync 了父目录。**只有 `internal/network` 两份
`writeFileAtomic` 一个 `Sync()` 都没有**——它不是"另一种风格"，是漏的。

### 为什么算阻断

`rename` 只保证新文件在**运行中的**文件系统里可见。ext4 `data=ordered` 下，
rename 进了日志而内容还在页缓存时掉电，重启后可能看到一个**长度正确、内容全零**的
文件。对 netplan 来说这就是一份解析不了的配置——一台远程管理的服务器，掉一次电之后
起网失败，等于把自己变砖，而回滚 timer 恰好也在"起网之后才跑"的那一侧。

而且这条路承载的不只是配置本身。`writeFileAtomic` 的全部调用点：

```
applyengine.go:1031   回滚记录 <token>.json
applyengine.go:1111   面板自己的 netplan 文件
applyengine.go:1135   回滚时还原面板文件
applyengine.go:1194   netrollback@.service 模板
applyengine.go:1229   netrollback@<token>.timer
firewallapply.go:1120 防火墙回滚记录
firewallapply.go:1227 fwrollback@.service 模板
firewallapply.go:1244 fwrollback@<token>.timer
```

也就是说**安全网本身（记录 + 单元 + timer）也走这条路**。一份零长度记录意味着 timer
到期后读不出记录、退不回去；一份零长度 timer 意味着它根本起不来——正好是 P14.2 设计里
声称"不存在"的那个状态：改坏了没人管。这不是"理论上的持久性问题"，是收不了场的失败模式。
按 P16.3 的处置原则（阻断性问题优先解决），修。

### 改了什么

| 文件 | 改动 |
|---|---|
| `internal/network/applyengine.go` | `WriteFileSystem` 增 `SyncDir(dir)`；`osWriteFS.WriteFile` 改成 open/write/**Sync**/close（不再是一次 `os.WriteFile`）；`osWriteFS.SyncDir` 打开目录 `Sync`；`writeFileAtomic` 在 rename 之后刷父目录，刷不下去就报错 |
| `internal/network/firewallapply.go` | 同名方法同一处补 `SyncDir` 并报错 |
| `internal/network/network_test.go` | `fakeFS` 增 `syncedDirs`/`syncDirErr` 与 `SyncDir`；新增 `TestWriteFileAtomicSyncsDirectoryAfterRename` |

为什么刷目录失败要**报错**而不是只记日志：内容这时已经换上了（运行中的 `netplan apply`
读得到新配置），但目录项没落盘。调用方据此在 apply 之前停手，比"以为写稳了"诚实；
EIO 一次意味着盘在坏，宁可这轮改动失败。

### 修完的读数

```
$ gofmt -l internal/network/      # 空
$ go test ./internal/network/
ok  	upanel/internal/network	0.770s
```

变异验证（证明新用例真会红）：把 `writeFileAtomic` 里那段 `SyncDir` 摘掉再跑——

```
--- FAIL: TestWriteFileAtomicSyncsDirectoryAfterRename (0.00s)
    network_test.go:821: rename 之后应刷一次父目录，实际刷了 []
```

恢复后转绿。**这条修复没有在真机上做过掉电演练**（要拔电源，不在本轮验收范围），
只验证了"代码路径被走到、失败会被报出来"。真机掉电语义标 NOT RUN。

## 7. 未验证 / 有意的残余风险

| 项 | 状态 | 说明 |
|---|---|---|
| 掉电后 netplan 文件完整性 | **NOT RUN** | 需要真机断电，不在本轮验收范围；只做了路径与失败的用例验证 |
| 回滚 timer 的到期行为 | **本轮未重做** | P14.2/P14.3 做过真机演练（记录在对应 PROGRESS 小节），P16.3 未重复 |
| 登录限速跨重启/多进程 | 有意不共享 | 进程内计数，PLAN.md 第 5 节的取舍 |
| SSRF 内网地址黑名单 | 有意不做 | 管理员配置本就是内网用途；见 5.5 残余 |
| `metric_samples` 增长 | 已量化，不改 | ~30 MB/天，被 24h 原始保留截住；数字交给 P16.4 的已知限制 |
| 老内核（< 5.6）无 `openat2` | 有意不降级 | 功能不可用优于围栏失效 |

## 8. 证据文件

`facts/` 下都是原始输出，未编辑：

| 文件 | 是什么 |
|---|---|
| `report.txt` | 探针的完整记录（11 个样本 + checkpoint） |
| `sizes.txt` | 12 行体积表（DB/-wal/-shm/agent -wal/总行数） |
| `tables-before.txt` / `tables-after.txt` | 各表行数前后对比，`metric_samples` +774 |
| `probe-stdout.txt` | 探针在 VM 里的标准输出 |
| `wal-probe.tgz` | 探针脚本与 VM 侧的原始输出打包 |
| `p16-3-wal-probe.sh` | 探针脚本本身（与 `tests/integration/p16-3-wal-probe.sh` 同源） |
| `http-and-unit.txt` | 真机 HTTP 响应头、`systemctl show` 读数 |
| `socket-boundary.txt` | 普通账号 vs `upanel-web` 连 agent.sock 的对比 |
| `live-routes.txt` | 真机匿名请求抽样（401/404/405） |
