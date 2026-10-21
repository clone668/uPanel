package services

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"upanel/internal/syscmd"
)

// 本文件里的样例输出全部是 2026-10-21 从测试虚拟机 upanel-2404 上真实抓取的
// （systemd 255 / Ubuntu 24.04），见 testdata/。解析器必须能吃掉这些真实字节，
// 而不是吃掉我猜的格式。

func readFixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("读取样例 %s 失败: %v", name, err)
	}
	return string(data)
}

// errTestExit 模拟“进程正常启动但非零退出”。
var errTestExit = &syscmd.ExitError{Program: "/usr/bin/systemctl", ExitCode: 1}

// showNames 取 `--` 之后的单元名（测试里用来区分“整批”与“单独重问”）。
func showNames(args []string) []string {
	for i, a := range args {
		if a == "--" {
			return args[i+1:]
		}
	}
	return nil
}

// TestParseRealShowOutput 用真实 systemctl show 输出核对属性解析。
func TestParseRealShowOutput(t *testing.T) {
	raws := parseShowOutput(readFixture(t, "show_cron.service.txt"))
	if len(raws) != 1 {
		t.Fatalf("解析出 %d 个块，期望 1 个", len(raws))
	}
	u, ok := unitFromRaw(raws[0], time.Now().UTC())
	if !ok {
		t.Fatal("真实输出解析出来的单元被判为无效")
	}
	if u.Unit != "cron.service" {
		t.Errorf("单元名 = %q", u.Unit)
	}
	if u.Description != "Regular background program processing daemon" {
		t.Errorf("描述 = %q", u.Description)
	}
	if u.LoadState != LoadLoaded || u.ActiveState != ActiveActive || u.SubState != "running" {
		t.Errorf("状态解析不对: %s/%s/%s", u.LoadState, u.ActiveState, u.SubState)
	}
	if u.UnitFileState != "enabled" || u.Enablement != EnablementEnabled {
		t.Errorf("开机启动解析不对: %q / %q", u.UnitFileState, u.Enablement)
	}
	if u.MainPID == nil || *u.MainPID != 781 {
		t.Errorf("MainPID = %v", u.MainPID)
	}
	if u.RestartCount == nil || *u.RestartCount != 0 {
		t.Errorf("NRestarts = %v", u.RestartCount)
	}
	if u.MemoryBytes == nil || *u.MemoryBytes != "483328" {
		t.Errorf("MemoryCurrent = %v", u.MemoryBytes)
	}
	if u.TasksCurrent == nil || *u.TasksCurrent != 1 {
		t.Errorf("TasksCurrent = %v", u.TasksCurrent)
	}
	if u.StartedAt == nil || !u.StartedAt.Equal(time.Date(2026, 10, 21, 1, 6, 39, 0, time.UTC)) {
		t.Errorf("ActiveEnterTimestamp = %v", u.StartedAt)
	}
	if u.Protection.Protected {
		t.Error("cron.service 不应受保护")
	}
	if got := strings.Join(u.Capabilities, ","); got != "stop,restart,disable" {
		t.Errorf("能力 = %q", got)
	}
}

// TestParseRealNotFoundOutput 不存在的单元：systemctl 输出 not-found 且 [not set]。
func TestParseRealNotFoundOutput(t *testing.T) {
	raws := parseShowOutput(readFixture(t, "show_notfound.service.txt"))
	if len(raws) != 1 {
		t.Fatalf("解析出 %d 个块", len(raws))
	}
	raw := raws[0]
	if raw.Get("Id") != "upanel-nope.service" {
		t.Errorf("Id = %q", raw.Get("Id"))
	}
	if raw.Get("LoadState") != LoadNotFound {
		t.Errorf("LoadState = %q", raw.Get("LoadState"))
	}
	u, ok := unitFromRaw(raw, time.Now().UTC())
	if !ok {
		t.Fatal("不存在的单元也要能构造出来（界面要显示它不存在）")
	}
	if u.LoadState != LoadNotFound {
		t.Errorf("LoadState = %q", u.LoadState)
	}
	if u.MemoryBytes != nil {
		t.Errorf("`[not set]` 必须解析成 nil，实际 %q", *u.MemoryBytes)
	}
	if u.StartedAt != nil {
		t.Errorf("空时间戳必须解析成 nil，实际 %v", *u.StartedAt)
	}
	if u.Enablement != EnablementUnknown {
		t.Errorf("空 UnitFileState 应为 unknown，实际 %q", u.Enablement)
	}
	if len(u.Capabilities) != 0 {
		t.Errorf("not-found 单元不应有任何能力: %v", u.Capabilities)
	}

	// 走一次 ShowOne：必须报 found=false，而不是把 not-found 当成正常单元。
	ctl := &Systemctl{path: "/usr/bin/systemctl", runFn: func(_ context.Context, _ time.Duration, _ ...string) (syscmd.Result, error) {
		return syscmd.Result{Stdout: readFixture(t, "show_notfound.service.txt")}, nil
	}}
	if _, found, err := ctl.ShowOne(context.Background(), "upanel-nope.service"); err != nil || found {
		t.Fatalf("ShowOne 应报 found=false: found=%v err=%v", found, err)
	}
}

// TestListUnitNamesRealOutput 用真实列表输出核对名字枚举。
func TestListUnitNamesRealOutput(t *testing.T) {
	files := readFixture(t, "list-unit-files.txt")
	units := readFixture(t, "list-units.txt")
	ctl := &Systemctl{path: "/usr/bin/systemctl", runFn: func(_ context.Context, _ time.Duration, args ...string) (syscmd.Result, error) {
		switch args[0] {
		case "list-unit-files":
			return syscmd.Result{Stdout: files}, nil
		case "list-units":
			return syscmd.Result{Stdout: units}, nil
		}
		return syscmd.Result{ExitCode: 1}, nil
	}}
	names, err := ctl.ListUnitNames(context.Background())
	if err != nil {
		t.Fatalf("枚举失败: %v", err)
	}
	seen := map[string]bool{}
	for _, n := range names {
		if !strings.HasSuffix(n, ".service") {
			t.Errorf("枚举出了非 service 单元: %q", n)
		}
		if strings.Contains(n, " ") {
			t.Errorf("枚举出了带空格的“名字”（说明解析错了列）: %q", n)
		}
		if seen[n] {
			t.Errorf("名字重复: %q", n)
		}
		seen[n] = true
	}
	for _, want := range []string{"apparmor.service", "apt-daily.service", "auditd.service"} {
		// auditd 在 list-units 里是 not-found，仍应被枚举到（界面上显示“不在这台机器上”）。
		if !seen[want] {
			t.Errorf("缺少 %q", want)
		}
	}
}

// TestListUnitNamesPartialFailure 一条命令失败、另一条成功时仍返回结果。
func TestListUnitNamesPartialFailure(t *testing.T) {
	units := readFixture(t, "list-units.txt")
	ctl := &Systemctl{path: "/usr/bin/systemctl", runFn: func(_ context.Context, _ time.Duration, args ...string) (syscmd.Result, error) {
		if args[0] == "list-unit-files" {
			return syscmd.Result{ExitCode: 1, Stderr: "Failed to list unit files: Permission denied"}, errTestExit
		}
		return syscmd.Result{Stdout: units}, nil
	}}
	names, err := ctl.ListUnitNames(context.Background())
	if err != nil {
		t.Fatalf("部分失败不应整体失败: %v", err)
	}
	if len(names) == 0 {
		t.Fatal("应保留成功那条命令的结果")
	}
}

// TestListUnitNamesBothFailed 两条都失败时把 systemctl 原话带出来。
func TestListUnitNamesBothFailed(t *testing.T) {
	ctl := &Systemctl{path: "/usr/bin/systemctl", runFn: func(_ context.Context, _ time.Duration, _ ...string) (syscmd.Result, error) {
		return syscmd.Result{ExitCode: 1, Stderr: "System has not been booted with systemd as init system (PID 1). Can't operate."}, errTestExit
	}}
	_, err := ctl.ListUnitNames(context.Background())
	if err == nil {
		t.Fatal("两条都失败必须报错")
	}
	if !strings.Contains(err.Error(), "System has not been booted with systemd") {
		t.Errorf("错误应包含 systemctl 原话，实际 %q", err)
	}
}

// TestNewSystemctlRejectsNonSystemctlPath 路径白名单：不能把别的程序塞进来。
func TestNewSystemctlRejectsNonSystemctlPath(t *testing.T) {
	for _, path := range []string{"", "systemctl", "/bin/sh", "/usr/bin/bash.sh", "/tmp/systemctl"} {
		if _, err := NewSystemctl(path); err == nil {
			t.Errorf("%q 应被拒绝", path)
		}
	}
	// 真实存在的 systemctl 才能通过（测试机上没有时跳过）。
	if _, err := os.Stat("/usr/bin/systemctl"); err == nil {
		if _, err := NewSystemctl("/usr/bin/systemctl"); err != nil {
			t.Errorf("/usr/bin/systemctl 应被接受: %v", err)
		}
	}
}

// TestParseRealMultiUnitOutput 一次 show 多个单元：块之间只有一个空行，
// 属性顺序由 systemd 决定（Id 不一定在第一行），解析按 '=' 切、不按位置。
func TestParseRealMultiUnitOutput(t *testing.T) {
	raws := parseShowOutput(readFixture(t, "show_three_units.txt"))
	if len(raws) != 3 {
		t.Fatalf("解析出 %d 个块，期望 3 个", len(raws))
	}
	got := make([]string, 0, 3)
	for _, raw := range raws {
		got = append(got, raw.Get("Id"))
	}
	want := []string{"apparmor.service", "cron.service", "upanel-nope.service"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 个块 = %q，期望 %q（全部：%v）", i, got[i], want[i], got)
		}
	}
	// 中间那块是真在运行的服务，最后一个是不存在的单元：两者的区别必须保留。
	if u, ok := unitFromRaw(raws[1], time.Now().UTC()); !ok ||
		u.ActiveState != ActiveActive || u.MainPID == nil || *u.MainPID != 781 {
		t.Errorf("cron.service 解析不对: ok=%v unit=%+v", ok, u)
	}
	if u, ok := unitFromRaw(raws[2], time.Now().UTC()); !ok || u.LoadState != LoadNotFound {
		t.Errorf("not-found 单元解析不对: ok=%v load_state=%q", ok, u.LoadState)
	}
}

// TestListUnitNamesExcludesTemplates 模板单元不进枚举，实例单元进。
//
// 这是实测结论：systemctl show 碰到 `foo@.service` 会打一行 stderr 并丢掉后面的名字，
// 所以模板名绝不能进 show 的参数（见 testdata/show_template_rejected.stderr.txt）。
func TestListUnitNamesExcludesTemplates(t *testing.T) {
	files := readFixture(t, "list-unit-files.txt")
	units := readFixture(t, "list-units.txt")
	ctl := &Systemctl{path: "/usr/bin/systemctl", runFn: func(_ context.Context, _ time.Duration, args ...string) (syscmd.Result, error) {
		switch args[0] {
		case "list-unit-files":
			return syscmd.Result{Stdout: files}, nil
		case "list-units":
			return syscmd.Result{Stdout: units}, nil
		}
		return syscmd.Result{ExitCode: 1}, nil
	}}
	names, err := ctl.ListUnitNames(context.Background())
	if err != nil {
		t.Fatalf("枚举失败: %v", err)
	}
	seen := map[string]bool{}
	for _, n := range names {
		seen[n] = true
	}
	for _, bad := range []string{"apport-forward@.service", "apport-coredump-hook@.service", "serial-getty@.service"} {
		if seen[bad] {
			t.Errorf("模板单元 %q 不应被枚举（show 会因它中断整次调用）", bad)
		}
	}
	for _, want := range []string{"serial-getty@ttyS0.service", "ssh.service", "apparmor.service"} {
		if !seen[want] {
			t.Errorf("实例单元 %q 应被枚举", want)
		}
	}
}

// TestShowFailsWhenChunkAborts 与实测同形：systemctl 在坏名字处中断，
// 只留下前半段的块 + 一行 stderr + 退出码 1。旧实现会静默返回这半份列表；
// 现在必须报错——否则页面显示的是一份“看起来正常”的假列表。
func TestShowFailsWhenChunkAborts(t *testing.T) {
	partial := readFixture(t, "show_three_units.txt")
	// 真实行为：前三块之后的 apport-forward@.service 让整批中断。
	partial = strings.SplitAfter(partial, "Id=upanel-nope.service")[0] + "\n"
	stderrLine := strings.TrimSpace(readFixture(t, "show_template_rejected.stderr.txt"))
	ctl := &Systemctl{path: "/usr/bin/systemctl", runFn: func(_ context.Context, _ time.Duration, args ...string) (syscmd.Result, error) {
		// 只有带多个名字的那次会“中断”；单独问一个名字时，坏名字报同一条 stderr。
		names := showNames(args)
		if len(names) == 1 && names[0] == "apport-forward@.service" {
			return syscmd.Result{ExitCode: 1, Stderr: stderrLine}, errTestExit
		}
		return syscmd.Result{Stdout: partial, ExitCode: 1, Stderr: stderrLine}, errTestExit
	}}
	_, err := ctl.Show(context.Background(), []string{"apparmor.service", "cron.service", "apport-forward@.service"})
	if err == nil {
		t.Fatal("被中断的 show 必须报错，不能返回半份列表")
	}
	if !strings.Contains(err.Error(), "apport-forward@.service") {
		t.Errorf("错误要说清是哪个单元读不到，实际 %q", err)
	}
	if !strings.Contains(err.Error(), "neither a valid invocation ID nor unit name") {
		t.Errorf("错误要带上 systemctl 原话，实际 %q", err)
	}
}

// TestShowRetriesMissingNamesIndividually 整批输出里缺的名字会单独重问一次：
// 一次只问一个，坏名字不连累别人，回来的仍是完整列表。
func TestShowRetriesMissingNamesIndividually(t *testing.T) {
	threeUnits := readFixture(t, "show_three_units.txt")
	// 整批只给了前两块（模拟“后面被丢掉”），单独问第三个时正常返回。
	batch := strings.SplitAfter(threeUnits, "ActiveEnterTimestamp=Wed 2026-10-21 01:06:39 UTC")[0] + "\n"
	var singleCalls int
	ctl := &Systemctl{path: "/usr/bin/systemctl", runFn: func(_ context.Context, _ time.Duration, args ...string) (syscmd.Result, error) {
		names := showNames(args)
		if len(names) == 1 && names[0] == "upanel-nope.service" {
			singleCalls++
			return syscmd.Result{Stdout: "Id=upanel-nope.service\nLoadState=not-found\n"}, nil
		}
		// 整批：第三个单元被“丢掉”，退出码 1（与实测一致）。
		return syscmd.Result{Stdout: batch, ExitCode: 1}, errTestExit
	}}
	raws, err := ctl.Show(context.Background(), []string{"apparmor.service", "cron.service", "upanel-nope.service"})
	if err != nil {
		t.Fatalf("单独重问能补回来时不该报错: %v", err)
	}
	if singleCalls == 0 {
		t.Fatal("缺失的名字必须单独重问一次")
	}
	if len(raws) != 3 {
		t.Fatalf("返回 %d 个块，期望 3 个（缺的要补回来，而不是悄悄少一个）", len(raws))
	}
	if raws[2].Get("Id") != "upanel-nope.service" {
		t.Errorf("第三个块 = %q", raws[2].Get("Id"))
	}
}

// TestShowMatchesAliasByName 别名单元的 Id 不是你请求的名字。
//
// 实测：`systemctl show -- dbus-org.freedesktop.ModemManager1.service` 的 Id 是
// ModemManager.service，别名只出现在 Names 里。只按 Id 匹配会把它当成“读不到”，
// 于是一次采集在 300 个单元里因为一个别名整个失败。
func TestShowMatchesAliasByName(t *testing.T) {
	aliasOut := readFixture(t, "show_alias.service.txt")
	ctl := &Systemctl{path: "/usr/bin/systemctl", runFn: func(_ context.Context, _ time.Duration, args ...string) (syscmd.Result, error) {
		names := showNames(args)
		if len(names) == 1 && names[0] == "dbus-org.freedesktop.ModemManager1.service" {
			return syscmd.Result{Stdout: aliasOut}, nil
		}
		// 整批：我们只“给了”别名这一个块，规范名那块没回来。
		return syscmd.Result{Stdout: aliasOut}, nil
	}}
	raws, err := ctl.Show(context.Background(), []string{"dbus-org.freedesktop.ModemManager1.service"})
	if err != nil {
		t.Fatalf("别名单元不该让 Show 失败: %v", err)
	}
	if len(raws) != 1 || raws[0].Get("Id") != "ModemManager.service" {
		t.Fatalf("别名块解析不对: %+v", raws)
	}
	u, ok := unitFromRaw(raws[0], time.Now().UTC())
	if !ok || u.Unit != "ModemManager.service" || u.ActiveState != ActiveActive {
		t.Errorf("别名块构造出的单元不对: ok=%v unit=%+v", ok, u)
	}
}

// TestListUnitNamesExcludesAliases 别名单元不进枚举，规范名进。
//
// 两个理由：同一份单元出现两行时 service:<unit> 锁锁不住同一个对象；
// 而保护名单按基名判定，别名会让受保护单元看起来“没受保护”。
func TestListUnitNamesExcludesAliases(t *testing.T) {
	files := readFixture(t, "list-unit-files.txt")
	units := readFixture(t, "list-units.txt")
	ctl := &Systemctl{path: "/usr/bin/systemctl", runFn: func(_ context.Context, _ time.Duration, args ...string) (syscmd.Result, error) {
		switch args[0] {
		case "list-unit-files":
			return syscmd.Result{Stdout: files}, nil
		case "list-units":
			return syscmd.Result{Stdout: units}, nil
		}
		return syscmd.Result{ExitCode: 1}, nil
	}}
	names, err := ctl.ListUnitNames(context.Background())
	if err != nil {
		t.Fatalf("枚举失败: %v", err)
	}
	seen := map[string]bool{}
	for _, n := range names {
		seen[n] = true
	}
	for _, bad := range []string{
		"dbus-org.freedesktop.ModemManager1.service",
		"dbus-org.freedesktop.login1.service",
		"autovt@.service", // 既是别名又是模板
	} {
		if seen[bad] {
			t.Errorf("别名单元 %q 不应被枚举", bad)
		}
	}
	if !seen["apparmor.service"] {
		t.Error("普通单元 apparmor.service 应被枚举")
	}
}

// TestControlRejectsUnsafeArgs 控制命令只接受枚举动词与合法单元名。
func TestControlRejectsUnsafeArgs(t *testing.T) {
	var called int
	ctl := &Systemctl{path: "/usr/bin/systemctl", runFn: func(_ context.Context, _ time.Duration, _ ...string) (syscmd.Result, error) {
		called++
		return syscmd.Result{}, nil
	}}
	if _, err := ctl.Control(context.Background(), "restart --no-block", "cron.service"); err == nil {
		t.Error("非枚举动词必须被拒绝")
	}
	if _, err := ctl.Control(context.Background(), OpRestart, "cron.service; reboot"); err == nil {
		t.Error("带元字符的单元名必须被拒绝")
	}
	if _, err := ctl.Control(context.Background(), OpRestart, "--now"); err == nil {
		t.Error("以 -- 开头的“单元名”必须被拒绝")
	}
	if called != 0 {
		t.Errorf("被拒绝的参数不应执行任何进程，实际执行了 %d 次", called)
	}
}
