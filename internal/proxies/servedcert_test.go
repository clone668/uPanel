package proxies

// servedCertFingerprint 的真握手用例（P10.3）。
//
// 为什么单独测这一个函数：证书轮换的判据全押在"问 443 一句"上，
// 而它最容易错的地方不是判断逻辑，是**握手参数**——不带 SNI 的话 Nginx
// 会给我们默认 server 块的证书，指纹永远对不上，于是每次应用都 reload，
// 看上去"能用"，实际是在每次点击时都重载一次服务。
//
// 所以这里起一个真的 TLS 监听，用 GetCertificate 按 SNI 选证书，
// 断言：① 指纹是服务端给的那张；② SNI 真的送到了服务端。

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"math/big"
	"net"
	"testing"
	"time"
)

// probeTestCert 造一张自签证书并返回 tls.Certificate 与指纹。
func probeTestCert(t *testing.T, cn string) (tls.Certificate, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("生成私钥失败: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(0x51),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		DNSNames:              []string{cn},
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}
	sum := sha256.Sum256(der)
	cert := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	return cert, hex.EncodeToString(sum[:])
}

// TestServedCertFingerprintReturnsServedLeaf 指纹来自服务端真正出示的叶证书。
func TestServedCertFingerprintReturnsServedLeaf(t *testing.T) {
	cert, want := probeTestCert(t, "probe.test")
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	})
	if err != nil {
		t.Fatalf("起监听失败: %v", err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				_ = c.(*tls.Conn).Handshake()
				_ = c.Close()
			}(conn)
		}
	}()

	port := ln.Addr().(*net.TCPAddr).Port
	got, err := servedCertFingerprint(context.Background(), "probe.test", port)
	if err != nil {
		t.Fatalf("握手失败: %v", err)
	}
	if got != want {
		t.Errorf("指纹 = %s，期望 %s", got, want)
	}
}

// TestServedCertFingerprintSendsSNI 服务端按 SNI 选证书时，我们拿到的是对的那张。
//
// 这一条是"必须带 SNI"的证据：不带 SNI 时服务端只会给 first 那张，
// 于是第二个域名的指纹永远对不上。
func TestServedCertFingerprintSendsSNI(t *testing.T) {
	first, firstFP := probeTestCert(t, "first.test")
	second, secondFP := probeTestCert(t, "second.test")
	if firstFP == secondFP {
		t.Fatal("两张测试证书指纹相同，用例的前提不成立")
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{first},
		MinVersion:   tls.VersionTLS12,
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			if hello.ServerName == "second.test" {
				return &second, nil
			}
			return &first, nil
		},
	})
	if err != nil {
		t.Fatalf("起监听失败: %v", err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				_ = c.(*tls.Conn).Handshake()
				_ = c.Close()
			}(conn)
		}
	}()

	port := ln.Addr().(*net.TCPAddr).Port
	got, err := servedCertFingerprint(context.Background(), "second.test", port)
	if err != nil {
		t.Fatalf("握手失败: %v", err)
	}
	if got != secondFP {
		t.Errorf("带 SNI 握手拿到的指纹 = %s，期望第二张 %s（说明 SNI 没送出去）", got, secondFP)
	}
}

// TestServedCertFingerprintNoListener 端口没人听时返回错误，而不是瞎猜一个指纹。
func TestServedCertFingerprintNoListener(t *testing.T) {
	// 先占一个端口再关掉，避免赌一个"大概没人用"的端口。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("占端口失败: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	if _, err := servedCertFingerprint(context.Background(), "gone.test", port); err == nil {
		t.Fatal("期望握手失败，实际成功了")
	}
}

// TestSameFingerprint 空值不等于相同——"不知道"不能当作"没变化"。
func TestSameFingerprint(t *testing.T) {
	lower := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	cases := []struct {
		name string
		a    string
		b    string
		want bool
	}{
		{"相同", lower, lower, true},
		{"大小写不同也算相同", lower, "0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF", true},
		{"不同", lower, lower[:63] + "0", false},
		{"磁盘为空", "", lower, false},
		{"服务端为空", lower, "", false},
		{"都为空", "", "", false},
	}
	for _, c := range cases {
		if got := sameFingerprint(c.a, c.b); got != c.want {
			t.Errorf("%s: sameFingerprint = %v，期望 %v", c.name, got, c.want)
		}
	}
	if got := shortFingerprint(lower); got != lower[:16]+"…" {
		t.Errorf("shortFingerprint = %q", got)
	}
	if got := shortFingerprint("abc"); got != "abc" {
		t.Errorf("短指纹不该被截断：%q", got)
	}
}
