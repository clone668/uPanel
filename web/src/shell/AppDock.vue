<script setup lang="ts">
// 应用坞：常驻的主导航。
//
// 与以前顶栏横排菜单的区别不只是位置：
//   - 每一项都有图标 + 中文名，新手不必先学会"这一排字是导航"；
//   - 顺序固定来自 modules/registry.ts（PLAN.md 第 7 节的 16 项顺序），不按使用频率重排——
//     位置会动的话，肌肉记忆就没法形成；
//   - 未实现模块不出现（visibleModules），这里不会有一个点了没反应的格子。
//
// 当前项用 2px 的强调线 + 文字颜色标记，不只是颜色：色弱也要看得出在哪一格。

import { computed } from 'vue'

import { visibleModules } from '@/modules/registry'
import ModuleIcon from '@/ui/ModuleIcon.vue'

const props = defineProps<{
  /** 当前模块的 path（来自 resolveActiveModule）；未知页面为 null。 */
  activePath: string | null
}>()

const emit = defineEmits<{ select: [path: string]; launcher: [] }>()

const modules = computed(() => visibleModules())
</script>

<template>
  <nav class="dock" aria-label="模块导航">
    <button
      class="dock__item"
      type="button"
      title="全部应用"
      @click="emit('launcher')"
    >
      <span class="dock__launcher" aria-hidden="true">
        <i></i><i></i><i></i><i></i>
      </span>
      <span class="dock__label">全部</span>
    </button>

    <span class="dock__rule" aria-hidden="true"></span>

    <button
      v-for="module in modules"
      :key="module.key"
      class="dock__item"
      type="button"
      :title="module.label"
      :data-active="module.path === props.activePath ? 'true' : 'false'"
      :aria-current="module.path === props.activePath ? 'page' : undefined"
      @click="emit('select', module.path)"
    >
      <ModuleIcon :name="module.key" :size="20" />
      <span class="dock__label">{{ module.label }}</span>
    </button>
  </nav>
</template>

<style scoped>
.dock {
  position: fixed;
  z-index: var(--z-dock);
  left: 50%;
  bottom: 12px;
  transform: translateX(-50%);
  display: flex;
  align-items: stretch;
  gap: 2px;
  max-width: calc(100vw - 24px);
  padding: 5px;
  overflow-x: auto;
  background: var(--surface);
  border: 1px solid var(--rule);
  border-radius: var(--r-3);
  box-shadow: var(--shadow-win);
  scrollbar-width: thin;
}

.dock__item {
  position: relative;
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  gap: 3px;
  width: 60px;
  flex: none;
  padding: 5px 2px 6px;
  font: inherit;
  color: var(--ink-2);
  background: transparent;
  border: 0;
  border-radius: var(--r-2);
  cursor: pointer;
}

.dock__item:hover {
  color: var(--ink);
  background: var(--surface-2);
}

.dock__item[data-active='true'] {
  color: var(--accent);
  background: var(--accent-soft);
}

/* 当前项：顶部一条 2px 强调线（不是只有颜色变化） */
.dock__item[data-active='true']::before {
  content: '';
  position: absolute;
  top: -5px;
  left: 8px;
  right: 8px;
  height: 2px;
  background: var(--accent);
  border-radius: 0 0 var(--r-1) var(--r-1);
}

.dock__label {
  font-size: var(--fs-xs);
  line-height: 1.1;
  white-space: nowrap;
}

/* “全部”的四宫格：用四个小方块画，和图标同一套语言 */
.dock__launcher {
  display: grid;
  grid-template-columns: repeat(2, 7px);
  grid-template-rows: repeat(2, 7px);
  gap: 2px;
  width: 16px;
  height: 16px;
  margin: 2px 0;
}

.dock__launcher i {
  background: currentColor;
  border-radius: var(--r-1);
  opacity: 0.85;
}

.dock__rule {
  width: 0;
  flex: none;
  margin: 4px 4px;
  border-left: 1px solid var(--rule);
}

@media (max-width: 760px) {
  .dock {
    bottom: 8px;
    padding: 4px;
  }

  .dock__item {
    width: 54px;
  }

  .dock__label {
    font-size: 10px;
  }
}
</style>
