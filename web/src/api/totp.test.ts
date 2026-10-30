// 二次验证三条接口与错误判别的用例（P14.6）。
//
// 这一组守的是前端在 TOTP 链路上不许自作主张的几处：
//   - 三条请求的方法、路径与请求体字段必须与 CONTRACTS 一致（DELETE 关掉、body 里
//     只出现调用方真的给了的因子）；
//   - `secondFactorRequired` 只认 `details.totp_required === true`，别的 401 一律
//     不算——把"口令错误"显示成"请输验证码"会让人一直在错的地方试；
//   - 种子只从 setup 的返回里读，前端不解析、不拼 otpauth URI，也不在 confirm 之后
//     再假设恢复码还在手里。

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { AUTH_TIMEOUT_MS } from './auth'
import type { ApiError } from './client'
import { setCsrfTokenProvider } from './client'
import {
  secondFactorRequired,
  totpConfirm,
  totpDisable,
  totpSetup,
  type TotpSetupData,
} from './totp'

function stubFetch(
  impl: (input: RequestInfo | URL, init?: RequestInit) => Promise<Response>,
): ReturnType<typeof vi.fn> {
  const mock = vi.fn(impl)
  vi.stubGlobal('fetch', mock)
  return mock
}

function dataResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify({ data: body, request_id: 'rid' }), {
    status,
    headers: { 'Content-Type': 'application/json' },
  })
}

function errorResponse(status: number, code: string, details?: unknown): Response {
  return new Response(
    JSON.stringify({ error: { code, message: 'x', details }, request_id: 'rid' }),
    { status, headers: { 'Content-Type': 'application/json' } },
  )
}

/** 取第 n 次请求的 [url, init]。 */
function callOf(mock: ReturnType<typeof vi.fn>, index = 0): [string, RequestInit] {
  const args = mock.mock.calls[index] as [string, RequestInit]
  return args
}

beforeEach(() => {
  setCsrfTokenProvider(() => 'csrf-token-1')
})

afterEach(() => {
  vi.unstubAllGlobals()
  vi.restoreAllMocks()
  setCsrfTokenProvider(() => null)
})

describe('三条请求的形状', () => {
  it('setup 是 POST 空对象，且只把返回里的字段原样交出去', async () => {
    const payload: TotpSetupData = {
      enrollment_id: 'enroll-1',
      otpauth_uri: 'otpauth://totp/uPanel:admin?secret=ABC&digits=6&period=30',
      issuer: 'uPanel',
      account: 'admin',
      digits: 6,
      period: 30,
      expires_at: '2026-01-01T00:10:00Z',
    }
    const mock = stubFetch(() => Promise.resolve(dataResponse(payload)))

    const result = await totpSetup()
    const [url, init] = callOf(mock)

    expect(url.endsWith('/auth/totp/setup')).toBe(true)
    expect(init.method).toBe('POST')
    expect(init.body).toBe(JSON.stringify({}))
    expect(result.data).toEqual(payload)
    // 过期时间缺失是代理没给，不是前端补的默认值。
    expect(result.data.expires_at).toBe('2026-01-01T00:10:00Z')
  })

  it('confirm 把 enrollment_id 与六位数一起发，字段名是 totp 不是 code', async () => {
    const mock = stubFetch(() =>
      Promise.resolve(dataResponse({ enabled: true, recovery_codes: ['3F7K9-QM2XP'] })),
    )

    const result = await totpConfirm('enroll-1', '123456')
    const [url, init] = callOf(mock)

    expect(url.endsWith('/auth/totp/confirm')).toBe(true)
    expect(init.method).toBe('POST')
    expect(JSON.parse(String(init.body))).toEqual({ enrollment_id: 'enroll-1', totp: '123456' })
    expect(result.data.recovery_codes).toHaveLength(1)
  })

  it('disable 用 DELETE，只带调用方真的给了的因子', async () => {
    const mock = stubFetch(() =>
      Promise.resolve(dataResponse({ enabled: false, revoked_sessions: 1 })),
    )
    const timeout = vi.spyOn(AbortSignal, 'timeout')

    const withCode = await totpDisable({ password: 'pw', totp: '123456' })
    expect(timeout).toHaveBeenCalledWith(AUTH_TIMEOUT_MS)
    expect(callOf(mock, 0)[1].method).toBe('DELETE')
    expect(JSON.parse(String(callOf(mock, 0)[1].body))).toEqual({ password: 'pw', totp: '123456' })
    expect(withCode.data.revoked_sessions).toBe(1)

    await totpDisable({ password: 'pw', recovery_code: '3F7K9-QM2XP' })
    expect(JSON.parse(String(callOf(mock, 1)[1].body))).toEqual({
      password: 'pw',
      recovery_code: '3F7K9-QM2XP',
    })

    // 两个都没给：前端不替人编一个空串上去，服务端回 400 由它说。
    await totpDisable({ password: 'pw' })
    expect(JSON.parse(String(callOf(mock, 2)[1].body))).toEqual({ password: 'pw' })
  })

  it('码不对时抛的是 ApiError，且错误体不经过加工', async () => {
    stubFetch(() =>
      Promise.resolve(
        errorResponse(401, 'UNAUTHENTICATED', { totp_required: true, reason: 'code_mismatch' }),
      ),
    )

    await expect(totpConfirm('enroll-1', '000000')).rejects.toMatchObject({
      code: 'UNAUTHENTICATED',
      status: 401,
      details: { totp_required: true, reason: 'code_mismatch' },
    })
  })
})

describe('secondFactorRequired', () => {
  it('只认 details.totp_required === true', () => {
    expect(secondFactorRequired({ details: { totp_required: true } })).toBe(true)
    expect(secondFactorRequired({ details: { totp_required: false } })).toBe(false)
    expect(secondFactorRequired({ details: { totp_required: 'true' } })).toBe(false)
    expect(secondFactorRequired({ details: {} })).toBe(false)
    expect(secondFactorRequired({})).toBe(false)
  })

  it('不是对象、details 不是对象、null 都回 false，而不是猜', () => {
    expect(secondFactorRequired(null)).toBe(false)
    expect(secondFactorRequired(undefined)).toBe(false)
    expect(secondFactorRequired('TOTP required')).toBe(false)
    expect(secondFactorRequired(new Error('invalid credentials'))).toBe(false)
    expect(secondFactorRequired({ details: null })).toBe(false)
    expect(secondFactorRequired({ details: 'totp_required' })).toBe(false)
  })

  it('口令错误（没有 details）不会被说成需要验证码', () => {
    const wrongPassword: Partial<ApiError> = { code: 'UNAUTHENTICATED', status: 401 }
    expect(secondFactorRequired(wrongPassword)).toBe(false)
  })
})
