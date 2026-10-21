# uPanel 接口与数据契约

版本：1.0；配套 PLAN.md 和 STAGES.md。本文中“必须”是实现约束，不允许前后端各自发明字段。

## 1. 命名、时间、数值与响应

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

## 2. 权限与会话

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

## 3. 系统与能力发现

| 方法与路径 | 输出 | 权限 |
|---|---|---|
| GET /health/live | 最小存活状态，不返回内部路径/版本细节 | 匿名 |
| GET /meta | version、mode、capabilities、server_time、timezone_display_policy | V |
| GET /system/summary | OS、内核、uptime、boot_id、CPU/内存/负载、告警摘要、采集时间 | V |
| GET /system/metrics?scope=&from=&to=&step= | 明确 scope 的时序样本；限制时间范围/点数 | V |
| GET /audit | 审计分页和筛选 | admin |

capabilities 为列表，每项包含 id、state、reason、required_packages、supported_actions。state 为 ready、missing_dependency、unsupported、disabled、mock。模式为 production 或 mock，不允许 silent fallback。mock 服务绑定 127.0.0.1:18881，与真实代理完全隔离；页面持续显示“演示数据，未操作服务器”。

## 4. 预检、执行和幂等

所有系统写动作通过以下两步提交。应用元数据等纯数据库 CRUD 不走代理，但仍有权限、版本和审计。

### 4.1 预检

POST /operations/preflight

    {"action":"storage.filesystem.format","params":{"device_id":"dev_...","filesystem":"ext4","label":"data"}}

返回：

    {"data":{"preflight_id":"...","action":"storage.filesystem.format","normalized_params":{"device_id":"dev_...","filesystem":"ext4","label":"data"},"target":{"display_name":"...","identity_fingerprint":"..."},"expires_at":"2026-10-20T08:32:00Z","allowed":true,"blockers":[],"warnings":[],"impacts":[{"kind":"data_loss","description":"该分区已有数据将被清除"}],"requires_reauth":true,"confirmation_phrase":"FORMAT ABC123 P1","cancellable":false,"locks":["disk:dev_..."]},"request_id":"..."}

预检有效期 120 秒，绑定用户、会话、动作、规范化参数和目标状态版本。危险确认短语由服务端生成，不能由前端决定。GPT 初始化、格式化、删除分区、递归删除目录、覆盖恢复和仓库清理必须要求确认短语；其他动作按注册策略要求普通确认或短语。预检本身不写磁盘、不停止服务、不安装软件。

### 4.2 创建任务

POST /jobs，必须携带 Idempotency-Key（客户端生成 UUID，一次意图重试时复用）。

    {"preflight_id":"...","confirmation_phrase":"FORMAT ABC123 P1"}

不再接受 action 或 params，避免提交时替换目标。服务端按预检恢复参数。预检为一次性，用户权限/会话变化则失效。相同用户+接口+幂等键+相同请求返回原 job；相同键但请求不同返回 409。

返回 202：

    {"data":{"job_id":"...","status":"queued","events_url":"/api/v1/jobs/.../events"},"request_id":"..."}

数据库先建立 durable job 和稳定 operation_id，随后向代理提交。网络超时后按 operation_id 查询，不再次生成新 operation_id 执行。代理也保存 operation_id 唯一约束和请求哈希。重复点击、HTTP 重试、Web 重启不应导致重复格式化或重复修改分区。

预检到执行间资源变化，代理即使已接受任务也必须在真正执行前检查，失败使用 PREFLIGHT_STALE 或 DEVICE_IDENTITY_CHANGED。锁冲突可在 30 秒内排队；超时返回 RESOURCE_BUSY，不能无限占用 worker。预检在有效期内被接受后可排队，但状态快照仍需执行前复核。

### 4.3 任务 API 与状态

| 方法与路径 | 行为 | 权限 |
|---|---|---|
| GET /jobs | 任务摘要列表；viewer 不含原始参数/日志 | V |
| GET /jobs/:id | 状态、步骤、百分比或 null、结果、错误、是否可取消 | V，字段按角色裁剪 |
| GET /jobs/:id/events | SSE 实时事件，支持 Last-Event-ID | admin |
| GET /jobs/:id/logs?cursor=&limit= | 脱敏日志分页 | admin |
| POST /jobs/:id/cancel | 请求安全取消；不可取消返回 409 | admin |

状态机：queued → running → succeeded / failed / canceled / interrupted；queued 也可直接 canceled / failed。终态不可再次回到 running，重新操作必须创建新任务和新预检。cancellation_requested 是独立布尔字段，不是成功取消。

progress_percent 不知道时为 null，不得用假的定时器从 0 涨到 100。step_code 采用实现登记的稳定枚举，message 为可读描述。

result 至少包括 outcome（verified、accepted、rolled_back、partial、unknown）、verified_at、observed_state、recovery_hint。普通写任务只有验证真实结果后才能 succeeded。电源类动作允许 succeeded + accepted，界面必须表述为“请求已受理”；不能据此声称关机已验证。没有结果的任务（尚未执行、已失败、已取消）`result` 是空对象 `{}`——错误写在 `error`（`code`、`message`、`recovery_hint`），不塞进 `result`，也不留一份上一次的 `observed_state` 冒充结果。

格式化、分区写入、APT 写事务开始后不可取消。日志跟踪、未开始的任务、支持安全中断的备份可以取消。代理异常退出后未决任务标为 interrupted；先查询实际状态，不自动重新执行。

SSE 使用持久递增 seq 作为 id，事件类型为 status、progress、log、gap；每 15 秒发送注释心跳。重连不重执行任务。历史已清理时发 gap，客户端重新拉取状态。限制每个用户连接数、慢客户端缓冲和日志容量。

## 5. 动作注册表

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

## 6. 存储 API 与对象

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

### 6.1 附加字段（P04.2 起）

在上面 Device 必需字段之外，P04.2 增加三个字段。它们是对必需字段的补充，不是替代：必需字段一个都不能少，新增字段对老客户端只是多出来的键。

| 字段 | 类型 | 语义 |
|---|---|---|
| protection | 对象 | system_disk 布尔；reasons 人类可读理由（去重、固定排序）；sources 触发保护的设备 id |
| holders | 字符串数组 | 直接依赖这台设备的设备 id（sysfs holders：dm/md 消费它） |
| swap | 对象或 null | 活动交换区来源；type 为 partition/file，含 size_bytes、used_bytes、priority |

protection 的定义方式：从 `/proc/self/mountinfo` 与 `/proc/swaps` 出发，找出承载 `/`、`/boot`、EFI 系统分区、面板数据与配置目录、以及活动交换区的设备，再沿 partition_of/depends_on **递归向上**标记所有底层盘。因此“整盘被标为系统盘”与“它的某个分区承载 /”必须同时成立，二者不能互相矛盾。判定失败时不能默认 false 放行写入，必须报错或标 unknown。

capabilities 在 P04.2 恒为空数组：动作注册表（第 5 节）尚未实现，返回空数组表示“这里不提供动作入口”，不得返回伪 available。前端必须把空 capabilities 渲染成“无可执行动作”，而不是“全部可用”。

partition_table 的 partitions 来自 sysfs（内核当前认定的分区），盘上读到的 MBR/GPT 只用来补充类型与 free_regions；两者不一致时保留内核结果并给出 warning，不静默采信任何一方。partition_table.source 区分 device（读了盘）、kernel（只用 sysfs，类型未知）、skipped（盘在休眠或读失败，未去打扰它）。盘在休眠时不得为探测分区表而唤醒。

filesystem.type/uuid/label 在未挂载时为 null，本阶段不调用 blkid，不靠猜。

### 6.2 附加字段（P04.3 起）

P04.3 再增加两个字段，同样是补充而不是替代：Device 的必需字段一个都不能少，老客户端看到多出来的键不影响渲染。

| 字段 | 类型 | 语义 |
|---|---|---|
| io | 对象或 null | 这台设备**自己**在 `/proc/diskstats` 里那一行的计数与速率；读不到 `/proc/diskstats` 时为 null |
| filesystem.mounts[].capacity_error | 字符串，无错时不出现 | capacity 为 null 的原因（人可读），例如查询超时与“多久之后才会重试” |

io 的字段：

| 字段 | 类型 | 说明 |
|---|---|---|
| sampled_at | RFC3339 UTC | 这次读计数的时刻 |
| read_bytes_total / write_bytes_total | 非负十进制字符串 | 开机以来的累计字节数（扇区 × 512） |
| reads_total / writes_total | 非负十进制字符串 | 开机以来的累计完成次数 |
| rate | 对象或 null | 相邻两次采样之间的速率；null 表示本次没有可用窗口 |

rate 的字段：interval_ms（整数）、read_bytes_per_second / write_bytes_per_second（非负十进制字符串）、
read_iops / write_iops（number，保留两位小数）、read_latency_ms / write_latency_ms（number 或 null）。

io 的硬规则：

- **只报自己那一行的计数，从不相加。** 分区的 I/O 已经包含在整盘里，dm/LVM 的 I/O 又与底层盘重复；把 nodes 的 io 加总就是双重计数（PLAN 第 8 节）。
- rate 为 **null** 的三种情况：首次采样（没有上一个窗口）；任一计数变小（内核重启、换盘、32 位回绕、同名新设备）；采样窗口为 0/负或超过采集间隔的 3 倍（进程被挂起、机器休眠）。这些都是“算不出来”，不能填 0。
- rate 为 **0** 是真结论：这一窗口确实没有任何 I/O。平均延迟只在对应完成次数 > 0 时给出，否则为 null——“没有样本”不等于“等了 0 毫秒”。
- 扇区固定按 **512 字节** 换算（内核在这个接口里不用设备逻辑扇区大小），拿 `logical_sector_bytes` 去乘会算错 4K 盘。
- io **不参与** `snapshot_version`：版本只由拓扑内容决定（P07 预检用它判断状态是否变化），速率每 5 秒都在变，放进去会让预检永远过期。

health 在 P04.3 的取值与来源：

- 只有拿到明确的通过结论才是 `healthy`（`smart_status.passed=true`，或 NVMe 健康日志存在且没有任何 Critical Warning 位）。
- 物理盘读不到 SMART 时是 `unknown` 并写清原因（未安装 smartctl、超时、打不开设备、输出无法解析）。
- `details` 要带上 smartctl 自己的诊断原话（它的 `smartctl.messages`，优先 `severity=error` 的那条）：
  “Unknown USB bridge”这类信息只有原话能说清是桥不被识别，而不是我们的参数写错了。
- USB 桥接盘同理：桥不被 smartctl 识别时是 `unknown`（真实反例见 `tests/fixtures/smart/upanel-smart-usb-unknown-bridge.json`），
  既不是 `unsupported`，更不是 `healthy`。
- 分区、loop、rom 恒为 `unsupported` 并带 details：它们没有独立的 SMART，这是“没有这个功能”，不是“功能坏了”，也不是 `unknown`。
- `health.source` 取 `sysfs`（只看 sysfs 就判断为休眠，未调用 smartctl）、`smartctl`、`none`（本机没有可用采集手段）。
- 采集节奏 15 分钟，可配置 5—60 分钟；**不为读健康而唤醒磁盘**：先读 sysfs 的电源状态，休眠即跳过，非休眠时对非 NVMe 设备再加 `-n standby` 让 smartctl 在最后一刻放弃。
- smartctl 的退出状态是**位掩码**（1 命令行、2 打开失败/低功耗、4 SMART 命令失败、8 DISK FAILING、16 预失效当前、32 预失效历史、64 错误日志、128 自检日志）。失效与预失效为 `critical`，历史位与日志位为 `warning`，命令失败与打开失败为 `unknown`。

网络挂载在快照里的位置（P04.3 起）：`Topology` 增加 `network_mounts` 数组，与 `nodes`/`edges` 平级。

| 字段 | 类型 | 语义 |
|---|---|---|
| path | 字符串 | 挂载点绝对路径。 |
| source | 字符串 | mountinfo 里的挂载来源（如 `10.0.0.5:/export`、`//host/share`）。 |
| fstype | 字符串 | 文件系统类型，判定隔离通道的唯一依据。 |
| read_only | 布尔 | 来自挂载选项里的 `ro`。 |
| reachable | 布尔或 null | `true` 读到容量；`false` 查询明确失败；`null` 还没查过。 |
| capacity | 对象或 null | 与设备挂载点的 capacity 同结构、同语义（读不到就是 null）。 |
| capacity_error | 字符串，无错时不出现 | capacity 为什么是 null。 |

- 只收**没有块设备**的网络挂载：NFS/CIFS 的 major:minor 是伪设备号，`/sys/dev/block` 里没有对应设备，因此它们不可能挂到某台盘下面。tmpfs、proc、cgroup 同样没有块设备但与磁盘管理无关，**不**进拓扑。
- 路径去重、按路径排序；同一次快照里同一路径只出现一次。
- 身份字段（path、source、fstype、read_only）参与 `snapshot_version`；`reachable`/`capacity`/`capacity_error` 是观测值，不参与。
- 它们与设备挂载点共用同一份 30 秒容量缓存、同一条隔离通道、同一个 5 分钟隔离期。
- `reachable` 为 `false` 表示“查询明确失败”，不能用它表示“还没查过”——那会让刚挂上的盘显示成断开。

网络挂载的容量查询（不可达时不得影响其它数据）：

- 判断依据是 `fstype`（nfs/nfs4/cifs/smb3/smbfs/fuse.sshfs/9p/ceph/glusterfs/afs/davfs），不是挂载点路径写法。
- 网络挂载的 statfs 在**隔离子进程**里执行（代理二进制的 `--statfs-helper` 模式：只 statfs 一个绝对路径，不读文件内容），父进程最多等 10 秒（可配 1—60 秒），并发上限 2。
- 超时后该挂载点在 5 分钟内**不再重试**（可能还挂在不可中断的内核 I/O 里的子进程杀不掉，不能每 30 秒再派一个），期间 capacity 为 null、capacity_error 说明解禁时间。
- 本地挂载直接 statfs，不走子进程，不因为网络配置而变慢。

## 7. 共享契约

GET /shares、GET /shares/:id、GET /shares/:id/connections、GET /share-accounts：admin。viewer 只在首页看到共享数量和健康摘要。

ShareDefinition 字段：name（1—64，受限字符）、protocol=smb/nfs、volume_id、relative_path、enabled、read_only、allowed_cidrs、smb_users、nfs_options。

- smb_users 为 account_id + access=read/write 数组；不允许空名单自动变 guest。
- nfs_options 初期仅允许 root_squash=true、sync=true；允许网段不能为空。
- 新建共享发现同名已有系统共享时返回冲突；不得通过名称相同就覆盖。
- 卷未挂载或 UUID 不同，应用动作返回 DEPENDENCY_BLOCKED。
- 更新返回配置哈希和验证结果；删除只移除导出配置，永远不删除目录内容。

AccountSummary：id、name、type=share/regular、managed、disabled、has_password；不返回密码/哈希。

POST /secrets：admin+再认证，仅接受用途枚举和 value；代理加密保存，返回 secret_ref 与过期时间。未被配置引用的临时秘密 10 分钟后销毁；一旦被引用转为持久秘密。用途包括 smb_password、account_password、proxy_basic_password、tls_private_key、backup_repository_password、sftp_private_key、notification_password、app_env。禁止通用 GET 明文接口。TOTP 种子使用代理内部的 totp_seed 用途，由 enrollment 流程生成，不接受通用秘密上传替代。

## 8. 应用、容器与部署契约

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

## 9. 反向代理与证书契约

GET /proxies、GET /proxies/:id、GET /certificates：admin。

POST /certificates/uploads：admin+再认证+CSRF，multipart 单个 PEM 公钥证书文件，最多 1 MiB；返回 certificate_upload_id，绑定会话，10 分钟到期。只暂存输入，导入生效必须执行 certificate.import 任务。

ProxyDefinition：app_id（可空）、domain、upstream_target_id、upstream_scheme、upstream_port、access_mode、allowed_cidrs、basic_auth_users、websocket_enabled、max_body_bytes、read_timeout_seconds、enabled。

domain 为一个不带协议/路径/端口的有效域名；初期不允许通配符。basic_auth_users 为 name + secret_ref，返回只含 name。CertificateSummary 返回 id、domains、issuer、not_before、not_after、renewal_state、last_error，不返回私钥路径或内容。

手动证书公钥通过受限上传接口接收，大小不超过 1 MiB；私钥通过 /secrets 上传。证书申请与续期不接受任意 Certbot 参数。

## 10. 服务、进程、日志、终端契约

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

### 10.1 服务列表与单元（P05.1 起）

GET /services 返回 `status`、`error`（失败时）、`collected_at`、`interval_ms`、`snapshot_version`、`truncated`、`units`；GET /services/:unit 返回 `unit`。两个接口都是权限 V（任何已登录用户可读），都通过代理查询 `services.list` / `services.unit` 取得，Web 不直接读 systemd。

Unit 字段：`unit`（完整单元名，唯一标识）、`description`、`load_state`、`active_state`、`sub_state`、`unit_file_state`（systemctl 原值，例如 `enabled-runtime`）、`enablement`（归一化值）、`template`、`main_pid`、`started_at`、`restart_count`、`memory_bytes`、`tasks_current`、`fragment_path`、`protection`、`capabilities`、`checked_at`。

load_state 闭集：`loaded`、`not-found`、`bad-setting`、`error`、`merged`、`masked`。
active_state 闭集：`active`、`reloading`、`inactive`、`failed`、`activating`、`deactivating`、`maintenance`。
enablement 闭集：`enabled`、`disabled`、`static`、`indirect`、`masked`、`generated`、`transient`、`alias`、`linked`、`unknown`。

模板单元（`foo@.service`，没有实例）**不进入列表**，详情接口对它返回 404：`systemctl show` 不接受这种名字（systemd 255 实测原文 `Unit name foo@.service is neither a valid invocation ID nor unit name.`）并且会**中断整次调用**，而模板本身也没有“已加载/未加载”的真相可填 `load_state`——宁可不出这一行，也不编一个 `not-found`。实例单元（`foo@bar.service`）照常返回，照常按状态给动作。预检遇到模板名给 `SERVICE_STATE_CONFLICT`（模板未指定实例）。

别名单元（UnitFileState=`alias`，例如 `dbus-org.freedesktop.login1.service`）同样**不进入列表**，也不能作为操作目标（对它预检得到 `SERVICE_NOT_FOUND`，请改用规范名）：它就是另一个单元的名字。列出来会有两个后果，而且都是错的——同一份单元出现两行，而资源锁按 `service:<unit>` 走，两个名字各拿一把锁，锁不住同一个对象；保护名单按基名判定，别名不在表里，于是受保护单元看起来“没受保护”，`systemctl` 却会把它解析成 `systemd-logind.service`。

列表里的单元数必须与机器上的真实规模相当：`systemctl show` 返回的块必须逐个对上请求的单元名（按 `Id` 或 `Names` 匹配，别名单元只有 `Names` 里才有你请求的那个名字），对不上就单问一次，仍对不上则采集按失败处理并保留上一次成功快照，**不允许**返回一份看起来正常、实际被截断的列表。

`main_pid`、`started_at`、`restart_count`、`memory_bytes`、`tasks_current` 没有值时是 `null`，不是 `0` 或空字符串。`memory_bytes` 是十进制字符串（超过 2^53 的字节数不能走 JSON number）。`started_at` 来自 ActiveEnterTimestamp，从未启动过为 `null`。

`protection` 包含 `protected` 布尔、`category`（未保护时省略）与 `reasons`（人类可读，去重且顺序固定）。category 闭集：`self`（面板与代理自身）、`ssh`（远程登录）、`systemd_core`（日志/会话/设备管理/总线）、`network`（网络基础服务）、`other_module`（由反代、共享、容器模块管理）。保护名单是代码里的固定表，只能通过改代码扩充；不接受配置、API 写入或临时豁免。判定依据是单元基名（去掉 `@实例` 与 `.service`），不做按名字前缀猜保护（`systemd-` 前缀除外）。

`other_module` 的语义是**整体拒绝**：nginx、smbd/nmbd/winbind、docker/containerd/podman 的启停必须由所属模块按依赖顺序执行，通用服务接口不实现一份简化版，否则会绕过域名、证书与卷挂载检查。

`capabilities` 只列出**当前状态与保护规则下**真正能执行的操作，固定顺序 `start`、`stop`、`restart`、`enable`、`disable`。推导规则：受保护、`not-found`、`error`、`bad-setting` 或模板单元一律为空数组（模板单元本身不进入列表，见上，实例单元照常按状态给动作）；`inactive` 给 start；`failed` 给 start 与 stop；`active`/`reloading` 给 stop 与 restart；`disabled` 给 enable；`enabled` 给 disable。状态已满足的操作不出现——active 不出现 start。空数组表示“这里不提供动作入口”，前端必须渲染成“无可执行动作”，不得渲染成“全部可用”，也不得在空数组时摆一个点了就报错的按钮。

`interval_ms` 默认 10000。列表走缓存快照（每 10 秒采集一次），采集失败时保留上一次成功内容并在 `status`/`error` 里标明原因与 `collected_at`，不清空列表——清空会被读成“这台机器没有服务”。详情接口与预检**不使用缓存**，每次现场读取：列表最多 10 秒旧，而“现在能不能 stop”必须看现在的状态。`truncated=true` 表示单元数超过上限（1000）被截断，不是“只有这些”。

`snapshot_version` 只由单元状态内容（名字、load_state、active_state、sub_state、enablement、template）决定，刻意排除采集时间、PID、内存与重启次数：否则一次普通重启或内存波动就会让预检无谓失效。

service.control 的参数只有 `unit` 与 `operation`（= 上面五个操作的闭集）。不接受的输入：额外的 systemctl 参数、`--now`、`--no-block`、`--all`、分号或管道、以 `-` 开头的单元名、带 `..` 的路径片段。单元名按 `^[A-Za-z0-9][A-Za-z0-9:_.@-]*\.service$` 校验（长度 1—256 字节），这个正则同时挡住 `/`、空格、shell 元字符和选项注入；校验失败返回 400，非法名字不进代理、也不进 systemctl。systemctl 一律用固定 argv 直接调用，不经过 shell。

阻塞项代码：`SERVICE_NOT_FOUND`（这台机器上没有该单元，不会把它当成命令片段去执行）、`SERVICE_PROTECTED`（受保护，理由随附）、`SERVICE_STATE_CONFLICT`（当前状态与该操作矛盾，或模板单元未指定实例、已 mask 无法启动）、`SERVICE_NOT_ENABLEABLE`（没有 `[Install]` 段或状态不允许改开机启动）。保护判定先于状态冲突：受保护单元的理由必须是“不能碰”，而不是“已经启动了所以不用启动”。

告警项代码：`service_interruption`（stop/restart 会中断该服务提供的能力）、`scheduled_start_removed`（disable 只改开机启动，不停止当前进程）。影响类别：`service_availability` 或 `none`。资源锁固定为 `service:<unit>`，同一单元的操作不会并发。

service.control 不需要再认证、不需要确认短语，`cancellable=false`：单单元启停可逆且影响范围明确，加一道人为确认只会让人盲目点过。

执行顺序：现场复核（保护名单与状态可能在上游复核之后又变了）→ 执行 systemctl → 轮询读回最多 15 秒（间隔 150 毫秒）。**systemctl 返回 0 不算成功**：Type=oneshot 的 ExecStart 可能失败而 systemctl 仍返回 0。只有读回状态符合期望才算成功——start 要求 `active`/`reloading`，stop 要求 `inactive`，enable/disable 要求对应 enablement。restart 额外比较 ActiveEnterTimestamp 不早于发起前 2 秒：只看 active 会把“本来就在 active、restart 实际没生效”判成成功，而那正是最需要发现的情况。未在 15 秒内收敛必须让任务失败并如实说明“命令成功但状态未收敛”，不得标记 succeeded。

不存在的单元不触发任何 systemctl 控制命令，也不做“按前缀批量操作”。

### 10.2 进程列表与信号（P05.2 起）

GET /processes 返回 `status`、`error`（失败时）、`collected_at`、`interval_ms`、`total`、`truncated`、`processes`；权限 **admin**（进程列表带属主与命令行，比服务状态更接近主机内部，不向 viewer 开放）。数据经代理的 `processes.list` 取回，Web 不直接读 /proc。

Process 字段：`pid`、`start_time_ticks`、`ppid`、`owner`、`state`、`cpu_percent`、`memory_bytes`、`command`、`unit`、`protection`、`capabilities`。

**身份是 `pid` + `start_time_ticks`，不是 `pid`。** 内核会重用 PID；`start_time_ticks`（/proc/<pid>/stat 第 22 字段，开机以来的时钟滴答数）不会。两者相加才指认“那一个”进程，预检、执行前复核与结果读回都用这一对。身份指纹是 `sha256("process/v1|<pid>|<start_time_ticks>")`，状态版本是 `sha256("process-state/v1|<pid>|<start_time_ticks>")`；状态版本刻意与身份同源——进程的 CPU、内存、运行状态随时在变，把它们算进去会让每一份预检在几秒内无谓失效，而不算进去也不会放过任何一次 PID 重用。

state 闭集（/proc 状态字母归一化）：`running`、`sleeping`、`disk_sleep`、`zombie`、`stopped`、`tracing`、`dead`、`idle`、`paging`、`unknown`。内核新增字母时给 `unknown`，不猜。

`cpu_percent` 是两次相邻采样之间 CPU 时间的增量除以墙钟时间（单核基准，可以超过 100）。**新出现的进程第一份快照里没有上一样本，值就是 `null`，不是 `0`**——0 会被读成“完全空闲”。`memory_bytes` 是十进制字符串（超过 2^53 的字节数不能走 JSON number），内核线程与已退出进程为 `null`：它们没有 VmRSS，给 0 是编数。`command` 是脱敏后的摘要（1—256 字符）：argv[0] 取基名，敏感开关（密码、令牌、密钥类参数）的值替换成固定掩码，无 argv 的进程显示成 `[comm]`。`unit` 来自 cgroup v2 归属，非 systemd 单元为空字符串。

`protection` 包含 `protected` 布尔、`category`（未保护时省略）与 `reasons`（人类可读，去重且顺序固定）。category 闭集：`init`（PID 1）、`kernel_thread`（无用户空间的线程）、`self`（面板与代理自身）、`ssh`（远程登录）、`systemd_core`（日志/会话/设备管理/总线）、`network`（网络基础服务）、`other_module`（由反代、共享、容器模块负责的守护进程）。保护名单是代码里的固定表，只能通过改代码扩充；不接受配置、API 写入或临时豁免。

`capabilities` 只列出当前状态与保护规则下真正能发送的信号，固定顺序 `TERM`、`KILL`。受保护与已退出（`zombie`/`dead`）的进程一律为空数组：空数组表示“这里不提供动作入口”，前端必须渲染成“不可操作”，不得摆一个点了就报错的按钮。

`interval_ms` 默认 5000，列表走缓存快照（每 5 秒采集一次），单次最多收录 2000 个进程，超出时 `truncated=true` 并按 PID 升序截断——`truncated=true` 表示“不是只有这些”，不是完整列表。采集失败时保留上一次成功内容并在 `status`/`error` 里标明原因与 `collected_at`，不清空列表：清空会被读成“这台机器没有进程”。预检与执行**不使用缓存**，每次都现场读 /proc——列表最多 5 秒旧，而“现在这个 PID 上还是不是那个进程”必须看现在。

process.signal 的参数只有 `pid`、`start_time_ticks`、`signal`（`TERM` | `KILL`，大写闭集）。不接受的输入：只有 PID 没有 starttime（那样等于允许误杀被重用的 PID）、按名字/正则/前缀匹配、批量目标、`force`/`now` 之类会被翻译成 kill 选项的开关、负数或 0 的 PID。一个进程，一个信号。

阻塞项代码：`PROCESS_NOT_FOUND`（这个 PID 上没有进程）、`PROCESS_IDENTITY_MISMATCH`（PID 还在，但 starttime 变了——这是另一个进程）、`PROCESS_PROTECTED`（PID 1、内核线程、面板/代理自身、受保护系统服务进程，理由随附）、`PROCESS_ALREADY_EXITED`（僵尸或已死，发信号不会让它更快消失）。**判定顺序固定为：参数 → 现场读取 → 身份 → 保护 → 是否已退出。** 身份先于保护：一个被重用的 PID 不是“受保护的进程”，而是“不是你要找的那个进程”，两者处置完全不同。

告警项代码：`process_terminate`（SIGTERM 可以被捕获或忽略）、`process_force_kill`（SIGKILL 无法被捕获或忽略）。影响类别：`process_termination`、`process_data_loss`、`none`。资源锁固定为 `process:<pid>:<start_time_ticks>`——按身份加锁，同一 PID 上的两个不同进程不会互相阻塞，同一个进程的并发操作不会叠加。

TERM 与 KILL 是**两次独立操作**，门槛不同：TERM 不需要再认证、不需要确认短语；KILL 需要 5 分钟内的密码再认证加确认短语 `KILL XXXXXX`（字母表去掉易混的 I/O/0/1，6 位，一次性，只在该次预检响应里出现，落库只存哈希）。两者的 `cancellable` 都是 `false`：信号发出之后没有“安全取消”可言。门槛在生成预检时按信号算定并落库（`requires_reauth` 与短语哈希），提交与执行读的是那份记录——所以受保护的 KILL 预检仍然带着再认证要求与短语，拦下它的是 `allowed=false`（提交得到 409 `DEPENDENCY_BLOCKED`），不是把门槛藏起来。

**面板不会在 TERM 超时后自动升级为 KILL。** 那等于把一个普通动作偷偷变成强制动作，还顺手绕过了 KILL 的再认证与短语。TERM 超时按失败结束并如实说明“信号已发出但进程仍在运行，需要单独发起 KILL 预检”。

执行顺序：现场复核（重新读 /proc 比对 starttime、保护名单与退出状态，任一不符就一个信号都不发）→ 发送信号 → 轮询读回（间隔 100 毫秒；TERM 最多 10 秒，KILL 最多 5 秒）。**发送成功不算成功**：只有读回确认进程消失、变成僵尸，或那个 PID 已经被另一个 starttime 的进程占用，才算 `outcome=verified`。读回超时必须让任务失败：TERM 说明进程未退出，KILL 说明它可能停在不可中断的内核等待（D 状态），提示到主机上确认。发送时内核回 `ESRCH`（检查与发送之间进程自己退出了）说明意图已达成，但结果里 `delivered=false` 并附一句“信号未送出”——不把没发生的事说成发生了。

不支持批量、不支持向“同名进程”发信号、不支持在列表接口上直接终止：任何写操作都必须走 `process.signal` 的预检与任务。

## 11. 文件契约

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

## 12. 软件、电源、备份与计划任务契约

GET /packages?query=&installed=&upgradable=、GET /packages/updates、GET /system/power-state：admin。

GET/POST /backup/repositories、PATCH /backup/repositories/:id：admin+再认证；这些接口只登记元数据，初始化真实仓库使用 backup.repository.initialize 任务。字段 name、backend=local/sftp、location、password_secret_ref、credential_secret_ref、version。location 按 backend 判别：local 为 root_id+relative_path；sftp 为 host、port、username、remote_path、host_key_fingerprint。代理验证路径/主机，不接受 restic 任意命令行选项；SFTP 主机密钥必须显式登记，禁止 StrictHostKeyChecking=no。

GET/POST /backup/policies、PATCH/DELETE /backup/policies/:id：admin；字段 name、repository_id、sources（root_id+relative_path）、consistency_mode=files_only/stop_managed_apps、app_ids、retention（keep_last/keep_daily/keep_weekly）、version。执行前展示停机影响。删除策略不删除仓库或快照。1.0 的清理使用管理员明确发起的 backup.prune；备份定时任务不隐式执行删除操作。

GET /backup/snapshots?repository_id=、GET /backup/snapshots/:id：admin；展示时间、来源、大小、consistency_mode 和验证记录。删除历史快照/清理仓库必须采用显式、单独预检的策略执行，不在页面查询时触发。

GET /schedules、GET /schedules/:id：admin；ScheduleDefinition 包含 name、kind=backup/certificate_renewal/user_script、target_id、calendar_expression、timezone、persistent、max_runtime_seconds、enabled。脚本 target_id 必须指向预先登记且由 upanel-shell 运行的普通脚本，不能是 shell 命令文本。

GET /scripts 只返回元数据，GET /scripts/:id 可读取管理员上传的内容，均为 admin。创建/修改/删除通过 script.apply/script.delete 预检与任务执行；content 为 UTF-8、最多 64 KiB，不能指定任意路径。代理保存为 root 所有且 upanel-shell 只读可执行的受管理脚本，运行前核对已批准内容哈希，以避免终端用户修改已获授权的定时脚本。script.run 执行时降权，永远不获得 root。

## 13. 网络、账号与告警契约

GET /network/interfaces、GET /network/routes、GET /network/listeners、GET /firewall、GET /accounts、GET /accounts/:id/ssh-keys：admin。

NetworkDefinition：mode=dhcp/static、address_cidr（静态必填）、gateway（可空）、dns_servers、confirmation_timeout_seconds=120。只允许 PLAN.md 的可写平台和配置类型。其他复杂配置返回 unsupported，不能丢弃未理解字段。

FirewallDefinition：enabled、default_incoming=deny、default_outgoing=allow、rules；每条 rule 为 id、action=allow/deny、protocol=tcp/udp、port_start、port_end、source_cidr。初期不支持任意 UFW 命令、转发和 NAT 规则。

POST /network/changes/:id/confirm、POST /firewall/changes/:id/confirm：admin+CSRF，输入 confirmation_token；token 绑定变更、会话和目标连接地址，服务端验证连接实际到达的本地地址，不相信浏览器自报。取消回滚 timer 前持久化确认结果，并确保不存在竞争窗口。

初版写网络配置时要求面板通过直连管理网地址访问；通过 SSH 隧道或外部反向代理无法可靠验证候选地址时，只读展示并提示使用主机控制台完成变更。不能用请求头伪造候选地址确认。

网络任务运行期间的 change_state：pending_apply、awaiting_confirmation、confirmed、rolling_back、rolled_back、rollback_failed。任务在 awaiting_confirmation 时仍为 running。确认成功后 succeeded；自动回滚成功后 failed + rolled_back；回滚失败为 failed + partial 并提供本机恢复步骤。

AccountDefinition：name、display_name、type=regular/share、disabled、password_secret_ref（可空）、shell_profile=disabled/restricted。系统 UID 由安全范围分配；禁止用户提交 uid=0、任意 shell 路径或任意 groups。sudo 模板初期只允许“查看选定服务状态/日志”等固定只读能力，不提供 unrestricted 或 ALL 模板。

GET/POST /alerts/rules、PATCH/DELETE /alerts/rules/:id：读 V、写 admin；规则字段 metric、target_id、comparison、threshold、secondary_threshold、combination=any/all、duration_seconds、recovery_threshold、cooldown_seconds、enabled、version。

GET /alerts/events：V；POST /alerts/events/:id/ack：admin；GET/POST /notification-channels、PATCH/DELETE /notification-channels/:id、POST /notification-channels/:id/test：admin；通道字段 type=in_app/smtp/webhook、受约束 endpoint/settings、secret_ref、enabled。测试发送是明确的操作，不能在保存时自动发送。

## 14. 最小数据库模型

以下为逻辑表；实施时用 SQL 迁移建立真实外键、索引和约束。不能只在前端检查唯一性。所有 mutable 元数据表包含 version、created_at、updated_at；PATCH 用 WHERE id=? AND version=? 更新并自增，零行即 409。

### 14.1 Web 数据库

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

### 14.2 代理数据库与秘密

代理数据库独立由 root 读写，不能让 Web 直接编辑。表至少包括：operations（operation_id UNIQUE、request_hash、动作、阶段、结果）、operation_events、preflight_plans（参数/设备指纹/到期/使用状态）、managed_resources（主机实际接管清单）、config_backups、secrets（密文、用途、引用、时间）、pending_network_rollbacks。

秘密使用维护中的 AEAD 实现，独立随机 nonce；本机密钥保存 /etc/upanel/agent.key，root:root 0600。密钥不可由 Web 读取；备份/灾难恢复文档明确要求单独保管该密钥。

两个数据库不是分布式事务。代理完成操作后记录最终资源和结果，Web 按 operation_id 对账后更新元数据。若 Web 写库失败，任务保持待对账标记并重试同步，不能重新执行系统操作。启动时检查所有非终态和未同步任务。

任务日志默认保留 30 天、每任务最多 10 MiB；审计保留 180 天且清理由管理员配置。日志截断必须有显式标记。幂等记录至少保留 24 小时，关联的执行历史不能因清理提前丢失导致重复执行。

## 15. Web 与代理私有协议

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

## 16. 契约验证要求

P00 建立 OpenAPI 文件骨架，后续每阶段先补齐对应 schema、枚举、权限、错误、请求示例再实现。不要在阶段未实现时生成“已支持”的 API 文档。

每个变更验证前后端类型、未知字段拒绝、枚举和状态机一致。关键契约至少覆盖：容量大整数、viewer 越权、CSRF、幂等冲突、预检过期、设备变化、文件版本冲突、任务重连、代理离线、非零退出、进程重启对账。

本文没有给出外围功能的所有 SQL 列与 UI 文案，但已固定其动作、参数、范围和实现次序。实施模型可以补充内部细节；不能借此新增产品能力、改名公开字段或取消检查。
