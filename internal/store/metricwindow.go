package store

import (
	"context"
	"fmt"
	"time"
)

// 比较方向（闭集）。告警规则里的 comparison / secondary_comparison 与这里的值一一对应，
// 存库前必须先过这个闭集：比较符号要拼进 SQL，只有白名单里的四个才允许出现。
const (
	CompareGT  = "gt"
	CompareGTE = "gte"
	CompareLT  = "lt"
	CompareLTE = "lte"
)

// IsKnownComparison 判断比较方向是否属于闭集。
func IsKnownComparison(v string) bool {
	switch v {
	case CompareGT, CompareGTE, CompareLT, CompareLTE:
		return true
	default:
		return false
	}
}

// MetricWindowQuery 描述“在原始样本上数一数”的一次统计。
//
// 这是给告警评估用的，和 QueryMetricSeries 的分工是：那个回答“过去怎么走的”，要的是
// 逐个点；这个只回答“这段时间里有多少个点在线外、多少个在线内、最极端的是多少”。
// 一个 5 分钟窗口是 60 个点，一天是 17280 个点——把点全拉回 Go 里再数，评估循环每
// 15 秒就要搬一遍，而 SUM(CASE ...) 在库里一次扫描就数完了。
type MetricWindowQuery struct {
	Scope    string
	TargetID string
	Metric   string
	// From 不含端点，To 含端点。
	//
	// 与 QueryMetricSeries 的 [From, To) 故意不同：评估窗口是“从现在往回数 duration 秒”，
	// 现在这一刻的桶必须算进去。两端对称地取半开区间会漏掉最新那个桶——而那正是
	// “此刻是否还超限”的唯一依据。
	From time.Time
	To   time.Time
	// Comparison / Threshold 是“告警线”。
	Comparison string
	Threshold  float64
	// RecoveryComparison / RecoveryThreshold 是“恢复线”，只用来数 Recovered。
	//
	// 恢复线必须显式给出，不能拿告警线取反：两条线之间那条带子里的样本既不算超限、
	// 也不算恢复——那正是 recovery_threshold 存在的意义（避免在阈值上下抖着刷屏）。
	// 留空（空字符串）表示这次不数恢复。
	RecoveryComparison string
	RecoveryThreshold  float64
}

// MetricWindowStat 是一次统计的结果。
type MetricWindowStat struct {
	// Samples 是窗口里的样本数（原始 5 秒桶的个数）。
	Samples int
	// Over 是满足告警线的样本数；Recovered 是满足恢复线的样本数。
	//
	// 三者关系：Samples >= Over + Recovered，差值是“落在两条线之间的样本”。
	Over      int
	Recovered int
	// Oldest / Newest 是窗口内最早与最晚的样本时间。
	//
	// 这两个字段是“数据够不够判断”的唯一依据：只看 Over == Samples 是不行的——
	// 采集停了以后窗口里一个样本都没有，Over 和 Samples 都是 0，0 == 0 会把
	// “没有数据”算成“全部超限”。调用方必须先看覆盖范围。
	Oldest time.Time
	Newest time.Time
	// Peak 是窗口内朝告警方向最极端的值：gt/gte 取最大值，lt/lte 取最小值。
	Peak float64
}

// Empty 表示窗口里一个样本都没有。
func (s MetricWindowStat) Empty() bool { return s.Samples == 0 }

// MetricWindowStats 统计一条序列在窗口内的样本分布。
//
// 只读 raw 粒度：告警要的是“这一刻到底有没有超限”，一分钟聚合里的平均值会把
// 30 秒的尖峰抹平。raw 保留 24 小时，因此 duration_seconds 的上限也必须是
// 一天以内——超过就没有原始点可数了，与其悄悄退到聚合值，不如在建规则时就拒绝。
func (db *DB) MetricWindowStats(ctx context.Context, q MetricWindowQuery) (MetricWindowStat, error) {
	var out MetricWindowStat
	if !IsKnownComparison(q.Comparison) {
		return out, fmt.Errorf("store: 未知的比较方向 %q", q.Comparison)
	}
	overExpr, overArgs, err := comparisonExpr("value", q.Comparison, q.Threshold)
	if err != nil {
		return out, err
	}
	// 最极端的值朝告警方向取：越高越糟看最大，越低越糟看最小。
	peakExpr := "MAX(value)"
	if q.Comparison == CompareLT || q.Comparison == CompareLTE {
		peakExpr = "MIN(value)"
	}

	args := []any{q.Scope, q.TargetID, q.Metric, q.From.Unix(), q.To.Unix()}
	recSelect := "0"
	if q.RecoveryComparison != "" {
		if !IsKnownComparison(q.RecoveryComparison) {
			return out, fmt.Errorf("store: 未知的恢复比较方向 %q", q.RecoveryComparison)
		}
		recExpr, recArgs, err := comparisonExpr("value", q.RecoveryComparison, q.RecoveryThreshold)
		if err != nil {
			return out, err
		}
		recSelect = "COALESCE(SUM(CASE WHEN " + recExpr + " THEN 1 ELSE 0 END), 0)"
		// 参数顺序必须与 SQL 里 ? 的出现顺序一致：告警线的参数在 SELECT 里先出现，
		// 恢复线的紧随其后，WHERE 的参数已经在 args 里垫在最后。
		overArgs = append(overArgs, recArgs...)
	}

	query := `
		SELECT COUNT(*),
		       COALESCE(SUM(CASE WHEN ` + overExpr + ` THEN 1 ELSE 0 END), 0),
		       ` + recSelect + `,
		       COALESCE(MIN(ts), 0), COALESCE(MAX(ts), 0),
		       COALESCE(` + peakExpr + `, 0)
		FROM metric_samples
		WHERE scope = ? AND target_id = ? AND metric = ? AND resolution = '` + MetricResolutionRaw + `'
		  AND ts > ? AND ts <= ?`

	// overArgs 是 SELECT 里的占位符，args 是 WHERE 里的；拼起来时 SELECT 在前。
	all := append(append([]any{}, overArgs...), args...)
	row := db.SQL().QueryRowContext(ctx, query, all...)
	var oldest, newest int64
	if err := row.Scan(&out.Samples, &out.Over, &out.Recovered, &oldest, &newest, &out.Peak); err != nil {
		return MetricWindowStat{}, fmt.Errorf("store: 统计指标窗口失败: %w", err)
	}
	if oldest > 0 {
		out.Oldest = time.Unix(oldest, 0).UTC()
	}
	if newest > 0 {
		out.Newest = time.Unix(newest, 0).UTC()
	}
	return out, nil
}

// comparisonExpr 把一个白名单里的比较方向变成 SQL 片段与它的参数。
//
// 值永远走参数绑定；只有比较符号按白名单拼进语句——符号不可能参数化，而这里
// 的四个候选项是写死的常量，调用方传进来的字符串只用于查表，不参与拼接。
func comparisonExpr(column, comparison string, threshold float64) (string, []any, error) {
	switch comparison {
	case CompareGT:
		return column + " > ?", []any{threshold}, nil
	case CompareGTE:
		return column + " >= ?", []any{threshold}, nil
	case CompareLT:
		return column + " < ?", []any{threshold}, nil
	case CompareLTE:
		return column + " <= ?", []any{threshold}, nil
	default:
		return "", nil, fmt.Errorf("store: 未知的比较方向 %q", comparison)
	}
}
