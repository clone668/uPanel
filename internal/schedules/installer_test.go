package schedules

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"upanel/internal/syscmd"
)

const testScheduleID = "sch_0123456789abcdef0123456789abcdef"

// fakeRunner 按子命令分派，记录调用顺序。
type fakeRunner struct {
	calls      []string
	calendar   string // systemd-analyze 的 stdout
	calErr     error
	envs       [][]string // 每次调用带的环境变量
	show       string     // systemctl show 的 stdout
	enableErr  error
	disableErr error
}

func (f *fakeRunner) run(_ context.Context, spec syscmd.Spec, _ syscmd.Limits) (syscmd.Result, error) {
	res := syscmd.Result{Program: spec.Program, ExitCode: 0}
	joined := strings.Join(spec.Args, " ")
	f.calls = append(f.calls, filepath.Base(spec.Program)+" "+joined)
	f.envs = append(f.envs, spec.Env)
	switch {
	case len(spec.Args) > 0 && spec.Args[0] == "calendar":
		res.Stdout = f.calendar
		if f.calErr != nil {
			res.Stderr = "Failed to parse calendar specification: Invalid argument"
			return res, f.calErr
		}
		return res, nil
	case len(spec.Args) > 0 && spec.Args[0] == "show":
		res.Stdout = f.show
		return res, nil
	case len(spec.Args) > 0 && spec.Args[0] == "enable":
		if f.enableErr != nil {
			res.Stderr = "Failed to enable unit: Unit file does not exist."
			res.ExitCode = 1
			return res, &syscmd.ExitError{Program: spec.Program, ExitCode: 1}
		}
		return res, nil
	case len(spec.Args) > 0 && spec.Args[0] == "disable":
		if f.disableErr != nil {
			res.Stderr = "Failed to stop unit: Operation refused."
			res.ExitCode = 1
			return res, &syscmd.ExitError{Program: spec.Program, ExitCode: 1}
		}
		return res, nil
	}
	return res, nil
}

func (f *fakeRunner) saw(sub string) bool {
	for _, c := range f.calls {
		if strings.Contains(c, sub) {
			return true
		}
	}
	return false
}

func (f *fakeRunner) indexOf(sub string) int {
	for i, c := range f.calls {
		if strings.Contains(c, sub) {
			return i
		}
	}
	return -1
}

const goodCalendar = `  Original form: *-*-* 03:30:00 Asia/Shanghai
Normalized form: *-*-* 03:30:00 Asia/Shanghai
    Next elapse: Sun 2026-10-25 03:30:00 CST
       (in UTC): Sat 2026-10-24 19:30:00 UTC
       From now: 18h left
`

const enabledShow = `UnitFileState=enabled
ActiveState=active
NextElapseUSecRealtime=Sat 2026-10-24 19:30:00 UTC
`

func testDefinition() Definition {
	return Definition{
		Name:               "每晚备份",
		Kind:               KindBackup,
		TargetID:           "pol_0123456789abcdef0123456789abcdef",
		CalendarExpression: "*-*-* 03:30:00",
		Timezone:           "Asia/Shanghai",
		Persistent:         true,
		MaxRuntimeSeconds:  3600,
		Enabled:            true,
	}
}

func newTestInstaller(t *testing.T, f *fakeRunner) (*Installer, string) {
	t.Helper()
	dir := t.TempDir()
	inst, err := NewInstaller(InstallerOptions{
		Dir:       dir,
		Runner:    f.run,
		Systemctl: "/usr/bin/systemctl",
		Analyzer:  "/usr/bin/systemd-analyze",
	})
	if err != nil {
		t.Fatalf("NewInstaller: %v", err)
	}
	return inst, dir
}

func TestNewInstallerRequiresTools(t *testing.T) {
	if _, err := NewInstaller(InstallerOptions{Dir: t.TempDir(), Stat: func(string) (os.FileInfo, error) {
		return nil, os.ErrNotExist
	}}); err == nil {
		t.Fatal("找不到 systemctl / systemd-analyze 时必须报错")
	}
	if _, err := NewInstaller(InstallerOptions{Dir: "etc/systemd"}); err == nil {
		t.Fatal("相对单元目录必须被拒绝")
	}
}

func TestEnsureWritesUnitsAndVerifiesEnabled(t *testing.T) {
	f := &fakeRunner{calendar: goodCalendar, show: enabledShow}
	inst, dir := newTestInstaller(t, f)

	st, err := inst.Ensure(context.Background(), testScheduleID, testDefinition())
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	// 顺序：先让 systemd 认表达式，再落盘，再 daemon-reload，最后 enable。
	ci, di, ei := f.indexOf("calendar"), f.indexOf("daemon-reload"), f.indexOf("enable --now")
	if ci < 0 || di < 0 || ei < 0 || !(ci < di && di < ei) {
		t.Fatalf("调用顺序不对: %v", f.calls)
	}
	if !f.saw("enable --now " + TimerName(testScheduleID)) {
		t.Fatalf("没有启用 timer: %v", f.calls)
	}
	svc, err := os.ReadFile(filepath.Join(dir, ServiceName(testScheduleID)))
	if err != nil {
		t.Fatalf("读 service: %v", err)
	}
	if !strings.Contains(string(svc), "--schedule-id "+testScheduleID) {
		t.Fatalf("service 没有带上计划 id:\n%s", svc)
	}
	timer, err := os.ReadFile(filepath.Join(dir, TimerName(testScheduleID)))
	if err != nil {
		t.Fatalf("读 timer: %v", err)
	}
	if !strings.Contains(string(timer), "OnCalendar=*-*-* 03:30:00 Asia/Shanghai") {
		t.Fatalf("timer 的 OnCalendar 不对:\n%s", timer)
	}
	if !st.Present || !st.Enabled() || !st.Active() {
		t.Fatalf("回读事实 = %+v, 期望 存在/enabled/active", st)
	}
}

func TestEnsureRefusesWhenCalendarRejected(t *testing.T) {
	f := &fakeRunner{calendar: "", calErr: &syscmd.ExitError{Program: "/usr/bin/systemd-analyze", ExitCode: 1}}
	inst, dir := newTestInstaller(t, f)

	_, err := inst.Ensure(context.Background(), testScheduleID, testDefinition())
	if err == nil {
		t.Fatal("日历被拒时必须报错")
	}
	if !strings.Contains(err.Error(), "不认这个日历表达式") {
		t.Fatalf("错误 = %v", err)
	}
	if f.saw("daemon-reload") || f.saw("enable") {
		t.Fatalf("被拒之后还动了 systemd: %v", f.calls)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("被拒之后留下了单元文件: %v", entries)
	}
}

func TestEnsureRefusesNeverElapsingCalendar(t *testing.T) {
	f := &fakeRunner{calendar: `Normalized form: *-*-* 03:30:00 Asia/Shanghai
    Next elapse: never
       (in UTC): never
`}
	inst, _ := newTestInstaller(t, f)
	if _, err := inst.Ensure(context.Background(), testScheduleID, testDefinition()); err == nil {
		t.Fatal("永远不触发的表达式必须被拒绝")
	}
}

func TestEnsureReportsEnableFailure(t *testing.T) {
	f := &fakeRunner{calendar: goodCalendar, show: enabledShow, enableErr: errors.New("boom")}
	inst, dir := newTestInstaller(t, f)

	_, err := inst.Ensure(context.Background(), testScheduleID, testDefinition())
	if err == nil {
		t.Fatal("enable 失败必须报错")
	}
	if !strings.Contains(err.Error(), "systemctl enable --now") {
		t.Fatalf("错误 = %v", err)
	}
	// 单元留在盘上是有意的：报错里说清了失败在哪一步，把文件也撤掉只会让人
	// 下次看见"文件不存在"而不是"启用失败"。
	if _, statErr := os.Stat(filepath.Join(dir, TimerName(testScheduleID))); statErr != nil {
		t.Fatalf("单元文件应当保留以便排查: %v", statErr)
	}
}

func TestEnsureRejectsBadID(t *testing.T) {
	f := &fakeRunner{calendar: goodCalendar, show: enabledShow}
	inst, _ := newTestInstaller(t, f)
	if _, err := inst.Ensure(context.Background(), "../../etc/passwd", testDefinition()); !errors.Is(err, ErrBadScheduleID) {
		t.Fatalf("错误 = %v, 期望 ErrBadScheduleID", err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("坏 id 不该触发任何调用: %v", f.calls)
	}
}

func TestRemoveStopsThenDeletes(t *testing.T) {
	f := &fakeRunner{calendar: goodCalendar, show: enabledShow}
	inst, dir := newTestInstaller(t, f)
	if _, err := inst.Ensure(context.Background(), testScheduleID, testDefinition()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	f.calls = nil

	if err := inst.Remove(context.Background(), testScheduleID); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if !f.saw("disable --now " + TimerName(testScheduleID)) {
		t.Fatalf("没有停用 timer: %v", f.calls)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("单元文件没删干净: %v", entries)
	}
	// daemon-reload 必须在删之后：不然 systemd 还记着已经不存在的那两份文件。
	disableAt, reloadAt := f.indexOf("disable"), f.indexOf("daemon-reload")
	if disableAt < 0 || reloadAt < 0 || reloadAt < disableAt {
		t.Fatalf("调用顺序不对: %v", f.calls)
	}
}

func TestRemoveKeepsFilesWhenStopFails(t *testing.T) {
	f := &fakeRunner{calendar: goodCalendar, show: enabledShow, disableErr: errors.New("boom")}
	inst, dir := newTestInstaller(t, f)
	// 先铺好单元文件（不走 Ensure，避免 enable 那一串）。
	if err := writeAtomic(dir, TimerName(testScheduleID), []byte("[Timer]\n")); err != nil {
		t.Fatalf("铺 timer: %v", err)
	}
	if err := writeAtomic(dir, ServiceName(testScheduleID), []byte("[Service]\n")); err != nil {
		t.Fatalf("铺 service: %v", err)
	}

	if err := inst.Remove(context.Background(), testScheduleID); err == nil {
		t.Fatal("停不掉时必须报错")
	}
	if _, err := os.Stat(filepath.Join(dir, TimerName(testScheduleID))); err != nil {
		t.Fatalf("停不掉时不该删单元文件: %v", err)
	}
}

func TestRemoveWithoutUnitsOnlyReloads(t *testing.T) {
	f := &fakeRunner{}
	inst, _ := newTestInstaller(t, f)
	if err := inst.Remove(context.Background(), testScheduleID); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if f.saw("disable") {
		t.Fatalf("没有单元时不该调 disable: %v", f.calls)
	}
	if !f.saw("daemon-reload") {
		t.Fatalf("应当收尾一次 daemon-reload: %v", f.calls)
	}
}

func TestStatusReportsNotFoundWithoutUnits(t *testing.T) {
	f := &fakeRunner{show: "UnitFileState=\nActiveState=\nNextElapseUSecRealtime=n/a\n"}
	inst, _ := newTestInstaller(t, f)
	st, err := inst.Status(context.Background(), testScheduleID)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.Present {
		t.Fatal("没有单元文件却说存在")
	}
	if st.Enabled() || st.Active() {
		t.Fatalf("空状态被读成启用/运行中: %+v", st)
	}
	if st.UnitFileState != "not-found" || st.ActiveState != "unknown" {
		t.Fatalf("空字段没有归一: %+v", st)
	}
}

func TestParseCalendarOutput(t *testing.T) {
	check, err := parseCalendarOutput(goodCalendar)
	if err != nil {
		t.Fatalf("parseCalendarOutput: %v", err)
	}
	if check.Normalized != "*-*-* 03:30:00 Asia/Shanghai" {
		t.Fatalf("归一化 = %q", check.Normalized)
	}
	want := time.Date(2026, 10, 24, 19, 30, 0, 0, time.UTC)
	if !check.NextElapse.Equal(want) {
		t.Fatalf("下次触发 = %s, 期望 %s", check.NextElapse, want)
	}
	if _, err := parseCalendarOutput("Normalized form: x\n"); err == nil {
		t.Fatal("没有触发时刻必须报错")
	}
	if _, err := parseCalendarOutput("(in UTC): 2026-10-24 19:30:00 UTC\n"); err == nil {
		t.Fatal("没有归一化形式必须报错")
	}
}

func TestParseShowAndEpoch(t *testing.T) {
	fields := parseShow("A=1\nB=2\n\nC=\nbadline\n")
	if fields["A"] != "1" || fields["B"] != "2" || fields["C"] != "" {
		t.Fatalf("fields = %v", fields)
	}
	if _, ok := parseEpochMicros("n/a"); ok {
		t.Fatal("n/a 不该被当成时间戳")
	}
	if _, ok := parseEpochMicros("Sat 2026-10-24 19:30:00 UTC"); ok {
		t.Fatal("人类可读时间不该被当成时间戳")
	}
	got, ok := parseEpochMicros("1792000000000000")
	if !ok || got.UTC().IsZero() {
		t.Fatalf("微秒时间戳解析失败: %v %v", got, ok)
	}
}

func TestValidateCalendarPinsUTCAndReadsElapseWithoutInUTCLine(t *testing.T) {
	// systemd 在进程时区就是 UTC 时会省掉 "(in UTC)" 那一行——真机 24.04 的形状。
	f := &fakeRunner{calendar: `Normalized form: *-*-* 04:30:00
    Next elapse: Mon 2026-10-26 04:30:00 UTC
       From now: 22h left
`}
	inst, _ := newTestInstaller(t, f)

	check, err := inst.ValidateCalendar(context.Background(), testDefinition())
	if err != nil {
		t.Fatalf("UTC 形状的输出必须读得出来: %v", err)
	}
	if got, want := check.NextElapse.Format(time.RFC3339), "2026-10-26T04:30:00Z"; got != want {
		t.Fatalf("下一次触发 = %s，期望 %s", got, want)
	}
	if check.Normalized != "*-*-* 04:30:00" {
		t.Fatalf("归一化形式 = %q", check.Normalized)
	}
	// 时区必须钉成 UTC：不钉死的话，输出形状与读出来的时刻都随宿主机而变。
	pinned := false
	for _, env := range f.envs {
		for _, kv := range env {
			if kv == "TZ=UTC" {
				pinned = true
			}
		}
	}
	if !pinned {
		t.Fatal("调用 systemd-analyze 时必须带 TZ=UTC")
	}
}

func TestValidateCalendarRefusesForeignTimezoneAbbrev(t *testing.T) {
	// 只有 Next elapse、且后缀不是 UTC/GMT："CST" 不是偏移，Go 会把它读成偏移 0 的假时区。
	// 宁可判成"这个表达式不会再有触发时刻"，也不要拿一个差 8 小时的时刻去写单元。
	f := &fakeRunner{calendar: `Normalized form: *-*-* 04:30:00 Asia/Shanghai
    Next elapse: Mon 2026-10-26 04:30:00 CST
       From now: 22h left
`}
	inst, _ := newTestInstaller(t, f)
	if _, err := inst.ValidateCalendar(context.Background(), testDefinition()); err == nil {
		t.Fatal("读不出 UTC 时刻时必须报错，不能拿错时区顶替")
	}
}

func TestValidateCalendarPrefersInUTCLineOverWallClock(t *testing.T) {
	// 两行都在时以 "(in UTC)" 为准：墙上时间那行带的是本机时区缩写。
	f := &fakeRunner{calendar: `Normalized form: *-*-* 04:30:00 Asia/Shanghai
    Next elapse: Mon 2026-10-26 04:30:00 CST
       (in UTC): Sun 2026-10-25 20:30:00 UTC
       From now: 22h left
`}
	inst, _ := newTestInstaller(t, f)
	check, err := inst.ValidateCalendar(context.Background(), testDefinition())
	if err != nil {
		t.Fatalf("ValidateCalendar: %v", err)
	}
	if got, want := check.NextElapse.Format(time.RFC3339), "2026-10-25T20:30:00Z"; got != want {
		t.Fatalf("下一次触发 = %s，期望 %s", got, want)
	}
}
