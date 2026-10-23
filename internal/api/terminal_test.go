package api

// P06.3 接口层用例：终端会话列表、一次性连接许可、关闭、WebSocket 握手。
//
// 这一层要证明的是**五件事**，每件都对应一条验收条款：
//
//  1. 整组接口 admin 专属：未登录 401、viewer 403（包括连接与许可——
//     只挡列表却允许连上去等于没挡）。
//  2. 没有配置代理时是 503，不是空列表与假会话。
//  3. 许可只能用一次、30 秒、绑定用户与会话；签发前先由代理判定归属
//     （别人的会话连许可都领不到）。
//  4. 握手四道检查按顺序生效：Origin 白名单 → 子协议 → 许可 → 连代理。
//     任何一道不过都不升级，且都记一条 failure 审计。
//  5. 转发是真的双向：浏览器发的字节到了代理，代理发的字节到了浏览器，
//     代理关闭后浏览器这条连接也结束。
//
// 真实 PTY、降权、超时、限额由 internal/terminal 的 Linux 用例与 VM 验收覆盖；
// 这里不冒充。

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"

	"upanel/internal/audit"
	"upanel/internal/auth"
	"upanel/internal/config"
	"upanel/internal/httpx"
	"upanel/internal/platform"
	"upanel/internal/terminal"
	"upanel/internal/version"
)

// testSessionID 是形态合法的会话 id（32 位小写十六进制）。
const testSessionID = "0123456789abcdef0123456789abcdef"

// stubTerminal 是 TerminalReader 的测试替身。
//
// 它不冒充 PTY：会话归属、限额与超时由代理侧用例覆盖，这里只验 HTTP 层的
// 权限、参数校验、许可与错误映射。
type stubTerminal struct {
	sessions    terminal.SessionsResult
	sessionsErr error

	info    terminal.SessionInfo
	infoErr error

	closeErr error

	// agentURL 非空时 Connect 会拨到它（用它模拟代理的终端 WebSocket）。
	agentURL   string
	connectErr error

	mu           sync.Mutex
	connectCalls int
	closedID     string
	closedUser   int64
	closedReason string
	sessionsUser int64
}

func (s *stubTerminal) Sessions(_ context.Context, userID int64, sessionID string) (terminal.SessionsResult, error) {
	s.mu.Lock()
	s.sessionsUser = userID
	s.mu.Unlock()
	if s.sessionsErr != nil {
		return terminal.SessionsResult{}, s.sessionsErr
	}
	result := s.sessions
	if sessionID != "" {
		kept := make([]terminal.SessionInfo, 0, 1)
		for _, item := range result.Sessions {
			if item.SessionID == sessionID {
				kept = append(kept, item)
			}
		}
		result.Sessions = kept
	}
	return result, nil
}

func (s *stubTerminal) Session(ctx context.Context, userID int64, sessionID string) (terminal.SessionInfo, terminal.SessionsResult, error) {
	if s.infoErr != nil {
		return terminal.SessionInfo{}, terminal.SessionsResult{}, s.infoErr
	}
	result, err := s.Sessions(ctx, userID, sessionID)
	if err != nil {
		return terminal.SessionInfo{}, terminal.SessionsResult{}, err
	}
	info := s.info
	info.SessionID = sessionID
	return info, result, nil
}

func (s *stubTerminal) Close(_ context.Context, userID int64, sessionID, reason string) error {
	s.mu.Lock()
	s.closedID, s.closedUser, s.closedReason = sessionID, userID, reason
	s.mu.Unlock()
	return s.closeErr
}

func (s *stubTerminal) Connect(ctx context.Context, userID int64, sessionID string) (*websocket.Conn, error) {
	s.mu.Lock()
	s.connectCalls++
	s.mu.Unlock()
	if s.connectErr != nil {
		return nil, s.connectErr
	}
	if s.agentURL == "" {
		return nil, errors.New("stubTerminal: 未配置 agentURL")
	}
	dialer := websocket.Dialer{Subprotocols: []string{terminal.ProtocolName}}
	conn, _, err := dialer.DialContext(ctx, s.agentURL, nil)
	if err != nil {
		return nil, err
	}
	return conn, nil
}

// sampleSessions 是一份“以 upanel-shell 运行”的会话列表。
func sampleSessions() terminal.SessionsResult {
	created := time.Date(2026, 10, 21, 3, 4, 5, 0, time.UTC)
	attached := created.Add(2 * time.Second)
	return terminal.SessionsResult{
		Account:                "upanel-shell",
		AccountUID:             998,
		IdleTimeoutSeconds:     900,
		MaxDurationSeconds:     3600,
		DisconnectGraceSeconds: 60,
		MaxPerUser:             1,
		MaxSessions:            2,
		Sessions: []terminal.SessionInfo{
			{
				SessionID:          testSessionID,
				UserID:             1,
				Account:            "upanel-shell",
				AccountUID:         998,
				State:              terminal.StateAttached,
				Cols:               120,
				Rows:               32,
				CreatedAt:          created,
				LastActivityAt:     created,
				AttachedAt:         &attached,
				IdleDeadline:       created.Add(15 * time.Minute),
				ExpiresAt:          created.Add(time.Hour),
				IdleTimeoutSeconds: 900,
				MaxDurationSeconds: 3600,
			},
		},
	}
}

// terminalFixture 是终端用例的固定装置。
type terminalFixture struct {
	srv    *Server
	cookie *http.Cookie
	csrf   string
	reader *stubTerminal
}

func newTerminalFixture(t *testing.T, reader *stubTerminal) *terminalFixture {
	t.Helper()
	authSvc, aud, db := newTestServicesWithDB(t)
	var injected TerminalReader
	if reader != nil {
		injected = reader
	}
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
		Terminal:        injected,
	})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	seedTestUser(t, srv.opts.Auth, testUser, testUserPass)
	cookie, csrf := loginOK(t, srv)
	return &terminalFixture{srv: srv, cookie: cookie, csrf: csrf, reader: reader}
}

// do 以管理员会话 + CSRF 发送请求。
func (f *terminalFixture) do(t *testing.T, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set("Origin", testOrigin)
	req.Header.Set("Cookie", f.cookie.String())
	if method != http.MethodGet && method != http.MethodHead {
		req.Header.Set(CSRFTokenHeader, f.csrf)
	}
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	return rec
}

// grant 领一张许可并返回其内容。
func (f *terminalFixture) grant(t *testing.T) terminalGrantResponse {
	t.Helper()
	rec := f.do(t, http.MethodPost, "/api/v1/terminal/sessions/"+testSessionID+"/connection-grants")
	if rec.Code != http.StatusOK {
		t.Fatalf("领取许可状态码 = %d；响应 %s", rec.Code, rec.Body.String())
	}
	var out terminalGrantResponse
	if err := json.Unmarshal(parseResult(t, rec).Data, &out); err != nil {
		t.Fatalf("许可响应不是契约形状: %v", err)
	}
	return out
}

// terminalPaths 是所有终端接口（连接路径在这里只验“没登录/不是管理员”时被拦下）。
func terminalPaths() []string {
	return []string{
		"/api/v1/terminal/sessions",
		"/api/v1/terminal/sessions/" + testSessionID + "/connect",
	}
}

// TestTerminalRequiresSession 未登录一律 401。
func TestTerminalRequiresSession(t *testing.T) {
	f := newTerminalFixture(t, &stubTerminal{sessions: sampleSessions()})
	for _, path := range terminalPaths() {
		rec := adminGet(t, f.srv, nil, path)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s 未登录状态码 = %d，期望 401", path, rec.Code)
		}
	}
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/terminal/sessions/"+testSessionID+"/connection-grants", nil)
	req.Header.Set("Origin", testOrigin)
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("许可路径未登录状态码 = %d，期望 401", rec.Code)
	}
}

// TestTerminalForbiddenForViewer 终端是管理员能力（PLAN.md 第 15 节）。
func TestTerminalForbiddenForViewer(t *testing.T) {
	f := newTerminalFixture(t, &stubTerminal{sessions: sampleSessions()})
	if _, err := f.srv.opts.Auth.CreateUser(context.Background(), auth.CreateUserInput{
		Username: "watcher", DisplayName: "watcher", Role: auth.RoleViewer, Password: "watcher-horse-9",
	}); err != nil {
		t.Fatalf("创建 viewer 失败: %v", err)
	}
	cookie, csrf := loginAsUser(t, f.srv, "watcher", "watcher-horse-9")

	for _, path := range terminalPaths() {
		rec := adminGet(t, f.srv, cookie, path)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s viewer 状态码 = %d，期望 403", path, rec.Code)
		}
		got := parseResult(t, rec).Error
		if got == nil || got.Code != string(httpx.CodeForbidden) {
			t.Fatalf("%s 错误码 = %+v", path, got)
		}
	}

	// 领取许可与关闭同样是管理员能力：viewer 连许可都拿不到。
	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/api/v1/terminal/sessions/" + testSessionID + "/connection-grants"},
		{http.MethodDelete, "/api/v1/terminal/sessions/" + testSessionID},
	} {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		req.Header.Set("Origin", testOrigin)
		req.Header.Set("Cookie", cookie.String())
		req.Header.Set(CSRFTokenHeader, csrf)
		rec := httptest.NewRecorder()
		f.srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s %s viewer 状态码 = %d，期望 403", tc.method, tc.path, rec.Code)
		}
	}
}

// TestTerminalUnavailableWithoutAgent 没有配置代理时是 503，不是空列表。
func TestTerminalUnavailableWithoutAgent(t *testing.T) {
	f := newTerminalFixture(t, nil)
	for _, path := range terminalPaths() {
		rec := adminGet(t, f.srv, f.cookie, path)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s 状态码 = %d，期望 503", path, rec.Code)
		}
		got := parseResult(t, rec).Error
		if got == nil || got.Code != string(httpx.CodeAgentUnavailable) {
			t.Fatalf("%s 错误码 = %+v", path, got)
		}
	}
}

// TestTerminalSessionsShape 会话列表带上终端边界（账号、uid、时限、数量）。
func TestTerminalSessionsShape(t *testing.T) {
	f := newTerminalFixture(t, &stubTerminal{sessions: sampleSessions()})
	rec := adminGet(t, f.srv, f.cookie, "/api/v1/terminal/sessions")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d；响应 %s", rec.Code, rec.Body.String())
	}
	result := parseResult(t, rec)
	var data struct {
		Account            string                 `json:"account"`
		AccountUID         uint32                 `json:"account_uid"`
		IdleTimeoutSeconds int                    `json:"idle_timeout_seconds"`
		MaxDurationSeconds int                    `json:"max_duration_seconds"`
		MaxPerUser         int                    `json:"max_per_user"`
		MaxSessions        int                    `json:"max_sessions"`
		Sessions           []terminal.SessionInfo `json:"sessions"`
	}
	if err := json.Unmarshal(result.Data, &data); err != nil {
		t.Fatalf("data 不是契约形状: %v（%s）", err, result.Data)
	}
	if data.Account != "upanel-shell" || data.AccountUID != 998 {
		t.Fatalf("终端边界字段不对: %+v", data)
	}
	if data.IdleTimeoutSeconds != 900 || data.MaxDurationSeconds != 3600 {
		t.Fatalf("时限字段 = %d / %d", data.IdleTimeoutSeconds, data.MaxDurationSeconds)
	}
	if data.MaxPerUser != 1 || data.MaxSessions != 2 {
		t.Fatalf("数量上限 = %d / %d", data.MaxPerUser, data.MaxSessions)
	}
	if len(data.Sessions) != 1 || data.Sessions[0].SessionID != testSessionID {
		t.Fatalf("sessions = %+v", data.Sessions)
	}
	// user_id 来自服务端会话，从不从请求参数取。
	if f.reader.sessionsUser != 1 {
		t.Fatalf("查询用的 user_id = %d，期望登录账号的 id 1", f.reader.sessionsUser)
	}
}

// TestTerminalSessionsEmptyIsArray 没有会话时是空数组，不是 null。
func TestTerminalSessionsEmptyIsArray(t *testing.T) {
	sessions := sampleSessions()
	sessions.Sessions = nil
	f := newTerminalFixture(t, &stubTerminal{sessions: sessions})
	rec := adminGet(t, f.srv, f.cookie, "/api/v1/terminal/sessions")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"sessions":[]`) {
		t.Fatalf("空会话列表必须是 []，响应 %s", rec.Body.String())
	}
}

// TestTerminalGrantRequiresCSRF 领许可必须带 CSRF 令牌：它是这次升级的 CSRF 替身。
func TestTerminalGrantRequiresCSRF(t *testing.T) {
	f := newTerminalFixture(t, &stubTerminal{sessions: sampleSessions()})
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/terminal/sessions/"+testSessionID+"/connection-grants", nil)
	req.Header.Set("Origin", testOrigin)
	req.Header.Set("Cookie", f.cookie.String())
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("无 CSRF 状态码 = %d，期望 403", rec.Code)
	}
	if got := parseResult(t, rec).Error; got == nil || got.Code != string(httpx.CodeCSRFFailed) {
		t.Fatalf("错误码 = %+v", got)
	}
}

// TestTerminalGrantForeignSessionIsNotFound 别人的会话连许可都领不到。
func TestTerminalGrantForeignSessionIsNotFound(t *testing.T) {
	reader := &stubTerminal{
		sessions: sampleSessions(),
		infoErr:  terminal.ErrNotFound,
	}
	f := newTerminalFixture(t, reader)
	rec := f.do(t, http.MethodPost, "/api/v1/terminal/sessions/"+testSessionID+"/connection-grants")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("状态码 = %d，期望 404；响应 %s", rec.Code, rec.Body.String())
	}
	if got := parseResult(t, rec).Error; got == nil || got.Code != string(httpx.CodeNotFound) {
		t.Fatalf("错误码 = %+v", got)
	}
}

// TestTerminalGrantBadSessionIDIsNotFound 形态不对的 id 直接 404，不进代理。
func TestTerminalGrantBadSessionIDIsNotFound(t *testing.T) {
	f := newTerminalFixture(t, &stubTerminal{sessions: sampleSessions()})
	for _, id := range []string{"short", strings.Repeat("A", 32), strings.Repeat("z", 32)} {
		rec := f.do(t, http.MethodPost, "/api/v1/terminal/sessions/"+id+"/connection-grants")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("id=%q 状态码 = %d，期望 404", id, rec.Code)
		}
	}
}

// TestTerminalGrantShapeOfResponse 许可的返回体必须自带协议名与路径。
func TestTerminalGrantShapeOfResponse(t *testing.T) {
	f := newTerminalFixture(t, &stubTerminal{sessions: sampleSessions()})
	out := f.grant(t)
	if !validGrantID(out.Grant) {
		t.Fatalf("许可 id = %q，期望 32 位小写十六进制", out.Grant)
	}
	if out.Protocol != terminal.ProtocolName {
		t.Fatalf("protocol = %q", out.Protocol)
	}
	if out.GrantProtocol != terminal.GrantProtocolPrefix+out.Grant {
		t.Fatalf("grant_protocol = %q", out.GrantProtocol)
	}
	if out.ConnectPath != "/terminal/sessions/"+testSessionID+"/connect" {
		t.Fatalf("connect_path = %q", out.ConnectPath)
	}
	expires, err := time.Parse(time.RFC3339, out.ExpiresAt)
	if err != nil {
		t.Fatalf("expires_at 不是 RFC3339: %v", err)
	}
	// 30 秒寿命，允许测试耗时带来的几秒误差，但绝不能允许“签出一张长效许可”。
	if d := time.Until(expires); d <= 0 || d > terminalGrantTTL {
		t.Fatalf("许可剩余寿命 = %s，期望 (0, %s]", d, terminalGrantTTL)
	}
}

// TestTerminalConnectRequiresOrigin 浏览器握手必须带白名单内的 Origin。
func TestTerminalConnectRequiresOrigin(t *testing.T) {
	f := newTerminalFixture(t, &stubTerminal{sessions: sampleSessions()})
	out := f.grant(t)

	for _, origin := range []string{"", "https://evil.example"} {
		req := httptest.NewRequest(http.MethodGet,
			"/api/v1/terminal/sessions/"+testSessionID+"/connect", nil)
		req.Header.Set("Cookie", f.cookie.String())
		req.Header.Set("Sec-WebSocket-Protocol", terminal.ProtocolName+", "+out.GrantProtocol)
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		rec := httptest.NewRecorder()
		f.srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("origin=%q 状态码 = %d，期望 403；响应 %s", origin, rec.Code, rec.Body.String())
		}
		if got := parseResult(t, rec).Error; got == nil || got.Code != string(httpx.CodeCSRFFailed) {
			t.Fatalf("origin=%q 错误码 = %+v", origin, got)
		}
	}
	// 上述两次都被 Origin 拦下，许可没有被动用，因此这里还能再领一张。
	if _, _, err := f.srv.terminalGrants.issue(1, testSessionID); err != nil {
		t.Fatalf("签发许可失败: %v", err)
	}
}

// TestTerminalConnectRequiresSubprotocol 没声明子协议就不是这个前端，不升级。
func TestTerminalConnectRequiresSubprotocol(t *testing.T) {
	f := newTerminalFixture(t, &stubTerminal{sessions: sampleSessions()})
	out := f.grant(t)

	for _, protocols := range []string{"", "other.protocol", out.GrantProtocol} {
		req := httptest.NewRequest(http.MethodGet,
			"/api/v1/terminal/sessions/"+testSessionID+"/connect", nil)
		req.Header.Set("Origin", testOrigin)
		req.Header.Set("Cookie", f.cookie.String())
		if protocols != "" {
			req.Header.Set("Sec-WebSocket-Protocol", protocols)
		}
		rec := httptest.NewRecorder()
		f.srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("protocols=%q 状态码 = %d，期望 400；响应 %s", protocols, rec.Code, rec.Body.String())
		}
		if got := parseResult(t, rec).Error; got == nil || got.Code != string(httpx.CodeValidationError) {
			t.Fatalf("protocols=%q 错误码 = %+v", protocols, got)
		}
	}
}

// TestTerminalConnectRejectsUnknownGrant 没领许可、或领了别人的，都连不上。
func TestTerminalConnectRejectsUnknownGrant(t *testing.T) {
	f := newTerminalFixture(t, &stubTerminal{sessions: sampleSessions()})
	other := strings.Repeat("ab", 16) // 形态合法但不存在

	for _, grant := range []string{"", other, "not-a-grant"} {
		req := httptest.NewRequest(http.MethodGet,
			"/api/v1/terminal/sessions/"+testSessionID+"/connect", nil)
		req.Header.Set("Origin", testOrigin)
		req.Header.Set("Cookie", f.cookie.String())
		req.Header.Set("Sec-WebSocket-Protocol",
			terminal.ProtocolName+", "+terminal.GrantProtocolPrefix+grant)
		rec := httptest.NewRecorder()
		f.srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("grant=%q 状态码 = %d，期望 403；响应 %s", grant, rec.Code, rec.Body.String())
		}
	}
}

// TestTerminalGrantIsSingleUse 许可用一次就作废，即使那次连接失败了。
func TestTerminalGrantIsSingleUse(t *testing.T) {
	reader := &stubTerminal{sessions: sampleSessions(), connectErr: errors.New("拨号失败")}
	f := newTerminalFixture(t, reader)
	out := f.grant(t)

	attempt := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet,
			"/api/v1/terminal/sessions/"+testSessionID+"/connect", nil)
		req.Header.Set("Origin", testOrigin)
		req.Header.Set("Cookie", f.cookie.String())
		req.Header.Set("Sec-WebSocket-Protocol",
			terminal.ProtocolName+", "+out.GrantProtocol)
		rec := httptest.NewRecorder()
		f.srv.Handler().ServeHTTP(rec, req)
		return rec
	}

	first := attempt()
	if first.Code == http.StatusOK {
		t.Fatalf("代理拨号失败却升级成功")
	}
	if got := parseResult(t, first).Error; got == nil || got.Code != string(httpx.CodeInternalError) {
		// 拨号失败不是“代理不可用”也不是代理的明确拒绝：这类未分类错误按 500 处理，
		// 关键是它没有伪装成 403（那会被前端当成“许可无效”）。
		t.Fatalf("首次失败错误码 = %+v", got)
	}
	second := attempt()
	if second.Code != http.StatusForbidden {
		t.Fatalf("同一张许可第二次状态码 = %d，期望 403", second.Code)
	}
	if reader.connectCalls != 1 {
		t.Fatalf("代理拨号次数 = %d，期望 1（第二次在许可处就被拦下）", reader.connectCalls)
	}
}

// TestTerminalClose 关闭会话：CSRF、reason 校验、审计、返回体。
func TestTerminalClose(t *testing.T) {
	f := newTerminalFixture(t, &stubTerminal{sessions: sampleSessions()})

	rec := f.do(t, http.MethodDelete, "/api/v1/terminal/sessions/"+testSessionID+"?reason=idle")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d；响应 %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"closed":true`) {
		t.Fatalf("返回体 = %s", rec.Body.String())
	}
	if f.reader.closedID != testSessionID || f.reader.closedReason != "idle" || f.reader.closedUser != 1 {
		t.Fatalf("代理收到的关闭请求 = %q/%q/%d", f.reader.closedID, f.reader.closedReason, f.reader.closedUser)
	}

	// 未登记的 reason 是请求内容不对，而不是“关闭失败”。
	bad := f.do(t, http.MethodDelete, "/api/v1/terminal/sessions/"+testSessionID+"?reason=whatever")
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("非法 reason 状态码 = %d，期望 400", bad.Code)
	}

	// 没有 CSRF 令牌不许关。
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/terminal/sessions/"+testSessionID, nil)
	req.Header.Set("Origin", testOrigin)
	req.Header.Set("Cookie", f.cookie.String())
	noCSRF := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(noCSRF, req)
	if noCSRF.Code != http.StatusForbidden {
		t.Fatalf("无 CSRF 状态码 = %d，期望 403", noCSRF.Code)
	}
}

// TestTerminalCloseAgentErrorsSurvive DELETE 代理的拒绝原样转出，不能变成 500。
func TestTerminalCloseAgentErrorsSurvive(t *testing.T) {
	reader := &stubTerminal{
		sessions: sampleSessions(),
		closeErr: &terminal.RemoteError{Status: http.StatusConflict,
			Code: httpx.CodeResourceBusy, Message: "会话已被占用"},
	}
	f := newTerminalFixture(t, reader)
	rec := f.do(t, http.MethodDelete, "/api/v1/terminal/sessions/"+testSessionID)
	if rec.Code != http.StatusConflict {
		t.Fatalf("状态码 = %d，期望 409；响应 %s", rec.Code, rec.Body.String())
	}
	if got := parseResult(t, rec).Error; got == nil || got.Code != string(httpx.CodeResourceBusy) {
		t.Fatalf("错误码 = %+v", got)
	}
}

// TestTerminalGrantStore 许可表的四条规则：单次、过期、归属、上限。
func TestTerminalGrantStore(t *testing.T) {
	grants := newTerminalGrants()
	clock := time.Date(2026, 10, 21, 3, 0, 0, 0, time.UTC)
	grants.now = func() time.Time { return clock }

	id, expiresAt, err := grants.issue(7, testSessionID)
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}
	if !expiresAt.Equal(clock.Add(terminalGrantTTL)) {
		t.Fatalf("到期时间 = %s", expiresAt)
	}
	// 归属不匹配：作废。
	if grants.consume(id, 8, testSessionID) {
		t.Fatalf("别的用户不该能用这张许可")
	}
	if grants.consume(id, 7, testSessionID) {
		t.Fatalf("失败的校验也必须把许可作废（一次性）")
	}

	// 过期：作废。
	id2, _, err := grants.issue(7, testSessionID)
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}
	clock = clock.Add(terminalGrantTTL + time.Second)
	if grants.consume(id2, 7, testSessionID) {
		t.Fatalf("过期许可不该可用")
	}

	// 上限：过期项先被清掉，所以这里还能继续签发。
	for i := 0; i < terminalGrantMax; i++ {
		if _, _, err := grants.issue(7, testSessionID); err != nil {
			t.Fatalf("第 %d 张签发失败: %v", i+1, err)
		}
	}
	if _, _, err := grants.issue(7, testSessionID); !errors.Is(err, errGrantStoreFull) {
		t.Fatalf("超过上限的错误 = %v，期望 errGrantStoreFull", err)
	}
}

// TestTerminalConnectRelaysBothWays 端到端：真实握手 + 双向转发 + 代理关闭后收尾。
//
// 代理那一侧用一个最小 WebSocket 服务代替：它按终端的帧协议发一条 ready，
// 之后把收到的每条消息原样回一条 output，收到 "bye" 就发 exit 并关闭。
// 这样断言的是“字节在这条链路里真的两个方向都通”，而不是帧语义
// （帧语义由 internal/terminal 的用例覆盖）。
func TestTerminalConnectRelaysBothWays(t *testing.T) {
	agentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		up := websocket.Upgrader{Subprotocols: []string{terminal.ProtocolName}}
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		if err := conn.WriteMessage(websocket.TextMessage,
			[]byte(`{"type":"ready","account":"upanel-shell","account_uid":998}`)); err != nil {
			return
		}
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if string(data) == `"bye"` {
				_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"exit","exit_code":0}`))
				return
			}
			if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
				return
			}
		}
	}))
	defer agentServer.Close()
	agentURL := "ws" + strings.TrimPrefix(agentServer.URL, "http")

	reader := &stubTerminal{sessions: sampleSessions(), agentURL: agentURL}
	f := newTerminalFixture(t, reader)
	out := f.grant(t)

	httpServer := httptest.NewServer(f.srv.Handler())
	defer httpServer.Close()
	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http") +
		"/api/v1/terminal/sessions/" + testSessionID + "/connect"

	dialer := websocket.Dialer{
		HandshakeTimeout: 5 * time.Second,
		Subprotocols:     []string{terminal.ProtocolName, out.GrantProtocol},
		// 许可只在子协议里，URL 上不带任何凭据（CONTRACTS.md 第 10 节）。
	}
	header := http.Header{}
	header.Set("Origin", testOrigin)
	header.Set("Cookie", f.cookie.String())

	conn, resp, err := dialer.Dial(wsURL, header)
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		t.Fatalf("握手失败: %v（状态码 %d）", err, status)
	}
	defer func() { _ = conn.Close() }()

	// 服务端只回 upanel.term.v1：许可不写进响应头。
	if got := conn.Subprotocol(); got != terminal.ProtocolName {
		t.Fatalf("选中子协议 = %q，期望 %q", got, terminal.ProtocolName)
	}

	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	typ, data, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("读 ready 失败: %v", err)
	}
	if typ != websocket.TextMessage || !strings.Contains(string(data), `"ready"`) {
		t.Fatalf("第一帧 = %d %s", typ, data)
	}

	// 浏览器 → 代理 → 浏览器：原样回来。
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"input","data":"bHM="}`)); err != nil {
		t.Fatalf("写 input 失败: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	_, echoed, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("读回显失败: %v", err)
	}
	if string(echoed) != `{"type":"input","data":"bHM="}` {
		t.Fatalf("回显 = %s", echoed)
	}

	// 代理关闭后，浏览器这一侧也要结束（而不是留一个永不结束的连接）。
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`"bye"`)); err != nil {
		t.Fatalf("写 bye 失败: %v", err)
	}
	sawExit := false
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			if !sawExit {
				t.Fatalf("连接结束前没有收到 exit 帧: %v", err)
			}
			break
		}
		if strings.Contains(string(msg), `"exit"`) {
			sawExit = true
		}
	}
	if reader.connectCalls != 1 {
		t.Fatalf("代理拨号次数 = %d", reader.connectCalls)
	}
}

// TestTerminalConnectForeignGrantRejectedWhenSessionNotOwned
// 归属是代理判定的：代理说“不是你的”，Web 连权限检查后的许可都签不出来。
func TestTerminalConnectForeignGrantRejectedWhenSessionNotOwned(t *testing.T) {
	reader := &stubTerminal{sessions: sampleSessions(), infoErr: terminal.ErrNotOwner}
	f := newTerminalFixture(t, reader)
	rec := f.do(t, http.MethodPost, "/api/v1/terminal/sessions/"+testSessionID+"/connection-grants")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("状态码 = %d，期望 403；响应 %s", rec.Code, rec.Body.String())
	}
	if got := parseResult(t, rec).Error; got == nil || got.Code != string(httpx.CodeForbidden) {
		t.Fatalf("错误码 = %+v", got)
	}
}

// TestSendableCloseCode 证明“给浏览器的关闭码”只放行可以合法发送的那些。
//
// 1005/1006/1015 是本地诊断码：它们由协议栈自己产生，写进 close 帧本身是协议错误
// （gorilla 会拒绝）。其余越界值同样不发——浏览器收到一个自己也不认识的码，只会
// 把它当成“异常关闭”，那正是这一帧要消除的东西。
func TestSendableCloseCode(t *testing.T) {
	cases := []struct {
		code int
		want bool
	}{
		{websocket.CloseNormalClosure, true},
		{terminal.CloseCodeFrameInvalid, true},
		{terminal.CloseCodeRateLimited, true},
		{websocket.CloseNoStatusReceived, false},
		{websocket.CloseAbnormalClosure, false},
		{websocket.CloseTLSHandshake, false},
		{999, false},
		{5000, false},
	}
	for _, c := range cases {
		if got := sendableCloseCode(c.code); got != c.want {
			t.Fatalf("sendableCloseCode(%d) = %v，期望 %v", c.code, got, c.want)
		}
	}
}

// TestDenyTargetPointsAtTerminalSession 证明中间件拒绝的审计目标指向会话。
//
// 一条 terminal.attach 的越权尝试如果把 target 记成固定的 "user"，事后只能知道
// “有人被拒过”，不知道他试的是哪个会话——而那正是要查的东西。
func TestDenyTargetPointsAtTerminalSession(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Params = gin.Params{{Key: "id", Value: testSessionID}}
	if got := denyTarget(c, audit.ActionTerminalAttach); got != "terminal:"+testSessionID {
		t.Fatalf("attach 的 target = %q", got)
	}
	if got := denyTarget(c, audit.ActionTerminalClose); got != "terminal:"+testSessionID {
		t.Fatalf("close 的 target = %q", got)
	}
	// 其余动作仍然按用户维度记，不受本次改动影响。
	if got := denyTarget(c, audit.ActionUserManage); got != "user" {
		t.Fatalf("用户路由的 target = %q", got)
	}
	// 形态不对的 :id 不进审计目标：记一个拼出来的值等于记了个假的对象。
	c.Params = gin.Params{{Key: "id", Value: "not-a-session-id"}}
	if got := denyTarget(c, audit.ActionTerminalAttach); got != "user" {
		t.Fatalf("非法 id 的 target = %q", got)
	}
}
