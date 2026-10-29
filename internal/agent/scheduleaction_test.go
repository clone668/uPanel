package agent

// schedule.apply / schedule.delete 的翻译层（P12.4）。
//
// 判断“这条计划能不能落地”在 internal/schedules 里有自己的测试；这里只验代理
// 有没有添油加醋：参数形状、结论搬运、锁、策略，以及执行失败的原因有没有被吞掉。

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"upanel/internal/schedules"
)

const testScheduleID = "sch_0123456789abcdef0123456789abcdef"

// fakeSchedules 是 agent 层的假计划端。
type fakeSchedules struct {
	applyAssessment schedules.Assessment
	applyErr        error
	applied         []schedules.ApplyRequest

	record    schedules.Record
	applyExec error

	deleteAssessment schedules.Assessment
	deleteErr        error
	deleted          []string
	deleteExec       error
}

func (f *fakeSchedules) AssessApply(_ context.Context, req schedules.ApplyRequest) (schedules.Assessment, error) {
	f.applied = append(f.applied, req)
	return f.applyAssessment, f.applyErr
}

func (f *fakeSchedules) Apply(_ context.Context, req schedules.ApplyRequest) (schedules.Record, error) {
	if f.applyExec != nil {
		return schedules.Record{}, f.applyExec
	}
	rec := f.record
	rec.ID = req.ScheduleID
	if rec.ID == "" {
		rec.ID = testScheduleID
	}
	return rec, nil
}

func (f *fakeSchedules) AssessDelete(_ context.Context, id string, _ int64) (schedules.Assessment, error) {
	f.deleted = append(f.deleted, id)
	return f.deleteAssessment, f.deleteErr
}

func (f *fakeSchedules) Delete(_ context.Context, id string, _ int64) error {
	f.deleted = append(f.deleted, id)
	return f.deleteExec
}

func newFakeSchedules() *fakeSchedules {
	return &fakeSchedules{
		applyAssessment: schedules.Assessment{
			Allowed:             true,
			DisplayName:         "夜间备份",
			IdentityFingerprint: "sha256:def",
			StateVersion:        "pol_0123456789abcdef0123456789abcdef:v7",
			Impacts: []schedules.Impact{
				{Kind: "systemd_unit", Description: "写入 /etc/systemd/system/upanel-sch_x.timer"},
			},
			NextElapse: timePtr(time.Date(2026, 10, 25, 3, 30, 0, 0, time.UTC)),
		},
		record: schedules.Record{
			Name:     "夜间备份",
			Kind:     schedules.KindBackup,
			Enabled:  true,
			Version:  1,
			TimerRef: "upanel-sch_x.timer",
		},
		deleteAssessment: schedules.Assessment{Allowed: true},
	}
}

func newScheduleRegistry(t *testing.T, src scheduleSource) *Registry {
	t.Helper()
	reg := NewRegistry()
	if err := RegisterScheduleActions(reg, src); err != nil {
		t.Fatalf("登记计划动作失败: %v", err)
	}
	return reg
}

func scheduleApplyJSON(t *testing.T) string {
	t.Helper()
	return `{"schedule_id":null,"version":0,"definition":{"name":"夜间备份",` +
		`"kind":"backup","target_id":"pol_0123456789abcdef0123456789abcdef",` +
		`"calendar_expression":"*-*-* 03:30:00","timezone":"Asia/Shanghai",` +
		`"persistent":true,"max_runtime_seconds":7200,"enabled":true}}`
}

func TestRegisterScheduleActionsRequiresSource(t *testing.T) {
	if err := RegisterScheduleActions(NewRegistry(), nil); err == nil {
		t.Fatal("没有实现端就不该登记成功")
	}
	// 没接线时静默跳过：能力表里不出现，而不是出现一个 panic 的入口。
	if err := RegisterScheduleActionsIfAvailable(NewRegistry(), nil); err != nil {
		t.Fatalf("没有实现端时不该报错: %v", err)
	}
}

func TestScheduleActionsRegistered(t *testing.T) {
	reg := newScheduleRegistry(t, newFakeSchedules())
	for _, name := range []string{schedules.ActionApply, schedules.ActionDelete} {
		if _, ok := reg.Action(name); !ok {
			t.Fatalf("%s 没有登记", name)
		}
		// 手动触发必须是“没有这个动作”，而不是“有但拒绝”：
		// 触发只能由 root 从专用路由进来（CONTRACTS §15）。
		if _, ok := reg.Action(name + ".trigger"); ok {
			t.Fatalf("不该登记 %s.trigger", name)
		}
	}
}

func TestScheduleActionPolicies(t *testing.T) {
	reg := newScheduleRegistry(t, newFakeSchedules())
	for _, name := range []string{schedules.ActionApply, schedules.ActionDelete} {
		action, _ := reg.Action(name)
		policy, err := action.ResolvePolicy(nil)
		if err != nil {
			t.Fatalf("%s 策略不合法: %v", name, err)
		}
		if !policy.RequiresReauth {
			t.Fatalf("%s 必须再认证（契约 R 列）", name)
		}
		if policy.RequiresPhrase {
			t.Fatalf("%s 不该要求短语（%q）", name, policy.PhraseVerb)
		}
		if policy.Cancellable {
			t.Fatalf("%s 不该可取消：单元写到一半停手只会留下半对文件", name)
		}
	}
}

func TestScheduleApplyDecodeStrict(t *testing.T) {
	reg := newScheduleRegistry(t, newFakeSchedules())
	action, _ := reg.Action(schedules.ActionApply)

	params, err := action.Decode(json.RawMessage(scheduleApplyJSON(t)))
	if err != nil {
		t.Fatalf("正常参数被拒: %v", err)
	}
	p, ok := params.(schedules.ApplyParams)
	if !ok {
		t.Fatalf("参数类型 = %T", params)
	}
	if p.ID() != "" || p.Definition.Kind != schedules.KindBackup {
		t.Fatalf("参数解码错：%+v", p)
	}

	bad := []string{
		// 未知字段：定义里多一个字段就意味着契约被改过，不能默默忽略。
		`{"schedule_id":null,"version":0,"definition":{"name":"x","kind":"backup",` +
			`"target_id":"pol_0123456789abcdef0123456789abcdef","calendar_expression":"*-*-* 03:30:00",` +
			`"timezone":"UTC","persistent":true,"max_runtime_seconds":60,"enabled":true,"command":"rm -rf /"}}`,
		`{"schedule_id":null,"version":3,"definition":{"name":"x","kind":"backup",` +
			`"target_id":"pol_0123456789abcdef0123456789abcdef","calendar_expression":"*-*-* 03:30:00",` +
			`"timezone":"UTC","persistent":true,"max_runtime_seconds":60,"enabled":true}}`, // 新建带了版本
		`{"schedule_id":"sch_zzz","version":1,"definition":{"name":"x","kind":"backup",` +
			`"target_id":"pol_0123456789abcdef0123456789abcdef","calendar_expression":"*-*-* 03:30:00",` +
			`"timezone":"UTC","persistent":true,"max_runtime_seconds":60,"enabled":true}}`, // id 形状
		`{"schedule_id":null,"version":0}`, // 没有定义
	}
	for _, raw := range bad {
		if _, err := action.Decode(json.RawMessage(raw)); !errors.Is(err, ErrParamsInvalid) {
			t.Fatalf("%s 应当被拒: %v", raw, err)
		}
	}
}

func TestScheduleDeleteDecodeStrict(t *testing.T) {
	reg := newScheduleRegistry(t, newFakeSchedules())
	action, _ := reg.Action(schedules.ActionDelete)
	if _, err := action.Decode(json.RawMessage(`{"schedule_id":"` + testScheduleID + `","version":2}`)); err != nil {
		t.Fatalf("正常参数被拒: %v", err)
	}
	for _, raw := range []string{
		`{"schedule_id":"` + testScheduleID + `","version":0}`,
		`{"schedule_id":"` + testScheduleID + `","version":1,"force":true}`,
		`{"version":1}`,
	} {
		if _, err := action.Decode(json.RawMessage(raw)); !errors.Is(err, ErrParamsInvalid) {
			t.Fatalf("%s 应当被拒: %v", raw, err)
		}
	}
}

// TestScheduleApplyPlanCarriesAssessment：结论一个字都不能改。
func TestScheduleApplyPlanCarriesAssessment(t *testing.T) {
	src := newFakeSchedules()
	src.applyAssessment.Blockers = []schedules.Notice{{Code: "SCHEDULE_CALENDAR_INVALID", Message: "systemd 不认"}}
	src.applyAssessment.Warnings = []schedules.Notice{{Code: "SCHEDULE_TARGET_NOTE", Message: "策略被改过"}}
	reg := newScheduleRegistry(t, src)
	action, _ := reg.Action(schedules.ActionApply)

	params, err := action.Decode(json.RawMessage(scheduleApplyJSON(t)))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := action.Plan(context.Background(), PlanInput{Params: params})
	if err != nil {
		t.Fatalf("预检失败: %v", err)
	}
	// blockers 非空但 Allowed 仍由模块给：代理不让“有阻塞项”自动变成“禁止”，
	// 判决只有一处来源。
	if len(plan.Blockers) != 1 || plan.Blockers[0].Code != "SCHEDULE_CALENDAR_INVALID" {
		t.Fatalf("阻塞项 = %+v", plan.Blockers)
	}
	if len(plan.Warnings) != 1 || plan.Warnings[0].Code != "SCHEDULE_TARGET_NOTE" {
		t.Fatalf("告警 = %+v", plan.Warnings)
	}
	if plan.Target.DisplayName != "夜间备份" || plan.Target.StateVersion != "pol_0123456789abcdef0123456789abcdef:v7" {
		t.Fatalf("目标 = %+v", plan.Target)
	}
	if len(plan.Impacts) != 1 || !strings.Contains(plan.Impacts[0].Description, "systemd") {
		t.Fatalf("影响 = %+v", plan.Impacts)
	}
	// 新建时没有 id，也就不该有锁：凭空的锁会让两次新建互相排队。
	if len(plan.Locks) != 0 {
		t.Fatalf("新建不该加锁: %v", plan.Locks)
	}
	if _, ok := plan.NormalizedParams.(schedules.ApplyParams); !ok {
		t.Fatalf("规范化参数类型 = %T", plan.NormalizedParams)
	}
}

func TestScheduleUpdateLocksByID(t *testing.T) {
	src := newFakeSchedules()
	reg := newScheduleRegistry(t, src)
	action, _ := reg.Action(schedules.ActionApply)

	raw := strings.Replace(scheduleApplyJSON(t), `"schedule_id":null,"version":0`,
		`"schedule_id":"`+testScheduleID+`","version":4`, 1)
	params, err := action.Decode(json.RawMessage(raw))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := action.Plan(context.Background(), PlanInput{Params: params})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Locks) != 1 || plan.Locks[0] != "schedule:"+testScheduleID {
		t.Fatalf("锁 = %v", plan.Locks)
	}
	if len(src.applied) != 1 || src.applied[0].Version != 4 || src.applied[0].ScheduleID != testScheduleID {
		t.Fatalf("送给模块的请求 = %+v", src.applied)
	}
}

func TestScheduleApplyExecuteVerified(t *testing.T) {
	reg := newScheduleRegistry(t, newFakeSchedules())
	action, _ := reg.Action(schedules.ActionApply)
	params, err := action.Decode(json.RawMessage(scheduleApplyJSON(t)))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 24, 20, 0, 0, 0, time.UTC)
	res, err := action.Execute(context.Background(), ExecuteInput{Params: params, Now: now})
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if res.Outcome != OutcomeVerified {
		t.Fatalf("结果 = %q，期望 verified（单元启用后 systemctl 已核对）", res.Outcome)
	}
	if res.ObservedState["timer_ref"] != "upanel-sch_x.timer" || res.ObservedState["enabled"] != true {
		t.Fatalf("观测状态 = %+v", res.ObservedState)
	}
	if !strings.Contains(res.RecoveryHint, "schedule-trigger") {
		t.Fatalf("提示没说清谁在计时: %q", res.RecoveryHint)
	}
}

func TestScheduleApplyExecuteFailureKeepsReason(t *testing.T) {
	src := newFakeSchedules()
	src.applyExec = errors.New("定义已保存，但定时单元没装上：systemctl enable 失败")
	reg := newScheduleRegistry(t, src)
	action, _ := reg.Action(schedules.ActionApply)
	params, err := action.Decode(json.RawMessage(scheduleApplyJSON(t)))
	if err != nil {
		t.Fatal(err)
	}
	_, err = action.Execute(context.Background(), ExecuteInput{Params: params})
	if err == nil {
		t.Fatal("失败必须报错，不能返回一个空结果")
	}
	var reasoner FailureReasoner
	if !errors.As(err, &reasoner) {
		t.Fatalf("执行失败应当能给出已核实原因（%T）", err)
	}
	if !strings.Contains(reasoner.FailureReason(), "没装上") {
		t.Fatalf("原因 = %q", reasoner.FailureReason())
	}
}

func TestScheduleDeletePlanAndExecute(t *testing.T) {
	src := newFakeSchedules()
	reg := newScheduleRegistry(t, src)
	action, _ := reg.Action(schedules.ActionDelete)
	params, err := action.Decode(json.RawMessage(`{"schedule_id":"` + testScheduleID + `","version":3}`))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := action.Plan(context.Background(), PlanInput{Params: params})
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Allowed || plan.Locks[0] != "schedule:"+testScheduleID {
		t.Fatalf("计划 = %+v", plan)
	}
	res, err := action.Execute(context.Background(), ExecuteInput{
		Params: params, Now: time.Date(2026, 10, 24, 20, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if res.Outcome != OutcomeVerified || res.ObservedState["deleted"] != true {
		t.Fatalf("结果 = %+v", res)
	}
	// 删除也走模块：代理不自己动单元目录，也就不会绕过"先停后删"。
	if len(src.deleted) == 0 || src.deleted[len(src.deleted)-1] != testScheduleID {
		t.Fatalf("删除请求 = %v", src.deleted)
	}
}

func timePtr(t time.Time) *time.Time { return &t }
