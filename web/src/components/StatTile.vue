<script setup lang="ts">
// 一个“指标 + 数值 + 说明”的小格子。
//
// 数值一律由调用方格式化后再传进来（utils/format 里已经处理了 null → —）。
// 这样组件不持有“怎么算”的逻辑，也就不会偷偷把 null 变成 0。
//
// 样式来自令牌：发丝边框、无阴影、数值用 tabular-nums（5 秒轮询刷新时列宽不跳）。

withDefaults(
  defineProps<{
    label: string
    value: string
    /** 补充说明：单位、口径、来源，或“为什么是 —”。 */
    hint?: string | undefined
    /** 强调级别，只影响视觉；文字本身已经说明了状态。 */
    tone?: 'default' | 'warning' | 'error'
  }>(),
  { hint: undefined, tone: 'default' },
)
</script>

<template>
  <div class="stat-tile" :data-tone="tone">
    <p class="stat-tile__label">{{ label }}</p>
    <p class="stat-tile__value">{{ value }}</p>
    <p v-if="hint" class="stat-tile__hint">{{ hint }}</p>
  </div>
</template>

<style scoped>
.stat-tile {
  display: flex;
  flex-direction: column;
  gap: 1px;
  min-width: 132px;
  padding: 8px 12px 9px;
  background: var(--surface);
  border: 1px solid var(--rule);
  border-radius: var(--r-2);
}

.stat-tile__label {
  margin: 0;
  font-size: var(--fs-xs);
  color: var(--ink-2);
}

.stat-tile__value {
  margin: 0;
  font-size: var(--fs-xl);
  font-weight: 500;
  line-height: 1.25;
  letter-spacing: -0.01em;
  color: var(--ink);
  /* 等宽数字但不换成等宽字体：值是“Ubuntu 24.04.5 LTS”“未启用”这类中英混排，
     整体换成 mono 会让中文掉到等宽回退字体，比不长不齐更难看。 */
  font-variant-numeric: tabular-nums;
  word-break: break-word;
}

.stat-tile__hint {
  margin: 0;
  font-size: var(--fs-xs);
  color: var(--ink-3);
}

.stat-tile[data-tone='warning'] .stat-tile__value {
  color: var(--warn);
}

.stat-tile[data-tone='error'] .stat-tile__value {
  color: var(--bad);
}
</style>
