package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"upanel/internal/config"
	"upanel/internal/platform"
)

const (
	testUser     = "admin"
	testUserPass = "correct-horse-9"
	testOrigin   = "http://127.0.0.1:9443"
)

// loginBody 构造登录请求体。
func loginBody(username, password string) string {
	b, _ := json.Marshal(map[string]any{"username": username, "password": password})
	return string(b)
}

// doLogin 发送一次登录请求，返回响应记录。
func doLogin(t *testing.T, srv *Server, body string, mutate func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(RequestMarkerHeader, RequestMarkerValue)
	req.Header.Set("Origin", testOrigin)
	if mutate != nil {
		mutate(req)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// loginOK 登录并返回会话 Cookie 与 CSRF 令牌。
func loginOK(t *testing.T, srv *Server) (*http.Cookie, string) {
	t.Helper()
	rec := doLogin(t, srv, loginBody(testUser, testUserPass), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("登录失败: 状态码 = %d，响应 = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Data loginResponse `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("登录响应不是合法 JSON: %v", err)
	}
	if body.Data.CSRFToken == "" || body.Data.User.Username != testUser {
		t.Fatalf("登录响应内容不完整: %+v", body.Data)
	}

	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("应只下发一个 Cookie，实际 %d 个", len(cookies))
	}
	return cookies[0], body.Data.CSRFToken
}

// authedServer 返回一个已建好账号的服务实例。
func authedServer(t *testing.T) *Server {
	t.Helper()
	srv := newTestServer(t, platform.ModeProduction, config.ListenProduction)
	seedTestUser(t, srv.opts.Auth, testUser, testUserPass)
	return srv
}

func TestLoginSetsHardenedCookie(t *testing.T) {
	srv := authedServer(t)
	rec := doLogin(t, srv, loginBody(testUser, testUserPass), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，响应 = %s", rec.Code, rec.Body.String())
	}

	raw := rec.Header().Get("Set-Cookie")
	for _, want := range []string{
		SessionCookieName + "=",
		"Path=/",
		"HttpOnly",
		"Secure",
		"SameSite=Strict",
	} {
		if !strings.Contains(raw, want) {
			t.Fatalf("Set-Cookie 缺少 %q: %s", want, raw)
		}
	}
	if strings.Contains(raw, "Domain=") {
		t.Fatalf("__Host- Cookie 不能带 Domain: %s", raw)
	}

	// 响应体里不能出现密码或哈希。
	if strings.Contains(rec.Body.String(), testUserPass) || strings.Contains(rec.Body.String(), "$argon2id$") {
		t.Fatalf("响应体泄漏秘密: %s", rec.Body.String())
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("认证响应必须 no-store，实际 %q", rec.Header().Get("Cache-Control"))
	}
}

func TestLoginEntryChecks(t *testing.T) {
	srv := authedServer(t)

	cases := []struct {
		name    string
		mutate  func(*http.Request)
		body    string
		status  int
		codeStr string
	}{
		{"缺少 Content-Type", func(r *http.Request) { r.Header.Del("Content-Type") }, loginBody(testUser, testUserPass), http.StatusBadRequest, "VALIDATION_ERROR"},
		{"错误 Content-Type", func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }, loginBody(testUser, testUserPass), http.StatusBadRequest, "VALIDATION_ERROR"},
		{"缺少 X-UPanel-Request", func(r *http.Request) { r.Header.Del(RequestMarkerHeader) }, loginBody(testUser, testUserPass), http.StatusBadRequest, "VALIDATION_ERROR"},
		{"错误的 X-UPanel-Request", func(r *http.Request) { r.Header.Set(RequestMarkerHeader, "0") }, loginBody(testUser, testUserPass), http.StatusBadRequest, "VALIDATION_ERROR"},
		{"缺少 Origin", func(r *http.Request) { r.Header.Del("Origin") }, loginBody(testUser, testUserPass), http.StatusForbidden, "CSRF_FAILED"},
		{"Origin 不在白名单", func(r *http.Request) { r.Header.Set("Origin", "http://evil.example.com") }, loginBody(testUser, testUserPass), http.StatusForbidden, "CSRF_FAILED"},
		{"Origin 端口不符", func(r *http.Request) { r.Header.Set("Origin", "http://127.0.0.1:9444") }, loginBody(testUser, testUserPass), http.StatusForbidden, "CSRF_FAILED"},
		{"未知字段", nil, `{"username":"admin","password":"correct-horse-9","extra":1}`, http.StatusBadRequest, "VALIDATION_ERROR"},
		{"非法 JSON", nil, `{`, http.StatusBadRequest, "VALIDATION_ERROR"},
		{"尾部多余内容", nil, `{"username":"admin","password":"correct-horse-9"} {"x":1}`, http.StatusBadRequest, "VALIDATION_ERROR"},
		{"提交 totp", nil, `{"username":"admin","password":"correct-horse-9","totp":"123456"}`, http.StatusBadRequest, "VALIDATION_ERROR"},
		{"空用户名", nil, loginBody("", testUserPass), http.StatusBadRequest, "VALIDATION_ERROR"},
		{"空密码", nil, loginBody(testUser, ""), http.StatusBadRequest, "VALIDATION_ERROR"},
	}
	for _, c := range cases {
		rec := doLogin(t, srv, c.body, c.mutate)
		if rec.Code != c.status {
			t.Fatalf("%s: 状态码 = %d，期望 %d（响应 %s）", c.name, rec.Code, c.status, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), c.codeStr) {
			t.Fatalf("%s: 错误码应包含 %s，响应 %s", c.name, c.codeStr, rec.Body.String())
		}
		if len(rec.Result().Cookies()) != 0 {
			t.Fatalf("%s: 失败的登录不能下发 Cookie", c.name)
		}
	}
}

func TestLoginWrongPasswordAndRateLimit(t *testing.T) {
	srv := authedServer(t)

	rec := doLogin(t, srv, loginBody(testUser, "wrong-password-1"), nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("状态码 = %d，期望 401（响应 %s）", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "UNAUTHENTICATED") {
		t.Fatalf("错误码应为 UNAUTHENTICATED: %s", rec.Body.String())
	}

	// 继续失败直到触发限速（默认 5 次）。
	var limited *httptest.ResponseRecorder
	for i := 0; i < 10; i++ {
		r := doLogin(t, srv, loginBody(testUser, "wrong-password-1"), nil)
		if r.Code == http.StatusTooManyRequests {
			limited = r
			break
		}
	}
	if limited == nil {
		t.Fatal("连续失败后应返回 429")
	}
	var body struct {
		Error struct {
			Code    string         `json:"code"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(limited.Body.Bytes(), &body); err != nil {
		t.Fatalf("限速响应不是合法 JSON: %v", err)
	}
	if body.Error.Code != "VALIDATION_ERROR" {
		t.Fatalf("限速响应的 code = %q", body.Error.Code)
	}
	if _, ok := body.Error.Details["retry_after_seconds"]; !ok {
		t.Fatalf("限速响应应给出 retry_after_seconds: %s", limited.Body.String())
	}

	// 正确密码在限速窗口内也必须被拒（限速先于密码校验）。
	rec = doLogin(t, srv, loginBody(testUser, testUserPass), nil)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("限速期间正确密码也应 429，实际 %d", rec.Code)
	}
}

func TestSessionRequiresCookieAndRotatesCSRF(t *testing.T) {
	srv := authedServer(t)

	// 未登录：401。
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/auth/session", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("未登录访问 session 状态码 = %d，期望 401", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "UNAUTHENTICATED") {
		t.Fatalf("错误码应为 UNAUTHENTICATED: %s", rec.Body.String())
	}

	cookie, csrf := loginOK(t, srv)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/session", nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，响应 = %s", rec.Code, rec.Body.String())
	}

	var body struct {
		Data sessionResponse `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if body.Data.User.Username != testUser {
		t.Fatalf("返回账号 = %+v", body.Data.User)
	}
	if body.Data.CSRFToken == "" {
		t.Fatal("session 必须返回 CSRF 令牌，供刷新后的前端重新拿到")
	}
	if body.Data.CSRFToken == csrf {
		t.Fatal("每次读取 session 都应轮换 CSRF 令牌")
	}
	if body.Data.ReauthExpiresAt != nil {
		t.Fatalf("尚未再认证时 reauth_expires_at 应为 null，实际 %v", *body.Data.ReauthExpiresAt)
	}

	// 旧 CSRF 令牌已失效。
	rec = doLogout(srv, cookie, csrf)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("旧 CSRF 令牌应被拒绝，状态码 = %d", rec.Code)
	}
}

func doLogout(srv *Server, cookie *http.Cookie, csrf string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", nil)
	req.AddCookie(cookie)
	if csrf != "" {
		req.Header.Set(CSRFTokenHeader, csrf)
	}
	req.Header.Set("Origin", testOrigin)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func TestLogoutRequiresCSRFAndInvalidatesSession(t *testing.T) {
	srv := authedServer(t)
	cookie, csrf := loginOK(t, srv)

	// 无 CSRF 头：403，会话仍有效。
	rec := doLogout(srv, cookie, "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("缺少 CSRF 令牌时状态码 = %d，期望 403", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "CSRF_FAILED") {
		t.Fatalf("错误码应为 CSRF_FAILED: %s", rec.Body.String())
	}

	// 错误 CSRF 头：403。
	rec = doLogout(srv, cookie, "not-the-token")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("错误 CSRF 令牌时状态码 = %d，期望 403", rec.Code)
	}

	// 正确 CSRF：200 并清除 Cookie。
	rec = doLogout(srv, cookie, csrf)
	if rec.Code != http.StatusOK {
		t.Fatalf("退出状态码 = %d，响应 = %s", rec.Code, rec.Body.String())
	}
	raw := rec.Header().Get("Set-Cookie")
	if !strings.Contains(raw, SessionCookieName+"=;") || !strings.Contains(raw, "Max-Age=0") {
		t.Fatalf("退出应清除 Cookie，实际 %s", raw)
	}

	// 退出后原会话立即失效。
	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/session", nil)
	req.AddCookie(cookie)
	rec2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec2, req)
	if rec2.Code != http.StatusUnauthorized {
		t.Fatalf("退出后访问 session 状态码 = %d，期望 401", rec2.Code)
	}
}

func TestOriginGuardRejectsForeignOriginOnWrites(t *testing.T) {
	srv := authedServer(t)
	cookie, csrf := loginOK(t, srv)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", nil)
	req.AddCookie(cookie)
	req.Header.Set(CSRFTokenHeader, csrf)
	req.Header.Set("Origin", "http://evil.example.com")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("跨站 Origin 的写请求状态码 = %d，期望 403", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "CSRF_FAILED") {
		t.Fatalf("错误码应为 CSRF_FAILED: %s", rec.Body.String())
	}
}

func TestForgedOrRevokedSessionIsRejected(t *testing.T) {
	srv := authedServer(t)
	cookie, csrf := loginOK(t, srv)

	// 伪造的 Cookie 值：401，且不泄漏任何会话信息。
	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/session", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: "deadbeef"})
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("伪造 Cookie 状态码 = %d，期望 401", rec.Code)
	}

	// 正常退出后同一个 Cookie 也必须失效（会话在服务端被删除，不靠客户端）。
	if rec := doLogout(srv, cookie, csrf); rec.Code != http.StatusOK {
		t.Fatalf("退出状态码 = %d", rec.Code)
	}
	req = httptest.NewRequest(http.MethodGet, "/api/v1/auth/session", nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("已作废会话状态码 = %d，期望 401", rec.Code)
	}
}
