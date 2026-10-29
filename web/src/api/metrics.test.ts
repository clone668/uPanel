// 历史监控的展示口径测试（P13.4）。
//
// 这些断言钉的都是"图上会不会撒谎"这件事：
//   - 缺失的桶一定落在**另一段**里（splitSeriesSegments），否则一次采集中断会画成直线；
//   - 空序列没有上下界（seriesBounds 返回 null），绝不给 0；
//   - 未开启历史时一块图都不出（buildChartBlocks），而不是出一排空轴；
//   - 陈旧判定分三种（未开启 / 没在采集 / 采集在跑但样本没进来），三句话不一样。

import { afterEach, describe, expect, it, vi } from 'vitest'

import {
  buildChartBlocks,
  fetchMetrics,
  formatMetricValue,
  GAP_BUCKETS,
  judgeStaleness,
  metricSeriesLabel,
  seriesBounds,
  splitSeriesSegments,
  type MetricPoint,
  type MetricsResult,
  type MetricSeries,
} from './metrics'

function point(timestamp: string, value: number, min = value, max = value): MetricPoint {
  return { timestamp, value, min, max, sample_count: 1 }
}

function series(overrides: Partial<MetricSeries> = {}): MetricSeries {
  return {
    scope: 'host',
    target_id: '',
    metric: 'cpu_usage_percent',
    unit: 'percent',
    points: [],
    missing_points: 0,
    ...overrides,
  }
}

function result(overrides: Partial<MetricsResult> = {}): MetricsResult {
  return {
    enabled: true,
    max_points: 600,
    series: [],
    observed_from: null,
    observed_to: null,
    generated_at: '2026-10-26T09:00:00Z',
    ...overrides,
  }
}

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('切段：缺失的地方一定断开', () => {
  it('连续的点留在一段里', () => {
    const points = [
      point('2026-10-26T09:00:00Z', 10),
      point('2026-10-26T09:00:05Z', 12),
      point('2026-10-26T09:00:10Z', 14),
    ]
    const segments = splitSeriesSegments(points, 5)
    expect(segments).toHaveLength(1)
    expect(segments[0]).toHaveLength(3)
  })

  it('缺了超过 GAP_BUCKETS 个桶就切成两段', () => {
    // 5 秒桶，从 09:00:15 跳到 09:00:45：中间空了 5 个桶 → 必须断开。
    const points = [
      point('2026-10-26T09:00:00Z', 10),
      point('2026-10-26T09:00:05Z', 12),
      point('2026-10-26T09:00:45Z', 90),
    ]
    const segments = splitSeriesSegments(points, 5)
    expect(segments).toHaveLength(2)
    expect(segments[0]).toHaveLength(2)
    expect(segments[1]).toHaveLength(1)
  })

  it('只差 1—2 个桶算抖动，不切段', () => {
    // 采集节奏本身会抖；把 10 秒的间隔当成断点会让曲线碎成一地。
    const points = [point('2026-10-26T09:00:00Z', 10), point('2026-10-26T09:00:10Z', 12)]
    expect(splitSeriesSegments(points, 5)).toHaveLength(1)
    expect(GAP_BUCKETS).toBe(3)
  })

  it('空序列切成零段（不是一段空线）', () => {
    expect(splitSeriesSegments([], 5)).toEqual([])
  })
})

describe('上下界', () => {
  it('一个点都没有时返回 null，而不是 0 上下界', () => {
    expect(seriesBounds([])).toBeNull()
    expect(seriesBounds([series()])).toBeNull()
  })

  it('恒定的曲线给一点高度，别贴在边框上', () => {
    const flat = series({ points: [point('2026-10-26T09:00:00Z', 0)] })
    const bounds = seriesBounds([flat])
    expect(bounds).not.toBeNull()
    expect(bounds!.max).toBeGreaterThan(bounds!.min)
  })

  it('用桶内 min/max 而不是只看平均值', () => {
    const wide = series({ points: [point('2026-10-26T09:00:00Z', 50, 10, 100)] })
    expect(seriesBounds([wide])).toEqual({ min: 10, max: 100 })
  })
})

describe('chart 块', () => {
  it('未开启历史时一块都不出', () => {
    const blocks = buildChartBlocks(result({ enabled: false, reason: '演示模式' }))
    expect(blocks.blocks).toEqual([])
    expect(blocks.fromMs).toBe(0)
  })

  it('null 输入同样给空结果（不发查询就没有图）', () => {
    expect(buildChartBlocks(null).blocks).toEqual([])
  })

  it('每条序列一块，缺失计数原样带出', () => {
    const blocks = buildChartBlocks(
      result({
        from: '2026-10-26T09:00:00Z',
        to: '2026-10-26T09:05:00Z',
        step_seconds: 5,
        series: [
          series({
            points: [point('2026-10-26T09:00:00Z', 10), point('2026-10-26T09:05:00Z', 20)],
            missing_points: 58,
          }),
        ],
      }),
    )
    expect(blocks.blocks).toHaveLength(1)
    expect(blocks.blocks[0]?.missing).toBe(58)
    expect(blocks.blocks[0]?.pointCount).toBe(2)
    // 中间缺了 59 个桶 → 两段。
    expect(blocks.blocks[0]?.segments).toHaveLength(2)
    expect(blocks.fromMs).toBe(Date.parse('2026-10-26T09:00:00Z'))
  })
})

describe('标签与数值', () => {
  it('非 host 序列带目标名，否则三块盘看起来一模一样', () => {
    expect(metricSeriesLabel(series())).toBe('CPU 使用率')
    expect(
      metricSeriesLabel({ scope: 'filesystem', target_id: '/srv', metric: 'used_percent' }),
    ).toBe('空间使用率 · /srv')
  })

  it('未知指标名原样显示，不被吞掉', () => {
    expect(metricSeriesLabel({ scope: 'host', target_id: '', metric: 'whatever' })).toBe('whatever')
  })

  it('百分比与字节各有各的写法', () => {
    expect(formatMetricValue(42.31, 'percent')).toBe('42.3%')
    expect(formatMetricValue(1536, 'bytes')).toBe('1.5KB')
    expect(formatMetricValue(2_000_000, 'bytes_per_sec')).toBe('2.0MB/s')
    expect(formatMetricValue(1.234, 'count')).toBe('1.23')
  })
})

describe('陈旧判定', () => {
  const now = new Date('2026-10-26T09:10:00Z')
  const base = {
    enabled: true,
    collecting: true,
    interval_seconds: 5,
    last_sample_at: '2026-10-26T09:09:50Z',
    reason: '',
    last_error: '',
  }

  it('刚写过样本就是正常', () => {
    expect(judgeStaleness(base, now).kind).toBe('ok')
  })

  it('不是故障：未开启历史时说清是未开启', () => {
    const verdict = judgeStaleness({ ...base, enabled: false, reason: '演示模式不采集历史' }, now)
    expect(verdict.kind).toBe('disabled')
    expect(verdict.message).toBe('演示模式不采集历史')
  })

  it('查得到旧数据但没在采集，与"陈旧"分开说', () => {
    const verdict = judgeStaleness(
      { ...base, collecting: false, last_sample_at: '2026-10-26T08:00:00Z', reason: '采集被关掉' },
      now,
    )
    expect(verdict.kind).toBe('idle')
    expect(verdict.message).toBe('采集被关掉')
    // 08:00 → 09:10 = 70 分钟 = 4200 秒：这是"最后一次样本有多旧"的实数。
    expect(verdict.ageSeconds).toBe(4200)
  })

  it('循环在跑但样本没进来 → 陈旧，并带上最近一次失败', () => {
    const verdict = judgeStaleness(
      { ...base, last_sample_at: '2026-10-26T09:00:00Z', last_error: '数据库已锁' },
      now,
    )
    expect(verdict.kind).toBe('stale')
    expect(verdict.message).toContain('数据库已锁')
    // 窗口是 max(3×周期, 20) = 20 秒；600 秒远超它。
    expect(verdict.ageSeconds).toBe(600)
  })

  it('循环在跑却从没写过样本，也不说"正常"', () => {
    const verdict = judgeStaleness({ ...base, last_sample_at: null }, now)
    expect(verdict.kind).toBe('stale')
    expect(verdict.ageSeconds).toBeNull()
  })
})

describe('查询参数', () => {
  function jsonResponse(body: unknown): Response {
    return new Response(JSON.stringify(body), {
      status: 200,
      headers: { 'Content-Type': 'application/json', 'X-Request-ID': 'rid-metrics' },
    })
  }

  it('多条指标用逗号拼在一个参数里，scope 一定带上', async () => {
    const mock = vi.fn(() => Promise.resolve(jsonResponse({ data: result(), request_id: 'rid-metrics' })))
    vi.stubGlobal('fetch', mock)

    await fetchMetrics({
      scope: 'host',
      metrics: ['cpu_usage_percent', 'memory_used_percent'],
      from: '2026-10-26T08:00:00Z',
      to: '2026-10-26T09:00:00Z',
    })

    const url = new URL(String((mock.mock.calls as unknown as [string][])[0]?.[0]), 'http://localhost')
    expect(url.pathname).toBe('/api/v1/system/metrics')
    expect(url.searchParams.get('scope')).toBe('host')
    expect(url.searchParams.get('metric')).toBe('cpu_usage_percent,memory_used_percent')
    expect(url.searchParams.get('from')).toBe('2026-10-26T08:00:00Z')
  })

  it('没给的条件不出现在查询串里', async () => {
    const mock = vi.fn(() => Promise.resolve(jsonResponse({ data: result(), request_id: 'rid-metrics' })))
    vi.stubGlobal('fetch', mock)

    await fetchMetrics({ scope: 'filesystem' })

    const url = new URL(String((mock.mock.calls as unknown as [string][])[0]?.[0]), 'http://localhost')
    expect(url.searchParams.get('scope')).toBe('filesystem')
    expect(url.searchParams.has('metric')).toBe(false)
    expect(url.searchParams.has('from')).toBe(false)
  })
})
