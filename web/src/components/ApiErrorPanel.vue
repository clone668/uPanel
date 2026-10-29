<script setup lang="ts">
// 错误展示组件：所有页面统一用它显示请求失败，保证“错误码 + request_id + 可重试性”
// 不会因为某个页面偷懒而丢掉。
//
// 不显示堆栈、不显示请求体、不显示 Authorization/Cookie（CONTRACTS.md 第 1 节）。
//
// 版式：一条带左侧粗边的告警条（--bad 色），不是一张圆角彩色卡片。
// 一屏里可能同时有三处失败，告警条能并排堆着看；卡片会各自占一大块。

import { computed } from 'vue'

import type { ApiError } from '@/api/client'
import { describeApiError } from '@/api/errors'
import UButton from '@/ui/UButton.vue'

const props = withDefaults(
  defineProps<{
    error: ApiError
    /** 调用方给的具体标题（“无法列出目录”）。不给则用错误码对应的通用标题。 */
    title?: string
    /** 调用方能否安全重试（写操作一般不能：需要重新走确认流程）。 */
    allowRetry?: boolean
  }>(),
  { allowRetry: true, title: '' },
)

const emit = defineEmits<{ retry: [] }>()

const view = computed(() => describeApiError(props.error))
// 具体标题优先：同一条 PATH_OUTSIDE_ROOT 在“列目录”和“读文件”两个动作里，
// 人想知道的是**这次想做什么失败了**，而不是错误码的通用说法。
const heading = computed(() => (props.title === '' ? view.value.title : props.title))
const canRetry = computed(() => props.allowRetry && view.value.canRetry)
</script>

<template>
  <div class="error-panel" role="alert" data-testid="api-error">
    <div class="error-panel__box">
      <p class="error-panel__title">{{ heading }}</p>
      <p class="error-panel__message">{{ error.message }}</p>
      <p class="error-panel__hint">{{ view.hint }}</p>
      <p class="error-panel__meta">错误码：<code class="u-mono">{{ error.code }}</code></p>
      <p v-if="error.requestId" class="error-panel__meta">
        request_id：<code class="u-mono">{{ error.requestId }}</code>
      </p>
      <p v-if="view.needsLogin" class="error-panel__meta">重新登录功能在 P02 提供。</p>
    </div>
    <UButton v-if="canRetry" size="sm" variant="secondary" @click="emit('retry')">重试</UButton>
  </div>
</template>

<style scoped>
.error-panel {
  display: flex;
  flex-direction: column;
  align-items: flex-start;
  gap: var(--sp-2);
}

.error-panel__box {
  width: 100%;
  padding: 9px 12px 10px;
  background: var(--bad-soft);
  border: 1px solid var(--bad);
  /* 左侧粗边：一排失败条并排时，靠这一条边就能扫出"哪里出事了" */
  border-left-width: 3px;
  border-radius: var(--r-2);
}

.error-panel__title {
  margin: 0 0 3px;
  font-size: var(--fs-md);
  font-weight: 600;
  color: var(--bad);
}

.error-panel__message {
  margin: 0;
  font-size: var(--fs-md);
  color: var(--ink);
  word-break: break-word;
}

.error-panel__hint,
.error-panel__meta {
  margin: 3px 0 0;
  font-size: var(--fs-xs);
  color: var(--ink-2);
  word-break: break-all;
}

.error-panel__meta code {
  font-family: var(--font-mono);
}
</style>
