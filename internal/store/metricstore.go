package store

// P13.1：历史监控样本的持久化（CONTRACTS.md §14.1 metric_samples、PLAN.md 第 18 节）。
//
// 这一层只做四件事：批量写入、把一个粒度卷成下一个粒度、按时间裁剪、按区间读。
// 判断（哪个样本该不该写、保留多久、请求的点数上限）都在 internal/metrics 里；
// 库层不隐藏“删了多少行”“缺了哪些桶”这类事实，调用方要能据此写日志与回答 API。
//
// 三条口径：
//
//  1. **幂等**。写入是 INSERT OR IGNORE（同一序列同一秒只留一行），聚合是
//     INSERT OR REPLACE（重算同一个桶得到同一份结果）。进程重启后补算历史不会产生
//     第二份数据，也不会因为“上次算到一半”留下半个桶。
//  2. **缺失就是没有行**。这里没有任何“补零”或“填平均值”的路径：某个序列那段时间
//     没采到，就是查不出来。CONTRACTS.md 与 PLAN.md 第 18 节要求丢失的数据不能当零。
//  3. **时间一律 Unix 秒**。对齐到桶边界由调用方与 SQL 里的整除共同保证。

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// 采样粒度（闭集，与迁移里的 CHECK 一致）。
const (
	// MetricResolutionRaw 是原始采样，5 秒一个点，保留 24 小时。
	MetricResolutionRaw = "raw"
	// MetricResolutionMinute 是 1 分钟聚合，保留 30 天。
	MetricResolutionMinute = "minute"
	// MetricResolutionHour 是 1 小时聚合，保留 365 天。
	MetricResolutionHour = "hour"
)

// 序列层级（闭集，与迁移里的 CHECK 一致）。
const (
	MetricScopeHost       = "host"
	MetricScopeInterface  = "interface"
	MetricScopeFilesystem = "filesystem"
)

// MetricBucketSeconds 返回一个粒度的桶宽（秒）。
func MetricBucketSeconds(resolution string) int64 {
	switch resolution {
	case MetricResolutionRaw:
		return 5
	case MetricResolutionMinute:
		return 60
	case MetricResolutionHour:
		return 3600
	default:
		return 0
	}
}

// MetricSample 是一个待写入的样本行。
type MetricSample struct {
	Scope      string
	TargetID   string
	Metric     string
	Resolution string
	// TS 是样本时间。写入前会被对齐到 Resolution 的桶边界。
	TS          time.Time
	Value       float64
	MinValue    float64
	MaxValue    float64
	SampleCount int
}

// MetricPoint 是查询结果里的一个点。
type MetricPoint struct {
	TS          time.Time
	Value       float64
	MinValue    float64
	MaxValue    float64
	SampleCount int
}

// MetricSeries 是一条序列在请求区间内的点。
type MetricSeries struct {
	Scope    string
	TargetID string
	Metric   string
	Points   []MetricPoint
}

// MetricQuery 是一次历史查询。
type MetricQuery struct {
	Scope string
	// TargetIDs 为空表示“这个 scope 下全部目标”。
	TargetIDs []string
	// Metrics 为空表示“这个 scope 下全部指标”。
	Metrics []string
	// From 含端点，To 不含端点。区间由调用方先校验过（跨度、点数是 API 层的判断）。
	From time.Time
	To   time.Time
	// Resolution 是实际读取的粒度；Step 是再采样步长（秒），必须 ≥ 粒度的桶宽。
	Resolution string
	Step       int64
}

// InsertMetricSamples 批量写入样本，返回真正新增的行数。
//
// 一批一个事务：这批样本来自同一个采集时刻，半个批次落地只会让历史里出现
// 一段“有的序列有、有的没有”的假缺口。重复的（序列 + 粒度 + 时间）由主键挡掉，
// 因此同一份代理快照被写两次不会变成两行，计数也只加一次。
func (db *DB) InsertMetricSamples(ctx context.Context, samples []MetricSample) (int, error) {
	if len(samples) == 0 {
		return 0, nil
	}
	inserted := 0
	err := db.WithTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		stmt, err := tx.PrepareContext(ctx, `
			INSERT OR IGNORE INTO metric_samples
			    (scope, target_id, metric, resolution, ts, value, min_value, max_value, sample_count)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`)
		if err != nil {
			return fmt.Errorf("store: 准备样本写入失败: %w", err)
		}
		defer func() { _ = stmt.Close() }()

		for _, s := range samples {
			width := MetricBucketSeconds(s.Resolution)
			if width <= 0 {
				return fmt.Errorf("store: 未知的采样粒度 %q", s.Resolution)
			}
			count := s.SampleCount
			if count <= 0 {
				count = 1
			}
			res, err := stmt.ExecContext(ctx,
				s.Scope, s.TargetID, s.Metric, s.Resolution,
				alignUnix(s.TS, width), s.Value, s.MinValue, s.MaxValue, count)
			if err != nil {
				return fmt.Errorf("store: 写入样本失败（%s/%s/%s）: %w", s.Scope, s.TargetID, s.Metric, err)
			}
			n, err := res.RowsAffected()
			if err != nil {
				return fmt.Errorf("store: 读取样本写入结果失败: %w", err)
			}
			inserted += int(n)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return inserted, nil
}

// AggregateMetricBucket 把一个源粒度的桶卷成目标粒度的一个桶。
//
// 只处理**已经完整**的桶：调用方负责只在桶的右边界过去之后调用它。重算是安全的
// （INSERT OR REPLACE），因此“上次算完但没来得及写”与“这次又算一遍”不会有区别。
//
// 平均值按 sample_count 加权：一分钟里只有 3 个样本和整整 12 个样本不该在小时桶里
// 等权——那会让停机时段的少量样本放大成整小时的结论。
//
// 加权平均要把结果夹回 [MIN(min_value), MAX(max_value)]。真机上这条不是理论洁癖：
// 一小时的 inode_used_percent 三个样本全是 1.9 时，SQLite 算出的是
// SUM(1.9*12)/SUM(12) = 1.8999999999999997，比 MIN(min_value)=1.9 小一个 ulp，
// 直接撞上表上的 CHECK (min_value <= value AND value <= max_value)，整个小时桶
// 写不进去、每轮重试都失败，那一小时的所有指标就永久缺一段（2026-10-25 24.04 现场）。
// 一组数的加权平均在数学上必然落在它们的最小值与最大值之间，落到区间外的只可能是
// 浮点误差，所以夹回去是还原事实，不是放宽不变量——CHECK 原样保留。
func (db *DB) AggregateMetricBucket(ctx context.Context, srcResolution, dstResolution string, bucket time.Time) (int, error) {
	srcWidth := MetricBucketSeconds(srcResolution)
	dstWidth := MetricBucketSeconds(dstResolution)
	if srcWidth <= 0 || dstWidth <= 0 {
		return 0, fmt.Errorf("store: 未知的采样粒度 %q → %q", srcResolution, dstResolution)
	}
	if dstWidth%srcWidth != 0 {
		return 0, fmt.Errorf("store: 粒度 %q 不能由 %q 卷成（宽度不是整数倍）", dstResolution, srcResolution)
	}
	from := alignUnix(bucket, dstWidth)
	to := from + dstWidth

	var affected int
	err := db.WithTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		// 两参数的 max()/min() 在 SQLite 里是标量函数（聚合的那一支是单参数形式），
		// 所以这里写的是“标量夹取”，不是嵌套聚合。
		res, err := tx.ExecContext(ctx, `
			INSERT OR REPLACE INTO metric_samples
			    (scope, target_id, metric, resolution, ts, value, min_value, max_value, sample_count)
			SELECT scope, target_id, metric, ?, ?,
			       min(max(SUM(value * sample_count) / SUM(sample_count), MIN(min_value)), MAX(max_value)),
			       MIN(min_value), MAX(max_value), SUM(sample_count)
			FROM metric_samples
			WHERE resolution = ? AND ts >= ? AND ts < ?
			GROUP BY scope, target_id, metric`,
			dstResolution, from, srcResolution, from, to)
		if err != nil {
			return fmt.Errorf("store: 聚合 %s 桶 %d 失败: %w", dstResolution, from, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("store: 读取聚合结果失败: %w", err)
		}
		affected = int(n)
		return nil
	})
	if err != nil {
		return 0, err
	}
	return affected, nil
}

// PendingMetricBuckets 返回 [from, to) 里“源粒度有数据、目标粒度还没有行”的桶，
// 按时间升序，最多 limit 个。
//
// 判据是**桶里有没有任何目标行**，而不是“桶是否已经过了”。这样重启、崩溃、
// 时钟回拨之后都能自愈：缺的桶会被补算，算过的桶不会被再算一遍（虽然重算也安全）。
// limit 让启动时的补算可以分批进行，不至于拿着写锁算上一分钟。
func (db *DB) PendingMetricBuckets(ctx context.Context, srcResolution, dstResolution string, from, to time.Time, limit int) ([]time.Time, error) {
	srcWidth := MetricBucketSeconds(srcResolution)
	dstWidth := MetricBucketSeconds(dstResolution)
	if srcWidth <= 0 || dstWidth <= 0 {
		return nil, fmt.Errorf("store: 未知的采样粒度 %q → %q", srcResolution, dstResolution)
	}
	if limit <= 0 {
		limit = 500
	}
	fromUnix := alignUnix(from, dstWidth)
	toUnix := alignUnix(to, dstWidth)

	rows, err := db.SQL().QueryContext(ctx, `
		SELECT DISTINCT (ts / ?) * ? AS bucket
		FROM metric_samples AS src
		WHERE src.resolution = ? AND src.ts >= ? AND src.ts < ?
		  AND NOT EXISTS (
		      SELECT 1 FROM metric_samples AS dst
		      WHERE dst.resolution = ? AND dst.ts = (src.ts / ?) * ?)
		ORDER BY bucket
		LIMIT ?`,
		dstWidth, dstWidth, srcResolution, fromUnix, toUnix,
		dstResolution, dstWidth, dstWidth, limit)
	if err != nil {
		return nil, fmt.Errorf("store: 查询待聚合桶失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []time.Time
	for rows.Next() {
		var ts int64
		if err := rows.Scan(&ts); err != nil {
			return nil, fmt.Errorf("store: 解析待聚合桶失败: %w", err)
		}
		out = append(out, time.Unix(ts, 0).UTC())
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历待聚合桶失败: %w", err)
	}
	return out, nil
}

// PruneMetricSamples 删除某个粒度下早于 before 的样本，返回删除行数。
//
// 按时间整桶地删：一半的桶留着没有意义，而桶边界与保留窗口天然对齐（窗口都是
// 桶宽的整数倍），所以这里只做 ts < before 这一个判断。
func (db *DB) PruneMetricSamples(ctx context.Context, resolution string, before time.Time) (int64, error) {
	width := MetricBucketSeconds(resolution)
	if width <= 0 {
		return 0, fmt.Errorf("store: 未知的采样粒度 %q", resolution)
	}
	var deleted int64
	err := db.WithTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`DELETE FROM metric_samples WHERE resolution = ? AND ts < ?`,
			resolution, alignUnix(before, width))
		if err != nil {
			return fmt.Errorf("store: 裁剪 %s 样本失败: %w", resolution, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("store: 读取裁剪结果失败: %w", err)
		}
		deleted = n
		return nil
	})
	if err != nil {
		return 0, err
	}
	return deleted, nil
}

// MetricSampleCounts 返回各粒度的行数（键是粒度，另外 "total" 是合计）。
//
// 只用于行数上限的判断与日志，不参与任何产品结论。
func (db *DB) MetricSampleCounts(ctx context.Context) (map[string]int64, error) {
	rows, err := db.SQL().QueryContext(ctx,
		`SELECT resolution, COUNT(*) FROM metric_samples GROUP BY resolution`)
	if err != nil {
		return nil, fmt.Errorf("store: 统计样本行数失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := map[string]int64{"total": 0}
	for rows.Next() {
		var resolution string
		var n int64
		if err := rows.Scan(&resolution, &n); err != nil {
			return nil, fmt.Errorf("store: 解析样本行数失败: %w", err)
		}
		out[resolution] = n
		out["total"] += n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历样本行数失败: %w", err)
	}
	return out, nil
}

// DeleteOldestMetricBuckets 删除某个粒度下最老的 buckets 个时间桶，返回删除行数。
//
// 行数超过上限时用它兜底：保留策略按时间删不动的部分（例如保留窗口配得过长）
// 用“最老的先丢”来收口。丢的是整桶，不是随机几行——一条曲线中间被打洞比尾巴被削掉
// 更容易被误读。
//
// 最新那个桶永远不丢：上限被配得比一份快照还小的时候，这个函数会退化成
// “只保留最近一个桶”，而不是把整层删空。删空之后采集器再写、再删空，
// 曲线会变成永远只有一条竖线，那比超出上限更糟。
func (db *DB) DeleteOldestMetricBuckets(ctx context.Context, resolution string, buckets int) (int64, error) {
	if buckets <= 0 {
		return 0, nil
	}
	var deleted int64
	err := db.WithTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			DELETE FROM metric_samples
			WHERE resolution = ?
			  AND ts IN (
				SELECT ts FROM (
					SELECT DISTINCT ts FROM metric_samples
					WHERE resolution = ?
					  AND ts < (SELECT MAX(ts) FROM metric_samples WHERE resolution = ?)
					ORDER BY ts
					LIMIT ?
				)
			  )`,
			resolution, resolution, resolution, buckets)
		if err != nil {
			return fmt.Errorf("store: 按行数上限裁剪 %s 样本失败: %w", resolution, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("store: 读取行数上限裁剪结果失败: %w", err)
		}
		deleted = n
		return nil
	})
	if err != nil {
		return 0, err
	}
	return deleted, nil
}

// QueryMetricSeries 按序列返回区间内的点。
//
// Step 大于粒度桶宽时在 SQL 里再卷一次（加权平均 + min/max 取区间），而不是把原始点
// 拉回 Go 里自己合并：一小时的数据是 720 个点，一年是 8760 个桶，合并在库里做
// 少一次全量传输。
func (db *DB) QueryMetricSeries(ctx context.Context, q MetricQuery) ([]MetricSeries, error) {
	width := MetricBucketSeconds(q.Resolution)
	if width <= 0 {
		return nil, fmt.Errorf("store: 未知的采样粒度 %q", q.Resolution)
	}
	if q.Step <= 0 {
		q.Step = width
	}
	if q.Step < width {
		return nil, fmt.Errorf("store: 步长 %d 秒小于粒度 %q 的桶宽 %d 秒", q.Step, q.Resolution, width)
	}

	args := []any{q.Step, q.Step, q.Scope, q.Resolution, q.From.Unix(), q.To.Unix()}
	var where strings.Builder
	if len(q.TargetIDs) > 0 {
		where.WriteString(" AND target_id IN (")
		where.WriteString(placeholders(len(q.TargetIDs)))
		where.WriteString(")")
		for _, id := range q.TargetIDs {
			args = append(args, id)
		}
	}
	if len(q.Metrics) > 0 {
		where.WriteString(" AND metric IN (")
		where.WriteString(placeholders(len(q.Metrics)))
		where.WriteString(")")
		for _, m := range q.Metrics {
			args = append(args, m)
		}
	}

	// 这里的加权平均同样要夹回 [MIN(min_value), MAX(max_value)]，理由与
	// AggregateMetricBucket 里那条注释一样：值全等于 1.9 的桶，SQL 算出来是
	// 1.8999999999999997，不但落在区间外，还会被原样返回给页面画图。
	// 桶宽等于步长时这条 GROUP BY 只合并一行，也照样要夹：误差就出在这一行里。
	query := `
		SELECT target_id, metric, (ts / ?) * ? AS bucket,
		       min(max(SUM(value * sample_count) / SUM(sample_count), MIN(min_value)), MAX(max_value)),
		       MIN(min_value), MAX(max_value), SUM(sample_count)
		FROM metric_samples
		WHERE scope = ? AND resolution = ? AND ts >= ? AND ts < ?` + where.String() + `
		GROUP BY target_id, metric, bucket
		ORDER BY target_id, metric, bucket`

	rows, err := db.SQL().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: 查询历史样本失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []MetricSeries
	index := map[string]int{}
	for rows.Next() {
		var targetID, metric string
		var ts, count int64
		var value, minValue, maxValue float64
		if err := rows.Scan(&targetID, &metric, &ts, &value, &minValue, &maxValue, &count); err != nil {
			return nil, fmt.Errorf("store: 解析历史样本失败: %w", err)
		}
		key := targetID + "\x00" + metric
		i, ok := index[key]
		if !ok {
			out = append(out, MetricSeries{Scope: q.Scope, TargetID: targetID, Metric: metric})
			i = len(out) - 1
			index[key] = i
		}
		out[i].Points = append(out[i].Points, MetricPoint{
			TS:          time.Unix(ts, 0).UTC(),
			Value:       value,
			MinValue:    minValue,
			MaxValue:    maxValue,
			SampleCount: int(count),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历历史样本失败: %w", err)
	}
	return out, nil
}

// MetricSeriesCatalog 返回库里出现过的序列（scope + target_id + metric），按 scope 分组。
//
// 它回答的是“这台机器的历史里有哪些曲线”，供页面在没有传 metric 时列出默认项；
// 不是“产品支持哪些指标”——那是 internal/metrics 里的登记表。
func (db *DB) MetricSeriesCatalog(ctx context.Context) (map[string][]MetricSeriesRef, error) {
	rows, err := db.SQL().QueryContext(ctx, `
		SELECT DISTINCT scope, target_id, metric FROM metric_samples
		ORDER BY scope, target_id, metric`)
	if err != nil {
		return nil, fmt.Errorf("store: 查询历史序列目录失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := map[string][]MetricSeriesRef{}
	for rows.Next() {
		var ref MetricSeriesRef
		if err := rows.Scan(&ref.Scope, &ref.TargetID, &ref.Metric); err != nil {
			return nil, fmt.Errorf("store: 解析历史序列目录失败: %w", err)
		}
		out[ref.Scope] = append(out[ref.Scope], ref)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历历史序列目录失败: %w", err)
	}
	return out, nil
}

// MetricSeriesRef 是一条序列的标识。
type MetricSeriesRef struct {
	Scope    string `json:"scope"`
	TargetID string `json:"target_id"`
	Metric   string `json:"metric"`
}

// alignUnix 把时间对齐到 width 秒的桶边界（向下取整），返回 Unix 秒。
func alignUnix(t time.Time, width int64) int64 {
	return t.Unix() - mod(t.Unix(), width)
}

func mod(a, b int64) int64 {
	m := a % b
	if m < 0 {
		m += b
	}
	return m
}

// placeholders 生成 n 个逗号分隔的问号。列表长度来自调用方的闭集/上限校验，
// 值本身仍然走参数绑定。
func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}
