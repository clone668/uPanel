# P15.4 真机验收：部署页的整条写链路（2026-10-28）

这一轮补的是 P15 唯一还空着的那一格：**从部署页发起的 `deployment.apply` / `update` / `remove`
一次都没跑过**（动作本身在 P15.2/P15.3 用脚本在真机验过）。CONTRACTS §8.9 当时写的是
“页面链路要等 P16 的统一验收轮（或单独一轮 VM 验收）才谈得上验收通过”。

结论：**在这轮专门起的验收 VM 上跑通了 —— 43 条断言全过**，但它不是白过的：
跑之前有一个**阻断性缺陷**（页面根本没法完成写操作），跑的时候又抓到两个。

---

## 1. 环境与字节

| 项 | 值 |
|---|---|
| 机器 | Ubuntu 22.04.5 LTS / 5.15.0-198-generic / x86_64 / 2 vCPU / 3.9 GiB / 单线程 TCG |
| 容器运行时 | docker.io 29.1.3-0ubuntu3~22.04.2、docker-compose-v2 2.40.3+ds1-0ubuntu1~22.04.1（apt 装的，安装前这台机器上没有 docker） |
| 面板 | 发布包 `upanel-1.0.0-linux-amd64.tar.gz`，`sh deploy/scripts/install.sh --source . --password-stdin`（口令两行走 stdin，没进 argv、没进日志） |
| 面板字节（最终） | amd64 `3f3b40c11247ea1b15aba024ddb3571fada3a69291465eb7e43242b2e9d6daf0`，44154866 字节；arm64 `33fc5add33befedfeb4429988b35bb1846be6cf1e0f778ae02bdfa0b3e643748`（`build_date: 2026-10-28T05:23:36Z`） |
| 字节的来龙去脉 | 四次打包：`a2d7b934…`（只含再认证的门）→ `af6d9102…`（加上两处页面修复）→ `b1d913e7…`（把 C12/支持矩阵等随包文档写进发布包）→ **`3f3b40c1…`（Q514：会话层不再把“浏览器放弃的请求”记成 500）**。**读数只对跑它的那份字节成立**（Q510）：第二、三次跑分别在 `af6d9102…` 与 `b1d913e7…` 上，第四次跑在最终交付字节 `3f3b40c1…` 上；七相位也在最终字节上重跑过（`P16/2026-10-28-p16.2-final/`） |
| 包内自检 | `sha256sum -c SHA256SUMS` **21/21 OK**；28 个条目 = 22 文件 + 6 目录；6 个可执行文件 0755（3 个二进制 + 3 个脚本） |
| VERSION | `1.0.0` / `commit: 8e52b36-dirty` / `build_date: 2026-10-28T04:36:36Z` / `arch: amd64` |
| 安装结果 | 13 步全过，13 份文档落 `/usr/local/share/doc/upanel`，证书指纹 `89c632a8…0b39`（825 天），面板 `127.0.0.1:9443`，web 进程 uid 不是 root（`upanel-web`） |
| 客户端 | 宿主上的 headless Chromium（Playwright），经 `ssh -N -L 9443:127.0.0.1:9443` 打这台 VM |

`facts-package.txt` 是最终字节的清单（含权限，取自 `tar tzvf`），`facts-vm.txt` 是跑完之后的机器现场。
**权限必须从 `tar tzvf` 读**：Windows/msys 上解开之后 `stat` 看不到无扩展名文件的执行位（Q510 记过这个坑）。

## 2. 结果

| | 断言 | 结果 |
|---|---|---|
| 第四次（**最终交付字节** `3f3b40c1…`，Q514 修复之后） | 43 | **PASS 43 / FAIL 0** |
| 第三次（`b1d913e7…`，随包文档写完） | 43 | **PASS 43 / FAIL 0** |
| 第二次（`af6d9102…`，两处页面修复之后） | 43 | **PASS 43 / FAIL 0** |
| 第一次（`a2d7b934…`，修复前） | 43 | PASS 31 / FAIL 12 |

`smoke-run2-final.log`、`smoke-run3-final-bytes.log`、`smoke-run4-final-bytes.log` 是三次全绿（逐条断言），
`smoke-run1-superseded.log` 是第一次（留着比对，12 条失败的成因在第 4 节逐条写清）。
后两次跑在冻结字节上：按 Q510 的规矩，读数只对跑它的那份字节成立，所以每重打一次就在新字节上再跑一遍，
不在旧读数上收口。驱动脚本是 `web/scripts/deploy-smoke.mjs`，四次调用参数相同
（`UPANEL_SMOKE_TIMEOUT_SCALE=2`，只放大等待，不放宽断言）。

第四次的三次写入（`jobs-timeline-run4.txt`，受理→完成的墙钟）：`deployment.apply` 58 秒、
`deployment.update` 62 秒、`deployment.remove` 12 秒，三次都 `outcome: verified`；
镜像从 `nginx:1.27-alpine` 换到 `nginx:1.28-alpine` 后容器确实换成新镜像。
面板侧 98 条请求：91 条 200、3 条 202、1 条预期的 401、**2 条 499**（浏览器 abort，Q514 之后如实记成
“客户端已断开”）、1 条 500（`/api/v1/apps` 的同类取消，读查询层本轮没改，见第 6 节）。
读查询最慢四条 9427 / 8559 / 8533 / 8177 ms，都在 10 秒预算内返回 200（C12 的边界，这次没顶破）。

跑之前这一页是**点不通的**，见第 3 节。

## 3. 跑之前修掉的阻断性缺陷：页面没有再认证的门

`deployment.apply` / `update` / `remove` 三条动作在契约里都是 **R=是**（`RequiresReauth: true`），
而**登录不开这扇门**——只有 `POST /auth/reauth` 会把窗口推到 `now+5min`。部署页原先把
“这一步要求再认证”只当文案写在确认卡上，于是真机上点确认**必然**拿到 403 `REAUTH_REQUIRED`，
页面链路永远走不完。单测里没人点过按钮，所以这个缺陷一直活着（这正是 P15.4 存在的理由）。

修法（`web/src/views/DeploymentsView.vue`）：预检说 `requires_reauth` 而
`session.reauthExpiresAt` 不在有效期内时，把面板密码（+二次验证码，开了 TOTP 才显示）发往
`/auth/reauth`，成功后 `session.restore()` 再自动接着把刚才那一步提交做完。密码只走这一条路径，
不进任务参数、不进日志。

真机上的证据（`web-requests.txt` + `jobs-timeline.txt`）：

```
04:38:36.123  POST /api/v1/operations/preflight  200   155ms
04:38:39.316  POST /api/v1/auth/reauth           200  3082ms
04:38:39.529  POST /api/v1/jobs                  202   180ms   → 后面换镜像、删除都没再要一次密码
```

三次预检全部 `requires_reauth: true`、`requires_phrase: false`、`blockers: 0`；
三次任务全部 `outcome: verified`（`deployment.apply` 26 秒 / `deployment.update` 31 秒 /
`deployment.remove` 18 秒，单线程 TCG）。5 分钟窗口内后两次写入没有再验一次。

## 4. 第一次跑出来的 12 条失败，逐个定性

| 条数 | 断言 | 定性 |
|---|---|---|
| 1 | 空状态说清“这台机器上还没有受管部署” | **产品缺陷**：`StatePanel` 的 empty 分支只把 `description` 交给 `NEmpty`，**页面传的 `title` 被丢掉了**——所有页面给空状态起的名字都看不见，只剩“暂无数据”。已在组件里修（标题落地为 `<p>`），并补了一条回归用例 |
| 8 | 详情六连 + 换镜像后的两条 | **脚本时序错**：详情卡先渲染“正在读取…”，我对着那三个字做了六次断言。改成等面板真的读出内容（`waitDetail`）后再断言 |
| 1 | 删除后列表回到空状态 | 同一空状态缺陷的后果 + 同一时序问题 |
| 1 | 除预期外没有 4xx/5xx | 两件事：**产品缺陷**（删完还去读那份部署的现场，注定 404——`afterSuccess` 里 `reloadDetail()` 与 `detailName=null` 顺序反了）+ **观察项**（见第 5 节） |
| 1 | 没有控制台错误 | **脚本的错**：把登录前那次 `/auth/session` 的 401 也算成了错误（那是预期探测） |

于是这一轮实际修了三处：`StatePanel` 空状态标题、`DeploymentsView.afterSuccess` 删除分支
（不再明知故问去读一个已经删掉的部署）、以及第 3 节那个阻断性缺陷。
第二次跑删掉的 404 没有再出现，空状态标题也回到页面上。

## 5. 观察项：任务执行中，只读查询可能顶到 10 秒预算

第一次跑抓到两条 503 `AGENT_UNAVAILABLE`（`GET /deployments/managed` 与
`/deployments/managed/:name`），duration 都是 10008ms / 10025ms；同一时刻代理侧日志是
`status: 200, duration_ms: 10020 / 10037`——也就是说**代理其实答了，只是答在 10 秒之后**，
而 `agentclient.DefaultTimeout = 10s` 先掐断了它。触发时机是首次拉镜像那一轮（重 I/O + 单线程 TCG）。

第二次跑（镜像已在本地）就没有再出现：`web-requests.txt` 里 57 条请求全是 200/202，
读查询最长 6470ms。

**没有改预算**，理由与记录见 `docs/DECISIONS.md` Q512：
慢路径各有各的调用上限（`packages` 45 秒、`schedule` 30 秒、`backup` 100 秒）是既有先例，
但部署读这次是在**单线程 TCG** 上顶到 10 秒，真实的 2 核以上机器里不该发生；
若真机上复现，照先例给这条读路径一个自己的上限，而不是把“代理不可用”当成结论。
这一条已写进 `KNOWN_LIMITATIONS.md`，不算通过。

## 6. 观察项二：浏览器放弃的请求曾被记成“服务端故障”和“代理不可用”

第三次跑（`b1d913e7…`）登录成功后，SPA 并发拉 `/system/summary`、`/apps`、`/storage/devices`，
紧接着导航到部署页，浏览器把还在飞的请求 abort 掉。客机日志里留下的是：

```
ERROR 会话校验失败      error="auth: 读取会话失败: context canceled"   path=/api/v1/system/summary
ERROR 会话校验失败      error="auth: 读取会话失败: context canceled"   path=/api/v1/apps
WARN  读取设备列表失败：代理不可用  error="agentclient: 代理不可用: … context canceled"
```

对外分别是 500 `INTERNAL_ERROR`、500 `INTERNAL_ERROR`、503 `AGENT_UNAVAILABLE`——**没有人收到这三个响应**，
但日志里留下的是“服务端坏了”“代理不可用”。同一个根因（`context canceled`）被两层分别翻错。

排除过三种别的原因（原始记录见 `client-canceled-misclassified.txt`）：不是进程崩了（`NRestarts=0`，
同期无 panic/recover）；不是代理预热（重启代理后 t+1s/t+2s 的 503 走的是“代理真不在”那条分支，
t+3s 起 45 次探测全 200）；不是稳定故障（同一会话连打 12 次全 200）。

**会话校验那一层当场修了（Q514）**：`requireSession` 先用 `clientGone()` 分辨取消（只看
`context.Canceled`，不看 `DeadlineExceeded`——后者是服务端自己的超时，需要人看），取消时记
`WARN 客户端已断开，会话校验中止` 并写 499（nginx 惯例；没人接这个响应，只为让访问日志说实话）。
两条回归用例：`TestClientAbortedRequestIsNotReportedAsServerFault`、`TestClientGoneRecognisesCancellationOnly`。

**第四次跑（最终字节 `3f3b40c1…`）在真机上验到了修复的效果**：

```
05:40:47.641  WARN  客户端已断开，会话校验中止  error="auth: 读取会话失败: context canceled"  499
05:40:47.683  WARN  客户端已断开，会话校验中止  error="auth: 读取会话失败: context canceled"  499
grep -c '会话校验失败' → 0
```

同一个窗口里还剩一条同类：`ERROR 读取应用列表失败 error="apps: 查询应用列表失败: context canceled"`
→ 500（`web-requests-run4.txt` 里那条 `/api/v1/apps`）。**读查询那一层本轮没动**：判定散在
`agentclient` 与十个领域包里，各自 `fmt.Errorf("%w: %v", ErrAgentUnavailable, err)`，逐包改要各自确认，
属于另一轮。记进 DECISIONS Q514 与 KNOWN_LIMITATIONS C13。

它不影响 43 条断言（页面链路一条都没碰到），但会让以后读日志的人去追一个不存在的故障。

## 7. 这一轮**没有**证明什么

- **“数据卷与业务目录一个字节都不动”只有文案证据**。这次新建的部署没有挂载卷，删除后
  `/etc/upanel/deploy` 是空目录、`docker ps -a` 为空、`docker compose ls` 为空、
  `/var/lib/upanel` 只剩数据库与 WAL——但没有一份真实的卷数据可以证明“没动”。带挂载的删除
  仍然是脚本级（P15.3）证据。
- **24.04 上没跑**：整轮只在 22.04.5 上，页面渲染差异没测。
- **多部署现场没测**：200 份截断、`skipped` 非空、`config_drift` 真实发生这三种，仍只有单测。
- **掉电语义仍是 NOT RUN**（P16.4 的结论不变）。
- 单线程 TCG 下的耗时（26/31/18 秒）不代表真机性能。

## 8. 复现

```sh
# 1) 起机器（宿主侧）
node /h/Code/vms/upanel-2204/serve-seed.js /h/Code/vms/upanel-2204/seed 8900
powershell -NoProfile -ExecutionPolicy Bypass -File /h/Code/vms/launch-vm.ps1 \
  -Name upanel-2204 -SshPort 2223 -SeedPort 8900 -MemoryMB 4096 -Cpus 2 -Accel tcg

# 2) VM 里装 docker 与面板（口令走 stdin）
sudo apt-get install -y docker.io docker-compose-v2
scp dist/upanel-1.0.0-linux-amd64.tar.gz upanel@127.0.0.1:/var/tmp/ -P 2223
tar xzf /var/tmp/upanel-1.0.0-linux-amd64.tar.gz -C /var/tmp
cd /var/tmp/upanel-1.0.0-linux-amd64 && sudo sh deploy/scripts/install.sh --source . --password-stdin

# 3) 宿主上开隧道并跑页面验收（口令只走环境变量）
ssh -N -L 9443:127.0.0.1:9443 upanel@127.0.0.1 -p 2223 &
UPANEL_SMOKE_USER=admin UPANEL_SMOKE_PASS=… UPANEL_SMOKE_TIMEOUT_SCALE=2 \
  node web/scripts/deploy-smoke.mjs --base https://127.0.0.1:9443 --name upanel-smoke
```

`UPANEL_SMOKE_TIMEOUT_SCALE=2` 只放大等待时间（单线程 TCG 上登录一次要 4—20 秒），
**不放宽任何断言**；第二次跑用的是同一个值。
第三次跑之前，这台机器还先跑过一遍 P16.2 七相位（见 `docs/evidence/P16/2026-10-28-p16.2-refreeze/`），
purge 相位之后机器是干净的，才在上面装 docker 与面板——所以第三次的起点没有上一轮的残留。

## 9. 文件清单

| 文件 | 是什么 |
|---|---|
| `smoke-run4-final-bytes.log` | **最终交付字节 `3f3b40c1…` 上的那一次：43/43**，逐条断言输出 |
| `smoke-run3-final-bytes.log` | 第三次（`b1d913e7…`）：43/43；Q514 修复之后被第四次取代 |
| `smoke-run2-final.log` | 第二次（`af6d9102…`，两处页面修复之后）：43/43 |
| `smoke-run1-superseded.log` | 第一次（修复前）：31/43，12 条失败的原始输出 |
| `jobs-timeline-run4.txt` / `jobs-timeline-run3.txt` | 代理侧时间线（预检/受理/完成），第四次与第三次 |
| `jobs-timeline.txt` | 第二轮的同一份东西（对照用） |
| `web-requests-run4.txt` / `web-requests-run3.txt` | 面板侧请求与耗时（98 条 / 104 条），含 499 与那条 500 |
| `web-requests.txt` | 第二轮的面板侧 57 条请求（200/202），含那次 `POST /auth/reauth` |
| `client-canceled-misclassified.txt` | 第 6 节那几条的原始日志、排除项与复现实验结果 |
| `facts-package.txt` | 最终字节：两份 sha256、包内 VERSION、21/21 校验、`tar tzvf` 全清单与权限 |
| `facts-vm.txt` | 跑完之后的机器现场：docker 版本、空目录、无容器、无残留单元、13 份文档、web 非 root |
