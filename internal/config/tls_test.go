package config

// 面板 HTTPS 材料的测试（P10.4）。
//
// 这里断言的是**判断**：证书能不能配上、好不好用、该不该拒绝。
// 真正的握手在 internal/api/server_test.go 里用真实监听与真实客户端验。

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type panelCert struct {
	certPath string
	keyPath  string
	certPEM  []byte
}

// writePanelCert 生成一对自签名证书/私钥并写到临时目录。
func writePanelCert(t *testing.T, notBefore, notAfter time.Time) panelCert {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("生成密钥失败: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1001),
		Subject:               pkix.Name{CommonName: "upanel.test"},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"upanel.test", "127.0.0.1"},
		IPAddresses:           nil,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("签发证书失败: %v", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("编码私钥失败: %v", err)
	}

	dir := t.TempDir()
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	certPath := filepath.Join(dir, "panel.pem")
	keyPath := filepath.Join(dir, "panel.key")
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		t.Fatalf("写证书失败: %v", err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatalf("写私钥失败: %v", err)
	}
	return panelCert{certPath: certPath, keyPath: keyPath, certPEM: certPEM}
}

func TestLoadTLSReturnsUsableMaterial(t *testing.T) {
	now := time.Now().UTC()
	f := writePanelCert(t, now.Add(-time.Hour), now.Add(90*24*time.Hour))

	web := Web{TLSCertPath: f.certPath, TLSKeyPath: f.keyPath}
	got, err := web.LoadTLS()
	if err != nil {
		t.Fatalf("加载证书失败: %v", err)
	}
	if got.Config == nil || len(got.Config.Certificates) != 1 {
		t.Fatalf("TLS 配置里应正好有一张证书: %+v", got.Config)
	}
	if got.Config.MinVersion != PanelTLSMinVersion {
		t.Fatalf("最低 TLS 版本 = %x，期望 %x", got.Config.MinVersion, PanelTLSMinVersion)
	}
	if len(got.Info.Fingerprint) != 64 {
		t.Fatalf("指纹 = %q，期望 sha256 的 64 位十六进制", got.Info.Fingerprint)
	}
	if got.Info.NotAfter.IsZero() {
		t.Fatal("到期时间不应为零值")
	}
	if !got.Info.SelfSigned {
		t.Fatal("自签名的证书必须被认出来（安装输出靠它提示用户核对指纹）")
	}
	if len(got.Warnings) == 0 {
		t.Fatal("自签名证书必须带一条告警：浏览器不会信任它")
	}
}

func TestLoadTLSWarnsWhenKeyIsWorldReadable(t *testing.T) {
	now := time.Now().UTC()
	f := writePanelCert(t, now.Add(-time.Hour), now.Add(90*24*time.Hour))
	if err := os.Chmod(f.keyPath, 0o644); err != nil {
		t.Fatalf("改权限失败: %v", err)
	}
	got, err := Web{TLSCertPath: f.certPath, TLSKeyPath: f.keyPath}.LoadTLS()
	if err != nil {
		t.Fatalf("加载证书失败: %v", err)
	}
	var found bool
	for _, w := range got.Warnings {
		if strings.Contains(w, "其他用户可读") {
			found = true
		}
	}
	if !found {
		t.Fatalf("私钥 0644 必须被报出来，实际告警: %v", got.Warnings)
	}
}

func TestLoadTLSRejectsMismatchedPair(t *testing.T) {
	now := time.Now().UTC()
	a := writePanelCert(t, now.Add(-time.Hour), now.Add(24*time.Hour))
	b := writePanelCert(t, now.Add(-time.Hour), now.Add(24*time.Hour))

	_, err := Web{TLSCertPath: a.certPath, TLSKeyPath: b.keyPath}.LoadTLS()
	if err == nil {
		t.Fatal("证书与私钥不是一对时必须报错")
	}
	if !strings.Contains(err.Error(), "不是一对") {
		t.Fatalf("错误信息应指出最常见原因，实际: %v", err)
	}
}

func TestLoadTLSRejectsExpiredCert(t *testing.T) {
	now := time.Now().UTC()
	f := writePanelCert(t, now.Add(-48*time.Hour), now.Add(-time.Hour))

	_, err := Web{TLSCertPath: f.certPath, TLSKeyPath: f.keyPath}.LoadTLS()
	if err == nil {
		t.Fatal("过期证书必须拒绝启动，而不是照常提供服务")
	}
	if !strings.Contains(err.Error(), "过期") {
		t.Fatalf("错误信息应说明过期，实际: %v", err)
	}
}

func TestLoadTLSRejectsNotYetValidCert(t *testing.T) {
	now := time.Now().UTC()
	f := writePanelCert(t, now.Add(48*time.Hour), now.Add(96*time.Hour))

	_, err := Web{TLSCertPath: f.certPath, TLSKeyPath: f.keyPath}.LoadTLS()
	if err == nil {
		t.Fatal("尚未生效的证书必须报错（多半是系统时间错了）")
	}
	if !strings.Contains(err.Error(), "才生效") {
		t.Fatalf("错误信息应说明尚未生效，实际: %v", err)
	}
}

func TestLoadTLSRejectsMissingFile(t *testing.T) {
	dir := t.TempDir()
	_, err := Web{
		TLSCertPath: filepath.Join(dir, "nope.pem"),
		TLSKeyPath:  filepath.Join(dir, "nope.key"),
	}.LoadTLS()
	if err == nil {
		t.Fatal("文件不存在时必须报错")
	}
	if !strings.Contains(err.Error(), "不存在") {
		t.Fatalf("错误信息应说明文件不存在，实际: %v", err)
	}
}

func TestWebValidateRequiresBothTLSPaths(t *testing.T) {
	cfg := Defaults()
	cfg.Web.TLSCertPath = "/etc/upanel/tls/panel.pem"
	if err := cfg.Validate(); err == nil {
		t.Fatal("只配证书不配私钥必须被拒绝")
	}

	cfg = Defaults()
	cfg.Web.TLSKeyPath = "/etc/upanel/tls/panel.key"
	if err := cfg.Validate(); err == nil {
		t.Fatal("只配私钥不配证书必须被拒绝")
	}

	cfg = Defaults()
	cfg.Web.TLSCertPath = "/etc/upanel/tls/panel.pem"
	cfg.Web.TLSKeyPath = "/etc/upanel/tls/panel.key"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("成对配置应通过: %v", err)
	}
}

func TestWebValidateRejectsRelativeTLSPath(t *testing.T) {
	cfg := Defaults()
	cfg.Web.TLSCertPath = "tls/panel.pem"
	cfg.Web.TLSKeyPath = "tls/panel.key"
	if err := cfg.Validate(); err == nil {
		t.Fatal("相对路径必须被拒绝")
	}
}

func TestCheckTransport(t *testing.T) {
	if err := CheckTransport(ListenProduction, false); err != nil {
		t.Fatalf("回环 + 明文（SSH 隧道部署）应允许: %v", err)
	}
	for _, listen := range []string{"0.0.0.0:9443", "192.0.2.10:9443", "[::]:9443"} {
		if err := CheckTransport(listen, false); err == nil {
			t.Fatalf("%s: 非回环且没开 TLS 必须拒绝", listen)
		}
		if err := CheckTransport(listen, true); err != nil {
			t.Fatalf("%s: 非回环 + TLS 应允许: %v", listen, err)
		}
	}
}

func TestResolveAllowedOriginsUsesHTTPSScheme(t *testing.T) {
	got, err := ResolveAllowedOrigins(nil, ListenProduction, true)
	if err != nil {
		t.Fatalf("推导允许源失败: %v", err)
	}
	want := []string{"https://127.0.0.1:9443", "https://localhost:9443"}
	if len(got) != len(want) {
		t.Fatalf("允许源 = %v，期望 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("允许源 = %v，期望 %v", got, want)
		}
	}
}
