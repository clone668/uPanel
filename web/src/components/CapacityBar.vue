<script setup lang="ts">
// 容量卡片：把一份 statfs 结果说清楚，且在“没读到”时绝不画成 0%。
//
// CONTRACTS.md 第 6 节：容量按 30 秒缓存，读取失败时整块是 null（不是全 0），
// 所以这里区分两种“空”：
//   - capacity === null → 显示“容量未知” + capacity_error 原话；
//   - capacity 里 used_percent 是 0 → 显示 0%，那是一个真实读数。

import { computed } from 'vue'
import { NProgress } from 'naive-ui'

import type { StorageCapacity } from '@/api/storage'
import { formatBytes, formatPercent, formatTimestamp } from '@/utils/format'

const props = withDefaults(
  defineProps<{
    capacity: StorageCapacity | null
    /** 为什么读不到容量（服务端 capacity_error 原话）。 */
    error?: string | undefined
    /** 这份容量对应的时间戳（可能比拓扑旧，因为按 30 秒缓存）。 */
    showTimestamp?: boolean
  }>(),
  { error: undefined, showTimestamp: false },
)

const total = computed(() => formatBytes(props.capacity?.total_bytes ?? null))
const available = computed(() => formatBytes(props.capacity?.available_bytes ?? null))
const usedPercentText = computed(() =>
  props.capacity === null ? '—' : formatPercent(props.capacity.used_percent, 1),
)
const inodePercentText = computed(() =>
  props.capacity === null ? '—' : formatPercent(props.capacity.inode_used_percent, 1),
)
const inodeAvailable = computed(() => formatBytes(props.capacity?.inode_available ?? null))

// 进度条只接受数字：没读到容量时干脆不画条，避免出现“0% 的长条”。
const percentage = computed(() => {
  if (props.capacity === null) return 0
  return Math.min(100, Math.max(0, props.capacity.used_percent))
})

const barStatus = computed<'default' | 'warning' | 'error'>(() => {
  if (props.capacity === null) return 'default'
  if (props.capacity.used_percent >= 90) return 'error'
  if (props.capacity.used_percent >= 75) return 'warning'
  return 'default'
})

const failureText = computed(() => props.error ?? '本次没有读到容量，也没有给出原因。')
</script>

<template>
  <div class="capacity" :data-has-capacity="capacity !== null">
    <template v-if="capacity !== null">
      <NProgress
        type="line"
        :percentage="percentage"
        :status="barStatus"
        :show-indicator="false"
        :height="8"
      />
      <p class="capacity__line">
        已用 <strong>{{ usedPercentText }}</strong
        >（共 {{ total }}，可用 {{ available }}）
      </p>
      <p class="capacity__line capacity__line--muted">
        inode 已用 {{ inodePercentText }}，剩余 {{ inodeAvailable }}
      </p>
      <p v-if="showTimestamp" class="capacity__line capacity__line--muted">
        容量采集于 {{ formatTimestamp(capacity.collected_at) }}
      </p>
    </template>

    <template v-else>
      <p class="capacity__line capacity__line--unknown">容量未知（不是 0，是没读到）</p>
      <p class="capacity__line capacity__line--muted">{{ failureText }}</p>
    </template>
  </div>
</template>

<style scoped>
.capacity {
  display: flex;
  flex-direction: column;
  gap: 6px;
  min-width: 160px;
}

.capacity__line {
  margin: 0;
  line-height: 1.6;
}

.capacity__line--muted {
  font-size: var(--fs-sm);
  color: var(--ink-3);
}

.capacity__line--unknown {
  font-weight: 600;
}
</style>
