package agent

// 定时触发路由（P12.4）：POST /internal/v1/schedules/:id/trigger。
//
// 这条路由是整个项目里唯一"没有人在键盘前"的写入入口，所以测试盯的都是**别让它松**：
// 形状不对的请求不进预检、被拦下的触发也要留痕、同一个时间槽不会跑第二遍、
// 要求交互确认的动作在这里一律拒绝。

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"upanel/internal/agentstore"
	"upanel/internal/schedules"
	"upanel/internal/version"
)

// testTriggerID 是测试里那个"这一次"的触发 ID。
//
// 它的值不重要，重要的是它在整个用例里保持不变：同一个时间槽无论触发几次，
// 算出来的 operation_id 都是同一个。
const testTriggerID = "trg_0123456789abcdef0123456789abcdef"

// testTriggerOperationID 是一个形状合法的操作 ID，用来验 root-only 路径的**形状**
// （"这条路径算不算专用路由"）。真实那一次的操作 ID 由 schedules.OperationID 从触发
// ID 推出来，值不一样，用例里从响应里取。
const testTriggerOperationID = "op_0123456789abcdef0123456789abcdef"

// claimCall 是一次触发记录的写入。
type claimCall struct {
	TriggerID string
	Outcome   string
	Reason    string
}

// fakeScheduleService 同时满足 scheduleSource 与 SchedulesService。
type fakeScheduleService struct {
	*fakeSchedules

	decision  schedules.TriggerDecision
	decideErr error
	decided   []string

	claims []claimCall
	// claimDuplicate 为 true 时模拟"这个槽此前被认领过"。
	claimDuplicate bool

	views    []schedules.View
	view     schedules.View
	viewOK   bool
	triggers []schedules.TriggerRecord
}

func (f *fakeScheduleService) Views(context.Context) ([]schedules.View, error) { return f.views, nil }

func (f *fakeScheduleService) View(_ context.Context, id string) (schedules.View, bool, error) {
	if !f.viewOK || f.view.Record.ID != id {
		return schedules.View{}, false, nil
	}
	return f.view, true, nil
}

func (f *fakeScheduleService) Triggers(_ context.Context, _ string, _ int) ([]schedules.TriggerRecord, error) {
	return f.triggers, nil
}

func (f *fakeScheduleService) DecideTrigger(_ context.Context, scheduleID, triggerID string) (schedules.TriggerDecision, error) {
	f.decided = append(f.decided, scheduleID+"|"+triggerID)
	if f.decideErr != nil {
		return schedules.TriggerDecision{}, f.decideErr
	}
	d := f.decision
	d.TriggerID = triggerID
	return d, nil
}

func (f *fakeScheduleService) ClaimTrigger(_ context.Context, d schedules.TriggerDecision, outcome, reason string) (string, bool, error) {
	f.claims = append(f.claims, claimCall{TriggerID: d.TriggerID, Outcome: outcome, Reason: reason})
	return d.OperationID, f.claimDuplicate, nil
}

// newFakeScheduleService 造一条"一切正常"的定时触发。
func newFakeScheduleService() *fakeScheduleService {
	return &fakeScheduleService{
		fakeSchedules: newFakeSchedules(),
		decision: schedules.TriggerDecision{
			Schedule: schedules.Record{
				ID:   testScheduleID,
				Name: "夜间备份",
				Kind: schedules.KindBackup,
			},
			OperationID: schedules.OperationID(testTriggerID),
			Slot:        time.Date(2026, 10, 24, 19, 30, 0, 0, time.UTC),
			Action: schedules.ActionRequest{
				Action: "test.timed",
				Params: json.RawMessage(`{"value":"夜间"}`),
			},
		},
	}
}

// triggerableRegistry 登记一个不需要交互确认、执行必定成功的动作。
//
// 不用验收动作表里的 acceptance.probe：它是**要求短语**的，正好用来验证
// "计划指向了需要交互确认的动作时必须拒绝"。
func triggerableRegistry(t *testing.T) *Registry {
	t.Helper()
	reg := NewRegistry()
	err := reg.RegisterAction(Action{
		Name: "test.timed",
		Decode: func(raw json.RawMessage) (any, error) {
			var p struct {
				Value string `json:"value"`
			}
			if err := DecodeParams(raw, &p); err != nil {
				return nil, err
			}
			if p.Value == "" {
				return nil, ErrParamsInvalid
			}
			return p, nil
		},
		Plan: func(_ context.Context, in PlanInput) (Plan, error) {
			return Plan{
				NormalizedParams: in.Params,
				Target: Target{
					DisplayName:         "定时目标",
					IdentityFingerprint: "sha256:timed",
					StateVersion:        "v1",
				},
				Allowed: true,
				Locks:   []string{"test:timed"},
			}, nil
		},
		Execute: func(_ context.Context, in ExecuteInput) (Result, error) {
			return Result{Outcome: OutcomeVerified, VerifiedAt: in.Now}, nil
		},
		Policy: ActionPolicy{RequiresReauth: true},
	})
	if err != nil {
		t.Fatalf("登记测试动作失败: %v", err)
	}
	return reg
}

// newTriggerServer 造一个挂了计划服务与操作存储的代理。
func newTriggerServer(t *testing.T, svc SchedulesService, reg *Registry) (*Server, *agentstore.Operations) {
	t.Helper()
	if reg == nil {
		reg = triggerableRegistry(t)
	}
	db := newTestAgentDB(t)
	ops := newTestOperationStoreOn(t, db)
	srv, err := New(Options{
		SocketPath:      socketPathForTest(t),
		Version:         version.Info{Version: "test", Commit: "test", BuildDate: "test", GoVersion: "test"},
		Logger:          quietLogger(),
		ShutdownTimeout: time.Second,
		Peer:            testPeer(),
		Registry:        reg,
		Secrets:         newTestSecretStore(t),
		Plans:           &recordingPlanStore{inner: newTestPlanStoreOn(t, db)},
		Operations:      ops,
		Schedules:       svc,
	})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	return srv, ops
}

func triggerBody(triggerID string) string {
	return `{"trigger_id":"` + triggerID + `"}`
}

func postTrigger(t *testing.T, srv *Server, uid uint32, scheduleID, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/internal/v1/schedules/"+scheduleID+"/trigger",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-ID", "rid-trigger")
	return serveAs(srv, req, uid)
}

// TestTriggerRootOnlyByPathShape：同一条前缀下，读路由给 Web、触发路由只给 root。
func TestTriggerRootOnlyByPathShape(t *testing.T) {
	cases := map[string]bool{
		"/internal/v1/schedules":                                false,
		"/internal/v1/schedules/" + testScheduleID:              false,
		"/internal/v1/schedules/" + testScheduleID + "/trigger": true,
		// --wait 的状态读：只给 root 的那一条，绑在计划 id 上。
		"/internal/v1/schedules/" + testScheduleID + "/trigger-runs/" + testTriggerOperationID: true,
		// 单独一条 /trigger-runs 或形状不对的都不算：判定按完整形状。
		"/internal/v1/schedules/" + testScheduleID + "/trigger-runs":                               false,
		"/internal/v1/schedules/" + testScheduleID + "/trigger-runs/op_x":                          false,
		"/internal/v1/schedules/x/trigger-runs/" + testTriggerOperationID:                          false,
		"/internal/v1/schedules/" + testScheduleID + "/trigger-runs/op_" + strings.Repeat("A", 32): false,
		"/internal/v1/scripts": false,
		// 形状不对的一律不算专用路由：判定按"计划 id 加 trigger"，不是按前缀，
		// 否则同前缀下的读路由会被一起划走。
		"/internal/v1/schedules/abc/trigger":      false,
		"/internal/v1/schedules/../trigger":       false,
		"/internal/v1/schedules/x/trigger/../../": false,
		"/internal/v1/scripts/" + testScriptID:    false,
	}
	for path, want := range cases {
		if got := isRootOnlyPath(path); got != want {
			t.Fatalf("isRootOnlyPath(%q) = %v，期望 %v", path, got, want)
		}
	}
}

func TestTriggerRejectsWebPeer(t *testing.T) {
	svc := newFakeScheduleService()
	srv, _ := newTriggerServer(t, svc, nil)
	rec := postTrigger(t, srv, testWebUID, testScheduleID, triggerBody(testTriggerID))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("状态码 = %d，期望 403（Web 账号不能触发）", rec.Code)
	}
	if len(svc.decided) != 0 {
		t.Fatalf("Web 账号的请求不该走到判决：%v", svc.decided)
	}
}

func TestTriggerRequiresPost(t *testing.T) {
	srv, _ := newTriggerServer(t, newFakeScheduleService(), nil)
	req := httptest.NewRequest(http.MethodGet, "/internal/v1/schedules/"+testScheduleID+"/trigger", nil)
	rec := serveAs(srv, req, 0)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("状态码 = %d", rec.Code)
	}
	if got := rec.Header().Get("Allow"); got != "POST" {
		t.Fatalf("Allow = %q", got)
	}
}

func TestTriggerDecodeStrict(t *testing.T) {
	svc := newFakeScheduleService()
	srv, _ := newTriggerServer(t, svc, nil)
	for _, body := range []string{
		`{}`,
		`{"trigger_id":""}`,
		`{"trigger_id":"trg_x","action":"format.disk"}`,
		`{"trigger_id":"trg_x","params":{"value":"x"}}`,
		`not json`,
	} {
		rec := postTrigger(t, srv, 0, testScheduleID, body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s 状态码 = %d，期望 400", body, rec.Code)
		}
	}
	if len(svc.decided) != 0 {
		t.Fatalf("被拒的请求不该走到判决：%v", svc.decided)
	}
}

// TestTriggerRejectsMismatchedSlot：调用方算错时间槽 → 400，不留痕也不执行。
func TestTriggerRejectsMismatchedSlot(t *testing.T) {
	svc := newFakeScheduleService()
	svc.decideErr = errors.New("触发 ID 与当前时间槽不符")
	srv, ops := newTriggerServer(t, svc, nil)
	rec := postTrigger(t, srv, 0, testScheduleID, triggerBody("trg_"+strings.Repeat("0", 32)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d", rec.Code)
	}
	if len(svc.claims) != 0 {
		t.Fatalf("槽不符时不该写触发记录：%+v", svc.claims)
	}
	if ops := ops; ops == nil {
		t.Fatal("操作存储不该为空")
	}
}

// TestTriggerBlockedIsRecorded：被拦下也要留痕——"那天为什么没跑"必须可查。
func TestTriggerBlockedIsRecorded(t *testing.T) {
	svc := newFakeScheduleService()
	svc.decision.Blockers = []schedules.Notice{{Code: "SCHEDULE_DISABLED", Message: "计划任务已停用"}}
	srv, _ := newTriggerServer(t, svc, nil)
	rec := postTrigger(t, srv, 0, testScheduleID, triggerBody(testTriggerID))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d", rec.Code)
	}
	if len(svc.claims) != 1 || svc.claims[0].Outcome != schedules.TriggerOutcomeBlocked {
		t.Fatalf("触发记录 = %+v", svc.claims)
	}
	if !strings.Contains(svc.claims[0].Reason, "SCHEDULE_DISABLED") {
		t.Fatalf("原因 = %q", svc.claims[0].Reason)
	}
	if !strings.Contains(rec.Body.String(), "已停用") {
		t.Fatalf("响应没说清原因: %s", rec.Body.String())
	}
}

// TestTriggerRejectsPhraseAction：计划指向需要交互确认的动作时不能定时。
func TestTriggerRejectsPhraseAction(t *testing.T) {
	svc := newFakeScheduleService()
	svc.decision.Action = schedules.ActionRequest{
		Action: "acceptance.probe",
		Params: json.RawMessage(`{"label":"夜间"}`),
	}
	srv, _ := newTriggerServer(t, svc, NewAcceptanceRegistry())
	rec := postTrigger(t, srv, 0, testScheduleID, triggerBody(testTriggerID))
	if rec.Code != http.StatusConflict {
		t.Fatalf("状态码 = %d，期望 409", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "交互确认") {
		t.Fatalf("响应 = %s", rec.Body.String())
	}
	if len(svc.claims) != 1 || svc.claims[0].Outcome != schedules.TriggerOutcomeBlocked {
		t.Fatalf("被拒的触发必须留痕：%+v", svc.claims)
	}
}

// TestTriggerRejectsUnknownAction：动作当前不可用 → 409，且不留成"跑过了"。
func TestTriggerRejectsUnknownAction(t *testing.T) {
	svc := newFakeScheduleService()
	svc.decision.Action = schedules.ActionRequest{
		Action: "backup.run",
		Params: json.RawMessage(`{"policy_id":"pol_x"}`),
	}
	srv, _ := newTriggerServer(t, svc, nil)
	rec := postTrigger(t, srv, 0, testScheduleID, triggerBody(testTriggerID))
	if rec.Code != http.StatusConflict {
		t.Fatalf("状态码 = %d", rec.Code)
	}
	if len(svc.claims) != 1 || svc.claims[0].Outcome != schedules.TriggerOutcomeBlocked {
		t.Fatalf("触发记录 = %+v", svc.claims)
	}
}

// TestTriggerAcceptedCreatesSystemOperation：受理后操作属于"系统"，不是任何用户。
func TestTriggerAcceptedCreatesSystemOperation(t *testing.T) {
	svc := newFakeScheduleService()
	srv, ops := newTriggerServer(t, svc, nil)
	rec := postTrigger(t, srv, 0, testScheduleID, triggerBody(testTriggerID))
	if rec.Code != http.StatusCreated {
		t.Fatalf("状态码 = %d（%s）", rec.Code, rec.Body.String())
	}

	var out triggerResponse
	decodeData(t, rec.Body.Bytes(), &out)
	if out.OperationID != svc.decision.OperationID || out.Outcome != schedules.TriggerOutcomeTriggered {
		t.Fatalf("响应 = %+v", out)
	}
	if out.Duplicate {
		t.Fatal("首次触发不该是重放")
	}
	if out.Action != "test.timed" || out.SlotAt != "2026-10-24T19:30:00Z" {
		t.Fatalf("响应 = %+v", out)
	}

	// 操作一定要落库，且操作者是"系统"（保留的 SchedulerUserID）：Web 对账导入时
	// 靠它区分"人点的"与"定时跑的"。
	op, err := ops.Get(context.Background(), svc.decision.OperationID)
	if err != nil {
		t.Fatalf("操作没有落库: %v", err)
	}
	if op.UserID != SchedulerUserID {
		t.Fatalf("user_id = %d，期望 %d", op.UserID, SchedulerUserID)
	}
	if op.SessionHash != hashHex([]byte("scheduler:"+testScheduleID)) {
		t.Fatalf("会话哈希 = %q", op.SessionHash)
	}
	if op.Action != "test.timed" || op.ID != schedules.OperationID(testTriggerID) {
		t.Fatalf("操作 = %+v", op)
	}
	// 触发记录最后一条必须是 triggered（前面可能有一条日志性记录）。
	if n := len(svc.claims); n == 0 || svc.claims[n-1].Outcome != schedules.TriggerOutcomeTriggered {
		t.Fatalf("触发记录 = %+v", svc.claims)
	}
}

// TestTriggerReplayReturnsExisting：同一个时间槽再进来一次，返回第一次的操作。
func TestTriggerReplayReturnsExisting(t *testing.T) {
	svc := newFakeScheduleService()
	srv, _ := newTriggerServer(t, svc, nil)
	first := postTrigger(t, srv, 0, testScheduleID, triggerBody(testTriggerID))
	if first.Code != http.StatusCreated {
		t.Fatalf("首次状态码 = %d", first.Code)
	}
	second := postTrigger(t, srv, 0, testScheduleID, triggerBody(testTriggerID))
	if second.Code != http.StatusOK {
		t.Fatalf("重放状态码 = %d，期望 200（不是错误，是同一件事）", second.Code)
	}
	var out triggerResponse
	decodeData(t, second.Body.Bytes(), &out)
	if !out.Duplicate {
		t.Fatal("重放必须标出来：界面要说\"这个槽已经跑过了\"")
	}
	if out.OperationID != svc.decision.OperationID {
		t.Fatalf("重放返回的 operation_id = %q，期望第一次那个", out.OperationID)
	}
}

// TestTriggerClaimedWithoutOperationIsNotAGate：认领过但没有对应操作（上次断在
// 落库之前）时，这一次照常执行——幂等的闸门是确定性的 operation_id，不是认领记录。
func TestTriggerClaimedWithoutOperationIsNotAGate(t *testing.T) {
	svc := newFakeScheduleService()
	svc.claimDuplicate = true
	srv, ops := newTriggerServer(t, svc, nil)
	rec := postTrigger(t, srv, 0, testScheduleID, triggerBody(testTriggerID))
	if rec.Code != http.StatusCreated {
		t.Fatalf("状态码 = %d，期望 201（不能因为认领过就拒绝执行）", rec.Code)
	}
	if _, err := ops.Get(context.Background(), svc.decision.OperationID); err != nil {
		t.Fatalf("操作没有落库: %v", err)
	}
}

// TestTriggerOperationConflict：同一个时间槽里计划的参数变了（管理员在这一分钟内
// 改过策略）→ 409，而不是悄悄按旧参数执行，也不是把两次意图合并成一个。
func TestTriggerOperationConflict(t *testing.T) {
	svc := newFakeScheduleService()
	srv, _ := newTriggerServer(t, svc, nil)
	if rec := postTrigger(t, srv, 0, testScheduleID, triggerBody(testTriggerID)); rec.Code != http.StatusCreated {
		t.Fatalf("首次状态码 = %d（%s）", rec.Code, rec.Body.String())
	}
	// 计划在同一个槽里指向了不同的参数：参数哈希变了，指纹也就变了。
	svc.decision.Action.Params = json.RawMessage(`{"value":"改过的"}`)
	rec := postTrigger(t, srv, 0, testScheduleID, triggerBody(testTriggerID))
	if rec.Code != http.StatusConflict {
		t.Fatalf("状态码 = %d，期望 409", rec.Code)
	}
	// 冲突也要留痕，否则库里只有一条"受理失败"却不知道是哪次触发。
	if n := len(svc.claims); n != 2 || svc.claims[n-1].Outcome != schedules.TriggerOutcomeFailed {
		t.Fatalf("触发记录 = %+v", svc.claims)
	}
	if !strings.Contains(rec.Body.String(), "不同的操作") {
		t.Fatalf("响应 = %s", rec.Body.String())
	}
}

// TestTriggerNotFoundWithoutService：没接线就是 404，不是 500 也不是空 200。
func TestTriggerNotFoundWithoutService(t *testing.T) {
	srv, _ := newTriggerServer(t, nil, nil)
	rec := postTrigger(t, srv, 0, testScheduleID, triggerBody(testTriggerID))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("状态码 = %d", rec.Code)
	}
}

// getTriggerRun 以某个 UID 读一次"这次触发的结果"。
func getTriggerRun(t *testing.T, srv *Server, uid uint32, scheduleID, opID string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet,
		"/internal/v1/schedules/"+scheduleID+"/trigger-runs/"+opID, nil)
	req.Header.Set("X-Request-ID", "rid-trigger-run")
	return serveAs(srv, req, uid)
}

// TestTriggerRunReadsOnlyOwnSchedule：--wait 的状态读**只**能看到这条计划自己触发的
// 那一次，别的一律当作不存在。
//
// 这条用例的存在理由不是"读得到"，而是"读不到别的"：这条路由是 root 唯一的读口，
// 一旦它按计划 id 放行任意操作，root CLI 就成了绕过 Web 账号边界的读通道。
func TestTriggerRunReadsOnlyOwnSchedule(t *testing.T) {
	svc := newFakeScheduleService()
	svc.decision.Schedule.Definition.MaxRuntimeSeconds = 90
	srv, _ := newTriggerServer(t, svc, nil)

	rec := postTrigger(t, srv, 0, testScheduleID, triggerBody(testTriggerID))
	if rec.Code != http.StatusCreated {
		t.Fatalf("状态码 = %d（%s）", rec.Code, rec.Body.String())
	}
	var trig triggerResponse
	decodeData(t, rec.Body.Bytes(), &trig)
	// 等待上限随答复一起来：root 读不到计划（那是 Web 账号的路由），
	// 少了这个字段 --wait 只能退回兜底上限，与单元的超时对不上。
	if trig.MaxRuntimeSeconds != 90 {
		t.Fatalf("max_runtime_seconds = %d，期望 90", trig.MaxRuntimeSeconds)
	}

	own := getTriggerRun(t, srv, 0, testScheduleID, trig.OperationID)
	if own.Code != http.StatusOK {
		t.Fatalf("root 读自己触发的操作 = %d（%s）", own.Code, own.Body.String())
	}
	var op operationResponse
	decodeData(t, own.Body.Bytes(), &op)
	if op.OperationID != trig.OperationID || op.Stage == "" {
		t.Fatalf("操作视图 = %+v", op)
	}

	// 另一条计划 id：路径形状合法，但那条操作不是它触发的 → 404，与"不存在"同形。
	otherSchedule := "sch_" + strings.Repeat("1", 32)
	if other := getTriggerRun(t, srv, 0, otherSchedule, trig.OperationID); other.Code != http.StatusNotFound {
		t.Fatalf("别的计划读这条操作 = %d，期望 404", other.Code)
	}

	// Web 账号连这条路都进不来：gate 在任何业务处理之前按形状拒掉。
	if web := getTriggerRun(t, srv, testWebUID, testScheduleID, trig.OperationID); web.Code != http.StatusForbidden {
		t.Fatalf("Web 账号读触发结果 = %d，期望 403", web.Code)
	}
}

// TestTriggerRunRejectsBadRequests：形状不对、方法不对、没有操作都要说清楚。
func TestTriggerRunRejectsBadRequests(t *testing.T) {
	srv, _ := newTriggerServer(t, newFakeScheduleService(), nil)

	req := httptest.NewRequest(http.MethodPost,
		"/internal/v1/schedules/"+testScheduleID+"/trigger-runs/"+testTriggerOperationID, nil)
	if rec := serveAs(srv, req, 0); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST 状态码 = %d，期望 405", rec.Code)
	}
	// 形状不对的操作 ID 根本不构成"专用路由"，于是回到普通规则：root 不是 Web 账号，
	// 403 在 gate 就结束了。定时单元只会用触发答复里那个 ID，永远走不到这里。
	if rec := getTriggerRun(t, srv, 0, testScheduleID, "op_x"); rec.Code != http.StatusForbidden {
		t.Fatalf("形状不对的操作 ID（root）= %d，期望 403", rec.Code)
	}
	// 换个 UID 就能看出这不是"路径不存在"，而是"这条路径不属于 root"。
	if rec := getTriggerRun(t, srv, testWebUID, testScheduleID, "op_x"); rec.Code != http.StatusNotFound {
		t.Fatalf("形状不对的操作 ID（Web）= %d，期望 404", rec.Code)
	}
	if rec := getTriggerRun(t, srv, 0, testScheduleID, testTriggerOperationID); rec.Code != http.StatusNotFound {
		t.Fatalf("不存在的操作 = %d，期望 404", rec.Code)
	}
}

// TestTriggerRunNotFoundWithoutService：没接线就是 404，不是空 200。
func TestTriggerRunNotFoundWithoutService(t *testing.T) {
	srv, _ := newTriggerServer(t, nil, nil)
	if rec := getTriggerRun(t, srv, 0, testScheduleID, testTriggerOperationID); rec.Code != http.StatusNotFound {
		t.Fatalf("状态码 = %d", rec.Code)
	}
}
