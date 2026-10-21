package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"upanel/internal/processes"
)

// newProcessSignalAction 登记 process.signal（P05.2 的唯一进程写动作）。
//
// 一件事：给一个**身份已确定**的进程发 TERM 或 KILL。
//
//   - 目标身份是 PID + starttime，不是 PID：预检把两者写进计划，执行前再读一次
//     /proc 核对；不一致就一个信号都不发（见 processes.ExecuteSignal）。
//   - 受保护目标（PID 1、内核线程、面板/代理自身、受保护系统服务进程）在预检、
//     执行前复核、执行器内三处都被拒——不是只靠界面隐藏按钮。
//   - TERM 与 KILL 是两次独立操作，门槛不同（见 PolicyFor）。面板不会在 TERM 超时后
//     自动升级为 KILL：那等于把一个普通动作偷偷变成强制动作。
func newProcessSignalAction(src processesSource) Action {
	return Action{
		Name: processes.ActionSignal,
		Decode: func(raw json.RawMessage) (any, error) {
			var p processes.SignalParams
			if err := DecodeParams(raw, &p); err != nil {
				return nil, err
			}
			p = processes.Normalize(p)
			if err := processes.ValidateParams(p); err != nil {
				return nil, fmt.Errorf("%w: %v", ErrParamsInvalid, err)
			}
			return p, nil
		},
		Plan: func(ctx context.Context, in PlanInput) (Plan, error) {
			p, ok := in.Params.(processes.SignalParams)
			if !ok {
				return Plan{}, fmt.Errorf("参数类型不匹配: %T", in.Params)
			}
			assessment, err := src.Assess(ctx, p)
			if err != nil {
				return Plan{}, err
			}
			blockers := make([]Notice, 0, len(assessment.Blockers))
			for _, b := range assessment.Blockers {
				blockers = append(blockers, Notice{Code: b.Code, Message: b.Message})
			}
			warnings := make([]Notice, 0, len(assessment.Warnings))
			for _, w := range assessment.Warnings {
				warnings = append(warnings, Notice{Code: w.Code, Message: w.Message})
			}
			impacts := make([]Impact, 0, len(assessment.Impacts))
			for _, i := range assessment.Impacts {
				impacts = append(impacts, Impact{Kind: i.Kind, Description: i.Description})
			}
			return Plan{
				// 规范化参数：signal 已大写。预检绑定这一份，执行时按它解码。
				NormalizedParams: p,
				Target: Target{
					DisplayName:         assessment.DisplayName,
					IdentityFingerprint: assessment.Identity,
					StateVersion:        assessment.StateVersion,
				},
				Allowed:  assessment.Allowed,
				Blockers: blockers,
				Warnings: warnings,
				Impacts:  impacts,
				Locks:    assessment.Locks,
			}, nil
		},
		// CONTRACTS.md 第 5 节：TERM 只是请求退出，不需要额外门槛；
		// KILL 无法被捕获或忽略，必须再认证 + 输入确认短语 KILL。
		//
		// 策略在生成预检时算出来并落库，提交与执行读的是那份记录，
		// 所以前端既不能给 KILL 省掉短语，也不能给 TERM 加一层假的门槛。
		PolicyFor: func(params any) (ActionPolicy, error) {
			p, ok := params.(processes.SignalParams)
			if !ok {
				return ActionPolicy{}, fmt.Errorf("参数类型不匹配: %T", params)
			}
			if p.Signal == processes.SignalKILL {
				return ActionPolicy{
					RequiresReauth: true,
					RequiresPhrase: true,
					PhraseVerb:     processes.PhraseVerbKill,
					// 已经发出 SIGKILL 之后没有“安全取消”可言：
					// 进程要么已经死了，要么正在内核里死。
					Cancellable: false,
				}, nil
			}
			if p.Signal == processes.SignalTERM {
				return ActionPolicy{
					RequiresReauth: false,
					RequiresPhrase: false,
					Cancellable:    false,
				}, nil
			}
			return ActionPolicy{}, fmt.Errorf("未知信号 %q", p.Signal)
		},
		Execute: func(ctx context.Context, in ExecuteInput) (Result, error) {
			p, ok := in.Params.(processes.SignalParams)
			if !ok {
				return Result{}, fmt.Errorf("参数类型不匹配: %T", in.Params)
			}
			res, err := src.ExecuteSignal(ctx, p, in.Progress)
			if err != nil {
				return Result{}, err
			}
			observed := map[string]any{
				"pid":              res.PID,
				"start_time_ticks": res.StartTimeTicks,
				"signal":           res.Signal,
				"state_before":     res.StateBefore,
				"state_after":      res.StateAfter,
				"gone":             res.Gone,
			}
			data := map[string]any{
				"pid":              res.PID,
				"start_time_ticks": res.StartTimeTicks,
				"signal":           res.Signal,
				"delivered":        res.Delivered,
				"duration_ms":      res.Duration.Milliseconds(),
			}
			// ESRCH：检查与发送之间进程自己退出了。意图已达成，但要说清楚信号没发出去。
			if !res.Delivered {
				data["note"] = "进程在发信号之前已经退出，信号未送出"
			}
			return Result{
				Outcome:       OutcomeVerified,
				VerifiedAt:    time.Now().UTC(),
				ObservedState: observed,
				Data:          data,
			}, nil
		},
	}
}
