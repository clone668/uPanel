<script setup lang="ts">
// 应用抽屉：把全部已接入的模块铺成一张带说明的大格子。
//
// 为什么要有它：应用坞只有图标和两个字，够熟手用；新手第一次进来需要"这玩意儿能干什么"，
// 所以这里的每一格都带一句来自 modules/registry.ts 的 summary —— 那句话是**已经能做的**，
// 不是路线图口吻的承诺。
//
// 未实现模块（implementedIn 为 null）在这里同样不出现：PLAN.md 第 7 节要求未实现模块隐藏，
// 摆一个灰格子虽然点不动，也仍然是"摆了个入口"。

import { computed, nextTick, ref, watch } from 'vue'

import { visibleModules } from '@/modules/registry'
import ModuleIcon from '@/ui/ModuleIcon.vue'

const props = defineProps<{
  open: boolean
  activePath: string | null
}>()

const emit = defineEmits<{ close: []; select: [path: string] }>()

const modules = computed(() => visibleModules())
const panel = ref<HTMLElement | null>(null)

watch(
  () => props.open,
  async (isOpen) => {
    if (!isOpen) return
    await nextTick()
    // 打开即聚焦面板本身：这样 Esc 立刻生效，读屏也会先读到"全部应用"。
    panel.value?.focus()
  },
)

function pick(path: string): void {
  emit('select', path)
}
</script>

<template>
  <Transition name="launcher">
    <div
      v-if="open"
      class="launcher"
      role="dialog"
      aria-modal="true"
      aria-label="全部应用"
      @click.self="emit('close')"
      @keydown.esc.stop="emit('close')"
    >
      <div ref="panel" class="launcher__panel" tabindex="-1">
        <div class="launcher__head">
          <span class="launcher__title">全部应用</span>
          <span class="launcher__count u-num">{{ modules.length }}</span>
          <button class="launcher__close" type="button" @click="emit('close')">
            关闭<span class="launcher__esc">Esc</span>
          </button>
        </div>

        <div class="launcher__grid">
          <button
            v-for="module in modules"
            :key="module.key"
            class="app"
            type="button"
            :data-active="module.path === props.activePath ? 'true' : 'false'"
            @click="pick(module.path)"
          >
            <span class="app__head">
              <ModuleIcon :name="module.key" :size="20" />
              <span class="app__label">{{ module.label }}</span>
              <span v-if="module.path === props.activePath" class="app__here">当前</span>
            </span>
            <span class="app__sum">{{ module.summary }}</span>
          </button>
        </div>
      </div>
    </div>
  </Transition>
</template>

<style scoped>
.launcher {
  position: fixed;
  inset: 0;
  z-index: var(--z-scrim);
  display: flex;
  align-items: flex-start;
  justify-content: center;
  padding: 64px var(--sp-4) var(--sp-6);
  background: color-mix(in srgb, var(--desk) 72%, transparent);
  backdrop-filter: blur(6px);
  overflow-y: auto;
}

.launcher__panel {
  width: 100%;
  max-width: 980px;
  padding: var(--sp-4);
  background: var(--surface);
  border: 1px solid var(--rule);
  border-radius: var(--r-3);
  box-shadow: var(--shadow-pop);
  outline: none;
}

.launcher__head {
  display: flex;
  align-items: baseline;
  gap: var(--sp-2);
  padding: 0 2px var(--sp-3);
  border-bottom: 1px solid var(--rule);
}

.launcher__title {
  font-size: var(--fs-lg);
  font-weight: 600;
  color: var(--ink);
}

.launcher__count {
  font-size: var(--fs-sm);
  color: var(--ink-3);
}

.launcher__close {
  margin-left: auto;
  display: inline-flex;
  align-items: center;
  gap: 6px;
  padding: 3px 8px;
  font: inherit;
  font-size: var(--fs-sm);
  color: var(--ink-2);
  background: transparent;
  border: 1px solid var(--rule);
  border-radius: var(--r-2);
  cursor: pointer;
}

.launcher__close:hover {
  color: var(--ink);
  background: var(--surface-2);
}

.launcher__esc {
  font-family: var(--font-mono);
  font-size: var(--fs-xs);
  color: var(--ink-3);
}

.launcher__grid {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(196px, 1fr));
  gap: var(--sp-2);
  padding-top: var(--sp-3);
}

.app {
  display: flex;
  flex-direction: column;
  gap: 6px;
  padding: 10px;
  text-align: left;
  font: inherit;
  color: var(--ink);
  background: var(--surface-2);
  border: 1px solid transparent;
  border-radius: var(--r-2);
  cursor: pointer;
}

.app:hover {
  background: var(--surface-3);
  border-color: var(--rule-strong);
}

.app[data-active='true'] {
  background: var(--accent-soft);
  border-color: var(--accent);
}

.app__head {
  display: flex;
  align-items: center;
  gap: 7px;
  color: var(--ink);
}

.app[data-active='true'] .app__head {
  color: var(--accent);
}

.app__label {
  font-size: var(--fs-md);
  font-weight: 600;
}

.app__here {
  margin-left: auto;
  font-size: var(--fs-xs);
  color: var(--accent);
}

/* 说明最多两行：超过就截断，格子高度不会参差（参差比截断更难看，也更难扫） */
.app__sum {
  display: -webkit-box;
  -webkit-line-clamp: 2;
  -webkit-box-orient: vertical;
  overflow: hidden;
  font-size: var(--fs-xs);
  line-height: 1.45;
  color: var(--ink-3);
}

.launcher-enter-active,
.launcher-leave-active {
  transition: opacity 120ms ease;
}

.launcher-enter-from,
.launcher-leave-to {
  opacity: 0;
}

@media (max-width: 760px) {
  .launcher {
    padding: 40px var(--sp-2) var(--sp-2);
  }

  .launcher__grid {
    grid-template-columns: 1fr;
  }
}
</style>
