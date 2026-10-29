package agent

// backup.repository.initialize（P12.1）：备份仓库的初始化。
//
// 与 package.*/system.power 一样，这一层只做翻译：把 `repository_id` + `version`
// 变成一次可审计的意图、把预检结论翻成 Plan、把执行结果翻成 Result。判断全在
// internal/backup 里（restic 在不在、秘密取不取得到、主机密钥对不对、位置有没有
// 已经存在的仓库、还有多少空间）。
//
// 三处刻意的不对称，理由都来自契约：
//
//	参数   只有 repository_id 与 version（CONTRACTS 第 5 节冻结）；定义从代理自己的
//	       镜像里取，版本不符就拒绝——Web 改过的定义必须重新预检
//	结论   建完必须读回来核对（restic cat config），核对通过才是 verified
//	影响   只新建，不覆盖：目标已有仓库时预检就挡住

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"upanel/internal/backup"
)

// backupPlanBudget 是仓库预检的同步计算预算。
//
// 预检要跑一次 `restic version`、可能还要拨一次 SFTP（连接 + 握手 + statvfs）。
// 拨号上限 30 秒、网络层失败还会再来一轮，最坏 60 秒；再给 statvfs 留十几秒，
// 75 秒能把“慢但活着”的机器和“真不可达”分开。这个数必须小于 Web 侧
// PreflightAgentTimeout（90 秒），否则 Web 会先报 AGENT_UNAVAILABLE，把代理的结论盖掉。
// 24.04 单线程 TCG 验收机的真实握手就在十几秒一档（见 DefaultDialTimeout 注释）。
const backupPlanBudget = 75 * time.Second

// backupSource 是备份动作需要的全部能力。
//
// 定义来源与现成实现分开列：GetRepository 来自代理数据库，Assess/Execute/State
// 来自 internal/backup 的 Ops。生产装配把两者合成一个值（见 cmd/upanel-agent）。
type backupSource interface {
	GetRepository(ctx context.Context, id string) (backup.Repository, bool, error)
	Assess(ctx context.Context, req backup.AssessRequest) (backup.Assessment, error)
	Execute(ctx context.Context, req backup.ExecuteRequest) (backup.Execution, error)
	State(ctx context.Context, req backup.StateRequest) (backup.RepositoryState, error)
}

// backupInitParams 是 backup.repository.initialize 的参数（字段名以 CONTRACTS 为准）。
type backupInitParams struct {
	RepositoryID string `json:"repository_id"`
	Version      int64  `json:"version"`
}

// RegisterBackupAction 登记 backup.repository.initialize。
func RegisterBackupAction(reg *Registry, src backupSource) error {
	if src == nil {
		return errors.New("agent: 备份动作没有实现端")
	}
	return reg.RegisterAction(Action{
		Name: backup.ActionRepositoryInitialize,
		// 预检可能要拨一次 SFTP，给它一条自己的秒表；执行则完全在后台跑。
		PlanBudget: backupPlanBudget,
		Decode: func(raw json.RawMessage) (any, error) {
			var p backupInitParams
			if err := DecodeParams(raw, &p); err != nil {
				return nil, err
			}
			if p.RepositoryID == "" {
				return nil, fmt.Errorf("%w: repository_id 不能为空", ErrParamsInvalid)
			}
			if p.Version < 1 {
				return nil, fmt.Errorf("%w: version 必须为正整数（仓库记录的乐观并发令牌）", ErrParamsInvalid)
			}
			return p, nil
		},
		Plan:    newBackupInitPlan(src),
		Execute: newBackupInitExecute(src),
		// 策略：初始化要在目标位置上写一个仓库，属于“要密码再确认一次”的动作
		// （CONTRACTS 第 5 节的 R 列）。不要短语——短语留给格式化、删除与清理；
		// 可取消：SFTP 上建仓库可能慢，中途取消不会留下半成品之外的东西，
		// 而半成品只是一个空目录（恢复提示会说要清掉它）。
		Policy: ActionPolicy{RequiresReauth: true, Cancellable: true},
	})
}

// newBackupInitPlan 生成预检计划。
func newBackupInitPlan(src backupSource) func(context.Context, PlanInput) (Plan, error) {
	return func(ctx context.Context, in PlanInput) (Plan, error) {
		params, ok := in.Params.(backupInitParams)
		if !ok {
			return Plan{}, fmt.Errorf("参数类型不匹配: %T", in.Params)
		}
		repo, found, err := src.GetRepository(ctx, params.RepositoryID)
		if err != nil {
			return Plan{}, err
		}
		if !found {
			// 找不到定义不是 500：它是“这个 id 在这台机器上没登记”，
			// 必须让管理员看见原因，而不是一个内部错误。
			return Plan{
				NormalizedParams: params,
				Target:           backupTarget(params, backup.Definition{}),
				Allowed:          false,
				Blockers: []Notice{{
					Code:    "BACKUP_REPOSITORY_NOT_FOUND",
					Message: fmt.Sprintf("代理里没有仓库 %s 的定义：先通过 /internal/v1/backup/repositories 登记", params.RepositoryID),
				}},
				Locks: []string{backup.LockName(params.RepositoryID)},
			}, nil
		}
		if repo.Version != params.Version {
			return Plan{
				NormalizedParams: params,
				Target:           backupTarget(params, repo.Definition),
				Allowed:          false,
				Blockers: []Notice{{
					Code: "BACKUP_REPOSITORY_VERSION_STALE",
					Message: fmt.Sprintf("仓库定义已经变成版本 %d（请求的是 %d）：重新预检一次",
						repo.Version, params.Version),
				}},
				Locks: []string{backup.LockName(params.RepositoryID)},
			}, nil
		}

		assessment, err := src.Assess(ctx, backup.AssessRequest{
			RepositoryID:       params.RepositoryID,
			Definition:         repo.Definition,
			Version:            repo.Version,
			ExcludeOperationID: in.OperationID,
		})
		if err != nil {
			return Plan{}, err
		}
		return planFromBackupAssessment(params, assessment), nil
	}
}

// newBackupInitExecute 生成执行器。
func newBackupInitExecute(src backupSource) func(context.Context, ExecuteInput) (Result, error) {
	return func(ctx context.Context, in ExecuteInput) (Result, error) {
		params, ok := in.Params.(backupInitParams)
		if !ok {
			return Result{}, fmt.Errorf("参数类型不匹配: %T", in.Params)
		}
		repo, found, err := src.GetRepository(ctx, params.RepositoryID)
		if err != nil {
			return Result{}, err
		}
		if !found {
			return Result{}, fmt.Errorf("%s：仓库定义在执行前不见了", "BACKUP_REPOSITORY_NOT_FOUND")
		}
		if repo.Version != params.Version {
			return Result{}, fmt.Errorf("%s：仓库定义在预检之后被改过（版本 %d → %d）",
				"BACKUP_REPOSITORY_VERSION_STALE", params.Version, repo.Version)
		}
		exec, err := src.Execute(ctx, backup.ExecuteRequest{
			RepositoryID: params.RepositoryID,
			Definition:   repo.Definition,
			OperationID:  in.OperationID,
			Progress:     in.Progress,
			Cancel:       in.Cancel,
		})
		if err != nil {
			// 三种“取消”都要认：本包的 ErrCanceled、ctx 自己结束、以及
			// backup 包在可中断点返回的 ErrCanceled。认漏一种，一次被管理员
			// 主动取消的操作就会被记成失败。
			if errors.Is(err, ErrCanceled) || errors.Is(err, context.Canceled) ||
				errors.Is(err, backup.ErrCanceled) {
				return Result{}, ErrCanceled
			}
			return Result{}, backupFailure{err: err}
		}
		if !exec.Verified {
			// 仓库可能建成了，但我们没读到它的 config。这种状态**不许**报成功：
			// 报 unknown 并说清楚下一步（重跑还是清理），由人决定。
			return Result{
				Outcome: OutcomeUnknown,
				ObservedState: map[string]any{
					"repository_id": params.RepositoryID,
					"location":      repo.Definition.Location.String(repo.Definition.Backend),
					"initialized":   exec.RepoID != "",
					"verify_error":  exec.VerifyError,
				},
				RecoveryHint: "restic 已经跑过 init，但读回 config 失败：" + exec.VerifyError +
					"；先确认目标位置是否已生成仓库（有 config 文件即为已建），再决定重跑还是清理该目录",
				Data: map[string]any{
					"repository_id": params.RepositoryID,
					"verified":      false,
				},
			}, nil
		}
		return backupVerifiedResult(params, repo, exec), nil
	}
}

// backupVerifiedResult 把核对通过的结果翻成任务的 result。
func backupVerifiedResult(params backupInitParams, repo backup.Repository, exec backup.Execution) Result {
	observed := map[string]any{
		"repository_id":   params.RepositoryID,
		"name":            repo.Name,
		"backend":         string(repo.Definition.Backend),
		"location":        repo.Definition.Location.String(repo.Definition.Backend),
		"repo_id":         exec.RepoID,
		"repo_version":    exec.RepoVersion,
		"restic_version":  exec.ResticVersion,
		"verified_by":     "restic cat config",
		"initialized_at":  exec.InitializedAt.Format(time.RFC3339),
		"duration_ms":     exec.Duration.Milliseconds(),
		"capacity_before": capacityState(exec.CapacityBefore),
		"capacity_after":  capacityState(exec.CapacityAfter),
	}
	if exec.AbsolutePath != "" {
		observed["absolute_path"] = exec.AbsolutePath
	}
	if exec.HostKey.Fingerprint != "" {
		observed["host_key"] = exec.HostKey.Type + " " + exec.HostKey.Fingerprint
		observed["known_hosts_written"] = exec.KnownHostsPath != ""
	}
	return Result{
		Outcome:       OutcomeVerified,
		VerifiedAt:    exec.InitializedAt,
		ObservedState: observed,
		RecoveryHint:  "仓库已建好。备份成功不等于恢复已验证：恢复验证要单独做一次（P12.3 的 backup.verify）",
		Data: map[string]any{
			"repository_id": params.RepositoryID,
			"repo_id":       exec.RepoID,
			"verified":      true,
		},
	}
}

// capacityState 把容量读数翻成 observed_state 里的简单值。
//
// 读不到就是读不到：`supported=false` + 原因，不写 0。
func capacityState(c backup.Capacity) map[string]any {
	out := map[string]any{"supported": c.Supported}
	if c.Supported {
		out["total_bytes"] = c.TotalBytes
		out["available_bytes"] = c.AvailableBytes
		out["target"] = c.Target
	} else if c.Error != "" {
		out["error"] = c.Error
	}
	return out
}

// planFromBackupAssessment 把 internal/backup 的结论逐字段翻成 Plan。
func planFromBackupAssessment(params backupInitParams, a backup.Assessment) Plan {
	blockers := make([]Notice, 0, len(a.Blockers))
	for _, b := range a.Blockers {
		blockers = append(blockers, Notice{Code: b.Code, Message: b.Message})
	}
	warnings := make([]Notice, 0, len(a.Warnings))
	for _, w := range a.Warnings {
		warnings = append(warnings, Notice{Code: w.Code, Message: w.Message})
	}
	impacts := make([]Impact, 0, len(a.Impacts))
	for _, i := range a.Impacts {
		impacts = append(impacts, Impact{Kind: i.Kind, Description: i.Description})
	}
	locks := a.Locks
	if len(locks) == 0 {
		locks = []string{backup.LockName(params.RepositoryID)}
	}
	plan := Plan{
		NormalizedParams: params,
		Target: Target{
			DisplayName:         a.Target.DisplayName,
			IdentityFingerprint: a.Target.IdentityFingerprint,
			StateVersion:        a.Target.StateVersion,
		},
		Allowed:  a.Allowed,
		Blockers: blockers,
		Warnings: warnings,
		Impacts:  impacts,
		Locks:    locks,
	}
	if !a.Capacity.Supported && a.Capacity.Error != "" {
		plan.Warnings = append(plan.Warnings, Notice{
			Code:    backup.CodeCapacityUnknown,
			Message: "容量未知：" + a.Capacity.Error,
		})
	}
	return plan
}

// backupTarget 在没有拿到评估结果时构造一个最小目标（阻塞项用得上）。
func backupTarget(params backupInitParams, def backup.Definition) Target {
	name := def.Name
	if name == "" {
		name = params.RepositoryID
	} else {
		name = def.DisplayName()
	}
	fingerprint := ""
	if def.Name != "" {
		if fp, err := def.IdentityFingerprint(); err == nil {
			fingerprint = fp
		}
	}
	return Target{
		DisplayName:         name,
		IdentityFingerprint: fingerprint,
		StateVersion:        fmt.Sprintf("backup-repository:%s:v%d", params.RepositoryID, params.Version),
	}
}

// backupFailure 让失败原因以一句人话进任务的 error.message。
type backupFailure struct{ err error }

func (f backupFailure) Error() string         { return f.err.Error() }
func (f backupFailure) Unwrap() error         { return f.err }
func (f backupFailure) FailureReason() string { return oneLine(f.err.Error(), 300) }
