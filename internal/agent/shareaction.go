package agent

// 共享与受管理账号动作（P08.1）：share.apply / share.disable / share.delete /
// share.account.apply / share.account.delete。
//
// 五个动作与存储动作共用同一套骨架（参数解码 → 预检 → 执行 → 结果核验），判决来自
// shares.Assess，执行来自 shares.ExecuteWrite。
//
// 策略（CONTRACTS.md 第 5 节 R 列，逐条对照）：
//   - share.apply           R=是：改目录 ACL，决定谁能读写这份数据
//   - share.disable         R=否：撤销授权，可逆
//   - share.delete          R=是：移除导出配置（不删数据，但客户端会突然断开）
//   - share.account.apply   R=是：改的是账号的认证凭据
//   - share.account.delete  R=是：删掉一个正在被引用的账号会让共享失效（判决层会先拒）
//
// 都不需要确认短语（短语只给初始化、格式化、删分区这类不可逆操作），都不可取消：
// ACL 的整份替换与 passdb 的写入都是“一动手就要走到结果”的动作，中途停下只会留下
// 一个说不出状态的账号或目录。

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"upanel/internal/shares"
)

// sharesSource 是登记共享动作与查询所需的能力。
//
// 与 storageSource 分开声明：只实现了卷判决的接线不该能登记共享动作，
// 否则第一次点击共享会在执行时返回 501。
type sharesSource interface {
	Assess(ctx context.Context, action string, params any) (shares.Assessment, error)
	ExecuteWrite(ctx context.Context, req shares.WriteRequest) (shares.WriteReport, error)
	Capability(ctx context.Context) shares.Capability
	Shares(ctx context.Context) ([]shares.ShareRecord, error)
	Accounts(ctx context.Context) ([]shares.AccountRecord, error)
	// Connections 读的是 smbstatus（这台机器此刻的会话表），不是登记表。
	Connections(ctx context.Context, shareID string) (shares.ConnectionsReport, error)
}

// RegisterShareActions 登记五个共享与账号动作。
func RegisterShareActions(reg *Registry, src sharesSource) error {
	for _, a := range []Action{
		newShareApplyAction(src),
		newShareDisableAction(src),
		newShareDeleteAction(src),
		newAccountApplyAction(src),
		newAccountDeleteAction(src),
	} {
		if err := reg.RegisterAction(a); err != nil {
			return fmt.Errorf("代理登记 %s 失败: %w", a.Name, err)
		}
	}
	return nil
}

// sharesPlanner 把共享判决映射成预检计划。
func sharesPlanner(src sharesSource, action string) func(context.Context, PlanInput) (Plan, error) {
	return func(ctx context.Context, in PlanInput) (Plan, error) {
		assessment, err := src.Assess(ctx, action, in.Params)
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

// sharesExecutor 把共享执行器接成代理的动作执行函数。
//
// 与 storageExecutor 的差别只有一处，但很要紧：**outcome 原样传递**。
// share.apply 在 P08.1 只能给出 accepted（目录与 ACL 准备好了，共享还没被提供）。
// 如果这里像存储执行器那样硬写 verified，任务中心会显示“已核验”，而事实是配置
// 还没写进 Samba。
func sharesExecutor(src sharesSource, action string) func(context.Context, ExecuteInput) (Result, error) {
	return func(ctx context.Context, in ExecuteInput) (Result, error) {
		report, err := src.ExecuteWrite(ctx, shares.WriteRequest{
			Action:               action,
			Params:               in.Params,
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
		case shares.OutcomeVerified:
			out.Outcome = OutcomeVerified
			out.VerifiedAt = time.Now().UTC().Truncate(time.Second)
		case shares.OutcomeAccepted:
			out.Outcome = OutcomeAccepted
		default:
			return Result{}, fmt.Errorf("%w: 共享执行器返回了未知 outcome %q",
				ErrInvalidResult, report.Outcome)
		}
		return out, nil
	}
}

// newShareApplyAction 登记 share.apply。
func newShareApplyAction(src sharesSource) Action {
	return Action{
		Name: shares.ActionShareApply,
		Decode: func(raw json.RawMessage) (any, error) {
			var p shares.ShareApplyParams
			if err := DecodeParams(raw, &p); err != nil {
				return nil, err
			}
			if p.Definition == nil {
				return nil, fmt.Errorf("%w: definition 不能为空", ErrParamsInvalid)
			}
			// 形状校验在 shares.ValidateDefinition 里（它同时被判决层与登记表调用），
			// 这里只把明显的填错提前挡住，并让错误消息带上参数名。
			p.Definition.Name = strings.TrimSpace(p.Definition.Name)
			p.Definition.VolumeID = strings.TrimSpace(p.Definition.VolumeID)
			p.Definition.RelativePath = strings.TrimSpace(p.Definition.RelativePath)
			if p.ShareID != nil {
				id := strings.TrimSpace(*p.ShareID)
				p.ShareID = &id
				if !shares.ValidShareID(id) {
					return nil, fmt.Errorf("%w: share_id 必须是 shr_ 加 32 位小写十六进制，收到 %q",
						ErrParamsInvalid, id)
				}
			}
			if p.Version != nil {
				v := strings.TrimSpace(*p.Version)
				p.Version = &v
				if err := validateVersionToken(v); err != nil {
					return nil, err
				}
			}
			return p, nil
		},
		Plan: sharesPlanner(src, shares.ActionShareApply),
		Policy: ActionPolicy{
			RequiresReauth: true,
			RequiresPhrase: false,
			Cancellable:    false,
		},
		Execute: sharesExecutor(src, shares.ActionShareApply),
	}
}

// newShareDisableAction 登记 share.disable。
func newShareDisableAction(src sharesSource) Action {
	return Action{
		Name: shares.ActionShareDisable,
		Decode: func(raw json.RawMessage) (any, error) {
			return decodeShareRef(raw)
		},
		Plan: sharesPlanner(src, shares.ActionShareDisable),
		Policy: ActionPolicy{
			RequiresReauth: false,
			RequiresPhrase: false,
			Cancellable:    false,
		},
		Execute: sharesExecutor(src, shares.ActionShareDisable),
	}
}

// newShareDeleteAction 登记 share.delete。
func newShareDeleteAction(src sharesSource) Action {
	return Action{
		Name: shares.ActionShareDelete,
		Decode: func(raw json.RawMessage) (any, error) {
			return decodeShareRef(raw)
		},
		Plan: sharesPlanner(src, shares.ActionShareDelete),
		Policy: ActionPolicy{
			RequiresReauth: true,
			RequiresPhrase: false,
			Cancellable:    false,
		},
		Execute: sharesExecutor(src, shares.ActionShareDelete),
	}
}

// newAccountApplyAction 登记 share.account.apply。
func newAccountApplyAction(src sharesSource) Action {
	return Action{
		Name: shares.ActionAccountApply,
		Decode: func(raw json.RawMessage) (any, error) {
			var p shares.AccountApplyParams
			if err := DecodeParams(raw, &p); err != nil {
				return nil, err
			}
			p.Name = strings.TrimSpace(p.Name)
			p.SecretRef = strings.TrimSpace(p.SecretRef)
			if p.AccountID != nil {
				id := strings.TrimSpace(*p.AccountID)
				p.AccountID = &id
				if !shares.ValidAccountID(id) {
					return nil, fmt.Errorf("%w: account_id 必须是 sac_ 加 32 位小写十六进制，收到 %q",
						ErrParamsInvalid, id)
				}
			}
			// secret_ref 的形状在这里也查一次：它是一个会被解引用的引用，
			// 不是密码本身——密码永远不从这条路径进来。
			if p.SecretRef != "" && !strings.HasPrefix(p.SecretRef, "sec_") {
				return nil, fmt.Errorf("%w: secret_ref 必须是秘密通道返回的引用（sec_ 开头），"+
					"不能直接放密码", ErrParamsInvalid)
			}
			if p.Name == "" {
				return nil, fmt.Errorf("%w: name 不能为空", ErrParamsInvalid)
			}
			return p, nil
		},
		Plan: sharesPlanner(src, shares.ActionAccountApply),
		Policy: ActionPolicy{
			RequiresReauth: true,
			RequiresPhrase: false,
			Cancellable:    false,
		},
		Execute: sharesExecutor(src, shares.ActionAccountApply),
	}
}

// newAccountDeleteAction 登记 share.account.delete。
func newAccountDeleteAction(src sharesSource) Action {
	return Action{
		Name: shares.ActionAccountDelete,
		Decode: func(raw json.RawMessage) (any, error) {
			var p shares.AccountRefParams
			if err := DecodeParams(raw, &p); err != nil {
				return nil, err
			}
			p.AccountID = strings.TrimSpace(p.AccountID)
			if !shares.ValidAccountID(p.AccountID) {
				return nil, fmt.Errorf("%w: account_id 必须是 sac_ 加 32 位小写十六进制，收到 %q",
					ErrParamsInvalid, p.AccountID)
			}
			return p, nil
		},
		Plan: sharesPlanner(src, shares.ActionAccountDelete),
		Policy: ActionPolicy{
			RequiresReauth: true,
			RequiresPhrase: false,
			Cancellable:    false,
		},
		Execute: sharesExecutor(src, shares.ActionAccountDelete),
	}
}

// decodeShareRef 解 share.disable / share.delete 的公共参数。
func decodeShareRef(raw json.RawMessage) (any, error) {
	var p shares.ShareRefParams
	if err := DecodeParams(raw, &p); err != nil {
		return nil, err
	}
	p.ShareID = strings.TrimSpace(p.ShareID)
	p.Version = strings.TrimSpace(p.Version)
	if !shares.ValidShareID(p.ShareID) {
		return nil, fmt.Errorf("%w: share_id 必须是 shr_ 加 32 位小写十六进制，收到 %q",
			ErrParamsInvalid, p.ShareID)
	}
	if err := validateVersionToken(p.Version); err != nil {
		return nil, err
	}
	return p, nil
}
