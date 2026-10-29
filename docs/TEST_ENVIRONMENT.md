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

### 8.7 前端验收（P04.4）所需的额外步骤

前端验收分两头：**数据在 VM 上验，像素在宿主上验**。

VM 侧（`tests/integration/p04-4-ui-acceptance.sh`，root，只需 Python 3 与 util-linux）：

```sh
# 宿主：交叉编译（不入库），拷进 VM
mkdir -p /tmp/upanel-bin
for b in upanel-web upanel-agent upanelctl; do
  GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -o /tmp/upanel-bin/$b ./cmd/$b
done
scp -i H:\Code\vms\upanel-2404\id_ed25519 -P 2222 /tmp/upanel-bin/* upanel@127.0.0.1:/tmp/upanel-bin/

# VM：跑验收（脚本自己建 upanel-web 账号、写 /etc/upanel/upanel.json、起代理与 Web）
sudo BIN_DIR=/tmp/upanel-bin sh tests/integration/p04-4-ui-acceptance.sh
```

两个坑，脚本已经处理，改动时别退回去：

1. **不能启动完 Web 就抓响应**。健康采集是独立 goroutine，比拓扑慢，早抓会拿到
   `SMART 采集尚未完成` 的占位。脚本先轮询整盘的 `health.checked_at`（`HEALTH_WAIT`，默认 120 秒，
   实测 5 秒），超时就记 `NOT RUN` 并写出是哪几块盘。
2. **健康对照必须用产品同一组参数**（`-j -i -H -A -n standby`）。只用 `smartctl -H` 时，
   QEMU 的虚拟盘会返回 `smart_status.passed=true`；带上 `-i -A` 后同一块盘是
   `smart_support.available=false`。两边看不同的输出，等于在测两件事。

还有一条与具体阶段无关、但每次都咬人的：**从 Windows 宿主 `scp` 上去的二进制没有可执行位**。
宿主是 Windows，`bin/linux-amd64/*` 上没有 Unix 权限位，`scp` 落到 VM 上就是 `0644`，
症状是 `Permission denied`（脚本里表现为“二进制存在且可执行”三条一起失败）。推完补一条：

```sh
ssh -i H:\Code\vms\upanel-2404\id_ed25519 -p 2222 upanel@127.0.0.1 'chmod 0755 /tmp/upanel-bin/*'
```

同理，**`.sh` 与 `.sql` 必须是 LF**。Windows 工作树里写出来的脚本会带 CRLF，推到 Linux 上是
`not found` / `set: Illegal option -`（第一行 `#!/bin/sh` 后面多了一个回车，解释器名就成了
`/bin/sh\r`），而带 CRLF 的迁移文件会让已建好的库校验和失配。
`scripts/check.sh` 的 `line-endings` 一关专拦这个（`*.sh` / `*.sql` / `*.service`）。

载荷会留在 `/var/tmp/upanel-accept-p04-4/payloads/`，拷回宿主后既进 `docs/evidence/`，
也是 `web/src/fixtures/p04-4-vm/` 的来源（逐字节相同）。

宿主侧（页面冒烟，需要 Playwright chromium）：`sh tests/smoke/p04-4-ui-smoke.sh`。
它跑的是真实 `upanel-web --mode mock`（端口固定 127.0.0.1:18881），因此与 `p02-1-*` 冒烟不能同时跑。

VM 上**不装浏览器**：VM 只证明“前端拿到的数据是真的”，像素证据在宿主。

### 8.8 服务管理验收（P05.1）所需的额外步骤

服务管理动的是真实 systemd，所以这一步必须在 VM-A（`upanel-2404`）里做，**不能**在开发宿主上做。
做法与 8.7 同一条流水线，脚本换成 `tests/integration/p05-1-services-acceptance.sh`。

```sh
# 宿主：交叉编译（静态，不依赖 VM 里的 Go）
mkdir -p /tmp/upanel-bin
for b in upanel-web upanel-agent upanelctl; do
  GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -o /tmp/upanel-bin/$b ./cmd/$b
done
scp -i H:\Code\vms\upanel-2404\id_ed25519 -P 2222 /tmp/upanel-bin/* upanel@127.0.0.1:/tmp/upanel-bin/

# VM：root 运行（脚本建 upanel-web 账号、写配置、起代理与 Web，端口 19449）
sudo BIN_DIR=/tmp/upanel-bin sh tests/integration/p05-1-services-acceptance.sh
```

脚本自己建两个一次性单元，验收完删掉；改动时注意它们的作用：

| 单元 | 作用 |
|---|---|
| `upanel-accept.service` | 一个正常的 `Type=simple` 单元（`sleep` 循环）。启停/重启/开机启动的全部正例都打在它身上，每一步都读回 `ActiveState`/`UnitFileState` 并与 `systemctl` 现场对照。 |
| `upanel-accept-fail.service` | `ExecStart` 指向一个不存在的命令。用来证明 **`systemctl` 返回 0 不算成功**：单元进入 `failed`，任务必须是 `failed` 且 `result` 为空对象 `{}`，失败原因只在 `error` 里。 |

这一台 VM 的实测环境（证据见 `docs/evidence/P05/2026-10-21-p05.1/env.txt`）：

- Ubuntu 24.04.5 LTS，内核 `6.8.0-147-generic`，**systemd 255（255.4-1ubuntu8.18）**，2 vCPU。
- 枚举到 258 个规范 service 单元，其中 26 个模板、19 个别名，列表里出现 239 个。
- 86 个单元命中保护名单——数量大是因为 `systemd-*` 前缀整体计入核心组件（CONTRACTS §10.1）。

三个只能在真机上发现的坑，改动解析时别退回去：

1. **`systemctl show` 遇到裸模板名会整条中断**。`systemctl show -- apport-coredump-hook@.service` 的
   真实输出是 `Unit name ... is neither a valid invocation ID nor unit name.`（退出 1），
   而且**后面的名字一起丢掉**。所以枚举阶段就要剔掉模板，并且拿到的单元数必须与 `list-unit-files`
   的规范名集合逐项核对——只判断“解析结果非空”，会让 282 个单元的机器上只显示 7 行。
2. **别名单元不是独立单元**。`UnitFileState=alias` 的条目 `Id=` 是规范名，请求的别名只出现在 `Names=` 里；
   按 `Id` 匹配会把它当成“读不到”，整次采集失败。枚举时剔掉别名，解析时按 `Id` **和** `Names` 建索引。
3. **保护名单会挡住 SSH 断连风险**。`ssh.service`/`sshd.service`/`NetworkManager.service` 都在名单里，
   所以“通过面板把 SSH 停掉”这件事在验收里只能验证**被拒绝**，验证不了后果。

明确不便在 VM 上覆盖的（脚本与 `docs/evidence/` 里都标 NOT RUN，不假装通过）：

- **浏览器像素**：VM 上没有浏览器。页面级证据在宿主上用同一批真实载荷回放——
  `web/src/components/realdata-services.test.ts` 读 `web/src/fixtures/p05-1-vm/`（与本次运行的抓包逐字节相同）。
- **执行后未收敛**（命令返回 0 但 15 秒内状态没变）：正常 systemd 上造不出来，
  只有 `internal/services/collector_test.go` 的 fake 覆盖。
- **面板自身作为 systemd 单元**：验收里 Web 与代理是直接启动的进程，不是 unit；
  这两类保护判定由单元测试覆盖。

载荷留在 `/var/tmp/upanel-accept-p05-1/payloads/`（65 个文件），拷回宿主后既进
`docs/evidence/P05/2026-10-21-p05.1/`，也是 `web/src/fixtures/p05-1-vm/` 的来源。

一个环境侧的坑：这台 VM 出现过一次 PID 1（systemd）SIGSEGV（`panic-6`，随后由内核重启），
发生在一次验收运行期间。复现不了，也无法排除是 QEMU/内核侧的问题；记录在案，
若再出现，先看 `docs/evidence/` 里的对应日志再决定是否重建 VM。

### 8.9 加速后端：`-accel whpx` 会随机 panic，`-accel tcg,thread=single` 才起得来（2026-10-24）

两台 VM 都建在 `-accel whpx` 上，但**这台宿主的 whpx 对 Ubuntu 24.04 不稳定**：客户机里
systemd 会在磁盘操作过程中 SIGSEGV，多次重试后变成
`Kernel panic - not syncing: Attempted to kill init! exitcode=0x0000008b`，内核直接重启。
2026-10-21 一天内 12 次，2026-10-24 又攒到第 10 次（`H:\Code\vms\upanel-2404\console.panic-*.log`，
其中 `console.panic-10-2026-10-24.log` 是最近一次）。panic 发生在验收运行中间时，
那一轮的结论只能作废重跑——它既不是产品缺陷也不是被测机器的状态。

换 TCG 时踩到第二个坑：**多线程 TCG（`-accel tcg,thread=multi`，即默认）起不来**，
客户机内核在时钟初始化阶段就死：

```
[    0.596417] ..MP-BIOS bug: 8254 timer not connected to IO-APIC
[    0.609204] ....... failed.
[    0.620000] ...trying to set up timer as Virtual Wire IRQ... failed.
[    0.775682] ..... failed :(.
[    0.776191] Kernel panic - not syncing: IO-APIC + timer doesn't work!
```

试过的两条都没用：`-smp 1`（消除 SMP 因素）、`-cpu max`（补 TSC-deadline）。**有用的是
`-accel tcg,thread=single`**——单线程 TCG 下中断投递不走 MTTCG 的路径，同一个镜像、
同一块盘，直接过 `check_timer` 进 systemd 正常引导（`console.tcg-multicore-apic-2026-10-24.log`、
`console.tcg-singlecore-apic-2026-10-24.log`、`console.tcg-cpumax-apic-2026-10-24.log` 是三次失败尝试的原文）。

```powershell
# 24.04 客户机用这条起（慢，但能跑完一整轮验收）
& H:/Code/vms/launch-vm.ps1 -Name upanel-2404 -SshPort 2222 -SeedPort 8899 -MemoryMB 4096 `
    -Cpus 2 -Accel 'tcg,thread=single'
```

`launch-vm.ps1` 的 `-Accel` 现在接受任何 `tcg*` 字串（原样传给 QEMU），`-Cpu` / `-Machine`
也可覆盖。**代价与纪律**：单线程 TCG 下单条命令慢数倍，拨号、超时类的断言会被宿主侧的
负载放大——跑验收时不要在宿主上并发 `go test`、不要另一台 VM 同时跑验收、不要一边
ssh 进去查东西（这一条我自己违反过两次，两次都制造了假失败，见 DECISIONS Q329）。
每份 `env.txt` 都要写清这一轮用的是哪个加速后端。

### 8.10 整机 fd 总数是验收环境的前置条件（`systemd-logind` 泄漏过 7477 个 fd，2026-10-26）

代理每 5 秒扫一次存储拓扑，"设备是否被占用"那一步要把**每个进程的每个 fd** 取一遍设备号
（`internal/storage/busy.go` 的 `ProcBusyScanner.walkProc`）。所以"整机开着多少 fd"不是
卫生指标，而是这条路径的输入规模。

2026-10-26 的 P13.2 首轮撞到过：24.04 验收机上整机 7976 个 fd，其中 `systemd-logind`
独占 **7477**（上一批验收反复进出 session 攒下的泄漏）。后果是一串看着像产品坏了的症状：

```
storage.topology            24.85 s / 41.08 s      ← 重启 logind 之后 0.027 / 0.023 / 0.307 s
system.summary               0.068 s
lsblk                        0.485 s
```

拓扑查询超时 → Web 侧记"读取存储拓扑失败：代理不可用" → 容量序列一个点也落不下来 →
`status.series` 里连 `filesystem` 这个 scope 都没有 → 所有与文件系统有关的断言连锁失败
（那一轮 117 PASS / 25 FAIL）。`agent.log` 里同时有 47 条"代理请求耗时偏长"，代理也就吃满
了一个核——上一轮记下的"agent 103% CPU"之谜同源。

处置：`sudo systemctl restart systemd-logind`（或重启这台 VM），之后 logind 回到 22 个 fd。
验收脚本 §0 已经自带体检，超过 4000 就打一行说明并 `exit 2`，不在这种机器上产出假红：

```sh
python3 -c '
import os
total=0; top=""; top_n=0
for pid in os.listdir("/proc"):
    if not pid.isdigit(): continue
    try: n=len(os.listdir("/proc/"+pid+"/fd"))
    except OSError: continue
    total+=n
    if n>top_n:
        try: top, top_n = open("/proc/"+pid+"/comm").read().strip(), n
        except OSError: pass
print(total, top, top_n)'
```

空载的 2 vCPU 客户机是 **30—50**（两台实测 42 / 36）。几百是正常，四位数就要查是谁。

### 8.11 NFS 验收的两条环境约束（P14.4，2026-10-27）

P14.4 在 **VM-B（Ubuntu 22.04.5、内核 `5.15.0-198-generic`、端口 2223）** 上跑，机器上原有
`nfs-kernel-server` 与 `nfs-common` 各 `1:2.6.1-1ubuntu1.2`（8.6 那条"只在 VM-B 上装"指的就是
这台）。两条约束在写验收脚本时必须知道，否则会造出看着像产品故障的假红：

**一、被导出、且客户端连过的文件系统，`umount` 会一直 `target is busy`。**
本机自己卸客户端挂载点时，内核 `knfsd` 仍持有那个文件系统——`fuser -vm <挂载点>` 直接点名
`kernel knfsd`。所以脚本卸挂载点必须**硬卸优先**（`umount` → `umount -f` → `umount -l`），
只写 `umount -l` 会留下"摘了命名空间但内核还拿着"的状态，后面所有"卷没在用了"的断言全红。
裸机复现（无面板参与）：挂上 → `umount -l` → 本机 `umount` 仍 rc=32。

**二、`-monitor none` 起的机器拔不了盘。**
8.3 的启动命令就是 `-monitor none`，所以"设备级掉线"（拔 sdb、I/O 错误之后 `statfs` 报错）
在现有 VM 上做不到。P14.4 验的是"挂载点从命名空间里消失之后面板做什么"（lazy umount），
设备级掉线在验收脚本里**登记为 NOT RUN**，不用 lazy umount 冒充。

另外两条只影响断言写法、不影响环境：NFSv3 客户端的 `findmnt` OPTIONS **永远报 `rw`**
（只读是服务端回 `EROFS`，不在挂载选项里）；`root_squash` 生效后客户端 root 落盘属
`nobody:nogroup`，服务端放进去的 `0600` 文件客户端读不到（夹具要自己 `chmod 0644`）。

### 8.12 账号验收的三条环境约束（P14.5，2026-10-27）

P14.5 在 **VM-A（Ubuntu 22.04.5、内核 `5.15.0-198-generic`、宿主转发 `127.0.0.1:2223`）** 上跑。
首轮在 §0 就红了一条基线，查下去是环境而非产品，记在这里免得下一轮再花时间去查：

**一、`/etc/sudoers.d/` 里任何文件的权限不是 `0440`，`visudo -c` 就是 rc=1。**
这台 VM 上有一份手写的 `/etc/sudoers.d/upanel`（内容 `upanel ALL=(ALL) NOPASSWD: ALL`，
`0644`）。`visudo -c` 会为它报一行
`/etc/sudoers.d/upanel: bad permissions, should be mode 0440` 并返回 1，于是验收脚本第一条
基线断言 `起点：整份 sudo 配置通过 visudo -c` 直接红，后面凡是依赖"整份配置能校验"的断言
（包括 P14.5 的双门禁）也跟着不可信。处置：删掉这份手写文件即可——cloud-init 生成的
`/etc/sudoers.d/90-cloud-init-users` 已经给了 `upanel` 用户 `NOPASSWD:ALL`，删掉之后
`visudo -c` rc=0、`sudo -n true` 仍然通。原始文件留档在 `/var/tmp/p145-envfix/upanel.badmode`。

**二、这台 VM 没装 Samba（`nfs-kernel-server` 装了）。**
账号验收不需要 Samba：普通账号要的是 `useradd`/`usermod`/`userdel`/`getent`/`passwd`/
`chpasswd`、两档壳与 `visudo`。所以验收脚本第 2 节断言 `GET /accounts` 的
`capability.status == available` 是对的判据——它问的是账号的能力，不是 Samba 的。
（首轮这条红是**产品缺陷**：接口当时把 `share.accounts` 带的 Samba 能力返回给了账号页。
修法与回归见 DECISIONS Q478；**不要**用"给 VM 装 Samba"来绕开它。）

**三、第 11 节的公钥登录两面要求 sshd 在跑。**
门是 `systemctl is-active ssh || systemctl is-active sshd`，不活跃就把那两条记成 NOT RUN
而不是红。VM-A 上 `ssh.service` 是 active（验收本身就是从宿主 SSH 进去跑的），所以这一节
会真跑：同一把钥匙对"有口令的账号"能登、对"锁定/没口令的账号"不能登，然后再给账号设上
口令、同一把钥匙又能登——最后这半步是归因，它把"被拒"钉在口令状态上而不是钥匙或
sshd 配置上。

### 8.13 重启类相位的三条约束（P16.2，2026-10-27）

P16.2 的相位之间夹着一次**真重启**（`boot` 相位的前提就是"机器刚重启过"）。这一轮有三处
踩空，都发生在验收脚本之外（宿主侧驱动），记在这里因为下一平台还要再走一遍：

**一、`systemctl reboot` 返回的时候 sshd 还活着，立刻探活必然成功。**
驱动若"发完重启就循环探活、第一次成功就认为重启完成"，会把**关机前的那一瞬间**当成
"机器回来了"：它报 `VM 回来了（约 0s 后 ssh 可达）`，然后接着跑 `boot`，而客户机正在关机
——那一相位与后面四个相位的 ssh 全部 `banner exchange: Connection timed out`，
`RESULT-*` 一律 0 字节，看起来像"五个相位都挂了"。正确写法是**先等掉线、再等回来**：

```sh
$SSH 'sudo systemctl reboot' >/dev/null 2>&1
while ...; do $SSH 'echo up' >/dev/null 2>&1 || break; sleep 5; done   # 等下去
while ...; do $SSH 'echo up' >/dev/null 2>&1 && break; sleep 5; done   # 等回来
```

**二、tcg 下 22.04 从关机到 sshd 可用实测约 4 分钟，whpx 也未必更快。**
这一轮的实际读数：重启发出约 22:52，客户机 `uptime -s` 记 14:54:33（UTC），宿主到 14:56
前后才稳定连上。所以"等回来"的上限要给到 15 分钟以上，5 分钟不够；反过来，**看到
`banner exchange` 超时不要判客户机卡死**——上一轮我在这一点上误杀过 QEMU（以为 whpx 下
卡住，实际它自己继续跑完了）。判是否真死看 `Get-Process qemu-system-x86_64` 的 CPU 时间
是否推进，而不是看 ssh 有没有回应。

**三、同一条验收序列任何时刻只能有一个驱动实例；`TaskStop` 只杀管道末端。**
上一轮我把驱动写成 `... | tail -40` 再 `TaskStop`，被杀的只是 `tail`，主脚本仍在 VM 里
`rm -rf /root/upanel-pkg`；我随即起了第二个驱动，两边撞车，日志里出现
`sha256sum: bin/upanelctl: No such file or directory`、`重装退出码 0: 期望 '0'，实际 '2'`、
`Connection refused`，整份分相位计数作废。停任务要确认进程真的没了（`Get-Process` /
`ps`），并且**换证据目录**、不要把上一轮的 `facts/` 当这一轮的基线。

**四、seed server 没起，客户机会带着 `DataSourceNone` 白跑一整轮开机。**
`launch-vm.ps1` 只把 `ds=nocloud;s=http://10.0.2.2:<SeedPort>/` 塞进 SMBIOS，**不会**替你起那个
HTTP 服务。2026-10-27 起 24.04 时漏了这一步：cloud-init 试到超时后落到 `DataSourceNone`
（console 里 `Used fallback datasource`、`no authorized SSH keys fingerprints found for user ubuntu`），
没有 `upanel` 用户、没有公钥，SSH 一直是 `Permission denied (publickey)`；从开机到 cloud-init
收尾花了 **987 秒**（tcg 下的重试等待），看起来像"机器起不来"。
起之前先确认 seed 在响应：

```sh
node H:/Code/vms/upanel-2404/serve-seed.js H:/Code/vms/upanel-2404/seed 8899 &
curl -sS http://127.0.0.1:8899/meta-data | head -1     # instance-id: upanel-2404-0001
curl -sS http://127.0.0.1:8899/user-data | head -1     # #cloud-config
```

漏了的那一轮要把叠层盘重建（cloud-init 已经以 fallback datasource 收过尾，重启不会补种），
并把那一轮的 `console.log` 改名留档——`console.noDatasource-run.log` 就是这么来的。



### 8.14 断言不要单次采样 `systemctl is-active`（P16.2，2026-10-28）

`failup` 相位要证明"坏包让代理起不来"。这条断言原来只读一次
`systemctl is-active upanel-agent.service`，判它不等于 `active`——24.04 上就这样假红过
一次（Q515），而安装器其实判得没错。原因是采样撞上了 systemd 的记账间隙：

* 单元是 `Type=simple`：`Started` 一打就进 `active`；
* 但"主进程 exit 1"要 **0.234~0.400 秒**之后才被记账（11 轮 journal 微秒实测）；
* `Restart=on-failure` + `RestartSec=3s` 让单元每约 3.4 秒重来一轮
  → **`active` 占空比约一成**，单次采样有约 10% 的概率正好落在里面。

写法改成两件都不吃这个竞态的事：① 断言 socket 不存在（`/run/upanel/agent.sock`——"面板用不了
代理"的直接判据，起不来的进程造不出它）；② 隔 1.2 秒采两次，只有**连续两次**都为 `active`
才判失败（跨过一轮重启）。同理，任何"等它起来/等它停"的循环都别只看 `is-active`：
`install.sh` 第 13 步要的是 `socket 存在 && is-active` 同时成立——socket 是稳定判据，
单靠 `is-active` 的瞬时真会放过一个立刻退出的二进制。

读数与算账在 `docs/evidence/P16/2026-10-28-p16.2-2404-final/`（`journal-restart-cycle.txt`、
`active-window-analysis.txt`、`run1-pre-tighten/`）。

### 8.15 `-race` 只能在 VM 里跑，而且得自己装 C 编译器与官方 Go（2026-10-28）

想跑 `go test -race` 的第一步不是跑，是**确认这台机器跑得动**：

* **宿主（Windows）跑不了**：`go env CGO_ENABLED` 是 `0`，`command -v gcc` 什么都没有，
  直接报 `-race requires cgo`。就算在 Windows 上装个 mingw 让它编得过，编出来的也只是
  `windows` 构建标签下的代码——要查竞态的那些 `*_linux.go` 根本不会进二进制。**-race 的读数
  必须在 linux 上取。**
* **`apt install golang-go` 也不行**：24.04 仓库里是 1.22，而 `go.mod` 写的是 `go 1.27.3`，
  老 Go 连模块都读不进去。得下 go.dev 的官方 tarball 解到 `/usr/local/go`。
* **cgo 要 C 编译器**：`apt install gcc`（24.04 上是 13.3.0），然后确认
  `go env CGO_ENABLED` = `1`、`CC` = `gcc`。别只看 `go test -race` 能不能编——先看 `go env`。
* **模块图比想象大**：`go mod download all` 在这台 VM 上拉了约 **640 MB**（含 `mongodb` 驱动、
  `quic-go`、`grpc`、`modernc.org/sqlite` 这些间接依赖），纯下载花了约两分钟。
  想省这一步就跳过它直接 `go test -race ./...`，Go 会按需拉。
* **`-count=1` 必须带**：不带的话命中了测试缓存，"这一轮到底跑没跑"说不清。

还有一个后果要知道：`-race` 把整段代码都插桩，**Argon2id 单次校验会被放大数倍**，
`TestDefaultPasswordParamsTiming` 那条 1 秒上界在插桩下必然红——这是工具的性质，不是参数变重了。
P04.3 在 KVM 上是 1.79—2.03 秒；P16 这一轮在 2 vCPU 的 TCG 机上是 **1 分 31 秒**
（`单次 Argon2id 校验耗时 1m30.976442106s`，用例自己打印的），同机不带 `-race` 的对照读数
见 `docs/evidence/P16/2026-10-28-race/`。

**这一轮（P16 全量）补上的四条，都是踩过才知道的：**

* **必须用 root 跑。** 用普通用户跑，`internal/agentstore` 一打开代理数据库就做属主校验
  （数据目录必须归 root），于是 `agentstore` / `agent` / `cmd/upanel-agent` / `cmd/upanelctl` /
  `internal/api` 里一片红——第一次跑抓到 **307 条** `代理数据目录不可用: … 属主必须是 root，实际为 1000`。
  那不是竞态，是产品自己的安全校验在正常工作。**读数必须在 root 下取**（P04.3 / P04.3-H2 也是 root）。
* **源码不能靠 `git ls-files | tar` 打包。** 工作树里有大量未跟踪的 `.go`（这一轮是 224 个），
  按 git 清单打会漏掉它们；更隐蔽的是漏掉 `tests/fixtures/`——`internal/agent` 的存储夹具用例
  会报"定位夹具目录 storage-2404 失败"。要按显式目录清单打：
  `tar czf … go.mod go.sum api cmd internal migrations deploy scripts tests`。
* **`-timeout` 必须放大，而且放到 90 分钟也可能不够。** `go test` 每包默认 10 分钟，
  P04.3 在 KVM 上 `internal/api` 的 `-race` 已用 273 秒；这台 2 vCPU 的 TCG 机上，
  `internal/agent` / `internal/api` / `internal/auth` 三个包**跑了 90 分钟都没跑完**
  （超时那一刻分别在 `TestFileActionsRejectUnknownFields`、`TestAppLogsSystemdRejectsContainerParams`、
  `TestLoginIssueAndAuthenticate`，都在正常推进，不是死锁）。补跑要 `-timeout=0`，并且让它们
  各占一条 vCPU。
* **墙钟类断言在插桩下会零星变红，别急着归因产品。** 本轮两条：`internal/storage` 的
  "隔离期内还等了 9.2 毫秒"（上界 5 毫秒，功能三条全过）、`internal/backup` 的
  "600 毫秒握手预算"（重试第二轮被插桩拖过预算）。判据实验放在证据目录里，
  不要为了让它们变绿去放宽上界——那是把验收标准改小。

**补跑那一轮（2026-10-28T14:44Z 起，9 小时 03 分）又补上四条，都是先踩了才写的：**

* **跑之前先把装机留下的 `upanel-web` / `upanel-agent` 停掉。** 这台 VM 的系统盘是上一轮
  P16.2 页面链路验收装机留下的，两个服务常驻；实测空闲状态下 `upanel-web` 在 6 小时 40 分里
  吃了 **9002 秒 CPU（平均 37.5% 个核）**，两次 10 秒定点采样是 46% 与 26%，`journalctl` 60 秒
  零行（不是刷日志）。2 条 TCG vCPU 被一个没人用的服务吃掉四分之一到一个半核，贴边界的墙钟
  读数就全是噪声。`systemctl is-active upanel-web upanel-agent` 必须先看到两个 `inactive`。
  读数在 `docs/evidence/P16/2026-10-28-race/idle-panel-cpu.txt`。
* **别把三个重包并行扔进 2 条 vCPU，如果其中有握手超时或墙钟上界这类断言。** 并行本身不会
  改变 pass/fail（少一份负载只会更容易过），但它会把"本机 TLS 握手"这种本该毫秒级的事拖过
  5 秒预算：本轮 `TestWebhookSelfSignedCertIsRejected` 在**本机** `httptest.NewTLSServer` 上
  拿到 `TLS handshake timeout`，隔离重跑 3/3 全过（0.13 s）。**要并行就挑不夹墙钟断言的包。**
* **`TestDefaultPasswordParamsTiming` 的红不全是 `-race` 的锅。** 原来记的是"检测器把 Argon2id
  放大到 1.79—2.03 秒"，补跑里跑了**不带 `-race`** 的对照：这台 TCG 24.04 是 **3.061838022 秒**，
  一样撞 1 秒上界。KVM 22.04 不带检测器是 0.6 秒。**判据量的是机器速度**——模拟机上这条
  永远过不了，别为了它改上界。
* **给 `internal/api` 留够时间。** 同一个包在 KVM 上是 273 秒，在这台 2 vCPU 的 TCG 机上
  `-race -timeout=0` 用了 **28929 秒（8 小时 02 分）**。三条大包并行那一趟已经 9 小时，
  排期时按"整个 `-race` 全量是一夜"来算，不要按分钟。

### 8.16 宿主 `scripts/check.sh` 的 node 固定在仓库外面（2026-10-29）

`scripts/check.sh` 的 `node-version` 项是**等值比较**：`"v$EXPECTED_NODE"` 对 `node -v`，`EXPECTED_NODE`
读 `.nvmrc`（`22.23.3`）。这台宿主 PATH 首位是系统装的 `C:\Program Files\nodejs\node.exe`（v24.15.0），
所以直接跑就是 `node-version:FAIL`，连带 `openapi-lint` 一起红。

* **固定版工具链不在仓库里，在仓库的上一级。** `git clone` 下来**不会自带**它。这台机器上的位置是
  `/h/Code/.toolchains/node-v22.23.3-win-x64`（= `C:\…\Code\.toolchains\…`，仓库 `uPanel` 的**兄弟目录**，
  同目录还有 `qemu/`）；另有一份拷贝在 `C:\Users\huxi\tools\node-v22.23.3-win-x64`。用哪份都行，前置到
  PATH 首位即可：`export PATH="/h/Code/.toolchains/node-v22.23.3-win-x64:$PATH"`。
  **注意别在仓库根下找 `.toolchains/`**——它不在那儿，`ls -d .toolchains/*` 只会安静地什么都不返回，
  `.gitignore` 里也没有这一条（所以 `git status` 不会提示你它缺了）。
* **`openapi-lint` 在 node 24 上的红是运行时退出崩，不是 spec 不合格。** 那一跑 redocly 已经打印
  `Woohoo! Your API description is valid.`（6 个 warning 全是既有的），随后 node 在退出路径上撞
  `Assertion failed: !(handle->flags & UV_HANDLE_CLOSING), file src\win\async.c, line 76`，exit 127。
  **不要去改 spec、也不要把这一项从 check.sh 里摘掉**——换成 pinned node 22 之后同一条命令干净退出。
* 遇到这两个红先怀疑 PATH，别怀疑代码。判据本身没放宽：比较是等值的，只是喂给它的 node 版本不对。
  三份读数都在 `docs/evidence/P16/2026-10-28-race/facts-host-check.txt`（13/2 一份，15/0 两份：`~/tools` 与 `/h/Code/.toolchains` 各一次）。

