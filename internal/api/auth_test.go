package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

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
		// totp 的形状检查（P14.6）：非数字与超长都拒。形状对但值不对不在这里判——
		// 那是代理的活，见 TestLoginWithTotp。未启用二次验证的账号即使带 totp 也照常登录。
		{"totp 非数字", nil, `{"username":"admin","password":"correct-horse-9","totp":"abcdef"}`, http.StatusBadRequest, "VALIDATION_ERROR"},
		{"totp 超长", nil, `{"username":"admin","password":"correct-horse-9","totp":"1234567890"}`, http.StatusBadRequest, "VALIDATION_ERROR"},
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

// TestSessionTouchRefreshesCookie 覆盖一个只在真机上暴露过的问题：
// 会话在库里按空闲时间滑动续期，但 Cookie 的 Expires 只在登录时下发过一次。
// 结果是“一直在用，30 分钟后照样被踢”——库里会话还活着，浏览器里的 Cookie 已经到期。
// 续期时必须重发 Cookie，让两边同步。
func TestSessionTouchRefreshesCookie(t *testing.T) {
	srv, db := newTestServerWithDB(t, platform.ModeProduction, config.ListenProduction)
	seedTestUser(t, srv.opts.Auth, testUser, testUserPass)
	cookie, _ := loginOK(t, srv)

	doGet := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/session", nil)
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("session 状态码 = %d，响应 = %s", rec.Code, rec.Body.String())
		}
		return rec
	}

	// 登录后立刻访问，仍在一分钟节流窗口内：不续期，也就不该重发 Cookie。
	if rec := doGet(); rec.Header().Get("Set-Cookie") != "" {
		t.Fatalf("节流窗口内不应重发 Cookie，实际 %s", rec.Header().Get("Set-Cookie"))
	}

	// 把会话回退成“上次活动是一分半前、库里到期时间只剩 5 分钟”。
	now := time.Now().UTC()
	if _, err := db.SQL().ExecContext(context.Background(),
		`UPDATE sessions SET last_seen_at = ?, expires_at = ?`,
		now.Add(-90*time.Second).Format(time.RFC3339),
		now.Add(5*time.Minute).Format(time.RFC3339)); err != nil {
		t.Fatalf("回退会话时间失败: %v", err)
	}

	rec := doGet()
	raw := rec.Header().Get("Set-Cookie")
	if raw == "" {
		t.Fatal("滑动续期后必须重发 Cookie，否则浏览器里的 Cookie 仍按登录时刻到期")
	}
	var refreshed *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == SessionCookieName {
			refreshed = c
		}
	}
	if refreshed == nil {
		t.Fatalf("响应里没有会话 Cookie: %s", raw)
	}
	if refreshed.Value != cookie.Value {
		t.Fatalf("续期必须沿用同一个会话令牌，实际被换成别的值")
	}
	if !refreshed.Expires.After(now.Add(25 * time.Minute)) {
		t.Fatalf("续期 Cookie 的 Expires 应推进到约 30 分钟后，实际 %s", refreshed.Expires)
	}

	// 库里到期时间也一起前移，两边不能各说各话。
	var expires string
	if err := db.SQL().QueryRowContext(context.Background(),
		`SELECT expires_at FROM sessions ORDER BY id DESC LIMIT 1`).Scan(&expires); err != nil {
		t.Fatalf("读取会话到期时间失败: %v", err)
	}
	got, err := time.Parse(time.RFC3339, expires)
	if err != nil {
		t.Fatalf("到期时间格式不合法: %v", err)
	}
	if !got.After(now.Add(25 * time.Minute)) {
		t.Fatalf("库里到期时间应推进到约 30 分钟后，实际 %s", expires)
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

// 浏览器导航或组件卸载时会把还在飞的请求 abort 掉，会话读取随之以 context.Canceled 收尾。
// 那条路径**不能**记成 500：没有人在接这个响应，日志里说"服务内部错误"是假话，
// 而 P15.4 的真机验收上正是靠读日志才抓到过一次（三条请求同一秒内消失）。
func TestClientAbortedRequestIsNotReportedAsServerFault(t *testing.T) {
	srv := authedServer(t)
	cookie, _ := loginOK(t, srv)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 请求还没进中间件，客户端就已经走了

	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/session", nil).WithContext(ctx)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != clientClosedRequest {
		t.Fatalf("客户端已取消的请求状态码 = %d，期望 %d（不能是 5xx）", rec.Code, clientClosedRequest)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("客户端已走，不应再写响应体，实际 %d 字节: %s", rec.Body.Len(), rec.Body.String())
	}
}

func TestClientGoneRecognisesCancellationOnly(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"原始 context.Canceled", context.Canceled, true},
		{"被包过的取消（会话读取就是这么包的）", fmt.Errorf("auth: 读取会话失败: %w", context.Canceled), true},
		{"超时是另一回事，得有人看", context.DeadlineExceeded, false},
		{"普通错误", errors.New("disk on fire"), false},
		{"没有错误", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/system/summary", nil)
			if got := clientGone(c, tc.err); got != tc.want {
				t.Fatalf("clientGone(%v) = %v，期望 %v", tc.err, got, tc.want)
			}
		})
	}
}
