package metrics

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"upanel/internal/monitoring"
	"upanel/internal/storage"
	"upanel/internal/store"
	"upanel/migrations"
)

// 本文件的用例全部跑在**真实 SQLite** 上（临时库 + 真实迁移），只把时钟与快照来源
// 换成可控的替身。理由和 store 的测试一样：主键去重、聚合 SQL、裁剪的索引行为
// 只有真库能验证；用内存假实现测出来的只是假实现自己的逻辑。

var testBase = time.Date(2026, 10, 25, 8, 0, 0, 0, time.UTC)

// quietLogger 让用例的失败信息只来自断言，不被日志刷掉。
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func at(sec int64) time.Time { return testBase.Add(time.Duration(sec) * time.Second) }

func testDB(t *testing.T) *store.DB {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(context.Background(), store.Options{
		Path:   dir + "/upanel.db",
		Logger: quietLogger(),
	})
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Migrate(context.Background(), migrations.FS); err != nil {
		t.Fatalf("迁移测试库失败: %v", err)
	}
	return db
}

// fakeSummary 是可控的系统快照来源。
type fakeSummary struct {
	sum monitoring.Summary
	err error
}

func (f *fakeSummary) Summary(ctx context.Context) (monitoring.Summary, error) {
	return f.sum, f.err
}

// fakeTopology 是可控的存储拓扑来源。
type fakeTopology struct {
	top storage.Topology
	err error
}

func (f *fakeTopology) Topology(ctx context.Context) (storage.Topology, error) {
	return f.top, f.err
}

// clock 是可推进的时钟。
type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) set(t time.Time)         { c.t = t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

// healthySummary 构造一份“所有段都成功”的快照。
func healthySummary(ts time.Time, cpu, load1, memUsedPercent float64) monitoring.Summary {
	collected := ts
	total := uint64(4 * 1024 * 1024 * 1024)
	available := uint64(float64(total) * (1 - memUsedPercent/100))
	return monitoring.Summary{
		CollectedAt: ts,
		IntervalMS:  5000,
		Host:        monitoring.HostSection{Section: monitoring.Section{Status: monitoring.StatusOK, CollectedAt: &collected}},
		CPU: monitoring.CPUSection{
			Section:      monitoring.Section{Status: monitoring.StatusOK, CollectedAt: &collected},
			UsagePercent: &cpu,
		},
		Memory: monitoring.MemorySection{
			Section:        monitoring.Section{Status: monitoring.StatusOK, CollectedAt: &collected},
			TotalBytes:     &total,
			AvailableBytes: &available,
		},
		Load: monitoring.LoadSection{
			Section: monitoring.Section{Status: monitoring.StatusOK, CollectedAt: &collected},
			Load1:   &load1,
		},
		Network: monitoring.NetworkSection{
			Section: monitoring.Section{Status: monitoring.StatusOK, CollectedAt: &collected},
		},
	}
}

func TestDeriveHostSkipsFailedSectionsAndNulls(t *testing.T) {
	sum := healthySummary(at(0), 12.5, 0.4, 25)
	// CPU 段失败：段里会留着上一次成功的值，但那一刻的值是未知的。
	sum.CPU.Status = monitoring.StatusError
	// 内存总量有、可用内存没有：算不出使用率。
	sum.Memory.AvailableBytes = nil

	samples := DeriveHost(sum)
	for _, s := range samples {
		switch s.Metric {
		case MetricCPUUsagePercent:
			t.Fatal("CPU 段失败时不应写入使用率（那是上一次成功的值）")
		case MetricMemoryUsedPercent:
			t.Fatal("可用内存为 null 时不应写入内存使用率（0 是假的）")
		}
	}
	if len(samples) != 1 || samples[0].Metric != MetricLoad1 {
		t.Fatalf("只剩负载一条，实际 %+v", samples)
	}
	if !samples[0].TS.Equal(at(0)) {
		t.Fatalf("时间戳 = %v，期望段自己的 collected_at", samples[0].TS)
	}
}

func TestDeriveHostUsesSectionTimestamp(t *testing.T) {
	// 快照时间是 60 秒，但 CPU 段的值是 10 秒时读到的：历史必须记那个更早的时间，
	// 否则采集卡住时会把旧值扮成新值。
	sum := healthySummary(at(60), 12.5, 0.4, 25)
	old := at(10)
	sum.CPU.CollectedAt = &old
	for _, s := range DeriveHost(sum) {
		if s.Metric != MetricCPUUsagePercent {
			continue
		}
		if !s.TS.Equal(old) {
			t.Fatalf("CPU 样本时间 = %v，期望段自己的 %v", s.TS, old)
		}
		return
	}
	t.Fatal("没有写入 CPU 样本")
}

func TestDeriveNetworkSkipsLoopbackAndNullRates(t *testing.T) {
	sum := healthySummary(at(0), 1, 1, 1)
	rx, tx := 1024.0, 2048.0
	noRate := 0.0
	sum.Network.Interfaces = []monitoring.NetInterface{
		{Name: "lo", Loopback: true, RXBytesPerSecond: &rx, TXBytesPerSecond: &tx},
		{Name: "eth0", RXBytesPerSecond: &rx, TXBytesPerSecond: &tx},
		{Name: "eth1", RXBytesPerSecond: nil, TXBytesPerSecond: &noRate},
	}
	samples := DeriveNetwork(sum)
	var seen []string
	for _, s := range samples {
		seen = append(seen, s.TargetID+"/"+s.Metric)
		if s.TargetID == "lo" {
			t.Fatal("回环不应该进历史曲线")
		}
		if s.TargetID == "eth1" && s.Metric == MetricNetRXBytesPerSec {
			t.Fatal("速率为 null 时不应写入（0 是假的）")
		}
	}
	if len(seen) != 3 {
		t.Fatalf("期望 eth0 两条 + eth1 一条，实际 %v", seen)
	}
}

func TestDeriveFilesystemsSkipsNullCapacity(t *testing.T) {
	collected := at(0)
	top := storage.Topology{
		CollectedAt: at(0),
		Nodes: []storage.Device{
			{
				ID: "sda1",
				Filesystem: &storage.Filesystem{
					Mounts: []storage.Mount{
						{Path: "/", Capacity: &storage.Capacity{UsedPercent: 40, InodeUsedPercent: 5, AvailableBytes: 100, CollectedAt: collected}},
						{Path: "/mnt/offline", Capacity: nil},
					},
				},
			},
		},
		NetworkMounts: []storage.NetworkMount{
			{Path: "/mnt/nfs", Capacity: nil},
		},
	}
	samples := DeriveFilesystems(top)
	targets := map[string]int{}
	for _, s := range samples {
		targets[s.TargetID]++
	}
	if targets["/"] != 3 {
		t.Fatalf("根挂载应当有 3 条样本，实际 %d", targets["/"])
	}
	if targets["/mnt/offline"] != 0 || targets["/mnt/nfs"] != 0 {
		t.Fatalf("没有容量的挂载点不应写入任何样本: %v", targets)
	}
}

func newRecorder(t *testing.T, db *store.DB, sum *fakeSummary, top *fakeTopology, c *clock, retention Retention, maxRows int64) *Recorder {
	t.Helper()
	r, err := NewRecorder(Options{
		Store:              db,
		Summary:            sum,
		Topology:           top,
		Logger:             quietLogger(),
		Interval:           time.Second,
		FilesystemInterval: time.Second,
		Retention:          retention,
		MaxRows:            maxRows,
		Now:                c.now,
	})
	if err != nil {
		t.Fatalf("构造 Recorder 失败: %v", err)
	}
	return r
}

func TestRecorderDoesNotWriteSameSnapshotTwice(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	c := &clock{t: at(0)}
	sum := &fakeSummary{sum: healthySummary(at(0), 10, 0.5, 20)}
	r := newRecorder(t, db, sum, nil, c, DefaultRetention(), DefaultMaxRows)

	// 轮询比采集快：同一份快照会被读到好几次。
	for i := 0; i < 3; i++ {
		r.Pass(ctx)
	}
	counts, err := db.MetricSampleCounts(ctx)
	if err != nil {
		t.Fatalf("统计行数失败: %v", err)
	}
	// 主机三条（cpu、load1、memory）+ 没有网卡 = 3 行。重复读三次仍然 3 行。
	if counts["total"] != 3 {
		t.Fatalf("总行数 = %d，期望 3（同一份快照只写一次）", counts["total"])
	}
}

func TestRecorderAggregatesOnlyCompletedBuckets(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	c := &clock{t: at(0)}
	sum := &fakeSummary{sum: healthySummary(at(0), 10, 0.5, 20)}
	r := newRecorder(t, db, sum, nil, c, DefaultRetention(), DefaultMaxRows)

	// 第 0 秒写一个原始点；第 30 秒时这个分钟桶还没结束。
	r.Pass(ctx)
	sum.sum = healthySummary(at(30), 20, 0.6, 21)
	c.set(at(30))
	r.Pass(ctx)
	if series := queryAll(t, db, store.MetricResolutionMinute, at(0), at(120), 60); len(series) != 0 {
		t.Fatalf("桶还没结束就聚合了: %+v", series)
	}

	// 第 65 秒：整分钟的桶结束，聚合应当发生，加权平均 = (10+20)/2 = 15。
	c.set(at(65))
	sum.sum = healthySummary(at(65), 30, 0.7, 22)
	r.Pass(ctx)
	series := queryAll(t, db, store.MetricResolutionMinute, at(0), at(120), 60)
	var cpu *store.MetricSeries
	for i := range series {
		if series[i].Metric == MetricCPUUsagePercent {
			cpu = &series[i]
		}
	}
	if cpu == nil || len(cpu.Points) != 1 {
		t.Fatalf("分钟桶应当有 1 个 CPU 点: %+v", series)
	}
	if v := cpu.Points[0].Value; v < 14.99 || v > 15.01 {
		t.Fatalf("分钟 CPU 均值 = %v，期望 15", v)
	}
	if !cpu.Points[0].TS.Equal(at(0)) {
		t.Fatalf("分钟桶起点 = %v，期望 %v", cpu.Points[0].TS, at(0))
	}
}

func TestRecorderPrunesExpiredSamples(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	c := &clock{t: at(0)}
	sum := &fakeSummary{sum: healthySummary(at(0), 10, 0.5, 20)}
	// 原始留 120 秒、分钟留 10 分钟、小时留 1 小时：压缩到秒级，一次用例就能
	// 走完“先聚合、后裁剪”的真实顺序。注意聚合窗口等于**源粒度的保留窗口**，
	// 所以原始桶必须在它过期之前被聚合成分钟，否则就只剩被丢掉这一条路。
	r := newRecorder(t, db, sum, nil, c, Retention{
		Raw: 2 * time.Minute, Minute: 10 * time.Minute, Hour: time.Hour,
	}, DefaultMaxRows)

	r.Pass(ctx)
	c.set(at(5))
	sum.sum = healthySummary(at(5), 11, 0.5, 20)
	r.Pass(ctx)
	if counts := counts(t, db); counts[store.MetricResolutionRaw] == 0 {
		t.Fatal("前置条件：应当已经写入原始样本")
	}

	// 第 60 秒：[0,60) 这个分钟桶完整了，聚合发生（窗口 120 秒覆盖得到它）。
	c.set(at(60))
	sum.sum = healthySummary(at(60), 12, 0.5, 20)
	r.Pass(ctx)
	if got := counts(t, db)[store.MetricResolutionMinute]; got == 0 {
		t.Fatal("前置条件：分钟桶在这一轮就应当被聚合出来")
	}

	// 第 400 秒：原始样本只剩 120 秒，前面全部过期；分钟层还在保留期内。
	c.set(at(400))
	sum.sum = healthySummary(at(400), 13, 0.5, 20)
	r.Pass(ctx)

	after := counts(t, db)
	// 只该留下第 400 秒这份快照的 3 行（cpu/load/memory）。
	if after[store.MetricResolutionRaw] != 3 {
		t.Fatalf("原始样本剩余 %d 行，期望 3 行（刚刚那份快照）", after[store.MetricResolutionRaw])
	}
	if after[store.MetricResolutionMinute] == 0 {
		t.Fatal("分钟聚合不应被原始样本的保留窗口带走")
	}
}

func TestRecorderRowLimitDropsOldest(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	c := &clock{t: at(0)}
	sum := &fakeSummary{sum: healthySummary(at(0), 10, 0.5, 20)}
	r := newRecorder(t, db, sum, nil, c, DefaultRetention(), 4)

	// 十个时间桶，每个 3 行，共 30 行，远超上限 4。
	for i := int64(0); i < 10; i++ {
		db.InsertMetricSamples(ctx, []store.MetricSample{
			{Scope: store.MetricScopeHost, Metric: MetricCPUUsagePercent, Resolution: store.MetricResolutionRaw,
				TS: at(i * 5), Value: 1, MinValue: 1, MaxValue: 1, SampleCount: 1},
			{Scope: store.MetricScopeHost, Metric: MetricLoad1, Resolution: store.MetricResolutionRaw,
				TS: at(i * 5), Value: 1, MinValue: 1, MaxValue: 1, SampleCount: 1},
			{Scope: store.MetricScopeHost, Metric: MetricMemoryUsedPercent, Resolution: store.MetricResolutionRaw,
				TS: at(i * 5), Value: 1, MinValue: 1, MaxValue: 1, SampleCount: 1},
		})
	}
	r.Pass(ctx)

	after := counts(t, db)
	if after["total"] > 4 {
		t.Fatalf("行数上限没生效：仍有 %d 行", after["total"])
	}
	// 丢的是最老的：最新那个桶必须还在（上限被配得再小也不该把整层删空）。
	series := queryAll(t, db, store.MetricResolutionRaw, at(45), at(50), 5)
	if len(series) == 0 {
		t.Fatal("最新一个时间桶被误删了")
	}
}

func TestRecorderKeepsGoingWhenAgentUnavailable(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	c := &clock{t: at(0)}
	sum := &fakeSummary{err: monitoring.ErrAgentUnavailable}
	r := newRecorder(t, db, sum, nil, c, DefaultRetention(), DefaultMaxRows)

	r.Pass(ctx) // 不应 panic，也不应写入任何东西
	if after := counts(t, db); after["total"] != 0 {
		t.Fatalf("代理不可用时写入了 %d 行", after["total"])
	}
	stats, _, err := r.Stats(ctx)
	if err != nil {
		t.Fatalf("读取状态失败: %v", err)
	}
	if stats.LastError == "" || stats.LastErrorAt == nil {
		t.Fatal("失败没有被记进状态（页面上就查不出为什么没有数据）")
	}
}

func TestReaderStatusWithoutRecorder(t *testing.T) {
	db := testDB(t)
	reader, err := NewReader(ReaderOptions{
		Store:            db,
		NoRecorderReason: "配置里关闭了历史采集",
	})
	if err != nil {
		t.Fatalf("构造 Reader 失败: %v", err)
	}
	st, err := reader.Status(context.Background())
	if err != nil {
		t.Fatalf("读取状态失败: %v", err)
	}
	if !st.Enabled {
		t.Fatal("库在就应当 Enabled=true（旧数据可查）")
	}
	if st.Collecting {
		t.Fatal("没有采集器时 Collecting 不能是 true")
	}
	if st.Reason != "配置里关闭了历史采集" {
		t.Fatalf("原因 = %q", st.Reason)
	}
	if len(st.Metrics) == 0 {
		t.Fatal("状态里应当带上登记过的指标，界面据此画选择框")
	}
}

// ---- 查询层（不碰数据库，只验证参数校验与粒度选择） ----

func TestPlanQueryRejectsBadParams(t *testing.T) {
	now := at(0)
	cases := []struct {
		name   string
		params QueryParams
		want   string
	}{
		{"缺 scope", QueryParams{}, "scope"},
		{"未知 scope", QueryParams{Scope: "disk"}, "scope"},
		{"未知指标", QueryParams{Scope: store.MetricScopeHost, Metrics: []string{"nope"}}, "指标"},
		{"指标属于别的 scope", QueryParams{Scope: store.MetricScopeHost, Metrics: []string{MetricFSUsedPercent}}, "指标"},
		{"空 target", QueryParams{Scope: store.MetricScopeInterface, TargetIDs: []string{""}}, "target_id"},
		{"from 不早于 to", QueryParams{Scope: store.MetricScopeHost, From: now, To: now}, "早于"},
		{"跨度超一年", QueryParams{Scope: store.MetricScopeHost, From: now.Add(-400 * 24 * time.Hour), To: now}, "跨度"},
		{"to 在未来", QueryParams{Scope: store.MetricScopeHost, From: now.Add(-time.Hour), To: now.Add(10 * time.Minute)}, "未来"},
	}
	for _, c := range cases {
		_, err := planQuery(c.params, now, defaultCoverage())
		if err == nil {
			t.Fatalf("%s: 期望报错", c.name)
		}
		if !IsInvalidQuery(err) {
			t.Fatalf("%s: 错误类型不是 InvalidQueryError: %v", c.name, err)
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: 错误信息 %q 里没有 %q", c.name, err.Error(), c.want)
		}
	}
}

func TestPlanQueryChoosesResolution(t *testing.T) {
	now := at(0)
	cases := []struct {
		name       string
		params     QueryParams
		resolution string
		step       int64
	}{
		{"默认回看一小时", QueryParams{Scope: store.MetricScopeHost}, store.MetricResolutionRaw, 5},
		{"一天跨度仍在 raw 层，步长加粗到一分钟", QueryParams{Scope: store.MetricScopeHost, From: now.Add(-24 * time.Hour), To: now},
			store.MetricResolutionRaw, 60},
		{"三十天跨度落到分钟层", QueryParams{Scope: store.MetricScopeHost, From: now.Add(-20 * 24 * time.Hour), To: now},
			store.MetricResolutionMinute, 1200},
		{"一年退到小时", QueryParams{Scope: store.MetricScopeHost, From: now.Add(-365 * 24 * time.Hour), To: now},
			store.MetricResolutionHour, 25200},
		{"显式大步长仍然读最细的粒度", QueryParams{Scope: store.MetricScopeHost, From: now.Add(-time.Hour), To: now, StepSeconds: 300},
			store.MetricResolutionRaw, 300},
	}
	for _, c := range cases {
		plan, err := planQuery(c.params, now, defaultCoverage())
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if plan.resolution != c.resolution || plan.step != c.step {
			t.Fatalf("%s: 得到 %s/%d 秒，期望 %s/%d 秒",
				c.name, plan.resolution, plan.step, c.resolution, c.step)
		}
		if n := bucketCount(plan.from, plan.to, plan.step); n > MaxPoints {
			t.Fatalf("%s: 点数 %d 超过上限 %d", c.name, n, MaxPoints)
		}
	}
}

func TestBucketCountAlignsToStep(t *testing.T) {
	// [0,90) 按 30 秒分桶：0、30、60 三个桶。
	if n := bucketCount(at(0), at(90), 30); n != 3 {
		t.Fatalf("桶数 = %d，期望 3", n)
	}
	// 起点不在整数倍上：桶仍然按 step 的整数倍对齐。
	if n := bucketCount(at(5), at(65), 30); n != 2 {
		t.Fatalf("桶数 = %d，期望 2（30 与 60）", n)
	}
	if n := bucketCount(at(0), at(0), 30); n != 0 {
		t.Fatalf("空区间桶数 = %d，期望 0", n)
	}
}

func TestQueryReportsMissingPointsNotZero(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	// 五个一分钟桶（0/60/120/180/240）里只有第 0 个和第 2 个有数据：
	// 每个桶里放一条 5 秒样本，查询按 60 秒卷一次。
	for _, offset := range []int64{0, 120} {
		if _, err := db.InsertMetricSamples(ctx, []store.MetricSample{{
			Scope: store.MetricScopeHost, Metric: MetricCPUUsagePercent,
			Resolution: store.MetricResolutionRaw, TS: at(offset),
			Value: 10, MinValue: 10, MaxValue: 10, SampleCount: 1,
		}}); err != nil {
			t.Fatalf("写入失败: %v", err)
		}
	}
	reader, err := NewReader(ReaderOptions{Store: db, Now: func() time.Time { return at(300) }})
	if err != nil {
		t.Fatalf("构造 Reader 失败: %v", err)
	}
	res, err := reader.Query(ctx, QueryParams{
		Scope: store.MetricScopeHost,
		From:  at(0), To: at(300), StepSeconds: 60,
	})
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if !res.Enabled {
		t.Fatal("Enabled 应当是 true")
	}
	if len(res.Series) != 1 {
		t.Fatalf("序列数 = %d，期望 1", len(res.Series))
	}
	if got := len(res.Series[0].Points); got != 2 {
		t.Fatalf("点数 = %d，期望 2（只有真有数据的桶）", got)
	}
	if res.Series[0].MissingPoints != 3 {
		t.Fatalf("missing_points = %d，期望 3（缺失的三个桶不能被补成 0）",
			res.Series[0].MissingPoints)
	}
	for _, p := range res.Series[0].Points {
		if p.Value == 0 {
			t.Fatal("出现了值为 0 的点：缺失被补零了")
		}
	}
	if res.ObservedFrom == nil || !res.ObservedFrom.Equal(at(0)) {
		t.Fatalf("observed_from = %v", res.ObservedFrom)
	}
	if res.Series[0].Unit != UnitPercent {
		t.Fatalf("unit = %q，期望 percent", res.Series[0].Unit)
	}
}

func TestQueryEmptyRangeReportsNoObservation(t *testing.T) {
	db := testDB(t)
	reader, err := NewReader(ReaderOptions{Store: db, Now: func() time.Time { return at(3600) }})
	if err != nil {
		t.Fatalf("构造 Reader 失败: %v", err)
	}
	res, err := reader.Query(context.Background(), QueryParams{
		Scope: store.MetricScopeHost, From: at(0), To: at(600),
	})
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if len(res.Series) != 0 {
		t.Fatalf("没有数据时不该凭空造出序列: %+v", res.Series)
	}
	if res.ObservedFrom != nil || res.ObservedTo != nil {
		t.Fatal("没有数据时 observed_from/to 必须是 null（与“有数据但稀疏”分开）")
	}
}

func TestQueryPropagatesInvalidParams(t *testing.T) {
	db := testDB(t)
	reader, err := NewReader(ReaderOptions{Store: db, Now: func() time.Time { return at(0) }})
	if err != nil {
		t.Fatalf("构造 Reader 失败: %v", err)
	}
	_, err = reader.Query(context.Background(), QueryParams{Scope: store.MetricScopeHost, Metrics: []string{"nope"}})
	if err == nil || !IsInvalidQuery(err) {
		t.Fatalf("未知指标应当返回 InvalidQueryError，实际 %v", err)
	}
	var invalid *InvalidQueryError
	if !errors.As(err, &invalid) {
		t.Fatal("errors.As 取不到 InvalidQueryError")
	}
}

// ---- 小工具 ----

func counts(t *testing.T, db *store.DB) map[string]int64 {
	t.Helper()
	c, err := db.MetricSampleCounts(context.Background())
	if err != nil {
		t.Fatalf("统计行数失败: %v", err)
	}
	return c
}

func queryAll(t *testing.T, db *store.DB, resolution string, from, to time.Time, step int64) []store.MetricSeries {
	t.Helper()
	series, err := db.QueryMetricSeries(context.Background(), store.MetricQuery{
		Scope: store.MetricScopeHost, From: from, To: to,
		Resolution: resolution, Step: step,
	})
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	return series
}
