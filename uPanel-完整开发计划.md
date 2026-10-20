# uPanel 完整开发计划（交付合并版）

基准日期：2026-10-20。当前只有开发规格，没有已实现的产品代码。

本文件依次包含：产品与技术计划、接口与数据契约、阶段任务与验收、DeepSeek 执行说明、初始进度账本。可以将本文件直接提供给开发模型；第四部分包含可复制的首轮提示词。

执行方式：先完整阅读，再从 P00 开始，一轮只做一个子任务，逐阶段验收。不要要求模型一次生成全部项目。高风险操作只能在独立 Ubuntu 测试虚拟机验证。

文档职责：PLAN 决定范围和边界，CONTRACTS 决定接口/字段/状态，STAGES 决定顺序与验收，HANDOFF 决定执行规则，PROGRESS 记录真实进度。发现实质冲突应记录并暂停相关工作，不能自行取消保护。

这是五份源文档的交付快照。持续开发时以 docs 目录中的对应文档和最新 PROGRESS.md 为准；源文档修改后须重新生成本合并版。


---

来源：docs/PLAN.md

## uPanel 产品与技术开发计划

文档版本：1.0；日期：2026-10-20；状态：待实施。

### 1. 项目目标

开发一套自托管 Web 管理系统，远程管理安装该系统的单台 Ubuntu 服务器。用户主要需求为：

1. 判断服务器、磁盘和项目是否正常。
2. 管理磁盘分区、文件系统、挂载和文件共享。
3. 集中打开和维护已经安装的 Web 项目。
4. 在浏览器中完成常见运维工作，并能追踪执行结果。

界面语言首先支持简体中文。服务器时间统一以 UTC 存储，界面按浏览器时区显示并明确标注时区。桌面浏览器为主要目标，手机至少能查看状态、任务和告警。

### 2. 版本范围与明确排除项

#### 2.1 0.1 核心单机版

P00—P10 全部通过后交付：登录与权限、审计、任务中心、系统首页、基础监控、磁盘监控、分区创建/删除、GPT 初始化、ext4/XFS 格式化、受管理数据卷挂载/卸载、SMB、服务/进程/日志、文件管理、受限 Web 终端、现有应用接入、现有容器生命周期、域名反向代理和 HTTPS。

#### 2.2 1.0 完整单机版

在 0.1 基础上完成 P11—P16：软件更新和电源、备份恢复、计划任务、历史监控和告警、受约束的网络变更、NFS、系统账号管理、受约束的 Docker 应用部署与更新、完整安装升级恢复流程。

#### 2.3 不在 1.0 范围内

- 多服务器、集群、Kubernetes、虚拟机管理、应用商店。
- LVM、mdraid、硬件 RAID、ZFS、Btrfs、LUKS 的创建、扩容或破坏性操作；可以识别并只读展示。
- 调整已存在分区大小，文件系统缩容，整盘直接创建文件系统，安全擦除/数据销毁。
- Ubuntu 跨发行版升级，内核调优，自动修复磁盘，自动执行 fsck。
- 任意 root 终端、任意 root 命令执行、任意 Compose YAML 的在线运行。
- 任意系统配置文件在线编辑、任意已有网络结构自动迁移。
- 自动承诺数据库一致性备份、任意应用升级回滚、对第三方应用自动单点登录。

这些排除项不得以“高级模式”按钮偷偷引入。新增范围必须先更改规格并得到项目负责人明确接受。

### 3. 固定技术方案

| 层次 | 统一选择 |
|---|---|
| 后端 | Go；Gin HTTP 框架；database/sql + modernc.org/sqlite；显式 SQL 迁移 |
| 前端 | Vue 3 + TypeScript strict + Vite + Vue Router + Pinia + Naive UI |
| 图表与终端 | Apache ECharts；xterm.js；终端 PTY 使用 creack/pty，WebSocket 使用 gorilla/websocket |
| 数据库 | SQLite WAL，开启 foreign_keys，设置 busy_timeout；数据库只放本机文件系统 |
| 实时更新 | 普通状态轮询；任务/日志使用 SSE；只有交互终端使用 WebSocket |
| 特权执行 | 独立 Go 特权代理 upanel-agent，Unix socket 通信，固定动作白名单 |
| 部署 | 两个 Go 二进制 + 前端嵌入 Web 二进制 + systemd；不要求生产机安装 Node.js |
| 反向代理 | Nginx；只管理 uPanel 自己创建的配置文件 |
| HTTPS 证书 | 受控调用 Certbot；初期只支持 HTTP-01 和手动导入证书 |
| 文件共享 | Samba；NFS 使用 nfs-kernel-server |
| 测试 | Go testing；Vitest；Playwright；Ubuntu 隔离虚拟机集成验收 |

不要改为 Python、Node 后端、微服务集群、Redis、PostgreSQL、Kubernetes 或额外消息队列。不要同时引入两套 UI 框架、ORM 或路由方案。

目标平台：Ubuntu Server 22.04 LTS、24.04 LTS，amd64，systemd。arm64 后续扩展。Windows 仅用于前端开发和显式 mock 模式，不能作为 Linux 系统功能已验证的证据。

P00 固定实施时仍受官方维护的 Go 稳定补丁版本、Node 22 LTS 的补丁版本，以及具体依赖版本，记录到 docs/DEPENDENCIES.md。锁定 go.mod/go.sum、package-lock.json 和 Node 版本文件；后续禁止无理由升级依赖。不要猜测尚不存在的版本。版本解析需要网络但不可用时，应登记阻塞，不要虚构安装成功。

### 4. 进程、目录与权限架构

浏览器通过 HTTPS 访问 upanel-web。Web 进程负责认证、权限、元数据、页面和任务编排；upanel-agent 负责经过校验的系统读取和特权动作。浏览器永远不能直连代理。

| 项目 | 固定约定 |
|---|---|
| Web 服务账号 | upanel-web，无 sudo，不加入 docker 组，不提供登录 shell |
| 终端账号 | upanel-shell，独立 home，无 sudo，不加入 docker/disk 等特权组 |
| 特权代理 | root，独立 systemd 服务，只监听 Unix socket |
| 代理 socket | /run/upanel/agent.sock，root:upanel-web，0660；代理核验 SO_PEERCRED 的实际 UID，普通 API 仅允许 upanel-web；root CLI 只使用单独限定的调度/恢复入口 |
| Web 数据 | /var/lib/upanel/upanel.db，以及本服务可写的数据子目录 |
| 代理数据 | /var/lib/upanel-agent/agent.db、操作记录、配置备份；root 独占 |
| 系统配置 | /etc/upanel/，root 所有；Web 账号只能读取必要的非秘密配置 |
| Web 程序 | /usr/local/lib/upanel/upanel-web，root 所有，Web 用户不可修改 |
| 代理程序 | /usr/local/lib/upanel/upanel-agent，root 所有 |
| 挂载目录 | /srv/upanel/volumes/<slug> |
| 应用目录 | /srv/upanel/apps/<slug>；实际数据卷通过依赖关系绑定 |
| 终端 home | /var/lib/upanel-shell，upanel-shell 所有 |
| 日志 | systemd journal；应用中只展示经过权限筛选的日志 |

代理必须重新校验动作参数、设备状态、路径和锁，不能因为请求来自 Web 就直接执行。不得开放 execute(command)、run(shell)、任意 argv、任意文件写入等接口。

常规系统操作用 exec.CommandContext 调用绝对路径的可执行文件，并逐项传参，不经 sh -c/bash -c。程序路径由安装探测确定并写入 root 管理配置，不能由 HTTP 参数指定。环境使用最小白名单，LANG=C.UTF-8，固定 PATH，禁止继承用户可控的加载器变量。

交互终端是单独受限通道：代理创建 PTY，并在启动 shell 前降为 upanel-shell 的 UID/GID，清空附加权限和环境；不得在 root 身份下解释用户输入。Web 进程不能从该通道获得 root shell。

systemd 加固必须与功能兼容：Web 使用 NoNewPrivileges 等限制；代理需要看到并操作主机真实挂载命名空间。不得盲目给代理启用 PrivateMounts 或会隐式创建挂载命名空间的沙箱选项，否则挂载只在代理内部生效。P07 必须从独立主机进程验证挂载可见性。

### 5. 管理访问与身份

- 面板独立监听 HTTPS 9443，不依赖其管理的 Nginx，避免反向代理出错后面板失联。
- 默认绑定 127.0.0.1；安装文档提供 SSH 隧道访问方法。管理员可以明确配置管理网 IP 或全网卡绑定；不自动打开公网防火墙端口。
- 初始证书可自签名，安装时显示指纹和替换方法。密码只能通过 HTTPS 提交。
- 首个管理员由服务器本地 CLI 交互创建。无默认密码、公开注册和永远有效的初始化 URL。
- 面板账号与 Linux/Samba 账号明确分开；不共用密码数据库。
- 0.1 支持 admin、viewer 两种角色。viewer 只能查看经过脱敏的状态、监控、应用入口和任务摘要，并维护自己的应用收藏；不能读取原始日志、文件、配置、终端或秘密，也不能执行系统操作。
- admin 可执行全部已实现管理功能。1.0 添加 TOTP，CLI 提供本机管理员恢复流程。禁止删除/禁用最后一个可用管理员。
- Cookie 会话和 CSRF 规则按 CONTRACTS.md 执行，不使用 localStorage 保存认证令牌。
- 危险操作必须在最近五分钟内完成密码再认证；启用 TOTP 时同时验证 TOTP。
- 登录失败按来源 IP 和账号双维度限速，不采用可被攻击者无限锁死管理员的永久锁定方式。
- 终端、共享密码、私钥、Cookie、令牌、敏感环境变量均不得写入普通日志或审计明文。

### 6. 统一操作流程

所有会改变系统状态的操作都遵守：读取状态 → 请求预检 → 展示影响与阻塞项 → 用户确认 → 创建持久化任务 → 代理再次校验 → 执行 → 读取真实结果 → 记录审计。

用户确认不能只依靠前端。预检由代理生成，绑定具体参数、目标指纹、配置版本和过期时间。执行参数变化后旧预检立即失效。格式化和分区删除需要输入预检给出的确认短语。

前端不发送命令字符串。后台任务有持久状态、日志、锁、幂等键、操作者和关联审计。浏览器断开不终止任务。是否可取消由动作定义决定，前端不能自行展示一个实际无效的取消按钮。

代理或机器重启后，不盲目重试非幂等操作。未能确认结果的任务标为 interrupted，展示已知步骤和人工检查建议。不得仅根据子进程已退出或 HTTP 200 判定操作成功。

### 7. 首页与全局交互

首页包含系统版本、CPU、内存、负载、网络流量、文件系统空间、物理盘健康、异常服务、应用状态、待更新信息、最近任务及常用应用。

导航固定为：概览、应用、存储、共享、容器、服务与进程、文件、终端、日志、备份、计划任务、网络、更新、监控告警、任务中心、设置。未实现模块隐藏，不摆放可点击但无效的功能。

每个页面必须具备加载、空数据、无权限、依赖未安装、数据不支持、请求失败和部分数据可用状态。不用绿色零值代替采集失败。危险按钮不能与普通刷新按钮紧邻。

容量默认显示 IEC 单位（GiB/TiB），悬停显示精确字节；输入分区大小时明确单位。采集时间和陈旧状态可见。状态不能仅靠颜色表达。

应用“打开”默认新标签页，不承诺通过 iframe 嵌入第三方项目，也不绕过对方的 CSP/X-Frame-Options。

### 8. 磁盘发现与监控

监控所有识别到的物理块设备，包括未分区、未格式化、未挂载设备。以设备关系图表达物理盘、分区、dm/LVM、RAID、文件系统和挂载；不能简单假设每个设备只有一个父节点。

采集来源：lsblk JSON、findmnt JSON、/proc/self/mountinfo、/proc/diskstats、/proc/meminfo、/proc/stat、/sys/class/block、swapon JSON、smartctl JSON。读取结果必须有解析器和测试，不解析本地化的列对齐文本。

对 NVMe 优先解析 smartctl 提供的 NVMe 信息；功能探测不支持时显示 unavailable。SMART 的退出状态是位掩码，不能简单把所有非零退出都解释为命令失败。

设备身份优先结合 WWN、唯一 /dev/disk/by-id、序列号、sysfs 路径、major:minor 和容量。API 使用不透明 device_id。执行时重新解析路径，重新核对所有可用身份信息，禁止只用 /dev/sdb 作为目标凭据。没有可靠身份或发现身份冲突的设备只读。

物理盘展示：型号、序列号、接口、容量、在线状态、温度、健康、读写速率、IOPS、平均 I/O 延迟、采集来源和时间。SMART 不可用与健康良好是不同状态。

文件系统展示：总量、可用量、使用率、inode、类型、挂载点、只读状态。网络挂载独立显示可达性和容量；不推断远端硬件健康。网络挂载查询在隔离子进程中执行、限制并发；超时后标记 unavailable，避免主采集器被阻塞。无法终止的内核 I/O 不得引发无限重复查询。

采集频率：基础资源/磁盘计数器每 5 秒；文件系统容量每 30 秒；SMART 每 15 分钟，允许配置为 5—60 分钟。休眠设备默认跳过会唤醒磁盘的 SMART 查询；不支持免唤醒检测时默认不强制探测。监控展示最近有效值及其时间。

吞吐与 IOPS 根据相邻计数差计算，计数器重置/设备替换/采样间隔异常时本次速率为空。平均延迟只在 I/O 次数大于零时计算。不能将分区、dm 和物理盘的同一次 I/O 叠加到总量。

### 9. 分区、格式化与挂载

#### 9.1 支持范围

- 磁盘操作仅限明确识别为普通数据盘的受支持设备。
- GPT 初始化只允许没有分区、文件系统签名及其他占用的空白盘；不自动清除未知签名。
- 只在现有 GPT 空闲区中创建分区，按 1 MiB 边界对齐；初期不修改 MBR 分区表。
- 删除分区前检查其文件系统、挂载、swap、holders、LVM/RAID、应用、共享和备份依赖。
- 格式化仅支持未挂载的普通分区，格式为 ext4 或 XFS；不支持对整盘直接 mkfs。
- 既有 ext4/XFS 普通分区可以导入为受管理卷，但不改写其已有数据。
- 1.0 只对 /srv/upanel/volumes/<slug> 下的受管理挂载执行写操作。其他挂载可以只读展示，不自动接管。

#### 9.2 强制保护

从 /、/boot、/boot/efi、/usr、/var、面板程序/配置/数据库所在文件系统追踪所有底层设备，保护其物理盘和分区。存在活动 swap、未知 holders、LVM/RAID/加密关系时禁止分区和格式化写操作。不能只保护挂载点为 / 的单个分区。

在涉及的整块物理盘上加排他锁，避免两个分区任务同时操作同一磁盘。执行前重读设备拓扑、占用、指纹和预检快照；发现变化则终止并要求重新预检。

GPT 初始化、格式化和分区删除页面展示目标型号、序列号、容量、分区号、现有文件系统、已知依赖和不可恢复的数据影响。确认短语由服务器生成，必须精确匹配。禁止默认勾选确认框。未检测到已知签名不代表盘上绝无原始数据，初始化页面仍必须明确覆盖分区元数据的影响。

分区使用 sfdisk 的固定模板输入，格式化调用固定的 mkfs.ext4/mkfs.xfs 参数集合。不得拼接用户任意选项，不自动追加 force 标志绕过占用检查。完成后等待 udev 稳定并重新探测结果。

#### 9.3 持久挂载

只按 UUID 写入 /etc/fstab 的 uPanel 管理块，保留所有无关内容。卷名限制为小写字母、数字和中划线，并检查重名。挂载选项从枚举选择，初期只提供读写/只读、nodev、nosuid、nofail 和固定设备等待时间；不允许任意字符串。

修改采用：读取并计算版本 → 生成候选 → 校验 → 备份 → 同目录原子替换 → fsync → 执行目标挂载 → 验证 UUID/路径/文件系统 → 提交结果。写入前检测外部修改，不覆盖别人的新配置。

候选文件使用 findmnt 的验证能力检查；不要执行全局 mount -a。ext4 数据卷 fsck pass 为 2，XFS 为 0。失败时恢复本任务改动，并记录实际挂载是否仍存在，不能只回滚数据库。

卸载前展示占用进程和依赖，默认拒绝繁忙卸载；1.0 不提供强制/懒卸载。系统外的既有服务不会被擅自停止。

未挂载的目录设置为不可供普通用户写入；这只是辅助措施，不能防止 root 容器误写，因此还必须执行第 11 节依赖规则。

### 10. SMB 与 NFS 共享

SMB 优先实现，NFS 在 P14。缺少依赖软件时显示安装预检和任务，不在页面加载时偷偷安装。

共享路径必须是已挂载受管理卷内的真实目录，不允许 /、/etc、/proc、/sys、/dev、面板秘密目录。路径校验必须防止符号链接和路径穿越，执行时再次确认卷 UUID。

SMB 默认关闭 guest，最低 SMB2，不使用 chmod 777 解决权限问题。禁用 wide links，初期不跟随共享目录内的符号链接。共享用户为受限 Linux/Samba 账号，不允许登录 shell；共享读写权限与底层 POSIX ACL 必须共同生效。用户密码通过标准输入传递，不放进 argv、日志或面板数据库。

只维护 uPanel 独立配置文件和经过批准的 include。对已有 Samba，先读取配置，识别冲突，首次接入预检展示最小修改；testparm 通过后才重载。失败恢复备份。

每条共享记录关联 volume_id 和相对目录。启用/重载前验证实际挂载 UUID。开机服务依赖确保共享卷存在；卷离线时撤销相应导出并报告错误，避免共享到系统盘空目录。

仅有定时轮询不够：服务依赖和设备事件监听共同处理启动与运行时脱盘。对于会影响已有第三方共享的全局服务停止，先展示影响并拒绝静默接管；用户可在主机上明确配置后重新接入。

NFS 默认仅指定可信网段，root_squash、sync，不允许默认 * 和 no_root_squash。只维护 /etc/exports.d/upanel.exports；应用配置后读取实际导出结果校验。已有共享不自动导入为可编辑。

### 11. Web 应用管理与存储依赖

应用类型固定为 link、systemd、docker、compose。

- link：保存入口、分组、说明、健康检查，可打开，但无启停按钮。
- systemd：关联确切 service 单元，使用服务模块启停与日志能力。
- docker：关联已存在容器的完整 ID，不以可能变化的名称作为唯一依据。
- compose：关联由面板创建并持有标记的项目；外部项目先只读发现，不能直接执行其 YAML。

端口扫描、容器标签、systemd 和进程信息只能生成候选，必须由管理员确认。禁止自动把所有监听端口认定为 Web 应用，也不扫描其他主机。

应用字段包括名称、图标预设、分组、说明、入口 URL、运行绑定、健康检查、依赖卷、部署/数据目录引用和是否收藏。初期图标只选内置图标，避免远程 SVG/图片带来的不必要复杂度。

运行状态和 HTTP 状态分别显示。例如容器 running 不代表 HTTP 可访问。HTTP 探测默认每 30 秒，超时 3 秒，不跟随重定向，响应体最多读取 64 KiB，默认健康状态码为 200—399，可配置明确集合。

健康检查和反向代理目标只允许本机回环地址或明确登记的本机接口地址，以及登记过的容器网络目标。拒绝云元数据地址、任意外网、危险协议和未经检查的 DNS 重绑定。应用入口 URL 可以是供用户浏览器打开的外部 HTTPS 地址，但服务器不能因此自动抓取它。

服务器内部 upstream 和浏览器访问 public_url 是不同字段。浏览器访问地址禁止自动生成 localhost/127.0.0.1，除非明确处于本机开发模式。

关键依赖规则：

1. 卷卸载、格式化、删除前检查关联应用/共享/备份，存在活动依赖时拒绝执行并给出列表。
2. 面板启动应用前检查依赖卷实际 UUID。
3. 面板管理的 systemd 服务使用 RequiresMountsFor，并对必需卷设置对应 mount unit 的 BindsTo 与 After，确保停止顺序也正确；仅写数据库依赖不算完成。
4. 面板管理的 Compose 使用 systemd 包装单元控制生命周期，依赖卷通过 mount unit 和启动前 UUID 校验约束。相关容器 restart 必须为 no，避免 Docker 在挂载前绕过包装单元启动。
5. 运行时卷失联应停止相关受管理服务/容器，并发出告警。不能保证保护的外部项目标为“依赖保护未接管”，不可显示为已保护。
6. 不通过自动创建缺失目录让挂载失败看起来正常。

### 12. Docker 与应用部署

0.1 读取容器、镜像、网络、数据卷、状态、资源和日志，支持现有普通容器的启停/重启。Web 账号不接触 docker.sock。代理通过固定 Docker API 操作，不暴露任意 API 转发。

发现 privileged、host PID/network、危险 capability、宿主根目录或 docker.sock 挂载的容器时，显示风险信息；1.0 的面板写操作不接管这类容器。外部 Compose 项目只读展示和逐容器管理，明确不具备 Compose 整体更新能力。

P15 提供受约束的单机应用定义：镜像引用、容器端口、主机端口、允许的数据目录、环境变量、健康检查和资源限制。由代理渲染 Compose 文件。用户不能提交任意 YAML、Dockerfile、构建上下文、宿主设备和扩展能力。

禁止 privileged、宿主网络/PID/IPC、任意 devices/cap_add、docker.sock、宿主 / 等挂载；允许卷路径必须来自受管理卷或面板专用应用目录，并通过句柄级路径校验。容器部署在能力上接近宿主管理操作，必须 admin + 再认证 + 预检。

更新流程：预检磁盘与卷 → 记录当前镜像 digest/配置 → 按用户选择备份 → 拉取新镜像 → 替换 → 检查健康 → 输出结果。镜像与配置可以回切，但发生数据库迁移时数据可能不可逆；不能标注为无条件一键回滚。恢复必须依据具体备份。

### 13. 反向代理与证书

只管理 /etc/nginx/conf.d/upanel-*.conf；保留已有配置。首次接入检测监听端口、server_name 和配置冲突，不覆盖发行版默认站点或用户文件。

0.1 只支持每个站点一个独立域名到一个明确上游。子路径代理、复杂 rewrite、多 upstream 和负载均衡后续扩展。域名做 IDNA/长度/字符校验，不接受用户原始 Nginx 片段。

支持 WebSocket，设置明确连接/读取超时和请求体大小上限。上游使用第 11 节目标限制。不得将管理面板的 9443 当作隐式公开目标。

变更通过预检、原子写入、nginx -t、reload、实测入口完成。校验失败恢复旧配置，不能重启整个 Nginx 作为常规重载方法。

站点访问模式：public、CIDR allowlist、basic_auth。basic_auth 密码仅保存安全哈希；UI 明确其与应用自身账号独立，不承诺第三方 SSO。

证书采用 Certbot HTTP-01，先检查域名/DNS提示、80 端口可达条件及站点归属，测试使用 ACME staging。不要在循环重试中消耗正式签发限额。自签名和手动导入也可用；私钥由 root 保管，禁止 API 读取或下载。

续期由受管理 timer 调用同一个任务处理流程；成功后校验并重载 Nginx，失败告警。无需 DNS API token，因此 1.0 不提供 DNS-01 或通配符证书申请。

### 14. 服务、进程、日志与终端

systemd 服务使用精确 unit 名称和结构化属性，校验单元存在。不得接受任意 systemctl 参数。禁止通过通用服务接口停止面板、代理、sshd、systemd 核心以及网络基础服务；Nginx、Samba、Docker 等由所属模块执行受约束动作。

进程终止绑定 PID + starttime，防止 PID 重用。先 SIGTERM；强制 SIGKILL 需要单独预检与确认。禁止终止 PID 1、面板/代理自身、内核线程和受保护系统服务进程。

日志按 unit、时间、级别和游标查询。原始日志仅 admin 可读；过滤秘密是尽力措施，不能作为向 viewer 开放日志的依据。限行、限字节、限时间范围；流断开释放子进程和订阅资源。

终端仅 admin，创建前再认证并审计。终端以 upanel-shell 运行，默认 15 分钟无输入关闭，最长会话 1 小时，每个管理员最多 1 个会话，全局最多 2 个。记录会话创建/结束及账号，不默认记录终端全文。网络断开宽限 60 秒后终止；0.1 不做 tmux 式持久终端。

### 15. 文件管理

仅浏览显式登记的文件根目录，初始仅受管理卷和面板应用数据目录。不能通过输入任意绝对路径访问整个系统。

API 使用 root_id + 相对路径，不接收任意服务器绝对路径。Linux 通过 openat2/dirfd 约束访问边界；初期不跟随符号链接。拒绝 ..、NUL、magic links 和越界硬链接/特殊文件处理。复制、移动、压缩包解压也不能绕过这些限制；1.0 不提供服务端解压功能以缩小风险。

功能：列表、创建目录、上传、下载、复制、移动、重命名、删除普通文件/目录、查看权限和所有者。文本编辑上限 1 MiB，允许文本编码仅 UTF-8；默认上传上限 2 GiB，可由本机配置调整。上传流式写临时文件，完成后原子替换，禁止整文件读入内存。

写入绑定文件版本，避免覆盖并发修改。编辑前创建备份，每文件最多保留 10 份、全部编辑备份默认最多 500 MiB，达到上限按最旧记录清理；备份位于对应卷的受保护目录，不能默默挤满系统盘。删除展示数量和预计大小，目录删除单独确认。权限修改仅限非系统数据目录，不允许 setuid/setgid、特殊设备文件和任意系统用户所有权赋值。

### 16. 软件更新与系统电源

读取 APT 已安装包、候选版本和安全更新；安装/卸载限定精确包名，不接受任意 apt 参数。卸载先模拟，若涉及 Essential/Protected 包、运行中的内核、SSH、网络或 uPanel 依赖则拒绝。内核安装按 Ubuntu 包管理流程完成，不自定义引导配置。

刷新索引、安装、升级、卸载使用统一全局锁，并尊重系统原有 apt/dpkg 锁；外部包管理正在执行时排队或报告忙，绝不删除锁文件。命令环境固定，现有配置文件默认保留，无法非交互处理的情况明确失败并提供人工处理说明。

APT 写操作开始后不可随意取消。检查磁盘空间、模拟结果、退出码、dpkg 状态和 /var/run/reboot-required。默认只提供明确选择的软件更新，不自动执行发行版升级、autoremove 或强制依赖修复。

重启/关机要求再认证、独立确认和短倒计时；先检查关键任务。不要把“关机命令已受理”显示成“服务器已成功关机”。重启可通过 boot_id 变化确认；关机后显示已下发且连接断开，最终状态不可确认时如实说明。

### 17. 备份、恢复与计划任务

1.0 备份引擎统一采用 restic，避免自行实现加密和去重。支持本地路径和 SFTP 仓库，其他后端以后扩展。仓库密码和 SFTP 凭据用本机 root 密钥加密保存；API 不回传原文。

备份源限受管理文件根目录和明确列出的面板配置。排除 /proc、/sys、/dev、socket 与临时目录。只选择一个目录不代表其内容具备数据库一致性；应用备份需选择“停止受管理应用后备份”或应用明确支持的导出流程。1.0 不执行任意 root 前后置脚本。

面板数据库通过 SQLite 在线备份接口取得一致副本，不能仅复制正在运行的 .db 而忽略 WAL。恢复数据库前停止写入服务并验证 schema 兼容性。

恢复先落到同一目标卷的空暂存目录，再检查路径、类型和权限后发布到目标。禁止特殊设备文件、setuid/setgid、越界链接和未经映射的系统账号所有权；不能用 restic 的目标参数绕过文件根限制。恢复默认到新的空目录；覆盖恢复需要单独预检、再认证、停止依赖应用和明确确认。恢复完成后检查文件和应用状态。备份成功不等于恢复已验证，界面分别展示最近备份和最近恢复验证时间。

计划任务只写 uPanel 自己的 systemd service/timer；现有 cron 和其他 timer 先只读展示。支持受管理的备份、证书续期及以 upanel-shell 身份运行的用户脚本，不支持任意 root 脚本。监控采集使用程序内固定调度，不暴露任意 root 监控脚本任务。

时区、下次执行时间、错过执行策略、最大运行时长和不重叠规则必须明确。配置 timer 前用 systemd-analyze calendar 校验。定时触发也通过统一任务幂等、资源锁和审计流程，不能形成另一套执行路径。

### 18. 历史监控与告警

基础监控从 P04 提供实时数据，P13 增加历史保留和告警。原始 5 秒数据保留 24 小时，1 分钟聚合保留 30 天，1 小时聚合保留 365 天；采集与聚合批量写入，定期清理并限制数据库大小。

告警包含 CPU/内存持续超限、空间与 inode 不足、磁盘健康异常/离线、服务异常、应用 HTTP 不健康、备份失败、证书临期。

空间阈值同时支持使用率和剩余 GiB，触发逻辑可选“任一满足/全部满足”；不同卷独立配置。连续触发时间、恢复阈值、通知冷却、维护静默和重复去重明确配置，避免瞬时尖峰刷屏。

默认提供面板内通知；1.0 支持 SMTP 邮件和 HTTPS webhook。通知目标只有 admin 可配置，限制协议、超时、大小和目标地址；发送只发生在用户配置启用后，支持显式发送测试通知。

### 19. 网络、防火墙与系统账号

网络先只读展示。1.0 仅支持由 systemd-networkd 管理的单个物理网卡的 DHCP/静态 IPv4、网关和 DNS 修改。桥接、bond、VLAN、Wi-Fi、NetworkManager、IPv6 写入先显示不支持，不能替换整个 netplan 配置来碰运气。

修改前保存配置和运行状态，在 systemd 中安排独立的 120 秒自动回滚任务，再应用候选配置。用户必须通过候选网络地址重新连接并确认；只从旧地址确认不证明新配置可用。若候选未改变管理地址，则确认流程绑定本次变更和正常连接。超时、浏览器关闭或 Web 崩溃都必须回滚。未确认变更还须持久记录，主机重启时由恢复单元优先处理，不能因运行时 timer 消失而永久保留错误配置。

UFW 修改同样提供预检、配置快照和独立回滚。不能直接启用 UFW 并意外挡住当前管理入口或 SSH；保留当前已知管理通路。若检测到非 UFW 防火墙管理器或无法完整恢复现状，则只读。

系统用户管理仅管理面板创建的普通账号与共享账号；已有系统账号只读，不修改 root 和系统 UID。SSH 公钥校验格式，原子更新面板管理的 authorized_keys 内容，权限正确；sudo 权限变更使用受约束模板并以 visudo 校验，不接受任意 sudoers 文本。

1.0 不提供通过 Web 给 upanel-web/upanel-shell 授予 sudo 权限的功能，也不通过用户管理绕过终端限制。

### 20. 安装、升级与恢复

安装程序必须先探测系统、架构、systemd、磁盘空间、端口、已有 Nginx/Samba/Docker 等依赖。按功能列出缺失依赖，由用户选择安装，不一次性装满所有软件。

提供带校验和的发布包和安装说明，不要求用户执行不可审阅的 curl | bash。首次安装生成账号、目录、socket、证书、配置和服务；失败时输出已完成步骤与可重入修复命令。

升级前备份配置和两个数据库，校验二进制与迁移兼容；关键系统任务执行期间拒绝升级。只有迁移兼容时才能回退程序，不能宣称所有数据库迁移均可自动降级。

卸载默认保留业务数据、卷、分区、备份仓库、第三方应用和已有共享。列出面板创建的服务/配置，由用户明确选择移除；不把卸载脚本实现成递归删除 /srv/upanel。

恢复文档至少覆盖：忘记管理员密码、证书问题、代理不可用、数据库损坏、fstab 错误、网络回滚、APT 中断、磁盘操作中断。磁盘格式化不存在自动恢复保证。

### 21. 性能与质量目标

- 参考环境：2 vCPU、4 GiB RAM、10 块盘、50 个服务、20 个应用，指标在该环境测量后记录。
- 普通缓存查询 API p95 小于 500 ms；不能在请求线程中串行执行多条慢命令。
- 采集器故障不阻塞登录和其他页面；并发、超时、日志大小、上传大小都有上限。
- 不把外部命令输出或日志直接当 HTML；输出统一转义。
- 正常管理员操作必须有审计；失败和被拒绝的高风险操作也有记录。
- 所有磁盘破坏性验收在独立 Ubuntu VM 和可丢弃虚拟数据盘中完成；不得在开发者或用户真实磁盘上做验收。
- 测试失败、未安装依赖、未执行 Linux 验收必须写明；禁止用 mock 测试通过冒充真实系统功能可用。

### 22. 仓库布局

    cmd/upanel-web/           Web 入口
    cmd/upanel-agent/         特权代理入口
    cmd/upanelctl/            本机初始化与恢复 CLI
    internal/api/            HTTP 路由、认证、DTO
    internal/auth/           会话、密码、权限、CSRF
    internal/store/          Web SQLite 与迁移
    internal/jobs/           编排、幂等、状态、对账
    internal/agentclient/    固定 Unix socket 客户端
    internal/agent/          动作注册、代理持久化、锁
    internal/platform/      Linux 实现与显式 mock 实现
    internal/storage/       发现、关系图、保护判断
    internal/apps/          应用绑定、健康与依赖
    internal/monitoring/    采集、聚合、告警
    internal/config/        配置读取与版本
    web/                    Vue 前端及 package-lock.json
    api/openapi.yaml        与 CONTRACTS.md 对齐的公开契约
    migrations/             Web 数据库 SQL 迁移
    agent-migrations/       代理数据库 SQL 迁移
    deploy/systemd/         systemd 单元模板
    deploy/scripts/         可审阅安装、升级、卸载脚本
    tests/fixtures/         脱敏的系统输出样本
    tests/integration/      Linux VM 集成测试
    docs/                   本计划、契约、验收、运维文档

包名和文件可合理细分，但不得把所有功能塞进 main.go、单个巨型 Vue 页面或一个万能命令执行器。Linux 特定代码使用 build tags；非 Linux 系统不能自动退回伪成功。

### 23. 实施原则

按 STAGES.md 的 P00—P16 严格依次实施。一个阶段只做该阶段范围，每阶段更新真实进度和证据。必要的跨模块接口先按契约建立，未实现部分返回 NOT_IMPLEMENTED 并在 UI 隐藏，不返回模拟成功。

无法安全实现时，提供明确的 unsupported 状态和阻塞原因。涉及磁盘写操作、权限边界、网络回滚或恢复的失败不能降级为普通待办后宣布发布。


---

来源：docs/CONTRACTS.md

## uPanel 接口与数据契约

版本：1.0；配套 PLAN.md 和 STAGES.md。本文中“必须”是实现约束，不允许前后端各自发明字段。

### 1. 命名、时间、数值与响应

- 公开 API 前缀：/api/v1；JSON 字段统一 snake_case，TypeScript DTO 保留同名，不做隐式驼峰转换。
- 元数据主键使用 UUID v4 字符串；设备身份 ID 为代理生成的不透明字符串，不能当设备路径解析。
- 时间为 RFC 3339 UTC，例如 2026-10-20T08:30:00Z。空值使用 null，不能混用空字符串、0 和 unknown。
- 字节、累计扇区和其他可能超过 JavaScript 安全整数范围的计数器使用非负十进制字符串，例如 size_bytes: "1000204886016"。前端使用 BigInt 或安全格式化工具；不经 Number 转换做精确运算。
- 百分比使用 0—100 浮点数；比率明确带 _per_sec，时延带 _ms，温度带 _celsius。未知值为 null。
- 列表默认 limit=50，最大 200，cursor 为不透明游标。列表排序稳定；不允许无限量导出。
- 所有请求都有 request_id，返回 X-Request-ID。错误包含可翻译的 code、中文可读 message 和结构化 details，不返回堆栈/原始秘密。

普通响应：

    {"data":{"id":"..."},"request_id":"..."}

列表响应：

    {"data":{"items":[],"next_cursor":null},"request_id":"..."}

错误响应：

    {"error":{"code":"DEVICE_IN_USE","message":"分区正在使用，不能格式化","details":{"blockers":[{"kind":"mount","id":"...","label":"/srv/upanel/volumes/data"}]}},"request_id":"..."}

状态码：400 语法错误；401 未登录/会话失效；403 权限或再认证不足；404 不存在；409 冲突/忙/预检失效；413 超限；422 参数合法但业务条件不允许；429 限速；501 功能尚未实现；503 代理或依赖不可用。异步写任务创建成功为 202，绝不是业务操作成功。

统一错误码至少包括：VALIDATION_ERROR、UNAUTHENTICATED、FORBIDDEN、REAUTH_REQUIRED、CSRF_FAILED、NOT_FOUND、VERSION_CONFLICT、RESOURCE_BUSY、DEVICE_IN_USE、PROTECTED_DEVICE、UNSUPPORTED_DEVICE、DEVICE_IDENTITY_CHANGED、PATH_OUTSIDE_ROOT、DEPENDENCY_BLOCKED、PREFLIGHT_EXPIRED、PREFLIGHT_STALE、CONFIRMATION_MISMATCH、IDEMPOTENCY_CONFLICT、AGENT_UNAVAILABLE、DEPENDENCY_MISSING、NOT_IMPLEMENTED、COMMAND_FAILED、OPERATION_INTERRUPTED、ROLLBACK_FAILED。

### 2. 权限与会话

admin：完整的已实现管理权限；viewer：仅允许下表明确标注 V 的只读接口，且服务端使用脱敏 DTO。未列出的权限默认拒绝。

会话 Cookie 固定名称 __Host-upanel_session，HttpOnly、Secure、SameSite=Strict、Path=/，不设置 Domain。随机值至少 256 bit，服务端只保存哈希。默认 30 分钟空闲超时、12 小时绝对超时；登录/权限变化后轮换会话，退出立即失效。

已登录所有非 GET/HEAD 请求需要 X-CSRF-Token；令牌绑定会话，前端只保存在内存。登录接口要求 JSON Content-Type、X-UPanel-Request: 1 和精确匹配的 Origin。Origin 白名单来自本机配置，不由任意 Host 或 X-Forwarded-* 生成。WebSocket 握手同样校验 Cookie、Origin 和单次会话许可。

日志流、下载和 SSE 可以使用 Cookie 的同源 GET，服务端不开放跨域。生产默认不信任代理头，只有明确配置可信代理地址才解析其来源。

| 方法与路径 | 输入/输出 | 权限 |
|---|---|---|
| POST /auth/login | username、password、可选 totp；返回 user、csrf_token、expires_at | 匿名，Origin+限速 |
| GET /auth/session | 返回当前 user、csrf_token、expires_at、reauth_expires_at | V |
| POST /auth/logout | 销毁当前会话 | V+CSRF |
| POST /auth/reauth | password、可选 totp；返回 reauth_expires_at | admin |
| GET/POST /users | 列表；创建 username、display_name、role、initial_password | admin |
| PATCH /users/:id | version、display_name、role、disabled；不能自我提权 | admin+再认证 |
| POST /users/:id/password | new_password；使目标已有会话失效 | admin+再认证 |
| DELETE /users/:id | version；不得删除最后可用管理员 | admin+再认证 |
| POST /auth/totp/setup | 返回一次性 enrollment_id 和二维码数据，尚未启用 | admin+再认证，P14 |
| POST /auth/totp/confirm | enrollment_id、totp；启用并返回一次性恢复码 | admin+再认证，P14 |
| DELETE /auth/totp | password、totp 或恢复码；使相关令牌失效 | admin+再认证，P14 |

密码使用 Argon2id，参数以实施环境基准设定并记录，目标校验约 100—300 ms，参数随哈希保存；采用维护中的库而非自行实现。秘密字段不能进入通用请求日志中间件。

### 3. 系统与能力发现

| 方法与路径 | 输出 | 权限 |
|---|---|---|
| GET /health/live | 最小存活状态，不返回内部路径/版本细节 | 匿名 |
| GET /meta | version、mode、capabilities、server_time、timezone_display_policy | V |
| GET /system/summary | OS、内核、uptime、boot_id、CPU/内存/负载、告警摘要、采集时间 | V |
| GET /system/metrics?scope=&from=&to=&step= | 明确 scope 的时序样本；限制时间范围/点数 | V |
| GET /audit | 审计分页和筛选 | admin |

capabilities 为列表，每项包含 id、state、reason、required_packages、supported_actions。state 为 ready、missing_dependency、unsupported、disabled、mock。模式为 production 或 mock，不允许 silent fallback。mock 服务绑定 127.0.0.1:18881，与真实代理完全隔离；页面持续显示“演示数据，未操作服务器”。

### 4. 预检、执行和幂等

所有系统写动作通过以下两步提交。应用元数据等纯数据库 CRUD 不走代理，但仍有权限、版本和审计。

#### 4.1 预检

POST /operations/preflight

    {"action":"storage.filesystem.format","params":{"device_id":"dev_...","filesystem":"ext4","label":"data"}}

返回：

    {"data":{"preflight_id":"...","action":"storage.filesystem.format","normalized_params":{"device_id":"dev_...","filesystem":"ext4","label":"data"},"target":{"display_name":"...","identity_fingerprint":"..."},"expires_at":"2026-10-20T08:32:00Z","allowed":true,"blockers":[],"warnings":[],"impacts":[{"kind":"data_loss","description":"该分区已有数据将被清除"}],"requires_reauth":true,"confirmation_phrase":"FORMAT ABC123 P1","cancellable":false,"locks":["disk:dev_..."]},"request_id":"..."}

预检有效期 120 秒，绑定用户、会话、动作、规范化参数和目标状态版本。危险确认短语由服务端生成，不能由前端决定。GPT 初始化、格式化、删除分区、递归删除目录、覆盖恢复和仓库清理必须要求确认短语；其他动作按注册策略要求普通确认或短语。预检本身不写磁盘、不停止服务、不安装软件。

#### 4.2 创建任务

POST /jobs，必须携带 Idempotency-Key（客户端生成 UUID，一次意图重试时复用）。

    {"preflight_id":"...","confirmation_phrase":"FORMAT ABC123 P1"}

不再接受 action 或 params，避免提交时替换目标。服务端按预检恢复参数。预检为一次性，用户权限/会话变化则失效。相同用户+接口+幂等键+相同请求返回原 job；相同键但请求不同返回 409。

返回 202：

    {"data":{"job_id":"...","status":"queued","events_url":"/api/v1/jobs/.../events"},"request_id":"..."}

数据库先建立 durable job 和稳定 operation_id，随后向代理提交。网络超时后按 operation_id 查询，不再次生成新 operation_id 执行。代理也保存 operation_id 唯一约束和请求哈希。重复点击、HTTP 重试、Web 重启不应导致重复格式化或重复修改分区。

预检到执行间资源变化，代理即使已接受任务也必须在真正执行前检查，失败使用 PREFLIGHT_STALE 或 DEVICE_IDENTITY_CHANGED。锁冲突可在 30 秒内排队；超时返回 RESOURCE_BUSY，不能无限占用 worker。预检在有效期内被接受后可排队，但状态快照仍需执行前复核。

#### 4.3 任务 API 与状态

| 方法与路径 | 行为 | 权限 |
|---|---|---|
| GET /jobs | 任务摘要列表；viewer 不含原始参数/日志 | V |
| GET /jobs/:id | 状态、步骤、百分比或 null、结果、错误、是否可取消 | V，字段按角色裁剪 |
| GET /jobs/:id/events | SSE 实时事件，支持 Last-Event-ID | admin |
| GET /jobs/:id/logs?cursor=&limit= | 脱敏日志分页 | admin |
| POST /jobs/:id/cancel | 请求安全取消；不可取消返回 409 | admin |

状态机：queued → running → succeeded / failed / canceled / interrupted；queued 也可直接 canceled / failed。终态不可再次回到 running，重新操作必须创建新任务和新预检。cancellation_requested 是独立布尔字段，不是成功取消。

progress_percent 不知道时为 null，不得用假的定时器从 0 涨到 100。step_code 采用实现登记的稳定枚举，message 为可读描述。

result 至少包括 outcome（verified、accepted、rolled_back、partial、unknown）、verified_at、observed_state、recovery_hint。普通写任务只有验证真实结果后才能 succeeded。电源类动作允许 succeeded + accepted，界面必须表述为“请求已受理”；不能据此声称关机已验证。

格式化、分区写入、APT 写事务开始后不可取消。日志跟踪、未开始的任务、支持安全中断的备份可以取消。代理异常退出后未决任务标为 interrupted；先查询实际状态，不自动重新执行。

SSE 使用持久递增 seq 作为 id，事件类型为 status、progress、log、gap；每 15 秒发送注释心跳。重连不重执行任务。历史已清理时发 gap，客户端重新拉取状态。限制每个用户连接数、慢客户端缓冲和日志容量。

### 5. 动作注册表

下表是公开 action 的固定命名。每一项都必须实现参数 DTO、代理校验器、预检器、执行器、结果验证器、审计摘要和测试。禁止以默认分支接受未知动作。

“R”表示提交任务时需要有效再认证。所有动作只允许 admin；定时任务仅能调用明确允许的动作，并使用受管理的内部身份审计。

| action | params 的允许字段 | 主要锁/约束 | R |
|---|---|---|---|
| storage.partition_table.initialize | device_id、table_type=gpt | disk；仅无签名空白盘 | 是 |
| storage.partition.create | device_id、start_bytes、size_bytes、name | disk；GPT、对齐、空闲区 | 是 |
| storage.partition.delete | device_id（分区） | parent disk；无任何活动依赖 | 是 |
| storage.filesystem.format | device_id（分区）、filesystem=ext4/xfs、label | parent disk；未挂载普通数据分区 | 是 |
| storage.volume.import | device_id、slug、display_name | disk+fstab；不格式化，不自动接管外部挂载 | 否 |
| storage.volume.mount | volume_id、version | disk+volume；核对 UUID | 否 |
| storage.volume.unmount | volume_id、version | disk+volume+依赖；拒绝繁忙 | 是 |
| storage.volume.configure | volume_id、version、automount、read_only、nodev、nosuid | fstab+volume；不任意更改路径 | 是 |
| storage.volume.forget | volume_id、version | volume+fstab；已卸载且无依赖，仅移除管理记录 | 是 |
| share.apply | share_id（新建为 null）、version（新建为 null）、definition | share-service+volume；testparm/exportfs 验证 | 是 |
| share.disable | share_id、version | share-service+volume | 否 |
| share.delete | share_id、version | share-service；只删导出配置，不删数据 | 是 |
| share.account.apply | account_id（新建为 null）、name、secret_ref、disabled | accounts+samba；只能受管理账号 | 是 |
| share.account.delete | account_id | accounts+samba；存在共享引用则拒绝 | 是 |
| service.control | unit、operation=start/stop/restart/enable/disable | service:unit；保护名单 | 否 |
| process.signal | pid、start_time_ticks、signal=TERM/KILL | process:pid:starttime；保护名单 | KILL 是 |
| terminal.create | cols、rows | 专用 UID；数量和会话上限 | 是 |
| file.mkdir | root_id、relative_path | file-root；禁止符号链接逃逸 | 否 |
| file.commit_upload | upload_id、overwrite、expected_version | file-root；流已完成、空间检查 | 覆盖是 |
| file.write_text | root_id、relative_path、expected_version、content | file-root；UTF-8、1 MiB、先备份 | 否 |
| file.copy / file.move | source_root_id、source_path、target_root_id、target_path、expected_version | 两个 root 按稳定顺序加锁；目标存在则拒绝 | 否 |
| file.delete | root_id、relative_path、expected_version、recursive | file-root；目录删除需短语 | 是 |
| file.chmod | root_id、relative_path、expected_version、mode | mode 仅普通权限位；禁止 setuid/setgid | 是 |
| file.chown | root_id、relative_path、expected_version、managed_account_id、managed_group_id | 仅受管理账号/组，禁止任意 UID | 是 |
| app.control | app_id、version、operation=start/stop/restart | app+服务/容器+依赖卷 | 否 |
| container.control | container_id、operation=start/stop/restart | container；受支持普通容器 | 否 |
| proxy.apply | proxy_id（新建为 null）、version、definition | nginx+domain | 是 |
| proxy.delete | proxy_id、version | nginx+domain；不删除应用 | 是 |
| certificate.issue | proxy_id、email、environment=staging/production | cert+domain；明确签发环境 | 是 |
| certificate.import | proxy_id、certificate_upload_id、private_key_secret_ref | cert+domain；验证证书与私钥匹配 | 是 |
| certificate.renew | certificate_id | cert+nginx；允许内置定时身份 | 否 |
| package.refresh | 空对象 | apt-global | 否 |
| package.install / package.remove | packages（精确包名数组） | apt-global；模拟和保护名单 | 是 |
| package.upgrade | packages（明确选中的包名数组） | apt-global；禁止隐式 dist-upgrade | 是 |
| system.power | operation=reboot/poweroff、delay_seconds（30—300） | power-global；检查关键任务 | 是 |
| backup.repository.initialize | repository_id、version | repository；只允许新空仓库，不覆盖已有数据 | 是 |
| backup.run | backup_policy_id、version | repository+源卷；内置定时身份可用 | 否 |
| backup.restore | repository_id、snapshot_id、target_root_id、relative_path、overwrite | repository+目标卷+依赖应用 | 是 |
| backup.verify | repository_id、snapshot_id、sample_restore | repository；临时恢复目录有空间限额 | 否 |
| backup.prune | backup_policy_id、version | repository；预检列出要删除的快照，保留策略校验 | 是 |
| schedule.apply | schedule_id（新建为 null）、version、definition | timers；只允许登记动作 | 是 |
| schedule.delete | schedule_id、version | timers；不碰外部 timer | 是 |
| script.apply | script_id（新建为 null）、version、name、content | scripts；只写固定目录、归属 upanel-shell | 是 |
| script.delete | script_id、version | scripts；存在计划引用则拒绝 | 是 |
| script.run | script_id、version、max_runtime_seconds | script；仅 upanel-shell，允许已授权的定时身份 | 是 |
| network.apply | interface_id、expected_config_hash、definition | network-global；独立回滚 | 是 |
| firewall.apply | expected_config_hash、definition | firewall-global；独立回滚 | 是 |
| account.apply | account_id（新建为 null）、version、definition | accounts；只管理面板创建账号 | 是 |
| account.delete | account_id、version | accounts；检查共享/文件等依赖，不删 home 数据 | 是 |
| ssh_key.apply / ssh_key.delete | account_id、version、key_id、public_key（仅 apply） | accounts；只修改受管理区域 | 是 |
| sudo_rule.apply / sudo_rule.delete | account_id、version、template_id | sudoers；固定模板且 visudo 通过 | 是 |
| deployment.apply | deployment_id（新建为 null）、version、definition | compose-project+ports+volumes | 是 |
| deployment.update | deployment_id、version、image_reference、backup_policy_id（可空） | compose-project+volumes | 是 |
| deployment.remove | deployment_id、version | compose-project；保留数据卷和业务目录 | 是 |

定义动作参数的结构在以下各模块契约中给出。字符串均设置最大长度，未知字段直接拒绝，不静默接受并忽略。

### 6. 存储 API 与对象

| 方法与路径 | 返回 |
|---|---|
| GET /storage/devices | devices 列表与采集时间；V |
| GET /storage/devices/:id | 设备、关系、健康、可操作能力与阻止理由；V |
| GET /storage/topology | nodes、edges、snapshot_version、collected_at；V |
| GET /storage/volumes | 受管理卷列表；V |
| GET /storage/volumes/:id | 卷及依赖摘要；V |
| GET /storage/volumes/:id/blockers | 占用进程/应用/共享/备份详情；admin |

Device 必需字段：id、kind、name、size_bytes、logical_sector_bytes、physical_sector_bytes、model、serial、wwn、transport、read_only、online、identity_confidence、identity_fingerprint、filesystem、partition_table、health、capabilities、collected_at。

kind：disk、partition、lvm、raid、crypt、loop、rom、other。loop 只用于显式测试环境；生产界面默认隐藏 loop/rom 的普通磁盘卡片，但拓扑可显示。未知类型不能提供破坏性按钮。

filesystem：type、uuid、label、mounts；partition_table：type、partitions、free_regions。partition 项有 device_id、number、start_bytes、size_bytes；free_regions 有 start_bytes、size_bytes，均来自同次探测。

health：status 为 healthy、warning、critical、unknown、unsupported、sleeping；包含 temperature_celsius、source、checked_at、details。不要把 unavailable 的温度转成 0°C。

TopologyEdge：from_device_id、to_device_id、relation=partition_of/depends_on；约定 from 是子设备/消费者，to 是父设备/底层提供者。保护和锁沿 depends_on/partition_of 递归追踪，不依靠设备名称猜测。

Volume：id、version、display_name、slug、device_id、filesystem_uuid、filesystem_type、mount_path、automount、read_only、nodev、nosuid、desired_state、observed_state、capacity、dependencies、updated_at。

observed_state：mounted、unmounted、missing、mismatch、error、unknown；实际 UUID 不匹配时为 mismatch 并拒绝启用依赖。capacity 只在实际挂载且检查成功时存在，包含 total_bytes、available_bytes、used_percent、inode_total、inode_available、inode_used_percent。

动作中的 label 校验按 ext4/XFS 的实际限制分别实现，不能只用通用 255 字符。分区 start_bytes/size_bytes 需满足逻辑扇区与 1 MiB 对齐，并且不得越界或重叠。

### 7. 共享契约

GET /shares、GET /shares/:id、GET /shares/:id/connections、GET /share-accounts：admin。viewer 只在首页看到共享数量和健康摘要。

ShareDefinition 字段：name（1—64，受限字符）、protocol=smb/nfs、volume_id、relative_path、enabled、read_only、allowed_cidrs、smb_users、nfs_options。

- smb_users 为 account_id + access=read/write 数组；不允许空名单自动变 guest。
- nfs_options 初期仅允许 root_squash=true、sync=true；允许网段不能为空。
- 新建共享发现同名已有系统共享时返回冲突；不得通过名称相同就覆盖。
- 卷未挂载或 UUID 不同，应用动作返回 DEPENDENCY_BLOCKED。
- 更新返回配置哈希和验证结果；删除只移除导出配置，永远不删除目录内容。

AccountSummary：id、name、type=share/regular、managed、disabled、has_password；不返回密码/哈希。

POST /secrets：admin+再认证，仅接受用途枚举和 value；代理加密保存，返回 secret_ref 与过期时间。未被配置引用的临时秘密 10 分钟后销毁；一旦被引用转为持久秘密。用途包括 smb_password、account_password、proxy_basic_password、tls_private_key、backup_repository_password、sftp_private_key、notification_password、app_env。禁止通用 GET 明文接口。TOTP 种子使用代理内部的 totp_seed 用途，由 enrollment 流程生成，不接受通用秘密上传替代。

### 8. 应用、容器与部署契约

| 方法与路径 | 行为 |
|---|---|
| GET /apps、GET /apps/:id | 脱敏应用详情；V |
| POST /apps | 创建元数据；admin |
| PATCH /apps/:id | version + 可编辑元数据；admin |
| DELETE /apps/:id | version；只删除入口/绑定，不卸载项目；admin |
| GET /apps/discovery | 候选列表，每项说明发现来源；admin |
| GET /apps/:id/health | 最近探测结果；V |
| GET /apps/:id/logs | 关联服务日志；admin |
| GET/POST /app-groups、PATCH/DELETE /app-groups/:id | 列表 V，写 admin；重名和版本检查 |
| PUT /apps/:id/favorite | 当前用户的 favorite 布尔值；V+CSRF |
| GET /containers、GET /containers/:id | admin；完整 ID、运行状态、资源、受支持能力 |
| GET /containers/:id/logs | admin；分页/流，沿用日志限额 |
| GET /images、GET /container-networks、GET /container-volumes | admin；0.1 只读 |
| GET /deployments、GET /deployments/:id | admin；配置脱敏，禁止回传 secret 值 |

AppDefinition：name（1—80）、description（最多 1000）、icon_key、group_id（可空）、type、binding、public_url、health_check、volume_dependencies。

binding 按 type 严格判别：link 为空；systemd 包含 unit；docker 包含 container_id；compose 包含 deployment_id。禁止一个对象同时绑定多个类型。

health_check：enabled、target_id、scheme=http/https、port、path、expected_status_codes、interval_seconds（30—300）、timeout_seconds（1—10）。target_id 来自代理发现的本机/容器目标，不允许任意 host。path 只能以 / 开头，不接受含域名的 URL。除非单独明确设定受信任 CA，不关闭 TLS 验证。

AppSummary 额外输出 runtime_state、http_state、last_checked_at、dependency_protection=managed/unmanaged/not_applicable、capabilities、version。public_url 必须是 http/https URL，禁止 javascript/data/file 等协议。

DeploymentDefinition：name、image_reference、container_port、host_bind_address、host_port、environment（公开值或 secret_ref）、mounts、resource_limits、health_check、volume_dependencies。

mounts 仅允许 source_root_id、relative_path、container_path、read_only；禁止 .. 和 /proc、/sys、/dev 等目标敏感路径。镜像引用允许 tag 或 digest，执行后记录实际 digest。资源限制初期必填 memory_limit_bytes、cpu_limit；端口 1—65535，冲突检测和锁必须同时存在。definition 不含原始 compose_yaml 字段。

环境变量名称按 POSIX 标识符校验；secret_ref 只能用于 app_env。渲染配置 root 独占、0600，读取 API 返回“已设置”。禁用 Docker restart，生命周期经包装 systemd 单元管理。

### 9. 反向代理与证书契约

GET /proxies、GET /proxies/:id、GET /certificates：admin。

POST /certificates/uploads：admin+再认证+CSRF，multipart 单个 PEM 公钥证书文件，最多 1 MiB；返回 certificate_upload_id，绑定会话，10 分钟到期。只暂存输入，导入生效必须执行 certificate.import 任务。

ProxyDefinition：app_id（可空）、domain、upstream_target_id、upstream_scheme、upstream_port、access_mode、allowed_cidrs、basic_auth_users、websocket_enabled、max_body_bytes、read_timeout_seconds、enabled。

domain 为一个不带协议/路径/端口的有效域名；初期不允许通配符。basic_auth_users 为 name + secret_ref，返回只含 name。CertificateSummary 返回 id、domains、issuer、not_before、not_after、renewal_state、last_error，不返回私钥路径或内容。

手动证书公钥通过受限上传接口接收，大小不超过 1 MiB；私钥通过 /secrets 上传。证书申请与续期不接受任意 Certbot 参数。

### 10. 服务、进程、日志、终端契约

| 方法与路径 | 行为 |
|---|---|
| GET /services、GET /services/:unit | V；状态与能力，unit 需要 URL 编码 |
| GET /processes | admin；pid、start_time_ticks、owner、cpu_percent、memory_bytes、脱敏命令摘要 |
| GET /logs?source=&unit=&from=&to=&cursor=&limit= | admin；最多 1000 行/1 MiB，时间范围最多 24 小时 |
| GET /logs/stream | admin；SSE，固定数据源白名单 |
| GET /terminal/sessions | admin；仅当前用户 |
| POST /terminal/sessions/:id/connection-grants | admin+CSRF；仅会话创建者，生成 30 秒单次连接许可；允许 60 秒宽限内重连 |
| GET /terminal/sessions/:id/connect | WebSocket；只允许创建者，握手 Origin 校验，创建许可单次使用 |
| DELETE /terminal/sessions/:id | admin；关闭自己会话，审计 |

terminal.create 任务完成后返回 session_id 和短期许可，连接许可通过 WebSocket 子协议的受控字段传递，禁止放 URL 查询参数/访问日志。握手后客户端只能发送 input 和 resize 两类结构化消息；cols/rows 限 1—500，单帧最大 64 KiB，总速率有限额。服务端返回 output 和 exit。二进制终端数据不经过普通 HTML 渲染。

### 11. 文件契约

所有文件 API 为 admin。以下为数据访问接口，实际修改通过第 5 节动作提交。

| 方法与路径 | 行为 |
|---|---|
| GET /files/roots | 返回允许访问的 root_id、名称、容量、可写能力 |
| GET /files/list?root_id=&path=&cursor= | 子项 name、type、size_bytes、mode、owner、mtime、version；不跟随 symlink |
| GET /files/text?root_id=&path= | UTF-8 文本、version；上限 1 MiB |
| GET /files/download?root_id=&path= | 流式下载普通文件；Content-Disposition 安全编码；无 inline HTML |
| POST /files/uploads | root_id、path、size_bytes、expected_version；创建上传暂存，会话绑定 |
| PUT /files/uploads/:id/content | application/octet-stream 流，CSRF 与大小检查；不支持断点续传 |
| GET /files/uploads/:id | received_bytes、state、expires_at |
| DELETE /files/uploads/:id | 删除暂存上传，不影响目标文件 |

上传完成后仍需 file.commit_upload 预检与任务才能替换目标。未完成暂存 24 小时后清理。容量预留在并发上传之间共享检查；磁盘不足必须删除本次暂存并返回错误。

路径边界：Linux 使用 openat2 的 RESOLVE_BENEATH、RESOLVE_NO_MAGICLINKS、RESOLVE_NO_XDEV 和不跟随 symlink 的策略；不支持该机制的平台拒绝真实文件操作。先验证根目录实际挂载 UUID 并打开根句柄，随后禁止跨入根下其他 bind mount/子挂载；子挂载如需访问须独立登记为文件根。初期拒绝 nlink>1 的普通文件读取和写入，避免硬链接越界难以判定；特殊文件不读写。不能用字符串 startsWith(root) 代替边界校验。

文件 version 由设备/ inode /大小/纳秒 mtime 等生成，并在已打开句柄上核验。跨根目录 move 不是原子操作：采用复制到目标暂存→校验→原子落位→删除源；中途失败保留源并显示实际状态，不声称原子完成。

### 12. 软件、电源、备份与计划任务契约

GET /packages?query=&installed=&upgradable=、GET /packages/updates、GET /system/power-state：admin。

GET/POST /backup/repositories、PATCH /backup/repositories/:id：admin+再认证；这些接口只登记元数据，初始化真实仓库使用 backup.repository.initialize 任务。字段 name、backend=local/sftp、location、password_secret_ref、credential_secret_ref、version。location 按 backend 判别：local 为 root_id+relative_path；sftp 为 host、port、username、remote_path、host_key_fingerprint。代理验证路径/主机，不接受 restic 任意命令行选项；SFTP 主机密钥必须显式登记，禁止 StrictHostKeyChecking=no。

GET/POST /backup/policies、PATCH/DELETE /backup/policies/:id：admin；字段 name、repository_id、sources（root_id+relative_path）、consistency_mode=files_only/stop_managed_apps、app_ids、retention（keep_last/keep_daily/keep_weekly）、version。执行前展示停机影响。删除策略不删除仓库或快照。1.0 的清理使用管理员明确发起的 backup.prune；备份定时任务不隐式执行删除操作。

GET /backup/snapshots?repository_id=、GET /backup/snapshots/:id：admin；展示时间、来源、大小、consistency_mode 和验证记录。删除历史快照/清理仓库必须采用显式、单独预检的策略执行，不在页面查询时触发。

GET /schedules、GET /schedules/:id：admin；ScheduleDefinition 包含 name、kind=backup/certificate_renewal/user_script、target_id、calendar_expression、timezone、persistent、max_runtime_seconds、enabled。脚本 target_id 必须指向预先登记且由 upanel-shell 运行的普通脚本，不能是 shell 命令文本。

GET /scripts 只返回元数据，GET /scripts/:id 可读取管理员上传的内容，均为 admin。创建/修改/删除通过 script.apply/script.delete 预检与任务执行；content 为 UTF-8、最多 64 KiB，不能指定任意路径。代理保存为 root 所有且 upanel-shell 只读可执行的受管理脚本，运行前核对已批准内容哈希，以避免终端用户修改已获授权的定时脚本。script.run 执行时降权，永远不获得 root。

### 13. 网络、账号与告警契约

GET /network/interfaces、GET /network/routes、GET /network/listeners、GET /firewall、GET /accounts、GET /accounts/:id/ssh-keys：admin。

NetworkDefinition：mode=dhcp/static、address_cidr（静态必填）、gateway（可空）、dns_servers、confirmation_timeout_seconds=120。只允许 PLAN.md 的可写平台和配置类型。其他复杂配置返回 unsupported，不能丢弃未理解字段。

FirewallDefinition：enabled、default_incoming=deny、default_outgoing=allow、rules；每条 rule 为 id、action=allow/deny、protocol=tcp/udp、port_start、port_end、source_cidr。初期不支持任意 UFW 命令、转发和 NAT 规则。

POST /network/changes/:id/confirm、POST /firewall/changes/:id/confirm：admin+CSRF，输入 confirmation_token；token 绑定变更、会话和目标连接地址，服务端验证连接实际到达的本地地址，不相信浏览器自报。取消回滚 timer 前持久化确认结果，并确保不存在竞争窗口。

初版写网络配置时要求面板通过直连管理网地址访问；通过 SSH 隧道或外部反向代理无法可靠验证候选地址时，只读展示并提示使用主机控制台完成变更。不能用请求头伪造候选地址确认。

网络任务运行期间的 change_state：pending_apply、awaiting_confirmation、confirmed、rolling_back、rolled_back、rollback_failed。任务在 awaiting_confirmation 时仍为 running。确认成功后 succeeded；自动回滚成功后 failed + rolled_back；回滚失败为 failed + partial 并提供本机恢复步骤。

AccountDefinition：name、display_name、type=regular/share、disabled、password_secret_ref（可空）、shell_profile=disabled/restricted。系统 UID 由安全范围分配；禁止用户提交 uid=0、任意 shell 路径或任意 groups。sudo 模板初期只允许“查看选定服务状态/日志”等固定只读能力，不提供 unrestricted 或 ALL 模板。

GET/POST /alerts/rules、PATCH/DELETE /alerts/rules/:id：读 V、写 admin；规则字段 metric、target_id、comparison、threshold、secondary_threshold、combination=any/all、duration_seconds、recovery_threshold、cooldown_seconds、enabled、version。

GET /alerts/events：V；POST /alerts/events/:id/ack：admin；GET/POST /notification-channels、PATCH/DELETE /notification-channels/:id、POST /notification-channels/:id/test：admin；通道字段 type=in_app/smtp/webhook、受约束 endpoint/settings、secret_ref、enabled。测试发送是明确的操作，不能在保存时自动发送。

### 14. 最小数据库模型

以下为逻辑表；实施时用 SQL 迁移建立真实外键、索引和约束。不能只在前端检查唯一性。所有 mutable 元数据表包含 version、created_at、updated_at；PATCH 用 WHERE id=? AND version=? 更新并自增，零行即 409。

#### 14.1 Web 数据库

| 表 | 关键字段/约束 |
|---|---|
| users | id、username UNIQUE、password_hash、role、disabled；角色 CHECK |
| sessions | id_hash UNIQUE、user_id FK、csrf_hash、created_at、last_seen_at、expires_at、reauth_until |
| user_totp | user_id UNIQUE FK、代理 secret_ref、enabled、恢复码哈希；无明文种子 |
| audit_events | id、actor_id（可空）、actor_snapshot、action、target、request_id、job_id、outcome、redacted_details、created_at；只追加 |
| preflights | id、user_id、session_hash、action、params_hash、agent_ref、expires_at、consumed_job_id |
| jobs | id、operation_id UNIQUE、user_id、action、status、progress、step、result_json、error_json、timestamps |
| job_events | job_id FK、seq、kind、redacted_payload、created_at；UNIQUE(job_id,seq) |
| idempotency_keys | user_id、route、key、body_hash、job_id、expires_at；联合 UNIQUE |
| volumes | id、filesystem_uuid UNIQUE、slug UNIQUE、device_identity、desired_config_json、observed_state、version |
| shares | id、name UNIQUE、protocol、volume_id FK、definition_json、applied_config_hash、enabled |
| managed_accounts | id、name UNIQUE、type、system_identity、managed、disabled；不保存密码 |
| app_groups | id、name UNIQUE、sort_order |
| apps | id、name、type、binding_json、public_url、health_json、group_id FK、version |
| app_favorites | user_id FK、app_id FK；联合 UNIQUE |
| app_volume_dependencies | app_id FK、volume_id FK；联合 UNIQUE |
| proxies | id、domain UNIQUE、app_id FK 可空、definition_json、applied_config_hash |
| certificates | id、proxy_id FK、domains_json、expiry、agent_certificate_ref、renewal_state |
| deployments | id、name UNIQUE、definition_json、applied_digest、agent_project_ref、version |
| file_roots | id、volume_id FK 可空、kind、agent_root_ref、display_name、read_only |
| backup_repositories | id、name UNIQUE、definition_json、agent_secret_refs；无秘密明文 |
| backup_policies | id、repository_id FK、definition_json、last_verified_at |
| schedules | id、name UNIQUE、definition_json、agent_timer_ref、enabled |
| scripts | id、name、agent_script_ref、content_hash、version |
| metric_samples | scope、target_id、metric、timestamp、resolution、value；组合索引 |
| alert_rules | id、definition_json、enabled、version |
| alert_events | id、rule_id FK、state、first_seen、last_seen、resolved_at、dedup_key |
| notification_channels | id、type、settings_json、secret_refs、enabled |
| settings | key PRIMARY KEY、value_json、version；不能存 root 秘密 |

复杂 definition_json 必须由严格 DTO 校验并包含 schema_version=1；JSON 不意味着允许任意字段。外键使用 RESTRICT 为默认，只有会话/收藏等明确从属对象可 CASCADE；删除卷必须检查全部依赖。

#### 14.2 代理数据库与秘密

代理数据库独立由 root 读写，不能让 Web 直接编辑。表至少包括：operations（operation_id UNIQUE、request_hash、动作、阶段、结果）、operation_events、preflight_plans（参数/设备指纹/到期/使用状态）、managed_resources（主机实际接管清单）、config_backups、secrets（密文、用途、引用、时间）、pending_network_rollbacks。

秘密使用维护中的 AEAD 实现，独立随机 nonce；本机密钥保存 /etc/upanel/agent.key，root:root 0600。密钥不可由 Web 读取；备份/灾难恢复文档明确要求单独保管该密钥。

两个数据库不是分布式事务。代理完成操作后记录最终资源和结果，Web 按 operation_id 对账后更新元数据。若 Web 写库失败，任务保持待对账标记并重试同步，不能重新执行系统操作。启动时检查所有非终态和未同步任务。

任务日志默认保留 30 天、每任务最多 10 MiB；审计保留 180 天且清理由管理员配置。日志截断必须有显式标记。幂等记录至少保留 24 小时，关联的执行历史不能因清理提前丢失导致重复执行。

### 15. Web 与代理私有协议

协议为 HTTP/JSON over Unix socket，前缀 /internal/v1。请求体最大值按动作独立限制；不得把公开 HTTP 路由整个转发给代理。Web 通过专门 agentclient 调用。

| 路径 | 含义 |
|---|---|
| GET /internal/v1/capabilities | 版本、支持动作、依赖和运行模式 |
| POST /internal/v1/queries | query 枚举 + typed params；只允许登记的结构化读取 |
| POST /internal/v1/preflights | actor_context、action、typed params；返回代理生成计划 |
| POST /internal/v1/operations | operation_id、preflight_ref、confirmation、actor_context；持久接受后异步运行 |
| GET /internal/v1/operations/:id | 实际状态和结果；可用于超时后对账 |
| GET /internal/v1/operations/:id/events | 代理日志/状态流 |
| POST /internal/v1/operations/:id/cancel | 仅安全取消 |
| POST /internal/v1/secrets | 用途与秘密；返回引用，无读取明文路由 |
| POST /internal/v1/schedules/:id/trigger | 仅 SO_PEERCRED UID=0 的本机 CLI 可调用；从代理已批准配置取动作，输入只允许触发 ID，不接受 action/params |
| POST /internal/v1/streams | 创建受限日志/文件上传下载/终端流许可，绑定用途和参数 |
| GET/PUT /internal/v1/streams/:id | 单次受限流；再次核验 socket 凭据、路径/UID/大小限制 |

query 枚举按模块登记，例如 system.summary、storage.topology、services.list、files.list；禁止 query=command。terminal 的字节通道只能连接已降权 PTY，不能进入代理的命令解释器。P14 增加 /internal/v1/totp/enroll、/confirm、/verify、/disable 四个专用接口，操作绑定用户与用途，种子不进入普通 query/secret 读取通道。

内置定时任务只可执行已登记且启用的 backup.run、certificate.renew 和 script.run。systemd 调用 root 所有的固定 upanelctl schedule-trigger 命令，代理只在专用 trigger 路由接受 UID=0；普通路由仍只接受 upanel-web。系统身份来自 root 持有的任务配置与本地凭据，不允许公开 /jobs 请求自称 scheduler 来跳过再认证。定时触发须重新预检资源与锁，创建稳定触发 ID 并审计；交互确认由管理员保存计划时的显式授权替代，不能扩展到格式化或网络变更。代理产生的定时 operation 会被 Web 对账导入任务表，不能只存在于 journal。

actor_context 供审计关联，包括 user_id、session_id_hash、request_id、reauth_at。代理信任 Web 完成用户登录认证，但仍独立实施系统安全边界；不能把该设计描述为可抵御已完全控制 Web 进程的管理员身份伪造。

锁统一由代理维护。磁盘锁沿拓扑追踪到所有底层盘，多个资源按规范字符串排序后获取，避免死锁。进程内锁不足以协调外部 apt/配置编辑，还必须检测系统锁和配置哈希。所有外部命令有运行时限、输出限额和退出状态处理。

### 16. 契约验证要求

P00 建立 OpenAPI 文件骨架，后续每阶段先补齐对应 schema、枚举、权限、错误、请求示例再实现。不要在阶段未实现时生成“已支持”的 API 文档。

每个变更验证前后端类型、未知字段拒绝、枚举和状态机一致。关键契约至少覆盖：容量大整数、viewer 越权、CSRF、幂等冲突、预检过期、设备变化、文件版本冲突、任务重连、代理离线、非零退出、进程重启对账。

本文没有给出外围功能的所有 SQL 列与 UI 文案，但已固定其动作、参数、范围和实现次序。实施模型可以补充内部细节；不能借此新增产品能力、改名公开字段或取消检查。


---

来源：docs/STAGES.md

## uPanel 阶段任务与验收

本文件是执行顺序。阶段为 P00—P16，阶段内使用 Pxx.n 标识子任务。一次开发对话优先只完成一个子任务；较小的相邻子任务可在同一阶段内连续处理，但不能跳过验收进入下一阶段。

每个子任务按以下顺序执行：阅读契约 → 更新 OpenAPI/迁移设计 → 实现后端 → 实现前端 → 必要测试 → 真实环境核验 → 更新 PROGRESS.md。前端必须使用同一套 API DTO，禁止单独写一套含义不同的模型。

### 1. 所有阶段通用的完成条件

1. 只完成本阶段约定范围，未引入未授权产品能力。
2. 构建、类型检查、相关测试通过，没有新增隐藏的 mock 或空成功响应。
3. 后端完成鉴权、参数校验、错误处理；前端完成加载、空状态、错误和无权限状态。
4. 系统写操作有预检、任务、幂等、锁、结果核验和审计。
5. 修改配置有原子写入、原配置保护和失败处理；涉及多系统状态时说明实际回滚边界。
6. 对应接口和运维说明更新；PROGRESS.md 记录命令、环境、结果、证据路径和已知限制。
7. 未执行的 Linux 测试标为“未验证”。mock 成功、代码能编译、页面能点开均不能替代真实系统验收。
8. 没有为了让测试通过而删除保护逻辑、放宽 root 权限、关闭 CSRF、忽略错误或硬编码成功。

推荐检查命令由 P00 固定为 Go test/vet、前端 typecheck/lint/test/build、Playwright 和 Linux VM 测试。不要假定某命令运行过；必须保留实际结果。

### 2. P00：冻结基线和验收环境

目标：确保开发模型理解项目边界，建立可复用环境，不实现业务功能。

- P00.1：阅读全部规格，输出范围摘要；创建 docs/DECISIONS.md，记录固定技术栈、版本范围、权限边界、文件路径和未决问题。无问题则写“无”，不要凭空发明阻塞。
- P00.2：核实依赖版本，建立 docs/DEPENDENCIES.md、Go/Node 版本约束和锁文件生成策略；建立 api/openapi.yaml 骨架、CI 工作流和统一检查脚本。
- P00.3：建立 Ubuntu 22.04/24.04 VM 验收说明，准备独立系统盘和至少两块可丢弃虚拟数据盘；建立 docs/TEST_ENVIRONMENT.md、tests/fixtures/ 和证据目录约定。

验收：能解释 Web、代理、终端三个身份；明确 0.1/1.0/后续范围；VM 数据盘有可识别序列号且与宿主磁盘隔离。安装测试软件需要外部权限时登记所需环境，不能在真实服务器上代替验收。

证据：版本文件、依赖清单、VM 配置和测试运行办法。此阶段不能标注任何产品模块已完成。

### 3. P01：最小可运行骨架

- P01.1：建立目录结构、两个服务入口和 CLI 入口；配置解析、结构化日志、版本、live health；Linux 与 mock 实现分离。
- P01.2：建立 SQLite 迁移机制、事务工具、错误响应、request_id、中间件和 API 路由组织。数据库重启后保留数据；迁移失败停止启动并说明原因。
- P01.3：Vue 页面骨架、路由、Naive UI、中文导航、统一 API 客户端、错误组件和 mock 横幅。尚未实现模块隐藏。
- P01.4：构建前端并嵌入 Go Web 二进制；建立开发与生产配置；提供初版 systemd 单元模板和本地启动文档。

验收：production 不连接 mock；非 Linux production 启动明确拒绝；mock 仅绑定本地 18881；同源 API 和静态页可加载；未知 API 不返回 index.html 或 200；无依赖时不伪装成正常。

检查：Go 单元测试与 vet、前端 typecheck/build、基础页面冒烟。P01 不需要写磁盘、文件或管理容器。

### 4. P02：认证、权限、审计与代理边界

- P02.1：通过 upanelctl 创建第一个管理员；密码哈希、登录、退出、会话轮换、CSRF、Origin、限速；无默认密码。
- P02.2：admin/viewer、账号管理、最后管理员保护、再认证；实现字段级脱敏 DTO。
- P02.3：代理 Unix socket、UID 校验、固定动作注册、结构化只读查询、命令执行器超时/输出上限；此时只开放已实现的只读能力。
- P02.4：追加式审计；日志秘密脱敏；独立代理数据库和秘密加密存储。

验收：未登录 401、viewer 系统写入 403、伪造 Origin/缺 CSRF 拒绝、Cookie 标志正确、退出后旧会话不能用；Web 用户无法访问 docker.sock、代理数据库和密钥；非白名单 UID 不能调用代理，root CLI 也仅能调用明确的专用路由；未知动作和用户命令字段被拒绝。

测试必须包含：SQL 注入输入、JSON 未知字段、最后管理员删除、登录重试限速、秘密不出现在日志/审计/错误响应中。原始系统日志不得向 viewer 开放。

### 5. P03：预检、持久任务和执行可靠性

- P03.1：预检 DTO、120 秒期限、动作/参数/身份绑定、再认证、确认短语、快照版本。
- P03.2：持久 jobs、幂等键、operation_id、代理执行记录、资源锁、排队和取消规则。
- P03.3：SSE 事件、断线重连、日志限额、任务中心、状态机和错误详情。
- P03.4：Web 与代理重启对账、重复提交、执行结果未知的 interrupted 处理；测试用无害动作由独立测试注册表提供，不放入生产白名单。

验收：同一次请求提交十次只产生一次系统执行；相同幂等键不同请求返回 409；预检过期和状态变化拒绝；浏览器关闭任务继续；Web 在提交后掉线仍能找到原 operation；代理崩溃不自动重试破坏性动作；不可取消动作不显示可取消。

到此为后续系统写操作的共同前置条件。未通过时禁止开始磁盘写入代码。

### 6. P04：系统首页和存储只读能力

- P04.1：CPU、内存、负载、网络、系统信息采集，轮询缓存与时间戳。
- P04.2：块设备关系图、所有物理盘、分区/逻辑设备、挂载和容量、swap/holders；设备身份和系统盘保护判断。
- P04.3：SMART/NVMe 解析、休眠跳过、不支持状态、diskstats 速率/IOPS/延迟、不可达网络挂载隔离。
- P04.4：首页、磁盘列表、拓扑/分区详情、健康与容量卡片；写操作能力此时不启用。

验收：未挂载盘可见；两块同容量磁盘不混淆；多层 dm/LVM/RAID 拓扑不重复累计；根分区底层整盘被保护；SMART 缺失显示未知/不支持；计数器归零不产生负速率；采集失败不会卡住登录。

夹具至少包括：SATA、NVMe、USB、无序列号/重复身份、SMART 非零位掩码、睡眠盘、LVM 根卷、RAID、loop、坏 JSON、空数组、多挂载点。Linux VM 实测基础数据与系统工具结果一致。

### 7. P05：服务、进程与日志

- P05.1：服务列表、详情和保护名单；通过任务执行允许的启停/重启/开机启动设置。
- P05.2：进程列表、PID+starttime 身份、TERM/KILL 分开的流程；禁止终止关键进程。
- P05.3：journal 结构化查询、筛选、游标和实时流，日志权限、截断和资源回收。

验收：操作测试 service 的真实状态发生变化且有审计；不存在的 unit 不被当作命令片段；不能停止代理/面板/SSH/核心服务；PID 重用后旧预检不能杀新进程；结束日志订阅不会残留无限 journalctl 子进程。

### 8. P06：文件管理与受限终端

- P06.1：受管理文件根、句柄级路径约束、列表/文本/下载；建立路径穿越和符号链接测试。
- P06.2：流式暂存上传、提交、编辑备份、目录创建、复制/移动/删除、权限/所有者受限修改、版本冲突与配额。
- P06.3：以 upanel-shell 启动 PTY、WebSocket、再认证、Origin、连接许可、窗口调整和超时；审计会话元数据。

验收：../../etc/shadow、绝对路径、符号链接替换和 magic link 无法逃逸；2 GiB 上传不随文件大小增长内存；断流/磁盘满清理暂存；版本冲突不覆盖他人修改；终端 id 命令显示 upanel-shell，无法 sudo、读取代理密钥或写系统目录。

文件 UI 不能显示“可管理整个服务器所有目录”。终端不是 root 终端，这个边界需要在页面和使用说明中明确。

### 9. P07：分区、格式化和挂载

这是高风险阶段，逐个子任务实现、测试，禁止一次性生成整个磁盘模块后直接宣布完成。

- P07.1：实现存储动作预检、可靠身份、底层盘锁、系统设备/挂载/swap/holders/依赖保护；此子任务只验证拒绝逻辑，不执行破坏性动作。
- P07.2：在隔离 VM 的空白虚拟盘上初始化 GPT、创建分区、删除未使用分区；固定模板、对齐、udev 等待、真实结果验证。
- P07.3：ext4/XFS 格式化、标签校验、确认短语、禁止取消、执行前二次身份检查；测试错误退出和进程中断。
- P07.4：导入卷、挂载/卸载、UUID 校验、fstab 最小修改、原子替换/备份/回滚、开机自动挂载、繁忙占用列表。
- P07.5：磁盘页面完整交互及系统外部独立检查；更新存储恢复文档。

必须通过以下真实 VM 验收：

1. 新虚拟盘 → GPT → 分区 → ext4 → 导入 → 挂载 → 写入文件 → 卸载 → 重挂载，文件一致。
2. 独立测试 XFS 分区，并验证 XFS fsck pass 为 0。
3. 重启 VM 后同 UUID 自动挂载到指定路径；代理外的主机进程也能看到挂载。
4. 系统根盘、根盘其他分区、活动 swap、LVM/RAID 成员、已挂载分区、繁忙卷全部被拒绝。
5. 预检后换盘/拓扑变化、相同 /dev 名称指向另一设备时拒绝；跨分区操作也持有整盘锁。
6. 外部编辑 fstab 后，旧版本任务不覆盖；候选校验失败保留原文件。
7. 设备移除、代理重启、错误退出、任务重复提交没有虚假成功和自动再次格式化。
8. 不存在强制卸载、忽略保护或自由命令字符串的逃生通道。

缺少可丢弃 VM 数据盘时，本阶段可完成代码与单元测试，但状态必须为 blocked，磁盘写功能不得进入已验收发布包。

### 10. P08：SMB 共享与挂载依赖

- P08.1：Samba 能力检测、受管理账号、密码秘密通道、POSIX ACL、共享定义和配置版本。
- P08.2：最小 include 接入、testparm、原子更新、重载、共享连接状态；已有配置保留。
- P08.3：共享与 volume_id 绑定、启用检查、启动依赖和运行时脱盘处理；卸载/格式化依赖阻断。
- P08.4：共享 UI、用户权限和客户端连接说明。

验收：从另一台测试客户端连接，read 用户不能写、write 用户能写、未授权/guest 被拒绝；Samba 密码不出现在命令行和日志；卷未挂载时不会共享系统盘空目录；运行时虚拟盘离线触发共享撤销；重启顺序正确；已有第三方共享无未经确认的变更。

必须验证真实协议访问，不能只用 testparm 通过当作共享可用。

### 11. P09：现有 Web 应用与 Docker 接入

- P09.1：应用/分组/收藏元数据，link/systemd/docker/compose 判别模型，公共入口与内部目标分离。
- P09.2：本机端口、服务、容器发现；管理员确认候选；普通应用健康检查和 SSRF 限制。
- P09.3：Docker 只读查询、日志和现有受支持容器启停；不接管危险容器，不执行外部 Compose YAML。
- P09.4：关联 systemd/容器的启停、日志、状态和依赖卷；受管理/未接管依赖保护明确显示。
- P09.5：应用卡片、搜索、分组、一键打开、状态区分和常用入口首页集成。

验收：link 没有启停按钮；容器 running 但 HTTP 500 时明确异常；容器重建 ID 变化后旧绑定要求重新确认；入口在远程浏览器打开服务器地址；不可探测云元数据/其他主机；卷离线禁止启动受管理依赖应用。

外部应用不能保证启动依赖时必须标记未接管。不要为了达到“支持已有项目”而开放任意配置执行。

### 12. P10：反向代理、HTTPS 与 0.1 发布验收

- P10.1：Nginx 发现、配置冲突、单域名单 upstream、WebSocket、请求限制、访问策略。
- P10.2：配置生成/原子写入/nginx -t/reload/失败恢复；不影响管理端 9443。
- P10.3：Certbot staging 签发流程、正式签发明确选择、手动证书导入、到期信息、私钥保护；建立续期动作，timer 在 P12 接入。
- P10.4：0.1 安装包、校验和、systemd 单元、初始用户、HTTPS/SSH 隧道使用说明、基本恢复说明；执行完整核心场景。

验收：域名正常访问指定应用，WebSocket 不断流；同域名/端口冲突拒绝；错误 Nginx 配置不会破坏已有站点；代理出错面板仍可登录；私钥不能通过任何文件/API/日志读取；证书测试使用 staging，不无上限调用正式 ACME。

0.1 发布门槛：P00—P10 全部 passed，验收矩阵中属于 P00—P10 的条目通过（跨 P15 的项目先验证 P09 支持范围），22.04 和 24.04 都有安装/重启/核心运维证据，README 明确尚未实现的 1.0 功能。不能仅在 Windows 上演示后发布为已支持 Ubuntu。

### 13. P11：软件更新与电源

- P11.1：APT 包列表/候选版本/安全更新/重启标志读取，包名和版本展示。
- P11.2：refresh/install/remove/upgrade 的模拟、保护名单、锁、非交互配置策略、不可取消阶段和结果校验。
- P11.3：重启/关机预检、再认证、倒计时与连接状态；重启 boot_id 确认。

验收：外部 apt 正在运行时不删锁、不并发；拒绝卸载 Essential/SSH/网络/面板关键依赖；APT 中断不自动强制修复；升级失败显示真实错误；关键任务运行时拒绝电源操作；关机界面不伪称已验证。

### 14. P12：备份、恢复与计划任务

- P12.1：restic 本地/SFTP 仓库、秘密引用、SFTP 主机密钥验证、仓库初始化和容量检测。
- P12.2：策略、运行、快照列表、文件一致性说明、停受管理应用备份、面板 SQLite 在线备份。
- P12.3：恢复到新目录、覆盖恢复预检、保留策略的显式清理、恢复验证及失败处理。
- P12.4：受管理 timer、时区/错过执行策略/不重叠/超时；内置动作复用任务框架；非 root 用户脚本。
- P12.5：证书续期 timer 接入，备份与定时任务 UI。

验收：备份后实际恢复并校验文件哈希；错误密码/主机密钥拒绝；存储满/网络断开标失败且保留原数据；恢复不会覆盖范围外文件；同计划重复触发仅运行一次；系统时区不同仍按指定时间执行；脚本不能升级为 root；清理只作用于指定仓库且遵守保留策略。

### 15. P13：历史监控和告警

- P13.1：原始/分钟/小时数据、聚合、保留和清理，历史查询点数限制。
- P13.2：告警规则、持续时间、恢复阈值、空间双阈值、去重、冷却、维护静默。
- P13.3：面板通知、SMTP、HTTPS webhook、秘密管理、显式测试发送和失败重试上限。
- P13.4：历史图表、告警列表、确认/恢复显示，采集陈旧与缺失曲线。

验收：模拟时间验证保留/聚合，正常采集不重复写入；持续超限才触发，恢复只发一次，未变化不刷屏；丢失数据不是 0；通知测试使用本地接收器或明确授权的测试目标，不擅自向第三方联系人发送。

### 16. P14：网络、NFS、账号和 TOTP

这些子任务相互独立，但都在已有安全、任务和配置框架内实现，不要合并成一个万能系统设置接口。

- P14.1：网络/路由/监听端口/UFW 只读；检测受支持配置和第三方管理冲突。
- P14.2：单物理网卡 DHCP/静态 IPv4 受限修改，独立 systemd 回滚 timer，候选地址确认和恢复流程。
- P14.3：UFW 规则与预检、保留管理通路、独立回滚、实际规则验证。
- P14.4：NFS 受管理导出、网段/root_squash、卷依赖、真实客户端读写验收。
- P14.5：受管理 Linux 账号、SSH 公钥、受限 sudo 模板；禁止修改 root/系统账号和面板身份提权。
- P14.6：TOTP、一次性恢复码、启用确认、CLI 本机恢复与会话失效。

验收：在 VM 中故意配置不可达地址/规则，断开浏览器并停止 Web 进程，120 秒后仍能自动恢复；确认旧地址不能绕过新地址验证；复杂 netplan 返回 unsupported；NFS root 被 squash；visudo 失败保留旧规则；TOTP 未确认不启用，恢复码只能使用一次。

如果无法真实验证独立网络回滚，网络写操作必须保持禁用，1.0 不可声称该项已完成。

### 17. P15：受约束应用部署与更新

- P15.1：受限 DeploymentDefinition、镜像/端口/资源/目录校验、秘密环境变量、Compose 配置渲染。
- P15.2：systemd 包装、挂载依赖、容器 restart=no、部署/停止/删除保留数据。
- P15.3：记录镜像 digest、可选备份、更新/健康检查、失败提示和配置/镜像回切边界。
- P15.4：部署和更新 UI，明确禁止原始 Compose YAML、危险宿主挂载和特权选项。

验收：恶意挂载 /、docker.sock、devices、privileged、host network 等全部无法表达或被拒绝；应用所需卷缺失时 VM 重启也不会绕过保护启动容器；正常镜像可部署/升级；健康失败显示失败，业务目录保留；不能把存在数据库迁移的更新宣传为无损回滚。

### 18. P16：完整单机版集成与发布

- P16.1：统一能力探测、安装/升级/卸载、两数据库和密钥备份、失败可重入；兼容已有软件。
- P16.2：22.04/24.04 双平台端到端验收，真实资源使用和延迟测量，日志/磁盘/连接容量限制。
- P16.3：认证/权限/路径/命令/SSRF/设备身份/配置变更专项复核；修复阻断性问题。
- P16.4：用户手册、管理员恢复手册、支持矩阵、已知限制、发布说明、校验和和完整证据索引。

验收：全新安装、重启、升级、失败升级恢复、默认保留数据的卸载通过；从浏览器完成主要用户流程；没有未处理的危险操作漏洞、不可恢复的管理断联问题或伪成功状态。

1.0 可以明确保留 PLAN.md 排除项，但不能把本版本承诺的未实现功能隐藏后仍宣布完整交付。若项目负责人主动缩小范围，需要同步版本定义和说明。

### 19. 核心验收矩阵

| 编号 | 场景 | 预期结果 | 最晚阶段 |
|---|---|---|---|
| SEC-01 | 未登录/伪造 Cookie/过期会话访问 | 401，无内部信息泄露 | P02 |
| SEC-02 | viewer 请求任何系统写动作或读取原始日志/秘密 | 403 | P02 |
| SEC-03 | 跨站提交/错误 Origin/缺 CSRF | 拒绝，正常同源不受影响 | P02 |
| SEC-04 | 其他 UID 访问代理或用户命令字符串输入 | 拒绝 | P02 |
| JOB-01 | 同一幂等请求提交十次、提交后断网 | 一次执行，可找回原任务 | P03 |
| JOB-02 | 预检到期/换参数/目标变化 | 拒绝，不执行 | P03/P07 |
| JOB-03 | Web/代理崩溃 | 对账或 interrupted，无自动重放破坏动作 | P03 |
| DISK-01 | 未挂载盘/SMART 不支持/睡眠盘 | 全盘发现、状态正确、不强制唤醒 | P04 |
| DISK-02 | 根盘在 LVM/RAID 下 | 递归保护，不能破坏任何底层盘 | P04/P07 |
| DISK-03 | GPT→分区→格式化→挂载→读写→重启 | 真实成功，UUID 和数据一致 | P07 |
| DISK-04 | 挂载/swap/holders/繁忙/依赖中格式化或卸载 | 拒绝并列明占用 | P07 |
| DISK-05 | 预检后磁盘替换、路径复用、身份冲突 | 拒绝 | P07 |
| DISK-06 | fstab 并发修改/错误候选/代理私有命名空间 | 不覆盖别人配置，挂载主机可见 | P07 |
| FILE-01 | 路径穿越、symlink 竞态、magic link | 不越界 | P06 |
| FILE-02 | 超大上传/断流/磁盘满/并发编辑 | 流式有界，清理暂存，不覆盖新版本 | P06 |
| TERM-01 | 终端获取身份和尝试提权 | 仅 upanel-shell，无 sudo | P06 |
| SHARE-01 | 未授权/只读/读写 SMB 客户端 | 实际权限正确 | P08 |
| SHARE-02 | 卷未挂载/启动顺序/运行时脱盘 | 不共享系统盘空目录 | P08 |
| APP-01 | 容器运行但 HTTP 失败/内部 localhost 入口 | 状态分开，远程入口正确 | P09 |
| APP-02 | metadata 地址/外部主机/重定向探测 | 拒绝 | P09 |
| APP-03 | 卷丢失后启动或 VM 重启 | 受管理应用不启动，不误写系统盘 | P09/P15 |
| PROXY-01 | 错误配置/重名/现有站点 | 校验拒绝或恢复，不覆盖他人文件 | P10 |
| PROXY-02 | WebSocket、证书、私钥访问 | 正常代理，私钥不泄露 | P10 |
| PKG-01 | 外部 APT 锁/删除关键包/中断 | 不删锁、不误删、明确失败 | P11 |
| BACKUP-01 | 备份后恢复、错误密码、网络断开 | 校验一致，失败不假成功 | P12 |
| TIMER-01 | 时区变化/错过执行/重叠 | 明确策略、无重复危险执行 | P12 |
| ALERT-01 | 持续超限→恢复→数据缺失 | 去重、恢复正确、缺失不当零 | P13 |
| NET-01 | 改错 IP/UFW 后浏览器和 Web 都离线 | 独立回滚恢复访问 | P14 |
| NFS-01 | 不允许网段与客户端 root | 拒绝访问/root_squash 生效 | P14 |
| DEPLOY-01 | 恶意容器参数/宿主挂载/卷丢失 | 拒绝且无宿主权限逃逸路径 | P15 |
| RELEASE-01 | 安装→重启→升级→恢复→卸载 | 服务正确、业务数据保留 | P16 |

### 20. 证据与发布记录

每次验收保存到 docs/evidence/<阶段>/<日期>/，包含环境版本、执行命令、退出码、脱敏输出和必要截图。大文件、测试磁盘镜像、秘密和虚拟机内存转储不得提交仓库。

测试报告必须区分：unit、mock integration、Linux VM integration、browser E2E、人工观察。没有工具权限或环境的测试写 NOT RUN，并说明如何补做；不能写“理论上通过”。

UI 截图只能证明展示效果。磁盘、共享、网络、备份和容器依赖的完成证据必须包含系统状态或真实客户端结果。


---

来源：docs/DEEPSEEK_HANDOFF.md

## 交给 DeepSeek 的执行说明

这份说明用于约束开发模型的实施过程，不表示对某个模型能力或版本作出保证。模型仍可能写错代码，尤其是分区、格式化、提权、网络变更和恢复流程；必须用契约与真实验收约束结果。

### 1. 推荐使用方法

1. 将整个 uPanel 目录提供给模型，确保它能读取 docs 下的全部规格。
2. 首轮只执行 P00，不让模型一次生成全部项目。
3. 后续一次执行一个子任务，例如 P02.1；通过阶段验收后才进入下一阶段。
4. 每次新对话先读取 PROGRESS.md，再读取当前阶段相关契约和现有代码。
5. 让模型把代码与进度保存在仓库中，不依赖聊天记忆。
6. 进入 P07/P14 等真实系统验收时使用独立 VM；不要把开发宿主或日常使用服务器作为破坏性测试目标。

如果对话平台没有文件读取能力，上传全部五份 docs 文档。上下文不足时至少提供完整 PLAN.md、当前模块 CONTRACTS.md 章节、当前 STAGES.md 阶段、HANDOFF 和最新 PROGRESS；不能省略全局权限、任务和设备保护规则。

### 2. 首轮提示词（可直接复制）

    你负责根据仓库规格开发 uPanel，这是一个 Ubuntu 单机 Web 管理面板。
    你必须先依次完整读取：
    1. docs/PLAN.md
    2. docs/CONTRACTS.md
    3. docs/STAGES.md
    4. docs/DEEPSEEK_HANDOFF.md
    5. docs/PROGRESS.md

    产品范围、技术栈、接口、字段、权限和实施顺序均以这些文件为准。
    不要自行更换技术栈，不要新增计划外功能，不要一次性生成整个系统。
    本轮只执行 P00。先用不超过十条内容复述关键约束，然后直接开始完成 P00。
    不需要再次询问已在规格中明确的普通实现选择。
    如果发现实质冲突，明确指出文件和段落，记录阻塞，不得自行降低安全要求。

    完成后更新 docs/PROGRESS.md，列出：完成子任务、修改文件、真实执行的检查、
    实际测试结果、未验证项目、阻塞与下一子任务。没有执行的测试必须标明未执行。
    禁止宣称 mock 通过等于 Ubuntu 真实系统功能可用。
    不得操作开发宿主或用户真实磁盘；破坏性验收仅限独立测试 VM 的可丢弃数据盘。

### 3. 每轮续接提示词

    继续开发 uPanel。
    先读取 docs/PROGRESS.md 和现有代码，再读取当前任务相关的 PLAN、CONTRACTS、STAGES。
    本轮只完成【填写子任务编号，例如 P03.2】及其必要测试。
    已实现的部分以代码和测试证据为准，不要重新生成整个项目或覆盖已有成果。
    先核对前置阶段是否通过；如果有相关未解决失败，先修复，不能跳到新功能。
    先更新本轮接口契约/迁移，再实现；公开 API 与 CONTRACTS.md 保持一致。
    完成后运行与改动相符的检查，更新 PROGRESS.md 并报告结果。
    禁止扩大范围、临时关闭保护、返回伪成功或把未执行测试写成通过。

### 4. 独立复核提示词

当关键阶段完成后，可以在新的对话中使用以下内容复核。复核仍可能遗漏问题，真实验收不能省略。

    请只复核 uPanel 的【填写阶段/模块】，暂不增加新功能。
    阅读 PLAN、CONTRACTS、STAGES 对应章节，检查实际代码是否满足要求。
    重点查：权限与 CSRF、命令和路径边界、预检与执行间状态变化、幂等与崩溃恢复、
    外部配置并发修改、设备身份、系统盘与依赖保护、失败是否会被显示成成功。
    先给出有代码位置与触发条件的具体问题，不要只复述通用安全建议。
    没有发现问题不等于已完成 Linux 验收；明确哪些检查实际运行过。
    更新进度中的复核记录，发现阻断性问题时取消对应阶段的通过状态。

### 5. 开发模型必须遵守的规则

#### 5.1 范围和工程组织

- 不把计划扩展为多主机控制台、应用商店、Kubernetes 或另一套 NAS 系统。
- 不把后端换成其他语言，不在前端直接调用系统工具，不让 Web 服务以 root 常驻。
- 不让 upanel-web 加入 docker 组，不加 NOPASSWD: ALL，不允许原始 root shell/API。
- 不引入自由 shell 命令、任意 argv、任意 root 文件写入、任意 Compose YAML。
- 不为尚未实现模块填充假数据后当成正式功能展示。只有显式 mock 模式允许模拟数据。
- 不使用单个巨型 handler/Vue 组件实现所有功能，复用类型和清晰模块边界。

#### 5.2 实现顺序

- 先契约和状态，再后端和代理，再前端，再验收；不能只做漂亮页面而空置危险操作逻辑。
- 共用的认证、任务、预检、幂等、日志和锁先做完，不为每个页面复制一套。
- 每个阶段严格依赖之前阶段。当前阶段的失败不得以“后面再修”越过发布门槛。
- 采用最小可验收提交；如果使用 Git，每个已通过子任务一个清晰提交，不提交秘密或 VM 镜像。
- 没有明确发布授权时，不自动上传安装包、推送远端分支或部署到真实服务器。

#### 5.3 代码与测试真实性

- 不吞掉错误后返回 success；外部命令退出码、结果读取和配置验证都要处理。
- 不把所有异常改成 HTTP 200；不把 unknown/unsupported 当健康状态。
- 不用字符串拼接调用 shell，不使用用户输入选择可执行文件。
- 不用字符串前缀判断文件路径安全，不只凭 /dev/sdX 判断磁盘身份。
- 不通过删除测试、跳过断言、关闭类型检查、添加全局 any 来“通过构建”。
- 不编造 API、依赖版本、命令参数、测试输出或执行证据；不确定时查官方文档或本机帮助。
- 所有 destructive integration tests 只能在隔离 VM 的指定虚拟盘执行。

#### 5.4 阻塞处理

可自主决定：文件拆分、函数名、局部组件布局、内部帮助函数、合理测试夹具。

必须记录并暂停相关依赖工作：规格冲突、无法验证设备身份、权限边界必须突破、没有 VM 却需要磁盘验收、没有独立网络回滚机制、需要引入计划外功能。

可以继续完成不依赖阻塞项的同阶段工作，但不能把阶段标为 passed。描述缺少什么、已经完成什么、如何恢复；不要笼统写“环境问题”。

### 6. 每轮完成报告模板

    当前任务：Pxx.n
    状态：passed / in_progress / blocked
    完成内容：
    修改文件：
    契约/迁移变化：
    实际执行的检查与结果：
    证据路径：
    未验证内容与原因：
    已知限制/阻塞：
    下一步：

报告应简短准确，完整事实保存到 PROGRESS.md。不要仅写“已经完成，请自行测试”。

### 7. 项目负责人验收建议

你不必一次读懂全部代码，但每阶段至少看实际演示和证据：按钮是否真的改变系统状态、失败是否如实显示、重启是否保留配置、权限是否真正生效。

P07 的磁盘保护、P14 的断网回滚、P16 的安装升级恢复最好安排一次具备 Linux 运维经验的人工复核。开发模型写出了相关代码，不能替代这些高风险流程的验收。

若模型不断跑偏，应缩小到一个明确失败案例，例如“修复 DISK-05，先写换盘后的拒绝测试，再修实现”，不要让它同时重构架构、换依赖和增加页面。


---

来源：docs/PROGRESS.md

## uPanel 开发进度账本

最后更新：2026-10-20。

当前事实：开发规格包已编写；尚无业务代码，尚未执行产品构建、产品测试或 Ubuntu VM 验收。

### 1. 当前任务

- 当前阶段：P00 尚未开始。
- 下一子任务：P00.1，读取规格并建立实施决策记录。
- 当前阻塞：未做环境探测，暂无已确认阻塞。Ubuntu VM 尚未准备，不得默认已具备。
- 当前版本：未发布。

### 2. 状态定义

- not_started：未开始。
- in_progress：正在实施，验收未全部通过。
- blocked：缺少条件或存在必须先解决的问题。
- passed：该阶段全部子任务与验收完成，证据可查。

“代码已写完但未跑真实系统测试”不是 passed。发现阻断性回归后必须撤销相关 passed 状态。

### 3. 阶段状态

| 阶段 | 内容 | 状态 | 证据/备注 |
|---|---|---|---|
| P00 | 基线和验收环境 | not_started | — |
| P01 | 最小可运行骨架 | not_started | — |
| P02 | 认证、权限、审计、代理 | not_started | — |
| P03 | 预检、任务、幂等、对账 | not_started | — |
| P04 | 首页和存储只读 | not_started | — |
| P05 | 服务、进程、日志 | not_started | — |
| P06 | 文件和受限终端 | not_started | — |
| P07 | 分区、格式化、挂载 | not_started | 需独立 VM 数据盘 |
| P08 | SMB 与卷依赖 | not_started | 需真实客户端 |
| P09 | 现有 Web 应用与 Docker | not_started | — |
| P10 | 反向代理、HTTPS、0.1 发布 | not_started | — |
| P11 | 软件更新与电源 | not_started | — |
| P12 | 备份恢复与计划任务 | not_started | 需恢复校验 |
| P13 | 历史监控和告警 | not_started | — |
| P14 | 网络、NFS、账号、TOTP | not_started | 需独立网络回滚验收 |
| P15 | 应用部署与更新 | not_started | — |
| P16 | 集成、恢复与 1.0 发布 | not_started | — |

### 4. 已冻结决策

- 单台 Ubuntu；先 amd64、22.04/24.04 LTS。
- Go + Vue 3 + TypeScript + Naive UI + SQLite。
- 非 root Web、root 受限代理、独立非 root 终端账号。
- 所有系统写操作采用预检和持久任务，不提供万能命令执行器。
- 所有物理盘分层监控；系统盘和不支持的复杂设备禁止破坏性操作。
- 0.1 为 P00—P10，1.0 为 P00—P16；高级存储和多服务器后续考虑。
- 具体工具链和依赖补丁版本在 P00 核实并锁定，不在此虚构。

### 5. 子任务记录模板

每次追加一条，不覆盖历史事实：

    日期：
    子任务：Pxx.n
    状态：
    修改文件：
    完成行为：
    API/数据库变化：
    测试环境：
    实际检查命令、退出码和结果：
    证据路径：
    未运行检查：
    已知问题/阻塞：
    下一步：

### 6. 未决问题与范围变更

当前无已批准的范围变更。

后续按“日期、问题/变更、依据、影响文件、决策人、处理结果”记录。不要把模型个人偏好当作项目负责人批准。

### 7. 验收证据索引

当前无产品验收证据。文档检查不算产品功能测试。

后续按 STAGES.md 的验收编号登记 passed / failed / NOT RUN，并链接证据文件。

### 8. 已知限制

- 当前只有计划，尚未实现任何管理功能。
- 尚未核实生产依赖版本、Linux 命令兼容性或部署环境。
- 所有高风险能力必须按计划在独立 VM 中实现并验证，不能从文档存在推断其安全性。
