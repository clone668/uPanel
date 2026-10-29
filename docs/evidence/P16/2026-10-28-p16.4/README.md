# P16.4 证据：1.0 的文档集、发布包与端到端验收

日期：2026-10-28　平台：Ubuntu **22.04.5**（kernel 5.15.0-198，QEMU `-accel tcg`，2 vCPU / 3912 MB）
发布字节：`dist/upanel-1.0.0-linux-amd64.tar.gz` sha256 `751a274d…95fba`（VERSION 1.0.0 / `8e52b36-dirty` / 2026-10-28T03:59:13Z）

这一轮交付的是**文档与发布物本身**，所以证据分两类：包长什么样（宿主事实）、装在真机上跑起来什么样（七个相位）。
另外补了一条 P16.3 留下的缺口：Q505 那次 fsync 修复此前只被假文件系统验证过，这一轮让**生产的写路径**在真机 ext4 上走了一遍。

## 1. 交付什么

| 交付物 | 状态 | 位置 |
|---|---|---|
| 用户手册（两种角色、四个阶段、每模块怎么点、常见任务、出问题先看哪里） | 完成 | `docs/USER_GUIDE.md` |
| 管理员恢复手册（忘密码、证书、代理不工作、库坏了、网络/防火墙改坏了） | 完成（本轮改写回滚触发方式与记录字段名） | `docs/RECOVERY.md` |
| 支持矩阵（发行版/架构/文件系统/后端/浏览器，六档证据分级） | 完成 | `docs/SUPPORT_MATRIX.md` |
| 已知限制（未验证 / 有意不做 / 容易读错） | 完成（新增"0.1 → 1.0 升级 未验证"一行） | `docs/KNOWN_LIMITATIONS.md` |
| 发布说明（有什么、包长什么样、验收读数、报问题带什么） | 完成 | `docs/RELEASE_NOTES.md` |
| 校验和（每包 + 发布级 `dist/SHA256SUMS`） | 完成 | 见 §2 |
| 完整证据索引 | 完成 | `docs/EVIDENCE_INDEX.md` |
| 1.0 入口文档（原来是 0.1 时代的阶段流水账） | 改写；原文整体迁到 `docs/DEVLOG.md` | `README.md` / `docs/DEVLOG.md` |

发布包必须**带着**这些文档才算数：`scripts/release.sh` 的文档列表从 6 份扩到 10 份、缺一份即 `exit 2`；
`deploy/scripts/install.sh` 装机时把 README + VERSION + 配置样例 + 10 份 `docs/` 共 **13 个文件**装到
`/usr/local/share/doc/upanel`。这两条都有断言在跑（见 §3 的 install 相位，新增 7 条）。

## 2. 发布包事实

两包都是 `CGO_ENABLED=0` 的静态二进制，tar 里显式写了权限位（打包机是 Windows，`chmod` 对无扩展名文件不生效）：

| 包 | sha256 | 大小 |
|---|---|---|
| `upanel-1.0.0-linux-amd64.tar.gz` | `751a274d2e3810a5459303f44b4a193b8a566d036ea3bcb968a2e8e409555fba` | 44,149,944 B |
| `upanel-1.0.0-linux-arm64.tar.gz` | `c94ed4d50f11805070583ea207c858bfae057740297cebe5f2de5d743352c6db` | 40,945,025 B |

包内布局（两个架构一致，22 个文件 + 6 个目录）：

```
bin/{upanel-web,upanel-agent,upanelctl}        0755
deploy/scripts/{install,uninstall,backup-state}.sh  0755
deploy/systemd/{upanel-agent,upanel-web}.service    0644
deploy/upanel.example.json                     0644
docs/*.md（10 份）                             0644
README.md / VERSION / SHA256SUMS               0644
```

* 包内 `SHA256SUMS` 覆盖 21 个文件（不含它自己），解包后 `sha256sum -c SHA256SUMS` → **21/21 OK**。
* tar 记录的权限：三个二进制与三个脚本 `-rwxr-xr-x`，其余 `-rw-r--r--`（全量清单见 `facts/packages.txt`）。
  注意宿主是 Windows，**解出来的 `stat` 看不到执行位**——所以这一栏是以 `tar tzvf` 自己的记录为准写的。
* `VERSION` 里的 `commit: 8e52b36-dirty` 是刻意的：工作树未提交，包里的代码不等于那个 commit 的内容，
  标 `-dirty` 比写一个对不上的 commit 诚实（Q507）。

## 3. 端到端验收（七个相位，跑在 §2 那两个字节上）

驱动 `tests/integration/p16-2-run-on-2204.sh`，包与二进制就是发布包里的那一份：

| 相位 | PASS | FAIL | NOT RUN | 这一相位看什么 |
|---|---|---|---|---|
| install | 47 | 0 | 0 | 13 步安装、密码只走 stdin、13 份文档落地、证书、单元 hardening、自检 |
| measure | 14 | 0 | 3 | 只读查询、事件流、限流、并发写入、库文件增长 |
| boot | 14 | 0 | 0 | 重启后单元自起、`boot_to_web_seconds=56`（判据 120 s） |
| upgrade | 22 | 0 | 0 | 同版本重装：备份 → 停服务 → 换二进制 → 迁移 → 起回 → 数据还在 |
| failup | 21 | 0 | 0 | 坏包（换掉 `upanelctl`）升级必须失败并**停下**，旧版本仍在跑 |
| uninstall | 23 | 0 | 0 | 保留数据卸载：单元/二进制/文档走，库与配置留 |
| purge | 14 | 0 | 0 | `--purge-*` 删干净，`/srv/upanel` 留 |
| **合计** | **155** | **0** | **3** | 驱动退出码 0（`run.log` 末行"全部相位无失败"） |

三条 NOT RUN（按设计，不是跳过）：

1. **上传 2 GiB 上限** —— 单线程 TCG 下 2 GiB 的 HTTP 上传要跑几十小时，改由 `internal/api` 的单测覆盖；
2. **文本编辑 1 MiB 上限** —— 同上（要构造 1 MiB 的编辑请求并等落盘）；
3. **浏览器完成主要用户流程** —— 验收 VM 里没有 Playwright（Q21），由宿主机 Playwright 经 SSH 隧道打过真面板
   （P16.2 的 `…-p16.2-browser/`，35/0/0）与前端用例覆盖。

install 相位新增的 7 条断言（本轮加的，就是为了不让"包里没带文档"悄悄过去）：

```
PASS  本机文档里有 README.md / VERSION / USER_GUIDE.md / SUPPORT_MATRIX.md / KNOWN_LIMITATIONS.md / RELEASE_NOTES.md
PASS  本机文档份数（README + VERSION + 配置样例 + 十份 docs）= 13
```

## 4. 与 `prelim-docdelta/` 的关系（为什么有两轮）

第一次完整跑（`prelim-docdelta/`，包 sha256 `fbad9fc9…`，44148659 B）之后，文档复核又改出五处**事实错误**
（Q509：改密码界面其实没有、恢复的短语动词应是 `OVERWRITE`、采样是 5 秒不是 10 秒等）。文档是这一轮的交付物，
所以字节必须重打，验收也必须跑在**冻结的那份字节**上，于是有了第二轮。

两轮之间的差异只有文档（bin 三个文件字节数逐个相同，`facts/packages.txt` 与 `prelim-docdelta/packages.txt` 可对）：

| 文件 | 第一轮 | 最终 |
|---|---|---|
| `docs/KNOWN_LIMITATIONS.md` | 8929 | 9471 |
| `docs/RECOVERY.md` | 19246 | 19424 |
| `docs/RELEASE_NOTES.md` | 6075 | 6358 |
| `docs/SUPPORT_MATRIX.md` | 10524 | 10624 |
| `docs/USER_GUIDE.md` | 14614 | 15348 |
| `bin/upanel-{web,agent,ctl}` | 35522540 / 28341854 / 18102286 | 同左 |

两轮的七相位计数一模一样（155/0/3），因为断言数量没变、变的是文档正文。第一轮的结果不当作最终读数，
但也不是白跑的：它证明这套驱动在同一台机器上是可重复的。

## 5. 网络写入路径的真机验证（补 P16.3 的缺口）

P16.3 修的 Q505 是"rename 之后没刷父目录"——修完只有假文件系统的单测（断言"刷过一次目录"），
真文件系统上 `os.File.Sync()` 打在一个**目录** fd 上会不会报 `EINVAL`/`EBADF` 没人验过；而这一步一旦失败，
每一次网络写入都会在 `netplan apply` **之前**停下。

做法：`internal/network/fsync_real_test.go`（`//go:build linux`，默认 `t.Skip`，只在给了 `UPANEL_REAL_FS_DIR` 时跑），
用生产的 `osWriteFS` 与 `ApplyEngine.writeFileAtomic`，宿主交叉编译成测试二进制、拷进 VM 由 root 跑，外面套 `strace` 看 syscall：

```
openat("…/90-upanel-probe.yaml.upanel-tmp", O_WRONLY|O_CREAT|O_TRUNC, 0600) = 3
fsync(3<…/90-upanel-probe.yaml.upanel-tmp>)                                = 0
renameat(… ".upanel-tmp" → "90-upanel-probe.yaml")                         = 0
openat("…/upanel-fsync-probe", O_RDONLY)                                   = 3</…/upanel-fsync-probe>
fsync(3</…/upanel-fsync-probe>)                                            = 0     ← Q505 加的就是这一步
```

覆盖写再走一遍同样的四条，无残留 `.upanel-tmp`，内容逐字节一致、权限 `0600`、测试 `PASS`（0.13 s）。
文件系统：`/dev/sda1 ext4`，`stat -f` 报 `ext2/ext3`（ext4 的族名，正常）。

**它证明"这条写路径在真机 ext4 上走得通、目录 fsync 不返回错误"，不证明掉电语义**——那要拔电源，仍是 NOT RUN。

## 6. 宿主检查（所有改动落定后的工作树）

`sh scripts/check.sh` → **PASS 15 / FAIL 0 / NOT RUN 0**，退出码 0。十五项全 PASS：

```
line-endings sh-syntax go-build go-vet go-test go-build-linux go-vet-linux gofmt
node-version web-typecheck web-lint web-test web-build web-smoke openapi-lint
```

* `go-test` 与 `internal/network` 显示 `(cached)` 是正常的：新加的 `fsync_real_test.go` 带 `//go:build linux`，
  在 Windows 宿主上被构建约束排除，包的输入没变。**这条真机探针在宿主上永远不会被当成"通过"**——
  它只给真机用，宿主侧能证明的只有"交叉编译得出来"（本轮当场构建过：`GOOS=linux go test -c` → 7,988,795 B、
  sha256 `8c28b861…`）。
* `openapi-lint` 的 6 条 warning 与 P15/P16.3 同源（3 条 `no-unused-components` + 3 条 4xx/2xx），不是本轮引入。
* 前端 **62 文件 / 1175 用例**。原始输出去色后存在 `facts/host-check.log`（329 行）。
* **这不是真机验收**。它只证明这台 Windows 宿主上的构建、静态检查与单测成立；本文件 §3 的计数才是真机读数。

## 7. 未验证与剩余风险

* **掉电语义 NOT RUN**：目录 fsync 生效不等于"掉电后一定是新内容"，只等于"内核收到了这条命令"。拔电源演练仍未做。
* **0.1 → 1.0 升级未验证**：upgrade 相位升的是 1.0 → 1.0（同版本重装，覆盖备份/迁移/重启/服务可用整条链）；
  从 0.1 的库升上来只有迁移脚本的单测。已写进 `KNOWN_LIMITATIONS` A 节。
* **arm64 只有打包与校验和**：端到端验收全在 amd64 上。已写进 `SUPPORT_MATRIX` 与发布说明。
* **裸机读数没有**：全部真机读数都在 QEMU/TCG 里。24.04 那条"开机 152 s"的红是这么归因的，24.04 的最终判定见 P16.2 证据。
* **本轮没有新增产品功能**，因此没有新的功能面未验证项。

## 8. 证据文件索引

| 文件 | 内容 |
|---|---|
| `RESULT-ALL` | 七个相位逐行的 PASS/FAIL/NOTRUN 计数（只含最终字节那一轮） |
| `RESULT-{install,measure,boot,upgrade,failup,uninstall,purge}` | 每个相位的 `PASS=/FAIL=/NOT RUN=` 与失败/未跑项名字 |
| `phase-*.log` | 每个相位的完整断言流水（含每一条的标题） |
| `run.log` | 驱动自己的流水：包、平台、相位顺序、VM 重启、facts 取回、结论 |
| `facts/packages.txt` | 两包 sha256、包内 VERSION、体积、tar 记录的权限全量清单、包内 21/21 校验 |
| `facts/fsync-probe.txt` | 真机写路径探针：测试输出、strace 序列、内核/文件系统、二进制与源码 sha256、能证明什么 |
| `facts/host-check.log` | 最终工作树上 `sh scripts/check.sh` 的完整输出（去色，329 行） |
| `facts/install-time.txt`、`facts/timings.txt` | `install_ms=26699`、`upgrade_seconds=41/46` |
| `facts/boot.txt` | `boot_to_web_seconds=56/50`（判据 120 s） |
| `facts/{lat-*,latency,growth-*,marker}.txt` | 只读查询与事件流的延迟、库文件增长、标记文件 |
| `logs/` | 客户机侧的安装/升级/卸载/重装日志原件 |
| `prelim-docdelta/` | 第一轮（`fbad9fc9…`）的同一套文件，作废理由见 §4 |
