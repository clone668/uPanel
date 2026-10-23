package power

// 电源动作的单元测试（P11.3）。
//
// 这一组只验自己这一层的判决，不碰真系统：boot_id、systemd、在跑的任务、systemctl
// 都换成假的。要点有四条：
//
//  1. 关键任务在跑时**拒绝**，而且是阻塞项，不是告警；
//  2. 查不到在跑的任务时也拒绝——查不到不等于没有；
//  3. 倒计时期间可以取消，取消之后命令**没有**执行；
//  4. 倒计时结束前再查一次，此时有人开跑就放弃（POWER_ABORTED_JOBS_STARTED），
//     绝不把一次 dpkg 写库断在半路。
//
// 真机能不能重启、关机之后界面会不会撒谎，是验收脚本的事，不是这里的。

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

const (
	bootA = "ab2eb5e8-c1b4-4a00-a3d0-33ced7870abc"
	bootB = "9f4d2c17-6a55-4c2e-8f31-0b7e5a1d9c40"
)

// fakeInFlight 是“在跑的任务”来源。
type fakeInFlight struct {
	ops     []InFlight
	err     error
	calls   int
	exclude []string
}

func (f *fakeInFlight) InFlight(exclude string) ([]InFlight, error) {
	f.calls++
	f.exclude = append(f.exclude, exclude)
	if f.err != nil {
		return nil, f.err
	}
	// 与 runner.runningOperations 同义：exclude 指的那个操作不算“在跑”。
	out := make([]InFlight, 0, len(f.ops))
	for _, op := range f.ops {
		if op.OperationID == exclude {
			continue
		}
		out = append(out, op)
	}
	return out, nil
}

// harness 把一台“正常的 Ubuntu”装起来：boot_id 可读、systemd 在、systemctl 在，
// 倒计时不真的等（把时钟往前拨）。
type harness struct {
	exec     *Executor
	sleeps   []time.Duration
	commands []string
	now      time.Time
	inFlight *fakeInFlight
}

type harnessOpts struct {
	bootID    string
	bootErr   error
	uptime    string
	systemd   bool
	systemctl bool
	rebootReq bool
	// inFlight 为 nil 时用一台“没有任务在跑”的来源。
	inFlight *fakeInFlight
	// result / runErr 是 fake systemctl 的返回。
	result CommandResult
	runErr error
}

func newHarness(o harnessOpts) *harness {
	h := &harness{now: time.Date(2026, 10, 23, 10, 0, 0, 0, time.UTC)}
	if o.bootID == "" && o.bootErr == nil {
		o.bootID = bootA
	}
	if o.uptime == "" {
		o.uptime = "86400.5 345678.90\n"
	}
	if o.inFlight == nil {
		o.inFlight = &fakeInFlight{}
	}
	h.inFlight = o.inFlight
	h.exec = New(Options{
		Paths: Paths{
			Systemctl:  "/usr/bin/systemctl",
			BootID:     "/proc/sys/kernel/random/boot_id",
			SystemdRun: "/run/systemd/system",
			Uptime:     "/proc/uptime",
		},
		Run: func(_ context.Context, program string, args []string, _ time.Duration) (CommandResult, error) {
			h.commands = append(h.commands, program+" "+strings.Join(args, " "))
			res := o.result
			res.Program = program
			return res, o.runErr
		},
		Now: func() time.Time { return h.now },
		Sleep: func(_ context.Context, d time.Duration) error {
			h.sleeps = append(h.sleeps, d)
			h.now = h.now.Add(d)
			return nil
		},
		ReadFile: func(path string) ([]byte, error) {
			switch path {
			case "/proc/sys/kernel/random/boot_id":
				if o.bootErr != nil {
					return nil, o.bootErr
				}
				return []byte(o.bootID + "\n"), nil
			case "/proc/uptime":
				return []byte(o.uptime), nil
			}
			return nil, errors.New("测试里没有这个文件：" + path)
		},
		Stat: func(path string) bool {
			switch path {
			case "/run/systemd/system":
				return o.systemd
			case "/usr/bin/systemctl":
				return o.systemctl
			case "/run/reboot-required":
				return o.rebootReq
			}
			return false
		},
		InFlight: o.inFlight,
	})
	return h
}

func rebootParams(delay int) Params   { return Params{Operation: OpReboot, DelaySeconds: delay} }
func poweroffParams(delay int) Params { return Params{Operation: OpPoweroff, DelaySeconds: delay} }

func hasCode(reasons []Reason, code string) bool {
	for _, r := range reasons {
		if r.Code == code {
			return true
		}
	}
	return false
}

func TestParamsValidate(t *testing.T) {
	valid := []Params{
		{Operation: OpReboot, DelaySeconds: 30},
		{Operation: OpReboot, DelaySeconds: 300},
		{Operation: OpPoweroff, DelaySeconds: 60},
	}
	for _, p := range valid {
		if err := p.Validate(); err != nil {
			t.Errorf("%+v 应当合法：%v", p, err)
		}
	}
	// 0 秒（没给 delay）刻意不算合法：省略默认值会把“我以为它立刻重启”和
	// “它其实 30 秒后才重启”混成同一个请求。
	invalid := []Params{
		{Operation: OpReboot, DelaySeconds: 0},
		{Operation: OpReboot, DelaySeconds: 29},
		{Operation: OpReboot, DelaySeconds: 301},
		{Operation: Op("suspend"), DelaySeconds: 60},
		{Operation: Op(""), DelaySeconds: 60},
	}
	for _, p := range invalid {
		if err := p.Validate(); err == nil {
			t.Errorf("%+v 应当被拒", p)
		}
	}
}

func TestAssessAllowsQuietSystem(t *testing.T) {
	h := newHarness(harnessOpts{systemd: true, systemctl: true})
	a, err := h.exec.Assess(context.Background(), AssessRequest{Params: rebootParams(60)})
	if err != nil {
		t.Fatalf("Assess 失败: %v", err)
	}
	if !a.Allowed || len(a.Blockers) != 0 {
		t.Fatalf("干净的系统不该有阻塞项：%+v", a.Blockers)
	}
	// 状态版本必须是 boot_id：预检之后机器要是换过一次启动，这次预检就作废。
	if a.Target.StateVersion != bootA {
		t.Errorf("StateVersion 必须是 boot_id，实际 %q", a.Target.StateVersion)
	}
	if len(a.Locks) != 1 || a.Locks[0] != LockName {
		t.Errorf("锁 = %v，期望 [%s]", a.Locks, LockName)
	}
	kinds := map[string]bool{}
	for _, i := range a.Impacts {
		kinds[i.Kind] = true
	}
	if !kinds[ImpactConnection] || !kinds[ImpactPowerState] {
		t.Errorf("影响清单缺项：%+v", a.Impacts)
	}
	if !kinds["reboot_confirmation"] {
		t.Error("重启应当说明“之后用 boot_id 确认”")
	}
}

func TestAssessBlocksOnRunningJobs(t *testing.T) {
	src := &fakeInFlight{ops: []InFlight{
		{OperationID: "op_bbbb", Action: "package.remove"},
		{OperationID: "op_aaaa", Action: "storage.format"},
	}}
	h := newHarness(harnessOpts{systemd: true, systemctl: true, inFlight: src})
	a, err := h.exec.Assess(context.Background(), AssessRequest{Params: poweroffParams(60)})
	if err != nil {
		t.Fatalf("Assess 失败: %v", err)
	}
	if a.Allowed {
		t.Fatal("有写任务在跑时必须拒绝电源操作")
	}
	if !hasCode(a.Blockers, CodeJobsRunning) {
		t.Fatalf("阻塞项 = %+v，期望含 %s", a.Blockers, CodeJobsRunning)
	}
	// 消息里点名是哪个动作在跑：只说“有任务在跑”帮不上忙。
	msg := a.Blockers[0].Message
	if !strings.Contains(msg, "package.remove") || !strings.Contains(msg, "storage.format") {
		t.Errorf("阻塞消息应点名在跑的动作为：%q", msg)
	}
	// 按 operation_id 排序：报出来的顺序不能跟着数据库的心情变。
	if len(a.InFlight) != 2 || a.InFlight[0].OperationID != "op_aaaa" {
		t.Errorf("InFlight 应按 operation_id 排序：%+v", a.InFlight)
	}
}

// TestAssessExcludesSelfWhenRevalidating：执行前复核时，发起这次复核的操作自己
// 就在“在跑的任务”里，必须排掉。
//
// 这是真机故障的回归测试。从前 Assess 固定调 InFlight("")，于是每个 system.power
// 任务都在 revalidate 阶段撞见自己，被 POWER_JOBS_RUNNING 拒掉；而它之前的预检
// 是 allowed——表面上“预检放行、提交必失败”，还被误读成“关键任务闸门失效”。
func TestAssessExcludesSelfWhenRevalidating(t *testing.T) {
	src := &fakeInFlight{ops: []InFlight{{OperationID: "op_self", Action: ActionPower}}}
	h := newHarness(harnessOpts{systemd: true, systemctl: true, inFlight: src})

	// 一、预检（不排除任何人）：自己也在跑，所以被拦——闸门方向没错。
	a, err := h.exec.Assess(context.Background(), AssessRequest{Params: rebootParams(60)})
	if err != nil {
		t.Fatalf("Assess 失败: %v", err)
	}
	if a.Allowed || !hasCode(a.Blockers, CodeJobsRunning) {
		t.Fatalf("别处跑着写操作时必须拦：%+v", a.Blockers)
	}

	// 二、执行前复核（排除自己）：放行，且 InFlight 为空。
	a, err = h.exec.Assess(context.Background(), AssessRequest{
		Params:             rebootParams(60),
		ExcludeOperationID: "op_self",
	})
	if err != nil {
		t.Fatalf("Assess 失败: %v", err)
	}
	if !a.Allowed || len(a.Blockers) != 0 {
		t.Fatalf("复核时不该把自己算成关键任务：%+v", a.Blockers)
	}
	if len(a.InFlight) != 0 {
		t.Errorf("InFlight 应已排除自己：%+v", a.InFlight)
	}

	// 三、排自己归排自己，别人照样拦。
	src.ops = append(src.ops, InFlight{OperationID: "op_other", Action: "package.install"})
	a, err = h.exec.Assess(context.Background(), AssessRequest{
		Params:             rebootParams(60),
		ExcludeOperationID: "op_self",
	})
	if err != nil {
		t.Fatalf("Assess 失败: %v", err)
	}
	if a.Allowed || !hasCode(a.Blockers, CodeJobsRunning) {
		t.Fatalf("别的写操作在跑时仍然要拦：%+v", a.Blockers)
	}
	if len(a.InFlight) != 1 || a.InFlight[0].OperationID != "op_other" {
		t.Errorf("只该留下别人：%+v", a.InFlight)
	}
}

func TestAssessBlocksWhenJobsUnknown(t *testing.T) {
	// 一、来源报错：查不到 ≠ 没有。
	h := newHarness(harnessOpts{
		systemd: true, systemctl: true,
		inFlight: &fakeInFlight{err: errors.New("sqlite 锁住了")},
	})
	a, err := h.exec.Assess(context.Background(), AssessRequest{Params: rebootParams(60)})
	if err != nil {
		t.Fatalf("Assess 失败: %v", err)
	}
	if a.Allowed || !hasCode(a.Blockers, CodeJobsUnknown) {
		t.Fatalf("查不到在跑的任务必须拦下：%+v", a.Blockers)
	}
	if !strings.Contains(a.Blockers[0].Message, "sqlite 锁住了") {
		t.Errorf("原因要带出来：%q", a.Blockers[0].Message)
	}

	// 二、来源根本没接上（nil）：同样拦，且不说成“没有任务”。
	e := New(Options{
		Paths:    Paths{Systemctl: "/usr/bin/systemctl", BootID: "/proc/sys/kernel/random/boot_id"},
		ReadFile: func(string) ([]byte, error) { return []byte(bootA), nil },
		Stat:     func(string) bool { return true },
	})
	a, err = e.Assess(context.Background(), AssessRequest{Params: rebootParams(60)})
	if err != nil {
		t.Fatalf("Assess 失败: %v", err)
	}
	if a.Allowed || !hasCode(a.Blockers, CodeJobsUnknown) {
		t.Fatalf("没接上来源也必须拦下：%+v", a.Blockers)
	}
}

func TestAssessBlocksOnMissingBootIDOrSystemd(t *testing.T) {
	// boot_id 读不出来：做可以，但做完说不清有没有发生 → 拦。
	h := newHarness(harnessOpts{
		bootErr:   errors.New("permission denied"),
		systemd:   true,
		systemctl: true,
	})
	a, _ := h.exec.Assess(context.Background(), AssessRequest{Params: rebootParams(60)})
	if a.Allowed || !hasCode(a.Blockers, CodeBootIDUnreadable) {
		t.Fatalf("boot_id 读不出来必须拦：%+v", a.Blockers)
	}

	// 没有 systemd（容器、chroot）：不提供替代路径。
	h = newHarness(harnessOpts{systemd: false, systemctl: true})
	a, _ = h.exec.Assess(context.Background(), AssessRequest{Params: rebootParams(60)})
	if a.Allowed || !hasCode(a.Blockers, CodeUnsupported) {
		t.Fatalf("没有 systemd 必须拦：%+v", a.Blockers)
	}

	// systemctl 不在也一样。
	h = newHarness(harnessOpts{systemd: true, systemctl: false})
	a, _ = h.exec.Assess(context.Background(), AssessRequest{Params: rebootParams(60)})
	if a.Allowed || !hasCode(a.Blockers, CodeUnsupported) {
		t.Fatalf("没有 systemctl 必须拦：%+v", a.Blockers)
	}
}

func TestAssessWarnings(t *testing.T) {
	// 关机：必须提醒“不会自己回来”。
	h := newHarness(harnessOpts{systemd: true, systemctl: true})
	a, _ := h.exec.Assess(context.Background(), AssessRequest{Params: poweroffParams(60)})
	if !hasCode(a.Warnings, "poweroff_needs_manual_start") {
		t.Errorf("关机的告警缺了：%+v", a.Warnings)
	}

	// 重启且系统本来就有重启标志：一并说清。
	h = newHarness(harnessOpts{systemd: true, systemctl: true, rebootReq: true})
	a, _ = h.exec.Assess(context.Background(), AssessRequest{Params: rebootParams(60)})
	if !hasCode(a.Warnings, "reboot_pending_elsewhere") {
		t.Errorf("重启告警缺了：%+v", a.Warnings)
	}

	// 刚启动不到 10 分钟：提醒确认不是要重启第二次。
	h = newHarness(harnessOpts{systemd: true, systemctl: true, uptime: "120.00 300.00\n"})
	a, _ = h.exec.Assess(context.Background(), AssessRequest{Params: rebootParams(60)})
	if !hasCode(a.Warnings, "recently_booted") {
		t.Errorf("刚启动的告警缺了：%+v", a.Warnings)
	}
}

func TestReadBootIDRejectsTruncated(t *testing.T) {
	// 截断的 boot_id 被收下，重启前后的比对就会在两个同样截断的值上判等——
	// 那等于没比对，而且看起来像比对过了。
	for _, bad := range []string{"", "  \n", "ab2eb5e8", "ab2eb5e8-c1b4-4a00-a3d0", "not-a-uuid"} {
		h := newHarness(harnessOpts{systemd: true, systemctl: true})
		h.exec.readFile = func(string) ([]byte, error) { return []byte(bad), nil }
		if _, err := h.exec.ReadBootID(); err == nil {
			t.Errorf("%q 应当被拒", bad)
		}
	}

	h := newHarness(harnessOpts{systemd: true, systemctl: true})
	h.exec.readFile = func(string) ([]byte, error) {
		return []byte(strings.ToUpper(bootB) + "\n"), nil
	}
	id, err := h.exec.ReadBootID()
	if err != nil {
		t.Fatalf("大写的 boot_id 应当接受：%v", err)
	}
	if id != bootB {
		t.Errorf("应当规范化成小写：%q", id)
	}
}

func TestParseUptime(t *testing.T) {
	if d, err := parseUptime([]byte("86400.5 345678.90\n")); err != nil ||
		d != 86400*time.Second+500*time.Millisecond {
		t.Errorf("解析结果 = %v, %v", d, err)
	}
	for _, bad := range []string{"", "   \n", "abc 123\n", "-5 1\n"} {
		if _, err := parseUptime([]byte(bad)); err == nil {
			t.Errorf("%q 应当解析失败", bad)
		}
	}
}

func TestExecuteIssuesCommandAfterCountdown(t *testing.T) {
	h := newHarness(harnessOpts{systemd: true, systemctl: true})
	var steps []string
	ex, err := h.exec.Execute(context.Background(), ExecuteRequest{
		Params:      rebootParams(30),
		OperationID: "op_selftest",
		Progress: func(step string, fields map[string]any) {
			steps = append(steps, step)
			if step == "countdown" {
				if _, ok := fields["remaining_seconds"]; !ok {
					t.Errorf("倒计时事件必须带 remaining_seconds：%+v", fields)
				}
				if fields["boot_id_before"] != bootA {
					t.Errorf("倒计时事件要带上下发前的 boot_id：%+v", fields)
				}
			}
		},
	})
	if err != nil {
		t.Fatalf("Execute 失败: %v", err)
	}
	if ex.Outcome != OutcomeAccepted {
		t.Errorf("结论只能是 accepted，实际 %q", ex.Outcome)
	}
	if ex.BootIDBefore != bootA {
		t.Errorf("BootIDBefore = %q", ex.BootIDBefore)
	}
	if len(h.commands) != 1 || h.commands[0] != "/usr/bin/systemctl reboot" {
		t.Fatalf("应当只下发一次 systemctl reboot，实际 %v", h.commands)
	}
	if ex.Command != "/usr/bin/systemctl reboot" {
		t.Errorf("Command = %q", ex.Command)
	}
	if ex.IssuedAt.IsZero() || ex.Canceled {
		t.Errorf("IssuedAt/Canceled = %v/%v", ex.IssuedAt, ex.Canceled)
	}
	// 30 秒被切成 6 个 5 秒的等待；每秒回报会把事件表灌满。
	if len(h.sleeps) != 6 {
		t.Errorf("30 秒倒计时应等 6 次，实际 %d 次：%v", len(h.sleeps), h.sleeps)
	}
	if len(steps) < 2 || steps[0] != "countdown" || steps[len(steps)-1] != "countdown" {
		t.Errorf("进度步骤 = %v", steps)
	}
	// 结束前那次检查要排除自己，否则会把自己的任务当成“在跑的任务”。
	if len(h.inFlight.exclude) != 1 || h.inFlight.exclude[0] != "op_selftest" {
		t.Errorf("执行前的复查应当排除自己：%v", h.inFlight.exclude)
	}
}

func TestExecuteCancelDuringCountdown(t *testing.T) {
	h := newHarness(harnessOpts{systemd: true, systemctl: true})
	cancel := make(chan struct{})
	close(cancel) // 一开始就取消：倒计时第一轮就该退出
	var steps []string
	ex, err := h.exec.Execute(context.Background(), ExecuteRequest{
		Params: rebootParams(300),
		Cancel: cancel,
		Progress: func(step string, _ map[string]any) {
			steps = append(steps, step)
		},
	})
	if err != nil {
		t.Fatalf("取消不是错误：%v", err)
	}
	if !ex.Canceled {
		t.Fatal("应当标记为已取消")
	}
	if !ex.IssuedAt.IsZero() {
		t.Error("取消时 IssuedAt 必须是零值（命令没有下发）")
	}
	if len(h.commands) != 0 {
		t.Fatalf("取消之后绝不能再下发命令：%v", h.commands)
	}
	if len(h.sleeps) != 0 {
		t.Errorf("取消应当立刻生效，不该先等一轮：%v", h.sleeps)
	}
	if len(steps) == 0 || steps[len(steps)-1] != "canceled" {
		t.Errorf("应当回报一次 canceled 进度：%v", steps)
	}
}

func TestExecuteAbortsWhenJobStartsDuringCountdown(t *testing.T) {
	// 预检时干净，倒计时结束时 package.remove 在跑：放弃，且不下发命令。
	h := newHarness(harnessOpts{
		systemd: true, systemctl: true,
		inFlight: &fakeInFlight{ops: []InFlight{{OperationID: "op_pkg", Action: "package.remove"}}},
	})
	ex, err := h.exec.Execute(context.Background(), ExecuteRequest{
		Params: rebootParams(300), OperationID: "op_self",
	})
	if err == nil {
		t.Fatal("有写任务在跑时必须放弃")
	}
	if !strings.Contains(err.Error(), CodeAbortedByJob) {
		t.Errorf("错误码应当是 %s：%v", CodeAbortedByJob, err)
	}
	if !strings.Contains(err.Error(), "package.remove") {
		t.Errorf("放弃的理由要点名是哪个动作：%v", err)
	}
	if len(h.commands) != 0 {
		t.Fatalf("放弃之后不能下发命令：%v", h.commands)
	}
	if ex.BootIDBefore != bootA || ex.Outcome != OutcomeAccepted {
		t.Errorf("返回的 Execution 仍应带着“本来要做什么”：%+v", ex)
	}
}

func TestExecuteFailsWhenInFlightUnknownAtIssue(t *testing.T) {
	h := newHarness(harnessOpts{
		systemd: true, systemctl: true,
		inFlight: &fakeInFlight{err: errors.New("库读失败")},
	})
	_, err := h.exec.Execute(context.Background(), ExecuteRequest{Params: rebootParams(30)})
	if err == nil || !strings.Contains(err.Error(), CodeJobsUnknown) {
		t.Fatalf("查不到在跑的任务时不能下发命令：%v", err)
	}
	if len(h.commands) != 0 {
		t.Errorf("不该下发任何命令：%v", h.commands)
	}
}

func TestExecuteReportsSystemctlFailure(t *testing.T) {
	h := newHarness(harnessOpts{
		systemd: true, systemctl: true,
		result: CommandResult{ExitCode: 1, Stderr: "Interactive authentication required.\n"},
	})
	ex, err := h.exec.Execute(context.Background(), ExecuteRequest{Params: poweroffParams(30)})
	if err == nil {
		t.Fatal("systemctl 失败必须返回错误")
	}
	if !strings.Contains(err.Error(), CodeIssueFailed) {
		t.Errorf("错误码应当是 %s：%v", CodeIssueFailed, err)
	}
	// 消息里要有能照做的信息：退出码与原始 stderr 摘要。
	if !strings.Contains(err.Error(), "退出码 1") ||
		!strings.Contains(err.Error(), "Interactive authentication required") {
		t.Errorf("失败消息太含糊：%v", err)
	}
	if ex.IssuedAt.IsZero() {
		t.Error("命令确实下发过了，IssuedAt 不该是零值")
	}
}

func TestExecuteRequiresCommandRunner(t *testing.T) {
	e := New(Options{
		Paths:    Paths{Systemctl: "/usr/bin/systemctl", BootID: "/proc/sys/kernel/random/boot_id"},
		ReadFile: func(string) ([]byte, error) { return []byte(bootA), nil },
		Sleep:    func(context.Context, time.Duration) error { return nil },
		InFlight: &fakeInFlight{},
	})
	_, err := e.Execute(context.Background(), ExecuteRequest{Params: rebootParams(30)})
	if err == nil || !strings.Contains(err.Error(), CodeIssueFailed) {
		t.Fatalf("没接执行器时应当明确失败：%v", err)
	}
}

func TestExecuteRefusesUnreadableBootID(t *testing.T) {
	h := newHarness(harnessOpts{
		bootErr: errors.New("permission denied"),
		systemd: true, systemctl: true,
	})
	_, err := h.exec.Execute(context.Background(), ExecuteRequest{Params: rebootParams(30)})
	if err == nil || !strings.Contains(err.Error(), CodeBootIDUnreadable) {
		t.Fatalf("读不到 boot_id 就不能做（做完说不清有没有发生）：%v", err)
	}
	if len(h.commands) != 0 {
		t.Errorf("不该下发任何命令：%v", h.commands)
	}
}

func TestDescribeIssueFailure(t *testing.T) {
	if got := describeIssueFailure("/usr/bin/systemctl reboot",
		CommandResult{TimedOut: true}, nil); !strings.Contains(got, "没有返回") {
		t.Errorf("超时的说法 = %q", got)
	}
	if got := describeIssueFailure("/usr/bin/systemctl reboot",
		CommandResult{ExitCode: 2}, nil); !strings.Contains(got, "没有输出") {
		t.Errorf("无输出的说法 = %q", got)
	}
	long := describeIssueFailure("/usr/bin/systemctl reboot",
		CommandResult{ExitCode: 1, Stderr: strings.Repeat("x", 400)}, nil)
	if !strings.Contains(long, "…") {
		t.Errorf("过长的 stderr 应当截断：%q", long)
	}
	if len(long) > 400 {
		t.Errorf("截断之后仍过长：%d 字节", len(long))
	}
}

func TestDescribeInFlightCapsList(t *testing.T) {
	msg := describeInFlight([]InFlight{
		{OperationID: "op_1", Action: "package.install"},
		{OperationID: "op_2", Action: "storage.format"},
		{OperationID: "op_3", Action: "share.apply"},
		{OperationID: "op_4", Action: "certificate.issue"},
	})
	if !strings.Contains(msg, "等共 4 个") {
		t.Errorf("超过三个要说清总数：%q", msg)
	}
	if strings.Contains(msg, "certificate.issue") {
		t.Errorf("只列前三个：%q", msg)
	}
}
