<script setup lang="ts">
// 状态点 + 词：全站表达“此刻好不好”的唯一形状。
//
// 三条规矩：
//   1. 永远带文字。只有颜色的小圆点在色盲眼里等于没有，也不进读屏。
//   2. 未知（unknown）有自己的一格，不复用“正常”的绿，也不缩成灰色小字——
//      “没读到”和“读到了是好的”是两件事。
//   3. 方块而不圆点：圆形在密集表格里会被看成装饰，方块更像仪表上的指示位。

withDefaults(
  defineProps<{
    tone: 'good' | 'warn' | 'bad' | 'unknown'
  }>(),
  {},
)
</script>

<template>
  <span class="u-state" :data-tone="tone">
    <i class="u-state__dot" aria-hidden="true"></i>
    <span class="u-state__label"><slot /></span>
  </span>
</template>

<style scoped>
.u-state {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  font-size: var(--fs-sm);
  white-space: nowrap;
}

.u-state__dot {
  width: 7px;
  height: 7px;
  flex: none;
  border-radius: 1px;
  background: currentColor;
}

.u-state__label {
  color: var(--ink);
}

.u-state[data-tone='good'] {
  color: var(--good);
}

.u-state[data-tone='warn'] {
  color: var(--warn);
}

.u-state[data-tone='bad'] {
  color: var(--bad);
}

.u-state[data-tone='unknown'] {
  color: var(--unknown);
}

.u-state[data-tone='unknown'] .u-state__label {
  color: var(--ink-3);
}
</style>
