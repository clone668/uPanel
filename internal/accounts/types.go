package accounts

// 受管理 Linux 账号、SSH 公钥与受限 sudo 的执行层（P14.5）。
//
// 这一层与共享那半边（internal/shares）复用同一套**判决形状**：Assessment /
// Blocker / WriteRequest / WriteReport / Outcome 闭集都来自 internal/shares，
// 不是因为名字相近，而是因为 CONTRACTS.md 第 5 节把 account.apply 与
// share.account.apply 放在同一张动作表里、写同一张 managed_accounts 表（DECISIONS
// Q475）。两套形状会让代理里出现两个几乎一样的适配器，而“两个适配器”就是
// “两条读回核验”的起点。
//
// 分工是硬的：**普通账号（regular）在这里执行，共享账号（share）不在这里执行**。
// account.apply 收到 type=share 直接拒绝并指向 share.account.apply——共享账号的
// 凭据在 Samba 的 passdb 里，那条路已经有 P08.1 交付并验收过的读回与回滚。

import (
	"context"
	"errors"
	"os"
	"time"

	"upanel/internal/shares"
	"upanel/internal/syscmd"
)

// 动作名（CONTRACTS.md 第 5 节）。
const (
	ActionAccountApply   = "account.apply"
	ActionAccountDelete  = "account.delete"
	ActionSSHKeyApply    = "ssh_key.apply"
	ActionSSHKeyDelete   = "ssh_key.delete"
	ActionSudoRuleApply  = "sudo_rule.apply"
	ActionSudoRuleDelete = "sudo_rule.delete"
)

// 只读查询名（P14.5 加了两条）。
//
// 账号列表没有新查询：GET /accounts 读的是 share.accounts——普通账号与共享账号
// 在同一张 managed_accounts 里（DECISIONS Q475），多一条 accounts.list 只会让
// 同一行记录从两条查询里出来、字段各自漂移。
//
// 公钥列表必须有新查询：它读的是 0014 才有的 managed_ssh_keys 表，而且按账号过滤。
//
// 能力报告也必须有新查询：share.accounts 带的那份能力是 **Samba 的**（smbd /
// smbpasswd / pdbedit…），拿它当账号页的判据会让一台没装 Samba 的机器连普通账号
// 都建不了。两个模块的工具集不相交，能力就必须各探各的（见 capability.go 开头）。
const (
	// QueryAccountsSSHKeys 返回某个账号在面板里登记的公钥。
	QueryAccountsSSHKeys = "accounts.ssh_keys"
	// QueryAccountsCapability 返回账号模块的能力报告（工具、壳档、sudo 可写性）。
	QueryAccountsCapability = "accounts.capability"
)

// ImpactDataKept 表示这次动作不会删数据。
//
// 与 shares.ImpactConfigChange / ImpactAccessChange 并列的一类：删账号这个动作
// 同时改了系统账号（access_change）和“数据还在不在”（data_kept），而后者是用户
// 最关心的那一句——他要知道 home 里的东西还在。
const ImpactDataKept = "data_kept"

// SudoersLockName 是 sudoers.d 的全局锁名。
//
// 一台机器只有一个 sudoers.d，所有账号的 sudo 规则都在它里面；两条规则同时校验
// 与写入会让“visudo 检查的是 A 状态、rename 的是 B 状态”。所以锁在目录上，
// 不锁单个账号。
const SudoersLockName = "sudoers"

// SSHKeyRecord 是一条已经收进面板的公钥（登记表里的一行）。
//
// Blob 是 base64 的密钥体，**不是**私钥、也不含私钥：公钥是公开材料，落在库里没问题。
// Fingerprint 是 OpenSSH 的 SHA256 指纹，它是“是不是同一把钥匙”的唯一判据——
// 同一把钥匙换了注释再加一次是同一把钥匙，指纹相同。
type SSHKeyRecord struct {
	ID        string `json:"id"`
	AccountID string `json:"account_id"`
	// Type 是算法名（ssh-ed25519 / ssh-rsa / ecdsa-sha2-nistp256…）。
	Type string `json:"type"`
	// Blob 是 base64 的密钥体。
	Blob string `json:"-"`
	// Comment 是行尾注释（可空）。
	Comment string `json:"comment"`
	// Fingerprint 形如 SHA256:xxxxxxxx。
	Fingerprint string    `json:"fingerprint"`
	CreatedAt   time.Time `json:"created_at"`
}

// SudoRuleRecord 是一个账号上的 sudo 规则（一个账号最多一条）。
//
// 一条而不是多条：CONTRACTS.md 第 5 节的 sudo_rule.apply 只带 template_id，
// 没有 rule_id——参数形状已经决定了这张表上 account_id 是唯一的。
type SudoRuleRecord struct {
	ID        string `json:"id"`
	AccountID string `json:"account_id"`
	// Template 是模板标识（闭集，见 sudo.go）。
	Template string `json:"template_id"`
	// Version 是渲染出来的 sudoers 文件的 sha256；内容没变就不重写（幂等）。
	Version   string    `json:"version"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// AccountApplyParams 是 account.apply 的参数（CONTRACTS.md 第 5 节）。
//
// account_id 与 version 用指针：新建时必须是 null，更新时必须是具体的值。
// 用 string 的话“没传”与“传了空串”无法区分，而两者的正确行为完全不同。
type AccountApplyParams struct {
	AccountID  *string     `json:"account_id"`
	Version    *string     `json:"version"`
	Definition *Definition `json:"definition"`
}

// AccountRefParams 是 account.delete 的参数。
type AccountRefParams struct {
	AccountID string  `json:"account_id"`
	Version   *string `json:"version"`
}

// SSHKeyApplyParams 是 ssh_key.apply 的参数。
//
// KeyID 为空表示新增一把钥匙；给了具体的值表示**替换**那一条记录的公钥内容
// （同一个 key_id、新指纹），而不是再加一条。这样“换钥匙”不会在账号上留两把。
type SSHKeyApplyParams struct {
	AccountID string  `json:"account_id"`
	Version   *string `json:"version"`
	KeyID     *string `json:"key_id"`
	PublicKey string  `json:"public_key"`
}

// SSHKeyRefParams 是 ssh_key.delete 的参数。
type SSHKeyRefParams struct {
	AccountID string  `json:"account_id"`
	Version   *string `json:"version"`
	KeyID     string  `json:"key_id"`
}

// SudoRuleApplyParams 是 sudo_rule.apply 的参数。
type SudoRuleApplyParams struct {
	AccountID  string  `json:"account_id"`
	Version    *string `json:"version"`
	TemplateID string  `json:"template_id"`
}

// SudoRuleRefParams 是 sudo_rule.delete 的参数。
//
// 没有 template_id：一个账号最多一条规则，删的就是那一条。
type SudoRuleRefParams struct {
	AccountID string  `json:"account_id"`
	Version   *string `json:"version"`
}

// Store 是账号、公钥与 sudo 规则的持久登记表（生产实现见 agentstore）。
//
// 接口定义在消费方：判决层与执行层都要读它，但两者都不该知道表结构。
// 账号那五个方法与 shares.Store 的账号部分**是同一批方法**（同一张表、同一个
// Record 类型），所以 *agentstore.ShareStore 同时满足两个接口，不需要第二份实现。
type Store interface {
	ListAccounts(ctx context.Context) ([]shares.AccountRecord, error)
	GetAccount(ctx context.Context, id string) (shares.AccountRecord, bool, error)
	GetAccountByName(ctx context.Context, name string) (shares.AccountRecord, bool, error)
	SaveAccount(ctx context.Context, rec shares.AccountRecord) error
	DeleteAccount(ctx context.Context, id string) error

	// ListShares 只用于“这个账号还被共享引用吗”这一条判决。
	//
	// 它读的是同一张 shares 表：删掉一个还在授权名单里的账号会让那份共享对所有人
	// 失效，而失效发生在客户端下一次连接时，不是在删除这一秒——所以必须先问。
	ListShares(ctx context.Context) ([]shares.ShareRecord, error)

	ListSSHKeys(ctx context.Context, accountID string) ([]SSHKeyRecord, error)
	GetSSHKey(ctx context.Context, accountID, keyID string) (SSHKeyRecord, bool, error)
	SaveSSHKey(ctx context.Context, rec SSHKeyRecord) error
	DeleteSSHKey(ctx context.Context, accountID, keyID string) error

	GetSudoRule(ctx context.Context, accountID string) (SudoRuleRecord, bool, error)
	SaveSudoRule(ctx context.Context, rec SudoRuleRecord) error
	DeleteSudoRule(ctx context.Context, accountID string) error
}

// CommandRunner 是把命令交给内核的唯一出口（生产实现转给 syscmd.Run）。
type CommandRunner interface {
	Run(ctx context.Context, spec syscmd.Spec, limits syscmd.Limits) (syscmd.Result, error)
}

// SecretResolver 是密码秘密通道的消费端（生产实现是 agentstore.Secrets）。
//
// 只有两个动作：解析（返回明文，调用方负责用完清零）与挂引用（把 10 分钟就销毁的
// 临时秘密转成持久的）。**没有**“列出秘密”或“读回明文”的通用接口。
type SecretResolver interface {
	Resolve(ctx context.Context, ref string) ([]byte, error)
	Reference(ctx context.Context, ref, reference string) error
}

// Paths 是 sudoers 与 home 根目录的位置。
//
// 生产接线不传它（用默认值）。它存在只为单元测试：真机上 /etc/sudoers.d 是 root 的，
// 而“候选不合格时正式文件一个字节都没动”这条不变量必须在单元测试里能反复验证，
// 不能只靠一次虚拟机运行来担保。
type Paths struct {
	// SudoersDir 是 sudoers.d 目录（默认 /etc/sudoers.d）。
	SudoersDir string
	// SudoersMain 是系统主配置，只用 `visudo -c` 校验整份配置时被间接读到。
	SudoersMain string
	// HomeBase 是普通账号 home 的父目录（默认 /home）。
	//
	// 它不参与创建（useradd --create-home 自己会用 /etc/default/useradd 里的值），
	// 只用于**拒绝**：读回来的 home 不在这个前缀之下时，面板不会去那里写
	// authorized_keys——那可能是一个被人指到 /etc 的账号。
	HomeBase string
}

// DefaultPaths 返回生产路径。
func DefaultPaths() Paths {
	return Paths{SudoersDir: "/etc/sudoers.d", SudoersMain: "/etc/sudoers", HomeBase: "/home"}
}

// FileFS 是文件读写的接缝。
//
// 与 shares.ConfigFS 分开声明而不是共用：这一层写的是**别人也会写的文件**
// （authorized_keys 的托管区、sudoers.d 里的单文件），需要的原语不一样
// （Chown 是必需的：authorized_keys 必须是那个账号的，不能是 root 的）。
type FileFS struct {
	ReadFile      func(name string) ([]byte, error)
	WriteFileSync func(name string, data []byte, perm os.FileMode) error
	Rename        func(oldpath, newpath string) error
	FsyncDir      func(dir string) error
	MkdirAll      func(dir string, perm os.FileMode) error
	Remove        func(name string) error
	Stat          func(name string) (os.FileInfo, error)
	Chown         func(name string, uid, gid int) error
}

// SystemFileFS 是真实的文件系统实现。
func SystemFileFS() FileFS {
	return FileFS{
		ReadFile:      os.ReadFile,
		WriteFileSync: writeFileSync,
		Rename:        os.Rename,
		FsyncDir:      fsyncDir,
		MkdirAll:      func(dir string, perm os.FileMode) error { return os.MkdirAll(dir, perm) },
		Remove:        os.Remove,
		Stat:          os.Stat,
		Chown:         os.Chown,
	}
}

// Options 是账号模块的装配参数。
type Options struct {
	// Store 是登记表（必需）。
	Store Store
	// Secrets 是密码秘密通道（definition 带 password_secret_ref 时必需）。
	Secrets SecretResolver
	// Runner 是命令执行器（必需）。
	Runner CommandRunner
	// Now 提供时间（可注入）。
	Now func() time.Time
	// Paths 是 sudoers 与 home 根目录（零值即生产路径）。
	Paths Paths
	// FS 是文件系统接缝（零值即真实 os 调用）。
	FS FileFS
	// Prober 是能力探针（零值即读真实文件系统的 SystemProber）。
	//
	// 与 shares.Options.Prober 同一个位置、同一个理由：页面要能说出“为什么这一页
	// 不能建账号”，而那句话必须来自一次真实的探测，不能来自一个写死的常量。
	Prober *Prober
}

// Engine 是账号模块的总入口。
type Engine struct {
	opts Options
}

// Capability 返回这台机器此刻能做哪些账号相关的事。
//
// 它**不读**登记表、不碰 sudoers、不跑命令：判据只有几个工具在不在（见
// capability.go 开头那段）。所以代理侧把它挂成一条独立查询（accounts.capability），
// 页面在打开账号页时拿到它与账号列表。
func (e *Engine) Capability(ctx context.Context) Capability {
	if e.opts.Prober == nil {
		// New 里已经补过默认值；这条分支只防“有人手搓 Engine{}”。
		return Capability{Status: CapabilityBlocked, Reason: "能力探针未装配",
			ShellProfiles: []string{}, Notices: []shares.Blocker{}}
	}
	return e.opts.Prober.Capability(ctx)
}

// New 构造 Engine。缺必需项时返回错误：宁可启动时报错，也不要在第一次点击时
// 返回一个说不清原因的 503。
func New(opts Options) (*Engine, error) {
	if opts.Store == nil {
		return nil, errors.New("accounts: 缺少登记表")
	}
	if opts.Runner == nil {
		return nil, errors.New("accounts: 缺少命令执行器")
	}
	if opts.Now == nil {
		opts.Now = func() time.Time { return time.Now().UTC() }
	}
	if opts.Paths.SudoersDir == "" {
		opts.Paths.SudoersDir = DefaultPaths().SudoersDir
	}
	if opts.Paths.SudoersMain == "" {
		opts.Paths.SudoersMain = DefaultPaths().SudoersMain
	}
	if opts.Paths.HomeBase == "" {
		opts.Paths.HomeBase = DefaultPaths().HomeBase
	}
	if opts.FS.ReadFile == nil {
		opts.FS = SystemFileFS()
	}
	if opts.Prober == nil {
		opts.Prober = SystemProber()
	}
	return &Engine{opts: opts}, nil
}

// Accounts 返回全部受管理账号（按名字排序）。
func (e *Engine) Accounts(ctx context.Context) ([]shares.AccountRecord, error) {
	recs, err := e.opts.Store.ListAccounts(ctx)
	if err != nil {
		return nil, err
	}
	if recs == nil {
		recs = []shares.AccountRecord{}
	}
	sortAccounts(recs)
	return recs, nil
}

// Account 返回单条账号。
func (e *Engine) Account(ctx context.Context, id string) (shares.AccountRecord, bool, error) {
	if !shares.ValidAccountID(id) {
		return shares.AccountRecord{}, false, notFound("账号标识 %q 形状不对", id)
	}
	return e.opts.Store.GetAccount(ctx, id)
}

// SSHKeys 返回一个账号的全部公钥（按创建时间排序）。
//
// 顺序是“先加的在前”：页面上的顺序应当与 authorized_keys 里的顺序一致，
// 否则人改一把钥匙之后看到的位置会跳。
func (e *Engine) SSHKeys(ctx context.Context, accountID string) ([]SSHKeyRecord, error) {
	if !shares.ValidAccountID(accountID) {
		return nil, notFound("账号标识 %q 形状不对", accountID)
	}
	if _, found, err := e.opts.Store.GetAccount(ctx, accountID); err != nil {
		return nil, err
	} else if !found {
		return nil, notFound("受管理账号 %s 已不存在", accountID)
	}
	keys, err := e.opts.Store.ListSSHKeys(ctx, accountID)
	if err != nil {
		return nil, err
	}
	if keys == nil {
		keys = []SSHKeyRecord{}
	}
	sortKeys(keys)
	return keys, nil
}

// SudoRule 返回一个账号上的 sudo 规则。
func (e *Engine) SudoRule(ctx context.Context, accountID string) (SudoRuleRecord, bool, error) {
	return e.opts.Store.GetSudoRule(ctx, accountID)
}

// now 返回当前时刻。
func (e *Engine) now() time.Time { return e.opts.Now().UTC() }

// run 执行一条命令并返回结果。非零退出不算“执行失败”，由调用方按语义解释。
func (e *Engine) run(ctx context.Context, spec syscmd.Spec, timeout time.Duration) (syscmd.Result, error) {
	if timeout <= 0 {
		timeout = commandTimeout
	}
	return e.opts.Runner.Run(ctx, spec, syscmd.Limits{Timeout: timeout})
}

// sortAccounts 按名字排序（同名再按 id，保证顺序稳定）。
func sortAccounts(recs []shares.AccountRecord) {
	for i := 1; i < len(recs); i++ {
		for j := i; j > 0; j-- {
			if recs[j].Name < recs[j-1].Name ||
				(recs[j].Name == recs[j-1].Name && recs[j].ID < recs[j-1].ID) {
				recs[j], recs[j-1] = recs[j-1], recs[j]
				continue
			}
			break
		}
	}
}

// sortKeys 按创建时刻排序（同一毫秒再按 id）。
func sortKeys(keys []SSHKeyRecord) {
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0; j-- {
			a, b := keys[j], keys[j-1]
			if a.CreatedAt.Before(b.CreatedAt) ||
				(a.CreatedAt.Equal(b.CreatedAt) && a.ID < b.ID) {
				keys[j], keys[j-1] = b, a
				continue
			}
			break
		}
	}
}
