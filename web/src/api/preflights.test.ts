// 预检 → 任务 提交链路的纯逻辑测试（P05.1）。
//
// 这里钉住的是“前端不许替服务端做决定”：
//   - 允许与否、是否要短语，全部来自预检；
//   - 过期预检不能提交；
//   - 幂等键是一次意图的标识，形状必须可判定；
//   - 提交请求只带 preflight_id 与（可选的）短语，不带目标与参数。

import { afterEach, describe, expect, it, vi } from 'vitest'

import {
  canSubmitPreflight,
  confirmationMatches,
  createPreflight,
  isPreflightExpired,
  newIdempotencyKey,
  preflightRemainingSeconds,
  preflightTimeoutFor,
  requiresConfirmation,
  submitJob,
  PACKAGE_PREFLIGHT_TIMEOUT_MS,
  PREFLIGHT_TIMEOUT_MS,
  type PreflightData,
} from './preflights'

function preflight(overrides: Partial<PreflightData> = {}): PreflightData {
  return {
    preflight_id: 'pf_0123456789abcdef0123456789abcdef',
    action: 'service.control',
    normalized_params: { unit: 'cron.service', operation: 'restart' },
    target: {
      display_name: 'cron.service（Regular background program processing daemon）',
      identity_fingerprint: 'sha256:fingerprint',
    },
    expires_at: '2026-10-21T00:22:00Z',
    allowed: true,
    blockers: [],
    warnings: [
      { code: 'service_interruption', message: '操作期间该服务提供的能力会中断' },
    ],
    impacts: [
      { kind: 'service_availability', description: '服务在操作期间不可用；已经建立的连接可能被断开' },
    ],
    requires_reauth: false,
    confirmation_phrase: null,
    cancellable: false,
    locks: ['service:cron.service'],
    ...overrides,
  }
}

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('预检有效期', () => {
  it('过期判定按 expires_at，过期与无法解析都算过期', () => {
    const plan = preflight()
    expect(isPreflightExpired(plan, new Date('2026-10-21T00:21:00Z'))).toBe(false)
    expect(isPreflightExpired(plan, new Date('2026-10-21T00:22:00Z'))).toBe(true)
    expect(isPreflightExpired(plan, new Date('2026-10-21T00:23:00Z'))).toBe(true)
    expect(isPreflightExpired(preflight({ expires_at: 'not-a-time' }), new Date())).toBe(true)
  })

  it('剩余秒数与能否提交', () => {
    const plan = preflight()
    expect(preflightRemainingSeconds(plan, new Date('2026-10-21T00:21:00Z'))).toBe(60)
    expect(preflightRemainingSeconds(plan, new Date('2026-10-21T00:30:00Z'))).toBe(0)
    expect(canSubmitPreflight(plan, new Date('2026-10-21T00:21:00Z'))).toBe(true)
    expect(canSubmitPreflight(plan, new Date('2026-10-21T00:23:00Z'))).toBe(false)
  })

  it('被拒绝的预检无论是否过期都不能提交', () => {
    const blocked = preflight({
      allowed: false,
      blockers: [{ code: 'SERVICE_PROTECTED', message: '受保护的服务' }],
    })
    expect(canSubmitPreflight(blocked, new Date('2026-10-21T00:21:00Z'))).toBe(false)
  })
})

describe('确认短语', () => {
  it('不需要短语时任何输入都算通过', () => {
    const plan = preflight({ confirmation_phrase: null })
    expect(requiresConfirmation(plan)).toBe(false)
    expect(confirmationMatches(plan, '')).toBe(true)
    expect(confirmationMatches(plan, '随便写的')).toBe(true)
  })

  it('需要短语时必须逐字相同（区分大小写）', () => {
    const plan = preflight({ confirmation_phrase: 'PROBE AB2CD3' })
    expect(requiresConfirmation(plan)).toBe(true)
    expect(confirmationMatches(plan, 'PROBE AB2CD3')).toBe(true)
    expect(confirmationMatches(plan, 'probe ab2cd3')).toBe(false)
    expect(confirmationMatches(plan, '')).toBe(false)
  })
})

describe('幂等键', () => {
  it('形状是 UUID v4，且每次不同', () => {
    const first = newIdempotencyKey()
    const second = newIdempotencyKey()
    expect(first).toMatch(/^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/)
    expect(second).not.toBe(first)
  })

  it('没有 crypto.randomUUID 时仍生成可用的键', () => {
    vi.stubGlobal('crypto', { getRandomValues: undefined })
    const key = newIdempotencyKey()
    expect(key).toMatch(/^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/)
  })
})

describe('请求形态', () => {
  function jsonResponse(body: unknown, status = 200): Response {
    return new Response(JSON.stringify(body), {
      status,
      headers: { 'Content-Type': 'application/json', 'X-Request-ID': 'rid-preflight' },
    })
  }

  it('预检请求只带 action 与 params', async () => {
    const mock = vi.fn(() => Promise.resolve(jsonResponse({ data: {}, request_id: 'rid' })))
    vi.stubGlobal('fetch', mock)

    await createPreflight('service.control', { unit: 'cron.service', operation: 'restart' })

    const [url, init] = mock.mock.calls[0] as unknown as [string, RequestInit]
    expect(String(url)).toMatch(/\/api\/v1\/operations\/preflight$/)
    expect(init.method).toBe('POST')
    expect(JSON.parse(String(init.body))).toEqual({
      action: 'service.control',
      params: { unit: 'cron.service', operation: 'restart' },
    })
  })

  it('提交任务带幂等键，不带目标与参数', async () => {
    const mock = vi.fn(() => Promise.resolve(jsonResponse({ data: {}, request_id: 'rid' })))
    vi.stubGlobal('fetch', mock)

    await submitJob({ preflightId: 'pf_abc', idempotencyKey: 'key-1' })

    const [url, init] = mock.mock.calls[0] as unknown as [string, RequestInit]
    expect(String(url)).toMatch(/\/api\/v1\/jobs$/)
    const headers = init.headers as Record<string, string>
    expect(headers['Idempotency-Key']).toBe('key-1')
    expect(JSON.parse(String(init.body))).toEqual({ preflight_id: 'pf_abc' })
  })

  it('需要短语时把短语放进请求体', async () => {
    const mock = vi.fn(() => Promise.resolve(jsonResponse({ data: {}, request_id: 'rid' })))
    vi.stubGlobal('fetch', mock)

    await submitJob({ preflightId: 'pf_abc', confirmationPhrase: 'PROBE AB2CD3', idempotencyKey: 'k' })

    const [, init] = mock.mock.calls[0] as unknown as [string, RequestInit]
    expect(JSON.parse(String(init.body))).toEqual({
      preflight_id: 'pf_abc',
      confirmation_phrase: 'PROBE AB2CD3',
    })
  })
})

// 预检的超时分层（P11.2 验收第一轮的真事）。
//
// 软件包动作的预检要跑 `apt-get -s`：24.04 上 `--only-upgrade install zlib1g`
// 的模拟实测 15.6 秒，冷索引更久。前端原来对所有动作都用 20 秒，于是升级预检
// 在前端自己掐断，屏幕上是"请求超时"，服务端其实正在回答。
describe('预检超时', () => {
  it('普通动作 20 秒，软件包动作放宽', () => {
    expect(PREFLIGHT_TIMEOUT_MS).toBe(20_000)
    expect(preflightTimeoutFor('service.control')).toBe(PREFLIGHT_TIMEOUT_MS)
    expect(preflightTimeoutFor('storage.volume.format')).toBe(PREFLIGHT_TIMEOUT_MS)
    expect(preflightTimeoutFor('package.install')).toBe(PACKAGE_PREFLIGHT_TIMEOUT_MS)
    expect(preflightTimeoutFor('package.upgrade')).toBe(PACKAGE_PREFLIGHT_TIMEOUT_MS)
  })

  it('软件包动作的超时比服务端上限（90 秒）宽，好让服务端的话先到', () => {
    expect(PACKAGE_PREFLIGHT_TIMEOUT_MS).toBeGreaterThan(90_000)
  })

  it('预检请求真的用了这个超时，而不是写死的数', async () => {
    const timeout = vi.spyOn(AbortSignal, 'timeout')
    const mock = vi.fn(() => Promise.resolve(new Response(
      JSON.stringify({ data: {}, request_id: 'rid' }),
      { status: 200, headers: { 'Content-Type': 'application/json' } },
    )))
    vi.stubGlobal('fetch', mock)

    await createPreflight('package.install', { packages: ['cowsay'] })
    expect(timeout).toHaveBeenCalledWith(PACKAGE_PREFLIGHT_TIMEOUT_MS)

    timeout.mockClear()
    await createPreflight('service.control', { unit: 'cron.service', operation: 'start' })
    expect(timeout).toHaveBeenCalledWith(PREFLIGHT_TIMEOUT_MS)
  })
})
