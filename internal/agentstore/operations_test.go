package agentstore

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"testing"
	"time"
)

// newTestOperations 构造一个带真实数据库的操作仓储，并把数据库句柄一并返回以便直接查库。
func newTestOperations(t *testing.T) (*Operations, *PreflightPlans, *DB) {
	t.Helper()
	db := openTestDB(t)
	ops, err := NewOperations(db)
	if err != nil {
		t.Fatalf("构造操作仓储失败: %v", err)
	}
	plans, err := NewPreflightPlans(db)
	if err != nil {
		t.Fatalf("构造预检仓储失败: %v", err)
	}
	return ops, plans, db
}

// insertPlan 写入一份仍有效的预检计划并返回它。
func insertPlan(t *testing.T, plans *PreflightPlans, seed string) PreflightPlan {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	plan := PreflightPlan{
		ID:                  planRefFor(seed),
		Action:              "acceptance.ping",
		Params:              []byte(`{"note":"` + seed + `"}`),
		ParamsHash:          hex64("params-" + seed),
		Target:              []byte(`{"display_name":"target","identity_fingerprint":"fp","state_version":"1"}`),
		IdentityFingerprint: "fp",
		StateVersion:        "1",
		Allowed:             true,
		Blockers:            []byte(`[]`),
		Warnings:            []byte(`[]`),
		Impacts:             []byte(`[]`),
		Locks:               []byte(`["acceptance:ping"]`),
		UserID:              7,
		SessionHash:         hex64("session"),
		RequestID:           "rid-plan",
		CreatedAt:           now,
		ExpiresAt:           now.Add(120 * time.Second),
	}
	if err := plans.Insert(context.Background(), plan); err != nil {
		t.Fatalf("写入预检计划失败: %v", err)
	}
	return plan
}

// opIdFor 造一个形态合法的操作标识（op_ + 16 字节十六进制）。
func opIdFor(seed string) string {
	var buf [16]byte
	copy(buf[:], seed)
	return OperationIDPrefix + hex.EncodeToString(buf[:])
}

// sampleOperation 由一份预检计划造出自洽的操作记录。
func sampleOperation(t *testing.T, plan PreflightPlan, operationID, requestSeed string) Operation {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	return Operation{
		ID:                  operationID,
		RequestHash:         hex64(requestSeed),
		Action:              plan.Action,
		PreflightRef:        plan.ID,
		Params:              plan.Params,
		ParamsHash:          plan.ParamsHash,
		IdentityFingerprint: plan.IdentityFingerprint,
		StateVersion:        plan.StateVersion,
		Target:              plan.Target,
		Locks:               plan.Locks,
		UserID:              plan.UserID,
		SessionHash:         plan.SessionHash,
		RequestID:           "rid-op",
		CreatedAt:           now,
		UpdatedAt:           now,
	}
}

// planUsage 直接查库读回预检的使用状态（用来证明“一次性”落在数据上）。
func planUsage(t *testing.T, db *DB, ref string) (usedAt int64, usedOp string) {
	t.Helper()
	row := db.store().SQL().QueryRow(`SELECT used_at, used_operation_id FROM preflight_plans WHERE id = ?`, ref)
	if err := row.Scan(&usedAt, &usedOp); err != nil {
		t.Fatalf("读取预检使用状态失败: %v", err)
	}
	return usedAt, usedOp
}

func TestAcceptConsumesPreflightExactlyOnce(t *testing.T) {
	ops, plans, db := newTestOperations(t)
	plan := insertPlan(t, plans, "once")
	ctx := context.Background()

	accepted, duplicate, err := ops.Accept(ctx, sampleOperation(t, plan, opIdFor("op1"), "req-1"))
	if err != nil {
		t.Fatalf("接受操作失败: %v", err)
	}
	if duplicate {
		t.Fatal("首次接受不应被标记为重放")
	}
	if accepted.Stage != StageAccepted {
		t.Fatalf("阶段 = %q，期望 %q", accepted.Stage, StageAccepted)
	}
	usedAt, usedOp := planUsage(t, db, plan.ID)
	if usedAt == 0 || usedOp != opIdFor("op1") {
		t.Fatalf("预检未被标记使用: used_at=%d used_operation_id=%q", usedAt, usedOp)
	}

	// 同一份预检换一个 operation_id 再来：必须失败，且不产生第二条记录。
	_, _, err = ops.Accept(ctx, sampleOperation(t, plan, opIdFor("op2"), "req-1"))
	if !errors.Is(err, ErrPreflightExpired) {
		t.Fatalf("重复消费预检的错误 = %v，期望 ErrPreflightExpired", err)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM operations`); n != 1 {
		t.Fatalf("操作记录数 = %d，期望 1", n)
	}
}

func TestAcceptReplayReturnsExistingOperation(t *testing.T) {
	ops, plans, db := newTestOperations(t)
	plan := insertPlan(t, plans, "replay")
	ctx := context.Background()
	op := sampleOperation(t, plan, opIdFor("replay"), "req-same")

	if _, _, err := ops.Accept(ctx, op); err != nil {
		t.Fatalf("首次接受失败: %v", err)
	}
	// 提交十次（这里循环九次）：每一次都拿回同一条记录，且不再写事件。
	for i := 0; i < 9; i++ {
		existing, duplicate, err := ops.Accept(ctx, op)
		if err != nil {
			t.Fatalf("第 %d 次重放失败: %v", i+2, err)
		}
		if !duplicate {
			t.Fatalf("第 %d 次重放未被识别为重放", i+2)
		}
		if existing.ID != op.ID {
			t.Fatalf("重放返回的操作 = %q，期望 %q", existing.ID, op.ID)
		}
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM operations`); n != 1 {
		t.Fatalf("操作记录数 = %d，期望 1", n)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM operation_events`); n != 1 {
		t.Fatalf("事件数 = %d，期望 1（重放不得追加事件）", n)
	}
}

func TestAcceptRejectsSameIDWithDifferentRequest(t *testing.T) {
	ops, plans, _ := newTestOperations(t)
	plan := insertPlan(t, plans, "conflict")
	ctx := context.Background()

	if _, _, err := ops.Accept(ctx, sampleOperation(t, plan, opIdFor("conflict"), "req-a")); err != nil {
		t.Fatalf("首次接受失败: %v", err)
	}
	other := sampleOperation(t, plan, opIdFor("conflict"), "req-b")
	if _, _, err := ops.Accept(ctx, other); !errors.Is(err, ErrOperationConflict) {
		t.Fatalf("同 ID 不同请求的错误 = %v，期望 ErrOperationConflict", err)
	}
}

func TestAcceptRejectsExpiredPreflight(t *testing.T) {
	ops, plans, _ := newTestOperations(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	plan := PreflightPlan{
		ID:                  planRefFor("expired"),
		Action:              "acceptance.ping",
		Params:              []byte(`{"note":"x"}`),
		ParamsHash:          hex64("params-expired"),
		Target:              []byte(`{}`),
		IdentityFingerprint: "fp",
		StateVersion:        "1",
		Allowed:             true,
		Blockers:            []byte(`[]`),
		Locks:               []byte(`[]`),
		UserID:              7,
		SessionHash:         hex64("session"),
		CreatedAt:           now.Add(-3 * time.Minute),
		ExpiresAt:           now.Add(-time.Minute),
	}
	if err := plans.Insert(ctx, plan); err != nil {
		t.Fatalf("写入过期预检失败: %v", err)
	}
	if _, _, err := ops.Accept(ctx, sampleOperation(t, plan, opIdFor("expired"), "req")); !errors.Is(err, ErrPreflightExpired) {
		t.Fatalf("消费过期预检的错误 = %v，期望 ErrPreflightExpired", err)
	}
}

func TestOperationLifecycleWritesEvents(t *testing.T) {
	ops, plans, db := newTestOperations(t)
	plan := insertPlan(t, plans, "life")
	ctx := context.Background()
	op := sampleOperation(t, plan, opIdFor("life"), "req-life")
	if _, _, err := ops.Accept(ctx, op); err != nil {
		t.Fatalf("接受失败: %v", err)
	}

	now := time.Now().UTC().Truncate(time.Second)
	if err := ops.MarkRunning(ctx, op.ID, "executing", now); err != nil {
		t.Fatalf("推进到 running 失败: %v", err)
	}
	// 幂等：再推进一次不报错，也不重复写事件。
	if err := ops.MarkRunning(ctx, op.ID, "executing", now); err != nil {
		t.Fatalf("重复推进到 running 失败: %v", err)
	}
	if err := ops.Finish(ctx, op.ID, StageSucceeded, "done", []byte(`{"outcome":"verified"}`), nil, now); err != nil {
		t.Fatalf("写入成功终态失败: %v", err)
	}
	// 重复写同一个终态是幂等的。
	if err := ops.Finish(ctx, op.ID, StageSucceeded, "done", []byte(`{"outcome":"verified"}`), nil, now); err != nil {
		t.Fatalf("重复写入成功终态失败: %v", err)
	}
	// 终态不能改成另一个终态。
	if err := ops.Finish(ctx, op.ID, StageFailed, "late", nil, nil, now); !errors.Is(err, ErrOperationFinished) {
		t.Fatalf("改写终态的错误 = %v，期望 ErrOperationFinished", err)
	}

	got, err := ops.Get(ctx, op.ID)
	if err != nil {
		t.Fatalf("读回操作失败: %v", err)
	}
	if got.Stage != StageSucceeded || got.FinishedAt.IsZero() {
		t.Fatalf("终态 = %q finished_at=%v", got.Stage, got.FinishedAt)
	}
	if string(got.Result) != `{"outcome":"verified"}` {
		t.Fatalf("结果 = %s", got.Result)
	}

	events, err := ops.Events(ctx, op.ID)
	if err != nil {
		t.Fatalf("读取事件失败: %v", err)
	}
	kinds := make([]string, 0, len(events))
	for _, ev := range events {
		kinds = append(kinds, ev.Kind)
	}
	want := []string{"accepted", "started", "finished"}
	if len(kinds) != len(want) {
		t.Fatalf("事件 = %v，期望 %v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("事件 = %v，期望 %v", kinds, want)
		}
	}
	for i, ev := range events {
		if ev.Seq != int64(i+1) {
			t.Fatalf("第 %d 条事件 seq = %d，期望 %d", i, ev.Seq, i+1)
		}
	}
	_ = db
}

func TestOperationTerminalStageIsFinalInDatabase(t *testing.T) {
	ops, plans, db := newTestOperations(t)
	plan := insertPlan(t, plans, "final")
	ctx := context.Background()
	op := sampleOperation(t, plan, opIdFor("final"), "req-final")
	if _, _, err := ops.Accept(ctx, op); err != nil {
		t.Fatalf("接受失败: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	if err := ops.Finish(ctx, op.ID, StageCanceled, "canceled", nil, nil, now); err != nil {
		t.Fatalf("写入取消终态失败: %v", err)
	}
	// 绕过仓储直接改库：触发器必须拦住“终态回到运行中”。
	_, err := db.store().SQL().Exec(`UPDATE operations SET stage = 'running' WHERE operation_id = ?`, op.ID)
	if err == nil {
		t.Fatal("终态被直接改回 running，触发器没有生效")
	}
}

func TestRequestCancelSemantics(t *testing.T) {
	ops, plans, _ := newTestOperations(t)
	ctx := context.Background()

	// accepted → canceled：还没开始，直接结束。
	planA := insertPlan(t, plans, "cancel-a")
	opA := sampleOperation(t, planA, opIdFor("cancel-a"), "req-a")
	if _, _, err := ops.Accept(ctx, opA); err != nil {
		t.Fatalf("接受失败: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	got, err := ops.RequestCancel(ctx, opA.ID, now)
	if err != nil {
		t.Fatalf("取消 accepted 操作失败: %v", err)
	}
	if got.Stage != StageCanceled || !got.CancellationRequested || got.FinishedAt.IsZero() {
		t.Fatalf("取消后 stage=%q requested=%v finished=%v", got.Stage, got.CancellationRequested, got.FinishedAt)
	}

	// running → 只置 cancellation_requested，阶段仍是 running。
	planB := insertPlan(t, plans, "cancel-b")
	opB := sampleOperation(t, planB, opIdFor("cancel-b"), "req-b")
	if _, _, err := ops.Accept(ctx, opB); err != nil {
		t.Fatalf("接受失败: %v", err)
	}
	if err := ops.MarkRunning(ctx, opB.ID, "executing", now); err != nil {
		t.Fatalf("推进失败: %v", err)
	}
	gotB, err := ops.RequestCancel(ctx, opB.ID, now)
	if err != nil {
		t.Fatalf("取消 running 操作失败: %v", err)
	}
	if gotB.Stage != StageRunning || !gotB.CancellationRequested {
		t.Fatalf("running 取消后 stage=%q requested=%v", gotB.Stage, gotB.CancellationRequested)
	}

	// 终态再取消：拒绝。
	if _, err := ops.RequestCancel(ctx, opA.ID, now); !errors.Is(err, ErrOperationFinished) {
		t.Fatalf("取消终态操作的错误 = %v，期望 ErrOperationFinished", err)
	}
}

func TestOperationEventsAreAppendOnly(t *testing.T) {
	ops, plans, db := newTestOperations(t)
	plan := insertPlan(t, plans, "events")
	op := sampleOperation(t, plan, opIdFor("events"), "req-events")
	if _, _, err := ops.Accept(context.Background(), op); err != nil {
		t.Fatalf("接受失败: %v", err)
	}
	if _, err := db.store().SQL().Exec(`UPDATE operation_events SET payload_json = '{}'`); err == nil {
		t.Fatal("事件被更新，只追加触发器没有生效")
	}
	if _, err := db.store().SQL().Exec(`DELETE FROM operation_events`); err == nil {
		t.Fatal("事件被删除，只追加触发器没有生效")
	}
}

func TestOperationValidationRejectsBadRecords(t *testing.T) {
	ops, plans, _ := newTestOperations(t)
	plan := insertPlan(t, plans, "valid")
	ctx := context.Background()
	base := sampleOperation(t, plan, opIdFor("valid"), "req-valid")

	cases := map[string]func(o *Operation){
		"operation_id":  func(o *Operation) { o.ID = "not-an-id" },
		"request_hash":  func(o *Operation) { o.RequestHash = "short" },
		"action":        func(o *Operation) { o.Action = "" },
		"preflight_ref": func(o *Operation) { o.PreflightRef = "pf_bad" },
		"params":        func(o *Operation) { o.Params = nil },
		"params_hash":   func(o *Operation) { o.ParamsHash = hex64("x")[:10] },
		"user_id":       func(o *Operation) { o.UserID = 0 },
		"session_hash":  func(o *Operation) { o.SessionHash = "abc" },
		"stage":         func(o *Operation) { o.Stage = "wat" },
		"created_at":    func(o *Operation) { o.CreatedAt = time.Time{} },
	}
	for name, mutate := range cases {
		op := base
		mutate(&op)
		if _, _, err := ops.Accept(ctx, op); !errors.Is(err, ErrOperationInvalid) {
			t.Fatalf("%s: 错误 = %v，期望 ErrOperationInvalid", name, err)
		}
	}
}

func TestGetMissingOperation(t *testing.T) {
	ops, _, _ := newTestOperations(t)
	if _, err := ops.Get(context.Background(), opIdFor("nope")); !errors.Is(err, ErrOperationNotFound) {
		t.Fatalf("错误 = %v，期望 ErrOperationNotFound", err)
	}
	if _, err := ops.Get(context.Background(), "op_invalid"); !errors.Is(err, ErrOperationNotFound) {
		t.Fatalf("非法 ID 的错误 = %v，期望 ErrOperationNotFound", err)
	}
}

func TestListUnfinished(t *testing.T) {
	ops, plans, _ := newTestOperations(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	pending := insertPlan(t, plans, "u-pending")
	done := insertPlan(t, plans, "u-done")
	opPending := sampleOperation(t, pending, opIdFor("u-pending"), "req-p")
	opDone := sampleOperation(t, done, opIdFor("u-done"), "req-d")
	for _, op := range []Operation{opPending, opDone} {
		if _, _, err := ops.Accept(ctx, op); err != nil {
			t.Fatalf("接受失败: %v", err)
		}
	}
	if err := ops.Finish(ctx, opDone.ID, StageSucceeded, "done", nil, nil, now); err != nil {
		t.Fatalf("写入终态失败: %v", err)
	}

	unfinished, err := ops.ListUnfinished(ctx)
	if err != nil {
		t.Fatalf("读取未完成操作失败: %v", err)
	}
	if len(unfinished) != 1 || unfinished[0].ID != opPending.ID {
		t.Fatalf("未完成操作 = %+v，期望只有 %s", unfinished, opPending.ID)
	}
}

func TestNewOperationsRequiresDB(t *testing.T) {
	if _, err := NewOperations(nil); err == nil {
		t.Fatal("缺少数据库句柄时应当返回错误")
	}
}

func TestValidOperationID(t *testing.T) {
	good := opIdFor("x")
	if !ValidOperationID(good) {
		t.Fatalf("%q 应当合法", good)
	}
	for _, bad := range []string{"", "op_", "op_xyz", "pf_" + good[3:], good + "0", "OP_" + good[3:] + "A"} {
		if ValidOperationID(bad) {
			t.Fatalf("%q 不应当合法", bad)
		}
	}
}

// countRows 执行一条计数查询；出错直接失败（测试辅助）。
func countRows(t *testing.T, db *DB, query string) int {
	t.Helper()
	var n int
	if err := db.store().SQL().QueryRow(query).Scan(&n); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0
		}
		t.Fatalf("计数查询失败 (%s): %v", query, err)
	}
	return n
}
