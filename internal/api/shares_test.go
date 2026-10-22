package api

// 共享路由与秘密通道的 HTTP 层测试（P08.1）。
//
// 这一层只钉四件事：
//   1. 权限（CONTRACTS.md 第 7 节：三条读路由整组 admin，秘密上传 admin+CSRF+再认证）；
//   2. 形状（能力与列表一起返回、空列表是 []、不存在的共享是 404、标识先过形状检查）；
//   3. 错误翻译（代理拒绝原样带出、代理不可用是 503）；
//   4. 秘密通道的**单向性**：明文只出现在请求里，不出现在响应、审计或错误信息里。
//
// 共享判决与执行本身由 internal/shares 与 internal/agent 的测试覆盖；
// 真实的 Samba/ACL 行为只在独立 Ubuntu 虚拟机上验证（见 docs/evidence/P08）。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"upanel/internal/agentclient"
	"upanel/internal/audit"
	"upanel/internal/auth"
	"upanel/internal/config"
	"upanel/internal/httpx"
	"upanel/internal/platform"
	"upanel/internal/shares"
	"upanel/internal/store"
	"upanel/internal/version"
)

const (
	apiShareID   = "shr_0123456789abcdef0123456789abcdef"
	apiAccountID = "sac_fedcba9876543210fedcba9876543210"
	// apiSecretValue 是一段永远不会出现在任何响应或审计里的明文。
	apiSecretValue = "s3cret-for-samba-9"
)

// stubShares 是 SharesReader 的测试替身。
//
// 它记录最后一次调用的参数，让“形状不对的标识没有被打扰读取端”“明文确实传到了
// 代理”这类断言能落到事实上。
type stubShares struct {
	capability shares.Capability
	records    []shares.ShareRecord
	accounts   []shares.AccountRecord

	connections shares.ConnectionsReport
	lastConnID  string

	sharesErr   error
	accountsErr error
	connErr     error
	putErr      error

	calls       int
	lastID      string
	putCalls    int
	lastPutIn   agentclient.PutSecretInput
	putResponse agentclient.SecretRef
}

func (s *stubShares) Shares(context.Context) (shares.Capability, []shares.ShareRecord, error) {
	s.calls++
	if s.sharesErr != nil {
		return shares.Capability{}, nil, s.sharesErr
	}
	return s.capability, s.records, nil
}

func (s *stubShares) Share(_ context.Context, id string) (shares.Capability, shares.ShareRecord, bool, error) {
	s.calls++
	s.lastID = id
	if s.sharesErr != nil {
		return shares.Capability{}, shares.ShareRecord{}, false, s.sharesErr
	}
	for _, rec := range s.records {
		if rec.ID == id {
			return s.capability, rec, true, nil
		}
	}
	return s.capability, shares.ShareRecord{}, false, nil
}

func (s *stubShares) Accounts(context.Context) (shares.Capability, []shares.AccountRecord, error) {
	s.calls++
	if s.accountsErr != nil {
		return shares.Capability{}, nil, s.accountsErr
	}
	return s.capability, s.accounts, nil
}

func (s *stubShares) Connections(_ context.Context, id string) (shares.ConnectionsReport, error) {
	s.calls++
	s.lastConnID = id
	if s.connErr != nil {
		return shares.ConnectionsReport{}, s.connErr
	}
	return s.connections, nil
}

func (s *stubShares) PutSecret(_ context.Context, in agentclient.PutSecretInput) (agentclient.SecretRef, error) {
	s.calls++
	s.putCalls++
	s.lastPutIn = in
	if s.putErr != nil {
		return agentclient.SecretRef{}, s.putErr
	}
	return s.putResponse, nil
}

// availableCapability 是一份“Samba 齐了、但配置写入还不支持”的能力报告。
//
// config_write=unsupported 是 P08.1 的关键字段：它让页面没法把“定义准备好了”
// 显示成“共享可用了”。
func availableCapability() shares.Capability {
	return shares.Capability{
		Status:         shares.CapabilityAvailable,
		SambaVersion:   "4.19.2-Ubuntu",
		Tools:          shares.ToolPaths{Smbd: "/usr/sbin/smbd", Setfacl: "/usr/bin/setfacl"},
		ConfigPath:     shares.DefaultConfigPath,
		ConfigReadable: true,
		IncludeDir:     shares.DefaultIncludeDir,
		IncludeReady:   false,
		IncludePresent: false,
		ConfigWrite:    shares.ConfigWriteUnsupported,
		Notices:        []shares.Blocker{},
		CollectedAt:    time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC),
	}
}

// sampleShare 是一条“定义已启用、目录与 ACL 已准备、配置还没写”的共享。
func sampleShare() shares.ShareRecord {
	return shares.ShareRecord{
		ID: apiShareID,
		Definition: shares.ShareDefinition{
			Name:         "docs",
			Protocol:     shares.ProtocolSMB,
			VolumeID:     "vol_0123456789abcdef0123456789abcdef",
			RelativePath: "docs",
			Enabled:      true,
			ReadOnly:     false,
			AllowedCIDRs: []string{"10.0.0.0/24"},
			SMBUsers:     []shares.ShareUser{{AccountID: apiAccountID, Access: shares.AccessWrite}},
		},
		ConfigHash:        strings.Repeat("a", 64),
		Version:           "2",
		DirectoryPrepared: true,
		ConfigApplied:     false,
		CreatedAt:         time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC),
		UpdatedAt:         time.Date(2026, 11, 4, 9, 5, 0, 0, time.UTC),
	}
}

// sampleAccount 是一条受管理账号。它没有密码字段——DTO 上就没有。
func sampleAccount() shares.AccountRecord {
	return shares.AccountRecord{
		ID:          apiAccountID,
		Name:        "upanel-share-docs",
		Type:        shares.AccountTypeShare,
		Managed:     true,
		Disabled:    false,
		HasPassword: true,
		UID:         995,
		GID:         990,
		Shell:       "/usr/sbin/nologin",
		Home:        "/nonexistent",
		SecretRef:   "sec_0123456789abcdef0123456789abcdef",
		Version:     "2",
		CreatedAt:   time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC),
		UpdatedAt:   time.Date(2026, 11, 4, 9, 3, 0, 0, time.UTC),
	}
}

// sampleConnections 是一份“服务在跑、有一条活动连接”的报告。
func sampleConnections() shares.ConnectionsReport {
	return shares.ConnectionsReport{
		ShareID:   apiShareID,
		ShareName: "docs",
		Service:   shares.ServiceStateActive,
		Connections: []shares.Connection{{
			Share:       "docs",
			Machine:     "10.0.0.42",
			Username:    "upanel-share-docs",
			Dialect:     "SMB3_11",
			Signing:     "partial(AES-128-GMAC)",
			ConnectedAt: "Tue Oct 21 08:12:41 2026 UTC",
			OpenFiles:   2,
		}},
		CollectedAt: time.Date(2026, 11, 4, 9, 6, 0, 0, time.UTC),
		Notices:     []shares.Blocker{},
	}
}

// sharesReader 返回一份预置了共享与账号的替身。
func sharesReader() *stubShares {
	return &stubShares{
		capability:  availableCapability(),
		records:     []shares.ShareRecord{sampleShare()},
		accounts:    []shares.AccountRecord{sampleAccount()},
		connections: sampleConnections(),
		putResponse: agentclient.SecretRef{
			Ref:       "sec_fedcba9876543210fedcba9876543210",
			Purpose:   "smb_password",
			State:     "temporary",
			ExpiresAt: "2026-11-04T09:10:00Z",
		},
	}
}

// sharesFixture 构造一个接上替身读取端的服务，并返回管理员会话与 CSRF 令牌。
func sharesFixture(t *testing.T, reader SharesReader) (*Server, *http.Cookie, string, *auth.Service, *store.DB) {
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
		AllowedOrigins:  testOrigins(),
		Shares:          reader,
	})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	seedTestUser(t, srv.opts.Auth, testUser, testUserPass)
	cookie, csrf := loginOK(t, srv)
	return srv, cookie, csrf, srv.opts.Auth, db
}

// sharesDo 以指定会话发送请求（cookie 为 nil 表示未登录）。
func sharesDo(t *testing.T, srv *Server, cookie *http.Cookie, csrf, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Origin", testOrigin)
	if cookie != nil {
		req.Header.Set("Cookie", cookie.String())
	}
	if method != http.MethodGet && method != http.MethodHead {
		if csrf != "" {
			req.Header.Set(CSRFTokenHeader, csrf)
		}
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// TestShareRoutesRequireSessionAndAdmin 三条读路由都要登录，并且都只给 admin。
func TestShareRoutesRequireSessionAndAdmin(t *testing.T) {
	reader := sharesReader()
	srv, cookie, csrf, authSvc, _ := sharesFixture(t, reader)

	paths := []string{
		"/api/v1/shares",
		"/api/v1/shares/" + apiShareID,
		"/api/v1/shares/" + apiShareID + "/connections",
		"/api/v1/share-accounts",
	}
	for _, path := range paths {
		if rec := sharesDo(t, srv, nil, "", http.MethodGet, path, ""); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: 未登录状态码 = %d，期望 401", path, rec.Code)
		}
	}

	// viewer（CONTRACTS.md 第 7 节：viewer 只在首页看数量与健康摘要）
	if _, err := authSvc.CreateUser(t.Context(), auth.CreateUserInput{
		Username: "alice", DisplayName: "alice", Role: auth.RoleViewer, Password: "alice-horse-9",
	}); err != nil {
		t.Fatalf("建 viewer 失败: %v", err)
	}
	viewerCookie, viewerCSRF := loginAsUser(t, srv, "alice", "alice-horse-9")
	for _, path := range paths {
		rec := sharesDo(t, srv, viewerCookie, viewerCSRF, http.MethodGet, path, "")
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: viewer 状态码 = %d，期望 403（%s）", path, rec.Code, rec.Body.String())
		}
	}

	// 同一批地址 admin 能拿到内容（否则上面的 403 也可能只是路由写错了）。
	for _, path := range paths {
		if rec := sharesDo(t, srv, cookie, csrf, http.MethodGet, path, ""); rec.Code != http.StatusOK {
			t.Errorf("%s: admin 状态码 = %d，期望 200（%s）", path, rec.Code, rec.Body.String())
		}
	}
}

// TestShareListShape 列表带能力报告，且把“已启用”与“已生效”分开透出。
func TestShareListShape(t *testing.T) {
	srv, cookie, csrf, _, _ := sharesFixture(t, sharesReader())
	rec := sharesDo(t, srv, cookie, csrf, http.MethodGet, "/api/v1/shares", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d（%s）", rec.Code, rec.Body.String())
	}
	var body struct {
		Data shareListResponse `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if !body.Data.Capability.Available() {
		t.Errorf("能力 = %+v", body.Data.Capability)
	}
	// 能力必须自带 config_write=unsupported：P08.1 不写 Samba 配置。
	if body.Data.Capability.ConfigWrite != shares.ConfigWriteUnsupported {
		t.Errorf("config_write = %q，P08.1 必须是 unsupported", body.Data.Capability.ConfigWrite)
	}
	if len(body.Data.Shares) != 1 {
		t.Fatalf("共享数 = %d", len(body.Data.Shares))
	}
	got := body.Data.Shares[0]
	if got.ID != apiShareID || got.Definition.Name != "docs" {
		t.Errorf("共享字段不对：%+v", got)
	}
	if !got.DirectoryPrepared || got.ConfigApplied {
		t.Errorf("directory_prepared/config_applied = %v/%v，P08.1 只能是「目录已备、配置未写」",
			got.DirectoryPrepared, got.ConfigApplied)
	}
}

// TestShareListEmptyIsArray 空列表是 [] 而不是 null。
func TestShareListEmptyIsArray(t *testing.T) {
	reader := sharesReader()
	reader.records = nil
	reader.accounts = nil
	srv, cookie, csrf, _, _ := sharesFixture(t, reader)

	for _, tc := range []struct{ path, key string }{
		{"/api/v1/shares", "shares"},
		{"/api/v1/share-accounts", "accounts"},
	} {
		rec := sharesDo(t, srv, cookie, csrf, http.MethodGet, tc.path, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: 状态码 = %d（%s）", tc.path, rec.Code, rec.Body.String())
		}
		var body struct {
			Data map[string]json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s: 响应不是合法 JSON: %v", tc.path, err)
		}
		raw, ok := body.Data[tc.key]
		if !ok {
			t.Fatalf("%s: 响应缺少 %q 字段", tc.path, tc.key)
		}
		if strings.TrimSpace(string(raw)) == "null" {
			t.Errorf("%s: 空列表序列化成了 null：%s", tc.path, rec.Body.String())
		}
	}
}

// TestShareAccountsHideSecretRef 账号列表里不能出现 secret_ref，也不能出现任何
// 密码形态的字段。这条测试是“DTO 上没有那个字段”的回归保护。
func TestShareAccountsHideSecretRef(t *testing.T) {
	srv, cookie, csrf, _, _ := sharesFixture(t, sharesReader())
	rec := sharesDo(t, srv, cookie, csrf, http.MethodGet, "/api/v1/share-accounts", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d（%s）", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, banned := range []string{"secret_ref", "sec_", "password_hash", "\"password\""} {
		if strings.Contains(body, banned) {
			t.Errorf("账号列表里出现了 %q：%s", banned, body)
		}
	}
	if !strings.Contains(body, "has_password") {
		t.Errorf("has_password 应该在（它只回答“设过没有”）：%s", body)
	}
}

// TestGetShareNotFoundAndShape 详情：命中 200，未知 404，形状不对 400 且不打扰读取端。
func TestGetShareNotFoundAndShape(t *testing.T) {
	reader := sharesReader()
	srv, cookie, csrf, _, _ := sharesFixture(t, reader)

	rec := sharesDo(t, srv, cookie, csrf, http.MethodGet, "/api/v1/shares/"+apiShareID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d（%s）", rec.Code, rec.Body.String())
	}
	var detail struct {
		Data shareDetailResponse `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if detail.Data.Share.ID != apiShareID {
		t.Errorf("共享 = %+v", detail.Data.Share)
	}
	// 详情也要带能力：页面不该为“这条共享现在生效了吗”再发一次请求。
	if detail.Data.Capability.ConfigWrite != shares.ConfigWriteUnsupported {
		t.Errorf("详情缺能力报告：%+v", detail.Data.Capability)
	}

	other := "shr_fedcba9876543210fedcba9876543210"
	rec = sharesDo(t, srv, cookie, csrf, http.MethodGet, "/api/v1/shares/"+other, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("未知共享状态码 = %d，期望 404（%s）", rec.Code, rec.Body.String())
	}
	if got := parseResult(t, rec); got.Error == nil || got.Error.Code != "NOT_FOUND" {
		t.Errorf("错误码 = %+v", got.Error)
	}

	// 形状不对的标识在本层就被挡住：它要进日志与审计，不能原样透传给代理。
	for _, id := range []string{"..", "shr_X", "shr_0123456789ABCDEF0123456789abcdef", "shr_123"} {
		before := reader.calls
		rec = sharesDo(t, srv, cookie, csrf, http.MethodGet, "/api/v1/shares/"+id, "")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%q: 状态码 = %d，期望 400（%s）", id, rec.Code, rec.Body.String())
		}
		if reader.calls != before {
			t.Errorf("%q: 形状不对的标识被透传给了读取端", id)
		}
	}
}

// TestGetShareConnections 连接状态是独立子路径，带服务状态，形状不对先挡住。
func TestGetShareConnections(t *testing.T) {
	reader := sharesReader()
	srv, cookie, csrf, _, _ := sharesFixture(t, reader)

	rec := sharesDo(t, srv, cookie, csrf, http.MethodGet,
		"/api/v1/shares/"+apiShareID+"/connections", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d（%s）", rec.Code, rec.Body.String())
	}
	var body struct {
		Data shares.ConnectionsReport `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if reader.lastConnID != apiShareID {
		t.Errorf("读取端收到的标识 = %q", reader.lastConnID)
	}
	if body.Data.Service != shares.ServiceStateActive {
		t.Errorf("service = %q：空列表与“服务没在跑”必须能分开", body.Data.Service)
	}
	if len(body.Data.Connections) != 1 || body.Data.Connections[0].Machine != "10.0.0.42" {
		t.Errorf("连接 = %+v", body.Data.Connections)
	}

	// 代理说“没有这条共享”是 404，不是 500：页面据此删掉这一行，而不是去查面板。
	reader.connErr = &shares.RemoteError{
		Status: http.StatusNotFound, Code: httpx.CodeNotFound, Message: "共享不存在",
	}
	rec = sharesDo(t, srv, cookie, csrf, http.MethodGet,
		"/api/v1/shares/"+apiShareID+"/connections", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("未知共享状态码 = %d，期望 404（%s）", rec.Code, rec.Body.String())
	}
	reader.connErr = nil

	// 形状不对的标识在本层就被挡住，不会透传给代理。
	for _, bad := range []string{"..", "shr_X", "shr_123", "docs"} {
		before := reader.calls
		rec = sharesDo(t, srv, cookie, csrf, http.MethodGet,
			"/api/v1/shares/"+bad+"/connections", "")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%q: 状态码 = %d，期望 400（%s）", bad, rec.Code, rec.Body.String())
		}
		if reader.calls != before {
			t.Errorf("%q: 形状不对的标识被透传给了读取端", bad)
		}
	}
}

// TestShareRoutesAgentUnavailable 没配置代理时是 503，不是空列表。
func TestShareRoutesAgentUnavailable(t *testing.T) {
	srv, cookie, csrf, _, _ := sharesFixture(t, nil)
	for _, path := range []string{
		"/api/v1/shares",
		"/api/v1/shares/" + apiShareID,
		"/api/v1/share-accounts",
	} {
		rec := sharesDo(t, srv, cookie, csrf, http.MethodGet, path, "")
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s: 状态码 = %d，期望 503（%s）", path, rec.Code, rec.Body.String())
		}
		if got := parseResult(t, rec); got.Error == nil || got.Error.Code != string(httpx.CodeAgentUnavailable) {
			t.Fatalf("%s: 错误码 = %+v，期望 AGENT_UNAVAILABLE", path, got.Error)
		}
	}
}

// TestShareListRemoteErrorPassthrough 代理的明确拒绝原样带出（不能压成 500/503）。
func TestShareListRemoteErrorPassthrough(t *testing.T) {
	reader := sharesReader()
	reader.sharesErr = &shares.RemoteError{
		Status: http.StatusConflict, Code: httpx.Code("DEPENDENCY_BLOCKED"), Message: "托管目录不可用",
	}
	srv, cookie, csrf, _, _ := sharesFixture(t, reader)
	rec := sharesDo(t, srv, cookie, csrf, http.MethodGet, "/api/v1/shares", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("状态码 = %d，期望 409（%s）", rec.Code, rec.Body.String())
	}
	if got := parseResult(t, rec); got.Error == nil || got.Error.Code != "DEPENDENCY_BLOCKED" {
		t.Errorf("错误码 = %+v", got.Error)
	}
}

// createSecretBody 拼一份上传请求体。
func createSecretBody(purpose, value, reference string) string {
	body, _ := json.Marshal(map[string]string{
		"purpose": purpose, "value": value, "reference": reference,
	})
	return string(body)
}

// TestCreateSecretRequiresAdminCSRFAndReauth 秘密通道的四道门：登录、admin、CSRF、再认证。
func TestCreateSecretRequiresAdminCSRFAndReauth(t *testing.T) {
	reader := sharesReader()
	srv, cookie, csrf, authSvc, _ := sharesFixture(t, reader)
	body := createSecretBody("smb_password", apiSecretValue, "")

	// 未登录
	if rec := sharesDo(t, srv, nil, "", http.MethodPost, "/api/v1/secrets", body); rec.Code != http.StatusUnauthorized {
		t.Errorf("未登录状态码 = %d，期望 401", rec.Code)
	}

	// 已登录但没有再认证：这一条必须在**调用代理之前**拦住。
	rec := sharesDo(t, srv, cookie, csrf, http.MethodPost, "/api/v1/secrets", body)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("未再认证状态码 = %d，期望 403（%s）", rec.Code, rec.Body.String())
	}
	if got := parseResult(t, rec); got.Error == nil || got.Error.Code != string(httpx.CodeReauthRequired) {
		t.Fatalf("错误码 = %+v，期望 REAUTH_REQUIRED", got.Error)
	}
	if reader.putCalls != 0 {
		t.Fatalf("没有再认证却调用了代理 %d 次", reader.putCalls)
	}

	// viewer：即使再认证过也不能上传秘密
	if _, err := authSvc.CreateUser(t.Context(), auth.CreateUserInput{
		Username: "alice", DisplayName: "alice", Role: auth.RoleViewer, Password: "alice-horse-9",
	}); err != nil {
		t.Fatalf("建 viewer 失败: %v", err)
	}
	viewerCookie, viewerCSRF := loginAsUser(t, srv, "alice", "alice-horse-9")
	if rec := sharesDo(t, srv, viewerCookie, viewerCSRF, http.MethodPost, "/api/v1/secrets", body); rec.Code != http.StatusForbidden {
		t.Errorf("viewer 状态码 = %d，期望 403（%s）", rec.Code, rec.Body.String())
	}

	// 再认证之后缺 CSRF：仍然拒绝
	if rec := doReauth(t, srv, cookie, csrf); rec.Code != http.StatusOK {
		t.Fatalf("再认证失败: %d %s", rec.Code, rec.Body.String())
	}
	rec = sharesDo(t, srv, cookie, "", http.MethodPost, "/api/v1/secrets", body)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("缺 CSRF 状态码 = %d，期望 403（%s）", rec.Code, rec.Body.String())
	}
	if got := parseResult(t, rec); got.Error == nil || got.Error.Code != "CSRF_FAILED" {
		t.Errorf("错误码 = %+v，期望 CSRF_FAILED", got.Error)
	}
	if reader.putCalls != 0 {
		t.Fatalf("被拒绝的请求仍然调用了代理 %d 次", reader.putCalls)
	}
}

// doReauth 完成一次密码再认证。
func doReauth(t *testing.T, srv *Server, cookie *http.Cookie, csrf string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"password": testUserPass})
	return sharesDo(t, srv, cookie, csrf, http.MethodPost, "/api/v1/auth/reauth", string(body))
}

// TestCreateSecretValidates 形状不对的请求在本地就拒绝，不惊动代理。
func TestCreateSecretValidates(t *testing.T) {
	reader := sharesReader()
	srv, cookie, csrf, _, _ := sharesFixture(t, reader)
	if rec := doReauth(t, srv, cookie, csrf); rec.Code != http.StatusOK {
		t.Fatalf("再认证失败: %d %s", rec.Code, rec.Body.String())
	}

	cases := []struct{ name, body string }{
		{"未知字段", `{"purpose":"smb_password","value":"x","extra":1}`},
		{"缺用途", `{"value":"x"}`},
		{"用途形状不对", createSecretBody("SMB-Password", "x", "")},
		{"用途以数字开头", createSecretBody("9smb", "x", "")},
		{"用途带空格", createSecretBody("smb password", "x", "")},
		{"空明文", createSecretBody("smb_password", "", "")},
		{"请求体为空", ""},
	}
	for _, tc := range cases {
		rec := sharesDo(t, srv, cookie, csrf, http.MethodPost, "/api/v1/secrets", tc.body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: 状态码 = %d，期望 400（%s）", tc.name, rec.Code, rec.Body.String())
			continue
		}
		if strings.Contains(rec.Body.String(), apiSecretValue) {
			t.Errorf("%s: 错误响应里出现了明文", tc.name)
		}
	}
	if reader.putCalls != 0 {
		t.Fatalf("被本地拒绝的请求仍调用代理 %d 次", reader.putCalls)
	}
}

// TestCreateSecretSuccess 成功后只返回引用，明文既不回显也不进审计。
func TestCreateSecretSuccess(t *testing.T) {
	reader := sharesReader()
	srv, cookie, csrf, _, db := sharesFixture(t, reader)
	if rec := doReauth(t, srv, cookie, csrf); rec.Code != http.StatusOK {
		t.Fatalf("再认证失败: %d %s", rec.Code, rec.Body.String())
	}

	rec := sharesDo(t, srv, cookie, csrf, http.MethodPost, "/api/v1/secrets",
		createSecretBody("smb_password", apiSecretValue, " share_account:"+apiAccountID+" "))
	if rec.Code != http.StatusCreated {
		t.Fatalf("状态码 = %d，期望 201（%s）", rec.Code, rec.Body.String())
	}

	// 明文确实传到了代理（否则上面 201 也可能是空跑），引用去掉了首尾空白。
	if reader.putCalls != 1 {
		t.Fatalf("代理调用次数 = %d", reader.putCalls)
	}
	if reader.lastPutIn.Purpose != "smb_password" || reader.lastPutIn.Value != apiSecretValue {
		t.Errorf("传给代理的参数 = %+v", agentclient.PutSecretInput{Purpose: reader.lastPutIn.Purpose})
	}
	if reader.lastPutIn.Reference != "share_account:"+apiAccountID {
		t.Errorf("引用未去掉首尾空白：%q", reader.lastPutIn.Reference)
	}

	var body struct {
		Data createSecretResponse `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if body.Data.SecretRef != reader.putResponse.Ref || body.Data.State != "temporary" {
		t.Errorf("响应 = %+v", body.Data)
	}
	// 单向性：明文不许出现在响应里，任何字段都不行。
	if strings.Contains(rec.Body.String(), apiSecretValue) {
		t.Fatalf("响应里出现了明文: %s", rec.Body.String())
	}

	// 审计：记下了动作与用途，没记内容；secret_ref 这个键按秘密字段过滤掉了。
	var action, details string
	if err := db.SQL().QueryRowContext(t.Context(),
		`SELECT action, redacted_details FROM audit_events WHERE action = ?`,
		audit.ActionSecretCreate).Scan(&action, &details); err != nil {
		t.Fatalf("读取审计失败: %v", err)
	}
	if strings.Contains(details, apiSecretValue) {
		t.Fatalf("审计明细里出现了明文: %s", details)
	}
	if !strings.Contains(details, "smb_password") {
		t.Errorf("审计应记录用途: %s", details)
	}
	if strings.Contains(details, reader.putResponse.Ref) {
		t.Errorf("审计里出现了 secret_ref 原文（应按秘密字段过滤）: %s", details)
	}
}

// TestCreateSecretAgentRejects 代理拒绝原样带出，并且留下一条失败审计。
func TestCreateSecretAgentRejects(t *testing.T) {
	reader := sharesReader()
	reader.putErr = &shares.RemoteError{
		Status: http.StatusBadRequest, Code: httpx.CodeValidationError,
		Message: `用途 "totp_seed" 只能由代理内部流程写入`,
	}
	srv, cookie, csrf, _, db := sharesFixture(t, reader)
	if rec := doReauth(t, srv, cookie, csrf); rec.Code != http.StatusOK {
		t.Fatalf("再认证失败: %d %s", rec.Code, rec.Body.String())
	}

	rec := sharesDo(t, srv, cookie, csrf, http.MethodPost, "/api/v1/secrets",
		createSecretBody("totp_seed", apiSecretValue, ""))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400（%s）", rec.Code, rec.Body.String())
	}
	if got := parseResult(t, rec); got.Error == nil || !strings.Contains(got.Error.Message, "内部流程") {
		t.Fatalf("代理的错误信息没有带出: %+v", got.Error)
	}
	if strings.Contains(rec.Body.String(), apiSecretValue) {
		t.Fatalf("错误响应里出现了明文: %s", rec.Body.String())
	}

	var failureCount int
	if err := db.SQL().QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM audit_events WHERE action = ? AND outcome = 'failure'`,
		audit.ActionSecretCreate).Scan(&failureCount); err != nil {
		t.Fatalf("统计失败审计失败: %v", err)
	}
	if failureCount != 1 {
		t.Fatalf("失败审计条数 = %d，期望 1", failureCount)
	}
}
