package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"upanel/internal/agentstore"
	"upanel/internal/httpx"
	"upanel/internal/processes"
)

// fakeProcesses 是 agent 层的假进程采集器：这一层验证的是参数校验、策略选择与
// 计划/结果映射。进程包自己的测试负责“真 /proc、真信号、真身份”的语义。
type fakeProcesses struct {
	snapshot processes.Snapshot

	assessment processes.Assessment
	assessErr  error

	result     processes.SignalResult
	executeErr error

	assessed []processes.SignalParams
	executed []processes.SignalParams
	progress []string
}

func (f *fakeProcesses) Snapshot() processes.Snapshot { return f.snapshot }

func (f *fakeProcesses) Assess(_ context.Context, params processes.SignalParams) (processes.Assessment, error) {
	f.assessed = append(f.assessed, params)
	return f.assessment, f.assessErr
}

func (f *fakeProcesses) ExecuteSignal(_ context.Context, params processes.SignalParams, progress func(string, map[string]any)) (processes.SignalResult, error) {
	f.executed = append(f.executed, params)
	if progress != nil {
		progress("signal_"+strings.ToLower(params.Signal), map[string]any{"pid": params.PID})
		f.progress = append(f.progress, "signal_"+strings.ToLower(params.Signal))
		progress("verified", map[string]any{"pid": params.PID})
		f.progress = append(f.progress, "verified")
	}
	return f.result, f.executeErr
}

// sampleProcess 是测试用的目标进程（PID + starttime 才构成身份）。
func sampleProcess() processes.Process {
	return processes.Process{
		PID:            39515,
		StartTimeTicks: 277319,
		PPID:           39443,
		Owner:          "upanel",
		State:          processes.StateSleeping,
		Command:        "sleep 15",
		Unit:           "session-705.scope",
		Protection:     processes.Protection{Reasons: []string{}},
		Capabilities:   []string{processes.CapTERM, processes.CapKILL},
	}
}

// newFakeProcesses 返回一份“默认可以执行 TERM”的假采集器。
func newFakeProcesses() *fakeProcesses {
	p := sampleProcess()
	return &fakeProcesses{
		snapshot: processes.Snapshot{
			Status:      processes.StatusOK,
			CollectedAt: time.Date(2026, 10, 21, 8, 0, 0, 0, time.UTC),
			IntervalMS:  5000,
			Total:       1,
			Processes:   []processes.Process{p},
		},
		assessment: processes.Assessment{
			Found:        true,
			Process:      p,
			DisplayName:  "PID 39515（upanel：sleep 15）",
			Identity:     processes.Identity(p.PID, p.StartTimeTicks),
			StateVersion: processes.StateVersion(p.PID, p.StartTimeTicks),
			Allowed:      true,
			Warnings:     []processes.Reason{{Code: processes.WarningTerminate, Message: "SIGTERM 只是请求退出"}},
			Impacts:      []processes.Impact{{Kind: processes.ImpactTermination, Description: "进程会退出"}},
			Locks:        []string{processes.SignalLock(p.PID, p.StartTimeTicks)},
		},
		result: processes.SignalResult{
			PID:            p.PID,
			StartTimeTicks: p.StartTimeTicks,
			Signal:         processes.SignalTERM,
			Delivered:      true,
			Gone:           true,
			StateBefore:    processes.StateSleeping,
			StateAfter:     "gone",
			Before:         p,
			Duration:       120 * time.Millisecond,
		},
	}
}

// newProcessesRegistry 只登记 P05.2 的进程查询与动作。
func newProcessesRegistry(t *testing.T, src processesSource) *Registry {
	t.Helper()
	reg := NewRegistry()
	if err := reg.RegisterQuery(newProcessesListQuery(src)); err != nil {
		t.Fatalf("登记 processes.list 失败: %v", err)
	}
	if err := reg.RegisterAction(newProcessSignalAction(src)); err != nil {
		t.Fatalf("登记 process.signal 失败: %v", err)
	}
	return reg
}

func TestProcessesListQueryReturnsSnapshot(t *testing.T) {
	src := newFakeProcesses()
	srv := newTestServerWithRegistry(t, newProcessesRegistry(t, src))

	rec := postQuery(t, srv, `{"query":"processes.list"}`, testWebUID)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（%s）", rec.Code, rec.Body.String())
	}
	var body struct {
		Data processes.Snapshot `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if body.Data.Status != processes.StatusOK || len(body.Data.Processes) != 1 {
		t.Fatalf("快照内容不对: %+v", body.Data)
	}
	p := body.Data.Processes[0]
	if p.PID != 39515 || p.StartTimeTicks != 277319 {
		t.Errorf("身份字段丢失: %+v", p)
	}
	if strings.Join(p.Capabilities, ",") != "TERM,KILL" {
		t.Errorf("能力字段 = %v", p.Capabilities)
	}
	if p.CPUPercent != nil {
		t.Errorf("CPU 百分比应为 null，实际 %v", *p.CPUPercent)
	}
}

// processes.list 不接受任何参数：塞命令不会被当成筛选条件。
func TestProcessesListQueryRejectsParams(t *testing.T) {
	srv := newTestServerWithRegistry(t, newProcessesRegistry(t, newFakeProcesses()))

	rec := postQuery(t, srv, `{"query":"processes.list","params":{"command":"kill -9 1"}}`, testWebUID)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("带参数的状态码 = %d，期望 400（%s）", rec.Code, rec.Body.String())
	}
}

func TestProcessSignalDecode(t *testing.T) {
	src := newFakeProcesses()
	srv, _ := newPreflightServer(t, newProcessesRegistry(t, src))

	cases := []struct {
		name   string
		params string
		wantOK bool
	}{
		{"正常 TERM", `{"pid":39515,"start_time_ticks":277319,"signal":"TERM"}`, true},
		{"小写与空格", `{"pid":39515,"start_time_ticks":277319,"signal":" term "}`, true},
		{"正常 KILL", `{"pid":39515,"start_time_ticks":277319,"signal":"KILL"}`, true},
		{"缺 starttime", `{"pid":39515,"signal":"TERM"}`, false},
		{"starttime 为零", `{"pid":39515,"start_time_ticks":0,"signal":"TERM"}`, false},
		{"pid 为零", `{"pid":0,"start_time_ticks":277319,"signal":"TERM"}`, false},
		{"pid 为负", `{"pid":-1,"start_time_ticks":277319,"signal":"TERM"}`, false},
		{"未知信号", `{"pid":39515,"start_time_ticks":277319,"signal":"HUP"}`, false},
		{"数字信号", `{"pid":39515,"start_time_ticks":277319,"signal":"9"}`, false},
		{"多余字段", `{"pid":39515,"start_time_ticks":277319,"signal":"TERM","force":true}`, false},
		{"自由字段", `{"pid":39515,"start_time_ticks":277319,"signal":"TERM","command":"kill -9 1"}`, false},
		{"批量目标", `{"pids":[39515],"start_time_ticks":277319,"signal":"TERM"}`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := postPreflight(t, srv, testWebUID, preflightBody(processes.ActionSignal, c.params))
			ok := rec.Code == http.StatusCreated
			if ok != c.wantOK {
				t.Fatalf("状态码 = %d，期望成功=%v（%s）", rec.Code, c.wantOK, rec.Body.String())
			}
		})
	}
}

// TestProcessSignalPolicyDependsOnSignal 是 P05.2 的核心策略：
// TERM 不需要再认证与短语；KILL 两者都要，且短语首词是 KILL。
func TestProcessSignalPolicyDependsOnSignal(t *testing.T) {
	src := newFakeProcesses()
	srv, _ := newPreflightServer(t, newProcessesRegistry(t, src))

	term := mustPreflight(t, srv, processes.ActionSignal,
		`{"pid":39515,"start_time_ticks":277319,"signal":"TERM"}`)
	if term.RequiresReauth || term.RequiresPhrase || term.ConfirmationPhrase != "" {
		t.Fatalf("TERM 不该要求再认证或短语：%+v", term)
	}
	if term.Cancellable {
		t.Fatalf("process.signal 不可安全取消")
	}

	kill := mustPreflight(t, srv, processes.ActionSignal,
		`{"pid":39515,"start_time_ticks":277319,"signal":"KILL"}`)
	if !kill.RequiresReauth || !kill.RequiresPhrase {
		t.Fatalf("KILL 必须要求再认证与短语：%+v", kill)
	}
	if !strings.HasPrefix(kill.ConfirmationPhrase, processes.PhraseVerbKill+" ") {
		t.Fatalf("确认短语 = %q，期望以 KILL 开头", kill.ConfirmationPhrase)
	}
	// 同一进程、同一预检流程，唯一区别是信号——说明门槛确实随参数走。
	if term.Target.IdentityFingerprint != kill.Target.IdentityFingerprint {
		t.Fatalf("同一目标的身份指纹不该随信号变化")
	}
}

// TestProcessSignalPreflightMapsAssessment 预检把评估结论映射成计划。
func TestProcessSignalPreflightMapsAssessment(t *testing.T) {
	src := newFakeProcesses()
	src.assessment.Allowed = false
	src.assessment.Blockers = []processes.Reason{{
		Code:    processes.CodeProtected,
		Message: "受保护的进程，面板不允许向它发送任何信号：PID 1 是 init",
	}}
	src.assessment.Impacts = []processes.Impact{{Kind: processes.ImpactNone, Description: "受保护的进程不会被改动"}}
	// 评估的是 PID 1（starttime=96），身份与锁必须来自评估结论而不是请求参数。
	src.assessment.Identity = processes.Identity(1, 96)
	src.assessment.StateVersion = processes.StateVersion(1, 96)
	src.assessment.Locks = []string{processes.SignalLock(1, 96)}
	src.assessment.DisplayName = "PID 1（root：/sbin/init）"
	srv, _ := newPreflightServer(t, newProcessesRegistry(t, src))

	plan := mustPreflight(t, srv, processes.ActionSignal,
		`{"pid":1,"start_time_ticks":96,"signal":"KILL"}`)
	if plan.Allowed {
		t.Fatal("带阻塞项的预检必须 allowed=false")
	}
	if len(plan.Blockers) != 1 || plan.Blockers[0].Code != processes.CodeProtected {
		t.Fatalf("阻塞项 = %+v", plan.Blockers)
	}
	if plan.Target.IdentityFingerprint != processes.Identity(1, 96) {
		t.Errorf("身份指纹 = %q", plan.Target.IdentityFingerprint)
	}
	if len(plan.Locks) != 1 || plan.Locks[0] != processes.SignalLock(1, 96) {
		t.Errorf("锁 = %v", plan.Locks)
	}
	// 预检只读：只有评估，没有执行。
	if len(src.executed) != 0 {
		t.Errorf("预检阶段调用了执行: %+v", src.executed)
	}
	// 规范化参数：signal 大写。
	var norm struct {
		PID            int64  `json:"pid"`
		StartTimeTicks uint64 `json:"start_time_ticks"`
		Signal         string `json:"signal"`
	}
	if err := json.Unmarshal(plan.NormalizedParams, &norm); err != nil {
		t.Fatalf("规范化参数不是合法 JSON: %v", err)
	}
	if norm.PID != 1 || norm.StartTimeTicks != 96 || norm.Signal != "KILL" {
		t.Errorf("规范化参数 = %+v", norm)
	}
	// 被拒绝的预检也不能省掉 KILL 的门槛：策略与是否允许无关。
	if !plan.RequiresReauth || !plan.RequiresPhrase {
		t.Errorf("KILL 的策略必须与允许与否无关：%+v", plan)
	}
}

// TestProcessSignalTERMExecutesWithoutPhrase TERM 一次提交即执行，不需要短语。
func TestProcessSignalTERMExecutesWithoutPhrase(t *testing.T) {
	src := newFakeProcesses()
	srv, _, _ := newOperationServer(t, newProcessesRegistry(t, src))

	plan := mustPreflight(t, srv, processes.ActionSignal,
		`{"pid":39515,"start_time_ticks":277319,"signal":"TERM"}`)
	id := opID("proc-term")
	rec := submitOperation(t, srv, operationSubmitBody(id, plan.PreflightRef, "", testSessionHash, ""))
	if rec.Code != http.StatusCreated {
		t.Fatalf("提交状态码 = %d（%s）", rec.Code, rec.Body.String())
	}
	final := waitForStage(t, srv, id, agentstore.StageSucceeded, agentstore.StageFailed)
	if final.Stage != agentstore.StageSucceeded {
		t.Fatalf("任务阶段 = %q（%s）", final.Stage, final.Error)
	}
	if len(src.executed) != 1 || src.executed[0].Signal != processes.SignalTERM {
		t.Fatalf("执行参数 = %+v", src.executed)
	}
	if src.executed[0].StartTimeTicks != 277319 {
		t.Fatalf("执行时丢了 starttime：%+v", src.executed[0])
	}
	if strings.Join(src.progress, ",") != "signal_term,verified" {
		t.Errorf("进度步骤 = %v", src.progress)
	}

	var result struct {
		Outcome       string         `json:"outcome"`
		ObservedState map[string]any `json:"observed_state"`
		Data          map[string]any `json:"data"`
	}
	if err := json.Unmarshal(final.Result, &result); err != nil {
		t.Fatalf("结果不是合法 JSON: %v（%s）", err, final.Result)
	}
	if result.Outcome != OutcomeVerified {
		t.Errorf("outcome = %q", result.Outcome)
	}
	if result.ObservedState["state_after"] != "gone" || result.ObservedState["gone"] != true {
		t.Errorf("读回状态 = %+v", result.ObservedState)
	}
	if result.ObservedState["signal"] != processes.SignalTERM {
		t.Errorf("读回信号 = %v", result.ObservedState["signal"])
	}
	if result.Data["delivered"] != true {
		t.Errorf("delivered = %v", result.Data["delivered"])
	}
}

// TestProcessSignalKILLRequiresPhraseAndReauth KILL 的两道门槛都必须真的挡住。
func TestProcessSignalKILLRequiresPhraseAndReauth(t *testing.T) {
	src := newFakeProcesses()
	srv, _, _ := newOperationServer(t, newProcessesRegistry(t, src))

	plan := mustPreflight(t, srv, processes.ActionSignal,
		`{"pid":39515,"start_time_ticks":277319,"signal":"KILL"}`)

	// 1) 没有短语：CONFIRMATION_MISMATCH。
	rec := submitOperation(t, srv, operationSubmitBody(opID("proc-kill-1"), plan.PreflightRef, "", testSessionHash, recentReauth()))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("无短语的状态码 = %d，期望 400（%s）", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), string(httpx.CodeConfirmationMismatch)) {
		t.Fatalf("错误码不对：%s", rec.Body.String())
	}

	// 2) 短语错误：同样是 CONFIRMATION_MISMATCH。
	rec = submitOperation(t, srv, operationSubmitBody(opID("proc-kill-2"), plan.PreflightRef, "KILL WRONG", testSessionHash, recentReauth()))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), string(httpx.CodeConfirmationMismatch)) {
		t.Fatalf("短语错误的状态码 = %d（%s）", rec.Code, rec.Body.String())
	}

	// 3) 短语正确但没有再认证：REAUTH_REQUIRED。
	rec = submitOperation(t, srv, operationSubmitBody(opID("proc-kill-3"), plan.PreflightRef, plan.ConfirmationPhrase, testSessionHash, ""))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("无可再认证的状态码 = %d，期望 403（%s）", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), string(httpx.CodeReauthRequired)) {
		t.Fatalf("错误码不对：%s", rec.Body.String())
	}

	// 4) 再认证已过期（10 分钟前）：REAUTH_REQUIRED。
	stale := time.Now().UTC().Add(-10 * time.Minute).Format(time.RFC3339)
	rec = submitOperation(t, srv, operationSubmitBody(opID("proc-kill-4"), plan.PreflightRef, plan.ConfirmationPhrase, testSessionHash, stale))
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), string(httpx.CodeReauthRequired)) {
		t.Fatalf("再认证过期的状态码 = %d（%s）", rec.Code, rec.Body.String())
	}

	// 前面四次都被拒绝：一个信号都没发出去。
	if len(src.executed) != 0 {
		t.Fatalf("被拒绝的提交仍然执行了动作：%+v", src.executed)
	}

	// 5) 短语 + 5 分钟内的再认证：执行。
	rec = submitOperation(t, srv, operationSubmitBody(opID("proc-kill-5"), plan.PreflightRef, plan.ConfirmationPhrase, testSessionHash, recentReauth()))
	if rec.Code != http.StatusCreated {
		t.Fatalf("合法提交的状态码 = %d（%s）", rec.Code, rec.Body.String())
	}
	final := waitForStage(t, srv, opID("proc-kill-5"), agentstore.StageSucceeded, agentstore.StageFailed)
	if final.Stage != agentstore.StageSucceeded {
		t.Fatalf("任务阶段 = %q（%s）", final.Stage, final.Error)
	}
	if len(src.executed) != 1 || src.executed[0].Signal != processes.SignalKILL {
		t.Fatalf("执行参数 = %+v", src.executed)
	}
}

// TestProcessSignalBlockedPreflightCannotExecute 受保护目标：预检不允许，
// 即使有人绕过界面直接提交也执行不了。
func TestProcessSignalBlockedPreflightCannotExecute(t *testing.T) {
	src := newFakeProcesses()
	src.assessment.Allowed = false
	src.assessment.Blockers = []processes.Reason{{Code: processes.CodeProtected, Message: "PID 1 是 init"}}
	srv, _, _ := newOperationServer(t, newProcessesRegistry(t, src))

	plan := mustPreflight(t, srv, processes.ActionSignal,
		`{"pid":1,"start_time_ticks":96,"signal":"KILL"}`)
	if plan.Allowed {
		t.Fatal("受保护目标的预检必须 allowed=false")
	}
	rec := submitOperation(t, srv, operationSubmitBody(opID("proc-protected"), plan.PreflightRef, plan.ConfirmationPhrase, testSessionHash, recentReauth()))
	if rec.Code != http.StatusConflict {
		t.Fatalf("状态码 = %d，期望 409（%s）", rec.Code, rec.Body.String())
	}
	if len(src.executed) != 0 {
		t.Fatalf("受保护目标被执行了：%+v", src.executed)
	}
}

// TestProcessSignalReusedPIDInvalidatesPreflight 是 STAGES §7 的验收条款在框架层的落实：
// 预检记录的是 (PID, starttime)，PID 被重用后重算的身份指纹不同，旧预检提交即被拒，
// 新进程不会被旧预检杀掉。
func TestProcessSignalReusedPIDInvalidatesPreflight(t *testing.T) {
	src := newFakeProcesses()
	srv, _, _ := newOperationServer(t, newProcessesRegistry(t, src))

	plan := mustPreflight(t, srv, processes.ActionSignal,
		`{"pid":39515,"start_time_ticks":277319,"signal":"KILL"}`)

	// 预检之后：原进程退出，同一个 PID 被新进程拿走（starttime 变了）。
	src.assessment.Identity = processes.Identity(39515, 999999)
	src.assessment.StateVersion = processes.StateVersion(39515, 999999)
	src.assessment.Allowed = false
	src.assessment.Blockers = []processes.Reason{{
		Code:    processes.CodeIdentityMismatch,
		Message: "PID 39515 现在是另一个进程",
	}}

	rec := submitOperation(t, srv, operationSubmitBody(opID("proc-reuse"), plan.PreflightRef, plan.ConfirmationPhrase, testSessionHash, recentReauth()))
	if rec.Code != http.StatusConflict {
		t.Fatalf("状态码 = %d，期望 409（%s）", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), string(httpx.CodeDeviceIdentityChanged)) {
		t.Fatalf("错误码应为目标身份已变化：%s", rec.Body.String())
	}
	if len(src.executed) != 0 {
		t.Fatalf("PID 重用后仍然执行了信号：%+v —— 这是绝不能发生的", src.executed)
	}
}

// TestProcessSignalExecutionFailure 执行失败 → 任务 failed，且原因来自执行器。
func TestProcessSignalExecutionFailure(t *testing.T) {
	src := newFakeProcesses()
	src.executeErr = fmt.Errorf("%w: PID 39515 现在是另一个进程", processes.ErrSignalRejected)
	srv, _, _ := newOperationServer(t, newProcessesRegistry(t, src))

	plan := mustPreflight(t, srv, processes.ActionSignal,
		`{"pid":39515,"start_time_ticks":277319,"signal":"TERM"}`)
	id := opID("proc-fail")
	if rec := submitOperation(t, srv, operationSubmitBody(id, plan.PreflightRef, "", testSessionHash, "")); rec.Code != http.StatusCreated {
		t.Fatalf("提交状态码 = %d（%s）", rec.Code, rec.Body.String())
	}
	final := waitForStage(t, srv, id, agentstore.StageSucceeded, agentstore.StageFailed)
	if final.Stage != agentstore.StageFailed {
		t.Fatalf("任务阶段 = %q", final.Stage)
	}
	// 执行器的具体原因只进代理日志：任务里给的是通用失败 + 恢复提示，
	// 避免把内部错误文本当成运维指引展示。这里核对的就是这个约定。
	var failure struct {
		Code         string `json:"code"`
		Message      string `json:"message"`
		RecoveryHint string `json:"recovery_hint"`
	}
	if err := json.Unmarshal(final.Error, &failure); err != nil {
		t.Fatalf("失败原因不是合法 JSON: %v（%s）", err, final.Error)
	}
	if failure.Code != string(httpx.Code("COMMAND_FAILED")) || failure.RecoveryHint == "" {
		t.Fatalf("失败原因不合规：%+v", failure)
	}
}

// TestProcessSignalSurfacesAssessmentError 评估失败不是“预检不允许”，而是实现失败。
func TestProcessSignalSurfacesAssessmentError(t *testing.T) {
	src := newFakeProcesses()
	src.assessErr = fmt.Errorf("读取 /proc 失败")
	srv, _ := newPreflightServer(t, newProcessesRegistry(t, src))

	rec := postPreflight(t, srv, testWebUID, preflightBody(processes.ActionSignal,
		`{"pid":39515,"start_time_ticks":277319,"signal":"TERM"}`))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("状态码 = %d，期望 500（%s）", rec.Code, rec.Body.String())
	}
}

// TestRegisterActionRejectsPolicyConflict 静态策略与按参数策略只能二选一。
func TestRegisterActionRejectsPolicyConflict(t *testing.T) {
	reg := NewRegistry()
	err := reg.RegisterAction(Action{
		Name:   "process.signal",
		Decode: func(json.RawMessage) (any, error) { return nil, nil },
		Policy: ActionPolicy{RequiresReauth: true},
		PolicyFor: func(any) (ActionPolicy, error) {
			return ActionPolicy{}, nil
		},
	})
	if err == nil {
		t.Fatalf("同时登记静态 Policy 与 PolicyFor 必须被拒")
	}
}

// TestResolvePolicyValidatesDerivedPolicy 按参数算出来的策略也要过校验。
func TestResolvePolicyValidatesDerivedPolicy(t *testing.T) {
	src := newFakeProcesses()
	action := newProcessSignalAction(src)

	term, err := action.ResolvePolicy(processes.SignalParams{PID: 39515, StartTimeTicks: 277319, Signal: processes.SignalTERM})
	if err != nil {
		t.Fatalf("TERM 策略解析失败: %v", err)
	}
	if term.RequiresReauth || term.RequiresPhrase || term.PhraseVerb != "" {
		t.Fatalf("TERM 策略 = %+v", term)
	}
	kill, err := action.ResolvePolicy(processes.SignalParams{PID: 39515, StartTimeTicks: 277319, Signal: processes.SignalKILL})
	if err != nil {
		t.Fatalf("KILL 策略解析失败: %v", err)
	}
	if !kill.RequiresReauth || !kill.RequiresPhrase || kill.PhraseVerb != processes.PhraseVerbKill {
		t.Fatalf("KILL 策略 = %+v", kill)
	}
	// 未知信号：策略解析拒绝，而不是默认给一份“不需要短语”的策略。
	if _, err := action.ResolvePolicy(processes.SignalParams{Signal: "HUP"}); err == nil {
		t.Fatalf("未知信号必须被拒绝")
	}
	// 类型不对：同样是错误，不能静默落到 TERM 分支。
	if _, err := action.ResolvePolicy("not-params"); err == nil {
		t.Fatalf("参数类型不匹配必须报错")
	}
}
