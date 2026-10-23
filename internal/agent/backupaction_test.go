package agent

// backup.repository.initialize / backup.repository.state 的翻译层（P12.1）。
//
// internal/backup 的测试负责“谁被拒绝、为什么、核对在哪一步”；这里只验代理有没有
// 给它添油加醋，四件事：
//
//  1. 参数只有 repository_id 与 version，别的字段一律拒绝；
//  2. 定义取自代理自己的镜像，版本不符就拒绝（Web 改过的定义必须重新预检）；
//  3. 执行结果**只有**读回 config 才是 verified；读不回就报 unknown，不报成功；
//  4. 只读查询不写任何东西，找不到定义时说“没登记”，不说“空”。

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"upanel/internal/backup"
)

// fakeBackup 是 agent 层的假备份端。
type fakeBackup struct {
	repo    backup.Repository
	found   bool
	repoErr error

	assessment backup.Assessment
	assessErr  error
	assessed   []backup.AssessRequest

	execution backup.Execution
	execErr   error
	requests  []backup.ExecuteRequest

	state    backup.RepositoryState
	stateErr error
	states   []backup.StateRequest
}

func (f *fakeBackup) GetRepository(_ context.Context, id string) (backup.Repository, bool, error) {
	if f.repoErr != nil {
		return backup.Repository{}, false, f.repoErr
	}
	if !f.found || f.repo.ID != id {
		return backup.Repository{}, false, nil
	}
	return f.repo, true, nil
}

func (f *fakeBackup) Assess(_ context.Context, req backup.AssessRequest) (backup.Assessment, error) {
	f.assessed = append(f.assessed, req)
	return f.assessment, f.assessErr
}

func (f *fakeBackup) Execute(_ context.Context, req backup.ExecuteRequest) (backup.Execution, error) {
	f.requests = append(f.requests, req)
	if req.Progress != nil {
		req.Progress("init", map[string]any{"repository": "x"})
	}
	return f.execution, f.execErr
}

func (f *fakeBackup) State(_ context.Context, req backup.StateRequest) (backup.RepositoryState, error) {
	f.states = append(f.states, req)
	return f.state, f.stateErr
}

const testRepoID = "rep_0123456789abcdef0123456789abcdef"

// fakeDefinition 返回一条已规范化的本地定义。
func fakeDefinition(t *testing.T) backup.Definition {
	t.Helper()
	def, err := backup.Definition{
		Name:              "夜间根文件",
		Backend:           backup.BackendLocal,
		Location:          backup.Location{RootID: "root1", RelativePath: "backups/nightly"},
		PasswordSecretRef: "sec_0123456789abcdef0123456789abcdef",
	}.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	return def
}

// newFakeBackup 造一个“一切正常”的假实现。
func newFakeBackup(t *testing.T) *fakeBackup {
	t.Helper()
	def := fakeDefinition(t)
	return &fakeBackup{
		repo:  backup.Repository{ID: testRepoID, Name: def.Name, Definition: def, Version: 3},
		found: true,
		assessment: backup.Assessment{
			RepositoryID: testRepoID,
			Definition:   def,
			AbsolutePath: "/srv/data/backups/nightly",
			Allowed:      true,
			Locks:        []string{backup.LockName(testRepoID)},
			Impacts: []backup.Impact{
				{Kind: backup.ImpactCreateRepository, Description: "在 local:root1:/backups/nightly 新建一个加密仓库"},
			},
			Target: backup.Target{
				DisplayName:         def.DisplayName(),
				IdentityFingerprint: "sha256:abc",
				StateVersion:        "backup-repository:" + testRepoID + ":v3",
			},
			Capacity: backup.Capacity{Supported: true, Target: "/srv/data", TotalBytes: 100 << 30, AvailableBytes: 40 << 30},
		},
		execution: backup.Execution{
			RepositoryID:  testRepoID,
			AbsolutePath:  "/srv/data/backups/nightly",
			RepoID:        "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			RepoVersion:   2,
			ResticVersion: "restic 0.12.1 compiled with go1.18.1 on linux/amd64",
			Verified:      true,
			InitializedAt: time.Date(2026, 11, 2, 3, 4, 5, 0, time.UTC),
			Duration:      1200 * time.Millisecond,
			CapacityBefore: backup.Capacity{
				Supported: true, Target: "/srv/data", TotalBytes: 100 << 30, AvailableBytes: 40 << 30,
			},
			CapacityAfter: backup.Capacity{
				Supported: true, Target: "/srv/data", TotalBytes: 100 << 30, AvailableBytes: 39 << 30,
			},
		},
		state: backup.RepositoryState{
			RepositoryID:  testRepoID,
			Backend:       backup.BackendLocal,
			Location:      "local:root1:/backups/nightly",
			Version:       3,
			Initialized:   boolRef(true),
			RepoID:        "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			ResticVersion: "restic 0.12.1",
		},
	}
}

func newBackupRegistry(t *testing.T, src backupSource) *Registry {
	t.Helper()
	reg := NewRegistry()
	if err := RegisterBackupAction(reg, src); err != nil {
		t.Fatalf("登记 %s 失败: %v", backup.ActionRepositoryInitialize, err)
	}
	if err := RegisterBackupQueries(reg, src); err != nil {
		t.Fatalf("登记 %s 失败: %v", backup.QueryRepositoryState, err)
	}
	return reg
}

func TestRegisterBackupActionRequiresSource(t *testing.T) {
	if err := RegisterBackupAction(NewRegistry(), nil); err == nil {
		t.Fatal("没有实现端就不该登记成功")
	}
}

func TestRegisterBackupActionNames(t *testing.T) {
	reg := newBackupRegistry(t, newFakeBackup(t))
	if _, ok := reg.Action(backup.ActionRepositoryInitialize); !ok {
		t.Fatalf("%s 没有登记", backup.ActionRepositoryInitialize)
	}
	if !contains(reg.ExecutableActionNames(), backup.ActionRepositoryInitialize) {
		t.Fatalf("初始化仓库应当可执行：%v", reg.ExecutableActionNames())
	}
	if _, ok := reg.Query(backup.QueryRepositoryState); !ok {
		t.Fatalf("%s 没有登记", backup.QueryRepositoryState)
	}
}

// TestBackupPolicy：契约的 R 列要求再认证；短语留给格式化/删除/清理；可取消。
func TestBackupPolicy(t *testing.T) {
	reg := newBackupRegistry(t, newFakeBackup(t))
	action, _ := reg.Action(backup.ActionRepositoryInitialize)
	policy, err := action.ResolvePolicy(nil)
	if err != nil {
		t.Fatalf("策略不合法: %v", err)
	}
	if !policy.RequiresReauth {
		t.Fatal("初始化仓库必须再认证（契约 R 列）")
	}
	if policy.RequiresPhrase {
		t.Fatalf("不该要求短语（%q）", policy.PhraseVerb)
	}
	if !policy.Cancellable {
		t.Fatal("SFTP 上建仓库可能慢，应当允许取消")
	}
}

func TestBackupDecodeStrict(t *testing.T) {
	reg := newBackupRegistry(t, newFakeBackup(t))
	action, _ := reg.Action(backup.ActionRepositoryInitialize)

	params, err := action.Decode(json.RawMessage(`{"repository_id":"` + testRepoID + `","version":3}`))
	if err != nil {
		t.Fatalf("正常参数被拒: %v", err)
	}
	got := params.(backupInitParams)
	if got.RepositoryID != testRepoID || got.Version != 3 {
		t.Fatalf("参数解码错：%+v", got)
	}

	bad := []string{
		`{"repository_id":"` + testRepoID + `","version":3,"host":"evil"}`, // 未知字段
		`{"version":3}`,                          // 缺 id
		`{"repository_id":"` + testRepoID + `"}`, // 缺 version
		`{"repository_id":"` + testRepoID + `","version":0}`,
		`{"repository_id":"","version":1}`,
	}
	for _, raw := range bad {
		if _, err := action.Decode(json.RawMessage(raw)); !errors.Is(err, ErrParamsInvalid) {
			t.Fatalf("%s 应当被拒: %v", raw, err)
		}
	}
}

func TestBackupPlanBlocksUnknownRepository(t *testing.T) {
	src := newFakeBackup(t)
	src.found = false
	reg := newBackupRegistry(t, src)
	action, _ := reg.Action(backup.ActionRepositoryInitialize)

	plan, err := action.Plan(context.Background(), PlanInput{
		Params: backupInitParams{RepositoryID: testRepoID, Version: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Allowed {
		t.Fatal("代理里没有定义时必须拒绝")
	}
	if len(plan.Blockers) != 1 || plan.Blockers[0].Code != "BACKUP_REPOSITORY_NOT_FOUND" {
		t.Fatalf("阻塞项应是 BACKUP_REPOSITORY_NOT_FOUND：%+v", plan.Blockers)
	}
	if len(src.assessed) != 0 {
		t.Fatal("没有定义就不该去评估现场（那会白拨一次 SFTP）")
	}
	// 即使拒绝也要带锁名：两个请求同时点不应当都走到执行。
	if len(plan.Locks) != 1 || plan.Locks[0] != backup.LockName(testRepoID) {
		t.Fatalf("锁名不对：%v", plan.Locks)
	}
}

func TestBackupPlanBlocksStaleVersion(t *testing.T) {
	src := newFakeBackup(t)
	reg := newBackupRegistry(t, src)
	action, _ := reg.Action(backup.ActionRepositoryInitialize)

	plan, err := action.Plan(context.Background(), PlanInput{
		Params: backupInitParams{RepositoryID: testRepoID, Version: 2}, // 镜像里是 3
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Allowed {
		t.Fatal("版本不符必须拒绝（定义在预检之后被改过）")
	}
	if plan.Blockers[0].Code != "BACKUP_REPOSITORY_VERSION_STALE" {
		t.Fatalf("阻塞项应是版本过期：%+v", plan.Blockers)
	}
	if len(src.assessed) != 0 {
		t.Fatal("版本不符不该评估现场")
	}
}

func TestBackupPlanMapping(t *testing.T) {
	src := newFakeBackup(t)
	src.assessment.Allowed = false
	src.assessment.Blockers = []backup.Reason{{Code: backup.CodeRepositoryExists, Message: "已经是一个 restic 仓库"}}
	src.assessment.Warnings = []backup.Reason{{Code: backup.CodeLowSpace, Message: "空间偏少"}}
	src.assessment.Locks = nil // 兜底
	reg := newBackupRegistry(t, src)
	action, _ := reg.Action(backup.ActionRepositoryInitialize)

	plan, err := action.Plan(context.Background(), PlanInput{
		Params: backupInitParams{RepositoryID: testRepoID, Version: 3},
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Allowed {
		t.Fatal("评估拒绝时计划不能允许")
	}
	if len(plan.Blockers) != 1 || plan.Blockers[0].Code != backup.CodeRepositoryExists {
		t.Fatalf("阻塞项没有逐字段直译：%+v", plan.Blockers)
	}
	if len(plan.Warnings) != 1 || plan.Warnings[0].Code != backup.CodeLowSpace {
		t.Fatalf("告警没有直译：%+v", plan.Warnings)
	}
	if len(plan.Impacts) != 1 || plan.Impacts[0].Kind != backup.ImpactCreateRepository {
		t.Fatalf("影响没有直译：%+v", plan.Impacts)
	}
	if plan.Target.IdentityFingerprint != "sha256:abc" {
		t.Fatalf("身份指纹没有直译：%q", plan.Target.IdentityFingerprint)
	}
	if plan.Target.StateVersion != "backup-repository:"+testRepoID+":v3" {
		t.Fatalf("状态版本没有直译：%q", plan.Target.StateVersion)
	}
	if len(plan.Locks) != 1 || plan.Locks[0] != backup.LockName(testRepoID) {
		t.Fatalf("锁名兜底失败：%v", plan.Locks)
	}
	// 评估请求必须带上镜像里的版本（状态版本由它决定）。
	if len(src.assessed) != 1 || src.assessed[0].Version != 3 {
		t.Fatalf("评估请求没有带版本：%+v", src.assessed)
	}
}

func TestBackupExecuteVerified(t *testing.T) {
	src := newFakeBackup(t)
	reg := newBackupRegistry(t, src)
	action, _ := reg.Action(backup.ActionRepositoryInitialize)

	result, err := action.Execute(context.Background(), ExecuteInput{
		Params:      backupInitParams{RepositoryID: testRepoID, Version: 3},
		OperationID: "op_1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != OutcomeVerified {
		t.Fatalf("读回 config 之后必须是 verified，得到 %q", result.Outcome)
	}
	if result.VerifiedAt.IsZero() {
		t.Fatal("verified 必须带核验时间")
	}
	if result.Data["repo_id"] == nil {
		t.Fatalf("结果里必须带仓库 id：%+v", result.Data)
	}
	if got := result.ObservedState["repo_id"]; got == nil {
		t.Fatalf("observed_state 必须带仓库 id：%+v", result.ObservedState)
	}
	if got := result.ObservedState["verified_by"]; got != "restic cat config" {
		t.Fatalf("核验方式要写清楚：%v", got)
	}
	if !strings.Contains(result.RecoveryHint, "恢复") {
		t.Fatalf("恢复提示必须说清“备份成功不等于恢复已验证”：%q", result.RecoveryHint)
	}
	capacity, ok := result.ObservedState["capacity_after"].(map[string]any)
	if !ok || capacity["available_bytes"] == nil {
		t.Fatalf("容量读数要进 observed_state：%+v", result.ObservedState)
	}
	if len(src.requests) != 1 || src.requests[0].OperationID != "op_1" {
		t.Fatalf("执行请求没有带 operation_id：%+v", src.requests)
	}
}

// TestBackupExecuteUnverifiedStaysUnknown：核对失败**不许**报成功。
func TestBackupExecuteUnverifiedStaysUnknown(t *testing.T) {
	src := newFakeBackup(t)
	src.execution.Verified = false
	src.execution.RepoID = ""
	src.execution.VerifyError = "目标位置还不是一个 restic 仓库"
	reg := newBackupRegistry(t, src)
	action, _ := reg.Action(backup.ActionRepositoryInitialize)

	result, err := action.Execute(context.Background(), ExecuteInput{
		Params:      backupInitParams{RepositoryID: testRepoID, Version: 3},
		OperationID: "op_1",
	})
	if err != nil {
		t.Fatalf("核对失败不是执行错误（要报 unknown）：%v", err)
	}
	if result.Outcome != OutcomeUnknown {
		t.Fatalf("读不回 config 只能报 unknown，得到 %q", result.Outcome)
	}
	if result.Data["verified"] != false {
		t.Fatalf("verified 必须是 false：%+v", result.Data)
	}
	if result.ObservedState["verify_error"] == nil {
		t.Fatalf("unknown 必须留下原因：%+v", result.ObservedState)
	}
	if result.RecoveryHint == "" {
		t.Fatal("unknown 必须给恢复提示")
	}
}

func TestBackupExecuteRejectsStaleVersion(t *testing.T) {
	src := newFakeBackup(t)
	reg := newBackupRegistry(t, src)
	action, _ := reg.Action(backup.ActionRepositoryInitialize)

	_, err := action.Execute(context.Background(), ExecuteInput{
		Params:      backupInitParams{RepositoryID: testRepoID, Version: 2},
		OperationID: "op_1",
	})
	if err == nil {
		t.Fatal("版本在执行前变了必须拒绝")
	}
	if !strings.Contains(err.Error(), "BACKUP_REPOSITORY_VERSION_STALE") {
		t.Fatalf("错误里要带码：%v", err)
	}
	if len(src.requests) != 0 {
		t.Fatal("版本不符不该真的执行")
	}
}

func TestBackupExecuteCanceledIsNotFailure(t *testing.T) {
	src := newFakeBackup(t)
	src.execErr = backup.ErrCanceled
	reg := newBackupRegistry(t, src)
	action, _ := reg.Action(backup.ActionRepositoryInitialize)

	_, err := action.Execute(context.Background(), ExecuteInput{
		Params:      backupInitParams{RepositoryID: testRepoID, Version: 3},
		OperationID: "op_1",
	})
	if !errors.Is(err, ErrCanceled) {
		t.Fatalf("取消要翻成 ErrCanceled（任务记 canceled 而不是 failed），得到 %v", err)
	}
}

func TestBackupExecuteFailureIsReasoned(t *testing.T) {
	src := newFakeBackup(t)
	src.execErr = errors.New("BACKUP_NO_SPACE：/srv/data 所在文件系统没有可用空间")
	reg := newBackupRegistry(t, src)
	action, _ := reg.Action(backup.ActionRepositoryInitialize)

	_, err := action.Execute(context.Background(), ExecuteInput{
		Params:      backupInitParams{RepositoryID: testRepoID, Version: 3},
		OperationID: "op_1",
	})
	if err == nil {
		t.Fatal("执行失败必须报错")
	}
	var reasoned FailureReasoner
	if !errors.As(err, &reasoned) {
		t.Fatalf("执行失败要带一句人话（FailureReasoner）：%v", err)
	}
	if !strings.Contains(reasoned.FailureReason(), "空间") {
		t.Fatalf("原因没有透出来：%q", reasoned.FailureReason())
	}
}

func TestBackupStateQuery(t *testing.T) {
	src := newFakeBackup(t)
	reg := newBackupRegistry(t, src)
	query, _ := reg.Query(backup.QueryRepositoryState)

	params, err := query.Decode(json.RawMessage(`{"repository_id":"` + testRepoID + `"}`))
	if err != nil {
		t.Fatal(err)
	}
	out, err := query.Run(context.Background(), params)
	if err != nil {
		t.Fatal(err)
	}
	m, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("返回形状不对：%T", out)
	}
	if m["registered"] != true {
		t.Fatalf("已登记却报未登记：%+v", m)
	}
	if len(src.states) != 1 || src.states[0].Version != 3 {
		t.Fatalf("查询要带上镜像版本：%+v", src.states)
	}
}

func TestBackupStateQueryUnregistered(t *testing.T) {
	src := newFakeBackup(t)
	src.found = false
	reg := newBackupRegistry(t, src)
	query, _ := reg.Query(backup.QueryRepositoryState)

	out, err := query.Run(context.Background(), backupStateParams{RepositoryID: testRepoID})
	if err != nil {
		t.Fatal(err)
	}
	m := out.(map[string]any)
	if m["registered"] != false {
		t.Fatalf("未登记必须报 registered=false：%+v", m)
	}
	if len(src.states) != 0 {
		t.Fatal("未登记不该去读现场")
	}
}

func TestBackupStateQueryDecodeStrict(t *testing.T) {
	reg := newBackupRegistry(t, newFakeBackup(t))
	query, _ := reg.Query(backup.QueryRepositoryState)
	for _, raw := range []string{`{}`, `{"repository_id":""}`, `{"repository_id":"x","extra":1}`} {
		if _, err := query.Decode(json.RawMessage(raw)); !errors.Is(err, ErrParamsInvalid) {
			t.Fatalf("%s 应当被拒: %v", raw, err)
		}
	}
}

// boolRef 让测试能给出三态字段里的“读到的事实”。
func boolRef(v bool) *bool { return &v }
