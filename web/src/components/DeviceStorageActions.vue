<script setup lang="ts">
// 设备页的写操作区（P07.5）：初始化 GPT、新建分区、删除分区、格式化。
//
// 三件事在这里定死，页面各处分头实现就一定会跑偏：
//
//  1. **按钮的可点状态完全来自 capabilities**（P07.1 起由判决层投影，30 秒缓存）。
//     unsupported（对整盘格式化、对分区初始化分区表）连按钮都不画；blocked 画出来但灰着，
//     旁边照抄服务端给的 reason；载荷里没有这个动作时也说清楚是版本不一致，不猜。
//     但真正的放行判断永远在预检——这里只是“别让人点了才知道不行”。
//  2. **参数在页面就把形状拼对**：字节一律十进制字符串、分区起点/大小按 1 MiB 对齐、
//     分区名与卷标按各自的实际上限校验。服务端仍然独立校验一次，这里是省一次往返。
//  3. **破坏性动作没有捷径**：初始化、删除、格式化都要服务端生成的确认短语；
//     新建分区不覆盖任何东西，因此不需要短语（CONTRACTS.md 第 5 节）。

import { computed, ref, watch } from 'vue'
import { NButton, NCard, NInput, NInputNumber, NSelect } from 'naive-ui'
import { RouterLink } from 'vue-router'

import {
  FILESYSTEM_CHOICES,
  FILESYSTEM_LABEL_LIMITS,
  STORAGE_ACTIONS,
  STORAGE_ACTION_LABELS,
  capabilityGate,
  checkFilesystemLabel,
  checkPartitionSize,
  createPartitionParams,
  deletePartitionParams,
  formatParams,
  initTableParams,
  isValidPartitionName,
  maxMibInRegion,
  mibToBytes,
  storageActionNeedsReauth,
  type FilesystemChoice,
  type StorageDevice,
  type StorageFreeRegion,
  type StoragePartition,
} from '@/api/storage'
import { formatBytes } from '@/utils/format'
import StorageActionDialog from './StorageActionDialog.vue'

const props = withDefaults(
  defineProps<{
    device: StorageDevice
    /** 这台设备上登记过的受管理挂载点；有就提醒先卸载并退出管理。 */
    volumeMountPaths?: string[]
    /** 只有管理员能提交写动作；只读账号看到的是“为什么看不到”。 */
    canAct?: boolean
  }>(),
  { volumeMountPaths: () => [], canAct: true },
)

const emit = defineEmits<{ changed: [] }>()

interface Pending {
  action: string
  title: string
  params: Record<string, unknown>
  lines: string[]
  irreversible: boolean
}

const pending = ref<Pending | null>(null)
const dialogShow = ref(false)

// ---- 初始化分区表 -------------------------------------------------------
const initGate = computed(() => capabilityGate(props.device, STORAGE_ACTIONS.initTable))

// ---- 新建分区 -----------------------------------------------------------
const createGate = computed(() => capabilityGate(props.device, STORAGE_ACTIONS.createPartition))
// 一台设备必定有四个动作判定（P07.1 起由判决层投影）；一个都没有说明两侧版本不一致。
const hasCapabilities = computed(() => props.device.capabilities.length > 0)
const regions = computed<StorageFreeRegion[]>(() => props.device.partition_table?.free_regions ?? [])
const regionKey = (region: StorageFreeRegion): string => `${region.start_bytes}:${region.size_bytes}`
const selectedRegionKey = ref<string>('')
const sizeMib = ref<number | null>(null)
const partitionName = ref('')

const selectedRegion = computed<StorageFreeRegion | null>(() => {
  const key = selectedRegionKey.value
  if (key === '') return null
  return regions.value.find((region) => regionKey(region) === key) ?? null
})
const regionMaxMib = computed(() => (selectedRegion.value ? maxMibInRegion(selectedRegion.value.size_bytes) : 0))
const sizeCheck = computed(() =>
  selectedRegion.value === null
    ? { ok: false, reason: '先选一个空闲区' }
    : checkPartitionSize(selectedRegion.value.size_bytes, sizeMib.value ?? 0),
)
const nameOk = computed(() => isValidPartitionName(partitionName.value))
const regionOptions = computed(() =>
  regions.value.map((region) => ({
    label: `从 ${formatBytes(region.start_bytes)} 起，可用 ${formatBytes(region.size_bytes)}`,
    value: regionKey(region),
  })),
)

function pickRegion(key: string): void {
  selectedRegionKey.value = key
  const region = regions.value.find((item) => regionKey(item) === key)
  sizeMib.value = region ? maxMibInRegion(region.size_bytes) : null
}

// 盘上的空闲区变了（刚初始化完、刚删完分区）就把选择重置到第一个，
// 否则会拿一个已经不存在的空闲区去建分区。
watch(
  regions,
  (list) => {
    if (list.length === 0) {
      selectedRegionKey.value = ''
      sizeMib.value = null
      return
    }
    const stillThere = list.some((region) => regionKey(region) === selectedRegionKey.value)
    if (!stillThere) {
      const first = list[0]
      if (first !== undefined) pickRegion(regionKey(first))
    }
  },
  { immediate: true },
)

// ---- 删除分区 -----------------------------------------------------------
// 目标必须是**分区**设备。整盘页面上不提供“删某个分区”的按钮：
// 那等于在看不到那块分区文件系统的情况下删它，风险与收益不对等。
const deleteGate = computed(() => capabilityGate(props.device, STORAGE_ACTIONS.deletePartition))
const partitions = computed<StoragePartition[]>(() => props.device.partition_table?.partitions ?? [])

// ---- 格式化 -------------------------------------------------------------
const formatGate = computed(() => capabilityGate(props.device, STORAGE_ACTIONS.format))
const filesystem = ref<FilesystemChoice>('ext4')
const label = ref('')
const filesystemOptions = FILESYSTEM_CHOICES.map((value) => ({ label: value, value }))
const labelCheck = computed(() => checkFilesystemLabel(filesystem.value, label.value))

// ---- 打开确认框 ---------------------------------------------------------

function openInit(): void {
  if (initGate.value.gate !== 'enabled') return
  pending.value = {
    action: STORAGE_ACTIONS.initTable,
    title: `初始化分区表 · ${props.device.id}`,
    params: initTableParams({ deviceId: props.device.id }),
    lines: [
      `把 ${props.device.id}（${formatBytes(props.device.size_bytes)}）的分区表写成 GPT。`,
      '只在这块盘现在是空的（没有分区表、没有文件系统签名、没有被挂载）时才放行。',
      '写下去之后，盘上原有的分区表与分区条目都会消失，数据不会因此被擦除但会失去索引。',
    ],
    irreversible: true,
  }
  dialogShow.value = true
}

function openCreate(): void {
  const region = selectedRegion.value
  const check = sizeCheck.value
  if (createGate.value.gate !== 'enabled' || region === null) return
  if (!check.ok || check.sizeBytes === undefined || !nameOk.value) return
  pending.value = {
    action: STORAGE_ACTIONS.createPartition,
    title: `新建分区 · ${props.device.id}`,
    params: createPartitionParams({
      deviceId: props.device.id,
      startBytes: region.start_bytes,
      sizeBytes: check.sizeBytes,
      ...(partitionName.value.trim() === '' ? {} : { name: partitionName.value }),
    }),
    lines: [
      `在 ${props.device.id} 的空闲区里新建一个分区：起点 ${formatBytes(region.start_bytes)}，大小 ${formatBytes(check.sizeBytes)}。`,
      '起点与大小都按 1 MiB 对齐；只动这一段还没被任何分区占用的空间。',
      '新建分区不覆盖任何已有数据，所以不需要确认短语。',
    ],
    irreversible: false,
  }
  dialogShow.value = true
}

function openDelete(): void {
  if (deleteGate.value.gate !== 'enabled') return
  const device = props.device
  const fsType = device.filesystem?.type ?? null
  const uuid = device.filesystem?.uuid ?? null
  pending.value = {
    action: STORAGE_ACTIONS.deletePartition,
    title: `删除分区 · ${device.id}`,
    params: deletePartitionParams({ deviceId: device.id }),
    lines: [
      `删除分区 ${device.id}（${formatBytes(device.size_bytes)}${fsType === null ? '' : `，现在是 ${fsType}${uuid === null ? '' : `，UUID ${uuid}`}`}）。`,
      '只删分区表里的这条条目，不写零、不擦数据；但这条分区上的文件系统会从这台机器上消失。',
    ],
    irreversible: true,
  }
  dialogShow.value = true
}

function openFormat(): void {
  if (formatGate.value.gate !== 'enabled' || !labelCheck.value.ok) return
  const trimmed = label.value.trim()
  pending.value = {
    action: STORAGE_ACTIONS.format,
    title: `格式化 · ${props.device.id}`,
    params: formatParams({
      deviceId: props.device.id,
      filesystem: filesystem.value,
      ...(trimmed === '' ? {} : { label: label.value }),
    }),
    lines: [
      `把 ${props.device.id}（${formatBytes(props.device.size_bytes)}）格式化成 ${filesystem.value}${trimmed === '' ? '' : `，卷标 ${trimmed}`}。`,
      '格式化会写新的文件系统：这块分区上原有的文件全部不可恢复。',
    ],
    irreversible: true,
  }
  dialogShow.value = true
}

function onFinished(): void {
  pending.value = null
  emit('changed')
}
</script>

<template>
  <NCard title="可执行的动作" size="small" data-testid="device-actions">
    <p class="actions__muted">
      能点哪些由服务端的判决决定（capabilities，30 秒缓存）；真正放行的是点下去之后的预检。
      这四个动作都要 5 分钟内的密码再认证。
    </p>

    <p v-if="volumeMountPaths && volumeMountPaths.length > 0" class="actions__muted">
      这台设备上登记了受管理卷（{{ volumeMountPaths.join('、') }}）：要重新分区或格式化，
      先到存储页把那个卷卸载并退出管理。
    </p>

    <p v-if="!canAct" class="actions__muted" data-testid="device-actions-readonly">
      只读账号可以看 capabilities 与分区表，但这四个动作都要求管理员。
    </p>

    <!--
      载荷里一个动作都没报告 = 代理与页面版本不一致（P07.1 起必定给四个）。
      这时四个分区各画一颗“服务端没有报告这个动作”的灰按钮只是噪音，
      所以整块收成一句话，别让人以为这台设备真的没有可做的事。
    -->
    <p v-if="canAct && !hasCapabilities" class="actions__muted" data-testid="device-actions-unreported">
      这台设备的载荷里没有任何动作判定（capabilities 为空）。这通常意味着代理与页面版本不一致，
      而不是“这块盘没有可做的事”。刷新一次；仍然为空就先检查两侧版本，别在这里动手。
    </p>

    <!-- 初始化分区表 -->
    <section v-if="canAct && hasCapabilities && initGate.gate !== 'hidden'" class="actions__section">
      <p class="actions__label">分区表</p>
      <p v-if="initGate.gate === 'disabled'" class="actions__muted">
        现在不能初始化：{{ initGate.reason }}
      </p>
      <p v-else class="actions__muted">
        这块盘现在是空的（没有分区表、没有文件系统签名），可以先写一个 GPT 分区表。
      </p>
      <NButton
        size="small"
        :disabled="initGate.gate !== 'enabled'"
        data-action="storage.partition_table.initialize"
        @click="openInit"
      >
        {{ STORAGE_ACTION_LABELS['storage.partition_table.initialize'] }}
      </NButton>
    </section>

    <!-- 新建分区 -->
    <section v-if="canAct && hasCapabilities && createGate.gate !== 'hidden'" class="actions__section">
      <p class="actions__label">新建分区</p>
      <p v-if="createGate.gate === 'disabled'" class="actions__muted">
        现在不能新建分区：{{ createGate.reason }}
      </p>
      <template v-else>
        <p v-if="regions.length === 0" class="actions__muted">
          这块盘上没有可用的空闲区（分区表里没有空位，或者盘上还没有分区表）。
        </p>
        <template v-else>
          <div class="actions__form">
            <NSelect
              :value="selectedRegionKey"
              :options="regionOptions"
              size="small"
              data-testid="free-region"
              @update:value="pickRegion"
            />
            <NInputNumber
              v-model:value="sizeMib"
              size="small"
              :min="1"
              :max="regionMaxMib"
              :step="1"
              data-testid="partition-size-mib"
            />
            <NInput
              v-model:value="partitionName"
              size="small"
              placeholder="分区名（可空，最多 36 个可打印字符）"
              data-testid="partition-name"
            />
            <NButton
              size="small"
              :disabled="!sizeCheck.ok || !nameOk"
              data-action="storage.partition.create"
              @click="openCreate"
            >
              {{ STORAGE_ACTION_LABELS['storage.partition.create'] }}
            </NButton>
          </div>
          <p v-if="!sizeCheck.ok && selectedRegion !== null" class="actions__warn">{{ sizeCheck.reason }}</p>
          <p v-if="!nameOk" class="actions__warn">
            分区名只能包含可打印 ASCII，不能含双引号或反斜杠，最多 36 个字符。
          </p>
          <p v-if="sizeCheck.ok && sizeCheck.sizeBytes !== undefined && selectedRegion !== null" class="actions__muted">
            将请求：起点 {{ selectedRegion.start_bytes }} 字节，大小 {{ sizeCheck.sizeBytes }} 字节
            （{{ sizeMib }} MiB，{{ formatBytes(mibToBytes(sizeMib ?? 0)) }}）。
          </p>
        </template>
      </template>
    </section>

    <!-- 删除分区 -->
    <section v-if="canAct && hasCapabilities && deleteGate.gate !== 'hidden'" class="actions__section">
      <p class="actions__label">删除分区</p>
      <p v-if="deleteGate.gate === 'disabled'" class="actions__muted">
        现在不能删除：{{ deleteGate.reason }}
      </p>
      <p v-else class="actions__muted">
        删除的是这台分区设备本身。删除后会留下一个空闲区，盘上其他分区不受影响。
      </p>
      <NButton
        size="small"
        :disabled="deleteGate.gate !== 'enabled'"
        data-action="storage.partition.delete"
        @click="openDelete"
      >
        {{ STORAGE_ACTION_LABELS['storage.partition.delete'] }}
      </NButton>
    </section>

    <!-- 格式化 -->
    <section v-if="canAct && hasCapabilities && formatGate.gate !== 'hidden'" class="actions__section">
      <p class="actions__label">格式化</p>
      <p v-if="formatGate.gate === 'disabled'" class="actions__muted">
        现在不能格式化：{{ formatGate.reason }}
      </p>
      <template v-else>
        <div class="actions__form">
          <NSelect v-model:value="filesystem" :options="filesystemOptions" size="small" data-testid="filesystem" />
          <NInput v-model:value="label" size="small" placeholder="卷标（可空）" data-testid="filesystem-label" />
          <NButton
            size="small"
            :disabled="formatGate.gate !== 'enabled' || !labelCheck.ok"
            data-action="storage.filesystem.format"
            @click="openFormat"
          >
            {{ STORAGE_ACTION_LABELS['storage.filesystem.format'] }}
          </NButton>
        </div>
        <p v-if="!labelCheck.ok" class="actions__warn">{{ labelCheck.reason }}</p>
        <p v-else class="actions__muted">
          卷标上限按文件系统算：ext4 {{ FILESYSTEM_LABEL_LIMITS.ext4 }} 字节、
          XFS {{ FILESYSTEM_LABEL_LIMITS.xfs }} 字节（超限会被 mkfs 静默截断）。
        </p>
      </template>
    </section>

    <!-- 整盘上已有的分区：给出到各自页面的入口，不在这里“猜”删哪一块。 -->
    <section v-if="device.kind === 'disk' && partitions.length > 0" class="actions__section">
      <p class="actions__label">这块盘上的分区</p>
      <ul class="actions__list">
        <li v-for="partition in partitions" :key="partition.device_id">
          <RouterLink :to="`/storage/devices/${partition.device_id}`">{{ partition.device_id }}</RouterLink>
          <span class="actions__muted">
            起点 {{ formatBytes(partition.start_bytes) }}，大小 {{ formatBytes(partition.size_bytes) }}
          </span>
        </li>
      </ul>
      <p class="actions__muted">
        删除分区与格式化都在分区自己的页面上做——那里能看清它现在是什么文件系统、有没有被挂载。
      </p>
    </section>

    <StorageActionDialog
      v-if="pending"
      v-model:show="dialogShow"
      :action="pending.action"
      :title="pending.title"
      :params="pending.params"
      :confirm-lines="pending.lines"
      :irreversible="pending.irreversible"
      :needs-reauth="storageActionNeedsReauth(pending.action)"
      @finished="onFinished"
    />
  </NCard>
</template>

<style scoped>
.actions__muted {
  margin: 0;
  font-size: var(--fs-md);
  opacity: 0.8;
  line-height: 1.7;
}

.actions__section {
  display: flex;
  flex-direction: column;
  gap: 8px;
  padding: 12px 0;
  border-top: 1px solid var(--surface-2);
}

.actions__section:first-of-type {
  border-top: none;
}

.actions__label {
  margin: 0;
  font-weight: 600;
}

.actions__form {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 8px;
}

.actions__warn {
  margin: 0;
  font-size: var(--fs-md);
  color: var(--bad);
  line-height: 1.7;
}

.actions__list {
  margin: 0;
  padding-left: 18px;
  line-height: 1.8;
}
</style>
