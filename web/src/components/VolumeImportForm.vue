<script setup lang="ts">
// “纳入管理”表单（P07.5）。
//
// 从 VolumeSection 里单独拆出来的理由：一是它有自己的校验与预览，二是它可以在
// 没有设备数据的情况下被渲染出来验证（列表区需要异步取数，表单只需要 props）。
//
// 三条不能松的地方：
//   1. 候选只来自 importCandidates（已分区、没挂在别处、非系统盘；类型已知时才按它筛）；
//   2. 挂载名不合规就**拒绝**，不静默替换字符——挂载点是由它派生的；
//   3. 提交前把“挂载点固定为 /srv/upanel/volumes/<slug>”说出来，不让人以为能改路径。
//
// 候选里**不能**要求知道文件系统类型：未挂载分区的 `filesystem.type` 按契约恒为 null
// （产品不调 blkid），要求它就是让候选永远为空（真机上撞到过，DECISIONS Q208）。

import { computed, ref, watch } from 'vue'
import { NAlert, NButton, NInput, NSelect, type SelectOption } from 'naive-ui'

import {
  MAX_DISPLAY_NAME_LENGTH,
  MAX_SLUG_LENGTH,
  VOLUME_ACTIONS,
  VOLUME_ACTION_LABELS,
  candidateFilesystemLabel,
  importCandidates,
  importParams,
  isValidSlug,
  mountPathForSlug,
  normalizeSlug,
  type ImportCandidateDevice,
} from '@/api/volumes'
import type { JobStatus } from '@/api/jobs'
import { formatBytes } from '@/utils/format'
import StorageActionDialog from './StorageActionDialog.vue'

const props = defineProps<{
  devices: readonly ImportCandidateDevice[]
  /** 只读账号看不到表单，只看得到一句解释。 */
  canAct: boolean
}>()

const emit = defineEmits<{ finished: [JobStatus] }>()

interface CandidateOption extends SelectOption {
  label: string
  value: string
}

const candidates = computed(() => importCandidates(props.devices))
const candidateOptions = computed<CandidateOption[]>(() =>
  candidates.value.map((device) => ({
    label: `${device.id} · ${candidateFilesystemLabel(device)} · ${formatBytes(device.size_bytes)}`,
    value: device.id,
  })),
)

const selectedId = ref<string | null>(null)
const slug = ref('')
const displayName = ref('')
const dialogShow = ref(false)

// 候选变了（刚建好分区、刚挂上别的盘）就校正选择，避免提交一台不在列表里的设备。
watch(
  candidates,
  (next) => {
    const stillThere = next.some((device) => device.id === selectedId.value)
    if (!stillThere) {
      const first = next[0]
      selectedId.value = first === undefined ? null : first.id
    }
  },
  { immediate: true },
)

const slugValid = computed(() => isValidSlug(slug.value))
const slugPreview = computed(() => (slugValid.value ? mountPathForSlug(slug.value) : '（还不可用）'))
const nameTooLong = computed(() => displayName.value.trim().length > MAX_DISPLAY_NAME_LENGTH)
const ready = computed(() => selectedId.value !== null && slugValid.value && !nameTooLong.value)

const params = computed<Record<string, unknown> | null>(() =>
  selectedId.value === null
    ? null
    : importParams({ deviceId: selectedId.value, slug: slug.value, displayName: displayName.value }),
)

// 选中的分区类型未知时就明说“要等服务端直读”，不假装已经知道。
const hasUnknownType = computed(() => {
  const device = candidates.value.find((item) => item.id === selectedId.value)
  return device !== undefined && (device.filesystem?.type ?? null) === null
})

const title = computed(() => `纳入管理 · ${selectedId.value ?? ''}`)
const confirmLines = computed(() => [
  `把 ${selectedId.value ?? ''} 登记成一个受管卷，挂载点固定为 ${mountPathForSlug(slug.value)}。`,
  '只读取它的文件系统 UUID 与类型，不改分区表、不格式化、不立刻挂载。',
])

// 失焦时才规范化：边打字边改大小写会让人以为键盘坏了。
function applyNormalize(): void {
  const normalized = normalizeSlug(slug.value)
  if (normalized !== slug.value) slug.value = normalized
}

function open(): void {
  if (!ready.value) return
  dialogShow.value = true
}

function onFinished(status: JobStatus): void {
  dialogShow.value = false
  slug.value = ''
  displayName.value = ''
  emit('finished', status)
}
</script>

<template>
  <div class="import" data-testid="volume-import">
    <p class="import__line">
      <strong>纳入管理</strong>
      把一块已经有文件系统的空闲分区交给面板管理：登记、按 UUID 挂载、可选开机自动挂载。
    </p>

    <p v-if="!canAct" class="import__muted" data-testid="volume-import-readonly">
      只读账号看不到这个表单：导入、挂载、卸载、改配置、退出管理都要求管理员。
    </p>

    <template v-else>
      <div class="import__form">
        <NSelect
          v-model:value="selectedId"
          :options="candidateOptions"
          :placeholder="candidates.length === 0 ? '没有可纳入管理的分区' : '选择分区'"
          :disabled="candidates.length === 0"
          data-testid="import-device"
        />
        <NInput
          v-model:value="slug"
          placeholder="挂载名（小写字母、数字、中划线）"
          :maxlength="MAX_SLUG_LENGTH"
          data-testid="import-slug"
          @blur="applyNormalize"
        />
        <NInput
          v-model:value="displayName"
          placeholder="显示名（可留空，默认用挂载名）"
          :maxlength="MAX_DISPLAY_NAME_LENGTH"
          data-testid="import-name"
        />
        <NButton type="primary" :disabled="!ready" data-testid="import-submit" @click="open">
          {{ VOLUME_ACTION_LABELS['storage.volume.import'] }}
        </NButton>
      </div>

      <p class="import__muted">
        挂载点会固定为 <code>{{ slugPreview }}</code>
        ——由挂载名派生，页面不提供自己填路径的入口。
      </p>
      <p v-if="hasUnknownType" class="import__muted" data-testid="import-unknown-type">
        选中的分区没挂载，所以页面读不到它的文件系统类型（产品不调用 blkid、不靠猜）。
        类型与 UUID 由服务端在导入前直读那块分区确认；读不出来会是一条拒绝，不会硬写进
        <code>/etc/fstab</code>。
      </p>
      <p v-if="slug !== '' && !slugValid" class="import__muted import__muted--warn">
        这个挂载名不合规：只能用小写字母、数字与中划线，长度 1~{{ MAX_SLUG_LENGTH }}，
        不能以中划线开头或结尾。中文会被直接拒绝，不会被替换成别的东西。
      </p>
      <NAlert v-if="nameTooLong" type="warning" :bordered="false">
        显示名最长 {{ MAX_DISPLAY_NAME_LENGTH }} 个字符。
      </NAlert>
      <p v-if="candidates.length === 0" class="import__muted" data-testid="import-no-candidates">
        候选为空：只有“已分区、当前没挂载、不是系统盘”的分区才会出现在这里
        （文件系统类型已知但不是 ext4/XFS 的也不列）。整盘要先建分区，才谈得上纳入管理。
      </p>
    </template>

    <StorageActionDialog
      v-if="params !== null"
      v-model:show="dialogShow"
      :action="VOLUME_ACTIONS.import"
      :title="title"
      :params="params"
      :confirm-lines="confirmLines"
      :irreversible="false"
      :needs-reauth="false"
      @finished="onFinished"
    />
  </div>
</template>

<style scoped>
.import {
  display: flex;
  flex-direction: column;
  gap: 6px;
  margin-top: 16px;
  padding: 12px;
  border: 1px solid var(--rule);
  border-radius: var(--r-2);
}

.import__line {
  margin: 0 0 8px;
  line-height: 1.7;
}

.import__form {
  display: grid;
  grid-template-columns: minmax(220px, 1.2fr) minmax(180px, 1fr) minmax(180px, 1fr) auto;
  gap: 8px;
  align-items: center;
}

.import__muted {
  margin: 0;
  font-size: var(--fs-md);
  opacity: 0.8;
  line-height: 1.7;
  word-break: break-all;
}

.import__muted--warn {
  color: var(--bad);
  opacity: 1;
}

@media (max-width: 760px) {
  .import__form {
    grid-template-columns: 1fr;
  }
}
</style>
