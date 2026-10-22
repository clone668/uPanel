package api

// 卷视图的 HTTP 层测试（P07.4）。
//
// 这一层只做三件事，测试也只钉这三件：
//   1. 权限（CONTRACTS.md 第 6 节：列表/详情是 V，占用详情是 admin）；
//   2. 形状（列表包一层对象、不存在的卷是 404、卷标识先过形状检查）；
//   3. 错误翻译（代理拒绝原样带出、代理不可用是 503、其余是 500）。
// 卷内容本身由 internal/storage 与 internal/agent 的测试覆盖。

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"upanel/internal/auth"
	"upanel/internal/httpx"
	"upanel/internal/storage"
)

const apiVolumeID = "vol_0123456789abcdef0123456789abcdef"

// sampleVolume 是一条形状完整的卷视图。
func sampleVolume() storage.Volume {
	return storage.Volume{
		ID:             apiVolumeID,
		Version:        "3",
		DisplayName:    "数据卷",
		Slug:           "data",
		DeviceID:       "sdb1",
		FilesystemUUID: "5f1c9a3e-2b64-4c7d-8e10-9a3b5c7d1e2f",
		FilesystemType: storage.FilesystemExt4,
		MountPath:      storage.MountPathOf("data"),
		Automount:      false,
		ReadOnly:       false,
		Nodev:          true,
		Nosuid:         true,
		DesiredState:   storage.DesiredUnmounted,
		// 现场观察到的状态，不是 desired_state 的抄写。
		ObservedState: storage.VolumeStateMismatch,
		ObservedError: "挂载点上读到的是另一个 UUID",
		Dependencies:  []storage.VolumeDependency{},
		UpdatedAt:     time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC),
	}
}

// sampleBlockers 是一份“有进程占着”的占用信息。
func sampleBlockers() storage.VolumeBlockers {
	return storage.VolumeBlockers{
		VolumeID:  apiVolumeID,
		Mounted:   true,
		Mounts:    []string{storage.MountPathOf("data")},
		Processes: []storage.BusyProcess{{PID: "4242", Comm: "postgres"}},
		Holders:   []string{},
		Apps:      []storage.VolumeDependency{},
		Shares:    []storage.VolumeDependency{},
		Backups:   []storage.VolumeDependency{},
		Busy:      true,
		CheckedAt: time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC),
	}
}

func volumeReader() *stubStorage {
	return &stubStorage{
		topo:     sampleTopology(),
		volumes:  []storage.Volume{sampleVolume()},
		blockers: sampleBlockers(),
	}
}

// TestVolumeRoutesRequireSession 三跳都要登录。
func TestVolumeRoutesRequireSession(t *testing.T) {
	srv, _ := storageFixture(t, volumeReader())
	for _, path := range []string{
		"/api/v1/storage/volumes",
		"/api/v1/storage/volumes/" + apiVolumeID,
		"/api/v1/storage/volumes/" + apiVolumeID + "/blockers",
	} {
		rec := storageGet(t, srv, nil, path)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: 未登录状态码 = %d，期望 401", path, rec.Code)
		}
	}
}

// TestVolumeRoutesViewerAndAdmin 钉住第 6 节的权限分界：
// 列表与详情 viewer 能看，占用详情（进程命令行、PID）只有 admin 能看。
func TestVolumeRoutesViewerAndAdmin(t *testing.T) {
	srv, _ := storageFixture(t, volumeReader())
	if _, err := srv.opts.Auth.CreateUser(t.Context(), auth.CreateUserInput{
		Username: "alice", DisplayName: "alice", Role: auth.RoleViewer, Password: "alice-horse-9",
	}); err != nil {
		t.Fatalf("建 viewer 失败: %v", err)
	}
	viewer, _ := loginAsUser(t, srv, "alice", "alice-horse-9")

	for _, path := range []string{
		"/api/v1/storage/volumes",
		"/api/v1/storage/volumes/" + apiVolumeID,
	} {
		if rec := storageGet(t, srv, viewer, path); rec.Code != http.StatusOK {
			t.Errorf("%s: viewer 状态码 = %d，期望 200（%s）", path, rec.Code, rec.Body.String())
		}
	}

	rec := storageGet(t, srv, viewer, "/api/v1/storage/volumes/"+apiVolumeID+"/blockers")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("viewer 看占用详情状态码 = %d，期望 403（%s）", rec.Code, rec.Body.String())
	}
	if got := parseResult(t, rec); got.Error == nil || got.Error.Code != "FORBIDDEN" {
		t.Errorf("错误码 = %+v", got.Error)
	}

	// 同一个地址 admin 能拿到内容（否则上面那条 403 也可能只是路由错了）。
	admin, _ := loginOK(t, srv)
	rec = storageGet(t, srv, admin, "/api/v1/storage/volumes/"+apiVolumeID+"/blockers")
	if rec.Code != http.StatusOK {
		t.Fatalf("admin 看占用详情状态码 = %d，期望 200（%s）", rec.Code, rec.Body.String())
	}
}

// TestVolumeListShape 列表是 {"volumes":[...]}，字段按同一套 DTO。
func TestVolumeListShape(t *testing.T) {
	srv, cookie := storageFixture(t, volumeReader())
	rec := storageGet(t, srv, cookie, "/api/v1/storage/volumes")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d（%s）", rec.Code, rec.Body.String())
	}
	var body struct {
		Data volumeListResponse `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if len(body.Data.Volumes) != 1 {
		t.Fatalf("卷数 = %d", len(body.Data.Volumes))
	}
	got := body.Data.Volumes[0]
	if got.ID != apiVolumeID || got.FilesystemUUID != sampleVolume().FilesystemUUID {
		t.Errorf("卷字段不对：%+v", got)
	}
	// 必须透出**观察到**的状态；desired_state 是 intent，两者不一致才是页面要提醒的事。
	if got.ObservedState != storage.VolumeStateMismatch || got.DesiredState != storage.DesiredUnmounted {
		t.Errorf("observed/desired = %q/%q，两个都要原样透出", got.ObservedState, got.DesiredState)
	}
	if got.ObservedError == "" {
		t.Error("observed_state 不是 mounted/unmounted 时必须带原因")
	}
}

// TestVolumeListEmptyIsArray 空列表是 [] 而不是 null。
func TestVolumeListEmptyIsArray(t *testing.T) {
	reader := volumeReader()
	reader.volumes = nil
	srv, cookie := storageFixture(t, reader)
	rec := storageGet(t, srv, cookie, "/api/v1/storage/volumes")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d（%s）", rec.Code, rec.Body.String())
	}
	if !json.Valid(rec.Body.Bytes()) {
		t.Fatalf("响应不是合法 JSON: %s", rec.Body.String())
	}
	var body struct {
		Data struct {
			Volumes []storage.Volume `json:"volumes"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("%v", err)
	}
	if body.Data.Volumes == nil {
		t.Errorf("空列表序列化成了 null：%s", rec.Body.String())
	}
}

// TestVolumeDetailAnd404 详情：命中 200，未知卷 404，形状不对 400（且不打扰读取端）。
func TestVolumeDetailAnd404(t *testing.T) {
	reader := volumeReader()
	srv, cookie := storageFixture(t, reader)

	rec := storageGet(t, srv, cookie, "/api/v1/storage/volumes/"+apiVolumeID)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d（%s）", rec.Code, rec.Body.String())
	}
	var body struct {
		Data storage.Volume `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if body.Data.ID != apiVolumeID || body.Data.MountPath != storage.MountPathOf("data") {
		t.Errorf("卷 = %+v", body.Data)
	}

	other := "vol_fedcba9876543210fedcba9876543210"
	rec = storageGet(t, srv, cookie, "/api/v1/storage/volumes/"+other)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("未知卷状态码 = %d，期望 404（%s）", rec.Code, rec.Body.String())
	}
	if got := parseResult(t, rec); got.Error == nil || got.Error.Code != "NOT_FOUND" {
		t.Errorf("错误码 = %+v", got.Error)
	}

	// 形状不对的标识在本层就被挡住：它要进日志与审计，不能原样透传。
	for _, id := range []string{"..", "vol_X", "vol_0123456789ABCDEF0123456789abcdef", "vol_123"} {
		before := reader.calls
		rec = storageGet(t, srv, cookie, "/api/v1/storage/volumes/"+id)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%q: 状态码 = %d，期望 400（%s）", id, rec.Code, rec.Body.String())
		}
		if reader.calls != before {
			t.Errorf("%q: 形状不对的标识被透传给了读取端", id)
		}
	}
}

// TestVolumeBlockersShape 占用详情要带齐事实与结论，空列表是 [] 不是 null。
func TestVolumeBlockersShape(t *testing.T) {
	srv, cookie := storageFixture(t, volumeReader())
	rec := storageGet(t, srv, cookie, "/api/v1/storage/volumes/"+apiVolumeID+"/blockers")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d（%s）", rec.Code, rec.Body.String())
	}
	var body struct {
		Data struct {
			VolumeID  string                     `json:"volume_id"`
			Mounted   bool                       `json:"mounted"`
			Mounts    []string                   `json:"mounts"`
			Processes []storage.BusyProcess      `json:"processes"`
			Holders   []string                   `json:"holders"`
			Apps      []storage.VolumeDependency `json:"apps"`
			Busy      bool                       `json:"busy"`
			CheckedAt time.Time                  `json:"checked_at"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if body.Data.VolumeID != apiVolumeID || !body.Data.Mounted || !body.Data.Busy {
		t.Errorf("占用信息 = %+v", body.Data)
	}
	if len(body.Data.Processes) != 1 || body.Data.Processes[0].PID != "4242" {
		t.Errorf("进程 = %+v", body.Data.Processes)
	}
	if body.Data.Holders == nil || body.Data.Apps == nil {
		t.Errorf("空列表不该序列化成 null：%s", rec.Body.String())
	}
	if body.Data.CheckedAt.IsZero() {
		t.Error("checked_at 是零值：占用是某一瞬间的快照")
	}
}

// TestVolumeBlockersErrors：卷不存在 404、代理不可用 503、其余 500。
func TestVolumeBlockersErrors(t *testing.T) {
	t.Run("卷不存在是 404", func(t *testing.T) {
		reader := volumeReader()
		reader.blockerErr = &storage.RemoteError{
			Status: http.StatusNotFound, Code: httpx.CodeNotFound, Message: "卷不存在：" + apiVolumeID,
		}
		srv, cookie := storageFixture(t, reader)
		rec := storageGet(t, srv, cookie, "/api/v1/storage/volumes/"+apiVolumeID+"/blockers")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("状态码 = %d，期望 404（%s）", rec.Code, rec.Body.String())
		}
		if got := parseResult(t, rec); got.Error == nil || got.Error.Code != "NOT_FOUND" {
			t.Errorf("错误码 = %+v", got.Error)
		}
	})

	t.Run("代理不可用是 503", func(t *testing.T) {
		reader := volumeReader()
		reader.blockerErr = storage.ErrAgentUnavailable
		srv, cookie := storageFixture(t, reader)
		rec := storageGet(t, srv, cookie, "/api/v1/storage/volumes/"+apiVolumeID+"/blockers")
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("状态码 = %d，期望 503（%s）", rec.Code, rec.Body.String())
		}
		if got := parseResult(t, rec); got.Error == nil || got.Error.Code != "AGENT_UNAVAILABLE" {
			t.Errorf("错误码 = %+v", got.Error)
		}
	})

	t.Run("其余错误是 500", func(t *testing.T) {
		reader := volumeReader()
		reader.blockerErr = errBoom{}
		srv, cookie := storageFixture(t, reader)
		rec := storageGet(t, srv, cookie, "/api/v1/storage/volumes/"+apiVolumeID+"/blockers")
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("状态码 = %d，期望 500（%s）", rec.Code, rec.Body.String())
		}
		if got := parseResult(t, rec); got.Error == nil || got.Error.Code != "COMMAND_FAILED" {
			t.Errorf("错误码 = %+v", got.Error)
		}
	})
}

// TestVolumeRoutesWithoutReaderAre503 没有代理连接时三跳都是 503，不是空列表。
func TestVolumeRoutesWithoutReaderAre503(t *testing.T) {
	srv, cookie := storageFixture(t, nil)
	for _, path := range []string{
		"/api/v1/storage/volumes",
		"/api/v1/storage/volumes/" + apiVolumeID,
		"/api/v1/storage/volumes/" + apiVolumeID + "/blockers",
	} {
		rec := storageGet(t, srv, cookie, path)
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s: 状态码 = %d，期望 503（%s）", path, rec.Code, rec.Body.String())
			continue
		}
		if got := parseResult(t, rec); got.Error == nil || got.Error.Code != "AGENT_UNAVAILABLE" {
			t.Errorf("%s: 错误码 = %+v", path, got.Error)
		}
	}
}
