// 任务事件流（SSE）客户端（CONTRACTS.md 4.3）。
//
// 为什么用 EventSource 而不是自己解析 fetch 流：
//   - 契约要求支持 Last-Event-ID 重连，浏览器 EventSource 原生就会带上最后收到的 id，
//     自己实现等于重写一遍规范，还会漏掉 `retry:` 之类的细节；
//   - 断线重连次数与退避由浏览器管理，页面只需要如实反映“现在连上了没有”。
//
// 页面只从 onEvent 拿到**已经校验过**的事件；格式不对的帧直接丢弃并计数，
// 不猜、不补，也不让一帧坏数据把整个任务中心搞崩。

import { API_BASE } from './client'
import type { JobLogEntry } from './jobs'

export type JobEventKind = 'status' | 'progress' | 'log' | 'gap'

export interface JobStreamEvent extends JobLogEntry {
  kind: JobEventKind
}

export type JobStreamState = 'connecting' | 'open' | 'reconnecting' | 'closed'

export interface JobStreamHandlers {
  onEvent?: (event: JobStreamEvent) => void
  onState?: (state: JobStreamState) => void
  /** 收到无法解析的帧时调用（计数用，不打断流）。 */
  onInvalidFrame?: (raw: string) => void
}

/** 只依赖 EventSource 的这几个成员，测试里可以塞一个假实现。 */
export interface EventSourceLike {
  readonly readyState: number
  addEventListener(type: string, listener: (event: MessageEvent) => void): void
  close(): void
}

export type EventSourceFactory = (url: string) => EventSourceLike

/** 事件流地址：契约里的 events_url 就是 `/api/v1/jobs/:id/events`。 */
export function jobEventsUrl(jobId: string): string {
  return `${API_BASE}/jobs/${encodeURIComponent(jobId)}/events`
}

const KINDS: readonly JobEventKind[] = ['status', 'progress', 'log', 'gap']

/** 把一帧 SSE 的 data 解析成事件；不合法返回 null。 */
export function parseJobStreamFrame(data: string | null | undefined): JobStreamEvent | null {
  if (!data) return null
  let parsed: unknown
  try {
    parsed = JSON.parse(data)
  } catch {
    return null
  }
  if (parsed === null || typeof parsed !== 'object') return null
  const frame = parsed as Record<string, unknown>
  const seq = frame.seq
  const kind = frame.kind
  if (typeof seq !== 'number' || !Number.isInteger(seq) || seq < 0) return null
  if (typeof kind !== 'string' || !KINDS.includes(kind as JobEventKind)) return null
  const payload =
    frame.payload !== null && typeof frame.payload === 'object' && !Array.isArray(frame.payload)
      ? (frame.payload as Record<string, unknown>)
      : {}
  return {
    seq,
    kind: kind as JobEventKind,
    payload,
    created_at: typeof frame.created_at === 'string' ? frame.created_at : '',
  }
}

export interface JobStreamHandle {
  close(): void
  /** 丢掉断点、从头重新建立连接（历史被清理后必须这样做）。 */
  restart(): void
  /** 当前是否处于已连接状态。 */
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
 * 打开一条任务事件流。
 *
 * 返回句柄由调用方负责 close()——组件卸载时必须关，否则每个打开过的任务
 * 都会留下一条连接（服务端按用户限制并发数，多余连接会被 429）。
 */
export function openJobStream(
  jobId: string,
  handlers: JobStreamHandlers = {},
  factory: EventSourceFactory = browserEventSource,
): JobStreamHandle {
  const url = jobEventsUrl(jobId)
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
    for (const kind of KINDS) {
      created.addEventListener(kind, (event: MessageEvent) => {
        const parsed = parseJobStreamFrame(event.data as string | undefined)
        if (parsed === null) {
          handlers.onInvalidFrame?.(String(event.data ?? ''))
          return
        }
        // 服务端写入的 event 名与 data.kind 必须一致；不一致按坏帧处理。
        if (parsed.kind !== kind) {
          handlers.onInvalidFrame?.(String(event.data ?? ''))
          return
        }
        handlers.onEvent?.(parsed)
      })
    }
    created.addEventListener('error', () => {
      open = false
      if (closedByCaller) return
      // readyState 为 CONNECTING 表示浏览器正在按 Last-Event-ID 重连；
      // CLOSED 表示它放弃了（例如服务端返回 4xx），此时页面必须自己给说法。
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
