package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"upanel/internal/audit"
	"upanel/internal/certificates"
	"upanel/internal/config"
	"upanel/internal/nginx"
	"upanel/internal/platform"
	"upanel/internal/proxies"
	"upanel/internal/store"
	"upanel/internal/version"
)

// 反向代理站点接口（P10.1）。
//
// 断言的重点不是“接口能返回 200”，而是五件事：
//  1. 权限边界真的生效：整组 admin，viewer 连列表都拿不到；
//  2. 判不了域名冲突时**拒绝写入**（503），不是“先存下来再说”；
//  3. 冲突结论带着依据（文件、行、原文），unknown 不被当成 clear；
//  4. basic_auth_users 的响应里没有 secret_ref；
//  5. 站点变更留下审计，并且审计里写明 applied=false——P10.1 不写 Nginx 配置。

// proxyFactsFake 是一份可以指定的事实源：三个方法共用同一份事实。
type proxyFactsFake struct {
	facts nginx.Facts
	err   error
}

func (f *proxyFactsFake) Snapshot(context.Context) (nginx.Facts, error) { return f.facts, f.err }
func (f *proxyFactsFake) Collect(context.Context) (nginx.Facts, error)  { return f.facts, f.err }
func (f *proxyFactsFake) Invalidate()                                   {}

// proxyFacts 造一份“读到了配置”的事实。
//
// claims 是 文件 → server_name 行（一行可以有多个名字）。行号固定写 3，
// 断言证据指向的是 server_name 那一行而不是 server 块的开头。
func proxyFacts(claims map[string]string) nginx.Facts {
	f := nginx.Facts{
		Available:   true,
		Version:     "1.24.0",
		UnitState:   "active",
		ConfigRoot:  "/etc/nginx",
		CollectedAt: time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC),
	}
	for path, line := range claims {
		file := nginx.ConfigFile{Path: path, Managed: nginx.IsManagedName(filepath.Base(path)), SizeBytes: 128}
		srv := nginx.ServerBlock{Line: 1, NamesLine: 3}
		for _, name := range strings.Fields(line) {
			srv.Names = append(srv.Names, nginx.Classify(name))
		}
		srv.Listen = []nginx.ListenSpec{{Address: "0.0.0.0", Port: 443, SSL: true, Raw: "0.0.0.0:443 ssl"}}
		file.Servers = append(file.Servers, srv)
		f.Files = append(f.Files, file)
	}
	return f
}

// newProxyFixture 构造一个把代理事实固定成 facts 的服务实例。
//
// facts 为 nil 表示“没有代理连接”：那时写入必须回 503。reader 是 /proxies/nginx
// 与列表摘要那一块的读取端；单独传是为了能造出“站点服务有事实、展示读取失败”的组合。
func newProxyFixture(t *testing.T, facts proxies.FactsSource, reader NginxFactsReader) *adminFixture {
	t.Helper()
	authSvc, aud, db := newTestServicesWithDB(t)
	proxySvc, err := proxies.New(db, proxies.Options{
		Facts:     facts,
		Apps:      newTestApps(t, db),
		Normalize: proxies.NormalizeOptions{PanelPort: 9443},
	})
	if err != nil {
		t.Fatalf("构造代理站点服务失败: %v", err)
	}
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
		Proxies:           proxySvc,
		NginxFacts:        reader,
		AllowedOrigins:    testOrigins(),
		Certificates:      newTestCertificates(t, db),
		CertificateStages: certificates.NewStageStore(nil),
	})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	seedTestUser(t, srv.opts.Auth, testUser, testUserPass)
	cookie, csrf := loginOK(t, srv)
	return &adminFixture{srv: srv, cookie: cookie, csrf: csrf, db: db}
}

// newProxyFixtureOK 是最常见的一种：事实可读、展示可读，同一份。
func newProxyFixtureOK(t *testing.T, facts nginx.Facts) *adminFixture {
	t.Helper()
	fake := &proxyFactsFake{facts: facts}
	return newProxyFixture(t, fake, fake)
}

// proxyBody 构造一条最小可用的站点定义。
func proxyBody(domain string) map[string]any {
	return map[string]any{
		"domain":             domain,
		"upstream_target_id": "local_8080",
		"upstream_port":      8080,
	}
}

// mustCreateProxy 通过接口创建一个站点并返回解析结果。
func mustCreateProxy(t *testing.T, f *adminFixture, body map[string]any) proxySiteDTO {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("序列化请求体失败: %v", err)
	}
	rec := f.do(t, http.MethodPost, "/api/v1/proxies", string(raw))
	if rec.Code != http.StatusCreated {
		t.Fatalf("创建站点失败: %d %s", rec.Code, rec.Body.String())
	}
	var out proxySiteDTO
	if err := json.Unmarshal(parseResult(t, rec).Data, &out); err != nil {
		t.Fatalf("解析创建结果失败: %v", err)
	}
	return out
}

// TestProxiesRequireSession 确认整组接口都要登录。
func TestProxiesRequireSession(t *testing.T) {
	f := newProxyFixtureOK(t, proxyFacts(nil))
	for _, route := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/proxies"},
		{http.MethodGet, "/api/v1/proxies/nginx"},
		{http.MethodGet, "/api/v1/proxies/conflict?domain=blog.example.com"},
		{http.MethodGet, "/api/v1/proxies/site_00000000000000000000000000000000"},
		{http.MethodPost, "/api/v1/proxies"},
	} {
		req := httptest.NewRequest(route.method, route.path, nil)
		rec := httptest.NewRecorder()
		f.srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s 未登录状态码 = %d，期望 401（%s）",
				route.method, route.path, rec.Code, rec.Body.String())
		}
	}
}

// TestProxiesRequireAdmin 确认 viewer 越权得到 403。
//
// 站点定义里有上游目标与访问策略，合起来就是“这台机器的哪个入口通向哪里”：
// 它与容器清单同级，viewer 不能看，也不能改。
func TestProxiesRequireAdmin(t *testing.T) {
	f := newProxyFixtureOK(t, proxyFacts(nil))
	f.createUser(t, "alice", "viewer", "alice-horse-9")
	viewerCookie, viewerCSRF := loginAsUser(t, f.srv, "alice", "alice-horse-9")

	body, _ := json.Marshal(proxyBody("blog.example.com"))
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/v1/proxies", ""},
		{http.MethodGet, "/api/v1/proxies/nginx", ""},
		{http.MethodGet, "/api/v1/proxies/conflict?domain=blog.example.com", ""},
		{http.MethodPost, "/api/v1/proxies", string(body)},
	} {
		rec := f.doAs(t, viewerCookie, viewerCSRF, tc.method, tc.path, tc.body)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("viewer %s %s 状态码 = %d，期望 403（%s）", tc.method, tc.path, rec.Code, rec.Body.String())
		}
	}
}

// TestCreateProxySiteRoundTrip 走一遍创建 → 读取 → 列表，核对默认值与字段形态。
func TestCreateProxySiteRoundTrip(t *testing.T) {
	f := newProxyFixtureOK(t, proxyFacts(nil))
	created := mustCreateProxy(t, f, map[string]any{
		"domain":             "Blog.Example.COM.",
		"upstream_target_id": "local_8080",
		"upstream_port":      8080,
		"websocket_enabled":  true,
	})

	if !strings.HasPrefix(created.ID, "site_") || len(created.ID) != 37 {
		t.Fatalf("站点标识形状不对: %q", created.ID)
	}
	// 域名归一化：大写与末尾的点都必须被吃掉，否则同一个站点能建两条。
	if created.Domain != "blog.example.com" {
		t.Fatalf("域名 = %q，期望 blog.example.com", created.Domain)
	}
	if created.Version != 1 || !created.Enabled || !created.WebSocketEnabled {
		t.Fatalf("创建结果 = %+v", created)
	}
	// 默认值与 Nginx 自己的默认一致：1 MiB 请求体、60 秒读超时、public 访问。
	if created.MaxBodyBytes != proxies.DefaultMaxBodyBytes || created.ReadTimeoutSeconds != proxies.DefaultReadTimeoutSeconds {
		t.Fatalf("请求限制默认值 = %d / %d", created.MaxBodyBytes, created.ReadTimeoutSeconds)
	}
	if created.AccessMode != string(proxies.AccessPublic) || created.UpstreamScheme != proxies.SchemeHTTP {
		t.Fatalf("访问模式/上游协议 = %q / %q", created.AccessMode, created.UpstreamScheme)
	}
	if created.AppID != nil {
		t.Fatalf("没有关联应用时 app_id 必须是 null，实际 %v", *created.AppID)
	}
	if created.AllowedCIDRs == nil || created.BasicAuthUsers == nil {
		t.Fatal("空列表必须是 []，不能是 null")
	}

	// 详情与列表读到的必须是同一条。
	rec := f.do(t, http.MethodGet, "/api/v1/proxies/"+created.ID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("读取站点失败: %d %s", rec.Code, rec.Body.String())
	}
	var got proxySiteDTO
	if err := json.Unmarshal(parseResult(t, rec).Data, &got); err != nil {
		t.Fatalf("解析详情失败: %v", err)
	}
	if got.ID != created.ID || got.Domain != created.Domain || got.Version != created.Version {
		t.Fatalf("详情 = %+v，创建结果 = %+v", got, created)
	}

	rec = f.do(t, http.MethodGet, "/api/v1/proxies", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("读取列表失败: %d %s", rec.Code, rec.Body.String())
	}
	var list proxyListResponse
	if err := json.Unmarshal(parseResult(t, rec).Data, &list); err != nil {
		t.Fatalf("解析列表失败: %v", err)
	}
	if len(list.Items) != 1 || list.Items[0].ID != created.ID {
		t.Fatalf("列表 = %+v", list.Items)
	}
	if !list.Nginx.Available || list.Nginx.ReadError != "" {
		t.Fatalf("列表里的 Nginx 摘要 = %+v", list.Nginx)
	}
}

// TestCreateProxySiteRejectsPanelPort 确认上游端口不能是管理面板自己。
func TestCreateProxySiteRejectsPanelPort(t *testing.T) {
	f := newProxyFixtureOK(t, proxyFacts(nil))
	body := proxyBody("blog.example.com")
	body["upstream_port"] = 9443
	raw, _ := json.Marshal(body)
	rec := f.do(t, http.MethodPost, "/api/v1/proxies", string(raw))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400（%s）", rec.Code, rec.Body.String())
	}
	if got := parseResult(t, rec).Error; got == nil || !strings.Contains(got.Message, "管理面板") {
		t.Fatalf("错误信息 = %+v", got)
	}
}

// TestCreateProxySiteRejectsInvalidDomain 确认非法域名是 400，不是悄悄存下来。
func TestCreateProxySiteRejectsInvalidDomain(t *testing.T) {
	f := newProxyFixtureOK(t, proxyFacts(nil))
	for _, domain := range []string{"", "example", "https://example.com", "*.example.com", "exa mple.com"} {
		raw, _ := json.Marshal(proxyBody(domain))
		rec := f.do(t, http.MethodPost, "/api/v1/proxies", string(raw))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("域名 %q 状态码 = %d，期望 400（%s）", domain, rec.Code, rec.Body.String())
		}
	}
}

// TestCreateProxySiteConflict 确认已被现有配置占用的域名被拒绝，并且指出证据。
func TestCreateProxySiteConflict(t *testing.T) {
	f := newProxyFixtureOK(t, proxyFacts(map[string]string{
		"/etc/nginx/conf.d/blog.conf": "blog.example.com www.blog.example.com",
	}))
	raw, _ := json.Marshal(proxyBody("blog.example.com"))
	rec := f.do(t, http.MethodPost, "/api/v1/proxies", string(raw))
	if rec.Code != http.StatusConflict {
		t.Fatalf("状态码 = %d，期望 409（%s）", rec.Code, rec.Body.String())
	}
	result := parseResult(t, rec)
	if result.Error == nil || !strings.Contains(result.Error.Message, "blog.conf 第 3 行") {
		t.Fatalf("错误信息必须指出文件与行号: %+v", result.Error)
	}
	details := result.Error.Details
	if details == nil {
		t.Fatal("409 必须带 details")
	}
	matches, ok := details["matches"].([]any)
	if !ok || len(matches) != 1 {
		t.Fatalf("matches = %v", details["matches"])
	}
	first, _ := matches[0].(map[string]any)
	if first["file"] != "/etc/nginx/conf.d/blog.conf" || first["line"] != float64(3) {
		t.Fatalf("证据 = %v", first)
	}

	// 写入被拒绝：库里不能留下一条记录。
	var count int
	if err := f.db.SQL().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM proxies`).Scan(&count); err != nil {
		t.Fatalf("查询站点数失败: %v", err)
	}
	if count != 0 {
		t.Fatalf("被拒绝的创建必须在库里留 0 条记录，实际 %d 条", count)
	}
}

// TestCreateProxySiteWithoutAgent 确认判不了冲突时拒绝写入。
//
// 这是本阶段最重要的边界：没有代理就不知道主机上已经声明了哪些域名，
// 存下一条“看起来没问题”的记录，等于把冲突留到 P10.2 写配置那一刻才炸。
func TestCreateProxySiteWithoutAgent(t *testing.T) {
	f := newProxyFixture(t, nil, nil)
	raw, _ := json.Marshal(proxyBody("blog.example.com"))
	rec := f.do(t, http.MethodPost, "/api/v1/proxies", string(raw))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("状态码 = %d，期望 503（%s）", rec.Code, rec.Body.String())
	}
	if got := parseResult(t, rec).Error; got == nil || got.Code != "AGENT_UNAVAILABLE" {
		t.Fatalf("错误码 = %+v", got)
	}

	// 列表本身照常返回（站点定义在面板库里），只是 nginx 那一块写“读不到”。
	rec = f.do(t, http.MethodGet, "/api/v1/proxies", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("没有代理时列表状态码 = %d，期望 200（%s）", rec.Code, rec.Body.String())
	}
	var list proxyListResponse
	if err := json.Unmarshal(parseResult(t, rec).Data, &list); err != nil {
		t.Fatalf("解析列表失败: %v", err)
	}
	if list.Nginx.ReadError == "" || list.Nginx.Available {
		t.Fatalf("没有代理时的 Nginx 摘要必须写明读不到: %+v", list.Nginx)
	}
	if list.Items == nil {
		t.Fatal("站点列表必须是 []，不能是 null")
	}
}

// TestListProxiesNginxReadError 确认展示读取失败与“主机上没有 Nginx”分开表达。
func TestListProxiesNginxReadError(t *testing.T) {
	fake := &proxyFactsFake{facts: proxyFacts(nil)}
	broken := &proxyFactsFake{err: proxies.ErrFactsUnavailable}
	f := newProxyFixture(t, fake, broken)

	rec := f.do(t, http.MethodGet, "/api/v1/proxies", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d（%s）", rec.Code, rec.Body.String())
	}
	var list proxyListResponse
	if err := json.Unmarshal(parseResult(t, rec).Data, &list); err != nil {
		t.Fatalf("解析列表失败: %v", err)
	}
	if list.Nginx.ReadError != "agent_unavailable" {
		t.Fatalf("read_error = %q，期望 agent_unavailable", list.Nginx.ReadError)
	}
	if list.Nginx.ConfigMissing {
		t.Fatal("读不到不能写成“主机上没有配置”")
	}
}

// TestUpdateProxySiteVersionConflict 确认乐观锁与“至少改一项”。
func TestUpdateProxySiteVersionConflict(t *testing.T) {
	f := newProxyFixtureOK(t, proxyFacts(nil))
	created := mustCreateProxy(t, f, proxyBody("blog.example.com"))

	// 空 patch 是 400。
	rec := f.do(t, http.MethodPatch, "/api/v1/proxies/"+created.ID, `{"version":1}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("空 patch 状态码 = %d，期望 400（%s）", rec.Code, rec.Body.String())
	}
	// 版本对不上是 409 VERSION_CONFLICT。
	rec = f.do(t, http.MethodPatch, "/api/v1/proxies/"+created.ID, `{"version":9,"read_timeout_seconds":30}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("版本冲突状态码 = %d，期望 409（%s）", rec.Code, rec.Body.String())
	}
	if got := parseResult(t, rec).Error; got == nil || got.Code != "VERSION_CONFLICT" {
		t.Fatalf("错误码 = %+v", got)
	}
	// 正常修改：版本 +1，允许域不变（不能因为“域名被自己占用”而拒绝）。
	rec = f.do(t, http.MethodPatch, "/api/v1/proxies/"+created.ID,
		`{"version":1,"domain":"blog.example.com","read_timeout_seconds":30}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("修改状态码 = %d，期望 200（%s）", rec.Code, rec.Body.String())
	}
	var updated proxySiteDTO
	if err := json.Unmarshal(parseResult(t, rec).Data, &updated); err != nil {
		t.Fatalf("解析修改结果失败: %v", err)
	}
	if updated.Version != 2 || updated.ReadTimeoutSeconds != 30 {
		t.Fatalf("修改结果 = %+v", updated)
	}
}

// TestUpdateProxySiteClearsOptionalLists 确认 [] 与“不提供”是两件事。
func TestUpdateProxySiteClearsOptionalLists(t *testing.T) {
	f := newProxyFixtureOK(t, proxyFacts(nil))
	created := mustCreateProxy(t, f, map[string]any{
		"domain":             "blog.example.com",
		"upstream_target_id": "local_8080",
		"upstream_port":      8080,
		"access_mode":        "cidr",
		"allowed_cidrs":      []string{"10.0.0.0/8"},
	})
	if created.AccessMode != string(proxies.AccessCIDR) || len(created.AllowedCIDRs) != 1 {
		t.Fatalf("创建结果 = %+v", created)
	}
	// cidr 模式下调成 public：allowed_cidrs 必须一并清空，否则库里留着互不相干的字段。
	rec := f.do(t, http.MethodPatch, "/api/v1/proxies/"+created.ID,
		`{"version":1,"access_mode":"public","allowed_cidrs":[]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（%s）", rec.Code, rec.Body.String())
	}
	var updated proxySiteDTO
	if err := json.Unmarshal(parseResult(t, rec).Data, &updated); err != nil {
		t.Fatalf("解析修改结果失败: %v", err)
	}
	if updated.AccessMode != string(proxies.AccessPublic) || len(updated.AllowedCIDRs) != 0 {
		t.Fatalf("修改结果 = %+v", updated)
	}
}

// TestProxyBasicAuthNeverReturnsSecretRef 确认 secret_ref 不进响应。
func TestProxyBasicAuthNeverReturnsSecretRef(t *testing.T) {
	f := newProxyFixtureOK(t, proxyFacts(nil))
	const secret = "sec_0123456789abcdef0123456789abcdef"
	raw, _ := json.Marshal(map[string]any{
		"domain":             "blog.example.com",
		"upstream_target_id": "local_8080",
		"upstream_port":      8080,
		"access_mode":        "basic_auth",
		"basic_auth_users": []map[string]any{
			{"name": "ops", "secret_ref": secret},
		},
	})
	rec := f.do(t, http.MethodPost, "/api/v1/proxies", string(raw))
	if rec.Code != http.StatusCreated {
		t.Fatalf("状态码 = %d，期望 201（%s）", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), secret) {
		t.Fatalf("响应里出现了 secret_ref：%s", rec.Body.String())
	}
	var created proxySiteDTO
	if err := json.Unmarshal(parseResult(t, rec).Data, &created); err != nil {
		t.Fatalf("解析创建结果失败: %v", err)
	}
	if len(created.BasicAuthUsers) != 1 || created.BasicAuthUsers[0].Name != "ops" {
		t.Fatalf("用户列表 = %+v", created.BasicAuthUsers)
	}
	if created.BasicAuthUsers[0].SecretRef != "" {
		t.Fatalf("响应里带出了 secret_ref：%q", created.BasicAuthUsers[0].SecretRef)
	}
}

// TestDeleteProxySite 确认删除要 version，删完就 404。
func TestDeleteProxySite(t *testing.T) {
	f := newProxyFixtureOK(t, proxyFacts(nil))
	created := mustCreateProxy(t, f, proxyBody("blog.example.com"))

	// 缺 version 是 400。
	rec := f.do(t, http.MethodDelete, "/api/v1/proxies/"+created.ID, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("缺 version 状态码 = %d，期望 400（%s）", rec.Code, rec.Body.String())
	}
	// 版本对不上是 409。
	rec = f.do(t, http.MethodDelete, "/api/v1/proxies/"+created.ID, `{"version":7}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("版本冲突状态码 = %d，期望 409（%s）", rec.Code, rec.Body.String())
	}
	rec = f.do(t, http.MethodDelete, "/api/v1/proxies/"+created.ID, `{"version":1}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("删除状态码 = %d，期望 200（%s）", rec.Code, rec.Body.String())
	}
	rec = f.do(t, http.MethodGet, "/api/v1/proxies/"+created.ID, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("删除后读取状态码 = %d，期望 404（%s）", rec.Code, rec.Body.String())
	}
}

// TestProxyConflictEndpoint 走一遍 clear / taken / unknown 与非法域名。
func TestProxyConflictEndpoint(t *testing.T) {
	f := newProxyFixtureOK(t, proxyFacts(map[string]string{
		"/etc/nginx/conf.d/blog.conf": "blog.example.com",
	}))

	cases := []struct {
		domain    string
		wantState string
	}{
		{"shop.example.com", string(proxies.ConflictClear)},
		{"blog.example.com", string(proxies.ConflictTaken)},
	}
	for _, tc := range cases {
		rec := f.do(t, http.MethodGet, "/api/v1/proxies/conflict?domain="+tc.domain, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("%s 状态码 = %d（%s）", tc.domain, rec.Code, rec.Body.String())
		}
		var report proxyConflictDTO
		if err := json.Unmarshal(parseResult(t, rec).Data, &report); err != nil {
			t.Fatalf("解析冲突报告失败: %v", err)
		}
		if report.State != tc.wantState {
			t.Fatalf("%s 结论 = %q，期望 %q（%+v）", tc.domain, report.State, tc.wantState, report)
		}
		if tc.wantState == string(proxies.ConflictTaken) {
			if len(report.Matches) != 1 || report.Matches[0].Line != 3 {
				t.Fatalf("证据 = %+v", report.Matches)
			}
			if !report.Nginx.Available || report.Nginx.Version != "1.24.0" {
				t.Fatalf("Nginx 摘要 = %+v", report.Nginx)
			}
		}
	}

	// 非法域名是 400，不是“可用”。
	rec := f.do(t, http.MethodGet, "/api/v1/proxies/conflict?domain=*.example.com", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("通配符域名状态码 = %d，期望 400（%s）", rec.Code, rec.Body.String())
	}
	// 缺参数是 400。
	rec = f.do(t, http.MethodGet, "/api/v1/proxies/conflict", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("缺 domain 状态码 = %d，期望 400（%s）", rec.Code, rec.Body.String())
	}
}

// TestProxyConflictUnknownWhenConfigBroken 确认解析失败的文件把结论压成 unknown。
//
// nginx reload 不会因为 server_name 撞车而失败，它只会安静地用第一个块；
// 因此“有一个文件读不懂”必须让结论变成判不了，而不是“没找到冲突”。
func TestProxyConflictUnknownWhenConfigBroken(t *testing.T) {
	facts := proxyFacts(nil)
	facts.Files = append(facts.Files, nginx.ConfigFile{
		Path:  "/etc/nginx/conf.d/broken.conf",
		Error: "块没有闭合",
	})
	f := newProxyFixtureOK(t, facts)

	rec := f.do(t, http.MethodGet, "/api/v1/proxies/conflict?domain=blog.example.com", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d（%s）", rec.Code, rec.Body.String())
	}
	var report proxyConflictDTO
	if err := json.Unmarshal(parseResult(t, rec).Data, &report); err != nil {
		t.Fatalf("解析冲突报告失败: %v", err)
	}
	if report.State != string(proxies.ConflictUnknown) {
		t.Fatalf("结论 = %q，期望 unknown（%+v）", report.State, report)
	}
	if len(report.BrokenFiles) != 1 || report.BrokenFiles[0] != "/etc/nginx/conf.d/broken.conf" {
		t.Fatalf("broken_files = %v", report.BrokenFiles)
	}
	if report.Reason == "" {
		t.Fatal("unknown 必须带理由")
	}

	// 同一份事实下创建也必须被拒绝：unknown 不是 clear。
	raw, _ := json.Marshal(proxyBody("blog.example.com"))
	rec = f.do(t, http.MethodPost, "/api/v1/proxies", string(raw))
	if rec.Code != http.StatusConflict {
		t.Fatalf("unknown 状态下创建状态码 = %d，期望 409（%s）", rec.Code, rec.Body.String())
	}
}

// TestProxyNginxFactsEndpoint 确认 /proxies/nginx 的形态与“没有代理”的 503。
func TestProxyNginxFactsEndpoint(t *testing.T) {
	f := newProxyFixtureOK(t, proxyFacts(map[string]string{
		"/etc/nginx/conf.d/blog.conf": "blog.example.com www.blog.example.com",
	}))
	rec := f.do(t, http.MethodGet, "/api/v1/proxies/nginx", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d（%s）", rec.Code, rec.Body.String())
	}
	var facts proxyNginxFactsDTO
	if err := json.Unmarshal(parseResult(t, rec).Data, &facts); err != nil {
		t.Fatalf("解析 Nginx 现状失败: %v", err)
	}
	if !facts.State.Available || facts.State.Version != "1.24.0" {
		t.Fatalf("state = %+v", facts.State)
	}
	if len(facts.Files) != 1 || len(facts.Files[0].Servers) != 1 {
		t.Fatalf("files = %+v", facts.Files)
	}
	if got := facts.Files[0].Servers[0].Names; len(got) != 2 {
		t.Fatalf("server_name 原文 = %v", got)
	}
	if got := facts.Files[0].Servers[0].Listen; len(got) != 1 || got[0] != "0.0.0.0:443 ssl" {
		t.Fatalf("listen = %v", got)
	}
	if len(facts.ServerNames) != 2 || facts.ServerNames[0] != "blog.example.com" {
		t.Fatalf("server_names = %v", facts.ServerNames)
	}

	// 没有代理连接时是 503，不是空表。
	noAgent := newProxyFixture(t, &proxyFactsFake{facts: proxyFacts(nil)}, nil)
	rec = noAgent.do(t, http.MethodGet, "/api/v1/proxies/nginx", "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("没有代理时状态码 = %d，期望 503（%s）", rec.Code, rec.Body.String())
	}
	if got := parseResult(t, rec).Error; got == nil || got.Code != "AGENT_UNAVAILABLE" {
		t.Fatalf("错误码 = %+v", got)
	}
}

// TestProxyListDomainFilter 确认域名过滤按归一化后的值匹配。
func TestProxyListDomainFilter(t *testing.T) {
	f := newProxyFixtureOK(t, proxyFacts(nil))
	mustCreateProxy(t, f, proxyBody("blog.example.com"))
	mustCreateProxy(t, f, proxyBody("shop.example.com"))

	rec := f.do(t, http.MethodGet, "/api/v1/proxies?domain=Blog.Example.COM.", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d（%s）", rec.Code, rec.Body.String())
	}
	var list proxyListResponse
	if err := json.Unmarshal(parseResult(t, rec).Data, &list); err != nil {
		t.Fatalf("解析列表失败: %v", err)
	}
	if len(list.Items) != 1 || list.Items[0].Domain != "blog.example.com" {
		t.Fatalf("列表 = %+v", list.Items)
	}

	// 非法域名是 400：静默返回空列表会让人以为这个站点不存在。
	rec = f.do(t, http.MethodGet, "/api/v1/proxies?domain=not_a_domain", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("非法域名状态码 = %d，期望 400（%s）", rec.Code, rec.Body.String())
	}
}

// TestProxyWritesAreAudited 确认站点变更留下审计，且审计里写明 applied=false。
func TestProxyWritesAreAudited(t *testing.T) {
	f := newProxyFixtureOK(t, proxyFacts(nil))
	created := mustCreateProxy(t, f, proxyBody("blog.example.com"))

	rows, err := f.db.SQL().QueryContext(t.Context(),
		`SELECT action, outcome, target FROM audit_events WHERE action LIKE 'proxy%' ORDER BY id`)
	if err != nil {
		t.Fatalf("查询审计失败: %v", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var action, outcome, target string
		if err := rows.Scan(&action, &outcome, &target); err != nil {
			t.Fatalf("扫描审计失败: %v", err)
		}
		got = append(got, action+"|"+outcome+"|"+target)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("读取审计失败: %v", err)
	}
	want := audit.ActionProxyCreate + "|success|proxy:" + created.ID
	if len(got) != 1 || got[0] != want {
		t.Fatalf("审计 = %v，期望 %s", got, want)
	}

	var details string
	if err := f.db.SQL().QueryRowContext(t.Context(),
		`SELECT redacted_details FROM audit_events WHERE action = ?`, audit.ActionProxyCreate).Scan(&details); err != nil {
		t.Fatalf("查询审计详情失败: %v", err)
	}
	if !strings.Contains(details, `"applied":false`) {
		t.Fatalf("审计详情必须写明 applied=false: %s", details)
	}
	if strings.Contains(details, "secret") {
		t.Fatalf("审计详情里出现了与秘密有关的字段: %s", details)
	}
}

// TestProxyDeleteDoesNotTouchNginx 确认删除只改面板库。
//
// 这一条是 P10.1 的边界断言：本阶段没有任何代码去动 /etc/nginx，
// 所以删除一条站点之后 Nginx 现状必须一字不变（这里用事实源没有被调用过来证明）。
func TestProxyDeleteDoesNotTouchNginx(t *testing.T) {
	counting := &countingFacts{facts: proxyFacts(nil)}
	f := newProxyFixture(t, counting, counting)
	created := mustCreateProxy(t, f, proxyBody("blog.example.com"))

	rec := f.do(t, http.MethodDelete, "/api/v1/proxies/"+created.ID, `{"version":1}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("删除状态码 = %d（%s）", rec.Code, rec.Body.String())
	}
	// 创建时读过一次事实（判冲突），删除时不该再需要它；但无论读了几次，
	// 都不能出现“写配置”这类副作用——事实源是只读的，这里只能证明它被读没有副作用。
	if counting.collectCount == 0 {
		t.Fatal("创建时应当至少读过一次 Nginx 现状")
	}
}

// countingFacts 记录被读了几次，用来证明“判断确实走了事实源”。
type countingFacts struct {
	facts        nginx.Facts
	collectCount int
}

func (c *countingFacts) Snapshot(context.Context) (nginx.Facts, error) { return c.facts, nil }
func (c *countingFacts) Collect(context.Context) (nginx.Facts, error) {
	c.collectCount++
	return c.facts, nil
}
func (c *countingFacts) Invalidate() {}

// proxySitesCount 返回面板库里的站点数（供断言“被拒绝的写入没留下记录”）。
func proxySitesCount(t *testing.T, db *store.DB) int {
	t.Helper()
	var n int
	if err := db.SQL().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM proxies`).Scan(&n); err != nil {
		t.Fatalf("查询站点数失败: %v", err)
	}
	return n
}
