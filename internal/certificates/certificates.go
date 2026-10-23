// Package certificates 维护站点用的 TLS 证书（P10.3）：Certbot 签发、手动导入、
// 到期信息、续期与私钥保护。
//
// 三条边界，与 proxies 同一套思路：
//
//  1. **私钥不经过面板。** 手动导入时私钥走秘密通道（POST /secrets，用途
//     tls_private_key），由代理解密后物化成 0600 root 的文件；Certbot 签发的私钥
//     自始至终在 /etc/letsencrypt 下，面板连读都不读。本包的记录里只有路径。
//  2. **状态是算出来的**（Q286）。库里存的是证书自身的事实（issuer、not_after、
//     指纹），不是“现在有效/临期”这个结论——那个结论每秒都可能变。
//  3. **证书与站点是显式绑定。** `proxies.certificate_id` 为空就只监听 80，非空才
//     渲染 443。按域名自动匹配省一次点击，却让“换了一张证书”和“换了绑定”变成
//     同一件事，出问题时无法区分。
//
// 本包不执行任何系统命令、不碰文件系统：签发与物化在代理侧（P10.3 的
// internal/agent）。面板侧只做记录、校验与派生。
package certificates

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	"upanel/internal/proxies"
	"upanel/internal/tlscert"
)

// Source 是证书的来路（闭集，与 0010 迁移的 CHECK 一致）。
type Source string

const (
	// SourceCertbot 表示由 ACME（Certbot）签发。
	SourceCertbot Source = "certbot"
	// SourceManual 表示管理员手动导入。
	SourceManual Source = "manual"
)

// Valid 判断来路是否在闭集内。
func (s Source) Valid() bool { return s == SourceCertbot || s == SourceManual }

// RenewalState 是“这张证书此刻算不算还能用”的派生结论（四态闭集）。
//
// 与 proxies.AppliedState 同一套理由：它每次读都重算，不落库。
type RenewalState string

const (
	// RenewalValid 表示有效期还长（超过 21 天）。
	RenewalValid RenewalState = "valid"
	// RenewalExpiring 表示已进入临期窗口（≤ 21 天，含已过期）。
	RenewalExpiring RenewalState = "expiring"
	// RenewalExpired 表示已经过期。
	RenewalExpired RenewalState = "expired"
	// RenewalUnknown 表示证书事实不完整，算不出来（只在数据被手工改坏时出现）。
	RenewalUnknown RenewalState = "unknown"
)

// 路径常量。
//
// 两套目录是两套信任边界：
//
//	/etc/ssl/upanel/          手动导入。目录 0700 root，证书 0644，私钥 0600。
//	/etc/letsencrypt/live/    Certbot。它是 certbot 自己的目录（0700 root），
//	                          面板只读其中的 fullchain.pem（公钥），私钥不读。
const (
	// DefaultCertDir 是手动导入证书的落地目录。
	DefaultCertDir = tlscert.CertDir
	// DefaultLiveDir 是 Certbot 的 live 目录（每个域名一个子目录）。
	DefaultLiveDir = tlscert.LiveDir
	// DefaultACMEWebroot 是 HTTP-01 校验文件的根目录。
	//
	// 托管站点总会渲染一个 `location ^~ /.well-known/acme-challenge/` 指向它，
	// 所以续期不需要临时改配置：证书过期前那次续期要能被访问，配置就必须一直
	// 留着这条路。
	DefaultACMEWebroot = tlscert.ACMEWebroot
	// DefaultCertbotPath 是 Certbot 可执行文件的绝对路径。
	DefaultCertbotPath = tlscert.CertbotPath

	// ACMEDirectoryProduction / ACMEDirectoryStaging 是 Let's Encrypt 的两个入口。
	//
	// **staging 是默认选项**：正式签发有速率限制（同一域名每周 5 张），一次调错
	// 参数就能把一个域名卡住一周。测试永远先走 staging（PLAN.md 第 13 节）。
	ACMEDirectoryProduction = "https://acme-v02.api.letsencrypt.org/directory"
	ACMEDirectoryStaging    = "https://acme-staging-v02.api.letsencrypt.org/directory"

	// AttemptCooldown 是同一域名两次签发尝试的最小间隔。
	//
	// 它挡的不是“手滑点两次”（那有任务锁），而是**循环重试**：一个失败的签发被
	// 反复重排，每次都会消耗一次 ACME 请求，正式环境里就是速率限额。一小时是
	// 一个既有意义又不至于让人等太久的间隔。
	AttemptCooldown = time.Hour

	// MaxACMEEmailLen 是邮箱长度上限（与 0010 迁移一致）。
	MaxACMEEmailLen = 254

	// DefaultListLimit / MaxListLimit 与 CONTRACTS.md 的分页一致。
	DefaultListLimit = 50
	MaxListLimit     = 200

	certIDPrefix = "crt_"
	idHexLen     = 32
)

// 包级错误。业务错误是 4xx，系统错误是 500 且只进日志。
var (
	// ErrNotFound 表示证书记录不存在。
	ErrNotFound = errors.New("certificates: 证书不存在")
	// ErrDomainTaken 表示这个域名已经有一条证书记录。
	ErrDomainTaken = errors.New("certificates: 该域名已有证书记录")
	// ErrVersionConflict 表示版本号不一致。
	ErrVersionConflict = errors.New("certificates: 版本不一致，已被其他人修改")
	// ErrInUse 表示还有站点绑着这张证书。
	ErrInUse = errors.New("certificates: 证书仍被站点使用")
	// ErrInvalidLimit 表示 limit 越界。
	ErrInvalidLimit = errors.New("certificates: 列表条数必须在 1 到 200 之间")
	// ErrInvalidCursor 表示翻页游标无效。
	ErrInvalidCursor = errors.New("certificates: 翻页游标无效")
	// ErrNotCovered 表示证书不覆盖某个域名。
	ErrNotCovered = errors.New("certificates: 证书不覆盖该域名")
	// ErrExpired 表示证书已经过期。绑一张过期证书能得到的最多是浏览器警告，
	// 所以它在绑定路径上是拒绝理由，不是提示。
	ErrExpired = errors.New("certificates: 证书已过期")
)

// ValidationError 是字段级不合法（400 VALIDATION_ERROR），与 proxies 同形。
type ValidationError struct {
	Field   string
	Message string
}

func (e *ValidationError) Error() string { return e.Message }

func invalid(field, format string, args ...any) error {
	return &ValidationError{Field: field, Message: fmt.Sprintf(format, args...)}
}

// Certificate 是一条证书记录（不含私钥内容，也没有“当前状态”结论）。
type Certificate struct {
	ID string
	// Domain 是这张证书绑定的域名（已规范化 A-label，唯一）。
	Domain string
	// Domains 是证书实际覆盖的域名（SAN + CN）。
	Domains []string
	// Source 是来路：certbot / manual。
	Source Source
	// Staging 表示 ACME 测试环境签发（浏览器不认，界面上必须显眼）。
	Staging bool
	// Issuer 是签发者可读名。
	Issuer string
	// Subject 是持有者可读名。
	Subject string
	// NotBefore / NotAfter 是有效期。
	NotBefore time.Time
	NotAfter  time.Time
	// Fingerprint 是整张 DER 的 SHA-256。
	Fingerprint string
	// CertPath / KeyPath 是主机上的绝对路径（nginx 用的就是这两个值）。
	CertPath string
	KeyPath  string
	// KeySecretRef 是手动导入时私钥在秘密存储里的引用；Certbot 签发时为空。
	KeySecretRef string
	// ACMEEmail 是 Certbot 账号邮箱；手动导入时为空。
	ACMEEmail string
	// LastAttemptAt 是上一次签发/续期尝试的时间（限流依据）。
	LastAttemptAt time.Time
	// LastError 是上一次失败的原因（成功后清空）。
	LastError string

	Version   int
	CreatedAt time.Time
	UpdatedAt time.Time
}

// State 返回这条证书此刻的派生状态。
//
// now 由调用方给：同一个请求里所有证书用同一个“现在”，否则一页列表里可能出现
// 两条证书对“同一秒”给出不同的判断。
func (c Certificate) State(now time.Time) RenewalState {
	if c.NotAfter.IsZero() || c.Fingerprint == "" {
		return RenewalUnknown
	}
	if !now.Before(c.NotAfter) {
		return RenewalExpired
	}
	if !now.Add(tlscert.ExpiringWindow).Before(c.NotAfter) {
		return RenewalExpiring
	}
	return RenewalValid
}

// CoversDomain 判断这张证书（按记录里的覆盖列表）是否覆盖某个域名。
func (c Certificate) CoversDomain(domain string) bool {
	return tlscert.CoversDomain(tlscert.Info{Domains: c.Domains}, domain)
}

// Paths 描述证书与私钥各放在哪儿。
type Paths struct {
	// CertDir 是手动导入的落地目录。
	CertDir string
	// LiveDir 是 Certbot 的 live 目录。
	LiveDir string
}

// WithDefaults 补齐零值。
func (p Paths) WithDefaults() Paths {
	if p.CertDir == "" {
		p.CertDir = DefaultCertDir
	}
	if p.LiveDir == "" {
		p.LiveDir = DefaultLiveDir
	}
	return p
}

// CertPathOf 返回某个域名在指定来路下的证书路径。
//
// 路径是**约定**而不是参数：它必须与代理物化时用的路径一致，也必须与该来路下
// 事实存在的位置一致（Certbot 的 live 目录布局是 certbot 自己定的，我们不该另存一份）。
func (p Paths) CertPathOf(domain string, source Source) string {
	p = p.WithDefaults()
	if source == SourceCertbot {
		return path.Join(p.LiveDir, domain, "fullchain.pem")
	}
	return path.Join(p.CertDir, domain+".pem")
}

// KeyPathOf 返回某个域名在指定来路下的私钥路径。
func (p Paths) KeyPathOf(domain string, source Source) string {
	p = p.WithDefaults()
	if source == SourceCertbot {
		return path.Join(p.LiveDir, domain, "privkey.pem")
	}
	return path.Join(p.CertDir, domain+".key")
}

// CertbotArgs 组装一次 Certbot 调用的参数（**固定模板**，不接受调用方传入的任意参数）。
//
// 只用 certonly + webroot：
//
//   - 不用 nginx 插件：它会自己改 /etc/nginx 的配置。改配置是本产品的核心动作，
//     必须走“渲染 → 原子写 → nginx -t → 回滚”那条流水线（P10.2），不能交给另一个
//     程序在黑盒里做一遍。
//   - --webroot 只是把校验文件写进目录，由我们自己的 server 块提供服务。
//   - --non-interactive 与 --agree-tos：没有 TTY，也没有“同意条款”这一步的交互。
//     --email 来自记录（Certbot 账号需要它）。
//
// serverURL 永远由调用方从两个常量里选（staging 或正式），不接受任意 URL 作为
// 参数——那是“任意 Certbot 参数”的另一种写法。
func CertbotArgs(certbotPath, webroot, domain, email, serverURL string, force bool) (string, []string) {
	args := []string{
		"certonly",
		"--non-interactive",
		"--agree-tos",
		"--email", email,
		"--webroot",
		"--webroot-path", webroot,
		"--cert-name", domain,
		"--domain", domain,
		"--server", serverURL,
		"--keep-until-expiring",
	}
	if force {
		args = append(args, "--force-renewal")
	}
	return certbotPath, args
}

// ServerURL 把“是不是 staging”映射成 ACME 目录地址。
//
// 两个常量之外没有其它取值：`--server` 由这里决定，调用方没有机会塞进一个任意 URL。
// overrides 只在代理配置显式覆盖时非空（私有 ACME / 验收用的本地测试 CA），
// 覆盖值必须是 https 或 http 的绝对 URL，由调用方在配置层校验。
func ServerURL(staging bool, productionOverride, stagingOverride string) string {
	if staging {
		if stagingOverride != "" {
			return stagingOverride
		}
		return ACMEDirectoryStaging
	}
	if productionOverride != "" {
		return productionOverride
	}
	return ACMEDirectoryProduction
}

// NewID 生成证书标识（crt_ + 32 位小写十六进制）。
func NewID() (string, error) {
	buf := make([]byte, idHexLen/2)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("certificates: 生成标识失败: %w", err)
	}
	return certIDPrefix + hex.EncodeToString(buf), nil
}

// ValidID 判断标识形状。
func ValidID(id string) bool {
	if len(id) != len(certIDPrefix)+idHexLen || !strings.HasPrefix(id, certIDPrefix) {
		return false
	}
	for _, r := range id[len(certIDPrefix):] {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// ValidEmail 做一次宽松但明确的邮箱校验。
//
// 不做 RFC 5322 的全量解析：真正用这个地址的是 ACME 服务器，它自己会拒。这里只挡
// “明显不是邮箱”的输入（没有 @、有空格、长度超限），免得把一次签发请求变成一次
// 不必要的往返。
func ValidEmail(email string) bool {
	if email == "" || len(email) > MaxACMEEmailLen {
		return false
	}
	if strings.ContainsAny(email, " \t\r\n<>\"'\\") {
		return false
	}
	at := strings.LastIndex(email, "@")
	if at <= 0 || at == len(email)-1 {
		return false
	}
	domain := email[at+1:]
	if !strings.Contains(domain, ".") || strings.HasPrefix(domain, ".") || strings.HasSuffix(domain, ".") {
		return false
	}
	return true
}

// Material 把一条证书记录变成渲染与执行需要的材料（P10.3）。
//
// 它只取**路径与事实**：私钥内容不在这条记录里，也不该在这里被拼出来。渲染出来的
// Nginx 配置里只有 ssl_certificate_key 指向的那个路径。
//
// 不在这里判断「能不能用」（覆盖不覆盖、过没过期）：那是判决，判决属于
// proxy.apply 的现场检查——它读的是磁盘上的文件，而记录可能已经落后于磁盘。
func (c Certificate) Material() proxies.TLSMaterial {
	return proxies.TLSMaterial{
		CertificateID: c.ID,
		Source:        string(c.Source),
		Domain:        c.Domain,
		CertPath:      c.CertPath,
		KeyPath:       c.KeyPath,
		Fingerprint:   c.Fingerprint,
		Issuer:        c.Issuer,
		NotAfter:      c.NotAfter.UTC().Format(time.RFC3339),
		Staging:       c.Staging,
	}
}
