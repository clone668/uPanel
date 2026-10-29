<script setup lang="ts">
// 桌面上的那一扇窗。
//
// 这不是装饰：标题栏的三个点是**真的**——关闭会回到桌面（路由到 /desk），
// 最大化会真的铺满，拖动与右下角拉伸会真的改变窗口位置与大小。做不到的那件事
// （同时开两扇窗）不画假按钮：内容由 vue-router 的一份 <RouterView> 渲染，同一时刻
// 只能挂一个页面，23 个视图都在用 useRoute()/useRouter()，要做真多窗口就得给每扇窗
// 塞一套伪路由上下文——那是改视图契约，不是换外壳（Q524 与 Q533）。
//
// 位置与大小记在模块级的 ref 里：切模块时窗口留在原地，回桌面再进来也不跳。
// 不写 localStorage：这是"这一会儿的摆法"，不是配置。

import { computed, onBeforeUnmount, ref } from 'vue'

import ModuleIcon from '@/ui/ModuleIcon.vue'

const props = defineProps<{
  /** 窗口标题（模块中文名）。 */
  title: string
  /** 图标键；未知页面为 null，此时只画文字。 */
  iconKey: string | null
}>()

const emit = defineEmits<{ close: [] }>()

const MIN_W = 620
const MIN_H = 420

// 初始摆法：居中偏上，占屏幕八成——第一眼看到的是"一扇窗"，不是"一整页"。
const rect = ref({ x: 0, y: 0, w: 0, h: 0 })
const maximized = ref(false)

function viewport(): { w: number; h: number } {
  return {
    w: document.documentElement.clientWidth,
    h: document.documentElement.clientHeight,
  }
}

function ensureRect(): void {
  if (rect.value.w > 0) return
  const { w, h } = viewport()
  const width = Math.min(Math.max(MIN_W, Math.round(w * 0.82)), w - 24)
  const height = Math.min(Math.max(MIN_H, Math.round(h * 0.78)), h - 96)
  rect.value = {
    x: Math.max(16, Math.round((w - width) / 2)),
    y: Math.max(56, Math.round((h - height) / 2)),
    w: width,
    h: height,
  }
}

ensureRect()

const frameStyle = computed(() => {
  if (maximized.value) {
    return { left: '8px', top: '8px', right: '8px', bottom: '8px', width: 'auto', height: 'auto' }
  }
  const r = rect.value
  return {
    left: `${r.x}px`,
    top: `${r.y}px`,
    width: `${r.w}px`,
    height: `${r.h}px`,
    right: 'auto',
    bottom: 'auto',
  }
})

// ---------------------------------------------------------------- 拖动
type DragState = { mode: 'move' | 'resize'; px: number; py: number; start: typeof rect.value }

let drag: DragState | null = null

function begin(mode: 'move' | 'resize', event: PointerEvent): void {
  if (maximized.value || event.button !== 0) return
  // 标题栏上的按钮不触发拖动；用 closest 比 stopPropagation 稳，按钮自己的 click 照常走。
  if ((event.target as HTMLElement).closest('button') !== null) return
  drag = { mode, px: event.clientX, py: event.clientY, start: { ...rect.value } }
  ;(event.currentTarget as HTMLElement).setPointerCapture(event.pointerId)
  event.preventDefault()
}

function move(event: PointerEvent): void {
  if (drag === null) return
  const dx = event.clientX - drag.px
  const dy = event.clientY - drag.py
  const { w: vw, h: vh } = viewport()
  if (drag.mode === 'move') {
    // 夹住位置：标题栏必须留在屏幕里，否则窗口会被拖到抓不回来的地方。
    rect.value = {
      ...rect.value,
      x: Math.min(Math.max(-drag.start.w + 160, drag.start.x + dx), vw - 160),
      y: Math.min(Math.max(0, drag.start.y + dy), vh - 80),
    }
    return
  }
  rect.value = {
    ...rect.value,
    w: Math.min(Math.max(MIN_W, drag.start.w + dx), vw - drag.start.x - 8),
    h: Math.min(Math.max(MIN_H, drag.start.h + dy), vh - drag.start.y - 8),
  }
}

function end(): void {
  drag = null
}

onBeforeUnmount(() => {
  drag = null
})
</script>

<template>
  <div class="win" :style="frameStyle" :data-max="maximized ? 'true' : 'false'">
    <header class="win__bar" @pointerdown="begin('move', $event)" @pointermove="move" @pointerup="end" @pointercancel="end" @dblclick="maximized = !maximized">
      <span class="win__dots">
        <button class="win__dot win__dot--close" type="button" title="关闭（回到桌面）" aria-label="关闭窗口" @click="emit('close')"></button>
        <button
          class="win__dot win__dot--max"
          type="button"
          :title="maximized ? '还原窗口' : '铺满窗口'"
          :aria-label="maximized ? '还原窗口' : '铺满窗口'"
          @click="maximized = !maximized"
          @dblclick.stop
        ></button>
      </span>
      <span class="win__icon"><ModuleIcon v-if="iconKey" :name="iconKey" :size="16" /></span>
      <span class="win__title">{{ title }}</span>
      <span class="win__spacer"></span>
      <span class="win__hint">拖动标题栏移动 · 双击铺满</span>
    </header>

    <div class="win__body">
      <slot />
    </div>

    <span class="win__grip" title="拖动改变窗口大小" @pointerdown="begin('resize', $event)" @pointermove="move" @pointerup="end" @pointercancel="end"></span>
  </div>
</template>

<style scoped>
.win {
  position: absolute;
  z-index: var(--z-window);
  display: flex;
  flex-direction: column;
  min-width: 0;
  min-height: 0;
  overflow: hidden;
  background: var(--surface);
  border: 1px solid var(--rule-strong);
  border-radius: var(--r-3);
  box-shadow: var(--shadow-win);
}

.win[data-max='true'] {
  border-radius: var(--r-2);
}

/* 标题栏：整条可拖，抓手是它自己 */
.win__bar {
  display: flex;
  align-items: center;
  gap: 8px;
  flex: none;
  height: 34px;
  padding: 0 8px 0 10px;
  background: var(--surface-2);
  border-bottom: 1px solid var(--rule);
  cursor: grab;
  user-select: none;
  touch-action: none;
}

.win__bar:active {
  cursor: grabbing;
}

/* 三个点：只画真的能用的两个动作 + 还原。红点=关闭，绿点=最大化，黄点=还原 */
.win__dots {
  display: flex;
  gap: 6px;
  align-items: center;
  padding-right: 2px;
}

.win__dot {
  width: 11px;
  height: 11px;
  padding: 0;
  border: 1px solid rgba(0, 0, 0, 0.18);
  border-radius: 50%;
  cursor: pointer;
}

.win__dot--close {
  background: #e2645a;
}

.win__dot--max {
  background: #63b165;
}

.win__dot:hover {
  filter: brightness(0.92);
}

.win__hint {
  flex: none;
  padding-right: 4px;
  font-size: 10px;
  color: var(--ink-3);
  opacity: 0;
  transition: opacity 0.15s ease;
}

.win__bar:hover .win__hint {
  opacity: 1;
}

.win__icon {
  display: flex;
  color: var(--ink-3);
}

.win__title {
  font-size: var(--fs-sm);
  font-weight: 600;
  color: var(--ink);
  letter-spacing: 0.02em;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}

.win__spacer {
  flex: 1 1 auto;
  min-width: 0;
}

/* 内容区：自己滚，不把窗口撑破 */
.win__body {
  flex: 1 1 auto;
  min-height: 0;
  overflow: auto;
  overscroll-behavior: contain;
}

/* 右下角的拉伸把手 */
.win__grip {
  position: absolute;
  right: 0;
  bottom: 0;
  width: 16px;
  height: 16px;
  cursor: nwse-resize;
  background: linear-gradient(
    135deg,
    transparent 0 55%,
    var(--rule-strong) 55% 60%,
    transparent 60% 72%,
    var(--rule-strong) 72% 77%,
    transparent 77%
  );
  touch-action: none;
}

/* 窄屏与触摸：窗口铺满，没有拖动可拉 */
@media (max-width: 820px) {
  .win {
    left: 6px !important;
    top: 6px !important;
    right: 6px !important;
    bottom: 6px !important;
    width: auto !important;
    height: auto !important;
  }

  .win__bar {
    cursor: default;
  }

  .win__grip {
    display: none;
  }
}
</style>
