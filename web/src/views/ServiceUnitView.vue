<script setup lang="ts">
// 单元详情（P05.1）。状态只读；启停/重启/开机启动走预检 → 任务。
//
// 三条纪律，任何一条破了都会变成“面板说自己做不到的事”：
//   1) 按钮只按服务端给的 capabilities 渲染——受保护/模板/not-found 的单元没有按钮；
//   2) 点了按钮先拿预检，把阻塞项、告警与影响摊开；被拒的预检不出现提交按钮；
//   3) 提交后的状态只信任务：queued/running/succeeded/failed…，不把“点了按钮”
//      当成“操作成功”。成功之后重新读取一次单元状态，显示读回来的值。

import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { NAlert, NButton, NCard, NTag } from 'naive-ui'
import { RouterLink, useRoute } from 'vue-router'

import { ApiError } from '@/api/client'
import { isTerminal, JOB_STATUS_LABELS, JOB_STATUS_TYPES, type JobStatus } from '@/api/jobs'
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
  SERVICE_OPERATION_LABELS,
  fetchService,
  isValidUnitName,
  type ServiceOperation,
  type ServiceUnit,
} from '@/api/services'
import ApiErrorPanel from '@/components/ApiErrorPanel.vue'
import ServiceActionConfirm from '@/components/ServiceActionConfirm.vue'
import ServiceUnitPanel from '@/components/ServiceUnitPanel.vue'
import StatePanel from '@/components/StatePanel.vue'
import { useSessionStore } from '@/stores/session'
import { createPoller, type Poller } from '@/utils/polling'
import { apiPanelState, retryablePanelState } from '@/utils/viewstate'

type Phase = 'loading' | 'ready' | 'missing' | 'invalid' | 'error'

const DEFAULT_INTERVAL_MS = 10000
/** 任务轮询间隔与上限：一次 systemctl 最多 90 秒，这里留 120 秒。 */
const JOB_POLL_MS = 1000
const JOB_POLL_LIMIT = 120

const route = useRoute()
const session = useSessionStore()

const unitName = computed(() => {
  const raw = route.params.unit
  return Array.isArray(raw) ? (raw[0] ?? '') : (raw ?? '')
})

const phase = ref<Phase>('loading')
const error = ref<ApiError | null>(null)
const unit = ref<ServiceUnit | null>(null)

const activeOp = ref<ServiceOperation | null>(null)
const preflight = ref<PreflightData | null>(null)
const actionBusy = ref(false)
const actionError = ref<string | null>(null)
const idempotencyKey = ref('')

const jobId = ref<string | null>(null)
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

watch(preflight, (value) => {
  if (value === null) {
    stopTicker()
    return
  }
  nowMs.value = Date.now()
  startTicker()
})

const panelState = computed(() => (error.value ? apiPanelState(error.value) : null))
const availableOperations = computed<ServiceOperation[]>(() =>
  unit.value ? [...unit.value.capabilities] : [],
)

let disposed = false
const sleep = (ms: number): Promise<void> =>
  new Promise((resolve) => {
    setTimeout(resolve, ms)
  })

async function load(): Promise<void> {
  const name = unitName.value
  if (name === '') {
    phase.value = 'missing'
    return
  }
  if (!isValidUnitName(name)) {
    phase.value = 'invalid'
    return
  }
  try {
    const result = await fetchService(name)
    // 期间切换了单元：这次结果属于上一个，丢掉。
    if (unitName.value !== name) return
    unit.value = result.data.unit
    phase.value = 'ready'
    error.value = null
  } catch (caught) {
    if (caught instanceof ApiError) {
      if (unitName.value !== name) return
      error.value = caught
      phase.value = caught.code === 'NOT_FOUND' ? 'missing' : 'error'
      return
    }
    throw caught
  }
}

const poller: Poller = createPoller(load, {
  intervalMs: () => DEFAULT_INTERVAL_MS,
})

function resetAction(): void {
  activeOp.value = null
  preflight.value = null
  actionError.value = null
  idempotencyKey.value = ''
  jobId.value = null
  jobStatus.value = null
  jobStep.value = ''
  jobMessage.value = null
  jobOutcome.value = null
}

async function beginAction(op: ServiceOperation): Promise<void> {
  resetAction()
  activeOp.value = op
  actionBusy.value = true
  // 幂等键在“一次意图”开始时生成，提交重试复用它。
  idempotencyKey.value = newIdempotencyKey()
  try {
    const result = await createPreflight('service.control', {
      unit: unitName.value,
      operation: op,
    })
    preflight.value = result.data
  } catch (caught) {
    actionError.value = errorText(caught)
  } finally {
    actionBusy.value = false
  }
}

async function confirmAction(phrase: string): Promise<void> {
  const current = preflight.value
  if (current === null) return
  actionBusy.value = true
  actionError.value = null
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
        applyJobResult(result.data.status, result.data.result, result.data.error)
        await load()
        return
      }
    } catch (caught) {
      // 读任务失败不等于任务失败：保留“未知”，让人去任务中心看。
      jobMessage.value = errorText(caught)
      return
    }
    await sleep(JOB_POLL_MS)
  }
  jobMessage.value = '任务在 120 秒内没有到达终态；请到任务中心查看最新状态。'
}

function applyJobResult(
  status: JobStatus,
  result: Record<string, unknown>,
  jobError: Record<string, unknown>,
): void {
  if (status !== 'succeeded') {
    const message = typeof jobError['message'] === 'string' ? jobError['message'] : '任务失败'
    jobMessage.value = message
    return
  }
  const observed = result['observed_state']
  if (observed !== null && typeof observed === 'object') {
    const state = observed as Record<string, unknown>
    const active = typeof state['active_state'] === 'string' ? state['active_state'] : ''
    const enablement = typeof state['enablement'] === 'string' ? state['enablement'] : ''
    jobOutcome.value = `执行后读回：运行状态 ${active || '未知'}，开机启动 ${enablement || '未知'}`
  } else {
    jobOutcome.value = '任务已成功；重新读取了单元状态。'
  }
}

function errorText(caught: unknown): string {
  if (caught instanceof ApiError) return caught.message
  if (caught instanceof RangeError) return caught.message
  return '请求失败，请稍后重试'
}

watch(unitName, () => {
  unit.value = null
  phase.value = 'loading'
  resetAction()
  poller.trigger()
})

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
  <section class="unit">
    <div class="unit__head">
      <RouterLink to="/services">← 返回服务列表</RouterLink>
      <h1 class="unit__title">{{ unitName === '' ? '服务单元' : unitName }}</h1>
      <NTag v-if="jobStatus" size="small" :type="JOB_STATUS_TYPES[jobStatus]">
        任务：{{ JOB_STATUS_LABELS[jobStatus] }}
      </NTag>
      <NButton size="small" secondary @click="poller.trigger()">刷新</NButton>
    </div>

    <StatePanel v-if="phase === 'loading'" state="loading" title="正在读取单元状态" />
    <StatePanel
      v-else-if="phase === 'invalid'"
      state="empty"
      title="单元名不合法"
      description="服务单元名必须以 .service 结尾，只允许字母、数字与 : _ . @ -。"
    />
    <StatePanel
      v-else-if="phase === 'missing'"
      state="empty"
      title="这台机器上没有这个单元"
      description="单元可能被卸载、改名，或者名字拼错了。服务列表里能看到当前存在的单元。"
    />
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

    <template v-else-if="unit">
      <ServiceUnitPanel :unit="unit" />

      <NCard size="small" title="操作">
        <template v-if="session.isAdmin">
          <p v-if="availableOperations.length === 0" class="unit__line unit__muted">
            这个单元当前没有可执行的动作（原因见上方“可执行动作”一行的说明）。
          </p>
          <div v-else class="unit__buttons">
            <NButton
              v-for="op in availableOperations"
              :key="op"
              size="small"
              :type="op === 'stop' || op === 'restart' ? 'warning' : 'default'"
              :disabled="activeOp !== null && activeOp !== op"
              @click="beginAction(op)"
            >
              {{ SERVICE_OPERATION_LABELS[op] }}
            </NButton>
          </div>
          <p class="unit__line unit__muted">
            每个操作都会先生成预检：显示影响与阻塞项后才可提交；提交后由任务执行并读回状态。
          </p>
        </template>
        <p v-else class="unit__line unit__muted">
          当前角色是只读（viewer）：可以查看状态，启停与开机启动需要管理员。
        </p>
      </NCard>

      <ServiceActionConfirm
        v-if="activeOp"
        :unit="unitName"
        :operation="activeOp"
        :preflight="preflight"
        :busy="actionBusy"
        :error-message="actionError"
        :expired="preflightExpired"
        :remaining-seconds="preflightRemaining"
        @confirm="confirmAction"
        @cancel="resetAction"
      />

      <NCard v-if="jobId" size="small" title="任务">
        <p class="unit__line">
          任务号 <code>{{ jobId }}</code>
          <span v-if="jobStatus">
            · {{ JOB_STATUS_LABELS[jobStatus] }}<span v-if="jobStep">（步骤 {{ jobStep }}）</span>
          </span>
        </p>
        <NAlert v-if="jobOutcome" type="success" :bordered="false">{{ jobOutcome }}</NAlert>
        <NAlert v-if="jobMessage" type="warning" :bordered="false">{{ jobMessage }}</NAlert>
        <p class="unit__line unit__muted">
          任务的完整事件与脱敏日志在
          <RouterLink to="/jobs">任务中心</RouterLink>。
        </p>
      </NCard>
    </template>
  </section>
</template>

<style scoped>
.unit {
  display: flex;
  flex-direction: column;
  gap: var(--sp-3);
  /* 不锁宽度：右侧留一大片空白看起来像没渲染完。长句子由 __line 的 78ch 限宽管住。 */
}

.unit__muted {
  font-size: var(--fs-sm);
  color: var(--ink-3);
}

.unit__buttons {
  display: flex;
  flex-wrap: wrap;
  gap: 8px;
}
</style>
