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

**空闲超时是滑动的，Cookie 必须跟着滑。** 每次校验把空闲期往后推时，服务端用同一令牌重发 `Set-Cookie`，`Expires` 同步到库里新的到期时间（节流：同一会话每分钟至多重发一次）。只在登录时下发一次到期时间是错的——那会让 Cookie 在登录后 30 分钟整点消失，而库里会话还活着，表现为"一直在用，到点照样被踢"，任何跨过 30 分钟的操作（备份恢复、长任务观察）都会半路断在 401。

已登录所有非 GET/HEAD 请求需要 X-CSRF-Token；令牌绑定会话，前端只保存在内存。登录接口要求 JSON Content-Type、X-UPanel-Request: 1 和精确匹配的 Origin。Origin 白名单来自本机配置，不由任意 Host 或 X-Forwarded-* 生成。WebSocket 握手同样校验 Cookie、Origin 和单次会话许可。

日志流、下载和 SSE 可以使用 Cookie 的同源 GET，服务端不开放跨域。生产默认不信任代理头，只有明确配置可信代理地址才解析其来源。

面板传输层（P10.4 定案，DECISIONS Q299）：管理面只经 HTTPS 对外。`web.tls_cert_path` 与 `web.tls_key_path` 必须成对配置（POSIX 绝对路径），只配一半是配置错误而不是“没配”；最低协议版本 TLS 1.2；未配置 TLS 时启动器只接受回环监听地址（`web.listen_address` 必须是 127.0.0.1、::1 或 localhost），非回环监听缺 TLS 一律拒绝启动。TLS 生效时所有响应带 `Strict-Transport-Security: max-age=15552000`（不含 includeSubDomains/preload），且派生允许源使用 `https://` scheme；`web.allowed_origins` 的显式条目同样必须是 `https://`。面板不做 ACME 客户端、不写系统信任库：证书来源自签名（安装脚本生成并当众打印 SHA256 指纹）或用户提供的受信任证书。

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
| GET /system/metrics?scope=&from=&to=&step= | 明确 scope 的时序样本；限制时间范围/点数（口径见 §13.1） | V |
| GET /system/metrics/status | 历史采集的运行时状态：是否在采、各粒度行数、保留窗口、最近错误 | V |
| GET /system/power-state | 重启标志、来源包清单与本 boot_id（口径见 §12.1） | admin |
| GET /system/capabilities | 外部程序能力快照：这台机器装了哪些程序、什么版本、缺的为什么缺（口径见 §3.1） | admin |
| GET /audit | 审计分页和筛选 | admin |

capabilities 为列表，每项包含 id、state、reason、required_packages、supported_actions。state 为 ready、missing_dependency、unsupported、disabled、mock。模式为 production 或 mock，不允许 silent fallback。mock 服务绑定 127.0.0.1:18881，与真实代理完全隔离；页面持续显示“演示数据，未操作服务器”。

### 3.1 P16.1 已实现范围：外部程序能力探测（一份实现，三种问法）

**为什么这件事以前没有答案。** 备份页要 restic、部署页要 docker、代理页要 nginx、防火墙页要
ufw，缺任何一个时界面只能显示“读了但没读到”——真正的原因（没装？装的是别的版本？跑在容器里
没有这个程序？）只有 SSH 进去才知道。同时 `capabilities.dependencies` 是一个**硬编码的空数组**：
字段存在、注释写着“当前尚未探测”，于是三处页面各读各的空。

**一份实现，三种问法。** 探测只有 `internal/hostcaps` 一处，三个出口读同一份事实：

| 出口 | 谁用 | 形状 |
|---|---|---|
| `upanelctl host-caps` | 安装器、没有面板时的排障 | 人读的表 / `--json` / `--line` |
| `GET /internal/v1/capabilities` 的 `host_caps` | 面板 Web 进程 | `Report` 或 `null` |
| `GET /api/v1/system/capabilities` | 浏览器 | `{ready, host_caps}` |

`capabilities.dependencies` **已删除**，不再保留一个永远为空的字段：留着它会让人以为那里本该
有内容、只是这一轮没填上。

**四条诚实性规则（每一条都对应一种会被读成假话的情形）。**

1. **只在固定目录里找**：`/usr/sbin`、`/sbin`、`/usr/bin`、`/bin`。不走 `PATH`——PATH 里可能有
   当前目录或别的可写路径，“在 PATH 里”与“这台机器上有”不是一件事。
2. **三态**：`present` / `absent` / `unknown`。固定目录读不出来时该条是 `unknown`，且整份报告
   `partial=true`、原因进 `issues[]`。把 `unknown` 并进 `absent` 会让人去重装已经装好的东西。
3. **版本只在真读到的时候填**：命令跑完、退出码 0、输出匹配到锚点才填版本；超时、认不出、
   没有可靠的版本开关（`exportfs`、`useradd`、`visudo` 就是这种）一律留空并写原因。空串的意思是
   “没有版本”，不是“版本是 0”，也不是“未知”。
4. **`null` ≠ 空**：代理还没探完时 `host_caps` 是 `null`，`ready=false`。前端必须画“还没探完”。
   一条表都不渲染，与渲染一张空表是两件事——后者在任何人眼里都是“这台机器什么都没装”。

**缓存而不是请求内探测**：代理启动时探一次，之后每 10 分钟重探。端点只读快照。理由是这条
端点会被页面反复刷新，让每次刷新都等十几条版本命令不合适；反过来，一次探测十几条命令并发跑
（`probeConcurrency=4`），正常 1 秒内结束。

**规格表**（21 条，`internal/hostcaps` 的 `specTable`）：systemd、journalctl、systemd-analyze、
docker、docker.compose、nginx、restic、ssh、ufw、ip、nfs、samba、apt、certbot、smartctl、sgdisk、
mkfs.ext4、mkfs.xfs、findmnt、shadow、sudo。登记标准是“有真实消费者”——表里每一条都对应一个
已经在用的子系统；没有消费者的条目不进表，一张越写越长、一半没人读的能力表会让人以为探到的
每一条都已经作数。

**`upanelctl host-caps --line` 的列契约**（安装脚本用它，POSIX sh 没有 jq）：每行一条、无表头、
制表符分隔、六列，顺序固定：

```
id <TAB> state <TAB> version <TAB> path <TAB> feature <TAB> hint
```

空字段表示“没有”，**不许**用 `-` 或 `n/a` 之类的占位符（脚本会把占位符当成版本号）。
`--json` 与 `--line` 互斥（400 → 这里是退出码 2）。

**退出码**：`host-caps` 的退出码只表示“这次探测本身有没有跑完”，**不表示缺不缺东西**。一台没装
Samba 的机器是正常的，把它算成失败会让安装器拒绝安装。超时（默认 `--timeout 30s`）打半份报告并
返回 1。

**权限**：`GET /system/capabilities` 是 admin。这份清单列的是这台机器装了哪些程序、什么版本、在
哪，合起来就是主机指纹（与容器清单、部署定义同一个判断，PLAN §14）。viewer 用不到它。

**只转述，不判决**：`present` 只代表程序在固定目录里、版本读得到；某个动作能不能真的做，仍然由
那个动作自己的预检回答。把这份清单当准入条件会造出第二套判决，两套判据迟早打架。

**同时补充的 CLI 字段**：`upanelctl check-config --json` 增加 `agent_database_path`（代理库的
生效路径）。备份脚本靠它定位代理库，而不是把 `/var/lib/upanel-agent` 写死在脚本里——配置里挪过
库的位置，备份就得跟着挪。


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
| proxy.apply | site_id（Web 展开成 site_id、version、definition、panel_port；见 §9.2） | nginx+domain | 是 |
| proxy.delete | proxy_id、version | nginx+domain；不删除应用；托管配置还在时 409 | 是 |
| certificate.issue | proxy_id、email、environment=staging/production | cert+domain；明确签发环境 | 是 |
| certificate.import | proxy_id、certificate_upload_id、private_key_secret_ref | cert+domain；验证证书与私钥匹配 | 是 |
| certificate.renew | certificate_id（面板展开；定时续期为 domain、staging、scheduled，见 §12.5） | cert+nginx；允许内置定时身份 | 否 |
| package.refresh | 空对象 | apt-global | 否 |
| package.install / package.remove | packages（精确包名数组） | apt-global；模拟和保护名单 | 是 |
| package.upgrade | packages（明确选中的包名数组） | apt-global；禁止隐式 dist-upgrade | 是 |
| system.power | operation=reboot/poweroff、delay_seconds（30—300） | power-global；检查关键任务 | 是 |
| backup.repository.initialize | repository_id、version | repository；只允许新空仓库，不覆盖已有数据 | 是 |
| backup.run | backup_policy_id、version | repository+源卷；内置定时身份可用 | 否 |
| backup.restore | repository_id、snapshot_id、target_root_id、relative_path、overwrite | repository+目标卷+依赖应用 | 是 |
| backup.verify | repository_id、snapshot_id、sample_restore | repository；临时恢复目录有空间限额 | 否 |
| backup.prune | backup_policy_id、version | repository；预检列出要删除的快照，保留策略校验 | 是 |
| schedule.apply | schedule_id（新建为 null）、version、definition（证书续期计划的 certificate 由面板按证书记录展开，见 §12.5） | timers；只允许登记动作 | 是 |
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

### 7.5 P14.4 已实现范围：NFS 受管理导出（协议第二支）

共享从这一节起是**两种协议**，不是一种协议的两种配置：SMB 的访问控制是“受管理账号 + POSIX
ACL”，NFS 的是“来源网段 + `root_squash`”。所以下面每一条都先问协议，而不是在一套规则里
塞“如果不是 nfs 则……”。定义结构（`ShareDefinition`）不变，只是按协议**必填项不同**。

**（1）定义字段的按协议取舍。** 同一份 `ShareDefinition`，`protocol=smb` 与 `protocol=nfs`
要求的字段不同，混填一律拒绝：

| 字段 | smb | nfs |
|---|---|---|
| `smb_users` | 必填，不允许空名单（空名单退化成 guest，本项目禁止） | **必须为空**，出现非空即 `NFS_USERS_NOT_ALLOWED` |
| `allowed_cidrs` | 可空（空 = 不限来源，但 §7.4(4) 要求说明里写明 Samba 对本机永远放行） | **不可空**（`SHARE_NO_CIDRS`），且每一项前缀长度不能为 0（`NFS_OPEN_TO_WORLD`） |
| `nfs_options` | **不得出现**（出现即拒） | 必填，且是闭集 `{root_squash:true, sync:true}`，缺项或 false 即 `NFS_OPTIONS_INVALID` |
| `relative_path` | 空串 = 卷根，非法写法见 `ValidateRelativePath` | 同左 |
| `read_only` | 所有账号 ACL 上限 `r-x` + 共享段 `read only = yes` | 导出行写 `ro` + 目录权限 `0755`（两道锁） |

`no_root_squash`、`async`、`insecure`、`*` **不是默认关闭的可选项**，而是根本不存在于取值
空间里（PLAN §10）。要放开需要改代码，不是改一次请求参数。开放网段同理：NFS 没有账号这一
层，允许 `0.0.0.0/0` 等于向全世界导出，所以它在定义校验里就是错的。

**（2）只维护一份文件，行尾标记是认领依据。** 面板写的全部 NFS 状态都在

    /etc/exports.d/upanel.exports

这一份里，**整份重写**（不是增量追加）。每一行末尾带 `# upanel:<share_id>`：

    /srv/upanel/volumes/data/media 10.0.2.0/24(rw,sync,root_squash,no_subtree_check) # upanel:shr_…

`# upanel:` 是“这一行是面板写的”的**唯一**判据。行尾注释不会影响 `exportfs` 解析（22.04
实测），所以认领标记可以就放在行尾，不必另开索引文件。三条硬规矩：

- `/etc/exports`（系统主导出文件）**只读**，面板一个字都不改；它只被用来做冲突检测。
- 别的 `*.exports` 文件（管理员自己的）不读进面板、不导入为可编辑共享、也不被改写。
- `/etc/exports.d/` 下只有以 `.exports` 结尾的文件才会被 `exportfs` 自动读取（实测：改名
  成 `.conf` 之后导出立刻消失）。文件名因此是常量，不是配置项。

**（3）“刷新成功”不等于“表变了”——判决一律读回实际导出表。** `exportfs -ra` 的退出码不是
证据（没有可刷新的内容时它照样 0）。执行顺序固定为：

1. 执行前**重新判决**：比对 `share_id` + `version`，卷此刻是否 `mounted`（不是则
   `DEPENDENCY_BLOCKED`）、目录是否仍在该卷里（`EXPORT_PATH_CONFLICT` 在预检已被拦过一次，
   执行期再查一次）；
2. 渲染整份 `upanel.exports` 候选内容（`sha256` 即 `config_hash`）→ 原子写入（写临时文件 +
   `fsync` + rename）；
3. `exportfs -ra`；
4. **读回** `exportfs -v`（实际生效的导出表）逐条比对：路径在、`rw`/`ro` 对、`sync`、
   `root_squash`、`no_subtree_check` 都在，且没有多余的面板路径；
5. 只有第 4 步通过且 `nfs-server` 为 `active` 且卷仍挂载，`served` 才为真、`outcome` 才是
   `verified`；否则 `accepted`，并说清是哪一项没就位；
6. 第 3、4 步任何一步失败：**立刻还原旧文件**并再 `exportfs -ra` 一次，`outcome=rolled_back`，
   错误里点名 `EXPORTS_RELOAD_FAILED` 或 `EXPORT_VERIFY_MISMATCH`。回滚只撤自己刚写的东西，
   不动别人的导出文件。

`outcome` 闭集仍是 §4 那五个（`verified`/`accepted`/`rolled_back`/`partial`/`unknown`），
**没有 `failed`**。

**（4）能力报告（`capability.nfs`）。** `GET /shares` 与 `GET /shares/:id` 的 `capability`
里多一个 `nfs` 对象（openapi `ShareNfsCapability`）：`status`（`available`/`blocked`）、
`reason`、`exportfs`（工具绝对路径）、`exports_dir`、`exports_file`、`exports_dir_ready`、
`exports_present`、`exports_hash`、`exported_paths`（此刻实际被导出的面板路径）、
`main_exports_readable`、`service_state`（`active`/`inactive`/`unknown`）、`config_write`、
`notices`。判据只有一条：**有没有 `exportfs`**；没有就 `blocked`，任何 NFS 应用在预检阶段
以 `NFS_UNAVAILABLE` 阻塞（"摘不掉导出行也刷不了表"）。

`exportfs` 的落点由能力报告给出、执行层从 `Capability().Tools.Exportfs` 取——两处必须是同一
个值。这不是洁癖：`Tools` 是执行层拿工具路径的唯一入口，这里漏填，`reloadExports` 与
`readExportTable` 就永远看到空字符串，真机上表现为“每次刷新都报缺少 exportfs”。

**（5）NFS 专用的阻塞与告警码。** 与 SMB 那套**不共用**（把 `SHARE_NO_USERS` 套到 NFS 上
会得到一句自相矛盾的话）：

| 码 | 类别 | 含义 |
|---|---|---|
| `NFS_OPTIONS_INVALID` | 阻塞 | `nfs_options` 缺失或取值不在闭集里 |
| `NFS_OPEN_TO_WORLD` | 阻塞 | 允许网段里有前缀长度 0 的一项 |
| `NFS_USERS_NOT_ALLOWED` | 阻塞 | nfs 定义里带了 `smb_users` |
| `SHARE_NO_CIDRS` | 阻塞 | nfs 共享没有点名允许网段 |
| `NFS_UNAVAILABLE` | 阻塞 | 本机没有 `exportfs` 或读不到导出配置 |
| `EXPORT_PATH_CONFLICT` | 阻塞 | 这个目录已经被导出：别人的导出文件里有，或面板自己的另一条共享占着（见下） |
| `EXPORTS_UNPARSABLE` | 告警 | 别人的导出文件里有读不懂的行 → “确认没有冲突”这句话打折扣 |
| `EXPORTS_UNREADABLE` | 告警 | `exportfs -v` 失败 → “此刻在不在导出”无从回答 |
| `EXPORTS_NOT_APPLIED` | 告警 | 这条共享此刻不在导出表里（还没应用 / 被撤销） |
| `NFS_SERVICE_INACTIVE` | 告警 | `nfs-server` 没在运行：会照写、照校验，但对外不提供服务 |
| `NFS_CALCULATED_ONLY` / 其它 smb 码 | — | 不出现在 nfs 分支里 |

`EXPORT_PATH_CONFLICT` 为什么是阻塞而不是告警：`exportfs` 遇到同一条路径被导出两次会报
`duplicated export entries` 并以 1 退出，整份导出表的刷新因此失败——别人的导出也一起停止
刷新。预检先拦一次，执行期再查一次（并发窗口）。

“被导出两次”有**两个来源**，判据必须都覆盖，漏一个用户就会在预检看到 `allowed`、在执行时
收到一句 `exportfs` 的 `duplicated`：

1. **别人的导出文件**导出过这个目录（`ExportConflicts`，比的是 `/etc/exports` 与别的
   `*.exports`；面板自己文件里的行由“整份重写”负责，不参与）；
2. **面板自己的另一条共享**指着同一个目录（`SameDirShares`）。两条共享的网段与只读标记
   可以不同，合并成一行等于替用户猜他想留哪一条——所以只判定、不合并。判定按名字排除
   自己：新建共享在预检时还没有 id，名字是那一刻唯一确定的身份（登记表里名字唯一）。
   子目录不算同目录（`/x` 与 `/x/ro` 是两条不同的导出，`exportfs` 认完整路径）。

**（6）目录与权限。** 目录不存在时按 `relative_path` 创建，权限按 `read_only` 定：读写
`0777`、只读 `0755`。**不设 POSIX ACL**：NFS 走 `sec=sys`、uid 透传，服务端事先不知道谁会
连上来，访问控制只能交给网段；ACL 在 NFS 上只会变成一层“看起来在管、实际挡不住”的假象。
只读共享的两道锁是 `ro` 导出行 + `0755` 目录，二者缺一都不算只读。

停用/删除共享**只摘导出行，永远不删目录与里面的文件**；NFS 没有 ACL 可收，`acl_revoked`
显式为 `false` 且 `acl_note` 写清原因（不留空让人猜是不是忘了做）。

**（7）锁与开机依赖。** 托管导出文件是整份重写的，所以 NFS 的写与停用都取
`nfs:exports` 这把全局锁（与 SMB 的 `samba:config` **分开**：两者写的是不同文件，共用一把锁
会让不相关的动作互相排队）。锁集合 = `{share:<名字>, volume:<卷 id>, nfs:exports}`。

开机依赖写的是 `nfs-server.service.d/upanel-exports.conf`（`RequiresMountsFor=` 点名各导出
目录的卷挂载点），与 SMB 的 `smbd` drop-in 各写各的。撤销最后一个 NFS 导出时这份 drop-in
一起收回。

**（8）卷依赖与运行时撤销。** 卷掉线时那条导出会被从 `upanel.exports` 里摘掉并重新生效
（`runtime_revoked` + `config_applied=false`），卷回来**不会**自动复活——与 §7.3 同一条
“撤销只减不增”。有启用导出的卷不允许从面板卸载（`VOLUME_IN_USE_BY_SHARE`）；卷在别处掉线
（拔盘、外部 `umount`、后端消失）就走撤销循环。重新应用是唯一的出路。

**（9）页面（NFS 那一半）。** 新建表单按协议切换整块，而不是把 SMB 的字段灰掉——灰掉的字段
会让人以为“填了也许有用”，而它根本不会被写下去：NFS 下不出现账号块、不出现 SMB/ACL 的说法，
出现的是固定选项（`root_squash` / `sync` / `no_subtree_check`，不可改）与“必须点名网段”的
说明。客户端说明换成三种：`mount.nfs`（Linux）、`mount_nfs`（macOS）、`mount -o anon`
（Windows NFS 客户端，可选装）。三条硬规矩：

- **命令里没有账号，也没有密码**——NFS 根本没有这一层；出现它们是误导。
- **导出目录不编绝对路径**：`ShareRecord` 里没有卷挂载点，说明给的是
  `<卷挂载点>/<相对路径>` 占位 + `showmount -e <host>`（打印服务端此刻的导出表），
  让人问出真路径，而不是猜一个像模像样的 `/srv/...`。
- **挂完先 `findmnt`，再写**：挂载失败时客户端可能**静默写本地目录**、`mount` 的退出码
  仍然是 0。说明里 `mount` 之后紧跟着 `findmnt -T <挂载点>`，这不是啰嗦，是唯一能避免把
  数据写到本机系统盘上的动作。

**验证要求**（§16 的追加）：NFS 的验收必须

- 用 `exportfs -v` 读**实际导出表**做断言，不接受 `exportfs -ra` 的退出码；
- 以真实客户端（`mount -t nfs -o vers=3,proto=tcp`）证明读写、只读拒写、**root_squash 后
  落盘属主是 `nobody:nogroup`**，并且写入前先 `findmnt` 确认挂上了；
- 证明面板**没有改** `/etc/exports`（前后哈希一致），也没有碰别人的 `*.exports`；
- 证明网段真的是唯一访问控制：把名单换成不含客户端的一项之后，本机客户端**连不上**；
- 证明卷掉线后导出被摘掉、客户端挂不上、卷回来后不自动复活、重新应用才恢复；
- Windows/macOS 的 NFS 客户端与 NFSv4 直挂在没有对应测试条件时标 **NOT RUN**，不得用
  Linux 客户端或 v3 的结果代替。

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

### 8.6 P15.1 已实现范围：受约束部署定义、校验与 Compose 渲染（`internal/deploy`）

P15.1 交付的是**纯函数层**：定义的结构、校验、以及把一份合法定义渲染成 Compose 配置的字节。
它没有接口、没有库表、不执行任何东西——执行与落盘属 P15.2，更新与镜像 digest 属 P15.3，
页面属 P15.4。这一节写在这里的原因是：这一层的每一条约束都是契约，不是实现细节。

**（1）定义是闭集。** 十个字段，一个不多一个不少：

| 字段 | 类型与范围 |
|---|---|
| `name` | `^[a-z][a-z0-9_-]{0,62}$`；同时是 Compose 项目名与容器名前缀 |
| `image_reference` | `[registry[:port]/]repo[:tag][@digest]`，≤255 字节；仓库名必须小写；digest 只收 `sha256:`+64 位或 `sha512:`+128 位小写十六进制；tag 与 digest 都没有时补 `:latest` |
| `container_port` | 1—65535（0 表示随机端口，不接受：面板记下的入口地址会失效） |
| `host_bind_address` | **字面 IP**，不接受主机名、不接受带端口的写法 |
| `host_port` | 1—65535 |
| `environment[]` | 名字是 POSIX 标识符且不重复，≤64 条；每条的 `value` 与 `secret_ref` **二选一**（显式空串 `""` 与“没给”是两件事）；`secret_ref` 形如 `sec_`+32 位小写十六进制 |
| `mounts[]` | ≤16 条；`source_root_id` 是已登记文件根 id；`relative_path` 空或 `.` 表示根自身，其余必须是 `path.Clean` 的不动点（`..`、绝对路径、`a//b` 全部拒绝）；`container_path` 必须是绝对路径且不是 `/`、`/proc`、`/sys`、`/dev`、`/boot`、`/lib/modules` 及各类容器运行时 socket，也不能重复 |
| `resource_limits` | `memory_limit_bytes` **必填**，16 MiB—64 GiB；`cpu_limit` **必填**，十进制字符串，0.05—8 核 |
| `health_check` | `enabled` 关时其余字段整体清零；开时 `kind` ∈ {http,tcp}，`port` 只能是容器端口（0 视为容器端口），http 必须给以 `/` 开头的 `path`；间隔 5—300 秒（默认 30）、单次超时 1—60 秒且**必须小于间隔**（默认 3）、重试 1—10（默认 3）、启动宽限 0—600 秒 |
| `volume_dependencies[]` | ≤32 条，形如 `vol_`+32 位小写十六进制 |

**没有** `compose_yaml`、`privileged`、`devices`、`network_mode`、`cap_add`、`pid`、`ipc`、
`restart`、`user`、`build`、`env_file`、`extends`、`secrets`。请求体里出现这些字段不是被忽略，
而是 **400 `unknown_field`** 并点名是哪个字段——静默丢弃会让人以为自己提交成功了。

**（2）拒绝码是闭集**，界面按 `field` 高亮、按 `code` 归类：

| 码 | 含义 |
|---|---|
| `unknown_field` | 定义里没有这个字段 |
| `missing` | 必填项缺失 |
| `malformed` | 形状不对（字符集、IP 字面量、路径规范形式、digest 形态） |
| `out_of_range` | 超出范围或条数上限 |
| `duplicate` | 环境变量名或容器内路径重复 |
| `not_allowed` | 形状合法但这份定义不允许（相对路径越界、容器路径命中禁用目标、绑通配地址、只读根读写挂载、健康检查探非容器端口……） |
| `unknown_root` | 挂载引用的文件根没有登记 |
| `volume_missing` | 依赖卷不在登记表里，或挂载所在卷没有写进 `volume_dependencies` |

只报第一个错误（与 `internal/apps` 同一个口径）。

**（3）三段校验，回答三个不同的问题。**

1. **形状**（`Normalize`）：定义自己能决定的事——字符集、范围、路径形状、默认值、排序。
2. **宿主事实**（`CheckHost`）：定义自己答不了的事——这个地址是不是本机此刻的地址、这个根在不在、
   它所在的**受管理卷**还在不在、以及那个卷有没有写进 `volume_dependencies`。三者必须对得上：
   门禁守的是“声明的依赖卷”，容器吃的是“挂载所在的根”，少一环“卷缺失时重启也不绕过保护”就
   断在半路。绑定地址只允许回环、显式放开的通配地址（默认关闭）或本机某一刻真的配着的地址。
3. **秘密**（渲染时）：`secret_ref` 只经 `app_env` 用途解出明文，只存在于渲染出的字节里。

**渲染在代理侧进行**，不在 Web 侧：环境变量的秘密只存在代理的密钥环里，Web 永远看不到明文
（§14.2）。Web 只保存“哪个变量引用哪个 `sec_` 标识”；对秘密的读取接口一律只说“已设置”。

**（4）渲染是确定性的，五处引号/写法是功能不是风格。**

- 同一份定义永远渲染出同一份字节：环境变量按名字排序、挂载按容器路径排序、无时间戳。
  配置摘要 = 这堆字节的 `sha256`，**只在内部比对**（字节里有明文秘密，公开摘要等于给离线
  爆破提供靶子）。
- `restart` 恒为字符串 `"no"`。裸 `no` 在 YAML 1.1 里是布尔 `false`；而任何自动重启策略都
  意味着开机或崩溃后容器绕过依赖卷门禁自己起来。
- 挂载用**长语法**并显式 `bind.create_host_path: false`：短语法靠冒号分隔（路径里一个冒号就
  切错），而 compose 默认会在宿主路径不存在时替你建一个空目录再照常启动——数据写进系统盘、
  界面显示“运行中”。
- 所有引号标量把 `$` 转义成 `$$`：compose 会做变量插值，口令里的一个字面 `$` 会被宿主环境
  改写甚至拼进容器。
- `health_check` **不**渲染进 Compose：容器内的 healthcheck 需要一条能在镜像里跑起来的命令，
  面板不知道镜像里有什么（`CMD-SHELL wget/curl` 在 slim 镜像上永远 unhealthy，还往容器里
  引入一条 shell 命令）。它由 P15.3 在启动后从面板侧探测。
- 渲染结果里不写顶层 `name` 字段（老版本 Compose 不认），项目名由执行时 `-p <name>` 传入，
  两者必须一致。

渲染出的形态（同一份定义 `webapp`，`nginx:1.27`，一个公开变量、一个秘密变量、两条挂载）：

```yaml
# 由 uPanel 渲染，请勿手工修改：改这个文件不会回到面板定义，下一次部署会被覆盖。
# 项目 webapp；服务 webapp；容器端口 8080；宿主 127.0.0.1:18080/tcp
services:
  "webapp":
    image: "nginx:1.27"
    restart: "no"
    ports:
      - target: 8080
        published: "18080"
        host_ip: "127.0.0.1"
        protocol: "tcp"
    environment:
      "APP_MODE": "production"
      "DB_PASSWORD": "pa$$$$word"
    volumes:
      - type: "bind"
        source: "/srv/upanel/volumes/media/webapp/assets"
        target: "/usr/share/nginx/html"
        read_only: true
        bind:
          create_host_path: false
      - type: "bind"
        source: "/srv/upanel/volumes/media"
        target: "/var/cache/nginx"
        read_only: false
        bind:
          create_host_path: false
    mem_limit: 536870912
    cpus: 0.5
```

**（5）秘密读不到就渲染不出来。** 没有“当成空串”的退路：一份带着空口令起来的容器，比一份
没起来的容器难查得多。渲染器为 nil、`secret_ref` 解不出来、值里有 NUL、值超过 32 KiB，都是
`ErrSecretUnavailable`，整份配置不产出半份。

**（6）仍未实现，也没有用别的东西假装实现。** 定义与渲染的持久化、`POST /deployments` 一类
写接口、配置文件的 0600 落盘、包装 systemd 单元、执行 `docker compose`、记录实际镜像 digest、
健康探测与更新/回切，全部属 P15.2/P15.3/P15.4。`GET /deployments` 仍然是只读观测（§8.3）。

**（7）验收状态。** 本子任务只有单元测试（表驱动，含 §17 点名的恶意输入逐条）；
**没有在任何真实 Ubuntu 虚拟机上跑过**，也没有被任何生产代码路径调用（P15.2 才接线）。
按 STAGES §17 的验收条目（真实部署/升级/卷缺失重启），本阶段结论是**未验证**。

### 8.7 P15.2 已实现范围：systemd 包装单元、挂载依赖与落地（`internal/deploy`）

本子任务把 §8.6 的“渲染出一份配置”推进到“在宿主上真的落地一个受管部署”，并且**只做创建
与删除**：`deployment.apply`（`deployment_id` 为 null）、`deployment.remove`。

**（1）卷的事实从“在不在”变成“挂在哪、是不是那块盘”。** `HostEnv.Volumes` 的取值由
`bool` 换成 `VolumeFacts{MountPoint, FilesystemUUID, Mounted, MountedUUID}`，两个新判据随之
成立：

| 判据 | 拒绝码 / 字段 | 为什么 |
|---|---|---|
| 依赖卷没有可用的挂载点 | `not_allowed` / `volume_dependencies` | 没有挂载点就拼不出 mount unit，`BindsTo` 无从谈起——那只是数据库里一行好看的字 |
| 文件根的路径不在它登记的卷挂载点之内 | `not_allowed` / `mounts[i].source_root_id` | 门禁守 A 卷、容器吃 B 目录；卷没挂上也不会有任何人发现 |

`CheckVolumesReady` 是 PLAN §11 规则 2/4 的“启动前 UUID 核对”：依赖卷必须 `Mounted` 且
`MountedUUID == FilesystemUUID`。三种情形各自有明确消息——没挂载、挂的是另一块盘、读不到 UUID。

**（2）包装单元（`RenderUnit`）。** 名字 `upanel-deploy-<name>.service`（名字可写就能覆盖别人的
单元，所以由定义名派生、形状校验在拼之前）。内容要点，逐条对应 PLAN §11 规则 3—6：

| 单元里的东西 | 落点 |
|---|---|
| `BindsTo=<mount unit>` + `After=<mount unit>`，逐卷 | 规则 3/4：卷没挂上单元起不来；运行中失联 systemd 停掉单元，`ExecStop` 收项目（规则 5） |
| `RequiresMountsFor=<卷挂载点…>` | 同上，开机与手动启动两条路径都覆盖 |
| `Type=oneshot` + `RemainAfterExit=yes` | `compose up -d` 的返回值是“这次启动”的结果，不是容器运行状态 |
| `ExecStart=/usr/bin/docker compose -p <name> -f <cfg> up -d --remove-orphans --no-build` | 只拉镜像、不在本机构建；孤容器随启动清掉 |
| `ExecStop=… down --remove-orphans` | 停止只有一条路径 |
| `Requires=docker.service`、`After=network-online.target docker.service` | docker 没起来时 `compose` 必然失败，那不该被当成“启动成功” |
| `[Install] WantedBy=multi-user.target` | 容器 `restart` 恒为 `no`，开机没人拉它——单元必须自己被 enable，否则机器重启后应用永远回不来 |
| 单元里没有 `sh -c`、没有 `$`、没有命令替换 | argv 里多一个可写元素，docker 就离“任意命令执行器”近一步 |

mount unit 名按 `systemd-escape --path` 逐字符实现（`/srv/upanel/volumes/my-app` →
`srv-upanel-volumes-my\x2dapp.mount`）：段内的 `-` 不转义会与 `/` 拼出的名字撞车，拼错一个字符
则 `BindsTo` 指向不存在的单元、整个部署起不来。

**（3）落地（`Applier`）。** 只碰两个路径：`/etc/systemd/system/upanel-deploy-<name>.service`
（0644）与 `/etc/upanel/deploy/<name>.compose.yaml`（0600，里面有 `app_env` 解出来的明文秘密）。

- **部署版本 = 配置文件字节的 `sha256:<hex>`**，与 §8.6 的 `ConfigDigest` 同一个值。
- 写入是临时文件 → `fsync` → `chmod` → `rename` → 目录 `fsync`；临时文件建在目标目录里，跨文件
  系统的 `rename` 会退化成复制 + 删除，那就不是原子的。
- **幂等**：同一份定义重放时摘要相同、单元字节相同 → 不重写、不重启，仍然 `enable` + `start`
  并读回。
- **启动动词看“写了什么”**：这次真的重写了单元（新建，或单元被人改过）而盘上早就站着这个单元
  时，动词是 `restart` 而不是 `start`——`start` 对 `RemainAfterExit` 的 active oneshot 是空操作，
  `ExecStart` 不会重跑，刚写下去的配置永远读不到（观测里 `unit_restarted=true`，见（7））。
- **不幂等的部分明确拒绝**：这个名字上已有一份**内容不同**的配置 → `ErrDeploymentExists`，
  并说明换定义属 `deployment.update`（P15.3）。在 P15.2 里悄悄覆盖，等于把一次更新伪装成一次
  部署，而更新需要的拉镜像、记 digest、按选择备份、健康检查、回切边界一件都没做。
- **单元被手工改过**（内容与生成的字节不同）→ 整篇重写并报告 `Written=true`。
- **成功与否看读回**：`systemctl is-enabled` / `is-active`，不看 `start` 的退出码。`compose up`
  返回 0 但单元不是 `active` 时，错误消息直接给出 `journalctl -u <unit>` 与 `compose logs`。
  这两个动词的**退出码是答案不是失败**（`disabled`/`masked` 退出 1、`inactive` 退出 3），只有
  超时、起不来、输出超限才算读不到——把退出码当失败，会让"删除一个已经停掉的部署"永远删不掉
  （真机 boot 段暴露，见 DECISIONS Q486）。
- **删除**：`systemctl disable --now`（`ExecStop` 跑 `compose down`）→ 不 `active` 才删两个文件 →
  `daemon-reload`。停不下来就**不删**（`ErrUnitStillActive`）：留下一个在跑、却没有单元文件的
  部署，下一次 apply 会当成“可以从头创建”。版本令牌对不上 → `ErrVersionMismatch`，一个字不动。
  **数据卷与业务目录一个字节都不动**（CONTRACTS §5 对 `deployment.remove` 的明文要求）。
- `Find` 按部署标识反查名字：标识是名字的 SHA-256 派生值，不可逆，只能扫单元目录比对；
  只认 `upanel-deploy-<合法名字>.service`，不匹配就跳过，命中多个则拒绝猜测。

**（4）`deployment.apply` / `deployment.remove` 的两阶段（`Service`）。**

- `AssessApply`：`Normalize` → `CheckHost` → `CheckVolumesReady` → `Render`（秘密读不到则整份
  不产出，进 blockers）→ `PlanUnit` → `Inspect`（磁盘上已有哪一份、摘要是什么）。返回的定义是
  **规范化后**的那一份，预检绑定的就是它。
- `ExecuteApply` 把上面每一步**重跑一遍**再落地：秘密可能在预检与执行之间被轮换、卷可能刚被
  卸下、单元可能刚被人改过。渲染摘要与预检时不同→ `stale`；状态令牌不同 → `stale`。
- `deployment.apply` 带非 null `deployment_id`（且等于名字派生值）→ `ServiceError{Code:
  "update_not_implemented"}`；标识与名字对不上 → `malformed` / `deployment_id`。
- `deployment.remove` 找不到 → blocker `no_such_deployment`。删除的预检永远带一条
  `data_kept` 告警：删的是单元与配置，不是数据。
- 健康检查（`health_check.enabled`）在预检里给一条 `health_probe_pending` 告警：**这一版不探测
  HTTP**，部署成功后核对的是 systemd 单元状态与容器状态。P15.3 接线。

**（5）接线（`internal/agent/deployaction.go`、`deployfacts.go`）。** 两条动作已经登记进生产登记
表（`RegisterDeployActions`），§5 表格里那两行现在是**动作**，不只是契约：

| 接口 | 形状 |
|---|---|
| `DeploySource` | `AssessApply` / `ExecuteApply` / `AssessRemove` / `ExecuteRemove`，生产实现是 `*deploy.Service` |
| `DeployFacts`（产出 `deploy.HostEnv`） | 受管理卷 → `VolumeFacts{MountPoint, FilesystemUUID, Mounted, MountedUUID}`，挂载事实读自拓扑采集器（网络挂载也算“挂着别的东西”）；文件根 → `RootFacts{Path, Writable, VolumeID}`，`VolumeID` 按**最长挂载点前缀**算出；`DataBase=/srv/upanel/volumes`；本机地址表来自网络采集器，每次调用重新问一遍（刚改完的地址必须在预检里存在） |
| `DeploySecretResolver` | `app_env` 的 `secret_ref` 走密钥环；解出来的明文只进渲染结果，不进观测、不进日志 |

- 登记了卷却没有拓扑来源、或拓扑状态不是 OK 时，`HostEnv` **报错**而不是返回空表：判断不出卷
  挂着没有，就不在这个状态上部署。
- 不在任何受管理卷里的文件根**照实登记**（`VolumeID` 留空），让校验层给出“这个根不受依赖卷门禁
  保护”，而不是把它藏起来（藏起来的结果是界面上那个根凭空消失）。
- docker 路径固定 `/usr/bin/docker`（`deploy.DockerBinary`，单元里逐字写死）：PATH 里的 `docker`
  可能是一个同名脚本。
- 单元起来之后多做一次**容器交叉核对**（按 compose 项目名数容器）。核对失败不降级成“部署成功”：
  观测里 `containers_checked=false` 加 `containers_error`，任务结论照它说。
- 锁：`compose:<name>`、`port:<proto>/<addr>:<port>`、`volume:<id>`（去重后排序）；删除只锁
  compose 项目（它释放端口、不动卷）。
- 进度步骤名只有两个：`writing`（写单元与配置）、`stopping`（删除时停单元）。只报**已经开始做**的
  步骤——被拒绝之后还挂一条“准备中”，人会以为部署已经开始。

**（6）版本令牌与“新建”的冲突（真机首跑暴露，已修）。** 代理层要求一条 allowed 的预检绑定非空
`Target.StateVersion`（`validatePlanResult`），而新建部署在磁盘上没有旧版本。因此
`deploy.deploymentStateVersion` 给出四种非空、可重算的令牌：有渲染配置 → 它的摘要；盘上什么都
没有 → **这一次将要落盘**的摘要；单元还在而配置被删 → `unit-only`；都没有 → `absent`。执行前复核
仍抓得住漂移（定义或秘密变了，摘要就变 → `stale`）。详见 DECISIONS Q484。

**（7）落在已有单元上：`start` 是空操作（真机首跑暴露，已修）。** 上一轮崩溃会留下一个
`active` 的包装单元（`Type=oneshot` + `RemainAfterExit=yes` 会一直“活着”），而配置目录可能已经
不在了。这时 `apply` 写单元写配置、`systemctl start` 立刻返回 0、`is-active` 读回 `active`，
**容器一个都不会起**——真机 P15.2 §5 就是这么读到 `containers_running=0` 的（任务因此诚实地报
`partial` 而不是 `verified`，这一层判决本来就是对的）。修法不是放宽判据，而是让动词对得上现场：
盘上已经有这个单元、这次又真的写了东西 → `restart`（`ExecStop` 收项目、`ExecStart` 拿新配置
重新 `up`），并把这个事实写进观测 `unit_restarted`。`start` 只在盘上本来没有单元时使用。

**（8）仍未实现，也没有用别的东西假装实现。**

- `deployment.update`（P15.3）、镜像 digest 记录、按选择备份、健康探测与回切、`/deployments` 写
  接口与 UI（P15.4）。
- bind mount 之前的**句柄级**路径核对（软链接在解析后替换）仍未实现：现在靠
  `path.Clean` + 前缀判据 + 渲染时的路径解析，挡得住 `..` 与绝对路径，挡不住“解析后被换成
  软链接”。这条限制在 §8.6 与 PROGRESS §8 里各记了一条。

**（9）验收状态（真机：VM-A，Ubuntu 22.04.5，docker 29.1.3 / compose 2.40.3）。** 证据目录
`docs/evidence/P15/2026-10-27-p15.2/`（`RESULT`、`RESULT-boot`、两段完整控制台、`facts/`、`logs/`、
`payloads/`）。

| 段 | PASS | FAIL | NOT RUN |
|---|---|---|---|
| `PHASE=main`（准备现场 → 部署 → 重放 → 判决层 → 停止与卷依赖 → 留状态并重启） | 199 | 0 | 2 |
| `PHASE=boot`（重启后不自启 → 依赖恢复后起来 → 删除保留数据） | 51 | 0 | 0 |

STAGES §17 的三条验收逐条落地：①**真实部署**——`deployment.apply` 在真机上写出单元与配置、
`systemctl start` 后读回 `enabled`/`active`、容器真的在跑、宿主端口 HTTP 200；重放不重写不重启
（`written=false`、`unit_restarted=false`、容器 ID 与 `StartedAt` 不变）。②**卷缺失重启不绕过**
——故障注入 `systemctl mask <mount unit>` 后 `systemctl start` 失败（`Unit … is masked.`），
单元不 active、没有容器、卷没挂上、没有替我们建目录；重启后单元 `enabled` 但 `inactive`，系统盘上
没有多出数据，解除 mask 并挂回卷之后单元自己起得来。③**删除保留数据**——`deployment.remove`
后单元与配置都不在了、`systemd` 报 `not-found`，业务目录还在、`marker.txt` 逐字节一致、卷仍挂着。

两条 NOT RUN 各有理由：`容器里读到的 marker`（`traefik/whoami` 是 scratch 镜像，容器内没有
`cat`/`sh`，挂载改由 `docker inspect` 的 `Source` 与 `findmnt` 交叉核对）；`运行中卷失联 →
systemd 自动停单元`（容器把那块卷 bind 进自己的 mount namespace，内核里是 busy，只能靠
`umount -l` 造假现场，那样的读数不算证据）。

**仍未验证**：`deployment.update` 与镜像 digest、按选择备份、健康探测与回切（P15.3）；
`/deployments` 写接口与页面（P15.4）；24.04 上的 systemd/docker 差异；bind mount 之前的句柄级
路径核对（（8）里的那一条）。真机首跑暴露的四个产品缺陷与两个"假件/脚本挡住测试"的教训见
DECISIONS Q484—Q486。

### 8.8 P15.3 已实现范围：部署记录、更新、健康探测与回切（internal/deploy）

**（1）三个文件，谁也不替谁作证。**

| 文件 | 权限 | 是什么 |
|---|---|---|
| `<name>.compose.yaml` | `0600` | 渲染字节，含明文秘密。**部署版本就是它的 `sha256:<hex>`** |
| `<name>.compose.yaml.prev` | `0600` | 上一版的渲染字节，回切时逐字节还原 |
| `<name>.deployment.json` | `0640` | 规范化后的定义 + 镜像记录（只有 `sec_` 引用，无明文秘密） |

为什么定义必须落盘：`apply` 落的是**渲染后的字节**，不是定义。拿一份 Compose 配置反推定义
（解析 YAML、再把明文口令塞回 `secret_ref`）既不可靠也危险，而 `update` 要做的恰恰是"在同一份
定义上只换镜像"。P15.2 起每次成功落地都顺手写一份记录——注意这条是**后加的**：P15.2 期间创建的
部署盘上没有记录，`update` 对它们一律拒绝（`no_definition_record`），只能 `remove` 后重新
`apply`。读取失败分两种，不许混：`ErrRecordMissing`（盘上没有）与"有但不是合法 JSON"
（`requireRecord` 报 `ErrRecordMissing` 的包装，两个都拒绝，但消息不同）。

镜像记录里两个 digest 是两件事：`Image.Requested` 是人话（`nginx:1.27`，可变），
`Image.Digest` 是 `docker image inspect` 的 `.Id`（唯一身份）。**空串只表示"没有读数"，不表示
"一样"**——观测里另有 `image_digest_known` 说这件事。

**（2）`deployment.update` 的动作形状（`internal/agent/deployaction.go`）。**

- 参数：`deployment_id`、`version`、`image_reference`、可选 `backup_policy_id`。四个字段都修剪空白。
- 判决层给的是**评估**不是 500：`AssessUpdate` 的 blocker 走 `deployPlanFromError`，与 apply 同一套。
  影响面五种：`backup_before_update`、`image_pull`、`unit_restart`、`health_probe_rollback`、
  `deployment_record`。
- `StateVersion` = 盘上配置的摘要（`deploymentStateVersion(state, "")`），执行前复核用它抓漂移。
- 锁：`compose:<name>`、`port:<proto>/<addr>/<port>`、`volume:<id>`，再加备份那两个
  （`backup-policy:<pol_id>`、`backup-repo:<repo_id>`）。**更新期间同一个仓库上不该同时跑一次
  `backup.run`**——两份快照在同一个 restic 进程池里抢锁，运维看到的是"备份莫名其妙失败"。
- 进度步骤名只有五个，且只报**已经开始做**的步骤：`stopping`、`backup`、`pulling`、`writing`、
  `probing`。回切不报新的开始步骤（它是失败路径上的收尾，不是计划里的一步）。

**（3）执行顺序，以及每一步为什么要这个顺序。**

1. 执行前复核：`AssessUpdate` 全部重跑。镜像 tag 是别人的东西、卷可能刚被卸下、记录可能刚被人
   改过——三种都答"现在不行"。秘密在预检与执行之间变了（渲染摘要不等）也拒绝，`code=stale`。
2. **要先备份就先停单元**（`stopping`）：容器还在写的时候拷出来的目录，只是一份看起来像备份的
   东西。停不下来（`StopUnit` 之后读回不是 inactive）就不开始备份——`start`/`stop` 对
   `Type=oneshot` 的空操作历史见 Q485。
3. 备份（`backup`）：`SnapshotID` 是唯一算数的证据；有快照但没有 `restic stats` 读回核对
   （`Verified=false`）同样不算"备过"。**没建成就不换镜像**，`code=backup_failed`，配置没写、
   镜像没换，单元按原样起回。
4. 拉镜像（`pulling`）。没选备份时这一步在写盘之前（不用先停，`restart` 会自己 down 再 up）。
5. 定义里钉了 `@sha256:…` 时，拉回来的 `.Id` 必须等于 `ExpectedImageDigest`，否则
   `code=image_digest_mismatch`，拒绝换上（镜像被动过，或 tag 指向了别的字节）。
6. 旧字节留档：`SavePreviousConfig` 写 `<name>.compose.yaml.prev`，**写之前再核一次**盘上摘要
   是否仍等于预检时的值（不等就 `stale`）——从这一步起回切有据可依。
7. 写新配置并拉起（`writing`）。这里需要两个 `ApplyRequest` 字段，它们各自只解决一件事：
   - `ReplaceExisting: true`——"盘上那份不同内容的配置就是要被换掉"。没有它，`Apply` 的
     `ErrDeploymentExists` 判据（P15.2 用来挡住"同名不同内容"的重建）会把更新挡在门外。
   - `MustRun: true`——必须 `restart` 而不是 `start`。`systemctl start` 对 `active` 的 oneshot
     是空操作（Q485），更新会在"配置换了、容器没换"上安静地成功。
8. 健康探测（`probing`），见（5）。

失败路径上的"起回来"走 `Applier.RestartUnit`，**不走 `Apply`**：`Apply` 会重新判断写不写文件，
而这时文件是什么状态恰恰不确定；只 restart 不碰文件，语义最小。

**（4）回切：原样字节，数据不跟着回切。**

`rollback` 把 `<name>.compose.yaml.prev` 的**原始字节**写回盘上并 `restart`，再探一次。
**不重新渲染**上一版：预检与执行之间秘密可能被轮换，重新渲染出来的字节与上一版不等，那就不叫
回切了。

两条"回切成功"的拒绝码与别的拒绝码不同：

```go
CodeUpdateFailedRolledBack = "update_failed_rolled_back" // 换上之后起不来，已回切
CodeHealthFailedRolledBack = "health_failed_rolled_back" // 探测没通，已回切
```

代理层据此把结果记成 `Outcome = rolled_back` **且 error 为 nil**：失败任务的结果按契约是空对象，
而回切现场最有价值的恰恰是读回值（回切后的摘要、单元状态、`rollback_health_*`）。回切本身也
失败才是错误（`code=update_failed_rollback_failed`），消息里带上 `.prev` 的路径。

**回切不碰数据。** 有数据库迁移的应用，新版本可能已经动过数据，旧版本读不了它——这不是无损
回滚。这句话逐字出现在 `CodeUpdateFailedRolledBack` 与 `CodeHealthFailedRolledBack` 的错误消息
里（"数据没有回切——如果新版本已经动过数据，旧版本可能读不了它"），不许软化。

**（5）健康探测：探公开入口，不探容器端口（`internal/deploy/probe.go`）。**

探的是**宿主绑定地址 + 宿主端口**，因为那才是运维与反向代理看到的东西。通配地址
（`0.0.0.0`、`::`）换成 `127.0.0.1`；不跟随重定向；禁 keep-alive；`http` 判状态码在
`200—399` 之间，`tcp` 判能建连。有 120 秒预算（`HealthProbeBudget`），预算不够探满时
`Truncated=true`——**被截断的探测不算探通**。启动宽限期（`health_check.start_period`）内的
失败不记成失败。只读 4 KiB 响应体，读完即关——探测只为拿状态码，不替应用读数据。

`HealthProbe` 是三值不是二值：`Checked=false` 表示"没探成"（这是告警，不是失败），
`Checked && !Healthy` 才是失败。这个区分很要紧——把"探不成"当成"探不通"会把一次好的更新回切
掉，反过来会把一次坏的应用报成成功。

**（6）时间预算：在 10 分钟的硬线里切段（`internal/deploy/update.go`）。**

操作本身有 10 分钟总预算（`agent.OperationTimeout`，主进程侧的硬线），更新在里面切：

```
UpdateBudget            = 9 min     更新自己的总预算
UpdateBackupBudget      = 360 s     更新前那次备份的上限
UpdateStopBudget        = ComposeDownTimeoutSeconds + 60 s
UpdateRollbackReserve   = 120 s     每次 stepBudget 都预留：写回旧字节 + restart + 再探一次
UpdateMinStep           = 5 s       一步至少需要这么多，不够就不开始这一步
```

备份那一段必须收窄：`backup.run` 本身动辄数小时的上限，放在更新里就是"安静地跑四十分钟、最后
因为操作超时把应用留在停止状态"。**超预算的备份停掉，本次更新什么都不改地失败**，并明确告诉人
"先用 `backup.run` 单独跑一次"。`stepBudget` 在**动手之前**判断剩余时间够不够，不够就失败——
不在做到一半的时候被掐断。

**（7）只接受 `files_only` 备份策略。**

`stop_managed_apps` 需要 Web 侧展开的"应用运行对象"（哪些单元、什么顺序、怎么等），
`deployment.update` 的参数里没有这些。给 `stop_managed_apps` 的策略在预检被拒
（`code=backup_policy_needs_apps`），执行时也再拒一次（预检与执行之间策略可能被改）。

**（8）`deployment.apply` 现在也做健康探测，`health_probe_pending` 告警已删除。**

§8.7 里那条"这一版不探测 HTTP，给一条 `health_probe_pending`"随 P15.3 接线作废：
`ExecuteApply` 在 `started` 之后、`recordAfterApply` 之前探测，写进 `ApplyOutcome.Health`。
探测不改变 apply 的判决（apply 不负责"应用能应答"），但探测失败时观测降级成 `partial`——
**只在原本是 `verified` 时覆盖**，不把 `rolled_back`/`unknown` 往下拉。`health_*` 与
`warnings` 都落在 `ObservedState`（`Result` 没有 `Warnings` 字段）。

**（9）仍未实现，也没有用别的东西假装实现。**

- `/deployments` 写接口与页面（P15.4）：UI 侧"选镜像 → 选备份策略 → 发起更新 → 看回切结果"
  整条链路还没有。
- 回切之后的**数据**一致性检查（见（4））：这一版明确不做，也不假装做。
- 探测只覆盖 `http`/`tcp` 两种 kind；脚本类健康检查（`exec`）不在契约里。

**（10）验收状态。**

**已验证（宿主）**：`go test ./internal/deploy/ ./internal/agent/` 全绿，含
`internal/deploy/update_test.go` 的 13 个用例、`record_test.go`、`probe_test.go`、
`internal/agent/deploybackup_test.go`；`sh scripts/check.sh` **PASS 14 / FAIL 0 / NOT RUN 0**。
单测抓到三个**产品缺陷**，都不是测试问题：`ErrDeploymentExists` 挡住更新、配置只在 `!exists` 时写、
`restartQuietly` 静默失败（细节见 DECISIONS Q493）。

**已验证（真机，2026-10-27，VM-A Ubuntu 22.04.5 / docker 29.1.3 / compose 2.40.3 / restic 0.12.1）**：
`tests/integration/p15-3-acceptance.sh` 单段 `PHASE=main` **229 PASS / 0 FAIL / 1 NOT RUN**，退出码 0，
证据 `docs/evidence/P15/2026-10-27-p15.3/`。这里的断言都读字段、并与独立工具交叉核对，不扫响应体：

- 记录：`0640`、属主 root；`image.digest` 与 `docker image inspect -f '{{.Id}}'` 逐字相同；
  除 `sec_` 引用外无秘密残留；首次建站**没有 `.prev`**。
- 正常更新：`ab9215e4…` → `e406bf01…`，`previous_config_digest` 等于更新前那个；
  `config_changed=true` / `idempotent=false` / `unit_restarted=true` / `health_ok=true`；
  `.prev` 的 sha256 与字节数与更新前逐字相同；三份文件 0600 / 0600 / 0640。
- 同镜像重放：`config_changed=false` 而 `unit_restarted=true`，预检给 `config_unchanged`
  （预检 `warnings[]`，不是执行期观测）。
- 回切：换成 `busybox:1.36.1` 后 104 秒（4 次尝试等满超时）→ `outcome=rolled_back`、`error` 为空、
  `health_ok=false`、`rollback_verified=true`、`rollback_health_ok=true`；盘上字节回到上一版、
  `.prev` 未被动、容器回到上一版镜像、HTTP 又通；**记录未被改**。
- 按选择备份：`unit_stopped=true`、`backup_verified=true`，快照在任务观测与独立 `restic snapshots`
  两处都能读到（标签含 `upanel:consistency:files_only`）。
- 拒绝与失败：`backup_policy_needs_apps` / `stale` / `record_mismatch` / `no_definition_record` /
  `update_not_implemented` / `deployment_exists` 之后盘上配置与容器一字节未动；钉一个拉不到的
  digest → 预检放行、执行期 `failed`、配置与 `.prev` 未动、容器仍在跑、单元仍 active、服务仍应答。

**仍然只有单测读数（真机造不出那个现场）**：`image_digest_mismatch`——注册表按 digest 寻址，
钉错 digest 会让 `docker pull` 先失败，走不到该判定（真机上验到的正是这一条：`failed to resolve
reference … not found`，此时配置零改动）。另外未覆盖：把时间耗到段边界逼出 `partial`、
连续两次失败的回切、24.04 的 systemd/docker 差异。

### 8.9 P15.4 已实现范围：部署现场的管理面只读接口与部署页

**两张表回答两个问题，谁也不替谁作证。**

| 接口 | 读什么 | 回答的问题 |
|---|---|---|
| `GET /deployments`（§8.3，未改动） | docker 的 compose 项目 | 这些容器此刻在不在跑 |
| `GET /deployments/managed` | `/etc/upanel/deploy` 下的三个文件 + systemd 的两个回答 | 这些是**面板部署的**哪几份、版本是什么、能不能更新 |

两张表按 `deployment_id` 连起来：它与 `containers.ProjectID(name)` 逐字相同（`dep_` + 名字的
sha256 前 32 位）。一个项目**只**出现在观测面是正常现象——有人在面板之外
`docker compose up` 起来的——那种东西管理面不给它编造版本；反过来，配置还在盘上而单元文件
被人删了，也只在管理面看得见。

**接口形状。**

- `GET /api/v1/deployments/managed` → `{deployments: ManagedDeployment[], skipped?: [...], truncated: bool}`；
  没有部署时 `deployments` 是 `[]`，不是 `null`。
- `GET /api/v1/deployments/managed/:name` → 单个 `ManagedDeployment`。
- 都是 admin；都是只读——**创建、换镜像、删除不在这两个接口上**，仍然走
  `deployment.apply` / `deployment.update` / `deployment.remove` 的预检 → 任务（§5）。这一层
  没有、也不会有“直接部署”的接口：绕过预检就绕过了幂等、资源锁、执行前复核与审计。
- 名字形状 `^[a-z][a-z0-9_-]{0,62}$`；不合法 → 400 `VALIDATION_ERROR`（`field=name`），
  三样文件全不在 → 404 `NOT_FOUND`（`code=no_such_deployment`），代理连不上 → 503。

**字段语义（四条容易读错的）。**

1. `version` ≠ `config_digest`。`version` 是**更新与删除要带回来的版本令牌**，由「这个名字此刻在
   磁盘上的样子」压出来：有渲染配置就是它的 sha256；盘上什么都没有就是 `absent`；单元还在而配置
   被人删了是 `unit-only`。它**永远不为空**。`config_digest` 只是盘上那份渲染配置的 sha256，
   配置不在时是空串。
2. `record_present=false` 与 `record_error` 是两件事：前者是“盘上没有定义记录”（P15.3 之前创建的
   部署），后者是“有这份文件但读不了”（不是合法 JSON、格式版本不对）。两种都不能更新，
   但原因不同，页面照实说。
3. `config_drift=true` 说明盘上配置的 sha256 与记录里的不一致——有人在面板之外动过那份文件。
   更新的预检会以 `record_mismatch` 拒绝。
4. `unit_state.known=false` **必须**带 `reason`：单元文件不在磁盘上（部署没落地）与 systemctl
   没答（systemd 出问题）对管理员意味着完全不同的事（去翻文件 vs 去翻 journal），不能压成
   同一个“未知”。单元文件不在时**不问** systemctl——那会得到一条与事实无关的读数。

**读取上限与截断。** 单次最多读 200 份（`deploy.MaxInspections`）。配置目录里的条目分两类：
后缀是 `.compose.yaml` / `.deployment.json` 的收名字（`.prev` 与无关文件不列）；能读成部署的进
`deployments`，名字不合法之类的进 `skipped`（附 `reason`）——**不许静默丢掉**，那是一份面板
不知道的东西。超过上限时 `truncated=true`，调用方必须说出来。

**代理侧（`internal/agent/deployquery.go`）。** 两条查询 `deployments.list` / `deployments.get`，
实现端是 `agent.DeployInspector`（`Inspect` / `List`），与 `agent.DeploySource`（六个写方法）
**分成两个字段**：能读出现场与能执行动作是两件事，只接了其中一个的代理不该声称另一个可用。
`systemquery.ProductionDeps.DeployInspector` 为 nil 时不登记这两条查询（能力表不许撒谎）；
`cmd/upanel-agent` 在 Applier 建成之后就登记读取端，只有 docker 不可用时才只登记读取端
（读现场不需要 docker：三个文件与 systemctl 就够）。`deployments.get` 在解码期校验名字形状，
非法名 → `ErrParamsInvalid` → 400，**不进实现端**。

**页面（`/apps/deployments`，`meta.moduleKey='apps'`）。** 挂应用模块下而不是去占导航里那一格
“容器”：导航固定 16 项、只有 `containers` 还是 `implementedIn: null`，而这一页读的是受管部署，
不是容器与镜像管理——用它去顶那一格属于超承诺。与账号挂在“共享”下同一个处置（Q475、Q495）。

页面按 STAGES §17 的明文要求**禁止**三样东西，而且不是“换个说法绕过”：

- **没有原始 Compose YAML 输入框**：定义是闭集（§8.2），`compose_yaml` 在数据模型里没有位置，
  页面也就变不出这个字段；留一个再拒，比不留更糟。
- **没有危险宿主挂载**：挂载只能选“受管理文件根 + 根内相对路径”，容器内目标禁 `/`、`/proc`、
  `/sys`、`/dev`、`/boot`、`/lib/modules` 与三个容器 socket 路径（含其子路径）。
- **没有特权选项**：`privileged` / `devices` / `network_mode` / `cap_add` / `user` 都不在表单里，
  也都写不进请求体（代理侧 `DisallowUnknownFields` → 400 `unknown_field`）。

本地校验（`web/src/api/deployments.ts` 的 `validateDeploymentDraft`）只是**立即反馈**：它拦下的
东西代理侧一律还会再拦一次。真正的判决在预检——它要看宿主事实（卷在不在、秘密读不读得到、
端口有没有被占）。`updateHint` / `removeHint` 同理：它们只解释**已知的**拦路条件（没有记录、
盘上被改过、版本令牌读不出来），返回 null 也只代表“没有已知的拦路条件”，不代表一定成功。

**已验证（宿主单测，2026-10-30）。**

- `internal/deploy`：`TestInspectReadsDeploymentFacts`、`TestInspectReportsConfigDrift`、
  `TestInspectWithoutRecordStillLists`、`TestInspectSurfacesBrokenRecord`、
  `TestInspectMissingDeployment`（断言**没调** systemctl）、`TestListEnumeratesConfigDir`（孤立
  记录 + 非法名 + 孤立 `.prev` + 无关文件）、`TestListMissingConfigDir`（`deployments` 是 `[]`）、
  `TestListTruncates`、`TestInspectionJSONShape`。
- `internal/agent`：`deployquery_test.go` 五个用例（列表、参数拒绝 400、单条 200/404/400、
  读盘失败 500、没有实现端就不登记）。
- `internal/agentclient`：缺 `deployments` 键与空数组被分开（前者 `UnavailableError`）。
- `internal/api`：`deployments_test.go` 六个用例（401/403、列表、空列表是 `[]`、详情 + 非法名
  400 + 编码斜杠“不是 200 且没问过读取端”、三种错误三种状态码、没有读取端时 503）。
  gin 同时注册 `/deployments/managed`、`/deployments/managed/:name` 与 `/deployments/:id`
  不 panic（静态段优先匹配）。
- 前端：`web/src/api/deployments.test.ts` 19 个用例、`jobresult.test.ts` 的部署段 5 个用例、
  `router/routes.test.ts` 新增一条（部署页挂在应用模块下，且不点亮“容器”），
  `vue-tsc` 与 `vite build` 通过；整轮 `sh scripts/check.sh` **PASS 14 / FAIL 0 / NOT RUN 0**
  （首跑 gofmt 抓到 5 个未格式化文件，`gofmt -w` 之后重跑；`git diff -w` 为空，只有空白改动）。

**真机读数（2026-10-28，22.04.5；`docs/evidence/P15/2026-10-28-p15.4/`）。**

这一页原先在真机上是**点不通的**：三条动作都是 R=是，而**登录不开再认证窗口**，页面只把
“这一步要求再认证”写成确认卡上的一句文案，于是按下确认必然 403 `REAUTH_REQUIRED`——页面链路
一次都走不完（P15.2/P15.3 验的是脚本直接打接口，不是这一页）。现在页面自己开门：预检
`requires_reauth=true` 而 `session.reauth_expires_at` 不在有效期内时，摆出再认证卡（面板密码 +
开了 TOTP 才出现的验证码），`POST /auth/reauth` 成功后 `session.restore()`，再把被打断的那一步
接着提交。**面板密码只走这一条路径**，不进任务参数、不进日志；5 分钟窗口内的后续动作不再要密码。

真机上走完的整条链路（43 条断言全过）：登录 → 空状态（“这台机器上还没有受管部署”，页面同时
说明自己读的是管理面）→ 新建（`nginx:1.27-alpine`，`127.0.0.1:18089` → 容器 80，128 MiB / 0.5 核）
→ 预检 `allowed=true` → 再认证 → 任务 `verified` → 列表（`dep_…`、阶段取自 `unit_state`）→
详情（配置与单元路径、非空 `sha256:…` 版本令牌、闭集、资源上限、上一版配置）→ 换镜像
（`nginx:1.28-alpine`，确认卡写明会回切）→ 窗口内不再要密码 → 任务 `verified` → 详情刷新出新镜像
并留着上一版 → 删除（确认卡写明数据卷与业务目录不动）→ 任务 `verified` → 回到空状态。
面板侧 57 条请求全是 200/202；三次预检的请求体里没有 `compose_yaml` / `privileged` /
`network_mode` / `cap_add` / `devices`。

**仍未验证。** 受管目录里 200 份以上的截断、`skipped` 非空、`config_drift` 真实发生这三种现场
仍然只有单测；**“删除没动卷数据”这次只有文案证据**（新建的部署没有挂载卷）；24.04 上的渲染
与表单行为没有读数；任务执行中读现场可能顶到 10 秒预算，超时被报成 `AGENT_UNAVAILABLE`
（KNOWN_LIMITATIONS C12）。

**仍未实现。** `/deployments` 的 REST 写接口（创建/更新/删除仍然只有动作通道）、
容器与镜像管理（导航里“容器”那一格仍是空的）、24.04 差异。

## 9. 反向代理与证书契约

GET /proxies、GET /proxies/:id、GET /certificates：admin。

POST /certificates/uploads：admin+再认证+CSRF，multipart 单个 PEM 公钥证书文件，最多 1 MiB；返回 certificate_upload_id，绑定会话，10 分钟到期。只暂存输入，导入生效必须执行 certificate.import 任务。

ProxyDefinition：app_id（可空）、domain、upstream_target_id、upstream_scheme、upstream_port、access_mode、allowed_cidrs、basic_auth_users、websocket_enabled、max_body_bytes、read_timeout_seconds、enabled。

domain 为一个不带协议/路径/端口的有效域名；初期不允许通配符。basic_auth_users 为 name + secret_ref，返回只含 name。CertificateSummary 返回 id、domains、issuer、not_before、not_after、renewal_state、last_error，不返回私钥路径或内容。

手动证书公钥通过受限上传接口接收，大小不超过 1 MiB；私钥通过 /secrets 上传。证书申请与续期不接受任意 Certbot 参数。

### 9.1 P10.1 已实现范围：发现、冲突判断与站点登记（不写配置）

本节只覆盖**登记与判断**。配置生成、原子写入、`nginx -t` 门禁、reload 与失败恢复是 P10.2；
证书是 P10.3。P10.1 期间 `/etc/nginx` 一个字节都不会被写。

| 方法与路径 | 权限 | 行为 |
|---|---|---|
| GET /proxies | admin | 站点定义列表（`limit`、`app_id`、`domain` 过滤），并带一份当前 Nginx 现状摘要 |
| GET /proxies/nginx | admin | 主机 Nginx 现状：可用性、版本、单元状态、配置根、文件清单（含每个 `server` 块的 `server_name` 与行号）、`include`、解析失败的文件 |
| GET /proxies/conflict?domain=&exclude_id= | admin | 域名可用性三态结论与证据 |
| GET /proxies/:id | admin | 单个站点定义 |
| POST /proxies | admin + CSRF | 新建，返回 201 |
| PATCH /proxies/:id | admin + CSRF | 局部修改，必须带 `version` |
| DELETE /proxies/:id | admin + CSRF | 删除，必须带 `version` |

**（1）事实与判断分开。** 代理只登记一条查询 `nginx.state`（参数 `{fresh: bool}`），返回主机事实；
“这个域名能不能用”由 Web 侧的 `internal/proxies` 判断。判断需要数据库（面板自己有没有同名记录），
事实需要 root，两边都不替对方猜。

**（2）展示走缓存，写入门禁走直读。** `fresh=false`（页面）走代理侧 5 秒快照：同一次请求里前后两个
域名依据同一时刻的事实。`fresh=true`（写操作前的冲突判断）不吃缓存——缓存窗口里正好可能有人手工
加了一条 `server_name`，而那是唯一需要判准的时刻。Web 侧**不设第二份缓存**。

**（3）冲突三态，`unknown` 与 `taken` 一样阻断。**

- `clear`：没有任何现有配置声明这个域名。
- `taken`：有配置声明了它（精确、通配符、`*.x` 的子域），或面板自己已有一条同域名记录（`exclude_id`
  指向自己时不算）。`matches` 给出 `file` / `line` / `name` / `managed`，`line` 是 `server_name` 那一行。
- `unknown`：判不了——读不到配置、有文件解析失败、有正则或不支持的 `server_name`。**也阻断**，因为
  `nginx reload` 不会因为 `server_name` 撞车而失败，它只会安静地用第一个块。

被阻断的写操作返回 409，details 带 `matches` / `undecidable` / `broken_files` / `existing_site_id`；
读不到主机事实返回 503 `AGENT_UNAVAILABLE`，不是 400——那是“现在判不了”，不是“你的域名有问题”。

**（4）ProxySite 载荷。** 平铺 `ProxyDefinition` 的 11 个字段，外加 `id`、`version`、`created_at`、
`updated_at`。**没有** `applied_*`、`config_path`、`config_hash`：P10.1 不写配置，`enabled` 只表示
“用户希望它生效”，不表示“已生效”。`basic_auth_users` 返回只含 `name`；`secret_ref` 永不出现在响应里，
审计明细里也没有。

**（5）PATCH 的语义。** 所有字段都是可选的指针；`version` 必填，`version` 与库中不一致 → 409
`VERSION_CONFLICT`；一个字段都不给 → 400。改域名会重新跑冲突判断（排除自己那条记录）；改域名以外的
字段不会因为“Nginx 里有同名配置”被挡。因为响应里拿不到 `secret_ref`，**修改 `basic_auth_users` 必须
重新提交**每个账号的 `secret_ref`，只给 `name` 会被当作密码引用非法而 400。

**（6）校验与默认值（与 nginx 自身的默认对齐）。**

- `domain`：IDNA 转 A-label、转小写、去掉一个尾点；不接受通配符、协议、路径、查询串、端口、IP 地址；
  至少要有一个点，顶级域不能全数字。归一化之后同一域名进不了第二条记录（409）。
- `upstream_target_id`：必须是发现出来的目标标识（`local_` / `cntr_` 前缀 + 限定字符集），不能填主机名、
  IP 或 URL。
- `upstream_scheme`：`http`（默认）或 `https`。
- `upstream_port`：1—65535，且**不得等于管理面板自己真正监听的端口**（取自运行时监听地址，不写死 9443）。
- `max_body_bytes`：默认 1048576（nginx 的 `1m`），范围 1 KiB—1 GiB。
- `read_timeout_seconds`：默认 60（nginx 的 `60s`），范围 1—3600。
- `access_mode`：`public`（默认）/ `cidr` / `basic_auth`，与附加字段必须一致——`cidr` 必须要网段，
  其它模式不许有网段；`basic_auth` 必须要账号，其它模式不许有账号（多给也 400，不静默丢弃）。
  网段收敛成网络地址、排序去重，`/0` 直接拒绝（那等于 `public` 却顶着“受限”的名字）。
- 账号名：1—64 个字符，小写字母/数字/`.`/`_`/`-`，以字母或数字开头；重名拒绝。

**（7）错误映射。** 400 `VALIDATION_ERROR`（输入不合法，消息点名出问题的要求）、409 `VALIDATION_ERROR`
+ `details`（域名冲突，带 `state` / `matches` / `undecidable` / `broken_files` / `existing_site_id`；
沿用同一个错误码是因为它的可修性相同：改输入）、404 `NOT_FOUND`、409 `VERSION_CONFLICT`、
503 `AGENT_UNAVAILABLE`。

**（8）审计。** `proxy.create` / `proxy.update` / `proxy.delete` 与业务写入同一事务；明细固定带
`"applied": false`，使“这次只写了库”在事后可查。

### 9.2 P10.2 已实现范围：配置生成、原子写入、`nginx -t` 门禁与 reload

本节覆盖**写**的那一半：一条站点定义变成一个真在提供服务的 Nginx 站点。证书仍是 P10.3；
管理面板自己的 9443 不在这条流水线上——托管站点只监听 80，两者没有共享配置，所以“把 nginx 配置
写坏”能弄坏站点，弄不坏面板（P10.2 验收 §7b 就是在坏配置存在的情况下登录面板的）。

**（1）一条记录、一份文件、一个名字。** 托管文件名由域名决定
（`conf.d/upanel-<domain>.conf`，`internal/nginx.ManagedFileName`），文件头第一行是
`# 由 uPanel 管理：站点 <domain>`；账号文件在 `nginx/upanel-auth/<domain>.htpasswd`。
**渲染是纯函数**：同一份定义永远渲染出同样的字节，包括 basic auth 的 apr1 哈希（盐由域名派生，
不是随机的）。因此“再应用一次”是**幂等重放**——不写文件、不产生备份、不 reload，
任务里如实写 `file_state=unchanged`、`reload=skipped_unchanged`。
P10.3 给这条加了一个例外，而且只有这一个：站点**绑定了证书**、且盘上那张证书与 443 上正在用的
那一张不是同一张时，配置字节没变也照样 reload（§9.3(13)）——续期换的正是“配置之外的那部分内容”。

**（2）参数由 Web 展开，不由调用方给。** `proxy.apply` 的请求体只有一个 `site_id`
（`internal/api/preflightexpand.go`）：定义取自面板库里的那条记录、`version` 取自记录、
`panel_port` 取自面板真实监听地址。理由是 HTTP 入口对任何 admin 都开着，而唯一该落进 Nginx 的
东西是“库里已经通过 P10.1 校验的那份定义”。`panel_port` 在执行层**再校验一次**：上游端口等于
面板端口一律拒绝——记录校验过了不替参数上的值担保。

**（3）先账号文件，后配置；先备份，后改名。** 顺序是固定的，每一步都有理由：
账号文件先写，因为配置一旦生效就要求它存在（否则 nginx 启动时找不到 `auth_basic_user_file` 会报错）；
配置先写到同目录的临时文件并 `fsync`，再 `rename` 覆盖目标，最后 `fsync` 目录——中间任何一刻掉电，
盘上要么是旧文件、要么是新文件，不会是半个文件。被替换或被删除的**托管文件**先备份到
`/var/lib/upanel-agent/nginx-backups/`（名字里带时间与内容哈希前缀，同一个内容不会被备份两次）。
非托管文件一个字节都不碰：验收 §13 断言 `/etc/nginx` 里其它文件的内容摘要前后一致。
账号文件的权限不是写死的：从主配置的 `user <name>;` 读出 nginx 的 worker 用户，写成
`root:<worker 组> 0640`（只有 root 能改、worker 能读）；读不到 worker 用户时退回 `0644` 并在任务里
明确报告 `PROXY_AUTH_FILE_WORLD_READABLE`——不猜一个 `www-data`（在一台用 `nobody` 的机器上，
猜只会多一个读不到的用户，而权限位看起来还很“正确”）。

**（4）门禁是 `nginx -t` 对整棵树，不是对这份文件。** 通过才保留、才 reload；失败就回滚。
回滚之后**再跑一次** `nginx -t`，把“回滚之后这棵树是好的”写成证据，而不是“我改了文件所以我猜它好了”。
失败路径绝不 reload：验收 §7b 断言 worker PID 没变、站点仍用旧配置提供服务。

**（5）reload 只 reload，不 restart，而且只在服务在跑时做。** `systemctl is-active nginx` 现场问一次
（执行路径不看十秒前的快照），active 才 `nginx -s reload`；不 active 时如实写
`reload=skipped_service_inactive`——重启服务是另一个动作，不在这条流水线的权限里。

**（6）执行前重新评估一次。** `proxy.apply` 的 Execute 会**重新**采集现状、重新判域名冲突
（这回忽略自己那份文件），而不是相信预检时的结论。预检到执行之间可能有人手工加了配置；
把预检当成免检通行证，等于把 TOCTOU 写进流水线。副作用是：一棵已经坏到判断层都读不了的配置树
（例如括号没闭合）会把执行挡在预检阶段（`PROXY_DOMAIN_UNKNOWN`），一份“结构合法、nginx 不认”的
配置则能走到写入（这是 `nginx -t` 门禁存在的理由）。

**（7）`applied` 从事实推导，不是数据库里的一列。** 响应里的 `applied` 是四个值之一：
`applied`（文件在，且内容与此刻的渲染结果逐字节相同）、`stale`（文件在但内容不同）、
`not_applied`（该生效却没有文件；或已停用且文件确实不在了）、`unknown`（读不到 Nginx 现状，
或这份定义渲染不出来，`reason` 说明是哪一种）。库里存 `applied=true` 只会多一个会漂移的真相：
有人手工改文件、或者文件被删了，库里的 true 还在。展示路径读 5 秒快照，写入门禁读直采（§9.1 的规则不变）。

**（8）任务结果。** `data` 里是这一次执行的事实：`file_state`（`written` / `removed` / `unchanged` /
`absent`）、`config_hash`（渲染结果的 sha256）、`reload`（`done` / `skipped_unchanged` /
`skipped_service_inactive` / `failed`）、`served`（重新采集后，这份文件在树上、域名与字节都对得上）、
`backup`（备份文件名，没有备份时不出现）、`readback`（重新采集的细节）。`served=false` 的任务不是成功。

**（9）错误码闭集。** `PROXY_NOT_FOUND`、`PROXY_VERSION_MISMATCH`、`PROXY_DOMAIN_TAKEN`、
`PROXY_DOMAIN_UNKNOWN`、`PROXY_SECRET_INVALID`、`PROXY_UPSTREAM_UNVERIFIED`（目标不在发现结果里，
只告警不阻断）、`NGINX_UNAVAILABLE`、`NGINX_NOT_INSTALLED`、`NGINX_UNIT_INACTIVE`、
`NGINX_CONFIG_NOT_WRITABLE`、`NGINX_ROLLBACK_RESTORED`（回滚成功且回滚后 `nginx -t` 通过）、
`NGINX_ROLLBACK_FAILED`（要人来看，不重试）。依赖类阻塞是 409 `DEPENDENCY_BLOCKED`，参数类
是 400 `VALIDATION_ERROR`——两者的修复动作不同（先满足前置条件 / 改输入）。

**（10）删除记录有门禁。** 托管文件还在盘上时 `DELETE /proxies/:id` 返回 409
`DEPENDENCY_BLOCKED`，消息直接说下一步（先把 `enabled` 改成 false 并应用一次再删）：
先删记录会留下一个面板不再管理、却仍在服务的 `server` 块。读不到事实时是 503，不是“当作没有”。

**（11）路径分隔符由 Nginx 决定，不由编译机决定。** 所有拼进配置内容的路径走 `path`，不走
`filepath`：面板可以在 Windows 上交叉编译，`filepath.Join` 会把 `/etc/nginx/...` 拼成
`/etc/nginx\...`——一份能被 `nginx -t` 拒绝、或者更糟、被 Nginx 当成相对路径的配置。
这条规则在 `internal/proxies/configfs.go` 与 `internal/api/proxies_applied.go` 里各写了一遍注释。

**（12）秘密不打日志、不进 argv。** basic auth 的明文只经标准输入传给 apr1 计算，数据库里
只有 `secret_ref`，Nginx 侧只有哈希。

### 9.3 P10.3 已实现范围：签发、手动导入、到期信息与私钥边界

本节覆盖证书的**生命周期**：一张证书从“什么都没有”到“装在一个站点上、443 正在用它服务”，
以及“私钥在哪儿、谁能读”。自动续期的**调度**（systemd timer）是 P12；本节只保证“续期这个
动作成立、可以被调度”。

| 方法与路径 / 动作 | 权限 | 行为 |
|---|---|---|
| GET /certificates?domain=&limit=&cursor= | admin | 证书记录列表（`renewal_state` 派生） |
| GET /certificates/:id | admin | 记录 + `observed` 现场事实 |
| POST /certificates/uploads | admin + CSRF + 再认证 | multipart 暂存公钥证书（1 MiB、10 分钟、一次性） |
| `certificate.issue` | admin + R，预检 → 任务 | ACME 签发（`staging` 必须显式） |
| `certificate.renew` | admin + R，预检 → 任务 | 续期既有记录（身份不变） |
| `certificate.import` | admin + R，预检 → 任务 | 把上传的证书与秘密存储里的私钥装到主机 |
| `certificate.state` | admin（只读查询） | 盘上那张证书此刻的事实 |

**（1）一条域名一条记录，一处写入。** `certificates.domain` 唯一。写这条记录的地方**只有一处**：
面板在任务从非终态转入终态时调用 `Options.OnJobFinished`，由它按任务的 `observed_state` 落账
（`internal/certificates/ledger.go`）。任务自己不改库、预检不改库、接口层不改库。这条约束的理由
不是洁癖：签发与导入都能把一张证书“事实上”换掉，若允许多处写，`last_error` 与 `fingerprint`
就会出现“谁最后写谁算”的竞争，而事后没人能从库里分辨是哪一次动作留下的。

标识在**预检展开时**就定下来（已有记录就复用，没有就生成 `crt_` + 32 位十六进制），所以站点绑定
指向的 id 不会因为“这次签发成功/失败”而变。来路不同则拒绝：一条记录不能既是 `certbot` 又是
`manual`——“当前这张是谁给的”必须只有一个答案。

**（2）staging 必须显式为 true 或 false。** 缺字段是 400，不是“当它选了正式”。理由两条：
默认正式会让人一句 JSON 写漏就烧掉速率限额；默认测试会让人以为签好了而浏览器仍然报警。
`--server` 的两个取值由 `staging` 决定，调用方没有机会塞进一个任意 URL；私有 ACME / 验收用的
本地测试 CA 只能由 root 独占的代理配置覆盖（`agent.certificates.{staging,production}_directory_url`，
必须是 `https://` 绝对地址）。签发出来的证书是不是 staging，由**证书自己的签发者**判断
（`(staging)` / `fake le` / `pebble`），不看人的声明——声明会错，字符串不会。

**（3）一小时限流查的是任务表，不是证书表。** 同一域名一小时内只允许一次 ACME 尝试。查
`jobs`（`RecentAttempts`，按 `action` + `target_json.display_name` + 时间窗）而不是查证书：
**失败的尝试同样消耗配额**，而失败的首次签发在证书表里根本没有记录。`certificates.last_attempt_at`
是第二道（同一个判断在读记录时也要成立，且续期之后要能被打上时间戳）。限流返回 429
`RESOURCE_BUSY`，消息里给出下一步（先看失败原因，再等一小时），不给“重试”按钮。

**（4）签发前先探一次 HTTP-01 可达性。** 校验路径不可达时**阻断**（`ACME_HTTP01_UNREACHABLE`）：
这一步不做，最常见的失败（域名没指到这台机器、80 没通）会在 certbot 真的去申请时才暴露，
而那时已经消耗了一次请求。探测返回非 200 但可达时只告警——路径存在、内容不对，让 certbot 自己试。

只做 HTTP-01，**不支持通配符**（那需要 DNS-01，意味着把 DNS 凭据交给面板）。
不用 certbot 的 nginx 插件：它会自己改 `/etc/nginx`，而改配置是本产品的核心动作，必须走
渲染 → 原子写 → `nginx -t` → 回滚那条流水线（§9.2），不能交给另一个程序在黑盒里做一遍。
`--webroot` 只写校验文件，由我们自己渲染的 ACME location 提供服务。

**（5）手动导入：公钥走上传，私钥走秘密通道。** 第三步分开是本节的核心边界：

- 公钥证书经 `POST /certificates/uploads`（multipart，字段 `certificate`，1 MiB 上限）。夹带私钥
  **直接 400**，不“忽略多余的块”——那样私钥已经进了这个进程的内存与可能的日志。暂存按会话绑定、
  10 分钟、一次性、每用户有数量上限（满了 429），只存在内存里。
- 私钥经 `POST /secrets`（用途 `tls_private_key`，16 KiB），进代理的 AEAD 加密库；导入参数里
  只有 `private_key_secret_ref`。
- 暂存**在展开的最后一步才被消费**（`Take` 是销毁语义）。唯一的例外是“证书不覆盖这个域名”：
  判覆盖必须先解出证书里的域名，所以那种情况会连上传一起消耗掉，错误信息直接说需要重传。

**（6）落地路径与权限是固定的。** 手动导入：目录 `/etc/ssl/upanel`（`0700 root`）、证书
`<domain>.pem`（`0644`）、私钥 `<domain>.key`（`0600 root`）。certbot 签发：文件在
`/etc/letsencrypt/live/<domain>/`（certbot 自己的 `0700` 目录），面板**只读取路径用于渲染**，
从不打开私钥。`nginx` 以 root 启动、以 worker 用户运行，两种文件它都读得到；面板 Web 以
`upanel-web`（uid 999）运行，**两种私钥都读不到**——这一条是验收里唯一靠 `runuser` 直接证明的。

**（7）私钥内容任何出口都不出现；路径只出现在任务报告里。** 接口 DTO 里没有 `key_path`、`cert_path`、
`key_secret_ref`、`cert_pem` 这些字段（不是忘了加）；`certificate.state` 的代理响应里**带着**
这两个路径（它排查问题要用），接口层用显式结构体接住响应，多出来的字段自然丢掉——比在每个调用点
记着删一个键可靠。审计只记域名、指纹、有效期与来路。任务参数里会出现公钥 PEM（`ImportParams.CertPEM`）：
它是公开材料，落进 `preflights.normalized_params` 没有问题；私钥反过来，只以 `secret_ref` 的形式存在。

**路径与内容要分开算。** 任务结果与预检的影响说明里**会**出现落地路径
（`/etc/ssl/upanel/<域名>.key`、`/etc/letsencrypt/live/<域名>/privkey.pem`）：那是运维要知道的
“文件写到了哪儿”，而路径本身不是凭据（能读到它的人已经能 `ls`）。私钥**内容**从不出现，一处也不行。
这条分界是可测的：验收 §10 把“路径只在任务报告里”写成断言，同时在证书接口的响应体、面板库、审计、
Web 与代理日志里搜 `PRIVATE KEY` 与这把私钥的一行正文，必须一处都搜不到。

**（8）到期是算出来的，不是存下来的。** `not_after` 是从证书解析出来的事实，`renewal_state` 是
读的时候按 21 天临期窗口算出来的四态（`valid` / `expiring` / `expired` / `unknown`）。已经过期的
证书在**预检就被挡**（`CERTIFICATE_EXPIRED`）：装上去只会让浏览器报警，那不是“装好了”。
详情页的 `observed` 是现场事实：文件还在不在、指纹还是不是当初那一张、私钥权限有没有被改松
（`key_present` / `key_mode`）。`observed.available=false` 不改变状态码——记录还在，为“读不到现场”
整页 503 会把用户已经掌握的事实一起丢掉。

**（9）续期不改身份。** `certificate.renew` 的请求体只有 `domain`（外加必须显式的 `staging`）：
记录 id、ACME 邮箱、来路都沿用既有记录，站点绑定不动。续期成功后指纹与到期时间刷新、`last_error`
清空、任务结果里带新证书的 `observed_state`。手动导入的记录**不能被 certbot 续期**（409）：
两条来路不会互相顶替。

**（10）错误码闭集。** 阻塞：`CERTIFICATE_PARAMS_INVALID`、`CERTIFICATE_PEM_INVALID`、
`CERTIFICATE_KEY_MISSING`、`CERTIFICATE_KEY_MISMATCH`、`CERTIFICATE_DOMAIN_UNCOVERED`、
`CERTIFICATE_EXPIRED`、`CERTIFICATE_IS_CA`、`CERTBOT_MISSING`、`ACME_HTTP01_UNREACHABLE`、
`CERTIFICATE_LIVE_DIR_MISSING`、`CERTIFICATE_FILES_UNREADABLE`。告警：`CERTIFICATE_STAGING`、
`CERTIFICATE_SELF_SIGNED`、`CERTIFICATE_EXPIRED`（临期窗口内）、`CERTIFICATE_WRITTEN`。
`proxy.apply` 这一侧另有十个与 TLS 现场相关的码。阻塞（渲染 443 之前的现场检查）：
`PROXY_TLS_MATERIAL_MISSING`、`PROXY_TLS_MATERIAL_INVALID`、`PROXY_TLS_KEY_UNAVAILABLE`、
`PROXY_TLS_DOMAIN_UNCOVERED`、`PROXY_TLS_CERT_EXPIRED`。告警：`PROXY_TLS_KEY_PERMISSIVE`、
`PROXY_TLS_CERT_EXPIRING`、`PROXY_TLS_STAGING_CERT`、`PROXY_TLS_FINGERPRINT_DRIFT`、
`PROXY_TLS_SERVED_STALE`（§9.3(13)）。
执行期失败：`CERTBOT_FAILED`（带 certbot 的输出摘要，其中不含私钥）。HTTP 侧：400
`VALIDATION_ERROR`、404 `NOT_FOUND`、409 `VALIDATION_ERROR` / `DEPENDENCY_BLOCKED`、
429 `RESOURCE_BUSY`（限流）、503 `AGENT_UNAVAILABLE`。

**（11）审计。** `certificate.upload`（域名、指纹、有效期、是否 staging）、`certificate.manage`
（被 admin / CSRF / 再认证挡下的请求）与任务链路的 `job.submit`；明细里没有 PEM、没有私钥、
没有路径。

**（12）本阶段没有调度。** 不创建 timer、不自动续期、不后台重试。续期只有两条触发路径：人在
界面上点，或者 P12 的 timer 调同一个动作。

**（13）续期只换文件；让新证书生效靠下一次应用，而这一次应用必须认出来。** `certificate.renew`
不 reload Nginx（它不该顺手重载一棵可能被别人改过的配置树），所以续期成功之后、应用之前，443 上
**仍然是旧证书**——这是 Nginx 的正常行为，worker 启动时把证书读进内存，文件换了它不重读。
把它接上的是 `proxy.apply`：站点绑了证书、且渲染结果与盘上那份逐字节相同（`file_state=unchanged`）
时，它用 SNI 向 `127.0.0.1:443` 做一次握手（2 秒上限），比对**叶证书指纹**与磁盘上那本证书：

- 不一致 → 视同“有变化”，照常 reload，任务 `observed_state` 里写 `reload_reason=cert_rotated`、
  `served_cert_fingerprint`（reload 之前 443 用的）、`served_cert_after_reload`（reload 之后再问一次）、
  `served_cert_probe_attempts`（问了几次）。
- reload 之后要**等新一代 worker 接上**再问：`nginx -s reload` 只发一个 HUP 就返回，真机上 reload
  刚返回时 443 上还是旧证书（P10.3 验收实测）。等待预算 12 × 250ms，对上就停。
- 等满预算仍不一致 → 告警 `PROXY_TLS_SERVED_STALE`（“reload 做了也没生效”：前面还有一层代理，
  或这个域名没命中本面板的 server 块）。这不是失败，但它必须出现在报告里。
- 握手失败 → **不改结论**（配置已经过 `nginx -t`，reload 修不好一个握不上手的 443），
  但 `served_cert_probe=failed: …` 要留在报告里：没测到与没变化不是一回事。

判据是**现场测量**：不新增“强制 reload”参数，也不存“上次 reload 时证书是哪一张”——
存下来的结论会过期，测量不会。因此 P12 的 timer 只需要做两步：续期 → 对绑定的站点各应用一次。

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

### 12.1 P11.1 已实现范围：APT 事实读取（包、候选版本、安全更新、重启标志）

本节只做**读**。刷新索引、安装、卸载、升级是动作（`package.refresh` / `package.install` /
`package.remove` / `package.upgrade`，P11.2），重启关机是 P11.3。这里没有一条路由能改系统状态。

| 方法与路径 | 权限 | 行为 |
|---|---|---|
| GET /packages?query=&installed=&upgradable=&security=&limit=&cursor= | admin | 已安装包、已安装版本、候选版本（`upgradable` / `candidate_relation` / `security` 派生） |
| GET /packages/updates | admin | 可升级清单（安全更新排在前面），带总数、安全条数与 `truncated` |
| GET /system/power-state | admin | `/run/reboot-required` 与其 `.pkgs` 列表、本次 `boot_id` |

代理侧对应两条只读查询：`apt.packages`（参数 `{fresh}`）与 `system.power_state`（空参数）。

**（1）代理给事实，面板给判断。** 代理只回源命令的原文事实：`dpkg-query` 的
`${binary:Package}` / `${Version}` / `${Architecture}` / `${db:Status-Abbrev}`，`apt-cache policy`
里每个包的 `Installed:` / `Candidate:` 与**候选版本那一条**的来源 suite（`jammy-updates`、
`jammy-security` 这类原文）。`upgradable`、`security`、`pocket` 三个派生字段由面板
`internal/packages` 算，理由是它们随判断规则变（例如“哪些 suite 算安全更新”是策略，
不是主机事实），而事实不该跟着策略一起变。

**（2）版本比较必须是 Debian 语义，不是字符串比较。** 真机上就有现成的反例：
`open-vm-tools 2:12.3.5-3~ubuntu0.22.04.3` → `2:12.3.5-3~ubuntu0.22.04.4`，字符串比较会说
`.3` 大于 `.4` 的前缀关系完全看不懂；`~` 在 Debian 里排在**空串之前**（`1.0~rc1` < `1.0`），
epoch（`2:`）比 upstream 版本更优先。因此比较由 `internal/debversion` 实现（按 dpkg 的规则：
epoch → upstream → revision，非数字段按“`~` < 空 < 字母 < 其他符号 < 数字”排序），
并有针对性的单测锁住这些反例。字符串相等只用于“完全没变”这一个判断。

**（3）`upgradable` 说的是“候选版本比已安装的新”。** `candidate_relation` 取
`newer` / `same` / `older` / `unknown`；`upgradable` 等价于 `candidate_relation=newer`。
候选比已安装**旧**是真实情形（pin、回退、第三方源降级），这时 `upgradable=false` 且
`candidate_relation=older`，页面据此显示“候选版本更旧”，而不是假装没有候选。
候选读不到（`apt-cache` 不可用或该包不在缓存里）时是 `unknown`，**不拿已安装版本冒充候选**。

候选的查表规则有一条真机坑必须写明：`dpkg-query` 给的 `${binary:Package}` 带架构限定
（`bind9-libs:amd64`），而 `apt-cache policy bind9-libs:amd64` 打出来的**表头是 `bind9-libs:`**
（apt 自己把 `:arch` 剥掉了，`Candidate:` 与版本表仍是请求那个架构的内容）。因此按请求名查表
必须允许回退到 base 名，且只在 base 名在本次采集里**唯一**时回退——同名双架构同时装着时，
那个表头分不清是哪一边的版本，宁可报 `unknown`（读不到），也不给另一架构的版本。
这条是 731 条里那一条谎报“候选读不到”的根因，误判的代价是把一条能升级的包藏起来。

载荷里的 `name` 就是 `${binary:Package}` 原文，所以带限定的包名里已经有 `:arch`；
`architecture` 是单独字段（`amd64` / `all` / …）。渲染时**不能再补一次** `:arch`，
否则屏幕上会出现 `bind9-libs:amd64:amd64`。

**（4）`security` 只看候选版本自己的来源。** 判定为：`candidate_relation=newer` 且候选版本那一条
的某个 suite 以 `-security` 结尾（`jammy-security`、`noble-security` 及其 `-infra-security` 等变体，
规则见 `internal/packages`）。**同一包的其它版本出现在 security 源里不算**——`apparmor` 在 22.04
上就是这样：候选在 `jammy-updates`，而 `jammy-security` 里那个是**更旧的**版本，把它算成安全更新
会让页面报一条根本不存在的安全更新。

本节**不评估** phased update（分阶段推送）与 held back（被保留的包）。那两件事的判决是
`apt-get -s upgrade` 给出的，属于 P11.2 的模拟；因此页面文案写的是“候选版本可用”，
不写“本次会被升级”。这条区别在 22.04 的验收机上能直接看到：`nano`、`snapd` 的候选比已安装新，
但 `apt-get -s upgrade` 把它们算作 kept back（仓库做了分阶段推送）。

**（5）重启标志是读，不是算。** `/run/reboot-required` 存在即 `reboot_required=true`，
`reboot_required_since` 取该文件的 mtime（“什么时候开始需要重启”），`/run/reboot-required.pkgs`
的每一行原样进 `reboot_required_packages`。面板**不自己判断**“装了内核所以要重启”——
那是包自己的 postinst 写的文件，我们只报告它。`boot_id` 来自
`/proc/sys/kernel/random/boot_id`，P11.3 用它确认一次重启真的发生过（同一个值说明没重启）。
读 `.pkgs` 失败不影响 `reboot_required`（文件本身在不在是主判据），失败原因进
`reboot_required_packages_error`。

**（6）读不到就说读不到，不降级成“没有”。** 三类边界分开处理：

- 主机上没有 `dpkg-query`（不是 Debian 系）：代理启动时**不登记**这两条查询，
  `capabilities.supported_queries` 里也就没有它们，接口回 503 并说明这台主机不支持；
  不是给一份空包列表——空列表会被读成“这台机器上没装任何软件”。
- `dpkg-query` 能跑但失败：查询直接报错（503），因为“装的什么”是这一页的全部内容。
- `apt-cache policy` 失败（缓存损坏、权限、超时）：**包列表照出**，每个包的
  `candidate_relation=unknown`，`errors[]` 里写明来源与原因，页面显式提示“候选版本读不到”。
  已安装版本与候选版本是两个独立来源，一个坏了不该把另一个也变成空白。

**（7）索引年代一起返回。** `index_updated_at` 取 `/var/lib/apt/lists` 的 mtime：页面要能说
“这份候选版本是几点几分算出来的”，否则一个三天没刷索引的机器会让人以为“没有更新”。
刷新索引是 `package.refresh`（P11.2），本节不做。

**（8）代理侧 30 秒快照、单飞采集、按（已安装版本，索引时间）失效的候选缓存。**
一次采集 = 一条 `dpkg-query` + 一条 `apt-cache policy`。翻页与筛选不该每次都跑命令，
因此快照按 30 秒缓存；写操作前的判断用 `fresh=true` 绕过它。缓存窗口里变的是“装了什么”，
而这 30 秒内装东西的只可能是别人在命令行里 apt 了一把——页面顶部显示的 `collected_at`
就是这句话的依据。

三层缓存的理由都是实测出来的，不是设计洁癖：

- **候选缓存按包失效**（键：该包已安装版本；全局键：`/var/lib/apt/lists` 的 mtime）。
  `apt-cache policy` 的成本随**参数个数**增长：22.04 的 apt 2.4 读 676 个名字只要 384ms，
  24.04 的 apt 2.8 读 731 个名字在冷页缓存下要 26.6 秒（约 36ms/名）。有了按包失效，
  快照过期重采时没变的包直接复用，稳定态回到 0.3 秒级；索引一刷（mtime 变）全部作废，
  不会拿旧索引的候选冒充新的。
- **单飞**：并发请求只跑一次采集，其他请求等这一份。否则十个标签页同时打开就是十次
  `apt-cache policy`，把一台 2 vCPU 的机器按在地上。
- **没有启动预热**。曾经加过，实测有害：开机时预热与 systemd 采集抢 `systemctl`，
  把 26 秒拖到 62 秒；而请求会排在这份预热后面，直接顶穿 45 秒预算，于是“最需要它的那一次”
  反而谎报“代理不可用”。冷一次（首次调用实测 12.5–15.6 秒）比在最需要的时候说谎好。

超时分层：Web 侧单条查询预算 45 秒（`internal/packages.AgentQueryTimeout`），
前端每个请求 60 秒（`PACKAGE_QUERY_TIMEOUT_MS`）；代理的 HTTP 写超时默认 15 秒，
对这两条查询按请求延长到 120 秒；Web 的 HTTP 写超时 30 秒，同样按请求延长到 60 秒。
不延长的话，冷采集正撞在写超时上，前端拿到的是一个没有响应体的连接中断。

**（9）分页与上限。** 列表按 `(name, architecture)` 稳定升序，默认 `limit=50`、最大 200，
`cursor` 是不透明串（`base64url(name + \0 + arch)`，指向“上一页最后一条”）。
`/packages/updates` 不分页，但一次最多 1000 条并带 `truncated`——可升级项超过一千台的机器
（长期没更新）会把列表读成一页长墙，而不是把响应撑爆。

**（10）本节不覆盖的东西。** ESM / Ubuntu Pro 源里的包（要 `pro` 订阅才有条目）、snap 包、
`apt-mark hold` 的状态、`/var/log/apt/history.log` 的历史：都不在 P11.1 里。页面不显示它们，
也不显示“没有新版本”这种把未知说成已知的话。

### 12.2 P12.2 已实现范围：备份仓库、策略、运行与快照

本节把第 12 节的备份契约落成真正的路由。**写侧只有一条路：编辑代理里那份定义。**
执行是 `backup.run` 的预检 → 任务（P12.2 已接），清理是 `backup.prune`（P12.3），恢复不在本节。

| 方法与路径 | 权限 | 行为 |
|---|---|---|
| GET /backup/repositories | admin | 仓库定义列表（`rep_` + 32 位十六进制 id、version、两处秘密的**引用**） |
| POST /backup/repositories | admin + CSRF + 再认证 | 登记仓库定义；只存元数据，不传明文 |
| GET /backup/repositories/:id | admin | 单条仓库定义 |
| PATCH /backup/repositories/:id | admin + CSRF + 再认证 | 改定义；必须带 `version` |
| GET /backup/policies | admin | 策略列表 |
| POST /backup/policies | admin + CSRF | 登记策略 |
| GET /backup/policies/:id | admin | 单条策略 |
| PATCH /backup/policies/:id | admin + CSRF | 改策略；必须带 `version` |
| DELETE /backup/policies/:id | admin + CSRF | 删策略；不删仓库、不删快照 |
| GET /backup/snapshots?repository_id=&policy_id= | admin | 快照列表（**不含**大小），`registered` 说明仓库在不在 |
| GET /backup/snapshots/:id?repository_id= | admin | 单条快照（含 `restic stats` 的大小与文件数），`found` 说明这条在不在 |

代理侧对应三条查询 `backup.snapshots`（参数 `{repository_id, policy_id?}`）、
`backup.snapshot`（`{repository_id, snapshot_id}`），与一个动作 `backup.run`
（参数 `backup_policy_id`、`version`，`apps` 由面板展开后填入）。

**（1）备份定义只有一份真相，就在代理里。** P12.1 的镜像不是为了性能，是为了让预检计划里
被冻结的那个目标在**执行时**没法被换掉。既然镜像已经是权威，面板就不该再存一张自己的
`backup_repositories` / `backup_policies` 表：两张表必然漂移，而漂移之后没人能回答
“这条策略到底绑的是哪个仓库”。P12.2 的选择是**面板不存第二份**，`/backup/*` 整组是镜像的
编辑器。直接后果：代理不在时这一组整组回 **503**（`AGENT_UNAVAILABLE`），不是“先登记、
回头同步”——那种“回头”从来没有发生过一次。CONTRACTS §14.1 里那两行表因此仍属逻辑模型，
本阶段没有对应的迁移。

**（2）权限分层按“这条定义引用了什么”定，不是按“备份重要不重要”定。**
仓库定义引用仓库口令与 SFTP 私钥（两处都是**引用**），改它等于改“下次备份把数据写到哪儿去”，
所以 POST/PATCH 要再认证；策略不引用任何秘密、也不动主机状态，它只描述“备份什么、按什么口径”，
因此只要 CSRF。快照是纯读，没有写入口。审计动作分三组
（`backup.repository.manage/create/update`、`backup.policy.manage/create/update/delete`），
中间件拒绝时也能记到正确的动作名下——group 级中间件是唯一能做到这件事的位置。

**（3）`version` 是乐观并发令牌，不是装饰。** PATCH 缺 `version` 或 `version < 1` 时面板
**先**拒（400，并在审计里记一条被拒），代理再拒是 409 `VERSION_CONFLICT`。两个状态码不能合并：
“你没说改的是哪一版”是调用方的错，“你说的那一版已经不在了”是别人先改了一步。前者 400，
后者 409 + 让用户刷新——界面只有分开这两种，才给得出正确的一句话。

**（4）`backup.run` 的参数里 `apps` 由面板展开，调用方给不了。** 契约 §5 的参数表写的是
`backup_policy_id、version`，所以面板侧 `/preflights` 的请求体也是这两个字段，而且严格解码：
请求体里出现 `apps` 直接 400（不是忽略）。展开规则只有两条：`apps.TypeSystemd` →
`kind=systemd`、`object=<unit>`；`apps.TypeDocker` → `kind=container`、`object=<容器 id>`。
**解析不出就阻塞，不降级**：`compose` 部署（一停就是整组，超出代理的启停通道）、只登记了入口
没有绑定的应用、已经删掉的应用，三种情况都回 `422 DEPENDENCY_BLOCKED` 并**点名**是哪个
`app_id`、为什么。理由很硬：把“停不了”的写成“不用停”，快照会被打上
`stop_managed_apps` 的标签，而其中一部分文件其实是在应用写着的时候读的——标签会撒谎，
而且事后无法从快照本身看出来。

**（5）参数与策略双向核对。** 代理侧 `appsVsPolicy` 两个方向都判：多了 = 调用方在**扩大**
策略授权（拒绝），少了 = **停机不全**（阻塞，`BACKUP_APP_UNAVAILABLE`，消息里写明
“这条快照做不到 stop_managed_apps”）。少了这一条，一个 app 解析失败就会变成一次安静的不一致备份，
而 `files_only` 与 `stop_managed_apps` 的区别正在这里。

**（6）一致性口径是闭集，说的是“保证什么”，不是“尽量”。**
`files_only` 只保证文件字节：预检会带一条告警说明正在写入的数据库（容器里的 MySQL、
面板以外的应用库）在快照里可能不一致，需要一致就改用 `stop_managed_apps`。
`stop_managed_apps` 则要求策略里**每一个** `app_id` 都解析出运行对象，一个不行就整轮阻塞（见上条）。
口径同时是快照标签（`upanel:consistency:<mode>`），读回来能认这份快照是按哪种口径做的。

**（7）停 → 备份 → 拉起，三件事各自留痕。** 顺序固定：读一遍应用现状（只有**刚才真的在跑**的
才需要拉起，静止的不动——备份不该顺手启动一个别人停掉的应用）→ 停（逐条记 `done`/`verified`/`note`）
→ 面板库副本 → `restic backup` → 读回核对 → 拉起 → 清临时副本。任何一步失败都要把
“谁被停了、谁没起来”写进结果，用 `context.WithoutCancel` 保证收尾不被取消带走：
**备份失败不是不拉起应用的理由**。拉起本身失败同样是一条结果事实（`verified=false` + 原因），
不能吞掉。

**（8）退出码不是证据。** `restic backup` 退出 0 只说明命令跑完了。判定“快照真的存在”靠
`--json` 输出里的 snapshot id，再用 `restic stats <id>` 重新读一次（`Verified=true` 只在读回来
之后成立），并把 `files_new` / `files_changed` / `data_added` / `restic_duration_ms` 落进
`observed_state`。证据链是“命令 → 快照 id → 回读”，少一环就只剩一句“应该成功了吧”。

**（8b）结果里的快照 id 一定是完整的 64 位。** 不同版本的 restic 在 `backup --json` 的
summary 里给的 id 长度不一样：0.14 起给完整 id，0.12（22.04 自带）只给 8 位短 id。短 id 是
**前缀**而不是身份——它可能同时是几条快照的前缀，所以不能直接拿去 `stats`，更不能写进结果。
代理遇到短 id 时回仓库查一次（`restic snapshots --json` 按前缀匹配），**命中恰好一条**才换成
完整 id 继续读回；命中 0 条或多条都判失败（证据不足不等于挑一条）。因此公开接口给出的
`snapshot_id` 永远是完整 id。

**（9）面板库用它自己的在线备份接口，`VACUUM INTO`，不用 `os.Copy`。** 直接拷贝 `.db`
会漏掉 `-wal` 里尚未合并的事务，得到一份看起来正常、实际更旧甚至半截的库。
`VACUUM INTO` 在**一个读事务**里读源库（连同 WAL），写出已整理、已自洽的新库；读连接以
`mode=ro` 打开，源库一个字节都不改。一个真机上的坑写在代码注释里：DSN 上加
`_pragma=query_only(1)` 会让 `VACUUM INTO` 直接报 `attempt to write a readonly database`——
“顺手加固”会把备份弄坏在原地，单测钉住了这一条。副本目录 0700 / 文件 0600（里面有口令
哈希与会话），只在这次运行期间存在，跑完即删；`observed_state` 里只留大小、
`PRAGMA user_version`、`integrity_check` 结果与时间——**不留副本，也不留路径**。

**（10）快照列表不返回大小，这是刻意的。** 每条快照的大小要单独跑一次 `restic stats`，
列 200 条就是 200 次进程启动。列表回 `size_included:false` 且 `size_bytes` / `file_count`
**字段不出现**（不是写 0——“没读”与“读了是 0 字节”是两件事），详情才带 stats。
`registered` / `found` 是**状态字段**，不回 404：`registered=false` 的意思是“这台机器上没登记
这个仓库”，404 会被读成“这条路由不存在”。

**（11）上限是明确的数。** 一次 `restic backup` 6 小时（几十 GB 首次全量在慢盘上就是这个量级，
再长说明源选错了）；快照查询 2 分钟；快照列表一次最多 500 条。同一个仓库上并发运行由
`BACKUP_REPOSITORY_BUSY` 挡住（锁由代理维护，见 §15）。

**（12）本节没有的东西。** `DELETE /backup/repositories/:id`：契约里没有，而且删掉定义会让
仓库里那些快照“属于谁”无从回答。删除快照与清理仓库是 `backup.prune`（P12.3）的预检 → 任务，
页面查询不触发任何删除。仓库初始化是 `backup.repository.initialize`（P12.1，动作已接）。
恢复演练、验证记录的回写不在本节。

### 12.3 P12.3 已实现范围：恢复、抽样验证与显式保留清理

本节接上契约动作 `backup.restore`、`backup.verify`、`backup.prune`。三者仍走
预检 → 任务 → 幂等 → 资源锁 → 结果读回 → 审计，不增加直接执行 restic 的 HTTP 旁路。

**（1）恢复公开参数严格只有五个。** `repository_id`、`snapshot_id`、`target_root_id`、
`relative_path`、`overwrite`；调用方传入 `apps` 直接 400。覆盖恢复需要停机时，面板根据
当前受管理应用绑定保守展开运行对象：systemd 单元与容器可进入计划，compose/link 等
无法由现有受控通道安全停整组的绑定阻塞，超过 32 个应用同样阻塞。展开结果进入冻结的
`normalized_params`，调用方不能借空数组缩小停机范围。覆盖动作要求再认证与服务端生成的
`OVERWRITE ...` 确认短语。

**（2）默认恢复不覆盖非空目录。** 目标只能尚不存在或已经为空；根自身、绝对路径、越界
路径与符号链接目标拒绝。预检同时绑定快照、目标目录状态版本、仓库定义指纹与仓库版本；
执行前重新评估，任何一项变化都按 `PREFLIGHT_STALE` 拒绝，不能拿旧许可写到新目标或新仓库。

**（3）先恢复进同父目录随机暂存树，核对后才发布。** restic 只拿到本次进程显式继承的目录
fd（子进程固定为 `/proc/self/fd/3`），不接受调用方给任意宿主路径。暂存树拒绝符号链接、
FIFO、特殊文件、setuid/setgid 与多硬链接普通文件；普通文件和目录经句柄归一为
`root:root`。Ubuntu 22.04/24.04 的 restic 0.12.1/0.16.4 都没有 `restore --no-owner`，
因此不靠虚构参数声称关闭所有权恢复。每个恢复文件还与仓库字节重新计算的 SHA-256 核对。

已存在目标使用 Linux `RENAME_EXCHANGE`，不存在目标使用 `RENAME_NOREPLACE` 发布；发布后
重新遍历并核对。发布或复核失败时交换回去或改回暂存名，旧目标只在新树发布并验证后清理。
覆盖恢复只停执行前真正运行的应用，收尾按原状态拉起并读回；新树已发布但应用未全部恢复
时任务如实为 `partial`，不能回滚成一句“全部失败”，也不能写成“全部成功”。

**（4）结构检查与抽样恢复是两种证据。** `backup.verify` 的 `sample_restore=false` 只执行
`restic check`，不写 `restore_verified_at`。`sample_restore=true` 才在代理 StateDir 下私有
0700 临时目录做真实抽样恢复：最多 8 个文件，单文件最多 8 MiB，总计最多 32 MiB；每个
抽样文件与 `restic dump` 的仓库字节比较 SHA-256。只有抽样和比对都成功才按仓库+完整快照
id 持久化验证时间，快照详情随后能读回该时间。超限文件是不参与抽样，不是假装已验证。

**（5）清理由管理员显式发起，定时备份不隐式删除。** `backup.prune` 使用策略里的
`keep_last`、`keep_daily`、`keep_weekly` 并集，日桶按 UTC、周桶按 ISO 周。预检列出
`expected_delete_ids` 并冻结策略/仓库/集合状态；执行前重新计算，集合变化即
`PREFLIGHT_STALE`，一个快照也不删。确认短语为服务端生成的 `DELETE ...`。清理的
`target.identity_fingerprint` 只描述“这条策略与这个仓库定义是谁”，待删集合属于会变化的
执行状态，只进 `state_version`；否则新增两次备份就会被错报成 `DEVICE_IDENTITY_CHANGED`，
而不是契约要求的 `PREFLIGHT_STALE`。仓库定义或位置真正改变时，身份指纹仍然变化。

实际删除只使用固定 argv `restic forget --prune <完整快照 id...>`，不接受调用方追加 restic
参数。清理后重新读取快照列表，核对拟删项消失且保留项仍存在；部分删除或读回无法确认时
保留真实 `deleted_ids` 并报告 `unknown`，不把未确认的仓库状态写成成功。

**（6）失败处理保护旧数据。** 存储写满、SFTP 连接中断、restic 非零退出或抽样不一致都在
发布前使恢复失败；暂存树不发布，原目标不动，已停应用仍进入恢复流程。恢复范围外文件不是
交换对象，也不能被清理旧暂存树顺带删除。restic 口令仍只经 `RESTIC_PASSWORD`，SFTP 私钥
仍只在 tmpfs 临时存在；恢复、验证与清理没有扩大秘密边界。

仓库里的**失效锁**由产品自己清理：`restic check` 与 `restic forget --prune` 需要独占锁，
撞上残留锁（持有进程已不在）时先执行 `restic unlock`——不带 `--remove-all`，只删 restic
自己认定为失效的锁，另一路正在跑的 restic 手里的锁不会被拿走——再重试一次。`unlock`
自身失败时原样报出原始失败，不拿清理失败盖掉原因。这条不是便利：面板是这台机器上唯一
碰得到仓库的东西（口令只经标准输入、私钥只在 tmpfs 里短暂存在），restic 那句
“the `unlock` command can be used to remove stale locks” 对运维是做不到的指令。

失败消息必须带上真正的原因。restic 的 stderr 末尾常常只是一句汇总
（`Fatal: There were 10 errors`），这种情况下要补齐第一条具体错误行（例如
`... no space left on device`），否则面板上"写满磁盘"和"没权限"长成同一个样子。

### 12.4 P12.4 已实现范围：受管理 timer、内置动作复用任务框架、受管理脚本

本节接上契约动作 `/schedules` 与 `/scripts` 两组路由，以及代理侧的 `schedule-trigger`
内部路由。计划不是"面板里的一条 cron 记录"：真正跑起来的是 systemd timer，面板负责把
定义渲染成单元文件、装上、读回状态，并按槽位幂等地触发已登记的内置动作。

**（1）计划与脚本只有代理里那份，Web 是镜像直读。** `GET /schedules`、`GET /schedules/:id`、
`GET /scripts`、`GET /scripts/:id` 都直读代理，代理不在时返回 **503 `AGENT_UNAVAILABLE`**，
不返回空列表——空列表会让管理员以为"计划都没了"，进而重建一遍。id 形状不对（不是
`sch_` + 32 位小写十六进制 / `scr_` + 同理）在打代理之前就 400。四个 GET 与全部写操作都只
admin；viewer 一律 403。计划的定义体只存在代理库，Web 不写第二份。

**（2）定义字段与校验是闭集。** `ScheduleDefinition`：`kind`（`backup` /
`certificate_renewal` / `user_script`）、`target`（kind 对应的动作与参数，与任务框架同形）、
`calendar_expression`（systemd `OnCalendar=` 的表达式部分，不含时区）、`timezone`（IANA 名）、
`persistent`、`max_runtime_seconds`（1—86400）。表达式与日历分离，是因为把 ` Asia/Shanghai`
拼进 `OnCalendar` 的写法在 systemd 里合法、在字符串校验里会变成"任意内容都放行"；
分开之后两边都能各自校验，拼装只在渲染单元时做一次。日历语法非法或时区不可解析在
保存前就 400，不落到"装上了但不响"。

一个对象只接受一条计划（库里 `UNIQUE(kind, target_id)`），计划名字也不重复
（`UNIQUE(name)`）。两条约束都在**预检**里说话：命中回 `SCHEDULE_TARGET_TAKEN` /
`SCHEDULE_NAME_TAKEN` 阻塞项，消息点名是哪条计划占着；更新自己不算占用。让它们留到
执行阶段才失败，形状就是"预检说允许、提交以后操作失败"。

合法性由 `systemd-analyze calendar` 回答，调用时把它的**进程时区钉成 `UTC`**
（`TZ=UTC`）：`Next elapse` 用的是进程时区，而进程时区就是 UTC 时 systemd 会省掉
`(in UTC)` 那一行——不钉死的话，同一份代码在 UTC 服务器上会把合法表达式判成"不会
触发"，在别的时区上又会把 `CST` 这类缩写当成偏移 0。`next_elapse` 因此恒为 UTC
绝对时刻，墙钟展示与时区换算只发生在面板这一侧。

**（3）意图与事实分开，`blockers` 永不为 null。** `enabled`、`version`、`timer_ref` 是面板
要的；`installed`、`unit_file_state`、`active_state` 是 systemd 说的原话
（`enabled`/`disabled`/`static`、`active`/`inactive`/`failed`）。`next_elapse` 只在能读到
`systemctl list-timers` 的真实时刻时出现，读不到时字段缺席而不是编一个时间。
`blockers` 是数组，没有阻塞时是空数组——`null` 会被前端读成"未知"。

**（4）单元是固定名字的 oneshot + timer。** 名字由计划 id 决定：
`upanel-sch_<32hex>.service` / `.timer`，与 `timer_ref` 的 pattern 一致。service 为
`Type=oneshot`、`User=root`、`ExecStart=/usr/local/bin/upanelctl schedule-trigger --schedule-id <id> --wait`、
`TimeoutStartSec=<max_runtime_seconds + 60>s`；timer 为 `OnCalendar=<expr> <tz>`、
`Persistent=<persistent>`、`AccuracySec=1s`、`RandomizedDelaySec=0`、
`Unit=…service`、`WantedBy=timers.target`。`AccuracySec=1s` 与
`RandomizedDelaySec=0` 是为了让"这一分钟该响"变成可复现的事实，否则 systemd 默认的
一分钟精度与随机延迟会把验收和故障排查都变成猜谜。

`--wait` 不是方便标志，它是“不重叠”的实现位置。代理对触发请求的答复是立刻的（真活跑在后台），没有 `--wait` 时单元几十毫秒就退出，oneshot 只能拦住同一次唤醒的重复执行，拦不住“上一次还在跑、下一次又起来”。带上 `--wait` 之后，`upanelctl` 守到这次操作到达终态才退出，单元在此期间一直处于 active（oneshot 跑着时 systemd 的原话是 `activating`），systemd 不会为它再起一个实例。

**“不重叠”的准确含义是“两次运行不并行”，不是“一天只跑一次”。** 到期时撞上运行中的那次唤醒，systemd 有两条路：跳过，或者把它排到当前这次运行结束之后再起一次（24.04、systemd 255 上是后者，journal 里能看到第二条 `Starting` 出现在上一条 `Finished` 之后）。两条都不产生并行，也就都不违反这里的承诺；能不能接受“补跑一次”取决于计划本身的长短（75 秒的活配每分钟的日历，本来就是每分钟一次）。要少跑几次就改日历表达式，不要指望不重叠去吞掉它。验收断言因此看的是**启动/结束序列**（每一条 `Starting` 之前必须有上一对 `Finished`），而不是总触发数（DECISIONS Q383）。

`TimeoutStartSec` 在计划最长运行时长之外多给 60 秒：那是代理杀进程、写结果、把阶段改成 `failed` 的收尾。两个数相等会出现“代理正好在超时那一刻收尾、单元同一时刻被判超时”的错位，日志里留下一条与事实相反的 `failed`。

`enabled=false` 的计划不渲染单元，并在保存时移除已有单元（含 `disable --now`）。写单元前
先 `systemd-analyze verify` 过一遍；渲染或校验失败就不落盘、不留半套（service 装了而
timer 没装）。装完 `daemon-reload`，再读回 `is-enabled`、`is-active`、`list-timers`。

**（5）触发只经代理的内部路由，且只认 UID 0。** `POST /internal/v1/schedules/:id/trigger`
由 `internal/agent/peer.go` 按路径形状单独放行：该路由的对端 UID 必须是 0，其它内部路由
仍然只接受 `upanel-web`；非 root 回 403「socket 对端 UID 无权调用该接口」。输入只接受
触发 ID，不接受 action/params——动作来自代理里已批准的计划定义，调用方不能借触发路由
跑任意动作。

`--wait` 需要读回"这一次跑到哪了"，而唯一的读口也必须是 root-only，且只能读到**这条计划
自己触发的那个操作**：`GET /internal/v1/schedules/:id/trigger-runs/:operation_id`
（同样按形状放行，见 `rootOnlyPathPatterns`）。代理在处理端核对操作记录的操作者
（`SchedulerUserID`）与会话哈希（`hashHex("scheduler:"+计划 id)`）是否都由这条计划派生，
核对不上回 404「操作不存在」，与"没有这条操作"同形——不让这条路由变成"root 能读任意
操作"的通道。触发答复因此必须自带 `max_runtime_seconds`（计划定义里的数）：root 读不到
计划详情（那是 `upanel-web` 的路由，root 调它是 403），等待上限只能由答复带。
`upanectl` 的等待上限 = `max_runtime_seconds + 30s`，单元 `TimeoutStartSec` =
`max_runtime_seconds + TriggerWaitSlackSeconds(60)`，两者留 30 秒差：超时那一刻先由
`upanectl` 在 journal 里说话（"等到 … 仍未见操作 … 到达终态"），而不是只剩一句
`start operation timed out`。

**（6）槽位幂等由触发 ID 承担。** `TriggerID(schedule, SlotAt(now))` 把当前时刻折到 UTC
分钟槽，同一槽里重放得到同一个触发 ID，返回 200 + `duplicate: true` 且不新增触发记录；
伪造槽（拿旧时刻算一个 ID 递上来）回 400「触发 ID 与当前时间槽不符」。宽限
`TriggerGraceSlots` 默认 2 分钟，覆盖 `Persistent=true` 的迟到唤醒：systemd 迟到的
`OnCalendar` 仍落在可接受的槽里，不会被当成伪造。时区只影响 timer 的墙钟解释，槽位
始终是 UTC 分钟——DST 与跨时区因此不产生"漏触发"或"双触发"（双跑由幂等兜住）。
一次触发的判决是闭集 `triggered|blocked|duplicate|failed`：预检挡住记 `blocked`，不记
`failed`，因为那是策略在起作用，不是故障。

**（7）内置动作复用任务框架，不新开旁路。** 触发产生的操作照旧经过预检、任务、幂等、
资源锁、结果验证与审计，操作 id 由 `OperationID(trigger)` 稳定派生，请求指纹
`scheduledOperationRequestHash` 不含预检引用（否则重试会算出不同指纹）。触发与计划
`schedule.update/delete` 共用 `schedule:<id>` 锁，取得锁后必须重新判决触发条件；初次判决后
被停用或删除的计划不得再接受操作。管理员保存计划时的显式授权替代交互确认；这条替代不能
扩展到格式化、网络变更等动作——`SCHEDULE_KIND_UNSUPPORTED` 之外的越界动作在预检阶段就被挡住。

**（8）系统操作者在库里有一行，但它不能登录。** 触发以保留用户 `SchedulerUserID = 1 << 62`
提交。面板在处理这类操作时惰性创建 `scheduler` 行（`role='viewer'`、`disabled=1`、
`password_hash='!system-account-no-password'`）：任务表要外键到用户，凭空一个 id 会破坏
库的自洽。该账号登录时得到的回答与"没有这个账号"完全一致，用户列表里不出现。计划产生的
任务在 P12.4 只能从库里读——`/jobs` 页面按用户隔离，读不到系统操作者名下的任务。

**（9）代理产生的定时操作会被对账导入。** 代理提供
`GET /internal/v1/operations?actor=scheduler&since=&since_id=&limit=`（只认 `actor=scheduler`），
Web 的对账循环每 30 秒一轮，在锁内先 `ImportScheduled` 再 `SweepPending`。导入三行如实
抄写：`users`（上面那行 `scheduler`）、`preflights`（代理给的预检引用与结论）、`jobs`
（状态、时间、结果）。导入行的 `cancellable=0`（取消要通过代理的取消路由，面板上的取消
按钮对定时任务不成立）、`needs_sync=1`（下轮继续从代理读回状态）。水位取
`jobs` 表里该用户按 `(created_at, operation_id)` 排序的最后一条，因为代理按这个复合游标
严格向后分页；只用创建时间会在同一毫秒有多条操作时整批漏记录，用导入时间做水位则会漏掉
创建早于上次同步、完成晚于它的操作。单条导入失败后停止处理本批后续记录，水位只推进到
最后一条成功记录，失败项及其后记录下一轮重试。

`needs_sync` 的语义只有一句话：**Web 有没有成功把这条操作的状态抄回来过**。它与
"能不能改这条任务的状态"是两件事——`jobs` 的终态不可回退，所以对账的条件更新会跳过
终态行，`needs_sync` 必须单独收掉（只动这一列，状态一个字节都不改）。不这么做的话，
导入时就已经跑完的定时操作（最常见的形状：脚本两秒结束、对账三十秒后才导入）会永远留
在待对账集合里：面板那栏一直说"还没核对过"，扫描每 30 秒再问一遍代理，没有尽头
（DECISIONS Q381）。代理的阶段与本地终态不一致时留一行 warn 并保留本地记录——终态是
终的，分歧进日志而不是靠改写历史来"对齐"。

**（10）脚本以固定解释器和非 root 身份运行。** 脚本落盘在
`/var/lib/upanel-agent/scripts`，`root:upanel-shell` `0750`，解释器固定 `/bin/sh`
（不接受调用方给解释器路径）。`interpreter` 在库内、镜像接口里与这里**同一个写法**：
`/bin/sh` 是绝对路径，不是基名 `sh`——同一个名字在库里和接口里指两个东西，读库的人迟早
会漏掉那一次翻译。脚本目录的每一级上级都要能被 `upanel-shell` 穿过：目录自身
`root:upanel-shell 0750`，上级只补 other 的 `x` 位（`0700 → 0711`，能穿过去、列不出来），
保存与运行前各核一次。运行身份 `upanel-shell`，该账号不能 sudo、不在 docker/disk
组里，因此脚本写不了 `/etc`——`script.run` 不是"root 顺便替用户跑脚本"的旁路。运行前
核对落盘文件的 SHA-256 与登记值，不符拒跑；正文只在详情里返回，列表不带正文。被计划
引用的脚本不能删（`SCRIPT_IN_USE`）。超过 `max_runtime_seconds` 的脚本被终止，任务原因
如实写「脚本 "x" 超过 N 秒被终止」，并且不留 `sleep` 子孙进程。

**（11）P12.4 期间尚未接线、P12.5 已接上的部分。** P12.4 保存 `certificate_renewal` 类计划
会被 `SCHEDULE_KIND_UNSUPPORTED` 拒绝；P12.5 把这条闸拆了——种类参数进定义、目标按盘上
live 目录判、参数只带域名与 ACME 环境（§12.5）。三类计划现在都能落成单元并按槽位触发。

**（12）读一份计划列表的代价随计划条数增长，调用上限也按这个定。** 每条计划的现场都要
真的问 systemd 两次（`systemd-analyze calendar` 算下次触发、`systemctl show` 读单元事实），
所以代理侧组装列表是**有界并发**（`schedules.ViewsConcurrency = 4`，按下标回填，顺序不变），
Web 侧给计划与脚本镜像单独一个 `agentclient.ScheduleQueryTimeout = 30s` 的客户端：代价随
行数增长的只读接口不套 10 秒通用上限——套上去的结果是计划越多、代理越健康，越容易被报成
`AGENT_UNAVAILABLE` 并回 503（DECISIONS Q385）。`ctx` 被调用方取消时列表接口报那个错误，
不把"我们不等了"落成"单元读不到"。

### 12.5 P12.5 已实现范围：证书续期 timer 与"定时触发也写得回账本"

**（1）证书续期计划的种类参数随定义存。** `ScheduleDefinition.certificate = {domain, staging}`，
只有 `kind=certificate_renewal` 能带它（别的种类带了直接报 `certificate` 字段非法，不静默忽略）。
理由是触发场景本身：`certificate.renew` 在那一天由 systemd 拉起，那一刻没有面板可问，而
"续哪个域名、用哪个 ACME 环境"必须在那之前就定下来。`staging` 尤其不能省略——它决定
`--server`，用正式入口续一张 staging 证书会换掉一个本不该上生产的对象，反过来更糟
（用测试入口续正式证书，盘上那张就变成浏览器不认的了）。

**（2）域名与 ACME 环境由面板按证书记录展开，不信调用方。** `schedule.apply` 的定义里
`target_id` 与 `certificate.domain` 是"指向哪张证书"的两个来源，两者不一致时界面说的是 A、
凌晨续的是 B，而 systemd 那边没有人能拦。代理判不了这件事（证书账本在面板库），所以
Web 在预检之前展开：`target_id` 必须是 `crt_` + 32 位小写十六进制、记录必须存在、来源必须
是 `certbot`（手动导入的证书 certbot 续不了，给它建定时续期只会每晚失败一次），然后按记录
**填**域名与 staging。调用方给了这一块就核对，不一致回 409 并点名两边的值——不做"以服务端
为准"的静默覆盖，那是把一个界面 bug 变成一条看起来正常的计划。

**（3）代理侧按盘上 live 目录回答"这张证书还在不在"。** `TargetLookup` 的入参因此从
`(kind, target_id)` 改成整份定义：判的目标事实不在任何一张表里，而在
`/etc/letsencrypt/live/<domain>`。三种结论分得很开——目录在 → 可跑；不存在 → `SCHEDULE_TARGET_NOT_READY`
并写明"这张证书不是 certbot 签的，或者已经被删掉了"；路径被同名文件占着 → 也说 NOT_READY 但
说法不同（certbot 写不进去）；读盘报错（权限、IO）→ 交回真实错误，不伪装成"证书不在"。域名
为空的老定义在代理侧报 `SCHEDULE_TARGET_NOT_FOUND`（判不了盘就是找不到）。

**（4）定时续期的参数只有三样。** `{domain, staging, scheduled: true}`。没有 `certificate_id`、
没有邮箱：两者都在面板库，触发那一刻问不到；域名已经足够决定续哪张证书（live 目录按域名），
ACME 账号在机器上、certbot 自己找。`IssueParams` 因此多一个 `scheduled` 字段，`Normalize`
在它为真时允许这两个字段为空——**可空不等于随便填**，填了照常校验形态。

**（5）定时续期不强制重签。** `--force-renewal` 只挂在"人在面板上点续期"那一下（他要的是
立刻重签，certbot 的"还没到期、跳过"会让按钮看起来没反应）。签发与定时续期都用
`--keep-until-expiring`：一张还没到期的证书不会被无谓地重签——每晚强制重签，一周就能把
Let's Encrypt 对某个域名的签发限额烧光（同一域名每周 5 张）。

**（6）定时续期也写得回面板的证书账本。** 两条路要补，缺一条面板上的指纹与到期时间就会
停在续期之前：
- **参数**：`JobApplier.applyIssue` 在 `certificate_id` 为空时按域名找记录（`domain` 上有唯一
  索引）。记录 id 不变，站点绑定与审计才跟得住；邮箱参数为空时保留原来的（`Renew` 的既有
  语义），不去改它；域名在账本里根本没有时如实报错并**不**建行——定时续期只按域名对账。
- **时机**：导入路径落下的任务经常"导入时就已经跑完"（触发两秒结束、对账三十秒后才导入），
  这样的行一进库就是终态，永远不会有"从非终态推进到终态"那一下去触发 `OnJobFinished`。
  所以终态的导入自己补一次结束回调，且只补一次（同一条 `operation_id` 的导入是幂等的）。

**（7）备份页与计划任务页（前端）只读定义、只经预检写。** 两页都只对 admin 出现（导航里
按 `implementedIn` 出现，路由 `/backup`、`/schedules`）。写操作全部落到 `StorageActionDialog`
这一条链上（`POST /operations/preflight` + `POST /jobs`），页面里没有"直接 PUT 一份定义"的
路径：

- 备份：仓库与策略的**登记/修改**走 REST（仓库 POST/PATCH 要 CSRF 与 5 分钟内再认证，错误码
  `REAUTH_REQUIRED` 由页面调 `/auth/reauth` 后重试**同一份** payload 与幂等键；策略不引用秘密，
  只要 CSRF），而 `backup.repository.initialize`、`backup.run`、`backup.restore`、`backup.verify`、
  `backup.prune` 走预检 → 任务。契约里没有"删除仓库"这条 REST 路由，页面因此也不给入口——删掉
  定义会让仓库里那些快照"属于谁"无从回答。
- 计划与脚本：`GET /schedules`、`GET /schedules/:id`、`GET /scripts`、`GET /scripts/:id` 是
  唯一的读路径；`schedule.apply`、`schedule.delete`、`script.apply`、`script.delete`、
  `script.run` 全部走预检 → 任务（都 `RequiresReauth`，`script.run` 另带 `max_runtime_seconds`）。
  `POST /internal/v1/schedules/:id/trigger` 是代理内部路由（只认 UID=0 的本机 CLI），浏览器
  够不到它；页面上因此没有"立即触发一条计划"，要现在跑一次只有备份页的「立即备份」与脚本的
  「立即运行」。
- **页面不自己拼待删快照名单**：`backup.prune` 的参数只有 `{backup_policy_id, version}`，
  待删集合由预检算出、代理写进 `normalized_params.expected_delete_ids`，提交时按同一份预检
  复核（期间多一份新快照即 `PREFLIGHT_STALE`）。把名单再从页面抄一遍进参数，只会让人以为
  那是来自另一处的证据。
- **快照列表不带大小**：`size_included=false` 时每条都不带 `size_bytes`/`file_count`（列表不跑
  restic stats，那是分钟级代价），点开详情才读。界面上把这件事写在明面上，否则"没有大小"
  会被读成"快照是空的"。
- **超时分层**：备份动作的预检与提交都用 95 秒（代理侧预检/再核各 90 秒），快照查询 75 秒。
  20 秒的通用上限会把一次正常的预检在前端自己掐断，屏幕上是"请求超时"，服务端其实正在回答。
- **三个观测字段分开显示**：计划的 `enabled`（定义的意图）、`installed`、`unit_file_state`、
  `active_state` 是四件不同的事，页面上不压成一个绿灯；"enabled 打开但 timer 没在跑"必须能
  一眼看见。
- **备份成功不等于恢复已验证**：`backup.run` 的核验是"写完读回来对得上"，结果页每次都写出
  这句话；要"这份数据读得回来"只能单独做一次 `backup.verify`，而且要勾上抽样恢复。

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

### 13.1 P13.1 已实现范围：历史监控的采集、粒度与查询口径

**两条端点，分工固定。** `GET /system/metrics` 回答"过去怎么走的"，只读样本表；
`GET /system/metrics/status` 回答"这个进程到底在不在记"，把运行时事实与库里的行数放在一起。
两者都是 **V** 权限。合成的理由不要试：图会反复拉 Query，而 Status 的行数统计是全表
`COUNT(*)`，合成一个接口等于让每次刷新都做一次全表扫描。

**`GET /system/metrics` 的参数**

| 参数 | 语义 |
|---|---|
| `scope` | 必填，闭集 `host` / `interface` / `filesystem`。缺失或越界 → 400 `VALIDATION_ERROR` |
| `target_id` | 可重复或逗号分隔；为空表示该 scope 下全部目标；上限 64 个，空串与超过 512 字节都拒绝 |
| `metric` | 可重复或逗号分隔；为空表示该 scope 全部登记指标；上限 16 个；不属于该 scope 的指标 → 400，报错里点名并列出可用指标 |
| `from` / `to` | RFC3339。都缺省时 `to=now`、`from=to-1h`；`from >= to` → 400；跨度超过 365 天 → 400；`to` 超前当前时间 2 分钟以上 → 400（未来窗口永远是空的，那不是"没有数据"） |
| `step` | 期望步长（秒）。`0` 表示自动；显式给 `< 本层桶宽` 会被抬到桶宽，不会返回"比原点还密"的插值点 |

**读哪一层由时间跨度决定，不由步长决定。** 判据是横跨多久与实际保留窗口（`web.metrics`
的配置，不是硬编码默认值）：跨度 ≤ raw 保留窗口读 `raw`；≤ minute 保留窗口读 `minute`；
否则读 `hour`。按步长选层是**错的**——步长 3600 去读已被裁掉的 raw 只会得到一条空曲线，
而"空"与"没有历史"在界面上长得一样。

步长的下界是所选层的桶宽（raw 5 秒、minute 60 秒、hour 3600 秒）；按该步长算出的桶数
超过 `max_points`（1440）时，步长加粗到"跨度 ÷ 1440 向上取整、再向上对齐到层宽"。
响应如实回报 `resolution`、`step_seconds`、`max_points`，界面据此解释"为什么被加粗了"。

**`GET /system/metrics` 的响应**：`enabled`、可空 `reason`、`scope`、`from`、`to`、
`step_seconds`、`resolution`、`max_points`、`series[]`、`observed_from`、`observed_to`、
`generated_at`。`series` 每项为 `scope`、`target_id`、`metric`、`unit`、`points[]`、
`missing_points`。`points` 每项为 `timestamp`（**桶的起点**，UTC）、`value`、`min`、`max`、
`sample_count`。

带 `min`/`max`/`sample_count` 不是锦上添花：只有平均值的话，"这一分钟 CPU 冲到 100% 又
落回来"在图上彻底消失。`missing_points` 是"该有、但没有"的桶数，它包含序列首尾没有数据的
部分，因此大于 0 不等于中间断了一段——但它是**唯一**能让人看出曲线有洞的字段。

**缺失就是没有点。** 段 `status != ok`、或值为 null 时不写样本；查询不补零、不插值；
没写的桶只进 `missing_points`。`observed_from`/`observed_to` 都为 null 表示这个区间真的
没有数据，与"有数据但稀疏"是两件事。

**采集口径**：登记 10 个指标——host `cpu_usage_percent`/`load1`/`load5`/`load15`/
`memory_used_percent`；interface `rx_bytes_per_sec`/`tx_bytes_per_sec`（`lo` 不入历史）；
filesystem `used_percent`/`inode_used_percent`/`available_bytes`。Web 每 2 秒轮询代理
快照（比代理 5 秒采集快一档，否则会踩点丢整份快照），按快照的 `collected_at` 去重，
库层 `INSERT OR IGNORE` 兜底；样本时间取**段自己的**采集时间，不取快照时间——采集卡住时
不能把旧值扮成新值。

**三层聚合与保留**：raw→minute、minute→hour，每层只卷**已经结束**的桶（上界是当前桶的
起点）；平均值按 `sample_count` 加权，区间取 `MIN`/`MAX`；聚合窗口等于源粒度的保留窗口，
裁剪点是 `now - 窗口 - 2×桶宽`，先聚合后裁剪。启动时与每轮都按 `NOT EXISTS` 自愈补算，
不依赖"上一轮一定成功"。行数上限（`max_rows`）从最老的桶开始删，**永不删最新桶**——
把整层删空等于让面板显示"没有历史"。

**`GET /system/metrics/status` 的响应**：`enabled`（这个进程能不能回答历史问题）、
`collecting`（采集循环在不在跑）、可空 `reason`、`interval_seconds`、
`filesystem_interval_seconds`、`retention.{raw_seconds,minute_seconds,hour_seconds}`、
`max_rows`、`samples`（`raw`/`minute`/`hour`/`total`）、`last_sample_at`、`last_error`、
`last_error_at`、`inserted`、`aggregated`、`pruned`、`metrics[]`、`series`、`generated_at`。

`series` 是 `map`，**键是 scope**（`host` / `interface` / `filesystem`），值是
`{scope, target_id, metric}` 的数组：这一栏回答的是"这台机器的历史里出现过哪些序列"，
按 scope 分组才让人一眼看出"哪些指标有历史、哪些根本没有"。它**不是**按粒度分组的——
粒度是查询时按跨度选出来的，库里三层同时存在，拿它当键会产生三份几乎一样的清单。

`enabled=true` 且 `collecting=false` 是合法状态：配置里关掉采集之后，旧数据仍然可查。
`last_sample_at` / `inserted` / `aggregated` / `pruned` 只说**这个进程**，重启后从零开始——
它们用来判断"循环真的在动"，不参与任何产品结论。演示模式下两份响应都是 `enabled=false`
且 `reason` 说明是演示模式，不是"没有采集器"。

### 13.2 P13.2 已实现范围：告警规则、判定口径与事件

**规则字段的最终形状。** §13 里那行只列了 `metric`、`target_id`、`comparison`、
`threshold`、`secondary_threshold`、`combination`、`duration_seconds`、
`recovery_threshold`、`cooldown_seconds`、`enabled`、`version`。P13.2 实现时发现一个
表达缺口：`secondary_threshold` 只有数字、没有指标名，而"使用率 ≥ 85% 或剩余 ≤ 10 GiB"
里的第二个数是 `available_bytes` 的量纲，拿它配 `used_percent` 去理解就成了"剩余 10%"——
那是另一条规则。因此**补两个字段**（`secondary_metric`、`secondary_comparison`），
原有字段一个不改、一个不删。

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `name` | string(1—120) | 是 | 给人看的短名，允许重名 |
| `metric` | string | 是 | 必须是 `/system/metrics/status` 的 `series` 里出现过的指标名 |
| `target_id` | string(≤512) | 视 scope | `host` 指标必须为空串；`interface`（网卡名）与 `filesystem`（挂载点路径）必须给出 |
| `comparison` | `gt`/`gte`/`lt`/`lte` | 是 | 告警方向 |
| `threshold` | number | 是 | 量纲随指标（percent / bytes / bytes_per_sec / count） |
| `secondary_metric` | string \| null | 否 | 第二口径的指标名；与 `metric` 同一 scope 且不同名 |
| `secondary_comparison` | `gt`/`gte`/`lt`/`lte` \| null | 否 | 第二口径的方向 |
| `secondary_threshold` | number \| null | 否 | 第二口径的阈值 |
| `combination` | `any`/`all` | 否（默认 `any`） | 双口径的合成方式；没有次级口径时恒为 `any` |
| `duration_seconds` | int(30—86400，且 ≥2×指标写入节奏) | 是 | 持续多久才算数；上限等于 raw 粒度保留窗口；容量指标的实际下限是 60 |
| `recovery_threshold` | number | 是 | 恢复线，必须严格落在告警线**安全侧**（`gte`/`gt` 时更小、`lt`/`lte` 时更大） |
| `cooldown_seconds` | int(0—86400) | 否（默认 300） | 恢复之后多久之内不再重新触发 |
| `enabled` | bool | 否（默认 true） | 停用会把打开的事件收口 |
| `version` | int | 读/写 | 乐观并发；PATCH 必须带，不匹配回 409 |

`scope` **不出现在请求里**：它由 `metric` 反查得到（同一个指标只在一个 scope 下登记）。
让调用方同时给 `scope` 和 `metric`，就多出一种"两者不匹配"的非法输入要解释，
而它没有正确的语义可供解释。

三个 `secondary_*` 字段**要么全给、要么全不给**；`secondary_metric` 给空字符串表示
显式清除双口径（此时另两项必须省略）。半套规则没有意义：有指标没阈值是空条件，
有阈值没指标不知道量纲。

**响应里的状态字段。** 除上表外，规则还带 `scope`、`state`（`ok`/`pending`/`firing`）、
`state_since`、`recover_since`、`cooldown_until`、`last_eval_at`、可空 `last_value`
（最近一轮朝告警方向最极端的值）与 `last_reason`（最近一轮为什么没下结论）。
`last_reason` 是"这条规则为什么一直不响"的唯一答案，不是调试信息。它按"最近一轮
对这条规则有话说"写回：数据不足时写清是哪一种不足；`pending` 与"正在计恢复持续"的
**每一轮**都写还差多久（不是只在进入的那一轮写一次，否则第二轮就会把它覆盖成空串，
一条正在攒持续的规则看起来像什么都没发生）；正常超限、正常恢复这两个结论本身不需要
解释，此时为空。

**端点。**

| 方法 | 路径 | 权限 |
|---|---|---|
| GET | `/alerts/rules` | V |
| POST | `/alerts/rules` | admin + CSRF |
| PATCH | `/alerts/rules/:id` | admin + CSRF（body 必须带 `version`） |
| DELETE | `/alerts/rules/:id?version=` | admin + CSRF（`version` 可选，给了就必须匹配） |
| GET | `/alerts/events?state=&rule_id=&limit=` | V |
| GET | `/alerts/silence` | V |
| PUT | `/alerts/silence` | admin + CSRF |

`PATCH` 是真正的部分更新：没出现的字段沿用原值（因此 `enabled=false`、
`cooldown_seconds=0` 这类零值能被区分出来）。`POST` 要求 `metric`、`comparison`、
`threshold`、`duration_seconds`、`recovery_threshold` 显式给出——零值不能当默认值，
一条 `threshold=0` 的 CPU 规则会在第一轮就永远 firing。

`PUT /alerts/silence` 的请求体是 `{starts_at?, ends_at?, reason?}`（RFC3339）。
起止都给 = 设置窗口（跨度 ≤ 7 天，`ends_at` 必须晚于 `starts_at`）；**都省略 = 清除**。
清除是一次显式动作，不是"把窗口设成过去的时间"那种会被时钟漂移意外造成的状态。
响应 `{active, starts_at, ends_at, reason, updated_at, updated_by}`，没有窗口时起止为 `null`。

**判定口径（这是 P13.2 真正的内容，不是实现细节）。**

1. **只读 raw 粒度。** 告警回答"这一刻到底有没有超限"，一分钟聚合的平均值会把 30 秒的
   尖峰抹平。`duration_seconds` 的上限因此是 86400（raw 保留 24 小时），不是随意定的。
2. **"持续"是区间的事实。** 窗口 `(now - duration, now]` 内的**每一个**样本都要在线外
   才算超限；有一个样本落回线内就从 `pending` 掉回 `ok`。用区间平均判断会把
   "冲高 30 秒又落回来"抹平，而那正是最需要看见的东西。
3. **恢复要跨过另一条线。** `recovery_threshold` 与 `threshold` 之间的样本既不算超限、
   也不算恢复——这条死区就是防刷屏的全部机制。恢复同样要持续 `duration_seconds`。
4. **没有数据不是 0。** 窗口里没有样本、最新样本超过容差没更新、或者窗口前段还没攒够
   （最早样本晚于窗口起点 + 2 个写入节奏）——这三种情况状态机**原地不动**：不触发、
   不恢复，只把原因写进 `last_reason`。采集停掉十分钟就自动"恢复"一次的告警，比没有告警
   更坏。同理，已经 firing 的规则遇到数据中断会继续开着——"不知道"不能得出"已经好了"。
5. **容差按序列自己的写入节奏算，不按 raw 桶宽一刀切。** 容量序列
   （`used_percent` / `inode_used_percent` / `available_bytes`）走 `storage` 的 30 秒缓存，
   30 秒才落一行；主机与网卡序列 5 秒一行。因此：新鲜度界限 = `max(3×节奏, 20 秒)`
   （主机 20 秒、容量 90 秒），窗口前段的缺口余量 = `max(2×节奏, 10 秒)`
   （主机 10 秒、容量 60 秒）。**这一条是验收在真机上撞出来的**：按 5 秒判容量序列，
   一条空间规则的表象是"配好了永远不响"，而 `last_reason` 写的是"窗口内没有任何样本"。
   同理 `duration_seconds` 有第二个地板：至少装得下**两个**采集周期（主机 ≥30 秒、
   容量 ≥60 秒），否则"窗口内每一个样本都超限"这句话没有内容，规则直接回 400。
6. **双口径的合成是对偶的。** `any`：窗口满足任一条件即超限，恢复要求**两个都**回到安全侧；
   `all`：窗口满足全部条件才超限，恢复只要**有一个**回到安全侧。次级口径没有恢复线字段，
   它的恢复就是"不超限"。
7. **状态机三态。** `ok --超限--> pending --超限持续够--> firing --恢复持续够--> ok`。
   数据不足时状态不变，因此不存在第四个状态。"unknown"不是状态，是这一轮没做判断。
8. **冷却。** 恢复时写入 `cooldown_until = now + cooldown_seconds`；冷却期内重新超限
   不重新起算。阈值贴着常态（备份刚跑完的那半小时）是最常见的刷屏来源，靠它挡住。

**事件的形状与去重。** 一个事件周期**只有一行**：触发时插入 `state=firing`，恢复时把同一行
改成 `resolved` 并写上 `resolved_at`、`duration_seconds`（实际时长）与 `peak_value`
（按 `[started_at, resolved_at]` 重算的极值，不是触发那一刻的值）。因此"恢复只发一次、
未变化不刷屏"是结构保证而不是逻辑保证：`alert_events` 上有一个
`UNIQUE INDEX ... ON alert_events (rule_id) WHERE state = 'firing'`，同一时刻一条规则最多
一条未恢复事件。

事件行里存**规则快照**（`rule_name`、`scope`、`target_id`、`metric`、`comparison`、
`threshold`）：规则改名、改阈值、甚至被删掉之后，这条历史仍然说得清"当时是什么条件"。
显示用的字段一律读快照，不回 join 规则表——join 出去的值会随规则改动而"穿越"，
让一条旧事件看起来像是按新阈值触发的。

规则被删除时事件**不级联删除**：`rule_id` 是 `ON DELETE SET NULL`，事件的 `rule_id` 变 0，
其余字段照旧。删除或停用规则之前会先把打开的事件收口——否则没有任何东西会再评估它，
那行会变成一条永远 firing 的孤儿行。

**`silenced` 与它的边界。** 事件触发时若落在维护静默窗口内，行上记 `silenced=1`。
事件照记、判定照跑，只是通知（P13.3）不发：机器确实超限了，这是事实，不该因为
"正在维护"就假装没发生。静默**不影响规则的状态机**。

**事件列表的响应字段**：`id`、`rule_id`（规则已删除时为 0）、`rule_name`、`scope`、
`target_id`、`metric`、`comparison`、`threshold`、`state`（`firing`/`resolved`）、
`started_at`、可空 `resolved_at`、`duration_seconds`、可空 `peak_value`、`silenced`、
可空 `acked_at`、`acked_by`。`POST /alerts/events/:id/ack` 属 P13.4（面板上的确认动作），
P13.2 只把列准备好。

**审计动作**：`alert.rule.create` / `alert.rule.update` / `alert.rule.delete` /
`alert.rule.manage`（被中间件拒绝）/ `alert.silence.update` / `alert.silence.manage`。
规则变更同时记改动前后的阈值、恢复线、持续、冷却与启停——只记改后值等于把一半事实丢了。

### 13.3 P13.3 已实现范围：通知通道、投递账与"由谁发送"

**五个端点，全部 admin + CSRF**（连读也是 admin：通道列表带着 webhook 地址与收件邮箱，
那是"告警会发给谁"的答案，与 /alerts 的读开给 viewer 不是一回事）：

| 端点 | 语义 |
|---|---|
| `GET /notification-channels` | 列表。字段 `id`、`name`、`type`、`settings`、可空 `secret_ref`、`enabled`、`max_attempts`、`version`、`target`、`created_at`、`updated_at` |
| `POST /notification-channels` | 新建。`type` 与 `name` 必填；`max_attempts` 缺省 3 |
| `PATCH /notification-channels/:id` | 修改，**`version` 必填**（防覆盖）。`settings` 是**整体替换**，不是逐字段合并 |
| `DELETE /notification-channels/:id` | 删除，`version` 可选。in_app 拒删（400） |
| `POST /notification-channels/:id/test` | **显式**测试发送，只发一次、不重试；回投递行 |

`type` 是闭集 `in_app` / `smtp` / `webhook`。`settings` 按类型解释：smtp 用
`host`/`port`/`tls`/`from`/`to`/`username`；webhook 用 `url` 与 `allow_private`。
`allow_private` 缺省为 `false`，只有管理员对单条通道显式设为 `true` 才允许连接非公网目标。
类型用不上的字段一律清空（`normalizeChannel` 做这件事，库里不存垃圾）。`target` 是只读的
人读形式（smtp 是收件人列表、webhook 是 URL），由服务端算，不接受提交。

**秘密从哪里来、到哪里去。** SMTP 口令与 webhook 令牌**不入面板库**：调用方先按
P02.4 的通道把口令上传成用途 `notification_password` 的秘密，再把返回的 `secret_ref`
填进通道。面板侧只保存这个引用。

**发送由代理执行。** `POST /internal/v1/notifications/deliver`（Web 账号专用）是唯一
的出口：面板把 `type`、`settings`、`secret_ref`、`title`、`body` 交给代理，代理自己去
加密库里解明文、发出去、回一张回执。这样做的理由是**明文不出持有它的进程**：如果面板
自己发，就必须给它一个"取回 SMTP 口令"的接口，而那是 CONTRACTS 从 P02.4 起就拒绝开
的洞。代价是代理多了"向外发一个请求"的能力，所以代理侧的限制是硬的、不看调用方：

- 只允许 `smtp` 与 `webhook`，没有第三种协议，也没有"自定义命令"；
- webhook 只接受 `https`，**拒绝 URL 里的用户名口令与 `#片段`，不跟随重定向**，
  响应只读 4 KiB；缺省只允许公网目标，并在每次实际 Dial 前逐个检查 DNS 答案，防止保存后
  解析变化或 DNS 重绑定绕过检查。单通道 `allow_private=true` 是管理员的显式内网例外，
  只放宽目标网段，不放宽 HTTPS、证书校验或重定向禁令；
- SMTP 只接受 `starttls` 或 `implicit`，**没有明文模式**；AUTH 只走 PLAIN，且必须在
  TLS 上（`smtp.PlainAuth` 自己也会拒绝非 TLS 连接）；
- 单次投递总时长上限 **8 秒**（小于面板侧 10 秒的调用上限，免得面板先把代理掐掉）；
- `secret_ref` 必须指向用途为 `notification_password` 的秘密。**这一条由代理自己核对，
  不靠调用方自觉**：否则面板只要把它填进一条自己控制的 SMTP 投递，就能让代理把仓库
  口令当 AUTH 口令发到自己的服务器上——"不能读明文"被绕成"把明文发给我"。
  用途核对与"不存在"回同一个 400，不区分，免得变成探测秘密用途的接口。

回执形状：`accepted`、`status`（SMTP 应答码或 HTTP 状态码，0 表示没拿到）、`detail`
（对端原话，截到 300 字符，不含秘密）、`retryable`。**投递失败也回 200**：从调用方看，
"代理按你的要求去发了、被对端拒绝"与"代理没理你"是两件事，做成 5xx 会让面板只能猜
该重试还是该放弃。`retryable` 由代理判定（它才知道那是 4xx 临时拒绝、超时，还是 5xx
永久错误）：SMTP 4xx、连接失败、超时、HTTP 429/5xx 为真；SMTP 5xx、证书错误、
HTTP 3xx 与其余 4xx 为假。

**投递账（outbox）。** 每次投递一行 `notification_deliveries`：通道快照（名字、类型、
目标）、事件引用（测试发送为 NULL）、`kind`（`fired`/`resolved`/`test`）、标题正文、
`state`（`pending`/`sent`/`failed`）、`attempts`、`max_attempts`、`next_attempt_at`、
`last_error`、`last_status`、`sent_at`。

- 入队与告警事件**在同一笔事务**里（`alerts.EventNotifier` 在 `applyOutcome` 的事务内
  被调用）：要么事件与投递一起进去，要么一起没有。中间崩一次不该留下"事件在库里、
  谁也不知道它发生过"。
- 同一事件在同一通道上每种 `kind` 只一条（部分唯一索引 `(event_id, channel_id, kind)
  WHERE event_id IS NOT NULL`）。**重试是改这一行，不是再插一行**。
- **维护静默的事件一条都不入队**（fired 与 resolved 都是）：一条你从未被告知开始的告警，
  不该用一条"它结束了"来收尾。因停用/删除规则而收口的事件同样不发。
- in_app 通道的投递**写入即 `sent`**：它不发到任何地方，读投递账就是读面板收件箱
  （读路径与桌面通知属 P13.4）。它是迁移里种下的唯一一条，不可删、只可停用。
- 通道被删或被停用时，到点的投递判 `failed` 并写清原因（"通道已停用，未发送"），
  **不计入 attempts**：一次都没发出去的东西不该看起来像试过一次。
- 退避：第 n 次失败后排 `30s × 2^(n-1)`，**上限 10 分钟**。上限不是怕算得太大，而是怕
  一次"重试上限 10"的通道把最后一次尝试排到几小时之后——那时人早就以为它在正常工作了。
- `max_attempts` 建在通道上（默认 3，上限 10）：重试几次是"这条通道有多重要"的一部分。

**测试发送**是显式动作，保存配置**绝不**自动发：保存一次就往外面喷一封邮件，是那种
"没人要求、但取消不掉"的行为。它不重试（要的是"此刻能不能发出去"这个事实）、立即尝试
一次、仍然记一行 `kind=test` 的投递账、结果原样回给调用方（含对端原话）。
in_app 回 400（没有外部动作可做），没有发送端回 503 `AGENT_UNAVAILABLE`
（"代理没连上"会自己好起来，不该说成"联系管理员"）。

**审计动作**：`notification.channel.create` / `.update` / `.delete` / `.test`，以及
被中间件拒绝时的 `notification.channel.manage`。记通道名、类型、目标、`settings`（里面
本来就没有口令）、`secret_ref`、启停、重试上限、版本；修改与删除额外记改动前的值。
测试发送在**投递之前**落审计：这次点击真的会让一个请求离开这台机器。

**已知限制（不在 P13.3 修）**：审计写入器按**键名片段**做红字化，`secret_ref` 命中
`secret` 片段，因此审计里只留下 `"secret_ref":"[redacted]"`——"用了哪份凭据"这件事在
审计里丢失。脱敏方向是"按键名 + 值类型白名单"，那会改到 P08/P12 已有的审计语义，
需要单独一轮评估，不在 P13.3 顺手改。

### 13.4 P13.4 已实现范围：确认、投递账读路径、历史曲线的展示口径

**两个新端点。**

| 端点 | 权限 | 语义 |
|---|---|---|
| `POST /alerts/events/:id/ack` | admin + CSRF | 确认一条事件。请求体为空；确认人取自会话 |
| `GET /notification-deliveries` | admin | 读投递账。过滤 `state`、`kind`、`limit` |

**确认不是恢复。** 这是本节最要紧的一条：`acked_at`/`acked_by` 是**旁挂**字段，写入它们
不动 `state`、`resolved_at`、`last_seen`，也不进规则的状态机。一条 `firing` 的事件被人点过
“知道了”之后仍然是 `firing`——恢复只由评估循环按 `recovery_threshold` 判。做成状态迁移
的话，“有人在管”与“事情已经好了”就成了同一格，而线上最需要区分的正是这两件事。

四条语义：

- **幂等**：第二次确认原样返回第一次的 `acked_at` 与 `acked_by`，不刷新时间。刷新等于
  “最后一次有人看”，那是另一列的事，本轮不开。
- **第一个确认人胜出**：写入条件是 `WHERE id = ? AND acked_at IS NULL`。两个人同一刻点，
  只有第一个落到库里，第二个人读回第一行——谁也没覆盖谁。
- **已恢复的事件也能确认**：“我看到了”在恢复之后仍然有意义，否则“哪些事后来有人查过”
  会在恢复那一刻断掉。
- **viewer 不能确认**：只读会话能看事件列表（§13.2），但认领是把名字挂到一次处置上的
  动作，与写同一档。

审计动作 `alert.event.ack`，target 为 `alert_event:<id>`，details 记 `rule_id`、`state`、
`acked_at`、`acked_by`。事件不存在时回 404 `NOT_FOUND`（消息“告警事件不存在”）并记
denied；id 非正整数回 400。“确认了一条不存在的事件”必须是可见的失败，不能是 200。

**投递账读路径是顶层路径，不挂在通道下。** 一行投递带着的是**投递当时**的通道快照
（名字、类型、目标），通道后来改名、停用、删除都不改变那一行——挂在
`/notification-channels/:id/` 下会把它说成“这个通道此刻的属性”。另外面板收件箱要回答
“最近发出去过什么”，那是跨通道的排序，挂通道下会变成“先列通道再逐个查”的 N+1。

- 行形状与 §13.3 通道端点里的投递回执**共用同一个 DTO**：`id`、`event_id`（测试发送为
  null）、`event_rule`、`channel_id`、`channel_name`、`channel_type`、`channel_target`、
  `kind`、`title`、`body`、`state`、`attempts`、`max_attempts`、`next_attempt_at`、
  `last_error`、`last_status`、`sent_at`、`created_at`。
- 过滤参数：`state` ∈ `pending`/`sent`/`failed`、`kind` ∈ `fired`/`resolved`/`test`；
  `limit` 默认 50、最大 200。非正整数回 400 `VALIDATION_ERROR`（"limit 必须是正整数"），
  **超过 200 按 200 截断而不报错**——“多要一点”不是参数错，是上限在生效；枚举取闭集外的
  值回 400（报错列出允许的取值）。
- **admin，连读也是 admin**：账里有收件邮箱、webhook 地址原文与正文。`/alerts` 那套
  “读开给 viewer”在这里不成立，理由与 §13.3 的通道列表相同。
- in_app 通道“写入即 `sent`”（§13.3），因此这条读路径就是**面板收件箱**：它没有外部
  发送方，读投递账是它唯一的读法。

**历史曲线的展示口径（前端约束，不是接口）。** 三件事写死在这里，因为它们是“图上会不会
撒谎”的问题，不该由每个页面各自判断：

- **缺失不是 0。** `metric_samples` 缺失的时间段**没有行**（§13.1），服务端不补零。前端
  按桶宽切段：相邻两点相差超过 `3 × step` 就断开，段与段之间**什么都不画**。把点按顺序
  连成一条，一次十分钟的采集中断会变成一条平滑直线——图上看是“一切正常”，真相是“这十
  分钟我们什么都不知道”。宽限取 3 个桶而不是 0，是因为采集节奏本身有抖动，相差 1—2 个
  桶是正常的。
- **一个点都没有时不画轴。** 上下界为 null 时说“这段时间没有样本”，而不是画一条贴着 0
  的线。单点画成小方块：它存在，但接不上任何东西。
- **`last_value` / `peak_value` 为 null 时显示“—”。** 那不是 0，是“这一次评估没有取到
  值”（例如采集被停用）。

**采集陈旧分三态，三句话不一样。** 判据（注意两个 `enabled` 不是一个意思，这是最容易
搞混的一处）：

- `status.enabled=false` → **未开启**：这个进程**没有库**，一条历史都答不了（演示模式 /
  未装配）。它不是故障，是模式。
- `status.enabled=true` 且 `collecting=false` → **没在采集**：库在、旧数据查得到，只是不再
  写新的。配置里 `web.metrics.enabled=false` 属于这一种，`reason` 会写明原因
  （”配置里关闭了历史采集（web.metrics.enabled=false）…”）。所以**关掉采集不等于
  `enabled=false`**：把两者混为一谈，界面会把一份正常工作的产品显示成”没有库”。
- `collecting=true` 但 `last_sample_at` 距今超过 `max(3 × interval_seconds, 20s)` → **陈旧**
  （循环还在、写不进去，此时 `last_error` 才是要看的）。

`last_sample_at` 为空时按 `collecting` 分开：没在采集的归”没在采集”，在采集却从没写过样本的
归”陈旧”，**两种都不许显示成正常**。查询接口（`GET /system/metrics`）的 `enabled` 也按同一
口径：它回答”这次查询有没有历史可比”，不回答”现在在不在记”——后者只有 status 答。

宽限取 `max(3 × interval, 20s)` 是因为主机 5 秒一行、容量 30 秒一行：拿 5 秒去判容量，
会把正常状态报成陈旧。

### 13.5 P14.1 已实现范围：网络现状的四条只读端点与"能不能改"的判定

**四条端点，全部 admin，全部只读。** `GET /network/interfaces`、`GET /network/routes`、
`GET /network/listeners`、`GET /firewall`。它们回答的是"现在是什么样"和"这份现状我们将来
能不能安全地改"，**一个字节都不写**：改地址是 `network.apply`（P14.2）、改规则是
`firewall.apply`（P14.3），两者都走预检 → 任务 → 独立回滚 timer。在这一组里开一个
"直接 netplan apply"或"直接 ufw allow"的入口，等于把确认、回滚与审计一起绕过去。

为什么是 admin 而不是 V：网卡、地址、路由、监听端口与防火墙规则合起来就是"这台机器上
开了什么入口、通向哪里"，与容器清单同级的侦察材料。

**审计只记被拒的那一次，不记成功的读。** 四个路由组登记了 `network.manage` /
`firewall.manage`，而登记的作用是让**中间件拒绝**落一条 `outcome=failure`——"某个 viewer
在什么时候试图看这台机器的网络全貌与防火墙规则"是值得留痕的那件事。成功的读取不写审计：
页面一打开就是四条 GET，刷新一次再加四条，一天下来审计表里只剩这一种行，真正的管理事件
会被淹掉（与 §9 `PUT /apps/:id/favorite` 不写审计是同一条理由）。写路径 `network.apply` /
`firewall.apply` 的审计在 P14.2/P14.3，记的是改了什么、凭什么改、改完读回来是什么。

**三条贯穿四条端点的口径。**

1. **查不到不等于没有。** 每一条响应都带 `issues[]`：`source` ∈ `sysfs`/`ip`/`proc-net`/
   `netplan`/`ufw`/`systemd`，`code` ∈ `missing`/`permission_denied`/`timeout`/`unparsable`/
   `failed`，`detail` 是给人看的一句话。某一路来源读不到时**其余内容照常返回**，
   HTTP 仍是 200——把"读不到"折成空数组，页面上就是"这台机器没有监听端口"。
2. **不猜。** 网卡类型从 `/sys/class/net/<name>/{bridge,bonding,wireless,phy80211,tun_flags,type}`
   与 `/proc/net/vlan/<name>` 判，不从名字猜；配置归属从 `/etc/netplan` 的**存在**与 `/run`
   下的**渲染产物**判，不解析 netplan 的 YAML（P14.2 的写路径会自己逐字段核对）。
3. **只读也不许慢。** 一次采集的总预算 8 秒，单条命令 6 秒；超时如实记 `timeout`。

**`GET /network/interfaces`**：`collected_at`、`interfaces[]`、`capability`、`issues[]`。

- `interfaces[]`：`name`、`index`、`kind`（闭集 `loopback`/`ethernet`/`bridge`/`bond`/`vlan`/
  `wifi`/`tun`/`other`）、`mac`（小写）、`mtu`、`state`（内核 operstate 原样小写）、
  `carrier`（`true`/`false`/**`null`＝读不到**，虚拟网卡常常读不到）、`master`、
  `physical`（`/sys/class/net/<name>/device` 存在）、`addresses[]`（`family`、
  `address`、`prefix_len`、`scope`、`type`）。
- 地址与网卡清单来自 `ip -j addr show`；sysfs 补 MTU/MAC/载波/物理性。`ip` 不在时**仍从
  sysfs 列出网卡**（只是没有地址），并在 `issues` 里记一条 `ip/missing`。
- `capability`：`backend` ∈ `netplan-networkd`/`netplan-networkmanager`/`networkmanager`/
  `unknown`、`editable`、`target`（可改的那张网卡）、`reasons[]`。
  `editable=true` **仅在** 以下全部成立时：`backend=netplan-networkd`，运行时网卡里没有
  Wi-Fi/网桥/bond/VLAN，物理以太网卡**恰好一张**且没有 `master`，NetworkManager 没在跑
  （`/run/NetworkManager` 不存在）。`editable=false` 时 `reasons` **至少一条**，写清究竟是
  哪一条不成立——灰掉的按钮后面没有原因，人只会以为面板坏了。
  读不到 `/etc/netplan` 归 `unknown`（"不知道"），**不**归"没有配置"。

**`GET /network/routes`**：`collected_at`、`routes[]`、`gateways[]`、`issues[]`。

- IPv4 与 IPv6 各查一次，只查 v4 会漏掉 v6 默认网关。默认路由（`0.0.0.0/0`、`::/0`）
  排在最前，v4 在 v6 前，顺序在代码里固定，不依赖 `ip` 的输出顺序。
- `metric` 与 `pref_set` **成对**：0 是合法优先级，"没有 metric"由 `pref_set=false` 表达。
- `gateways[]` 是去重后的默认下一跳（`family`、`address`、`device`、`metric`）。

**`GET /network/listeners`**：`collected_at`、`listeners[]`、`truncated`、`issues[]`。

- 数据来自 `/proc/net/{tcp,tcp6,udp,udp6}`，只取 LISTEN（TCP `0A`）与 UDP `07`，
  **不跑 `ss`**：`ss` 的输出格式在不同版本间会变，而 procfs 的列布局是稳定的。
- 进程归属由 `/proc/<pid>/fd` 的 `socket:[inode]` 反查；反查不到时 `pid=0`——那是
  **读不到**，不是"没有进程"。套接字 inode 只用于反查，出响应前清零，不上线。
- `truncated=true` 表示结果被上限（2000 条）截断，页面必须说明"这不是全部"。

**`GET /firewall`**：`collected_at`、`manager`、`manager_reasons[]`、`installed`、`enabled`、
`default_incoming/outgoing/routed`、`logging`、`rules[]`、`editable`、`reasons[]`、`issues[]`。

- `manager` ∈ `ufw`/`firewalld`/`nftables`/`iptables-persistent`/`none`/`unknown`。
  **`none` 不是"没有防火墙"**，`unknown` 也不是：ufw 与 firewalld 同时在场时判 `unknown`，
  因为两套规则同存时"我加了一条放行"在另一套的默认拒绝面前什么都不是。`manager_reasons[]`
  写清是哪条事实让它这么判。
- **每类管理者都要有"在管"的证据，不能只看程序在不在。** firewalld：`firewall-cmd`/`firewalld`
  程序在，或 `/run/firewalld` 在（表示它在跑）。nftables：`/etc/nftables.conf` 存在**并且**
  `nftables.service` 被 `enable` 过（`/etc/systemd/system/*.wants/nftables.service`）。
  只认那个配置文件会把**每一台 Ubuntu** 都判成"ufw 与 nftables 同存"——那份文件是 nftables
  包自带的默认规则集，装包就有，而服务默认 disabled，于是 P14.3 的写路径在一台最标准的
  机器上永远打不开（22.04 验收机第一轮就是这么红出来的）。iptables-persistent：
  `/etc/iptables/rules.v4` 或 `rules.v6` 存在（Ubuntu 默认没有这个目录，只有装了才会出现）。
  **已知局限**：手工 `nft -f` 载入规则、但没有 `enable` 服务的机器看不到证据——那种机器上
  内核里确实另有规则，而面板只能看到"没有开机加载的证据"。P14.3 若要在这种机器上开写路径，
  得先有办法读出内核里的规则集，那是另一件事。
- `installed` 与 `enabled` 分开：装了没启用（`Status: inactive`）与根本没装是两件事，
  后者额外有一条 `ufw/missing` 的 issue。
- `rules[]`：`number`、`action`（`ALLOW`/`DENY`/`REJECT`/`LIMIT`）、`direction`
  （`IN`/`OUT`/`FWD`）、`interface`、`to`、`from`、`raw`（UFW 原话，解析不准的靠它兜底）。
- `editable` **仅在** `manager=ufw` 且 `installed` 时为 true；其余一律只读并给出原因。

**代理侧查询名**：`network.interfaces`、`network.routes`、`network.listeners`、
`firewall.state`。四条**无参数**（空结构 + 拒绝未知字段，`{"command":"..."}` 会被拒）。
代理没有接上网络采集端时这四条不登记，`capabilities.supported_queries` 因此不会报告一个
点开就 503 的入口；Web 侧对应地回 503 `AGENT_UNAVAILABLE`，**不给空列表/空规则表**。

### 13.6 P14.2 已实现范围：单物理网卡 IPv4 的受控写入、独立回滚与候选地址确认

P14.1 把"现在是什么样"和"能不能改"读出来了；这一节定义**改**。范围刻意窄到一条缝：
只改 `capability.target` 那一张物理网卡的 **IPv4**（DHCP 或单地址静态），只通过
netplan 写一个面板自己的文件，只走一条能自己退回去的路。IPv6、第二地址、桥接、bond、
VLAN、Wi-Fi、NetworkManager 一律不支持——**返回 `unsupported` 而不是"尽力而为"**：
STAGES 的验收要求写"复杂 netplan 返回 unsupported"，所以"不支持"必须是一个明确答案，
不能是静默降级。

| 方法与路径 | 返回 | 说明 |
|---|---|---|
| `GET /network/config` | 当前可改网卡的生效配置、`config_hash`、是否有未确认的回滚 | admin、只读。`config_hash` 就是 `network.apply` 要带的 `expected_config_hash` |
| `POST /operations/preflight`（`action=network.apply`） | 预检计划 | 改地址前的影响与阻塞项；`locks=["network-global"]` |
| `POST /jobs`（`action=network.apply`） | 任务 | 写配置、装载、核验、**武装回滚 timer** |
| `POST /operations/preflight` + `POST /jobs`（`action=network.confirm`） | 任务 | 保留新配置：停掉回滚 timer |
| `POST /operations/preflight` + `POST /jobs`（`action=network.rollback`） | 任务 | 主动反悔：立刻执行回滚（与 timer 到期走同一条代码） |

**`network.apply` 的参数**

```json
{
  "interface_id": "ens4",
  "expected_config_hash": "sha256:…",
  "definition": {
    "mode": "dhcp",
    "ipv4": null
  }
}
```

```json
{
  "interface_id": "ens4",
  "expected_config_hash": "sha256:…",
  "definition": {
    "mode": "static",
    "ipv4": { "address": "10.0.2.15", "prefix_length": 24,
              "gateway": "10.0.2.2", "dns": ["1.1.1.1"] }
  }
}
```

- `mode` 闭集 `dhcp` / `static`；`dhcp` 时 `ipv4` 必须为 `null`（不是空对象）。
- `static` 时 `address`、`prefix_length`（1—32）必填；`gateway` 可空，非空时必须在
  `address/prefix_length` 同一个网段内（`/32` 不允许带网关）；`dns` 0—3 个，每个是 IPv4。
- 拒绝：`0.0.0.0/8`、`127.0.0.0/8`、`169.254.0.0/16`、`224.0.0.0/4`、`240.0.0.0/4`、
  `255.255.255.255`、以及组播/广播/未指定地址。
- `interface_id` 是**网卡名**（`ens4`）。名字会变（`set-name`、udev 改名），所以身份
  由预检钉死：`interface_id` 解析出来的 `permanent_mac` 与 `expected_config_hash` 里的
  那一份必须是同一个 MAC；MAC 变了就是"这台机器的网卡换过了"，判 `NETWORK_TARGET_CHANGED`。

**`expected_config_hash` 怎么算**

`sha256:` 前缀 + 对下面这份**规范化 JSON**（键排序、无空白、无浮点）的 sha256：

```json
{"interface":"ens4","permanent_mac":"52:54:00:12:34:56",
 "netplan":{"50-cloud-init.yaml":"<文件内容的 sha256>"},
 "panel_file":{"name":"90-upanel-ens4.yaml","sha256":"<…>"}}
```

- `netplan` 是 `/etc/netplan` 下**全部**配置文件的名字与内容哈希（面板自己的那份也在里面）；
- `panel_file` 是面板自己那份文件的现状，不存在时整个键为 `null`。

理由：把"我看到的那份配置"钉住。别的管理员或 cloud-init 在预检之后改了一个字节，
提交时哈希就对不上，判 `PREFLIGHT_STALE`（沿用该码，不新造一个"差不多"的）。
不解析、不"大概等价"——**字节不同就是不同**。

**预检（`network.apply`）**

阻塞项（`blockers`）：

| code | 触发条件 |
|---|---|
| `NETWORK_UNSUPPORTED` | `capability.editable=false`（附 capability 的 reasons 原文）；或目标网卡不是唯一物理以太网卡；或 `/etc/netplan` 里存在我们无法安全覆盖的配置（见下） |
| `NETWORK_CONFIG_CHANGED` | `expected_config_hash` 与现场算出来的不一致 |
| `NETWORK_TARGET_CHANGED` | `interface_id` 现在解析不到，或 permanent MAC 与哈希里的不一致 |
| `NETWORK_DEFINITION_INVALID` | 定义越出闭集（模式、地址、前缀、网关、DNS、取值禁令） |
| `NETWORK_ROLLBACK_PENDING` | 已有一条未确认的回滚记录（先确认或先回滚，**不许叠加**） |

告警项（`warnings`，不拦）：`NETWORK_MANAGEMENT_PATH`——面板的访问路径就在这张网卡上
（`0.0.0.0` 监听或监听地址属于目标网卡的网段）：改完之后浏览器可能立刻断，120 秒后会
自己回来；`NETWORK_NO_CARRIER`——现在没有载波，改完也不会通。

"无法安全覆盖"的判定（`netplaninspect`，用 YAML 解析，不靠字符串嗅探）：目标网卡在
**任何** netplan 文件里出现时，允许出现的键只有
`match`、`set-name`、`dhcp4`、`dhcp6`、`addresses`、`routes`、`nameservers`、`optional`、
`dhcp4-overrides`、`dhcp6-overrides`、`accept-ra`、`link-local`、`mtu`；出现其它键
（`macaddress` 覆盖、`gateway4`、`routing-policy`、`ipv6-*` 之类）就是 `unsupported`。
任何 `bridges`/`bonds`/`vlans`/`wifis` 提到目标网卡 → `unsupported`。文件解析不了
→ `unsupported`（读不懂的配置不许覆盖）。`match.macaddress` 与网卡永久 MAC 不一致
→ `unsupported`（配置本来就不属于这张卡）。

**执行（`network.apply`）**

1. 重新算哈希、重比 MAC、重跑 `netplaninspect`（预检之后现场可能变了）。
2. 记录"改之前的样子"：面板文件是否存在、内容哈希、当前 IPv4 地址、默认网关。
3. 写 `/etc/netplan/90-upanel-<iface>.yaml`（root:root 0600，YAML 由面板渲染，
   原子替换：同目录临时文件 + `rename`）。
4. `netplan generate`——**这一步只渲染、不动网卡**。非零退出一律判失败，恢复原文件
   （或删除我们刚建的），到此为止，任务结果 `rolled_back`（现场没变；恢复本身失败才是
   `unknown`）。
5. 写入回滚记录、渲染并启动 `upanel-netrollback@<token>.timer`（见下）。**先武装、
   再 apply**：反过来的话，`netplan apply` 卡住或把系统弄成不通，就没有人负责把我们
   带回来。
6. `netplan apply`。
7. 核验（读回内核事实，不信 netplan 的退出码）：目标地址在前缀长度上存在、`dhcp` 模式下
   这张网卡上**恰好一个**全局 IPv4 地址（DHCP 服务器可以把同一个地址再发一次，所以不能
   拿"旧地址必须消失"当判据；多出来一个说明别的配置文件也在给这张网卡配地址）、静态模式下
   默认网关等于要求的那一个、旧地址（与新的不同时）已经不在。核验不过 → **立即执行回滚**
   （不等 timer），任务结果 `rolled_back`，并带上回滚后核验到的现场；回滚本身没走完则是
   `unknown` + 记录里 `state=failed`。DHCP 的等待窗 30 秒、静态 15 秒（`dhcpVerifyWindow`）。
8. 结果里给出 `deadline`（回滚到期的时刻）与 `rollback_token`，页面据此倒计时。

结果的取值只在代理的 outcome 闭集里（`verified`/`accepted`/`rolled_back`/`partial`/
`unknown`）——**没有 `failed` 这个结果值**。"改坏了但已经退回去"与"我们不知道现在
是什么样"是两件事，界面对它们的下一步动作不同（前者不用管，后者要立刻去看一眼），
折成一个"失败"就把这个判断依据抹掉了。`failed` 只出现在回滚记录的 `state` 里。

**回滚 timer（独立于面板，这是本条最要紧的一条）**

- 单元：`/etc/systemd/system/upanel-netrollback@.service` +
  `upanel-netrollback@<token>.timer`，`OnActiveSec=120s`、`AccuracySec=1s`、
  `RemainAfterExit=no`、`DefaultDependencies=yes`。
- `ExecStart=` 指向**代理自己的二进制**（`os.Executable()` 在 apply 时取到并写进单元与
  记录），子命令 `netrollback --config <cfg> --token <token>`。不写"面板在运行时生成一段
  root shell 脚本"——那等于把"面板能执行任意 root 命令"这条禁令自己打开。
- 记录：`/var/lib/upanel-agent/network/rollback/<token>.json`（root 0600），内容含
  目标网卡、MAC、候选地址、改之前的文件与内容、到期时刻、执行过的命令与结果、
  `state`（`pending`/`confirmed`/`rolled_back`/`failed`）。
- 回滚做的事与 apply 对称：恢复（或删除）面板文件 → `netplan generate` → `netplan apply`
  → 核验旧地址回来了 → 写 `state=rolled_back`。任何一步失败都写 `failed` + 原因，
  **不重试**（重试会把一个说不清的状态变成两个）。
- 确认（`network.confirm`）＝ 停 timer + 删单元 + 记录 `state=confirmed`。
- **开机重放**：代理启动时扫 `state=pending` 的记录——到期时间已过就立刻执行回滚，还没到
  就按剩余时间重新武装 timer。没有这一步，"回滚期间重启"会留下一个永远不回滚的坏配置。

**确认必须来自候选地址（`NETWORK_CONFIRM_WRONG_ADDRESS`）**

`network.confirm` 的参数是 `interface_id`、`token`、`arrived_on`。`arrived_on` **不是**
前端填的：Web 从这条连接的 socket 本地地址（`http.LocalAddrContextKey`）取，也就是
"浏览器这次是连到哪个地址上来的"。代理要求：

1. 记录存在且 `state=pending`、未到期（过期判 `NETWORK_ROLLBACK_EXPIRED`，让它回滚）；
2. `arrived_on` 归一化后**等于候选地址**——用旧地址到达的确认一律拒绝；
3. 候选地址现在确实在这张网卡上（读回内核证明改成功了），旧地址确实不在。

这样"在旧地址上点确认"永远过不了关：旧地址要么已经不在（连不上），要么在这里被拒。
用地址本身做凭据而不是发一个确认码到浏览器：确认码能转发、能被日志带走，而"你现在是从
哪个地址连过来的"是每次请求都重新发生的现场事实。

**动作策略**

| 动作 | 再认证 | 确认短语 | 可取消 | 资源锁 |
|---|---|---|---|---|
| `network.apply` | 是 | **是**（`RECONFIGURE XXXXXX`，短语码由服务端生成，与其他动作同一套规则；"改成什么"写在计划的显示名与影响列表里，与短语并排显示） | 否 | `network-global` |
| `network.confirm` | 否 | 否 | 否 | `network-global` |
| `network.rollback` | 否 | 否 | 否 | `network-global` |

`network.apply` 要短语：它是唯一一个能把管理员关在门外的动作，短语里写清"改成什么"，
让人在敲之前读一遍。`confirm`/`rollback` 不要再认证也不要短语——它们正是**救命**的两步，
门槛越高，出事后越救不回来。

**审计**

`network.apply`、`network.confirm`、`network.rollback` 都记（`network.apply`/`rollback`
记改之前与之后的现场）；回滚 timer 自己执行的那一次由**代理**记一条
`agent.network.rollback`（面板不在场，所以这条只能由代理写进自己的日志与记录文件，
Web 恢复后从 `GET /network/config` 读到 `state=rolled_back` 并把它显示出来）。

**未接线时**：代理没有接上写入端时不登记这三个动作，`capabilities.supported_queries`
也不会报告 `network.config`；Web 侧 `GET /network/config` 回 503 `AGENT_UNAVAILABLE`，
`network.apply` 的预检回 503，**不给一份看起来能用的空配置**。


### 13.7 P14.3 已实现范围：UFW 规则集的受控写入、独立回滚与"只碰带标记的规则"

§13.6 改的是地址，这一节改的是**规则集**。两条路共用同一套骨架（预检 → 任务 → 武装回滚
→ 读回核验 → 120 秒确认窗），差别在改的东西的性质上，所以分成两节而不是一节：

- 地址是**一份文件**，规则集是**一组条目**，而条目没有名字。所以面板给每条自己加的规则
  挂一个标记（`comment upanel:<id>`），靠它认出"这条是我的"。
- 地址改错的结果是"连不上"，规则改错的结果是"谁也连不上"。所以管理通路（面板 + SSH）
  在规则这条路上是**阻塞项**，不是告警项。
- 只做 **UFW**、只做 **IPv4**、只做"一条规则一个端口一个协议"。规则区间、应用配置
  （`ufw allow 'Nginx Full'`）、按接口限定、`prepend`/`insert` 指定位置、默认策略
  （`ufw default`）一律 `FIREWALL_UNSUPPORTED`——**返回不支持，而不是尽力而为**。

| 方法与路径 | 返回 | 说明 |
|---|---|---|
| `GET /firewall/config` | 面板管理的规则、`config_hash`、启用状态、是否有未确认的回滚 | admin、只读。**只列带 `upanel:` 标记的规则**；管理员手写的规则仍在 `GET /firewall` 里完整可见 |
| `POST /operations/preflight`（`action=firewall.apply`） | 预检计划 | 影响、阻塞项；`locks=["firewall-global"]` |
| `POST /jobs`（`action=firewall.apply`） | 任务 | 武装回滚 timer、加缺的删多的、切启用状态、读回核验 |
| `POST /operations/preflight` + `POST /jobs`（`action=firewall.confirm`） | 任务 | 保留这次改动：停 timer |
| `POST /operations/preflight` + `POST /jobs`（`action=firewall.rollback`） | 任务 | 主动反悔：立刻回滚（与 timer 到期走同一条代码） |

**`firewall.apply` 的参数**

```json
{
  "expected_config_hash": "sha256:…",
  "definition": {
    "enabled": true,
    "rules": [
      { "id": "web",  "action": "allow", "port": 443, "proto": "tcp", "from": "" },
      { "id": "ssh",  "action": "limit", "port": 22,  "proto": "tcp", "from": "" },
      { "id": "db",   "action": "allow", "port": 5432, "proto": "tcp", "from": "10.0.0.0/8" }
    ]
  }
}
```

- `definition` 是**整份**规则集，不是一条。apply 的语义是"加缺的、删多的"：
  少写一条就等于删掉它。
- `enabled` 在定义里：只加规则不启用等于什么都没做（ufw 未启用时规则不生效），
  而启用恰恰是最容易把人关在门外的那一步——它必须和规则一起过预检、一起回滚。
- `id` 形状 `^[a-z0-9][a-z0-9-]{0,31}$`。它进 ufw 的 comment，也进命令行，
  所以先收窄到不需要引号的形状。**id 一旦用过就不要再改含义**——改 id 等于"删一条旧的、
  加一条新的"。
- `action` 闭集 `allow`/`deny`/`reject`/`limit`（与 UFW 的四个动作一一对应，**不做同义词**）。
- `proto` 闭集 `tcp`/`udp`。**一条规则一个协议**：`ufw allow 22` 同时开 tcp+udp，
  读回来是两条规则，"我加了一条"与"读回两条"之间那点含糊会在回滚时变成"删了一条还剩一条"。
- `from` 空表示 `from any`；否则是 IPv4 地址或 IPv4 CIDR，归一成**掩码之后**的 CIDR
  （单地址 → `/32`，`10.0.0.5/24` → `10.0.0.0/24`）。只认 IPv4：v6 规则在 UFW 里是另一份表
  （`user6.rules`），塞进同一份定义会让"v4 那条删了、v6 那条还在"变成真实可发生的状态。
- 规则最多 64 条。再多，页面上的那份列表就变成一面没人读的规则墙。

**`expected_config_hash` 怎么算**

`sha256:` 前缀 + 对下面这段文本的 sha256：

```
enabled=0|1\n
<id> <规范写法>\n     （每条一行，顺序是 `ufw show added` 的顺序）
```

规范写法只有两种形状：`allow 22/tcp`、`allow from 10.0.0.0/8 to any port 5432 proto tcp`。

**只算启用状态与带面板标记的规则**，不算管理员手写的那些。理由：手写规则面板不改，
把它们算进指纹，会让"管理员在别处加了一条规则"变成"这边的预检作废"。指纹要防的是
"有人动了我要改的东西"，不是"现场跟五分钟前一模一样"。与 §13.6 的 netplan 指纹同一口径。

**预检（`firewall.apply`）**

阻塞项（`blockers`）：

| code | 触发条件 |
|---|---|
| `FIREWALL_UNSUPPORTED` | `editable=false`（附 reasons 原文）；或 `ufw status` / `ufw show added` 跑不出来（读不到现状就不许改） |
| `FIREWALL_DEFINITION_INVALID` | 定义越出闭集（id 形状、动作、端口、协议、来源、条数）。**真机上通常看不到它**：代理的参数解码（`Decode`）先跑一遍 `ValidateFirewallDefinition`，形状不合法在那一层就返回 `400 VALIDATION_ERROR` 并点名第几条、哪个字段、合法范围。这条阻塞项是纵深防御——`Assess` 独立再校验一次，只在"解码放行而预检拒绝"时才出现（例如将来放宽解码）。它存在的原因是预检侧不能依赖上游一定校验过 |
| `FIREWALL_CONFIG_CHANGED` | `expected_config_hash` 与现场算出来的不一致 |
| `FIREWALL_ROLLBACK_PENDING` | 已有一条未确认的回滚记录（先确认或先退回，**不许叠加**） |
| `FIREWALL_UNKNOWN_MANAGED_RULE` | 现场有一条带 `upanel:` 标记、但形状认不出来的规则（附原话） |
| `FIREWALL_MANAGEMENT_PATH` | **要启用**，而某个管理端口在改完之后没有 `from any` 的 tcp allow |
| `FIREWALL_MANAGEMENT_UNKNOWN` | 要启用，但一个管理端口都认不出来、且监听表本身没读全（分不开"面板没在听"与"我没看见面板"，只能停下） |

"管理端口"怎么定（`managementPorts`）：

1. 面板自己绑在**非回环**地址上的监听端口。绑在回环上的不需要规则——ufw 只管过网卡的包。
2. SSH。优先按进程名认（`sshd` 前缀），一个都认不出来时**退回到 22/tcp**：
   宁可多要一条放行，也不能少要一条——少要的那条会让人再也连不上，多要的只是浪费一行。

"改完之后有放行"这件事从两处算：管理员手写的（`ufw show added` 里没有 `upanel:` 前缀的
条目，原样保留）+ 这次定义里的。**带面板标记的条目一律交给定义算，不看现场**：
这次可能正把它从 allow 改成 deny、或者整个删掉，拿现场当依据会让预检看不到自己正要
关掉的那个口。

停用（`enabled=false`）时这项检查没有对象，直接跳过——没有任何规则会挡人。

告警项（`warnings`）：没有。这一版里"改完之后会怎样"全部写在 `impacts` 里，不额外制造
告警噪音；介于"一定安全"与"一定不安全"之间的判断，宁可放进阻塞项。

**执行（`firewall.apply`）**

1. 重新算指纹、重读现状（`ufw status verbose numbered` + `ufw show added`）；指纹与
   `ExpectedStateVersion` 都要对上，现场有认不出来的面板规则就停。
2. 写回滚记录（含"改之前的样子"：启用状态 + 规则集）。
3. **武装回滚 timer**。装不上就不许改：这一步失败直接返回，**没有改动任何规则**。
4. 改规则，分三段，顺序不是随意的：
   a. **补**：定义里有、现场没有的；同 id 但形状变了的先删后加（保证读回来只有一条）。
   b. **收**：现场有、定义里没有的（只收带 `upanel:` 标记的）。
   c. **切启用状态**，放在最后：启用是唯一会把规则真的套到包上的动作，前面失败还来得及。
5. **核验**（读回来，不信退出码）：重新跑 `ufw status` 与 `ufw show added`，比启用状态与
   面板管理的规则集与定义一致。等 15 秒（`firewallVerifyWindow`，200ms 轮询），
   短了会在一台正忙的机器上误判，长了让人多等。
6. 核验不符 → **立即回滚**（不等 timer），任务结果 `rolled_back`；回滚本身没走完才是
   `unknown`。

`ufw delete` 有一个必须说清的坑：**删一条不存在的规则时它打印
"Could not delete non-existent rule"，退出码仍然是 0**。所以删除的判据是**输出里那句话**，
不是退出码。拿退出码当判据，"删了但没删掉"会被记成成功，然后回滚时同一条规则会被
"删"第二次、依然是 0，记录里就成了一份干净的成功。

**回滚 timer（独立于面板）**

- 单元：`/etc/systemd/system/upanel-fwrollback@.service` +
  `upanel-fwrollback@<token>.timer`，`OnActiveSec=120s`、`AccuracySec=1s`。
- `ExecStart=` 指向**代理自己的二进制**（apply 时 `os.Executable()` 取到并写进单元与记录），
  子命令 `fwrollback --token <token>`。这个子命令**不读配置、不开库、不碰 socket**：
  它跑在面板完全不在场的时候。
- 记录：`/var/lib/upanel-agent/firewall/rollback/<token>.json`（root 0600，原子写），
  token 前缀 `frb_`（network 那边是 `nrb_`，两者同目录，靠前缀分得清是谁的）。
  记录里含：改之前与想要的规则集与启用状态、到期时刻、执行过的每一步、`state`
  （`pending`/`confirmed`/`rolled_back`/`failed`）。
- 回滚做的事与 apply 对称：补回旧的 → 删掉多的 → 恢复启用状态 → 读回比对。
  任何一步失败写 `state=failed` + 原因，**不重试**（重试会把一个说不清的状态变成两个）。
- 确认（`firewall.confirm`）＝ 停 timer + 删 timer 单元 + 记录 `state=confirmed`
  + `daemon-reload`。
- **开机重放**：代理启动时扫 `state=pending` 的记录——已过期立刻回滚，没到就按剩余时间
  重新武装 timer。ufw 的规则是开机就在的（不像 netplan 那样"没 apply 就不生效"），
  所以这一步同样不能省：漏掉它，那条 pending 记录就再也没有机会自己退回去了。

`daemon-reload` 会把 `OnActiveSec` 重新起算，所以在"重新武装"这条路上，timer 的时长是
按**剩余时间**算的（记录里的 `Deadline` 减现在），不是又给满 120 秒。

**确认不需要换地址（与 §13.6 的差别）**

`firewall.confirm` 的参数只有 `token`，**没有 `arrived_on`**。这不是漏了：
§13.6 那个字段存在的理由是"改完地址后旧地址还能连上、确认必须来自新地址"；防火墙改动
**不允许**把面板挡在外面（`FIREWALL_MANAGEMENT_PATH` 就是这道门），能发出这条请求，
本身就说明管理通路还在。多一个用不上的门槛，只在出事后让人多点一次。

**拒绝分支必须带规范化参数**

三个 `Assess`（`apply` / `confirm` / `rollback`）的**每一条**返回路径都要返回非 nil 的
`Normalized`，包括每一条拒绝。理由是一条会把"说人话的拒绝"变成故障的链：代理在回响应前
跑 `validatePlanResult`，它见到 `NormalizedParams == nil` 就判**"预检结果不合法"**，于是
`POST /operations/preflight` 返回 `500 INTERNAL_ERROR`。用户该看到的是
"没有待确认的防火墙改动（已经确认过、已经退回去、或者根本没改过）"，实际看到的是
"服务内部错误"，还会诱使人去翻日志找一个不存在的 bug。

这不是假想：真机 P14.3 验收第一轮，`firewall.confirm` / `firewall.rollback` 在没有 pending
记录时的 8 条断言全部因此变红（500）。修法是在函数第一行就设 `Normalized`（§13.6 的
`AssessConfirm` 本来就是这么写的，防火墙这一版漏了）；回归测试
`TestFirewallAssessRefusalsKeepNormalizedParams` 与 `TestFirewallTokenMismatchKeepsNormalizedParams`
分别钉住"无记录"与"token 不符"两个拒绝分支。

**动作策略**

| 动作 | 再认证 | 确认短语 | 可取消 | 资源锁 |
|---|---|---|---|---|
| `firewall.apply` | 是 | 是（`RECONFIGURE XXXXXX`；改了什么写在计划的显示名与影响列表里） | 否 | `firewall-global` |
| `firewall.confirm` | 否 | 否 | 否 | `firewall-global` |
| `firewall.rollback` | 否 | 否 | 否 | `firewall-global` |

`firewall-global` 与 `network-global` 是**两把锁**：改的是不同的东西，同时做不会互相
掩盖，但都同样"改完可能让人失去面板"，所以各自串行。

**审计**

三个动作都记；`firewall.apply` 与 `firewall.rollback` 记改之前与之后的规则集。
timer 自己执行的那一次由**代理**记 `agent.firewall.rollback`（面板不在场），Web 恢复后
从 `GET /firewall/config` 的 `pending` 与记录状态读到并显示出来。

**未接线时**：`-firewall-writes` 是**独立开关**，与 `-network-writes` 分开——两个领域
的写路径各有各的风险，一个开一个不开是正常配置。未接线时不登记这三个动作，
`capabilities.supported_queries` 也不报告 `firewall.config`；Web 侧 `GET /firewall/config`
回 503 `AGENT_UNAVAILABLE`，页面上那张卡改说"读不到写路径的入口状态"，
**不给一份看起来能用的空规则集**（空规则集在这里是一个**有含义的输入**：它等于
"清空面板管理的规则"，绝不能当作"读不到"的默认值）。


### 13.8 P14.5 已实现范围：受管理 Linux 账号、SSH 公钥与受限 sudo 模板

P14.5 把 §13 开头那两行（`GET /accounts`、`GET /accounts/:id/ssh-keys`）与 `AccountDefinition`
落成实现。它管的是**普通 Linux 账号**；`type=share` 的共享账号仍走 P08 的
`share.account.apply`（凭据在 Samba 的 passdb 里），`account.apply` 收到 `type=share`
会直接拒绝并指向那一条。

**为什么共用一张表、一套判决形状。** 普通账号与共享账号落在同一张 `managed_accounts`
（DECISIONS Q475），区别只有 `type` 一列。所以 `internal/accounts` 复用 `internal/shares`
的 `Assessment` / `Blocker` / `Impact` / `WriteRequest` / `WriteReport` 与
`Outcome` 闭集，`*agentstore.ShareStore` 同时满足两个消费方接口，代理里那对
`sharesPlanner` / `sharesExecutor` 适配器原样用于账号动作。这不是省事：
两套形状会长出两个适配器，两个适配器会长出两条读回核验。

| 方法与路径 | 返回 | 说明 |
|---|---|---|
| `GET /accounts` | `{capability, accounts}` | admin、只读。**记录**与 `GET /share-accounts` 同源（同一张表），但 `capability` 是账号自己的那一份（见下）；旧路由保留是因为共享页已经在用 |
| `GET /accounts/:id/ssh-keys` | `{account_id, ssh_keys}` | admin、只读。读的是登记表 `managed_ssh_keys`，**不是**此刻的 `authorized_keys`；`Blob` 在 DTO 上是 `json:"-"`，密钥体不出代理 |

六个动作都不开专门路由：一律 `POST /operations/preflight` + `POST /jobs`。

| 动作 | 参数 | 策略 | 落盘与读回 |
|---|---|---|---|
| `account.apply` | `account_id`（新建 null）、`version`、`definition` | 再认证；短语否；可取消否 | `useradd --create-home --shell <档> --user-group` → `getent` 读回 uid/shell/home → `chpasswd`（口令只经 stdin）→ `passwd -S` 读回 `P` |
| `account.delete` | `account_id`、`version`（**必填**） | 同上 | 先删 sudoers 文件与记录，再 `userdel`（**不加 `-r`**）并读回消失，最后删记录；报告 `home_data_kept` |
| `ssh_key.apply` | `account_id`、`version`、`key_id`（替换时给）、`public_key` | 同上 | 重写托管区 → `rename` → `fsync` 目录 → 整份重读比对指纹集合与托管区外逐字节不变 |
| `ssh_key.delete` | `account_id`、`version`（必填）、`key_id` | 同上 | 同上 |
| `sudo_rule.apply` | `account_id`、`version`、`template_id` | 同上 | 候选写入临时文件 → `visudo -c -f 候选` → `rename` → `visudo -c` → `ParseSudoers` 读回 → `sudo -l -U` 读回逐条比对 |
| `sudo_rule.delete` | `account_id`、`version`（必填） | 同上 | 删文件 → `visudo -c` → 读回确认 |

三个删除动作的 `version` 是必填而不是可选：删的东西已经存在，没有版本号就没有并发
校验，两次点击之间别人换了壳或换了口径，第二次点击照样会把新状态一起抹掉。apply 类
的 `version` 可空（目标可以还不存在），但目标存在却没给时不会静默通过——判决层把当前
版本放进 `StateVersion`，执行前复核以版本冲突拒绝。

**两份能力，不是一个。**

`GET /accounts` 的 `capability` 判的是 `useradd` / `usermod` / `userdel` / `getent` /
`passwd` / `chpasswd` 在不在，外加两档壳与 `visudo`；`GET /shares` 的 `capability`
判的是 `smbd` / `smbpasswd` / `pdbedit` / `setfacl` / `getfacl` 与 `smb.conf`。两个工具集
**不相交**，混用会把“没装 Samba”误判成“不能管账号”——一台标准 Ubuntu 服务器上
就是这个后果：账号页显示不支持、新建按钮是灰的，而建一个普通账号一个 Samba 工具
都不需要。这条是虚拟机验收打出来的，能力因此拆成两个对象——与
`internal/shares/capability.go` 里给 NFS 单独开一份判定用的是同一条理由：把两个
彼此独立的工具集塞进一个 status，缺一个就把另一个也判死。账号那份的形状：

| 字段 | 取值 | 说明 |
|---|---|---|
| `status` | `available` / `blocked` | 缺必需工具或两档壳全缺才是 blocked |
| `reason` | 文本 | blocked 时点名缺什么，页面原样显示 |
| `tools` | 对象 | 探到的绝对路径（缺的项不出现）：useradd…chpasswd、visudo、restricted_shell、disabled_shell |
| `sudo_rule_write` | `available` / `unsupported` | 判据只有 visudo 在不在；缺它不阻塞账号本身 |
| `shell_profiles` | 数组 | 这台机器上真的能落的壳档（`disabled` / `restricted` 的子集） |
| `notices` | 数组 | 缺一档壳、缺 visudo 这类不改变 status 的说明 |
| `collected_at` | 时间 | 采集时间；探针缓存 30 秒，与共享能力同一窗口 |

**`AccountDefinition`**

| 字段 | 取值 | 约束 |
|---|---|---|
| `name` | 小写字母/数字/`-`/`_` | 保留名闭集（`root`、`sudo`、`upanel-agent`、`upanel-web`、`upanel-shell` …）一律拒绝 |
| `display_name` | 文本 | 只是标签，不参与任何系统调用 |
| `type` | `regular` / `share` | `share` 在本动作里只用于指向 `share.account.apply` |
| `disabled` | 布尔 | 见下 |
| `password_secret_ref` | `sec_` 引用或空 | 只接受引用，不接受明文 |
| `shell_profile` | `disabled` / `restricted` | 落盘是 `/usr/sbin/nologin` / `/bin/rbash`，**不是**任意 shell 路径 |

没有 `uid`、`gid`、`groups`、`home`：这四项由系统分配，面板只读回、只展示、只据此
对账。能提交它们就等于能提交 `uid=0`。

线上字段名就是上表那六个（`name`、`display_name`、`type`、`disabled`、
`password_secret_ref`、`shell_profile`），代理解参数时用 `DisallowUnknownFields`：
未知字段与上面那四项一样，一律 400 `VALIDATION_ERROR`，不静默忽略。

**禁用用换壳，不用 `usermod -L`。** `-U` 对从未设过口令的账号会写一个**空口令**，
而空口令比 `nologin` 危险得多；换壳的结果还能用 `getent` 一眼读回。所以 `disabled`
落成 shell 变成 `nologin`，而 `shell_profile` 那一列记的是**期望**——禁用之后再启用
回到的是那一档，不是一句“原样”。

**UID 区间**：普通账号 `1000–59999`（Debian/Ubuntu 的 `FIRST_UID` 与 adduser 常规
上沿）。区间外拒绝，不是因为 60000 之后不能用，而是面板解释不了“为什么这个账号的
uid 和别人不一样”。共享账号走 `--system`，落在下限以下，同一张表里两种类型靠这一条
就能分开验证。

**`authorized_keys` 的托管区**

`authorized_keys` 是人和面板共用的文件，全份重写会吃掉人手写的行。所以面板只重写
自己那一块，两块标记之间：

```
# >>> upanel 托管区（整块由面板重写，手改会被覆盖）>>>
# <<< upanel 托管区 <<<
```

`>>>` / `<<<` 不是 `authorized_keys` 的合法指令，也不像任何一条正常钥匙的开头，所以
“标记被当成钥匙读进去”不可能发生。写完之后**整份重读**：托管区里的指纹集合必须等于
登记的集合，托管区**之外**必须逐字节不变——后者是“没吃掉别人的行”的唯一证据。
`~/.ssh` 是 `0700`、`authorized_keys` 是 `0600`；home 必须在 `/home` 之下，且 home 与
`~/.ssh` 都不能是符号链接（否则面板在往一个被人指到别处的目录写钥匙）。

**sudo 模板是闭集，而且不带参数**

| 模板 | 允许的命令（逐条一次 sudoers 行） |
|---|---|
| `system_status` | `/usr/bin/systemctl status --no-pager` |
| `journal_recent` | `/usr/bin/journalctl -n 200 --no-pager` |

两条都是只读。这一版**故意**不提供启停服务的 sudo：那会绕过面板自己的服务动作
（保护名单、依赖检查、审计摘要）。模板里**没有通配符**，也**不给单元名参数**：sudoers
的 `*` 按单参数 glob 匹配，`journalctl -u *` 会连 `--vacuum-size` 一起放行，那样一条
“看最近日志”就会变成“能清日志”。

**双门禁，失败保留旧规则。** 候选内容先写临时文件，`visudo -c -f 候选` 通过才
`rename` 到正式路径；然后整份 `visudo -c`、再 `ParseSudoers` 读回、再 `sudo -l -U`
把 sudo 自己解析出来的授权逐条比对模板。任何一步失败都恢复旧文件——**visudo 失败时
正式文件根本没被动过**，这就是“失败保留旧规则”的机制：不是“先写坏的再回滚”，而是
“坏的根本上不去”。

**`/etc/sudoers.d` 里属于面板的文件只有一个前缀 `upanel-`**，文件名由账号名拼成。
目录里出现同名但**没有认领标记**的文件时判 `SUDO_FILE_DAMAGED` 并停手——那可能是
别人手写的规则，面板不替人做决定。

**阻塞码与告警码**

阻塞（拒绝执行）：`ACCOUNT_NAME_INVALID`、`ACCOUNT_RESERVED`、`ACCOUNT_TYPE_INVALID`、
`ACCOUNT_SHELL_PROFILE_INVALID`、`ACCOUNT_DISPLAY_NAME_INVALID`、`ACCOUNT_SECRET_INVALID`、
`ACCOUNT_NAME_TAKEN`（系统里已有同名账号，**拒绝接管**）、`ACCOUNT_NOT_MANAGED`、
`ACCOUNT_MISSING`、`ACCOUNT_UID_OUT_OF_RANGE`、`ACCOUNT_HAS_SHARES`、
`ACCOUNT_LOOKUP_TIMEOUT`、`SSH_KEY_INVALID`、`SSH_KEY_DUPLICATE`、`SSH_KEY_TOO_MANY`
（每账号 10 把）、`SSH_KEY_NOT_FOUND`、`SSH_KEY_HOME_INVALID`、`SSH_KEY_BLOCK_DAMAGED`
（托管区标记不成对或重复）、`SUDO_TEMPLATE_INVALID`、`SUDO_ACCOUNT_NOT_ELIGIBLE`
（保留名、共享账号或已禁用）、`SUDO_RULE_MISSING`、`SUDO_FILE_DAMAGED`、
`SUDO_VISUDO_UNAVAILABLE`、`SUDO_VISUDO_FAILED`、`ACCOUNTS_UNSUPPORTED`（缺
`useradd`/`chpasswd`/`visudo`/`sudo` 一整套工具，503）。

告警（不拦，但要看见）：`ACCOUNT_NO_PASSWORD`——账号没有口令。**这不等于“登不上”**：
publickey 认证不看 shadow 的口令字段，真机上 key-only 账号（没有任何口令、只配了公钥）
照样能登（P14.5 真机读数，见 DECISIONS Q479）。要拦住一个账号只有一条路——`disabled`
档把壳换成 `/usr/sbin/nologin`；口令只决定“密码登录”这一种方式能不能用。页面文案因此
说的是“只能用已配的公钥登录”，不是“登不上”。

**审计**：六个动作都记，`account.apply` 的记录里**不含口令**（只有 `secret_ref` 是否
存在这一事实），`sudo_rule.*` 记模板标识与最终文件哈希，`ssh_key.*` 记指纹。

**未接线时**：`ProductionDeps.Accounts` 为 nil 时不登记 `accounts.ssh_keys` 与六个
动作，`capabilities` 里也不会出现它们；Web 侧 `GET /accounts` 与
`GET /accounts/:id/ssh-keys` 回 503 `AGENT_UNAVAILABLE`，**不给一份空账号列表**——
空列表会被读成“这台机器上一个账号都没有”。账号与共享是**两个开关**：只装了 Samba
的机器不该被登记成“能建账号”。

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
| proxies | id、domain UNIQUE、app_id FK 可空、upstream_target_id/scheme/port、access_mode、allowed_cidrs_json、basic_auth_json、websocket_enabled、max_body_bytes、read_timeout_seconds、enabled（**意图**，不是已生效）、version；P10.1 的落地形态与本节的关系见 §9.1 与 DECISIONS Q281（`definition_json` 被展平成能下 CHECK 的列）。P10.2 **仍然没有** `applied_config_hash` 这一列：落地状态由“此刻渲染的字节”与“树上那份文件的字节”比对得到，见 §9.2（7）与 DECISIONS Q286 |
| certificates | id、proxy_id FK、domains_json、expiry、agent_certificate_ref、renewal_state |
| deployments | id、name UNIQUE、definition_json、applied_digest、agent_project_ref、version |
| file_roots | id、volume_id FK 可空、kind、agent_root_ref、display_name、read_only |
| backup_repositories | id、name UNIQUE、definition_json、agent_secret_refs；无秘密明文 |
| backup_policies | id、repository_id FK、definition_json、last_verified_at |
| schedules | id、name UNIQUE、definition_json、agent_timer_ref、enabled |
| scripts | id、name、agent_script_ref、content_hash、version |
| metric_samples | scope、target_id（默认空串）、metric、resolution=raw/minute/hour、ts（**INTEGER Unix 秒，向下对齐到本粒度桶边界**）、value、min_value、max_value、sample_count；主键 `(scope,target_id,metric,resolution,ts)` 且 `WITHOUT ROWID`；`CHECK(ts > 0)`、`CHECK(min_value <= value AND value <= max_value)`；索引 `(resolution, ts)` 供裁剪；见 §13.1 |
| alert_rules | id、definition_json、enabled、version |
| alert_events | id、rule_id FK、state、first_seen、last_seen、resolved_at、dedup_key |
| notification_channels | id、type、settings_json、secret_refs、enabled |
| settings | key PRIMARY KEY、value_json、version；不能存 root 秘密 |

**本机恢复入口（P14.6）**：`user_totp` 有两类写入者。Web 是 `POST /auth/totp/setup`、
`POST /auth/totp/confirm`、`DELETE /auth/totp` 三条端点，全部要求 admin + 再认证 + CSRF，
且 `DELETE` 还要口令加一种第二因子。另一类是**本机 CLI**：`upanelctl totp-reset
--username <name> [--config PATH] [--db PATH] [--socket PATH] [--no-agent] [--json]`，
用于"既是唯一管理员、又同时丢了 authenticator 与十枚恢复码"这一种情形——那时 Web 上
没有任何合法入口。它清掉状态行与全部恢复码、作废该账号**全部**会话（认证强度变了）、
写一条 `auth.totp.reset` 审计（actor 是 `local-cli:<who>` 快照，`actor_id` 为 NULL：
没有人证明过自己是谁），并尽力让代理销毁种子。代理销毁这一步**不阻断恢复**：面板库里
已经没有任何人引用那条种子，登录不会再校验它；但输出必须如实报 `agent_status`
（`destroyed`/`already_gone`/`unreachable`/`forbidden`/`skipped`），不假装清干净了。
SO_PEERCRED 规则只认 `upanel-web`，所以以 root 运行会拿到 `forbidden`——这是设计，不是缺陷：
要连代理那半边一起清，就以 `upanel-web` 身份运行。`reset-password` 与 `totp-reset` 的
授权都是"能写 upanel.db"这把钥匙，不新增特权通道。

复杂 definition_json 必须由严格 DTO 校验并包含 schema_version=1；JSON 不意味着允许任意字段。外键使用 RESTRICT 为默认，只有会话/收藏等明确从属对象可 CASCADE；删除卷必须检查全部依赖。

### 14.2 代理数据库与秘密

代理数据库独立由 root 读写，不能让 Web 直接编辑。表至少包括：operations（operation_id UNIQUE、request_hash、动作、阶段、结果）、operation_events、preflight_plans（参数/设备指纹/到期/使用状态）、managed_resources（主机实际接管清单）、config_backups、secrets（密文、用途、引用、时间）、pending_network_rollbacks。P14.2 把 `pending_network_rollbacks` 落成**记录文件**而不是库表：`/var/lib/upanel-agent/network/rollback/<token>.json`（root 0600）。理由是这张表唯一的读者是回滚 timer 自己——它跑在面板完全不在场的时候（Web 停了、库可能都打不开），读一个自足的文件比开库可靠；`netrollback` 子命令因此不碰数据库、不碰 socket。P14.3 的防火墙回滚记录同理由落在
`/var/lib/upanel-agent/firewall/rollback/<token>.json`，由 `fwrollback` 子命令读（见 §13.7）。

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
| GET /internal/v1/operations?actor=scheduler&since=&since_id=&limit= | P12.4 起：只列系统操作者提交的定时操作，供 Web 对账导入；`actor` 只认 `scheduler`，`(since, since_id)` 为创建时间与操作 ID 的复合游标，`limit` 有上限 |
| POST /internal/v1/secrets | 用途与秘密；返回引用，无读取明文路由 |
| POST /internal/v1/schedules/:id/trigger | 仅 SO_PEERCRED UID=0 的本机 CLI 可调用；从代理已批准配置取动作，输入只允许触发 ID，不接受 action/params；答复带 `max_runtime_seconds` |
| GET /internal/v1/schedules/:id/trigger-runs/:operation_id | 同样是 UID=0 专用；只返回**这条计划自己触发的那个操作**，其它一律 404。`--wait` 的状态来源 |
| POST /internal/v1/streams | 创建受限日志/文件上传下载/终端流许可，绑定用途和参数 |
| GET/PUT /internal/v1/streams/:id | 单次受限流；再次核验 socket 凭据、路径/UID/大小限制 |
| GET/POST /internal/v1/backup/repositories | 备份仓库定义的代理镜像（P12.1）：只同步元数据，不传任何明文 |
| GET/PATCH/DELETE /internal/v1/backup/repositories/:id | 单条镜像；PATCH 带 version 做乐观并发，DELETE 只删定义、不删数据 |
| POST /internal/v1/totp/enroll | P14.6：代理生成种子（用途 `totp_seed`，**临时秘密**，10 分钟自毁），回 `secret_ref` 与一次性 `otpauth_uri` |
| POST /internal/v1/totp/confirm | 校验一次码；通过后把临时种子转为持久秘密并挂引用 `totp:<user_id>` |
| POST /internal/v1/totp/verify | 核对归属后验码，返回匹配到的时间步（供 Web 做重放防护） |
| POST /internal/v1/totp/disable | 销毁种子（清空密文、留墓碑）；幂等 |

query 枚举按模块登记，例如 system.summary、storage.topology、services.list、files.list、logs.query；P10.1 起增加 nginx.state（参数 `{fresh}`，见 §9.1）；P11.1 起增加 apt.packages（参数 `{fresh}`）与 system.power_state（空参数，见 §12.1）；P12.1 起增加 backup.repository.state（参数 `{repository_id}`，见 §11 的仓库登记小节），P12.2 起增加 backup.snapshots（`{repository_id, policy_id?}`）与 backup.snapshot（`{repository_id, snapshot_id}`，见 §12.2）；P14.5 起增加 accounts.ssh_keys（`{account_id}`，见 §13.8）与 accounts.capability（空参数，见 §13.8）——账号列表不另开查询，它读的就是 share.accounts；禁止 query=command。terminal 的字节通道只能连接已降权 PTY，不能进入代理的命令解释器。P14 增加 /internal/v1/totp/enroll、/confirm、/verify、/disable 四个专用接口，操作绑定用户与用途，种子不进入普通 query/secret 读取通道。

**二次验证的四个接口（P14.6）。** 种子的明文从头到尾只在代理进程里存在：Web 调
`enroll` 拿回 `secret_ref` 与一条 `otpauth://` URI（给二维码用，除了它没有第二个地方能
拿到种子），此后所有校验都由代理解密种子来算。三个细节是硬的：

- **未确认的登记是临时秘密**：`enroll` 不挂引用，种子按 §8 的临时秘密规则 10 分钟后自毁，
  `confirm` 通过才 `Reference` 成持久秘密，引用形如 `totp:<user_id>`。"TOTP 未确认不启用"
  因此不只靠 Web 的 `enabled` 字段——代理侧这条种子在被确认之前根本没有归属，`verify`
  会以 403 拒绝它。
- **归属单独核对**：任何一条 `totp_seed` 都解得开，所以"能解密"证明不了"这条种子是你的"。
  拿 3 号用户的 `secret_ref` 去验 5 号用户的码，代理在解密之前就用 `Binding`（只读用途与
  引用，不解密）挡住。用途不是 `totp_seed` 的引用一律 400：拿一张 TLS 私钥的引用冒充种子
  必须在这一层失败，而不是在"解出来不像码"时失败。
- **码不对是结果，不是错误**：HTTP 200 + `ok:false` + 闭集 `reason`（`code_mismatch`），
  因为调用方要的是"这次尝试失败了"这个事实。请求本身不成立（参数畸形、用途不对、种子不
  存在）才用 4xx。`verify`/`confirm` 成功的响应带 `step`（匹配到的 30 秒时间步），Web 用它
  拒绝重放：同一个窗口里同一个码不许用第二次。

`disable` **幂等**：种子已经不在了（第一次关闭、或临时登记到期自毁）同样回 200，只是
`destroyed:false`。理由两条——Web 的重试不该因为第一次其实成功而报错；"不存在"与"存在
但已销毁"给出不同响应，本身就是一个存在性探针。没接线时四条路由都是 404（不是"在、但
一律拒绝"），root 也拿不到它们（只有 `upanel-web` 能调），种子的生成与校验没有 root 旁路。

**备份仓库定义的镜像（P12.1）。** 预检计划里的 `normalized_params` 是冻结的，只含 `repository_id` 与 `version`；执行时再向 Web 要一遍定义就等于让“被冻结的那个目标”可替换，所以代理自己持有一份仓库定义的镜像，预检与执行都从镜像里取。镜像只存元数据（名称、后端、位置、两份秘密的**引用**），不存也不接受任何明文；秘密仍只经 `POST /internal/v1/secrets` 进入代理的加密存储。这条路由的存在与“这台机器能不能初始化仓库”是两件事：镜像没接线时路由不存在（404），动作没接线时能力表里也不会出现 `backup.repository.initialize`，装配不谎报能力。路由只接受 `upanel-web` 的对端凭据，root 也会被拒——root 直接改镜像等于绕过 Web 的再认证与审计。

`/internal/v1/streams` 自 P05.3 起已实现，但目前只登记一种用途 **logs.stream**：`POST` 建许可（绑定用途与查询参数、单次使用、30 秒过期且到期即回收），`GET /streams/:id` 打开流式响应（`application/x-ndjson`，每行一帧 `{"type":"entry|end|error", ...}`；`entry` 帧的 `entry` 是日志条目本身，`end` 帧带 `reason`，`error` 帧带 `code`/`message`）。未登记的用途回 501，不回一条空流——多报一个用途等于把能力清单变成谎话。许可只用一次：第二次 `GET` 回 404，被放进地址栏或日志里的 URL 因此最多只能被用一次。流式消费端的「帧类型来自事件名」与外层 SSE 不同，是因为 NDJSON 是单条无名字的字节流，类型必须写在帧里。

**`backup.repository.state` 的三态（P12.1）。** 返回里的 `initialized` 是 `true`/`false`/`null` 三者之一：`true`/`false` 是**这次真的探测到了**的事实，`null` 是**没探测到**（SFTP 连不上或主机密钥没核对通过、文件根不可用、秘密取不到、restic 不存在）。原因分别写在 `host_key_error` / `probe_error` / `repo_config_error` / `restic_missing` 里，四个字段各有归属，不许把原因折成一句 `initialized:false`。同一原则早已用在 `capacity.supported` 上（读不到容量不是 0 字节）：**“查不了”与“没有”在界面上必须能分开**，否则管理员会把一次网络超时读成“仓库还没建”。`repo_id` 只在 `initialized:true` 且 `restic cat config` 读成功时出现；口令错时 `initialized:true`（仓库确实在）而 `repo_id` 为空、`repo_config_error` 指向口令——那是**事实 + 原因**，不是“空仓库”。

内置定时任务只可执行已登记且启用的 backup.run、certificate.renew 和 script.run。systemd 调用 root 所有的固定 upanelctl schedule-trigger 命令，代理只在专用 trigger 路由接受 UID=0；普通路由仍只接受 upanel-web。系统身份来自 root 持有的任务配置与本地凭据，不允许公开 /jobs 请求自称 scheduler 来跳过再认证。定时触发须重新预检资源与锁，创建稳定触发 ID 并审计；交互确认由管理员保存计划时的显式授权替代，不能扩展到格式化或网络变更。代理产生的定时 operation 会被 Web 对账导入任务表，不能只存在于 journal。

导入用的正是 `GET /internal/v1/operations?actor=scheduler`（P12.4）：`actor` 不是身份断言，参数只筛出代理库里 `user_id` 等于保留系统操作者的那些操作；谁能调这条路由仍由 socket 对端凭据决定（只接受 `upanel-web`）。导入按 `(created_at, operation_id)` 复合游标增量拉取，`jobs` 行的来历因此可追：`preflight_id` 抄代理给的引用，`cancellable=0`、`needs_sync=1`（见 §12.4 第 9 条）。

actor_context 供审计关联，包括 user_id、session_id_hash、request_id、reauth_at。代理信任 Web 完成用户登录认证，但仍独立实施系统安全边界；不能把该设计描述为可抵御已完全控制 Web 进程的管理员身份伪造。

锁统一由代理维护。磁盘锁沿拓扑追踪到所有底层盘，多个资源按规范字符串排序后获取，避免死锁。进程内锁不足以协调外部 apt/配置编辑，还必须检测系统锁和配置哈希。所有外部命令有运行时限、输出限额和退出状态处理。

## 16. 契约验证要求

P00 建立 OpenAPI 文件骨架，后续每阶段先补齐对应 schema、枚举、权限、错误、请求示例再实现。不要在阶段未实现时生成“已支持”的 API 文档。

每个变更验证前后端类型、未知字段拒绝、枚举和状态机一致。关键契约至少覆盖：容量大整数、viewer 越权、CSRF、幂等冲突、预检过期、设备变化、文件版本冲突、任务重连、代理离线、非零退出、进程重启对账。

本文没有给出外围功能的所有 SQL 列与 UI 文案，但已固定其动作、参数、范围和实现次序。实施模型可以补充内部细节；不能借此新增产品能力、改名公开字段或取消检查。
