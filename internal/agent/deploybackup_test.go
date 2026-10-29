package agent

// 更新前备份的适配层（P15.3）。
//
// 它只做一件事：把 deploy.BackupCause 的两个方法接到备份模块上，并坚持一条边界——
// **只接受 files_only 策略**。stop_managed_apps 要求"备份前把每个受管应用都停下"，
// 而应用 → 单元/容器的绑定只有面板侧知道；deployment.update 的参数里没有它，
// 代理不能凭空发明一份。少停一个应用的快照照样建得出来，但它打着
// consistency=stop_managed_apps 的标签，实际是在应用写着的中间态下读的——那是一份
// 骗人的备份，比没有备份糟。

import (
	"context"
	"errors"
	"strings"
	"testing"

	"upanel/internal/backup"
)

// fakeBackupModule 是 deployBackupModule 的假实现。
type fakeBackupModule struct {
	policy   backup.PolicyRecord
	policyOK bool
	policyFn func() (backup.PolicyRecord, bool, error)

	repo   backup.Repository
	repoOK bool

	assess    backup.RunAssessment
	assessErr error

	result backup.RunResult
	runErr error

	runCalls  int
	runReq    backup.RunRequest
	assessReq backup.RunAssessRequest
}

func (f *fakeBackupModule) GetPolicy(_ context.Context, _ string) (backup.PolicyRecord, bool, error) {
	if f.policyFn != nil {
		return f.policyFn()
	}
	return f.policy, f.policyOK, nil
}

func (f *fakeBackupModule) GetRepository(context.Context, string) (backup.Repository, bool, error) {
	return f.repo, f.repoOK, nil
}

func (f *fakeBackupModule) AssessRun(_ context.Context, req backup.RunAssessRequest) (backup.RunAssessment, error) {
	f.assessReq = req
	return f.assess, f.assessErr
}

func (f *fakeBackupModule) Run(_ context.Context, req backup.RunRequest) (backup.RunResult, error) {
	f.runCalls++
	f.runReq = req
	return f.result, f.runErr
}

const (
	deployBackupPolicy = "pol_11111111111111111111111111111111"
	deployBackupRepo   = "repo_1"
)

// filesOnlyModule 造一条能跑的 files_only 策略。
func filesOnlyModule() *fakeBackupModule {
	return &fakeBackupModule{
		policy: backup.PolicyRecord{
			ID:   deployBackupPolicy,
			Name: "业务目录",
			Policy: backup.Policy{
				RepositoryID:    deployBackupRepo,
				Sources:         []backup.Source{{Kind: backup.SourceKindRoot, RootID: "data", RelativePath: "webapp"}},
				ConsistencyMode: backup.ConsistencyFilesOnly,
			},
		},
		policyOK: true,
		repo:     backup.Repository{ID: deployBackupRepo, Name: "offsite"},
		repoOK:   true,
		assess:   backup.RunAssessment{Allowed: true},
	}
}

// TestDeployBackupAcceptsFilesOnly：files_only 策略被接受，锁里带策略与仓库。
func TestDeployBackupAcceptsFilesOnly(t *testing.T) {
	mod := filesOnlyModule()
	src := NewDeployBackup(mod)
	assessment, err := src.AssessBackup(context.Background(), deployBackupPolicy)
	if err != nil {
		t.Fatalf("预检失败：%v", err)
	}
	if !assessment.Allowed {
		t.Fatalf("files_only 策略应被接受：%+v", assessment.Blockers)
	}
	if assessment.RepositoryID != deployBackupRepo {
		t.Fatalf("仓库 = %q", assessment.RepositoryID)
	}
	if !strings.Contains(assessment.Description, "业务目录") {
		t.Fatalf("描述 = %q", assessment.Description)
	}
	joined := strings.Join(assessment.Locks, ",")
	for _, want := range []string{backup.PolicyLockName(deployBackupPolicy), backup.LockName(deployBackupRepo)} {
		if !strings.Contains(joined, want) {
			t.Fatalf("锁里缺 %s：%s", want, joined)
		}
	}
}

// TestDeployBackupRefusesStopManagedApps：要求停应用的策略在预检就被挡住并说清出路。
func TestDeployBackupRefusesStopManagedApps(t *testing.T) {
	mod := filesOnlyModule()
	mod.policy.Policy.ConsistencyMode = backup.ConsistencyStopManagedApps
	mod.policy.Policy.AppIDs = []string{"app_1"}
	src := NewDeployBackup(mod)

	assessment, err := src.AssessBackup(context.Background(), deployBackupPolicy)
	if err != nil {
		t.Fatalf("拒绝是判决，不该是故障：%v", err)
	}
	if assessment.Allowed {
		t.Fatal("stop_managed_apps 策略必须被拒绝")
	}
	if len(assessment.Blockers) != 1 || assessment.Blockers[0].Code != "backup_policy_needs_apps" {
		t.Fatalf("阻塞项 = %+v", assessment.Blockers)
	}
	if !strings.Contains(assessment.Blockers[0].Message, "files_only") ||
		!strings.Contains(assessment.Blockers[0].Message, "backup.run") {
		t.Fatalf("要说清出路：%q", assessment.Blockers[0].Message)
	}
	if mod.assessReq.PolicyID != "" {
		t.Fatal("策略都不接受时不该再去问备份预检")
	}
}

// TestDeployBackupRefusesAtRunTimeToo：策略在预检与执行之间被改过，执行时再拒一次。
func TestDeployBackupRefusesAtRunTimeToo(t *testing.T) {
	mod := filesOnlyModule()
	src := NewDeployBackup(mod)
	if _, err := src.AssessBackup(context.Background(), deployBackupPolicy); err != nil {
		t.Fatalf("预检失败：%v", err)
	}
	mod.policy.Policy.ConsistencyMode = backup.ConsistencyStopManagedApps
	if _, err := src.RunBackup(context.Background(), deployBackupPolicy, "op-1"); err == nil {
		t.Fatal("执行前策略变成 stop_managed_apps 必须拒绝")
	}
	if mod.runCalls != 0 {
		t.Fatal("被拒绝时不该真的去跑备份")
	}
}

// TestDeployBackupReportsMissingPolicyAndRepository：策略/仓库不在了要说清是哪一个。
func TestDeployBackupReportsMissingPolicyAndRepository(t *testing.T) {
	mod := filesOnlyModule()
	mod.policyOK = false
	assessment, err := NewDeployBackup(mod).AssessBackup(context.Background(), deployBackupPolicy)
	if err != nil {
		t.Fatalf("预检失败：%v", err)
	}
	if assessment.Allowed || assessment.Blockers[0].Code != "backup_policy_not_found" {
		t.Fatalf("阻塞项 = %+v", assessment.Blockers)
	}

	mod = filesOnlyModule()
	mod.repoOK = false
	assessment, err = NewDeployBackup(mod).AssessBackup(context.Background(), deployBackupPolicy)
	if err != nil {
		t.Fatalf("预检失败：%v", err)
	}
	if assessment.Allowed || assessment.Blockers[0].Code != backup.CodeRepositoryMissing {
		t.Fatalf("阻塞项 = %+v", assessment.Blockers)
	}
}

// TestDeployBackupNilModuleIsNotAbility：没有备份模块时不许说"能备份"。
func TestDeployBackupNilModuleIsNotAbility(t *testing.T) {
	if NewDeployBackup(nil) != nil {
		t.Fatal("没有模块时构造函数该返回 nil")
	}
	var src *DeployBackup
	assessment, err := src.AssessBackup(context.Background(), deployBackupPolicy)
	if err != nil {
		t.Fatalf("预检失败：%v", err)
	}
	if assessment.Allowed || assessment.Blockers[0].Code != "backup_unavailable" {
		t.Fatalf("阻塞项 = %+v", assessment.Blockers)
	}
	if _, err := src.RunBackup(context.Background(), deployBackupPolicy, "op-1"); err == nil {
		t.Fatal("没有备份能力时执行必须报错")
	}
}

// TestDeployBackupRunCarriesEvidence：跑完把快照 id 与是否核对过一起带回去。
func TestDeployBackupRunCarriesEvidence(t *testing.T) {
	mod := filesOnlyModule()
	mod.result = backup.RunResult{SnapshotID: "snap_abc", Verified: true, RepositoryID: deployBackupRepo}
	out, err := NewDeployBackup(mod).RunBackup(context.Background(), deployBackupPolicy, "op-update-1")
	if err != nil {
		t.Fatalf("备份失败：%v", err)
	}
	if out.SnapshotID != "snap_abc" || !out.Verified {
		t.Fatalf("结果 = %+v", out)
	}
	if mod.runReq.OperationID != "op-update-1" {
		t.Fatalf("操作标识没传下去：%q", mod.runReq.OperationID)
	}
	if !strings.Contains(out.Summary, "已读回核对") {
		t.Fatalf("摘要 = %q", out.Summary)
	}

	// 没核对过：摘要必须照实说，不能把"写了快照"说成"备份成功"。
	mod = filesOnlyModule()
	mod.result = backup.RunResult{SnapshotID: "snap_abc", VerifyError: "restic stats 失败"}
	out, err = NewDeployBackup(mod).RunBackup(context.Background(), deployBackupPolicy, "op-update-1")
	if err != nil {
		t.Fatalf("备份失败：%v", err)
	}
	if out.Verified {
		t.Fatal("没有读回核对就不算 verified")
	}
	if !strings.Contains(out.Summary, "读回核对失败") {
		t.Fatalf("摘要 = %q", out.Summary)
	}

	mod = filesOnlyModule()
	mod.runErr = errors.New("restic 退出码 1")
	if _, err := NewDeployBackup(mod).RunBackup(context.Background(), deployBackupPolicy, "op-1"); err == nil {
		t.Fatal("备份失败必须把错误上抛")
	}
}
