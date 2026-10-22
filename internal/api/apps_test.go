package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"upanel/internal/apps"
	"upanel/internal/audit"
	"upanel/internal/config"
	"upanel/internal/httpx"
	"upanel/internal/platform"
	"upanel/internal/store"
	"upanel/internal/version"
)

// 应用与分组接口（P09.1）。
//
// 断言的重点不是“接口能返回 200”，而是四件事：
//  1. 权限与 CSRF 真的生效（viewer 只读、收藏属于本人、写操作必须带 CSRF）；
//  2. 摘要字段是诚实的“不知道”，不是伪造的“运行中”；
//  3. 判别式绑定在接口层就被挡住错位（类型说 docker、绑定给单元名）；
//  4. 元数据变更留下审计，收藏不留。

const (
	// apiAppContainerID 是 64 位十六进制的容器 ID（绑定要求完整 ID）。
	apiAppContainerID = "3f4a9b2c1d5e6f708192a3b4c5d6e7f809a1b2c3d4e5f60718293a4b5c6d7e8f"
	// apiAppVolumeID / apiAppOtherVolumeID 是形状合法的卷标识（vol_ + 32 位十六进制）。
	apiAppVolumeID      = "vol_0123456789abcdef0123456789abcdef"
	apiAppOtherVolumeID = "vol_fedcba9876543210fedcba9876543210"
)

// newAppsFixture 与 newAdminFixture 相同，但应用服务由 build 用同一个库构造。
//
// 需要它是因为“代理不可达 / 卷不存在 / 卷存在”三种结局取决于注入的卷检查器，
// 而 newAdminFixture 固定用不带检查器的实例；服务必须和会话共用一个数据库，
// 否则卷检查器看到的是另一个库。
func newAppsFixture(t *testing.T, build func(*store.DB) *apps.Service) *adminFixture {
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
		Apps:            build(db),
		AllowedOrigins:  testOrigins(),
	})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	seedTestUser(t, srv.opts.Auth, testUser, testUserPass)
	cookie, csrf := loginOK(t, srv)
	return &adminFixture{srv: srv, cookie: cookie, csrf: csrf, db: db}
}

// createAppBody 构造最小可用的创建请求体。
func createAppBody(name, typ string) map[string]any {
	return map[string]any{"name": name, "type": typ, "public_url": "https://app.example.com"}
}

// mustCreateApp 通过接口创建应用并返回解析结果。
func mustCreateApp(t *testing.T, f *adminFixture, body map[string]any) appDTO {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("序列化请求体失败: %v", err)
	}
	rec := f.do(t, http.MethodPost, "/api/v1/apps", string(raw))
	if rec.Code != http.StatusCreated {
		t.Fatalf("创建应用失败: %d %s", rec.Code, rec.Body.String())
	}
	var out appDTO
	if err := json.Unmarshal(parseResult(t, rec).Data, &out); err != nil {
		t.Fatalf("解析创建结果失败: %v", err)
	}
	return out
}

// appBody 是响应的原始形态，用来检查 JSON 里的 null 与 []。
func appBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var envelope struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("响应不是合法 JSON: %v（%s）", err, rec.Body.String())
	}
	return envelope.Data
}

// TestAppsRequireSession 确认整组接口都要登录。
func TestAppsRequireSession(t *testing.T) {
	f := newAdminFixture(t)
	for _, route := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/apps"},
		{http.MethodGet, "/api/v1/apps/app_00000000000000000000000000000000"},
		{http.MethodGet, "/api/v1/app-groups"},
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

// TestCreateAppFourTypes 确认四种判别的响应形态。
func TestCreateAppFourTypes(t *testing.T) {
	f := newAdminFixture(t)

	// link：有入口地址、无绑定、无生命周期能力。
	link := mustCreateApp(t, f, map[string]any{
		"name": "面板入口", "type": "link", "public_url": "https://panel.example.com",
		"icon_key": "globe",
	})
	if !strings.HasPrefix(link.ID, "app_") || len(link.ID) != 36 {
		t.Fatalf("应用标识形状不对: %q", link.ID)
	}
	if link.Version != 1 || link.Type != "link" {
		t.Fatalf("创建结果 = %+v", link)
	}
	if link.GroupID != nil || link.PublicURL == nil || *link.PublicURL != "https://panel.example.com" {
		t.Fatalf("link 的分组/入口 = %v / %v", link.GroupID, link.PublicURL)
	}
	if link.RuntimeState != "not_applicable" || link.HTTPState != "not_applicable" {
		t.Fatalf("link 的状态枚举 = %q / %q", link.RuntimeState, link.HTTPState)
	}
	if link.LastCheckedAt != nil {
		t.Fatalf("没有探测器时 last_checked_at 必须是 null，实际 %v", *link.LastCheckedAt)
	}
	if !link.Capabilities.Open || link.Capabilities.Start || link.Capabilities.Stop ||
		link.Capabilities.Restart || link.Capabilities.Logs {
		t.Fatalf("link 的能力 = %+v", link.Capabilities)
	}

	// systemd：有绑定、无入口地址，运行时与依赖保护都是“不知道/未接管”。
	systemd := mustCreateApp(t, f, map[string]any{
		"name": "反向代理", "type": "systemd", "binding": map[string]any{"unit": "nginx.service"},
	})
	if systemd.Binding.Unit != "nginx.service" || systemd.Binding.ContainerID != "" {
		t.Fatalf("systemd 绑定 = %+v", systemd.Binding)
	}
	if systemd.PublicURL != nil {
		t.Fatalf("没填入口地址时 public_url 必须是 null，实际 %q", *systemd.PublicURL)
	}
	if systemd.RuntimeState != "unknown" || systemd.HTTPState != "not_applicable" {
		t.Fatalf("systemd 的状态枚举 = %q / %q", systemd.RuntimeState, systemd.HTTPState)
	}
	if systemd.DependencyProtection != "not_applicable" {
		t.Fatalf("无依赖卷时依赖保护 = %q", systemd.DependencyProtection)
	}
	if systemd.Capabilities.Open || systemd.Capabilities.Start || systemd.Capabilities.Logs {
		t.Fatalf("P09.1 不该提供启停/日志能力: %+v", systemd.Capabilities)
	}

	// docker：容器 ID 规范化为小写。
	docker := mustCreateApp(t, f, map[string]any{
		"name": "网盘", "type": "docker",
		"binding": map[string]any{"container_id": strings.ToUpper(apiAppContainerID)},
	})
	if docker.Binding.ContainerID != apiAppContainerID {
		t.Fatalf("容器 ID 未规范化为小写: %q", docker.Binding.ContainerID)
	}

	// compose：部署标识。
	compose := mustCreateApp(t, f, map[string]any{
		"name": "监控栈", "type": "compose",
		"binding": map[string]any{"deployment_id": "dep_0123456789abcdef0123456789abcdef"},
	})
	if compose.Binding.DeploymentID != "dep_0123456789abcdef0123456789abcdef" {
		t.Fatalf("compose 绑定 = %+v", compose.Binding)
	}

	// 列表：四条都在，且依赖卷列表是 [] 而不是 null（前端按数组渲染）。
	rec := f.do(t, http.MethodGet, "/api/v1/apps", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("列表状态码 = %d（%s）", rec.Code, rec.Body.String())
	}
	var list appListResponse
	if err := json.Unmarshal(parseResult(t, rec).Data, &list); err != nil {
		t.Fatalf("解析列表失败: %v", err)
	}
	if len(list.Items) != 4 {
		t.Fatalf("列表条数 = %d，期望 4", len(list.Items))
	}
	if list.NextCursor != nil {
		t.Fatalf("一页装得下时 next_cursor 必须是 null，实际 %q", *list.NextCursor)
	}
	raw := appBody(t, rec)["items"].([]any)[0].(map[string]any)
	if _, ok := raw["volume_dependencies"].([]any); !ok {
		t.Fatalf("volume_dependencies 不是 JSON 数组: %T", raw["volume_dependencies"])
	}
	if raw["group_id"] != nil {
		t.Fatalf("未分组应用的 group_id 应为 null，实际 %v", raw["group_id"])
	}
}

// TestBindingMustMatchType 确认“类型说 docker、绑定给单元名”在接口层就被挡。
func TestBindingMustMatchType(t *testing.T) {
	f := newAdminFixture(t)
	cases := []struct {
		name string
		body map[string]any
		want string
	}{
		{
			name: "systemd 给了 container_id",
			body: map[string]any{"name": "x", "type": "systemd",
				"binding": map[string]any{"unit": "a.service", "container_id": apiAppContainerID}},
			want: "container_id",
		},
		{
			name: "docker 少了 container_id",
			body: map[string]any{"name": "x", "type": "docker", "binding": map[string]any{}},
			want: "container_id",
		},
		{
			name: "docker 用了容器名",
			body: map[string]any{"name": "x", "type": "docker",
				"binding": map[string]any{"container_id": "my-thing"}},
			want: "64",
		},
		{
			name: "link 带了绑定",
			body: map[string]any{"name": "x", "type": "link",
				"public_url": "https://a.example.com",
				"binding":    map[string]any{"unit": "a.service"}},
			want: "link",
		},
		{
			name: "link 没有入口地址",
			body: map[string]any{"name": "x", "type": "link"},
			want: "入口地址",
		},
		{
			name: "未知类型",
			body: map[string]any{"name": "x", "type": "kubernetes"},
			want: "类型",
		},
	}
	for _, tc := range cases {
		raw, _ := json.Marshal(tc.body)
		rec := f.do(t, http.MethodPost, "/api/v1/apps", string(raw))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: 状态码 = %d，期望 400（%s）", tc.name, rec.Code, rec.Body.String())
		}
		result := parseResult(t, rec)
		if result.Error == nil || result.Error.Code != string(httpx.CodeValidationError) {
			t.Fatalf("%s: 错误 = %+v", tc.name, result.Error)
		}
		if !strings.Contains(result.Error.Message, tc.want) {
			t.Fatalf("%s: 错误信息 %q 不含 %q", tc.name, result.Error.Message, tc.want)
		}
	}
}

// TestDuplicateBindingRejected 确认同一容器/单元不能被两个应用同时占用。
func TestDuplicateBindingRejected(t *testing.T) {
	f := newAdminFixture(t)
	mustCreateApp(t, f, map[string]any{"name": "A", "type": "docker",
		"binding": map[string]any{"container_id": apiAppContainerID}})

	raw, _ := json.Marshal(map[string]any{"name": "B", "type": "docker",
		"binding": map[string]any{"container_id": apiAppContainerID}})
	rec := f.do(t, http.MethodPost, "/api/v1/apps", string(raw))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("重复绑定状态码 = %d，期望 422（%s）", rec.Code, rec.Body.String())
	}
	if got := parseResult(t, rec); got.Error == nil || got.Error.Code != "VALIDATION_ERROR" {
		t.Fatalf("错误码 = %+v", got.Error)
	}
}

// TestPublicURLRules 确认公共入口地址的规矩（生产模式下回环地址被拒）。
func TestPublicURLRules(t *testing.T) {
	f := newAdminFixture(t)
	cases := []struct {
		url  string
		name string
		ok   bool
		want string
	}{
		{url: "https://panel.example.com/app", name: "带路径的域名", ok: true},
		{url: "http://10.0.0.5:8080", name: "内网地址", ok: true},
		{url: "https://127.0.0.1:8080", name: "回环 IPv4", want: "回环"},
		{url: "https://localhost", name: "localhost", want: "回环"},
		{url: "https://0.0.0.0", name: "通配地址", want: "浏览器"},
		{url: "ftp://example.com", name: "非 http 协议", want: "http"},
		{url: "https://user:pw@example.com", name: "带账号密码", want: "用户名或密码"},
	}
	for _, tc := range cases {
		body := map[string]any{"name": "入口-" + tc.name, "type": "link", "public_url": tc.url}
		raw, _ := json.Marshal(body)
		rec := f.do(t, http.MethodPost, "/api/v1/apps", string(raw))
		if tc.ok {
			if rec.Code != http.StatusCreated {
				t.Fatalf("%s: 状态码 = %d，期望 201（%s）", tc.name, rec.Code, rec.Body.String())
			}
			continue
		}
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: 状态码 = %d，期望 400（%s）", tc.name, rec.Code, rec.Body.String())
		}
		if msg := parseResult(t, rec).Error.Message; !strings.Contains(msg, tc.want) {
			t.Fatalf("%s: 错误信息 %q 不含 %q", tc.name, msg, tc.want)
		}
	}
}

// TestHealthTargetMustBeDiscoveredID 确认健康检查目标只能是探测出来的标识。
//
// 这一条是结构性的 SSRF 防线：target_id 的形状容不下主机名、IP、端口或完整 URL，
// 所以“填一个 169.254.169.254 让面板去请求”这件事在接口层根本无法表达。
func TestHealthTargetMustBeDiscoveredID(t *testing.T) {
	f := newAdminFixture(t)
	for _, target := range []string{
		"127.0.0.1", "169.254.169.254", "localhost:8080",
		"http://local_1", "local_", "Local_1", "host_1",
	} {
		raw, _ := json.Marshal(map[string]any{
			"name": "健康检查", "type": "link", "public_url": "https://a.example.com",
			"health_check": map[string]any{"enabled": true, "target_id": target, "port": 80},
		})
		rec := f.do(t, http.MethodPost, "/api/v1/apps", string(raw))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("target_id=%q 状态码 = %d，期望 400（%s）", target, rec.Code, rec.Body.String())
		}
	}

	// 合法的探测标识通过，并且默认值被补齐（scheme=http、path=/、间隔 30、超时 3）。
	created := mustCreateApp(t, f, map[string]any{
		"name": "健康检查", "type": "systemd", "binding": map[string]any{"unit": "app.service"},
		"health_check": map[string]any{"enabled": true, "target_id": "local_1", "port": 8080},
	})
	if created.HealthCheck.TargetID != "local_1" || created.HealthCheck.Scheme != "http" ||
		created.HealthCheck.Path != "/" || created.HealthCheck.IntervalSeconds != 30 ||
		created.HealthCheck.TimeoutSeconds != 3 {
		t.Fatalf("健康检查默认值 = %+v", created.HealthCheck)
	}
	// 启用但还没探测：HTTP 状态只能是 unknown。
	if created.HTTPState != "unknown" {
		t.Fatalf("启用健康检查但未探测时 http_state = %q", created.HTTPState)
	}
	// 没给期望状态码时是空集合（语义为“默认 200—399”），不是 null。
	if created.HealthCheck.ExpectedStatusCodes == nil {
		t.Fatal("expected_status_codes 应为空数组而非 null")
	}
	if len(created.HealthCheck.ExpectedStatusCodes) != 0 {
		t.Fatalf("未指定期望状态码时应为空集合，实际 %v", created.HealthCheck.ExpectedStatusCodes)
	}
}

// TestHealthStatusCodesAreNormalized 确认期望状态码去重、排序、超限被拒。
func TestHealthStatusCodesAreNormalized(t *testing.T) {
	f := newAdminFixture(t)
	created := mustCreateApp(t, f, map[string]any{
		"name": "健康检查", "type": "systemd", "binding": map[string]any{"unit": "app.service"},
		"health_check": map[string]any{
			"enabled": true, "target_id": "cntr_web", "port": 8080,
			"expected_status_codes": []int{204, 200, 204, 302},
		},
	})
	got := created.HealthCheck.ExpectedStatusCodes
	if len(got) != 3 || got[0] != 200 || got[1] != 204 || got[2] != 302 {
		t.Fatalf("期望状态码 = %v，期望 [200 204 302]", got)
	}

	raw, _ := json.Marshal(map[string]any{
		"name": "健康检查", "type": "link", "public_url": "https://a.example.com",
		"health_check": map[string]any{
			"enabled": true, "target_id": "local_1", "port": 80,
			"expected_status_codes": []int{99},
		},
	})
	if rec := f.do(t, http.MethodPost, "/api/v1/apps", string(raw)); rec.Code != http.StatusBadRequest {
		t.Fatalf("非法状态码 = %d，期望 400（%s）", rec.Code, rec.Body.String())
	}
}

// TestUpdateAppRules 覆盖版本必填、乐观锁、空补丁与改类型必须带绑定。
func TestUpdateAppRules(t *testing.T) {
	f := newAdminFixture(t)
	app := mustCreateApp(t, f, map[string]any{
		"name": "服务", "type": "systemd", "binding": map[string]any{"unit": "nginx.service"},
	})

	send := func(body string) *httptest.ResponseRecorder {
		return f.do(t, http.MethodPatch, "/api/v1/apps/"+app.ID, body)
	}

	if rec := send(`{"name":"改名"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("缺 version 状态码 = %d，期望 400（%s）", rec.Code, rec.Body.String())
	}
	if rec := send(`{"version":1}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("空补丁状态码 = %d，期望 400（%s）", rec.Code, rec.Body.String())
	}
	if rec := send(`{"version":1,"nope":1}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("未知字段状态码 = %d，期望 400（%s）", rec.Code, rec.Body.String())
	}
	if rec := send(`{"version":1,"type":"docker"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("只改类型状态码 = %d，期望 400（%s）", rec.Code, rec.Body.String())
	}

	rec := send(`{"version":1,"name":"反向代理"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("正常更新状态码 = %d（%s）", rec.Code, rec.Body.String())
	}
	var updated appDTO
	if err := json.Unmarshal(parseResult(t, rec).Data, &updated); err != nil {
		t.Fatalf("解析更新结果失败: %v", err)
	}
	if updated.Version != 2 || updated.Name != "反向代理" || updated.Binding.Unit != "nginx.service" {
		t.Fatalf("更新结果 = %+v", updated)
	}

	rec = send(`{"version":1,"name":"又改"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("旧版本状态码 = %d，期望 409（%s）", rec.Code, rec.Body.String())
	}
	if got := parseResult(t, rec); got.Error == nil || got.Error.Code != string(httpx.CodeVersionConflict) {
		t.Fatalf("错误码 = %+v", got.Error)
	}

	// 改类型并同时给绑定：通过，旧单元名消失。
	rec = send(`{"version":2,"type":"docker","binding":{"container_id":"` + apiAppContainerID + `"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("改类型状态码 = %d（%s）", rec.Code, rec.Body.String())
	}
	// 必须解到新变量：解到旧变量时，响应里缺席的键会留着上一次的值（unit 就是这么“复活”的）。
	var switched appDTO
	if err := json.Unmarshal(parseResult(t, rec).Data, &switched); err != nil {
		t.Fatalf("解析更新结果失败: %v", err)
	}
	if switched.Type != "docker" || switched.Binding.ContainerID != apiAppContainerID || switched.Binding.Unit != "" {
		t.Fatalf("改类型后的记录 = %+v", switched)
	}
}

// TestDeleteAppThenGone 确认删除后读不到，且重复删除是 404 而不是成功。
func TestDeleteAppThenGone(t *testing.T) {
	f := newAdminFixture(t)
	app := mustCreateApp(t, f, createAppBody("面板", "link"))

	if rec := f.do(t, http.MethodDelete, "/api/v1/apps/"+app.ID+"?version=2", ""); rec.Code != http.StatusConflict {
		t.Fatalf("版本不符状态码 = %d，期望 409（%s）", rec.Code, rec.Body.String())
	}
	if rec := f.do(t, http.MethodDelete, "/api/v1/apps/"+app.ID, ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("缺 version 状态码 = %d，期望 400（%s）", rec.Code, rec.Body.String())
	}
	if rec := f.do(t, http.MethodDelete, "/api/v1/apps/"+app.ID+"?version=1", ""); rec.Code != http.StatusOK {
		t.Fatalf("删除状态码 = %d（%s）", rec.Code, rec.Body.String())
	}
	if rec := f.do(t, http.MethodGet, "/api/v1/apps/"+app.ID, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("删除后读取状态码 = %d，期望 404（%s）", rec.Code, rec.Body.String())
	}
	if rec := f.do(t, http.MethodDelete, "/api/v1/apps/"+app.ID+"?version=1", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("重复删除状态码 = %d，期望 404（%s）", rec.Code, rec.Body.String())
	}
}

// TestAppPermissions viewer 能看能收藏，但不能增删改。
func TestAppPermissions(t *testing.T) {
	f := newAdminFixture(t)
	app := mustCreateApp(t, f, createAppBody("面板", "link"))
	f.createUser(t, "app-watcher", "viewer", "watcher-horse-9")
	viewerCookie, viewerCSRF := loginAsUser(t, f.srv, "app-watcher", "watcher-horse-9")

	if rec := f.doAs(t, viewerCookie, viewerCSRF, http.MethodGet, "/api/v1/apps", ""); rec.Code != http.StatusOK {
		t.Fatalf("viewer 读列表状态码 = %d（%s）", rec.Code, rec.Body.String())
	}
	if rec := f.doAs(t, viewerCookie, viewerCSRF, http.MethodGet, "/api/v1/apps/"+app.ID, ""); rec.Code != http.StatusOK {
		t.Fatalf("viewer 读单个状态码 = %d（%s）", rec.Code, rec.Body.String())
	}
	if rec := f.doAs(t, viewerCookie, viewerCSRF, http.MethodGet, "/api/v1/app-groups", ""); rec.Code != http.StatusOK {
		t.Fatalf("viewer 读分组状态码 = %d（%s）", rec.Code, rec.Body.String())
	}

	writeCases := []struct{ method, path, body string }{
		{http.MethodPost, "/api/v1/apps", `{"name":"越权","type":"link","public_url":"https://a.example.com"}`},
		{http.MethodPatch, "/api/v1/apps/" + app.ID, `{"version":1,"name":"越权"}`},
		{http.MethodDelete, "/api/v1/apps/" + app.ID + "?version=1", ""},
		{http.MethodPost, "/api/v1/app-groups", `{"name":"越权组"}`},
		{http.MethodPatch, "/api/v1/app-groups/grp_00000000000000000000000000000000", `{"version":1,"name":"越权"}`},
		{http.MethodDelete, "/api/v1/app-groups/grp_00000000000000000000000000000000?version=1", ""},
	}
	for _, tc := range writeCases {
		rec := f.doAs(t, viewerCookie, viewerCSRF, tc.method, tc.path, tc.body)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("viewer %s %s 状态码 = %d，期望 403（%s）",
				tc.method, tc.path, rec.Code, rec.Body.String())
		}
	}

	// 收藏属于“自己的偏好”，viewer 可以设。
	rec := f.doAs(t, viewerCookie, viewerCSRF, http.MethodPut,
		"/api/v1/apps/"+app.ID+"/favorite", `{"favorite":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("viewer 收藏状态码 = %d，期望 200（%s）", rec.Code, rec.Body.String())
	}
	if f.doAs(t, viewerCookie, viewerCSRF, http.MethodDelete,
		"/api/v1/apps/"+app.ID+"?version=1", "").Code != http.StatusForbidden {
		t.Fatal("viewer 不该能删除应用")
	}
	if rec := f.do(t, http.MethodGet, "/api/v1/apps/"+app.ID, ""); rec.Code != http.StatusOK {
		t.Fatalf("应用被越权删除了: %d", rec.Code)
	}
}

// TestAppWriteRequiresCSRF 确认写操作缺 CSRF 令牌会被挡。
func TestAppWriteRequiresCSRF(t *testing.T) {
	f := newAdminFixture(t)
	app := mustCreateApp(t, f, createAppBody("面板", "link"))

	for _, tc := range []struct{ method, path, body string }{
		{http.MethodPost, "/api/v1/apps", `{"name":"x","type":"link","public_url":"https://a.example.com"}`},
		{http.MethodPatch, "/api/v1/apps/" + app.ID, `{"version":1,"name":"x"}`},
		{http.MethodDelete, "/api/v1/apps/" + app.ID + "?version=1", ""},
		{http.MethodPut, "/api/v1/apps/" + app.ID + "/favorite", `{"favorite":true}`},
		{http.MethodPost, "/api/v1/app-groups", `{"name":"组"}`},
	} {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		req.Header.Set("Origin", testOrigin)
		req.Header.Set("Cookie", f.cookie.String())
		if tc.body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		rec := httptest.NewRecorder()
		f.srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s %s 缺 CSRF 状态码 = %d，期望 403（%s）",
				tc.method, tc.path, rec.Code, rec.Body.String())
		}
		if got := parseResult(t, rec); got.Error == nil || got.Error.Code != string(httpx.CodeCSRFFailed) {
			t.Fatalf("%s %s 错误码 = %+v", tc.method, tc.path, got.Error)
		}
	}
}

// TestFavoriteIsPerUser 确认收藏是每个人自己的偏好。
func TestFavoriteIsPerUser(t *testing.T) {
	f := newAdminFixture(t)
	app := mustCreateApp(t, f, createAppBody("面板", "link"))
	f.createUser(t, "fav-watcher", "viewer", "watcher-horse-9")
	viewerCookie, viewerCSRF := loginAsUser(t, f.srv, "fav-watcher", "watcher-horse-9")

	if rec := f.do(t, http.MethodPut, "/api/v1/apps/"+app.ID+"/favorite", `{"favorite":true}`); rec.Code != http.StatusOK {
		t.Fatalf("管理员收藏状态码 = %d（%s）", rec.Code, rec.Body.String())
	}

	get := func(cookie *http.Cookie, csrf string) appDTO {
		rec := f.doAs(t, cookie, csrf, http.MethodGet, "/api/v1/apps/"+app.ID, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("读取状态码 = %d（%s）", rec.Code, rec.Body.String())
		}
		var out appDTO
		if err := json.Unmarshal(parseResult(t, rec).Data, &out); err != nil {
			t.Fatalf("解析失败: %v", err)
		}
		return out
	}
	if !get(f.cookie, f.csrf).Favorite {
		t.Fatal("管理员应看到已收藏")
	}
	if get(viewerCookie, viewerCSRF).Favorite {
		t.Fatal("viewer 不该看到别人的收藏")
	}

	// 收藏幂等：重复设置同一个值不报错。
	if rec := f.do(t, http.MethodPut, "/api/v1/apps/"+app.ID+"/favorite", `{"favorite":true}`); rec.Code != http.StatusOK {
		t.Fatalf("重复收藏状态码 = %d（%s）", rec.Code, rec.Body.String())
	}
	if rec := f.do(t, http.MethodPut, "/api/v1/apps/"+app.ID+"/favorite", `{"favorite":false}`); rec.Code != http.StatusOK {
		t.Fatalf("取消收藏状态码 = %d（%s）", rec.Code, rec.Body.String())
	}
	if get(f.cookie, f.csrf).Favorite {
		t.Fatal("取消收藏后不该仍是收藏状态")
	}
	if rec := f.do(t, http.MethodPut,
		"/api/v1/apps/app_00000000000000000000000000000000/favorite", `{"favorite":true}`); rec.Code != http.StatusNotFound {
		t.Fatalf("收藏不存在的应用状态码 = %d，期望 404（%s）", rec.Code, rec.Body.String())
	}
}

// TestAppGroupsThroughAPI 覆盖分组的增查改删与“非空不能删”。
func TestAppGroupsThroughAPI(t *testing.T) {
	f := newAdminFixture(t)

	rec := f.do(t, http.MethodPost, "/api/v1/app-groups", `{"name":"运维"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("建分组状态码 = %d（%s）", rec.Code, rec.Body.String())
	}
	var group groupDTO
	if err := json.Unmarshal(parseResult(t, rec).Data, &group); err != nil {
		t.Fatalf("解析分组失败: %v", err)
	}
	if group.ID == "" || group.Version != 1 {
		t.Fatalf("分组 = %+v", group)
	}

	if rec := f.do(t, http.MethodPost, "/api/v1/app-groups", `{"name":"运维"}`); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("重名状态码 = %d，期望 422（%s）", rec.Code, rec.Body.String())
	}

	rec = f.do(t, http.MethodGet, "/api/v1/app-groups", "")
	var list groupListResponse
	if err := json.Unmarshal(parseResult(t, rec).Data, &list); err != nil {
		t.Fatalf("解析分组列表失败: %v", err)
	}
	if len(list.Items) != 1 || list.Items[0].ID != group.ID {
		t.Fatalf("分组列表 = %+v", list.Items)
	}

	rec = f.do(t, http.MethodPatch, "/api/v1/app-groups/"+group.ID, `{"version":1,"name":"基础设施"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("改分组名状态码 = %d（%s）", rec.Code, rec.Body.String())
	}
	if rec := f.do(t, http.MethodPatch, "/api/v1/app-groups/"+group.ID, `{"version":1,"name":"再改"}`); rec.Code != http.StatusConflict {
		t.Fatalf("旧版本状态码 = %d，期望 409（%s）", rec.Code, rec.Body.String())
	}

	app := mustCreateApp(t, f, map[string]any{
		"name": "面板", "type": "link", "public_url": "https://panel.example.com",
		"group_id": group.ID,
	})
	if app.GroupID == nil || *app.GroupID != group.ID {
		t.Fatalf("应用分组 = %v", app.GroupID)
	}

	rec = f.do(t, http.MethodDelete, "/api/v1/app-groups/"+group.ID+"?version=2", "")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("非空分组删除状态码 = %d，期望 422（%s）", rec.Code, rec.Body.String())
	}
	if got := parseResult(t, rec); got.Error == nil || got.Error.Code != string(httpx.CodeDependencyBlocked) {
		t.Fatalf("错误码 = %+v", got.Error)
	}

	// 先移出分组（显式 null），再删。
	rec = f.do(t, http.MethodPatch, "/api/v1/apps/"+app.ID, `{"version":1,"group_id":null}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("移出分组状态码 = %d（%s）", rec.Code, rec.Body.String())
	}
	var moved appDTO
	if err := json.Unmarshal(parseResult(t, rec).Data, &moved); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if moved.GroupID != nil {
		t.Fatalf("移出分组后 group_id = %v", *moved.GroupID)
	}
	if rec := f.do(t, http.MethodDelete, "/api/v1/app-groups/"+group.ID+"?version=2", ""); rec.Code != http.StatusOK {
		t.Fatalf("删空分组状态码 = %d（%s）", rec.Code, rec.Body.String())
	}
	if rec := f.do(t, http.MethodDelete, "/api/v1/app-groups/"+group.ID+"?version=2", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("重复删分组状态码 = %d，期望 404（%s）", rec.Code, rec.Body.String())
	}
}

// TestVolumeDependencyOutcomes 区分“无法确认”“卷不存在”“卷存在”三种结局。
func TestVolumeDependencyOutcomes(t *testing.T) {
	body := map[string]any{
		"name": "数据应用", "type": "systemd", "binding": map[string]any{"unit": "data.service"},
		"volume_dependencies": []string{apiAppVolumeID},
	}
	raw, _ := json.Marshal(body)

	// 1) 没有卷检查器（演示模式或代理连接未配置）：不写库，报“暂时无法确认”。
	noChecker := newAppsFixture(t, func(db *store.DB) *apps.Service { return newTestApps(t, db) })
	rec := noChecker.do(t, http.MethodPost, "/api/v1/apps", string(raw))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("无检查器状态码 = %d，期望 503（%s）", rec.Code, rec.Body.String())
	}
	if got := parseResult(t, rec); got.Error == nil || got.Error.Code != string(httpx.CodeAgentUnavailable) {
		t.Fatalf("错误码 = %+v", got.Error)
	}
	var count int
	if err := noChecker.db.SQL().QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM apps`).Scan(&count); err != nil {
		t.Fatalf("统计失败: %v", err)
	}
	if count != 0 {
		t.Fatalf("无法确认时不应写库，实际 %d 行", count)
	}

	// 2) 代理列表里没有这个卷：业务拒绝（422），带出卷标识。
	missing := newAppsFixture(t, func(db *store.DB) *apps.Service {
		return newTestAppsWithVolumes(t, db, apiAppOtherVolumeID)
	})
	rec = missing.do(t, http.MethodPost, "/api/v1/apps", string(raw))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("卷不存在状态码 = %d，期望 422（%s）", rec.Code, rec.Body.String())
	}
	got := parseResult(t, rec)
	if got.Error == nil || got.Error.Code != string(httpx.CodeDependencyMissing) {
		t.Fatalf("错误码 = %+v", got.Error)
	}
	if got.Error.Details["volume_id"] != apiAppVolumeID {
		t.Fatalf("details.volume_id = %v", got.Error.Details)
	}

	// 3) 卷存在：通过，并把依赖登记下来（保护状态是“未接管”，不是“已保护”）。
	present := newAppsFixture(t, func(db *store.DB) *apps.Service {
		return newTestAppsWithVolumes(t, db, apiAppVolumeID)
	})
	rec = present.do(t, http.MethodPost, "/api/v1/apps", string(raw))
	if rec.Code != http.StatusCreated {
		t.Fatalf("卷存在状态码 = %d，期望 201（%s）", rec.Code, rec.Body.String())
	}
	var created appDTO
	if err := json.Unmarshal(parseResult(t, rec).Data, &created); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(created.VolumeDeps) != 1 || created.VolumeDeps[0] != apiAppVolumeID {
		t.Fatalf("依赖卷 = %v", created.VolumeDeps)
	}
	if created.DependencyProtection != "unmanaged" {
		t.Fatalf("依赖保护 = %q，期望 unmanaged", created.DependencyProtection)
	}
}

// TestAppWritesAreAudited 确认元数据变更留审计、收藏不留。
func TestAppWritesAreAudited(t *testing.T) {
	f := newAdminFixture(t)
	app := mustCreateApp(t, f, createAppBody("面板", "link"))
	if rec := f.do(t, http.MethodPatch, "/api/v1/apps/"+app.ID, `{"version":1,"name":"改过"}`); rec.Code != http.StatusOK {
		t.Fatalf("更新状态码 = %d（%s）", rec.Code, rec.Body.String())
	}
	if rec := f.do(t, http.MethodDelete, "/api/v1/apps/"+app.ID+"?version=2", ""); rec.Code != http.StatusOK {
		t.Fatalf("删除状态码 = %d（%s）", rec.Code, rec.Body.String())
	}
	if rec := f.do(t, http.MethodPost, "/api/v1/app-groups", `{"name":"运维"}`); rec.Code != http.StatusCreated {
		t.Fatalf("建分组状态码 = %d（%s）", rec.Code, rec.Body.String())
	}

	rows, err := f.db.SQL().QueryContext(t.Context(),
		`SELECT action, outcome, target FROM audit_events WHERE action LIKE 'app%' ORDER BY id`)
	if err != nil {
		t.Fatalf("读取审计失败: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var got []string
	for rows.Next() {
		var action, outcome, target string
		if err := rows.Scan(&action, &outcome, &target); err != nil {
			t.Fatalf("扫描审计失败: %v", err)
		}
		got = append(got, action+"|"+outcome+"|"+target)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历审计失败: %v", err)
	}
	want := []string{
		audit.ActionAppCreate + "|success|app:" + app.ID,
		audit.ActionAppUpdate + "|success|app:" + app.ID,
		audit.ActionAppDelete + "|success|app:" + app.ID,
		audit.ActionAppGroupCreate + "|success|app_group:",
	}
	if len(got) != len(want) {
		t.Fatalf("审计条数 = %d，期望 %d：%v", len(got), len(want), got)
	}
	for i := range want {
		if !strings.HasPrefix(got[i], want[i]) {
			t.Fatalf("第 %d 条审计 = %q，期望前缀 %q", i, got[i], want[i])
		}
	}

	// 收藏不记审计（否则点星星会淹掉真正的管理事件）。
	app2 := mustCreateApp(t, f, map[string]any{
		"name": "第二个", "type": "link", "public_url": "https://b.example.com",
	})
	before := auditCount(t, f.db)
	if rec := f.do(t, http.MethodPut, "/api/v1/apps/"+app2.ID+"/favorite", `{"favorite":true}`); rec.Code != http.StatusOK {
		t.Fatalf("收藏状态码 = %d（%s）", rec.Code, rec.Body.String())
	}
	if after := auditCount(t, f.db); after != before {
		t.Fatalf("收藏产生了 %d 条审计", after-before)
	}
}

// auditCount 返回审计表行数。
func auditCount(t *testing.T, db *store.DB) int {
	t.Helper()
	var n int
	if err := db.SQL().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM audit_events`).Scan(&n); err != nil {
		t.Fatalf("统计审计失败: %v", err)
	}
	return n
}
