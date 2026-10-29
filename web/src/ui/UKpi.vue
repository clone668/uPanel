<script setup lang="ts">
// 指标格：一屏概览里四个数字，是这套界面最常被看的东西。
//
// 规矩：
//   - value 由调用方格式化好再传（utils/format 已把 null 变成 —）。组件不猜数。
//   - value 用等宽 + tabular-nums：5 秒轮询刷新时列宽不抖。
//   - foot 讲口径（采样窗口、可用量、5m/15m），不重复 value 已经说过的。
//   - 没有任何数值时也要能显示“没有读数”，因此 value 允许传 '—'。

withDefaults(
  defineProps<{
    label: string
    value: string
    /** 单位/分母，跟在数值后面用弱化字号（% 、/ 3.66 GiB）。 */
    unit?: string | undefined
    /** 数值的语气：只在真需要着色时用，正常读数不上色。 */
    tone?: 'default' | 'warn' | 'bad'
  }>(),
  { unit: undefined, tone: 'default' },
)
</script>

<template>
  <div class="u-kpi" :data-tone="tone">
    <p class="u-kpi__label">{{ label }}</p>
    <p class="u-kpi__value u-num">
      {{ value }}<small v-if="unit !== undefined" class="u-kpi__unit">{{ unit }}</small>
    </p>
    <div v-if="$slots.foot" class="u-kpi__foot">
      <slot name="foot" />
    </div>
    <!-- 容量条单独占一行、铺满格宽：它量的是这一格自己讲的那个数，
         贴着脚注缩在一角时看起来像一根走漏的短线。 -->
    <div v-if="$slots.extra" class="u-kpi__extra">
      <slot name="extra" />
    </div>
  </div>
</template>

<style scoped>
.u-kpi {
  display: flex;
  flex-direction: column;
  min-width: 0;
  padding: 11px var(--sp-4) 13px;
}

/* 四格并排时用发丝线分隔，而不是四张卡片（概览顶部就是这么用的）。 */
.u-kpi + .u-kpi {
  border-left: 1px solid var(--rule);
}

.u-kpi__label {
  margin: 0;
  font-size: var(--fs-xs);
  color: var(--ink-2);
}

.u-kpi__value {
  margin: 3px 0 0;
  font-size: var(--fs-2xl);
  font-weight: 500;
  letter-spacing: -0.025em;
  line-height: 1.1;
  word-break: break-word;
}

.u-kpi__unit {
  margin-left: 2px;
  font-size: var(--fs-sm);
  font-weight: 400;
  color: var(--ink-3);
}

.u-kpi[data-tone='warn'] .u-kpi__value {
  color: var(--warn);
}

.u-kpi[data-tone='bad'] .u-kpi__value {
  color: var(--bad);
}

.u-kpi__foot {
  margin-top: 6px;
  font-size: var(--fs-xs);
  color: var(--ink-3);
  min-width: 0;
}

.u-kpi__extra {
  margin-top: 8px;
}
</style>
