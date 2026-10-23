package apt

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"upanel/internal/syscmd"
)

// 假文件系统的 FileInfo。
type fakeInfo struct {
	name  string
	mode  os.FileMode
	size  int64
	mtime time.Time
	isDir bool
}

func (f fakeInfo) Name() string       { return f.name }
func (f fakeInfo) Size() int64        { return f.size }
func (f fakeInfo) Mode() os.FileMode  { return f.mode }
func (f fakeInfo) ModTime() time.Time { return f.mtime }
func (f fakeInfo) IsDir() bool        { return f.isDir }
func (f fakeInfo) Sys() any           { return nil }

// fakeEnv 把命令执行与文件访问都换成可数的假实现。
type fakeEnv struct {
	// run 按程序路径给出结果；返回 error 表示"命令没跑起来"。
	run func(program string, args []string) (syscmd.Result, error)
	// files 是能读到的文件内容（读不到的文件不在表里 → os.ErrNotExist）。
	files map[string][]byte
	// infos 是 stat 能看到的路径。
	infos map[string]fakeInfo
	// statErrs 优先于 infos：模拟权限/IO 错误。
	statErrs map[string]error

	runCalls  []string
	readCalls []string
}

func (e *fakeEnv) options() Options {
	return Options{
		Run: func(_ context.Context, spec syscmd.Spec, _ syscmd.Limits) (syscmd.Result, error) {
			e.runCalls = append(e.runCalls, spec.Program)
			return e.run(spec.Program, spec.Args)
		},
		Read: func(path string) ([]byte, error) {
			e.readCalls = append(e.readCalls, path)
			if data, ok := e.files[path]; ok {
				return data, nil
			}
			return nil, os.ErrNotExist
		},
		Stat: func(path string) (os.FileInfo, error) {
			if err, ok := e.statErrs[path]; ok {
				return nil, err
			}
			if info, ok := e.infos[path]; ok {
				return info, nil
			}
			return nil, os.ErrNotExist
		},
	}
}

func newDirInfo(mod time.Time) fakeInfo {
	return fakeInfo{name: "lists", mode: os.ModeDir | 0o755, mtime: mod, isDir: true}
}

func newExecInfo() fakeInfo {
	return fakeInfo{name: "dpkg-query", mode: 0o755}
}

// happyEnv 是一个"两台验收机都正常"的环境。
func happyEnv(policy string) *fakeEnv {
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	return &fakeEnv{
		run: func(program string, args []string) (syscmd.Result, error) {
			switch program {
			case DefaultDpkgQuery:
				return syscmd.Result{Program: program, Stdout: realDpkgQuery}, nil
			case DefaultAptCache:
				return syscmd.Result{Program: program, Stdout: policy}, nil
			}
			return syscmd.Result{Program: program, ExitCode: 127}, fmt.Errorf("unexpected program %s", program)
		},
		files: map[string][]byte{
			DefaultBootIDPath: []byte("ab2eb5e8-c1b4-4a00-a3d0-33ced7870abc\n"),
		},
		infos: map[string]fakeInfo{
			DefaultDpkgQuery: newExecInfo(),
			DefaultListsDir:  newDirInfo(now),
		},
	}
}

func TestCollectorPackagesHappyPath(t *testing.T) {
	env := happyEnv(realPolicy2204)
	c := New(env.options())

	snap, err := c.Packages(context.Background(), false)
	if err != nil {
		t.Fatalf("Packages 失败: %v", err)
	}
	if len(snap.Errors) != 0 {
		t.Fatalf("不该有降级来源: %v", snap.Errors)
	}
	if snap.IndexUpdatedAt == nil || !snap.IndexUpdatedAt.Equal(time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)) {
		t.Errorf("index_updated_at = %v", snap.IndexUpdatedAt)
	}
	if len(snap.Packages) != 8 {
		t.Fatalf("包数 = %d", len(snap.Packages))
	}
	// 排序：(name, architecture) 升序。真机输出本来就是按名的，但这里必须是代码
	// 保证的，不能依赖 dpkg 的输出顺序。
	for i := 1; i < len(snap.Packages); i++ {
		prev, cur := snap.Packages[i-1], snap.Packages[i]
		if prev.Name > cur.Name || (prev.Name == cur.Name && prev.Architecture > cur.Architecture) {
			t.Fatalf("排序错：%s/%s 在 %s/%s 之后", prev.Name, prev.Architecture, cur.Name, cur.Architecture)
		}
	}
	byName := map[string]Package{}
	for _, p := range snap.Packages {
		byName[p.Name] = p
	}
	// 有候选的包拿到候选与候选来源。
	if p := byName["bash"]; p.CandidateVersion != "5.1-6ubuntu1.1" ||
		len(p.CandidateSuites) != 2 {
		t.Errorf("bash = %+v", p)
	}
	// 没出现在 policy 输出里的包（真机上 8 个里只问了 5 个）：候选为空，
	// 但已经不是"读不到"——errors 是空的，页面据此说"这台机器上 apt 说不出候选"。
	if p := byName["libc6:amd64"]; p.CandidateVersion != "" || p.CandidateFromDpkgStatus {
		t.Errorf("libc6:amd64 = %+v", p)
	}
}

func TestCollectorPackagesAptCacheFails(t *testing.T) {
	env := happyEnv("")
	env.run = func(program string, args []string) (syscmd.Result, error) {
		if program == DefaultDpkgQuery {
			return syscmd.Result{Program: program, Stdout: realDpkgQuery}, nil
		}
		return syscmd.Result{Program: program, ExitCode: 100, Stderr: "E: 无法打开缓存"}, nil
	}
	c := New(env.options())

	snap, err := c.Packages(context.Background(), false)
	if err != nil {
		t.Fatalf("apt-cache 失败不该让整个列表失败: %v", err)
	}
	if len(snap.Packages) != 8 {
		t.Fatalf("包列表应照常返回，实际 %d 个", len(snap.Packages))
	}
	for _, p := range snap.Packages {
		if p.CandidateVersion != "" || p.CandidateSuites != nil {
			t.Fatalf("%s 在没有候选数据时不该有候选: %+v", p.Name, p)
		}
	}
	if len(snap.Errors) != 1 || snap.Errors[0].Source != SourceAptCache {
		t.Fatalf("errors = %+v", snap.Errors)
	}
	if !strings.Contains(snap.Errors[0].Message, "100") {
		t.Errorf("错误信息里应带退出码: %q", snap.Errors[0].Message)
	}
}

func TestCollectorPackagesDpkgQueryFails(t *testing.T) {
	env := happyEnv(realPolicy2204)
	env.run = func(program string, args []string) (syscmd.Result, error) {
		return syscmd.Result{Program: program, ExitCode: 2, Stderr: "dpkg-query: error"}, nil
	}
	c := New(env.options())
	if _, err := c.Packages(context.Background(), false); err == nil {
		t.Fatal("dpkg-query 非零退出应让采集失败")
	}

	// 命令根本没跑起来（路径被删、权限不足）。
	env2 := happyEnv(realPolicy2204)
	env2.run = func(program string, args []string) (syscmd.Result, error) {
		return syscmd.Result{Program: program, ExitCode: -1}, errors.New("fork/exec: no such file")
	}
	if _, err := New(env2.options()).Packages(context.Background(), false); err == nil {
		t.Fatal("执行失败应报错")
	}
}

func TestCollectorPackagesRejectsTruncatedOutput(t *testing.T) {
	env := happyEnv(realPolicy2204)
	env.run = func(program string, args []string) (syscmd.Result, error) {
		if program == DefaultDpkgQuery {
			return syscmd.Result{Program: program, Stdout: realDpkgQuery, StdoutTruncated: true}, nil
		}
		return syscmd.Result{Program: program, Stdout: realPolicy2204}, nil
	}
	_, err := New(env.options()).Packages(context.Background(), false)
	if err == nil {
		t.Fatal("截断的包列表必须报错，不能当完整结果")
	}
	if !strings.Contains(err.Error(), "截断") {
		t.Errorf("错误信息应说明截断: %v", err)
	}
}

func TestCollectorSnapshotCache(t *testing.T) {
	env := happyEnv(realPolicy2204)
	clock := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	opts := env.options()
	opts.TTL = 30 * time.Second
	opts.Now = func() time.Time { return clock }
	c := New(opts)

	if _, err := c.Packages(context.Background(), false); err != nil {
		t.Fatalf("第一次采集失败: %v", err)
	}
	dpkgCalls := countIn(env.runCalls, DefaultDpkgQuery)
	if dpkgCalls != 1 {
		t.Fatalf("第一次采集 dpkg-query 调用 %d 次", dpkgCalls)
	}

	// 缓存窗口内：不再跑命令。
	clock = clock.Add(20 * time.Second)
	if _, err := c.Packages(context.Background(), false); err != nil {
		t.Fatalf("第二次采集失败: %v", err)
	}
	if got := countIn(env.runCalls, DefaultDpkgQuery); got != 1 {
		t.Fatalf("缓存窗口内又跑了一次 dpkg-query（共 %d 次）", got)
	}

	// 过期后重跑。
	clock = clock.Add(20 * time.Second)
	if _, err := c.Packages(context.Background(), false); err != nil {
		t.Fatalf("第三次采集失败: %v", err)
	}
	if got := countIn(env.runCalls, DefaultDpkgQuery); got != 2 {
		t.Fatalf("缓存过期后应重跑，实际 %d 次", got)
	}

	// fresh=true 绕过缓存：写操作前的判断必须看见刚刚发生的变化。
	if _, err := c.Packages(context.Background(), true); err != nil {
		t.Fatalf("fresh 采集失败: %v", err)
	}
	if got := countIn(env.runCalls, DefaultDpkgQuery); got != 3 {
		t.Fatalf("fresh=true 应重跑，实际 %d 次", got)
	}
}

func TestCollectorChunksAptCacheArgs(t *testing.T) {
	// 2500 个包 → 2000 + 500 两次 apt-cache policy。
	var sb strings.Builder
	for i := 0; i < 2500; i++ {
		fmt.Fprintf(&sb, "pkg%04d\t1.0-%d\tamd64\tii \n", i, i)
	}
	var chunks []int
	env := happyEnv("")
	env.run = func(program string, args []string) (syscmd.Result, error) {
		if program == DefaultDpkgQuery {
			return syscmd.Result{Program: program, Stdout: sb.String()}, nil
		}
		chunks = append(chunks, len(args)-1)
		// 每块都返回一份真实 policy 输出：内容不重要，这里只看分块。
		return syscmd.Result{Program: program, Stdout: realPolicy2204}, nil
	}
	if _, err := New(env.options()).Packages(context.Background(), false); err != nil {
		t.Fatalf("Packages 失败: %v", err)
	}
	if len(chunks) != 2 || chunks[0] != aptCacheChunk || chunks[1] != 2500-aptCacheChunk {
		t.Fatalf("分块 = %v，期望 [%d %d]", chunks, aptCacheChunk, 2500-aptCacheChunk)
	}
}

func TestCollectorNoListsDir(t *testing.T) {
	// 索引目录不存在 = 从来没刷新过，不是读失败。
	env := happyEnv(realPolicy2204)
	delete(env.infos, DefaultListsDir)
	snap, err := New(env.options()).Packages(context.Background(), false)
	if err != nil {
		t.Fatalf("Packages 失败: %v", err)
	}
	if snap.IndexUpdatedAt != nil {
		t.Errorf("index_updated_at 应为 nil，实际 %v", snap.IndexUpdatedAt)
	}
	if len(snap.Errors) != 0 {
		t.Errorf("索引目录不存在不该算错误: %v", snap.Errors)
	}
}

func TestCollectorPowerStateNoReboot(t *testing.T) {
	env := happyEnv(realPolicy2204)
	st, err := New(env.options()).PowerState(context.Background())
	if err != nil {
		t.Fatalf("PowerState 失败: %v", err)
	}
	if st.RebootRequired {
		t.Error("没有 /run/reboot-required 时不该说需要重启")
	}
	if st.RebootRequiredSince != nil {
		t.Errorf("since = %v", st.RebootRequiredSince)
	}
	if st.RebootRequiredPackages == nil || len(st.RebootRequiredPackages) != 0 {
		t.Errorf("packages 应为空切片，实际 %v", st.RebootRequiredPackages)
	}
	if st.RebootRequiredPackagesError != "" || st.BootIDError != "" {
		t.Errorf("不该有读失败: %+v", st)
	}
	if st.BootID != "ab2eb5e8-c1b4-4a00-a3d0-33ced7870abc" {
		t.Errorf("boot_id = %q", st.BootID)
	}
}

func TestCollectorPowerStateRebootRequired(t *testing.T) {
	// 「合成」：两台验收机现在都不需要重启。文件内容/形状按真机 postinst 的写法构造
	// （主文件为空、.pkgs 每行一个包），mtime 用假值。
	env := happyEnv(realPolicy2204)
	at := time.Date(2026, 10, 23, 9, 30, 0, 0, time.UTC)
	env.infos[DefaultRebootFlag] = fakeInfo{name: "reboot-required", mode: 0o644, mtime: at}
	env.files[DefaultRebootPkgs] = []byte("linux-image-5.15.0-198-generic\nlibssl3:amd64\n")

	st, err := New(env.options()).PowerState(context.Background())
	if err != nil {
		t.Fatalf("PowerState 失败: %v", err)
	}
	if !st.RebootRequired {
		t.Fatal("标志文件存在时应报告需要重启")
	}
	if st.RebootRequiredSince == nil || !st.RebootRequiredSince.Equal(at) {
		t.Errorf("since = %v", st.RebootRequiredSince)
	}
	want := []string{"linux-image-5.15.0-198-generic", "libssl3:amd64"}
	if len(st.RebootRequiredPackages) != 2 ||
		st.RebootRequiredPackages[0] != want[0] || st.RebootRequiredPackages[1] != want[1] {
		t.Errorf("packages = %v", st.RebootRequiredPackages)
	}
}

func TestCollectorPowerStateLegacyPath(t *testing.T) {
	// /var/run 是 /run 的符号链接；只有 /run 那个不存在时才看它。
	env := happyEnv(realPolicy2204)
	at := time.Date(2026, 10, 23, 9, 30, 0, 0, time.UTC)
	env.infos[rebootFlagPathLegacy] = fakeInfo{name: "reboot-required", mode: 0o644, mtime: at}
	st, err := New(env.options()).PowerState(context.Background())
	if err != nil {
		t.Fatalf("PowerState 失败: %v", err)
	}
	if !st.RebootRequired || st.RebootRequiredSince == nil {
		t.Errorf("旧路径上的标志文件应被认出来: %+v", st)
	}
}

func TestCollectorPowerStateReadErrors(t *testing.T) {
	env := happyEnv(realPolicy2204)
	permErr := &os.PathError{Op: "stat", Path: DefaultRebootFlag, Err: os.ErrPermission}
	env.infos[DefaultRebootFlag] = fakeInfo{name: "reboot-required", mode: 0o644}
	env.statErrs = map[string]error{DefaultRebootFlag: permErr}

	// 读不到标志文件不能当成"不需要重启"。
	if _, err := New(env.options()).PowerState(context.Background()); err == nil {
		t.Fatal("stat 出错时应返回错误，而不是报告不需要重启")
	}

	// .pkgs 读失败、boot_id 形状不对：都是局部错误，状态照出。
	env2 := happyEnv(realPolicy2204)
	env2.infos[DefaultRebootFlag] = fakeInfo{name: "reboot-required", mode: 0o644}
	st, err := New(Options{
		Run: env2.options().Run,
		Read: func(path string) ([]byte, error) {
			switch path {
			case DefaultRebootPkgs:
				return nil, &os.PathError{Op: "open", Path: path, Err: os.ErrPermission}
			case DefaultBootIDPath:
				return []byte("not-a-uuid\n"), nil
			}
			return nil, os.ErrNotExist
		},
		Stat: env2.options().Stat,
	}).PowerState(context.Background())
	if err != nil {
		t.Fatalf("局部错误不该让 PowerState 失败: %v", err)
	}
	if st.RebootRequiredPackagesError == "" {
		t.Error(".pkgs 读失败应写进 reboot_required_packages_error")
	}
	if st.BootID != "" || st.BootIDError == "" {
		t.Errorf("坏的 boot_id 应写进 boot_id_error: %+v", st)
	}
	if !st.RebootRequired {
		t.Error("主标志文件读到了，仍应报告需要重启")
	}
}

func TestCollectorAvailable(t *testing.T) {
	env := happyEnv(realPolicy2204)
	if !New(env.options()).Available() {
		t.Error("dpkg-query 存在且可执行时应为 true")
	}

	// 没有执行位（比如被换成了一个普通文件）。
	env2 := happyEnv(realPolicy2204)
	env2.infos[DefaultDpkgQuery] = fakeInfo{name: "dpkg-query", mode: 0o644}
	if New(env2.options()).Available() {
		t.Error("没有执行位时不该报可用")
	}

	// 直接不存在。
	env3 := happyEnv(realPolicy2204)
	delete(env3.infos, DefaultDpkgQuery)
	if New(env3.options()).Available() {
		t.Error("不存在时不该报可用")
	}

	// 是个目录。
	env4 := happyEnv(realPolicy2204)
	env4.infos[DefaultDpkgQuery] = newDirInfo(time.Now())
	if New(env4.options()).Available() {
		t.Error("目录不该被当成可执行文件")
	}
}

func countIn(hay []string, needle string) int {
	n := 0
	for _, s := range hay {
		if s == needle {
			n++
		}
	}
	return n
}

// recordAptPolicy 把每次 apt-cache policy 的参数原样记下来。
func recordAptPolicy(env *fakeEnv) *[][]string {
	var calls [][]string
	base := env.run
	env.run = func(program string, args []string) (syscmd.Result, error) {
		if program == DefaultAptCache {
			calls = append(calls, append([]string(nil), args...))
		}
		return base(program, args)
	}
	return &calls
}

// TestCollectorCandidateCacheReuse 锁住候选缓存的省法：索引没变就不重问 apt-cache。
//
// 这一条是量出来的性能问题的解药：24.04（apt 2.8）上 `apt-cache policy` 对每个参数
// 都做一次全库模式匹配，731 个包名要 26 秒；22.04（apt 2.4）上 0.3 秒。如果每次采集
// 都重问一遍，面板每 30 秒就要烧掉一颗核 26 秒。
func TestCollectorCandidateCacheReuse(t *testing.T) {
	env := happyEnv(realPolicy2204)
	calls := recordAptPolicy(env)
	c := New(env.options())

	for i := 0; i < 3; i++ {
		snap, err := c.Packages(context.Background(), true)
		if err != nil {
			t.Fatalf("第 %d 次采集失败: %v", i+1, err)
		}
		if got := snap.Packages[1].CandidateVersion; got == "" {
			t.Fatalf("第 %d 次采集丢了候选版本", i+1)
		}
	}
	if len(*calls) != 1 {
		t.Fatalf("索引没变却问了 %d 次 apt-cache policy，期望 1 次", len(*calls))
	}
	if len((*calls)[0]) <= 1 {
		t.Fatalf("第一次应当带上包名: %v", (*calls)[0])
	}
	if got := countIn(env.runCalls, DefaultDpkgQuery); got != 3 {
		t.Fatalf("dpkg-query 调用 %d 次，期望 3 次（包列表每次都重读）", got)
	}
}

// TestCollectorCandidateCacheInvalidatesOnIndexChange：索引一变，全部候选作废。
//
// 不能只重算"已安装版本变了"的包：候选变化最常见的起因就是 apt update，
// 那时所有包的已安装版本一个字都没变。
func TestCollectorCandidateCacheInvalidatesOnIndexChange(t *testing.T) {
	env := happyEnv(realPolicy2204)
	calls := recordAptPolicy(env)
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	c := New(env.options())

	if _, err := c.Packages(context.Background(), true); err != nil {
		t.Fatalf("第一次采集失败: %v", err)
	}
	// 索引刷新：mtime 前进。
	env.infos[DefaultListsDir] = newDirInfo(now.Add(10 * time.Minute))
	if _, err := c.Packages(context.Background(), true); err != nil {
		t.Fatalf("第二次采集失败: %v", err)
	}
	if len(*calls) != 2 {
		t.Fatalf("索引变了应当重问，实际 %d 次", len(*calls))
	}
	if len((*calls)[1]) != len((*calls)[0]) {
		t.Fatalf("索引变了应当重算全部包名：第一次 %d 个，第二次 %d 个",
			len((*calls)[0]), len((*calls)[1]))
	}
}

// TestCollectorCandidateCacheRecomputesChangedPackage：只有换了版本的那个包重问。
func TestCollectorCandidateCacheRecomputesChangedPackage(t *testing.T) {
	env := happyEnv(realPolicy2204)
	calls := recordAptPolicy(env)
	base := env.run
	upgraded := strings.Replace(realDpkgQuery, "bash\t5.1-6ubuntu1.1", "bash\t5.1-6ubuntu1.2", 1)
	useUpgraded := false
	env.run = func(program string, args []string) (syscmd.Result, error) {
		if program == DefaultDpkgQuery && useUpgraded {
			return syscmd.Result{Program: program, Stdout: upgraded}, nil
		}
		return base(program, args)
	}
	c := New(env.options())

	if _, err := c.Packages(context.Background(), true); err != nil {
		t.Fatalf("第一次采集失败: %v", err)
	}
	useUpgraded = true
	if _, err := c.Packages(context.Background(), true); err != nil {
		t.Fatalf("第二次采集失败: %v", err)
	}
	if len(*calls) != 2 {
		t.Fatalf("期望两次 apt-cache policy，实际 %d 次", len(*calls))
	}
	got := (*calls)[1][1:]
	if len(got) != 1 || got[0] != "bash" {
		t.Fatalf("只该重问 bash，实际 %v", got)
	}
}

// TestCollectorPackagesSingleFlight：同时来的两个读取只跑一次采集。
//
// 这条不是洁癖：开机预热和第一个页面请求必然撞在一起，而 24.04 上一次冷采要二十多秒。
// 两次并发冷采不是"快一倍"，是两边抢 CPU 一起被拖过超时——用户看到的会是"代理不可用"。
func TestCollectorPackagesSingleFlight(t *testing.T) {
	base := happyEnv(realPolicy2204)
	entered := make(chan struct{}, 8)
	release := make(chan struct{})
	var dpkgCalls int32
	env := *base
	env.run = func(program string, args []string) (syscmd.Result, error) {
		if program == DefaultDpkgQuery {
			atomic.AddInt32(&dpkgCalls, 1)
			entered <- struct{}{}
			<-release
		}
		return base.run(program, args)
	}
	c := New(env.options())

	// 先把锁占住，保证下面两个调用者都已经进到 Packages 里面、且都还没跑起来，
	// 再放锁。谁先拿到锁谁去采集，另一个必然看见 inflight 而等着——不靠 sleep 赌时间。
	c.mu.Lock()
	type result struct {
		snap Snapshot
		err  error
	}
	results := make(chan result, 2)
	for i := 0; i < 2; i++ {
		go func() {
			snap, err := c.Packages(context.Background(), false)
			results <- result{snap: snap, err: err}
		}()
	}
	c.mu.Unlock()

	<-entered
	close(release)

	first, second := <-results, <-results
	if n := atomic.LoadInt32(&dpkgCalls); n != 1 {
		t.Fatalf("并发读取应当只采集一次，实际 dpkg-query 跑了 %d 次", n)
	}
	for i, r := range []result{first, second} {
		if r.err != nil {
			t.Fatalf("第 %d 个调用失败: %v", i+1, r.err)
		}
		if len(r.snap.Packages) != 8 {
			t.Fatalf("第 %d 个调用拿到 %d 个包", i+1, len(r.snap.Packages))
		}
	}
	// 等的那个人拿到的是正在跑的那次采集的结果，不是他自己另采的一份。
	if !first.snap.CollectedAt.Equal(second.snap.CollectedAt) {
		t.Fatalf("两个调用拿到了不同的快照：%v / %v", first.snap.CollectedAt, second.snap.CollectedAt)
	}
}

// TestCollectorPackagesFreshWaitsForInflight：fresh=true 也不并发冷采，但绝不拿旧快照充数。
func TestCollectorPackagesFreshWaitsForInflight(t *testing.T) {
	base := happyEnv(realPolicy2204)
	entered := make(chan struct{}, 8)
	release := make(chan struct{})
	var dpkgCalls int32
	env := *base
	env.run = func(program string, args []string) (syscmd.Result, error) {
		if program == DefaultDpkgQuery {
			atomic.AddInt32(&dpkgCalls, 1)
			entered <- struct{}{}
			<-release
		}
		return base.run(program, args)
	}
	c := New(env.options())

	// 先放一份旧快照进缓存：fresh=true 不许用它。
	stale := time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC)
	c.cached = Snapshot{CollectedAt: stale}
	c.hasCache = true

	c.mu.Lock()
	done := make(chan Snapshot, 1)
	go func() {
		snap, err := c.Packages(context.Background(), true)
		if err != nil {
			done <- Snapshot{}
			return
		}
		done <- snap
	}()
	c.mu.Unlock()

	<-entered
	close(release)
	snap := <-done
	if n := atomic.LoadInt32(&dpkgCalls); n != 1 {
		t.Fatalf("dpkg-query 跑了 %d 次", n)
	}
	if !snap.CollectedAt.After(stale) {
		t.Fatalf("fresh=true 拿到了旧快照: %v", snap.CollectedAt)
	}
}

// realPolicy2404Qualified 是 24.04 上 `apt-cache policy bind9-libs:amd64 libc6:amd64 libc6:i386`
// 的输出形状：**块头把架构限定去掉了**，问了三个名字只回两个块，而且两个 libc6 块同名。
// 这三件事都是真机看出来的，不是造的。
const realPolicy2404Qualified = `bind9-libs:
  Installed: 1:9.18.39-0ubuntu0.24.04.8
  Candidate: 1:9.18.39-0ubuntu0.24.04.8
  Version table:
 *** 1:9.18.39-0ubuntu0.24.04.8 500
        500 http://archive.ubuntu.com/ubuntu noble-updates/main amd64 Packages
        500 http://security.ubuntu.com/ubuntu noble-security/main amd64 Packages
        100 /var/lib/dpkg/status
     1:9.18.24-0ubuntu5 500
        500 http://archive.ubuntu.com/ubuntu noble/main amd64 Packages
libc6:
  Installed: 2.39-0ubuntu8.6
  Candidate: 2.39-0ubuntu8.6
  Version table:
 *** 2.39-0ubuntu8.6 500
        500 http://archive.ubuntu.com/ubuntu noble-updates/main amd64 Packages
        100 /var/lib/dpkg/status
`

const dpkg2404Qualified = "bind9-libs:amd64\t1:9.18.39-0ubuntu0.24.04.8\tamd64\tii \n" +
	"libc6:amd64\t2.39-0ubuntu8.6\tamd64\tii \n"

// TestCollectorArchQualifiedCandidate：带架构限定的包名也能拿到候选。
//
// 这一条是被真机抓出来的：`bind9-libs:amd64` 明明有候选、而且就等于已安装版本，
// 页面却写着"候选读不到"——731 个包里就这一个多架构包，所以一眼看不出是通病。
func TestCollectorArchQualifiedCandidate(t *testing.T) {
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	env := &fakeEnv{
		run: func(program string, args []string) (syscmd.Result, error) {
			switch program {
			case DefaultDpkgQuery:
				return syscmd.Result{Program: program, Stdout: dpkg2404Qualified}, nil
			case DefaultAptCache:
				return syscmd.Result{Program: program, Stdout: realPolicy2404Qualified}, nil
			}
			return syscmd.Result{Program: program, ExitCode: 127}, fmt.Errorf("unexpected %s", program)
		},
		files: map[string][]byte{
			DefaultBootIDPath: []byte("ab2eb5e8-c1b4-4a00-a3d0-33ced7870abc\n"),
		},
		infos: map[string]fakeInfo{
			DefaultDpkgQuery: newExecInfo(),
			DefaultListsDir:  newDirInfo(now),
		},
	}
	c := New(env.options())

	snap, err := c.Packages(context.Background(), true)
	if err != nil {
		t.Fatalf("Packages 失败: %v", err)
	}
	byName := map[string]Package{}
	for _, p := range snap.Packages {
		byName[p.Name] = p
	}
	// 唯一装着的是 amd64：基名不歧义，可以退回基名找候选。
	if p := byName["bind9-libs:amd64"]; p.CandidateVersion != "1:9.18.39-0ubuntu0.24.04.8" ||
		!containsString(p.CandidateSuites, "noble-updates") {
		t.Errorf("bind9-libs:amd64 的候选应能读到，实际 %+v", p)
	}
	if p := byName["libc6:amd64"]; p.CandidateVersion != "2.39-0ubuntu8.6" {
		t.Errorf("libc6:amd64 = %+v", p)
	}
}

// TestCollectorArchQualifiedAmbiguousStaysUnknown：同名不同架构都装着时不猜。
func TestCollectorArchQualifiedAmbiguousStaysUnknown(t *testing.T) {
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	both := dpkg2404Qualified + "libc6:i386\t2.39-0ubuntu8.6\ti386\tii \n"
	env := &fakeEnv{
		run: func(program string, args []string) (syscmd.Result, error) {
			switch program {
			case DefaultDpkgQuery:
				return syscmd.Result{Program: program, Stdout: both}, nil
			case DefaultAptCache:
				return syscmd.Result{Program: program, Stdout: realPolicy2404Qualified}, nil
			}
			return syscmd.Result{Program: program, ExitCode: 127}, fmt.Errorf("unexpected %s", program)
		},
		files: map[string][]byte{
			DefaultBootIDPath: []byte("ab2eb5e8-c1b4-4a00-a3d0-33ced7870abc\n"),
		},
		infos: map[string]fakeInfo{
			DefaultDpkgQuery: newExecInfo(),
			DefaultListsDir:  newDirInfo(now),
		},
	}
	c := New(env.options())

	snap, err := c.Packages(context.Background(), true)
	if err != nil {
		t.Fatalf("Packages 失败: %v", err)
	}
	byName := map[string]Package{}
	for _, p := range snap.Packages {
		byName[p.Name] = p
	}
	// 两个架构都在：`apt-cache policy` 的两个 `libc6:` 块在解析时已经合成一个，
	// 那个块只说得出其中一个架构。宁可两个都记"读不到"，也不能把 amd64 的版本
	// 写到 i386 上——那是编数据。
	for _, name := range []string{"libc6:amd64", "libc6:i386"} {
		if p := byName[name]; p.CandidateVersion != "" {
			t.Errorf("%s 在同名不同架构都装着时不该有候选，实际 %q", name, p.CandidateVersion)
		}
	}
	// 不受影响的那个照旧。
	if p := byName["bind9-libs:amd64"]; p.CandidateVersion != "1:9.18.39-0ubuntu0.24.04.8" {
		t.Errorf("bind9-libs:amd64 = %+v", p)
	}
}
