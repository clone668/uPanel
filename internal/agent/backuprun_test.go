package agent

// backup.run / backup.snapshots / backup.snapshot 的翻译层（P12.2）。
//
// internal/backup 的测试已经验过"谁被停机、收尾在哪一步、快照怎么读回核对"；
// 这里只验代理有没有添油加醋，五件事：
//
//  1. 参数只认 policy_id / version / 解析好的应用对象，别的字段一律拒绝；
//  2. 参数里的应用不能超出策略的 app_ids（参数不扩大授权范围）；
//  3. 策略与仓库都取自代理自己的镜像，版本不符就拒绝；
//  4. 快照建成了但没读回核对 = unknown，不是 verified，也不是 failed；
//  5. 快照列表不带大小（那要每条跑一次 restic stats），详情才带。

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"upanel/internal/backup"
	"upanel/internal/operations"
)

const (
	runTestPolicyID = "pol_0123456789abcdef0123456789abcdef"
	runTestAppA     = "app_0123456789abcdef0123456789abcdef"
	runTestAppB     = "app_fedcba9876543210fedcba9876543210"
	runTestSnap     = "1111111111111111111111111111111111111111111111111111111111111111"
	runTestUnit     = "upanel-app-a.service"
	runTestCntrID   = "abcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcd"
)

// fakeBackupRun 是 agent 层的假运行端。
type fakeBackupRun struct {
	policy      backup.PolicyRecord
	policyFound bool
	policyErr   error
	policies    []string

	repo    backup.Repository
	found   bool
	repoErr error

	assessment backup.RunAssessment
	assessed   []backup.RunAssessRequest
	assessErr  error

	run    backup.RunResult
	runErr error
	runs   []backup.RunRequest

	snapshots  []backup.Snapshot
	snapshot   backup.Snapshot
	listErr    error
	detailErr  error
	listReqs   []backup.SnapshotRequest
	detailReqs []backup.SnapshotRequest
}

func (f *fakeBackupRun) GetPolicy(_ context.Context, id string) (backup.PolicyRecord, bool, error) {
	f.policies = append(f.policies, id)
	if f.policyErr != nil {
		return backup.PolicyRecord{}, false, f.policyErr
	}
	if !f.policyFound || f.policy.ID != id {
		return backup.PolicyRecord{}, false, nil
	}
	return f.policy, true, nil
}

func (f *fakeBackupRun) GetRepository(_ context.Context, id string) (backup.Repository, bool, error) {
	if f.repoErr != nil {
		return backup.Repository{}, false, f.repoErr
	}
	if !f.found || f.repo.ID != id {
		return backup.Repository{}, false, nil
	}
	return f.repo, true, nil
}

func (f *fakeBackupRun) AssessRun(_ context.Context, req backup.RunAssessRequest) (backup.RunAssessment, error) {
	f.assessed = append(f.assessed, req)
	return f.assessment, f.assessErr
}

func (f *fakeBackupRun) Run(_ context.Context, req backup.RunRequest) (backup.RunResult, error) {
	f.runs = append(f.runs, req)
	if req.Progress != nil {
		req.Progress("backup", map[string]any{"paths": 1})
	}
	return f.run, f.runErr
}

func (f *fakeBackupRun) ListSnapshots(_ context.Context, req backup.SnapshotRequest) ([]backup.Snapshot, error) {
	f.listReqs = append(f.listReqs, req)
	return f.snapshots, f.listErr
}

func (f *fakeBackupRun) GetSnapshot(_ context.Context, req backup.SnapshotRequest) (backup.Snapshot, error) {
	f.detailReqs = append(f.detailReqs, req)
	return f.snapshot, f.detailErr
}

// runTestPolicy 是一条 stop_managed_apps 的策略（最复杂的那一种）。
func runTestPolicy(t *testing.T) backup.Policy {
	t.Helper()
	pol, err := backup.Policy{
		Name:            "站点整机",
		RepositoryID:    testRepoID,
		Sources:         []backup.Source{{RootID: "root1", RelativePath: "srv/app"}},
		ConsistencyMode: backup.ConsistencyStopManagedApps,
		AppIDs:          []string{runTestAppA, runTestAppB},
		Retention:       backup.Retention{KeepLast: 7},
	}.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	return pol
}

func newFakeBackupRun(t *testing.T) *fakeBackupRun {
	t.Helper()
	def := fakeDefinition(t)
	pol := runTestPolicy(t)
	return &fakeBackupRun{
		policy:      backup.PolicyRecord{ID: runTestPolicyID, Name: pol.Name, Policy: pol, Version: 4},
		policyFound: true,
		repo:        backup.Repository{ID: testRepoID, Name: def.Name, Definition: def, Version: 3},
		found:       true,
		assessment: backup.RunAssessment{
			PolicyID:     runTestPolicyID,
			RepositoryID: testRepoID,
			Allowed:      true,
			Locks:        []string{backup.LockName(testRepoID), backup.PolicyLockName(runTestPolicyID)},
			Impacts:      []backup.Impact{{Kind: backup.ImpactAvailability, Description: "备份期间会停掉 1 个应用"}},
			Target: backup.Target{
				DisplayName:         "站点整机 → 夜间根文件",
				IdentityFingerprint: "sha256:policy|sha256:repo",
				StateVersion:        "backup-policy:" + runTestPolicyID + ":v4|repository:" + testRepoID + ":v3",
			},
		},
		run: backup.RunResult{
			PolicyID:        runTestPolicyID,
			RepositoryID:    testRepoID,
			SnapshotID:      runTestSnap,
			ConsistencyMode: backup.ConsistencyStopManagedApps,
			StartedAt:       time.Date(2026, 10, 24, 3, 0, 0, 0, time.UTC),
			FinishedAt:      time.Date(2026, 10, 24, 3, 2, 0, 0, time.UTC),
			Duration:        2 * time.Minute,
			Verified:        true,
			AppsRestored:    true,
			Summary:         backup.BackupSummary{SnapshotID: runTestSnap, FilesNew: 12, DataAdded: 4096},
		},
		snapshots: []backup.Snapshot{{
			ID: runTestSnap, ShortID: "11111111",
			Time:  time.Date(2026, 10, 24, 3, 2, 0, 0, time.UTC),
			Paths: []string{"/srv/data/srv/app"},
			Tags:  []string{"upanel"},
		}},
		snapshot: backup.Snapshot{
			ID: runTestSnap, ShortID: "11111111",
			Time:      time.Date(2026, 10, 24, 3, 2, 0, 0, time.UTC),
			Paths:     []string{"/srv/data/srv/app"},
			SizeBytes: 4096, FileCount: 12,
		},
	}
}

func newBackupRunRegistry(t *testing.T, src backupRunSource) *Registry {
	t.Helper()
	reg := NewRegistry()
	if err := RegisterBackupRunAction(reg, src); err != nil {
		t.Fatalf("登记 %s 失败: %v", backup.ActionRun, err)
	}
	if err := RegisterBackupSnapshotQueries(reg, src); err != nil {
		t.Fatalf("登记快照查询失败: %v", err)
	}
	return reg
}

func TestRegisterBackupRunRequiresSource(t *testing.T) {
	if err := RegisterBackupRunAction(NewRegistry(), nil); err == nil {
		t.Fatal("没有实现端就不该登记成功")
	}
}

func TestRegisterBackupRunNames(t *testing.T) {
	reg := newBackupRunRegistry(t, newFakeBackupRun(t))
	if _, ok := reg.Action(backup.ActionRun); !ok {
		t.Fatalf("%s 没有登记", backup.ActionRun)
	}
	if !contains(reg.ExecutableActionNames(), backup.ActionRun) {
		t.Fatalf("%s 应当可执行：%v", backup.ActionRun, reg.ExecutableActionNames())
	}
	for _, name := range []string{backup.QuerySnapshots, backup.QuerySnapshot} {
		if _, ok := reg.Query(name); !ok {
			t.Fatalf("%s 没有登记", name)
		}
	}
}

// TestBackupRunPolicy：备份要再认证（读口令、可能停应用），不要短语，可取消。
func TestBackupRunPolicy(t *testing.T) {
	reg := newBackupRunRegistry(t, newFakeBackupRun(t))
	action, _ := reg.Action(backup.ActionRun)
	policy, err := action.ResolvePolicy(nil)
	if err != nil {
		t.Fatalf("策略不合法: %v", err)
	}
	if !policy.RequiresReauth {
		t.Fatal("备份要读仓库口令，必须再认证")
	}
	if policy.RequiresPhrase {
		t.Fatalf("备份不是破坏性动作，不该要短语（%q）", policy.PhraseVerb)
	}
	if !policy.Cancellable {
		t.Fatal("restic 支持在两次写入之间被打断，应当允许取消")
	}
}

func TestBackupRunDecodeStrict(t *testing.T) {
	reg := newBackupRunRegistry(t, newFakeBackupRun(t))
	action, _ := reg.Action(backup.ActionRun)

	raw := `{"backup_policy_id":"` + runTestPolicyID + `","version":4,"apps":[
		{"app_id":"` + runTestAppA + `","kind":"systemd","object":"` + runTestUnit + `"},
		{"app_id":"` + runTestAppB + `","kind":"container","object":"` + runTestCntrID + `"}]}`
	params, err := action.Decode(json.RawMessage(raw))
	if err != nil {
		t.Fatalf("正常参数被拒: %v", err)
	}
	got := params.(backupRunParams)
	if got.PolicyID != runTestPolicyID || got.Version != 4 || len(got.Apps) != 2 {
		t.Fatalf("参数解码错：%+v", got)
	}
	if got.Apps[1].Kind != backup.AppKindContainer {
		t.Fatalf("容器种类应保留：%+v", got.Apps[1])
	}

	bad := []string{
		`{"backup_policy_id":"` + runTestPolicyID + `","version":4,"consistency_mode":"files_only"}`, // 未知字段
		`{"version":4}`, // 缺 policy_id
		`{"backup_policy_id":"` + runTestPolicyID + `"}`,             // 缺 version
		`{"backup_policy_id":"` + runTestPolicyID + `","version":0}`, // version 非正
		`{"backup_policy_id":"` + runTestPolicyID + `","version":4,"apps":[{"app_id":"x","kind":"systemd","object":"` + runTestUnit + `"}]}`,
		`{"backup_policy_id":"` + runTestPolicyID + `","version":4,"apps":[{"app_id":"` + runTestAppA + `","kind":"pod","object":"` + runTestUnit + `"}]}`,
		`{"backup_policy_id":"` + runTestPolicyID + `","version":4,"apps":[{"app_id":"` + runTestAppA + `","kind":"systemd","object":"../etc/passwd"}]}`,
		`{"backup_policy_id":"` + runTestPolicyID + `","version":4,"apps":[{"app_id":"` + runTestAppA + `","kind":"container","object":"short"}]}`,
		// 同一个运行对象两次
		`{"backup_policy_id":"` + runTestPolicyID + `","version":4,"apps":[
			{"app_id":"` + runTestAppA + `","kind":"systemd","object":"` + runTestUnit + `"},
			{"app_id":"` + runTestAppB + `","kind":"systemd","object":"` + runTestUnit + `"}]}`,
	}
	for _, raw := range bad {
		if _, err := action.Decode(json.RawMessage(raw)); !errors.Is(err, ErrParamsInvalid) {
			t.Fatalf("%s 应当被拒: %v", raw, err)
		}
	}
}

// runParams 是一条**完整**的请求：策略管两个应用，这里把两个都解析出来了。
//
// 少一个不是"小一点"，是错——见 TestBackupRunPlanBlocksUnresolvedPolicyApp。
func runParams() backupRunParams {
	return backupRunParams{
		PolicyID: runTestPolicyID,
		Version:  4,
		Apps: []backupAppParam{
			{AppID: runTestAppA, Kind: backup.AppKindSystemd, Object: runTestUnit},
			{AppID: runTestAppB, Kind: backup.AppKindContainer, Object: runTestCntrID},
		},
	}
}

func TestBackupRunPlanBlocksUnknownPolicy(t *testing.T) {
	src := newFakeBackupRun(t)
	src.policyFound = false
	reg := newBackupRunRegistry(t, src)
	action, _ := reg.Action(backup.ActionRun)

	plan, err := action.Plan(context.Background(), PlanInput{Params: runParams()})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Allowed || len(plan.Blockers) != 1 || plan.Blockers[0].Code != "BACKUP_POLICY_NOT_FOUND" {
		t.Fatalf("代理里没有策略时必须拒绝：%+v", plan.Blockers)
	}
	if len(src.assessed) != 0 {
		t.Fatal("没有策略就不该去评估现场")
	}
	if len(plan.Locks) != 1 || plan.Locks[0] != backup.PolicyLockName(runTestPolicyID) {
		t.Fatalf("拒绝时也要带锁名：%v", plan.Locks)
	}
}

func TestBackupRunPlanBlocksStalePolicyVersion(t *testing.T) {
	reg := newBackupRunRegistry(t, newFakeBackupRun(t))
	action, _ := reg.Action(backup.ActionRun)

	params := runParams()
	params.Version = 3 // 镜像里是 4
	plan, err := action.Plan(context.Background(), PlanInput{Params: params})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Allowed || plan.Blockers[0].Code != "BACKUP_POLICY_VERSION_STALE" {
		t.Fatalf("版本不符必须拒绝：%+v", plan.Blockers)
	}
}

func TestBackupRunPlanBlocksMissingRepository(t *testing.T) {
	src := newFakeBackupRun(t)
	src.found = false
	reg := newBackupRunRegistry(t, src)
	action, _ := reg.Action(backup.ActionRun)

	plan, err := action.Plan(context.Background(), PlanInput{Params: runParams()})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Allowed || plan.Blockers[0].Code != backup.CodeRepositoryMissing {
		t.Fatalf("仓库被删掉时必须拒绝：%+v", plan.Blockers)
	}
}

func TestBackupRunPlanBlocksAppOutsidePolicy(t *testing.T) {
	src := newFakeBackupRun(t)
	reg := newBackupRunRegistry(t, src)
	action, _ := reg.Action(backup.ActionRun)

	params := runParams()
	params.Apps = []backupAppParam{{AppID: "app_22222222222222222222222222222222", Kind: backup.AppKindSystemd, Object: runTestUnit}}
	plan, err := action.Plan(context.Background(), PlanInput{Params: params})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Allowed {
		t.Fatal("参数不能停策略之外的应用")
	}
	if plan.Blockers[0].Code != backup.CodePolicyAppsUnavailable {
		t.Fatalf("阻塞码不对：%+v", plan.Blockers)
	}
	if len(src.assessed) != 0 {
		t.Fatal("授权范围不符时不该开始评估")
	}
}

// TestBackupRunPlanBlocksUnresolvedPolicyApp：策略管两个应用，请求里只解析出一个。
//
// 这是**必须阻塞**的一条：快照照样建得成，但它会被打上 stop_managed_apps 的标签，
// 而实际上有一个应用在备份时正写着文件。降级成"停机不全的备份"比失败更糟。
func TestBackupRunPlanBlocksUnresolvedPolicyApp(t *testing.T) {
	src := newFakeBackupRun(t)
	reg := newBackupRunRegistry(t, src)
	action, _ := reg.Action(backup.ActionRun)

	params := runParams()
	params.Apps = params.Apps[:1] // 只解析出 app A，策略里的 app B 没着落
	plan, err := action.Plan(context.Background(), PlanInput{Params: params})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Allowed {
		t.Fatal("停机不全就不该允许备份")
	}
	if len(plan.Blockers) != 1 || plan.Blockers[0].Code != backup.CodePolicyAppsUnavailable {
		t.Fatalf("阻塞码不对：%+v", plan.Blockers)
	}
	// 阻塞理由要点名是哪个应用——否则管理员不知道该去修哪条绑定。
	if !strings.Contains(plan.Blockers[0].Message, runTestAppB) {
		t.Fatalf("要说清少的是哪个应用：%q", plan.Blockers[0].Message)
	}
	if len(src.assessed) != 0 {
		t.Fatal("授权范围不符时不该开始评估")
	}
}

// TestBackupRunPlanFilesOnlyTakesNoApps：files_only 策略没有 app_ids，也就不带应用。
func TestBackupRunPlanFilesOnlyTakesNoApps(t *testing.T) {
	src := newFakeBackupRun(t)
	pol := backup.Policy{
		Name:            "日志目录",
		RepositoryID:    testRepoID,
		Sources:         []backup.Source{{RootID: "root1", RelativePath: "var/log"}},
		ConsistencyMode: backup.ConsistencyFilesOnly,
		Retention:       backup.Retention{KeepLast: 7},
	}
	norm, err := pol.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	src.policy = backup.PolicyRecord{ID: runTestPolicyID, Name: norm.Name, Policy: norm, Version: 4}

	reg := newBackupRunRegistry(t, src)
	action, _ := reg.Action(backup.ActionRun)

	params := runParams()
	params.Apps = nil
	plan, err := action.Plan(context.Background(), PlanInput{Params: params})
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Allowed {
		t.Fatalf("files_only 不带应用是正常形态：%+v", plan.Blockers)
	}
	// 反过来，给 files_only 策略带应用是扩大了授权范围。
	params.Apps = runParams().Apps
	plan, err = action.Plan(context.Background(), PlanInput{Params: params})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Allowed {
		t.Fatal("files_only 策略不该接受任何应用")
	}
}

func TestBackupRunPlanCarriesAssessment(t *testing.T) {
	src := newFakeBackupRun(t)
	reg := newBackupRunRegistry(t, src)
	action, _ := reg.Action(backup.ActionRun)

	plan, err := action.Plan(context.Background(), PlanInput{Params: runParams()})
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Allowed {
		t.Fatalf("应当允许：%+v", plan.Blockers)
	}
	if len(src.assessed) != 1 {
		t.Fatalf("应当评估一次，实际 %d 次", len(src.assessed))
	}
	if got := src.assessed[0]; got.PolicyID != runTestPolicyID || len(got.Apps) != 2 || got.Apps[0].Object != runTestUnit {
		t.Fatalf("评估时要带上解析好的运行对象：%+v", got)
	}
	if len(plan.Locks) != 2 {
		t.Fatalf("仓库锁与策略锁都要带上：%v", plan.Locks)
	}
	if len(plan.Impacts) != 1 || plan.Impacts[0].Kind != backup.ImpactAvailability {
		t.Fatalf("停机影响必须进计划：%+v", plan.Impacts)
	}
	if plan.Target.IdentityFingerprint == "" || plan.Target.StateVersion == "" {
		t.Fatalf("目标身份不能空：%+v", plan.Target)
	}
}

func TestBackupRunExecuteRejectsStalePolicy(t *testing.T) {
	src := newFakeBackupRun(t)
	src.policy.Version = 5 // 预检时是 4
	reg := newBackupRunRegistry(t, src)
	action, _ := reg.Action(backup.ActionRun)

	_, err := action.Execute(context.Background(), ExecuteInput{Params: runParams()})
	if err == nil {
		t.Fatal("策略在预检之后被改过就必须停下")
	}
	if !strings.Contains(err.Error(), "BACKUP_POLICY_VERSION_STALE") {
		t.Fatalf("错误码不对：%v", err)
	}
	if len(src.runs) != 0 {
		t.Fatal("没通过复核就不能开始跑")
	}
}

func TestBackupRunExecuteVerifies(t *testing.T) {
	src := newFakeBackupRun(t)
	reg := newBackupRunRegistry(t, src)
	action, _ := reg.Action(backup.ActionRun)

	res, err := action.Execute(context.Background(), ExecuteInput{Params: runParams(), OperationID: "op-run-1"})
	if err != nil {
		t.Fatalf("应当成功：%v", err)
	}
	if res.Outcome != OutcomeVerified {
		t.Fatalf("读回核对通过才是 verified：%s", res.Outcome)
	}
	if res.ObservedState["snapshot_id"] != runTestSnap {
		t.Fatalf("结果里要有快照 id：%+v", res.ObservedState)
	}
	if res.ObservedState["apps_restored"] != true {
		t.Fatalf("应用有没有拉回来必须写进结果：%+v", res.ObservedState)
	}
	if len(src.runs) != 1 || src.runs[0].OperationID != "op-run-1" {
		t.Fatalf("执行请求不对：%+v", src.runs)
	}
	// 恢复验证还没做，提示里必须点明那是另一件事。
	if !strings.Contains(res.RecoveryHint, "恢复验证") {
		t.Fatalf("提示要说清恢复验证是下一步：%q", res.RecoveryHint)
	}
}

func TestBackupRunExecuteUnverifiedSnapshotIsUnknown(t *testing.T) {
	src := newFakeBackupRun(t)
	// 快照建成了，但 restic stats 读不回来。
	src.run.Verified = false
	src.run.VerifyError = "restic stats 超时"
	src.runErr = errors.New("backup: 快照建完后读不回来：restic stats 超时")
	reg := newBackupRunRegistry(t, src)
	action, _ := reg.Action(backup.ActionRun)

	res, err := action.Execute(context.Background(), ExecuteInput{Params: runParams()})
	if err != nil {
		t.Fatalf("这种情况要报 unknown，不是失败：%v", err)
	}
	if res.Outcome != OutcomeUnknown {
		t.Fatalf("没读回核对就是 unknown：%s", res.Outcome)
	}
	if !strings.Contains(res.RecoveryHint, "backup.snapshots") {
		t.Fatalf("提示要告诉人先去确认快照在不在：%q", res.RecoveryHint)
	}
}

func TestBackupRunExecuteFailure(t *testing.T) {
	src := newFakeBackupRun(t)
	src.run.SnapshotID = ""
	src.runErr = errors.New("BACKUP_SOURCE_MISSING：来源 srv/app 不存在")
	reg := newBackupRunRegistry(t, src)
	action, _ := reg.Action(backup.ActionRun)

	_, err := action.Execute(context.Background(), ExecuteInput{Params: runParams()})
	if err == nil {
		t.Fatal("没建成快照就是失败")
	}
	var reason interface{ FailureReason() string }
	if !errors.As(err, &reason) {
		t.Fatalf("失败原因要能进任务的 error.message：%T", err)
	}
	if !strings.Contains(reason.FailureReason(), "BACKUP_SOURCE_MISSING") {
		t.Fatalf("失败原因丢了：%q", reason.FailureReason())
	}
}

func TestBackupSnapshotsQuery(t *testing.T) {
	src := newFakeBackupRun(t)
	reg := newBackupRunRegistry(t, src)
	query, _ := reg.Query(backup.QuerySnapshots)

	out, err := query.Run(context.Background(), backupSnapshotsParams{
		RepositoryID: testRepoID, PolicyID: runTestPolicyID,
	})
	if err != nil {
		t.Fatal(err)
	}
	data := out.(map[string]any)
	if data["count"] != 1 {
		t.Fatalf("条数不对：%+v", data)
	}
	if data["size_included"] != false {
		t.Fatal("列表不含大小，必须说出来")
	}
	items := data["snapshots"].([]map[string]any)
	if _, ok := items[0]["size_bytes"]; ok {
		t.Fatal("列表里不该有 size_bytes（那是每条一次 restic stats）")
	}
	if items[0]["restore_verified_at"] != nil {
		t.Fatal("P12.3 之前恢复验证恒为 null")
	}
	if len(src.listReqs) != 1 || src.listReqs[0].PolicyID != runTestPolicyID {
		t.Fatalf("策略过滤没带下去：%+v", src.listReqs)
	}
}

func TestBackupSnapshotsQueryUnregisteredRepository(t *testing.T) {
	src := newFakeBackupRun(t)
	src.found = false
	reg := newBackupRunRegistry(t, src)
	query, _ := reg.Query(backup.QuerySnapshots)

	out, err := query.Run(context.Background(), backupSnapshotsParams{RepositoryID: testRepoID})
	if err != nil {
		t.Fatal(err)
	}
	data := out.(map[string]any)
	if data["registered"] != false {
		t.Fatalf("没登记就说没登记：%+v", data)
	}
	if len(src.listReqs) != 0 {
		t.Fatal("没登记就不要去问 restic")
	}
}

func TestBackupSnapshotQueryDetail(t *testing.T) {
	src := newFakeBackupRun(t)
	reg := newBackupRunRegistry(t, src)
	query, _ := reg.Query(backup.QuerySnapshot)

	out, err := query.Run(context.Background(), backupSnapshotParams{RepositoryID: testRepoID, SnapshotID: runTestSnap})
	if err != nil {
		t.Fatal(err)
	}
	data := out.(map[string]any)
	if data["found"] != true || data["size_bytes"] != int64(4096) || data["file_count"] != int64(12) {
		t.Fatalf("详情要带 stats：%+v", data)
	}
}

func TestBackupSnapshotQueryNotFound(t *testing.T) {
	src := newFakeBackupRun(t)
	src.detailErr = backup.ErrSnapshotNotFound
	reg := newBackupRunRegistry(t, src)
	query, _ := reg.Query(backup.QuerySnapshot)

	out, err := query.Run(context.Background(), backupSnapshotParams{RepositoryID: testRepoID, SnapshotID: runTestSnap})
	if err != nil {
		t.Fatalf("不在仓库里是一种状态，不是错误：%v", err)
	}
	data := out.(map[string]any)
	if data["found"] != false || data["registered"] != true {
		t.Fatalf("要说清「仓库在、快照不在」：%+v", data)
	}
}

// TestBackupPlanBudgetsLeaveRoomForSlowSFTP：预检预算必须容得下“慢但活着”的 SFTP。
//
// 拨号是两轮封顶（每轮 DefaultDialTimeout），预算连两轮都盖不住时，一次正常的
// 慢握手就会被写成 BACKUP_SFTP_UNREACHABLE——2026-10-25 的 24.04 验收机上就是这么
// 撞到的：两条 15 秒握手超时之后，第三条 11 秒才认证成功，45 秒预算已经见底。
// 同时预算必须小于 Web 的 PreflightAgentTimeout，真超时的时候先说话的是代理，
// 而不是 Web 猜的 AGENT_UNAVAILABLE。
func TestBackupPlanBudgetsLeaveRoomForSlowSFTP(t *testing.T) {
	for name, budget := range map[string]time.Duration{
		"backupPlanBudget":    backupPlanBudget,
		"backupRunPlanBudget": backupRunPlanBudget,
	} {
		if budget <= 2*backup.DefaultDialTimeout {
			t.Fatalf("%s = %s，盖不住两轮拨号 %s", name, budget, 2*backup.DefaultDialTimeout)
		}
		if budget >= operations.PreflightAgentTimeout {
			t.Fatalf("%s = %s，不小于 Web 的等待上限 %s：超时会由 Web 先报",
				name, budget, operations.PreflightAgentTimeout)
		}
	}
}
