<script setup lang="ts">
// 服务列表（P05.1，只读 + 详情页操作入口）。
//
// 数据来自 GET /services 的一份快照：代理每 10 秒采集一次 systemctl 的状态，
// 页面按 interval_ms 刷新，并明确写出“这份列表有多旧”。
//
// 显示规则：
//   - 失败的单元排在最前面：列表的第一用途是“看出哪出了问题”；
//   - 受保护单元照样显示，只是明确写“受保护”与原因类别，不给操作入口；
//   - 采集失败（status=error）时说明“显示的是上一次成功的快照”，不假装正常；
//   - 筛选是客户端过滤：services.list 不接受任何参数。

import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { NAlert, NInput, NCard, NTag } from 'naive-ui'

import { ApiError } from '@/api/client'
import {
  fetchServices,
  filterUnits,
  groupUnits,
  summarizeUnits,
  type ServiceListData,
  type ServiceUnit,
} from '@/api/services'
import ApiErrorPanel from '@/components/ApiErrorPanel.vue'
import ServiceUnitCard from '@/components/ServiceUnitCard.vue'
import StatePanel from '@/components/StatePanel.vue'
import { useSessionStore } from '@/stores/session'
import { formatTimestamp } from '@/utils/format'
import { createPoller, type Poller } from '@/utils/polling'
import { apiPanelState, retryablePanelState } from '@/utils/viewstate'

type Phase = 'loading' | 'ready' | 'error'

const DEFAULT_INTERVAL_MS = 10000

const session = useSessionStore()

const phase = ref<Phase>('loading')
const error = ref<ApiError | null>(null)
const data = ref<ServiceListData | null>(null)
const keyword = ref('')

const panelState = computed(() => (error.value ? apiPanelState(error.value) : null))
const units = computed<ServiceUnit[]>(() => data.value?.units ?? [])
const counts = computed(() => summarizeUnits(units.value))
const filtered = computed(() => filterUnits(units.value, keyword.value))
const groups = computed(() => groupUnits(filtered.value))
const sampleLine = computed(() => {
  const list = data.value
  if (!list) return ''
  const seconds = Math.round(list.interval_ms / 1000)
  return `采集于 ${formatTimestamp(list.collected_at)}，每 ${seconds} 秒刷新 · 快照 ${list.snapshot_version.slice(0, 23)}…`
})

async function load(): Promise<void> {
  try {
    const result = await fetchServices()
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

onMounted(() => {
  poller.start()
})

onBeforeUnmount(() => {
  poller.stop()
})
</script>

<template>
  <section class="services">
    <div class="services__head">
      <h1 class="services__title">服务与进程</h1>
      <NTag v-if="data?.status === 'error'" size="small" type="warning">采集失败</NTag>
      <NTag v-if="counts.attention > 0" size="small" type="error">
        {{ counts.attention }} 个单元需要注意
      </NTag>
      <NTag v-if="counts.protected > 0" size="small" type="warning">
        {{ counts.protected }} 个受保护
      </NTag>
    </div>
    <p v-if="sampleLine" class="services__meta">{{ sampleLine }}</p>

    <NInput
      v-model:value="keyword"
      clearable
      placeholder="按单元名或描述筛选（只过滤当前快照，不向 systemctl 传条件）"
    />

    <StatePanel v-if="phase === 'loading'" state="loading" title="正在读取服务列表" />
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
        的快照（{{ formatTimestamp(data.collected_at) }}），运行状态可能已经变化。
      </NAlert>

      <NAlert v-if="data.truncated" type="warning" :bordered="false">
        这台机器的服务数量超过了单次快照上限（1000 个），列表被截断；未显示的部分不代表不存在。
      </NAlert>

      <NCard size="small" title="概览">
        <p class="services__line">
          共 {{ counts.total }} 个单元：运行中 {{ counts.running }} · 未运行 {{ counts.stopped }} ·
          失败 {{ counts.failed }} · 受保护 {{ counts.protected }} · 不在这台机器上
          {{ counts.missing }}。
        </p>
        <p class="services__line services__meta">
          启停、重启与开机启动在单元详情页操作：先看预检给出的影响与阻塞项，再提交任务。
          <template v-if="!session.isAdmin">当前角色是只读（viewer），只能查看状态。</template>
        </p>
      </NCard>

      <StatePanel
        v-if="units.length === 0"
        state="empty"
        title="没有读到任何 service 单元"
        description="这台机器上的 systemd 没有任何 .service 单元，或者采集尚未成功。"
      />
      <StatePanel
        v-else-if="filtered.length === 0"
        state="empty"
        title="没有匹配的单元"
        description="换一个关键字试试；筛选只作用于当前快照。"
      />

      <NCard v-else-if="groups.attention.length > 0" size="small" title="需要注意">
        <div class="services__grid" data-testid="service-group-attention">
          <ServiceUnitCard v-for="unit in groups.attention" :key="unit.unit" :unit="unit" />
        </div>
      </NCard>

      <NCard v-if="groups.running.length > 0" size="small" title="运行中">
        <div class="services__grid" data-testid="service-group-running">
          <ServiceUnitCard v-for="unit in groups.running" :key="unit.unit" :unit="unit" />
        </div>
      </NCard>

      <NCard v-if="groups.stopped.length > 0" size="small" title="未运行">
        <div class="services__grid" data-testid="service-group-stopped">
          <ServiceUnitCard v-for="unit in groups.stopped" :key="unit.unit" :unit="unit" />
        </div>
      </NCard>

      <NCard v-if="groups.other.length > 0" size="small" title="其他状态">
        <div class="services__grid" data-testid="service-group-other">
          <ServiceUnitCard v-for="unit in groups.other" :key="unit.unit" :unit="unit" />
        </div>
      </NCard>
    </template>
  </section>
</template>

<style scoped>
.services {
  display: flex;
  flex-direction: column;
  gap: 16px;
  max-width: 1200px;
}

.services__head {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 10px;
}

.services__title {
  margin: 0;
  font-size: 20px;
}

.services__line {
  margin: 0;
  line-height: 1.7;
}

.services__meta {
  font-size: 13px;
  opacity: 0.8;
  word-break: break-all;
}

.services__grid {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(340px, 1fr));
  gap: 12px;
}
</style>
