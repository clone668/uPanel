<script setup lang="ts">
// 桌面图标：一列一列排下去，像真的桌面上摆着的应用。
//
// 为什么按列排而不是按行：桌面上图标总是从左上角往下长，排满一列再开下一列
// （grid-auto-flow: column + 固定行数）。按行排会变成"一张卡片列表"，
// 那是网页，不是桌面。
//
// 单击即打开，不用双击：双击是 1995 年的约定，这里没有"选中"这回事，
// 选中状态只会让人多点一次。键盘 Tab 一样能到，Enter 打开。

import { computed } from 'vue'

import { visibleModules } from '@/modules/registry'
import ModuleIcon from '@/ui/ModuleIcon.vue'

const props = defineProps<{
  /** 当前模块 path；在桌面上（无窗口）时为 null。 */
  activePath: string | null
}>()

const emit = defineEmits<{ open: [path: string] }>()

const modules = computed(() => visibleModules())
</script>

<template>
  <div class="icons" role="list" aria-label="桌面应用">
    <button
      v-for="module in modules"
      :key="module.key"
      class="icons__item"
      type="button"
      role="listitem"
      :title="module.summary"
      :data-active="module.path === props.activePath ? 'true' : 'false'"
      @click="emit('open', module.path)"
    >
      <span class="icons__tile"><ModuleIcon :name="module.key" :size="26" /></span>
      <span class="icons__label">{{ module.label }}</span>
    </button>
  </div>
</template>

<style scoped>
.icons {
  position: absolute;
  z-index: 1;
  top: 56px;
  left: 18px;
  right: 18px;
  bottom: 104px;
  display: grid;
  /* 先竖向排满一列，再开下一列 */
  grid-template-rows: repeat(auto-fill, 96px);
  grid-auto-flow: column;
  grid-auto-columns: 96px;
  align-content: start;
  justify-content: start;
  gap: 4px 6px;
  overflow: hidden;
  pointer-events: none;
}

.icons__item {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 5px;
  width: 92px;
  padding: 8px 4px 6px;
  font: inherit;
  color: var(--ink-2);
  background: transparent;
  border: 0;
  border-radius: var(--r-3);
  cursor: pointer;
  pointer-events: auto;
}

.icons__item:hover {
  background: rgba(251, 250, 246, 0.72);
}

.icons__item:focus-visible {
  outline: 2px solid var(--accent);
  outline-offset: -2px;
}

/* 图标的底板：一块小纸面，让线条图标在白墙上也立得住 */
.icons__tile {
  display: grid;
  place-items: center;
  width: 46px;
  height: 46px;
  color: var(--ink-2);
  background: var(--surface);
  border: 1px solid var(--rule-strong);
  border-radius: 11px;
  box-shadow: 0 1px 0 rgba(255, 255, 255, 0.7) inset;
}

.icons__item[data-active='true'] .icons__tile {
  color: var(--accent-ink);
  background: var(--accent);
  border-color: var(--accent);
}

.icons__label {
  max-width: 92px;
  padding: 1px 4px;
  font-size: var(--fs-sm);
  line-height: 1.25;
  text-align: center;
  color: var(--ink);
  border-radius: var(--r-1);
}

.icons__item[data-active='true'] .icons__label {
  color: var(--accent);
  font-weight: 600;
}

@media (max-width: 820px) {
  .icons {
    top: 52px;
    left: 10px;
    right: 10px;
    bottom: 96px;
    grid-template-rows: repeat(auto-fill, 88px);
    grid-auto-columns: 84px;
  }

  .icons__item {
    width: 80px;
    padding: 6px 2px 4px;
  }

  .icons__tile {
    width: 42px;
    height: 42px;
  }
}
</style>
