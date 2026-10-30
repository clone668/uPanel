package certificates

// 证书动作的参数与判据（P10.3）。
//
// 四个动作，全部走「预检 → 执行」这条固定链路：
//
//	certificate.import  手动导入：把一份公钥证书链 + 秘密存储里的私钥装到 /etc/ssl/upanel
//	certificate.issue   用 Certbot 签发一张新证书（staging 是默认，正式要显式选）
//	certificate.renew   续期：同一条 certbot 命令，certbot 自己判断是否到期
//	certificate.state   只读：读盘上那张证书的事实（到期、签发者、指纹）
//
// 与 proxy.apply 的关系是**单向**的：证书先落地，站点才能绑它。站点绑定的是
// 一条证书记录（proxies.Definition.CertificateID），渲染时按记录的路径写
// ssl_certificate——所以证书的路径与权限必须是这里说了算，而不是 certbot 碰巧
// 把它们放在哪。
//
// 这里的判决与报告结构直接复用 internal/proxies 的 Assessment / WriteReport：
// 它们描述的是「一次系统写动作」的形状，与反向代理没有本质关系，代理只是第一个
// 使用者。另起一套平行类型只会让动作登记表里出现两种计划与两种结果。

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"strings"
	"time"

	"upanel/internal/proxies"
	"upanel/internal/syscmd"
	"upanel/internal/tlscert"
)

// 动作名（登记表里用）。
const (
	ActionImport = "certificate.import"
	ActionIssue  = "certificate.issue"
	ActionRenew  = "certificate.renew"
	ActionState  = "certificate.state"
)

// 锁。ACME 一次会话要写 /etc/letsencrypt（账号、renewal 配置、归档），
// 两个 certbot 同时跑会互相踩；手动导入动的是另一个目录，但证书记录是同一个，
// 所以两者共用一个「按域名」的锁，签发再额外加一把全局的。
const (
	LockACMEClient = "acme:certbot"
	LockPrefixCert = "certificate:"
)

// 阻塞码与告警码闭集。
const (
	CodeCertParams          = "CERTIFICATE_PARAMS_INVALID"
	CodeCertPEMInvalid      = "CERTIFICATE_PEM_INVALID"
	CodeCertKeyMissing      = "CERTIFICATE_KEY_MISSING"
	CodeCertKeyMismatch     = "CERTIFICATE_KEY_MISMATCH"
	CodeCertNotCovering     = "CERTIFICATE_DOMAIN_UNCOVERED"
	CodeCertExpired         = "CERTIFICATE_EXPIRED"
	CodeCertIsCA            = "CERTIFICATE_IS_CA"
	CodeCertbotMissing      = "CERTBOT_MISSING"
	CodeCertbotFailed       = "CERTBOT_FAILED"
	CodeACMEReachability    = "ACME_HTTP01_UNREACHABLE"
	CodeCertStaging         = "CERTIFICATE_STAGING"
	CodeCertSelfSigned      = "CERTIFICATE_SELF_SIGNED"
	CodeCertLiveDirMissing  = "CERTIFICATE_LIVE_DIR_MISSING"
	CodeCertFilesUnreadable = "CERTIFICATE_FILES_UNREADABLE"
	CodeCertWritten         = "CERTIFICATE_WRITTEN"
)

// 结果闭集（与 proxies 的 outcome 一致）。
const (
	OutcomeVerified = "verified"
	OutcomeUnknown  = "unknown"
)

// ImportParams 是 certificate.import 的参数。
//
// 证书链在参数里走：它是**公开材料**，参数会落进面板数据库的 normalized_params，
// 一份 4—6 KiB 的 PEM 在那里没有问题。私钥反过来——它只以 secret_ref 的形式出现，
// 明文从来不在参数、日志、数据库里。
type ImportParams struct {
	CertificateID string `json:"certificate_id"`
	Domain        string `json:"domain"`
	// CertPEM 是公钥证书链（叶子在前，fullchain 形态）。
	CertPEM string `json:"cert_pem"`
	// KeySecretRef 是私钥在代理秘密存储里的引用（用途 tls_private_key）。
	KeySecretRef string `json:"key_secret_ref"`
	// Staging 只进报告：手动导入的证书是不是测试环境签的，由证书自己说了算
	// （tlscert 会从签发者名字判断），这里给的是「用户说它是」。
	Staging bool `json:"staging"`
}

// Normalize 校验并归一化导入参数。
func (p ImportParams) Normalize() (ImportParams, error) {
	out := p
	out.CertificateID = strings.TrimSpace(out.CertificateID)
	if !ValidID(out.CertificateID) {
		return ImportParams{}, invalidParams("certificate_id", "证书标识必须是 crt_ 加 32 位小写十六进制")
	}
	domain, err := normalizeDomain(out.Domain)
	if err != nil {
		return ImportParams{}, err
	}
	out.Domain = domain
	if strings.TrimSpace(out.CertPEM) == "" {
		return ImportParams{}, invalidParams("cert_pem", "证书内容不能为空")
	}
	if len(out.CertPEM) > tlscert.MaxCertBytes {
		return ImportParams{}, invalidParams("cert_pem", "证书内容超过 1 MiB")
	}
	out.CertPEM = strings.TrimSpace(out.CertPEM) + "\n"
	out.KeySecretRef = strings.TrimSpace(out.KeySecretRef)
	if out.KeySecretRef == "" {
		return ImportParams{}, invalidParams("key_secret_ref",
			"私钥必须先经 POST /secrets（用途 tls_private_key）上传，再在这里引用")
	}
	if len(out.KeySecretRef) > MaxSecretRefLen {
		return ImportParams{}, invalidParams("key_secret_ref", "私钥引用过长")
	}
	return out, nil
}

// DisplayName 返回动作在任务列表里的显示名。
func (p ImportParams) DisplayName() string { return p.Domain }

// StateVersionOf 是预检绑定的状态版本。
func (p ImportParams) StateVersionOf() string {
	return "certificate:" + p.Domain + ":import"
}

// IssueParams 是 certificate.issue / certificate.renew 的参数。
type IssueParams struct {
	// CertificateID 与 Email 在定时续期里是空的，`omitempty` 让它们在
	// normalized_params 里**不出现**，而不是出现成 `""`。
	//
	// 这不是为了好看：面板把代理交上来的定时任务导入任务表时，会把参数按这个结构体
	// 重新编码一遍，于是计划里本来没有的两个字段会以空值出现在审计行里。真机验收
	// §5 的断言"定时续期的参数里不该有 certificate_id"就是这么红的——字段在，值空。
	// 一条审计记录应当长得像它记录的那次操作，而不是像解码它的那个结构体。
	CertificateID string `json:"certificate_id,omitempty"`
	Domain        string `json:"domain"`
	// Email 是 ACME 账号邮箱（Let's Encrypt 用它发到期提醒）。
	Email string `json:"email,omitempty"`
	// Staging 为 true 时用测试环境。**默认就是它**：正式签发有速率限额
	// （同一域名每周 5 张），一次参数写错就能把域名卡一周。
	Staging bool `json:"staging"`
	// Scheduled 表示这次续期由计划任务触发（P12.5），而不是有人在面板上点的。
	//
	// 它改变三件事，每一件都是"定时"这个前提逼出来的：
	//
	//	证书 id 与邮箱可以空着   触发发生在 systemd 里，那一刻没有面板可问；
	//	                         域名就够了（ACME 账号在 /etc/letsencrypt，
	//	                         证书账本在面板，两边都不是这里能读的）。
	//	不强制重签               `--force-renewal` 每晚跑一次，一周就把 Let's
	//	                         Encrypt 的速率限额烧光；定时续期只做"该续才续"
	//	                         （`--keep-until-expiring` 本来就是这个意思）。
	//	结果自己说话             定时那次没有人看返回值，账本要靠 result 里的
	//	                         observed 回写（面板侧按域名对账）。
	Scheduled bool `json:"scheduled,omitempty"`
}

// Normalize 校验并归一化签发参数。
func (p IssueParams) Normalize() (IssueParams, error) {
	out := p
	out.CertificateID = strings.TrimSpace(out.CertificateID)
	// 定时续期没有证书 id（面板库里的账本是触发那一刻问不到的东西）。空着可以，
	// 但填了就必须合法——"可空"不能变成"随便填"。
	if out.CertificateID != "" || !out.Scheduled {
		if !ValidID(out.CertificateID) {
			return IssueParams{}, invalidParams("certificate_id", "证书标识必须是 crt_ 加 32 位小写十六进制")
		}
	}
	domain, err := normalizeDomain(out.Domain)
	if err != nil {
		return IssueParams{}, err
	}
	out.Domain = domain
	if !strings.Contains(domain, ".") {
		// ACME 只给解析得到、且能从公网（或这台机器的 DNS）访问到的名字签发证书。
		// 单个标签（比如 localhost）不可能签发成功，与其让它去撞一次失败并消耗
		// 一次请求，不如在参数层说清楚。
		return IssueParams{}, invalidParams("domain", "ACME 签发需要一个带点的完整域名")
	}
	out.Email = strings.TrimSpace(out.Email)
	if out.Email != "" || !out.Scheduled {
		if !ValidEmail(out.Email) {
			return IssueParams{}, invalidParams("email", "ACME 账号邮箱不合法")
		}
	}
	return out, nil
}

// DisplayName 返回动作在任务列表里的显示名。
func (p IssueParams) DisplayName() string { return p.Domain }

// StateVersionOf 是预检绑定的状态版本。动作名进版本：签发与续期是两个不同的
// 计划，不能让一个计划的结果去满足另一个的执行。
func (p IssueParams) StateVersionOf(action string) string {
	return "certificate:" + p.Domain + ":" + action
}

// StateParams 是 certificate.state 的参数（只读查询）。
type StateParams struct {
	Domain string `json:"domain"`
	Source string `json:"source"`
}

// Normalize 校验查询参数。
func (p StateParams) Normalize() (StateParams, error) {
	out := p
	domain, err := normalizeDomain(out.Domain)
	if err != nil {
		return StateParams{}, err
	}
	out.Domain = domain
	out.Source = strings.TrimSpace(out.Source)
	if !Source(out.Source).Valid() {
		return StateParams{}, invalidParams("source", "证书来源只能是 certbot 或 manual")
	}
	return out, nil
}

// SecretResolver 是秘密存储的读取端（只有解析，没有列表）。
type SecretResolver interface {
	Resolve(ctx context.Context, ref string) ([]byte, error)
}

// Runner 是命令执行端（生产实现转给 syscmd.Run）。
type Runner interface {
	Run(ctx context.Context, spec syscmd.Spec, limits syscmd.Limits) (syscmd.Result, error)
}

// HTTPGet 是一次 HTTP 探测（HTTP-01 可达性检查用）。
//
// 做成函数类型而不是接口：它只有一个调用点，而测试要能替换它。
type HTTPGet func(ctx context.Context, url string) (status int, body string, err error)

// OpsOptions 是证书动作的装配参数。
type OpsOptions struct {
	// Secrets 必需：手动导入的私钥只存在于秘密存储里。
	Secrets SecretResolver
	// Runner 必需：签发与续期都要跑 certbot。
	Runner Runner
	// FS / Paths 是落盘接缝（零值走真实实现与生产路径）。
	FS    MaterialFS
	Paths MaterialPaths
	// Now 提供时间（证书是否过期、备份名）。
	Now func() time.Time
	// CertbotPath 是 certbot 的绝对路径（零值走 /usr/bin/certbot）。
	CertbotPath string
	// Webroot 是 HTTP-01 校验目录（零值走 /var/www/upanel-acme）。
	Webroot string
	// StagingDirectoryURL / ProductionDirectoryURL 是 ACME 目录地址。
	//
	// 默认是 Let's Encrypt 的两个入口。可覆盖只有一个实际用途：换一个私有 ACME
	// （企业内网 CA、或者验收用的本地测试服务器）。值来自 root 独占的配置文件，
	// 不来自 HTTP 参数——**面板只能选「staging 还是正式」，不能指定地址**。
	StagingDirectoryURL    string
	ProductionDirectoryURL string
	// HTTPGet 是 HTTP-01 可达性探测（零值走真实实现）。
	HTTPGet HTTPGet
	// RunnerAvailable 报告 certbot 此刻是否可用（零值：检查文件是否存在）。
	CertbotAvailable func() (bool, string)
}

// Ops 是证书动作的实现。
type Ops struct {
	opts OpsOptions
}

// NewOps 构造证书动作实现。缺必需项时返回错误：宁可启动时报错，
// 也不要让第一次点击返回一个说不清原因的失败。
func NewOps(opts OpsOptions) (*Ops, error) {
	if opts.Secrets == nil {
		return nil, errors.New("certificates: 缺少秘密存储读取端，手动导入无法完成")
	}
	if opts.Runner == nil {
		return nil, errors.New("certificates: 缺少命令执行端，签发与续期无法完成")
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.FS.ReadFile == nil {
		opts.FS = SystemMaterialFS()
	}
	return &Ops{opts: opts}, nil
}

func (o *Ops) webroot() string {
	if o.opts.Webroot == "" {
		return tlscert.ACMEWebroot
	}
	return o.opts.Webroot
}

func (o *Ops) paths() MaterialPaths { return o.opts.Paths }

func (o *Ops) now() time.Time {
	if o.opts.Now == nil {
		return time.Now()
	}
	return o.opts.Now()
}

// ---- 手动导入 ----

// AssessImport 计算一次手动导入的判决（预检调用，不写任何东西）。
func (o *Ops) AssessImport(ctx context.Context, in ImportParams) (proxies.Assessment, error) {
	p, err := in.Normalize()
	if err != nil {
		return proxies.Assessment{}, err
	}
	assess := proxies.Assessment{
		Action:              ActionImport,
		NormalizedParams:    p,
		TargetID:            p.CertificateID,
		DisplayName:         p.Domain,
		IdentityFingerprint: p.CertificateID,
		StateVersion:        p.StateVersionOf(),
		Locks:               []string{LockPrefixCert + p.Domain},
	}
	info, blockers := o.judgeCertPEM(ctx, []byte(p.CertPEM), p.Domain, p.QueryStaging())
	assess.Blockers = append(assess.Blockers, blockers...)
	if info == nil {
		assess.Allowed = len(assess.Blockers) == 0
		return assess, nil
	}

	assess.Impacts = append(assess.Impacts,
		proxies.Impact{
			Kind: "certificate_file",
			Description: "写入公钥证书链 " + path.Join(o.paths().certDir(), p.Domain+".pem") +
				"（0644，内容公开）",
		},
		proxies.Impact{
			Kind: "private_key_file",
			Description: "把秘密存储里的私钥写到 " + path.Join(o.paths().certDir(), p.Domain+".key") +
				"（0600 root：同组与其他用户都读不到，明文不经过日志和数据库）",
		},
		proxies.Impact{
			Kind: "certificate_binding",
			Description: "证书文件落地之后，还要在站点定义里绑定 " + p.CertificateID +
				" 并应用一次，Nginx 才会监听 443",
		},
	)
	assess.Allowed = len(assess.Blockers) == 0
	return assess, nil
}

// ExecuteImport 执行一次手动导入。
func (o *Ops) ExecuteImport(ctx context.Context, req proxies.WriteRequest) (proxies.WriteReport, error) {
	params, ok := req.Params.(ImportParams)
	if !ok {
		return proxies.WriteReport{}, fmt.Errorf("%w: certificate.import 的参数类型是 %T",
			ErrInvalidParams, req.Params)
	}
	p, err := params.Normalize()
	if err != nil {
		return proxies.WriteReport{}, err
	}
	now := o.now()

	// 私钥：解析引用 → 立刻算出配对结论 → 清零。明文只在这个函数的生命周期里存在。
	keyPEM, err := o.opts.Secrets.Resolve(ctx, p.KeySecretRef)
	if err != nil {
		return proxies.WriteReport{}, dependencyBlocked(CodeCertKeyMissing,
			"私钥引用 %s 解不开：%v", p.KeySecretRef, err)
	}
	defer zero(keyPEM)

	info, blockers := o.judgeCertPEM(ctx, []byte(p.CertPEM), p.Domain, p.Staging)
	if len(blockers) > 0 {
		return proxies.WriteReport{}, blockedBy(blockers)
	}
	if info == nil {
		return proxies.WriteReport{}, commandFailed("证书解析没有给出结论")
	}

	observed := map[string]any{
		"certificate_id": p.CertificateID,
		"domain":         p.Domain,
		"source":         string(SourceManual),
		"staging":        p.Staging,
		"issuer":         info.Issuer,
		"subject":        info.Subject,
		"domains":        info.Domains,
		"not_before":     info.NotBefore.UTC().Format(time.RFC3339),
		"not_after":      info.NotAfter.UTC().Format(time.RFC3339),
		"fingerprint":    info.Fingerprint,
		"chain_length":   info.ChainLength,
		"self_signed":    info.SelfSigned,
		"attempted_at":   now.UTC().Format(time.RFC3339),
	}

	outcome, err := materialize(o.opts.FS, o.paths(), []byte(p.CertPEM), keyPEM, p.Domain, now)
	if err != nil {
		observed["error"] = err.Error()
		return proxies.WriteReport{
			Outcome:  OutcomeUnknown,
			Observed: observed,
			RecoveryHint: "证书文件可能没有写到最终位置。检查 " +
				path.Join(o.paths().certDir(), p.Domain+".pem") + " 与同名 .key 是否存在且配对；" +
				"备份在 " + o.paths().backupDir() + "（若存在）",
		}, fmt.Errorf("%w: %v", ErrMaterializeFailed, err)
	}
	observed["cert_path"] = outcome.CertPath
	observed["key_path"] = outcome.KeyPath
	observed["key_mode"] = outcome.KeyMode
	if outcome.Backup != "" {
		observed["backup"] = outcome.Backup
	}

	warnings := []proxies.Blocker(nil)
	if info.Staging {
		warnings = append(warnings, proxies.Blocker{
			Code:    CodeCertStaging,
			Message: "这张证书来自 ACME 测试环境：浏览器不信任它，确认流程之后要重新用正式签发",
		})
	}
	if info.SelfSigned {
		warnings = append(warnings, proxies.Blocker{
			Code: CodeCertSelfSigned,
			Message: "这是自签名证书：" + "客户端要么手动信任它，要么会报证书错误。" +
				"它适合内网自用，不适合任何对外站点",
		})
	}
	if info.Expiring(now) {
		warnings = append(warnings, proxies.Blocker{
			Code: CodeCertExpired,
			Message: "这张证书在 " + info.NotAfter.UTC().Format(time.RFC3339) +
				" 到期（不足 21 天）：导入完就该安排续期",
		})
	}
	return proxies.WriteReport{
		Outcome:  OutcomeVerified,
		Observed: observed,
		Data: map[string]any{
			"cert_path":    outcome.CertPath,
			"key_path":     outcome.KeyPath,
			"fingerprint":  info.Fingerprint,
			"not_after":    info.NotAfter.UTC().Format(time.RFC3339),
			"warning_list": warningsOf(warnings),
		},
	}, nil
}

// ---- 签发与续期 ----

// targetIdentity 是预检绑定的目标身份。
//
// 手工签发/续期用证书 id：那是能复核到具体一条账本记录的东西。**定时续期没有 id**
// （触发发生在 systemd 里，那一刻没有面板可问，见 IssueParams.Scheduled 的注释），
// 目标就是那个域名本身——盘上 live/<domain> 那一份，除此之外没有第二个候选。
//
// 少了这个回退，定时触发在预检层就被判成"实现错误"而不是"被拒绝"：判决说
// allowed=true 却没有绑定，注册表按 internal/agent/preflight.go 的规则回
// 500 INTERNAL_ERROR，systemd 只看到单元失败，人到手的是一个数字。P12.5 第四轮
// 真机验收撞到的就是这个形状（journal 里是"定时触发的预检失败：缺少目标身份指纹"）。
func (p IssueParams) targetIdentity() string {
	if p.CertificateID != "" {
		return p.CertificateID
	}
	return "domain:" + p.Domain
}

// AssessIssue 计算一次签发/续期的判决。
func (o *Ops) AssessIssue(ctx context.Context, in IssueParams, action string) (proxies.Assessment, error) {
	p, err := in.Normalize()
	if err != nil {
		return proxies.Assessment{}, err
	}
	assess := proxies.Assessment{
		Action:              action,
		NormalizedParams:    p,
		TargetID:            p.CertificateID,
		DisplayName:         p.Domain,
		IdentityFingerprint: p.targetIdentity(),
		StateVersion:        p.StateVersionOf(action),
		Locks:               []string{LockACMEClient, LockPrefixCert + p.Domain},
	}

	server := o.serverURL(p.Staging)
	if ok, reason := o.certbotAvailable(); !ok {
		assess.Blockers = append(assess.Blockers, proxies.Blocker{
			Code:    CodeCertbotMissing,
			Message: "这台机器上没有可用的 certbot：" + reason,
		})
		assess.Allowed = false
		return assess, nil
	}
	// HTTP-01 可达性：这一步不做的话，最常见的失败（域名没指到这台机器、
	// 80 端口没通）会在 certbot 真的去申请时才暴露，而那时已经消耗了一次请求。
	if status, err := o.probeHTTP01(ctx, p.Domain); err != nil {
		assess.Blockers = append(assess.Blockers, proxies.Blocker{
			Code: CodeACMEReachability,
			Message: fmt.Sprintf("HTTP-01 校验目录对 http://%s/.well-known/acme-challenge/ 不可达（%s）："+
				"签发前必须让这个域名解析到本机、80 端口能访问，并且已经应用过一个带 ACME 校验位置的站点"+
				"（这台机器上还没有站点时，先建站点并应用一次）", p.Domain, err),
		})
		assess.Allowed = false
		return assess, nil
	} else if status != 200 {
		// 与 ExecuteIssue 同一个判据：执行路径对非 200 是拒绝（探测失败必须发生在
		// certbot 之前，见 certbot.go 文件头），预检就不能只给告警——否则用户看到的
		// 是"预检通过、带着一条警告"，点执行却必定被拒。
		assess.Blockers = append(assess.Blockers, proxies.Blocker{
			Code: CodeACMEReachability,
			Message: fmt.Sprintf("校验路径返回 %d（不是 200）：80 端口上回答的很可能是别的东西，"+
				"现在执行 certbot 只会白消耗一次请求", status),
		})
		assess.Allowed = false
		return assess, nil
	}

	where := "测试环境（staging）"
	if !p.Staging {
		where = "**正式环境**"
	}
	assess.Impacts = append(assess.Impacts,
		proxies.Impact{
			Kind:        "acme_order",
			Description: fmt.Sprintf("用 Certbot 向 %s 申请 %s 的证书（ACME 目录 %s）", where, p.Domain, server),
		},
		proxies.Impact{
			Kind: "certificate_file",
			Description: "证书由 Certbot 写到 " + path.Join(liveDirOf(p.Domain), "fullchain.pem") +
				"，私钥留在 " + path.Join(liveDirOf(p.Domain), "privkey.pem") +
				"（目录 0700 root，面板只读公钥，私钥从不经过面板与代理数据库）",
		},
	)
	if !p.Staging {
		assess.Warnings = append(assess.Warnings, proxies.Blocker{
			Code: "CERTIFICATE_PRODUCTION_ACME",
			Message: "这是**正式**签发：会消耗 Let's Encrypt 对这个域名的速率限额（每周 5 张）。" +
				"第一次请先用 staging 走通流程",
		})
	}
	assess.Allowed = len(assess.Blockers) == 0
	return assess, nil
}

// forceRenewal 报告这一次要不要让 certbot 无条件重签。
//
// 只有"人在面板上点续期"那一下要：他要的是立刻重签，certbot 的"还没到期、
// 跳过"会让按钮看起来没反应。签发与**定时**续期都用 --keep-until-expiring——
// 一张还没到期的证书不会被无谓地重签，这正是速率限额的用法（每晚
// --force-renewal，一周就能把 Let'"'"'s Encrypt 对某个域名的限额烧光）。
func forceRenewal(action string, p IssueParams) bool {
	return action == ActionRenew && !p.Scheduled
}

// ExecuteIssue 执行一次签发或续期。
func (o *Ops) ExecuteIssue(ctx context.Context, req proxies.WriteRequest, action string) (proxies.WriteReport, error) {
	params, ok := req.Params.(IssueParams)
	if !ok {
		return proxies.WriteReport{}, fmt.Errorf("%w: %s 的参数类型是 %T",
			ErrInvalidParams, action, req.Params)
	}
	p, err := params.Normalize()
	if err != nil {
		return proxies.WriteReport{}, err
	}
	if ok, reason := o.certbotAvailable(); !ok {
		return proxies.WriteReport{}, dependencyBlocked(CodeCertbotMissing,
			"这台机器上没有可用的 certbot：%s", reason)
	}
	if status, err := o.probeHTTP01(ctx, p.Domain); err != nil || status != 200 {
		msg := fmt.Sprintf("HTTP-01 校验目录不可达（状态 %d）", status)
		if err != nil {
			msg = "HTTP-01 校验目录不可达：" + err.Error()
		}
		return proxies.WriteReport{}, dependencyBlocked(CodeACMEReachability, "%s", msg)
	}

	now := o.now()
	force := forceRenewal(action, p)
	certPath, keyPath, result, err := o.runCertbot(ctx, p, force)
	if err != nil {
		observed := map[string]any{
			"certificate_id": p.CertificateID,
			"domain":         p.Domain,
			"staging":        p.Staging,
			"server":         o.serverURL(p.Staging),
			"exit_code":      result.ExitCode,
			"stderr_tail":    tail(result.Stderr, 1500),
			"attempted_at":   now.UTC().Format(time.RFC3339),
		}
		return proxies.WriteReport{
			Outcome:      OutcomeUnknown,
			Observed:     observed,
			RecoveryHint: "Certbot 没有成功。上面 stderr_tail 是它自己的最后一段输出；常见原因：域名没有解析到本机、80 端口不通、速率限额、邮箱无效。",
		}, certbotFailure{
			err:    fmt.Errorf("%w: %v", ErrCertbotFailed, err),
			reason: oneLine(err.Error(), 300),
		}
	}

	raw, err := o.opts.FS.ReadFile(certPath)
	if err != nil {
		return proxies.WriteReport{}, commandFailed("Certbot 报告成功，但读不到证书 %s：%v", certPath, err)
	}
	info, err := tlscert.ParseCertificate(raw)
	if err != nil {
		return proxies.WriteReport{}, commandFailed("Certbot 写下的证书解析失败：%v", err)
	}
	if !tlscert.CoversDomain(info, p.Domain) {
		return proxies.WriteReport{}, commandFailed("Certbot 签下的证书不覆盖 %s（覆盖的是 %v）",
			p.Domain, info.Domains)
	}
	keyStat, statErr := o.opts.FS.Stat(keyPath)
	observed := map[string]any{
		"certificate_id": p.CertificateID,
		"domain":         p.Domain,
		"source":         string(SourceCertbot),
		"staging":        p.Staging,
		"server":         o.serverURL(p.Staging),
		"cert_path":      certPath,
		"key_path":       keyPath,
		"issuer":         info.Issuer,
		"subject":        info.Subject,
		"domains":        info.Domains,
		"not_before":     info.NotBefore.UTC().Format(time.RFC3339),
		"not_after":      info.NotAfter.UTC().Format(time.RFC3339),
		"fingerprint":    info.Fingerprint,
		"chain_length":   info.ChainLength,
		"self_signed":    info.SelfSigned,
		"attempted_at":   now.UTC().Format(time.RFC3339),
		"exit_code":      result.ExitCode,
	}
	if statErr == nil {
		// 只 stat，不读：私钥的内容没有任何理由进入这个进程。
		observed["key_mode"] = fmt.Sprintf("%04o", keyStat.Mode().Perm())
	}
	warnings := []proxies.Blocker(nil)
	if info.Staging {
		warnings = append(warnings, proxies.Blocker{
			Code:    CodeCertStaging,
			Message: "这是 staging 证书：浏览器不信任它。流程确认之后，用同一个域名再签发一次并选择正式环境",
		})
	}
	if info.Expiring(now) {
		warnings = append(warnings, proxies.Blocker{
			Code:    CodeCertExpired,
			Message: "刚签下的证书已进入 21 天临期窗口：这通常意味着测试 CA 的有效期很短（staging 3 个月、Pebble 更短）",
		})
	}
	return proxies.WriteReport{
		Outcome:  OutcomeVerified,
		Observed: observed,
		Data: map[string]any{
			"cert_path":    certPath,
			"key_path":     keyPath,
			"fingerprint":  info.Fingerprint,
			"not_after":    info.NotAfter.UTC().Format(time.RFC3339),
			"warning_list": warningsOf(warnings),
		},
	}, nil
}

// ---- 只读状态 ----

// Inspect 读盘上一张证书的事实（不写任何东西）。
//
// 它存在的理由：certbot 包自带一个 systemd timer，它会在后台续期
// /etc/letsencrypt/renewal 下的谱系——那份文件可能比面板数据库里的记录新。
// 让详情页能问一句「盘上现在是什么样」，比让面板显示一个可能过期的到期时间诚实。
func (o *Ops) Inspect(ctx context.Context, in StateParams) (map[string]any, error) {
	p, err := in.Normalize()
	if err != nil {
		return nil, err
	}
	certPath, keyPath := o.pathsOf(p.Domain, Source(p.Source))
	out := map[string]any{
		"domain":    p.Domain,
		"source":    p.Source,
		"cert_path": certPath,
		"key_path":  keyPath,
	}
	raw, err := o.opts.FS.ReadFile(certPath)
	if err != nil {
		out["present"] = false
		out["reason"] = "读不到证书文件：" + err.Error()
		return out, nil
	}
	info, err := tlscert.ParseCertificate(raw)
	if err != nil {
		out["present"] = true
		out["reason"] = "证书文件解析失败：" + err.Error()
		return out, nil
	}
	now := o.now()
	out["present"] = true
	out["issuer"] = info.Issuer
	out["subject"] = info.Subject
	out["domains"] = info.Domains
	out["not_before"] = info.NotBefore.UTC().Format(time.RFC3339)
	out["not_after"] = info.NotAfter.UTC().Format(time.RFC3339)
	out["fingerprint"] = info.Fingerprint
	out["chain_length"] = info.ChainLength
	out["staging"] = info.Staging
	out["self_signed"] = info.SelfSigned
	out["expired"] = info.Expired(now)
	out["expiring"] = info.Expiring(now)
	if fi, err := o.opts.FS.Stat(keyPath); err == nil {
		out["key_present"] = true
		out["key_mode"] = fmt.Sprintf("%04o", fi.Mode().Perm())
	} else {
		out["key_present"] = false
	}
	return out, nil
}

// pathsOf 返回一条记录对应的证书与私钥路径（与面板渲染时用的是同一套规则）。
func (o *Ops) pathsOf(domain string, source Source) (string, string) {
	if source == SourceCertbot {
		dir := liveDirOf(domain)
		return path.Join(dir, "fullchain.pem"), path.Join(dir, "privkey.pem")
	}
	dir := o.paths().certDir()
	return path.Join(dir, domain+".pem"), path.Join(dir, domain+".key")
}

// liveDirOf 是 certbot 为一个域名建的谱系目录。
func liveDirOf(domain string) string { return path.Join(tlscert.LiveDir, domain) }

// judgeCertPEM 检查一份证书 PEM 能不能用在这个域名上。
//
// 返回 nil Info 表示「解析都没通过」，此时 blockers 非空。
func (o *Ops) judgeCertPEM(ctx context.Context, pemBytes []byte, domain string, staging bool) (*tlscert.Info, []proxies.Blocker) {
	var blockers []proxies.Blocker
	if tlscert.ContainsPrivateKey(pemBytes) {
		blockers = append(blockers, proxies.Blocker{
			Code: CodeCertPEMInvalid,
			Message: "上传的内容里夹带了私钥：这个接口只收**公钥证书**。" +
				"私钥请用 POST /secrets（用途 tls_private_key）单独上传，它不进面板数据库",
		})
		return nil, blockers
	}
	info, err := tlscert.ParseCertificate(pemBytes)
	if err != nil {
		blockers = append(blockers, proxies.Blocker{
			Code:    CodeCertPEMInvalid,
			Message: "证书解析失败：" + err.Error(),
		})
		return nil, blockers
	}
	if info.IsCA {
		blockers = append(blockers, proxies.Blocker{
			Code: CodeCertIsCA,
			Message: "这是一张 CA 证书（基本约束 CA=true），不能当站点证书用：" +
				"要上传的是签发链里属于这个域名的那一张（叶子）",
		})
	}
	if !tlscert.CoversDomain(info, domain) {
		blockers = append(blockers, proxies.Blocker{
			Code: CodeCertNotCovering,
			Message: fmt.Sprintf("证书覆盖的是 %v，不包含 %s：装上去浏览器一定会拒绝",
				info.Domains, domain),
		})
	}
	if info.Expired(o.now()) {
		blockers = append(blockers, proxies.Blocker{
			Code:    CodeCertExpired,
			Message: "这张证书已于 " + info.NotAfter.UTC().Format(time.RFC3339) + " 过期，导入它没有意义",
		})
	}
	if info.Staging && !staging {
		blockers = append(blockers, proxies.Blocker{
			Code:    CodeCertStaging,
			Message: "这张证书的签发者看起来是 ACME 测试环境，但你声明的是正式证书：请核对来源",
		})
	}
	if len(blockers) > 0 {
		return &info, blockers
	}
	return &info, nil
}

// certbotAvailable 报告 certbot 此刻能不能用。
func (o *Ops) certbotAvailable() (bool, string) {
	if o.opts.CertbotAvailable != nil {
		return o.opts.CertbotAvailable()
	}
	p := o.opts.CertbotPath
	if p == "" {
		p = tlscert.CertbotPath
	}
	fi, err := os.Stat(p)
	if err != nil {
		return false, "找不到 " + p + "（" + err.Error() + "）"
	}
	if fi.IsDir() {
		return false, p + " 是目录"
	}
	if fi.Mode().Perm()&0o111 == 0 {
		return false, p + " 没有可执行权限"
	}
	return true, ""
}

// warningsOf 把告警列表变成可序列化的形状。
func warningsOf(list []proxies.Blocker) []map[string]any {
	out := make([]map[string]any, 0, len(list))
	for _, b := range list {
		out = append(out, map[string]any{"code": b.Code, "message": b.Message})
	}
	return out
}

// tail 取一段输出的末尾。certbot 的报错在最后几行，而它的日志可以很长。
func tail(s string, n int) string {
	if len(s) <= n {
		return strings.TrimSpace(s)
	}
	return "…" + strings.TrimSpace(s[len(s)-n:])
}

// oneLine 把多行原因压成一行：error.message 在界面上按一行显示，换行会被吃掉，
// 而 certbot 的 stderr 是多行的。
func oneLine(text string, limit int) string {
	if fields := strings.Fields(text); len(fields) > 0 {
		text = strings.Join(fields, " ")
	}
	return truncateRunes(text, limit)
}

// certbotFailure 把 certbot 已经核实过的原因带进任务的 error.message。
//
// 为什么需要它：注册表（internal/agent/runner.go）在动作失败时只写 error.message，
// 失败任务的 result 按契约是空对象——报告里的 stderr_tail 到不了管理员眼前。没有
// FailureReason()，面板与账本上留下的就是一句"动作执行失败"：一次每晚都在失败的
// 定时续期，界面上看不出是 80 端口不通、账号失效还是速率限额。
//
// 这个接口是结构性的（agent 侧用 errors.As 找 FailureReason() string），所以这里
// 不必反向依赖 internal/agent。
type certbotFailure struct {
	err    error
	reason string
}

func (f certbotFailure) Error() string { return f.err.Error() }

func (f certbotFailure) Unwrap() error { return f.err }

// FailureReason 是 agent 侧 FailureReasoner 要的那一句人话。
func (f certbotFailure) FailureReason() string { return f.reason }

// QueryStaging 让导入参数的 staging 声明参与判决（证书自己说了算的部分在
// judgeCertPEM 里，这里只把用户的声明带过去）。
func (p ImportParams) QueryStaging() bool { return p.Staging }
