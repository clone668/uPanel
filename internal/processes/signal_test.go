package processes

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeKiller 记录每一次“真的发信号”的调用。
//
// 它的存在只为回答一个问题：在拒绝路径上，信号到底有没有发出去。答案必须是“没有”。
type fakeKiller struct {
	mu       sync.Mutex
	calls    []killerCall
	onSignal func(pid int, signal string)
	err      error
}

type killerCall struct {
	pid    int
	signal string
}

func (k *fakeKiller) Signal(pid int, signal string) error {
	k.mu.Lock()
	k.calls = append(k.calls, killerCall{pid: pid, signal: signal})
	onSignal := k.onSignal
	err := k.err
	k.mu.Unlock()
	if err != nil {
		return err
	}
	if onSignal != nil {
		onSignal(pid, signal)
	}
	return nil
}

func (k *fakeKiller) callsMade() []killerCall {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]killerCall{}, k.calls...)
}

// newSignalCollector 构造一个响应很快的采集器（等待上限 30ms，轮询 5ms）。
func newSignalCollector(fs *fakeFS, killer Killer) *Collector {
	return NewCollector(Options{
		FS:             fs,
		Paths:          testPaths(),
		SelfPID:        424242,
		Interval:       5 * time.Second,
		Killer:         killer,
		VerifyTerm:     30 * time.Millisecond,
		VerifyKill:     30 * time.Millisecond,
		VerifyInterval: 5 * time.Millisecond,
	})
}

func blockerCodes(assessment Assessment) []string {
	out := make([]string, 0, len(assessment.Blockers))
	for _, blocker := range assessment.Blockers {
		out = append(out, blocker.Code)
	}
	return out
}

func warningCodes(assessment Assessment) []string {
	out := make([]string, 0, len(assessment.Warnings))
	for _, warning := range assessment.Warnings {
		out = append(out, warning.Code)
	}
	return out
}

func impactKinds(assessment Assessment) []string {
	out := make([]string, 0, len(assessment.Impacts))
	for _, impact := range assessment.Impacts {
		out = append(out, impact.Kind)
	}
	return out
}

func TestValidateParams(t *testing.T) {
	if err := ValidateParams(SignalParams{PID: 39515, StartTimeTicks: 277319, Signal: " term "}); err != nil {
		t.Fatalf("合法参数被判错：%v", err)
	}
	if err := ValidateParams(SignalParams{PID: 39515, StartTimeTicks: 277319, Signal: "KILL"}); err != nil {
		t.Fatalf("合法参数被判错：%v", err)
	}

	cases := []struct {
		name   string
		params SignalParams
		want   string
	}{
		{"pid 为零", SignalParams{PID: 0, StartTimeTicks: 1, Signal: "TERM"}, "pid"},
		{"pid 为负", SignalParams{PID: -5, StartTimeTicks: 1, Signal: "TERM"}, "pid"},
		{"pid 超上限", SignalParams{PID: 4194305, StartTimeTicks: 1, Signal: "TERM"}, "pid"},
		{"没有 starttime", SignalParams{PID: 39515, StartTimeTicks: 0, Signal: "TERM"}, "start_time_ticks"},
		{"信号不在闭集", SignalParams{PID: 39515, StartTimeTicks: 1, Signal: "HUP"}, "signal"},
		{"信号为空", SignalParams{PID: 39515, StartTimeTicks: 1, Signal: ""}, "signal"},
		{"信号是数字", SignalParams{PID: 39515, StartTimeTicks: 1, Signal: "9"}, "signal"},
	}
	for _, c := range cases {
		err := ValidateParams(c.params)
		if !errors.Is(err, ErrInvalidParams) {
			t.Fatalf("%s：期望 ErrInvalidParams，实际 %v", c.name, err)
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s：错误里没有提到 %q：%v", c.name, c.want, err)
		}
	}
}

func TestAssessNotFound(t *testing.T) {
	fs := newTestFS(t)
	c := newSignalCollector(fs, &fakeKiller{})

	assessment, err := c.Assess(t.Context(), SignalParams{PID: 3939393, StartTimeTicks: 7, Signal: "TERM"})
	if err != nil {
		t.Fatalf("Assess 返回错误：%v", err)
	}
	if assessment.Found {
		t.Fatalf("不存在的 PID 不该被判为存在")
	}
	if assessment.Allowed {
		t.Fatalf("不存在的 PID 不该允许发信号")
	}
	if codes := blockerCodes(assessment); len(codes) != 1 || codes[0] != CodeNotFound {
		t.Fatalf("阻塞项 = %v，期望 [%s]", codes, CodeNotFound)
	}
	if assessment.Identity != AbsentIdentity(3939393) {
		t.Fatalf("不存在时的身份指纹不对：%s", assessment.Identity)
	}
	if got := assessment.Locks; len(got) != 1 || got[0] != SignalLock(3939393, 7) {
		t.Fatalf("资源锁 = %v", got)
	}
}

func TestAssessIdentityMismatch(t *testing.T) {
	fs := newTestFS(t)
	c := newSignalCollector(fs, &fakeKiller{})

	// 真实现场是 starttime=277319；这里请求一个旧值，模拟 PID 已被重用。
	assessment, err := c.Assess(t.Context(), SignalParams{PID: 39515, StartTimeTicks: 100, Signal: "KILL"})
	if err != nil {
		t.Fatalf("Assess 返回错误：%v", err)
	}
	if assessment.Allowed {
		t.Fatalf("身份不匹配却允许发信号")
	}
	if codes := blockerCodes(assessment); len(codes) != 1 || codes[0] != CodeIdentityMismatch {
		t.Fatalf("阻塞项 = %v，期望 [%s]", codes, CodeIdentityMismatch)
	}
	// 身份指纹必须是**现存进程**的：请求的旧值重算出来与本值不同，旧预检因此会失效。
	if assessment.Identity != Identity(39515, 277319) {
		t.Fatalf("身份指纹 = %s，期望现存进程的 %s", assessment.Identity, Identity(39515, 277319))
	}
	if assessment.Identity == Identity(39515, 100) {
		t.Fatalf("身份指纹不该等于请求里的旧 starttime")
	}
	if assessment.Process.StartTimeTicks != 277319 {
		t.Fatalf("展示的进程信息应来自现存进程")
	}
	if kinds := impactKinds(assessment); len(kinds) != 1 || kinds[0] != ImpactNone {
		t.Fatalf("影响 = %v，期望 [none]", kinds)
	}
}

func TestAssessProtectedTargets(t *testing.T) {
	fs := newTestFS(t)
	c := newSignalCollector(fs, &fakeKiller{})

	cases := []struct {
		name string
		pid  int64
		code string
		note string
	}{
		{"PID 1", 1, CodeProtected, "init"},
		{"内核线程", 3, CodeProtected, "内核线程"},
		{"sshd", 860, CodeProtected, "ssh.service"},
	}
	for _, tc := range cases {
		row, ok := readRow(fs, testPaths(), tc.pid)
		if !ok {
			t.Fatalf("%s：样例读不到", tc.name)
		}
		assessment, err := c.Assess(t.Context(), SignalParams{
			PID: tc.pid, StartTimeTicks: row.startTick, Signal: "TERM",
		})
		if err != nil {
			t.Fatalf("%s：Assess 返回错误：%v", tc.name, err)
		}
		if assessment.Allowed {
			t.Fatalf("%s：受保护目标却允许发信号", tc.name)
		}
		if codes := blockerCodes(assessment); len(codes) != 1 || codes[0] != tc.code {
			t.Fatalf("%s：阻塞项 = %v，期望 [%s]", tc.name, codes, tc.code)
		}
		if !strings.Contains(assessment.Blockers[0].Message, tc.note) {
			t.Fatalf("%s：阻塞说明里没有提到 %q：%s", tc.name, tc.note, assessment.Blockers[0].Message)
		}
	}
}

func TestAssessAlreadyExited(t *testing.T) {
	fs := newTestFS(t)
	c := newSignalCollector(fs, &fakeKiller{})

	// 僵尸的 starttime 取自样例本身。
	row, _ := readRow(fs, testPaths(), 39503)
	assessment, err := c.Assess(t.Context(), SignalParams{PID: 39503, StartTimeTicks: row.startTick, Signal: "KILL"})
	if err != nil {
		t.Fatalf("Assess 返回错误：%v", err)
	}
	if assessment.Allowed {
		t.Fatalf("僵尸却允许发信号")
	}
	if codes := blockerCodes(assessment); len(codes) != 1 || codes[0] != CodeAlreadyExited {
		t.Fatalf("阻塞项 = %v，期望 [%s]", codes, CodeAlreadyExited)
	}
}

func TestAssessAllowedSignals(t *testing.T) {
	fs := newTestFS(t)
	c := newSignalCollector(fs, &fakeKiller{})

	term, err := c.Assess(t.Context(), SignalParams{PID: 39515, StartTimeTicks: 277319, Signal: "TERM"})
	if err != nil {
		t.Fatalf("Assess(TERM) 返回错误：%v", err)
	}
	if !term.Allowed || len(term.Blockers) != 0 {
		t.Fatalf("普通进程的 TERM 预检应允许：%+v", term)
	}
	if codes := warningCodes(term); len(codes) != 1 || codes[0] != WarningTerminate {
		t.Fatalf("TERM 告警 = %v，期望 [%s]", codes, WarningTerminate)
	}
	if kinds := impactKinds(term); len(kinds) != 2 || kinds[0] != ImpactTermination || kinds[1] != ImpactNone {
		t.Fatalf("TERM 影响 = %v", kinds)
	}
	// TERM 的说明必须明确“不会自动升级”。
	joined := term.Impacts[1].Description
	if !strings.Contains(joined, "单独发起") || !strings.Contains(joined, "SIGKILL") {
		t.Fatalf("TERM 影响说明没有提到需要单独发起强制结束：%q", joined)
	}

	kill, err := c.Assess(t.Context(), SignalParams{PID: 39515, StartTimeTicks: 277319, Signal: "kill"})
	if err != nil {
		t.Fatalf("Assess(KILL) 返回错误：%v", err)
	}
	if !kill.Allowed {
		t.Fatalf("普通进程的 KILL 预检应允许")
	}
	if codes := warningCodes(kill); len(codes) != 1 || codes[0] != WarningForceKill {
		t.Fatalf("KILL 告警 = %v，期望 [%s]", codes, WarningForceKill)
	}
	if kinds := impactKinds(kill); len(kinds) != 2 || kinds[0] != ImpactTermination || kinds[1] != ImpactDataLoss {
		t.Fatalf("KILL 影响 = %v，期望 [process_termination process_data_loss]", kinds)
	}
	if kill.Identity != term.Identity || kill.StateVersion != term.StateVersion {
		t.Fatalf("同一进程的两个信号应共享身份与状态版本")
	}
	if kill.Process.PID != 39515 || kill.DisplayName == "" {
		t.Fatalf("预检缺少目标信息：%+v", kill)
	}
}

func TestExecuteRefusesIdentityMismatchWithoutSignaling(t *testing.T) {
	fs := newTestFS(t)
	killer := &fakeKiller{}
	c := newSignalCollector(fs, killer)

	_, err := c.ExecuteSignal(t.Context(),
		SignalParams{PID: 39515, StartTimeTicks: 100, Signal: "KILL"}, nil)
	if !errors.Is(err, ErrSignalRejected) {
		t.Fatalf("期望 ErrSignalRejected，实际 %v", err)
	}
	if calls := killer.callsMade(); len(calls) != 0 {
		t.Fatalf("PID 重用后仍然发出了信号：%+v —— 这是绝不能发生的", calls)
	}
	if !strings.Contains(err.Error(), "starttime") {
		t.Fatalf("错误里应说明 starttime 变了：%v", err)
	}
}

func TestExecuteRefusesProtectedWithoutSignaling(t *testing.T) {
	fs := newTestFS(t)
	killer := &fakeKiller{}
	c := newSignalCollector(fs, killer)

	row, _ := readRow(fs, testPaths(), 1)
	if _, err := c.ExecuteSignal(t.Context(),
		SignalParams{PID: 1, StartTimeTicks: row.startTick, Signal: "KILL"}, nil); !errors.Is(err, ErrSignalRejected) {
		t.Fatalf("期望 ErrSignalRejected，实际 %v", err)
	}
	if calls := killer.callsMade(); len(calls) != 0 {
		t.Fatalf("受保护进程被发了信号：%+v", calls)
	}

	zombie, _ := readRow(fs, testPaths(), 39503)
	if _, err := c.ExecuteSignal(t.Context(),
		SignalParams{PID: 39503, StartTimeTicks: zombie.startTick, Signal: "TERM"}, nil); !errors.Is(err, ErrSignalRejected) {
		t.Fatalf("僵尸：期望 ErrSignalRejected，实际 %v", err)
	}
	if calls := killer.callsMade(); len(calls) != 0 {
		t.Fatalf("僵尸被发了信号：%+v", calls)
	}

	if _, err := c.ExecuteSignal(t.Context(),
		SignalParams{PID: 39515, StartTimeTicks: 277319, Signal: "STOP"}, nil); !errors.Is(err, ErrInvalidParams) {
		t.Fatalf("非法信号：期望 ErrInvalidParams，实际 %v", err)
	}
	if calls := killer.callsMade(); len(calls) != 0 {
		t.Fatalf("非法参数被发了信号：%+v", calls)
	}
}

func TestExecuteTERMGoneAfterSignal(t *testing.T) {
	fs := newTestFS(t)
	killer := &fakeKiller{onSignal: func(pid int, signal string) {
		// 进程收到 TERM 后自行退出：/proc/<pid> 消失。
		fs.remove("/proc/" + strconv.Itoa(pid) + "/stat")
	}}
	c := newSignalCollector(fs, killer)

	var steps []string
	res, err := c.ExecuteSignal(t.Context(),
		SignalParams{PID: 39515, StartTimeTicks: 277319, Signal: "TERM"},
		func(step string, _ map[string]any) { steps = append(steps, step) })
	if err != nil {
		t.Fatalf("ExecuteSignal 返回错误：%v", err)
	}
	if !res.Delivered {
		t.Fatalf("信号应已发出")
	}
	if !res.Gone {
		t.Fatalf("进程已消失，Gone 应为 true")
	}
	if res.StateAfter != "gone" {
		t.Fatalf("StateAfter = %q，期望 gone", res.StateAfter)
	}
	if res.Signal != SignalTERM || res.PID != 39515 || res.StartTimeTicks != 277319 {
		t.Fatalf("结果里丢了身份信息：%+v", res)
	}
	if res.StateBefore != StateSleeping {
		t.Fatalf("StateBefore = %q", res.StateBefore)
	}
	if calls := killer.callsMade(); len(calls) != 1 || calls[0].signal != "TERM" || calls[0].pid != 39515 {
		t.Fatalf("发出的信号 = %+v", calls)
	}
	if strings.Join(steps, ",") != "signal_term,verified" {
		t.Fatalf("进度步骤 = %v", steps)
	}
}

func TestExecuteKILLBecomesZombie(t *testing.T) {
	fs := newTestFS(t)
	killer := &fakeKiller{onSignal: func(pid int, _ string) {
		// 被 SIGKILL 结束但父进程还没回收：stat 仍在，状态变成 Z。
		fs.set("/proc/"+strconv.Itoa(pid)+"/stat",
			makeStat(39515, "sleep", 'Z', 39443, 15, 0, 277319))
	}}
	c := newSignalCollector(fs, killer)

	res, err := c.ExecuteSignal(t.Context(),
		SignalParams{PID: 39515, StartTimeTicks: 277319, Signal: "KILL"}, nil)
	if err != nil {
		t.Fatalf("ExecuteSignal 返回错误：%v", err)
	}
	if !res.Delivered || !res.Gone {
		t.Fatalf("结果 = %+v", res)
	}
	if res.StateAfter != StateZombie {
		t.Fatalf("StateAfter = %q，期望 zombie", res.StateAfter)
	}
}

func TestExecuteTERMTimeoutDoesNotEscalate(t *testing.T) {
	fs := newTestFS(t)
	// 进程忽略 TERM：信号发出去了，但它一直在。
	killer := &fakeKiller{}
	c := newSignalCollector(fs, killer)

	res, err := c.ExecuteSignal(t.Context(),
		SignalParams{PID: 39515, StartTimeTicks: 277319, Signal: "TERM"}, nil)
	if err == nil {
		t.Fatalf("等待超时必须返回错误，而不是假装成功")
	}
	if !res.Delivered || res.Gone {
		t.Fatalf("结果 = %+v，期望“发出但未退出”", res)
	}
	if !strings.Contains(err.Error(), "不会自动升级") {
		t.Fatalf("错误里必须说明不会自动升级为 SIGKILL：%v", err)
	}
	if !strings.Contains(err.Error(), "KILL 预检") {
		t.Fatalf("错误里应指引单独发起 KILL 预检：%v", err)
	}
	// 只发过一次信号，而且就是 TERM：没有任何自动升级。
	calls := killer.callsMade()
	if len(calls) != 1 || calls[0].signal != "TERM" {
		t.Fatalf("发出的信号 = %+v，期望只有一次 TERM", calls)
	}
	if res.StateAfter != StateSleeping {
		t.Fatalf("StateAfter = %q", res.StateAfter)
	}
}

func TestExecuteSignalReusedPIDDuringWait(t *testing.T) {
	fs := newTestFS(t)
	killer := &fakeKiller{onSignal: func(pid int, _ string) {
		// 极端情况：进程退出后 PID 立刻被别的进程拿走。
		fs.set("/proc/"+strconv.Itoa(pid)+"/stat",
			makeStat(39515, "python3", 'S', 1, 1, 1, 999999))
	}}
	c := newSignalCollector(fs, killer)

	res, err := c.ExecuteSignal(t.Context(),
		SignalParams{PID: 39515, StartTimeTicks: 277319, Signal: "TERM"}, nil)
	if err != nil {
		t.Fatalf("ExecuteSignal 返回错误：%v", err)
	}
	if !res.Gone {
		t.Fatalf("原进程已结束，Gone 应为 true")
	}
	if res.StateAfter != "gone" {
		t.Fatalf("StateAfter = %q", res.StateAfter)
	}
	// 新进程没有被碰过：只发了一次信号，而且是在身份校验通过之后。
	if calls := killer.callsMade(); len(calls) != 1 {
		t.Fatalf("发出的信号 = %+v", calls)
	}
}

func TestExecuteSignalCanceledContext(t *testing.T) {
	fs := newTestFS(t)
	killer := &fakeKiller{}
	c := newSignalCollector(fs, killer)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if _, err := c.ExecuteSignal(ctx,
		SignalParams{PID: 39515, StartTimeTicks: 277319, Signal: "TERM"}, nil); err == nil {
		t.Fatalf("已取消的上下文必须返回错误")
	}
	if calls := killer.callsMade(); len(calls) != 0 {
		t.Fatalf("上下文已取消却发了信号：%+v", calls)
	}
}

// 断言 DTO 里的两个指纹互不相同：进程身份与进程状态是两个问题，
// 混成一个会让“预检是否过期”与“目标是否还是同一个”无法分别判断。
func TestFingerprintsDiffer(t *testing.T) {
	identity := Identity(39515, 277319)
	version := StateVersion(39515, 277319)
	if identity == version {
		t.Fatalf("身份指纹与状态版本不该相同")
	}
	if !strings.HasPrefix(identity, "sha256:") || len(identity) != len("sha256:")+64 {
		t.Fatalf("身份指纹格式不对：%s", identity)
	}
	if !strings.HasPrefix(version, "sha256:") || len(version) != len("sha256:")+64 {
		t.Fatalf("状态版本格式不对：%s", version)
	}
	if Identity(39515, 277319) != identity {
		t.Fatalf("身份指纹必须稳定")
	}
	if Identity(39515, 277320) == identity {
		t.Fatalf("starttime 变了，身份指纹必须变")
	}
	if AbsentIdentity(39515) == identity || AbsentIdentity(39515) == AbsentIdentity(39516) {
		t.Fatalf("不存在身份必须与任何现存身份、以及其他 PID 的不存在身份都不同")
	}
	if got := SignalLock(39515, 277319); got != "process:39515:277319" {
		t.Fatalf("资源锁 = %q", got)
	}
	if fmt.Sprint(ValidSignal("TERM"), ValidSignal("KILL"), ValidSignal("HUP")) != "true true false" {
		t.Fatalf("信号闭集判断不对")
	}
}
