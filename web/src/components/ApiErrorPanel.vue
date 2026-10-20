<script setup lang="ts">
// 错误展示组件：所有页面统一用它显示请求失败，保证“错误码 + request_id + 可重试性”
// 不会因为某个页面偷懒而丢掉。
//
// 不显示堆栈、不显示请求体、不显示 Authorization/Cookie（CONTRACTS.md 第 1 节）。

import { computed } from 'vue'
import { NAlert, NButton } from 'naive-ui'

import type { ApiError } from '@/api/client'
import { describeApiError } from '@/api/errors'

const props = withDefaults(
  defineProps<{
    error: ApiError
    /** 调用方能否安全重试（写操作一般不能：需要重新走确认流程）。 */
    allowRetry?: boolean
  }>(),
  { allowRetry: true },
)

const emit = defineEmits<{ retry: [] }>()

const view = computed(() => describeApiError(props.error))
const canRetry = computed(() => props.allowRetry && view.value.canRetry)
</script>

<template>
  <div class="error-panel" role="alert" data-testid="api-error">
    <NAlert type="error" :title="view.title">
      <p class="error-panel__message">{{ error.message }}</p>
      <p class="error-panel__hint">{{ view.hint }}</p>
      <p class="error-panel__meta">错误码：<code>{{ error.code }}</code></p>
      <p v-if="error.requestId" class="error-panel__meta">
        request_id：<code>{{ error.requestId }}</code>
      </p>
      <p v-if="view.needsLogin" class="error-panel__meta">重新登录功能在 P02 提供。</p>
    </NAlert>
    <NButton v-if="canRetry" secondary type="primary" @click="emit('retry')">重试</NButton>
  </div>
</template>

<style scoped>
.error-panel {
  display: flex;
  flex-direction: column;
  align-items: flex-start;
  gap: 12px;
}

.error-panel__message {
  margin: 0 0 8px;
  font-weight: 600;
}

.error-panel__hint,
.error-panel__meta {
  margin: 0;
  font-size: 13px;
  word-break: break-all;
}
</style>
