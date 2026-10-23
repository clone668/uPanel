// Package proxies 维护反向代理站点（P10.1）：一个独立域名指向一个明确上游，
// 外加访问策略、请求体上限、读取超时与 WebSocket 开关。
//
// 这一层只做**模型与判断**：站点该长什么样、这个域名现在能不能用。它不生成 Nginx
// 配置、不写文件、不 reload——那些是 P10.2 的任务链路（预检 → 任务 → nginx -t →
// reload → 实测入口）。分界线在这里，因为“判断”可以随时重算，而“写下去”一旦发生
// 就必须能回滚。
//
// 本包负责守住四条规则：
//
//  1. **一个域名一个 server 块，一个 server 块一个上游。** 域名在库里是唯一索引：
//     两个同名记录会生成两个 server_name 相同的 server 块，而 nginx 只用第一个——
//     第二个看起来存在、实际上从没生效。这种“配置写对了但不生效”比写错更难查。
//  2. **域名必须能落成文件名。** 归一化成小写 A-label 之后才入库，因为 P10.2 要把
//     它拼进 /etc/nginx/conf.d/upanel-<domain>.conf。带路径分隔符或通配符的输入在
//     这一步就被挡掉，而不是等写文件时才发现。
//  3. **冲突是查出来的，不是问出来的。** 域名是否已被占用，靠读主机上现有的 Nginx
//     配置来判断（internal/nginx），而不是让用户自己保证“没冲突”。读不到 Nginx
//     就不能写：无法判断不等于没有冲突。
//  4. **管理面板自己的端口不是上游。** 上游端口等于面板端口一律拒绝，否则“给某个
//     域名配个反代”就能配出一个把 9443 暴露出去的公网入口。
//
// 上游目标只接受 local_ / cntr_ 形状的标识（与 internal/apps 的健康检查目标同一套
// 形状）：主机名、IP、URL、任意端口在这里都表达不出来，因此“代理到 169.254.169.254”
// 这种输入在数据模型层面就写不进库。
package proxies

import (
	"errors"
	"fmt"
	"time"
)

// AccessMode 是站点的访问模式（PLAN.md 第 13 节：public、CIDR allowlist、basic_auth）。
type AccessMode string

const (
	// AccessPublic 表示谁都能访问。
	AccessPublic AccessMode = "public"
	// AccessCIDR 表示只允许 allowed_cidrs 里的来源。
	AccessCIDR AccessMode = "cidr"
	// AccessBasicAuth 表示需要 HTTP Basic 认证（与应用自己的账号无关）。
	AccessBasicAuth AccessMode = "basic_auth"
)

// Valid 判断访问模式是否在固定集合内。
func (m AccessMode) Valid() bool {
	switch m {
	case AccessPublic, AccessCIDR, AccessBasicAuth:
		return true
	}
	return false
}

// 上流协议。
const (
	SchemeHTTP  = "http"
	SchemeHTTPS = "https"
)

// 字段上限与默认值。
//
// 数值与 migrations/0009_proxies.sql 的 CHECK 约束一一对应：库层约束是最后一道，
// 不是唯一一道。默认值取 nginx 自己 client_max_body_size / proxy_read_timeout 的
// 默认（1m / 60s），这样“面板没改过”和“nginx 默认”是同一件事。
const (
	MaxDomainLen        = 253
	MaxAllowedCIDRs     = 32
	MaxBasicAuthUsers   = 32
	MaxBasicAuthNameLen = 64

	MinMaxBodyBytes = 1 << 10 // 1 KiB
	MaxMaxBodyBytes = 1 << 30 // 1 GiB

	MinReadTimeoutSeconds = 1
	MaxReadTimeoutSeconds = 3600

	DefaultMaxBodyBytes       = 1 << 20 // 与 nginx 的 1m 一致
	DefaultReadTimeoutSeconds = 60      // 与 nginx 的 60s 一致

	// DefaultListLimit / MaxListLimit 与 CONTRACTS.md 第 1 节的分页一致。
	DefaultListLimit = 50
	MaxListLimit     = 200

	siteIDPrefix = "site_"
	idHexLen     = 32
)

// 包级错误。业务错误与系统错误分开：前者是 4xx，后者是 500 且只进日志。
var (
	// ErrNotFound 表示站点不存在（或已被删除）。
	ErrNotFound = errors.New("proxies: 站点不存在")
	// ErrDomainTaken 表示这个域名已经有记录了。
	ErrDomainTaken = errors.New("proxies: 该域名已有代理站点")
	// ErrVersionConflict 表示写入的版本号与库中不一致。
	ErrVersionConflict = errors.New("proxies: 版本不一致，已被其他人修改")
	// ErrInvalidLimit 表示 limit 超出范围。
	ErrInvalidLimit = errors.New("proxies: 列表条数必须在 1 到 200 之间")
	// ErrInvalidCursor 表示游标不是本服务签发的。
	ErrInvalidCursor = errors.New("proxies: 翻页游标无效")
	// ErrFactsUnavailable 表示读不到 Nginx 现状，因此无法判断域名冲突。
	//
	// 这种情况下**不写库**：读不到现有配置就写一份新配置，等于在不知道已经有什么
	// 的机器上动手。宁可让用户重试。
	ErrFactsUnavailable = errors.New("proxies: 读不到 Nginx 现状，无法判断域名冲突")
)

// ValidationError 表示调用方传入的字段不合法，接口层映射为 400 VALIDATION_ERROR。
type ValidationError struct {
	// Field 是出错的字段名（与 JSON 字段同名），前端可直接高亮。
	Field string
	// Message 是中文可读说明，不含内部细节。
	Message string
}

func (e *ValidationError) Error() string { return e.Message }

func invalid(field, format string, args ...any) error {
	return &ValidationError{Field: field, Message: fmt.Sprintf(format, args...)}
}

// BasicAuthUser 是一个访问账号：名字 + 指向安全存储里密码哈希的引用。
//
// 密码本身从不进这条记录，也不进日志：库里只有名字与 sec_ 引用。UI 上必须讲清楚
// 这是**代理层的**账号，与应用自己的登录体系没有关系（PLAN.md 第 13 节）。
type BasicAuthUser struct {
	// Name 是用户名，1—64 个 [a-z0-9._-]，首字符为字母或数字。
	Name string
	// SecretRef 是密码哈希在安全存储里的引用（sec_ + 32 位小写十六进制）。
	SecretRef string
}

// Definition 是站点本体（不含 id、version 与时间）。
//
// 创建与修改都以它为单位校验：过一次 Normalize 之后才是可入库的形态，默认值、
// 归一化、去重都在那一步完成，存储层不再二次解释。
type Definition struct {
	// AppID 是关联的应用（可空：也可以纯粹给一个目标配代理）。
	AppID string
	// Domain 是访问用的域名，归一化后的 A-label。
	Domain string
	// UpstreamTargetID 是上游目标标识（local_… / cntr_…）。
	UpstreamTargetID string
	// UpstreamScheme 只允许 http / https。
	UpstreamScheme string
	// UpstreamPort 是上游端口，1—65535，且不得等于面板端口。
	UpstreamPort int
	// AccessMode 决定谁能访问。
	AccessMode AccessMode
	// AllowedCIDRs 是 CIDR 白名单（AccessCIDR 时非空，其它模式必须为空）。
	AllowedCIDRs []string
	// BasicAuthUsers 是访问账号（AccessBasicAuth 时非空，其它模式必须为空）。
	BasicAuthUsers []BasicAuthUser
	// WebSocketEnabled 表示放行 Upgrade 头与长连接超时。
	WebSocketEnabled bool
	// MaxBodyBytes 是请求体上限。
	MaxBodyBytes int64
	// ReadTimeoutSeconds 是上游读取超时。
	ReadTimeoutSeconds int
	// Enabled 表示这个站点是否生效。P10.1 只存这个意图，写配置是 P10.2。
	Enabled bool
}

// Site 是一条站点记录。
type Site struct {
	ID string
	Definition
	// Version 是乐观锁版本号，每次成功修改加一。
	Version   int
	CreatedAt time.Time
	UpdatedAt time.Time
}

// NewSiteID 生成站点标识（site_ + 32 位小写十六进制）。
func NewSiteID() (string, error) { return newID() }
