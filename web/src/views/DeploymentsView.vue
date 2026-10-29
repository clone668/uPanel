<script setup lang="ts">
// 部署页（P15.4）：受管部署的列表、详情、新建、换镜像与删除。
//
// 这一页只读**管理面** `GET /deployments/managed`（`/etc/upanel/deploy` 下的三个文件 +
// systemd 的两个回答），不是观测面 `GET /deployments`（docker 报回来的 compose 项目，
// 那张表在应用页）。两张表按 `deployment_id` 连起来；一个项目可能只在观测面出现
// （有人在面板之外 `docker compose up` 起来的），那种东西这里不给它编造版本。
//
// 四条纪律：
//
//  1. **没有原始 Compose YAML、没有危险宿主挂载、没有特权选项**。这不是“暂时不支持”，
//     是这一层要达到的效果：定义是闭集（CONTRACTS §8.2），`privileged`、`devices`、
//     `network_mode`、`cap_add`、`compose_yaml` 在数据模型里没有落点，页面也就不给它们
//     留输入框。挂载只能选“受管理文件根 + 根内相对路径”，宿主绝对路径表达不出来。
//  2. **没有“直接部署”的按钮**。创建、更新、删除一律走预检 → 任务
//     （`deployment.apply` / `deployment.update` / `deployment.remove`）：先摊开阻塞项、
//     告警与影响，才轮到提交。
//  3. **版本令牌只能来自服务端**。更新与删除都带回列表里那份 `version`——它同时是
//     “你看到的是哪一份”的判据，页面不自己算摘要，也不拿配置文件名当版本。
//  4. **结论只信任务**。点了按钮不等于做成了；任务到终态之后重新读一遍现场，
//     把代理的现场读数摊开（含回切与部分成功——那两种既不是成功也不是失败）。

import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import {
  NAlert,
  NButton,
  NCard,
  NCheckbox,
  NInput,
  NInputNumber,
  NSelect,
  NSwitch,
  NTag,
} from 'naive-ui'
import { RouterLink } from 'vue-router'

import { fetchBackupPolicies, type BackupPolicy } from '@/api/backup'
import { reauthPassword } from '@/api/auth'
import { ApiError } from '@/api/client'
import {
  BLOCKED_CONTAINER_PATHS,
  DEPLOYMENT_PHASE_LABELS,
  MAX_DEPLOYMENT_NAME_LENGTH,
  applyParams,
  definitionFromDraft,
  deploymentPhase,
  deploymentPhaseDetail,
  describeDeploymentFiles,
  emptyDeploymentDraft,
  getManagedDeployment,
  listManagedDeployments,
  removeHint,
  removeParams,
  shortDigest,
  shortVersion,
  updateHint,
  updateParams,
  validateDeploymentDraft,
  type DeploymentDraft,
  type ManagedDeployment,
  type ManagedDeploymentListData,
} from '@/api/deployments'
import { fetchFileRoots, type FileRoot } from '@/api/files'
import { JOB_STATUS_LABELS, JOB_STATUS_TYPES, isTerminal, type JobStatus } from '@/api/jobs'
import {
  createPreflight,
  fetchJobStatus,
  isPreflightExpired,
  newIdempotencyKey,
  preflightRemainingSeconds,
  submitJob,
  submitTimeoutFor,
  type PreflightData,
} from '@/api/preflights'
import { fetchVolumes, type Volume } from '@/api/volumes'
import ApiErrorPanel from '@/components/ApiErrorPanel.vue'
import FileActionConfirm from '@/components/FileActionConfirm.vue'
import SecondFactorField from '@/components/SecondFactorField.vue'
import StatePanel from '@/components/StatePanel.vue'
import { useSessionStore } from '@/stores/session'
import { summarizeDeploymentJob, type JobOutcomeSummary } from '@/utils/jobresult'
import { createPoller, type Poller } from '@/utils/polling'
import { apiPanelState, retryablePanelState } from '@/utils/viewstate'

type Phase = 'loading' | 'ready' | 'error'

const DEFAULT_INTERVAL_MS = 20_000
/**
 * 任务轮询间隔与上限。
 *
 * 10 分钟不是随手取的：`deployment.update` 在代理侧的时间预算是 9 分钟（拉镜像、
 * 可选的更新前备份、写配置、健康探测都在里面）。用 120 秒会在一次慢速拉取上提前
 * 收手，把人推去任务中心看一条其实还在跑的任务。超过 10 分钟就明说“去任务中心看”，
 * 而不是继续无声地轮询。
 */
const JOB_POLL_MS = 2000
const JOB_POLL_LIMIT = 300

const session = useSessionStore()

// ---------------------------------------------------------------- 列表

const phase = ref<Phase>('loading')
const error = ref<ApiError | null>(null)
const deployments = ref<ManagedDeployment[]>([])
const skipped = ref<{ name: string; reason: string }[]>([])
const truncated = ref(false)

const panelState = computed(() => (error.value ? apiPanelState(error.value) : null))
const runningCount = computed(
  () => deployments.value.filter((item) => deploymentPhase(item) === 'running').length,
)
const driftCount = computed(() => deployments.value.filter((item) => item.config_drift).length)
const missingUnitCount = computed(
  () => deployments.value.filter((item) => deploymentPhase(item) === 'unit_missing').length,
)

async function load(): Promise<void> {
  try {
    const result = await listManagedDeployments()
    applyList(result.data)
    error.value = null
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

function applyList(data: ManagedDeploymentListData): void {
  deployments.value = data.deployments
  skipped.value = data.skipped ?? []
  truncated.value = data.truncated
}

const poller: Poller = createPoller(load, { intervalMs: () => DEFAULT_INTERVAL_MS })

// ---------------------------------------------------------------- 详情

const detailName = ref<string | null>(null)
const detail = ref<ManagedDeployment | null>(null)
const detailBusy = ref(false)
const detailError = ref<string | null>(null)

async function openDetail(name: string): Promise<void> {
  if (detailName.value === name) {
    detailName.value = null
    detail.value = null
    detailError.value = null
    return
  }
  detailName.value = name
  detailBusy.value = true
  detailError.value = null
  detail.value = null
  try {
    const result = await getManagedDeployment(name)
    if (detailName.value !== name) return
    detail.value = result.data
  } catch (caught) {
    if (detailName.value !== name) return
    detailError.value = errorText(caught)
  } finally {
    if (detailName.value === name) detailBusy.value = false
  }
}

async function reloadDetail(): Promise<void> {
  const name = detailName.value
  if (name === null) return
  detailName.value = null
  await openDetail(name)
}

// ---------------------------------------------------------------- 表单的选项来源

const roots = ref<FileRoot[]>([])
const volumes = ref<Volume[]>([])
const policies = ref<BackupPolicy[]>([])
const optionsError = ref('')

/**
 * 三个下拉的来源都是只读查询，任何一个失败都不该让部署页打不开：
 * 表单里那一栏空着，并写明“这一轮没读到”，而不是说“本机什么都没有”。
 */
async function loadOptions(): Promise<void> {
  if (!session.isAdmin) return
  const problems: string[] = []
  try {
    roots.value = (await fetchFileRoots()).data.roots
  } catch (caught) {
    roots.value = []
    problems.push(`文件根（${errorText(caught)}）`)
  }
  try {
    volumes.value = (await fetchVolumes()).data.volumes
  } catch (caught) {
    volumes.value = []
    problems.push(`受管理卷（${errorText(caught)}）`)
  }
  try {
    policies.value = (await fetchBackupPolicies()).data.items
  } catch (caught) {
    policies.value = []
    problems.push(`备份策略（${errorText(caught)}）`)
  }
  optionsError.value = problems.length === 0 ? '' : `${problems.join('、')}这一轮没读到`
}

/** 挂载源只能选“此刻可写且可读”的根：不可用的根会让部署起不来，但原因在别处。 */
const mountRootOptions = computed(() =>
  roots.value
    .filter((root) => root.writable)
    .map((root) => ({
      label: `${root.name} · ${root.path}${root.available ? '' : '（此刻读不到）'}`,
      value: root.root_id,
    })),
)

const volumeOptions = computed(() =>
  volumes.value.map((volume) => ({
    label: `${volume.display_name} · ${volume.mount_path}${volume.observed_state === 'mounted' ? '' : `（${volume.observed_state}）`}`,
    value: volume.id,
  })),
)

/**
 * 更新前备份只收 `files_only` 的策略：`stop_managed_apps` 需要 Web 侧展开“应用运行对象”，
 * 而 `deployment.update` 的参数里没有那东西，代理会在预检阶段就拒。这里先筛掉，
 * 免得好好的下拉里摆着一个必然被拒的选项。
 */
const backupPolicyOptions = computed(() =>
  policies.value
    .filter((policy) => policy.consistency_mode === 'files_only')
    .map((policy) => ({ label: policy.name, value: policy.id })),
)

// ---------------------------------------------------------------- 新建表单

const formOpen = ref(false)
const draft = ref<DeploymentDraft>(emptyDeploymentDraft())
const formProblem = computed(() => validateDeploymentDraft(draft.value))

function openCreate(): void {
  draft.value = emptyDeploymentDraft()
  formOpen.value = true
  resetAction()
}

function addEnvRow(): void {
  draft.value.environment.push({ name: '', kind: 'value', value: '', secret_ref: '' })
}

function removeEnvRow(index: number): void {
  draft.value.environment.splice(index, 1)
}

function addMountRow(): void {
  draft.value.mounts.push({
    source_root_id: mountRootOptions.value[0]?.value ?? '',
    relative_path: '',
    container_path: '',
    read_only: false,
  })
}

function removeMountRow(index: number): void {
  draft.value.mounts.splice(index, 1)
}

// ---------------------------------------------------------------- 更新

const updateTarget = ref<ManagedDeployment | null>(null)
const updateImage = ref('')
const updatePolicyId = ref('')

const updateHintText = computed(() =>
  updateTarget.value === null ? null : updateHint(updateTarget.value),
)

function openUpdate(deployment: ManagedDeployment): void {
  updateTarget.value = deployment
  updateImage.value = deployment.record?.image.requested ?? deployment.record?.definition.image_reference ?? ''
  updatePolicyId.value = ''
  resetAction()
}

// ---------------------------------------------------------------- 预检 → 任务

type ActionKind = 'apply' | 'update' | 'remove'

interface PendingAction {
  kind: ActionKind
  action: string
  label: string
  target: string
  detail: string
  danger: boolean
}

const pending = ref<PendingAction | null>(null)
const preflight = ref<PreflightData | null>(null)
const actionBusy = ref(false)
const actionError = ref<string | null>(null)
const idempotencyKey = ref('')

const jobId = ref<string | null>(null)
const jobStatus = ref<JobStatus | null>(null)
const jobStep = ref('')
const jobSummary = ref<JobOutcomeSummary | null>(null)
const jobNote = ref<string | null>(null)

// 预检有效期 120 秒：按秒刷新，过期就不给提交按钮（服务端也会拒）。
const nowMs = ref(Date.now())
let ticker: number | null = null

const preflightExpired = computed(() =>
  preflight.value === null ? false : isPreflightExpired(preflight.value, new Date(nowMs.value)),
)
const preflightRemaining = computed(() =>
  preflight.value === null ? null : preflightRemainingSeconds(preflight.value, new Date(nowMs.value)),
)

// ---------------------------------------------------------------- 再认证
//
// deployment.apply / update / remove 三条都是 CONTRACTS 第 5 节 R=是，而**登录本身不打开
// 再认证窗口**（只有 POST /auth/reauth 会把它推到 now+5 分钟）。这一页原先只把
// “这一步要求再认证”当提示写在确认卡上，于是真机上点确认必然拿到 403 REAUTH_REQUIRED——
// 页面链路走不通，而单测里没人点过按钮（P15.4 的未验证项就是这么留下来的）。
//
// 这里补的是**那一扇门本身**：预检说 requires_reauth 而窗口不在有效期内时，把密码
// （+二次验证码）发给 /auth/reauth，成功后自动接着把刚才那一步提交做完，人不用重填表单。
// 面板密码只走 /auth/reauth，不进任务参数、不进日志。
const reauthNeeded = ref(false)
const reauthInput = ref('')
const reauthCode = ref('')
const reauthBusy = ref(false)
const pendingPhrase = ref('')

const reauthValid = computed(() => {
  // 没验过的时候服务端给的 reauth_expires_at 是 null——那就是“窗口没开”，不是“不知道”。
  const until = session.reauthExpiresAt
  if (typeof until !== 'string' || until === '') return false
  return new Date(until).getTime() > nowMs.value
})

const reauthRequired = computed(() => preflight.value?.requires_reauth === true)
const reauthPrompt = computed(
  () => pending.value !== null && reauthRequired.value && (!reauthValid.value || reauthNeeded.value),
)

async function finishReauth(): Promise<void> {
  if (reauthInput.value === '' || reauthBusy.value) return
  reauthBusy.value = true
  actionError.value = null
  try {
    await reauthPassword(reauthInput.value, reauthCode.value === '' ? undefined : reauthCode.value)
    reauthInput.value = ''
    reauthCode.value = ''
    await session.restore()
    reauthNeeded.value = false
    // 接着把被打断的那一步做完：预检还在有效期内就一路提交，过期了服务端会拒，
    // 那句拒绝会原样显示出来，人重新生成预检即可。
    await confirmAction(pendingPhrase.value)
  } catch (cause) {
    actionError.value = cause instanceof ApiError ? cause.message : '再认证失败'
  } finally {
    reauthBusy.value = false
  }
}

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

function resetAction(): void {
  pending.value = null
  preflight.value = null
  actionError.value = null
  idempotencyKey.value = ''
  jobId.value = null
  jobStatus.value = null
  jobStep.value = ''
  jobSummary.value = null
  jobNote.value = null
  reauthNeeded.value = false
  reauthInput.value = ''
  reauthCode.value = ''
  pendingPhrase.value = ''
  stopTicker()
}

let disposed = false
const sleep = (ms: number): Promise<void> =>
  new Promise((resolve) => {
    setTimeout(resolve, ms)
  })

async function beginPreflight(input: PendingAction, params: Record<string, unknown>): Promise<void> {
  resetAction()
  pending.value = input
  actionBusy.value = true
  // 幂等键在“一次意图”开始时生成，提交重试复用它。
  idempotencyKey.value = newIdempotencyKey()
  try {
    const result = await createPreflight(input.action, params)
    preflight.value = result.data
    nowMs.value = Date.now()
    startTicker()
  } catch (caught) {
    actionError.value = errorText(caught)
  } finally {
    actionBusy.value = false
  }
}

function beginApply(): void {
  if (formProblem.value !== null) return
  const definition = definitionFromDraft(draft.value)
  // 契约规定：部署定义里没有 compose_yaml，也没有任何能改宿主的东西。这里多说一句
  // 是为了让确认卡上写清“这次要做的到底是什么”。
  void beginPreflight(
    {
      kind: 'apply',
      action: 'deployment.apply',
      label: '部署',
      target: definition.name,
      detail:
        `写入包装单元与渲染配置，并以 compose 项目 ${definition.name} 拉起容器；` +
        `端口 ${definition.host_bind_address}:${definition.host_port} → 容器 ${definition.container_port}`,
      danger: false,
    },
    applyParams(definition),
  )
}

function beginUpdate(): void {
  const target = updateTarget.value
  if (target === null) return
  void beginPreflight(
    {
      kind: 'update',
      action: 'deployment.update',
      label: '换镜像',
      target: target.name,
      detail:
        `把镜像从 ${target.record?.image.requested ?? '（记录里没有）'} 换成 ${updateImage.value.trim()}；` +
        '停单元 →（可选）备份 → 拉镜像 → 写新配置并 restart → 健康探测，探测不通就回切上一版',
      danger: false,
    },
    updateParams(target, updateImage.value, updatePolicyId.value === '' ? null : updatePolicyId.value),
  )
}

function beginRemove(target: ManagedDeployment): void {
  void beginPreflight(
    {
      kind: 'remove',
      action: 'deployment.remove',
      label: '删除部署',
      target: target.name,
      detail: '停掉包装单元并删除单元文件与渲染配置；数据卷与业务目录一个字节都不动',
      danger: true,
    },
    removeParams(target),
  )
}

async function confirmAction(phrase: string): Promise<void> {
  const current = preflight.value
  const input = pending.value
  if (current === null || input === null) return
  pendingPhrase.value = phrase
  actionBusy.value = true
  actionError.value = null
  try {
    const submitted = await submitJob({
      preflightId: current.preflight_id,
      ...(phrase === '' ? {} : { confirmationPhrase: phrase }),
      idempotencyKey: idempotencyKey.value,
      timeoutMs: submitTimeoutFor(input.action),
    })
    jobId.value = submitted.data.job_id
    jobStatus.value = 'queued'
    await trackJob(submitted.data.job_id)
  } catch (caught) {
    if (caught instanceof ApiError && caught.code === 'REAUTH_REQUIRED') {
      // 不是失败，是“这一步要先说一句你是谁”。把门摆出来，别把 403 写成做不了。
      reauthNeeded.value = true
      actionError.value = reauthMessage(caught)
    } else {
      actionError.value = errorText(caught)
    }
  } finally {
    actionBusy.value = false
  }
}

/** 再认证被拒时的一句人话（服务端文案可能只说“需要重新验证身份”）。 */
function reauthMessage(error: ApiError): string {
  return `${error.message}——需要先完成再认证（窗口 5 分钟），完成后面板会接着提交这一步。`
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
        jobSummary.value = summarizeDeploymentJob(result.data)
        if (result.data.status === 'succeeded') await afterSuccess()
        return
      }
    } catch (caught) {
      // 读任务失败不等于任务失败：保留“未知”，让人去任务中心看。
      jobNote.value = errorText(caught)
      return
    }
    await sleep(JOB_POLL_MS)
  }
  jobNote.value = '任务在 10 分钟内没有到达终态；请到任务中心查看最新状态。'
}

/** 成功之后重新读现场：结论只信读回来的值。 */
async function afterSuccess(): Promise<void> {
  const finished = pending.value
  formOpen.value = false
  if (finished?.kind === 'update') updateTarget.value = null
  if (finished?.kind === 'remove') {
    // 删除成功之后再去读这一份的现场，注定是 404：那不是错误，是我们明知故问。
    // 先把详情收起来，只重读列表。
    detailName.value = null
    detail.value = null
    return load()
  }
  return Promise.all([load(), reloadDetail()]).then(() => undefined)
}

function errorText(caught: unknown): string {
  if (caught instanceof ApiError) return caught.message
  if (caught instanceof RangeError) return caught.message
  return '请求失败，请稍后重试'
}

onMounted(async () => {
  poller.start()
  await loadOptions()
})

onBeforeUnmount(() => {
  disposed = true
  stopTicker()
  poller.stop()
})
</script>

<template>
  <section class="deploy">
    <div class="deploy__head">
      <RouterLink to="/apps">← 返回应用</RouterLink>
      <h1 class="deploy__title">受管部署</h1>
      <NTag size="small">{{ deployments.length }} 份部署</NTag>
      <NTag v-if="runningCount > 0" size="small" type="success">运行中 {{ runningCount }}</NTag>
      <NTag v-if="missingUnitCount > 0" size="small" type="warning">
        没有单元读数 {{ missingUnitCount }}
      </NTag>
      <NTag v-if="driftCount > 0" size="small" type="error">盘上配置被改过 {{ driftCount }}</NTag>
      <NButton v-if="session.isAdmin" size="small" type="primary" data-testid="deploy-new" @click="openCreate">
        新建部署
      </NButton>
      <NButton size="small" secondary @click="poller.trigger()">刷新</NButton>
    </div>

    <NAlert type="info" :bordered="false">
      这一页读的是<b>管理面</b>：`/etc/upanel/deploy` 下的渲染配置、上一版配置、定义记录，加上
      systemd 对包装单元的两个回答。容器此刻在不在跑是<b>观测面</b>的事，在
      <RouterLink to="/apps">应用</RouterLink> 页看；两张表按部署标识连起来，一个项目只在观测面出现
      说明它不是从这一页部署的。
    </NAlert>

    <NAlert v-if="truncated" type="warning" :bordered="false">
      配置目录里的条目超过了单次读取上限（200），列表被截断；没有显示的部署不代表不存在。
    </NAlert>
    <NAlert v-if="skipped.length > 0" type="warning" :bordered="false">
      配置目录里还有 {{ skipped.length }} 个条目没能读成部署（面板不知道它们是什么）：
      <ul class="deploy__list">
        <li v-for="item in skipped" :key="item.name">
          <code>{{ item.name }}</code>：{{ item.reason }}
        </li>
      </ul>
    </NAlert>
    <NAlert v-if="optionsError !== ''" type="warning" :bordered="false">{{ optionsError }}</NAlert>

    <StatePanel v-if="phase === 'loading'" state="loading" title="正在读取部署现场" />
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
      <StatePanel
        v-if="deployments.length === 0"
        state="empty"
        title="这台机器上还没有受管部署"
        description="从这一页创建部署会写出包装单元与渲染后的 Compose 配置。已经在跑的容器不会自动出现在这里——它们属于观测面（应用页），不是面板部署的。"
      />

      <div v-else class="deploy__rows">
        <article v-for="item in deployments" :key="item.name" class="deploy__row" :data-name="item.name">
          <div class="deploy__row-head">
            <strong class="deploy__name">{{ item.name }}</strong>
            <NTag size="small" :type="deploymentPhase(item) === 'running' ? 'success' : 'default'">
              {{ DEPLOYMENT_PHASE_LABELS[deploymentPhase(item)] }}
            </NTag>
            <NTag v-if="item.config_drift" size="small" type="error">配置与记录不一致</NTag>
            <NTag v-if="!item.record_present" size="small" type="warning">没有定义记录</NTag>
            <code class="deploy__id">{{ item.deployment_id }}</code>
          </div>

          <p class="deploy__line">{{ deploymentPhaseDetail(item) }}</p>
          <p class="deploy__line deploy__muted">{{ describeDeploymentFiles(item) }}</p>
          <p class="deploy__line deploy__muted">
            版本 {{ shortVersion(item.version) }} · 单元 <code>{{ item.unit }}</code>
            <template v-if="item.record">
              · 镜像 {{ item.record.image.requested }}（{{ shortDigest(item.record.image.digest) }}）
            </template>
          </p>

          <div class="deploy__row-actions">
            <NButton size="small" secondary :loading="detailBusy && detailName === item.name" @click="openDetail(item.name)">
              {{ detailName === item.name ? '收起详情' : '详情' }}
            </NButton>
            <template v-if="session.isAdmin">
              <NButton
                size="small"
                :disabled="updateHint(item) !== null"
                data-testid="deploy-update"
                @click="openUpdate(item)"
              >
                换镜像
              </NButton>
              <NButton size="small" type="error" secondary data-testid="deploy-remove" @click="beginRemove(item)">
                删除
              </NButton>
            </template>
          </div>
          <p v-if="session.isAdmin && updateHint(item)" class="deploy__line deploy__warn">
            不能换镜像：{{ updateHint(item) }}
          </p>
          <p v-if="session.isAdmin && removeHint(item)" class="deploy__line deploy__warn">
            不能删除：{{ removeHint(item) }}
          </p>
        </article>
      </div>

      <p v-if="!session.isAdmin" class="deploy__line deploy__muted">
        当前角色是只读（viewer）：可以查看部署现场，创建、换镜像与删除需要管理员。
      </p>

      <!-- 详情：读单个部署的现场（比列表更新，一次请求） -->
      <NCard v-if="detailName" size="small" :title="`部署现场：${detailName}`" data-testid="deploy-detail">
        <p v-if="detailBusy" class="deploy__line deploy__muted">正在读取…</p>
        <NAlert v-else-if="detailError" type="error" :bordered="false">{{ detailError }}</NAlert>
        <template v-else-if="detail">
          <p class="deploy__line">
            <NTag size="small">{{ DEPLOYMENT_PHASE_LABELS[deploymentPhase(detail)] }}</NTag>
            {{ deploymentPhaseDetail(detail) }}
          </p>
          <p class="deploy__line">配置：<code>{{ detail.config_path }}</code></p>
          <p class="deploy__line">单元：<code>{{ detail.unit_path }}</code></p>
          <p class="deploy__line">
            版本令牌 <code>{{ detail.version }}</code>
            <span class="deploy__muted">
              （更新与删除都要带回来；配置缺失时是 unit-only / absent）
            </span>
          </p>
          <p class="deploy__line deploy__muted">
            上一版配置：{{ detail.previous_config_present ? '还在盘上（回切的依据）' : '不在盘上' }} ·
            定义记录：{{ detail.record_present ? '有' : '没有' }}
            <template v-if="detail.record_error">（读不了：{{ detail.record_error }}）</template>
          </p>

          <template v-if="detail.record">
            <p class="deploy__line">
              镜像 {{ detail.record.image.requested }} · digest
              <code>{{ shortDigest(detail.record.image.digest) }}</code>
              <span class="deploy__muted">
                （空着表示没有读数，不表示和上一版一样）
              </span>
            </p>
            <p v-if="detail.record.previous" class="deploy__line deploy__muted">
              上一版镜像 {{ detail.record.previous.requested }}（{{ shortDigest(detail.record.previous.digest) }}）
            </p>
            <p class="deploy__line deploy__muted">
              记录更新于 {{ detail.record.updated_at }}
            </p>

            <div class="deploy__def">
              <p class="deploy__line">受约束的定义（闭集，没有 Compose YAML、没有特权选项）：</p>
              <ul class="deploy__list">
                <li>
                  名字 <code>{{ detail.record.definition.name }}</code> · 镜像
                  <code>{{ detail.record.definition.image_reference }}</code>
                </li>
                <li>
                  端口 {{ detail.record.definition.host_bind_address }}:{{ detail.record.definition.host_port }} →
                  容器 {{ detail.record.definition.container_port }}
                </li>
                <li>
                  资源上限 {{ Math.round(detail.record.definition.resource_limits.memory_limit_bytes / 1048576) }} MiB ·
                  {{ detail.record.definition.resource_limits.cpu_limit }} 核
                </li>
                <li v-if="(detail.record.definition.environment ?? []).length > 0">
                  环境变量 {{ (detail.record.definition.environment ?? []).length }} 条（含秘密的只显示引用）：
                  <code
                    v-for="item in detail.record.definition.environment ?? []"
                    :key="item.name"
                    class="deploy__chip"
                  >
                    {{ item.name }}{{ item.secret_ref ? '=sec_…' : '' }}
                  </code>
                </li>
                <li v-if="(detail.record.definition.mounts ?? []).length > 0">
                  挂载：
                  <span v-for="mount in detail.record.definition.mounts ?? []" :key="mount.container_path">
                    {{ mount.source_root_id }}/{{ mount.relative_path }} → {{ mount.container_path }}（{{
                      mount.read_only ? '只读' : '读写'
                    }}）
                  </span>
                </li>
                <li v-if="(detail.record.definition.volume_dependencies ?? []).length > 0">
                  依赖卷：
                  <code v-for="volume in detail.record.definition.volume_dependencies ?? []" :key="volume" class="deploy__chip">
                    {{ volume }}
                  </code>
                </li>
                <li>
                  面板侧健康探测：{{ detail.record.definition.health_check?.enabled ? '已启用' : '未启用' }}
                  <span class="deploy__muted">
                    （探测的是宿主公开入口，不渲染进 Compose——面板不知道镜像里有什么命令）
                  </span>
                </li>
              </ul>
            </div>
          </template>
        </template>
      </NCard>

      <!-- 新建 -->
      <NCard v-if="formOpen && session.isAdmin" size="small" title="新建部署" data-testid="deploy-form">
        <p class="deploy__line deploy__muted">
          定义是一个闭集：没有原始 Compose YAML、没有 privileged / devices / network_mode / cap_add，
          挂载只能选“受管理文件根 + 根内相对路径”。这些选项之所以危险，正是因为它们能被写进配置文件——
          数据模型里没有它们的位置，于是也写不进渲染结果。
        </p>

        <div class="deploy__grid">
          <label class="deploy__field">
            <span>部署名（同时是 compose 项目名）</span>
            <NInput
              v-model:value="draft.name"
              :maxlength="MAX_DEPLOYMENT_NAME_LENGTH"
              placeholder="小写字母开头：webapp、my-app"
              data-testid="deploy-name"
            />
          </label>
          <label class="deploy__field">
            <span>镜像引用</span>
            <NInput v-model:value="draft.image_reference" placeholder="nginx:1.27 或 nginx@sha256:…" />
          </label>
          <label class="deploy__field">
            <span>绑定地址（必须是字面 IP）</span>
            <NInput v-model:value="draft.host_bind_address" placeholder="127.0.0.1" />
          </label>
          <label class="deploy__field">
            <span>宿主端口</span>
            <NInputNumber v-model:value="draft.host_port" :min="1" :max="65535" />
          </label>
          <label class="deploy__field">
            <span>容器端口</span>
            <NInputNumber v-model:value="draft.container_port" :min="1" :max="65535" />
          </label>
          <label class="deploy__field">
            <span>内存上限（MiB）</span>
            <NInputNumber v-model:value="draft.memory_limit_mib" :min="16" :max="65536" />
          </label>
          <label class="deploy__field">
            <span>CPU 上限（核）</span>
            <NInput v-model:value="draft.cpu_limit" placeholder="0.5" />
          </label>
          <label class="deploy__field">
            <span>依赖的受管理卷</span>
            <NSelect
              v-model:value="draft.volume_dependencies"
              multiple
              filterable
              tag
              :options="volumeOptions"
              placeholder="卷里的数据必须先挂上，部署才能起来"
            />
          </label>
        </div>

        <div class="deploy__block">
          <div class="deploy__block-head">
            <strong>环境变量</strong>
            <NButton size="tiny" secondary @click="addEnvRow">加一条</NButton>
          </div>
          <p v-if="draft.environment.length === 0" class="deploy__line deploy__muted">没有环境变量。</p>
          <div v-for="(item, index) in draft.environment" :key="index" class="deploy__env">
            <NInput v-model:value="item.name" placeholder="NAME" class="deploy__env-name" />
            <NSelect
              v-model:value="item.kind"
              :options="[
                { label: '公开值', value: 'value' },
                { label: '秘密引用', value: 'secret' },
              ]"
              class="deploy__env-kind"
            />
            <NInput
              v-if="item.kind === 'value'"
              v-model:value="item.value"
              placeholder="值会写进渲染后的配置（0600 root）"
            />
            <NInput
              v-else
              v-model:value="item.secret_ref"
              placeholder="sec_… （明文只存在代理的密钥环里）"
            />
            <NButton size="tiny" secondary @click="removeEnvRow(index)">删除</NButton>
          </div>
        </div>

        <div class="deploy__block">
          <div class="deploy__block-head">
            <strong>数据目录挂载</strong>
            <NButton size="tiny" secondary @click="addMountRow">加一条</NButton>
          </div>
          <p class="deploy__line deploy__muted">
            宿主侧只能用“文件根 + 根内相对路径”表达。禁止的容器内目标：
            <code v-for="blocked in BLOCKED_CONTAINER_PATHS" :key="blocked" class="deploy__chip">{{ blocked }}</code>
          </p>
          <p v-if="draft.mounts.length === 0" class="deploy__line deploy__muted">没有挂载。</p>
          <div v-for="(item, index) in draft.mounts" :key="index" class="deploy__mount">
            <NSelect
              v-model:value="item.source_root_id"
              :options="mountRootOptions"
              placeholder="文件根"
              class="deploy__mount-root"
            />
            <NInput v-model:value="item.relative_path" placeholder="根内相对路径（空表示根自身）" />
            <NInput v-model:value="item.container_path" placeholder="容器内路径 /data" />
            <NCheckbox v-model:checked="item.read_only">只读</NCheckbox>
            <NButton size="tiny" secondary @click="removeMountRow(index)">删除</NButton>
          </div>
        </div>

        <div class="deploy__block">
          <div class="deploy__block-head">
            <strong>面板侧健康探测</strong>
            <NSwitch v-model:value="draft.health_check.enabled" />
          </div>
          <p class="deploy__line deploy__muted">
            探的是宿主上的公开入口。探测不通时，部署动作会把 `verified` 降成部分成功——
            单元起来了但应用没应答，是管理员现在就该知道的一件事。
          </p>
          <div v-if="draft.health_check.enabled" class="deploy__grid">
            <label class="deploy__field">
              <span>类型</span>
              <NSelect
                v-model:value="draft.health_check.kind"
                :options="[
                  { label: 'HTTP', value: 'http' },
                  { label: 'TCP', value: 'tcp' },
                ]"
              />
            </label>
            <label class="deploy__field">
              <span>容器内端口（0 表示用容器端口）</span>
              <NInputNumber v-model:value="draft.health_check.port" :min="0" :max="65535" />
            </label>
            <label class="deploy__field">
              <span>HTTP 路径</span>
              <NInput v-model:value="draft.health_check.path" placeholder="/healthz" />
            </label>
            <label class="deploy__field">
              <span>间隔（秒）</span>
              <NInputNumber v-model:value="draft.health_check.interval_seconds" :min="5" :max="300" />
            </label>
            <label class="deploy__field">
              <span>超时（秒）</span>
              <NInputNumber v-model:value="draft.health_check.timeout_seconds" :min="1" :max="60" />
            </label>
            <label class="deploy__field">
              <span>重试次数</span>
              <NInputNumber v-model:value="draft.health_check.retries" :min="0" :max="10" />
            </label>
            <label class="deploy__field">
              <span>启动宽限（秒）</span>
              <NInputNumber v-model:value="draft.health_check.start_period_seconds" :min="0" :max="600" />
            </label>
          </div>
        </div>

        <NAlert v-if="formProblem" type="warning" :bordered="false" data-testid="deploy-problem">
          {{ formProblem.field !== '' ? `${formProblem.field}：` : '' }}{{ formProblem.message }}
        </NAlert>

        <div class="deploy__buttons">
          <NButton type="primary" :disabled="formProblem !== null || actionBusy" @click="beginApply">
            获取预检
          </NButton>
          <NButton secondary :disabled="actionBusy" @click="formOpen = false">取消</NButton>
        </div>
        <p class="deploy__line deploy__muted">
          这里没有“直接部署”的按钮：点上面那一步只是生成预检，阻塞项、告警与影响摊开之后才可提交。
        </p>
      </NCard>

      <!-- 换镜像 -->
      <NCard
        v-if="updateTarget && session.isAdmin"
        size="small"
        :title="`换镜像：${updateTarget.name}`"
        data-testid="deploy-update-form"
      >
        <p class="deploy__line deploy__muted">
          更新只换镜像。换端口、挂载或环境变量属于“另一套定义”，要删除后重建——
          把两件事混在一起会让“回切”失去意义（回切只回配置与镜像）。
        </p>
        <p class="deploy__line">
          当前版本令牌 <code>{{ updateTarget.version }}</code> · 当前镜像
          {{ updateTarget.record?.image.requested ?? '（记录里没有）' }}
        </p>
        <div class="deploy__grid">
          <label class="deploy__field">
            <span>新镜像引用</span>
            <NInput v-model:value="updateImage" placeholder="nginx:1.28" data-testid="deploy-new-image" />
          </label>
          <label class="deploy__field">
            <span>更新前备份（可选，只列 files_only 的策略）</span>
            <NSelect
              v-model:value="updatePolicyId"
              clearable
              :options="backupPolicyOptions"
              placeholder="不备份"
            />
          </label>
        </div>
        <NAlert v-if="updateHintText" type="warning" :bordered="false">{{ updateHintText }}</NAlert>
        <div class="deploy__buttons">
          <NButton type="primary" :disabled="updateImage.trim() === '' || actionBusy" @click="beginUpdate">
            获取预检
          </NButton>
          <NButton secondary :disabled="actionBusy" @click="updateTarget = null">取消</NButton>
        </div>
      </NCard>

      <!-- 确认卡：预检结论与影响摊在这里 -->
      <FileActionConfirm
        v-if="pending"
        :action-label="pending.label"
        :target-label="pending.target"
        :detail="pending.detail"
        :preflight="preflight"
        :busy="actionBusy"
        :error-message="actionError"
        :expired="preflightExpired"
        :remaining-seconds="preflightRemaining"
        :danger="pending.danger"
        @confirm="confirmAction"
        @cancel="resetAction"
      />

      <!-- 再认证：三条部署动作都是 R=是，登录本身不打开这扇门 -->
      <NCard
        v-if="reauthPrompt"
        size="small"
        title="这一步要求再认证"
        data-testid="deploy-reauth"
      >
        <p class="deploy__line deploy__muted">
          三条部署动作在契约里都要求再认证（最近 5 分钟内验过身份）。<b>登录本身不打开这扇门</b>：
          面板密码只会发往 <code>/auth/reauth</code>，不进任务参数、不进日志。
        </p>
        <div class="deploy__grid">
          <label class="deploy__field">
            <span>面板密码</span>
            <NInput
              v-model:value="reauthInput"
              type="password"
              show-password-on="click"
              placeholder="当前登录账号的密码"
              :disabled="reauthBusy"
              data-testid="deploy-reauth-password"
              @keyup.enter="finishReauth"
            />
          </label>
        </div>
        <SecondFactorField
          :show="session.totpEnabled"
          :model-value="reauthCode"
          :disabled="reauthBusy"
          testid="deploy-reauth-code"
          @update:model-value="(value: string) => (reauthCode = value)"
          @keyup.enter="finishReauth"
        />
        <div class="deploy__buttons">
          <NButton
            type="primary"
            :disabled="reauthInput === '' || reauthBusy"
            :loading="reauthBusy"
            data-testid="deploy-reauth-submit"
            @click="finishReauth"
          >
            验证并继续
          </NButton>
        </div>
        <p class="deploy__line deploy__muted">
          验证通过后会接着把上一步提交做完；预检已经过期（120 秒）的话，重新生成预检再提交一次。
        </p>
      </NCard>

      <!-- 任务 -->
      <NCard v-if="jobId" size="small" title="任务">
        <p class="deploy__line">
          任务号 <code>{{ jobId }}</code>
          <span v-if="jobStatus">
            · {{ JOB_STATUS_LABELS[jobStatus] }}<span v-if="jobStep">（步骤 {{ jobStep }}）</span>
          </span>
          <NTag v-if="jobStatus" size="small" :type="JOB_STATUS_TYPES[jobStatus]">
            {{ JOB_STATUS_LABELS[jobStatus] }}
          </NTag>
        </p>
        <template v-if="jobSummary">
          <NAlert
            :type="jobSummary.ok ? (jobSummary.headline.includes('回切') ? 'warning' : 'success') : 'error'"
            :bordered="false"
            :title="jobSummary.headline"
          >
            <ul v-if="jobSummary.lines.length > 0" class="deploy__list">
              <li v-for="line in jobSummary.lines" :key="line">{{ line }}</li>
            </ul>
          </NAlert>
          <NAlert v-if="jobSummary.hint" type="info" :bordered="false">{{ jobSummary.hint }}</NAlert>
        </template>
        <NAlert v-if="jobNote" type="warning" :bordered="false">{{ jobNote }}</NAlert>
        <p class="deploy__line deploy__muted">
          任务的完整事件与脱敏日志在
          <RouterLink to="/jobs">任务中心</RouterLink>。
        </p>
      </NCard>
    </template>
  </section>
</template>

<style scoped>
.deploy {
  display: flex;
  flex-direction: column;
  gap: 16px;
  max-width: 1100px;
}

.deploy__rows {
  display: flex;
  flex-direction: column;
  gap: 10px;
}

.deploy__row {
  display: flex;
  flex-direction: column;
  gap: 6px;
  padding: 12px 14px;
  border: 1px solid var(--rule);
  border-radius: var(--r-2);
}

.deploy__row-head {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 8px;
}

.deploy__name {
  font-size: var(--fs-lg);
  word-break: break-all;
}

.deploy__id {
  font-size: var(--fs-sm);
  opacity: 0.7;
}

.deploy__row-actions {
  display: flex;
  flex-wrap: wrap;
  gap: 8px;
}

.deploy__muted {
  font-size: var(--fs-md);
  opacity: 0.78;
}

.deploy__warn {
  font-size: var(--fs-md);
  color: var(--bad);
}

.deploy__list {
  margin: 0;
  padding-left: 20px;
  line-height: 1.8;
}

.deploy__chip {
  display: inline-block;
  margin-right: 6px;
  padding: 0 4px;
  border-radius: var(--r-2);
  background: var(--rule);
  font-size: var(--fs-sm);
}

.deploy__def {
  display: flex;
  flex-direction: column;
  gap: 4px;
}

.deploy__grid {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(240px, 1fr));
  gap: 10px;
}

.deploy__field {
  display: flex;
  flex-direction: column;
  gap: 4px;
  font-size: var(--fs-md);
}

.deploy__block {
  display: flex;
  flex-direction: column;
  gap: 8px;
  padding-top: 8px;
  border-top: 1px solid var(--rule);
}

.deploy__block-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 8px;
}

.deploy__env,
.deploy__mount {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 8px;
}

.deploy__env-name {
  max-width: 200px;
}

.deploy__env-kind {
  max-width: 130px;
}

.deploy__mount-root {
  min-width: 200px;
}

.deploy__buttons {
  display: flex;
  flex-wrap: wrap;
  gap: 8px;
}
</style>
