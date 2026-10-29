<script setup lang="ts">
// 监控告警（P13.4）：历史曲线、告警规则、告警事件与确认、通知投递账。
//
// 这个页面只做"把服务端的事实摆出来"，三条口径原样传递，不做任何修饰：
//
//  1) **缺失不是 0。** 历史曲线的缺口画成断开（MetricChart 收的是切好的段），
//     缺失的桶数写在标题右边；`last_value` / `peak_value` 为 null 时显示"—"
//     而不是 0。采集停了十分钟就"恢复正常"的告警，比没有告警更坏。
//  2) **陈旧要说清是哪一种。** 未开启历史、采集循环没跑、循环在跑但样本没进来，
//     三件事的处置完全不同（judgeStaleness 判定，页面只显示它给的句子）。
//  3) **确认不是解决。** 点"确认"只写 acked_at：事件仍然是 firing，规则状态机
//     一点没动。所以按钮上写的是"确认已知"，成功之后行上仍然标着"还在持续"。
//
// 页面不缓存、不轮询：这里没有"实时"承诺，刷新按钮与时间窗选择就是全部控制。

import { computed, h, onMounted, ref } from 'vue'
import {
  NAlert,
  NButton,
  NCard,
  NDataTable,
  NRadioButton,
  NRadioGroup,
  NSpace,
  NTag,
  NText,
} from 'naive-ui'
import type { DataTableColumns } from 'naive-ui'

import { ApiError } from '@/api/client'
import {
  ackAlertEvent,
  acknowledgementState,
  alertConditionText,
  alertEventSummary,
  ALERT_EVENT_STATE_LABELS,
  ALERT_RULE_STATE_LABELS,
  fetchAlertEvents,
  fetchAlertRules,
  formatClock,
  formatDuration,
  type AlertEvent,
  type AlertRule,
} from '@/api/alerts'
import {
  buildChartBlocks,
  DEFAULT_METRICS_WINDOW_MS,
  fetchMetrics,
  fetchMetricsStatus,
  judgeStaleness,
  METRIC_UNIT_LABELS,
  type ChartBlock,
  type ChartSet,
  type MetricsStatus,
  type StalenessVerdict,
} from '@/api/metrics'
import {
  CHANNEL_TYPE_LABELS,
  deliveryStateText,
  DELIVERY_KIND_LABELS,
  DELIVERY_STATE_LABELS,
  fetchNotificationDeliveries,
  willRetry,
  type NotificationDelivery,
} from '@/api/notifications'
import ApiErrorPanel from '@/components/ApiErrorPanel.vue'
import MetricChart from '@/components/MetricChart.vue'
import StatePanel from '@/components/StatePanel.vue'
import { useSessionStore } from '@/stores/session'

const session = useSessionStore()

// ---------------------------------------------------------------- 时间窗

interface WindowPreset {
  key: string
  label: string
  ms: number
}

/** 三档。24 小时是 raw 粒度的保留上限（PLAN §18），再长就该看分钟聚合了。 */
const WINDOW_PRESETS: readonly WindowPreset[] = [
  { key: '1h', label: '1 小时', ms: DEFAULT_METRICS_WINDOW_MS },
  { key: '6h', label: '6 小时', ms: 6 * 60 * 60 * 1000 },
  { key: '24h', label: '24 小时', ms: 24 * 60 * 60 * 1000 },
]
const windowKey = ref<string>('1h')
const windowMs = computed<number>(
  () => WINDOW_PRESETS.find((p) => p.key === windowKey.value)?.ms ?? DEFAULT_METRICS_WINDOW_MS,
)

/** 主机曲线的三条：CPU、内存、1 分钟负载（与 PLAN §18 的告警种类对齐）。 */
const HOST_METRICS = ['cpu_usage_percent', 'memory_used_percent', 'load1'] as const
/** 容量曲线：只看使用率——剩余字节的绝对值在每块盘上量级差太远，同一屏画不出来。 */
const FS_METRICS = ['used_percent'] as const

// ---------------------------------------------------------------- 采集状态

const status = ref<MetricsStatus | null>(null)
const statusError = ref<ApiError | null>(null)

const staleness = computed<StalenessVerdict | null>(() => {
  if (!status.value) return null
  return judgeStaleness(status.value, new Date())
})

const stalenessType = computed<'success' | 'info' | 'warning' | 'error'>(() => {
  switch (staleness.value?.kind) {
    case 'ok':
      return 'success'
    case 'disabled':
      return 'info'
    case 'idle':
      return 'warning'
    default:
      return 'error'
  }
})

// ---------------------------------------------------------------- 历史曲线

const chartsLoading = ref(false)
const chartsError = ref<ApiError | null>(null)
/** null = 这个进程不记历史（enabled=false），reason 在 status 里。 */
const hostCharts = ref<ChartSet | null>(null)
const fsCharts = ref<ChartSet | null>(null)

const chartsDisabled = computed<boolean>(
  () => status.value !== null && !status.value.enabled,
)

const hostBlocks = computed<readonly ChartBlock[]>(() => hostCharts.value?.blocks ?? [])
const fsBlocks = computed<readonly ChartBlock[]>(() => fsCharts.value?.blocks ?? [])

// ---------------------------------------------------------------- 告警

const alertsLoading = ref(false)
const alertsError = ref<ApiError | null>(null)
const rules = ref<AlertRule[]>([])
const events = ref<AlertEvent[]>([])

/** 正在确认的事件 id（防连点），以及确认失败的原因。 */
const ackBusyId = ref<number | null>(null)
const ackError = ref<ApiError | null>(null)

const firingCount = computed<number>(() => events.value.filter((e) => e.state === 'firing').length)
const unackedFiring = computed<number>(
  () => events.value.filter((e) => acknowledgementState(e).needsAttention).length,
)

// ---------------------------------------------------------------- 投递账

const deliveriesLoading = ref(false)
const deliveriesError = ref<ApiError | null>(null)
const deliveries = ref<NotificationDelivery[]>([])

// ---------------------------------------------------------------- 加载

async function loadStatus(): Promise<void> {
  statusError.value = null
  try {
    const result = await fetchMetricsStatus()
    status.value = result.data
  } catch (error) {
    statusError.value = error instanceof ApiError ? error : null
    status.value = null
  }
}

async function loadCharts(): Promise<void> {
  chartsLoading.value = true
  chartsError.value = null
  const to = new Date()
  const from = new Date(to.getTime() - windowMs.value)
  try {
    // 两个 scope 分开查（接口一次只接受一个 scope）：主机的三条曲线与每块盘的使用率
    // 属于两类。**一个失败就当整组失败**：真正的失败是查询参数或库的问题，两半都会
    // 中招；把半边画出来、半边空着，会让人以为"文件系统曲线本来就没有"。
    const [host, fs] = await Promise.all([
      fetchMetrics({
        scope: 'host',
        metrics: HOST_METRICS,
        from: from.toISOString(),
        to: to.toISOString(),
      }),
      fetchMetrics({
        scope: 'filesystem',
        metrics: FS_METRICS,
        from: from.toISOString(),
        to: to.toISOString(),
      }),
    ])
    hostCharts.value = buildChartBlocks(host.data)
    fsCharts.value = buildChartBlocks(fs.data)
  } catch (error) {
    chartsError.value = error instanceof ApiError ? error : null
    hostCharts.value = null
    fsCharts.value = null
  } finally {
    chartsLoading.value = false
  }
}

async function loadAlerts(): Promise<void> {
  alertsLoading.value = true
  alertsError.value = null
  try {
    const [ruleResult, eventResult] = await Promise.all([
      fetchAlertRules(),
      fetchAlertEvents({ limit: 50 }),
    ])
    rules.value = ruleResult.data
    events.value = eventResult.data
  } catch (error) {
    alertsError.value = error instanceof ApiError ? error : null
    rules.value = []
    events.value = []
  } finally {
    alertsLoading.value = false
  }
}

async function loadDeliveries(): Promise<void> {
  if (!session.isAdmin) return
  deliveriesLoading.value = true
  deliveriesError.value = null
  try {
    const result = await fetchNotificationDeliveries({ limit: 50 })
    deliveries.value = result.data
  } catch (error) {
    deliveriesError.value = error instanceof ApiError ? error : null
    deliveries.value = []
  } finally {
    deliveriesLoading.value = false
  }
}

async function refreshAll(): Promise<void> {
  await Promise.all([loadStatus(), loadCharts(), loadAlerts(), loadDeliveries()])
}

onMounted(() => {
  void refreshAll()
})

async function onWindowChange(value: string | number | boolean): Promise<void> {
  windowKey.value = String(value)
  await loadCharts()
}

/** 确认一条事件。成功之后**只**改 acked 两个字段，状态原样保留。 */
async function onAck(event: AlertEvent): Promise<void> {
  ackBusyId.value = event.id
  ackError.value = null
  try {
    const result = await ackAlertEvent(event.id)
    const updated = result.data
    events.value = events.value.map((e) => (e.id === updated.id ? updated : e))
  } catch (error) {
    ackError.value = error instanceof ApiError ? error : null
  } finally {
    ackBusyId.value = null
  }
}

// ---------------------------------------------------------------- 表格

function ruleTone(state: AlertRule['state']): 'default' | 'info' | 'warning' | 'error' {
  switch (state) {
    case 'firing':
      return 'error'
    case 'pending':
      return 'warning'
    default:
      return 'default'
  }
}

const ruleColumns: DataTableColumns<AlertRule> = [
  { title: '规则', key: 'name' },
  {
    title: '条件',
    key: 'condition',
    render: (row) => alertConditionText(row),
  },
  {
    title: '持续 / 恢复线',
    key: 'duration',
    render: (row) => `${formatDuration(row.duration_seconds)}，回到 ${String(row.recovery_threshold)} 才算恢复`,
  },
  {
    title: '状态',
    key: 'state',
    render: (row) =>
      h(NTag, { type: ruleTone(row.state), size: 'small' }, { default: () => ALERT_RULE_STATE_LABELS[row.state] }),
  },
  {
    title: '最近一轮',
    key: 'last',
    // last_value 为 null 时写"—"，不写 0：那是"这一轮没读到数据"，不是"值是 0"。
    render: (row) => {
      const value = row.last_value === null ? '—' : String(row.last_value)
      return row.last_reason === '' ? value : `${value}（${row.last_reason}）`
    },
  },
  {
    title: '冷却至',
    key: 'cooldown',
    render: (row) => (row.cooldown_until === '' ? '—' : formatClock(row.cooldown_until)),
  },
]

const eventColumns = computed<DataTableColumns<AlertEvent>>(() => [
  {
    title: '规则',
    key: 'rule_name',
    render: (row) =>
      // rule_id 为 0 表示规则后来被删掉了。历史行仍然完整（存的是快照），
      // 但要标出来，否则人会去找一条已经不存在的规则。
      row.rule_id === 0 ? `${row.rule_name}（规则已删除）` : row.rule_name,
  },
  {
    title: '条件（触发时）',
    key: 'condition',
    render: (row) => `${row.metric} ${row.comparison} ${String(row.threshold)}${
      row.target_id === '' ? '' : `（${row.target_id}）`
    }`,
  },
  {
    title: '状态',
    key: 'state',
    render: (row) =>
      h(
        NTag,
        { type: row.state === 'firing' ? 'error' : 'success', size: 'small' },
        { default: () => ALERT_EVENT_STATE_LABELS[row.state] },
      ),
  },
  {
    title: '情况',
    key: 'summary',
    render: (row) => alertEventSummary(row),
  },
  {
    title: '峰值',
    key: 'peak',
    // 区间里一个样本都没有时是 null。写 0 会让人以为"当时的值是 0"。
    render: (row) => (row.peak_value === null ? '—' : String(row.peak_value)),
  },
  {
    title: '确认',
    key: 'ack',
    render: (row) => {
      const ackedAt = row.acked_at
      if (ackedAt !== null) {
        // 先取到常量再进闭包：属性在回调里不会被收窄，写成 row.acked_at 就要多一次断言。
        return h(NText, { depth: 3 }, { default: () => `${row.acked_by || '某人'} · ${formatClock(ackedAt)}` })
      }
      if (!session.isAdmin) {
        // viewer 能看事件，但确认是 admin 的动作（它是"谁对这台机器负责"的一部分）。
        return h(NText, { depth: 3 }, { default: () => '未确认' })
      }
      return h(
        NButton,
        {
          size: 'small',
          quaternary: true,
          loading: ackBusyId.value === row.id,
          onClick: () => {
            void onAck(row)
          },
        },
        { default: () => '确认已知' },
      )
    },
  },
])

const deliveryColumns: DataTableColumns<NotificationDelivery> = [
  { title: '时间', key: 'created_at', render: (row) => formatClock(row.created_at) },
  {
    title: '来源',
    key: 'kind',
    render: (row) => {
      const label = DELIVERY_KIND_LABELS[row.kind]
      return row.kind === 'test' ? label : `${label}：${row.event_rule}`
    },
  },
  {
    title: '通道',
    key: 'channel',
    render: (row) =>
      row.channel_id === 0
        ? `${CHANNEL_TYPE_LABELS[row.channel_type]}（通道已删除）`
        : `${CHANNEL_TYPE_LABELS[row.channel_type]}：${row.channel_name}`,
  },
  { title: '标题', key: 'title' },
  {
    title: '状态',
    key: 'state',
    render: (row) => {
      const tone = row.state === 'sent' ? 'success' : row.state === 'failed' ? 'error' : 'warning'
      const hint = willRetry(row) ? '，会自己重试' : ''
      return h(
        NTag,
        { type: tone, size: 'small' },
        { default: () => `${DELIVERY_STATE_LABELS[row.state]}${hint}` },
      )
    },
  },
  {
    title: '说明',
    key: 'detail',
    // 对端原话原样显示：翻译成"发送失败"之后，人只能去翻日志。
    render: (row) => deliveryStateText(row),
  },
]

</script>

<template>
  <section class="alerts-view" data-testid="alerts-view">
    <header class="alerts-view__header">
      <h1>监控告警</h1>
      <NButton :loading="chartsLoading || alertsLoading" @click="refreshAll">刷新</NButton>
      <!-- 说明整行占满：放在 flex 行里会和标题、按钮挤在同一行，
           一个 549px 的 max-width 让三者并排时谁都不像页头。 -->
      <div class="alerts-view__lead">
        <p class="alerts-view__subtitle">
          历史曲线来自面板自己的样本库（代理不在也能看）；告警规则与事件只回答“现在有没有事”；
          通知是否发出去了，看最下面的投递账。
        </p>
      </div>
    </header>

    <!-- 采集状态：三种"不正常"分开说 -->
    <ApiErrorPanel
      v-if="statusError"
      :error="statusError"
      title="读不到采集状态"
      @retry="loadStatus"
    />
    <NAlert v-else-if="staleness" :type="stalenessType" data-testid="staleness-alert">
      {{ staleness.message }}
      <template v-if="status && status.last_sample_at">
        <span class="alerts-view__meta">
          最后一个样本：{{ formatClock(status.last_sample_at) }}
          <template v-if="staleness.ageSeconds !== null">（{{ formatDuration(staleness.ageSeconds) }}前）</template>
        </span>
      </template>
    </NAlert>

    <!-- 概览数字 -->
    <NSpace class="alerts-view__tiles" :size="12">
      <div class="alerts-view__tile">
        <div class="alerts-view__tile-value">{{ firingCount }}</div>
        <div class="alerts-view__tile-label">还在持续的事件</div>
      </div>
      <div class="alerts-view__tile">
        <div class="alerts-view__tile-value">{{ unackedFiring }}</div>
        <div class="alerts-view__tile-label">持续且没人确认</div>
      </div>
      <div class="alerts-view__tile">
        <div class="alerts-view__tile-value">{{ rules.length }}</div>
        <div class="alerts-view__tile-label">告警规则</div>
      </div>
    </NSpace>

    <!-- 历史曲线 -->
    <NCard title="历史曲线" size="small" class="alerts-view__card">
      <template #header-extra>
        <NRadioGroup :value="windowKey" size="small" @update:value="onWindowChange">
          <NRadioButton v-for="preset in WINDOW_PRESETS" :key="preset.key" :value="preset.key">
            {{ preset.label }}
          </NRadioButton>
        </NRadioGroup>
      </template>

      <StatePanel
        v-if="chartsDisabled"
        state="dependency_missing"
        title="这个进程没有在记历史"
        :description="status?.reason ?? '历史监控未启用。曲线要的是过去的数据，没有就是没有，不会补零。'"
      />
      <ApiErrorPanel v-else-if="chartsError" :error="chartsError" title="读不到历史曲线" @retry="loadCharts" />
      <div v-else class="alerts-view__charts">
        <section v-if="hostBlocks.length > 0" class="alerts-view__chart-group">
          <h2>主机</h2>
          <div v-for="block in hostBlocks" :key="block.key" class="alerts-view__chart">
            <div class="alerts-view__chart-head">
              <span class="alerts-view__chart-title">{{ block.label }}</span>
              <span class="alerts-view__chart-meta">
                {{ METRIC_UNIT_LABELS[block.unit] }}
                <template v-if="block.missing > 0">
                  · 缺 {{ block.missing }} 个点（图上断开处）
                </template>
              </span>
            </div>
            <MetricChart
              v-if="hostCharts"
              :segments="block.segments"
              :unit="block.unit"
              :bounds="block.bounds"
              :from-ms="hostCharts.fromMs"
              :to-ms="hostCharts.toMs"
            />
          </div>
        </section>
        <section v-else class="alerts-view__chart-group">
          <StatePanel state="empty" title="这段时间没有主机样本" description="缺失就是缺失：这里不画 0，也不补一条直线。" />
        </section>

        <section v-if="fsBlocks.length > 0" class="alerts-view__chart-group">
          <h2>文件系统使用率</h2>
          <div v-for="block in fsBlocks" :key="block.key" class="alerts-view__chart">
            <div class="alerts-view__chart-head">
              <span class="alerts-view__chart-title">{{ block.label }}</span>
              <span class="alerts-view__chart-meta">
                百分比
                <template v-if="block.missing > 0">
                  · 缺 {{ block.missing }} 个点（图上断开处）
                </template>
              </span>
            </div>
            <MetricChart
              v-if="fsCharts"
              :segments="block.segments"
              :unit="block.unit"
              :bounds="block.bounds"
              :from-ms="fsCharts.fromMs"
              :to-ms="fsCharts.toMs"
            />
          </div>
        </section>
      </div>
    </NCard>

    <!-- 告警事件 -->
    <NCard title="告警事件" size="small" class="alerts-view__card">
      <ApiErrorPanel v-if="alertsError" :error="alertsError" title="读不到告警" @retry="loadAlerts" />
      <template v-else>
        <ApiErrorPanel v-if="ackError" :error="ackError" title="确认失败" :allow-retry="false" />
        <NDataTable
          v-if="events.length > 0"
          :columns="eventColumns"
          :data="events"
          :row-key="(row: AlertEvent) => row.id"
          size="small"
        />
        <StatePanel
          v-else
          state="empty"
          title="还没有触发过任何告警"
          description="事件只记录真实的超限区间；没有数据时不会凭空产生一条“正常”的记录。"
        />
      </template>
    </NCard>

    <!-- 告警规则 -->
    <NCard title="告警规则" size="small" class="alerts-view__card">
      <NDataTable
        v-if="rules.length > 0"
        :columns="ruleColumns"
        :data="rules"
        :row-key="(row: AlertRule) => row.id"
        size="small"
      />
      <StatePanel
        v-else-if="!alertsError"
        state="empty"
        title="还没有配置告警规则"
        description="规则通过接口管理（POST /alerts/rules）。没有规则就不会有事件，也不会有通知。"
      />
      <p v-if="alertsError" class="alerts-view__meta">规则读不到：{{ alertsError.message }}</p>
    </NCard>

    <!-- 投递账 / 面板内通知 -->
    <NCard v-if="session.isAdmin" title="通知投递账" size="small" class="alerts-view__card">
      <p class="alerts-view__meta">
        每次投递一行。面板内通知写入即“已投递”（不往外发）；邮件与 webhook 由代理发送，
        失败的对端原话原样留在这里。投递账是面板自己的账，代理连不上也读得到。
      </p>
      <ApiErrorPanel
        v-if="deliveriesError"
        :error="deliveriesError"
        title="读不到投递账"
        @retry="loadDeliveries"
      />
      <NDataTable
        v-else-if="deliveries.length > 0"
        :columns="deliveryColumns"
        :data="deliveries"
        :row-key="(row: NotificationDelivery) => row.id"
        size="small"
      />
      <StatePanel
        v-else
        state="empty"
        title="还没有任何投递记录"
        description="事件触发时才会入队；维护静默期间的事件一条都不入队。"
        :retryable="deliveriesLoading === false"
        @retry="loadDeliveries"
      />
    </NCard>
  </section>
</template>

<style scoped>
.alerts-view {
  display: flex;
  flex-direction: column;
  gap: var(--sp-4);
}

.alerts-view__lead {
  flex: 1 0 100%;
  min-width: 0;
}

.alerts-view__header h1 {
  margin: 0;
  font-size: var(--fs-xl);
}

.alerts-view__tiles {
  display: flex;
  flex-wrap: wrap;
}

/* 三格读数铺满整行：NSpace 的子元素默认按内容宽排，三个 0 挤在左边
   看起来像三个残片，而不是这一页的三个总览读数。 */
.alerts-view__tiles > * {
  flex: 1 1 200px;
  min-width: 0;
}

.alerts-view__tile {
  min-width: 140px;
  padding: 12px 16px;
  border: 1px solid var(--rule);
  border-radius: var(--r-2);
}

.alerts-view__tile-value {
  font-size: var(--fs-xl);
  font-weight: 600;
}

.alerts-view__tile-label {
  color: var(--ink-3);
  font-size: var(--fs-sm);
}

.alerts-view__charts {
  display: flex;
  flex-direction: column;
  gap: 20px;
}

.alerts-view__chart-group h2 {
  margin: 0 0 8px;
  font-size: var(--fs-lg);
  color: var(--ink-2);
}

.alerts-view__chart {
  margin-bottom: 12px;
}

.alerts-view__chart-head {
  display: flex;
  align-items: baseline;
  justify-content: space-between;
  gap: 8px;
  margin-bottom: 2px;
}

.alerts-view__chart-title {
  font-size: var(--fs-md);
}

.alerts-view__chart-meta {
  color: var(--ink-3);
  font-size: var(--fs-xs);
}
</style>
