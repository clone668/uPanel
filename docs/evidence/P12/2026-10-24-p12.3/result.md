# P12.3 验收结果：产品恢复、覆盖恢复、恢复验证、保留清理与失败处理

- 日期：2026-10-24 起，交付轮 2026-10-25（UTC）
- 分支 `main`，commit `8e52b36`（工作树未提交，按用户要求不提交不推送）
- 验收对象：`backup.restore`、`backup.verify`、`backup.prune` 三条契约动作的真实实现
- 验收方式：真实 Web（非 root）→ `/run/upanel/agent.sock` → root 代理 → 真实 restic，
  两台专用 Ubuntu VM 各跑一轮，**同一脚本、同一组二进制，不并发**

## 1. 结论

| VM | 发行版 | restic | 结果 | 退出码 |
|---|---|---|---|---|
| 2404 | Ubuntu 24.04.5 | 0.16.4 | **PASS 117 / FAIL 0 / NOT RUN 0** | 0 |
| 2204 | Ubuntu 22.04.5 | 0.12.1 | **PASS 117 / FAIL 0 / NOT RUN 0** | 0 |

两台逐节断言数完全相同，`results.txt` 的断言名单与顺序逐字一致（`diff` 无输出）。
宿主 `scripts/check.sh` 见 §7。

**这一轮把 P12.2 留下的三条 NOT RUN 里的两条做成了真机事实**：产品恢复（含覆盖恢复、
失败不发布）与保留清理。第三条（浏览器 UI）属于 P12.5，本轮不碰，仍记 NOT RUN。

## 2. 交付指纹

构建入口唯一：`scripts/build-linux.sh`；同一条命令连跑两次，三个产物逐字节相同。

```
VERSION=0.0.0-dev COMMIT=8e52b36 BUILD_DATE=2026-10-25T03:40:00Z sh scripts/build-linux.sh
```

| 产物 | sha256 |
|---|---|
| `/var/tmp/upanel-bin/upanel-web` | `5ce3c24aab878a7c495e033cb61f473261a53d1a1ea1a04fcd11c215577f40ce` |
| `/var/tmp/upanel-bin/upanel-agent` | `2bee85c4088038c831c93259779918a9c9a207f8a8b3e7b18fb1f49eecfce50b` |
| `/var/tmp/upanel-bin/upanelctl` | `123ae7beb06ffc9a04e54805f6ebd9b0cfc7a7c69c06634649f8e89abeb6b5c9` |
| `tests/integration/p12-3-recovery-acceptance.sh` | `1f74ba99a0d4085901ec764032f364c044498e862598267f1f3044068d1533a4` |
| `tests/integration/json_helper.py` | `767b9844676a7f570c195b4de1a2e1768bb1f50b5aa7219963c016c1c9d6a837` |

两台 VM 的逐文件核对输出在 `logs/deploy-verify.txt`，版本戳均为
`upanel-agent 0.0.0-dev (commit 8e52b36, built 2026-10-25T03:40:00Z, go1.27.3)`。
`logs/build-sha256.txt` 记了三版二进制的来历，前两版作废，原因见 Q363/Q364。

## 3. 逐节断言数（两台完全相同）

| 节 | 内容 | 断言数 |
|---|---|---|
| §0 | 前置、版本与独立故障卷 | 11 |
| §1 | 96 MiB 真实快照：本地与 SFTP | 14 |
| §2 | 恢复到新目录、默认覆盖保护与结构/抽样验证 | 24 |
| §3 | 覆盖恢复：真停/拉起应用、整目录替换 | 13 |
| §4 | 恢复写满独立 tmpfs：字节充足但 inode 耗尽，要求失败且不发布 | 15 |
| §5 | SFTP 恢复中断：失败且原目录不变 | 10 |
| §6 | 显式保留清理：冻结集合、过期拒绝、只删本策略 | 30 |
| 合计 | | **117** |

### 关键任务 id（24.04）

取自 `payloads/2404/job-*.json` 的 `data.job_id`，可直接按 id 复核：

| payload | job id |
|---|---|
| `job-recovery-baseline` | `job_2698c30c1d205d815c99979e84178a50` |
| `job-restore-new` | `job_cc9538e587471cec0ac16ffe1a5b1142` |
| `job-restore-overwrite` | `job_996a4ef17ce6e5c8674fe41dbb01f26d` |
| `job-restore-enospc` | `job_10f990780d8cb8dab207be642f023d1a` |
| `job-restore-network-cut` | `job_a176abcb2b6e197e5e03b536f15de6e0` |
| `job-verify-structure` | `job_a7f03e88dda88220c24c8661454c0e06` |
| `job-verify-sample` | `job_c0bd8cc8aa362ac6325432c00e4a7bf7` |
| `job-prune-success` | `job_6dcdfab58f9f759be2146af0b1c518fa` |

22.04 是独立一轮、id 全不同（例如 `job-restore-new` = `job_fe0596c2b8cedd4a9577205779942fb3`、
`job-restore-enospc` = `job_ec28de0d168bafbfa100d2c58e560474`、`job-prune-success` =
`job_4881c6c72effde6f5bbdbdf5415bd527`），因此不能拿一机的 id 去另一机查——这正好也是
"两台各跑一轮、不并发"的副产品。

## 4. 几条最要紧的判据

**默认不覆盖。** `backup.restore` 的目标必须是"不存在或空目录"；目标非空且没带
`overwrite` 时预检直接 `allowed=false`（`restore-nonempty-blocked`、`restore-nonempty-blocker`、
`restore-nonempty-preflight-200`）。契约里 `apps` 只能由面板展开、调用方给了就 400，
本轮同样断言（`restore-caller-apps-400`）。

**发布是原子的，旧目标只在新树通过复核之后才消失。** 恢复走同父目录随机暂存 →
`RENAME_EXCHANGE`（目标已存在）或 `RENAME_NOREPLACE`（目标不存在）→ 发布后重新遍历
核对 → 才删旧树。§3 的 `overwrite-*` 一组证明的是整目录替换（`overwrite-restored-hash`、
`overwrite-stale-removed`）、应用与容器 id 由面板展开（`overwrite-apps-expanded`）、
恢复完应用仍在跑（`overwrite-app-active`）**且确实重启过**——用 `ActiveEnterTimestamp`
前移来证明它真被停过、不是"根本没停"，而不是只看 `is-active` 那一句
（`overwrite-app-really-restarted`），`observed_state.apps_restored` 也如实记录
（`overwrite-apps-restored`）。

**失败不发布。** §4 用 `size=256m,nr_inodes=32` 的独立 tmpfs 做夹具：先建
`overwrite/original.txt`，再建 25 个空文件把空闲 inode 压到 3——字节还够（
`enospc-volume-bytes-available` 要求剩余 > 160 MiB），预检按字节估算因此放行，restic
建到第三个节点时撞 `no space left on device`。这一组证明四件独立的事：任务终态 `failed`、
错误码 `COMMAND_FAILED`、旧内容哈希不变、新树未发布且无 `.upanel-restore-*` 残留、
应用仍 `active`，最后清 filler 并断言卷可恢复。

面板上看到的原文（`payloads/2404/job-restore-enospc.json`）：

```
restic restore 失败（退出状态 1）：Fatal: There were 10 errors；首个错误：
ignoring error for /var/tmp/upanel-accept-p12-2: mkdir /proc/self/fd/3/var/tmp/upanel-accept-p12-2:
no space left on device
```

**结构检查不等于"备份能恢复"。** `backup.verify` 的 `sample_restore=false` 只做仓库结构
检查（`restic check`），契约里必须自我说明这一点，脚本断言
`verify-structure-not-restore-proof`；`sample_restore=true` 才真抽样恢复并比对哈希
（`verify-sample-*`）。

**保留清理只删自己冻结的那一批。** `backup.prune` 预检冻结 `expected_delete_ids`，
执行时集合变了就是 `PREFLIGHT_STALE`（`prune-stale-*`）；成功清理后断言同一仓库的
SFTP 快照仍在（清理**后**重新拉列表，不复用清理前的响应，Q355），另一条策略的快照没被
带走（`prune-other-policy-kept`），且另一个仓库的快照列表逐字未变
（`network-repository-untouched-by-local-prune`）。

## 5. 真机抓出的产品缺陷（本轮修掉并复验）

| 编号 | 缺陷 | 现象 | 修法 |
|---|---|---|---|
| Q356 | `os.NewFile` 包装借用的 fd 被 GC 关掉 | 96 MiB 真实恢复在发布后复核时报 `重置恢复目录游标失败: bad file descriptor`；短测试在 GC 前结束所以是绿的 | `F_DUPFD_CLOEXEC` 复制独立描述符只给本次 exec，files 层始终拥有原 fd |
| Q357 | 会话在库里滑动续期，Cookie 却按登录时刻到期 | 长任务跑到 30 分钟整就全 401，`wait_job` 只能空转；库里会话明明还有效 | `auth.Session.Extended()` + `requireSession` 重发 `Set-Cookie`；脚本改 `-b/-c` 同罐并加 `session-cookie-slipped` / `session-cookie-still-sliding` |
| Q359 | 仓库失效锁让 `verify` / `prune` 永久失败，而清锁办法在面板上没有 | `restic check 失败（退出状态 1）：the unlock command can be used to remove stale locks` | `runLocked`：命中残留锁句式先 `restic unlock`（不带 `--remove-all`）再重试一次；`unlock` 失败照原样报原因 |
| Q360 / Q363 | 失败消息只留最后一行汇总 | `Fatal: There were 10 errors` —— 说错了十次，没说为什么 | 汇总行补第一条具体错误；Q363 又修了"只认 `error:` 前缀"，真实形状是 `ignoring error for ...` |

另有一条纯产品侧的既有修复在 P12.3 期间落地并被本轮复验：P12.1 的 `659ba02`（超时不再
报成 restic 退出状态 -1）。Q351 关于 fd 的描述由 Q356 取代。

## 6. 验收方法上的修正（不是产品缺陷，但会误导判断）

- **Q355**：清理后必须重新读现场。复用 prune 之前的快照列表去证明"清理没碰其他仓库"，
  任务动作正确也不构成证据。
- **Q358**：按字节填满 tmpfs 会被预检正确拦下，测不到"写到一半失败"，而且和 restic 抢
  空间靠调度胜负；已改用 inode 耗尽夹具（Q362）。
- **Q361**：两轮验收并发会让失败算到产品头上（共享 WORK、端口、agent、SQLite、tmpfs）。
  处置是起跑前三查：没有别的 `p12-3-*` 在跑、没有遗留验收 systemd 单元、没有残留 agent/web
  与 tmpfs 挂载；日志用唯一路径。**本轮被作废的两份日志就是这样产生的，不作证据。**
- **Q364**：`gofmt -l .` 不为 0 就不构建；`BUILD_DATE` 固定并连跑两次比对；部署一律
  `install -m 755` 逐文件覆盖并逐文件核哈希（`mv` 通配曾静默留下旧的 `upanelctl`）。

## 7. 宿主检查

`sh scripts/check.sh` 最终 **PASS 13 / FAIL 0 / NOT RUN 0**，`web` 45 文件 820 用例全过，
`web-smoke` production 10/0/0，`openapi-lint` 有效（6 条警告全部是改动前就有的）。
日志：`logs/host-check.txt`。

**第一次跑是 12/1/0，红在 `node-version`**：默认 PATH 里的 `C:\Program Files\nodejs`
是 v24.15.0，而 `.nvmrc` / `.node-version` 锁的是 22.23.3。这是闸门按 DECISIONS Q14
正常工作，不是可以"顺手放过"的一条——web 的 typecheck/lint/test 结果只有在对的工具链上
才算数。处置：把 nvm 那份 v22.23.3（`C:\Users\huxi\AppData\Local\nvm\v22.23.3`，自带
npm 10.9.9）前置到 PATH 后重跑全量，而不是只重跑 `node-version` 一项。用错工具链的那份
日志留在 `logs/host-check-wrong-node.txt` 作对照。

**宿主检查覆盖不到的那部分，在 VM 上补跑了。** `//go:build linux` 的用例在 Windows 上根本
不参与编译，`scripts/check.sh` 的绿不包含它们。所以另外交叉编译了一份测试二进制，直接在两台
VM 上跑（`logs/2404-linux-tests.txt`、`logs/2204-linux-tests.txt`）：

```
TestSlowProgressCallbackDoesNotFailTheCommand      24.04 PASS / 22.04 PASS
TestResticRestoreThroughInheritedDirectoryFD       24.04 PASS / 22.04 PASS
TestRecoveryCoreOnRealRestic                       24.04 PASS / 22.04 PASS
```

`TestResticRestoreThroughInheritedDirectoryFD` 正是 Q356 的回归（真实 restic 恢复后
`debug.FreeOSMemory()` 再 `Fstat` 原 fd，旧实现在此 EBADF），`TestRecoveryCoreOnRealRestic`
覆盖"`restore_verified_at` 由验证记录合并"这条路径——两条在宿主上永远跑不到，不补跑就等于
没验。测试二进制 `sha256 = 2175f490…`（记在 `logs/build-sha256.txt`），跑完已从两台 VM 删除。

P12.3 期间新增/改动的宿主侧用例：

- `internal/backup/restic_retry_test.go`：残留锁判定、`check` / `forget --prune` 重试序列、
  失败消息补原因（其中一条用真机抓到的 `ignoring error for` 原文）、退回第一条非汇总行。
- `internal/backup/restic_linux_test.go`：真实 restic 恢复后 `debug.FreeOSMemory()` 再
  `Fstat` 原 fd，旧实现（`os.NewFile` 包装）会在此失败。
- `internal/backup/exec_unix_test.go`、`prune_test.go`、`retention_test.go`（保留策略与
  冻结集合）、`policy_test.go`、`paneldb_test.go`、
  `recovery_linux_integration_test.go`（Linux 上真实 restic 的恢复核心）、
  `internal/agent/backuprecovery_test.go`（7 条：暂存/交换/核对/拉起/失败路径）、
  `internal/agentstore/verificationstore_test.go`、
  `internal/api/backup_test.go`、`internal/auth/session_test.go`（`Extended` 正反两条）。

说明：`restore.go` 本身没有独立的 `restore_test.go`，它的覆盖面在
`internal/agent/backuprecovery_test.go` 与 `recovery_linux_integration_test.go` 两层——
这是现状，不写成"每条分支都有单测"。

## 8. STAGES 的 P12 验收条目对到本轮

`docs/STAGES.md` §14 的"验收"是一整段，P12.3 只承担其中属于恢复/验证/清理的部分。
逐条对一遍，避免用"看起来都覆盖了"糊过去：

| STAGES 条目 | 本轮状态 | 对应判据 |
|---|---|---|
| 备份后实际恢复并校验文件哈希 | **已验** | §2 `restore-new-hash`、§3 `overwrite-restored-hash`（哈希取自源文件而不是任务自述）、`verify-sample-files`（`sample_restore=true` 抽样真恢复，`files_checked=1`） |
| 错误密码拒绝 | 由 P12.1 承担，本轮不重复计 | P12.1 真机轮 `state-wrongpass-says-password-wrong`、`secret-wrong-password`，原因原文"仓库口令不正确（RESTIC 拒绝了这次解密）" |
| 主机密钥拒绝 | 由 P12.1 承担，本轮不重复计 | P12.1 `host-key-scanned`、`mirror-create-badkey`、`preflight-badkey-code` |
| 存储满标失败且保留原数据 | **已验** | §4 整节（inode 耗尽：`restore-enospc-failed` + `enospc-original-content-preserved` + `enospc-no-stage-left`） |
| 网络断开标失败且保留原数据 | **已验** | §5 整节（`restore-network-cut-failed`、`network-original-content-preserved`） |
| 恢复不会覆盖范围外文件 | **已验** | §2 `restore-new-outside-unchanged`——目标旁放一个兄弟文件，恢复前后哈希必须相同；覆盖恢复走 `RENAME_EXCHANGE`，交换对象只有目标目录本身 |
| 清理只作用于指定仓库且遵守保留策略 | **已验** | §6 全 30 条：`prune-preflight-names-old`、`prune-stale-submit-409` + `prune-stale-deleted-nothing-*`、`prune-old-deleted` + `prune-newest-kept`、`prune-other-policy-kept`、`network-repository-untouched-by-local-prune` |
| 同计划重复触发仅运行一次 | **不属本轮** | P12.4 的 timer 语义，现在没有 timer 可测，记 NOT RUN |
| 系统时区不同仍按指定时间执行 | **不属本轮** | P12.4 |
| 脚本不能升级为 root | **不属本轮** | P12.4（非 root 用户脚本的执行模型） |

## 9. 明确区分

**已实现**（代码在，且有真机证据）：`backup.restore`（新目录 / 覆盖恢复 / 失败不发布 /
应用停拉）、`backup.verify`（结构检查与真实抽样两态）、`backup.prune`（冻结集合、过期拒绝、
只删本策略、残留锁自愈）、恢复目标身份与设备指纹复核、会话滑动与 Cookie 同步。

**已验证**（本轮真机跑出来的）：上面每一组判据在 24.04/restic 0.16.4 与 22.04/restic 0.12.1
上各跑一遍，117 条全绿、两台逐字一致；失败路径（写满、SFTP 断连）不只核对任务终态，还核对
源/旧目标哈希不变与应用仍存活。另外 `//go:build linux` 的三条用例（含 Q356 的 fd 回归与
`restore_verified_at` 合并路径）在两台 VM 上各实跑一遍全 PASS——这三条 Windows 宿主编译不到。

**未验证**：
- 浏览器里的恢复/验证/清理页面与交互——P12.5。
- 定时任务（timer 触发备份与保留清理）——P12.4。
- 从**备份中恢复整台机器**（引导、分区、系统盘）——不属于产品范围，见 PLAN 的 BACKUP-01
  边界；产品只恢复到目录。
- 恢复大目录（数十 GB 级）的耗时与中断重试——本轮最大对象是 96 MiB；没有编造更大规模的
  结论。

**受阻**：无。

## 10. 证据清单

```
logs/2404-run.log                     24.04 交付轮控制台输出（117/0/0）
logs/2404-run-round-enospc-message.log 24.04 上一轮 116/1/0（Q363 的现场，保留对照）
logs/2404-run-aborted-rebuild.log      被主动中断的那一轮（构建作废，结果不用）
logs/2204-run.log                     22.04 交付轮控制台输出（117/0/0）
logs/2404/{results.txt,agent.log,web.log}
logs/2204/{results.txt,agent.log,web.log}
payloads/2404/*, payloads/2204/*       预检、任务、快照列表、df/dfi、网络现场
logs/build-sha256.txt                 三版二进制的来历与作废原因
logs/deploy-verify.txt                两台 VM 的逐文件哈希与版本戳
logs/host-check.txt                   宿主全量检查（13/0/0，node v22.23.3）
logs/host-check-wrong-node.txt        宿主检查第一次（12/1/0，红在 node-version：v24.15.0）
logs/host-check-before-q363.txt       本轮修复前的宿主检查（对照）
logs/2404-linux-tests.txt             Linux 专属用例在 24.04（restic 0.16.4）上的实跑输出
logs/2204-linux-tests.txt             Linux 专属用例在 22.04（restic 0.12.1）上的实跑输出
logs/2404-rehearsal-*.log, 2404-lock-cleanup-attempt1.txt, 2404-single-thread-attempt/
                                      本轮早前现场（EBADF、Cookie 不落罐、清锁失败、
                                      Q361 那份并发作废日志的目录）
```

秘密卫生：取回前在 VM 上、取回后在宿主各扫一遍测试口令与 OpenSSH 私钥文件头标记，
**两次均零命中**（`tools/pull-p123-evidence.sh` 内置，命中即中止取回）。写这份结果时又对整个
证据目录扫了一遍，唯一命中是本句自己提到的标记字样，已改写措辞；日志与 payload 里没有任何
口令或私钥内容。
restic 口令仍只经 `RESTIC_PASSWORD` 传入，面板库副本与 SFTP 私钥都没进入证据包。
