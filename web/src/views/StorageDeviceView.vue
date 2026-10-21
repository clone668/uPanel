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
import ApiErrorPanel from '@/components/ApiErrorPanel.vue'
import DeviceDetailPanel from '@/components/DeviceDetailPanel.vue'
import StatePanel from '@/components/StatePanel.vue'
import { createPoller, type Poller } from '@/utils/polling'
import { apiPanelState, retryablePanelState } from '@/utils/viewstate'

type Phase = 'loading' | 'ready' | 'missing' | 'error'

const DEFAULT_INTERVAL_MS = 5000

const route = useRoute()
const deviceId = computed(() => {
  const raw = route.params.id
  return Array.isArray(raw) ? (raw[0] ?? '') : (raw ?? '')
})

const phase = ref<Phase>('loading')
const error = ref<ApiError | null>(null)
const detail = ref<StorageDeviceDetail | null>(null)

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
  phase.value = 'loading'
  poller.trigger()
})

onMounted(() => {
  poller.start()
})

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
      />
    </template>
  </section>
</template>

<style scoped>
.device {
  display: flex;
  flex-direction: column;
  gap: 16px;
  max-width: 1100px;
}

.device__head {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 12px;
}

.device__title {
  margin: 0;
  font-size: 20px;
}

.device__line {
  margin: 0;
  line-height: 1.7;
}

.device__muted {
  font-size: 13px;
  opacity: 0.75;
}
</style>
