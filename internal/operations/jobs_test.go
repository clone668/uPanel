package operations

// P03.2 服务层用例：幂等、一次性预检、提交失败的两条路（业务拒绝 / 不可用）、对账与取消。
//
// 这些用例跑在真实 SQLite 上（临时目录 + 真实迁移），不是内存假仓库：
// 事务边界、UNIQUE 约束、触发器行为本身就是被验收的东西。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"upanel/internal/agentclient"
)

// testSessionHashA 等是任务用例使用的会话哈希（64 位十六进制，满足 CHECK 约束）。
var (
	testSessionHashA = strings.Repeat("ab", 32)
	testSessionHashB = strings.Repeat("ef", 32)
)

// mutableClock 是可推进的时钟：预检过期、终态判断都要靠它。
type mutableClock struct {
	mu sync.Mutex
	at time.Time
}

func newClock(at time.Time) *mutableClock {
	return &mutableClock{at: at.UTC().Truncate(time.Second)}
}

func (c *mutableClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *mutableClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

// jobFixture 是一套跑在真实库上的“Web 侧 + 假代理”装置。
type jobFixture struct {
	t     *testing.T
	svc   *Service
	db    *sql.DB
	agent *fakeAgent
	clock *mutableClock
	plan  agentclient.PreflightPlan
}

// newJobFixture 建库、建账号、构造服务。plan 通过 modify 调整后用于生成预检。
func newJobFixture(t *testing.T, modify func(*agentclient.PreflightPlan)) *jobFixture {
	t.Helper()
	clock := newClock(time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC))
	agent := &fakeAgent{}
	plan := agentclient.PreflightPlan{
		Ref:              "pf_0123456789abcdef0123456789abcdef",
		Action:           "acceptance.ping",
		NormalizedParams: json.RawMessage(`{"label":"alpha"}`),
		Target: agentclient.PreflightTarget{
			DisplayName:         "验收对象（alpha）",
			IdentityFingerprint: "fp-alpha",
			StateVersion:        "1",
		},
		ExpiresAt:      clock.Now().Add(120 * time.Second).Format(time.RFC3339),
		Allowed:        true,
		Blockers:       []agentclient.PreflightNotice{},
		Warnings:       []agentclient.PreflightNotice{},
		Impacts:        []agentclient.PreflightImpact{},
		RequiresReauth: false,
		RequiresPhrase: true,
		Cancellable:    true,
		Locks:          []string{"acceptance:alpha"},
	}
	if modify != nil {
		modify(&plan)
	}
	agent.plan = plan
	agent.op = agentclient.Operation{
		OperationID: "op_pending",
		Action:      plan.Action,
		Stage:       "accepted",
		Step:        "accepted",
		Cancellable: plan.Cancellable,
	}

	svc, db := newTestService(t, agent, clock.Now)
	if _, err := db.ExecContext(context.Background(),
		`INSERT INTO users (id, username, password_hash, role, created_at, updated_at)
		 VALUES (8, 'other', 'x', 'admin', '2026-10-20T08:00:00Z', '2026-10-20T08:00:00Z')`); err != nil {
		t.Fatalf("准备第二个账号失败: %v", err)
	}
	return &jobFixture{t: t, svc: svc, db: db.SQL(), agent: agent, clock: clock, plan: plan}
}

// preflight 走真实路径生成一份预检（代理返回计划 → Web 落库）。
func (f *jobFixture) preflight(userID int64) Preflight {
	f.t.Helper()
	pf, err := f.svc.CreatePreflight(context.Background(), CreateInput{
		UserID:      userID,
		SessionHash: testSessionHashA,
		ReauthAt:    f.clock.Now().Add(-time.Minute),
		RequestID:   "rid-preflight",
		Action:      f.plan.Action,
		Params:      json.RawMessage(`{"label":"alpha"}`),
	})
	if err != nil {
		f.t.Fatalf("创建预检失败: %v", err)
	}
	return pf
}

// submitInput 返回一份合法的提交输入；调用方按需改字段。
func (f *jobFixture) submitInput(preflightID, key, phrase string) SubmitInput {
	return SubmitInput{
		UserID:         7,
		SessionHash:    testSessionHashA,
		ReauthAt:       f.clock.Now().Add(-time.Minute),
		RequestID:      "rid-submit",
		IdempotencyKey: key,
		PreflightID:    preflightID,
		Confirmation:   phrase,
	}
}

func (f *jobFixture) mustSubmit(in SubmitInput) Job {
	f.t.Helper()
	job, err := f.svc.SubmitJob(context.Background(), in)
	if err != nil {
		f.t.Fatalf("提交任务失败: %v", err)
	}
	return job
}

func countRows(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("计数失败（%s）: %v", query, err)
	}
	return n
}

// TestSubmitTenTimesExecutesOnce 是同一次请求提交十次只产生一次系统执行的 Web 侧证据。
func TestSubmitTenTimesExecutesOnce(t *testing.T) {
	f := newJobFixture(t, nil)
	pf := f.preflight(7)

	var first Job
	for i := 0; i < 10; i++ {
		job := f.mustSubmit(f.submitInput(pf.ID, "key-1", "PING AAAA"))
		if i == 0 {
			first = job
			continue
		}
		if job.ID != first.ID {
			t.Fatalf("第 %d 次提交产生了新任务 %q（首次 %q）", i+1, job.ID, first.ID)
		}
		if !job.Duplicate {
			t.Fatalf("第 %d 次提交应标记为幂等重放", i+1)
		}
	}
	if got := len(f.agent.opCalls); got != 1 {
		t.Fatalf("代理收到 %d 次操作提交，期望 1", got)
	}
	if got := countRows(t, f.db, `SELECT COUNT(*) FROM jobs`); got != 1 {
		t.Fatalf("jobs 行数 = %d，期望 1", got)
	}
	if got := countRows(t, f.db, `SELECT COUNT(*) FROM idempotency_keys`); got != 1 {
		t.Fatalf("idempotency_keys 行数 = %d，期望 1", got)
	}
	// 事件数 = 落库那一条 queued + 首次对账后 step 从空变为 accepted 那一条。
	// 关键不是“正好两条”，而是**不会随提交次数增长**：十次提交不能堆出十条流水。
	if got := countRows(t, f.db, `SELECT COUNT(*) FROM job_events`); got != 2 {
		t.Fatalf("job_events 行数 = %d，期望 2（queued + accepted 对账）", got)
	}
	// operation_id 必须由 Web 生成、先落库、再交给代理，三处一致。
	if !strings.HasPrefix(first.ID, jobIDPrefix) || len(first.ID) != len(jobIDPrefix)+32 {
		t.Fatalf("job_id 形态不合法: %q", first.ID)
	}
	if !strings.HasPrefix(first.OperationID, operationIDPrefix) || len(first.OperationID) != len(operationIDPrefix)+32 {
		t.Fatalf("operation_id 形态不合法: %q", first.OperationID)
	}
	if f.agent.opCalls[0].OperationID != first.OperationID {
		t.Fatalf("代理收到的 operation_id = %q，库里是 %q",
			f.agent.opCalls[0].OperationID, first.OperationID)
	}
	if f.agent.opCalls[0].PreflightRef != f.plan.Ref {
		t.Fatalf("代理收到的 preflight_ref = %q，期望 %q",
			f.agent.opCalls[0].PreflightRef, f.plan.Ref)
	}
}

// TestSameKeyDifferentBodyConflicts 验收“相同幂等键不同请求返回 409”。
func TestSameKeyDifferentBodyConflicts(t *testing.T) {
	f := newJobFixture(t, nil)
	pf := f.preflight(7)
	f.mustSubmit(f.submitInput(pf.ID, "key-1", "PING AAAA"))

	_, err := f.svc.SubmitJob(context.Background(), f.submitInput(pf.ID, "key-1", "PING BBBB"))
	if !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("错误 = %v，期望 ErrIdempotencyConflict", err)
	}
	if got := len(f.agent.opCalls); got != 1 {
		t.Fatalf("代理收到 %d 次提交，期望 1", got)
	}
}

// TestDifferentKeySamePreflightIsUsed 预检是一次性凭据：换幂等键也不行。
func TestDifferentKeySamePreflightIsUsed(t *testing.T) {
	f := newJobFixture(t, nil)
	pf := f.preflight(7)
	f.mustSubmit(f.submitInput(pf.ID, "key-1", "PING AAAA"))

	_, err := f.svc.SubmitJob(context.Background(), f.submitInput(pf.ID, "key-2", "PING AAAA"))
	if !errors.Is(err, ErrPreflightUsed) {
		t.Fatalf("错误 = %v，期望 ErrPreflightUsed", err)
	}
	if got := len(f.agent.opCalls); got != 1 {
		t.Fatalf("代理收到 %d 次提交，期望 1", got)
	}
}

// TestSubmitRejectsExpiredPreflight 验收“预检过期拒绝”。
func TestSubmitRejectsExpiredPreflight(t *testing.T) {
	f := newJobFixture(t, nil)
	pf := f.preflight(7)
	f.clock.Advance(121 * time.Second)

	_, err := f.svc.SubmitJob(context.Background(), f.submitInput(pf.ID, "key-1", "PING AAAA"))
	if !errors.Is(err, ErrPreflightExpired) {
		t.Fatalf("错误 = %v，期望 ErrPreflightExpired", err)
	}
	if got := len(f.agent.opCalls); got != 0 {
		t.Fatalf("过期预检不应提交给代理，实际 %d 次", got)
	}
	if got := countRows(t, f.db, `SELECT COUNT(*) FROM jobs`); got != 0 {
		t.Fatalf("被拒的提交不应留下任务，实际 %d 行", got)
	}
}

// TestSubmitRejectsSessionChange 验收“状态变化拒绝”里的会话轮换一项。
func TestSubmitRejectsSessionChange(t *testing.T) {
	f := newJobFixture(t, nil)
	pf := f.preflight(7)
	in := f.submitInput(pf.ID, "key-1", "PING AAAA")
	in.SessionHash = testSessionHashB

	_, err := f.svc.SubmitJob(context.Background(), in)
	if !errors.Is(err, ErrPreflightSessionChanged) {
		t.Fatalf("错误 = %v，期望 ErrPreflightSessionChanged", err)
	}
}

// TestSubmitRejectsForeignPreflight 别人的预检按“不存在”处理。
func TestSubmitRejectsForeignPreflight(t *testing.T) {
	f := newJobFixture(t, nil)
	pf := f.preflight(8)
	in := f.submitInput(pf.ID, "key-1", "PING AAAA")
	in.UserID = 7
	in.SessionHash = testSessionHashA

	_, err := f.svc.SubmitJob(context.Background(), in)
	if !errors.Is(err, ErrPreflightNotFound) {
		t.Fatalf("错误 = %v，期望 ErrPreflightNotFound", err)
	}
}

// TestSubmitRejectsBlockedPreflight 预检结论为“不允许”时提交必然被拒。
func TestSubmitRejectsBlockedPreflight(t *testing.T) {
	f := newJobFixture(t, func(p *agentclient.PreflightPlan) {
		p.Allowed = false
		p.Blockers = []agentclient.PreflightNotice{{Code: "mount_in_use", Message: "目标正在被挂载"}}
	})
	pf := f.preflight(7)

	_, err := f.svc.SubmitJob(context.Background(), f.submitInput(pf.ID, "key-1", "PING AAAA"))
	if !errors.Is(err, ErrPreflightBlocked) {
		t.Fatalf("错误 = %v，期望 ErrPreflightBlocked", err)
	}
}

// TestSubmitRequiresReauth 验收再认证窗口。
func TestSubmitRequiresReauth(t *testing.T) {
	f := newJobFixture(t, func(p *agentclient.PreflightPlan) { p.RequiresReauth = true })
	pf := f.preflight(7)

	in := f.submitInput(pf.ID, "key-1", "PING AAAA")
	in.ReauthAt = time.Time{}
	if _, err := f.svc.SubmitJob(context.Background(), in); !errors.Is(err, ErrReauthRequired) {
		t.Fatalf("错误 = %v，期望 ErrReauthRequired", err)
	}

	in.ReauthAt = f.clock.Now().Add(-6 * time.Minute)
	if _, err := f.svc.SubmitJob(context.Background(), in); !errors.Is(err, ErrReauthRequired) {
		t.Fatalf("超出窗口的错误 = %v，期望 ErrReauthRequired", err)
	}

	// 窗口内的再认证才放行；前面两次拒绝不能消费掉预检。
	in.ReauthAt = f.clock.Now().Add(-time.Minute)
	if _, err := f.svc.SubmitJob(context.Background(), in); err != nil {
		t.Fatalf("窗口内提交失败: %v", err)
	}
}

// TestSubmitAgentUnavailableKeepsJobForReconcile 验收“浏览器关闭/掉线后仍能找到原 operation”。
func TestSubmitAgentUnavailableKeepsJobForReconcile(t *testing.T) {
	f := newJobFixture(t, nil)
	pf := f.preflight(7)
	f.agent.opErr = &agentclient.UnavailableError{Err: errors.New("dial unix: connection refused")}

	job := f.mustSubmit(f.submitInput(pf.ID, "key-1", "PING AAAA"))
	if job.Status != "queued" || !job.NeedsSync {
		t.Fatalf("提交超时后任务 = %+v，期望 queued + needs_sync", job)
	}
	// 代理侧其实已经接受了这次操作（网络只是回包丢了）：对账要能找到它。
	f.agent.opErr = nil
	f.agent.getOp = agentclient.Operation{
		OperationID: job.OperationID,
		Action:      f.plan.Action,
		Stage:       "succeeded",
		Step:        "done",
		Cancellable: false,
		Result:      json.RawMessage(`{"outcome":"verified","verified_at":"2026-10-20T09:00:05Z","observed_state":{}}`),
		StartedAt:   "2026-10-20T09:00:01Z",
		FinishedAt:  "2026-10-20T09:00:05Z",
	}

	got, err := f.svc.GetJob(context.Background(), 7, job.ID)
	if err != nil {
		t.Fatalf("对账失败: %v", err)
	}
	if got.Status != "succeeded" || got.NeedsSync {
		t.Fatalf("对账后任务 = %+v，期望 succeeded 且已同步", got)
	}
	if got.FinishedAt.IsZero() || !strings.Contains(string(got.Result), "verified") {
		t.Fatalf("对账后缺少结果或完成时间: %+v", got)
	}
	// 关键：对账用的是同一个 operation_id，绝不重新提交。
	if len(f.agent.opCalls) != 1 {
		t.Fatalf("代理收到 %d 次提交，期望 1", len(f.agent.opCalls))
	}
	if len(f.agent.getCalls) != 1 || f.agent.getCalls[0] != job.OperationID {
		t.Fatalf("对账查询 = %v，期望按 %q 查一次", f.agent.getCalls, job.OperationID)
	}
}

// TestSubmitAgentRejectionMarksJobFailed 代理明确拒绝：任务失败、留痕、不重试。
func TestSubmitAgentRejectionMarksJobFailed(t *testing.T) {
	f := newJobFixture(t, nil)
	pf := f.preflight(7)
	f.agent.opErr = &agentclient.Error{Status: 409, Code: "PREFLIGHT_STALE", Message: "目标状态已变化"}

	job, err := f.svc.SubmitJob(context.Background(), f.submitInput(pf.ID, "key-1", "PING AAAA"))
	var rejected *Rejected
	if !errors.As(err, &rejected) || rejected.Code != "PREFLIGHT_STALE" {
		t.Fatalf("错误 = %v，期望 *Rejected{PREFLIGHT_STALE}", err)
	}
	if job.Status != "failed" || job.NeedsSync {
		t.Fatalf("任务 = %+v，期望 failed 且不再待对账", job)
	}
	if !strings.Contains(string(job.Error), "PREFLIGHT_STALE") {
		t.Fatalf("失败任务的 error_json 缺少错误码: %s", job.Error)
	}
	// 被拒之后任务仍在库里：拒绝本身也是需要留痕的事实。
	if got := countRows(t, f.db, `SELECT COUNT(*) FROM jobs WHERE status = 'failed'`); got != 1 {
		t.Fatalf("失败任务行数 = %d，期望 1", got)
	}
}

// TestGetJobUnconfirmedWhenAgentHasNoRecord 代理查不到操作：如实标失败，不重试。
func TestGetJobUnconfirmedWhenAgentHasNoRecord(t *testing.T) {
	f := newJobFixture(t, nil)
	pf := f.preflight(7)
	f.agent.opErr = &agentclient.UnavailableError{Err: errors.New("timeout")}
	job := f.mustSubmit(f.submitInput(pf.ID, "key-1", "PING AAAA"))

	f.agent.getErr = &agentclient.Error{Status: 404, Code: "NOT_FOUND", Message: "操作不存在"}
	got, err := f.svc.GetJob(context.Background(), 7, job.ID)
	if err != nil {
		t.Fatalf("读任务失败: %v", err)
	}
	if got.Status != "failed" || got.Step != "unconfirmed" {
		t.Fatalf("任务 = %+v，期望 failed/unconfirmed", got)
	}
	if !strings.Contains(string(got.Error), "AGENT_UNAVAILABLE") {
		t.Fatalf("error_json 缺少 AGENT_UNAVAILABLE: %s", got.Error)
	}
	if len(f.agent.opCalls) != 1 {
		t.Fatalf("代理收到 %d 次提交，期望 1（不能重试）", len(f.agent.opCalls))
	}
}

// TestGetJobTerminalDoesNotCallAgent 终态任务不再回连代理。
func TestGetJobTerminalDoesNotCallAgent(t *testing.T) {
	f := newJobFixture(t, nil)
	pf := f.preflight(7)
	f.agent.op.Stage = "succeeded"
	job := f.mustSubmit(f.submitInput(pf.ID, "key-1", "PING AAAA"))
	if job.Status != "succeeded" {
		t.Fatalf("任务状态 = %q，期望 succeeded", job.Status)
	}

	f.agent.getErr = errors.New("不该被调用")
	got, err := f.svc.GetJob(context.Background(), 7, job.ID)
	if err != nil {
		t.Fatalf("读终态任务失败: %v", err)
	}
	if got.Status != "succeeded" {
		t.Fatalf("状态 = %q", got.Status)
	}
	if len(f.agent.getCalls) != 0 {
		t.Fatalf("终态任务仍然查询了代理 %d 次", len(f.agent.getCalls))
	}
}

// TestCancelRespectsPolicy 验收“不可取消动作不显示可取消”。
func TestCancelRespectsPolicy(t *testing.T) {
	f := newJobFixture(t, func(p *agentclient.PreflightPlan) { p.Cancellable = false })
	f.agent.op.Cancellable = false
	pf := f.preflight(7)
	job := f.mustSubmit(f.submitInput(pf.ID, "key-1", "PING AAAA"))
	if job.Cancellable {
		t.Fatalf("不可取消的动作在任务视图里被标成可取消: %+v", job)
	}
	if _, err := f.svc.CancelJob(context.Background(), 7, job.ID); !errors.Is(err, ErrJobNotCancelable) {
		t.Fatalf("错误 = %v，期望 ErrJobNotCancelable", err)
	}
	if len(f.agent.cancelCalls) != 0 {
		t.Fatalf("不可取消的动作仍然问了代理 %d 次", len(f.agent.cancelCalls))
	}
}

// TestCancelCancellableJob 可取消动作：Web → 代理 → 状态回写。
func TestCancelCancellableJob(t *testing.T) {
	f := newJobFixture(t, nil)
	pf := f.preflight(7)
	job := f.mustSubmit(f.submitInput(pf.ID, "key-1", "PING AAAA"))
	if !job.Cancellable {
		t.Fatalf("可取消的动作在任务视图里被标成不可取消: %+v", job)
	}

	f.agent.cancelOp = agentclient.Operation{
		OperationID:           job.OperationID,
		Action:                f.plan.Action,
		Stage:                 "canceled",
		Step:                  "canceled",
		Cancellable:           true,
		CancellationRequested: true,
		FinishedAt:            "2026-10-20T09:00:02Z",
	}
	got, err := f.svc.CancelJob(context.Background(), 7, job.ID)
	if err != nil {
		t.Fatalf("取消失败: %v", err)
	}
	if got.Status != "canceled" || !got.CancellationRequested {
		t.Fatalf("取消后任务 = %+v，期望 canceled", got)
	}
	if len(f.agent.cancelCalls) != 1 || f.agent.cancelCalls[0] != job.OperationID {
		t.Fatalf("取消请求 = %v，期望按 %q 取消一次", f.agent.cancelCalls, job.OperationID)
	}
}

// TestCancelFinishedJob 终态任务不能再取消。
func TestCancelFinishedJob(t *testing.T) {
	f := newJobFixture(t, nil)
	pf := f.preflight(7)
	f.agent.op.Stage = "succeeded"
	job := f.mustSubmit(f.submitInput(pf.ID, "key-1", "PING AAAA"))

	if _, err := f.svc.CancelJob(context.Background(), 7, job.ID); !errors.Is(err, ErrJobFinished) {
		t.Fatalf("错误 = %v，期望 ErrJobFinished", err)
	}
	if len(f.agent.cancelCalls) != 0 {
		t.Fatalf("终态任务仍然问了代理 %d 次", len(f.agent.cancelCalls))
	}
}

// TestListAndGetRespectOwnership 任务列表只返回本人的记录。
func TestListAndGetRespectOwnership(t *testing.T) {
	f := newJobFixture(t, nil)
	mine := f.preflight(7)
	mineJob := f.mustSubmit(f.submitInput(mine.ID, "key-1", "PING AAAA"))

	// 第二个账号的预检与任务。
	pf, err := f.svc.CreatePreflight(context.Background(), CreateInput{
		UserID:      8,
		SessionHash: testSessionHashB,
		Action:      f.plan.Action,
		Params:      json.RawMessage(`{"label":"alpha"}`),
	})
	if err != nil {
		t.Fatalf("创建第二个预检失败: %v", err)
	}
	otherIn := SubmitInput{
		UserID:         8,
		SessionHash:    testSessionHashB,
		RequestID:      "rid-other",
		IdempotencyKey: "key-other",
		PreflightID:    pf.ID,
		Confirmation:   "PING AAAA",
	}
	if _, err := f.svc.SubmitJob(context.Background(), otherIn); err != nil {
		t.Fatalf("第二个账号提交失败: %v", err)
	}

	list, err := f.svc.ListJobs(context.Background(), 7, 50)
	if err != nil {
		t.Fatalf("读任务列表失败: %v", err)
	}
	if len(list) != 1 || list[0].ID != mineJob.ID {
		t.Fatalf("列表 = %+v，期望只有 %q", list, mineJob.ID)
	}
	if _, err := f.svc.GetJob(context.Background(), 8, mineJob.ID); !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("跨账号读取错误 = %v，期望 ErrJobNotFound", err)
	}
}

// TestSubmitValidatesIdempotencyKey 幂等键缺失或形状不合法一律拒绝。
func TestSubmitValidatesIdempotencyKey(t *testing.T) {
	f := newJobFixture(t, nil)
	pf := f.preflight(7)

	in := f.submitInput(pf.ID, "", "PING AAAA")
	if _, err := f.svc.SubmitJob(context.Background(), in); !errors.Is(err, ErrInvalidIdempotencyKey) {
		t.Fatalf("空键错误 = %v，期望 ErrInvalidIdempotencyKey", err)
	}

	in = f.submitInput(pf.ID, strings.Repeat("k", MaxIdempotencyKeyLength+1), "PING AAAA")
	if _, err := f.svc.SubmitJob(context.Background(), in); !errors.Is(err, ErrInvalidIdempotencyKey) {
		t.Fatalf("超长键错误 = %v，期望 ErrInvalidIdempotencyKey", err)
	}

	in = f.submitInput(pf.ID, "bad\x00key", "PING AAAA")
	if _, err := f.svc.SubmitJob(context.Background(), in); !errors.Is(err, ErrInvalidIdempotencyKey) {
		t.Fatalf("控制字符错误 = %v，期望 ErrInvalidIdempotencyKey", err)
	}
	if got := len(f.agent.opCalls); got != 0 {
		t.Fatalf("非法键仍然提交了 %d 次", got)
	}
}

// TestJobEventsRecordTransitions 任务流水按 seq 递增、状态与实际一致。
func TestJobEventsRecordTransitions(t *testing.T) {
	f := newJobFixture(t, nil)
	pf := f.preflight(7)
	f.agent.op.Stage = "succeeded"
	f.agent.op.StartedAt = "2026-10-20T09:00:01Z"
	f.agent.op.FinishedAt = "2026-10-20T09:00:03Z"
	job := f.mustSubmit(f.submitInput(pf.ID, "key-1", "PING AAAA"))

	rows, err := f.db.QueryContext(context.Background(),
		`SELECT seq, kind, redacted_payload FROM job_events WHERE job_id = ? ORDER BY seq`, job.ID)
	if err != nil {
		t.Fatalf("读事件失败: %v", err)
	}
	defer rows.Close()
	var seqs []int
	var payloads []string
	for rows.Next() {
		var seq int
		var kind, payload string
		if err := rows.Scan(&seq, &kind, &payload); err != nil {
			t.Fatalf("读事件行失败: %v", err)
		}
		seqs = append(seqs, seq)
		payloads = append(payloads, payload)
		if kind != "status" {
			t.Fatalf("P03.2 只写 status 事件，实际 %q", kind)
		}
	}
	if len(seqs) != 2 || seqs[0] != 1 || seqs[1] != 2 {
		t.Fatalf("事件 seq = %v，期望 [1 2]（queued → succeeded）", seqs)
	}
	if !strings.Contains(payloads[0], "queued") || !strings.Contains(payloads[1], "succeeded") {
		t.Fatalf("事件内容 = %v", payloads)
	}
	// 终态不能被后来的对账拉回运行中。
	if _, err := f.db.ExecContext(context.Background(),
		`UPDATE jobs SET status = 'running' WHERE id = ?`, job.ID); err == nil {
		t.Fatal("终态任务竟然被改回 running")
	}
}
