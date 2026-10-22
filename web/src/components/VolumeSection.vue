<script setup lang="ts">
// 受管理卷区（P07.5）。
//
// 卷和块设备是两个概念，所以是两个区域：块设备讲“这块盘长什么样”，
// 这一块讲“面板接管了哪些文件系统、它们现在挂没挂”。
//
// 三条硬规矩写进了结构里：
//   1. 只列出服务端报上来的卷——不在本地的卷不会凭空出现，也不会被本地推算出来；
//   2. 加载中就说“正在读取”，不提前画一个“还没有受管的卷”这种假结论；
//   3. 读失败是错误，不是空数据（空数据是“服务端确实返回了零个卷”）。

import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { NCard } from 'naive-ui'

import { ApiError } from '@/api/client'
import type { StorageDevice } from '@/api/storage'
import { fetchVolumes, type Volume } from '@/api/volumes'
import ApiErrorPanel from '@/components/ApiErrorPanel.vue'
import StatePanel from '@/components/StatePanel.vue'
import VolumeCard from '@/components/VolumeCard.vue'
import VolumeImportForm from '@/components/VolumeImportForm.vue'
import { createPoller, type Poller } from '@/utils/polling'

const props = defineProps<{
  devices: readonly StorageDevice[]
  canAct: boolean
}>()

const emit = defineEmits<{ changed: [] }>()

type Phase = 'loading' | 'ready' | 'error'

const DEFAULT_INTERVAL_MS = 5000

const phase = ref<Phase>('loading')
const error = ref<ApiError | null>(null)
const volumes = ref<Volume[]>([])

async function load(): Promise<void> {
  try {
    const result = await fetchVolumes()
    volumes.value = result.data.volumes
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

const poller: Poller = createPoller(load, { intervalMs: () => DEFAULT_INTERVAL_MS })

onMounted(() => {
  poller.start()
})

onBeforeUnmount(() => {
  poller.stop()
})

const countLabel = computed(() => (phase.value === 'ready' ? `${volumes.value.length} 个卷` : '读取中'))

function onVolumeChanged(): void {
  void load()
  emit('changed')
}
</script>

<template>
  <NCard size="small" title="受管卷">
    <template #header-extra>
      <span class="volsection__meta" data-testid="volume-count">{{ countLabel }}</span>
    </template>

    <StatePanel v-if="phase === 'loading'" state="loading" title="正在读取受管卷" />
    <ApiErrorPanel v-else-if="error" :error="error" @retry="load" />

    <template v-else>
      <StatePanel
        v-if="volumes.length === 0"
        state="empty"
        title="还没有受管的卷"
        description="面板只认自己登记过的卷。已经分好区、有文件系统的空闲分区可以在下面“纳入管理”。"
      />
      <div v-else class="volsection__list" data-testid="volume-list">
        <VolumeCard
          v-for="volume in volumes"
          :key="volume.id"
          :volume="volume"
          :can-act="canAct"
          @changed="onVolumeChanged"
        />
      </div>
    </template>

    <VolumeImportForm :devices="props.devices" :can-act="canAct" @finished="onVolumeChanged" />
  </NCard>
</template>

<style scoped>
.volsection__list {
  display: flex;
  flex-direction: column;
  gap: 12px;
}

.volsection__meta {
  font-size: 13px;
  opacity: 0.8;
}
</style>
