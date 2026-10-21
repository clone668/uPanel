package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"upanel/internal/services"
)

// serviceControlParams 是 service.control 的参数（CONTRACTS.md 第 5 节）。
//
// 只有两个字段：单元名与操作。没有 params/args/flags 之类的自由字段，
// 也没有 “force”“now” 这类会被翻译成 systemctl 选项的开关。
type serviceControlParams struct {
	Unit      string `json:"unit"`
	Operation string `json:"operation"`
}

// newServiceControlAction 登记 service.control（P05.1 的第一个真实生产动作）。
//
// 它只做三件事：校验单元名与操作、按当前状态给出计划、执行一个固定的 systemctl 动词。
// 受保护单元在预检、执行前复核、执行器内三处都会被拒——不是只靠界面隐藏按钮。
func newServiceControlAction(src servicesSource) Action {
	return Action{
		Name: services.ActionControl,
		Decode: func(raw json.RawMessage) (any, error) {
			var p serviceControlParams
			if err := DecodeParams(raw, &p); err != nil {
				return nil, err
			}
			p.Unit = strings.TrimSpace(p.Unit)
			p.Operation = strings.ToLower(strings.TrimSpace(p.Operation))
			if err := services.ValidateUnitName(p.Unit); err != nil {
				return nil, fmt.Errorf("%w: %v", ErrParamsInvalid, err)
			}
			if !services.ValidOperation(p.Operation) {
				return nil, fmt.Errorf("%w: operation 只能是 start/stop/restart/enable/disable，收到 %q",
					ErrParamsInvalid, p.Operation)
			}
			return p, nil
		},
		Plan: func(ctx context.Context, in PlanInput) (Plan, error) {
			p, ok := in.Params.(serviceControlParams)
			if !ok {
				return Plan{}, fmt.Errorf("参数类型不匹配: %T", in.Params)
			}
			assessment, err := src.AssessControl(ctx, p.Unit, p.Operation)
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
				// 规范化参数：trim + 小写操作。预检绑定的是这一份，执行时按它解码。
				NormalizedParams: serviceControlParams{Unit: p.Unit, Operation: p.Operation},
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
		Policy: ActionPolicy{
			// CONTRACTS.md 第 5 节：service.control 不要求再认证，也不要求确认短语。
			// 启停是高频运维动作，每次都输密码会让人习惯性跳过验证。
			RequiresReauth: false,
			RequiresPhrase: false,
			// systemctl 已经开始的动作没有“安全取消”的语义：取消一个正在 stop 的服务
			// 只会让状态更不可知，所以任务不可取消。
			Cancellable: false,
		},
		Execute: func(ctx context.Context, in ExecuteInput) (Result, error) {
			p, ok := in.Params.(serviceControlParams)
			if !ok {
				return Result{}, fmt.Errorf("参数类型不匹配: %T", in.Params)
			}
			res, err := src.ExecuteControl(ctx, p.Unit, p.Operation, in.Progress)
			if err != nil {
				return Result{}, err
			}
			observed := map[string]any{
				"unit":                res.After.Unit,
				"operation":           res.Op,
				"active_state":        res.After.ActiveState,
				"sub_state":           res.After.SubState,
				"enablement":          res.After.Enablement,
				"unit_file_state":     res.After.UnitFileState,
				"expected":            res.Expectation,
				"verified":            res.Verified,
				"previous_state":      res.Before.ActiveState,
				"previous_enablement": res.Before.Enablement,
			}
			if res.After.MainPID != nil {
				observed["main_pid"] = *res.After.MainPID
			}
			data := map[string]any{
				"unit":        res.Unit,
				"operation":   res.Op,
				"exit_code":   res.ExitCode,
				"duration_ms": res.Duration.Milliseconds(),
			}
			if strings.TrimSpace(res.Stderr) != "" {
				data["stderr"] = strings.TrimSpace(res.Stderr)
			}
			if strings.TrimSpace(res.Stdout) != "" {
				data["stdout"] = strings.TrimSpace(res.Stdout)
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
