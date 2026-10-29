package hostcaps

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeHost 是一台假机器：目录里有什么、命令吐什么，全由测试给定。
type fakeHost struct {
	dirs    map[string][]string
	dirErr  map[string]error
	exists  map[string]bool
	runFn   func(ctx context.Context, path string, args ...string) RunResult
	now     time.Time
	mu      sync.Mutex
	asked   []string
	listDir []string
}

func newFakeHost() *fakeHost {
	return &fakeHost{
		dirs:   map[string][]string{},
		dirErr: map[string]error{},
		exists: map[string]bool{},
		now:    time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC),
		runFn: func(_ context.Context, path string, args ...string) RunResult {
			// 默认：程序在、跑得动，但输出里没有任何版本锚点。
			// 这样“默认路径”与“真的读到了”在测试里是被分开的。
			return RunResult{Stdout: "", Stderr: ""}
		},
	}
}

// put 在某个固定目录里放一个程序。
func (f *fakeHost) put(dir, name string) { f.dirs[dir] = append(f.dirs[dir], name) }

// putDir 让某个路径“是一个存在的目录”（systemd 的 /run/systemd/system）。
func (f *fakeHost) putDir(path string) { f.exists[path] = true }

// answer 给某个程序（按"路径 参数..."匹配）指定输出。
func (f *fakeHost) answer(key string, res RunResult) {
	prev := f.runFn
	f.runFn = func(ctx context.Context, path string, args ...string) RunResult {
		full := strings.TrimSpace(path + " " + strings.Join(args, " "))
		if full == key {
			return res
		}
		return prev(ctx, path, args...)
	}
}

func (f *fakeHost) deps() Deps {
	return Deps{
		ListDir: func(dir string) ([]string, error) {
			f.mu.Lock()
			f.listDir = append(f.listDir, dir)
			f.mu.Unlock()
			if err, ok := f.dirErr[dir]; ok {
				return nil, err
			}
			return f.dirs[dir], nil
		},
		StatIsDir: func(path string) (bool, error) {
			if f.exists[path] {
				return true, nil
			}
			if err, ok := f.dirErr[path]; ok {
				return false, err
			}
			return false, errors.New("no such directory")
		},
		Run: func(ctx context.Context, path string, args ...string) RunResult {
			f.mu.Lock()
			f.asked = append(f.asked, strings.TrimSpace(path+" "+strings.Join(args, " ")))
			f.mu.Unlock()
			return f.runFn(ctx, path, args...)
		},
		Now: func() time.Time { return f.now },
	}
}

// itemOf 取出某条结果。
func itemOf(t *testing.T, report Report, id string) Item {
	t.Helper()
	for _, item := range report.Items {
		if item.ID == id {
			return item
		}
	}
	t.Fatalf("报告里没有 %q 这一条：%+v", id, ItemIDs())
	return Item{}
}

func TestProbeReportsAbsentWithReasonAndHint(t *testing.T) {
	host := newFakeHost() // 四个固定目录都是空的
	report := Probe(context.Background(), host.deps())

	docker := itemOf(t, report, "docker")
	if docker.State != StateAbsent {
		t.Fatalf("没有 docker 应当是 absent，实际 %q", docker.State)
	}
	if !strings.Contains(docker.Reason, "/usr/bin") || !strings.Contains(docker.Reason, "docker") {
		t.Fatalf("absent 的理由要点名找过哪些目录、缺哪个程序，实际 %q", docker.Reason)
	}
	if docker.Hint != "apt install docker.io" {
		t.Fatalf("absent 要给出能补上的包，实际 %q", docker.Hint)
	}
	if docker.Version != "" || docker.Path != "" {
		t.Fatalf("没有的程序不该有路径或版本，实际 path=%q version=%q", docker.Path, docker.Version)
	}
	if report.Partial {
		t.Fatalf("目录都读得出来时不该标 partial")
	}
	if report.GeneratedAt != "2026-10-30T12:00:00Z" {
		t.Fatalf("时间必须是 UTC 的 RFC3339，实际 %q", report.GeneratedAt)
	}
}

func TestProbeReadsVersionFromBothStreams(t *testing.T) {
	host := newFakeHost()
	host.put("/usr/bin", "nginx")
	host.put("/usr/sbin", "restic")
	host.put("/usr/bin", "ssh")
	// nginx -v 把版本写到 stderr，这是它二十年来的习惯。
	host.answer("/usr/bin/nginx -v", RunResult{Stderr: "nginx version: nginx/1.18.0 (Ubuntu)\n"})
	host.answer("/usr/sbin/restic version", RunResult{Stdout: "restic 0.12.1 compiled with go1.18.1 on linux/amd64\n"})
	host.answer("/usr/bin/ssh -V", RunResult{Stderr: "OpenSSH_8.9p1 Ubuntu-3ubuntu0.10, OpenSSL 3.0.2 15 Mar 2022\n"})

	report := Probe(context.Background(), host.deps())

	for _, want := range []struct{ id, version string }{
		{"nginx", "1.18.0"},
		{"restic", "0.12.1"},
		{"ssh", "8.9p1"},
	} {
		item := itemOf(t, report, want.id)
		if item.State != StatePresent {
			t.Fatalf("%s 应当在，实际 %q（%s）", want.id, item.State, item.Reason)
		}
		if item.Version != want.version {
			t.Fatalf("%s 的版本应当是 %q，实际 %q", want.id, want.version, item.Version)
		}
		if item.Reason != "" {
			t.Fatalf("干净读到的版本不该再带解释，%s 带了 %q", want.id, item.Reason)
		}
	}
}

func TestProbeUnreadableDirMakesMissingUnknown(t *testing.T) {
	host := newFakeHost()
	host.dirErr["/usr/bin"] = errors.New("permission denied")
	report := Probe(context.Background(), host.deps())

	if !report.Partial {
		t.Fatalf("目录读不出来时必须标 partial——那些 absent 不能当“没有”用")
	}
	item := itemOf(t, report, "docker")
	if item.State != StateUnknown {
		t.Fatalf("有目录读不出来时，找不到的程序只能是 unknown，实际 %q", item.State)
	}
	if !strings.Contains(item.Reason, "说不清") {
		t.Fatalf("unknown 的理由要把话说明白，实际 %q", item.Reason)
	}
	if item.Hint != "" {
		t.Fatalf("说不清的时候不该建议装包（可能本来就装着），实际 %q", item.Hint)
	}
	if len(report.Issues) == 0 || !strings.Contains(strings.Join(report.Issues, "\n"), "/usr/bin") {
		t.Fatalf("Issues 要点名那个目录，实际 %v", report.Issues)
	}
}

func TestProbeTimeoutKeepsProgramPresent(t *testing.T) {
	host := newFakeHost()
	host.put("/usr/bin", "docker")
	host.answer("/usr/bin/docker --version", RunResult{ExitCode: -1, TimedOut: true})

	item := itemOf(t, Probe(context.Background(), host.deps()), "docker")
	if item.State != StatePresent {
		t.Fatalf("超时只是版本没读到，程序仍在，实际 %q", item.State)
	}
	if item.Version != "" {
		t.Fatalf("超时不该有版本，实际 %q", item.Version)
	}
	if !strings.Contains(item.Reason, "超时") {
		t.Fatalf("理由要说清是超时，实际 %q", item.Reason)
	}
}

func TestProbeUnparsableOutputIsHonest(t *testing.T) {
	host := newFakeHost()
	host.put("/usr/bin", "certbot")
	host.answer("/usr/bin/certbot --version", RunResult{Stdout: "这不是版本号\n"})

	item := itemOf(t, Probe(context.Background(), host.deps()), "certbot")
	if item.State != StatePresent || item.Version != "" {
		t.Fatalf("认不出版本就留空，实际 %q / %q", item.State, item.Version)
	}
	if !strings.Contains(item.Reason, "认不出版本") {
		t.Fatalf("要说清是认不出，而不是没探，实际 %q", item.Reason)
	}
}

func TestProbeNonZeroExitKeepsReadingWithCaveat(t *testing.T) {
	host := newFakeHost()
	host.put("/usr/sbin", "restic")
	host.answer("/usr/sbin/restic version", RunResult{
		Stdout: "restic 0.16.4 compiled with go1.21.6 on linux/amd64\n", ExitCode: 2,
	})

	item := itemOf(t, Probe(context.Background(), host.deps()), "restic")
	if item.Version != "0.16.4" {
		t.Fatalf("版本是真读数，退出码非零也该保留，实际 %q", item.Version)
	}
	if !strings.Contains(item.Reason, "退出码 2") {
		t.Fatalf("要把退出码说出来，实际 %q", item.Reason)
	}
}

func TestProbeProgramWithoutVersionFlag(t *testing.T) {
	host := newFakeHost()
	host.put("/usr/sbin", "exportfs")

	item := itemOf(t, Probe(context.Background(), host.deps()), "nfs")
	if item.State != StatePresent || item.Version != "" {
		t.Fatalf("exportfs 没有版本开关：present + 空版本，实际 %q / %q", item.State, item.Version)
	}
	if !strings.Contains(item.Reason, "只判存在") {
		t.Fatalf("要说明是“只判存在”，实际 %q", item.Reason)
	}
	if len(host.asked) != 0 {
		t.Fatalf("没有版本开关的条目不该跑命令，实际跑了 %v", host.asked)
	}
}

func TestProbeRequiresEveryProgramOfASpec(t *testing.T) {
	host := newFakeHost()
	host.put("/usr/sbin", "smbd") // testparm 不在

	item := itemOf(t, Probe(context.Background(), host.deps()), "samba")
	if item.State != StateAbsent {
		t.Fatalf("samba 要 smbd 与 testparm 都在才算在，实际 %q", item.State)
	}
	if !strings.Contains(item.Reason, "testparm") {
		t.Fatalf("缺席理由要点名缺的是 testparm，实际 %q", item.Reason)
	}
}

func TestProbeSystemdJudgedByPid1NotPath(t *testing.T) {
	host := newFakeHost()
	host.put("/usr/bin", "systemctl") // 程序在，但 /run/systemd/system 不在
	item := itemOf(t, Probe(context.Background(), host.deps()), "systemd")
	if item.State != StateAbsent {
		t.Fatalf("没有 /run/systemd/system 就不是 systemd 引导的系统，实际 %q", item.State)
	}
	if !strings.Contains(item.Reason, "/run/systemd/system") {
		t.Fatalf("理由要点名那个目录，实际 %q", item.Reason)
	}

	host2 := newFakeHost()
	host2.put("/usr/bin", "systemctl")
	host2.putDir("/run/systemd/system")
	host2.answer("/run/systemd/system --version", RunResult{Stdout: "systemd 249 (249.11-0ubuntu3.12)\n+PAM +AUDIT\n"})
	item2 := itemOf(t, Probe(context.Background(), host2.deps()), "systemd")
	if item2.State != StatePresent {
		t.Fatalf(".Run/systemd/system 在就是 present，实际 %q", item2.State)
	}
	if item2.Version != "249" {
		t.Fatalf("systemd 版本应当取第一段，实际 %q", item2.Version)
	}
	if item2.Path != "/run/systemd/system" {
		t.Fatalf("系统形态类的 path 是那个目录，实际 %q", item2.Path)
	}
}

func TestProbeListsEachFixedDirOnce(t *testing.T) {
	host := newFakeHost()
	Probe(context.Background(), host.deps())
	if len(host.listDir) != len(programDirs) {
		t.Fatalf("四个固定目录各列一次就够，实际列了 %d 次：%v", len(host.listDir), host.listDir)
	}
	for _, dir := range host.listDir {
		found := false
		for _, want := range programDirs {
			if dir == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("列了不该列的目录 %q", dir)
		}
	}
}

func TestProbeNeverLooksOutsideFixedDirs(t *testing.T) {
	host := newFakeHost()
	host.put("/usr/local/bin", "docker") // 可写目录里的同名程序不算
	item := itemOf(t, Probe(context.Background(), host.deps()), "docker")
	if item.State != StateAbsent {
		t.Fatalf("/usr/local/bin 里的程序不该被当成“系统装了”，实际 %q", item.State)
	}
}

func TestSpecTableIsClosedAndSelfConsistent(t *testing.T) {
	seen := map[string]bool{}
	for _, sp := range specTable {
		if sp.ID == "" {
			t.Fatalf("spec 缺 ID")
		}
		if seen[sp.ID] {
			t.Fatalf("ID 重复：%q", sp.ID)
		}
		seen[sp.ID] = true
		if len(sp.Programs) == 0 {
			t.Fatalf("%s 没有程序", sp.ID)
		}
		if sp.Kind != KindProgram && sp.Kind != KindSystem {
			t.Fatalf("%s 的 Kind 越界：%q", sp.ID, sp.Kind)
		}
		if sp.Kind == KindProgram && sp.Hint == "" {
			t.Fatalf("%s 是程序依赖，缺席时要能告诉人装什么", sp.ID)
		}
		if len(sp.Version) > 0 && sp.Pattern == "" {
			t.Fatalf("%s 有版本命令却没有解析锚点", sp.ID)
		}
		if len(sp.Version) == 0 && sp.Pattern != "" {
			t.Fatalf("%s 有锚点却没有版本命令", sp.ID)
		}
		if len(sp.Version) > 0 {
			if _, err := regexp.Compile(sp.Pattern); err != nil {
				t.Fatalf("%s 的锚点编译不过：%v", sp.ID, err)
			}
		}
	}
	if len(ItemIDs()) != len(specTable) {
		t.Fatalf("ItemIDs 与表长度不一致")
	}
}

func TestReportJSONShape(t *testing.T) {
	host := newFakeHost()
	host.put("/usr/sbin", "restic")
	host.answer("/usr/sbin/restic version", RunResult{Stdout: "restic 0.12.1\n"})
	report := Probe(context.Background(), host.deps())

	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("报告不能序列化：%v", err)
	}
	for _, key := range []string{`"generated_at"`, `"items"`, `"partial"`} {
		if !strings.Contains(string(raw), key) {
			t.Fatalf("JSON 里缺 %s：%s", key, raw)
		}
	}
	// 字段名是契约：面板与 CLI 都按这些名字读。
	for _, key := range []string{`"id"`, `"kind"`, `"state"`, `"version"`, `"path"`, `"reason"`, `"hint"`} {
		if !strings.Contains(string(raw), key) {
			t.Fatalf("条目里缺 %s：%s", key, raw)
		}
	}
	if strings.Contains(string(raw), `"items":null`) {
		t.Fatalf("items 必须是数组，不能是 null")
	}
}

func TestCacheSnapshotBeforeAnyProbeIsNotReady(t *testing.T) {
	host := newFakeHost()
	cache := NewCache(host.deps(), time.Minute)
	report, ready := cache.Snapshot()
	if ready {
		t.Fatalf("没探过就不能说 ready")
	}
	if report.Items != nil {
		t.Fatalf("没探过时不该有条目（否则空数组会被读成“什么都没有”）")
	}
}

func TestCacheRefreshIsReusable(t *testing.T) {
	host := newFakeHost()
	host.put("/usr/sbin", "restic")
	cache := NewCache(host.deps(), time.Minute)

	if _, err := cache.Refresh(context.Background()); err != nil {
		t.Fatalf("探测不该失败：%v", err)
	}
	report, ready := cache.Snapshot()
	if !ready || len(report.Items) != len(specTable) {
		t.Fatalf("探完之后应当有全部条目，实际 ready=%v items=%d", ready, len(report.Items))
	}
	if cache.Runs() != 1 {
		t.Fatalf("只探了一次，实际 %d", cache.Runs())
	}
	if cache.ProbedAt().IsZero() {
		t.Fatalf("探过之后 ProbedAt 不该是零值")
	}
}

func TestCacheRefreshAfterCancelKeepsOldReport(t *testing.T) {
	host := newFakeHost()
	cache := NewCache(host.deps(), time.Minute)
	if _, err := cache.Refresh(context.Background()); err != nil {
		t.Fatalf("首次探测失败：%v", err)
	}
	before, _ := cache.Snapshot()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := cache.Refresh(ctx); err == nil {
		t.Fatalf("ctx 已取消时不该报告成功")
	}
	after, ready := cache.Snapshot()
	if !ready || after.GeneratedAt != before.GeneratedAt {
		t.Fatalf("取消不该动缓存里的旧读数")
	}
	if cache.Runs() != 1 {
		t.Fatalf("取消的那一次不该计入成功次数，实际 %d", cache.Runs())
	}
}

func TestCacheStartProbesInBackground(t *testing.T) {
	host := newFakeHost()
	cache := NewCache(host.deps(), 10*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cache.Start(ctx)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, ready := cache.Snapshot(); ready {
			cancel()
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("后台探测两秒内没有产出读数")
}
