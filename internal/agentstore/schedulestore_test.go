package agentstore

// 计划任务与受管理脚本仓储的测试（P12.4，CONTRACTS.md 12 / 14.2）。
//
// 重点同样不在 CRUD 形状，而在五条不变量：
//  1. 定义原样往返——它决定"到点了要跑哪个动作"；
//  2. 名字与目标各自不撞车（唯一索引是最后一道，不能只靠应用层）；
//  3. 版本冲突与"没有这条记录"必须分得开（409 与 404 是两件事）；
//  4. 同一个槽里的第二次触发插不进去，且能把已有的 operation_id 读回来——
//     这就是"稳定触发 id"；
//  5. 删计划不删触发记录（出事后第一个要查的就是它）。

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"upanel/internal/schedules"
	"upanel/internal/scripts"
)

func testScheduleDef(name string) schedules.Definition {
	return schedules.Definition{
		Name:               name,
		Kind:               schedules.KindBackup,
		TargetID:           "pol_0123456789abcdef0123456789abcdef",
		CalendarExpression: "*-*-* 03:30:00",
		Timezone:           "Asia/Shanghai",
		Persistent:         true,
		MaxRuntimeSeconds:  3600,
		Enabled:            true,
	}
}

func newTestSchedules(t *testing.T) *ScheduleStore {
	t.Helper()
	store, err := NewScheduleStore(openTestDB(t))
	if err != nil {
		t.Fatalf("构造计划仓储失败: %v", err)
	}
	return store
}

func TestNewScheduleStoreRejectsNilDB(t *testing.T) {
	if _, err := NewScheduleStore(nil); err == nil {
		t.Fatal("没有数据库句柄却构造成功了")
	}
}

// TestScheduleStoreRoundTrip：定义原样往返，单元引用与版本各自独立。
func TestScheduleStoreRoundTrip(t *testing.T) {
	store := newTestSchedules(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 25, 3, 0, 0, 0, time.UTC)

	rec, err := store.Create(ctx, testScheduleDef("每天备份"), now)
	if err != nil {
		t.Fatalf("新建计划失败: %v", err)
	}
	if !schedules.ValidID(rec.ID) {
		t.Fatalf("计划 id 形态不对: %q", rec.ID)
	}
	if rec.Version != 1 || rec.TimerRef != "" {
		t.Fatalf("新建后的版本/单元引用不对: version=%d ref=%q", rec.Version, rec.TimerRef)
	}

	got, found, err := store.Get(ctx, rec.ID)
	if err != nil || !found {
		t.Fatalf("读取计划失败: %v found=%v", err, found)
	}
	if got.Definition != testScheduleDef("每天备份") {
		t.Fatalf("定义没有原样回来: %#v", got.Definition)
	}
	if got.DefinitionKey == "" || got.DefinitionKey != rec.DefinitionKey {
		t.Fatalf("定义指纹不对: %q vs %q", got.DefinitionKey, rec.DefinitionKey)
	}

	// 单元引用单独写，不动版本。
	if _, err := store.SetTimerRef(ctx, rec.ID, "upanel-"+rec.ID+".timer", now); err != nil {
		t.Fatalf("写单元引用失败: %v", err)
	}
	got, _, _ = store.Get(ctx, rec.ID)
	if got.TimerRef != "upanel-"+rec.ID+".timer" {
		t.Fatalf("单元引用没写进去: %q", got.TimerRef)
	}
	if got.Version != 1 {
		t.Fatalf("写单元引用不该改版本，得到 %d", got.Version)
	}
}

// TestScheduleStoreRejectsDuplicateNameAndTarget：两个唯一约束各自有话说。
func TestScheduleStoreRejectsDuplicateNameAndTarget(t *testing.T) {
	store := newTestSchedules(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 25, 3, 0, 0, 0, time.UTC)
	if _, err := store.Create(ctx, testScheduleDef("每天备份"), now); err != nil {
		t.Fatalf("新建计划失败: %v", err)
	}

	if _, err := store.Create(ctx, testScheduleDef("每天备份"), now); !errors.Is(err, ErrScheduleName) {
		t.Fatalf("重名应当被拒绝，得到 %v", err)
	}

	other := testScheduleDef("另一条")
	other.Kind = schedules.KindUserScript
	other.TargetID = "scr_0123456789abcdef0123456789abcdef"
	if _, err := store.Create(ctx, other, now); err != nil {
		t.Fatalf("同一目标但不同种类应当允许: %v", err)
	}
	sameTarget := testScheduleDef("第三条")
	if _, err := store.Create(ctx, sameTarget, now); !errors.Is(err, ErrScheduleTarget) {
		t.Fatalf("同一备份策略上的第二条计划应当被拒绝，得到 %v", err)
	}
}

// TestScheduleStoreVersionConflicts：版本冲突与不存在分得开。
func TestScheduleStoreVersionConflicts(t *testing.T) {
	store := newTestSchedules(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 25, 3, 0, 0, 0, time.UTC)
	rec, err := store.Create(ctx, testScheduleDef("每天备份"), now)
	if err != nil {
		t.Fatalf("新建计划失败: %v", err)
	}

	updated := testScheduleDef("每天备份")
	updated.CalendarExpression = "*-*-* 04:00:00"
	got, err := store.Update(ctx, rec.ID, updated, 1, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("更新计划失败: %v", err)
	}
	if got.Version != 2 {
		t.Fatalf("更新后版本应当为 2，得到 %d", got.Version)
	}
	if _, err := store.Update(ctx, rec.ID, updated, 1, now); !errors.Is(err, ErrScheduleVersion) {
		t.Fatalf("旧版本更新应当报版本冲突，得到 %v", err)
	}
	if _, err := store.Update(ctx, "sch_ffffffffffffffffffffffffffffffff", updated, 1, now); !errors.Is(err, ErrScheduleNotFound) {
		t.Fatalf("不存在的计划应当报找不到，得到 %v", err)
	}
	if err := store.Delete(ctx, "sch_ffffffffffffffffffffffffffffffff", 1); err != nil {
		t.Fatalf("删除不存在的计划应当幂等返回 nil，得到 %v", err)
	}
	if err := store.Delete(ctx, rec.ID, 1); !errors.Is(err, ErrScheduleVersion) {
		t.Fatalf("旧版本删除应当报版本冲突，得到 %v", err)
	}
	if err := store.Delete(ctx, rec.ID, 2); err != nil {
		t.Fatalf("删除计划失败: %v", err)
	}
}

// TestScheduleTriggerSlotIsUnique 是"稳定触发 id"的落地验证。
func TestScheduleTriggerSlotIsUnique(t *testing.T) {
	store := newTestSchedules(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 25, 3, 30, 0, 0, time.UTC)
	rec, err := store.Create(ctx, testScheduleDef("每天备份"), now)
	if err != nil {
		t.Fatalf("新建计划失败: %v", err)
	}

	slot := schedules.SlotAt(now)
	triggerID := schedules.TriggerID(rec.ID, now)
	opID := schedules.OperationID(triggerID)
	first := TriggerRecord{
		TriggerID: triggerID, ScheduleID: rec.ID, SlotAt: slot,
		Action: "backup.run", OperationID: opID,
		Outcome: TriggerOutcomeTriggered, CreatedAt: now,
	}
	gotOp, duplicate, err := store.RecordTrigger(ctx, first)
	if err != nil {
		t.Fatalf("记录触发失败: %v", err)
	}
	if duplicate || gotOp != opID {
		t.Fatalf("第一次触发不该是重放: duplicate=%v op=%s", duplicate, gotOp)
	}

	// 同一分钟内再来一次（systemd 重试 / 手工 systemctl start）。
	second := first
	second.CreatedAt = now.Add(30 * time.Second)
	gotOp, duplicate, err = store.RecordTrigger(ctx, second)
	if err != nil {
		t.Fatalf("第二次触发记录失败: %v", err)
	}
	if !duplicate {
		t.Fatal("同一个槽的第二次触发应当被认成重放")
	}
	if gotOp != opID {
		t.Fatalf("重放应当返回原有的 operation_id，得到 %s", gotOp)
	}

	// 下一个槽是另一次触发。
	next := now.Add(time.Minute)
	nextTrigger := schedules.TriggerID(rec.ID, next)
	if nextTrigger == triggerID {
		t.Fatal("跨分钟应当得到不同的触发 id")
	}
	if _, duplicate, err := store.RecordTrigger(ctx, TriggerRecord{
		TriggerID: nextTrigger, ScheduleID: rec.ID, SlotAt: schedules.SlotAt(next),
		Action: "backup.run", OperationID: schedules.OperationID(nextTrigger),
		Outcome: TriggerOutcomeTriggered, CreatedAt: next,
	}); err != nil || duplicate {
		t.Fatalf("下一个槽应当是新的触发: err=%v duplicate=%v", err, duplicate)
	}

	list, err := store.ListTriggers(ctx, rec.ID, 10)
	if err != nil {
		t.Fatalf("读取触发记录失败: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("应当有两条触发记录，得到 %d", len(list))
	}
}

// TestScheduleDeleteKeepsTriggers：删计划不删触发记录。
func TestScheduleDeleteKeepsTriggers(t *testing.T) {
	store := newTestSchedules(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 25, 3, 30, 0, 0, time.UTC)
	rec, err := store.Create(ctx, testScheduleDef("每天备份"), now)
	if err != nil {
		t.Fatalf("新建计划失败: %v", err)
	}
	triggerID := schedules.TriggerID(rec.ID, now)
	if _, _, err := store.RecordTrigger(ctx, TriggerRecord{
		TriggerID: triggerID, ScheduleID: rec.ID, SlotAt: schedules.SlotAt(now),
		Action: "backup.run", OperationID: schedules.OperationID(triggerID),
		Outcome: TriggerOutcomeTriggered, CreatedAt: now,
	}); err != nil {
		t.Fatalf("记录触发失败: %v", err)
	}
	if err := store.Delete(ctx, rec.ID, 1); err != nil {
		t.Fatalf("删除计划失败: %v", err)
	}
	list, err := store.ListTriggers(ctx, rec.ID, 10)
	if err != nil {
		t.Fatalf("删除计划后读取触发记录失败: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("删计划后触发记录应当留着，得到 %d 条", len(list))
	}
}

// TestTriggerRecordsAreAppendOnly：改写历史会让"这一晚跑过没有"失去意义。
func TestTriggerRecordsAreAppendOnly(t *testing.T) {
	store := newTestSchedules(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 25, 3, 30, 0, 0, time.UTC)
	rec, err := store.Create(ctx, testScheduleDef("每天备份"), now)
	if err != nil {
		t.Fatalf("新建计划失败: %v", err)
	}
	triggerID := schedules.TriggerID(rec.ID, now)
	if _, _, err := store.RecordTrigger(ctx, TriggerRecord{
		TriggerID: triggerID, ScheduleID: rec.ID, SlotAt: schedules.SlotAt(now),
		Action: "backup.run", OperationID: schedules.OperationID(triggerID),
		Outcome: TriggerOutcomeTriggered, CreatedAt: now,
	}); err != nil {
		t.Fatalf("记录触发失败: %v", err)
	}
	if _, err := store.db.store().ExecContext(ctx,
		`UPDATE schedule_triggers SET outcome = 'failed' WHERE trigger_id = ?`, triggerID); err == nil {
		t.Fatal("触发记录应当只追加，不允许 UPDATE")
	}
}

// ---- 脚本仓储 ----

func newTestScripts(t *testing.T) *ScriptStore {
	t.Helper()
	db := openTestDB(t)
	store, err := NewScriptStore(db, filepath.Join(t.TempDir(), "scripts"))
	if err != nil {
		t.Fatalf("构造脚本仓储失败: %v", err)
	}
	return store
}

func TestScriptStoreRoundTrip(t *testing.T) {
	store := newTestScripts(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 25, 3, 0, 0, 0, time.UTC)
	in := scripts.Input{Name: "清理临时文件", Content: "#!/bin/sh\nrm -rf /var/tmp/x/*\n"}

	rec, err := store.Create(ctx, in, now)
	if err != nil {
		t.Fatalf("新建脚本失败: %v", err)
	}
	if !scripts.ValidID(rec.ID) {
		t.Fatalf("脚本 id 形态不对: %q", rec.ID)
	}
	if rec.ContentHash != scripts.Hash(in.Content) {
		t.Fatalf("内容指纹不对: %q", rec.ContentHash)
	}
	wantPath, _ := scripts.Path(store.Dir(), rec.ID)
	if rec.Ref != wantPath {
		t.Fatalf("落盘路径不对: %q，期望 %q", rec.Ref, wantPath)
	}

	got, found, err := store.Get(ctx, rec.ID)
	if err != nil || !found {
		t.Fatalf("读取脚本失败: %v found=%v", err, found)
	}
	if got.Content != in.Content || got.Name != in.Name || got.Interpreter != scripts.Interpreter {
		t.Fatalf("脚本没有原样回来: %#v", got)
	}

	if _, err := store.Create(ctx, in, now); !errors.Is(err, ErrScriptName) {
		t.Fatalf("重名脚本应当被拒绝，得到 %v", err)
	}

	next := scripts.Input{Name: "清理临时文件", Content: "#!/bin/sh\necho 2\n"}
	updated, err := store.Update(ctx, rec.ID, next, 1, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("更新脚本失败: %v", err)
	}
	if updated.Version != 2 || updated.ContentHash != scripts.Hash(next.Content) {
		t.Fatalf("更新后的版本/指纹不对: %#v", updated)
	}
	if _, err := store.Update(ctx, rec.ID, next, 1, now); !errors.Is(err, ErrScriptVersion) {
		t.Fatalf("旧版本更新应当报版本冲突，得到 %v", err)
	}
	if err := store.Delete(ctx, rec.ID, 2); err != nil {
		t.Fatalf("删除脚本失败: %v", err)
	}
	if _, found, _ := store.Get(ctx, rec.ID); found {
		t.Fatal("删除后不该还能读到")
	}
}

func TestScriptStoreRejectsNilDB(t *testing.T) {
	if _, err := NewScriptStore(nil, ""); err == nil {
		t.Fatal("没有数据库句柄却构造成功了")
	}
}
