# 夹具来源（tests/fixtures）

本目录的 P04 夹具记录命令、环境、退出码与采集日期。凡是 `*.synthetic.*` 的文件都是
**手写反例**，不是真实采集结果。

## 真实采集（VM-B，`upanel-2404`）

| 项 | 值 |
|---|---|
| 采集日期 | 2026-10-21（宿主本地时间） |
| 虚拟机 | `upanel-2404`（VM-B），127.0.0.1:2222 |
| 发行版 | Ubuntu 24.04.5 LTS（Noble） |
| 内核 | 见 `os-release-2404.txt` 同机 `uname -r`；本次采集未单独保存 |
| 登录身份 | `upanel`（uid 1000，非 root），只读 `cat` |
| 退出码 | 全部 0 |
| 脱敏 | 无需替换：这些文件不含主机名、用户名、IP、MAC、序列号 |

| 文件 | 命令 |
|---|---|
| `stat-cpu-2404.txt` | `cat /proc/stat` |
| `meminfo-2404.txt` | `cat /proc/meminfo` |
| `loadavg-2404.txt` | `cat /proc/loadavg` |
| `netdev-2404.txt` | `cat /proc/net/dev` |
| `os-release-2404.txt` | `cat /etc/os-release` |
| `uptime-2404.txt` | `cat /proc/uptime` |

`netdev-2404.txt` 只有表头、`lo` 与 `ens4` 三行数据；网卡名是内核分配的名字，
其中的 MAC 与 IP 不在 `/proc/net/dev` 里，因此不需要脱敏。

## 手写反例

| 文件 | 用途 |
|---|---|
| `stat.synthetic.truncated.txt` | cpu 行只有 5 个分量（少于内核固定提供的至少 8 个），必须被判为格式错误，而不是按缺省 0 补齐 |
| `stat.synthetic.no-agg.txt` | 只有 `cpu0`，没有合计 `cpu` 行：没有合计就无从算总使用率，必须报错 |
| `meminfo.synthetic.missing-available.txt` | 删掉 `MemAvailable`：缺必需字段时必须整段失败，不能拿 `MemFree` 顶替 |
| `netdev.synthetic.badcols.txt` | 冒号后只有 15 列（少一列），必须报错 |
| `netdev.synthetic.empty.txt` | 只有表头、没有网卡：必须报错，不能返回空列表冒充“没有网卡” |
| `os-release.synthetic.no-id.txt` | 没有 `ID` 也没有 `NAME`：不是可识别的 os-release，必须报错 |
| `loadavg.synthetic.bad.txt` | 第三个负载值不是数字，必须报错 |

## 存储拓扑夹具（P04.2）

`storage-2404/` 与 `storage-2204/` 是**整棵目录**，不是单个文件：`storage.Scan` 读的是目录树
（`sys/block`、`sys/class/block/<dev>/…`、`dev/disk/by-*`、`proc/self/mountinfo`、`proc/swaps`）
外加盘头部切片。它们的来源、采集时间、宿主机与逐文件 sha256 写在各自的 `MANIFEST.txt` 里，
采集脚本是 `scripts/capture-storage-fixtures.sh`（在验收 VM 内原样拷贝，不改写内容）。

| 目录 | 来源 | 用途 |
|---|---|---|
| `storage-2404/` | Ubuntu 24.04.5 / 6.8.0-147（upanel-2404），2026-10-20T17:33:19Z 采集 | 真机样本：sda 40 GiB + sda1(`/`)/sda14/sda15(`/boot/efi`)/sda16(`/boot`)、sdb 8 GiB、sdc/sdd 4 GiB、8 个 loop、fd0 |
| `storage-2204/` | Ubuntu 22.04.5 / 5.15.0-198（upanel-2204），2026-10-20 采集 | 同一形态的另一代内核（snap loop 挂载、分区号不同） |

两条注意：

- `dev/disk/by-*` 里的条目在真机上是**符号链接**（指向块设备），夹具里是写着目标路径的**普通文件**。
  这正是 P04.2 那个 OOM 缺陷（DECISIONS Q89）逃过夹具单测的原因——只有真机会按符号链接读；
  补上的 `TestReadDiskLinksUsesSymlinkTarget` 用**真的**相对符号链接把两条路径区分开。
- 内容不含主机名以外的敏感信息；`MANIFEST.txt` 里的 UUID 是虚拟机的一次性文件系统标识，不是生产数据。

## SMART 与 /proc/diskstats 夹具（P04.3）

三台虚拟机各采一份，因为它们的磁盘形态不同，这正是三份都要留的理由：

| 虚拟机 | 镜像 | 磁盘形态 | 采集时间（UTC） |
|---|---|---|---|
| `upanel-2404`（VM-B，127.0.0.1:2222） | Ubuntu 24.04.5 / 6.8.0-147 | 4 块 QEMU SCSI 盘（40 GiB + 8 + 4 + 4），无 NVMe | 2026-10-20T19:29:28Z |
| `upanel-2204`（VM-A，127.0.0.1:2223） | Ubuntu 22.04.5 / 5.15.0-198 | 4 块 QEMU SCSI 盘 + snap 的 8 个 loop | 2026-10-20T19:29:29Z |
| `upanel-smart`（VM-E，127.0.0.1:2224） | Ubuntu 24.04.5 / 6.8.0-147 | 1 块 QEMU SCSI 系统盘 + 1 块 QEMU NVMe 4 GiB（加 `-WithUsbDisk` 启动时另有 1 块 2 GiB QEMU USB 盘） | 2026-10-20T19:29:29Z（USB 夹具 21:31:45Z） |

| 文件 | 命令 | sha256 |
|---|---|---|
| `diskstats-2404.txt` | `cat /proc/diskstats`（VM-B，`upanel` 用户） | `a6602041c86125dbeeca2ea6366da2966abe72b52110ac59528abdcd57039d1a` |
| `diskstats-2204.txt` | `cat /proc/diskstats`（VM-A） | `62d9ce3eadaac14852c54be346b2f7194d7abce1a41367756d55d2011e78a053` |
| `diskstats-smart-2404.txt` | `cat /proc/diskstats`（VM-E） | `f2a6c922237c79233c2ccbc0c81e9829c5973b0656d6e618b164f07dad3da8cb` |
| `smart/upanel-2404-sdb-scsi.json` | `sudo -n smartctl -j -i -H -A /dev/sdb`（VM-B，退出状态 0） | `235d4cfedb436d429c4973321a18b1f1ca12b15b0fe76eaefa403f157ef20a14` |
| `smart/upanel-smart-sda-scsi.json` | `sudo -n smartctl -j -i -H -A /dev/sda`（VM-E，退出状态 0） | `8e62443da5fd4de706d3f2f13ce10d584a16476ef332385ebe83439f3cb6bd25` |
| `smart/upanel-smart-nvme0n1.json` | `sudo -n smartctl -j -i -H -A /dev/nvme0n1`（VM-E，退出状态 0） | `27d995e22c3b0dc6c2b62e3766b4af52183b3e8e6502caee81dd02f3a87f46c9` |
| `smart/upanel-smart-usb-unknown-bridge.json` | `sudo -n smartctl -j -i -H -A -n standby /dev/sdb`（VM-E，QEMU USB 盘，**按产品自己的参数**采集，退出状态 1） | `a47cc17bcb1a71a5dfcbc6a4c0680d72c7617d4a1ec1f966f53926bf7ad76095` |
| `smart/upanel-smart-usb-sat.json` | `sudo -n smartctl -j -i -H -A -d sat /dev/sdb`（同一块 USB 盘显式指定 SAT，退出状态 2） | `759d891bac5076070d039c43f778e1219b41dc3552e44c99cb8b73cbc77dfec3` |

`storage-2404/proc/diskstats` 与 `storage-2204/proc/diskstats` 分别是上面两份同名夹具的副本，
与各自的 sysfs 树配套（同一台机器、同一时刻）。内容与源文件 sha256 相同。

真机上观察到的事实（决定了解析逻辑的取舍，记录在此以免下次重新怀疑）：

- QEMU 的 **SCSI** 盘在 smartctl 里是 `smart_support.available=false`、没有 `smart_status`、
  `temperature.current=0`。因此 `temperature ≤ 0` 必须映射成 `null`：SCSI 盘确实没有温度读数，
  把它写成 0 °C 会让界面显示一个不存在的凉快磁盘。
- QEMU 的 **NVMe** 盘给出 `smart_support.available/enabled=true`、`smart_status.passed=true`、
  `critical_warning=0`、`temperature.current=50`，这是本阶段唯一能拿到真实 `healthy` 的来源。
- **睡眠盘在这两个虚拟化形态里造不出来**：`smartctl -s standby,now /dev/sda` 之后
  `/sys/block/sda/device/state` 仍是 `running`，NVMe 的 `runtime_status` 是 `unsupported`。
  所以“休眠跳过”只有单测（`TestSmartProberSleepingDeviceNoSpawn`、`TestSleepingSysfsStates`），
  真机验收里标为 NOT RUN，不拿合成结果冒充真机证据。
- VM-E 试过挂 ATA 盘（`-device ide-hd` / `ich9-ahci`），guest 卡在固件阶段不启动；
  ATA 分支（`ata_smart_attributes`）因此也只有合成输入。限制写在
  `H:\Code\vms\launch-vm-smart.ps1` 的注释里。
- QEMU 的 **USB 盘**（`qemu-xhci` + `usb-storage`，2 GiB）在 smartctl 7.4 里是两个真实的反例，
  两份夹具就是它们的原样输出：
  - 走产品自己的参数（`-n standby`，不带 `-d`）：`/dev/sdb: Unknown USB bridge [0x46f4:0x0001 (0x000)]`
    + `Please specify device type with the -d option`，**退出状态 1**，JSON 里没有 `device`、
    没有 `smart_support`、没有温度；
  - 显式 `-d sat`：`Read Device Identity failed: scsi error unsupported scsi opcode`，
    **退出状态 2**，JSON 里只有 `device.type=sat` / `protocol=ATA`，仍然没有健康与温度。
  两种都必须落到 `unknown` 并带上 smartctl 的原话——USB 桥不认识是“没问出来”，
  既不是 `unsupported`（那是盘没有这个功能），更不是 `healthy`。
- **同一块盘挂两处**（多挂载点）的真机记录取自 VM-E：`mount --bind /srv/upanel/mm/a /srv/upanel/mm/b`
  之后 `/proc/self/mountinfo` 给出 `85 28 8:1 /srv/upanel/mm/a /srv/upanel/mm/b ... - ext4 /dev/sda1 ...`，
  与 `/` 同为 `8:1`。这行原话被 `TestDeviceWithTwoMountPointsKeepsBoth` 追加到 `storage-2404`
  的 mountinfo 副本上，用来证明两个挂载点是两个 `Mount` 条目、按 path 排序，而不是被合并成一条。


### 合成输入（不在本目录，避免被误认成采集结果）

`smartctl` 不会输出坏 JSON，也不会给我们一块正在失效的盘。以下分支的输入直接写在
`internal/storage/smart_test.go` 里，占位符是 `__EXIT__` / `__BODY__`，函数名是 `synthetic`：

| 用例 | 覆盖的分支 |
|---|---|
| `TestEvaluateSmartBitmask` | 退出状态位 1/2/4/8/16/32/64/128 与组合位 |
| `TestEvaluateSmartNVMECriticalWarning` | NVMe `critical_warning` 的第 0—3 位、`media_errors`、`percentage_used=100` |
| `TestEvaluateSmartStandby` | `-n standby` 的“待机未唤醒”与“打不开设备”的区别 |
| `TestEvaluateSmartUnparsableOutput` | 空输出、截断 JSON、非 JSON、权限拒绝 |
| `TestEvaluateSmartEmptyArrays` / `TestEvaluateSmartEmptyJSONObject` | 空数组与空对象 |
| `TestEvaluateSmartATAWhenFailed` | `ata_smart_attributes` 里的 `when_failed` |
