<script setup lang="ts">
// 按钮：整套界面上只有四种含义，多一个都是噪音。
//
//   primary    —— 这一屏的主线动作（新建任务、初始化磁盘）
//   secondary  —— 并列的次要动作（重新采集、取消）
//   quiet      —— 视觉上退到文字里（刷新、查看全部）
//   danger     —— 会破坏数据/服务的动作（格式化、卸载、删除）
//
// 断言的只有一件事：禁用时**不吞点击**，并且仍然能被键盘 Focus 到——
// 用户需要知道“这个按钮存在但此刻不能用”，而不是它凭空消失。
//
// href 存在时渲染成 <a>：打开入口是中键/Ctrl+点击，必须是真链接。

import { computed } from 'vue'

import { isInert } from '@/ui/button'

const props = withDefaults(
  defineProps<{
    variant?: 'primary' | 'secondary' | 'quiet' | 'danger'
    size?: 'sm' | 'md'
    disabled?: boolean
    loading?: boolean
    type?: 'button' | 'submit'
    href?: string | undefined
    target?: string | undefined
    rel?: string | undefined
  }>(),
  {
    variant: 'secondary',
    size: 'md',
    disabled: false,
    loading: false,
    type: 'button',
    href: undefined,
    target: undefined,
    rel: undefined,
  },
)

const emit = defineEmits<{ click: [event: MouseEvent] }>()

const inert = computed(() => isInert(props.disabled, props.loading))

function onClick(event: MouseEvent): void {
  if (inert.value) {
    // 禁用态不发事件：否则调用方会在“按钮看着灰着”的情况下收到一次提交。
    event.preventDefault()
    event.stopPropagation()
    return
  }
  emit('click', event)
}
</script>

<template>
  <a
    v-if="href !== undefined"
    class="u-btn"
    :class="[`u-btn--${variant}`, `u-btn--${size}`]"
    :data-variant="variant"
    :href="inert ? undefined : href"
    :target="target"
    :rel="rel ?? (target === '_blank' ? 'noopener noreferrer' : undefined)"
    :aria-disabled="inert ? 'true' : undefined"
    @click="onClick"
  >
    <span v-if="loading" class="u-btn__spin" aria-hidden="true"></span>
    <slot />
  </a>

  <button
    v-else
    class="u-btn"
    :class="[`u-btn--${variant}`, `u-btn--${size}`]"
    :data-variant="variant"
    :type="type"
    :disabled="inert"
    :aria-busy="loading ? 'true' : undefined"
    @click="onClick"
  >
    <span v-if="loading" class="u-btn__spin" aria-hidden="true"></span>
    <slot />
  </button>
</template>

<style scoped>
.u-btn {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  gap: 6px;
  font-family: inherit;
  font-weight: 500;
  line-height: 1.2;
  white-space: nowrap;
  text-decoration: none;
  border-radius: var(--r-2);
  border: 1px solid var(--rule-strong);
  background: var(--surface);
  color: var(--ink);
  cursor: pointer;
  transition:
    background-color 120ms ease,
    border-color 120ms ease,
    color 120ms ease;
}

.u-btn--sm {
  font-size: var(--fs-sm);
  padding: 4px 10px;
}

.u-btn--md {
  font-size: var(--fs-md);
  padding: 6px 12px;
}

.u-btn:hover {
  border-color: var(--ink-3);
}

.u-btn--primary {
  background: var(--accent);
  border-color: var(--accent);
  color: var(--accent-ink);
}

.u-btn--primary:hover {
  border-color: var(--accent);
  filter: brightness(1.08);
}

.u-btn--secondary {
  background: var(--surface);
}

.u-btn--secondary:hover {
  background: var(--surface-2);
}

.u-btn--quiet {
  background: transparent;
  border-color: transparent;
  color: var(--ink-2);
}

.u-btn--quiet:hover {
  background: var(--surface-2);
  border-color: transparent;
  color: var(--ink);
}

.u-btn--danger {
  background: transparent;
  border-color: var(--bad);
  color: var(--bad);
}

.u-btn--danger:hover {
  background: var(--bad);
  border-color: var(--bad);
  color: var(--surface);
}

.u-btn:disabled,
.u-btn[aria-disabled='true'] {
  cursor: not-allowed;
  /* 禁用不等于灰到看不见：要能读清“是什么按钮不能按”。 */
  color: var(--ink-3);
  border-color: var(--rule);
  background: var(--surface-2);
}

.u-btn--primary:disabled,
.u-btn--danger:disabled {
  filter: none;
}

.u-btn__spin {
  width: 11px;
  height: 11px;
  border: 1.5px solid currentColor;
  border-right-color: transparent;
  border-radius: 50%;
  animation: u-btn-spin 620ms linear infinite;
}

@keyframes u-btn-spin {
  to {
    transform: rotate(360deg);
  }
}
</style>
