package apphealth

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"upanel/internal/apps"
	"upanel/internal/discovery"
	"upanel/internal/store"
	"upanel/migrations"
)

// fakeProber 记录被探测的目标，并按脚本返回结果。
type fakeProber struct {
	calls []discovery.ProbeParams
	// reply 按调用序号返回结果；用完之后一直返回最后一个。
	reply []probeReply
	// now 是“代理的时钟”。日历型用例把调度器的时钟给它，结论时间才会跟调度器一致。
	now func() time.Time
}

func (f *fakeProber) checkedAt() time.Time {
	if f.now != nil {
		return f.now().UTC().Truncate(time.Second)
	}
	return time.Now().UTC().Truncate(time.Second)
}

type probeReply struct {
	result discovery.ProbeResult
	err    error
}

func (f *fakeProber) Probe(_ context.Context, params discovery.ProbeParams) (discovery.ProbeResult, error) {
	idx := len(f.calls)
	f.calls = append(f.calls, params)
	if len(f.reply) == 0 {
		return discovery.ProbeResult{TargetID: params.TargetID, HTTPState: discovery.HTTPOK,
			Scheme: params.Scheme, Port: params.Port, Path: params.Path,
			Address: "127.0.0.1", URL: "http://127.0.0.1:" + strconv.Itoa(params.Port) + params.Path,
			CheckedAt: f.checkedAt()}, nil
	}
	if idx >= len(f.reply) {
		idx = len(f.reply) - 1
	}
	r := f.reply[idx]
	// 真实的代理总会带上它实际用的 scheme/port/path，这里照做。
	if r.result.TargetID == "" {
		r.result.TargetID = params.TargetID
	}
	if r.result.Scheme == "" {
		r.result.Scheme = params.Scheme
	}
	if r.result.Port == 0 {
		r.result.Port = params.Port
	}
	if r.result.Path == "" {
		r.result.Path = params.Path
	}
	if r.result.CheckedAt.IsZero() {
		r.result.CheckedAt = f.checkedAt()
	}
	return r.result, r.err
}

// newTestService 造一个跑在临时数据库里的应用服务。
func newTestService(t *testing.T) *apps.Service {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(context.Background(), store.Options{
		Path:   filepath.Join(dir, "upanel.db"),
		Logger: testLogger(),
	})
	if err != nil {
		t.Fatalf("打开测试数据库失败: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("关闭测试数据库失败: %v", err)
		}
	})
	if _, err := db.Migrate(context.Background(), migrations.FS); err != nil {
		t.Fatalf("迁移测试数据库失败: %v", err)
	}
	svc, err := apps.New(db, apps.Options{Targets: allowTargets{}})
	if err != nil {
		t.Fatalf("构造应用服务失败: %v", err)
	}
	return svc
}

// allowTargets 假装代理发现了任何形状合法的目标（调度器用例不关心写入期校验）。
type allowTargets struct{}

func (allowTargets) TargetExists(context.Context, string) (bool, error) { return true, nil }

func createApp(t *testing.T, svc *apps.Service, name, unit string, health apps.HealthCheck) apps.App {
	t.Helper()
	app, err := svc.Create(context.Background(), apps.Definition{
		Name:    name,
		Type:    apps.TypeSystemd,
		Binding: apps.Binding{Unit: unit},
		Health:  health,
	})
	if err != nil {
		t.Fatalf("创建应用 %s 失败: %v", name, err)
	}
	return app
}

func healthOn(target string, port int) apps.HealthCheck {
	return apps.HealthCheck{
		Enabled:         true,
		TargetID:        target,
		Scheme:          "http",
		Port:            port,
		Path:            "/",
		IntervalSeconds: 30,
		TimeoutSeconds:  3,
	}
}

// TestStepProbesDueAppsOnly 确认：从没探过的探一次；刚探过的不重复探。
func TestStepProbesDueAppsOnly(t *testing.T) {
	svc := newTestService(t)
	app := createApp(t, svc, "站点", "site.service", healthOn("local_site", 8080))
	prober := &fakeProber{}

	scheduler, err := New(svc, prober, testLogger())
	if err != nil {
		t.Fatalf("构造调度器失败: %v", err)
	}
	now := time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)
	scheduler.now = func() time.Time { return now }
	prober.now = scheduler.now

	probed, err := scheduler.Step(context.Background())
	if err != nil {
		t.Fatalf("第一轮失败: %v", err)
	}
	if probed != 1 || len(prober.calls) != 1 {
		t.Fatalf("第一轮探测 %d 个（调用 %d 次），期望 1", probed, len(prober.calls))
	}
	if call := prober.calls[0]; call.TargetID != "local_site" || call.Port != 8080 || call.TimeoutSeconds != 3 {
		t.Fatalf("探测参数 = %+v", call)
	}

	got, ok, err := svc.HealthResult(context.Background(), app.ID)
	if err != nil || !ok {
		t.Fatalf("读取探测结果失败: ok=%v err=%v", ok, err)
	}
	if got.HTTPState != apps.HTTPOK {
		t.Fatalf("探测结论 = %+v", got)
	}

	// 同一个时间点再来一轮：间隔是 30 秒，不该重复探测。
	if probed, _ := scheduler.Step(context.Background()); probed != 0 {
		t.Fatalf("第二轮又探了 %d 个，期望 0（还没到点）", probed)
	}
	// 走到 31 秒后：到点。
	now = now.Add(31 * time.Second)
	scheduler.now = func() time.Time { return now }
	if probed, _ := scheduler.Step(context.Background()); probed != 1 {
		t.Fatalf("第三轮探了 %d 个，期望 1（已过间隔）", probed)
	}
}

// TestStepRecordsErrorState 确认 500 落库为 error 并带上状态码与原因。
func TestStepRecordsErrorState(t *testing.T) {
	svc := newTestService(t)
	app := createApp(t, svc, "坏站点", "broken.service", healthOn("local_broken", 8080))
	code := 500
	prober := &fakeProber{reply: []probeReply{{result: discovery.ProbeResult{
		HTTPState: discovery.HTTPError, StatusCode: &code, DurationMS: 12,
		Address: "127.0.0.1", URL: "http://127.0.0.1:8080/", Scheme: "http", Port: 8080, Path: "/",
		Error: "状态码 500 不在期望集合里（默认 200—399）",
	}}}}

	scheduler, _ := New(svc, prober, testLogger())
	prober.now = scheduler.now
	if _, err := scheduler.Step(context.Background()); err != nil {
		t.Fatalf("探测失败: %v", err)
	}
	got, ok, _ := svc.HealthResult(context.Background(), app.ID)
	if !ok || got.HTTPState != apps.HTTPError || got.StatusCode == nil || *got.StatusCode != 500 {
		t.Fatalf("探测结论 = %+v", got)
	}
	if got.Error == "" || got.DurationMS != 12 {
		t.Fatalf("探测结论缺原因或耗时: %+v", got)
	}

	// 摘要里读到的是 error，而不是“不知道”。
	loaded, err := svc.Get(context.Background(), 0, app.ID)
	if err != nil {
		t.Fatalf("读取应用失败: %v", err)
	}
	if apps.ObservedHTTPState(loaded.Health, loaded.HealthResult) != apps.HTTPError {
		t.Fatalf("摘要 HTTP 状态 = %q，期望 error",
			apps.ObservedHTTPState(loaded.Health, loaded.HealthResult))
	}
}

// TestStepStopsWhenAgentUnavailable 确认代理不可用时写 unknown（而不是 error），
// 并且本轮剩下的目标不再逐个尝试。
func TestStepStopsWhenAgentUnavailable(t *testing.T) {
	svc := newTestService(t)
	// 名字带 ASCII 后缀，让调度顺序（按 name,id）确定下来：先“站点 A”，再“站点 B”。
	first := createApp(t, svc, "站点 A", "a.service", healthOn("local_a", 8080))
	createApp(t, svc, "站点 B", "b.service", healthOn("local_b", 8081))
	prober := &fakeProber{reply: []probeReply{{
		err: errors.Join(discovery.ErrAgentUnavailable, errors.New("socket 不存在")),
	}}}

	scheduler, _ := New(svc, prober, testLogger())
	scheduler.now = func() time.Time { return time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC) }
	prober.now = scheduler.now
	probed, err := scheduler.Step(context.Background())
	if err != nil {
		t.Fatalf("探测失败: %v", err)
	}
	if probed != 1 || len(prober.calls) != 1 {
		t.Fatalf("代理不可用时应当只尝试一次，实际 %d 次", len(prober.calls))
	}
	got, ok, _ := svc.HealthResult(context.Background(), first.ID)
	if !ok {
		t.Fatal("代理不可用也必须留下一条结论")
	}
	if got.HTTPState != apps.HTTPUnknown {
		t.Fatalf("代理不可用应当写 unknown，实际 %q", got.HTTPState)
	}
	if got.Address != "" || got.URL != "" {
		t.Fatalf("没有真的发出请求时不该填地址: %+v", got)
	}
	if got.Error == "" {
		t.Fatal("unknown 必须带原因")
	}
}

// TestDueRespectsInterval 覆盖到点判断的边界。
func TestDueRespectsInterval(t *testing.T) {
	now := time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)
	app := apps.App{Definition: apps.Definition{Health: healthOn("local_x", 80)}}
	if !due(apps.HealthTarget{App: app}, now) {
		t.Fatal("从没探过时必须到点")
	}
	last := apps.HealthResult{CheckedAt: now.Add(-29 * time.Second)}
	if due(apps.HealthTarget{App: app, Last: &last}, now) {
		t.Fatal("29 秒时不该到点")
	}
	last.CheckedAt = now.Add(-30 * time.Second)
	if !due(apps.HealthTarget{App: app, Last: &last}, now) {
		t.Fatal("30 秒时应当到点")
	}
	// 库里的时间在未来（时钟被调过）：按到点处理，否则会一直等到那个时间。
	last.CheckedAt = now.Add(time.Hour)
	if !due(apps.HealthTarget{App: app, Last: &last}, now) {
		t.Fatal("库里的时间在未来时应当按到点处理")
	}
}

// testLogger 是丢弃式日志：用例不关心日志内容，但也不该把噪音写进测试输出。
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}
