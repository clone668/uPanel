# P14.4 验收结果：NFS 受管理导出——网段是唯一的访问控制

**日期**：2026-10-27（VM 内时间 2026-10-26T20:40:27Z — 20:44:24Z，UTC）
**机器**：Ubuntu 22.04.5 / kernel 5.15.0-198-generic / nfs-kernel-server 1:2.6.1-1ubuntu1.2 / nfs-common 同版本，QEMU slirp，`ens4`
**结果**：**PASS 141 / FAIL 0 / NOT RUN 6**，退出码 0（第七轮）
**脚本**：`tests/integration/p14-4-acceptance.sh`，sha256 `8c732998bd0b1e68730f136cc821dd1c69bbfbec3bc152dd560a7b86b26aa1af`
**二进制**：`upanel-agent 3a0caa42…cfc56de9`、`upanel-web e269a391…4102e159`、`upanelctl fb39b0c1…089ff6b0a5`（宿上交叉编译的同一组，VM 内 `sha256sum` 核对一致）

---

## 1. 结论

**P14.4 的四个验收点在真机上成立。**

1. **只维护一份文件、认领靠行尾标记**：读写导出写进 `/etc/exports.d/upanel.exports`，行的
   结尾是 `# upanel:<share_id>`；`/etc/exports`（管理员那份）前后 sha256 逐字节相同。
   停用/删除之后托管文件里一条导出都不剩（只剩三行说明），目录与文件一个不少、内容哈希不变。
2. **网段是唯一的访问控制**：把允许网段改成 `192.168.77.0/24` 之后，同一台机器（`127.0.0.1`）
   挂不上；改回 `127.0.0.0/8` 立刻又能挂上。中间没有任何账号、ACL、密码参与。
3. **只读是两道锁**：导出行 `ro` + 目录 `0755`。客户端把文件写进去被内核以 `Read-only file
   system` 拒绝，服务端目录里没有多出这个文件。
4. **卷依赖与运行时撤销**：卷在挂载树里消失之后，30 秒一次的撤销循环把导出行摘掉、重新
   `exportfs -ra`、写 `runtime_revoked` 与 `config_applied=false`，实测 **13 秒**完成；卷挂
   回来**不会**自动复活，重新应用才恢复，客户端随后又能挂上并读到之前写进去的文件。

**这一轮跑了七次才全绿，而七次里真正暴露产品缺陷的只有两次**（第 1 轮的迁移 CHECK、第 6 轮
的同目录冲突）。其余五次的红全部来自**验收脚本自己**——从一条少写 `$` 的命令替换，到
"要求 disable 顺手删记录"这种与契约相反的断言。逐条记在第 4、5 节：这一轮最贵的成本不是写
NFS 代码，是**把红的理由一条条查到根**。

---

## 2. 这一轮验的范围

STAGES 对 P14.4 的要求是「NFS 受管理导出、网段/root_squash、卷依赖、真实客户端读写验收」。
拆成 12 个阶段、141 条断言：

| 阶段 | 断言 | 验的是什么 |
|---|---|---|
| 0 环境与起点 | 24 | root、三个二进制、12 个夹具工具、`nfs-server` 在跑；一次性盘不是系统盘；**上一轮的 drop-in / fstab 块 / 客户端挂载先清掉**；`/etc/exports` 与 `exports.d` 整份留档后清成干净起点 |
| 1 装配 | 6 | 代理以 root 起、Web 以 `upanel-web` 起（`ps` 核 uid），密钥与管理员初始化 |
| 2 夹具 | 8 | 分区表初始化 → 建分区 → `ext4` → 导入为受管理卷 → 挂载 |
| 3 能力报告 | 8 | `capability.nfs`：`available`、`exportfs` 绝对路径、`exports_dir_ready`、`service_state=active`、此刻没有面板写的导出 |
| 4 应用读写导出 | 21 | `verified`、`served=True`、目录 `0777`、行里带认领标记与网段、`rw`、**没有** `no_root_squash`/`async`/`insecure`/`*`、`exportfs -v` 里路径在且带 `root_squash`；幂等重放 `unchanged` |
| 5 真实客户端 | 12 | NFSv3 `vers=3,proto=tcp` 挂载 → 先 `findmnt` 再读写 → 服务端读回哈希一致 → root 写入落盘为 `nobody:nogroup` |
| 6 网段是唯一访问控制 | 9 | 改成名单外网段 → 挂载非 0 退出、`findmnt` 为空；改回来又能挂上 |
| 7 只读共享 | 14 | 第二轮导出 `ro` + 目录 `0755`、`managed_line_count=2`、客户端读得到、写被内核拒绝且原因是只读、服务端没多出文件 |
| 8 同路径冲突 | 3 | 同一目录的第二条导出在**预检**被 `EXPORT_PATH_CONFLICT` 拦下，实际导出表没被动 |
| 9 卷依赖 | 21 | 有启用导出时面板拒卸载（`VOLUME_IN_USE_BY_SHARE`）；卷摘出命名空间 → 撤销记录/文件/导出表/客户端四面一致；挂回来不复活；重新应用清除撤销 |
| 10 撤干净 | 13 | 停用 + 删除：导出表与托管文件都清空、目录与文件还在、`/etc/exports` 哈希不变、只剩一条已停用的共享记录 |
| 11 结论 | 6 | 6 条 NOT RUN 逐条说明为什么没验 |

---

## 3. 真机验到的四件硬事

### 3.1 认领标记是唯一判据，别人的文件一个字都不碰

托管文件头三行是说明，正文是导出。`/etc/exports`（只有 `/srv/upanel-nfs *(ro,async,…fsid=0)`）
在整轮里 sha256 前后相同（阶段 10 的 `MAIN_SHA_BEFORE`）。别的 `.exports` 文件只被读来做冲突
检测，不被改写、也不导入成可编辑共享。

### 3.2 `exportfs -ra` 的退出码不是证据，判决一律读回 `exportfs -v`

阶段 4、7、9、10 的核心断言全部读 `exportfs -v` 的实际导出表，按**路径整行**匹配再取其缩进
的客户端行。形状（`cat -A` 实测）：路径单独一行、无尾空格；客户端行两个 tab 缩进。

### 3.3 root_squash 是行为，不是配置项

阶段 5：客户端以 root 写进共享的文件，在服务端落盘属于 `nobody:nogroup`。阶段 7：服务端放
的文件若只有 root 能读（`0600`），被 squash 成 `nobody` 的客户端**读不到**——这是 root_squash
正常工作，不是缺陷（脚本因此显式 `chmod 0644`）。

### 3.4 卷从挂载树里消失之后，撤销只减不增

阶段 9：`runtime_revoked.volume_state=unmounted`、`config_applied=false`、托管文件里没有那条
路径、实际导出表里也没有、客户端挂不上；卷挂回来之后上述五项**一样不变**，只有重新
`share.apply` 才恢复（`verified`，撤销记录清掉）。

---

## 4. 这一轮暴露的产品缺陷（2 条，真机先红、修完再验）

| # | 现象（真机） | 根因 | 修法 |
|---|---|---|---|
| 1 | `share.apply` 写好了导出、`exportfs` 也吃下去了，最后在 `SaveShare` 被 `CHECK` 挡住：`shares.protocol CHECK (protocol IN ('smb'))` | 0005 迁移把协议钉死成 smb，P14.4 的新协议没有对应迁移 | 新增 `0013_shares_protocol_nfs.sql`：表重建（建新表→拷数据→删旧表→改名→重建两个索引），并把"旧 schema 真的拒绝 nfs"写进 `TestMigration0013RebuildsSharesAndKeepsRows` |
| 2 | 同路径的第二条导出，预检 `allowed=true` | `ExportConflicts` 只比"别人的导出文件"，面板自己的两条共享指向同一个目录没人拦 | 新增 `SameDirShares`（纯函数）+ `sharesSharingDir`（预检），执行期再拦一次；判据同一个函数，CONTRACTS §7(5) 补上两个来源 |

另有一条**同类未修**：SMB 路径（`internal/shares/write.go:230`）在配置生效之后落库失败时不回
滚。修它要把 `configState` 带出 `applyManagedConfig` 并在还原后 reload `smbd`，属于 P08 代码
路径，本轮不动，已登记在 DECISIONS（Q472）。

---

## 5. 验收方法自身的缺陷（7 类，逐条记，因为它们会伪装成产品故障）

| 现象 | 真相 |
|---|---|
| `PRE_ALLOWED: not found` + 之后整片红 | 脚本写了 `"$(PRE_ALLOWED)"`（少一个 `$`），把变量当命令执行。一条语法错把第 7—10 节全带红了 |
| "两条导出"数成 3、"一条都不剩"数成 1 | `grep -c '# upanel:'` 把文件头那句示例注释也数进去 |
| 只读共享的客户端挂载卸掉之后，卷仍然 `target is busy` | `umount -l` 只摘命名空间，本机 nfsd 仍持有文件系统（`fuser -vm` 点名 `kernel knfsd`）。改成硬卸优先 |
| "挂载选项里是 ro" 永远红 | NFSv3 客户端的 `findmnt` OPTIONS 永远报 `rw`；只读是**服务端拒绝**（EROFS）。这条断言不成立，改成留档 + 用写入被拒当证据 |
| "客户端读得到服务端放进去的文件" | 脚本以 root 写、sudo 带下来的 umask 是 077 → `0600` → 被 squash 的 `nobody` 读不到。夹具没给权限，不是产品 |
| "共享列表回到空数组" | `share.disable` 按契约**保留记录**（`enabled=false`）。断言改成"只剩一条已停用的 `media-ro`、没有一条启用、没有一条 `config_applied`" |
| 第一个应用任务失败后，后面 60 多条断言全用垃圾值变红 | 加了"前置失败即收手"护栏 + `expect_share_id`（`^shr_[0-9a-f]{32}$`） |

---

## 6. 没验的部分（NOT RUN，逐条说明）

| 项 | 为什么没验 |
|---|---|
| 从另一台机器挂进来 | VM 里只有本机；网段限制用"改名单"在本机验过，跨机器链路没跑过 |
| Windows / macOS 的 NFS 客户端 | 没有这两种测试机；地址形态由 web 侧断言（`web/src/api/shares.test.ts`） |
| NFSv4 直挂 | 面板不假设 v4（`fsid=0` 假根会挡）；脚本只跑 `vers=3,proto=tcp`，v4 结果只记录在 `facts/mount-v4.txt` |
| 两条导出并发写托管文件 | 锁行为由 `internal/shares/exports_test.go` 覆盖；真机并发会把冲突变成时序问题 |
| kerberos（`sec=krb5*`） | 这一版固定 `sec=sys`，没有这一层 |
| 设备级掉线（拔盘 / I/O 错误之后 `statfs` 报错） | 这台 VM 是 `-monitor none` 起的，跑的时候没法把 sdb 拔掉；验过的是"挂载点从命名空间里消失之后面板做什么"（lazy umount） |

---

## 7. 文件

- `logs/run.log`：VM 内完整输出（141 PASS / 0 FAIL / 6 NOT RUN）
- `RESULT`：脚本写的结论行（`PASS=/FAIL=/NOT RUN=` 与失败项名字）
- `facts/lsblk-initial.txt`：夹具盘起点
- `facts/mount-rw.txt`、`facts/mount-rw-findmnt.txt`：读写共享的客户端挂载与 `findmnt`
- `facts/mount-ro.txt`：只读共享的 `findmnt` OPTIONS（`rw`）、客户端看到的内容、服务端目录
- `facts/mount-foreign-cidr.txt`：名单外网段挂载失败原文
- `facts/mount-after-revoke.txt`：撤销之后客户端挂不上的原文
- `facts/mount-v4.txt`、`facts/mount-v4.err`：NFSv4 尝试的原始结果（**不作为通过项**）
- `logs/host-gotest.txt`：验收结束后在宿上跑的 `go test` / `go vet` / `gofmt -l`
