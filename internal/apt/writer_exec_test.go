package apt

// 执行与核对的单元测试（P11.2）。
//
// 这一层最容易被写错的地方是"把退出码当成验收"。所以这里刻意造了几个
// **退出码 0 但事实不对**的场景：apt 说成功了，dpkg 数据库里却不是那个版本。
// 面板必须报 failed，而不是报 verified。

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestExecuteVerifiesVersions：计划的每一步都要在读回的 dpkg 事实里对上。
func TestExecuteVerifiesVersions(t *testing.T) {
	f := &fakeApt{
		simulateOut: readCapture(t, "2404-sim-install-upgrade.txt"),
		execOut:     "正在读取软件包列表...\n",
		dpkgSequences: []string{
			// 执行后：zlib1g 已经是计划里的目标版本。
			dpkgLine("zlib1g", "ii ", "1:1.3.dfsg-3.1ubuntu2.3", false),
		},
		mtimeSeq: []time.Time{testNow.Add(-time.Hour), testNow},
	}
	w := newTestWriter(f, testNow)

	res, err := w.Execute(context.Background(), OpUpgrade, []string{"zlib1g"}, nil)
	if err != nil {
		t.Fatalf("执行报错: %v", err)
	}
	if !res.Verified {
		t.Fatalf("应当核对通过，Mismatches=%v", res.Mismatches)
	}
	if len(res.Mismatches) != 0 {
		t.Fatalf("不该有对不上的条目: %v", res.Mismatches)
	}
	if res.ExitCode != 0 || res.Duration < 0 {
		t.Fatalf("退出码/耗时不正常: %+v", res)
	}
	observed := res.Observed["zlib1g"]
	if !observed.Installed() || observed.Version != "1:1.3.dfsg-3.1ubuntu2.3" {
		t.Fatalf("Observed 应记下真实版本: %+v", observed)
	}
	if len(res.Plan.Steps) != 1 || res.Plan.Steps[0].Kind != StepUpgrade {
		t.Fatalf("结果里要带执行前那份计划: %+v", res.Plan.Steps)
	}
	// 索引时间要在执行前后各读一次（refresh 的核对靠它，这里顺带记下来）。
	if res.IndexBefore == nil || res.IndexUpdatedAt == nil {
		t.Fatalf("索引时间没记: before=%v after=%v", res.IndexBefore, res.IndexUpdatedAt)
	}
	// 执行前必须重新核对锁，而且跑的命令要与模拟同一个动词。
	if calls := f.callsMatching("apt-get -y"); len(calls) != 1 {
		t.Fatalf("写命令应当只跑一次: %v", calls)
	}
	exec := f.callsMatching("apt-get -y")[0]
	if !strings.Contains(exec, "install --only-upgrade zlib1g") {
		t.Fatalf("升级必须用 install --only-upgrade（不能是 upgrade/dist-upgrade）: %s", exec)
	}
	for _, bad := range []string{" dist-upgrade", " full-upgrade", " upgrade "} {
		if strings.Contains(exec, bad) {
			t.Fatalf("升级里出现了隐式全量升级: %s", exec)
		}
	}
}

// TestExecuteDetectsVersionMismatch：退出码 0，但装上的不是那一版。
//
// 这是"apt 成功 ≠ 事情如预期"的最典型形态：可能被 hold 住、可能 postinst 回滚，
// 也可能索引在执行期间变了。面板要报失败并指名道姓。
func TestExecuteDetectsVersionMismatch(t *testing.T) {
	f := &fakeApt{
		simulateOut: readCapture(t, "2404-sim-install-upgrade.txt"),
		execOut:     "",
		dpkgSequences: []string{
			// 执行后还是旧版本。
			dpkgLine("zlib1g", "ii ", "1:1.3.dfsg-3.1ubuntu2.2", false),
		},
		mtimeSeq: []time.Time{testNow, testNow},
	}
	w := newTestWriter(f, testNow)

	res, err := w.Execute(context.Background(), OpUpgrade, []string{"zlib1g"}, nil)
	if err != nil {
		t.Fatalf("apt 没报错，这里不该返回 error: %v", err)
	}
	if res.Verified {
		t.Fatal("版本对不上不能判核对通过")
	}
	if len(res.Mismatches) != 1 {
		t.Fatalf("应有一条不匹配: %v", res.Mismatches)
	}
	msg := res.Mismatches[0]
	if !strings.Contains(msg, "zlib1g") ||
		!strings.Contains(msg, "1:1.3.dfsg-3.1ubuntu2.2") ||
		!strings.Contains(msg, "1:1.3.dfsg-3.1ubuntu2.3") {
		t.Fatalf("不匹配的说明要给出包名、实际版本、期望版本: %q", msg)
	}
}

// TestExecuteDetectsNotInstalled：计划要装，结果根本没装上。
func TestExecuteDetectsNotInstalled(t *testing.T) {
	f := &fakeApt{
		simulateOut:   readCapture(t, "2404-sim-install-new.txt"),
		dpkgSequences: []string{""},
	}
	w := newTestWriter(f, testNow)
	res, err := w.Execute(context.Background(), OpInstall, []string{"cowsay"}, nil)
	if err != nil {
		t.Fatalf("执行报错: %v", err)
	}
	if res.Verified {
		t.Fatal("没装上不能判核对通过")
	}
	if len(res.Mismatches) != 1 || !strings.Contains(res.Mismatches[0], "没有装上") {
		t.Fatalf("不匹配说明错: %v", res.Mismatches)
	}
}

// TestExecuteRemoveAcceptsRCState：卸载后留下配置文件（`rc`）是正常结果。
//
// 如果这里判失败，每一次正常的卸载都会报错——apt-get remove 的语义本来就是
// "删程序、留配置"，删配置是 purge。判据是"不再是 ii"，不是"记录消失"。
func TestExecuteRemoveAcceptsRCState(t *testing.T) {
	f := &fakeApt{
		simulateOut:   readCapture(t, "2404-sim-remove-ssh.txt"),
		dpkgSequences: []string{dpkgLine("openssh-server", "rc ", "1:9.6p1-3ubuntu13.20", false)},
	}
	w := newTestWriter(f, testNow)
	res, err := w.Execute(context.Background(), OpRemove, []string{"openssh-server"}, nil)
	if err != nil {
		t.Fatalf("执行报错: %v", err)
	}
	if !res.Verified {
		t.Fatalf("留下 rc 是正常卸载结果，不该判失败: %v", res.Mismatches)
	}
	if res.Observed["openssh-server"].Installed() {
		t.Fatal("rc 不是已安装")
	}
}

// TestExecuteRemoveDetectsStillInstalled：计划要删，结果还装着一整份。
func TestExecuteRemoveDetectsStillInstalled(t *testing.T) {
	f := &fakeApt{
		simulateOut:   readCapture(t, "2404-sim-remove-ssh.txt"),
		dpkgSequences: []string{dpkgLine("openssh-server", "ii ", "1:9.6p1-3ubuntu13.20", false)},
	}
	w := newTestWriter(f, testNow)
	res, err := w.Execute(context.Background(), OpRemove, []string{"openssh-server"}, nil)
	if err != nil {
		t.Fatalf("执行报错: %v", err)
	}
	if res.Verified || len(res.Mismatches) != 1 {
		t.Fatalf("没删掉必须报出来: %v", res.Mismatches)
	}
	if !strings.Contains(res.Mismatches[0], "仍然是安装状态") {
		t.Fatalf("说明错: %q", res.Mismatches[0])
	}
}

// TestExecuteBusyLockRunsNothing：执行前复核锁，被占着就一次 apt-get 都不跑。
func TestExecuteBusyLockRunsNothing(t *testing.T) {
	f := &fakeApt{simulateOut: readCapture(t, "2404-sim-install-new.txt")}
	w := newTestWriter(f, testNow)
	w.locks = func(paths []string) []LockState {
		out := make([]LockState, 0, len(paths))
		for _, path := range paths {
			out = append(out, LockState{Path: path, Held: path == DefaultDpkgLockFrontend})
		}
		return out
	}
	_, err := w.Execute(context.Background(), OpInstall, []string{"cowsay"}, nil)
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("应报 ErrBusy，实际 %v", err)
	}
	if !strings.Contains(err.Error(), DefaultDpkgLockFrontend) {
		t.Fatalf("错误要说明是哪把锁: %v", err)
	}
	if calls := f.callsMatching("apt-get"); len(calls) != 0 {
		t.Fatalf("锁被占着时不该跑 apt-get: %v", calls)
	}
}

// TestExecuteFailureKeepsEvidence：apt 非零退出时，退出码与 E: 行都要留下来。
// TestExecuteKilledReasonNamesTheSignal：被信号停住的 apt，原因不能写成
// "退出码 -1 + 半截日志"。
//
// 这条来自真机：P11.2 验收在 22.04/24.04 上硬杀安装中的 apt，面板当时真的写出
// 「apt-get install 失败（退出码 -1）：Reading package lists...」——读的人既不知道
// 是谁停的，也会把那半截日志当成 apt 的报错。
func TestExecuteKilledReasonNamesTheSignal(t *testing.T) {
	f := &fakeApt{
		simulateOut:   readCapture(t, "2404-sim-install-upgrade.txt"),
		execOut:       "正在读取软件包列表...\n",
		execSignal:    "SIGKILL",
		dpkgSequences: []string{dpkgLine("zlib1g", "ii ", "1:1.2.11.dfsg-2ubuntu9.2", false)},
	}
	w := newTestWriter(f, testNow)
	res, err := w.Execute(context.Background(), OpInstall, []string{"zlib1g"}, nil)
	if err == nil {
		t.Fatal("被信号终止必须返回 error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "被信号 SIGKILL 终止") {
		t.Fatalf("原因里要写清是谁停的: %q", msg)
	}
	if !strings.Contains(msg, "事务可能停在中间") {
		t.Fatalf("原因里要说明事务可能停在中间: %q", msg)
	}
	if !strings.Contains(msg, "-1") {
		t.Fatalf("退出码这个事实不能丢: %q", msg)
	}
	if res.Verified {
		t.Fatal("被信号终止不能判核对通过")
	}
	// 失败后的读回照样要做：它是判断"停在哪"的唯一依据。
	if res.Observed["zlib1g"].Version == "" {
		t.Fatalf("失败后仍要记录真实版本: %+v", res.Observed)
	}
}

// TestRefreshKilledReasonNamesTheSignal：刷新被停住时同理。
func TestRefreshKilledReasonNamesTheSignal(t *testing.T) {
	f := &fakeApt{refreshOut: "命中:1 http://archive.ubuntu.com/ubuntu noble InRelease\n", refreshSignal: "SIGTERM"}
	w := newTestWriter(f, testNow)
	res, err := w.Refresh(context.Background(), nil)
	if err == nil {
		t.Fatal("被信号终止必须返回 error")
	}
	if !strings.Contains(err.Error(), "被信号 SIGTERM 终止") {
		t.Fatalf("原因里要写清是谁停的: %q", err.Error())
	}
	if res.Verified {
		t.Fatal("被信号终止不能判核对通过")
	}
}

// TestExecuteFailureKeepsEvidence：apt 非零退出时保留现场。
func TestExecuteFailureKeepsEvidence(t *testing.T) {
	f := &fakeApt{
		simulateOut:   readCapture(t, "2404-sim-remove-ssh.txt"),
		execExit:      100,
		execErrOut:    "E: 子进程 已安装 post-removal 脚本 返回了错误号 1\n",
		dpkgSequences: []string{dpkgLine("openssh-server", "ii ", "1:9.6p1-3ubuntu13.20", false)},
	}
	w := newTestWriter(f, testNow)
	res, err := w.Execute(context.Background(), OpRemove, []string{"openssh-server"}, nil)
	if err == nil {
		t.Fatal("apt 非零退出必须返回 error")
	}
	if res.ExitCode != 100 {
		t.Fatalf("退出码要留下: %d", res.ExitCode)
	}
	if !strings.Contains(err.Error(), "post-removal") {
		t.Fatalf("错误信息要带 apt 自己的话（而不是'操作失败'）: %v", err)
	}
	if !strings.Contains(err.Error(), "100") {
		t.Fatalf("错误信息要带退出码: %v", err)
	}
	// 失败也要读回事实：它说明"改到一半"是什么样。
	if res.Observed["openssh-server"].Status != "ii" {
		t.Fatalf("失败后仍要记录真实状态: %+v", res.Observed)
	}
	if res.Verified {
		t.Fatal("失败不能判核对通过")
	}
}

// TestExecuteRefusesWhenPlanFails：执行前的模拟被 apt 拒绝时不动系统。
func TestExecuteRefusesWhenPlanFails(t *testing.T) {
	f := &fakeApt{
		simulateOut:   readCapture(t, "2404-sim-missing.txt"),
		simulateExit:  100,
		dpkgSequences: []string{baseState()},
	}
	w := newTestWriter(f, testNow)
	_, err := w.Execute(context.Background(), OpInstall, []string{"nosuchpkg-xyz"}, nil)
	if err == nil {
		t.Fatal("模拟被拒绝时必须停下")
	}
	if !strings.Contains(err.Error(), "Unable to locate package") {
		t.Fatalf("错误要带 apt 的原话: %v", err)
	}
	if calls := f.callsMatching("apt-get -y"); len(calls) != 0 {
		t.Fatalf("计划不成立时不该执行写命令: %v", calls)
	}
}

// TestExecuteUnknownOp：不认识的写操作不能靠猜。
func TestExecuteUnknownOp(t *testing.T) {
	w := newTestWriter(&fakeApt{}, testNow)
	if _, err := w.Execute(context.Background(), Op("autoremove"), nil, nil); !errors.Is(err, ErrOpUnsupported) {
		t.Fatalf("应报 ErrOpUnsupported，实际 %v", err)
	}
}

// TestExecuteProgressSteps：进度事件必须是真实发生过的阶段，不编百分比。
func TestExecuteProgressSteps(t *testing.T) {
	f := &fakeApt{
		simulateOut:   readCapture(t, "2404-sim-install-upgrade.txt"),
		dpkgSequences: []string{dpkgLine("zlib1g", "ii ", "1:1.3.dfsg-3.1ubuntu2.3", false)},
	}
	w := newTestWriter(f, testNow)
	var steps []string
	if _, err := w.Execute(context.Background(), OpUpgrade, []string{"zlib1g"}, func(step string, fields map[string]any) {
		steps = append(steps, step)
	}); err != nil {
		t.Fatalf("执行报错: %v", err)
	}
	want := []string{"simulate", "execute", "verify"}
	if strings.Join(steps, ",") != strings.Join(want, ",") {
		t.Fatalf("进度阶段应为 %v，实际 %v", want, steps)
	}
}

// ---------- 刷新索引 ----------

// TestRefreshAdvancesIndex：索引 mtime 往前走了，核对通过。
func TestRefreshAdvancesIndex(t *testing.T) {
	f := &fakeApt{
		refreshOut: readCapture(t, "2404-sim-refresh.txt"),
		mtimeSeq:   []time.Time{testNow.Add(-time.Hour), testNow},
	}
	w := newTestWriter(f, testNow)
	res, err := w.Refresh(context.Background(), nil)
	if err != nil {
		t.Fatalf("刷新报错: %v", err)
	}
	if !res.Verified {
		t.Fatal("应当核对通过")
	}
	if res.IndexBefore == nil || res.IndexUpdatedAt == nil {
		t.Fatal("索引时间要前后都记")
	}
	if !res.IndexUpdatedAt.After(*res.IndexBefore) {
		t.Fatalf("索引时间应前进: %v → %v", res.IndexBefore, res.IndexUpdatedAt)
	}
	if len(res.RefreshErrors) != 0 {
		t.Fatalf("这份真实输出没有错误行: %v", res.RefreshErrors)
	}
	// 刷新必须是 update，且带非交互环境变量（不弹 debconf）。
	exec := f.callsMatching("apt-get")
	if len(exec) != 1 || !strings.Contains(exec[0], "update") {
		t.Fatalf("应跑一次 apt-get update: %v", exec)
	}
}

// TestRefreshDetectsPerSourceError：apt-get update 对单个源失败时**退出码可能是 0**，
// 所以不能只看退出码。
func TestRefreshDetectsPerSourceError(t *testing.T) {
	f := &fakeApt{
		refreshOut: "Hit:1 http://archive.ubuntu.com/ubuntu noble InRelease\n" +
			"Err:2 http://deb.example.invalid/repo stable InRelease\n" +
			"  Could not resolve 'deb.example.invalid'\n" +
			"Reading package lists...\n",
		refreshExit: 0,
	}
	w := newTestWriter(f, testNow)
	res, err := w.Refresh(context.Background(), nil)
	if err == nil {
		t.Fatal("退出码 0 但有 Err: 行，必须报错")
	}
	if !strings.Contains(err.Error(), "deb.example.invalid") {
		t.Fatalf("错误要说明是哪个源: %v", err)
	}
	if len(res.RefreshErrors) != 1 {
		t.Fatalf("错误行要记下来: %v", res.RefreshErrors)
	}
	if res.Verified {
		t.Fatal("有错误行不能判核对通过")
	}
}

// TestRefreshRecordsWarnings：`W:` 行是告警不是失败。
func TestRefreshRecordsWarnings(t *testing.T) {
	f := &fakeApt{
		refreshOut: "W: https://repo.example/key.gpg: Key is stored in legacy trusted.gpg keyring\n" +
			"Reading package lists...\n",
	}
	w := newTestWriter(f, testNow)
	res, err := w.Refresh(context.Background(), nil)
	if err != nil {
		t.Fatalf("告警不该导致失败: %v", err)
	}
	if len(res.RefreshWarnings) != 1 || !strings.Contains(res.RefreshWarnings[0], "legacy trusted.gpg") {
		t.Fatalf("告警要原文保留: %v", res.RefreshWarnings)
	}
	if !res.Verified {
		t.Fatal("仅有告警时应判通过")
	}
}

// TestRefreshFailureReportsExitCode。
func TestRefreshFailureReportsExitCode(t *testing.T) {
	f := &fakeApt{
		refreshOut:  "E: Could not get lock /var/lib/apt/lists/lock. It is held by process 1234 (apt-get)\n",
		refreshExit: 100,
	}
	w := newTestWriter(f, testNow)
	_, err := w.Refresh(context.Background(), nil)
	if err == nil {
		t.Fatal("退出码 100 必须报错")
	}
	if !strings.Contains(err.Error(), "100") || !strings.Contains(err.Error(), "It is held by process") {
		t.Fatalf("错误要带退出码与 apt 原话: %v", err)
	}
}

// TestRefreshBusyLockRunsNothing。
func TestRefreshBusyLockRunsNothing(t *testing.T) {
	f := &fakeApt{refreshOut: readCapture(t, "2404-sim-refresh.txt")}
	w := newTestWriter(f, testNow)
	w.locks = func(paths []string) []LockState {
		out := make([]LockState, 0, len(paths))
		for _, path := range paths {
			out = append(out, LockState{Path: path, Held: true})
		}
		return out
	}
	if _, err := w.Refresh(context.Background(), nil); !errors.Is(err, ErrBusy) {
		t.Fatalf("应报 ErrBusy，实际 %v", err)
	}
	if calls := f.callsMatching("apt-get"); len(calls) != 0 {
		t.Fatalf("锁被占着时不该跑 apt-get: %v", calls)
	}
}

// TestExecVerbMatchesSimulation：执行用的动词必须与模拟完全一致。
//
// 如果两边不一致，预检拿 A 方案给人确认、执行却跑 B 方案——这正是最危险的一类
// 缺陷（"我确认的是删一个包，它删了 26 个"）。
func TestExecVerbMatchesSimulation(t *testing.T) {
	for _, op := range []Op{OpInstall, OpUpgrade, OpRemove} {
		sim := strings.Join(simulationVerb(op), " ")
		exec := strings.Join(execVerb(op), " ")
		if sim != exec {
			t.Fatalf("%s 的模拟动词 %q 与执行动词 %q 不一致", op, sim, exec)
		}
	}
	if got := strings.Join(execVerb(OpUpgrade), " "); got != "install --only-upgrade" {
		t.Fatalf("升级动词错: %q", got)
	}
	if got := strings.Join(execVerb(OpInstall), " "); got != "install" {
		t.Fatalf("安装动词错: %q", got)
	}
	if got := strings.Join(execVerb(OpRemove), " "); got != "remove" {
		t.Fatalf("卸载动词错: %q", got)
	}
	// 面板不提供 dist-upgrade / autoremove 这类隐式全量操作。
	for _, op := range []Op{OpInstall, OpUpgrade, OpRemove} {
		if strings.Contains(strings.Join(execVerb(op), " "), "dist-upgrade") {
			t.Fatalf("%s 不该出现 dist-upgrade", op)
		}
	}
}

// TestNonInteractiveArgsAreFixed：非交互参数是代码里的固定值。
func TestNonInteractiveArgsAreFixed(t *testing.T) {
	joined := strings.Join(aptGetNonInteractive(), " ")
	for _, want := range []string{"-y", "-q", "Dpkg::Options::=--force-confdef", "Dpkg::Options::=--force-confold", "Dpkg::Use-Pty=0"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("非交互参数缺 %s: %s", want, joined)
		}
	}
	env := strings.Join(aptNonInteractiveEnv(), " ")
	for _, want := range []string{"DEBIAN_FRONTEND=noninteractive", "APT_LISTCHANGES_FRONTEND=none", "NEEDRESTART_MODE=l"} {
		if !strings.Contains(env, want) {
			t.Fatalf("非交互环境缺 %s: %s", want, env)
		}
	}
	// NEEDRESTART_MODE 必须是 l（只列出来），不是 a（自动重启服务）。
	if strings.Contains(env, "NEEDRESTART_MODE=a") {
		t.Fatal("不能在装包时自动重启服务")
	}
}
