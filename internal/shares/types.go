package shares

// SMB 共享与受管理共享账号（P08.1）。
//
// 这一包只做 P08.1 界定的四件事：
//   - Samba 能力检测（只读探针，不安装、不改配置）；
//   - 受管理共享账号的生命周期（Linux 账号 + Samba passdb，只用受管理记录里的账号）；
//   - 密码秘密通道的**消费端**（参数里只有 secret_ref，明文经标准输入交给 smbpasswd）；
//   - 共享定义、配置版本（渲染块的 sha256）、目录准备与 POSIX ACL。
//
// 明确**不做**（属于 P08.2/P08.3，见 docs/STAGES.md 第 10 节）：
//   - 写 Samba 配置文件、最小 include、testparm、原子替换、重载；
//   - 共享连接状态；
//   - 启动依赖与运行时脱盘撤销。
//
// 因此 share.apply 的执行结果是 `accepted` 而不是 `verified`：定义、目录与 ACL 都是
// **真的**做出并核验过的，但这份共享此刻还没有被任何 Samba 进程提供。observed_state
// 里 `samba_config` 恒为 not_written、`served` 恒为 false，预检也带一条同名的告警。
// 把“定义准备好了”说成“共享可用”是这一阶段最容易犯的谎，所以它被写死在返回结构里。

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
	ConfigApplied   bool      `json:"config_applied"`
	LastOperationID string    `json:"last_operation_id,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
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

var (
	// ErrStoreUnavailable 表示没有接上登记表：共享能力因此不可用。
	ErrStoreUnavailable = errors.New("shares: 缺少登记表")
	// ErrNotImplemented 表示该动作在本阶段只有预检、没有执行器。
	ErrNotImplemented = errors.New("shares: 本阶段不提供执行")
)
