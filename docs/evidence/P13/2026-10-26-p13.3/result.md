# P13.3 验收结果：通知通道、秘密引用、SMTP/HTTPS 投递、测试发送、投递账与失败重试

- 日期：2026-10-26（UTC）
- 分支 `main`，commit `8e52b36`（工作树未提交，按用户要求不提交不推送）
- 验收对象：**"告警响了之后，谁真的会知道"**——通道的形状与校验、`secret_ref` 的用途边界、
  保存配置时**一封都不发**、显式测试发送的回执语义（含"投递失败也是 200"）、webhook 的
  六条硬限制（只 https、拒 userinfo 与 `#片段`、不跟随重定向、只读 4 KiB、8 秒上限、
  令牌走 Authorization）、SMTP 的两条 TLS 路线（STARTTLS / 隐式，没有明文模式）与 AUTH、
  告警驱动下的投递账（入队与事件同事务、静默不入队、`(event_id, channel_id, kind)` 去重、
  30 秒退避、用尽上限判死、通道停用判死且不计 attempts）
- 验收方式：真实 Web（非 root）→ `/run/upanel/agent.sock` → root 代理 → 真实 Ubuntu 主机；
  脚本自带本地接收器（SMTP STARTTLS 19471 / SMTP 隐式 TLS 19472 / HTTPS 19470，应答码由
  控制文件决定）与只在本次验收存在的本地 CA（装进系统信任库、结束删除，**不是关掉校验**）。
  两台专用 Ubuntu VM **同一份脚本、同一组二进制、一台一跑不并发**。

## 1. 结论

| VM | 发行版 | 结果 | 退出码 | 断言 |
|---|---|---|---|---|
| 2404 | Ubuntu 24.04.5（内核 6.8.0-147，Python 3.12.3，sqlite 3.45.1，2 vCPU） | **PASS** | 0 | 211 通过 / 0 失败 / 0 未执行 |
| 2204 | Ubuntu 22.04.5（内核 5.15.0-198，Python 3.10.12，sqlite 3.37.2，2 vCPU） | **PASS** | 0 | 211 通过 / 0 失败 / 0 未执行 |

逐节断言数（两台一致）：§0 环境 **13**、§1 装配（本地 CA + 接收器 + 代理 + 管理员 + Web）
**11**、§2 通道校验与权限 **48**、§3 秘密引用 **12**、§4 保存不发送 + SMTP 测试发送 **44**、
§5 webhook 测试发送 **34**、§6 告警驱动/退避/上限/停用判死 **35**、§7 审计与日志卫生
**14**。归一化后的 211 条断言名单跨机 `diff` 只有两行不同，且两行都是 **PASS**，差别只在
括号里量到的数值：

```
151c151
< PASS  投递在 8 秒上限上结束（实测 9 秒，面板的调用上限是 10 秒）
---
> PASS  投递在 8 秒上限上结束（实测 8 秒，面板的调用上限是 10 秒）
178c178
< PASS  退避时间是 30 秒（next_attempt_at - created_at = 35，节拍 5 秒内，此刻还剩 20 秒）
---
> PASS  退避时间是 30 秒（next_attempt_at - created_at = 31，节拍 5 秒内，此刻还剩 25 秒）
```

这两处差异是**调度相位**，不是产品差异：8 秒上限那条的判据是 7—10 秒（`now_unix` 只有
整秒精度）；退避那条的判据是 30—35 秒（=30 秒基准 + 一个 5 秒调度节拍），两台分别落在
区间的两端（35 与 31），正好把区间从两边各钉了一次。

宿主 `scripts/check.sh`：**PASS 14 / FAIL 0 / NOT RUN 0**，见第 6 节。

**24.04 跑了六轮，只有第 5、6 轮全绿。** 前四轮的红没有一条是"产品把功能做错了"——前两轮
是验收工具与验收环境的缺陷，后两轮是验收判据本身错（其中 Q426 那条尤其危险：它把产品**合法**
的 400 读成了"产品把停用判成了用尽上限"，两条断言还互相印证）：

| 24.04 轮次 | 结果 | 红在哪 | 归类 |
|---|---|---|---|
| 1 | 153 / 27 | SMTP 的 STARTTLS 一次都过不去，连带 §4、§6 全部超时 | **验收工具缺陷** Q424：接收器的 `start_tls` 把握手后的数据丢了 |
| 2 | 203 / 5 | 种入一个样本都没落库、viewer 的错误码读空、超时措辞不匹配 | **验收脚本缺陷**：`phase` 写的文件是 0600 root、断言读错了响应文件、判据过窄 |
| 3 | 204 / 3 | 停用判死两条（`failed\|2` + 原因 `boom`）+ 退避 19 秒 | **验收脚本缺陷 + 判据缺陷** Q426 / Q425 |
| 4 | 210 / 1 | 只剩退避（量到 `35`，判据当时要求正好 30） | **判据缺陷** Q425 的后半段 |
| 5 | **211 / 0** | — | 第一次全绿（当时 `/tmp/upanel-bin` 里那批二进制） |
| 6 | **211 / 0** | — | 本文件指纹那批二进制（从当前工作树重新构建） |

22.04 只跑一轮（211 / 0），用的就是第 6 轮同一份脚本与同一组二进制。

**证据的边界**：第 1、2 轮的脚本 stdout 被后续轮次覆盖了（同一个 `/var/tmp/run*.log` 路径，
第 4—6 轮的都在），只留下当时读进对话里的失败名单，逐字抄在第 3 节；第 3 轮的 `$WORK`
日志留在宿主的临时解包里，其中"停用 PATCH 被产品判 400"的那几行已单独归档为
`logs/2404/round3-disable-400.txt`（Q426 的现场）。不把已经丢掉的日志假装还留着。

## 2. 交付指纹

宿主交叉编译（当前工作树、`8e52b36` + 未提交改动）：

```
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags '-s -w' \
  -o /tmp/upanel-p133/upanel-{agent,web,ctl} ./cmd/upanel-{agent,web,ctl}
```

| 产物 | md5 | sha256 |
|---|---|---|
| `upanel-agent` | `f7acfd272a1bbed5717f6e78e0e62432` | `9677c147f8602e8e3ac8d71c5faf64c882e9e61627d62b1e20e629c332f300cb` |
| `upanel-web` | `8c8b258d9e5eb8be5cb42762d561afea` | `26d1c7f0e40c181ab679791d30c543721a09f8f15821b154feb3b8482e041f91` |
| `upanelctl` | `45fb01bebc2694f191480832bec75637` | `9b75746eb4487230f8dff8e0bc9eb6bd006ae7f1995f62df4f7a0838341775c4` |
| `tests/integration/p13-3-acceptance.sh` | `f8f6984d0f5e302970b161899a90ff28` | — |

两台 VM 上 `md5sum /tmp/upanel-bin/* /var/tmp/p13-3-acceptance.sh` 与上表逐字一致，见
`logs/2404/env.txt` 与 `logs/2204/env.txt`。

**为什么第 6 轮是重新构建的**：第 5 轮全绿时，`/tmp/upanel-bin` 里那批二进制是上一轮会话
留下的（md5 `e2ccb78d…` / `500d6145…` / `cca62b0d…`），来源无法从仓库里证明。为了让"两台
机器跑的是**当前工作树**编出来的同一组二进制"这句话站得住，从当前工作树重新构建了一次、
两台一起换、各跑一轮。第 5 轮同样留在表里，不因为它是旧二进制就抹掉。

## 3. 这一阶段撞到并修掉的问题（全部是验收侧）

P13.3 的两台真机验收**没有抓到产品缺陷**——抓到的是验收工具、验收环境与验收判据上的四个
问题（Q424—Q426 写进 `docs/DECISIONS.md`）。这不等于"产品一次就对了"，只等于"这一轮红过的
地方都在脚本这一侧"，逐条如下：

### Q424：接收器的 STARTTLS 在 Python 3.12 上必挂

首轮 24.04 的 27 条红里，绝大多数是同一件事的连锁：代理调 SMTP，`net/smtp` 卡在 STARTTLS
满 8 秒，报 `STARTTLS 失败：read tcp …: use of closed network connection`（代理的 8 秒
deadline 把连接掐了）。而 `openssl s_client -starttls smtp` 对**同一个**接收器全程畅通——
两个客户端一个过一个不过，最容易得出"产品的 SMTP 客户端坏了"的结论。实际不是。

定位链：写 Go 探针（`net/smtp` + STARTTLS + PlainAuth，跑在宿主，经 SSH 隧道打 VM 的接收器）
先复现；探针把接收器的统计打出来后出现 `dropped-while-paused n=16 hex=45484c4f…`——那串
hex 正好是明文 `EHLO localhost`；再用 `tcpdump -i lo -A` 证明接收器线上发出去的确实是
ClientHello（所以不是网络或证书问题）。结论：`loop.start_tls(transport, protocol, ctx,
server_side=True)` 的第二个参数决定**握手解密之后**的应用数据交给哪个协议对象；3.12 上
对端在握手后的第一句 EHLO 会在 `await` 返回前几毫秒抵达，被当成本次升级之前的明文丢掉，
对端于是只能等到超时。

修法是给接收器加一层 `TLSBridge`：握手期间收到的字节只进 `pending_in`，`start_tls` 返回后
置 `ready=True` 再把缓冲重放给 `SMTPSession`；同时显式 `server_side=True`（默认是 False，
漏掉会以"对端不是 TLS"失败，看起来像证书问题）、接管返回的新 transport（不接管的话握手
之后的字节会以**明文**发出去——那是最坏的一种失败）、丢掉升级前的旧缓冲（升级前读到的
明文不该被当成命令执行）。修完 Go 探针输出 `starttls: ok / auth: ok / delivered`，隐式 TLS
与 535 拒绝路径各复验一次。

### Q425：退避时长不能用"我们到达时还剩多少秒"来量

第 3 轮 `退避时间是 30 秒: 此刻还剩 19 秒` 判红，产品一秒不差。真因在产品的调度语义里：
`dispatch.go` 的 `now` 取自 `DispatchOnce` 开头，`deliveries.go` 写
`next = now.Add(retryDelay(attempt))`、`retryBaseDelay = 30s`；而调度节拍
`dispatchInterval = 5s`，投递行入队即到期，于是首次尝试一定落在入队后的**第一个节拍**上——
`next_attempt_at - created_at ∈ [30, 35]`，区间宽度就等于节拍本身。我们晚十几秒去读，
读到的"剩余"当然不是退避的长度；把它当退避长度，就把一个正确的实现判成了错。

改成 `30 ≤ next_attempt_at - created_at ≤ 35` 并把上面这段推导连三个常量一起写进脚本注释
（不写成"放点余量"，是因为区间的宽度有确切来源）。附带修掉两个取值坑：
`strftime('%s','2026-10-26T09:03:43Z')` 在 SQLite 里返回 **NULL**（要先
`replace(created_at,'Z','')` 才是 epoch），`next_attempt_at` 在"还没排过"和"已经收口"时
都是 **0**（要 `COALESCE`，否则拼出来的判断字符串是空的）。两台机器的实测值 35 / 31
正好是节拍相位的两端。

### Q426：空版本号拼进 PATCH 的 body，产品合法的 400 被读成了"停用没生效"

第 3 轮留下的两条红是这一阶段最危险的一次误判：

```
FAIL  停用后 pending 判 failed，attempts 仍是 1（一次都没多算）: 期望 'failed|1'，实际 'failed|2'
FAIL  判死原因说是通道停用: 缺少 '停用'（实际: boom: acceptance always fails here）
```

看上去像产品把"停用判死"做成了"用尽上限判死"，而且两条互相印证。真因在脚本一行调用：
`chan_version "$WORK/channels.json" "$FAIL_ID"` 多传了一个参数，而该函数只吃一个 id、
文件名是写死的，于是 id 变成那句路径、`int()` 抛异常、末尾的 `2>/dev/null` 把 stderr 吞干净
→ `FAIL_VER_NOW` 是空串 → 请求体拼成 `{"enabled":false,"version":}` → 产品回 **400
VALIDATION_ERROR**，停用从未发生。现场（`logs/2404/round3-disable-400.txt`）：

```
web.log  09:00:25 PATCH /api/v1/notification-channels/5  200   ← 启用成功
web.log  09:04:36 PATCH /api/v1/notification-channels/5  400   ← 停用被拒
audit 58 2026-10-26T09:04:36Z notification.channel.update failure
         {"error_code":"VALIDATION_ERROR","http_method":"PATCH",
          "http_path":"/api/v1/notification-channels/5","stage":"validation"}
channel 5  enabled=1 version=2   ← 停用从未落地
```

修法：调用改对；这一步现在自己有四条断言（列表 HTTP 200、取到的是数字、PATCH 回 200、
`enabled=false` 生效），非数字版本号**直接不发**——空值不再能穿过断言链。停用窗口没等到时
（`ABANDON_READY != yes`）如实报"没赶上退避窗口"并带上当时那一行，不再把"用尽上限"说成
"停用判死"。修完投递账里是：

```
验收-回调-必失败 | resolved | failed | 1 | 2 | 500 | 通道已停用，未发送
```

attempts 停在 **1**（一次都没多算），原因来自 `dispatch.go` 的
`abandonDelivery(ctx, d.ID, "通道已停用，未发送")`。

### 其余验收脚本缺陷（第 2 轮那 5 条红）

- **种入一个样本都没落库**：`phase()` 写的 `probe.phase` 是 root 下的 0600，而种入以
  `upanel-web` 身份跑，读不到 → 0 样本 → 告警不触发 → 后面二十多条断言连坐。加 `chmod 0644`。
- **viewer 的错误码读空**：`channelsapi` 把响应写进 `channels.json`，断言却读 `resp.json`。
- **超时措辞断言过窄**：期望 `Client.Timeout`，代理用的是带 8 秒 deadline 的 context，
  Go 的原话是 `context deadline exceeded`。契约只要求"超时算可重试的失败并说清原因"，
  改成两种都收。
- **`$(( ))` 撞上非数字把整轮验收掀翻**：`DELAY=$((FAIL_NEXT - NOW))` 收到
  `<sql-error: …>` 时 dash 直接 `Illegal number` 退 2，后半程验收点一个都跑不到。加 `is_num`。
- **`sqlq` 把"没有这一行"打成错误**：`fetchone()` 返回 None 被当成异常，`E1` 变成
  `<sql-error: 'NoneType'…>` 拼进后面的 SQL，连环 `syntax error`。改成 `<missing>` 语义，
  并 `is_num "$E1" || E1=0`。
- **接收器把 `MAIL FROM` 记成 `upanel@accept.test> BODY=8BITMIME`**：没切 ESMTP 参数，
  改成第一个空格之后丢掉再剥尖括号。

## 4. 断言覆盖了什么

- **§2 通道校验与权限（48 条）**：viewer 读通道列表 403（通道列表就是"告警会发给谁"）、未登录
  401、写要 admin + CSRF；迁移种下的 in_app 通道只有一条、默认启用、没有秘密、`max_attempts=1`、
  再建一条 in_app 400 且报错说清"面板内通知只有一条"；`type` 取闭集外 400 且报错列出允许的
  三种；`name` 空、smtp 缺 `host`/`port`/`from`/`to`、要明文模式、只有用户名没秘密、只有秘密
  没用户名——每条都断言**报错点出的正是缺的那个字段**；webhook 用 http、带 userinfo、带
  `#片段` 全部 400（各自的理由：正文不走明文、令牌该用 `secret_ref`、片段不会发出去）；
  `max_attempts` 超上限 400；失败的创建一条都没落库；改/删不存在的通道 404；删 in_app
  400 且报错说清"改用 `enabled=false`"。
- **§3 秘密引用（12 条）**：`POST /secrets` 要再认证（未再认证 403、口令错 403、对了 200）；
  上传 SMTP 口令与 webhook 令牌都 201，返回的是**引用不是明文**、响应里没有口令/令牌本身；
  用途取闭集外 400；空秘密 400。
- **§4 保存不发送 + 通道并发 + SMTP 测试发送（44 条）**：建好/改好 smtp 与 webhook 通道之后，
  本地接收器**一个请求都没收到**、投递账 **0 行**（保存一次就往外面喷一封邮件是取消不掉的行为，
  PATCH 之后也再验一次 0 行）；PATCH 的 `version` 语义（不给 400 且报错说明用途、对了 200 且
  `version` 递增、没提的字段沿用原值、旧 `version` 409）；STARTTLS 带 AUTH 的信被收下且正文
  **base64 解码后 sha256 与契约逐字相同**、信封发件人/收件人/标题都对；隐式 TLS 同样通、
  没配用户名就没有 AUTH；对端 535 时接口仍回 **200** + 投递行 `failed` + 对端原话 + 说清是
  "认证失败" + 里面没有口令，且**只多连了一次**（测试发送不重试）。
- **§5 webhook 测试发送（34 条）**：200 → `sent` 且接收器收到一个请求、打到配置的那条路径、
  令牌以 `Authorization` 到达（日志里只有 sha256）、请求体带 `source=upanel` 与测试标题；
  500 → 接口仍 200、投递 `failed`、`last_status=500`、`last_error` 是对端原话、且 `/always500`
  **只被打了一次**；302 → `failed`、`last_error` 说明不接受重定向、`/redirect` 只被打一次
  （**没有跟随**）；12 秒不回 → 8 秒上限内结束（实测 8/9 秒，面板上限 10 秒）、
  `last_error` 说清是超时；`secret_ref` 用途不对 → 面板照样建（它只存引用）、投递 `failed`、
  投递账里说明原因就是用途，且**连接根本没建起来**（代理在核用途时就拒了）；in_app 测试发送
  400 且报错说清"它的投递就是收件箱里的一行"；删掉测试通道后投递账里历史还在
  （`channel_id` 置空、行不消失）。
- **§6 告警驱动与投递账（35 条，受控种入的一条序列）**：维护静默期间规则照常触发、事件被标
  `silenced`，但**一条投递都没入队**，收口时也不补发；触发事件按通道各一行、同事件同通道同
  kind 只有一行（去重）；必失败通道第一次尝试后 `pending`，
  `next_attempt_at - created_at = 30（+一个节拍）`，`last_error` 是对端原话；先坏后好的通道
  第二次尝试送达（退避到点真的会重试，`attempts=2`）；`max_attempts=2` 的通道用尽后判
  `failed` 且 `attempts=max_attempts`；in_app 写入即 `sent`、`attempts=0`/`max_attempts=1`
  （它不出门）；恢复事件同样发、正文说清是恢复；**停用通道上 pending 的恢复投递判 `failed`、
  `attempts` 一次都没多算、原因是"通道已停用，未发送"**；停用之后不再入队。
- **§7 审计与日志卫生（14 条）**：`notification.channel.create/update/delete/test/manage`
  五类都落 `audit_events`；审计里 `secret_ref` 是 `[redacted]`（Q423 的已知限制）；**口令与
  令牌不出现在面板库、代理库、两端日志与接收器日志里**（扫整个 `$WORK`）；接收器读的口令
  文件是 0600；两端日志没有 panic、web 日志没有"代理返回 500"。

## 5. 验收方法与环境的修正

- 本地接收器：`TLSBridge`（Q424）；`MAIL FROM` 切掉 ESMTP 参数；`phase` 写 0644。
- 查询与取值：`sqlq` 区分"查不到"（`<missing>`）与"查错了"（`<sql-error: …>`）；`is_num()`
  守卫所有进 `$(( ))` 的值；`strftime` 先剥 ISO 尾巴上的 `Z`；`next_attempt_at` 一律
  `COALESCE(…, 0)`。
- 断言链：每一步自己断言，不允许静默降级成空值（Q426）；停用窗口等不到时如实报"没赶上"。
- 日志取值：接收器的 `http.jsonl` 里字段顺序不固定、且有被截断的行，解析一律按正则取
  `"ts"` / `"path"`，不假设 `json.loads` 能整行通过。
- VM 上的进程管理：`pkill -f "recv/receiver.py"` 的模式会匹配到承载它的 `bash -c` 并把
  自己杀掉（exit 255），改用 `fuser -k PORT/tcp`。

## 6. 宿主检查

```
PATH="/h/Code/.toolchains/node-v22.23.3-win-x64:$PATH" sh scripts/check.sh
```

**PASS 14 / FAIL 0 / NOT RUN 0**（`logs/host-check.txt`）。明细：`line-endings` / `go-build` /
`go-vet` / `go-test` / `go-build-linux` / `go-vet-linux` / `gofmt` / `node-version` /
`web-typecheck` / `web-lint` / `web-test` / `web-build` / `web-smoke` / `openapi-lint` 全过。
`go test ./...` 46 个包 ok、3 个 no test files（多数命中缓存，`internal/api` 这一轮显示
`(cached)`——代码与测试文件自上次全绿以来没有改动）；web **49 文件 939 用例**；`web-smoke`
10/0/0；`openapi-lint` **valid + 6 warnings**（6 条 warning 本阶段之前就有）。

## 7. NOT RUN（明确未执行，不算通过）

1. **真实第三方邮件服务**：收件方是脚本自带的本地接收器，没有向任何真实联系人发过信（这是
   STAGES P13.3 划的边界）。真实服务商的限速、SPF/DKIM/DMARC 反馈、退信（bounce/DSN）解析
   都没有样本——产品也没有实现退信解析。
2. **真实公网 webhook**：只有本地 HTTPS 接收器与本地 CA。公网回调、真实 429/5xx 的节奏、
   对端 IP 变化、代理链都不在证据范围内。
3. **长周期退避与 10 分钟封顶**：真机上只验到第二次尝试（`max_attempts=2`/`3`，30 秒 → 60 秒），
   `30s × 2^(n-1)` 一路涨到 `maxRetryDelay=10m` 封顶那条只有单测
   （`TestRetryBackoffAndGiveUp`），没有在真机上等到十分钟量级。
4. **投递账的读接口属 P13.4**：CONTRACTS §13.3 明确读路径在 P13.4，所以本轮断言一律按
   事实直读 `/var/tmp/…/webdata/upanel.db`（只读、`busy_timeout=15s`），**不是**验证接口。
5. **多收件人、大正文、附件**：SMTP 通道的 `to` 只有一个地址，正文是纯文本；`BODY=8BITMIME`
   交给了 `net/smtp` 自己加，但没有大正文/多收件人/字符集边界（中文以外）的样本。
6. **审计红字化只验到"秘密没进表"**：`secret_ref` 也一起被抹成 `[redacted]`（Q423 的已知
   限制），"凭据从 A 换成 B 在审计里看得出来"这条**做不到**，本轮没有改，也没有验。
6. **`retryable` / `accepted` / `detail` 这三个字段本身没在真机上断言**：它们是代理回给面板的
   内部回执形状（`/internal/v1/notifications/deliver`），公开的测试发送接口回的是投递行
   （`state`/`last_status`/`last_error`）。"可重试 vs 判死"的**效果**在真机上是间接证明的
   ——可重试的走退避重试（§6 先坏后好第二次成功），不可重试的一次判死（§5 的 302 与 500）
   ——字段层面的形状只有单测（`classifySMTPError` / `classifyWebhookResponse` 的表驱动用例）。
7. **`go test -race` 仍未执行（NOT RUN）**：宿主 Windows 无 gcc、`-race requires cgo`，
   与 P12.3—P12.5、P13.1、P13.2 同。

## 8. 相关文件

- 实现：`internal/notifications/{notifications,channels,deliveries,dispatch,agentsender}.go`、
  `internal/agent/notify.go`、`internal/api/notification_channels.go`、
  `migrations/0013_notification_channels.sql`
- 契约：`docs/CONTRACTS.md` §13.3（含"已知限制"与投递账列语义）、`api/openapi.yaml`
- 决策：Q421（由代理发送、用途由代理核对）、Q422（失败回 200 + `retryable` 由代理判、
  10 分钟封顶、入队与事件同事务）、Q423（审计红字化抹掉引用值）、Q424（接收器的 TLS 升级）、
  Q425（退避的观测量）、Q426（断言链不许静默降级）
- 脚本：`tests/integration/p13-3-acceptance.sh`（md5 `f8f6984d0f5e302970b161899a90ff28`）
- 日志：`logs/2404/{acceptance.log,acceptance.round4.log,acceptance.round5.log,
  results.txt,env.txt,round3-disable-400.txt}`、`logs/2204/{acceptance.log,results.txt,env.txt}`、
  `logs/host-check.txt`
