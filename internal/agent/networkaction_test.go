package agent

// P14.2：网络写动作的接线层测试。
//
// 这一层只做三件事——解码、把判决翻成计划、把执行报告翻成结果。所以这里测的也是这三件：
// 参数不许有未知字段、判决**原样**过桥（不加料也不吞）、`rolled_back` 与 `unknown`
// 必须各自活着走出这个包。真正的判断在 internal/network，那边有自己的测试。

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"upanel/internal/network"
)

const testNetworkIface = "ens4"

// fakeNetworkSource 是 NetworkApplySource 的假实现：只记录被调了什么、返回预置的值。
type fakeNetworkSource struct {
	current     network.ConfigState
	assessment  network.Assessment
	assessErr   error
	report      network.WriteReport
	execErr     error
	applyCalls  []network.ApplyParams
	confirmCall []network.ConfirmParams
	rollCall    []network.RollbackParams
	requests    []network.WriteRequest
}

func (f *fakeNetworkSource) Current(context.Context) network.ConfigState { return f.current }

func (f *fakeNetworkSource) Assess(_ context.Context, params network.ApplyParams) (network.Assessment, error) {
	f.applyCalls = append(f.applyCalls, params)
	if f.assessErr != nil {
		return network.Assessment{}, f.assessErr
	}
	return f.assessment, nil
}

func (f *fakeNetworkSource) AssessConfirm(_ context.Context, params network.ConfirmParams) (network.Assessment, error) {
	f.confirmCall = append(f.confirmCall, params)
	if f.assessErr != nil {
		return network.Assessment{}, f.assessErr
	}
	return f.assessment, nil
}

func (f *fakeNetworkSource) AssessRollback(_ context.Context, params network.RollbackParams) (network.Assessment, error) {
	f.rollCall = append(f.rollCall, params)
	if f.assessErr != nil {
		return network.Assessment{}, f.assessErr
	}
	return f.assessment, nil
}

func (f *fakeNetworkSource) ExecuteWrite(_ context.Context, req network.WriteRequest) (network.WriteReport, error) {
	f.requests = append(f.requests, req)
	if f.execErr != nil {
		return network.WriteReport{}, f.execErr
	}
	return f.report, nil
}

func newNetworkRegistry(t *testing.T, src NetworkApplySource) *Registry {
	t.Helper()
	reg := NewRegistry()
	if err := RegisterNetworkWriteActions(reg, src); err != nil {
		t.Fatalf("登记网络写动作失败: %v", err)
	}
	if err := RegisterNetworkConfigQuery(reg, src); err != nil {
		t.Fatalf("登记 %s 失败: %v", NetworkConfigQueryName, err)
	}
	return reg
}

// staticDefinition 是一份合法的静态配置：面板愿意改的那种。
func staticDefinition() *network.ApplyDefinition {
	return &network.ApplyDefinition{Mode: "static", IPv4: &network.IPv4Setting{
		Address: "192.168.1.50", PrefixLength: 24, Gateway: "192.168.1.1",
	}}
}

func decodeApply(t *testing.T, reg *Registry) any {
	t.Helper()
	action, ok := reg.Action(network.ActionApply)
	if !ok {
		t.Fatalf("%s 没有登记", network.ActionApply)
	}
	raw, err := json.Marshal(network.ApplyParams{
		InterfaceID:        testNetworkIface,
		ExpectedConfigHash: "sha256:abc",
		Definition:         staticDefinition(),
	})
	if err != nil {
		t.Fatal(err)
	}
	params, err := action.Decode(raw)
	if err != nil {
		t.Fatalf("正常参数被拒: %v", err)
	}
	return params
}

func TestNetworkWriteRegistryRequiresSource(t *testing.T) {
	if err := RegisterNetworkWriteActions(NewRegistry(), nil); err == nil {
		t.Fatal("没有实现端不该登记成功（点了才 panic 的入口不算登记成功）")
	}
	if err := RegisterNetworkConfigQuery(NewRegistry(), nil); err == nil {
		t.Fatal("没有实现端不该登记 network.config")
	}
	// 未接线时连名字都不该出现：capabilities 不该报告一个永远跑不起来的入口。
	reg := NewRegistry()
	if err := RegisterNetworkWriteActionsIfAvailable(reg, nil); err != nil {
		t.Fatalf("可选登记失败: %v", err)
	}
	if err := RegisterNetworkConfigQueryIfAvailable(reg, nil); err != nil {
		t.Fatalf("可选登记失败: %v", err)
	}
	for _, name := range []string{network.ActionApply, network.ActionConfirm, network.ActionRollback} {
		if _, ok := reg.Action(name); ok {
			t.Fatalf("未接线时不该出现 %s", name)
		}
	}
	if _, ok := reg.Query(NetworkConfigQueryName); ok {
		t.Fatalf("未接线时不该出现 %s", NetworkConfigQueryName)
	}
}

func TestNetworkWriteRegistryNames(t *testing.T) {
	reg := newNetworkRegistry(t, &fakeNetworkSource{})
	for _, name := range []string{network.ActionApply, network.ActionConfirm, network.ActionRollback} {
		if _, ok := reg.Action(name); !ok {
			t.Fatalf("%s 没有登记", name)
		}
		if !contains(reg.ExecutableActionNames(), name) {
			t.Fatalf("%s 应当可执行：%v", name, reg.ExecutableActionNames())
		}
	}
	if _, ok := reg.Query(NetworkConfigQueryName); !ok {
		t.Fatalf("%s 没有登记", NetworkConfigQueryName)
	}
}

// TestNetworkActionPolicies：CONTRACTS §13.6 的策略表。
//
// apply 是唯一能把管理员关在门外的动作，所以它有门槛；confirm 与 rollback 是
// 出事时救命的两步，门槛越高越救不回来，所以它们没有。
func TestNetworkActionPolicies(t *testing.T) {
	reg := newNetworkRegistry(t, &fakeNetworkSource{})

	apply, _ := reg.Action(network.ActionApply)
	policy, err := apply.ResolvePolicy(nil)
	if err != nil {
		t.Fatalf("apply 策略不合法: %v", err)
	}
	if !policy.RequiresReauth {
		t.Fatal("network.apply 必须再认证")
	}
	if !policy.RequiresPhrase || policy.PhraseVerb != "RECONFIGURE" {
		t.Fatalf("network.apply 的短语应是 RECONFIGURE，得到 %q（RequiresPhrase=%v）",
			policy.PhraseVerb, policy.RequiresPhrase)
	}
	if policy.Cancellable {
		t.Fatal("network.apply 不可取消：netplan apply 中途停下比跑完更糟")
	}

	for _, name := range []string{network.ActionConfirm, network.ActionRollback} {
		action, _ := reg.Action(name)
		p, err := action.ResolvePolicy(nil)
		if err != nil {
			t.Fatalf("%s 策略不合法: %v", name, err)
		}
		if p.RequiresReauth || p.RequiresPhrase {
			t.Fatalf("%s 不该有门槛（救命的两步）：%+v", name, p)
		}
		if p.Cancellable {
			t.Fatalf("%s 不可取消", name)
		}
	}
}

func TestNetworkDecodeStrict(t *testing.T) {
	reg := newNetworkRegistry(t, &fakeNetworkSource{})

	apply, _ := reg.Action(network.ActionApply)
	badApply := []string{
		`{"interface_id":"ens4","expected_config_hash":"h","definition":{"mode":"dhcp","ipv4":null},"extra":1}`,
		`{"interface_id":"ens4","expected_config_hash":"h"}`, // 缺 definition
		`{"interface_id":"ens4","expected_config_hash":"h","definition":{"mode":"bogus"}}`,
		`{"interface_id":"ens4","expected_config_hash":"h","definition":{"mode":"static"}}`,
		`{"interface_id":"ens4","expected_config_hash":"h","definition":{"mode":"static","ipv4":{"address":"10.0.0.1","prefix_length":33}}}`,
		// 0.0.0.0/8 与回环即使写进 netplan 也只让机器以更隐蔽的方式坏掉。
		`{"interface_id":"ens4","expected_config_hash":"h","definition":{"mode":"static","ipv4":{"address":"127.0.0.5","prefix_length":8}}}`,
	}
	for _, raw := range badApply {
		if _, err := apply.Decode(json.RawMessage(raw)); !errors.Is(err, ErrParamsInvalid) {
			t.Fatalf("%s 应当被拒: %v", raw, err)
		}
	}

	// mode=dhcp 时 ipv4 必须是 null：空对象和"没写"在语义上不同，面板不接受含糊。
	params := decodeApply(t, reg)
	if got := params.(network.ApplyParams); got.InterfaceID != testNetworkIface ||
		got.Definition == nil || got.Definition.Mode != "static" ||
		got.Definition.IPv4 == nil || got.Definition.IPv4.Address != "192.168.1.50" {
		t.Fatalf("参数解码错：%+v", got)
	}

	confirm, _ := reg.Action(network.ActionConfirm)
	good := `{"interface_id":"ens4","token":"nrb_` + "0123456789abcdef0123456789abcdef" + `","arrived_on":"192.168.1.50"}`
	if _, err := confirm.Decode(json.RawMessage(good)); err != nil {
		t.Fatalf("正常 confirm 参数被拒: %v", err)
	}
	for _, raw := range []string{
		`{"interface_id":"ens4","token":"t","arrived_on":"1.2.3.4","why":"because"}`,
		`{"interface_id":"ens4","token":"t","arrived_on":"1.2.3.4","token_again":"t"}`,
	} {
		if _, err := confirm.Decode(json.RawMessage(raw)); !errors.Is(err, ErrParamsInvalid) {
			t.Fatalf("%s 应当被拒: %v", raw, err)
		}
	}

	rollback, _ := reg.Action(network.ActionRollback)
	if _, err := rollback.Decode(json.RawMessage(`{"interface_id":"ens4","token":"t"}`)); err != nil {
		t.Fatalf("正常 rollback 参数被拒: %v", err)
	}
	if _, err := rollback.Decode(json.RawMessage(`{"interface_id":"ens4","token":"t","force":true}`)); !errors.Is(err, ErrParamsInvalid) {
		t.Fatalf("未知字段应当被拒: %v", err)
	}
}

// TestNetworkPlanMapping：判决原样过桥，一行都不许改。
func TestNetworkPlanMapping(t *testing.T) {
	src := &fakeNetworkSource{}
	src.assessment = network.Assessment{
		Normalized:          network.ApplyParams{InterfaceID: testNetworkIface},
		DisplayName:         "ens4 → 192.168.1.50/24",
		IdentityFingerprint: "iface:ens4:aa:bb",
		StateVersion:        "sha256:current",
		Allowed:             false,
		Blockers:            []network.Notice{{Code: "NETWORK_UNSUPPORTED", Message: "复杂 netplan"}},
		Warnings:            []network.Notice{{Code: "NETWORK_MANAGED_PATH", Message: "管理口"}},
		Impacts:             []network.Impact{{Kind: "connectivity", Description: "当前连接会断"}},
		Locks:               []string{network.NetworkLockName},
	}
	reg := newNetworkRegistry(t, src)
	action, _ := reg.Action(network.ActionApply)

	plan, err := action.Plan(context.Background(), PlanInput{Params: decodeApply(t, reg)})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Allowed {
		t.Fatal("判决说不可改，计划就必须不可改")
	}
	if plan.Target.DisplayName != src.assessment.DisplayName ||
		plan.Target.IdentityFingerprint != src.assessment.IdentityFingerprint ||
		plan.Target.StateVersion != src.assessment.StateVersion {
		t.Fatalf("目标信息没原样过桥：%+v", plan.Target)
	}
	if len(plan.Blockers) != 1 || plan.Blockers[0].Code != "NETWORK_UNSUPPORTED" {
		t.Fatalf("阻塞项不对：%+v", plan.Blockers)
	}
	if len(plan.Warnings) != 1 || plan.Warnings[0].Code != "NETWORK_MANAGED_PATH" {
		t.Fatalf("告警不对：%+v", plan.Warnings)
	}
	if len(plan.Impacts) != 1 || plan.Impacts[0].Kind != "connectivity" {
		t.Fatalf("影响不对：%+v", plan.Impacts)
	}
	if len(plan.Locks) != 1 || plan.Locks[0] != network.NetworkLockName {
		t.Fatalf("锁名不对：%v（改地址与改防火墙共用一把锁）", plan.Locks)
	}
	// 判决层被问到的必须是我们解码出来的那份参数，不是别处重造的。
	if len(src.applyCalls) != 1 || src.applyCalls[0].Definition == nil ||
		src.applyCalls[0].Definition.IPv4.Address != "192.168.1.50" {
		t.Fatalf("判决没拿到参数：%+v", src.applyCalls)
	}
}

// TestNetworkPlanFallsBackWhenNotAllowedWithoutBlockers：漏填 Blockers 的判决不许变成谜语。
func TestNetworkPlanFallsBackWhenNotAllowedWithoutBlockers(t *testing.T) {
	src := &fakeNetworkSource{assessment: network.Assessment{Allowed: false}}
	reg := newNetworkRegistry(t, src)
	action, _ := reg.Action(network.ActionApply)

	plan, err := action.Plan(context.Background(), PlanInput{Params: decodeApply(t, reg)})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Blockers) != 1 || plan.Blockers[0].Code != "NETWORK_UNSUPPORTED" {
		t.Fatalf("兜底阻塞项应是 NETWORK_UNSUPPORTED：%+v", plan.Blockers)
	}
}

// TestNetworkPlanAllowedKeepsEmptyBlockers：允许的路径不许被兜底逻辑塞进阻塞项。
func TestNetworkPlanAllowedKeepsEmptyBlockers(t *testing.T) {
	src := &fakeNetworkSource{assessment: network.Assessment{Allowed: true}}
	reg := newNetworkRegistry(t, src)
	action, _ := reg.Action(network.ActionApply)

	plan, err := action.Plan(context.Background(), PlanInput{Params: decodeApply(t, reg)})
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Allowed || len(plan.Blockers) != 0 {
		t.Fatalf("允许的计划不该有阻塞项：allowed=%v %+v", plan.Allowed, plan.Blockers)
	}
}

// TestNetworkConfirmPassesArrivedOn：候选地址是不是"这次请求到达的那个"由 Web 注入，
// 接线层只负责原样传下去——它不该在这里被补、被改、被清空。
func TestNetworkConfirmPassesArrivedOn(t *testing.T) {
	src := &fakeNetworkSource{report: network.WriteReport{Outcome: network.OutcomeVerified}}
	reg := newNetworkRegistry(t, src)
	action, _ := reg.Action(network.ActionConfirm)

	params := network.ConfirmParams{InterfaceID: testNetworkIface, Token: "nrb_" + "0123456789abcdef0123456789abcdef",
		ArrivedOn: "192.168.1.50"}
	if _, err := action.Execute(context.Background(), ExecuteInput{
		Params: params, OperationID: "op_1",
		Target: Target{IdentityFingerprint: "iface:ens4:aa:bb", StateVersion: "sha256:current"},
	}); err != nil {
		t.Fatal(err)
	}
	if len(src.requests) != 1 {
		t.Fatalf("没有真的执行：%+v", src.requests)
	}
	req := src.requests[0]
	if req.Action != network.ActionConfirm || req.OperationID != "op_1" {
		t.Fatalf("请求头不对：%+v", req)
	}
	got, ok := req.Params.(network.ConfirmParams)
	if !ok {
		t.Fatalf("参数类型不对：%T", req.Params)
	}
	if got.ArrivedOn != "192.168.1.50" {
		t.Fatalf("到达地址没传下去：%q", got.ArrivedOn)
	}
	// 预检绑定的身份与状态版本要带进执行器：执行前还要复核一次"还是那张网卡、还是那个现场"。
	if req.ExpectedIdentity != "iface:ens4:aa:bb" || req.ExpectedStateVersion != "sha256:current" {
		t.Fatalf("目标没带进执行器：%+v", req)
	}
}

// TestNetworkResultOutcomes：三种 outcome 各自活着走出这个包。
func TestNetworkResultOutcomes(t *testing.T) {
	cases := []struct {
		report   network.WriteReport
		want     string
		verified bool
	}{
		{network.WriteReport{Outcome: network.OutcomeVerified}, OutcomeVerified, true},
		{network.WriteReport{Outcome: network.OutcomeRolledBack}, OutcomeRolledBack, false},
		{network.WriteReport{Outcome: network.OutcomeUnknown}, OutcomeUnknown, false},
	}
	for _, c := range cases {
		src := &fakeNetworkSource{report: network.WriteReport{
			Outcome: c.report.Outcome, Observed: map[string]any{"k": 1},
			Data: map[string]any{"d": 2}, RecoveryHint: "去控制台看 ens4",
		}}
		reg := newNetworkRegistry(t, src)
		action, _ := reg.Action(network.ActionApply)
		result, err := action.Execute(context.Background(), ExecuteInput{
			Params: decodeApply(t, reg), OperationID: "op_1",
		})
		if err != nil {
			t.Fatalf("%s: %v", c.report.Outcome, err)
		}
		if result.Outcome != c.want {
			t.Fatalf("outcome 翻错了：想要 %q，得到 %q", c.want, result.Outcome)
		}
		if result.VerifiedAt.IsZero() == c.verified {
			t.Fatalf("%s: verified_at 有无不对（%v）", c.report.Outcome, result.VerifiedAt)
		}
		if result.ObservedState["k"] != 1 || result.Data["d"] != 2 {
			t.Fatalf("%s: 现场数据没带出来：%+v %+v", c.report.Outcome, result.ObservedState, result.Data)
		}
		if result.RecoveryHint != "去控制台看 ens4" {
			t.Fatalf("%s: 恢复提示没带出来：%q", c.report.Outcome, result.RecoveryHint)
		}
	}
}

// TestNetworkResultRejectsUnknownOutcome：执行器返回闭集之外的值是缺陷，不许当成功放过去。
func TestNetworkResultRejectsUnknownOutcome(t *testing.T) {
	src := &fakeNetworkSource{report: network.WriteReport{Outcome: "failed"}}
	reg := newNetworkRegistry(t, src)
	action, _ := reg.Action(network.ActionApply)

	if _, err := action.Execute(context.Background(), ExecuteInput{
		Params: decodeApply(t, reg), OperationID: "op_1",
	}); !errors.Is(err, ErrInvalidResult) {
		t.Fatalf("未知 outcome 应当报 ErrInvalidResult：%v", err)
	}
}

// TestNetworkResultPassesErrorThrough：执行器报错就原样往上抛，不在这里折成 outcome。
func TestNetworkResultPassesErrorThrough(t *testing.T) {
	sentinel := errors.New("netplan: 渲染失败")
	src := &fakeNetworkSource{execErr: sentinel}
	reg := newNetworkRegistry(t, src)
	action, _ := reg.Action(network.ActionApply)

	if _, err := action.Execute(context.Background(), ExecuteInput{
		Params: decodeApply(t, reg), OperationID: "op_1",
	}); !errors.Is(err, sentinel) {
		t.Fatalf("执行错误应当原样抛出：%v", err)
	}
}

// TestNetworkAssessErrorSurfaces：判决失败不是"不允许"，是错误，要抛给调用方。
func TestNetworkAssessErrorSurfaces(t *testing.T) {
	sentinel := errors.New("netplan 目录读不动")
	src := &fakeNetworkSource{assessErr: sentinel}
	reg := newNetworkRegistry(t, src)
	action, _ := reg.Action(network.ActionApply)

	if _, err := action.Plan(context.Background(), PlanInput{
		Params: decodeApply(t, reg),
	}); !errors.Is(err, sentinel) {
		t.Fatalf("判决错误应当原样抛出：%v", err)
	}
}

// TestNetworkConfigQuery：只读查询原样返回采集结果，且拒绝未知参数。
func TestNetworkConfigQuery(t *testing.T) {
	src := &fakeNetworkSource{current: network.ConfigState{
		Mode:        "static",
		Unsupported: []string{"存在 P14.1 不认识的 netplan 键"},
	}}
	reg := newNetworkRegistry(t, src)
	query, ok := reg.Query(NetworkConfigQueryName)
	if !ok {
		t.Fatalf("%s 没有登记", NetworkConfigQueryName)
	}
	params, err := query.Decode(json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("空参数应当被接受: %v", err)
	}
	if _, err := query.Decode(json.RawMessage(`{"interface_id":"ens4"}`)); err == nil {
		t.Fatal("未知参数应当被拒（查询不接受筛选条件，采集口径只有一份）")
	}
	got, err := query.Run(context.Background(), params)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	state, ok := got.(network.ConfigState)
	if !ok {
		t.Fatalf("返回类型不对：%T", got)
	}
	if state.Mode != "static" || len(state.Unsupported) != 1 {
		t.Fatalf("采集结果没原样返回：%+v", state)
	}
}
