# 本地启动与安装

本文档对应 P01.4：把前端构建产物嵌入 Go Web 二进制，并给出开发、试运行与 systemd 安装步骤。
只描述**当前阶段真实可用**的流程；尚未实现的阶段能力会明确标注。

前置约定（来自 PLAN.md 第 3、4 节，CONTRACTS.md 第 3 节）：

- 面板只监听回环地址：production 用 `127.0.0.1:9443`，mock 只能用 `127.0.0.1:18881`。
- 前端与 API 同源（同一个二进制、同一个端口），没有跨域代理，也不配置 CORS。
- 生产机**不需要** Node.js：前端产物在构建期嵌入 `upanel-web`。
- Web 以非 root 用户 `upanel-web` 运行；特权动作由 `upanel-agent` 承担（P02 起）。
- 数据库固定为 SQLite，Linux 默认 `/var/lib/upanel/upanel.db`。

---

## 1. 工具链

| 工具 | 版本 | 说明 |
| --- | --- | --- |
| Go | 1.27.3 | 见 `go.mod`；`modernc.org/sqlite` 不需要 cgo |
| Node.js | 22.23.3 | 精确版本见 `web/.nvmrc`；更高版本未经锁定 |
| npm | 随 Node | 依赖版本固定在 `web/package-lock.json` |

```sh
# 示例：nvm 环境
nvm use            # 读取 web/.nvmrc
go version
node -v
```

未安装 `web/node_modules` 时先装依赖：

```sh
npm --prefix web ci --ignore-scripts
```

冒烟用的浏览器不在仓库里，换机需要单独安装一次：

```sh
npx playwright install chromium     # 版本需与 @playwright/test 匹配
```

---

## 2. 构建二进制（含前端）

```sh
sh scripts/build.sh
```

这个脚本做四件事，顺序不能换：

1. `npm --prefix web run build` 生成 `web/dist/`；
2. 把 `web/dist/` 同步到 `internal/webui/dist/`；
3. `go build` 三个二进制到 `bin/`：`upanel-web`、`upanel-agent`、`upanelctl`；
4. 打印 `bin/` 文件列表和嵌入的 `index.html` 字节数。

为什么要先同步再构建：`internal/webui/webui.go` 使用 `//go:embed all:dist`，
资源在**编译期**打进二进制。只改前端不重新构建 Go，二进制里的页面仍是旧的。
仓库里只提交 `internal/webui/dist/.keep` 占位，因此直接 `go build` 也能通过，
但那样的二进制**没有任何页面**，启动时会打警告并对页面请求返回 503（见第 6 节）。

交叉编译 Linux 二进制（在非 Linux 开发机上构建 `bin/linux-amd64/`）：

```sh
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 sh -c '
  for c in upanel-web upanel-agent upanelctl; do
    go build -trimpath -o bin/linux-amd64/$c ./cmd/$c || exit 1
  done'
```

交叉编译前同样要先跑 `scripts/build.sh`（或至少让 `internal/webui/dist/` 里有最新产物）。

---

## 3. 运行（不经 systemd）

```sh
# mock：进程内演示数据，不读取也不写入真实系统
bin/upanel-web --mode mock --db "$PWD/bin/dev-mock.db"

# production：默认 127.0.0.1:9443
bin/upanel-web --mode production --db "$PWD/bin/dev.db"
```

打开 <http://127.0.0.1:18881/>（mock）或 <http://127.0.0.1:9443/>（production）。
`--db` 必须是绝对路径：非 Linux 平台没有默认数据库位置，避免误写系统目录。

常用参数（`bin/upanel-web -h` 可看全量）：

| 参数 | 作用 |
| --- | --- |
| `--config <路径>` | 指定 JSON 配置文件；显式给出时文件必须存在且合法 |
| `--mode production\|mock` | 覆盖 `web.mode` |
| `--listen host:port` | 覆盖 `web.listen_address` |
| `--db <绝对路径>` | 覆盖 `database.path` |
| `--log-level`、`--log-format` | 覆盖日志级别与格式 |
| `--version` | 打印版本后退出 |

数据库迁移不需要单独执行：`upanel-web` 启动时打开库并应用未执行的迁移，
失败即拒绝启动。需要单独迁移或检查时：

```sh
bin/upanelctl migrate --config /etc/upanel/upanel.json
bin/upanelctl check-config --config /etc/upanel/upanel.json
```

配置示例见 `deploy/upanel.example.json`（production）与 `deploy/upanel.mock.example.json`（mock）。

### 3.1 首次创建管理员（P02.1 起）

面板**没有默认账号，也没有默认密码**。账号只能由本机命令行创建，Web 侧不提供注册入口：

```sh
bin/upanelctl init-admin --config /etc/upanel/upanel.json --username admin --display-name "管理员"
```

- 交互终端下密码输入两次且不回显；非终端（脚本、CI）时从 stdin 读两行，两行必须一致。
- 没有任何 `--password` 参数：密码不进入命令行、shell 历史和进程列表。
- 库里一旦已存在账号，命令直接拒绝（退出码非 0），不会覆盖或重置。
- 密码策略见 CONTRACTS §2；口令写入前先用 Argon2id 哈希，数据库里只有 PHC 字符串。

账号为空时 `upanel-web` 仍会启动，但日志里会有 `WARN 尚未创建任何账号，登录不可用`，登录接口一律返回
`401 UNAUTHENTICATED`。登录、会话、登出的接口细节见 `api/openapi.yaml` 的 `/auth/*`。

网页登录在**非 loopback** 的明文 HTTP 下不可用：会话 cookie 带 `Secure`，浏览器不会在明文连接上回传它
（DECISIONS Q28）。本机 `http://127.0.0.1:9443` 上 Chrome/Edge 把 loopback 当作安全上下文，允许 `Secure`
cookie；Firefox/Safari 尚未实测。**目前还没有登录页面**（前端仍是 P01 的骨架，认证与账号管理都只有
接口），所以浏览器侧没有任何证据：登录、再认证与账号管理目前只能通过 API/脚本操作。命令行验证用
`tests/smoke/p02-1-auth-smoke.sh` 与 `tests/smoke/p02-2-account-smoke.sh`。

---

## 4. 前端开发

```sh
npm --prefix web run dev        # Vite dev server, http://127.0.0.1:5173
VITE_UPANEL_MODE=mock npm --prefix web run dev   # 让页面显示 mock 横幅
```

开发服务器**只提供前端**：它没有配置到后端的代理，`/api/v1/*` 请求会返回 404。
这是刻意的——面板要求同源，避免形成“开发期跨域也能跑、上线才发现不行”的假象。
要联调真实 API，两种做法：

1. 构建后用二进制访问（推荐，也是最接近生产的路径）：

   ```sh
   sh scripts/build.sh && bin/upanel-web --mode mock --db "$PWD/bin/dev-mock.db"
   ```

2. 在 `web/vite.config.ts` 里临时加 `server.proxy` 指向 `http://127.0.0.1:18881`
   （只在本机调试时用，不要提交）。

运行模式来源优先级：服务端注入的 `window.__UPANEL_MODE__` → 构建变量 `VITE_UPANEL_MODE`
→ 兜底 `production`。二进制运行永远走服务端注入，dev server 走构建变量。

---

## 5. 检查与冒烟

```sh
sh scripts/check.sh                      # 全量：Go/前端/脚本基线检查
npm --prefix web run typecheck           # vue-tsc --noEmit
npm --prefix web run lint                # ESLint
npm --prefix web test                    # Vitest（node 环境）
npm --prefix web run smoke               # 真实 Chromium：先构建，再对 vite preview 跑
npm --prefix web run smoke:mock          # 同上，mock 产物 + mock 预期
```

对已运行的服务做冒烟（不起 preview，直接访问二进制）：

```sh
node web/scripts/smoke.mjs --base http://127.0.0.1:18881 --expect mock
node web/scripts/smoke.mjs --base http://127.0.0.1:9443 --expect production
```

冒烟会检查页面可加载、运行模式注入、同源 API 信封、控制台无报错；失败退出码非 0。

认证链路（P02.1）用脚本冒烟，不需要浏览器：

```sh
sh tests/smoke/p02-1-auth-smoke.sh
```

它自建临时库、临时端口，覆盖 `init-admin`（含重复创建拒绝）、登录入口检查、cookie 属性、会话与
CSRF 轮换、登出立即失效、失败限速 429，最后检查日志里没有密码和 CSRF token。输出 `PASS n / FAIL n`，
有失败即非 0 退出。

认证的 Linux 验收脚本是 `tests/integration/p02-1-acceptance.sh`（58 项断言），**只在 Ubuntu 验收 VM
里以非 root 用户运行**，宿主上跑它没有意义（其中的库文件权限、进程 uid 断言依赖 POSIX 语义）：

```sh
# VM 内
BIN_DIR=/tmp/upanel-bin sh tests/integration/p02-1-acceptance.sh
```

账号管理（P02.2）同样两级证据：

```sh
sh tests/smoke/p02-2-account-smoke.sh          # 宿主：81 项断言，临时库/临时端口，不需要浏览器
BIN_DIR=/tmp/upanel-bin sh tests/integration/p02-2-acceptance.sh   # VM 内：103 项断言，非 root
```

冒烟覆盖 admin/viewer 边界、账号增删改查、脱敏 DTO 与 `limit`/`cursor` 翻页、乐观并发 `version`、
自我提权/自我禁用/自删拒绝、5 分钟再认证窗口、改密与删号立刻踢掉目标会话、审计四类事件
（含“表只追加，UPDATE/DELETE 被数据库触发器拒绝”），以及库与日志里没有明文口令。

代理边界（P02.3）**没有宿主冒烟**：它要求 root、真实账号与 Unix socket 权限，只在 Ubuntu 验收 VM
里跑，并且**必须用 root 执行**（脚本会自己装 systemd 单元、按不同 UID 发起调用、最后清理）：

```sh
# VM 内
sudo BIN_DIR=/tmp/upanel-bin sh tests/integration/p02-3-agent-acceptance.sh   # 84 项断言
```

它准备两个额外账号来把“拒绝”分成两层：`upanel-probe`（与 `upanel-web` 同组，能连上 socket，
用来证明 403 来自 SO_PEERCRED 的 UID 判断）与 `nobody`（连连接都建立不了，用来证明文件权限也生效）。
脚本还会在 VM 内核上原生执行两个测试二进制（`agent.test`、`syscmd.test`），因为验收 VM 里没有
Go 工具链（DECISIONS Q9）。

秘密存储（P02.4）同样只在 VM 内跑，**必须 root**（要按不同 UID 验证“Web 读不到库与密钥”，
还要安装单元、生成密钥）：

```sh
# VM 内
sudo BIN_DIR=/tmp/upanel-bin sh tests/integration/p02-4-agent-secrets-acceptance.sh   # 87 项断言
```

它覆盖密钥生命周期（无密钥拒绝启动、`-init-key` 输出指纹且绝不覆盖）、代理库与目录的归属/模式、
`POST /internal/v1/secrets` 的正反面（含“没有任何读取明文的路由”）、密文落盘（库里搜不到明文、
nonce 每条独立）、重启持久性、日志无明文，并在 VM 内核上原生执行 `agentstore.test` 与 `agent.test`。
其中的“10 分钟挂钟到期销毁”是 **NOT RUN**（到期逻辑用短 TTL 在真实 SQLite + 真实 AEAD 上执行）。

预检（P03.1）**必须 root**（要建 socket、chown、以 `upanel-web` 身份起 Web），并额外传入
`-acceptance-actions` 才能拿到验收动作表：

```sh
# VM 内
sudo BIN_DIR=/tmp/upanel-bin sh tests/integration/p03-1-preflight-acceptance.sh   # 93 项断言
```

它验证：socket 与目录权限、Web 子进程 uid 是 `upanel-web`（root 名下没有 Web 进程）、
预检 200 的完整字段、响应不含 `state_version`/`agent_ref`、`expires_at` 距今 115—121 秒且代理库
`expires_at - created_at = 120000` ms、两端 `user_id`/`session_hash`/`params_hash`/`state_version`
逐一相等、带空白与紧凑参数得到同一哈希、`acceptance.ping` 无短语可取消（对照项）、
短语形如 `PROBE K7M2QX` 且在 `agent.db`/`-wal`/`-shm` 里搜不到明文、Web 库唯一含 `phrase` 的列是
`requires_phrase`、未登录/非 admin/缺 CSRF 都不落库、未登记动作与夹带字段都 400、`GET` 405、
代理停止后 503 `AGENT_UNAVAILABLE`。预检本身不执行动作——消费它的是 `POST /api/v1/jobs`（P03.2）。

任务提交（P03.2）同样**必须 root**，同样需要 `-acceptance-actions`：

```sh
# VM 内
sudo BIN_DIR=/tmp/upanel-bin sh tests/integration/p03-2-jobs-acceptance.sh
```

它按 `== 0` 环境 … `== 11` 终态/日志卫生共 12 个实质段落（`== 12` 汇总）验证：先预检再
`POST /api/v1/jobs`（`Idempotency-Key` 必填）拿到 `job_id`/`status`/
`events_url`、预检被一次性消费（`consumed_job_id` 指向该 job）、幂等键 TTL≥24 小时、
任务在代理侧 `succeeded` 且 `progress_percent` 为 `null`（不编进度）；同一幂等键提交十次只产生
一个 job、一次代理执行、一条 `accepted` 事件；同键不同 body → 409、换一个键用同一预检 →
409 `PREFLIGHT_STALE`；过期预检被拒；缺再认证 → 403 `REAUTH_REQUIRED`；把 Web 进程杀掉再起来，
任务仍然完成、`operation_id` 不变；把代理 `kill -9`，任务变 `interrupted` 且 `finished_at` 有值、
**不会自动重跑**；`acceptance.probe` 不可取消（取消请求 409）而 `acceptance.ping` 可取消
（202 → `canceled` + `cancellation_requested`）；未登录/非 admin/缺 CSRF/夹带未知字段的拒绝路径。
**这个脚本已在两台验收 VM 上跑过并通过**：Ubuntu 24.04.5 与 22.04.5 各 **通过 101 项 / 失败 0 项**
（`docs/evidence/P03/2026-10-20-p03.2/`）。跑之前先清 `/var/tmp/upanel-accept-*`；脚本以 root
运行（要建 socket 目录、以 `upanel-web` 身份模拟 Web 进程、`kill -9` 代理并重启）。
如果它在你的环境里挂了，先看 `== 0` 段的**登录是否 200**——登录失败会让后面每个 `PF_*` 变量
都没值，脚本会立刻早退（exit 1）而不是继续报一堆无关错误。

事件流与任务中心（P03.3）同样**必须 root**：

```sh
# VM 内
sudo BIN_DIR=/tmp/upanel-bin sh tests/integration/p03-3-events-acceptance.sh   # 73 项断言
```

它验证：SSE 响应头（`text/event-stream`、`no-store`、`X-Accel-Buffering: no`）、首帧
`: connected`、`id:` 单调递增、帧格式与四种 `event:`、`agent_kind`/`agent_seq` 证明事件确实
来自代理镜像、带 `Last-Event-ID` 续传不会重发也不会多执行、坏 `Last-Event-ID` 400、
未登录 401 / viewer 403 / 别人的任务 404、日志分页（`limit`、`next_cursor`、坏 cursor 400、
`limit` 0 与 201 都 400）、日志环被裁后先发 `event: gap`、`job_events` 的追加式触发器
（改状态被拒、删状态事件被拒、删日志允许、终态不能回退）、每用户 4 条连接上限（第 5 条 429，
关闭后配额释放）、关闭事件流不影响任务继续执行、取消语义（202 / 409），以及日志里没有
CSRF 令牌与口令。
**两台验收 VM 上均已通过（24.04.5 与 22.04.5 各 73 项 / 0 失败，见
`docs/evidence/P03/2026-10-20-p03.3/`）。**

重启对账与“结果未知”（P03.4）同样**必须 root**（脚本要 `kill -9` 代理、重建代理库）：

```sh
# VM 内
sudo BIN_DIR=/tmp/upanel-bin sh tests/integration/p03-4-reconcile-acceptance.sh   # 78 项断言
```

它验证：生产动作登记表为空（不带 `-acceptance-actions` 时 `acceptance.probe` 预检被拒）、
同一个幂等键十次提交与并发提交都只产生一个 job 与一次代理执行、同键不同请求 409、
**Web 重启后重放同一个键拿回原 job 而不重新执行**、提交没到达（代理从未见过这条操作）由
**启动扫描**判为 `failed`/`unconfirmed`/`AGENT_UNAVAILABLE`、代理 `kill -9` 后遗留操作被代理
标 `interrupted` 且 Web 同步为 `interrupted`（`cancellable=0`，不重放）、**代理侧记录被删**时
结果为 `interrupted`/`unknown_result`/`OPERATION_INTERRUPTED` 而不是 `failed`、终态不回退、
没有“重试”接口、启动对账在日志里留痕且日志不含口令与 CSRF。脚本用 `--max-time 60` 兜住每个
请求，卡住的响应会超时而不是把整轮验收挂死。

完整回归用一条命令跑完（**按脚本头声明的身份调度**，跑前清理残留进程与 `/run/upanel`）：

```sh
# VM 内（upanel 用户即可，root 也行）
BIN_DIR=/tmp/upanel-bin sh tests/integration/run-all.sh
BIN_DIR=/tmp/upanel-bin sh tests/integration/run-all.sh p03-3-events    # 只跑指定几个
sudo BIN_DIR=/tmp/upanel-bin sh tests/run-native-tests.sh               # VM 内核上的 Go 测试二进制
LOG_DIR=/var/tmp/my-logs BIN_DIR=/tmp/upanel-bin sh tests/integration/run-all.sh   # 换日志目录
```

> 逐脚本日志默认写到 `/var/tmp/upanel-logs/`（目录由脚本自己创建并 `chmod 0777`，可用 `LOG_DIR`
> 覆盖），**不再写 `/tmp/upanel-accept-<name>.log`**：`/tmp` 有粘滞位，上一次以 root 建的同名
> 文件会让这次的 `runuser`/`setpriv` 进程写不进去，于是脚本全跑通了、重定向却失败、退出码变成 2，
> 看起来像环境不满足（24.04 上真发生过两次）。

> 身份不要凭感觉给：`p01-1`/`p01-2`/`p01-4`/`p02-1`/`p02-2` 必须以**非 root** 运行
> （脚本自己用 sudo 做特权部分，并断言 Web 不以 root 跑、库文件属主是运行者），
> `p02-3`/`p02-4`/`p03-1`/`p03-2`/`p03-3`/`p03-4` 必须以 **root** 运行（代理只能在 root 下启动）。
> 用 root 跑 `p01-2` 不会报“身份错”，而是会在“数据库目录不可写必须拒绝启动”那条上一直等
> ——root 绕过目录权限，进程永远不退（DECISIONS Q71）。

手工验证账号管理（先用 `init-admin` 建号，再登录取 CSRF 令牌）：

```sh
# 1) 登录：拿 cookie 与一次性 CSRF 令牌
curl -s -D /tmp/h -o /tmp/b -X POST http://127.0.0.1:9443/api/v1/auth/login   -H 'Content-Type: application/json' -H 'X-UPanel-Request: 1'   -H 'Origin: http://127.0.0.1:9443'   -d '{"username":"admin","password":"<你的口令>"}'
# 2) 从 /tmp/h 取 __Host-upanel_session，从 /tmp/b 取 data.csrf_token
# 3) 创建 viewer（初始口令不回显；至少 12 字符）
curl -s -X POST http://127.0.0.1:9443/api/v1/users   -H 'Content-Type: application/json' -H 'Origin: http://127.0.0.1:9443'   -H "Cookie: __Host-upanel_session=<session>" -H "X-CSRF-Token: <csrf>"   -d '{"username":"alice","display_name":"Alice","role":"viewer","initial_password":"<初始口令>"}'
```

改动别人的账号/口令或删除账号之前，先 `POST /api/v1/auth/reauth`（body 只要 `password`）拿到 5 分钟
窗口，否则返回 403 `REAUTH_REQUIRED`。管理员不能改自己的角色或启用状态，也不能删除自己——这是
防止自我提权与自我锁死，不是权限不足。

---

## 6. 生产安装（Ubuntu + systemd）

```sh
# 1) 二进制与配置
sudo install -m 0755 bin/linux-amd64/upanel-web /usr/local/bin/upanel-web
sudo install -m 0755 bin/linux-amd64/upanelctl  /usr/local/bin/upanelctl
sudo install -d -m 0755 /etc/upanel
sudo install -m 0644 deploy/upanel.example.json /etc/upanel/upanel.json

# 2) 服务用户（非 root，无 sudo，不能登录）
sudo useradd --system --home-dir /var/lib/upanel --no-create-home \
  --shell /usr/sbin/nologin upanel-web
sudo install -d -m 0750 -o upanel-web -g upanel-web /var/lib/upanel

# 3) 初始化数据库（以服务用户身份执行，文件归属才对）
sudo -u upanel-web /usr/local/bin/upanelctl migrate --config /etc/upanel/upanel.json

# 4) systemd 单元
sudo install -m 0644 deploy/systemd/upanel-web.service /etc/systemd/system/upanel-web.service
sudo systemctl daemon-reload
sudo systemctl enable --now upanel-web
sudo systemctl status upanel-web --no-pager
```

访问方式（面板不对外暴露，TLS 在 P10 接入，当前 9443 是明文回环）：

```sh
ssh -N -L 9443:127.0.0.1:9443 user@server
# 本机浏览器打开 http://127.0.0.1:9443/
```

单元要点：

- `User=upanel-web`，且 `NoNewPrivileges=true`、`ProtectSystem=strict` 等加固项已开启；
- `StateDirectory=upanel` 由 systemd 创建并设置归属，对应 `/var/lib/upanel`；
- `ExecStartPre=upanelctl check-config` 配置不合法时直接失败，不会带病启动；
- `/run/upanel/` 属于特权代理（`upanel-agent`）：由代理单元的 `RuntimeDirectory=upanel` 创建，
  模式 **0755 root:root**（可穿越、不可写）；Web 单元不创建它，代理也**不会**替它创建配置文件目录。

代理（`upanel-agent`，P02.3 起）单独安装，仍以 root 运行：

```sh
# 5) 特权代理
sudo install -m 0755 bin/linux-amd64/upanel-agent /usr/local/lib/upanel/upanel-agent
sudo install -m 0644 deploy/systemd/upanel-agent.service /etc/systemd/system/upanel-agent.service

# 5a) 代理数据目录（0700 root）+ 首次生成秘密加密密钥（P02.4 起）
sudo install -d -m 0700 -o root -g root /var/lib/upanel-agent
sudo /usr/local/lib/upanel/upanel-agent -init-key     # 打印路径与指纹，绝不覆盖已有密钥

sudo systemctl daemon-reload
sudo systemctl enable --now upanel-agent
ls -l /run/upanel/agent.sock        # srw-rw---- root upanel-web
sudo ls -l /etc/upanel/agent.key    # -rw------- root root
sudo ls -ld /var/lib/upanel-agent   # drwx------ root root
```

**密钥备份**：`/etc/upanel/agent.key` 是代理侧所有秘密（SMB 口令、备份仓库口令、
TLS 私钥等）的唯一解密材料。它**只存在这一台机器上**，不写库、不进备份包、
也无法从数据库反推。换机或重装前必须单独离线备份；密钥丢失等于已存秘密全部永久失效
（数据库里只剩密文），密钥泄露等于秘密泄露。`-init-key` 已存在就失败退出，
这是有意的：覆盖密钥会让所有已存秘密静默变成垃圾。

代理的 `agent.socket_group`（默认与 `agent.web_user` 同名，即 `upanel-web`）决定 socket 的属组，
因此**创建 Web 服务账号是安装代理的前置条件**，否则代理拒绝启动并说明“账号不存在”。
`agent.web_user` 必须是账号名而不是数字 UID——写数字会在配置校验阶段直接以退出码 2 拒绝。
代理启动时会自检 `/run/upanel` 与 socket：属主不是 root、目录组/其他可写、或目录无法被 socket 组
穿越，都会拒绝启动而不是带着坏权限运行。

代理的数据目录由单元里的 `StateDirectory=upanel-agent`（`StateDirectoryMode=0700`）创建并归属 root；
数据库文件 `/var/lib/upanel-agent/agent.db` 与 WAL/SHM 由代理打开时自建，并收紧到 `0600 root:root`。
代理在启动时**再校验一次**目录与库文件的属主/权限，不合规就拒绝启动——所以“把库文件搬到 Web 可写的
目录里”不会静悄悄地工作，而是明确失败。Web 账号读不到 `agent.db`、`agent.key`，也列不出这个目录。

验证运行身份：

```sh
systemctl show -p User --value upanel-web          # upanel-web
ps -o user= -p "$(systemctl show -p MainPID --value upanel-web)"   # upanel-web
```

---

## 7. 故障排查

| 现象 | 原因 | 处理 |
| --- | --- | --- |
| 页面返回 503，日志 `二进制未嵌入前端资源` | 编译时 `internal/webui/dist/` 只有 `.keep` | 跑 `sh scripts/build.sh` 重新构建 |
| 启动报 `mock 模式只能绑定 127.0.0.1:18881` | mock 被指到别的地址 | mock 只允许固定地址；要对外服务请用 production |
| 启动报 `P10 接入 TLS 之前 production 只能绑定回环地址` | production 配了 `0.0.0.0` 等非回环地址 | 保持 `127.0.0.1:9443`，用 SSH 隧道访问 |
| 启动报 `production 模式仅支持 Linux`，退出码 1 | 在 macOS/Windows 上跑 production | 开发机用 `--mode mock`；production 只在 Linux |
| 启动报 `数据库打开失败` | 目录不存在、不可写，或路径不是绝对路径 | 建目录并授权给 `upanel-web`；`--db` 用绝对路径 |
| `/api/v1/...` 返回 404 JSON `NOT_FOUND` | 该接口尚未实现或路径写错 | 按 CONTRACTS.md 核对路径；未知 API 不会回退到页面 |
| 深链接刷新 200 但页面空白 | 资源 404（CSP 或构建不完整） | 看浏览器控制台；确认 `assets/*` 能 200 加载 |
| 端口被占用 | 前一个进程没退出 | `ss -ltnp \| grep 9443`，结束占用进程后重启 |
| 代理退出码 1，日志 `必须以 root 运行` | 手工/错误单元里以普通用户启动了 `upanel-agent` | 代理只能由 root 启动；用 `deploy/systemd/upanel-agent.service`（`User=root`） |
| 代理退出码 1，日志 `无法解析 Web 账号` / `组` | `agent.web_user` 或 `agent.socket_group` 指向不存在的账号 | 先建 `upanel-web`（§6 第 2 步），账号名不能写成数字 UID |
| 代理退出码 1，日志提到 `/run/upanel` 权限不合规 | 目录不是 root 属主、组/其他可写，或 socket 组无法穿越 | 不要手工 chmod：修正目录属主与模式（0755 root:root），或让 systemd 的 `RuntimeDirectory` 重建 |
| 代理退出码 1，日志 `拒绝覆盖` | `/run/upanel/agent.sock` 位置被普通文件占着 | 删掉那个文件（脚本会刻意保留它，证明不覆盖）后重启 |
| 调代理接口返回 403 `FORBIDDEN` | 调用方 UID 不是 `agent.web_user`（root 也会被拒） | 用 `sudo -u upanel-web` 或对应的服务身份调用；root 的专用路由当前为空 |
| 调代理接口拿到的是连接失败（curl 返回 000） | 文件权限层就过不去：不在 socket 组，或目录不可穿越 | 把调用账号加入 `upanel-web` 组；这是第二层防线，设计如此 |
| 代理启动失败，日志提到 `agent.key` 不存在并提供 `-init-key` 提示 | 首次安装还没生成密钥（代理**不会**自动生成，避免密钥随安装路径漂移） | `sudo /usr/local/lib/upanel/upanel-agent -init-key`，然后启动服务 |
| `-init-key` 报“已存在，拒绝覆盖” | 密钥已生成过 | 这是保护，不是故障；核对 `-init-key` 输出的指纹与备份是否一致，需要轮换请走单独流程 |
| 代理启动失败，日志提到 `/var/lib/upanel-agent` 属主或权限不合规 | 目录不是 root 属主，或组/其他可写 | `install -d -m 0700 -o root -g root /var/lib/upanel-agent`；库文件由代理自己建，不要提前 `touch` |
| 秘密解密失败 / 引用失效，日志 `密钥版本` 或 AEAD 错误 | `agent.key` 被替换或从别的机器拷来 | 换回原密钥；原密钥丢失时这些秘密无法恢复，只能重新录入 |
| `journalctl -u upanel-agent` 里看到 `[redacted]` 而不是你排查的字段值 | 字段名命中脱敏词表（含 secret/token/password/key_file…） | 不是故障：脱敏是固定策略，`key_path`/`key_fingerprint`/`ref` 这类元数据名可见 |

日志：`journalctl -u upanel-web -f`（systemd）或直接看进程 stderr。
