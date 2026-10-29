# 发布说明 · uPanel 1.0.0

单机版，一台 Ubuntu 服务器一块面板。版本号写在包里的 `VERSION` 文件（含 commit 与构建时间），
`upanel-web --version` 也会打印同一份。

## 1. 这个版本是什么

uPanel 1.0 是一个**单机** Linux 服务器管理面板：一个非 root 的 Web 服务（`upanel-web`）通过
Unix socket 与一个 root 代理（`upanel-agent`）通信。所有写操作都走同一条路径：

```
预检 → 任务 → 资源锁 → 执行 → 读回核对 → 审计
```

这条路径是这一版的核心承诺：**没有一个按钮会在你不知道会发生什么的情况下直接动手**，
也没有任何"执行任意命令"的入口。

## 2. 这一版包含什么

| 域 | 能力 |
|---|---|
| 概览 | 主机、CPU、内存、负载、网络与磁盘概要 |
| 存储 | 磁盘/分区/文件系统查看、GPT 分区表、建删分区、格式化（ext4/XFS）、受管理卷与 fstab、SMART、系统盘保护 |
| 共享 | SMB（Samba）与 NFS 导出、受管理 Linux 账号、SSH 公钥、固定模板 sudo |
| 应用 | 本机应用发现与确认、容器启停、日志、依赖卷门禁；受约束 Compose 部署、更新、健康探测与回切 |
| 服务与进程 | systemd 单元启停/开机自启、进程信号（KILL 要短语）、保护名单 |
| 文件 | 受管理根内浏览/预览/下载/写入/复制/移动/删除、属性修改、暂存上传 |
| 终端 | 受限 Web 终端：降权、无 sudo、一次性许可、数量与时长上限 |
| 日志 | journald 查询与实时流（单页上限、窗口上限） |
| 反向代理与证书 | nginx 站点生成与 `nginx -t` 门禁、reload、证书签发/导入/续期 |
| 软件更新 | 软件包事实、可升级与安全更新、安装/升级/删除（无隐式 dist-upgrade） |
| 电源 | 重启/关机，带倒计时与取消 |
| 备份 | restic（local / sftp）、策略、快照、备份/恢复/抽样验证/清理 |
| 计划任务 | systemd timer、受管理脚本、内置定时身份（备份、证书续期） |
| 网络 | 网卡/路由/监听端口/防火墙查看；单网卡 IPv4 受控写入带独立回滚；UFW 规则受控写入带独立回滚 |
| 监控告警 | 历史监控（5 秒原始点，24h/30d/365d 保留）、规则与事件、确认、静默、通知投递账 |
| 任务与审计 | 全部任务的状态机、事件流、脱敏日志；危险操作有审计与短语 |

## 3. 包里有什么

```
upanel-1.0.0-linux-amd64/
├── bin/upanel-web         面板（非 root 运行，前端已编进二进制）
├── bin/upanel-agent       特权代理（root，只监听 Unix socket）
├── bin/upanelctl          本机 CLI：check-config / migrate / init-admin / reset-password / host-caps
├── deploy/systemd/*.service
├── deploy/scripts/install.sh uninstall.sh backup-state.sh
├── deploy/upanel.example.json
├── docs/                  INSTALL / ACCESS / USER_GUIDE / RECOVERY / STORAGE_RECOVERY /
│                          SHARES_CLIENT / LOCAL_START / SUPPORT_MATRIX / KNOWN_LIMITATIONS /
│                          RELEASE_NOTES
├── README.md
├── VERSION
└── SHA256SUMS             包内每个文件的校验和
```

`dist/SHA256SUMS` 是**发布级**校验和，覆盖两个架构的 tar.gz。

## 4. 先校验，再安装

```sh
# 1) 校验发布包（SHA256SUMS 与 tar.gz 同目录）
sha256sum -c SHA256SUMS

# 2) 解包，逐文件校验
tar xzf upanel-1.0.0-linux-amd64.tar.gz
cd upanel-1.0.0-linux-amd64 && sha256sum -c SHA256SUMS

# 3) 读一遍安装脚本，再跑它（安装器不做 curl | bash 这种事）
less deploy/scripts/install.sh
sudo sh deploy/scripts/install.sh --source .
```

安装步骤、依赖选择、装完自检、重入与卸载见 [INSTALL.md](INSTALL.md)；怎么连上浏览器见
[ACCESS.md](ACCESS.md)。**不要**在没核对校验和之前把它装到生产机上。

## 5. 从 0.1 升级

数据库迁移**只向前**。升级前必须备份（安装器第 9 步会自动做，也可以手工先做）：

```sh
sudo sh deploy/scripts/backup-state.sh --to /root/upanel-backup-$(date +%F)
sudo sh upanel-1.0.0-linux-amd64/deploy/scripts/install.sh --source upanel-1.0.0-linux-amd64
sudo -u upanel-web upanelctl check-config --config /etc/upanel/upanel.json
```

回退到 0.1 需要**同时**恢复数据库备份——1.0 不声称支持自动降级。

说清楚证据状态：**这条路本身没有真机读数**。P16.2 在真机上升级的是"1.0 → 1.0"（备份、迁移、
重启、服务可用整条链），从一份 0.1 的库升上来只有迁移脚本的单测。见
[KNOWN_LIMITATIONS.md](KNOWN_LIMITATIONS.md) A 节。

## 6. 这一版不做什么

不在范围内的东西（多机、IPv6 写入、桥接/bond/VLAN/Wi-Fi、非 UFW 防火墙、非 ext4/XFS 卷、
S3 类备份后端、面板自身在线升级、任意 sudoers、通用命令执行、面板自己的用户管理页面）见
[KNOWN_LIMITATIONS.md](KNOWN_LIMITATIONS.md) 与 [SUPPORT_MATRIX.md](SUPPORT_MATRIX.md)。
**这两份加起来才是完整的"能承诺什么"**，别只看第 2 节的能力清单。

## 7. 验收状态

代码与包的验收读数在**仓库**的 `docs/evidence/`，按子任务分目录，完整索引见仓库里的
`docs/EVIDENCE_INDEX.md`（不随包发出）。三件必须知道的事：

1. **端到端验收跑在两台专用 Ubuntu 虚拟机（单线程 TCG）上**：22.04 全绿（148 PASS / 0 FAIL / 3 NOT RUN），
   24.04 有一条红——`开机到 Web active 152 s > 120 s`，探针实测面板自身只占 6.3 s，其余是模拟器下的
   客户机引导。裸机读数**没有**。
2. **arm64 只验证了打包与校验和**，端到端验收全部在 amd64 上。
3. **部分现场真机造不出来**（例如"钉错镜像 digest"、"断电"），这些条目只有单测或 NOT RUN，
   在 `KNOWN_LIMITATIONS.md` 的 A 节逐条列着，不折算成通过。

## 8. 遇到问题

先按 [USER_GUIDE.md](USER_GUIDE.md) 第 7 节的顺序看：任务中心 → 能力探测 → 审计 → 日志。
报问题请带上：`VERSION` 文件内容、`upanelctl host-caps` 的输出、任务 id（或审计里的 request_id）、
以及你要做的事情和它给你的原文错误。没有这些信息，任何"面板坏了"都只能靠猜。
