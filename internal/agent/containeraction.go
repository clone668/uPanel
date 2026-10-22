package agent

// P09.3：container.control（CONTRACTS.md 第 8.3 节）。
//
// 这是容器模块唯一的写动作，能做的只有 start / stop / restart 三个 docker 动词，
// 作用对象只能是**完整的 64 位容器 ID**——名字、镜像名、标签、compose 服务名
// 一律不接受，因为它们会被翻译成 argv 的一部分，而 argv 里多一个可写的字符串
// 就多一条把 docker 变成“任意命令执行器”的路。
//
// 危险容器（privileged、host net/pid/ipc、危险 capability、挂 docker.sock 或宿主根）
// 在预检里就被判为不接管：界面显示风险，不给按钮。这一点不靠 UI 隐藏按钮保证——
// 预检拒绝、执行前复核拒绝，两处都在代理里。

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"upanel/internal/containers"
	"upanel/internal/httpx"
)

// newContainerControlAction 登记 container.control。
func newContainerControlAction(src ContainersSource) Action {
	return Action{
		Name: containers.ActionControl,
		Decode: func(raw json.RawMessage) (any, error) {
			var params containers.ControlParams
			if err := DecodeParams(raw, &params); err != nil {
				return nil, err
			}
			if err := params.Normalize(); err != nil {
				// 参数错误是 400，不是 500：代理回 500 会被 Web 当成“代理不可用”。
				return nil, &containers.ValidationError{Message: err.Error()}
			}
			return params, nil
		},
		Plan: func(ctx context.Context, in PlanInput) (Plan, error) {
			params, ok := in.Params.(containers.ControlParams)
			if !ok {
				return Plan{}, fmt.Errorf("参数类型不匹配: %T", in.Params)
			}
			assessment, err := src.AssessControl(ctx, params)
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

			c := assessment.Container
			var impacts []Impact
			if assessment.Allowed {
				impacts = append(impacts, Impact{
					Kind:        "availability",
					Description: impactText(c, params.Operation),
				})
			}

			// 目标身份只由容器 ID 决定：容器被删除重建后 ID 会变，旧预检因此
			// 不会再作用到“同名的另一个容器”上。
			target := Target{
				DisplayName:         displayNameOf(c, params.ContainerID),
				IdentityFingerprint: "container:" + params.ContainerID,
			}
			if c.ID != "" {
				target.StateVersion = containers.StateVersion(c)
			}
			return Plan{
				NormalizedParams: params,
				Target:           target,
				Allowed:          assessment.Allowed,
				Blockers:         blockers,
				Warnings:         warnings,
				Impacts:          impacts,
				// 除容器自己外不加锁：启停一个容器不影响别的容器，
				// 而 docker 自己对同一个容器的并发操作是串行的。
				Locks: []string{"container:" + params.ContainerID},
			}, nil
		},
		Policy: ActionPolicy{
			// 启停是高频运维动作，不要求再认证，也不要求确认短语
			// （与 service.control 同一个判断：每次都输密码会让人习惯性跳过验证）。
			RequiresReauth: false,
			RequiresPhrase: false,
			// docker stop 已经在执行时取消只会让状态更不可知。
			Cancellable: false,
		},
		Execute: func(ctx context.Context, in ExecuteInput) (Result, error) {
			params, ok := in.Params.(containers.ControlParams)
			if !ok {
				return Result{}, fmt.Errorf("参数类型不匹配: %T", in.Params)
			}

			// 执行前复核两件事：还是同一个状态版本（预检之后没被重启/重建），
			// 以及现在依然允许这个操作（风险没有变化）。
			before, err := src.Get(ctx, params.ContainerID)
			if err != nil {
				return Result{}, err
			}
			if in.Target.StateVersion != "" && containers.StateVersion(before) != in.Target.StateVersion {
				return Result{}, &containers.StaleError{Detail: fmt.Sprintf("当前状态 %s",
					containers.ObservedStateText(before.State))}
			}
			assessment, err := src.AssessControl(ctx, params)
			if err != nil {
				return Result{}, err
			}
			if !assessment.Allowed {
				// 预检到执行之间被改成了危险容器（或状态不再适用）：拒绝执行，
				// 而不是照着旧预检硬上。
				return Result{}, &controlBlockedError{blockers: assessment.Blockers}
			}

			res, err := src.ExecuteControl(ctx, params, assessment.Container, in.Progress)
			if err != nil {
				return Result{}, err
			}

			observed := map[string]any{
				"container_id":   res.ContainerID,
				"operation":      res.Operation,
				"state":          containers.ObservedStateText(res.After.State),
				"expected":       res.Expectation,
				"verified":       res.Verified,
				"previous_state": containers.ObservedStateText(res.Before.State),
			}
			data := map[string]any{
				"container_id": res.ContainerID,
				"operation":    res.Operation,
				"exit_code":    res.ExitCode,
				"duration_ms":  res.Duration.Milliseconds(),
			}
			if s := strings.TrimSpace(res.Stdout); s != "" {
				data["stdout"] = s
			}
			if s := strings.TrimSpace(res.Stderr); s != "" {
				data["stderr"] = s
			}

			if res.Verified {
				return Result{
					Outcome:       OutcomeVerified,
					VerifiedAt:    time.Now().UTC(),
					ObservedState: observed,
					Data:          data,
				}, nil
			}
			// 退出码 0 但读回的状态不对：这是 unknown，不是成功。
			hint := res.VerifyNote
			if hint == "" {
				hint = "docker 已返回，但读回的状态与期望不一致；请再确认一次容器状态"
			}
			return Result{
				Outcome:       OutcomeUnknown,
				ObservedState: observed,
				RecoveryHint:  hint,
				Data:          data,
			}, nil
		},
	}
}

// controlBlockedError 表示执行时容器已不再允许该操作。
//
// 它与 StaleError 的区别在于原因：状态版本没变但风险变了（例如有人在预检之后
// 给容器加了 CAP_SYS_ADMIN 并重建——重建会换 ID，所以更常见的是被改成
// privileged 后重新 preflight 之前的窗口）。对外仍然是 409 PREFLIGHT_STALE：
// 对调用方来说，结局都是“重来一次预检”。
type controlBlockedError struct {
	blockers []containers.Block
}

func (e *controlBlockedError) Error() string {
	if len(e.blockers) == 0 {
		return "容器当前不允许这个操作，请重新预检"
	}
	parts := make([]string, 0, len(e.blockers))
	for _, b := range e.blockers {
		parts = append(parts, b.Message)
	}
	return "容器当前不允许这个操作：" + strings.Join(parts, "；")
}

func (e *controlBlockedError) Is(target error) bool { return target == containers.ErrStateChanged }

func (e *controlBlockedError) HTTPStatus() (int, httpx.Code) {
	return 409, httpx.CodePreflightStale
}

// displayNameOf 给出目标展示名：有容器名就用名字，没有就退回到短 ID。
func displayNameOf(c containers.Container, id string) string {
	if name := strings.TrimSpace(c.Name); name != "" {
		return name
	}
	return "容器 " + shortContainerID(id)
}

// impactText 说明这个操作会造成的后果。
func impactText(c containers.Container, op string) string {
	name := displayNameOf(c, c.ID)
	switch op {
	case containers.OpStop:
		return fmt.Sprintf("停止 %s：容器内的进程会被终止（优雅期 10 秒），依赖它的服务会中断", name)
	case containers.OpRestart:
		return fmt.Sprintf("重启 %s：进程终止后重新启动，期间服务不可用", name)
	default:
		return fmt.Sprintf("启动 %s：容器内进程开始运行，可能占用端口与资源", name)
	}
}

// shortContainerID 取 ID 前 12 位（与 docker 自己的短 ID 一致）。
func shortContainerID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
