<script setup lang="ts">
// 进程列表（P05.2）与终止入口。
//
// 数据来自 GET /processes 的一份快照：代理每 5 秒采集一次，页面按 interval_ms 刷新。
// 显示规则与验收条款逐条对应：
//   - 身份是 pid + starttime：表格里两个值都显示，终止请求也两个都带；
//   - 受保护进程（PID 1 / 内核线程 / 面板与代理自身 / 受保护系统服务）照常显示，
//     但服务端给它们的 capabilities 是空数组，界面因此不给按钮，只说明原因；
//   - 已退出（zombie/dead）的进程同样没有按钮：杀一个僵尸不会让它更快被回收；
//   - TERM 与 KILL 是两个按钮、两次预检、两条任务。界面上没有任何“失败后自动
//     升级为 KILL”的开关，KILL 额外要求再认证与确认短语；
//   - 筛选是客户端过滤：processes.list 不接受任何参数。
//
// 按钮只按服务端的 capabilities 渲染，界面不自己推“这个应该能杀”。

import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { NAlert, NButton, NCard, NInput, NModal, NTag } from 'naive-ui'
import { RouterLink } from 'vue-router'

import { ApiError } from '@/api/client'
import {
  JOB_STATUS_LABELS,
  JOB_STATUS_TYPES,
  isTerminal,
  jobErrorMessage,
  type JobDTO,
  type JobStatus,
} from '@/api/jobs'
import {
  createPreflight,
  fetchJobStatus,
  isPreflightExpired,
  newIdempotencyKey,
  preflightRemainingSeconds,
  submitJob,
  type PreflightData,
} from '@/api/preflights'
import {
  SIGNAL_LABELS,
  SIGNAL_VERBS,
  fetchProcesses,
  filterProcesses,
  formatCPUPercent,
  isExited,
  processProtectionLabel,
  processStateLabel,
  processStateType,
  sameProcess,
  signalParams,
  summarizeProcesses,
  type ProcessEntry,
  type ProcessListData,
  type ProcessSignal,
} from '@/api/processes'
import ApiErrorPanel from '@/components/ApiErrorPanel.vue'
import ProcessSignalConfirm from '@/components/ProcessSignalConfirm.vue'
import StatePanel from '@/components/StatePanel.vue'
import { useSessionStore } from '@/stores/session'
import { bytesTitle, formatBytes, formatTimestamp, formatSampleSummary } from '@/utils/format'
import { createPoller, type Poller } from '@/utils/polling'
import { apiPanelState, retryablePanelState } from '@/utils/viewstate'

type Phase = 'loading' | 'ready' | 'error'

const DEFAULT_INTERVAL_MS = 5000
/** 一次 systemctl/kill 最多 30 秒，这里留 60 秒。 */
const JOB_POLL_MS = 1000
const JOB_POLL_LIMIT = 60
/**
 * 单次渲染的行数上限。
 *
 * 这不是“快照上限”（那是服务端的 2000）：列表可能真有 2000 个进程，但把 2000 行
 * 一次性塞进 DOM 只会让页面卡住。超出部分明确写出来，让人用筛选缩小范围，
 * 而不是悄悄少显示。
 */
const RENDER_LIMIT = 200

const session = useSessionStore()

const phase = ref<Phase>('loading')
const error = ref<ApiError | null>(null)
const data = ref<ProcessListData | null>(null)
const keyword = ref('')

const activeTarget = ref<{ process: ProcessEntry; signal: ProcessSignal } | null>(null)
const preflight = ref<PreflightData | null>(null)
const actionBusy = ref(false)
const actionError = ref<string | null>(null)
const idempotencyKey = ref('')

const jobId = ref<string | null>(null)
const jobLabel = ref('')
const jobStatus = ref<JobStatus | null>(null)
const jobStep = ref('')
const jobMessage = ref<string | null>(null)
const jobOutcome = ref<string | null>(null)

// 预检有效期是 120 秒：按秒刷新，过期就不给提交按钮（服务端也会拒）。
const nowMs = ref(Date.now())
let ticker: number | null = null

const preflightExpired = computed(() =>
  preflight.value === null ? false : isPreflightExpired(preflight.value, new Date(nowMs.value)),
)
const preflightRemaining = computed(() =>
  preflight.value === null ? null : preflightRemainingSeconds(preflight.value, new Date(nowMs.value)),
)

function startTicker(): void {
  if (typeof window === 'undefined' || ticker !== null) return
  ticker = window.setInterval(() => {
    nowMs.value = Date.now()
  }, 1000)
}

function stopTicker(): void {
  if (ticker !== null) {
    clearInterval(ticker)
    ticker = null
  }
}

const panelState = computed(() => (error.value ? apiPanelState(error.value) : null))
const processes = computed<ProcessEntry[]>(() => data.value?.processes ?? [])
const counts = computed(() => summarizeProcesses(processes.value))
const filtered = computed(() => filterProcesses(processes.value, keyword.value))
const rows = computed(() => filtered.value.slice(0, RENDER_LIMIT))
const hiddenByLimit = computed(() => Math.max(0, filtered.value.length - RENDER_LIMIT))
const sampleLine = computed(() => {
  const list = data.value
  if (!list) return ''
  return `${formatSampleSummary(list.collected_at, list.interval_ms)} · 共 ${list.total} 个进程`
})
/** 目标是否还在最新快照里。不在不代表一定死了（快照可能落后），所以只提示不拦。 */
const targetPresent = computed(() => {
  const target = activeTarget.value
  if (target === null) return true
  return processes.value.some((process) => sameProcess(process, target.process))
})

let disposed = false
const sleep = (ms: number): Promise<void> =>
  new Promise((resolve) => {
    setTimeout(resolve, ms)
  })

async function load(): Promise<void> {
  try {
    const result = await fetchProcesses()
    data.value = result.data
    phase.value = 'ready'
    error.value = null
  } catch (caught) {
    if (caught instanceof ApiError) {
      error.value = caught
      phase.value = 'error'
      return
    }
    throw caught
  }
}

const poller: Poller = createPoller(load, {
  intervalMs: () => data.value?.interval_ms ?? DEFAULT_INTERVAL_MS,
})

function resetAction(): void {
  activeTarget.value = null
  preflight.value = null
  actionError.value = null
  idempotencyKey.value = ''
  stopTicker()
}

function clearJob(): void {
  jobId.value = null
  jobLabel.value = ''
  jobStatus.value = null
  jobStep.value = ''
  jobMessage.value = null
  jobOutcome.value = null
}

async function beginSignal(process: ProcessEntry, signal: ProcessSignal): Promise<void> {
  clearJob()
  resetAction()
  activeTarget.value = { process, signal }
  actionBusy.value = true
  // 幂等键在“一次意图”开始时生成，提交重试复用它。
  idempotencyKey.value = newIdempotencyKey()
  try {
    const result = await createPreflight('process.signal', signalParams(process, signal))
    preflight.value = result.data
    nowMs.value = Date.now()
    startTicker()
  } catch (caught) {
    actionError.value = errorText(caught)
  } finally {
    actionBusy.value = false
  }
}

async function confirmSignal(phrase: string): Promise<void> {
  const current = preflight.value
  const target = activeTarget.value
  if (current === null || target === null) return
  actionBusy.value = true
  actionError.value = null
  jobLabel.value = `${SIGNAL_VERBS[target.signal]} PID ${String(target.process.pid)}`
  try {
    const submitted = await submitJob({
      preflightId: current.preflight_id,
      ...(phrase === '' ? {} : { confirmationPhrase: phrase }),
      idempotencyKey: idempotencyKey.value,
    })
    jobId.value = submitted.data.job_id
    jobStatus.value = 'queued'
    await trackJob(submitted.data.job_id)
  } catch (caught) {
    actionError.value = errorText(caught)
  } finally {
    actionBusy.value = false
  }
}

/** 轮询任务直到终态。终态是唯一的“结论”，中间状态只用来显示进度。 */
async function trackJob(id: string): Promise<void> {
  for (let attempt = 0; attempt < JOB_POLL_LIMIT; attempt += 1) {
    if (disposed) return
    try {
      const result = await fetchJobStatus(id)
      jobStatus.value = result.data.status
      jobStep.value = result.data.step
      if (isTerminal(result.data.status)) {
        applyJobResult(result.data)
        // 结果已经落到页面上的任务卡片里，确认卡功成身退。
        resetAction()
        poller.trigger()
        return
      }
    } catch (caught) {
      // 读任务失败不等于任务失败：保留“未知”，让人去任务中心看。
      jobMessage.value = errorText(caught)
      resetAction()
      return
    }
    await sleep(JOB_POLL_MS)
  }
  jobMessage.value = '任务在 60 秒内没有到达终态；请到任务中心查看最新状态。'
  resetAction()
}

function applyJobResult(job: JobDTO): void {
  if (job.status !== 'succeeded') {
    const detail = jobErrorMessage(job)
    const message = detail?.message ?? '任务失败'
    jobMessage.value = detail?.hint ? `${message}（${detail.hint}）` : message
    return
  }
  const observed = job.result['observed_state']
  if (observed !== null && typeof observed === 'object') {
    const state = observed as Record<string, unknown>
    const after = typeof state['state_after'] === 'string' ? state['state_after'] : ''
    const gone = state['gone'] === true
    jobOutcome.value = gone
      ? `执行后读回：这个 PID 上已经没有任何进程（信号送达并确认退出${after === '' ? '' : `，最后状态 ${processStateLabel(after)}`}）。`
      : `执行后读回：进程仍在，当前状态 ${after === '' ? '未知' : processStateLabel(after)}。`
  } else {
    jobOutcome.value = '任务已成功；重新读取了进程列表。'
  }
}

function errorText(caught: unknown): string {
  if (caught instanceof ApiError) return caught.message
  if (caught instanceof RangeError) return caught.message
  return '请求失败，请稍后重试'
}

function onModalShow(show: boolean): void {
  if (!show && !actionBusy.value) resetAction()
}

function protectionTitle(process: ProcessEntry): string | undefined {
  return process.protection.reasons.length === 0 ? undefined : process.protection.reasons.join('；')
}

onMounted(() => {
  poller.start()
})

onBeforeUnmount(() => {
  disposed = true
  stopTicker()
  poller.stop()
})
</script>

<template>
  <section class="procs">
    <div class="procs__head">
      <h1 class="procs__title">进程</h1>
      <NTag v-if="data?.status === 'error'" size="small" type="warning">采集失败</NTag>
      <NTag v-if="data?.truncated === true" size="small" type="warning">列表被截断</NTag>
      <NTag v-if="counts.protected > 0" size="small" type="info">
        {{ counts.protected }} 个受保护
      </NTag>
      <RouterLink class="procs__link" to="/services">服务单元 →</RouterLink>
    </div>
    <p v-if="sampleLine" class="procs__meta">{{ sampleLine }}</p>

    <NInput
      v-model:value="keyword"
      clearable
      placeholder="按 PID、属主、命令摘要或单元名筛选（只过滤当前快照）"
    />

    <StatePanel v-if="phase === 'loading'" state="loading" title="正在读取进程列表" />
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

    <template v-else-if="data">
      <NAlert v-if="data.status === 'error'" type="warning" :bordered="false">
        最近一次采集失败：{{ data.error ?? '代理没有给出原因' }}。下面显示的是<strong>上一次成功</strong>
        的快照（{{ formatTimestamp(data.collected_at) }}），进程可能已经变化。
      </NAlert>

      <NAlert v-if="data.truncated" type="warning" :bordered="false">
        这台机器的进程数超过了单次快照上限（2000 个），列表被截断；未显示的部分不代表不存在。
      </NAlert>

      <NCard v-if="jobId" size="small" :title="`任务：${jobLabel}`">
        <p class="procs__line">
          任务号 <code>{{ jobId }}</code>
          <NTag v-if="jobStatus" size="small" :type="JOB_STATUS_TYPES[jobStatus]">
            {{ JOB_STATUS_LABELS[jobStatus] }}<template v-if="jobStep">（步骤 {{ jobStep }}）</template>
          </NTag>
        </p>
        <NAlert v-if="jobOutcome" type="success" :bordered="false">{{ jobOutcome }}</NAlert>
        <NAlert v-if="jobMessage" type="warning" :bordered="false">{{ jobMessage }}</NAlert>
        <p class="procs__line procs__meta">
          完整事件与脱敏日志在 <RouterLink to="/jobs">任务中心</RouterLink>。
        </p>
      </NCard>

      <NCard size="small" title="概览">
        <p class="procs__line">
          共 {{ counts.total }} 个进程：运行中 {{ counts.running }} · 睡眠 {{ counts.sleeping }} ·
          已退出 {{ counts.exited }} · 受保护 {{ counts.protected }} · 可发送信号
          {{ counts.killable }}。
        </p>
        <p class="procs__line procs__meta">
          终止分成两步：<strong>请求退出</strong>（SIGTERM，给进程清理的机会）与
          <strong>强制结束</strong>（SIGKILL，立即终止）。两者各自走一次预检与一次任务，面板不会在
          请求退出超时后自动升级为强制结束。目标身份是 PID + starttime：同一个 PID 换了 starttime
          就是另一个进程，旧预检会因此被拒绝。
          <template v-if="!session.isAdmin">当前角色是只读（viewer），只能查看进程列表。</template>
        </p>
      </NCard>

      <StatePanel
        v-if="processes.length === 0"
        state="empty"
        title="没有读到任何进程"
        description="代理尚未成功采集 /proc，或者这台机器上真的一无所获；稍后会自动刷新。"
      />
      <StatePanel
        v-else-if="filtered.length === 0"
        state="empty"
        title="没有匹配的进程"
        description="换一个关键字试试；筛选只作用于当前快照，不会把条件传给代理。"
      />

      <template v-else>
        <p v-if="hiddenByLimit > 0" class="procs__line procs__meta">
          当前筛选命中 {{ filtered.length }} 个进程，表格只渲染前 {{ RENDER_LIMIT }} 个（还有
          {{ hiddenByLimit }} 个未渲染）。用筛选缩小范围即可看到其余部分。
        </p>

        <div class="procs__table-wrap">
          <table class="procs__table" data-testid="processes-table">
            <thead>
              <tr>
                <th scope="col">PID / starttime</th>
                <th scope="col">命令</th>
                <th scope="col">属主</th>
                <th scope="col">状态</th>
                <th scope="col">CPU</th>
                <th scope="col">内存</th>
                <th scope="col">单元</th>
                <th scope="col">保护</th>
                <th v-if="session.isAdmin" scope="col">操作</th>
              </tr>
            </thead>
            <tbody>
              <tr
                v-for="process in rows"
                :key="`${process.pid}:${process.start_time_ticks}`"
                :data-pid="process.pid"
                :data-start-time-ticks="process.start_time_ticks"
              >
                <td class="procs__num">
                  <code>{{ process.pid }}</code>
                  <span class="procs__sub" :title="`starttime ${process.start_time_ticks} 滴答（开机以来）`">
                    {{ process.start_time_ticks }}
                  </span>
                </td>
                <td class="procs__cmd" :title="process.command">{{ process.command }}</td>
                <td>{{ process.owner }}</td>
                <td>
                  <NTag size="small" :type="processStateType(process.state)">
                    {{ processStateLabel(process.state) }}
                  </NTag>
                </td>
                <td class="procs__num">{{ formatCPUPercent(process.cpu_percent) }}</td>
                <td class="procs__num" :title="bytesTitle(process.memory_bytes)">
                  {{ formatBytes(process.memory_bytes) }}
                </td>
                <td class="procs__unit">{{ process.unit === '' ? '—' : process.unit }}</td>
                <td>
                  <NTag
                    v-if="process.protection.protected"
                    size="small"
                    type="warning"
                    :title="protectionTitle(process)"
                  >
                    {{ processProtectionLabel(process.protection.category) }}
                  </NTag>
                  <span v-else class="procs__sub">—</span>
                </td>
                <td v-if="session.isAdmin" class="procs__actions">
                  <NButton
                    v-for="signal in process.capabilities"
                    :key="signal"
                    size="tiny"
                    :secondary="signal === 'TERM'"
                    :type="signal === 'KILL' ? 'error' : 'default'"
                    :title="SIGNAL_LABELS[signal]"
                    :disabled="activeTarget !== null && actionBusy"
                    @click="beginSignal(process, signal)"
                  >
                    {{ SIGNAL_VERBS[signal] }}
                  </NButton>
                  <span v-if="process.capabilities.length === 0" class="procs__sub">
                    {{ isExited(process) ? '已退出' : '不可操作' }}
                  </span>
                </td>
              </tr>
            </tbody>
          </table>
        </div>
      </template>
    </template>

    <NModal
      :show="activeTarget !== null"
      preset="card"
      :title="activeTarget ? `${SIGNAL_VERBS[activeTarget.signal]} PID ${activeTarget.process.pid}` : '确认'"
      class="procs__modal"
      :mask-closable="false"
      :close-on-esc="!actionBusy"
      @update:show="onModalShow"
    >
      <div v-if="activeTarget" class="procs__modal-body">
        <NAlert v-if="!targetPresent" type="warning" :bordered="false">
          这个进程已经不在最新快照里。快照可能落后于实际状态，提交时由服务端重新核对身份；
          如果它确实已经退出，操作会被拒绝。
        </NAlert>
        <ProcessSignalConfirm
          :process="activeTarget.process"
          :signal="activeTarget.signal"
          :preflight="preflight"
          :busy="actionBusy"
          :error-message="actionError"
          :expired="preflightExpired"
          :remaining-seconds="preflightRemaining"
          @confirm="confirmSignal"
          @cancel="resetAction"
        />
        <p class="procs__line procs__meta">
          提交后状态只看任务：{{ jobStatus ? JOB_STATUS_LABELS[jobStatus] : '尚未提交' }}。
        </p>
      </div>
    </NModal>
  </section>
</template>

<style scoped>
.procs {
  display: flex;
  flex-direction: column;
  gap: 16px;
  max-width: 1200px;
}

.procs__head {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 10px;
}

.procs__title {
  margin: 0;
  font-size: 20px;
}

.procs__link {
  margin-left: auto;
  font-size: 13px;
}

.procs__line {
  margin: 0;
  line-height: 1.7;
}

.procs__meta {
  font-size: 13px;
  opacity: 0.8;
  word-break: break-all;
}

.procs__table-wrap {
  overflow-x: auto;
  border: 1px solid rgba(128, 128, 128, 0.25);
  border-radius: 10px;
}

.procs__table {
  width: 100%;
  border-collapse: collapse;
  font-size: 13px;
}

.procs__table th,
.procs__table td {
  padding: 6px 10px;
  text-align: left;
  border-bottom: 1px solid rgba(128, 128, 128, 0.16);
  vertical-align: top;
}

.procs__table th {
  position: sticky;
  top: 0;
  background: rgba(128, 128, 128, 0.1);
  font-weight: 600;
  white-space: nowrap;
}

.procs__num {
  white-space: nowrap;
  font-variant-numeric: tabular-nums;
}

.procs__sub {
  display: block;
  font-size: 11px;
  opacity: 0.65;
}

.procs__cmd {
  max-width: 380px;
  word-break: break-all;
}

.procs__unit {
  max-width: 200px;
  word-break: break-all;
  font-size: 12px;
}

.procs__actions {
  display: flex;
  flex-wrap: wrap;
  gap: 6px;
}

.procs__modal {
  max-width: 640px;
}

.procs__modal-body {
  display: flex;
  flex-direction: column;
  gap: 10px;
}
</style>
