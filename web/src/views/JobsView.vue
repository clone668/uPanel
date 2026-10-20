<script setup lang="ts">
// 任务中心（P03.3）。
//
// 这个页面的全部数据都来自服务端：任务来自 GET /jobs，详情来自 GET /jobs/:id，
// 实时进度来自 SSE /jobs/:id/events，日志页来自 GET /jobs/:id/logs。
// 没有演示数据、没有“假进度条”，拿不到就显示拿不到。
//
// 状态机按 CONTRACTS.md 4.3：终态不再变化；状态以**重新拉取详情**为准，
// 事件流只负责“该刷新了”和“有一行日志”这两种信号。理由：事件载荷是历史，
// 任务视图是结论，页面显示的结论必须来自权威接口。

import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import {
  NAlert,
  NButton,
  NCard,
  NEmpty,
  NProgress,
  NSpace,
  NTable,
  NTag,
  NText,
} from 'naive-ui'
import type { DataTableColumns } from 'naive-ui'

import { ApiError } from '@/api/client'
import {
  canCancel,
  cancelJob,
  fetchJobLogs,
  formatTime,
  getJob,
  isTerminal,
  jobErrorMessage,
  listJobs,
  mergeLogs,
  progressText,
  trimLogs,
  JOB_STATUS_LABELS,
  JOB_STATUS_TYPES,
  type JobDTO,
  type JobLogEntry,
} from '@/api/jobs'
import { openJobStream, type JobStreamHandle, type JobStreamState } from '@/api/jobstream'
import ApiErrorPanel from '@/components/ApiErrorPanel.vue'
import StatePanel from '@/components/StatePanel.vue'
import { useSessionStore } from '@/stores/session'

const session = useSessionStore()

type Phase = 'loading' | 'ready' | 'error'

const phase = ref<Phase>('loading')
const listError = ref<ApiError | null>(null)
const jobs = ref<JobDTO[]>([])
const selectedId = ref<string | null>(null)
const detail = ref<JobDTO | null>(null)
const detailPhase = ref<Phase>('loading')
const detailError = ref<ApiError | null>(null)
const cancelError = ref<ApiError | null>(null)
const cancelling = ref(false)
const actionMessage = ref('')

const logs = ref<JobLogEntry[]>([])
const logCursor = ref<string | null>(null)
const logPhase = ref<Phase>('loading')
const logError = ref<ApiError | null>(null)
const loadingLogs = ref(false)
const streamState = ref<JobStreamState>('closed')
const lastEventSeq = ref(0)
const invalidFrames = ref(0)

let stream: JobStreamHandle | null = null
let listTimer: ReturnType<typeof setInterval> | null = null

const selectedJob = computed<JobDTO | null>(() => detail.value)
const showCancel = computed(() =>
  selectedJob.value ? canCancel(selectedJob.value, session.role) : false,
)
const streamLabel = computed(() => {
  if (!session.isAdmin) return '不订阅（仅管理员可见事件流）'
  switch (streamState.value) {
    case 'open':
      return `实时接收中（最近 seq ${lastEventSeq.value}）`
    case 'connecting':
      return '正在连接事件流…'
    case 'reconnecting':
      return '连接中断，正在按 Last-Event-ID 重连…'
    default:
      return '未订阅'
  }
})
const statusText = computed(() =>
  selectedJob.value ? JOB_STATUS_LABELS[selectedJob.value.status] : '',
)
const statusType = computed(() =>
  selectedJob.value ? JOB_STATUS_TYPES[selectedJob.value.status] : 'default',
)
const errorDetail = computed(() => (selectedJob.value ? jobErrorMessage(selectedJob.value) : null))

const columns: DataTableColumns<JobDTO> = [
  {
    title: '动作',
    key: 'action',
    minWidth: 160,
  },
  {
    title: '状态',
    key: 'status',
    width: 140,
    render: (row) => `${JOB_STATUS_LABELS[row.status]}`,
  },
  { title: '步骤', key: 'step', width: 140 },
  {
    title: '进度',
    key: 'progress_percent',
    width: 100,
    render: (row) => progressText(row),
  },
  {
    title: '创建时间',
    key: 'created_at',
    width: 180,
    render: (row) => formatTime(row.created_at),
  },
  {
    title: '结束时间',
    key: 'finished_at',
    width: 180,
    render: (row) => formatTime(row.finished_at),
  },
]

/** 拉列表。失败必须显式进入错误态：静默保留旧列表会让人以为服务还在。 */
async function loadJobs(options: { quiet?: boolean } = {}): Promise<void> {
  if (!options.quiet) phase.value = 'loading'
  try {
    const result = await listJobs(200)
    jobs.value = result.data.jobs
    phase.value = 'ready'
    listError.value = null
  } catch (caught) {
    if (caught instanceof ApiError) {
      listError.value = caught
      phase.value = 'error'
      return
    }
    throw caught
  }
}

/** 拉详情：事件流的每一轮“该刷新了”都落到这里。 */
async function loadDetail(jobId: string, options: { quiet?: boolean } = {}): Promise<void> {
  if (!options.quiet) detailPhase.value = 'loading'
  try {
    const result = await getJob(jobId)
    if (selectedId.value !== jobId) return
    detail.value = result.data
    detailPhase.value = 'ready'
    detailError.value = null
    // 列表里的那一条也同步更新，避免列表和详情显示两个状态。
    const index = jobs.value.findIndex((job) => job.job_id === jobId)
    if (index >= 0) jobs.value.splice(index, 1, result.data)
  } catch (caught) {
    if (caught instanceof ApiError) {
      detailError.value = caught
      detailPhase.value = 'error'
      return
    }
    throw caught
  }
}

async function loadLogs(jobId: string, options: { cursor?: string | null; append?: boolean } = {}) {
  loadingLogs.value = true
  if (!options.append) logPhase.value = 'loading'
  try {
    const result = await fetchJobLogs(jobId, { cursor: options.cursor ?? null, limit: 100 })
    if (selectedId.value !== jobId) return
    logs.value = trimLogs(
      options.append ? mergeLogs(logs.value, result.data.logs) : result.data.logs,
    )
    logCursor.value = result.data.next_cursor
    logPhase.value = 'ready'
    logError.value = null
  } catch (caught) {
    if (caught instanceof ApiError) {
      logError.value = caught
      logPhase.value = 'error'
      return
    }
    throw caught
  } finally {
    loadingLogs.value = false
  }
}

function stopStream(): void {
  stream?.close()
  stream = null
  streamState.value = 'closed'
}

/** 订阅事件流。只有管理员能订阅（契约里 /events 与 /logs 是 admin）。 */
function startStream(jobId: string): void {
  stopStream()
  lastEventSeq.value = 0
  invalidFrames.value = 0
  if (!session.isAdmin) return
  stream = openJobStream(jobId, {
    onState: (state) => {
      streamState.value = state
    },
    onInvalidFrame: () => {
      invalidFrames.value += 1
    },
    onEvent: (event) => {
      lastEventSeq.value = Math.max(lastEventSeq.value, event.seq)
      if (event.kind === 'gap') {
        // 历史已被清理：断点补发已经不可能，重新拉一遍状态与第一页日志。
        logs.value = []
        logCursor.value = null
        void loadDetail(jobId, { quiet: true })
        void loadLogs(jobId)
        stream?.restart()
        return
      }
      if (event.kind === 'log') {
        logs.value = trimLogs(mergeLogs(logs.value, [event]))
        return
      }
      if (event.kind === 'status') {
        // 状态事件只是“结论变了”的提示；结论本身去权威接口取。
        void loadDetail(jobId, { quiet: true })
        return
      }
      // progress：本地不推算百分比，等下一轮详情把服务端的值带回来。
      if (detail.value && detail.value.status !== 'running') {
        void loadDetail(jobId, { quiet: true })
      }
    },
  })
}

function select(jobId: string): void {
  if (selectedId.value === jobId) return
  selectedId.value = jobId
  detail.value = jobs.value.find((job) => job.job_id === jobId) ?? null
  logs.value = []
  logCursor.value = null
  actionMessage.value = ''
  cancelError.value = null
  void loadDetail(jobId)
  if (session.isAdmin) void loadLogs(jobId)
  startStream(jobId)
}

async function requestCancel(): Promise<void> {
  const job = selectedJob.value
  if (!job || cancelling.value) return
  cancelling.value = true
  cancelError.value = null
  actionMessage.value = ''
  try {
    const result = await cancelJob(job.job_id)
    detail.value = result.data
    actionMessage.value =
      result.data.status === 'canceled'
        ? '任务已取消。'
        : '已请求取消：代理会在安全检查点停止，结果以下一次刷新为准。'
  } catch (caught) {
    if (caught instanceof ApiError) {
      cancelError.value = caught
      return
    }
    throw caught
  } finally {
    cancelling.value = false
  }
}

watch(
  () => selectedJob.value?.status,
  (status) => {
    // 终态的任务不需要继续收事件：关掉连接，别占着服务端的并发名额。
    if (status && isTerminal(status)) stopStream()
  },
)

onMounted(() => {
  void loadJobs()
  // 列表 5 秒轮询一次：任务状态是后台推进的，不刷新就看不到变化。
  listTimer = setInterval(() => {
    void loadJobs({ quiet: true })
  }, 5000)
})

onBeforeUnmount(() => {
  if (listTimer !== null) clearInterval(listTimer)
  listTimer = null
  stopStream()
})
</script>

<template>
  <section class="jobs">
    <NCard title="任务中心" size="small">
      <template #header-extra>
        <NSpace align="center" :size="8">
          <NTag v-if="!session.isAdmin" size="small" type="info">viewer：只读</NTag>
          <NButton size="small" secondary :loading="phase === 'loading'" @click="loadJobs()">
            刷新
          </NButton>
        </NSpace>
      </template>

      <p class="jobs__lead">
        任务由预检后的动作提交产生。这里显示真实的任务状态、事件流与脱敏日志；
        没有任务时列表为空，不显示演示数据。
      </p>

      <StatePanel v-if="phase === 'loading'" state="loading" title="正在读取任务列表" />
      <ApiErrorPanel v-else-if="phase === 'error' && listError" :error="listError" @retry="loadJobs" />
      <NEmpty v-else-if="jobs.length === 0" description="暂无任务：提交一个动作后，任务会出现在这里" />
      <NTable
        v-else
        :columns="columns"
        :data="jobs"
        :row-props="(row: JobDTO) => ({ style: 'cursor: pointer', onClick: () => select(row.job_id) })"
        :row-key="(row: JobDTO) => row.job_id"
        size="small"
        :single-line="false"
        data-testid="jobs-table"
      />
    </NCard>

    <NCard v-if="selectedId" title="任务详情" size="small">
      <StatePanel v-if="detailPhase === 'loading' && !detail" state="loading" title="正在读取任务详情" />
      <ApiErrorPanel
        v-else-if="detailPhase === 'error' && detailError"
        :error="detailError"
        @retry="() => loadDetail(selectedId!)"
      />

      <div v-else-if="detail" class="jobs__detail" data-testid="job-detail">
        <p class="jobs__row">
          <span class="jobs__label">任务</span>
          <code>{{ detail.job_id }}</code>
        </p>
        <p class="jobs__row">
          <span class="jobs__label">动作</span>
          <span>{{ detail.action }}</span>
        </p>
        <p class="jobs__row">
          <span class="jobs__label">状态</span>
          <NTag size="small" :type="statusType">{{ statusText }}</NTag>
          <NText v-if="detail.cancellation_requested" depth="3" class="jobs__meta">
            已请求取消，等待代理在安全检查点停止
          </NText>
        </p>
        <p class="jobs__row">
          <span class="jobs__label">步骤</span>
          <span>{{ detail.step === '' ? '—' : detail.step }}</span>
        </p>
        <p class="jobs__row">
          <span class="jobs__label">进度</span>
          <span v-if="detail.progress_percent === null">未上报</span>
          <NProgress
            v-else
            class="jobs__progress"
            type="line"
            :percentage="detail.progress_percent"
            :height="10"
          />
        </p>
        <p class="jobs__row">
          <span class="jobs__label">时间</span>
          <span>
            创建 {{ formatTime(detail.created_at) }} · 开始 {{ formatTime(detail.started_at) }} ·
            结束 {{ formatTime(detail.finished_at) }}
          </span>
        </p>
        <p class="jobs__row">
          <span class="jobs__label">事件流</span>
          <span data-testid="job-stream-state">{{ streamLabel }}</span>
          <NText v-if="invalidFrames > 0" depth="3" class="jobs__meta">
            已丢弃 {{ invalidFrames }} 帧格式不符的事件
          </NText>
        </p>

        <NAlert v-if="errorDetail" type="error" :bordered="false" data-testid="job-error">
          <p class="jobs__error-title">失败原因：{{ errorDetail.code }}</p>
          <p class="jobs__error-message">{{ errorDetail.message }}</p>
          <p v-if="errorDetail.hint" class="jobs__meta">{{ errorDetail.hint }}</p>
        </NAlert>

        <NAlert v-if="actionMessage" type="success" :bordered="false">
          {{ actionMessage }}
        </NAlert>
        <ApiErrorPanel v-if="cancelError" :error="cancelError" @retry="requestCancel" />

        <div v-if="showCancel" class="jobs__actions">
          <NButton type="warning" secondary :loading="cancelling" @click="requestCancel">
            请求取消
          </NButton>
          <NText depth="3" class="jobs__meta">取消是在安全检查点停止，不是立刻杀进程。</NText>
        </div>
        <p v-else-if="detail.cancellable && !isTerminal(detail.status)" class="jobs__meta">
          只有管理员可以请求取消。
        </p>

        <section v-if="detail.result && Object.keys(detail.result).length > 0" class="jobs__block">
          <h3 class="jobs__heading">结果</h3>
          <pre class="jobs__pre">{{ JSON.stringify(detail.result, null, 2) }}</pre>
        </section>

        <section v-if="session.isAdmin" class="jobs__block">
          <h3 class="jobs__heading">日志（脱敏，最多保留最近 1000 行）</h3>
          <StatePanel v-if="logPhase === 'loading'" state="loading" title="正在读取日志" />
          <ApiErrorPanel
            v-else-if="logPhase === 'error' && logError"
            :error="logError"
            @retry="() => loadLogs(selectedId!, {})"
          />
          <p v-else-if="logs.length === 0" class="jobs__meta">这个任务还没有日志行。</p>
          <template v-else>
            <ul class="jobs__logs" data-testid="job-logs">
              <li v-for="entry in logs" :key="entry.seq">
                <span class="jobs__log-meta">#{{ entry.seq }} {{ formatTime(entry.created_at) }}</span>
                <code class="jobs__log-body">{{ JSON.stringify(entry.payload) }}</code>
              </li>
            </ul>
            <NButton
              v-if="logCursor"
              size="small"
              secondary
              :loading="loadingLogs"
              @click="() => loadLogs(selectedId!, { cursor: logCursor, append: true })"
            >
              加载更多
            </NButton>
          </template>
        </section>
        <p v-else class="jobs__meta">事件流与日志按契约仅管理员可见，viewer 只能看任务摘要。</p>
      </div>
    </NCard>
  </section>
</template>

<style scoped>
.jobs {
  display: flex;
  flex-direction: column;
  gap: 16px;
  max-width: 1100px;
}

.jobs__lead {
  margin: 0 0 12px;
  line-height: 1.7;
  font-size: 13px;
  opacity: 0.85;
}

.jobs__detail {
  display: flex;
  flex-direction: column;
  gap: 10px;
}

.jobs__row {
  display: flex;
  align-items: center;
  gap: 10px;
  margin: 0;
  line-height: 1.7;
  flex-wrap: wrap;
}

.jobs__label {
  min-width: 56px;
  opacity: 0.7;
  font-size: 13px;
}

.jobs__progress {
  max-width: 320px;
}

.jobs__meta {
  font-size: 12px;
  opacity: 0.75;
}

.jobs__error-title {
  margin: 0;
  font-weight: 600;
}

.jobs__error-message {
  margin: 4px 0 0;
  word-break: break-word;
}

.jobs__actions {
  display: flex;
  align-items: center;
  gap: 12px;
}

.jobs__block {
  display: flex;
  flex-direction: column;
  gap: 8px;
  margin-top: 8px;
}

.jobs__heading {
  margin: 0;
  font-size: 14px;
}

.jobs__pre {
  margin: 0;
  padding: 10px 12px;
  background: rgba(127, 127, 127, 0.1);
  border-radius: 6px;
  font-size: 12px;
  max-height: 240px;
  overflow: auto;
}

.jobs__logs {
  margin: 0;
  padding: 0;
  list-style: none;
  display: flex;
  flex-direction: column;
  gap: 4px;
  max-height: 320px;
  overflow: auto;
}

.jobs__log-meta {
  font-size: 11px;
  opacity: 0.65;
  margin-right: 8px;
}

.jobs__log-body {
  font-size: 12px;
  word-break: break-all;
}
</style>
