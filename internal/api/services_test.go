package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"upanel/internal/auth"
	"upanel/internal/config"
	"upanel/internal/platform"
	"upanel/internal/services"
	"upanel/internal/version"
)

// stubServices 是 ServicesReader 的测试替身：只验证 HTTP 层映射
// （状态码、错误码、404、空列表归一化）。采集与 systemctl 语义由
// internal/services 的测试覆盖，这里不冒充真实采集。
type stubServices struct {
	snap     services.Snapshot
	unit     services.Unit
	found    bool
	listErr  error
	unitErr  error
	lastUnit string
}

func (s *stubServices) Snapshot(context.Context) (services.Snapshot, error) {
	if s.listErr != nil {
		return services.Snapshot{}, s.listErr
	}
	return s.snap, nil
}

func (s *stubServices) Unit(_ context.Context, name string) (services.Unit, bool, error) {
	s.lastUnit = name
	if s.unitErr != nil {
		return services.Unit{}, false, s.unitErr
	}
	return s.unit, s.found, nil
}

func servicesFixture(t *testing.T, reader ServicesReader) (*Server, *http.Cookie) {
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
		Apps:            newTestApps(t, db),
		Proxies:         newTestProxies(t, db),
		AllowedOrigins:  testOrigins(),
		Services:        reader,
	})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	seedTestUser(t, srv.opts.Auth, testUser, testUserPass)
	cookie, _ := loginOK(t, srv)
	return srv, cookie
}

func servicesGet(t *testing.T, srv *Server, cookie *http.Cookie, path string) *httptest.ResponseRecorder {
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

// sampleServiceUnit 是一份最小但合法的单元状态（字段形态与真实 systemctl 一致，
// 数值来自 24.04 测试机；这里只用于 HTTP 层断言）。
func sampleServiceUnit() services.Unit {
	pid := int64(784)
	mem := "483328"
	restart := int64(0)
	tasks := int64(1)
	started := time.Date(2026, 10, 21, 0, 16, 5, 0, time.UTC)
	return services.Unit{
		Unit:          "cron.service",
		Description:   "Regular background program processing daemon",
		LoadState:     services.LoadLoaded,
		ActiveState:   services.ActiveActive,
		SubState:      "running",
		UnitFileState: "enabled",
		Enablement:    services.EnablementEnabled,
		MainPID:       &pid,
		StartedAt:     &started,
		RestartCount:  &restart,
		MemoryBytes:   &mem,
		TasksCurrent:  &tasks,
		FragmentPath:  "/usr/lib/systemd/system/cron.service",
		Protection:    services.Protection{Reasons: []string{}},
		Capabilities:  []string{services.OpStop, services.OpRestart, services.OpDisable},
		CheckedAt:     started,
	}
}

func sampleServiceSnapshot() services.Snapshot {
	return services.Snapshot{
		Status:          services.StatusOK,
		CollectedAt:     time.Date(2026, 10, 21, 0, 20, 0, 0, time.UTC),
		IntervalMS:      10000,
		SnapshotVersion: "sha256:abc",
		Units:           []services.Unit{sampleServiceUnit()},
	}
}

// TestServicesRoutesRequireSession 未登录一律 401。
func TestServicesRoutesRequireSession(t *testing.T) {
	srv, _ := servicesFixture(t, &stubServices{snap: sampleServiceSnapshot(), unit: sampleServiceUnit(), found: true})
	for _, path := range []string{"/api/v1/services", "/api/v1/services/cron.service"} {
		rec := servicesGet(t, srv, nil, path)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: 未登录状态码 = %d，期望 401", path, rec.Code)
		}
		got := parseResult(t, rec)
		if got.Error == nil || got.Error.Code != "UNAUTHENTICATED" {
			t.Errorf("%s: 错误码 = %+v", path, got.Error)
		}
	}
}

// TestServicesRoutesAllowedForViewer 权限 V：viewer 也能看服务状态。
func TestServicesRoutesAllowedForViewer(t *testing.T) {
	srv, _ := servicesFixture(t, &stubServices{snap: sampleServiceSnapshot(), unit: sampleServiceUnit(), found: true})
	if _, err := srv.opts.Auth.CreateUser(context.Background(), auth.CreateUserInput{
		Username: "alice", DisplayName: "alice", Role: auth.RoleViewer, Password: "alice-horse-9",
	}); err != nil {
		t.Fatalf("建 viewer 失败: %v", err)
	}
	viewerCookie, _ := loginAsUser(t, srv, "alice", "alice-horse-9")
	for _, path := range []string{"/api/v1/services", "/api/v1/services/cron.service"} {
		rec := servicesGet(t, srv, viewerCookie, path)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: viewer 状态码 = %d，期望 200（%s）", path, rec.Code, rec.Body.String())
		}
	}
}

// TestServiceListPassesThrough 列表原样透传采集结果。
func TestServiceListPassesThrough(t *testing.T) {
	srv, cookie := servicesFixture(t, &stubServices{snap: sampleServiceSnapshot()})
	rec := servicesGet(t, srv, cookie, "/api/v1/services")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d（%s）", rec.Code, rec.Body.String())
	}
	var body struct {
		Data serviceListResponse `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	d := body.Data
	if d.Status != services.StatusOK || d.SnapshotVersion != "sha256:abc" || d.IntervalMS != 10000 {
		t.Errorf("外层字段 = %+v", d)
	}
	if len(d.Units) != 1 || d.Units[0].Unit != "cron.service" {
		t.Fatalf("单元列表 = %+v", d.Units)
	}
	u := d.Units[0]
	if u.Enablement != services.EnablementEnabled || u.ActiveState != services.ActiveActive {
		t.Errorf("状态字段 = %+v", u)
	}
	if strings.Join(u.Capabilities, ",") != "stop,restart,disable" {
		t.Errorf("能力字段 = %v", u.Capabilities)
	}
	// 字节与时间必须是可解析的字符串，不是浮点数。
	if !strings.Contains(rec.Body.String(), `"memory_bytes":"483328"`) {
		t.Errorf("memory_bytes 不是十进制字符串: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"started_at":"2026-10-21T00:16:05Z"`) {
		t.Errorf("started_at 格式不对: %s", rec.Body.String())
	}
}

// TestServiceListNilUnitsNormalized 采集失败时 units 必须是 []，不是 null。
func TestServiceListNilUnitsNormalized(t *testing.T) {
	srv, cookie := servicesFixture(t, &stubServices{snap: services.Snapshot{
		Status: services.StatusError, Error: "服务状态尚未完成第一次采集",
		IntervalMS: 10000, Units: nil,
	}})
	rec := servicesGet(t, srv, cookie, "/api/v1/services")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d（%s）", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"units":[]`) {
		t.Errorf("units 应归一化为空数组: %s", rec.Body.String())
	}
	var body struct {
		Data serviceListResponse `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	// 失败原因要透出，页面才能说清“为什么没有数据”。
	if body.Data.Status != services.StatusError || body.Data.Error == "" {
		t.Errorf("失败状态没有透出: %+v", body.Data)
	}
}

// TestServiceDetailPassesThrough 详情是现场读取的单单元。
func TestServiceDetailPassesThrough(t *testing.T) {
	stub := &stubServices{unit: sampleServiceUnit(), found: true}
	srv, cookie := servicesFixture(t, stub)
	rec := servicesGet(t, srv, cookie, "/api/v1/services/cron.service")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d（%s）", rec.Code, rec.Body.String())
	}
	if stub.lastUnit != "cron.service" {
		t.Errorf("传给读取端的单元名 = %q", stub.lastUnit)
	}
	var body struct {
		Data serviceDetailResponse `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if body.Data.Unit.Unit != "cron.service" || body.Data.Unit.MainPID == nil {
		t.Errorf("详情字段 = %+v", body.Data.Unit)
	}
	if len(body.Data.Unit.Protection.Reasons) != 0 {
		t.Errorf("未受保护单元不应有原因: %v", body.Data.Unit.Protection.Reasons)
	}
}

// TestServiceDetailNotFound 单元不存在 → 404（不是 500，也不是空对象）。
func TestServiceDetailNotFound(t *testing.T) {
	srv, cookie := servicesFixture(t, &stubServices{found: false})
	rec := servicesGet(t, srv, cookie, "/api/v1/services/ghost.service")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("状态码 = %d，期望 404（%s）", rec.Code, rec.Body.String())
	}
	got := parseResult(t, rec)
	if got.Error == nil || got.Error.Code != "NOT_FOUND" {
		t.Errorf("错误码 = %+v", got.Error)
	}
}

// TestServiceDetailTemplateUnit 模板单元在 API 层就说清楚“需要实例”，不进代理。
//
// systemctl show 不接受 `foo@.service` 这种名字（会中断整次调用），列表里也不出现它，
// 所以这里必须是 404 + 一句能看懂的解释，而不是让代理去试、再报一个内部错误。
func TestServiceDetailTemplateUnit(t *testing.T) {
	stub := &stubServices{unit: sampleServiceUnit(), found: true}
	srv, cookie := servicesFixture(t, stub)
	rec := servicesGet(t, srv, cookie, "/api/v1/services/serial-getty@.service")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("状态码 = %d，期望 404（%s）", rec.Code, rec.Body.String())
	}
	got := parseResult(t, rec)
	if got.Error == nil || got.Error.Code != "NOT_FOUND" {
		t.Fatalf("错误码 = %+v", got.Error)
	}
	if !strings.Contains(got.Error.Message, "实例") {
		t.Errorf("错误消息要说清“需要指定实例”，实际 %q", got.Error.Message)
	}
	if stub.lastUnit != "" {
		t.Errorf("模板单元不应到达读取端，实际传了 %q", stub.lastUnit)
	}
}

// TestServiceDetailRejectsInvalidName 非法单元名在 API 层就被拒，不进代理。
func TestServiceDetailRejectsInvalidName(t *testing.T) {
	stub := &stubServices{unit: sampleServiceUnit(), found: true}
	srv, cookie := servicesFixture(t, stub)
	for _, path := range []string{
		"/api/v1/services/cron",         // 不是 .service
		"/api/v1/services/cron.socket",  // 非 service 类型
		"/api/v1/services/-rf.service",  // 以 - 开头
		"/api/v1/services/a..b.service", // 双点
	} {
		rec := servicesGet(t, srv, cookie, path)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: 状态码 = %d，期望 400（%s）", path, rec.Code, rec.Body.String())
		}
		got := parseResult(t, rec)
		if got.Error == nil || got.Error.Code != "VALIDATION_ERROR" {
			t.Errorf("%s: 错误码 = %+v", path, got.Error)
		}
	}
	if stub.lastUnit != "" {
		t.Errorf("非法单元名不应到达读取端，实际传了 %q", stub.lastUnit)
	}
}

// TestServicesUnavailableWhenReaderMissing 没有配置代理时是 503，不是空列表。
func TestServicesUnavailableWhenReaderMissing(t *testing.T) {
	srv, cookie := servicesFixture(t, nil)
	rec := servicesGet(t, srv, cookie, "/api/v1/services")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("列表状态码 = %d，期望 503（%s）", rec.Code, rec.Body.String())
	}
	got := parseResult(t, rec)
	if got.Error == nil || got.Error.Code != "AGENT_UNAVAILABLE" {
		t.Errorf("错误码 = %+v", got.Error)
	}
	rec = servicesGet(t, srv, cookie, "/api/v1/services/cron.service")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("详情状态码 = %d，期望 503（%s）", rec.Code, rec.Body.String())
	}
}

// TestServicesErrorMapping 读取端错误如实翻译成状态码。
func TestServicesErrorMapping(t *testing.T) {
	cases := []struct {
		name string
		err  error
		code int
		want string
	}{
		{"代理不可用", fmt.Errorf("%w: socket", services.ErrAgentUnavailable), http.StatusServiceUnavailable, "AGENT_UNAVAILABLE"},
		{"依赖缺失", fmt.Errorf("%w: /usr/bin/systemctl", services.ErrSystemctlUnavailable), http.StatusServiceUnavailable, "DEPENDENCY_MISSING"},
		{"非法单元名", fmt.Errorf("%w: ../etc", services.ErrInvalidUnitName), http.StatusBadRequest, "VALIDATION_ERROR"},
		{"内部错误", errors.New("解析输出失败"), http.StatusInternalServerError, "INTERNAL_ERROR"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv, cookie := servicesFixture(t, &stubServices{listErr: c.err, unitErr: c.err})
			rec := servicesGet(t, srv, cookie, "/api/v1/services")
			if rec.Code != c.code {
				t.Fatalf("列表状态码 = %d，期望 %d（%s）", rec.Code, c.code, rec.Body.String())
			}
			if got := parseResult(t, rec); got.Error == nil || got.Error.Code != c.want {
				t.Errorf("列表错误码 = %+v，期望 %s", got.Error, c.want)
			}
			rec = servicesGet(t, srv, cookie, "/api/v1/services/cron.service")
			if rec.Code != c.code {
				t.Fatalf("详情状态码 = %d，期望 %d（%s）", rec.Code, c.code, rec.Body.String())
			}
			if got := parseResult(t, rec); got.Error == nil || got.Error.Code != c.want {
				t.Errorf("详情错误码 = %+v，期望 %s", got.Error, c.want)
			}
		})
	}
}
