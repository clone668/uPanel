<script setup lang="ts">
// 存储写动作的确认框（P07.5）：九个动作共用这一个。
//
// 它只做一件事：把“服务端说会发生什么”原样摊开，然后让人自己决定。
// 顺序是 **影响 → 阻塞项 → 再认证 → 短语 → 提交 → 现场读数**：
//   - 阻塞项非空时按钮根本不出现，因为服务端已经判了不行；
//   - 再认证窗口由服务端策略决定（requires_reauth），密码只走 /auth/reauth，
//     不进任务请求体；
//   - 确认短语是服务端生成的一次性口令，这个组件**不生成**短语，只判断输对了没；
//   - 成功之后不写一句“操作成功”就完事，而是把 result.observed_state 里的
//     现场读数（写进 fstab 的那一行、备份路径、checked_via）摊出来——那才是证据。
//
// 没有第二条执行路径：这里调的仍然是 POST /operations/preflight + POST /jobs。

import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { NAlert, NButton, NInput, NModal, NTag } from 'naive-ui'

import { reauthPassword } from '@/api/auth'
import { ApiError } from '@/api/client'
import { JOB_STATUS_LABELS, JOB_STATUS_TYPES, isTerminal, type JobStatus } from '@/api/jobs'
import {
  createPreflight,
  fetchJobStatus,
  isPreflightExpired,
  newIdempotencyKey,
  preflightRemainingSeconds,
  requiresConfirmation,
  submitJob,
  type PreflightData,
} from '@/api/preflights'
import { useSessionStore } from '@/stores/session'
import { summarizeStorageJob, type JobOutcomeSummary } from '@/utils/jobresult'
import { formatTimestamp } from '@/utils/format'

const props = defineProps<{
  show: boolean
  /** 动作名（storage.* 下的九个之一）。 */
  action: string
  /** 对话框标题，由调用方给出（「挂载数据卷」这类具体说法）。 */
  title: string
  /** 已经拼好的动作参数；null 表示表单还没填好，此时不预检。 */
  params: Record<string, unknown> | null
  /** 提交前给人看的“你即将做什么”，由表单给出，不参与请求。 */
  confirmLines?: string[]
  /** 是否提醒这一次是不可逆的（初始化分区表 / 格式化 / 删分区）。 */
  irreversible?: boolean
  /** 这个动作需要 5 分钟内再认证（界面提示；真正的拦截在服务端）。 */
  needsReauth?: boolean
}>()

const emit = defineEmits<{ 'update:show': [boolean]; finished: [JobStatus] }>()

const JOB_POLL_MS = 1000
const JOB_POLL_LIMIT = 120

const session = useSessionStore()

const phase = ref<'idle' | 'preflight' | 'submitting' | 'tracking' | 'done'>('idle')
const preflight = ref<PreflightData | null>(null)
const actionError = ref<string | null>(null)
const phrase = ref('')
const password = ref('')
const jobStatus = ref<JobStatus | null>(null)
const jobStep = ref('')
const outcome = ref<JobOutcomeSummary | null>(null)
const idempotencyKey = ref('')
const now = ref(new Date())
let timer: ReturnType<typeof setInterval> | null = null
let disposed = false

const expired = computed(() => (preflight.value ? isPreflightExpired(preflight.value, now.value) : false))
const remaining = computed(() => (preflight.value ? preflightRemainingSeconds(preflight.value, now.value) : null))
const blocked = computed(() => (preflight.value ? !preflight.value.allowed : true))
const phraseNeeded = computed(() => (preflight.value ? requiresConfirmation(preflight.value) : false))
const phraseMatches = computed(
  () => !phraseNeeded.value || phrase.value === preflight.value?.confirmation_phrase,
)
const reauthNotice = computed(
  () => props.needsReauth === true || preflight.value?.requires_reauth === true,
)
const busy = computed(() => phase.value === 'preflight' || phase.value === 'submitting' || phase.value === 'tracking')
const canSubmit = computed(
  () =>
    preflight.value !== null &&
    preflight.value.allowed &&
    phraseMatches.value &&
    !busy.value &&
    !expired.value,
)
const jobTagType = computed(() => (jobStatus.value === null ? 'default' : JOB_STATUS_TYPES[jobStatus.value]))

function errorText(caught: unknown): string {
  if (caught instanceof ApiError) return caught.message
  if (caught instanceof RangeError) return caught.message
  return '请求失败，请稍后重试'
}

function stopTimer(): void {
  if (timer !== null) {
    clearInterval(timer)
    timer = null
  }
}

function reset(): void {
  phase.value = 'idle'
  preflight.value = null
  actionError.value = null
  phrase.value = ''
  password.value = ''
  jobStatus.value = null
  jobStep.value = ''
  outcome.value = null
  idempotencyKey.value = ''
  stopTimer()
}

async function runPreflight(): Promise<void> {
  const params = props.params
  if (params === null) return
  reset()
  phase.value = 'preflight'
  // 幂等键在“一次意图”开始时生成，提交重试复用它（CONTRACTS.md 4.2）。
  idempotencyKey.value = newIdempotencyKey()
  try {
    const result = await createPreflight(props.action, params)
    if (disposed) return
    preflight.value = result.data
    phase.value = 'idle'
    now.value = new Date()
    stopTimer()
    timer = setInterval(() => {
      now.value = new Date()
    }, 1000)
  } catch (caught) {
    if (disposed) return
    actionError.value = errorText(caught)
    phase.value = 'idle'
  }
}

async function submit(): Promise<void> {
  const current = preflight.value
  if (current === null) return
  phase.value = 'submitting'
  actionError.value = null
  try {
    // 再认证是独立的一步：密码只发给 /auth/reauth，绝不进任务请求。
    if (reauthNotice.value && password.value !== '') {
      await reauthPassword(password.value)
      await session.restore()
      password.value = ''
    }
    const submitted = await submitJob({
      preflightId: current.preflight_id,
      ...(phrase.value === '' ? {} : { confirmationPhrase: phrase.value }),
      idempotencyKey: idempotencyKey.value,
    })
    jobStatus.value = 'queued'
    phase.value = 'tracking'
    await track(submitted.data.job_id)
  } catch (caught) {
    if (disposed) return
    const error = caught instanceof ApiError ? caught : null
    if (error?.code === 'REAUTH_REQUIRED') {
      actionError.value = '需要 5 分钟内的密码再认证：请在下面输入密码后重试（预检 120 秒内仍有效）'
    } else if (error?.code === 'PREFLIGHT_STALE' || error?.code === 'DEVICE_IDENTITY_CHANGED') {
      actionError.value = `${error.message}——现场已经变了，请重新预检。`
    } else {
      actionError.value = errorText(caught)
    }
    phase.value = 'idle'
  }
}

async function track(jobId: string): Promise<void> {
  for (let attempt = 0; attempt < JOB_POLL_LIMIT; attempt += 1) {
    if (disposed) return
    try {
      const result = await fetchJobStatus(jobId)
      if (disposed) return
      jobStatus.value = result.data.status
      jobStep.value = result.data.step
      if (isTerminal(result.data.status)) {
        outcome.value = summarizeStorageJob(result.data)
        phase.value = 'done'
        stopTimer()
        emit('finished', result.data.status)
        return
      }
    } catch (caught) {
      if (disposed) return
      // 读任务失败不等于任务失败：保留“未知”，让人去任务中心看。
      actionError.value = `${errorText(caught)}（任务 ${jobId} 仍在执行，请到任务中心核对）`
      phase.value = 'done'
      stopTimer()
      return
    }
    await new Promise((resolve) => setTimeout(resolve, JOB_POLL_MS))
  }
  actionError.value = `任务在 ${(JOB_POLL_LIMIT * JOB_POLL_MS) / 1000} 秒内没有到达终态；请到任务中心查看最新状态。`
  phase.value = 'done'
  stopTimer()
}

function close(): void {
  emit('update:show', false)
}

function onShowChange(value: boolean): void {
  emit('update:show', value)
  if (value && props.params !== null) {
    void runPreflight()
  }
  if (!value) stopTimer()
}

// 参数变了（表单改了）就重新预检：预检绑定的是参数与现场，换了参数必须重算。
watch(
  () => [props.action, JSON.stringify(props.params ?? null)] as const,
  () => {
    if (props.show && props.params !== null) void runPreflight()
  },
)

onBeforeUnmount(() => {
  disposed = true
  stopTimer()
})
</script>

<template>
  <NModal
    :show="show"
    preset="card"
    :title="title"
    class="dialog"
    :mask-closable="!busy"
    @update:show="onShowChange"
  >
    <div class="dialog__body" :data-action="action">
      <ul v-if="confirmLines && confirmLines.length > 0" class="dialog__list">
        <li v-for="line in confirmLines" :key="line">{{ line }}</li>
      </ul>

      <p v-if="irreversible" class="dialog__warn">
        这一步写下去就找不回来：盘上原有的数据会被覆盖或丢弃，面板不提供撤销。
      </p>

      <p v-if="phase === 'preflight'" class="dialog__muted">正在生成预检计划…</p>

      <NAlert v-if="actionError" type="error" :title="actionError" />

      <template v-if="preflight">
        <p class="dialog__muted">
          目标：{{ preflight.target.display_name }} · 预检到期
          {{ formatTimestamp(preflight.expires_at) }}
          <template v-if="remaining !== null && !expired">（还剩 {{ remaining }} 秒）</template>
        </p>

        <NAlert v-if="expired" type="warning" :bordered="false">
          预检已过期（有效期 120 秒）。预检绑定的是生成那一刻的状态，过期后必须重新生成。
        </NAlert>

        <NAlert v-if="blocked" type="warning" title="预检不允许执行这个操作" :bordered="false">
          <ul class="dialog__list">
            <li v-for="blocker in preflight.blockers" :key="blocker.code + blocker.message">
              <code>{{ blocker.code }}</code> {{ blocker.message }}
            </li>
          </ul>
        </NAlert>

        <div v-if="preflight.impacts.length > 0" class="dialog__block">
          <p class="dialog__label">会影响什么</p>
          <ul class="dialog__list">
            <li v-for="impact in preflight.impacts" :key="impact.kind + impact.description">
              <NTag size="small">{{ impact.kind }}</NTag> {{ impact.description }}
            </li>
          </ul>
        </div>

        <div v-if="preflight.warnings.length > 0" class="dialog__block">
          <p class="dialog__label">告警</p>
          <ul class="dialog__list">
            <li v-for="warning in preflight.warnings" :key="warning.code + warning.message">
              <code>{{ warning.code }}</code> {{ warning.message }}
            </li>
          </ul>
        </div>

        <p v-if="preflight.locks.length > 0" class="dialog__muted">
          资源锁：<code v-for="lock in preflight.locks" :key="lock">{{ lock }}</code>
        </p>

        <div v-if="reauthNotice" class="dialog__block">
          <p class="dialog__label">这次操作需要密码再认证（5 分钟窗口）</p>
          <NInput
            v-model:value="password"
            type="password"
            placeholder="当前账号密码"
            :disabled="busy"
            data-testid="storage-reauth"
          />
        </div>

        <div v-if="phraseNeeded" class="dialog__block">
          <p class="dialog__label">请输入确认短语（区分大小写）</p>
          <p class="dialog__muted">短语由服务端生成、只在这次预检里出现一次：{{ preflight.confirmation_phrase }}</p>
          <NInput
            v-model:value="phrase"
            placeholder="照抄上面的短语"
            :disabled="busy"
            data-testid="storage-phrase"
          />
          <p v-if="!phraseMatches" class="dialog__warn">短语不一致，提交会被拒绝（400 CONFIRMATION_MISMATCH）。</p>
        </div>
      </template>

      <template v-if="jobStatus !== null">
        <p class="dialog__label">
          任务状态
          <NTag size="small" :type="jobTagType">{{ JOB_STATUS_LABELS[jobStatus] }}</NTag>
          <span v-if="jobStep !== ''" class="dialog__muted">（{{ jobStep }}）</span>
        </p>
        <template v-if="outcome">
          <p class="dialog__line">{{ outcome.headline }}</p>
          <ul class="dialog__list" data-testid="storage-job-evidence">
            <li v-for="line in outcome.lines" :key="line">{{ line }}</li>
          </ul>
          <p v-if="outcome.hint" class="dialog__muted">建议：{{ outcome.hint }}</p>
        </template>
      </template>

      <div class="dialog__actions">
        <NButton v-if="phase !== 'done'" secondary :disabled="busy" @click="close">取消</NButton>
        <NButton v-if="phase !== 'done'" secondary :disabled="busy" @click="runPreflight">
          重新预检
        </NButton>
        <NButton
          v-if="phase !== 'done'"
          type="primary"
          :disabled="!canSubmit"
          :data-allowed="preflight?.allowed ?? false"
          @click="submit"
        >
          提交
        </NButton>
        <NButton v-if="phase === 'done'" type="primary" @click="close">关闭</NButton>
      </div>
    </div>
  </NModal>
</template>

<style scoped>
.dialog__body {
  display: flex;
  flex-direction: column;
  gap: 10px;
}

.dialog__line {
  margin: 0;
  line-height: 1.7;
}

.dialog__list {
  margin: 0;
  padding-left: 18px;
  line-height: 1.7;
}

.dialog__label {
  margin: 0;
  font-weight: 600;
}

.dialog__muted {
  margin: 0;
  font-size: 13px;
  opacity: 0.8;
  word-break: break-all;
}

.dialog__warn {
  margin: 0;
  line-height: 1.7;
  color: #d03050;
}

.dialog__block {
  display: flex;
  flex-direction: column;
  gap: 6px;
}

.dialog__actions {
  display: flex;
  justify-content: flex-end;
  gap: 8px;
}
</style>
