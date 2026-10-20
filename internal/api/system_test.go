package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"upanel/internal/auth"
	"upanel/internal/config"
	"upanel/internal/monitoring"
	"upanel/internal/platform"
	"upanel/internal/version"
)

// stubMonitor 是 Monitor 的测试替身：只用来验证 HTTP 层的映射（状态码、错误码、透传）。
// 采集本身的行为由 internal/monitoring 的测试覆盖，不在这里重测。
type stubMonitor struct {
	sum   monitoring.Summary
	err   error
	calls int
}

func (s *stubMonitor) Summary(ctx context.Context) (monitoring.Summary, error) {
	s.calls++
	return s.sum, s.err
}

// systemFixture 是系统信息用例的固定装置：真实数据库、真实会话、可替换的采集端。
func systemFixture(t *testing.T, mon Monitor) (*Server, *http.Cookie) {
	t.Helper()
	authSvc, aud, db := newTestServicesWithDB(t)
	srv, err := New(Options{
		ListenAddress:   config.ListenProduction,
		Mode:            platform.ModeProduction,
		Version:         version.Info{Version: "test", GoVersion: "test"},
		Logger:          quietLogger(),
		ShutdownTimeout: time.Second,
		Auth:            authSvc,
		Audit:           aud,
		Operations:      newTestOperations(t, db, nil),
		AllowedOrigins:  testOrigins(),
		Monitor:         mon,
	})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	seedTestUser(t, srv.opts.Auth, testUser, testUserPass)
	cookie, _ := loginOK(t, srv)
	return srv, cookie
}

func systemGet(t *testing.T, srv *Server, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/system/summary", nil)
	req.Header.Set("Origin", testOrigin)
	if cookie != nil {
		req.Header.Set("Cookie", cookie.String())
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func sampleSummary() monitoring.Summary {
	collected := time.Date(2026, 10, 20, 8, 0, 5, 0, time.UTC)
	total := uint64(4009864 * 1024)
	usage := 12.5
	return monitoring.Summary{
		CollectedAt: collected,
		IntervalMS:  5000,
		Host: monitoring.HostSection{
			Section:  monitoring.Section{Status: monitoring.StatusOK, CollectedAt: &collected},
			Hostname: "upanel-2404", OSID: "ubuntu", OSVersion: "24.04",
		},
		CPU: monitoring.CPUSection{
			Section: monitoring.Section{Status: monitoring.StatusOK}, LogicalCores: 2,
			UsagePercent: &usage,
		},
		Memory: monitoring.MemorySection{
			Section: monitoring.Section{Status: monitoring.StatusOK}, TotalBytes: &total,
		},
		Load:    monitoring.LoadSection{Section: monitoring.Section{Status: monitoring.StatusOK}},
		Network: monitoring.NetworkSection{Section: monitoring.Section{Status: monitoring.StatusOK}},
	}
}

func TestSystemSummaryRequiresSession(t *testing.T) {
	srv, _ := systemFixture(t, &stubMonitor{sum: sampleSummary()})

	rec := systemGet(t, srv, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("未登录状态码 = %d，期望 401", rec.Code)
	}
	got := parseResult(t, rec)
	if got.Error == nil || got.Error.Code != "UNAUTHENTICATED" {
		t.Fatalf("错误码 = %+v", got.Error)
	}
}

func TestSystemSummaryAllowedForViewer(t *testing.T) {
	srv, _ := systemFixture(t, &stubMonitor{sum: sampleSummary()})
	// CONTRACTS.md 第 3 节：GET /system/summary 的权限是 V，viewer 也能看。
	if _, err := srv.opts.Auth.CreateUser(context.Background(), auth.CreateUserInput{
		Username: "alice", DisplayName: "alice", Role: auth.RoleViewer, Password: "alice-horse-9",
	}); err != nil {
		t.Fatalf("创建 viewer 失败: %v", err)
	}
	viewerCookie, _ := loginAsUser(t, srv, "alice", "alice-horse-9")

	rec := systemGet(t, srv, viewerCookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("viewer 状态码 = %d，期望 200（%s）", rec.Code, rec.Body.String())
	}
	var body struct {
		Data monitoring.Summary `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if body.Data.Host.Hostname != "upanel-2404" {
		t.Fatalf("主机名 = %q", body.Data.Host.Hostname)
	}
	if !body.Data.CollectedAt.Equal(sampleSummary().CollectedAt) {
		t.Fatalf("collected_at 被改写了: %v", body.Data.CollectedAt)
	}
}

func TestSystemSummaryPassesThroughDegraded(t *testing.T) {
	sum := sampleSummary()
	sum.Degraded = true
	sum.Network = monitoring.NetworkSection{
		Section: monitoring.Section{Status: monitoring.StatusUnavailable, Error: "open /proc/net/dev: no such file"},
	}
	srv, cookie := systemFixture(t, &stubMonitor{sum: sum})

	rec := systemGet(t, srv, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("部分采集失败仍然应回 200（数据里带状态），实际 %d", rec.Code)
	}
	var body struct {
		Data monitoring.Summary `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if !body.Data.Degraded {
		t.Fatal("degraded 被抹掉了")
	}
	if body.Data.Network.Status != monitoring.StatusUnavailable || body.Data.Network.Error == "" {
		t.Fatalf("网卡段的失败状态没有透传: %+v", body.Data.Network.Section)
	}
	if body.Data.Network.Interfaces != nil {
		t.Fatal("失败段不应凭空出现网卡列表")
	}
}

func TestSystemSummaryWithoutMonitorReturns503(t *testing.T) {
	// 演示模式或未配置 socket：没有采集端，接口必须明确说不可用。
	srv, cookie := systemFixture(t, nil)

	rec := systemGet(t, srv, cookie)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("状态码 = %d，期望 503", rec.Code)
	}
	got := parseResult(t, rec)
	if got.Error == nil || got.Error.Code != "AGENT_UNAVAILABLE" {
		t.Fatalf("错误码 = %+v，期望 AGENT_UNAVAILABLE", got.Error)
	}
	if strings.Contains(rec.Body.String(), "upanel-2404") {
		t.Fatal("不可用时不能回落到演示数据")
	}
}

func TestSystemSummaryMapsAgentUnavailable(t *testing.T) {
	srv, cookie := systemFixture(t, &stubMonitor{err: monitoring.ErrAgentUnavailable})

	rec := systemGet(t, srv, cookie)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("状态码 = %d，期望 503", rec.Code)
	}
	got := parseResult(t, rec)
	if got.Error == nil || got.Error.Code != "AGENT_UNAVAILABLE" {
		t.Fatalf("错误码 = %+v", got.Error)
	}
}

func TestSystemSummaryMapsCollectionFailure(t *testing.T) {
	srv, cookie := systemFixture(t, &stubMonitor{err: errors.New("代理返回 500 COMMAND_FAILED: 查询执行失败")})

	rec := systemGet(t, srv, cookie)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("状态码 = %d，期望 500", rec.Code)
	}
	got := parseResult(t, rec)
	if got.Error == nil || got.Error.Code != "COMMAND_FAILED" {
		t.Fatalf("错误码 = %+v，期望 COMMAND_FAILED", got.Error)
	}
}

func TestSystemSummaryRejectsNonGET(t *testing.T) {
	srv, cookie := systemFixture(t, &stubMonitor{sum: sampleSummary()})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/system/summary", strings.NewReader("{}"))
	req.Header.Set("Origin", testOrigin)
	req.Header.Set("Cookie", cookie.String())
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST 状态码 = %d，期望 405", rec.Code)
	}
}
