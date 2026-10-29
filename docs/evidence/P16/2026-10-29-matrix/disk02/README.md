# DISK-02 · 根盘在叠层设备上（LVM / RAID）→ 递归保护

矩阵里 `DISK-02` 是**最后一条"部分"**。它缺的不是代码，是现场：`根盘在 LVM/RAID 下` 这件事
从 P00 起就没造出来过，两处账本自己写着「`dmsetup`/`mdadm` 未构造」「md 只在单测夹具里覆盖」
（`P04/2026-10-20-p04.2/result.md:93`、`P07/2026-10-21-p07.1/result.md:190`）。

这一轮把两个现场都造了出来，用的是**同一份脚本、同一批二进制**：

| 场 | 客机 | 根设备 | 成员 | 物理盘 | 读数 |
|---|---|---|---|---|---|
| `lvm/` | `upanel-lvm`，Ubuntu 24.04 | `/dev/mapper/upanel--vg-root`（`dm-0`） | `sda2`（PV） | 1 块 | **91 PASS / 0 FAIL / 3 NOT RUN** |
| `raid/` | `upanel-md`，Ubuntu 24.04 | `/dev/md0`（RAID1） | `sda2` + `sdb2` | 2 块 | **113 PASS / 0 FAIL / 3 NOT RUN** |

两台都不是装出来的，是**在客机里把根迁过去**造的（cloud image 没有 server installer ISO，
subiquity 的 `layout: lvm|raid` 走不通）：第二/三块虚盘 → PV/VG/LV 或 `mdadm` → mkfs → `rsync` 整个根
→ chroot `grub-install` → 换盘重启。脚本原样留档在各自的目录里。

## 为什么必须是两台，不是一台

因为**一台单盘机分不出这两件事**：

* 「锁只锁目标自己那块盘」和「锁住整台机所有物理盘」
* 「保护是沿链递归标上去的」和「这块盘上有根所以整盘染红」

LVM 那一场只有一根 PV，两种读法在每一条断言上**恰好同值**。RAID1 有两块成员盘，两种读法立刻分开——
第一场就因此红了 5 条锁断言（`raid/runs1-pre-fix/`）。查下来是**脚本的期望写错**，产品与
`_test.go` 零改动：`chain()` 是沿**目标自己**的 `parents` 往上追（`internal/storage/assess.go:761`），
CONTRACTS:279 的原话也是「保护和锁沿 `depends_on`/`partition_of` 递归追踪」。详见
`raid/README.md` §3 与 `docs/DECISIONS.md` Q522。

所以第二台机器买到的不是"多一个绿勾"，是**把一条写错的期望抓了出来**。原来的断言只在"块数=1"
时能过；现在 5 条按目标自己的链算，另加 1 条「两块盘时单盘目标的锁不许含另一块盘」——**太窄和太宽
都夹住了**。

## 这一条到底证明了什么

DISK-02 的原话是「递归保护，不能破坏任何底层盘」。拆成三个能读的子句：

| 子句 | 怎么证明 | 读数 |
|---|---|---|
| **递归真的发生了** | 每块物理盘 `protection.sources` **只有**根设备的 id，而它自己 `filesystem` 是 `null`（不是挂载点、不是交换区、不承载文件系统）——它的"受保护"只可能来自递归 | lvm `sda → ['dm-0']`；raid `sda/sdb → ['md0']` |
| **叠层各层都被拒** | 整盘 / 成员分区 / 根设备本身，四条写动作预检全 `allowed=false` + `SYSTEM_DISK_PROTECTED` + 理由里说得出"系统数据" | 两场都是 4/4 |
| **一个字节都没写** | 每块整盘首末 1 MiB 哈希前后一致、成员签名（`LVM2_member` / `linux_raid_member`）还在、sysfs 身份没变、零存储写任务；同时代理日志确认**写原语已接线**，所以"没执行"不是"缺工具" | 两场都过 |

反证也在：`sda1`（LVM 那场的 bios_grub）与 `sda1`/`sdb1`（RAID 那场的 bios_grub）**都不受保护**——
它们同样在这两块盘上，但不在根链路上。递归是沿链走的，不是按盘染红。
再加上一块 4 GiB 空白备用盘（LVM 是 `sdb`，RAID 是 `sdc`）：不受保护、`initialize` 能力 `available`、
预检 `allowed=true`、锁只有它自己——**递归没有把整台机锁死**。

## 明确不验（两场共用同一张表，不假装通过）

| 项 | 为什么不做 |
|---|---|
| 降级 / 重建中的 RAID（成员 `faulted`、`rebuild` 进行中） | 现场没造。递归不看阵列健康，但没有真机读数就不算验过 |
| 根在叠层设备上写满（ENOSPC） | P16.4 的 ENOSPC 现场是独立 tmpfs；把根写满会伤到这台机的系统盘 |
| 执行期复核（`PREFLIGHT_STALE` / `DEVICE_IDENTITY_CHANGED`） | 预检就拒了，提交门到不了；那两条由 P07.2 在一次性盘上验 |

## 读数绑定的字节

两份读数跑的是同一批二进制（`binaries.txt`）：agent `5ad12b15…5533`、web `cd6dc5ee…9a77`、
ctl `563f9389…425b`，`go build -trimpath` 直出、**没有**走 `scripts/release.sh`（所以没有 `BuildDate`）。
验收脚本 `tests/integration/p16-5-root-disk-recursion.sh` 在 RAID 第一场之后改过（Q522），
**两份存档读数都是改后那份**（`c07b0e4f…e56b`），改之前的两场读数留在各自的 `runs1-pre-fix/`。

## 文件清单

| 路径 | 内容 |
|---|---|
| `README.md` | 本文件 |
| `binaries.txt` | 两份读数共用的二进制与脚本 sha256、仓库状态、两台客机的环境 |
| `lvm/README.md` | LVM 场：现场怎么造、拓扑读数、预检读数 |
| `lvm/2404-lvm-root-guard.log` | 读数全文，末行 `PASS 91 / FAIL 0 / NOT RUN 3` |
| `lvm/migrate-lvm.sh`、`lvm/in-chroot-lvm.sh` | 迁移脚本原样字节 |
| `lvm/payloads/topology.json`、`lvm/payloads/preflight-root-format.json` | 原始响应体 |
| `raid/README.md` | RAID 场：现场怎么造、★ 锁断言的更正（Q522） |
| `raid/2404-raid-root-guard.log` | 读数全文，末行 `PASS 113 / FAIL 0 / NOT RUN 3` |
| `raid/migrate-md.sh`、`raid/in-chroot-md.sh` | 迁移脚本原样字节 |
| `raid/payloads/topology.json`、`raid/payloads/preflight-root-format.json` | 原始响应体 |
| `*/runs1-pre-fix/` | 脚本改正之前的两场读数，保留不删 |
