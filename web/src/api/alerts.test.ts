// 告警展示口径与确认动作的测试（P13.4）。
//
// 断言的落点：界面上的每一句话都要能被后端事实支撑。
//   - 双口径条件必须带"任一/全部"，否则读的人会把 any 当成 all；
//   - 事件总结先说状态再说"有没有人确认"，顺序反了会把正在报的告警写成过去式；
//   - 确认是 POST /alerts/events/:id/ack，且它的返回不改状态。

import { afterEach, describe, expect, it, vi } from 'vitest'

import {
  ackAlertEvent,
  acknowledgementState,
  alertConditionText,
  alertEventSummary,
  fetchAlertEvents,
  formatClock,
  formatDuration,
  type AlertEvent,
  type AlertRule,
} from './alerts'

function event(overrides: Partial<AlertEvent> = {}): AlertEvent {
  return {
    id: 7,
    rule_id: 2,
    rule_name: 'CPU 持续高',
    scope: 'host',
    target_id: '',
    metric: 'cpu_usage_percent',
    comparison: 'gt',
    threshold: 90,
    state: 'firing',
    started_at: '2026-10-26T09:00:00Z',
    resolved_at: null,
    duration_seconds: 0,
    peak_value: 96.5,
    silenced: false,
    acked_at: null,
    acked_by: '',
    ...overrides,
  }
}

function rule(overrides: Partial<AlertRule> = {}): AlertRule {
  return {
    id: 2,
    name: 'CPU 持续高',
    scope: 'host',
    target_id: '',
    metric: 'cpu_usage_percent',
    comparison: 'gt',
    threshold: 90,
    secondary_metric: null,
    secondary_comparison: null,
    secondary_threshold: null,
    combination: 'any',
    duration_seconds: 300,
    recovery_threshold: 85,
    cooldown_seconds: 600,
    enabled: true,
    version: 3,
    state: 'ok',
    state_since: '2026-10-26T09:00:00Z',
    recover_since: '',
    cooldown_until: '',
    last_eval_at: '2026-10-26T09:00:00Z',
    last_value: null,
    last_reason: '',
    created_at: '2026-10-20T00:00:00Z',
    updated_at: '2026-10-20T00:00:00Z',
    ...overrides,
  }
}

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('条件文案', () => {
  it('单口径：指标 + 方向 + 阈值', () => {
    expect(alertConditionText(rule())).toBe('cpu_usage_percent > 90')
  })

  it('非 host 口径把目标名写进去，否则三块盘分不清', () => {
    expect(
      alertConditionText(
        rule({ scope: 'filesystem', target_id: '/srv', metric: 'used_percent', comparison: 'gte', threshold: 90 }),
      ),
    ).toBe('［/srv］used_percent ≥ 90')
  })

  it('双口径带合成词：any 是"或"', () => {
    expect(
      alertConditionText(
        rule({
          metric: 'load1',
          comparison: 'gt',
          threshold: 8,
          secondary_metric: 'cpu_usage_percent',
          secondary_comparison: 'gt',
          secondary_threshold: 90,
          combination: 'any',
        }),
      ),
    ).toBe('load1 > 8 或 cpu_usage_percent > 90')
  })

  it('双口径带合成词：all 是"且"', () => {
    expect(
      alertConditionText(
        rule({
          metric: 'load1',
          comparison: 'gt',
          threshold: 8,
          secondary_metric: 'cpu_usage_percent',
          secondary_comparison: 'gt',
          secondary_threshold: 90,
          combination: 'all',
        }),
      ),
    ).toBe('load1 > 8 且 cpu_usage_percent > 90')
  })

  it('副条件缺一项就按单口径说，不写半句', () => {
    expect(
      alertConditionText(rule({ secondary_metric: 'load1', secondary_comparison: null, secondary_threshold: 8 })),
    ).toBe('cpu_usage_percent > 90')
  })
})

describe('时长与时刻', () => {
  it('秒 / 分 / 时 / 天各自进位', () => {
    expect(formatDuration(45)).toBe('45 秒')
    expect(formatDuration(90)).toBe('1 分钟')
    expect(formatDuration(3600)).toBe('1 小时')
    expect(formatDuration(5400)).toBe('1 小时 30 分钟')
    expect(formatDuration(90000)).toBe('1 天 1 小时')
  })

  it('非法输入说"未知"，不编一个数', () => {
    expect(formatDuration(Number.NaN)).toBe('未知')
    expect(formatDuration(-1)).toBe('未知')
  })

  it('时刻是本地的 MM-DD HH:mm:ss；解析不了原样返回', () => {
    expect(formatClock('2026-10-26T09:00:00Z')).toMatch(/^\d{2}-\d{2} \d{2}:\d{2}:\d{2}$/)
    expect(formatClock('不是时间')).toBe('不是时间')
  })
})

describe('事件总结', () => {
  const now = new Date('2026-10-26T09:10:00Z')

  it('还在持续：先状态和时长，再说没人确认', () => {
    expect(alertEventSummary(event(), now)).toBe('还在持续，已经 10 分钟；还没有人确认')
  })

  it('已恢复：用事件自己的持续时长，不用现在推算', () => {
    expect(
      alertEventSummary(
        event({ state: 'resolved', resolved_at: '2026-10-26T09:04:00Z', duration_seconds: 240 }),
        now,
      ),
    ).toBe('已恢复，持续了 4 分钟；还没有人确认')
  })

  it('静默窗口内发生的事要说出来——它不是"没报过"', () => {
    expect(alertEventSummary(event({ silenced: true }), now)).toBe(
      '还在持续，已经 10 分钟；触发时处在维护静默窗口内，没有发出通知；还没有人确认',
    )
  })

  it('确认过的人与时刻写在同一句里', () => {
    expect(alertEventSummary(event({ acked_at: '2026-10-26T09:05:00Z', acked_by: 'alice' }), now)).toBe(
      `还在持续，已经 10 分钟；alice 在 ${formatClock('2026-10-26T09:05:00Z')} 确认过`,
    )
  })

  it('确认人是空串时不留窟窿', () => {
    expect(alertEventSummary(event({ acked_at: '2026-10-26T09:05:00Z' }), now)).toContain('某人 在')
  })
})

describe('有没有人知道', () => {
  it('还在报 + 没人确认，才是要红的那一格', () => {
    expect(acknowledgementState(event())).toEqual({ unacked: true, needsAttention: true })
  })

  it('还在报但有人确认过：有人在管，只是没解决', () => {
    expect(acknowledgementState(event({ acked_at: '2026-10-26T09:05:00Z' }))).toEqual({
      unacked: false,
      needsAttention: false,
    })
  })

  it('已恢复且没人确认过：不算需要盯', () => {
    expect(acknowledgementState(event({ state: 'resolved' }))).toEqual({
      unacked: true,
      needsAttention: false,
    })
  })
})

describe('查询与确认走统一客户端', () => {
  function jsonResponse(body: unknown): Response {
    return new Response(JSON.stringify(body), {
      status: 200,
      headers: { 'Content-Type': 'application/json', 'X-Request-ID': 'rid-alerts' },
    })
  }

  function stubFetch(body: unknown): ReturnType<typeof vi.fn<() => Promise<Response>>> {
    const mock = vi.fn(() => Promise.resolve(jsonResponse(body)))
    vi.stubGlobal('fetch', mock)
    return mock
  }

  it('事件列表的过滤条件进查询串', async () => {
    const mock = stubFetch({ data: [event()], request_id: 'rid-alerts' })

    await fetchAlertEvents({ state: 'firing', ruleId: 2, limit: 20 })

    const url = new URL(String((mock.mock.calls as unknown as [string][])[0]?.[0]), 'http://localhost')
    expect(url.pathname).toBe('/api/v1/alerts/events')
    expect(url.searchParams.get('state')).toBe('firing')
    expect(url.searchParams.get('rule_id')).toBe('2')
    expect(url.searchParams.get('limit')).toBe('20')
  })

  it('不给条件就不带查询串', async () => {
    const mock = stubFetch({ data: [], request_id: 'rid-alerts' })

    await fetchAlertEvents()

    const url = new URL(String((mock.mock.calls as unknown as [string][])[0]?.[0]), 'http://localhost')
    expect(url.search).toBe('')
  })

  it('data 为 null 时给空数组，页面不用再判一次', async () => {
    stubFetch({ data: null, request_id: 'rid-alerts' })
    const result = await fetchAlertEvents()
    expect(result.data).toEqual([])
  })

  it('确认打到 /alerts/events/:id/ack，且是 POST', async () => {
    const mock = stubFetch({
      data: event({ acked_at: '2026-10-26T09:05:00Z', acked_by: 'alice' }),
      request_id: 'rid-alerts',
    })

    const result = await ackAlertEvent(7)

    const init = (mock.mock.calls as unknown as [string, RequestInit | undefined][])[0]?.[1]
    const url = new URL(String((mock.mock.calls as unknown as [string][])[0]?.[0]), 'http://localhost')
    expect(url.pathname).toBe('/api/v1/alerts/events/7/ack')
    expect(init?.method).toBe('POST')
    // 确认不改状态机：返回的这条仍然是 firing。
    expect(result.data?.state).toBe('firing')
    expect(result.data?.acked_by).toBe('alice')
  })
})
