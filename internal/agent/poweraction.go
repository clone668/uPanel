package agent

// system.power（P11.3）：重启与关机。
//
// 与 package.* 一样，这一层只做翻译：把参数变成一次可审计的意图、把预检结论翻成 Plan、
// 把执行结果翻成 Result。判断全在 internal/power 里（boot_id、systemd、关键任务、倒计时）。
//
// 与其它动作不同的是**它的执行会把执行者自己杀掉**。所以有三处刻意的不对称：
//
//	预检     要能回答“现在能不能重启”——查在跑的任务是硬条件（STAGES §13）
//	执行     倒计时期间任务还活着，取消是真的（命令还没发出去）
//	结果     outcome 恒为 accepted：机器要消失了，谁也不能声称自己验证过
//
// 第三条是契约明写的（CONTRACTS.md 4.3：电源类动作允许 succeeded + accepted，
// 界面必须表述为“请求已受理”）。真正确认发生在下次连上之后，用 boot_id 比。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"upanel/internal/power"
)

// PowerSource 是电源动作的实现端（生产实现是 *power.Executor）。
type PowerSource interface {
	Assess(ctx context.Context, req power.AssessRequest) (power.Assessment, error)
	Execute(ctx context.Context, req power.ExecuteRequest) (power.Execution, error)
}

// powerParams 是请求参数。字段名与类型以 CONTRACTS.md 第 5 节为准。
type powerParams struct {
	Operation    string `json:"operation"`
	DelaySeconds int    `json:"delay_seconds"`
}

// RegisterPowerAction 登记 system.power。
func RegisterPowerAction(reg *Registry, src PowerSource) error {
	if src == nil {
		return errors.New("agent: 电源动作没有实现端")
	}
	return reg.RegisterAction(Action{
		Name: power.ActionPower,
		// 预检只读三个小文件加一次内存查询，不跑外部命令；预算用默认的毫秒级即可。
		Decode: func(raw json.RawMessage) (any, error) {
			var params powerParams
			if err := DecodeParams(raw, &params); err != nil {
				return nil, err
			}
			// 立刻校验并规范化：预检绑定的就是这份值，提交时不再接受新参数。
			p := power.Params{Operation: power.Op(params.Operation), DelaySeconds: params.DelaySeconds}
			if err := p.Validate(); err != nil {
				return nil, fmt.Errorf("%w: %v", ErrParamsInvalid, err)
			}
			return p, nil
		},
		Plan: func(ctx context.Context, in PlanInput) (Plan, error) {
			params, ok := in.Params.(power.Params)
			if !ok {
				return Plan{}, fmt.Errorf("参数类型不匹配: %T", in.Params)
			}
			assessment, err := src.Assess(ctx, power.AssessRequest{
				Params: params,
				// 执行前复核时这个操作自己正在跑；交给 power 排掉，否则
				// “检查关键任务”会把发起者自己算成关键任务，每次重启都自己拒自己。
				// 预检阶段 in.OperationID 是空串，等价于“谁在跑都算”。
				ExcludeOperationID: in.OperationID,
			})
			if err != nil {
				return Plan{}, err
			}
			return planFromPowerAssessment(assessment), nil
		},
		// 策略：要再认证（CONTRACTS 第 5 节的 R 列），不要短语（“独立确认”由界面
		// 那一次单独点击承担，不是靠打字），**可取消**——倒计时期间取消是有意义的，
		// 因为命令还没发出去。
		Policy: ActionPolicy{RequiresReauth: true, Cancellable: true},
		Execute: func(ctx context.Context, in ExecuteInput) (Result, error) {
			params, ok := in.Params.(power.Params)
			if !ok {
				return Result{}, fmt.Errorf("参数类型不匹配: %T", in.Params)
			}
			exec, err := src.Execute(ctx, power.ExecuteRequest{
				Params:      params,
				OperationID: in.OperationID,
				Progress:    in.Progress,
				Cancel:      in.Cancel,
			})
			if err != nil {
				return Result{}, powerFailure{err: err}
			}
			if exec.Canceled {
				// 倒计时里被取消：这不是失败，是“没做”。代理据此把操作标成 canceled，
				// 与“发出去之后”的状态分得清清楚楚。
				return Result{}, ErrCanceled
			}
			return powerResult(exec), nil
		},
	})
}

// planFromPowerAssessment 把 internal/power 的结论逐字段翻成 Plan。
func planFromPowerAssessment(a power.Assessment) Plan {
	blockers := make([]Notice, 0, len(a.Blockers))
	for _, b := range a.Blockers {
		blockers = append(blockers, Notice{Code: b.Code, Message: b.Message})
	}
	warnings := make([]Notice, 0, len(a.Warnings))
	for _, w := range a.Warnings {
		warnings = append(warnings, Notice{Code: w.Code, Message: w.Message})
	}
	impacts := make([]Impact, 0, len(a.Impacts))
	for _, i := range a.Impacts {
		impacts = append(impacts, Impact{Kind: i.Kind, Description: i.Description})
	}
	locks := a.Locks
	if len(locks) == 0 {
		locks = []string{power.LockName}
	}
	return Plan{
		NormalizedParams: a.Params,
		Target: Target{
			DisplayName:         a.Target.DisplayName,
			IdentityFingerprint: a.Target.IdentityFingerprint,
			StateVersion:        a.Target.StateVersion,
		},
		Allowed:  a.Allowed,
		Blockers: blockers,
		Warnings: warnings,
		Impacts:  impacts,
		Locks:    locks,
	}
}

// powerResult 把一次下发翻成任务的 result。
//
// observed_state 里放的是**下发那一刻的事实**（boot_id 之前、延迟、命令、受理时间），
// 不是“重启成功了”。页面据此说“请求已受理”；重启是否真的发生，由之后的 boot_id
// 比对来回答（那是另一条记录，见 operations 的重启确认）。
func powerResult(e power.Execution) Result {
	observed := map[string]any{
		"operation":      string(e.Params.Operation),
		"delay_seconds":  e.Params.DelaySeconds,
		"boot_id_before": e.BootIDBefore,
		"issued_at":      e.IssuedAt.UTC().Format("2006-01-02T15:04:05Z"),
		"command":        e.Command,
		"exit_code":      e.IssuedResult.ExitCode,
	}
	hint := "重启后重新登录即可；面板会用 boot_id 的变化确认它真的重启过"
	if e.Params.Operation == power.OpPoweroff {
		hint = "关机后面板连不上，只能人工开机；开机后重新登录，面板会按 boot_id 说明这次关机之后机器是否回来过"
	}
	return Result{
		Outcome:       power.OutcomeAccepted,
		ObservedState: observed,
		RecoveryHint:  hint,
		Data: map[string]any{
			"confirmation":   "boot_id",
			"boot_id_before": e.BootIDBefore,
		},
	}
}

// powerFailure 让失败原因以一句人话进任务的 error.message。
//
// 失败任务的 result 按契约是空对象，管理员唯一能看到的解释就是这里。
type powerFailure struct{ err error }

func (f powerFailure) Error() string { return f.err.Error() }
func (f powerFailure) Unwrap() error { return f.err }

// FailureReason 实现 FailureReasoner：把 power 的原因写进任务的 error.message。
//
// 这里**不重排**消息：internal/power 的错误已经以码开头（POWER_ABORTED_JOBS_STARTED、
// POWER_ISSUE_FAILED …），管理员据此分辨“被别人的任务挤掉”和“systemctl 失败”。
// 从前这个方法是 `(string, string)`，签名对不上接口，等于这句话从来没被用上——
// 于是所有失败都以代理的兜底文案出现在任务里。
func (f powerFailure) FailureReason() string { return oneLine(f.err.Error(), 300) }
