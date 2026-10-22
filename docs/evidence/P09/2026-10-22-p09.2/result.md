# P09.2 验收结果 — 本机端口/服务/容器发现、管理员确认候选、健康检查与 SSRF 约束

证据类型：**Linux VM integration**（一次性 Ubuntu 24.04.5 虚拟机，root 起代理、uid 999 起 Web）
执行时间：2026-10-22（UTC）
脚本：`tests/integration/p09-2-app-discovery-acceptance.sh`
环境：见 `env.txt`；宿主检查见 `check.txt`；单元测试见 `native-tests.txt`

---

## 1. 结论

| 项 | 数据 |
|---|---|
| 最终一遍（第五遍，`run.log`） | **PASS=120 / FAIL=0 / NOT RUN=1**（退出码 0） |
| 唯一 NOT RUN | 「卷离线禁止启动受管理依赖应用」——P09.2 没有启停接口，属于 P09.4（脚本里那一条断言的是“按钮确实不存在”，不是“保护生效”） |
| 真机发现的产品缺陷 | **3 条**（Q258 pid 1 代持的 socket 单元重复候选、Q259 docker-proxy 让同一个容器在候选里出现两次、Q260 来源没采成却报“绑定对象不在了”），全部修完重验 |
| 容器相关的三条验收条款 | 装上 docker 29.1.3 + `alpine:3.20` 夹具后**真的验了**：容器候选与发布端口、running 但 HTTP 500 明确异常、重建 ID 变化后旧绑定要求重新确认 |
| SSRF 边界 | 探测地址永远是 `127.0.0.1`；`169.254.169.254`、外部主机、完整 URL 在 `target_id` 这个形状里写不出来（第 3、4 节逐条试过） |
| 三种 unknown | 没启用 / 从没探过 / 探过但没结论，接口输出与库里的记录都能分开读 |
| 库/日志里的秘密 | Web/代理日志里没有云元数据地址的请求，也没有口令；本轮没有秘密通道 |

这一轮**改的都是产品代码**，不是把断言改松：三条缺陷都是真机跑出来、能说清“谁会被这句话误导”的
问题。第一遍跑出来的失败里有 5 条是脚本自己的毛病（见 §5），也逐条写清了。

## 2. 范围（做了什么、没做什么）

已做（STAGES §11 第 2 条的全部内容）：

- **发现** `internal/discovery`：`/proc/net/tcp{,6}` 的 LISTEN（`st=0A`）→ inode → `/proc/<pid>/fd`
  → cgroup v2 `0::` → `.service` 单元；与 P05.1 的服务快照同一个采集者（不开第二条 `systemctl`
  通道）；Docker 走 `docker ps -a --no-trunc --format`。三路来源各自带 `status`/`error`/`count`，
  全部失败是 `error`、部分失败是 `partial`。候选上限 500，超了 `truncated=true`。
- **探测目标由代理自己算**：`target_id` 只有 `local_…` / `cntr_…` 两种形状，地址永远由代理拼成
  `http(s)://127.0.0.1:<port><path>`，客户端 `Proxy: nil`、不跟随重定向、TLS 校验开、正文读满
  64 KiB 即丢。调用方没有任何地方能塞进一个 host——不是被过滤，是结构上不存在。
- **候选确认 = `POST /apps`**：没有第二个“确认”接口。写入时若带 `enabled=true` 的 `health_check`，
  经代理确认目标在当前发现结果里：代理不可达 → 503 `AGENT_UNAVAILABLE` 一个字不写；目标不在 →
  422 且 `details.target_id`。`PATCH` 只在请求体带了 `health_check` 时才检查。
- **健康检查**：调度在 Web（`internal/apphealth`，5 秒一拍、一轮 60 秒、每应用 30—300 秒节流、
  最小间隔 5 秒），执行在代理；结论落 `app_health_results`（迁移 `0008`，一行一个应用），
  定义仍在 `apps.health_json`。没发出请求时 `address`/`url` 是空串、`status_code` 是 `NULL`。
- **候选与绑定合并**：只按身份匹配（单元名 / 完整 64 位容器 ID）；合并结果的 `missing_bindings`
  分开写原因；对应来源这一轮没采成时不进失效列表。
- **接口**：`GET /apps/discovery`（admin）、`GET /apps/:id/health`（V）。openapi 与
  `docs/CONTRACTS.md` §8.2、`docs/DECISIONS.md` Q251—Q260 同步。

没做（留给后续阶段，本轮用断言反过来证明没做）：

- `/apps/:id/logs`、`/containers*`、`/images`、`/container-networks`、`/container-volumes`、
  `/deployments*`（P09.3—P09.4）。
- 任何启停：`capabilities.start/stop/restart/logs` 全为 `false`（第 9 节钉住了“按钮不存在”）。
- 前端页面（P09.5）：本轮验的是页面会读到的载荷。

## 3. 逐节覆盖（第五遍真机运行，`run.log`）

| 节 | PASS | FAIL |
|---|---|---|
| 0. 环境与安全守卫 | 5 | 0 |
| 1. 配置与启动（代理 root、Web 非 root） | 9 | 0 |
| 2. 三个只绑回环的被测服务 + 两个容器夹具 | 4 | 0 |
| 3. 发现：候选来自本机，权限与代理边界都成立 | 17 | 0 |
| 3b. 容器候选（真 docker） | 8 | 0 |
| 4. 确认候选 = 创建应用（没有第二个“确认”接口） | 12 | 0 |
| 5. 健康探测：三种 unknown 分得开 | 6 | 0 |
| 6. 真实结论：200 → ok，500 → error，不回应 → error（超时） | 15 | 0 |
| 7. 服务停掉之后：结论变成 error，而不是停在 ok | 5 | 0 |
| 8. 代理不可用：503 而不是空列表，unknown 而不是 error | 16 | 0 |
| 8b. 容器：running 但 500 明确异常；重建后要求重新确认；docker 停掉不误报 | 17 | 0 |
| 9. 本轮不验的（明确标出来） | 1 | 0 |
| 10. 收尾不变量 | 6 | 0 |

几条值得单独指出的（都在 `run.log` 里）：

- `候选之间没有重复的监听（地址+端口+协议）` → 这一条是 Q258/Q259 两条缺陷的守卫。修之前
  22 端口在候选里出现两次（sshd 与 pid 1），19487 也出现两次（容器与 docker.service）。
- `容器候选的身份是完整 64 位容器 ID（不是名字、不是短 ID）` → 直接拿 `docker inspect` 的 `.Id`
  比对，不是看响应像不像。
- `重建后容器 ID 真的变了（名字照旧）` + `应用记录里的绑定仍是旧容器 ID` + `旧绑定要求重新确认`
  → 三件事一起成立才算“没有自动改绑”。
- `docker 采不到时，容器应用不被报成“容器不在了”` → 真停掉 `docker.socket`/`docker.service`，
  等快照追上来（发现缓存 15 秒）后再看；来源如实是 `error`，总状态 `partial`。
- `停服务之前结论是 ok` / `服务停掉后结论变成 error` / `checked_at 前进了` → 目标从发现结果里
  消失时记 `error` 而不是 `unknown`（Q257），否则一个挂掉的站点会永远显示“暂不确定”。
- `超时要说明白（原因里提到超时）` → 只接受连接不回应的服务，原因是“请求超时：…”，不是
  “不可用”。
- `库里的 unknown 记录 address 是空串而不是 127.0.0.1` → 没发出请求就不许留地址。
- `监听 19482 的 Web 进程到最后都不是 root（uid=999）` → 用 `ss -lptn` 找真正监听的 pid。

## 4. 独立证据（不经过接口）

| 事实 | 怎么独立核验 |
|---|---|
| 迁移到第 8 版 | `SELECT MAX(version) FROM schema_migrations` = 8 |
| 一行一个应用 | `SELECT app_id, COUNT(*) FROM app_health_results GROUP BY app_id` 无 >1 |
| 探测地址只指向回环 | `SELECT COUNT(*) FROM app_health_results WHERE address <> '127.0.0.1' AND address <> ''` = 0 |
| URL 只有回环前缀 | `url NOT LIKE 'http://127.0.0.1:%' AND url NOT LIKE 'https://127.0.0.1:%' AND url <> ''` = 0 |
| 没有云元数据地址 | 全表的 `address`/`url`/`target_id` 都不含 `169.254.169.254` |
| 没发请求的记录 | `http_state='unknown'` 的行 `address=''` 且 `status_code IS NULL` |
| 容器身份 | `docker inspect -f '{{.Id}}'` 与候选的 `container_id` 逐字符相等 |
| 绑定没被偷偷改 | 重建后 `GET /apps/:id` 的 `binding.container_id` 仍是旧 ID |
| 候选来自真机 | 自建监听出现在候选里、没人监听的端口不出现；容器端口与 `docker ps` 的发布端口一致 |

## 5. 这一轮改了什么

### 产品代码（4 处，全部由真机跑出来的失败触发）

| # | 现象 | 根因 | 改法 | 决策 |
|---|---|---|---|---|
| 1 | `服务停掉后结论变成 error` 时好时坏 | 目标一旦离开 15 秒发现缓存，`Probe` 就把“它不在”写成 `unknown`；同一个物理事实（站点没了）因缓存新鲜度给出两种结论 | 目标缺席按快照状态分开判：来源没采成 → `unknown`；看成了但没有 → `error` | Q257 |
| 2 | 目标缺席/容器端口没发布这两类记录里带着 `address=127.0.0.1`、空 `url` | 地址在构造时就填了，可这两类**根本没发出请求** | 地址与 URL 挪到“真的要发请求”那一刻才填 | Q257 |
| 3 | 候选里 22 端口出现两次（`local_sshd-22` 与 `local_systemd-22`） | `.socket` 单元的套接字由 pid 1 持有，守护进程也持有同一个端口 | 同一个（地址, 端口, 协议）只归有人认领的进程；pid 1 独占的仍是候选，原因改写为“代持的 socket 单元” | Q258 |
| 4 | 有 docker 时 19487 出现两次（`cntr_…` 与 `local_docker`） | docker-proxy 跑在 `docker.service` 的 cgroup 里，抑制逻辑只作用在“裸监听”分支，先按单元分类就把它认成了 docker.service 的监听 | 把“摘掉发布端口上的监听”挪到单元分类**之前**，只摘发布端口那几条（dockerd 自己的 2375 留着） | Q259 |
| 5 | 停掉 dockerd，那个容器应用立刻出现在 `missing_bindings`（“可能被重建过，需要重新确认”） | 合并只看候选表，不看来源是否采成 | `missing_bindings` 以“对应来源 `status=ok`”为前提 | Q260 |

新增单元用例：`TestProbeMissingTargetVerdict`、`TestProbeContainerNoPublishedPort`、
`TestCollectDropsShadowedSocketActivation`、`TestCollectDropsDockerProxyInsideDockerUnit`、
`TestAppDiscoverySkipsIssuesWhenSourceFailed`。

### 验收脚本（这一轮扩了两节）

- 新增 `3b`（容器候选与发布端口）与 `8b`（容器 running 但 500、重建后要求重新确认、docker 停掉
  不误报），并给容器夹具装了 `alpine:3.20`；没有 docker / 拉不到镜像时它们仍是 **NOT RUN**，
  不改成“通过”。
- 新增“候选之间没有重复的监听（地址+端口+协议）”这条不变量断言——Q258/Q259 就是它抓出来的。

### 脚本自己的毛病（5 条，第一遍到第四遍之间修掉）

| 现象 | 原因 | 处理 |
|---|---|---|
| 第一次跑就 `端口 19482 空闲: 已被占用` | 上一轮遗留的 `upanel-web`/`upanel-agent` 还在跑，`/run/upanel/agent.sock` 也没清 | 手工清干净后重跑；脚本本身的环境守卫是对的（它就该拒绝在脏环境里跑） |
| 装了新二进制后 `候选之间没有重复的监听` 仍然是 4 | 改了代码没重装到虚拟机，跑的还是旧二进制 | 重新交叉编译 + `install` 后对比 `sha256sum` 再跑 |
| `docker 守护进程停掉后 docker 来源如实变成 unavailable` 实际是 `error` | 断言把状态词写死了；停掉 `docker.socket` 后套接字文件有时还在，`docker ps` 失败就是 `error` | 断言改成“不是 ok”：`unavailable`（没命令/没守护进程）与 `error`（读失败）都算没采到，同时在注释里写清两者怎么来的 |
| `容器 running 但 HTTP 500` 一开始被标 NOT RUN | 测试机没有 docker（前几遍） | 装上 docker 29.1.3 + `alpine:3.20`，真的起容器验；没有 docker 的机器上仍旧 NOT RUN |
| `docker-proxy` 的 8080/9000 注释与夹具端口对不上 | 注释写错（`0x2328` 是 9000，不是 8080） | 只影响可读性，未改行为；这一轮新增的用例把端口写成十进制常量 |

**关于留存**：这一轮跑了六遍（`93/0`（还没有 docker）→ `115/1`（装了容器夹具但虚拟机里是旧二进制）
→ `114/2` → `116/0` → `117/1` → `120/0`），只留了最后一遍的 `run.log`。中间几遍的红都是上面
表里那几条，逐条结论写在 §5，不留在仓库里。

## 6. 明确没验（NOT RUN，不假装）

| 项 | 为什么没验 |
|---|---|
| 卷离线禁止启动受管理依赖应用 | P09.2 没有任何启停接口（`capabilities.start=false`），这条属于 P09.4 的生命周期动作 |
| 前端页面渲染与交互 | P09.5。本轮只验页面会拿到的载荷；DOM/交互由 vitest 覆盖 |
| 多用户同时确认同一候选、并发写冲突 | 单测覆盖（`internal/apps`、`internal/api`），真机没跑 |
| Docker 之外的其他容器运行时（podman 等） | 发现层只实现对 `docker` CLI 的读取；podman 不在 P09 范围 |
| 真实浏览器里的 CSRF/SameSite 行为 | 用 curl + cookie jar + `X-CSRF-Token` 验的协议行为，浏览器行为留给前端验收 |
| 长时间运行的调度漂移（数小时） | 只验了“到点会探、间隔内不探、代理掉线写 unknown”，没有跑小时级 soak |

另外一条**观察**（不是 NOT RUN，是行为记录）：代理刚启动后的第一个采集周期内，服务快照还没有，
`systemd` 来源会如实报“服务状态尚未完成第一次采集”，这一小段时间里单元候选为零（`partial`）。
这是 P05.1 就定下的语义（还没采过 ≠ 本机没有服务），Q260 保证了它不会被误读成“绑定失效”。

## 7. 复现方式

```sh
# 宿主：交叉编译三个二进制并拷到虚拟机
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -o /tmp/p092bin/ ./cmd/...
scp -P 2222 /tmp/p092bin/upanel-{agent,web,ctl} upanel@127.0.0.1:/tmp/
scp -P 2222 tests/integration/p09-2-app-discovery-acceptance.sh upanel@127.0.0.1:/tmp/p09-2.sh

# 虚拟机：装进去
ssh -p 2222 upanel@127.0.0.1 'sudo install -o root -g root -m 0755 /tmp/upanel-{agent,ctl,web} /var/tmp/upanel-bin/'

# 虚拟机：容器夹具需要 docker + alpine（本轮实地装的：docker.io 29.1.3、alpine:3.20）
ssh -p 2222 upanel@127.0.0.1 'sudo DEBIAN_FRONTEND=noninteractive apt-get install -y docker.io && sudo docker pull alpine:3.20'

# 虚拟机：跑（脚本自己起代理/Web、三个回环测试服务与两个容器，收尾时删容器）
ssh -p 2222 upanel@127.0.0.1 'sudo BIN_DIR=/var/tmp/upanel-bin bash /tmp/p09-2.sh'
```

脚本只读 `/proc`、只对 `127.0.0.1` 发请求；唯一会改动系统状态的是容器夹具（起/删两个容器）
和 8b 节里**临时停一下 `docker.socket`/`docker.service`**（随后立刻启动回来）。不碰磁盘、不碰
网络配置、不碰任何业务服务。
