package services

import (
	"strings"
	"testing"
	"time"
)

// TestValidateUnitName 守的是“单元名不会变成 systemctl 选项或命令片段”这条边界。
func TestValidateUnitName(t *testing.T) {
	valid := []string{
		"cron.service",
		"ssh.service",
		"systemd-journald.service",
		"getty@tty1.service",
		"upanel-agent.service",
		"foo_bar-baz:qux.service",
		"snap.foo.bar.service",
	}
	for _, name := range valid {
		if err := ValidateUnitName(name); err != nil {
			t.Errorf("ValidateUnitName(%q) 应该通过，实际: %v", name, err)
		}
	}
	invalid := []string{
		"",
		"cron",
		"cron.service.service?",
		"-rf.service",
		"cron.service;rm -rf /",
		"../../etc/passwd",
		"cron service",
		"cron.service\n",
		"a..b.service",
		"cron.socket",
		strings.Repeat("a", 300) + ".service",
		"foo/bar.service",
		"$(id).service",
		"`id`.service",
	}
	for _, name := range invalid {
		if err := ValidateUnitName(name); err == nil {
			t.Errorf("ValidateUnitName(%q) 应该被拒绝", name)
		}
	}
}

// TestProtect 逐类核对保护名单：面板、代理、SSH、systemd 核心、网络、其他模块。
func TestProtect(t *testing.T) {
	cases := []struct {
		unit     string
		category string
	}{
		{"upanel-agent.service", ProtectSelf},
		{"upanel-web.service", ProtectSelf},
		{"ssh.service", ProtectSSH},
		{"sshd.service", ProtectSSH},
		{"ssh@session1.service", ProtectSSH},
		{"systemd-journald.service", ProtectCore},
		{"systemd-logind.service", ProtectCore},
		{"systemd-anything-new.service", ProtectCore},
		{"dbus.service", ProtectCore},
		{"systemd-resolved.service", ProtectCore}, // 同时命中 systemd- 前缀与网络名单：类别取第一条
		{"NetworkManager.service", ProtectNetwork},
		{"networking.service", ProtectNetwork},
		{"nginx.service", ProtectOtherModule},
		{"smbd.service", ProtectOtherModule},
		{"docker.service", ProtectOtherModule},
	}
	for _, c := range cases {
		p := Protect(c.unit)
		if !p.Protected {
			t.Errorf("Protect(%q) 应该受保护", c.unit)
			continue
		}
		if p.Category != c.category {
			t.Errorf("Protect(%q) 类别 = %q，期望 %q", c.unit, p.Category, c.category)
		}
		if len(p.Reasons) == 0 {
			t.Errorf("Protect(%q) 必须给出理由", c.unit)
		}
	}
	for _, unit := range []string{"cron.service", "apt-daily.service", "myapp.service"} {
		if p := Protect(unit); p.Protected {
			t.Errorf("Protect(%q) 不应受保护，实际: %+v", unit, p)
		}
	}
	// 理由去重且排序固定：同一单元多次调用结果一致。
	a := Protect("systemd-resolved.service")
	b := Protect("systemd-resolved.service")
	if strings.Join(a.Reasons, "|") != strings.Join(b.Reasons, "|") {
		t.Fatalf("Protect 的理由不稳定：%v vs %v", a.Reasons, b.Reasons)
	}
}

// TestCapabilities 核对“当前状态下允许哪些操作”，受保护单元必须为空。
func TestCapabilities(t *testing.T) {
	base := Unit{Unit: "demo.service", LoadState: LoadLoaded, Protection: Protection{Reasons: []string{}}}

	active := base
	active.ActiveState = ActiveActive
	active.Enablement = EnablementEnabled
	if got := strings.Join(capabilityFor(active), ","); got != "stop,restart,disable" {
		t.Errorf("active+enabled 的能力 = %q", got)
	}

	inactive := base
	inactive.ActiveState = ActiveInactive
	inactive.Enablement = EnablementDisabled
	if got := strings.Join(capabilityFor(inactive), ","); got != "start,enable" {
		t.Errorf("inactive+disabled 的能力 = %q", got)
	}

	failed := base
	failed.ActiveState = ActiveFailed
	if got := strings.Join(capabilityFor(failed), ","); got != "start,stop" {
		t.Errorf("failed 的能力 = %q", got)
	}

	static := base
	static.ActiveState = ActiveInactive
	static.Enablement = EnablementStatic
	if got := strings.Join(capabilityFor(static), ","); got != "start" {
		t.Errorf("static 的能力 = %q（不应出现 enable）", got)
	}

	protected := active
	protected.Protection = Protection{Protected: true, Category: ProtectSSH, Reasons: []string{"x"}}
	if got := capabilityFor(protected); len(got) != 0 {
		t.Errorf("受保护单元的能力应为空，实际 %v", got)
	}

	tmpl := base
	tmpl.Unit = "getty@.service"
	tmpl.Template = true
	if got := capabilityFor(tmpl); len(got) != 0 {
		t.Errorf("模板单元的能力应为空，实际 %v", got)
	}

	missing := base
	missing.LoadState = LoadNotFound
	if got := capabilityFor(missing); len(got) != 0 {
		t.Errorf("not-found 单元的能力应为空，实际 %v", got)
	}
}

// TestComputeSnapshotVersionIgnoresRuntimeNoise 证明版本只由状态内容决定。
func TestComputeSnapshotVersionIgnoresRuntimeNoise(t *testing.T) {
	now := time.Now().UTC()
	pid := int64(100)
	mem := "1024"
	one := []Unit{{
		Unit: "a.service", LoadState: LoadLoaded, ActiveState: ActiveActive,
		SubState: "running", Enablement: EnablementEnabled,
		MainPID: &pid, MemoryBytes: &mem, CheckedAt: now,
	}}
	two := []Unit{{
		Unit: "a.service", LoadState: LoadLoaded, ActiveState: ActiveActive,
		SubState: "running", Enablement: EnablementEnabled,
		MainPID: nil, MemoryBytes: nil, RestartCount: nil,
		CheckedAt: now.Add(time.Minute),
	}}
	if ComputeSnapshotVersion(one) != ComputeSnapshotVersion(two) {
		t.Fatal("PID/内存/采集时间不应影响快照版本")
	}
	changed := []Unit{{
		Unit: "a.service", LoadState: LoadLoaded, ActiveState: ActiveInactive,
		SubState: "dead", Enablement: EnablementEnabled,
	}}
	if ComputeSnapshotVersion(one) == ComputeSnapshotVersion(changed) {
		t.Fatal("active→inactive 必须改变快照版本")
	}
	// 顺序无关。
	x := Unit{Unit: "a.service", LoadState: LoadLoaded, ActiveState: ActiveActive, Enablement: EnablementEnabled}
	y := Unit{Unit: "b.service", LoadState: LoadLoaded, ActiveState: ActiveInactive, Enablement: EnablementDisabled}
	if ComputeSnapshotVersion([]Unit{x, y}) != ComputeSnapshotVersion([]Unit{y, x}) {
		t.Fatal("快照版本应与单元顺序无关")
	}
}

// TestStateVersion 证明单个单元的状态版本只看影响操作的因素。
func TestStateVersion(t *testing.T) {
	a := Unit{Unit: "a.service", LoadState: LoadLoaded, ActiveState: ActiveInactive, SubState: "dead", Enablement: EnablementDisabled}
	b := a
	b.CheckedAt = time.Now().UTC()
	b.MainPID = nil
	if StateVersion(a) != StateVersion(b) {
		t.Fatal("采集时间/PID 不应影响单元状态版本")
	}
	c := a
	c.Enablement = EnablementEnabled
	if StateVersion(a) == StateVersion(c) {
		t.Fatal("enablement 变化必须改变状态版本")
	}
}

// TestParsers 核对 systemd 属性的解析：没有值要变成 nil，而不是 0。
func TestParsers(t *testing.T) {
	if got := parseNumPtr("812"); got == nil || *got != 812 {
		t.Errorf("parseNumPtr(812) = %v", got)
	}
	for _, v := range []string{"", "[not set]", "infinity"} {
		if got := parseNumPtr(v); got != nil {
			t.Errorf("parseNumPtr(%q) 应为 nil，实际 %v", v, *got)
		}
	}
	if got := parseBytesPtr("3403776"); got == nil || *got != "3403776" {
		t.Errorf("parseBytesPtr 结果不对: %v", got)
	}
	if got := parseBytesPtr("[not set]"); got != nil {
		t.Errorf("MemoryCurrent=[not set] 应为 nil，实际 %v", *got)
	}
	// MainPID=0 是 systemd 的“没有主进程”，不是 PID 0。真机上每个停止的服务都是这个值，
	// 原样传出去界面就会写“PID 0”——一个不存在的进程号。
	if got := parsePIDPtr("784"); got == nil || *got != 784 {
		t.Errorf("parsePIDPtr(784) = %v", got)
	}
	for _, v := range []string{"0", "-1", "", "[not set]"} {
		if got := parsePIDPtr(v); got != nil {
			t.Errorf("parsePIDPtr(%q) 应为 nil，实际 %v", v, *got)
		}
	}
	ts := parseTimestamp("Tue 2026-10-20 21:31:02 UTC")
	if ts == nil || !ts.Equal(time.Date(2026, 10, 20, 21, 31, 2, 0, time.UTC)) {
		t.Errorf("parseTimestamp 结果不对: %v", ts)
	}
	for _, v := range []string{"", "n/a", "garbage"} {
		if got := parseTimestamp(v); got != nil {
			t.Errorf("parseTimestamp(%q) 应为 nil，实际 %v", v, got)
		}
	}
}
