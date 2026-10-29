<script setup lang="ts">
// 统一页面状态组件。
//
// PLAN.md 第 7 节要求每个页面都能表达七种状态：
//   加载、空数据、无权限、依赖未安装、数据不支持、请求失败、部分数据可用。
// 这里把它们收敛成一个组件，避免每个页面各自发明一套图标/文案，也避免
// “采集失败却显示 0”这类被明令禁止的假正常。
//
// 状态不只用颜色表达：每种状态都有明确的文字标签与说明（无障碍要求）。

import { computed } from 'vue'
import { NAlert, NButton, NEmpty, NResult, NSpin } from 'naive-ui'

export type PanelState =
  | 'loading'
  | 'empty'
  | 'forbidden'
  | 'dependency_missing'
  | 'unsupported'
  | 'error'
  | 'partial'

const props = withDefaults(
  defineProps<{
    state: PanelState
    /** 覆盖默认标题（不填就用状态默认中文文案）。 */
    title?: string | undefined
    /** 补充说明：告诉用户下一步能做什么，不承诺做不到的事。 */
    description?: string | undefined
    /** 部分可用时，哪些块缺数据。 */
    missing?: string[]
    /** 失败时的错误码（服务端 code 或客户端 code）。 */
    code?: string | undefined
    /** 失败时的 request_id，便于报障。 */
    requestId?: string | undefined
    /** 是否显示“重试”。只在真的可以重试时打开。 */
    retryable?: boolean
    /**
     * 紧凑版：给窄面板（概览右侧那几栏）用。
     * 大号的 NResult 插图在 320px 宽的栏里会占掉半屏，把真正要读的原因挤到折线以下；
     * 紧凑版只留“状态 / 标题 / 原因 / 错误码”，文案与大号版逐字相同。
     */
    compact?: boolean
  }>(),
  {
    title: undefined,
    description: undefined,
    missing: () => [],
    code: undefined,
    requestId: undefined,
    retryable: false,
    compact: false,
  },
)

const emit = defineEmits<{ retry: [] }>()

interface Presentation {
  label: string
  title: string
  description: string
}

// 纯映射：状态 → 文案。测试直接断言这张表，不需要 DOM。
const PRESENTATIONS: Record<PanelState, Presentation> = {
  loading: {
    label: '加载中',
    title: '正在加载',
    description: '正在向面板服务请求数据，请稍候。',
  },
  empty: {
    label: '空数据',
    title: '暂无数据',
    description: '当前没有可显示的内容；这不是错误。',
  },
  forbidden: {
    label: '无权限',
    title: '没有查看权限',
    description: '当前账号无权访问这部分内容，需要管理员授予权限。',
  },
  dependency_missing: {
    label: '依赖未安装',
    title: '缺少必需的依赖',
    description: '该功能依赖的软件尚未安装或未启用，安装后可用。',
  },
  unsupported: {
    label: '数据不支持',
    title: '当前环境不支持该数据',
    description: '硬件或系统版本不支持这项采集，未知值不会显示为 0。',
  },
  error: {
    label: '请求失败',
    title: '请求失败',
    description: '面板服务没有返回可用结果，请稍后重试。',
  },
  partial: {
    label: '部分数据可用',
    title: '部分数据不可用',
    description: '页面已显示可用部分，缺失部分已单独标注。',
  },
}

const view = computed<Presentation>(() => {
  const base = PRESENTATIONS[props.state]
  return {
    label: base.label,
    title: props.title ?? base.title,
    description: props.description ?? base.description,
  }
})

// 状态标签总是可见，避免“只有颜色变了”。
const showSpinner = computed(() => props.state === 'loading')

// 紧凑版里，“状态：请求失败”下一行又写“请求失败”是纯噪音：
// 只有两个词真的不一样（例如“部分数据可用 / 部分数据不可用”）时才各写一行。
const compactLabelDiffers = computed(() => view.value.label !== view.value.title)
</script>

<template>
  <div class="state-panel" :class="{ 'state-panel--compact': compact }" :data-state="state">
    <p v-if="!compact || compactLabelDiffers" class="state-panel__label">
      状态：{{ view.label }}
    </p>

    <!-- 紧凑版：同样的文案，去掉插图与转圈，只留能读清的那几行。 -->
    <template v-if="compact">
      <p class="state-panel__compact-title">{{ view.title }}</p>
      <p class="state-panel__compact-desc">{{ view.description }}</p>
      <p v-if="missing.length > 0" class="state-panel__compact-desc">
        缺失项：{{ missing.join('、') }}
      </p>
      <p v-if="code" class="state-panel__meta">错误码：<code>{{ code }}</code></p>
      <p v-if="requestId" class="state-panel__meta">request_id：<code>{{ requestId }}</code></p>
      <NButton
        v-if="retryable"
        size="small"
        secondary
        type="primary"
        @click="emit('retry')"
      >
        重试
      </NButton>
    </template>

    <template v-else>
      <div v-if="showSpinner" class="state-panel__spinner">
        <NSpin size="medium" />
        <span class="state-panel__spin-text">{{ view.title }}</span>
      </div>

      <NEmpty v-else-if="state === 'empty'">
        <!-- 标题必须落地：页面给空状态起的名字（“这台机器上还没有受管部署”）
             才是用户看到的第一句话，只留 NEmpty 的默认“暂无数据”等于把它丢了。 -->
        <p class="state-panel__empty-title">{{ view.title }}</p>
        <p class="state-panel__empty-desc">{{ view.description }}</p>
      </NEmpty>

      <NResult
        v-else-if="state === 'error'"
        status="error"
        :title="view.title"
        :description="view.description"
      />

      <NResult
        v-else-if="state === 'forbidden'"
        status="403"
        :title="view.title"
        :description="view.description"
      />

      <NResult
        v-else-if="state === 'dependency_missing'"
        status="warning"
        :title="view.title"
        :description="view.description"
      />

      <NResult
        v-else-if="state === 'unsupported'"
        status="info"
        :title="view.title"
        :description="view.description"
      />

      <NAlert v-else type="warning" :title="view.title">
        {{ view.description }}
        <span v-if="missing.length > 0">缺失项：{{ missing.join('、') }}</span>
      </NAlert>

      <p v-if="code" class="state-panel__meta">错误码：<code>{{ code }}</code></p>
      <p v-if="requestId" class="state-panel__meta">request_id：<code>{{ requestId }}</code></p>

      <NButton v-if="retryable" secondary type="primary" @click="emit('retry')">重试</NButton>
    </template>
  </div>
</template>

<style scoped>
.state-panel {
  display: flex;
  flex-direction: column;
  align-items: flex-start;
  gap: 12px;
  padding: 24px;
}

.state-panel__label {
  margin: 0;
  font-size: var(--fs-md);
  color: var(--n-text-color-3, var(--ink-2));
}

.state-panel__spinner {
  display: flex;
  align-items: center;
  gap: 12px;
}

.state-panel__empty-title {
  margin: 0 0 6px;
  font-size: var(--fs-lg);
  font-weight: 600;
}

.state-panel__empty-desc {
  margin: 0;
  font-size: var(--fs-md);
  color: var(--n-text-color-3, var(--ink-2));
}

.state-panel__spin-text {
  font-size: var(--fs-lg);
}

.state-panel__meta {
  margin: 0;
  font-size: var(--fs-md);
  word-break: break-all;
}

/* 紧凑版：窄面板里只留几行字，不再给插图留出半屏。 */
.state-panel--compact {
  gap: 4px;
  padding: 4px 0;
}

.state-panel__compact-title {
  margin: 0;
  font-size: var(--fs-lg);
  font-weight: 600;
  color: var(--ink);
}

.state-panel__compact-desc {
  margin: 0;
  font-size: var(--fs-md);
  line-height: 1.5;
  color: var(--ink-2);
  word-break: break-word;
}

.state-panel--compact .state-panel__label {
  font-size: var(--fs-xs);
}
</style>
