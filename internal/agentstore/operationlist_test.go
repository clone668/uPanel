package agentstore

// 按操作者列操作（P12.4 的定时操作对账）。
//
// 这个查询存在的唯一理由：定时操作是代理自己产生的，Web 只能反过来问"这段时间
// 你替我跑了什么"。所以断言集中在"问答的边界"上：只给这个操作者的、按时间升序、
// 从 since 起（含）、limit 有上限、参数不对就报错而不是给一份空列表。

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// insertPlanFor 写入一份指定操作者与时间的预检计划（insertPlan 用的是 time.Now）。
func insertPlanFor(t *testing.T, plans *PreflightPlans, seed string, userID int64, at time.Time) PreflightPlan {
	t.Helper()
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
		UserID:              userID,
		SessionHash:         hex64("session-" + seed),
		RequestID:           "rid-plan",
		CreatedAt:           at,
		ExpiresAt:           at.Add(120 * time.Second),
	}
	if err := plans.Insert(context.Background(), plan); err != nil {
		t.Fatalf("写入预检计划失败: %v", err)
	}
	return plan
}

// sampleOperationAt 由一份预检计划造出自洽的操作记录（时间由调用方指定）。
func sampleOperationAt(t *testing.T, plan PreflightPlan, operationID, seed string, at time.Time) Operation {
	t.Helper()
	return Operation{
		ID:                  operationID,
		RequestHash:         hex64("request-" + seed),
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
		CreatedAt:           at,
		UpdatedAt:           at,
	}
}

func TestListByActorFiltersAndOrders(t *testing.T) {
	ops, plans, _ := newTestOperations(t)
	ctx := context.Background()
	base := time.Date(2026, 10, 20, 3, 0, 0, 0, time.UTC)

	// 三条同一操作者的记录（时间乱序写入），一条别人的。
	mk := func(planSeed, opSeed string, userID int64, at time.Time) Operation {
		plan := insertPlanFor(t, plans, planSeed, userID, at)
		op := sampleOperationAt(t, plan, opIdFor(opSeed), opSeed, at)
		if _, _, err := ops.Accept(ctx, op); err != nil {
			t.Fatalf("接受操作失败: %v", err)
		}
		return op
	}
	third := mk("list-c", "list-c", 99, base.Add(3*time.Hour))
	first := mk("list-a", "list-a", 99, base.Add(time.Hour))
	second := mk("list-b", "list-b", 99, base.Add(2*time.Hour))
	mk("list-x", "list-x", 7, base.Add(time.Hour))

	got, err := ops.ListByActor(ctx, 99, time.Time{}, "", 10)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("返回 %d 条，期望 3（不含别的操作者）", len(got))
	}
	want := []string{first.ID, second.ID, third.ID}
	for i, id := range want {
		if got[i].ID != id {
			t.Fatalf("第 %d 条 = %s，期望 %s（必须按创建时间升序）", i, got[i].ID, id)
		}
	}

	// since 是"含"：正好等于第二条的时间，就该从第二条开始。
	got, err = ops.ListByActor(ctx, 99, base.Add(2*time.Hour), "", 10)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if len(got) != 2 || got[0].ID != second.ID {
		t.Fatalf("since 过滤不对：%+v", got)
	}

	// limit 按升序截前面的，不是"最新的几条"：调用方靠它往前推水位。
	got, err = ops.ListByActor(ctx, 99, time.Time{}, "", 2)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if len(got) != 2 || got[1].ID != second.ID {
		t.Fatalf("limit 截断不对：%+v", got)
	}
}

func TestListByActorCompoundCursorDoesNotRepeatSameTimestampPage(t *testing.T) {
	ops, plans, _ := newTestOperations(t)
	ctx := context.Background()
	at := time.Date(2026, 10, 20, 3, 0, 0, 0, time.UTC)
	for _, id := range []string{
		"op_" + "00000000000000000000000000000003",
		"op_" + "00000000000000000000000000000001",
		"op_" + "00000000000000000000000000000002",
	} {
		plan := insertPlanFor(t, plans, "cursor-"+id[len(id)-2:], 99, at)
		op := sampleOperationAt(t, plan, id, id, at)
		if _, _, err := ops.Accept(ctx, op); err != nil {
			t.Fatalf("接受操作失败: %v", err)
		}
	}

	first, err := ops.ListByActor(ctx, 99, at, "", 2)
	if err != nil {
		t.Fatalf("读取第一页失败: %v", err)
	}
	if len(first) != 2 || !strings.HasSuffix(first[0].ID, "01") || !strings.HasSuffix(first[1].ID, "02") {
		t.Fatalf("第一页顺序不对: %+v", first)
	}
	second, err := ops.ListByActor(ctx, 99, at, first[1].ID, 2)
	if err != nil {
		t.Fatalf("读取第二页失败: %v", err)
	}
	if len(second) != 1 || !strings.HasSuffix(second[0].ID, "03") {
		t.Fatalf("复合游标漏掉同时间戳的下一页: %+v", second)
	}
}

func TestListByActorRejectsBadParams(t *testing.T) {
	ops, _, _ := newTestOperations(t)
	ctx := context.Background()

	if _, err := ops.ListByActor(ctx, 0, time.Time{}, "", 10); !errors.Is(err, ErrOperationInvalid) {
		t.Fatalf("user_id=0 错误 = %v，期望 ErrOperationInvalid", err)
	}
	if _, err := ops.ListByActor(ctx, 99, time.Time{}, "", 0); !errors.Is(err, ErrOperationInvalid) {
		t.Fatalf("limit=0 错误 = %v，期望 ErrOperationInvalid", err)
	}

	// 超过上限是 clamp，不是错误——但要真的不超过上限。
	got, err := ops.ListByActor(ctx, 99, time.Time{}, "", MaxActorsBatch+50)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("空库应当返回空列表，实际 %d 条", len(got))
	}
}
