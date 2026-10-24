P12.1 验收结果（2026-10-24）
============================

范围
----
STAGES §14 第 1 项：**P12.1：restic 本地/SFTP 仓库、秘密引用、SFTP 主机密钥验证、
仓库初始化和容量检测。**

验收条件（原文，STAGES §14）：BACKUP-01「备份后恢复、错误密码、网络断开 → 校验一致，
失败不假成功」、TIMER-01，以及 §14 正文「错误密码/主机密钥拒绝；存储满/网络断开标失败
且保留原数据」。

这一段的边界要说清楚：**BACKUP-01 的"备份后恢复"本轮没有对象**——写快照与还原是
P12.2/P12.3。所以 P12.1 只对"仓库"这一层负责，能证明的是：

1. 仓库**真的建起来了**：`restic cat config` 读回 64 位十六进制仓库 id，代理报出的
   id 与脚本自己再跑一次 `restic cat config` 读到的逐字节相同（不是"面板说成功"）。
2. **只建新仓库，不覆盖已有数据**：目标位置已经是仓库时预检 `allowed=false` +
   `BACKUP_REPOSITORY_EXISTS`，任务不会下发。
3. **主机密钥是我们自己核对的**：指纹只接受显式登记的那把；不匹配就拒绝，且把登记的
   与实际的两把一起报出来。已知主机文件由我们在核对通过后写出（0600），
   `StrictHostKeyChecking=no` 全链路一次都不出现。
4. **秘密按用途把关**：仓库口令只认 `backup_repository_password`、私钥只认
   `sftp_private_key`；拿 TLS 私钥当口令 → `BACKUP_SECRET_PURPOSE`，引用不存在 →
   `BACKUP_SECRET_UNRESOLVED`。秘密明文不进 argv、不进日志、不进面板库。
5. **容量是读出来的，不是 0**：本地用 `statfs`、SFTP 用 `StatVFS`（沿祖先目录向上找到
   第一个存在的目录）；读不到就 `supported=false`，**绝不把"不知道"写成 0**。
6. **口令错要说得出口令不对**：现状查询在 `repo_config_error` 里给原因，`repo_id` 留空，
   不把"解不开"当成"空仓库"。
7. **默认拒绝**：镜像路由只认 `upanel-web` 的 SO_PEERCRED，root 直接 403（真机上验到：
   `拒绝对端 UID 不在允许范围, uid 0, allowed_uid 997`）。
8. **"查不了"不等于"没有"**：连不上的主机上 `initialized` 是 `null` 而不是 `false`，
   原因写进 `host_key_error`。

产品面（P12.1 只做后端与代理，没有 Web 页面）
---------------------------------------------
- `internal/backup`：仓库定义（本地/SFTP 两种后端、路径规范化、父目录穿越拒绝）、
  指纹解析与比较、SSH 拨号与主机密钥核对、known_hosts 写出、restic 调用
  （`-o sftp.command=<我们自己的 ssh>`，口令只经 `RESTIC_PASSWORD` 环境变量）、
  `restic cat config` 读回、容量探测、判决（`Assess`）与执行（`Execute`）。
- `internal/agent`：动作 `backup.repository.initialize`（Decode/Plan/Execute/Policy）、
  查询 `backup.repository.state`、以及**仓库定义镜像**路由
  `GET/POST /internal/v1/backup/repositories`、`GET/PATCH/DELETE …/:id`。
- 秘密用途闭集新增 `backup_repository_password`、`sftp_private_key`。
- 契约：`docs/CONTRACTS.md` §15 增补镜像路由与 `backup.repository.state`；预检参数
  冻结为 `{repository_id, version}`（`normalized_params` 只有这两项）。

为什么定义走"镜像路由"而不是"动作"
----------------------------------
登记一条仓库定义是**配置写入**，不是危险写操作：它不该产生 job、不该占资源锁、也不该
要再认证。所以它走一条只同步元数据的镜像路由（PATCH 带 `version` 做乐观并发，DELETE
只删定义、**不删数据**），而"真的去把仓库建出来"才是动作——那一步预检要说清影响面
（`create_repository` + `secret_use`）、占 `backup-repo:<id>` 锁、要再认证。
预检计划里的 `normalized_params` 是**冻结**的：执行前复核用同一份，仓库定义在此期间
被改过就会因为 `version` 不一致而失败，不会拿旧计划去建一个新定义描述的东西。

代码清单（跑验收时 HEAD = 659ba02）
-----------------------------------
6c0074a  feat(p12.1): restic 仓库——定义镜像、秘密按用途把关、主机密钥自验、初始化后读回核对
         internal/backup/{definition,fingerprint,ssh,restic,capacity,ops,local_linux,
                          exec,module,repository}.go + 单测
         internal/agent/{backupaction,backuprepo,backupdeps,backupquery}.go + 单测
         internal/secret、internal/agentstore（`backup_repositories` 迁移，schema 7）
         docs/CONTRACTS.md §15、go.mod/go.sum（`github.com/pkg/sftp v1.13.9` 直接依赖）
d99998c  fix(backup): 主机密钥按算法逐个重试，直到拿到登记的那把          （缺陷 A）
         + 验收脚本自身六处修复（见下）
de3cef8  fix(backup): 握手撞上网络抖动时整轮重来一次，不再一次判死          （缺陷 B）
90a669b  fix(api): power-state 的 503 不再说成"读不到软件包状态"           （P11.3 的观测缺口）
1ae4e39  fix(agent): 执行前复核被拒时把原因写进日志                      （缺陷 B 的诊断前提）
659ba02  fix(backup): 被上层掐断的超时不再报成 restic 退出状态 -1，
         只读查询预算按最慢现场定                                       （缺陷 C）

真机抓出的三个产品缺陷（都不是配置问题）
----------------------------------------
### A. 登记的是指纹，服务器先递的是另一把（d99998c）

第一遍真机验收在 24.04 上是 **PASS 88 / FAIL 33**，失败集中在同一条线上：SFTP 的
"正确那一份"也过不去（`preflight-ok-allowed=False`），指纹**错**的仓库报
`BACKUP_HOST_KEY_MISMATCH`（本来就该是这个），但指纹**对**的仓库也报同一个码。原文：

    登记 SHA256:R7OLp576qkjHvSdXo0CqPKnCOMq4A4adNKT7D5ZCexY
    实际 ecdsa-sha2-nistp256 SHA256:SwVU9L+kHgmfG5gQemVJu2screBFlraCvUpDZ0bK/KI8

根因：x/crypto 的 `defaultHostKeyAlgos` 把 `ecdsa-sha2-nistp256/384/521` 排在
`ssh-ed25519` **前面**，而 `HostKeyCallback` 只能对协商出来的那一把表态，返回错误就
中止握手——没有"换下一把"的钩子。管理员抄的 `ssh-keygen -lf` 第三列**只有摘要、没有
类型**，所以契约里表达不出"按类型钉死"。

修法：不匹配时重新握手，把 `HostKeyAlgorithms` 钉成一种算法逐个试，已经见过的类型不再
试；**只有指纹相等才算通过**。TCP 连不上不换算法，认证失败不重试（指纹已经对了）。

### B. 一次握手抖动就把"连不上"定死（de3cef8）

Q329 修完分类之后，22.04 上仍然有两轮卡在 SFTP 仓库初始化。`1ae4e39` 加的日志给出了
原因（原文见 `logs/2204-dial-timeout-round.txt`）：

    {"level":"WARN","msg":"执行前复核被拒","action":"backup.repository.initialize",
     "code":"BACKUP_SFTP_UNREACHABLE",
     "reason":"backup: 与 127.0.0.1:22 的握手失败（网络层）: ssh: handshake failed:
               read tcp 127.0.0.1:56442->127.0.0.1:22: i/o timeout"}

（原文是一行 JSON，这里按字段换行。）连接已经建起来了（有本地端口），是**握手读超时**
吃满了 15 秒 `DefaultDialTimeout`；紧接着在同一台机器上手动重放 6 次全部在数百毫秒
内连上，sshd DEBUG2 没有鉴权失败，每秒一次的连接采样里也没有 ≥3 秒的断层——机器没被
冻住，远端没被改配置。原实现一次网络层失败就把结论定死。

修法：`Dial` 拆成"网络层重试外壳 + 算法轮"，外壳只对 `isNetworkFailure` 再走一轮
（两轮封顶、间隔 250 ms、ctx 到期优先返回上一轮的网络错误），认证失败与指纹不符不重试。
回归两条：第一次握手卡住、第二次成功 → 连上；一直卡住 → 只连两轮就收手，报网络层错误。
这一轮的两台虚机都**没有**再撞上握手抖动（agent.log 里 0 条握手失败），说明它是兜底。

### C. "我们没等完"被写成"restic 出错"（659ba02）

24.04 上用 `de3cef8` 的二进制跑，**PASS 132 / FAIL 1**，唯一一条失败是
`state-wrongpass-says-password-wrong`，原因原文：

    restic cat config 失败：backup: /usr/bin/restic 以退出状态 -1 结束

查下去是两件事：

1. 只读查询自己的 **30 秒预算先用尽**，上层 ctx 到期把整组进程一起杀掉；而
   `runOutcome` 只判了"restic 自己的超时"，父 ctx 到期掉进 `ExitError` 分支，
   `ExitCode` 是 -1，于是那句"以退出状态 -1 结束"——**谁停的、是崩溃还是被杀，
   读的人只能猜**，而且方向指错了：人会去查 restic、查仓库、查口令。
2. 同一台机器上量到：`ssh` 握手 3—9 秒、SFTP 逐次 stat 各一个往返、
   `restic cat config` 5—12 秒（口令错时 12.39 s / 5.16 s 两次实测），整串 30 秒上下。
   **30 秒的预算刚好卡在边界上**，把一次正常的查询判成超时。

修法两条：判定表拆成纯函数 `runOutcome`，分出 `ErrBudgetExhausted`（上层预算用尽，
话是"这次没有结论，不等于仓库有问题"）与 `ErrExecTimeout`（restic 自己跑太久），
`ExitError` 带上信号名；`backupStateBudget` 30 秒 → 45 秒，按"最慢的合理现场"定，
预检本来就是 40 秒。重跑后两台都是 133/0/5，口令错的原因回到
`仓库口令不正确（RESTIC 拒绝了这次解密）`。

验收脚本自己抓出的六条问题（其中三条会造出假绿）
------------------------------------------------
1. **假绿：`空等于空`。** `independent-read-repo-id` 比的是"代理读回的 id"与"脚本自己
   读回的 id"，两边都失败时都是空串，于是相等、于是 PASS。改成两边都要先非空才比。
2. **假绿：`known_hosts 是我们写的`。** 上一轮留下的 known_hosts 会让这条断言无条件
   成立。改成准备阶段清掉，并在第 6 段断言**预检之后它还不存在**（预检不写任何东西，
   CONTRACTS 4.1），第 7 段再验它出现、0600、内容含登记的指纹。
3. **假绿：`run_op` 的响应被请求体覆盖。** 提交失败时那句 409 正文才是证据，而脚本把
   请求写到了响应那个文件上。改成响应进 `payloads/operation-<名字>.json`、请求另存。
4. **每个操作都 `PREFLIGHT_STALE`。** 预检与提交的 `session_id_hash` 各随机一次——
   代理按"会话换了，冻结的计划不能沿用"如实拒绝。改成整轮一个值。
5. **`preflight-malformed-id` 断言错了对象。** 形状不合法的仓库 id 在动作这一层不是
   400：动作的职责是对着世界给结论，而世界里没有这条记录 → 201 +
   `BACKUP_REPOSITORY_NOT_FOUND`。形状校验属于镜像路由（那里 400）。
6. **上一轮的定义让第三段连锁失败。** 脚本只清工作目录，没清代理库，于是"仓库名字已被
   占用"的 400 出现在创建那一步，后面 20 多条跟着红。改成每次用工作目录下的代理库、
   并清掉 known_hosts 与远端仓库目录。

另外两处为了让证据可读：阻塞原因用 `ensure_ascii=False` 输出；失败信息里保留第一次见到
的那把钥匙。

结果
----
| 机器 | 结果 | 用时 | 说明 |
|---|---|---|---|
| Ubuntu 22.04.5（VM-A，`-accel tcg,thread=multi`） | **PASS 133 / FAIL 0 / NOT RUN 5** | 05:03:19Z → 05:07:37Z（4 分 18 秒） | restic 0.12.1、OpenSSH 8.9p1 |
| Ubuntu 24.04.5（VM-B，`-accel tcg,thread=single`） | **PASS 133 / FAIL 0 / NOT RUN 5** | 05:08:24Z → 05:19:34Z（11 分 10 秒） | restic 0.16.4、OpenSSH 9.6p1 |

两台都 `EXIT=0`，逐节断言数**完全相同**：§0 2、§1 4、§2 3、§3 25、§4 7、§5 19、§6 11、
§7 13、§8 5、§9 5、§10 6、§10b 6、§11 13、§12 7、§13 6、证据卫生 1（合计 133），
另 5 条 NOT RUN 在文末单列。两台的两次运行都是同一份二进制（sha256 见 env.txt）与同一份
脚本（sha256 见 env.txt）。

选几条最有说服力的真机事实（原文都在 `logs/` 与 `payloads/` 里）：

- **仓库 id 是读回来的，两台都独立核对过**：22.04 `5b2ac62df10e3348d82aefb12176f4dc69fd02cc462816b14334911aa0f41ec2`，
  24.04 `6044228bc53c74a6316f232f6b5416ccac76789842be823a646b061b55320eae`；脚本自己再跑一次
  `restic cat config` 读到的与代理报出的逐字节相同（`independent-cat-config.stderr` 为空，
  两边都非空才比）。两台的 `repo_version` 分别是 1 与 2——restic 0.16 默认写 version 2，
  我们只如实转述读到的值，不假设它一定是 1。
- **known_hosts 是我们写的**：`/var/lib/upanel-agent/backup/known_hosts/1a8546568fdcbcdc`，
  0600；初始化**之前**它不存在（§6 断言），初始化之后才出现，指纹就是登记的那把
  （22.04 `SHA256:cN3tDj8D…`、24.04 `SHA256:R7OLp576…`）。
- **容量是读出来的**：22.04 总量 41429692416 B / 可用 36277723136 B；24.04 总量
  40483942400 B / 可用 36232720384 B；初始化前后两次读数都在，前后差异只来自仓库自己的
  少量元数据。
- **已存在的仓库不许覆盖**：预检 `allowed=false` + `BACKUP_REPOSITORY_EXISTS`，
  初始化**没有再跑**（状态查询显示 `initialized=true` 且 id 不变）。
- **口令错说口令错**：`仓库口令不正确（RESTIC 拒绝了这次解密）`，`repo_id` 为空。
- **连不上的主机说"不知道"**：`initialized=null`、`capacity.supported=false`、
  `host_key_error` 是 `dial tcp 127.0.0.1:9: connect: connection refused`。
- **私有路径**：`root` 连镜像路由 → 403 `拒绝对端 UID 不在允许范围`（`uid: 0`、
  `allowed_uid: 997` 见两台 agent.log）。

宿主侧检查（`scripts/check.sh`，两台虚机空闲时跑的）
---------------------------------------------------
**PASS 13 / FAIL 0 / NOT RUN 0**：`go-build`、`go-vet`、`go-test`、`go-build-linux`、
`go-vet-linux`、`gofmt`、`node-version`、web 的 typecheck/lint/test/build/smoke、
openapi lint。web 侧 **45 文件 820 用例全过**；node `v22.23.3`（`/nvm/v22.23.3` 前置到
PATH——这台宿主的默认 node 是 v24.15.0，`check.sh` 的版本闸门会直接 FAIL，不是跳过）。
原文在 `logs/host-check.txt`。

NOT RUN（本阶段不该假装通过的项目）
-----------------------------------
1. **备份与恢复**（写快照、还原、"恢复后校验一致"）——P12.2/P12.3 才有实现，BACKUP-01
   的"备份后恢复"要到那时才有对象。
2. **备份跑到一半网络断开**——本轮只验"连不上/认证失败时预检拒绝"，中途掉线属于 P12.2
   的备份执行。
3. **存储满**——真填满验收机的文件系统会连 apt/journal 一起弄坏，挂 loop 设备要动
   `/etc/fstab`；该判决由 `internal/backup` 的 `capacityVerdict` 单测覆盖。
4. **目标路径存在但不是目录**——与文件模块的用例重复，由评估逻辑单测覆盖。
5. **两个 restic 版本行为一致**——22.04 是 0.12.1、24.04 是 0.16.4，脚本把两个版本的
   原文记进 `payloads/*/restic-versions.txt`，只证明这两台机器上的这两条路径成立。

另外三条本阶段根本没有的东西，一并写在这里免得被读成已验证：**Web 侧 `/api/v1/backup/*`
与备份页面**（P12.2—P12.5）、**定时任务**（P12.4/P12.5）、**`go test -race`**（宿主与两台
VM 都没有启用 cgo，`-race requires cgo`）。

环境事故
--------
① **加速后端**。这台宿主的 WHPX 对 24.04 不稳定：客户机内核随机
`Attempted to kill init! exitcode=0x0000008b`（2026-10-21 一天 12 次、10-24 又到第 10 次）。
换 TCG 又踩到多线程 TCG 起不来（`IO-APIC + timer doesn't work!`，`-smp 1` 与 `-cpu max`
都不管用），最终 **`-accel tcg,thread=single`** 才能稳定跑完整轮。代价是慢一个量级：
VM-A 4 分 18 秒、VM-B 11 分 10 秒——**缺陷 C 就是被这份慢照出来的**，在快机器上它只会
偶尔出现，在那台机器上必然出现。三条失败尝试的串口原文与决策记在
`docs/TEST_ENVIRONMENT.md` §8.9。
② **并发纪律**。两台虚机不同时跑验收，宿主也不并发跑 `go test`、不并发 ssh 探测。
违反过一次（两台同时跑），一台在 CPU 争抢下握手超时——那轮的结论作废重跑，同时也暴露了
缺陷 B。现在每一台都是**独占**跑完一整轮。
③ **构建可复现**。加 `-buildvcs=false` 之后同一份源码两次构建逐字节相同；不带它时 Go 会
把工作树的 VCS 状态嵌进二进制，工作树脏的时候同一个提交也构不出同一个文件（P12.1 第一轮
的两个二进制就是这样对不上的）。本轮三个二进制两台逐字节相同，sha256 与自报 commit 都记在
`env.txt`。

证据
----
    env.txt                        两台环境、二进制指纹与自报版本、加速后端、环境事故
    logs/2204-run.log              22.04 完整验收输出（186 行，13 节 + 证据卫生 + 汇总）
    logs/2404-run.log              24.04 完整验收输出（186 行，逐节断言数与 22.04 相同）
    logs/2204-results.txt          逐条 PASS/FAIL 记录（133 PASS / 0 FAIL / 5 NOT RUN）
    logs/2404-results.txt
    logs/2204-agent.log            代理日志（含"已启用备份仓库动作"、root 被拒的原文）
    logs/2404-agent.log
    logs/2204-dial-timeout-round.txt  缺陷 B 的现场原文与"为什么判成抖动"的依据
    logs/system-packages.txt       restic / openssh-server 的 apt-cache policy 与自报版本
    logs/host-check.txt            宿主 scripts/check.sh 输出（PASS 13 / FAIL 0）
    payloads/2204/*.json           22.04 每一步的原始响应（42 个）
    payloads/2404/*.json           24.04 每一步的原始响应（42 个）
    payloads/*/restic-versions.txt 两个 restic 版本的自报原文

`logs/2404-console-panic.log` 是 2026-10-23 那一轮 24.04 的 guest panic 串口原文：那一轮的
**证据**已整份作废并重跑（panic 丢掉页缓存，10 个 payload 是 0 字节），但这个文件本身是真实
的环境记录，保留下来给 `docs/TEST_ENVIRONMENT.md` §8.9 的结论做凭据。
