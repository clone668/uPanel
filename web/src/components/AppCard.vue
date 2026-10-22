<script setup lang="ts">
// 一张应用卡片（P09.5）。
//
// 卡片要在一眼之内回答四个问题，所以四件事各占一行，谁也不合并：
//   1. 它此刻在不在跑（runtime_state，来自 systemd/docker 的观测）；
//   2. 它对外那一面通不通（http_state，来自面板的探测）；
//   3. 依赖卷的自动启动有没有绕开面板（dependency_protection）；
//   4. 我现在能对它做什么（capabilities → 按钮）。
//
// 第 1 与第 2 条**永远分开显示**。合成一句“状态良好”就等于把“容器在跑但 500”这件事
// 藏起来，而 STAGES §11 的验收行恰恰要求它明确显示成异常。
//
// 卡片上没有“删除”之外的管理动作：新建/编辑在表单里，启停走同一个预检→任务对话框。
// 按钮只按服务端给的 capabilities 渲染——link 没有启停，受保护单元只有日志，
// 这是服务端的判决，不是页面的猜测。

import { computed } from 'vue'
import { NButton, NTag } from 'naive-ui'

import {
  APP_ACTION_LABELS,
  DEPENDENCY_PROTECTION_LABELS,
  DEPENDENCY_PROTECTION_TONES,
  HTTP_STATE_LABELS,
  RUNTIME_STATE_LABELS,
  RUNTIME_STATE_TONES,
  availableActions,
  describeBinding,
  iconGlyph,
  openTarget,
  primaryAction,
  type App,
  type AppAction,
} from '@/api/apps'
import { formatTimestamp } from '@/utils/format'

const props = defineProps<{
  app: App
  /** 当前账号能不能做管理动作（viewer 只读）。 */
  canAct: boolean
  /** 这个应用的某个动作正在提交（按钮转圈）。 */
  busy?: boolean
}>()

const emit = defineEmits<{
  control: [app: App, action: AppAction]
  logs: [app: App]
  edit: [app: App]
  favorite: [app: App, next: boolean]
  remove: [app: App]
}>()

const target = computed(() => openTarget(props.app))
const actions = computed(() => availableActions(props.app.capabilities))
const primary = computed(() => primaryAction(props.app.capabilities))
/** 生命周期按钮（启动/停止/重启）：走预检→任务，与“日志”“打开”不是一条路。 */
const lifecycle = computed(() =>
  actions.value.filter((action) => action === 'start' || action === 'stop' || action === 'restart'),
)
const canLogs = computed(() => actions.value.includes('logs'))
/** “打开”是一个真的链接：这样中键与 Ctrl+点击的行为与普通入口一致。 */
const opensInNewTab = computed(() => primary.value === 'open' && target.value.href !== '')

const httpTone = computed(() => {
  switch (props.app.http_state) {
    case 'ok':
      return 'success' as const
    case 'error':
      return 'error' as const
    case 'unknown':
      return 'warning' as const
    default:
      return 'default' as const
  }
})

const checkedLine = computed(() => {
  const at = props.app.last_checked_at
  if (at === null) return '从没探测过'
  return `上次探测 ${formatTimestamp(at)}`
})

function onPrimary(): void {
  if (primary.value !== null && primary.value !== 'open') emit('control', props.app, primary.value)
}
</script>

<template>
  <article class="app-card" :data-app="app.id" :data-type="app.type" :data-runtime="app.runtime_state">
    <header class="app-card__head">
      <span class="app-card__icon" :data-icon="app.icon_key" aria-hidden="true">
        {{ iconGlyph(app.icon_key) }}
      </span>
      <h3 class="app-card__name">{{ app.name }}</h3>

      <NTag size="small" :type="RUNTIME_STATE_TONES[app.runtime_state]">
        {{ RUNTIME_STATE_LABELS[app.runtime_state] }}
      </NTag>
      <NTag size="small" :type="httpTone" :data-http="app.http_state">
        入口{{ HTTP_STATE_LABELS[app.http_state] }}
      </NTag>
      <NTag
        v-if="app.dependency_protection !== 'not_applicable'"
        size="small"
        :type="DEPENDENCY_PROTECTION_TONES[app.dependency_protection]"
      >
        {{ DEPENDENCY_PROTECTION_LABELS[app.dependency_protection] }}
      </NTag>

      <NButton
        size="tiny"
        quaternary
        :title="app.favorite ? '取消常用' : '设为常用（首页常用入口会用它）'"
        :data-favorite="app.favorite"
        @click="emit('favorite', app, !app.favorite)"
      >
        {{ app.favorite ? '★ 常用' : '☆ 常用' }}
      </NButton>
    </header>

    <p v-if="app.description !== ''" class="app-card__line">{{ app.description }}</p>

    <p class="app-card__line app-card__muted">
      {{ describeBinding(app.type, app.binding) }}
      <template v-if="app.public_url"> · 入口 <code class="app-card__url">{{ app.public_url }}</code></template>
      <template v-else> · 没登记入口地址</template>
    </p>

    <!-- 第 1 与第 2 条分开写：运行时在跑不代表入口是通的，反之亦然。 -->
    <p v-if="app.runtime_note" class="app-card__line app-card__muted">
      运行时：{{ app.runtime_note }}
    </p>
    <p
      v-if="app.dependency_protection_note"
      class="app-card__line app-card__muted"
      data-testid="app-protection-note"
    >
      依赖：{{ app.dependency_protection_note }}
    </p>
    <p class="app-card__line app-card__muted">
      健康检查：{{ app.health_check.enabled ? '已启用' : '未启用' }} · {{ checkedLine }}
    </p>

    <div class="app-card__buttons">
      <!-- 打开：真的链接（新标签、noopener），不是发一个请求的按钮。 -->
      <NButton
        v-if="opensInNewTab"
        size="small"
        type="primary"
        tag="a"
        :href="target.href"
        target="_blank"
        rel="noopener noreferrer"
        data-action="open"
      >
        {{ APP_ACTION_LABELS.open }}
      </NButton>
      <NButton
        v-else-if="primary !== null"
        size="small"
        type="primary"
        :loading="busy === true"
        :data-action="primary"
        @click="onPrimary"
      >
        {{ APP_ACTION_LABELS[primary] }}
      </NButton>

      <NButton
        v-for="action in lifecycle"
        :key="action"
        size="small"
        secondary
        :disabled="busy === true"
        :data-action="action"
        @click="emit('control', app, action)"
      >
        {{ APP_ACTION_LABELS[action] }}
      </NButton>

      <NButton v-if="canLogs" size="small" secondary data-action="logs" @click="emit('logs', app)">
        {{ APP_ACTION_LABELS.logs }}
      </NButton>

      <span v-if="actions.length === 0" class="app-card__muted">
        这个应用此刻没有可执行的动作（没有运行时或对象不在本机）。
      </span>

      <span class="app-card__spacer" />

      <template v-if="canAct">
        <NButton size="tiny" quaternary data-action="edit" @click="emit('edit', app)">编辑</NButton>
        <NButton size="tiny" quaternary data-action="remove" @click="emit('remove', app)">删除</NButton>
      </template>
    </div>

    <p v-if="target.href === '' && app.public_url !== null" class="app-card__line app-card__muted">
      {{ target.reason }}
    </p>
  </article>
</template>

<style scoped>
.app-card {
  display: flex;
  flex-direction: column;
  gap: 6px;
  padding: 12px 14px;
  border: 1px solid rgba(128, 128, 128, 0.25);
  border-radius: 10px;
  background: rgba(127, 127, 127, 0.04);
}

.app-card__head {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 8px;
}

.app-card__icon {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 26px;
  height: 26px;
  border-radius: 7px;
  background: rgba(127, 127, 127, 0.16);
  font-size: 15px;
  line-height: 1;
}

.app-card__name {
  margin: 0;
  font-size: 15px;
  font-weight: 700;
  word-break: break-all;
}

.app-card__line {
  margin: 0;
  line-height: 1.6;
  word-break: break-word;
}

.app-card__muted {
  font-size: 12px;
  opacity: 0.78;
}

.app-card__url {
  word-break: break-all;
}

.app-card__buttons {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 8px;
  margin-top: 2px;
}

.app-card__spacer {
  flex: 1 1 auto;
}
</style>
