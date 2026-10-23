package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"upanel/internal/agentclient"
	"upanel/internal/config"
	"upanel/internal/operations"
	"upanel/internal/platform"
	"upanel/internal/version"
)

// stubAgent 是接口层用例用的假代理：可以按需返回计划或错误，并记录收到的请求。
type stubAgent struct {
	plan agentclient.PreflightPlan
	err  error

	calls []agentclient.PreflightInput

	// P03.2：操作提交、查询与取消。
	op        agentclient.Operation
	opErr     error
	getOp     agentclient.Operation
	getErr    error
	cancelOp  agentclient.Operation
	cancelErr error

	// P03.3：代理侧事件流水（镜像进 job_events 的那一份）。
	events    []agentclient.OperationEvent
	eventsErr error

	opCalls     []agentclient.OperationInput
	getCalls    []string
	cancelCalls []string
	eventCalls  []string
}

func (s *stubAgent) CreatePreflight(_ context.Context, in agentclient.PreflightInput) (agentclient.PreflightPlan, error) {
	s.calls = append(s.calls, in)
	if s.err != nil {
		return agentclient.PreflightPlan{}, s.err
	}
	return s.plan, nil
}

func (s *stubAgent) CreateOperation(_ context.Context, in agentclient.OperationInput) (agentclient.Operation, error) {
	s.opCalls = append(s.opCalls, in)
	if s.opErr != nil {
		return agentclient.Operation{}, s.opErr
	}
	return s.op, nil
}

func (s *stubAgent) GetOperation(_ context.Context, operationID string) (agentclient.Operation, error) {
	s.getCalls = append(s.getCalls, operationID)
	if s.getErr != nil {
		return agentclient.Operation{}, s.getErr
	}
	return s.getOp, nil
}

func (s *stubAgent) CancelOperation(_ context.Context, operationID string) (agentclient.Operation, error) {
	s.cancelCalls = append(s.cancelCalls, operationID)
	if s.cancelErr != nil {
		return agentclient.Operation{}, s.cancelErr
	}
	return s.cancelOp, nil
}

func (s *stubAgent) OperationEvents(_ context.Context, operationID string) ([]agentclient.OperationEvent, error) {
	s.eventCalls = append(s.eventCalls, operationID)
	if s.eventsErr != nil {
		return nil, s.eventsErr
	}
	return s.events, nil
}

// stubPlan 返回一份完整的代理侧计划。
func stubPlan() agentclient.PreflightPlan {
	now := time.Now().UTC().Truncate(time.Second)
	return agentclient.PreflightPlan{
		Ref:              "pf_0123456789abcdef0123456789abcdef",
		Action:           "acceptance.probe",
		NormalizedParams: json.RawMessage(`{"label":"alpha"}`),
		Target: agentclient.PreflightTarget{
			DisplayName:         "验收测试对象（alpha）",
			IdentityFingerprint: "fp-acceptance-probe",
			StateVersion:        "1",
		},
		ExpiresAt:      now.Add(120 * time.Second).Format(time.RFC3339),
		Allowed:        true,
		Blockers:       []agentclient.PreflightNotice{},
		Warnings:       []agentclient.PreflightNotice{{Code: "acceptance_only", Message: "验收专用动作"}},
		Impacts:        []agentclient.PreflightImpact{{Kind: "none", Description: "不改动系统状态"}},
		RequiresReauth: true,
		RequiresPhrase: true,
		Cancellable:    false,
		Locks:          []string{"acceptance:probe"},
	}
}

// newPreflightFixture 构造带指定代理的接口测试装置（真实数据库、真实登录）。
func newPreflightFixture(t *testing.T, agent operations.Agent) *adminFixture {
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
		Operations:      newTestOperations(t, db, agent),
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

// preflightBody 构造预检请求体。
func preflightBody(action, params string) string {
	return `{"action":"` + action + `","params":` + params + `}`
}

// preflightResult 是接口返回的预检 DTO（按契约字段名解析）。
type preflightResult struct {
	PreflightID      string          `json:"preflight_id"`
	Action           string          `json:"action"`
	NormalizedParams json.RawMessage `json:"normalized_params"`
	Target           struct {
		DisplayName         string `json:"display_name"`
		IdentityFingerprint string `json:"identity_fingerprint"`
	} `json:"target"`
	ExpiresAt string `json:"expires_at"`
	Allowed   bool   `json:"allowed"`
	Blockers  []struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"blockers"`
	Warnings []struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"warnings"`
	Impacts []struct {
		Kind        string `json:"kind"`
		Description string `json:"description"`
	} `json:"impacts"`
	RequiresReauth     bool     `json:"requires_reauth"`
	ConfirmationPhrase *string  `json:"confirmation_phrase"`
	Cancellable        bool     `json:"cancellable"`
	Locks              []string `json:"locks"`
}

func TestPreflightReturnsPlanToAdmin(t *testing.T) {
	agent := &stubAgent{plan: stubPlan()}
	agent.plan.ConfirmationPhrase = "PROBE K7M2QX"
	f := newPreflightFixture(t, agent)

	rec := f.do(t, http.MethodPost, "/api/v1/operations/preflight",
		preflightBody("acceptance.probe", `{"label":"alpha"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200；响应 %s", rec.Code, rec.Body.String())
	}
	var pre preflightResult
	if err := json.Unmarshal(parseResult(t, rec).Data, &pre); err != nil {
		t.Fatalf("解析预检结果失败: %v", err)
	}
	if !strings.HasPrefix(pre.PreflightID, "pf_") {
		t.Fatalf("preflight_id = %q", pre.PreflightID)
	}
	// 公开 DTO 必须隐藏 Web 库里的代理引用：那是内部绑定信息。
	if strings.Contains(rec.Body.String(), "pf_0123456789abcdef0123456789abcdef") {
		t.Fatalf("响应不应包含代理侧计划引用: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "state_version") {
		t.Fatalf("state_version 属于内部绑定信息，不应出现在公开 DTO: %s", rec.Body.String())
	}
	if pre.Action != "acceptance.probe" || !pre.Allowed {
		t.Fatalf("预检 = %+v", pre)
	}
	if pre.Target.IdentityFingerprint != "fp-acceptance-probe" || pre.Target.DisplayName == "" {
		t.Fatalf("目标 = %+v", pre.Target)
	}
	if pre.ConfirmationPhrase == nil || *pre.ConfirmationPhrase != "PROBE K7M2QX" {
		t.Fatalf("确认短语 = %v", pre.ConfirmationPhrase)
	}
	if !pre.RequiresReauth || pre.Cancellable {
		t.Fatalf("策略字段 = %+v", pre)
	}
	if len(pre.Warnings) != 1 || len(pre.Impacts) != 1 || len(pre.Locks) != 1 {
		t.Fatalf("列表字段 = %+v", pre)
	}
	if pre.Blockers == nil {
		t.Fatal("blockers 必须是 []，不能是 null")
	}
	if _, err := time.Parse(time.RFC3339, pre.ExpiresAt); err != nil {
		t.Fatalf("expires_at 不是 RFC3339: %q", pre.ExpiresAt)
	}

	// 身份必须来自会话，而不是请求体：代理看到的 user_id 应是登录账号。
	if len(agent.calls) != 1 {
		t.Fatalf("代理被调用 %d 次", len(agent.calls))
	}
	if agent.calls[0].Actor.UserID <= 0 {
		t.Fatalf("操作者 user_id = %d", agent.calls[0].Actor.UserID)
	}
	if len(agent.calls[0].Actor.SessionIDHash) != 64 {
		t.Fatalf("会话哈希 = %q", agent.calls[0].Actor.SessionIDHash)
	}

	// 审计要留痕：预检是“准备执行危险动作”的明确意图。
	var action, outcome, target string
	err := f.db.SQL().QueryRowContext(context.Background(),
		`SELECT action, outcome, target FROM audit_events ORDER BY id DESC LIMIT 1`).
		Scan(&action, &outcome, &target)
	if err != nil {
		t.Fatalf("读取审计事件失败: %v", err)
	}
	if action != "operation.preflight" || outcome != "success" {
		t.Fatalf("审计 = %s / %s", action, outcome)
	}
	if target != "preflight:acceptance.probe" {
		t.Fatalf("审计目标 = %q", target)
	}
	var details string
	if err := f.db.SQL().QueryRowContext(context.Background(),
		`SELECT redacted_details FROM audit_events ORDER BY id DESC LIMIT 1`).Scan(&details); err != nil {
		t.Fatalf("读取审计详情失败: %v", err)
	}
	if !strings.Contains(details, pre.PreflightID) {
		t.Fatalf("审计详情里应记录预检 id: %q", details)
	}
	// 审计不能记录确认短语明文。
	var allDetails string
	if err := f.db.SQL().QueryRowContext(context.Background(),
		`SELECT group_concat(redacted_details, ',') FROM audit_events`).Scan(&allDetails); err != nil {
		t.Fatalf("读取审计详情失败: %v", err)
	}
	if strings.Contains(allDetails, "PROBE K7M2QX") {
		t.Fatal("审计详情里出现了确认短语明文")
	}
}

func TestPreflightWithoutPhraseReturnsNull(t *testing.T) {
	plan := stubPlan()
	plan.RequiresPhrase = false
	plan.ConfirmationPhrase = ""
	agent := &stubAgent{plan: plan}
	f := newPreflightFixture(t, agent)

	rec := f.do(t, http.MethodPost, "/api/v1/operations/preflight", preflightBody("acceptance.ping", `{}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d；响应 %s", rec.Code, rec.Body.String())
	}
	// 字段必须存在且为 null：省略会让前端分不清“不需要短语”和“服务端漏了”。
	if !strings.Contains(rec.Body.String(), `"confirmation_phrase":null`) {
		t.Fatalf("不要求短语时必须返回 null: %s", rec.Body.String())
	}
}

func TestPreflightRequiresAdminAndCSRF(t *testing.T) {
	agent := &stubAgent{plan: stubPlan()}
	f := newPreflightFixture(t, agent)

	// viewer 越权。
	f.createUser(t, "viewer1", "viewer", "viewer-horse-9")
	viewerCookie, viewerCSRF := loginAsUser(t, f.srv, "viewer1", "viewer-horse-9")
	rec := f.doAs(t, viewerCookie, viewerCSRF, http.MethodPost,
		"/api/v1/operations/preflight", preflightBody("acceptance.probe", `{}`))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("viewer 状态码 = %d，期望 403；响应 %s", rec.Code, rec.Body.String())
	}

	// 缺少 CSRF 令牌。
	req := httptest.NewRequest(http.MethodPost, "/api/v1/operations/preflight",
		strings.NewReader(preflightBody("acceptance.probe", `{}`)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", testOrigins()[0])
	req.Header.Set("Cookie", f.cookie.String())
	rec = httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("缺 CSRF 状态码 = %d，期望 403；响应 %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "CSRF_FAILED") {
		t.Fatalf("错误码 = %s", rec.Body.String())
	}

	// 未登录。
	req = httptest.NewRequest(http.MethodPost, "/api/v1/operations/preflight",
		strings.NewReader(preflightBody("acceptance.probe", `{}`)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", testOrigins()[0])
	rec = httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("未登录状态码 = %d，期望 401；响应 %s", rec.Code, rec.Body.String())
	}

	if len(agent.calls) != 0 {
		t.Fatalf("被拒绝的请求不应打到代理，实际 %d 次", len(agent.calls))
	}
}

func TestPreflightAgentUnavailable(t *testing.T) {
	// 没有任何代理客户端（mock 模式）：必须是 503，而不是伪造一份“允许执行”的计划。
	f := newPreflightFixture(t, nil)
	rec := f.do(t, http.MethodPost, "/api/v1/operations/preflight", preflightBody("acceptance.probe", `{}`))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("状态码 = %d，期望 503；响应 %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "AGENT_UNAVAILABLE") {
		t.Fatalf("错误码 = %s", rec.Body.String())
	}
}

func TestPreflightAgentUnreachable(t *testing.T) {
	agent := &stubAgent{err: &agentclient.UnavailableError{Err: errors.New("connection refused")}}
	f := newPreflightFixture(t, agent)
	rec := f.do(t, http.MethodPost, "/api/v1/operations/preflight", preflightBody("acceptance.probe", `{}`))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("状态码 = %d，期望 503；响应 %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "AGENT_UNAVAILABLE") {
		t.Fatalf("错误码 = %s", rec.Body.String())
	}
}

func TestPreflightRejectsUnknownActionAndBadParams(t *testing.T) {
	agent := &stubAgent{err: &agentclient.Error{
		Status: http.StatusBadRequest, Code: "VALIDATION_ERROR", Message: "未登记的动作 \"storage.volume.mount\"",
	}}
	f := newPreflightFixture(t, agent)

	rec := f.do(t, http.MethodPost, "/api/v1/operations/preflight", preflightBody("storage.volume.mount", `{}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400；响应 %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "VALIDATION_ERROR") {
		t.Fatalf("错误码 = %s", rec.Body.String())
	}

	// 未登记动作的拒绝也要留痕（outcome=failure）。
	var outcome string
	if err := f.db.SQL().QueryRowContext(context.Background(),
		`SELECT outcome FROM audit_events ORDER BY id DESC LIMIT 1`).Scan(&outcome); err != nil {
		t.Fatalf("读取审计失败: %v", err)
	}
	if outcome != "failure" {
		t.Fatalf("审计 outcome = %q，期望 failure", outcome)
	}
}

func TestPreflightValidatesRequestShape(t *testing.T) {
	agent := &stubAgent{plan: stubPlan()}
	f := newPreflightFixture(t, agent)

	cases := []struct {
		name        string
		contentType string
		body        string
		wantStatus  int
		wantCode    string
	}{
		{"非 JSON Content-Type", "text/plain", preflightBody("acceptance.probe", `{}`),
			http.StatusBadRequest, "VALIDATION_ERROR"},
		{"非法 JSON", "application/json", `{`, http.StatusBadRequest, "VALIDATION_ERROR"},
		{"参数不是对象", "application/json", preflightBody("acceptance.probe", `[]`),
			http.StatusBadRequest, "VALIDATION_ERROR"},
		{"动作名不合法", "application/json", preflightBody("FORMAT /dev/sdb", `{}`),
			http.StatusBadRequest, "VALIDATION_ERROR"},
		// 前端不能自己塞结论：请求体只允许 action 与 params。
		{"未知字段", "application/json",
			`{"action":"acceptance.probe","params":{},"allowed":true}`, http.StatusBadRequest, "VALIDATION_ERROR"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/v1/operations/preflight", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", tc.contentType)
			req.Header.Set("Origin", testOrigins()[0])
			req.Header.Set("Cookie", f.cookie.String())
			req.Header.Set(CSRFTokenHeader, f.csrf)
			rec := httptest.NewRecorder()
			f.srv.Handler().ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Fatalf("状态码 = %d，期望 %d；响应 %s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.wantCode) {
				t.Fatalf("错误码 = %s，期望 %s", rec.Body.String(), tc.wantCode)
			}
			if !strings.Contains(rec.Body.String(), tc.wantCode) {
				t.Fatalf("错误码不符: %s", rec.Body.String())
			}
		})
	}

	// 形状校验失败的请求不该打到代理。
	if len(agent.calls) != 0 {
		t.Fatalf("形状不合法的请求不应转发给代理，实际 %d 次", len(agent.calls))
	}
}

func TestPreflightRejectsWrongMethod(t *testing.T) {
	agent := &stubAgent{plan: stubPlan()}
	f := newPreflightFixture(t, agent)

	rec := f.do(t, http.MethodGet, "/api/v1/operations/preflight", "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("状态码 = %d，期望 405；响应 %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "VALIDATION_ERROR") {
		t.Fatalf("405 应带 VALIDATION_ERROR: %s", rec.Body.String())
	}
	if allow := rec.Header().Get("Allow"); allow != "POST" {
		t.Fatalf("Allow = %q，期望 POST", allow)
	}
}

// clientFromDB 读回预检行，确认接口层确实把绑定写进了库。
func TestPreflightPersistsBindingRow(t *testing.T) {
	agent := &stubAgent{plan: stubPlan()}
	agent.plan.ConfirmationPhrase = "PROBE ABCD23"
	f := newPreflightFixture(t, agent)

	rec := f.do(t, http.MethodPost, "/api/v1/operations/preflight",
		preflightBody("acceptance.probe", `{"label":"alpha"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d；响应 %s", rec.Code, rec.Body.String())
	}
	var pre preflightResult
	if err := json.Unmarshal(parseResult(t, rec).Data, &pre); err != nil {
		t.Fatalf("解析失败: %v", err)
	}

	var (
		userID                                                  int64
		sessionHash, action, paramsHash, stateVersion, agentRef string
		requiresReauth, requiresPhrase                          int
		expiresAt                                               string
	)
	err := f.db.SQL().QueryRowContext(context.Background(), `
		SELECT user_id, session_hash, action, params_hash, state_version, agent_ref,
		       requires_reauth, requires_phrase, expires_at
		  FROM preflights WHERE id = ?`, pre.PreflightID).
		Scan(&userID, &sessionHash, &action, &paramsHash, &stateVersion, &agentRef,
			&requiresReauth, &requiresPhrase, &expiresAt)
	if err != nil {
		t.Fatalf("读取预检行失败: %v", err)
	}
	if userID <= 0 || len(sessionHash) != 64 {
		t.Fatalf("绑定列不对: user=%d session=%q", userID, sessionHash)
	}
	if action != "acceptance.probe" || stateVersion != "1" || agentRef != agent.plan.Ref {
		t.Fatalf("动作/状态版本/代理引用 = %s / %s / %s", action, stateVersion, agentRef)
	}
	if requiresReauth != 1 || requiresPhrase != 1 {
		t.Fatalf("策略列 = reauth=%d phrase=%d", requiresReauth, requiresPhrase)
	}
	if len(paramsHash) != 64 {
		t.Fatalf("参数哈希 = %q", paramsHash)
	}
	if expiresAt != pre.ExpiresAt {
		t.Fatalf("库中到期时间 %s 与响应 %s 不一致", expiresAt, pre.ExpiresAt)
	}

	// 会话绑定：换一个会话（重新登录）后的预检不应与旧会话混同。
	var sessionCount int
	if err := f.db.SQL().QueryRowContext(context.Background(),
		`SELECT COUNT(DISTINCT session_hash) FROM preflights`).Scan(&sessionCount); err != nil {
		t.Fatalf("统计失败: %v", err)
	}
	if sessionCount != 1 {
		t.Fatalf("预检应只绑定一个会话，实际 %d", sessionCount)
	}

	// 库里不能有确认短语明文。
	var storedPhrase int
	if err := f.db.SQL().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM preflights
		  WHERE normalized_params LIKE '%PROBE ABCD23%' OR target_json LIKE '%PROBE ABCD23%'`).
		Scan(&storedPhrase); err != nil {
		t.Fatalf("统计失败: %v", err)
	}
	if storedPhrase != 0 {
		t.Fatal("预检行里出现了确认短语")
	}
}
