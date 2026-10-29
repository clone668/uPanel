<script setup lang="ts">
// 面板：窗口里的一块内容区。
//
// 用发丝分隔线而不是“卡片 + 圆角 + 阴影”三件套：一屏摆八个带阴影的盒子，
// 眼睛会被盒子边界拽走，看不到里面的数。这里有边框、有表头、没有阴影。
//
// visible 只是方便传空数据时整块不渲染；真正的空态请用 StatePanel，
// 让“没有数据”和“请求失败”各有各的说法。

withDefaults(
  defineProps<{
    title?: string | undefined
    /** 标题右边的补充说明（计数、采样时间、口径），等宽小字。 */
    meta?: string | undefined
    /** 内容自带 padding 时关掉（表格通常要自己贴边）。 */
    flush?: boolean
    /** 面板里正在加载：标题留着，内容区换成骨架。 */
    loading?: boolean
  }>(),
  { title: undefined, meta: undefined, flush: false, loading: false },
)
</script>

<template>
  <section class="u-panel">
    <header v-if="title !== undefined || $slots.actions" class="u-panel__head">
      <h2 v-if="title !== undefined" class="u-panel__title">{{ title }}</h2>
      <span v-if="meta !== undefined" class="u-panel__meta u-num">{{ meta }}</span>
      <span class="u-panel__spacer"></span>
      <div v-if="$slots.actions" class="u-panel__actions">
        <slot name="actions" />
      </div>
    </header>

    <div class="u-panel__body" :class="{ 'u-panel__body--flush': flush }">
      <slot v-if="!loading" />
      <div v-else class="u-panel__loading" role="status" aria-live="polite">
        <span class="u-visually-hidden">正在加载</span>
        <i v-for="n in 3" :key="n" class="u-panel__bar" aria-hidden="true"></i>
      </div>
    </div>

    <footer v-if="$slots.footer" class="u-panel__foot">
      <slot name="footer" />
    </footer>
  </section>
</template>

<style scoped>
.u-panel {
  display: flex;
  flex-direction: column;
  min-width: 0;
  background: var(--surface);
  border: 1px solid var(--rule);
  border-radius: var(--r-2);
  overflow: hidden;
}

.u-panel__head {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
  padding: 8px var(--sp-3);
  border-bottom: 1px solid var(--rule);
  background: var(--surface-2);
}

.u-panel__title {
  margin: 0;
  font-size: var(--fs-sm);
  font-weight: 600;
  letter-spacing: 0.01em;
}

.u-panel__meta {
  font-size: 10px;
  color: var(--ink-3);
}

.u-panel__spacer {
  flex: 1;
}

.u-panel__actions {
  display: flex;
  align-items: center;
  gap: 6px;
}

.u-panel__body {
  padding: var(--sp-3) var(--sp-4);
  min-width: 0;
}

.u-panel__body--flush {
  padding: 0;
}

.u-panel__foot {
  padding: 8px var(--sp-3);
  border-top: 1px solid var(--rule);
  font-size: var(--fs-sm);
  color: var(--ink-2);
}

/* 骨架：一块一块来，不是全屏转圈——一页四块数据，一块慢不该拖住另外三块。 */
.u-panel__loading {
  display: flex;
  flex-direction: column;
  gap: 10px;
  padding: 6px 0;
}

.u-panel__bar {
  height: 12px;
  border-radius: var(--r-1);
  background: linear-gradient(90deg, var(--surface-2), var(--surface-3), var(--surface-2));
  background-size: 200% 100%;
  animation: u-panel-shimmer 1400ms ease-in-out infinite;
}

.u-panel__bar:nth-child(3) {
  width: 72%;
}

.u-panel__bar:nth-child(4) {
  width: 88%;
}

@keyframes u-panel-shimmer {
  from {
    background-position: 0% 0;
  }
  to {
    background-position: -200% 0;
  }
}

@media (prefers-reduced-motion: reduce) {
  .u-panel__bar {
    animation: none;
  }
}
</style>
