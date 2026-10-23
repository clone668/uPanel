package api

// 面板 HTTPS 的测试（P10.4）。
//
// 判断在 internal/config 里测；这里测的是**真的能握手**：监听一个真端口、
// 用真 TLS 客户端连上去、拿到 200 与 HSTS 头。自签名证书在测试里当根证书用，
// 这样做的好处是“证书链错了”也会被握手指出来，而不是被 InsecureSkipVerify 盖住。

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
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"upanel/internal/certificates"
	"upanel/internal/config"
	"upanel/internal/platform"
	"upanel/internal/version"
)

type tlsFixture struct {
	certPath string
	keyPath  string
	certPEM  []byte
	panel    *config.PanelTLS
}

func newTLSFixture(t *testing.T) tlsFixture {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("生成密钥失败: %v", err)
	}
	now := time.Now().UTC()
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(2002),
		Subject:               pkix.Name{CommonName: "upanel.test"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(90 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"upanel.test"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("签发证书失败: %v", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("编码私钥失败: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})

	dir := t.TempDir()
	certPath := filepath.Join(dir, "panel.pem")
	keyPath := filepath.Join(dir, "panel.key")
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		t.Fatalf("写证书失败: %v", err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatalf("写私钥失败: %v", err)
	}
	panel, err := config.Web{TLSCertPath: certPath, TLSKeyPath: keyPath}.LoadTLS()
	if err != nil {
		t.Fatalf("加载证书失败: %v", err)
	}
	return tlsFixture{certPath: certPath, keyPath: keyPath, certPEM: certPEM, panel: panel}
}

// newTLSServer 起一个真的在监听的服务，返回它的地址。
func newTLSServer(t *testing.T, panel *config.PanelTLS) (string, func()) {
	t.Helper()
	authSvc, aud, db := newTestServicesWithDB(t)
	srv, err := New(Options{
		ListenAddress:     "127.0.0.1:0",
		Mode:              platform.ModeProduction,
		Version:           version.Info{Version: "test", Commit: "test", BuildDate: "test", GoVersion: "test"},
		Logger:            quietLogger(),
		ShutdownTimeout:   time.Second,
		Auth:              authSvc,
		Audit:             aud,
		Operations:        newTestOperations(t, db, nil),
		Apps:              newTestApps(t, db),
		Proxies:           newTestProxies(t, db),
		Certificates:      newTestCertificates(t, db),
		CertificateStages: certificates.NewStageStore(nil),
		AllowedOrigins:    testOrigins(),
		TLS:               panel,
	})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ctx) }()

	deadline := time.Now().Add(5 * time.Second)
	for srv.Addr() == "" {
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("Serve 未在超时内开始监听")
		}
		time.Sleep(5 * time.Millisecond)
	}
	stop := func() {
		cancel()
		select {
		case err := <-errCh:
			if err != nil {
				t.Fatalf("停止时返回错误: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("Serve 未在超时内退出")
		}
	}
	return srv.Addr(), stop
}

// tlsClient 用夹具里的证书当根证书，ServerName 写证书里的名字。
func tlsClient(t *testing.T, fixture tlsFixture) *http.Client {
	t.Helper()
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(fixture.certPEM) {
		t.Fatal("证书无法加入根池")
	}
	return &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "upanel.test", MinVersion: tls.VersionTLS12},
		},
	}
}

func TestServeTLSAnswersHealthOverHTTPS(t *testing.T) {
	fixture := newTLSFixture(t)
	addr, stop := newTLSServer(t, fixture.panel)
	defer stop()

	resp, err := tlsClient(t, fixture).Get("https://" + addr + "/api/v1/health/live")
	if err != nil {
		t.Fatalf("HTTPS 请求失败: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("状态码 = %d，期望 200；body=%s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Strict-Transport-Security"); got != hstsHeaderValue {
		t.Fatalf("HSTS = %q，期望 %q（开了 TLS 就要发；明文部署不发）", got, hstsHeaderValue)
	}

	state := resp.TLS
	if state == nil || len(state.PeerCertificates) == 0 {
		t.Fatal("响应里没有 TLS 状态：这次连接不是 TLS")
	}
	sum := sha256Hex(t, state.PeerCertificates[0].Raw)
	if sum != fixture.panel.Info.Fingerprint {
		t.Fatalf("握到的证书指纹 %s ≠ 夹具指纹 %s：443 上服务的不是配置里那张证书",
			sum, fixture.panel.Info.Fingerprint)
	}
}

func TestServeTLSRejectsPlaintextClient(t *testing.T) {
	fixture := newTLSFixture(t)
	addr, stop := newTLSServer(t, fixture.panel)
	defer stop()

	// 明文客户端连 TLS 端口。Go 的 TLS 监听器会自己回一句 400
	// "Client sent an HTTP request to an HTTPS server."——这不经过 gin，
	// 也就意味着明文请求拿不到任何接口数据。要断言的是这一点：
	// 响应不能是健康检查的 JSON，也不能带会话相关的头。
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("连接失败: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.Write([]byte("GET /api/v1/health/live HTTP/1.1\r\nHost: upanel.test\r\n\r\n")); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 512)
	n, readErr := conn.Read(buf)
	if readErr != nil {
		return // 直接断开也是一种拒绝，符合预期
	}
	text := string(buf[:n])
	if strings.Contains(text, `"status"`) || strings.Contains(text, "Set-Cookie") {
		t.Fatalf("明文请求拿到了接口响应：%q", text)
	}
	if !strings.Contains(text, "HTTPS") {
		t.Fatalf("明文请求应被 TLS 层拒绝并说明原因，实际：%q", text)
	}
}

func TestHSTSHeaderOnlyWhenTLS(t *testing.T) {
	// 明文部署（回环 + SSH 隧道）：绝不能发 HSTS，否则浏览器会记住
	// http://127.0.0.1:9443 要升级成 https，隧道从此打不开。
	srv := newTestServer(t, platform.ModeProduction, config.ListenProduction)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/health/live", nil)
	srv.Handler().ServeHTTP(rec, req)
	if got := rec.Header().Get("Strict-Transport-Security"); got != "" {
		t.Fatalf("明文部署下不应出现 HSTS，实际 %q", got)
	}
}

func sha256Hex(t *testing.T, data []byte) string {
	t.Helper()
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
