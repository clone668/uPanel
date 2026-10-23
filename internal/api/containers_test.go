package api

// P09.3 接口层用例：容器、镜像、网络、卷、部署的只读视图与容器日志流。
//
// 这里验的是 HTTP 层的三件事，不冒充 docker：
//   - 权限：整组 admin（容器清单等于这台机器跑了什么，属于侦察情报）；
//   - 参数：容器 ID 必须是完整 64 位十六进制，尾数越界是 400；
//   - 错误映射：代理拒绝（404/422）如实带出，代理不可用是 503，
//     别的错误才是 500——把“容器不存在”压成 500 会让人去查错方向。

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
	"upanel/internal/containers"
	"upanel/internal/platform"
	"upanel/internal/version"
)

// stubContainers 是 ContainersReader 的测试替身。
type stubContainers struct {
	list    containers.ListResponse
	listErr error

	container containers.Container
	getErr    error
	gotIDs    []string

	page      containers.LogPage
	logsErr   error
	logParams []containers.LogParams

	images   containers.ImagesResponse
	networks containers.NetworksResponse
	volumes  containers.VolumesResponse
	projects containers.ProjectsResponse
	otherErr error

	entries      []containers.LogEntry
	reason       string
	streamErr    error
	streamParams containers.LogParams
}

func newStubContainers() *stubContainers {
	return &stubContainers{
		list: containers.ListResponse{
			Available:   true,
			Containers:  []containers.Container{sampleAPIContainer()},
			CollectedAt: "2026-10-22T10:00:00Z",
		},
		container: sampleAPIContainer(),
		page: containers.LogPage{
			ContainerID: stubContainerID,
			Entries:     []containers.LogEntry{},
		},
		projects: containers.ProjectsResponse{
			Available: true,
			Projects: []containers.Project{{
				ID:           "dep_abcd",
				Name:         "stack",
				Services:     []string{"web"},
				ContainerIDs: []string{stubContainerID},
				Running:      1,
				Total:        1,
				Capabilities: containers.ProjectCapabilities{},
			}},
		},
		reason: containers.StreamEndIdleTimeout,
	}
}

// stubContainerID / stubDangerID 是 64 位小写十六进制（形状与真实 ID 一致）。
const (
	stubContainerID = "3f4a1c2d3e5b6a7c8d9e0f1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e"
	stubDangerID    = "9a8b7c6d5e4f3a2b1c0d9e8f7a6b5c4d3e2f1a0b9c8d7e6f5a4b3c2d1e0f9a8b"
)

// sampleAPIContainer 是一个普通运行中容器。
func sampleAPIContainer() containers.Container {
	c := containers.Container{
		ID:            stubContainerID,
		Name:          "web",
		Image:         "nginx:1.27",
		State:         containers.StateRunning,
		StateText:     "Up 3 hours",
		StartedAt:     "2026-10-22T08:00:00.000000000Z",
		RestartPolicy: "unless-stopped",
		Ports:         []containers.Port{},
		Mounts:        []containers.Mount{},
		Risk:          containers.Risk{Level: containers.RiskLow, Reasons: []containers.RiskReason{}},
	}
	c.Capabilities = c.ControlCapabilities()
	return c
}

func (s *stubContainers) List(context.Context) (containers.ListResponse, error) {
	return s.list, s.listErr
}

func (s *stubContainers) Get(_ context.Context, id string) (containers.Container, error) {
	s.gotIDs = append(s.gotIDs, id)
	if s.getErr != nil {
		return containers.Container{}, s.getErr
	}
	return s.container, nil
}

func (s *stubContainers) Logs(_ context.Context, params containers.LogParams) (containers.LogPage, error) {
	s.logParams = append(s.logParams, params)
	if s.logsErr != nil {
		return containers.LogPage{}, s.logsErr
	}
	return s.page, nil
}

func (s *stubContainers) Images(context.Context) (containers.ImagesResponse, error) {
	return s.images, s.otherErr
}

func (s *stubContainers) Networks(context.Context) (containers.NetworksResponse, error) {
	return s.networks, s.otherErr
}

func (s *stubContainers) Volumes(context.Context) (containers.VolumesResponse, error) {
	return s.volumes, s.otherErr
}

func (s *stubContainers) Projects(context.Context) (containers.ProjectsResponse, error) {
	return s.projects, s.otherErr
}

func (s *stubContainers) StreamWithReason(ctx context.Context, params containers.LogParams,
	emit func(containers.LogEntry) error) (string, error) {
	s.streamParams = params
	if s.streamErr != nil {
		return "", s.streamErr
	}
	for _, entry := range s.entries {
		if err := emit(entry); err != nil {
			return containers.StreamEndClientClosed, err
		}
	}
	if err := ctx.Err(); err != nil {
		return containers.StreamEndClientClosed, err
	}
	return s.reason, nil
}

func containersFixture(t *testing.T, reader ContainersReader) (*Server, *http.Cookie) {
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
		Containers:        reader,
	})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	seedTestUser(t, srv.opts.Auth, testUser, testUserPass)
	cookie, _ := loginOK(t, srv)
	return srv, cookie
}

// TestContainersRequiresSession 未登录一律 401。
func TestContainersRequiresSession(t *testing.T) {
	srv, _ := containersFixture(t, newStubContainers())
	for _, path := range []string{
		"/api/v1/containers",
		"/api/v1/images",
		"/api/v1/container-networks",
		"/api/v1/container-volumes",
		"/api/v1/deployments",
	} {
		rec := adminGet(t, srv, nil, path)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s 状态码 = %d，期望 401（%s）", path, rec.Code, rec.Body.String())
		}
	}
}

// TestContainersForbiddenForViewer 整组 admin：viewer 连流也一起被挡。
func TestContainersForbiddenForViewer(t *testing.T) {
	srv, _ := containersFixture(t, newStubContainers())
	if _, err := srv.opts.Auth.CreateUser(context.Background(), auth.CreateUserInput{
		Username: "watcher", DisplayName: "watcher", Role: auth.RoleViewer, Password: "watcher-horse-9",
	}); err != nil {
		t.Fatalf("创建 viewer 失败: %v", err)
	}
	cookie, _ := loginAsUser(t, srv, "watcher", "watcher-horse-9")
	for _, path := range []string{
		"/api/v1/containers",
		"/api/v1/images",
		"/api/v1/container-networks",
		"/api/v1/container-volumes",
		"/api/v1/deployments",
	} {
		rec := adminGet(t, srv, cookie, path)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s viewer 状态码 = %d，期望 403（%s）", path, rec.Code, rec.Body.String())
		}
	}
}

// TestContainersListReturnsRiskAndCapabilities 危险容器的 risk 与 capabilities
// 原样透出：界面据此显示“不接管”，而不是自己再算一遍。
func TestContainersListReturnsRiskAndCapabilities(t *testing.T) {
	src := newStubContainers()
	danger := containers.Container{
		ID:    stubDangerID,
		Name:  "danger",
		Image: "alpine:3.20",
		State: containers.StateRunning,
		Risk: containers.Risk{
			Level: containers.RiskHigh,
			Reasons: []containers.RiskReason{{
				Code:    containers.RiskDockerSock,
				Message: "容器挂载了 docker.sock：容器内的进程可以直接控制宿主的 Docker",
			}},
		},
		Capabilities: containers.Capabilities{},
	}
	src.list.Containers = append(src.list.Containers, danger)
	srv, cookie := containersFixture(t, src)

	rec := adminGet(t, srv, cookie, "/api/v1/containers")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d（%s）", rec.Code, rec.Body.String())
	}
	var body struct {
		Data containers.ListResponse `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if len(body.Data.Containers) != 2 {
		t.Fatalf("容器数量 = %d", len(body.Data.Containers))
	}
	got := body.Data.Containers[1]
	if got.Risk.Level != containers.RiskHigh || len(got.Risk.Reasons) != 1 {
		t.Fatalf("风险信息丢失: %+v", got.Risk)
	}
	if got.Capabilities.Managed {
		t.Errorf("危险容器不应受管: %+v", got.Capabilities)
	}
}

// TestContainerDetailRejectsBadID 名字与短 ID 在进代理之前就是 400。
func TestContainerDetailRejectsBadID(t *testing.T) {
	src := newStubContainers()
	srv, cookie := containersFixture(t, src)

	for _, id := range []string{"web", "3f4a1c2d3e5b"} {
		rec := adminGet(t, srv, cookie, "/api/v1/containers/"+id)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s 状态码 = %d，期望 400（%s）", id, rec.Code, rec.Body.String())
		}
	}
	if len(src.gotIDs) != 0 {
		t.Errorf("被拒的 ID 不应到达读取端: %v", src.gotIDs)
	}
}

// TestContainerDetailNotFound 代理说没有 → 404 CONTAINER_NOT_FOUND。
func TestContainerDetailNotFound(t *testing.T) {
	src := newStubContainers()
	src.getErr = &containers.RejectedError{
		Status: http.StatusNotFound, Code: "CONTAINER_NOT_FOUND", Message: "容器不在本机",
	}
	srv, cookie := containersFixture(t, src)

	rec := adminGet(t, srv, cookie, "/api/v1/containers/"+stubContainerID)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("状态码 = %d，期望 404（%s）", rec.Code, rec.Body.String())
	}
	if got := parseResult(t, rec).Error; got == nil || got.Code != "CONTAINER_NOT_FOUND" {
		t.Fatalf("错误码 = %+v", got)
	}
}

// TestContainerDetailAgentUnavailable 代理不可用 → 503 AGENT_UNAVAILABLE。
func TestContainerDetailAgentUnavailable(t *testing.T) {
	src := newStubContainers()
	src.listErr = containers.ErrAgentUnavailable
	srv, cookie := containersFixture(t, src)

	rec := adminGet(t, srv, cookie, "/api/v1/containers")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("状态码 = %d，期望 503（%s）", rec.Code, rec.Body.String())
	}
	if got := parseResult(t, rec).Error; got == nil || got.Code != "AGENT_UNAVAILABLE" {
		t.Fatalf("错误码 = %+v", got)
	}
}

// TestContainerDetailDockerUnavailable 代理说“Docker 不可用” → 503 DOCKER_UNAVAILABLE。
//
// 与上一条隔开：两种 503 的处置完全不同（一个是“等代理”，一个是“去起 dockerd”），
// 所以代理带回来的域内码必须在 Web 这一层原样透出，不能因为状态码是 5xx 就改写。
func TestContainerDetailDockerUnavailable(t *testing.T) {
	src := newStubContainers()
	src.getErr = &containers.RejectedError{
		Status:  http.StatusServiceUnavailable,
		Code:    "DOCKER_UNAVAILABLE",
		Message: "Docker 守护进程没有应答（套接字在，但连不上）",
	}
	srv, cookie := containersFixture(t, src)

	rec := adminGet(t, srv, cookie, "/api/v1/containers/"+stubContainerID)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("状态码 = %d，期望 503（%s）", rec.Code, rec.Body.String())
	}
	if got := parseResult(t, rec).Error; got == nil || got.Code != "DOCKER_UNAVAILABLE" {
		t.Fatalf("错误码 = %+v，期望 DOCKER_UNAVAILABLE", got)
	}
}

// TestContainerLogsRejectsBadParams tail 越界是 400，且不会调用读取端。
func TestContainerLogsRejectsBadParams(t *testing.T) {
	src := newStubContainers()
	srv, cookie := containersFixture(t, src)

	for _, q := range []string{"?tail=0", "?tail=1001", "?tail=abc"} {
		rec := adminGet(t, srv, cookie, "/api/v1/containers/"+stubContainerID+"/logs"+q)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s 状态码 = %d，期望 400（%s）", q, rec.Code, rec.Body.String())
		}
	}
	if len(src.logParams) != 0 {
		t.Errorf("被拒的参数不应到达读取端: %+v", src.logParams)
	}
}

// TestContainerLogsUnsupportedDriver 日志驱动不支持读取 → 422（代理判决透出）。
func TestContainerLogsUnsupportedDriver(t *testing.T) {
	src := newStubContainers()
	src.logsErr = &containers.RejectedError{
		Status:  http.StatusUnprocessableEntity,
		Code:    "VALIDATION_ERROR",
		Message: "容器的日志驱动不支持读取",
	}
	srv, cookie := containersFixture(t, src)

	rec := adminGet(t, srv, cookie, "/api/v1/containers/"+stubContainerID+"/logs")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("状态码 = %d，期望 422（%s）", rec.Code, rec.Body.String())
	}
}

// TestContainerLogsStreamSSE 实时流：entry 帧带时间戳 id，结束帧带原因。
func TestContainerLogsStreamSSE(t *testing.T) {
	src := newStubContainers()
	entry := containers.LogEntry{
		Timestamp: time.Date(2026, 10, 22, 10, 0, 1, 0, time.UTC),
		Message:   "listening on :8080",
	}
	src.entries = []containers.LogEntry{entry}
	src.reason = containers.StreamEndIdleTimeout
	srv, cookie := containersFixture(t, src)

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	req, err := http.NewRequest(http.MethodGet,
		ts.URL+"/api/v1/containers/"+stubContainerID+"/logs/stream", nil)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	req.Header.Set("Cookie", cookie.String())
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q", ct)
	}

	buf := make([]byte, 4096)
	var sb strings.Builder
	for sb.Len() < 4096 {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			sb.Write(buf[:n])
		}
		if err != nil || strings.Contains(sb.String(), "event: end") {
			break
		}
	}
	got := sb.String()
	if !strings.Contains(got, "event: entry") || !strings.Contains(got, "listening on :8080") {
		t.Fatalf("流里没有 entry 帧: %q", got)
	}
	if !strings.Contains(got, "id: 2026-10-22T10:00:01.000000000Z") {
		t.Fatalf("entry 帧应带时间戳 id（用于 Last-Event-ID 续传）: %q", got)
	}
	if !strings.Contains(got, `"reason":"`+containers.StreamEndIdleTimeout+`"`) {
		t.Fatalf("结束帧应带结束原因: %q", got)
	}
	if src.streamParams.ContainerID != stubContainerID {
		t.Fatalf("落到读取端的参数 = %+v", src.streamParams)
	}
}

// TestContainerLogsStreamInterrupted 流在没有结束标记的情况下断了 → error 帧。
func TestContainerLogsStreamInterrupted(t *testing.T) {
	src := newStubContainers()
	src.streamErr = errors.New("containers: 流中断")
	srv, cookie := containersFixture(t, src)

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodGet,
		ts.URL+"/api/v1/containers/"+stubContainerID+"/logs/stream", nil)
	req.Header.Set("Cookie", cookie.String())
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	buf := make([]byte, 4096)
	var sb strings.Builder
	for sb.Len() < 4096 {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			sb.Write(buf[:n])
		}
		if err != nil || strings.Contains(sb.String(), "event: error") {
			break
		}
	}
	if !strings.Contains(sb.String(), "event: error") {
		t.Fatalf("中断应写成 error 帧: %q", sb.String())
	}
}

// TestDeploymentDetail 部署详情：项目 + 它下面的容器。
func TestDeploymentDetail(t *testing.T) {
	srv, cookie := containersFixture(t, newStubContainers())

	rec := adminGet(t, srv, cookie, "/api/v1/deployments/dep_abcd")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d（%s）", rec.Code, rec.Body.String())
	}
	var body struct {
		Data deploymentDetail `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if body.Data.Name != "stack" {
		t.Fatalf("项目名 = %q", body.Data.Name)
	}
	if len(body.Data.Containers) != 1 || body.Data.Containers[0].ID != stubContainerID {
		t.Fatalf("项目下的容器 = %+v", body.Data.Containers)
	}
	if body.Data.Capabilities.Update {
		t.Error("0.1 不具备 Compose 整体更新能力，capabilities.update 必须为 false")
	}
}

// TestDeploymentDetailNotFound 未知部署 → 404（不按名字模糊匹配）。
func TestDeploymentDetailNotFound(t *testing.T) {
	srv, cookie := containersFixture(t, newStubContainers())
	rec := adminGet(t, srv, cookie, "/api/v1/deployments/dep_nope")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("状态码 = %d，期望 404（%s）", rec.Code, rec.Body.String())
	}
}
