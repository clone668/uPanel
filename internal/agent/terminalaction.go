package agent

// P06.3：受限终端在代理侧的登记（CONTRACTS.md 第 5、10 节）。
//
// 终端与其它动作的区别只有一个：它创建的东西**活得比任务长**。所以这里不做
// “执行里等终端结束”那种事——任务是“开一个会话”，返回时就结束了，会话由
// internal/terminal 的会话表管理，后续字节走 terminal.io 流，关闭走代理内部的
// DELETE。三件事分开之后，每一个都可以单独说清楚“谁能做、能做什么”。
//
// 本层只做三件事：解码参数、把会话层的状态翻译成预检的阻塞项、在 Execute 里
// 真的创建并**读回**它。判决（数量上限、账号可用性、超时）全部来自会话层。

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"upanel/internal/terminal"
)

// TerminalManager 是会话层在代理层需要的最小接口（生产实现是 *terminal.Manager）。
//
// 导出是因为 cmd/upanel-agent 要把它塞进 Options 与 ProductionDeps；
// 方法集只有“创建、查询、关闭、连接”四件事——没有“执行命令”，也没有“列出所有人的会话”。
type TerminalManager interface {
	Account() terminal.Account
	Limits() terminal.Limits
	Create(ctx context.Context, in terminal.CreateInput) (terminal.SessionInfo, error)
	List(filter terminal.ListFilter) ([]terminal.SessionInfo, error)
	Get(filter terminal.ListFilter) (terminal.SessionInfo, error)
	Close(in terminal.CloseInput) error
	Attach(sessionID string, owner int64) (*terminal.Attachment, error)
}

// 端子会话预检里的阻塞项码。它们只出现在预检响应的 blockers 里，
// 不是 HTTP 错误码（HTTP 码仍然来自 terminal.HTTPStatusOf）。
const (
	// BlockTerminalAccount：终端账号不可用（不存在、是 root、shell 是 nologin、home 权限过宽）。
	BlockTerminalAccount = "TERMINAL_ACCOUNT_UNAVAILABLE"
	// BlockTerminalLimit：会话数量已达上限。
	BlockTerminalLimit = "TERMINAL_SESSION_LIMIT"
)

// newTerminalCreateAction 登记 terminal.create。
func newTerminalCreateAction(mgr TerminalManager) Action {
	return Action{
		Name: terminal.ActionCreate,
		Decode: func(raw json.RawMessage) (any, error) {
			var params terminal.Params
			if err := DecodeParams(raw, &params); err != nil {
				return nil, err
			}
			if err := params.Validate(); err != nil {
				return nil, fmt.Errorf("%w: %v", ErrParamsInvalid, err)
			}
			return params, nil
		},
		Plan: func(ctx context.Context, in PlanInput) (Plan, error) {
			params, ok := in.Params.(terminal.Params)
			if !ok {
				return Plan{}, fmt.Errorf("%w: 终端参数类型不一致", ErrParamsInvalid)
			}
			account := mgr.Account()
			limits := mgr.Limits()

			// 目标身份指纹用账号 + uid：它描述的是“这次会以谁的身份运行”，
			// 而不是“第几个会话”。账号被换掉（比如 uid 变了）时，旧预检自然失效。
			label := fmt.Sprintf("%s（uid %d）", account.User, account.UID)

			// 预检只是**报告**当前是否还有位置；真正的判定在 Create 里再原子地做一次。
			// 两次判定不是重复劳动：预检给出的是人能读懂的阻塞项，Create 给的是
			// “两次并发创建不会同时成功”的保证。
			live, listErr := mgr.List(terminal.ListFilter{UserID: in.Actor.UserID})
			var blockers []Notice
			allowed := true
			stateVersion := "sessions=0"
			if listErr != nil {
				allowed = false
				blockers = append(blockers, Notice{
					Code:    BlockTerminalLimit,
					Message: fmt.Sprintf("无法读取当前会话数量：%v", listErr),
				})
			} else {
				stateVersion = fmt.Sprintf("sessions=%d", len(live))
				if len(live) >= limits.MaxPerUser {
					allowed = false
					blockers = append(blockers, Notice{
						Code: BlockTerminalLimit,
						Message: fmt.Sprintf("每个账号最多 %d 个终端会话，当前已有 %d 个；"+
							"请先关闭已有的终端", limits.MaxPerUser, len(live)),
					})
				}
			}
			if account.UID == 0 || account.User == "" {
				allowed = false
				blockers = append(blockers, Notice{
					Code:    BlockTerminalAccount,
					Message: "终端账号不可用，代理未启起 PTY 会话层",
				})
			}

			return Plan{
				NormalizedParams: params,
				Target: Target{
					DisplayName:         label,
					IdentityFingerprint: fmt.Sprintf("terminal:%s:%d", account.User, account.UID),
					StateVersion:        stateVersion,
				},
				Allowed:  allowed,
				Blockers: blockers,
				Warnings: nil,
				Impacts: []Impact{{
					Kind: "interactive_access",
					Description: fmt.Sprintf("将以 %s 的身份打开一个交互 shell（%dx%d）。"+
						"这不是 root 终端：无法 sudo、读不到代理密钥、写不了系统目录。",
						account.User, params.Cols, params.Rows),
				}},
				// 会话表自己就是被争用的资源，并且由会话层的互斥保护（Create 在锁内
				// 数完再 fork）。所以这里不登记资源锁：一把只在这一个动作里出现、
				// 又不对应任何外部资源的锁，只会让 RESOURCE_BUSY 出现得没有道理。
				Locks: nil,
			}, nil
		},
		Execute: func(ctx context.Context, in ExecuteInput) (Result, error) {
			params, ok := in.Params.(terminal.Params)
			if !ok {
				return Result{}, fmt.Errorf("%w: 终端参数类型不一致", ErrParamsInvalid)
			}
			// 执行前再核一次账号：预检到提交之间配置可能被改过（代理重启后
			// 会话层会被替换），这里读到的是当前真正会用来 fork 的那一份。
			account := mgr.Account()
			if account.UID == 0 || account.User == "" {
				return Result{}, fmt.Errorf("%w: 终端账号不可用", terminal.ErrAccountInvalid)
			}
			if in.Progress != nil {
				in.Progress("creating_session", map[string]any{
					"account": account.User, "account_uid": account.UID,
				})
			}
			info, err := mgr.Create(ctx, terminal.CreateInput{
				Owner:     in.Actor.UserID,
				OwnerName: "",
				Params:    params,
			})
			if err != nil {
				// 会话层的拒绝带着自己的判决（数量到顶是 RESOURCE_BUSY、账号不可用是
				// NOT_IMPLEMENTED）。不包这一层，任务中心只会显示“动作执行失败”，
				// 而同一个原因在 GET/POST 接口上却是 409/501——两处说法不一致。
				return Result{}, terminalRejection(err)
			}
			// 读回：会话确实在表里、属于这个人、状态是未连接。
			back, err := mgr.Get(terminal.ListFilter{UserID: in.Actor.UserID, SessionID: info.SessionID})
			if err != nil {
				return Result{}, fmt.Errorf("terminal: 创建后读回会话失败: %w", err)
			}
			if back.Account != account.User || back.AccountUID != account.UID {
				return Result{}, fmt.Errorf("terminal: 读回的会话账号是 %s(uid %d)，与预期 %s(uid %d) 不一致",
					back.Account, back.AccountUID, account.User, account.UID)
			}
			now := in.Now.UTC()
			if now.IsZero() {
				now = time.Now().UTC()
			}
			return Result{
				Outcome:    OutcomeVerified,
				VerifiedAt: now,
				ObservedState: map[string]any{
					"session_id":  back.SessionID,
					"account":     back.Account,
					"account_uid": back.AccountUID,
					"state":       back.State,
					"cols":        back.Cols,
					"rows":        back.Rows,
				},
				// Data 是任务结果里给 Web 用的部分：session_id 是接下来申请连接许可的
				// 唯一凭据，账号与时限让界面不靠硬编码复述边界。
				Data: map[string]any{
					"session_id":           back.SessionID,
					"account":              back.Account,
					"account_uid":          back.AccountUID,
					"state":                back.State,
					"created_at":           back.CreatedAt.Format(time.RFC3339),
					"expires_at":           back.ExpiresAt.Format(time.RFC3339),
					"idle_deadline":        back.IdleDeadline.Format(time.RFC3339),
					"idle_timeout_seconds": back.IdleTimeoutSeconds,
					"max_duration_seconds": back.MaxDurationSeconds,
				},
			}, nil
		},
		// 策略按 CONTRACTS.md 第 5 节的动作表：terminal.create 需要再认证（“是”），
		// 不在“必须要求确认短语”的名单里（GPT/格式化/删分区/递归删除/覆盖恢复/仓库清理）。
		// 开一个降权 shell 的门槛因此是“5 分钟内的密码再认证”，而不是朗读一段短语——
		// 短语留给不可逆的操作。
		Policy: ActionPolicy{
			RequiresReauth: true,
			RequiresPhrase: false,
			Cancellable:    false,
		},
	}
}

// terminalRejection 把会话层的拒绝包成带判决的错误。
//
// 与 files 的做法一致（internal/agent/filesquery.go 的 statusCarrier）：拒绝的
// 状态码由知道原因的那一层给出，任务层只负责照抄。已分类的错误会变成
// *terminal.HTTPError（它实现了 HTTPStatus），于是任务中心的 error.code 是
// RESOURCE_BUSY / NOT_IMPLEMENTED 这类可处理的原因，而不是 COMMAND_FAILED。
// 未分类的错误原样返回，让它看起来就是失败。
func terminalRejection(err error) error {
	if mapped, ok := terminal.HTTPStatusOf(err); ok {
		return mapped
	}
	return err
}

// newTerminalSessionsQuery 登记 terminal.sessions。
func newTerminalSessionsQuery(mgr TerminalManager) Query {
	return Query{
		Name: terminal.QuerySessions,
		Decode: func(raw json.RawMessage) (any, error) {
			var params terminal.SessionsParams
			if err := DecodeParams(raw, &params); err != nil {
				return nil, err
			}
			if err := params.Validate(); err != nil {
				return nil, fmt.Errorf("%w: %v", ErrParamsInvalid, err)
			}
			return params, nil
		},
		Run: func(ctx context.Context, params any) (any, error) {
			p, ok := params.(terminal.SessionsParams)
			if !ok {
				return nil, fmt.Errorf("%w: 终端查询参数类型不一致", ErrParamsInvalid)
			}
			account := mgr.Account()
			limits := mgr.Limits()
			var sessions []terminal.SessionInfo
			if p.SessionID != "" {
				// 单个会话：不属于该用户时按“不存在”回答（terminal.Get 负责这条规则）。
				// 判决要走 terminalRejection：ErrNotFound 不带判决时查询层只能回
				// 500“查询执行失败”，而同一个原因在公开接口上是 404——两处说法不一致。
				info, err := mgr.Get(p.Filter())
				if err != nil {
					return nil, terminalRejection(err)
				}
				sessions = []terminal.SessionInfo{info}
			} else {
				list, err := mgr.List(p.Filter())
				if err != nil {
					return nil, terminalRejection(err)
				}
				sessions = list
			}
			if sessions == nil {
				sessions = []terminal.SessionInfo{}
			}
			return terminal.SessionsResult{
				Account:    account.User,
				AccountUID: account.UID,

				IdleTimeoutSeconds:     int(limits.IdleTimeout.Seconds()),
				MaxDurationSeconds:     int(limits.MaxDuration.Seconds()),
				DisconnectGraceSeconds: int(limits.DisconnectGrace.Seconds()),
				MaxPerUser:             limits.MaxPerUser,
				MaxSessions:            limits.MaxSessions,
				Sessions:               sessions,
			}, nil
		},
	}
}
