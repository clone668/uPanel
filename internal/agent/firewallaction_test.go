package agent

// P14.3：防火墙写动作的接线层测试。
//
// 与 networkaction_test.go 同一个口径：这一层只做解码、翻计划、翻结果三件事，
// 所以只测这三件。真正的判断（管理通路、加缺删多、读回核验、回滚）在
// internal/network 的 firewallapply_test.go 里。

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"upanel/internal/network"
)

// fakeFirewallSource 是 FirewallApplySource 的假实现。
type fakeFirewallSource struct {
	current     network.FirewallConfigState
	assessment  network.Assessment
	assessErr   error
	report      network.FirewallWriteReport
	execErr     error
	applyCalls  []network.FirewallApplyParams
	confirmCall []network.FirewallConfirmParams
	rollCall    []network.FirewallRollbackParams
	requests    []network.FirewallWriteRequest
}

func (f *fakeFirewallSource) Current(context.Context) network.FirewallConfigState { return f.current }

func (f *fakeFirewallSource) Assess(_ context.Context, params network.FirewallApplyParams) (network.Assessment, error) {
	f.applyCalls = append(f.applyCalls, params)
	if f.assessErr != nil {
		return network.Assessment{}, f.assessErr
	}
	return f.assessment, nil
}

func (f *fakeFirewallSource) AssessConfirm(_ context.Context, params network.FirewallConfirmParams) (network.Assessment, error) {
	f.confirmCall = append(f.confirmCall, params)
	if f.assessErr != nil {
		return network.Assessment{}, f.assessErr
	}
	return f.assessment, nil
}

func (f *fakeFirewallSource) AssessRollback(_ context.Context, params network.FirewallRollbackParams) (network.Assessment, error) {
	f.rollCall = append(f.rollCall, params)
	if f.assessErr != nil {
		return network.Assessment{}, f.assessErr
	}
	return f.assessment, nil
}

func (f *fakeFirewallSource) ExecuteWrite(_ context.Context, req network.FirewallWriteRequest) (network.FirewallWriteReport, error) {
	f.requests = append(f.requests, req)
	if f.execErr != nil {
		return network.FirewallWriteReport{}, f.execErr
	}
	return f.report, nil
}

func newFirewallRegistry(t *testing.T, src FirewallApplySource) *Registry {
	t.Helper()
	reg := NewRegistry()
	if err := RegisterFirewallWriteActions(reg, src); err != nil {
		t.Fatalf("登记防火墙写动作失败: %v", err)
	}
	if err := RegisterFirewallConfigQuery(reg, src); err != nil {
		t.Fatalf("登记 %s 失败: %v", FirewallConfigQueryName, err)
	}
	return reg
}

// firewallDefinition 是一份合法的定义：启用 + 一条放行 SSH。
func firewallDefinition() *network.FirewallDefinition {
	return &network.FirewallDefinition{Enabled: true, Rules: []network.ManagedRule{
		{ID: "ssh", Action: network.RuleAllow, Port: 22, Proto: network.ProtoTCP},
	}}
}

func TestFirewallWriteRegistryRequiresSource(t *testing.T) {
	if err := RegisterFirewallWriteActions(NewRegistry(), nil); err == nil {
		t.Fatal("没有实现端不该登记成功")
	}
	if err := RegisterFirewallConfigQuery(NewRegistry(), nil); err == nil {
		t.Fatal("没有实现端不该登记 firewall.config")
	}
	reg := NewRegistry()
	if err := RegisterFirewallWriteActionsIfAvailable(reg, nil); err != nil {
		t.Fatalf("可选登记失败: %v", err)
	}
	if err := RegisterFirewallConfigQueryIfAvailable(reg, nil); err != nil {
		t.Fatalf("可选登记失败: %v", err)
	}
	for _, name := range []string{network.FirewallActionApply, network.FirewallActionConfirm,
		network.FirewallActionRollback} {
		if _, ok := reg.Action(name); ok {
			t.Fatalf("未接线时不该出现 %s", name)
		}
	}
	if _, ok := reg.Query(FirewallConfigQueryName); ok {
		t.Fatalf("未接线时不该出现 %s", FirewallConfigQueryName)
	}
}

func TestFirewallWriteRegistryNames(t *testing.T) {
	reg := newFirewallRegistry(t, &fakeFirewallSource{})
	for _, name := range []string{network.FirewallActionApply, network.FirewallActionConfirm,
		network.FirewallActionRollback} {
		if _, ok := reg.Action(name); !ok {
			t.Fatalf("%s 没有登记", name)
		}
		if !contains(reg.ExecutableActionNames(), name) {
			t.Fatalf("%s 应当可执行：%v", name, reg.ExecutableActionNames())
		}
	}
	if _, ok := reg.Query(FirewallConfigQueryName); !ok {
		t.Fatalf("%s 没有登记", FirewallConfigQueryName)
	}
}

// TestFirewallActionPolicies：CONTRACTS §13.7 的策略表。
func TestFirewallActionPolicies(t *testing.T) {
	reg := newFirewallRegistry(t, &fakeFirewallSource{})

	apply, _ := reg.Action(network.FirewallActionApply)
	policy, err := apply.ResolvePolicy(nil)
	if err != nil {
		t.Fatalf("apply 策略不合法: %v", err)
	}
	if !policy.RequiresReauth {
		t.Fatal("firewall.apply 必须再认证")
	}
	if !policy.RequiresPhrase || policy.PhraseVerb != "RECONFIGURE" {
		t.Fatalf("firewall.apply 的短语应是 RECONFIGURE，得到 %q", policy.PhraseVerb)
	}
	if policy.Cancellable {
		t.Fatal("firewall.apply 不可取消：改到一半停下比跑完更糟")
	}

	for _, name := range []string{network.FirewallActionConfirm, network.FirewallActionRollback} {
		action, _ := reg.Action(name)
		p, err := action.ResolvePolicy(nil)
		if err != nil {
			t.Fatalf("%s 策略不合法: %v", name, err)
		}
		if p.RequiresReauth || p.RequiresPhrase || p.Cancellable {
			t.Fatalf("%s 不该有门槛、也不可取消：%+v", name, p)
		}
	}
}

func TestFirewallDecodeStrict(t *testing.T) {
	reg := newFirewallRegistry(t, &fakeFirewallSource{})
	apply, _ := reg.Action(network.FirewallActionApply)

	raw, err := json.Marshal(network.FirewallApplyParams{
		ExpectedConfigHash: "sha256:abc", Definition: firewallDefinition()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := apply.Decode(raw); err != nil {
		t.Fatalf("正常参数被拒: %v", err)
	}

	bad := []string{
		// 未知字段
		`{"expected_config_hash":"h","definition":{"enabled":true,"rules":[]},"extra":1}`,
		// 缺定义
		`{"expected_config_hash":"h"}`,
		// 规则 ID 形状不对（会拼进 ufw 的 comment）
		`{"expected_config_hash":"h","definition":{"enabled":true,"rules":[{"id":"SSH!","action":"allow","port":22,"proto":"tcp"}]}}`,
		// 端口越界
		`{"expected_config_hash":"h","definition":{"enabled":true,"rules":[{"id":"a","action":"allow","port":70000,"proto":"tcp"}]}}`,
		// 协议只认 tcp/udp
		`{"expected_config_hash":"h","definition":{"enabled":true,"rules":[{"id":"a","action":"allow","port":22,"proto":"sctp"}]}}`,
		// 动作只认四个
		`{"expected_config_hash":"h","definition":{"enabled":true,"rules":[{"id":"a","action":"drop","port":22,"proto":"tcp"}]}}`,
		// 来源只认 IPv4 CIDR：IPv6 与主机名会让"这条规则到底放行了谁"变得含糊
		`{"expected_config_hash":"h","definition":{"enabled":true,"rules":[{"id":"a","action":"allow","port":22,"proto":"tcp","from":"::1"}]}}`,
		`{"expected_config_hash":"h","definition":{"enabled":true,"rules":[{"id":"a","action":"allow","port":22,"proto":"tcp","from":"anywhere"}]}}`,
		// 同一个 ID 出现两次：删谁加谁都会变成猜
		`{"expected_config_hash":"h","definition":{"enabled":true,"rules":[{"id":"a","action":"allow","port":22,"proto":"tcp"},{"id":"a","action":"allow","port":23,"proto":"tcp"}]}}`,
	}
	for _, one := range bad {
		if _, err := apply.Decode(json.RawMessage(one)); !errors.Is(err, ErrParamsInvalid) {
			t.Fatalf("%s 应当被拒: %v", one, err)
		}
	}
}

func TestFirewallConfirmDecodeHasNoArrivedOn(t *testing.T) {
	reg := newFirewallRegistry(t, &fakeFirewallSource{})
	confirm, _ := reg.Action(network.FirewallActionConfirm)
	// arrived_on 是网络那条路的字段；防火墙这边收到它必须被当成未知字段拒掉，
	// 而不是默默接受一个这里根本没有意义的参数。
	if _, err := confirm.Decode(json.RawMessage(
		`{"token":"frb_abc","arrived_on":"10.0.0.5"}`)); !errors.Is(err, ErrParamsInvalid) {
		t.Fatalf("arrived_on 应当被拒: %v", err)
	}
	if _, err := confirm.Decode(json.RawMessage(`{"token":"frb_abc"}`)); err != nil {
		t.Fatalf("正常参数被拒: %v", err)
	}
}

func TestFirewallPlanMapping(t *testing.T) {
	src := &fakeFirewallSource{assessment: network.Assessment{
		Normalized:          network.FirewallApplyParams{ExpectedConfigHash: "sha256:abc"},
		DisplayName:         "防火墙：启用，2 条规则",
		IdentityFingerprint: "firewall:ufw",
		StateVersion:        "sha256:abc",
		Allowed:             true,
		Warnings:            []network.Notice{{Code: "W1", Message: "提醒"}},
		Impacts:             []network.Impact{{Kind: "firewall-add", Description: "新增 1 条"}},
		Locks:               []string{network.FirewallLockName},
	}}
	reg := newFirewallRegistry(t, src)
	action, _ := reg.Action(network.FirewallActionApply)
	raw, _ := json.Marshal(network.FirewallApplyParams{
		ExpectedConfigHash: "sha256:abc", Definition: firewallDefinition()})
	params, err := action.Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := action.Plan(context.Background(), PlanInput{Params: params})
	if err != nil {
		t.Fatalf("预检失败: %v", err)
	}
	if !plan.Allowed || plan.Target.StateVersion != "sha256:abc" ||
		plan.Target.IdentityFingerprint != "firewall:ufw" {
		t.Fatalf("判决没有原样过桥: %+v", plan)
	}
	if len(plan.Warnings) != 1 || len(plan.Impacts) != 1 || len(plan.Locks) != 1 {
		t.Fatalf("提醒/影响/锁没有原样过桥: %+v", plan)
	}
}

func TestFirewallPlanFallsBackWhenNotAllowedWithoutBlockers(t *testing.T) {
	src := &fakeFirewallSource{assessment: network.Assessment{Allowed: false}}
	reg := newFirewallRegistry(t, src)
	action, _ := reg.Action(network.FirewallActionApply)
	raw, _ := json.Marshal(network.FirewallApplyParams{Definition: firewallDefinition()})
	params, err := action.Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := action.Plan(context.Background(), PlanInput{Params: params})
	if err != nil {
		t.Fatalf("预检失败: %v", err)
	}
	if plan.Allowed || len(plan.Blockers) != 1 {
		t.Fatalf("拒绝时必须给出原因: %+v", plan)
	}
}

func TestFirewallResultOutcomes(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{network.OutcomeVerified, OutcomeVerified},
		{network.OutcomeRolledBack, OutcomeRolledBack},
		{network.OutcomeUnknown, OutcomeUnknown},
	}
	for _, c := range cases {
		got, err := firewallResultFromReport(network.FirewallWriteReport{
			Outcome: c.in, Observed: map[string]any{"enabled": true},
			Data: map[string]any{"rollback_token": "frb_x"}, RecoveryHint: "提示",
		}, nil)
		if err != nil {
			t.Fatalf("%s 翻译失败: %v", c.in, err)
		}
		if got.Outcome != c.want {
			t.Fatalf("%s 翻成了 %s", c.in, got.Outcome)
		}
		if got.RecoveryHint != "提示" || got.ObservedState == nil || got.Data == nil {
			t.Fatalf("%s 的观察/数据/提示被吞了: %+v", c.in, got)
		}
	}
	if _, err := firewallResultFromReport(network.FirewallWriteReport{Outcome: "failed"}, nil); err == nil {
		t.Fatal("未知 outcome 应当报错：防火墙这一层没有 failed")
	}
	if _, err := firewallResultFromReport(network.FirewallWriteReport{}, errors.New("炸了")); err == nil {
		t.Fatal("执行错误应当原样带出")
	}
}

// TestFirewallApplyExecutePassesTarget 确认执行时把计划的指纹与状态版本原样带上：
// 少了它，预检与执行之间现场变了也照样改。
func TestFirewallApplyExecutePassesTarget(t *testing.T) {
	src := &fakeFirewallSource{report: network.FirewallWriteReport{Outcome: network.OutcomeVerified}}
	reg := newFirewallRegistry(t, src)
	action, _ := reg.Action(network.FirewallActionApply)
	raw, _ := json.Marshal(network.FirewallApplyParams{
		ExpectedConfigHash: "sha256:abc", Definition: firewallDefinition()})
	params, err := action.Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := action.Execute(context.Background(), ExecuteInput{
		Params: params,
		Target: Target{IdentityFingerprint: "firewall:ufw", StateVersion: "sha256:abc"},
	}); err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if len(src.requests) != 1 {
		t.Fatalf("应调用一次执行器，实际 %d", len(src.requests))
	}
	req := src.requests[0]
	if req.Action != network.FirewallActionApply || req.ExpectedIdentity != "firewall:ufw" ||
		req.ExpectedStateVersion != "sha256:abc" {
		t.Fatalf("执行请求没带上目标的身份与状态版本: %+v", req)
	}
}
