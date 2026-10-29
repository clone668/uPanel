<script setup lang="ts">
// 网络地址修改（P14.2）：一张卡片 + 一个倒计时。
//
// 这一页的核心不是表单，是**改完之后那 120 秒**。所以结构按那两分钟来排：
//
//   1) 没有待确认的改动 → 表单（DHCP / 静态）→ 「预检并修改」；
//   2) 有一条 `pending` 记录 → 倒计时 + 两个按钮（确认保留 / 立刻退回），
//      表单收起。两个入口不许同时出现：一个是"要不要再改一次"，一个是"先把上一次
//      的事情处理掉"，同时摆出来只会让人点错。
//
// 三条必须在此处说清的事（说错了人就会做错事）：
//
//   * **确认只接受从新地址发起的请求。** 判据是服务端从连接里取的本地地址
//     （`arrived_on`），不是页面能填的字段。所以在旧地址上打开的这一页点确认会被拒；
//     这里不假装能成功，直接把它写出来：先在新地址上重新打开面板。
//   * **倒计时按 `deadline` 现算**，不用 `seconds_left`（那是采集那一刻的数字）。
//     这条倒计时决定"还来不来得及确认"，一秒不动就是在骗人。
//   * **不可改就不给按钮**，并把原因逐条摆出来（`unsupported` + `capability.reasons`）。
//     一个灰按钮后面没有原因，人只会以为面板坏了。
//
// 真正的执行仍然只有一条路：预检 → 任务。这里复用的是那个共用确认框
// （StorageActionDialog），再认证、确认短语、轮询、结果读数都在它里面，不在这张卡片里
// 再写一遍。

import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { NAlert, NButton, NCard, NInput, NSelect, NTag } from 'naive-ui'

import {
  EMPTY_APPLY_FORM,
  applyModeLabel,
  buildApplyParams,
  buildRollbackParams,
  countdownText,
  currentAddressText,
  pendingHeadline,
  rollbackSecondsLeft,
  rollbackStateLabel,
  type ApplyFormInput,
  type NetworkApplyParams,
  type NetworkConfigState,
} from '@/api/network'
import { summarizeNetworkJob } from '@/utils/jobresult'
import StorageActionDialog from './StorageActionDialog.vue'

const props = defineProps<{ config: NetworkConfigState | null }>()

const emit = defineEmits<{ changed: [] }>()

const form = ref<ApplyFormInput>({ ...EMPTY_APPLY_FORM })
const formError = ref<string | null>(null)
const now = ref(new Date())
let ticker: ReturnType<typeof setInterval> | null = null

const pending = computed(() => props.config?.pending ?? null)
const pendingState = computed(() => pending.value?.state ?? '')
/** 只有 pending 才是"还来得及"：已确认/已回滚/失败都不需要人再点。 */
const pendingActionable = computed(() => pendingState.value === 'pending')
const secondsLeft = computed(() =>
  pending.value === null ? 0 : rollbackSecondsLeft(pending.value.deadline, now.value),
)

const capability = computed(() => props.config?.capability ?? null)
const editable = computed(() => capability.value?.editable === true)
const reasons = computed(() => [
  ...(capability.value?.reasons ?? []),
  ...(props.config?.unsupported ?? []),
])
const hasHash = computed(() => (props.config?.config_hash ?? '') !== '')
/** 能提交的最低条件：可改、有指纹、而且没有一条待处理的记录。 */
const canApply = computed(() => editable.value && hasHash.value && pending.value === null)

const modeOptions = [
  { label: applyModeLabel('static'), value: 'static' },
  { label: applyModeLabel('dhcp'), value: 'dhcp' },
]

/** 一次待发起的动作（三个写动作共用一个确认框）。 */
interface PendingAction {
  action: string
  title: string
  params: Record<string, unknown>
  lines: string[]
  needsReauth: boolean
}

const pendingRequest = ref<PendingAction | null>(null)
const dialogShow = ref(false)

function applyConfirmLines(params: NetworkApplyParams): string[] {
  const lines: string[] = []
  if (params.definition.mode === 'dhcp') {
    lines.push(`把 ${params.interface_id} 改成 DHCP（地址由网络里的 DHCP 服务器分配）`)
  } else {
    const ipv4 = params.definition.ipv4
    if (ipv4 !== null) {
      const gateway = ipv4.gateway === '' ? '不带网关' : `网关 ${ipv4.gateway}`
      const dns = ipv4.dns === null || ipv4.dns.length === 0 ? '不改 DNS' : `DNS ${ipv4.dns.join('、')}`
      lines.push(`把 ${params.interface_id} 改成 ${ipv4.address}/${ipv4.prefix_length}（${gateway}，${dns}）`)
    }
  }
  lines.push('改完之后浏览器可能立刻断开：面板就在这台机器上')
  lines.push('120 秒内不在新地址上确认，系统会自动退回改之前的样子')
  return lines
}

function openApply(): void {
  const config = props.config
  if (config === null) return
  const built = buildApplyParams(config, form.value)
  if (!built.ok) {
    formError.value = built.error
    return
  }
  formError.value = null
  pendingRequest.value = {
    action: 'network.apply',
    title: `修改 ${built.params.interface_id} 的 IPv4 配置`,
    params: built.params as unknown as Record<string, unknown>,
    lines: applyConfirmLines(built.params),
    needsReauth: true,
  }
  dialogShow.value = true
}

function openConfirm(): void {
  const record = pending.value
  if (record === null) return
  pendingRequest.value = {
    action: 'network.confirm',
    title: '确认保留新配置',
    params: buildRollbackParams(record) as unknown as Record<string, unknown>,
    lines: [
      `停掉 ${record.token} 的回滚 timer，新配置保留下来`,
      '确认只接受从新地址发起的请求：如果你现在是从旧地址打开的这一页，服务端会拒绝',
    ],
    needsReauth: false,
  }
  dialogShow.value = true
}

function openRollback(): void {
  const record = pending.value
  if (record === null) return
  pendingRequest.value = {
    action: 'network.rollback',
    title: '立刻退回改之前的配置',
    params: buildRollbackParams(record) as unknown as Record<string, unknown>,
    lines: [
      `不等到期，立刻把 ${record.interface} 退回改之前的样子`,
      '退回之后这台机器的地址会变回旧的，浏览器可能再次断开',
    ],
    needsReauth: false,
  }
  dialogShow.value = true
}

function onFinished(): void {
  // 任务结束就重新读一次现场：这一屏上所有判断（指纹、pending、模式）都可能已经变了。
  emit('changed')
}

onMounted(() => {
  // 只有真的有待确认的记录才需要每秒跳一次。没有的时候不起定时器：
  // 一个什么都不做的 setInterval 就是一块永远在跑的电。
  ticker = setInterval(() => {
    if (pendingActionable.value) now.value = new Date()
  }, 1000)
})

onBeforeUnmount(() => {
  if (ticker !== null) {
    clearInterval(ticker)
    ticker = null
  }
})
</script>

<template>
  <NCard size="small" class="net-apply">
    <template #header>
      <span class="net-apply__header">
        修改 IPv4 配置
        <NTag v-if="pending" size="small" :type="pendingActionable ? 'warning' : 'info'">
          {{ rollbackStateLabel(pendingState) }}
        </NTag>
      </span>
    </template>

    <p v-if="!config" class="net-apply__sub">读不到写路径的入口状态，无法修改。</p>

    <template v-else>
      <p class="net-apply__current">
        现在：{{ config.interface ?? '（没有可改的网卡）' }} ·
        {{ currentAddressText(config) }} · 模式 {{ applyModeLabel(config.mode) }}
      </p>
      <p v-if="config.panel_file" class="net-apply__sub">
        面板在管的那份文件：{{ config.panel_file.name }}（{{ config.panel_file.sha256.slice(0, 16) }}…）
      </p>
      <p v-else class="net-apply__sub">面板还没有写过配置：这个网卡目前由别的文件管着。</p>

      <!-- 待确认的记录：倒计时与两个出路。 -->
      <div v-if="pending" class="net-apply__pending" data-testid="net-pending">
        <NAlert :type="pendingActionable ? 'warning' : 'info'" :bordered="false">
          <p class="net-apply__headline">{{ pendingHeadline(pending, secondsLeft) }}</p>
          <p v-if="pendingActionable" class="net-apply__countdown">
            剩余 <strong>{{ countdownText(secondsLeft) }}</strong>（到期时刻 {{ pending.deadline }}）
          </p>
          <p v-if="pending.result" class="net-apply__sub">{{ pending.result }}</p>
        </NAlert>

        <div v-if="pendingActionable" class="net-apply__actions">
          <NButton type="primary" size="small" data-testid="net-confirm" @click="openConfirm">
            从新地址确认保留
          </NButton>
          <NButton secondary size="small" data-testid="net-rollback" @click="openRollback">
            立刻退回
          </NButton>
        </div>
        <p v-if="pendingActionable" class="net-apply__sub">
          确认的凭据是"这次请求到达的地址"，由服务端从连接里取。所以：先在
          {{ pending.candidate_address ?? '新地址' }} 上重新打开面板，再点确认；
          在旧地址上点会被拒绝。
        </p>
      </div>

      <!-- 不可改：原因逐条摆出来，不给按钮。 -->
      <template v-if="!editable">
        <p class="net-apply__headline">这台机器的网络配置不能由面板修改</p>
        <ul v-if="reasons.length > 0" class="net-apply__reasons">
          <li v-for="reason in reasons" :key="reason">{{ reason }}</li>
        </ul>
        <p v-else class="net-apply__sub">代理没有给出原因（这是缺陷，请报告）。</p>
      </template>

      <!-- 可改但没有指纹：代理不给 config_hash 时提交必被拒，所以不给按钮。 -->
      <NAlert v-else-if="!hasHash" type="warning" :bordered="false">
        代理没有给出配置指纹（config_hash），现在提交会被判"预检之后现场变了"。先刷新一次。
      </NAlert>

      <!-- 表单：只在没有待确认记录时出现。 -->
      <template v-else-if="pending === null">
        <div class="net-apply__form">
          <label class="net-apply__field">
            <span>方式</span>
            <NSelect
              v-model:value="form.mode"
              size="small"
              :options="modeOptions"
              :disabled="!canApply"
            />
          </label>
          <template v-if="form.mode === 'static'">
            <label class="net-apply__field">
              <span>地址</span>
              <NInput v-model:value="form.address" size="small" placeholder="192.168.1.50" />
            </label>
            <label class="net-apply__field net-apply__field--narrow">
              <span>前缀</span>
              <NInput v-model:value="form.prefixLength" size="small" placeholder="24" />
            </label>
            <label class="net-apply__field">
              <span>网关（可空）</span>
              <NInput v-model:value="form.gateway" size="small" placeholder="192.168.1.1" />
            </label>
            <label class="net-apply__field">
              <span>DNS（可空，最多三个）</span>
              <NInput v-model:value="form.dns" size="small" placeholder="1.1.1.1 8.8.8.8" />
            </label>
          </template>
        </div>

        <NAlert v-if="formError" type="error" :bordered="false" class="net-apply__gap">
          {{ formError }}
        </NAlert>

        <div class="net-apply__actions">
          <NButton type="primary" size="small" :disabled="!canApply" data-testid="net-apply" @click="openApply">
            预检并修改
          </NButton>
          <p class="net-apply__sub">
            点下去先拿到预检：影响、阻塞项与确认短语由服务端给。真正执行要再确认一次。
          </p>
        </div>
      </template>

      <p v-else class="net-apply__sub">
        先把上面那条待确认的改动处理掉（确认或退回），再改下一次：两条回滚记录不许叠加。
      </p>
    </template>

    <StorageActionDialog
      v-if="pendingRequest"
      v-model:show="dialogShow"
      :action="pendingRequest.action"
      :title="pendingRequest.title"
      :params="pendingRequest.params"
      :confirm-lines="pendingRequest.lines"
      :needs-reauth="pendingRequest.needsReauth"
      :summarizer="summarizeNetworkJob"
      @finished="onFinished"
    />
  </NCard>
</template>

<style scoped>
.net-apply__header {
  display: inline-flex;
  align-items: center;
  gap: 8px;
}

.net-apply__current {
  margin: 0 0 4px;
  font-weight: 600;
}

.net-apply__headline {
  margin: 0 0 4px;
  font-weight: 600;
}

.net-apply__countdown {
  margin: 4px 0 0;
  font-variant-numeric: tabular-nums;
}

.net-apply__sub {
  color: var(--upanel-text-muted, var(--ink-2));
  font-size: var(--fs-sm);
  margin: 4px 0 0;
}

.net-apply__reasons {
  margin: 8px 0 0;
  padding-left: 18px;
  color: var(--upanel-text-muted, var(--ink-2));
  font-size: var(--fs-sm);
}

.net-apply__pending {
  display: flex;
  flex-direction: column;
  gap: 8px;
  margin-top: 12px;
}

.net-apply__form {
  display: flex;
  flex-wrap: wrap;
  gap: 12px;
  margin-top: 12px;
}

.net-apply__field {
  display: flex;
  flex-direction: column;
  gap: 4px;
  font-size: var(--fs-sm);
  min-width: 180px;
}

.net-apply__field--narrow {
  min-width: 90px;
}

.net-apply__actions {
  display: flex;
  align-items: center;
  gap: 12px;
  flex-wrap: wrap;
  margin-top: 12px;
}

.net-apply__gap {
  margin-top: 12px;
}
</style>
