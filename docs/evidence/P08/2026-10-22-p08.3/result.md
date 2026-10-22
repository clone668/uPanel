# P08.3 验收结果 — 共享与 volume_id 绑定、启用检查、启动依赖、运行时脱盘

对应 STAGES §10 P08.3；契约见 CONTRACTS「运行时脱盘」「开机依赖（systemd drop-in）」「存储侧的反向约束」；
决策见 DECISIONS Q240–Q243。

## 1. 结论

| 项 | 结果 |
| --- | --- |
| 真机验收（Linux VM integration） | **PASS=207 FAIL=0 NOT RUN=5**（14 节，`run.log`） |
| 重启顺序验收（三阶段，真 `reboot`） | **pre 18 / post 11 / clean 5，各 0 失败**（`logs/reboot-*.txt`） |
| 主机侧全套检查（`scripts/check.sh`） | **PASS 13 / FAIL 0 / NOT RUN 0**（`check.txt`） |
| 主机侧 `go test ./... -count=1` | 全部包 `ok`，0 失败（`native-tests.txt`） |
| 迁移升级路径探针 | **通过**：15 列旧库就地补成 16 列，旧行取到空串（`probe-migration-upgrade.txt`） |
| 这轮之前的状态 | 实现在，但**真机跑不通**：`runtime_revoked` 从来没落过盘（Q240） |

“PASS” 在这里的含义与 P08.2 一致：断言由**面板之外**的独立手段复核，不是面板自己回一句 success。
每一节用的外部证据是：`findmnt`/`lsblk`/`blkid`、根分区上的标记文件、`testparm` 读回、
真实 `smbclient` 的 `NT_STATUS_*`、`smbstatus` 的 tcon 计数、`systemctl show` 自己解析出的
`RequiresMountsFor=`/`After=`/`DropInPaths=`、`smb.conf` 逐字节前缀比对、`stat` 的权限位、
以及五处明文扫描。

## 2. 环境与被测物

一次性 Ubuntu 测试虚拟机（**不是开发宿主，也不是日常服务器**），完整采集见 `env.txt`：

- 被测二进制 sha256（与宿主构建产物一致）：
  - `07445d395bf8db3b3dcca353cbd2ccbfc4e112be96802d1356bae702a30b3a45  upanel-agent`
  - `210847ba37e80c0210a256f767e7e9827f44e11e2523697ec293fccc385df854  upanel-web`
  - `ea4183224865cd1c0e301a50b4b8cd279ab22b98d593687e6833993b226dd0a4  upanelctl`
- 主验收脚本 `p08-3-share-volume-binding-acceptance.sh`：1222 行，sha256
  `a4ade9377cc63cea3ac29656453c04404bc163ffb29215fdec9451a589f4e3bb`，
  与仓库里 `tests/integration/p08-3-share-volume-binding-acceptance.sh` **一致**（跑的就是提交的这份）
- 重启顺序脚本 `p08-3-reboot-order.sh`：395 行，sha256
  `e8dec05d9c20216f6f1ce63cb9b29164799f215352afc45f106d82a0875d1556`，
  与仓库里 `tests/integration/p08-3-reboot-order.sh` 一致（`env.txt` 末尾的 `sha256sum` 是同一份）

## 3. 逐节断言（`run.log`）

| 节 | 断言数 | 这一节证明什么 / 外部的眼睛是什么 |
| --- | --- | --- |
| 0. 环境与安全守卫 | 42 | 依赖命令与 Samba 工具齐备（含 `systemd-escape`）；`/` 在 `sda`；`smb.conf` 起点哈希已记录；上一轮遗留的 drop-in / 挂载已清；`systemd` 里起点**没有** `RequiresMountsFor` |
| 1. 配置与启动 | 18 | 代理以 root 起、Web 以 `upanel-web` 起；日志声明共享模块已接线、监视循环已启用（并写明“不自动恢复”）；能力表里有 `share.apply/disable/delete`、`storage.volume.unmount/forget`、`storage.filesystem.format` |
| 2. 夹具 | 15 | 空盘 → GPT → 分区 → ext4 → 导入 → 挂载全部走产品自己的链路；`blkid` 复核卷标；受管理账号经秘密通道建好 |
| 3. 基线：共享真的能用 | 10 | `share.apply` → `verified` + `served`；真实 `smbclient` 能上传、能列出；`smb.conf` 与 prompt 前的基线逐字节可比 |
| 4. 开机依赖 | 12 | drop-in 由作业写出（`startup_deps=written`、`startup_paths` 是挂载点）；文件 `0644`、目录 `0755`、只有一行 `RequiresMountsFor`；**systemd 自己**在 `DropInPaths`/`RequiresMountsFor`/`After` 里认了它 |
| 5. 运行中脱盘 | 23 | 外部 `umount` 之后 24 s 内撤销（监视间隔 30 s）：登记表记下 `runtime_revoked`（卷、卷名、`unmounted`、时间、原因），`config_applied=false`，托管文件里那一段没了，`smbstatus` 的 tcon 归 0，客户端拿到 `NT_STATUS_BAD_NETWORK_NAME`，**并且看不到根分区上的标记文件**；撤销同时把 drop-in 收回 |
| 6. 幂等（走启动首轮） | 8 | 重启代理之后：托管文件哈希与 mtime 都不变、撤销时间仍是第一次的、`updated_at` 没被推到当下、`config_applied` 仍是 false；这条同时证明“开机时盘就没挂上”走同一套判定；**代理进程自己的 umask 是 0077**（与生产单元一致） |
| 7. 卷没挂时再应用 | 3 | 预检被 `VOLUME_NOT_MOUNTED` 挡下，理由说清“挂载点此时是系统盘上的空目录”；因为被挡下，这一节不提交任何作业 |
| 8. 卷回来不自动恢复 | 17 | 按设备挂回卷之后等满一个周期：那一段仍然不在配置里、托管文件哈希不动、撤销现场保留；**人**再应用一次才 `verified`+`served`，drop-in 装回来，客户端看到的是**卷里**的 `probe.txt` 而不是根分区上的标记文件（标记文件此刻被挂载遮住，但确实还在根分区上） |
| 9. 依赖阻断的三个阈值 | 26 | 启用中的共享挡住卸载/格式化/退出管理，码分别是 `VOLUME_IN_USE_BY_SHARE`×2 与 `VOLUME_HAS_DEPENDENCIES`；`/blockers` 的 `busy=true`、`shares[]` 点名；**禁用**之后卸载放行，但退出管理与格式化仍然被挡 |
| 10. 重新启用 + 依赖清零 | 22 | 重新启用把 drop-in 装回来（`written`，文件权限仍是 `0644`）；删掉最后一条共享后 `startup_deps=removed`、drop-in 消失、`RequiresMountsFor` 为空、`dependencies[]` 清空、`busy=false`；没有依赖时卸载放行并真的卸下来 |
| 11. 收尾不变量 | 11 | `include` 仍只有一行；`smb.conf` 原有段名一个没少、逐字节前缀延伸；托管目录 `0755`、托管文件 `0644`（**在 umask 0077 的进程里**）；两个日志 + 两个数据库（含 WAL）+ 所有进程 argv/environ 里都扫不到共享密码明文；代理没退出、socket 还在、`smbd` 仍在跑 |
| 12. 明确不验（NOT RUN） | 5 | 见 §7，每一条都写了原因和替代证据 |

断言全部落在“外部能看见的状态”上。这一阶段最容易犯的谎是“配置写进去了所以共享可用”，
所以每一处都分开报：`config_applied`（段在文件里）、`served`（include 在、合并 `testparm` 过、
smbd active 且重载完成、卷仍挂载）、以及**客户端真的能不能连上**——三者互不代替。

## 4. 三遍运行与四个缺陷

同一份脚本在同一台一次性 VM 上跑了三遍，三份日志原样留在 `runs/`（`run1.log`、`run2.log`，
`run.log` 是最终那份）：

| 运行 | 结果 | 失败来自哪些根因 |
| --- | --- | --- |
| `runs/run1.log` | PASS=184 **FAIL=14** NOT RUN=5 | Q240（7 条）+ Q241（6 条）+ Q243（1 条） |
| `runs/run2.log` | PASS=203 **FAIL=2** NOT RUN=5 | Q243（1 条）+ Q241 第三处（1 条） |
| `run.log` | **PASS=207 FAIL=0 NOT RUN=5** | — |

失败条数远多于根因数，这是有意的记账方式：**一个根因可以打掉一串断言，但只有一个是根因**
（Q233 定下的规则）。下面三条是产品侧的、一条是脚本侧的。

### Q240 — 产品缺陷：结构体有字段、表里没有列

第一遍的 §5 先卡在这里（`runs/run1.log:119`）：

```
FAIL  监视循环在一个周期内撤销了共享: 等了 75s 仍没有 runtime_revoked
FAIL  撤销现场记的是这块卷: 期望 'vol_343f…'，实际 'ERR:KeyError:'runtime_revoked''
FAIL  第二轮之后 updated_at 没有被推到当下: 期望 '2026-10-22T14:55:58.591Z'，
                                                  实际 '2026-10-22T14:56:07.502Z'
FAIL  一个周期之后撤销现场仍然保留: 期望 'True'，实际 'False'
```

而代理自己的日志明明白白记着撤销发生了：

```json
{"level":"WARN","msg":"卷离线，相关共享已从托管配置撤销","shares":"[p08sh3]",
 "volume_states":"[unmounted]","config_written":true,"reload":"done","served":true,
 "note":"卷恢复后需要重新应用共享——不会自动写回"}
```

`pragma table_info(shares)` 只有 15 列，没有撤销现场那一列：`shares.ShareRecord` 加了
`RuntimeRevoked *RuntimeRevocation`、`SaveShare` 也带着它，但**没有迁移为它加列**，
`agentstore` 的 SELECT/INSERT 里自然也没有它。后果两层：API 永远交不出“这条共享为什么不见了”；
更隐蔽的是幂等判据失效——“记录上已经记过这次撤销”永远是假的，于是每 30 秒把同一份撤销重写一次，
`updated_at` 一路被推到当下，备份与重载跟着重复。

修法：迁移 `0006_share_runtime_revoked.sql` 加列 `runtime_revoked_json TEXT NOT NULL DEFAULT ''`，
`sharesColumns` 增列、`SaveShare` 序列化（`nil` 写空串，于是 upsert 能清掉它）、`scanShare`
反序列化并在**读不懂时报错而不是猜空**。补两条单测（往返 + 坏 JSON 的响亮报错）。
升级路径另做探针：把一个 16 列的库 DROP 回 15 列、删掉最高版本那条迁移记录，再起代理——
列补回来了、旧行取到空串、其余迁移没有重跑（`probe-migration-upgrade.txt`）。

### Q243 — 产品缺陷：权限被 umask 掩掉

第一遍和第二遍各有一条，是收尾不变量：

```
FAIL  托管文件权限是 0644: 期望 '644'，实际 '600'
```

P08.2 跑同一台机器时这条是**通过**的。差别在于这一遍在第 6 节重启了代理，而重启之前设了
`umask 077`——与生产的 `deploy/systemd/upanel-agent.service` 里 `UMask=0077` 一致。
`os.OpenFile(name, O_CREATE, 0644)` 的 `perm` 先被 umask 掩一遍才生效：`0644 &^ 0077 = 0600`；
候选文件每次新建，`rename` 又把候选的权限带给正式文件。也就是说**生产上的托管配置与开机依赖
一直是 0600**，而契约写着 drop-in 是 `0644`。

修法：`writeFileSync` 写完内容之后显式 `Chmod(perm)`——权限由代码里那个常量决定，不由启动它的
shell 决定；已经存在的旧文件也在同一步被纠正。补 `TestWriteFileSyncEnforcesMode`（新文件、
遗留 0600 文件、本该 0600 的文件三种情形）。验收侧不再只断言结果：第 6 节先断言**代理进程
自己的 umask 是 0077**，第 10 节断言 drop-in `0644`，第 11 节断言托管文件 `0644`——三条合起来
才说明“权限没被 umask 掩掉”；在 umask 022 的进程里跑那条断言，它通过也证明不了什么。

### Q241 — 脚本缺陷：按猜测的 JSON 形状断言

第一遍 §9 有 5 条报 `ERR:NameError:name 'ERR' is not defined`。两处都是脚本自己的错：
卷列表的标识字段是 `id` 不是 `volume_id`，`busy`/`shares[]` 在 `/storage/volumes/{id}/blockers`
而不在列表里；`vol_field` 又把 `vol_index` 的 `-1` 直接当 Python 下标用——负下标不报错，
会静默取到最后一条卷。同一遍还有第三处同类：`$(PRE_ALLOWED)`（本该是 `$PRE_ALLOWED`），
shell 报 `1126: FAIL … 期望 'True'，实际 ''`——变量被当成命令执行了，空串交给断言。
第二遍仍是这条（说明修在第二遍之后），第三遍才干净。

修法：改读 `/blockers`；`vol_field` 越界时故意抛 `IndexError`；`-1` 先用 `expect_ne` 挡住；
变量当命令那一处改正。规则记在 Q241：**验收脚本里每个取值都必须能表达“没取到”**。

### Q244 — 脚本缺陷：固定的幂等键，加上把提交失败当成了作业超时

重启脚本第一遍三阶段全绿之后，我为了留日志原样又跑了一遍，`pre` 当场 8 条失败，
第一条是 `挂载卷任务终态: 期望 'succeeded'，实际 'timeout'`。而代理日志在那段时间里
**什么都没收到**。真正的原因在 Web 侧：

```json
{"error":{"code":"IDEMPOTENCY_CONFLICT",
          "message":"同一个 Idempotency-Key 已经用于另一个请求；重试时请复用原请求体，或换一个新键"}}
```

`run_action` 用固定字符串当键（`reboot-mount`、`reboot-apply`…），而幂等记录是持久的：
第一遍用过的键，第二遍再提交就冲突，作业**根本没被创建**。主验收脚本没暴露这个问题，
因为它每次开头重建 Web 库、幂等表跟着清空；重启脚本不能重建库（要用主脚本留下的卷与账号）。

第二层更值得记：提交失败之后 `jget` 返回的是 `ERR:KeyError:'data'`，脚本把它当成作业 id 去轮询，
对着 `/api/v1/jobs/ERR:...` 打满 60 秒 404（`logs/web-reboot.log` 里那一串
`path":"/api/v1/jobs/ERR:KeyError:'data'"`），最后报“作业超时”——把人的注意力引到“代理跑不动”上。
这与 Q241 是同一种病：**假的默认值让断言在错误的地方失败**。

修法两处：幂等键改成 `"reboot-mount$RUN_KEY"`，`RUN_KEY="-$(date -u +%Y%m%d%H%M%S)-$$"`
（run 内稳定、跨 run 不同）；拿到 job_id 之后先判空与 `ERR:` 前缀，命中就当场报
`提交作业（键）` 并带上 `error.code`/`error.message`，`JOB_STATE=submit_failed`，不再去轮询假 id。

## 5. 主机侧检查（未在 VM 上跑的部分）

- `scripts/check.sh`：PASS 13 / FAIL 0 / NOT RUN 0，明细 `go-build go-vet go-test go-build-linux
  go-vet-linux gofmt node-version web-typecheck web-lint web-test web-build web-smoke openapi-lint`
  全 PASS（`check.txt`）。
- `go test ./... -count=1`：全部包 `ok`（`native-tests.txt`）。
- 本轮新增测试：
  - `TestSaveShareRoundTripsRuntimeRevocation`、`TestScanShareRejectsBadRuntimeRevocationJSON`
    （`internal/agentstore`）——PASS；
  - `TestWriteFileSyncEnforcesMode`（`internal/shares`）——**在 Windows 宿主上 SKIP**（那里没有
    POSIX 权限位）；它的 Linux 证据是验收第 10/11 节那两条 0644 断言（在 umask 0077 的进程里）。

## 6. 重启顺序（三阶段，真 `reboot`）

主脚本 §12 把“真的重启一次看开机窗口”挂成 NOT RUN，因为它在跑的过程中不能被打断。
这条由单独的 `p08-3-reboot-order.sh`（395 行，sha256 `e8dec05d…`）在**同一台 VM 上连同一次真重启**
补上：`pre` 布置现场 → `sudo systemctl reboot` → `post` 在新的那次开机上核对 → `clean` 收场。

| 阶段 | 断言 | 结果 | 日志 |
| --- | --- | --- | --- |
| `pre` | 18 | **PASS=18 FAIL=0** | `logs/reboot-pre.txt` |
| `post` | 11 | **PASS=11 FAIL=0** | `logs/reboot-post.txt` |
| `clean` | 5 | **PASS=5 FAIL=0** | `logs/reboot-clean.txt` |

`post` 里最关键的一条不是“挂上了”，而是**顺序**——两个单元各自的
`ActiveEnterTimestampMonotonic` 由 systemd 自己给出，挂载单元必须先于 `smbd`：

```
挂载单元 ActiveEnterTimestampMonotonic = 32172243 (Thu 2026-10-22 15:39:13 UTC)
smbd      ActiveEnterTimestampMonotonic = 89382152 (Thu 2026-10-22 15:40:10 UTC)
PASS  本次开机里挂载单元先进入 active，smbd 之后才起来（差 57209ms）
```

剩下的十条：`boot_id` 与重启前不同（确实是新的一次开机）、运行时间 < 30 分钟、drop-in 还在、
systemd 仍解析出 `RequiresMountsFor=/srv/upanel/volumes/p08v3`、`After=` 里有挂载单元、
`smbd` 与挂载单元都 active、`findmnt` 独立确认卷挂着、**客户端能列出共享里的 `probe.txt`**、
以及 `smb.conf` 重启前后**逐字节相同**（重启不该是一次隐式配置变更）。
`logs/reboot-journal.txt` 与 `logs/reboot-systemd-after.txt` 是这一段的原始旁证。

这一遍是**第二遍**跑：第一遍三阶段也全绿（pre 18 / post 11 / clean 5），为了把三段输出留成文件
又原样跑了一遍，结果 `pre` 报 8 条失败——根因是脚本自己的固定幂等键（Q244），不是产品。
失败的那 8 行逐字留在 `runs/reboot-pre-before-q244-fix.txt`（原文件被我下一遍覆盖了，这一点也写在
那个文件的开头）。修掉之后上面这张表才是最终结果。

## 7. 未验证 / NOT RUN（`run.log` §12 原文）

| 项 | 状态 | 原因与替代证据 |
| --- | --- | --- |
| 真的重启一次看开机窗口 | **本脚本 NOT RUN → 已由另一份脚本补上** | 本脚本中途不能被 `reboot` 打断；改用两阶段脚本 `p08-3-reboot-order.sh` 做，见 §6 |
| `SHARE_VOLUME_STATE_UNKNOWN`（观测不明） | **NOT RUN**（单测覆盖） | 真机要造出“卷挂着但读不出来”得先弄坏采集器；判定与渲染在 `internal/shares` 单测里 |
| `VOLUME_DEPENDENCIES_UNKNOWN`（依赖查询失败） | **NOT RUN**（单测覆盖） | 代理总是接了这个查询，无法在真机上让它失败而不改代码；`internal/storage/deps_test.go` 覆盖 |
| `P00-VM-D`（跨机器 SMB 客户端） | **NOT RUN** | 客户端 VM 未建立；SMB 客户端行为改用 VM 内真实 `smbclient` 验证，走的是同一条协议路径，但**不是**跨机器的真实网络路径 |
| 浏览器里的真实渲染 | **NOT RUN** | 本脚本验的是页面读到的载荷（`/shares`、`/blockers` 的 JSON），没有浏览器 DOM 断言 |

## 8. 已知限制（写进契约的，不是“以后再说”）

1. **运行时只撤不恢复**：卷回来不自动写回配置，页面提示“重新应用一次”。自动写回会把一次外部
   故障变成一次静默配置变更。
2. **撤销保留第一次的现场**：一直离线时第二轮不写文件、不重载、不动 `updated_at`。
3. **`unknown` 按“没挂”处理**：问不出卷的挂载状态时不渲染这一段。不对称是刻意的——少一份共享
   是可见、可重放、可修的；多一份指向空目录的共享是静默误导。
4. **开机拉起挂载的是 `RequiresMountsFor`，不是 fstab**：面板写进 fstab 的条目带 `noauto`
   （导入卷 `automount=false`），`local-fs.target` 不会去挂它。
5. **drop-in 与托管配置同生同死**：最后一条共享被撤销/禁用/删除时 drop-in 整份撤走，避免一块
   坏盘拖住 `smbd` 的启动。
6. **文件权限与 umask 无关**（Q243）：托管配置与 drop-in 都是 `0644`，由代码钉住。
