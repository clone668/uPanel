# 支持矩阵（uPanel 1.0，单机版）

这张表回答一个问题：**在你手上这台机器上，面板的哪些功能真的能动，哪些只会显示，哪些根本不支持。**
它按"功能"排，不按"阶段"排。

## 0. 怎么读

每一行带一个证据等级，等级说的是"这个结论是从哪来的"，不是"我猜的"：

| 记号 | 含义 |
|---|---|
| **实机** | 在 Ubuntu 虚拟机里真跑过；读数在**仓库**的 `docs/evidence/` 对应目录（不随发布包发出，包里这份表只写结论） |
| **实机（单平台）** | 只在 22.04 上跑过，24.04 没有同一条的读数 |
| **只读实机** | 读路径有真机读数；写路径只有单测 |
| **单测** | 只有宿主上的 `go test` / 假件，**没有**在 Ubuntu 上按过 |
| **不支持** | 面板明确拒绝，或在界面上标"只读 / 不支持"并给出原因 |

一句话原则：**"不支持"是产品行为，"未验证"是证据状态，两者都不等于"能用"**。
凡是这张表没点名验收过的组合，按"未验证"对待。

## 1. 操作系统与平台

| 项 | 支持 | 证据 |
|---|---|---|
| Ubuntu 22.04 LTS | ✅ 全量（最终交付字节上七相位 155 PASS / 0 FAIL / 3 NOT RUN + 页面链路 43/43） | **实机**：`evidence/P16/2026-10-28-p16.2-final/`、`evidence/P15/2026-10-28-p15.4/` |
| Ubuntu 24.04 LTS | ✅ 全量（**同一份字节**上七相位 155 PASS / 0 FAIL / 3 NOT RUN + 页面链路 35/35 与 43/43） | **实机**：`evidence/P16/2026-10-28-p16.2-2404-final/`、`evidence/P16/2026-10-28-p16.2-2404-browser/` |
| 其它发行版（Debian、CentOS、Fedora…） | ✗ 安装脚本直接拒绝 | 安装脚本 `--help` 与 INSTALL.md §1 |
| systemd | ✅ 必需；没有 `/run/systemd/system` 就拒绝安装 | 安装脚本第 2 步 |
| OpenRC / SysVinit | ✗ 不支持；服务、计划任务、部署、回滚定时器全依赖 systemd | 同上 |
| 内核 ≥ 5.6 | ✅ 必需（文件管理器用 `openat2`） | 22.04 是 5.15、24.04 是 6.8，都满足 |
| 面板自身运行身份 | 非 root（`upanel-web`），代理 root 但只挂 Unix socket | **实机**：`evidence/P16/2026-10-27-p16.3/facts/socket-boundary.txt` |

## 2. 架构

| 架构 | 状态 |
|---|---|
| x86_64 / amd64 | ✅ 全部真机验收都在这个架构上 |
| aarch64 / arm64 | ○ 包能打、校验和正确、二进制能起；**没有** arm64 真机端到端验收 |

arm64 这一行不是"不支持"，是"打出来了但没验过"。谁在 arm64 上跑出问题，属于预期内的未知。

## 3. 磁盘、分区与文件系统

| 功能 | 支持范围 | 证据 |
|---|---|---|
| 磁盘/分区/文件系统**查看** | 全部（`lsblk` 家族 + SMART 健康） | **实机**：`evidence/P07/*` |
| 分区表初始化 | 仅 GPT，且只对**无签名空白盘** | **实机**：`evidence/P07/*` |
| 建分区 / 删分区 | 仅 GPT 对齐空闲区；删除要求"没有任何活动依赖" | **实机** |
| 格式化 | 仅 **ext4 / XFS**；要求未挂载的普通数据分区 | **实机** |
| 系统盘保护 | ✅ 沿 `partition_of`/`depends_on` 走 LVM/RAID/swap 闭包 | **实机**：`evidence/P07/*` |
| 受管理卷（挂载、fstab 托管） | ✅ ext4 / XFS 的**普通分区** | **实机**：`evidence/P07/*` |
| LVM / RAID / swap | ◐ 识别、展示、保护；**不做**创建/扩容/重组 | 只读路径实机，写路径不存在 |
| btrfs / vfat / NTFS 等 | ✗ 不受管理卷接管（`NoticeFilesystemUnsupported`） | `internal/storage/volumeassess.go:322`（`FILESYSTEM_UNSUPPORTED`） |
| 格式化后的数据恢复 | ✗ **不存在**自动恢复路径（PLAN 明确写了） | RECOVERY.md §6 |

## 4. 网络与防火墙

| 功能 | 支持范围 | 证据 |
|---|---|---|
| 网卡/地址/路由/监听端口**查看** | ✅ | **实机**：`evidence/P14/*-p14.1/` |
| 改 IPv4（DHCP/静态 + 网关 + DNS） | ✅ 仅 **systemd-networkd** 管理的**单块物理网卡** | **实机**：`evidence/P14/*-p14.2/` |
| NetworkManager | ✗ 只读，并说明原因 | `internal/network/*` 的 `Editable` 判定 |
| 桥接 / bond / VLAN / Wi-Fi | ✗ 只读展示 | 同上 |
| IPv6 写入 | ✗ 只读展示 | 同上 |
| 改地址后的自动回滚 | ✅ 独立 timer（`upanel-netrollback@<token>`），不依赖面板存活 | **实机**：P14.2 演练记录 |
| UFW 规则写 | ✅ 只碰带面板标记的规则 | **实机**：`evidence/P14/*-p14.3/` |
| nftables / iptables（非 UFW） | ✗ 检出后整体只读，不混写两套规则 | 同上的 `ManagerReasons` |
| 改规则后的自动回滚 | ✅ 独立 timer（`upanel-fwrollback@<token>`） | **实机** |

## 5. 共享与账号

| 功能 | 支持范围 | 证据 |
|---|---|---|
| SMB 共享（Samba） | ✅ 定义、`testparm` 校验、配置写入与 reload | **实机**：`evidence/P08/*` |
| NFS 导出 | ✅ 定义、`exportfs` 校验 | **实机**：`evidence/P14/*-p14.4/` |
| 共享绑定卷 | ✅ 卷缺失时拒绝启动 | **实机**：`evidence/P08/*-p08.3/` |
| 受管理 Linux 账号 | ✅ 建/改/删/设口令，**只碰面板创建的账号** | **实机**：`evidence/P14/*-p14.5/` |
| 已有系统账号 / root / 系统 UID | ✗ 只读，不修改 | `internal/accounts` 的 `managed` 判定 |
| SSH 公钥 | ✅ 只改面板管理的 `authorized_keys` 区域 | **实机** |
| sudo 权限 | ✅ 仅**固定模板** + `visudo` 校验；不接受任意 sudoers 文本 | **实机** |
| SMB 客户端 | Windows / macOS / Linux 的 SMB 客户端都能连（标准 SMB3 配置） | **单测** + 文档：SHARES_CLIENT.md |
| NFS 客户端 | 标准 NFSv4 客户端 | **单测** |

## 6. 应用、容器与部署

| 功能 | 支持范围 | 证据 |
|---|---|---|
| 发现本机应用/容器 | ✅ `docker ps` + 单元关联 | **实机**：`evidence/P09/*` |
| 容器启停/重启 | ✅ 仅"受支持的普通容器" | **实机** |
| 应用一键启停 | ✅ 经预检与任务，依赖卷缺失时拦下 | **实机** |
| 受管 Compose 部署 | ✅ 严格受限的部署定义（禁止 raw YAML、privileged、docker.sock、宿主危险挂载） | **实机**：`evidence/P15/*-p15.2/` |
| 部署更新与回切 | ✅ 镜像 digest 记录、更新前备份、健康探测、失败可见 | **实机**：`evidence/P15/*-p15.3/` |
| 部署页（页面链路） | ✅ 新建 → 预检 → 再认证 → 任务 → 详情 → 换镜像 → 删除，全部经页面完成 | **实机**：`evidence/P15/*-p15.4/`（43 条断言，22.04）与 `evidence/P16/2026-10-28-p16.2-2404-browser/`（同样 43 条，24.04，最终交付字节） |
| 回切数据 | ✗ 只回切配置与镜像，**不回切数据** | KNOWN_LIMITATIONS.md §3 |
| Kubernetes / Swarm | ✗ 不在范围 | PLAN.md §2.3 |
| docker / compose 版本 | 22.04 上验过 docker 29.1.3-0ubuntu3~22.04.2 + compose 2.40.3；24.04 上验过 29.1.3-0ubuntu3~24.04.2 + 2.40.3+ds1 | **实机** |

## 7. 反向代理与证书

| 功能 | 支持范围 | 证据 |
|---|---|---|
| nginx 站点 | ✅ 生成配置、`nginx -t` 门禁、原子写入与 reload | **实机**：`evidence/P10/*` |
| 证书签发 | ✅ certbot（需要 `certbot` 与可达的 ACME 目录） | **实机**：`evidence/P10/*-p10.3/` |
| 证书导入 | ✅ 校验证书与私钥匹配 | **实机** |
| 面板自身证书 | 自签名（安装时生成并打印指纹）或用户提供；**不做** ACME | CONTRACTS.md §2 |
| 其它 Web 服务器（Apache、Caddy） | ✗ 不支持 | — |

## 8. 备份

| 功能 | 支持范围 | 证据 |
|---|---|---|
| restic 仓库 | ✅ 两种后端：`local`、`sftp` | **实机**：`evidence/P12/*` |
| S3 / B2 / Azure 等后端 | ✗ 不在范围（restic 支持，面板不支持） | 定界在 `BackendLocal`/`BackendSFTP` |
| 备份 / 恢复 / 抽样验证 / 清理 | ✅ 全部经预检与任务 | **实机** |
| 恢复是否等于可用 | ✗ 「备份成功 ≠ 恢复已验证」，抽样验证是另一件事 | 界面文案与 CONTRACTS §12.3 |
| restic 版本 | 需要机器上装了 restic；版本由 `upanelctl host-caps` 真读 | 见 §11 |

## 9. 浏览器与客户端

| 客户端 | 状态 |
|---|---|
| Chromium（Playwright 驱动，桌面 1440×900） | ✅ 35 条主流程 **实机**：`evidence/P16/2026-10-27-p16.2-browser/` |
| Firefox / Safari / Edge | ○ **未验证**（同一套标准 API，但没跑过） |
| 移动端 / 窄屏 | ○ **未验证** |
| 无 JS 环境 | ✗ 前端是 Vue SPA，必须有 JS |
| RSS / 第三方 API 客户端 | 可以用下面第 10 节的 HTTP 契约，但没做客户端库 |

## 10. 接口与脚本

| 项 | 状态 |
|---|---|
| HTTP API | 全部 `/api/v1`，契约见 `docs/CONTRACTS.md`，OpenAPI 见 `api/openapi.yaml` |
| SSE / WebSocket | ✅ 任务事件流、日志流、终端（同源、Cookie、单次许可） |
| CLI | `upanelctl`：`check-config` / `migrate` / `init-admin` / `reset-password` / `host-caps` |
| 客户端脚本 | `deploy/scripts/backup-state.sh`（备份）、`install.sh`、`uninstall.sh` |

## 11. 外部程序（缺了会怎样）

`upanelctl host-caps` 会真去读这些程序在不在、版本多少，**一共 21 条**（下表把 `mkfs.ext4` 与
`mkfs.xfs` 合成一行；`systemd`、`journalctl`、`systemd-analyze` 是三条独立的）。面板**不会**替你装依赖：

| 能力 | 程序 | 缺了之后 |
|---|---|---|
| 服务、计划任务、部署单元、回滚定时器 | `systemd` | 装不上（安装脚本拒绝） |
| 服务日志 | `journalctl` | 日志页不可用 |
| 容器清单、容器启停、Compose 部署 | `docker` | 应用/部署模块显示"依赖未安装" |
| 受管 Compose 部署 | `docker compose` | 部署模块显示"依赖未安装" |
| 反向代理与 HTTPS 站点 | `nginx` | 代理模块显示"依赖未安装"；面板本身照常工作 |
| 备份与恢复 | `restic` | 备份模块不可用 |
| SFTP 备份仓库、公钥登录 | `ssh`/`sftp` | 只能用本地仓库 |
| 防火墙 | `ufw` | 防火墙只读 |
| 网卡、路由、监听端口 | `ip` | 网络页不可用 |
| NFS 共享 | `exportfs` | NFS 页不可用 |
| SMB 共享 | `smbd`、`testparm` | 共享页不可用 |
| 软件包 | `apt` | 更新页不可用 |
| 证书签发 | `certbot` | 只能手工导入证书 |
| 磁盘 SMART | `smartctl` | 健康信息显示"未安装" |
| 分区表写入 | `sgdisk` | 分区功能不可用 |
| 格式化 | `mkfs.ext4` / `mkfs.xfs` | 对应文件系统不可选 |
| 挂载状态核对 | `findmnt` | 存储写操作拒绝执行 |
| 受管 Linux 账号 | `useradd`/`usermod`/`userdel`/`chpasswd`（shadow 套件） | 账号页不可用 |
| 受限 sudo 模板 | `sudo`/`visudo` | sudo 模板不可用 |
| 计划任务的单元校验 | `systemd-analyze` | 计划任务不可用（不跳过校验） |

## 12. 明确不在范围内

多机管理、集群、容器编排、IPv6 写入、桥接/bond/VLAN/Wi-Fi、NetworkManager、非 UFW 防火墙、
非 ext4/XFS 的卷、S3 类备份后端、面板自身在线升级、任意 sudoers 文本、通过 Web 给面板账号 sudo、
通用命令执行/任意 root shell。理由与边界写在 `docs/PLAN.md` 第 2.3 节与 `docs/CONTRACTS.md` 第 1 节。
