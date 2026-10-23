# P10.2 验收结果 — 配置生成、原子写入、`nginx -t` 门禁、reload 与失败恢复

证据类型：**Linux VM integration**（一次性 Ubuntu 24.04.5 虚拟机，root 起代理、uid 999 起 Web）
执行时间：2026-10-23（UTC，虚拟机时钟）
脚本：`tests/integration/p10-2-proxy-apply-acceptance.sh`
环境：见 `env.txt`；宿主检查见 `check.txt`；单元测试见 `native-tests.txt`

---

## 1. 结论

| 项 | 数据 |
|---|---|
| 最终一遍（`run.log`） | **PASS=153 / FAIL=0 / NOT RUN=7**（退出码 0） |
| 真机抓出的产品缺陷 | **1 条**：预检偶发 6.7–9.3 秒，其中一次顶到 10 秒上限变成 503 "代理不可用"（Q290） |
| 写入真的到了 Nginx | 托管文件 `conf.d/upanel-<domain>.conf`、`0644`、首行托管标记；`nginx -t` 由脚本自己再跑一遍也通过；worker PID 换了一代（reload 真的发生）；`Host:` 头穿过 nginx 拿到上游页面 |
| 幂等重放 | 同一份定义再应用一次：`file_state=unchanged`、`reload=skipped_unchanged`、文件字节不变、**备份数量不变**、worker 没换 |
| 门禁与回滚 | `p102_no_such_directive on;` 这种"结构合法、nginx 不认"的夹具走到写入 → `nginx -t` 失败 → 逐字节回滚 → 任务 `failed`/`COMMAND_FAILED` → worker 没换、站点仍在服务、面板照常健康 |
| `applied` 是推理不是列 | 手工改文件 → `stale` → 再应用 → `applied`，被改坏的那一份进了备份；库迁移仍是第 9 版（P10.2 **没有**加 `applied_config_hash` 列，Q286） |
| 改名 | 旧域名文件被移除、旧域名不再指向上游、机器上只剩一份托管配置 |
| 停用 / 删除记录 | 停用 → `file_state=removed`、`served=false`、文件移除（仍留备份）；配置还在时 `DELETE` 409 `DEPENDENCY_BLOCKED` 且记录没被悄悄删掉，移除配置后 `200` |
| basic auth | 账号文件在 `upanel-auth/` 下、`0640`（属组给 worker）、内容只有 apr1 哈希；不带口令 401、正确口令 200、错误口令 401 —— **真 nginx** 认这个哈希 |
| 只写自己的文件 | `/etc/nginx` 里非面板文件的内容摘要前后一致；托管文件只有 `upanel-*` |
| 面板与站点互不牵连 | nginx 配置树坏着的时候，面板健康检查仍 200、仍能登录（托管站点只监听 80，管理端不在这条流水线上） |
| 预检耗时 | 本轮 16 次预检：14 次 5–433ms，**最慢一次 1698ms**（紧随一次 `proxy.apply` 执行之后；同一时段 `GET /jobs/:id` 也花了 610ms、`PATCH /proxies/:id` 313ms）。代理侧本轮 **0 条**慢请求日志，即这 1.7 秒不是又退回到现场问 systemd，而是 2 vCPU 上的瞬时时延；预算 10000ms，§13b 的 2 秒门禁通过 |

**这一遍 `run.log` 对应哪份代码**：`peer.go`/`main.go`/`htpasswd.go` 的注释在上一遍跑完之后改过
（Q214→Q290 的引用、两处被证伪的解释按实测改写），所以重新交叉编译了三个二进制、重新拷进 VM、
**整份验收重跑了一遍**。`env.txt` 里的 sha256 就是提交进仓库这版源码编出来的那三个二进制
（`check.txt` 末尾有同样三个哈希，宿主侧重编一遍对齐）。

## 2. 范围（做了什么、没做什么）

已做（STAGES §12 第 2 条的全整内容）：配置生成（纯函数、确定性 apr1 盐）、原子写入、
`nginx -t` 门禁、失败回滚与回滚复验、`nginx -s reload`、`applied` 四态推导、改名与停用的
文件清理、账号文件与权限、删除记录的孤儿配置门禁、`proxy.apply` 参数服务端展开。

**没做**（写在这里而不是假装通过）：443 与证书（P10.3）、管理面板 HTTPS 与 SSH 隧道文档（P10.4）、
WebSocket 握手实测（测试上游不支持 Upgrade，只验配置里 `Upgrade`/`Connection` 成对出现）、
浏览器渲染、真实 DNS 解析、密码哈希的跨机器标定（Q292）。

## 3. 这一轮改了什么产品代码

- `internal/proxies/{render,configfs,apply,applyrun,htpasswd,httperr}.go` —— 渲染、原子写入、
  门禁、回滚、备份、apr1。
- `internal/api/{preflightexpand,proxies_applied}.go` —— `proxy.apply` 参数展开、`applied` 推导。
- `internal/agent/{proxyaction,nginxquery}.go` —— 代理侧的动作登记与事实查询。
- `cmd/upanel-agent/main.go` —— 接线；其中**两处与 Q290 有关**：单元状态改读服务快照、
  `TargetExists` 只读发现快照。
- `internal/agent/peer.go` —— 超过 500ms 的请求记一条（`path`/`status`/`duration_ms`/`request_id`）。
  这条日志不是锦上添花：Q290 的定位就是卡在"代理侧一个字都没有"上。

## 4. 执行的检查

| 检查 | 结果 |
|---|---|
| `gofmt -l ./cmd ./internal` | 无输出（`check.txt`） |
| `go vet ./...` | 通过（`check.txt`） |
| `go test ./...` | 34 个包全绿（`native-tests.txt`） |
| 真机脚本 | 153 PASS / 0 FAIL / 7 NOT RUN（`run.log`） |

## 5. 定位过程（Q290）— 写清楚，因为里面有一次错误的"反证"

1. 症状：§7 附近三轮运行里，预检偶发 6.7 / 8.5 / 9.3 秒；其中一次顶到 `agentclient.DefaultTimeout`
   （10s）→ 503 `AGENT_UNAVAILABLE`。
2. 第一次猜错：以为 `TargetExists` 触发了一次全量应用发现。改成只读快照后重跑，**没有变快**。
   这条改动本身是成立的（告警不该买采集），但它不是原因——注释里已按事实改写。
3. 第二次猜错（更值得记）：以为 systemd 会把插入的属性查询排在后台大查询后面，于是用 shell
   起了一个"大查询"再计时，得到 0.168s，写下"systemd 不排队"。**那个后台查询根本没跑起来**
   （事后 `pgrep systemctl` 是 0），所以这个"反证"是空的。
4. 真正的复现（`.tmp/probe2`，输出见 `probe-unit-timing.txt`）：在进程内、后台服务采集一直跑着的
   条件下比较两条路。现场问 `systemctl show nginx.service`：**6.389 秒**，其后几次 104–124ms
   （掉在两段之间的空档）；同一个探针里读服务快照：113–252ms，平均 170ms。根因就是原来的解释，
   只是当时没有被正确地测出来。
5. 修法与永久设施：单元状态读 `servicesCollector.Snapshot()`（仅在还没采过时现场问一次，快照被
   截断时明确报错）；`internal/agent/peer.go` 记慢请求；验收 §13b 把"预检不许被代理拖慢"变成
   每次都会回答的断言。
6. 顺带发现（不在 P10.2 修，Q292）：`argon2id`（m=128 MiB、t=3、p=2）在这台 2 vCPU 虚机上单次
   1.4–3.9 秒（`argon2-timing.txt`），与 DECISIONS Q23 记的 100–300ms 差一个数量级；登录因此
   稳定 4–6 秒（`run.log` 的 note: 本轮 4281ms；前几轮 4895ms / 5744ms；同轮的 `POST /auth/reauth`
   3330ms 也是同一次哈希成本）。

## 6. 脚本自己的毛病（都改在脚本里，没动产品代码迁就）

- §14 收尾原来直接 `run_apply`，但"停用"是两步（先 `enabled=false` 改记录，再应用）——只应用
  等于幂等重放，收尾永远清不干净。修：先 PATCH 再 apply，并断言文件"确实没了"。
- §10 删除记录用了裸 `DELETE` 期待 204；契约是 `?version=N` 且返回 **200**。顺带暴露一个真实
  时间差：Web 侧 nginx 事实是 5 秒快照，所以"移除配置后立刻删记录"可能仍看到 409——那不是缺陷，
  是缓存；脚本改成轮询。
- **§13b 第一次跑就误报**：`web.log` 是追加写，里面还留着上一轮那条 9312ms，于是"本轮最慢预检"
  读出了旧数据。修：每轮开跑前截断两份日志（Q291）。
- 最初把 §7 写成一节，想用"未闭合的括号"验证回滚——那种树连判断层都读不了，预检就拦下了，
  永远走不到门禁。拆成 §7（判断层能看出来的坏树）与 §7b（判断层看不出来的坏树）。

## 7. NOT RUN（本阶段不验证的东西）

见 `run.log` 末尾 7 条：443 与证书、面板 HTTPS/SSH 隧道文档、`panel_port` 的二次校验（由单测覆盖）、
WebSocket 握手实测、浏览器渲染、真实 DNS、密码哈希参数的跨机器标定。

## 8. 机器状态

收尾后：托管配置 0 份、账号文件目录空、`nginx -t` 通过、Web 库已删除。代理库
（`/var/lib/upanel-agent/agent.db`）按设计保留，里面是历次验收累积的记录；P10.2 没有给 Web 库
新增任何表（迁移仍是第 9 版）。
