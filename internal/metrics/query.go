package metrics

// P13.1：历史查询的参数校验与粒度选择（CONTRACTS.md 第 3 节 GET /system/metrics）。
//
// 契约只说了“明确 scope 的时序样本；限制时间范围/点数”，这里把“限制”变成可断言的规则：
//
//   - scope 必须给，且属于闭集。不给 scope 就是“全表扫”，那不是这个接口要做的事。
//   - 区间最多 365 天（正好是小时聚合的保留窗口），`to` 不能落在 2 分钟以后的未来
//     ——未来窗口永远为空，与其让人以为“这段时间没有数据”，不如直说参数写错了。
//   - 单次最多 1440 个点。点数超了不是报错，而是**加粗步长**：请求 24 小时的 5 秒
//     曲线会得到 1 分钟粒度的点。响应里如实写清用了哪个粒度、步长多少，界面据此
//     标注，而不是让人以为自己看到的是 5 秒数据。
//   - 未知指标名回 400。回 200 加空数组会让人以为“过去没有数据”，而真相是这个
//     指标根本不存在（见 series.go 的登记表）。

import (
	"context"
	"errors"
	"fmt"
	"time"

	"upanel/internal/store"
)

const (
	// MaxPoints 是单条序列一次返回的点数上限。
	MaxPoints = 1440
	// MaxRange 是一次查询的最大跨度（小时聚合的保留窗口）。
	MaxRange = 365 * 24 * time.Hour
	// DefaultWindow 是没给 from 时的默认回看窗口。
	DefaultWindow = time.Hour
	// MaxFutureSkew 是 to 允许超过当前时间的余量。
	MaxFutureSkew = 2 * time.Minute
	// MaxTargets 是一次查询里显式点名的目标数上限。
	MaxTargets = 64
	// MaxMetrics 是一次查询里显式点名的指标数上限。
	MaxMetrics = 16
	// MaxTargetIDLen 是目标标识的长度上限（挂载点路径可能很长，但不是无限的）。
	MaxTargetIDLen = 512
)

// InvalidQueryError 是参数校验失败。API 层据此回 400 而不是 500。
type InvalidQueryError struct {
	Message string
}

func (e *InvalidQueryError) Error() string { return e.Message }

// IsInvalidQuery 判断错误是不是参数问题。
func IsInvalidQuery(err error) bool {
	var target *InvalidQueryError
	return errors.As(err, &target)
}

func invalid(format string, args ...any) error {
	return &InvalidQueryError{Message: fmt.Sprintf(format, args...)}
}

// QueryParams 是一次历史查询的入参（HTTP 上的查询串已经解析成这些字段）。
type QueryParams struct {
	Scope string
	// TargetIDs 为空表示这个 scope 下的全部目标。
	TargetIDs []string
	// Metrics 为空表示这个 scope 下登记过的全部指标。
	Metrics []string
	// From / To 都是零值时取 to=now、from=to-DefaultWindow。
	From time.Time
	To   time.Time
	// StepSeconds 是期望的步长；0 表示自动（按区间挑一个不超过 MaxPoints 的密度）。
	StepSeconds int64
}

// Point 是响应里的一个点。
//
// 除了平均值还带上桶内的最小/最大值与样本数：只有平均值的话，“这一分钟里 CPU 冲到
// 100% 又落回来”在图上彻底消失。timestamp 是桶的起点，UTC。
type Point struct {
	Timestamp   time.Time `json:"timestamp"`
	Value       float64   `json:"value"`
	Min         float64   `json:"min"`
	Max         float64   `json:"max"`
	SampleCount int       `json:"sample_count"`
}

// Series 是一条序列在请求区间内的结果。
type Series struct {
	Scope    string  `json:"scope"`
	TargetID string  `json:"target_id"`
	Metric   string  `json:"metric"`
	Unit     string  `json:"unit"`
	Points   []Point `json:"points"`
	// MissingPoints 是“该有、但没有”的桶数。它包含序列开头/结尾没有数据的部分，
	// 因此它大于 0 不等于“中间断了一段”——但它是**唯一**能让人看出曲线有洞的字段。
	// 缺失的桶不会出现在 points 里，也永远不会是 0。
	MissingPoints int `json:"missing_points"`
}

// Result 是 GET /api/v1/system/metrics 的响应体。
type Result struct {
	// Enabled 为 false 表示这个进程没有在记历史（演示模式），此时 Series 为空且
	// Reason 说明原因。界面上要因此显示“未开启”而不是“没有数据”。
	Enabled bool   `json:"enabled"`
	Reason  string `json:"reason,omitempty"`

	Scope       string    `json:"scope,omitempty"`
	From        time.Time `json:"from,omitempty"`
	To          time.Time `json:"to,omitempty"`
	StepSeconds int64     `json:"step_seconds,omitempty"`
	// Resolution 是实际读的粒度：raw / minute / hour。
	Resolution string `json:"resolution,omitempty"`
	// MaxPoints 是这次查询的点数上限，界面可以据此解释“为什么被加粗了”。
	MaxPoints int      `json:"max_points"`
	Series    []Series `json:"series"`

	// ObservedFrom / ObservedTo 是本次结果里最早/最晚的点。两者都为 null 时，
	// 这个区间真的一点数据都没有——与“有数据但稀疏”是两件事。
	ObservedFrom *time.Time `json:"observed_from"`
	ObservedTo   *time.Time `json:"observed_to"`
	// GeneratedAt 是这次查询的服务端时间。
	GeneratedAt time.Time `json:"generated_at"`
}

// QueryReader 是查询端（生产实现是 *Reader）。
type QueryReader interface {
	Query(ctx QueryParams) (Result, error)
}

// Query 执行一次历史查询。
func (r *Reader) Query(ctx context.Context, params QueryParams) (Result, error) {
	now := r.now().UTC()
	plan, err := planQuery(params, now, r.coverage())
	if err != nil {
		return Result{}, err
	}

	raw, err := r.store.QueryMetricSeries(ctx, store.MetricQuery{
		Scope:      plan.scope,
		TargetIDs:  plan.targets,
		Metrics:    plan.metrics,
		From:       plan.from,
		To:         plan.to,
		Resolution: plan.resolution,
		Step:       plan.step,
	})
	if err != nil {
		return Result{}, err
	}

	expected := bucketCount(plan.from, plan.to, plan.step)
	out := Result{
		Enabled:     true,
		Scope:       plan.scope,
		From:        plan.from,
		To:          plan.to,
		StepSeconds: plan.step,
		Resolution:  plan.resolution,
		MaxPoints:   MaxPoints,
		Series:      make([]Series, 0, len(raw)),
		GeneratedAt: now,
	}
	for _, s := range raw {
		series := Series{
			Scope:    s.Scope,
			TargetID: s.TargetID,
			Metric:   s.Metric,
			Unit:     unitOf(s.Scope, s.Metric),
			Points:   make([]Point, 0, len(s.Points)),
		}
		for _, p := range s.Points {
			series.Points = append(series.Points, Point{
				Timestamp:   p.TS.UTC(),
				Value:       p.Value,
				Min:         p.MinValue,
				Max:         p.MaxValue,
				SampleCount: p.SampleCount,
			})
		}
		if missing := expected - len(series.Points); missing > 0 {
			series.MissingPoints = missing
		}
		if len(series.Points) > 0 {
			first := series.Points[0].Timestamp
			last := series.Points[len(series.Points)-1].Timestamp
			if out.ObservedFrom == nil || first.Before(*out.ObservedFrom) {
				out.ObservedFrom = &first
			}
			if out.ObservedTo == nil || last.After(*out.ObservedTo) {
				out.ObservedTo = &last
			}
		}
		out.Series = append(out.Series, series)
	}
	return out, nil
}

// queryPlan 是校验与粒度选择的结果。
type queryPlan struct {
	scope      string
	targets    []string
	metrics    []string
	from       time.Time
	to         time.Time
	resolution string
	step       int64
}

// planQuery 校验参数并选出粒度和步长。cov 是各层实际覆盖的回看长度。
func planQuery(params QueryParams, now time.Time, cov coverage) (queryPlan, error) {
	plan := queryPlan{scope: params.Scope}

	if !IsKnownScope(params.Scope) {
		return plan, invalid("scope 必须是 host、interface 或 filesystem 之一，当前为 %q", params.Scope)
	}
	if len(params.TargetIDs) > MaxTargets {
		return plan, invalid("一次最多指定 %d 个目标，当前 %d 个", MaxTargets, len(params.TargetIDs))
	}
	for _, id := range params.TargetIDs {
		if id == "" {
			return plan, invalid("target_id 不能为空字符串（要看全部目标就不要传这个参数）")
		}
		if len(id) > MaxTargetIDLen {
			return plan, invalid("target_id 太长（上限 %d 字节）", MaxTargetIDLen)
		}
	}
	plan.targets = params.TargetIDs

	if len(params.Metrics) > MaxMetrics {
		return plan, invalid("一次最多指定 %d 个指标，当前 %d 个", MaxMetrics, len(params.Metrics))
	}
	for _, m := range params.Metrics {
		if !IsKnownMetric(params.Scope, m) {
			return plan, invalid("指标 %q 不属于 scope %q；可用指标：%s", m, params.Scope, metricNames(params.Scope))
		}
	}
	plan.metrics = params.Metrics

	to := params.To.UTC()
	if to.IsZero() {
		to = now
	}
	from := params.From.UTC()
	if from.IsZero() {
		from = to.Add(-DefaultWindow)
	}
	if !from.Before(to) {
		return plan, invalid("from（%s）必须早于 to（%s）",
			from.Format(time.RFC3339), to.Format(time.RFC3339))
	}
	if to.Sub(from) > MaxRange {
		return plan, invalid("时间跨度最多 %d 天，当前请求 %.1f 天",
			int(MaxRange/(24*time.Hour)), to.Sub(from).Hours()/24)
	}
	if to.After(now.Add(MaxFutureSkew)) {
		return plan, invalid("to（%s）落在未来：未来窗口永远是空的，请检查时间参数",
			to.Format(time.RFC3339))
	}
	plan.from, plan.to = from, to

	step, resolution := chooseStep(from, to, params.StepSeconds, cov)
	plan.step, plan.resolution = step, resolution
	return plan, nil
}

// coverage 返回各层数据实际能覆盖多长的回看窗口。
//
// 没有采集器时（配置里关了采集、库是旧数据）用默认保留窗口：那时没法更准，
// 而猜得太细只会读到空的层。
func (r *Reader) coverage() coverage {
	if r.recorder != nil {
		ret := r.recorder.retention()
		return coverage{
			raw:    int64(ret.Raw / time.Second),
			minute: int64(ret.Minute / time.Second),
		}
	}
	return defaultCoverage()
}

// defaultCoverage 是保留窗口按默认值时的覆盖长度。
func defaultCoverage() coverage {
	ret := DefaultRetention()
	return coverage{
		raw:    int64(ret.Raw / time.Second),
		minute: int64(ret.Minute / time.Second),
	}
}

// coverage 是各粒度实际覆盖的回看长度（秒）。
type coverage struct {
	raw    int64
	minute int64
}

// resolutionForSpan 按跨度选**源粒度**。
//
// 粒度决定的不是“点画多细”，而是读库里哪一层。只按步长选的话，“最近三天、
// 每小时一个点”会挑到 raw——而 raw 只留 24 小时，于是明明有数据却回空曲线。
// 所以先按跨度落到一层确定覆盖得住的数据上，再把步长顶到时候需要的粗细。
func resolutionForSpan(span int64, cov coverage) (string, int64) {
	switch {
	case span <= cov.raw:
		return store.MetricResolutionRaw, 5
	case span <= cov.minute:
		return store.MetricResolutionMinute, 60
	default:
		return store.MetricResolutionHour, 3600
	}
}

// chooseStep 选出粒度与步长。
//
// 起点是“跨度覆盖得住的最细那层”，步长不小于该层的桶宽；点数超过 MaxPoints 时
// 把步长按该层桶宽的整数倍加粗。响应里如实写清用了哪层、步长多少，界面据此标注，
// 而不是让人以为自己看到的是 5 秒数据。
func chooseStep(from, to time.Time, requested int64, cov coverage) (int64, string) {
	span := to.Unix() - from.Unix()
	resolution, width := resolutionForSpan(span, cov)
	step := requested
	if step < width {
		step = width
	}
	if bucketCount(from, to, step) > MaxPoints {
		// 一次最多 MaxPoints 个点：步长顶到“跨度 / 点数”向上取整，再对齐到桶宽。
		step = (span + MaxPoints - 1) / MaxPoints
		step = ((step + width - 1) / width) * width
	}
	if step < width {
		step = width
	}
	return step, resolution
}

// bucketCount 返回 [from, to) 里按 step 对齐的桶数。
//
// 桶的起点是 step 的整数倍（与库里的 (ts/step)*step 一致），因此 from 不必自己
// 对齐——从 from 之后的第一个整数倍开始算。
func bucketCount(from, to time.Time, step int64) int {
	if step <= 0 {
		return 0
	}
	fromUnix, toUnix := from.Unix(), to.Unix()
	if toUnix <= fromUnix {
		return 0
	}
	first := fromUnix
	if rem := fromUnix % step; rem != 0 {
		if rem < 0 {
			first = fromUnix - rem - step
		} else {
			first = fromUnix - rem + step
		}
	}
	if first >= toUnix {
		return 0
	}
	last := toUnix - 1
	last -= last % step
	return int((last-first)/step) + 1
}

// metricNames 返回某个 scope 下的指标名，供错误信息列出可用值。
func metricNames(scope string) string {
	defs := DefinitionsFor(scope)
	if len(defs) == 0 {
		return "（没有）"
	}
	out := ""
	for i, d := range defs {
		if i > 0 {
			out += "、"
		}
		out += d.Metric
	}
	return out
}

// unitOf 返回指标的单位；未登记时返回空字符串（而不是猜一个）。
func unitOf(scope, metric string) string {
	for _, d := range definitions {
		if d.Scope == scope && d.Metric == metric {
			return d.Unit
		}
	}
	return ""
}
