# P02.3 证据：代理 Unix socket、UID 边界、固定登记表、结构化只读查询、命令执行器

日期：2026-10-20
子任务：P02.3（STAGES.md §4；CONTRACTS.md §15；PLAN.md §4）
结论：**通过**。两台 Ubuntu VM（24.04 / 22.04）各 **84 项断言全部通过**，VM 内原生执行
28 个代理用例与 9 个命令执行器用例全部通过；P02.2 回归各 99 项仍全部通过。

## 环境

| 项目 | 值 |
|---|---|
| 开发宿主 | Windows + Go 1.27.3、Node 22.23.3（PATH 前置 nvm 目录，DECISIONS Q14） |
| VM-B（24.04） | Ubuntu 24.04.5 LTS，内核 6.8.0-147-generic，Python 3.12.3，SSH `127.0.0.1:2222`；`upanel-web` uid 999 / `upanel-probe` uid 997（同组） |
| VM-A（22.04） | Ubuntu 22.04.5 LTS，内核 5.15.0-198-generic，Python 3.10.12，SSH `127.0.0.1:2223`；`upanel-web` uid 997 / `upanel-probe` uid 996（同组） |
| 二进制 | 宿主 `GOOS=linux GOARCH=amd64` 交叉编译；两台 VM 使用同一份，sha256 与 `cross-build-sha256.txt` 逐字一致（见 `vm-2404-env.txt`、`vm-2204-env.txt`） |
| 工具链 | VM 内**没有** Go/Node（DECISIONS Q9），所以单元测试以 `go test -c` 编出的 Linux 测试二进制在原内核上直接执行 |

## 文件清单

| 文件 | 内容 |
|---|---|
| `host-build.txt` / `go-vet.txt` / `gofmt.txt` | 宿主 `go build ./...` / `go vet ./...` / `gofmt -l .`：全部干净（gofmt 输出 0 行） |
| `go-test.txt` | `go test ./... -count=1`：16 个包 ok（含新增的 `internal/agent`、`internal/agentclient`、`internal/syscmd`），`migrations` 无测试文件 |
| `check.txt` | `sh scripts/check.sh`：PASS 12 / FAIL 0 / NOT RUN 0（含 go-build-linux / go-vet-linux 交叉检查、redocly lint） |
| `cross-build-sha256.txt` | 5 个 Linux 产物（3 个服务二进制 + 2 个测试二进制）的 sha256 |
| `vm-2404-p023-acceptance.txt` / `vm-2204-p023-acceptance.txt` | `tests/integration/p02-3-agent-acceptance.sh` 完整输出：两台均 PASS 84 / FAIL 0 |
| `vm-2404-agent-test.txt` / `vm-2204-agent-test.txt` | 代理单元测试二进制在 VM 内核上原生执行：28 PASS（`peercred`、`gate`、`registry`、`queries`、`server`、`agentclient`） |
| `vm-2404-syscmd-test.txt` / `vm-2204-syscmd-test.txt` | 命令执行器单元测试二进制原生执行：9 PASS + 1 SKIP（辅助进程自身的入口） |
| `vm-2404-systemd-analyze-verify.txt` / `vm-2204-systemd-analyze-verify.txt` | `systemd-analyze verify` 原文；22.04 那条是系统自带 `snapd.service` 的告警，与本单元无关（见下） |
| `vm-2404-journal.txt` / `vm-2204-journal.txt` | 代理 journal：启动字段、UID 拒绝记录、未登记查询拒绝记录、优雅停止 |
| `vm-2404-p022-regression.txt` / `vm-2204-p022-regression.txt` | P02.2 验收回归：两台均 `结果: PASS=99 FAIL=0` |
| `vm-2404-env.txt` / `vm-2204-env.txt` | os-release、内核、Python、二进制 sha256、三个身份的 uid/gid 与组成员 |
| `commands.log` | 实际执行过的命令序列与本轮踩到的三个坑 |

## 已验证（摘要，两台 VM 一致）

**进程与 socket 形态**

- 代理由 systemd 以 `User=root` 拉起（`systemctl show` 与 `ps` 双重确认），`RuntimeDirectory=upanel`；
- `/run/upanel` 属主 root、模式 **0755**（可穿越但不可写），socket `/run/upanel/agent.sock` 属主 root、
  组 `upanel-web`、模式 **0660**；停止服务后 socket 被移除、服务 `inactive`；
- 代理进程**没有任何 TCP 监听**（`ss -ltnp | grep pid=$MAINPID` 计数为 0）——它只听 Unix socket。

**UID 边界（内核 SO_PEERCRED，不是自己解析的头部）**

- `upanel-web` 调 `GET /internal/v1/capabilities` → 200，`mode=production`、`actions=[]`、`queries=[]`；
- **root** 调同一接口 → 403 `FORBIDDEN`（普通接口不接受 root，root 只能走专用路由，而专用路由表当前为空）；
- `upanel-probe`（**与 upanel-web 同一组**，文件权限上完全能连 socket）→ 403 `FORBIDDEN`，
  错误信息为“无权调用”且**不泄露允许的 UID**——证明拒绝来自 SO_PEERCRED 的 UID 判断；
- `nobody` → 连不上（HTTP 000），文件系统层就已经拒绝，与 UID 判断构成两层防线；
- journal 里能看到真实拒绝记录：`uid=0`、`uid=997`（probe）被拒，`allowed_uid=999`（24.04）。

**结构化只读查询通道：只认登记过的名字与字段**

- 未登记的 `query`（`system.summary`）→ 400，message 指明“未登记的查询”；
- 命令形状的请求（`{"query":"command","params":{"command":"id"}}`）→ 400；
- 未知字段（信封层与 params 层各一次）、非 JSON、空 body → 400；超长 body → **413**；`GET /queries` → **405 + Allow**。

**没有万能转发**

- Web 账号请求 `/internal/v1/operations`（未实现）→ 404；
- Web 账号请求任意未知路径 → 404，而 root 请求同一路径 → 403（不是 404）。即 root 连"探测有哪些路径"都做不到。

**启动期拒绝，不降级**

- `web_user` 写成不存在的账号 → 退出码 1，日志说清原因；
- `web_user` 写成数字 UID（`"1000"`）→ 退出码 **2**（配置校验阶段就拒绝，不做“先跑起来再说”）；
- 以非 root 运行 → 退出码 1，“必须以 root 运行”；
- socket 路径已被一个普通文件占据 → 退出码 1，“拒绝覆盖”，且该文件事后仍在（`agent-kept-foreign-file`）。

**命令执行器与登记表（真实 Linux 内核）**

- `syscmd.test` 原生跑：拒绝相对路径/空程序、拒绝 8 种命令字符串形状（`;`、空格、换行、`$( )`、
  反引号、`|`、`&&`、引号）、参数逐字传递（`"a b"`、`"; rm -rf /"` 不会被解释）、
  固定环境（父进程的 `LD_PRELOAD` 不泄漏）、输出在 1 KiB 上限处截断并继续读完、
  300 ms 时限到点杀进程（**含进程组**）、父上下文取消同样生效；
- `agent.test` 原生跑：登记表名字形态校验、未知字段拒绝、请求体上限、`gate` 对 root / 其他 UID /
  未知路径的判定、`queries` 的四类拒绝、`server` 的启动校验、`agentclient` 的双错误类型。

**回归**

- P02.2 的 99 项在改动 `config.Agent`（新增 `web_user` / `socket_group`）与重写 `agent.Server` 之后
  仍全部通过：账号、再认证、乐观并发、审计只追加这些行为没被碰坏。

## 过程中发现并修复的问题

1. **命令字符串被当成程序路径**（真实缺陷，`ErrProgramInvalid`）。POSIX 下 `"/bin/sh; rm -rf /tmp/x"`
   按路径规则是“绝对路径”，`filepath.IsAbs` 判定为真，于是执行器会去 exec 它——虽然 `execve`
   本身不会解释 `;`，但错误信息与判定语义都是错的（用户看到的是“文件不存在”而不是“这不是一个程序”）。
   修复：`validateProgram` 在 `IsAbs` **之前**跑一遍字符白名单（只允许路径里合法出现的字符），
   并新增 `TestRunRejectsCommandStrings`。这个缺陷是**在 VM 上原生跑测试**才暴露的，宿主没发现。
2. **`/run/upanel` 是 0750**（首次 VM 运行的 20 项 FAIL 的根因）。`RuntimeDirectoryMode` 默认继承了
   限制性 umask，导致 Web 账号连 socket 都走不到（全部 000）。修复：单元里显式
   `RuntimeDirectoryMode=0755`，并在代理启动时新增 `verifySocketDir()`——目录必须 root 属主、
   组/其他不可写、且能被 socket 组或其他人穿越，否则**拒绝启动**。这样"目录权限被收紧成打不开"
   不会再表现成“接口莫名 000”。
3. **systemd 单元里的 `ConfigurationDirectory` 与 Web 单元冲突**（`mode is different (755 vs 750)`）。
   修复：代理单元删掉 `ConfigurationDirectory*`，`/etc/upanel` 由 Web 单元负责，注释写明原因。
4. **`ExecStartPre` 用到了旧版 `upanelctl`**（`json: unknown field "web_user"`）。修复：验收脚本把
   新编的 `upanelctl` 一起装到 `/usr/local/bin/`，脚本新增 `ctl-installed` 断言——本机残留的旧二进制
   不该让服务启动失败变成“配置错”。
5. **验收脚本的断言过宽**：`systemd-analyze verify` 会把依赖单元一起加载，22.04 自带 `snapd.service`
   在 systemd 249 下报 `Unknown key name 'RestartMode'`，首轮因此 FAIL 1（83/84）。修复：只对
   本单元自身的告警判失败，其余原文写日志并打一条不计分 NOTE（两台 VM 复跑 84/84）。
   同一轮还发现投放的 `tests/agent.test` 是旧构建（sha256 与宿主重编不一致）——重新投放并核对哈希后
   完整重跑，因此上面的 84/84 与 28 个用例对应的是**当前源码**。

## 未验证（NOT RUN）

- **真实系统数据采集**：生产登记表**故意为空**（`NewProductionRegistry` 返回空表），
  `capabilities` 如实报告 `actions=[]`、`queries=[]`。磁盘/服务/进程等只读查询随 P04/P05 逐项登记，
  本子任务只交付机制与边界，不交付采集内容。
- **审计与业务同事务、日志秘密脱敏、代理独立数据库与秘密加密存储**：属 P02.4，本轮没有实现。
- **写操作路径**（`/preflights`、`/operations`、`/schedules/:id/trigger`）：按 PLAN 属 P03/P11，
  当前 404，未实现也未测试其行为。
- **并发压力**：同一 socket 上多连接并发、长时间运行下的 fd/goroutine 泄漏没有压测；
  `agent.test` 只覆盖单连接语义与超时。
- **TLS / 前端**：代理不涉及；Web 侧 TLS 属 P10。
- 22.04 上的 `systemd-analyze` 对**其他单元**的告警未修复也不属本仓库范围（见 NOTE）。

## 复现

```sh
# 宿主
export PATH=/c/Users/huxi/AppData/Local/nvm/v22.23.3:$PATH
go test ./... -count=1
sh scripts/check.sh

GOOS=linux GOARCH=amd64 go build -o .tmp-linux-bin/upanel-agent ./cmd/upanel-agent
GOOS=linux GOARCH=amd64 go build -o .tmp-linux-bin/upanelctl    ./cmd/upanelctl
GOOS=linux GOARCH=amd64 go test -c -o .tmp-linux-bin/tests/agent.test  ./internal/agent
GOOS=linux GOARCH=amd64 go test -c -o .tmp-linux-bin/tests/syscmd.test ./internal/syscmd

# VM 内（root）
sudo env BIN_DIR=/tmp/upanel-bin sh /tmp/upanel-src/tests/integration/p02-3-agent-acceptance.sh
# VM 内（非 root，回归）
cd /tmp/upanel-src && BIN_DIR=/tmp/upanel-bin sh tests/integration/p02-2-acceptance.sh
```
