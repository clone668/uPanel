<script setup lang="ts">
// 模块图标：把 shell/icons.ts 的路径画成 24×24 描边图标。
//
// 一处细节：路径全部按 24 的格子画，所以 width/height 只改渲染尺寸，
// 不改线宽比例——同一套图标在 16px 和 28px 上粗细一致，图标格才对得齐。
// stroke-width 用 1.4 是为了在 16px 上仍然清晰：1 太细，2 在小尺寸会糊成块。

import { computed } from 'vue'

import { moduleIconPath } from '@/shell/icons'

const props = withDefaults(defineProps<{ name: string; size?: number }>(), { size: 20 })

const d = computed(() => moduleIconPath(props.name))
</script>

<template>
  <svg
    class="u-icon"
    :width="size"
    :height="size"
    viewBox="0 0 24 24"
    fill="none"
    stroke="currentColor"
    stroke-width="1.4"
    stroke-linecap="round"
    stroke-linejoin="round"
    aria-hidden="true"
    focusable="false"
  >
    <path :d="d" />
  </svg>
</template>

<style scoped>
.u-icon {
  display: block;
  flex: none;
}
</style>
