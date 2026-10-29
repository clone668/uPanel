// 历史曲线的渲染测试（P13.4）。
//
// 这里守的是同一件事的两面：
//   - 有缺口 → 画**两条**折线，而不是一条跨过缺口的线；
//   - 没有点 → 说"这段时间没有样本"，而不是画一条贴着 0 的线。
//
// 无 jsdom，用 vue/server-renderer 渲染成字符串断言模板分支（DECISIONS Q21）。

import { describe, expect, it } from 'vitest'
import { createSSRApp, h, type Component } from 'vue'
import { renderToString } from 'vue/server-renderer'

import { splitSeriesSegments, type MetricPoint } from '@/api/metrics'
import MetricChart from './MetricChart.vue'

function render(props: Record<string, unknown>): Promise<string> {
  // 与 panels.test.ts 同一写法：props 是 Record，只有把组件收窄成 Component 才通得过
  // 泛型约束（`h` 的最后一个重载要求 props 与 SFC 的 props 类型完全对齐）。
  return renderToString(createSSRApp({ render: () => h(MetricChart as Component, props) }))
}

function point(ms: number, value: number, min = value, max = value): MetricPoint {
  return { timestamp: new Date(ms).toISOString(), value, min, max, sample_count: 1 }
}

function count(html: string, needle: string): number {
  return html.split(needle).length - 1
}

/** 本地时间构造，避免时区把横轴标签推上一天。 */
const base = new Date(2026, 9, 26, 9, 0, 0, 0).getTime()
const minute = 60 * 1000

describe('MetricChart', () => {
  it('没有点时说明缺失，不画成 0', async () => {
    const html = await render({ segments: [], unit: 'percent', bounds: null, fromMs: base, toMs: base + minute })
    expect(html).toContain('data-testid="metric-chart-empty"')
    expect(html).toContain('这段时间没有样本。缺失就是缺失，不画成 0。')
    expect(html).not.toContain('data-testid="metric-chart"')
  })

  it('边界为 null 时有段也不画（段里没有可用的纵轴）', async () => {
    const segments = [[point(base, 10), point(base + minute, 12)]]
    const html = await render({ segments, unit: 'percent', bounds: null, fromMs: base, toMs: base + minute })
    expect(html).toContain('metric-chart-empty')
  })

  it('两段就画两条折线——缺口处断，不连成一条', async () => {
    // 09:00、09:01 连续；跳到 09:30 再来两个点：中间缺失，必须断开。
    const segments = splitSeriesSegments(
      [point(base, 10), point(base + minute, 12), point(base + 30 * minute, 40), point(base + 31 * minute, 42)],
      60,
    )
    expect(segments).toHaveLength(2)

    const html = await render({
      segments,
      unit: 'percent',
      bounds: { min: 0, max: 40 },
      fromMs: base,
      toMs: base + 30 * minute,
    })

    expect(html).toContain('data-testid="metric-chart"')
    expect(count(html, 'class="metric-chart__line"')).toBe(2)
    // min/max 带子同样分段画。
    expect(count(html, 'class="metric-chart__band"')).toBe(2)
  })

  it('孤立的一个点画成小方块，不画成空折线', async () => {
    const html = await render({
      segments: [[point(base, 55)]],
      unit: 'percent',
      bounds: { min: 0, max: 100 },
      fromMs: base,
      toMs: base + minute,
    })
    expect(count(html, 'class="metric-chart__dot"')).toBe(1)
    expect(html).not.toContain('class="metric-chart__line"')
  })

  it('纵轴三档刻度按单位写，横轴标首尾时刻', async () => {
    const html = await render({
      segments: [[point(base, 10), point(base + 30 * minute, 20)]],
      unit: 'percent',
      bounds: { min: 0, max: 100 },
      fromMs: base,
      toMs: base + 30 * minute,
    })
    expect(html).toContain('100.0%')
    expect(html).toContain('50.0%')
    expect(html).toContain('0.0%')
    expect(html).toContain('09:00')
    expect(html).toContain('09:30')
  })

  it('零宽区间不把点全挤到左边（跨度退化成 1ms 而不是除零）', async () => {
    const html = await render({
      segments: [[point(base, 10), point(base, 20)]],
      unit: 'count',
      bounds: { min: 0, max: 20 },
      fromMs: base,
      toMs: base,
    })
    // 两条 path 都应该是有限坐标，没有 NaN。
    expect(html).not.toContain('NaN')
    expect(html).toContain('class="metric-chart__line"')
  })
})
