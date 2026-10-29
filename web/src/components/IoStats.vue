<script setup lang="ts">
// 设备 I/O 面板（CONTRACTS.md 第 6.2 节）。
//
// 三条规则：
//   - 计数只报这台设备自己那一行，从不相加（分区已含在整盘里，dm 与底层又重复）；
//   - rate 为 null 时说明“没有可用采样窗口”，绝不画成 0 B/s；
//   - 时延为 null 时说明“这个窗口没有完成的读/写”，不是 0 ms。

import { computed } from 'vue'
import { NTag } from 'naive-ui'

import type { StorageIO } from '@/api/storage'
import StatTile from '@/components/StatTile.vue'
import { formatBytes, formatIops, formatLatency, formatRate, formatTimestamp } from '@/utils/format'

const props = defineProps<{ io: StorageIO | null }>()

const rate = computed(() => props.io?.rate ?? null)
const sampledAt = computed(() => formatTimestamp(props.io?.sampled_at ?? null))
const windowMs = computed(() => rate.value?.interval_ms ?? null)
</script>

<template>
  <div class="io" data-testid="io-panel">
    <p v-if="io === null" class="io__line io__line--unknown">
      没有 I/O 数据：本次没有读到这块设备在 /proc/diskstats 里的计数。
    </p>

    <template v-else>
      <div class="io__grid">
        <StatTile label="累计读" :value="formatBytes(io.read_bytes_total)" hint="自本次启动起" />
        <StatTile label="累计写" :value="formatBytes(io.write_bytes_total)" hint="自本次启动起" />
        <StatTile label="读次数" :value="io.reads_total" hint="完成的读请求数" />
        <StatTile label="写次数" :value="io.writes_total" hint="完成的写请求数" />
      </div>

      <template v-if="rate !== null">
        <div class="io__grid">
          <StatTile label="读速率" :value="formatRate(rate.read_bytes_per_second)" />
          <StatTile label="写速率" :value="formatRate(rate.write_bytes_per_second)" />
          <StatTile label="读 IOPS" :value="formatIops(rate.read_iops)" />
          <StatTile label="写 IOPS" :value="formatIops(rate.write_iops)" />
          <StatTile label="读时延" :value="formatLatency(rate.read_latency_ms)" hint="这个窗口的平均值" />
          <StatTile label="写时延" :value="formatLatency(rate.write_latency_ms)" hint="这个窗口的平均值" />
        </div>
        <p class="io__line io__line--muted">
          速率来自相邻两次采样的差值，采样窗口 {{ windowMs }} 毫秒；采样时间 {{ sampledAt }}。
        </p>
      </template>

      <p v-else class="io__line io__line--unknown">
        这一次没有可用的采样窗口（首次采样、计数器变小或窗口异常），
        所以速率未知——不是 0。累计计数仍然有效。
        <NTag size="small" type="info">等待下一次采样</NTag>
      </p>
    </template>
  </div>
</template>

<style scoped>
.io {
  display: flex;
  flex-direction: column;
  gap: 10px;
}

.io__grid {
  display: flex;
  flex-wrap: wrap;
  gap: 8px;
}

.io__line {
  margin: 0;
  line-height: 1.7;
}

.io__line--muted {
  font-size: var(--fs-md);
  opacity: 0.8;
}

.io__line--unknown {
  font-weight: 600;
}
</style>
