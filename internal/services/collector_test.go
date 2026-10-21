package services

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"upanel/internal/syscmd"
)

// fakeSystemctl 是一个按“命令 + 单元”返回预设输出的假 systemctl。
//
// 它只替换进程执行（Systemctl.runFn），其余逻辑——参数构造、输出解析、状态判定、
// 收敛轮询——全部走生产代码。
type fakeSystemctl struct {
	mu sync.Mutex
	// units 是“单元名 → 属性”的状态表；控制命令会就地修改它。
	units map[string]map[string]string
	// listErr 让 list 命令失败，用来测“采集失败保留上一次快照”。
	listErr error
	// controlErr 让控制命令失败（模拟 systemctl 非零退出）。
	controlErr error
	// stuck 为 true 时控制命令不改变状态（模拟“命令成功但没生效”）。
	stuck bool
	// calls 记录每次调用的参数，用来断言 argv 形态。
	calls [][]string
}

func newFakeSystemctl() *fakeSystemctl {
	return &fakeSystemctl{units: map[string]map[string]string{}}
}

// add 登记一个单元的属性。state 形如 "active|running"。
func (f *fakeSystemctl) add(unit, load, active, sub, enablement, description string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	pid := "0"
	if active == ActiveActive {
		pid = "812"
	}
	f.units[unit] = map[string]string{
		"Id":                   unit,
		"LoadState":            load,
		"ActiveState":          active,
		"SubState":             sub,
		"UnitFileState":        enablement,
		"Description":          description,
		"FragmentPath":         "/usr/lib/systemd/system/" + unit,
		"MainPID":              pid,
		"NRestarts":            "0",
		"ActiveEnterTimestamp": "Tue 2026-10-20 21:31:02 UTC",
		"MemoryCurrent":        "3403776",
		"TasksCurrent":         "1",
	}
}

func (f *fakeSystemctl) callsCopy() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]string, len(f.calls))
	copy(out, f.calls)
	return out
}

// run 模拟 systemctl 的输出。
func (f *fakeSystemctl) run(_ context.Context, _ time.Duration, args ...string) (syscmd.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, append([]string(nil), args...))
	if len(args) == 0 {
		return syscmd.Result{ExitCode: 1}, fmt.Errorf("空参数")
	}
	switch args[0] {
	case "list-unit-files", "list-units":
		if f.listErr != nil {
			return syscmd.Result{ExitCode: 1, Stderr: f.listErr.Error()}, f.listErr
		}
		var b strings.Builder
		for unit := range f.units {
			fmt.Fprintf(&b, "%s enabled\n", unit)
		}
		return syscmd.Result{ExitCode: 0, Stdout: b.String()}, nil
	case "show":
		names := namesAfterDashDash(args)
		var b strings.Builder
		for _, name := range names {
			props, ok := f.units[name]
			if !ok {
				props = map[string]string{"Id": name, "LoadState": LoadNotFound, "ActiveState": ActiveInactive}
			}
			for _, prop := range propertyList {
				fmt.Fprintf(&b, "%s=%s\n", prop, props[prop])
			}
			b.WriteString("\n")
		}
		return syscmd.Result{ExitCode: 0, Stdout: b.String()}, nil
	case OpStart, OpStop, OpRestart, OpEnable, OpDisable:
		if f.controlErr != nil {
			return syscmd.Result{ExitCode: 1, Stderr: f.controlErr.Error()}, f.controlErr
		}
		if len(args) < 2 {
			return syscmd.Result{ExitCode: 1, Stderr: "缺少单元名"}, fmt.Errorf("缺少单元名")
		}
		unit := args[len(args)-1]
		if !f.stuck {
			f.apply(args[0], unit)
		}
		return syscmd.Result{ExitCode: 0, Stdout: ""}, nil
	}
	return syscmd.Result{ExitCode: 1, Stderr: "未预期的命令"}, fmt.Errorf("未预期的命令: %v", args)
}

// apply 把控制命令的效果写进状态表。
func (f *fakeSystemctl) apply(op, unit string) {
	props, ok := f.units[unit]
	if !ok {
		return
	}
	switch op {
	case OpStart:
		props["ActiveState"] = ActiveActive
		props["SubState"] = "running"
		props["MainPID"] = "900"
		props["ActiveEnterTimestamp"] = time.Now().UTC().Format(timestampLayout)
	case OpStop:
		props["ActiveState"] = ActiveInactive
		props["SubState"] = "dead"
		props["MainPID"] = "0"
	case OpRestart:
		props["ActiveState"] = ActiveActive
		props["SubState"] = "running"
		props["MainPID"] = "901"
		props["ActiveEnterTimestamp"] = time.Now().UTC().Format(timestampLayout)
	case OpEnable:
		props["UnitFileState"] = "enabled"
	case OpDisable:
		props["UnitFileState"] = "disabled"
	}
}

func namesAfterDashDash(args []string) []string {
	for i, a := range args {
		if a == "--" {
			return args[i+1:]
		}
	}
	return nil
}

// newTestCollector 构造一个用假 systemctl 的采集器，收敛等待缩短到 600ms。
func newTestCollector(f *fakeSystemctl) *Collector {
	ctl := &Systemctl{path: "/usr/bin/systemctl", runFn: f.run}
	c := NewCollector(Options{
		Systemctl: ctl,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	c.verifyTimeout = 600 * time.Millisecond
	c.verifyInterval = 20 * time.Millisecond
	return c
}

// TestCollectBuildsSnapshot 核对采集结果：单元状态、保护判定、能力与版本。
func TestCollectBuildsSnapshot(t *testing.T) {
	f := newFakeSystemctl()
	f.add("cron.service", LoadLoaded, ActiveActive, "running", "enabled", "Regular background program processing daemon")
	f.add("ssh.service", LoadLoaded, ActiveActive, "running", "enabled", "OpenBSD Secure Shell server")
	f.add("myapp.service", LoadLoaded, ActiveInactive, "dead", "disabled", "示例应用")
	c := newTestCollector(f)

	snap := c.Collect(context.Background())
	if snap.Status != StatusOK {
		t.Fatalf("采集状态 = %q（%s）", snap.Status, snap.Error)
	}
	if snap.CollectedAt.IsZero() || snap.IntervalMS != DefaultInterval.Milliseconds() {
		t.Fatalf("采集时间/间隔不对: %+v", snap)
	}
	if len(snap.Units) != 3 {
		t.Fatalf("单元数 = %d，期望 3", len(snap.Units))
	}
	if snap.Units[0].Unit != "cron.service" {
		t.Fatalf("单元未按名字排序: %v", snap.Units[0].Unit)
	}
	byName := map[string]Unit{}
	for _, u := range snap.Units {
		byName[u.Unit] = u
	}
	if !byName["ssh.service"].Protection.Protected {
		t.Error("ssh.service 必须受保护")
	}
	if len(byName["ssh.service"].Capabilities) != 0 {
		t.Errorf("受保护的 ssh.service 不应有任何能力: %v", byName["ssh.service"].Capabilities)
	}
	if got := strings.Join(byName["cron.service"].Capabilities, ","); got != "stop,restart,disable" {
		t.Errorf("cron.service 能力 = %q", got)
	}
	if got := strings.Join(byName["myapp.service"].Capabilities, ","); got != "start,enable" {
		t.Errorf("myapp.service 能力 = %q", got)
	}
	if byName["cron.service"].MainPID == nil || *byName["cron.service"].MainPID != 812 {
		t.Errorf("MainPID 解析不对: %v", byName["cron.service"].MainPID)
	}
	if byName["cron.service"].MemoryBytes == nil || *byName["cron.service"].MemoryBytes != "3403776" {
		t.Errorf("MemoryCurrent 解析不对: %v", byName["cron.service"].MemoryBytes)
	}
	if snap.SnapshotVersion == "" || !strings.HasPrefix(snap.SnapshotVersion, "sha256:") {
		t.Errorf("快照版本不合法: %q", snap.SnapshotVersion)
	}
	// 只有一次 list-unit-files、一次 list-units、一次 show：没有每单元一个进程。
	if calls := f.callsCopy(); len(calls) != 3 {
		t.Errorf("采集调用了 %d 次 systemctl，期望 3 次（两次列表 + 一次 show）: %v", len(calls), calls)
	}
}

// TestCollectKeepsPreviousSnapshotOnFailure 证明采集失败不清空列表。
func TestCollectKeepsPreviousSnapshotOnFailure(t *testing.T) {
	f := newFakeSystemctl()
	f.add("cron.service", LoadLoaded, ActiveActive, "running", "enabled", "")
	c := newTestCollector(f)
	first := c.Collect(context.Background())
	if first.Status != StatusOK {
		t.Fatalf("第一次采集应成功: %s", first.Error)
	}

	f.mu.Lock()
	f.listErr = fmt.Errorf("Failed to list units: Connection refused")
	f.mu.Unlock()

	second := c.Collect(context.Background())
	if second.Status != StatusError {
		t.Fatalf("第二次采集应标记失败，实际 %q", second.Status)
	}
	if len(second.Units) != 1 || second.Units[0].Unit != "cron.service" {
		t.Fatalf("失败时不应清空单元列表: %+v", second.Units)
	}
	if !second.CollectedAt.Equal(first.CollectedAt) {
		t.Error("失败时采集时间应停在上一次成功的时间")
	}
	if !strings.Contains(second.Error, "Connection refused") {
		t.Errorf("失败原因应包含 systemctl 的原话，实际 %q", second.Error)
	}
	if !strings.Contains(second.Error, "采集尝试于") {
		t.Errorf("失败原因应带采集尝试时间，实际 %q", second.Error)
	}
}

// TestSnapshotBeforeFirstCollect 在还没采集过时如实报失败，而不是给空列表。
func TestSnapshotBeforeFirstCollect(t *testing.T) {
	c := newTestCollector(newFakeSystemctl())
	snap := c.Snapshot()
	if snap.Status != StatusError || snap.Error == "" {
		t.Fatalf("未采集时应报失败: %+v", snap)
	}
	if snap.Units == nil || len(snap.Units) != 0 {
		t.Errorf("未采集时应返回空数组而不是 nil: %#v", snap.Units)
	}
}

// TestCollectWithoutSystemctl 报依赖缺失。
func TestCollectWithoutSystemctl(t *testing.T) {
	c := NewCollector(Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	snap := c.Collect(context.Background())
	if snap.Status != StatusError || !strings.Contains(snap.Error, "systemctl") {
		t.Fatalf("没有 systemctl 时应报依赖缺失: %+v", snap)
	}
	if _, _, err := c.Unit(context.Background(), "cron.service"); err != ErrSystemctlUnavailable {
		t.Fatalf("Unit 应返回 ErrSystemctlUnavailable，实际 %v", err)
	}
	if _, err := c.AssessControl(context.Background(), "cron.service", OpStart); err != ErrSystemctlUnavailable {
		t.Fatalf("AssessControl 应返回 ErrSystemctlUnavailable，实际 %v", err)
	}
}

// TestUnitLiveRead 现场读取：存在与不存在要能区分。
func TestUnitLiveRead(t *testing.T) {
	f := newFakeSystemctl()
	f.add("cron.service", LoadLoaded, ActiveActive, "running", "enabled", "cron")
	c := newTestCollector(f)

	u, found, err := c.Unit(context.Background(), "cron.service")
	if err != nil || !found {
		t.Fatalf("读取 cron.service 失败: found=%v err=%v", found, err)
	}
	if u.ActiveState != ActiveActive {
		t.Errorf("状态 = %q", u.ActiveState)
	}
	if _, found, err := c.Unit(context.Background(), "nope.service"); err != nil || found {
		t.Fatalf("不存在的单元应 found=false: found=%v err=%v", found, err)
	}
	if _, _, err := c.Unit(context.Background(), "../etc/passwd"); err == nil {
		t.Fatal("非法单元名必须被拒绝")
	}
	// 模板单元：found=false，而且一次 systemctl 都不该调用——systemctl show 不接受这种名字。
	f.mu.Lock()
	before := len(f.calls)
	f.mu.Unlock()
	if _, found, err := c.Unit(context.Background(), "tpl@.service"); err != nil || found {
		t.Fatalf("模板单元应 found=false: found=%v err=%v", found, err)
	}
	f.mu.Lock()
	after := len(f.calls)
	f.mu.Unlock()
	if after != before {
		t.Errorf("模板单元不应触发 systemctl 调用，实际 %d 次", after-before)
	}
}

// TestAssessControl 逐条核对预检拒绝理由。
func TestAssessControl(t *testing.T) {
	f := newFakeSystemctl()
	f.add("cron.service", LoadLoaded, ActiveActive, "running", "enabled", "cron")
	f.add("myapp.service", LoadLoaded, ActiveInactive, "dead", "disabled", "app")
	f.add("static.service", LoadLoaded, ActiveInactive, "dead", "static", "static unit")
	f.add("ssh.service", LoadLoaded, ActiveActive, "running", "enabled", "ssh")
	f.add("tpl@.service", LoadLoaded, ActiveInactive, "dead", "disabled", "template")
	c := newTestCollector(f)

	cases := []struct {
		name   string
		unit   string
		op     string
		code   string
		allowe bool
	}{
		{"受保护", "ssh.service", OpStop, CodeProtected, false},
		{"不存在", "ghost.service", OpStart, CodeUnitNotFound, false},
		{"active 还要 start", "cron.service", OpStart, CodeStateConflict, false},
		{"inactive 还要 stop", "myapp.service", OpStop, CodeStateConflict, false},
		{"inactive 不能 restart", "myapp.service", OpRestart, CodeStateConflict, false},
		{"已启用还要 enable", "cron.service", OpEnable, CodeStateConflict, false},
		{"未启用还要 disable", "myapp.service", OpDisable, CodeStateConflict, false},
		{"static 不能 enable", "static.service", OpEnable, CodeNotEnableable, false},
		{"模板单元", "tpl@.service", OpStart, CodeStateConflict, false},
		{"可以 stop", "cron.service", OpStop, "", true},
		{"可以 restart", "cron.service", OpRestart, "", true},
		{"可以 start", "myapp.service", OpStart, "", true},
		{"可以 enable", "myapp.service", OpEnable, "", true},
		{"可以 disable", "cron.service", OpDisable, "", true},
	}
	for _, c2 := range cases {
		t.Run(c2.name, func(t *testing.T) {
			a, err := c.AssessControl(context.Background(), c2.unit, c2.op)
			if err != nil {
				t.Fatalf("AssessControl 出错: %v", err)
			}
			if a.Allowed != c2.allowe {
				t.Fatalf("Allowed = %v，期望 %v（blockers=%v）", a.Allowed, c2.allowe, a.Blockers)
			}
			if c2.code == "" {
				return
			}
			found := false
			for _, b := range a.Blockers {
				if b.Code == c2.code {
					found = true
				}
				if strings.TrimSpace(b.Message) == "" {
					t.Errorf("阻塞项 %s 缺少说明", b.Code)
				}
			}
			if !found {
				t.Errorf("缺少阻塞项 %s，实际 %v", c2.code, a.Blockers)
			}
		})
	}

	// 拒绝时也要有身份与状态版本：预检 DTO 需要它们做绑定。
	a, err := c.AssessControl(context.Background(), "ghost.service", OpStart)
	if err != nil {
		t.Fatal(err)
	}
	if a.Identity == "" || a.StateVersion == "" || a.DisplayName == "" {
		t.Errorf("不存在的单元也要有身份/版本/展示名: %+v", a)
	}
	if len(a.Locks) != 1 || a.Locks[0] != "service:ghost.service" {
		t.Errorf("锁 = %v", a.Locks)
	}
	// 告警与影响：stop 必须说明会中断。
	a, err = c.AssessControl(context.Background(), "cron.service", OpStop)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Warnings) == 0 || a.Warnings[0].Code != WarningInterruption {
		t.Errorf("stop 应带中断告警: %+v", a.Warnings)
	}
	if len(a.Impacts) == 0 || a.Impacts[0].Kind != ImpactAvailability {
		t.Errorf("stop 应带可用性影响: %+v", a.Impacts)
	}
	// 预检必须只读：整个 AssessControl 只调用 show，不调用控制动词。
	for _, call := range f.callsCopy() {
		switch call[0] {
		case "start", "stop", "restart", "enable", "disable":
			t.Fatalf("预检阶段出现了写操作: %v", call)
		}
	}
}

// TestExecuteControlVerified 走过“执行 → 读回 → 收敛”的完整路径。
func TestExecuteControlVerified(t *testing.T) {
	f := newFakeSystemctl()
	f.add("myapp.service", LoadLoaded, ActiveInactive, "dead", "disabled", "app")
	c := newTestCollector(f)

	var steps []string
	res, err := c.ExecuteControl(context.Background(), "myapp.service", OpStart, func(step string, _ map[string]any) {
		steps = append(steps, step)
	})
	if err != nil {
		t.Fatalf("启动失败: %v", err)
	}
	if !res.Verified {
		t.Fatal("状态收敛后应标记 verified")
	}
	if res.After.ActiveState != ActiveActive {
		t.Errorf("读回状态 = %q", res.After.ActiveState)
	}
	if res.Before.ActiveState != ActiveInactive {
		t.Errorf("执行前状态 = %q", res.Before.ActiveState)
	}
	if strings.Join(steps, ",") != "systemctl_start,verified" {
		t.Errorf("进度步骤 = %v", steps)
	}
	// argv 固定：动词、--no-ask-password、单元名。没有多余选项。
	var control []string
	for _, call := range f.callsCopy() {
		if call[0] == OpStart {
			control = call
		}
	}
	want := []string{OpStart, "--no-ask-password", "myapp.service"}
	if control == nil {
		t.Fatal("没有发出 start 命令")
	}
	if strings.Join(control, " ") != strings.Join(want, " ") {
		t.Errorf("systemctl 参数 = %v，期望 %v", control, want)
	}
}

// TestExecuteControlNotConverged 命令成功但状态没变时必须报错，不能算成功。
func TestExecuteControlNotConverged(t *testing.T) {
	f := newFakeSystemctl()
	f.add("myapp.service", LoadLoaded, ActiveInactive, "dead", "disabled", "app")
	f.stuck = true
	c := newTestCollector(f)

	res, err := c.ExecuteControl(context.Background(), "myapp.service", OpStart, nil)
	if err == nil {
		t.Fatal("状态未收敛必须返回错误")
	}
	if res.Verified {
		t.Fatal("未收敛时 Verified 必须为 false")
	}
	if !strings.Contains(err.Error(), "没有在") || !strings.Contains(err.Error(), "active") {
		t.Errorf("错误应说明期望状态，实际 %q", err)
	}
}

// TestExecuteControlCommandFailure 命令失败时把 systemctl 原话带回。
func TestExecuteControlCommandFailure(t *testing.T) {
	f := newFakeSystemctl()
	f.add("myapp.service", LoadLoaded, ActiveActive, "running", "enabled", "app")
	f.controlErr = fmt.Errorf("Job for myapp.service failed because the control process exited with error code")
	c := newTestCollector(f)

	_, err := c.ExecuteControl(context.Background(), "myapp.service", OpStop, nil)
	if err == nil {
		t.Fatal("命令失败必须返回错误")
	}
	if !strings.Contains(err.Error(), "control process exited") {
		t.Errorf("错误应包含 systemctl 原话，实际 %q", err)
	}
	if !strings.Contains(err.Error(), "退出状态 1") {
		t.Errorf("错误应包含退出状态，实际 %q", err)
	}
}

// TestExecuteControlRechecksProtection 证明执行前会再查一次保护名单（TOCTOU）。
func TestExecuteControlRechecksProtection(t *testing.T) {
	f := newFakeSystemctl()
	f.add("ssh.service", LoadLoaded, ActiveActive, "running", "enabled", "ssh")
	c := newTestCollector(f)

	_, err := c.ExecuteControl(context.Background(), "ssh.service", OpStop, nil)
	if err == nil || !strings.Contains(err.Error(), "受保护") {
		t.Fatalf("受保护单元的执行必须被拒绝，实际 %v", err)
	}
	if len(f.callsCopy()) != 1 { // 只有一次 show，没有 stop
		t.Errorf("不应调用任何控制命令: %v", f.callsCopy())
	}
}

// TestExecuteControlMissingUnit 目标不存在时拒绝。
func TestExecuteControlMissingUnit(t *testing.T) {
	f := newFakeSystemctl()
	c := newTestCollector(f)
	if _, err := c.ExecuteControl(context.Background(), "ghost.service", OpStart, nil); err == nil {
		t.Fatal("不存在的单元必须被拒绝")
	}
}
