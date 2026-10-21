<script setup lang="ts">
// 一台设备的列表卡片。
//
// 列表页只给“够用来做判断”的信息：身份、容量、挂载、健康、速率。
// 详情页负责把分区表、空闲区、依赖关系摊开。
//
// 两个不可妥协的点：
//   - 容量与大小是十进制字节串，悬停显示精确字节（PLAN.md 第 7 节）；
//   - 没读到容量就写“容量未知”，不画 0%。

import { computed } from 'vue'
import { NTag } from 'naive-ui'
import { RouterLink } from 'vue-router'

import {
  DEVICE_KIND_LABELS,
  healthNeedsAttention,
  mountsOf,
  type StorageDevice,
  type StorageMount,
} from '@/api/storage'
import CapacityBar from '@/components/CapacityBar.vue'
import HealthBadge from '@/components/HealthBadge.vue'
import { UNKNOWN_TEXT, bytesTitle, formatBytes, formatRate } from '@/utils/format'

const props = defineProps<{ device: StorageDevice }>()

/** 展示用挂载点：优先“最满的那个”，没有容量读数时退到第一个挂载点。 */
const shownMount = computed<StorageMount | null>(() => {
  const mounts = mountsOf(props.device)
  let best: StorageMount | null = null
  for (const mount of mounts) {
    if (best === null) {
      best = mount
      continue
    }
    const bestPercent = best.capacity?.used_percent ?? -1
    const percent = mount.capacity?.used_percent ?? -1
    if (percent > bestPercent) best = mount
  }
  return best
})

const extraMounts = computed(() => Math.max(0, mountsOf(props.device).length - 1))
const rate = computed(() => props.device.io?.rate ?? null)
const media = computed(() => (props.device.rotational ? '旋转介质（HDD）' : '非旋转介质（SSD）'))
const kindLabel = computed(() => DEVICE_KIND_LABELS[props.device.kind])
const deviceLink = computed(() => `/storage/devices/${props.device.id}`)
</script>

<template>
  <article class="device-card" :data-device-id="device.id" :data-health="device.health.status">
    <header class="device-card__head">
      <RouterLink class="device-card__name" :to="deviceLink">{{ device.name }}</RouterLink>
      <NTag size="small">{{ kindLabel }}</NTag>
      <NTag v-if="device.protection.system_disk" size="small" type="warning">系统盘（受保护）</NTag>
      <NTag v-if="device.read_only" size="small" type="info">只读</NTag>
      <NTag v-if="!device.online" size="small" type="error">离线</NTag>
      <!--
        只有真的“有问题”才挂告警标签。unknown 是“没读到”、unsupported 是“这类设备不适用”
        （分区、loop、光驱），把它们写成“不通过”是在替设备下结论。
      -->
      <NTag v-if="healthNeedsAttention(device.health.status)" size="small" type="error">
        健康需要处理
      </NTag>
    </header>

    <p class="device-card__line">
      型号：{{ device.model ?? '未读到' }} · 接口：{{ device.transport ?? UNKNOWN_TEXT }} ·
      {{ media }}
    </p>

    <p class="device-card__line" :title="bytesTitle(device.size_bytes)">
      容量：<strong>{{ formatBytes(device.size_bytes) }}</strong>
      <span class="device-card__muted">
        （扇区 {{ device.logical_sector_bytes }} / {{ device.physical_sector_bytes }} 字节）
      </span>
    </p>

    <HealthBadge :health="device.health" />

    <div class="device-card__capacity">
      <template v-if="shownMount">
        <p class="device-card__line">
          挂载点：<code>{{ shownMount.path }}</code>（{{ shownMount.fstype
          }}<span v-if="shownMount.read_only">，只读挂载</span>）
          <span v-if="extraMounts > 0" class="device-card__muted">另有 {{ extraMounts }} 个挂载点</span>
        </p>
        <CapacityBar
          :capacity="shownMount.capacity"
          :error="shownMount.capacity_error"
          :show-timestamp="true"
        />
      </template>
      <p v-else class="device-card__line device-card__muted">
        未挂载{{ device.filesystem?.type ? `（文件系统 ${device.filesystem.type}，无挂载点）` : '' }}。
      </p>
    </div>

    <p class="device-card__line device-card__muted">
      I/O：读 {{ rate ? formatRate(rate.read_bytes_per_second) : UNKNOWN_TEXT }} · 写
      {{ rate ? formatRate(rate.write_bytes_per_second) : UNKNOWN_TEXT }}
      <span v-if="rate === null">（没有可用采样窗口，不是 0）</span>
    </p>

    <p class="device-card__line device-card__muted">
      序列号：{{ device.serial ?? '未读到' }} · 身份可信度：{{ device.identity_confidence }} ·
      标识 {{ device.identity_fingerprint.slice(0, 19) }}…
    </p>
  </article>
</template>

<style scoped>
.device-card {
  display: flex;
  flex-direction: column;
  gap: 8px;
  padding: 14px 16px;
  border: 1px solid rgba(128, 128, 128, 0.25);
  border-radius: 10px;
  background: rgba(127, 127, 127, 0.04);
}

.device-card__head {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 8px;
}

.device-card__name {
  font-size: 16px;
  font-weight: 700;
  text-decoration: none;
}

.device-card__line {
  margin: 0;
  line-height: 1.7;
}

.device-card__muted {
  font-size: 12px;
  opacity: 0.75;
}

.device-card__capacity {
  display: flex;
  flex-direction: column;
  gap: 6px;
}
</style>
