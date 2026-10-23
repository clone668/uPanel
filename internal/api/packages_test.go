package api

// P11.1 接口层用例：/packages、/packages/updates、/system/power-state。
//
// 关注的是 HTTP 层该负责的几件事，不重复 internal/packages 的判断测试：
//   - 权限：整组 admin，viewer 403、未登录 401；
//   - 演示模式（没有 Packages 读取端）：503 AGENT_UNAVAILABLE，**不是**空列表；
//   - 参数：limit 越界、installed=maybe 这类含混写法一律 400；
//   - 错误映射：游标坏 400、代理不可用 503、其余 500。

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
	"upanel/internal/httpx"
	"upanel/internal/packages"
	"upanel/internal/platform"
	"upanel/internal/version"
)

// stubPackages 是 PackagesReader 的测试替身。
type stubPackages struct {
	list      packages.List
	updates   packages.Updates
	power     packages.PowerStateView
	listErr   error
	updErr    error
	powerErr  error
	gotQuery  packages.ListQuery
	listCalls int
	// P11.3：任务详情里的 power_check 由读取端提供，替身这里直接给结论。
	check      func(action string) *packages.PowerCheckView
	checkCalls int
}

func (s *stubPackages) List(_ context.Context, q packages.ListQuery) (packages.List, error) {
	s.listCalls++
	s.gotQuery = q
	if s.listErr != nil {
		return packages.List{}, s.listErr
	}
	return s.list, nil
}

func (s *stubPackages) Updates(context.Context) (packages.Updates, error) {
	if s.updErr != nil {
		return packages.Updates{}, s.updErr
	}
	return s.updates, nil
}

func (s *stubPackages) PowerState(context.Context) (packages.PowerStateView, error) {
	if s.powerErr != nil {
		return packages.PowerStateView{}, s.powerErr
	}
	return s.power, nil
}

// PowerJobCheck 默认什么也不说（nil）。要测这一节时给 check 一个返回值。
func (s *stubPackages) PowerJobCheck(_ context.Context, action string, _ json.RawMessage) *packages.PowerCheckView {
	s.checkCalls++
	if s.check == nil {
		return nil
	}
	return s.check(action)
}

// packagesFixture 构造一个接了假读取端的生产模式服务器。
func packagesFixture(t *testing.T, reader PackagesReader) (*Server, *http.Cookie) {
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
		Packages:          reader,
	})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	seedTestUser(t, srv.opts.Auth, testUser, testUserPass)
	cookie, _ := loginOK(t, srv)
	return srv, cookie
}

// samplePackageList 是一份真机形状的响应（版本串都来自 VM-A）。
func samplePackageList() packages.List {
	at := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	idx := at.Add(-90 * time.Minute)
	cand := "5.1-6ubuntu1.1"
	next := "YmFzaABhbWQ2NA"
	return packages.List{
		CollectedAt:    at,
		IndexUpdatedAt: &idx,
		Counts:         packages.Counts{Installed: 676, Upgradable: 2, Security: 1},
		Filters:        packages.Filters{Installed: true},
		Matched:        676,
		Limit:          50,
		NextCursor:     &next,
		Packages: []packages.Entry{
			{
				Name: "bash", Architecture: "amd64", Status: "ii ", Installed: true,
				InstalledVersion: "5.1-6ubuntu1", CandidateVersion: &cand,
				CandidateRelation: packages.RelationNewer, Upgradable: true, Security: true,
				Suites:  []string{"jammy-security", "jammy-updates"},
				Pockets: []string{packages.PocketUpdates, packages.PocketSecurity},
			},
		},
	}
}

func TestListPackagesOK(t *testing.T) {
	list := samplePackageList()
	stub := &stubPackages{list: list}
	srv, cookie := packagesFixture(t, stub)

	rec := adminGet(t, srv, cookie, "/api/v1/packages?query=bash&upgradable=true&security=true")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d（%s）", rec.Code, rec.Body.String())
	}
	var body struct {
		Data packages.List `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if len(body.Data.Packages) != 1 || body.Data.Packages[0].Name != "bash" {
		t.Fatalf("packages = %+v", body.Data.Packages)
	}
	if body.Data.Packages[0].CandidateVersion == nil ||
		*body.Data.Packages[0].CandidateVersion != "5.1-6ubuntu1.1" {
		t.Errorf("候选版本没透传: %+v", body.Data.Packages[0])
	}
	// filters 与查询参数都要原样交给下层。
	q := stub.gotQuery
	if q.Query != "bash" || !q.Upgradable || !q.Security || !q.Installed {
		t.Errorf("下层收到的参数 = %+v（installed 缺省应为 true）", q)
	}
}

func TestListPackagesInstalledFalse(t *testing.T) {
	stub := &stubPackages{}
	srv, cookie := packagesFixture(t, stub)
	if rec := adminGet(t, srv, cookie, "/api/v1/packages?installed=false"); rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d（%s）", rec.Code, rec.Body.String())
	}
	if stub.gotQuery.Installed {
		t.Error("installed=false 应把过滤关掉（把 rc 的包也算进来）")
	}
}

func TestListPackagesBadParams(t *testing.T) {
	stub := &stubPackages{}
	srv, cookie := packagesFixture(t, stub)

	cases := []string{
		"/api/v1/packages?limit=201", // 超过上限
		"/api/v1/packages?limit=0",   // 不是正整数
		"/api/v1/packages?limit=abc", // 不是整数
		"/api/v1/packages?installed=maybe",
		"/api/v1/packages?upgradable=2",
		"/api/v1/packages?security=y",
	}
	for _, path := range cases {
		rec := adminGet(t, srv, cookie, path)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s 状态码 = %d，期望 400（%s）", path, rec.Code, rec.Body.String())
		}
	}
	if stub.listCalls != 0 {
		t.Errorf("参数被拒时不该调用下层（实际 %d 次）", stub.listCalls)
	}
}

func TestPackagesErrorMapping(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
		code httpx.Code
	}{
		{"坏游标", packages.ErrInvalidCursor, http.StatusBadRequest, httpx.CodeValidationError},
		{"坏筛选", errors.Join(packages.ErrInvalidQuery), http.StatusBadRequest, httpx.CodeValidationError},
		{"代理不可用", packages.ErrFactsUnavailable, http.StatusServiceUnavailable, httpx.CodeAgentUnavailable},
		{"其他", errors.New("boom"), http.StatusInternalServerError, httpx.CodeInternalError},
	}
	for _, tc := range cases {
		stub := &stubPackages{listErr: tc.err, updErr: tc.err, powerErr: tc.err}
		srv, cookie := packagesFixture(t, stub)
		for _, path := range []string{"/api/v1/packages", "/api/v1/packages/updates", "/api/v1/system/power-state"} {
			rec := adminGet(t, srv, cookie, path)
			if rec.Code != tc.want {
				t.Errorf("%s %s 状态码 = %d，期望 %d（%s）", tc.name, path, rec.Code, tc.want, rec.Body.String())
				continue
			}
			var body struct {
				Error struct {
					Code httpx.Code `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Errorf("%s %s 响应不是合法 JSON: %v", tc.name, path, err)
				continue
			}
			if body.Error.Code != tc.code {
				t.Errorf("%s %s code = %q，期望 %q", tc.name, path, body.Error.Code, tc.code)
			}
		}
	}
}

func TestPackagesUnavailableWithoutAgent(t *testing.T) {
	// 演示模式：没有 Packages 读取端。三个接口都必须是 503，而不是 200 + 空列表。
	srv, cookie := packagesFixture(t, nil)
	for _, path := range []string{"/api/v1/packages", "/api/v1/packages/updates", "/api/v1/system/power-state"} {
		rec := adminGet(t, srv, cookie, path)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s 状态码 = %d，期望 503（%s）", path, rec.Code, rec.Body.String())
		}
		if body := rec.Body.String(); !strings.Contains(body, string(httpx.CodeAgentUnavailable)) {
			t.Errorf("%s 错误码应是 AGENT_UNAVAILABLE：%s", path, body)
		}
	}
}

func TestPackagesRequiresAdmin(t *testing.T) {
	stub := &stubPackages{}
	srv, adminCookie := packagesFixture(t, stub)

	// 未登录：401。
	for _, path := range []string{"/api/v1/packages", "/api/v1/packages/updates", "/api/v1/system/power-state"} {
		if rec := adminGet(t, srv, nil, path); rec.Code != http.StatusUnauthorized {
			t.Errorf("未登录 %s 状态码 = %d，期望 401", path, rec.Code)
		}
	}

	// viewer：403。包版本属于主机内部信息，不给只读账号看。
	if _, err := srv.opts.Auth.CreateUser(context.Background(), auth.CreateUserInput{
		Username: "pkg-watcher", DisplayName: "pkg-watcher",
		Role: auth.RoleViewer, Password: "watcher-horse-9",
	}); err != nil {
		t.Fatalf("创建 viewer 失败: %v", err)
	}
	viewerCookie, _ := loginAsUser(t, srv, "pkg-watcher", "watcher-horse-9")
	for _, path := range []string{"/api/v1/packages", "/api/v1/packages/updates", "/api/v1/system/power-state"} {
		rec := adminGet(t, srv, viewerCookie, path)
		if rec.Code != http.StatusForbidden {
			t.Errorf("viewer %s 状态码 = %d，期望 403（%s）", path, rec.Code, rec.Body.String())
		}
	}

	// admin 仍然可以。（stub 返回零值，只看状态码。）
	if rec := adminGet(t, srv, adminCookie, "/api/v1/packages"); rec.Code != http.StatusOK {
		t.Errorf("admin 状态码 = %d（%s）", rec.Code, rec.Body.String())
	}
}

func TestPackageUpdatesAndPowerStateOK(t *testing.T) {
	at := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	stub := &stubPackages{
		updates: packages.Updates{
			CollectedAt: at, Total: 2, Security: 1,
			Updates: []packages.UpdateEntry{
				{Name: "bash", Architecture: "amd64", InstalledVersion: "5.1-6ubuntu1",
					CandidateVersion: "5.1-6ubuntu1.1", Security: true,
					Suites: []string{"jammy-security"}, Pockets: []string{packages.PocketSecurity}},
			},
		},
		power: packages.PowerStateView{
			CollectedAt: at, RebootRequired: true, RebootRequiredSince: &at,
			RebootRequiredPackages: []string{"linux-image-5.15.0-198-generic"},
			BootID:                 "ab2eb5e8-c1b4-4a00-a3d0-33ced7870abc",
		},
	}
	srv, cookie := packagesFixture(t, stub)

	rec := adminGet(t, srv, cookie, "/api/v1/packages/updates")
	if rec.Code != http.StatusOK {
		t.Fatalf("updates 状态码 = %d（%s）", rec.Code, rec.Body.String())
	}
	var upd struct {
		Data packages.Updates `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &upd); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if upd.Data.Total != 2 || len(upd.Data.Updates) != 1 || !upd.Data.Updates[0].Security {
		t.Fatalf("updates = %+v", upd.Data)
	}

	rec = adminGet(t, srv, cookie, "/api/v1/system/power-state")
	if rec.Code != http.StatusOK {
		t.Fatalf("power-state 状态码 = %d（%s）", rec.Code, rec.Body.String())
	}
	var ps struct {
		Data packages.PowerStateView `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &ps); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if !ps.Data.RebootRequired || ps.Data.BootID == "" || len(ps.Data.RebootRequiredPackages) != 1 {
		t.Fatalf("power-state = %+v", ps.Data)
	}
}

// TestPackagesNoWriteRoutes 确认这一组没有变更入口。
//
// P11.2 的安装/卸载/升级必须走预检 → 任务链路；这里若出现一条 POST /packages/…
// 就等于把那条链路上的保护名单、锁与审计全绕过去。所以这条断言用 404 固定下来。
func TestPackagesNoWriteRoutes(t *testing.T) {
	stub := &stubPackages{}
	srv, cookie := packagesFixture(t, stub)
	for _, path := range []string{"/api/v1/packages", "/api/v1/packages/bash"} {
		for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
			rec := doMethod(t, srv, cookie, method, path, "{}")
			if rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
				t.Errorf("%s %s 状态码 = %d，期望 404/405（%s）", method, path, rec.Code, rec.Body.String())
			}
		}
	}
}

// doMethod 发一个带 cookie 的请求（GET 不需要 CSRF，其余方法带上测试 CSRF 头）。
func doMethod(t *testing.T, srv *Server, cookie *http.Cookie, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Origin", testOrigin)
	if cookie != nil {
		req.Header.Set("Cookie", cookie.String())
	}
	if method != http.MethodGet && method != http.MethodHead {
		req.Header.Set(CSRFTokenHeader, "test-csrf")
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}
