<script setup lang="ts">
// 单台设备详情（只读）。
//
// 这是 P04 里信息最密的一页，顺序按“先确认是不是我以为的那块盘，再看能不能动它”：
//   身份与介质 → 容量与挂载 → 分区表与空闲区 → 健康 → I/O → 保护与依赖 → 可执行的动作。
//
// capabilities 自 P07.1 起是代理判决层的投影（available / blocked / unsupported + reason），
// 这里如实列出它，但按钮的画与灰由 DeviceStorageActions 负责，放行仍然在预检。

import { computed } from 'vue'
import { NCard, NTag } from 'naive-ui'
import { RouterLink } from 'vue-router'

import {
  DEVICE_KIND_LABELS,
  EDGE_RELATION_LABELS,
  IDENTITY_CONFIDENCE_LABELS,
  PARTITION_TABLE_SOURCE_LABELS,
  PARTITION_TABLE_TYPE_LABELS,
  type StorageDevice,
  type StorageEdge,
} from '@/api/storage'
import CapacityBar from '@/components/CapacityBar.vue'
import DeviceStorageActions from '@/components/DeviceStorageActions.vue'
import HealthBadge from '@/components/HealthBadge.vue'
import IoStats from '@/components/IoStats.vue'
import {
  UNKNOWN_TEXT,
  bytesTitle,
  formatBytes,
  formatSampleSummary,
  formatTimestamp,
} from '@/utils/format'

const props = withDefaults(
  defineProps<{
    device: StorageDevice
    edges: StorageEdge[]
    /** 采集节奏，用于页头说明数据多久刷新一次。 */
    intervalMs: number
    /** 快照采集时间（与设备自身的 collected_at 相同，页头用快照那份）。 */
    collectedAt: string
    /** 这台设备上登记过的受管理挂载点（来自卷列表，不是本地推算）。 */
    volumeMountPaths?: string[]
    /** 只有管理员能提交写动作。 */
    canAct?: boolean
  }>(),
  { volumeMountPaths: () => [], canAct: true },
)

const emit = defineEmits<{ changed: [] }>()

const table = computed(() => props.device.partition_table)
const mounts = computed(() => props.device.filesystem?.mounts ?? [])
const capabilities = computed(() => props.device.capabilities)
const protection = computed(() => props.device.protection)
const holders = computed(() => props.device.holders ?? [])
const typeLabel = computed(() => PARTITION_TABLE_TYPE_LABELS[table.value?.type ?? 'unknown'])
const sourceLabel = computed(() =>
  table.value ? PARTITION_TABLE_SOURCE_LABELS[table.value.source] : UNKNOWN_TEXT,
)
const linkTo = (id: string): string => `/storage/devices/${id}`
const kindLabel = computed(() => DEVICE_KIND_LABELS[props.device.kind])
const confidenceLabel = computed(() => IDENTITY_CONFIDENCE_LABELS[props.device.identity_confidence])
</script>

<template>
  <div class="detail" data-testid="device-detail">
    <p class="detail__sample">{{ formatSampleSummary(collectedAt, intervalMs) }}</p>

    <NCard title="身份与介质" size="small">
      <p class="detail__row">
        <span class="detail__label">设备</span>
        <code>{{ device.id }}</code>
        <NTag size="small">{{ kindLabel }}</NTag>
        <NTag v-if="protection.system_disk" size="small" type="warning">系统盘（受保护）</NTag>
        <NTag v-if="!device.online" size="small" type="error">离线</NTag>
        <NTag v-if="device.read_only" size="small" type="info">设备只读</NTag>
      </p>
      <p class="detail__row">
        <span class="detail__label">型号</span>
        <span>{{ device.model ?? '未读到' }}</span>
      </p>
      <p class="detail__row">
        <span class="detail__label">序列号 / WWN</span>
        <span>{{ device.serial ?? '未读到' }} / {{ device.wwn ?? '未读到' }}</span>
      </p>
      <p class="detail__row">
        <span class="detail__label">接口</span>
        <span>{{ device.transport ?? UNKNOWN_TEXT }}</span>
        <span>{{ device.rotational ? '旋转介质（HDD）' : '非旋转介质（SSD）' }}</span>
      </p>
      <p class="detail__row">
        <span class="detail__label">容量</span>
        <span :title="bytesTitle(device.size_bytes)">{{ formatBytes(device.size_bytes) }}</span>
        <span class="detail__muted">
          扇区 {{ device.logical_sector_bytes }} / {{ device.physical_sector_bytes }} 字节
        </span>
      </p>
      <p class="detail__row">
        <span class="detail__label">身份</span>
        <span>{{ confidenceLabel }}</span>
      </p>
      <p class="detail__row">
        <span class="detail__label">稳定标识</span>
        <code class="detail__code">{{ device.identity_fingerprint }}</code>
      </p>
      <p class="detail__row">
        <span class="detail__label">采集于</span>
        <span>{{ formatTimestamp(device.collected_at) }}</span>
      </p>
    </NCard>

    <NCard title="容量与挂载" size="small">
      <p v-if="mounts.length === 0" class="detail__muted">
        没有挂载点{{ device.filesystem?.type ? `（文件系统 ${device.filesystem.type}）` : '' }}。
        未挂载的设备同样会被采集与展示。
      </p>
      <ul v-else class="detail__mounts">
        <li v-for="mount in mounts" :key="mount.path" class="detail__mount">
          <p class="detail__row">
            <span class="detail__label">挂载点</span>
            <code>{{ mount.path }}</code>
            <span>{{ mount.fstype }}</span>
            <NTag v-if="mount.read_only" size="small" type="info">只读挂载</NTag>
          </p>
          <p class="detail__row">
            <span class="detail__label">来源</span>
            <code>{{ mount.source }}</code>
            <span class="detail__muted">选项：{{ mount.options.join(',') }}</span>
          </p>
          <CapacityBar
            :capacity="mount.capacity"
            :error="mount.capacity_error"
            :show-timestamp="true"
          />
        </li>
      </ul>
      <p v-if="device.filesystem" class="detail__row detail__muted">
        文件系统：{{ device.filesystem.type ?? '未读到' }} · UUID：{{ device.filesystem.uuid ?? '未读到' }} ·
        卷标：{{ device.filesystem.label ?? '未读到' }}
      </p>
    </NCard>

    <NCard title="分区表与空闲区" size="small">
      <p v-if="table === null" class="detail__muted">
        这台设备没有可读的分区表信息（分区属于整盘，逻辑卷与分区没有自己的分区表）。
      </p>
      <template v-else>
        <p class="detail__row">
          <span class="detail__label">类型</span>
          <span>{{ typeLabel }}</span>
          <span class="detail__muted">来源：{{ sourceLabel }}</span>
        </p>
        <p v-if="table.warning" class="detail__warning">注意：{{ table.warning }}</p>

        <p class="detail__subhead">内核认定的分区（{{ table.partitions.length }}）</p>
        <p v-if="table.partitions.length === 0" class="detail__muted">没有分区。</p>
        <ul v-else class="detail__list">
          <li v-for="part in table.partitions" :key="part.device_id" class="detail__item">
            <RouterLink :to="linkTo(part.device_id)">{{ part.device_id }}</RouterLink>
            <span class="detail__muted">第 {{ part.number }} 号 · 起点 </span>
            <span :title="bytesTitle(part.start_bytes)">{{ formatBytes(part.start_bytes) }}</span>
            <span class="detail__muted"> · 大小 </span>
            <span :title="bytesTitle(part.size_bytes)">{{ formatBytes(part.size_bytes) }}</span>
          </li>
        </ul>

        <p class="detail__subhead">空闲区（{{ table.free_regions.length }}）</p>
        <p v-if="table.free_regions.length === 0" class="detail__muted">
          盘上分区表里没有空闲区（或本次没有去读盘）。
        </p>
        <ul v-else class="detail__list">
          <li v-for="region in table.free_regions" :key="region.start_bytes" class="detail__item">
            <span class="detail__muted">起点 </span>
            <span :title="bytesTitle(region.start_bytes)">{{ formatBytes(region.start_bytes) }}</span>
            <span class="detail__muted"> · 可用 </span>
            <span :title="bytesTitle(region.size_bytes)">{{ formatBytes(region.size_bytes) }}</span>
          </li>
        </ul>
      </template>
    </NCard>

    <NCard title="健康" size="small">
      <HealthBadge :health="device.health" detailed />
    </NCard>

    <NCard title="I/O" size="small">
      <IoStats :io="device.io" />
    </NCard>

    <NCard title="保护与依赖" size="small">
      <p class="detail__row">
        <span class="detail__label">系统盘保护</span>
        <NTag size="small" :type="protection.system_disk ? 'warning' : 'default'">
          {{ protection.system_disk ? '受保护' : '不受保护' }}
        </NTag>
      </p>
      <ul v-if="protection.reasons.length > 0" class="detail__list">
        <li v-for="reason in protection.reasons" :key="reason" class="detail__item">
          理由：{{ reason }}
        </li>
      </ul>
      <p v-if="protection.sources.length > 0" class="detail__row">
        <span class="detail__label">保护来源</span>
        <span>
          <RouterLink v-for="id in protection.sources" :key="id" :to="linkTo(id)" class="detail__chip">
            {{ id }}
          </RouterLink>
        </span>
      </p>

      <p class="detail__row">
        <span class="detail__label">依赖它的设备</span>
        <span v-if="holders.length === 0" class="detail__muted">没有设备直接依赖它。</span>
        <span v-else>
          <RouterLink v-for="id in holders" :key="id" :to="linkTo(id)" class="detail__chip">
            {{ id }}
          </RouterLink>
        </span>
      </p>

      <p v-if="device.swap" class="detail__row">
        <span class="detail__label">交换区</span>
        <span>
          类型 {{ device.swap.type === 'partition' ? '分区' : '文件' }} · 已用
          {{ formatBytes(device.swap.used_bytes) }} / {{ formatBytes(device.swap.size_bytes) }} ·
          优先级 {{ device.swap.priority }}
        </span>
      </p>
    </NCard>

    <NCard title="拓扑关系" size="small">
      <p v-if="edges.length === 0" class="detail__muted">没有与这台设备直接相关的关系边。</p>
      <ul v-else class="detail__list">
        <li v-for="edge in edges" :key="`${edge.from_device_id}-${edge.to_device_id}-${edge.relation}`" class="detail__item">
          <RouterLink :to="linkTo(edge.from_device_id)">{{ edge.from_device_id }}</RouterLink>
          <span class="detail__muted"> {{ EDGE_RELATION_LABELS[edge.relation] }} </span>
          <RouterLink :to="linkTo(edge.to_device_id)">{{ edge.to_device_id }}</RouterLink>
        </li>
      </ul>
    </NCard>

    <NCard title="服务端判定的动作" size="small">
      <p v-if="capabilities.length === 0" class="detail__muted" data-testid="no-actions">
        服务端没有报告这台设备上的任何动作。可能原因：代理与页面的版本不一致
        （载荷里没有 capabilities 字段），或者这台设备不属于可写操作的种类。
      </p>
      <ul v-else class="detail__list">
        <li v-for="cap in capabilities" :key="cap.action" class="detail__item">
          {{ cap.action }}（{{ cap.state }}）<span v-if="cap.reason" class="detail__muted">：{{ cap.reason }}</span>
        </li>
      </ul>
      <p class="detail__muted">
        这份列表有 30 秒缓存，所以它是“刚才的判定”，不是放行凭据：
        点下去之后的预检会拿现场状态重新算一遍。
      </p>
    </NCard>

    <DeviceStorageActions
      :device="device"
      :volume-mount-paths="volumeMountPaths"
      :can-act="canAct"
      @changed="emit('changed')"
    />
  </div>
</template>

<style scoped>
.detail {
  display: flex;
  flex-direction: column;
  gap: 16px;
}

.detail__sample {
  margin: 0;
  font-size: var(--fs-md);
  opacity: 0.8;
}

.detail__row {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 8px;
  margin: 0;
  line-height: 1.7;
}

.detail__label {
  min-width: 88px;
  font-size: var(--fs-md);
  opacity: 0.7;
}

.detail__muted {
  font-size: var(--fs-md);
  opacity: 0.75;
}

.detail__mounts {
  margin: 0;
  padding: 0;
  list-style: none;
  display: flex;
  flex-direction: column;
  gap: 14px;
}

.detail__mount {
  display: flex;
  flex-direction: column;
  gap: 6px;
  padding: 10px 12px;
  border: 1px solid var(--rule);
  border-radius: var(--r-2);
}

.detail__subhead {
  margin: 12px 0 4px;
  font-size: var(--fs-md);
  font-weight: 600;
}

.detail__list {
  margin: 0;
  padding-left: 18px;
  display: flex;
  flex-direction: column;
  gap: 4px;
}

/* 分区名和后面的读数之间要有间隔："sda1第 1 号" 会读成一个词。 */
.detail__item > :first-child {
  margin-right: 6px;
}

.detail__item {
  line-height: 1.7;
}

.detail__warning {
  margin: 6px 0 0;
  color: var(--warn);
  font-size: var(--fs-md);
}

.detail__code {
  font-size: var(--fs-sm);
  word-break: break-all;
}

.detail__chip {
  display: inline-block;
  margin-right: 8px;
}

.detail__chip:last-child {
  margin-right: 0;
}
</style>
