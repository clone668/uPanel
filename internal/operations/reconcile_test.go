package operations

// P03.4 Web 侧对账用例：重启后补齐、提交未到达、结果未知、提交途中不判定、重复提交。
//
// 全部跑在真实 SQLite 上（临时目录 + 真实迁移）：这些行为靠的是事务、条件更新与触发器，
// 内存假仓库证明不了它们。

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"upanel/internal/agentclient"
)

// notFound 是代理“查不到这条操作”的响应。
func notFound() error {
	return &agentclient.Error{Status: 404, Code: "NOT_FOUND", Message: "操作不存在"}
}

// jobStage 读回任务的公开状态与步骤。
func jobStage(t *testing.T, f *jobFixture, jobID string) (status, step, errJSON string, needsSync int) {
	t.Helper()
	if err := f.db.QueryRowContext(context.Background(),
		`SELECT status, step, error_json, needs_sync FROM jobs WHERE id = ?`, jobID).
		Scan(&status, &step, &errJSON, &needsSync); err != nil {
		t.Fatalf("读取任务状态失败: %v", err)
	}
	return status, step, errJSON, needsSync
}

// eventKinds 返回某个任务的事件类型序列。
func eventKinds(t *testing.T, f *jobFixture, jobID string) []string {
	t.Helper()
	rows, err := f.db.QueryContext(context.Background(),
		`SELECT kind FROM job_events WHERE job_id = ? ORDER BY seq`, jobID)
	if err != nil {
		t.Fatalf("读取事件失败: %v", err)
	}
	defer rows.Close()
	var kinds []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			t.Fatalf("读取事件失败: %v", err)
		}
		kinds = append(kinds, k)
	}
	return kinds
}

// TestSweepPendingResolvesQueuedJobAfterAgentComesBack 验收“Web 重启后补齐”。
//
// 场景：提交时代理不可用（任务留在 queued + needs_sync=1），用户关掉页面，Web 重启。
// 代理回来了，并且其实已经接受过这条操作。启动扫描必须把状态补齐，且**不再提交一次**。
func TestSweepPendingResolvesQueuedJobAfterAgentComesBack(t *testing.T) {
	f := newJobFixture(t, nil)
	pf := f.preflight(7)

	f.agent.opErr = &agentclient.UnavailableError{Err: errors.New("dial unix: connection refused")}
	job := f.mustSubmit(f.submitInput(pf.ID, "key-sweep-1", "PING AAAA"))
	if job.Status != "queued" || !job.NeedsSync {
		t.Fatalf("提交失败后任务 = %+v，期望 queued + needs_sync", job)
	}

	// 代理恢复：这条操作确实存在，正在跑。
	f.agent.opErr = nil
	f.agent.getErr = nil
	f.agent.getOp = agentclient.Operation{
		OperationID: job.OperationID,
		Action:      f.plan.Action,
		Stage:       "running",
		Step:        "executing",
		Cancellable: true,
	}

	// 模拟 Web 重启：新进程、同一份库。内存里的在途登记已经没了，这正是重启后的真实状态。
	restarted, err := New(f.svc.db, f.agent, f.svc.logger, Options{Now: f.clock.Now})
	if err != nil {
		t.Fatalf("重启后构造服务失败: %v", err)
	}
	stats, err := restarted.SweepPending(context.Background(), ReconcileBatch)
	if err != nil {
		t.Fatalf("启动对账失败: %v", err)
	}
	if stats.Scanned != 1 || stats.Advanced != 1 {
		t.Fatalf("对账统计 = %+v，期望扫描 1 条、推进 1 条", stats)
	}
	status, step, _, needsSync := jobStage(t, f, job.ID)
	if status != "running" || step != "executing" || needsSync != 0 {
		t.Fatalf("对账后 status=%q step=%q needs_sync=%d，期望 running/executing/0",
			status, step, needsSync)
	}
	// 关键：对账只查不跑。代理收到的操作提交次数仍然是失败的那一次。
	if got := len(f.agent.opCalls); got != 1 {
		t.Fatalf("代理收到 %d 次操作提交，期望 1（对账不得重新提交）", got)
	}

	// 再扫一次：已同步的任务不该产生第二条事件，也不该再问代理。
	eventsBefore := len(eventKinds(t, f, job.ID))
	stats, err = restarted.SweepPending(context.Background(), ReconcileBatch)
	if err != nil {
		t.Fatalf("第二轮对账失败: %v", err)
	}
	if stats.Scanned != 1 || stats.Advanced != 0 {
		t.Fatalf("第二轮统计 = %+v，期望扫描但不推进", stats)
	}
	if got := len(eventKinds(t, f, job.ID)); got != eventsBefore {
		t.Fatalf("第二轮对账追加了事件：%d → %d（事件记的是变化，不是回声）", eventsBefore, got)
	}
}

// TestSweepPendingMarksUnconfirmedWhenAgentNeverSawOperation 验收“提交没到达”。
//
// Web 落库后崩溃，代理从没见过这条操作。对账必须写 failed + unconfirmed，
// 而不是假装成功、也不是无限期挂着 queued。
func TestSweepPendingMarksUnconfirmedWhenAgentNeverSawOperation(t *testing.T) {
	f := newJobFixture(t, nil)
	pf := f.preflight(7)

	f.agent.opErr = &agentclient.UnavailableError{Err: errors.New("dial unix: connection refused")}
	job := f.mustSubmit(f.submitInput(pf.ID, "key-sweep-2", "PING AAAA"))

	f.agent.opErr = nil
	f.agent.getErr = notFound()
	stats, err := f.svc.SweepPending(context.Background(), ReconcileBatch)
	if err != nil {
		t.Fatalf("对账失败: %v", err)
	}
	if stats.Unconfirmed != 1 || stats.Advanced != 0 {
		t.Fatalf("对账统计 = %+v，期望 unconfirmed=1", stats)
	}
	status, step, errJSON, needsSync := jobStage(t, f, job.ID)
	if status != "failed" || step != "unconfirmed" || needsSync != 0 {
		t.Fatalf("status=%q step=%q needs_sync=%d，期望 failed/unconfirmed/0", status, step, needsSync)
	}
	if !strings.Contains(errJSON, "AGENT_UNAVAILABLE") {
		t.Fatalf("错误体 = %s，期望带 AGENT_UNAVAILABLE", errJSON)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(errJSON), &body); err != nil {
		t.Fatalf("错误体不是 JSON: %v", err)
	}
	if msg, _ := body["message"].(string); !strings.Contains(msg, "重新预检") {
		t.Fatalf("错误信息没有给出下一步: %q", msg)
	}
}

// TestSweepPendingMarksUnknownResultInterrupted 验收“执行结果未知”。
//
// 任务曾经同步成功过（代理确认接受了），之后代理查不到它——代理的数据被清理或回滚。
// 此时不能写 failed（那是在断言“没执行”），只能写 interrupted，并清掉可取消标记。
func TestSweepPendingMarksUnknownResultInterrupted(t *testing.T) {
	f := newJobFixture(t, nil)
	pf := f.preflight(7)
	job := f.mustSubmit(f.submitInput(pf.ID, "key-sweep-3", "PING AAAA"))

	// 正常情况下第一次提交就已经同步过：代理确认接受，needs_sync 清零。
	if status, _, _, needsSync := jobStage(t, f, job.ID); status != "queued" || needsSync != 0 {
		t.Fatalf("提交后 status=%q needs_sync=%d，期望 queued/0", status, needsSync)
	}

	f.agent.getErr = notFound()
	stats, err := f.svc.SweepPending(context.Background(), ReconcileBatch)
	if err != nil {
		t.Fatalf("对账失败: %v", err)
	}
	if stats.Unknown != 1 {
		t.Fatalf("对账统计 = %+v，期望 unknown=1", stats)
	}
	status, step, errJSON, needsSync := jobStage(t, f, job.ID)
	if status != "interrupted" || step != "unknown_result" || needsSync != 0 {
		t.Fatalf("status=%q step=%q needs_sync=%d，期望 interrupted/unknown_result/0",
			status, step, needsSync)
	}
	if !strings.Contains(errJSON, "OPERATION_INTERRUPTED") {
		t.Fatalf("错误体 = %s，期望带 OPERATION_INTERRUPTED", errJSON)
	}
	if !strings.Contains(errJSON, "核对") {
		t.Fatalf("错误体没有要求人工核对: %s", errJSON)
	}
	var cancellable int
	if err := f.db.QueryRowContext(context.Background(),
		`SELECT cancellable FROM jobs WHERE id = ?`, job.ID).Scan(&cancellable); err != nil {
		t.Fatalf("读取 cancellable 失败: %v", err)
	}
	if cancellable != 0 {
		t.Fatal("结果未知的任务仍然可取消：取消一个不知道结果的任务只会让状态更糊")
	}
	kinds := eventKinds(t, f, job.ID)
	if kinds[len(kinds)-1] != "status" {
		t.Fatalf("最后一条事件 = %q，期望 status", kinds[len(kinds)-1])
	}

	// 终态之后不再动它：再扫一遍不推进、不改状态。
	stats, err = f.svc.SweepPending(context.Background(), ReconcileBatch)
	if err != nil {
		t.Fatalf("第二轮对账失败: %v", err)
	}
	if stats.Scanned != 0 {
		t.Fatalf("第二轮扫描了 %d 条终态任务，期望 0", stats.Scanned)
	}
	if status2, _, _, _ := jobStage(t, f, job.ID); status2 != "interrupted" {
		t.Fatalf("第二轮之后 status=%q，期望仍是 interrupted", status2)
	}
}

// TestSweepPendingSkipsInFlightSubmit 验收“提交途中不判定”。
//
// 这条用例守的是一个会毁磁盘的错：格式化任务已经落库，CreateOperation 还没返回，
// 此时对账问代理必然得到 NOT_FOUND。若据此写 failed，用户会看到“失败”，
// 而代理随后照样执行——面板说了谎，且是最危险的那种。
func TestSweepPendingSkipsInFlightSubmit(t *testing.T) {
	f := newJobFixture(t, nil)
	pf := f.preflight(7)
	job := f.mustSubmit(f.submitInput(pf.ID, "key-sweep-4", "PING AAAA"))

	// 把任务放回“还没同步”的状态，并登记为在途（等价于 CreateOperation 正在返回路上）。
	if _, err := f.db.ExecContext(context.Background(),
		`UPDATE jobs SET status = 'queued', step = '', needs_sync = 1 WHERE id = ?`, job.ID); err != nil {
		t.Fatalf("准备在途状态失败: %v", err)
	}
	f.agent.getErr = notFound()
	f.svc.beginSubmit(job.ID)
	defer f.svc.endSubmit(job.ID)

	stats, err := f.svc.SweepPending(context.Background(), ReconcileBatch)
	if err != nil {
		t.Fatalf("对账失败: %v", err)
	}
	if stats.SkippedInflight != 1 || stats.Scanned != 0 {
		t.Fatalf("对账统计 = %+v，期望跳过 1 条在途任务", stats)
	}
	if got := len(f.agent.getCalls); got != 0 {
		t.Fatalf("在途任务被问了 %d 次代理，期望 0", got)
	}
	status, _, _, needsSync := jobStage(t, f, job.ID)
	if status != "queued" || needsSync != 1 {
		t.Fatalf("在途任务 status=%q needs_sync=%d，期望保持 queued/1", status, needsSync)
	}

	// 提交返回后（解除在途），下一轮就该正常判定。
	f.svc.endSubmit(job.ID)
	stats, err = f.svc.SweepPending(context.Background(), ReconcileBatch)
	if err != nil {
		t.Fatalf("第二轮对账失败: %v", err)
	}
	if stats.Unconfirmed != 1 {
		t.Fatalf("第二轮统计 = %+v，期望 unconfirmed=1", stats)
	}
}

// TestSweepPendingKeepsJobWhenAgentIsDown 验收“代理离线不猜”。
func TestSweepPendingKeepsJobWhenAgentIsDown(t *testing.T) {
	f := newJobFixture(t, nil)
	pf := f.preflight(7)
	job := f.mustSubmit(f.submitInput(pf.ID, "key-sweep-5", "PING AAAA"))

	f.agent.getErr = &agentclient.UnavailableError{Err: errors.New("i/o timeout")}
	stats, err := f.svc.SweepPending(context.Background(), ReconcileBatch)
	if err != nil {
		t.Fatalf("对账失败: %v", err)
	}
	if stats.Deferred != 1 || stats.Unconfirmed != 0 || stats.Unknown != 0 {
		t.Fatalf("对账统计 = %+v，期望 deferred=1 且不做结论", stats)
	}
	status, _, _, _ := jobStage(t, f, job.ID)
	if status != "queued" {
		t.Fatalf("代理离线时 status=%q，期望保持 queued", status)
	}
}

// TestSweepSkipsTerminalJobs 保证扫描范围只含非终态与待对账任务。
func TestSweepSkipsTerminalJobs(t *testing.T) {
	f := newJobFixture(t, nil)
	pf := f.preflight(7)
	job := f.mustSubmit(f.submitInput(pf.ID, "key-sweep-6", "PING AAAA"))

	f.agent.getOp = agentclient.Operation{
		OperationID: job.OperationID,
		Stage:       "succeeded",
		Step:        "done",
		Result:      json.RawMessage(`{"ok":true}`),
	}
	if _, err := f.svc.SweepPending(context.Background(), ReconcileBatch); err != nil {
		t.Fatalf("对账失败: %v", err)
	}
	if status, _, _, _ := jobStage(t, f, job.ID); status != "succeeded" {
		t.Fatalf("status=%q，期望 succeeded", status)
	}
	callsBefore := len(f.agent.getCalls)
	stats, err := f.svc.SweepPending(context.Background(), ReconcileBatch)
	if err != nil {
		t.Fatalf("第二轮对账失败: %v", err)
	}
	if stats.Scanned != 0 {
		t.Fatalf("对终态任务又扫了 %d 条，期望 0", stats.Scanned)
	}
	if len(f.agent.getCalls) != callsBefore {
		t.Fatal("终态任务仍然被拿去问代理")
	}
}

// TestRepeatSubmitSurvivesWebRestart 验收“重复提交 + Web 重启”。
//
// 十次提交、一次执行；换一个进程（同一份库）再提交同一个键，仍然拿回原任务，
// 代理不会再收到一次操作。这是 CONTRACTS.md JOB-01 的 Web 侧证据。
func TestRepeatSubmitSurvivesWebRestart(t *testing.T) {
	f := newJobFixture(t, nil)
	pf := f.preflight(7)
	first := f.mustSubmit(f.submitInput(pf.ID, "key-restart", "PING AAAA"))
	for i := 1; i < 10; i++ {
		job := f.mustSubmit(f.submitInput(pf.ID, "key-restart", "PING AAAA"))
		if job.ID != first.ID || !job.Duplicate {
			t.Fatalf("第 %d 次提交 job=%q duplicate=%v，期望原任务 %q 的重放",
				i+1, job.ID, job.Duplicate, first.ID)
		}
	}
	if got := len(f.agent.opCalls); got != 1 {
		t.Fatalf("代理收到 %d 次操作提交，期望 1", got)
	}

	restarted, err := New(f.svc.db, f.agent, f.svc.logger, Options{Now: f.clock.Now})
	if err != nil {
		t.Fatalf("重启后构造服务失败: %v", err)
	}
	job, err := restarted.SubmitJob(context.Background(), f.submitInput(pf.ID, "key-restart", "PING AAAA"))
	if err != nil {
		t.Fatalf("重启后重放提交失败: %v", err)
	}
	if job.ID != first.ID || !job.Duplicate {
		t.Fatalf("重启后重放 job=%q duplicate=%v，期望原任务 %q", job.ID, job.Duplicate, first.ID)
	}
	if got := len(f.agent.opCalls); got != 1 {
		t.Fatalf("重启后重放让代理收到 %d 次操作提交，期望仍是 1", got)
	}
}

// TestSubmitRacingSweepDoesNotMarkFalseFailure 验收“提交与对账并发”。
//
// 代理调用被卡住（模拟慢提交），对账同时在扫：这期间绝不能把任务判成 failed/unknown，
// 提交返回后任务必须是代理给出的真实阶段。
func TestSubmitRacingSweepDoesNotMarkFalseFailure(t *testing.T) {
	f := newJobFixture(t, nil)
	pf := f.preflight(7)

	gate := make(chan struct{})
	entered := make(chan struct{})
	f.agent.opHook = func() {
		close(entered)
		<-gate
	}
	done := make(chan Job, 1)
	go func() {
		job, err := f.svc.SubmitJob(context.Background(), f.submitInput(pf.ID, "key-race", "PING AAAA"))
		if err != nil {
			t.Errorf("并发提交失败: %v", err)
		}
		done <- job
	}()
	<-entered

	// 提交卡在代理调用里，代理此刻“没有”这条操作。
	f.agent.getErr = notFound()
	stats, err := f.svc.SweepPending(context.Background(), ReconcileBatch)
	if err != nil {
		t.Fatalf("并发对账失败: %v", err)
	}
	if stats.SkippedInflight != 1 || stats.Unconfirmed != 0 || stats.Unknown != 0 {
		t.Fatalf("并发对账统计 = %+v，期望只跳过在途任务", stats)
	}

	// 放行提交：代理接受。
	f.agent.getErr = nil
	f.agent.op = agentclient.Operation{
		OperationID: "op_race",
		Action:      f.plan.Action,
		Stage:       "accepted",
		Cancellable: true,
	}
	close(gate)
	job := <-done
	if job.Status != "queued" || job.NeedsSync {
		t.Fatalf("提交返回后 status=%q needs_sync=%v，期望 queued/false", job.Status, job.NeedsSync)
	}
	if status, step, _, needsSync := jobStage(t, f, job.ID); status != "queued" || needsSync != 0 {
		t.Fatalf("库里 status=%q step=%q needs_sync=%d，期望 queued 且已同步", status, step, needsSync)
	}
}

// TestRunReconcilerStopsWithContext 验收后台循环的退出行为：ctx 取消即返回，不拖住进程。
func TestRunReconcilerStopsWithContext(t *testing.T) {
	f := newJobFixture(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.svc.RunReconciler(ctx)
	}()
	// 等启动那一轮跑完（fakeAgent 记了调用；没有待对账任务时不会有调用，所以只看日志之外
	// 的可见信号：等一小会儿再取消，要求 goroutine 及时退出）。
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("RunReconciler 在 ctx 取消后没有退出")
	}
}

// TestPurgeExpiredIdempotencyKeysRemovesFinishedJobs 验收“至少保留 24 小时”之后的清理。
func TestPurgeExpiredIdempotencyKeysRemovesFinishedJobs(t *testing.T) {
	f := newJobFixture(t, nil)
	pf := f.preflight(7)
	job := f.mustSubmit(f.submitInput(pf.ID, "key-purge-1", "PING AAAA"))

	// 还没过期：一条都不该删。
	if n, err := f.svc.PurgeExpiredIdempotencyKeys(context.Background()); err != nil || n != 0 {
		t.Fatalf("未过期时清理了 %d 条（err=%v），期望 0", n, err)
	}

	f.agent.getOp = agentclient.Operation{
		OperationID: job.OperationID,
		Stage:       "succeeded",
		Step:        "done",
		Result:      json.RawMessage(`{"ok":true}`),
	}
	if _, err := f.svc.GetJob(context.Background(), 7, job.ID); err != nil {
		t.Fatalf("读回任务失败: %v", err)
	}

	f.clock.Advance(25 * time.Hour)
	n, err := f.svc.PurgeExpiredIdempotencyKeys(context.Background())
	if err != nil {
		t.Fatalf("清理过期幂等记录失败: %v", err)
	}
	if n != 1 {
		t.Fatalf("清理了 %d 条，期望 1", n)
	}
	if got := countRows(t, f.db, `SELECT COUNT(*) FROM idempotency_keys`); got != 0 {
		t.Fatalf("清理后仍有 %d 条幂等记录", got)
	}
	if got := countRows(t, f.db, `SELECT COUNT(*) FROM jobs`); got != 1 {
		t.Fatalf("清理幂等记录不该动执行历史：jobs 行数 = %d", got)
	}
}

// TestPurgeExpiredIdempotencyKeysKeepsUnfinishedJobs 是这条清理最危险的边界：
// 任务还没结束就删键，用户重试会拿到第二次执行。
func TestPurgeExpiredIdempotencyKeysKeepsUnfinishedJobs(t *testing.T) {
	f := newJobFixture(t, nil)
	pf := f.preflight(7)
	job := f.mustSubmit(f.submitInput(pf.ID, "key-purge-2", "PING AAAA"))
	if status, _, _, _ := jobStage(t, f, job.ID); status != "queued" {
		t.Fatalf("status=%q，期望 queued（用例前提是任务未结束）", status)
	}

	f.clock.Advance(25 * time.Hour)
	n, err := f.svc.PurgeExpiredIdempotencyKeys(context.Background())
	if err != nil {
		t.Fatalf("清理过期幂等记录失败: %v", err)
	}
	if n != 0 {
		t.Fatalf("清理了 %d 条未结束任务的幂等记录，期望 0", n)
	}
	// 键还在：重试仍然拿回原任务，代理不会收到第二次操作。
	replay := f.mustSubmit(f.submitInput(pf.ID, "key-purge-2", "PING AAAA"))
	if replay.ID != job.ID || !replay.Duplicate {
		t.Fatalf("重试拿到 job=%q duplicate=%v，期望原任务 %q", replay.ID, replay.Duplicate, job.ID)
	}
	if got := len(f.agent.opCalls); got != 1 {
		t.Fatalf("代理收到 %d 次操作提交，期望 1", got)
	}
}
