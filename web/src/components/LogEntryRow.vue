<script setup lang="ts">
// 一条日志的展示单元（P05.3）。
//
// 一个组件只做一件事：把一条 LogEntry 如实画出来。三条显示规则来自
// PLAN.md 第 14 节，也正好是这一块最容易做错的地方：
//
//   1) binary=true 表示 journald 没给文本（消息是 JSON null，通常是二进制数据）。
//      这时**不能**显示一条空白行——那看起来像“空日志”，实际是“读不出来”。
//   2) truncated=true 表示消息超过单条上限被截断。截断过的不是完整消息，
//      必须显式标出来，否则用户会以为那就是全部内容。
//   3) redacted=true 表示消息里有秘密形态被滤掉过。过滤是尽力措施，
//      原样展示反而会让人以为拿到的是原文。
//
// 时间用等宽字体：日志是要对时间的，比例字体对不齐。

import { computed } from 'vue'
import { NTag } from 'naive-ui'

import { entryPlainText, logLevelLabel, logLevelType, type LogEntry } from '@/api/logs'

const props = defineProps<{
  entry: LogEntry
}>()

const levelType = computed(() => logLevelType(props.entry.level))
const levelText = computed(() => logLevelLabel(props.entry.level))

/** 时间只保留到秒：毫秒对排查没有帮助，反而让每行都长一截。 */
const timeText = computed(() => {
  const raw = props.entry.timestamp
  const date = new Date(raw)
  if (Number.isNaN(date.getTime())) return raw
  // 用本地时间显示：读日志的人就在这台机器的时区里看。
  const pad = (n: number): string => String(n).padStart(2, '0')
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())} ${pad(
    date.getHours(),
  )}:${pad(date.getMinutes())}:${pad(date.getSeconds())}`
})

/** 来源标识：优先 unit，其次 identifier，最后 comm（都没有就不显示）。 */
const sourceText = computed(() => {
  if (props.entry.unit) return props.entry.unit
  if (props.entry.identifier) return props.entry.identifier
  return props.entry.comm ?? ''
})

const pidText = computed(() =>
  typeof props.entry.pid === 'number' && props.entry.pid > 0 ? `pid ${props.entry.pid}` : '',
)

const plainText = computed(() => entryPlainText(props.entry))
</script>

<template>
  <div class="log-entry" :data-level="entry.level" :data-testid="'log-entry-' + entry.cursor">
    <div class="log-entry__meta">
      <span class="log-entry__time">{{ timeText }}</span>
      <NTag size="small" :type="levelType">{{ levelText }}</NTag>
      <span v-if="sourceText" class="log-entry__source">{{ sourceText }}</span>
      <span v-if="pidText" class="log-entry__pid">{{ pidText }}</span>
      <NTag v-if="entry.redacted" size="small" type="warning" title="这条消息里有秘密形态被过滤掉了">
        已过滤
      </NTag>
      <NTag v-if="entry.truncated" size="small" type="warning" title="消息超过单条上限，后半段已丢弃">
        已截断
      </NTag>
      <NTag v-if="entry.binary" size="small" title="journald 没有为这条条目提供文本（通常是二进制数据）">
        无文本
      </NTag>
    </div>
    <pre class="log-entry__message" :data-testid="'log-message-' + entry.cursor">{{
      entry.binary ? '（journald 未提供文本，可能是二进制数据）' : entry.message
    }}</pre>
    <!-- 供“复制整页”使用：复制出去的是带时间与级别的整行，不是裸消息。 -->
    <span class="log-entry__plain" hidden>{{ plainText }}</span>
  </div>
</template>

<style scoped>
.log-entry {
  display: flex;
  flex-direction: column;
  gap: 4px;
  padding: 8px 10px;
  border-left: 3px solid transparent;
  border-bottom: 1px solid var(--rule);
}

.log-entry[data-level='emergency'],
.log-entry[data-level='alert'],
.log-entry[data-level='critical'],
.log-entry[data-level='error'] {
  border-left-color: var(--bad);
}

.log-entry[data-level='warning'] {
  border-left-color: var(--warn);
}

.log-entry__meta {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 8px;
  font-size: var(--fs-sm);
  opacity: 0.9;
}

.log-entry__time {
  font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
}

.log-entry__source,
.log-entry__pid {
  font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
  opacity: 0.8;
}

.log-entry__message {
  margin: 0;
  font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
  font-size: var(--fs-md);
  line-height: 1.6;
  white-space: pre-wrap;
  word-break: break-word;
}
</style>
