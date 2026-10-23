package tlscert

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"
)

// fixture 是一张自制证书及其配套私钥（全部在内存里生成，不碰磁盘）。
type fixture struct {
	certPEM []byte
	keyPEM  []byte
	info    Info
}

// newFixture 造一张证书。issuer 为 nil 时自签名。
//
// 用真实 crypto 生成而不是硬编码一段 PEM：PEM 会过期，而这一包的核心断言是
// “解析出来的日期与 SAN 与生成时给的一致”，只有生成才能知道标准答案。
func newFixture(t *testing.T, opts fixtureOptions) fixture {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("生成私钥失败: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(opts.serial),
		Subject: pkix.Name{
			CommonName:   opts.commonName,
			Organization: []string{"uPanel Test"},
		},
		NotBefore:             opts.notBefore,
		NotAfter:              opts.notAfter,
		DNSNames:              opts.dnsNames,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  opts.isCA,
	}
	// 父证书决定 issuer：CreateCertificate 用 parent.Subject 当 Issuer，所以想造
	// 一张“看起来由某个 CA 签发”的证书，就得给一个 Subject 是那个 CA 的 parent。
	parent := tmpl
	if opts.issuerCN != "" {
		parent = &x509.Certificate{
			SerialNumber:          big.NewInt(opts.serial + 1),
			Subject:               pkix.Name{CommonName: opts.issuerCN, Organization: []string{opts.issuerOrg}},
			NotBefore:             opts.notBefore,
			NotAfter:              opts.notAfter,
			KeyUsage:              x509.KeyUsageCertSign,
			BasicConstraintsValid: true,
			IsCA:                  true,
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("签发证书失败: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("编码私钥失败: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	info, err := ParseCertificate(certPEM)
	if err != nil {
		t.Fatalf("解析刚签发的证书失败: %v", err)
	}
	return fixture{certPEM: certPEM, keyPEM: keyPEM, info: info}
}

type fixtureOptions struct {
	serial     int64
	commonName string
	dnsNames   []string
	notBefore  time.Time
	notAfter   time.Time
	isCA       bool
	issuerCN   string
	issuerOrg  string
}

func defaultOptions() fixtureOptions {
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	return fixtureOptions{
		serial:     0x1234abcd,
		commonName: "p103.example.test",
		dnsNames:   []string{"p103.example.test"},
		notBefore:  now.Add(-24 * time.Hour),
		notAfter:   now.Add(90 * 24 * time.Hour),
	}
}

// TestParseCertificateFacts 解析出的域名、日期、指纹与生成时给的一致。
func TestParseCertificateFacts(t *testing.T) {
	opts := defaultOptions()
	opts.dnsNames = []string{"p103.example.test", "www.p103.example.test"}
	fx := newFixture(t, opts)

	if got, want := strings.Join(fx.info.Domains, ","), "p103.example.test,www.p103.example.test"; got != want {
		t.Fatalf("域名不对: got %q want %q", got, want)
	}
	if !fx.info.NotAfter.Equal(opts.notAfter) {
		t.Fatalf("not_after 不对: got %s want %s", fx.info.NotAfter, opts.notAfter)
	}
	if fx.info.Serial != "1234ABCD" {
		t.Fatalf("序列号应为大写十六进制: got %q", fx.info.Serial)
	}
	if len(fx.info.Fingerprint) != 64 {
		t.Fatalf("指纹应为 64 位十六进制: got %q", fx.info.Fingerprint)
	}
	if fx.info.ChainLength != 1 {
		t.Fatalf("链长度应为 1: got %d", fx.info.ChainLength)
	}
	if !fx.info.SelfSigned {
		t.Fatalf("自签名证书应被标记")
	}
	if fx.info.Staging {
		t.Fatalf("自制证书不该被标记成 staging")
	}
}

// TestStagingDetection 测试环境签发链被认出来，正式链不会被误判。
func TestStagingDetection(t *testing.T) {
	cases := []struct {
		name    string
		issuer  string
		org     string
		staging bool
	}{
		{"le-staging", "(STAGING) Pretend Pear X1", "(STAGING) Let's Encrypt", true},
		{"pebble", "Pebble Intermediate CA 3f4a1c", "Pebble", true},
		{"le-production", "R11", "Let's Encrypt", false},
		{"self", "p103.example.test", "uPanel Test", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := defaultOptions()
			opts.issuerCN = tc.issuer
			opts.issuerOrg = tc.org
			fx := newFixture(t, opts)
			if fx.info.Staging != tc.staging {
				t.Fatalf("staging 判断错误: issuer=%q got=%v want=%v",
					fx.info.Issuer, fx.info.Staging, tc.staging)
			}
		})
	}
}

// TestCoversDomain 覆盖判断：精确、通配一级、跨级不通配。
func TestCoversDomain(t *testing.T) {
	opts := defaultOptions()
	opts.dnsNames = []string{"p103.example.test", "*.wild.example.test"}
	fx := newFixture(t, opts)
	cases := map[string]bool{
		"p103.example.test":     true,
		"P103.Example.TEST.":    true,
		"a.wild.example.test":   true,
		"a.b.wild.example.test": false,
		"wild.example.test":     false,
		"other.example.test":    false,
		"":                      false,
	}
	for domain, want := range cases {
		if got := CoversDomain(fx.info, domain); got != want {
			t.Fatalf("覆盖判断错误: domain=%q got=%v want=%v", domain, got, want)
		}
	}
}

// TestKeyMatchesCertificate 配套与不配套两种结果。
func TestKeyMatchesCertificate(t *testing.T) {
	fx := newFixture(t, defaultOptions())
	ok, info, err := KeyMatchesCertificate(fx.certPEM, fx.keyPEM)
	if err != nil || !ok {
		t.Fatalf("配套的证书与私钥应判为匹配: ok=%v err=%v", ok, err)
	}
	if info.Fingerprint != fx.info.Fingerprint {
		t.Fatalf("返回的证书事实不对")
	}

	other := newFixture(t, defaultOptions())
	ok, _, err = KeyMatchesCertificate(fx.certPEM, other.keyPEM)
	if err != nil {
		t.Fatalf("比较另一把私钥不该报错（结果应是 false）: %v", err)
	}
	if ok {
		t.Fatalf("不配套的私钥被判成匹配")
	}
}

// TestParsePrivateKeyFormats 三种私钥编码都能解析，带口令的拒绝。
func TestParsePrivateKeyFormats(t *testing.T) {
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("生成 RSA 私钥失败: %v", err)
	}
	pkcs1 := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(rsaKey)})
	if _, err := ParsePrivateKey(pkcs1); err != nil {
		t.Fatalf("PKCS#1 应能解析: %v", err)
	}

	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("生成 EC 私钥失败: %v", err)
	}
	sec1DER, err := x509.MarshalECPrivateKey(ecKey)
	if err != nil {
		t.Fatalf("编码 EC 私钥失败: %v", err)
	}
	sec1 := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: sec1DER})
	if _, err := ParsePrivateKey(sec1); err != nil {
		t.Fatalf("SEC1 应能解析: %v", err)
	}

	enc := pem.EncodeToMemory(&pem.Block{Type: "ENCRYPTED PRIVATE KEY", Bytes: []byte("not-really-encrypted")})
	if _, err := ParsePrivateKey(enc); err != ErrEncryptedKey {
		t.Fatalf("带口令的私钥应被拒绝: got %v", err)
	}

	if _, err := ParsePrivateKey([]byte("hello")); err != ErrNoPrivateKey {
		t.Fatalf("非 PEM 应报 ErrNoPrivateKey: got %v", err)
	}
}

// TestContainsPrivateKey 上传内容夹带私钥必须能被识别。
func TestContainsPrivateKey(t *testing.T) {
	fx := newFixture(t, defaultOptions())
	if ContainsPrivateKey(fx.certPEM) {
		t.Fatalf("纯证书不该被判成含私钥")
	}
	joined := append(append([]byte{}, fx.certPEM...), fx.keyPEM...)
	if !ContainsPrivateKey(joined) {
		t.Fatalf("证书 + 私钥应被识别出来")
	}
	enc := pem.EncodeToMemory(&pem.Block{Type: "ENCRYPTED PRIVATE KEY", Bytes: []byte("x")})
	if !ContainsPrivateKey(enc) {
		t.Fatalf("带口令私钥也应被识别出来")
	}
}

// TestParseCertificateRejectsJunk 各种“不是证书”的输入都有明确错误。
func TestParseCertificateRejectsJunk(t *testing.T) {
	if _, err := ParseCertificate(nil); err != ErrNoCertificate {
		t.Fatalf("空输入应报 ErrNoCertificate: got %v", err)
	}
	if _, err := ParseCertificate([]byte("not a pem")); err != ErrNoCertificate {
		t.Fatalf("非 PEM 应报 ErrNoCertificate: got %v", err)
	}
	bad := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("junk")})
	if _, err := ParseCertificate(bad); err == nil {
		t.Fatalf("坏 DER 应报错")
	}
	big := make([]byte, MaxCertBytes+1)
	if _, err := ParseCertificate(big); err != ErrTooLarge {
		t.Fatalf("超限输入应报 ErrTooLarge: got %v", err)
	}
}

// TestCAIsFlagged CA 证书被标出来（导入给站点用时上层据此拒绝）。
func TestCAIsFlagged(t *testing.T) {
	opts := defaultOptions()
	opts.isCA = true
	fx := newFixture(t, opts)
	if !fx.info.IsCA {
		t.Fatalf("CA 证书应被标记")
	}
}

// TestExpiryStates 到期与临期的边界。
func TestExpiryStates(t *testing.T) {
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	opts := defaultOptions()
	opts.notAfter = now.Add(ExpiringWindow - time.Minute)
	fx := newFixture(t, opts)
	if !fx.info.Expiring(now) {
		t.Fatalf("窗口内应判为临期")
	}
	if fx.info.Expired(now) {
		t.Fatalf("尚未到期不该判成已过期")
	}

	opts.notAfter = now.Add(ExpiringWindow + time.Hour)
	fx = newFixture(t, opts)
	if fx.info.Expiring(now) {
		t.Fatalf("窗口外不该判为临期")
	}

	opts.notAfter = now.Add(-time.Minute)
	fx = newFixture(t, opts)
	if !fx.info.Expired(now) || !fx.info.Expiring(now) {
		t.Fatalf("过期证书应同时满足两个判断")
	}
}
