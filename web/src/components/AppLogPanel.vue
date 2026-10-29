<script setup lang="ts">
// 一个应用背后的日志（P09.5，接口是 P09.4 的 /apps/{id}/logs）。
//
// 这里刻意不发明新的读取能力：页面上看到的就是 `/logs`（journal）或
// `/containers/{id}/logs`（docker）的同一页，只是**对象来自应用的绑定**。
// 两件事因此必须显式写出来，否则会看错：
//   1. 来源是哪一边（journal 的 unit 还是 docker 的 container_id）；
//   2. 这一页是截断过的、还是翻到了头。
//
// 两个来源的参数集合不通用：journal 认 level/cursor/limit，docker 认 tail/cursor。
// 界面按来源只显示对应的筛选器，而不是发一个服务端会 400 的参数组合。

import { computed, ref, watch } from 'vue'
import { NAlert, NButton, NModal, NSelect, NTag } from 'naive-ui'

import {
  getAppLogs,
  type AppLogsData,
  type ContainerLogEntry,
} from '@/api/apps'
import { ApiError } from '@/api/client'
import {
  DEFAULT_LOG_LIMIT,
  LOG_LEVELS,
  LOG_LEVEL_LABELS,
  summarizeLogPage,
  type LogEntry,
} from '@/api/logs'
import ApiErrorPanel from '@/components/ApiErrorPanel.vue'
import LogEntryRow from '@/components/LogEntryRow.vue'
import StatePanel from '@/components/StatePanel.vue'
import { formatTimestamp } from '@/utils/format'
import { apiPanelState, retryablePanelState } from '@/utils/viewstate'

const props = defineProps<{
  show: boolean
  appId: string
  appName: string
}>()

const emit = defineEmits<{ 'update:show': [boolean] }>()

type Phase = 'loading' | 'ready' | 'error'

const phase = ref<Phase>('loading')
const error = ref<ApiError | null>(null)
const data = ref<AppLogsData | null>(null)
const level = ref('')
const limit = ref(DEFAULT_LOG_LIMIT)
/** 已加载的前几页（按“更早”顺序追加）。 */
const older = ref<AppLogsData[]>([])

const panelState = computed(() => (error.value ? apiPanelState(error.value) : null))

const pages = computed<AppLogsData[]>(() => {
  const first = data.value
  return first === null ? [] : [first, ...older.value]
})

const journalPages = computed(() => {
  const list = pages.value.filter((page) => page.source === 'journal')
  if (list.length === 0) return []
  return list as Extract<AppLogsData, { source: 'journal' }>[]
})

const dockerPages = computed(() => {
  const list = pages.value.filter((page) => page.source === 'docker')
  if (list.length === 0) return []
  return list as Extract<AppLogsData, { source: 'docker' }>[]
})

const journalEntries = computed<LogEntry[]>(() =>
  journalPages.value.flatMap((page) => page.page.entries ?? []),
)

const containerEntries = computed<ContainerLogEntry[]>(() =>
  dockerPages.value.flatMap((page) => page.page.entries ?? []),
)

/** 最后一页的游标：null 表示没有更早的日志了。 */
const nextCursor = computed<string | null>(() => {
  const last = pages.value[pages.value.length - 1]
  return last?.page.next_cursor ?? null
})

const journalSummary = computed(() => summarizeLogPage(journalPages.value[0]?.page))
const dockerTruncated = computed(() => dockerPages.value.some((page) => page.page.truncated))
const streamsMerged = computed(() => dockerPages.value.some((page) => page.page.streams_merged))

const levelOptions = computed(() => [
  { label: '全部级别', value: '' },
  ...LOG_LEVELS.map((value) => ({ label: LOG_LEVEL_LABELS[value] ?? value, value })),
])

const limitOptions = [50, 100, 200, 500].map((value) => ({ label: `${value} 条`, value }))

const target = computed(() => {
  const first = pages.value[0]
  if (!first) return ''
  return first.source === 'journal' ? `单元 ${first.unit}` : `容器 ${first.container_id.slice(0, 12)}`
})

async function load(): Promise<void> {
  phase.value = 'loading'
  error.value = null
  older.value = []
  try {
    const result = await getAppLogs(props.appId, {
      limit: limit.value,
      ...(level.value === '' ? {} : { level: level.value }),
    })
    data.value = result.data
    phase.value = 'ready'
  } catch (caught) {
    if (caught instanceof ApiError) {
      error.value = caught
      phase.value = 'error'
      return
    }
    throw caught
  }
}

/** 往前翻一页：同一次打开里游标只往前走，不跳页。 */
async function loadOlder(): Promise<void> {
  const cursor = nextCursor.value
  if (cursor === null) return
  try {
    const result = await getAppLogs(props.appId, {
      cursor,
      limit: limit.value,
      ...(data.value?.source === 'journal' && level.value !== '' ? { level: level.value } : {}),
    })
    older.value = [...older.value, result.data]
  } catch (caught) {
    if (caught instanceof ApiError) {
      error.value = caught
      return
    }
    throw caught
  }
}

function onShowChange(value: boolean): void {
  emit('update:show', value)
}

/** 打开时（以及换了应用/筛选之后）重新读第一页。 */
watch(
  () => [props.show, props.appId, level.value, limit.value] as const,
  ([show]) => {
    if (show) void load()
    else {
      data.value = null
      older.value = []
      phase.value = 'loading'
    }
  },
  { immediate: true },
)
</script>

<template>
  <NModal
    :show="show"
    preset="card"
    :title="`「${appName}」的日志`"
    class="app-logs"
    style="width: min(960px, 94vw)"
    @update:show="onShowChange"
  >
    <div class="app-logs__body" :data-source="data?.source ?? ''">
      <div class="app-logs__filters">
        <NTag v-if="target !== ''" size="small">{{ target }}</NTag>
        <NSelect
          v-if="data?.source !== 'docker'"
          v-model:value="level"
          :options="levelOptions"
          size="small"
          class="app-logs__select"
        />
        <NSelect v-model:value="limit" :options="limitOptions" size="small" class="app-logs__select" />
        <NButton size="small" secondary :loading="phase === 'loading'" @click="load">重新读取</NButton>
      </div>

      <p class="app-logs__muted">
        对象来自应用的绑定，不来自请求：页面上没有“按任意单元名读日志”的入口。
        <template v-if="data?.source === 'docker'">
          stdout 与 stderr 是合并读出的，同一时刻的先后不保证。
        </template>
      </p>

      <StatePanel v-if="phase === 'loading'" state="loading" title="正在读取日志" />
      <StatePanel
        v-else-if="panelState && error && panelState !== 'error'"
        :state="panelState"
        :description="error.message"
        :code="error.code"
        :request-id="error.requestId"
        :retryable="retryablePanelState(panelState)"
        @retry="load"
      />
      <ApiErrorPanel v-else-if="error" :error="error" @retry="load" />

      <template v-else>
        <NAlert v-if="journalSummary.truncated" type="warning" :bordered="false">
          这一页被截断：{{ journalSummary.truncatedReason === '' ? '达到单页上限' : journalSummary.truncatedReason }}。
          截断过的页面不是“这段时间的全部日志”。
        </NAlert>
        <NAlert v-if="dockerTruncated || streamsMerged" type="warning" :bordered="false">
          <template v-if="dockerTruncated">这一页被容器日志的读取上限截断。</template>
          <template v-if="streamsMerged"> stdout 与 stderr 已合并。</template>
        </NAlert>

        <StatePanel
          v-if="journalEntries.length === 0 && containerEntries.length === 0"
          state="empty"
          title="这段时间没有日志"
          description="空列表不等于“没问题”：它只说明这个对象在这段窗口里没写出任何一条。"
        />

        <div v-else class="app-logs__list" data-testid="app-log-list">
          <LogEntryRow v-for="entry in journalEntries" :key="entry.cursor" :entry="entry" />
          <div
            v-for="(entry, index) in containerEntries"
            :key="`${entry.timestamp}-${index}`"
            class="app-logs__container-entry"
            :data-truncated="entry.truncated"
          >
            <div class="app-logs__container-meta">
              <span class="app-logs__time">{{ formatTimestamp(entry.timestamp) }}</span>
              <NTag v-if="entry.redacted" size="small" type="warning">已过滤</NTag>
              <NTag v-if="entry.truncated" size="small" type="warning">已截断</NTag>
            </div>
            <pre class="app-logs__message">{{ entry.message }}</pre>
          </div>
        </div>

        <p v-if="journalEntries.length > 0" class="app-logs__muted">
          这一页显示 {{ journalSummary.shown }} 条，扫描 {{ journalSummary.scanned }} 条；
          其中被过滤 {{ journalSummary.redacted }} 条、被截断 {{ journalSummary.truncatedEntries }} 条、
          无文本 {{ journalSummary.binary }} 条。
        </p>

        <div class="app-logs__footer">
          <NButton v-if="nextCursor !== null" size="small" secondary @click="loadOlder">更早的日志</NButton>
          <span v-else class="app-logs__muted">没有更早的日志了。</span>
        </div>
      </template>
    </div>
  </NModal>
</template>

<style scoped>
.app-logs__body {
  display: flex;
  flex-direction: column;
  gap: 10px;
  max-height: 70vh;
  overflow: auto;
}

.app-logs__filters {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 8px;
}

.app-logs__select {
  width: 150px;
}

.app-logs__muted {
  margin: 0;
  font-size: var(--fs-sm);
  line-height: 1.6;
  opacity: 0.78;
}

.app-logs__list {
  display: flex;
  flex-direction: column;
  border: 1px solid var(--rule);
  border-radius: var(--r-2);
  overflow: hidden;
}

.app-logs__container-entry {
  display: flex;
  flex-direction: column;
  gap: 4px;
  padding: 8px 10px;
  border-bottom: 1px solid var(--rule);
}

.app-logs__container-meta {
  display: flex;
  align-items: center;
  gap: 8px;
  font-size: var(--fs-sm);
}

.app-logs__time {
  font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
}

.app-logs__message {
  margin: 0;
  font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
  font-size: var(--fs-md);
  line-height: 1.6;
  white-space: pre-wrap;
  word-break: break-word;
}

.app-logs__footer {
  display: flex;
  align-items: center;
  gap: 10px;
}
</style>
