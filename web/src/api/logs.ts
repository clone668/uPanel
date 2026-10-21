// 日志接口 DTO（GET /logs 与 GET /logs/stream）。
//
// 字段与 api/openapi.yaml 的 LogEntry / LogPage 逐字对应；语义来源是
// PLAN.md 第 14 节与 CONTRACTS.md 第 10 节。
//
// 四条纪律（每条都对应一条验收要求）：
//   1) 原始日志**只有 admin 能读**：过滤秘密是尽力措施，不能作为向 viewer 开放的理由。
//      页面在无权限时不显示“脱敏版日志”，而是明确说没有权限。
//   2) 限额来自服务端，界面不放大也不假装更大：单页最多 1000 行 / 1 MiB、
//      时间范围最多 24 小时。这里的常量只用于**提前拦**明显越界的输入，
//      真正的判决永远在服务端。
//   3) 时间窗口必须显示出来。翻页时窗口会随游标一起固定，页面写着“这段时间”，
//      就不能在翻页后悄悄换成另一段。
//   4) 截断与脱敏要显式标出：一条被截断的消息不是完整消息，一条被过滤过的消息
//      不是原文；把两者都当正常日志显示，等于让用户以为自己看全了。

import { get, type ApiResult, type QueryValue } from './client'

// ---------------------------------------------------------------- 数据源与级别

export const LOG_SOURCES = ['journal', 'kernel'] as const

export type LogSource = (typeof LOG_SOURCES)[number]

export const LOG_SOURCE_LABELS: Record<LogSource, string> = {
  journal: '系统日志（journald）',
  kernel: '内核日志',
}

export function logSourceLabel(source: string | undefined | null): string {
  if (source === undefined || source === null || source === '') return LOG_SOURCE_LABELS.journal
  return (LOG_SOURCE_LABELS as Record<string, string | undefined>)[source] ?? source
}

/** 级别闭集，顺序与 syslog 一致（0 最严重）。 */
export const LOG_LEVELS = [
  'emergency',
  'alert',
  'critical',
  'error',
  'warning',
  'notice',
  'info',
  'debug',
] as const

export type LogLevel = (typeof LOG_LEVELS)[number]

export const LOG_LEVEL_LABELS: Record<string, string> = {
  emergency: '紧急',
  alert: '告警',
  critical: '严重',
  error: '错误',
  warning: '警告',
  notice: '通知',
  info: '信息',
  debug: '调试',
  // 服务端读不出 PRIORITY 时会回这个值。它不是 info 的别名。
  unknown: '未知级别',
}

export const LOG_LEVEL_TYPES: Record<
  LogLevel | 'unknown',
  'default' | 'info' | 'success' | 'warning' | 'error'
> = {
  emergency: 'error',
  alert: 'error',
  critical: 'error',
  error: 'error',
  warning: 'warning',
  notice: 'info',
  info: 'default',
  debug: 'default',
  unknown: 'default',
}

/** 未知取值不假装正常：原样显示。 */
export function logLevelLabel(level: string | undefined | null): string {
  if (level === undefined || level === null || level === '') {
    return LOG_LEVEL_LABELS['unknown'] ?? '未知级别'
  }
  return (LOG_LEVEL_LABELS as Record<string, string | undefined>)[level] ?? level
}

export function logLevelType(
  level: string | undefined | null,
): 'default' | 'info' | 'success' | 'warning' | 'error' {
  const key = level ?? 'unknown'
  return (
    (LOG_LEVEL_TYPES as Record<string, 'default' | 'info' | 'success' | 'warning' | 'error' | undefined>)[
      key
    ] ?? 'default'
  )
}

// ---------------------------------------------------------------- 限额（与服务端一致）

export const DEFAULT_LOG_LIMIT = 100
export const MAX_LOG_LIMIT = 1000
/** 单页消息字节上限（服务端也会按它截断，界面只用于说明）。 */
export const MAX_LOG_PAGE_BYTES = 1 << 20
/** 单条消息保留的字节数；超出部分被丢弃并标记 truncated。 */
export const MAX_LOG_MESSAGE_BYTES = 8 << 10
export const MAX_LOG_SPAN_HOURS = 24
export const DEFAULT_LOG_WINDOW_HOURS = 1

// ---------------------------------------------------------------- DTO

export interface LogEntry {
  /** journal 游标：条目身份，也是流续传的位置。 */
  cursor: string
  timestamp: string
  /** emergency…debug，或 unknown（读不出 PRIORITY）。 */
  level: string
  message: string
  /** 消息被截断（超过单条上限）。 */
  truncated: boolean
  /** 消息里有秘密形态被抹掉过。 */
  redacted: boolean
  /** journald 没给文本（二进制或 null）：message 为空不代表“空日志”。 */
  binary: boolean
  unit?: string
  identifier?: string
  comm?: string
  pid?: number | null
}

export interface LogPage {
  source: string
  unit?: string
  level?: string
  from: string
  to: string
  /** nil 页会被序列化成 null，不是 []。 */
  entries: LogEntry[] | null
  next_cursor: string | null
  truncated: boolean
  truncated_reason?: string
  scanned: number
  duration_ms: number
}

/** 查询参数。空串与 undefined 都不进 URL。 */
export interface LogQuery {
  source?: string
  unit?: string
  level?: string
  from?: string
  to?: string
  cursor?: string
  limit?: number
}

/**
 * 把查询对象变成 URL 参数。
 *
 * 只放契约里列出的键：多传一个未知键不会改变服务端行为，但会让“我明明筛了”
 * 与“结果没变”之间的矛盾更难查。
 */
export function logQueryParams(query: LogQuery): Record<string, QueryValue> {
  const params: Record<string, QueryValue> = {}
  if (query.source !== undefined && query.source !== '') params.source = query.source
  if (query.unit !== undefined && query.unit !== '') params.unit = query.unit.trim()
  if (query.level !== undefined && query.level !== '') params.level = query.level
  if (query.from !== undefined && query.from !== '') params.from = query.from
  if (query.to !== undefined && query.to !== '') params.to = query.to
  if (query.cursor !== undefined && query.cursor !== '') params.cursor = query.cursor
  if (query.limit !== undefined && query.limit > 0) params.limit = query.limit
  return params
}

export function fetchLogs(
  query: LogQuery,
  signal?: AbortSignal,
): Promise<ApiResult<LogPage>> {
  return get<LogPage>('/logs', {
    query: logQueryParams(query),
    // 单页最多 1000 行、1 MiB：代理侧有 15 秒执行上限，这里给到 20 秒留余量。
    timeoutMs: 20_000,
    ...(signal === undefined ? {} : { signal }),
  })
}

/** entries 为 null 时归一化成空数组：页面不该到处写 `?? []`。 */
export function pageEntries(page: LogPage | null | undefined): LogEntry[] {
  if (!page || !Array.isArray(page.entries)) return []
  return page.entries
}

// ---------------------------------------------------------------- 输入校验（早拦）

/** 单元名形状必须与服务端的判断一致，否则会出现“界面放行、服务端 400”。 */
const UNIT_PATTERN =
  /^[A-Za-z0-9][A-Za-z0-9:_.@-]{0,200}\.(service|socket|timer|mount|automount|swap|target|path|slice|scope|device)$/

export function isValidUnit(unit: string): boolean {
  return UNIT_PATTERN.test(unit)
}

/** 级别必须是闭集里的值；空串表示不过滤。 */
export function isValidLevel(level: string): boolean {
  return level === '' || (LOG_LEVELS as readonly string[]).includes(level)
}

// ---------------------------------------------------------------- 时间输入

function pad(value: number): string {
  return String(value).padStart(2, '0')
}

/** Date → `datetime-local` 需要的本地时间文本（无时区后缀）。 */
export function toLocalInputDate(date: Date): string {
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}T${pad(
    date.getHours(),
  )}:${pad(date.getMinutes())}`
}

/**
 * 把 `<input type="datetime-local">` 的值转成 RFC3339。
 *
 * 浏览器的值是本地时间、没有时区（`2026-10-21T03:00`），直接发出去会被服务端
 * 当成格式错误。这里按本地时区解释它，再转成带 Z 的 UTC 时间，于是
 * “用户选的是本地时间”这件事成立。
 */
export function toRFC3339(local: string): string {
  if (local === '') return ''
  const date = new Date(local)
  if (Number.isNaN(date.getTime())) return ''
  return date.toISOString().replace(/\.\d{3}Z$/, 'Z')
}

/** 反向：RFC3339 → datetime-local 的本地时间文本。 */
export function toLocalInput(rfc3339: string | undefined | null): string {
  if (!rfc3339) return ''
  const date = new Date(rfc3339)
  if (Number.isNaN(date.getTime())) return ''
  return toLocalInputDate(date)
}

/** 默认窗口：最近一小时，秒与毫秒对齐到 0（避免每次查询都换一个窗口）。 */
export function defaultWindow(now: Date = new Date()): { from: string; to: string } {
  const toDate = new Date(now.getTime())
  toDate.setSeconds(0, 0)
  const fromDate = new Date(toDate.getTime() - DEFAULT_LOG_WINDOW_HOURS * 3600 * 1000)
  return {
    from: toRFC3339(toLocalInputDate(fromDate)),
    to: toRFC3339(toLocalInputDate(toDate)),
  }
}

// ---------------------------------------------------------------- 实时流

/**
 * SSE 帧（服务端 sseWriter 写出的三种命名事件）。
 *
 * `error` 这个名字与 EventSource 自带的连接错误事件重名：
 * 服务端发来的 error 帧有 data，连接错误没有。解析时靠这一点区分。
 */
export type LogStreamFrame =
  | { kind: 'entry'; entry: LogEntry }
  | { kind: 'end'; reason: string }
  | { kind: 'error'; code: string; message: string }

export type LogStreamEventName = 'entry' | 'end' | 'error'

export type LogStreamState = 'connecting' | 'open' | 'reconnecting' | 'closed'

export interface LogStreamHandlers {
  onEntry?: (entry: LogEntry) => void
  /** 服务端给出了结束原因：重连与否由页面决定。 */
  onEnd?: (reason: string) => void
  /** 服务端报告流失败。 */
  onError?: (code: string, message: string) => void
  onState?: (state: LogStreamState) => void
  /** 无法解析的帧（计数用，不打断流）。 */
  onInvalidFrame?: (raw: string) => void
}

/** 只依赖 EventSource 的这几个成员，测试里可以塞一个假实现。 */
export interface EventSourceLike {
  readonly readyState: number
  addEventListener(type: string, listener: (event: MessageEvent) => void): void
  close(): void
}

export type EventSourceFactory = (url: string) => EventSourceLike

export function logStreamQueryParams(query: LogQuery): Record<string, QueryValue> {
  // 游标不进流的 URL：断点续传靠 EventSource 的 Last-Event-ID，
  // 放在 URL 里会让“重连”与“刚打开”走两条不同的语义。
  const params: Record<string, QueryValue> = {}
  if (query.source !== undefined && query.source !== '') params.source = query.source
  if (query.unit !== undefined && query.unit !== '') params.unit = query.unit.trim()
  if (query.level !== undefined && query.level !== '') params.level = query.level
  if (query.from !== undefined && query.from !== '') params.from = query.from
  if (query.to !== undefined && query.to !== '') params.to = query.to
  if (query.limit !== undefined && query.limit > 0) params.limit = query.limit
  return params
}

/** 由页面用 API_BASE 拼出 SSE 地址；这里单独导出便于测试。 */
export function logStreamUrl(base: string, query: LogQuery): string {
  const params = new URLSearchParams()
  for (const [key, value] of Object.entries(logStreamQueryParams(query))) {
    if (value === undefined || value === null) continue
    params.append(key, String(value))
  }
  const qs = params.toString()
  return `${base}/logs/stream${qs === '' ? '' : `?${qs}`}`
}

/**
 * 解析一帧的 data；不合法返回 null。
 *
 * kind 来自 SSE 的 event 名（entry / end / error），**不**从 data 里读：
 * 服务端把 Entry 本身当 data 发出来，帧类型由 event 名承担，data 里再放一个
 * `type` 只是把同一件事说两遍，而两处说法迟早会不一致。因此 event 名是什么，
 * 就按什么形状校验 data —— 名字与内容对不上时算坏帧，而不是猜一个。
 */
export function parseLogStreamFrame(
  kind: LogStreamEventName,
  data: string | null | undefined,
): LogStreamFrame | null {
  if (!data) return null
  let parsed: unknown
  try {
    parsed = JSON.parse(data)
  } catch {
    return null
  }
  if (parsed === null || typeof parsed !== 'object' || Array.isArray(parsed)) return null
  const frame = parsed as Record<string, unknown>
  switch (kind) {
    case 'entry': {
      // Entry 的必填字段（服务端 schema 里是 required）：缺了就没法定位、没法翻页。
      if (typeof frame.cursor !== 'string' || frame.cursor === '') return null
      if (typeof frame.timestamp !== 'string' || frame.timestamp === '') return null
      if (typeof frame.level !== 'string' || frame.level === '') return null
      if (typeof frame.message !== 'string') return null
      return { kind: 'entry', entry: frame as unknown as LogEntry }
    }
    case 'end': {
      if (typeof frame.reason !== 'string' || frame.reason === '') return null
      return { kind: 'end', reason: frame.reason }
    }
    case 'error': {
      return {
        kind: 'error',
        code: typeof frame.code === 'string' ? frame.code : 'STREAM_FAILED',
        message: typeof frame.message === 'string' ? frame.message : '',
      }
    }
    default:
      return null
  }
}

/** 结束原因的中文说明。未知原因原样显示——不猜。 */
export const LOG_STREAM_END_LABELS: Record<string, string> = {
  client_closed: '已关闭连接',
  duration_limit: '到达单次订阅时长上限',
  idle_timeout: '一段时间没有新日志（空闲超时）',
  entry_limit: '到达单次订阅条数上限',
  byte_limit: '到达单次订阅字节上限',
  journal_exit: 'journal 跟随进程结束',
}

export function logStreamEndLabel(reason: string): string {
  return (LOG_STREAM_END_LABELS as Record<string, string | undefined>)[reason] ?? reason
}

export interface LogStreamHandle {
  close(): void
  restart(): void
  isOpen(): boolean
}

function browserEventSource(url: string): EventSourceLike {
  if (typeof EventSource === 'undefined') {
    throw new Error('当前环境不支持 EventSource')
  }
  // 同源请求默认携带 Cookie，与服务端“靠会话 Cookie 鉴权”的设计一致。
  return new EventSource(url, { withCredentials: false })
}

/**
 * 打开一条日志实时流。
 *
 * 返回句柄由页面负责 close()——组件卸载时必须关，否则服务端会按用户累计连接数
 * 429，而且代理那边会一直挂着 journalctl。
 */
export function openLogStream(
  base: string,
  query: LogQuery,
  handlers: LogStreamHandlers = {},
  factory: EventSourceFactory = browserEventSource,
): LogStreamHandle {
  const url = logStreamUrl(base, query)
  let source: EventSourceLike | null = null
  let open = false
  let closedByCaller = false

  function attach(): void {
    const created = factory(url)
    source = created
    handlers.onState?.('connecting')
    created.addEventListener('open', () => {
      open = true
      handlers.onState?.('open')
    })

    created.addEventListener('entry', (event: MessageEvent) => {
      const frame = parseLogStreamFrame('entry', event.data as string | undefined)
      if (frame === null || frame.kind !== 'entry') {
        handlers.onInvalidFrame?.(String(event.data ?? ''))
        return
      }
      handlers.onEntry?.(frame.entry)
    })

    created.addEventListener('end', (event: MessageEvent) => {
      const frame = parseLogStreamFrame('end', event.data as string | undefined)
      if (frame === null || frame.kind !== 'end') {
        handlers.onInvalidFrame?.(String(event.data ?? ''))
        return
      }
      open = false
      handlers.onEnd?.(frame.reason)
    })

    created.addEventListener('error', (event: MessageEvent) => {
      // 服务端发的 error 帧带 data；EventSource 自己的连接错误不带。
      if (typeof event.data === 'string' && event.data !== '') {
        const frame = parseLogStreamFrame('error', event.data)
        if (frame === null || frame.kind !== 'error') {
          handlers.onInvalidFrame?.(event.data)
          return
        }
        open = false
        handlers.onError?.(frame.code, frame.message)
        return
      }
      open = false
      if (closedByCaller) return
      // readyState 为 CONNECTING 表示浏览器正按 Last-Event-ID 重连；
      // CLOSED 表示它放弃了（例如服务端回了 4xx），此时页面必须自己给说法。
      handlers.onState?.(created.readyState === 2 ? 'closed' : 'reconnecting')
    })
  }

  attach()

  return {
    close() {
      closedByCaller = true
      open = false
      source?.close()
      source = null
      handlers.onState?.('closed')
    },
    restart() {
      closedByCaller = false
      open = false
      source?.close()
      attach()
    },
    isOpen() {
      return open
    },
  }
}

// ---------------------------------------------------------------- 纯函数

/** 一页的统计。截断与脱敏要能数出来，而不是只靠颜色。 */
export interface LogPageSummary {
  shown: number
  scanned: number
  truncated: boolean
  truncatedReason: string
  redacted: number
  truncatedEntries: number
  binary: number
}

export function summarizeLogPage(page: LogPage | null | undefined): LogPageSummary {
  const entries = pageEntries(page)
  let redacted = 0
  let truncatedEntries = 0
  let binary = 0
  for (const entry of entries) {
    if (entry.redacted) redacted += 1
    if (entry.truncated) truncatedEntries += 1
    if (entry.binary) binary += 1
  }
  return {
    shown: entries.length,
    scanned: page?.scanned ?? 0,
    truncated: page?.truncated === true,
    truncatedReason: page?.truncated_reason ?? '',
    redacted,
    truncatedEntries,
    binary,
  }
}

/** 条目的可复制文本：带上时间与级别，粘出去仍然读得懂。 */
export function entryPlainText(entry: LogEntry): string {
  const parts = [entry.timestamp]
  if (entry.unit) parts.push(entry.unit)
  if (entry.identifier) parts.push(entry.identifier)
  parts.push(`[${entry.level}]`)
  parts.push(entry.binary ? '（journald 未提供文本）' : entry.message)
  if (entry.truncated) parts.push('（消息被截断）')
  if (entry.redacted) parts.push('（含被过滤的秘密）')
  return parts.join(' ')
}
