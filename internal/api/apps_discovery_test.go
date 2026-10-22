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

	"upanel/internal/apps"
	"upanel/internal/config"
	"upanel/internal/discovery"
	"upanel/internal/httpx"
	"upanel/internal/platform"
	"upanel/internal/version"
)

// 应用发现与健康结果接口（P09.2）。
//
// 断言的重点：
//  1. 发现列表只给 admin——它列出本机所有监听端口与容器，是侦察材料；
//  2. “已绑定”与“绑定对象已经不在了”是两种事实，接口必须分开说；
//     容器重建（ID 变了）只能要求重新确认，不能被自动接到新容器上；
//  3. 没有代理时回 503，而不是给一份空候选列表（空列表会被读成“本机什么都没有”）；
//  4. 健康结果里三种 unknown 分得开：没启用 / 从没探过 / 探过但没结论。

const apiDiscoveryContainerID = "7d1c1a2b3c4d5e6f708192a3b4c5d6e7f809a1b2c3d4e5f60718293a4b5c6d7e8"

// fakeDiscoveryReader 是一份固定的发现结果与探测结论。
type fakeDiscoveryReader struct {
	snap     discovery.Snapshot
	snapErr  error
	probe    discovery.ProbeResult
	probeErr error
	probes   []discovery.ProbeParams
}

func (f *fakeDiscoveryReader) Snapshot(context.Context) (discovery.Snapshot, error) {
	if f.snapErr != nil {
		return discovery.Snapshot{}, f.snapErr
	}
	return f.snap, nil
}

func (f *fakeDiscoveryReader) Probe(_ context.Context, params discovery.ProbeParams) (discovery.ProbeResult, error) {
	f.probes = append(f.probes, params)
	if f.probeErr != nil {
		return discovery.ProbeResult{}, f.probeErr
	}
	return f.probe, nil
}

// discoveryFixture 是带发现读取端的夹具，同时把应用服务交出来给用例直接写结论。
type discoveryFixture struct {
	*adminFixture
	apps *apps.Service
}

func newDiscoveryFixture(t *testing.T, reader DiscoveryReader) *discoveryFixture {
	t.Helper()
	authSvc, aud, db := newTestServicesWithDB(t)
	appSvc := newTestApps(t, db)
	srv, err := New(Options{
		ListenAddress:   config.ListenProduction,
		Mode:            platform.ModeProduction,
		Version:         version.Info{Version: "test", GoVersion: "test"},
		Logger:          quietLogger(),
		ShutdownTimeout: time.Second,
		Auth:            authSvc,
		Audit:           aud,
		Operations:      newTestOperations(t, db, nil),
		Apps:            appSvc,
		Discovery:       reader,
		AllowedOrigins:  testOrigins(),
	})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	seedTestUser(t, srv.opts.Auth, testUser, testUserPass)
	cookie, csrf := loginOK(t, srv)
	return &discoveryFixture{adminFixture: &adminFixture{srv: srv, cookie: cookie, csrf: csrf, db: db}, apps: appSvc}
}

// discoverySnapshot 造一份“nginx + 一个容器”的发现结果。
func discoverySnapshot() discovery.Snapshot {
	return discovery.Snapshot{
		Status:      "ok",
		CollectedAt: time.Date(2026, 10, 22, 9, 30, 0, 0, time.UTC),
		Sources: []discovery.SourceState{
			{Source: discovery.SourceSystemd, Status: discovery.StatusOK, Count: 2},
			{Source: discovery.SourceDocker, Status: discovery.StatusOK, Count: 1},
		},
		Candidates: []discovery.Candidate{
			{
				TargetID: "local_nginx", Kind: discovery.KindSystemd, Source: discovery.SourceSystemd,
				Name: "nginx.service", Title: "nginx", Reason: "单元 nginx.service 的进程在监听 80",
				ObservedState:   "running",
				Listeners:       []discovery.Listener{{Address: "0.0.0.0", Port: 80, Protocol: "tcp", Loopback: true}},
				SuggestedScheme: "http", SuggestedPort: 80, ProbeAvailable: true,
				Unit: "nginx.service",
			},
			{
				TargetID: "cntr_web", Kind: discovery.KindDocker, Source: discovery.SourceDocker,
				Name: "web", Title: "web", Reason: "容器 web 发布了宿主机端口 9000",
				ObservedState:   "running",
				Listeners:       []discovery.Listener{{Address: "127.0.0.1", Port: 9000, Protocol: "tcp", Loopback: true}},
				SuggestedScheme: "http", SuggestedPort: 9000, ProbeAvailable: true,
				ContainerID: apiDiscoveryContainerID,
			},
		},
	}
}

// TestAppDiscoveryRequiresAdmin 确认候选列表只给管理员。
func TestAppDiscoveryRequiresAdmin(t *testing.T) {
	reader := &fakeDiscoveryReader{snap: discoverySnapshot()}
	f := newDiscoveryFixture(t, reader)

	// 未登录：401。
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/apps/discovery", nil)
	f.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("未登录状态码 = %d，期望 401（%s）", rec.Code, rec.Body.String())
	}

	// viewer：403。候选列表暴露本机所有监听端口，不是只读用户该看的东西。
	f.createUser(t, "disc-watcher", "viewer", "watcher-horse-9")
	cookie, csrf := loginAsUser(t, f.srv, "disc-watcher", "watcher-horse-9")
	if rec := f.doAs(t, cookie, csrf, http.MethodGet, "/api/v1/apps/discovery", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer 状态码 = %d，期望 403（%s）", rec.Code, rec.Body.String())
	}

	// admin：200，且候选原样带出。
	rec = f.do(t, http.MethodGet, "/api/v1/apps/discovery", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("admin 状态码 = %d（%s）", rec.Code, rec.Body.String())
	}
}

// TestAppDiscoveryMarksBindings 确认已绑定候选被标出，失效绑定单独列出。
// TestAppDiscoverySkipsIssuesWhenSourceFailed 确认“这一轮没采成”不会被写成“绑定对象不在了”。
//
// 具体场景：代理刚重启，services 还没有第一次快照，systemd 来源这一轮是 error。这时如果
// 照旧按候选表判断，每一个 systemd 绑定都会冒出“这个单元当前没有在监听任何端口”，用户
// 会照着这句话去删一条其实好好的应用。没采成就不下结论——快照的 sources 里已经写着哪个
// 来源失败了，那才是该看的地方。
func TestAppDiscoverySkipsIssuesWhenSourceFailed(t *testing.T) {
	snap := discoverySnapshot()
	snap.Status = discovery.StatusPartial
	snap.Sources = []discovery.SourceState{
		{Source: discovery.SourceSystemd, Status: discovery.StatusError, Error: "服务状态尚未完成第一次采集"},
		{Source: discovery.SourceDocker, Status: "unavailable", Error: "本机没有 docker 命令，容器来源不可用"},
	}
	// 两份来源都没采成，候选列表自然是空的。
	snap.Candidates = nil
	f := newDiscoveryFixture(t, &fakeDiscoveryReader{snap: snap})

	mustCreateApp(t, f.adminFixture, map[string]any{
		"name": "反向代理", "type": "systemd", "binding": map[string]any{"unit": "nginx.service"},
	})
	mustCreateApp(t, f.adminFixture, map[string]any{
		"name": "容器站点", "type": "docker",
		"binding": map[string]any{"container_id": apiAppContainerID},
	})

	rec := f.do(t, http.MethodGet, "/api/v1/apps/discovery", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d（%s）", rec.Code, rec.Body.String())
	}
	var body struct {
		Data appDiscoveryResponse `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v（%s）", err, rec.Body.String())
	}
	if len(body.Data.MissingBindings) != 0 {
		t.Fatalf("来源没采成时不该报失效绑定，实际 %+v", body.Data.MissingBindings)
	}
	if body.Data.Status != discovery.StatusPartial {
		t.Fatalf("快照状态 = %q，期望 partial（部分来源失败要如实说）", body.Data.Status)
	}
}

func TestAppDiscoveryMarksBindings(t *testing.T) {
	f := newDiscoveryFixture(t, &fakeDiscoveryReader{snap: discoverySnapshot()})

	bound := mustCreateApp(t, f.adminFixture, map[string]any{
		"name": "反向代理", "type": "systemd", "binding": map[string]any{"unit": "nginx.service"},
	})
	// 一个容器被重建过：应用记的是旧 ID，本机现在只有新的那个。
	rebuilt := mustCreateApp(t, f.adminFixture, map[string]any{
		"name": "旧容器", "type": "docker",
		"binding": map[string]any{"container_id": apiAppContainerID},
	})
	// 一个已经不监听任何端口的单元：它不在候选里，因此“面板看不到它”也是事实，
	// 但原因与容器重建不同——它可能只是被停用了，不一定是身份变了。
	quiet := mustCreateApp(t, f.adminFixture, map[string]any{
		"name": "没在监听的单元", "type": "systemd",
		"binding": map[string]any{"unit": "worker.service"},
	})

	rec := f.do(t, http.MethodGet, "/api/v1/apps/discovery", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d（%s）", rec.Code, rec.Body.String())
	}
	var body struct {
		Data appDiscoveryResponse `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v（%s）", err, rec.Body.String())
	}
	data := body.Data
	if len(data.Candidates) != 2 {
		t.Fatalf("候选数 = %d，期望 2", len(data.Candidates))
	}

	nginx := data.Candidates[0]
	if nginx.TargetID != "local_nginx" {
		t.Fatalf("第一项 = %+v", nginx)
	}
	if nginx.BoundAppID == nil || *nginx.BoundAppID != bound.ID {
		t.Fatalf("nginx 候选的已绑定应用 = %v，期望 %s", nginx.BoundAppID, bound.ID)
	}
	if nginx.BoundAppName == nil || *nginx.BoundAppName != "反向代理" {
		t.Fatalf("nginx 候选的已绑定应用名 = %v", nginx.BoundAppName)
	}

	// 新容器还没有应用绑定它。
	if web := data.Candidates[1]; web.BoundAppID != nil {
		t.Fatalf("容器候选不该有绑定，实际 %v", *web.BoundAppID)
	}

	// 失效绑定两条：容器 ID 不在列表里（需要重新确认），单元没有候选（可能被停用）。
	if len(data.MissingBindings) != 2 {
		t.Fatalf("失效绑定数 = %d，期望 2（%+v）", len(data.MissingBindings), data.MissingBindings)
	}
	issues := make(map[string]appBindingIssue, len(data.MissingBindings))
	for _, item := range data.MissingBindings {
		issues[item.AppID] = item
	}
	issue, ok := issues[rebuilt.ID]
	if !ok {
		t.Fatalf("容器重建没有出现在失效绑定里: %+v", data.MissingBindings)
	}
	if issue.Type != string(apps.TypeDocker) || issue.Binding != apiAppContainerID {
		t.Fatalf("失效绑定 = %+v", issue)
	}
	if !strings.Contains(issue.Reason, "重新确认") {
		t.Fatalf("容器重建的原因必须要求重新确认: %q", issue.Reason)
	}
	if got, ok := issues[quiet.ID]; !ok || got.Binding != "worker.service" {
		t.Fatalf("没在监听的单元没有出现在失效绑定里: %+v", data.MissingBindings)
	}

	// 容器重建不能被悄悄接上：旧应用仍然指着旧 ID。
	var stored appDTO
	rec = f.do(t, http.MethodGet, "/api/v1/apps/"+rebuilt.ID, "")
	if err := json.Unmarshal(parseResult(t, rec).Data, &stored); err != nil {
		t.Fatalf("解析应用失败: %v", err)
	}
	if stored.Binding.ContainerID != apiAppContainerID {
		t.Fatalf("应用绑定被改成了 %q，重建的容器不该自动绑定", stored.Binding.ContainerID)
	}
}

// TestAppDiscoveryUnavailable 确认没有代理时回 503，而不是一份空列表。
func TestAppDiscoveryUnavailable(t *testing.T) {
	// 1. 部署里没有发现读取端（演示模式）。
	plain := newAdminFixture(t)
	rec := plain.do(t, http.MethodGet, "/api/v1/apps/discovery", "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("无读取端状态码 = %d，期望 503（%s）", rec.Code, rec.Body.String())
	}
	if got := parseResult(t, rec); got.Error == nil || got.Error.Code != string(httpx.CodeAgentUnavailable) {
		t.Fatalf("错误码 = %+v，期望 %s", got.Error, httpx.CodeAgentUnavailable)
	}

	// 2. 读取端在，但代理不可用。
	f := newDiscoveryFixture(t, &fakeDiscoveryReader{
		snapErr: errors.Join(discovery.ErrAgentUnavailable, errors.New("socket 不存在")),
	})
	rec = f.do(t, http.MethodGet, "/api/v1/apps/discovery", "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("代理不可用状态码 = %d，期望 503（%s）", rec.Code, rec.Body.String())
	}

	// 3. 其它错误是 500，并且不带出内部细节。
	f = newDiscoveryFixture(t, &fakeDiscoveryReader{snapErr: errors.New("/proc/net/tcp 读取失败")})
	rec = f.do(t, http.MethodGet, "/api/v1/apps/discovery", "")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("内部错误状态码 = %d，期望 500（%s）", rec.Code, rec.Body.String())
	}
	if got := parseResult(t, rec); got.Error != nil && strings.Contains(got.Error.Message, "/proc") {
		t.Fatalf("500 响应泄露了内部路径：%s", got.Error.Message)
	}
}

// TestAppHealthEndpointStates 覆盖三种 unknown 与一条真实结论的形态。
func TestAppHealthEndpointStates(t *testing.T) {
	f := newDiscoveryFixture(t, &fakeDiscoveryReader{})

	// 1. 没启用健康检查：not_applicable，并且说明不会发请求。
	off := mustCreateApp(t, f.adminFixture, createAppBody("面板", "link"))
	state := getAppHealth(t, f.adminFixture, off.ID)
	if state.Enabled || state.HTTPState != "not_applicable" {
		t.Fatalf("未启用健康检查的状态 = %+v", state)
	}
	if state.Note == "" || state.CheckedAt != nil {
		t.Fatalf("未启用时不该有 checked_at，且必须说明原因: %+v", state)
	}
	if state.Address != nil || state.URL != nil {
		t.Fatalf("未启用时不该有地址: %+v", state)
	}

	// 2. 启用了但从没探过：unknown + “还没有探测结果”。
	app := mustCreateApp(t, f.adminFixture, map[string]any{
		"name": "站点", "type": "systemd", "binding": map[string]any{"unit": "nginx.service"},
		"health_check": map[string]any{
			"enabled": true, "target_id": "local_1", "port": 8080,
			"expected_status_codes": []int{200, 204},
		},
	})
	state = getAppHealth(t, f.adminFixture, app.ID)
	if state.HTTPState != "unknown" || state.CheckedAt != nil || state.Note == "" {
		t.Fatalf("从未探测时的状态 = %+v", state)
	}
	if len(state.ExpectedStatusCodes) != 2 {
		t.Fatalf("期望状态码 = %v", state.ExpectedStatusCodes)
	}

	// 3. 一条真实的失败结论：状态码、耗时、地址都如实带出。
	code := 500
	if err := f.apps.SaveHealthResult(context.Background(), apps.HealthResult{
		AppID:      app.ID,
		CheckedAt:  time.Date(2026, 10, 22, 9, 31, 0, 0, time.UTC),
		HTTPState:  apps.HTTPError,
		StatusCode: &code,
		DurationMS: 12,
		TargetID:   "local_1",
		Scheme:     "http",
		Port:       8080,
		Path:       "/healthz",
		Address:    "127.0.0.1",
		URL:        "http://127.0.0.1:8080/healthz",
		Error:      "状态码 500 不在期望集合里（200、204）",
	}); err != nil {
		t.Fatalf("写入探测结论失败: %v", err)
	}
	state = getAppHealth(t, f.adminFixture, app.ID)
	if state.HTTPState != "error" || state.StatusCode == nil || *state.StatusCode != 500 {
		t.Fatalf("失败结论 = %+v", state)
	}
	if state.CheckedAt == nil || *state.CheckedAt != "2026-10-22T09:31:00Z" {
		t.Fatalf("checked_at = %v", state.CheckedAt)
	}
	if state.DurationMS == nil || *state.DurationMS != 12 {
		t.Fatalf("duration_ms = %v", state.DurationMS)
	}
	if state.URL == nil || *state.URL != "http://127.0.0.1:8080/healthz" {
		t.Fatalf("url = %v", state.URL)
	}
	// 结论里的 path 是上次实际探的值（这里和定义不同），必须如实显示历史。
	if state.Path != "/healthz" {
		t.Fatalf("结论里的 path = %q", state.Path)
	}
	if state.LastError == "" {
		t.Fatal("失败结论必须带原因")
	}

	// 4. 探测没得出结果（例如代理不可用）：unknown，地址为空，不能假装探过。
	if err := f.apps.SaveHealthResult(context.Background(), apps.HealthResult{
		AppID:     app.ID,
		CheckedAt: time.Date(2026, 10, 22, 9, 32, 0, 0, time.UTC),
		HTTPState: apps.HTTPUnknown,
		TargetID:  "local_1",
		Scheme:    "http",
		Port:      8080,
		Path:      "/",
		Error:     "代理不可用，本轮没有发起探测：socket 不存在",
	}); err != nil {
		t.Fatalf("写入探测结论失败: %v", err)
	}
	state = getAppHealth(t, f.adminFixture, app.ID)
	if state.HTTPState != "unknown" || state.Note == "" || state.LastError == "" {
		t.Fatalf("无结论时的状态 = %+v", state)
	}
	if state.Address != nil || state.URL != nil {
		t.Fatalf("没有发出请求时不该有地址: %+v", state)
	}
	if state.StatusCode != nil {
		t.Fatalf("没有拿到状态码时 status_code 必须是 null，实际 %v", *state.StatusCode)
	}
}

// TestAppHealthPermissionsAndMissing 确认健康结果对已登录用户可读，未知应用是 404。
func TestAppHealthPermissionsAndMissing(t *testing.T) {
	f := newDiscoveryFixture(t, &fakeDiscoveryReader{})
	app := mustCreateApp(t, f.adminFixture, createAppBody("面板", "link"))

	if rec := f.do(t, http.MethodGet, "/api/v1/apps/"+app.ID+"/health", ""); rec.Code != http.StatusOK {
		t.Fatalf("admin 状态码 = %d（%s）", rec.Code, rec.Body.String())
	}

	// viewer 可以读：健康结论只是“这个入口通不通”，不含侦察材料。
	f.createUser(t, "health-watcher", "viewer", "watcher-horse-9")
	cookie, csrf := loginAsUser(t, f.srv, "health-watcher", "watcher-horse-9")
	if rec := f.doAs(t, cookie, csrf, http.MethodGet,
		"/api/v1/apps/"+app.ID+"/health", ""); rec.Code != http.StatusOK {
		t.Fatalf("viewer 状态码 = %d，期望 200（%s）", rec.Code, rec.Body.String())
	}

	// 未登录：401。
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/apps/"+app.ID+"/health", nil)
	f.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("未登录状态码 = %d，期望 401", rec.Code)
	}

	// 不存在的应用：404，而不是“没有探测结果”。
	rec = f.do(t, http.MethodGet, "/api/v1/apps/app_00000000000000000000000000000000/health", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("未知应用状态码 = %d，期望 404（%s）", rec.Code, rec.Body.String())
	}
}

// getAppHealth 读取一个应用的健康结论。
func getAppHealth(t *testing.T, f *adminFixture, appID string) appHealthStateDTO {
	t.Helper()
	rec := f.do(t, http.MethodGet, "/api/v1/apps/"+appID+"/health", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("读取健康结论失败: %d %s", rec.Code, rec.Body.String())
	}
	var out appHealthStateDTO
	if err := json.Unmarshal(parseResult(t, rec).Data, &out); err != nil {
		t.Fatalf("解析健康结论失败: %v", err)
	}
	return out
}

// 编译期确认：发现读取端在测试里能替换成替身。
var _ DiscoveryReader = (*fakeDiscoveryReader)(nil)
