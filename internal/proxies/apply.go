package proxies

// 代理站点的写入路径（P10.2）：生成配置、原子写入、nginx -t 门禁、reload、失败恢复。
//
// 执行顺序是固定的，每一步都在为一件事兜底：
//
//	现场复核（版本 + 域名冲突，不吃缓存）
//	  → 渲染（纯函数）
//	  → htpasswd（basic_auth 时先写账号文件：配置一旦生效，账号文件必须在位）
//	  → 写候选（fsync）→ 备份现行文件 → rename → fsync 目录
//	  → 清理同一站点改名后留下的旧文件（备份在前）
//	  → nginx -t（**合并后**的整棵树）→ 失败则回滚 → 再验证回滚后的树
//	  → nginx -s reload（仅在 nginx 为 active 时）
//	  → 再采集一次，读回核对（文件内容哈希、server_name、服务状态）
//
// 三条纪律，与 P08.2 的 Samba 写入路径同源：
//
//  1. **先备份，后替换。** rename 之后再备份，备份的就是新内容。
//  2. **nginx -t 的退出码是证据，但不够。** 它只证明整棵树此刻可解析；证明不了
//     reload 被接受、也证明不了服务还在跑。所以还要读回服务状态与文件内容。
//  3. **失败要回到原样。** 校验失败时把旧文件放回去、再跑一次 nginx -t，把
//     “回滚之后这台机器上的配置是好的”这件事也变成证据。回滚不回去就如实说
//     outcome=unknown 并给出人工恢复路径，绝不假装成功。
//
// 与管理面板的关系：**这条路径不碰 9443。** 托管站点只监听 80（P10.3 加 443），
// 监听端口是常量而不是参数；上游端口等于面板端口在模型层就被拒绝（P10.1）。
// 面板自己不经过 Nginx（PLAN.md 第 6 节），因此这里写错配置的最坏结果是“站点不可用、
// 面板照常登录”，而这一点必须由验收证明，不是靠这句话。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"upanel/internal/syscmd"
)

const (
	// ActionProxyApply 是唯一一个代理站点动作：把登记的定义写成 Nginx 配置并生效。
	//
	// 新建、改名、改策略、停用、重新启用都是它——真正变化的只有“文件内容是什么”，
	// 而那个差别由定义本身表达（enabled=false 渲染出的结果是“这个文件不该存在”）。
	// 拆成 apply/enable/disable/rename 四个动作会让四条路径各自演化，最后必然出现
	// “停用没删文件”“启用没重建文件”这类只在一条路径上修好的缺陷。
	ActionProxyApply = "proxy.apply"

	// 锁。托管文件按站点一份，但 nginx -t 与 reload 作用于**整棵树**：
	// 两个并发应用各自都合法，合在一起可能让第二个把第一个的 reload 挤掉，
	// 也可能各自备份一份、回滚回不同的版本。全局锁是这条流水线的必要条件。
	LockNginxConfig = "nginx:config"

	// 时间预算。nginx -t 读整棵树，机器忙时几秒；reload 只是发信号，但它要等
	// 旧 worker 退出——那一步不由我们等待，命令本身是快的。
	validateTimeout = 20 * time.Second
	reloadTimeout   = 20 * time.Second
	// unitProbeTimeout 是一次 systemctl is-active 的预算。
	unitProbeTimeout = 10 * time.Second

	// OutcomeVerified / OutcomeAccepted 与 internal/agent 的结果闭集一致。
	OutcomeVerified = "verified"
	OutcomeAccepted = "accepted"

	// 文件状态闭集（报告里用）。
	FileStateWritten   = "written"
	FileStateRemoved   = "removed"
	FileStateUnchanged = "unchanged"
	FileStateAbsent    = "absent"

	// 重载结果闭集。
	ReloadDone            = "done"
	ReloadSkippedNoChange = "skipped_unchanged"
	ReloadSkippedInactive = "skipped_service_inactive"
	ReloadFailed          = "failed"

	// ReloadReasonCertRotated 是"配置字节没变、但证书换过了"这一次 reload 的理由。
	//
	// 它存在的意义是把一次看起来"什么都没改却重载了"的应用解释清楚：续期只换证书
	// 文件，渲染结果逐字节相同，按配置字节判断就会漏掉这次 reload。
	ReloadReasonCertRotated = "cert_rotated"
)

// 阻塞码与告警码（闭集，测试按码断言）。
const (
	CodeProxyNotFound         = "PROXY_NOT_FOUND"
	CodeProxyVersionMismatch  = "PROXY_VERSION_MISMATCH"
	CodeNginxUnavailable      = "NGINX_UNAVAILABLE"
	CodeNginxNotInstalled     = "NGINX_NOT_INSTALLED"
	CodeDomainTaken           = "PROXY_DOMAIN_TAKEN"
	CodeDomainUnknown         = "PROXY_DOMAIN_UNKNOWN"
	CodeSecretInvalid         = "PROXY_SECRET_INVALID"
	CodeTargetMissing         = "PROXY_UPSTREAM_UNVERIFIED"
	CodeUnitInactive          = "NGINX_UNIT_INACTIVE"
	CodeConfigDrift           = "PROXY_CONFIG_DRIFT"
	CodeStaleFileRemoved      = "PROXY_STALE_FILE_REMOVED"
	CodeAuthFileWorldReadable = "PROXY_AUTH_FILE_WORLD_READABLE"
	CodeNotWritable           = "NGINX_CONFIG_NOT_WRITABLE"
	CodeRollbackRestored      = "NGINX_ROLLBACK_RESTORED"
	CodeRollbackFailed        = "NGINX_ROLLBACK_FAILED"
)

// Blocker 是一条阻塞项或告警项。
type Blocker struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Impact 是这次动作的影响。
type Impact struct {
	Kind        string `json:"kind"`
	Description string `json:"description"`
}

// ApplyParams 是 proxy.apply 的参数。
//
// 定义随参数走，而不是让代理去面板库里读：库在面板那一侧（它描述的是人想怎么配，
// 不是这台主机的事实），代理读不到它。参数由面板在预检时从记录里展开、固化，
// 因此在预检与执行之间改了记录也不会让这次任务写到别的内容上——那正是
// “绑定了什么就执行什么”的意思。
type ApplyParams struct {
	SiteID     string      `json:"site_id"`
	Version    int         `json:"version"`
	Definition *Definition `json:"definition"`
	// PanelPort 是管理面板自己监听的端口，由面板在预检展开时写入。
	//
	// 它必须来自面板的真实监听地址，而不是参数里的任意值：同一条规则在 P10.1 的
	// 记录校验里已经执行过一次，这里是执行层的第二次——因为执行层的入口是 HTTP
	// 参数，而“记录校验过了”不能替参数上的值担保。
	PanelPort int `json:"panel_port"`
	// Certificate 是站点绑定的 TLS 材料（P10.3）。nil 表示这个站点只监听 80。
	//
	// 它随参数走，理由与 Definition 相同：执行时写进配置的必须是预检那一刻
	// 认定的那张证书。**它里面没有私钥**，只有路径与指纹。
	Certificate *TLSMaterial `json:"certificate,omitempty"`
}

// Normalize 校验并归一化参数。
func (p ApplyParams) Normalize() (ApplyParams, error) {
	out := p
	out.SiteID = strings.TrimSpace(out.SiteID)
	if !siteIDPattern.MatchString(out.SiteID) {
		return ApplyParams{}, invalid("site_id", "站点标识必须是 site_ 加 32 位小写十六进制")
	}
	if out.Definition == nil {
		return ApplyParams{}, invalid("definition", "定义不能为空")
	}
	if out.PanelPort < 0 || out.PanelPort > 65535 {
		return ApplyParams{}, invalid("panel_port", "面板端口不合法")
	}
	def, err := out.Definition.Normalize(NormalizeOptions{PanelPort: out.PanelPort})
	if err != nil {
		return ApplyParams{}, err
	}
	out.Definition = &def
	if err := out.normalizeCertificate(); err != nil {
		return ApplyParams{}, err
	}
	return out, nil
}

// normalizeCertificate 检查“站点定义”和“证书材料”这两半是否对得上。
//
// 三分之一的一致性在这里，三分之二在面板的证书记录校验里（证书必须覆盖域名、
// 必须未过期、必须未过界）。重复的那点检查是有意的：执行层的入口是参数，
// 上面那条路径已经证明过的事不能替参数上的值担保。
func (p *ApplyParams) normalizeCertificate() error {
	if p.Certificate == nil {
		// 没有证书就不能有证书标识——反过来也一样。两半只有一半在位时，
		// 最可能的后果是“以为绑了证书，实际只听 80”，而浏览器只会给一个
		// 让人摸不着头脑的连接被拒。
		if p.Definition.CertificateID != "" {
			return invalid("certificate", "定义里绑定了证书，但参数里没有证书材料")
		}
		return nil
	}
	m := *p.Certificate
	m.CertificateID = strings.TrimSpace(m.CertificateID)
	m.Domain = strings.ToLower(strings.TrimSpace(m.Domain))
	m.CertPath = strings.TrimSpace(m.CertPath)
	m.KeyPath = strings.TrimSpace(m.KeyPath)
	m.Fingerprint = strings.ToLower(strings.TrimSpace(m.Fingerprint))
	m.NotAfter = strings.TrimSpace(m.NotAfter)
	m.Issuer = strings.TrimSpace(m.Issuer)
	if err := m.Validate(); err != nil {
		return invalid("certificate", "证书材料不合法：%s", err.Error())
	}
	if p.Definition.CertificateID != m.CertificateID {
		return invalid("certificate", "证书标识与定义里的绑定不一致")
	}
	if m.Domain != p.Definition.Domain {
		// 证书的域名必须就是站点的域名。通配证书由面板在记录校验里折算成
		// “覆盖了这个域名”，到执行层只剩一个等价判断：能写进 server_name 的
		// 那个域名必须被证书覆盖，而这里的最简形式是二者相同。
		return invalid("certificate", "证书域名 %q 与站点域名 %q 不一致", m.Domain, p.Definition.Domain)
	}
	p.Certificate = &m
	return nil
}

// Assessment 是一次判决（预检用）。
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

// WriteRequest 是一次执行请求（与 shares 同形）。
type WriteRequest struct {
	Action               string
	Params               any
	ExpectedIdentity     string
	ExpectedStateVersion string
	OperationID          string
	Progress             func(step string, fields map[string]any)
}

// WriteReport 是一次执行的结果。
type WriteReport struct {
	Outcome      string
	Observed     map[string]any
	Data         map[string]any
	RecoveryHint string
}

// SecretResolver 是秘密通道的消费端（生产实现是 agentstore.Secrets）。
//
// 只有解析，没有列表：代理层需要的仅仅是“这个引用对应的明文在这里”，
// 用完立刻清零。**没有任何把明文交出去的接口。**
type SecretResolver interface {
	Resolve(ctx context.Context, ref string) ([]byte, error)
}

// CommandRunner 是把命令交给内核的唯一出口（生产实现转给 syscmd.Run）。
type CommandRunner interface {
	Run(ctx context.Context, spec syscmd.Spec, limits syscmd.Limits) (syscmd.Result, error)
}

// ApplierOptions 是写入侧的装配参数。
type ApplierOptions struct {
	// Facts 是 Nginx 现状（必需：没有它就无法判断域名冲突，也无法读回核对）。
	Facts FactsSource
	// Secrets 是秘密通道（basic_auth 时必需）。
	Secrets SecretResolver
	// Runner 是命令执行器（必需）。
	Runner CommandRunner
	// Paths 是配置路径（零值走默认）。
	Paths ConfigPaths
	// FS 是文件系统接缝（零值走真实实现）。
	FS ConfigFS
	// Now 提供时间（可注入）。
	Now func() time.Time
	// TargetExists 判断上游目标此刻是否在代理的发现结果里（可选）。
	//
	// 它只产生**告警**：目标没在跑不等于配置写错了，而“把它当阻塞”会让一次
	// 正常的上线被一个正在重启的应用挡死。真正危险的目标（外网、云元数据）在
	// 形状层就被拒绝了，这里问的是“它现在活着吗”，是健康问题不是安全问题。
	TargetExists func(ctx context.Context, targetID string) (bool, error)

	// ServedCert 取“443 上此刻正在用的证书指纹”（可选，零值走真实握手）。
	//
	// 它是可注入的，因为真实的判断是向 127.0.0.1:443 做一次带 SNI 的握手——
	// 单元测试里既没有 443 也不该去监听它。注入它换来的是：把“证书换了但配置
	// 没变”这条分支放进普通测试里跑，而不是只靠真机验收。
	ServedCert func(ctx context.Context, domain string, port int) (string, error)
}

// Applier 是反向代理写入路径的入口。
type Applier struct {
	opts ApplierOptions
}

// NewApplier 构造写入侧。缺必需项时返回错误：宁可启动时报错，也不要在第一次
// 点击时返回一个说不清原因的 503。
func NewApplier(opts ApplierOptions) (*Applier, error) {
	if opts.Facts == nil {
		return nil, errors.New("proxies: 写入路径缺少 Nginx 现状来源")
	}
	if opts.Runner == nil {
		return nil, errors.New("proxies: 写入路径缺少命令执行器")
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Applier{opts: opts}, nil
}

// servedCert 取 443 上此刻在用的证书指纹（未注入时走真实握手）。
func (a *Applier) servedCert(ctx context.Context, domain string, port int) (string, error) {
	if a.opts.ServedCert != nil {
		return a.opts.ServedCert(ctx, domain, port)
	}
	return servedCertFingerprint(ctx, domain, port)
}

func (a *Applier) paths() ConfigPaths {
	p := a.opts.Paths
	if p.Root == "" {
		p.Root = DefaultConfigRoot
	}
	if p.NginxPath == "" {
		p.NginxPath = DefaultNginxPath
	}
	if p.BackupDir == "" {
		p.BackupDir = DefaultBackupDir
	}
	return p
}

func (a *Applier) fs() ConfigFS {
	if a.opts.FS.ReadFile != nil {
		return a.opts.FS
	}
	return SystemConfigFS()
}

// DisplayName 返回一个站点的显示名（域名）。
func (p ApplyParams) DisplayName() string {
	if p.Definition == nil {
		return p.SiteID
	}
	return p.Definition.Domain
}

// StateVersionOf 是预检绑定的状态版本。
//
// 绑“站点 id + 版本号”：执行时参数本来就是预检固化下来的那一份，这个值只用来
// 证明“任务提交的是这份计划”，不承担乐观并发——并发由面板的记录版本负责。
func StateVersionOf(p ApplyParams) string {
	return fmt.Sprintf("site:%s:v%d", p.SiteID, p.Version)
}
