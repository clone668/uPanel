package backup

// 备份执行与快照查询的测试（P12.2）。
//
// 这里钉的是**顺序与收尾**，不是 restic 的行为：
//   1. 停机发生在 backup 之前，拉起发生在之后——包括 backup 失败、被取消的路径；
//   2. 快照 id 不是证据，读回来的 stats 才是（Verified 假的通过不了）；
//   3. 面板库副本进了快照的路径列表，且运行结束后从盘上消失；
//   4. 预检能说清"为什么现在不能跑"，且什么都不改。

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	testPolicyID = "pol_0123456789abcdef0123456789abcdef"
	testSnapA    = "1111111111111111111111111111111111111111111111111111111111111111"
	testSnapB    = "2222222222222222222222222222222222222222222222222222222222222222"
)

// runRunner 是一个按子命令回应的假 restic。
//
// 与 ops_test 里的 fakeRunner 不同：那边按最后一个参数认子命令，这里按第一个
// 非选项参数认——因为 P12.2 的子命令后面跟着的东西太多了（标签、排除、路径、id），
// 最后一个参数是 id 还是路径取决于调用点。
type runRunner struct {
	calls       [][]string
	snapshots   string
	backupLines []string
	stats       string
	statsExit   int
	backupExit  int
	backupErr   string
}

func (f *runRunner) run(_ context.Context, spec ExecSpec) (ExecResult, error) {
	f.calls = append(f.calls, spec.Args)
	res := ExecResult{Program: spec.Program, Duration: time.Millisecond}
	switch subcommandOf(spec.Args) {
	case "version":
		res.Stdout = "restic 0.16.4 compiled with go1.21.5 on linux/amd64"
	case "config":
		res.Stdout = repoConfigJSON(testRepoID)
	case "backup":
		for _, line := range f.backupLines {
			if spec.OnStdoutLine != nil {
				spec.OnStdoutLine(line)
			}
		}
		res.Stdout = strings.Join(f.backupLines, "\n")
		if f.backupExit != 0 {
			res.Stderr = f.backupErr
			return res, &ExitError{Program: spec.Program, ExitCode: f.backupExit}
		}
	case "snapshots":
		res.Stdout = f.snapshots
	case "stats":
		if f.statsExit != 0 {
			res.Stderr = "Fatal: unable to load snapshot"
			return res, &ExitError{Program: spec.Program, ExitCode: f.statsExit}
		}
		res.Stdout = f.stats
	}
	return res, nil
}

// subcommandOf 取第一个非选项参数（跳过 -r/-o 的值）。
func subcommandOf(args []string) string {
	skip := false
	for _, a := range args {
		switch {
		case skip:
			skip = false
		case a == "-r" || a == "-o" || a == "--repo":
			skip = true
		case strings.HasPrefix(a, "-"):
			continue
		default:
			return a
		}
	}
	return ""
}

// callsOf 取出某个子命令的调用参数（最后一次）。
func (f *runRunner) callsOf(sub string) [][]string {
	var out [][]string
	for _, c := range f.calls {
		if subcommandOf(c) == sub {
			out = append(out, c)
		}
	}
	return out
}

// hasPath 判断一次调用里有没有某个路径参数。
//
// 按 ToSlash 比：joinRoot 是 POSIX 拼法（代理只跑 Linux），而宿主上的临时目录
// 是 Windows 路径，两者只差分隔符。
func hasPath(args []string, want string) bool {
	want = filepath.ToSlash(want)
	for _, a := range args {
		if filepath.ToSlash(a) == want {
			return true
		}
	}
	return false
}

// hasArg 判断一次调用里有没有某个参数（支持 "a b" 成对出现）。
func hasArg(args []string, want ...string) bool {
	for i := 0; i < len(args); i++ {
		matched := true
		for j, w := range want {
			if i+j >= len(args) || args[i+j] != w {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

// fakeApps 是一份内存应用现状表 + 停/起的记录。
type fakeApps struct {
	runtimes   []AppRuntime
	stopErr    error
	startErr   error
	stopCalls  []AppTarget
	startCalls []AppTarget
	// verifyStop / verifyStart 决定回报的动作是否"读回确认过"。
	verifyStop  bool
	verifyStart bool
}

func (f *fakeApps) Inspect(_ context.Context, targets []AppTarget) ([]AppRuntime, error) {
	out := make([]AppRuntime, 0, len(targets))
	for _, t := range targets {
		found := false
		for _, rt := range f.runtimes {
			if rt.Target.Object == t.Object {
				out = append(out, rt)
				found = true
				break
			}
		}
		if !found {
			out = append(out, AppRuntime{Target: t})
		}
	}
	return out, nil
}

func (f *fakeApps) Stop(_ context.Context, runtimes []AppRuntime) ([]AppAction, error) {
	var out []AppAction
	for _, rt := range runtimes {
		f.stopCalls = append(f.stopCalls, rt.Target)
		out = append(out, AppAction{Target: rt.Target, Operation: "stop", Done: true, Verified: f.verifyStop})
	}
	return out, f.stopErr
}

func (f *fakeApps) Start(_ context.Context, runtimes []AppRuntime) ([]AppAction, error) {
	var out []AppAction
	for _, rt := range runtimes {
		f.startCalls = append(f.startCalls, rt.Target)
		out = append(out, AppAction{Target: rt.Target, Operation: "start", Done: true, Verified: f.verifyStart})
	}
	return out, f.startErr
}

// newRunOps 造一个本地后端可用的 Ops，并把仓库目录准备成"已经是仓库"。
func newRunOps(t *testing.T) (*Ops, *runRunner, string) {
	t.Helper()
	dir := t.TempDir()
	paths := DefaultPaths()
	paths.Restic = filepath.Join(dir, "restic-fake")
	paths.SSH = filepath.Join(dir, "ssh-fake")
	paths.StateDir = filepath.Join(dir, "state")
	paths.CacheDir = filepath.Join(dir, "cache")
	paths.RunDir = filepath.Join(dir, "run")
	for _, p := range []string{paths.Restic, paths.SSH} {
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	runner := &runRunner{}
	secrets := &fakeSecrets{refs: map[string]fakeSecret{
		"sec_0123456789abcdef0123456789abcdef": {purpose: "backup_repository_password", value: "s3cr3t"},
	}}
	ops, err := NewOps(Options{
		Restic:  NewRestic(paths, runner.run),
		Roots:   fakeRoots{paths: map[string]string{"root1": filepath.Join(dir, "data")}},
		Secrets: secrets,
		Paths:   paths,
		Now:     func() time.Time { return time.Date(2026, 10, 24, 3, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	// 仓库目录 + config：预检会认为它已经初始化。
	repoDir := filepath.Join(dir, "data", "backups", "nightly")
	if err := os.MkdirAll(repoDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "config"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	// 来源目录。
	if err := os.MkdirAll(filepath.Join(dir, "data", "srv", "app"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "data", "srv", "app", "index.html"), []byte("hi"), 0o600); err != nil {
		t.Fatal(err)
	}
	return ops, runner, dir
}

// filesOnlyPolicy 是一条最小的 files_only 策略。
func filesOnlyPolicy() Policy {
	return Policy{
		Name:            "站点文件",
		RepositoryID:    "rep_0123456789abcdef0123456789abcdef",
		Sources:         []Source{{RootID: "root1", RelativePath: "srv/app"}},
		ConsistencyMode: ConsistencyFilesOnly,
	}
}

// backupOutput 是 restic backup --json 的正常输出。
func backupOutput(snapshotID string) []string {
	return []string{
		`{"message_type":"status","percent_done":0.5,"files_done":1,"total_files":2,"bytes_done":10,"total_bytes":20}`,
		`{"message_type":"summary","files_new":2,"data_added":1024,"total_files_processed":2,"total_bytes_processed":2048,"total_duration":1.5,"snapshot_id":"` + snapshotID + `"}`,
	}
}

func TestRunFilesOnlyHappyPath(t *testing.T) {
	ops, runner, dir := newRunOps(t)
	runner.backupLines = backupOutput(testSnapA)
	runner.stats = `{"total_size":2048,"total_file_count":3,"snapshots_count":1}`

	var steps []string
	out, err := ops.Run(context.Background(), RunRequest{
		PolicyID: testPolicyID,
		Policy:   filesOnlyPolicy(),
		Repository: Repository{
			ID: "rep_0123456789abcdef0123456789abcdef", Definition: validLocal(), Version: 1,
		},
		OperationID: "op-1",
		Progress:    func(step string, _ map[string]any) { steps = append(steps, step) },
	})
	if err != nil {
		t.Fatalf("备份应当成功: %v", err)
	}
	if out.SnapshotID != testSnapA {
		t.Fatalf("快照 id 不对: %q", out.SnapshotID)
	}
	if !out.Verified {
		t.Fatalf("建完必须读回来核对: %+v", out)
	}
	if !out.AppsRestored {
		t.Fatalf("没停过应用，AppsRestored 应为 true")
	}
	if out.PanelCopy != nil {
		t.Fatalf("没列面板库，不该有副本")
	}

	backups := runner.callsOf("backup")
	if len(backups) != 1 {
		t.Fatalf("应当只跑一次 backup，实际 %d 次", len(backups))
	}
	args := backups[0]
	for _, tag := range []string{"upanel", tagPolicyPrefix + testPolicyID, tagConsistencyPrefix + string(ConsistencyFilesOnly)} {
		if !hasArg(args, "--tag", tag) {
			t.Fatalf("快照标签缺 %q：%v", tag, args)
		}
	}
	for _, ex := range []string{"/proc", "/sys", "/dev"} {
		if !hasArg(args, "--exclude", ex) {
			t.Fatalf("默认排除缺 %q：%v", ex, args)
		}
	}
	if !hasPath(args, filepath.ToSlash(dir)+"/data/srv/app") {
		t.Fatalf("备份路径里没有解析后的来源：%v", args)
	}
	if runner.callsOf("stats") == nil {
		t.Fatal("备份后必须用 stats 读回快照")
	}
	if strings.Join(steps, ",") != "backup,backup-progress,verify,verified" {
		t.Fatalf("进度步骤不对: %v", steps)
	}
}

func TestRunResolvesShortSnapshotID(t *testing.T) {
	// restic 0.12 的 `backup --json` summary 只报 8 位短 id（22.04 自带版本）。
	// 短 id 是前缀、可能撞多条，不能直接当证据：必须回仓库查一次，命中唯一那条才认。
	ops, runner, _ := newRunOps(t)
	runner.backupLines = backupOutput("2fbed800")
	runner.snapshots = `[{"id":"2fbed80011111111111111111111111111111111111111111111111111111111",` +
		`"short_id":"2fbed800","time":"2026-10-24T03:00:00Z","hostname":"h","paths":["/srv/app"],"tags":[]}]`
	runner.stats = `{"total_size":2048,"total_file_count":3,"snapshots_count":1}`

	out, err := ops.Run(context.Background(), RunRequest{
		PolicyID: testPolicyID,
		Policy:   filesOnlyPolicy(),
		Repository: Repository{
			ID: "rep_0123456789abcdef0123456789abcdef", Definition: validLocal(), Version: 1,
		},
		OperationID: "op-short",
	})
	if err != nil {
		t.Fatalf("短 id 应当被解析成完整 id 后成功: %v", err)
	}
	want := "2fbed80011111111111111111111111111111111111111111111111111111111"
	if out.SnapshotID != want {
		t.Fatalf("结果里应当是完整 id: %q", out.SnapshotID)
	}
	if !out.Verified {
		t.Fatalf("读回核对必须完成: %+v", out)
	}
	stats := runner.callsOf("stats")
	if len(stats) != 1 {
		t.Fatalf("应当只跑一次 stats，实际 %d 次", len(stats))
	}
	if !hasArg(stats[0], want) {
		t.Fatalf("stats 必须用完整 id: %v", stats[0])
	}
}

func TestRunFailsWhenShortIDIsAmbiguous(t *testing.T) {
	// 短 id 撞上两条快照：这不是"挑一条"，是证据不足，必须失败而不是猜。
	ops, runner, _ := newRunOps(t)
	runner.backupLines = backupOutput("2fbed800")
	runner.snapshots = `[{"id":"2fbed80011111111111111111111111111111111111111111111111111111111",` +
		`"short_id":"2fbed800","time":"2026-10-24T03:00:00Z","hostname":"h","paths":["/srv/app"],"tags":[]},` +
		`{"id":"2fbed80022222222222222222222222222222222222222222222222222222222",` +
		`"short_id":"2fbed800","time":"2026-10-24T03:01:00Z","hostname":"h","paths":["/srv/app"],"tags":[]}]`

	_, err := ops.Run(context.Background(), RunRequest{
		PolicyID: testPolicyID,
		Policy:   filesOnlyPolicy(),
		Repository: Repository{
			ID: "rep_0123456789abcdef0123456789abcdef", Definition: validLocal(), Version: 1,
		},
		OperationID: "op-ambiguous",
	})
	if err == nil {
		t.Fatal("短 id 匹配到多条快照时必须失败")
	}
	if !strings.Contains(err.Error(), "匹配到") {
		t.Fatalf("错误信息应当说明是匹配歧义: %v", err)
	}
}

func TestRunStopsOnlyRunningAppsAndRestartsThem(t *testing.T) {
	ops, runner, _ := newRunOps(t)
	runner.backupLines = backupOutput(testSnapA)
	runner.stats = `{"total_size":1,"total_file_count":1}`

	pol := filesOnlyPolicy()
	pol.ConsistencyMode = ConsistencyStopManagedApps
	pol.AppIDs = []string{testAppID, testAppID2}
	apps := &fakeApps{
		verifyStop:  true,
		verifyStart: true,
		runtimes: []AppRuntime{
			{Target: AppTarget{AppID: testAppID, Kind: AppKindSystemd, Object: "upanel-app-a.service", Label: "站点 A"},
				Found: true, Running: true},
			{Target: AppTarget{AppID: testAppID2, Kind: AppKindContainer, Object: "abc123", Label: "站点 B"},
				Found: true, Running: false},
		},
	}

	out, err := ops.Run(context.Background(), RunRequest{
		PolicyID: testPolicyID,
		Policy:   pol,
		Repository: Repository{
			ID: "rep_0123456789abcdef0123456789abcdef", Definition: validLocal(), Version: 1,
		},
		OperationID: "op-2",
		Apps: []AppTarget{
			{AppID: testAppID, Kind: AppKindSystemd, Object: "upanel-app-a.service", Label: "站点 A"},
			{AppID: testAppID2, Kind: AppKindContainer, Object: "abc123", Label: "站点 B"},
		},
		Controller: apps,
	})
	if err != nil {
		t.Fatalf("备份应当成功: %v", err)
	}
	if len(apps.stopCalls) != 1 || apps.stopCalls[0].Object != "upanel-app-a.service" {
		t.Fatalf("只该停「在跑」的那个: %+v", apps.stopCalls)
	}
	if len(apps.startCalls) != 1 || apps.startCalls[0].Object != "upanel-app-a.service" {
		t.Fatalf("按原状态拉起: %+v", apps.startCalls)
	}
	if !out.AppsRestored {
		t.Fatal("拉起确认过，AppsRestored 应为 true")
	}
	if len(out.Apps) != 2 {
		t.Fatalf("结果里应当有 stop 与 start 两条：%+v", out.Apps)
	}
	if !hasArg(runner.callsOf("backup")[0], "--tag", tagConsistencyPrefix+string(ConsistencyStopManagedApps)) {
		t.Fatal("口径标签应为 stop_managed_apps")
	}
}

func TestRunRestartsAppsEvenIfBackupFails(t *testing.T) {
	ops, runner, _ := newRunOps(t)
	runner.backupExit = 1
	runner.backupErr = "Fatal: unable to save snapshot: connection reset"

	pol := filesOnlyPolicy()
	pol.ConsistencyMode = ConsistencyStopManagedApps
	pol.AppIDs = []string{testAppID}
	apps := &fakeApps{
		verifyStop:  true,
		verifyStart: true,
		runtimes: []AppRuntime{{Target: AppTarget{AppID: testAppID, Kind: AppKindSystemd,
			Object: "upanel-app-a.service", Label: "站点 A"}, Found: true, Running: true}},
	}

	out, err := ops.Run(context.Background(), RunRequest{
		PolicyID: testPolicyID,
		Policy:   pol,
		Repository: Repository{
			ID: "rep_0123456789abcdef0123456789abcdef", Definition: validLocal(), Version: 1,
		},
		Apps:       []AppTarget{{AppID: testAppID, Kind: AppKindSystemd, Object: "upanel-app-a.service", Label: "站点 A"}},
		Controller: apps,
	})
	if err == nil {
		t.Fatal("restic 失败必须报错")
	}
	if out.SnapshotID != "" {
		t.Fatalf("没建成就不该有快照 id: %q", out.SnapshotID)
	}
	if len(apps.startCalls) != 1 {
		t.Fatalf("备份失败也必须拉起停过的应用: %+v", apps.startCalls)
	}
	if !out.AppsRestored {
		t.Fatal("拉起确认过，AppsRestored 应为 true")
	}
}

func TestRunRefusesMissingSource(t *testing.T) {
	ops, runner, _ := newRunOps(t)
	pol := filesOnlyPolicy()
	pol.Sources = []Source{{RootID: "root1", RelativePath: "srv/gone"}}

	_, err := ops.Run(context.Background(), RunRequest{
		PolicyID: testPolicyID,
		Policy:   pol,
		Repository: Repository{
			ID: "rep_0123456789abcdef0123456789abcdef", Definition: validLocal(), Version: 1,
		},
	})
	if err == nil {
		t.Fatal("来源不存在必须拒绝")
	}
	if !strings.Contains(err.Error(), CodeSourceMissing) {
		t.Fatalf("错误里要写清是哪一类问题: %v", err)
	}
	var blocked *sourceBlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("应当是来源阻塞错误: %T", err)
	}
	if len(runner.callsOf("backup")) != 0 {
		t.Fatal("来源不可用就不能调 restic backup")
	}
}

func TestRunRefusesProtectedApp(t *testing.T) {
	ops, runner, _ := newRunOps(t)
	pol := filesOnlyPolicy()
	pol.ConsistencyMode = ConsistencyStopManagedApps
	pol.AppIDs = []string{testAppID}
	apps := &fakeApps{runtimes: []AppRuntime{{
		Target: AppTarget{AppID: testAppID, Kind: AppKindSystemd, Object: "ssh.service", Label: "sshd"},
		Found:  true, Running: true,
		Blockers: []Reason{{Code: "APP_PROTECTED", Message: "ssh.service 在保护名单里，不能停"}},
	}}}

	_, err := ops.Run(context.Background(), RunRequest{
		PolicyID: testPolicyID,
		Policy:   pol,
		Repository: Repository{
			ID: "rep_0123456789abcdef0123456789abcdef", Definition: validLocal(), Version: 1,
		},
		Apps:       []AppTarget{{AppID: testAppID, Kind: AppKindSystemd, Object: "ssh.service", Label: "sshd"}},
		Controller: apps,
	})
	if err == nil {
		t.Fatal("有阻塞项的应用不能被停")
	}
	if len(apps.stopCalls) != 0 {
		t.Fatal("一个都不该停")
	}
	if len(runner.callsOf("backup")) != 0 {
		t.Fatal("预检没过就不能调 restic backup")
	}
}

func TestRunIncludesPanelDatabaseCopyAndRemovesIt(t *testing.T) {
	ops, runner, dir := newRunOps(t)
	runner.backupLines = backupOutput(testSnapA)
	runner.stats = `{"total_size":4096,"total_file_count":2}`

	src := filepath.Join(dir, "state", "upanel.db")
	if err := os.MkdirAll(filepath.Dir(src), 0o700); err != nil {
		t.Fatal(err)
	}
	db := openWALDB(t, src)
	if _, err := db.Exec(`INSERT INTO settings VALUES ('second', 'v2')`); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	pol := filesOnlyPolicy()
	pol.Sources = append(pol.Sources, Source{Kind: SourceKindPanelDatabase})

	copyDir := filepath.Join(dir, "state", "panel-copies")
	out, err := ops.Run(context.Background(), RunRequest{
		PolicyID: testPolicyID,
		Policy:   pol,
		Repository: Repository{
			ID: "rep_0123456789abcdef0123456789abcdef", Definition: validLocal(), Version: 1,
		},
		PanelDatabasePath: src,
		PanelCopyDir:      copyDir,
	})
	if err != nil {
		t.Fatalf("备份应当成功: %v", err)
	}
	if out.PanelCopy == nil {
		t.Fatal("列了面板库就必须有副本记录")
	}
	if out.PanelCopy.Integrity != "ok" {
		t.Fatalf("副本自洽性没通过: %+v", *out.PanelCopy)
	}
	if out.PanelCopy.SourceSchemaVersion != 7 {
		t.Fatalf("副本要带 schema 版本: %d", out.PanelCopy.SourceSchemaVersion)
	}
	copyPath := out.PanelCopy.CopyPath
	if !hasPath(runner.callsOf("backup")[0], copyPath) {
		t.Fatalf("副本必须进快照路径: %v", runner.callsOf("backup")[0])
	}
	if _, err := os.Stat(filepath.Dir(copyPath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("运行结束后副本要删掉（里面有会话与口令哈希）: %v", err)
	}
}

func TestRunPanelDatabaseMissingBlocksBeforeRestic(t *testing.T) {
	ops, runner, dir := newRunOps(t)
	pol := filesOnlyPolicy()
	pol.Sources = append(pol.Sources, Source{Kind: SourceKindPanelDatabase})

	_, err := ops.Run(context.Background(), RunRequest{
		PolicyID: testPolicyID,
		Policy:   pol,
		Repository: Repository{
			ID: "rep_0123456789abcdef0123456789abcdef", Definition: validLocal(), Version: 1,
		},
		PanelDatabasePath: filepath.Join(dir, "state", "nope.db"),
		PanelCopyDir:      filepath.Join(dir, "state", "panel-copies"),
	})
	if err == nil {
		t.Fatal("面板库不存在必须拒绝")
	}
	if !strings.Contains(err.Error(), CodePanelDatabaseMissing) {
		t.Fatalf("错误码不对: %v", err)
	}
	if len(runner.callsOf("backup")) != 0 {
		t.Fatal("面板库取不到副本就不能开始备份")
	}
}

func TestRunWithoutConfirmingStopFails(t *testing.T) {
	ops, runner, _ := newRunOps(t)
	pol := filesOnlyPolicy()
	pol.ConsistencyMode = ConsistencyStopManagedApps
	pol.AppIDs = []string{testAppID}
	apps := &fakeApps{
		verifyStop: false, // 停完读不回"已停"的状态
		runtimes: []AppRuntime{{Target: AppTarget{AppID: testAppID, Kind: AppKindSystemd,
			Object: "upanel-app-a.service", Label: "站点 A"}, Found: true, Running: true}},
	}

	_, err := ops.Run(context.Background(), RunRequest{
		PolicyID: testPolicyID,
		Policy:   pol,
		Repository: Repository{
			ID: "rep_0123456789abcdef0123456789abcdef", Definition: validLocal(), Version: 1,
		},
		Apps:       []AppTarget{{AppID: testAppID, Kind: AppKindSystemd, Object: "upanel-app-a.service", Label: "站点 A"}},
		Controller: apps,
	})
	if err == nil {
		t.Fatal("没确认停下来的应用不能当停好了")
	}
	if len(runner.callsOf("backup")) != 0 {
		t.Fatal("应用没停干净就备份，会拿到半写状态的字节")
	}
}

func TestListSnapshotsParsesTagsAndSorts(t *testing.T) {
	ops, runner, _ := newRunOps(t)
	runner.snapshots = `[
	  {"id":"` + testSnapA + `","short_id":"11111111","time":"2026-10-20T03:00:00.123456789Z","hostname":"h","paths":["/srv/app"],
	   "tags":["upanel","upanel:policy:` + testPolicyID + `","upanel:consistency:files_only"]},
	  {"id":"` + testSnapB + `","short_id":"22222222","time":"2026-10-22T03:00:00Z","hostname":"h","paths":["/srv/app"],
	   "tags":["upanel","upanel:policy:` + testPolicyID + `","upanel:consistency:stop_managed_apps"]}
	]`

	snaps, err := ops.ListSnapshots(context.Background(), SnapshotRequest{
		RepositoryID: "rep_0123456789abcdef0123456789abcdef",
		Definition:   validLocal(),
		PolicyID:     testPolicyID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 2 {
		t.Fatalf("应当两条: %+v", snaps)
	}
	if snaps[0].ID != testSnapB || snaps[1].ID != testSnapA {
		t.Fatalf("必须按时间倒序: %v", []string{snaps[0].ID, snaps[1].ID})
	}
	if snaps[0].PolicyID != testPolicyID || snaps[0].ConsistencyMode != string(ConsistencyStopManagedApps) {
		t.Fatalf("标签没解出来: %+v", snaps[0])
	}
	for _, s := range snaps {
		if s.RestoreVerifiedAt != nil {
			t.Fatal("P12.3 之前恢复验证恒为 null，不能填一个看起来验证过的值")
		}
	}
	// 列表只带策略标签过滤，不带 id。
	args := runner.callsOf("snapshots")[0]
	if !hasArg(args, "--tag", tagPolicyPrefix+testPolicyID) {
		t.Fatalf("策略过滤没传给 restic: %v", args)
	}
}

func TestGetSnapshotAddsStats(t *testing.T) {
	ops, runner, _ := newRunOps(t)
	runner.snapshots = `[{"id":"` + testSnapA + `","short_id":"11111111","time":"2026-10-20T03:00:00Z","paths":["/srv/app"],"tags":["upanel"]}]`
	runner.stats = `{"total_size":12345,"total_file_count":67}`

	snap, err := ops.GetSnapshot(context.Background(), SnapshotRequest{
		RepositoryID: "rep_0123456789abcdef0123456789abcdef",
		Definition:   validLocal(),
		SnapshotID:   testSnapA,
	})
	if err != nil {
		t.Fatal(err)
	}
	if snap.SizeBytes != 12345 || snap.FileCount != 67 {
		t.Fatalf("大小只来自 stats: %+v", snap)
	}
	if snap.Time.Location() != time.UTC {
		t.Fatalf("时间要归一成 UTC: %v", snap.Time)
	}
}

func TestGetSnapshotUnknownID(t *testing.T) {
	ops, runner, _ := newRunOps(t)
	runner.snapshots = `[]`

	_, err := ops.GetSnapshot(context.Background(), SnapshotRequest{
		RepositoryID: "rep_0123456789abcdef0123456789abcdef",
		Definition:   validLocal(),
		SnapshotID:   testSnapA,
	})
	if !errors.Is(err, ErrSnapshotNotFound) {
		t.Fatalf("不在仓库里要明确报找不到: %v", err)
	}
}

func TestGetSnapshotRejectsBadID(t *testing.T) {
	ops, _, _ := newRunOps(t)
	if _, err := ops.GetSnapshot(context.Background(), SnapshotRequest{
		Definition: validLocal(),
		SnapshotID: "../../etc/passwd",
	}); err == nil {
		t.Fatal("快照 id 形状不合法必须拒绝")
	}
}

func TestAssessRunBlocksUninitializedRepository(t *testing.T) {
	ops, runner, dir := newRunOps(t)
	// 把 config 拿掉：目录还在，但已经不是仓库。
	repoDir := filepath.Join(dir, "data", "backups", "nightly")
	if err := os.Remove(filepath.Join(repoDir, "config")); err != nil {
		t.Fatal(err)
	}

	a, err := ops.AssessRun(context.Background(), RunAssessRequest{
		PolicyID: testPolicyID,
		Policy:   filesOnlyPolicy(),
		Repository: Repository{
			ID: "rep_0123456789abcdef0123456789abcdef", Definition: validLocal(), Version: 1,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if a.Allowed {
		t.Fatal("仓库没初始化就不能备份")
	}
	if !hasCode(a.Blockers, CodeRepositoryNotReady) {
		t.Fatalf("应当报 %s：%+v", CodeRepositoryNotReady, a.Blockers)
	}
	if len(a.Locks) != 2 || a.Locks[1] != PolicyLockName(testPolicyID) {
		t.Fatalf("锁要仓库一个、策略一个: %v", a.Locks)
	}
	if len(runner.callsOf("init")) != 0 {
		t.Fatal("预检不能顺手初始化")
	}
}

func TestAssessRunWarnsFilesOnlyAndRetention(t *testing.T) {
	ops, _, _ := newRunOps(t)
	pol := filesOnlyPolicy()
	pol.Retention = Retention{KeepLast: 7, KeepDaily: 14}

	a, err := ops.AssessRun(context.Background(), RunAssessRequest{
		PolicyID: testPolicyID,
		Policy:   pol,
		Repository: Repository{
			ID: "rep_0123456789abcdef0123456789abcdef", Definition: validLocal(), Version: 1,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !a.Allowed {
		t.Fatalf("应当允许: %+v", a.Blockers)
	}
	if !hasCode(a.Warnings, CodeConsistencyFilesOnly) {
		t.Fatalf("files_only 必须提醒一致性边界: %+v", a.Warnings)
	}
	if !hasCode(a.Warnings, CodeRetentionNotEnforced) {
		t.Fatalf("保留策略没执行必须说出来: %+v", a.Warnings)
	}
	if len(a.Impacts) == 0 {
		t.Fatal("至少要说明会写入一个快照")
	}
}

func TestAssessRunBlocksStopWithoutController(t *testing.T) {
	ops, _, _ := newRunOps(t)
	pol := filesOnlyPolicy()
	pol.ConsistencyMode = ConsistencyStopManagedApps
	pol.AppIDs = []string{testAppID}

	a, err := ops.AssessRun(context.Background(), RunAssessRequest{
		PolicyID: testPolicyID,
		Policy:   pol,
		Repository: Repository{
			ID: "rep_0123456789abcdef0123456789abcdef", Definition: validLocal(), Version: 1,
		},
		Apps: []AppTarget{{AppID: testAppID, Kind: AppKindSystemd, Object: "x.service"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if a.Allowed || !hasCode(a.Blockers, CodePolicyAppsUnavailable) {
		t.Fatalf("没有应用控制能力就必须阻塞: %+v", a.Blockers)
	}
}

// partialApps 是一个"停一半"的控制器：第一个停成功，第二个没确认。
type partialApps struct {
	stopCalls  []AppTarget
	startCalls []AppTarget
}

func (p *partialApps) Inspect(_ context.Context, targets []AppTarget) ([]AppRuntime, error) {
	out := make([]AppRuntime, 0, len(targets))
	for _, t := range targets {
		out = append(out, AppRuntime{Target: t, Found: true, Running: true})
	}
	return out, nil
}

func (p *partialApps) Stop(_ context.Context, runtimes []AppRuntime) ([]AppAction, error) {
	var out []AppAction
	for i, rt := range runtimes {
		p.stopCalls = append(p.stopCalls, rt.Target)
		out = append(out, AppAction{Target: rt.Target, Operation: "stop", Done: true, Verified: i == 0})
	}
	return out, errors.New("stop 应用失败（1/2）：站点 B：命令返回成功，但没有读回状态确认")
}

func (p *partialApps) Start(_ context.Context, runtimes []AppRuntime) ([]AppAction, error) {
	var out []AppAction
	for _, rt := range runtimes {
		p.startCalls = append(p.startCalls, rt.Target)
		out = append(out, AppAction{Target: rt.Target, Operation: "start", Done: true, Verified: true})
	}
	return out, nil
}

func TestRunRestartsPartiallyStoppedApps(t *testing.T) {
	ops, runner, _ := newRunOps(t)
	pol := filesOnlyPolicy()
	pol.ConsistencyMode = ConsistencyStopManagedApps
	pol.AppIDs = []string{testAppID, testAppID2}
	apps := &partialApps{}
	targets := []AppTarget{
		{AppID: testAppID, Kind: AppKindSystemd, Object: "upanel-app-a.service", Label: "站点 A"},
		{AppID: testAppID2, Kind: AppKindSystemd, Object: "upanel-app-b.service", Label: "站点 B"},
	}

	_, err := ops.Run(context.Background(), RunRequest{
		PolicyID: testPolicyID,
		Policy:   pol,
		Repository: Repository{
			ID: "rep_0123456789abcdef0123456789abcdef", Definition: validLocal(), Version: 1,
		},
		Apps:       targets,
		Controller: apps,
	})
	if err == nil {
		t.Fatal("停一半必须整体失败")
	}
	if len(runner.callsOf("backup")) != 0 {
		t.Fatal("机器处于半停状态时不能开始备份")
	}
	// 两条都要拉回来：第二个虽然没确认，但 stop 命令返回了成功——它很可能真的停了，
	// 而它在我们动手之前是跑着的。只有"stop 命令本身失败"的那种才不碰。
	if len(apps.startCalls) != 2 {
		t.Fatalf("发过 stop 的对象都要被拉回来: %+v", apps.startCalls)
	}
	if apps.startCalls[0].Object != "upanel-app-a.service" || apps.startCalls[1].Object != "upanel-app-b.service" {
		t.Fatalf("拉起顺序应与停机顺序一致: %+v", apps.startCalls)
	}
}
