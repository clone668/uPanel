# DISK-02 · LVM 场：根文件系统在 LVM 上

这一场要证明的是同一句话在**另一种叠层设备**上也成立：`/` 落在一个 LV 上时，「不能碰的底层盘」能不能
追到那块物理盘上。读数全文 `2404-lvm-root-guard.log`（**PASS 91 / FAIL 0 / NOT RUN 3**，末行
`场景 lvm：根设备 dm-0 / 成员 sda2 / 物理盘 sda`）。

## 1. 现场怎么造出来的

`H:\Code\vms\images\` 里只有 Ubuntu cloud image，**没有 server installer ISO**，subiquity 的
`layout: lvm` 那条安装路径走不通。走的是"在客机里把根迁过去"：

| 步 | 做了什么 |
|---|---|
| 1 | 第二块虚盘 `/dev/sda`（24 GiB）切两刀：`sda1` 1 MiB `ef02`（bios_grub，**没有文件系统**）、`sda2` 余下 `8e00`（LVM PV） |
| 2 | `pvcreate /dev/sda2` → `vgcreate upanel-vg` → `lvcreate -l 100%FREE -n root` |
| 3 | `mkfs.ext4 -L upanel-root /dev/upanel-vg/root` |
| 4 | `rsync -aHAXx --numeric-ids /` → `/mnt/root`，再单独 `rsync /boot/` |
| 5 | chroot：`fstab` 按 UUID、`rm -rf /boot/efi`、`RESUME=none`、`/etc/initramfs-tools/scripts/local-top/zz-lvm-activate`（`modprobe dm-mod` + `vgchange -ay --sysinit`）、`GRUB_CMDLINE_LINUX="rd.lvm.vg=upanel-vg rd.lvm.lv=upanel-vg/root"`、`update-initramfs -u -k all`、`update-grub`、`grub-install --target=i386-pc` |
| 6 | 关机，摘掉系统盘，只挂这块盘（外加一块 4 GiB 空白盘当反面对照）重启 |

原样留档：`migrate-lvm.sh`、`in-chroot-lvm.sh`。

**`/boot` 故意留在 LV 里面**，不在物理盘上单独开分区。这不是省事——如果 `/boot` 单独占一个分区，
那块物理盘受保护就可能来自"它挂着 `/boot`"，递归就没被证明，只被绕过去了。现在物理盘上唯一不属于
LVM 的东西是 `sda1` 那 1 MiB 的 bios_grub（没有文件系统、没有挂载点），所以 `sda` 的
`system_disk=true` 只能来自递归。

**`/dev/mapper/upanel--vg-root` 的连字符要小心**：VG/LV 名字里的 `-` 在 dm 名里写成 `--`，分隔符只写
一个 `-`。用 `s/[-]/--/g` 去转义会得到不存在的 `upanel--vg--root`；脚本里一律走 `/dev/<vg>/<lv>`。

### 迁完之后的现场自证（脚本 §0 自己量的）

```
/dev/mapper/upanel--vg-root -> /dev/dm-0      ← findmnt -no SOURCE /
dm/uuid 前缀 LVM-                              ← 拓扑里应显示为 lvm
根设备的成员（sysfs slaves）：sda2
sda 24G disk   sda1 1M part(bios_grub)   sda2 24G part LVM2_member └─dm-0 lvm ext4 /
sdb  4G disk   ← 空白，反面对照
```

## 2. 读数

### 2.1 拓扑：递归追到了那块物理盘

`payloads/topology.json`（原始响应体，14 个节点 / 3 条边）：

| 设备 | kind | `protection.system_disk` | `protection.sources` | `filesystem` |
|---|---|---|---|---|
| `dm-0` | lvm | **true** | `["dm-0"]` | ext4，挂 `/` |
| `sda2`（PV） | partition | **true** | `["dm-0"]` | null |
| `sda` | disk | **true** | `["dm-0"]` | **null** |
| `sda1`（bios_grub） | partition | false | `[]` | null |
| `sdb`（备用） | disk | false | `[]` | null |

三条边：`dm-0 --depends_on--> sda2`、`sda2 --partition_of--> sda`、`sda1 --partition_of--> sda`。

**`sda` 的 `filesystem` 是 `null` 而 `system_disk` 是 `true`**——它自己不承载文件系统、不是挂载点、
不是交换区。这个 `true` 只能是沿边递归标上去的。`sda1` 是同一块盘上的分区、但不是根链路，所以 `false`：
递归沿链走，不按盘染红。

### 2.2 四条写动作全部被拒

| 目标 | 动作 | `allowed` | 阻塞码 | `locks` |
|---|---|---|---|---|
| `sda` | `storage.partition_table.initialize` | false | `SYSTEM_DISK_PROTECTED` | `disk:sda` |
| `sda2` | `storage.partition.delete` | false | `SYSTEM_DISK_PROTECTED` | `disk:sda` |
| `sda2` | `storage.filesystem.format` | false | `SYSTEM_DISK_PROTECTED` | `disk:sda` |
| `dm-0` | `storage.filesystem.format` | false | `SYSTEM_DISK_PROTECTED` | `disk:sda` |

`payloads/preflight-root-format.json` 是对 `dm-0` 走格式化那条的原始响应体：`allowed=false`，
`blockers = IDENTITY_UNKNOWN, SYSTEM_DISK_PROTECTED, DEVICE_MOUNTED, UNSUPPORTED_DEVICE_KIND`，
`locks = ["disk:sda"]`。

这一场只有一根 PV，所以「锁只锁目标自己那块盘」与「锁住整台机所有物理盘」**在每一条断言上同值**——
这就是为什么必须再造一台 RAID 根盘：两种读法要靠两块成员盘才分得开（见 `../raid/README.md` §3）。

### 2.3 一个字节都没写

整盘首末 1 MiB 哈希前后一致、`sda2` 的 `LVM2_member` 签名还在、`/sys/block/dm-0` 的 sysfs 身份没变、
根仍在同一个设备上、零存储写任务；代理启动日志里有「磁盘写原语已接线」，且没有「动作执行失败」
「执行前复核被拒」「动作缺少执行实现」。**先证明工具接上了，那个"一次都没执行"才有意义。**

反面对照：备用盘 `sdb` 不受保护、`initialize` 能力 `available`、预检 `allowed=true`、锁只有它自己、
阻塞列表里没有 `SYSTEM_DISK_PROTECTED`——递归没有把整台机锁死。

## 3. 明确不验（NOT RUN）

与 `../raid/README.md` §4 同一张表：降级/重建中的阵列、根在叠层设备上写满、执行期复核。

## 4. 文件清单

| 文件 | 内容 |
|---|---|
| `README.md` | 本文件 |
| `2404-lvm-root-guard.log` | 修正后的读数全文（91/0/3） |
| `runs1-pre-fix/2404-lvm-root-guard.log` | 脚本改正之前的同一场读数（**也是 91/0/3**，但那一版 5 条锁断言写的是错期望，在这一场恰好同值所以没暴露） |
| `migrate-lvm.sh` / `in-chroot-lvm.sh` | 迁移脚本原样字节 |
| `payloads/topology.json` | `GET /api/v1/storage/topology` 原始响应体 |
| `payloads/preflight-root-format.json` | 对 `dm-0` 格式化的预检原始响应体 |
