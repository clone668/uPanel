package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"upanel/internal/auth"
	"upanel/internal/certificates"
	"upanel/internal/config"
	"upanel/internal/platform"
	"upanel/internal/processes"
	"upanel/internal/version"
)

// stubProcesses 是 ProcessesReader 的测试替身：只验证 HTTP 层映射
// （权限、状态码、错误码、空列表归一化）。/proc 解析与信号语义由
// internal/processes 的测试覆盖，这里不冒充真实采集。
type stubProcesses struct {
	snap    processes.Snapshot
	listErr error
}

func (s *stubProcesses) Snapshot(context.Context) (processes.Snapshot, error) {
	if s.listErr != nil {
		return processes.Snapshot{}, s.listErr
	}
	return s.snap, nil
}

func processesFixture(t *testing.T, reader ProcessesReader) (*Server, *http.Cookie) {
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
		Processes:         reader,
	})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	seedTestUser(t, srv.opts.Auth, testUser, testUserPass)
	cookie, _ := loginOK(t, srv)
	return srv, cookie
}

// sampleProcessEntry 是一份真实的进程视图（数值来自 24.04 测试机，见
// internal/processes/testdata/sleep.*）。这里只用于 HTTP 层断言。
func sampleProcessEntry() processes.Process {
	cpu := 0.4
	mem := "2015232"
	return processes.Process{
		PID:            39515,
		StartTimeTicks: 277319,
		PPID:           39443,
		Owner:          "upanel",
		State:          processes.StateSleeping,
		CPUPercent:     &cpu,
		MemoryBytes:    &mem,
		Command:        "sleep 15",
		Unit:           "session-705.scope",
		Protection:     processes.Protection{Reasons: []string{}},
		Capabilities:   []string{processes.CapTERM, processes.CapKILL},
	}
}

func sampleProcessSnapshot() processes.Snapshot {
	protected := processes.Process{
		PID:            1,
		StartTimeTicks: 96,
		PPID:           0,
		Owner:          "root",
		State:          processes.StateSleeping,
		Command:        "/sbin/init",
		Unit:           "init.scope",
		Protection: processes.Protection{
			Protected: true,
			Category:  processes.ProtectInit,
			Reasons:   []string{"PID 1 是 init（在 Ubuntu 上是 systemd）"},
		},
		Capabilities: []string{},
	}
	return processes.Snapshot{
		Status:      processes.StatusOK,
		CollectedAt: time.Date(2026, 10, 21, 8, 0, 0, 0, time.UTC),
		IntervalMS:  5000,
		Total:       2,
		Processes:   []processes.Process{sampleProcessEntry(), protected},
	}
}

// TestProcessesRouteRequiresSession 未登录一律 401。
func TestProcessesRouteRequiresSession(t *testing.T) {
	srv, _ := processesFixture(t, &stubProcesses{snap: sampleProcessSnapshot()})
	rec := servicesGet(t, srv, nil, "/api/v1/processes")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("未登录状态码 = %d，期望 401", rec.Code)
	}
	got := parseResult(t, rec)
	if got.Error == nil || got.Error.Code != "UNAUTHENTICATED" {
		t.Fatalf("错误码 = %+v", got.Error)
	}
}

// TestProcessesRouteForbiddenForViewer 进程列表是 admin（CONTRACTS.md 第 10 节）：
// 命令摘要里有主机内部信息，不对 viewer 开放。
func TestProcessesRouteForbiddenForViewer(t *testing.T) {
	srv, _ := processesFixture(t, &stubProcesses{snap: sampleProcessSnapshot()})
	if _, err := srv.opts.Auth.CreateUser(context.Background(), auth.CreateUserInput{
		Username: "alice", DisplayName: "alice", Role: auth.RoleViewer, Password: "alice-horse-9",
	}); err != nil {
		t.Fatalf("建 viewer 失败: %v", err)
	}
	viewerCookie, _ := loginAsUser(t, srv, "alice", "alice-horse-9")
	rec := servicesGet(t, srv, viewerCookie, "/api/v1/processes")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("viewer 状态码 = %d，期望 403（%s）", rec.Code, rec.Body.String())
	}
	got := parseResult(t, rec)
	if got.Error == nil || got.Error.Code != "FORBIDDEN" {
		t.Fatalf("错误码 = %+v", got.Error)
	}
}

// TestProcessListPassesThrough 列表原样透传采集结果。
func TestProcessListPassesThrough(t *testing.T) {
	srv, cookie := processesFixture(t, &stubProcesses{snap: sampleProcessSnapshot()})
	rec := servicesGet(t, srv, cookie, "/api/v1/processes")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d（%s）", rec.Code, rec.Body.String())
	}
	var body struct {
		Data processListResponse `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	d := body.Data
	if d.Status != processes.StatusOK || d.IntervalMS != 5000 || d.Total != 2 || d.Truncated {
		t.Fatalf("外层字段 = %+v", d)
	}
	if len(d.Processes) != 2 {
		t.Fatalf("进程数 = %d", len(d.Processes))
	}
	p := d.Processes[0]
	if p.PID != 39515 || p.StartTimeTicks != 277319 {
		t.Errorf("身份字段 = %+v", p)
	}
	// CPU 是数字（可为 null），内存是十进制字符串，时间是可解析的 RFC3339。
	if !strings.Contains(rec.Body.String(), `"cpu_percent":0.4`) {
		t.Errorf("cpu_percent 形态不对: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"memory_bytes":"2015232"`) {
		t.Errorf("memory_bytes 不是十进制字符串: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"collected_at":"2026-10-21T08:00:00Z"`) {
		t.Errorf("collected_at 格式不对: %s", rec.Body.String())
	}
	// 受保护进程：能力是空数组、保护原因可见。
	protected := d.Processes[1]
	if !protected.Protection.Protected || protected.Protection.Category != processes.ProtectInit {
		t.Errorf("保护字段 = %+v", protected.Protection)
	}
	if len(protected.Capabilities) != 0 {
		t.Errorf("受保护进程不该有能力: %v", protected.Capabilities)
	}
	if !strings.Contains(rec.Body.String(), `"capabilities":[]`) {
		t.Errorf("空能力必须是 [] 而不是 null: %s", rec.Body.String())
	}
}

// TestProcessListNilNormalized 采集失败时 processes 必须是 []，不是 null。
func TestProcessListNilNormalized(t *testing.T) {
	srv, cookie := processesFixture(t, &stubProcesses{snap: processes.Snapshot{
		Status: processes.StatusError, Error: "进程状态尚未完成第一次采集",
		IntervalMS: 5000, Processes: nil,
	}})
	rec := servicesGet(t, srv, cookie, "/api/v1/processes")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d（%s）", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"processes":[]`) {
		t.Errorf("processes 应归一化为空数组: %s", rec.Body.String())
	}
	var body struct {
		Data processListResponse `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if body.Data.Status != processes.StatusError || body.Data.Error == "" {
		t.Errorf("失败状态没有透出: %+v", body.Data)
	}
}

// TestProcessListAgentUnavailable 代理不可用 → 503 AGENT_UNAVAILABLE。
func TestProcessListAgentUnavailable(t *testing.T) {
	srv, cookie := processesFixture(t, &stubProcesses{
		listErr: fmt.Errorf("%w: 连接被拒绝", processes.ErrAgentUnavailable),
	})
	rec := servicesGet(t, srv, cookie, "/api/v1/processes")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("状态码 = %d，期望 503（%s）", rec.Code, rec.Body.String())
	}
	got := parseResult(t, rec)
	if got.Error == nil || got.Error.Code != "AGENT_UNAVAILABLE" {
		t.Fatalf("错误码 = %+v", got.Error)
	}
}

// TestProcessListWithoutAgent 没配置代理（演示模式）时回 503，而不是空列表：
// 空列表会让人以为这台机器上没有进程。
func TestProcessListWithoutAgent(t *testing.T) {
	srv, cookie := processesFixture(t, nil)
	rec := servicesGet(t, srv, cookie, "/api/v1/processes")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("状态码 = %d，期望 503（%s）", rec.Code, rec.Body.String())
	}
	got := parseResult(t, rec)
	if got.Error == nil || got.Error.Code != "AGENT_UNAVAILABLE" {
		t.Fatalf("错误码 = %+v", got.Error)
	}
}
