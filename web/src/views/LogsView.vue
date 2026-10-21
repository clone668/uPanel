<script setup lang="ts">
// 日志页（P05.3）：结构化查询 + 实时流。
//
// 与“服务与进程”这类快照页不同，日志是**窗口**：每次查询都带一段时间范围。
// 因此页面必须把“我现在看的是哪一段时间”写出来，翻页时窗口固定不漂移，
// 保存下来的游标只在同一个窗口里有效。
//
// 三条不可让步的规则：
//   1) 原始日志只有 admin 能读。viewer 看到的是“没有权限”，不是“脱敏后的日志”
//      ——过滤秘密是尽力措施，不能作为开放日志的依据（PLAN.md 第 14 节）。
//   2) 限额来自服务端：单页最多 1000 行 / 1 MiB、时间范围最多 24 小时。
//      页面提前拦明显越界的输入只是为了少一次往返，绝不放大上限。
//   3) 实时流断开就收干净：组件卸载或用户关掉时必须 close()，
//      代理那边才会杀掉 journalctl（P05.3 的验收之一）。
//
// 这里不做轮询：日志不是快照，定时重拉会让“翻了 3 页”的位置随时跳掉。

import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { NAlert, NButton, NCard, NInput, NSelect, NSwitch, NTag } from 'naive-ui'

import { API_BASE, ApiError } from '@/api/client'
import {
  DEFAULT_LOG_LIMIT,
  MAX_LOG_LIMIT,
  MAX_LOG_SPAN_HOURS,
  type LogEntry,
  type LogPage,
  type LogQuery,
  type LogStreamHandle,
  type LogStreamState,
  defaultWindow,
  fetchLogs,
  isValidUnit,
  logLevelLabel,
  logSourceLabel,
  logStreamEndLabel,
  openLogStream,
  pageEntries,
  summarizeLogPage,
  toLocalInput,
  toRFC3339,
} from '@/api/logs'
import ApiErrorPanel from '@/components/ApiErrorPanel.vue'
import LogEntryRow from '@/components/LogEntryRow.vue'
import StatePanel from '@/components/StatePanel.vue'
import { useSessionStore } from '@/stores/session'
import { formatTimestamp } from '@/utils/format'
import { apiPanelState } from '@/utils/viewstate'

type Phase = 'idle' | 'loading' | 'ready' | 'error'

const session = useSessionStore()

// ---------------------------------------------------------------- 表单

const initial = defaultWindow()
const source = ref<string>('journal')
const unit = ref('')
const level = ref<string>('')
const fromLocal = ref(toLocalInput(initial.from))
const toLocal = ref(toLocalInput(initial.to))
const limit = ref<number>(DEFAULT_LOG_LIMIT)
const formError = ref('')

const sourceOptions = [
  { label: logSourceLabel('journal'), value: 'journal' },
  { label: logSourceLabel('kernel'), value: 'kernel' },
]

const levelOptions = [
  { label: '全部级别', value: '' },
  ...['emergency', 'alert', 'critical', 'error', 'warning', 'notice', 'info', 'debug'].map(
    (value) => ({ label: logLevelLabel(value), value }),
  ),
]

const limitOptions = [100, 200, 500, MAX_LOG_LIMIT].map((value) => ({
  label: `${value} 行`,
  value,
}))

// ---------------------------------------------------------------- 查询状态

const phase = ref<Phase>('idle')
const error = ref<ApiError | null>(null)
const page = ref<LogPage | null>(null)
const entries = ref<LogEntry[]>([])
const nextCursor = ref<string | null>(null)
const loadingMore = ref(false)
/** 本次结果的窗口与生效查询：翻页时原样沿用，不重新按“现在”算。 */
const appliedWindow = ref<{ from: string; to: string } | null>(null)
const appliedQuery = ref<LogQuery>({})

const panelState = computed(() => (error.value ? apiPanelState(error.value) : null))
const summary = computed(() => summarizeLogPage(page.value))
const windowText = computed(() => {
  const w = appliedWindow.value
  if (!w) return ''
  return `${formatTimestamp(w.from)} — ${formatTimestamp(w.to)}`
})

/** 客户端提前拦：这些输入一定过不了服务端，先说出来少一次往返。 */
function validateForm(): LogQuery | null {
  formError.value = ''
  const from = toRFC3339(fromLocal.value)
  const to = toRFC3339(toLocal.value)
  if (from !== '' && to !== '') {
    const spanHours = (new Date(to).getTime() - new Date(from).getTime()) / 3_600_000
    if (Number.isNaN(spanHours)) {
      formError.value = '时间格式无法识别，请重新选择'
      return null
    }
    if (spanHours <= 0) {
      formError.value = '开始时间必须早于结束时间'
      return null
    }
    if (spanHours > MAX_LOG_SPAN_HOURS) {
      formError.value = `一次最多查询 ${MAX_LOG_SPAN_HOURS} 小时，当前选择了 ${Math.round(
        spanHours,
      )} 小时`
      return null
    }
  }
  const unitText = unit.value.trim()
  if (unitText !== '' && source.value === 'kernel') {
    formError.value = '内核日志不接受单元过滤'
    return null
  }
  // 服务端也校验一次；两边规则一致，界面放行的不会被服务端拒。
  if (unitText !== '' && !isValidUnit(unitText)) {
    formError.value = '单元名需要形如 ssh.service，不接受通配符与路径'
    return null
  }
  const query: LogQuery = { source: source.value, limit: limit.value }
  if (unitText !== '') query.unit = unitText
  if (level.value !== '') query.level = level.value
  if (from !== '') query.from = from
  if (to !== '') query.to = to
  return query
}

async function runQuery(query: LogQuery, options: { append?: boolean; cursor?: string } = {}): Promise<void> {
  const full: LogQuery = { ...query }
  if (options.cursor !== undefined) full.cursor = options.cursor
  try {
    if (options.append === true) loadingMore.value = true
    else phase.value = 'loading'
    const result = await fetchLogs(full)
    const older = pageEntries(result.data)
    entries.value = options.append === true ? [...entries.value, ...older] : older
    page.value = result.data
    // next_cursor 只在“还有更早的”时才有值；它属于本次窗口。
    nextCursor.value = result.data.next_cursor ?? null
    appliedWindow.value = { from: result.data.from, to: result.data.to }
    appliedQuery.value = query
    error.value = null
    phase.value = 'ready'
  } catch (caught) {
    if (caught instanceof ApiError) {
      error.value = caught
      // 翻页失败时不要把已经看到的内容清掉：那是用户唯一还留着的东西。
      if (options.append !== true) {
        entries.value = []
        page.value = null
        nextCursor.value = null
      }
      phase.value = 'error'
      return
    }
    throw caught
  } finally {
    loadingMore.value = false
  }
}

function submit(): void {
  const query = validateForm()
  if (query === null) return
  stopLive()
  void runQuery(query)
}

function loadOlder(): void {
  const cursor = nextCursor.value
  if (cursor === null || loadingMore.value) return
  void runQuery(appliedQuery.value, { append: true, cursor })
}

// ---------------------------------------------------------------- 实时流

const live = ref(false)
const streamState = ref<LogStreamState>('closed')
const streamEnd = ref('')
const streamError = ref<{ code: string; message: string } | null>(null)
const invalidFrames = ref(0)
let handle: LogStreamHandle | null = null

const liveStateText = computed(() => {
  switch (streamState.value) {
    case 'connecting':
      return '正在连接'
    case 'open':
      return '实时接收中'
    case 'reconnecting':
      return '重新连接中'
    default:
      return '未连接'
  }
})

function addLiveEntry(entry: LogEntry): void {
  // 实时流是“从现在往后”，按到达顺序追加在顶部区域的末尾；
  // 同一游标重复到达（重连后的回放）只保留一份。
  if (entries.value.some((existing) => existing.cursor === entry.cursor)) return
  entries.value = [entry, ...entries.value].slice(0, MAX_LOG_LIMIT)
}

function stopLive(): void {
  handle?.close()
  handle = null
  live.value = false
  streamState.value = 'closed'
}

function startLive(): void {
  const query = validateForm()
  if (query === null) return
  formError.value = ''
  stopLive()
  streamEnd.value = ''
  streamError.value = null
  invalidFrames.value = 0
  appliedQuery.value = query
  live.value = true
  handle = openLogStream(API_BASE, query, {
    onState: (state) => {
      streamState.value = state
    },
    onEntry: (entry) => {
      addLiveEntry(entry)
    },
    onEnd: (reason) => {
      // 服务端给的是“为什么结束”。到时长/条数上限不是错误，应当如实说清并让用户自己决定要不要重开。
      streamEnd.value = reason
      live.value = false
      streamState.value = 'closed'
    },
    onError: (code, message) => {
      streamError.value = { code, message }
      live.value = false
      streamState.value = 'closed'
    },
    onInvalidFrame: () => {
      invalidFrames.value += 1
    },
  })
}

function toggleLive(value: boolean): void {
  if (value) startLive()
  else stopLive()
}

onMounted(() => {
  // 进页面先拉一次“最近一小时”：空白页面比一次真实的空结果更难理解。
  if (session.isAdmin) submit()
})

onBeforeUnmount(() => {
  // 必须关：不关的话代理那边会一直挂着一个 journalctl 跟随进程。
  stopLive()
})

// ---------------------------------------------------------------- 复制

const copyMessage = ref('')

async function copyAll(): Promise<void> {
  if (entries.value.length === 0) return
  const text = entries.value
    .map((entry) => {
      const parts = [entry.timestamp]
      if (entry.unit) parts.push(entry.unit)
      if (entry.identifier) parts.push(entry.identifier)
      parts.push(`[${entry.level}]`)
      parts.push(entry.binary ? '（journald 未提供文本）' : entry.message)
      return parts.join(' ')
    })
    .join('\n')
  try {
    await navigator.clipboard.writeText(text)
    copyMessage.value = `已复制 ${entries.value.length} 条`
  } catch {
    // 剪贴板被浏览器拒绝时如实说，不假装复制成功。
    copyMessage.value = '浏览器拒绝了剪贴板访问，请手动选择文本'
  }
}
</script>

<template>
  <section class="logs">
    <div class="logs__head">
      <h1 class="logs__title">日志</h1>
      <NTag size="small" type="info">{{ logSourceLabel(source) }}</NTag>
      <NTag v-if="live" size="small" type="success">{{ liveStateText }}</NTag>
      <NTag v-else-if="streamState === 'reconnecting'" size="small" type="warning">
        {{ liveStateText }}
      </NTag>
    </div>
    <p class="logs__meta">
      结构化查询 journald：按数据源、单元、级别与时间窗口过滤。原始日志仅管理员可读。
    </p>

    <StatePanel
      v-if="!session.isAdmin"
      state="forbidden"
      title="没有权限查看日志"
      description="原始日志里可能有口令形态的字符串、内网地址与别人的命令行。过滤是尽力措施，不能作为向只读账号开放日志的依据，因此日志只对管理员开放。"
    />

    <template v-else>
      <NCard size="small" title="查询条件">
        <div class="logs__form">
          <label class="logs__field">
            <span class="logs__label">数据源</span>
            <NSelect v-model:value="source" :options="sourceOptions" :disabled="live" />
          </label>
          <label class="logs__field">
            <span class="logs__label">单元（可留空）</span>
            <NInput
              v-model:value="unit"
              clearable
              placeholder="例如 ssh.service"
              :disabled="live || source === 'kernel'"
            />
          </label>
          <label class="logs__field">
            <span class="logs__label">级别下限</span>
            <NSelect v-model:value="level" :options="levelOptions" :disabled="live" />
          </label>
          <label class="logs__field">
            <span class="logs__label">从</span>
            <input
              v-model="fromLocal"
              class="logs__time"
              type="datetime-local"
              :disabled="live"
            />
          </label>
          <label class="logs__field">
            <span class="logs__label">到</span>
            <input v-model="toLocal" class="logs__time" type="datetime-local" :disabled="live" />
          </label>
          <label class="logs__field">
            <span class="logs__label">每页条数</span>
            <NSelect v-model:value="limit" :options="limitOptions" :disabled="live" />
          </label>
        </div>
        <div class="logs__actions">
          <NButton type="primary" :disabled="phase === 'loading' || live" @click="submit">
            查询
          </NButton>
          <NButton :disabled="phase === 'loading' || live" @click="loadOlder">
            {{ loadingMore ? '加载中…' : '加载更早' }}
          </NButton>
          <span class="logs__live">
            <NSwitch :value="live" @update:value="toggleLive" />
            <span>实时订阅</span>
          </span>
          <NButton quaternary :disabled="entries.length === 0" @click="copyAll">复制本页</NButton>
          <span v-if="copyMessage" class="logs__meta">{{ copyMessage }}</span>
        </div>
        <NAlert v-if="formError" type="error" :bordered="false">{{ formError }}</NAlert>
        <p class="logs__meta">
          单次最多 {{ MAX_LOG_LIMIT }} 行、单条消息超过 8 KiB 会被截断，时间范围最多
          {{ MAX_LOG_SPAN_HOURS }} 小时；这些上限由服务端强制。
        </p>
      </NCard>

      <StatePanel v-if="phase === 'loading'" state="loading" title="正在读取日志" />
      <StatePanel
        v-else-if="panelState && error && panelState !== 'error'"
        :state="panelState"
        :description="error.message"
        :code="error.code"
        :request-id="error.requestId"
        @retry="submit"
      />
      <ApiErrorPanel v-else-if="error" :error="error" @retry="submit" />

      <NAlert v-if="streamEnd" type="info" :bordered="false">
        实时订阅已结束：{{ logStreamEndLabel(streamEnd) }}（{{ streamEnd }}）。
        已经收到的条目仍显示在下面。
      </NAlert>
      <NAlert v-if="streamError" type="error" :bordered="false">
        实时订阅失败：{{ streamError.message || streamError.code }}
      </NAlert>
      <NAlert v-if="invalidFrames > 0" type="warning" :bordered="false">
        丢弃了 {{ invalidFrames }} 帧无法解析的流数据（服务端协议与页面不一致时应排查版本）。
      </NAlert>

      <NCard v-if="page" size="small">
        <p class="logs__line">
          窗口 {{ windowText }} · 本页 {{ summary.shown }} 条（扫描 {{ summary.scanned }} 条，用时
          {{ page.duration_ms }} ms）
          <NTag v-if="summary.redacted > 0" size="small" type="warning">
            {{ summary.redacted }} 条含被过滤的秘密
          </NTag>
          <NTag v-if="summary.truncatedEntries > 0" size="small" type="warning">
            {{ summary.truncatedEntries }} 条消息被截断
          </NTag>
          <NTag v-if="summary.binary > 0" size="small">{{ summary.binary }} 条无文本</NTag>
        </p>
        <NAlert v-if="summary.truncated" type="warning" :bordered="false">
          这一页被服务端裁剪过（{{ summary.truncatedReason || '达到条数或字节上限' }}）。
          未显示的部分不代表不存在，请缩小时间范围或提高级别。
        </NAlert>
        <p v-if="nextCursor" class="logs__meta">还有更早的日志，用“加载更早”继续。</p>
      </NCard>

      <StatePanel
        v-if="phase === 'ready' && entries.length === 0 && !live"
        state="empty"
        title="这段时间没有日志"
        description="换一个时间窗口、放宽级别，或换成内核日志再看看。"
      />

      <NCard v-if="entries.length > 0" size="small" title="日志条目">
        <div class="logs__list" data-testid="log-list">
          <LogEntryRow v-for="entry in entries" :key="entry.cursor" :entry="entry" />
        </div>
      </NCard>
    </template>
  </section>
</template>

<style scoped>
.logs {
  display: flex;
  flex-direction: column;
  gap: 16px;
  max-width: 1200px;
}

.logs__head {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 10px;
}

.logs__title {
  margin: 0;
  font-size: 20px;
}

.logs__meta {
  font-size: 13px;
  opacity: 0.8;
  margin: 0;
  line-height: 1.7;
}

.logs__line {
  margin: 0;
  line-height: 1.9;
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 8px;
}

.logs__form {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(200px, 1fr));
  gap: 12px;
}

.logs__field {
  display: flex;
  flex-direction: column;
  gap: 4px;
}

.logs__label {
  font-size: 12px;
  opacity: 0.8;
}

/* 时间输入用原生控件：naive-ui 的 NInput 不支持 datetime-local，
   而这里要的就是“浏览器自带的时间选择器”。外观跟着全局主题走。 */
.logs__time {
  height: 34px;
  padding: 0 10px;
  border-radius: 3px;
  border: 1px solid rgba(128, 128, 128, 0.4);
  background: transparent;
  color: inherit;
  font: inherit;
  font-size: 14px;
}

.logs__actions {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 12px;
  margin: 12px 0;
}

.logs__live {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  font-size: 13px;
}

.logs__list {
  display: flex;
  flex-direction: column;
  max-height: 70vh;
  overflow-y: auto;
}
</style>
