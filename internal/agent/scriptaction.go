package agent

// 受管理脚本动作（P12.4）：script.apply / script.delete / script.run。
//
// 三条动作的共同点是"永远不获得 root"：apply/delete 只写固定目录里 root 所有的文件，
// run 用内核 setuid 语义降权到 upanel-shell。这不是策略选择，是 CONTRACTS 第 5 节
// 写死的边界——一个"以 root 跑我的脚本"的入口，等于把整套权限模型绕开。
//
// script.run 可取消：执行器把 in.Cancel 接到自己的 ctx 上，取消时 syscmd 会杀掉整个
// 进程组。一个跑一小时的脚本不能被取消，等于"点错了只能等它跑完"。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"

	"upanel/internal/scripts"
	"upanel/internal/syscmd"
)

// scriptSource 是脚本动作需要的全部能力（生产实现是 *scripts.Module）。
type scriptSource interface {
	AssessApply(ctx context.Context, req scripts.ApplyRequest) (scripts.Assessment, error)
	Apply(ctx context.Context, req scripts.ApplyRequest) (scripts.Record, error)
	AssessDelete(ctx context.Context, id string, version int64) (scripts.Assessment, error)
	Delete(ctx context.Context, id string, version int64) error
	AssessRun(ctx context.Context, req scripts.RunRequest) (scripts.Assessment, error)
	Run(ctx context.Context, req scripts.RunRequest) (scripts.RunOutcome, error)
}

// scriptApplyParamsLimit 是 script.apply 的参数上限：正文上限加上名字与信封的余量。
const scriptApplyParamsLimit = scripts.MaxContentBytes + 8<<10

// RegisterScriptActions 登记脚本动作。
func RegisterScriptActions(reg *Registry, src scriptSource) error {
	if src == nil {
		return fmt.Errorf("agent: 脚本动作没有实现端")
	}
	for _, a := range []Action{
		newScriptApplyAction(src),
		newScriptDeleteAction(src),
		newScriptRunAction(src),
	} {
		if err := reg.RegisterAction(a); err != nil {
			return fmt.Errorf("脚本登记 %s 失败: %w", a.Name, err)
		}
	}
	return nil
}

// RegisterScriptActionsIfAvailable 给 ProductionDeps 用。
func RegisterScriptActionsIfAvailable(reg *Registry, src scriptSource) error {
	if src == nil {
		return nil
	}
	return RegisterScriptActions(reg, src)
}

// newScriptApplyAction 登记 script.apply。
func newScriptApplyAction(src scriptSource) Action {
	return Action{
		Name:             scripts.ActionScriptApply,
		ParamsLimitBytes: scriptApplyParamsLimit,
		Decode: func(raw json.RawMessage) (any, error) {
			var p scripts.ApplyParams
			if err := DecodeParamsLimit(raw, &p, scriptApplyParamsLimit); err != nil {
				return nil, err
			}
			if err := p.Validate(); err != nil {
				return nil, fmt.Errorf("%w: %v", ErrParamsInvalid, err)
			}
			// 正文规范化（CRLF → LF）放在解码这一步：预检绑定的参数与落盘的字节
			// 必须是同一份，否则"批准的内容"和"能跑的内容"会差一个回车。
			p.Name = strings.TrimSpace(p.Name)
			p.Content = strings.ReplaceAll(p.Content, "\r\n", "\n")
			return p, nil
		},
		Plan: func(ctx context.Context, in PlanInput) (Plan, error) {
			p, ok := in.Params.(scripts.ApplyParams)
			if !ok {
				return Plan{}, fmt.Errorf("%w: script.apply 的参数类型是 %T", ErrParamsInvalid, in.Params)
			}
			assessment, err := src.AssessApply(ctx, scripts.ApplyRequest{
				ScriptID: p.ID(),
				Version:  p.Version,
				Input:    scripts.Input{Name: p.Name, Content: p.Content},
			})
			if err != nil {
				return Plan{}, err
			}
			return scriptPlan(p, assessment, scriptLocks(p.ID()), nil), nil
		},
		Policy: ActionPolicy{RequiresReauth: true, Cancellable: false},
		Execute: func(ctx context.Context, in ExecuteInput) (Result, error) {
			p, ok := in.Params.(scripts.ApplyParams)
			if !ok {
				return Result{}, fmt.Errorf("%w: script.apply 的参数类型是 %T", ErrParamsInvalid, in.Params)
			}
			rec, err := src.Apply(ctx, scripts.ApplyRequest{
				ScriptID: p.ID(),
				Version:  p.Version,
				Input:    scripts.Input{Name: p.Name, Content: p.Content},
			})
			if err != nil {
				return Result{}, scriptFailure{reason: err.Error()}
			}
			return Result{
				Outcome:    OutcomeVerified,
				VerifiedAt: in.Now,
				ObservedState: map[string]any{
					"script_id":    rec.ID,
					"name":         rec.Name,
					"version":      rec.Version,
					"content_hash": rec.ContentHash,
					"path":         rec.Ref,
					"bytes":        len(rec.Content),
				},
				RecoveryHint: "脚本已以 root 所有、" + scripts.ShellUser + " 只读可执行的方式落盘；" +
					"运行它（或让它被计划引用）时会再核一次内容指纹",
				Data: map[string]any{"script_id": rec.ID, "version": rec.Version},
			}, nil
		},
	}
}

// newScriptDeleteAction 登记 script.delete。
func newScriptDeleteAction(src scriptSource) Action {
	return Action{
		Name: scripts.ActionScriptDelete,
		Decode: func(raw json.RawMessage) (any, error) {
			var p scripts.DeleteParams
			if err := DecodeParams(raw, &p); err != nil {
				return nil, err
			}
			if err := p.Validate(); err != nil {
				return nil, fmt.Errorf("%w: %v", ErrParamsInvalid, err)
			}
			return p, nil
		},
		Plan: func(ctx context.Context, in PlanInput) (Plan, error) {
			p, ok := in.Params.(scripts.DeleteParams)
			if !ok {
				return Plan{}, fmt.Errorf("%w: script.delete 的参数类型是 %T", ErrParamsInvalid, in.Params)
			}
			assessment, err := src.AssessDelete(ctx, p.ScriptID, p.Version)
			if err != nil {
				return Plan{}, err
			}
			return scriptPlan(p, assessment, scriptLocks(p.ScriptID), nil), nil
		},
		Policy: ActionPolicy{RequiresReauth: true, Cancellable: false},
		Execute: func(ctx context.Context, in ExecuteInput) (Result, error) {
			p, ok := in.Params.(scripts.DeleteParams)
			if !ok {
				return Result{}, fmt.Errorf("%w: script.delete 的参数类型是 %T", ErrParamsInvalid, in.Params)
			}
			if err := src.Delete(ctx, p.ScriptID, p.Version); err != nil {
				return Result{}, scriptFailure{reason: err.Error()}
			}
			return Result{
				Outcome:       OutcomeVerified,
				VerifiedAt:    in.Now,
				ObservedState: map[string]any{"script_id": p.ScriptID, "deleted": true},
				RecoveryHint:  "脚本记录与文件都已删除",
			}, nil
		},
	}
}

// newScriptRunAction 登记 script.run。
func newScriptRunAction(src scriptSource) Action {
	return Action{
		Name: scripts.ActionScriptRun,
		Decode: func(raw json.RawMessage) (any, error) {
			var p scripts.RunParams
			if err := DecodeParams(raw, &p); err != nil {
				return nil, err
			}
			if err := p.Validate(); err != nil {
				return nil, fmt.Errorf("%w: %v", ErrParamsInvalid, err)
			}
			return p, nil
		},
		Plan: func(ctx context.Context, in PlanInput) (Plan, error) {
			p, ok := in.Params.(scripts.RunParams)
			if !ok {
				return Plan{}, fmt.Errorf("%w: script.run 的参数类型是 %T", ErrParamsInvalid, in.Params)
			}
			assessment, err := src.AssessRun(ctx, scripts.RunRequest{
				ScriptID:          p.ScriptID,
				Version:           p.Version,
				MaxRuntimeSeconds: p.MaxRuntimeSeconds,
			})
			if err != nil {
				return Plan{}, err
			}
			return scriptPlan(p, assessment, scriptLocks(p.ScriptID), nil), nil
		},
		// 再认证是必需的：跑一段脚本是"现在就让它在这台机器上做事"，与保存脚本
		// 同级的门槛，而不是"保存过就可以随时跑"。
		Policy: ActionPolicy{RequiresReauth: true, Cancellable: true},
		Execute: func(ctx context.Context, in ExecuteInput) (Result, error) {
			p, ok := in.Params.(scripts.RunParams)
			if !ok {
				return Result{}, fmt.Errorf("%w: script.run 的参数类型是 %T", ErrParamsInvalid, in.Params)
			}
			runCtx, cancelRun := context.WithCancel(ctx)
			defer cancelRun()
			if in.Cancel != nil {
				stop := make(chan struct{})
				defer close(stop)
				go func() {
					select {
					case <-in.Cancel:
						// 取消 → ctx 结束 → syscmd 杀掉整个进程组。
						cancelRun()
					case <-stop:
					}
				}()
			}
			out, err := src.Run(runCtx, scripts.RunRequest{
				ScriptID:          p.ScriptID,
				Version:           p.Version,
				MaxRuntimeSeconds: p.MaxRuntimeSeconds,
			})
			if err != nil {
				if errors.Is(err, context.Canceled) {
					return Result{}, ErrCanceled
				}
				if errors.Is(err, syscmd.ErrTimeout) {
					return Result{}, scriptFailure{reason: fmt.Sprintf("脚本 %q 超过 %d 秒被终止",
						scriptNameOf(out), p.MaxRuntimeSeconds)}
				}
				return Result{}, scriptFailure{reason: err.Error()}
			}
			observed := map[string]any{
				"script_id":   p.ScriptID,
				"exit_code":   out.ExitCode,
				"duration_ms": out.Duration.Milliseconds(),
				"stdout":      len(out.Stdout),
				"stderr":      len(out.Stderr),
				"truncated":   out.Truncated,
			}
			if out.ExitCode != 0 {
				return Result{}, scriptFailure{reason: fmt.Sprintf("脚本 %q 以退出状态 %d 结束：%s",
					scriptNameOf(out), out.ExitCode, outputTail(out.Stderr))}
			}
			return Result{
				Outcome:       OutcomeVerified,
				VerifiedAt:    in.Now,
				ObservedState: observed,
				RecoveryHint:  "脚本以 " + scripts.ShellUser + " 身份运行并以退出状态 0 结束；输出不落库，需要时写日志文件",
			}, nil
		},
	}
}

// scriptPlan 把脚本模块的预检结论翻成登记表的 Plan。
func scriptPlan(params any, a scripts.Assessment, locks []string, extra []Notice) Plan {
	warnings := make([]Notice, 0, len(a.Warnings))
	for _, n := range a.Warnings {
		warnings = append(warnings, Notice(n))
	}
	warnings = append(warnings, extra...)
	blockers := make([]Notice, 0, len(a.Blockers))
	for _, n := range a.Blockers {
		blockers = append(blockers, Notice(n))
	}
	impacts := make([]Impact, 0, len(a.Impacts))
	for _, i := range a.Impacts {
		impacts = append(impacts, Impact(i))
	}
	return Plan{
		NormalizedParams: params,
		Target: Target{
			DisplayName:         a.DisplayName,
			IdentityFingerprint: a.IdentityFingerprint,
			StateVersion:        a.StateVersion,
		},
		Allowed:  a.Allowed,
		Blockers: blockers,
		Warnings: warnings,
		Impacts:  impacts,
		Locks:    locks,
	}
}

// scriptLocks 返回一条脚本的资源锁。
func scriptLocks(id string) []string {
	if id == "" {
		return nil
	}
	return []string{"script:" + id}
}

// scriptNameOf 在只有运行结果时尽力给出脚本名（错误文案里要用）。
func scriptNameOf(out scripts.RunOutcome) string {
	if out.ScriptName != "" {
		return out.ScriptName
	}
	return "脚本"
}

// outputTail 取脚本输出的第一行可读文本，去掉控制字符并截断。
//
// 它进的是任务的 error.message：既要让人看懂脚本说了什么，也不能把整段输出
// （可能包含路径、密钥片段）塞进任务记录里。
func outputTail(s string) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(strings.Map(func(r rune) rune {
			if unicode.IsControl(r) {
				return -1
			}
			return r
		}, line))
		if line == "" {
			continue
		}
		if len(line) > 160 {
			line = line[:160] + "…"
		}
		return line
	}
	return "（没有输出）"
}

// scriptFailure 把脚本模块已经核实的原因带进任务的 error.message。
type scriptFailure struct{ reason string }

func (f scriptFailure) Error() string { return f.reason }

// FailureReason 实现 FailureReasoner。
func (f scriptFailure) FailureReason() string { return f.reason }
