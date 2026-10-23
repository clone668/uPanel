package backup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// 这些测试跑的是**判断链**：谁被拒绝、因为什么被拒绝、核对发生在哪一步。
// 它们不能证明真实 Ubuntu 上的 restic 行为——那要靠虚拟机验收（P12.1 证据行）。

// fakeSecrets 是一份内存秘密表。
type fakeSecrets struct {
	refs map[string]fakeSecret
}

type fakeSecret struct {
	purpose string
	value   string
}

func (f *fakeSecrets) Resolve(_ context.Context, ref string) ([]byte, error) {
	entry, ok := f.refs[ref]
	if !ok {
		return nil, errors.New("没有这条秘密")
	}
	return []byte(entry.value), nil
}

func (f *fakeSecrets) Purpose(_ context.Context, ref string) (string, bool, error) {
	entry, ok := f.refs[ref]
	if !ok {
		return "", false, nil
	}
	return entry.purpose, true, nil
}

// fakeRoots 是一张内存根表。
type fakeRoots struct {
	paths    map[string]string
	readOnly map[string]bool
}

func (f fakeRoots) ResolveRoot(rootID string) (string, bool, bool) {
	path, ok := f.paths[rootID]
	if !ok {
		return "", false, true
	}
	return path, !f.readOnly[rootID], false
}

// fakeRunner 记录 restic 的调用并按最后一段子命令回应。
type fakeRunner struct {
	specs     []ExecSpec
	catConfig string
	catErr    error
	initErr   error
	stats     string
}

func (f *fakeRunner) run(_ context.Context, spec ExecSpec) (ExecResult, error) {
	f.specs = append(f.specs, spec)
	res := ExecResult{Program: spec.Program, Duration: 10 * time.Millisecond}
	last := ""
	if len(spec.Args) > 0 {
		last = spec.Args[len(spec.Args)-1]
	}
	switch last {
	case "version":
		res.Stdout = "restic 0.12.1 compiled with go1.18.1 on linux/amd64"
	case "init":
		if f.initErr != nil {
			res.Stderr = "Fatal: create repository at ... failed: config file already exists"
			return res, &ExitError{Program: spec.Program, ExitCode: 1}
		}
		res.Stdout = "created restic repository deadbeef at ..."
	case "config":
		if f.catErr != nil {
			res.Stderr = f.catErr.Error()
			return res, &ExitError{Program: spec.Program, ExitCode: 1}
		}
		res.Stdout = f.catConfig
	case "stats", "--json":
		res.Stdout = f.stats
	}
	return res, nil
}

// repoConfigJSON 返回一份合法的 config 输出。
func repoConfigJSON(id string) string {
	return `{"version":2,"id":"` + id + `","chunker_polynomial":"3da3358b4dc173"}`
}

const testRepoID = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// newTestOps 造一个本地后端可用的 Ops。
func newTestOps(t *testing.T, rootPath string, run *fakeRunner) (*Ops, *fakeSecrets, Definition) {
	t.Helper()
	paths := DefaultPaths()
	paths.Restic = filepath.Join(rootPath, "restic-fake")
	paths.SSH = filepath.Join(rootPath, "ssh-fake")
	paths.StateDir = filepath.Join(rootPath, "state")
	paths.CacheDir = filepath.Join(rootPath, "cache")
	paths.RunDir = filepath.Join(rootPath, "run")
	for _, p := range []string{paths.Restic, paths.SSH} {
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	secrets := &fakeSecrets{refs: map[string]fakeSecret{
		"sec_0123456789abcdef0123456789abcdef": {purpose: "backup_repository_password", value: "s3cr3t"},
		"sec_fedcba9876543210fedcba9876543210": {purpose: "sftp_private_key", value: "-----BEGIN OPENSSH PRIVATE KEY-----\n"},
		"sec_11111111111111111111111111111111": {purpose: "tls_private_key", value: "-----BEGIN PRIVATE KEY-----\n"},
	}}
	ops, err := NewOps(Options{
		Restic:  NewRestic(paths, run.run),
		Roots:   fakeRoots{paths: map[string]string{"root1": filepath.Join(rootPath, "data")}},
		Secrets: secrets,
		Paths:   paths,
	})
	if err != nil {
		t.Fatal(err)
	}
	def, err := validLocal().Normalize()
	if err != nil {
		t.Fatal(err)
	}
	return ops, secrets, def
}

func TestAssessLocalAllowsFreshDirectory(t *testing.T) {
	requireLinux(t)
	dir := t.TempDir()
	run := &fakeRunner{}
	ops, _, def := newTestOps(t, dir, run)

	a, err := ops.Assess(context.Background(), AssessRequest{RepositoryID: "rep_0123456789abcdef0123456789abcdef", Definition: def, Version: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !a.Allowed {
		t.Fatalf("空目录应该允许初始化，阻塞项：%+v", a.Blockers)
	}
	if len(a.Blockers) != 0 {
		t.Fatalf("不该有阻塞项：%+v", a.Blockers)
	}
	// joinRoot 是 POSIX 拼法（代理只跑 Linux），Windows 上的测试要按 "/" 比。
	if want := filepath.ToSlash(dir) + "/data/backups/nightly"; a.AbsolutePath != want {
		t.Fatalf("绝对路径不对：%q != %q", a.AbsolutePath, filepath.ToSlash(dir)+"/data/backups/nightly")
	}
	if !a.Capacity.Supported || a.Capacity.AvailableBytes <= 0 {
		t.Fatalf("本地容量应该读得到：%+v", a.Capacity)
	}
	if len(a.Locks) != 1 || a.Locks[0] != "backup-repo:rep_0123456789abcdef0123456789abcdef" {
		t.Fatalf("锁名不对：%v", a.Locks)
	}
	// 预检不写盘：连仓库目录都不该被创建。
	if _, err := os.Stat(a.AbsolutePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("预检不能创建目录（CONTRACTS 4.1）")
	}
}

func TestAssessLocalBlocksExistingRepository(t *testing.T) {
	requireLinux(t)
	dir := t.TempDir()
	run := &fakeRunner{}
	ops, _, def := newTestOps(t, dir, run)

	target := filepath.Join(dir, "data", "backups", "nightly")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "config"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	a, err := ops.Assess(context.Background(), AssessRequest{RepositoryID: "rep_0123456789abcdef0123456789abcdef", Definition: def, Version: 1})
	if err != nil {
		t.Fatal(err)
	}
	if a.Allowed {
		t.Fatal("目标已是仓库时必须拒绝（只允许新空仓库）")
	}
	if !hasCode(a.Blockers, CodeRepositoryExists) {
		t.Fatalf("应该报 %s：%+v", CodeRepositoryExists, a.Blockers)
	}
}

func TestAssessLocalBlocksUnknownRoot(t *testing.T) {
	dir := t.TempDir()
	ops, _, def := newTestOps(t, dir, &fakeRunner{})
	def.Location.RootID = "root-not-here"

	a, err := ops.Assess(context.Background(), AssessRequest{RepositoryID: "rep_0123456789abcdef0123456789abcdef", Definition: def, Version: 1})
	if err != nil {
		t.Fatal(err)
	}
	if a.Allowed || !hasCode(a.Blockers, CodeRootUnknown) {
		t.Fatalf("未登记的根必须阻塞（%s）：%+v", CodeRootUnknown, a.Blockers)
	}
}

func TestAssessBlocksPurposeMismatch(t *testing.T) {
	dir := t.TempDir()
	ops, _, def := newTestOps(t, dir, &fakeRunner{})
	// 拿一张 TLS 私钥当仓库口令：形状合法，用途不对。
	def.PasswordSecretRef = "sec_11111111111111111111111111111111"

	a, err := ops.Assess(context.Background(), AssessRequest{RepositoryID: "rep_0123456789abcdef0123456789abcdef", Definition: def, Version: 1})
	if err != nil {
		t.Fatal(err)
	}
	if a.Allowed || !hasCode(a.Blockers, CodeSecretPurpose) {
		t.Fatalf("用途不符必须阻塞（%s）：%+v", CodeSecretPurpose, a.Blockers)
	}
}

func TestAssessBlocksDestroyedSecret(t *testing.T) {
	dir := t.TempDir()
	ops, secrets, def := newTestOps(t, dir, &fakeRunner{})
	delete(secrets.refs, def.PasswordSecretRef)

	a, err := ops.Assess(context.Background(), AssessRequest{RepositoryID: "rep_0123456789abcdef0123456789abcdef", Definition: def, Version: 1})
	if err != nil {
		t.Fatal(err)
	}
	if a.Allowed || !hasCode(a.Blockers, CodeSecretUnresolved) {
		t.Fatalf("取不到口令必须阻塞（%s）：%+v", CodeSecretUnresolved, a.Blockers)
	}
}

func TestAssessBlocksMissingRestic(t *testing.T) {
	dir := t.TempDir()
	ops, _, def := newTestOps(t, dir, &fakeRunner{})
	// 只删测试目录里那个假 restic；不碰系统路径。
	if err := os.Remove(ops.opts.Paths.Restic); err != nil {
		t.Fatal(err)
	}
	a, err := ops.Assess(context.Background(), AssessRequest{RepositoryID: "rep_0123456789abcdef0123456789abcdef", Definition: def, Version: 1})
	if err != nil {
		t.Fatal(err)
	}
	if a.Allowed || !hasCode(a.Blockers, CodeResticMissing) {
		t.Fatalf("没有 restic 必须阻塞（%s）：%+v", CodeResticMissing, a.Blockers)
	}
}

func TestExecuteLocalVerifiesByCatConfig(t *testing.T) {
	dir := t.TempDir()
	run := &fakeRunner{catConfig: repoConfigJSON(testRepoID)}
	ops, _, def := newTestOps(t, dir, run)

	exec, err := ops.Execute(context.Background(), ExecuteRequest{
		RepositoryID: "rep_0123456789abcdef0123456789abcdef",
		Definition:   def,
		OperationID:  "op_1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !exec.Verified {
		t.Fatalf("cat config 读回来了就必须是已验证：verify_error=%q", exec.VerifyError)
	}
	if exec.RepoID != testRepoID {
		t.Fatalf("仓库 id 不对：%q", exec.RepoID)
	}
	if exec.RepoVersion != 2 {
		t.Fatalf("仓库版本应为 2，得到 %d", exec.RepoVersion)
	}
	if _, err := os.Stat(filepath.Join(dir, "data", "backups", "nightly", "config")); err == nil {
		t.Fatal("假 restic 不会真的建 config，这里不该有文件")
	}
	// 口令只能进环境，不能进 argv（argv 会进审计与日志）。
	for _, spec := range run.specs {
		if !sawSubcommand([]ExecSpec{spec}, "init") && !sawSubcommand([]ExecSpec{spec}, "config") {
			continue
		}
		if spec.Password != "s3cr3t" {
			t.Fatalf("口令应通过环境传递，spec.Password=%q", spec.Password)
		}
		for _, arg := range spec.Args {
			if strings.Contains(arg, "s3cr3t") {
				t.Fatalf("口令不能出现在 argv 里：%v", spec.Args)
			}
		}
	}
	// 必须真的跑过 init 与 cat config。
	if !sawSubcommand(run.specs, "init") || !sawSubcommand(run.specs, "config") {
		t.Fatalf("应依次跑 init 与 cat config：%v", subcommands(run.specs))
	}
}

func TestExecuteLocalReportsUnverifiedWhenCatConfigFails(t *testing.T) {
	dir := t.TempDir()
	run := &fakeRunner{
		catConfig: repoConfigJSON(testRepoID),
		catErr:    errors.New("Fatal: unable to open config file: Stat: no such file or directory"),
	}
	ops, _, def := newTestOps(t, dir, run)

	exec, err := ops.Execute(context.Background(), ExecuteRequest{
		RepositoryID: "rep_0123456789abcdef0123456789abcdef",
		Definition:   def,
		OperationID:  "op_1",
	})
	if err != nil {
		t.Fatalf("核对失败不是执行错误（要报 unknown 而不是失败）：%v", err)
	}
	if exec.Verified {
		t.Fatal("读不回 config 就不能说已验证")
	}
	if exec.VerifyError == "" {
		t.Fatal("核对失败必须留下原因")
	}
}

func TestExecuteLocalRefusesExistingRepository(t *testing.T) {
	dir := t.TempDir()
	run := &fakeRunner{initErr: errors.New("exists")}
	ops, _, def := newTestOps(t, dir, run)

	_, err := ops.Execute(context.Background(), ExecuteRequest{
		RepositoryID: "rep_0123456789abcdef0123456789abcdef",
		Definition:   def,
		OperationID:  "op_1",
	})
	if err == nil {
		t.Fatal("restic 拒绝时执行必须报错")
	}
	// restic 的 "config file already exists" 要翻成人话。
	if !strings.Contains(err.Error(), "已经是一个 restic 仓库") {
		t.Fatalf("错误应被翻译：%v", err)
	}
}

func TestStateLocalReportsInitialized(t *testing.T) {
	requireLinux(t)
	dir := t.TempDir()
	run := &fakeRunner{catConfig: repoConfigJSON(testRepoID)}
	ops, _, def := newTestOps(t, dir, run)

	target := filepath.Join(dir, "data", "backups", "nightly")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "config"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	state, err := ops.State(context.Background(), StateRequest{
		RepositoryID: "rep_0123456789abcdef0123456789abcdef",
		Definition:   def,
		Version:      3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if state.Initialized == nil || !*state.Initialized {
		t.Fatal("有 config 就该报已初始化")
	}
	if state.RepoID != testRepoID {
		t.Fatalf("仓库 id 不对：%q", state.RepoID)
	}
	if state.Version != 3 {
		t.Fatalf("版本应原样带回：%d", state.Version)
	}
	// 只读查询不该写任何东西：StateDir 里的 known_hosts 是初始化的副作用。
	if entries, err := os.ReadDir(ops.opts.Paths.StateDir); err == nil && len(entries) > 0 {
		t.Fatalf("只读查询不该写 state 目录：%v", entries)
	}
}

func TestStateLocalReportsNotInitializedWithoutConfig(t *testing.T) {
	dir := t.TempDir()
	ops, _, def := newTestOps(t, dir, &fakeRunner{})

	state, err := ops.State(context.Background(), StateRequest{
		RepositoryID: "rep_0123456789abcdef0123456789abcdef",
		Definition:   def,
		Version:      1,
	})
	if err != nil {
		t.Fatal(err)
	}
	// 读到的事实是 false，不是“读不到”：这两者在界面上必须分得开。
	if state.Initialized == nil {
		t.Fatal("目录读得到，就不该报“不知道”")
	}
	if *state.Initialized {
		t.Fatal("空目录不该报已初始化")
	}
	if state.RepoID != "" {
		t.Fatalf("没有仓库却报出 id：%q", state.RepoID)
	}
	if state.ResticVersion == "" {
		t.Fatal("restic 版本应该读得到（假的也会回答）")
	}
}

// TestStateReportsUnknownInsteadOfNotInitialized：读不到时 `initialized` 必须是 null，
// 不是 false；而且“为什么读不到”必须回到调用方，不能被吞掉。
//
// 这条来自真机：22.04（TCG）在两台虚机同时跑验收时，一次 SSH 握手超时让现状查询
// 报出 `initialized=false`——目标位置其实是一个已经建好的仓库，只是那一次没连上。
// 界面上这会被读成“还没有仓库”，管理员会去点初始化（预检会拦住，所以不会丢数据，
// 但结论是假的）。同一件事以前还更糟：`stateInvocation` 返回的错误被 `return out, nil`
// 直接扔掉，连“受管理根不可用”这种话都到不了调用方。
func TestStateReportsUnknownInsteadOfNotInitialized(t *testing.T) {
	dir := t.TempDir()
	ops, _, def := newTestOps(t, dir, &fakeRunner{})
	def.Location.RootID = "root-ghost" // 这台机器没管这个根

	state, err := ops.State(context.Background(), StateRequest{
		RepositoryID: "rep_0123456789abcdef0123456789abcdef",
		Definition:   def,
		Version:      1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if state.Initialized != nil {
		t.Fatalf("没探测成功就不许给出布尔结论，实际 %v", *state.Initialized)
	}
	if state.ProbeError == "" {
		t.Fatal("探测失败的原因被吞掉了")
	}
	if !strings.Contains(state.ProbeError, "root-ghost") &&
		!strings.Contains(state.ProbeError, CodeRootUnknown) {
		t.Fatalf("原因里应点名出问题的根：%q", state.ProbeError)
	}
}

func TestExecuteRespectsCancel(t *testing.T) {
	dir := t.TempDir()
	ops, _, def := newTestOps(t, dir, &fakeRunner{catConfig: repoConfigJSON(testRepoID)})
	cancel := make(chan struct{})
	close(cancel)

	_, err := ops.Execute(context.Background(), ExecuteRequest{
		RepositoryID: "rep_0123456789abcdef0123456789abcdef",
		Definition:   def,
		OperationID:  "op_1",
		Cancel:       cancel,
	})
	if !errors.Is(err, ErrCanceled) {
		t.Fatalf("已取消的请求必须中止，得到 %v", err)
	}
}

func TestSFTPDefinitionRejectsUnparseableFingerprint(t *testing.T) {
	dir := t.TempDir()
	ops, _, _ := newTestOps(t, dir, &fakeRunner{})
	def := validSFTP()
	def.Location.HostKeyFingerprint = "deadbeef"
	// 指纹在**定义层**就被拒绝：一个解析不出来的指纹连“比较”都做不到，
	// 让它走到连接阶段就等于 TOFU。所以这里期望的是 ErrInvalid，而不是一条阻塞项。
	if _, err := def.Normalize(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("指纹文本不合法必须在定义层被拒绝，得到 %v", err)
	}
	good, err := validSFTP().Normalize()
	if err != nil {
		t.Fatal(err)
	}
	// 有效指纹但连不上：阻塞项是 SFTP_UNREACHABLE（这里用 192.0.2.0/24 的文档地址，
	// 不会真的连到任何东西）。主机密钥**失配**那条路径需要一台真 sshd，见 P12.1 验收。
	good.Location.Host = "192.0.2.1"
	good.Location.Port = 2222
	a, err := ops.Assess(context.Background(), AssessRequest{
		RepositoryID: "rep_0123456789abcdef0123456789abcdef",
		Definition:   good,
		Version:      1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if a.Allowed {
		t.Fatal("连不上远端时必须阻塞")
	}
	if !hasCode(a.Blockers, CodeSFTPUnreachable) && !hasCode(a.Blockers, CodeSFTPAuth) {
		t.Fatalf("连不上应报 %s 或 %s：%+v", CodeSFTPUnreachable, CodeSFTPAuth, a.Blockers)
	}
	if a.HostKey.Fingerprint != "" {
		t.Fatal("没有握手就不该报出实际指纹")
	}
}

// requireLinux 让依赖本地探测的用例在非 Linux 上显式跳过。
//
// internal/backup 的本地后端本来就只有 Linux 实现（local_linux.go）：非 Linux 上
// ProbeLocal 是一个“读不到容量”的桩，joinRoot 也是 POSIX 拼法。在这些用例里让断言
// 失败，报的是“测试跑错地方”，不是“代码有问题”——那会把真正的回归淹掉。
func requireLinux(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("本地后端探测只有 Linux 实现；这条用例在 " + runtime.GOOS + " 上不适用（真实验收在 Ubuntu 虚机上）")
	}
}

// hasCode 判断阻塞/告警里有没有某个码。
func hasCode(reasons []Reason, code string) bool {
	for _, r := range reasons {
		if r.Code == code {
			return true
		}
	}
	return false
}

// subcommands 返回每次调用最后一段参数（诊断用）。
func subcommands(specs []ExecSpec) []string {
	out := make([]string, 0, len(specs))
	for _, spec := range specs {
		if len(spec.Args) == 0 {
			out = append(out, "")
			continue
		}
		out = append(out, spec.Args[len(spec.Args)-1])
	}
	return out
}

func sawSubcommand(specs []ExecSpec, sub string) bool {
	for _, spec := range specs {
		if len(spec.Args) > 0 && spec.Args[len(spec.Args)-1] == sub {
			return true
		}
	}
	return false
}
