# 验收测试环境

本文是 P00.3 的产出。目标不是“跑一下看看”，而是给出可重复、可审计、与开发宿主物理隔离的
Ubuntu 验收环境定义，以及每个阶段需要什么证据。

**当前状态（2026-10-20 更新）：VM-A 与 VM-B 已创建并核验通过。**
- 宿主无管理员权限，但已启用 Windows Hypervisor Platform（`HypervisorPresent=True`、`WinHvPlatform.dll` 存在），
  因此使用**免安装、用户级便携 QEMU 11.1.0**（`H:\Code\.toolchains\qemu`，仓库之外，无系统级安装）搭配 `-accel whpx`。
- VM-A（Ubuntu 22.04.5 LTS，内核 5.15.0-198）与 VM-B（Ubuntu 24.04.5 LTS，内核 6.8.0-147）均为
  2 vCPU / **4096 MB** / 系统盘 40 GiB + 三块虚拟数据盘，序列号与本文件第 1 节表格完全一致。
- 隔离证据与核验输出见 `docs/evidence/P00/2026-10-20/vm-2404-verification.txt`、
  `vm-2204-verification.txt`、`host-isolation.txt`；创建过程见第 8 节。
- **仍未创建**：VM-C（LVM 根盘场景，P04/DISK-02 需要）、VM-D（SMB/NFS 客户端，P08/P14 需要）。
- 因此 P00.3 的验收条件已满足（数据盘序列号可识别 + 与宿主磁盘隔离），但 P04/P08/P14
  在对应 VM 补齐前不得开始，详见第 6、7 节。

---

## 1. 需要的环境

| 编号 | 用途 | 规格 |
|---|---|---|
| VM-A | Ubuntu **22.04 LTS** amd64 server，0.1 主线验收 | 2 vCPU / 4 GiB / 系统盘 40 GiB（GPT，单分区 ext4 根，不用 LVM）/ 数据盘 8 GiB + 4 GiB / virtio-scsi |
| VM-B | Ubuntu **24.04 LTS** amd64 server，双平台验收 | 同 VM-A |
| VM-C | LVM 根盘场景（DISK-02 保护判断） | 2 vCPU / 4 GiB / 系统盘 40 GiB（LVM 根）/ 数据盘 4 GiB |
| VM-D | 独立 SMB/NFS 测试客户端（P08/P14） | Ubuntu server 最小安装，不装面板 |
| 数据盘模板 | 可丢弃、可重建的虚拟数据盘 | `upanel-data-a.qcow2` 8 GiB、`upanel-data-b.qcow2` 4 GiB、`upanel-data-c.qcow2` 4 GiB（换盘测试） |

数据盘必须有**可识别的序列号**，并且序列号与用途一一对应，便于在 VM 内核对：

| 虚拟盘 | 序列号（必须显式配置） | 用途 |
|---|---|---|
| data-a | `UPANEL-DATA-A-8G` | ext4 全流程（GPT→分区→格式化→挂载→读写→重启） |
| data-b | `UPANEL-DATA-B-4G` | XFS 流程与 fsck pass=0 验证 |
| data-c | `UPANEL-DATA-C-4G` | 换盘/同名设备身份冲突（DISK-05），以及依赖阻断测试 |

`qemu-img` 与 libvirt 可以设置序列号；VirtualBox/VMware 也可以设置虚拟磁盘序列号。
无论使用哪种 hypervisor，都必须做到：**VM 内 `lsblk -o SERIAL` 显示的序列号等于上表**，
并且在宿主 `lsblk` 中看不到这些序列号（隔离证据）。

## 2. 隔离硬性要求

1. 数据盘只能是**独立的虚拟磁盘镜像文件**。禁止把宿主物理磁盘、U 盘、移动硬盘、
   宿主分区或宿主整盘透传/直通给 VM。
2. 禁止 USB passthrough、PCI passthrough、raw device mapping 到宿主磁盘。
3. 禁止把宿主目录（尤其 `H:\Code`、用户目录、`/mnt`、`/media`）以读写方式共享进 VM；
   共享目录只能用于只读文档传递，且不得与数据盘混淆为目标设备。
4. VM 的网络使用 host-only / NAT；不得把 VM 暴露到公网，也不得把它当成“日常服务器”。
5. 破坏性命令（`sfdisk`、`mkfs.*`、`wipefs`、`umount -f`、写 fstab 等）
   **只在 VM 内执行**，开发宿主一次也不执行。开发宿主上的 `lsblk` 只读观察允许。
6. 每个破坏性验收用例开始前重置：VM 系统盘回到快照，数据盘用模板重建
   （`qemu-img create -f qcow2 -b base.qcow2 ...` 或删除后重建），
   不复用“上一次已经被格式化过”的盘去测另一个用例。

## 3. VM 内环境核验（每次验收前执行并保存输出）

```sh
# 1) 系统身份
lsb_release -a; uname -a; systemd --version | head -n 1

# 2) 工具链（必须等于 docs/DEPENDENCIES.md）
go version            # 期望 go1.27.3
node -v               # 期望 v22.23.3
npm -v                # 期望 10.9.9

# 3) 磁盘清单与序列号（核心证据）
lsblk -d -o NAME,SIZE,TYPE,TRAN,VENDOR,MODEL,SERIAL,WWN
ls -l /dev/disk/by-id/
for d in /dev/sdb /dev/sdc; do
  udevadm info --query=property --name="$d" \
    | grep -E '^(ID_SERIAL|ID_SERIAL_SHORT|ID_WWN|ID_BUS|ID_MODEL)=' || true
done

# 4) 根文件系统与祖先链（确认数据盘不在系统盘上）
findmnt -no SOURCE,TARGET,FSTYPE /
lsblk -o NAME,SIZE,TYPE,FSTYPE,MOUNTPOINTS,PKNAME

# 5) 确认是虚拟盘
cat /sys/block/sdb/device/vendor 2>/dev/null || true

# 6) 确认没有宿主共享目录混入
findmnt -t 9p,virtiofs,cifs,nfs,nfs4 || echo "no host shares mounted"
```

通过条件：

- `sdb` / `sdc` 的 `SERIAL` 与第 1 节表格一致；
- 根文件系统的祖先链上不包含任何数据盘；
- 第 6 项没有输出（或只有明确记录的、与数据盘无关的只读共享）；
- 宿主侧 `lsblk -o NAME,SIZE,SERIAL` 中不出现 `UPANEL-DATA-*`。

**工具链状态**：VM-A/VM-B 目前只装了基础系统，**未安装 Go/Node 工具链**，
所以第 2 项（`go version` / `node -v` / `npm -v`）在 VM 内为 **NOT RUN**。
P01 起需要 VM 内构建/运行时可再补装并核验，届时必须与 `docs/DEPENDENCIES.md` 完全一致，
不得用宿主版本冒充。当前 P00 只依赖第 3—6 项（磁盘身份与隔离），这四项已实际执行并留证。

## 4. 快照、重建与回滚流程

当前环境**不使用 libvirt/qemu 内部快照**，采用更彻底的文件级重建（效果等价且更不容易残留状态）：

```sh
# 每个用例前：系统盘从 cloud image 后备链重建（丢弃上一轮所有写入）
cd H:/Code/vms/upanel-2204
rm -f system.qcow2
qemu-img create -f qcow2 -F qcow2 -b ../images/ubuntu-22.04-server-cloudimg-amd64.img system.qcow2 40G

# 数据盘重建为空白模板（容量必须与序列号一致）
rm -f data-a.qcow2 && qemu-img create -f qcow2 data-a.qcow2 8G
rm -f data-b.qcow2 && qemu-img create -f qcow2 data-b.qcow2 4G
rm -f data-c.qcow2 && qemu-img create -f qcow2 data-c.qcow2 4G

# 重新启动（序列号必须显式给出，不能依赖自动生成）
& H:/Code/vms/launch-vm.ps1 -Name upanel-2204 -SshPort 2223 -SeedPort 8900 -MemoryMB 4096
```

（若后续接入 libvirt/qemu 内部快照，等价命令为 `virsh snapshot-revert <vm> baseline`；
无论哪种方式，结论必须来自重建后的干净盘，不能复用已有分区表/文件系统的盘。）

不使用 `dd` 或 `wipefs` 把上一轮数据盘“擦干净”后继续用；必须重建，
避免残留分区表/文件系统指纹影响下一次身份判断的结论。

## 5. 证据采集约定

- 目录：`docs/evidence/<阶段>/<YYYY-MM-DD>/`，例如 `docs/evidence/P07/2026-10-25/`。
- 每个用例至少包含：
  - `env.txt`：第 3 节的核验输出；
  - `commands.log`：执行的命令、退出码、脱敏后的输出（用 `set -x` 或手动记录均可，但必须真实）；
  - `result.md`：预期、实际、结论（pass / fail / NOT RUN）、失败时的实际系统状态；
  - 需要时附截图，但截图只能证明界面，不能替代系统状态。
- 禁止提交：磁盘镜像、VM 内存转储、私钥、密码、Agent 密钥、真实用户数据。
- 证据命名与 PROGRESS.md 的验收编号（SEC-01、DISK-03 等）对应，便于索引。

## 6. 需要的外部条件

| 条件 | 为什么需要 | 现状 |
|---|---|---|
| 可用的 hypervisor | 创建 VM-A/VM-B/VM-C | **已解决**：Windows Hypervisor Platform + 用户级便携 QEMU 11.1.0（`-accel whpx`），无需管理员权限、无系统级安装 |
| Ubuntu 22.04/24.04 server 镜像下载与校验 | 安装系统 | **已完成**：使用官方 cloud image（`*-cloudimg-amd64.img`），下载后校验 SHA256SUMS 通过，原始镜像保留在 `H:\Code\vms\images` |
| 至少两块可丢弃虚拟数据盘的创建权限 | 破坏性验收 | **已完成**：每台 VM 三块 qcow2（8G/4G/4G），显式 `serial=` |
| 独立客户端 VM（VM-D） | SMB/NFS 真实客户端验收（P08/P14） | **未创建**（P08 前补齐） |
| LVM 根盘 VM（VM-C） | P04 DISK-02 系统盘保护判断 | **未创建**（P04 前补齐） |
| 支持 SMART 透传的配置，或一台可接受的只读验证主机 | P04.3 SMART/NVMe 解析验收 | 未确认；若 VM 不提供 SMART 数据，该项记 NOT RUN，不得用未知冒充健康 |
| 独立网络回滚验收环境（VM 内即可，不依赖宿主网络） | P14 NET-01 | 依赖 VM-A/VM-B，已具备载体 |

**VM-C/VM-D 补齐前，P04 的 LVM 根场景与 P08/P14 的真实客户端验收不得开始，也不得用 mock 结果顶替。**

## 7. 阶段与环境的对应关系

| 阶段 | 需要的环境 | 备注 |
|---|---|---|
| P00 | 本文件 + 版本证据 + VM-A/VM-B 隔离核验 | **已通过**（见 `docs/evidence/P00/2026-10-20/`） |
| P01—P03 | 开发宿主可完成代码与单元测试；VM 用于最终确认 | mock 结果不能代替 VM 结果 |
| P04 | VM-A/VM-B + 真实块设备（含 LVM 根场景 VM-C） | 只读采集，但需要真实设备多样性；**VM-C 未创建** |
| P05—P06 | VM-A | 服务/日志/文件/终端 |
| P07 | VM-A + data-a/data-b/data-c | 破坏性；每用例前回快照并重建数据盘 |
| P08 | VM-A + VM-D 客户端 | 真实 SMB 访问；**VM-D 未创建** |
| P10 | VM-A + 可解析的测试域名/DNS 或明确的 staging 说明 | 证书只用 ACME staging |
| P14 | VM-A/VM-B | 断网回滚必须在 VM 内验证，且 Web 与浏览器同时断开 |
| P16 | VM-A + VM-B | 全新安装、升级、卸载、恢复 |

## 8. 已创建环境记录（实际执行，2026-10-20）

本节记录真实使用的创建方式，供复现。所有 VM 文件位于**仓库之外**的 `H:\Code\vms`，
`.gitignore` 同时禁止任何磁盘镜像入库。

### 8.1 选型与授权边界

- 开发宿主为 Windows 家庭/普通用户环境，**无管理员权限**，不能安装 VirtualBox/VMware（需要内核驱动与管理员提权）。
- 检测到 Windows Hypervisor Platform 已启用：`(Get-CimInstance Win32_ComputerSystem).HypervisorPresent` = `True`，
  `C:\Windows\System32\WinHvPlatform.dll` 存在。
- 因此选用**便携 QEMU 11.1.0**（解压到 `H:\Code\.toolchains\qemu`，用户目录内，卸载即删除目录），
  加速后端 `-accel whpx`。整个过程未修改系统注册表、未安装服务、未要求提权。
- 磁盘 I/O 后端使用 virtio-scsi，每块盘通过 `serial=` 显式设置序列号（见 8.3）。
- 不使用 9p/virtiofs/SMB 共享，宿主与 VM 之间只经 SSH（回环端口转发）传递文本输出。

### 8.2 网络与 cloud-init（NoCloud over HTTP）

宿主 → VM 的装机配置使用 cloud-init NoCloud，但因为宿主无管理员权限、无法可靠生成 CIDATA ISO，
改为**通过 HTTP 提供 seed**：

1. 在宿主 `127.0.0.1:8899`（VM-B 用 `8900`）启动一个只读 Node HTTP 服务，仅白名单返回
   `user-data`、`meta-data`、`vendor-data`、`network-config`；服务脚本位于 `H:\Code\vms\...\serve-seed.js`（仓库之外）。
2. QEMU 参数 `-smbios type=1,serial=ds=nocloud;s=http://10.0.2.2:8899/`，cloud-init 在 user 网络下经
   `10.0.2.2`（QEMU 用户网络对宿主的固定映射）取到 seed。
3. user-data 内容（**不含任何密钥明文以外的敏感信息，私钥留在宿主**）：
   - `hostname: upanel-2204` / `upanel-2404`
   - `ssh_pwauth: false`
   - 用户 `upanel`，`sudo: ALL=(ALL) NOPASSWD:ALL`（**仅验收 VM 内部使用**，与产品权限边界无关：
     产品侧的 `upanel-web` 不得 sudo，见 PLAN）
   - 注入一次性生成的 ed25519 公钥，`lock_passwd: true`

### 8.3 QEMU 启动命令（实际参数）

参数化脚本：`H:\Code\vms\launch-vm.ps1`（仓库之外）。核心参数（以 VM-B 为例）：

```
qemu-system-x86_64.exe -machine pc -accel whpx -smp 2 -m 4096 \
  -drive file=...\upanel-2404\system.qcow2,if=none,id=drv_sys,format=qcow2 \
  -device virtio-scsi-pci,id=scsi0 \
  -device scsi-hd,drive=drv_sys,bus=scsi0.0,serial=UPANEL-SYS-40G \
  -drive file=...\upanel-2404\data-a.qcow2,if=none,id=drv_a,format=qcow2 \
  -device scsi-hd,drive=drv_a,bus=scsi0.0,serial=UPANEL-DATA-A-8G \
  -drive file=...\upanel-2404\data-b.qcow2,if=none,id=drv_b,format=qcow2 \
  -device scsi-hd,drive=drv_b,bus=scsi0.0,serial=UPANEL-DATA-B-4G \
  -drive file=...\upanel-2404\data-c.qcow2,if=none,id=drv_c,format=qcow2 \
  -device scsi-hd,drive=drv_c,bus=scsi0.0,serial=UPANEL-DATA-C-4G \
  -netdev user,id=net0,hostfwd=tcp:127.0.0.1:2222-:22 \
  -device virtio-net-pci,netdev=net0 \
  -smbios type=1,serial=ds=nocloud;s=http://10.0.2.2:8899/ \
  -display none -monitor none -serial file:...\console.log -pidfile ...\qemu.pid
```

| VM | Ubuntu | 宿主 SSH 转发 | seed 端口 | 系统盘序列号 |
|---|---|---|---|---|
| VM-B | 24.04.5 LTS | `127.0.0.1:2222` | 8899 | `UPANEL-SYS-40G` |
| VM-A | 22.04.5 LTS | `127.0.0.1:2223` | 8900 | `UPANEL-SYS-40G` |

注：系统盘序列号在首轮创建 VM-B 时漏配，已关机后按上表重新启动并复验（见 result.md 的修正记录）。

### 8.4 复现步骤

```powershell
# 1) 准备便携 QEMU（用户目录，无需管理员）
#    解压 qemu-w64-setup / qemu 归档到 H:\Code\.toolchains\qemu

# 2) 准备基础镜像（校验通过后复制为可写 qcow2）
#    H:\Code\vms\images\<release>-server-cloudimg-amd64.img

# 3) 生成 SSH 密钥（每 VM 一套，私钥留在宿主，公钥写入 cloud-init user-data）
ssh-keygen -t ed25519 -f H:\Code\vms\upanel-2404\id_ed25519 -N '""'

# 4) 启动 seed HTTP 服务（只读白名单）
node H:\Code\vms\upanel-2404\serve-seed.js 8899

# 5) 启动 VM
& H:\Code\vms\launch-vm.ps1 -Name upanel-2404 -SshPort 2222 -SeedPort 8899 -MemoryMB 4096

# 6) 核验（第 3 节命令），把输出保存到 docs/evidence/<阶段>/<日期>/
```

### 8.5 资源与限制

- 宿主：16 逻辑处理器 / 63.9 GiB 物理内存；两台 VM 各 4 GiB / 2 vCPU，可同时运行（实测 RSS 755 MB / 878 MB）。
- 同一时间只运行**当前验收所需**的 VM；破坏性用例前必须关机重建数据盘（第 4 节）。
- 便携 QEMU 与 VM 文件均不入库；换机或清理时直接删除 `H:\Code\vms` 与 `H:\Code\.toolchains\qemu` 即可。

### 8.6 VM-E（NVMe / SMART 验收机，2026-10-21 创建）

P04.3 需要一块**能读出真实 SMART 结论**的盘。两台既有 VM 都是纯 QEMU SCSI（`smart_support.available=false`、
没有 `smart_status`、温度字段是 0），只能证明“不支持”这条分支，证明不了 healthy。因此新建第四台 VM。

| 项 | 值 |
|---|---|
| 名称 | `upanel-smart` |
| 系统 | Ubuntu 24.04.5 LTS，内核 `6.8.0-147-generic` |
| 平台 | QEMU（便携版，与 VM-A/VM-B 同一套脚本） |
| 端口 | `127.0.0.1:2224` → guest 22 |
| 密钥 | `H:\Code\vms\upanel-smart\id_ed25519` |
| 启动 | `powershell -NoProfile -File H:\Code\vms\launch-vm-smart.ps1` |
| 磁盘布局 | `sda` 40 GiB（SCSI，系统盘）+ `nvme0n1` 4 GiB（QEMU NVMe 控制器）；加 `-WithUsbDisk` 启动时另有 `sdb` 2 GiB（`qemu-xhci` + `usb-storage`） |
| 账号 | `upanel`（免密 sudo），与 VM-A/VM-B 一致 |

实测结论（`tests/fixtures/smart/upanel-smart-nvme0n1.json` 是这台机器的真实输出）：

- NVMe：`smart_status.passed=true`、`critical_warning=0`、`temperature.current=50`、`smart_support.available=true`。
  这是本阶段唯一能真机证实“healthy + 真实温度”的来源。
- SCSI 系统盘（`sda`）：`smart_support.available=false`，没有 `smart_status`，温度 0 → 产品必须报 `unsupported`。
- **ATA/SATA 盘无法在这台机器上验证**：给 guest 挂 `ide-hd` 会让固件阶段卡死（连内核都起不来），
  所以 ATA 分支的解析只有合成输入单测（`internal/storage/smart_test.go` 里的 `synthetic()`）。
- 休眠盘同样造不出来：SCSI 忽略 `-s standby,now`，NVMe 的 `runtime_status` 报 `unsupported`，
  `sleeping` 与 `-n standby` 只有单测覆盖，验收脚本内标 NOT RUN。
- 与 VM-B 一样安装 `smartmontools 7.4`；`nfs-kernel-server` 只在 VM-B 上装（P04.3 的不可达挂载实验在那儿做）。
- USB 盘（`powershell -File launch-vm-smart.ps1 -WithUsbDisk`）：`lsblk` 里 `TRAN=usb`，但 QEMU 的 USB 桥
  （`0x46f4:0x0001`）不在 smartctl 的桥表里——真实输出是 `Unknown USB bridge`（退出状态 1）；
  显式 `-d sat` 是 `Read Device Identity failed: scsi error unsupported scsi opcode`（退出状态 2）。
  两份原样输出都在 `tests/fixtures/smart/`，用来钉住“桥不认识 → `unknown`，不是 unsupported、更不是 healthy”。
  换句话说，这台 VM 提供的 USB 证据是**真实的反例**，不是一块能读出 SMART 的 USB 盘。
