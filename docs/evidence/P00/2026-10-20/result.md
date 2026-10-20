# P00 执行结果

- 日期：2026-10-20
- 证据类型：**开发宿主本机检查**（非 Linux VM 验收、非浏览器 E2E）
- 结论（2026-10-20 更新）：P00.1 完成；P00.2 完成（前端构建兼容性未验证）；
  P00.3 文档、约定与**验收 VM 创建/核验全部完成**。
  **P00 整体状态：passed。** 本文件前半部分记录首次执行（当时 VM 为 NOT RUN，保留为历史），
  第 4 节为解除阻塞后的补充记录。

## 1. 实际执行的命令与结果

| 命令 | 退出码 | 结果 |
|---|---|---|
| `GOTOOLCHAIN=go1.27.3 go version` | 0 | `go version go1.27.3 windows/amd64`（自动下载并生效） |
| `curl https://go.dev/dl/?mode=json` | 0 | 稳定版 `go1.27.3`、`go1.26.10` |
| `curl https://proxy.golang.org/<module>/@latest`（7 个模块） | 0 | 版本见 `dependency-resolution.txt` |
| `curl https://nodejs.org/dist/index.json` | 0 | Node 22 最新补丁 `v22.23.3`（npm 10.9.9） |
| `curl https://registry.npmjs.org/<pkg>`（19 个包） | 0 | 版本见 `dependency-resolution.txt` |
| `npm install --package-lock-only --ignore-scripts`（Node 22.23.3 / npm 10.9.9） | 0 | 生成 `web/package-lock.json`，lockfileVersion 3，267 条目 |
| `npm ci --ignore-scripts` | 0 | added 241 packages |
| `npm ls --depth=0` | 0 | 19 个声明依赖全部解析，无 missing/extraneous |
| `redocly lint --format=summary api/openapi.yaml` | 0 | 契约有效；20 条 `no-unused-components` 警告（骨架期 paths 为空，预期） |
| `sh scripts/check.sh`（PATH 含 Node 22.23.3） | 0 | PASS 2 / FAIL 0 / NOT RUN 8，见 `check-run.txt` |
| `sh scripts/check.sh`（宿主 Node v24.15.0） | 1 | `node-version` FAIL，证明版本闸门有效，见 `check-run-host-node.txt` |

## 2. NOT RUN（本轮未执行，不得视为通过）

| 项目 | 原因 | 补做方式 |
|---|---|---|
| Go `build`/`vet`/`test`/`gofmt` | 仓库尚无 Go 源码（P01.1 才建立入口） | P01.1 实现后由 `scripts/check.sh` 自动启用 |
| 前端 `typecheck`/`lint`/`test`/`build` | 尚无应用代码与对应 npm scripts（P01.3） | P01.3 建立后由同一脚本执行 |
| Ubuntu 22.04/24.04 上的构建与运行 | 首次执行时无 VM（现已具备 VM，但 VM 内未装 Go/Node） | P01 起如需 VM 内构建，先补装并核验到与 DEPENDENCIES.md 一致 |
| ~~虚拟数据盘序列号与宿主隔离核验~~ | **已在第 4 节补做并通过** | 见 `vm-2404-verification.txt`、`vm-2204-verification.txt`、`host-isolation.txt` |
| 依赖大版本运行时兼容性 | 无应用可构建；仅核实 registry 元数据与 peer 范围 | P01.3 的真实构建 |
| Playwright 浏览器 | 未安装（`--ignore-scripts`） | P16 前显式 `npx playwright install` |
| CI 工作流 | 仓库未初始化 Git、无远端 | 建立远端后首次运行 |
| 任何真实系统写入（分区/格式化/挂载/服务/SMB/终端） | 不属于 P00 范围 | 按阶段执行，破坏性项仅限 VM 数据盘 |

## 3. 本轮产出的证据文件

- `env.txt`：宿主与工具链实际版本、虚拟化能力探测。
- `dependency-resolution.txt`：Go/Node/npm 版本的原始查询结果与 peer 约束。
- `check-run.txt`：使用冻结 Node 版本的统一检查完整输出与退出码。
- `check-run-host-node.txt`：版本闸门失败演示（退出码 1）。

## 4. 补充记录：验收 VM 创建与核验（2026-10-20，解除 P00.3 阻塞）

- 环境：Windows Hypervisor Platform + 用户级便携 QEMU 11.1.0（`H:\Code\.toolchains\qemu`，
  无管理员权限、无系统级安装）+ `-accel whpx`。过程见 `docs/TEST_ENVIRONMENT.md` 第 8 节。
- 镜像：官方 Ubuntu cloud image 22.04.5 / 24.04.5，SHA256SUMS 校验通过（仓库之外保存）。
- 装机配置：cloud-init NoCloud 经 HTTP 注入（`-smbios type=1,serial=ds=nocloud;s=http://10.0.2.2:PORT/`），
  宿主回环只读白名单服务；VM 网络为 QEMU user NAT，仅 `hostfwd=tcp:127.0.0.1:<port>-:22`。
- VM 规格：2 vCPU / 4096 MB / 40 GiB 系统盘 + 8 GiB + 4 GiB + 4 GiB 数据盘（virtio-scsi，显式 serial）。

| 验收项 | 期望 | 实际 | 结论 |
|---|---|---|---|
| 系统版本 | 22.04 / 24.04 LTS | 22.04.5（5.15.0-198）/ 24.04.5（6.8.0-147） | pass |
| 资源配置 | 2 vCPU / 4096 MB | `nproc` 2 / 2；`free -m` 3915 / 3911 MB | pass |
| 系统盘序列号 | `UPANEL-SYS-40G` | 两台 VM `/dev/sda` 一致（lsblk + /dev/disk/by-id + udevadm 三处交叉） | pass |
| 数据盘序列号 | `UPANEL-DATA-A-8G` / `-B-4G` / `-C-4G` | sdb/sdc/sdd 逐项一致，容量 8G/4G/4G | pass |
| 虚拟盘标识 | VENDOR=QEMU，MODEL=QEMU HARDDISK | 一致 | pass |
| 数据盘未被使用 | 无分区、无挂载 | `lsblk` 拓扑中 sdb/sdc/sdd 无子项 | pass |
| 根文件系统 | 单分区 ext4，在系统盘上 | `/dev/sda1` ext4 37.7G(24.04) / 38.6G(22.04) | pass |
| 宿主磁盘隔离 | 宿主物理盘不得透传 | QEMU `-drive` 只引用 `H:\Codems\` 下 qcow2；宿主 4 块物理盘均未出现 | pass |
| 宿主共享隔离 | 无 9p/virtiofs/cifs/nfs | `findmnt -t 9p,virtiofs,cifs,nfs,nfs4` 空；阳性对照 `-t ext4` 正常输出 | pass |
| 仓库清洁 | 仓库内无磁盘镜像 | `*.qcow2/*.vdi/*.vmdk/*.iso` 数量 0 | pass |
| swap | 未启用（避免干扰内存类验收） | `swapon --show` 空 | pass |

已修正的两处缺陷（首轮 → 本轮）：

1. VM-B 首次启动时系统盘使用 `-drive id=drv_sys` 而未给 `serial=`，导致 `/dev/sda` 无序列号；
   已关机后按统一参数重启，复验三处读取均为 `UPANEL-SYS-40G`。
2. 两台 VM 首轮以 3072 MB 启动，低于 TEST_ENVIRONMENT.md 的 4 GiB 规格；
   已关机后以 `-m 4096` 重启，本表与两份 `vm-*-verification.txt` 均来自 4096 MB 配置。

仍未执行（NOT RUN）：

| 项目 | 原因 |
|---|---|
| VM 内 Go/Node 工具链版本核验 | VM 仅装基础系统；P00 只要求磁盘身份与隔离（DECISIONS Q9） |
| `smartctl` / NVMe SMART 读取 | virtio 盘不提供真实 SMART 数据；P04.3 前另行确认 |
| 破坏性磁盘操作（分区/格式化） | P07 才做；每用例前回快照并重建数据盘 |
| VM-C（LVM 根）/ VM-D（SMB 客户端） | 未创建；P04 / P08 前补齐 |
| QEMU 内部快照（`savevm`） | 当前用文件级重建（保留原始 cloud image 副本）实现回滚 |

证据文件：`vm-2404-verification.txt`、`vm-2204-verification.txt`、`host-isolation.txt`。
