package shares

// SMB 共享与受管理共享账号。
//
// P08.1 做了“定义与准备”这一半：Samba 能力检测（只读探针，不安装、不改配置）、
// 受管理共享账号的生命周期（Linux 账号 + Samba passdb）、密码秘密通道的**消费端**
// （参数里只有 secret_ref，明文经标准输入交给 smbpasswd）、共享定义与配置版本、
// 目录准备与 POSIX ACL。
//
// P08.2 补上“配置写入”这一半：托管 include 目录与文件、最小改动地给 smb.conf
// 接一行 include、testparm 校验、原子替换（带备份）、smbcontrol 重载、连接状态。
//
// 仍然**不做**（属于 P08.3/P08.4，见 docs/STAGES.md 第 10 节）：
//   - 共享与 volume_id 的启动依赖、运行时脱盘撤销、卸载/格式化依赖阻断；
//   - 共享 UI 与客户端连接说明。
//
// 诚实性仍然是这一包最要紧的属性：`served` 只在“配置已就位 + include 在 + smbd
// 在运行 + 重载被接受”时才是 true；smbd 没运行时执行结果只能是 accepted。
// “配置写好了”与“客户端连得上”是两件事，返回结构里分别有字段，谁也不能替另一个说话。

import (
	"context"
	"errors"
	"time"
)

// 公开动作名（CONTRACTS.md 第 5 节）。名字是契约的一部分，不允许别名。
const (
	ActionShareApply    = "share.apply"
	ActionShareDisable  = "share.disable"
	ActionShareDelete   = "share.delete"
	ActionAccountApply  = "share.account.apply"
	ActionAccountDelete = "share.account.delete"
)

// 只读查询名。
//
// 三条查询与三条 Web 路由一一对应：能力、共享列表、账号列表。共享详情由列表里取，
// 不单独开一条查询——列表本来就是全量返回（共享数量是几十的量级，不是几万）。
const (
	QueryCapability = "share.capability"
	QueryShares     = "share.list"
	QueryAccounts   = "share.accounts"
	// QueryConnections 是 P08.2 新增的连接状态查询（按共享名过滤 smbstatus 的树连接）。
	QueryConnections = "share.connections"
)

// 协议闭集。P08 只实现 smb；nfs 在定义里认得出来，但判决层会以
// PROTOCOL_UNSUPPORTED 拒绝，而不是假装能导出。
const (
	ProtocolSMB = "smb"
	ProtocolNFS = "nfs"
)

// 访问级别闭集。read 渲染成 ACL 的 r-x，write 渲染成 rwx。
const (
	AccessRead  = "read"
	AccessWrite = "write"
)

// 账号类型。P08.1 只创建 share 类型；regular 留给 P09 的普通账号模块。
const (
	AccountTypeShare   = "share"
	AccountTypeRegular = "regular"
)

// 能力状态闭集。
//
// 与设备动作的 available/blocked/unsupported 三态不同，这里只有两态：Samba 要么齐了，
// 要么缺东西（缺什么写在 reason 里）。没有“本机不支持 SMB”这种状态——Linux 上装的
// Samba 就是 Samba，缺包是缺包。
const (
	CapabilityAvailable = "available"
	CapabilityBlocked   = "blocked"
)

// 配置写入状态闭集。P08.1 恒为 unsupported（配置写入与重载在 P08.2）。
const (
	ConfigWriteUnsupported = "unsupported"
	ConfigWriteAvailable   = "available"
)

// 阻塞码与告警码是闭集：测试按码断言，message 才给人读。
//
// 一条阻塞项 = 现在不能做，并且原因不是“等一下就好”；一条告警 = 能做，但用户
// 需要知道这件事。两者都不允许出现没有 message 的条目。
const (
	// NoticeShareNotFound 表示登记表里没有这条共享。
	NoticeShareNotFound = "SHARE_NOT_FOUND"
	// NoticeShareVersionMismatch 表示调用方手上的版本已经不是现在这一版。
	NoticeShareVersionMismatch = "SHARE_VERSION_MISMATCH"
	// NoticeShareNameTaken 表示同名共享已存在（禁止靠“名字相同”覆盖）。
	NoticeShareNameTaken = "SHARE_NAME_TAKEN"
	// NoticeVolumeNotFound 表示定义里的 volume_id 不在受管理卷里。
	NoticeVolumeNotFound = "VOLUME_NOT_FOUND"
	// NoticeVolumeNotMounted 表示卷没挂载。此时共享路径会落到系统盘的空目录上，
	// 这是 PLAN.md 第 10 节明令禁止的状态，必须硬拒。
	NoticeVolumeNotMounted = "VOLUME_NOT_MOUNTED"
	// NoticeVolumeUUIDMismatch 表示卷挂着的不是登记的那个文件系统。
	NoticeVolumeUUIDMismatch = "VOLUME_UUID_MISMATCH"
	// NoticeSharePathInvalid 表示 relative_path 形状不合法（绝对路径、..、空段）。
	NoticeSharePathInvalid = "SHARE_PATH_INVALID"
	// NoticeSharePathEscapes 表示规范化后的真实路径不在卷挂载点里面
	// （含符号链接穿出去的情形）。
	NoticeSharePathEscapes = "SHARE_PATH_ESCAPES_VOLUME"
	// NoticeSharePathSymlink 表示路径上有符号链接。初期一律拒绝跟随（PLAN.md 第 10 节）。
	NoticeSharePathSymlink = "SHARE_PATH_SYMLINK"
	// NoticeSharePathNotDirectory 表示目标存在但不是目录。
	NoticeSharePathNotDirectory = "SHARE_PATH_NOT_DIRECTORY"
	// NoticeSharePathNotTraversable 是告警：共享路径上有一级上级目录没有 other 的 x 位，
	// 共享账号进不去。应用时会给它补上 x（只加穿越，不加读取与列出）。
	//
	// 为什么是告警不是阻塞：这些目录是面板自己建的，挡住的是面板自己创建的账号，
	// 属于面板该修的东西；拒绝执行只会把修复推迟到下一次。
	NoticeSharePathNotTraversable = "SHARE_PATH_NOT_TRAVERSABLE"
	// NoticeHostsAllowLoopback 是告警：allowed_cidrs 里没有 127.0.0.0/8，但 Samba 对
	// 本机（127.0.0.1）永远放行，这份名单管不住本机连进来的访问。
	NoticeHostsAllowLoopback = "SHARE_HOSTS_ALLOW_LOOPBACK"
	// NoticeVolumeOffline 是告警：这条共享绑定的卷此刻没挂着，因此它**这一次不会**
	// 被渲染进托管配置。
	//
	// 为什么是跳过而不是照写：卷没挂时挂载点只是系统盘上的一个空目录，把段留在
	// 配置里等于把根分区的一块空目录共享出去（PLAN.md 第 10 节）。跳过是唯一安全的
	// 方向——客户端看到“共享不存在”，而不是看到一个空目录以为数据没了。
	NoticeVolumeOffline = "SHARE_VOLUME_OFFLINE"
	// NoticeVolumeStateUnknown 是告警：问不出卷挂没挂。
	//
	// 与 NoticeVolumeOffline 的处理一致（同样跳过），因为“不知道挂着没有”不能当作
	// “挂着”。区别只在说法：这条说的是观测失败，那条说的是观测到的结果。
	NoticeVolumeStateUnknown = "SHARE_VOLUME_STATE_UNKNOWN"
	// NoticeStartupDepFailed 是告警：systemd 开机依赖没写成功或没生效。
	//
	// 配置本身照样是对的，缺的只是“开机时 smbd 等挂载点就位再启动”这一层保护。
	// 说成阻塞会让人以为共享不能用；说成通过会让人以为重启顺序已经被保证——两者都不对。
	NoticeStartupDepFailed = "STARTUP_DEP_NOT_APPLIED"
	// NoticeShareRevokedOffline 是告警：这条共享在运行时因为卷离线被撤销了段。
	// 它不会自动恢复：卷回来之后需要重新应用一次（写配置是要人点头的动作）。
	NoticeShareRevokedOffline = "SHARE_REVOKED_VOLUME_OFFLINE"
	// NoticeShareNoUsers 表示 smb_users 为空。空名单会退化成 guest，必须硬拒。
	NoticeShareNoUsers = "SHARE_NO_USERS"
	// NoticeAccountMissing 表示定义引用了一个不存在的账号。
	NoticeAccountMissing = "ACCOUNT_MISSING"
	// NoticeAccountNotManaged 表示同名账号存在于系统上，但不是面板创建的。
	// 拒绝静默接管（与“已有第三方共享不自动覆盖”同一条原则）。
	NoticeAccountNotManaged = "ACCOUNT_NOT_MANAGED"
	// NoticeAccountDisabled 表示账号已被禁用（Samba 层 D 标志）。
	NoticeAccountDisabled = "ACCOUNT_DISABLED"
	// NoticeAccountHasShares 表示还有共享定义引用这个账号，不能删。
	NoticeAccountHasShares = "ACCOUNT_HAS_SHARES"
	// NoticeAccountNameInvalid 表示账号名不符合形状要求。
	NoticeAccountNameInvalid = "ACCOUNT_NAME_INVALID"
	// NoticeAccountNameTaken 表示这个账号名已经有一条受管理记录。
	// 新建与改名的正确做法是更新那条记录，而不是再造一条同名记录。
	NoticeAccountNameTaken = "ACCOUNT_NAME_TAKEN"
	// NoticeAccountLookupTimeout 是告警：预检没能在预算内问出“系统里有没有同名账号”。
	//
	// 判决照常继续：执行阶段会用更长的预算再核验一次，而 useradd 本身对已存在的名字
	// 会失败，所以“不接管既有账号”这条不变量不靠这次预检。告警的意义是不把
	// “没核验”说成“核验过了”。
	NoticeAccountLookupTimeout = "ACCOUNT_LOOKUP_TIMEOUT"
	// NoticeProtocolUnsupported 表示 protocol=nfs（P08 只做 smb）。
	NoticeProtocolUnsupported = "PROTOCOL_UNSUPPORTED"
	// NoticeSMBUnavailable 表示本机 Samba 能力不可用（缺包或缺工具）。
	NoticeSMBUnavailable = "SMB_UNAVAILABLE"
	// NoticeSecretInvalid 表示 secret_ref 不是本机秘密存储里的引用，或用途不对。
	NoticeSecretInvalid = "SECRET_INVALID"
	// NoticeACLToolsMissing 表示 setfacl/getfacl 缺失，无法准备或核验权限。
	NoticeACLToolsMissing = "ACL_TOOLS_MISSING"
	// NoticeIncludeDirMissing 表示托管 include 目录不存在（P08.2 才会创建它）。
	// 它在 P08.1 只是告警：账号与 ACL 不依赖它。
	NoticeIncludeDirMissing = "INCLUDE_DIR_MISSING"
	// NoticeConfigNotReadable 表示读不到 smb.conf。读不到就不能说“这台机器的
	// Samba 配置长什么样”，能力因此是 blocked 而不是 available。
	NoticeConfigNotReadable = "CONFIG_NOT_READABLE"

	// NoticeShareConfigNotApplied 是告警：这份定义**还没有**写进 Samba 配置，
	// 因此共享此刻不对外提供访问（P08.2 才有配置写入与重载）。
	NoticeShareConfigNotApplied = "SHARE_CONFIG_NOT_APPLIED"
	// NoticeShareDirectoryNotEmpty 是告警：目录里已经有文件，ACL 会覆盖既有权限。
	NoticeShareDirectoryNotEmpty = "SHARE_DIRECTORY_NOT_EMPTY"
	// NoticeShareAccountPasswordKept 是告警：这次没有换密码，沿用上次设置的。
	NoticeShareAccountPasswordKept = "ACCOUNT_PASSWORD_KEPT"
	// NoticeShareACLReplaced 是告警：目录上原有的扩展 ACL 会被整份替换。
	NoticeShareACLReplaced = "SHARE_ACL_REPLACED"

	// ---- P08.2 起的配置写入相关码 ----
	//
	// 这些码描述“写 Samba 配置”这一半。执行期的失败码（CONFIG_VALIDATION_FAILED 等）
	// 不出现在预检的阻塞项里：它们描述的是“真的去写的时候失败了”，而不是
	// “现在不该做”。

	// NoticeConfigNotWritable 表示“配置写不下去”：缺 testparm 或 smbcontrol，
	// 或 smb.conf 读不到。它由能力探针给出，不是对目录做的写探测——预检时去试写
	// 就等于提前改动机器。真正的写失败在执行期以 CONFIG_WRITE_FAILED 出现。
	NoticeConfigNotWritable = "CONFIG_NOT_WRITABLE"
	// NoticeTestparmMissing 表示没有 testparm：配置校验工具缺失时不允许写配置。
	//
	// 没有它就没有“先校验再重载”这条纪律可依——写完直接重载等于拿一个语法没验过的
	// 文件去换掉正在服务的配置。
	NoticeTestparmMissing = "TESTPARM_MISSING"
	// NoticeSMBServiceInactive 是告警：smbd 没在运行。配置会照写、照校验，
	// 但**共享此刻不对外提供访问**，执行结果因此只能是 accepted。
	NoticeSMBServiceInactive = "SMB_SERVICE_INACTIVE"
	// NoticeSMBServiceUnknown 是告警：问不出 smbd 的运行状态。
	NoticeSMBServiceUnknown = "SMB_SERVICE_UNKNOWN"
	// NoticeConfigGlobalOverride 是告警：托管文件的 [global] 段会覆盖现有配置里的取值。
	// 消息里逐项列出“现在是什么 → 会变成什么”。
	NoticeConfigGlobalOverride = "CONFIG_GLOBAL_OVERRIDE"
	// NoticeConfigIncludeUnresolved 是告警：现有配置里有解析不了的 include
	// （含 % 变量或通配符），于是“有没有同名段”这件事无法完全确认。
	NoticeConfigIncludeUnresolved = "CONFIG_INCLUDE_UNRESOLVED"
	// NoticeShareConfigDrift 是告警：托管配置文件与登记表不一致（有人手工改过，
	// 或者上一次写入没走到最后）。应用这条共享会把它整份重写回去。
	NoticeShareConfigDrift = "SHARE_CONFIG_DRIFT"
	// NoticeShareNameTakenBySystem 表示同名段已经存在于现有 Samba 配置里
	// （不在面板登记表里）。不允许靠重名覆盖别人的段。
	//
	// 它与 SHARE_NAME_TAKEN 的区别只在“在哪发现的”：一个是面板自己的登记表，
	// 一个是 /etc/samba 里的既有配置。两者都是“不许覆盖”。
	NoticeShareNameTakenBySystem = "SHARE_NAME_TAKEN_BY_SYSTEM"

	// ---- NFS（P14.4）相关码 ----
	//
	// NFS 的访问控制是“网段 + root_squash”，没有 Samba 账号那一层，所以它的阻塞项
	// 与 smb 不共用：把 smb 的 SHARE_NO_USERS 套到 nfs 上会得到一句自相矛盾的话
	// （“没有授权账号”——NFS 本来就不认账号）。

	// NoticeNFSOptionsInvalid 表示 nfs_options 缺失或取值不受支持。
	NoticeNFSOptionsInvalid = "NFS_OPTIONS_INVALID"
	// NoticeNFSOpenToWorld 表示允许网段覆盖全部地址（前缀长度为 0）。
	NoticeNFSOpenToWorld = "NFS_OPEN_TO_WORLD"
	// NoticeNFSUsersNotAllowed 表示 nfs 定义里带了 smb_users。
	NoticeNFSUsersNotAllowed = "NFS_USERS_NOT_ALLOWED"
	// NoticeShareNoCIDRs 表示 nfs 共享没有点名允许网段。
	NoticeShareNoCIDRs = "SHARE_NO_CIDRS"
	// NoticeNFSUnavailable 表示本机 NFS 能力不可用（缺 exportfs 或读不到导出配置）。
	NoticeNFSUnavailable = "NFS_UNAVAILABLE"
	// NoticeExportPathConflict 表示这条共享的目录已经被导出了——要么被别的导出
	// 文件导出，要么被面板自己的另一条共享占着。两种来源，同一个后果。
	//
	// 为什么是阻塞：exportfs 对同一条路径被导出两次报 `duplicated export entries`
	// 并以 1 退出，整份导出表的刷新因此失败——别人原本好用的导出也一起停止刷新。
	NoticeExportPathConflict = "EXPORT_PATH_CONFLICT"
	// NoticeExportsUnparsable 是告警：别的导出文件里有读不懂的行。
	//
	// 有它就不能说“确认没有冲突”。不阻塞是因为那不是面板的文件，也不该由面板
	// 拒绝提供服务：执行时 exportfs 自己会报错，而那条路径带自动回滚。
	NoticeExportsUnparsable = "EXPORTS_UNPARSABLE"
	// NoticeExportsNotApplied 是告警：这条共享此刻不在导出表里。
	NoticeExportsNotApplied = "EXPORTS_NOT_APPLIED"
	// NoticeExportsUnreadable 是告警：读不到实际导出表（`exportfs -v` 失败）。
	//
	// 与 EXPORTS_UNPARSABLE 的区别在“读的是谁”：那条说的是别人的导出文件内容读不懂，
	// 这条说的是连实际生效的那张表都拿不到——“此刻有没有在被导出”因此无从回答。
	NoticeExportsUnreadable = "EXPORTS_UNREADABLE"
	// NoticeNFSServiceInactive 是告警：nfs-server 没在运行，导出写下去也提供不了访问。
	NoticeNFSServiceInactive = "NFS_SERVICE_INACTIVE"

	// ---- 连接状态（share.connections）相关码 ----
	// NoticeConnectionStatusUnavailable 表示没有 smbstatus，读不到连接状态。
	// 它不影响配置与共享：只是“谁连着”这个问题回答不了。
	NoticeConnectionStatusUnavailable = "CONNECTION_STATUS_UNAVAILABLE"
	// NoticeConnectionStatusFailed 表示 smbstatus 跑了但没给出可用结果
	// （非 root、smbd 已退出但 socket 还在……）。
	NoticeConnectionStatusFailed = "CONNECTION_STATUS_FAILED"
	// NoticeConnectionStatusPartial 是告警：连接列表可信，但打开文件数归并不了
	// （算不出这份共享的目录）。
	NoticeConnectionStatusPartial = "CONNECTION_STATUS_PARTIAL"
)

// 执行期的配置失败码。它们只出现在错误消息里（以及 DECISIONS/文档），不进预检阻塞项。
const (
	// FailConfigValidation 表示候选配置没有通过 testparm。
	FailConfigValidation = "CONFIG_VALIDATION_FAILED"
	// FailConfigWrite 表示写文件这一步失败（磁盘、权限、fsync）。
	FailConfigWrite = "CONFIG_WRITE_FAILED"
	// FailConfigReload 表示重载被拒绝。
	FailConfigReload = "CONFIG_RELOAD_FAILED"
	// FailConfigReadback 表示写完之后 Samba 读回来的东西不是我们要的那份。
	FailConfigReadback = "CONFIG_READBACK_MISMATCH"
	// FailConnectionsRead 表示 smbstatus 的输出读不懂（格式变了、被截断）。
	//
	// 这是“解析失败”，不是“有权限问题”：权限问题会以一条 notice 说明，
	// 因为那种情况下答案“不知道”比答案“空”更接近事实。
	FailConnectionsRead = "CONNECTIONS_READ_FAILED"

	// FailExportsWrite 表示写托管导出文件这一步失败（磁盘、权限、fsync）。
	FailExportsWrite = "EXPORTS_WRITE_FAILED"
	// FailExportsReload 表示 `exportfs -ra` 被拒绝或失败。
	FailExportsReload = "EXPORTS_RELOAD_FAILED"
	// FailExportVerify 表示应用之后 `exportfs -v` 读回来的实际导出与定义不符。
	FailExportVerify = "EXPORT_VERIFY_MISMATCH"
)

// 配置写入与重载的结果闭集。
const (
	// ConfigFileWritten 表示内容变了，文件被重写。
	ConfigFileWritten = "written"
	// ConfigFileUnchanged 表示内容与文件里已有的一模一样，没有写、没有重载。
	ConfigFileUnchanged = "unchanged"
	// IncludeInstalled 表示这次往 smb.conf 里加了 include 行。
	IncludeInstalled = "installed"
	// IncludePresent 表示 include 行本来就在。
	IncludePresent = "present"
	// ReloadDone 表示 smbd 在运行且接受了重载请求。
	ReloadDone = "done"
	// ReloadSkippedInactive 表示 smbd 没运行，重载无从谈起（不是“失败”，是“没做”）。
	ReloadSkippedInactive = "skipped_service_inactive"
	// ReloadFailed 表示 smbd 在运行但重载请求失败或之后不再活跃。
	ReloadFailed = "failed"
	// ReloadSkippedUnchanged 表示文件内容与已有的完全相同，没有重载的必要。
	//
	// 它与 skipped_service_inactive 分开记：一个是“不需要做”，一个是“做不了”。
	// 混成同一个值，页面就没法解释为什么一次幂等重放没有重载。
	ReloadSkippedUnchanged = "skipped_unchanged"

	// ServiceStateActive / Inactive / Unknown 是 smbd 的运行状态闭集。
	ServiceStateActive   = "active"
	ServiceStateInactive = "inactive"
	ServiceStateUnknown  = "unknown"
)

// ImpactConfigChange 表示这次动作会改写这台主机上的持久配置。
const ImpactConfigChange = "config_change"

// ImpactAccessChange 表示这次动作会改变谁可以访问哪些文件。
const ImpactAccessChange = "access_change"

// Blocker 是一条阻塞/告警/提示。
type Blocker struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Impact 是这次动作的影响。
type Impact struct {
	Kind        string `json:"kind"`
	Description string `json:"description"`
}

// ShareUser 是共享定义里的一条访问授权。
type ShareUser struct {
	AccountID string `json:"account_id"`
	Access    string `json:"access"`
}

// NFSOptions 是 NFS 导出选项。P08 不实现 NFS，但定义的结构按契约保留：
// 丢掉字段以后再加会变成破坏性变更。
type NFSOptions struct {
	RootSquash bool `json:"root_squash"`
	Sync       bool `json:"sync"`
}

// ShareDefinition 是共享定义（CONTRACTS.md 第 7 节）。
//
// 字段一个都不多：没有“强制”“跟随符号链接”“guess ok”这类开关——有开关就有绕过
// PLAN.md 第 10 节那条“不跟随符号链接、不开 guest”的路。
type ShareDefinition struct {
	Name         string      `json:"name"`
	Protocol     string      `json:"protocol"`
	VolumeID     string      `json:"volume_id"`
	RelativePath string      `json:"relative_path"`
	Enabled      bool        `json:"enabled"`
	ReadOnly     bool        `json:"read_only"`
	AllowedCIDRs []string    `json:"allowed_cidrs"`
	SMBUsers     []ShareUser `json:"smb_users"`
	NFSOptions   *NFSOptions `json:"nfs_options,omitempty"`
}

// ShareRecord 是登记表里的一条共享（生产实现在 agentstore）。
//
// 三个状态字段是这一阶段诚实性的核心，它们回答的是不同的问题：
//   - ConfigHash    这份定义渲染出来的配置是哪一版（sha256，十六进制）；
//   - DirectoryPrepared  目录与 POSIX ACL 是否已经按这份定义准备**并核验**过；
//   - ConfigApplied 这份定义是否已经写进 Samba 配置并被重载（P08.1 恒为 false）。
type ShareRecord struct {
	ID         string          `json:"id"`
	Definition ShareDefinition `json:"definition"`
	ConfigHash string          `json:"config_hash"`
	Version    string          `json:"version"`
	// DirectoryPrepared 只在 share.apply 成功核验 ACL 之后为真；share.disable 会清回 false
	// （禁用即撤销 ACL）。
	DirectoryPrepared bool `json:"directory_prepared"`
	// ConfigApplied 由 P08.2 起才会变真。P08.1 里它恒为 false，且没有代码路径能把它置真。
	ConfigApplied bool `json:"config_applied"`
	// RuntimeRevoked 是“运行时因为卷离线被撤销”的记录（P08.3）。
	//
	// 它只由代理的卷监视循环写：发现某条共享的卷掉线，就把它的段从托管配置里摘掉
	// 并把这里记上。摘掉之后**不会自动恢复**——卷回来时要把共享重新应用一次，
	// 因为“写配置”是需要人点头的动作，自动写回等于把一次外部故障变成一次静默变更。
	// 为空表示这条共享没有被运行时撤销过。
	RuntimeRevoked  *RuntimeRevocation `json:"runtime_revoked,omitempty"`
	LastOperationID string             `json:"last_operation_id,omitempty"`
	CreatedAt       time.Time          `json:"created_at"`
	UpdatedAt       time.Time          `json:"updated_at"`
}

// RuntimeRevocation 是一次运行时撤销的现场。
type RuntimeRevocation struct {
	At time.Time `json:"at"`
	// VolumeID / VolumeName 是被判定离线的卷。
	VolumeID   string `json:"volume_id"`
	VolumeName string `json:"volume_name"`
	// VolumeState 是当时的观测值（unmounted / unknown / missing…）。
	//
	// 原样记录而不是归成一句“离线”：以后要回答“为什么那次被撤了”时，
	// “当时它是 unmounted”与“当时读不出来”是两条完全不同的线索。
	VolumeState string `json:"volume_state"`
	// Reason 给人读的一句话。
	Reason string `json:"reason"`
}

// AccountRecord 是登记表里的一条受管理账号。
//
// 它**没有**密码字段，也没有密码哈希字段：明文只在密码秘密通道里流动一次，之后
// 以密文存在代理秘密表里，记录里只有引用（SecretRef）。
type AccountRecord struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
	// Managed 恒为 true：登记表里的每一条都是面板创建的。字段保留是因为
	// CONTRACTS.md 第 7 节的 AccountSummary 有它，前端按它决定能不能删。
	Managed  bool `json:"managed"`
	Disabled bool `json:"disabled"`
	// HasPassword 报告这个账号在 Samba passdb 里是否已经设过密码。
	HasPassword bool   `json:"has_password"`
	UID         int    `json:"uid"`
	GID         int    `json:"gid"`
	Shell       string `json:"shell"`
	Home        string `json:"home"`
	// DisplayName 是给人看的标签（P14.5）。空串表示“和账号名一样”。
	//
	// 它不参与任何系统调用：页面上叫“张三的备份账号”与人能登录的 alice 之间
	// 没有任何映射关系，改名也只改这一列。
	DisplayName string `json:"display_name"`
	// ShellProfile 是 P14.5 的两档壳（disabled/restricted），即**期望**的那一档。
	//
	// 它和 Shell 不是一回事：禁用一个账号时 Shell 会变成 nologin，而 ShellProfile
	// 仍然记着“启用之后该回到哪个壳”，否则启用就无从下手（见 accounts/execute.go）。
	// 共享账号只可能是 disabled——DB 上有 CHECK 兜着。
	ShellProfile string `json:"shell_profile"`
	// SecretRef 是密码密文的引用（sec_ 加 32 位小写十六进制）。它**不是**密码。
	SecretRef       string    `json:"-"`
	Version         string    `json:"version"`
	LastOperationID string    `json:"last_operation_id,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// ShareApplyParams 是 share.apply 的参数（CONTRACTS.md 第 5 节）。
//
// share_id 与 version 用指针：新建时必须是 null，更新时必须是具体的值。
// 用 string 的话“没传”与“传了空串”无法区分，而两者的正确行为完全不同。
type ShareApplyParams struct {
	ShareID    *string          `json:"share_id"`
	Version    *string          `json:"version"`
	Definition *ShareDefinition `json:"definition"`
}

// ShareRefParams 是 share.disable / share.delete 的参数。
type ShareRefParams struct {
	ShareID string `json:"share_id"`
	Version string `json:"version"`
}

// AccountApplyParams 是 share.account.apply 的参数。
//
// secret_ref 可以为空——那表示“只改禁用状态，不换密码”。非空时它必须指向本机
// 秘密存储里一条 smb_password 用途的秘密；明文由执行器在最后一刻解析，
// 只经标准输入交给 smbpasswd。
type AccountApplyParams struct {
	AccountID *string `json:"account_id"`
	Name      string  `json:"name"`
	SecretRef string  `json:"secret_ref"`
	Disabled  bool    `json:"disabled"`
}

// AccountRefParams 是 share.account.delete 的参数。
type AccountRefParams struct {
	AccountID string `json:"account_id"`
}

// Assessment 是一次预检判决的结果。形状与 storage.Assessment 一致，
// 代理层的映射函数因此可以共用同一套规则。
type Assessment struct {
	Action           string
	NormalizedParams any

	TargetID            string
	DisplayName         string
	IdentityFingerprint string
	StateVersion        string

	Allowed  bool
	Blockers []Blocker
	Warnings []Blocker
	Impacts  []Impact
	Locks    []string
}

// WriteRequest 是一次执行请求。
//
// 与 storage.WriteRequest 同形：目标身份与状态版本来自预检，参数是预检落库的
// 规范化参数，执行器自己会再复核一次“还是那个卷、还是那个版本”。
type WriteRequest struct {
	Action               string
	Params               any
	ExpectedIdentity     string
	ExpectedStateVersion string
	OperationID          string
	Progress             func(step string, fields map[string]any)
}

// WriteReport 是一次执行的结果。
//
// Outcome 用与代理 Result 相同的闭集语义：P08.1 的 share.apply 是 accepted
// （定义/目录/ACL 已核验，但共享尚未提供访问），账号动作是 verified
// （账号真的建出来/删掉了，并用 getent 与 pdbedit 读回核对过）。
type WriteReport struct {
	Outcome      string
	Observed     map[string]any
	Data         map[string]any
	RecoveryHint string
}

// Store 是共享与账号的持久登记表（生产实现见 agentstore）。
//
// 接口定义在消费方：判决层与执行层都要读它，但两者都不该知道表结构。
type Store interface {
	ListShares(ctx context.Context) ([]ShareRecord, error)
	GetShare(ctx context.Context, id string) (ShareRecord, bool, error)
	// GetShareByName 用于“同名冲突”判决。名字在登记表里唯一，这是数据库约束。
	GetShareByName(ctx context.Context, name string) (ShareRecord, bool, error)
	SaveShare(ctx context.Context, rec ShareRecord) error
	DeleteShare(ctx context.Context, id string) error

	ListAccounts(ctx context.Context) ([]AccountRecord, error)
	GetAccount(ctx context.Context, id string) (AccountRecord, bool, error)
	// GetAccountByName 用于“系统上已有同名账号”与“新增时改名”两条路径。
	GetAccountByName(ctx context.Context, name string) (AccountRecord, bool, error)
	SaveAccount(ctx context.Context, rec AccountRecord) error
	DeleteAccount(ctx context.Context, id string) error
}

// 执行结果闭集（与 internal/agent 的 Result.Outcome 保持一致）。
const (
	OutcomeVerified = "verified"
	OutcomeAccepted = "accepted"
)

// ManagedShare 是一条**已经渲染好**、准备写进托管配置的共享。
//
// 它是渲染层的输入单位：Block 是 [名字] 开头的那一段，Dir 与 ValidUsers 是写完之后
// 要拿 testparm 读回核对的两件事（“文件里写了什么”不等于“Samba 读出来是什么”）。
type ManagedShare struct {
	// Name 是段名（共享名，原样保留大小写；Samba 内部按小写比较）。
	Name string `json:"name"`
	// Dir 是共享目录的绝对路径。
	Dir string `json:"dir"`
	// MountPath 是这条共享所在卷的挂载点。
	//
	// 它不参与渲染（配置里写的是 Dir），但启动依赖要用它：`RequiresMountsFor=`
	// 收的是“必须就位才能启动”的路径，卷的挂载点才是那个东西——写共享子目录也行，
	// 但挂载点更直接，而且它是面板自己按 slug 生成的，不可能带空格。
	MountPath string `json:"mount_path"`
	// ValidUsers 是授权名单（已解析成系统用户名，按名字排序）。
	ValidUsers []string `json:"valid_users"`
	// Block 是渲染出来的配置段（首行是 [Name]）。
	Block string `json:"-"`
}

// ConfigOverride 是一项会被托管文件覆盖的全局设置。
//
// Current 是**现在** samba 读出来的有效值（由 testparm 读回，不是从文件里猜的），
// Desired 是托管文件会把它改成什么。两者不同才出现在报告里。
type ConfigOverride struct {
	Key     string `json:"key"`
	Current string `json:"current"`
	Desired string `json:"desired"`
}

// ManagedConfigReport 是一次配置写入的结果。
//
// 每一个字段都是“真的做了什么”，不是“打算做什么”：
//   - File 是 written / unchanged（内容没变就不写、不重载）；
//   - Include 是 installed / present；
//   - Reload 是 done / skipped_service_inactive / failed；
//   - Served 只有在前三项都对且 smbd 在运行时才是 true。
type ManagedConfigReport struct {
	Path    string `json:"path"`
	Hash    string `json:"hash"`
	File    string `json:"file"`
	Include string `json:"include"`
	Reload  string `json:"reload"`
	// Service 是 smbd 的运行状态（active/inactive/unknown）。
	Service string `json:"service"`
	// Served 是“这份配置此刻真的在被 Samba 提供”的结论。
	Served bool `json:"served"`
	// Testparm 是校验结果（候选文件一次、合并后的主配置一次）。
	Testparm string `json:"testparm"`
	// Sections 是写完之后托管文件里的段名（不含 global）。
	Sections []string `json:"sections"`
	// Overrides 是这次覆盖掉的全局设置。
	Overrides []ConfigOverride `json:"overrides"`
	// Backups 是这次产生的备份文件路径（没有写就没有）。
	Backups []string `json:"backups"`
	// VolumeOffline 是这次**被跳过**的共享名（它们绑定的卷此刻没挂着）。
	//
	// 跳过是 P08.3 的暴露面保护：托管文件是整份重写的，所以“应用 B 的共享”
	// 这件事本身会把 A 的段重新写一遍。如果 A 的卷此刻没挂，重新写就等于把
	// 系统盘上的一个空目录又共享出去一次——那不是幂等，那是回归。
	VolumeOffline []string `json:"volume_offline_shares"`
	// StartupDeps 是开机依赖的写入结果：written / unchanged / removed / failed /
	// skipped_no_service。
	StartupDeps string `json:"startup_deps"`
	// StartupPaths 是写进 smbd 单元的那些“必须先就位”的路径。
	StartupPaths []string `json:"startup_paths"`
	// Notes 是给人看的补充说明。
	Notes []Blocker `json:"notes"`
}

// Connection 是一条 SMB 树连接（一个客户端连到一份共享）。
//
// 字段取自 smbstatus --json 的 tcons 与 sessions：树连接说“连到哪份共享”，
// 会话说“谁连的、用什么方言、有没有签名”。
type Connection struct {
	// Share 是树连接指向的服务名（Samba 里是小写的段名）。
	Share string `json:"share"`
	// Machine 是客户端地址。
	Machine string `json:"machine"`
	// Username 是认证通过的账号名；匿名会话里会是空串。
	Username string `json:"username"`
	// Dialect 是协商出来的 SMB 方言（例如 SMB3_11）。
	Dialect string `json:"dialect"`
	// Signing 是签名状态（例如 partial(AES-128-GMAC) 或 none）。
	Signing string `json:"signing"`
	// ConnectedAt 是这条树连接建立的时间（smbstatus 给的原样字符串）。
	ConnectedAt string `json:"connected_at"`
	// OpenFiles 是这条连接所在共享上打开着的文件数（按 service_path 归并）。
	OpenFiles int `json:"open_files"`
}

// ConnectionsReport 是 share.connections 的返回体。
//
// ServiceActive 单独给出来，因为**空连接列表有两种完全不同的含义**：
// “没有人连着”与“smbd 根本没在跑”。真机验证过：smbd 停掉之后 smbstatus 仍然以
// 退出码 0 返回空的 sessions/tcons，两种状态在输出上一模一样。把服务状态一起报，
// 页面才不会把“服务停了”显示成“一切正常，只是没人用”。
type ConnectionsReport struct {
	ShareID   string `json:"share_id"`
	ShareName string `json:"share_name"`
	// Service 是 active / inactive / unknown。
	Service string `json:"service"`
	// Connections 按发起时间排序；没有连接时是 []，不是 null。
	Connections []Connection `json:"connections"`
	// CollectedAt 是这次读取 smbstatus 的时间。
	CollectedAt time.Time `json:"collected_at"`
	// Notices 说明读不到或读不全的原因（例如 smbstatus 缺失、服务没在运行）。
	Notices []Blocker `json:"notices"`
}

var (
	// ErrStoreUnavailable 表示没有接上登记表：共享能力因此不可用。
	ErrStoreUnavailable = errors.New("shares: 缺少登记表")
	// ErrNotImplemented 表示该动作在本阶段只有预检、没有执行器。
	ErrNotImplemented = errors.New("shares: 本阶段不提供执行")
)
