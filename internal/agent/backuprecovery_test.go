package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"upanel/internal/backup"
)

type fakeBackupRecovery struct {
	*fakeBackupRun
	restoreAssessment backup.RestoreAssessment
	restoreAssessErr  error
	restoreOut        backup.RestoreResult
	restoreErr        error
	restoreCalls      int
	verifyOut         backup.VerifyResult
	verifyErr         error
	verifyCalls       int
	pruneAssessment   backup.PruneAssessment
	pruneAssessErr    error
	pruneOut          backup.PruneResult
	pruneErr          error
	pruneCalls        int
}

func newFakeBackupRecovery(t *testing.T) *fakeBackupRecovery {
	t.Helper()
	base := newFakeBackupRun(t)
	target := backup.Target{DisplayName: "root1:/restored", IdentityFingerprint: runTestSnap + "|repo", StateVersion: "snapshot:" + runTestSnap + "|target:v1|repository:" + testRepoID + ":v3"}
	return &fakeBackupRecovery{
		fakeBackupRun: base,
		restoreAssessment: backup.RestoreAssessment{Allowed: true, Target: target,
			Locks: []string{backup.LockName(testRepoID)}, Snapshot: base.snapshot},
		restoreOut: backup.RestoreResult{RepositoryID: testRepoID, SnapshotID: runTestSnap,
			Published: true, Verified: true, AppsRestored: true, FinishedAt: time.Date(2026, 10, 24, 5, 0, 0, 0, time.UTC)},
		verifyOut: backup.VerifyResult{RepositoryID: testRepoID, SnapshotID: runTestSnap, Checked: true},
		pruneAssessment: backup.PruneAssessment{Allowed: true,
			Target: backup.Target{DisplayName: "policy", IdentityFingerprint: "delete|repo", StateVersion: "policy|delete|repository:v3"},
			Delete: []backup.Snapshot{{ID: runTestSnap}}, Keep: []backup.Snapshot{{ID: strings.Repeat("2", 64)}}},
		pruneOut: backup.PruneResult{PolicyID: runTestPolicyID, RepositoryID: testRepoID,
			DeletedIDs: []string{runTestSnap}, KeptIDs: []string{strings.Repeat("2", 64)}, Verified: true,
			FinishedAt: time.Date(2026, 10, 24, 6, 0, 0, 0, time.UTC)},
	}
}

func (f *fakeBackupRecovery) AssessRestore(_ context.Context, _ backup.RestoreRequest) (backup.RestoreAssessment, error) {
	return f.restoreAssessment, f.restoreAssessErr
}
func (f *fakeBackupRecovery) Restore(_ context.Context, _ backup.RestoreRequest) (backup.RestoreResult, error) {
	f.restoreCalls++
	return f.restoreOut, f.restoreErr
}
func (f *fakeBackupRecovery) Verify(_ context.Context, _ backup.VerifyRequest) (backup.VerifyResult, error) {
	f.verifyCalls++
	return f.verifyOut, f.verifyErr
}
func (f *fakeBackupRecovery) AssessPrune(_ context.Context, _ backup.PruneRequest) (backup.PruneAssessment, error) {
	return f.pruneAssessment, f.pruneAssessErr
}
func (f *fakeBackupRecovery) Prune(_ context.Context, _ backup.PruneRequest) (backup.PruneResult, error) {
	f.pruneCalls++
	return f.pruneOut, f.pruneErr
}

func newBackupRecoveryRegistry(t *testing.T, src *fakeBackupRecovery) *Registry {
	t.Helper()
	reg := NewRegistry()
	if err := RegisterBackupRecoveryActions(reg, src); err != nil {
		t.Fatal(err)
	}
	return reg
}

func restoreParams() backupRestoreParams {
	return backupRestoreParams{RepositoryID: testRepoID, SnapshotID: runTestSnap,
		TargetRootID: "root1", RelativePath: "restored"}
}

func TestBackupRecoveryPoliciesAndStrictDecode(t *testing.T) {
	reg := newBackupRecoveryRegistry(t, newFakeBackupRecovery(t))
	restore, _ := reg.Action(backup.ActionRestore)
	decoded, err := restore.Decode(json.RawMessage(`{"repository_id":"` + testRepoID + `","snapshot_id":"` + runTestSnap + `","target_root_id":"root1","relative_path":"restored","overwrite":true}`))
	if err != nil {
		t.Fatal(err)
	}
	policy, err := restore.ResolvePolicy(decoded)
	if err != nil || !policy.RequiresReauth || !policy.RequiresPhrase || policy.PhraseVerb != PhraseVerbOverwrite || !policy.Cancellable {
		t.Fatalf("覆盖恢复门槛不正确: %+v, %v", policy, err)
	}
	params := restoreParams()
	policy, err = restore.ResolvePolicy(params)
	if err != nil || !policy.RequiresReauth || policy.RequiresPhrase {
		t.Fatalf("新目录恢复不应要求覆盖短语: %+v, %v", policy, err)
	}
	if _, err := restore.Decode(json.RawMessage(`{"repository_id":"` + testRepoID + `","snapshot_id":"` + runTestSnap[:8] + `","target_root_id":"root1","relative_path":"restored","overwrite":false}`)); !errors.Is(err, ErrParamsInvalid) {
		t.Fatalf("短快照 id 不应获授权: %v", err)
	}
	verify, _ := reg.Action(backup.ActionVerify)
	policy, err = verify.ResolvePolicy(nil)
	if err != nil || policy.RequiresReauth || policy.RequiresPhrase || !policy.Cancellable {
		t.Fatalf("验证动作策略不正确: %+v, %v", policy, err)
	}
	prune, _ := reg.Action(backup.ActionPrune)
	policy, err = prune.ResolvePolicy(nil)
	if err != nil || !policy.RequiresReauth || !policy.RequiresPhrase || policy.PhraseVerb != PhraseVerbDelete {
		t.Fatalf("清理动作门槛不正确: %+v, %v", policy, err)
	}
}

func TestBackupRestoreExecuteRechecksTargetAndReportsPartial(t *testing.T) {
	src := newFakeBackupRecovery(t)
	reg := newBackupRecoveryRegistry(t, src)
	action, _ := reg.Action(backup.ActionRestore)
	plan, err := action.Plan(context.Background(), PlanInput{Params: restoreParams()})
	if err != nil || !plan.Allowed {
		t.Fatalf("预检失败: %+v, %v", plan, err)
	}
	src.restoreAssessment.Target.StateVersion = "changed"
	_, err = action.Execute(context.Background(), ExecuteInput{Params: plan.NormalizedParams, Target: plan.Target})
	if err == nil || !strings.Contains(err.Error(), "PREFLIGHT_STALE") || src.restoreCalls != 0 {
		t.Fatalf("目标变化后不应恢复: %v, calls=%d", err, src.restoreCalls)
	}

	src = newFakeBackupRecovery(t)
	reg = newBackupRecoveryRegistry(t, src)
	action, _ = reg.Action(backup.ActionRestore)
	plan, _ = action.Plan(context.Background(), PlanInput{Params: restoreParams()})
	src.restoreErr = errors.New("应用未全部拉起")
	src.restoreOut.AppsRestored = false
	result, err := action.Execute(context.Background(), ExecuteInput{Params: plan.NormalizedParams, Target: plan.Target})
	if err != nil || result.Outcome != OutcomePartial || result.ObservedState["published"] != true {
		t.Fatalf("已发布但应用未恢复必须是 partial: %+v, %v", result, err)
	}
}

func TestBackupPruneNormalizedParamsSurviveSecondDecode(t *testing.T) {
	src := newFakeBackupRecovery(t)
	reg := newBackupRecoveryRegistry(t, src)
	action, _ := reg.Action(backup.ActionPrune)
	raw := json.RawMessage(`{"backup_policy_id":"` + runTestPolicyID + `","version":4}`)
	decoded, err := action.Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := action.Plan(context.Background(), PlanInput{Params: decoded})
	if err != nil || !plan.Allowed {
		t.Fatalf("清理预检失败: %+v, %v", plan, err)
	}
	normalized, err := json.Marshal(plan.NormalizedParams)
	if err != nil {
		t.Fatal(err)
	}
	executionParams, err := action.Decode(normalized)
	if err != nil {
		t.Fatalf("执行阶段二次解码拒绝预检冻结参数: %v", err)
	}
	result, err := action.Execute(context.Background(), ExecuteInput{Params: executionParams, Target: plan.Target})
	if err != nil || result.Outcome != OutcomeVerified || src.pruneCalls != 1 {
		t.Fatalf("清理执行失败: %+v, %v, calls=%d", result, err, src.pruneCalls)
	}
}

func TestBackupPruneExecuteRefusesChangedDeleteSet(t *testing.T) {
	src := newFakeBackupRecovery(t)
	reg := newBackupRecoveryRegistry(t, src)
	action, _ := reg.Action(backup.ActionPrune)
	decoded, _ := action.Decode(json.RawMessage(`{"backup_policy_id":"` + runTestPolicyID + `","version":4}`))
	plan, err := action.Plan(context.Background(), PlanInput{Params: decoded})
	if err != nil {
		t.Fatal(err)
	}
	src.pruneAssessment.Delete = nil
	_, err = action.Execute(context.Background(), ExecuteInput{Params: plan.NormalizedParams, Target: plan.Target})
	if err == nil || !strings.Contains(err.Error(), "PREFLIGHT_STALE") || src.pruneCalls != 0 {
		t.Fatalf("待删集合变化不应执行删除: %v, calls=%d", err, src.pruneCalls)
	}
}

func TestBackupPrunePartialDeletionIsUnknown(t *testing.T) {
	src := newFakeBackupRecovery(t)
	src.pruneErr = errors.New("清理后读回失败")
	reg := newBackupRecoveryRegistry(t, src)
	action, _ := reg.Action(backup.ActionPrune)
	decoded, _ := action.Decode(json.RawMessage(`{"backup_policy_id":"` + runTestPolicyID + `","version":4}`))
	plan, _ := action.Plan(context.Background(), PlanInput{Params: decoded})
	result, err := action.Execute(context.Background(), ExecuteInput{Params: plan.NormalizedParams, Target: plan.Target})
	if err != nil || result.Outcome != OutcomeUnknown || result.ObservedState["verified"] != false {
		t.Fatalf("部分清理状态必须是 unknown: %+v, %v", result, err)
	}
}

func TestBackupVerifyExecuteRefusesChangedRepository(t *testing.T) {
	src := newFakeBackupRecovery(t)
	reg := newBackupRecoveryRegistry(t, src)
	action, _ := reg.Action(backup.ActionVerify)
	params := backupVerifyParams{RepositoryID: testRepoID, SnapshotID: runTestSnap}
	plan, err := action.Plan(context.Background(), PlanInput{Params: params})
	if err != nil || !plan.Allowed {
		t.Fatalf("验证预检失败: %+v, %v", plan, err)
	}
	src.repo.Version++
	_, err = action.Execute(context.Background(), ExecuteInput{Params: plan.NormalizedParams, Target: plan.Target})
	if err == nil || !strings.Contains(err.Error(), "PREFLIGHT_STALE") || src.verifyCalls != 0 {
		t.Fatalf("仓库版本变化不应执行验证: %v, calls=%d", err, src.verifyCalls)
	}
}

func TestBackupStructuralCheckDoesNotClaimRestoreVerification(t *testing.T) {
	src := newFakeBackupRecovery(t)
	reg := newBackupRecoveryRegistry(t, src)
	action, _ := reg.Action(backup.ActionVerify)
	params := backupVerifyParams{RepositoryID: testRepoID, SnapshotID: runTestSnap, SampleRestore: false}
	plan, _ := action.Plan(context.Background(), PlanInput{Params: params})
	result, err := action.Execute(context.Background(), ExecuteInput{Params: plan.NormalizedParams, Target: plan.Target})
	if err != nil || result.ObservedState["checked"] != true || result.ObservedState["restore_verified"] != false {
		t.Fatalf("结构检查不能冒充抽样恢复验证: %+v, %v", result, err)
	}
}
