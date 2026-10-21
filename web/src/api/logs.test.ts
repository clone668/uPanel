// 日志 DTO 与实时流的纯逻辑用例（P05.3）。
//
// 这里不渲染页面，只验三件容易出错的事：
//   1) 查询参数怎么拼：空值不能变成 `unit=` 这种"看起来筛了其实没筛"的参数；
//   2) 帧怎么解析：服务端的 error 帧与 EventSource 自身的连接错误同名，必须分开；
//   3) 默认时间窗口：它是分钟对齐的 RFC3339，而不是浏览器本地时间原样发出去。

import { describe, expect, it, vi } from 'vitest'

import {
  DEFAULT_LOG_LIMIT,
  MAX_LOG_LIMIT,
  MAX_LOG_SPAN_HOURS,
  defaultWindow,
  entryPlainText,
  isValidLevel,
  isValidUnit,
  logLevelLabel,
  logLevelType,
  logQueryParams,
  logSourceLabel,
  logStreamEndLabel,
  logStreamQueryParams,
  logStreamUrl,
  openLogStream,
  pageEntries,
  parseLogStreamFrame,
  summarizeLogPage,
  toLocalInput,
  toRFC3339,
  type EventSourceLike,
  type LogEntry,
  type LogPage,
} from './logs'

/** 一条真实形状的条目（游标格式与 systemd 255 一致）。 */
function entry(overrides: Partial<LogEntry> = {}): LogEntry {
  return {
    cursor: 's=8a1e0b6f2c4d4f39;i=41;b=8a1e0b6f2c4d4f39;m=5f0c;t=5f0c;x=9b30',
    timestamp: '2026-10-21T02:59:00Z',
    level: 'warning',
    message: 'sshd: 收到连接',
    truncated: false,
    redacted: false,
    binary: false,
    unit: 'ssh.service',
    identifier: 'sshd',
    comm: 'sshd',
    pid: 900,
    ...overrides,
  }
}

function samplePage(overrides: Partial<LogPage> = {}): LogPage {
  return {
    source: 'journal',
    from: '2026-10-21T02:00:00Z',
    to: '2026-10-21T03:00:00Z',
    entries: [entry()],
    next_cursor: 's=8a1e0b6f2c4d4f39;i=40;b=8a1e0b6f2c4d4f39;m=5f0c;t=5f0c;x=9b2f',
    truncated: false,
    scanned: 2,
    duration_ms: 7,
    ...overrides,
  }
}

describe('查询参数', () => {
  it('空值不进 URL：`unit=` 会被服务端当成"筛了一个空单元名"的歧义', () => {
    expect(logQueryParams({ source: 'journal', unit: '', level: '', from: '', to: '', cursor: '' })).toEqual({
      source: 'journal',
    })
  })

  it('只放契约里的键，且 unit 去首尾空白', () => {
    const params = logQueryParams({
      source: 'journal',
      unit: '  ssh.service  ',
      level: 'error',
      from: '2026-10-21T02:00:00Z',
      to: '2026-10-21T03:00:00Z',
      limit: 200,
    })
    expect(params).toEqual({
      source: 'journal',
      unit: 'ssh.service',
      level: 'error',
      from: '2026-10-21T02:00:00Z',
      to: '2026-10-21T03:00:00Z',
      limit: 200,
    })
  })

  it('limit 为 0 或负数时不发送（由服务端用默认值，而不是"要 0 条"）', () => {
    expect(logQueryParams({ limit: 0 })).toEqual({})
    expect(logQueryParams({ limit: -5 })).toEqual({})
  })

  it('限额常量与服务端一致', () => {
    expect(DEFAULT_LOG_LIMIT).toBe(100)
    expect(MAX_LOG_LIMIT).toBe(1000)
    expect(MAX_LOG_SPAN_HOURS).toBe(24)
  })
})

describe('输入早拦', () => {
  it('单元名只接受服务端认可的形状', () => {
    expect(isValidUnit('ssh.service')).toBe(true)
    expect(isValidUnit('systemd-journald.service')).toBe(true)
    expect(isValidUnit('ssh*')).toBe(false)
    expect(isValidUnit('sshd')).toBe(false)
    expect(isValidUnit('../../etc/passwd')).toBe(false)
    expect(isValidUnit('ssh.service; rm -rf /')).toBe(false)
  })

  it('级别只接受闭集', () => {
    expect(isValidLevel('')).toBe(true)
    expect(isValidLevel('warning')).toBe(true)
    expect(isValidLevel('verbose')).toBe(false)
    expect(isValidLevel('3')).toBe(false)
  })

  it('未知级别原样显示，不冒充 info', () => {
    expect(logLevelLabel('unknown')).toBe('未知级别')
    expect(logLevelLabel('verbose')).toBe('verbose')
    expect(logLevelLabel('')).toBe('未知级别')
    expect(logLevelType('error')).toBe('error')
    expect(logLevelType('unknown')).toBe('default')
  })

  it('数据源中文名与未知值', () => {
    expect(logSourceLabel('journal')).toBe('系统日志（journald）')
    expect(logSourceLabel('kernel')).toBe('内核日志')
    expect(logSourceLabel('dmesg')).toBe('dmesg')
  })
})

describe('时间', () => {
  it('本地时间转 RFC3339 后能被 Date 解析回同一时刻', () => {
    const local = '2026-10-21T03:00'
    const rfc = toRFC3339(local)
    expect(rfc).toMatch(/Z$/)
    expect(new Date(rfc).getTime()).toBe(new Date(local).getTime())
    expect(toLocalInput(rfc)).toBe(local)
  })

  it('空串与非法值不产生时间', () => {
    expect(toRFC3339('')).toBe('')
    expect(toRFC3339('不是时间')).toBe('')
    expect(toLocalInput('')).toBe('')
    expect(toLocalInput('2026-13-45T99:99:99Z')).toBe('')
  })

  it('默认窗口是最近一小时，且分钟对齐', () => {
    const now = new Date('2026-10-21T03:05:42.500Z')
    const { from, to } = defaultWindow(now)
    const span = (new Date(to).getTime() - new Date(from).getTime()) / 3_600_000
    expect(span).toBeCloseTo(1, 6)
    expect(new Date(to).getSeconds()).toBe(0)
    expect(new Date(from).getSeconds()).toBe(0)
    expect(to).toMatch(/Z$/)
  })
})

describe('页与条目的归一化', () => {
  it('entries 为 null 时归一化成空数组', () => {
    expect(pageEntries(samplePage({ entries: null }))).toEqual([])
    expect(pageEntries(null)).toEqual([])
    expect(pageEntries(undefined)).toEqual([])
  })

  it('统计截断、脱敏与无文本的条数', () => {
    const page = samplePage({
      entries: [
        entry({ cursor: 'a', redacted: true }),
        entry({ cursor: 'b', truncated: true }),
        entry({ cursor: 'c', binary: true, message: '' }),
      ],
      truncated: true,
      truncated_reason: 'byte_limit',
      scanned: 400,
    })
    expect(summarizeLogPage(page)).toEqual({
      shown: 3,
      scanned: 400,
      truncated: true,
      truncatedReason: 'byte_limit',
      redacted: 1,
      truncatedEntries: 1,
      binary: 1,
    })
  })

  it('复制文本带上时间、来源、级别与标记', () => {
    const text = entryPlainText(entry({ truncated: true, redacted: true }))
    expect(text).toContain('2026-10-21T02:59:00Z')
    expect(text).toContain('ssh.service')
    expect(text).toContain('[warning]')
    expect(text).toContain('消息被截断')
    expect(text).toContain('被过滤的秘密')
    expect(entryPlainText(entry({ binary: true, message: '' }))).toContain('未提供文本')
  })
})

describe('实时流', () => {
  it('流的 URL 不带 cursor：续传只靠 Last-Event-ID', () => {
    const url = logStreamUrl('/api/v1', {
      source: 'journal',
      unit: 'ssh.service',
      level: 'error',
      cursor: 's=a;i=1;b=b;m=1;t=1;x=1',
      limit: 200,
    })
    expect(url).toBe('/api/v1/logs/stream?source=journal&unit=ssh.service&level=error&limit=200')
    expect(logStreamQueryParams({ cursor: 'x' })).toEqual({})
  })

  it('空查询不留下问号', () => {
    expect(logStreamUrl('/api/v1', {})).toBe('/api/v1/logs/stream')
  })

  it('按事件名解析三种帧，形状对不上就是坏帧', () => {
    // 服务端的 data 就是 Entry 本身：帧类型由 SSE 的 event 名给出，不在 data 里重复。
    expect(parseLogStreamFrame('entry', JSON.stringify(entry()))).toMatchObject({ kind: 'entry' })
    expect(parseLogStreamFrame('end', JSON.stringify({ reason: 'duration_limit' }))).toEqual({
      kind: 'end',
      reason: 'duration_limit',
    })
    expect(
      parseLogStreamFrame('error', JSON.stringify({ code: 'STREAM_FAILED', message: '挂了' })),
    ).toEqual({ kind: 'error', code: 'STREAM_FAILED', message: '挂了' })

    expect(parseLogStreamFrame('entry', '')).toBeNull()
    expect(parseLogStreamFrame('entry', '不是 JSON')).toBeNull()
    expect(parseLogStreamFrame('entry', JSON.stringify([1, 2]))).toBeNull()
    // 事件名说 entry、data 却是 end 的形状：不猜，直接算坏帧。
    expect(parseLogStreamFrame('entry', JSON.stringify({ reason: 'x' }))).toBeNull()
    // 缺必填字段（游标没法翻页、时间没法排序）同样是坏帧。
    expect(parseLogStreamFrame('entry', JSON.stringify({ ...entry(), cursor: '' }))).toBeNull()
    expect(parseLogStreamFrame('entry', JSON.stringify({ ...entry(), timestamp: '' }))).toBeNull()
    expect(parseLogStreamFrame('end', JSON.stringify({}))).toBeNull()
    expect(parseLogStreamFrame('end', JSON.stringify({ reason: '' }))).toBeNull()
  })

  it('结束原因：已知翻成中文，未知原样', () => {
    expect(logStreamEndLabel('idle_timeout')).toContain('空闲')
    expect(logStreamEndLabel('something_new')).toBe('something_new')
  })

  /** 假的 EventSource：手动派发事件，验证页面侧的分支。 */
  function fakeEventSource(): {
    source: EventSourceLike
    emit: (type: string, data?: string) => void
    wasClosed: () => boolean
  } {
    const listeners = new Map<string, Array<(event: MessageEvent) => void>>()
    let closed = false
    let readyState = 0
    const source: EventSourceLike = {
      get readyState() {
        return readyState
      },
      addEventListener(type, listener) {
        const list = listeners.get(type) ?? []
        list.push(listener)
        listeners.set(type, list)
      },
      close() {
        closed = true
        readyState = 2
      },
    }
    return {
      source,
      wasClosed: () => closed,
      emit(type, data) {
        for (const listener of listeners.get(type) ?? []) {
          listener({ data } as MessageEvent)
        }
      },
    }
  }

  it('entry/end 帧走各自的回调，close 会真的关连接', () => {
    const fake = fakeEventSource()
    const onEntry = vi.fn()
    const onEnd = vi.fn()
    const onState = vi.fn()
    const handle = openLogStream(
      '/api/v1',
      { source: 'journal' },
      { onEntry, onEnd, onState },
      () => fake.source,
    )
    expect(onState).toHaveBeenCalledWith('connecting')

    fake.emit('open')
    expect(onState).toHaveBeenLastCalledWith('open')
    expect(handle.isOpen()).toBe(true)

    fake.emit('entry', JSON.stringify(entry()))
    expect(onEntry).toHaveBeenCalledTimes(1)

    fake.emit('end', JSON.stringify({ reason: 'duration_limit' }))
    expect(onEnd).toHaveBeenCalledWith('duration_limit')
    expect(handle.isOpen()).toBe(false)

    handle.close()
    expect(fake.wasClosed()).toBe(true)
    expect(onState).toHaveBeenLastCalledWith('closed')
  })

  it('服务端的 error 帧与连接错误分开处理', () => {
    const fake = fakeEventSource()
    const onError = vi.fn()
    const onState = vi.fn()
    openLogStream('/api/v1', {}, { onError, onState }, () => fake.source)

    // 带 data 的 error：服务端报告流失败。
    fake.emit(
      'error',
      JSON.stringify({ code: 'STREAM_FAILED', message: 'journalctl 退出码 1' }),
    )
    expect(onError).toHaveBeenCalledWith('STREAM_FAILED', 'journalctl 退出码 1')
    expect(onState).not.toHaveBeenCalledWith('reconnecting')

    // 不带 data 的 error：连接层问题，交给 state 表达。
    fake.emit('error')
    expect(onState).toHaveBeenCalledWith('reconnecting')
  })

  it('无法解析的帧被计数，不影响后续帧', () => {
    const fake = fakeEventSource()
    const onEntry = vi.fn()
    const onInvalidFrame = vi.fn()
    openLogStream('/api/v1', {}, { onEntry, onInvalidFrame }, () => fake.source)

    fake.emit('entry', '不是 JSON')
    expect(onInvalidFrame).toHaveBeenCalledTimes(1)
    fake.emit('entry', JSON.stringify(entry()))
    expect(onEntry).toHaveBeenCalledTimes(1)
  })
})
