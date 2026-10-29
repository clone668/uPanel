<script setup lang="ts">
// 一条历史曲线（P13.4）。
//
// 这个组件的全部意义是"**缺失的部分不画**"：服务端不补零，缺失的桶根本不在数据里
// （见 api/metrics.ts 的 splitSeriesSegments）。所以这里收到的不是一串点，而是
// **若干段**已经切好的连续点——每段画一条独立的折线，段与段之间什么都不画。
// 如果把它们连成一条，一次十分钟的采集中断会变成一条平滑直线，看起来像"一切正常"。
//
// 同时画桶内的最小/最大值（一条半透明的带子）：只有平均值的话，"这一分钟冲到
// 100% 又落回来"在图上会彻底消失，而那正是运维最需要看见的东西。

import { computed } from 'vue'

import { formatMetricValue, type MetricPoint, type MetricUnit } from '@/api/metrics'

const props = defineProps<{
  /** 已经按缺口切好的连续段（来自 splitSeriesSegments）。 */
  segments: MetricPoint[][]
  unit: MetricUnit
  /** 纵轴上下界；null 表示没有任何点，组件自己显示"没有数据"。 */
  bounds: { min: number; max: number } | null
  fromMs: number
  toMs: number
  height?: number
}>()

const WIDTH = 600
const PAD_LEFT = 56
const PAD_RIGHT = 8
const PAD_TOP = 8
const PAD_BOTTOM = 18

const height = computed<number>(() => props.height ?? 150)

const hasData = computed<boolean>(() => props.bounds !== null && props.segments.some((s) => s.length > 0))

/** 时间跨度；查询区间为零宽时（同一个 from/to）退化成 1，避免除零把点全挤到边上。 */
const spanMs = computed<number>(() => {
  const span = props.toMs - props.fromMs
  return span > 0 ? span : 1
})

function x(timestamp: string): number {
  const t = new Date(timestamp).getTime()
  const ratio = (t - props.fromMs) / spanMs.value
  const clamped = Math.min(1, Math.max(0, ratio))
  return PAD_LEFT + clamped * (WIDTH - PAD_LEFT - PAD_RIGHT)
}

function y(value: number): number {
  const bounds = props.bounds
  if (!bounds) return PAD_TOP
  const usable = height.value - PAD_TOP - PAD_BOTTOM
  const ratio = (bounds.max - value) / (bounds.max - bounds.min)
  return PAD_TOP + Math.min(1, Math.max(0, ratio)) * usable
}

/** 一段的折线。只有一个点时 polyline 什么都不画，所以单点单独画成小圆。 */
function linePath(points: readonly MetricPoint[]): string {
  return points.map((p, i) => `${i === 0 ? 'M' : 'L'}${x(p.timestamp).toFixed(2)},${y(p.value).toFixed(2)}`).join(' ')
}

/**
 * 一段的 min/max 带子：正向走 min，再倒着走 max，闭合。
 *
 * 带子的上下界来自**同一个桶**的 min/max，所以它表示的"这一格里的波动范围"，
 * 不是"相邻桶之间的连线"。
 */
function bandPath(points: readonly MetricPoint[]): string {
  if (points.length === 0) return ''
  const forward = points.map((p, i) => `${i === 0 ? 'M' : 'L'}${x(p.timestamp).toFixed(2)},${y(p.min).toFixed(2)}`)
  const backward = [...points].reverse().map((p) => `L${x(p.timestamp).toFixed(2)},${y(p.max).toFixed(2)}`)
  return `${forward.join(' ')} ${backward.join(' ')} Z`
}

/** 纵轴三档刻度（顶、中、底）。 */
const ticks = computed<{ y: number; label: string }[]>(() => {
  const bounds = props.bounds
  if (!bounds) return []
  const values = [bounds.max, (bounds.max + bounds.min) / 2, bounds.min]
  return values.map((v) => ({ y: y(v), label: formatMetricValue(v, props.unit) }))
})

/** 横轴只标首尾两个时刻：一格一个时间戳在 300px 宽的手机上会糊成一片。 */
const xLabels = computed<{ x: number; label: string }[]>(() => {
  const format = (ms: number): string => {
    const d = new Date(ms)
    if (Number.isNaN(d.getTime())) return ''
    const pad = (n: number): string => String(n).padStart(2, '0')
    return `${pad(d.getHours())}:${pad(d.getMinutes())}`
  }
  return [
    { x: PAD_LEFT, label: format(props.fromMs) },
    { x: WIDTH - PAD_RIGHT, label: format(props.toMs) },
  ]
})
</script>

<template>
  <div class="metric-chart">
    <div v-if="!hasData" class="metric-chart__empty" data-testid="metric-chart-empty">
      这段时间没有样本。缺失就是缺失，不画成 0。
    </div>
    <svg
      v-else
      class="metric-chart__svg"
      :viewBox="`0 0 ${WIDTH} ${height}`"
      preserveAspectRatio="none"
      role="img"
      data-testid="metric-chart"
    >
      <!-- 刻度线只画横线，不画竖线：竖线在窄屏上会和折线糊在一起。 -->
      <g class="metric-chart__grid">
        <line
          v-for="tick in ticks"
          :key="`grid-${String(tick.y)}`"
          :x1="PAD_LEFT"
          :x2="WIDTH - PAD_RIGHT"
          :y1="tick.y"
          :y2="tick.y"
        />
      </g>
      <g class="metric-chart__ticks">
        <text v-for="tick in ticks" :key="`label-${String(tick.y)}`" :x="PAD_LEFT - 6" :y="tick.y + 3" text-anchor="end">
          {{ tick.label }}
        </text>
        <text
          v-for="label in xLabels"
          :key="`x-${label.label}`"
          :x="label.x"
          :y="height - 4"
          text-anchor="middle"
        >
          {{ label.label }}
        </text>
      </g>
      <!-- 每一段独立画：段与段之间的空档就是数据缺口，这里什么都不画。 -->
      <g v-for="(segment, index) in segments" :key="`seg-${String(index)}`">
        <path v-if="segment.length > 1" class="metric-chart__band" :d="bandPath(segment)" />
        <path v-if="segment.length > 1" class="metric-chart__line" :d="linePath(segment)" />
        <!-- 孤立的一个点（两边都断了）画成小方块：它存在，但接不上任何东西。 -->
        <rect
          v-else-if="segment.length === 1 && segment[0]"
          class="metric-chart__dot"
          :x="x(segment[0].timestamp) - 1.5"
          :y="y(segment[0].value) - 1.5"
          width="3"
          height="3"
        />
      </g>
    </svg>
  </div>
</template>

<style scoped>
.metric-chart {
  width: 100%;
}

.metric-chart__svg {
  display: block;
  width: 100%;
  height: auto;
}

.metric-chart__empty {
  padding: 28px 12px;
  color: var(--ink-3);
  font-size: var(--fs-md);
  text-align: center;
  border: 1px dashed var(--ink-3);
  border-radius: var(--r-2);
}

.metric-chart__grid line {
  stroke: var(--rule);
  stroke-width: 1;
  vector-effect: non-scaling-stroke;
}

.metric-chart__ticks text {
  fill: var(--ink-3);
  font-size: 10px;
}

.metric-chart__band {
  fill: var(--accent-soft);
}

.metric-chart__line {
  fill: none;
  stroke: var(--accent);
  stroke-width: 1.5;
  stroke-linejoin: round;
  vector-effect: non-scaling-stroke;
}

.metric-chart__dot {
  fill: var(--accent);
}
</style>
