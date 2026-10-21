<script setup lang="ts">
// 文件列表里的一行（P06.1）。
//
// 抽成组件是为了两件事：
//   1) 一行里的判断（能不能进、能不能读、为什么不能）只有一处，不会表格里一套、
//      预览面板里另一套；
//   2) 它可以被单独渲染成 HTML 做断言——冻结依赖里没有 DOM 环境
//      （见 docs/DECISIONS.md Q21），整页交互测不了，但"这行显示成什么文字"能测。
//
// 规则来源：
//   - 只有目录能进，只有普通文件能预览/下载；符号链接与特殊文件显示理由，不显示按钮。
//   - 大小只在普通文件上有意义；目录与链接显示 U+2014，不用 0 B 冒充。
//   - 超过文本上限的文件不给"预览"，但**能下载**——上限是文本预览的上限，不是文件的上限。
//   - 按钮只是入口；越界与否由服务端在打开句柄之后判定，界面不替它下结论。

import { computed } from 'vue'
import { NButton, NTag } from 'naive-ui'

import {
  MAX_TEXT_BYTES,
  canEnter,
  canRead,
  entryDisabledReason,
  fileEntryTypeLabel,
  safeBytes,
  type FileEntry,
} from '@/api/files'
import { bytesTitle, formatBytes, formatTimestamp, UNKNOWN_TEXT } from '@/utils/format'

const props = defineProps<{
  entry: FileEntry
  /** 这一行是否正在下载（只影响按钮的 loading 态）。 */
  downloading?: boolean
}>()

const emit = defineEmits<{ enter: [FileEntry]; open: [FileEntry]; save: [FileEntry] }>()

const enterable = computed(() => canEnter(props.entry))
const readable = computed(() => canRead(props.entry))
const disabledReason = computed(() => entryDisabledReason(props.entry))

const sizeText = computed(() =>
  props.entry.type === 'file' ? formatBytes(props.entry.size_bytes) : UNKNOWN_TEXT,
)
const sizeTitle = computed(() =>
  props.entry.type === 'file' ? bytesTitle(props.entry.size_bytes) : undefined,
)
const ownerText = computed(() => `${props.entry.owner.uid}:${props.entry.owner.gid}`)

/** 超出文本预览上限：只提示并允许下载，不在界面上假装能预览。 */
const tooLargeForText = computed(() => {
  const size = safeBytes(props.entry.size_bytes)
  return size !== null && size > MAX_TEXT_BYTES
})
</script>

<template>
  <tr>
    <td class="row__name">
      <NButton v-if="enterable" text type="primary" @click="emit('enter', entry)">
        {{ entry.name }}
      </NButton>
      <NButton v-else-if="readable" text type="primary" @click="emit('open', entry)">
        {{ entry.name }}
      </NButton>
      <span v-else class="row__dim">{{ entry.name }}</span>
    </td>
    <td>
      <NTag size="small" :type="entry.type === 'symlink' ? 'warning' : 'default'">
        {{ fileEntryTypeLabel(entry.type) }}
      </NTag>
    </td>
    <td :title="sizeTitle">{{ sizeText }}</td>
    <td><code>{{ entry.mode }}</code></td>
    <td><code>{{ ownerText }}</code></td>
    <td>{{ formatTimestamp(entry.mtime) }}</td>
    <td class="row__ops">
      <template v-if="readable">
        <NButton size="tiny" :disabled="tooLargeForText" @click="emit('open', entry)">
          预览
        </NButton>
        <NButton size="tiny" quaternary :loading="downloading === true" @click="emit('save', entry)">
          下载
        </NButton>
        <span v-if="tooLargeForText" class="row__dim">
          超过 {{ formatBytes(String(MAX_TEXT_BYTES)) }}，只能下载
        </span>
      </template>
      <span v-else class="row__dim">{{ disabledReason }}</span>
    </td>
  </tr>
</template>

<style scoped>
.row__name {
  max-width: 320px;
  word-break: break-all;
}

.row__ops {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 8px;
}

.row__dim {
  opacity: 0.7;
  font-size: 12px;
}
</style>
