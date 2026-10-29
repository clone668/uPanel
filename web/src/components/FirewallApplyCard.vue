<script setup lang="ts">
// 防火墙规则集（P14.3）：一张卡片 + 一个倒计时。
//
// 与 NetworkApplyCard 是同一套骨架，但这一页的形状不同，所以没有合成一个组件：
//
//   1) 地址改动是一份**表单**（方式、地址、网关、DNS）。规则改动是一张**表**：
//      提交的是整份规则集，apply 的语义是"加缺的、删多的"——少一条就等于删掉它。
//      所以这张表一开始就把服务端返回的每一条铺满，而不是让人从空白开始填。
//   2) 表里只有**面板管理的**规则（带 `upanel:<id>` 标记）。管理员手写的规则不在这张表里，
//      也不该在这里出现：这张表删一行就意味着"把它从机器上删掉"，而手写规则面板不碰。
//   3) 改动的一半是 `enabled`。只加规则不启用，等于什么都没做（ufw 未启用时规则不生效）；
//      而启用恰恰是最容易把人关在门外的那一步。所以它是一个开关，和规则一起过预检。
//
// 三条必须在此处说清的事：
//
//   * **管理通路的判据在服务端**。这里只在最明显的情况下提醒一句（要启用、且面板管理的
//     规则里没有任何 `from any` 的放行），并明说那是提醒——见 `managementHint` 的注释。
//     **不要**拿它去关按钮：页面看不到 SSH 端口、看不到手写规则，算不准。
//   * **倒计时按 `deadline` 现算**，不用 `seconds_left`（采集那一刻的数字）。
//   * **不可改就不给按钮**，并把 `reasons` 逐条摆出来。一个灰按钮后面没有原因，
//     人只会以为面板坏了。
//
// 真正的执行仍然只有一条路：预检 → 任务。再认证、确认短语、轮询、结果读数都在共用的
// 那个确认框（StorageActionDialog）里，不在这张卡片里再写一遍。

import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { NAlert, NButton, NCard, NInput, NSelect, NSwitch, NTag } from 'naive-ui'

import {
  EMPTY_RULE_FORM,
  FIREWALL_PROTOS,
  FIREWALL_RULE_ACTIONS,
  buildFirewallApplyParams,
  buildFirewallRollbackParams,
  buildRule,
  describeFirewallChange,
  enabledText,
  firewallConfigHeadline,
  firewallPendingHeadline,
  managementHint,
  ruleActionLabel,
  ruleFormFromRule,
  ruleProtoLabel,
  ruleText,
  type FirewallConfigState,
  type FirewallRuleForm,
} from '@/api/firewall'
import { countdownText, rollbackSecondsLeft } from '@/api/network'
import { summarizeFirewallJob } from '@/utils/jobresult'
import StorageActionDialog from './StorageActionDialog.vue'

const props = defineProps<{ config: FirewallConfigState | null }>()

const emit = defineEmits<{ changed: [] }>()

const rows = ref<FirewallRuleForm[]>([])
const enabled = ref(false)
const formError = ref<string | null>(null)
const now = ref(new Date())
let ticker: ReturnType<typeof setInterval> | null = null

/**
 * 每次现场重新读回来就把表重铺一次。
 *
 * 不是"只在第一次铺"：每完成一次任务，`changed` 会让上层重新拉 `firewall.config`，
 * 而那时的规则集才是真的（可能已经回滚、可能被别人改过）。留着上一轮的表单，
 * 人看到的会是一份"我以为改成了"的规则集。
 */
watch(
  () => props.config,
  (config) => {
    if (config === null) {
      rows.value = []
      enabled.value = false
      return
    }
    // `?? []`：契约里 `rules` 是数组，服务端在 ufw 读不到时本该给 `[]`；这里仍然兜一层，
    // 因为这一行的失败方式是整张卡片崩掉，而"读不到 ufw"恰恰是最需要看到卡片的时候。
    rows.value = (config.rules ?? []).map(ruleFormFromRule)
    enabled.value = config.enabled
    formError.value = null
  },
  { immediate: true },
)

const pending = computed(() => props.config?.pending ?? null)
const secondsLeft = computed(() =>
  pending.value === null ? 0 : rollbackSecondsLeft(pending.value.deadline, now.value),
)
const pendingActionable = computed(() => pending.value !== null && secondsLeft.value > 0)

const editable = computed(() => props.config?.editable === true)
const hasHash = computed(() => (props.config?.config_hash ?? '') !== '')
const reasons = computed(() => props.config?.reasons ?? [])
/** 能提交的最低条件：可改、有指纹、而且没有一条待确认的改动。 */
const canApply = computed(() => editable.value && hasHash.value && pending.value === null)

const actionOptions = FIREWALL_RULE_ACTIONS.map((action) => ({
  label: ruleActionLabel(action),
  value: action,
}))
const protoOptions = FIREWALL_PROTOS.map((proto) => ({ label: ruleProtoLabel(proto), value: proto }))

/** 每一行自己的错：整体拼参数会停在第一条，而人需要看到每一条各自哪里不对。 */
const rowErrors = computed<(string | null)[]>(() => {
  const seen: string[] = []
  return rows.value.map((row) => {
    const built = buildRule(row, seen)
    if (!built.ok) return built.error
    seen.push(built.rule.id)
    return null
  })
})

/** 表里当前有效的规则（给"这次改了什么"与提醒用；有错时为空数组）。 */
const validRules = computed(() => {
  if (rowErrors.value.some((error) => error !== null)) return []
  const seen: string[] = []
  return rows.value.flatMap((row) => {
    const built = buildRule(row, seen)
    if (!built.ok) return []
    seen.push(built.rule.id)
    return [built.rule]
  })
})

const hint = computed(() => managementHint(validRules.value, enabled.value))

function addRow(): void {
  // 标识自动给一个不撞的：手填留给"这个名字我要认得出"的时候，
  // 一进来就要人想一个 id，只会让人填 `1`。
  let index = rows.value.length + 1
  const taken = new Set(rows.value.map((row) => row.id))
  while (taken.has(`rule-${index}`)) index += 1
  rows.value = [...rows.value, { ...EMPTY_RULE_FORM, id: `rule-${index}` }]
  formError.value = null
}

function removeRow(index: number): void {
  rows.value = rows.value.filter((_, at) => at !== index)
  formError.value = null
}

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

function openApply(): void {
  const config = props.config
  if (config === null) return
  const built = buildFirewallApplyParams(config, rows.value, enabled.value)
  if (!built.ok) {
    formError.value = built.error
    return
  }
  formError.value = null
  const lines = describeFirewallChange(config.rules ?? [], built.params.definition.rules, enabled.value)
  pendingRequest.value = {
    action: 'firewall.apply',
    title: enabled.value ? '修改 UFW 规则并启用' : '修改 UFW 规则',
    params: built.params as unknown as Record<string, unknown>,
    lines,
    needsReauth: true,
  }
  dialogShow.value = true
}

function openConfirm(): void {
  const record = pending.value
  if (record === null) return
  pendingRequest.value = {
    action: 'firewall.confirm',
    title: '确认保留这次规则改动',
    params: buildFirewallRollbackParams(record) as unknown as Record<string, unknown>,
    lines: [
      `停掉 ${record.token} 的回滚 timer，这套规则保留下来`,
      '确认不需要换地址发起：预检已经保证这次改动不会挡住面板与 SSH',
    ],
    needsReauth: false,
  }
  dialogShow.value = true
}

function openRollback(): void {
  const record = pending.value
  if (record === null) return
  pendingRequest.value = {
    action: 'firewall.rollback',
    title: '立刻退回改之前的规则',
    params: buildFirewallRollbackParams(record) as unknown as Record<string, unknown>,
    lines: [
      '不等到期，立刻把 UFW 的规则退回改之前的样子',
      `会退回成：${record.previous}`,
    ],
    needsReauth: false,
  }
  dialogShow.value = true
}

function onFinished(): void {
  // 任务结束就重新读一次现场：这一屏上所有判断（指纹、pending、规则集）都可能已经变了。
  emit('changed')
}

onMounted(() => {
  // 只有真的有待确认的改动才需要每秒跳一次。没有的时候不起定时器：
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
  <NCard size="small" class="fw-apply">
    <template #header>
      <span class="fw-apply__header">
        修改 UFW 规则
        <NTag v-if="pending" size="small" type="warning">等待确认</NTag>
      </span>
    </template>

    <p v-if="!config" class="fw-apply__sub">
      读不到防火墙写路径的入口状态（代理可能没开 `-firewall-writes`），无法修改。
    </p>

    <template v-else>
      <p class="fw-apply__current">{{ firewallConfigHeadline(config) }}</p>

      <!-- 待确认的改动：倒计时与两个出路。表单收起——两个入口同时出现只会让人点错。 -->
      <div v-if="pending" class="fw-apply__pending" data-testid="fw-pending">
        <NAlert type="warning" :bordered="false">
          <p class="fw-apply__headline">{{ firewallPendingHeadline(secondsLeft) }}</p>
          <p class="fw-apply__countdown">
            <template v-if="pendingActionable">
              剩余 <strong>{{ countdownText(secondsLeft) }}</strong>（到期时刻 {{ pending.deadline }}）
            </template>
            <template v-else>已经到期，系统随时会动手</template>
          </p>
          <p class="fw-apply__sub">改之前的样子：{{ pending.previous }}</p>
        </NAlert>

        <div v-if="pendingActionable" class="fw-apply__actions">
          <NButton type="primary" size="small" data-testid="fw-confirm" @click="openConfirm">
            确认保留
          </NButton>
          <NButton secondary size="small" data-testid="fw-rollback" @click="openRollback">
            立刻退回
          </NButton>
        </div>
        <p v-if="pendingActionable" class="fw-apply__sub">
          确认只是停掉那条回滚 timer：防火墙改动不允许挡住管理通路，所以「从新地址确认」这套
          在这里不存在——能发出这条请求，本身就说明面板还进得来。
        </p>
      </div>

      <!-- 不可改：原因逐条摆出来，不给按钮。 -->
      <template v-if="!editable">
        <p class="fw-apply__headline">这台机器的防火墙不能由面板修改</p>
        <ul v-if="reasons.length > 0" class="fw-apply__reasons">
          <li v-for="reason in reasons" :key="reason">{{ reason }}</li>
        </ul>
        <p v-else class="fw-apply__sub">代理没有给出原因（这是缺陷，请报告）。</p>
      </template>

      <!-- 可改但没有指纹：代理不给 config_hash 时提交必被拒，所以不给按钮。 -->
      <NAlert v-else-if="!hasHash" type="warning" :bordered="false" class="fw-apply__gap">
        代理没有给出规则集指纹（config_hash），现在提交会被判「预检之后现场变了」。先刷新一次。
      </NAlert>

      <!-- 规则表：只在没有待确认改动时出现。 -->
      <template v-else-if="pending === null">
        <p class="fw-apply__sub">
          这张表里只有「面板管理的」规则（每条带 upanel: 标记）。管理员手写的规则不在
          这里，也不会被这次改动碰——它们在下面的只读列表里照常可见。
        </p>

        <table class="fw-apply__table">
          <thead>
            <tr>
              <th>标识</th>
              <th>动作</th>
              <th>端口</th>
              <th>协议</th>
              <th>来源（空 = 所有来源）</th>
              <th></th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="(row, index) in rows" :key="`${index}-${row.id}`">
              <td>
                <NInput v-model:value="row.id" size="tiny" placeholder="web" :disabled="!canApply" />
              </td>
              <td>
                <NSelect
                  v-model:value="row.action"
                  size="tiny"
                  :options="actionOptions"
                  :disabled="!canApply"
                />
              </td>
              <td class="fw-apply__port">
                <NInput v-model:value="row.port" size="tiny" placeholder="443" :disabled="!canApply" />
              </td>
              <td>
                <NSelect
                  v-model:value="row.proto"
                  size="tiny"
                  :options="protoOptions"
                  :disabled="!canApply"
                />
              </td>
              <td>
                <NInput
                  v-model:value="row.from"
                  size="tiny"
                  placeholder="10.0.0.0/8"
                  :disabled="!canApply"
                />
              </td>
              <td>
                <NButton
                  size="tiny"
                  quaternary
                  type="error"
                  :disabled="!canApply"
                  @click="removeRow(index)"
                >
                  删
                </NButton>
              </td>
            </tr>
            <tr v-if="rows.length === 0">
              <td colspan="6" class="fw-apply__sub">面板还没有管任何规则（提交一份空定义就是「清空面板管理的规则」）。</td>
            </tr>
          </tbody>
        </table>

        <ul v-if="rowErrors.some((error) => error !== null)" class="fw-apply__reasons">
          <li v-for="(error, index) in rowErrors" :key="index">
            <template v-if="error !== null">第 {{ index + 1 }} 条：{{ error }}</template>
          </li>
        </ul>

        <div class="fw-apply__enable">
          <NSwitch v-model:value="enabled" :disabled="!canApply" />
          <span>启用 UFW</span>
        </div>
        <p class="fw-apply__sub">
          现在是{{ enabledText(enabled) }}。只加规则不启用等于什么都没做（ufw 未启用时
          规则不生效），而启用恰恰是最容易把人关在门外的那一步——所以它和规则一起过预检。
        </p>

        <NAlert v-if="hint" type="warning" :bordered="false" class="fw-apply__gap">
          {{ hint }}
        </NAlert>

        <NAlert v-if="formError" type="error" :bordered="false" class="fw-apply__gap">
          {{ formError }}
        </NAlert>

        <div class="fw-apply__actions">
          <NButton size="small" :disabled="!canApply" data-testid="fw-add" @click="addRow">
            加一条规则
          </NButton>
          <NButton
            type="primary"
            size="small"
            :disabled="!canApply"
            data-testid="fw-apply"
            @click="openApply"
          >
            预检并应用
          </NButton>
          <p class="fw-apply__sub">
            点下去先拿到预检：影响、阻塞项与确认短语由服务端给。真正执行要再确认一次。
          </p>
        </div>

        <ul v-if="validRules.length > 0" class="fw-apply__preview">
          <li v-for="rule in validRules" :key="rule.id">{{ ruleText(rule) }}</li>
        </ul>
      </template>

      <p v-else class="fw-apply__sub">
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
      :summarizer="summarizeFirewallJob"
      @finished="onFinished"
    />
  </NCard>
</template>

<style scoped>
.fw-apply__header {
  display: inline-flex;
  align-items: center;
  gap: 8px;
}

.fw-apply__current {
  margin: 0 0 4px;
  font-weight: 600;
}

.fw-apply__headline {
  margin: 0 0 4px;
  font-weight: 600;
}

.fw-apply__countdown {
  margin: 4px 0 0;
  font-variant-numeric: tabular-nums;
}

.fw-apply__sub {
  color: var(--upanel-text-muted, var(--ink-2));
  font-size: var(--fs-sm);
  margin: 4px 0 0;
}

.fw-apply__reasons {
  margin: 8px 0 0;
  padding-left: 18px;
  color: var(--upanel-text-muted, var(--ink-2));
  font-size: var(--fs-sm);
}

.fw-apply__pending {
  display: flex;
  flex-direction: column;
  gap: 8px;
  margin-top: 12px;
}

.fw-apply__table {
  width: 100%;
  border-collapse: collapse;
  margin-top: 12px;
  font-size: var(--fs-sm);
}

.fw-apply__table th,
.fw-apply__table td {
  padding: 4px 6px;
  text-align: left;
  vertical-align: middle;
}

.fw-apply__table th {
  color: var(--upanel-text-muted, var(--ink-2));
  font-weight: 500;
}

.fw-apply__port {
  width: 90px;
}

.fw-apply__enable {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-top: 12px;
  font-size: var(--fs-sm);
}

.fw-apply__actions {
  display: flex;
  align-items: center;
  gap: 12px;
  flex-wrap: wrap;
  margin-top: 12px;
}

.fw-apply__preview {
  margin: 8px 0 0;
  padding-left: 18px;
  color: var(--upanel-text-muted, var(--ink-2));
  font-size: var(--fs-sm);
}

.fw-apply__gap {
  margin-top: 12px;
}
</style>
