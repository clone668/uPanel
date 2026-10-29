package agent

// deployment.apply / deployment.remove 的翻译层（P15.2）。
//
// internal/deploy 的测试负责“什么样的定义被拒绝、单元长什么样、落地按什么顺序”；
// 这里只验代理有没有把话传错，五件事：
//
//  1. 预检结论原样变成计划：目标指纹由部署标识派生、状态版本就是磁盘上那份配置的摘要、
//     锁是 compose 项目 + 端口 + 卷；
//  2. deploy 的**判决**（ServiceError）变成 blocker，不是 500——“这个版本不做更新”
//     是拒绝理由，不是故障；
//  3. 定义不自洽（ValidationError）走参数错误；
//  4. 单元起来之后多问一句容器：0 个容器降级成 partial（并说清看哪两条日志），
//     读不到容器状态不降级——读不到不等于部署失败；
//  5. 删除报告里必须写着数据保留。

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"upanel/internal/backup"
	"upanel/internal/containers"
	"upanel/internal/deploy"
)

// fakeDeploySource 是 agent 层的假部署端。
type fakeDeploySource struct {
	apply    deploy.ApplyAssessment
	applyErr error
	applies  []deploy.ApplyParams

	applyOutcome deploy.ApplyOutcome
	applyExecErr error
	expectState  string
	executed     int

	remove        deploy.RemoveAssessment
	removeErr     error
	removeOut     deploy.RemoveOutcome
	removeExecErr error

	update        deploy.UpdateAssessment
	updateErr     error
	updates       []deploy.UpdateParams
	updateOut     deploy.UpdateOutcome
	updateExecErr error
	operationID   string
}

func (f *fakeDeploySource) AssessApply(_ context.Context, p deploy.ApplyParams) (deploy.ApplyAssessment, error) {
	f.applies = append(f.applies, p)
	return f.apply, f.applyErr
}

func (f *fakeDeploySource) ExecuteApply(_ context.Context, p deploy.ApplyParams, expectState string,
	_ func(step string, fields map[string]any)) (deploy.ApplyOutcome, error) {
	f.executed++
	f.expectState = expectState
	if f.applyExecErr != nil {
		return deploy.ApplyOutcome{}, f.applyExecErr
	}
	return f.applyOutcome, nil
}

func (f *fakeDeploySource) AssessRemove(_ context.Context, p deploy.RemoveParams) (deploy.RemoveAssessment, error) {
	f.applies = append(f.applies, deploy.ApplyParams{})
	return f.remove, f.removeErr
}

func (f *fakeDeploySource) ExecuteRemove(_ context.Context, p deploy.RemoveParams, expectState string,
	_ func(step string, fields map[string]any)) (deploy.RemoveOutcome, error) {
	f.executed++
	f.expectState = expectState
	if f.removeExecErr != nil {
		return deploy.RemoveOutcome{}, f.removeExecErr
	}
	return f.removeOut, nil
}

func (f *fakeDeploySource) AssessUpdate(_ context.Context, p deploy.UpdateParams) (deploy.UpdateAssessment, error) {
	f.updates = append(f.updates, p)
	return f.update, f.updateErr
}

func (f *fakeDeploySource) ExecuteUpdate(_ context.Context, p deploy.UpdateParams, expectState, operationID string,
	_ func(step string, fields map[string]any)) (deploy.UpdateOutcome, error) {
	f.executed++
	f.expectState = expectState
	f.operationID = operationID
	f.updates = append(f.updates, p)
	if f.updateExecErr != nil {
		return f.updateOut, f.updateExecErr
	}
	return f.updateOut, nil
}

// fakeProjects 是容器项目清单的假实现。
type fakeProjects struct {
	response containers.ProjectsResponse
	err      error
}

func (f fakeProjects) Projects(context.Context) (containers.ProjectsResponse, error) {
	return f.response, f.err
}

// deployApplyParams 造一份 ApplyParams（字段够用即可，判决由假实现给）。
func deployApplyParams(t *testing.T) deploy.ApplyParams {
	t.Helper()
	definition := deploy.Definition{
		Name:               "webapp",
		ImageReference:     "nginx:1.27",
		ContainerPort:      80,
		HostBindAddress:    "127.0.0.1",
		HostPort:           8080,
		ResourceLimits:     deploy.ResourceLimits{MemoryLimitBytes: 256 << 20, CPULimit: "0.5"},
		VolumeDependencies: []string{"vol_media"},
	}
	return deploy.ApplyParams{Definition: definition}
}

// newDeployTestRegistry 登记两条部署动作，返回登记表与假实现。
func newDeployTestRegistry(t *testing.T, src *fakeDeploySource, projects deployProjectLister) *Registry {
	t.Helper()
	reg := NewRegistry()
	if err := RegisterDeployActions(reg, src, projects); err != nil {
		t.Fatalf("登记部署动作失败：%v", err)
	}
	return reg
}

// TestDeployApplyPlanCarriesAssessment：预检结论原样进计划。
func TestDeployApplyPlanCarriesAssessment(t *testing.T) {
	src := &fakeDeploySource{apply: deploy.ApplyAssessment{
		Definition:   deployApplyParams(t).Definition,
		DeploymentID: deploy.DeploymentIDForName("webapp"),
		ProjectName:  "webapp",
		ConfigDigest: "sha256:" + strings.Repeat("a", 64),
		UnitName:     "upanel-deploy-webapp.service",
		StateVersion: "sha256:" + strings.Repeat("b", 64),
		Allowed:      false,
		Blockers:     []deploy.Notice{{Code: "volume_not_ready", Message: "卷没挂上"}},
		Warnings:     []deploy.Notice{{Code: "health_probe_pending", Message: "健康检查在 P15.3"}},
		Claims:       []deploy.Claim{{Address: "127.0.0.1", Port: 8080, Protocol: deploy.ProtocolTCP}},
	}}
	reg := newDeployTestRegistry(t, src, nil)
	action, ok := reg.Action(deploy.ActionApply)
	if !ok {
		t.Fatal("deployment.apply 没有登记")
	}
	params, err := action.Decode(json.RawMessage(`{"deployment_id":null,"version":"","definition":{"name":"webapp"}}`))
	if err != nil {
		t.Fatalf("解码失败：%v", err)
	}
	plan, err := action.Plan(context.Background(), PlanInput{Params: params})
	if err != nil {
		t.Fatalf("预检失败：%v", err)
	}
	if plan.Allowed {
		t.Fatal("有阻塞项时不该允许执行")
	}
	if len(plan.Blockers) != 1 || plan.Blockers[0].Code != "volume_not_ready" {
		t.Fatalf("阻塞项 = %+v", plan.Blockers)
	}
	if plan.Target.StateVersion != src.apply.StateVersion {
		t.Fatalf("状态版本 = %q", plan.Target.StateVersion)
	}
	want := "deployment:" + deploy.DeploymentIDForName("webapp")
	if plan.Target.IdentityFingerprint != want {
		t.Fatalf("身份指纹 = %q，期望 %q", plan.Target.IdentityFingerprint, want)
	}
	locks := strings.Join(plan.Locks, ",")
	for _, wantLock := range []string{"compose:webapp", "port:tcp/127.0.0.1/8080", "volume:vol_media"} {
		if !strings.Contains(locks, wantLock) {
			t.Fatalf("锁里缺 %s：%s", wantLock, locks)
		}
	}
	if len(plan.Impacts) == 0 {
		t.Fatal("写动作至少要说明它改什么")
	}
}

// TestDeployApplyPlanServiceErrorBecomesBlocker：判决变成 blocker，不是 500。
func TestDeployApplyPlanServiceErrorBecomesBlocker(t *testing.T) {
	src := &fakeDeploySource{applyErr: &deploy.ServiceError{
		Code: "update_not_implemented", Message: "换定义属于 deployment.update",
	}}
	reg := newDeployTestRegistry(t, src, nil)
	action, _ := reg.Action(deploy.ActionApply)
	params, err := action.Decode(json.RawMessage(`{"version":"v1","definition":{"name":"webapp"}}`))
	if err != nil {
		t.Fatalf("解码失败：%v", err)
	}
	plan, err := action.Plan(context.Background(), PlanInput{Params: params})
	if err != nil {
		t.Fatalf("判决不该变成错误：%v", err)
	}
	if plan.Allowed {
		t.Fatal("判决为拒绝时 Allowed 必须为 false")
	}
	if len(plan.Blockers) != 1 || plan.Blockers[0].Code != "update_not_implemented" {
		t.Fatalf("阻塞项 = %+v", plan.Blockers)
	}
	if !strings.Contains(plan.Blockers[0].Message, "deployment.update") {
		t.Fatalf("阻塞说明没了原因：%q", plan.Blockers[0].Message)
	}
}

// TestDeployApplyPlanValidationErrorIsParamsInvalid：定义不自洽走 400。
func TestDeployApplyPlanValidationErrorIsParamsInvalid(t *testing.T) {
	src := &fakeDeploySource{applyErr: &deploy.ValidationError{
		Field: "host_port", Code: deploy.CodeOutOfRange, Message: "宿主端口超出范围",
	}}
	reg := newDeployTestRegistry(t, src, nil)
	action, _ := reg.Action(deploy.ActionApply)
	params, err := action.Decode(json.RawMessage(`{"definition":{"name":"webapp"}}`))
	if err != nil {
		t.Fatalf("解码失败：%v", err)
	}
	if _, err := action.Plan(context.Background(), PlanInput{Params: params}); err == nil {
		t.Fatal("定义不自洽时必须报参数错误")
	} else if !errors.Is(err, ErrParamsInvalid) {
		t.Fatalf("错误类型 = %v，期望 ErrParamsInvalid", err)
	}
}

// TestDeployApplyExecutePassesStateVersion：执行前复核拿的是预检时的状态版本。
func TestDeployApplyExecutePassesStateVersion(t *testing.T) {
	src := &fakeDeploySource{applyOutcome: deploy.ApplyOutcome{
		Report:      deploy.ApplyReport{DeploymentID: deploy.DeploymentIDForName("webapp"), UnitName: "upanel-deploy-webapp.service", Enabled: true, Active: true, Written: true},
		ProjectName: "webapp",
	}}
	reg := newDeployTestRegistry(t, src, nil)
	action, _ := reg.Action(deploy.ActionApply)
	params, err := action.Decode(json.RawMessage(`{"definition":{"name":"webapp"}}`))
	if err != nil {
		t.Fatalf("解码失败：%v", err)
	}
	result, err := action.Execute(context.Background(), ExecuteInput{
		Params: params, Target: Target{StateVersion: "sha256:" + strings.Repeat("c", 64)},
	})
	if err != nil {
		t.Fatalf("执行失败：%v", err)
	}
	if src.expectState != "sha256:"+strings.Repeat("c", 64) {
		t.Fatalf("传给部署端的状态版本 = %q", src.expectState)
	}
	if result.Outcome != OutcomeVerified {
		t.Fatalf("outcome = %q", result.Outcome)
	}
	if result.ObservedState["unit_active"] != true {
		t.Fatalf("observed 里应有单元读数：%v", result.ObservedState)
	}
	if result.ObservedState["containers_checked"] != false {
		t.Fatalf("没有容器读取端时应写明没核对：%v", result.ObservedState)
	}
}

// TestDeployApplyExecuteReadsContainers：项目里有容器时记下总数与运行数。
func TestDeployApplyExecuteReadsContainers(t *testing.T) {
	deploymentID := deploy.DeploymentIDForName("webapp")
	src := &fakeDeploySource{applyOutcome: deploy.ApplyOutcome{
		Report:      deploy.ApplyReport{DeploymentID: deploymentID, UnitName: "upanel-deploy-webapp.service", Enabled: true, Active: true},
		ProjectName: "webapp",
	}}
	projects := fakeProjects{response: containers.ProjectsResponse{
		Available: true,
		Projects: []containers.Project{
			{ID: "dep_other", Name: "other", Total: 3, Running: 3},
			{ID: deploymentID, Name: "webapp", Total: 2, Running: 2},
		},
	}}
	reg := newDeployTestRegistry(t, src, projects)
	action, _ := reg.Action(deploy.ActionApply)
	params, _ := action.Decode(json.RawMessage(`{"definition":{"name":"webapp"}}`))
	result, err := action.Execute(context.Background(), ExecuteInput{Params: params})
	if err != nil {
		t.Fatalf("执行失败：%v", err)
	}
	if result.Outcome != OutcomeVerified {
		t.Fatalf("outcome = %q", result.Outcome)
	}
	if result.ObservedState["containers_total"] != 2 || result.ObservedState["containers_running"] != 2 {
		t.Fatalf("容器读数 = %v", result.ObservedState)
	}
}

// TestDeployApplyExecutePartialWhenProjectEmpty：单元 active 但没有容器 → partial。
func TestDeployApplyExecutePartialWhenProjectEmpty(t *testing.T) {
	deploymentID := deploy.DeploymentIDForName("webapp")
	src := &fakeDeploySource{applyOutcome: deploy.ApplyOutcome{
		Report:      deploy.ApplyReport{DeploymentID: deploymentID, UnitName: "upanel-deploy-webapp.service", Enabled: true, Active: true},
		ProjectName: "webapp",
	}}
	projects := fakeProjects{response: containers.ProjectsResponse{Available: true}}
	reg := newDeployTestRegistry(t, src, projects)
	action, _ := reg.Action(deploy.ActionApply)
	params, _ := action.Decode(json.RawMessage(`{"definition":{"name":"webapp"}}`))
	result, err := action.Execute(context.Background(), ExecuteInput{Params: params})
	if err != nil {
		t.Fatalf("执行失败：%v", err)
	}
	if result.Outcome != OutcomePartial {
		t.Fatalf("outcome = %q，期望 partial", result.Outcome)
	}
	if !strings.Contains(result.RecoveryHint, "docker compose -p webapp ps") {
		t.Fatalf("恢复提示 = %q", result.RecoveryHint)
	}
	if result.ObservedState["containers_total"] != 0 {
		t.Fatalf("容器读数 = %v", result.ObservedState)
	}
}

// TestDeployApplyExecuteKeepsVerifiedWhenContainerReadFails：读不到容器状态不降级。
//
// 一个已经落地的部署在审计里被标成说不清，比标错更糟：人会去查一件没坏的事。
func TestDeployApplyExecuteKeepsVerifiedWhenContainerReadFails(t *testing.T) {
	src := &fakeDeploySource{applyOutcome: deploy.ApplyOutcome{
		Report:      deploy.ApplyReport{DeploymentID: deploy.DeploymentIDForName("webapp"), UnitName: "upanel-deploy-webapp.service", Enabled: true, Active: true},
		ProjectName: "webapp",
	}}
	projects := fakeProjects{response: containers.ProjectsResponse{Available: false, Notice: "docker 不可用"}}
	reg := newDeployTestRegistry(t, src, projects)
	action, _ := reg.Action(deploy.ActionApply)
	params, _ := action.Decode(json.RawMessage(`{"definition":{"name":"webapp"}}`))
	result, err := action.Execute(context.Background(), ExecuteInput{Params: params})
	if err != nil {
		t.Fatalf("执行失败：%v", err)
	}
	if result.Outcome != OutcomeVerified {
		t.Fatalf("outcome = %q，读不到容器状态时不该降级", result.Outcome)
	}
	if result.ObservedState["containers_error"] != "docker 不可用" {
		t.Fatalf("容器错误没有记下来：%v", result.ObservedState)
	}
}

// TestDeployApplyExecuteRejectionCarriesReason：执行期拒绝要带一句人话。
func TestDeployApplyExecuteRejectionCarriesReason(t *testing.T) {
	src := &fakeDeploySource{applyExecErr: &deploy.ServiceError{
		Code: "stale", Message: "预检时磁盘上是 A，现在是 B",
	}}
	reg := newDeployTestRegistry(t, src, nil)
	action, _ := reg.Action(deploy.ActionApply)
	params, _ := action.Decode(json.RawMessage(`{"definition":{"name":"webapp"}}`))
	_, err := action.Execute(context.Background(), ExecuteInput{Params: params})
	if err == nil {
		t.Fatal("执行期拒绝必须变成错误")
	}
	var reasoned FailureReasoner
	if !errors.As(err, &reasoned) {
		t.Fatalf("拒绝要带 FailureReasoner：%v", err)
	}
	if !strings.Contains(reasoned.FailureReason(), "stale") ||
		!strings.Contains(reasoned.FailureReason(), "现在是 B") {
		t.Fatalf("原因 = %q", reasoned.FailureReason())
	}
}

// TestDeployRemovePlanAndExecute：删除的预检与结果。
func TestDeployRemovePlanAndExecute(t *testing.T) {
	id := deploy.DeploymentIDForName("webapp")
	src := &fakeDeploySource{
		remove: deploy.RemoveAssessment{
			DeploymentID: id, ProjectName: "webapp", UnitName: "upanel-deploy-webapp.service",
			ConfigDigest: "sha256:" + strings.Repeat("d", 64), StateVersion: "sha256:" + strings.Repeat("d", 64),
			Allowed:  true,
			Warnings: []deploy.Notice{{Code: "data_kept", Message: "数据卷与业务目录保留"}},
		},
		removeOut: deploy.RemoveOutcome{
			Report: deploy.RemoveReport{DeploymentID: id, UnitName: "upanel-deploy-webapp.service",
				Stopped: true, UnitRemoved: true, ConfigRemoved: true},
			ProjectName: "webapp",
		},
	}
	reg := newDeployTestRegistry(t, src, nil)
	action, ok := reg.Action(deploy.ActionRemove)
	if !ok {
		t.Fatal("deployment.remove 没有登记")
	}
	params, err := action.Decode(json.RawMessage(`{"deployment_id":"` + id + `","version":"v1"}`))
	if err != nil {
		t.Fatalf("解码失败：%v", err)
	}
	plan, err := action.Plan(context.Background(), PlanInput{Params: params})
	if err != nil {
		t.Fatalf("预检失败：%v", err)
	}
	if !plan.Allowed {
		t.Fatalf("应允许删除：%+v", plan.Blockers)
	}
	if got := strings.Join(plan.Locks, ","); got != deploy.LockName("webapp") {
		t.Fatalf("删除只该锁 compose 项目，实际 %q", got)
	}
	if len(plan.Warnings) != 1 || plan.Warnings[0].Code != "data_kept" {
		t.Fatalf("删除必须说明数据保留：%+v", plan.Warnings)
	}
	result, err := action.Execute(context.Background(), ExecuteInput{
		Params: params, Target: Target{StateVersion: "sha256:" + strings.Repeat("d", 64)},
	})
	if err != nil {
		t.Fatalf("执行失败：%v", err)
	}
	if result.Outcome != OutcomeVerified {
		t.Fatalf("outcome = %q", result.Outcome)
	}
	if result.ObservedState["data_kept"] != true {
		t.Fatalf("结果里必须写着数据保留：%v", result.ObservedState)
	}
	if result.ObservedState["unit_removed"] != true || result.ObservedState["config_removed"] != true {
		t.Fatalf("删除读数 = %v", result.ObservedState)
	}
}

// TestDeployActionsAreExecutable：两条动作都必须进得了 capabilities。
//
// 只登记预检、没有执行器的动作不出现在 supported_actions 里（那是能力撒谎的反面），
// 而部署这两条是要真的能被执行的。
func TestDeployActionsAreExecutable(t *testing.T) {
	reg := newDeployTestRegistry(t, &fakeDeploySource{}, nil)
	names := reg.ExecutableActionNames()
	want := map[string]bool{deploy.ActionApply: false, deploy.ActionRemove: false}
	for _, name := range names {
		if _, ok := want[name]; ok {
			want[name] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Fatalf("%s 不在可执行动作表里：%v", name, names)
		}
	}
}

// TestDeployApplyUpdateRefusalIsAPlanNotAnError 确认“这一版不做更新”是**判决**，不是 500。
//
// `deploy.AssessApply` 对非 null 的 `deployment_id` 返回 ServiceError，翻译层把它变成
// 一条 blocker。这条计划还必须能过 `validatePlanResult`：漏掉 NormalizedParams 会让一次
// 正常的拒绝变成“预检结果不合法”的实现错误，用户拿到 500 而不是那句“换定义属于
// deployment.update”——真机 P15.2 验收 §7a 撞到的就是它。
func TestDeployApplyUpdateRefusalIsAPlanNotAnError(t *testing.T) {
	src := &fakeDeploySource{applyErr: &deploy.ServiceError{
		Code:    "update_not_implemented",
		Message: "这一版只创建新部署；换定义属于 deployment.update（P15.3）",
	}}
	reg := newDeployTestRegistry(t, src, nil)
	action, ok := reg.Action(deploy.ActionApply)
	if !ok {
		t.Fatal("deployment.apply 没有登记")
	}
	params, err := action.Decode(json.RawMessage(
		`{"deployment_id":"dep_0123456789abcdef0123456789abcdef","version":"","definition":{"name":"webapp"}}`))
	if err != nil {
		t.Fatalf("解码失败：%v", err)
	}
	plan, err := action.Plan(context.Background(), PlanInput{Params: params})
	if err != nil {
		t.Fatalf("判决不该变成错误：%v", err)
	}
	if plan.Allowed {
		t.Fatal("拒绝的计划不该允许执行")
	}
	if len(plan.Blockers) != 1 || plan.Blockers[0].Code != "update_not_implemented" {
		t.Fatalf("阻塞项 = %+v", plan.Blockers)
	}
	if plan.NormalizedParams == nil {
		t.Fatal("被拒的计划也必须带规范化参数：少了它，预检校验会判“计划不合法”")
	}
	if err := validatePlanResult(plan); err != nil {
		t.Fatalf("这条计划必须能通过预检校验：%v", err)
	}
}

// TestDeployRemoveRefusalIsAPlanNotAnError 是同一个形状：删除的判决也走 Plan。
func TestDeployRemoveRefusalIsAPlanNotAnError(t *testing.T) {
	src := &fakeDeploySource{removeErr: &deploy.ServiceError{
		Code:    "blocked",
		Message: "找不到这份部署",
	}}
	reg := newDeployTestRegistry(t, src, nil)
	action, ok := reg.Action(deploy.ActionRemove)
	if !ok {
		t.Fatal("deployment.remove 没有登记")
	}
	params, err := action.Decode(json.RawMessage(`{"deployment_id":"dep_0123456789abcdef0123456789abcdef","version":""}`))
	if err != nil {
		t.Fatalf("解码失败：%v", err)
	}
	plan, err := action.Plan(context.Background(), PlanInput{Params: params})
	if err != nil {
		t.Fatalf("判决不该变成错误：%v", err)
	}
	if plan.Allowed || len(plan.Blockers) != 1 {
		t.Fatalf("计划 = %+v", plan)
	}
	if err := validatePlanResult(plan); err != nil {
		t.Fatalf("这条计划必须能通过预检校验：%v", err)
	}
}

// ---- deployment.update（P15.3） ----

// deployUpdateParams 造一份 UpdateParams。
func deployUpdateParams(t *testing.T) deploy.UpdateParams {
	t.Helper()
	policy := "pol_" + strings.Repeat("1", 32)
	return deploy.UpdateParams{
		DeploymentID:   deploy.DeploymentIDForName("webapp"),
		Version:        "sha256:" + strings.Repeat("a", 64),
		ImageReference: "nginx:1.28",
		BackupPolicyID: &policy,
	}
}

// TestDeployUpdatePlanCarriesAssessment：预检结论原样进计划，且说清这次会做什么。
func TestDeployUpdatePlanCarriesAssessment(t *testing.T) {
	cur := "sha256:" + strings.Repeat("a", 64)
	next := "sha256:" + strings.Repeat("b", 64)
	policy := "pol_" + strings.Repeat("1", 32)
	src := &fakeDeploySource{update: deploy.UpdateAssessment{
		DeploymentID:        deploy.DeploymentIDForName("webapp"),
		ProjectName:         "webapp",
		UnitName:            "upanel-deploy-webapp.service",
		CurrentImage:        "nginx:1.27",
		TargetImage:         "nginx:1.28",
		CurrentImageDigest:  "sha256:" + strings.Repeat("9", 64),
		CurrentConfigDigest: cur,
		ConfigDigest:        next,
		ConfigChanged:       true,
		StateVersion:        cur,
		BackupPolicyID:      policy,
		BackupRequired:      true,
		HealthCheckEnabled:  true,
		Allowed:             true,
		Warnings:            []deploy.Notice{{Code: "backup_before_update", Message: "先备份"}},
		Claims:              []deploy.Claim{{Address: "127.0.0.1", Port: 8080, Protocol: deploy.ProtocolTCP}},
		BackupLocks:         []string{backup.PolicyLockName(policy), backup.LockName("repo_1")},
		VolumeMountPoints:   []string{"/srv/upanel/volumes/vol_media"},
	}}
	reg := newDeployTestRegistry(t, src, nil)
	action, ok := reg.Action(deploy.ActionUpdate)
	if !ok {
		t.Fatal("deployment.update 没有登记")
	}
	params, err := action.Decode(json.RawMessage(`{"deployment_id":"dep_x","version":"v1","image_reference":" nginx:1.28 "}`))
	if err != nil {
		t.Fatalf("解码失败：%v", err)
	}
	plan, err := action.Plan(context.Background(), PlanInput{Params: params})
	if err != nil {
		t.Fatalf("预检失败：%v", err)
	}
	if err := validatePlanResult(plan); err != nil {
		t.Fatalf("计划必须能通过预检校验：%v", err)
	}
	if plan.Target.StateVersion != cur {
		t.Fatalf("复核令牌 = %q，期望盘上配置的摘要 %q", plan.Target.StateVersion, cur)
	}
	if want := "deployment:" + deploy.DeploymentIDForName("webapp"); plan.Target.IdentityFingerprint != want {
		t.Fatalf("身份指纹 = %q，期望 %q", plan.Target.IdentityFingerprint, want)
	}
	if !plan.Allowed || len(plan.Warnings) != 1 {
		t.Fatalf("计划 = %+v", plan)
	}
	kinds := map[string]bool{}
	for _, impact := range plan.Impacts {
		kinds[impact.Kind] = true
	}
	for _, want := range []string{"backup_before_update", "image_pull", "unit_restart",
		"health_probe_rollback", "deployment_record"} {
		if !kinds[want] {
			t.Fatalf("计划里缺影响项 %s：%+v", want, plan.Impacts)
		}
	}
	locks := strings.Join(plan.Locks, ",")
	for _, wantLock := range []string{deploy.LockName("webapp"), "port:tcp/127.0.0.1/8080",
		backup.PolicyLockName(policy)} {
		if !strings.Contains(locks, wantLock) {
			t.Fatalf("锁里缺 %s：%s", wantLock, locks)
		}
	}
}

// TestDeployUpdatePlanRefusalIsAPlanNotAnError：没有部署记录这类判决也是计划，不是 500。
func TestDeployUpdatePlanRefusalIsAPlanNotAnError(t *testing.T) {
	src := &fakeDeploySource{updateErr: &deploy.ServiceError{
		Code: "no_definition_record", Message: "这份部署没有定义记录，无法安全更新",
	}}
	reg := newDeployTestRegistry(t, src, nil)
	action, _ := reg.Action(deploy.ActionUpdate)
	params, err := action.Decode(json.RawMessage(
		`{"deployment_id":"dep_0123456789abcdef0123456789abcdef","version":"v1","image_reference":"nginx:1.28"}`))
	if err != nil {
		t.Fatalf("解码失败：%v", err)
	}
	plan, err := action.Plan(context.Background(), PlanInput{Params: params})
	if err != nil {
		t.Fatalf("判决不该变成错误：%v", err)
	}
	if plan.Allowed || len(plan.Blockers) != 1 || plan.Blockers[0].Code != "no_definition_record" {
		t.Fatalf("计划 = %+v", plan)
	}
	if err := validatePlanResult(plan); err != nil {
		t.Fatalf("这条计划必须能通过预检校验：%v", err)
	}
}

// TestDeployUpdateExecuteReportsReadback：执行成功后读回值要能回答“换成了什么、备过没有、
// 探通了没有”。
func TestDeployUpdateExecuteReportsReadback(t *testing.T) {
	cur := "sha256:" + strings.Repeat("a", 64)
	next := "sha256:" + strings.Repeat("b", 64)
	status := 200
	src := &fakeDeploySource{updateOut: deploy.UpdateOutcome{
		Report: deploy.UpdateReport{
			DeploymentID:         deploy.DeploymentIDForName("webapp"),
			ProjectName:          "webapp",
			UnitName:             "upanel-deploy-webapp.service",
			ConfigPath:           "/etc/upanel/deploy/webapp.compose.yaml",
			PreviousConfigDigest: cur,
			ConfigDigest:         next,
			Image:                deploy.ImageRecord{Requested: "nginx:1.28", Digest: "sha256:" + strings.Repeat("7", 64)},
			ImageDigestKnown:     true,
			UnitStopped:          true,
			Backup:               deploy.BackupOutcome{PolicyID: "pol_1", RepositoryID: "repo_1", SnapshotID: "snap_abc", Verified: true},
			Health:               deploy.HealthProbe{Checked: true, Healthy: true, Attempts: 2, StatusCode: &status, URL: "http://127.0.0.1:8080/"},
			Enabled:              true,
			Active:               true,
			Restarted:            true,
		},
		ProjectName: "webapp",
		Warnings:    []string{"读不到镜像 digest 的次要读数"},
	}}
	projects := fakeProjects{response: containers.ProjectsResponse{Available: true, Projects: []containers.Project{
		{ID: deploy.DeploymentIDForName("webapp"), Name: "webapp", Total: 1, Running: 1},
	}}}
	reg := newDeployTestRegistry(t, src, projects)
	action, _ := reg.Action(deploy.ActionUpdate)
	params, err := action.Decode(json.RawMessage(
		`{"deployment_id":"dep_x","version":"sha256:` + strings.Repeat("a", 64) + `","image_reference":"nginx:1.28"}`))
	if err != nil {
		t.Fatalf("解码失败：%v", err)
	}
	result, err := action.Execute(context.Background(), ExecuteInput{
		Params: params, OperationID: "op-update-1",
		Target: Target{StateVersion: "sha256:" + strings.Repeat("a", 64)},
	})
	if err != nil {
		t.Fatalf("执行失败：%v", err)
	}
	if src.expectState != "sha256:"+strings.Repeat("a", 64) {
		t.Fatalf("传给部署端的复核令牌 = %q", src.expectState)
	}
	if src.operationID != "op-update-1" {
		t.Fatalf("备份要用操作标识命名临时文件，实际 %q", src.operationID)
	}
	if result.Outcome != OutcomeVerified {
		t.Fatalf("outcome = %q", result.Outcome)
	}
	observed := result.ObservedState
	checks := map[string]any{
		"previous_config_digest": cur,
		"config_digest":          next,
		"config_changed":         true,
		"image":                  "nginx:1.28",
		"image_digest":           "sha256:" + strings.Repeat("7", 64),
		"image_digest_known":     true,
		"unit_restarted":         true,
		"unit_active":            true,
		"backup_snapshot_id":     "snap_abc",
		"backup_verified":        true,
		"health_checked":         true,
		"health_ok":              true,
		"health_status_code":     200,
		"rolled_back":            false,
		"containers_total":       1,
	}
	for key, want := range checks {
		if got := observed[key]; got != want {
			t.Fatalf("observed[%s] = %v，期望 %v（全部：%v）", key, got, want, observed)
		}
	}
	if _, ok := observed["warnings"]; !ok {
		t.Fatalf("执行期告警必须留下：%v", observed)
	}
	if result.Outcome != OutcomeVerified || result.VerifiedAt.IsZero() {
		t.Fatalf("verified 必须带核验时间：%+v", result)
	}
}

// TestDeployUpdateExecuteRolledBackIsNotAFailure：回切成功记 rolled_back 并保留读回值。
//
// 按失败记会把 result 丢掉（失败任务的结果是空对象），于是审计里只剩一句话——连现在
// 跑的是哪一版都看不出来。而这里最该被看见的恰恰是“退回了哪个摘要”。
func TestDeployUpdateExecuteRolledBackIsNotAFailure(t *testing.T) {
	cur := "sha256:" + strings.Repeat("a", 64)
	status := 200
	src := &fakeDeploySource{
		updateExecErr: &deploy.ServiceError{
			Code: deploy.CodeHealthFailedRolledBack,
			Message: "新版本健康探测失败（连续 5 次连不上）；已回切到上一版配置并重新拉起。" +
				"数据没有回切：这不是一次无损回滚",
		},
		updateOut: deploy.UpdateOutcome{
			Report: deploy.UpdateReport{
				DeploymentID:         deploy.DeploymentIDForName("webapp"),
				ProjectName:          "webapp",
				UnitName:             "upanel-deploy-webapp.service",
				PreviousConfigDigest: cur,
				ConfigDigest:         "sha256:" + strings.Repeat("b", 64),
				Image:                deploy.ImageRecord{Requested: "nginx:1.28"},
				RolledBack:           true,
				RollbackHealth:       deploy.HealthProbe{Checked: true, Healthy: true, StatusCode: &status},
				RollbackVerified:     true,
				Enabled:              true,
				Active:               true,
			},
			ProjectName: "webapp",
		},
	}
	reg := newDeployTestRegistry(t, src, nil)
	action, _ := reg.Action(deploy.ActionUpdate)
	params, err := action.Decode(json.RawMessage(`{"deployment_id":"dep_x","version":"v1","image_reference":"nginx:1.28"}`))
	if err != nil {
		t.Fatalf("解码失败：%v", err)
	}
	result, err := action.Execute(context.Background(), ExecuteInput{Params: params})
	if err != nil {
		t.Fatalf("回切不是执行错误：%v", err)
	}
	if result.Outcome != OutcomeRolledBack {
		t.Fatalf("outcome = %q，期望 rolled_back", result.Outcome)
	}
	if !strings.Contains(result.RecoveryHint, "数据没有回切") {
		t.Fatalf("回切提示必须说清数据不在其中：%q", result.RecoveryHint)
	}
	if result.ObservedState["rolled_back"] != true || result.ObservedState["rollback_health_ok"] != true {
		t.Fatalf("回切读数 = %v", result.ObservedState)
	}
	if result.ObservedState["previous_config_digest"] != cur {
		t.Fatalf("退回的是哪一版必须留下：%v", result.ObservedState)
	}
}

// TestDeployUpdateExecuteRollbackFailureIsAnError：回切也失败时必须是一声警铃，不是一条结果。
func TestDeployUpdateExecuteRollbackFailureIsAnError(t *testing.T) {
	src := &fakeDeploySource{updateExecErr: &deploy.ServiceError{
		Code:    "health_failed_rollback_failed",
		Message: "新版本健康探测失败；回切也失败了：单元起不来",
	}}
	reg := newDeployTestRegistry(t, src, nil)
	action, _ := reg.Action(deploy.ActionUpdate)
	params, _ := action.Decode(json.RawMessage(`{"deployment_id":"dep_x","version":"v1","image_reference":"nginx:1.28"}`))
	_, err := action.Execute(context.Background(), ExecuteInput{Params: params})
	if err == nil {
		t.Fatal("回切失败必须变成错误")
	}
	var reasoned FailureReasoner
	if !errors.As(err, &reasoned) {
		t.Fatalf("失败要带 FailureReasoner：%v", err)
	}
	if !strings.Contains(reasoned.FailureReason(), "回切也失败") {
		t.Fatalf("原因 = %q", reasoned.FailureReason())
	}
}

// TestDeployUpdateExecutePartialWhenProjectEmpty：换完之后项目下没有容器 → partial。
func TestDeployUpdateExecutePartialWhenProjectEmpty(t *testing.T) {
	src := &fakeDeploySource{updateOut: deploy.UpdateOutcome{
		Report: deploy.UpdateReport{
			DeploymentID: deploy.DeploymentIDForName("webapp"),
			ProjectName:  "webapp",
			UnitName:     "upanel-deploy-webapp.service",
			Image:        deploy.ImageRecord{Requested: "nginx:1.28"},
			Enabled:      true,
			Active:       true,
		},
		ProjectName: "webapp",
	}}
	projects := fakeProjects{response: containers.ProjectsResponse{Available: true}}
	reg := newDeployTestRegistry(t, src, projects)
	action, _ := reg.Action(deploy.ActionUpdate)
	params, _ := action.Decode(json.RawMessage(`{"deployment_id":"dep_x","version":"v1","image_reference":"nginx:1.28"}`))
	result, err := action.Execute(context.Background(), ExecuteInput{Params: params})
	if err != nil {
		t.Fatalf("执行失败：%v", err)
	}
	if result.Outcome != OutcomePartial {
		t.Fatalf("outcome = %q，期望 partial", result.Outcome)
	}
	if !strings.Contains(result.RecoveryHint, "docker compose -p webapp ps") {
		t.Fatalf("恢复提示 = %q", result.RecoveryHint)
	}
}
