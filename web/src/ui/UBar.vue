<script setup lang="ts">
// 容量条：把“用了多少”画成一条，并且拒绝把“没读到”画成 0%。
//
// CONTRACTS 第 6 节：容量读不到时整块是 null，不是 0。所以这里 percent === null
// 时**一个填充元素都不渲染**——画一条 0% 的空槽，看图的人会当成“几乎没用”。
// 阈值沿用页面既有口径：≥90% 危险、≥75% 警告，其余强调色。

import { computed } from 'vue'

const props = withDefaults(
  defineProps<{
    /** 已用百分比；null 表示没读到，不是 0。 */
    percent: number | null
    /** auto 按阈值选色；显式指定时以调用方为准。 */
    tone?: 'auto' | 'accent' | 'warn' | 'bad'
    size?: 'thin' | 'normal'
    /** 读屏用：这条代表什么（“根分区已用”）。 */
    label?: string | undefined
  }>(),
  { tone: 'auto', size: 'normal', label: undefined },
)

const kind = computed<'accent' | 'warn' | 'bad'>(() => {
  if (props.tone !== 'auto') return props.tone
  const value = props.percent
  if (value === null) return 'accent'
  if (value >= 90) return 'bad'
  if (value >= 75) return 'warn'
  return 'accent'
})

const width = computed(() => {
  if (props.percent === null) return 0
  return Math.min(100, Math.max(0, props.percent))
})
</script>

<template>
  <div
    class="u-bar"
    :class="[`u-bar--${size}`]"
    :data-tone="percent === null ? 'unknown' : kind"
    role="img"
    :aria-label="label !== undefined ? `${label}：${percent === null ? '没有读数' : `${Math.round(width)}%`}` : undefined"
  >
    <span v-if="percent !== null" class="u-bar__fill" :style="{ width: `${width}%` }"></span>
  </div>
</template>

<style scoped>
.u-bar {
  position: relative;
  width: 100%;
  height: 6px;
  background: var(--surface-2);
  border: 1px solid var(--rule);
  border-radius: var(--r-1);
  overflow: hidden;
}

.u-bar--thin {
  height: 4px;
}

.u-bar__fill {
  display: block;
  height: 100%;
  background: var(--accent);
}

.u-bar[data-tone='warn'] .u-bar__fill {
  background: var(--warn);
}

.u-bar[data-tone='bad'] .u-bar__fill {
  background: var(--bad);
}

/* 没读到：虚线框，明确“这里没有数”，不给长度暗示。 */
.u-bar[data-tone='unknown'] {
  border-style: dashed;
  background: transparent;
}
</style>
