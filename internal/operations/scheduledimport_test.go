package operations

// 定时操作的对账导入（P12.4）。
//
// 断言的重点不是"能被导入"，而是五件事：
//  1. 导入是**如实抄写**：动作、参数与哈希、目标、会话哈希、阶段、时间都来自代理，
//     面板不自己编一个；
//  2. 两条表约束是硬约束：impersonal 的 user_id 用保留账号（只在不出现于账号管理里），
//     preflight_id 指向一条本地预检行——所以导入必须同时落下这三行；
//  3. 幂等：同一批导入十次也只有一条任务，靠 operation_id 的去重；
//  4. 导入的任务带 needs_sync=1，同一轮对账就会把它推进到代理说的阶段；
//  5. 代理不可用是"下一轮再说"（Deferred），不是错误，更不能被写成一堆失败任务。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"upanel/internal/agentclient"
	"upanel/internal/schedules"
	"upanel/internal/store"
)

const (
	importOpID     = "op_0123456789abcdef0123456789abcdef"
	importPlanRef  = "pf_0123456789abcdef0123456789abcdef"
	importAction   = "backup.run"
	importSession  = "aa11bb22cc33dd44ee55ff66aa77bb88cc99dd00ee11ff22aa33bb44cc55dd66"
	importAtLayout = time.RFC3339
)

// sampleScheduledOperation 是一条"代理自己跑过"的定时操作。
func sampleScheduledOperation() agentclient.ScheduledOperation {
	created := time.Date(2026, 10, 23, 19, 30, 0, 0, time.UTC)
	return agentclient.ScheduledOperation{
		OperationID:  importOpID,
		Action:       importAction,
		PreflightRef: importPlanRef,
		Params:       json.RawMessage(`{"backup_policy_id":"pol_0123456789abcdef0123456789abcdef","version":4}`),
		ParamsHash:   strings.Repeat("ab", 32),
		StateVersion: "4",
		Target:       json.RawMessage(`{"display_name":"夜间根文件"}`),
		SessionHash:  importSession,
		Stage:        "succeeded",
		Step:         "done",
		Result:       json.RawMessage(`{"outcome":"verified","observed_state":{"snapshot_id":"abc"}}`),
		Error:        json.RawMessage(`{}`),
		CreatedAt:    created,
		StartedAt:    created.Add(time.Second),
		FinishedAt:   created.Add(3 * time.Minute),
		UpdatedAt:    created.Add(3 * time.Minute),
	}
}

// TestImportScheduledWritesAllThreeRows：导入必须同时落下保留账号、预检、任务。
func TestImportScheduledWritesAllThreeRows(t *testing.T) {
	agent := &fakeAgent{scheduled: []agentclient.ScheduledOperation{sampleScheduledOperation()}}
	svc, db := newTestService(t, agent, nil)

	stats, err := svc.ImportScheduled(context.Background(), 0)
	if err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	if stats.Asked != 1 || stats.Imported != 1 || stats.Skipped != 0 || stats.Deferred {
		t.Fatalf("统计不对：%+v", stats)
	}
	if agent.scheduledLimit != scheduledImportBatch {
		t.Fatalf("limit = %d，期望 %d", agent.scheduledLimit, scheduledImportBatch)
	}

	// 任务行：字段来自代理，不是面板编的。
	var (
		jobID, opID, preflightID, action, status, step string
		userID                                         int64
		needsSync, cancellable                         int
		result, errJSON, created, updated, finished    string
		started                                        string
	)
	err = db.SQL().QueryRow(`SELECT id, operation_id, preflight_id, user_id, action, status, step,
		result_json, error_json, cancellable, needs_sync, created_at, updated_at, started_at, finished_at
		FROM jobs WHERE operation_id = ?`, importOpID).
		Scan(&jobID, &opID, &preflightID, &userID, &action, &status, &step,
			&result, &errJSON, &cancellable, &needsSync, &created, &updated, &started, &finished)
	if err != nil {
		t.Fatalf("读任务失败: %v", err)
	}
	if userID != schedules.SystemUserID {
		t.Fatalf("user_id = %d，期望保留值 %d", userID, schedules.SystemUserID)
	}
	if action != importAction || status != "succeeded" || step != "done" {
		t.Fatalf("任务内容不对：action=%s status=%s step=%s", action, status, step)
	}
	if needsSync != 1 {
		t.Fatal("导入的任务必须带 needs_sync=1：状态由下一轮对账回代理核对")
	}
	if cancellable != 0 {
		t.Fatal("定时操作不该带取消入口：没有人在键盘前做过再认证")
	}
	if created != "2026-10-23T19:30:00Z" || started != "2026-10-23T19:30:01Z" ||
		finished != "2026-10-23T19:33:00Z" {
		t.Fatalf("时间没有如实抄写：%s / %s / %s", created, started, finished)
	}
	if !strings.Contains(result, `"snapshot_id":"abc"`) {
		t.Fatalf("结果没有抄过来：%s", result)
	}
	if jobID != derivedScheduledJobID(importOpID) {
		t.Fatalf("job id = %s，期望派生值（重复导入必须得到同一个 id）", jobID)
	}

	// 预检行：如实抄写代理那一份。
	var (
		pfAction, pfParamsHash, pfParams, pfTarget, pfAgentRef, pfSession string
		pfStateVersion, pfCreated, pfExpires, pfConsumed                  string
		pfUserID                                                          int64
		pfAllowed                                                         int
	)
	err = db.SQL().QueryRow(`SELECT user_id, action, params_hash, normalized_params, target_json,
		state_version, agent_ref, session_hash, allowed, created_at, expires_at, consumed_job_id
		FROM preflights WHERE id = ?`, preflightID).
		Scan(&pfUserID, &pfAction, &pfParamsHash, &pfParams, &pfTarget, &pfStateVersion,
			&pfAgentRef, &pfSession, &pfAllowed, &pfCreated, &pfExpires, &pfConsumed)
	if err != nil {
		t.Fatalf("读预检失败: %v", err)
	}
	if pfUserID != schedules.SystemUserID || pfAction != importAction {
		t.Fatalf("预检归属/动作不对：%d / %s", pfUserID, pfAction)
	}
	if pfAgentRef != importPlanRef || pfSession != importSession {
		t.Fatalf("代理引用/会话哈希没抄过来：%s / %s", pfAgentRef, pfSession)
	}
	if pfParamsHash != strings.Repeat("ab", 32) || !strings.Contains(pfParams, `"version":4`) {
		t.Fatalf("参数与哈希不对：%s / %s", pfParamsHash, pfParams)
	}
	if pfStateVersion != "4" || !strings.Contains(pfTarget, "夜间根文件") {
		t.Fatalf("目标快照没抄过来：%s / %s", pfStateVersion, pfTarget)
	}
	if pfConsumed != jobID {
		t.Fatalf("consumed_job_id = %s，期望 %s", pfConsumed, jobID)
	}
	if pfCreated != "2026-10-23T19:30:00Z" || pfExpires != "2026-10-23T19:32:00Z" {
		t.Fatalf("预检时间不对：%s → %s", pfCreated, pfExpires)
	}

	// 保留账号：disabled、不可校验的口令标记、viewer。
	var (
		sysName, sysHash, sysRole string
		sysDisabled               int
	)
	err = db.SQL().QueryRow(`SELECT username, password_hash, role, disabled FROM users WHERE id = ?`,
		schedules.SystemUserID).Scan(&sysName, &sysHash, &sysRole, &sysDisabled)
	if err != nil {
		t.Fatalf("读保留账号失败: %v", err)
	}
	if sysDisabled != 1 || sysRole != "viewer" {
		t.Fatalf("保留账号必须不可登录：disabled=%d role=%s", sysDisabled, sysRole)
	}
	if sysHash == "" || !strings.HasPrefix(sysHash, "!") {
		t.Fatalf("保留账号的口令必须是不可能校验通过的标记，实际 %q", sysHash)
	}

	// 一条状态事件，来源要说清是导入的。
	var (
		kind    string
		payload string
	)
	if err := db.SQL().QueryRow(`SELECT kind, redacted_payload FROM job_events WHERE job_id = ?`, jobID).
		Scan(&kind, &payload); err != nil {
		t.Fatalf("读任务事件失败: %v", err)
	}
	if kind != "status" || !strings.Contains(payload, "scheduled_import") {
		t.Fatalf("任务事件不对：%s %s", kind, payload)
	}
}

// TestImportScheduledIsIdempotent：同一批导入十次也只有一条任务。
func TestImportScheduledIsIdempotent(t *testing.T) {
	agent := &fakeAgent{scheduled: []agentclient.ScheduledOperation{sampleScheduledOperation()}}
	svc, db := newTestService(t, agent, nil)

	for i := 0; i < 10; i++ {
		stats, err := svc.ImportScheduled(context.Background(), 0)
		if err != nil {
			t.Fatalf("第 %d 次导入失败: %v", i+1, err)
		}
		if i == 0 && stats.Imported != 1 {
			t.Fatalf("第一次导入 Imported = %d，期望 1", stats.Imported)
		}
		if i > 0 && (stats.Imported != 0 || stats.Skipped != 1) {
			t.Fatalf("第 %d 次导入应当全部跳过，实际 %+v", i+1, stats)
		}
	}
	assertCount(t, db, `SELECT COUNT(*) FROM jobs WHERE operation_id = ?`, 1, importOpID)
	assertCount(t, db, `SELECT COUNT(*) FROM preflights WHERE agent_ref = ?`, 1, importPlanRef)
}

// TestImportScheduledUsesWatermark：水位来自已导入任务的最新创建时间。
func TestImportScheduledUsesWatermark(t *testing.T) {
	agent := &fakeAgent{scheduled: []agentclient.ScheduledOperation{sampleScheduledOperation()}}
	svc, _ := newTestService(t, agent, nil)

	if _, err := svc.ImportScheduled(context.Background(), 0); err != nil {
		t.Fatalf("第一次导入失败: %v", err)
	}
	if !agent.scheduledSince.IsZero() {
		t.Fatalf("第一次导入不该带水位，实际 %s", agent.scheduledSince)
	}
	if _, err := svc.ImportScheduled(context.Background(), 0); err != nil {
		t.Fatalf("第二次导入失败: %v", err)
	}
	want := time.Date(2026, 10, 23, 19, 30, 0, 0, time.UTC)
	if !agent.scheduledSince.Equal(want) || agent.scheduledSinceID != importOpID {
		t.Fatalf("复合水位 = %s / %s，期望 %s / %s", agent.scheduledSince, agent.scheduledSinceID, want, importOpID)
	}
}

// TestImportScheduledSameTimestampPagination：同秒超过一批时，第二页从 ID 之后继续。
func TestImportScheduledSameTimestampPagination(t *testing.T) {
	scheduled := make([]agentclient.ScheduledOperation, 0, 101)
	for i := 1; i <= 101; i++ {
		op := sampleScheduledOperation()
		op.OperationID = fmt.Sprintf("op_%032x", i)
		op.PreflightRef = fmt.Sprintf("pf_%032x", i)
		scheduled = append(scheduled, op)
	}
	agent := &fakeAgent{scheduled: scheduled, scheduledPaged: true}
	svc, db := newTestService(t, agent, nil)

	first, err := svc.ImportScheduled(context.Background(), 100)
	if err != nil || first.Imported != 100 {
		t.Fatalf("第一批导入不对：%+v / %v", first, err)
	}
	second, err := svc.ImportScheduled(context.Background(), 100)
	if err != nil || second.Imported != 1 {
		t.Fatalf("同时间戳的第二批被漏掉：%+v / %v", second, err)
	}
	assertCount(t, db, `SELECT COUNT(*) FROM jobs WHERE user_id = ?`, 101, schedules.SystemUserID)
	if agent.scheduledSinceID != fmt.Sprintf("op_%032x", 100) {
		t.Fatalf("第二页游标 ID = %q，期望第 100 条", agent.scheduledSinceID)
	}
}

// TestImportScheduledFailureDoesNotAdvancePastRecord：失败项之后的记录不能抢跑水位。
func TestImportScheduledFailureDoesNotAdvancePastRecord(t *testing.T) {
	first := sampleScheduledOperation()
	second := sampleScheduledOperation()
	second.OperationID = "op_1123456789abcdef0123456789abcdef"
	second.PreflightRef = "pf_1123456789abcdef0123456789abcdef"
	second.CreatedAt = second.CreatedAt.Add(time.Second)
	second.Stage = "暂时无法识别"
	third := sampleScheduledOperation()
	third.OperationID = "op_2123456789abcdef0123456789abcdef"
	third.PreflightRef = "pf_2123456789abcdef0123456789abcdef"
	third.CreatedAt = third.CreatedAt.Add(2 * time.Second)
	agent := &fakeAgent{scheduled: []agentclient.ScheduledOperation{first, second, third}, scheduledPaged: true}
	svc, db := newTestService(t, agent, nil)

	stats, err := svc.ImportScheduled(context.Background(), 100)
	if err != nil || stats.Imported != 1 {
		t.Fatalf("失败项之前的一条应导入，之后的一条必须停下：%+v / %v", stats, err)
	}
	assertCount(t, db, `SELECT COUNT(*) FROM jobs`, 1)

	agent.scheduled[1].Stage = "succeeded"
	stats, err = svc.ImportScheduled(context.Background(), 100)
	if err != nil || stats.Imported != 2 {
		t.Fatalf("恢复后应重试失败项并继续：%+v / %v", stats, err)
	}
	assertCount(t, db, `SELECT COUNT(*) FROM jobs`, 3)
	if agent.scheduledSinceID != first.OperationID {
		t.Fatalf("重试游标 = %q，期望停在最后成功的 %q", agent.scheduledSinceID, first.OperationID)
	}
}

// TestImportScheduledDeferredWhenAgentDown：代理不可用是"下一轮再说"，不是错误。
func TestImportScheduledDeferredWhenAgentDown(t *testing.T) {
	agent := &fakeAgent{scheduledErr: &agentclient.UnavailableError{Err: errors.New("dial unix: connection refused")}}
	svc, db := newTestService(t, agent, nil)

	stats, err := svc.ImportScheduled(context.Background(), 0)
	if err != nil {
		t.Fatalf("代理不可用不该报错: %v", err)
	}
	if !stats.Deferred || stats.Imported != 0 {
		t.Fatalf("统计不对：%+v", stats)
	}
	assertCount(t, db, `SELECT COUNT(*) FROM jobs`, 0)
	// 连保留账号都不该被建出来：一轮没做成的导入不该留下任何痕迹。
	assertCount(t, db, `SELECT COUNT(*) FROM users WHERE id = ?`, 0, schedules.SystemUserID)
}

// TestImportScheduledRejectsUnknownStage：阶段不认识就不导——猜一个状态比不导更糟。
func TestImportScheduledRejectsUnknownStage(t *testing.T) {
	op := sampleScheduledOperation()
	op.Stage = "半路卡住"
	agent := &fakeAgent{scheduled: []agentclient.ScheduledOperation{op}}
	svc, db := newTestService(t, agent, nil)

	stats, err := svc.ImportScheduled(context.Background(), 0)
	if err != nil {
		t.Fatalf("导入不该整体失败: %v", err)
	}
	if stats.Imported != 0 || stats.Asked != 1 {
		t.Fatalf("统计不对：%+v", stats)
	}
	assertCount(t, db, `SELECT COUNT(*) FROM jobs`, 0)
}

// TestImportScheduledWithoutAgent：演示模式没有代理，导入是空操作。
func TestImportScheduledWithoutAgent(t *testing.T) {
	svc, db := newTestService(t, nil, nil)
	stats, err := svc.ImportScheduled(context.Background(), 0)
	if err != nil || stats.Asked != 0 || stats.Imported != 0 {
		t.Fatalf("没有代理时应当是空操作：%+v / %v", stats, err)
	}
	assertCount(t, db, `SELECT COUNT(*) FROM jobs`, 0)
}

// TestSweepImportsThenAdvances：同一轮扫描里，导入的任务会被立刻对账推进。
//
// 这是"导入"与"对账"之间的那个承诺：页面不该为了看到定时任务的状态而多等半分钟。
func TestSweepImportsThenAdvances(t *testing.T) {
	op := sampleScheduledOperation()
	op.Stage = "accepted"
	op.Step = ""
	op.StartedAt = time.Time{}
	op.FinishedAt = time.Time{}
	agent := &fakeAgent{
		scheduled: []agentclient.ScheduledOperation{op},
		// 代理那边其实已经跑完了：同一轮对账要把它推进到 succeeded。
		getOp: agentclient.Operation{
			OperationID: importOpID,
			Action:      importAction,
			Stage:       "succeeded",
			Step:        "done",
			Result:      json.RawMessage(`{"outcome":"verified"}`),
			CreatedAt:   op.CreatedAt.Format(importAtLayout),
			UpdatedAt:   op.CreatedAt.Add(2 * time.Minute).Format(importAtLayout),
			FinishedAt:  op.CreatedAt.Add(2 * time.Minute).Format(importAtLayout),
		},
	}
	svc, db := newTestService(t, agent, nil)

	stats, err := svc.SweepPending(context.Background(), 0)
	if err != nil {
		t.Fatalf("扫描失败: %v", err)
	}
	if stats.Imported != 1 {
		t.Fatalf("Imported = %d，期望 1", stats.Imported)
	}
	if stats.Advanced != 1 {
		t.Fatalf("Advanced = %d，期望 1（导入的任务应当在同一轮被推进）", stats.Advanced)
	}

	var status string
	var needsSync int
	if err := db.SQL().QueryRow(`SELECT status, needs_sync FROM jobs WHERE operation_id = ?`, importOpID).
		Scan(&status, &needsSync); err != nil {
		t.Fatalf("读任务失败: %v", err)
	}
	if status != "succeeded" {
		t.Fatalf("任务状态 = %s，期望 succeeded（代理说的）", status)
	}
	if needsSync != 0 {
		t.Fatal("对账确认过之后 needs_sync 必须清成 0")
	}
}

// TestSweepClearsNeedsSyncOnAlreadyTerminalImport：导入时就已经跑完的任务也必须收敛。
//
// 现场形状（P12.4，24.04 VM）：定时触发两秒就结束，对账三十秒后才把它导进任务表，
// 于是导入落下的第一条记录**本身就是终态**。此时 syncFromAgent 的条件更新按
// "终态不可回退"被 WHERE 挡在门外，needs_sync 跟着一起留在 1：任务中心那一栏永远
// 说"还没跟代理核对过"，而扫描视它为待对账、每 30 秒再问一遍代理，一圈一圈没有尽头
// （真机上过了三个小时还是 needs_sync=1）。
//
// 断言两件事：标记会清；清完之后这一行不再进扫描集。
func TestSweepClearsNeedsSyncOnAlreadyTerminalImport(t *testing.T) {
	op := sampleScheduledOperation() // Stage 已经是 succeeded / done
	agent := &fakeAgent{
		scheduled: []agentclient.ScheduledOperation{op},
		getOp: agentclient.Operation{
			OperationID: importOpID,
			Action:      importAction,
			Stage:       "succeeded",
			Step:        "done",
			Result:      op.Result,
			CreatedAt:   op.CreatedAt.Format(importAtLayout),
			UpdatedAt:   op.UpdatedAt.Format(importAtLayout),
			FinishedAt:  op.FinishedAt.Format(importAtLayout),
		},
	}
	svc, db := newTestService(t, agent, nil)

	stats, err := svc.SweepPending(context.Background(), 0)
	if err != nil {
		t.Fatalf("扫描失败: %v", err)
	}
	if stats.Imported != 1 {
		t.Fatalf("Imported = %d，期望 1", stats.Imported)
	}

	var status string
	var needsSync int
	if err := db.SQL().QueryRow(`SELECT status, needs_sync FROM jobs WHERE operation_id = ?`, importOpID).
		Scan(&status, &needsSync); err != nil {
		t.Fatalf("读任务失败: %v", err)
	}
	if status != "succeeded" {
		t.Fatalf("任务状态 = %s，期望 succeeded（终态不可回退，代理也是这么说的）", status)
	}
	if needsSync != 0 {
		t.Fatal("导入时已终态的任务也要清 needs_sync：否则它永远在对账队列里，面板那栏也一直在说谎")
	}

	// 清完之后这一行就该离开扫描集：终态且不需要核对。
	before := len(agent.getCalls)
	if _, err := svc.SweepPending(context.Background(), 0); err != nil {
		t.Fatalf("第二轮扫描失败: %v", err)
	}
	if extra := len(agent.getCalls) - before; extra != 0 {
		t.Fatalf("第二轮扫描又问了代理 %d 次，期望 0（已终态且已核对的任务不再进扫描集）", extra)
	}
}

// assertCount 断言一条计数查询的结果。
func assertCount(t *testing.T, db *store.DB, query string, want int, args ...any) {
	t.Helper()
	var got int
	if err := db.SQL().QueryRow(query, args...).Scan(&got); err != nil {
		t.Fatalf("计数查询失败（%s）: %v", query, err)
	}
	if got != want {
		t.Fatalf("计数 = %d，期望 %d（%s）", got, want, query)
	}
}

// TestImportScheduledFiresFinishedHookForTerminalJob：导入时就已经结束的任务也要
// 补一次结束回调。
//
// 这是定时续期能写回证书账本的唯一通路：触发两秒结束、对账三十秒后才把它导进
// 任务表，那条任务一进库就是终态，永远不会"从非终态推进到终态"，于是
// syncFromAgent 里那一次 notifyFinished 永远不会发生——盘上的证书换了，面板上
// 的指纹与到期时间还停在续期之前。
func TestImportScheduledFiresFinishedHookForTerminalJob(t *testing.T) {
	op := sampleScheduledOperation() // Stage 已经是 succeeded / done
	op.Action = "certificate.renew"
	op.Params = json.RawMessage(`{"domain":"panel.example.test","staging":true,"scheduled":true}`)
	agent := &fakeAgent{scheduled: []agentclient.ScheduledOperation{op}}
	svc, _ := newTestService(t, agent, nil)
	hook := &hookRecorder{}
	svc.onJobFinished = hook.record

	if _, err := svc.ImportScheduled(context.Background(), 0); err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	if hook.count() != 1 {
		t.Fatalf("结束回调次数 = %d，期望 1", hook.count())
	}
	if hook.jobs[0].Action != "certificate.renew" || hook.jobs[0].Status != "succeeded" {
		t.Fatalf("回调拿到的任务不对: %+v", hook.jobs[0])
	}
	// 参数是这次续期真正用的那一份（代理写的），定时续期靠它按域名对账。
	var params map[string]any
	if err := json.Unmarshal(hook.args[0], &params); err != nil {
		t.Fatalf("回调参数不是合法 JSON（%s）: %v", string(hook.args[0]), err)
	}
	if params["domain"] != "panel.example.test" || params["scheduled"] != true {
		t.Fatalf("回调参数不对: %s", string(hook.args[0]))
	}

	// 再导一次：幂等导入不得让账本被写第二遍。
	if _, err := svc.ImportScheduled(context.Background(), 0); err != nil {
		t.Fatalf("第二轮导入失败: %v", err)
	}
	if hook.count() != 1 {
		t.Fatalf("重复导入又触发了一次结束回调（%d 次）", hook.count())
	}
}

// TestImportScheduledDoesNotFireHookForRunningJob：还没结束的任务不补回调——
// 它之后由对账推进到终态时，syncFromAgent 会照常触发一次。
func TestImportScheduledDoesNotFireHookForRunningJob(t *testing.T) {
	op := sampleScheduledOperation()
	op.Action = "certificate.renew"
	op.Stage = "running"
	op.Step = "certbot"
	op.Result = json.RawMessage(`{}`)
	op.FinishedAt = time.Time{}
	agent := &fakeAgent{scheduled: []agentclient.ScheduledOperation{op}}
	svc, _ := newTestService(t, agent, nil)
	hook := &hookRecorder{}
	svc.onJobFinished = hook.record

	if _, err := svc.ImportScheduled(context.Background(), 0); err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	if hook.count() != 0 {
		t.Fatalf("还在跑的任务不该补结束回调（%d 次）", hook.count())
	}
}
