<script setup lang="ts">
// 健康标记：把 health.status 翻成一句人话 + 一个颜色，但不靠颜色传达信息。
//
// CONTRACTS.md 第 6.2 节的规则在这里落地：
//   - status 是封闭集合，六个取值各有中文标签，未知取值不会被说成“正常”；
//   - 温度是 null 时显示“—”，不能显示 0 °C；
//   - details 是来源的原话（smartctl 的 messages），有就显示，不吞掉；
//   - source 与 checked_at 一起显示，避免“这个 healthy 是谁说的、什么时候说的”说不清。

import { computed } from 'vue'
import { NTag } from 'naive-ui'

import {
  HEALTH_STATUS_LABELS,
  HEALTH_STATUS_TYPES,
  healthNeedsAttention,
  type HealthStatus,
  type StorageHealth,
} from '@/api/storage'
import { UNKNOWN_TEXT, formatTemperature, formatTimestamp } from '@/utils/format'

const props = withDefaults(
  defineProps<{
    health: StorageHealth
    /** 是否显示来源、时间与详情（列表里可关掉，详情页打开）。 */
    detailed?: boolean
  }>(),
  { detailed: false },
)

// 每种状态一句解释：状态名本身不够，用户要知道“这意味着什么”。
const EXPLANATIONS: Record<HealthStatus, string> = {
  healthy: '健康数据来源判定通过。',
  warning: '来源报告了需要注意的指标，详见下方说明。',
  critical: '来源判定这块盘有故障风险，请尽快备份并更换。',
  unknown: '没有拿到可用的健康数据，不能当作正常。',
  unsupported: '这块盘或它的桥接方式不支持健康检测。',
  sleeping: '检测前盘处于休眠，为避免唤醒它，本次没有读取健康数据。',
}

const label = computed(() => HEALTH_STATUS_LABELS[props.health.status])
const tagType = computed(() => HEALTH_STATUS_TYPES[props.health.status])
const attention = computed(() => healthNeedsAttention(props.health.status))
const explanation = computed(() => EXPLANATIONS[props.health.status])
const temperature = computed(() => formatTemperature(props.health.temperature_celsius))
const hasTemperature = computed(() => temperature.value !== UNKNOWN_TEXT)
const checkedAt = computed(() => formatTimestamp(props.health.checked_at))
const details = computed(() => props.health.details ?? '')
</script>

<template>
  <div class="health-badge" :data-health="health.status">
    <div class="health-badge__head">
      <span class="health-badge__label">健康</span>
      <NTag size="small" :type="tagType">{{ label }}</NTag>
      <span v-if="attention" class="health-badge__flag">需要处理</span>
    </div>

    <p class="health-badge__line">{{ explanation }}</p>

    <p class="health-badge__line health-badge__line--muted">
      温度：{{ temperature }}<span v-if="!hasTemperature">（没有读数）</span>
    </p>

    <template v-if="detailed">
      <p class="health-badge__line health-badge__line--muted">
        来源：{{ health.source }}；检查时间：{{ checkedAt }}
      </p>
      <p v-if="details" class="health-badge__details">来源原话：{{ details }}</p>
    </template>
  </div>
</template>

<style scoped>
.health-badge {
  display: flex;
  flex-direction: column;
  gap: 4px;
}

.health-badge__head {
  display: flex;
  align-items: center;
  gap: 8px;
}

.health-badge__label {
  font-size: 13px;
  opacity: 0.8;
}

.health-badge__flag {
  font-size: 12px;
  font-weight: 600;
}

.health-badge__line {
  margin: 0;
  line-height: 1.6;
}

.health-badge__line--muted {
  font-size: 13px;
  opacity: 0.8;
}

.health-badge__details {
  margin: 0;
  font-size: 13px;
  word-break: break-word;
}
</style>
