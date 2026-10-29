package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"upanel/internal/certificates"
	"upanel/internal/config"
	"upanel/internal/platform"
	"upanel/internal/store"
	"upanel/internal/version"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func newTestServer(t *testing.T, mode platform.Mode, listen string) *Server {
	t.Helper()
	srv, _ := newTestServerWithDB(t, mode, listen)
	return srv
}

// newTestServerWithDB 与 newTestServer 相同，但把数据库句柄也交给用例。
// 需要直接改表来模拟“时间过去了”的用例用它，避免为了一个续期断言造第二套装配。
func newTestServerWithDB(t *testing.T, mode platform.Mode, listen string) (*Server, *store.DB) {
	t.Helper()
	authSvc, aud, db := newTestServicesWithDB(t)
	srv, err := New(Options{
		ListenAddress:     listen,
		Mode:              mode,
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
	})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	return srv, db
}

func TestHealthLiveShape(t *testing.T) {
	srv := newTestServer(t, platform.ModeProduction, config.ListenProduction)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/health/live", nil)
	req.Header.Set("X-Request-ID", "rid-health")

	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d", rec.Code)
	}
	if got := rec.Header().Get("X-Request-ID"); got != "rid-health" {
		t.Fatalf("X-Request-ID = %q", got)
	}

	var body struct {
		Data      map[string]any `json:"data"`
		RequestID string         `json:"request_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if body.Data["status"] != "ok" {
		t.Fatalf("data.status = %v", body.Data["status"])
	}
	if body.RequestID != "rid-health" {
		t.Fatalf("request_id = %q", body.RequestID)
	}
	// 契约：存活检查不得泄露内部路径或版本细节。
	raw := strings.ToLower(rec.Body.String())
	for _, forbidden := range []string{"/var/", "/etc/", "version", "built", "go1."} {
		if strings.Contains(raw, forbidden) {
			t.Fatalf("存活响应不应包含 %q：%s", forbidden, rec.Body.String())
		}
	}
}

func TestHealthLiveRejectsOtherMethods(t *testing.T) {
	srv := newTestServer(t, platform.ModeProduction, config.ListenProduction)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/health/live", strings.NewReader("{}"))

	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("状态码 = %d，期望 405", rec.Code)
	}
	if allow := rec.Header().Get("Allow"); allow != "GET, HEAD" {
		t.Fatalf("Allow = %q", allow)
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("错误响应不是合法 JSON: %v", err)
	}
	if body.Error.Code == "" {
		t.Fatal("错误响应必须带 code")
	}
}

func TestUnknownPathReturnsJSON404(t *testing.T) {
	srv := newTestServer(t, platform.ModeProduction, config.ListenProduction)
	for _, path := range []string{"/", "/api/v1/unknown", "/index.html", "/api/v2/health/live"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		srv.Handler().ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s: 状态码 = %d，期望 404", path, rec.Code)
		}
		var body struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s: 响应不是 JSON（不能回落到 HTML）: %v", path, err)
		}
		if body.Error.Code != "NOT_FOUND" {
			t.Fatalf("%s: code = %q", path, body.Error.Code)
		}
		if strings.Contains(strings.ToLower(rec.Body.String()), "<html") {
			t.Fatalf("%s: 未知路径不得返回 HTML 页面", path)
		}
	}
}

func TestMockModeMustBindLoopback18881(t *testing.T) {
	authSvc, aud, db := newTestServicesWithDB(t)
	preflights := newTestOperations(t, db, nil)
	appSvc := newTestApps(t, db)
	if _, err := New(Options{
		ListenAddress:     config.ListenProduction,
		Mode:              platform.ModeMock,
		Logger:            quietLogger(),
		ShutdownTimeout:   time.Second,
		Auth:              authSvc,
		Audit:             aud,
		Operations:        preflights,
		Apps:              appSvc,
		Proxies:           newTestProxies(t, db),
		Certificates:      newTestCertificates(t, db),
		CertificateStages: certificates.NewStageStore(nil),
		AllowedOrigins:    testOrigins(),
	}); err == nil {
		t.Fatal("mock 模式绑定 9443 必须被拒绝")
	}
	if _, err := New(Options{
		ListenAddress:     "0.0.0.0:18881",
		Mode:              platform.ModeMock,
		Logger:            quietLogger(),
		ShutdownTimeout:   time.Second,
		Auth:              authSvc,
		Audit:             aud,
		Operations:        preflights,
		Apps:              appSvc,
		Proxies:           newTestProxies(t, db),
		Certificates:      newTestCertificates(t, db),
		CertificateStages: certificates.NewStageStore(nil),
		AllowedOrigins:    testOrigins(),
	}); err == nil {
		t.Fatal("mock 模式绑定 0.0.0.0 必须被拒绝")
	}
	if _, err := New(Options{
		ListenAddress:     config.ListenMock,
		Mode:              platform.ModeMock,
		Logger:            quietLogger(),
		ShutdownTimeout:   time.Second,
		Auth:              authSvc,
		Audit:             aud,
		Operations:        preflights,
		Apps:              appSvc,
		Proxies:           newTestProxies(t, db),
		Certificates:      newTestCertificates(t, db),
		CertificateStages: certificates.NewStageStore(nil),
		AllowedOrigins:    testOrigins(),
	}); err != nil {
		t.Fatalf("mock 模式绑定 127.0.0.1:18881 应成功: %v", err)
	}
}

func TestNewRejectsInvalidOptions(t *testing.T) {
	authSvc, aud, db := newTestServicesWithDB(t)
	base := Options{
		ListenAddress:     config.ListenProduction,
		Mode:              platform.ModeProduction,
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
	}
	if _, err := New(Options{ListenAddress: config.ListenProduction, Mode: base.Mode, ShutdownTimeout: time.Second}); err == nil {
		t.Fatal("缺少 Logger 必须报错")
	}
	badAuth := base
	badAuth.Auth = nil
	if _, err := New(badAuth); err == nil {
		t.Fatal("缺少认证服务必须报错")
	}
	badOrigins := base
	badOrigins.AllowedOrigins = nil
	if _, err := New(badOrigins); err == nil {
		t.Fatal("空的允许源必须报错")
	}
	bad := base
	bad.Mode = platform.Mode("demo")
	if _, err := New(bad); err == nil {
		t.Fatal("非法模式必须报错")
	}
	bad = base
	bad.ListenAddress = ""
	if _, err := New(bad); err == nil {
		t.Fatal("空监听地址必须报错")
	}
	bad = base
	bad.ShutdownTimeout = 0
	if _, err := New(bad); err == nil {
		t.Fatal("停止超时必须为正")
	}
}

// TestServeBindsAndStops 真实监听一个临时端口，验证启动与优雅停止路径。
func TestServeBindsAndStops(t *testing.T) {
	srv := newTestServer(t, platform.ModeProduction, "127.0.0.1:0")
	ctx, cancel := context.WithCancel(context.Background())

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ctx) }()

	// 等待监听就绪：轮询内部地址不可行（端口为 0 时由内核分配），
	// 因此改为等待 Serve 已进入运行态的最短时间后立即停止，只验证不报错退出。
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("优雅停止不应返回错误: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve 未在超时内退出")
	}
}

// TestWriteTimeoutCoversWorstCaseAuth 钉住写超时的下限。
//
// 登录要算 Argon2id（128 MiB、3 轮）：开发宿主上约 110 ms，单核慢机器上实测
// 22—30 秒。写超时一旦比这短，net/http 会在 handler 算完之后把连接关掉，
// 客户端只看到"连接被断开"（curl 拿到 000），连"密码错了"都读不到——
// 24.04 验收机上一轮就是这么红的：服务端日志写着 200，客户端拿到的却是 000。
func TestWriteTimeoutCoversWorstCaseAuth(t *testing.T) {
	srv := newTestServer(t, platform.ModeProduction, config.ListenProduction)
	if got := srv.http.WriteTimeout; got < 2*time.Minute {
		t.Fatalf("写超时必须容得下一次最慢的认证校验（实测 30 秒），实际 %s", got)
	}
}
