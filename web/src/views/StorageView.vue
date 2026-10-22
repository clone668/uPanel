<script setup lang="ts">
// 磁盘列表（P04.4）。
//
// 数据来自 GET /storage/topology 的一份快照：nodes、edges 与 network_mounts 同源，
// 所以列表里的“受保护”“依赖谁”与详情页永远一致。
//
// 显示规则：
//   - loop 与光盘默认收起（数量照实写出来），可以展开，不会被悄悄丢掉；
//   - 没有块设备时是“空数据”，不是错误；
//   - 扫描失败（status=error）时明确写出“显示的是上一次成功的快照”；
//   - 写操作入口只出现在管理员视图里，且每次都要过预检（P07.5）。

import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { NCard, NCheckbox, NTag } from 'naive-ui'

import { ApiError } from '@/api/client'
import {
  attentionDevices,
  fetchStorageTopology,
  needsAttentionNetworkMount,
  reachabilityLabel,
  splitDevices,
  type StorageTopology,
} from '@/api/storage'
import ApiErrorPanel from '@/components/ApiErrorPanel.vue'
import CapacityBar from '@/components/CapacityBar.vue'
import DeviceCard from '@/components/DeviceCard.vue'
import StatePanel from '@/components/StatePanel.vue'
import VolumeSection from '@/components/VolumeSection.vue'
import { useSessionStore } from '@/stores/session'
import { formatSampleSummary, formatTimestamp } from '@/utils/format'
import { createPoller, type Poller } from '@/utils/polling'
import { apiPanelState, retryablePanelState } from '@/utils/viewstate'

type Phase = 'loading' | 'ready' | 'error'

const DEFAULT_INTERVAL_MS = 5000

const session = useSessionStore()
const phase = ref<Phase>('loading')
const error = ref<ApiError | null>(null)
const topology = ref<StorageTopology | null>(null)
const showBackground = ref(false)

const panelState = computed(() => (error.value ? apiPanelState(error.value) : null))

const split = computed(() => splitDevices(topology.value?.nodes ?? []))
// “纳入管理”的候选分区来自同一份快照：不额外请求，避免两处设备列表说法不一致。
const allDevices = computed(() => topology.value?.nodes ?? [])
const networkMounts = computed(() => topology.value?.network_mounts ?? [])
const attention = computed(() => attentionDevices(topology.value?.nodes ?? []))
const sampleLine = computed(() =>
  topology.value
    ? formatSampleSummary(topology.value.collected_at, topology.value.interval_ms)
    : '',
)
const unreachableCount = computed(
  () => networkMounts.value.filter((mount) => needsAttentionNetworkMount(mount)).length,
)

async function load(): Promise<void> {
  try {
    const result = await fetchStorageTopology()
    topology.value = result.data
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
  intervalMs: () => topology.value?.interval_ms ?? DEFAULT_INTERVAL_MS,
})

onMounted(() => {
  poller.start()
})

onBeforeUnmount(() => {
  poller.stop()
})
</script>

<template>
  <section class="storage">
    <div class="storage__head">
      <h1 class="storage__title">存储</h1>
      <NCheckbox v-model:checked="showBackground">
        显示循环设备与光驱（{{ split.background.length }}）
      </NCheckbox>
      <NTag v-if="topology?.status === 'error'" size="small" type="warning">扫描失败</NTag>
      <NTag v-if="attention.length > 0" size="small" type="warning">
        {{ attention.length }} 块盘需要处理
      </NTag>
      <NTag v-if="unreachableCount > 0" size="small" type="warning">
        {{ unreachableCount }} 个网络挂载不可达
      </NTag>
    </div>
    <p v-if="sampleLine" class="storage__meta">{{ sampleLine }}</p>

    <StatePanel v-if="phase === 'loading'" state="loading" title="正在读取磁盘快照" />
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

    <template v-else-if="topology">
      <NCard v-if="topology.status === 'error'" size="small">
        <p class="storage__line">
          最近一次扫描失败：{{ topology.error ?? '代理没有给出原因' }}。
          下面显示的是**上一次成功**的快照（{{ formatTimestamp(topology.collected_at) }}），
          磁盘数量可能已经变化。
        </p>
      </NCard>

      <NCard title="块设备" size="small">
        <template #header-extra>
          <span class="storage__meta">
            快照版本 <code>{{ topology.snapshot_version.slice(0, 23) }}…</code>
          </span>
        </template>

        <StatePanel
          v-if="split.primary.length === 0 && (!showBackground || split.background.length === 0)"
          state="empty"
          title="没有识别到块设备"
          description="这台机器当前没有可展示的磁盘、分区或逻辑设备。"
        />
        <div v-else class="storage__grid" data-testid="device-grid">
          <DeviceCard v-for="device in split.primary" :key="device.id" :device="device" />
          <template v-if="showBackground">
            <DeviceCard v-for="device in split.background" :key="device.id" :device="device" />
          </template>
        </div>
        <p v-if="split.background.length > 0 && !showBackground" class="storage__meta">
          另有 {{ split.background.length }} 台循环设备/光驱已收起。
        </p>
      </NCard>

      <VolumeSection :devices="allDevices" :can-act="session.isAdmin" @changed="load" />

      <NCard title="网络挂载" size="small">
        <p v-if="networkMounts.length === 0" class="storage__meta">
          没有网络挂载（NFS/CIFS/SSHFS…）。本地块设备的挂载点显示在各自的卡片里。
        </p>
        <ul v-else class="storage__mounts" data-testid="network-mounts">
          <li v-for="mount in networkMounts" :key="mount.path" class="storage__mount">
            <p class="storage__line">
              <code>{{ mount.path }}</code>
              <span class="storage__meta">{{ mount.fstype }} · 来源 {{ mount.source }}</span>
              <NTag v-if="mount.read_only" size="small" type="info">只读</NTag>
              <NTag size="small" :type="mount.reachable === false ? 'error' : 'default'">
                {{ reachabilityLabel(mount.reachable) }}
              </NTag>
            </p>
            <CapacityBar :capacity="mount.capacity" :error="mount.capacity_error" />
          </li>
        </ul>
      </NCard>

      <NCard title="关于这些数字" size="small">
        <p class="storage__line">
          每台设备只报告自己在 /proc/diskstats 里的那一行计数，从不把分区与整盘相加
          （分区的 I/O 已经包含在整盘里，相加就是重复计算）。
        </p>
        <p class="storage__line">
          容量默认用 IEC 单位（GiB/TiB），鼠标悬停可以看到精确字节数。
        </p>
        <p class="storage__line">
          写操作没有“直接执行”的接口：每次点下去都先要一份预检（当前状态允不允许、
          有没有占用、锁有没有被别处拿着），预检通过了才提交任务。
          预检结果 120 秒内有效，过期就重新预检——页面不会拿一份旧结论去写盘。
        </p>
        <p class="storage__line">
          破坏性动作（初始化分区表、删分区、格式化）要输入确认短语，并通过二次身份检查；
          卷的挂载/卸载/改配置/退出管理只要求二次身份检查，因为数据不会被销毁。
        </p>
      </NCard>
    </template>
  </section>
</template>

<style scoped>
.storage {
  display: flex;
  flex-direction: column;
  gap: 16px;
  max-width: 1200px;
}

.storage__head {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 10px;
}

.storage__title {
  margin: 0;
  font-size: 20px;
}

.storage__line {
  margin: 0;
  line-height: 1.7;
}

.storage__meta {
  font-size: 13px;
  opacity: 0.8;
  word-break: break-all;
}

.storage__grid {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(320px, 1fr));
  gap: 12px;
}

.storage__mounts {
  margin: 0;
  padding: 0;
  list-style: none;
  display: flex;
  flex-direction: column;
  gap: 12px;
}

.storage__mount {
  display: flex;
  flex-direction: column;
  gap: 6px;
  padding: 10px 12px;
  border: 1px solid rgba(128, 128, 128, 0.2);
  border-radius: 8px;
}
</style>
