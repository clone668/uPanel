package proxies

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/netip"
	"regexp"
	"sort"
	"strings"

	"golang.org/x/net/idna"

	"upanel/internal/apps"
)

// NormalizeOptions 是校验时的环境开关。
type NormalizeOptions struct {
	// PanelPort 是管理面板自己监听的端口（默认 9443）。上游端口等于它一律拒绝：
	// 配一个反代就把管理面板暴露到公网上，这是最容易犯也最不该犯的错。
	// 0 表示不检查——只允许在没有面板端口的测试里用。
	PanelPort int
}

var (
	siteIDPattern = regexp.MustCompile(`^site_[0-9a-f]{32}$`)

	// basicAuthNamePattern：htpasswd 的用户名，只允许小写字母数字与 . _ -。
	basicAuthNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

	// secretRefPattern：安全存储里的秘密引用（sec_ + 32 位小写十六进制）。
	// 与 internal/shares 用的是同一套形状。
	secretRefPattern = regexp.MustCompile(`^sec_[0-9a-f]{32}$`)

	// domainLabelPattern：DNS 标签。IDNA 转成 A-label 之后只剩下这些字符。
	domainLabelPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
)

// idnaProfile 是域名转 A-label 用的档位。
//
// MapForLookup 做大小写折叠与兼容字符映射；StrictDomainName(true) 拒绝下划线、
// 空格、符号这类“看起来像域名但不是域名”的输入；Transitional(false) 走现代规则
// （ß 不映射成 ss）——域名归一是为了写进配置和文件名，用宽松规则会把两个不同的
// 域名折叠成一个。
var idnaProfile = idna.New(
	idna.MapForLookup(),
	idna.StrictDomainName(true),
	idna.Transitional(false),
)

func newID() (string, error) {
	var b [idHexLen / 2]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("proxies: 生成标识失败: %w", err)
	}
	return siteIDPrefix + hex.EncodeToString(b[:]), nil
}

// NormalizeDomain 把用户输入的域名归一化成入库形态：小写、A-label、无尾点。
//
// 只接受域名本身：协议、路径、端口、通配符全部拒绝，而不是把它们悄悄剪掉。
// 悄悄剪掉的问题是用户以为写的是别的东西——“https://a.com/x”被剪成 a.com 之后，
// 面板上一个字都没提他多写了什么。
func NormalizeDomain(raw string) (string, error) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return "", invalid("domain", "域名不能为空")
	}
	if strings.Contains(v, "*") {
		return "", invalid("domain", "暂不支持通配符域名，请填写一个具体域名")
	}
	if i := strings.IndexAny(v, "/?#@"); i >= 0 {
		return "", invalid("domain", "域名里不能带协议、路径或查询串（发现 %q）", string(v[i]))
	}
	if strings.Contains(v, ":") {
		return "", invalid("domain", "域名里不能带端口，端口请填在 upstream_port")
	}
	if strings.ContainsAny(v, " \t\r\n") {
		return "", invalid("domain", "域名里不能有空白")
	}
	// 允许写一个尾点（FQDN 写法），去掉它再归一。
	v = strings.TrimSuffix(v, ".")

	ascii, err := idnaProfile.ToASCII(v)
	if err != nil {
		return "", invalid("domain", "域名不合法：%s", idnaReason(err))
	}
	host := strings.ToLower(ascii)
	if len(host) > MaxDomainLen {
		return "", invalid("domain", "域名太长（%d 个字符，上限 %d）", len(host), MaxDomainLen)
	}
	if ip, perr := netip.ParseAddr(host); perr == nil {
		return "", invalid("domain", "%s 是 IP 地址，不是域名；上游请用 upstream_target_id 指定", ip)
	}
	labels := strings.Split(host, ".")
	for _, label := range labels {
		if !domainLabelPattern.MatchString(label) {
			return "", invalid("domain", "域名里有不合法的片段 %q（只允许字母、数字、连字符，且不能以连字符开头或结尾）", label)
		}
	}
	if len(labels) < 2 {
		return "", invalid("domain", "域名至少要有一个点（%q 不是可访问的域名）", host)
	}
	// 全是数字的顶级域说明这是被人写错的 IP。
	if isAllDigits(labels[len(labels)-1]) {
		return "", invalid("domain", "顶级域不能全是数字（%q 看起来像 IP 地址）", host)
	}
	return host, nil
}

// idnaReason 把 IDNA 的英文错误折成一句中文，同时保留原错误的关键词。
func idnaReason(err error) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "unsupported rune"):
		return "含有不能用于域名的字符"
	case strings.Contains(msg, "label too long"):
		return "某一段超过 63 个字符"
	case strings.Contains(msg, "empty label"):
		return "有点号相连的空片段"
	case strings.Contains(msg, "domain name too long"):
		return "域名太长"
	default:
		return msg
	}
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// Normalize 校验并规范化一份站点定义。
//
// 返回值才是可入库的形态：域名归一、CIDR 收敛成网络地址并排序去重、账号去重、
// 默认值填好。任何不合法都返回 *ValidationError（400）——输入问题不是系统故障。
//
// 应用是否存在、域名是否被现有 Nginx 配置占用需要数据库与主机事实，不在这里判断
// （见 Service.Create / Service.Update）。
func (d Definition) Normalize(opts NormalizeOptions) (Definition, error) {
	out := d

	if out.AppID != "" && !apps.ValidAppID(out.AppID) {
		return Definition{}, invalid("app_id", "应用标识必须是 app_ 加 32 位小写十六进制")
	}

	domain, err := NormalizeDomain(out.Domain)
	if err != nil {
		return Definition{}, err
	}
	out.Domain = domain

	if out.UpstreamTargetID == "" {
		return Definition{}, invalid("upstream_target_id", "必须指定上游目标")
	}
	if !apps.ValidTargetID(out.UpstreamTargetID) {
		return Definition{}, invalid("upstream_target_id",
			"上游目标必须是代理发现的目标标识（local_ 或 cntr_ 开头），不能填主机名、IP 或 URL")
	}

	switch out.UpstreamScheme {
	case "":
		out.UpstreamScheme = SchemeHTTP
	case SchemeHTTP, SchemeHTTPS:
	default:
		return Definition{}, invalid("upstream_scheme", "上游协议只支持 http 或 https")
	}

	if out.UpstreamPort < 1 || out.UpstreamPort > 65535 {
		return Definition{}, invalid("upstream_port", "上游端口必须在 1 到 65535 之间")
	}
	if opts.PanelPort > 0 && out.UpstreamPort == opts.PanelPort {
		return Definition{}, invalid("upstream_port",
			"上游端口不能是管理面板自己的端口 %d：那等于把一个公网入口指向面板", opts.PanelPort)
	}

	if out.AccessMode == "" {
		out.AccessMode = AccessPublic
	}
	if !out.AccessMode.Valid() {
		return Definition{}, invalid("access_mode", "访问模式只支持 public、cidr、basic_auth")
	}

	cidrs, err := normalizeCIDRs(out.AllowedCIDRs, out.AccessMode)
	if err != nil {
		return Definition{}, err
	}
	out.AllowedCIDRs = cidrs

	users, err := normalizeBasicAuthUsers(out.BasicAuthUsers, out.AccessMode)
	if err != nil {
		return Definition{}, err
	}
	out.BasicAuthUsers = users

	if out.MaxBodyBytes == 0 {
		out.MaxBodyBytes = DefaultMaxBodyBytes
	}
	if out.MaxBodyBytes < MinMaxBodyBytes || out.MaxBodyBytes > MaxMaxBodyBytes {
		return Definition{}, invalid("max_body_bytes",
			"请求体上限必须在 %d 到 %d 字节之间（1 KiB — 1 GiB）", MinMaxBodyBytes, MaxMaxBodyBytes)
	}

	if out.ReadTimeoutSeconds == 0 {
		out.ReadTimeoutSeconds = DefaultReadTimeoutSeconds
	}
	if out.ReadTimeoutSeconds < MinReadTimeoutSeconds || out.ReadTimeoutSeconds > MaxReadTimeoutSeconds {
		return Definition{}, invalid("read_timeout_seconds",
			"读取超时必须在 %d 到 %d 秒之间", MinReadTimeoutSeconds, MaxReadTimeoutSeconds)
	}

	return out, nil
}

// normalizeCIDRs 校验白名单：cidr 模式必须非空，其它模式必须为空。
func normalizeCIDRs(in []string, mode AccessMode) ([]string, error) {
	if mode != AccessCIDR {
		if len(in) > 0 {
			return nil, invalid("allowed_cidrs",
				"只有 cidr 访问模式才能填允许的来源网段（当前是 %s）", mode)
		}
		return nil, nil
	}
	if len(in) == 0 {
		return nil, invalid("allowed_cidrs", "cidr 访问模式至少需要一个允许的来源网段")
	}
	if len(in) > MaxAllowedCIDRs {
		return nil, invalid("allowed_cidrs", "最多只能填 %d 个网段", MaxAllowedCIDRs)
	}

	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, raw := range in {
		v := strings.TrimSpace(raw)
		if v == "" {
			return nil, invalid("allowed_cidrs", "网段不能是空字符串")
		}
		prefix, err := netip.ParsePrefix(v)
		if err != nil {
			return nil, invalid("allowed_cidrs", "%q 不是合法的网段（示例：192.168.1.0/24）", v)
		}
		prefix = prefix.Masked()
		if prefix.Bits() == 0 {
			// 0.0.0.0/0 和 ::/0 在效果上就是 public，却顶着一个“受限”的名字。
			return nil, invalid("allowed_cidrs",
				"%s 等于对所有来源开放，请直接用 public 访问模式", prefix)
		}
		s := prefix.String()
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	// 排序让入库形态确定：同一组网段换个输入顺序，存下来应该一模一样。
	sort.Strings(out)
	return out, nil
}

// normalizeBasicAuthUsers 校验访问账号：basic_auth 模式必须非空，其它模式必须为空。
func normalizeBasicAuthUsers(in []BasicAuthUser, mode AccessMode) ([]BasicAuthUser, error) {
	if mode != AccessBasicAuth {
		if len(in) > 0 {
			return nil, invalid("basic_auth_users",
				"只有 basic_auth 访问模式才能填访问账号（当前是 %s）", mode)
		}
		return nil, nil
	}
	if len(in) == 0 {
		return nil, invalid("basic_auth_users", "basic_auth 访问模式至少需要一个访问账号")
	}
	if len(in) > MaxBasicAuthUsers {
		return nil, invalid("basic_auth_users", "最多只能填 %d 个访问账号", MaxBasicAuthUsers)
	}

	seen := make(map[string]struct{}, len(in))
	out := make([]BasicAuthUser, 0, len(in))
	for _, u := range in {
		name := strings.TrimSpace(u.Name)
		if !basicAuthNamePattern.MatchString(name) {
			return nil, invalid("basic_auth_users",
				"账号名 %q 不合法：1—%d 个字符，只允许小写字母、数字与 . _ -，且以字母或数字开头",
				u.Name, MaxBasicAuthNameLen)
		}
		ref := strings.TrimSpace(u.SecretRef)
		if !secretRefPattern.MatchString(ref) {
			return nil, invalid("basic_auth_users",
				"账号 %q 的密码引用不合法：密码要先传到 /secrets，这里只填返回的 sec_ 引用", name)
		}
		if _, dup := seen[name]; dup {
			return nil, invalid("basic_auth_users", "账号名 %q 重复了", name)
		}
		seen[name] = struct{}{}
		out = append(out, BasicAuthUser{Name: name, SecretRef: ref})
	}
	return out, nil
}

// validSiteID 判断站点标识的形状。
func validSiteID(id string) bool { return siteIDPattern.MatchString(id) }

// ValidSiteID 是 validSiteID 的对外形式。
//
// 接口层要在把标识透传给业务层之前先挡掉形状不对的输入：站点标识会进日志与审计，
// 也会被拼进托管文件名（`upanel-<domain>.conf` 用的是域名，但 id 同样不能被
// 任意字符串顶替）。
func ValidSiteID(id string) bool { return validSiteID(id) }
