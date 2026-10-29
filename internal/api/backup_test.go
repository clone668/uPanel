package api

// 备份管理接口（P12.2）。
//
// 断言的重点不是"接口能返回 200"，而是七件事：
//  1. 权限：整组 admin——viewer 连列表都拿不到；未登录 401；
//  2. 仓库的登记与修改要再认证（定义引用仓库口令与 SFTP 私钥），策略的不需要；
//  3. 定义先过 internal/backup 的规范化：形状不对是 400，不是"存下来再说"；
//  4. 乐观并发与"记录不在"是两个状态码：VERSION_CONFLICT 409、NOT_FOUND 404；
//  5. 没有 `Backup` 调用端时回 503，**不是**空列表——空列表会被读成"没配备份"；
//  6. 快照列表不带大小、详情才带；列表必须给 repository_id；registered=false 是状态；
//  7. backup.run 的参数展开：app_ids 变成运行对象，调用方不能自己给 apps，
//     解析不出就 422 阻塞而不是"少停一个也照跑"。

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"upanel/internal/agentclient"
	"upanel/internal/apps"
	"upanel/internal/audit"
	"upanel/internal/backup"
	"upanel/internal/certificates"
	"upanel/internal/config"
	"upanel/internal/httpx"
	"upanel/internal/platform"
	"upanel/internal/store"
	"upanel/internal/version"
)

const (
	testRepoID     = "rep_0123456789abcdef0123456789abcdef"
	testPolicyID   = "pol_0123456789abcdef0123456789abcdef"
	testSnapshotID = "1111111111111111111111111111111111111111111111111111111111111111"
	testUnit       = "upanel-web.service"
	testContainer  = "abcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcd"
)

// fakeBackupManager 是 BackupManager 的测试替身。
//
// 它不做判断：只记录收到什么、回放事先摆好的结果。判断（策略形状、应用授权、
// 仓库位置）分别由 internal/backup 与代理层的用例覆盖。
type fakeBackupManager struct {
	repos     []agentclient.BackupRepository
	repo      agentclient.BackupRepository
	repoErr   error
	created   backup.Definition
	createErr error
	updatedID string
	updatedV  int64
	updateErr error

	policies  []agentclient.BackupPolicy
	policy    agentclient.BackupPolicy
	policyErr error
	newPolicy backup.Policy
	updPolicy backup.Policy
	updV      int64
	updErr    error
	deletedID string
	deleteErr error

	page      agentclient.BackupSnapshotPage
	pageErr   error
	gotRepoID string
	gotPolicy string
	detail    agentclient.BackupSnapshotDetail
	detailErr error
	gotSnapID string
}

func (f *fakeBackupManager) ListBackupRepositories(context.Context) ([]agentclient.BackupRepository, error) {
	if f.repoErr != nil {
		return nil, f.repoErr
	}
	return f.repos, nil
}

func (f *fakeBackupManager) GetBackupRepository(_ context.Context, id string) (agentclient.BackupRepository, error) {
	if f.repoErr != nil {
		return agentclient.BackupRepository{}, f.repoErr
	}
	if f.repo.ID != id {
		return agentclient.BackupRepository{}, &agentclient.Error{Status: 404, Code: "NOT_FOUND", Message: "代理里没有这条仓库"}
	}
	return f.repo, nil
}

func (f *fakeBackupManager) CreateBackupRepository(_ context.Context, def backup.Definition) (agentclient.BackupRepository, error) {
	f.created = def
	if f.createErr != nil {
		return agentclient.BackupRepository{}, f.createErr
	}
	repo := f.repo
	repo.ID = testRepoID
	repo.Version = 1
	repo.Definition = def
	repo.Name = def.Name
	repo.Backend = string(def.Backend)
	return repo, nil
}

func (f *fakeBackupManager) UpdateBackupRepository(_ context.Context, id string, def backup.Definition, version int64) (agentclient.BackupRepository, error) {
	f.updatedID, f.updatedV, f.created = id, version, def
	if f.updateErr != nil {
		return agentclient.BackupRepository{}, f.updateErr
	}
	repo := f.repo
	repo.ID = id
	repo.Version = version + 1
	repo.Definition = def
	repo.Name = def.Name
	repo.Backend = string(def.Backend)
	return repo, nil
}

func (f *fakeBackupManager) ListBackupPolicies(context.Context) ([]agentclient.BackupPolicy, error) {
	if f.policyErr != nil {
		return nil, f.policyErr
	}
	return f.policies, nil
}

func (f *fakeBackupManager) GetBackupPolicy(_ context.Context, id string) (agentclient.BackupPolicy, error) {
	if f.policyErr != nil {
		return agentclient.BackupPolicy{}, f.policyErr
	}
	if f.policy.ID != id {
		return agentclient.BackupPolicy{}, &agentclient.Error{Status: 404, Code: "NOT_FOUND", Message: "代理里没有这条策略"}
	}
	return f.policy, nil
}

func (f *fakeBackupManager) CreateBackupPolicy(_ context.Context, pol backup.Policy) (agentclient.BackupPolicy, error) {
	f.newPolicy = pol
	if f.policyErr != nil {
		return agentclient.BackupPolicy{}, f.policyErr
	}
	out := policyView(testPolicyID, 1, pol)
	return out, nil
}

func (f *fakeBackupManager) UpdateBackupPolicy(_ context.Context, id string, pol backup.Policy, version int64) (agentclient.BackupPolicy, error) {
	f.updPolicy, f.updV = pol, version
	if f.updErr != nil {
		return agentclient.BackupPolicy{}, f.updErr
	}
	out := policyView(id, version+1, pol)
	return out, nil
}

func (f *fakeBackupManager) DeleteBackupPolicy(_ context.Context, id string) error {
	f.deletedID = id
	return f.deleteErr
}

func (f *fakeBackupManager) ListBackupSnapshots(_ context.Context, repositoryID, policyID string) (agentclient.BackupSnapshotPage, error) {
	f.gotRepoID, f.gotPolicy = repositoryID, policyID
	if f.pageErr != nil {
		return agentclient.BackupSnapshotPage{}, f.pageErr
	}
	return f.page, nil
}

func (f *fakeBackupManager) GetBackupSnapshot(_ context.Context, repositoryID, snapshotID string) (agentclient.BackupSnapshotDetail, error) {
	f.gotRepoID, f.gotSnapID = repositoryID, snapshotID
	if f.detailErr != nil {
		return agentclient.BackupSnapshotDetail{}, f.detailErr
	}
	return f.detail, nil
}

// backupFixture 是一个接了假备份调用端的生产模式服务器。
type backupFixture struct {
	*adminFixture
	mgr *fakeBackupManager
}

// newBackupFixture 构造夹具。mgr 为 nil 表示"演示模式没接代理"。
func newBackupFixture(t *testing.T, mgr BackupManager) *backupFixture {
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
		Backup:            mgr,
	})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	seedTestUser(t, srv.opts.Auth, testUser, testUserPass)
	cookie, csrf := loginOK(t, srv)
	f := &backupFixture{adminFixture: &adminFixture{srv: srv, cookie: cookie, csrf: csrf, db: db}}
	if fake, ok := mgr.(*fakeBackupManager); ok {
		f.mgr = fake
	}
	return f
}

// sampleRepository 是一条形状正确的 local 仓库定义。
func sampleRepository() backup.Definition {
	return backup.Definition{
		Name:              "夜间根文件",
		Backend:           backup.BackendLocal,
		Location:          backup.Location{RootID: "root1", RelativePath: "backups/root"},
		PasswordSecretRef: "sec_0123456789abcdef0123456789abcdef",
	}
}

// samplePolicy 是一条 files_only 策略（最简形态）。
func samplePolicy() backup.Policy {
	return backup.Policy{
		Name:            "日志目录",
		RepositoryID:    testRepoID,
		Sources:         []backup.Source{{Kind: backup.SourceKindRoot, RootID: "root1", RelativePath: "var/log"}},
		ConsistencyMode: backup.ConsistencyFilesOnly,
		Retention:       backup.Retention{KeepLast: 7},
	}
}

// mgrWithRepository 返回一个已经有一条仓库的假调用端。
func mgrWithRepository() *fakeBackupManager {
	def := sampleRepository()
	repo := agentclient.BackupRepository{
		ID: testRepoID, Name: def.Name, Backend: string(def.Backend),
		Definition: def, Version: 3,
	}
	return &fakeBackupManager{repo: repo, repos: []agentclient.BackupRepository{repo}}
}

// TestBackupRequiresAdmin：整组 admin，未登录 401、viewer 403。
func TestBackupRequiresAdmin(t *testing.T) {
	f := newBackupFixture(t, mgrWithRepository())

	for _, path := range []string{
		"/api/v1/backup/repositories",
		"/api/v1/backup/policies",
		"/api/v1/backup/snapshots?repository_id=" + testRepoID,
		"/api/v1/backup/snapshots/" + testSnapshotID + "?repository_id=" + testRepoID,
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		f.srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("未登录 %s 状态码 = %d，期望 401", path, rec.Code)
		}
	}

	f.createUser(t, "alice", "viewer", "alice-horse-9")
	viewerCookie, viewerCSRF := loginAsUser(t, f.srv, "alice", "alice-horse-9")
	rec := f.doAs(t, viewerCookie, viewerCSRF, http.MethodGet, "/api/v1/backup/repositories", "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("viewer 读仓库列表状态码 = %d，期望 403", rec.Code)
	}
}

// TestBackupUnavailableWithoutManager：没接代理时 503，不是空列表。
func TestBackupUnavailableWithoutManager(t *testing.T) {
	f := newBackupFixture(t, nil)
	rec := f.do(t, http.MethodGet, "/api/v1/backup/repositories", "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("状态码 = %d，期望 503", rec.Code)
	}
	if code := errorCode(t, rec); code != string(httpx.CodeAgentUnavailable) {
		t.Fatalf("错误码 = %q，期望 AGENT_UNAVAILABLE", code)
	}
	// 空列表会被人读成"这台机器上还没有配置备份"，那是不一样的话。
	if strings.Contains(rec.Body.String(), "items") {
		t.Fatalf("503 不该带一份列表: %s", rec.Body.String())
	}
}

func TestListBackupRepositories(t *testing.T) {
	f := newBackupFixture(t, mgrWithRepository())
	rec := f.do(t, http.MethodGet, "/api/v1/backup/repositories", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，响应 = %s", rec.Code, rec.Body.String())
	}
	var out backupRepositoryListResponse
	if err := json.Unmarshal(parseResult(t, rec).Data, &out); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(out.Items) != 1 || out.Items[0].ID != testRepoID {
		t.Fatalf("列表内容不对：%+v", out.Items)
	}
	if out.Items[0].PasswordSecretRef == "" {
		t.Fatal("引用要返回：PATCH 只能靠“读出来、改一处、存回去”")
	}
	if out.RepositoryStateQuery != backup.QueryRepositoryState {
		t.Fatalf("要告诉界面去哪里问仓库现状，实际 %q", out.RepositoryStateQuery)
	}
}

// TestCreateRepositoryRequiresReauth：登记仓库要 CSRF + 再认证。
func TestCreateRepositoryRequiresReauth(t *testing.T) {
	f := newBackupFixture(t, mgrWithRepository())
	body := `{"name":"夜间根文件","backend":"local","location":{"root_id":"root1","relative_path":"backups/root"},"password_secret_ref":"sec_0123456789abcdef0123456789abcdef"}`

	// 没有 CSRF 头。
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/backup/repositories", strings.NewReader(body))
	req.Header.Set("Origin", testOrigin)
	req.Header.Set("Cookie", f.cookie.String())
	req.Header.Set("Content-Type", "application/json")
	f.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("没有 CSRF 头状态码 = %d，期望 403", rec.Code)
	}

	// 有 CSRF 但没再认证。
	rec = f.do(t, http.MethodPost, "/api/v1/backup/repositories", body)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("没再认证状态码 = %d，期望 403", rec.Code)
	}
	if code := errorCode(t, rec); code != string(httpx.CodeReauthRequired) {
		t.Fatalf("错误码 = %q，期望 REAUTH_REQUIRED", code)
	}

	// 再认证之后可以登记。
	if rec := f.reauth(t, testUserPass); rec.Code != http.StatusOK {
		t.Fatalf("再认证失败: %d %s", rec.Code, rec.Body.String())
	}
	rec = f.do(t, http.MethodPost, "/api/v1/backup/repositories", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("登记失败: %d %s", rec.Code, rec.Body.String())
	}
	// 定义必须先过 backup.Definition.Normalize：转发给代理的是规范化之后的那一份。
	if f.mgr.created.Name != "夜间根文件" || f.mgr.created.Backend != backup.BackendLocal {
		t.Fatalf("转发给代理的定义不对：%+v", f.mgr.created)
	}

	// 审计：谁把备份改到了哪儿，要能回答。
	events := readAuditActions(t, f.db)
	if !slices.Contains(events, audit.ActionBackupRepositoryCreate) {
		t.Fatalf("缺少审计事件 %s：%v", audit.ActionBackupRepositoryCreate, events)
	}
}

func TestCreateRepositoryRejectsBadShape(t *testing.T) {
	f := newBackupFixture(t, mgrWithRepository())
	if rec := f.reauth(t, testUserPass); rec.Code != http.StatusOK {
		t.Fatalf("再认证失败: %d", rec.Code)
	}
	cases := []string{
		// local 后端带 sftp 字段
		`{"name":"x","backend":"local","location":{"root_id":"r","relative_path":"a","host":"h"},"password_secret_ref":"sec_0123456789abcdef0123456789abcdef"}`,
		// 未知后端
		`{"name":"x","backend":"s3","location":{"root_id":"r"},"password_secret_ref":"sec_0123456789abcdef0123456789abcdef"}`,
		// 秘密引用形状不对
		`{"name":"x","backend":"local","location":{"root_id":"r"},"password_secret_ref":"hunter2"}`,
		// 未知字段
		`{"name":"x","backend":"local","location":{"root_id":"r"},"password_secret_ref":"sec_0123456789abcdef0123456789abcdef","argv":"--insecure"}`,
	}
	for _, body := range cases {
		rec := f.do(t, http.MethodPost, "/api/v1/backup/repositories", body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s 状态码 = %d，期望 400（响应 %s）", body, rec.Code, rec.Body.String())
		}
	}
}

// TestUpdateRepositoryVersionConflict：版本不符是 409，记录不在是 404。
func TestUpdateRepositoryVersionConflict(t *testing.T) {
	f := newBackupFixture(t, mgrWithRepository())
	if rec := f.reauth(t, testUserPass); rec.Code != http.StatusOK {
		t.Fatalf("再认证失败: %d", rec.Code)
	}
	f.mgr.updateErr = &agentclient.Error{Status: 409, Code: "VERSION_CONFLICT", Message: "版本不符"}
	// 仓库体本身必须是能过 Normalize 的：否则 400 会出现在调用代理之前，
	// 这条用例就变成"在测形状校验"而不是"在测版本冲突"。
	body := `{"name":"夜间根文件","backend":"local","location":{"root_id":"root1","relative_path":"backups/root"},"password_secret_ref":"sec_0123456789abcdef0123456789abcdef","version":2}`
	rec := f.do(t, http.MethodPatch, "/api/v1/backup/repositories/"+testRepoID, body)
	if rec.Code != http.StatusConflict {
		t.Fatalf("状态码 = %d，期望 409", rec.Code)
	}
	if code := errorCode(t, rec); code != string(httpx.CodeVersionConflict) {
		t.Fatalf("错误码 = %q，期望 VERSION_CONFLICT", code)
	}

	f.mgr.updateErr = &agentclient.Error{Status: 404, Code: "NOT_FOUND", Message: "代理里没有这条仓库"}
	rec = f.do(t, http.MethodPatch, "/api/v1/backup/repositories/"+testRepoID, body)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("状态码 = %d，期望 404", rec.Code)
	}

	// version 是必填的乐观并发令牌：没有它代理会拒，面板先一步拒。
	f.mgr.updateErr = nil
	f.mgr.updatedID, f.mgr.updatedV = "", 0 // 前两次调用留下的痕迹要清掉，否则这条断言读的是它们
	rec = f.do(t, http.MethodPatch, "/api/v1/backup/repositories/"+testRepoID,
		`{"name":"夜间根文件","backend":"local","location":{"root_id":"root1","relative_path":"backups/root"},"password_secret_ref":"sec_0123456789abcdef0123456789abcdef"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("没有 version 状态码 = %d，期望 400", rec.Code)
	}
	if f.mgr.updatedV != 0 {
		t.Fatal("没有 version 就不该去问代理")
	}
}

// TestPolicyCRUD：策略登记不要求再认证，删除回 204。
func TestPolicyCRUD(t *testing.T) {
	f := newBackupFixture(t, mgrWithRepository())
	pol := samplePolicy()
	f.mgr.policy = policyView(testPolicyID, 2, pol)

	body, _ := json.Marshal(backupPolicyBody{
		Name:            pol.Name,
		RepositoryID:    pol.RepositoryID,
		Sources:         pol.Sources,
		ConsistencyMode: string(pol.ConsistencyMode),
		Retention:       pol.Retention,
	})
	rec := f.do(t, http.MethodPost, "/api/v1/backup/policies", string(body))
	if rec.Code != http.StatusCreated {
		t.Fatalf("登记策略失败: %d %s", rec.Code, rec.Body.String())
	}
	if f.mgr.newPolicy.ConsistencyMode != backup.ConsistencyFilesOnly {
		t.Fatalf("转发给代理的策略不对：%+v", f.mgr.newPolicy)
	}
	if !slices.Contains(readAuditActions(t, f.db), audit.ActionBackupPolicyCreate) {
		t.Fatal("策略登记要留审计")
	}

	// 读一条：app_ids 输出空数组而不是 null。
	rec = f.do(t, http.MethodGet, "/api/v1/backup/policies/"+testPolicyID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("读策略失败: %d %s", rec.Code, rec.Body.String())
	}
	var got backupPolicyDTO
	if err := json.Unmarshal(parseResult(t, rec).Data, &got); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if got.AppIDs == nil {
		t.Fatal("app_ids 要输出 [] 而不是 null")
	}

	// 修改要带 version。
	f.mgr.policy = policyView(testPolicyID, 2, pol)
	rec = f.do(t, http.MethodPatch, "/api/v1/backup/policies/"+testPolicyID, string(body))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("没有 version 状态码 = %d，期望 400", rec.Code)
	}
	withVersion, _ := json.Marshal(map[string]any{
		"name": pol.Name, "repository_id": pol.RepositoryID,
		"sources": pol.Sources, "consistency_mode": string(pol.ConsistencyMode),
		"retention": pol.Retention, "version": 2,
	})
	rec = f.do(t, http.MethodPatch, "/api/v1/backup/policies/"+testPolicyID, string(withVersion))
	if rec.Code != http.StatusOK {
		t.Fatalf("修改策略失败: %d %s", rec.Code, rec.Body.String())
	}
	if f.mgr.updV != 2 {
		t.Fatalf("version 没带下去：%d", f.mgr.updV)
	}

	// 删除：200 + 空对象（统一信封，不用 204），且审计写清"没删仓库与快照"。
	rec = f.do(t, http.MethodDelete, "/api/v1/backup/policies/"+testPolicyID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("删除状态码 = %d，期望 200", rec.Code)
	}
	if f.mgr.deletedID != testPolicyID {
		t.Fatal("删除的 id 不对")
	}
	if !slices.Contains(readAuditActions(t, f.db), audit.ActionBackupPolicyDelete) {
		t.Fatal("删除策略要留审计")
	}
}

// TestPolicyRejectsInconsistentAppList：stop_managed_apps 必须给 app_ids，
// files_only 不许给——这是 backup.Policy.Normalize 的规则，接口层不能放宽。
func TestPolicyRejectsInconsistentAppList(t *testing.T) {
	f := newBackupFixture(t, mgrWithRepository())
	cases := []string{
		`{"name":"x","repository_id":"` + testRepoID + `","sources":[{"root_id":"r","relative_path":"a"}],"consistency_mode":"stop_managed_apps"}`,
		`{"name":"x","repository_id":"` + testRepoID + `","sources":[{"root_id":"r","relative_path":"a"}],"consistency_mode":"files_only","app_ids":["app_0123456789abcdef0123456789abcdef"]}`,
	}
	for _, body := range cases {
		rec := f.do(t, http.MethodPost, "/api/v1/backup/policies", body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s 状态码 = %d，期望 400（响应 %s）", body, rec.Code, rec.Body.String())
		}
	}
}

// TestListSnapshotsRequiresRepository：没有跨仓库的快照列表。
func TestListSnapshotsRequiresRepository(t *testing.T) {
	f := newBackupFixture(t, mgrWithRepository())
	rec := f.do(t, http.MethodGet, "/api/v1/backup/snapshots", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400", rec.Code)
	}
	if f.mgr.gotRepoID != "" {
		t.Fatal("参数不全就不该去问代理")
	}
}

// TestListSnapshotsNoSize：列表不带大小，registered=false 是一种状态。
func TestListSnapshotsNoSize(t *testing.T) {
	f := newBackupFixture(t, mgrWithRepository())
	f.mgr.page = agentclient.BackupSnapshotPage{
		RepositoryID: testRepoID,
		Registered:   true,
		Snapshots: []agentclient.BackupSnapshot{{
			SnapshotID: testSnapshotID,
			ShortID:    "11111111",
			Time:       timeAt(),
			Paths:      []string{"/srv/logs"},
			Tags:       []string{"upanel"},
		}},
		SizeIncluded: false,
	}
	rec := f.do(t, http.MethodGet, "/api/v1/backup/snapshots?repository_id="+testRepoID+"&policy_id="+testPolicyID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，响应 = %s", rec.Code, rec.Body.String())
	}
	if f.mgr.gotPolicy != testPolicyID {
		t.Fatalf("策略过滤没带下去：%q", f.mgr.gotPolicy)
	}
	raw := parseResult(t, rec).Data
	if strings.Contains(string(raw), "size_bytes") {
		t.Fatalf("列表里不该有 size_bytes（每条要单独跑一次 restic stats）: %s", raw)
	}
	if !strings.Contains(string(raw), `"size_included":false`) {
		t.Fatalf("要说清这次没带大小: %s", raw)
	}

	// 没登记仓库：registered=false，仍然是 200。
	f.mgr.page = agentclient.BackupSnapshotPage{RepositoryID: testRepoID, Registered: false}
	rec = f.do(t, http.MethodGet, "/api/v1/backup/snapshots?repository_id="+testRepoID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("没登记仓库状态码 = %d，期望 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"registered":false`) {
		t.Fatalf("要说清是仓库不在，而不是没有快照: %s", rec.Body.String())
	}
	// 空列表要是 [] 而不是 null。
	if !strings.Contains(rec.Body.String(), `"items":[]`) {
		t.Fatalf("空列表要写成 []: %s", rec.Body.String())
	}
}

// TestGetSnapshotDetail：详情才带大小；缺 repository_id 或短 id 都是 400。
func TestGetSnapshotDetail(t *testing.T) {
	f := newBackupFixture(t, mgrWithRepository())
	size, count := int64(4096), int64(12)
	f.mgr.detail = agentclient.BackupSnapshotDetail{
		BackupSnapshot: agentclient.BackupSnapshot{
			SnapshotID: testSnapshotID, Time: timeAt(),
			Paths: []string{"/srv/logs"}, Tags: []string{"upanel"},
			SizeBytes: &size, FileCount: &count,
			ConsistencyMode: string(backup.ConsistencyStopManagedApps),
		},
		Registered: true,
		Found:      true,
	}
	rec := f.do(t, http.MethodGet,
		"/api/v1/backup/snapshots/"+testSnapshotID+"?repository_id="+testRepoID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，响应 = %s", rec.Code, rec.Body.String())
	}
	var out backupSnapshotDetailResponse
	if err := json.Unmarshal(parseResult(t, rec).Data, &out); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	// 大小按十进制字符串输出（大计数器不用 Number），不是数字。
	if out.Snapshot == nil || out.Snapshot.SizeBytes == nil || *out.Snapshot.SizeBytes != "4096" {
		t.Fatalf("详情要带大小（十进制字符串）：%+v", out.Snapshot)
	}
	if out.Snapshot.FileCount == nil || *out.Snapshot.FileCount != "12" {
		t.Fatalf("详情要带文件数（十进制字符串）：%+v", out.Snapshot)
	}
	// 恢复验证还没做：字段存在且为 null，不是假装已验证。
	if out.Snapshot.RestoreVerifiedAt != nil {
		t.Fatal("P12.3 之前 restore_verified_at 恒为 null")
	}

	if rec := f.do(t, http.MethodGet, "/api/v1/backup/snapshots/"+testSnapshotID, ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("缺 repository_id 状态码 = %d，期望 400", rec.Code)
	}
	if rec := f.do(t, http.MethodGet,
		"/api/v1/backup/snapshots/11111111?repository_id="+testRepoID, ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("短 id 状态码 = %d，期望 400（短 id 不唯一）", rec.Code)
	}
}

// TestBackupRunExpandIsRejectedWhenAppsSupplied：调用方不能自己给 apps。
func TestBackupRunExpandIsRejectedWhenAppsSupplied(t *testing.T) {
	f := newBackupFixture(t, mgrWithRepository())
	body := `{"action":"backup.run","params":{"backup_policy_id":"` + testPolicyID +
		`","version":1,"apps":[{"app_id":"app_0123456789abcdef0123456789abcdef","kind":"systemd","object":"x.service"}]}}`
	rec := f.do(t, http.MethodPost, "/api/v1/operations/preflight", body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400（响应 %s）", rec.Code, rec.Body.String())
	}
}

// TestBackupRunExpandPolicyNotFound：策略不在镜像里是 404。
func TestBackupRunExpandPolicyNotFound(t *testing.T) {
	f := newBackupFixture(t, mgrWithRepository())
	body := `{"action":"backup.run","params":{"backup_policy_id":"` + testPolicyID + `","version":1}}`
	rec := f.do(t, http.MethodPost, "/api/v1/operations/preflight", body)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("状态码 = %d，期望 404（响应 %s）", rec.Code, rec.Body.String())
	}
	if code := errorCode(t, rec); code != string(httpx.CodeNotFound) {
		t.Fatalf("错误码 = %q", code)
	}
}

// TestBackupRunExpandUnresolvedApp：策略要求停的应解析不出运行对象时，422 阻塞。
//
// 这条是 P12.2 最要紧的一条：少停一个应用的备份照样能建成，但它会被打上
// stop_managed_apps 的标签，而实际有一份文件是在应用写着的中间态下读的。
func TestBackupRunExpandUnresolvedApp(t *testing.T) {
	mgr := mgrWithRepository()
	pol := samplePolicy()
	pol.ConsistencyMode = backup.ConsistencyStopManagedApps
	pol.AppIDs = []string{"app_0123456789abcdef0123456789abcdef"}
	mgr.policy = policyView(testPolicyID, 1, pol)

	f := newBackupFixture(t, mgr)
	body := `{"action":"backup.run","params":{"backup_policy_id":"` + testPolicyID + `","version":1}}`
	rec := f.do(t, http.MethodPost, "/api/v1/operations/preflight", body)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("状态码 = %d，期望 422（响应 %s）", rec.Code, rec.Body.String())
	}
	if code := errorCode(t, rec); code != string(httpx.CodeDependencyBlocked) {
		t.Fatalf("错误码 = %q，期望 DEPENDENCY_BLOCKED", code)
	}
	// 提示里必须点名是哪个应用，否则管理员不知道该去修哪条绑定。
	if !strings.Contains(rec.Body.String(), "app_0123456789abcdef0123456789abcdef") {
		t.Fatalf("要说清是哪个应用解析不出：%s", rec.Body.String())
	}
}

// TestBackupRunExpandComposeApp：compose 部署不能当停机对象，理由要说清。
func TestBackupRunExpandComposeApp(t *testing.T) {
	mgr := mgrWithRepository()
	f := newBackupFixture(t, mgr)
	app, err := f.srv.opts.Apps.Create(context.Background(), apps.Definition{
		Name: "站点", Type: apps.TypeCompose,
		Binding: apps.Binding{DeploymentID: "dep_0123456789abcdef0123456789abcdef"},
	})
	if err != nil {
		t.Fatalf("建应用失败: %v", err)
	}
	pol := samplePolicy()
	pol.ConsistencyMode = backup.ConsistencyStopManagedApps
	pol.AppIDs = []string{app.ID}
	mgr.policy = policyView(testPolicyID, 1, pol)

	body := `{"action":"backup.run","params":{"backup_policy_id":"` + testPolicyID + `","version":1}}`
	rec := f.do(t, http.MethodPost, "/api/v1/operations/preflight", body)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("状态码 = %d，期望 422（响应 %s）", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "compose") {
		t.Fatalf("理由要说是 compose：%s", rec.Body.String())
	}
}

// TestBackupRunExpandResolvesApps：正常展开——app_ids 变成 kind+object，策略版本带下去。
func TestBackupRunExpandResolvesApps(t *testing.T) {
	mgr := mgrWithRepository()
	f := newBackupFixture(t, mgr)
	svcApp, err := f.srv.opts.Apps.Create(context.Background(), apps.Definition{
		Name: "站点服务", Type: apps.TypeSystemd,
		Binding: apps.Binding{Unit: testUnit},
	})
	if err != nil {
		t.Fatalf("建应用失败: %v", err)
	}
	boxApp, err := f.srv.opts.Apps.Create(context.Background(), apps.Definition{
		Name: "站点容器", Type: apps.TypeDocker,
		Binding: apps.Binding{ContainerID: strings.ToUpper(testContainer)},
	})
	if err != nil {
		t.Fatalf("建应用失败: %v", err)
	}
	pol := samplePolicy()
	pol.ConsistencyMode = backup.ConsistencyStopManagedApps
	pol.AppIDs = []string{svcApp.ID, boxApp.ID}
	mgr.policy = policyView(testPolicyID, 4, pol)

	body := `{"action":"backup.run","params":{"backup_policy_id":"` + testPolicyID + `","version":4}}`
	rec := f.do(t, http.MethodPost, "/api/v1/operations/preflight", body)
	// 预检本身会去问代理（这里没有代理），所以只要不是展开阶段的错误就说明参数过去了。
	// 展开失败会给出 4xx 与明确错误码；代理不可用是 503 AGENT_UNAVAILABLE。
	if rec.Code == http.StatusBadRequest || rec.Code == http.StatusUnprocessableEntity {
		t.Fatalf("展开不该失败: %d %s", rec.Code, rec.Body.String())
	}
	if code := errorCode(t, rec); code != string(httpx.CodeAgentUnavailable) {
		t.Fatalf("错误码 = %q，期望 AGENT_UNAVAILABLE（说明参数已展开、卡在代理那一步）", code)
	}

	expanded := backupRunExpandedParams{
		PolicyID: testPolicyID,
		Version:  4,
		Apps: []backupRunAppParam{
			{AppID: svcApp.ID, Kind: backup.AppKindSystemd, Object: testUnit},
			{AppID: boxApp.ID, Kind: backup.AppKindContainer, Object: testContainer},
		},
	}
	raw, err := json.Marshal(expanded)
	if err != nil {
		t.Fatal(err)
	}
	// 直接调展开函数，核对它交给代理的东西（上面那次 HTTP 请求已经证明它不报错）。
	got, ok := f.srv.expandBackupRun(newGinProbe(t), json.RawMessage(
		`{"backup_policy_id":"`+testPolicyID+`","version":4}`))
	if !ok {
		t.Fatal("展开失败")
	}
	if string(got) != string(raw) {
		t.Fatalf("展开结果不对：\n得到 %s\n期望 %s", got, raw)
	}
}

// TestBackupRunExpandFilesOnly：files_only 策略展开出空数组（不是缺字段）。
func TestBackupRunExpandFilesOnly(t *testing.T) {
	mgr := mgrWithRepository()
	pol := samplePolicy()
	mgr.policy = policyView(testPolicyID, 1, pol)
	f := newBackupFixture(t, mgr)

	got, ok := f.srv.expandBackupRun(newGinProbe(t), json.RawMessage(
		`{"backup_policy_id":"`+testPolicyID+`","version":1}`))
	if !ok {
		t.Fatal("展开失败")
	}
	if !strings.Contains(string(got), `"apps":[]`) {
		t.Fatalf("files_only 要展开出空数组: %s", got)
	}
}

func TestBackupRestoreExpandServerOwnsAppSet(t *testing.T) {
	f := newBackupFixture(t, mgrWithRepository())
	svc, err := f.srv.opts.Apps.Create(context.Background(), apps.Definition{Name: "service", Type: apps.TypeSystemd,
		Binding: apps.Binding{Unit: testUnit}})
	if err != nil {
		t.Fatal(err)
	}
	raw := `{"repository_id":"` + testRepoID + `","snapshot_id":"` + testSnapshotID + `","target_root_id":"root1","relative_path":"restored","overwrite":true}`
	got, ok := f.srv.expandBackupRestore(newGinProbe(t), json.RawMessage(raw))
	if !ok {
		t.Fatal("可停止绑定的覆盖恢复展开失败")
	}
	var expanded backupRestoreExpandedParams
	if err := json.Unmarshal(got, &expanded); err != nil {
		t.Fatal(err)
	}
	if len(expanded.Apps) != 1 || expanded.Apps[0].AppID != svc.ID || expanded.Apps[0].Object != testUnit {
		t.Fatalf("覆盖恢复没有由服务端展开应用: %+v", expanded.Apps)
	}

	probe := newGinProbe(t)
	withCallerApps := strings.TrimSuffix(raw, "}") + `,"apps":[]}`
	if _, ok := f.srv.expandBackupRestore(probe, json.RawMessage(withCallerApps)); ok || probe.Writer.Status() != http.StatusBadRequest {
		t.Fatalf("调用方不得自行指定 apps: ok=%v status=%d", ok, probe.Writer.Status())
	}

	nonOverwrite := strings.Replace(raw, `"overwrite":true`, `"overwrite":false`, 1)
	got, ok = f.srv.expandBackupRestore(newGinProbe(t), json.RawMessage(nonOverwrite))
	if !ok || !strings.Contains(string(got), `"apps":[]`) {
		t.Fatalf("新目录恢复不应展开停机集合: %s, %v", got, ok)
	}
}

func TestBackupRestoreExpandBlocksUnstoppableBinding(t *testing.T) {
	f := newBackupFixture(t, mgrWithRepository())
	if _, err := f.srv.opts.Apps.Create(context.Background(), apps.Definition{Name: "compose", Type: apps.TypeCompose,
		Binding: apps.Binding{DeploymentID: "dep_0123456789abcdef0123456789abcdef"}}); err != nil {
		t.Fatal(err)
	}
	probe := newGinProbe(t)
	raw := `{"repository_id":"` + testRepoID + `","snapshot_id":"` + testSnapshotID + `","target_root_id":"root1","relative_path":"restored","overwrite":true}`
	if _, ok := f.srv.expandBackupRestore(probe, json.RawMessage(raw)); ok || probe.Writer.Status() != http.StatusUnprocessableEntity {
		t.Fatalf("无法安全停止的绑定必须阻塞覆盖恢复: ok=%v status=%d", ok, probe.Writer.Status())
	}
}

// TestBackupErrorMapping：代理拒绝的其余状态码原样透出，未知码不当 400 糊过去。
func TestBackupErrorMapping(t *testing.T) {
	f := newBackupFixture(t, mgrWithRepository())
	f.mgr.repoErr = &agentclient.UnavailableError{Err: errors.New("socket 不存在")}
	rec := f.do(t, http.MethodGet, "/api/v1/backup/repositories", "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("代理不可用状态码 = %d，期望 503", rec.Code)
	}

	f.mgr.repoErr = &agentclient.Error{Status: 400, Code: "TOTALLY_UNKNOWN", Message: "???"}
	rec = f.do(t, http.MethodGet, "/api/v1/backup/repositories", "")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("未知错误码状态码 = %d，期望 500（不猜 400）", rec.Code)
	}
}

// 测试里用的固定时间。
func timeAt() time.Time { return time.Date(2026, 10, 24, 3, 2, 0, 0, time.UTC) }

// readAuditActions 读出全部审计动作名（按写入顺序）。
func readAuditActions(t *testing.T, db *store.DB) []string {
	t.Helper()
	rows, err := db.SQL().QueryContext(t.Context(), `SELECT action FROM audit_events ORDER BY id`)
	if err != nil {
		t.Fatalf("读审计失败: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var action string
		if err := rows.Scan(&action); err != nil {
			t.Fatalf("读审计行失败: %v", err)
		}
		out = append(out, action)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("读审计失败: %v", err)
	}
	return out
}

// newGinProbe 造一个只用来直接调展开函数的 gin 上下文。
//
// 展开函数会往 c.Writer 写错误响应，这里给它一个 httptest 记录器就够。
func newGinProbe(t *testing.T) *gin.Context {
	t.Helper()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/operations/preflight", nil)
	return c
}

// policyView 把一条 backup.Policy 摆成镜像里的扁平形状。
//
// agentclient.BackupPolicy 的字段就是内部策略摊开的那几项（见它的注释），
// 这里按同一口径拼，省得在用例里重复七行赋值。
func policyView(id string, version int64, pol backup.Policy) agentclient.BackupPolicy {
	return agentclient.BackupPolicy{
		ID:              id,
		Name:            pol.Name,
		Version:         version,
		RepositoryID:    pol.RepositoryID,
		Sources:         pol.Sources,
		ConsistencyMode: string(pol.ConsistencyMode),
		AppIDs:          pol.AppIDs,
		Retention:       pol.Retention,
	}
}
