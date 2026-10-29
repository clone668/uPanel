package agent

// 计划任务动作（P12.4）：schedule.apply / schedule.delete。
//
// 这一层只做翻译：参数 → 意图，结论 → Plan，执行 → Result。判断全在
// internal/schedules 里（日历认不认、目标还存不存在、单元装没装上）。
//
// 两点刻意的不对称：
//
//	策略   两个动作都要再认证（CONTRACTS 第 5 节 R 列）：写的是 systemd 单元，
//	       也就是"这台机器以后会在没有人看着的时候自己做事"。不可取消：
//	       单元写到一半停手，留下的是一对只能靠人手收拾的文件。
//	锁     按 schedule:<id> 加锁。同一个计划的两次修改必须串行，否则两个
//	       daemon-reload + enable 交错执行，最后装上的单元是谁的那个说不准。

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"upanel/internal/schedules"
)

// scheduleSource 是计划动作需要的全部能力（生产实现是 *schedules.Module）。
type scheduleSource interface {
	AssessApply(ctx context.Context, req schedules.ApplyRequest) (schedules.Assessment, error)
	Apply(ctx context.Context, req schedules.ApplyRequest) (schedules.Record, error)
	AssessDelete(ctx context.Context, id string, version int64) (schedules.Assessment, error)
	Delete(ctx context.Context, id string, version int64) error
}

// RegisterScheduleActions 登记计划的写动作。
func RegisterScheduleActions(reg *Registry, src scheduleSource) error {
	if src == nil {
		return fmt.Errorf("agent: 计划任务动作没有实现端")
	}
	for _, a := range []Action{newScheduleApplyAction(src), newScheduleDeleteAction(src)} {
		if err := reg.RegisterAction(a); err != nil {
			return fmt.Errorf("计划任务登记 %s 失败: %w", a.Name, err)
		}
	}
	return nil
}

// RegisterScheduleActionsIfAvailable 给 ProductionDeps 用：没有接线就不登记。
func RegisterScheduleActionsIfAvailable(reg *Registry, src scheduleSource) error {
	if src == nil {
		return nil
	}
	return RegisterScheduleActions(reg, src)
}

// newScheduleApplyAction 登记 schedule.apply。
func newScheduleApplyAction(src scheduleSource) Action {
	return Action{
		Name: schedules.ActionApply,
		Decode: func(raw json.RawMessage) (any, error) {
			var p schedules.ApplyParams
			if err := DecodeParams(raw, &p); err != nil {
				return nil, err
			}
			if err := p.Validate(); err != nil {
				return nil, fmt.Errorf("%w: %v", ErrParamsInvalid, err)
			}
			return p, nil
		},
		Plan: func(ctx context.Context, in PlanInput) (Plan, error) {
			p, ok := in.Params.(schedules.ApplyParams)
			if !ok {
				return Plan{}, fmt.Errorf("%w: schedule.apply 的参数类型是 %T", ErrParamsInvalid, in.Params)
			}
			assessment, err := src.AssessApply(ctx, schedules.ApplyRequest{
				ScheduleID: p.ID(),
				Version:    p.Version,
				Definition: p.Definition,
			})
			if err != nil {
				return Plan{}, err
			}
			return schedulePlan(p, assessment, scheduleLocks(p.ID())), nil
		},
		Policy: ActionPolicy{RequiresReauth: true, Cancellable: false},
		Execute: func(ctx context.Context, in ExecuteInput) (Result, error) {
			p, ok := in.Params.(schedules.ApplyParams)
			if !ok {
				return Result{}, fmt.Errorf("%w: schedule.apply 的参数类型是 %T", ErrParamsInvalid, in.Params)
			}
			rec, err := src.Apply(ctx, schedules.ApplyRequest{
				ScheduleID: p.ID(),
				Version:    p.Version,
				Definition: p.Definition,
			})
			if err != nil {
				return Result{}, scheduleFailure{reason: err.Error()}
			}
			return Result{
				Outcome:    OutcomeVerified,
				VerifiedAt: in.Now,
				ObservedState: map[string]any{
					"schedule_id": rec.ID,
					"name":        rec.Name,
					"enabled":     rec.Enabled,
					"version":     rec.Version,
					"timer_ref":   rec.TimerRef,
				},
				RecoveryHint: scheduleApplyHint(rec),
				Data: map[string]any{
					"schedule_id": rec.ID,
					"version":     rec.Version,
				},
			}, nil
		},
	}
}

// newScheduleDeleteAction 登记 schedule.delete。
func newScheduleDeleteAction(src scheduleSource) Action {
	return Action{
		Name: schedules.ActionDelete,
		Decode: func(raw json.RawMessage) (any, error) {
			var p schedules.DeleteParams
			if err := DecodeParams(raw, &p); err != nil {
				return nil, err
			}
			if err := p.Validate(); err != nil {
				return nil, fmt.Errorf("%w: %v", ErrParamsInvalid, err)
			}
			return p, nil
		},
		Plan: func(ctx context.Context, in PlanInput) (Plan, error) {
			p, ok := in.Params.(schedules.DeleteParams)
			if !ok {
				return Plan{}, fmt.Errorf("%w: schedule.delete 的参数类型是 %T", ErrParamsInvalid, in.Params)
			}
			assessment, err := src.AssessDelete(ctx, p.ScheduleID, p.Version)
			if err != nil {
				return Plan{}, err
			}
			return schedulePlan(p, assessment, scheduleLocks(p.ScheduleID)), nil
		},
		Policy: ActionPolicy{RequiresReauth: true, Cancellable: false},
		Execute: func(ctx context.Context, in ExecuteInput) (Result, error) {
			p, ok := in.Params.(schedules.DeleteParams)
			if !ok {
				return Result{}, fmt.Errorf("%w: schedule.delete 的参数类型是 %T", ErrParamsInvalid, in.Params)
			}
			if err := src.Delete(ctx, p.ScheduleID, p.Version); err != nil {
				return Result{}, scheduleFailure{reason: err.Error()}
			}
			return Result{
				Outcome:       OutcomeVerified,
				VerifiedAt:    in.Now,
				ObservedState: map[string]any{"schedule_id": p.ScheduleID, "deleted": true},
				RecoveryHint:  "计划已删除；触发记录仍然保留在代理库里，用来回答「那一天到底跑没跑」",
			}, nil
		},
	}
}

// scheduleLocks 返回一条计划的资源锁；新建时（没有 id）不加锁。
func scheduleLocks(id string) []string {
	if id == "" {
		return nil
	}
	return []string{"schedule:" + id}
}

// schedulePlan 把计划模块的预检结论翻成登记表的 Plan。
//
// params 是解码后的 DTO：预检绑定的规范化参数就是它，提交时按它执行
// （CONTRACTS 4.2：提交那一刻不再接受新参数）。
func schedulePlan(params any, a schedules.Assessment, locks []string) Plan {
	warnings := make([]Notice, 0, len(a.Warnings))
	for _, n := range a.Warnings {
		warnings = append(warnings, Notice(n))
	}
	return Plan{
		NormalizedParams: params,
		Target: Target{
			DisplayName:         a.DisplayName,
			IdentityFingerprint: a.IdentityFingerprint,
			StateVersion:        a.StateVersion,
		},
		Allowed:  a.Allowed,
		Blockers: scheduleNotices(a.Blockers),
		Warnings: warnings,
		Impacts:  scheduleImpacts(a.Impacts),
		Locks:    locks,
	}
}

func scheduleNotices(in []schedules.Notice) []Notice {
	if len(in) == 0 {
		return nil
	}
	out := make([]Notice, 0, len(in))
	for _, n := range in {
		out = append(out, Notice(n))
	}
	return out
}

// scheduleNoticeSlice 把计划模块的阻塞项翻成代理的 Notice（空入参返回 nil）。
func scheduleNoticeSlice(in []schedules.Notice) []Notice { return scheduleNotices(in) }

func scheduleImpacts(in []schedules.Impact) []Impact {
	if len(in) == 0 {
		return nil
	}
	out := make([]Impact, 0, len(in))
	for _, i := range in {
		out = append(out, Impact(i))
	}
	return out
}

// scheduleApplyHint 给保存之后的人一句该看什么的话。
func scheduleApplyHint(rec schedules.Record) string {
	if !rec.Enabled {
		return "计划已停用：没有 timer 在计时，随时可以再启用"
	}
	return "已启用：" + rec.TimerRef + " 会在下次触发时刻以 root 身份调用 upanelctl schedule-trigger；" +
		"真实结果去任务列表里看（定时触发的任务由 Web 对账导入）"
}

// scheduleFailure 把计划模块已经核实的原因带进任务的 error.message。
type scheduleFailure struct{ reason string }

func (f scheduleFailure) Error() string { return f.reason }

// FailureReason 实现 FailureReasoner。
func (f scheduleFailure) FailureReason() string { return f.reason }

// scheduleTriggerHint 给定时触发失败时的一句提示。
func scheduleTriggerHint(code string) string {
	switch code {
	case "SCHEDULE_DISABLED":
		return "计划已停用；启用后下一次触发会自己开始"
	case "SCHEDULE_TARGET_NOT_FOUND":
		return "计划指向的对象已经不在了；改掉或删掉这条计划"
	case "SCHEDULE_TARGET_NOT_READY":
		return "对象当前不可用；处理完那个对象的问题后，下一次触发会自己重试"
	default:
		return "看代理日志里的 trigger_id，它与这次触发一一对应"
	}
}

// scheduleNow 把代理时间截断到秒（与预检/操作的落库精度一致）。
func scheduleNow(now time.Time) time.Time { return now.UTC().Truncate(time.Second) }
