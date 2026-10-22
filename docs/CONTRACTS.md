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

统一错误码至少包括：VALIDATION_ERROR、UNAUTHENTICATED、FORBIDDEN、REAUTH_REQUIRED、CSRF_FAILED、NOT_FOUND、VERSION_CONFLICT、RESOURCE_BUSY、DEVICE_IN_USE、PROTECTED_DEVICE、UNSUPPORTED_DEVICE、DEVICE_IDENTITY_CHANGED、PATH_OUTSIDE_ROOT、DEPENDENCY_BLOCKED、PREFLIGHT_EXPIRED、PREFLIGHT_STALE、CONFIRMATION_MISMATCH、IDEMPOTENCY_CONFLICT、AGENT_UNAVAILABLE、DEPENDENCY_MISSING、NOT_IMPLEMENTED、COMMAND_FAILED、OPERATION_INTERRUPTED、ROLLBACK_FAILED、ROOT_UNAVAILABLE、FILE_NOT_TEXT。

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

**被拒绝的预检没有目标可绑定。** `allowed=false` 时 `target.identity_fingerprint` 允许为空——典型情形就是设备已经不在拓扑里（阻塞项 `DEVICE_NOT_FOUND`：拔了、改名了、`device_id` 写错），此时 `state_version` 退到拓扑版本。目标绑定（指纹 + 状态版本）是**放行**的前提，因为它是执行前复核“还是那个目标”的唯一依据；被拒的预检不可能被执行（提交端对 `!allowed` 直接 409），所以不要求绑定，也**不得为了让校验通过而编造指纹**。`target.display_name` 始终填，界面靠它说明拒绝的是谁。（Q190）

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

### 6.3 附加字段（P07.2 起）

P07.2 开始分区动作会真的写盘，于是 `partition_table` 多出两个**只在读到 GPT 时出现**的字段。
两个都是补充：`additionalProperties: false` 的客户端在没有 GPT 的盘上看到的还是老样子。

| 字段 | 类型 | 语义 |
|---|---|---|
| partition_table.disk_guid | 字符串，读不到时不出现 | 盘上 GPT 头里的 DiskGUID（规范大写带连字符） |
| partition_table.disk_entries | 数组，读不到时不出现 | 盘上条目表（LBA 2 起）的原始内容，与 partitions 是两个来源 |

disk_entries 的每一项：

| 字段 | 类型 | 说明 |
|---|---|---|
| number | 整数 ≥1 | 条目表里的槽位号，也是内核给分区的编号 |
| start_bytes / size_bytes | 十进制字符串 | 按 512 字节扇区换算 |
| type_guid | 字符串 | 分区类型 GUID 的规范大写形态（`type=linux` 即 `0FC63DAF-8483-4772-8E79-3D69D8477DE4`） |
| name | 字符串 | 条目里的 UTF-16LE 分区名；空串表示名字为空，不是解析失败 |

规则：

- **两个来源分开报，不合并。** partitions 来自 sysfs（内核此刻认定的分区），disk_entries 来自盘上的字节。
  写动作的结果核验要求两边都对上：只看盘上字节会漏掉“内核没重读分区表”，只看内核会漏掉
  “盘上写坏了但内核还拿着旧表”。
- 只读**主** GPT（LBA 1—33）。盘尾的备份表不解、不比对，这是已记录的限制（PROGRESS 第 8 节）。
- 这两个键只在 `source=device` 且真的读到 GPT 时出现。`kernel`/`skipped` 的设备上键**不存在**——
  不是空数组、不是空串。判据是“读到了”与“没读到”，不是“有没有分区”。
- 分区名只接受可打印 ASCII、长度 ≤36，并且**不允许 `"` 与 `\`**：模板里名字是 `name="<名字>"`，
  一个双引号会提前结束引号、让 sfdisk 报 `line 1: unsupported command`（实测 rc=1）；反斜杠会被
  转义处理吃掉。其余可打印 ASCII 原样落盘、原样读回（实测）。
- 写动作的 result 沿用第 4.3 节的通用结构，字段如下：
  - `observed_state`：`device_id`、`table_type`、`disk_guid`、`disk_entries`、`kernel_partitions`
    （字符串数组）、`udev_settled`（布尔）。
  - `data`：`device_id`、`table_type`、`partitions`（整数）、`checked_via`（固定
    `["disk_bytes","kernel_sysfs"]`）；create 另带 `created_partition`（`number`、`start_bytes`、
    `size_bytes`、`name`、`device_id`），delete 另带 `deleted_device_id`。
- 写动作只在**两源都对上**之后才是 `verified`：退出码 0、`sfdisk` 打印成功、udev 已稳定，都不算验证。

### 6.4 附加字段（P07.3 起）

P07.3 开始 `storage.filesystem.format` 会真的调用 `mkfs` 并读回核验。格式化**不改变分区表**，
所以它的 result 不复用 6.3 的 `table_type`/`disk_entries`，改用下面这组字段。

预检与门槛：

| 项 | 值 |
|---|---|
| 参数 | `device_id`（分区）、`filesystem`（闭集 `ext4`/`xfs`）、`label`（可空） |
| 标签上限 | ext4 **16 字节**、XFS **12 字节**；只接受可打印 ASCII，且不能含 `/` |
| 超限的判决 | **预检之前**就拒：400 `VALIDATION_ERROR`，消息里带真实上限；不生成 `preflight_id` |
| 再认证 / 确认短语 | 都需要；短语形如 `FORMAT XXXXXX`（6 位，去掉易混的 I/O/0/1） |
| `cancellable` | `false`：动作开始后不能取消，取消请求得到 409 `VALIDATION_ERROR` |
| 锁 | 整盘（`disk:<disk_id>`），与分区动作互斥 |

result.observed_state（写完之后从设备读回来的事实）：

| 字段 | 类型 | 说明 |
|---|---|---|
| device_id / disk_id | 字符串 | 目标分区与它所在的整盘 |
| filesystem | 字符串 | 请求的文件系统类型（读回的分类也必须与它一致） |
| uuid | 字符串 | 新文件系统的 UUID；**必须与写之前不同** |
| label | 字符串 | 读回的卷标，逐字节与请求一致（截断会被抓出来） |
| size_bytes | 十进制字符串 | 文件系统容量 |
| partition_size_bytes | 十进制字符串 | 分区容量（`0 < size_bytes ≤ partition_size_bytes`，且不小于其 9/10） |
| block_size_bytes | 十进制字符串 | 文件系统块大小 |
| signature | 字符串 | **独立**签名分类器（P07.1 的扫描器）读到的类型；与 superblock 结论不一致即失败 |
| udev_settled | 布尔 | `udevadm settle` 是否成功；失败不单独判失败，只如实上报 |
| node_present | 布尔 | 写完之后设备节点是否还在 |

result.data（这次到底做了什么）：

| 字段 | 类型 | 说明 |
|---|---|---|
| device_id / filesystem | 字符串 | 同上 |
| uuid / label / size_bytes | 同上 | 同上 |
| checked_via | 字符串数组 | 固定 `["superblock","signature_probe"]`：两条独立的读路径 |
| command | 字符串数组 | 实际执行的 argv（含程序路径），供审计回放 |
| previous_kind | 字符串 | 写之前那个分区上是什么文件系统；空串表示没有签名 |
| previous_uuid | 字符串 | 写之前的 UUID（无签名时为空） |

规则：

- **退出码 0 不算成功。** 只有 superblock 读回、几何关系、标签逐字节一致、UUID 已更换，
  并且独立分类器给出同一个结论，才算 `verified`。任务失败时消息必须带 mkfs 的真实输出
  与“盘上现在是……”（读回的实际状态），并且明确提示“核实实际状态之前不要重新提交”。
- **ext4 不带 `-F`**（实测：覆盖 ext4/xfs/随机内容都不需要它，stdin 为空时也不会提问）；
  **XFS 固定带 `-f`**（实测：任何已有签名都会让 mkfs.xfs 以 rc=1 要求 `-f`）。两者都
  解不开 mkfs 自己的“已挂载”检查，所以 `-f` 不是占用保护的旁路。
- 格式化动作在**进程执行期间**任务上下文结束时，一律记为 `interrupted`
  （`OPERATION_INTERRUPTED`，实际状态未知），不记为 `failed`——失败的语义是“可以放心重来”。
- 预检之后的第二道门：提交时与执行前各重算一次判决并逐字比较。比较用的是**采集快照的版本**
  （采集间隔 5 秒），而写动作真正动手前还会**直接从设备**重读分区表条目、签名与节点。
  两者分工：版本比较抓“从预检到现在盘上变了”，设备直读抓“此刻这块盘还是不是那块盘”。
- **版本只覆盖结构，不覆盖盘上的签名。** `snapshot_version` 由拓扑内容算出：节点（整盘/分区/
  LVM/RAID/crypt/loop）、边、分区表条目、挂载、身份指纹、容量。未挂载分区的文件系统签名、UUID
  与卷标**不在其中**——`storage.scan` 只给整盘与**已挂载**的分区填 `filesystem`。真机实测
  （P07.3 验收 §10a）：在一个未挂载、带卷标的 ext4 分区上 `wipefs -a`，前后两次快照版本
  **逐字节相同**；在同一块盘上新建一个分区，版本立刻变。于是版本比较能抓“结构变了”，抓不到
  “签名被换了”；后者由写动作动手前的设备直读承担，并如实上报 `previous_kind`/`previous_uuid`。
  对 `format` 来说签名变化本来也不构成拒绝理由：它的工作就是覆盖掉那个签名。

### 6.5 受管理卷（P07.4 起）

P07.4 新增五个写动作与三个读接口。**卷动作都不要确认短语**：短语留给“一旦写下去就找不回来”的
动作（初始化分区表、格式化、删分区）；导入只是登记 + 写一行 fstab，卸载随时能挂回去，
配置与退出管理都有备份可回滚，用短语只会训练人闭眼确认。

| 动作 | 参数 | 再认证 | 锁 | 结果核验来源（`checked_via`） |
|---|---|---|---|---|
| `storage.volume.import` | `device_id`（分区）、`slug`、`display_name` | 否 | `disk:<整盘>`、`fstab` | `["fstab_reread","systemctl_show"]` |
| `storage.volume.mount` | `volume_id`、`version` | 否 | `volume:<id>`、`disk:<整盘>` | `["findmnt","topology_scan"]` |
| `storage.volume.unmount` | `volume_id`、`version` | 是 | `volume:<id>`、`disk:<整盘>` | `["findmnt","topology_scan"]` |
| `storage.volume.configure` | `volume_id`、`version`、`automount`/`read_only`/`nodev`/`nosuid`（都可空） | 是 | `volume:<id>`、`fstab` | `["fstab_reread","systemctl_show"]` |
| `storage.volume.forget` | `volume_id`、`version` | 是 | `volume:<id>`、`fstab` | `["fstab_reread"]` |

五个动作全部 `cancellable=false`。挂载点由 `slug` 派生（`/srv/upanel/volumes/<slug>`），不接受
调用方指定路径；`slug` 只允许小写字母、数字与中划线。`version` 是登记记录的乐观并发令牌，
与页面看到的那份不一致时判 `VOLUME_VERSION_MISMATCH` 而不是“按请求里的版本执行”。

五个动作**放行的预检都必须绑定两样东西**：目标身份指纹（`target.identity_fingerprint`）与状态版本。
执行端动手前用它们复核“要动的还是预检时那个、现场没变过”。缺任何一样都是实现错误（面板侧 500），
不是“可以放宽的检查”——P07.4 的 configure 就这么漏过一格（Q199）。被拒绝的预检没有目标可绑，
指纹可以为空（Q190）。

fstab 受管块（只有一块，块外内容逐字保留）：

```
# BEGIN upanel managed volumes
# 以下条目由 uPanel 维护（同一块内的手工修改会在下次变更时被覆盖）
UUID=<uuid> <mount_path> <fstype> rw|ro,nodev,nosuid,nofail,x-systemd.device-timeout=30[,noauto] 0 2|0
# END upanel managed volumes
```

- 选项顺序固定；`nofail` 总是有（一块数据盘不在，不该让机器起不来）；`automount=false` 时追加
  `noauto`。第六列 ext4 是 `2`、XFS 是 `0`（`fsck.xfs` 只会回一句 nothing to do，甚至挂住开机）。
- 写路径固定：读 → 比哈希（变了就是 `PREFLIGHT_STALE`）→ 渲染候选 → 写临时文件 → fsync →
  `findmnt --verify --tab <候选>` → 备份 → `rename` → fsync 目录。**候选不合格时目标文件一个字节
  都没动**（`COMMAND_FAILED`，消息带“候选 fstab 未通过校验”）；渲染结果与原文件相同则直接返回，
  既不写也不留备份。
- 备份目录 `/var/lib/upanel-agent/fstab-backups/`，文件名 `<UTC 时间戳>-<短哈希>.fstab`，0600。
- 明确禁止 `mount -a`：一条写坏的条目会顺手把别的东西挂到不该挂的地方。

`GET /storage/volumes`（对象包一层 `{"volumes":[…]}`，空列表是 `[]`）、`GET /storage/volumes/:id`、
`GET /storage/volumes/:id/blockers`（admin）。卷标识形状不对是 400 `VALIDATION_ERROR`
（不透传任意字符串给代理），不存在是 404 `NOT_FOUND`。

VolumeBlockers 字段与语义：

| 字段 | 语义 |
|---|---|
| `mounted` / `mounts` | 这块设备现在挂在哪儿。挂载状态**只**在这里表达 |
| `processes` | 打开着这块设备**或**这个挂载点上任何文件的进程，`[{pid, comm}]`，按 pid 升序去重 |
| `holders` / `swap_active` | dm/md 消费者、活动交换区 |
| `apps` / `shares` / `backups` | 面板侧依赖；P07.4 时三个模块尚未实现，恒为 `[]` |
| `busy` | 结论：**现在卸载会被谁挡住**。只有真的有人开着文件、有 dm/md 消费者、是交换区或有面板侧依赖时为真；**挂着但空闲不算忙** |
| `checked_at` | 这次检查的时间（占用是某一瞬间的快照，必须带时间） |

- 两种占用都要查，理由不同：`umount` 撞的 `EBUSY` 来自“这个文件系统上还有打开的文件”，
  而 `dd`/`mkfs` 这类直接开块设备的进程反过来不会让 `umount` 失败。只查其一，页面就会在
  肯定卸不掉的时候说“没人用”。
- 查不到就报“查不到”：读不到 `/proc`、扫描出错、或本机没配占用检查器，一律判为忙
  （`DEVICE_BUSY`，消息说明“无法确认没人在用”），绝不返回空列表冒充“没人用”。
- 两道门的时间差是真实的：预检时无人占用（`allowed=true`）→ 执行前占用者出现 → `umount` 返回
  `target is busy`，任务失败并把当时的占用者重新列一遍（`当前占用：…`），登记记录与
  `desired_state` 不动。

`observed_state` 沿用 §6 的闭集，不新增取值：“文件系统挂着但不在面板的挂载点上”仍是 `mounted`，
区别放在内部布尔 `MountedElsewhere` 上，判决据此拒绝（`VOLUME_MOUNTED_ELSEWHERE`）。`capacity`
只在真的挂着且读到时存在。

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

### 7.1 P08.1 已实现范围

P08.1 落地的是“定义与准备”这一半：Samba 能力检测、受管理共享账号、密码秘密通道、目录与
POSIX ACL 的准备与核验、共享定义与配置版本（渲染块的 sha256）。**配置写入这一半不在本阶段**：
不创建 `/etc/samba/upanel.d`、不往 `smb.conf` 里加 include、不跑 testparm、不重载。因此本阶段
的共享**不对外提供任何访问**，`capability.config_write` 恒为 `unsupported`。

四个端点，权限全部是 admin（共享定义里有路径、账号名单与来源网段，合起来就是“谁能访问哪个
目录”的完整答案；viewer 只在首页看数量与健康摘要）：

| 端点 | 代理查询 | 说明 |
|---|---|---|
| `GET /shares` | `share.list` | 能力报告 + 共享列表，一次拿全 |
| `GET /shares/:id` | `share.list` | 从同一份列表里挑一条；形状不对 400，不存在 404 |
| `GET /share-accounts` | `share.accounts` | 能力报告 + 受管理账号列表 |
| `POST /secrets` | `POST /internal/v1/secrets` | 秘密通道，admin + CSRF + 再认证，只进不出 |

三条读的路由与三条查询一一对应，**详情不单独开一条查询**：共享数量是几十的量级，全量返回的
代价远小于两个只差一个过滤条件的入口各自演化。能力报告与列表一起返回，是因为分成两次请求时
中间会出现一小段时间界面显示“已启用、已生效”，而 Samba 那边还没有这一段。

未实现（P08.3 起）：运行时脱盘撤销、启动依赖、共享与卷的绑定检查。配置写入、最小 include、
testparm 校验、重载与连接状态在 §7.2（P08.2）。

五个动作的 P08.1 执行结果：

| 动作 | 参数 | 再认证 | 锁 | outcome | 执行了什么、核验到什么 |
|---|---|---|---|---|---|
| `share.apply` | `share_id`（新建 null）、`version`（新建 null）、`definition` | 是 | `volume:<id>`、`share:<id>` | `accepted` | 卷内建目录（0700）、`setfacl --set` 写入并读回核验 mask、算 `config_hash`、登记记录 `directory_prepared=true`、`config_applied=false` |
| `share.disable` | `share_id`、`version` | 否 | `share:<id>` | `accepted` | 读回 ACL 确认撤销、`definition.enabled=false`、`directory_prepared=false`；目录与文件一个都不删 |
| `share.delete` | `share_id`、`version` | 是 | `share:<id>` | `accepted` | 撤销 ACL、删登记记录；**目录内容永远不删** |
| `share.account.apply` | `account_id`（新建 null）、`name`、`secret_ref`、`disabled` | 是 | `accounts`、`samba` | `verified` | `useradd --system`（nologin、无 home）+ `smbpasswd -s -a`，用 `getent passwd`、`passwd -S`、`pdbedit -L -v` 读回核验 |
| `share.account.delete` | `account_id` | 是 | `accounts`、`samba` | `verified` | 先 `smbpasswd -x` 再 `userdel`（反过来会留下一条指向已消失 UID 的 passdb 记录），两侧都读回确认已消失 |

五个动作全部 `cancellable=false`、**都不要确认短语**：短语留给“一旦写下去就找不回来”的动作
（初始化分区表、格式化、删分区）；共享动作用短语只会训练人闭眼确认。

**放行的预检必须绑定两样东西**（与 §6 的存储动作同一条规则）：`target.identity_fingerprint` 与
状态版本。新建对象的状态版本是 `absent:<名字>`——对一条还不存在的对象，可绑定的事实就是“此刻
还没有它”（Q214）；账号类动作的指纹是账号名（改名在预检里就被拒，它在预检与执行之间不会变），
删除也一样（Q220）。被拒绝的预检没有目标可绑，指纹可以为空（Q190）；缺绑定是**实现错误**
（面板侧 500），不是“可以放宽的检查”。

**为什么 `share.apply` 是 `accepted` 而不是 `verified`。** 它确实做出了东西并核验过（目录、ACL、
哈希、记录），但“共享可用”这件事在本阶段不成立：没有配置写入就没有 Samba 侧的这一段。
执行结果里因此写死两件事实：`observed.samba_config="not_written"`、`observed.served=false`，并且
预检一律带一条 `SHARE_CONFIG_NOT_APPLIED` 告警。代理层的执行器**原样传递** outcome——如果它像
存储执行器那样硬写 `verified`，任务中心就会显示“已核验”，而事实是配置还没写。

配置版本与准备状态是三个字段，回答三个不同的问题，界面必须分开显示：

- `config_hash`：这份定义渲染出来的配置是哪一版（sha256 十六进制）；
- `directory_prepared`：目录与 POSIX ACL 是否已经按这份定义准备**并核验**过；
- `config_applied`：这份定义是否已经写进 Samba 配置并被重载。P08.1 恒为 `false`，没有代码路径能把它置真。

`smb_users` 为空是**硬拒绝**（`SHARE_NO_USERS`）：空名单在 Samba 里会退化成 guest，而 guest 在
PLAN.md 第 10 节是明确禁止的。`read_only` 为真时每个账号的 ACL 上限都是 `r-x`（更强的限制优先）。

能力报告的字段（`ShareCapability`）：`status`（`available`/`blocked`）、`reason`、`samba_version`、
`tools`（探到的绝对路径，缺失项不出现）、`config_path`、`config_readable`、`include_dir`、
`include_ready`、`include_present`、`config_write`、`notices`、`collected_at`。探针只做只读的事，
**不安装任何东西**（PLAN.md 第 10 节：禁止静默安装）；工具路径是代码里的常量，不可配置——一旦
可配置，配置就变成了任意命令执行的入口。

阻塞码与告警码是闭集，测试按码断言：

| 码 | 类别 | 含义 |
|---|---|---|
| `SHARE_NOT_FOUND` | 阻塞 | 登记表里没有这条共享 |
| `SHARE_VERSION_MISMATCH` | 阻塞 | 手上的版本不是现在这一版（乐观并发） |
| `SHARE_NAME_TAKEN` | 阻塞 | 同名共享已存在；禁止靠“名字相同”覆盖 |
| `VOLUME_NOT_FOUND` | 阻塞 | `volume_id` 不在受管理卷里 |
| `VOLUME_NOT_MOUNTED` | 阻塞 | 卷没挂载；共享路径会落到系统盘的空目录上 |
| `VOLUME_UUID_MISMATCH` | 阻塞 | 卷挂着的不是登记的那个文件系统 |
| `SHARE_PATH_INVALID` | 阻塞 | `relative_path` 形状不合法（绝对路径、`..`、空段） |
| `SHARE_PATH_ESCAPES_VOLUME` | 阻塞 | 规范化后的真实路径不在卷挂载点里面 |
| `SHARE_PATH_SYMLINK` | 阻塞 | 路径上有符号链接；初期一律拒绝跟随 |
| `SHARE_PATH_NOT_DIRECTORY` | 阻塞 | 目标存在但不是目录 |
| `SHARE_PATH_NOT_TRAVERSABLE` | 告警 | 共享路径上有一级上级目录缺 other 的 `x` 位，共享账号进不去；应用时会补成 `0711`（只加穿越，不加读取与列出） |
| `SHARE_HOSTS_ALLOW_LOOPBACK` | 告警 | `allowed_cidrs` 里没有 `127.0.0.0/8`：Samba 仍然放行本机，这份名单管不住本机连进来的访问 |
| `SHARE_NO_USERS` | 阻塞 | `smb_users` 为空（会退化成 guest） |
| `ACCOUNT_MISSING` | 阻塞 | 定义引用了不存在的账号 |
| `ACCOUNT_NOT_MANAGED` | 阻塞 | 同名账号在系统上存在但不是面板创建的；拒绝静默接管 |
| `ACCOUNT_DISABLED` | 阻塞 | 账号已被禁用（Samba 层 D 标志） |
| `ACCOUNT_HAS_SHARES` | 阻塞 | 还有共享引用这个账号，不能删 |
| `ACCOUNT_NAME_INVALID` | 阻塞 | 账号名不符合形状要求 |
| `ACCOUNT_NAME_TAKEN` | 阻塞 | 这个账号名已经有一条受管理记录 |
| `PROTOCOL_UNSUPPORTED` | 阻塞 | `protocol=nfs`（P08 只做 smb） |
| `SMB_UNAVAILABLE` | 阻塞 | 本机 Samba 能力不可用（缺包或缺工具） |
| `SECRET_INVALID` | 阻塞 | `secret_ref` 不是本机秘密存储里的引用，或用途不对 |
| `ACL_TOOLS_MISSING` | 阻塞 | `setfacl`/`getfacl` 缺失，无法准备或核验权限 |
| `CONFIG_NOT_READABLE` | 阻塞 | 读不到 `smb.conf`；读不到就不能说这台机器的 Samba 长什么样 |
| `INCLUDE_DIR_MISSING` | 告警 | 托管 include 目录不存在（P08.2 才创建） |
| `SHARE_CONFIG_NOT_APPLIED` | 告警 | 定义**还没有**写进 Samba 配置，共享此刻不提供访问 |
| `SHARE_DIRECTORY_NOT_EMPTY` | 告警 | 目录里已经有文件，ACL 会覆盖既有权限 |
| `ACCOUNT_PASSWORD_KEPT` | 告警 | 这次没有换密码，沿用上次设置的 |
| `ACCOUNT_LOOKUP_TIMEOUT` | 告警 | 预检没能在预算内确认系统里是否已有同名账号（机器繁忙）；执行阶段会用更长的预算再复核，已存在的账号不会被接管 |
| `SHARE_ACL_REPLACED` | 告警 | 目录原有的扩展 ACL 会被整份替换 |

**秘密通道。** `POST /secrets` 只接受 `purpose`、`value`、`reference` 三个字段（多余字段 400）。
用途是闭集，取值在代理侧登记（`smb_password`、`account_password`、`proxy_basic_password`、
`backup_repository_password`、`notification_password`、`tls_private_key`、`sftp_private_key`、
`app_env`）；未登记的用途与内部用途（`totp_seed`）一律 400。Web 层只挡 `purpose` 的形状，不复制
枚举——复制只会得到两个各自漂移的事实来源。响应只有 `secret_ref`、`purpose`、`state`、
`expires_at`；明文不缓存、不写日志、不回显，任何错误信息里都不含它。秘密引用同样按秘密处理：
日志与审计里 `secret_ref` 的原文会被过滤成 `[redacted]`。

**POSIX ACL。** 目录用 0700 创建，属主之外的访问全部由 ACL 给：每个 `smb_users` 条目一条
`u:<account>:r-x|rwx`，`read_only` 时上限 `r-x`。写入用 `setfacl --set`（整份替换）而不是 `-m`
（增量修改）——增量修改会让**上一次授权过的账号继续留在 ACL 上**，“撤销”就成了空话。写完必须
读回 `getfacl` 并核对两件事：每个期望的条目在，并且 `mask` 覆盖每条期望的权限。`mask` 太窄时
`getfacl` 看起来条目齐全，实际权限却被静默削弱，这是这一层唯一会“看起来成功”的失败方式。
撤销（disable/delete）用 `setfacl -b`，读回时 `mask` 为 `""` 或 `---` 都算已撤销——仍然存在的
mask 才是真正要拒绝的状态。

**观察命令的预算按阶段分。** 预检跑在 Web 等着的链路上（`agentclient` 的一次调用有 10 秒总预算），
所以它的观察命令上限是 3 秒；执行前复核跑在任务自己的预算里，没有人拿着 HTTP 连接等它，同一个
观察命令给 20 秒。这两处**不是**同一个数，也不该被写成同一个数：写成 3 秒，机器忙时会把一次正确的
操作判死；写成 20 秒，预检就会撞穿整条链路（真机数据与推理见 DECISIONS Q222、Q223）。
预检里问不出结果时给 `ACCOUNT_LOOKUP_TIMEOUT` 告警而不是 500，因为“不接管既有账号”这条不变量
由执行阶段兜底：`useradd` 对已存在的名字以退出码 9 失败，执行器据此回 409 `DEPENDENCY_BLOCKED`
并点名 `ACCOUNT_NOT_MANAGED`，绝不继续设置 Samba 密码。

**登记表在代理库。** `shares`（id、name UNIQUE、protocol、volume_id FK→`managed_resources`、
relative_path、enabled、read_only、definition_json、config_hash、directory_prepared、config_applied、
version、last_operation_id、时间戳）与 `managed_accounts`（id、name UNIQUE、type、managed、disabled、
has_password、uid、gid、shell、home、secret_ref、version、时间戳）都在代理自己的数据库里，因为它们
描述的是**这台主机**的状态；Web 侧的 SQLite 保存的是人、会话与任务。两张表**没有密码列、没有
哈希列**，将来也不会加：明文只在秘密通道里流动一次，之后以密文存在代理秘密表里。`shares.volume_id`
是外键，因此一个被共享引用的卷无法被删除（P08.3 会把它变成一句人话的阻塞项，而不是数据库错误）。

### 7.2 P08.2 已实现范围：托管配置、重载与连接状态

P08.2 补上“写入与生效”这一半：面板把共享写进**自己独占的一份托管配置文件**，在主配置末尾
接一行 include，用 testparm 校验候选与合并结果，原子替换并重载，然后读回核对。本阶段新增
一个端点：

| 端点 | 代理查询 | 说明 |
|---|---|---|
| `GET /shares/:id/connections` | `share.connections` | 这条共享此刻的连接状态；形状不对 400、不存在 404 |

**托管配置的架构**（这是本阶段唯一允许改动 Samba 配置的方式）：

- 面板独占的文件是 `/etc/samba/upanel.d/upanel.conf`。所有共享段都写在里面，**整份重写**，
  不做局部编辑——局部编辑要靠文本定位，而文本定位在“别人的文件里”是错的。
- `smb.conf` 只被追加**一行** `include = /etc/samba/upanel.d/upanel.conf`（另加一行说明注释），
  且只追加在**末尾**。Samba 里靠后的 `[global]` 胜出，所以接在末尾能覆盖前面遗留的全局设置；
  接在开头则可能被后面的段落反覆盖。
- 已有的第三方共享与全局设置**一律保留**：面板不改、不删、不移动任何既有行。诊断上提供
  `SHARE_NAME_TAKEN_BY_SYSTEM`（同名段已在既有配置里）与 `SHARE_CONFIG_DRIFT`（托管文件与
  登记表不一致）两条告警，但都不会自动去改别人的配置。

**写入流水线**（每一步都是可断言的，顺序固定）：

1. 渲染托管文件内容（禁用/未登记的共享不渲染）；
2. 写候选文件 `upanel.conf.upanel-new`（`fsync` 到磁盘）；
3. `testparm -s <候选>` —— 候选自己先得过；
4. 备份现行托管文件与主配置（`/var/lib/upanel-agent/smb-backups/`，`<时间戳>-<hash12>.<原名>`）；
5. `rename` 换上去，然后 `fsync` 目录（掉电后 rename 是否持久取决于这一步）；
6. 若主配置还没有 include 行，在同一次文件序列里追加它（备份在前）；
7. `testparm -s <smb.conf>`——**合并后**的主配置还得过（本阶段无条件执行）；
8. `smbcontrol all reload-config`（仅在 smbd 为 active 时）；
9. 读回：托管文件里每个段的 `path` 与 `valid users` 用
   `testparm -s --section-name=X --parameter-name=Y` 核对，而不是回头读我们自己写的文本。

任何一步失败都回滚到备份（配置与 include 一起回滚），并返回闭集里的失败码，不会留下半份配置。

**幂等与重载**。渲染结果与文件里已有的字节完全相同时**不写、不备份、不重载**：报告里是
`file=unchanged`、`reload=skipped_unchanged`。它与 `skipped_service_inactive` 分开记——一个是
“不需要做”，一个是“做不了”；混成一个值，页面就解释不了一次幂等重放为什么没有重载。

**缺 testparm 时仍允许“只删不增”**。检测不到 testparm/smbcontrol 时 `config_write=unsupported`，
但那不等于什么都做不了：如果这次渲染的结果是“只从现有内容里删掉若干段”（前缀与空内容完全
相同、且每一段都逐字出现在旧文件里），就允许写入。理由是不能让一台缺工具的机器**失去停掉
暴露面的能力**，而删行不可能引入语法错误。任何新增或改动都必须照常过 testparm。

**`served` 的定义**（`observed.served`、任务 `data.served`）：托管段写进去了（或本来就在）、
include 接上了、合并后 testparm 通过、smbd 为 active、重载 `done` 或 `skipped_unchanged`。
四条都成立才是 `true`。同时 `config_applied` 只说“这一条共享的段在托管文件里”——它**不等于**
可用：服务没在跑、或重载失败时，配置写对了也提供不了任何访问。

因此执行结果的 `outcome`：`served=true` 时是 `verified`，否则是 `accepted`（配置写下去了、但
此刻没有对外提供访问）。smbd 没在运行时**不**回滚：配置是对的，等它起来就生效，回滚反而会
把一份正确的配置丢掉；报告里用 `reload=skipped_service_inactive`、`served=false` 加一条
`SMB_SERVICE_INACTIVE` 告警说清楚。

**目录权限的两种语义**（真机抓出的缺陷，见 DECISIONS Q233）。共享目录自己按 `0700` 创建，
再由 `setfacl` 把具体账号放进去（先紧后松）；它**上级**的每一级按 `0711` 创建——只有 `x`，
没有 `r`/`w`。原因：共享账号不是上级目录的属主、也不在它的属组里，能不能穿过去只看 other 的
`x` 位；`0700` 的上级会让客户端在 `put` 时拿到 `NT_STATUS_ACCESS_DENIED`，而目标目录的 ACL
一条不差、`testparm` 读回的 `path` 一字不差。早于这条规则建出来的 `0700` 上级在下次应用时被
补成 `0711`，修过哪几级写在 `observed.path_traversal_repaired` 里；预检则以
`SHARE_PATH_NOT_TRAVERSABLE` 告警事先说明，且**不阻塞**——这些目录是面板自己建的，挡住的是
面板自己创建的账号，属于面板该修的东西。

**`allowed_cidrs` 的真实边界**（真机验证，见 DECISIONS Q234）。`allowed_cidrs` 渲染成
Samba 的 `hosts allow`，按来源地址过滤，**但挡不住本机**：`man 5 smb.conf` 的 `hosts allow`
一节写着 "the localhost address 127.0.0.1 will always be allowed access unless specifically
denied by a hosts deny option"。真机上 `allowed_cidrs=["10.0.0.0/24"]` 的共享，本机
`smbclient` 照样列得出目录，而从 `10.99.0.2` 连进来是 `NT_STATUS_ACCESS_DENIED`。
面板不偷偷补 `hosts deny = 127.0.0.1`：那会把面板自己依赖的本地通道一并关掉，是一次没人
要求的语义改变。做法是**说出来**——名单不覆盖环回时给一条 `SHARE_HOSTS_ALLOW_LOOPBACK` 告警。

**并发**：托管文件是整份重写的，因此所有写共享的动作都额外取一把全局锁 `samba:config`
（与 `share:<id>`、`volume:<id>` 一起）。没有这把锁，两个并发的应用会互相擦掉对方的段。

**连接状态**。`GET /shares/:id/connections` 读 `smbstatus --json`，按共享名过滤树连接，
与会话靠 `session_id` 关联（树连接说共享与机器，会话说账号、方言与签名），打开文件数按
`service_path` 归并。返回体：`share_id`、`share_name`、`service`（`active`/`inactive`/`unknown`）、
`connections[]`（`share`、`machine`、`username`、`dialect`、`signing`、`connected_at`、
`open_files`）、`collected_at`、`notices`。

**为什么 `service` 必须和列表一起返回**：真机验证（Samba 4.19.5）——smbd 停掉之后
`smbstatus --json` 仍以退出码 0 返回 `{"sessions":{},"tcons":{},"open_files":{}}`，“没人连着”
与“服务没在跑”在输出上一模一样。只给列表，页面就会把“服务停了”显示成“一切正常”。同理，
smbstatus 缺失时列表为空但带一条 `CONNECTION_STATUS_UNAVAILABLE`，那也不是“没人连”。
字段里所有 id 都是字符串（`"1879008955"`），一律按字符串解析：按整数解析会在换版本时静默
变成零值，而零值会让所有会话关联到同一条连接上。

**能力报告新增字段**（`ShareCapability`）：`service_state`、`service_units`、`managed_config`、
`managed_config_present`、`managed_config_hash`（整份文件的 sha256，与单条 `config_hash` 不同）、
`managed_shares`；`tools` 增加 `smbcontrol`、`smbstatus`。`config_write` 不再恒为 `unsupported`。

**P08.2 追加的闭集码**（与 §7.1 的表同一个闭集，测试按码断言）：

| 码 | 类别 | 含义 |
|---|---|---|
| `CONFIG_NOT_WRITABLE` | 告警 | 配置写不下去：缺 testparm、缺 smbcontrol，或 `smb.conf` 读不到（只能“只删不增”） |
| `TESTPARM_MISSING` | 告警 | 没找到 testparm，新配置无法校验 |
| `SMB_SERVICE_INACTIVE` | 告警 | smbd 没在运行：配置写得对也提供不了访问 |
| `SMB_SERVICE_UNKNOWN` | 告警 | 问不出 smbd 状态，重载因此没做 |
| `CONFIG_GLOBAL_OVERRIDE` | 告警 | 托管文件会覆盖既有的全局设置（`key`/`current`/`desired` 逐项列出） |
| `CONFIG_INCLUDE_UNRESOLVED` | 告警 | 主配置里有指向 include 目录的符号链接或解析不到的路径 |
| `SHARE_CONFIG_DRIFT` | 告警 | 托管文件与登记表不一致（有人手工改过，或上次没走到最后） |
| `SHARE_NAME_TAKEN_BY_SYSTEM` | 告警 | 同名段已存在于既有 Samba 配置里；不允许靠重名覆盖 |
| `CONNECTION_STATUS_UNAVAILABLE` | 告警 | 没有 smbstatus：读不到连接状态，配置与共享本身不受影响 |
| `CONNECTION_STATUS_FAILED` | 告警 | smbstatus 跑了但没给出可用结果 |
| `CONNECTION_STATUS_PARTIAL` | 告警 | 连接列表可信，但打开文件数归并不了 |

执行期失败码（只出现在错误消息与任务里，不进预检阻塞项）：`CONFIG_VALIDATION_FAILED`、
`CONFIG_WRITE_FAILED`、`CONFIG_RELOAD_FAILED`、`CONFIG_READBACK_MISMATCH`、
`CONNECTIONS_READ_FAILED`。

### 7.3 P08.3 已实现范围：卷绑定、开机依赖与运行时脱盘

P08.3 把“共享绑在一个卷上”这句话里隐含的三件事做完：**没挂就不写**、**开机先挂再起服务**、
**运行中掉盘就撤**。它同时补上了存储侧的反向约束——有共享挂在卷上时，卸载与格式化会被
明确挡住，而不是让人把正在对外服务的卷拔走。

**暴露边界是“挂载”，不是“登记”。** 渲染托管配置时逐条重新问卷的现场状态，只有
`observed_state=mounted` 的共享才写进配置；`unmounted`、`missing`、`mismatch`、`error`
一律跳过；`unknown`（问不出来）**同样跳过**。理由是不对称的：少一份共享是可见、可重放、
可修的；多一份指向系统盘空目录的共享是静默误导——客户端连上去看到的是“一个空文件夹”，
而面板上一切正常。跳过的共享名进入 `observed.volume_offline`，并在 `notices` 里给出
`SHARE_VOLUME_OFFLINE`（观测到没挂）或 `SHARE_VOLUME_STATE_UNKNOWN`（没观测成）。

这条判断在**渲染层**而不只在判决层，因为托管文件是整份重写的：“应用 B”会顺带把 A 的段
重写一遍，如果 A 的卷此刻没挂，重写就是一次回归。判决层也照旧拦“卷没挂时应用共享”
（`VOLUME_NOT_MOUNTED`），但那拦不住“预检之后、执行之前掉线”。

**`served` 的前提再加一条**：除了 §7.2 的四条（段在、include 在、合并 testparm 过、smbd
active 且重载完成），执行结束时还会再问一次卷的挂载状态；卷不在挂载状态时 `served=false`、
`outcome=accepted`。写下去的配置本身不回滚。

**开机依赖（systemd drop-in）**。给 `smbd.service` 写一份面板独占的片段：

- 路径 `/etc/systemd/system/smbd.service.d/upanel-shares.conf`，`0644`，目录 `0755`；
- 托管配置 `/etc/samba/upanel.d/upanel.conf` 也是 `0644`，目录 `0755`；
- 这两个权限**与进程 umask 无关**：生产的代理单元带 `UMask=0077`，靠 `OpenFile` 的 `perm`
  只会落成 `0600`（Q243）。文件权限由代码里的常量决定，不由启动它的 shell 决定；
- 内容是 `[Unit]` 段加若干行 `RequiresMountsFor=<挂载点> …`（去重、排序、每行最多 4 个）。
  systemd 把它翻译成对相应 `.mount` 单元的 `Requires=` 与 `After=`：卷挂不上，smbd 不启动，
  而不是启动了去服务一个空目录；
- 写入与托管配置用同一套原语：先写 `*.upanel-new`，再 `rename`，然后 `fsync` 目录；
- 内容与现有文件完全相同就**不写、不重载**；一组挂载点都没有时删掉文件并重载，让 systemd
  忘掉旧要求；
- 写文件之后必须 `systemctl daemon-reload`，否则 systemd 根本不看它。
- **这个片段与托管配置同生同死**：运行时撤销摘掉某条共享的段时，它声明的挂载点也随之少一条；
  最后一条被撤走时整份片段删掉（`startup_deps=removed`）并重载。留着没人用的挂载点等于让
  一块坏掉的盘拖住 `smbd` 的启动。人重新应用共享时片段重新装回来（`written`）。
- **开机拉起这个挂载的是它，不是 fstab**：面板写进 fstab 的条目带 `noauto`（导入卷
  `automount=false`），`local-fs.target` 不会去挂它；真正把 `.mount` 单元拉起来的是
  `RequiresMountsFor=` 生成的依赖。所以“重启后共享可用”= `smbd` 等挂载点 + 挂载点被带起来 +
  `smbd` 启动时路径已经是卷。验收时 `smb.conf` 在重启前后必须逐字节相同：重启不是一次隐式配置变更。

报告字段：`startup_deps` ∈ {`written`, `unchanged`, `removed`, `failed`, `skipped`}、
`startup_paths`（这一次声明的挂载点）。开机依赖失败**不回滚**配置：配置本身是对的，缺的只是
开机顺序这一层保护，用 `STARTUP_DEP_NOT_APPLIED` 告警说清楚。路径含空白/引号、非绝对路径或
非规范形式时整份拒绝——`RequiresMountsFor=` 是按空白切分的列表，一个带空格的路径会静默变成
两条，其中一条永远挂不上。

**运行时脱盘（只撤不恢复）**。代理每 30 秒看一次启用中的共享（`DefaultShareWatchInterval`），
发现某条共享绑定的卷不在挂载状态，就把它的段从托管配置里摘掉（同样的整份重写 + 重载流水线），
并在登记表上记一笔 `runtime_revoked`：

```json
"runtime_revoked": {
  "at": "2026-10-22T13:41:09Z",
  "volume_id": "vol_…",
  "volume_name": "数据卷",
  "volume_state": "unmounted",
  "reason": "卷 数据卷 当前不在挂载状态（observed_state=unmounted）"
}
```

- **只撤不恢复**：卷回来时不自动写回配置。写配置是需要人点头的动作，自动写回等于把一次
  外部故障变成一次静默变更。页面据此提示“重新应用一次”。
- **重新应用即作废**：`share.apply` 成功之后 `runtime_revoked` 被清空——人已经点过那条出路，
  再留着它等于让页面永远显示“已撤销”，而客户端的拒绝理由早已不成立（P08.4 真机抓到，Q247）。
  撤销**只由对账循环写**：卷要是还在离线，下一轮对账会重新记一笔新的现场。
- **幂等**：卷一直离线时，第二轮发现“配置里已经没有这几段、记录上也记过了”，于是不写文件、
  不重载、不动 `updated_at`，并保留**第一次**撤销的时间与原因。
- 撤销循环自己拿 `samba:config` 锁，拿不到就跳过这一轮（那时正有人在应用共享，而那次应用
  本来就会按同一口径渲染一遍全量配置）。
- 卷已经不在受管理卷里（登记表层面的丢失）同样撤销，`volume_state` 写 `not_managed`。

**存储侧的反向约束**。共享是受管理卷的依赖，因此（闭集码见 §6.5）：

| 动作 | 口径 | 阻塞码 |
|---|---|---|
| `storage.volume.unmount` | 只算**启用中**的共享 | `VOLUME_IN_USE_BY_SHARE` |
| `storage.filesystem.format` | **任何**共享都算（含禁用中） | `VOLUME_IN_USE_BY_SHARE` |
| `storage.volume.forget` | 任何共享都算 | `VOLUME_HAS_DEPENDENCIES` |
| 任何一项问不出来 | 一律阻塞 | `VOLUME_DEPENDENCIES_UNKNOWN` |

口径不同是刻意的：卸载只是把它停下来，配置与数据都还在，一条禁用的共享不构成卸载的理由；
格式化之后共享指向的内容就没了，禁用与否一样。退出管理会删掉绑定关系，所以按最严的算。
`VOLUME_DEPENDENCIES_UNKNOWN` 与“没有依赖”必须分开：读不到登记表就证明不了没人依赖它，
而这两个动作一个不可逆、一个会让下次开机没人把它挂回来。

`GET /storage/volumes/:id/blockers` 的 `shares[]` 现在会真的被填上（`{"kind":"share",
"id":…, "name":…}`），并计入 `busy`；依赖查询失败时 `busy=true`——页面上的 busy 直接对应
“卸载按钮能不能点”，把它显示成空闲会把人引到一次注定被拒的操作上。

**P08.3 追加的闭集码**（与 §7.1、§7.2 的表同一个闭集）：

| 码 | 类别 | 含义 |
|---|---|---|
| `SHARE_VOLUME_OFFLINE` | 告警 | 共享绑定的卷此刻没挂：这一次不渲染它的段 |
| `SHARE_VOLUME_STATE_UNKNOWN` | 告警 | 问不出卷挂没挂：按没挂处理，同样不渲染 |
| `STARTUP_DEP_NOT_APPLIED` | 告警 | 开机依赖没写成或没生效（配置本身仍然是对的） |
| `SHARE_REVOKED_VOLUME_OFFLINE` | 告警 | 这些共享的段在运行时因为卷离线被撤销，需要重新应用 |
| `VOLUME_IN_USE_BY_SHARE` | 阻塞 | 卷上还有共享：先停共享（卸载）或先删共享（格式化） |
| `VOLUME_HAS_DEPENDENCIES` | 阻塞 | 卷上还有共享：退出管理会丢掉绑定关系 |
| `VOLUME_DEPENDENCIES_UNKNOWN` | 阻塞 | 问不出面板侧依赖：证明不了没人用它 |

**验证要求**：不能只用 `testparm` 通过当作共享可用（§16）。本阶段的验收必须在独立 Ubuntu
虚拟机里用真实协议客户端验证：卷未挂载时客户端**看不到**那条共享；运行时 `umount` 之后一个
监视周期内共享消失且可再访问性为否；`systemctl show smbd.service` 里能看到
`RequiresMountsFor=`；有共享时卸载/格式化的预检被上面三条码之一挡下。

### 7.4 P08.4 已实现范围：共享页面与客户端连接说明

P08.4 不加接口、不加动作、不改任何既有字段。它做的是把 §7.1–7.3 已经算出来的东西**如实**
摆到人面前，并且把“客户端该怎么连”写成人能照做的一段说明。这一节约束的是**呈现**，而呈现
有它自己的失败方式：把三个状态合成一个绿灯、把“写过配置”说成“现在能用”、把密码写进命令行。

**（1）三个准备状态必须分开显示。** 页面在一条共享上同时给出：

| 字段 | 它回答的问题 | 页面怎么显示 |
|---|---|---|
| `config_hash` | 渲染出来的是哪一版定义（sha256，前 16 位足够区分） | 一行等宽文本；为空说明这份定义从没被渲染过 |
| `directory_prepared` | 目录与 POSIX ACL 是否按这份定义准备**并核验**过 | 独立一行，布尔 |
| `config_applied` | 是否已写进托管配置并重载 | 独立一行，布尔 |

三者**不得**合并成“已生效”这类单一状态（§7.1 已规定，这里是页面侧的对应条款）。它们
失败的后果完全不同：`config_hash` 有值而 `config_applied` 为假是“算过了但没写”，
`directory_prepared` 为假而配置写了是“客户端连得上去但读写会失败”。合成一个状态就把
“我该重新应用”与“我该去修 ACL”变成同一句无用的提示。

**（1b）卷的状态与开机依赖都不在共享记录里，页面自己取。**

`ShareRecord` 只有 §7.1 那十个字段；`volume_state` / `volume_name` / `startup_deps` /
`startup_paths` **都不是**它的字段（openapi 里也没有）。两处各有归属：

- **卷此刻挂没挂**属于卷，不属于共享：页面用 `GET /storage/volumes` 的 `observed_state` 与
  `display_name`，按 `definition.volume_id` 对上来。共享记录里再存一份就是两份会互相矛盾的
  真相。对不上（卷列表没回来、卷已退出管理）时页面**少显示一行观测值**，不写“卷没挂”——
  把未知画成离线会让人去查一个没坏的卷。
  **取到了但值是“不确定”的**（`unknown` / `error` / `mismatch`）也要与 `unmounted` / `missing`
  分开说：页面显示“卷状态读不出来”，不显示“卷此刻没挂”。渲染层对这与离线一视同仁——两种都
  不写配置段（不能拿一个空目录冒充共享）——但给人看的话必须分开：一句话决定他去看盘还是去看
  观测链路。
- **开机依赖**（`startup_deps` / `startup_paths`）属于**那次操作**，落在 `share.apply` 的
  任务结果 `result.data` 里（§7.3 的“报告字段”），不落在共享记录上：它是“这一次写 drop-in
  的结果”，下一轮重载可能变成 `unchanged`，把它固化成记录字段只会让页面显示一个过期判决。

**（2）`runtime_revoked` 单独一块，与 `config_applied` 分开读。** 撤销之后 `config_applied`
就是假的（段真的不在了）；撤销块要额外说清**为什么**、以及**卷当时是什么状态**——这是人
判断“是不是我拔了盘”的唯一线索。页面必须同时显示 `runtime_revoked` 与“重新应用”这个出路；
只显示前者等于把人留在死路上。撤销**不会**因为卷回来而消失（§7.3），页面因此不能说
“等卷回来就好”；反过来，人**重新应用成功之后它必须消失**——出路得真的通，否则页面就在
通告一条永远走不出去的死路（§7.3“重新应用即作废”，Q247）。

**（3）账号载荷里没有密码。** `ShareAccount` 只有 `has_password`（布尔），没有密文、没有
哈希、没有 `secret_ref`（§7.1、openapi）。页面设密码的顺序固定为：明文 → `POST /secrets`
（`purpose=smb_password`）→ `secret_ref` → `share.account.apply`。明文只活到 `POST /secrets`
返回为止；页面在拿到 `secret_ref` 之后立刻清空输入框里的字符串，且**不**把它写进任何状态、
提示、日志或错误信息。

**（4）客户端说明的硬规矩。** 每条共享都能展开一份“客户端怎么连”（Windows 资源管理器 /
macOS Finder / Linux `mount.cifs` / Linux `smbclient`），受四条约束：

- **地址取自面板自己**：`host` 由页面用 `window.location.hostname` 传入，代理不猜、不硬编码。
  代理只有网卡计数器、没有地址表（§3），让它挑一个“对外地址”就是让它猜。说明里同时写明：
  客户端在别的机器上时要换成服务器的内网地址。
- **命令里永远不出现密码**。`mount.cifs` 的 `password=` 会留在进程列表里；面板给的是
  `credentials=<0600 文件>` 的写法。凭据文件的写法也受限：必须用 here-doc 从标准输入写，
  **禁止** `sh -c 'printf "password=…" > f'` —— 那种写法虽然不挂在 `mount` 的 argv 上，
  却把密码留在 `sh` 自己的 argv 里，`/proc/<pid>/cmdline` 一样读得到（DECISIONS Q245）。
- **说明不是“已可用”的证明**：`config_applied` 为假或 `runtime_revoked` 非空时，说明照给，
  但第一条提示必须写明“这条共享此刻没有写在 Samba 配置里，要等它应用之后才能连”。
- **名单管不住本机**：`allowed_cidrs` 非空时，说明里除了原样列出网段，还要带一句“Samba 对
  本机（127.0.0.1）永远放行，这份名单管不住本机”（`SHARE_HOSTS_ALLOW_LOOPBACK` 的同一条事实）。

Windows 与 macOS 两种说明只给地址形状（`\\host\名` 与 `smb://host/名`），不给命令行——它们
没有一条“命令行等价物”能被面板安全地教。

**（5）空列表与“服务没在跑”必须分开说。** 连接列表为空有两种含义（§7.2 的 `service`
字段就是为这个存在的）。页面在此刻没有连接时必须同时显示 `service`：`inactive` 时那句话
是“smbd 没在提供服务”，不是“没人连着”。

**（6）只读可见范围。** 共享页面与“谁正连着”对 viewer 可见（只读），任何应用/停用/删除/
账号变更都要求 admin（§2）。viewer 看到的是一张说明，不是一个能点的界面。

**验证要求**（§16 的追加）：本阶段的验收必须**逐字执行**页面给出的客户端命令，而不是只看
`testparm` 通过——命令文本在 `web/src/api/shares.test.ts` 里与验收脚本互相钉住，改一边
另一边会红；必须扫描代理日志、Web 日志、两个数据库、`/proc/*/cmdline` 与 `/proc/mounts`，
证明密码明文在其中出现 **0** 次；必须以真实协议证明“写用户能写、读用户能读不能写、
名单外账号与匿名被拒”。Windows/macOS 客户端在没有对应测试虚拟机时标 **NOT RUN**，不得
用 Linux 客户端的结果代替。

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

### 8.1 P09.1 已实现范围：元数据、判别式绑定与入口/目标分离

P09.1 只做**元数据**：应用、分组、收藏的增删改查，加上“这条元数据描述的是什么”的判别模型。
没有任何探测、没有任何生命周期动作、没有任何容器或镜像读取——`/apps/discovery`、
`/apps/:id/health`、`/apps/:id/logs`、`/containers*`、`/images`、`/container-networks`、
`/container-volumes`、`/deployments*` **尚未实现**，openapi 里也没有它们（未实现的接口不出现在
契约里，见 §16 与 DECISIONS Q21）。

**（1）四张表，两处数据库边界。** 迁移 `0007_apps.sql` 建 `app_groups`、`apps`、
`app_favorites`、`app_volume_dependencies`。应用标识是 `app_` + 32 位小写十六进制，分组是
`grp_` + 32 位小写十六进制——与 `job_`/`pf_`/`shr_`/`sac_` 同一套形状（§1 的 UUID v4 只用于
前端不解析的外部标识；面板内部标识统一用这种可读前缀，见 DECISIONS Q250）。

`app_volume_dependencies` **没有**指向卷表的外键，因为卷是代理数据库里的对象，Web 侧没有
`volumes` 表，也不该建一个影子副本：影子表迟早与代理的观测不一致，而不一致的副本比没有副本
更危险。替代方案是写入时经 `VolumeChecker`（`GET /storage/volumes`）确认存在——
**代理不可达返回 503 且一个字都不写**（“暂时无法确认”），卷确实不在列表里返回 422
`DEPENDENCY_MISSING` 并带出 `volume_id`。两件事必须分开：前者等一会儿重试就好，后者要换卷。
卷标识落库前用代理侧的 `storage.ValidVolumeID` 校验形状（`vol_` + 32 位十六进制）。

**（2）binding 是判别式联合，落库为规范 JSON，唯一性建在数据库上。**
`type` 决定 `binding` 的唯一合法形状：link → `{}`；systemd → 只允许 `unit`；docker → 只允许
`container_id`；compose → 只允许 `deployment_id`。出现多余键一律 400，并说明是哪个键不属
于这个类型——不猜、不忽略、不“就近匹配”。

`binding_json` 存的是**按键排序、无空格**的规范形式（例如 `{"container_id":"3f4a…"}`），因为
唯一性靠它：`CREATE UNIQUE INDEX uq_apps_binding ON apps (type, binding_json) WHERE type <> 'link'`。
同一个容器/单元/部署不能属于两个应用，这条约束由数据库而不是“先查后写”保证——后者在并发下
必然漏。link 没有绑定对象，因此被排除在唯一索引之外（可以有多条）。表上还有
`CHECK (type <> 'link' OR binding_json = '{}')` 兜住“link 偷偷带绑定”。

`container_id` 必须是**完整** 64 位十六进制（服务端归一化为小写）：容器名会随重建变化，
拿它当绑定依据等于把一条记录系在一个会漂移的字符串上。

**（3）公共入口与内部探测目标是两个字段，不能互相代替。** `public_url` 是给**浏览器**点的
地址（可能是反代后的域名）；`health_check.target_id` 是给**面板自己**用的探测目标。把探测
指向 public_url 会让面板从一个不可信的对外地址上请求自己；把入口写成 target_id 则没人能点。

- `public_url`：只允许 http/https，禁止 `javascript:`/`data:`/`file:`，禁止含用户名密码；
  生产模式下回环地址（`127.0.0.0/8`、`localhost`、`::1`）与通配地址（`0.0.0.0`、`::`）被拒——
  那是“面板自己”而不是“用户能访问的入口”。演示模式（`ModeMock`）放宽回环限制，因为演示实例
  本来就在本机（Q249）。link 类型必须填 `public_url`。
- `health_check.target_id` **结构性**地只能是 `^(local|cntr)_[a-z0-9][a-z0-9_-]{0,47}$`：
  主机名、IP、端口、完整 URL 在这个形状里根本表达不出来，所以“填一个 `169.254.169.254`
  让面板去请求”这件事不是“会被校验拦下”，而是**没有地方填**。这一条是 SSRF 的正面约束，
  不是事后过滤（§3 的同一原则：探测目标只能来自代理发现）。

**（4）健康检查的默认值与语义。** `enabled=false` 时也要输出完整的 health_check 对象
（字段集合固定，读回来不需要猜缺了哪个键），此时 `target_id` 为空、`port` 为 0。`enabled=true`
时 `target_id` 与 `port` 必填；`scheme` 默认 `http`、`path` 默认 `/`、
`interval_seconds` 默认 30（范围 30—300）、`timeout_seconds` 默认 3（范围 1—10）。
`path` 只能以单个 `/` 开头且不得含 `://`。`expected_status_codes` 去重后升序、最多 8 个、
范围 100—599；**空数组表示“默认 200—399”**，落库为空数组而不是 `null`（`null` 与空集合在
JSON 里是两种意思，这里两者语义相同，存储不该因为写法差别而不同）。

**（5）摘要是诚实的“不知道”。** P09.1 没有任何探测器，所以：

| 字段 | link | systemd / docker / compose |
|---|---|---|
| `runtime_state` | `not_applicable` | `unknown` |
| `http_state` | `not_applicable`（未启用健康检查时所有类型都是这个） | `unknown`（仅在 `enabled=true` 时） |
| `last_checked_at` | `null` | `null` |
| `dependency_protection` | `not_applicable` | 有依赖卷时 `unmanaged`，否则 `not_applicable` |

绝不出现“没探测过所以填 `stopped`”：那会把人骗去点一个不必要的启动按钮，也会让“服务真挂了”
与“还没看过”变成同一句话。`dependency_protection` 在 P09.1 里**只可能是** `unmanaged`——
还没有任何接管动作，写 `managed` 就是撒谎。

**（6）`capabilities` 说的是“此刻真的能做到的事”，不是“这个类型将来能做什么”。**
`open = public_url 非空`；`start`/`stop`/`restart`/`logs` 在 P09.1 全部为 `false`，等
P09.3/P09.4 真正接上预检 + 任务之后再逐个放开。这条机制存在的理由是：先渲染按钮再让点击
全部失败，比不显示按钮更糟——前者教人不再相信这个页面。

**（7）删除只删元数据。** `DELETE /apps/:id` 只删除入口与绑定记录：不卸载项目、不停止服务、
不删容器、不删镜像、不碰依赖卷里的数据；收藏随应用级联删除。`DELETE /app-groups/:id` 在分组
非空时返回 422 `DEPENDENCY_BLOCKED`，不会顺手把应用移出分组或一起删掉——一个“删分组”的动作
不该改变别的记录。要删就先 `PATCH /apps/:id` 带 `group_id: null`。

**（8）权限、CSRF、审计与版本。** 读 V；写 admin + CSRF。`PATCH`/`DELETE` 都必须带
`version`，不符返回 409 `VERSION_CONFLICT`。`group_id` 在 `PATCH` 里用“显式 `null` = 移出
分组”，与“没提供这个字段”是两件事；改 `type` 必须同时给 `binding`。

审计动作：`app.create`、`app.update`、`app.delete`、`app_group.create`、`app_group.update`、
`app_group.delete`，target 形如 `app:<id>` / `app_group:<id>`；元数据变更与审计**同事务**。

`PUT /apps/:id/favorite` **不写审计**：收藏是当前用户自己的偏好，不是管理动作；每点一次星标
就产生一条审计，真正的管理事件会被淹掉。它也**不需要** `version`（幂等）。


### 8.2 P09.2 已实现范围：本机发现、候选与健康探测

P09.2 补上 §8 表里的两行：`GET /apps/discovery`（admin）与 `GET /apps/:id/health`（V）。
**候选的“确认”不是一个新接口**——确认就是 §8.1 的 `POST /apps`，用候选的 `target_id` /
`unit` / `container_id` 当绑定。再开一个 `/apps/discovery/:id/confirm` 等于让“创建一个应用”
有两条代码路径，而其中一条迟早会和另一条不一致。

仍未实现：`/apps/:id/logs`、`/containers*`、`/images`、`/container-networks`、
`/container-volumes`、`/deployments*`（P09.3 起）。openapi 里没有它们。

**（1）发现发生在代理里，Web 只做合并。** 三路来源：

| 来源 | 依据 | 失败时怎么说 |
|---|---|---|
| `systemd` | `/proc/net/tcp{,6}` 中 `st=0A` 的监听 → inode → 持有进程 → cgroup v2 `0::` 路径 → `.service` 单元；与 P05.1 的服务快照同一个采集者 | “systemctl 读不出来” |
| `local_listen` | 同样的监听表，但背后没有能认出来的单元（脚本、screen/tmux、cgroup 里没单元名） | 归入上一条 |
| `docker` | `docker ps -a --no-trunc --format` | 分开说“没装 docker”/“docker 没在跑” |

快照在代理侧缓存 15 秒（多个页面同时打开不等于每 15 秒采一次）。`status` 是
`ok`/`partial`/`error`：全部来源失败时是 `error`——这份结果**不能**被读成“本机什么都没有”。
单个来源失败不影响其它来源，各自带 `status` 与 `error`。

**（2）`target_id` 由代理分配，地址由代理计算。** `local_<slug>` / `cntr_<slug>`，形状与
§8.1 的 `health_check.target_id` 完全一致。slug 只是给人看的提示（重名时加序号），**不是
身份**：真正的身份是候选出现在这次发现结果里。

探测地址永远是 `http(s)://127.0.0.1:<port><path>`，由代理自己拼。调用方能给的只有
`target_id`、`scheme`、`port`、`path`、`expected_status_codes`、`timeout_seconds`——**没有
host 这个字段**。于是 `169.254.169.254`（云元数据）、内网其它主机、任意域名在这条路径上
没有出现的位置：不是被过滤掉，是结构上不存在（与 §3、Q21 的同一原则）。

探测客户端：`Proxy: nil`（不吃环境代理，否则 `127.0.0.1` 会被转发出去）、不跟随重定向
（3xx 就是 3xx，`Location` 指向哪儿与结论无关）、TLS 验证保持系统根证书、响应体最多读
64 KiB 后丢弃（健康检查只看状态码）。

容器的 `port` 必须是**这次发现里它真的发布出来的宿主机端口**：`docker ps` 没显示
`0.0.0.0:9000->80/tcp` 之类的映射时，探测被拒并说明已发布的是哪些——而不是去连一个
容器网络的地址（那等于绕过上面的结构性约束）。docker-proxy 在已发布端口上的监听不会额外
产出一个 `local_…` 候选：那是同一个服务的第二次曝光，不是第二个服务。判定发生在**单元分类
之前**——docker-proxy 跑在 `docker.service` 的 cgroup 里，先分类的话它会变成“docker.service
在监听 19487”，于是同一个容器在候选里出现两次（容器 + 那个单元）。摘的时候只摘发布端口上
的那几条监听：`dockerd` 自己的 2375/2376 仍然会作为单元候选留下。

同一条道理用在 pid 1 上：`.socket` 单元（`ssh.socket`、`rpcbind` 的 111 等）的监听套接字
由 systemd 代持，守护进程起来后自己也持有同一个端口，套接字表里因此出现两个 pid。候选里
**同一个监听（地址 + 端口 + 协议）只归属那个有人认领的进程**；pid 1 **独占**的端口仍然是
候选，原因写“systemd（pid 1）正在这个端口上等待连接，背后是一个还没被激活的 socket 单元”
——那是真实状态，不是噪音。否则确认候选的列表里会并排出现两条“端口 22”，而哪个是 sshd
只能靠猜。

`ProbeResult.Valid()` 兜住“半条结论”（缺 target/scheme/port/path）：调度器把它记为
`unknown` 而不是拿定义里的值补齐——补齐会把“不知道探的是哪儿”写成“探过了”。

**（3）探测的三种结局，`unknown` 不是 `error`。**

| `http_state` | 含义 | `address`/`url` |
|---|---|---|
| `ok` | 拿到期望集合里的状态码（空集合 = 默认 200—399） | 有 |
| `error` | 连不上、超时、TLS 失败、状态码不在期望集合里；或**看成了但目标不在**（服务停了、容器没了、单元不再监听） | 前三种有；目标缺席时为空（没有发出请求） |
| `unknown` | 这一轮没有得出可用结论：代理不可达、发现本身没成功、或装着这个目标的来源这一轮没采成 | **空** |

“我看不到”和“它坏了”是两件事：前者等一会儿就好，后者要人去修。没有发出请求时
`address`、`url` 是空串、`status_code` 是 `null`（不是 0——0 不是状态码），落库与接口
都照此输出。

**目标缺席按快照状态分开记**，因为同一个物理状态（服务停了）不能因为缓存新鲜度不同
而一会儿 `error` 一会儿 `unknown`：

- 快照 `status=error`（一个来源都没采成）→ `unknown`：这一轮**没有看成本机**，不能据
  此说目标不在。
- 快照 `status=partial` 且**装着这个目标的来源**失败（`cntr_…` 看 docker、`local_…` 看
  systemd 与裸监听）→ `unknown`，原因里点名是哪个来源。
- 其余情况（看成了，目标就是不在）→ `error`，原因写“不在本机当前的监听/容器列表里”。
  写成 `unknown` 会让一个已经挂掉的站点永远显示“暂不确定”。

**（4）调度在 Web，执行在代理。** `app_health_results`（迁移 `0008`）一行一个应用，只存
**结论**；定义仍在 `apps.health_json`。分开的理由：定义是用户写的，结论是面板观测到的，
混在一行里会出现“定义被改成 8080 而结论还写着 200 OK，其实是上一个端口的结果”。

调度节拍：每 5 秒扫一次“到点的应用”（从没探过，或距 `checked_at` 已过该应用自己的
`interval_seconds`，下限 5 秒）；一轮总时限 60 秒。代理不可用时的处理是**写一条
`unknown` 并结束本轮**，而不是把剩余应用逐个写一遍同样的原因——日志里如实写明“剩余 N 个
目标本轮未探测”，因为它们的 `checked_at` 没有前进，界面读到的仍是上一次结论。

Web 进程**不发**任何 HTTP 请求。它要是能出站，那它就成了一个能访问任意地址的东西，而
“不能访问任意地址”正是这一层的安全边界。

**（5）写入时确认目标存在（与 §8.1 的卷检查同一形状）。** 带 `enabled=true` 的
`health_check` 写入时（创建总是；`PATCH` 仅当请求体里带了 `health_check`）：

- 代理不可达 → **503 `AGENT_UNAVAILABLE`**，本次一个字都不写；
- 目标不在代理当前的发现结果里 → **422**，`details.target_id` 指出是哪一个。

只做形状校验会让一个手写的 `local_foo` 被存下去，然后每次探测都报“目标不存在”；只问存在
性而不校验形状，则没法解释为什么 `169.254.169.254` 从来不被接受。两步缺一不可。

`PATCH` 只在带了 `health_check` 时才检查：否则一次改名会因为这个目标恰好在那一刻重启而
失败——那是把一个与本次修改无关的瞬时状态，变成了保存失败的理由。

**（6）候选与绑定合并：只看身份，不做名字匹配。** `candidates[].bound_app_id` 非空表示
该目标已被某个应用登记（`uq_apps_binding` 保证至多一个）。systemd 用单元名、docker 用
**完整容器 ID** 匹配。

`missing_bindings` 列出现在还绑着、但发现结果里已经没有的对象，原因分开写：

- 容器 ID 不在本机容器列表里 → “容器可能被重建过；重建出来的是新记录，需要重新确认后再绑定”。
  面板**不会**因为容器名相同就自动把旧应用接到新容器上——那是一个没人确认过的绑定。
- 单元没有出现在候选里 → “这个单元当前没有在监听任何端口”。它可能只是被停用，也可能改名了，
  所以这里只说“看不到候选”，不说“已删除”。

**这个判断以“对应来源这一轮真的采到了”为前提。** `sources` 里 `docker`（或 `systemd`）不是
`ok` 时——没装 docker、守护进程没跑、`docker ps` 失败、服务状态还没完成第一次采集——绑定
**不进** `missing_bindings`：没看成本机就等于没说，`partial` 与来源自己的 `error` 已经写在
快照上，那里才是该看的地方。反过来说，把一个没做的检查写成“这个容器不在了”，用户会照着
这句话去删一条其实好好的应用。这一条与探测侧 `unknown`/`error` 的分法（§8.2（3））是同一条
道理，两处必须一致。

**（7）权限与“确认”的动作边界。** 候选列表是 admin（暴露本机全部监听端口与容器，是侦察
材料）；健康结论是 V（只说明这个入口通不通）。`link` 类型没有启停按钮（§8.1 的
`capabilities`），`start`/`stop`/`restart`/`logs` 仍然全为 `false`，等 P09.3/P09.4 接上
预检 + 任务后再逐个放开。

没有发现读取端的部署（演示模式）与代理不可达一样回 **503 `AGENT_UNAVAILABLE`**：空候选
列表会被读成“这台机器上没有任何服务”，那是一句假话。

DeploymentDefinition：name、image_reference、container_port、host_bind_address、host_port、environment（公开值或 secret_ref）、mounts、resource_limits、health_check、volume_dependencies。
mounts 仅允许 source_root_id、relative_path、container_path、read_only；禁止 .. 和 /proc、/sys、/dev 等目标敏感路径。镜像引用允许 tag 或 digest，执行后记录实际 digest。资源限制初期必填 memory_limit_bytes、cpu_limit；端口 1—65535，冲突检测和锁必须同时存在。definition 不含原始 compose_yaml 字段。

环境变量名称按 POSIX 标识符校验；secret_ref 只能用于 app_env。渲染配置 root 独占、0600，读取 API 返回“已设置”。禁用 Docker restart，生命周期经包装 systemd 单元管理。

### 8.3 P09.3 已实现范围：容器只读查询、日志与受支持容器的启停

P09.3 补上 §8 表里的六行：`GET /containers`、`GET /containers/:id`、`GET /containers/:id/logs`
（含 `/containers/:id/logs/stream`）、`GET /images`、`GET /container-networks`、
`GET /container-volumes`、`GET /deployments`、`GET /deployments/:id`，外加一个写动作
`container.control`（走预检 → 任务，见 §5）。

仍未实现：`/apps/:id/logs`（P09.4）、任何 Compose 写操作与 `DeploymentDefinition` 的落地
（1.0）。`/deployments*` 是**只读观测**，不是项目管理。

**（1）只认完整容器 ID，因为它是 argv 的一部分。** 容器 ID 必须是 64 位小写十六进制。
名字、12 位短 ID、大写、缺位一律 400，在 Web 与代理两层各拒一次。理由不是格式洁癖：代理
真正执行的是 `docker <verb> <id>`，而 argv 里每多一个可写的字符串就多一条把 docker 变成
“任意命令执行器”的路。名字还会随重建变化（§8.1 已按同一理由只认 ID）。

**（2）Web 进程永不接触 `docker.sock`。** Web 侧没有 docker 客户端、没有 socket 路径、
没有命令行拼接；它只把结构化查询与参数交给代理。代理侧的 docker 调用是**固定 argv**：

| 用途 | argv（除了被校验过的容器 ID） |
|---|---|
| 列容器 | `ps -a --no-trunc --format <固定格式串>` |
| 详情 | `inspect --type container --format <固定格式串> <id...>`（每批最多 200 个 ID） |
| 日志 | `logs --timestamps --tail N [--since] [--until] <id>` |
| 跟随 | `logs --follow --tail 0 --timestamps --since <t> <id>` |
| 启停 | `start <id>` / `stop --time 10 <id>` / `restart --time 10 <id>` |
| 镜像/网络 | `images --format`、`network ls --format` |
| 卷/项目 | `volume ls`、`docker compose version`（只探“有没有插件”，不用它做管理） |

docker 可执行文件走绝对路径探测（`/usr/bin/docker`、`/snap/bin/docker`），套接字固定
`/var/run/docker.sock`。没有“把任意 docker 子命令转发过去”的接口。

**（3）危险容器显示风险，不接管。** 风险是**闭集**，命中任意一条即 `risk.level=high`、
`capabilities.managed=false`、三项能力全 false：

| 码 | 判据 |
|---|---|
| `privileged` | `HostConfig.Privileged` |
| `host_network` / `host_pid` / `host_ipc` | `NetworkMode=host` / `PidMode=host` / `IpcMode=host` |
| `docker_sock_mount` | 有 bind 挂载的源是 `docker.sock` |
| `host_root_mount` | 有 bind 挂载的源是 `/` |
| `dangerous_capability` | `CapAdd` 含 `CAP_SYS_ADMIN`、`CAP_SYS_MODULE`、`CAP_SYS_PTRACE`、`CAP_SYS_RAWIO`、`CAP_NET_ADMIN`、`CAP_DAC_READ_SEARCH`（带不带 `CAP_` 前缀都算） |

拒绝发生在**代理里两处**：预检（`allowed=false` + `CONTAINER_NOT_MANAGED` 阻塞项）与
执行前的复核（不再允许就拒绝，理由同样是 `PREFLIGHT_STALE` 那一类——对调用方来说结局都是
“重来一次预检”）。界面隐藏按钮不是保证，代理的判决才是。

状态不适用的操作是 `CONTAINER_STATE_CONFLICT`（对 stopped 容器 stop、对 running 容器
start）。属于 compose 项目的容器**不阻塞**，但带 `COMPOSE_MEMBER` 告警：单独启停它会让
项目状态与标签和你看到的不一致。

**（4）不请求的东西，就不存在泄漏的可能。** 容器的 `Config.Env`、完整标签表、原始
`inspect` 输出**从未被读取**：`inspect --format` 的格式串里只有 ID、名字、镜像、状态、
时间、重启次数、重启策略、端口、挂载、Resources、Risk 判定所需的几项，以及**六个**
`com.docker.compose.*` 标签。环境变量里通常就是数据库口令与令牌；其余标签是第三方自己写的，
也可能含秘密。日志内容走与 §10.3 同一套 `logging.Redact` 脱敏。

**（5）两种“不可用”必须分开说。**

| 状况 | 表现 |
|---|---|
| 代理连不上 / 超时 / 响应不合法 | 503 `AGENT_UNAVAILABLE`（等一会儿会好） |
| 本机没有 docker 命令，或守护进程没在跑 | 列表类接口 200 + `available=false` + `notice`；`/containers/:id` 与日志 503 `DOCKER_UNAVAILABLE` |
| 套接字还在、守护进程没应答（dockerd 被杀/被停，socket 文件残留） | 同上：列表类接口 200 + `available=false` + `notice`；详情与日志 503 `DOCKER_UNAVAILABLE`。判据不是“套接字在不在”，而是 docker 自己那句 `Cannot connect to the Docker daemon` |
| 同上，发生在 `container.control` 预检 | 200 + `allowed=false` + 阻塞码 `DOCKER_UNAVAILABLE`（见下） |

把两者合成一句“容器不可用”会让人去修错的东西（面板/代理 vs Docker）。

这条边界在 Web 侧也必须守住：代理回的 5xx **不能只按状态码分类**。带域内码的 5xx
（`503 DOCKER_UNAVAILABLE`、`409 PREFLIGHT_STALE`…）原样透出，只有通用码
（空、`AGENT_UNAVAILABLE`、`INTERNAL_ERROR`）和连不上才翻成 `AGENT_UNAVAILABLE`。
真机上就是这里漏的：dockerd 停掉后读详情，状态码 503 是对的，码却被改写成了
`AGENT_UNAVAILABLE`，页面于是说“等代理一会儿”，而用户该做的是把 dockerd 起起来。

预检不返回 503：`DOCKER_UNAVAILABLE` 是**拒绝理由**而不是“预检算错了”。预检层把 Plan 的
error 当实现失败（500），所以代理在 `AssessControl` 里把它写成 `allowed=false` + 阻塞项，
文案说清是“没有 docker 命令”还是“守护进程没在跑”。提交这种预检得到 409（预检结果为不允许）。

**（6）日志翻页是时间键集，不是游标。** docker 没有游标。`next_cursor` 是本页**最旧**一条
的时间戳，下一页把它当 `cursor` 传回，语义是“看比它更早的”。

往回翻**不能**用 `docker logs --tail N --until <游标>`：docker 先取整个流的最后 N 行，
再按 `--until` 过滤，比游标早的行在过滤前就被 tail 丢光了（真机上第二页只剩边界那一条，
再被去重掉就成了空页）。往回翻用时间窗 `--since … --until 游标`：

- 窗口初始 15 分钟；一页没凑满就把窗口放大 8 倍，直到窗口铺到**容器创建时间**
  （容器创建时间由一次 `docker inspect --format {{.Created}}` 取得，作为“更早的确实没有了”
  的可靠下限），或放大到 30 天上限；
- 单次 `docker` 的输出有 8 MiB 上限，撞上就说明窗口太大，于是把窗口缩小 4 倍重试
  ——被截掉的正是窗口末尾（最接近游标）的行，缺了它们的页没有意义；
- `truncated` 只在“满一页”或“单页 1 MiB 上限到了”时为 true；窗口覆盖到容器创建时间时
  是 false（更早的确实不存在，而不是含糊地说“可能还有”）。

同一时间戳可能同时落在两页边界，服务端丢掉“不比游标更新”的条目。限额沿用 §10.3：
单页 1000 行 / 1 MiB、单行 8 KiB 截断；跟随流 10 分钟 / 60 秒空闲 / 5000 条 / 8 MiB，
与日志流共用“每账号 4 条同时流”的额度。

结束原因是闭集：`duration_limit`、`idle_timeout`、`entry_limit`、`byte_limit`、
`container_exit`（容器停了）、`client_closed`。**开流前**先确认容器存在（否则 404）与日志
驱动可读（否则 422）——先回 200 再发一条 error 帧，会让“容器不存在”看起来像一条断掉的流。

**（7）数据卷只读清单，不做 `volume inspect`。** 卷名是用户字符串（不进 argv），而
`mountpoint` 是宿主路径。`in_use_by` 由容器的 `Mounts` 归并而来，这比再跑一次 inspect 更
贴近“谁在用”这个问题，也少一条把用户字符串送进命令行的路。

**（8）compose 项目是观测结果。** `GET /deployments` 不从磁盘读 compose 文件、不执行
`docker compose`，只按容器标签归并。`id` = `dep_` + `sha256("compose:" + 项目名)` 前 32 位
十六进制——由名字派生，因此项目被整体删除后不会突然指向另一个项目；`GET /deployments/:id`
按这个 id 精确匹配，拿不到就是 404，**不按项目名模糊匹配**（那会把“名字改过”说成“找到了”）。
`capabilities` 的 `apply`/`update`/`remove` 三项固定为 `false`，这不是占位：0.1 确实不会去
执行外部 Compose YAML。

**（9）权限与写操作边界。** 整组 admin：容器清单等于“这台机器上跑了什么、发布了哪些端口、
挂了哪些目录”，与日志同理属于侦察材料，不给 viewer。这里**没有**“直接启停”的路由：写操作
只有 `container.control`，经 `POST /operations/preflight` → `POST /jobs`。该动作不要求再认证
（与 `service.control` 同一个判断：高频运维动作每次都输密码会让人习惯性跳过验证）、不可取消。
不删除容器、不删除镜像、不清理卷数据、不接管危险容器、不执行外部 Compose。

**（10）验收状态。** docker 语义（启动竞态、日志驱动差异、compose 标签形状）的真实证据只在
独立 Ubuntu 测试虚拟机上取得（`tests/integration/p09-3-container-acceptance.sh`）；单测证明的
是参数、判决与映射，不能当成“真机上的容器功能已验收”。

### 8.4 P09.4 已实现范围：运行状态、关联日志与依赖卷门禁

P09.4 补上 §8 表里的最后一行 `GET /apps/:id/logs`，并把 `/apps`、`/apps/:id` 里四个
“还没有观测”的摘要字段真正接上观测：

- `runtime_state` 不再恒为 `unknown`：systemd 读单元状态，docker 读容器状态；
- `capabilities` 的 `start`/`stop`/`restart`/`logs` 来自底层对象此刻的能力，不再恒为 `false`；
- `dependency_protection` 可以真的是 `managed`；
- 新增两个**可空**字段 `runtime_note` 与 `dependency_protection_note`，把“为什么是这个值”
  写在同一行里。可空而不是空串：`null` 是“没有话要说”，空串会让页面显示一行空白。

**（1）摘要来自观测，不来自类型。** 三个来源、三套映射：

| 类型 | 运行状态来源 | 映射 |
|---|---|---|
| link | 无运行时 | 恒 `not_applicable` |
| systemd | 服务快照里该单元的 `active_state` | `active`/`reloading` → `running`；`failed` → `failed`；`activating`/`deactivating` → `unknown`（过渡态说成任何一头都是撒谎）；`inactive` 等 → `stopped` |
| docker | 容器清单里该 ID 的 `state` | `running` → `running`；`stopped` → `stopped`；`unknown`（restarting/paused）→ `unknown` |
| compose | —— | `unknown`（部署能力 1.0 才有） |

`unknown` 有五种来由，每一种都必须写进 `runtime_note`，因为它们要人做的事完全不同：这一轮
没读到（代理不可用 / 服务快照采集失败 / Docker 不可用）、单元或容器不在本机列表里、
`load_state=not-found`、过渡态、`compose` 未落地。“读不到”永远不会被写成 `stopped`——
那会把人骗去点一个本来在跑的东西的启动按钮。

docker 的 `failed` 在这一版不出现：判据是退出码，而 `Container` DTO 里没有它。崩退的容器
显示为 `stopped`，`state_text`（Docker 原始状态串，形如 `Exited (1) 2 minutes ago`）
仍然在 `/containers/:id` 里可读。这一点写在这里而不是含糊过去：`runtime_state` 的闭集
里有 `failed`，但在 docker 一侧它此刻取不到。

**（2）能力来自底层对象，不来自类型。** systemd 的三项取自 `services.Unit.capabilities`
（已包含状态与保护判定：active 不出现 start、受保护单元为空数组），docker 的三项取自
`containers.Container.capabilities`（高风险容器三项全 false）。Web 侧不重算一遍——两份规则
迟早会分叉，分叉之后按钮就会亮在代理必定拒绝的动作上。

`logs` 与三项生命周期能力**分开判定**，因为读日志不是写操作：受保护的单元（面板自己、
SSH、核心服务）和判定为高风险的容器都允许读日志，`/logs` 与 `/containers/:id/logs`
本来就是 admin 只读。反过来，单元文件读不出来（`not-found`/`error`）或绑定的容器 ID 不在
本机列表里时不给 `logs`：那时点下去只会得到空页或 404。

**（3）绑定对象消失要求重新确认，不做名字匹配。** 容器 ID 不在本机容器列表里 →
`runtime_state=unknown` + `runtime_note` 写明“容器可能已被重建，请重新确认绑定”；
单元不在服务列表里 → 同一形状的说法（可能改名或被卸载，不说“已删除”）。面板**不会**
因为新容器的名字和旧的一样就把应用接过去——那是一个没人确认过的绑定，与 §8.2（6）同一个判断。

**（4）`dependency_protection` 的准确含义。** `managed` 说的是：
**面板是受管理启动路径上唯一的自动启动者，而且那条路径上有实测的依赖卷门禁。** 它不声称
能阻止管理员在 shell 里手工 `docker start` 或 `systemctl start`——面板看不到那些动作，
把它们算进 `managed` 就是撒谎。

| 情形 | 值 | note 说什么 |
|---|---|---|
| link，或没有依赖卷 | `not_applicable` | （无） |
| docker，`restart_policy` 是 `no` 或空（Docker 默认） | `managed` | 容器不会自己启动，经面板发起的启动会被依赖卷门禁拦住 |
| docker，`restart_policy` 是 `always`/`unless-stopped`/`on-failure` | `unmanaged` | 开机或崩溃后由 Docker 自行拉起，不经过面板的检查 |
| systemd（任何情况） | `unmanaged` | P09.4 没有改写单元文件：`enabled` 的单元开机会被 systemd 直接拉起；被别的单元拉起时同样不经过面板 |
| compose | `unmanaged` | 部署未落地，接管不了启动顺序 |
| 单元/容器状态这一轮没读到 | `unmanaged` | 判断不了它会不会在卷挂载之前自己起来 |

**（5）启动门禁：Web 补代理看不到的那半边。** 依赖是“哪个应用吃哪个卷”，只存在 Web 库里；
代理那边只有共享的依赖，它不知道也不能知道应用元数据。所以门禁加在 Web 的预检之后：

- 只对 `service.control` 与 `container.control` 且 `operation` 是 `start`/`restart` 生效；
- 按**绑定**反查应用（`type` + 规范化 `binding_json`），查不到就不拦——面板不认识的
  单元/容器没有依赖关系要守，拦它等于“不认识的一律不许动”；
- 逐个核对依赖卷的 `observed_state`，命中即追加阻塞项；
- **只能收紧**：代理说 `allowed=false` 的预检不会因为门禁没意见而变成可执行；门禁自己故障
  （数据库读不出依赖关系）时预检整体失败（500），不做“当它没拦”；
- 落库的是收紧后的 `allowed=0`，于是 `POST /jobs` 得到 **409 `DEPENDENCY_BLOCKED`**。

追加的闭集阻塞码（与 §7.1、§7.3 的表同一个口径）：

| 码 | 级别 | 含义 |
|---|---|---|
| `VOLUME_OFFLINE` | 阻塞 | 依赖卷当前未挂载（含 `observed_state` 与卷自己的 `observed_error`） |
| `VOLUME_MISSING` | 阻塞 | 依赖卷不在受管理卷列表里（已删除或退出管理） |
| `VOLUME_IDENTITY_CHANGED` | 阻塞 | 卷在，但文件系统 UUID 与登记的不一致（可能换了一块盘） |
| `VOLUME_CHECK_UNAVAILABLE` | 阻塞 | 无法确认依赖卷状态（代理不可用）——**没检查不等于检查通过** |

`stop` 不在门禁范围内：停机在任何状态下都安全，拦它只会让一个起在空目录上的应用没法被停下来。

**（6）`GET /apps/:id/logs` 只做判别，不新增读取能力。** systemd 应用走与 `/logs` 同一条
读取链（`journalctl -u <绑定单元>`），docker 应用走与 `/containers/:id/logs` 同一条链。
响应带判别式 `source`（`journal`/`docker`）与对应的 `unit`/`container_id`，`page` 的字段就是
那两个接口的同一套。

**对象来自绑定，不来自请求。** 请求里的 `unit`、`source` 一律 400——否则这个接口就是
“按任意单元名读日志”的通用入口，绕过 `/logs` 自己的边界；两个来源的参数集合也不互相串味
（journal 只收 `level`/`from`/`to`/`cursor`/`limit`，docker 只收 `since`/`until`/`tail`/`cursor`），
传错集合得到 400 而不是被静默忽略。

状态码：admin（日志里什么都可能有）；`link` → 422（没有运行时就没有日志）；`compose` → 501
`NOT_IMPLEMENTED`（不返回空 page 冒充成功）；绑定的容器已不存在 → 404 `CONTAINER_NOT_FOUND`
（原样带出代理的判决）；代理不可用 → 503 `AGENT_UNAVAILABLE`。

**（7）仍未实现，也没有用别的东西假装实现。**

- **运行时卷失联自动停止受管理服务/容器**（PLAN.md 第 11 节关键依赖规则 5 的后半句）。
  那需要在代理侧起一个常驻协调器并给它停机权限；P09.4 交付的是启动门禁与状态显示。
  在它落地之前，“卷在运行中被拔掉”这件事的表现是：应用页面上的依赖卷变成 `unmounted`，
  下一次经面板发起的启动被拦；已经在跑的服务不会被面板自动停掉。
- **写入 `RequiresMountsFor`/`BindsTo`、包装 systemd 单元、改容器 restart 策略**。
  这三件事都会改写宿主上已有的单元文件或容器配置，属于比“显示”重得多的动作，
  没有在 P09.4 的范围内，因此 systemd 应用的保护状态一律是 `unmanaged`。
- **compose 部署的生命周期**（1.0）。`/deployments*` 仍然只是只读观测。

**（8）验收状态。** 运行状态映射、门禁判决与日志判别的真实证据来自独立 Ubuntu 测试虚拟机
（`tests/integration/p09-4-app-runtime-acceptance.sh`）；单测证明的是映射、门禁与参数边界，
不能当成“真机上依赖保护已验收”。

### 8.5 P09.5 已实现范围：页面、卡片与分桶规则（无新接口）

P09.5 **没有新增或修改任何接口**。这一节存在的原因是：页面把一些规则从服务端搬到了前端，
那些规则同样是契约的一部分——它们决定“用户看到的按钮点了会发生什么”。

**（1）页面不自己算状态，也不自己造动作。** 卡片上的每一个按钮都对应 `capabilities` 里的一个
`true`；`false` 的画成不可用并说明原因，缺省的（老载荷）一律不画。启停请求只落成**既有的**
`service.control`（`{unit, operation}`）与 `container.control`（`{container_id, operation}`）——
**不存在也不该存在 `app.control`**：依赖卷门禁挂在上面那两个动作名上（§8.4(5)），
多一条“应用控制”的捷径等于给门禁开一个后门。

**（2）`open` 是锚点，不是请求。** `public_url` 非空时渲染成
`<a href target="_blank" rel="noopener noreferrer">`；为空时卡片写理由而不是画一个点了没反应的
按钮。`public_url` 与 `target_id` 的分工见 §8.1(3)：入口是给浏览器点的，探测是面板自己做的。

**（3）三个状态分开显示，任何一格读不到就说读不到。** `runtime_state`（底层对象在不在跑）、
`http_state`（入口通不通）、`dependency_protection`（依赖被不被告知）三件事各有自己的标签，
互不覆盖。“容器在跑但回 500”是 `running` + `error` 同时成立，不是一句“状态良好”，也不是
“已停止”。

**（4）列表过滤的两条规则要能分辨。**

- `GET /apps?group_id=…`：形状对（`grp_` + 32 位小写十六进制）但分组不存在 → **200 + 空数组**
  （“这个分组里没有应用”，与“分组被清空了”是同一件事）；形状不对 → **400 `VALIDATION_ERROR`**
  并说明是 `group_id`。被默默忽略的过滤器比报错更危险：调用方会以为自己在看某个分组，
  实际看的是全部——这与 §3“读不到不能变成没有”是同一条纪律（DECISIONS Q277）。
- **`q` 不是接口参数**。搜索是页面侧对已取回数据的匹配（名字、描述、绑定对象），
  不接受服务端搜索参数，也不因为传了 `q` 而改变结果集。分页仍由服务端的 `cursor` 负责，
  两件事互不替代。

**（5）收藏是“我”的，不是应用的。** `PUT /apps/:id/favorite` 改的是当前用户在
`app_favorites` 里的那一行：viewer 收藏之后，admin 的列表里它仍未收藏。收藏不是审计事件，
重复收藏幂等（不需要 `version`，也不报冲突）。

**（6）卡片显示的是“读到的那一轮”。** 摘要里的 `runtime_state` 来自服务采集器的快照
（默认 10 秒一轮），动作完成之后页面不会“乐观更新”，下一次刷新才会变。这条写在这里是为了
避免把“晚一拍”当成缺陷去修——要更快只能改采集节奏，那是整台机器的开销权衡，不是页面的决定。

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
| GET /logs?source=&unit=&level=&from=&to=&cursor=&limit= | admin；最多 1000 行/1 MiB，时间范围最多 24 小时 |
| GET /logs/stream | admin；SSE，固定数据源白名单 |
| GET /terminal/sessions | admin；仅当前用户 |
| POST /terminal/sessions/:id/connection-grants | admin+CSRF；仅会话创建者，生成 30 秒单次连接许可；允许 60 秒宽限内重连 |
| GET /terminal/sessions/:id/connect | WebSocket；只允许创建者，握手 Origin 校验，创建许可单次使用 |
| DELETE /terminal/sessions/:id | admin；关闭自己会话，审计 |

terminal.create 任务完成后返回 session_id 和会话元数据（不是短期许可——许可是单独签发的，见 10.4）。连接许可通过 WebSocket 子协议的受控字段传递，禁止放 URL 查询参数/访问日志。握手后客户端只能发送 input 和 resize 两类结构化消息；cols/rows 限 1—500，单帧最大 64 KiB，总速率有限额。服务端返回 output 和 exit。二进制终端数据不经过普通 HTML 渲染。

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

### 10.3 日志查询与实时流（P05.3 起）

`GET /logs` 与 `GET /logs/stream` 都是权限 **admin**。数据经代理的 `logs.query` 与 `logs.stream` 取自 journald；Web 不读 `/var/log`，也不执行任何命令。

**为什么不给 viewer。** PLAN 第 14 节写明：秘密过滤是尽力措施，不能作为向 viewer 开放日志的依据。因此 viewer 得到 403 `FORBIDDEN`，而不是“脱敏后的日志”。一份看起来干净的日志会让人以为它可以随便贴出去，而那正是这一条要避免的误判。

**本表上面那行的 `level=` 是 P05.3 补的。** PLAN 第 14 节要求按级别筛选，而原表漏了它。补参数而不是“前端自己过滤”的理由：级别过滤发生在 journalctl 的 `--priority`，服务端过滤后返回的 `scanned`、条数与 `next_cursor` 才是真的；前端过滤会让“这页只有 3 条”与“这段时间只有 3 条 error”混成一件看不清的事。语义是**下限**（`level=error` 表示 error 及更严重），与 journalctl 一致。

参数闭集：`source`（`journal` | `kernel`，缺省 journal）、`unit`（完整单元名，长度 1—256，`^[A-Za-z0-9][A-Za-z0-9:_.@-]*\.(service|socket|timer|mount|automount|swap|target|path|slice|scope|device)$`，不接受 glob/空格/路径，`source=kernel` 时不允许）、`level`（`emergency`/`alert`/`critical`/`error`/`warning`/`notice`/`info`/`debug` 之一）、`from`/`to`（RFC 3339，含端点）、`cursor`（上一页的 `next_cursor`，不透明，≤512 字节）、`limit`（1—1000，缺省 100）。`from` 必须早于 `to`，跨度 ≤24 小时；只给一端或都不给时回看 1 小时（`DefaultWindow`）；`to` 超过当前时间 2 分钟以上返回 400——未来窗口永远为空，与其让人以为“这段时间没有日志”，不如直说参数写错了。时间格式在**进入读取端之前**就校验：`from=2026-10-21` 这种手写日期若被当成“没给时间”，页面显示的就不是用户以为的那段时间。

`level=unknown` **不是合法入参**。`unknown` 只出现在结果里，表示该条目没有可识别的 PRIORITY；把它当成 `info` 会让“只看 error”的过滤看起来生效了，实际却混进了未知级别。同理，读不出来的级别不会在结果里被写成 `info`。

**分页是降序键集**：`entries` 按时间从新到旧（最新在前），`next_cursor` 是本页最后一条的游标，带回来即“从这里（含）继续向更早翻”。游标是 journald 的 `__CURSOR`，翻页与断线续传都靠它，**不透明**：只允许原样带回，不要解析、比较或构造。被拒绝的游标返回 400（重试同一个游标不会变好，要重新查第一页）；翻到窗口起点时 `next_cursor` 为 `null`。实现用的是 `--cursor=X -n N --reverse`（journald 实测 **含** X 这一条），因此边界条目会重复一次，由读取端按游标全等去重——按时间比较不行，同一微秒里的多条日志用时间根本分不开。

带游标时**命令行里不能同时出现 `--since/--until`**（systemd 255 实测：`Please specify only one of --since=, --cursor=, and --after-cursor=.`，退出码 1）。所以游标翻页时时间窗口由读取端在解析之后过滤：窗口外的条目不会出现在页里，但 `next_cursor` 依然往前走（整页都落在窗口外时它指向这一页扫到的最老一条）。这条不是实现细节而是契约的一部分——否则“翻页 + 时间窗口”这个组合要么报错，要么拿着同一个游标原地打转，客户端只能看到重复的第一页。`scanned` 仍是实际解析过的条数（含被窗口过滤掉的），页面据此能看出“扫了多少、给了多少”。

查询串必须是合法 URL 编码：`cursor` 里带 `;`，未转义时 Go 的 `url.ParseQuery` 会把整对参数丢掉，读端于是把它当成“没给游标”而退回第一页——**静默忽略比报错更难查**，所以这种请求现在是 400，而不是悄悄换一个查询。

`limit=0` 也是 400：契约里 `limit` 的下界是 1，显式给 0 不是“用默认值”，默认值只在参数缺省时生效。

限额由服务端强制，前端传什么都放宽不了：单页最多 1000 行、单页消息累计 1 MiB、单条消息保留 8 KiB。触到上限时 `truncated=true`，`truncated_reason` 说明是哪条限额先到（`page_bytes` 字节上限、`output_limit` 表示 journalctl 输出被单次读取上限截断、字段缺省表示撞到调用方给的 `limit`）。`truncated=true` 表示“不是只有这些”，不是完整结果。

`unit` 是 journald 原始 `_SYSTEMD_UNIT`，因此有例外要说清：systemd 自己写的 `Starting <unit>` / `Finished <unit>` 两条生命周期消息，`_SYSTEMD_UNIT` 是 `init.scope`（它记的是“systemd 在说这个单元”），而 `journalctl -u <unit>` 仍把它们算进该单元。API 如实回传原字段，不替 journald 猜测归属。

`LogPage` 字段：`source`、`unit`（生效的过滤，未过滤时不出现）、`level`（同上）、`from`/`to`（本次**实际生效**的窗口，默认值已补齐，用来让页面说清“查的是哪一段”）、`entries`（没有条目时是空数组，不是 `null`）、`next_cursor`、`truncated`、`truncated_reason`、`scanned`（实际解析过的条数，含被去重丢掉的条目，用于诊断“是不是扫太多了”）、`duration_ms`。

`LogEntry` 字段：`cursor`、`timestamp`（`__REALTIME_TIMESTAMP`，UTC）、`level`（含 `unknown`）、`message`（脱敏并截断后的文本）、`truncated`、`redacted`、`binary`、`unit`/`identifier`/`comm`（没有时字段不出现，不是空串）、`pid`（缺失时为 `null`，不是 `0`；0 是合法 PID 的邻居，容易被误读）。原始 journal 字段（`_AUDIT_LOGINUID`、`_BOOT_ID`、`_MACHINE_ID` 等）一律不下发：它们对排查有用，但会把整机指纹一起带出去，而本阶段的需求是按 unit/时间/级别查日志，不是 journal 字段浏览器。

`redacted=true` 是“这条里至少有一处被抹掉了”的事实，**不是**“这条已经干净”的保证。`binary=true` 表示 journald 没有给出可显示的文本（`MESSAGE` 为 `null` 或字节数组；systemd 255 对超过约 8 KiB 的单条消息就是这样处理的），此时 `message` 是空串，前端必须渲染成“没有文本”而不是一条空白日志行。

`GET /logs/stream` 是 SSE：`id` 是 journal 游标，命名事件三种——`entry`（data 与 `LogEntry` 同形，帧类型由事件名承担，data 里不再重复一个 `type` 字段）、`end`（`{"reason": ...}`）、`error`（`{"code":"STREAM_FAILED","message": ...}`）。`end` 的 `reason` 是闭集：`duration_limit`（单条流最长 10 分钟）、`idle_timeout`（60 秒没有新条目）、`entry_limit`（5000 条）、`byte_limit`（8 MiB）、`journal_exit`（journalctl 自己退出）、`client_closed`（调用方不再接收——此时客户端不会收到这一帧，连接已经没了）。每 15 秒一行注释心跳（`: keepalive`）。续传靠浏览器带回的 `Last-Event-ID`（就是最后一个 entry 的 `id`），语义是“从该游标**之后**发送”，因此重连不重复也不需要客户端记位置；日志流**没有**任务事件流那样的 `gap` 帧——续传点是 journal 自己的游标，历史不由本服务裁剪。

**响应头立刻发出。** 流的响应头在开流时立即 flush，而不是等第一条日志：安静的系统上第一条可能要等几十秒，而调用方（Web 的代理客户端）有“等待响应头”的计时器——不 flush 的话它先把连接掐掉，服务端的空闲/时长上限根本没机会生效（真实 VM 上表现为 `timeout awaiting response headers`）。代理→Web 这层的客户端对**流**请求不设 `ResponseHeaderTimeout`，普通请求仍然是 10 秒。

**资源回收。** 客户端断开、心跳写入失败或代理返回错误时，请求 ctx 结束，该 ctx 一路传到代理的 http 请求；代理看见连接断开即杀掉 journalctl 所在的进程组（`internal/syscmd`，进程组 kill + 2 秒 `WaitDelay`）。STAGES 第 7 阶段的验收条目“结束日志订阅不会残留无限 journalctl 子进程”验的就是这条链路，两半各有测试：代理侧 `syscmd.TestStreamContextCancelKills`、`journal.TestStreamClientClosedStops`，Web 侧 `api.TestLogsStreamReleasesOnDisconnect`。每个账号同时最多 4 条流（与任务事件流共用额度），超出返回 429。

错误映射：参数问题（时间格式/先后/跨度、`level`/`source` 不在闭集、`unit` 形状、`limit` 越界、游标被拒）一律 400 `VALIDATION_ERROR`；代理不可用（未配置 socket、连接失败、本机没有 journald）503 `AGENT_UNAVAILABLE`。代理回的 400 原样转成 Web 的 400，代理回的 5xx 转成 503——只有代理能判的条件（例如 `to` 在未来）不能变成 Web 的 500。

### 10.4 受限 Web 终端（P06.3 起）

终端是**两条 WebSocket 加一个 PTY**：

```
浏览器 ──WS(upanel.term.v1 + upanel.grant.<id>)──► Web ──WS(upanel.term.v1)──► 代理 ──PTY──► upanel-shell
```

PTY 只存在于代理进程里，`fork` 之后、`exec` 之前降权到终端账号（`setgroups` 清空附加组 → `setgid` → `setuid`），并从 `/proc/<pid>/status` 读回真实/有效 uid、gid 与附加组核对；任何一项不符就杀掉子进程并让这次创建失败。Web 进程从头到尾不接触 PTY：它不认识 `input` 帧的内容，也不知道终端里发生了什么。

**会话是代理进程内的对象，不是任务。** `terminal.create` 的预检 → 任务链路只负责“开一个会话”，任务在会话创建完成时就 `succeeded`；会话此后的生命周期（连接、断开、空闲、到期、关闭）不进任务中心。把两者绑在一起会得到一个“任务一直运行到终端关闭”的假象，而任务中心的状态机表达不了“一条连接正在被使用”。

**会话状态**只有两个：`detached`（PTY 在、没有连接）与 `attached`（一条连接正连着）。同一个会话同时只允许一条连接，第二条在握手前得到 409 `RESOURCE_BUSY`——不是把第一条踢掉，也不是两个窗口共享一个 PTY（那会让两边的光标互相打架）。

**三个时限，配置只能收紧。** 空闲关闭 60—900 秒（默认 900）、单会话最长 60—3600 秒（默认 3600）、断流宽限 5—60 秒（默认 60）；`config` 的 `Validate` 只接受落在区间内的值，放宽会被启动拒绝。数量上限的契约值是每账号 1 个、全局 2 个，配置同样只能收紧。空闲计时看的是**最后一次活动**（输入、窗口调整、以及一次成功的重连），不是最后一次输出——`tail -f` 那种一直有输出但没人碰键盘的会话仍然会被判定为空闲。

**断流不等于结束。** 连接掉了（关页面、浏览器崩溃、网线被拔）之后会话进入 `detached`，PTY 与 shell 进程都留着，宽限期内重连接上的是**同一个 shell**（导出的变量、当前目录都还在）。宽限期到了才结束会话并回收子进程；结束原因是闭集 `user`、`exit`、`idle`、`expired`、`disconnect`。断开期间 PTY 的输出照常被读走并丢弃——留下来的只有 PTY 自己那点缓冲，不做无限攒。

**“回收”指 shell 进程真的没了，不只是从会话表里消失。** 会话结束（到期、空闲、宽限到期、用户关闭、代理停机）时必须显式挂断这个 tty 的进程组：只关 master 不足以让子进程收到 SIGHUP——读泵正阻塞在该 master 的 `read` 上，`close(2)` 要等那次 read 返回，tty 因此不会被释放。判定标准是 `pgrep -u upanel-shell` 归零、`/proc/<pid>` 消失，而不是接口返回 4xx/2xx。子进程不理会 SIGHUP 时由代理补 `SIGKILL`；发信号前必须核对 `/proc/<pid>/stat` 的 ppid 与 starttime，确认这个 pid 仍是自己的子进程（pid 被回收后可能复用）。

**帧协议。** 握手协商出的子协议必须是 `upanel.term.v1`；升级之后两个方向都只发文本帧，内容是 JSON，字段是**闭集**（多一个未知字段即拒绝）：

| 方向 | type | 字段 | 说明 |
|---|---|---|---|
| 客户端→服务端 | `input` | `data_b64` | 键盘输入（base64），解码后 ≤64 KiB |
| 客户端→服务端 | `resize` | `cols`、`rows` | 窗口尺寸，1—500 |
| 服务端→客户端 | `ready` | `session_id`、`account`、`account_uid`、`cols`、`rows`、`server_time` | 连上之后的第一帧 |
| 服务端→客户端 | `output` | `data_b64` | PTY 输出 |
| 服务端→客户端 | `exit` | `reason`、`exit_code`、`signal` | 会话结束；`reason` 同上面的闭集 |
| 服务端→客户端 | `error` | `code`、`message` | 帧级拒绝：`FRAME_INVALID`、`RATE_LIMITED` |

闭集是**服务端**的判定：未知 `type`、越界尺寸、非法 base64、空 input、非 JSON 一律 `FRAME_INVALID` 并断开连接。这里没有 `run`/`exec`/`command` 帧，也不会加——一条“请执行这条命令”的帧等于把整个终端做成万能命令接口。

限额：单帧数据 64 KiB、整帧 95 KiB（base64 与信封的余量）、输入 256 KiB/s 与 200 帧/s（滑动窗口）。超限是 `RATE_LIMITED`，连接断开，会话不受影响（断流宽限照常生效）。

**断开的原因要让浏览器看得见。** 服务端在断开之前发一帧 close，关闭码是 1000（正常结束）、4001（帧不合法）、4002（超速率）。不发这一帧的话，浏览器只能报 1006“异常关闭”，于是协议违规、超速、到期、掉线在页面上长得完全一样。

**连接许可。** `POST /terminal/sessions/:id/connection-grants`（admin + CSRF）签发 32 位小写十六进制的许可：30 秒有效、只能用一次、绑定用户与会话。许可只经**子协议**传递，子协议名是 `upanel.grant.<id>`；它不进 URL 查询参数、不进 Cookie、不进访问日志，也不回显在 101 响应的 `Sec-WebSocket-Protocol` 里（只回 `upanel.term.v1`）。同一个许可用第二次得到 403，拨号失败也算用过——重连必须重新签发。

**握手四道门，顺序固定，且都在升级之前。**

| 顺序 | 检查 | 失败 | 审计 `stage` |
|---|---|---|---|
| 1 | `Origin` 在白名单内 | 403 `CSRF_FAILED` | `origin` |
| 2 | 子协议里有 `upanel.term.v1` | 400 `VALIDATION_ERROR` | `protocol` |
| 3 | 子协议里的许可有效、未过期、属于该会话 | 403 `FORBIDDEN` | `grant` |
| 4 | 代理附加成功（会话存在、属于该用户、未被占用） | 原样映射（404 / 409 / 503） | `agent` |

四道门都在**升级之前**判，所以拒绝是一次正常的 HTTP 响应（带 JSON 错误信封），不是“浏览器拿到 101 之后莫名其妙地断开”。为此 Web 先拨代理、再升级浏览器：反过来的话，代理的拒绝只能以“连接被关闭”呈现。

**错误映射。** 同一张表（`terminal.HTTPStatusOf`）在代理、Web、任务三处共用：会话不存在 404 `NOT_FOUND`（别人的会话也是这个——区分“没有”和“不是你的”等于告诉对方这个 id 有效）、已被占用或数量到顶 409 `RESOURCE_BUSY`、参数问题 400 `VALIDATION_ERROR`、账号不可用/本机没有 PTY 501 `NOT_IMPLEMENTED`、代理不可用 503 `AGENT_UNAVAILABLE`。`terminal.create` 的任务失败也带同一个码，不压成 `COMMAND_FAILED`。

**关闭是幂等的。** `DELETE /terminal/sessions/:id` 对“已经不在跑”的会话回 200 与 `closed=true`：调用方要的结果是“它现在不跑了”，而那已经成立。别人的会话仍然是 404。

**能力软关闭。** 终端账号不可用（不存在、是 root、shell 是 nologin、home 权限过宽、在特权组里）或本机没有 PTY 时，代理**不登记** `terminal.create` 与 `terminal.sessions`，并在日志里说明原因；Web 启动时探测一次能力表，据此让四个终端接口统一回 501 并说明“本机没有可用的终端”。带着一个注定起不来的 shell 对外宣称“有终端”，只会把问题推给用户去猜。探测失败（代理还没起来、socket 不通）不改变行为——那只说明代理不可用（503），不说明本机没有终端。

**审计只记元数据。** 动作 `terminal.attach`（握手四道门、连接建立、连接结束）与 `terminal.close`；细节里只有 `origin`、`protocol`、`stage`、`end_side`、`end_reason`、`duration_ms`、`input_bytes`、`output_bytes`，`target` 是 `terminal:<session_id>`（中间件层拒绝的也指向同一个会话）。**终端内容一律不进审计、不进日志**：没有输入回放、没有输出片段、没有屏幕录制。会话内容不是元数据，落库就等于把“只记元数据”这句话作废。

**明确不提供**：root 或 sudo 终端（终端身份是一个无 sudo、无特权组、独立 home 的专用账号，PTY 在 exec 之前就已降权）、任意命令执行接口（没有 `run` 帧，也没有 `POST /exec`）、tmux 式持久化（会话最长 1 小时，进程不跨 Web 重启存活）、终端录像/回放、多人共享同一个会话、SFTP/文件传输（文件走第 11 节的文件契约）。

## 11. 文件契约

所有文件 API 为 admin。以下为数据访问接口，实际修改通过第 5 节动作提交。

| 方法与路径 | 行为 |
|---|---|
| GET /files/roots | 返回允许访问的 root_id、名称、容量、可写能力 |
| GET /files/list?root_id=&path=&cursor= | 子项 name、type、size_bytes、mode、owner、mtime、version；不跟随 symlink |
| GET /files/text?root_id=&path=&expected_version= | UTF-8 文本、version；上限 1 MiB |
| GET /files/download?root_id=&path=&expected_version= | 流式下载普通文件；Content-Disposition 安全编码；无 inline HTML |
| POST /files/uploads | root_id、path、size_bytes、expected_version；创建上传暂存，会话绑定 |
| PUT /files/uploads/:id/content | application/octet-stream 流，CSRF 与大小检查；不支持断点续传 |
| GET /files/uploads/:id | received_bytes、state、expires_at |
| DELETE /files/uploads/:id | 删除暂存上传，不影响目标文件 |

上传完成后仍需 file.commit_upload 预检与任务才能替换目标。未完成暂存 24 小时后清理。容量预留在并发上传之间共享检查；磁盘不足必须删除本次暂存并返回错误。

路径边界：Linux 使用 openat2 的 RESOLVE_BENEATH、RESOLVE_NO_MAGICLINKS、RESOLVE_NO_XDEV 和不跟随 symlink 的策略；不支持该机制的平台拒绝真实文件操作。先验证根目录实际挂载 UUID 并打开根句柄，随后禁止跨入根下其他 bind mount/子挂载；子挂载如需访问须独立登记为文件根。初期拒绝 nlink>1 的普通文件读取和写入，避免硬链接越界难以判定；特殊文件不读写。不能用字符串 startsWith(root) 代替边界校验。

文件 version 由设备/ inode /大小/纳秒 mtime 等生成，并在已打开句柄上核验。跨根目录 move 不是原子操作：采用复制到目标暂存→校验→原子落位→删除源；中途失败保留源并显示实际状态，不声称原子完成。

### 11.1 读取接口的字段与判决（P06.1）

**根只能来自配置。** 请求里没有绝对路径字段：只有 root_id 与根内相对路径。未登记的 root_id 是 404 NOT_FOUND，不会退回到“按路径找找看”。`agent.files.roots[i].path` 不能是 `/`：那等于把整个服务器文件系统登记成一个文件根，配置校验直接拒绝并让代理启动失败，错误信息指向该字段。界面能管理的范围因此**在数据上**就只有一个根列表，不可能显示“可管理整个服务器所有目录”。

**GET /files/roots** 的每一项：root_id、name、path、writable、available、unavailable_reason（仅不可用时出现）、mount_point、fs_type、capacity、capacity_error。capacity 为 null 表示这次没读出来，原因见 capacity_error——null 不是“0 字节可用”，界面不得显示成磁盘满。available=false 的根显示成不可用，不是显示成空目录。没有登记任何根时返回空数组 `roots: []`，界面据此说“尚未配置文件根”，而不是渲染一个空下拉框。

**GET /files/list** 的 data：root_id、path、entries[]、next_cursor、truncated。path 是规范化后的相对路径，根自身是空串。entries 每项：name、type（`file`/`dir`/`symlink`/`other` 闭集）、size_bytes（十进制字符串）、mode（**四位八进制字符串**，如 `"0644"`）、owner{uid,gid}（只给数字，不查 NSS 账号名）、mtime（RFC 3339 UTC）、version。不跟随 symlink：symlink 的 version 与 mtime 描述链接本身，不描述目标；`other` 覆盖设备、FIFO、socket，可以被列出，绝不被读取。

分页是**键集分页**：游标是上一页最后一个 name，下一页从它之后开始，不是 offset；页大小固定 200，因此同一个游标总是同一个起点。`next_cursor` 为 null 表示没有下一页（与 /jobs、/logs、/users 一致），**不是空串**——空串会让“尾页”和“游标恰好是空”在 JSON 里长得一样。单次列举最多扫 50 000 个名字，超出时 `truncated=true`；这是“列出的不是全部”，界面必须说明，不得当成完整列表。

**GET /files/text** 与 **GET /files/download** 都接受可选的 expected_version：非空时必须与**已打开句柄**上 stat 得到的版本一致，否则 409 VERSION_CONFLICT。text 上限 1 MiB（超出 413 VALIDATION_ERROR），内容必须是合法 UTF-8（否则 422 FILE_NOT_TEXT）且不含 NUL——二进制不是文本，不猜编码。text 的 data：root_id、path、version、size_bytes（十进制字符串）、encoding（`utf-8`）、content。

**version 的形状**：`v1-<dev 十六进制>-<ino 十六进制>-<size 十六进制>-<mtime 纳秒十六进制>`。它不是哈希，也不承诺跨挂载稳定；它只是一份“这个句柄上的字节还是不是那一份”的凭据，客户端不得解析、拼接或推算。

**GET /files/download** 的响应体不是 JSON，而是文件原始字节：Content-Type 固定 `application/octet-stream` 并带 `X-Content-Type-Options: nosniff`（浏览器永远不把这里的内容当成 HTML/脚本渲染）、`Cache-Control: no-store`、Content-Length 与本次字节数一致、Content-Disposition 为 `attachment` 且同时给 ASCII 退化名与 RFC 5987 的 UTF-8 名（控制字符丢弃、引号与反斜杠替换，响应头里不会出现换行）；`X-UPanel-File-Version` 带本次下载的 version。被拒绝的下载返回 JSON 错误信封，绝不是 octet-stream——否则浏览器会把错误当文件存下来。

**读取的打开方式**：一律 `O_NOFOLLOW | O_NONBLOCK`。O_NOFOLLOW 让“最后一段是符号链接”由内核拒绝，而不是先检查再打开；O_NONBLOCK 是因为根里可能有 FIFO，`open(fifo, O_RDONLY)` 会无限阻塞（对普通文件无副作用）。硬链接（nlink>1）拒绝为 PATH_OUTSIDE_ROOT：拒绝它的理由正是这个 inode 在根外可能还有别的名字，与越界是同一件事。

**错误判决**单一来源于 `files.HTTPStatusOf`，代理的查询、代理的流、Web 的公开接口共用同一张表，因此同一个 `..` 无论在哪一层被拦下都是同一个状态码与同一个 code：PATH_OUTSIDE_ROOT 422、NOT_FOUND 404、ROOT_UNAVAILABLE 409、FILE_NOT_TEXT 422、VERSION_CONFLICT 409、超限 413 VALIDATION_ERROR、形状问题 400 VALIDATION_ERROR、平台无 openat2 为 501 NOT_IMPLEMENTED、代理连不上为 503 AGENT_UNAVAILABLE。新增错误码只有 ROOT_UNAVAILABLE 与 FILE_NOT_TEXT 两个，其余情形落到第 1 节已有的码上。

### 11.2 写入接口、暂存上传与动作参数（P06.2）

**没有“直接写文件”的公开接口。** 文件系统里发生的每一次改变都是一次**动作**（第 5 节），走预检 → 任务 → 结果验证 → 审计。第 11 节表里的四个 upload 端点只负责把字节搬进/搬出暂存区，它们不改任何服务器文件：`POST /files/uploads` 与 `PUT /files/uploads/:id/content` 收完之后，目标路径一个字节都没变。

**POST /files/uploads** 请求体：`root_id`、`path`（目标文件的根内相对路径）、`size_bytes`（十进制字符串）、`expected_version`（目标此刻的版本；目标不存在时为 null 或省略）。响应 data 是上传视图：`upload_id`（`up_` + 32 位小写十六进制）、`root_id`、`path`、`size_bytes`、`received_bytes`、`state`、`expected_version`、`created_at`、`expires_at`、`error`（仅 failed 时出现）。逐个字段都要自洽：代理回的 root_id/path/size_bytes/expected_version 必须与请求一致，不一致是 503 AGENT_UNAVAILABLE（“代理给的不是我声明的这份上传”），不是 200。

**PUT /files/uploads/:id/content** 的请求体是原始字节（`application/octet-stream`），不是 JSON、不是 multipart：多一层编码就多一次把整份内容读进内存的机会。必须声明 `Content-Length`，且它必须等于创建时的 `size_bytes`——不支持分块传输，也不支持断点续传。长度不符在入口就 400 VALIDATION_ERROR，不让它先传 2 GiB 再失败。收完之后 `state=received` 且 `received_bytes == size_bytes`；少一个字节也是失败。上传期间 Web 与代理各自用 `http.ResponseController` 推读截止时间（空闲 60 秒即断，慢不是错，停住才是），响应前再推写截止时间——否则一次几分钟的上传会被服务器的 30 秒 `ReadTimeout`/`WriteTimeout` 掐断，表现为“传到一半连接没了”。

**GET /files/uploads/:id** 返回同一个上传视图；`state` 是闭集 `open|received|failed`，描述的是**暂存**，不是目标文件：目标只有在 `file.commit_upload` 成功之后才会改变，而那时上传记录已经不存在了。`failed` 表示传输失败（大小不符、空间不足、超限），`error` 说明原因，记录留到过期时间——让调用方看到“为什么失败”，而不是一个忽然消失的 id。**DELETE /files/uploads/:id** 删除暂存并释放容量预留，不影响目标文件。未完成或未提交的暂存在 24 小时后由代理定期清理，清理同样释放预留。

**容量与配额。** 单次上传上限默认 2 GiB（`agent.files.max_upload_bytes` 可配，0 表示默认值），声明值超过上限或扣掉已预留容量之后放不下，都在创建暂存时 413 VALIDATION_ERROR，并且**不留下任何暂存**。并发上传按声明值共享预留：两个各声明 1.5 GiB 的上传在一个只剩 2 GiB 的卷上，第二个在创建时就被拒绝，而不是两个都传完才有一个失败。写入过程中撞上 ENOSPC 同样是 413，本次暂存会被删除，预留随之释放。

**保留目录 `.upanel`。** 每个文件根内有一个保留目录：`staging`（暂存上传）与 `backups`（编辑备份）都在里面。它必须留在根内而不是 `/var/lib`，因为原子替换要求临时文件与目标在**同一个文件系统**上，跨挂载的 rename 会退化成复制，而复制不是原子替换。它不出现在列目录结果里，任何指向它的路径在形状层就被 `ErrPathRejected` 拒绝（422 PATH_OUTSIDE_ROOT）：里面是正在写的半成品和别人改之前的旧版本，它们不是“服务器上的文件”。

**动作与它们的门槛。** 参数以第 5 节表为准（`relative_path`、`source_root_id`、`source_path`、`target_root_id`、`target_path`、`expected_version`、`recursive`、`mode`、`managed_account_id`、`managed_group_id`、`upload_id`、`overwrite`、`content`）。策略：

| 动作 | 再认证 | 确认短语 |
|---|---|---|
| file.mkdir | 否 | 否 |
| file.write_text | 否 | 否 |
| file.copy / file.move | 否 | 否 |
| file.commit_upload | 仅 `overwrite=true` 时 | 仅 `overwrite=true` 时，`OVERWRITE` |
| file.delete | 是 | 仅 `recursive=true` 时，`DELETE` |
| file.chmod | 是 | 否 |
| file.chown | 是 | 否 |

**版本冲突的语义。** `expected_version` 在写动作里是**强制**的，不是可选的乐观提示：

- `file.write_text`：目标已存在时必须给 `expected_version`，否则 409 VERSION_CONFLICT（“覆盖”等于“谁先写完算谁”，不接受）。目标不存在时它必须为空。
- `file.commit_upload`：目标已存在时必须 `overwrite=true` **并且**给出匹配的 `expected_version`；只给其中一个是拒绝，不是“尽力而为”。
- `file.delete` / `file.chmod` / `file.chown`：目标必须存在；给了 `expected_version` 就必须匹配。
- `file.copy` / `file.move`：`expected_version` 描述**目标**。目标已存在即阻塞（409 VERSION_CONFLICT），不存在时它必须为空。

预检与执行各核验一次：预检把目标的 IdentityFingerprint 与 StateVersion 记进计划，提交任务时重算，不一致分别是 DEVICE_IDENTITY_CHANGED 与 PREFLIGHT_STALE；到了执行期，真正的版本核对发生在**已打开的句柄**上（`verifyVersion`），而不是路径字符串。

**结果验证。** `outcome=verified` 的意思是“读回来的东西与预期一致”，不是“调用没报错”：文本写入与上传落位都重新打开目标读回 size 并核对 sha256（上传落位还核对落位前后的 dev+ino 相同）；建目录读回核对 `S_IFDIR` 与设备号；删除后 `fstatat` 必须 ENOENT；chmod/chown 读回核对权限位与所有者。任何一步读不回预期就如实报 `unknown`，不假装成功。

**编辑备份。** `file.write_text` 与 `file.commit_upload` 覆盖已存在文件之前，先把旧版本真实复制到 `.upanel/backups/<路径摘要>/<纳秒时间>`，再从最旧开始清理：单文件最多 10 份、总量最多 500 MiB。**不能用硬链接做备份**——硬链接会让 nlink>1，而读路径自己拒绝 nlink>1 的普通文件。备份路径与份数随 `WriteResult` 一起返回（`backup_path`、`backup_count`、`backup_bytes`）。

**递归操作的上限。** 单次递归删除/复制/统计最多走 50 000 个目录项，撞上上限直接拒绝（413 VALIDATION_ERROR），不是“删一半停下”。目录树里含符号链接、特殊文件或多硬链接文件时，递归复制拒绝整个操作（422 PATH_OUTSIDE_ROOT）：跳过几个会让结果与“复制了这个目录”这句话不一致。

**权限与所有者。** `mode` 只接受普通权限位（四位八进制 `"0644"`），setuid/setgid/sticky 一律拒绝——它们改变的是执行语义，文件管理面板没有资格顺手写上去。`file.chown` 引用的是**受管理账号/组**的 id，不是任意 uid/gid；账号模块（P08）尚未落地时，预检直接以 `MANAGED_ACCOUNTS_UNAVAILABLE` 阻塞，而不是接受一个数字或假成功。

**逐动作参数上限。** 请求体上限按动作独立设置（不是全局一个数）：`file.write_text` 的参数携带最多 1 MiB 内容，传输层因此放宽到 2 MiB；其余动作仍是 64 KiB。超限在解码前就是 413 VALIDATION_ERROR。

**跨根 move 不是原子操作。** 同根的 move 是 `renameat`，一次系统调用；跨根是“复制到目标暂存 → 校验 → 原子落位 → 删除源”，中途失败保留源并如实报错。预检的 impacts 里明确给出 `not_atomic`，界面必须展示，不得声称原子完成。

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
| shares | 代理库：id、name UNIQUE、protocol、volume_id FK、relative_path、enabled、read_only、definition_json、config_hash、directory_prepared、config_applied、version |
| managed_accounts | 代理库：id、name UNIQUE、type、system_identity、managed、disabled、has_password、secret_ref（只是引用）、version；不保存密码或哈希 |
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

query 枚举按模块登记，例如 system.summary、storage.topology、services.list、files.list、logs.query；禁止 query=command。terminal 的字节通道只能连接已降权 PTY，不能进入代理的命令解释器。P14 增加 /internal/v1/totp/enroll、/confirm、/verify、/disable 四个专用接口，操作绑定用户与用途，种子不进入普通 query/secret 读取通道。

`/internal/v1/streams` 自 P05.3 起已实现，但目前只登记一种用途 **logs.stream**：`POST` 建许可（绑定用途与查询参数、单次使用、30 秒过期且到期即回收），`GET /streams/:id` 打开流式响应（`application/x-ndjson`，每行一帧 `{"type":"entry|end|error", ...}`；`entry` 帧的 `entry` 是日志条目本身，`end` 帧带 `reason`，`error` 帧带 `code`/`message`）。未登记的用途回 501，不回一条空流——多报一个用途等于把能力清单变成谎话。许可只用一次：第二次 `GET` 回 404，被放进地址栏或日志里的 URL 因此最多只能被用一次。流式消费端的「帧类型来自事件名」与外层 SSE 不同，是因为 NDJSON 是单条无名字的字节流，类型必须写在帧里。

内置定时任务只可执行已登记且启用的 backup.run、certificate.renew 和 script.run。systemd 调用 root 所有的固定 upanelctl schedule-trigger 命令，代理只在专用 trigger 路由接受 UID=0；普通路由仍只接受 upanel-web。系统身份来自 root 持有的任务配置与本地凭据，不允许公开 /jobs 请求自称 scheduler 来跳过再认证。定时触发须重新预检资源与锁，创建稳定触发 ID 并审计；交互确认由管理员保存计划时的显式授权替代，不能扩展到格式化或网络变更。代理产生的定时 operation 会被 Web 对账导入任务表，不能只存在于 journal。

actor_context 供审计关联，包括 user_id、session_id_hash、request_id、reauth_at。代理信任 Web 完成用户登录认证，但仍独立实施系统安全边界；不能把该设计描述为可抵御已完全控制 Web 进程的管理员身份伪造。

锁统一由代理维护。磁盘锁沿拓扑追踪到所有底层盘，多个资源按规范字符串排序后获取，避免死锁。进程内锁不足以协调外部 apt/配置编辑，还必须检测系统锁和配置哈希。所有外部命令有运行时限、输出限额和退出状态处理。

## 16. 契约验证要求

P00 建立 OpenAPI 文件骨架，后续每阶段先补齐对应 schema、枚举、权限、错误、请求示例再实现。不要在阶段未实现时生成“已支持”的 API 文档。

每个变更验证前后端类型、未知字段拒绝、枚举和状态机一致。关键契约至少覆盖：容量大整数、viewer 越权、CSRF、幂等冲突、预检过期、设备变化、文件版本冲突、任务重连、代理离线、非零退出、进程重启对账。

本文没有给出外围功能的所有 SQL 列与 UI 文案，但已固定其动作、参数、范围和实现次序。实施模型可以补充内部细节；不能借此新增产品能力、改名公开字段或取消检查。
