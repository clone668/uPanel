package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"upanel/internal/agentclient"
	"upanel/internal/auth"
	"upanel/internal/certificates"
	"upanel/internal/config"
	"upanel/internal/httpx"
	"upanel/internal/platform"
	"upanel/internal/store"
	"upanel/internal/version"
)

// fakeTotpAgent 是二次验证的假代理：种子材料它自己留着，面板只能看到引用。
//
// 它不"什么都答应"：goodCode 之外一律 code_mismatch，返回的时间步由用例显式推进
// （advance），这样"同一个步用两次被拒"能被真实地复现出来，而不是靠时间流逝。
type fakeTotpAgent struct {
	mu sync.Mutex

	goodCode string
	step     int64

	enrollErr  error
	confirmErr error
	verifyErr  error
	disableErr error

	// disableDestroyed 是 TotpDisable 的返回（是否真的销毁了一条）。
	disableDestroyed bool

	enrolls  int
	confirms int
	verifies int
	disables int

	lastEnrollUser  int64
	lastEnrollName  string
	lastConfirmUser int64
	lastConfirmRef  string
	lastConfirmCode string
	lastVerifyUser  int64
	lastVerifyRef   string
	lastVerifyCode  string
	lastDisableUser int64
	lastDisableRef  string
}

func newFakeTotpAgent() *fakeTotpAgent {
	return &fakeTotpAgent{goodCode: "123456", step: 100, disableDestroyed: true}
}

func (f *fakeTotpAgent) advance(to int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.step = to
}

func (f *fakeTotpAgent) TotpEnroll(_ context.Context, userID int64, username string) (agentclient.TotpEnrollment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enrolls++
	f.lastEnrollUser, f.lastEnrollName = userID, username
	if f.enrollErr != nil {
		return agentclient.TotpEnrollment{}, f.enrollErr
	}
	ref := fmt.Sprintf("totp-secret-%d", userID)
	return agentclient.TotpEnrollment{
		SecretRef:  ref,
		OTPAuthURI: fmt.Sprintf("otpauth://totp/uPanel:%s?secret=JBSWY3DPEHPK3PXP&issuer=uPanel&digits=6&period=30", username),
		Issuer:     "uPanel",
		Account:    username,
		Digits:     6,
		Period:     30,
		ExpiresAt:  time.Now().UTC().Add(10 * time.Minute).Format(time.RFC3339),
	}, nil
}

func (f *fakeTotpAgent) TotpConfirm(_ context.Context, userID int64, secretRef, code string) (agentclient.TotpCheck, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.confirms++
	f.lastConfirmUser, f.lastConfirmRef, f.lastConfirmCode = userID, secretRef, code
	if f.confirmErr != nil {
		return agentclient.TotpCheck{}, f.confirmErr
	}
	if strings.TrimSpace(code) != f.goodCode {
		return agentclient.TotpCheck{OK: false, Reason: "code_mismatch"}, nil
	}
	return agentclient.TotpCheck{OK: true, Step: f.step}, nil
}

func (f *fakeTotpAgent) TotpVerify(_ context.Context, userID int64, secretRef, code string) (agentclient.TotpCheck, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.verifies++
	f.lastVerifyUser, f.lastVerifyRef, f.lastVerifyCode = userID, secretRef, code
	if f.verifyErr != nil {
		return agentclient.TotpCheck{}, f.verifyErr
	}
	if strings.TrimSpace(code) != f.goodCode {
		return agentclient.TotpCheck{OK: false, Reason: "code_mismatch"}, nil
	}
	return agentclient.TotpCheck{OK: true, Step: f.step}, nil
}

func (f *fakeTotpAgent) TotpDisable(_ context.Context, userID int64, secretRef string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.disables++
	f.lastDisableUser, f.lastDisableRef = userID, secretRef
	if f.disableErr != nil {
		return false, f.disableErr
	}
	return f.disableDestroyed, nil
}

// totpFixture 是二次验证用例的固定装置：真实数据库 + 假代理 + 一个已建好的管理员。
//
// agent 传 nil 表示"这台机器的代理没有二次验证能力"（Options.Totp 为 nil），
// 用来验证失败关闭。
type totpFixture struct {
	srv    *Server
	user   auth.User
	cookie *http.Cookie
	csrf   string
	db     *store.DB
	auth   *auth.Service
	agent  *fakeTotpAgent
}

func newTotpFixture(t *testing.T, agent TotpAgent) *totpFixture {
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
		Totp:              agent,
		Operations:        newTestOperations(t, db, nil),
		Apps:              newTestApps(t, db),
		Proxies:           newTestProxies(t, db),
		Certificates:      newTestCertificates(t, db),
		CertificateStages: certificates.NewStageStore(nil),
		AllowedOrigins:    testOrigins(),
	})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	user := seedTestUser(t, srv.opts.Auth, testUser, testUserPass)
	cookie, csrf := loginOK(t, srv)

	f := &totpFixture{srv: srv, user: user, cookie: cookie, csrf: csrf, db: db, auth: authSvc}
	if fake, ok := agent.(*fakeTotpAgent); ok {
		f.agent = fake
	}
	return f
}

// do 以当前会话 + CSRF 发送请求。
func (f *totpFixture) do(t *testing.T, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	return f.doAs(t, f.cookie, f.csrf, method, path, body)
}

func (f *totpFixture) doAs(t *testing.T, cookie *http.Cookie, csrf, method, path, body string) *httptest.ResponseRecorder {
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

// reauth 完成一次再认证。totp 为空时不带验证码字段。
func (f *totpFixture) reauth(t *testing.T, password, totp string) *httptest.ResponseRecorder {
	t.Helper()
	payload := map[string]any{"password": password}
	if totp != "" {
		payload["totp"] = totp
	}
	body, _ := json.Marshal(payload)
	return f.do(t, http.MethodPost, "/api/v1/auth/reauth", string(body))
}

// loginWith 用给定请求体登录，返回会话 Cookie 与 CSRF。
func (f *totpFixture) loginWith(t *testing.T, payload map[string]any) (*http.Cookie, string) {
	t.Helper()
	body, _ := json.Marshal(payload)
	rec := doLogin(t, f.srv, string(body), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("登录失败: 状态码 = %d，响应 = %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Data loginResponse `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("登录响应不是合法 JSON: %v", err)
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("登录应只下发一个 Cookie，实际 %d 个", len(cookies))
	}
	return cookies[0], out.Data.CSRFToken
}

// enableTotpDirect 直接经服务层启用二次验证（跳过 setup/confirm），返回恢复码明文。
//
// 时间步写 0：这条路径模拟的是"这台机器的账号早就登记过了"，不走确认流程，也就没有
// "确认码要用掉哪一步"这回事。真的走过 /totp/confirm 的用例在 TestTotpConfirmConsumesCode。
func (f *totpFixture) enableTotpDirect(t *testing.T, ref string) []string {
	t.Helper()
	codes, err := f.auth.EnableTotp(context.Background(), f.user.ID, ref, 0)
	if err != nil {
		t.Fatalf("直接启用二次验证失败: %v", err)
	}
	return codes
}

// totpState 读库里的状态行，返回状态与"有没有这一行"。
func (f *totpFixture) totpState(t *testing.T) (auth.TotpState, bool) {
	t.Helper()
	state, exists, err := f.auth.TotpState(context.Background(), f.user.ID)
	if err != nil {
		t.Fatalf("读取二次验证状态失败: %v", err)
	}
	return state, exists
}

// recoveryRows 数库里与该账号相关的恢复码行数与其中的哈希前缀。
func (f *totpFixture) recoveryRows(t *testing.T) (total int, allHashed bool) {
	t.Helper()
	var plain int
	if err := f.db.SQL().QueryRow(
		`SELECT COUNT(*) FROM user_totp_recovery_codes WHERE user_id = ?`, f.user.ID).Scan(&total); err != nil {
		t.Fatalf("统计恢复码失败: %v", err)
	}
	if err := f.db.SQL().QueryRow(
		`SELECT COUNT(*) FROM user_totp_recovery_codes
		 WHERE user_id = ? AND substr(code_hash, 1, 7) = 'sha256:'`, f.user.ID).Scan(&plain); err != nil {
		t.Fatalf("统计恢复码哈希失败: %v", err)
	}
	return total, plain == total && total > 0
}

// TestTotpSetupConfirmFlow 走一遍完整登记：setup → 码错 → 确认 → 启用。
//
// 关键断言是"码错的时候什么都没启用"：库里没有状态行，恢复码一枚都没发，
// 代理那条种子仍是临时秘密（由代理侧用例覆盖）。
func TestTotpSetupConfirmFlow(t *testing.T) {
	f := newTotpFixture(t, newFakeTotpAgent())

	// 未再认证不得登记。
	rec := f.do(t, http.MethodPost, "/api/v1/auth/totp/setup", "")
	if got := errorCode(t, rec); got != string(httpx.CodeReauthRequired) {
		t.Fatalf("未再认证 setup 错误码 = %s，期望 %s（响应 %s）",
			got, httpx.CodeReauthRequired, rec.Body.String())
	}
	if rec := f.reauth(t, testUserPass, ""); rec.Code != http.StatusOK {
		t.Fatalf("再认证失败: %d %s", rec.Code, rec.Body.String())
	}

	rec = f.do(t, http.MethodPost, "/api/v1/auth/totp/setup", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("setup 状态码 = %d，响应 = %s", rec.Code, rec.Body.String())
	}
	var setup totpSetupResponse
	if err := json.Unmarshal(parseResult(t, rec).Data, &setup); err != nil {
		t.Fatalf("解析 setup 响应失败: %v", err)
	}
	if setup.EnrollmentID == "" || !strings.HasPrefix(setup.OTPAuthURI, "otpauth://totp/") {
		t.Fatalf("setup 响应缺引用或 URI: %+v", setup)
	}
	if setup.Digits != 6 || setup.Period != 30 || setup.Issuer != "uPanel" || setup.Account != testUser {
		t.Fatalf("setup 参数不对: %+v", setup)
	}
	if setup.ExpiresAt == "" {
		t.Fatalf("setup 必须告诉调用方登记窗口何时过期: %+v", setup)
	}
	if f.agent.lastEnrollUser != f.user.ID || f.agent.lastEnrollName != testUser {
		t.Fatalf("代理收到的登记对象 = %d/%q", f.agent.lastEnrollUser, f.agent.lastEnrollName)
	}

	// 登记只是登记：库里不能有任何状态行。
	if _, exists := f.totpState(t); exists {
		t.Fatalf("setup 之后库里不该有状态行")
	}

	// 码错：401，且仍然什么都没启用。
	body, _ := json.Marshal(map[string]string{"enrollment_id": setup.EnrollmentID, "totp": "000000"})
	rec = f.do(t, http.MethodPost, "/api/v1/auth/totp/confirm", string(body))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("码错 confirm 状态码 = %d，期望 401（响应 %s）", rec.Code, rec.Body.String())
	}
	if _, exists := f.totpState(t); exists {
		t.Fatalf("码错之后库里不该有状态行")
	}

	// 码对：启用，并发十枚一次性恢复码。
	body, _ = json.Marshal(map[string]string{"enrollment_id": setup.EnrollmentID, "totp": "123456"})
	rec = f.do(t, http.MethodPost, "/api/v1/auth/totp/confirm", string(body))
	if rec.Code != http.StatusOK {
		t.Fatalf("confirm 状态码 = %d，响应 = %s", rec.Code, rec.Body.String())
	}
	var confirmed totpConfirmResponse
	if err := json.Unmarshal(parseResult(t, rec).Data, &confirmed); err != nil {
		t.Fatalf("解析 confirm 响应失败: %v", err)
	}
	if !confirmed.Enabled || len(confirmed.RecoveryCodes) != 10 {
		t.Fatalf("confirm 结果不对: enabled=%v codes=%d", confirmed.Enabled, len(confirmed.RecoveryCodes))
	}
	seen := map[string]bool{}
	for _, code := range confirmed.RecoveryCodes {
		if len(code) != 11 || !strings.Contains(code, "-") {
			t.Fatalf("恢复码形状不对: %q", code)
		}
		if seen[code] {
			t.Fatalf("恢复码重复: %q", code)
		}
		seen[code] = true
	}

	state, exists := f.totpState(t)
	// LastStep 是确认用的那个码匹配到的时间步（假代理回 100）：代理已经验过它，
	// 面板这边必须把它记成"已用过"，否则同一个码在同一个 30 秒窗口里还能再登录一次。
	if !exists || !state.Enabled || state.SecretRef != setup.EnrollmentID || state.LastStep != 100 {
		t.Fatalf("启用后的状态行不对: %+v exists=%v", state, exists)
	}
	if total, hashed := f.recoveryRows(t); total != 10 || !hashed {
		t.Fatalf("恢复码入库情况: 行数=%d 全为哈希=%v", total, hashed)
	}
	// 明文恢复码绝不能出现在库里（任意表任意列）。
	var plainRows int
	if err := f.db.SQL().QueryRow(
		`SELECT COUNT(*) FROM user_totp_recovery_codes WHERE code_hash = ?`,
		confirmed.RecoveryCodes[0]).Scan(&plainRows); err != nil {
		t.Fatalf("查询明文恢复码失败: %v", err)
	}
	if plainRows != 0 {
		t.Fatalf("库里出现了恢复码明文")
	}

	// 已启用再登记：422，不重新发码。
	rec = f.do(t, http.MethodPost, "/api/v1/auth/totp/setup", "")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("重复 setup 状态码 = %d，期望 422（响应 %s）", rec.Code, rec.Body.String())
	}
	if total, _ := f.recoveryRows(t); total != 10 {
		t.Fatalf("重复 setup 不应改动恢复码，实际 %d 行", total)
	}
}

// TestTotpConfirmConsumesCode 确认用的那个码不能再拿来登录（Q480 的回归）。
//
// 真机第一轮抓到的是：confirm 之后库里 last_step 还是 -1，于是刚刚用来确认的那个六位数
// 在同一个 30 秒窗口里还能登录一次。代理那边"验过"和面板这边"记下这一步"必须同时发生，
// 所以这条用例从**接口层**走完再拿同一个码去登录。
func TestTotpConfirmConsumesCode(t *testing.T) {
	f := newTotpFixture(t, newFakeTotpAgent())
	if rec := f.reauth(t, testUserPass, ""); rec.Code != http.StatusOK {
		t.Fatalf("再认证失败: %d %s", rec.Code, rec.Body.String())
	}
	rec := f.do(t, http.MethodPost, "/api/v1/auth/totp/setup", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("setup 状态码 = %d，响应 = %s", rec.Code, rec.Body.String())
	}
	var setup totpSetupResponse
	if err := json.Unmarshal(parseResult(t, rec).Data, &setup); err != nil {
		t.Fatalf("解析 setup 响应失败: %v", err)
	}
	body, _ := json.Marshal(map[string]string{"enrollment_id": setup.EnrollmentID, "totp": "123456"})
	if rec = f.do(t, http.MethodPost, "/api/v1/auth/totp/confirm", string(body)); rec.Code != http.StatusOK {
		t.Fatalf("confirm 状态码 = %d，响应 = %s", rec.Code, rec.Body.String())
	}

	login, _ := json.Marshal(map[string]any{
		"username": testUser, "password": testUserPass, "totp": "123456",
	})
	rec = doLogin(t, f.srv, string(login), nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("确认码重放登录状态码 = %d，期望 401（响应 %s）", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "刚刚用过") {
		t.Fatalf("重放应说明码已用过: %s", rec.Body.String())
	}
	if len(rec.Result().Cookies()) != 0 {
		t.Fatalf("重放被拒时不得下发 Cookie")
	}

	// 下一个时间步仍然放行：重放防护不能顺手把"下一步的码"也关掉。
	f.agent.advance(101)
	if rec = doLogin(t, f.srv, string(login), nil); rec.Code != http.StatusOK {
		t.Fatalf("下一个时间步登录状态码 = %d，响应 = %s", rec.Code, rec.Body.String())
	}
}

// TestTotpRoutesAreAdminOnly viewer 连登记入口都碰不到。
func TestTotpRoutesAreAdminOnly(t *testing.T) {
	f := newTotpFixture(t, newFakeTotpAgent())

	svc := f.srv.opts.Auth
	if _, err := svc.CreateUser(context.Background(), auth.CreateUserInput{
		Username: "alice", DisplayName: "alice", Role: auth.RoleViewer, Password: "alice-horse-9",
	}); err != nil {
		t.Fatalf("创建 viewer 失败: %v", err)
	}
	cookie, csrf := loginAsUser(t, f.srv, "alice", "alice-horse-9")

	cases := []struct{ method, path, body string }{
		{http.MethodPost, "/api/v1/auth/totp/setup", ""},
		{http.MethodPost, "/api/v1/auth/totp/confirm", `{"enrollment_id":"x","totp":"123456"}`},
		{http.MethodDelete, "/api/v1/auth/totp", `{"password":"alice-horse-9","totp":"123456"}`},
	}
	for _, tc := range cases {
		rec := f.doAs(t, cookie, csrf, tc.method, tc.path, tc.body)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("viewer %s %s 状态码 = %d，期望 403", tc.method, tc.path, rec.Code)
		}
		if got := errorCode(t, rec); got != string(httpx.CodeForbidden) {
			t.Fatalf("viewer 越权错误码 = %s，期望 %s", got, httpx.CodeForbidden)
		}
	}
}

// TestTotpWithoutAgentFailsClosed 代理没有这项能力时：三条入口一律 503，且不写状态。
func TestTotpWithoutAgentFailsClosed(t *testing.T) {
	f := newTotpFixture(t, nil)

	if rec := f.reauth(t, testUserPass, ""); rec.Code != http.StatusOK {
		t.Fatalf("再认证失败: %d %s", rec.Code, rec.Body.String())
	}
	rec := f.do(t, http.MethodPost, "/api/v1/auth/totp/setup", "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("无代理 setup 状态码 = %d，期望 503（响应 %s）", rec.Code, rec.Body.String())
	}
	if got := errorCode(t, rec); got != string(httpx.CodeAgentUnavailable) {
		t.Fatalf("无代理 setup 错误码 = %s，期望 %s", got, httpx.CodeAgentUnavailable)
	}

	// 代理侧不可用，但库里已经启用了二次验证的账号：登录必须失败关闭（503），不是放行。
	f.enableTotpDirect(t, "totp-secret-1")
	rec = doLogin(t, f.srv, loginBody(testUser, testUserPass), nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("无代理时已启用账号登录状态码 = %d，期望 503（响应 %s）", rec.Code, rec.Body.String())
	}
	if len(rec.Result().Cookies()) != 0 {
		t.Fatalf("失败关闭时不得下发 Cookie")
	}
}

// TestTotpAgentUnavailableIs503 代理连不上时，三条入口都报 AGENT_UNAVAILABLE。
func TestTotpAgentUnavailableIs503(t *testing.T) {
	agent := newFakeTotpAgent()
	agent.enrollErr = &agentclient.UnavailableError{Err: fmt.Errorf("socket 不在")}
	f := newTotpFixture(t, agent)

	if rec := f.reauth(t, testUserPass, ""); rec.Code != http.StatusOK {
		t.Fatalf("再认证失败: %d %s", rec.Code, rec.Body.String())
	}
	rec := f.do(t, http.MethodPost, "/api/v1/auth/totp/setup", "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("代理不可用 setup 状态码 = %d，期望 503（响应 %s）", rec.Code, rec.Body.String())
	}
	if _, exists := f.totpState(t); exists {
		t.Fatalf("代理不可用不该写状态行")
	}
}

// TestTotpEnrollmentExpiredIs409 登记窗口过了（代理回 404）时给 409 与"重新开始"的指示。
func TestTotpEnrollmentExpiredIs409(t *testing.T) {
	agent := newFakeTotpAgent()
	agent.confirmErr = &agentclient.Error{Status: http.StatusNotFound, Code: "NOT_FOUND", Message: "登记不存在"}
	f := newTotpFixture(t, agent)

	if rec := f.reauth(t, testUserPass, ""); rec.Code != http.StatusOK {
		t.Fatalf("再认证失败: %d %s", rec.Code, rec.Body.String())
	}
	body, _ := json.Marshal(map[string]string{"enrollment_id": "expired-ref", "totp": "123456"})
	rec := f.do(t, http.MethodPost, "/api/v1/auth/totp/confirm", string(body))
	if rec.Code != http.StatusConflict {
		t.Fatalf("登记过期 confirm 状态码 = %d，期望 409（响应 %s）", rec.Code, rec.Body.String())
	}
	if _, exists := f.totpState(t); exists {
		t.Fatalf("登记过期不该写状态行")
	}
}

// TestLoginWithTotp 启用之后登录必须带码，且同一个码不能用两次。
func TestLoginWithTotp(t *testing.T) {
	f := newTotpFixture(t, newFakeTotpAgent())
	f.enableTotpDirect(t, "totp-secret-1")

	// 不带码：401 + totp_required，且不签发会话。
	rec := doLogin(t, f.srv, loginBody(testUser, testUserPass), nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("缺码登录状态码 = %d，期望 401（响应 %s）", rec.Code, rec.Body.String())
	}
	if len(rec.Result().Cookies()) != 0 {
		t.Fatalf("缺码登录不得下发 Cookie")
	}
	if got := parseResult(t, rec); got.Error == nil || got.Error.Details["totp_required"] != true {
		t.Fatalf("缺码登录必须告诉前端 totp_required: %+v", got.Error)
	}

	// 码错：401，仍不下发。
	body, _ := json.Marshal(map[string]any{"username": testUser, "password": testUserPass, "totp": "999999"})
	rec = doLogin(t, f.srv, string(body), nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("码错登录状态码 = %d，期望 401（响应 %s）", rec.Code, rec.Body.String())
	}
	if len(rec.Result().Cookies()) != 0 {
		t.Fatalf("码错登录不得下发 Cookie")
	}

	// 码对：登录成功，且响应里 totp_enabled=true。
	body, _ = json.Marshal(map[string]any{"username": testUser, "password": testUserPass, "totp": "123456"})
	rec = doLogin(t, f.srv, string(body), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("带码登录状态码 = %d，响应 = %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Data loginResponse `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("登录响应不是合法 JSON: %v", err)
	}
	if !out.Data.TotpEnabled {
		t.Fatalf("已启用二次验证的账号登录后必须回报 totp_enabled=true")
	}
	if f.agent.lastVerifyRef != "totp-secret-1" {
		t.Fatalf("登录校验用的引用 = %q，期望库里那条 secret_ref", f.agent.lastVerifyRef)
	}
	state, _ := f.totpState(t)
	if state.LastStep != 100 {
		t.Fatalf("通过后 last_step = %d，期望 100", state.LastStep)
	}

	// 同一个时间步（同一个码）再来一次：401 重放。
	rec = doLogin(t, f.srv, string(body), nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("重放登录状态码 = %d，期望 401（响应 %s）", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "刚刚用过") {
		t.Fatalf("重放应说明码已用过: %s", rec.Body.String())
	}

	// 下一个时间步：放行。
	f.agent.advance(101)
	rec = doLogin(t, f.srv, string(body), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("下一步登录状态码 = %d，响应 = %s", rec.Code, rec.Body.String())
	}
}

// TestReauthWithTotp 启用之后再认证也要带码；没带不推进再认证窗口。
func TestReauthWithTotp(t *testing.T) {
	f := newTotpFixture(t, newFakeTotpAgent())
	f.enableTotpDirect(t, "totp-secret-1")

	// 这条会话是启用之前登录的（旧票），但再认证仍按当前状态要求第二因子。
	rec := f.reauth(t, testUserPass, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("缺码再认证状态码 = %d，期望 401（响应 %s）", rec.Code, rec.Body.String())
	}
	rec = f.reauth(t, testUserPass, "123456")
	if rec.Code != http.StatusOK {
		t.Fatalf("带码再认证状态码 = %d，响应 = %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Data reauthResponse `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("再认证响应不是合法 JSON: %v", err)
	}
	if out.Data.ReauthExpiresAt == "" {
		t.Fatalf("再认证响应缺到期时间")
	}
	// 再认证也算用掉一个时间步。
	state, _ := f.totpState(t)
	if state.LastStep != 100 {
		t.Fatalf("再认证后 last_step = %d，期望 100", state.LastStep)
	}
	// 已经过了再认证，可以登记（已启用 → 422 而不是 401/403）。
	if rec := f.do(t, http.MethodPost, "/api/v1/auth/totp/setup", ""); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("启用后再认证后 setup 状态码 = %d，期望 422（响应 %s）", rec.Code, rec.Body.String())
	}
}

// TestTotpDisable 关闭：口令错 / 码错都关不掉，恢复码能关掉一次。
func TestTotpDisable(t *testing.T) {
	f := newTotpFixture(t, newFakeTotpAgent())
	codes := f.enableTotpDirect(t, "totp-secret-1")

	if rec := f.reauth(t, testUserPass, "123456"); rec.Code != http.StatusOK {
		t.Fatalf("再认证失败: %d %s", rec.Code, rec.Body.String())
	}

	// 口令错：403，状态不动。
	body, _ := json.Marshal(map[string]string{"password": "wrong-horse-1", "totp": "123456"})
	rec := f.do(t, http.MethodDelete, "/api/v1/auth/totp", string(body))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("口令错关闭状态码 = %d，期望 403（响应 %s）", rec.Code, rec.Body.String())
	}
	if state, exists := f.totpState(t); !exists || !state.Enabled {
		t.Fatalf("口令错不该改动状态")
	}

	// 缺第二因子：400。
	body, _ = json.Marshal(map[string]string{"password": testUserPass})
	rec = f.do(t, http.MethodDelete, "/api/v1/auth/totp", string(body))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("缺第二因子关闭状态码 = %d，期望 400（响应 %s）", rec.Code, rec.Body.String())
	}

	// 码错：401，状态不动，也不销毁种子。
	body, _ = json.Marshal(map[string]string{"password": testUserPass, "totp": "000000"})
	rec = f.do(t, http.MethodDelete, "/api/v1/auth/totp", string(body))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("码错关闭状态码 = %d，期望 401（响应 %s）", rec.Code, rec.Body.String())
	}
	if state, exists := f.totpState(t); !exists || !state.Enabled {
		t.Fatalf("码错不该改动状态")
	}
	if f.agent.disables != 0 {
		t.Fatalf("码错不该去代理销毁种子，实际调用 %d 次", f.agent.disables)
	}

	// 恢复码：关掉，且销毁代理侧种子。
	body, _ = json.Marshal(map[string]string{"password": testUserPass, "recovery_code": codes[0]})
	rec = f.do(t, http.MethodDelete, "/api/v1/auth/totp", string(body))
	if rec.Code != http.StatusOK {
		t.Fatalf("恢复码关闭状态码 = %d，响应 = %s", rec.Code, rec.Body.String())
	}
	var disabled totpDisableResponse
	if err := json.Unmarshal(parseResult(t, rec).Data, &disabled); err != nil {
		t.Fatalf("解析关闭响应失败: %v", err)
	}
	if disabled.Enabled {
		t.Fatalf("关闭响应 enabled 必须为 false")
	}
	if f.agent.lastDisableRef != "totp-secret-1" || f.agent.lastDisableUser != f.user.ID {
		t.Fatalf("代理销毁的引用 = %q/%d", f.agent.lastDisableRef, f.agent.lastDisableUser)
	}
	if _, exists := f.totpState(t); exists {
		t.Fatalf("关闭后库里不该留着状态行")
	}
	if total, _ := f.recoveryRows(t); total != 0 {
		t.Fatalf("关闭后恢复码应随外键级联删除，实际 %d 行", total)
	}

	// 再关一次：404（没有二次验证可关），不是 500。
	body, _ = json.Marshal(map[string]string{"password": testUserPass, "recovery_code": codes[0]})
	rec = f.do(t, http.MethodDelete, "/api/v1/auth/totp", string(body))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("重复关闭状态码 = %d，期望 404（响应 %s）", rec.Code, rec.Body.String())
	}
}

// TestTotpDisableKeepsCurrentSession 关闭会作废**其它**会话，发起请求的那条留着。
func TestTotpDisableKeepsCurrentSession(t *testing.T) {
	f := newTotpFixture(t, newFakeTotpAgent())
	codes := f.enableTotpDirect(t, "totp-secret-1")

	// f.cookie 是启用之前的那条会话（另一个"设备"）；这里再登录一条，在它上面关闭。
	other, otherCSRF := f.cookie, f.csrf
	second, secondCSRF := f.loginWith(t, map[string]any{
		"username": testUser, "password": testUserPass, "totp": "123456",
	})

	// 在第二条会话上再认证并关闭（再认证本身也要一个新时间步：同一个码不能用两次）。
	f.agent.advance(101)
	body, _ := json.Marshal(map[string]string{"password": testUserPass, "totp": "123456"})
	if rec := f.doAs(t, second, secondCSRF, http.MethodPost, "/api/v1/auth/reauth", string(body)); rec.Code != http.StatusOK {
		t.Fatalf("再认证失败: %d %s", rec.Code, rec.Body.String())
	}
	body, _ = json.Marshal(map[string]string{"password": testUserPass, "recovery_code": codes[0]})
	rec := f.doAs(t, second, secondCSRF, http.MethodDelete, "/api/v1/auth/totp", string(body))
	if rec.Code != http.StatusOK {
		t.Fatalf("关闭状态码 = %d，响应 = %s", rec.Code, rec.Body.String())
	}
	var out totpDisableResponse
	if err := json.Unmarshal(parseResult(t, rec).Data, &out); err != nil {
		t.Fatalf("解析关闭响应失败: %v", err)
	}
	if out.RevokedSessions != 1 {
		t.Fatalf("作废会话数 = %d，期望 1", out.RevokedSessions)
	}

	// 另一条会话已作废。
	if rec := f.doAs(t, other, otherCSRF, http.MethodGet, "/api/v1/auth/session", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("其它会话应已作废，状态码 = %d（响应 %s）", rec.Code, rec.Body.String())
	}
	// 发起关闭的那条还在。
	if rec := f.doAs(t, second, secondCSRF, http.MethodGet, "/api/v1/auth/session", ""); rec.Code != http.StatusOK {
		t.Fatalf("发起关闭的会话不该被作废，状态码 = %d（响应 %s）", rec.Code, rec.Body.String())
	}
}
