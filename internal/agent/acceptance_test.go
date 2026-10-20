package agent

// P03.4：测试用无害动作必须来自**独立注册表**，不得混入生产白名单。
//
// STAGES.md P03.4 的措辞是“测试用无害动作由独立测试注册表提供，不放入生产白名单”。
// 这句话不能只靠注释遵守——生产白名单一旦被塞进一个 acceptance 动作，运维看到的就是
// 一台能执行“测试动作”的机器，而“测试动作”这个名字并不保证它永远不碰系统。
// 这个文件把那句约束变成断言。

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestProductionRegistryHasNoAcceptanceActions 断言两件事：
// 生产登记表为空；即便将来它有了真实动作，也不会包含任何 acceptance.* 名字。
func TestProductionRegistryHasNoAcceptanceActions(t *testing.T) {
	prod, err := NewProductionRegistry(ProductionDeps{})
	if err != nil {
		t.Fatalf("构造生产登记表失败: %v", err)
	}
	if names := prod.ActionNames(); len(names) != 0 {
		t.Fatalf("生产动作表应为空（第一个真实动作不在 P03 范围），实际 %v", names)
	}
	acceptance := NewAcceptanceRegistry()
	names := acceptance.ActionNames()
	if len(names) == 0 {
		t.Fatal("验收注册表为空：P03 的端到端验收就没有无害动作可用了")
	}
	for _, name := range names {
		if !strings.HasPrefix(name, AcceptanceActionPrefix) {
			t.Fatalf("验收动作 %q 没有使用 %q 前缀，一眼看不出它是测试用的", name, AcceptanceActionPrefix)
		}
		if _, ok := prod.Action(name); ok {
			t.Fatalf("生产登记表里出现了验收动作 %q", name)
		}
	}
}

// TestAcceptanceActionsAreCompleteAndHarmless 检查每个验收动作都能预检、都带
// “这是验收专用”的告警，并且预检计划里没有任何真实系统影响。
func TestAcceptanceActionsAreCompleteAndHarmless(t *testing.T) {
	acceptance := NewAcceptanceRegistry()
	now := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	cases := map[string]string{
		"acceptance.probe": `{"label":"alpha"}`,
		"acceptance.ping":  `{"note":"beta"}`,
	}
	for name, raw := range cases {
		action, ok := acceptance.Action(name)
		if !ok {
			t.Fatalf("验收动作 %q 未登记", name)
		}
		if action.Decode == nil || action.Plan == nil || action.Execute == nil {
			t.Fatalf("验收动作 %q 不完整（Decode/Plan/Execute 必须都有）", name)
		}
		params, err := action.Decode([]byte(raw))
		if err != nil {
			t.Fatalf("%s 解码 %s 失败: %v", name, raw, err)
		}
		plan, err := action.Plan(context.Background(), PlanInput{
			Params:    params,
			Actor:     ActorContext{UserID: 1},
			Now:       now,
			ExpiresAt: now.Add(120 * time.Second),
		})
		if err != nil {
			t.Fatalf("%s 预检失败: %v", name, err)
		}
		var hasWarning bool
		for _, w := range plan.Warnings {
			if w.Code == "acceptance_only" {
				hasWarning = true
			}
		}
		if !hasWarning {
			t.Fatalf("%s 的预检计划没有 acceptance_only 告警：界面上会被当成真实动作", name)
		}
		if len(plan.Impacts) != 1 || plan.Impacts[0].Kind != "none" {
			t.Fatalf("%s 的预检计划声明了系统影响: %+v", name, plan.Impacts)
		}
		if !strings.Contains(plan.Target.DisplayName, "验收") {
			t.Fatalf("%s 的目标显示名 %q 没有标明这是验收对象", name, plan.Target.DisplayName)
		}
		if len(plan.Locks) == 0 {
			t.Fatalf("%s 没有声明资源锁：验收动作同样要走锁语义", name)
		}
	}
}

// TestAcceptanceActionsRejectUnknownParams 证明验收动作也走严格解码：
// 它们不能变成“参数随便传、执行器自己看着办”的后门。
func TestAcceptanceActionsRejectUnknownParams(t *testing.T) {
	acceptance := NewAcceptanceRegistry()
	for _, name := range acceptance.ActionNames() {
		action, _ := acceptance.Action(name)
		if _, err := action.Decode([]byte(`{"note":"x","command":"rm -rf /"}`)); err == nil {
			t.Fatalf("%s 接受了未知字段：验收动作也不会给字符串命令留入口", name)
		}
	}
}
