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
	"upanel/internal/certificates"
	"upanel/internal/config"
	"upanel/internal/metrics"
	"upanel/internal/platform"
	"upanel/internal/store"
	"upanel/internal/version"
)

// stubMetrics 是历史监控读取端的替身：只验证 HTTP 层怎么映射参数、错误和空态。
// 采集与计算的行为由 internal/metrics 的测试覆盖，不在这里重测。
type stubMetrics struct {
	res       metrics.Result
	err       error
	status    metrics.Status
	statusErr error

	queries    int
	lastParams metrics.QueryParams
}

func (s *stubMetrics) Query(ctx context.Context, params metrics.QueryParams) (metrics.Result, error) {
	s.queries++
	s.lastParams = params
	if s.err != nil {
		return metrics.Result{}, s.err
	}
	return s.res, nil
}

func (s *stubMetrics) Status(ctx context.Context) (metrics.Status, error) {
	if s.statusErr != nil {
		return metrics.Status{}, s.statusErr
	}
	return s.status, nil
}

// metricsFixture 用真实数据库与会话装配一个带历史读取端的服务。
func metricsFixture(t *testing.T, reader MetricsReader) (*Server, *http.Cookie) {
	t.Helper()
	authSvc, aud, db := newTestServicesWithDB(t)
	srv, err := New(Options{
		ListenAddress:     config.ListenProduction,
		Mode:              platform.ModeProduction,
		Version:           version.Info{Version: "test", GoVersion: "test"},
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
		Metrics:           reader,
	})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	seedTestUser(t, srv.opts.Auth, testUser, testUserPass)
	cookie, _ := loginOK(t, srv)
	return srv, cookie
}

func metricsGet(t *testing.T, srv *Server, cookie *http.Cookie, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Origin", testOrigin)
	if cookie != nil {
		req.Header.Set("Cookie", cookie.String())
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func TestSystemMetricsRequiresSession(t *testing.T) {
	srv, _ := metricsFixture(t, &stubMetrics{})

	rec := metricsGet(t, srv, nil, "/api/v1/system/metrics?scope=host")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("未登录状态码 = %d，期望 401", rec.Code)
	}
	if got := parseResult(t, rec); got.Error == nil || got.Error.Code != "UNAUTHENTICATED" {
		t.Fatalf("错误码 = %+v", got.Error)
	}
}

func TestSystemMetricsStatusRequiresSession(t *testing.T) {
	srv, _ := metricsFixture(t, &stubMetrics{})

	rec := metricsGet(t, srv, nil, "/api/v1/system/metrics/status")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("未登录状态码 = %d，期望 401", rec.Code)
	}
}

// CONTRACTS.md：GET /system/metrics 的权限是 V，viewer 也能看。
func TestSystemMetricsAllowedForViewer(t *testing.T) {
	reader := &stubMetrics{res: metrics.Result{
		Enabled: true, Scope: store.MetricScopeHost, StepSeconds: 60,
		Resolution: store.MetricResolutionRaw, MaxPoints: metrics.MaxPoints,
		Series: []metrics.Series{{
			Scope: store.MetricScopeHost, Metric: "cpu_usage_percent", Unit: "percent",
			Points: []metrics.Point{{Timestamp: time.Date(2026, 10, 25, 8, 0, 0, 0, time.UTC), Value: 12.5, Min: 12, Max: 13, SampleCount: 12}},
		}},
	}}
	srv, _ := metricsFixture(t, reader)
	if _, err := srv.opts.Auth.CreateUser(context.Background(), auth.CreateUserInput{
		Username: "alice", DisplayName: "alice", Role: auth.RoleViewer, Password: "alice-horse-9",
	}); err != nil {
		t.Fatalf("创建 viewer 失败: %v", err)
	}
	viewerCookie, _ := loginAsUser(t, srv, "alice", "alice-horse-9")

	rec := metricsGet(t, srv, viewerCookie, "/api/v1/system/metrics?scope=host&metric=cpu_usage_percent")
	if rec.Code != http.StatusOK {
		t.Fatalf("viewer 状态码 = %d，期望 200（%s）", rec.Code, rec.Body.String())
	}
	var body struct {
		Data metrics.Result `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if !body.Data.Enabled || len(body.Data.Series) != 1 {
		t.Fatalf("响应内容不对: %+v", body.Data)
	}
	if reader.lastParams.Scope != store.MetricScopeHost {
		t.Fatalf("scope 没有传到底层: %q", reader.lastParams.Scope)
	}
}

func TestSystemMetricsParsesQueryParams(t *testing.T) {
	reader := &stubMetrics{res: metrics.Result{Enabled: true, Scope: store.MetricScopeInterface}}
	srv, cookie := metricsFixture(t, reader)

	// 重复参数与逗号分隔都接受，空项丢掉。
	path := "/api/v1/system/metrics?scope=interface" +
		"&target_id=eth0,eth1&target_id=eth2" +
		"&metric=rx_bytes_per_sec&metric=tx_bytes_per_sec,bogus_empty" +
		"&from=2026-10-25T08:00:00Z&to=2026-10-25T09:00:00Z&step=30"
	rec := metricsGet(t, srv, cookie, path)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（%s）", rec.Code, rec.Body.String())
	}
	got := reader.lastParams
	if len(got.TargetIDs) != 3 || got.TargetIDs[2] != "eth2" {
		t.Fatalf("target_id 解析 = %v", got.TargetIDs)
	}
	if len(got.Metrics) != 3 || got.Metrics[1] != "tx_bytes_per_sec" {
		t.Fatalf("metric 解析 = %v", got.Metrics)
	}
	if got.StepSeconds != 30 {
		t.Fatalf("step = %d", got.StepSeconds)
	}
	if !got.From.Equal(time.Date(2026, 10, 25, 8, 0, 0, 0, time.UTC)) {
		t.Fatalf("from = %v", got.From)
	}
	if !got.To.Equal(time.Date(2026, 10, 25, 9, 0, 0, 0, time.UTC)) {
		t.Fatalf("to = %v", got.To)
	}
}

func TestSystemMetricsRejectsMalformedParams(t *testing.T) {
	cases := []struct {
		name string
		path string
	}{
		{"from 不是 RFC3339", "/api/v1/system/metrics?scope=host&from=昨天"},
		{"to 不是 RFC3339", "/api/v1/system/metrics?scope=host&to=2026-10-25"},
		{"step 不是数字", "/api/v1/system/metrics?scope=host&step=abc"},
		{"step 是 0", "/api/v1/system/metrics?scope=host&step=0"},
		{"step 大于一年秒数", "/api/v1/system/metrics?scope=host&step=31536001"},
	}
	for _, c := range cases {
		reader := &stubMetrics{res: metrics.Result{Enabled: true}}
		srv, cookie := metricsFixture(t, reader)
		rec := metricsGet(t, srv, cookie, c.path)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: 状态码 = %d，期望 400（%s）", c.name, rec.Code, rec.Body.String())
		}
		got := parseResult(t, rec)
		if got.Error == nil || got.Error.Code != "VALIDATION_ERROR" {
			t.Fatalf("%s: 错误码 = %+v", c.name, got.Error)
		}
		if reader.queries != 0 {
			t.Fatalf("%s: 参数就不合法，不该问到下层", c.name)
		}
	}
}

// errMetricsBoom 是“底层真的出错了”的替身。
var errMetricsBoom = errors.New("sqlite: database is locked")

// 未知指标、缺 scope 这类判断在 metrics 包里做（它知道指标登记表），
// API 层只负责把 InvalidQueryError 翻成 400 —— 翻成 500 会让人以为服务器坏了。
func TestSystemMetricsMapsInvalidQueryTo400(t *testing.T) {
	srv, cookie := metricsFixture(t, &stubMetrics{
		err: &metrics.InvalidQueryError{Message: `指标 "nope" 不属于 scope "host"`},
	})

	rec := metricsGet(t, srv, cookie, "/api/v1/system/metrics?scope=host&metric=nope")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400", rec.Code)
	}
	got := parseResult(t, rec)
	if got.Error == nil || got.Error.Code != "VALIDATION_ERROR" {
		t.Fatalf("错误码 = %+v", got.Error)
	}
	if !strings.Contains(got.Error.Message, "nope") {
		t.Fatalf("错误信息应当说明是哪个指标: %q", got.Error.Message)
	}
}

func TestSystemMetricsMapsInternalErrorTo500(t *testing.T) {
	srv, cookie := metricsFixture(t, &stubMetrics{err: errMetricsBoom})
	rec := metricsGet(t, srv, cookie, "/api/v1/system/metrics?scope=host")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("状态码 = %d，期望 500", rec.Code)
	}
	// 500 的正文不能带内部错误原文（泄漏实现细节）。
	if strings.Contains(rec.Body.String(), errMetricsBoom.Error()) {
		t.Fatalf("响应泄漏了内部错误: %s", rec.Body.String())
	}
}

func TestSystemMetricsWithoutReaderSaysDisabled(t *testing.T) {
	srv, cookie := metricsFixture(t, nil)
	rec := metricsGet(t, srv, cookie, "/api/v1/system/metrics?scope=host")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（未启用不是错误）", rec.Code)
	}
	var body struct {
		Data struct {
			Enabled bool             `json:"enabled"`
			Reason  string           `json:"reason"`
			Series  []metrics.Series `json:"series"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if body.Data.Enabled {
		t.Fatal("没有采集器时 enabled 必须是 false")
	}
	if body.Data.Reason == "" {
		t.Fatal("必须说明为什么没有历史")
	}
	// series 必须是 []，不能是 null：接口层要能直接 .length。
	if !strings.Contains(rec.Body.String(), `"series":[]`) {
		t.Fatalf("series 应当是空数组: %s", rec.Body.String())
	}
}

func TestSystemMetricsStatusShape(t *testing.T) {
	lastSample := time.Date(2026, 10, 25, 8, 0, 5, 0, time.UTC)
	st := metrics.Status{
		Enabled: true, Collecting: true, IntervalSeconds: 2,
		MaxRows:      2_000_000,
		Samples:      map[string]int64{"total": 42, store.MetricResolutionRaw: 42},
		LastSampleAt: &lastSample,
		Metrics:      metrics.Definitions(),
		Series:       map[string][]metrics.MetricSeriesRef{},
	}
	st.Retention.RawSeconds = 86400
	st.Retention.MinuteSeconds = 2592000
	st.Retention.HourSeconds = 31536000
	reader := &stubMetrics{status: st}
	srv, cookie := metricsFixture(t, reader)

	rec := metricsGet(t, srv, cookie, "/api/v1/system/metrics/status")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（%s）", rec.Code, rec.Body.String())
	}
	var body struct {
		Data struct {
			Enabled    bool                                 `json:"enabled"`
			Collecting bool                                 `json:"collecting"`
			Samples    map[string]int64                     `json:"samples"`
			Metrics    []metrics.Definition                 `json:"metrics"`
			Series     map[string][]metrics.MetricSeriesRef `json:"series"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if !body.Data.Enabled || !body.Data.Collecting {
		t.Fatalf("状态字段不对: %+v", body.Data)
	}
	if body.Data.Samples["total"] != 42 {
		t.Fatalf("samples.total = %d", body.Data.Samples["total"])
	}
	if len(body.Data.Metrics) == 0 {
		t.Fatal("状态里应当带上登记的指标（界面据此画选择框）")
	}
	if strings.Contains(rec.Body.String(), `"series":null`) {
		t.Fatalf("series 应当是对象而不是 null: %s", rec.Body.String())
	}
}

func TestSystemMetricsStatusWithoutReader(t *testing.T) {
	authSvc, aud, db := newTestServicesWithDB(t)
	srv, err := New(Options{
		ListenAddress:     config.ListenMock,
		Mode:              platform.ModeMock,
		Version:           version.Info{Version: "test", GoVersion: "test"},
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
	seedTestUser(t, srv.opts.Auth, testUser, testUserPass)
	adminCookie, _ := loginOK(t, srv)

	rec := metricsGet(t, srv, adminCookie, "/api/v1/system/metrics/status")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", rec.Code)
	}
	var body struct {
		Data struct {
			Enabled bool   `json:"enabled"`
			Reason  string `json:"reason"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if body.Data.Enabled {
		t.Fatal("演示模式下 enabled 必须是 false")
	}
	if !strings.Contains(body.Data.Reason, "演示模式") {
		t.Fatalf("演示模式的原因要写清楚: %q", body.Data.Reason)
	}
}

func TestSystemMetricsRejectsPost(t *testing.T) {
	srv, cookie := metricsFixture(t, &stubMetrics{})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/system/metrics", strings.NewReader(`{"scope":"host"}`))
	req.Header.Set("Origin", testOrigin)
	req.Header.Set("Cookie", cookie.String())
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("状态码 = %d，期望 405", rec.Code)
	}
}
