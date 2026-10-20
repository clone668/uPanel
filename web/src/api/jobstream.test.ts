// 事件流客户端测试。
//
// 用假 EventSource 驱动：真 EventSource 只在浏览器里存在，而这里要验的是
// “帧怎么解析、状态怎么变、重连怎么做”——这些逻辑不该等到有浏览器才能测。

import { describe, expect, it } from 'vitest'

import {
  jobEventsUrl,
  openJobStream,
  parseJobStreamFrame,
  type EventSourceLike,
  type JobStreamEvent,
  type JobStreamState,
} from './jobstream'

/** 假 EventSource：记录监听器，测试里手动触发。 */
class FakeEventSource implements EventSourceLike {
  static instances: FakeEventSource[] = []
  readonly url: string
  readyState = 0
  closed = false
  private listeners = new Map<string, ((event: MessageEvent) => void)[]>()

  constructor(url: string) {
    this.url = url
    FakeEventSource.instances.push(this)
  }

  addEventListener(type: string, listener: (event: MessageEvent) => void): void {
    const existing = this.listeners.get(type) ?? []
    existing.push(listener)
    this.listeners.set(type, existing)
  }

  close(): void {
    this.closed = true
    this.readyState = 2
  }

  emit(type: string, data?: string): void {
    const listeners = this.listeners.get(type) ?? []
    const event = { data } as MessageEvent
    for (const listener of listeners) listener(event)
  }
}

function fakeFactory(): { factory: (url: string) => EventSourceLike; last: () => FakeEventSource } {
  FakeEventSource.instances = []
  return {
    factory: (url) => new FakeEventSource(url),
    last: () => {
      const instance = FakeEventSource.instances.at(-1)
      if (!instance) throw new Error('没有创建任何 EventSource')
      return instance
    },
  }
}

function collect(): {
  events: JobStreamEvent[]
  states: JobStreamState[]
  invalid: string[]
  handlers: {
    onEvent: (event: JobStreamEvent) => void
    onState: (state: JobStreamState) => void
    onInvalidFrame: (raw: string) => void
  }
} {
  const events: JobStreamEvent[] = []
  const states: JobStreamState[] = []
  const invalid: string[] = []
  return {
    events,
    states,
    invalid,
    handlers: {
      onEvent: (event) => events.push(event),
      onState: (state) => states.push(state),
      onInvalidFrame: (raw) => invalid.push(raw),
    },
  }
}

describe('jobEventsUrl', () => {
  it('与契约的 events_url 一致，并对任务 id 做编码', () => {
    expect(jobEventsUrl('job_abc')).toBe('/api/v1/jobs/job_abc/events')
    expect(jobEventsUrl('a/b')).toBe('/api/v1/jobs/a%2Fb/events')
  })
})

describe('parseJobStreamFrame', () => {
  it('解析合法帧', () => {
    const parsed = parseJobStreamFrame(
      JSON.stringify({
        seq: 7,
        kind: 'progress',
        payload: { step: 'ping_done' },
        created_at: '2026-10-20T09:00:00Z',
      }),
    )
    expect(parsed).toEqual({
      seq: 7,
      kind: 'progress',
      payload: { step: 'ping_done' },
      created_at: '2026-10-20T09:00:00Z',
    })
  })

  it('seq=0 是合法的 gap 帧位置（服务端用 0 表示不是持久事件）', () => {
    const parsed = parseJobStreamFrame(
      JSON.stringify({ seq: 0, kind: 'gap', payload: { reason: 'history_pruned' } }),
    )
    expect(parsed?.kind).toBe('gap')
    expect(parsed?.seq).toBe(0)
  })

  it.each([
    ['空值', undefined],
    ['空字符串', ''],
    ['非 JSON', 'not json'],
    ['JSON 数组', '[]'],
    ['缺少 seq', JSON.stringify({ kind: 'log', payload: {} })],
    ['seq 不是整数', JSON.stringify({ seq: 1.5, kind: 'log', payload: {} })],
    ['seq 为负', JSON.stringify({ seq: -1, kind: 'log', payload: {} })],
    ['未知 kind', JSON.stringify({ seq: 1, kind: 'stdout', payload: {} })],
    ['缺少 kind', JSON.stringify({ seq: 1, payload: {} })],
  ])('坏帧返回 null：%s', (_label, data) => {
    expect(parseJobStreamFrame(data as string | undefined)).toBeNull()
  })

  it('payload 不是对象时退化成空对象，不抛异常', () => {
    const parsed = parseJobStreamFrame(JSON.stringify({ seq: 2, kind: 'log', payload: 'text' }))
    expect(parsed?.payload).toEqual({})
  })
})

describe('openJobStream', () => {
  it('建立连接后进入 open 状态，并分发 status 事件', () => {
    const { factory, last } = fakeFactory()
    const sink = collect()
    const handle = openJobStream('job_1', sink.handlers, factory)

    const source = last()
    expect(source.url).toBe('/api/v1/jobs/job_1/events')
    expect(sink.states).toEqual(['connecting'])

    source.readyState = 1
    source.emit('open')
    expect(sink.states).toEqual(['connecting', 'open'])

    source.emit(
      'status',
      JSON.stringify({ seq: 4, kind: 'status', payload: { status: 'running' }, created_at: '' }),
    )
    expect(sink.events).toHaveLength(1)
    expect(sink.events[0]?.seq).toBe(4)
    handle.close()
  })

  it('丢弃坏帧并计数，不让一帧坏数据打断整条流', () => {
    const { factory, last } = fakeFactory()
    const sink = collect()
    openJobStream('job_1', sink.handlers, factory)

    const source = last()
    source.emit('log', 'not json')
    source.emit(
      'log',
      JSON.stringify({ seq: 5, kind: 'log', payload: { line: 'ok' }, created_at: '' }),
    )
    expect(sink.invalid).toEqual(['not json'])
    expect(sink.events).toHaveLength(1)
  })

  it('event 名与 data.kind 不一致的帧按坏帧处理（错投的帧不能当成真事件）', () => {
    const { factory, last } = fakeFactory()
    const sink = collect()
    openJobStream('job_1', sink.handlers, factory)

    last().emit('progress', JSON.stringify({ seq: 6, kind: 'log', payload: {} }))
    expect(sink.events).toHaveLength(0)
    expect(sink.invalid).toHaveLength(1)
  })

  it('断线时报告 reconnecting；浏览器放弃（readyState=CLOSED）时报告 closed', () => {
    const { factory, last } = fakeFactory()
    const sink = collect()
    openJobStream('job_1', sink.handlers, factory)

    const source = last()
    source.readyState = 0
    source.emit('error')
    expect(sink.states.at(-1)).toBe('reconnecting')

    source.readyState = 2
    source.emit('error')
    expect(sink.states.at(-1)).toBe('closed')
  })

  it('close 之后关闭连接，并且不再继续报告重连', () => {
    const { factory, last } = fakeFactory()
    const sink = collect()
    const handle = openJobStream('job_1', sink.handlers, factory)

    const source = last()
    handle.close()
    expect(source.closed).toBe(true)
    expect(handle.isOpen()).toBe(false)

    const before = sink.states.length
    source.emit('error')
    expect(sink.states).toHaveLength(before)
  })

  it('restart 会新建一条不带断点的连接（历史被清理后必须这样）', () => {
    const { factory, last } = fakeFactory()
    const sink = collect()
    const handle = openJobStream('job_1', sink.handlers, factory)
    const first = last()
    handle.restart()
    const second = last()

    expect(second).not.toBe(first)
    expect(first.closed).toBe(true)
    expect(second.url).toBe('/api/v1/jobs/job_1/events')
    expect(sink.states.filter((state) => state === 'connecting')).toHaveLength(2)
    handle.close()
  })
})
