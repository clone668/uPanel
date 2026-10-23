package agent

// system.power 的翻译层（P11.3）。
//
// internal/power 的测试负责 boot_id、systemd、关键任务与倒计时本身；这里只验代理
// 有没有给它添油加醋，四件事：
//
//  1. 参数形状：未知字段、越界的 delay、缺 delay 一律拒绝（不做“忽略继续”）；
//  2. 预检结论逐字段直译，资源锁有兜底；
//  3. 执行结果只允许 accepted——**这台机器马上要消失，谁也不能声称验证过**；
//  4. 倒计时里取消 → ErrCanceled（不是失败）；被别人的任务挤掉 → 自己的码。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"upanel/internal/power"
)

// fakePower 是 agent 层的假电源端。
type fakePower struct {
	assessment power.Assessment
	assessErr  error
	assessed   []power.AssessRequest

	execution power.Execution
	execErr   error
	requests  []power.ExecuteRequest
}

func (f *fakePower) Assess(_ context.Context, req power.AssessRequest) (power.Assessment, error) {
	f.assessed = append(f.assessed, req)
	return f.assessment, f.assessErr
}

func (f *fakePower) Execute(_ context.Context, req power.ExecuteRequest) (power.Execution, error) {
	f.requests = append(f.requests, req)
	if req.Progress != nil {
		req.Progress("countdown", map[string]any{"remaining_seconds": 30})
	}
	return f.execution, f.execErr
}

func newFakePower() *fakePower {
	issuedAt := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	return &fakePower{
		assessment: power.Assessment{
			Op:       power.OpReboot,
			Params:   power.Params{Operation: power.OpReboot, DelaySeconds: 60},
			Allowed:  true,
			Locks:    []string{power.LockName},
			Impacts:  []power.Impact{{Kind: power.ImpactConnection, Description: "会断开连接"}},
			Target:   power.Target{DisplayName: "这台服务器", IdentityFingerprint: "boot:abc", StateVersion: "abc"},
			InFlight: nil,
		},
		execution: power.Execution{
			Outcome:      power.OutcomeAccepted,
			Params:       power.Params{Operation: power.OpReboot, DelaySeconds: 60},
			BootIDBefore: "ab2eb5e8-c1b4-4a00-a3d0-33ced7870abc",
			IssuedAt:     issuedAt,
			Command:      "/usr/bin/systemctl reboot",
			IssuedResult: power.CommandResult{Program: "/usr/bin/systemctl", ExitCode: 0},
		},
	}
}

func newPowerRegistry(t *testing.T, src PowerSource) *Registry {
	t.Helper()
	reg := NewRegistry()
	if err := RegisterPowerAction(reg, src); err != nil {
		t.Fatalf("登记 system.power 失败: %v", err)
	}
	return reg
}

func TestRegisterPowerActionRequiresSource(t *testing.T) {
	if err := RegisterPowerAction(NewRegistry(), nil); err == nil {
		t.Fatal("没有实现端就不该登记成功")
	}
}

func TestRegisterPowerActionName(t *testing.T) {
	reg := newPowerRegistry(t, newFakePower())
	if _, ok := reg.Action(power.ActionPower); !ok {
		t.Fatalf("%s 没有登记", power.ActionPower)
	}
	if !contains(reg.ExecutableActionNames(), power.ActionPower) {
		t.Fatalf("电源动作应当可执行: %v", reg.ExecutableActionNames())
	}
}

// TestPowerPolicy：要再认证（契约 R 列），不要短语（独立确认靠界面那一次点击），
// 可取消（倒计时期间命令还没发出去，取消是真的）。
func TestPowerPolicy(t *testing.T) {
	reg := newPowerRegistry(t, newFakePower())
	action, _ := reg.Action(power.ActionPower)
	policy, err := action.ResolvePolicy(nil)
	if err != nil {
		t.Fatalf("策略不合法: %v", err)
	}
	if !policy.RequiresReauth {
		t.Fatal("电源动作必须再认证")
	}
	if policy.RequiresPhrase {
		t.Fatalf("不该要求短语（%q）：独立确认由界面承担", policy.PhraseVerb)
	}
	if !policy.Cancellable {
		t.Fatal("倒计时期间必须可以取消")
	}
}

func TestPowerDecodeStrict(t *testing.T) {
	reg := newPowerRegistry(t, newFakePower())
	action, _ := reg.Action(power.ActionPower)

	params, err := action.Decode(json.RawMessage(`{"operation":"poweroff","delay_seconds":120}`))
	if err != nil {
		t.Fatalf("正常参数被拒: %v", err)
	}
	got := params.(power.Params)
	if got.Operation != power.OpPoweroff || got.DelaySeconds != 120 {
		t.Fatalf("参数解码错: %+v", got)
	}

	bad := []string{
		`{"operation":"reboot","delay_seconds":60,"force":true}`, // 未知字段
		`{"operation":"reboot"}`,                                 // 缺 delay（刻意无默认值）
		`{"operation":"reboot","delay_seconds":5}`,               //
		`{"operation":"reboot","delay_seconds":9999}`,            //
		`{"operation":"halt","delay_seconds":60}`,                // 未定义的操作
		`{}`, //
	}
	for _, raw := range bad {
		if _, err := action.Decode(json.RawMessage(raw)); !errors.Is(err, ErrParamsInvalid) {
			t.Fatalf("%s 应当被拒: %v", raw, err)
		}
	}
}

func TestPowerPlanMapping(t *testing.T) {
	src := newFakePower()
	src.assessment.Allowed = false
	src.assessment.Blockers = []power.Reason{{Code: power.CodeJobsRunning, Message: "有写操作正在执行"}}
	src.assessment.Warnings = []power.Reason{{Code: "poweroff_needs_manual_start", Message: "不会自己回来"}}
	src.assessment.Locks = nil // 兜底
	reg := newPowerRegistry(t, src)
	action, _ := reg.Action(power.ActionPower)

	plan, err := action.Plan(context.Background(), PlanInput{
		Params: power.Params{Operation: power.OpPoweroff, DelaySeconds: 60},
	})
	if err != nil {
		t.Fatalf("预检报错: %v", err)
	}
	if plan.Allowed {
		t.Fatal("预检拒绝时计划不能放行")
	}
	if len(plan.Blockers) != 1 || plan.Blockers[0].Code != power.CodeJobsRunning {
		t.Fatalf("阻塞项没照抄: %+v", plan.Blockers)
	}
	if len(plan.Warnings) != 1 || plan.Warnings[0].Code != "poweroff_needs_manual_start" {
		t.Fatalf("告警没照抄: %+v", plan.Warnings)
	}
	if len(plan.Locks) != 1 || plan.Locks[0] != power.LockName {
		t.Fatalf("兜底锁错: %+v", plan.Locks)
	}
	if plan.Target.StateVersion != "abc" || plan.Target.IdentityFingerprint != "boot:abc" {
		t.Fatalf("目标绑定错: %+v", plan.Target)
	}
	if len(src.assessed) != 1 || src.assessed[0].Params.Operation != power.OpPoweroff {
		t.Fatalf("预检没有把参数交下去: %+v", src.assessed)
	}
	// 预检阶段没有操作 id：谁在跑都要算进来。
	if src.assessed[0].ExcludeOperationID != "" {
		t.Fatalf("预检不该排除任何操作，实际排除了 %q", src.assessed[0].ExcludeOperationID)
	}
}

// TestPowerPlanPassesOperationIDToPower：执行前复核（runner 会填 PlanInput.OperationID）
// 必须把它交到 power 层，否则电源动作会把自己算成“正在跑的关键任务”并自己拒掉自己。
//
// 这是 P11.3 在真机上暴露过的故障：每个 system.power 任务都停在 revalidate，
// 错误 POWER_JOBS_RUNNING，而同一时刻的预检是 allowed。
func TestPowerPlanPassesOperationIDToPower(t *testing.T) {
	src := newFakePower()
	reg := newPowerRegistry(t, src)
	action, _ := reg.Action(power.ActionPower)

	if _, err := action.Plan(context.Background(), PlanInput{
		Params:      power.Params{Operation: power.OpReboot, DelaySeconds: 60},
		OperationID: "op_self",
	}); err != nil {
		t.Fatalf("预检报错: %v", err)
	}
	if len(src.assessed) != 1 || src.assessed[0].ExcludeOperationID != "op_self" {
		t.Fatalf("执行前复核没有把操作 id 交下去: %+v", src.assessed)
	}
}

// TestPowerExecuteAccepted：结论只能是 accepted，且 observed_state 记的是
// **下发那一刻**的事实，不是“重启成功了”。
func TestPowerExecuteAccepted(t *testing.T) {
	src := newFakePower()
	reg := newPowerRegistry(t, src)
	action, _ := reg.Action(power.ActionPower)

	res, err := action.Execute(context.Background(), ExecuteInput{
		Params:      power.Params{Operation: power.OpReboot, DelaySeconds: 60},
		OperationID: "op_power",
	})
	if err != nil {
		t.Fatalf("执行报错: %v", err)
	}
	if err := validateResult(res); err != nil {
		t.Fatalf("结果不自洽: %v", err)
	}
	if res.Outcome != OutcomeAccepted {
		t.Fatalf("电源动作只能是 accepted，实际 %s", res.Outcome)
	}
	if !res.VerifiedAt.IsZero() {
		t.Fatal("没人验证过，不能留下 verified_at")
	}
	if res.ObservedState["boot_id_before"] != src.execution.BootIDBefore {
		t.Fatalf("observed_state 要记下发前的 boot_id: %+v", res.ObservedState)
	}
	if res.ObservedState["command"] != "/usr/bin/systemctl reboot" {
		t.Fatalf("observed_state 要记下实际命令: %+v", res.ObservedState)
	}
	if res.ObservedState["issued_at"] != "2026-10-23T12:00:00Z" {
		t.Fatalf("受理时间要记下: %+v", res.ObservedState)
	}
	if res.Data["confirmation"] != "boot_id" {
		t.Fatalf("确认方式要写明: %+v", res.Data)
	}
	if len(src.requests) != 1 || src.requests[0].OperationID != "op_power" {
		t.Fatalf("OperationID 没有透传（审计与事件靠它对齐）: %+v", src.requests)
	}
}

// TestPowerExecutePoweroffHint：关机后的恢复提示必须说清“要人工开机”，
// 不能说成“重启后重新登录”。
func TestPowerExecutePoweroffHint(t *testing.T) {
	src := newFakePower()
	src.execution.Params = power.Params{Operation: power.OpPoweroff, DelaySeconds: 60}
	reg := newPowerRegistry(t, src)
	action, _ := reg.Action(power.ActionPower)

	res, err := action.Execute(context.Background(), ExecuteInput{
		Params: power.Params{Operation: power.OpPoweroff, DelaySeconds: 60},
	})
	if err != nil {
		t.Fatalf("执行报错: %v", err)
	}
	if !strings.Contains(res.RecoveryHint, "人工开机") {
		t.Fatalf("关机要说明需要人工开机: %q", res.RecoveryHint)
	}
}

// TestPowerExecuteCanceled：倒计时里取消不是失败，是“没做”。
func TestPowerExecuteCanceled(t *testing.T) {
	src := newFakePower()
	src.execution = power.Execution{Outcome: power.OutcomeAccepted, Canceled: true}
	reg := newPowerRegistry(t, src)
	action, _ := reg.Action(power.ActionPower)

	_, err := action.Execute(context.Background(), ExecuteInput{
		Params: power.Params{Operation: power.OpReboot, DelaySeconds: 60},
	})
	if !errors.Is(err, ErrCanceled) {
		t.Fatalf("取消应当返回 ErrCanceled: %v", err)
	}
}

// TestPowerFailureReason：两种失败要分成两个码，别都说成“下发失败”。
func TestPowerFailureReason(t *testing.T) {
	src := newFakePower()
	src.execErr = fmt.Errorf("%s: %w: package.remove（op_pkg）",
		power.CodeAbortedByJob, power.ErrJobsStarted)
	reg := newPowerRegistry(t, src)
	action, _ := reg.Action(power.ActionPower)

	_, err := action.Execute(context.Background(), ExecuteInput{
		Params: power.Params{Operation: power.OpReboot, DelaySeconds: 60},
	})
	if err == nil {
		t.Fatal("执行失败必须返回 error")
	}
	reasoner, ok := err.(FailureReasoner)
	if !ok {
		t.Fatalf("失败原因要能走到 error.message: %T", err)
	}
	msg := reasoner.FailureReason()
	if !strings.HasPrefix(msg, power.CodeAbortedByJob) {
		t.Fatalf("被任务挤掉的码 = %q，期望以 %q 开头", msg, power.CodeAbortedByJob)
	}
	if !strings.Contains(msg, "package.remove") {
		t.Fatalf("原因要点名是哪个任务: %q", msg)
	}

	// systemctl 失败：另一个码。
	src2 := newFakePower()
	src2.execErr = errors.New(power.CodeIssueFailed + ": /usr/bin/systemctl poweroff 失败（退出码 1）")
	reg2 := newPowerRegistry(t, src2)
	action2, _ := reg2.Action(power.ActionPower)
	_, err = action2.Execute(context.Background(), ExecuteInput{
		Params: power.Params{Operation: power.OpPoweroff, DelaySeconds: 60},
	})
	reasoner, ok = err.(FailureReasoner)
	if !ok {
		t.Fatalf("失败原因要能走到 error.message: %T", err)
	}
	if msg := reasoner.FailureReason(); !strings.HasPrefix(msg, power.CodeIssueFailed) {
		t.Fatalf("下发失败的码 = %q，期望以 %q 开头", msg, power.CodeIssueFailed)
	}
}

// TestPowerDependentsUnbound：没接线时报错，绝不当成“没有任务在跑”。
func TestPowerDependentsUnbound(t *testing.T) {
	if _, err := NewPowerDependents().InFlight(""); err == nil {
		t.Fatal("没接线必须报错（查不到 ≠ 没有）")
	}
	// 接线之后仍然是错的（还没有 Server），但错误换了内容——说明它确实去问了。
	d := NewPowerDependents()
	d.Bind(nil)
	if _, err := d.InFlight(""); err == nil {
		t.Fatal("Server 为空时也必须报错")
	}
}

// TestPowerDependentsNilReceiver：一个 nil 的持有者不该 panic。
func TestPowerDependentsNilReceiver(t *testing.T) {
	var d *PowerDependents
	if _, err := d.InFlight(""); err == nil {
		t.Fatal("nil 持有者应当报错而不是 panic")
	}
}
