package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"upanel/internal/config"
	"upanel/internal/platform"
	"upanel/internal/version"
)

// stubFrontend 记录调用次数：用来验证“哪些路径会/不会交给静态处理器”。
type stubFrontend struct {
	calls []string
}

func (s *stubFrontend) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.calls = append(s.calls, r.Method+" "+r.URL.Path)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`<div id="app"></div>`))
}

func newServerWithFrontend(t *testing.T, frontend http.Handler) *Server {
	t.Helper()
	authSvc, aud, db := newTestServicesWithDB(t)
	srv, err := New(Options{
		ListenAddress:   config.ListenProduction,
		Mode:            platform.ModeProduction,
		Version:         version.Info{Version: "test", GoVersion: "test"},
		Logger:          quietLogger(),
		ShutdownTimeout: time.Second,
		Frontend:        frontend,
		Auth:            authSvc,
		Audit:           aud,
		Operations:      newTestOperations(t, db, nil),
		Apps:            newTestApps(t, db),
		Proxies:         newTestProxies(t, db),
		AllowedOrigins:  testOrigins(),
	})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	return srv
}

func TestFrontendServesPagePaths(t *testing.T) {
	frontend := &stubFrontend{}
	srv := newServerWithFrontend(t, frontend)

	for _, target := range []string{"/", "/storage", "/settings/network"} {
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: 期望 200，实际 %d", target, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), `id="app"`) {
			t.Fatalf("%s: 未交给前端处理器", target)
		}
	}
	if len(frontend.calls) != 3 {
		t.Fatalf("前端处理器调用次数 = %d，期望 3", len(frontend.calls))
	}
}

func TestUnknownAPINeverReachesFrontend(t *testing.T) {
	frontend := &stubFrontend{}
	srv := newServerWithFrontend(t, frontend)

	for _, target := range []string{
		"/api",
		"/api/",
		"/api/v1/nope",
		"/api/v1/health",
		"/api/v2/health/live",
	} {
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))

		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s: 未知 API 必须 404，实际 %d", target, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
			t.Fatalf("%s: 未知 API 必须返回 JSON，实际 Content-Type %q", target, ct)
		}
		if strings.Contains(rec.Body.String(), `<div id="app"`) {
			t.Fatalf("%s: 未知 API 返回了 index.html", target)
		}
		if !strings.Contains(rec.Body.String(), `"code":"NOT_FOUND"`) {
			t.Fatalf("%s: 错误码应为 NOT_FOUND，实际 %s", target, rec.Body.String())
		}
	}

	if len(frontend.calls) != 0 {
		t.Fatalf("API 前缀不应交给前端处理器，实际 %v", frontend.calls)
	}
}

func TestUnknownAPIPostAlsoJSON(t *testing.T) {
	frontend := &stubFrontend{}
	srv := newServerWithFrontend(t, frontend)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/unknown", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("状态码 = %d，期望 404", rec.Code)
	}
	if !strings.Contains(rec.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("Content-Type = %q", rec.Header().Get("Content-Type"))
	}
	if len(frontend.calls) != 0 {
		t.Fatalf("未知 API 的 POST 不应交给前端处理器：%v", frontend.calls)
	}
}

func TestNoFrontendKeepsJSON404(t *testing.T) {
	srv := newTestServer(t, platform.ModeProduction, config.ListenProduction)

	for _, target := range []string{"/", "/storage", "/api/v1/nope"} {
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s: 没有前端处理器时应全体 404，实际 %d", target, rec.Code)
		}
		if !strings.Contains(rec.Header().Get("Content-Type"), "application/json") {
			t.Fatalf("%s: 应返回 JSON，实际 %q", target, rec.Header().Get("Content-Type"))
		}
	}
}

func TestRegisteredAPIPathWrongMethodStaysJSON(t *testing.T) {
	frontend := &stubFrontend{}
	srv := newServerWithFrontend(t, frontend)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/health/live", nil))

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("状态码 = %d，期望 405", rec.Code)
	}
	if !strings.Contains(rec.Header().Get("Allow"), "GET") {
		t.Fatalf("缺少 Allow 头: %q", rec.Header().Get("Allow"))
	}
	if len(frontend.calls) != 0 {
		t.Fatalf("405 不应交给前端处理器：%v", frontend.calls)
	}
}
