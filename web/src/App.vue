<script setup lang="ts">
// 根组件：主题/语言提供者 + 全局错误边界 + 外壳 + 路由出口。
//
// 语言固定中文（PLAN.md 要求界面中文），日期区域同样中文；不使用英文回退。

import { onErrorCaptured, ref } from 'vue'
import {
  NConfigProvider,
  NDialogProvider,
  NMessageProvider,
  dateZhCN,
  zhCN,
} from 'naive-ui'

import AppShell from '@/components/AppShell.vue'
import MockBanner from '@/components/MockBanner.vue'
import StatePanel from '@/components/StatePanel.vue'
import { useModeStore } from '@/stores/mode'

const modeStore = useModeStore()
const crashed = ref(false)

// 兜住渲染期异常：显示可读的失败态，而不是白屏。
onErrorCaptured((error) => {
  console.error('页面渲染失败', error)
  crashed.value = true
  return false
})
</script>

<template>
  <NConfigProvider :locale="zhCN" :date-locale="dateZhCN">
    <NMessageProvider>
      <NDialogProvider>
        <MockBanner :mode="modeStore.mode" />
        <AppShell>
          <StatePanel
            v-if="crashed"
            state="error"
            title="页面渲染失败"
            description="请刷新页面；若持续出现，请把浏览器控制台的错误信息反馈给管理员。"
          />
          <RouterView v-else />
        </AppShell>
      </NDialogProvider>
    </NMessageProvider>
  </NConfigProvider>
</template>
