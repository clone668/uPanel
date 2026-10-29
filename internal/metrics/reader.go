package metrics

// P13.1：查询端与状态端。
//
// 两条接口分工明确：
//
//   - Query 回答“过去怎么走的”，只读样本表；
//   - Status 回答“这个进程到底在不在记”，它把运行时事实（本进程写了多少、
//     最后一次成功是什么时候、最近一次错误是什么）与库里的行数放在一起。
//
// 分开的理由很实际：图会反复拉 Query，而 Status 里的行数统计是全表 COUNT(*)。
// 合成一个接口就是让每次刷新都做一次全表统计。

import (
	"context"
	"errors"
	"fmt"
	"time"

	"upanel/internal/store"
)

// MetricSeriesRef 是 store.MetricSeriesRef 的别名。
//
// API 层要在“没有采集器”的分支里造一个空的目录，用别名就不必为了一个结构体
// 再去导入 store——它对外暴露的形状与 JSON 字段完全一致。
type MetricSeriesRef = store.MetricSeriesRef

// Reader 是历史查询的实现。
type Reader struct {
	store            MetricStore
	recorder         *Recorder
	now              func() time.Time
	noRecorderReason string
}

// ReaderOptions 是 Reader 的构造参数。
type ReaderOptions struct {
	Store MetricStore
	// Recorder 可以为 nil：那样 Status 只报库里的行数，运行时事实一栏说清“没有采集器”。
	Recorder *Recorder
	// NoRecorderReason 说明“为什么没有采集器”。它只在该出现的时候出现：
	// 配置里关掉了采集（web.metrics.enabled=false）与装配时忘了接是两件事，
	// 面板上那一句提示必须是人能照着改的。
	NoRecorderReason string
	// Now 是可注入的时钟，测试用；nil 时用 time.Now。
	Now func() time.Time
}

// NewReader 构造 Reader。没有存储就没有历史可查——那是装配错误，不是运行期状态。
func NewReader(opts ReaderOptions) (*Reader, error) {
	if opts.Store == nil {
		return nil, errors.New("metrics: 缺少样本存储")
	}
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	reason := opts.NoRecorderReason
	if reason == "" {
		reason = "没有采集器在运行（装配时未接上）"
	}
	return &Reader{store: opts.Store, recorder: opts.Recorder, now: now, noRecorderReason: reason}, nil
}

// Status 是 GET /api/v1/system/metrics/status 的响应体。
type Status struct {
	// Enabled 表示这个进程能回答历史问题（库在）。它**不**表示正在记：
	// 采集被配置关掉之后，旧数据仍然可查，那才是诚实的状态。
	Enabled bool   `json:"enabled"`
	Reason  string `json:"reason,omitempty"`
	// Collecting 表示采集循环在这个进程里跑着。Enabled=true 且 Collecting=false
	// 就是“查得到旧数据、但不再写新数据”，Reason 说明是哪一种原因。
	Collecting bool `json:"collecting"`

	IntervalSeconds           int64 `json:"interval_seconds"`
	FilesystemIntervalSeconds int64 `json:"filesystem_interval_seconds"`
	Retention                 struct {
		RawSeconds    int64 `json:"raw_seconds"`
		MinuteSeconds int64 `json:"minute_seconds"`
		HourSeconds   int64 `json:"hour_seconds"`
	} `json:"retention"`
	MaxRows int64 `json:"max_rows"`

	// Samples 是库里的行数（键：raw / minute / hour / total）。
	Samples map[string]int64 `json:"samples"`

	// LastSampleAt 是本进程最后一次成功写入主机样本的时间。重启后从零开始——
	// 它说的是“这个进程”，不是“这台机器的历史”。
	LastSampleAt *time.Time `json:"last_sample_at"`
	LastError    string     `json:"last_error,omitempty"`
	LastErrorAt  *time.Time `json:"last_error_at"`

	// Inserted / Aggregated / Pruned 是本进程的累计行数（不是样本条数：一条序列一个点
	// 算一行）。它们只用来判断“循环真的在动”，不参与任何产品结论。
	Inserted   int64 `json:"inserted"`
	Aggregated int64 `json:"aggregated"`
	Pruned     int64 `json:"pruned"`

	// Metrics 是登记过的指标（产品支持哪些曲线）。
	Metrics []Definition `json:"metrics"`
	// Series 是库里真正出现过的序列（这台机器的历史里有什么）。
	Series map[string][]MetricSeriesRef `json:"series"`
	// GeneratedAt 是这次查询的服务端时间。
	GeneratedAt time.Time `json:"generated_at"`
}

// Status 汇总运行时事实与库里的行数。
func (r *Reader) Status(ctx context.Context) (Status, error) {
	out := Status{
		Enabled:     true,
		Collecting:  r.recorder != nil,
		Metrics:     Definitions(),
		MaxRows:     DefaultMaxRows,
		Samples:     map[string]int64{"total": 0},
		GeneratedAt: r.now().UTC(),
	}

	if r.recorder != nil {
		stats, counts, err := r.recorder.Stats(ctx)
		if err != nil {
			return out, err
		}
		out.IntervalSeconds = stats.IntervalSeconds
		out.FilesystemIntervalSeconds = stats.FilesystemInterval
		out.Retention.RawSeconds = stats.RetentionRawSeconds
		out.Retention.MinuteSeconds = stats.RetentionMinuteSecs
		out.Retention.HourSeconds = stats.RetentionHourSeconds
		out.MaxRows = stats.MaxRows
		out.Samples = counts
		out.LastSampleAt = stats.LastSampleAt
		out.LastError = stats.LastError
		out.LastErrorAt = stats.LastErrorAt
		out.Inserted = stats.Inserted
		out.Aggregated = stats.Aggregated
		out.Pruned = stats.Pruned
	} else {
		out.Reason = r.noRecorderReason
		counts, err := r.store.MetricSampleCounts(ctx)
		if err != nil {
			return out, fmt.Errorf("metrics: 读取样本行数失败: %w", err)
		}
		out.Samples = counts
	}

	catalog, err := r.store.MetricSeriesCatalog(ctx)
	if err != nil {
		return out, err
	}
	out.Series = catalog
	return out, nil
}
