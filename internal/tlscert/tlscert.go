// Package tlscert 解析与校验 PEM 证书/私钥（P10.3）。
//
// 这个包是**纯函数**：不读磁盘、不发请求、不写日志。三处地方要用它，而它们的
// 信任边界完全不同：
//
//  1. 面板在上传证书时先解析一遍，好让用户在第 10 分钟到期之前看到“这是谁的证书、
//     覆盖哪些域名、什么时候过期”——上传不等于导入，这一步只是预览。
//  2. 代理在执行导入时**再解析一遍**，这一遍才是判据：面板看过什么不算数。
//  3. 代理在签发/续期之后读回证书文件核验（not_after 变了没有、是不是 staging）。
//
// 私钥相关的能力只有两件事：解析出公钥用于“私钥与证书是否配套”，以及判断这是不是
// 一段带口令的私钥（我们不支持口令，因为口令本身没地方存）。**本包不提供任何把私钥
// 转成字符串、写进日志或者返回给上层的函数**，调用方拿到的只有 crypto.Signer 和
// 比较结果。
package tlscert

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	// MaxCertBytes 是公钥证书 PEM 的上限（CONTRACTS.md 第 9 节：1 MiB）。
	MaxCertBytes = 1 << 20
	// MaxKeyBytes 是私钥 PEM 的上限（agentstore 的 tls_private_key 用途是 16 KiB，
	// 这里留同一数量级；密码学上 8 KiB 已经足够放下 RSA-4096 的 PKCS#8）。
	MaxKeyBytes = 16 << 10

	// ExpiringWindow 是“临期”的判据。21 天是有意的松弛值：Let's Encrypt 在到期前
	// 30 天开始建议续期，而自动续期每天跑一次；把阈值设成 21 天，意味着“连续几次
	// 续期失败”才会变成临期，而不是一进入 30 天窗口就满屏告警。
	ExpiringWindow = 21 * 24 * time.Hour
)

// 错误。它们都是“输入不对”，由接口层映射成 400，不映射成 500。
var (
	// ErrNoCertificate 表示输入里没有 CERTIFICATE 块。
	ErrNoCertificate = errors.New("tlscert: 输入里没有 PEM 证书块")
	// ErrTooLarge 表示输入超过上限。
	ErrTooLarge = errors.New("tlscert: 输入超过大小上限")
	// ErrEncryptedKey 表示私钥带口令（本产品没有存放口令的地方，因此不支持）。
	ErrEncryptedKey = errors.New("tlscert: 私钥带口令，不支持")
	// ErrNoPrivateKey 表示输入里没有可识别的私钥块。
	ErrNoPrivateKey = errors.New("tlscert: 输入里没有可识别的私钥块")
	// ErrKeyMismatch 表示私钥与证书不配套。
	ErrKeyMismatch = errors.New("tlscert: 私钥与证书不配套")
	// ErrUnsupportedKey 表示私钥类型不在支持范围内。
	ErrUnsupportedKey = errors.New("tlscert: 不支持的私钥类型")
)

// Info 是一张证书的可展示事实。
//
// 这里**没有**任何私钥字段，也没有“证书内容”字段：需要内容的只有写盘那一步，
// 而那一步拿的是原始字节，不从这个结构走。
type Info struct {
	// Domains 是证书覆盖的域名（SAN + CN，规范化成小写、去重、排序）。
	Domains []string
	// CommonName 是主题里的 CN（可能为空，现代证书不一定有）。
	CommonName string
	// Issuer 是签发者的可读名字（优先 O，其次 CN）。
	Issuer string
	// Subject 是持有者的可读名字（优先 CN，其次 O）。
	Subject string
	// NotBefore / NotAfter 是有效期（UTC）。
	NotBefore time.Time
	NotAfter  time.Time
	// Serial 是序列号（十六进制大写）。
	Serial string
	// Fingerprint 是整张证书 DER 的 SHA-256（小写十六进制），用于“文件里的是不是
	// 还是那一张”的判断。
	Fingerprint string
	// IsCA 表示这张证书本身是 CA 证书（导入给站点用的话是错的，要拒绝）。
	IsCA bool
	// SelfSigned 表示签发者就是自己（PLAN.md 第 13 节允许自签名，但要提示）。
	SelfSigned bool
	// Staging 表示签发链看起来是 ACME 测试环境（Let's Encrypt 的 staging 证书
	// issuer 里带 "(STAGING)"，Pebble 带 "Pebble"）。判断放在这里而不是让上层猜：
	// staging 证书在浏览器里不受信任，界面上必须一眼可见。
	Staging bool
	// ChainLength 是输入里 CERTIFICATE 块的个数（fullchain 常见 2—3）。
	ChainLength int
}

// Expired 判断证书在 now 之后是否已过期。
func (i Info) Expired(now time.Time) bool { return !now.Before(i.NotAfter) }

// Expiring 判断证书是否已进入临期窗口（含已过期）。
func (i Info) Expiring(now time.Time) bool { return !now.Add(ExpiringWindow).Before(i.NotAfter) }

// ContainsPrivateKey 判断一段上传内容里是否夹带了私钥。
//
// 证书上传接口接受的是**公钥证书**。私钥走秘密通道（POST /secrets，用途
// tls_private_key），面板数据库里永远不出现私钥内容。有人把 fullchain + privkey 一起
// 粘进上传框是很容易发生的事故，所以这里明确检查并拒绝，而不是“忽略多余的块”。
func ContainsPrivateKey(data []byte) bool {
	for {
		var block *pem.Block
		block, data = pem.Decode(data)
		if block == nil {
			return false
		}
		switch block.Type {
		case "PRIVATE KEY", "RSA PRIVATE KEY", "EC PRIVATE KEY",
			"ENCRYPTED PRIVATE KEY", "OPENSSH PRIVATE KEY":
			return true
		}
	}
}

// ParseCertificate 解析 PEM 证书链，返回第一张（叶子）的事实。
//
// 输入里可以有多个 CERTIFICATE 块（fullchain），只解析第一张：nginx 的
// ssl_certificate 也读 fullchain，顺序就是叶子在前。其它类型的块（比如私钥）
// 被忽略——但调用方应当先用 ContainsPrivateKey 拒绝掉那种输入。
func ParseCertificate(data []byte) (Info, error) {
	if len(data) == 0 {
		return Info{}, ErrNoCertificate
	}
	if len(data) > MaxCertBytes {
		return Info{}, ErrTooLarge
	}
	var leafDER []byte
	chain := 0
	rest := data
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		chain++
		if leafDER == nil {
			leafDER = block.Bytes
		}
	}
	if leafDER == nil {
		return Info{}, ErrNoCertificate
	}
	cert, err := x509.ParseCertificate(leafDER)
	if err != nil {
		return Info{}, fmt.Errorf("tlscert: 证书解析失败: %w", err)
	}
	sum := sha256.Sum256(leafDER)
	issuer := readableName(cert.Issuer)
	info := Info{
		Domains:     certDomains(cert),
		CommonName:  strings.ToLower(strings.TrimSpace(cert.Subject.CommonName)),
		Issuer:      issuer,
		Subject:     readableName(cert.Subject),
		NotBefore:   cert.NotBefore.UTC().Truncate(time.Second),
		NotAfter:    cert.NotAfter.UTC().Truncate(time.Second),
		Serial:      strings.ToUpper(cert.SerialNumber.Text(16)),
		Fingerprint: hex.EncodeToString(sum[:]),
		IsCA:        cert.IsCA,
		SelfSigned:  isSelfSigned(cert),
		Staging:     looksStaging(issuer, cert.Subject.String()),
		ChainLength: chain,
	}
	return info, nil
}

// ParsePublicKey 从证书里取出公钥（用于和私钥比对）。
//
// 它不是给上层用的通用能力：唯一的用处是 KeyMatchesCertificate。
func ParsePublicKey(data []byte) (crypto.PublicKey, error) {
	info, err := parseLeaf(data)
	if err != nil {
		return nil, err
	}
	return info.PublicKey, nil
}

// KeyMatchesCertificate 判断私钥是不是这张证书对应的那一把。
//
// 比较的是公钥而不是任何摘要：DER 的两种编码（PKCS#1 与 PKCS#8）装着同一把钥匙时
// 摘要不同、公钥相同，那正是“配套”的定义。
func KeyMatchesCertificate(certPEM, keyPEM []byte) (bool, Info, error) {
	info, err := ParseCertificate(certPEM)
	if err != nil {
		return false, Info{}, err
	}
	leaf, err := parseLeaf(certPEM)
	if err != nil {
		return false, Info{}, err
	}
	signer, err := ParsePrivateKey(keyPEM)
	if err != nil {
		return false, Info{}, err
	}
	ok, err := samePublicKey(leaf.PublicKey, signer.Public())
	return ok, info, err
}

// ParsePrivateKey 解析一段 PEM 私钥（无口令）。
//
// 支持 PKCS#8、PKCS#1（RSA）与 SEC1（EC）。返回值只用于比较公钥：调用方拿不到
// 私钥字节，也拿不到“把它打印出来”的方法。带口令的私钥直接拒绝——我们没有地方
// 存放口令，接受它就等于把口令写进某个字段里。
func ParsePrivateKey(data []byte) (crypto.Signer, error) {
	if len(data) == 0 {
		return nil, ErrNoPrivateKey
	}
	if len(data) > MaxKeyBytes {
		return nil, ErrTooLarge
	}
	rest := data
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			return nil, ErrNoPrivateKey
		}
		switch block.Type {
		case "ENCRYPTED PRIVATE KEY":
			return nil, ErrEncryptedKey
		case "PRIVATE KEY":
			key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
			if err != nil {
				return nil, fmt.Errorf("tlscert: PKCS#8 私钥解析失败: %w", err)
			}
			return asSigner(key)
		case "RSA PRIVATE KEY":
			key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
			if err != nil {
				return nil, fmt.Errorf("tlscert: PKCS#1 私钥解析失败: %w", err)
			}
			return key, nil
		case "EC PRIVATE KEY":
			key, err := x509.ParseECPrivateKey(block.Bytes)
			if err != nil {
				return nil, fmt.Errorf("tlscert: EC 私钥解析失败: %w", err)
			}
			return key, nil
		}
		// 其它块（证书、参数）跳过，继续找私钥。
	}
}

// CoversDomain 判断证书是否覆盖某个域名。
//
// 精确匹配之外只接受“一级通配”：*.example.com 覆盖 a.example.com，不覆盖
// example.com 也不覆盖 a.b.example.com。这条规则与浏览器一致，也与 P10.1
// “站点域名不允许通配符”不冲突——通配证书可以导入，只是不能作为站点域名。
func CoversDomain(info Info, domain string) bool {
	domain = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(domain), "."))
	if domain == "" {
		return false
	}
	for _, d := range info.Domains {
		if d == domain {
			return true
		}
		if strings.HasPrefix(d, "*.") {
			suffix := d[1:] // ".example.com"
			if strings.HasSuffix(domain, suffix) {
				label := strings.TrimSuffix(domain, suffix)
				if label != "" && !strings.Contains(label, ".") {
					return true
				}
			}
		}
	}
	return false
}

// 内部辅助

// leafCert 是 parseLeaf 的结果：整张证书。
type leafCert = x509.Certificate

func parseLeaf(data []byte) (*leafCert, error) {
	if len(data) > MaxCertBytes {
		return nil, ErrTooLarge
	}
	rest := data
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			return nil, ErrNoCertificate
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("tlscert: 证书解析失败: %w", err)
		}
		return cert, nil
	}
}

func asSigner(key any) (crypto.Signer, error) {
	switch k := key.(type) {
	case *rsa.PrivateKey:
		return k, nil
	case *ecdsa.PrivateKey:
		return k, nil
	case ed25519.PrivateKey:
		return k, nil
	case crypto.Signer:
		return k, nil
	default:
		return nil, fmt.Errorf("%w: %T", ErrUnsupportedKey, key)
	}
}

func samePublicKey(a, b crypto.PublicKey) (bool, error) {
	type equaler interface{ Equal(crypto.PublicKey) bool }
	if eq, ok := a.(equaler); ok {
		return eq.Equal(b), nil
	}
	return false, fmt.Errorf("%w: 公钥类型 %T 不支持比较", ErrUnsupportedKey, a)
}

// certDomains 取出证书覆盖的全部域名：SAN + CN，小写、去重、排序。
//
// 排序是为了让库里的 domains_json 稳定——同一个证书重复导入时不该因为 SAN 顺序
// 不同而产生两条“看起来不一样”的记录。
func certDomains(cert *x509.Certificate) []string {
	seen := make(map[string]struct{}, len(cert.DNSNames)+1)
	out := make([]string, 0, len(cert.DNSNames)+1)
	add := func(name string) {
		name = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(name), "."))
		if name == "" {
			return
		}
		if _, ok := seen[name]; ok {
			return
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	for _, name := range cert.DNSNames {
		add(name)
	}
	if cert.Subject.CommonName != "" {
		add(cert.Subject.CommonName)
	}
	sort.Strings(out)
	return out
}

// readableName 把 pkix.Name 压成一行：有 CN 用 CN，否则用 O，再否则用整体字符串。
//
// 用名字而不是 DER：这个值要出现在“这张证书是谁签的”那一行字里，给人看的。
func readableName(name pkix.Name) string {
	if cn := strings.TrimSpace(name.CommonName); cn != "" {
		return cn
	}
	if len(name.Organization) > 0 {
		if org := strings.TrimSpace(name.Organization[0]); org != "" {
			return org
		}
	}
	return strings.TrimSpace(name.String())
}

func isSelfSigned(cert *x509.Certificate) bool {
	if !bytes.Equal(cert.RawSubject, cert.RawIssuer) {
		return false
	}
	// 用 CheckSignature 而不是 CheckSignatureFrom：后者会连带检查 CA 位与
	// KeyUsageCertSign，而一张“自己签自己”的服务端证书**不该**带 CA 位——
	// 用 CheckSignatureFrom 会把它们全都判成“不是自签名”。
	return cert.CheckSignature(cert.SignatureAlgorithm, cert.RawTBSCertificate, cert.Signature) == nil
}

// looksStaging 判断签发链是不是测试环境。
//
// 依据是 issuer 名字里的事实：Let's Encrypt staging 的中间 CA 叫
// "(STAGING) Pretend Pear X1"，Pebble 的签发者叫 "Pebble Intermediate CA …"。
// 这不是“猜”，是那一串字就是证据；判断放在这里，界面才不会把一张浏览器不认的
// 证书画成正常证书。
func looksStaging(issuer, subject string) bool {
	probe := strings.ToLower(issuer + "\x00" + subject)
	for _, marker := range []string{"(staging)", "fake le", "pebble"} {
		if strings.Contains(probe, marker) {
			return true
		}
	}
	return false
}
