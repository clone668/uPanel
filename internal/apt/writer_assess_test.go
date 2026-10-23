package apt

// 预检与执行的单元测试（P11.2）。
//
// 三条贯穿始终的断言：
//
//   - 锁被占着时**一次 apt-get 都不跑**（不是跑了之后报错）；
//   - 执行后的结论来自**读回的 dpkg 事实**，不是退出码；
//   - 每一种阻塞项都点名到包、每一种告警都能被人读懂。
//
// 模拟输出全部来自真机抓取（testdata），只有 dpkg 状态与锁状态是造的——那两样
// 在开发宿主上造不出来，而且在真机上也无法重复摆放。

import (
	"context"
	"errors"
	"io/fs"
	"strings"
	"sync"
	"testing"
	"time"

	"upanel/internal/syscmd"
)

// fakeApt 是一台"可以摆状态的机器"。
type fakeApt struct {
	simulateOut  string
	simulateExit int
	execOut      string
	execErrOut   string
	execExit     int
	// execSignal 非空表示执行阶段不是"apt 自己失败"，而是被这个信号停住
	// （退出码 -1、Signal 有值）。硬杀场景靠它造。
	execSignal    string
	refreshSignal string
	dpkgSequences []string // 第 n 次 dpkg-query 的输出；用完之后重复最后一份
	refreshOut    string
	refreshExit   int
	listsMTime    time.Time
	// mtimeSeq 让索引时间可以"刷一次就往前走"（refresh 的验证需要）。
	mtimeSeq []time.Time

	mu         sync.Mutex
	dpkgCalls  int
	mtimeCalls int
	commands   []string
}

func (f *fakeApt) run(_ context.Context, spec syscmd.Spec, _ syscmd.Limits) (syscmd.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, strings.Join(append([]string{spec.Program}, spec.Args...), " "))

	switch {
	case strings.HasSuffix(spec.Program, "dpkg-query"):
		seq := f.dpkgSequence()
		f.dpkgCalls++
		return syscmd.Result{Program: spec.Program, Stdout: seq, ExitCode: 0}, nil
	case strings.HasSuffix(spec.Program, "apt-get"):
		if hasArg(spec.Args, "-s") {
			return f.exit(spec.Program, f.simulateOut, f.simulateExit)
		}
		if hasArg(spec.Args, "update") {
			if f.refreshSignal != "" {
				return killed(spec.Program, f.refreshOut, f.refreshSignal)
			}
			return f.exit(spec.Program, f.refreshOut, f.refreshExit)
		}
		if f.execSignal != "" {
			return killed(spec.Program, f.execOut+f.execErrOut, f.execSignal)
		}
		return f.exit(spec.Program, f.execOut+f.execErrOut, f.execExit)
	}
	return syscmd.Result{}, errors.New("fakeApt: 没预料到的程序 " + spec.Program)
}

func (f *fakeApt) exit(program, out string, code int) (syscmd.Result, error) {
	res := syscmd.Result{Program: program, Stdout: out, ExitCode: code}
	if code != 0 {
		return res, &syscmd.ExitError{Program: program, ExitCode: code}
	}
	return res, nil
}

// killed 造一次"被信号停住"的执行结果：退出码 -1、Signal 有值。
// syscmd 在真机上也是这样填的（见 internal/syscmd 的 signalName）。
func killed(program, out, signal string) (syscmd.Result, error) {
	res := syscmd.Result{Program: program, Stdout: out, ExitCode: -1, Signal: signal}
	return res, &syscmd.ExitError{Program: program, ExitCode: -1}
}

func (f *fakeApt) dpkgSequence() string {
	if len(f.dpkgSequences) == 0 {
		return ""
	}
	idx := f.dpkgCalls
	if idx >= len(f.dpkgSequences) {
		idx = len(f.dpkgSequences) - 1
	}
	return f.dpkgSequences[idx]
}

func (f *fakeApt) stat(string) (fs.FileInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.mtimeSeq) > 0 {
		idx := f.mtimeCalls
		if idx >= len(f.mtimeSeq) {
			idx = len(f.mtimeSeq) - 1
		}
		f.mtimeCalls++
		return fakeInfo{name: "lists", mtime: f.mtimeSeq[idx], isDir: true}, nil
	}
	if f.listsMTime.IsZero() {
		return nil, fs.ErrNotExist
	}
	return fakeInfo{name: "lists", mtime: f.listsMTime, isDir: true}, nil
}

// callsMatching 返回包含子串的命令。
func (f *fakeApt) callsMatching(sub string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, 2)
	for _, cmd := range f.commands {
		if strings.Contains(cmd, sub) {
			out = append(out, cmd)
		}
	}
	return out
}

func hasArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

// dpkgLine 拼一行 dpkg-query 的输出（真机格式：`名<TAB>状态<TAB>版本<TAB>Essential`）。
func dpkgLine(name, status, version string, essential bool) string {
	flag := "no"
	if essential {
		flag = "yes"
	}
	return name + "\t" + status + "\t" + version + "\t" + flag + "\n"
}

// baseState 是这台假机器上的常备包。
func baseState() string {
	return dpkgLine("bash", "ii ", "5.2.21-2ubuntu4", true) +
		dpkgLine("zlib1g", "ii ", "1:1.3.dfsg-3.1ubuntu2.2", false) +
		dpkgLine("openssh-server", "ii ", "1:9.6p1-3ubuntu13.20", false) +
		dpkgLine("ca-certificates", "ii ", "20260816ubuntu0.24.04.1", false) +
		dpkgLine("netcat-openbsd", "ii ", "1.226-1ubuntu2", false)
}

// newTestWriter 造一个 Writer，锁默认"没人占用"。
func newTestWriter(f *fakeApt, now time.Time) *Writer {
	return NewWriter(WriterOptions{
		Paths: DefaultPaths(),
		Run:   f.run,
		Now:   func() time.Time { return now },
		Stat:  f.stat,
		Locks: func(paths []string) []LockState {
			out := make([]LockState, 0, len(paths))
			for _, path := range paths {
				out = append(out, LockState{Path: path})
			}
			return out
		},
	})
}

var testNow = time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)

// ---------- 预检 ----------

// TestAssessInstallNew：装一个没装的包。
func TestAssessInstallNew(t *testing.T) {
	f := &fakeApt{
		simulateOut:   readCapture(t, "2404-sim-install-new.txt"),
		dpkgSequences: []string{baseState()},
		listsMTime:    testNow.Add(-2 * time.Hour),
	}
	w := newTestWriter(f, testNow)

	got, err := w.Assess(context.Background(), OpInstall, []string{"cowsay"})
	if err != nil {
		t.Fatalf("预检报错: %v", err)
	}
	if !got.Allowed {
		t.Fatalf("应当放行，阻塞项: %+v", got.Blockers)
	}
	if got.DisplayName != "安装 cowsay" {
		t.Fatalf("DisplayName 错: %q", got.DisplayName)
	}
	if len(got.Impacts) != 1 || got.Impacts[0].Kind != ImpactSoftware {
		t.Fatalf("影响类别错: %+v", got.Impacts)
	}
	if !strings.Contains(got.Impacts[0].Description, "安装 1 个") {
		t.Fatalf("影响描述没说清装几个: %q", got.Impacts[0].Description)
	}
	if got.Protected != nil || got.Collateral != nil {
		t.Fatalf("装新包不该有保护/连带条目: %v %v", got.Protected, got.Collateral)
	}
	if len(got.Locks) != 1 || got.Locks[0] != "apt-global" {
		t.Fatalf("资源锁应为 apt-global: %v", got.Locks)
	}
	// 预检只能跑模拟，不能跑真正的写命令。
	if calls := f.callsMatching("apt-get -y"); len(calls) != 0 {
		t.Fatalf("预检不该执行写命令: %v", calls)
	}
	sim := f.callsMatching("apt-get -s")
	if len(sim) != 1 {
		t.Fatalf("应当恰好跑一次模拟，实际 %v", sim)
	}
	// 非交互参数必须在模拟里就带上：预检与执行验的必须是同一条命令。
	for _, want := range []string{"-y", "--force-confdef", "--force-confold", "Dpkg::Use-Pty=0"} {
		if !strings.Contains(sim[0], want) {
			t.Fatalf("模拟命令缺 %s: %s", want, sim[0])
		}
	}
}

// TestAssessBusyLock：锁被占着就拒绝，而且**连模拟都不跑**。
func TestAssessBusyLock(t *testing.T) {
	f := &fakeApt{dpkgSequences: []string{baseState()}}
	w := newTestWriter(f, testNow)
	w.locks = func(paths []string) []LockState {
		out := make([]LockState, 0, len(paths))
		for i, path := range paths {
			out = append(out, LockState{Path: path, Held: i == 0})
		}
		return out
	}

	got, err := w.Assess(context.Background(), OpInstall, []string{"cowsay"})
	if err != nil {
		t.Fatalf("预检报错: %v", err)
	}
	if got.Allowed {
		t.Fatal("锁被占着不能放行")
	}
	if len(got.Blockers) != 1 || got.Blockers[0].Code != CodeAptBusy {
		t.Fatalf("应只有一个 APT_BUSY 阻塞项: %+v", got.Blockers)
	}
	if len(got.BusyLocks) != 1 || !got.BusyLocks[0].Held {
		t.Fatalf("BusyLocks 没记下来: %+v", got.BusyLocks)
	}
	if !strings.Contains(got.Blockers[0].Message, "不会删除锁文件") {
		t.Fatalf("消息必须说明不删锁: %q", got.Blockers[0].Message)
	}
	if calls := f.callsMatching("apt-get"); len(calls) != 0 {
		t.Fatalf("锁被占着时不该跑 apt-get（哪怕是模拟）: %v", calls)
	}
}

// TestAssessLockUnreadable：读不到锁不等于没人占用，也不等于有人占用。
func TestAssessLockUnreadable(t *testing.T) {
	f := &fakeApt{dpkgSequences: []string{baseState()}}
	w := newTestWriter(f, testNow)
	w.locks = func(paths []string) []LockState {
		out := make([]LockState, 0, len(paths))
		for _, path := range paths {
			out = append(out, LockState{Path: path, Error: "permission denied"})
		}
		return out
	}
	got, err := w.Assess(context.Background(), OpInstall, []string{"cowsay"})
	if err != nil {
		t.Fatalf("预检报错: %v", err)
	}
	if got.Allowed {
		t.Fatal("探测不了锁不能放行（不能把未知当成空闲）")
	}
	for _, b := range got.Blockers {
		if b.Code == CodeAptBusy {
			t.Fatal("探测失败不是 busy，两者的处理方式不同")
		}
	}
	if len(got.Blockers) != len(w.Paths().LockPaths()) {
		t.Fatalf("四把锁各要一条 APT_LOCK_UNREADABLE，实际 %+v", got.Blockers)
	}
	if got.Blockers[0].Code != CodeLockUnreadable {
		t.Fatalf("代码错: %s", got.Blockers[0].Code)
	}
}

// TestAssessRemoveEssential：apt 自己拒绝，且保护名单也命中，两条都要报。
func TestAssessRemoveEssential(t *testing.T) {
	f := &fakeApt{
		simulateOut:   readCapture(t, "2404-sim-remove.txt"),
		simulateExit:  100,
		dpkgSequences: []string{baseState()},
	}
	w := newTestWriter(f, testNow)
	got, err := w.Assess(context.Background(), OpRemove, []string{"bash"})
	if err != nil {
		t.Fatalf("预检不该因为 apt 拒绝就报内部错误: %v", err)
	}
	if got.Allowed {
		t.Fatal("删 Essential 必须拒绝")
	}
	codes := map[string]bool{}
	for _, b := range got.Blockers {
		codes[b.Code] = true
	}
	if !codes[CodeUnmet] || !codes[CodeProtected] {
		t.Fatalf("应同时报 PACKAGE_UNMET 与 PACKAGE_PROTECTED: %+v", got.Blockers)
	}
	if len(got.Protected) != 1 || got.Protected[0] != "bash" {
		t.Fatalf("保护命中错: %v", got.Protected)
	}
	for _, b := range got.Blockers {
		if b.Code == CodeProtected && !strings.Contains(b.Message, "essential") {
			t.Fatalf("受保护的理由要说明是 Essential: %q", b.Message)
		}
	}
}

// TestAssessRemoveSSH：apt 同意，但保护名单拦住。
func TestAssessRemoveSSH(t *testing.T) {
	f := &fakeApt{
		simulateOut:   readCapture(t, "2404-sim-remove-ssh.txt"),
		dpkgSequences: []string{baseState()},
	}
	w := newTestWriter(f, testNow)
	got, err := w.Assess(context.Background(), OpRemove, []string{"openssh-server"})
	if err != nil {
		t.Fatalf("预检报错: %v", err)
	}
	if got.Allowed {
		t.Fatal("删 SSH 必须拒绝")
	}
	if len(got.Blockers) != 1 || got.Blockers[0].Code != CodeProtected {
		t.Fatalf("应只有 PACKAGE_PROTECTED: %+v", got.Blockers)
	}
	if !strings.Contains(got.Blockers[0].Message, "断开就再也连不上") {
		t.Fatalf("理由要具体: %q", got.Blockers[0].Message)
	}
	if got.Impacts[0].Kind != ImpactRemoval {
		t.Fatalf("卸载的影响类别错: %+v", got.Impacts)
	}
}

// TestAssessRemoveCascade：连带删除是告警，受保护的是阻塞。
func TestAssessRemoveCascade(t *testing.T) {
	f := &fakeApt{
		simulateOut:   readCapture(t, "2404-sim-remove-ca.txt"),
		dpkgSequences: []string{baseState()},
	}
	w := newTestWriter(f, testNow)
	got, err := w.Assess(context.Background(), OpRemove, []string{"ca-certificates"})
	if err != nil {
		t.Fatalf("预检报错: %v", err)
	}
	if got.Allowed {
		t.Fatalf("ca-certificates 受保护，必须拒绝: %+v", got.Blockers)
	}
	if len(got.Collateral) != 25 {
		t.Fatalf("连带应为 25，实际 %d", len(got.Collateral))
	}
	var collateralReason *Reason
	for i := range got.Warnings {
		if got.Warnings[i].Code == WarningCollateral {
			collateralReason = &got.Warnings[i]
		}
	}
	if collateralReason == nil {
		t.Fatalf("连带删除必须有告警: %+v", got.Warnings)
	}
	if !strings.Contains(collateralReason.Message, "25 个没有点名的包") {
		t.Fatalf("告警要报数量: %q", collateralReason.Message)
	}
	if len(got.Impacts) != 1 || !strings.Contains(got.Impacts[0].Description, "26 个") {
		t.Fatalf("影响要报总删除数: %+v", got.Impacts)
	}
}

// TestAssessRemoveUnmetDeps：apt 因为依赖冲突拒绝。
func TestAssessRemoveUnmetDeps(t *testing.T) {
	f := &fakeApt{
		simulateOut:   readCapture(t, "2404-sim-remove-cascade.txt"),
		simulateExit:  100,
		dpkgSequences: []string{baseState()},
	}
	w := newTestWriter(f, testNow)
	got, err := w.Assess(context.Background(), OpRemove, []string{"zlib1g"})
	if err != nil {
		t.Fatalf("预检报错: %v", err)
	}
	if got.Allowed {
		t.Fatal("依赖冲突必须拒绝")
	}
	var unmet *Reason
	for i := range got.Blockers {
		if got.Blockers[i].Code == CodeUnmet {
			unmet = &got.Blockers[i]
		}
	}
	if unmet == nil {
		t.Fatalf("应报 PACKAGE_UNMET: %+v", got.Blockers)
	}
	// 拒绝理由必须带 apt 自己的话，不能只写"操作失败"。
	if !strings.Contains(unmet.Message, "apt : Depends: gpgv") {
		t.Fatalf("理由要引用 apt 原文: %q", unmet.Message)
	}
}

// TestAssessUpgradeNotInstalled：升级一个没装的包是空操作，要明确拒绝。
func TestAssessUpgradeNotInstalled(t *testing.T) {
	f := &fakeApt{
		simulateOut:   readCapture(t, "2404-sim-upgrade-notinstalled.txt"),
		dpkgSequences: []string{baseState()},
	}
	w := newTestWriter(f, testNow)
	got, err := w.Assess(context.Background(), OpUpgrade, []string{"cowsay"})
	if err != nil {
		t.Fatalf("预检报错: %v", err)
	}
	if got.Allowed {
		t.Fatal("升级没装的包必须拒绝")
	}
	if got.Blockers[0].Code != CodeNotInstalled {
		t.Fatalf("应为 PACKAGE_NOT_INSTALLED: %+v", got.Blockers)
	}
	// 没装就无从升级，此时连模拟都不该跑（不该问 apt 一个注定被跳过的问题）。
	if calls := f.callsMatching("apt-get -s"); len(calls) != 0 {
		t.Fatalf("包没装就不该跑模拟: %v", calls)
	}
	// 但仍然给出"不会变化"的告警：预检要说清为什么。
	foundNoChange := false
	for _, w := range got.Warnings {
		if w.Code == WarningNoChange {
			foundNoChange = true
		}
	}
	if !foundNoChange {
		t.Fatalf("应报 no_change: %+v", got.Warnings)
	}
}

// TestAssessInstallAlreadyNewest：已是最新 → 放行但明确"什么都不变"。
func TestAssessInstallAlreadyNewest(t *testing.T) {
	f := &fakeApt{
		simulateOut:   readCapture(t, "2404-sim-already.txt"),
		dpkgSequences: []string{baseState()},
	}
	w := newTestWriter(f, testNow)
	got, err := w.Assess(context.Background(), OpInstall, []string{"bash"})
	if err != nil {
		t.Fatalf("预检报错: %v", err)
	}
	if !got.Allowed {
		t.Fatalf("已是最新不是错误，应放行: %+v", got.Blockers)
	}
	var noChange *Reason
	for i := range got.Warnings {
		if got.Warnings[i].Code == WarningNoChange {
			noChange = &got.Warnings[i]
		}
	}
	if noChange == nil || !strings.Contains(noChange.Message, "已经是最新版本") {
		t.Fatalf("告警要说清什么都不变: %+v", got.Warnings)
	}
}

// TestAssessRefresh：刷新不跑模拟，只核对索引时间。
func TestAssessRefresh(t *testing.T) {
	f := &fakeApt{
		dpkgSequences: []string{baseState()},
		listsMTime:    testNow.Add(-72 * time.Hour),
	}
	w := newTestWriter(f, testNow)
	got, err := w.Assess(context.Background(), OpRefresh, nil)
	if err != nil {
		t.Fatalf("预检报错: %v", err)
	}
	if !got.Allowed {
		t.Fatalf("刷新索引不该被拒: %+v", got.Blockers)
	}
	if calls := f.callsMatching("apt-get"); len(calls) != 0 {
		t.Fatalf("刷新预检不该跑 apt-get: %v", calls)
	}
	if got.IndexUpdatedAt == nil {
		t.Fatal("要读到索引时间")
	}
	var stale *Reason
	for i := range got.Warnings {
		if got.Warnings[i].Code == WarningIndexAge {
			stale = &got.Warnings[i]
		}
	}
	if stale == nil || !strings.Contains(stale.Message, "3 天") {
		t.Fatalf("三天没刷新要报出来: %+v", got.Warnings)
	}
	if got.DisplayName != "软件源索引" {
		t.Fatalf("DisplayName 错: %q", got.DisplayName)
	}
}

// TestAssessRefreshIndexUnreadable：读不到索引时间要说读不到，不能假装刚刷过。
func TestAssessRefreshIndexUnreadable(t *testing.T) {
	f := &fakeApt{dpkgSequences: []string{baseState()}}
	w := newTestWriter(f, testNow)
	got, err := w.Assess(context.Background(), OpRefresh, nil)
	if err != nil {
		t.Fatalf("预检报错: %v", err)
	}
	if got.IndexUpdatedAt != nil {
		t.Fatal("读不到就该是 nil")
	}
	var found bool
	for _, warn := range got.Warnings {
		if warn.Code == WarningIndexAge && strings.Contains(warn.Message, "读不到") {
			found = true
		}
	}
	if !found {
		t.Fatalf("要明确说读不到索引时间: %+v", got.Warnings)
	}
}

// TestAssessRejectsUnknownOp：不认识的写操作不能靠猜。
func TestAssessRejectsUnknownOp(t *testing.T) {
	w := newTestWriter(&fakeApt{}, testNow)
	if _, err := w.Assess(context.Background(), Op("dist-upgrade"), nil); !errors.Is(err, ErrOpUnsupported) {
		t.Fatalf("应报 ErrOpUnsupported，实际 %v", err)
	}
}

// TestIdentityIndependentOfOrder：同一意图的两种写法必须同一个指纹。
func TestIdentityIndependentOfOrder(t *testing.T) {
	a := identityFor(OpInstall, []string{"curl", "wget"})
	b := identityFor(OpInstall, []string{"wget", "curl", "curl"})
	if a != b {
		t.Fatalf("顺序/重复不该改变指纹:\n%s\n%s", a, b)
	}
	if identityFor(OpRemove, []string{"curl", "wget"}) == a {
		t.Fatal("不同操作必须不同指纹")
	}
	if !strings.HasPrefix(a, "sha256:") || len(a) != len("sha256:")+64 {
		t.Fatalf("指纹形状错: %q", a)
	}
}

// TestStateVersionTracksIndexAndVersions：状态版本必须随索引时间与已装版本变化。
func TestStateVersionTracksIndexAndVersions(t *testing.T) {
	at1 := testNow
	at2 := testNow.Add(time.Minute)
	state := &dpkgState{version: map[string]string{"bash": "1.0", "zlib1g": "2.0"}}
	base := stateVersion(&at1, state, []string{"bash"})
	if base != stateVersion(&at1, state, []string{"bash"}) {
		t.Fatal("同一状态必须得到同一个值（否则每次预检都会被判过期）")
	}
	if base == stateVersion(&at2, state, []string{"bash"}) {
		t.Fatal("索引时间变了，状态版本必须变")
	}
	state.version["bash"] = "1.1"
	if base == stateVersion(&at1, state, []string{"bash"}) {
		t.Fatal("已装版本变了，状态版本必须变")
	}
	// 架构限定不影响状态版本（`bash:amd64` 与 `bash` 是同一条记录）。
	state.version["bash"] = "1.0"
	if base != stateVersion(&at1, state, []string{"bash:amd64"}) {
		t.Fatal("架构限定不该改变状态版本")
	}
}
