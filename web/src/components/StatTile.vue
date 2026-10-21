<script setup lang="ts">
// 一个“指标 + 数值 + 说明”的小卡片。
//
// 数值一律由调用方格式化后再传进来（utils/format 里已经处理了 null → —）。
// 这样组件不持有“怎么算”的逻辑，也就不会偷偷把 null 变成 0。

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
  gap: 2px;
  padding: 10px 12px;
  border: 1px solid rgba(128, 128, 128, 0.25);
  border-radius: 8px;
  min-width: 140px;
}

.stat-tile__label {
  margin: 0;
  font-size: 13px;
  opacity: 0.75;
}

.stat-tile__value {
  margin: 0;
  font-size: 20px;
  font-weight: 600;
  line-height: 1.3;
  word-break: break-word;
}

.stat-tile__hint {
  margin: 0;
  font-size: 12px;
  opacity: 0.75;
}

.stat-tile[data-tone='warning'] .stat-tile__value {
  color: #d08b00;
}

.stat-tile[data-tone='error'] .stat-tile__value {
  color: #d03050;
}
</style>
