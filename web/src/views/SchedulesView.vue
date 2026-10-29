<script setup lang="ts">
// 计划任务与脚本页（P12.5）。
//
// 两块东西放在一页，因为它们是同一件事的两半：**一条计划指向一个对象，一个对象只接受一条计划**。
// 脚本可以单独存在（手动运行），计划必须挂在别的东西上（备份策略 / 证书 / 脚本）。
//
// 这一页只能读定义。建立、修改、删除、立即执行全都是系统写操作：
// 预检 → 任务，要 5 分钟内再认证，要审计。所以页面上任何一个"保存"按钮按下去，
// 走的是和格式化分区同一条链路，而不是直接 PUT 一份定义进来。
//
// 三个状态分开显示，不压成一个绿灯：
//   enabled          定义里的意图（我们想不想让它跑）
//   installed        两个单元文件此刻在不在盘上
//   unit_file_state  是否已 enable
//   active_state     timer 此刻是否 active
// 「enabled 打开但 timer 没在跑」是最常见的一种坏状态，它必须能一眼看见。

import { computed, onBeforeUnmount, onMounted, reactive, ref } from 'vue'
import {
  NAlert,
  NButton,
  NCard,
  NCheckbox,
  NInput,
  NSelect,
  NTag,
} from 'naive-ui'

import { ApiError } from '@/api/client'
import { fetchBackupPolicies, type BackupPolicy } from '@/api/backup'
import {
  CERTIFICATE_OPTION_LIMIT,
  MAX_RUNTIME_SECONDS,
  SCHEDULE_APPLY_ACTION,
  SCHEDULE_DELETE_ACTION,
  SCHEDULE_KIND_LABELS,
  SCHEDULE_KINDS,
  SCRIPT_APPLY_ACTION,
  SCRIPT_DELETE_ACTION,
  SCRIPT_RUN_ACTION,
  canRenewCertificateReason,
  fetchCertificateOptions,
  fetchSchedule,
  fetchSchedules,
  fetchScript,
  fetchScripts,
  runtimeLabel,
  scheduleApplyParams,
  scheduleDeleteParams,
  scheduleRuntimeLabel,
  scriptApplyParams,
  scriptDeleteParams,
  scriptRunParams,
  triggerOutcomeLabel,
  type CertificateOption,
  type ScheduleKind,
  type ScheduleRecord,
  type ScheduleTrigger,
  type ScriptRecord,
} from '@/api/schedules'
import {
  CALENDAR_PRESETS,
  DEFAULT_SCRIPT_RUNTIME_SECONDS,
  MAX_SCRIPT_NAME,
  browserTimezone,
  buildScheduleDefinition,
  calendarPresetOf,
  emptyScheduleForm,
  emptyScriptForm,
  scheduleApplyConfirmLines,
  scheduleDeleteConfirmLines,
  scheduleFormOf,
  scheduleStateLines,
  scriptApplyConfirmLines,
  scriptByteLength,
  scriptDeleteConfirmLines,
  scriptRunConfirmLines,
  validateScheduleForm,
  validateScriptForm,
  type ScheduleFormState,
  type ScriptFormState,
} from '@/components/schedules'
import ApiErrorPanel from '@/components/ApiErrorPanel.vue'
import StatePanel from '@/components/StatePanel.vue'
import StorageActionDialog from '@/components/StorageActionDialog.vue'
import { useSessionStore } from '@/stores/session'
import { formatTimestamp } from '@/utils/format'
import { summarizeScheduleJob } from '@/utils/jobresult'
import { createPoller, type Poller } from '@/utils/polling'
import { apiPanelState, retryablePanelState } from '@/utils/viewstate'

type Phase = 'loading' | 'ready' | 'error'

const POLL_INTERVAL_MS = 30_000

const session = useSessionStore()
const phase = ref<Phase>('loading')
const error = ref<ApiError | null>(null)

const schedules = ref<ScheduleRecord[]>([])
const scripts = ref<ScriptRecord[]>([])
const backupPolicies = ref<BackupPolicy[]>([])
const certificateOptions = ref<CertificateOption[]>([])
const certificatesTruncated = ref(false)
const certificatesError = ref<string | null>(null)
/** 目标对象的名字查询表：列表里只存 id，说明文字要名字。 */
const targetNames = computed(() => {
  const table = new Map<string, string>()
  for (const policy of backupPolicies.value) table.set(policy.id, `备份策略「${policy.name}」`)
  for (const option of certificateOptions.value) table.set(option.id, `证书 ${option.domain}`)
  for (const script of scripts.value) table.set(script.id, `脚本「${script.name}」`)
  return table
})

// ---------------------------------------------------------------- 计划表单

const scheduleFormOpen = ref(false)
const editingSchedule = ref<ScheduleRecord | null>(null)
const scheduleForm = reactive<ScheduleFormState>(emptyScheduleForm())
const scheduleTouched = ref(false)
const scheduleProblems = computed(() => validateScheduleForm(scheduleForm))

const kindOptions = SCHEDULE_KINDS.map((kind) => ({ label: SCHEDULE_KIND_LABELS[kind], value: kind }))

/** 目标下拉：按种类换来源，每一种都给"为什么不可选"的理由。 */
const targetOptions = computed(() => {
  switch (scheduleForm.kind) {
    case 'backup':
      return backupPolicies.value.map((policy) => ({
        label: `备份策略「${policy.name}」`,
        value: policy.id,
      }))
    case 'certificate_renewal':
      return certificateOptions.value.map((option) => {
        const reason = canRenewCertificateReason(option)
        return {
          label:
            reason === null
              ? `证书 ${option.domain}${option.staging ? '（staging）' : ''}`
              : `证书 ${option.domain}（不可续期：${reason}）`,
          value: option.id,
          disabled: reason !== null,
        }
      })
    default:
      return scripts.value.map((script) => ({ label: `脚本「${script.name}」`, value: script.id }))
  }
})

const calendarHint = computed(() => calendarPresetOf(scheduleForm.calendarExpression))

const targetKindHint = computed(() => {
  switch (scheduleForm.kind) {
    case 'backup':
      return '先有一条备份策略，计划才有东西可跑'
    case 'certificate_renewal':
      return '先有一张 certbot 签出来的证书；手动导入的证书续不了'
    default:
      return '先写一条脚本，计划才挂得上'
  }
})

function onKindChange(kind: ScheduleKind): void {
  scheduleForm.kind = kind
  scheduleForm.targetId = ''
  scheduleForm.certificateDomain = ''
  scheduleForm.certificateStaging = false
}

function onTargetChange(id: string): void {
  scheduleForm.targetId = id
  if (scheduleForm.kind !== 'certificate_renewal') return
  const option = certificateOptions.value.find((entry) => entry.id === id) ?? null
  // 域名与 ACME 环境从账本抄进来：触发发生在 systemd 里，那一刻没有面板可问。
  scheduleForm.certificateDomain = option?.domain ?? ''
  scheduleForm.certificateStaging = option?.staging ?? false
}

// ---------------------------------------------------------------- 脚本表单

const scriptFormOpen = ref(false)
const editingScript = ref<ScriptRecord | null>(null)
const scriptForm = reactive<ScriptFormState>(emptyScriptForm())
const scriptTouched = ref(false)
const scriptProblems = computed(() => validateScriptForm(scriptForm))
const scriptLoading = ref(false)
/** 读正文失败时额外的那句话（形状校验管不了"没读到"这件事）。 */
const scriptProblemsOverride = ref<string | null>(null)

// ---------------------------------------------------------------- 详情

const detail = ref<{ schedule: ScheduleRecord; triggers: ScheduleTrigger[] } | null>(null)
const detailBusy = ref(false)
const detailError = ref<string | null>(null)

// ---------------------------------------------------------------- 动作框

interface ActionDialogState {
  action: string
  title: string
  params: Record<string, unknown>
  lines: string[]
  irreversible: boolean
  needsReauth: boolean
}

const actionDialog = ref<ActionDialogState | null>(null)
const actionShow = ref(false)

function openAction(state: ActionDialogState): void {
  actionDialog.value = state
  actionShow.value = true
}

// ---------------------------------------------------------------- 取数

const panelState = computed(() => (error.value ? apiPanelState(error.value) : null))

async function load(signal?: AbortSignal): Promise<void> {
  try {
    const [scheduleResult, scriptResult] = await Promise.all([
      fetchSchedules(signal),
      fetchScripts(signal),
    ])
    if (signal?.aborted) return
    schedules.value = scheduleResult.data.items
    scripts.value = scriptResult.data.items
    phase.value = 'ready'
    error.value = null
  } catch (caught) {
    if (signal?.aborted) return
    if (caught instanceof ApiError) {
      error.value = caught
      phase.value = 'error'
      return
    }
    throw caught
  }
}

/** 目标来源：读不到时只让"这类目标选不出来"，不让整页失败。 */
async function loadTargetSources(): Promise<void> {
  try {
    const result = await fetchBackupPolicies()
    backupPolicies.value = result.data.items
  } catch {
    backupPolicies.value = []
  }
  try {
    const result = await fetchCertificateOptions()
    certificateOptions.value = result.data.items
    certificatesTruncated.value = result.data.truncated
    certificatesError.value = null
  } catch (caught) {
    certificateOptions.value = []
    certificatesError.value = caught instanceof ApiError ? caught.message : '读取证书目录失败'
  }
}

const poller: Poller = createPoller(load, { intervalMs: () => POLL_INTERVAL_MS })

onMounted(() => {
  if (!session.isAdmin) {
    phase.value = 'ready'
    return
  }
  void poller.start()
  void loadTargetSources()
})

onBeforeUnmount(() => {
  poller.stop()
})

// ---------------------------------------------------------------- 计划写操作

function openCreateSchedule(): void {
  editingSchedule.value = null
  Object.assign(scheduleForm, emptyScheduleForm())
  scheduleTouched.value = false
  scheduleFormOpen.value = true
  void loadTargetSources()
}

function openEditSchedule(record: ScheduleRecord): void {
  editingSchedule.value = record
  Object.assign(scheduleForm, scheduleFormOf(record))
  scheduleTouched.value = false
  scheduleFormOpen.value = true
  void loadTargetSources()
}

function useBrowserTimezone(): void {
  const zone = browserTimezone()
  if (zone !== '') scheduleForm.timezone = zone
}

function submitSchedule(): void {
  scheduleTouched.value = true
  if (scheduleProblems.value.length > 0) return
  const current = editingSchedule.value
  const definition = buildScheduleDefinition(scheduleForm)
  const targetName = targetNames.value.get(definition.target_id) ?? ''
  openAction({
    action: SCHEDULE_APPLY_ACTION,
    title: current === null ? `新建计划「${definition.name}」` : `修改计划「${definition.name}」`,
    params: scheduleApplyParams(definition, current ?? undefined),
    lines: scheduleApplyConfirmLines(scheduleForm, targetName, current ?? undefined),
    irreversible: false,
    // schedule.apply 装/卸 systemd 单元：与其它系统写操作一样要再认证。
    needsReauth: true,
  })
}

function askDeleteSchedule(record: ScheduleRecord): void {
  openAction({
    action: SCHEDULE_DELETE_ACTION,
    title: `删除计划「${record.name}」`,
    params: scheduleDeleteParams(record),
    lines: scheduleDeleteConfirmLines(record),
    irreversible: true,
    needsReauth: true,
  })
}

// ---------------------------------------------------------------- 脚本写操作

function openCreateScript(): void {
  editingScript.value = null
  Object.assign(scriptForm, emptyScriptForm())
  scriptTouched.value = false
  scriptFormOpen.value = true
}

async function openEditScript(record: ScriptRecord): Promise<void> {
  editingScript.value = record
  scriptTouched.value = false
  scriptForm.name = record.name
  scriptForm.content = record.content ?? ''
  scriptFormOpen.value = true
  if (record.content !== undefined) return
  // 列表不带正文：打开编辑框才去取详情。
  scriptLoading.value = true
  try {
    const result = await fetchScript(record.id)
    scriptForm.content = result.data.script.content ?? ''
  } catch (caught) {
    scriptForm.content = ''
    scriptTouched.value = true
    scriptProblemsOverride.value =
      caught instanceof ApiError ? `读取脚本正文失败：${caught.message}` : '读取脚本正文失败'
  } finally {
    scriptLoading.value = false
  }
}

function submitScript(): void {
  scriptTouched.value = true
  scriptProblemsOverride.value = null
  if (scriptProblems.value.length > 0) return
  const current = editingScript.value
  openAction({
    action: SCRIPT_APPLY_ACTION,
    title: current === null ? `新建脚本「${scriptForm.name.trim()}」` : `修改脚本「${scriptForm.name.trim()}」`,
    params: scriptApplyParams(
      { name: scriptForm.name.trim(), content: scriptForm.content },
      current ?? undefined,
    ),
    lines: scriptApplyConfirmLines(scriptForm, current ?? undefined),
    irreversible: false,
    needsReauth: true,
  })
}

const runRuntime = ref(String(DEFAULT_SCRIPT_RUNTIME_SECONDS))

const runRuntimeProblem = computed(() => {
  const value = Number.parseInt(runRuntime.value.trim(), 10)
  if (Number.isNaN(value) || value < 1 || value > MAX_RUNTIME_SECONDS) {
    return `单次运行上限必须是 1—${MAX_RUNTIME_SECONDS} 秒之间的整数（${runtimeLabel(MAX_RUNTIME_SECONDS)}封顶）`
  }
  return null
})

function askRunScript(record: ScriptRecord): void {
  if (runRuntimeProblem.value !== null) return
  const seconds = Number.parseInt(runRuntime.value.trim(), 10)
  openAction({
    action: SCRIPT_RUN_ACTION,
    title: `立即运行脚本「${record.name}」`,
    params: scriptRunParams(record, seconds),
    lines: scriptRunConfirmLines({ name: record.name, content_hash: record.content_hash }, seconds),
    irreversible: false,
    needsReauth: true,
  })
}

function askDeleteScript(record: ScriptRecord): void {
  openAction({
    action: SCRIPT_DELETE_ACTION,
    title: `删除脚本「${record.name}」`,
    params: scriptDeleteParams(record),
    lines: scriptDeleteConfirmLines(record),
    irreversible: true,
    needsReauth: true,
  })
}

async function onActionFinished(): Promise<void> {
  await load()
  const current = detail.value?.schedule ?? null
  if (current === null) return
  if (!schedules.value.some((record) => record.id === current.id)) {
    // 刚删掉的就是它：详情没有意义了，也别留一句"读不到"。
    detail.value = null
    detailError.value = null
    return
  }
  await openDetail(current)
}

// ---------------------------------------------------------------- 详情

async function openDetail(record: ScheduleRecord): Promise<void> {
  detailBusy.value = true
  detailError.value = null
  try {
    const result = await fetchSchedule(record.id)
    detail.value = { schedule: result.data.schedule, triggers: result.data.triggers }
  } catch (caught) {
    detail.value = null
    detailError.value = caught instanceof ApiError ? caught.message : '读取计划详情失败'
  } finally {
    detailBusy.value = false
  }
}

function targetLabel(record: ScheduleRecord): string {
  return targetNames.value.get(record.target_id) ?? record.target_id
}
</script>

<template>
  <section class="schedules">
    <div class="schedules__head">
      <h1 class="schedules__title">计划任务</h1>
      <NTag size="small">{{ schedules.length }} 条计划</NTag>
      <NTag size="small">{{ scripts.length }} 条脚本</NTag>
      <NButton v-if="session.isAdmin" size="small" :loading="phase === 'loading'" @click="load()">
        刷新
      </NButton>
      <NButton v-if="session.isAdmin" size="small" type="primary" data-testid="schedule-new" @click="openCreateSchedule">
        新建计划
      </NButton>
      <NButton v-if="session.isAdmin" size="small" type="primary" secondary data-testid="script-new" @click="openCreateScript">
        新建脚本
      </NButton>
    </div>

    <StatePanel
      v-if="!session.isAdmin"
      state="forbidden"
      title="计划任务只对管理员可见"
      description="这一页能决定这台机器每天自动跑什么，也能立即以 upanel-shell 身份运行一段脚本。只读账号看不到它。"
    />
    <StatePanel v-else-if="phase === 'loading'" state="loading" title="正在读取计划与脚本" />
    <StatePanel
      v-else-if="panelState && error && panelState !== 'error'"
      :state="panelState"
      :description="error.message"
      :code="error.code"
      :request-id="error.requestId"
      :retryable="retryablePanelState(panelState)"
      @retry="load()"
    />
    <ApiErrorPanel v-else-if="error" :error="error" @retry="load()" />

    <template v-else>
      <!-- 计划表单 -->
      <NCard v-if="scheduleFormOpen" size="small" :title="editingSchedule ? `修改计划「${editingSchedule.name}」` : '新建计划'">
        <div class="schedules__field">
          <label class="schedules__label" for="schedule-name">计划名</label>
          <NInput id="schedule-name" v-model:value="scheduleForm.name" placeholder="例如 每晚全量备份" data-testid="schedule-name" />
        </div>

        <div class="schedules__field">
          <label class="schedules__label">种类</label>
          <NSelect
            :value="scheduleForm.kind"
            :options="kindOptions"
            data-testid="schedule-kind"
            @update:value="onKindChange"
          />
        </div>

        <div class="schedules__field">
          <label class="schedules__label">目标</label>
          <NSelect
            :value="scheduleForm.targetId"
            :options="targetOptions"
            :placeholder="targetKindHint"
            data-testid="schedule-target"
            @update:value="onTargetChange"
          />
          <p class="schedules__muted">一个对象只接受一条计划：要让同一件事跑两个时间窗，用一条日历表达式写多个时刻。</p>
        </div>

        <NAlert v-if="scheduleForm.kind === 'certificate_renewal' && certificatesTruncated" type="warning" :bordered="false">
          证书比一次读回来的多（只列了前 {{ CERTIFICATE_OPTION_LIMIT }} 张）：这里列不全不代表没有，缺的那张要到证书页去续。
        </NAlert>
        <NAlert v-if="scheduleForm.kind === 'certificate_renewal' && certificatesError" type="warning" :title="certificatesError" />

        <template v-if="scheduleForm.kind === 'certificate_renewal'">
          <div class="schedules__field">
            <label class="schedules__label">域名</label>
            <NInput v-model:value="scheduleForm.certificateDomain" placeholder="从证书账本抄来的域名" />
            <p class="schedules__muted">
              域名与 ACME 环境随定义一起存进代理库：触发发生在 systemd 里，那一刻没有面板可问。
              选目标时会自动抄过来，改之前想清楚——改错了就是每晚给一个别的域名续期。
            </p>
          </div>
          <NCheckbox v-model:checked="scheduleForm.certificateStaging">
            用 ACME staging 环境（签出来的证书浏览器不认，只用来试流程）
          </NCheckbox>
        </template>

        <div class="schedules__field">
          <label class="schedules__label">日历表达式（systemd OnCalendar）</label>
          <NInput v-model:value="scheduleForm.calendarExpression" placeholder="*-*-* 03:30:00" data-testid="schedule-calendar" />
          <div class="schedules__presets">
            <NButton
              v-for="preset in CALENDAR_PRESETS"
              :key="preset.expression"
              size="tiny"
              :type="calendarHint === preset.label ? 'primary' : 'default'"
              secondary
              @click="scheduleForm.calendarExpression = preset.expression"
            >
              {{ preset.label }}
            </NButton>
          </div>
          <p class="schedules__muted">
            这一页只查形状（长度、字符集、句首不是 -）。表达式到底认不认、下一次什么时候触发，
            只有 <code>systemd-analyze calendar</code> 说了算——那是预检在算。
          </p>
        </div>

        <div class="schedules__field">
          <label class="schedules__label">时区</label>
          <div class="schedules__inline">
            <NInput v-model:value="scheduleForm.timezone" placeholder="Asia/Shanghai" data-testid="schedule-timezone" />
            <NButton size="small" secondary @click="useBrowserTimezone">用浏览器时区</NButton>
          </div>
          <p class="schedules__muted">不猜默认值：时区猜错会让"每天凌晨 3 点"跑在别人的凌晨 3 点。</p>
        </div>

        <div class="schedules__field">
          <label class="schedules__label">单次运行上限（秒）</label>
          <NInput v-model:value="scheduleForm.maxRuntimeSeconds" placeholder="3600" />
          <p class="schedules__muted">
            {{ runtimeLabel(Number.parseInt(scheduleForm.maxRuntimeSeconds, 10) || 0) }}。到点整棵进程组被杀掉：
            上限猜大了，一个挂住的备份会占着定时器到明天。
          </p>
        </div>

        <div class="schedules__checks">
          <NCheckbox v-model:checked="scheduleForm.persistent">
            停机期间错过的触发在开机后补跑一次
          </NCheckbox>
          <NCheckbox v-model:checked="scheduleForm.enabled">
            保存后启用（enable 并启动 timer）
          </NCheckbox>
        </div>

        <ul v-if="scheduleTouched && scheduleProblems.length > 0" class="schedules__problems">
          <li v-for="problem in scheduleProblems" :key="problem">{{ problem }}</li>
        </ul>

        <div class="schedules__actions">
          <NButton secondary @click="scheduleFormOpen = false">取消</NButton>
          <NButton type="primary" data-testid="schedule-submit" @click="submitSchedule">保存</NButton>
        </div>
      </NCard>

      <NCard size="small" title="计划">
        <p v-if="schedules.length === 0" class="schedules__muted">
          还没有计划。这一页不自己跑定时器：每一条计划都会写成 systemd 的 timer + service 单元，
          到点由 systemd 触发，触发时不需要面板在跑。
        </p>
        <div v-else class="schedules__list" data-testid="schedule-list">
          <div v-for="record in schedules" :key="record.id" class="schedules__row">
            <div class="schedules__row-main">
              <strong>{{ record.name }}</strong>
              <NTag size="small">{{ SCHEDULE_KIND_LABELS[record.kind] }}</NTag>
              <NTag size="small" :type="record.enabled ? 'success' : 'default'">
                {{ record.enabled ? '定义：启用' : '定义：停用' }}
              </NTag>
              <NTag size="small" :type="record.active_state === 'active' ? 'success' : 'warning'">
                timer {{ record.active_state }}
              </NTag>
              <span class="schedules__meta">{{ targetLabel(record) }}</span>
              <span class="schedules__meta">· {{ scheduleRuntimeLabel(record) }}</span>
              <span v-if="record.next_elapse" class="schedules__meta">· 下次 {{ formatTimestamp(record.next_elapse) }}</span>
            </div>
            <div class="schedules__actions">
              <NButton size="tiny" secondary data-testid="schedule-detail" @click="openDetail(record)">
                详情
              </NButton>
              <NButton size="tiny" secondary @click="openEditSchedule(record)">修改</NButton>
              <NButton size="tiny" secondary data-testid="schedule-delete" @click="askDeleteSchedule(record)">
                删除
              </NButton>
            </div>
            <ul v-if="record.blockers.length > 0" class="schedules__blockers">
              <li v-for="blocker in record.blockers" :key="blocker.code">
                {{ blocker.code }}：{{ blocker.message }}
              </li>
            </ul>
          </div>
        </div>
        <p class="schedules__muted">
          页面不能"立即触发一条计划"：那条路是代理内部的（到点由 systemd 调），开给浏览器就等于给人一个
          "以定时任务身份直接跑"的开关。要现在跑一次，脚本用右边的"立即运行"，备份用备份页的"立即备份"。
        </p>
      </NCard>

      <NCard v-if="detail || detailError" size="small" title="计划详情">
        <p v-if="detailBusy" class="schedules__muted">正在读详情…</p>
        <NAlert v-if="detailError" type="error" :title="detailError" />
        <template v-if="detail">
          <p class="schedules__label">{{ detail.schedule.name }}</p>
          <ul class="schedules__facts">
            <li v-for="line in scheduleStateLines(detail.schedule)" :key="line">{{ line }}</li>
            <li>种类：{{ SCHEDULE_KIND_LABELS[detail.schedule.kind] }} → {{ targetLabel(detail.schedule) }}</li>
            <li>日历：{{ detail.schedule.definition.calendar_expression }}（时区 {{ detail.schedule.definition.timezone }}）</li>
            <li>单次运行上限：{{ runtimeLabel(detail.schedule.definition.max_runtime_seconds) }}</li>
            <li>版本：{{ detail.schedule.version }}，更新于 {{ formatTimestamp(detail.schedule.updated_at) }}</li>
          </ul>

          <p class="schedules__label">触发历史</p>
          <p v-if="detail.triggers.length === 0" class="schedules__muted">
            还没有触发记录：到点没跑过，或者装了但没启用。
          </p>
          <table v-else class="schedules__table">
            <thead>
              <tr>
                <th>时刻</th>
                <th>动作</th>
                <th>结果</th>
                <th>操作</th>
                <th>说明</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="trigger in detail.triggers" :key="trigger.trigger_id">
                <td>{{ formatTimestamp(trigger.slot_at) }}</td>
                <td><code>{{ trigger.action }}</code></td>
                <td>{{ triggerOutcomeLabel(trigger.outcome) }}</td>
                <td><code>{{ trigger.operation_id || '—' }}</code></td>
                <td>{{ trigger.reason || '—' }}</td>
              </tr>
            </tbody>
          </table>
          <p class="schedules__muted">
            结果只有四种：已提交操作、被拦下、跳过（同一时刻已触发过）、提交失败。
            "被拦下"说明到点那一刻条件不成立（例如目标被删了）——那不是系统坏了，是计划说的那件事当时不该做。
          </p>
        </template>
      </NCard>

      <!-- 脚本表单 -->
      <NCard v-if="scriptFormOpen" size="small" :title="editingScript ? `修改脚本「${editingScript.name}」` : '新建脚本'">
        <div class="schedules__field">
          <label class="schedules__label" for="script-name">脚本名</label>
          <NInput
            id="script-name"
            v-model:value="scriptForm.name"
            :maxlength="MAX_SCRIPT_NAME"
            placeholder="例如 清缓存"
            data-testid="script-name-input"
          />
        </div>

        <div class="schedules__field">
          <label class="schedules__label">正文</label>
          <NInput
            v-model:value="scriptForm.content"
            type="textarea"
            :autosize="{ minRows: 8, maxRows: 24 }"
            :disabled="scriptLoading"
            data-testid="script-content"
          />
          <p class="schedules__muted">
            {{ scriptByteLength(scriptForm.content) }} 字节（上限 64 KiB）。以 upanel-shell 身份运行，
            拿不到 root，也没有 sudo。输出不落库：要留证据就在脚本里写日志文件。
          </p>
        </div>

        <NAlert v-if="scriptProblemsOverride" type="warning" :title="scriptProblemsOverride" />
        <ul v-if="scriptTouched && scriptProblems.length > 0" class="schedules__problems">
          <li v-for="problem in scriptProblems" :key="problem">{{ problem }}</li>
        </ul>

        <div class="schedules__actions">
          <NButton secondary @click="scriptFormOpen = false">取消</NButton>
          <NButton type="primary" :loading="scriptLoading" data-testid="script-submit" @click="submitScript">
            保存
          </NButton>
        </div>
      </NCard>

      <NCard size="small" title="脚本">
        <p v-if="scripts.length === 0" class="schedules__muted">
          还没有脚本。脚本是"一段要以 upanel-shell 身份跑的命令"：可以手动立即运行，也可以挂到计划上定时运行。
        </p>
        <div v-else class="schedules__list" data-testid="script-list">
          <div v-for="record in scripts" :key="record.id" class="schedules__row">
            <div class="schedules__row-main">
              <strong>{{ record.name }}</strong>
              <NTag size="small">{{ record.interpreter }}</NTag>
              <span class="schedules__meta"><code>{{ record.content_hash }}</code></span>
              <span class="schedules__meta">· 版本 {{ record.version }}</span>
            </div>
            <div class="schedules__actions">
              <NButton size="tiny" type="primary" data-testid="script-run" @click="askRunScript(record)">
                立即运行
              </NButton>
              <NButton size="tiny" secondary @click="openEditScript(record)">修改</NButton>
              <NButton size="tiny" secondary data-testid="script-delete" @click="askDeleteScript(record)">
                删除
              </NButton>
            </div>
          </div>
        </div>

        <div class="schedules__field">
          <label class="schedules__label">立即运行的时长上限（秒）</label>
          <NInput v-model:value="runRuntime" placeholder="600" data-testid="script-runtime" />
          <p class="schedules__muted">
            {{ runRuntimeProblem ?? runtimeLabel(Number.parseInt(runRuntime, 10) || 0) }}
          </p>
        </div>
      </NCard>

      <NCard size="small" title="这一页的规矩">
        <p class="schedules__line">
          页面上每一个"保存""删除""运行"都走预检 → 任务，都要 5 分钟内再认证、都进审计。
          这里没有"直接写一份定义进去"的路由。
        </p>
        <p class="schedules__line">
          项目里没有任意命令执行接口：能跑的东西只有**已登记脚本**，而且只以 upanel-shell 身份跑。
          一条计划最多只能指向 backup.run / certificate.renew / script.run 这三个动作之一。
        </p>
        <p class="schedules__line">
          计划记录里的 <code>enabled</code>、<code>installed</code>、<code>unit_file_state</code>、
          <code>active_state</code> 是四件不同的事，所以分开显示：「定义里开了」不等于「timer 在跑」。
        </p>
      </NCard>

      <StorageActionDialog
        v-if="actionDialog"
        v-model:show="actionShow"
        :action="actionDialog.action"
        :title="actionDialog.title"
        :params="actionDialog.params"
        :confirm-lines="actionDialog.lines"
        :irreversible="actionDialog.irreversible"
        :needs-reauth="actionDialog.needsReauth"
        :summarizer="summarizeScheduleJob"
        @finished="onActionFinished"
      />
    </template>
  </section>
</template>

<style scoped>

.schedules__muted {
  margin: 0;
  font-size: var(--fs-md);
  line-height: 1.7;
  opacity: 0.8;
}

.schedules__list {
  display: flex;
  flex-direction: column;
  gap: 8px;
  margin: 8px 0;
}

.schedules__row {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  flex-wrap: wrap;
  padding: 8px 0;
  border-bottom: 1px solid var(--rule);
}

.schedules__row-main {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
}

.schedules__field {
  display: flex;
  flex-direction: column;
  gap: 4px;
  margin-bottom: 10px;
}

.schedules__label {
  margin: 0;
  font-size: var(--fs-md);
  font-weight: 600;
}

.schedules__inline {
  display: flex;
  gap: 8px;
  align-items: center;
  flex-wrap: wrap;
}

.schedules__checks {
  display: flex;
  flex-direction: column;
  gap: 6px;
  margin: 10px 0;
}

.schedules__actions {
  display: flex;
  gap: 8px;
  justify-content: flex-end;
  flex-wrap: wrap;
  margin-top: 8px;
}

.schedules__presets {
  display: flex;
  gap: 6px;
  flex-wrap: wrap;
}

.schedules__problems {
  margin: 8px 0;
  padding-left: 18px;
  font-size: var(--fs-md);
  line-height: 1.7;
  color: var(--bad);
}

.schedules__blockers {
  flex: 1 1 100%;
  margin: 4px 0 0;
  padding-left: 18px;
  font-size: var(--fs-md);
  line-height: 1.7;
  color: var(--warn);
}

.schedules__facts {
  margin: 8px 0;
  padding-left: 18px;
  font-size: var(--fs-md);
  line-height: 1.7;
}

.schedules__table {
  width: 100%;
  border-collapse: collapse;
  font-size: var(--fs-md);
}

.schedules__table th,
.schedules__table td {
  text-align: left;
  padding: 4px 8px;
  border-bottom: 1px solid var(--rule);
  vertical-align: top;
}
</style>
