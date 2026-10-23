package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"upanel/internal/audit"
	"upanel/internal/config"
	"upanel/internal/httpx"
	"upanel/internal/platform"
	"upanel/internal/store"
	"upanel/internal/version"
)

// apiResult 是测试里解析响应用的通用信封。
type apiResult struct {
	Data  json.RawMessage `json:"data"`
	Error *struct {
		Code    string         `json:"code"`
		Message string         `json:"message"`
		Details map[string]any `json:"details"`
	} `json:"error"`
}

func parseResult(t *testing.T, rec *httptest.ResponseRecorder) apiResult {
	t.Helper()
	var out apiResult
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是合法 JSON: %v（%s）", err, rec.Body.String())
	}
	return out
}

// adminFixture 是账号管理用例的固定装置：真实数据库、已登录的管理员、数据库句柄。
type adminFixture struct {
	srv    *Server
	cookie *http.Cookie
	csrf   string
	db     *store.DB
}

func newAdminFixture(t *testing.T) *adminFixture {
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
		Proxies:         newTestProxies(t, db),
		AllowedOrigins:  testOrigins(),
	})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	seedTestUser(t, srv.opts.Auth, testUser, testUserPass)
	cookie, csrf := loginOK(t, srv)
	return &adminFixture{srv: srv, cookie: cookie, csrf: csrf, db: db}
}

// do 以管理员会话 + CSRF 发送请求。
func (f *adminFixture) do(t *testing.T, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	return f.doAs(t, f.cookie, f.csrf, method, path, body)
}

// doAs 用指定会话发送请求（用于 viewer 越权用例）。
func (f *adminFixture) doAs(t *testing.T, cookie *http.Cookie, csrf, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Origin", testOrigin)
	req.Header.Set("Cookie", cookie.String())
	if method != http.MethodGet && method != http.MethodHead {
		req.Header.Set(CSRFTokenHeader, csrf)
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
	}
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	return rec
}

// createUser 通过 API 建账号并返回创建结果。
func (f *adminFixture) createUser(t *testing.T, username, role, password string) adminUserDTO {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"username":         username,
		"display_name":     username,
		"role":             role,
		"initial_password": password,
	})
	rec := f.do(t, http.MethodPost, "/api/v1/users", string(body))
	if rec.Code != http.StatusCreated {
		t.Fatalf("创建账号 %s 失败: %d %s", username, rec.Code, rec.Body.String())
	}
	var created adminUserDTO
	if err := json.Unmarshal(parseResult(t, rec).Data, &created); err != nil {
		t.Fatalf("解析创建结果失败: %v", err)
	}
	return created
}

// reauth 完成一次密码再认证。
func (f *adminFixture) reauth(t *testing.T, password string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"password": password})
	return f.do(t, http.MethodPost, "/api/v1/auth/reauth", string(body))
}

// loginAsUser 用指定账号登录并返回 Cookie 与 CSRF 令牌。
func loginAsUser(t *testing.T, srv *Server, username, password string) (*http.Cookie, string) {
	t.Helper()
	rec := doLogin(t, srv, loginBody(username, password), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s 登录失败: 状态码 = %d，响应 = %s", username, rec.Code, rec.Body.String())
	}
	var body struct {
		Data loginResponse `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("登录响应不是合法 JSON: %v", err)
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("登录应只下发一个 Cookie，实际 %d 个", len(cookies))
	}
	return cookies[0], body.Data.CSRFToken
}

func TestUsersRequireLoginAndAdmin(t *testing.T) {
	f := newAdminFixture(t)

	// 未登录：401（SEC-01）。
	req := httptest.NewRequest(http.MethodGet, "/api/v1/users", nil)
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("未登录访问 /users 状态码 = %d，期望 401", rec.Code)
	}

	// viewer 会话访问账号管理：403（SEC-02）。
	f.createUser(t, "alice", "viewer", "alice-horse-9")
	viewerCookie, viewerCSRF := loginAsUser(t, f.srv, "alice", "alice-horse-9")

	cases := []struct{ method, path, body string }{
		{http.MethodGet, "/api/v1/users", ""},
		{http.MethodPost, "/api/v1/users",
			`{"username":"bob","role":"viewer","initial_password":"bob-horse-99"}`},
		{http.MethodPatch, "/api/v1/users/1", `{"version":1,"display_name":"x"}`},
		{http.MethodPost, "/api/v1/users/1/password", `{"new_password":"another-horse-7"}`},
		{http.MethodDelete, "/api/v1/users/1", `{"version":1}`},
	}
	for _, tc := range cases {
		rec := f.doAs(t, viewerCookie, viewerCSRF, tc.method, tc.path, tc.body)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("viewer %s %s 状态码 = %d，期望 403", tc.method, tc.path, rec.Code)
		}
		if got := parseResult(t, rec); got.Error == nil || got.Error.Code != "FORBIDDEN" {
			t.Fatalf("viewer 越权错误码 = %+v，期望 FORBIDDEN", got.Error)
		}
	}

	// viewer 不能再认证（接口只对 admin 开放）。
	rec = f.doAs(t, viewerCookie, viewerCSRF, http.MethodPost, "/api/v1/auth/reauth",
		`{"password":"alice-horse-9"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("viewer 再认证状态码 = %d，期望 403", rec.Code)
	}
}

func TestUsersListIsRedacted(t *testing.T) {
	f := newAdminFixture(t)
	rec := f.do(t, http.MethodGet, "/api/v1/users", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，响应 = %s", rec.Code, rec.Body.String())
	}

	lower := strings.ToLower(rec.Body.String())
	for _, forbidden := range []string{"password", "argon2", "csrf", "session"} {
		if strings.Contains(lower, forbidden) {
			t.Fatalf("账号列表不应包含 %q: %s", forbidden, rec.Body.String())
		}
	}

	var list usersListResponse
	if err := json.Unmarshal(parseResult(t, rec).Data, &list); err != nil {
		t.Fatalf("解析列表失败: %v", err)
	}
	if len(list.Items) != 1 || list.Items[0].Username != testUser {
		t.Fatalf("账号列表 = %+v", list.Items)
	}
	if list.Items[0].Role != "admin" || list.Items[0].Version != 1 || list.Items[0].Disabled {
		t.Fatalf("账号字段不对: %+v", list.Items[0])
	}
	if list.NextCursor != nil {
		t.Fatalf("单页结果 next_cursor 必须为 null，实际 %q", *list.NextCursor)
	}
	if !strings.Contains(rec.Body.String(), `"next_cursor":null`) {
		t.Fatalf("响应必须显式带 next_cursor 字段: %s", rec.Body.String())
	}
}

func TestUsersListPagination(t *testing.T) {
	f := newAdminFixture(t)
	f.createUser(t, "alice", "viewer", "alice-horse-9")
	f.createUser(t, "bob", "viewer", "bob-horse-99")

	rec := f.do(t, http.MethodGet, "/api/v1/users?limit=2", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，响应 = %s", rec.Code, rec.Body.String())
	}
	var page1 usersListResponse
	if err := json.Unmarshal(parseResult(t, rec).Data, &page1); err != nil {
		t.Fatalf("解析第一页失败: %v", err)
	}
	if len(page1.Items) != 2 || page1.Items[0].Username != "admin" || page1.Items[1].Username != "alice" {
		t.Fatalf("第一页 = %+v", page1.Items)
	}
	if page1.NextCursor == nil {
		t.Fatal("还有下一页时 next_cursor 不能为 null")
	}

	rec = f.do(t, http.MethodGet, "/api/v1/users?limit=2&cursor="+*page1.NextCursor, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("翻页状态码 = %d，响应 = %s", rec.Code, rec.Body.String())
	}
	var page2 usersListResponse
	if err := json.Unmarshal(parseResult(t, rec).Data, &page2); err != nil {
		t.Fatalf("解析第二页失败: %v", err)
	}
	if len(page2.Items) != 1 || page2.Items[0].Username != "bob" || page2.NextCursor != nil {
		t.Fatalf("第二页 = %+v（next %v）", page2.Items, page2.NextCursor)
	}

	// 非法 limit / 游标：400 VALIDATION_ERROR，不是 500。
	for _, q := range []string{"?limit=0", "?limit=201", "?limit=abc", "?cursor=not-a-cursor"} {
		rec = f.do(t, http.MethodGet, "/api/v1/users"+q, "")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s 状态码 = %d，期望 400（响应 %s）", q, rec.Code, rec.Body.String())
		}
	}
}

func TestCreateUserAndLogin(t *testing.T) {
	f := newAdminFixture(t)

	body, _ := json.Marshal(map[string]any{
		"username": "alice", "display_name": "爱丽丝", "role": "viewer", "initial_password": "alice-horse-9",
	})
	rec := f.do(t, http.MethodPost, "/api/v1/users", string(body))
	if rec.Code != http.StatusCreated {
		t.Fatalf("状态码 = %d，响应 = %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "alice-horse-9") {
		t.Fatal("创建响应不能回显初始密码")
	}
	var created adminUserDTO
	if err := json.Unmarshal(parseResult(t, rec).Data, &created); err != nil {
		t.Fatalf("解析创建结果失败: %v", err)
	}
	if created.Username != "alice" || created.Role != "viewer" || created.Version != 1 || created.Disabled {
		t.Fatalf("创建结果 = %+v", created)
	}

	// 新账号能用初始密码登录。
	if cookie, _ := loginAsUser(t, f.srv, "alice", "alice-horse-9"); cookie == nil {
		t.Fatal("新账号应能登录")
	}

	// 重复用户名：422（业务条件不允许），不是 500。
	if rec = f.do(t, http.MethodPost, "/api/v1/users", string(body)); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("重复用户名状态码 = %d，期望 422（响应 %s）", rec.Code, rec.Body.String())
	}

	// 非法用户名（含注入形状）、未知角色、弱密码：400。
	for _, bad := range []string{
		`{"username":"Admin'--","role":"viewer","initial_password":"alice-horse-9"}`,
		`{"username":"bob","role":"root","initial_password":"alice-horse-9"}`,
		`{"username":"bob","role":"viewer","initial_password":"short"}`,
		`{"username":"bob","role":"viewer","initial_password":"alice-horse-9","password_hash":"x"}`,
	} {
		rec = f.do(t, http.MethodPost, "/api/v1/users", bad)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("非法输入 %s 状态码 = %d，期望 400（响应 %s）", bad, rec.Code, rec.Body.String())
		}
	}
}

func TestUpdateUserNeedsReauthAndVersion(t *testing.T) {
	f := newAdminFixture(t)
	created := f.createUser(t, "alice", "viewer", "alice-horse-9")
	path := "/api/v1/users/" + strconv.FormatInt(created.ID, 10)
	patchBody := `{"version":` + strconv.Itoa(created.Version) + `,"display_name":"爱丽丝"}`

	// 没有再认证：403 REAUTH_REQUIRED。
	rec := f.do(t, http.MethodPatch, path, patchBody)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("未再认证状态码 = %d，期望 403", rec.Code)
	}
	if got := parseResult(t, rec); got.Error == nil || got.Error.Code != "REAUTH_REQUIRED" {
		t.Fatalf("未再认证错误码 = %+v，期望 REAUTH_REQUIRED", got.Error)
	}

	// 错误的再认证密码：403，且不产生有效期。
	rec = f.reauth(t, "wrong-password-1")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("错误密码再认证状态码 = %d，期望 403", rec.Code)
	}
	if rec = f.do(t, http.MethodPatch, path, patchBody); rec.Code != http.StatusForbidden {
		t.Fatalf("失败的再认证不能放行写操作，状态码 = %d", rec.Code)
	}

	// 正确密码再认证：200，返回 reauth_expires_at。
	rec = f.reauth(t, testUserPass)
	if rec.Code != http.StatusOK {
		t.Fatalf("再认证状态码 = %d，响应 = %s", rec.Code, rec.Body.String())
	}
	var reauth reauthResponse
	if err := json.Unmarshal(parseResult(t, rec).Data, &reauth); err != nil || reauth.ReauthExpiresAt == "" {
		t.Fatalf("再认证响应缺 reauth_expires_at: %v", err)
	}

	// 现在 PATCH 成功，version 自增。
	rec = f.do(t, http.MethodPatch, path, patchBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("再认证后 PATCH 状态码 = %d，响应 = %s", rec.Code, rec.Body.String())
	}
	var updated adminUserDTO
	if err := json.Unmarshal(parseResult(t, rec).Data, &updated); err != nil {
		t.Fatalf("解析 PATCH 结果失败: %v", err)
	}
	if updated.DisplayName != "爱丽丝" || updated.Version != created.Version+1 {
		t.Fatalf("PATCH 结果 = %+v", updated)
	}

	// 过期 version：409 VERSION_CONFLICT。
	rec = f.do(t, http.MethodPatch, path, patchBody)
	if rec.Code != http.StatusConflict {
		t.Fatalf("过期 version 状态码 = %d，期望 409（响应 %s）", rec.Code, rec.Body.String())
	}
	if got := parseResult(t, rec); got.Error == nil || got.Error.Code != "VERSION_CONFLICT" {
		t.Fatalf("冲突错误码 = %+v", got.Error)
	}

	// 不能改自己的角色（防自我提权/降权）。
	rec = f.do(t, http.MethodGet, "/api/v1/users", "")
	var list usersListResponse
	if err := json.Unmarshal(parseResult(t, rec).Data, &list); err != nil {
		t.Fatalf("解析列表失败: %v", err)
	}
	var self adminUserDTO
	for _, u := range list.Items {
		if u.Username == testUser {
			self = u
		}
	}
	if self.ID == 0 {
		t.Fatal("列表里应有当前管理员")
	}
	rec = f.do(t, http.MethodPatch, "/api/v1/users/"+strconv.FormatInt(self.ID, 10),
		`{"version":`+strconv.Itoa(self.Version)+`,"role":"viewer"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("改自己的角色状态码 = %d，期望 403（响应 %s）", rec.Code, rec.Body.String())
	}

	// GET /auth/session 也要反映再认证状态。放在最后：CONTRACTS Q25 规定该接口轮换
	// CSRF 令牌，调用之后 fixture 里的旧令牌就失效了。
	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/session", nil)
	req.Header.Set("Cookie", f.cookie.String())
	recSession := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(recSession, req)
	if !strings.Contains(recSession.Body.String(), `"reauth_expires_at":"`) {
		t.Fatalf("session 响应应包含非空 reauth_expires_at: %s", recSession.Body.String())
	}
}

func TestSetUserPasswordRevokesTargetSessions(t *testing.T) {
	f := newAdminFixture(t)
	created := f.createUser(t, "alice", "viewer", "alice-horse-9")
	aliceCookie, _ := loginAsUser(t, f.srv, "alice", "alice-horse-9")

	if rec := f.reauth(t, testUserPass); rec.Code != http.StatusOK {
		t.Fatalf("再认证失败: %d %s", rec.Code, rec.Body.String())
	}

	body, _ := json.Marshal(map[string]string{"new_password": "alice-horse-7"})
	rec := f.do(t, http.MethodPost,
		"/api/v1/users/"+strconv.FormatInt(created.ID, 10)+"/password", string(body))
	if rec.Code != http.StatusOK {
		t.Fatalf("重置密码状态码 = %d，响应 = %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "alice-horse-7") {
		t.Fatal("重置密码响应不能回显新密码")
	}

	// 目标账号的旧会话立即失效。
	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/session", nil)
	req.Header.Set("Cookie", aliceCookie.String())
	recSession := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(recSession, req)
	if recSession.Code != http.StatusUnauthorized {
		t.Fatalf("改密码后旧会话状态码 = %d，期望 401", recSession.Code)
	}

	// 新密码可登录，旧密码不行。
	if cookie, _ := loginAsUser(t, f.srv, "alice", "alice-horse-7"); cookie == nil {
		t.Fatal("新密码应能登录")
	}
	rec = doLogin(t, f.srv, loginBody("alice", "alice-horse-9"), nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("旧密码登录状态码 = %d，期望 401", rec.Code)
	}
}

func TestDeleteUserAndAuditTrail(t *testing.T) {
	f := newAdminFixture(t)
	created := f.createUser(t, "alice", "viewer", "alice-horse-9")
	aliceCookie, _ := loginAsUser(t, f.srv, "alice", "alice-horse-9")
	path := "/api/v1/users/" + strconv.FormatInt(created.ID, 10)

	if rec := f.reauth(t, testUserPass); rec.Code != http.StatusOK {
		t.Fatalf("再认证失败: %d %s", rec.Code, rec.Body.String())
	}

	// 缺 version：400。
	if rec := f.do(t, http.MethodDelete, path, ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("缺 version 状态码 = %d，期望 400", rec.Code)
	}
	// 错误 version：409。
	if rec := f.do(t, http.MethodDelete, path+"?version=99", ""); rec.Code != http.StatusConflict {
		t.Fatalf("错误 version 状态码 = %d，期望 409", rec.Code)
	}
	// 正确 version（走查询参数）：200。
	if rec := f.do(t, http.MethodDelete, path+"?version="+strconv.Itoa(created.Version), ""); rec.Code != http.StatusOK {
		t.Fatalf("删除状态码 = %d，响应 = %s", rec.Code, rec.Body.String())
	}

	// 被删除账号的会话立即失效。
	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/session", nil)
	req.Header.Set("Cookie", aliceCookie.String())
	recSession := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(recSession, req)
	if recSession.Code != http.StatusUnauthorized {
		t.Fatalf("删除账号后旧会话状态码 = %d，期望 401", recSession.Code)
	}

	// 再删一次：404。
	if rec := f.do(t, http.MethodDelete, path+"?version=1", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("删除不存在账号状态码 = %d，期望 404", rec.Code)
	}

	// 审计（P02.4）：成功与被拒绝的请求都要留痕，且不含口令。
	//
	// 本用例依次发生：创建（成功）、再认证（成功）、删除缺 version（拒绝）、
	// 删除错误 version（业务拒绝 409）、删除成功、再删一次（404）。
	// 因此成功事件 3 条（user.create / auth.reauth / user.delete），失败事件 3 条（全是 user.delete）。
	var successCount, failureCount int
	if err := f.db.SQL().QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM audit_events WHERE outcome = 'success'`).Scan(&successCount); err != nil {
		t.Fatalf("统计成功审计失败: %v", err)
	}
	if err := f.db.SQL().QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM audit_events WHERE outcome = 'failure'`).Scan(&failureCount); err != nil {
		t.Fatalf("统计失败审计失败: %v", err)
	}
	if successCount != 3 {
		t.Fatalf("成功审计条数 = %d，期望 3", successCount)
	}
	if failureCount != 3 {
		t.Fatalf("失败审计条数 = %d，期望 3", failureCount)
	}

	var actions, details string
	if err := f.db.SQL().QueryRowContext(t.Context(),
		`SELECT group_concat(action, ','), group_concat(redacted_details, ',') FROM audit_events`).
		Scan(&actions, &details); err != nil {
		t.Fatalf("读取审计失败: %v", err)
	}
	for _, want := range []string{audit.ActionUserCreate, audit.ActionUserDelete, audit.ActionAuthReauth} {
		if !strings.Contains(actions, want) {
			t.Fatalf("审计动作缺少 %q：%q", want, actions)
		}
	}
	if strings.Contains(details, "alice-horse-9") {
		t.Fatalf("审计明细里出现了口令: %s", details)
	}
	if !strings.Contains(details, "alice") {
		t.Fatalf("审计明细应记录用户名: %s", details)
	}
	if !strings.Contains(details, "version_conflict") && !strings.Contains(details, "VERSION_CONFLICT") {
		t.Fatalf("失败审计应带错误码: %s", details)
	}
}

func TestUserWritesRequireCSRF(t *testing.T) {
	f := newAdminFixture(t)
	created := f.createUser(t, "alice", "viewer", "alice-horse-9")

	// 账号管理写操作一律必须先过 CSRF：管理员会话 + 正确 Origin 也不够。
	cases := []struct{ method, path, body string }{
		{http.MethodPost, "/api/v1/users",
			`{"username":"bob","role":"viewer","initial_password":"bob-horse-99"}`},
		{http.MethodPatch, "/api/v1/users/" + strconv.FormatInt(created.ID, 10), `{"version":1,"disabled":true}`},
		{http.MethodPost, "/api/v1/users/" + strconv.FormatInt(created.ID, 10) + "/password",
			`{"new_password":"alice-horse-7"}`},
		{http.MethodDelete, "/api/v1/users/" + strconv.FormatInt(created.ID, 10) + "?version=1", ""},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		req.Header.Set("Origin", testOrigin)
		req.Header.Set("Cookie", f.cookie.String())
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		f.srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s %s 缺 CSRF 状态码 = %d，期望 403（响应 %s）",
				tc.method, tc.path, rec.Code, rec.Body.String())
		}
		if got := parseResult(t, rec); got.Error == nil || got.Error.Code != "CSRF_FAILED" {
			t.Fatalf("%s %s 缺 CSRF 错误码 = %+v，期望 CSRF_FAILED", tc.method, tc.path, got.Error)
		}
	}
}

// TestAuditFailureRollsBackBusiness 证明审计写不进时业务不生效（P02.4 的事务要求）。
//
// 构造方式：迁移完成后删掉 audit_events 表，让审计插入在事务内失败。期望
// 500 INTERNAL_ERROR，且新建账号**没有**落库——不能出现“账号建好了但没有审计”。
// 这也顺带证明审计确实写在同一笔事务里：如果它是另开连接写的，业务会照常提交。
func TestAuditFailureRollsBackBusiness(t *testing.T) {
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
		Proxies:         newTestProxies(t, db),
		AllowedOrigins:  testOrigins(),
	})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	seedTestUser(t, authSvc, testUser, testUserPass)
	cookie, csrf := loginOK(t, srv)

	if _, err := db.SQL().ExecContext(context.Background(), `DROP TABLE audit_events`); err != nil {
		t.Fatalf("删除审计表失败: %v", err)
	}

	body := `{"username":"ghost","display_name":"Ghost","role":"viewer","initial_password":"ghost-horse-9"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/users", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", testOrigins()[0])
	req.Header.Set("Cookie", cookie.String())
	req.Header.Set(CSRFTokenHeader, csrf)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("审计失败时状态码 = %d，期望 500；响应 = %s", rec.Code, rec.Body.String())
	}
	var envelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("解析错误响应失败: %v", err)
	}
	if envelope.Error.Code != string(httpx.CodeInternalError) {
		t.Fatalf("错误码 = %q，期望 INTERNAL_ERROR", envelope.Error.Code)
	}
	// 响应体不能带内部细节（表名、SQL 文本、审计内部错误）。
	if strings.Contains(rec.Body.String(), "audit_events") || strings.Contains(rec.Body.String(), "审计") {
		t.Fatalf("响应泄漏内部细节: %s", rec.Body.String())
	}

	var n int
	if err := db.SQL().QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM users WHERE username = 'ghost'`).Scan(&n); err != nil {
		t.Fatalf("统计账号失败: %v", err)
	}
	if n != 0 {
		t.Fatalf("审计失败但账号已落库（%d 行）：业务与审计不在同一事务", n)
	}
}
