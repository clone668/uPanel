# DISK-02 · RAID 场：根文件系统在 RAID1 上

这一场要回答的问题只有一个：**当 `/` 落在一块 `md` 阵列上，「不能碰的底层盘」这句话能不能真的追到
两块成员盘上？** 不是"预检会不会拒"——预检拒在 P07 就验过了；是**递归**：物理盘自己没有挂载点、
没有文件系统、也不是任何东西的使用者，它的"受保护"只可能是从根设备顺着 `depends_on`/`partition_of`
标上去的。

读数全文：`2404-raid-root-guard.log`（**PASS 113 / FAIL 0 / NOT RUN 3**，末行 `场景 raid：根设备 md0 /
成员 sda2 sdb2 / 物理盘 sda sdb`）。

## 1. 现场怎么造出来的

`H:\Code\vms\images\` 里只有 Ubuntu cloud image，**没有 server installer ISO**，所以 subiquity 的
`layout: lvm|raid` 那条安装路径走不通。走的是"在客机里把根迁过去"：

| 步 | 做了什么 |
|---|---|
| 1 | 第二、第三块虚盘 `/dev/sdb` `/dev/sdc`（各 12 GiB）各切两刀：`sdb1` 1 MiB `ef02`（bios_grub，**没有文件系统**）、`sdb2` 余下 `fd00`（Linux RAID）；`sdc` 同 |
| 2 | `mdadm --create /dev/md0 --level=1 --raid-devices=2 --metadata=1.2 --uuid=1a2b3c4d:5e6f7a8b:9c0d1e2f:3a4b5c6d --name=root`，等 `array_state` 变 `clean` |
| 3 | `mkfs.ext4 -L upanel-md-root /dev/md0`（文件系统 UUID `0ce2df56-02a7-4aa4-960b-0bc1543d2068`） |
| 4 | `rsync -aHAXx --numeric-ids /` → `/mnt/root`，再单独 `rsync /boot/`（`/boot` 跟着根走，**不单独分区**） |
| 5 | chroot：`fstab` 写 `UUID=<md0 的 ext4 UUID>`、`rm -rf /boot/efi`、`RESUME=none`、`/etc/mdadm/mdadm.conf` 取自 `mdadm --detail --scan`、`/etc/initramfs-tools/scripts/local-top/zz-md-assemble`、`update-initramfs -u -k all`、`update-grub`、`grub-install --target=i386-pc` **在两块成员盘上各一次** |
| 6 | 关机，摘掉系统盘，只挂两块成员盘（外加一块 4 GiB 空白盘当反面对照）重启 |

原样留档：`migrate-md.sh`、`in-chroot-md.sh`。

**为什么第 5 步两条 `grub-install` 都要跑**：RAID1 的 MBR 不是冗余的——阵列的引导扇区不在阵列里，
在两块盘各自的 MBR 上。只装一块，那块盘坏了就开不了机，而 RAID1 的全部意义就是那块盘坏了还能开。

### 迁完之后的现场自证（脚本 §0 自己量的，不是搬过来的说法）

```
/dev/md0            ← findmnt -no SOURCE /
sda 12G disk        sda1 1M part   sda2 12G part linux_raid_member └─md0 raid1 ext4 /
sdb 12G disk        sdb1 1M part   sdb2 12G part linux_raid_member └─md0 raid1 ext4 /
sdc  4G disk        ← 空白，反面对照
md0 : active raid1 sdb2[1] sda2[0]   12571584 blocks super 1.2 [2/2] [UU]
array_state = clean
```

`lsinitramfs` 里能看到 `etc/mdadm/mdadm.conf`、`scripts/local-block/mdadm`、`scripts/local-bottom/mdadm`
——装配发生在 initramfs 里，**不是靠内核对 metadata 1.2 的自动装配**（那条路现代内核上是关的）。
`grub.cfg` 的搜索行是 `--hint='mduuid/1a2b3c4d5e6f7a8b9c0d1e2f3a4b5c6d'` 优先、`0ce2df56-…` 兜底。

## 2. 读数

### 2.1 拓扑：递归追到了两块盘

`payloads/topology.json`（原始响应体，17 个节点 / 6 条边）：

| 设备 | kind | `protection.system_disk` | `protection.sources` | `filesystem` |
|---|---|---|---|---|
| `md0` | raid | **true** | `["md0"]` | ext4，挂 `/` |
| `sda2` | partition | **true** | `["md0"]` | null |
| `sdb2` | partition | **true** | `["md0"]` | null |
| `sda` | disk | **true** | `["md0"]` | **null** |
| `sdb` | disk | **true** | `["md0"]` | **null** |
| `sda1` / `sdb1`（bios_grub） | partition | false | `[]` | null |
| `sdc`（备用） | disk | false | `[]` | null |

四条边凑成两条完整的链：`md0 --depends_on--> sda2 --partition_of--> sda`、
`md0 --depends_on--> sdb2 --partition_of--> sdb`。

**关键在两块物理盘的 `filesystem` 是 `null` 而 `system_disk` 是 `true`**：它们自己不承载文件系统、
不是挂载点、不是交换区，所以这个 `true` 不可能是"它挂了 `/`"得来的，只能是沿边递归标上去的。
`sda1`/`sdb1` 是反证——同样是这块盘上的分区，但不在根链路上，于是 `false`。**递归是沿着链走的，不是
按"这块盘上有根就整盘染红"。**

### 2.2 四条写动作全部被拒，锁按目标自己的链走

| 目标 | 动作 | `allowed` | 阻塞码 | `locks` |
|---|---|---|---|---|
| `sda` | `storage.partition_table.initialize` | false | `SYSTEM_DISK_PROTECTED` | `disk:sda` |
| `sdb` | 同上 | false | `SYSTEM_DISK_PROTECTED` | `disk:sdb` |
| `sda2` | `storage.partition.delete` | false | `SYSTEM_DISK_PROTECTED` | `disk:sda` |
| `sda2` | `storage.filesystem.format` | false | `SYSTEM_DISK_PROTECTED` | `disk:sda` |
| **`md0`** | `storage.filesystem.format` | false | `SYSTEM_DISK_PROTECTED` | **`disk:sda` + `disk:sdb`** |

`payloads/preflight-root-format.json` 是最后那一条的原始响应体：`allowed=false`，
`blockers = IDENTITY_UNKNOWN, SYSTEM_DISK_PROTECTED, DEVICE_MOUNTED, UNSUPPORTED_DEVICE_KIND`，
`locks = ["disk:sda","disk:sdb"]`。

**锁不是保护。** 目标是设备本身时锁它自己（互斥），目标是叠层设备时把依赖链上的**每一块**物理盘都锁上。
过宽的锁会让两块无关的盘上的动作互相排队，所以脚本里同时有一条"单盘目标的锁不许含别的盘"。

### 2.3 一个字节都没写

| 断言 | 读数 |
|---|---|
| 每块整盘首 1 MiB / 末 1 MiB 哈希前后一致 | 4/4 PASS |
| `sda2` / `sdb2` 的 `linux_raid_member` 签名还在 | PASS |
| `/sys/block/md0` 的 sysfs 身份没变、根仍在同一个设备上 | PASS |
| 没有任何存储写任务被执行 | PASS |
| 代理启动日志里有「磁盘写原语已接线」（`sfdisk`/`udevadm`/`mkfs.ext4`/`mkfs.xfs`） | PASS |
| 日志里没有「动作执行失败」/「执行前复核被拒」/「动作缺少执行实现」 | 3/3 PASS |
| 日志里没有明文口令 | PASS |

最后两条是连在一起读的：**先证明写原语真的接上了**，那个"一次都没执行"才有意义——否则"没执行"可能只是
"工具没装"。

## 3. 第一场为什么红了 5 条：验收脚本的期望写错了（Q522）

第一场 `runs1-pre-fix/2404-raid-root-guard.log` 是 **PASS 107 / FAIL 5 / NOT RUN 3**。五条全在锁上：

```
FAIL  整盘初始化的锁覆盖每一块底层盘: 期望 'disk:sda,disk:sdb'，实际 'disk:sda'
FAIL  成员分区删除的锁覆盖每一块底层盘: 期望 'disk:sda,disk:sdb'，实际 'disk:sda'
...（根设备格式化那一条是 PASS）
```

查下来**产品没错，脚本错了**：

* `internal/storage/assess.go:761` 的 `chain()` 从**目标**出发沿 `parents` 往上追。目标 `sda` 自己就是
  物理盘 → 链上只有 `sda` → 一把锁；目标 `md0` → `sda2`/`sdb2` → `sda`/`sdb` → 两把锁。
* CONTRACTS:279 的原话是「保护和锁沿 `depends_on`/`partition_of` 递归追踪」——从目标自己追；
  CONTRACTS:424 对单动作写的锁就是「整盘（`disk:<disk_id>`）」。
* 锁是**互斥**用的，保护看 `SYSTEM_DISK_PROTECTED`。混成一件事，就会把"目标只锁自己那块盘"
  读成"递归没生效"。

**这个错期望在 LVM 那场恰好同值**：只有一根 PV，两种读法都得出 `disk:sda`，所以 `91/0/3` 一直没暴露它。
一台只有一块数据盘的机器分不出"目标自己的链"和"整台机所有物理盘"——**这正是非要再造一台 RAID 根盘的原因，
不是为了多一个绿勾。**

改法不是删断言：

1. 五条改成按目标自己的链算（新增 `lock_for()`，走 sysfs 判分区还是整盘）；
2. 再加一条更严的：**两块盘时，单盘目标的锁不许含另一块盘**——原来的断言只在"块数=1"时才能通过，
   新断言在"块数>1"时才执行，两者合起来把"太窄"和"太宽"都夹住了。

## 4. 明确不验（NOT RUN，不假装通过）

| 项 | 为什么不做 |
|---|---|
| 降级 / 重建中的 RAID（成员 `faulted`、`rebuild` 进行中） | 现场没造。递归不看阵列健康，但没有真机读数就不算验过 |
| 根在叠层设备上写满（ENOSPC） | P16.4 的 ENOSPC 现场是独立 tmpfs；把根写满会伤到这台机的系统盘 |
| 执行期复核（`PREFLIGHT_STALE` / `DEVICE_IDENTITY_CHANGED`） | 预检就拒了，提交门到不了；那两条由 P07.2 在一次性盘上验，与根在不在叠层设备上无关 |

## 5. 文件清单

| 文件 | 内容 |
|---|---|
| `README.md` | 本文件 |
| `2404-raid-root-guard.log` | 修正后的读数全文（113/0/3），末行是场景自述 |
| `runs1-pre-fix/2404-raid-root-guard.log` | 第一场红读数（107/5/3），保留，理由是它记着"脚本曾经这么期望过" |
| `migrate-md.sh` / `in-chroot-md.sh` | 迁移脚本原样字节 |
| `payloads/topology.json` | `GET /api/v1/storage/topology` 的原始响应体 |
| `payloads/preflight-root-format.json` | 对 `md0` 格式化的预检原始响应体（含两个 `disk:` 锁） |
