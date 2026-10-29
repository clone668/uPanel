package agent

// 网络写动作（P14.2）：network.apply / network.confirm / network.rollback，以及
// 只读查询 network.config。
//
// 这三个名字背后的东西和别的写动作不一样，所以接线的形状也不一样：apply 会在改完之后
// **留一个独立的 timer** 把配置退回去，而"确认"与"回滚"正是处理那条遗留的两条路。
// 接线层因此只做三件事——解码、把判决翻成计划、把执行报告翻成结果；所有判断都在
// internal/network 里，谁都不在别处再判一遍"能不能改"。
//
// 有一件事特意**不**做：不在这里检查"是不是 root 该做的事"。代理本来就以 root 运行，
// 权限边界由动作登记表与 POLICY 决定，不由调用点的 if 决定。

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

// NetworkConfigQueryName 是"可改网卡现在是什么样"的查询名。
const NetworkConfigQueryName = "network.config"

// networkApplyPlanBudget 是预检允许占用的时间。
//
// 预检要读 netplan 目录、跑几次 `ip`、读 /run 里的渲染产物：正常几十毫秒。给 20 秒
// 是因为它和别的动作共用同一把秒表——网络栈卡住时，我们要的是"预检超时"这条明确的
// 结论，而不是一条挂到 HTTP 超时为止的连接。
const networkApplyPlanBudget = 20 * time.Second

// networkWriteProgramDirs 是写路径允许找到 netplan/systemctl 的目录，顺序固定。
var networkWriteProgramDirs = []string{"/usr/sbin", "/sbin", "/usr/bin", "/bin"}

// networkWritePrograms 是写路径允许执行的程序。**闭集**：任何人想加第三个程序，
// 都必须先改这里——不允许把参数或配置里的字符串变成"要跑的程序"。
var networkWritePrograms = map[string]bool{"netplan": true, "systemctl": true}

// networkWriteRunner 是 network.CommandRunner 的生产实现。
//
// 与只读那侧的 networkRunner 分开：它要拿到 syscmd 的完整结果（退出码、stderr、超时），
// 因为"netplan apply 说了什么"是执行报告的一部分；而只读那侧只需要 stdout。
type networkWriteRunner struct{}

func (networkWriteRunner) Run(ctx context.Context, spec syscmd.Spec, limits syscmd.Limits) (syscmd.Result, error) {
	base := filepath.Base(spec.Program)
	if !networkWritePrograms[base] {
		return syscmd.Result{ExitCode: -1}, fmt.Errorf("%w: 写路径不允许执行 %q",
			network.ErrProgramMissing, spec.Program)
	}
	if filepath.Dir(spec.Program) == "." || !isFixedProgramDir(filepath.Dir(spec.Program)) {
		return syscmd.Result{ExitCode: -1}, fmt.Errorf("%w: %q 不在固定程序目录里",
			network.ErrProgramMissing, spec.Program)
	}
	return syscmd.Run(ctx, spec, limits)
}

func isFixedProgramDir(dir string) bool {
	for _, allowed := range networkWriteProgramDirs {
		if dir == allowed {
			return true
		}
	}
	return false
}

// NewNetworkApplyEngine 组装生产用的网络写路径。
//
// configPath 只会被写进回滚记录与 systemd 单元的命令行——那条命令是代理自己在
// 回滚时执行的，所以它必须记下"从哪个配置文件启动"。
func NewNetworkApplyEngine(configPath string) (*network.ApplyEngine, error) {
	deps := network.SystemDeps()
	deps.Runner = networkRunner{}
	return network.NewApplyEngine(network.ApplyOptions{
		Read:       deps,
		Runner:     networkWriteRunner{},
		Executable: os.Executable,
		ConfigPath: configPath,
	})
}

// NetworkApplySource 是登记这三个动作与一条查询所需的能力。
//
// 生产实现是 *network.ApplyEngine；测试给假实现。定义在消费方，和其余模块一致。
type NetworkApplySource interface {
	Current(ctx context.Context) network.ConfigState
	Assess(ctx context.Context, params network.ApplyParams) (network.Assessment, error)
	AssessConfirm(ctx context.Context, params network.ConfirmParams) (network.Assessment, error)
	AssessRollback(ctx context.Context, params network.RollbackParams) (network.Assessment, error)
	ExecuteWrite(ctx context.Context, req network.WriteRequest) (network.WriteReport, error)
}

// RegisterNetworkWriteActions 登记三个写动作。
//
// 没有实现端就直接报错：这三个动作的 Plan/Execute 会立刻调 src，把一个 nil 记进去
// 等于给自己埋一个"点了才 panic"的入口——装配错误应当在启动日志里出现。
func RegisterNetworkWriteActions(reg *Registry, src NetworkApplySource) error {
	if src == nil {
		return fmt.Errorf("网络写动作没有实现端（未接线时不要登记）")
	}
	for _, build := range []func(NetworkApplySource) Action{
		newNetworkApplyAction, newNetworkConfirmAction, newNetworkRollbackAction,
	} {
		action := build(src)
		if err := reg.RegisterAction(action); err != nil {
			return fmt.Errorf("网络登记 %s 失败: %w", action.Name, err)
		}
	}
	return nil
}

// RegisterNetworkConfigQuery 登记 network.config。
func RegisterNetworkConfigQuery(reg *Registry, src NetworkApplySource) error {
	if src == nil {
		return fmt.Errorf("%s 没有实现端（未接线时不要登记）", NetworkConfigQueryName)
	}
	if err := reg.RegisterQuery(newNetworkConfigQuery(src)); err != nil {
		return fmt.Errorf("网络登记 %s 失败: %w", NetworkConfigQueryName, err)
	}
	return nil
}

// RegisterNetworkWriteActionsIfAvailable 给装配层用：没有接线就不登记。
//
// 没接上写路径时这三个动作**不存在**：capabilities 不会报告它们，Web 侧对应的
// 预检入口回 503，而不是接受一个永远跑不起来的任务。
func RegisterNetworkWriteActionsIfAvailable(reg *Registry, src NetworkApplySource) error {
	if src == nil {
		return nil
	}
	return RegisterNetworkWriteActions(reg, src)
}

// RegisterNetworkConfigQueryIfAvailable 同上（只读查询）。
func RegisterNetworkConfigQueryIfAvailable(reg *Registry, src NetworkApplySource) error {
	if src == nil {
		return nil
	}
	return RegisterNetworkConfigQuery(reg, src)
}

// ---- 三个动作 ----

func newNetworkApplyAction(src NetworkApplySource) Action {
	return Action{
		Name:             network.ActionApply,
		ParamsLimitBytes: 4 << 10,
		Decode: func(raw json.RawMessage) (any, error) {
			var p network.ApplyParams
			if err := DecodeParams(raw, &p); err != nil {
				return nil, err
			}
			def, err := network.ValidateDefinition(p.Definition)
			if err != nil {
				return nil, fmt.Errorf("%w: %v", ErrParamsInvalid, err)
			}
			p.Definition = def
			return p, nil
		},
		Plan: func(ctx context.Context, in PlanInput) (Plan, error) {
			params, ok := in.Params.(network.ApplyParams)
			if !ok {
				return Plan{}, fmt.Errorf("%w: network.apply 的参数类型是 %T", ErrParamsInvalid, in.Params)
			}
			assessment, err := src.Assess(ctx, params)
			if err != nil {
				return Plan{}, err
			}
			return networkPlan(assessment), nil
		},
		// 再认证 + 确认短语：这是唯一一个能把管理员关在门外的动作。短语本身是随机码
		// （CONTRACTS §4.2），"改成什么"写在计划的显示名与影响列表里，与短语并排显示。
		Policy: ActionPolicy{RequiresReauth: true, RequiresPhrase: true,
			PhraseVerb: "RECONFIGURE", Cancellable: false},
		PlanBudget: networkApplyPlanBudget,
		Execute: func(ctx context.Context, in ExecuteInput) (Result, error) {
			params, ok := in.Params.(network.ApplyParams)
			if !ok {
				return Result{}, fmt.Errorf("%w: network.apply 的参数类型是 %T", ErrParamsInvalid, in.Params)
			}
			return networkResultFromReport(src.ExecuteWrite(ctx, network.WriteRequest{
				Action:               network.ActionApply,
				Params:               params,
				ExpectedIdentity:     in.Target.IdentityFingerprint,
				ExpectedStateVersion: in.Target.StateVersion,
				OperationID:          in.OperationID,
				Progress:             in.Progress,
			}))
		},
	}
}

func newNetworkConfirmAction(src NetworkApplySource) Action {
	return Action{
		Name:             network.ActionConfirm,
		ParamsLimitBytes: 2 << 10,
		Decode: func(raw json.RawMessage) (any, error) {
			var p network.ConfirmParams
			if err := DecodeParams(raw, &p); err != nil {
				return nil, err
			}
			return p, nil
		},
		Plan: func(ctx context.Context, in PlanInput) (Plan, error) {
			params, ok := in.Params.(network.ConfirmParams)
			if !ok {
				return Plan{}, fmt.Errorf("%w: network.confirm 的参数类型是 %T", ErrParamsInvalid, in.Params)
			}
			assessment, err := src.AssessConfirm(ctx, params)
			if err != nil {
				return Plan{}, err
			}
			return networkPlan(assessment), nil
		},
		// 不要再认证、不要短语、不可取消：这一步是**救命**的两步之一。门槛越高，
		// 出事后越救不回来——而它做的事只是"别把当前配置退回去"。
		Policy:     ActionPolicy{Cancellable: false},
		PlanBudget: networkApplyPlanBudget,
		Execute: func(ctx context.Context, in ExecuteInput) (Result, error) {
			params, ok := in.Params.(network.ConfirmParams)
			if !ok {
				return Result{}, fmt.Errorf("%w: network.confirm 的参数类型是 %T", ErrParamsInvalid, in.Params)
			}
			return networkResultFromReport(src.ExecuteWrite(ctx, network.WriteRequest{
				Action:               network.ActionConfirm,
				Params:               params,
				ExpectedIdentity:     in.Target.IdentityFingerprint,
				ExpectedStateVersion: in.Target.StateVersion,
				OperationID:          in.OperationID,
				Progress:             in.Progress,
			}))
		},
	}
}

func newNetworkRollbackAction(src NetworkApplySource) Action {
	return Action{
		Name:             network.ActionRollback,
		ParamsLimitBytes: 2 << 10,
		Decode: func(raw json.RawMessage) (any, error) {
			var p network.RollbackParams
			if err := DecodeParams(raw, &p); err != nil {
				return nil, err
			}
			return p, nil
		},
		Plan: func(ctx context.Context, in PlanInput) (Plan, error) {
			params, ok := in.Params.(network.RollbackParams)
			if !ok {
				return Plan{}, fmt.Errorf("%w: network.rollback 的参数类型是 %T", ErrParamsInvalid, in.Params)
			}
			assessment, err := src.AssessRollback(ctx, params)
			if err != nil {
				return Plan{}, err
			}
			return networkPlan(assessment), nil
		},
		// 与 confirm 同一个理由：主动反悔的那一步不该有门槛。
		Policy:     ActionPolicy{Cancellable: false},
		PlanBudget: networkApplyPlanBudget,
		Execute: func(ctx context.Context, in ExecuteInput) (Result, error) {
			params, ok := in.Params.(network.RollbackParams)
			if !ok {
				return Result{}, fmt.Errorf("%w: network.rollback 的参数类型是 %T", ErrParamsInvalid, in.Params)
			}
			return networkResultFromReport(src.ExecuteWrite(ctx, network.WriteRequest{
				Action:               network.ActionRollback,
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

type networkConfigParams struct{}

func newNetworkConfigQuery(src NetworkApplySource) Query {
	return Query{
		Name: NetworkConfigQueryName,
		Decode: func(raw json.RawMessage) (any, error) {
			var params networkConfigParams
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

// networkPlan 把 network.Assessment 翻成代理的计划结构。
//
// 只翻，不加料：Allowed、Blockers、Warnings、Impacts、Locks 全部原样来自判决层。
// 这一层如果开始"顺手补一条告警"，判决就会有两个来源。
func networkPlan(a network.Assessment) Plan {
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
		// 登记表那边要求"不允许就必须给出原因"。这里兜一条，是为了不让一个漏填
		// Blockers 的判决变成一句谜语；正常情况下走不到。
		out.Blockers = append(out.Blockers, Notice{Code: "NETWORK_UNSUPPORTED",
			Message: "网络写入被拒绝，但没有给出原因（这是缺陷，请报告）"})
	}
	return out
}

// networkResultFromReport 把执行报告翻成代理结果。
//
// `rolled_back` 与 `unknown` 必须原样传出去：前者说"改坏了，但已经退回去了"，
// 后者说"我们不知道现在是什么样"。把这两种折成"失败"，界面上就失去了
// "要不要现在去看一眼"这个判断依据。
func networkResultFromReport(report network.WriteReport, err error) (Result, error) {
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
		return Result{}, fmt.Errorf("%w: 网络执行器返回了未知 outcome %q",
			ErrInvalidResult, report.Outcome)
	}
	return out, nil
}
