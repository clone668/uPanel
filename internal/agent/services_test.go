package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"upanel/internal/agentstore"
	"upanel/internal/services"
)

// fakeServices 是 agent 层的假采集器：这一层要验证的是参数校验、计划映射与
// 结果映射，不需要真的有一台 systemd 主机。services 包自己的测试负责真实语义。
type fakeServices struct {
	snapshot services.Snapshot

	unit      services.Unit
	unitFound bool
	unitErr   error

	assessment services.Assessment
	assessErr  error

	result     services.ControlResult
	executeErr error

	// 记录调用参数，验证 agent 层没有偷偷改写入参。
	assessed []string
	executed []string
	progress []string
}

func (f *fakeServices) Snapshot() services.Snapshot { return f.snapshot }

func (f *fakeServices) Unit(_ context.Context, _ string) (services.Unit, bool, error) {
	return f.unit, f.unitFound, f.unitErr
}

func (f *fakeServices) AssessControl(_ context.Context, unit, op string) (services.Assessment, error) {
	f.assessed = append(f.assessed, unit+"|"+op)
	return f.assessment, f.assessErr
}

func (f *fakeServices) ExecuteControl(_ context.Context, unit, op string, progress func(string, map[string]any)) (services.ControlResult, error) {
	f.executed = append(f.executed, unit+"|"+op)
	if progress != nil {
		progress("systemctl_"+op, map[string]any{"unit": unit})
		f.progress = append(f.progress, "systemctl_"+op)
		progress("verified", map[string]any{"unit": unit})
		f.progress = append(f.progress, "verified")
	}
	return f.result, f.executeErr
}

// newFakeServices 返回一份“默认可以执行”的假采集器：预检要求目标指纹、
// 状态版本、影响与锁齐备，零值计划会被如实判为不合法，所以默认值必须完整。
func newFakeServices() *fakeServices {
	u := sampleUnit()
	return &fakeServices{
		snapshot: sampleSnapshot(),
		unit:     u,
		assessment: services.Assessment{
			Found:        true,
			Unit:         u,
			DisplayName:  "cron.service",
			Identity:     services.ServiceIdentity(u.Unit),
			StateVersion: services.StateVersion(u),
			Allowed:      true,
			Impacts:      []services.Impact{{Kind: services.ImpactNone, Description: "只影响目标单元自身"}},
			Locks:        []string{"service:" + u.Unit},
		},
	}
}

// newServicesRegistry 只登记 P05.1 的服务查询与动作。
func newServicesRegistry(t *testing.T, src servicesSource) *Registry {
	t.Helper()
	reg := NewRegistry()
	if err := reg.RegisterQuery(newServicesListQuery(src)); err != nil {
		t.Fatalf("登记 services.list 失败: %v", err)
	}
	if err := reg.RegisterQuery(newServicesUnitQuery(src)); err != nil {
		t.Fatalf("登记 services.unit 失败: %v", err)
	}
	if err := reg.RegisterAction(newServiceControlAction(src)); err != nil {
		t.Fatalf("登记 service.control 失败: %v", err)
	}
	return reg
}

func sampleUnit() services.Unit {
	pid := int64(784)
	mem := "483328"
	started := time.Date(2026, 10, 21, 0, 16, 5, 0, time.UTC)
	return services.Unit{
		Unit:          "cron.service",
		Description:   "Regular background program processing daemon",
		LoadState:     services.LoadLoaded,
		ActiveState:   services.ActiveActive,
		SubState:      "running",
		UnitFileState: "enabled",
		Enablement:    services.EnablementEnabled,
		MainPID:       &pid,
		StartedAt:     &started,
		MemoryBytes:   &mem,
		FragmentPath:  "/usr/lib/systemd/system/cron.service",
		Protection:    services.Protection{Reasons: []string{}},
		Capabilities:  []string{services.OpStop, services.OpRestart, services.OpDisable},
		CheckedAt:     started,
	}
}

func sampleSnapshot() services.Snapshot {
	return services.Snapshot{
		Status:          services.StatusOK,
		CollectedAt:     time.Date(2026, 10, 21, 0, 20, 0, 0, time.UTC),
		IntervalMS:      10000,
		SnapshotVersion: "sha256:abc",
		Units:           []services.Unit{sampleUnit()},
	}
}

// TestServicesListQueryReturnsSnapshot 查 services.list：返回采集快照的原样字段。
func TestServicesListQueryReturnsSnapshot(t *testing.T) {
	src := newFakeServices()
	srv := newTestServerWithRegistry(t, newServicesRegistry(t, src))

	rec := postQuery(t, srv, `{"query":"services.list"}`, testWebUID)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（%s）", rec.Code, rec.Body.String())
	}
	var body struct {
		Data services.Snapshot `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if body.Data.Status != services.StatusOK || len(body.Data.Units) != 1 {
		t.Fatalf("快照内容不对: %+v", body.Data)
	}
	u := body.Data.Units[0]
	if u.Unit != "cron.service" || u.MainPID == nil || *u.MainPID != 784 {
		t.Errorf("单元字段丢失: %+v", u)
	}
	if strings.Join(u.Capabilities, ",") != "stop,restart,disable" {
		t.Errorf("能力字段 = %v", u.Capabilities)
	}
}

// TestServicesListQueryRejectsParams services.list 不接受任何参数：
// 这条是“禁止万能命令执行”的一部分——参数里塞命令不会被当成筛选条件。
func TestServicesListQueryRejectsParams(t *testing.T) {
	src := newFakeServices()
	srv := newTestServerWithRegistry(t, newServicesRegistry(t, src))

	rec := postQuery(t, srv, `{"query":"services.list","params":{"command":"systemctl stop sshd"}}`, testWebUID)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("带参数的状态码 = %d，期望 400（%s）", rec.Code, rec.Body.String())
	}
}

// TestServicesUnitQuery 现场读取单个单元，区分存在与不存在。
func TestServicesUnitQuery(t *testing.T) {
	src := newFakeServices()
	src.unitFound = true
	srv := newTestServerWithRegistry(t, newServicesRegistry(t, src))

	rec := postQuery(t, srv, `{"query":"services.unit","params":{"unit":"cron.service"}}`, testWebUID)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d（%s）", rec.Code, rec.Body.String())
	}
	var body struct {
		Data services.UnitResponse `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if !body.Data.Found || body.Data.Unit == nil || body.Data.Unit.Unit != "cron.service" {
		t.Fatalf("单元响应不对: %+v", body.Data)
	}

	// 不存在：found=false 且 unit 为 null，不是错误。
	src.unitFound = false
	rec = postQuery(t, srv, `{"query":"services.unit","params":{"unit":"ghost.service"}}`, testWebUID)
	if rec.Code != http.StatusOK {
		t.Fatalf("不存在单元的状态码 = %d（%s）", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"found":false`) {
		t.Errorf("不存在时 found 应为 false: %s", rec.Body.String())
	}
}

// TestServicesUnitQueryRejectsBadNames 非法单元名在解码阶段就被拒。
func TestServicesUnitQueryRejectsBadNames(t *testing.T) {
	src := newFakeServices()
	src.unitFound = true
	srv := newTestServerWithRegistry(t, newServicesRegistry(t, src))

	for _, unit := range []string{
		"cron",                // 不是 .service
		"cron.service;reboot", // shell 元字符
		"../../etc/passwd",    // 路径穿越
		"--now.service",       // 以 - 开头
		"cron.socket",         // 非 service 类型
		strings.Repeat("a", 300) + ".service",
	} {
		payload := fmt.Sprintf(`{"query":"services.unit","params":{"unit":%q}}`, unit)
		rec := postQuery(t, srv, payload, testWebUID)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%q 的状态码 = %d，期望 400（%s）", unit, rec.Code, rec.Body.String())
		}
	}
}

// TestServiceControlDecode 参数只接受 unit + operation，没有第三个字段。
func TestServiceControlDecode(t *testing.T) {
	src := newFakeServices()
	srv, _ := newPreflightServer(t, newServicesRegistry(t, src))

	cases := []struct {
		name   string
		params string
		wantOK bool
	}{
		{"正常", `{"unit":"cron.service","operation":"restart"}`, true},
		{"大小写与空格", `{"unit":" cron.service ","operation":" RESTART "}`, true},
		{"多余字段", `{"unit":"cron.service","operation":"restart","force":true}`, false},
		{"自由命令", `{"unit":"cron.service","command":"systemctl stop sshd"}`, false},
		{"未知操作", `{"unit":"cron.service","operation":"mask"}`, false},
		{"缺操作", `{"unit":"cron.service"}`, false},
		{"缺单元", `{"operation":"restart"}`, false},
		{"非法单元名", `{"unit":"cron.service --now","operation":"restart"}`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := postPreflight(t, srv, testWebUID, preflightBody("service.control", c.params))
			ok := rec.Code == http.StatusCreated
			if ok != c.wantOK {
				t.Fatalf("状态码 = %d，期望成功=%v（%s）", rec.Code, c.wantOK, rec.Body.String())
			}
		})
	}
}

// TestServiceControlPreflightMapsAssessment 预检把评估结论映射成计划。
func TestServiceControlPreflightMapsAssessment(t *testing.T) {
	src := &fakeServices{assessment: services.Assessment{
		Found:        true,
		Unit:         sampleUnit(),
		DisplayName:  "cron.service（Regular background program processing daemon）",
		Identity:     services.ServiceIdentity("cron.service"),
		StateVersion: services.StateVersion(sampleUnit()),
		Allowed:      false,
		Blockers:     []services.Reason{{Code: services.CodeStateConflict, Message: "服务当前已经是 active，不需要再启动"}},
		Warnings:     []services.Reason{{Code: services.WarningInterruption, Message: "操作期间该服务提供的能力会中断"}},
		Impacts:      []services.Impact{{Kind: services.ImpactNone, Description: "不改变其他服务"}},
		Locks:        []string{"service:cron.service"},
	}}
	srv, _ := newPreflightServer(t, newServicesRegistry(t, src))

	plan := mustPreflight(t, srv, services.ActionControl, `{"unit":"cron.service","operation":"start"}`)
	if plan.Allowed {
		t.Fatal("带阻塞项的预检必须 allowed=false")
	}
	if len(plan.Blockers) != 1 || plan.Blockers[0].Code != services.CodeStateConflict {
		t.Fatalf("阻塞项 = %+v", plan.Blockers)
	}
	if len(plan.Warnings) != 1 || plan.Warnings[0].Code != services.WarningInterruption {
		t.Fatalf("告警项 = %+v", plan.Warnings)
	}
	if plan.Target.IdentityFingerprint != services.ServiceIdentity("cron.service") {
		t.Errorf("身份指纹 = %q", plan.Target.IdentityFingerprint)
	}
	if plan.Target.StateVersion == "" || plan.Target.DisplayName == "" {
		t.Errorf("目标信息不完整: %+v", plan.Target)
	}
	if len(plan.Locks) != 1 || plan.Locks[0] != "service:cron.service" {
		t.Errorf("锁 = %v", plan.Locks)
	}
	// 策略由服务端决定：不带再认证、不带确认短语、不可取消。
	if plan.RequiresReauth || plan.RequiresPhrase || plan.ConfirmationPhrase != "" || plan.Cancellable {
		t.Errorf("策略不符合 CONTRACTS.md：%+v", plan)
	}
	// 预检必须只读：只有评估，没有执行。
	if len(src.executed) != 0 {
		t.Errorf("预检阶段调用了执行: %v", src.executed)
	}
	// 规范化参数：trim + 小写。
	var norm struct {
		Unit      string `json:"unit"`
		Operation string `json:"operation"`
	}
	if err := json.Unmarshal(plan.NormalizedParams, &norm); err != nil {
		t.Fatalf("规范化参数不是合法 JSON: %v", err)
	}
	if norm.Unit != "cron.service" || norm.Operation != "start" {
		t.Errorf("规范化参数 = %+v", norm)
	}
}

// TestServiceControlExecutesAndVerifies 预检通过 → 提交任务 → 执行并读回验证。
func TestServiceControlExecutesAndVerifies(t *testing.T) {
	after := sampleUnit()
	after.ActiveState = services.ActiveInactive
	after.SubState = "dead"
	after.MainPID = nil
	before := sampleUnit()

	src := &fakeServices{assessment: services.Assessment{
		Found:        true,
		Unit:         before,
		DisplayName:  "cron.service",
		Identity:     services.ServiceIdentity("cron.service"),
		StateVersion: services.StateVersion(before),
		Allowed:      true,
		Impacts:      []services.Impact{{Kind: services.ImpactNone, Description: "只影响目标单元自身"}},
		Locks:        []string{"service:cron.service"},
	}}
	// 执行成功后状态变成 inactive：结果里必须带上读回的 After。
	src.result = services.ControlResult{
		Op:          services.OpStop,
		Unit:        "cron.service",
		Before:      before,
		After:       after,
		ExitCode:    0,
		Verified:    true,
		Expectation: "服务处于 inactive",
		Duration:    250 * time.Millisecond,
	}
	srv, _, _ := newOperationServer(t, newServicesRegistry(t, src))

	plan := mustPreflight(t, srv, services.ActionControl, `{"unit":"cron.service","operation":"stop"}`)
	id := opID("svc-stop")
	if rec := submitOperation(t, srv, operationSubmitBody(id, plan.PreflightRef, "", testSessionHash, "")); rec.Code != http.StatusCreated {
		t.Fatalf("提交状态码 = %d（%s）", rec.Code, rec.Body.String())
	}
	final := waitForStage(t, srv, id, agentstore.StageSucceeded, agentstore.StageFailed)
	if final.Stage != agentstore.StageSucceeded {
		t.Fatalf("任务阶段 = %q（%s）", final.Stage, final.Error)
	}
	if strings.Join(src.executed, ",") != "cron.service|stop" {
		t.Errorf("执行参数 = %v（预检的规范化参数必须原样传给执行器）", src.executed)
	}
	if strings.Join(src.progress, ",") != "systemctl_stop,verified" {
		t.Errorf("进度步骤 = %v", src.progress)
	}

	var result struct {
		Outcome       string         `json:"outcome"`
		ObservedState map[string]any `json:"observed_state"`
		Data          map[string]any `json:"data"`
	}
	if err := json.Unmarshal(final.Result, &result); err != nil {
		t.Fatalf("结果不是合法 JSON: %v（%s）", err, final.Result)
	}
	if result.ObservedState["active_state"] != services.ActiveInactive {
		t.Errorf("读回状态 = %v", result.ObservedState["active_state"])
	}
	if result.ObservedState["verified"] != true {
		t.Errorf("verified = %v", result.ObservedState["verified"])
	}
	if result.ObservedState["previous_state"] != services.ActiveActive {
		t.Errorf("previous_state = %v", result.ObservedState["previous_state"])
	}
	if result.Data["unit"] != "cron.service" || result.Data["operation"] != "stop" {
		t.Errorf("data = %+v", result.Data)
	}
}

// TestServiceControlExecutionFailure 执行失败 → 任务 failed（COMMAND_FAILED）。
func TestServiceControlExecutionFailure(t *testing.T) {
	src := &fakeServices{assessment: services.Assessment{
		Found: true, Unit: sampleUnit(), DisplayName: "cron.service",
		Identity: services.ServiceIdentity("cron.service"), StateVersion: services.StateVersion(sampleUnit()),
		Allowed: true, Locks: []string{"service:cron.service"},
	}}
	src.executeErr = fmt.Errorf("services: systemctl stop cron.service 失败（退出状态 1）：Job failed")
	srv, _, _ := newOperationServer(t, newServicesRegistry(t, src))

	plan := mustPreflight(t, srv, services.ActionControl, `{"unit":"cron.service","operation":"stop"}`)
	id := opID("svc-fail")
	if rec := submitOperation(t, srv, operationSubmitBody(id, plan.PreflightRef, "", testSessionHash, "")); rec.Code != http.StatusCreated {
		t.Fatalf("提交状态码 = %d（%s）", rec.Code, rec.Body.String())
	}
	final := waitForStage(t, srv, id, agentstore.StageFailed, agentstore.StageSucceeded)
	if final.Stage != agentstore.StageFailed {
		t.Fatalf("任务阶段 = %q，期望 failed", final.Stage)
	}
	var errBody struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(final.Error, &errBody); err != nil {
		t.Fatalf("错误体不是合法 JSON: %v", err)
	}
	if errBody.Code != "COMMAND_FAILED" {
		t.Errorf("错误代码 = %q", errBody.Code)
	}
	// 命令原文属于内部细节，不进对外错误体（审计里另有记录）。
	if strings.Contains(errBody.Message, "Job failed") {
		t.Errorf("错误信息泄漏了 systemctl 原文: %q", errBody.Message)
	}
}

// TestServiceControlBlockedPreflightCannotExecute 被拒绝的计划提交时会重新复核并拒绝。
func TestServiceControlBlockedPreflightCannotExecute(t *testing.T) {
	protected := sampleUnit()
	protected.Unit = "ssh.service"
	protected.Protection = services.Protection{Protected: true, Category: services.ProtectSSH, Reasons: []string{"SSH 远程登录"}}
	protected.Capabilities = []string{}

	src := &fakeServices{assessment: services.Assessment{
		Found: true, Unit: protected, DisplayName: "ssh.service",
		Identity: services.ServiceIdentity("ssh.service"), StateVersion: services.StateVersion(protected),
		Allowed:  false,
		Blockers: []services.Reason{{Code: services.CodeProtected, Message: "受保护的服务"}},
		Impacts:  []services.Impact{{Kind: services.ImpactNone, Description: "受保护单元不会被改动"}},
		Locks:    []string{"service:ssh.service"},
	}}
	srv, _, _ := newOperationServer(t, newServicesRegistry(t, src))

	plan := mustPreflight(t, srv, services.ActionControl, `{"unit":"ssh.service","operation":"stop"}`)
	if plan.Allowed {
		t.Fatal("受保护单元的预检必须被拒")
	}
	rec := submitOperation(t, srv, operationSubmitBody(opID("svc-protected"), plan.PreflightRef, "", testSessionHash, ""))
	if rec.Code == http.StatusCreated {
		t.Fatal("被拒绝的预检不得被接受为任务")
	}
	if len(src.executed) != 0 {
		t.Errorf("被拒绝的计划不能执行: %v", src.executed)
	}
}

// TestServiceControlRejectsUnknownAction 未登记的动作不存在默认分支。
func TestServiceControlRejectsUnknownAction(t *testing.T) {
	src := newFakeServices()
	srv, _ := newPreflightServer(t, newServicesRegistry(t, src))
	rec := postPreflight(t, srv, testWebUID, preflightBody("service.shell",
		`{"unit":"cron.service","operation":"restart"}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400（%s）", rec.Code, rec.Body.String())
	}
}

// TestServiceControlSurfacesAssessmentError 采集层报错（例如没有 systemctl）时预检失败，
// 对外只给统一文案，内部原因（命令原文/依赖细节）只进日志。
func TestServiceControlSurfacesAssessmentError(t *testing.T) {
	src := newFakeServices()
	src.assessErr = fmt.Errorf("%w: 测试环境没有 systemd", services.ErrSystemctlUnavailable)
	srv, _ := newPreflightServer(t, newServicesRegistry(t, src))
	rec := postPreflight(t, srv, testWebUID, preflightBody(services.ActionControl,
		`{"unit":"cron.service","operation":"restart"}`))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("状态码 = %d，期望 500（%s）", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "预检计算失败") {
		t.Errorf("响应应说明预检失败: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "systemd") {
		t.Errorf("响应泄漏了内部细节: %s", rec.Body.String())
	}
}
