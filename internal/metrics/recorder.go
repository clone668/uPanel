package metrics

// P13.1：历史样本的写入、聚合、保留与行数上限。
//
// 这个循环做四件事，顺序固定：
//
//  1. 采：把 Web 已经拿到的两份快照（系统概况、存储拓扑）变成样本写进库里。
//     采集本身仍在代理侧；本包只决定“哪些值值得留、时间戳取哪个”。
//  2. 卷：把已经完整的原始桶卷成分钟桶，再把完整的分钟桶卷成小时桶。
//  3. 裁：按 PLAN.md 第 18 节的 24h / 30d / 365d 删过期数据。
//  4. 兜底：库里的行数超过上限时丢掉最老的整桶，宁可少一段历史，也不让面板的
//     数据库被历史撑满——CONTRACTS.md 第 14.1 节要求定期清理并限制数据库大小。
//
// 失败一律“记下来，下个周期再试”，绝不让一次代理不可用把整个面板拖住：历史断了是
// 可以补的，进程挂了不行。

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"time"

	"upanel/internal/monitoring"
	"upanel/internal/storage"
	"upanel/internal/store"
)

const (
	// DefaultInterval 是轮询代理快照的间隔。
	//
	// 它比代理的 5 秒采集**快一档**：5 秒轮询 5 秒采集必然踩点，踩上就丢一整份快照
	// （读到的是上一次那份）。写入本身按 collected_at 去重，所以轮询更勤不会写重，
	// 只会让“历史里的点就是代理真正采集的那些”这件事真的成立。
	DefaultInterval = 2 * time.Second
	// DefaultFilesystemInterval 是拉存储拓扑的间隔。代理侧容量缓存本来就是 30 秒。
	DefaultFilesystemInterval = 30 * time.Second

	// pruneInterval 是保留策略的执行间隔。一分钟一次足够：裁剪是幂等的，
	// 而保留窗口是小时级的。
	pruneInterval = time.Minute
	// rowLimitInterval 是行数上限的检查间隔。COUNT(*) 走索引，但没必要每秒来一次。
	rowLimitInterval = 10 * time.Minute

	// aggregateBatch 是一次补算请求的桶数上限。它让启动时的补算分批拿写锁，
	// 不至于把一次启动变成“几十秒内谁都写不进库”。
	aggregateBatch = 200
	// aggregateBucketsPerPass 是一个周期内最多补算的桶数。积压再多也留给下个周期，
	// 保持循环的周期稳定。
	aggregateBucketsPerPass = 4000

	// logInterval 是同一类错误在两个日志之间的最短间隔。代理不可用时每 2 秒一条
	// 告警日志会把 journal 刷满，把真正的信息埋掉。
	logInterval = time.Minute
)

// Retention 是三个粒度的保留窗口（PLAN.md 第 18 节）。
type Retention struct {
	Raw    time.Duration
	Minute time.Duration
	Hour   time.Duration
}

// DefaultRetention 返回 PLAN.md 第 18 节的默认保留窗口：24 小时 / 30 天 / 365 天。
func DefaultRetention() Retention {
	return Retention{
		Raw:    24 * time.Hour,
		Minute: 30 * 24 * time.Hour,
		Hour:   365 * 24 * time.Hour,
	}
}

// Window 返回某个粒度的保留窗口。未知粒度返回 0，调用方按“不裁剪”处理。
func (r Retention) Window(resolution string) time.Duration {
	switch resolution {
	case store.MetricResolutionRaw:
		return r.Raw
	case store.MetricResolutionMinute:
		return r.Minute
	case store.MetricResolutionHour:
		return r.Hour
	default:
		return 0
	}
}

// SummarySource 是系统快照的来源。生产实现是 *monitoring.Fetcher。
type SummarySource interface {
	Summary(ctx context.Context) (monitoring.Summary, error)
}

// TopologySource 是存储拓扑的来源。生产实现是 *storage.Fetcher。
type TopologySource interface {
	Topology(ctx context.Context) (storage.Topology, error)
}

// MetricStore 是持久化端口。生产实现是 *store.DB。
type MetricStore interface {
	InsertMetricSamples(ctx context.Context, samples []store.MetricSample) (int, error)
	AggregateMetricBucket(ctx context.Context, srcResolution, dstResolution string, bucket time.Time) (int, error)
	PendingMetricBuckets(ctx context.Context, srcResolution, dstResolution string, from, to time.Time, limit int) ([]time.Time, error)
	PruneMetricSamples(ctx context.Context, resolution string, before time.Time) (int64, error)
	MetricSampleCounts(ctx context.Context) (map[string]int64, error)
	DeleteOldestMetricBuckets(ctx context.Context, resolution string, buckets int) (int64, error)
	QueryMetricSeries(ctx context.Context, q store.MetricQuery) ([]store.MetricSeries, error)
	MetricSeriesCatalog(ctx context.Context) (map[string][]store.MetricSeriesRef, error)
}

// Options 是 Recorder 的构造参数。
type Options struct {
	Store    MetricStore
	Summary  SummarySource
	Topology TopologySource
	Logger   *slog.Logger

	// Interval 是轮询间隔，0 时用 DefaultInterval。
	Interval time.Duration
	// FilesystemInterval 是存储拓扑的轮询间隔，0 时用 DefaultFilesystemInterval。
	FilesystemInterval time.Duration
	// Retention 是保留窗口，零值时用 DefaultRetention。
	Retention Retention
	// MaxRows 是 metric_samples 的行数上限，0 时用 DefaultMaxRows。
	MaxRows int64
	// Now 是可注入的时钟，测试用来模拟时间推进；nil 时用 time.Now。
	Now func() time.Time
}

// DefaultMaxRows 是 metric_samples 的默认行数上限（约两天半的原始样本 + 全部聚合）。
const DefaultMaxRows int64 = 2_000_000

// Recorder 是历史样本的写入与维护循环。
//
// 它不是一个“后台任务的调度器”：没有队列、没有重试表、没有跨进程的协调。
// 每个周期独立完成，上一周期失败不影响下一周期——历史数据可以补，进程不能被拖住。
type Recorder struct {
	opts Options
	now  func() time.Time

	// lastSummaryAt 是上一次写入的主机样本时间。同一份代理快照（collected_at 相同）
	// 只写一次；库层也有主键兜底，这里只是省掉一次无意义的写事务。
	lastSummaryAt time.Time
	// lastFSAt 是上一次拉存储拓扑的时间（拓扑的轮询间隔比系统快照慢）。
	lastFSAt time.Time

	lastPrune    time.Time
	lastRowLimit time.Time
	lastLogByKey map[string]time.Time

	lastSampleAt   time.Time
	lastError      string
	lastErrorAt    time.Time
	lastInserted   int64
	lastAggregated int64
	lastPruned     int64
}

// NewRecorder 构造 Recorder。
//
// Store 与 Summary 必须非空：缺了任何一个都构不出“能工作的循环”，与其启动后每 2 秒
// 报一次错，不如在装配阶段就失败。Topology 可以为 nil——那样只有存储曲线缺失，
// 主机与网卡曲线照常，日志里会说明为什么（不猜、不补）。
func NewRecorder(opts Options) (*Recorder, error) {
	// 接口里塞一个具体类型的 nil 指针，接口本身不是 nil：那样这里的检查会放过它，
	// 采集循环到第一轮解引用时 panic，把整个 Web 进程带走。采集循环跑在后台，
	// 没人接得住这个 panic，所以在这里先把“长得像 nil 的 nil”拍平。
	opts.Summary = nilIfNil(opts.Summary)
	opts.Topology = nilIfNil(opts.Topology)
	if opts.Store == nil {
		return nil, errors.New("metrics: 缺少样本存储")
	}
	if opts.Summary == nil {
		return nil, errors.New("metrics: 缺少系统快照来源")
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Interval <= 0 {
		opts.Interval = DefaultInterval
	}
	if opts.FilesystemInterval <= 0 {
		opts.FilesystemInterval = DefaultFilesystemInterval
	}
	if opts.Retention == (Retention{}) {
		opts.Retention = DefaultRetention()
	}
	if opts.MaxRows <= 0 {
		opts.MaxRows = DefaultMaxRows
	}
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	r := &Recorder{opts: opts, now: now, lastLogByKey: map[string]time.Time{}}
	if opts.Topology == nil {
		opts.Logger.Warn("历史监控没有存储容量来源，存储曲线不会被记录")
	}
	return r, nil
}

// retention 是本进程实际使用的保留窗口。查询端拿它决定读哪一层：
// 保留窗口被配短了，查询就不能还去 raw 里找三天前的点。
func (r *Recorder) retention() Retention { return r.opts.Retention }

// Run 启动循环，直到 ctx 结束。
//
// 第一轮立刻执行（不等到第一个 tick）：重启之后最需要的是尽快把落下的桶补上、
// 把过期的数据裁掉，然后再开始按节奏走。
func (r *Recorder) Run(ctx context.Context) {
	r.opts.Logger.Info("历史监控开始采集",
		"interval", r.opts.Interval.String(),
		"filesystem_interval", r.opts.FilesystemInterval.String(),
		"raw_retention", r.opts.Retention.Raw.String(),
		"minute_retention", r.opts.Retention.Minute.String(),
		"hour_retention", r.opts.Retention.Hour.String(),
		"max_rows", r.opts.MaxRows,
	)
	r.Pass(ctx)

	ticker := time.NewTicker(r.opts.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.Pass(ctx)
		}
	}
}

// Pass 执行一个完整周期。它会吞掉所有错误（记日志），因为调用方是循环本身；
// 需要断言结果的调用方（测试）请直接调用各个导出的单步方法。
func (r *Recorder) Pass(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	now := r.now().UTC()
	r.recordSummary(ctx)
	r.recordFilesystems(ctx, now)
	r.aggregate(ctx, now)
	r.prune(ctx, now)
	r.enforceRowLimit(ctx, now)
}

// recordSummary 采一份系统快照并写入主机与网卡样本。
func (r *Recorder) recordSummary(ctx context.Context) {
	sum, err := r.opts.Summary.Summary(ctx)
	if err != nil {
		r.noteError("summary", err)
		return
	}
	if sum.CollectedAt.IsZero() {
		r.noteError("summary", errors.New("快照没有 collected_at"))
		return
	}
	if !sum.CollectedAt.After(r.lastSummaryAt) {
		// 还是上一份快照。重复写入会被主键挡掉，但省掉一次事务更好；
		// 更重要的是别把 lastSampleAt 更新成“刚刚”。
		return
	}
	samples := DeriveHost(sum)
	samples = append(samples, DeriveNetwork(sum)...)
	if len(samples) == 0 {
		// 整份快照都是失败段：没有可写的值。这不是错误，但要让状态接口说得清
		// “采集在跑、只是这一份没有可用数据”。
		r.lastSummaryAt = sum.CollectedAt
		return
	}
	inserted, err := r.opts.Store.InsertMetricSamples(ctx, samples)
	if err != nil {
		r.noteError("insert", err)
		return
	}
	// 只有落库成功才算消费。写入失败时保留旧水位，让下一轮能重试同一份快照。
	r.lastSummaryAt = sum.CollectedAt
	if inserted > 0 {
		r.lastInserted += int64(inserted)
		r.lastSampleAt = sum.CollectedAt
	}
}

// recordFilesystems 按 FilesystemInterval 拉一次存储拓扑，写入容量样本。
func (r *Recorder) recordFilesystems(ctx context.Context, now time.Time) {
	if r.opts.Topology == nil {
		return
	}
	if !r.lastFSAt.IsZero() && now.Sub(r.lastFSAt) < r.opts.FilesystemInterval {
		return
	}
	r.lastFSAt = now

	top, err := r.opts.Topology.Topology(ctx)
	if err != nil {
		r.noteError("topology", err)
		return
	}
	samples := DeriveFilesystems(top)
	if len(samples) == 0 {
		return
	}
	inserted, err := r.opts.Store.InsertMetricSamples(ctx, samples)
	if err != nil {
		r.noteError("insert-fs", err)
		return
	}
	if inserted > 0 {
		r.lastInserted += int64(inserted)
	}
}

// aggregate 把完整的桶向上卷一层。
//
// 只卷**已经结束**的桶（上界是当前桶的起点）：把一个还在填的桶卷上去，会让同一个
// 分钟桶在小时桶里被写好几遍，而且每次的平均值都不一样——历史里留下的是最后一次
// 算出来的那个，与真实区间无关。
func (r *Recorder) aggregate(ctx context.Context, now time.Time) {
	r.aggregateOne(ctx, store.MetricResolutionRaw, store.MetricResolutionMinute, now, r.opts.Retention.Raw)
	r.aggregateOne(ctx, store.MetricResolutionMinute, store.MetricResolutionHour, now, r.opts.Retention.Minute)
}

func (r *Recorder) aggregateOne(ctx context.Context, src, dst string, now time.Time, window time.Duration) {
	if window <= 0 {
		return
	}
	width := store.MetricBucketSeconds(dst)
	if width <= 0 {
		return
	}
	// 上界不含当前这个未完成的桶。
	to := time.Unix(alignDown(now.Unix(), width), 0).UTC()
	from := to.Add(-window).UTC()
	if !from.Before(to) {
		return
	}

	budget := aggregateBucketsPerPass
	for budget > 0 {
		limit := aggregateBatch
		if budget < limit {
			limit = budget
		}
		buckets, err := r.opts.Store.PendingMetricBuckets(ctx, src, dst, from, to, limit)
		if err != nil {
			r.noteError("pending-"+dst, err)
			return
		}
		if len(buckets) == 0 {
			return
		}
		for _, bucket := range buckets {
			if ctx.Err() != nil {
				return
			}
			n, err := r.opts.Store.AggregateMetricBucket(ctx, src, dst, bucket)
			if err != nil {
				r.noteError("aggregate-"+dst, err)
				return
			}
			r.lastAggregated += int64(n)
		}
		budget -= len(buckets)
		if len(buckets) < limit {
			return
		}
	}
}

// prune 按保留窗口裁掉过期样本。
//
// 裁剪点比窗口再晚两个桶宽：聚合要用原始样本，先裁后卷会让最老的那个桶永远卷不上。
// 多留两个桶的代价是几十行数据，换来的是“保留期边界上不会凭空缺一段”。
func (r *Recorder) prune(ctx context.Context, now time.Time) {
	if !r.lastPrune.IsZero() && now.Sub(r.lastPrune) < pruneInterval {
		return
	}
	r.lastPrune = now

	for _, resolution := range []string{
		store.MetricResolutionRaw,
		store.MetricResolutionMinute,
		store.MetricResolutionHour,
	} {
		window := r.opts.Retention.Window(resolution)
		if window <= 0 {
			continue
		}
		width := time.Duration(store.MetricBucketSeconds(resolution)) * time.Second
		before := now.Add(-window).Add(-2 * width)
		deleted, err := r.opts.Store.PruneMetricSamples(ctx, resolution, before)
		if err != nil {
			r.noteError("prune-"+resolution, err)
			continue
		}
		if deleted > 0 {
			r.lastPruned += deleted
			r.opts.Logger.Debug("历史样本已按保留窗口裁剪",
				"resolution", resolution, "deleted", deleted, "before", before.Format(time.RFC3339))
		}
	}
}

// enforceRowLimit 在总行数超过上限时丢掉最老的整桶。
//
// 顺序是 raw → minute → hour：先丢最细、最不值钱的那一层。丢完还超就继续往下丢，
// 但不会把整个库删空——每层最多丢一半（一次调用删最老的 500 个桶），剩下的留到下个
// 周期再判断，避免一次长时间持锁。
func (r *Recorder) enforceRowLimit(ctx context.Context, now time.Time) {
	if !r.lastRowLimit.IsZero() && now.Sub(r.lastRowLimit) < rowLimitInterval {
		return
	}
	r.lastRowLimit = now

	counts, err := r.opts.Store.MetricSampleCounts(ctx)
	if err != nil {
		r.noteError("count", err)
		return
	}
	total := counts["total"]
	if total <= r.opts.MaxRows {
		return
	}
	r.opts.Logger.Warn("历史样本超过行数上限，开始丢弃最老的数据",
		"total", total, "max_rows", r.opts.MaxRows,
		"raw", counts[store.MetricResolutionRaw],
		"minute", counts[store.MetricResolutionMinute],
		"hour", counts[store.MetricResolutionHour],
	)
	for _, resolution := range []string{
		store.MetricResolutionRaw,
		store.MetricResolutionMinute,
		store.MetricResolutionHour,
	} {
		deleted, err := r.opts.Store.DeleteOldestMetricBuckets(ctx, resolution, 500)
		if err != nil {
			r.noteError("row-limit-"+resolution, err)
			return
		}
		if deleted > 0 {
			r.lastPruned += deleted
			total -= deleted
			r.opts.Logger.Warn("已丢弃最老的历史样本",
				"resolution", resolution, "deleted", deleted, "remaining", total)
		}
		if total <= r.opts.MaxRows {
			return
		}
	}
}

// noteError 记录一次失败，同一类错误每分钟最多打一条。
func (r *Recorder) noteError(key string, err error) {
	r.lastError = err.Error()
	now := r.now()
	r.lastErrorAt = now.UTC()

	if last, ok := r.lastLogByKey[key]; ok && now.Sub(last) < logInterval {
		return
	}
	r.lastLogByKey[key] = now
	r.opts.Logger.Warn("历史监控采集或维护失败", "stage", key, "error", err)
}

// Stats 是状态接口要用的运行时事实。
//
// 它只说**本进程**知道的事：这个进程启动以来写了多少、裁了多少、最后一次成功拿到
// 快照是什么时候、最近一次错误是什么。库里的行数是查出来的，不是记出来的。
type Stats struct {
	IntervalSeconds      int64
	FilesystemInterval   int64
	RetentionRawSeconds  int64
	RetentionMinuteSecs  int64
	RetentionHourSeconds int64
	MaxRows              int64

	LastSampleAt *time.Time
	LastError    string
	LastErrorAt  *time.Time

	Inserted int64
	// Aggregated 是本进程写入的聚合行数。
	Aggregated int64
	// Pruned 是本进程删掉的行数（保留策略 + 行数上限）。
	Pruned int64
}

// Stats 返回运行时事实与库里的行数。
//
// 行数查询失败时返回错误而不是零值：状态接口必须能区分“没有数据”和“查不出来”。
func (r *Recorder) Stats(ctx context.Context) (Stats, map[string]int64, error) {
	s := Stats{
		IntervalSeconds:      int64(r.opts.Interval / time.Second),
		FilesystemInterval:   int64(r.opts.FilesystemInterval / time.Second),
		RetentionRawSeconds:  int64(r.opts.Retention.Raw / time.Second),
		RetentionMinuteSecs:  int64(r.opts.Retention.Minute / time.Second),
		RetentionHourSeconds: int64(r.opts.Retention.Hour / time.Second),
		MaxRows:              r.opts.MaxRows,
		LastError:            r.lastError,
		Inserted:             r.lastInserted,
		Aggregated:           r.lastAggregated,
		Pruned:               r.lastPruned,
	}
	if !r.lastSampleAt.IsZero() {
		t := r.lastSampleAt
		s.LastSampleAt = &t
	}
	if !r.lastErrorAt.IsZero() {
		t := r.lastErrorAt
		s.LastErrorAt = &t
	}
	counts, err := r.opts.Store.MetricSampleCounts(ctx)
	if err != nil {
		return s, nil, fmt.Errorf("metrics: 读取样本行数失败: %w", err)
	}
	return s, counts, nil
}

// alignDown 把 Unix 秒向下对齐到 width 的整数倍。
func alignDown(unix int64, width int64) int64 {
	if width <= 0 {
		return unix
	}
	m := unix % width
	if m < 0 {
		m += width
	}
	return unix - m
}

// nilIfNil 把“装着一个 nil 指针的接口值”还原成真正的 nil 接口。
//
// 只处理指针类型：其它类型（值类型、map、func）在这个包里不会被当接口传。
func nilIfNil[T any](v T) T {
	rv := reflect.ValueOf(v)
	if rv.Kind() == reflect.Ptr && rv.IsNil() {
		var zero T
		return zero
	}
	return v
}
