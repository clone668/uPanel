package api

// P09.4：应用关联对象的日志与运行摘要。
//
// 断言的重点是四件事：
//  1. 日志对象来自绑定，请求里的 unit/source 一律被拒——否则这个接口就是
//     “按任意单元读日志”的通用入口；
//  2. 两种来源各自的参数集合不互相串味（journal 不收 tail，docker 不收 level）；
//  3. link 没有日志、compose 明确 NOT_IMPLEMENTED，不返回一个空 page 冒充成功；
//  4. AppSummary 的 runtime_state / capabilities / dependency_protection 来自观测，
//     不是从类型推出来的。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"upanel/internal/appruntime"
	"upanel/internal/apps"
	"upanel/internal/config"
	"upanel/internal/containers"
	"upanel/internal/httpx"
	"upanel/internal/journal"
	"upanel/internal/platform"
	"upanel/internal/services"
	"upanel/internal/version"
)

// stubAppServices 是可控的服务快照来源（appruntime.ServicesSource）。
type stubAppServices struct {
	snap services.Snapshot
	err  error
}

func (s stubAppServices) Snapshot(context.Context) (services.Snapshot, error) {
	return s.snap, s.err
}

// stubAppContainers 是可控的容器清单来源（appruntime.ContainersSource）。
type stubAppContainers struct {
	resp containers.ListResponse
	err  error
}

func (s stubAppContainers) List(context.Context) (containers.ListResponse, error) {
	return s.resp, s.err
}

// appLogsFixture 构造一个同时带应用元数据、日志读取端与容器读取端的服务。
func appLogsFixture(t *testing.T, rt *appruntime.Resolver, logs LogsReader,
	cn ContainersReader) *adminFixture {
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
		AllowedOrigins:  testOrigins(),
		AppRuntime:      rt,
		Logs:            logs,
		Containers:      cn,
	})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	seedTestUser(t, srv.opts.Auth, testUser, testUserPass)
	cookie, csrf := loginOK(t, srv)
	return &adminFixture{srv: srv, cookie: cookie, csrf: csrf, db: db}
}

// mustCreateSystemdApp 创建一个绑定到 unit 的应用。
func mustCreateSystemdApp(t *testing.T, f *adminFixture, unit string) appDTO {
	t.Helper()
	return mustCreateApp(t, f, map[string]any{
		"name": "示例服务", "type": "systemd",
		"binding": map[string]any{"unit": unit},
	})
}

// TestAppLogsSystemdUsesBinding 确认日志对象来自绑定，参数只取该来源应该有的那些。
func TestAppLogsSystemdUsesBinding(t *testing.T) {
	logs := &stubLogs{page: sampleLogPage(), paramsCh: make(chan journal.Params, 1)}
	f := appLogsFixture(t, nil, logs, nil)
	app := mustCreateSystemdApp(t, f, "nginx.service")

	rec := f.do(t, http.MethodGet, "/api/v1/apps/"+app.ID+"/logs?limit=5", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("读应用日志失败: %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Source string `json:"source"`
		Unit   string `json:"unit"`
		Page   struct {
			Entries []journal.Entry `json:"entries"`
		} `json:"page"`
	}
	if err := json.Unmarshal(parseResult(t, rec).Data, &out); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if out.Source != "journal" || out.Unit != "nginx.service" {
		t.Fatalf("判别字段 = %q/%q", out.Source, out.Unit)
	}
	if logs.params.Source != journal.SourceJournal || logs.params.Unit != "nginx.service" {
		t.Fatalf("转发给日志读取端的参数 = %+v", logs.params)
	}
	if logs.params.Limit != 5 {
		t.Fatalf("limit 没有透传: %+v", logs.params)
	}
}

// TestAppLogsRejectsObjectOverride 确认请求里指定的 unit/source 被明确拒绝。
//
// 这里必须是 400 而不是“悄悄用绑定覆盖”：一个被忽略的参数会让人以为自己在看
// 另一个单元的日志。
func TestAppLogsRejectsObjectOverride(t *testing.T) {
	logs := &stubLogs{page: sampleLogPage(), paramsCh: make(chan journal.Params, 1)}
	f := appLogsFixture(t, nil, logs, nil)
	app := mustCreateSystemdApp(t, f, "nginx.service")

	for _, query := range []string{"?unit=smbd.service", "?source=kernel"} {
		rec := f.do(t, http.MethodGet, "/api/v1/apps/"+app.ID+"/logs"+query, "")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s 应被拒绝: %d %s", query, rec.Code, rec.Body.String())
		}
		if got := errorCode(t, rec); got != string(httpx.CodeValidationError) {
			t.Fatalf("%s 的错误码 = %q", query, got)
		}
	}
	if logs.params.Unit != "" {
		t.Fatalf("被拒绝的请求不该走到读取端: %+v", logs.params)
	}
}

// TestAppLogsSystemdRejectsContainerParams 确认 docker 的参数不会在 journal 上静默失效。
func TestAppLogsSystemdRejectsContainerParams(t *testing.T) {
	logs := &stubLogs{page: sampleLogPage(), paramsCh: make(chan journal.Params, 1)}
	f := appLogsFixture(t, nil, logs, nil)
	app := mustCreateSystemdApp(t, f, "nginx.service")

	rec := f.do(t, http.MethodGet, "/api/v1/apps/"+app.ID+"/logs?tail=20", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("tail 在 journal 上应被拒绝: %d %s", rec.Code, rec.Body.String())
	}
}

// TestAppLogsDockerUsesBinding 确认 docker 应用走容器日志读取端。
func TestAppLogsDockerUsesBinding(t *testing.T) {
	cn := newStubContainers()
	cn.page = containers.LogPage{Entries: []containers.LogEntry{{Message: "hello"}}}
	f := appLogsFixture(t, nil, nil, cn)
	app := mustCreateApp(t, f, map[string]any{
		"name": "示例容器", "type": "docker",
		"binding": map[string]any{"container_id": apiAppContainerID},
	})

	rec := f.do(t, http.MethodGet, "/api/v1/apps/"+app.ID+"/logs?tail=20", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("读应用日志失败: %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Source      string `json:"source"`
		ContainerID string `json:"container_id"`
	}
	if err := json.Unmarshal(parseResult(t, rec).Data, &out); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if out.Source != "docker" || out.ContainerID != apiAppContainerID {
		t.Fatalf("判别字段 = %q/%q", out.Source, out.ContainerID)
	}
	if len(cn.logParams) != 1 || cn.logParams[0].ContainerID != apiAppContainerID || cn.logParams[0].Tail != 20 {
		t.Fatalf("转发给容器读取端的参数 = %+v", cn.logParams)
	}
}

// TestAppLogsDockerRejectsJournalParams 确认 journal 的参数不会在 docker 上静默失效。
func TestAppLogsDockerRejectsJournalParams(t *testing.T) {
	cn := newStubContainers()
	f := appLogsFixture(t, nil, nil, cn)
	app := mustCreateApp(t, f, map[string]any{
		"name": "示例容器", "type": "docker",
		"binding": map[string]any{"container_id": apiAppContainerID},
	})
	rec := f.do(t, http.MethodGet, "/api/v1/apps/"+app.ID+"/logs?level=error", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("level 在 docker 上应被拒绝: %d %s", rec.Code, rec.Body.String())
	}
	if len(cn.logParams) != 0 {
		t.Fatalf("被拒绝的请求不该走到读取端: %+v", cn.logParams)
	}
}

// TestAppLogsDockerContainerGone 确认容器不存在时保留代理的 404，而不是变成 500。
func TestAppLogsDockerContainerGone(t *testing.T) {
	cn := newStubContainers()
	cn.logsErr = &containers.RejectedError{Status: http.StatusNotFound,
		Code: string(httpx.CodeContainerNotFound), Message: "容器不存在"}
	f := appLogsFixture(t, nil, nil, cn)
	app := mustCreateApp(t, f, map[string]any{
		"name": "示例容器", "type": "docker",
		"binding": map[string]any{"container_id": apiAppContainerID},
	})
	rec := f.do(t, http.MethodGet, "/api/v1/apps/"+app.ID+"/logs", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("应回 404: %d %s", rec.Code, rec.Body.String())
	}
	if got := errorCode(t, rec); got != string(httpx.CodeContainerNotFound) {
		t.Fatalf("错误码 = %q", got)
	}
}

// TestAppLogsLinkAndCompose 确认没有日志来源的类型不被说成“这里没有日志”。
func TestAppLogsLinkAndCompose(t *testing.T) {
	f := appLogsFixture(t, nil, &stubLogs{paramsCh: make(chan journal.Params, 1)}, newStubContainers())

	link := mustCreateApp(t, f, map[string]any{"name": "文档", "type": "link", "public_url": "https://docs.example.com"})
	rec := f.do(t, http.MethodGet, "/api/v1/apps/"+link.ID+"/logs", "")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("link 应回 422: %d %s", rec.Code, rec.Body.String())
	}

	compose := mustCreateApp(t, f, map[string]any{
		"name": "项目", "type": "compose",
		"binding": map[string]any{
			"deployment_id": "dep_0123456789abcdef0123456789abcdef",
		},
	})
	rec = f.do(t, http.MethodGet, "/api/v1/apps/"+compose.ID+"/logs", "")
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("compose 应回 501: %d %s", rec.Code, rec.Body.String())
	}
	if got := errorCode(t, rec); got != string(httpx.CodeNotImplemented) {
		t.Fatalf("错误码 = %q", got)
	}
}

// TestAppLogsRequiresAdmin 确认 viewer 拿不到日志（日志里什么都可能有）。
func TestAppLogsRequiresAdmin(t *testing.T) {
	f := appLogsFixture(t, nil, &stubLogs{paramsCh: make(chan journal.Params, 1)}, nil)
	app := mustCreateSystemdApp(t, f, "nginx.service")

	f.createUser(t, "log-watcher", "viewer", "watcher-horse-9")
	viewerCookie, viewerCSRF := loginAsUser(t, f.srv, "log-watcher", "watcher-horse-9")
	rec := f.doAs(t, viewerCookie, viewerCSRF, http.MethodGet, "/api/v1/apps/"+app.ID+"/logs", "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("viewer 应得到 403: %d %s", rec.Code, rec.Body.String())
	}
}

// errorCode 取出统一错误信封里的码。
func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	res := parseResult(t, rec)
	if res.Error == nil {
		t.Fatalf("响应里没有错误码: %s", rec.Body.String())
	}
	return res.Error.Code
}

// TestAppSummaryReflectsRuntimeFacts 确认 AppSummary 的摘要是观测结果。
func TestAppSummaryReflectsRuntimeFacts(t *testing.T) {
	unit := services.Unit{
		Unit: "nginx.service", LoadState: services.LoadLoaded,
		ActiveState: services.ActiveActive, SubState: "running",
		Enablement:   services.EnablementEnabled,
		Capabilities: []string{services.OpStop, services.OpRestart, services.OpDisable},
	}
	rt := &appruntime.Resolver{Services: stubAppServices{snap: services.Snapshot{
		Status: services.StatusOK, Units: []services.Unit{unit},
	}}}
	f := appLogsFixture(t, rt, nil, nil)
	app := mustCreateSystemdApp(t, f, "nginx.service")

	rec := f.do(t, http.MethodGet, "/api/v1/apps/"+app.ID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("读应用失败: %d %s", rec.Code, rec.Body.String())
	}
	var out appDTO
	if err := json.Unmarshal(parseResult(t, rec).Data, &out); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if out.RuntimeState != apps.RuntimeRunning {
		t.Fatalf("runtime_state = %q（note=%v）", out.RuntimeState, out.RuntimeNote)
	}
	if out.RuntimeNote != nil {
		t.Fatalf("running 且无异常时不该有 note: %v", *out.RuntimeNote)
	}
	if !out.Capabilities.Stop || !out.Capabilities.Restart || !out.Capabilities.Logs {
		t.Fatalf("能力 = %+v", out.Capabilities)
	}
	// active 的单元没有 start 能力：状态已经满足的操作不该出现（services.capabilityFor）。
	if out.Capabilities.Start {
		t.Fatalf("active 的单元不该有 start 能力: %+v", out.Capabilities)
	}
	// 没有依赖卷：保护不适用，且不该有理由。
	if out.DependencyProtection != apps.ProtectionNotApplicable || out.DependencyProtectionNote != nil {
		t.Fatalf("dependency_protection = %q / %v", out.DependencyProtection, out.DependencyProtectionNote)
	}
}

// TestAppSummaryRebindingHint 确认容器被重建后旧绑定要求重新确认。
func TestAppSummaryRebindingHint(t *testing.T) {
	rt := &appruntime.Resolver{Containers: stubAppContainers{resp: containers.ListResponse{
		Available: true, Containers: []containers.Container{},
	}}}
	f := appLogsFixture(t, rt, nil, nil)
	app := mustCreateApp(t, f, map[string]any{
		"name": "示例容器", "type": "docker",
		"binding": map[string]any{"container_id": apiAppContainerID},
	})
	rec := f.do(t, http.MethodGet, "/api/v1/apps/"+app.ID, "")
	var out appDTO
	if err := json.Unmarshal(parseResult(t, rec).Data, &out); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if out.RuntimeState != apps.RuntimeUnknown {
		t.Fatalf("runtime_state = %q", out.RuntimeState)
	}
	if out.RuntimeNote == nil || !containsAll(*out.RuntimeNote, "重新确认绑定") {
		t.Fatalf("note = %v", out.RuntimeNote)
	}
	if out.Capabilities.Start || out.Capabilities.Stop || out.Capabilities.Restart || out.Capabilities.Logs {
		t.Fatalf("旧绑定不该有 capability: %+v", out.Capabilities)
	}
}

// TestAppsListWithoutRuntimeSource 确认演示模式下列表仍可用，摘要诚实为 unknown。
func TestAppsListWithoutRuntimeSource(t *testing.T) {
	f := appLogsFixture(t, nil, nil, nil)
	app := mustCreateSystemdApp(t, f, "nginx.service")
	rec := f.do(t, http.MethodGet, "/api/v1/apps", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("列表失败: %d %s", rec.Code, rec.Body.String())
	}
	var out appListResponse
	if err := json.Unmarshal(parseResult(t, rec).Data, &out); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if len(out.Items) != 1 || out.Items[0].ID != app.ID {
		t.Fatalf("列表 = %+v", out.Items)
	}
	item := out.Items[0]
	if item.RuntimeState != apps.RuntimeUnknown {
		t.Fatalf("没有运行事实来源时 runtime_state = %q", item.RuntimeState)
	}
	if item.RuntimeNote == nil || !containsAll(*item.RuntimeNote, "未配置代理连接") {
		t.Fatalf("note = %v", item.RuntimeNote)
	}
	if item.Capabilities.Start || item.Capabilities.Logs {
		t.Fatalf("没有事实来源时不该有 capability: %+v", item.Capabilities)
	}
}

// containsAll 判断 s 是否包含全部片段（测试里只想表达“这些意思都在”）。
func containsAll(s string, parts ...string) bool {
	for _, p := range parts {
		if !strings.Contains(s, p) {
			return false
		}
	}
	return true
}
