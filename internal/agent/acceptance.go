package agent

// 验收专用动作表（P03.4 要求的“独立测试注册表”的先行部分）。
//
// 这张表**不在生产登记表里**：`NewProductionRegistry` 依旧返回空表，只有显式传入
// `upanel-agent -acceptance-actions` 时才会被构造并装进代理。它的用途是让 P03 的
// 预检/任务链路能在两台验收 VM 上端到端跑通，而不必为了测试提前把某个真实磁盘动作
// 塞进生产白名单。
//
// 硬约束：
//   - 动作名一律以 `acceptance.` 开头，一眼可辨；
//   - 预检器**不读也不写任何系统资源**，只返回固定的计划；
//   - 计划里带 `acceptance_only` 告警，界面上不可能被误认为真实操作。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// AcceptanceActionPrefix 是验收动作的固定前缀。
const AcceptanceActionPrefix = "acceptance."

// 验收动作的执行时长。
//
// 它们不读也不写系统，耗时只是为了让“接受 / 运行中 / 完成”三个阶段在真实系统上
// 有可观察的间隔：验收脚本要能证明任务确实跑过、取消确实生效。
const (
	probeDuration = 100 * time.Millisecond
	pingDuration  = 2 * time.Second
	pingSlice     = 50 * time.Millisecond
)

// probeParams 是 acceptance.probe 的参数。
type probeParams struct {
	// Label 是可选标签，用来证明参数规范化与哈希绑定确实按内容区分。
	Label string `json:"label"`
}

// pingParams 是 acceptance.ping 的参数。
type pingParams struct {
	Note string `json:"note"`
}

// NewAcceptanceRegistry 返回验收专用动作表。
//
// 调用方必须把“为什么启用了验收动作”写进日志与部署记录；生产环境不得传这个表。
func NewAcceptanceRegistry() *Registry {
	r := NewRegistry()
	// 登记失败属于编程错误：这里直接 panic 而不是返回半张表，
	// 因为一张缺项的动作表会让验收在别处莫名失败。
	mustRegister := func(a Action) {
		if err := r.RegisterAction(a); err != nil {
			panic(fmt.Sprintf("agent: 验收动作登记失败: %v", err))
		}
	}

	mustRegister(Action{
		Name: "acceptance.probe",
		Decode: func(raw json.RawMessage) (any, error) {
			var p probeParams
			if err := DecodeParams(raw, &p); err != nil {
				return nil, err
			}
			if len(p.Label) > 32 {
				return nil, fmt.Errorf("%w: label 最多 32 个字符", ErrParamsInvalid)
			}
			return p, nil
		},
		Plan: func(_ context.Context, in PlanInput) (Plan, error) {
			p, ok := in.Params.(probeParams)
			if !ok {
				return Plan{}, fmt.Errorf("参数类型不匹配: %T", in.Params)
			}
			return Plan{
				// 规范化参数：空标签统一成空串，便于证明“规范化后的参数”才是被绑定的那一份。
				NormalizedParams: probeParams{Label: p.Label},
				Target: Target{
					DisplayName:         "验收测试对象（" + labelOrNone(p.Label) + "）",
					IdentityFingerprint: fingerprint("acceptance/probe/v1", p.Label),
					StateVersion:        "1",
				},
				Allowed:  true,
				Blockers: nil,
				Warnings: []Notice{{
					Code:    "acceptance_only",
					Message: "这是验收专用动作，不会对系统做任何变更",
				}},
				Impacts: []Impact{{
					Kind:        "none",
					Description: "该动作不修改任何系统状态",
				}},
				Locks: []string{"acceptance:probe"},
			}, nil
		},
		Policy: ActionPolicy{
			RequiresReauth: true,
			RequiresPhrase: true,
			PhraseVerb:     "PROBE",
			Cancellable:    false,
		},
		Execute: func(ctx context.Context, in ExecuteInput) (Result, error) {
			p, ok := in.Params.(probeParams)
			if !ok {
				return Result{}, fmt.Errorf("参数类型不匹配: %T", in.Params)
			}
			// 固定耗时：让“接受→运行中→完成”这三个阶段在真实系统上有可见的间隔，
			// 验收脚本才能观察到 running。不读写任何系统资源。
			select {
			case <-time.After(probeDuration):
			case <-ctx.Done():
				return Result{}, fmt.Errorf("%w: %v", ErrCanceled, ctx.Err())
			}
			if in.Progress != nil {
				in.Progress("probe_done", map[string]any{"label": p.Label})
			}
			return Result{
				Outcome:    OutcomeVerified,
				VerifiedAt: time.Now().UTC(),
				ObservedState: map[string]any{
					"label":        p.Label,
					"did_anything": false,
				},
				Data: map[string]any{
					"operation_id":  in.OperationID,
					"preflight_ref": in.PreflightRef,
				},
			}, nil
		},
	})

	mustRegister(Action{
		Name: "acceptance.ping",
		Decode: func(raw json.RawMessage) (any, error) {
			var p pingParams
			if err := DecodeParams(raw, &p); err != nil {
				return nil, err
			}
			return p, nil
		},
		Plan: func(_ context.Context, in PlanInput) (Plan, error) {
			p, ok := in.Params.(pingParams)
			if !ok {
				return Plan{}, fmt.Errorf("参数类型不匹配: %T", in.Params)
			}
			return Plan{
				NormalizedParams: pingParams{Note: p.Note},
				Target: Target{
					DisplayName:         "验收测试对象（ping）",
					IdentityFingerprint: fingerprint("acceptance/ping/v1", ""),
					StateVersion:        "1",
				},
				Allowed:  true,
				Warnings: []Notice{{Code: "acceptance_only", Message: "这是验收专用动作，不会对系统做任何变更"}},
				Impacts:  []Impact{{Kind: "none", Description: "该动作不修改任何系统状态"}},
				Locks:    []string{"acceptance:ping"},
			}, nil
		},
		Policy: ActionPolicy{
			// 对照项：不要求再认证、不要求短语、可安全取消。
			RequiresReauth: false,
			RequiresPhrase: false,
			Cancellable:    true,
		},
		Execute: func(ctx context.Context, in ExecuteInput) (Result, error) {
			// 可取消动作：在可中断点检查 Cancel，收到请求就返回 ErrCanceled。
			// 用固定时长（而不是参数）是为了让“提交后掉线仍能查到任务”有真实的执行窗口。
			deadline := time.Now().Add(pingDuration)
			for time.Now().Before(deadline) {
				select {
				case <-in.Cancel:
					return Result{}, ErrCanceled
				case <-ctx.Done():
					return Result{}, fmt.Errorf("%w: %v", ErrCanceled, ctx.Err())
				case <-time.After(pingSlice):
				}
			}
			if in.Progress != nil {
				in.Progress("ping_done", map[string]any{"slices": int(pingDuration / pingSlice)})
			}
			return Result{
				Outcome:       OutcomeVerified,
				VerifiedAt:    time.Now().UTC(),
				ObservedState: map[string]any{"did_anything": false},
			}, nil
		},
	})
	return r
}

func labelOrNone(label string) string {
	if label == "" {
		return "无标签"
	}
	return label
}

// fingerprint 生成稳定指纹：只用来区分目标，不用来保密。
func fingerprint(prefix, value string) string {
	sum := sha256.Sum256([]byte(prefix + "|" + value))
	return hex.EncodeToString(sum[:16])
}
