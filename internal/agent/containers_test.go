package agent

// P09.3 的代理层单测：验证的是参数校验、预检映射与结果映射，
// 不验证 docker 的真实语义（那是 internal/containers 的测试与真机验收的事）。
//
// 这里最要紧的几条：
//   - 容器 ID 只认完整 64 位十六进制，名字/短 ID 在解码阶段就被拒（不进 argv）；
//   - 危险容器在**预检**里就是 allowed=false + CONTAINER_NOT_MANAGED，
//     不是“界面上不显示按钮”；
//   - 执行前复核状态版本，变了就是 409 PREFLIGHT_STALE；
//   - 退出码 0 但读回状态不对 → outcome=unknown，不是 verified。

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"upanel/internal/containers"
	"upanel/internal/httpx"
)

// sampleContainerID 是 64 位小写十六进制（形状与真实 ID 一致）。
const sampleContainerID = "3f4a1c2d3e5b6a7c8d9e0f1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e"

// samplePrivilegedID 是第二个容器（危险容器，用于“不接管”）。
const samplePrivilegedID = "9a8b7c6d5e4f3a2b1c0d9e8f7a6b5c4d3e2f1a0b9c8d7e6f5a4b3c2d1e0f9a8b"

// fakeContainers 是 ContainersSource 的假实现。
type fakeContainers struct {
	list    containers.ListResponse
	listErr error

	container containers.Container
	getErr    error
	gets      []string

	logsPage   containers.LogPage
	logsErr    error
	logsParams []containers.LogParams

	images   containers.ImagesResponse
	networks containers.NetworksResponse
	volumes  containers.VolumesResponse
	projects containers.ProjectsResponse
	otherErr error

	assessment containers.ControlAssessment
	assessErr  error
	assessed   []containers.ControlParams

	result     containers.ControlResult
	executeErr error
	executed   []containers.ControlParams
	progress   []string

	streamReason  string
	streamErr     error
	streamEntries []containers.LogEntry
	streamCalls   int
}

func (f *fakeContainers) List(context.Context) (containers.ListResponse, error) {
	return f.list, f.listErr
}

func (f *fakeContainers) Get(_ context.Context, id string) (containers.Container, error) {
	f.gets = append(f.gets, id)
	if f.getErr != nil {
		return containers.Container{}, f.getErr
	}
	return f.container, nil
}

func (f *fakeContainers) Logs(_ context.Context, params containers.LogParams) (containers.LogPage, error) {
	f.logsParams = append(f.logsParams, params)
	return f.logsPage, f.logsErr
}

func (f *fakeContainers) Images(context.Context) (containers.ImagesResponse, error) {
	return f.images, f.otherErr
}

func (f *fakeContainers) Networks(context.Context) (containers.NetworksResponse, error) {
	return f.networks, f.otherErr
}

func (f *fakeContainers) Volumes(context.Context) (containers.VolumesResponse, error) {
	return f.volumes, f.otherErr
}

func (f *fakeContainers) Projects(context.Context) (containers.ProjectsResponse, error) {
	return f.projects, f.otherErr
}

func (f *fakeContainers) Stream(_ context.Context, params containers.LogParams,
	_ containers.StreamOptions, emit func(containers.LogEntry) error) (string, error) {
	f.streamCalls++
	f.logsParams = append(f.logsParams, params)
	if f.streamErr != nil {
		return "", f.streamErr
	}
	for _, entry := range f.streamEntries {
		if err := emit(entry); err != nil {
			return containers.StreamEndClientClosed, nil
		}
	}
	return f.streamReason, nil
}

func (f *fakeContainers) AssessControl(_ context.Context, params containers.ControlParams) (containers.ControlAssessment, error) {
	f.assessed = append(f.assessed, params)
	return f.assessment, f.assessErr
}

func (f *fakeContainers) ExecuteControl(_ context.Context, params containers.ControlParams,
	before containers.Container, progress func(string, map[string]any)) (containers.ControlResult, error) {
	f.executed = append(f.executed, params)
	if progress != nil {
		progress("docker."+params.Operation, map[string]any{"container_id": "..."})
		f.progress = append(f.progress, "docker."+params.Operation)
	}
	if f.executeErr != nil {
		return containers.ControlResult{}, f.executeErr
	}
	result := f.result
	result.Before = before
	return result, nil
}

// runningContainer 是一个普通的、受管的运行中容器。
func runningContainer() containers.Container {
	c := containers.Container{
		ID:            sampleContainerID,
		Name:          "web",
		Image:         "nginx:1.27",
		State:         containers.StateRunning,
		StateText:     "Up 3 hours",
		StartedAt:     "2026-10-22T08:00:00.000000000Z",
		RestartPolicy: "unless-stopped",
		Ports:         []containers.Port{{HostIP: "127.0.0.1", HostPort: 8080, ContainerPort: 80, Protocol: "tcp"}},
		Mounts:        []containers.Mount{},
		Risk:          containers.Risk{Level: containers.RiskLow, Reasons: []containers.RiskReason{}},
	}
	c.Capabilities = c.ControlCapabilities()
	return c
}

// privilegedContainer 是危险容器：面板只显示，不接管。
func privilegedContainer() containers.Container {
	c := containers.Container{
		ID:        samplePrivilegedID,
		Name:      "danger",
		Image:     "alpine:3.20",
		State:     containers.StateRunning,
		StateText: "Up 1 hours",
		Risk: containers.Risk{
			Level: containers.RiskHigh,
			Reasons: []containers.RiskReason{{
				Code:    containers.RiskPrivileged,
				Message: "容器以 privileged 运行：拥有宿主全部设备的访问权限",
			}},
		},
	}
	c.Capabilities = c.ControlCapabilities()
	return c
}

// newFakeContainers 返回“默认一切就绪”的假来源：普通运行中容器 + 可执行的预检。
func newFakeContainers() *fakeContainers {
	normal := runningContainer()
	danger := privilegedContainer()
	return &fakeContainers{
		list: containers.ListResponse{
			Available:   true,
			Containers:  []containers.Container{normal, danger},
			CollectedAt: "2026-10-22T10:00:00Z",
		},
		container: normal,
		assessment: containers.ControlAssessment{
			Container:   normal,
			Allowed:     true,
			Expectation: containers.StateStopped,
		},
		result: containers.ControlResult{
			ContainerID: sampleContainerID,
			Operation:   containers.OpStop,
			Expectation: containers.StateStopped,
			Verified:    true,
			After:       containers.Container{ID: sampleContainerID, State: containers.StateStopped, StateText: "Exited (0)"},
			Before:      normal,
		},
		streamReason: containers.StreamEndIdleTimeout,
	}
}

// newContainersRegistry 只登记 P09.3 的查询与动作。
func newContainersRegistry(t *testing.T, src ContainersSource) *Registry {
	t.Helper()
	reg := NewRegistry()
	for _, q := range []Query{
		newContainersListQuery(src),
		newContainersGetQuery(src),
		newContainersLogsQuery(src),
		newContainersImagesQuery(src),
		newContainersNetworksQuery(src),
		newContainersVolumesQuery(src),
		newContainersProjectsQuery(src),
	} {
		if err := reg.RegisterQuery(q); err != nil {
			t.Fatalf("登记 %s 失败: %v", q.Name, err)
		}
	}
	if err := reg.RegisterAction(newContainerControlAction(src)); err != nil {
		t.Fatalf("登记 container.control 失败: %v", err)
	}
	return reg
}

// TestContainersListQueryReturnsItems 查 containers.list：原样返回，危险容器带风险。
func TestContainersListQueryReturnsItems(t *testing.T) {
	src := newFakeContainers()
	srv := newTestServerWithRegistry(t, newContainersRegistry(t, src))

	rec := postQuery(t, srv, `{"query":"containers.list"}`, testWebUID)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（%s）", rec.Code, rec.Body.String())
	}
	var body struct {
		Data containers.ListResponse `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if !body.Data.Available || len(body.Data.Containers) != 2 {
		t.Fatalf("清单内容不对: %+v", body.Data)
	}
	danger := body.Data.Containers[1]
	if danger.Risk.Level != containers.RiskHigh {
		t.Errorf("危险容器的 risk 应为 high，实际 %q", danger.Risk.Level)
	}
	if danger.Capabilities.Managed {
		t.Errorf("危险容器不应受管: %+v", danger.Capabilities)
	}
	if danger.Capabilities.Start || danger.Capabilities.Stop || danger.Capabilities.Restart {
		t.Errorf("危险容器的三项能力都应为 false: %+v", danger.Capabilities)
	}
	normal := body.Data.Containers[0]
	if !normal.Capabilities.Managed || !normal.Capabilities.Stop {
		t.Errorf("普通运行中容器应可停止: %+v", normal.Capabilities)
	}
}

// TestContainersListQueryRejectsParams 无参数查询不接受任何字段：
// 塞一个 {"command":"docker ps"} 不会被静默忽略。
func TestContainersListQueryRejectsParams(t *testing.T) {
	src := newFakeContainers()
	srv := newTestServerWithRegistry(t, newContainersRegistry(t, src))

	rec := postQuery(t, srv, `{"query":"containers.list","params":{"command":"docker rm -f web"}}`, testWebUID)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("带参数的状态码 = %d，期望 400（%s）", rec.Code, rec.Body.String())
	}
}

// TestContainersGetQueryRejectsNonID 名字与短 ID 都不接受：它们会变成 argv 的一部分。
func TestContainersGetQueryRejectsNonID(t *testing.T) {
	src := newFakeContainers()
	srv := newTestServerWithRegistry(t, newContainersRegistry(t, src))

	for _, id := range []string{
		"web",
		"3f4a1c2d3e5b", // 短 ID
		"3F4A1C2D3E5B6A7C8D9E0F1A2B3C4D5E6F708192A3B4C5D6E7F8091A2B3C4D5E", // 大写
		"3f4a1c2d3e5b6a7c8d9e0f1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5",  // 63 位
	} {
		body := `{"query":"containers.get","params":{"container_id":"` + id + `"}}`
		rec := postQuery(t, srv, body, testWebUID)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("container_id=%q 的状态码 = %d，期望 400（%s）", id, rec.Code, rec.Body.String())
		}
	}
	if len(src.gets) != 0 {
		t.Errorf("被拒的参数不应到达实现层，实际调用了 %v", src.gets)
	}
}

// TestContainersGetQueryNotFoundIs404 容器不存在是 404 CONTAINER_NOT_FOUND，
// 不是 500：前端据此提示“重新选择目标”，而不是“等一会儿再试”。
func TestContainersGetQueryNotFoundIs404(t *testing.T) {
	src := newFakeContainers()
	src.getErr = &containers.NotFoundError{ContainerID: sampleContainerID}
	srv := newTestServerWithRegistry(t, newContainersRegistry(t, src))

	body := `{"query":"containers.get","params":{"container_id":"` + sampleContainerID + `"}}`
	rec := postQuery(t, srv, body, testWebUID)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("状态码 = %d，期望 404（%s）", rec.Code, rec.Body.String())
	}
	if code := decodeErrorCode(t, rec.Body.Bytes()); code != "CONTAINER_NOT_FOUND" {
		t.Fatalf("错误码 = %q", code)
	}
}

// TestContainersLogsQueryValidatesTail 参数越界是 400，且不会调用实现层。
func TestContainersLogsQueryValidatesTail(t *testing.T) {
	src := newFakeContainers()
	srv := newTestServerWithRegistry(t, newContainersRegistry(t, src))

	body := `{"query":"containers.logs","params":{"container_id":"` + sampleContainerID + `","tail":1001}}`
	rec := postQuery(t, srv, body, testWebUID)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400（%s）", rec.Code, rec.Body.String())
	}
	if len(src.logsParams) != 0 {
		t.Errorf("被拒的 tail 不应到达实现层: %+v", src.logsParams)
	}
}

// TestContainersLogsQueryFillsDefaults 缺省 tail 由校验补成 DefaultLogLines。
func TestContainersLogsQueryFillsDefaults(t *testing.T) {
	src := newFakeContainers()
	src.logsPage = containers.LogPage{ContainerID: sampleContainerID, Entries: []containers.LogEntry{}}
	srv := newTestServerWithRegistry(t, newContainersRegistry(t, src))

	body := `{"query":"containers.logs","params":{"container_id":"` + sampleContainerID + `"}}`
	rec := postQuery(t, srv, body, testWebUID)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d（%s）", rec.Code, rec.Body.String())
	}
	if len(src.logsParams) != 1 || src.logsParams[0].Tail != containers.DefaultLogLines {
		t.Fatalf("落到的参数 = %+v", src.logsParams)
	}
}

// TestContainerControlPreflightAllowsNormal 普通运行中容器 stop：允许，目标只由 ID 定身份。
func TestContainerControlPreflightAllowsNormal(t *testing.T) {
	src := newFakeContainers()
	srv := newTestServerWithRegistry(t, newContainersRegistry(t, src))

	body := preflightBody("container.control",
		`{"container_id":"`+sampleContainerID+`","operation":"stop"}`)
	rec := postPreflight(t, srv, testWebUID, body)
	if rec.Code != http.StatusCreated && rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d（%s）", rec.Code, rec.Body.String())
	}
	got := decodePreflight(t, rec)
	if !got.Allowed {
		t.Fatalf("应允许 stop: %+v", got.Blockers)
	}
	if got.Target.IdentityFingerprint != "container:"+sampleContainerID {
		t.Errorf("身份指纹 = %q，期望只由容器 ID 决定", got.Target.IdentityFingerprint)
	}
	if got.Target.StateVersion == "" {
		t.Error("状态版本不能为空：执行前的复核靠它")
	}
	if len(got.Locks) != 1 || got.Locks[0] != "container:"+sampleContainerID {
		t.Errorf("锁 = %v", got.Locks)
	}
	if got.RequiresReauth || got.RequiresPhrase {
		t.Errorf("启停不应要求再认证或短语: %+v", got)
	}
}

// TestContainerControlPreflightRefusesPrivileged 危险容器：allowed=false，
// 阻塞项带 CONTAINER_NOT_MANAGED。这是“不接管”的落点，不在界面上。
func TestContainerControlPreflightRefusesPrivileged(t *testing.T) {
	src := newFakeContainers()
	danger := privilegedContainer()
	src.assessment = containers.ControlAssessment{
		Container: danger,
		Allowed:   false,
		Blockers: []containers.Block{
			{Code: containers.BlockUnmanaged, Message: "面板不接管这个容器（danger）：它拥有宿主级的权限"},
			{Code: containers.BlockUnmanaged, Message: "容器以 privileged 运行：拥有宿主全部设备的访问权限"},
		},
	}
	srv := newTestServerWithRegistry(t, newContainersRegistry(t, src))

	body := preflightBody("container.control",
		`{"container_id":"`+samplePrivilegedID+`","operation":"stop"}`)
	rec := postPreflight(t, srv, testWebUID, body)
	got := decodePreflight(t, rec)
	if got.Allowed {
		t.Fatal("危险容器不应被允许执行任何写操作")
	}
	if len(got.Blockers) == 0 {
		t.Fatal("不允许时必须给出阻塞项")
	}
	found := false
	for _, b := range got.Blockers {
		if b.Code == containers.BlockUnmanaged {
			found = true
		}
	}
	if !found {
		t.Fatalf("阻塞项里应包含 %s: %+v", containers.BlockUnmanaged, got.Blockers)
	}
}

// TestContainerControlDecodeRejects 参数形状不对一律 400：名字、未知操作、多余字段。
func TestContainerControlDecodeRejects(t *testing.T) {
	src := newFakeContainers()
	srv := newTestServerWithRegistry(t, newContainersRegistry(t, src))

	for _, params := range []string{
		`{"container_id":"web","operation":"stop"}`,
		`{"container_id":"` + sampleContainerID + `","operation":"rm"}`,
		`{"container_id":"` + sampleContainerID + `","operation":"stop","force":true}`,
	} {
		rec := postPreflight(t, srv, testWebUID, preflightBody("container.control", params))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("params=%s 的状态码 = %d，期望 400（%s）", params, rec.Code, rec.Body.String())
		}
	}
	if len(src.assessed) != 0 {
		t.Errorf("被拒的参数不应进入预检实现: %+v", src.assessed)
	}
}

// TestContainerControlExecuteVerifies 执行成功：读回状态一致 → verified + 前后状态。
func TestContainerControlExecuteVerifies(t *testing.T) {
	src := newFakeContainers()
	srv := newTestServerWithRegistry(t, newContainersRegistry(t, src))

	action, ok := srv.opts.Registry.Action(containers.ActionControl)
	if !ok {
		t.Fatal("container.control 没登记")
	}
	params := containers.ControlParams{ContainerID: sampleContainerID, Operation: containers.OpStop}
	target := Target{
		DisplayName:         "web",
		IdentityFingerprint: "container:" + sampleContainerID,
		StateVersion:        containers.StateVersion(src.container),
	}
	var steps []string
	result, err := action.Execute(t.Context(), ExecuteInput{
		Params:   params,
		Target:   target,
		Progress: func(step string, _ map[string]any) { steps = append(steps, step) },
	})
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if result.Outcome != OutcomeVerified {
		t.Fatalf("outcome = %q，期望 verified", result.Outcome)
	}
	if result.VerifiedAt.IsZero() {
		t.Error("verified 必须带核验时间")
	}
	if state, _ := result.ObservedState["state"].(string); state != "stopped" {
		t.Errorf("读回状态 = %v", result.ObservedState["state"])
	}
	if prev, _ := result.ObservedState["previous_state"].(string); prev != "running" {
		t.Errorf("操作前状态 = %v", result.ObservedState["previous_state"])
	}
	if verified, _ := result.ObservedState["verified"].(bool); !verified {
		t.Error("observed_state.verified 应为 true")
	}
	if len(src.executed) != 1 || src.executed[0].Operation != containers.OpStop {
		t.Fatalf("执行的参数 = %+v", src.executed)
	}
	if len(steps) == 0 {
		t.Error("应有进度回报")
	}
}

// TestContainerControlExecuteStaleState 预检之后状态变了 → 409 PREFLIGHT_STALE，
// 而且**不执行**任何 docker 命令。
func TestContainerControlExecuteStaleState(t *testing.T) {
	src := newFakeContainers()
	srv := newTestServerWithRegistry(t, newContainersRegistry(t, src))

	action, ok := srv.opts.Registry.Action(containers.ActionControl)
	if !ok {
		t.Fatal("container.control 没登记")
	}
	params := containers.ControlParams{ContainerID: sampleContainerID, Operation: containers.OpStop}
	_, err := action.Execute(t.Context(), ExecuteInput{
		Params: params,
		Target: Target{
			IdentityFingerprint: "container:" + sampleContainerID,
			StateVersion:        "stale-version",
		},
	})
	if err == nil {
		t.Fatal("状态版本不一致时应拒绝执行")
	}
	if !errors.Is(err, containers.ErrStateChanged) {
		t.Fatalf("错误 = %v，期望 ErrStateChanged", err)
	}
	var carrier interface{ HTTPStatus() (int, httpx.Code) }
	if !errors.As(err, &carrier) {
		t.Fatalf("错误应带 HTTP 状态: %v", err)
	}
	if status, code := carrier.HTTPStatus(); status != http.StatusConflict || string(code) != "PREFLIGHT_STALE" {
		t.Fatalf("状态码 = %d/%s", status, code)
	}
	if len(src.executed) != 0 {
		t.Errorf("过期预检不应执行命令: %+v", src.executed)
	}
}

// TestContainerControlExecuteRefusesWhenRiskAppears 执行前复核发现不再允许 → 拒绝。
func TestContainerControlExecuteRefusesWhenRiskAppears(t *testing.T) {
	src := newFakeContainers()
	src.assessment = containers.ControlAssessment{
		Container: runningContainer(),
		Allowed:   false,
		Blockers:  []containers.Block{{Code: containers.BlockUnmanaged, Message: "面板不接管这个容器"}},
	}
	srv := newTestServerWithRegistry(t, newContainersRegistry(t, src))

	action, _ := srv.opts.Registry.Action(containers.ActionControl)
	params := containers.ControlParams{ContainerID: sampleContainerID, Operation: containers.OpStop}
	_, err := action.Execute(t.Context(), ExecuteInput{
		Params: params,
		Target: Target{
			IdentityFingerprint: "container:" + sampleContainerID,
			StateVersion:        containers.StateVersion(src.container),
		},
	})
	if err == nil {
		t.Fatal("预检复审不允许时应拒绝执行")
	}
	if !errors.Is(err, containers.ErrStateChanged) {
		t.Fatalf("错误 = %v，期望 ErrStateChanged", err)
	}
	if len(src.executed) != 0 {
		t.Errorf("拒绝时不应执行命令: %+v", src.executed)
	}
}

// TestContainerControlExecuteUnverifiedIsUnknown 退出码 0 但读回状态不对 → unknown。
func TestContainerControlExecuteUnverifiedIsUnknown(t *testing.T) {
	src := newFakeContainers()
	src.result = containers.ControlResult{
		ContainerID: sampleContainerID,
		Operation:   containers.OpStop,
		Expectation: containers.StateStopped,
		Verified:    false,
		After:       containers.Container{ID: sampleContainerID, State: containers.StateRunning, StateText: "Up"},
		VerifyNote:  "15 秒内状态没有变成 stopped，读回的是 running",
	}
	srv := newTestServerWithRegistry(t, newContainersRegistry(t, src))

	action, _ := srv.opts.Registry.Action(containers.ActionControl)
	result, err := action.Execute(t.Context(), ExecuteInput{
		Params: containers.ControlParams{ContainerID: sampleContainerID, Operation: containers.OpStop},
		Target: Target{
			IdentityFingerprint: "container:" + sampleContainerID,
			StateVersion:        containers.StateVersion(src.container),
		},
	})
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if result.Outcome != OutcomeUnknown {
		t.Fatalf("outcome = %q，期望 unknown（退出码 0 不等于成功）", result.Outcome)
	}
	if strings.TrimSpace(result.RecoveryHint) == "" {
		t.Error("unknown 必须给恢复提示")
	}
}

// TestContainerStreamPurposeRejectedWithoutSource 没有容器读取端时，
// 容器日志流用途不在白名单里（501），不会先建一张永远跑不起来的许可。
func TestContainerStreamPurposeRejectedWithoutSource(t *testing.T) {
	srv := newTestServer(t) // 生产登记表，没有 Containers
	body := `{"purpose":"` + StreamPurposeContainerLogs + `","params":{"container_id":"` +
		sampleContainerID + `"}}`
	rec := postJSON(t, srv, "/internal/v1/streams", body)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("状态码 = %d，期望 501（%s）", rec.Code, rec.Body.String())
	}
}

// TestContainerControlPreflightDockerUnavailableIsBlocked Docker 不可用时预检仍是 200：
// allowed=false + DOCKER_UNAVAILABLE，而不是 500“预检计算失败”。
// 这条路径在验收 VM 上真的会走到（把 dockerd 停掉），所以它必须在单元测试里也钉住。
func TestContainerControlPreflightDockerUnavailableIsBlocked(t *testing.T) {
	src := newFakeContainers()
	src.assessment = containers.ControlAssessment{
		Allowed: false,
		Blockers: []containers.Block{
			{Code: containers.BlockUnavailable, Message: "Docker 守护进程没有运行（套接字不存在），容器功能不可用"},
		},
	}
	srv := newTestServerWithRegistry(t, newContainersRegistry(t, src))

	body := preflightBody("container.control",
		`{"container_id":"`+sampleContainerID+`","operation":"stop"}`)
	rec := postPreflight(t, srv, testWebUID, body)
	if rec.Code != http.StatusCreated && rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200/201（%s）", rec.Code, rec.Body.String())
	}
	got := decodePreflight(t, rec)
	if got.Allowed {
		t.Fatal("Docker 不可用时预检不该允许执行")
	}
	if len(got.Blockers) != 1 || got.Blockers[0].Code != containers.BlockUnavailable {
		t.Fatalf("阻塞项 = %+v，期望一条 %s", got.Blockers, containers.BlockUnavailable)
	}
	if got.Target.StateVersion != "" {
		t.Error("读不到容器时不该有状态版本：那会让执行前的复核看起来有依据")
	}
}
