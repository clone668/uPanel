package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"upanel/migrations"
)

// openMigrated 打开一个已迁移到最新版本的临时库。
//
// 样本表的关键行为（主键去重、聚合 SQL、索引裁剪）只有真 SQLite 能验证：
// 用内存假实现测“INSERT OR IGNORE 会不会去重”只会测到假实现自己的逻辑。
func openMigrated(t *testing.T) *DB {
	t.Helper()
	dir := t.TempDir()
	db, err := Open(context.Background(), Options{
		Path:   filepath.Join(dir, "upanel.db"),
		Logger: testLogger(),
	})
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Migrate(context.Background(), migrations.FS); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	return db
}

// base 是所有用例的基准时间。**不能用 Unix epoch**：迁移里 ts > 0 的 CHECK 会把
// 0 秒挡掉，而用 0 做基准会让人以为是“代码对 epoch 有特殊处理”。真实时间更好读。
var base = time.Date(2026, 10, 25, 8, 0, 0, 0, time.UTC)

// at 返回基准时间之后 sec 秒。用例里所有时间偏移都走它，避免手写 time.Unix 把
// 秒和纳秒搞混。
func at(sec int64) time.Time { return base.Add(time.Duration(sec) * time.Second) }

func rawSample(target, metric string, ts time.Time, value float64) MetricSample {
	return MetricSample{
		Scope: MetricScopeHost, TargetID: target, Metric: metric,
		Resolution: MetricResolutionRaw, TS: ts,
		Value: value, MinValue: value, MaxValue: value, SampleCount: 1,
	}
}

func TestInsertMetricSamplesIsIdempotent(t *testing.T) {
	db := openMigrated(t)
	ctx := context.Background()
	ts := time.Date(2026, 10, 25, 8, 0, 0, 0, time.UTC).Add(3 * time.Second)

	// 同一份代理快照被写两次：时间戳相同（采集时间没变），值也相同。
	batch := []MetricSample{
		rawSample("", "cpu_usage_percent", ts, 12.5),
		rawSample("", "load1", ts, 0.4),
	}
	first, err := db.InsertMetricSamples(ctx, batch)
	if err != nil {
		t.Fatalf("首次写入失败: %v", err)
	}
	if first != 2 {
		t.Fatalf("首次写入 %d 行，期望 2 行", first)
	}
	second, err := db.InsertMetricSamples(ctx, batch)
	if err != nil {
		t.Fatalf("重复写入失败: %v", err)
	}
	if second != 0 {
		t.Fatalf("重复写入新增了 %d 行，期望 0 行（主键应当挡掉）", second)
	}

	counts, err := db.MetricSampleCounts(ctx)
	if err != nil {
		t.Fatalf("统计行数失败: %v", err)
	}
	if counts["total"] != 2 {
		t.Fatalf("总行数 = %d，期望 2", counts["total"])
	}
}

func TestInsertMetricSamplesAlignsToBucket(t *testing.T) {
	db := openMigrated(t)
	ctx := context.Background()
	// 原始粒度的桶宽是 5 秒：3 秒与 7 秒落在同一个桶（0—5 与 5—10 其实不同桶，
	// 这里刻意取 5 秒内的两个时刻验证对齐：1 秒与 4 秒都对齐到 0）。
	db.InsertMetricSamples(ctx, []MetricSample{
		rawSample("", "load1", at(1), 1),
		rawSample("", "load5", at(4), 2),
	})
	series, err := db.QueryMetricSeries(ctx, MetricQuery{
		Scope: MetricScopeHost, From: at(0), To: at(10),
		Resolution: MetricResolutionRaw, Step: 5,
	})
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if len(series) != 2 {
		t.Fatalf("序列数 = %d，期望 2", len(series))
	}
	for _, s := range series {
		if len(s.Points) != 1 {
			t.Fatalf("%s 的点数 = %d，期望 1", s.Metric, len(s.Points))
		}
		if !s.Points[0].TS.Equal(at(0)) {
			t.Fatalf("%s 的时间 = %v，期望对齐到 0", s.Metric, s.Points[0].TS)
		}
	}
}

func TestAggregateMetricBucketWeightedAverage(t *testing.T) {
	db := openMigrated(t)
	ctx := context.Background()

	// 分钟桶 [0,60)：两个原始点 10 与 20。
	db.InsertMetricSamples(ctx, []MetricSample{
		rawSample("", "cpu_usage_percent", at(0), 10),
		rawSample("", "cpu_usage_percent", at(5), 20),
	})
	if _, err := db.AggregateMetricBucket(ctx, MetricResolutionRaw, MetricResolutionMinute,
		at(0)); err != nil {
		t.Fatalf("分钟聚合失败: %v", err)
	}

	// 小时桶 [3600,7200)：两个分钟点，样本数不同（3 与 1），加权平均必须是 15。
	// 不按样本数加权的话会得到 (10+30)/2 = 20，一个停机时段的少量样本就被放大了。
	bucket := at(3600)
	db.InsertMetricSamples(ctx, []MetricSample{
		{
			Scope: MetricScopeHost, Metric: "cpu_usage_percent", Resolution: MetricResolutionMinute,
			TS: bucket, Value: 10, MinValue: 8, MaxValue: 11, SampleCount: 3,
		},
		{
			Scope: MetricScopeHost, Metric: "cpu_usage_percent", Resolution: MetricResolutionMinute,
			TS: bucket.Add(time.Minute), Value: 30, MinValue: 25, MaxValue: 31, SampleCount: 1,
		},
	})
	if _, err := db.AggregateMetricBucket(ctx, MetricResolutionMinute, MetricResolutionHour, bucket); err != nil {
		t.Fatalf("小时聚合失败: %v", err)
	}

	series, err := db.QueryMetricSeries(ctx, MetricQuery{
		Scope: MetricScopeHost, From: at(0), To: at(7200),
		Resolution: MetricResolutionHour, Step: 3600,
	})
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if len(series) != 1 || len(series[0].Points) != 1 {
		t.Fatalf("小时的点数不对: %+v", series)
	}
	p := series[0].Points[0]
	if p.Value < 14.99 || p.Value > 15.01 {
		t.Fatalf("加权平均 = %v，期望 15", p.Value)
	}
	if p.MinValue != 8 || p.MaxValue != 31 {
		t.Fatalf("区间 = [%v,%v]，期望 [8,31]", p.MinValue, p.MaxValue)
	}
	if p.SampleCount != 4 {
		t.Fatalf("样本数 = %d，期望 4", p.SampleCount)
	}
}

// TestAggregateMetricBucketEqualValuesStayInRange 复现真机缺陷：一整桶的样本值完全相同
// （inode_used_percent 三个样本都是 1.9、used_percent 三个都是 14.2）时，加权平均在双精度
// 下会偏离到区间外一个 ulp，撞上表上的 CHECK (min_value <= value <= max_value)，导致
// 整个小时桶写不进去、每轮重试都失败——那一小时的所有指标就永久缺一段。
func TestAggregateMetricBucketEqualValuesStayInRange(t *testing.T) {
	db := openMigrated(t)
	ctx := context.Background()
	bucket := at(3600)

	samples := []MetricSample{
		{
			Scope: MetricScopeFilesystem, TargetID: "/", Metric: "inode_used_percent",
			Resolution: MetricResolutionMinute, TS: bucket,
			Value: 1.9, MinValue: 1.9, MaxValue: 1.9, SampleCount: 12,
		},
		{
			Scope: MetricScopeFilesystem, TargetID: "/", Metric: "inode_used_percent",
			Resolution: MetricResolutionMinute, TS: bucket.Add(time.Minute),
			Value: 1.9, MinValue: 1.9, MaxValue: 1.9, SampleCount: 12,
		},
		{
			Scope: MetricScopeFilesystem, TargetID: "/", Metric: "inode_used_percent",
			Resolution: MetricResolutionMinute, TS: bucket.Add(2 * time.Minute),
			Value: 1.9, MinValue: 1.9, MaxValue: 1.9, SampleCount: 12,
		},
		{
			Scope: MetricScopeFilesystem, TargetID: "/boot", Metric: "used_percent",
			Resolution: MetricResolutionMinute, TS: bucket,
			Value: 14.2, MinValue: 14.2, MaxValue: 14.2, SampleCount: 12,
		},
	}
	if _, err := db.InsertMetricSamples(ctx, samples); err != nil {
		t.Fatalf("写入分钟样本失败: %v", err)
	}
	if _, err := db.AggregateMetricBucket(ctx, MetricResolutionMinute, MetricResolutionHour, bucket); err != nil {
		t.Fatalf("值相同时也必须聚合成功（浮点误差不能撞 CHECK）: %v", err)
	}

	series, err := db.QueryMetricSeries(ctx, MetricQuery{
		Scope: MetricScopeFilesystem, From: at(0), To: at(7200),
		Resolution: MetricResolutionHour, Step: 3600,
	})
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if len(series) != 2 {
		t.Fatalf("小时序列数 = %d，期望 2（两个挂载点各一个指标）", len(series))
	}
	for _, s := range series {
		if len(s.Points) != 1 {
			t.Fatalf("%s/%s 的点数 = %d，期望 1", s.TargetID, s.Metric, len(s.Points))
		}
		p := s.Points[0]
		if p.Value < p.MinValue || p.Value > p.MaxValue {
			t.Fatalf("%s/%s 的平均值 %v 落在区间 [%v,%v] 外", s.TargetID, s.Metric, p.Value, p.MinValue, p.MaxValue)
		}
		if p.SampleCount != 36 && p.SampleCount != 12 {
			t.Fatalf("%s/%s 的样本数 = %d，期望 36 或 12", s.TargetID, s.Metric, p.SampleCount)
		}
	}
}

func TestAggregateMetricBucketIsRerunnable(t *testing.T) {
	db := openMigrated(t)
	ctx := context.Background()
	db.InsertMetricSamples(ctx, []MetricSample{
		rawSample("", "load1", at(0), 1),
		rawSample("", "load1", at(5), 1),
	})
	bucket := at(0)
	for i := 0; i < 3; i++ {
		if _, err := db.AggregateMetricBucket(ctx, MetricResolutionRaw, MetricResolutionMinute, bucket); err != nil {
			t.Fatalf("第 %d 次聚合失败: %v", i+1, err)
		}
	}
	series, err := db.QueryMetricSeries(ctx, MetricQuery{
		Scope: MetricScopeHost, From: bucket, To: bucket.Add(time.Minute),
		Resolution: MetricResolutionMinute, Step: 60,
	})
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if len(series) != 1 || len(series[0].Points) != 1 {
		t.Fatalf("重算产生了重复行: %+v", series)
	}
	if series[0].Points[0].Value != 1 {
		t.Fatalf("值 = %v，期望 1", series[0].Points[0].Value)
	}
}

func TestPendingMetricBuckets(t *testing.T) {
	db := openMigrated(t)
	ctx := context.Background()
	db.InsertMetricSamples(ctx, []MetricSample{
		rawSample("", "load1", at(0), 1),
		rawSample("", "load1", at(60), 2),
	})

	pending, err := db.PendingMetricBuckets(ctx, MetricResolutionRaw, MetricResolutionMinute,
		at(0), at(120), 10)
	if err != nil {
		t.Fatalf("查询待聚合桶失败: %v", err)
	}
	if len(pending) != 2 {
		t.Fatalf("待聚合桶数 = %d，期望 2: %v", len(pending), pending)
	}
	if !pending[0].Equal(at(0)) || !pending[1].Equal(at(60)) {
		t.Fatalf("待聚合桶时间不对: %v", pending)
	}

	for _, b := range pending {
		if _, err := db.AggregateMetricBucket(ctx, MetricResolutionRaw, MetricResolutionMinute, b); err != nil {
			t.Fatalf("聚合失败: %v", err)
		}
	}
	pending, err = db.PendingMetricBuckets(ctx, MetricResolutionRaw, MetricResolutionMinute,
		at(0), at(120), 10)
	if err != nil {
		t.Fatalf("二次查询待聚合桶失败: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("聚合之后仍有待聚合桶: %v", pending)
	}
}

func TestPruneMetricSamplesKeepsInsideWindow(t *testing.T) {
	db := openMigrated(t)
	ctx := context.Background()
	db.InsertMetricSamples(ctx, []MetricSample{
		rawSample("", "load1", at(0), 1),
		rawSample("", "load1", at(600), 2),
	})

	deleted, err := db.PruneMetricSamples(ctx, MetricResolutionRaw, at(300))
	if err != nil {
		t.Fatalf("裁剪失败: %v", err)
	}
	if deleted == 0 {
		t.Fatal("裁剪没有删掉任何行")
	}
	counts, err := db.MetricSampleCounts(ctx)
	if err != nil {
		t.Fatalf("统计行数失败: %v", err)
	}
	if counts[MetricResolutionRaw] != 1 {
		t.Fatalf("裁剪后剩余 %d 行，期望 1（只删窗口外的）", counts[MetricResolutionRaw])
	}
}

func TestDeleteOldestMetricBuckets(t *testing.T) {
	db := openMigrated(t)
	ctx := context.Background()
	db.InsertMetricSamples(ctx, []MetricSample{
		rawSample("", "load1", at(0), 1),
		rawSample("", "load5", at(0), 1),
		rawSample("", "load1", at(5), 2),
		rawSample("", "load1", at(10), 3),
	})
	deleted, err := db.DeleteOldestMetricBuckets(ctx, MetricResolutionRaw, 1)
	if err != nil {
		t.Fatalf("按时间桶裁剪失败: %v", err)
	}
	if deleted != 2 {
		t.Fatalf("删除了 %d 行，期望 2 行（最老那个桶的两条序列）", deleted)
	}
	counts, err := db.MetricSampleCounts(ctx)
	if err != nil {
		t.Fatalf("统计行数失败: %v", err)
	}
	if counts[MetricResolutionRaw] != 2 {
		t.Fatalf("剩余 %d 行，期望 2", counts[MetricResolutionRaw])
	}
}

func TestQueryMetricSeriesDownsamplesByStep(t *testing.T) {
	db := openMigrated(t)
	ctx := context.Background()
	// 一分钟内 12 个原始点（5 秒一个），值是 0—11。
	for i := 0; i < 12; i++ {
		db.InsertMetricSamples(ctx, []MetricSample{
			rawSample("", "load1", at(int64(i*5)), float64(i)),
		})
	}
	series, err := db.QueryMetricSeries(ctx, MetricQuery{
		Scope: MetricScopeHost, From: at(0), To: at(60),
		Resolution: MetricResolutionRaw, Step: 30,
	})
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if len(series) != 1 || len(series[0].Points) != 2 {
		t.Fatalf("步长 30 秒应当得到 2 个点: %+v", series)
	}
	// 前 30 秒是 0—5，均值 2.5；后 30 秒是 6—11，均值 8.5。
	if v := series[0].Points[0].Value; v < 2.49 || v > 2.51 {
		t.Fatalf("第一个点 = %v，期望 2.5", v)
	}
	if v := series[0].Points[1].Value; v < 8.49 || v > 8.51 {
		t.Fatalf("第二个点 = %v，期望 8.5", v)
	}
	if series[0].Points[0].SampleCount != 6 {
		t.Fatalf("第一个点的样本数 = %d，期望 6", series[0].Points[0].SampleCount)
	}
}

func TestQueryMetricSeriesFilters(t *testing.T) {
	db := openMigrated(t)
	ctx := context.Background()
	db.InsertMetricSamples(ctx, []MetricSample{
		rawSample("", "load1", at(0), 1),
		rawSample("", "load5", at(0), 2),
		{
			Scope: MetricScopeInterface, TargetID: "eth0", Metric: "rx_bytes_per_sec",
			Resolution: MetricResolutionRaw, TS: at(0),
			Value: 3, MinValue: 3, MaxValue: 3, SampleCount: 1,
		},
	})

	series, err := db.QueryMetricSeries(ctx, MetricQuery{
		Scope: MetricScopeHost, Metrics: []string{"load1"},
		From: at(0), To: at(60),
		Resolution: MetricResolutionRaw, Step: 5,
	})
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if len(series) != 1 || series[0].Metric != "load1" {
		t.Fatalf("指标过滤没生效: %+v", series)
	}

	series, err = db.QueryMetricSeries(ctx, MetricQuery{
		Scope: MetricScopeInterface, TargetIDs: []string{"eth0"},
		From: at(0), To: at(60),
		Resolution: MetricResolutionRaw, Step: 5,
	})
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if len(series) != 1 || series[0].TargetID != "eth0" {
		t.Fatalf("目标过滤没生效: %+v", series)
	}

	series, err = db.QueryMetricSeries(ctx, MetricQuery{
		Scope: MetricScopeInterface, TargetIDs: []string{"eth1"},
		From: at(0), To: at(60),
		Resolution: MetricResolutionRaw, Step: 5,
	})
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if len(series) != 0 {
		t.Fatalf("不存在的目标应当返回空结果，实际 %+v", series)
	}
}

func TestMetricSeriesCatalog(t *testing.T) {
	db := openMigrated(t)
	ctx := context.Background()
	db.InsertMetricSamples(ctx, []MetricSample{
		rawSample("", "load1", at(0), 1),
		{
			Scope: MetricScopeFilesystem, TargetID: "/", Metric: "used_percent",
			Resolution: MetricResolutionRaw, TS: at(0),
			Value: 10, MinValue: 10, MaxValue: 10, SampleCount: 1,
		},
	})
	catalog, err := db.MetricSeriesCatalog(ctx)
	if err != nil {
		t.Fatalf("查询目录失败: %v", err)
	}
	if len(catalog[MetricScopeHost]) != 1 || len(catalog[MetricScopeFilesystem]) != 1 {
		t.Fatalf("目录不对: %+v", catalog)
	}
	if catalog[MetricScopeFilesystem][0].TargetID != "/" {
		t.Fatalf("挂载点没记下来: %+v", catalog[MetricScopeFilesystem])
	}
}

func TestQueryMetricSeriesRejectsStepBelowBucket(t *testing.T) {
	db := openMigrated(t)
	_, err := db.QueryMetricSeries(context.Background(), MetricQuery{
		Scope: MetricScopeHost, From: at(0), To: at(60),
		Resolution: MetricResolutionMinute, Step: 5,
	})
	if err == nil {
		t.Fatal("步长小于桶宽应当报错，而不是悄悄按桶宽算")
	}
}
