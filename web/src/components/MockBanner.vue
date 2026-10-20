<script setup lang="ts">
// mock 模式横幅。
//
// CONTRACTS.md 第 3 节：mock 服务与真实代理完全隔离，页面**持续显示**
// “演示数据，未操作服务器”。所以这个横幅不带关闭按钮，也不做“记住已读”。
//
// 文案里必须同时说清：这是演示数据、且没有对服务器做任何操作。

import { computed } from 'vue'
import { NAlert } from 'naive-ui'

import type { AppMode } from '@/stores/mode'

const props = defineProps<{ mode: AppMode }>()

const visible = computed(() => props.mode === 'mock')
</script>

<template>
  <NAlert
    v-if="visible"
    class="mock-banner"
    type="warning"
    :bordered="false"
    data-testid="mock-banner"
    title="演示模式：数据为演示数据，未操作服务器"
  >
    当前面板以 mock 模式运行，所有内容都是演示数据，所有操作都不会写入本机；
    真实系统功能请在 production 模式下使用。
  </NAlert>
</template>

<style scoped>
.mock-banner {
  border-radius: 0;
}
</style>
