<script setup lang="ts">
// 单台设备详情页（P04.4，只读）。
//
// 路由参数是内核设备名（sda、sda1、dm-0）。它只作为查询键使用，不会被拼成路径或命令；
// 服务端对非法 id 返回 400，对不存在的设备返回 404 —— 这里把两种情况都如实显示，
// 尤其不把“设备已经拔掉”画成空数据或错误面板里一句含糊的话。

import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { NButton, NCard, NTag } from 'naive-ui'
import { RouterLink, useRoute } from 'vue-router'

import { ApiError } from '@/api/client'
import {
  fetchStorageDevice,
  type StorageDeviceDetail,
} from '@/api/storage'
import { fetchVolumes, type Volume } from '@/api/volumes'
import ApiErrorPanel from '@/components/ApiErrorPanel.vue'
import DeviceDetailPanel from '@/components/DeviceDetailPanel.vue'
import StatePanel from '@/components/StatePanel.vue'
import { useSessionStore } from '@/stores/session'
import { createPoller, type Poller } from '@/utils/polling'
import { apiPanelState, retryablePanelState } from '@/utils/viewstate'

type Phase = 'loading' | 'ready' | 'missing' | 'error'

const DEFAULT_INTERVAL_MS = 5000

const session = useSessionStore()
const route = useRoute()
const deviceId = computed(() => {
  const raw = route.params.id
  return Array.isArray(raw) ? (raw[0] ?? '') : (raw ?? '')
})

const phase = ref<Phase>('loading')
const error = ref<ApiError | null>(null)
const detail = ref<StorageDeviceDetail | null>(null)
const volumes = ref<Volume[]>([])

// 这台设备上登记过的受管理挂载点。用来在“重新分区/格式化”前提醒先退出管理。
// 卷列表读失败不影响设备详情——它只是提示，不是判断依据。
const volumeMountPaths = computed(() =>
  volumes.value.filter((volume) => volume.device_id === deviceId.value).map((volume) => volume.mount_path),
)

async function loadVolumes(): Promise<void> {
  try {
    const result = await fetchVolumes()
    volumes.value = result.data.volumes
  } catch {
    volumes.value = []
  }
}

const panelState = computed(() => (error.value ? apiPanelState(error.value) : null))

async function load(): Promise<void> {
  const id = deviceId.value
  if (id === '') {
    phase.value = 'missing'
    return
  }
  try {
    const result = await fetchStorageDevice(id)
    // 期间用户切换了设备：这次结果属于上一台，丢掉。
    if (deviceId.value !== id) return
    detail.value = result.data
    phase.value = 'ready'
    error.value = null
  } catch (caught) {
    if (caught instanceof ApiError) {
      if (deviceId.value !== id) return
      error.value = caught
      phase.value = caught.code === 'NOT_FOUND' ? 'missing' : 'error'
      return
    }
    throw caught
  }
}

const poller: Poller = createPoller(load, {
  intervalMs: () => detail.value?.interval_ms ?? DEFAULT_INTERVAL_MS,
})

watch(deviceId, () => {
  detail.value = null
  volumes.value = []
  phase.value = 'loading'
  poller.trigger()
  void loadVolumes()
})

onMounted(() => {
  poller.start()
  void loadVolumes()
})

function onChanged(): void {
  // 动作刚跑完：设备拓扑与受管卷都可能变了，两路都重读，不拿旧快照继续显示。
  poller.trigger()
  void loadVolumes()
}

onBeforeUnmount(() => {
  poller.stop()
})
</script>

<template>
  <section class="device">
    <div class="device__head">
      <RouterLink to="/storage">← 返回磁盘列表</RouterLink>
      <h1 class="device__title">{{ deviceId === '' ? '设备' : deviceId }}</h1>
      <NTag v-if="detail && detail.status === 'error'" size="small" type="warning">扫描失败</NTag>
      <NButton size="small" secondary @click="poller.trigger()">刷新</NButton>
    </div>

    <StatePanel v-if="phase === 'loading' && detail === null" state="loading" title="正在读取设备详情" />

    <StatePanel
      v-else-if="phase === 'missing'"
      state="empty"
      title="设备不存在"
      description="这台设备可能已经被移除（热插拔、重启后编号变化），或者 id 不属于任何块设备。请回到磁盘列表重新选择。"
    />

    <StatePanel
      v-else-if="error && panelState && panelState !== 'error'"
      :state="panelState"
      :description="error.message"
      :code="error.code"
      :request-id="error.requestId"
      :retryable="retryablePanelState(panelState)"
      @retry="load"
    />
    <ApiErrorPanel v-else-if="error" :error="error" @retry="load" />

    <template v-else-if="detail">
      <NCard v-if="detail.status === 'error'" size="small">
        <p class="device__line">
          最近一次扫描失败：{{ detail.error ?? '代理没有给出原因' }}。
          下面显示的是上一次成功的数据，可能与当前状态不一致。
        </p>
      </NCard>

      <DeviceDetailPanel
        :device="detail.device"
        :edges="detail.edges"
        :interval-ms="detail.interval_ms"
        :collected-at="detail.collected_at"
        :volume-mount-paths="volumeMountPaths"
        :can-act="session.isAdmin"
        @changed="onChanged"
      />
    </template>
  </section>
</template>

<style scoped>
.device {
  display: flex;
  flex-direction: column;
  gap: var(--sp-3);
}

.device__muted {
  font-size: var(--fs-sm);
  color: var(--ink-3);
}
</style>
