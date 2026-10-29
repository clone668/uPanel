package deploy

// 更新（P15.3）：deployment.update。
//
// 这一层要回答的问题是「换镜像的时候，哪些东西会跟着变、哪些绝不跟着变」：
//
//	会变    配置字节（新镜像引用）、部署记录（新 digest 与上一版）、单元的一次 restart
//	不变    数据卷与业务目录（一个字节都不动）、别的部署（锁之外互不干涉）
//
// 以及失败时「退回到什么程度」：回切的是配置与镜像，**数据不跟着回切**。这里用假件
// 把四条失败路径各走一遍——备份没建成、拉镜像失败、digest 不符、健康探测不过——
// 每一条都要落到"要么什么都没改，要么退回到上一版"。

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeBackupCause 是"更新前备份"的假实现。
type fakeBackupCause struct {
	assess     BackupAssessment
	assessErr  error
	out        BackupOutcome
	err        error
	policyIDs  []string
	operations []string
	onRun      func()
}

func (f *fakeBackupCause) AssessBackup(_ context.Context, policyID string) (BackupAssessment, error) {
	f.policyIDs = append(f.policyIDs, policyID)
	return f.assess, f.assessErr
}

func (f *fakeBackupCause) RunBackup(_ context.Context, policyID, operationID string) (BackupOutcome, error) {
	if f.onRun != nil {
		f.onRun()
	}
	f.policyIDs = append(f.policyIDs, policyID)
	f.operations = append(f.operations, operationID)
	return f.out, f.err
}

const (
	testOldImageID = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	testNewImageID = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
)

// inspectKey 是 `docker image inspect` 在假件里的 argv 键。
func inspectKey(template, reference string) string {
	return "image inspect --format " + template + " " + reference
}

// newUpdateFixture 搭一个已经部署好的现场：单元在盘上、配置在盘上、记录也在。
//
// 返回的 fake 已经配好"两个镜像都能读到 id"，oldConfig 是这次要替换掉的字节。
func newUpdateFixture(t *testing.T) (*Service, *Applier, *fakeSystemctl, []byte) {
	t.Helper()
	fake := &fakeSystemctl{
		enabled: "enabled",
		active:  "active",
		stdout: map[string]string{
			inspectKey("{{.Id}}", "nginx:1.27"): testOldImageID,
			inspectKey("{{.Id}}", "nginx:1.28"): testNewImageID,
		},
	}
	svc, applier, _ := newTestService(t, fake, nil)
	svc.prober = &fakeHealthProber{}

	out, err := svc.ExecuteApply(context.Background(), ApplyParams{Definition: goldenDefinition()}, "", nil)
	if err != nil {
		t.Fatalf("先把部署建起来失败：%v", err)
	}
	if !out.Recorded {
		t.Fatalf("部署之后必须留下记录，否则更新无从下手：%+v", out.Warnings)
	}
	rec, found, err := applier.ReadRecord("webapp")
	if err != nil || !found {
		t.Fatalf("读记录失败：found=%v err=%v", found, err)
	}
	if rec.Image.Digest != testOldImageID {
		t.Fatalf("记录里的镜像 digest = %q", rec.Image.Digest)
	}
	// 部署那一步自己也探了一次（P15.3 把健康探测接进了 apply），换一个干净的假件
	// 再来数这次更新的探测次数。
	svc.prober = &fakeHealthProber{}
	oldConfig, _, found, err := applier.ReadConfig("webapp")
	if err != nil || !found {
		t.Fatalf("读配置失败：found=%v err=%v", found, err)
	}
	fake.calls = nil
	return svc, applier, fake, oldConfig
}

// updateParams 造一份"只换镜像"的更新参数。
func updateParams() UpdateParams {
	return UpdateParams{
		DeploymentID:   DeploymentIDForName("webapp"),
		ImageReference: "nginx:1.28",
	}
}

// TestAssessUpdateNeedsRecord：没有记录的部署不能更新（P15.2 建的部署就是这样）。
func TestAssessUpdateNeedsRecord(t *testing.T) {
	svc, applier, _, _ := newUpdateFixture(t)
	if err := applier.RemoveRecord("webapp"); err != nil {
		t.Fatalf("删记录失败：%v", err)
	}
	assessment, err := svc.AssessUpdate(context.Background(), updateParams())
	if err != nil {
		t.Fatalf("没有记录是判决，不该是故障：%v", err)
	}
	if assessment.Allowed {
		t.Fatal("没有记录时必须拒绝更新")
	}
	if len(assessment.Blockers) != 1 || assessment.Blockers[0].Code != "no_definition_record" {
		t.Fatalf("阻塞项 = %+v", assessment.Blockers)
	}
	if !strings.Contains(assessment.Blockers[0].Message, "deployment.remove") {
		t.Fatalf("拒绝要说清出路：%q", assessment.Blockers[0].Message)
	}
}

// TestAssessUpdateBlocksStaleVersionAndTamperedConfig：版本对不上、记录与盘上不一致都拒绝。
func TestAssessUpdateBlocksStaleVersionAndTamperedConfig(t *testing.T) {
	svc, applier, _, oldConfig := newUpdateFixture(t)

	params := updateParams()
	params.Version = "sha256:" + strings.Repeat("f", 64)
	assessment, err := svc.AssessUpdate(context.Background(), params)
	if err != nil {
		t.Fatalf("预检失败：%v", err)
	}
	if assessment.Allowed || assessment.Blockers[0].Code != "stale" {
		t.Fatalf("版本对不上必须 stale：%+v", assessment.Blockers)
	}

	// 有人手工改了盘上的配置：记录里那一版与盘上那份不再是同一份字节。
	path, err := applier.configFile("webapp")
	if err != nil {
		t.Fatalf("取配置路径失败：%v", err)
	}
	tampered := append([]byte("# 手工改过\n"), oldConfig...)
	if err := os.WriteFile(path, tampered, configFileMode); err != nil {
		t.Fatalf("改配置文件失败：%v", err)
	}
	assessment, err = svc.AssessUpdate(context.Background(), updateParams())
	if err != nil {
		t.Fatalf("预检失败：%v", err)
	}
	if assessment.Allowed || assessment.Blockers[0].Code != "record_mismatch" {
		t.Fatalf("记录与盘上对不上必须记录级拒绝：%+v", assessment.Blockers)
	}
}

// TestAssessUpdateReportsWhatChanges：预检要说清换的是哪一版、会不会动配置、探不探。
func TestAssessUpdateReportsWhatChanges(t *testing.T) {
	svc, _, _, _ := newUpdateFixture(t)
	assessment, err := svc.AssessUpdate(context.Background(), updateParams())
	if err != nil {
		t.Fatalf("预检失败：%v", err)
	}
	if !assessment.Allowed {
		t.Fatalf("应该允许：%+v", assessment.Blockers)
	}
	if assessment.CurrentImage != "nginx:1.27" || assessment.TargetImage != "nginx:1.28" {
		t.Fatalf("镜像 = %q → %q", assessment.CurrentImage, assessment.TargetImage)
	}
	if assessment.CurrentImageDigest != testOldImageID {
		t.Fatalf("当前 digest = %q", assessment.CurrentImageDigest)
	}
	if !assessment.ConfigChanged || assessment.ConfigDigest == assessment.CurrentConfigDigest {
		t.Fatalf("换了镜像必然换配置：changed=%v", assessment.ConfigChanged)
	}
	if assessment.StateVersion != assessment.CurrentConfigDigest {
		t.Fatalf("复核令牌必须是盘上的摘要：%q vs %q",
			assessment.StateVersion, assessment.CurrentConfigDigest)
	}
	if !assessment.HealthCheckEnabled {
		t.Fatal("goldenDefinition 开了健康检查，预检必须说会探")
	}
	// 锁里要带上端口与卷：更新期间别的部署不来抢同一个入口。
	joined := strings.Join(assessment.Locks(), ",")
	for _, want := range []string{LockName("webapp"), "port:tcp/127.0.0.1/18080", VolumeLockName(testVolumeID)} {
		if !strings.Contains(joined, want) {
			t.Fatalf("锁里缺 %s：%s", want, joined)
		}
	}
}

// TestExecuteUpdateSuccess：正常更新——留旧字节、写新配置、restart、探通、换记录。
func TestExecuteUpdateSuccess(t *testing.T) {
	svc, applier, fake, oldConfig := newUpdateFixture(t)

	var steps []string
	out, err := svc.ExecuteUpdate(context.Background(), updateParams(), "", "op-update-1",
		func(step string, _ map[string]any) { steps = append(steps, step) })
	if err != nil {
		t.Fatalf("更新失败：%v", err)
	}

	// 单元是 restart 而不是 start：单元文件一个字节都没变，start 对 active 的话是空操作。
	if !fake.called("restart upanel-deploy-webapp.service") {
		t.Fatalf("更新必须 restart 包装单元：%v", fake.calls)
	}
	if !out.Report.Restarted || !out.Report.Enabled || !out.Report.Active {
		t.Fatalf("读回事实 = %+v", out.Report)
	}
	if out.Report.ConfigDigest == out.Report.PreviousConfigDigest {
		t.Fatal("配置摘要没变，那就不叫换镜像")
	}
	if out.Idempotent {
		t.Fatal("换了镜像不是幂等重放")
	}

	// 新配置里是新镜像，旧字节原样留在 .prev 里。
	current, digest, found, err := applier.ReadConfig("webapp")
	if err != nil || !found {
		t.Fatalf("读新配置失败：found=%v err=%v", found, err)
	}
	if !strings.Contains(string(current), "nginx:1.28") {
		t.Fatalf("新配置里没有新镜像：%s", current)
	}
	if digest != out.Report.ConfigDigest {
		t.Fatalf("配置摘要 = %q，报告里是 %q", digest, out.Report.ConfigDigest)
	}
	previous, found, err := applier.ReadPreviousConfig("webapp")
	if err != nil || !found {
		t.Fatalf("上一版没留下来：found=%v err=%v", found, err)
	}
	if string(previous) != string(oldConfig) {
		t.Fatal("上一版必须是原样字节，不是重新渲染的结果")
	}

	// 记录：新 digest、上一版指向旧镜像。
	rec, found, err := applier.ReadRecord("webapp")
	if err != nil || !found {
		t.Fatalf("读记录失败：found=%v err=%v", found, err)
	}
	if rec.Image.Requested != "nginx:1.28" || rec.Image.Digest != testNewImageID {
		t.Fatalf("记录里的镜像 = %+v", rec.Image)
	}
	if rec.ConfigDigest != out.Report.ConfigDigest {
		t.Fatalf("记录里的摘要 = %q", rec.ConfigDigest)
	}
	if rec.Previous == nil || rec.Previous.Requested != "nginx:1.27" ||
		rec.Previous.Digest != testOldImageID {
		t.Fatalf("上一版记录 = %+v", rec.Previous)
	}
	if !out.Report.ImageDigestKnown {
		t.Fatalf("digest 读到了就该标 known：%+v", out.Warnings)
	}

	// 探测打在宿主端口上（公开入口），不是容器端口。
	if !out.Report.Health.Checked || !out.Report.Health.Healthy {
		t.Fatalf("探测结论 = %+v", out.Report.Health)
	}
	prober, _ := svc.prober.(*fakeHealthProber)
	if len(prober.targets) != 1 || prober.targets[0].Port != 18080 {
		t.Fatalf("探测目标 = %+v，期望宿主端口 18080", prober.targets)
	}

	// 进度里要看得见它在做什么。
	for _, want := range []string{"pulling", "writing", "probing"} {
		if !containsStep(steps, want) {
			t.Fatalf("进度里缺 %s：%v", want, steps)
		}
	}
}

// TestExecuteUpdateWithoutBackupDoesNotStop：不选备份就不先停单元（restart 自己会 down 再 up）。
func TestExecuteUpdateWithoutBackupDoesNotStop(t *testing.T) {
	svc, _, fake, _ := newUpdateFixture(t)
	if _, err := svc.ExecuteUpdate(context.Background(), updateParams(), "", "op-update-1", nil); err != nil {
		t.Fatalf("更新失败：%v", err)
	}
	if fake.called("stop upanel-deploy-webapp.service") {
		t.Fatal("没选备份时不该先停单元")
	}
}

// TestExecuteUpdateBackupStopsFirstAndRuns：选了备份就先停、再备、备不成不换镜像。
func TestExecuteUpdateBackupStopsFirstAndRuns(t *testing.T) {
	svc, _, fake, _ := newUpdateFixture(t)
	policy := "pol_" + strings.Repeat("1", 32)
	callsAtBackup := -1
	backup := &fakeBackupCause{
		assess: BackupAssessment{PolicyID: policy, RepositoryID: "repo_1", Description: "业务目录",
			Allowed: true, Locks: []string{"backup-repo:repo_1"}},
		out: BackupOutcome{PolicyID: policy, RepositoryID: "repo_1", SnapshotID: "snap_1", Verified: true},
		onRun: func() {
			callsAtBackup = len(fake.calls)
		},
	}
	svc.backup = backup
	params := updateParams()
	params.BackupPolicyID = &policy

	assessment, err := svc.AssessUpdate(context.Background(), params)
	if err != nil {
		t.Fatalf("预检失败：%v", err)
	}
	if !assessment.Allowed || !assessment.BackupRequired {
		t.Fatalf("预检 = %+v", assessment)
	}
	if !strings.Contains(strings.Join(assessment.Locks(), ","), "backup-repo:repo_1") {
		t.Fatalf("备份仓库锁没并进更新锁：%v", assessment.Locks())
	}

	out, err := svc.ExecuteUpdate(context.Background(), params, "", "op-update-1", nil)
	if err != nil {
		t.Fatalf("更新失败：%v", err)
	}
	if callsAtBackup <= 0 {
		t.Fatalf("备份没被调用：%v", fake.calls)
	}
	if fake.calls[0] != "stop upanel-deploy-webapp.service" {
		t.Fatalf("备份之前必须先停单元（容器还在写的时候拷出来的目录不是备份），实际顺序：%v",
			fake.calls[:callsAtBackup])
	}
	if out.Report.Backup.SnapshotID != "snap_1" || !out.Report.Backup.Verified {
		t.Fatalf("备份结果没进报告：%+v", out.Report.Backup)
	}
	if len(backup.operations) != 1 || backup.operations[0] != "op-update-1" {
		t.Fatalf("备份要用操作标识命名临时文件：%v", backup.operations)
	}
}

// TestExecuteUpdateBackupFailureChangesNothing：备份没建成 → 什么都没改，单元起回来。
func TestExecuteUpdateBackupFailureChangesNothing(t *testing.T) {
	svc, applier, fake, oldConfig := newUpdateFixture(t)
	policy := "pol_" + strings.Repeat("1", 32)
	svc.backup = &fakeBackupCause{
		assess: BackupAssessment{PolicyID: policy, Allowed: true},
		out:    BackupOutcome{PolicyID: policy},
	}
	params := updateParams()
	params.BackupPolicyID = &policy

	_, err := svc.ExecuteUpdate(context.Background(), params, "", "op-update-1", nil)
	var svcErr *ServiceError
	if !errors.As(err, &svcErr) || svcErr.Code != "backup_failed" {
		t.Fatalf("期望 backup_failed，实际 %v", err)
	}
	current, _, _, err := applier.ReadConfig("webapp")
	if err != nil {
		t.Fatalf("读配置失败：%v", err)
	}
	if string(current) != string(oldConfig) {
		t.Fatal("备份失败时配置一个字节都不该改")
	}
	if fake.called("image pull nginx:1.28") {
		t.Fatal("备份失败时不该拉镜像")
	}
	// 单元按原样起回来（它被停过）。
	if !fake.called("restart upanel-deploy-webapp.service") {
		t.Fatalf("停过的单元必须起回来：%v", fake.calls)
	}
}

// TestExecuteUpdatePullFailureLeavesConfig：拉镜像失败 → 配置没动、记录没动。
func TestExecuteUpdatePullFailureLeavesConfig(t *testing.T) {
	svc, applier, fake, oldConfig := newUpdateFixture(t)
	fake.exitCodes = map[string]int{"image pull nginx:1.28": 1}

	_, err := svc.ExecuteUpdate(context.Background(), updateParams(), "", "op-update-1", nil)
	if err == nil {
		t.Fatal("拉镜像失败必须报错")
	}
	if !strings.Contains(err.Error(), "拉取镜像") {
		t.Fatalf("错误要说清是拉取失败：%v", err)
	}
	current, _, _, err := applier.ReadConfig("webapp")
	if err != nil {
		t.Fatalf("读配置失败：%v", err)
	}
	if string(current) != string(oldConfig) {
		t.Fatal("拉镜像失败时配置不该被改")
	}
	rec, _, err := applier.ReadRecord("webapp")
	if err != nil {
		t.Fatalf("读记录失败：%v", err)
	}
	if rec.Image.Requested != "nginx:1.27" {
		t.Fatalf("记录不该被改：%+v", rec.Image)
	}
}

// TestExecuteUpdateRejectsDigestMismatch：定义钉了 digest 时，拉回来的不是它就不换。
func TestExecuteUpdateRejectsDigestMismatch(t *testing.T) {
	svc, applier, fake, oldConfig := newUpdateFixture(t)
	pinned := "nginx@sha256:" + strings.Repeat("9", 64)
	fake.stdout[inspectKey("{{.Id}}", pinned)] = testNewImageID

	params := updateParams()
	params.ImageReference = pinned
	_, err := svc.ExecuteUpdate(context.Background(), params, "", "op-update-1", nil)
	var svcErr *ServiceError
	if !errors.As(err, &svcErr) || svcErr.Code != "image_digest_mismatch" {
		t.Fatalf("期望 image_digest_mismatch，实际 %v", err)
	}
	if !strings.Contains(svcErr.Message, strings.Repeat("9", 64)) {
		t.Fatalf("拒绝要说清钉住的是哪一个：%q", svcErr.Message)
	}
	current, _, _, err := applier.ReadConfig("webapp")
	if err != nil {
		t.Fatalf("读配置失败：%v", err)
	}
	if string(current) != string(oldConfig) {
		t.Fatal("digest 不符时配置不该被改")
	}
}

// TestExecuteUpdateHealthFailureRollsBack：探不通就回切配置与镜像，数据不跟着回切。
func TestExecuteUpdateHealthFailureRollsBack(t *testing.T) {
	svc, applier, _, oldConfig := newUpdateFixture(t)
	svc.prober = &fakeHealthProber{results: []HealthProbe{
		{Checked: true, Healthy: false, Attempts: 4, LastError: "连不上 127.0.0.1:18080"},
		{Checked: true, Healthy: true, Attempts: 1},
	}}

	out, err := svc.ExecuteUpdate(context.Background(), updateParams(), "", "op-update-1", nil)
	var svcErr *ServiceError
	if !errors.As(err, &svcErr) || svcErr.Code != CodeHealthFailedRolledBack {
		t.Fatalf("期望 %s，实际 %v", CodeHealthFailedRolledBack, err)
	}
	if !strings.Contains(svcErr.Message, "数据没有回切") {
		t.Fatalf("回切提示必须写清数据不在其中：%q", svcErr.Message)
	}
	if !out.Report.RolledBack || !out.Report.RollbackVerified {
		t.Fatalf("报告 = %+v", out.Report)
	}
	if !out.Report.RollbackHealth.Healthy {
		t.Fatalf("回切之后的探测 = %+v", out.Report.RollbackHealth)
	}
	// 盘上是旧字节，记录还是旧版本（回切之后没有写成新记录）。
	current, digest, _, err := applier.ReadConfig("webapp")
	if err != nil {
		t.Fatalf("读配置失败：%v", err)
	}
	if string(current) != string(oldConfig) {
		t.Fatalf("回切之后盘上必须是上一版字节：%s", current)
	}
	if digest != out.Report.PreviousConfigDigest {
		t.Fatalf("盘上摘要 = %q，预检时的摘要 = %q", digest, out.Report.PreviousConfigDigest)
	}
	rec, _, err := applier.ReadRecord("webapp")
	if err != nil {
		t.Fatalf("读记录失败：%v", err)
	}
	if rec.Image.Requested != "nginx:1.27" {
		t.Fatalf("回切之后记录不该指向新镜像：%+v", rec.Image)
	}
}

// TestExecuteUpdateStaleBetweenAssessAndExecute：盘上被改过就不动手。
func TestExecuteUpdateStaleBetweenAssessAndExecute(t *testing.T) {
	svc, applier, _, _ := newUpdateFixture(t)
	params := updateParams()
	params.Version = "sha256:" + strings.Repeat("f", 64)
	_, err := svc.ExecuteUpdate(context.Background(), params, "", "op-update-1", nil)
	// 执行前复核重跑预检，版本对不上时预检本身就不允许 → 通用的 blocked 拒绝，
	// 理由来自那条 stale 阻塞项（消息里带着盘上与请求里的两个摘要）。
	var svcErr *ServiceError
	if !errors.As(err, &svcErr) || svcErr.Code != "blocked" {
		t.Fatalf("期望 blocked，实际 %v", err)
	}
	if !strings.Contains(svcErr.Message, "期间有人改过这份部署") {
		t.Fatalf("拒绝要说清是版本对不上：%q", svcErr.Message)
	}
	if _, found, err := applier.ReadPreviousConfig("webapp"); err != nil || found {
		t.Fatalf("被拒绝时不该写下上一版：found=%v err=%v", found, err)
	}
}

// TestExecuteUpdateRefusesStopIneffectiveUnit：单元停不下来就不备份、不换镜像。
func TestExecuteUpdateRefusesStopIneffectiveUnit(t *testing.T) {
	svc, _, fake, _ := newUpdateFixture(t)
	policy := "pol_" + strings.Repeat("1", 32)
	svc.backup = &fakeBackupCause{assess: BackupAssessment{PolicyID: policy, Allowed: true}}
	params := updateParams()
	params.BackupPolicyID = &policy
	// stop 之后单元仍然 active（容器不退）：StopUnit 必须拒绝。
	fake.stopStuck = true

	_, err := svc.ExecuteUpdate(context.Background(), params, "", "op-update-1", nil)
	if err == nil || !strings.Contains(err.Error(), "仍是 active") {
		t.Fatalf("停不下来必须拒绝，实际 %v", err)
	}
}

// containsStep 判断进度里有没有某一步。
func containsStep(steps []string, want string) bool {
	for _, step := range steps {
		if step == want {
			return true
		}
	}
	return false
}

// TestUpdatePreviousHintPointsAtFile：错误消息里的回切文件路径必须真在配置目录下。
func TestUpdatePreviousHintPointsAtFile(t *testing.T) {
	path, err := PreviousConfigPath("webapp")
	if err != nil {
		t.Fatalf("取上一版路径失败：%v", err)
	}
	if filepath.Base(path) != "webapp.compose.yaml.prev" {
		t.Fatalf("上一版文件名 = %q", path)
	}
	if hint := previousHint("webapp"); !strings.Contains(hint, "webapp.compose.yaml.prev") {
		t.Fatalf("提示 = %q", hint)
	}
	if _, err := PreviousConfigPath("../etc"); err == nil {
		t.Fatal("名字里带路径分隔符时必须拒绝")
	}
}
