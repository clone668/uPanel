//go:build linux

package backup_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"upanel/internal/backup"
	"upanel/internal/files"
)

const (
	integrationRepoID   = "rep_0123456789abcdef0123456789abcdef"
	integrationPolicyID = "pol_0123456789abcdef0123456789abcdef"
	integrationSecret   = "sec_0123456789abcdef0123456789abcdef"
)

type integrationRoots struct{ root string }

func (r integrationRoots) ResolveRoot(id string) (string, bool, bool) {
	if id != "root1" {
		return "", false, true
	}
	return r.root, true, false
}

type integrationSecrets struct{ password string }

func (s *integrationSecrets) Resolve(context.Context, string) ([]byte, error) {
	return []byte(s.password), nil
}
func (s *integrationSecrets) Purpose(context.Context, string) (string, bool, error) {
	return "backup_repository_password", true, nil
}

type integrationVerifications struct {
	records map[string]backup.RestoreVerification
}

func (s *integrationVerifications) MarkRestoreVerified(_ context.Context, record backup.RestoreVerification) error {
	s.records[record.RepositoryID+"/"+record.SnapshotID] = record
	return nil
}
func (s *integrationVerifications) RestoreVerification(_ context.Context, repositoryID, snapshotID string) (backup.RestoreVerification, bool, error) {
	record, ok := s.records[repositoryID+"/"+snapshotID]
	return record, ok, nil
}

type integrationRestoreFS struct{ manager *files.Manager }

func (r integrationRestoreFS) ProbeRestore(ctx context.Context, params backup.RestoreParams) (backup.RestoreProbe, error) {
	return r.manager.ProbeRestore(ctx, params)
}
func (r integrationRestoreFS) BeginRestore(ctx context.Context, params backup.RestoreParams) (backup.RestoreSession, error) {
	return r.manager.BeginRestore(ctx, params)
}

// TestRecoveryCoreOnRealRestic 是 P12.3 核心端到端，不用 fakeRunner。它在独立 Ubuntu
// VM 上跑真实 restic、openat2/renameat2 和磁盘字节；agent/API 的授权翻译另由专项测试覆盖。
func TestRecoveryCoreOnRealRestic(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("覆盖恢复与所有权归一需要生产身份 root")
	}
	if _, err := os.Stat("/usr/bin/restic"); err != nil {
		t.Skip("真实 restic 未安装")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	root := t.TempDir()
	state := filepath.Join(root, "agent-state")
	cache := filepath.Join(root, "restic-cache")
	for _, dir := range []string{state, cache, filepath.Join(root, "data")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	manager, err := files.New([]files.Root{{ID: "root1", Name: "test", Path: root, Writable: true}}, files.Options{})
	if err != nil {
		t.Fatal(err)
	}
	secrets := &integrationSecrets{password: "p12-3-integration-repository"}
	verifications := &integrationVerifications{records: make(map[string]backup.RestoreVerification)}
	restic := backup.NewRestic(backup.Paths{Restic: "/usr/bin/restic", StateDir: state, CacheDir: cache, RunDir: state}, nil)
	ops, err := backup.NewOps(backup.Options{Restic: restic, Roots: integrationRoots{root: root}, Secrets: secrets,
		Paths:    backup.Paths{Restic: "/usr/bin/restic", StateDir: state, CacheDir: cache, RunDir: state},
		Restores: integrationRestoreFS{manager: manager}, Verifications: verifications})
	if err != nil {
		t.Fatal(err)
	}
	definition, err := (backup.Definition{Name: "integration", Backend: backup.BackendLocal,
		Location: backup.Location{RootID: "root1", RelativePath: "repository"}, PasswordSecretRef: integrationSecret}).Normalize()
	if err != nil {
		t.Fatal(err)
	}
	repository := backup.Repository{ID: integrationRepoID, Name: definition.Name, Definition: definition, Version: 1}
	inv := backup.Invocation{Repository: filepath.Join(root, "repository"), Password: secrets.password, Timeout: time.Minute}
	if _, err := restic.Init(ctx, inv); err != nil {
		t.Fatal(err)
	}
	policy, err := (backup.Policy{Name: "data", RepositoryID: integrationRepoID,
		Sources: []backup.Source{{RootID: "root1", RelativePath: "data"}}, ConsistencyMode: backup.ConsistencyFilesOnly,
		Retention: backup.Retention{KeepLast: 1}}).Normalize()
	if err != nil {
		t.Fatal(err)
	}
	payload := filepath.Join(root, "data", "payload.txt")
	outside := filepath.Join(root, "outside.txt")
	if err := os.WriteFile(payload, []byte("version one\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outside, []byte("must not change\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(operation string) backup.RunResult {
		t.Helper()
		out, err := ops.Run(ctx, backup.RunRequest{PolicyID: integrationPolicyID, Policy: policy,
			Repository: repository, OperationID: operation})
		if err != nil || !out.Verified || out.SnapshotID == "" {
			t.Fatalf("真实备份未读回验证: %+v, %v", out, err)
		}
		return out
	}
	first := run("op-first")
	if err := os.WriteFile(payload, []byte("version two\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "data", "over-sample-limit.bin"), make([]byte, backup.VerifySampleMaxFileBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	second := run("op-second")

	assessment, err := ops.AssessRestore(ctx, backup.RestoreRequest{RepositoryID: integrationRepoID, Repository: repository,
		SnapshotID: second.SnapshotID, TargetRootID: "root1", RelativePath: "restored"})
	if err != nil || !assessment.Allowed {
		t.Fatalf("新目录恢复预检失败: %+v, %v", assessment, err)
	}
	restored, err := ops.Restore(ctx, backup.RestoreRequest{RepositoryID: integrationRepoID, Repository: repository,
		SnapshotID: second.SnapshotID, TargetRootID: "root1", RelativePath: "restored"})
	if err != nil || !restored.Published || !restored.Verified {
		t.Fatalf("新目录恢复失败: %+v, %v", restored, err)
	}
	restoredPayload := filepath.Join(root, "restored", root, "data", "payload.txt")
	assertFileContent(t, restoredPayload, "version two\n")
	assertFileContent(t, outside, "must not change\n")

	blocked, err := ops.AssessRestore(ctx, backup.RestoreRequest{RepositoryID: integrationRepoID, Repository: repository,
		SnapshotID: second.SnapshotID, TargetRootID: "root1", RelativePath: "restored"})
	if err != nil || blocked.Allowed || len(blocked.Blockers) == 0 {
		t.Fatalf("默认恢复非空目录应阻塞: %+v, %v", blocked, err)
	}
	stale := filepath.Join(root, "restored", "stale.txt")
	if err := os.WriteFile(stale, []byte("old target\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	overwritten, err := ops.Restore(ctx, backup.RestoreRequest{RepositoryID: integrationRepoID, Repository: repository,
		SnapshotID: second.SnapshotID, TargetRootID: "root1", RelativePath: "restored", Overwrite: true})
	if err != nil || !overwritten.Published || !overwritten.Verified {
		t.Fatalf("覆盖恢复失败: %+v, %v", overwritten, err)
	}
	if _, err := os.Lstat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("覆盖恢复必须替换整个目录，旧文件仍存在: %v", err)
	}
	assertFileContent(t, restoredPayload, "version two\n")
	assertFileContent(t, outside, "must not change\n")

	structural, err := ops.Verify(ctx, backup.VerifyRequest{RepositoryID: integrationRepoID, Repository: repository,
		SnapshotID: second.SnapshotID, SampleRestore: false})
	if err != nil || !structural.Checked || structural.Verified {
		t.Fatalf("结构检查不应写成恢复验证: %+v, %v", structural, err)
	}
	if _, found, _ := verifications.RestoreVerification(ctx, integrationRepoID, second.SnapshotID); found {
		t.Fatal("sample_restore=false 写入了恢复验证记录")
	}
	sampled, err := ops.Verify(ctx, backup.VerifyRequest{RepositoryID: integrationRepoID, Repository: repository,
		SnapshotID: second.SnapshotID, SampleRestore: true})
	if err != nil || !sampled.Verified || sampled.FilesChecked != 1 || sampled.BytesChecked != int64(len("version two\n")) {
		t.Fatalf("抽样恢复验证或空间限额不正确: %+v, %v", sampled, err)
	}
	snapshot, err := ops.GetSnapshot(ctx, backup.SnapshotRequest{RepositoryID: integrationRepoID,
		Definition: definition, SnapshotID: second.SnapshotID})
	if err != nil || snapshot.RestoreVerifiedAt == nil {
		t.Fatalf("快照详情没有真实恢复验证时间: %+v, %v", snapshot, err)
	}

	beforeWrongPassword, err := os.ReadFile(restoredPayload)
	if err != nil {
		t.Fatal(err)
	}
	secrets.password = "definitely wrong"
	if _, err := ops.Restore(ctx, backup.RestoreRequest{RepositoryID: integrationRepoID, Repository: repository,
		SnapshotID: first.SnapshotID, TargetRootID: "root1", RelativePath: "restored", Overwrite: true}); err == nil {
		t.Fatal("错误仓库口令不应恢复成功")
	}
	assertFileContent(t, restoredPayload, string(beforeWrongPassword))
	secrets.password = inv.Password

	otherPolicy := policy
	otherPolicy.Name = "other-policy"
	otherSnapshot, err := ops.Run(ctx, backup.RunRequest{PolicyID: "pol_fedcba9876543210fedcba9876543210",
		Policy: otherPolicy, Repository: repository, OperationID: "op-other-policy"})
	if err != nil || !otherSnapshot.Verified {
		t.Fatalf("其他策略快照创建失败: %+v, %v", otherSnapshot, err)
	}
	pruneAssessment, err := ops.AssessPrune(ctx, backup.PruneRequest{PolicyID: integrationPolicyID, Policy: policy, Repository: repository})
	if err != nil || !pruneAssessment.Allowed || len(pruneAssessment.Delete) != 1 || pruneAssessment.Delete[0].ID != first.SnapshotID {
		t.Fatalf("保留集合预检不正确: %+v, %v", pruneAssessment, err)
	}
	expected := []string{pruneAssessment.Delete[0].ID}
	third := run("op-third")
	if _, err := ops.Prune(ctx, backup.PruneRequest{PolicyID: integrationPolicyID, Policy: policy,
		Repository: repository, ExpectedDeleteIDs: expected}); err == nil || !strings.Contains(err.Error(), "PREFLIGHT_STALE") {
		t.Fatalf("预检后新增快照必须阻止旧清理: %v", err)
	}
	for _, id := range []string{first.SnapshotID, second.SnapshotID, third.SnapshotID, otherSnapshot.SnapshotID} {
		if _, err := ops.GetSnapshot(ctx, backup.SnapshotRequest{RepositoryID: integrationRepoID, Definition: definition, SnapshotID: id}); err != nil {
			t.Fatalf("过期预检不得删除任何快照 %s: %v", id, err)
		}
	}
	pruneAssessment, err = ops.AssessPrune(ctx, backup.PruneRequest{PolicyID: integrationPolicyID, Policy: policy, Repository: repository})
	if err != nil {
		t.Fatal(err)
	}
	expected = nil
	for _, item := range pruneAssessment.Delete {
		expected = append(expected, item.ID)
	}
	pruned, err := ops.Prune(ctx, backup.PruneRequest{PolicyID: integrationPolicyID, Policy: policy,
		Repository: repository, ExpectedDeleteIDs: expected})
	if err != nil || !pruned.Verified {
		t.Fatalf("显式清理失败: %+v, %v", pruned, err)
	}
	remaining, err := ops.ListSnapshots(ctx, backup.SnapshotRequest{RepositoryID: integrationRepoID, Definition: definition})
	if err != nil {
		t.Fatal(err)
	}
	ids := make(map[string]bool)
	for _, item := range remaining {
		ids[item.ID] = true
	}
	if len(ids) != 2 || !ids[third.SnapshotID] || !ids[otherSnapshot.SnapshotID] || ids[first.SnapshotID] || ids[second.SnapshotID] {
		t.Fatalf("清理越界或保留集合错误: %+v", ids)
	}
}

func assertFileContent(t *testing.T, name, want string) {
	t.Helper()
	got, err := os.ReadFile(name)
	if err != nil || string(got) != want {
		t.Fatalf("文件 %s 内容 = %q, %v；期望 %q", name, got, err, want)
	}
}
