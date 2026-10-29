# uPanel

uPanel 是面向**单台 Ubuntu 服务器**的 Web 管理面板。一个非 root 的 Web 服务（`upanel-web`）
通过 Unix socket 指挥一个 root 特权代理（`upanel-agent`），第三个二进制 `upanelctl` 是只能在
本机跑的 CLI（建管理员、迁移库、查配置）。所有写操作走同一条路径：

```
权限检查 → 预检 → 任务 → 幂等 → 资源锁 → 执行 → 读回核对 → 审计
```

**当前版本 1.0.0，P00—P16 全部完成。** 覆盖系统与进程、磁盘与文件系统、SMB/NFS 共享、
应用与容器、反向代理与证书、软件更新与电源、备份与计划任务、网络与防火墙、监控与告警、
任务中心与审计。

能力清单在 [发布说明](docs/RELEASE_NOTES.md) 第 2 节，但**它只说明有什么，不说明能承诺什么**。
能承诺什么由两份文档加起来才算数：[支持矩阵](docs/SUPPORT_MATRIX.md)（每个域是实机验收过的、
只有单元测试的、还是干脆不支持的）与 [已知限制](docs/KNOWN_LIMITATIONS.md)（未验证的、有意不做的、
容易被误读的）。

## 装一台机器

```sh
sha256sum -c SHA256SUMS                                  # 先校验发布包
tar xzf upanel-1.0.0-linux-amd64.tar.gz
cd upanel-1.0.0-linux-amd64 && sha256sum -c SHA256SUMS    # 再校验包内每个文件
less deploy/scripts/install.sh                           # 读一遍它做什么、不做什么
sudo sh deploy/scripts/install.sh --source .
```

依赖按功能装（`--install-deps` 只装你点名的包），装完的自检、重入、升级与卸载在
[安装说明](docs/INSTALL.md)；装完打不开浏览器见 [访问与证书](docs/ACCESS.md)。
默认给的是 SSH 隧道 + 825 天自签名证书，**不对公网直接开端口**。

## 三条先要知道的边界

- **面板自己不是 root。** Web 服务以 `upanel-web` 用户运行、无 sudo；只有代理是 root，
  它只监听 `/run/upanel/agent.sock`，并且只认契约里登记的动作与查询——**没有执行任意命令的接口**。
- **不可逆的操作要过三道关。** 格式化、删除、覆盖这类动作要求设备/目标身份指纹、服务端签发的
  确认短语与 5 分钟内的密码再认证，而不是一个"确定"按钮。
- **面板不管自己的升级。** 1.0 没有在线自更新；升级是换包重跑安装器，回退要连数据库一起恢复
  （迁移只向前）。见 [安装说明第 7 节](docs/INSTALL.md)。

## 文档在哪

包里带的（`docs/`，装机后机器上同一份，面板不可用时也查得到）：

| 文档 | 什么时候读 |
|---|---|
| [INSTALL.md](docs/INSTALL.md) | 装、升、卸；依赖怎么按需选；装完自检 |
| [ACCESS.md](docs/ACCESS.md) | 用 SSH 隧道打开面板、换受信任证书、对管理网开放 |
| [USER_GUIDE.md](docs/USER_GUIDE.md) | 日常使用：两种角色、四个阶段、每个模块怎么点、常见任务、出问题先看哪里 |
| [RECOVERY.md](docs/RECOVERY.md) | 忘密码、证书坏了、代理不工作、库坏了、网络/防火墙改坏了 |
| [STORAGE_RECOVERY.md](docs/STORAGE_RECOVERY.md) | 面板对磁盘到底写了什么；分区/格式化/fstab 出事怎么处置 |
| [SHARES_CLIENT.md](docs/SHARES_CLIENT.md) | 共享连不上，以及密码经过哪些地方 |
| [SUPPORT_MATRIX.md](docs/SUPPORT_MATRIX.md) | 我这套环境（发行版、架构、文件系统、后端、浏览器）到底行不行 |
| [KNOWN_LIMITATIONS.md](docs/KNOWN_LIMITATIONS.md) | 哪些没验证、哪些不做、哪些语义容易读错 |
| [RELEASE_NOTES.md](docs/RELEASE_NOTES.md) | 这一版有什么、包长什么样、验收读数、报问题带什么 |
| [LOCAL_START.md](docs/LOCAL_START.md) | 开发者：工具链、构建、本地跑、冒烟与排障 |

仓库里还有（不进发布包）：[PLAN.md](docs/PLAN.md) 产品与安全边界、
[CONTRACTS.md](docs/CONTRACTS.md) 接口与数据契约、[STAGES.md](docs/STAGES.md) 阶段与验收条件、
[PROGRESS.md](docs/PROGRESS.md) 逐题进度账本、[EVIDENCE_INDEX.md](docs/EVIDENCE_INDEX.md)
全部证据索引、[DECISIONS.md](docs/DECISIONS.md) 实施决策、[DEVLOG.md](docs/DEVLOG.md) 逐阶段开发记录。

## 验收到了哪一步

真机读数在 `docs/evidence/`，按子任务分目录，索引见 [EVIDENCE_INDEX.md](docs/EVIDENCE_INDEX.md)。
三件事直说：

1. 端到端验收跑在两台专用 Ubuntu 虚拟机上（QEMU、单线程 TCG）：**22.04.5 全绿 148 / 0 / 3**；
   **24.04.5 147 / 1 / 3**，那一条红是"开机到 Web active 152 s"，探针实测面板自己只占 6.3 s，
   其余是模拟器里的客户机引导——**裸机读数没有**。
2. **arm64 只验证了打包与校验和**，端到端验收全在 amd64 上。
3. 有些现场真机造不出来（断电、钉错镜像 digest、多客户端并发等），这些条目只有单测或
   NOT RUN，在 [KNOWN_LIMITATIONS.md](docs/KNOWN_LIMITATIONS.md) A 节逐条列着，**不折算成通过**。
   单元测试与 mock 通过不算真机验收。

## 开发

```sh
npm --prefix web ci --ignore-scripts   # 需要 Node 22.23.3（见 .nvmrc）
sh scripts/check.sh                    # 统一检查；没有的检查输出 NOT RUN，不算通过
sh scripts/build.sh                    # 前端构建 → 嵌入 → 三个二进制到 bin/
sh scripts/release.sh                  # 双架构 tar.gz + 每包 SHA256SUMS + dist/SHA256SUMS
```

工具链版本、本地运行、前端开发与 systemd 手工安装见 [LOCAL_START.md](docs/LOCAL_START.md)。
开发过程与踩过的坑见 [DEVLOG.md](docs/DEVLOG.md)；改代码前请先读
[CONTRACTS.md](docs/CONTRACTS.md)——接口、字段与状态机的唯一来源。
