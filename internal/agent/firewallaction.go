package agent

// 防火墙写动作（P14.3）：firewall.apply / firewall.confirm / firewall.rollback，
// 以及只读查询 firewall.config。
//
// 形状与 networkaction.go 一模一样，理由也一样：改防火墙同样能把管理员关在门外，
// 所以同样"先武装一条独立的 timer，再改，改完读回来核验，失败立刻退回"。这一层只做
// 三件事——解码、把判决翻成计划、把执行报告翻成结果。所有判断都在 internal/network
// 里，谁也不在别处再判一遍"能不能改"。
//
// 与网络那条路的一处区别：防火墙的 confirm **不接受 arrived_on**。网络那条要它，是因为
// 改地址可能把人从旧地址上踢下来，确认必须来自新地址；防火墙的预检里已经有一条
// FIREWALL_MANAGEMENT_PATH，它保证"改完之后面板与 SSH 仍然进得来"——被挡住的改动
// 根本走不到确认这一步，所以再要求一个来源地址，只是在给救命那一步加门槛。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"upanel/internal/network"
	"upanel/internal/syscmd"
)

// FirewallConfigQueryName 是"防火墙现在是什么样"的查询名。
const FirewallConfigQueryName = "firewall.config"

// firewallPlanBudget 是预检允许占用的时间。两次 ufw 调用加一次听筒扫描，
// 正常几十毫秒；给 20 秒是为了让"卡住了"变成一条明确的结论。
const firewallPlanBudget = 20 * time.Second

// firewallWritePrograms 是防火墙写路径允许执行的程序。**闭集**，与网络那条路分开：
// 改地址不需要 ufw，改防火墙也不需要 netplan。两个闭集各自只有两个程序。
var firewallWritePrograms = map[string]bool{"ufw": true, "systemctl": true}

// firewallWriteRunner 是 network.CommandRunner 的生产实现（防火墙版）。
type firewallWriteRunner struct{}

func (firewallWriteRunner) Run(ctx context.Context, spec syscmd.Spec, limits syscmd.Limits) (syscmd.Result, error) {
	base := filepath.Base(spec.Program)
	if !firewallWritePrograms[base] {
		return syscmd.Result{ExitCode: -1}, fmt.Errorf("%w: 防火墙写路径不允许执行 %q",
			network.ErrProgramMissing, spec.Program)
	}
	if filepath.Dir(spec.Program) == "." || !isFixedProgramDir(filepath.Dir(spec.Program)) {
		return syscmd.Result{ExitCode: -1}, fmt.Errorf("%w: %q 不在固定程序目录里",
			network.ErrProgramMissing, spec.Program)
	}
	return syscmd.Run(ctx, spec, limits)
}

// NewFirewallApplyEngine 组装生产用的防火墙写路径。
func NewFirewallApplyEngine(configPath string) (*network.FirewallEngine, error) {
	deps := network.SystemDeps()
	deps.Runner = networkRunner{}
	return network.NewFirewallEngine(network.FirewallOptions{
		Read:       deps,
		Runner:     firewallWriteRunner{},
		Executable: os.Executable,
		ConfigPath: configPath,
	})
}

// FirewallApplySource 是登记这三个动作与一条查询所需的能力。
type FirewallApplySource interface {
	Current(ctx context.Context) network.FirewallConfigState
	Assess(ctx context.Context, params network.FirewallApplyParams) (network.Assessment, error)
	AssessConfirm(ctx context.Context, params network.FirewallConfirmParams) (network.Assessment, error)
	AssessRollback(ctx context.Context, params network.FirewallRollbackParams) (network.Assessment, error)
	ExecuteWrite(ctx context.Context, req network.FirewallWriteRequest) (network.FirewallWriteReport, error)
}

// RegisterFirewallWriteActions 登记三个写动作。
func RegisterFirewallWriteActions(reg *Registry, src FirewallApplySource) error {
	if src == nil {
		return fmt.Errorf("防火墙写动作没有实现端（未接线时不要登记）")
	}
	for _, build := range []func(FirewallApplySource) Action{
		newFirewallApplyAction, newFirewallConfirmAction, newFirewallRollbackAction,
	} {
		action := build(src)
		if err := reg.RegisterAction(action); err != nil {
			return fmt.Errorf("防火墙登记 %s 失败: %w", action.Name, err)
		}
	}
	return nil
}

// RegisterFirewallConfigQuery 登记 firewall.config。
func RegisterFirewallConfigQuery(reg *Registry, src FirewallApplySource) error {
	if src == nil {
		return fmt.Errorf("%s 没有实现端（未接线时不要登记）", FirewallConfigQueryName)
	}
	if err := reg.RegisterQuery(newFirewallConfigQuery(src)); err != nil {
		return fmt.Errorf("防火墙登记 %s 失败: %w", FirewallConfigQueryName, err)
	}
	return nil
}

// RegisterFirewallWriteActionsIfAvailable 给装配层用：没有接线就不登记。
func RegisterFirewallWriteActionsIfAvailable(reg *Registry, src FirewallApplySource) error {
	if src == nil {
		return nil
	}
	return RegisterFirewallWriteActions(reg, src)
}

// RegisterFirewallConfigQueryIfAvailable 同上（只读查询）。
func RegisterFirewallConfigQueryIfAvailable(reg *Registry, src FirewallApplySource) error {
	if src == nil {
		return nil
	}
	return RegisterFirewallConfigQuery(reg, src)
}

// ---- 三个动作 ----

func newFirewallApplyAction(src FirewallApplySource) Action {
	return Action{
		Name:             network.FirewallActionApply,
		ParamsLimitBytes: 8 << 10, // 64 条规则 + 每一项的几个字段
		Decode: func(raw json.RawMessage) (any, error) {
			var p network.FirewallApplyParams
			if err := DecodeParams(raw, &p); err != nil {
				return nil, err
			}
			def, err := network.ValidateFirewallDefinition(p.Definition)
			if err != nil {
				return nil, fmt.Errorf("%w: %v", ErrParamsInvalid, err)
			}
			p.Definition = def
			return p, nil
		},
		Plan: func(ctx context.Context, in PlanInput) (Plan, error) {
			params, ok := in.Params.(network.FirewallApplyParams)
			if !ok {
				return Plan{}, fmt.Errorf("%w: firewall.apply 的参数类型是 %T", ErrParamsInvalid, in.Params)
			}
			assessment, err := src.Assess(ctx, params)
			if err != nil {
				return Plan{}, err
			}
			return firewallPlan(assessment), nil
		},
		// 再认证 + 确认短语：与 network.apply 同等门槛。它做的事同样能让人进不来，
		// 区别只是"进不来"的原因从网卡变成了规则。
		Policy: ActionPolicy{RequiresReauth: true, RequiresPhrase: true,
			PhraseVerb: "RECONFIGURE", Cancellable: false},
		PlanBudget: firewallPlanBudget,
		Execute: func(ctx context.Context, in ExecuteInput) (Result, error) {
			params, ok := in.Params.(network.FirewallApplyParams)
			if !ok {
				return Result{}, fmt.Errorf("%w: firewall.apply 的参数类型是 %T", ErrParamsInvalid, in.Params)
			}
			return firewallResultFromReport(src.ExecuteWrite(ctx, network.FirewallWriteRequest{
				Action:               network.FirewallActionApply,
				Params:               params,
				ExpectedIdentity:     in.Target.IdentityFingerprint,
				ExpectedStateVersion: in.Target.StateVersion,
				OperationID:          in.OperationID,
				Progress:             in.Progress,
			}))
		},
	}
}

func newFirewallConfirmAction(src FirewallApplySource) Action {
	return Action{
		Name:             network.FirewallActionConfirm,
		ParamsLimitBytes: 1 << 10,
		Decode: func(raw json.RawMessage) (any, error) {
			var p network.FirewallConfirmParams
			if err := DecodeParams(raw, &p); err != nil {
				return nil, err
			}
			return p, nil
		},
		Plan: func(ctx context.Context, in PlanInput) (Plan, error) {
			params, ok := in.Params.(network.FirewallConfirmParams)
			if !ok {
				return Plan{}, fmt.Errorf("%w: firewall.confirm 的参数类型是 %T", ErrParamsInvalid, in.Params)
			}
			assessment, err := src.AssessConfirm(ctx, params)
			if err != nil {
				return Plan{}, err
			}
			return firewallPlan(assessment), nil
		},
		// 救命的两步之一：门槛越高越救不回来。
		Policy:     ActionPolicy{Cancellable: false},
		PlanBudget: firewallPlanBudget,
		Execute: func(ctx context.Context, in ExecuteInput) (Result, error) {
			params, ok := in.Params.(network.FirewallConfirmParams)
			if !ok {
				return Result{}, fmt.Errorf("%w: firewall.confirm 的参数类型是 %T", ErrParamsInvalid, in.Params)
			}
			return firewallResultFromReport(src.ExecuteWrite(ctx, network.FirewallWriteRequest{
				Action:               network.FirewallActionConfirm,
				Params:               params,
				ExpectedIdentity:     in.Target.IdentityFingerprint,
				ExpectedStateVersion: in.Target.StateVersion,
				OperationID:          in.OperationID,
				Progress:             in.Progress,
			}))
		},
	}
}

func newFirewallRollbackAction(src FirewallApplySource) Action {
	return Action{
		Name:             network.FirewallActionRollback,
		ParamsLimitBytes: 1 << 10,
		Decode: func(raw json.RawMessage) (any, error) {
			var p network.FirewallRollbackParams
			if err := DecodeParams(raw, &p); err != nil {
				return nil, err
			}
			return p, nil
		},
		Plan: func(ctx context.Context, in PlanInput) (Plan, error) {
			params, ok := in.Params.(network.FirewallRollbackParams)
			if !ok {
				return Plan{}, fmt.Errorf("%w: firewall.rollback 的参数类型是 %T", ErrParamsInvalid, in.Params)
			}
			assessment, err := src.AssessRollback(ctx, params)
			if err != nil {
				return Plan{}, err
			}
			return firewallPlan(assessment), nil
		},
		Policy:     ActionPolicy{Cancellable: false},
		PlanBudget: firewallPlanBudget,
		Execute: func(ctx context.Context, in ExecuteInput) (Result, error) {
			params, ok := in.Params.(network.FirewallRollbackParams)
			if !ok {
				return Result{}, fmt.Errorf("%w: firewall.rollback 的参数类型是 %T", ErrParamsInvalid, in.Params)
			}
			return firewallResultFromReport(src.ExecuteWrite(ctx, network.FirewallWriteRequest{
				Action:               network.FirewallActionRollback,
				Params:               params,
				ExpectedIdentity:     in.Target.IdentityFingerprint,
				ExpectedStateVersion: in.Target.StateVersion,
				OperationID:          in.OperationID,
				Progress:             in.Progress,
			}))
		},
	}
}

// ---- 查询 ----

type firewallConfigParams struct{}

func newFirewallConfigQuery(src FirewallApplySource) Query {
	return Query{
		Name: FirewallConfigQueryName,
		Decode: func(raw json.RawMessage) (any, error) {
			var params firewallConfigParams
			if err := DecodeParams(raw, &params); err != nil {
				return nil, err
			}
			return params, nil
		},
		Run: func(ctx context.Context, _ any) (any, error) {
			return src.Current(ctx), nil
		},
	}
}

// ---- 判决与结果的翻译 ----

// firewallPlan 把 network.Assessment 翻成代理的计划结构。只翻，不加料。
func firewallPlan(a network.Assessment) Plan {
	out := Plan{
		NormalizedParams: a.Normalized,
		Target: Target{
			DisplayName:         a.DisplayName,
			IdentityFingerprint: a.IdentityFingerprint,
			StateVersion:        a.StateVersion,
		},
		Allowed:  a.Allowed,
		Blockers: make([]Notice, 0, len(a.Blockers)),
		Warnings: make([]Notice, 0, len(a.Warnings)),
		Impacts:  make([]Impact, 0, len(a.Impacts)),
		Locks:    a.Locks,
	}
	for _, b := range a.Blockers {
		out.Blockers = append(out.Blockers, Notice{Code: b.Code, Message: b.Message})
	}
	for _, w := range a.Warnings {
		out.Warnings = append(out.Warnings, Notice{Code: w.Code, Message: w.Message})
	}
	for _, i := range a.Impacts {
		out.Impacts = append(out.Impacts, Impact{Kind: i.Kind, Description: i.Description})
	}
	if !out.Allowed && len(out.Blockers) == 0 {
		out.Blockers = append(out.Blockers, Notice{Code: "FIREWALL_UNSUPPORTED",
			Message: "防火墙写入被拒绝，但没有给出原因（这是缺陷，请报告）"})
	}
	return out
}

// firewallResultFromReport 把执行报告翻成代理结果。
//
// 三个 outcome 原样传：verified / rolled_back / unknown。防火墙这一层没有 failed，
// 因为"改失败了"最终都会落到"退回去了"或"不知道现在什么样"这两种事实之一。
func firewallResultFromReport(report network.FirewallWriteReport, err error) (Result, error) {
	if err != nil {
		return Result{}, err
	}
	out := Result{
		ObservedState: report.Observed,
		Data:          report.Data,
		RecoveryHint:  report.RecoveryHint,
	}
	switch report.Outcome {
	case network.OutcomeVerified:
		out.Outcome = OutcomeVerified
		out.VerifiedAt = time.Now().UTC().Truncate(time.Second)
	case network.OutcomeRolledBack:
		out.Outcome = OutcomeRolledBack
	case network.OutcomeUnknown:
		out.Outcome = OutcomeUnknown
	default:
		return Result{}, fmt.Errorf("%w: 防火墙执行器返回了未知 outcome %q",
			ErrInvalidResult, report.Outcome)
	}
	return out, nil
}
