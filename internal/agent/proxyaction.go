package agent

// 反向代理动作（P10.2）：proxy.apply。
//
// 一个动作，不是四个。新建、改名、改访问策略、停用、重新启用都走它——真正变化的
// 只有“托管文件应该是什么内容”，而那个差别由定义本身表达（enabled=false 渲染结果
// 就是“这个文件不该存在”）。拆成 apply/enable/disable/rename 只会让四条路径各自
// 演化，最后出现“停用没删文件”这类只修好一条路的缺陷。
//
// 策略：R=是（写的是“哪个域名指向本机哪个端口、谁能访问”，等于改一次对外暴露面），
// 不可取消（写到一半的配置比不写更危险），不需要确认短语（短语留给初始化分区表、
// 格式化、删分区这类不可逆操作；配置总有备份，回滚是一条 rename）。

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"upanel/internal/httpx"
	"upanel/internal/proxies"
)

// proxySource 是登记代理动作所需的判决与执行能力。
//
// 判决与执行分成两个方法而不是一个：预检不写系统（CONTRACTS.md 4.1），执行才写。
// 把两者合成一个 "Apply(dryRun bool)" 会让某一次调用里“到底有没有真的动手”取决于
// 一个布尔参数——那正是最容易写错的地方。
type proxySource interface {
	AssessApply(ctx context.Context, in proxies.ApplyParams) (proxies.Assessment, error)
	ExecuteApply(ctx context.Context, req proxies.WriteRequest) (proxies.WriteReport, error)
}

// RegisterProxyActions 登记反向代理动作。
func RegisterProxyActions(reg *Registry, src proxySource) error {
	if err := reg.RegisterAction(newProxyApplyAction(src)); err != nil {
		return fmt.Errorf("代理登记 %s 失败: %w", proxies.ActionProxyApply, err)
	}
	return nil
}

// newProxyApplyAction 登记 proxy.apply。
func newProxyApplyAction(src proxySource) Action {
	return Action{
		Name: proxies.ActionProxyApply,
		Decode: func(raw json.RawMessage) (any, error) {
			var p proxies.ApplyParams
			if err := DecodeParams(raw, &p); err != nil {
				return nil, err
			}
			if p.Definition != nil {
				p.Definition.Domain = strings.TrimSpace(p.Definition.Domain)
				p.Definition.UpstreamTargetID = strings.TrimSpace(p.Definition.UpstreamTargetID)
			}
			// 归一化在解码阶段做一次：预检落库的是规范化之后的参数，执行时拿到的是
			// 同一份字节。放到执行阶段再归一，等于让“预检展示的东西”和“实际写下去的
			// 东西”有两次各自的解释机会。
			normalized, err := p.Normalize()
			if err != nil {
				return nil, fmt.Errorf("%w: %v", ErrParamsInvalid, err)
			}
			return normalized, nil
		},
		Plan:   proxyPlanner(src),
		Policy: ActionPolicy{RequiresReauth: true, RequiresPhrase: false, Cancellable: false},
		Execute: func(ctx context.Context, in ExecuteInput) (Result, error) {
			p, ok := in.Params.(proxies.ApplyParams)
			if !ok {
				return Result{}, fmt.Errorf("%w: proxy.apply 的参数类型是 %T", ErrParamsInvalid, in.Params)
			}
			report, err := src.ExecuteApply(ctx, proxies.WriteRequest{
				Action:               proxies.ActionProxyApply,
				Params:               p,
				ExpectedIdentity:     in.Target.IdentityFingerprint,
				ExpectedStateVersion: in.Target.StateVersion,
				OperationID:          in.OperationID,
				Progress:             in.Progress,
			})
			if err != nil {
				return Result{}, err
			}
			out := Result{
				Outcome:       OutcomeUnknown,
				ObservedState: report.Observed,
				Data:          report.Data,
				RecoveryHint:  report.RecoveryHint,
			}
			switch report.Outcome {
			case proxies.OutcomeVerified:
				out.Outcome = OutcomeVerified
				// 核验时间由动作层盖：代理层只说“读回核对通过了”，不替它记钟点。
				// 截到秒，与其它动作一致——任务是给人看的，秒以下没有意义。
				out.VerifiedAt = time.Now().UTC().Truncate(time.Second)
			case proxies.OutcomeAccepted:
				out.Outcome = OutcomeAccepted
			default:
				return Result{}, fmt.Errorf("%w: 代理执行器返回了未知 outcome %q",
					ErrInvalidResult, report.Outcome)
			}
			return out, nil
		},
	}
}

// proxyPlanner 把代理判决映射成预检计划。
func proxyPlanner(src proxySource) func(context.Context, PlanInput) (Plan, error) {
	return func(ctx context.Context, in PlanInput) (Plan, error) {
		p, ok := in.Params.(proxies.ApplyParams)
		if !ok {
			return Plan{}, fmt.Errorf("%w: proxy.apply 的参数类型是 %T", ErrPlanFailed, in.Params)
		}
		assessment, err := src.AssessApply(ctx, p)
		if err != nil {
			return Plan{}, fmt.Errorf("%w: %v", ErrPlanFailed, err)
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
			NormalizedParams: in.Params,
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
		}, nil
	}
}

// 编译期确认：proxies.HTTPError 满足代理层的判决接口，错误不会被压成 500。
var _ interface {
	HTTPStatus() (int, httpx.Code)
} = (*proxies.HTTPError)(nil)
