package agent

// /internal/v1/backup/repositories 的测试（P12.1）。
//
// 这条路由的存在理由见 backuprepo.go：动作参数被冻结成 repository_id + version，
// 定义只能由代理自己持有。于是这里要验的是“代理不轻信 Web”这件事本身：
// 定义必须过形状校验、版本冲突要 409、删定义不动数据、没接线就 404。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"upanel/internal/agentstore"
	"upanel/internal/backup"
	"upanel/internal/version"
)

// memoryMirror 是一个内存镜像（只用于本测试）。
type memoryMirror struct {
	mu     sync.Mutex
	next   int
	byID   map[string]backup.Repository
	byName map[string]string
}

func newMemoryMirror() *memoryMirror {
	return &memoryMirror{byID: map[string]backup.Repository{}, byName: map[string]string{}}
}

func mirrorID(n int) string {
	// rep_ + 32 位小写十六进制。
	return "rep_" + strings.Repeat("0", 31) + string(rune('0'+n))
}

func (m *memoryMirror) ListRepositories(context.Context) ([]backup.Repository, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]backup.Repository, 0, len(m.byID))
	for _, repo := range m.byID {
		out = append(out, repo)
	}
	return out, nil
}

func (m *memoryMirror) GetRepository(_ context.Context, id string) (backup.Repository, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	repo, ok := m.byID[id]
	return repo, ok, nil
}

func (m *memoryMirror) CreateRepository(_ context.Context, def backup.Definition, _ time.Time) (backup.Repository, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, clash := m.byName[def.Name]; clash {
		return backup.Repository{}, agentstore.ErrRepositoryName
	}
	m.next++
	id := mirrorID(m.next)
	repo := backup.Repository{ID: id, Name: def.Name, Definition: def, Version: 1}
	m.byID[id] = repo
	m.byName[def.Name] = id
	return repo, nil
}

func (m *memoryMirror) UpdateRepository(_ context.Context, id string, def backup.Definition,
	expectedVersion int64, _ time.Time) (backup.Repository, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	repo, ok := m.byID[id]
	if !ok {
		return backup.Repository{}, agentstore.ErrRepositoryNotFound
	}
	if repo.Version != expectedVersion {
		return backup.Repository{}, agentstore.ErrRepositoryVersion
	}
	repo.Definition = def
	repo.Name = def.Name
	repo.Version++
	m.byID[id] = repo
	return repo, nil
}

func (m *memoryMirror) DeleteRepository(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if repo, ok := m.byID[id]; ok {
		delete(m.byName, repo.Name)
		delete(m.byID, id)
	}
	return nil
}

// newBackupMirrorServer 构造一个接了镜像的代理服务。
func newBackupMirrorServer(t *testing.T, mirror BackupRepositoryMirror) *Server {
	t.Helper()
	srv, err := New(Options{
		SocketPath:         socketPathForTest(t),
		Version:            version.Info{Version: "test", Commit: "test", BuildDate: "test", GoVersion: "test"},
		Logger:             quietLogger(),
		ShutdownTimeout:    time.Second,
		Peer:               testPeer(),
		Secrets:            newTestSecretStore(t),
		Plans:              newTestPlanStore(t),
		Operations:         newTestOperationStore(t),
		BackupRepositories: mirror,
	})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	return srv
}

// localDefinitionObj 是一条合法的本地定义（不含外层包装）。
func localDefinitionObj(name string) string {
	return `{"name":"` + name + `","backend":"local",` +
		`"location":{"root_id":"root1","relative_path":"backups/nightly"},` +
		`"password_secret_ref":"sec_0123456789abcdef0123456789abcdef"}`
}

// localDefinitionJSON 是 POST 的请求体。
func localDefinitionJSON(name string) string {
	return `{"definition":` + localDefinitionObj(name) + `}`
}

// decodeData 取出信封里的 data 字段。
//
// 代理的响应一律是 {"data":…,"request_id":…}（CONTRACTS 第 1 节），
// 直接 Unmarshal 到业务结构只会得到一堆零值——那会让“创建成功”看起来像成功，
// 而里面全是空的。
func decodeData(t *testing.T, body []byte, dst any) {
	t.Helper()
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("响应不是合法 JSON: %v（%s）", err, body)
	}
	if len(env.Data) == 0 {
		t.Fatalf("响应里没有 data：%s", body)
	}
	if err := json.Unmarshal(env.Data, dst); err != nil {
		t.Fatalf("data 不是期望的形状: %v（%s）", err, env.Data)
	}
}

func TestBackupMirrorRouteHidesWithoutWiring(t *testing.T) {
	srv := newTestServer(t) // 没有 BackupRepositories
	req := httptest.NewRequest(http.MethodGet, "/internal/v1/backup/repositories", nil)
	rec := serveAs(srv, req, testWebUID)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("没接线时应 404，得到 %d", rec.Code)
	}
}

func TestBackupMirrorCreateAndRead(t *testing.T) {
	srv := newBackupMirrorServer(t, newMemoryMirror())

	req := httptest.NewRequest(http.MethodPost, "/internal/v1/backup/repositories",
		strings.NewReader(localDefinitionJSON("夜间根文件")))
	req.Header.Set("Content-Type", "application/json")
	rec := serveAs(srv, req, testWebUID)
	if rec.Code != http.StatusCreated {
		t.Fatalf("创建应 201，得到 %d（%s）", rec.Code, rec.Body.String())
	}
	var created backupRepositoryResponse
	decodeData(t, rec.Body.Bytes(), &created)
	if created.Repository.Version != 1 {
		t.Fatalf("新仓库版本应为 1，得到 %d", created.Repository.Version)
	}
	if created.Repository.Backend != "local" {
		t.Fatalf("后端不对：%q", created.Repository.Backend)
	}
	if created.Repository.Location != "local:root1:/backups/nightly" {
		t.Fatalf("位置展示不对：%q", created.Repository.Location)
	}
	if !agentstore.ValidRepositoryID(created.Repository.ID) {
		t.Fatalf("id 形态不对：%q", created.Repository.ID)
	}

	// 读回来。
	path := "/internal/v1/backup/repositories/" + created.Repository.ID
	rec = serveAs(srv, httptest.NewRequest(http.MethodGet, path, nil), testWebUID)
	if rec.Code != http.StatusOK {
		t.Fatalf("按 id 读取应 200，得到 %d", rec.Code)
	}

	// 列表。
	rec = serveAs(srv, httptest.NewRequest(http.MethodGet, "/internal/v1/backup/repositories", nil), testWebUID)
	if rec.Code != http.StatusOK {
		t.Fatalf("列表应 200，得到 %d", rec.Code)
	}
	var list backupRepositoryListResponse
	decodeData(t, rec.Body.Bytes(), &list)
	if len(list.Repositories) != 1 {
		t.Fatalf("列表应有 1 条，得到 %d", len(list.Repositories))
	}
}

func TestBackupMirrorRejectsInvalidDefinition(t *testing.T) {
	srv := newBackupMirrorServer(t, newMemoryMirror())
	// 本地后端带 host：定义是拼凑的，必须 400 并说清是哪个字段。
	body := `{"definition":{"name":"坏的","backend":"local",` +
		`"location":{"root_id":"root1","relative_path":"backups","host":"evil.example.com"},` +
		`"password_secret_ref":"sec_0123456789abcdef0123456789abcdef"}}`
	req := httptest.NewRequest(http.MethodPost, "/internal/v1/backup/repositories", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := serveAs(srv, req, testWebUID)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("非法定义应 400，得到 %d（%s）", rec.Code, rec.Body.String())
	}
	code, message := decodeErrorEnvelope(t, rec.Body.Bytes())
	if code != "VALIDATION_ERROR" {
		t.Fatalf("错误码 = %q", code)
	}
	if !strings.Contains(message, "host") {
		t.Fatalf("错误信息要指出字段：%q", message)
	}
}

func TestBackupMirrorRejectsPathTraversal(t *testing.T) {
	srv := newBackupMirrorServer(t, newMemoryMirror())
	body := `{"definition":{"name":"越界","backend":"local",` +
		`"location":{"root_id":"root1","relative_path":"../../etc"},` +
		`"password_secret_ref":"sec_0123456789abcdef0123456789abcdef"}}`
	req := httptest.NewRequest(http.MethodPost, "/internal/v1/backup/repositories", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := serveAs(srv, req, testWebUID)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("越界路径应 400，得到 %d（%s）", rec.Code, rec.Body.String())
	}
}

func TestBackupMirrorPatchVersionConflict(t *testing.T) {
	mirror := newMemoryMirror()
	srv := newBackupMirrorServer(t, mirror)

	rec := serveAs(srv, func() *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/internal/v1/backup/repositories",
			strings.NewReader(localDefinitionJSON("夜间根文件")))
		req.Header.Set("Content-Type", "application/json")
		return req
	}(), testWebUID)
	var created backupRepositoryResponse
	decodeData(t, rec.Body.Bytes(), &created)
	path := "/internal/v1/backup/repositories/" + created.Repository.ID

	// 版本对：改成功，版本 +1。
	req := httptest.NewRequest(http.MethodPatch, path,
		strings.NewReader(`{"version":1,"definition":`+localDefinitionObj("新名字")+`}`))
	req.Header.Set("Content-Type", "application/json")
	rec = serveAs(srv, req, testWebUID)
	if rec.Code != http.StatusOK {
		t.Fatalf("正常 PATCH 应 200，得到 %d（%s）", rec.Code, rec.Body.String())
	}
	var updated backupRepositoryResponse
	decodeData(t, rec.Body.Bytes(), &updated)
	if updated.Repository.Version != 2 {
		t.Fatalf("改完版本应为 2，得到 %d", updated.Repository.Version)
	}

	// 拿着旧版本再改：409。
	req = httptest.NewRequest(http.MethodPatch, path,
		strings.NewReader(`{"version":1,"definition":`+localDefinitionObj("又改名")+`}`))
	req.Header.Set("Content-Type", "application/json")
	rec = serveAs(srv, req, testWebUID)
	if rec.Code != http.StatusConflict {
		t.Fatalf("版本冲突应 409，得到 %d（%s）", rec.Code, rec.Body.String())
	}
	if code := decodeErrorCode(t, rec.Body.Bytes()); code != "VERSION_CONFLICT" {
		t.Fatalf("错误码 = %q", code)
	}

	// 缺 version：400（不许默默当成“改成最新”）。
	req = httptest.NewRequest(http.MethodPatch, path, strings.NewReader(localDefinitionJSON("没版本号")))
	req.Header.Set("Content-Type", "application/json")
	rec = serveAs(srv, req, testWebUID)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("缺 version 应 400，得到 %d", rec.Code)
	}
}

func TestBackupMirrorDeleteIsIdempotent(t *testing.T) {
	mirror := newMemoryMirror()
	srv := newBackupMirrorServer(t, mirror)

	rec := serveAs(srv, func() *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/internal/v1/backup/repositories",
			strings.NewReader(localDefinitionJSON("夜间根文件")))
		req.Header.Set("Content-Type", "application/json")
		return req
	}(), testWebUID)
	var created backupRepositoryResponse
	decodeData(t, rec.Body.Bytes(), &created)
	path := "/internal/v1/backup/repositories/" + created.Repository.ID

	if rec := serveAs(srv, httptest.NewRequest(http.MethodDelete, path, nil), testWebUID); rec.Code != http.StatusNoContent {
		t.Fatalf("删除应 204，得到 %d", rec.Code)
	}
	// 再删一次也是 204：幂等（重试不会变成错误）。
	if rec := serveAs(srv, httptest.NewRequest(http.MethodDelete, path, nil), testWebUID); rec.Code != http.StatusNoContent {
		t.Fatalf("重复删除应仍是 204，得到 %d", rec.Code)
	}
	// 读已删的定义：404。
	if rec := serveAs(srv, httptest.NewRequest(http.MethodGet, path, nil), testWebUID); rec.Code != http.StatusNotFound {
		t.Fatalf("删除后读取应 404，得到 %d", rec.Code)
	}
}

func TestBackupMirrorRejectsBadID(t *testing.T) {
	srv := newBackupMirrorServer(t, newMemoryMirror())
	rec := serveAs(srv, httptest.NewRequest(http.MethodGet, "/internal/v1/backup/repositories/rep_zzz", nil), testWebUID)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("形态不对的 id 应 400，得到 %d", rec.Code)
	}
}

func TestBackupMirrorDeniesRoot(t *testing.T) {
	srv := newBackupMirrorServer(t, newMemoryMirror())
	rec := serveAs(srv, httptest.NewRequest(http.MethodGet, "/internal/v1/backup/repositories", nil), 0)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("root 不该能改仓库镜像，得到 %d", rec.Code)
	}
}
