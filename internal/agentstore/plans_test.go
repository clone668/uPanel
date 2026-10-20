package agentstore

import (
	"context"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"
)

// newTestPlans 构造一个带真实数据库的预检计划仓储，并把数据库句柄一并返回以便直接查库。
func newTestPlans(t *testing.T) (*PreflightPlans, *DB) {
	t.Helper()
	db := openTestDB(t)
	plans, err := NewPreflightPlans(db)
	if err != nil {
		t.Fatalf("构造预检计划仓储失败: %v", err)
	}
	return plans, db
}

// planRefFor 造一个固定内容、形态合法的计划引用（pf_ + 32 位小写十六进制）。
func planRefFor(seed string) string {
	var buf [16]byte
	copy(buf[:], seed)
	return PlanRefPrefix + hex.EncodeToString(buf[:])
}

// hex64 把任意短字符串摊成一个 64 位十六进制摘要（仅用于测试数据）。
func hex64(seed string) string {
	var b strings.Builder
	for b.Len() < 64 {
		b.WriteString(seed)
	}
	out := b.String()
	return hex.EncodeToString([]byte(out[:32]))
}

// samplePlan 返回一份自洽的计划。调用方按需改动字段制造反例。
func samplePlan(t *testing.T) PreflightPlan {
	t.Helper()
	now := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	return PreflightPlan{
		ID:                  planRefFor("sample"),
		Action:              "storage.filesystem.format",
		Params:              []byte(`{"device":"/dev/sdb","label":null}`),
		ParamsHash:          hex64("params"),
		Target:              []byte(`{"display_name":"/dev/sdb","identity_fingerprint":"abc","state_version":"7"}`),
		IdentityFingerprint: "abc",
		StateVersion:        "7",
		Allowed:             true,
		Blockers:            []byte(`[]`),
		Warnings:            []byte(`[{"code":"data_loss","message":"目标上的数据会全部丢失"}]`),
		Impacts:             []byte(`[{"kind":"data_loss","description":"分区表被重写"}]`),
		Locks:               []byte(`["device:/dev/sdb"]`),
		RequiresReauth:      true,
		PhraseHash:          hex64("phrase"),
		UserID:              7,
		SessionHash:         hex64("session"),
		RequestID:           "rid-plan",
		CreatedAt:           now,
		ExpiresAt:           now.Add(120 * time.Second),
	}
}

func TestPreflightPlanRoundTrip(t *testing.T) {
	plans, _ := newTestPlans(t)
	ctx := context.Background()
	plan := samplePlan(t)

	if err := plans.Insert(ctx, plan); err != nil {
		t.Fatalf("保存计划失败: %v", err)
	}
	got, err := plans.Get(ctx, plan.ID)
	if err != nil {
		t.Fatalf("读取计划失败: %v", err)
	}
	// 参数与列表必须逐字节一致：参数哈希按字节算，任何重新编码都可能改变它。
	if string(got.Params) != string(plan.Params) {
		t.Fatalf("参数被改写: %s", got.Params)
	}
	if string(got.Target) != string(plan.Target) {
		t.Fatalf("目标被改写: %s", got.Target)
	}
	if string(got.Warnings) != string(plan.Warnings) || string(got.Impacts) != string(plan.Impacts) ||
		string(got.Blockers) != string(plan.Blockers) || string(got.Locks) != string(plan.Locks) {
		t.Fatal("列表字段没有原样读回")
	}
	if got.Action != plan.Action || got.ParamsHash != plan.ParamsHash || got.PhraseHash != plan.PhraseHash {
		t.Fatalf("标量字段不一致: %+v", got)
	}
	if got.IdentityFingerprint != plan.IdentityFingerprint || got.StateVersion != plan.StateVersion {
		t.Fatal("目标身份指纹或状态版本没有读回")
	}
	if !got.Allowed || !got.RequiresReauth {
		t.Fatal("布尔字段没有读回")
	}
	if got.UserID != plan.UserID || got.SessionHash != plan.SessionHash || got.RequestID != plan.RequestID {
		t.Fatalf("绑定字段不一致: user=%d session=%s rid=%s", got.UserID, got.SessionHash, got.RequestID)
	}
	if !got.CreatedAt.Equal(plan.CreatedAt) || !got.ExpiresAt.Equal(plan.ExpiresAt) {
		t.Fatalf("时间不一致: %s → %s", got.CreatedAt, got.ExpiresAt)
	}
}

func TestPreflightPlanStartsUnused(t *testing.T) {
	plans, db := newTestPlans(t)
	ctx := context.Background()
	plan := samplePlan(t)
	if err := plans.Insert(ctx, plan); err != nil {
		t.Fatalf("保存计划失败: %v", err)
	}
	var usedAt int64
	var usedOpID string
	err := db.store().SQL().QueryRowContext(ctx,
		`SELECT used_at, used_operation_id FROM preflight_plans WHERE id = ?`, plan.ID).Scan(&usedAt, &usedOpID)
	if err != nil {
		t.Fatalf("读取使用状态失败: %v", err)
	}
	if usedAt != 0 || usedOpID != "" {
		t.Fatalf("新计划必须是未使用状态，实际 used_at=%d used_operation_id=%q", usedAt, usedOpID)
	}
}

func TestPreflightPlanRejectsUsedFlagReset(t *testing.T) {
	plans, db := newTestPlans(t)
	ctx := context.Background()
	plan := samplePlan(t)
	if err := plans.Insert(ctx, plan); err != nil {
		t.Fatalf("保存计划失败: %v", err)
	}
	if _, err := db.store().ExecContext(ctx,
		`UPDATE preflight_plans SET used_at = ?, used_operation_id = ? WHERE id = ?`,
		millis(plan.CreatedAt.Add(time.Second)), "op_1", plan.ID); err != nil {
		t.Fatalf("标记已使用失败: %v", err)
	}
	// 一次性是安全属性：数据库必须拒绝把它改回未使用。
	if _, err := db.store().ExecContext(ctx,
		`UPDATE preflight_plans SET used_at = 0 WHERE id = ?`, plan.ID); err == nil {
		t.Fatal("已使用的计划不能被重置为未使用")
	}
}

func TestPreflightPlanDBChecksBindings(t *testing.T) {
	plans, db := newTestPlans(t)
	ctx := context.Background()
	ctxPlan := samplePlan(t)
	if err := plans.Insert(ctx, ctxPlan); err != nil {
		t.Fatalf("保存计划失败: %v", err)
	}
	// 直接绕过仓储插入非法行：数据库层也必须挡住。
	cases := []struct {
		name string
		sql  string
		args []any
	}{
		{"user_id 为 0", `UPDATE preflight_plans SET user_id = 0 WHERE id = ?`, []any{ctxPlan.ID}},
		{"session_hash 长度", `UPDATE preflight_plans SET session_hash = 'abc' WHERE id = ?`, []any{ctxPlan.ID}},
		{"到期不晚于创建", `UPDATE preflight_plans SET expires_at = created_at WHERE id = ?`, []any{ctxPlan.ID}},
		{"allowed 越界", `UPDATE preflight_plans SET allowed = 2 WHERE id = ?`, []any{ctxPlan.ID}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := db.store().ExecContext(ctx, tc.sql, tc.args...); err == nil {
				t.Fatal("数据库必须拒绝这行更新")
			}
		})
	}
}

func TestPreflightPlanValidation(t *testing.T) {
	plans, _ := newTestPlans(t)
	ctx := context.Background()

	cases := []struct {
		name   string
		mutate func(*PreflightPlan)
	}{
		{"计划引用不合法", func(p *PreflightPlan) { p.ID = "pf_xyz" }},
		{"缺少动作名", func(p *PreflightPlan) { p.Action = "  " }},
		{"缺少参数", func(p *PreflightPlan) { p.Params = nil }},
		{"参数哈希不是摘要", func(p *PreflightPlan) { p.ParamsHash = "nothex" }},
		{"用户为 0", func(p *PreflightPlan) { p.UserID = 0 }},
		{"会话哈希长度不对", func(p *PreflightPlan) { p.SessionHash = "abc" }},
		{"会话哈希含大写", func(p *PreflightPlan) { p.SessionHash = strings.ToUpper(hex64("session")) }},
		{"短语哈希不是摘要", func(p *PreflightPlan) { p.PhraseHash = "zz" }},
		{"缺少创建时间", func(p *PreflightPlan) { p.CreatedAt = time.Time{} }},
		{"到期不晚于创建", func(p *PreflightPlan) { p.ExpiresAt = p.CreatedAt }},
		{"拒绝却没有阻塞项", func(p *PreflightPlan) {
			p.Allowed = false
			p.Blockers = []byte(`[]`)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan := samplePlan(t)
			tc.mutate(&plan)
			err := plans.Insert(ctx, plan)
			if !errors.Is(err, ErrPlanInvalid) {
				t.Fatalf("错误 = %v，期望 ErrPlanInvalid", err)
			}
		})
	}
}

func TestPreflightPlanDuplicateRejected(t *testing.T) {
	plans, _ := newTestPlans(t)
	ctx := context.Background()
	plan := samplePlan(t)
	if err := plans.Insert(ctx, plan); err != nil {
		t.Fatalf("保存计划失败: %v", err)
	}
	if err := plans.Insert(ctx, plan); !errors.Is(err, ErrPlanInvalid) {
		t.Fatalf("重复引用必须被拒绝，实际错误 %v", err)
	}
}

func TestGetPreflightPlanMissing(t *testing.T) {
	plans, _ := newTestPlans(t)
	ctx := context.Background()
	for _, ref := range []string{
		"",
		"pf_",
		"pf_0123456789abcdef0123456789abcdef",
		"pf_0123456789ABCDEF0123456789ABCDEF",
		PlanRefPrefix + strings.Repeat("z", 32),
	} {
		if _, err := plans.Get(ctx, ref); !errors.Is(err, ErrPlanNotFound) {
			t.Fatalf("%q: 错误 = %v，期望 ErrPlanNotFound", ref, err)
		}
	}
}

func TestDeleteExpiredKeepsUsedPlans(t *testing.T) {
	plans, db := newTestPlans(t)
	ctx := context.Background()

	expiredUnused := samplePlan(t)
	expiredUnused.ID = planRefFor("expired-unused")
	expiredUsed := samplePlan(t)
	expiredUsed.ID = planRefFor("expired-used")

	stillValid := samplePlan(t)
	stillValid.ID = planRefFor("still-valid")
	stillValid.CreatedAt = time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	stillValid.ExpiresAt = stillValid.CreatedAt.Add(120 * time.Second)

	for _, plan := range []PreflightPlan{expiredUnused, expiredUsed, stillValid} {
		if err := plans.Insert(ctx, plan); err != nil {
			t.Fatalf("保存计划失败: %v", err)
		}
	}
	if _, err := db.store().ExecContext(ctx,
		`UPDATE preflight_plans SET used_at = ?, used_operation_id = 'op_x' WHERE id = ?`,
		millis(expiredUsed.CreatedAt), expiredUsed.ID); err != nil {
		t.Fatalf("标记已使用失败: %v", err)
	}

	// 清理时间取在“样本计划已过期、stillValid 还没过期”之间。
	removed, err := plans.DeleteExpired(ctx, time.Date(2026, 10, 20, 9, 30, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("清理过期计划失败: %v", err)
	}
	if removed != 1 {
		t.Fatalf("删除条数 = %d，期望 1（只删未使用的过期计划）", removed)
	}
	if _, err := plans.Get(ctx, expiredUnused.ID); !errors.Is(err, ErrPlanNotFound) {
		t.Fatal("未使用的过期计划应被删除")
	}
	if _, err := plans.Get(ctx, expiredUsed.ID); err != nil {
		t.Fatalf("已使用的计划属于执行历史，不应被清理: %v", err)
	}
	if _, err := plans.Get(ctx, stillValid.ID); err != nil {
		t.Fatalf("未过期的计划不应被清理: %v", err)
	}
}

func TestNewPreflightPlansRequiresDB(t *testing.T) {
	if _, err := NewPreflightPlans(nil); err == nil {
		t.Fatal("缺少数据库句柄必须报错")
	}
}

func TestValidPlanRef(t *testing.T) {
	good := planRefFor("0123456789abcdef")
	if !ValidPlanRef(good) {
		t.Fatalf("%q 应是合法引用", good)
	}
	bad := []string{
		"",
		"pf_",
		"pf_0123456789abcdef",                  // 太短
		good + "0",                             // 太长
		"PF_0123456789abcdef0123456789abcdef",  // 前缀大写
		"pf_0123456789ABCDEF0123456789ABCDEF",  // 正文大写
		"sec_0123456789abcdef0123456789abcdef", // 前缀不对
		"pf_" + strings.Repeat("g", 32),        // 非十六进制
		"pf_0123456789abcdef0123456789abcde\n", // 尾随换行
	}
	for _, ref := range bad {
		if ValidPlanRef(ref) {
			t.Fatalf("%q 不应是合法引用", ref)
		}
	}
}
