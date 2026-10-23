package packages

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"upanel/internal/apt"
)

// P11.3 的核对逻辑：受理 ≠ 发生。
//
// 这一组用例守的是一条纪律：**只有 boot_id 变了才允许说"重启已确认"**。
// 关机永远不会有这种证据，所以它只能是 unreachable + 一句难听但真话的说明。

const (
	bootBefore = "ab2eb5e8-c1b4-4a00-a3d0-33ced7870abc"
	bootAfter  = "9f4d2c17-6a55-4c2e-8f31-0b7e5a1d9c40"
)

// powerResultJSON 拼一个代理会写出来的 result。
func powerResultJSON(outcome, operation, bootIDBefore string) json.RawMessage {
	observed := map[string]any{"operation": operation}
	if bootIDBefore != "" {
		observed["boot_id_before"] = bootIDBefore
	}
	raw, err := json.Marshal(map[string]any{"outcome": outcome, "observed_state": observed})
	if err != nil {
		panic(err)
	}
	return raw
}

// at 固定时钟，让 CheckedAt 可断言。
var at = time.Date(2026, 10, 23, 10, 0, 0, 0, time.UTC)

func checkService(t *testing.T, src Source) *Service {
	t.Helper()
	svc := newService(t, src)
	svc.now = func() time.Time { return at }
	return svc
}

func TestPowerJobCheckIgnoresOtherActions(t *testing.T) {
	src := &fakeSource{state: apt.PowerState{BootID: bootAfter}}
	svc := checkService(t, src)
	raw := powerResultJSON("accepted", "reboot", bootBefore)

	if got := svc.PowerJobCheck(context.Background(), "apt.upgrade", raw); got != nil {
		t.Fatalf("非电源动作不该有 power_check: %+v", got)
	}
	if src.power != 0 {
		t.Errorf("非电源动作不该去问代理，实际问了 %d 次", src.power)
	}
}

func TestPowerJobCheckSkipsNonAccepted(t *testing.T) {
	src := &fakeSource{state: apt.PowerState{BootID: bootAfter}}
	svc := checkService(t, src)
	for _, outcome := range []string{"verified", "failed", "canceled", "unknown"} {
		raw := powerResultJSON(outcome, "reboot", bootBefore)
		if got := svc.PowerJobCheck(context.Background(), PowerActionName, raw); got != nil {
			t.Errorf("outcome=%s 不该有 power_check: %+v", outcome, got)
		}
	}
	// 失败任务的 result 按契约是空对象，也不能崩。
	if got := svc.PowerJobCheck(context.Background(), PowerActionName, json.RawMessage(`{}`)); got != nil {
		t.Errorf("空 result 不该有 power_check: %+v", got)
	}
	if got := svc.PowerJobCheck(context.Background(), PowerActionName, json.RawMessage(`不是 json`)); got != nil {
		t.Errorf("坏 result 不该有 power_check: %+v", got)
	}
}

func TestPowerJobCheckRebootConfirmed(t *testing.T) {
	src := &fakeSource{state: apt.PowerState{BootID: bootAfter}}
	svc := checkService(t, src)

	got := svc.PowerJobCheck(context.Background(), PowerActionName,
		powerResultJSON("accepted", "reboot", bootBefore))
	if got == nil {
		t.Fatal("accepted 的重启任务应该有 power_check")
	}
	if got.State != PowerCheckConfirmed || !got.Changed {
		t.Fatalf("boot_id 变了却没说确认: %+v", got)
	}
	if got.BootIDBefore != bootBefore || got.BootIDNow != bootAfter {
		t.Fatalf("两个 boot_id 要原样带出: %+v", got)
	}
	if !got.CheckedAt.Equal(at) {
		t.Errorf("CheckedAt = %v，期望 %v", got.CheckedAt, at)
	}
	if !strings.Contains(got.Message, "重启已确认") || !strings.Contains(got.Message, "ab2eb5e8") {
		t.Errorf("确认语要说清楚且带短 boot_id: %q", got.Message)
	}
	if src.power != 1 {
		t.Errorf("核对应该现场问一次代理，实际 %d 次", src.power)
	}
}

func TestPowerJobCheckRebootUnchanged(t *testing.T) {
	src := &fakeSource{state: apt.PowerState{BootID: bootBefore}}
	svc := checkService(t, src)

	got := svc.PowerJobCheck(context.Background(), PowerActionName,
		powerResultJSON("accepted", "reboot", bootBefore))
	if got == nil || got.State != PowerCheckUnchanged || got.Changed {
		t.Fatalf("boot_id 没变就不能说重启过: %+v", got)
	}
	if strings.Contains(got.Message, "已确认") {
		t.Errorf("没变却说确认: %q", got.Message)
	}
	if !strings.Contains(got.Message, "倒计时") {
		t.Errorf("要指出可能是还在倒计时: %q", got.Message)
	}
}

// 关机之后代理必然连不上。这是预期之内，但**不是**关机成功的证据。
func TestPowerJobCheckPoweroffUnreachable(t *testing.T) {
	src := &fakeSource{stateErr: fmt.Errorf("%w: 拨号失败", ErrFactsUnavailable)}
	svc := checkService(t, src)

	got := svc.PowerJobCheck(context.Background(), PowerActionName,
		powerResultJSON("accepted", "poweroff", bootBefore))
	if got == nil || got.State != PowerCheckUnreachable {
		t.Fatalf("代理连不上应为 unreachable: %+v", got)
	}
	if got.Changed {
		t.Error("连不上时 changed 不能是真")
	}
	if strings.Contains(got.Message, "已关机") || strings.Contains(got.Message, "关机成功") {
		t.Errorf("界面话术不能伪称关机已验证: %q", got.Message)
	}
	if !strings.Contains(got.Message, "现场确认") {
		t.Errorf("要说清最终状态只能现场确认: %q", got.Message)
	}
}

func TestPowerJobCheckPoweroffCameBack(t *testing.T) {
	// 关机后人工开机：boot_id 变了。这只证明机器回来过，不证明电源切断过。
	src := &fakeSource{state: apt.PowerState{BootID: bootAfter}}
	svc := checkService(t, src)

	got := svc.PowerJobCheck(context.Background(), PowerActionName,
		powerResultJSON("accepted", "poweroff", bootBefore))
	if got == nil || got.State != PowerCheckConfirmed || !got.Changed {
		t.Fatalf("boot_id 变了应为 confirmed: %+v", got)
	}
	if strings.Contains(got.Message, "关机成功") {
		t.Errorf("关机不能宣称成功: %q", got.Message)
	}
	if !strings.Contains(got.Message, "电源当时是否真的切断") {
		t.Errorf("要留下无法确认的那一句: %q", got.Message)
	}
}

func TestPowerJobCheckUnknownStates(t *testing.T) {
	// 代理在，但 boot_id 读不出来。
	src := &fakeSource{state: apt.PowerState{BootIDError: "permission denied"}}
	svc := checkService(t, src)
	got := svc.PowerJobCheck(context.Background(), PowerActionName,
		powerResultJSON("accepted", "reboot", bootBefore))
	if got == nil || got.State != PowerCheckUnknown {
		t.Fatalf("boot_id 读不出来应为 unknown: %+v", got)
	}
	if !strings.Contains(got.Message, "permission denied") {
		t.Errorf("原因要带出来: %q", got.Message)
	}

	// 代理在，但给了个空 boot_id。
	src = &fakeSource{state: apt.PowerState{}}
	got = checkService(t, src).PowerJobCheck(context.Background(), PowerActionName,
		powerResultJSON("accepted", "reboot", bootBefore))
	if got == nil || got.State != PowerCheckUnknown {
		t.Fatalf("空 boot_id 应为 unknown: %+v", got)
	}
}

// 任务里没记下基准时，连代理都不该去问：没有可比的东西，问了也只能编。
func TestPowerJobCheckNoBaseline(t *testing.T) {
	src := &fakeSource{state: apt.PowerState{BootID: bootAfter}}
	svc := checkService(t, src)

	got := svc.PowerJobCheck(context.Background(), PowerActionName,
		powerResultJSON("accepted", "reboot", ""))
	if got == nil || got.State != PowerCheckUnknown {
		t.Fatalf("没有基准应为 unknown: %+v", got)
	}
	if src.power != 0 {
		t.Errorf("没有基准不该问代理，实际问了 %d 次", src.power)
	}
}

// 核对有自己的时间上限：它不能把任务轮询拖住。
func TestPowerJobCheckBoundedByContext(t *testing.T) {
	src := &blockingSource{}
	svc := newService(t, src)
	svc.now = func() time.Time { return at }

	got := svc.PowerJobCheck(context.Background(), PowerActionName,
		powerResultJSON("accepted", "reboot", bootBefore))
	if got == nil || got.State != PowerCheckUnreachable {
		t.Fatalf("查询超时应说成 unreachable: %+v", got)
	}
	if !src.hadDeadline {
		t.Error("核对必须带着自己的截止时间问代理")
	}
}

// blockingSource 一直等到 ctx 结束（模拟代理活着但不回话）。
type blockingSource struct{ hadDeadline bool }

func (b *blockingSource) Packages(context.Context, bool) (apt.Snapshot, error) {
	return apt.Snapshot{}, nil
}

func (b *blockingSource) PowerState(ctx context.Context) (apt.PowerState, error) {
	if _, ok := ctx.Deadline(); ok {
		b.hadDeadline = true
	}
	// 模拟"立刻就知道查询要超时"：真正等满 5 秒会让用例慢得没人愿意跑，
	// 而这里要验的是"有截止时间、超时算不可达"，不是计时精度。
	return apt.PowerState{}, context.DeadlineExceeded
}
