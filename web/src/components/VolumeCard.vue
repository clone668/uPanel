<script setup lang="ts">
// 一个受管理卷的卡片（P07.5）。
//
// 这块面的目的不是“把字段列出来”，而是让人一眼回答三个问题：
//   1. 这块盘现在到底挂没挂？（observed_state，不是 desired_state）
//   2. 它要是没按期望挂上，是为什么？（observed_error / observedStateHint）
//   3. 我现在能对它做什么？（挂载 / 卸载 / 改选项 / 退出管理，全部走预检）
//
// 占用详情要多一次请求（且只有 admin 能读：里面有进程 PID 与命令行），
// 所以默认不查，点“看占用”才查。查不到就照实说“查不到”，不画成“没人用”。

import { computed, ref } from 'vue'
import { NAlert, NButton, NCheckbox, NCard, NTag } from 'naive-ui'

import { ApiError } from '@/api/client'
import {
  VOLUME_ACTIONS,
  VOLUME_ACTION_LABELS,
  VOLUME_DESIRED_STATE_LABELS,
  VOLUME_OBSERVED_STATE_LABELS,
  VOLUME_OBSERVED_STATE_TYPES,
  busySummary,
  configureDiff,
  configureParams,
  desiredMismatch,
  fetchVolumeBlockers,
  forgetParams,
  mountParams,
  observedStateHint,
  unmountParams,
  volumeActionNeedsReauth,
  volumeOptions,
  type Volume,
  type VolumeBlockers,
} from '@/api/volumes'
import { storageActionNeedsReauth } from '@/api/storage'
import { formatTimestamp } from '@/utils/format'
import CapacityBar from './CapacityBar.vue'
import StorageActionDialog from './StorageActionDialog.vue'

const props = defineProps<{
  volume: Volume
  /** 只有 admin 能读占用详情，也只有 admin 能提交动作。 */
  canAct: boolean
}>()

const emit = defineEmits<{ changed: [] }>()

const blockers = ref<VolumeBlockers | null>(null)
const blockersError = ref<string | null>(null)
const blockersBusy = ref(false)

const stateLabel = computed(() => VOLUME_OBSERVED_STATE_LABELS[props.volume.observed_state])
const stateType = computed(() => VOLUME_OBSERVED_STATE_TYPES[props.volume.observed_state])
const hint = computed(() => observedStateHint(props.volume))
const mismatch = computed(() => desiredMismatch(props.volume))
const options = computed(() => volumeOptions(props.volume))

// ---- 配置表单 -----------------------------------------------------------
const editing = ref(false)
const editAutomount = ref(props.volume.automount)
const editReadOnly = ref(props.volume.read_only)
const editNodev = ref(props.volume.nodev)
const editNosuid = ref(props.volume.nosuid)
const diff = computed(() =>
  configureDiff(props.volume, {
    automount: editAutomount.value,
    readOnly: editReadOnly.value,
    nodev: editNodev.value,
    nosuid: editNosuid.value,
  }),
)
const diffCount = computed(() => Object.keys(diff.value).length)

function startEdit(): void {
  editAutomount.value = props.volume.automount
  editReadOnly.value = props.volume.read_only
  editNodev.value = props.volume.nodev
  editNosuid.value = props.volume.nosuid
  editing.value = true
}

// ---- 确认框 -------------------------------------------------------------
interface Pending {
  action: string
  title: string
  params: Record<string, unknown>
  lines: string[]
  irreversible: boolean
  needsReauth: boolean
}

const pending = ref<Pending | null>(null)
const dialogShow = ref(false)

function openMount(): void {
  pending.value = {
    action: VOLUME_ACTIONS.mount,
    title: `挂载 · ${props.volume.display_name}`,
    params: mountParams(props.volume),
    lines: [
      `按 UUID ${props.volume.filesystem_uuid} 把这块盘挂到 ${props.volume.mount_path}。`,
      '挂载成功后代理会用 findmnt 与拓扑扫描两路读回确认。',
    ],
    irreversible: false,
    needsReauth: volumeActionNeedsReauth(VOLUME_ACTIONS.mount),
  }
  dialogShow.value = true
}

function openUnmount(): void {
  pending.value = {
    action: VOLUME_ACTIONS.unmount,
    title: `卸载 · ${props.volume.display_name}`,
    params: unmountParams(props.volume),
    lines: [
      `把 ${props.volume.mount_path} 卸下来。登记记录与 fstab 条目都会保留，随时能再挂回去。`,
      '有人打开着这个挂载点上的文件时会被拒绝（EBUSY），面板不会强制卸载。',
    ],
    irreversible: false,
    needsReauth: volumeActionNeedsReauth(VOLUME_ACTIONS.unmount),
  }
  dialogShow.value = true
}

function openConfigure(): void {
  const changes = diff.value
  if (Object.keys(changes).length === 0) return
  const parts: string[] = []
  if (changes.automount !== undefined) parts.push(changes.automount ? '开机自动挂载' : '取消开机自动挂载')
  if (changes.readOnly !== undefined) parts.push(changes.readOnly ? '改成只读挂载' : '改成可读写挂载')
  if (changes.nodev !== undefined) parts.push(changes.nodev ? '禁止设备文件' : '允许设备文件')
  if (changes.nosuid !== undefined) parts.push(changes.nosuid ? '禁止 setuid' : '允许 setuid')
  pending.value = {
    action: VOLUME_ACTIONS.configure,
    title: `修改挂载选项 · ${props.volume.display_name}`,
    params: configureParams({
      volume: props.volume,
      ...changes,
    }),
    lines: [
      `改这个卷在 /etc/fstab 里的那一行：${parts.join('、')}。`,
      '只改面板自己的托管块，块外内容逐字保留；改动会留一份 fstab 备份。',
    ],
    irreversible: false,
    needsReauth: volumeActionNeedsReauth(VOLUME_ACTIONS.configure),
  }
  dialogShow.value = true
}

function openForget(): void {
  pending.value = {
    action: VOLUME_ACTIONS.forget,
    title: `退出管理 · ${props.volume.display_name}`,
    params: forgetParams(props.volume),
    lines: [
      `从面板的管理记录里移除 ${props.volume.display_name}，并删掉 fstab 托管块里它那一行。`,
      '盘、文件系统和里面的数据都不动；只是这台机器开机时不再按面板的配置挂它。',
      '退出管理要求它已经卸载、且没有任何依赖。',
    ],
    irreversible: false,
    needsReauth: volumeActionNeedsReauth(VOLUME_ACTIONS.forget),
  }
  dialogShow.value = true
}

async function loadBlockers(): Promise<void> {
  blockersBusy.value = true
  blockersError.value = null
  try {
    const result = await fetchVolumeBlockers(props.volume.id)
    blockers.value = result.data
  } catch (caught) {
    if (caught instanceof ApiError) blockersError.value = caught.message
    else if (caught instanceof RangeError) blockersError.value = caught.message
    else blockersError.value = '请求失败，请稍后重试'
  } finally {
    blockersBusy.value = false
  }
}

function onFinished(): void {
  pending.value = null
  // 现场变了：占用结论与卷状态都要重读，不能拿旧的“没人用”继续显示。
  blockers.value = null
  emit('changed')
}

const needsReauthFor = (action: string): boolean =>
  action.startsWith('storage.volume.') ? volumeActionNeedsReauth(action as never) : storageActionNeedsReauth(action)
</script>

<template>
  <NCard size="small" :data-volume-id="volume.id" data-testid="volume-card">
    <template #header>
      <span class="volume__title">
        {{ volume.display_name }}
        <NTag size="small" type="info">{{ volume.filesystem_type }}</NTag>
        <NTag size="small" :type="stateType">{{ stateLabel }}</NTag>
        <NTag v-if="volume.automount" size="small">开机自动挂载</NTag>
        <NTag v-if="volume.read_only" size="small" type="info">只读</NTag>
      </span>
    </template>
    <template #header-extra>
      <span class="volume__meta">v{{ volume.version }}</span>
    </template>

    <div class="volume__rows">
      <p class="volume__row">
        <span class="volume__label">挂载点</span>
        <code>{{ volume.mount_path }}</code>
      </p>
      <p class="volume__row">
        <span class="volume__label">设备 / UUID</span>
        <code>{{ volume.device_id }}</code>
        <span class="volume__meta">{{ volume.filesystem_uuid }}</span>
      </p>
      <p class="volume__row">
        <span class="volume__label">挂载选项</span>
        <code>{{ options.join(',') }}</code>
      </p>
      <p class="volume__row">
        <span class="volume__label">期望状态</span>
        <span>{{ VOLUME_DESIRED_STATE_LABELS[volume.desired_state] }}</span>
        <span class="volume__meta">最后变更 {{ formatTimestamp(volume.updated_at) }}</span>
      </p>
    </div>

    <NAlert v-if="mismatch" type="warning" :bordered="false">
      期望与实际不一致：登记的是「{{ VOLUME_DESIRED_STATE_LABELS[volume.desired_state] }}」，
      现场看到的是「{{ stateLabel }}」。
    </NAlert>
    <p v-if="hint" class="volume__hint">{{ hint }}</p>

    <CapacityBar
      v-if="volume.capacity !== null"
      :capacity="volume.capacity"
      :error="volume.observed_state === 'mounted' ? undefined : '现在没挂载，没有容量读数'"
    />
    <p v-else class="volume__meta">
      没有容量读数——容量只在真的挂着且读到时才有，不是 0 字节可用。
    </p>

    <p v-if="volume.dependencies.length > 0" class="volume__meta">
      依赖它的东西：{{ volume.dependencies.map((d) => `${d.kind} ${d.name}`).join('、') }}
    </p>

    <!-- 占用详情：按需查询，不预取（一次扫描要读 /proc 下所有 fd） -->
    <div v-if="canAct" class="volume__blockers">
      <NButton size="tiny" secondary :loading="blockersBusy" @click="loadBlockers">看占用</NButton>
      <template v-if="blockers">
        <NTag size="small" :type="blockers.busy ? 'warning' : 'success'">
          {{ blockers.busy ? '现在卸载会被挡住' : '没有占用' }}
        </NTag>
        <span class="volume__meta">{{ busySummary(blockers) }} · 检查于 {{ formatTimestamp(blockers.checked_at) }}</span>
        <ul v-if="blockers.processes.length > 0" class="volume__list">
          <li v-for="process in blockers.processes" :key="process.pid">
            <code>{{ process.pid }}</code> {{ process.comm }}
          </li>
        </ul>
        <span v-if="blockers.mounts.length > 0" class="volume__meta">
          当前挂载点：{{ blockers.mounts.join('、') }}
        </span>
      </template>
      <NAlert v-if="blockersError" type="error" :title="blockersError" />
    </div>

    <template v-if="canAct">
      <div class="volume__actions">
        <NButton
          v-if="volume.observed_state !== 'mounted'"
          size="small"
          :data-action="VOLUME_ACTIONS.mount"
          @click="openMount"
        >
          {{ VOLUME_ACTION_LABELS['storage.volume.mount'] }}
        </NButton>
        <NButton
          v-else
          size="small"
          :data-action="VOLUME_ACTIONS.unmount"
          @click="openUnmount"
        >
          {{ VOLUME_ACTION_LABELS['storage.volume.unmount'] }}
        </NButton>
        <NButton size="small" secondary :data-action="VOLUME_ACTIONS.configure" @click="startEdit">
          {{ VOLUME_ACTION_LABELS['storage.volume.configure'] }}
        </NButton>
        <NButton size="small" secondary type="error" :data-action="VOLUME_ACTIONS.forget" @click="openForget">
          {{ VOLUME_ACTION_LABELS['storage.volume.forget'] }}
        </NButton>
      </div>

      <div v-if="editing" class="volume__edit">
        <NCheckbox v-model:checked="editAutomount">开机自动挂载（去掉 noauto）</NCheckbox>
        <NCheckbox v-model:checked="editReadOnly">只读挂载（ro）</NCheckbox>
        <NCheckbox v-model:checked="editNodev">禁止设备文件（nodev）</NCheckbox>
        <NCheckbox v-model:checked="editNosuid">禁止 setuid（nosuid）</NCheckbox>
        <div class="volume__actions">
          <NButton
            size="small"
            type="primary"
            :disabled="diffCount === 0"
            data-testid="volume-configure-submit"
            @click="openConfigure"
          >
            提交修改（{{ diffCount }} 项）
          </NButton>
          <NButton size="small" secondary @click="editing = false">取消</NButton>
        </div>
        <p v-if="diffCount === 0" class="volume__meta">
          和现在的配置一样，没有要提交的改动——面板不会为了“点一下”去改 fstab。
        </p>
        <p v-else class="volume__meta">
          只把改动过的项发给服务端：没动的项按“不改”处理，不会被静默关掉。
        </p>
      </div>
    </template>
    <p v-else class="volume__meta">
      只读账号可以看卷列表与状态，但导入、挂载、卸载、改配置、退出管理都要求管理员。
    </p>

    <StorageActionDialog
      v-if="pending"
      v-model:show="dialogShow"
      :action="pending.action"
      :title="pending.title"
      :params="pending.params"
      :confirm-lines="pending.lines"
      :irreversible="pending.irreversible"
      :needs-reauth="needsReauthFor(pending.action)"
      @finished="onFinished"
    />
  </NCard>
</template>

<style scoped>
.volume__title {
  display: inline-flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
}

.volume__rows {
  display: flex;
  flex-direction: column;
  gap: 4px;
}

.volume__row {
  display: flex;
  align-items: baseline;
  gap: 8px;
  margin: 0;
  line-height: 1.7;
  flex-wrap: wrap;
}

.volume__label {
  min-width: 72px;
  font-size: 13px;
  opacity: 0.7;
}

.volume__meta {
  font-size: 13px;
  opacity: 0.8;
  word-break: break-all;
}

.volume__hint {
  margin: 8px 0 0;
  font-size: 13px;
  line-height: 1.7;
}

.volume__blockers {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 8px;
  margin-top: 8px;
}

.volume__actions {
  display: flex;
  flex-wrap: wrap;
  gap: 8px;
  margin-top: 10px;
}

.volume__edit {
  display: flex;
  flex-direction: column;
  gap: 6px;
  margin-top: 10px;
  padding: 10px 12px;
  border: 1px solid rgba(128, 128, 128, 0.2);
  border-radius: 8px;
}

.volume__list {
  margin: 0;
  padding-left: 18px;
  line-height: 1.7;
  font-size: 13px;
}
</style>
