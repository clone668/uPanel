package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"upanel/internal/auth"
	"upanel/internal/config"
	"upanel/internal/platform"
	"upanel/internal/storage"
	"upanel/internal/version"
)

// stubStorage 是 StorageReader 的测试替身：只验证 HTTP 层的映射
// （状态码、错误码、404、透传）。采集与拓扑计算由 internal/storage 的测试覆盖。
type stubStorage struct {
	topo   storage.Topology
	err    error
	calls  int
	lastID string

	// P07.4：卷视图。volErr / blockerErr 让“代理拒绝”的翻译能单独被测。
	volumes    []storage.Volume
	volErr     error
	blockers   storage.VolumeBlockers
	blockerErr error
	lastVolID  string
}

func (s *stubStorage) Topology(ctx context.Context) (storage.Topology, error) {
	s.calls++
	return s.topo, s.err
}

func (s *stubStorage) Device(ctx context.Context, id string) (storage.Device, storage.Topology, bool, error) {
	s.calls++
	s.lastID = id
	if s.err != nil {
		return storage.Device{}, storage.Topology{}, false, s.err
	}
	for _, d := range s.topo.Nodes {
		if d.ID == id {
			return d, s.topo, true, nil
		}
	}
	return storage.Device{}, s.topo, false, nil
}

// Volumes 返回预置的卷列表。
func (s *stubStorage) Volumes(context.Context) ([]storage.Volume, error) {
	s.calls++
	if s.volErr != nil {
		return nil, s.volErr
	}
	return s.volumes, nil
}

// Volume 按 id 挑一条；不存在的卷第二个返回值为 false（接口层按 404 处理）。
func (s *stubStorage) Volume(_ context.Context, id string) (storage.Volume, bool, error) {
	s.calls++
	s.lastVolID = id
	if s.volErr != nil {
		return storage.Volume{}, false, s.volErr
	}
	for _, v := range s.volumes {
		if v.ID == id {
			return v, true, nil
		}
	}
	return storage.Volume{}, false, nil
}

// VolumeBlockers 返回预置的占用信息。
func (s *stubStorage) VolumeBlockers(_ context.Context, id string) (storage.VolumeBlockers, error) {
	s.calls++
	s.lastVolID = id
	if s.blockerErr != nil {
		return storage.VolumeBlockers{}, s.blockerErr
	}
	return s.blockers, nil
}

func storageFixture(t *testing.T, reader StorageReader) (*Server, *http.Cookie) {
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
		Storage:         reader,
	})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	seedTestUser(t, srv.opts.Auth, testUser, testUserPass)
	cookie, _ := loginOK(t, srv)
	return srv, cookie
}

func storageGet(t *testing.T, srv *Server, cookie *http.Cookie, path string) *httptest.ResponseRecorder {
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

// sampleTopology 是一份最小但合法的拓扑：一块受保护的系统盘 + 一个分区 + 一条边。
// 只用于 HTTP 层断言，不冒充真实采集结果。
func sampleTopology() storage.Topology {
	collected := time.Date(2026, 10, 20, 17, 40, 0, 0, time.UTC)
	return storage.Topology{
		Status:          storage.StatusOK,
		SnapshotVersion: "sha256:abc",
		CollectedAt:     collected,
		IntervalMS:      5000,
		Nodes: []storage.Device{
			{
				ID: "sda", Kind: storage.KindDisk, Name: "sda", SizeBytes: 40 << 30,
				LogicalSectorBytes: 512, PhysicalSectorBytes: 512,
				Online: true, IdentityConfidence: storage.ConfidenceMedium,
				IdentityFingerprint: "sha256:disk",
				Health:              storage.Health{Status: storage.HealthUnknown},
				Capabilities:        []storage.Capability{},
				Protection: storage.Protection{
					SystemDisk: true, Reasons: []string{"承载根文件系统"}, Sources: []string{"sda1"},
				},
				CollectedAt: collected,
			},
			{
				ID: "sda1", Kind: storage.KindPartition, Name: "sda1", SizeBytes: 39 << 30,
				LogicalSectorBytes: 512, PhysicalSectorBytes: 512,
				Online: true, IdentityConfidence: storage.ConfidenceMedium,
				IdentityFingerprint: "sha256:part",
				Health:              storage.Health{Status: storage.HealthUnknown},
				Capabilities:        []storage.Capability{},
				Protection: storage.Protection{
					SystemDisk: true, Reasons: []string{"承载根文件系统"}, Sources: []string{"sda1"},
				},
				CollectedAt: collected,
			},
		},
		Edges: []storage.Edge{
			{FromDeviceID: "sda1", ToDeviceID: "sda", Relation: storage.RelationPartitionOf},
		},
	}
}

func TestStorageRoutesRequireSession(t *testing.T) {
	srv, _ := storageFixture(t, &stubStorage{topo: sampleTopology()})
	for _, path := range []string{
		"/api/v1/storage/topology",
		"/api/v1/storage/devices",
		"/api/v1/storage/devices/sda",
	} {
		rec := storageGet(t, srv, nil, path)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: 未登录状态码 = %d，期望 401", path, rec.Code)
		}
		got := parseResult(t, rec)
		if got.Error == nil || got.Error.Code != "UNAUTHENTICATED" {
			t.Errorf("%s: 错误码 = %+v", path, got.Error)
		}
	}
}

func TestStorageRoutesAllowedForViewer(t *testing.T) {
	srv, cookie := storageFixture(t, &stubStorage{topo: sampleTopology()})
	if _, err := srv.opts.Auth.CreateUser(context.Background(), auth.CreateUserInput{
		Username: "alice", DisplayName: "alice", Role: auth.RoleViewer, Password: "alice-horse-9",
	}); err != nil {
		t.Fatalf("建 viewer 失败: %v", err)
	}
	viewerCookie, _ := loginAsUser(t, srv, "alice", "alice-horse-9")
	_ = cookie

	// CONTRACTS.md 第 6 节：三条读接口的权限都是 V，viewer 也能看设备树。
	for _, path := range []string{
		"/api/v1/storage/topology",
		"/api/v1/storage/devices",
		"/api/v1/storage/devices/sda",
	} {
		rec := storageGet(t, srv, viewerCookie, path)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: viewer 状态码 = %d，期望 200（%s）", path, rec.Code, rec.Body.String())
		}
	}
}

func TestStorageTopologyPassesThrough(t *testing.T) {
	srv, cookie := storageFixture(t, &stubStorage{topo: sampleTopology()})
	rec := storageGet(t, srv, cookie, "/api/v1/storage/topology")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d（%s）", rec.Code, rec.Body.String())
	}
	var body struct {
		Data storage.Topology `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if body.Data.SnapshotVersion != "sha256:abc" {
		t.Errorf("snapshot_version = %q", body.Data.SnapshotVersion)
	}
	if len(body.Data.Nodes) != 2 || len(body.Data.Edges) != 1 {
		t.Errorf("nodes/edges = %d/%d，想要 2/1", len(body.Data.Nodes), len(body.Data.Edges))
	}
	// 字节字段在线上必须是十进制字符串。
	if !strings.Contains(rec.Body.String(), `"size_bytes":"42949672960"`) {
		t.Errorf("size_bytes 不是十进制字符串: %s", rec.Body.String())
	}
}

func TestStorageDeviceListShape(t *testing.T) {
	srv, cookie := storageFixture(t, &stubStorage{topo: sampleTopology()})
	rec := storageGet(t, srv, cookie, "/api/v1/storage/devices")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d（%s）", rec.Code, rec.Body.String())
	}
	var body struct {
		Data deviceListResponse `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if body.Data.Status != storage.StatusOK || body.Data.SnapshotVersion != "sha256:abc" {
		t.Errorf("元数据 = %+v", body.Data)
	}
	if body.Data.CollectedAt.IsZero() || body.Data.IntervalMS != 5000 {
		t.Errorf("采集时间/间隔 = %v/%d", body.Data.CollectedAt, body.Data.IntervalMS)
	}
	if len(body.Data.Devices) != 2 {
		t.Fatalf("设备数 = %d", len(body.Data.Devices))
	}
	if !body.Data.Devices[0].Protection.SystemDisk {
		t.Errorf("设备列表里丢了 protection：%+v", body.Data.Devices[0].Protection)
	}
}

func TestStorageDeviceDetailAnd404(t *testing.T) {
	reader := &stubStorage{topo: sampleTopology()}
	srv, cookie := storageFixture(t, reader)

	rec := storageGet(t, srv, cookie, "/api/v1/storage/devices/sda1")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d（%s）", rec.Code, rec.Body.String())
	}
	var body struct {
		Data deviceDetailResponse `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if body.Data.Device.ID != "sda1" {
		t.Errorf("device.id = %q", body.Data.Device.ID)
	}
	// 详情只带与这台设备直接相关的边。
	if len(body.Data.Edges) != 1 || body.Data.Edges[0].ToDeviceID != "sda" {
		t.Errorf("edges = %+v，想要唯一的 sda1->sda", body.Data.Edges)
	}
	if !body.Data.Device.Protection.SystemDisk {
		t.Errorf("详情里丢了 protection")
	}

	// 不存在的设备：404 NOT_FOUND，不是 500，也不是空对象。
	rec = storageGet(t, srv, cookie, "/api/v1/storage/devices/sdz")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("未知设备状态码 = %d，期望 404（%s）", rec.Code, rec.Body.String())
	}
	got := parseResult(t, rec)
	if got.Error == nil || got.Error.Code != "NOT_FOUND" {
		t.Errorf("错误码 = %+v", got.Error)
	}
	if reader.lastID != "sdz" {
		t.Errorf("传给读取端的 id = %q", reader.lastID)
	}
}

// TestStorageDeviceIDIsNotACommand 钉住“设备 id 只当标识用”：
// 任何字符串都不会被拼成路径或命令，未知的就是 404。
func TestStorageDeviceIDIsNotACommand(t *testing.T) {
	reader := &stubStorage{topo: sampleTopology()}
	srv, cookie := storageFixture(t, reader)
	for _, id := range []string{"..", "../../etc/shadow", "sda;id", "sda$(id)", "%2e%2e"} {
		rec := storageGet(t, srv, cookie, "/api/v1/storage/devices/"+id)
		if rec.Code != http.StatusNotFound && rec.Code != http.StatusBadRequest {
			t.Errorf("%q: 状态码 = %d，期望 404/400（%s）", id, rec.Code, rec.Body.String())
			continue
		}
		if got := parseResult(t, rec); got.Error == nil ||
			(got.Error.Code != "NOT_FOUND" && got.Error.Code != "VALIDATION_ERROR") {
			t.Errorf("%q: 错误码 = %+v", id, got.Error)
		}
	}
}

func TestStorageUnavailableWhenAgentDown(t *testing.T) {
	srv, cookie := storageFixture(t, &stubStorage{err: storage.ErrAgentUnavailable})
	for _, path := range []string{
		"/api/v1/storage/topology",
		"/api/v1/storage/devices",
		"/api/v1/storage/devices/sda",
	} {
		rec := storageGet(t, srv, cookie, path)
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s: 状态码 = %d，期望 503", path, rec.Code)
			continue
		}
		got := parseResult(t, rec)
		if got.Error == nil || got.Error.Code != "AGENT_UNAVAILABLE" {
			t.Errorf("%s: 错误码 = %+v，期望 AGENT_UNAVAILABLE", path, got.Error)
		}
	}
}

func TestStorageNotConfiguredIs503(t *testing.T) {
	// 没有配置存储读取端（演示模式）：503 AGENT_UNAVAILABLE，而不是空列表或 500。
	srv, cookie := storageFixture(t, nil)
	rec := storageGet(t, srv, cookie, "/api/v1/storage/devices")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("状态码 = %d，期望 503（%s）", rec.Code, rec.Body.String())
	}
	got := parseResult(t, rec)
	if got.Error == nil || got.Error.Code != "AGENT_UNAVAILABLE" {
		t.Errorf("错误码 = %+v", got.Error)
	}
}

func TestStorageScanFailureIsReportedNotHidden(t *testing.T) {
	// 代理扫描失败时返回的是 status=error、nodes 保留上一次成功内容的快照。
	// HTTP 层必须原样透出 status 与 error，让页面能说“这是多久之前的数据”。
	topo := sampleTopology()
	topo.Status = storage.StatusError
	topo.Error = "storage: 读不到 /sys/block（没有 sysfs？）"
	srv, cookie := storageFixture(t, &stubStorage{topo: topo})

	rec := storageGet(t, srv, cookie, "/api/v1/storage/devices")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（扫描失败不是 HTTP 错误）", rec.Code)
	}
	var body struct {
		Data deviceListResponse `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if body.Data.Status != storage.StatusError || body.Data.Error == "" {
		t.Errorf("status/error = %q/%q，必须透出失败原因", body.Data.Status, body.Data.Error)
	}
	if len(body.Data.Devices) == 0 {
		t.Errorf("扫描失败时不该清空设备列表")
	}
}

func TestStorageDetailOtherErrorIs500(t *testing.T) {
	// 代理明确拒绝或返回不合法内容：500 COMMAND_FAILED（要人看日志），不是 503。
	srv, cookie := storageFixture(t, &stubStorage{err: errBoom{}})
	rec := storageGet(t, srv, cookie, "/api/v1/storage/topology")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("状态码 = %d，期望 500（%s）", rec.Code, rec.Body.String())
	}
	got := parseResult(t, rec)
	if got.Error == nil || got.Error.Code != "COMMAND_FAILED" {
		t.Errorf("错误码 = %+v", got.Error)
	}
}

type errBoom struct{}

func (errBoom) Error() string { return "boom" }
