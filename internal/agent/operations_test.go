package agent

// P03.2 的 HTTP 层测试：提交、重放、冲突、取消、启动对账。
//
// 这些测试走真实的处理器与真实 SQLite（只是没有 Unix socket），因此
// “一次预检只能换一个操作”“十次提交只执行一次”这类结论是数据上的结论，
// 不是对某个桩行为的断言。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"upanel/internal/agentstore"
	"upanel/internal/version"
)

// countingAction 统计执行次数，用来证明“十次提交只执行一次”。
type countingAction struct {
	calls atomic.Int32
	// planCalls 记录预检器被调用的次数（预检一次 + 提交复核一次 + 执行复核一次）。
	planCalls atomic.Int32
	// stateVersions 按调用顺序返回状态版本；用完后返回最后一项。
	stateVersions []string
	// fingerprints 同上，用于身份变化。
	fingerprints []string
	// failWith 非空时执行器直接返回这个错误。
	failWith error
	// cancellable 决定策略。
	cancellable bool
	// execDuration 是执行耗时。
	execDuration time.Duration
	// planBudget 非零时登记到动作上（见 Action.PlanBudget）。
	planBudget time.Duration
	// slowPlanFrom 让第 N 次及以后的 Plan 调用一直等到 ctx 结束：模拟"预检快、
	// 提交复核慢"的软件包动作（24.04 上 apt 模拟两次都慢，但测试不需要真的慢）。
	slowPlanFrom int32
}

func (c *countingAction) registry(t *testing.T) *Registry {
	t.Helper()
	r := NewRegistry()
	nth := func(list []string, n int32) string {
		if len(list) == 0 {
			return "1"
		}
		i := int(n) - 1
		if i < 0 {
			i = 0
		}
		if i >= len(list) {
			i = len(list) - 1
		}
		return list[i]
	}
	action := Action{
		Name: "acceptance.count",
		Decode: func(raw json.RawMessage) (any, error) {
			var p pingParams
			if err := DecodeParams(raw, &p); err != nil {
				return nil, err
			}
			return p, nil
		},
		Plan: func(ctx context.Context, in PlanInput) (Plan, error) {
			n := c.planCalls.Add(1)
			if c.slowPlanFrom > 0 && n >= c.slowPlanFrom {
				<-ctx.Done()
				return Plan{}, ctx.Err()
			}
			return Plan{
				NormalizedParams: in.Params,
				Target: Target{
					DisplayName:         "计数对象",
					IdentityFingerprint: nth(c.fingerprints, n),
					StateVersion:        nth(c.stateVersions, n),
				},
				Allowed:  true,
				Warnings: []Notice{{Code: "acceptance_only", Message: "验收动作"}},
				Impacts:  []Impact{{Kind: "none", Description: "不修改任何状态"}},
				Locks:    []string{"acceptance:count"},
			}, nil
		},
		Execute: func(ctx context.Context, in ExecuteInput) (Result, error) {
			c.calls.Add(1)
			if c.execDuration > 0 {
				select {
				case <-time.After(c.execDuration):
				case <-ctx.Done():
					return Result{}, ErrCanceled
				}
			}
			if c.failWith != nil {
				return Result{}, c.failWith
			}
			return Result{
				Outcome:       OutcomeVerified,
				VerifiedAt:    time.Now().UTC(),
				ObservedState: map[string]any{"counter": c.calls.Load()},
			}, nil
		},
		PlanBudget: c.planBudget,
		Policy:     ActionPolicy{Cancellable: c.cancellable},
	}
	if err := r.RegisterAction(action); err != nil {
		t.Fatalf("登记计数动作失败: %v", err)
	}
	return r
}

// operationServer 构造一个带真实库的代理服务，并返回操作存储以便直接查库。
func newOperationServer(t *testing.T, reg *Registry) (*Server, *agentstore.Operations, *agentstore.PreflightPlans) {
	t.Helper()
	if reg == nil {
		reg = NewAcceptanceRegistry()
	}
	db := newTestAgentDB(t)
	ops := newTestOperationStoreOn(t, db)
	plans := newTestPlanStoreOn(t, db)
	srv, err := New(Options{
		SocketPath:      socketPathForTest(t),
		Version:         version.Info{Version: "test", Commit: "test", BuildDate: "test", GoVersion: "test"},
		Logger:          quietLogger(),
		ShutdownTimeout: time.Second,
		Peer:            testPeer(),
		Registry:        reg,
		Secrets:         newTestSecretStore(t),
		Plans:           plans,
		Operations:      ops,
	})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	return srv, ops, plans
}

// preflight creates a preflight and returns the decoded plan.
func mustPreflight(t *testing.T, srv *Server, action, params string) preflightResponse {
	t.Helper()
	rec := postPreflight(t, srv, testWebUID, preflightBody(action, params))
	if rec.Code != http.StatusCreated {
		t.Fatalf("预检失败: %d %s", rec.Code, rec.Body.String())
	}
	return decodePreflight(t, rec)
}

// submitOperation 发送一次 POST /internal/v1/operations。
func submitOperation(t *testing.T, srv *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/internal/v1/operations", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-ID", "rid-operation")
	return serveAs(srv, req, testWebUID)
}

// operationSubmitBody 拼一个提交请求体。
func operationSubmitBody(operationID, preflightRef, phrase, sessionHash, reauthAt string) string {
	body := `{"operation_id":"` + operationID + `","preflight_ref":"` + preflightRef + `"`
	if phrase != "" {
		body += `,"confirmation":"` + phrase + `"`
	}
	body += `,"actor_context":{"user_id":7,"session_id_hash":"` + sessionHash + `","request_id":"rid-submit"`
	if reauthAt != "" {
		body += `,"reauth_at":"` + reauthAt + `"`
	}
	return body + `}}`
}

// recentReauth 返回一个落在 5 分钟窗口内的再认证时间。
func recentReauth() string { return time.Now().UTC().Add(-time.Minute).Format(time.RFC3339) }

func decodeOperationResponse(t *testing.T, rec *httptest.ResponseRecorder) operationResponse {
	t.Helper()
	var body struct {
		Data      operationResponse `json:"data"`
		RequestID string            `json:"request_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v；响应 %s", err, rec.Body.String())
	}
	return body.Data
}

// getOperation 发送一次 GET /internal/v1/operations/<id>。
func getOperation(t *testing.T, srv *Server, id string) (operationResponse, int) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/internal/v1/operations/"+id, nil)
	rec := serveAs(srv, req, testWebUID)
	if rec.Code != http.StatusOK {
		return operationResponse{}, rec.Code
	}
	return decodeOperationResponse(t, rec), rec.Code
}

// waitForStage 轮询直到操作到达任一期望阶段。
func waitForStage(t *testing.T, srv *Server, id string, want ...string) operationResponse {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var last operationResponse
	for time.Now().Before(deadline) {
		op, code := getOperation(t, srv, id)
		if code == http.StatusOK {
			last = op
			for _, w := range want {
				if op.Stage == w {
					return op
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("操作 %s 在超时内没有到达 %v，最后状态 %+v", id, want, last)
	return last
}

func opID(seed string) string {
	var buf [16]byte
	copy(buf[:], seed)
	return agentstore.OperationIDPrefix + fmt.Sprintf("%x", buf[:])
}

func TestOperationSubmitRunsOnceAndRecordsResult(t *testing.T) {
	counter := &countingAction{}
	srv, _, _ := newOperationServer(t, counter.registry(t))
	plan := mustPreflight(t, srv, "acceptance.count", `{"note":"hello"}`)

	rec := submitOperation(t, srv, operationSubmitBody(opID("run-once"), plan.PreflightRef, "", testSessionHash, ""))
	if rec.Code != http.StatusCreated {
		t.Fatalf("状态码 = %d，期望 201；响应 %s", rec.Code, rec.Body.String())
	}
	accepted := decodeOperationResponse(t, rec)
	if accepted.Stage != agentstore.StageAccepted {
		t.Fatalf("接受后的阶段 = %q", accepted.Stage)
	}
	if accepted.Duplicate {
		t.Fatal("首次提交不应标记为重放")
	}
	if accepted.ProgressPercent != nil {
		t.Fatal("P03.2 不报百分比，progress_percent 必须是 null")
	}

	done := waitForStage(t, srv, accepted.OperationID, agentstore.StageSucceeded)
	if done.Step != "done" {
		t.Fatalf("step = %q", done.Step)
	}
	var result struct {
		Outcome       string         `json:"outcome"`
		VerifiedAt    string         `json:"verified_at"`
		ObservedState map[string]any `json:"observed_state"`
	}
	if err := json.Unmarshal(done.Result, &result); err != nil {
		t.Fatalf("结果不是合法 JSON: %v", err)
	}
	if result.Outcome != OutcomeVerified || result.VerifiedAt == "" {
		t.Fatalf("结果 = %+v", result)
	}
	if counter.calls.Load() != 1 {
		t.Fatalf("执行次数 = %d，期望 1", counter.calls.Load())
	}

	// 事件流水必须是 accepted → started → progress → finished。
	req := httptest.NewRequest(http.MethodGet, "/internal/v1/operations/"+accepted.OperationID+"/events", nil)
	evRec := serveAs(srv, req, testWebUID)
	if evRec.Code != http.StatusOK {
		t.Fatalf("读取事件状态码 = %d", evRec.Code)
	}
	var events struct {
		Data operationEventsResponse `json:"data"`
	}
	if err := json.Unmarshal(evRec.Body.Bytes(), &events); err != nil {
		t.Fatalf("事件响应不是合法 JSON: %v", err)
	}
	if len(events.Data.Events) < 3 {
		t.Fatalf("事件太少: %+v", events.Data.Events)
	}
	if events.Data.Events[0].Kind != "accepted" || events.Data.Events[1].Kind != "started" {
		t.Fatalf("事件顺序不对: %+v", events.Data.Events)
	}
}

func TestOperationTenSubmissionsExecuteOnce(t *testing.T) {
	counter := &countingAction{}
	srv, _, _ := newOperationServer(t, counter.registry(t))
	plan := mustPreflight(t, srv, "acceptance.count", `{"note":"idempotent"}`)

	id := opID("ten-times")
	body := operationSubmitBody(id, plan.PreflightRef, "", testSessionHash, "")
	created := 0
	duplicates := 0
	for i := 0; i < 10; i++ {
		rec := submitOperation(t, srv, body)
		switch rec.Code {
		case http.StatusCreated:
			created++
		case http.StatusOK:
			if !decodeOperationResponse(t, rec).Duplicate {
				t.Fatalf("第 %d 次提交返回 200 但没有标记为重放", i+1)
			}
			duplicates++
		default:
			t.Fatalf("第 %d 次提交状态码 = %d；响应 %s", i+1, rec.Code, rec.Body.String())
		}
	}
	if created != 1 || duplicates != 9 {
		t.Fatalf("created=%d duplicates=%d，期望 1 与 9", created, duplicates)
	}
	waitForStage(t, srv, id, agentstore.StageSucceeded)
	if counter.calls.Load() != 1 {
		t.Fatalf("执行次数 = %d，期望 1（十次提交只能产生一次系统执行）", counter.calls.Load())
	}
}

func TestOperationSameIDDifferentRequestIsConflict(t *testing.T) {
	counter := &countingAction{}
	srv, _, _ := newOperationServer(t, counter.registry(t))
	first := mustPreflight(t, srv, "acceptance.count", `{"note":"a"}`)
	second := mustPreflight(t, srv, "acceptance.count", `{"note":"b"}`)

	id := opID("conflict")
	if rec := submitOperation(t, srv, operationSubmitBody(id, first.PreflightRef, "", testSessionHash, "")); rec.Code != http.StatusCreated {
		t.Fatalf("首次提交状态码 = %d", rec.Code)
	}
	rec := submitOperation(t, srv, operationSubmitBody(id, second.PreflightRef, "", testSessionHash, ""))
	if rec.Code != http.StatusConflict {
		t.Fatalf("状态码 = %d，期望 409；响应 %s", rec.Code, rec.Body.String())
	}
	if code := errorCodeOf(t, rec); code != "IDEMPOTENCY_CONFLICT" {
		t.Fatalf("错误码 = %q，期望 IDEMPOTENCY_CONFLICT", code)
	}
	waitForStage(t, srv, id, agentstore.StageSucceeded)
	if counter.calls.Load() != 1 {
		t.Fatalf("执行次数 = %d，期望 1", counter.calls.Load())
	}
}

func TestOperationWrongPhraseRejected(t *testing.T) {
	srv, _, _ := newOperationServer(t, nil)
	plan := mustPreflight(t, srv, "acceptance.probe", `{"label":"phrase"}`)

	rec := submitOperation(t, srv, operationSubmitBody(opID("phrase"), plan.PreflightRef, "PROBE WRONG1", testSessionHash, recentReauth()))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400；响应 %s", rec.Code, rec.Body.String())
	}
	if code := errorCodeOf(t, rec); code != "CONFIRMATION_MISMATCH" {
		t.Fatalf("错误码 = %q，期望 CONFIRMATION_MISMATCH", code)
	}
	// 短语错了不能消费预检：用正确的短语还能提交成功。
	rec = submitOperation(t, srv, operationSubmitBody(opID("phrase-ok"), plan.PreflightRef, plan.ConfirmationPhrase, testSessionHash, recentReauth()))
	if rec.Code != http.StatusCreated {
		t.Fatalf("正确短语的状态码 = %d；响应 %s", rec.Code, rec.Body.String())
	}
}

func TestOperationRequiresReauth(t *testing.T) {
	srv, _, _ := newOperationServer(t, nil)
	plan := mustPreflight(t, srv, "acceptance.probe", `{"label":"reauth"}`)

	cases := map[string]string{
		"missing": "",
		"stale":   time.Now().UTC().Add(-30 * time.Minute).Format(time.RFC3339),
		"future":  time.Now().UTC().Add(10 * time.Minute).Format(time.RFC3339),
	}
	for name, reauthAt := range cases {
		rec := submitOperation(t, srv, operationSubmitBody(opID("reauth-"+name), plan.PreflightRef, plan.ConfirmationPhrase, testSessionHash, reauthAt))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s: 状态码 = %d，期望 403；响应 %s", name, rec.Code, rec.Body.String())
		}
		if code := errorCodeOf(t, rec); code != "REAUTH_REQUIRED" {
			t.Fatalf("%s: 错误码 = %q", name, code)
		}
	}
	// 被拒绝的提交不消费预检。
	rec := submitOperation(t, srv, operationSubmitBody(opID("reauth-ok"), plan.PreflightRef, plan.ConfirmationPhrase, testSessionHash, recentReauth()))
	if rec.Code != http.StatusCreated {
		t.Fatalf("状态码 = %d，期望 201；响应 %s", rec.Code, rec.Body.String())
	}
	waitForStage(t, srv, opID("reauth-ok"), agentstore.StageSucceeded, agentstore.StageFailed)
}

func TestOperationSessionMismatchInvalidatesPreflight(t *testing.T) {
	srv, _, _ := newOperationServer(t, nil)
	plan := mustPreflight(t, srv, "acceptance.probe", `{"label":"session"}`)
	other := strings.Repeat("cd", 32)

	rec := submitOperation(t, srv, operationSubmitBody(opID("session"), plan.PreflightRef, plan.ConfirmationPhrase, other, recentReauth()))
	if rec.Code != http.StatusConflict {
		t.Fatalf("状态码 = %d，期望 409；响应 %s", rec.Code, rec.Body.String())
	}
	if code := errorCodeOf(t, rec); code != "PREFLIGHT_STALE" {
		t.Fatalf("错误码 = %q，期望 PREFLIGHT_STALE", code)
	}
}

func TestOperationForeignPreflightForbidden(t *testing.T) {
	srv, _, _ := newOperationServer(t, nil)
	plan := mustPreflight(t, srv, "acceptance.ping", `{"note":"owner"}`)
	// user_id 换成别人：这是越权，不是“老化”。
	body := `{"operation_id":"` + opID("foreign") + `","preflight_ref":"` + plan.PreflightRef +
		`","actor_context":{"user_id":99,"session_id_hash":"` + testSessionHash + `"}}`
	rec := submitOperation(t, srv, body)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("状态码 = %d，期望 403；响应 %s", rec.Code, rec.Body.String())
	}
	if code := errorCodeOf(t, rec); code != "FORBIDDEN" {
		t.Fatalf("错误码 = %q", code)
	}
}

func TestOperationExpiredPreflightRejected(t *testing.T) {
	srv, _, plans := newOperationServer(t, nil)
	store := &mutatingPlanStore{inner: plans, mutate: func(p *agentstore.PreflightPlan) {
		p.ExpiresAt = time.Now().UTC().Add(-time.Second)
	}}
	srv.opts.Plans = store
	plan := mustPreflight(t, srv, "acceptance.ping", `{"note":"expired"}`)

	rec := submitOperation(t, srv, operationSubmitBody(opID("expired"), plan.PreflightRef, "", testSessionHash, ""))
	if rec.Code != http.StatusConflict {
		t.Fatalf("状态码 = %d，期望 409；响应 %s", rec.Code, rec.Body.String())
	}
	if code := errorCodeOf(t, rec); code != "PREFLIGHT_EXPIRED" {
		t.Fatalf("错误码 = %q，期望 PREFLIGHT_EXPIRED", code)
	}
}

func TestOperationStaleStateVersionRejected(t *testing.T) {
	counter := &countingAction{stateVersions: []string{"1", "2"}}
	srv, _, _ := newOperationServer(t, counter.registry(t))
	plan := mustPreflight(t, srv, "acceptance.count", `{"note":"stale"}`)

	rec := submitOperation(t, srv, operationSubmitBody(opID("stale"), plan.PreflightRef, "", testSessionHash, ""))
	if rec.Code != http.StatusConflict {
		t.Fatalf("状态码 = %d，期望 409；响应 %s", rec.Code, rec.Body.String())
	}
	if code := errorCodeOf(t, rec); code != "PREFLIGHT_STALE" {
		t.Fatalf("错误码 = %q，期望 PREFLIGHT_STALE", code)
	}
	if counter.calls.Load() != 0 {
		t.Fatalf("执行次数 = %d，期望 0", counter.calls.Load())
	}
}

func TestOperationIdentityChangeRejected(t *testing.T) {
	counter := &countingAction{fingerprints: []string{"fp-a", "fp-b"}}
	srv, _, _ := newOperationServer(t, counter.registry(t))
	plan := mustPreflight(t, srv, "acceptance.count", `{"note":"identity"}`)

	rec := submitOperation(t, srv, operationSubmitBody(opID("identity"), plan.PreflightRef, "", testSessionHash, ""))
	if rec.Code != http.StatusConflict {
		t.Fatalf("状态码 = %d，期望 409；响应 %s", rec.Code, rec.Body.String())
	}
	if code := errorCodeOf(t, rec); code != "DEVICE_IDENTITY_CHANGED" {
		t.Fatalf("错误码 = %q，期望 DEVICE_IDENTITY_CHANGED", code)
	}
	if counter.calls.Load() != 0 {
		t.Fatalf("执行次数 = %d，期望 0", counter.calls.Load())
	}
}

func TestOperationCancelNonCancellableRefused(t *testing.T) {
	srv, _, _ := newOperationServer(t, nil)
	plan := mustPreflight(t, srv, "acceptance.probe", `{"label":"no-cancel"}`)
	id := opID("no-cancel")
	if rec := submitOperation(t, srv, operationSubmitBody(id, plan.PreflightRef, plan.ConfirmationPhrase, testSessionHash, recentReauth())); rec.Code != http.StatusCreated {
		t.Fatalf("提交状态码 = %d；响应 %s", rec.Code, rec.Body.String())
	}

	req := httptest.NewRequest(http.MethodPost, "/internal/v1/operations/"+id+"/cancel", nil)
	rec := serveAs(srv, req, testWebUID)
	if rec.Code != http.StatusConflict {
		t.Fatalf("取消不可取消动作的状态码 = %d，期望 409；响应 %s", rec.Code, rec.Body.String())
	}
	if op, _ := getOperation(t, srv, id); op.Cancellable {
		t.Fatal("不可取消的动作不能在视图里显示为可取消")
	}
}

func TestOperationCancelCancellableAction(t *testing.T) {
	counter := &countingAction{cancellable: true, execDuration: 3 * time.Second}
	srv, _, _ := newOperationServer(t, counter.registry(t))
	plan := mustPreflight(t, srv, "acceptance.count", `{"note":"cancel"}`)
	id := opID("cancel-me")
	if rec := submitOperation(t, srv, operationSubmitBody(id, plan.PreflightRef, "", testSessionHash, "")); rec.Code != http.StatusCreated {
		t.Fatalf("提交状态码 = %d；响应 %s", rec.Code, rec.Body.String())
	}
	// 等它真的跑起来，再取消：这样测到的是 running + cancellation_requested 路径。
	waitForStage(t, srv, id, agentstore.StageRunning)

	req := httptest.NewRequest(http.MethodPost, "/internal/v1/operations/"+id+"/cancel", nil)
	rec := serveAs(srv, req, testWebUID)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("取消状态码 = %d，期望 202；响应 %s", rec.Code, rec.Body.String())
	}
	after := decodeOperationResponse(t, rec)
	if !after.CancellationRequested {
		t.Fatal("取消请求必须体现在 cancellation_requested 上")
	}

	final := waitForStage(t, srv, id, agentstore.StageCanceled, agentstore.StageInterrupted, agentstore.StageFailed)
	if final.Stage != agentstore.StageCanceled {
		t.Fatalf("最终阶段 = %q，期望 canceled（%s）", final.Stage, final.Error)
	}
	if counter.calls.Load() != 1 {
		t.Fatalf("执行次数 = %d，期望 1（取消不是重跑）", counter.calls.Load())
	}
}

func TestOperationExecutionFailureRecorded(t *testing.T) {
	counter := &countingAction{failWith: fmt.Errorf("磁盘炸了: %w", ErrExecuteFailed)}
	srv, _, _ := newOperationServer(t, counter.registry(t))
	plan := mustPreflight(t, srv, "acceptance.count", `{"note":"fail"}`)
	id := opID("boom")
	if rec := submitOperation(t, srv, operationSubmitBody(id, plan.PreflightRef, "", testSessionHash, "")); rec.Code != http.StatusCreated {
		t.Fatalf("提交状态码 = %d；响应 %s", rec.Code, rec.Body.String())
	}
	final := waitForStage(t, srv, id, agentstore.StageFailed)
	var errBody struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(final.Error, &errBody); err != nil {
		t.Fatalf("错误体不是合法 JSON: %v（%s）", err, final.Error)
	}
	if errBody.Code != "COMMAND_FAILED" || errBody.Message == "" {
		t.Fatalf("错误体 = %+v", errBody)
	}
	// 失败信息不得泄漏执行器原始错误的内部细节（命令原文/堆栈）。
	if strings.Contains(errBody.Message, "磁盘炸了") {
		t.Fatalf("错误信息泄漏了内部细节: %q", errBody.Message)
	}
}

// reasonedFailure 模拟"动作已经核实过原因"的失败（见 FailureReasoner）。
type reasonedFailure struct{ err error }

func (f reasonedFailure) Error() string { return f.err.Error() }
func (f reasonedFailure) Unwrap() error { return f.err }
func (f reasonedFailure) FailureReason() string {
	return "apt-get update 有错误行：Err:3 http://deb.example.invalid/repo stable InRelease"
}

// TestOperationFailureReasonReachesErrorMessage：核实过的原因要进 error.message。
//
// 失败任务的 result 按契约是空对象，所以 error.message 是管理员唯一能看到原因
// 的地方；上一轮的 P11.2 真机验收就卡在这里——apt 明明报了 `Err:` 行，面板只说
// "动作执行失败"。
func TestOperationFailureReasonReachesErrorMessage(t *testing.T) {
	counter := &countingAction{failWith: reasonedFailure{err: errors.New("apt: 模拟失败")}}
	srv, _, _ := newOperationServer(t, counter.registry(t))
	plan := mustPreflight(t, srv, "acceptance.count", `{"note":"reason"}`)
	id := opID("reason")
	if rec := submitOperation(t, srv, operationSubmitBody(id, plan.PreflightRef, "", testSessionHash, "")); rec.Code != http.StatusCreated {
		t.Fatalf("提交状态码 = %d；响应 %s", rec.Code, rec.Body.String())
	}
	final := waitForStage(t, srv, id, agentstore.StageFailed)
	var errBody struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(final.Error, &errBody); err != nil {
		t.Fatalf("错误体不是合法 JSON: %v（%s）", err, final.Error)
	}
	if errBody.Code != "COMMAND_FAILED" {
		t.Fatalf("错误码 = %q", errBody.Code)
	}
	if !strings.Contains(errBody.Message, "Err:3 http://deb.example.invalid/repo") {
		t.Fatalf("error.message 没带上核实过的原因: %q", errBody.Message)
	}
	// 原因里不该出现内部包裹的那一层原文：给管理员看的是 apt 那句话。
	if strings.Contains(errBody.Message, "模拟失败") {
		t.Fatalf("error.message 混进了内部错误原文: %q", errBody.Message)
	}
	if len(final.Result) != 0 && string(final.Result) != "{}" {
		t.Fatalf("失败任务的 result 必须是空对象，实际 %s", final.Result)
	}
}

// TestOperationRevalidateBudgetTurnsIntoResourceBusy：提交时的复核超预算要说人话。
//
// 提交这一步会再跑一次动作的 Plan（软件包动作=apt 模拟），服务器的 WriteTimeout 只有
// 15 秒。不给它一把自己的秒表，连接会先断，Web 看到的是 EOF，把一次正常的慢提交记成
// "代理不可用"——P11.2 验收第二轮在 24.04 上就是这么失败的（代理日志写着 201，
// Web 收到 EOF，任务挂在待对账）。这里钉两件事：预算是动作自己声明的那个数，
// 超时变成 409 RESOURCE_BUSY 而不是断连接。
func TestOperationRevalidateBudgetTurnsIntoResourceBusy(t *testing.T) {
	counter := &countingAction{
		planBudget:   80 * time.Millisecond,
		slowPlanFrom: 2, // 第 1 次（预检）快，第 2 次（提交复核）慢
	}
	srv, _, _ := newOperationServer(t, counter.registry(t))
	plan := mustPreflight(t, srv, "acceptance.count", `{"note":"slow-revalidate"}`)
	id := opID("slowreval")
	rec := submitOperation(t, srv, operationSubmitBody(id, plan.PreflightRef, "", testSessionHash, ""))
	if rec.Code != http.StatusConflict {
		t.Fatalf("提交状态码 = %d；响应 %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v（%s）", err, rec.Body.String())
	}
	if body.Error.Code != "RESOURCE_BUSY" {
		t.Fatalf("错误码 = %q；响应 %s", body.Error.Code, rec.Body.String())
	}
	if !strings.Contains(body.Error.Message, "执行前复核超过") {
		t.Fatalf("message 要说清是哪一步超时: %q", body.Error.Message)
	}
	// 复核失败时任务没有被接受：代理侧不该留下操作记录，预检也仍然读得到。
	if _, err := srv.opts.Operations.Get(context.Background(), "op_slowreval"); !errors.Is(err, agentstore.ErrOperationNotFound) {
		t.Fatalf("复核失败不该留下操作记录: %v", err)
	}
	if _, err := srv.opts.Plans.Get(context.Background(), plan.PreflightRef); err != nil {
		t.Fatalf("复核失败不该让预检消失: %v", err)
	}
}

func TestReconcileMarksUnfinishedOperationsInterrupted(t *testing.T) {
	counter := &countingAction{}
	srv, ops, _ := newOperationServer(t, counter.registry(t))
	plan := mustPreflight(t, srv, "acceptance.count", `{"note":"interrupted"}`)

	// 直接落一条 accepted 记录，模拟“接受之后进程就崩了”，不经过 runner。
	now := time.Now().UTC().Truncate(time.Second)
	record := agentstore.Operation{
		ID:                  opID("orphan"),
		RequestHash:         hashHex([]byte("orphan-request")),
		Action:              "acceptance.count",
		PreflightRef:        plan.PreflightRef,
		Params:              []byte(`{"note":"interrupted"}`),
		ParamsHash:          hashHex([]byte(`{"note":"interrupted"}`)),
		IdentityFingerprint: "1",
		StateVersion:        "1",
		UserID:              7,
		SessionHash:         testSessionHash,
		CreatedAt:           now,
		UpdatedAt:           now,
	}
	if _, _, err := ops.Accept(context.Background(), record); err != nil {
		t.Fatalf("落库失败: %v", err)
	}

	srv.reconcileInterrupted(context.Background())

	got, err := ops.Get(context.Background(), record.ID)
	if err != nil {
		t.Fatalf("读回失败: %v", err)
	}
	if got.Stage != agentstore.StageInterrupted {
		t.Fatalf("阶段 = %q，期望 interrupted", got.Stage)
	}
	var errBody struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(got.ErrorJSON, &errBody); err != nil || errBody.Code != "OPERATION_INTERRUPTED" {
		t.Fatalf("错误体 = %s", got.ErrorJSON)
	}
	if counter.calls.Load() != 0 {
		t.Fatalf("执行次数 = %d，期望 0（重启不得自动重试）", counter.calls.Load())
	}
}

// mutatingPlanStore 在读取计划时改写它，用来模拟“预检在提交前已经过期”。
type mutatingPlanStore struct {
	inner  PlanStore
	mutate func(*agentstore.PreflightPlan)
}

func (m *mutatingPlanStore) Insert(ctx context.Context, plan agentstore.PreflightPlan) error {
	return m.inner.Insert(ctx, plan)
}

func (m *mutatingPlanStore) Get(ctx context.Context, id string) (agentstore.PreflightPlan, error) {
	plan, err := m.inner.Get(ctx, id)
	if err != nil {
		return plan, err
	}
	if m.mutate != nil {
		m.mutate(&plan)
	}
	return plan, nil
}

func (m *mutatingPlanStore) DeleteExpired(ctx context.Context, now time.Time) (int, error) {
	return m.inner.DeleteExpired(ctx, now)
}

// errorCodeOf 从错误响应里取出 code。
func errorCodeOf(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("错误响应不是合法 JSON: %v；响应 %s", err, rec.Body.String())
	}
	return body.Error.Code
}
