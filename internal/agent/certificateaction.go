package agent

// 证书动作（P10.3）：certificate.import / certificate.issue / certificate.renew，
// 以及只读查询 certificate.state。
//
// 三个写动作共用一个形状（预检 → 执行 → 报告），差别只在参数与「这次到底做什么」：
//
//	import  手动导入：公钥证书链走参数，私钥走秘密存储
//	issue   签发：跑 certbot，staging 是默认，正式要显式选
//	renew   续期：同一条 certbot 命令，由 certbot 自己判断是否到期
//
// 为什么续期单独登记一个动作，而不是给 issue 加一个 force 参数：审计与任务列表要能
// 一眼分出「人点了签发」和「系统在续期」，而 P12 的 timer 将来只会调用 renew 那一个名字。

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"upanel/internal/certificates"
	"upanel/internal/proxies"
	"upanel/internal/tlscert"
)

// certificateSource 是登记证书动作所需的判决与执行能力。
type certificateSource interface {
	AssessImport(ctx context.Context, in certificates.ImportParams) (proxies.Assessment, error)
	ExecuteImport(ctx context.Context, req proxies.WriteRequest) (proxies.WriteReport, error)
	AssessIssue(ctx context.Context, in certificates.IssueParams, action string) (proxies.Assessment, error)
	ExecuteIssue(ctx context.Context, req proxies.WriteRequest, action string) (proxies.WriteReport, error)
	Inspect(ctx context.Context, in certificates.StateParams) (map[string]any, error)
}

// RegisterCertificateActions 登记证书写动作。
func RegisterCertificateActions(reg *Registry, src certificateSource) error {
	if err := reg.RegisterAction(newCertificateImportAction(src)); err != nil {
		return fmt.Errorf("证书登记 %s 失败: %w", certificates.ActionImport, err)
	}
	for _, name := range []string{certificates.ActionIssue, certificates.ActionRenew} {
		if err := reg.RegisterAction(newCertificateIssueAction(src, name)); err != nil {
			return fmt.Errorf("证书登记 %s 失败: %w", name, err)
		}
	}
	return nil
}

// RegisterCertificateQueries 登记证书只读查询。
func RegisterCertificateQueries(reg *Registry, src certificateSource) error {
	if err := reg.RegisterQuery(newCertificateStateQuery(src)); err != nil {
		return fmt.Errorf("证书登记 %s 失败: %w", certificates.ActionState, err)
	}
	return nil
}

// registerCertificateQueriesIfAvailable 给 ProductionDeps 用：没有接线就不登记。
func registerCertificateQueriesIfAvailable(reg *Registry, src certificateSource) error {
	if src == nil {
		return nil
	}
	return RegisterCertificateQueries(reg, src)
}

// RegisterCertificateActionsIfAvailable 同上（写动作）。
func RegisterCertificateActionsIfAvailable(reg *Registry, src certificateSource) error {
	if src == nil {
		return nil
	}
	return RegisterCertificateActions(reg, src)
}

// newCertificateImportAction 登记 certificate.import。
func newCertificateImportAction(src certificateSource) Action {
	return Action{
		Name: certificates.ActionImport,
		// 参数上限放到 1 MiB 加一点余量：公钥证书链是公开材料，但也确实可能有一
		// 整个链（叶子 + 中间证书）。默认的 64 KiB 对一张正常证书够用，对
		// 一个带三条中间证书的链就紧了，而失败的形状会是「参数不合法」这种
		// 看不出原因的错误。
		ParamsLimitBytes: tlscert.MaxCertBytes + 8<<10,
		Decode: func(raw json.RawMessage) (any, error) {
			var p certificates.ImportParams
			if err := DecodeParamsLimit(raw, &p, tlscert.MaxCertBytes+8<<10); err != nil {
				return nil, err
			}
			normalized, err := p.Normalize()
			if err != nil {
				return nil, fmt.Errorf("%w: %v", ErrParamsInvalid, err)
			}
			return normalized, nil
		},
		Plan: certificateImportPlanner(src),
		// 写的是 root 拥有的私钥文件：再认证是必需的。不可取消：写到一半的证书
		// 与写一半的私钥一样，都不该留在盘上等下一次 explain。
		Policy: ActionPolicy{RequiresReauth: true, RequiresPhrase: false, Cancellable: false},
		Execute: func(ctx context.Context, in ExecuteInput) (Result, error) {
			p, ok := in.Params.(certificates.ImportParams)
			if !ok {
				return Result{}, fmt.Errorf("%w: certificate.import 的参数类型是 %T",
					ErrParamsInvalid, in.Params)
			}
			report, err := src.ExecuteImport(ctx, proxies.WriteRequest{
				Action:               certificates.ActionImport,
				Params:               p,
				ExpectedIdentity:     in.Target.IdentityFingerprint,
				ExpectedStateVersion: in.Target.StateVersion,
				OperationID:          in.OperationID,
				Progress:             in.Progress,
			})
			if err != nil {
				return Result{}, err
			}
			return resultFromReport(report)
		},
	}
}

// newCertificateIssueAction 登记 certificate.issue 与 certificate.renew。
func newCertificateIssueAction(src certificateSource, name string) Action {
	return Action{
		Name: name,
		Decode: func(raw json.RawMessage) (any, error) {
			var p certificates.IssueParams
			if err := DecodeParams(raw, &p); err != nil {
				return nil, err
			}
			normalized, err := p.Normalize()
			if err != nil {
				return nil, fmt.Errorf("%w: %v", ErrParamsInvalid, err)
			}
			return normalized, nil
		},
		Plan: certificateIssuePlanner(src, name),
		// 签发会真的向 CA 提交订单（正式环境里消耗速率限额），并且改的是证书目录：
		// 再认证必需。不可取消：一次被中断的 ACME 订单不会因为「点了取消」而消失，
		// 让它在原地跑完，比留下一个不知道处于什么状态的订单更可解释。
		Policy: ActionPolicy{RequiresReauth: true, RequiresPhrase: false, Cancellable: false},
		Execute: func(ctx context.Context, in ExecuteInput) (Result, error) {
			p, ok := in.Params.(certificates.IssueParams)
			if !ok {
				return Result{}, fmt.Errorf("%w: %s 的参数类型是 %T",
					ErrParamsInvalid, name, in.Params)
			}
			report, err := src.ExecuteIssue(ctx, proxies.WriteRequest{
				Action:               name,
				Params:               p,
				ExpectedIdentity:     in.Target.IdentityFingerprint,
				ExpectedStateVersion: in.Target.StateVersion,
				OperationID:          in.OperationID,
				Progress:             in.Progress,
			}, name)
			if err != nil {
				return Result{}, err
			}
			return resultFromReport(report)
		},
	}
}

// newCertificateStateQuery 登记 certificate.state：读盘上那张证书的事实。
func newCertificateStateQuery(src certificateSource) Query {
	return Query{
		Name: certificates.ActionState,
		Decode: func(raw json.RawMessage) (any, error) {
			var p certificates.StateParams
			if err := DecodeParams(raw, &p); err != nil {
				return nil, err
			}
			normalized, err := p.Normalize()
			if err != nil {
				return nil, fmt.Errorf("%w: %v", ErrParamsInvalid, err)
			}
			return normalized, nil
		},
		Run: func(ctx context.Context, params any) (any, error) {
			p, ok := params.(certificates.StateParams)
			if !ok {
				return nil, fmt.Errorf("参数类型不匹配: %T", params)
			}
			return src.Inspect(ctx, p)
		},
	}
}

// certificateImportPlanner 把导入判决映射成预检计划。
func certificateImportPlanner(src certificateSource) func(context.Context, PlanInput) (Plan, error) {
	return func(ctx context.Context, in PlanInput) (Plan, error) {
		p, ok := in.Params.(certificates.ImportParams)
		if !ok {
			return Plan{}, fmt.Errorf("%w: certificate.import 的参数类型是 %T",
				ErrPlanFailed, in.Params)
		}
		assessment, err := src.AssessImport(ctx, p)
		if err != nil {
			return Plan{}, fmt.Errorf("%w: %v", ErrPlanFailed, err)
		}
		return planFromAssessment(in.Params, assessment), nil
	}
}

// certificateIssuePlanner 把签发/续期判决映射成预检计划。
func certificateIssuePlanner(src certificateSource, action string) func(context.Context, PlanInput) (Plan, error) {
	return func(ctx context.Context, in PlanInput) (Plan, error) {
		p, ok := in.Params.(certificates.IssueParams)
		if !ok {
			return Plan{}, fmt.Errorf("%w: %s 的参数类型是 %T",
				ErrPlanFailed, action, in.Params)
		}
		assessment, err := src.AssessIssue(ctx, p, action)
		if err != nil {
			return Plan{}, fmt.Errorf("%w: %v", ErrPlanFailed, err)
		}
		return planFromAssessment(in.Params, assessment), nil
	}
}

// planFromAssessment 把判决转成登记表要的计划（与 proxyPlanner 同一套映射）。
func planFromAssessment(params any, assessment proxies.Assessment) Plan {
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
		NormalizedParams: params,
		Target: Target{
			DisplayName:         assessment.DisplayName,
			IdentityFingerprint: assessment.IdentityFingerprint,
			StateVersion:        assessment.StateVersion,
		},
		Allowed:  assessment.Allowed,
		Blockers: blockers,
		Warnings: warnings,
		Impacts:  impacts,
		Locks:    assessment.Locks,
	}
}

// resultFromReport 把一次写报告转成登记表的执行结果。
//
// 与 proxy.apply 一样：核验时间由动作层盖，代理层只说「读回核对通过了」。
func resultFromReport(report proxies.WriteReport) (Result, error) {
	out := Result{
		Outcome:       OutcomeUnknown,
		ObservedState: report.Observed,
		Data:          report.Data,
		RecoveryHint:  report.RecoveryHint,
	}
	switch report.Outcome {
	case proxies.OutcomeVerified:
		out.Outcome = OutcomeVerified
		out.VerifiedAt = time.Now().UTC().Truncate(time.Second)
	case proxies.OutcomeAccepted:
		out.Outcome = OutcomeAccepted
	default:
		return Result{}, fmt.Errorf("%w: 证书执行器返回了未知 outcome %q",
			ErrInvalidResult, report.Outcome)
	}
	return out, nil
}

// 编译期确认：certificates 的错误也实现了带判决的接口。
var _ interface {
	Error() string
} = (*certificates.ParamError)(nil)
