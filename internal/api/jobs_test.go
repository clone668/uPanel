package api

// P03.2 接口层用例：幂等键、重复提交、预检一次性、再认证、取消策略与权限。
//
// 这里不重复服务层已经证明的 SQL 细节，只验收 HTTP 契约：
// 状态码、错误码、响应字段、谁可以调用哪一条。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"upanel/internal/agentclient"
)

const jobTestPhrase = "PROBE K7M2QX"

// jobFixture 是一个已登录管理员 + 已完成的预检 + 假代理。
type jobFixture struct {
	*adminFixture
	agent       *stubAgent
	preflightID string
}

// newJobFixture 构造装置：先由代理给出计划，再走真实预检接口拿到 preflight_id，
// 最后完成一次密码再认证（提交任务要求 5 分钟内的再认证）。
func newJobFixture(t *testing.T, modify func(*agentclient.PreflightPlan)) *jobFixture {
	t.Helper()
	agent := &stubAgent{plan: stubPlan()}
	agent.plan.ConfirmationPhrase = jobTestPhrase
	if modify != nil {
		modify(&agent.plan)
	}
	agent.op = agentclient.Operation{
		OperationID: "op_0123456789abcdef0123456789abcdef",
		Action:      agent.plan.Action,
		Stage:       "accepted",
		Step:        "accepted",
		Cancellable: agent.plan.Cancellable,
	}
	base := newPreflightFixture(t, agent)

	rec := base.do(t, http.MethodPost, "/api/v1/operations/preflight",
		preflightBody("acceptance.probe", `{"label":"alpha"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("预检失败: 状态码 = %d，响应 %s", rec.Code, rec.Body.String())
	}
	var pre preflightResult
	if err := json.Unmarshal(parseResult(t, rec).Data, &pre); err != nil {
		t.Fatalf("解析预检结果失败: %v", err)
	}
	if rec := base.reauth(t, testUserPass); rec.Code != http.StatusOK {
		t.Fatalf("再认证失败: 状态码 = %d，响应 %s", rec.Code, rec.Body.String())
	}
	return &jobFixture{adminFixture: base, agent: agent, preflightID: pre.PreflightID}
}

// doWithHeaders 发送带额外请求头的请求（Idempotency-Key 走这里）。
func (f *jobFixture) doWithHeaders(t *testing.T, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Origin", testOrigin)
	req.Header.Set("Cookie", f.cookie.String())
	if method != http.MethodGet && method != http.MethodHead {
		req.Header.Set(CSRFTokenHeader, f.csrf)
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	return rec
}

// submit 用给定幂等键与短语提交任务。
func (f *jobFixture) submit(t *testing.T, key, phrase string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"preflight_id":"` + f.preflightID + `","confirmation_phrase":"` + phrase + `"}`
	return f.doWithHeaders(t, http.MethodPost, "/api/v1/jobs", body,
		map[string]string{"Idempotency-Key": key})
}

// jobSubmitResult 是按契约字段名解析的创建结果。
type jobSubmitResult struct {
	JobID     string `json:"job_id"`
	Status    string `json:"status"`
	EventsURL string `json:"events_url"`
}

// jobResult 是按契约字段名解析的任务视图。
type jobResult struct {
	JobID                 string          `json:"job_id"`
	Action                string          `json:"action"`
	Status                string          `json:"status"`
	Step                  string          `json:"step"`
	ProgressPercent       *int            `json:"progress_percent"`
	Result                json.RawMessage `json:"result"`
	Error                 json.RawMessage `json:"error"`
	Cancellable           bool            `json:"cancellable"`
	CancellationRequested bool            `json:"cancellation_requested"`
	CreatedAt             string          `json:"created_at"`
	UpdatedAt             string          `json:"updated_at"`
	StartedAt             *string         `json:"started_at"`
	FinishedAt            *string         `json:"finished_at"`
}

// mustSubmitOK 提交并断言 202，返回解析后的创建结果。
func (f *jobFixture) mustSubmitOK(t *testing.T, key, phrase string) jobSubmitResult {
	t.Helper()
	rec := f.submit(t, key, phrase)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("提交状态码 = %d，期望 202；响应 %s", rec.Code, rec.Body.String())
	}
	var out jobSubmitResult
	if err := json.Unmarshal(parseResult(t, rec).Data, &out); err != nil {
		t.Fatalf("解析创建结果失败: %v", err)
	}
	return out
}

// TestCreateJobRequiresIdempotencyKey 缺键直接拒绝，不产生任务。
func TestCreateJobRequiresIdempotencyKey(t *testing.T) {
	f := newJobFixture(t, nil)
	body := `{"preflight_id":"` + f.preflightID + `","confirmation_phrase":"` + jobTestPhrase + `"}`
	rec := f.do(t, http.MethodPost, "/api/v1/jobs", body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400；响应 %s", rec.Code, rec.Body.String())
	}
	if got := parseResult(t, rec).Error.Code; got != "VALIDATION_ERROR" {
		t.Fatalf("错误码 = %q，期望 VALIDATION_ERROR", got)
	}
	if got := countJobs(t, f); got != 0 {
		t.Fatalf("被拒的提交留下了 %d 条任务", got)
	}
}

// TestCreateJobRejectsUnknownFields 提交阶段不能替换目标。
func TestCreateJobRejectsUnknownFields(t *testing.T) {
	f := newJobFixture(t, nil)
	body := `{"preflight_id":"` + f.preflightID + `","action":"storage.filesystem.format"}`
	rec := f.doWithHeaders(t, http.MethodPost, "/api/v1/jobs", body,
		map[string]string{"Idempotency-Key": "k1"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400；响应 %s", rec.Code, rec.Body.String())
	}
	if got := parseResult(t, rec).Error.Code; got != "VALIDATION_ERROR" {
		t.Fatalf("错误码 = %q", got)
	}
}

// TestCreateJobAndReadBack 提交 → 202 → 读回 → 列表。
func TestCreateJobAndReadBack(t *testing.T) {
	f := newJobFixture(t, nil)
	created := f.mustSubmitOK(t, "key-1", jobTestPhrase)
	if !strings.HasPrefix(created.JobID, "job_") || len(created.JobID) != len("job_")+32 {
		t.Fatalf("job_id 形态不合法: %q", created.JobID)
	}
	if created.Status != "queued" {
		t.Fatalf("提交后状态 = %q，期望 queued", created.Status)
	}
	if created.EventsURL != "/api/v1/jobs/"+created.JobID+"/events" {
		t.Fatalf("events_url = %q", created.EventsURL)
	}

	rec := f.do(t, http.MethodGet, "/api/v1/jobs/"+created.JobID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("读取任务状态码 = %d，响应 %s", rec.Code, rec.Body.String())
	}
	var job jobResult
	if err := json.Unmarshal(parseResult(t, rec).Data, &job); err != nil {
		t.Fatalf("解析任务失败: %v", err)
	}
	if job.JobID != created.JobID || job.Action != "acceptance.probe" {
		t.Fatalf("任务视图 = %+v", job)
	}
	// 不知道进度就必须是 null，不能是 0 或一个编出来的数字。
	if job.ProgressPercent != nil {
		t.Fatalf("progress_percent = %d，P03.2 必须是 null", *job.ProgressPercent)
	}
	if job.Cancellable {
		t.Fatal("stub 计划是“不可取消”，任务视图却标成可取消")
	}
	if job.StartedAt != nil || job.FinishedAt != nil {
		t.Fatalf("尚未执行的任务不应该有开始/结束时间: %+v", job)
	}

	rec = f.do(t, http.MethodGet, "/api/v1/jobs", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("列表状态码 = %d，响应 %s", rec.Code, rec.Body.String())
	}
	var list struct {
		Jobs []jobResult `json:"jobs"`
	}
	if err := json.Unmarshal(parseResult(t, rec).Data, &list); err != nil {
		t.Fatalf("解析列表失败: %v", err)
	}
	if len(list.Jobs) != 1 || list.Jobs[0].JobID != created.JobID {
		t.Fatalf("列表 = %+v", list.Jobs)
	}
}

// TestCreateJobReplayReturnsSameJob 验收“同一次请求提交多次只产生一次执行”的 HTTP 侧。
func TestCreateJobReplayReturnsSameJob(t *testing.T) {
	f := newJobFixture(t, nil)
	first := f.mustSubmitOK(t, "key-1", jobTestPhrase)
	for i := 0; i < 9; i++ {
		again := f.mustSubmitOK(t, "key-1", jobTestPhrase)
		if again.JobID != first.JobID {
			t.Fatalf("第 %d 次重放产生了新任务 %q", i+2, again.JobID)
		}
	}
	if got := len(f.agent.opCalls); got != 1 {
		t.Fatalf("代理收到 %d 次提交，期望 1", got)
	}
	if got := countJobs(t, f); got != 1 {
		t.Fatalf("jobs 行数 = %d，期望 1", got)
	}
}

// TestCreateJobSameKeyDifferentBody 验收 409 IDEMPOTENCY_CONFLICT。
func TestCreateJobSameKeyDifferentBody(t *testing.T) {
	f := newJobFixture(t, nil)
	f.mustSubmitOK(t, "key-1", jobTestPhrase)

	rec := f.submit(t, "key-1", "PROBE OTHER")
	if rec.Code != http.StatusConflict {
		t.Fatalf("状态码 = %d，期望 409；响应 %s", rec.Code, rec.Body.String())
	}
	if got := parseResult(t, rec).Error.Code; got != "IDEMPOTENCY_CONFLICT" {
		t.Fatalf("错误码 = %q，期望 IDEMPOTENCY_CONFLICT", got)
	}
}

// TestCreateJobConsumesPreflight 换一个幂等键也不能复用同一份预检。
func TestCreateJobConsumesPreflight(t *testing.T) {
	f := newJobFixture(t, nil)
	f.mustSubmitOK(t, "key-1", jobTestPhrase)

	rec := f.submit(t, "key-2", jobTestPhrase)
	if rec.Code != http.StatusConflict {
		t.Fatalf("状态码 = %d，期望 409；响应 %s", rec.Code, rec.Body.String())
	}
	if got := parseResult(t, rec).Error.Code; got != "PREFLIGHT_STALE" {
		t.Fatalf("错误码 = %q，期望 PREFLIGHT_STALE", got)
	}
}

// TestCreateJobRejectsExpiredPreflight 预检过期后提交被拒。
func TestCreateJobRejectsExpiredPreflight(t *testing.T) {
	f := newJobFixture(t, nil)
	// 直接把到期时间改到过去：等价于“管理员在预检页停留超过 120 秒”。
	past := time.Now().UTC().Add(-time.Second).Format(time.RFC3339)
	if _, err := f.db.ExecContext(context.Background(),
		`UPDATE preflights SET expires_at = ? WHERE id = ?`, past, f.preflightID); err != nil {
		t.Fatalf("修改预检到期时间失败: %v", err)
	}
	rec := f.submit(t, "key-1", jobTestPhrase)
	if rec.Code != http.StatusConflict {
		t.Fatalf("状态码 = %d，期望 409；响应 %s", rec.Code, rec.Body.String())
	}
	if got := parseResult(t, rec).Error.Code; got != "PREFLIGHT_EXPIRED" {
		t.Fatalf("错误码 = %q，期望 PREFLIGHT_EXPIRED", got)
	}
	if len(f.agent.opCalls) != 0 {
		t.Fatalf("过期预检仍然提交给了代理 %d 次", len(f.agent.opCalls))
	}
}

// TestCreateJobRequiresReauth 没有 5 分钟内的再认证时拒绝提交。
func TestCreateJobRequiresReauth(t *testing.T) {
	agent := &stubAgent{plan: stubPlan()}
	agent.plan.ConfirmationPhrase = jobTestPhrase
	base := newPreflightFixture(t, agent)

	rec := base.do(t, http.MethodPost, "/api/v1/operations/preflight",
		preflightBody("acceptance.probe", `{"label":"alpha"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("预检失败: %s", rec.Body.String())
	}
	var pre preflightResult
	if err := json.Unmarshal(parseResult(t, rec).Data, &pre); err != nil {
		t.Fatalf("解析预检失败: %v", err)
	}
	// 刻意不做 reauth。请求头仍然齐全（幂等键 + CSRF），只为单独证明再认证这一道门。
	body := `{"preflight_id":"` + pre.PreflightID + `","confirmation_phrase":"` + jobTestPhrase + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/jobs", strings.NewReader(body))
	req.Header.Set("Origin", testOrigin)
	req.Header.Set("Cookie", base.cookie.String())
	req.Header.Set(CSRFTokenHeader, base.csrf)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "key-1")
	rec = httptest.NewRecorder()
	base.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("状态码 = %d，期望 403；响应 %s", rec.Code, rec.Body.String())
	}
	if got := parseResult(t, rec).Error.Code; got != "REAUTH_REQUIRED" {
		t.Fatalf("错误码 = %q，期望 REAUTH_REQUIRED", got)
	}
	if len(agent.opCalls) != 0 {
		t.Fatalf("未再认证仍然提交给了代理 %d 次", len(agent.opCalls))
	}
}

// TestCreateJobReportsAgentRejection 代理拒绝（例如目标状态变化）映射成契约错误码。
func TestCreateJobReportsAgentRejection(t *testing.T) {
	f := newJobFixture(t, nil)
	f.agent.opErr = &agentclient.Error{Status: 409, Code: "PREFLIGHT_STALE", Message: "目标状态已变化"}

	rec := f.submit(t, "key-1", jobTestPhrase)
	if rec.Code != http.StatusConflict {
		t.Fatalf("状态码 = %d，期望 409；响应 %s", rec.Code, rec.Body.String())
	}
	if got := parseResult(t, rec).Error.Code; got != "PREFLIGHT_STALE" {
		t.Fatalf("错误码 = %q，期望 PREFLIGHT_STALE", got)
	}
	// 拒绝也要留痕：任务存在且是 failed，错误码写在任务里。
	var status, errJSON string
	if err := f.db.SQL().QueryRowContext(context.Background(),
		`SELECT status, error_json FROM jobs ORDER BY created_at DESC LIMIT 1`).
		Scan(&status, &errJSON); err != nil {
		t.Fatalf("读取任务失败: %v", err)
	}
	if status != "failed" || !strings.Contains(errJSON, "PREFLIGHT_STALE") {
		t.Fatalf("任务落库状态 = %q，error_json = %s", status, errJSON)
	}
}

// TestCreateJobAgentUnavailableKeepsQueued 代理不可用时任务保持 queued，随后可对账。
func TestCreateJobAgentUnavailableKeepsQueued(t *testing.T) {
	f := newJobFixture(t, nil)
	f.agent.opErr = &agentclient.UnavailableError{Err: context.DeadlineExceeded}

	created := f.mustSubmitOK(t, "key-1", jobTestPhrase)
	if created.Status != "queued" {
		t.Fatalf("提交后状态 = %q，期望 queued（不知道代理是否接受）", created.Status)
	}

	// 代理其实接受了：网络只是回包丢了。对账要按原 operation_id 找到它。
	f.agent.opErr = nil
	f.agent.getOp = agentclient.Operation{
		OperationID: "op_0123456789abcdef0123456789abcdef",
		Action:      "acceptance.probe",
		Stage:       "succeeded",
		Step:        "done",
		Result:      json.RawMessage(`{"outcome":"verified","verified_at":"2026-10-20T09:00:05Z","observed_state":{}}`),
		StartedAt:   "2026-10-20T09:00:01Z",
		FinishedAt:  "2026-10-20T09:00:05Z",
	}
	rec := f.do(t, http.MethodGet, "/api/v1/jobs/"+created.JobID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("读取任务失败: %s", rec.Body.String())
	}
	var job jobResult
	if err := json.Unmarshal(parseResult(t, rec).Data, &job); err != nil {
		t.Fatalf("解析任务失败: %v", err)
	}
	if job.Status != "succeeded" {
		t.Fatalf("对账后状态 = %q，期望 succeeded", job.Status)
	}
	if len(f.agent.opCalls) != 1 {
		t.Fatalf("代理收到 %d 次提交，期望 1（不能重新提交）", len(f.agent.opCalls))
	}
	// 代理给出 verified_at，前端才能说“已核验”，而不是“应该成了”。
	if !strings.Contains(string(job.Result), "verified_at") {
		t.Fatalf("结果里缺少 verified_at: %s", job.Result)
	}
}

// TestCancelJobNotCancelable 不可取消动作：409，且任务视图不给取消入口。
func TestCancelJobNotCancelable(t *testing.T) {
	f := newJobFixture(t, func(p *agentclient.PreflightPlan) { p.Cancellable = false })
	created := f.mustSubmitOK(t, "key-1", jobTestPhrase)

	rec := f.do(t, http.MethodPost, "/api/v1/jobs/"+created.JobID+"/cancel", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("状态码 = %d，期望 409；响应 %s", rec.Code, rec.Body.String())
	}
	if got := parseResult(t, rec).Error.Code; got != "VALIDATION_ERROR" {
		t.Fatalf("错误码 = %q", got)
	}
	if len(f.agent.cancelCalls) != 0 {
		t.Fatalf("不可取消的动作仍然问了代理 %d 次", len(f.agent.cancelCalls))
	}
}

// TestCancelJobCancellable 可取消动作：202 + 状态回写。
func TestCancelJobCancellable(t *testing.T) {
	f := newJobFixture(t, func(p *agentclient.PreflightPlan) { p.Cancellable = true })
	created := f.mustSubmitOK(t, "key-1", jobTestPhrase)

	f.agent.cancelOp = agentclient.Operation{
		OperationID:           "op_0123456789abcdef0123456789abcdef",
		Action:                "acceptance.probe",
		Stage:                 "canceled",
		Step:                  "canceled",
		Cancellable:           true,
		CancellationRequested: true,
		FinishedAt:            "2026-10-20T09:00:02Z",
	}
	rec := f.do(t, http.MethodPost, "/api/v1/jobs/"+created.JobID+"/cancel", "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("状态码 = %d，期望 202；响应 %s", rec.Code, rec.Body.String())
	}
	var job jobResult
	if err := json.Unmarshal(parseResult(t, rec).Data, &job); err != nil {
		t.Fatalf("解析取消结果失败: %v", err)
	}
	if job.Status != "canceled" || !job.CancellationRequested {
		t.Fatalf("取消后视图 = %+v", job)
	}
	if len(f.agent.cancelCalls) != 1 {
		t.Fatalf("代理收到 %d 次取消，期望 1", len(f.agent.cancelCalls))
	}
}

// TestJobPermissions viewer 能看自己的任务，但不能提交也不能取消。
func TestJobPermissions(t *testing.T) {
	f := newJobFixture(t, func(p *agentclient.PreflightPlan) { p.Cancellable = true })
	created := f.mustSubmitOK(t, "key-1", jobTestPhrase)

	f.createUser(t, "watcher", "viewer", "viewer-pass-9")
	cookie, csrf := loginAsUser(t, f.srv, "watcher", "viewer-pass-9")

	// viewer 读列表与单个任务：允许（CONTRACTS.md 4.3 权限列是 V），但只看到自己的。
	rec := f.doAs(t, cookie, csrf, http.MethodGet, "/api/v1/jobs", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("viewer 读列表状态码 = %d，响应 %s", rec.Code, rec.Body.String())
	}
	var list struct {
		Jobs []jobResult `json:"jobs"`
	}
	if err := json.Unmarshal(parseResult(t, rec).Data, &list); err != nil {
		t.Fatalf("解析列表失败: %v", err)
	}
	if len(list.Jobs) != 0 {
		t.Fatalf("viewer 看到了别人的任务: %+v", list.Jobs)
	}
	rec = f.doAs(t, cookie, csrf, http.MethodGet, "/api/v1/jobs/"+created.JobID, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("viewer 读他人任务状态码 = %d，期望 404；响应 %s", rec.Code, rec.Body.String())
	}

	// 提交与取消都要求 admin。
	body := `{"preflight_id":"` + f.preflightID + `","confirmation_phrase":"` + jobTestPhrase + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/jobs", strings.NewReader(body))
	req.Header.Set("Origin", testOrigin)
	req.Header.Set("Cookie", cookie.String())
	req.Header.Set(CSRFTokenHeader, csrf)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "viewer-key")
	rec = httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("viewer 提交状态码 = %d，期望 403；响应 %s", rec.Code, rec.Body.String())
	}
	if got := parseResult(t, rec).Error.Code; got != "FORBIDDEN" {
		t.Fatalf("错误码 = %q", got)
	}
	rec = f.doAs(t, cookie, csrf, http.MethodPost, "/api/v1/jobs/"+created.JobID+"/cancel", "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("viewer 取消状态码 = %d，期望 403；响应 %s", rec.Code, rec.Body.String())
	}
}

// TestGetJobNotFound 未知任务 404，而不是 500 或空对象。
func TestGetJobNotFound(t *testing.T) {
	f := newJobFixture(t, nil)
	rec := f.do(t, http.MethodGet, "/api/v1/jobs/job_"+strings.Repeat("0", 32), "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("状态码 = %d，期望 404；响应 %s", rec.Code, rec.Body.String())
	}
	if got := parseResult(t, rec).Error.Code; got != "NOT_FOUND" {
		t.Fatalf("错误码 = %q", got)
	}
}

// countJobs 统计任务行数（用真实的库，不用内存计数器）。
func countJobs(t *testing.T, f *jobFixture) int {
	t.Helper()
	var n int
	if err := f.db.SQL().QueryRowContext(context.Background(), `SELECT COUNT(*) FROM jobs`).Scan(&n); err != nil {
		t.Fatalf("统计任务失败: %v", err)
	}
	return n
}
