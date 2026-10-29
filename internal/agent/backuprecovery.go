package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"upanel/internal/backup"
	"upanel/internal/files"
)

const backupRecoveryPlanBudget = 90 * time.Second

type backupRecoverySource interface {
	GetPolicy(context.Context, string) (backup.PolicyRecord, bool, error)
	GetRepository(context.Context, string) (backup.Repository, bool, error)
	GetSnapshot(context.Context, backup.SnapshotRequest) (backup.Snapshot, error)
	AssessRestore(context.Context, backup.RestoreRequest) (backup.RestoreAssessment, error)
	Restore(context.Context, backup.RestoreRequest) (backup.RestoreResult, error)
	Verify(context.Context, backup.VerifyRequest) (backup.VerifyResult, error)
	AssessPrune(context.Context, backup.PruneRequest) (backup.PruneAssessment, error)
	Prune(context.Context, backup.PruneRequest) (backup.PruneResult, error)
}

type backupRestoreParams struct {
	RepositoryID string           `json:"repository_id"`
	SnapshotID   string           `json:"snapshot_id"`
	TargetRootID string           `json:"target_root_id"`
	RelativePath string           `json:"relative_path"`
	Overwrite    bool             `json:"overwrite"`
	Apps         []backupAppParam `json:"apps,omitempty"`
}

type backupVerifyParams struct {
	RepositoryID  string `json:"repository_id"`
	SnapshotID    string `json:"snapshot_id"`
	SampleRestore bool   `json:"sample_restore"`
}

type backupPruneParams struct {
	PolicyID          string   `json:"backup_policy_id"`
	Version           int64    `json:"version"`
	ExpectedDeleteIDs []string `json:"expected_delete_ids,omitempty"`
}

func RegisterBackupRecoveryActions(reg *Registry, src backupRecoverySource) error {
	if src == nil {
		return errors.New("agent: 备份恢复/验证/清理没有实现端")
	}
	if err := reg.RegisterAction(Action{
		Name: backup.ActionRestore, PlanBudget: backupRecoveryPlanBudget,
		Decode: func(raw json.RawMessage) (any, error) {
			var p backupRestoreParams
			if err := DecodeParams(raw, &p); err != nil {
				return nil, err
			}
			p.RepositoryID = strings.TrimSpace(p.RepositoryID)
			p.SnapshotID = strings.ToLower(strings.TrimSpace(p.SnapshotID))
			p.TargetRootID = strings.TrimSpace(p.TargetRootID)
			if !backup.ValidRepositoryID(p.RepositoryID) || !backup.ValidSnapshotID(p.SnapshotID) || !backup.ValidRootID(p.TargetRootID) {
				return nil, fmt.Errorf("%w: repository_id、snapshot_id 或 target_root_id 形状不合法", ErrParamsInvalid)
			}
			if err := files.ValidateRelPath(p.RelativePath); err != nil || p.RelativePath == "" {
				return nil, fmt.Errorf("%w: relative_path 必须是根下非空目录", ErrParamsInvalid)
			}
			apps, err := normalizeApps(p.Apps)
			if err != nil {
				return nil, err
			}
			if !p.Overwrite && len(apps) != 0 {
				return nil, fmt.Errorf("%w: 只有覆盖恢复可以携带依赖应用", ErrParamsInvalid)
			}
			p.Apps = apps
			return p, nil
		},
		Plan: newBackupRestorePlan(src), Execute: newBackupRestoreExecute(src),
		PolicyFor: func(value any) (ActionPolicy, error) {
			p, ok := value.(backupRestoreParams)
			if !ok {
				return ActionPolicy{}, fmt.Errorf("参数类型不匹配: %T", value)
			}
			policy := ActionPolicy{RequiresReauth: true, Cancellable: true}
			if p.Overwrite {
				policy.RequiresPhrase, policy.PhraseVerb = true, PhraseVerbOverwrite
			}
			return policy, nil
		},
	}); err != nil {
		return err
	}
	if err := reg.RegisterAction(Action{
		Name: backup.ActionVerify, PlanBudget: backupRecoveryPlanBudget,
		Decode: func(raw json.RawMessage) (any, error) {
			var p backupVerifyParams
			if err := DecodeParams(raw, &p); err != nil {
				return nil, err
			}
			p.RepositoryID = strings.TrimSpace(p.RepositoryID)
			p.SnapshotID = strings.ToLower(strings.TrimSpace(p.SnapshotID))
			if !backup.ValidRepositoryID(p.RepositoryID) || !backup.ValidSnapshotID(p.SnapshotID) {
				return nil, fmt.Errorf("%w: repository_id 或 snapshot_id 形状不合法", ErrParamsInvalid)
			}
			return p, nil
		},
		Plan: newBackupVerifyPlan(src), Execute: newBackupVerifyExecute(src),
		Policy: ActionPolicy{Cancellable: true},
	}); err != nil {
		return err
	}
	return reg.RegisterAction(Action{
		Name: backup.ActionPrune, PlanBudget: backupRecoveryPlanBudget,
		Decode: func(raw json.RawMessage) (any, error) {
			var p backupPruneParams
			if err := DecodeParams(raw, &p); err != nil {
				return nil, err
			}
			p.PolicyID = strings.TrimSpace(p.PolicyID)
			if !backup.ValidPolicyID(p.PolicyID) || p.Version < 1 {
				return nil, fmt.Errorf("%w: backup_policy_id 或 version 不合法", ErrParamsInvalid)
			}
			for _, id := range p.ExpectedDeleteIDs {
				if !backup.ValidSnapshotID(id) {
					return nil, fmt.Errorf("%w: 预检待清理快照 id 形状不合法", ErrParamsInvalid)
				}
			}
			return p, nil
		},
		Plan: newBackupPrunePlan(src), Execute: newBackupPruneExecute(src),
		Policy: ActionPolicy{RequiresReauth: true, RequiresPhrase: true, PhraseVerb: PhraseVerbDelete},
	})
}

func repositoryForAction(ctx context.Context, src backupRecoverySource, id string) (backup.Repository, *Plan, error) {
	repo, found, err := src.GetRepository(ctx, id)
	if err != nil {
		return backup.Repository{}, nil, err
	}
	if !found {
		plan := Plan{Allowed: false, Blockers: []Notice{{Code: backup.CodeRepositoryMissing, Message: "代理里没有这条备份仓库定义"}}, Locks: []string{backup.LockName(id)}}
		return backup.Repository{}, &plan, nil
	}
	return repo, nil, nil
}

func newBackupRestorePlan(src backupRecoverySource) func(context.Context, PlanInput) (Plan, error) {
	return func(ctx context.Context, in PlanInput) (Plan, error) {
		p := in.Params.(backupRestoreParams)
		repo, blocked, err := repositoryForAction(ctx, src, p.RepositoryID)
		if blocked != nil || err != nil {
			if blocked != nil {
				blocked.NormalizedParams = p
				return *blocked, nil
			}
			return Plan{}, err
		}
		a, err := src.AssessRestore(ctx, backup.RestoreRequest{RepositoryID: p.RepositoryID, Repository: repo,
			SnapshotID: p.SnapshotID, TargetRootID: p.TargetRootID, RelativePath: p.RelativePath,
			Overwrite: p.Overwrite, Apps: appTargetsOf(p.Apps)})
		if err != nil {
			return Plan{}, err
		}
		return Plan{NormalizedParams: p, Allowed: a.Allowed, Blockers: noticesOf(a.Blockers),
			Warnings: noticesOf(a.Warnings), Impacts: impactsOf(a.Impacts), Locks: a.Locks,
			Target: Target{DisplayName: a.Target.DisplayName, IdentityFingerprint: a.Target.IdentityFingerprint, StateVersion: a.Target.StateVersion}}, nil
	}
}

func newBackupRestoreExecute(src backupRecoverySource) func(context.Context, ExecuteInput) (Result, error) {
	return func(ctx context.Context, in ExecuteInput) (Result, error) {
		p := in.Params.(backupRestoreParams)
		repo, found, err := src.GetRepository(ctx, p.RepositoryID)
		if err != nil || !found {
			return Result{}, fmt.Errorf("BACKUP_REPOSITORY_MISSING：执行前仓库定义不可用: %v", err)
		}
		req := backup.RestoreRequest{RepositoryID: p.RepositoryID, Repository: repo, SnapshotID: p.SnapshotID,
			TargetRootID: p.TargetRootID, RelativePath: p.RelativePath, Overwrite: p.Overwrite,
			Apps: appTargetsOf(p.Apps), Cancel: in.Cancel, Progress: in.Progress}
		a, err := src.AssessRestore(ctx, req)
		if err != nil {
			return Result{}, err
		}
		if !a.Allowed {
			return Result{}, fmt.Errorf("PREFLIGHT_STALE：恢复执行前复核不通过：%s", joinReasons(a.Blockers))
		}
		if a.Target.StateVersion != in.Target.StateVersion || a.Target.IdentityFingerprint != in.Target.IdentityFingerprint {
			return Result{}, errors.New("PREFLIGHT_STALE：恢复快照或目标目录状态在预检后发生变化")
		}
		out, err := src.Restore(ctx, req)
		if err != nil && out.Published {
			return Result{Outcome: OutcomePartial, ObservedState: restoreObserved(out),
				RecoveryHint: "目录已经发布并验证；请按结果中的应用状态手工恢复未拉起的依赖应用。错误：" + err.Error(),
				Data:         map[string]any{"published": true, "apps_restored": out.AppsRestored}}, nil
		}
		if err != nil {
			return Result{}, err
		}
		return Result{Outcome: OutcomeVerified, VerifiedAt: out.FinishedAt, ObservedState: restoreObserved(out),
			Data: map[string]any{"snapshot_id": out.SnapshotID, "published": out.Published, "cleanup_error": out.CleanupError}}, nil
	}
}

func restoreObserved(out backup.RestoreResult) map[string]any {
	return map[string]any{"repository_id": out.RepositoryID, "snapshot_id": out.SnapshotID,
		"target_root_id": out.TargetRootID, "relative_path": out.RelativePath,
		"published": out.Published, "verified": out.Verified, "files": out.Files,
		"directories": out.Directories, "bytes": out.Bytes, "apps_restored": out.AppsRestored}
}

func newBackupVerifyPlan(src backupRecoverySource) func(context.Context, PlanInput) (Plan, error) {
	return func(ctx context.Context, in PlanInput) (Plan, error) {
		p := in.Params.(backupVerifyParams)
		repo, blocked, err := repositoryForAction(ctx, src, p.RepositoryID)
		if blocked != nil || err != nil {
			if blocked != nil {
				blocked.NormalizedParams = p
				return *blocked, nil
			}
			return Plan{}, err
		}
		snap, err := src.GetSnapshot(ctx, backup.SnapshotRequest{RepositoryID: p.RepositoryID, Definition: repo.Definition, SnapshotID: p.SnapshotID})
		target, targetErr := backupVerifyTarget(p, repo)
		if targetErr != nil {
			return Plan{}, targetErr
		}
		plan := Plan{NormalizedParams: p, Locks: []string{backup.LockName(p.RepositoryID)}, Target: target}
		if errors.Is(err, backup.ErrSnapshotNotFound) {
			plan.Allowed = false
			plan.Blockers = []Notice{{Code: "BACKUP_SNAPSHOT_NOT_FOUND", Message: "指定快照不在该仓库中"}}
			return plan, nil
		}
		if err != nil {
			return Plan{}, err
		}
		plan.Allowed = true
		plan.Impacts = []Impact{{Kind: "repository_check", Description: "检查仓库结构；不会修改快照"}}
		if p.SampleRestore {
			plan.Impacts = []Impact{{Kind: "sample_restore", Description: fmt.Sprintf("在代理私有临时目录抽样恢复最多 %d 个文件、%d 字节，逐文件比对 SHA-256 后删除", backup.VerifySampleMaxFiles, backup.VerifySampleMaxTotalBytes)}}
		}
		plan.Target.DisplayName = snap.ID
		return plan, nil
	}
}

func backupVerifyTarget(p backupVerifyParams, repo backup.Repository) (Target, error) {
	def, err := repo.Definition.Normalize()
	if err != nil {
		return Target{}, err
	}
	fingerprint, err := def.IdentityFingerprint()
	if err != nil {
		return Target{}, err
	}
	return Target{DisplayName: p.SnapshotID, IdentityFingerprint: p.SnapshotID + "|" + fingerprint,
		StateVersion: fmt.Sprintf("snapshot:%s|repository:%s:v%d", p.SnapshotID, repo.ID, repo.Version)}, nil
}

func newBackupVerifyExecute(src backupRecoverySource) func(context.Context, ExecuteInput) (Result, error) {
	return func(ctx context.Context, in ExecuteInput) (Result, error) {
		p := in.Params.(backupVerifyParams)
		repo, found, err := src.GetRepository(ctx, p.RepositoryID)
		if err != nil || !found {
			return Result{}, fmt.Errorf("BACKUP_REPOSITORY_MISSING：执行前仓库定义不可用: %v", err)
		}
		if _, err := src.GetSnapshot(ctx, backup.SnapshotRequest{RepositoryID: p.RepositoryID,
			Definition: repo.Definition, SnapshotID: p.SnapshotID}); err != nil {
			return Result{}, fmt.Errorf("PREFLIGHT_STALE：验证执行前快照不可读: %w", err)
		}
		target, err := backupVerifyTarget(p, repo)
		if err != nil {
			return Result{}, err
		}
		if target.StateVersion != in.Target.StateVersion || target.IdentityFingerprint != in.Target.IdentityFingerprint {
			return Result{}, errors.New("PREFLIGHT_STALE：验证执行前快照或仓库定义已经变化")
		}
		out, err := src.Verify(ctx, backup.VerifyRequest{RepositoryID: p.RepositoryID, Repository: repo,
			SnapshotID: p.SnapshotID, SampleRestore: p.SampleRestore})
		if err != nil {
			return Result{}, err
		}
		at := in.Now
		if out.VerifiedAt != nil {
			at = *out.VerifiedAt
		}
		return Result{Outcome: OutcomeVerified, VerifiedAt: at, ObservedState: map[string]any{
			"repository_id": out.RepositoryID, "snapshot_id": out.SnapshotID, "checked": out.Checked,
			"sample_restore": out.SampleRestore, "restore_verified": out.Verified,
			"files_checked": out.FilesChecked, "bytes_checked": out.BytesChecked}}, nil
	}
}

func newBackupPrunePlan(src backupRecoverySource) func(context.Context, PlanInput) (Plan, error) {
	return func(ctx context.Context, in PlanInput) (Plan, error) {
		p := in.Params.(backupPruneParams)
		rec, found, err := src.GetPolicy(ctx, p.PolicyID)
		if err != nil {
			return Plan{}, err
		}
		if !found || rec.Version != p.Version {
			return Plan{NormalizedParams: p, Allowed: false, Blockers: []Notice{{Code: "BACKUP_POLICY_VERSION_STALE",
				Message: "策略不存在或版本已变化，请重新预检"}}, Locks: []string{backup.PolicyLockName(p.PolicyID)}}, nil
		}
		repo, blocked, err := repositoryForAction(ctx, src, rec.Policy.RepositoryID)
		if blocked != nil || err != nil {
			if blocked != nil {
				blocked.NormalizedParams = p
				return *blocked, nil
			}
			return Plan{}, err
		}
		a, err := src.AssessPrune(ctx, backup.PruneRequest{PolicyID: p.PolicyID, Policy: rec.Policy, Repository: repo})
		if err != nil {
			return Plan{}, err
		}
		p.ExpectedDeleteIDs = snapshotIDStrings(a.Delete)
		impacts := []Impact{{Kind: "delete_snapshots", Description: fmt.Sprintf("按保留策略显式删除 %d 个快照并清理无引用数据；保留 %d 个；不会触碰其他策略或其他仓库", len(a.Delete), len(a.Keep))}}
		return Plan{NormalizedParams: p, Allowed: a.Allowed, Blockers: noticesOf(a.Blockers), Impacts: impacts,
			Locks: a.Locks, Target: Target{DisplayName: a.Target.DisplayName,
				IdentityFingerprint: a.Target.IdentityFingerprint, StateVersion: a.Target.StateVersion}}, nil
	}
}

func newBackupPruneExecute(src backupRecoverySource) func(context.Context, ExecuteInput) (Result, error) {
	return func(ctx context.Context, in ExecuteInput) (Result, error) {
		p := in.Params.(backupPruneParams)
		rec, found, err := src.GetPolicy(ctx, p.PolicyID)
		if err != nil || !found || rec.Version != p.Version {
			return Result{}, errors.New("BACKUP_POLICY_VERSION_STALE：策略在执行前不存在或版本变化，没有删除快照")
		}
		repo, found, err := src.GetRepository(ctx, rec.Policy.RepositoryID)
		if err != nil || !found {
			return Result{}, errors.New("BACKUP_REPOSITORY_MISSING：仓库在执行前不可用，没有删除快照")
		}
		a, err := src.AssessPrune(ctx, backup.PruneRequest{PolicyID: p.PolicyID, Policy: rec.Policy, Repository: repo})
		if err != nil {
			return Result{}, err
		}
		if !a.Allowed {
			return Result{}, fmt.Errorf("PREFLIGHT_STALE：清理执行前复核不通过：%s", joinReasons(a.Blockers))
		}
		if a.Target.StateVersion != in.Target.StateVersion || a.Target.IdentityFingerprint != in.Target.IdentityFingerprint ||
			!equalSnapshotIDs(snapshotIDStrings(a.Delete), p.ExpectedDeleteIDs) {
			return Result{}, errors.New("PREFLIGHT_STALE：清理执行前策略、仓库或待删快照集合已经变化，没有删除快照")
		}
		out, err := src.Prune(ctx, backup.PruneRequest{PolicyID: p.PolicyID, Policy: rec.Policy,
			Repository: repo, ExpectedDeleteIDs: p.ExpectedDeleteIDs, Cancel: in.Cancel})
		if err != nil && len(out.DeletedIDs) > 0 {
			return Result{Outcome: OutcomeUnknown, ObservedState: map[string]any{"deleted_ids": out.DeletedIDs, "verified": false},
				RecoveryHint: "部分快照可能已删除；先重新读取快照列表，不要直接重跑清理。错误：" + err.Error()}, nil
		}
		if err != nil {
			return Result{}, err
		}
		return Result{Outcome: OutcomeVerified, VerifiedAt: out.FinishedAt, ObservedState: map[string]any{
			"policy_id": out.PolicyID, "repository_id": out.RepositoryID, "deleted_ids": out.DeletedIDs,
			"kept_ids": out.KeptIDs, "verified": out.Verified}}, nil
	}
}

func equalSnapshotIDs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func snapshotIDStrings(snapshots []backup.Snapshot) []string {
	out := make([]string, 0, len(snapshots))
	for _, snap := range snapshots {
		out = append(out, snap.ID)
	}
	return out
}
func noticesOf(in []backup.Reason) []Notice {
	out := make([]Notice, 0, len(in))
	for _, r := range in {
		out = append(out, Notice{Code: r.Code, Message: r.Message})
	}
	return out
}
func impactsOf(in []backup.Impact) []Impact {
	out := make([]Impact, 0, len(in))
	for _, i := range in {
		out = append(out, Impact{Kind: i.Kind, Description: i.Description})
	}
	return out
}
func joinReasons(in []backup.Reason) string {
	parts := make([]string, 0, len(in))
	for _, r := range in {
		parts = append(parts, r.Code+":"+r.Message)
	}
	return strings.Join(parts, "；")
}
