package proxies

// P10.2 写入流水线用例：生成 → 原子写入 → nginx -t 门禁 → reload → 失败回滚 → 读回核对。
//
// 这一层的每一条断言都对应一条不能让步的纪律：
//
//  1. **正式文件只通过 rename 出现。** 候选文件写坏了，正式文件一个字节都没动。
//  2. **nginx -t 不过就回到原样，而且回滚后的树也要再校验一次。** 校验失败不是
//     “配置没写成”，是“这台机器现在的状态需要被证实”。
//  3. **没变就不写、不备份、不 reload。** 幂等是这个动作能被反复点、能被自动重试的
//     全部基础。
//  4. **明文密码不进文件、不进 argv、不进报告。**
//
// 文件系统用内存树（memFS）而不是临时目录：托管路径是 POSIX 绝对路径（会被原样写进
// nginx 配置），在开发宿主（Windows）上造不出来。真实 /etc/nginx 上的落盘与 nginx -t
// 由虚拟机验收脚本 tests/integration/p10-2-*.sh 证明——那一条不能由这些用例代替。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"upanel/internal/httpx"
	"upanel/internal/nginx"
	"upanel/internal/storage"
	"upanel/internal/syscmd"
)

// ---- 测试用的固定值 ----

const (
	applySiteID   = "site_0123456789abcdef0123456789abcdef"
	applyRoot     = "/etc/nginx"
	panelTestPort = 9443
	applyDomain   = "app.example.com"
)

func applyConfPath(domain string) string {
	return path.Join(applyRoot, "conf.d", "upanel-"+domain+".conf")
}

func applyAuthPath() string {
	return path.Join(applyRoot, AuthDirName, applySiteID+".htpasswd")
}

// applyDefinition 返回一份完整的、可写入的站点定义（public 模式）。
func applyDefinition(domain string) Definition {
	return Definition{
		Domain:             domain,
		UpstreamTargetID:   "local_3f2a1b",
		UpstreamScheme:     SchemeHTTP,
		UpstreamPort:       8080,
		AccessMode:         AccessPublic,
		MaxBodyBytes:       DefaultMaxBodyBytes,
		ReadTimeoutSeconds: DefaultReadTimeoutSeconds,
		Enabled:            true,
	}
}

func applyParamsOf(def Definition, version int) ApplyParams {
	return ApplyParams{
		SiteID:     applySiteID,
		Version:    version,
		Definition: &def,
		PanelPort:  panelTestPort,
	}
}

// ---- memFS：一棵内存里的配置树 ----

// memFS 是一个最小的 ConfigFS 实现，键是 POSIX 路径。
//
// 除了读写，它还记下**每个操作的顺序**（ops）：流水线里“候选先写、再 rename、
// 再 fsync 目录”这个顺序本身就是不变量，没有顺序就没法断言正式文件从不是被直接写出来的。
type memFS struct {
	mu     sync.Mutex
	files  map[string][]byte
	dirs   map[string]bool
	modes  map[string]os.FileMode
	writes map[string]int
	ops    []string
	// failWrite 让某一次写入失败（用来证明回滚失败时如实报告）。
	failWrite func(name string, nth int) error
}

func newMemFS() *memFS {
	return &memFS{
		files:  make(map[string][]byte),
		dirs:   make(map[string]bool),
		modes:  make(map[string]os.FileMode),
		writes: make(map[string]int),
	}
}

// key 把路径归一成 POSIX 形式，顺便吃掉宿主的路径分隔符差异。
func (m *memFS) key(name string) string {
	return path.Clean(strings.ReplaceAll(name, "\\", "/"))
}

func (m *memFS) seed(name, content string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.files[m.key(name)] = []byte(content)
	m.mkdirs(path.Dir(m.key(name)))
}

func (m *memFS) mkdirs(dir string) {
	for dir != "/" && dir != "." && dir != "" {
		m.dirs[dir] = true
		dir = path.Dir(dir)
	}
}

func (m *memFS) exists(name string) bool {
	_, ok := m.files[m.key(name)]
	return ok
}

func (m *memFS) content(name string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return string(m.files[m.key(name)])
}

func (m *memFS) modeOf(name string) os.FileMode {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.modes[m.key(name)]
}

func (m *memFS) writeCount(name string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.writes[m.key(name)]
}

// opIndex 返回某个操作在 ops 里的下标，没找到返回 -1。
func (m *memFS) opIndex(match func(op string) bool) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, op := range m.ops {
		if match(op) {
			return i
		}
	}
	return -1
}

func (m *memFS) fs() ConfigFS {
	return ConfigFS{
		ReadFile: func(name string) ([]byte, error) {
			m.mu.Lock()
			defer m.mu.Unlock()
			k := m.key(name)
			if data, ok := m.files[k]; ok {
				return append([]byte(nil), data...), nil
			}
			if m.dirs[k] {
				return nil, &os.PathError{Op: "read", Path: name, Err: fs.ErrInvalid}
			}
			return nil, &os.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
		},
		WriteFileSync: func(name string, data []byte, perm os.FileMode) error {
			m.mu.Lock()
			defer m.mu.Unlock()
			k := m.key(name)
			m.writes[k]++
			m.ops = append(m.ops, "write "+k)
			if m.failWrite != nil {
				if err := m.failWrite(k, m.writes[k]); err != nil {
					return err
				}
			}
			m.mkdirs(path.Dir(k))
			m.files[k] = append([]byte(nil), data...)
			m.modes[k] = perm
			return nil
		},
		Rename: func(from, to string) error {
			m.mu.Lock()
			defer m.mu.Unlock()
			f, tk := m.key(from), m.key(to)
			data, ok := m.files[f]
			if !ok {
				return &os.PathError{Op: "rename", Path: from, Err: fs.ErrNotExist}
			}
			m.ops = append(m.ops, "rename "+f+" -> "+tk)
			m.files[tk] = data
			if mode, ok := m.modes[f]; ok {
				m.modes[tk] = mode
				delete(m.modes, f)
			}
			delete(m.files, f)
			return nil
		},
		FsyncDir: func(dir string) error {
			m.mu.Lock()
			defer m.mu.Unlock()
			k := m.key(dir)
			m.ops = append(m.ops, "fsync "+k)
			if !m.dirs[k] {
				return &os.PathError{Op: "open", Path: dir, Err: fs.ErrNotExist}
			}
			return nil
		},
		MkdirAll: func(dir string, _ os.FileMode) error {
			m.mu.Lock()
			defer m.mu.Unlock()
			k := m.key(dir)
			m.ops = append(m.ops, "mkdir "+k)
			m.mkdirs(k)
			return nil
		},
		Remove: func(name string) error {
			m.mu.Lock()
			defer m.mu.Unlock()
			k := m.key(name)
			m.ops = append(m.ops, "remove "+k)
			if _, ok := m.files[k]; !ok {
				return &os.PathError{Op: "remove", Path: name, Err: fs.ErrNotExist}
			}
			delete(m.files, k)
			delete(m.modes, k)
			return nil
		},
		Stat: func(name string) (os.FileInfo, error) {
			m.mu.Lock()
			defer m.mu.Unlock()
			k := m.key(name)
			if data, ok := m.files[k]; ok {
				return memInfo{name: path.Base(k), size: int64(len(data))}, nil
			}
			if m.dirs[k] {
				return memInfo{name: path.Base(k), dir: true}, nil
			}
			return nil, &os.PathError{Op: "stat", Path: name, Err: fs.ErrNotExist}
		},
		ReadDir: func(name string) ([]os.DirEntry, error) {
			m.mu.Lock()
			defer m.mu.Unlock()
			k := m.key(name)
			if !m.dirs[k] {
				return nil, &os.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
			}
			prefix := k + "/"
			var out []os.DirEntry
			for file := range m.files {
				if strings.HasPrefix(file, prefix) && !strings.Contains(strings.TrimPrefix(file, prefix), "/") {
					out = append(out, memEntry{name: strings.TrimPrefix(file, prefix)})
				}
			}
			sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
			return out, nil
		},
		// Chown 为 nil：代理层的代码在 uid<0 时会跳过它，测试里不需要。
	}
}

// memInfo / memEntry 是 os.FileInfo / os.DirEntry 的最小实现。
type memInfo struct {
	name string
	size int64
	dir  bool
}

func (i memInfo) Name() string       { return i.name }
func (i memInfo) Size() int64        { return i.size }
func (i memInfo) Mode() os.FileMode  { return 0o644 }
func (i memInfo) ModTime() time.Time { return time.Time{} }
func (i memInfo) IsDir() bool        { return i.dir }
func (i memInfo) Sys() any           { return nil }

type memEntry struct{ name string }

func (e memEntry) Name() string               { return e.name }
func (e memEntry) IsDir() bool                { return false }
func (e memEntry) Type() os.FileMode          { return 0 }
func (e memEntry) Info() (os.FileInfo, error) { return memInfo{name: e.name}, nil }

// ---- 假的事实源：从 memFS 里读 ----

// treeFacts 把 memFS 里的 conf.d 变成 nginx.Facts。
//
// 它刻意不复用真实的解析器：单元测试要断言的是流水线的顺序与判决，不是 Nginx 语法
// 解析（那部分在 internal/nginx 里另有真实配置的用例）。但它计算的 SHA256 与
// server_name 声明足够真——读回核对正是拿这两样东西比对的。
type treeFacts struct {
	fs       ConfigFS
	paths    ConfigPaths
	unit     string
	err      error
	override *nginx.Facts

	collects    int
	invalidates int
}

func (f *treeFacts) Snapshot(ctx context.Context) (nginx.Facts, error) { return f.Collect(ctx) }

func (f *treeFacts) Collect(context.Context) (nginx.Facts, error) {
	f.collects++
	if f.err != nil {
		return nginx.Facts{}, f.err
	}
	if f.override != nil {
		return *f.override, nil
	}
	unit := f.unit
	if unit == "" {
		unit = "active"
	}
	facts := nginx.Facts{Available: true, Version: "1.24.0", UnitState: unit, ConfigRoot: applyRoot}
	entries, err := f.fs.ReadDir(f.paths.ConfDir())
	if err != nil {
		if os.IsNotExist(err) {
			return facts, nil
		}
		return nginx.Facts{}, err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		full := path.Join(f.paths.ConfDir(), e.Name())
		data, err := f.fs.ReadFile(full)
		file := nginx.ConfigFile{
			Path:      full,
			Managed:   nginx.IsManagedName(e.Name()),
			SizeBytes: int64(len(data)),
		}
		if err != nil {
			file.Error = err.Error()
			facts.Files = append(facts.Files, file)
			continue
		}
		file.SHA256 = hashOf(string(data))
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "server_name ") {
				continue
			}
			srv := nginx.ServerBlock{Line: 1, NamesLine: 1}
			for _, name := range strings.Fields(strings.TrimSuffix(strings.TrimPrefix(line, "server_name "), ";")) {
				srv.Names = append(srv.Names, nginx.Classify(name))
			}
			file.Servers = append(file.Servers, srv)
		}
		facts.Files = append(facts.Files, file)
	}
	return facts, nil
}

func (f *treeFacts) Invalidate() { f.invalidates++ }

// ---- 假的命令执行器 ----

// fakeRunner 按调用序列回答 nginx -t / nginx -s reload / systemctl is-active。
//
// testExits 按顺序消费：第 n 次 nginx -t 用第 n 个退出码，用完之后重复最后一个。
// 回滚路径需要“第一次不过、第二次（回滚后）通过”，就是靠这个序列表达的。
type fakeRunner struct {
	calls      []string
	testExits  []int
	testCalls  int
	reloadExit int
	unitOut    string
	runErr     error
	// afterReload 在 reload 成功之后调用一次：模拟"Nginx 换了新一代 worker，
	// 从此读的是新证书"——证书轮换用例要靠它把"reload 之前/之后"分开。
	afterReload func()
}

func (r *fakeRunner) Run(_ context.Context, spec syscmd.Spec, _ syscmd.Limits) (syscmd.Result, error) {
	key := spec.Program + " " + strings.Join(spec.Args, " ")
	r.calls = append(r.calls, key)
	if r.runErr != nil {
		return syscmd.Result{Program: spec.Program}, r.runErr
	}
	switch {
	case spec.Program == storage.DefaultSystemctlPath:
		out := r.unitOut
		if out == "" {
			out = "active"
		}
		return syscmd.Result{Program: spec.Program, Stdout: out + "\n"}, nil
	case len(spec.Args) == 1 && spec.Args[0] == "-t":
		r.testCalls++
		exit := 0
		if len(r.testExits) > 0 {
			i := r.testCalls - 1
			if i >= len(r.testExits) {
				i = len(r.testExits) - 1
			}
			exit = r.testExits[i]
		}
		res := syscmd.Result{Program: spec.Program, ExitCode: exit}
		if exit != 0 {
			res.Stderr = "nginx: [emerg] invalid parameter \"typo\""
		}
		return res, nil
	case len(spec.Args) == 2 && spec.Args[0] == "-s" && spec.Args[1] == "reload":
		if r.reloadExit == 0 && r.afterReload != nil {
			r.afterReload()
		}
		return syscmd.Result{Program: spec.Program, ExitCode: r.reloadExit}, nil
	}
	return syscmd.Result{}, fmt.Errorf("测试里没有预料到的命令：%s", key)
}

func (r *fakeRunner) count(prefix string) int {
	n := 0
	for _, c := range r.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

// reset 清空调用记录，让“这一段之后调用了几次”可以被单独断言。
func (r *fakeRunner) reset() {
	r.calls = nil
	r.testCalls = 0
}

// ---- 假的秘密存储 ----

type fakeSecrets struct {
	values map[string]string
	err    error
	refs   []string
}

func (s *fakeSecrets) Resolve(_ context.Context, ref string) ([]byte, error) {
	s.refs = append(s.refs, ref)
	if s.err != nil {
		return nil, s.err
	}
	v, ok := s.values[ref]
	if !ok {
		return nil, errors.New("秘密不存在：" + ref)
	}
	return []byte(v), nil
}

// ---- 夹具 ----

type applyFixture struct {
	t       *testing.T
	paths   ConfigPaths
	fsys    *memFS
	facts   *treeFacts
	runner  *fakeRunner
	secrets *fakeSecrets
	applier *Applier
}

func applyFixedNow() time.Time {
	return time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)
}

func newApplyFixture(t *testing.T) *applyFixture {
	t.Helper()
	fsys := newMemFS()
	paths := ConfigPaths{
		Root:      applyRoot,
		NginxPath: DefaultNginxPath,
		BackupDir: DefaultBackupDir,
	}
	facts := &treeFacts{paths: paths}
	facts.fs = fsys.fs()
	runner := &fakeRunner{}
	secrets := &fakeSecrets{values: map[string]string{}}
	applier, err := NewApplier(ApplierOptions{
		Facts:   facts,
		Secrets: secrets,
		Runner:  runner,
		Paths:   paths,
		FS:      fsys.fs(),
		Now:     applyFixedNow,
		// 不开上游发现：TargetExists 只产生告警，开着会让每条用例都要人造目标。
	})
	if err != nil {
		t.Fatalf("构造写入侧失败: %v", err)
	}
	return &applyFixture{t: t, paths: paths, fsys: fsys, facts: facts,
		runner: runner, secrets: secrets, applier: applier}
}

func (f *applyFixture) execute(p ApplyParams) (WriteReport, error) {
	f.t.Helper()
	return f.applier.ExecuteApply(context.Background(), WriteRequest{
		Action: ActionProxyApply,
		Params: p,
	})
}

// requireHTTPError 断言拒绝的状态码与错误码——拒绝的原因不能只看文本。
func requireHTTPError(t *testing.T, err error, wantStatus int, wantCode httpx.Code, wantSubstr string) {
	t.Helper()
	if err == nil {
		t.Fatalf("期望被拒绝，实际成功了")
	}
	var herr *HTTPError
	if !errors.As(err, &herr) {
		t.Fatalf("错误类型 = %T，期望 *HTTPError：%v", err, err)
	}
	if herr.Status != wantStatus {
		t.Errorf("状态码 = %d，期望 %d（%s）", herr.Status, wantStatus, herr.Message)
	}
	if herr.Code != wantCode {
		t.Errorf("错误码 = %q，期望 %q", herr.Code, wantCode)
	}
	if wantSubstr != "" && !strings.Contains(herr.Message, wantSubstr) {
		t.Errorf("错误说明里应当提到 %q：%s", wantSubstr, herr.Message)
	}
}

// blockerCodes 把阻塞码拼成字符串，便于断言。
func blockerCodes(list []Blocker) string {
	codes := make([]string, 0, len(list))
	for _, b := range list {
		codes = append(codes, b.Code)
	}
	return strings.Join(codes, ",")
}

// ---- 用例 ----

// TestApplyWritesConfigAtomically 第一次写入：候选 → rename → fsync，正式文件从没被直接写过。
func TestApplyWritesConfigAtomically(t *testing.T) {
	f := newApplyFixture(t)
	confPath := applyConfPath(applyDomain)
	candidate := confPath + ".upanel-new"

	report, err := f.execute(applyParamsOf(applyDefinition(applyDomain), 1))
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}

	if report.Outcome != OutcomeVerified {
		t.Errorf("outcome = %q，期望 verified（%+v）", report.Outcome, report.Observed)
	}
	if got := report.Data["file_state"]; got != FileStateWritten {
		t.Errorf("file_state = %v，期望 %s", got, FileStateWritten)
	}
	if got := report.Data["reload"]; got != ReloadDone {
		t.Errorf("reload = %v，期望 %s", got, ReloadDone)
	}
	if got := report.Data["served"]; got != true {
		t.Errorf("served = %v，期望 true", got)
	}
	if got := report.Data["tested"]; got != true {
		t.Errorf("tested = %v，期望 true", got)
	}

	// 内容就是渲染结果，一个字节都不差。
	want, err := RenderSite(applySiteID, applyDefinition(applyDomain), RenderOptions{HtpasswdPath: applyAuthPath()})
	if err != nil {
		t.Fatalf("渲染失败: %v", err)
	}
	if got := f.fsys.content(confPath); got != want {
		t.Errorf("落盘内容与渲染结果不一致：\n%s", got)
	}
	if got := report.Data["config_hash"]; got != hashOf(want) {
		t.Errorf("config_hash = %v，期望 %q", got, hashOf(want))
	}

	// 原子性：正式路径一次都没被 Write 过，只通过 rename 出现。
	if n := f.fsys.writeCount(confPath); n != 0 {
		t.Errorf("正式文件被直接写了 %d 次：候选 + rename 的意义全丢了", n)
	}
	if n := f.fsys.writeCount(candidate); n != 1 {
		t.Errorf("候选文件写了 %d 次，期望 1", n)
	}
	writeIdx := f.fsys.opIndex(func(op string) bool { return op == "write "+candidate })
	renameIdx := f.fsys.opIndex(func(op string) bool { return strings.HasPrefix(op, "rename ") && strings.HasSuffix(op, "-> "+confPath) })
	fsyncIdx := f.fsys.opIndex(func(op string) bool { return op == "fsync "+path.Dir(confPath) })
	if writeIdx < 0 || renameIdx < 0 || fsyncIdx < 0 {
		t.Fatalf("缺少写候选 / rename / fsync 目录中的某一步：%v", f.fsys.ops)
	}
	if !(writeIdx < renameIdx && renameIdx < fsyncIdx) {
		t.Errorf("顺序不对，期望 写候选 → rename → fsync 目录：%v", f.fsys.ops)
	}

	// 权限钉死 644：nginx 要能读，别的进程不该能写。
	if mode := f.fsys.modeOf(confPath); mode != 0o644 {
		t.Errorf("托管文件权限 = %o，期望 644", mode)
	}

	// 首次写入没有旧文件，所以不该产生备份。
	if _, ok := report.Observed["backup"]; ok {
		t.Errorf("首次写入却产生了备份：%v", report.Observed["backup"])
	}

	// 命令序列：先校验，再问服务状态，最后 reload。
	if got := f.runner.count(DefaultNginxPath + " -t"); got != 1 {
		t.Errorf("nginx -t 调用 %d 次，期望 1", got)
	}
	if got := f.runner.count(storage.DefaultSystemctlPath + " is-active"); got != 1 {
		t.Errorf("systemctl is-active 调用 %d 次，期望 1", got)
	}
	if got := f.runner.count(DefaultNginxPath + " -s reload"); got != 1 {
		t.Errorf("nginx -s reload 调用 %d 次，期望 1", got)
	}
	if len(f.runner.calls) != 3 {
		t.Errorf("命令序列 = %v，期望正好三条", f.runner.calls)
	}
}

// TestApplyIsIdempotentOnReplay 同样内容再应用一次：不写、不备份、不 reload。
func TestApplyIsIdempotentOnReplay(t *testing.T) {
	f := newApplyFixture(t)
	confPath := applyConfPath(applyDomain)

	if _, err := f.execute(applyParamsOf(applyDefinition(applyDomain), 1)); err != nil {
		t.Fatalf("第一次执行失败: %v", err)
	}
	firstContent := f.fsys.content(confPath)
	writeCount := f.fsys.writeCount(confPath + ".upanel-new")

	report, err := f.execute(applyParamsOf(applyDefinition(applyDomain), 2))
	if err != nil {
		t.Fatalf("第二次执行失败: %v", err)
	}
	if got := report.Data["file_state"]; got != FileStateUnchanged {
		t.Errorf("file_state = %v，期望 %s", got, FileStateUnchanged)
	}
	if got := report.Data["reload"]; got != ReloadSkippedNoChange {
		t.Errorf("reload = %v，期望 %s", got, ReloadSkippedNoChange)
	}
	// served 仍然为 true：内容没变、服务在跑，站点此刻就是在提供服务。
	if got := report.Data["served"]; got != true {
		t.Errorf("served = %v，期望 true", got)
	}
	if report.Outcome != OutcomeVerified {
		t.Errorf("outcome = %q，期望 verified", report.Outcome)
	}
	if _, ok := report.Observed["backup"]; ok {
		t.Error("内容没变却产生了备份")
	}
	if got := f.fsys.content(confPath); got != firstContent {
		t.Error("内容没变却把文件重写了")
	}
	if got := f.fsys.writeCount(confPath + ".upanel-new"); got != writeCount {
		t.Errorf("内容没变却又写了候选文件（%d → %d）", writeCount, got)
	}
	if got := f.runner.count(DefaultNginxPath + " -s reload"); got != 1 {
		t.Errorf("reload 调用 %d 次，期望仍然是 1（第二次不该 reload）", got)
	}
	// 没变也要过一遍 nginx -t：这份配置在这台机器上是不是好的，每次都要能回答。
	if got := f.runner.count(DefaultNginxPath + " -t"); got != 2 {
		t.Errorf("nginx -t 调用 %d 次，期望 2", got)
	}
}

// TestApplyTestFailureRestoresPreviousFile 候选项不过 nginx -t：旧文件原样回来，并且回滚后的树也被校验。
func TestApplyTestFailureRestoresPreviousFile(t *testing.T) {
	f := newApplyFixture(t)
	confPath := applyConfPath(applyDomain)

	// 先建成一份好配置。
	if _, err := f.execute(applyParamsOf(applyDefinition(applyDomain), 1)); err != nil {
		t.Fatalf("建立初始配置失败: %v", err)
	}
	original := f.fsys.content(confPath)

	// 再改一次（读超时变化 → 内容变化 → 会真的写入），让这次 nginx -t 失败。
	// nginx -t 的第二次（回滚之后）必须通过——那正是“回滚之后这台机器是好的”这句话的证据。
	// 计数从这一刻重新开始：下面断言的是这一次执行做了几步，不是夹具搭了几次。
	f.runner.reset()
	f.runner.testExits = []int{1, 0}
	changed := applyDefinition(applyDomain)
	changed.ReadTimeoutSeconds = 120

	report, err := f.execute(applyParamsOf(changed, 2))
	requireHTTPError(t, err, 409, httpx.CodeDependencyBlocked, "已回滚")
	if report.Outcome != "" {
		t.Errorf("失败却给出了 outcome %q", report.Outcome)
	}

	if got := f.fsys.content(confPath); got != original {
		t.Errorf("旧配置没有回来：\n%s", got)
	}
	if got := f.runner.count(DefaultNginxPath + " -t"); got != 2 {
		t.Errorf("nginx -t 调用 %d 次，期望 2（候选一次、回滚后一次）", got)
	}
	// 校验失败绝不能 reload：那会让 nginx 用上刚被回滚掉的东西。
	if got := f.runner.count(DefaultNginxPath + " -s reload"); got != 0 {
		t.Errorf("reload 调用 %d 次，期望 0（失败那次不 reload）", got)
	}
	// 备份必须在替换之前生成：它是回滚之外最后一道人工抓手。
	backups, err := f.fsys.fs().ReadDir(DefaultBackupDir)
	if err != nil {
		t.Fatalf("读备份目录失败: %v", err)
	}
	if len(backups) != 1 {
		t.Errorf("备份数 = %d，期望 1：%v", len(backups), backups)
	}
	if got := f.fsys.content(path.Join(DefaultBackupDir, backups[0].Name())); got != original {
		t.Error("备份内容不是替换前的旧文件")
	}
}

// TestApplyRollbackRevalidationFailureIsReportedAs500 回滚后的树仍然坏：这是事故，不是参数错误。
func TestApplyRollbackRevalidationFailureIsReportedAs500(t *testing.T) {
	f := newApplyFixture(t)
	// 一开始就有一份“坏掉的”其他人配置：机器本来就有问题。
	f.fsys.seed(path.Join(applyRoot, "conf.d", "broken.conf"), "server { listen 80 }\n")
	f.runner.testExits = []int{1, 1}

	_, err := f.execute(applyParamsOf(applyDefinition(applyDomain), 1))
	requireHTTPError(t, err, 500, httpx.CodeCommandFailed, "回滚后")
	if got := f.runner.count(DefaultNginxPath + " -t"); got != 2 {
		t.Errorf("nginx -t 调用 %d 次，期望 2", got)
	}
	// 这次是我们的文件新建后回滚，所以它必须被删掉（回到“从来没写过”）。
	if f.fsys.exists(applyConfPath(applyDomain)) {
		t.Error("回滚后托管文件仍然存在：新建的回滚应当把它删掉")
	}
}

// TestApplyRollbackFailureIsHonest 回滚本身失败：绝不假装成功，给出人工恢复路径。
func TestApplyRollbackFailureIsHonest(t *testing.T) {
	f := newApplyFixture(t)
	confPath := applyConfPath(applyDomain)
	if _, err := f.execute(applyParamsOf(applyDefinition(applyDomain), 1)); err != nil {
		t.Fatalf("建立初始配置失败: %v", err)
	}
	original := f.fsys.content(confPath)

	// 回滚要写回正式文件；让这次写入失败。前提是这次确实会改文件——
	// 定义不变时流水线根本不写，也就没有回滚可言。
	f.fsys.failWrite = func(name string, _ int) error {
		if name == confPath {
			return errors.New("磁盘只读")
		}
		return nil
	}
	f.runner.reset()
	f.runner.testExits = []int{1}

	changed := applyDefinition(applyDomain)
	changed.ReadTimeoutSeconds = 120
	_, err := f.execute(applyParamsOf(changed, 2))
	requireHTTPError(t, err, 500, httpx.CodeCommandFailed, "回滚失败")
	if got := f.fsys.content(confPath); got == original {
		t.Error("这条用例的前提是回滚没能恢复文件")
	}
	// 失败时也绝不 reload。
	if got := f.runner.count(DefaultNginxPath + " -s reload"); got != 0 {
		t.Errorf("reload 调用 %d 次，期望 0", got)
	}
}

// TestApplyDisableRemovesManagedFile enabled=false 就是把文件拿掉，同一个流水线。
func TestApplyDisableRemovesManagedFile(t *testing.T) {
	f := newApplyFixture(t)
	confPath := applyConfPath(applyDomain)

	if _, err := f.execute(applyParamsOf(applyDefinition(applyDomain), 1)); err != nil {
		t.Fatalf("建立配置失败: %v", err)
	}
	disabled := applyDefinition(applyDomain)
	disabled.Enabled = false

	report, err := f.execute(applyParamsOf(disabled, 2))
	if err != nil {
		t.Fatalf("停用失败: %v", err)
	}
	if f.fsys.exists(confPath) {
		t.Error("停用之后托管文件仍然在 conf.d 里：它会被 include，站点还在被代理")
	}
	if got := report.Data["file_state"]; got != FileStateRemoved {
		t.Errorf("file_state = %v，期望 %s", got, FileStateRemoved)
	}
	if got := report.Data["served"]; got != false {
		t.Errorf("served = %v，期望 false", got)
	}
	if got := report.Data["reload"]; got != ReloadDone {
		t.Errorf("reload = %v，期望 %s", got, ReloadDone)
	}
	if report.Outcome != OutcomeVerified {
		t.Errorf("outcome = %q，期望 verified", report.Outcome)
	}
	// 停用也要备份：用户改主意时文件还在。
	backups, err := f.fsys.fs().ReadDir(DefaultBackupDir)
	if err != nil || len(backups) != 1 {
		t.Fatalf("备份 = %v（err=%v），期望 1 份", backups, err)
	}
}

// TestApplyRenameRemovesStaleManagedFile 域名改名之后，旧文件必须被清掉，否则旧域名继续生效。
func TestApplyRenameRemovesStaleManagedFile(t *testing.T) {
	f := newApplyFixture(t)
	oldPath := applyConfPath("old.example.com")
	newPath := applyConfPath("new.example.com")

	if _, err := f.execute(applyParamsOf(applyDefinition("old.example.com"), 1)); err != nil {
		t.Fatalf("建立旧域名配置失败: %v", err)
	}
	report, err := f.execute(applyParamsOf(applyDefinition("new.example.com"), 2))
	if err != nil {
		t.Fatalf("改名失败: %v", err)
	}

	if f.fsys.exists(oldPath) {
		t.Error("改名之后旧域名的托管文件仍在：两个域名会同时指向同一个上游")
	}
	if !f.fsys.exists(newPath) {
		t.Error("新域名的托管文件没写出来")
	}
	if !strings.Contains(report.RecoveryHint, "旧配置") {
		t.Errorf("报告里应当提到移除了旧配置：%q", report.RecoveryHint)
	}
	// 旧文件先备份再删。
	found := false
	backups, _ := f.fsys.fs().ReadDir(DefaultBackupDir)
	for _, b := range backups {
		if strings.HasSuffix(b.Name(), "upanel-old.example.com.conf") {
			found = true
		}
	}
	if !found {
		t.Errorf("旧文件没有备份就被删了：%v", backups)
	}
}

// TestApplyLeavesForeignConfigUntouched 只动 upanel-<domain>.conf 与 upanel-auth/。
func TestApplyLeavesForeignConfigUntouched(t *testing.T) {
	f := newApplyFixture(t)
	foreign := path.Join(applyRoot, "conf.d", "default.conf")
	const foreignBody = "server {\n    listen 80 default_server;\n    server_name _;\n}\n"
	f.fsys.seed(foreign, foreignBody)

	if _, err := f.execute(applyParamsOf(applyDefinition(applyDomain), 1)); err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if got := f.fsys.content(foreign); got != foreignBody {
		t.Errorf("发行版默认站点被改了：\n%s", got)
	}
	entries, err := f.fsys.fs().ReadDir(path.Join(applyRoot, "conf.d"))
	if err != nil {
		t.Fatalf("读 conf.d 失败: %v", err)
	}
	if len(entries) != 2 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("conf.d 里的文件 = %v，期望只有 default.conf 与我们的托管文件", names)
	}
	// 不能碰主配置：主配置里出现 user 指令才决定 worker 用户，改写它会牵动整机。
	if f.fsys.exists(path.Join(applyRoot, "nginx.conf")) {
		t.Error("写了主配置 nginx.conf：托管文件必须是独立的 conf.d 文件")
	}
}

// TestApplyBasicAuthWritesHtpasswd 账号模式：文件写到 upanel-auth/，内容是 apr1，明文哪都不在。
func TestApplyBasicAuthWritesHtpasswd(t *testing.T) {
	f := newApplyFixture(t)
	const (
		ref      = "sec_0123456789abcdef0123456789abcdef"
		password = "correct horse battery staple"
	)
	f.secrets.values[ref] = password

	def := applyDefinition(applyDomain)
	def.AccessMode = AccessBasicAuth
	def.BasicAuthUsers = []BasicAuthUser{{Name: "alice", SecretRef: ref}}

	report, err := f.execute(applyParamsOf(def, 1))
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	authPath := applyAuthPath()
	if !f.fsys.exists(authPath) {
		t.Fatalf("账号文件没写出来：%s", authPath)
	}
	content := f.fsys.content(authPath)
	want := "alice:" + APR1(password, deriveSalt(applySiteID, "alice")) + "\n"
	if content != want {
		t.Errorf("账号文件内容 = %q，期望 %q", content, want)
	}
	if strings.Contains(content, password) {
		t.Error("账号文件里有明文密码")
	}
	// 配置里引用的是绝对路径的账号文件。
	if cfg := f.fsys.content(applyConfPath(applyDomain)); !strings.Contains(cfg, "auth_basic_user_file "+authPath+";") {
		t.Errorf("配置没有引用账号文件：\n%s", cfg)
	}
	// 密码不能出现在任何一次命令的参数里，也不能出现在报告里。
	for _, call := range f.runner.calls {
		if strings.Contains(call, password) {
			t.Errorf("命令行里出现了明文密码：%q", call)
		}
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("序列化报告失败: %v", err)
	}
	if strings.Contains(string(encoded), password) {
		t.Error("报告里有明文密码")
	}
	// 找不到 nginx 的 worker 用户时权限退回 0644，并且必须在报告里说出来。
	if mode := f.fsys.modeOf(authPath); mode != 0o644 {
		t.Errorf("账号文件权限 = %o，期望 0644（测试环境没有 nginx.conf，退到这一档）", mode)
	}
	if !strings.Contains(report.RecoveryHint, "0644") {
		t.Errorf("权限放宽到 0644 却没有在报告里说明：%q", report.RecoveryHint)
	}
	if got := report.Observed["auth_file_state"]; got != "written" {
		t.Errorf("auth_file_state = %v，期望 written", got)
	}
}

// TestApplyBasicAuthRemovesAuthFileWhenSwitchingToPublic 改成 public 之后账号文件不该留着。
func TestApplyBasicAuthRemovesAuthFileWhenSwitchingToPublic(t *testing.T) {
	f := newApplyFixture(t)
	const ref = "sec_0123456789abcdef0123456789abcdef"
	f.secrets.values[ref] = "s3cret"

	def := applyDefinition(applyDomain)
	def.AccessMode = AccessBasicAuth
	def.BasicAuthUsers = []BasicAuthUser{{Name: "alice", SecretRef: ref}}
	if _, err := f.execute(applyParamsOf(def, 1)); err != nil {
		t.Fatalf("建立账号模式失败: %v", err)
	}
	if !f.fsys.exists(applyAuthPath()) {
		t.Fatal("账号文件没写出来")
	}

	report, err := f.execute(applyParamsOf(applyDefinition(applyDomain), 2))
	if err != nil {
		t.Fatalf("改成 public 失败: %v", err)
	}
	if f.fsys.exists(applyAuthPath()) {
		t.Error("改成 public 之后账号文件仍然在：一份没人引用但存在的哈希文件没有理由保留")
	}
	if got := report.Observed["auth_file_state"]; got != "removed" {
		t.Errorf("auth_file_state = %v，期望 removed", got)
	}
}

// TestApplyMissingSecretBlocks 密码引用解不开：一条都不能写。
func TestApplyMissingSecretBlocks(t *testing.T) {
	f := newApplyFixture(t)
	def := applyDefinition(applyDomain)
	def.AccessMode = AccessBasicAuth
	def.BasicAuthUsers = []BasicAuthUser{{Name: "alice", SecretRef: "sec_ffffffffffffffffffffffffffffffff"}}

	// 预检（判决）阶段就该拦住。
	assess, err := f.applier.AssessApply(context.Background(), applyParamsOf(def, 1))
	if err != nil {
		t.Fatalf("判决失败: %v", err)
	}
	if assess.Allowed {
		t.Fatal("秘密解不开却放行了")
	}
	if blockerCodes(assess.Blockers) != CodeSecretInvalid {
		t.Errorf("阻塞码 = %s，期望 %s", blockerCodes(assess.Blockers), CodeSecretInvalid)
	}

	// 执行层同样拒绝，并且一个字节都没写。
	_, err = f.execute(applyParamsOf(def, 1))
	requireHTTPError(t, err, 409, httpx.CodeDependencyBlocked, CodeSecretInvalid)
	if f.fsys.exists(applyConfPath(applyDomain)) || f.fsys.exists(applyAuthPath()) {
		t.Error("秘密解不开却写了文件")
	}
	if got := f.runner.count(DefaultNginxPath); got != 0 {
		t.Errorf("拒绝时调用了 %d 次 nginx，期望 0", got)
	}
}

// TestApplyDomainTakenBlocks 域名被别人占着：拒绝，并且不写任何东西。
func TestApplyDomainTakenBlocks(t *testing.T) {
	f := newApplyFixture(t)
	f.fsys.seed(path.Join(applyRoot, "conf.d", "legacy.conf"),
		"server {\n    listen 80;\n    server_name app.example.com;\n}\n")

	assess, err := f.applier.AssessApply(context.Background(), applyParamsOf(applyDefinition(applyDomain), 1))
	if err != nil {
		t.Fatalf("判决失败: %v", err)
	}
	if assess.Allowed || blockerCodes(assess.Blockers) != CodeDomainTaken {
		t.Fatalf("阻塞码 = %s，期望 %s", blockerCodes(assess.Blockers), CodeDomainTaken)
	}

	_, err = f.execute(applyParamsOf(applyDefinition(applyDomain), 1))
	requireHTTPError(t, err, 409, httpx.CodeDependencyBlocked, CodeDomainTaken)
	if f.fsys.exists(applyConfPath(applyDomain)) {
		t.Error("域名冲突却写出了配置")
	}
}

// TestAssessApplyIgnoresOwnManagedFile 自己那份托管文件里的同名 server_name 不算冲突。
func TestAssessApplyIgnoresOwnManagedFile(t *testing.T) {
	f := newApplyFixture(t)
	if _, err := f.execute(applyParamsOf(applyDefinition(applyDomain), 1)); err != nil {
		t.Fatalf("建立配置失败: %v", err)
	}
	// 第二次判决必须仍然是 allowed（否则这个站点永远改不了自己）。
	assess, err := f.applier.AssessApply(context.Background(), applyParamsOf(applyDefinition(applyDomain), 2))
	if err != nil {
		t.Fatalf("判决失败: %v", err)
	}
	if !assess.Allowed {
		t.Fatalf("自己的文件被当成了冲突：%s", blockerCodes(assess.Blockers))
	}
	if len(assess.Locks) != 2 || assess.Locks[0] != LockNginxConfig {
		t.Errorf("锁 = %v，期望包含 %s 与站点锁", assess.Locks, LockNginxConfig)
	}
}

// TestApplyBrokenOwnFileCanBeRewritten 自己那份文件坏了也要能重写（坏文件正是这次要覆盖的东西）。
func TestApplyBrokenOwnFileCanBeRewritten(t *testing.T) {
	f := newApplyFixture(t)
	confPath := applyConfPath(applyDomain)
	f.fsys.seed(confPath, "server { 这行被手工改坏了\n")
	// 采集到的这份文件解析失败，因此出现在 BrokenFiles 里。
	facts, err := f.facts.Collect(context.Background())
	if err != nil {
		t.Fatalf("采集失败: %v", err)
	}
	_ = facts // 解析失败的判定在 internal/nginx 里，这里的假事实源不模拟它

	// 直接让事实源报告一份坏文件：别人的坏文件会让结论变 unknown，自己的不会。
	broken := nginx.Facts{Available: true, UnitState: "active", Files: []nginx.ConfigFile{
		{Path: confPath, Managed: true, Error: "无效配置"},
	}}
	got := JudgeDomainIgnoring(broken, applyDomain, confPath)
	if got.State != ConflictClear {
		t.Errorf("自己那份坏文件把结论变成了 %s（期望 clear）", got.State)
	}
	// 换成别人的坏文件，结论必须是 unknown。
	other := nginx.Facts{Available: true, UnitState: "active", Files: []nginx.ConfigFile{
		{Path: path.Join(applyRoot, "conf.d", "someone.conf"), Error: "无效配置"},
	}}
	if got := JudgeDomainIgnoring(other, applyDomain, confPath); got.State != ConflictUnknown {
		t.Errorf("别人的坏文件没有让结论变成 unknown：%s", got.State)
	}

	report, err := f.execute(applyParamsOf(applyDefinition(applyDomain), 1))
	if err != nil {
		t.Fatalf("坏文件没能被重写: %v", err)
	}
	if got := report.Data["file_state"]; got != FileStateWritten {
		t.Errorf("file_state = %v，期望 %s", got, FileStateWritten)
	}
	if got := f.fsys.content(confPath); strings.Contains(got, "改坏了") {
		t.Error("坏内容还在文件里")
	}
}

// TestApplyUnitInactiveSkipsReload 服务没在跑：写下去、校验、但不 reload，结果只能是 accepted。
func TestApplyUnitInactiveSkipsReload(t *testing.T) {
	f := newApplyFixture(t)
	// 两处都要：判决（预检告警）看事实里的 UnitState，执行看 systemctl is-active 的回答。
	f.facts.unit = "inactive"
	f.runner.unitOut = "inactive"

	report, err := f.execute(applyParamsOf(applyDefinition(applyDomain), 1))
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if got := report.Data["reload"]; got != ReloadSkippedInactive {
		t.Errorf("reload = %v，期望 %s", got, ReloadSkippedInactive)
	}
	if got := report.Data["served"]; got != false {
		t.Errorf("served = %v，期望 false（服务没在跑）", got)
	}
	if report.Outcome != OutcomeAccepted {
		t.Errorf("outcome = %q，期望 accepted", report.Outcome)
	}
	if got := f.runner.count(DefaultNginxPath + " -s reload"); got != 0 {
		t.Errorf("reload 调用 %d 次，期望 0", got)
	}
	if !strings.Contains(report.RecoveryHint, "inactive") {
		t.Errorf("报告里应当说明服务状态：%q", report.RecoveryHint)
	}
}

// TestApplyReloadRejectedKeepsConfig 配置是对的但 reload 没被接受：不回滚，如实说没生效。
func TestApplyReloadRejectedKeepsConfig(t *testing.T) {
	f := newApplyFixture(t)
	f.runner.reloadExit = 1

	report, err := f.execute(applyParamsOf(applyDefinition(applyDomain), 1))
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if got := report.Data["reload"]; got != ReloadFailed {
		t.Errorf("reload = %v，期望 %s", got, ReloadFailed)
	}
	if report.Outcome != OutcomeAccepted {
		t.Errorf("outcome = %q，期望 accepted", report.Outcome)
	}
	// 回滚会把一份正确的配置丢掉——不做这件事。
	if !f.fsys.exists(applyConfPath(applyDomain)) {
		t.Error("reload 没被接受却把正确的配置删了")
	}
	if !strings.Contains(report.RecoveryHint, "reload") {
		t.Errorf("报告里应当说明 reload 没生效：%q", report.RecoveryHint)
	}
}

// TestApplyUnavailableFactsBlocks 读不到 Nginx / 没装 Nginx：一律不写。
func TestApplyUnavailableFactsBlocks(t *testing.T) {
	cases := []struct {
		name string
		set  func(*applyFixture)
		code string
	}{
		{"读不到现状", func(f *applyFixture) { f.facts.err = errors.New("代理不可用") }, CodeNginxUnavailable},
		{"没有 Nginx", func(f *applyFixture) {
			f.facts.override = &nginx.Facts{Available: false, ConfigMissing: true, Reason: "没有配置目录"}
		}, CodeNginxNotInstalled},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newApplyFixture(t)
			tc.set(f)
			assess, err := f.applier.AssessApply(context.Background(), applyParamsOf(applyDefinition(applyDomain), 1))
			if err != nil {
				t.Fatalf("判决失败: %v", err)
			}
			if assess.Allowed || blockerCodes(assess.Blockers) != tc.code {
				t.Fatalf("阻塞码 = %s，期望 %s", blockerCodes(assess.Blockers), tc.code)
			}
			if _, err := f.execute(applyParamsOf(applyDefinition(applyDomain), 1)); err == nil {
				t.Fatal("读不到 Nginx 现状却写了配置")
			}
			if got := f.runner.count(DefaultNginxPath); got != 0 {
				t.Errorf("拒绝时调用了 %d 次 nginx，期望 0", got)
			}
		})
	}
}

// TestApplyRejectsPanelPort 上游端口等于面板端口：在参数层就被拒，永远到不了写文件。
func TestApplyRejectsPanelPort(t *testing.T) {
	f := newApplyFixture(t)
	def := applyDefinition(applyDomain)
	def.UpstreamPort = panelTestPort

	_, err := f.execute(applyParamsOf(def, 1))
	requireHTTPError(t, err, 400, httpx.CodeValidationError, "上游端口")
	if strings.Contains(err.Error(), "上游端口") && !strings.Contains(err.Error(), "面板") {
		t.Errorf("错误说明应当点明是面板端口：%v", err)
	}
	if f.fsys.exists(applyConfPath(applyDomain)) {
		t.Error("面板端口被写下去了")
	}
	// 不传面板端口（测试或其它调用）时这条规则不生效——它只在知道端口时有意义。
	p := applyParamsOf(def, 1)
	p.PanelPort = 0
	if _, err := p.Normalize(); err != nil {
		t.Fatalf("没有面板端口时不该拒绝: %v", err)
	}
}

// TestApplyRejectsVersionAndIdentityDrift 预检绑定的身份/版本对不上：拒绝执行。
func TestApplyRejectsVersionAndIdentityDrift(t *testing.T) {
	f := newApplyFixture(t)
	p := applyParamsOf(applyDefinition(applyDomain), 1)

	_, err := f.applier.ExecuteApply(context.Background(), WriteRequest{
		Action:           ActionProxyApply,
		Params:           p,
		ExpectedIdentity: "site_fedcba9876543210fedcba9876543210",
	})
	requireHTTPError(t, err, 409, httpx.CodeVersionConflict, "站点标识")

	_, err = f.applier.ExecuteApply(context.Background(), WriteRequest{
		Action:               ActionProxyApply,
		Params:               p,
		ExpectedIdentity:     applySiteID,
		ExpectedStateVersion: StateVersionOf(applyParamsOf(applyDefinition(applyDomain), 9)),
	})
	requireHTTPError(t, err, 409, httpx.CodeVersionConflict, "目标状态")
	if f.fsys.exists(applyConfPath(applyDomain)) {
		t.Error("身份/版本对不上却写了配置")
	}
}

// TestApplyReadbackMismatchIsReported 写下去但读回来对不上（例如被别的进程改了）：不能报成功。
func TestApplyReadbackMismatchIsReported(t *testing.T) {
	f := newApplyFixture(t)
	p := applyParamsOf(applyDefinition(applyDomain), 1)
	if _, err := p.Normalize(); err != nil {
		t.Fatalf("参数归一化失败: %v", err)
	}
	// 让事实源在最后那次读回时报告“文件不在”。
	facts := &readbackLiar{inner: f.facts, confPath: applyConfPath(applyDomain)}
	f.applier.opts.Facts = facts

	_, err := f.applier.ExecuteApply(context.Background(), WriteRequest{Action: ActionProxyApply, Params: p})
	requireHTTPError(t, err, 500, httpx.CodeCommandFailed, "读回核对")
	if got := f.runner.count(DefaultNginxPath + " -s reload"); got != 1 {
		t.Errorf("reload 调用 %d 次，期望 1", got)
	}
}

// readbackLiar 在第 2 次采集时把托管文件藏起来，模拟“写下去了但读不回来”。
type readbackLiar struct {
	inner    FactsSource
	confPath string
	calls    int
}

func (r *readbackLiar) Snapshot(ctx context.Context) (nginx.Facts, error) {
	return r.inner.Snapshot(ctx)
}

func (r *readbackLiar) Collect(ctx context.Context) (nginx.Facts, error) {
	r.calls++
	facts, err := r.inner.Collect(ctx)
	if err != nil || r.calls < 2 {
		return facts, err
	}
	kept := facts.Files[:0]
	for _, file := range facts.Files {
		if file.Path != r.confPath {
			kept = append(kept, file)
		}
	}
	facts.Files = kept
	return facts, nil
}

func (r *readbackLiar) Invalidate() {}

// TestReadbackState 读回核对的四种结论。
func TestReadbackState(t *testing.T) {
	confPath := applyConfPath(applyDomain)
	content := "server {\n    server_name app.example.com;\n}\n"
	want := hashOf(content)
	good := nginx.Facts{Available: true, Files: []nginx.ConfigFile{{
		Path: confPath, Managed: true, SHA256: want,
		Servers: []nginx.ServerBlock{{Names: []nginx.ServerName{nginx.Classify(applyDomain)}}},
	}}}

	if got := readbackState(good, confPath, want, true, applyDomain); !got.OK {
		t.Errorf("正常的读回却判失败：%s", got.Reason)
	}
	// 文件在，但内容不是刚写下去的那一份。
	tampered := good
	tampered.Files = []nginx.ConfigFile{{Path: confPath, Managed: true, SHA256: hashOf("别的")}}
	if got := readbackState(tampered, confPath, want, true, applyDomain); got.OK {
		t.Error("内容被改过却判通过")
	}
	// 文件内容对，但 server_name 不是这个域名：写对了却没生效。
	wrongName := nginx.Facts{Available: true, Files: []nginx.ConfigFile{{
		Path: confPath, Managed: true, SHA256: want,
		Servers: []nginx.ServerBlock{{Names: []nginx.ServerName{nginx.Classify("other.example.com")}}},
	}}}
	got := readbackState(wrongName, confPath, want, true, applyDomain)
	if got.OK || !strings.Contains(got.Reason, "server_name") {
		t.Errorf("server_name 不匹配却判通过：%+v", got)
	}
	// 停用之后文件不该在；在就是没生效。
	if got := readbackState(good, confPath, "", false, applyDomain); got.OK {
		t.Error("停用之后文件还在却判通过")
	}
	// 停用之后文件不在，这才是对的。
	if got := readbackState(nginx.Facts{Available: true}, confPath, "", false, applyDomain); !got.OK {
		t.Errorf("停用之后文件不在却判失败：%s", got.Reason)
	}
}

// TestApplyReportCarriesNoPlaintext 报告里不该出现账号明文（兜底：任何路径都不要泄漏）。
func TestApplyReportCarriesNoPlaintext(t *testing.T) {
	f := newApplyFixture(t)
	const ref = "sec_0123456789abcdef0123456789abcdef"
	const password = "Tr0ub4dor&3-very-secret"
	f.secrets.values[ref] = password
	def := applyDefinition(applyDomain)
	def.AccessMode = AccessBasicAuth
	def.BasicAuthUsers = []BasicAuthUser{{Name: "alice", SecretRef: ref}}

	report, err := f.execute(applyParamsOf(def, 1))
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	blob, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	if strings.Contains(string(blob), password) {
		t.Error("报告里出现了明文密码")
	}
	if strings.Contains(f.fsys.content(applyConfPath(applyDomain)), password) ||
		strings.Contains(f.fsys.content(applyAuthPath()), password) {
		t.Error("文件里出现了明文密码")
	}
}

// TestStateVersionOf 状态版本是“站点 + 版本号”，预检与执行之间的比对靠它。
func TestStateVersionOf(t *testing.T) {
	p := applyParamsOf(applyDefinition(applyDomain), 7)
	if got := StateVersionOf(p); got != "site:"+applySiteID+":v7" {
		t.Errorf("StateVersionOf = %q", got)
	}
	if p.DisplayName() != applyDomain {
		t.Errorf("DisplayName = %q", p.DisplayName())
	}
	empty := ApplyParams{SiteID: applySiteID}
	if empty.DisplayName() != applySiteID {
		t.Errorf("没有定义时 DisplayName = %q，期望站点标识", empty.DisplayName())
	}
}
