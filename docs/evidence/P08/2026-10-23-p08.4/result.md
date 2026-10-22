# P08.4 结果：共享 UI、用户权限与客户端连接说明

日期：2026-10-23　分支：`main`　runner：`tests/integration/p08-4-share-ui-client-acceptance.sh`
VM：`upanel-2404`（Ubuntu 24.04.5 LTS，内核 6.8.0-147，Samba 4.19.5-Ubuntu，`mount.cifs` 7.0）

## 结论

**passed。** 第四遍真机运行 **PASS=190 / FAIL=0 / NOT RUN=4**（`run.log`）。
宿主 `scripts/check.sh` **PASS 13 / FAIL 0 / NOT RUN 0**（`check.txt`）；
`go test ./...` 全部包 ok、前端 637 条用例全绿（`native-tests.txt`）。

四遍运行（`runs/run1.log`、`runs/run2.log`、`run.log`，第三遍即 `run.log` 本轮定稿那遍）：

| 遍次 | 结果 | 红的原因 |
| --- | --- | --- |
| #1 | PASS=167 / FAIL=20 / NOT RUN=0 | 产品缺陷 **Q246**（4 条）、构建顺序（1 条：二进制早于前端产物同步）、脚本缺陷 3 处（起点 include 残留 1 条、只读写探针 1 条、第 9 节设计整段 13 条） |
| #2 | PASS=187 / FAIL=2 / NOT RUN=4 | 产品缺陷 **Q247**（1 条）+ 脚本仍读不存在的 `volume_state`（1 条，Q246 的同类残留） |
| #3（定稿） | **PASS=190 / FAIL=0 / NOT RUN=4** | — |

## 逐节断言数（共 190）

| 节 | PASS |
| --- | --- |
| 0 环境与安全守卫 | 40 |
| 1 配置与启动（代理 root、Web 非 root） | 10 |
| 2 夹具：卷 / 三个账号 / 一条共享 | 30 |
| 3 三个准备状态分开，且与现场一致 | 15 |
| 4 页面载荷契约：账号里没有密码，连接状态带 `service` | 12 |
| 5 照页面给的命令，逐条执行 | 26 |
| 6 权限矩阵（协议层，身份来自真实秘密通道） | 10 |
| 7 密码卫生：命令行、日志、数据库、挂载表 | 8 |
| 8 抓取页面夹具（供 web 侧回放） | 9 |
| 9 卷掉线：页面说的“连不上”与现场一致 | 19 |
| 10 收尾不变量：撤掉共享之后卷里的东西一个不少 | 11 |

## 这一阶段到底证明了什么

**1. 页面读的载荷与契约一模一样。** 记录的键必须是 openapi `ShareRecord` 十个字段的**子集**；
`volume_state` / `volume_name` / `startup_deps` / `startup_paths` 出现即红。
卷的挂没挂由页面按 `definition.volume_id` 从 `GET /storage/volumes` 的 `observed_state` join；
开机依赖在 `share.apply` 的 `result.data` 里（`written|unchanged|removed|failed|skipped_no_service` 闭集）。

**2. 说明里的命令真的能跑。** 第 5 节逐字执行页面给出的那 11 行（只把占位符换成真值）：
`smbclient //127.0.0.1/data -U smbp08rw -m SMB3 -c ls` 连上并列出内容；
`mount.cifs` 用 here-doc 写入的 `0600` 凭据文件挂到 `/mnt/data`，通过挂载点写入、读回、
`sha256` 与卷里那份一致，最后 `umount` 干净卸载。这几行字符串与
`web/src/api/shares.test.ts` 钉住的是同一批文本，与 `web/src/fixtures/p08-4-vm/client-mount-steps.txt`
逐字比对（`realdata-p08-4.test.ts`）。

**3. 权限矩阵是协议层的事实。** write 账号能上传（`putting file` + 文件真的落在卷里）；
read 账号能列目录、上传被拒（真实 `NT_STATUS_*`）且卷里没有留下文件；
在 passdb 里但不在名单里的账号被拒；匿名（guest）被拒。四类都带独立证据（卷里文件的有无）。

**4. 密码不出现在任何不该出现的地方。** 代理日志、Web 日志、抓回的 58 份载荷、
两个数据库（含 `-wal`/`-shm`）、`/proc/*/cmdline` 与 `environ`、`/proc/mounts` 全部 0 命中；
凭据文件 `0600`、目录 `0700`；账号动作的任务参数里带的是 `sec_` 引用。

**5. 页面说“连不上”的时候确实连不上，说“能连”的时候连得上。** 第 9 节从外面 `umount`
复现运行中脱盘：11 秒后对账撤销（撤销块五字段齐全、`config_applied` 变假、托管配置里没有这一段、
`smbclient` 拿到 `NT_STATUS_*`）；卷挂回来**仍是撤销状态**；点“重新应用”之后撤销现场消失、
`config_applied` 回到真、`smbclient` 又能读写。面板自己拒绝在这块卷上卸载（`VOLUME_IN_USE_BY_SHARE`）——
所以“运行中掉线”只能用外部卸载复现，这正是现实里的样子。

## 真机抓出的两个产品缺陷

**Q246 — 前端 DTO 里写着后端从不发送的字段。** `ShareRecord` 上多了 `volume_state`、`volume_name`、
`startup_deps`、`startup_paths`。后果不是“多显示一个空”：`summarizeShareState` 的“卷没挂”分支
判 `volume_state !== 'mounted'`，字段恒为空 → 那个分支永远进不去，**卷真掉线时页面仍显示“已写入配置”**；
`clientHintsUsable` 同理。能活下来是因为单测夹具是自己写的、带着这些字段。修法：删掉四个凭空字段，
卷的状态由页面 join（`ShareVolumeInfo`），开机依赖那行从卡片上删掉（它属于任务结果），
并让验收脚本反向钉死“记录的键必须是契约的子集”。

**Q247 — “重新应用”这条出路走不通。** 卷掉线 → 对账撤销 → 卷挂回 → 点“重新应用”：任务
`succeeded/verified`、`config_applied` 回真、客户端立刻能读写，**但 `runtime_revoked` 原封不动**，
全仓库没有一处把它写回 `nil`。于是页面永远显示“运行时已撤销”、`clientHintsUsable` 恒假——
CONTRACTS §7.4(2) 要求的“同时给出现状与出路”变成只通告坏消息的死路。修法：成功应用后清空撤销现场
（撤销只由对账产生；卷还在离线时下一轮对账会记一笔新现场），配
`TestExecuteShareApplyClearsRuntimeRevocation`，契约补两处。

## 验收脚本自身的缺陷（改脚本，不改产品）

1. **起点不干净就断言“干净”**：上一遍留下的 `include` 行让“起点没有残留 include”直接红。
   改为先把 `smb.conf` 整份留档、摘掉指向 `/etc/samba/upanel.d/` 的行，收尾时原样放回。
2. **只读写探针抓不到错误**：`printf > file` 失败时错误是 *shell* 打的，绕开命令自己的 `2>`，
   于是“拒绝是真的系统错误”拿到空 stderr。改用 `touch … 2>file`，错误由 `touch` 自己写。
3. **第 9 节按“面板自己卸卷”设计**：面板正确地拦住了（`VOLUME_IN_USE_BY_SHARE`），整段 13 条连锁红。
   改为“断言面板拒绝 → 从外面 `umount` 复现 → 轮询 ≤75 秒等 30 秒对账 → 从外面挂回 → 仍是撤销
   → 重新应用恢复”。
4. **又去读不存在的字段**：Q246 之后第 9 节还在读共享记录的 `volume_state`（拿到 `None`）。
   改为读卷列表的 `observed_state`，并额外断言“共享记录里依旧没有这个键”。
5. **构建顺序**：`bin/linux-amd64` 里的二进制早于 `internal/webui/dist` 同步，于是二进制里没有共享页
   （“Web 二进制里嵌入了共享页”红）。改为先构建前端 → 同步 dist → 再 `GOOS=linux go build`。

## 未验证（NOT RUN，逐条写原因）

- **Windows 客户端**（资源管理器 / `net use`）：没有 Windows 测试机，`P00-VM-D` 客户端 VM 仍未建立；
  说明里给它的地址形状（`\\主机\名字`）由 web 侧断言，真实连接没跑过。
- **macOS 客户端**（Finder）：同上，没有 macOS 测试机。
- **从另一台物理机器连进来**：本机客户端与 `smbd` 同机；跨机器还牵涉防火墙，属于没验过的路径。
- **浏览器里的真实渲染**：脚本验的是页面读到的载荷；DOM 由 vitest 的 SSR 回放覆盖
  （`realdata-p08-4.test.ts` 用这批真机载荷渲染卡片与账号面板），没有真浏览器截图。

## 已知限制

- 夹具是**某一次运行**的样子：`shares-offline.json` 与 `shares-restored.json` 逐字节相同
  （差别在卷那一侧），回放用例因此显式把“挂载中的卷”传进去，而不是从文件里读卷状态。
- `web-smoke` 在 `check.sh` 里跑的是生产/ mock 两种模式的冒烟，不覆盖浏览器交互；
  交互仍由单元测试（含真机载荷回放）守。
- 本轮没有新增接口、没有改字段、没有换技术栈；`api/openapi.yaml` 只把版本推到 `0.0.0-p08.4`。

## 证据清单

| 文件 | 内容 |
| --- | --- |
| `run.log` | 定稿那遍的完整输出（190 PASS / 0 FAIL / 4 NOT RUN） |
| `runs/run1.log`、`runs/run2.log` | 前两遍的原始输出（红在哪、为什么红） |
| `payloads/` | 58 份真机原始载荷（页面载荷、预检、任务、`smbclient` 输出、被拒绝的卸载） |
| `logs/` | 代理日志、Web 日志、`smb.conf` 初始留档、`lsblk` 初始快照、`fstab` 重置前留档 |
| `env.txt` | 宿主与 VM 的版本、脚本与三个二进制的 `sha256`（VM 上与仓库里一致） |
| `check.txt` | `scripts/check.sh` 全量输出（PASS 13 / FAIL 0 / NOT RUN 0） |
| `native-tests.txt` | `go test ./...`、本轮新增的 Go 用例、前端 637 条用例 |

回放夹具在 `web/src/fixtures/p08-4-vm/`（含 README 说明来源与“别手改”的理由）。
