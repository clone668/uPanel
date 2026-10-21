<script setup lang="ts">
// 概览页（P04.4）。
//
// 数据全部来自服务端，页面只做两件事：按服务端给的 interval_ms 轮询、把结果画出来。
//   1) /health/live：匿名存活检查，只证明“面板服务能应答”；
//   2) /system/summary：主机、CPU、内存、负载、网络；
//   3) /storage/devices：磁盘数量与健康提醒（详情在存储页）。
//
// 三份数据各自有加载/失败状态：一份挂了不影响另外两份显示，
// 但页头会明确写出“哪一块没取到”，不做“看起来一切正常”的默认。

import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { NButton, NCard, NTag } from 'naive-ui'
import { RouterLink } from 'vue-router'

import { ApiError, get } from '@/api/client'
import { fetchSystemSummary, type SystemSummary } from '@/api/system'
import { attentionDevices, type StorageDeviceList } from '@/api/storage'
import ApiErrorPanel from '@/components/ApiErrorPanel.vue'
import StatePanel from '@/components/StatePanel.vue'
import SystemOverviewPanel from '@/components/SystemOverviewPanel.vue'
import { useModeStore } from '@/stores/mode'
import { formatBytes, formatTimestamp } from '@/utils/format'
import { createPoller, type Poller } from '@/utils/polling'
import { apiPanelState, retryablePanelState } from '@/utils/viewstate'

interface HealthLive {
  status: string
}

type Phase = 'loading' | 'ready' | 'error'

const modeStore = useModeStore()

const healthPhase = ref<Phase>('loading')
const healthError = ref<ApiError | null>(null)
const health = ref<HealthLive | null>(null)
const healthRequestId = ref('')
const healthCheckedAt = ref('')

const summaryPhase = ref<Phase>('loading')
const summaryError = ref<ApiError | null>(null)
const summary = ref<SystemSummary | null>(null)

const storagePhase = ref<Phase>('loading')
const storageError = ref<ApiError | null>(null)
const storage = ref<StorageDeviceList | null>(null)

const DEFAULT_INTERVAL_MS = 5000

const summaryPanelState = computed(() =>
  summaryError.value
    ? apiPanelState(summaryError.value)
    : summaryPhase.value === 'error'
      ? 'error'
      : null,
)
const storagePanelState = computed(() => (storageError.value ? apiPanelState(storageError.value) : null))

const diskCount = computed(() => storage.value?.devices.filter((d) => d.kind === 'disk').length ?? 0)
const needsAttention = computed(() => attentionDevices(storage.value?.devices ?? []).length)
/** 所有整盘的容量之和；一块都没采到时不显示合计，而不是显示 0 B。 */
const totalDiskBytes = computed<string | null>(() => {
  const disks = storage.value?.devices.filter((d) => d.kind === 'disk') ?? []
  if (disks.length === 0) return null
  let total = BigInt(0)
  for (const disk of disks) {
    try {
      total += BigInt(disk.size_bytes)
    } catch {
      // 非法字符串说明契约被破坏，合计就不给，避免显示一个假的数。
      return null
    }
  }
  return total.toString()
})

async function loadHealth(): Promise<void> {
  healthPhase.value = 'loading'
  healthError.value = null
  try {
    const result = await get<HealthLive>('/health/live', { timeoutMs: 8000 })
    health.value = result.data
    healthRequestId.value = result.requestId
    healthCheckedAt.value = new Date().toLocaleString('zh-CN', { hour12: false })
    healthPhase.value = 'ready'
  } catch (caught) {
    if (caught instanceof ApiError) {
      healthError.value = caught
      healthPhase.value = 'error'
      return
    }
    throw caught
  }
}

async function loadSummary(): Promise<void> {
  try {
    const result = await fetchSystemSummary()
    summary.value = result.data
    summaryPhase.value = 'ready'
    summaryError.value = null
  } catch (caught) {
    if (caught instanceof ApiError) {
      summaryError.value = caught
      summaryPhase.value = 'error'
      return
    }
    throw caught
  }
}

async function loadStorage(): Promise<void> {
  try {
    const result = await get<StorageDeviceList>('/storage/devices', { timeoutMs: 10_000 })
    storage.value = result.data
    storagePhase.value = 'ready'
    storageError.value = null
  } catch (caught) {
    if (caught instanceof ApiError) {
      storageError.value = caught
      storagePhase.value = 'error'
      return
    }
    throw caught
  }
}

/** 两份快照并行取，互不阻塞；各自的失败各自显示。 */
async function refreshAll(): Promise<void> {
  await Promise.all([loadSummary(), loadStorage()])
}

const poller: Poller = createPoller(refreshAll, {
  intervalMs: () => summary.value?.interval_ms ?? DEFAULT_INTERVAL_MS,
})

onMounted(() => {
  void loadHealth()
  poller.start()
})

onBeforeUnmount(() => {
  poller.stop()
})
</script>

<template>
  <section class="overview">
    <div class="overview__head">
      <h1 class="overview__title">概览</h1>
      <NTag size="small" :type="modeStore.isMock ? 'warning' : 'success'">
        模式：{{ modeStore.isMock ? 'mock（演示数据）' : 'production' }}
      </NTag>
      <NButton size="small" secondary @click="poller.trigger()">刷新</NButton>
      <p v-if="summary" class="overview__meta">
        采集时间：{{ formatTimestamp(summary.collected_at) }}（每 {{ summary.interval_ms }} 毫秒刷新）
      </p>
    </div>

    <NCard title="面板服务" size="small">
      <StatePanel v-if="healthPhase === 'loading'" state="loading" title="正在检查面板服务" />
      <ApiErrorPanel v-else-if="healthPhase === 'error' && healthError" :error="healthError" @retry="loadHealth" />
      <div v-else-if="health" class="overview__health">
        <p class="overview__line">
          存活检查：<strong>{{ health.status === 'ok' ? '服务可响应' : '状态未知' }}</strong>
        </p>
        <p class="overview__meta">检查时间：{{ healthCheckedAt }}</p>
        <p class="overview__meta">request_id：<code>{{ healthRequestId }}</code></p>
        <p class="overview__meta">
          这一项只说明面板服务能处理请求，不代表采集或写操作可用。
        </p>
      </div>
    </NCard>

    <NCard title="系统概况" size="small">
      <template #header-extra>
        <NTag v-if="summary?.degraded" size="small" type="warning">部分数据不可用</NTag>
      </template>

      <StatePanel v-if="summaryPhase === 'loading'" state="loading" title="正在读取系统快照" />
      <StatePanel
        v-else-if="summaryError && summaryPanelState && summaryPanelState !== 'error'"
        :state="summaryPanelState"
        :description="summaryError.message"
        :code="summaryError.code"
        :request-id="summaryError.requestId"
        :retryable="retryablePanelState(summaryPanelState)"
        @retry="loadSummary"
      />
      <ApiErrorPanel v-else-if="summaryError" :error="summaryError" @retry="loadSummary" />
      <SystemOverviewPanel v-else-if="summary" :summary="summary" />
    </NCard>

    <NCard title="存储" size="small">
      <template #header-extra>
        <RouterLink to="/storage">查看磁盘列表</RouterLink>
      </template>

      <StatePanel v-if="storagePhase === 'loading'" state="loading" title="正在读取磁盘快照" />
      <StatePanel
        v-else-if="storagePanelState && storageError"
        :state="storagePanelState"
        :description="storageError.message"
        :code="storageError.code"
        :request-id="storageError.requestId"
        :retryable="retryablePanelState(storagePanelState)"
        @retry="loadStorage"
      />
      <div v-else-if="storage" class="overview__storage">
        <p class="overview__line">
          整盘 <strong>{{ diskCount }}</strong> 块，共
          <strong>{{ formatBytes(totalDiskBytes) }}</strong>
        </p>
        <p class="overview__line">
          健康需要处理：<strong>{{ needsAttention }}</strong> 块
          <span v-if="needsAttention > 0" class="overview__meta">（详情见存储页）</span>
        </p>
        <p v-if="storage.status === 'error'" class="overview__meta">
          注意：最近一次扫描失败（{{ storage.error ?? '未给出原因' }}），
          下面显示的是上一次成功的快照。
        </p>
        <p class="overview__meta">
          快照版本：<code>{{ storage.snapshot_version.slice(0, 19) }}…</code>
        </p>
      </div>
    </NCard>

    <NCard title="尚未接入" size="small">
      <p class="overview__line">
        异常服务、应用状态、待更新与最近任务在后续阶段接入；没有接入之前，
        它们不出现在导航里，这里也不显示任何示例数字。
      </p>
    </NCard>
  </section>
</template>

<style scoped>
.overview {
  display: flex;
  flex-direction: column;
  gap: 16px;
  max-width: 1100px;
}

.overview__head {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 10px;
}

.overview__title {
  margin: 0;
  font-size: 20px;
}

.overview__health,
.overview__storage {
  display: flex;
  flex-direction: column;
  gap: 4px;
}

.overview__line {
  margin: 0;
  line-height: 1.7;
}

.overview__meta {
  margin: 0;
  font-size: 13px;
  opacity: 0.8;
  word-break: break-all;
}
</style>
