# 存储写操作的现场恢复手册

这份文档讲的是**面板动过这台机器的磁盘之后，人怎么自己把它查清楚、改回来**。

它不解释面板怎么实现，只回答三个问题：

1. 面板到底往这台机器上写了什么？
2. 出事的时候先看哪里、按什么顺序查？
3. 怎么回滚，以及哪些“顺手”的动作绝对不能做？

适用范围：P07 起登记的四类设备动作（初始化分区表、建分区、删分区、格式化）与
五个卷动作（纳入管理、挂载、卸载、改挂载选项、退出管理）。

---

## 1. 面板到底写了什么

按“有证据可查”的顺序列，每一条都对应一个**不经过面板**就能读的文件或命令。

### 1.1 数据盘的挂载：`/etc/fstab` 里的一个托管块

面板从不替换整个 `/etc/fstab`。它只维护文件里**唯一的一块**：

```
# BEGIN upanel managed volumes
# 以下条目由 uPanel 维护（同一块内的手工修改会在下次变更时被覆盖）
UUID=6f1a2b3c-4d5e-6f70-8192-a3b4c5d6e7f8 /srv/upanel/volumes/data ext4 rw,nofail,x-systemd.device-timeout=30 0 2
# END upanel managed volumes
```

几条硬事实：

- **块外一个字节都不动**。块之前的内容与块之后的内容（别人后来加的行、注释、空行、缩进）
  在面板替换后保持原样。判断方法：改之前把文件存一份，改完 `diff` 一下，
  差异应当只出现在两个标记之间。
- **块内是整体重渲染**，不做“按行替换”。所以块内的手工修改会在下一次变更时被覆盖——
  这也是块首那行说明的来由。
- **结构不对就拒绝**：BEGIN 没有 END、出现两块 BEGIN、块里有解析不了的行，面板会报错
  `storage: /etc/fstab 的托管块结构不完整` 并**不写任何东西**。它不猜、不修。
- **写前比哈希**：预检时算过整个文件的 SHA-256，落盘前再算一次；不一致就报
  `storage: /etc/fstab 已被外部修改`。这时要先弄清是谁改的，再谈重试。
- **写后先校验再换**：候选文件先过 `findmnt --verify --tab <候选>`，通过之后才原子地
  rename 到 `/etc/fstab`（并 fsync 文件与目录）。校验不过就什么都不换。

### 1.2 条目行的字段含义

`UUID=<uuid> <挂载点> <类型> <选项> <dump> <fsck 顺序>`

| 列 | 面板写什么 | 为什么 |
|---|---|---|
| UUID | 文件系统 UUID（`blkid` 那种标准形态） | 用 UUID 而不是 `/dev/sdX`：设备名会随插拔与启动顺序变 |
| 挂载点 | `/srv/upanel/volumes/<挂载名>` | 由挂载名派生，页面不提供自己填路径的入口 |
| 类型 | `ext4` / `xfs` | 只有这两种在本阶段的支持范围内 |
| 选项 | 见下 | 顺序固定，便于人读与比对 |
| dump | 恒 `0` | 现代系统不用 dump 备份 |
| fsck 顺序 | ext4 → `2`，XFS → `0` | XFS 有自己的日志恢复，让 `fsck.xfs` 跑只会白等 |

选项的固定顺序（`storage.MountOptions`）：

1. `ro` 或 `rw`（第一眼看读写）
2. `nodev`（若开启）
3. `nosuid`（若开启）
4. `nofail` —— **总是加**。一块数据盘不在，不该让这台机器起不来。
5. `x-systemd.device-timeout=30` —— 设备慢时，等 30 秒而不是无限等
6. `noauto`（若关掉了开机自动挂载）

### 1.3 备份：`/var/lib/upanel-agent/fstab-backups/`

- 目录权限 `0700`，每份备份文件 `0600`（里面有整份 fstab）。
- 文件名：`<UTC 时间戳>-<内容短哈希>.fstab`，例如
  `20261021T093145Z-6f1a2b3c.fstab`。排序即时间顺序，最后一份是最近的。
- 备份**先于** rename 存在：rename 之后再备份，备份的就是新内容了，那没有意义。
- 文件原本不存在时，备份是一个空文件——这样“回滚到文件不存在”也是可能的。

```sh
sudo ls -lt /var/lib/upanel-agent/fstab-backups/ | head
sudo diff -u /var/lib/upanel-agent/fstab-backups/<最新那份> /etc/fstab
```

`diff` 的结果就是“面板这次到底改了哪几行”。

### 1.4 挂载点目录

- 目录由面板创建（幂等），权限**上限 0755**：未挂载时往挂载点里写东西会落到根分区上，
  所以不让普通用户写。若目录已存在且组/其他人有写权限，面板会改成 0755。
- 建之前会查：路径上有符号链接、不是目录、父目录权限不对，都会拒绝并报
  `MOUNT_POINT_UNSAFE`，不会“先建了再说”。

### 1.5 分区表与文件系统

- 分区表只写 **GPT**（`sfdisk`），绝不写 MBR。写之前要求这块盘是空的
  （没有分区表、没有文件系统签名、没有挂载）。
- 格式化只用 `mkfs.ext4` / `mkfs.xfs`，卷标上限按文件系统算：ext4 16 字节、XFS 12 字节。
  超限的卷标会被 `mkfs` **静默截断**（P07.3 实测过 rc=0 但名字短了），所以页面上先拦一道。
- 写完之后不读产品自己的日志说“成功”，而是**两路读回**：盘上的字节（直读分区表/超级块）
  与内核的视图（sysfs / `findmnt`）。两条都说一样，才算 `verified`。

---

## 2. 出事时的排查顺序

**先只读，不动手。** 下面这套顺序是为了在改任何东西之前把“现在到底是什么状态”钉死。

### 第一步：一条不经过面板的核对

```sh
sudo scripts/storage-external-check.sh
```

它只读 `/etc/fstab`、`findmnt`、`/proc/self/mountinfo`、`blkid`、`systemctl show`
与备份目录，逐条打印 PASS/FAIL/SKIP，最后给一行 `RESULT`。

- `RESULT 一致` + 退出码 0：fstab 托管块、挂载事实、设备身份能互相对上。
- 有 `FAIL` + 退出码 1：按 FAIL 行逐条查，别重启、别重复提交任务。
- 退出码 2：环境不满足（不是 root、读不到文件、缺工具），先解决这个。

也可以带断言跑（验收时用得上）：

```sh
sudo scripts/storage-external-check.sh --expect /srv/upanel/volumes/data=mounted
```

### 第二步：这行 fstab 引用的盘还在不在

```sh
sudo blkid -U <uuid>          # 有输出 = 这块盘现在插着
sudo blkid -p -s TYPE -o value /dev/sdX   # 直读超级块看类型
ls -l /dev/disk/by-uuid/ | grep <uuid>
```

### 第三步：现在到底挂没挂（两路各读一遍）

```sh
findmnt -n -o SOURCE,FSTYPE,OPTIONS --mountpoint /srv/upanel/volumes/data
grep ' /srv/upanel/volumes/data ' /proc/self/mountinfo
```

**必须是 `--mountpoint`，不能是 `--target`**：`--target` 问的是“哪个挂载覆盖了这个路径”，
它对任何存在的路径都会返回上一层挂载（没挂载的 `/srv/upanel/volumes/data` 会读到 `/dev/sda1`），
于是“没挂”被读成“挂着，挂在根分区上”。`--mountpoint` 问的是“这个路径本身是不是挂载点”，
没挂就是空输出、退出码 1。这条差别是 P07.5 的外部检查脚本真机上撞出来的（DECISIONS Q204）。

两路说法不一致时**以内核为准**（`/proc/self/mountinfo` 是内核的挂载表），
`findmnt` 只是它的一个视图。

### 第四步：systemd 那边怎么看这个条目

```sh
systemctl show -p LoadState --value srv-upanel-volumes-data.mount
systemctl status srv-upanel-volumes-data.mount
```

`LoadState=loaded` 表示 systemd 认得这个单元。条目带 `noauto` 时单元存在但不会被启动，
这**不是**故障。

### 第五步：谁在占着它（卸不掉的时候）

```sh
sudo fuser -vm /srv/upanel/volumes/data     # 或者
sudo lsof +f -- /srv/upanel/volumes/data
```

面板给的占用列表（`GET /storage/volumes/<id>/blockers`）里有 PID 与 comm，
可以拿它对上面两条命令的结果——两边对得上，说明它没编。

---

## 3. 回滚

### 3.1 只回滚 fstab 的那次改动

```sh
# 1) 先看一眼差异，确认这就是你要撤销的那次
sudo diff -u /var/lib/upanel-agent/fstab-backups/<那份备份> /etc/fstab

# 2) 备份当前（不要覆盖旧的备份，新开一个名字）
sudo cp -a /etc/fstab /var/tmp/fstab.before-rollback.$(date -u +%Y%m%dT%H%M%SZ)

# 3) 换回去，保留原权限与属主
sudo install -m "$(stat -c %a /etc/fstab)" -o "$(stat -c %U /etc/fstab)" -g "$(stat -c %G /etc/fstab)" \
    /var/lib/upanel-agent/fstab-backups/<那份备份> /etc/fstab.new
sudo mv /etc/fstab.new /etc/fstab

# 4) 让 systemd 重新读 fstab（只重建单元，不改任何挂载状态）
sudo systemctl daemon-reload
```

第 3 步先写 `.new` 再 `mv`，因为 `mv` 在同一目录内是原子 rename：
中途断电不会留下一个半截的 `/etc/fstab`。

### 3.2 回滚后要确认的两件事

```sh
findmnt --verify --tab /etc/fstab    # 结构校验；有输出就说明有行不对
sudo scripts/storage-external-check.sh
```

`findmnt --verify` 报的是“这行格式/选项有问题”，不是“这会挂不上”——两者都要看。

### 3.3 如果只想让开机时别再挂它（不删数据）

面板里的“退出管理”就是这件事：删掉托管块里它那一行、保留盘与数据。
手工等价的写法是只删那一行，然后 `systemctl daemon-reload`。
它要求这个卷已经卸载、且没有依赖（应用/共享/备份）——有依赖时面板会拒绝，
手工操作时也要先确认这一点，否则开机会挂到一个不存在的位置上。

---

## 4. 绝对不能顺手做的事

| 别做 | 为什么 |
|---|---|
| `mount -a` | 它会把**整台机器**所有条目都挂一遍，等于顺手改了别的盘的挂载状态，不属于任何单个动作的权限范围。面板自己也不执行它（PLAN.md 第 9.3 节明确禁止）。要挂就 `mount /srv/upanel/volumes/data` 一条。 |
| 直接 `rm -rf` 一个**挂载着**的目录 | 删的是盘里的数据，不是那个空目录。先卸载，再动目录。 |
| 在挂载着的卷上 `mkfs` | 面板的预检会拒绝；手工做没人拦你，结果就是数据没了。 |
| 用编辑器“整份重写” `/etc/fstab` | 很容易丢掉结尾换行、把 `UUID=` 拼错、或者顺手改了别人的行。要改就只改一块内的那一行。 |
| 在 `interrupted` 的任务上直接重试 | 中断的意思是**结果未知**：可能已经挂上了。先按第 2 节查，再决定。 |
| 把备份目录里的文件删了“清理一下” | 那是唯一能让你回到上一次状态的东西。它很小，留着。 |
| 给 `/etc/fstab` 加 `defaults` 就以为安全 | `defaults` 不含 `nofail`。盘不在时，开机仍会卡在 emergency mode。 |

---

## 5. 开机起不来的处置（最糟情况）

症状：开机进 emergency mode，或者卡在 `A start job is running for /srv/upanel/volumes/...`。

1. 进 root shell（或者用 live 环境挂上根分区）。
2. **先别改 fstab**，先确认是哪一行出的事：
   ```sh
   cat /etc/fstab
   findmnt --verify --tab /etc/fstab
   blkid -U <可疑的 uuid>
   ```
3. 最可能的两类原因：
   - 盘不在 / UUID 变了 → 该行有 `nofail` 的话本不该阻塞开机，所以先确认 `nofail` 真的在；
   - 托管块被手工改坏（BEGIN/END 不配对）→ 用 3.1 的步骤换回最近一份备份。
4. 需要临时进去的话，在 GRUB 里加 `systemd.unit=multi-user.target` 或在 emergency shell 里
   把那一行注释掉——**注释要连 `#` 一起保留缩进**，改完 `systemctl daemon-reload` 再继续。
5. 起来之后第一件事是跑第 2 节的核对，别急着再点一次面板上的按钮。

---

## 6. 面板不做的事（划清边界，免得误判成故障）

- **不接管非面板写的行**：`/etc/fstab` 里块外的条目归系统管理员，面板只读不写。
- **不改别人的挂载状态**：一次动作只动它自己那一个挂载点。
- **不擦数据**：初始化分区表、删分区都只动分区表条目，不写零；格式化才是真的重写文件系统。
- **不自动修复**：托管块结构被破坏时不猜、不修，报错让人来看。
- **不在没有命令执行器时写 fstab**：`findmnt --verify` 跑不了就等于“没法确认开机能起来”，
  这种情况它直接拒绝写，而不是赌一把。

---

## 7. 相关文档

- `docs/PLAN.md` 第 9 节：存储写的范围与红线（不做什么、为什么）。
- `docs/CONTRACTS.md` 第 5、6 节：动作参数、校验上限、结果字段。
- `docs/STAGES.md` §9：P07 各子任务的验收条件。
- `docs/DECISIONS.md`：Q174–Q203 是 P07 的取舍记录——锁加在整盘与短语动词（Q174）、
  `mount -a` 为什么不能当作开机挂载的证据（Q195）、`umount` 的占用判定按 `st_dev` 才算得准
  （Q191/Q192）、哪些失败路径明确不验（Q196）。`/etc/fstab` 备份目录与文件权限的契约在
  `docs/CONTRACTS.md` 第 6.5 节（`fstab-backups` 条目）。
- `scripts/storage-external-check.sh`：本文第 2 节那条核对命令的实现。
