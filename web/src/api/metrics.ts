// 历史监控查询（P13.1 的接口，P13.4 的图表）。
//
// 与 api/openapi.yaml 的 MetricsResult / MetricsStatus 逐字对应（字段是 snake_case，
// 前端不另造驼峰视图模型）。
//
// 这个文件存在的唯一理由是**不让页面自己编历史**：
//
//   1) 缺失的时间桶**不在 points 里**，服务端也从不补零（PLAN §18、ALERT-01）。
//      所以画图时"没有点"必须画成断开，绝不能插一条直线或落到 0——那会把一次
//      采集中断画成"CPU 掉到底"。
//   2) `enabled=false` 表示这个进程根本没在记历史（演示模式），不是"过去没数据"。
//      两者的界面话术完全不同，所以这里不把它折成一个空数组。
//   3) `collecting=false` + `enabled=true` 是第三种情况：查得到旧数据、但不再写新
//      数据（采集被关掉或循环挂了）。它的判据是 `last_sample_at` 有多旧。
//
// 权限是 V（任何已登录用户）：历史曲线里没有秘密，它与首页的实时指标同级。

import { get, type ApiResult } from './client'

/** 序列层级闭集（CONTRACTS §14.1 metric_samples.scope）。 */
export type MetricScope = 'host' | 'interface' | 'filesystem'

/** 单位闭集（internal/metrics 的 Unit* 常量）。 */
export type MetricUnit = 'percent' | 'bytes_per_sec' | 'bytes' | 'count'

/** 采样粒度闭集。 */
export type MetricResolution = 'raw' | 'minute' | 'hour'

/** 一个桶。timestamp 是桶的**起点**（UTC）。 */
export interface MetricPoint {
  timestamp: string
  value: number
  /** 同一个桶里的最小值。只有 value 的话"这一分钟冲到 100% 又落回来"会消失。 */
  min: number
  max: number
  /** 参与聚合的样本数（raw 恒为 1）。它是"这个桶有多少数据"的证据。 */
  sample_count: number
}

export interface MetricSeries {
  scope: MetricScope
  target_id: string
  metric: string
  unit: MetricUnit
  points: MetricPoint[]
  /**
   * 该有、但没有的桶数。包含序列首尾的空档，所以 >0 不等于"中间断了一段"。
   * 它是**唯一**能让人看出曲线有洞的字段——缺失的桶不会出现在 points 里。
   */
  missing_points: number
}

export interface MetricsResult {
  /** false = 这个进程没在记历史（演示模式），此时 series 为空、reason 说明原因。 */
  enabled: boolean
  reason?: string | undefined
  scope?: MetricScope | undefined
  from?: string | undefined
  to?: string | undefined
  step_seconds?: number | undefined
  resolution?: MetricResolution | undefined
  /** 一次查询的点数上限，界面据此解释"为什么被加粗了"。 */
  max_points: number
  series: MetricSeries[]
  /** 本次结果里最早/最晚的点。都为 null = 这个区间真的一点数据都没有。 */
  observed_from: string | null
  observed_to: string | null
  generated_at: string
}

/** 采集循环的状态（GET /system/metrics/status）。 */
export interface MetricsStatus {
  /** 能回答历史问题（库在）。**不**表示正在记。 */
  enabled: boolean
  reason?: string | undefined
  /** 采集循环在这个进程里跑着。 */
  collecting: boolean
  interval_seconds: number
  filesystem_interval_seconds: number
  retention: {
    raw_seconds: number
    minute_seconds: number
    hour_seconds: number
  }
  max_rows: number
  /** 库里的行数（键：raw / minute / hour / total）。 */
  samples: Record<string, number>
  /** 这个进程最后一次成功写入主机样本的时间；重启后从零开始。 */
  last_sample_at: string | null
  last_error?: string | undefined
  last_error_at: string | null
  inserted: number
  aggregated: number
  pruned: number
}

export interface MetricsQuery {
  scope: MetricScope
  /** 空表示这个 scope 下全部登记过的指标。 */
  metrics?: readonly string[]
  /** 空表示这个 scope 下全部目标（网卡名 / 挂载点）。 */
  targetIds?: readonly string[]
  from?: string
  to?: string
  stepSeconds?: number
}

/** 默认看多久：一小时。raw 粒度保留 24 小时，一小时是"刚发生了什么"的常用窗口。 */
export const DEFAULT_METRICS_WINDOW_MS = 60 * 60 * 1000

/** 查询历史（GET /system/metrics）。 */
export async function fetchMetrics(query: MetricsQuery): Promise<ApiResult<MetricsResult>> {
  const params: Record<string, string | number | undefined> = { scope: query.scope }
  if (query.metrics && query.metrics.length > 0) params['metric'] = query.metrics.join(',')
  if (query.targetIds && query.targetIds.length > 0) params['target_id'] = query.targetIds.join(',')
  if (query.from) params['from'] = query.from
  if (query.to) params['to'] = query.to
  if (query.stepSeconds) params['step'] = query.stepSeconds
  return get<MetricsResult>('/system/metrics', { query: params })
}

/** 取采集循环状态（GET /system/metrics/status）。 */
export async function fetchMetricsStatus(): Promise<ApiResult<MetricsStatus>> {
  return get<MetricsStatus>('/system/metrics/status')
}

// ---------------------------------------------------------------- 展示口径

/** 纵轴标题与数值格式用的单位标签。 */
export const METRIC_UNIT_LABELS: Record<MetricUnit, string> = {
  percent: '%',
  bytes_per_sec: 'B/s',
  bytes: 'B',
  count: '',
}

/** 已登记的指标名 → 中文短名（PLAN §18 的几类曲线）。未知的名字原样显示。 */
export const METRIC_LABELS: Record<string, string> = {
  cpu_usage_percent: 'CPU 使用率',
  memory_used_percent: '内存使用率',
  load1: '1 分钟负载',
  load5: '5 分钟负载',
  load15: '15 分钟负载',
  rx_bytes_per_sec: '接收速率',
  tx_bytes_per_sec: '发送速率',
  used_percent: '空间使用率',
  inode_used_percent: 'inode 使用率',
  available_bytes: '剩余空间',
}

/**
 * 一条序列在界面上的名字。
 *
 * host 序列没有 target；interface / filesystem 必须带上目标名，否则三块盘的三条
 * `used_percent` 会在列表里看起来一模一样。
 */
export function metricSeriesLabel(series: Pick<MetricSeries, 'scope' | 'target_id' | 'metric'>): string {
  const name = METRIC_LABELS[series.metric] ?? series.metric
  if (series.scope === 'host' || series.target_id === '') return name
  return `${name} · ${series.target_id}`
}

/** 字节数与速率的人读形式（十进制，与 CONTRACTS 的 bytes 口径一致）。 */
export function formatMetricValue(value: number, unit: MetricUnit): string {
  switch (unit) {
    case 'percent':
      return `${value.toFixed(1)}%`
    case 'bytes':
    case 'bytes_per_sec': {
      const suffix = unit === 'bytes' ? 'B' : 'B/s'
      const units = ['', 'K', 'M', 'G', 'T', 'P']
      let scaled = value
      let index = 0
      while (scaled >= 1000 && index < units.length - 1) {
        scaled /= 1000
        index += 1
      }
      const text = index === 0 ? scaled.toFixed(0) : scaled.toFixed(1)
      return `${text}${units[index]}${suffix}`
    }
    default:
      return value.toFixed(2)
  }
}

/**
 * 采集是否陈旧，以及怎么讲这件事。
 *
 * 三种情况分开说，因为它们对运维的含义完全不同：
 *
 *   - 未开启（enabled=false）：这个进程不记历史。不是故障，是配置或模式。
 *   - 不再采集（collecting=false）：查得到旧数据，但没有新数据进来。
 *   - 陈旧（collecting=true 但太久没有新样本）：循环还在，可是写不进去——
 *     这时 reason/last_error 才是要看的。
 *
 * staleAfterSeconds 用两条采集节奏+宽限来判：主机 5 秒一行、容量 30 秒一行，
 * 拿 5 秒去判容量会把正常状态报成陈旧。
 */
export interface StalenessVerdict {
  kind: 'ok' | 'disabled' | 'idle' | 'stale'
  message: string
  /** 距最后一个样本多少秒；没有样本或没开时是 null。 */
  ageSeconds: number | null
}

export function judgeStaleness(
  status: Pick<
    MetricsStatus,
    'enabled' | 'collecting' | 'interval_seconds' | 'last_sample_at' | 'reason' | 'last_error'
  >,
  now: Date = new Date(),
): StalenessVerdict {
  if (!status.enabled) {
    return { kind: 'disabled', message: status.reason ?? '历史监控未启用', ageSeconds: null }
  }
  const last = status.last_sample_at
  if (!last) {
    // 没有时间戳有两种来源：进程刚起来还没写第一行，或者从来没写成功。
    // 不管哪种，界面都不能说"一切正常"。
    if (!status.collecting) {
      return {
        kind: 'idle',
        message: status.reason ?? '采集循环没有在跑：查得到旧数据，但不会有新数据',
        ageSeconds: null,
      }
    }
    return { kind: 'stale', message: '还没有写入过任何样本', ageSeconds: null }
  }
  const ageMs = now.getTime() - new Date(last).getTime()
  const ageSeconds = Math.max(0, Math.round(ageMs / 1000))
  const interval = status.interval_seconds > 0 ? status.interval_seconds : 5
  // 宽限取 3 个周期或 20 秒（取大者）：一次 GC 停顿或一次订阅抖动不该被报成陈旧。
  const limit = Math.max(3 * interval, 20)
  if (!status.collecting) {
    return {
      kind: 'idle',
      message: status.reason ?? '采集循环没有在跑：查得到旧数据，但不会有新数据',
      ageSeconds,
    }
  }
  if (ageSeconds > limit) {
    const detail = status.last_error ? `最近一次失败：${status.last_error}` : '采集循环没有报错，但样本没有进来'
    return {
      kind: 'stale',
      message: `已经 ${ageSeconds} 秒没有新样本（判据 ${limit} 秒）：${detail}`,
      ageSeconds,
    }
  }
  return { kind: 'ok', message: '采集正常', ageSeconds }
}

/**
 * 把一条序列切成若干段**连续**的部分，供画线使用。
 *
 * 为什么必须切：服务端不补零，缺失的桶根本不在 points 里。如果把点按顺序连起来，
 * 一次十分钟的采集中断会画成一条跨过缺口的直线——图上看是"CPU 平稳"，真相是
 * "这十分钟我们什么都不知道"。断开才是诚实的画法。
 *
 * 判据用桶宽（stepSeconds）而不是"任意两个点之间的实际间隔"：采集节奏本身有抖动，
 * 相差 1—2 个桶是正常的；连续缺 3 个桶才算断。
 */
export const GAP_BUCKETS = 3

export function splitSeriesSegments(
  points: readonly MetricPoint[],
  stepSeconds: number,
): MetricPoint[][] {
  if (points.length === 0) return []
  const step = stepSeconds > 0 ? stepSeconds : 0
  const segments: MetricPoint[][] = []
  let current: MetricPoint[] = [points[0]!]
  for (let i = 1; i < points.length; i += 1) {
    const prev = points[i - 1]!
    const next = points[i]!
    const deltaSeconds = (new Date(next.timestamp).getTime() - new Date(prev.timestamp).getTime()) / 1000
    const broken = step > 0 ? deltaSeconds > step * GAP_BUCKETS : deltaSeconds > 0
    if (broken) {
      segments.push(current)
      current = [next]
    } else {
      current.push(next)
    }
  }
  segments.push(current)
  return segments
}

/** 一段序列的极值（画纵轴用）。空序列返回 null，绝不返回 0 上下界。 */
export function seriesBounds(series: readonly MetricSeries[]): { min: number; max: number } | null {
  let min = Number.POSITIVE_INFINITY
  let max = Number.NEGATIVE_INFINITY
  for (const s of series) {
    for (const p of s.points) {
      if (p.min < min) min = p.min
      if (p.max > max) max = p.max
    }
  }
  if (!Number.isFinite(min) || !Number.isFinite(max)) return null
  if (min === max) {
    // 一条恒定的曲线（负载一直是 0）会让折线贴在边框上；给它一点高度。
    return { min: Math.max(0, min - 1), max: max + 1 }
  }
  return { min, max }
}

/** 一个图表块：一条序列 + 它的切段、极值与缺失计数。 */
export interface ChartBlock {
  /** 稳定键（scope/target/metric），用作 v-for 的 key。 */
  key: string
  label: string
  unit: MetricUnit
  /** 已按缺口切好的连续段。段与段之间是数据缺失，图上不连。 */
  segments: MetricPoint[][]
  bounds: { min: number; max: number } | null
  /** 该有、但没有的桶数（0 表示这条曲线是完整的）。 */
  missing: number
  /** 点的个数；0 表示这段区间里一个样本都没有。 */
  pointCount: number
}

export interface ChartSet {
  fromMs: number
  toMs: number
  blocks: ChartBlock[]
}

/**
 * 把一次查询的结果变成可以直接画的块。
 *
 * 三件事在这里做完，页面只管画：
 *   - 按缺口切段（splitSeriesSegments）；
 *   - 算这条序列自己的上下界（不是所有序列共用一个：CPU 0—100 与负载 0.1—3
 *     共用一个纵轴的话，负载那条会贴成一条直线）；
 *   - 把 missing_points 原样带出来——它是"这条曲线有洞"的唯一证据。
 */
export function buildChartBlocks(result: MetricsResult | null): ChartSet {
  if (!result || !result.enabled) {
    return { fromMs: 0, toMs: 0, blocks: [] }
  }
  const fromMs = result.from ? new Date(result.from).getTime() : 0
  const toMs = result.to ? new Date(result.to).getTime() : 0
  const step = result.step_seconds ?? 0
  const blocks: ChartBlock[] = result.series.map((series) => ({
    key: `${series.scope}/${series.target_id}/${series.metric}`,
    label: metricSeriesLabel(series),
    unit: series.unit,
    segments: splitSeriesSegments(series.points, step),
    bounds: seriesBounds([series]),
    missing: series.missing_points,
    pointCount: series.points.length,
  }))
  return { fromMs, toMs, blocks }
}

