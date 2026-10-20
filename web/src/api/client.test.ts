import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import {
  ApiError,
  API_BASE,
  get,
  request,
  setCsrfTokenProvider,
} from './client'

type FetchMock = ReturnType<typeof vi.fn>

function stubFetch(impl: (input: RequestInfo | URL, init?: RequestInit) => Promise<Response>): FetchMock {
  const mock = vi.fn(impl)
  vi.stubGlobal('fetch', mock)
  return mock
}

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json', 'X-Request-ID': 'header-rid' },
  })
}

function lastCall(mock: FetchMock): { url: string; init: RequestInit } {
  const call = mock.mock.calls.at(-1)
  if (!call) throw new Error('fetch 未被调用')
  return { url: String(call[0]), init: (call[1] ?? {}) as RequestInit }
}

function headersOf(init: RequestInit): Record<string, string> {
  return (init.headers ?? {}) as Record<string, string>
}

beforeEach(() => {
  setCsrfTokenProvider(() => null)
})

afterEach(() => {
  vi.unstubAllGlobals()
  vi.restoreAllMocks()
})

describe('request', () => {
  it('解包成功信封并返回 data 与 request_id', async () => {
    const mock = stubFetch(() => Promise.resolve(jsonResponse({ data: { status: 'ok' }, request_id: 'rid-1' })))

    const result = await get<{ status: string }>('/health/live')

    expect(result.data).toEqual({ status: 'ok' })
    expect(result.requestId).toBe('rid-1')
    const { url, init } = lastCall(mock)
    expect(url).toBe(`${API_BASE}/health/live`)
    expect(init.method).toBe('GET')
    expect(init.credentials).toBe('same-origin')
  })

  it('GET 不带 Content-Type 与 CSRF 头（避免简单请求被升级成预检）', async () => {
    const mock = stubFetch(() => Promise.resolve(jsonResponse({ data: 1, request_id: 'r' })))
    setCsrfTokenProvider(() => 'token-should-not-be-used')

    await get('/system/summary')

    const { init } = lastCall(mock)
    const headers = headersOf(init)
    expect(headers['Content-Type']).toBeUndefined()
    expect(headers['X-CSRF-Token']).toBeUndefined()
    expect(headers['X-UPanel-Request']).toBeUndefined()
    expect(init.body).toBeUndefined()
  })

  it('写请求自动带 JSON Content-Type、X-UPanel-Request、CSRF 与幂等键', async () => {
    const mock = stubFetch(() => Promise.resolve(jsonResponse({ data: { job_id: 'j1' }, request_id: 'r2' }, 202)))
    setCsrfTokenProvider(() => 'csrf-abc')

    const result = await request<{ job_id: string }>('/operations', {
      method: 'POST',
      body: { action: 'service.restart', target: 'nginx' },
      idempotencyKey: '8a1f-uuid',
    })

    expect(result.data).toEqual({ job_id: 'j1' })
    const { init } = lastCall(mock)
    const headers = headersOf(init)
    expect(init.method).toBe('POST')
    expect(headers['Content-Type']).toBe('application/json')
    expect(headers['X-UPanel-Request']).toBe('1')
    expect(headers['X-CSRF-Token']).toBe('csrf-abc')
    expect(headers['Idempotency-Key']).toBe('8a1f-uuid')
    expect(init.body).toBe(JSON.stringify({ action: 'service.restart', target: 'nginx' }))
  })

  it('没有 CSRF 令牌时不伪造该头（交给服务端返回 403）', async () => {
    const mock = stubFetch(() => Promise.resolve(jsonResponse({ data: null, request_id: 'r' })))
    await request('/auth/logout', { method: 'POST' })
    expect(headersOf(lastCall(mock).init)['X-CSRF-Token']).toBeUndefined()
  })

  it('序列化 query，跳过 null 与 undefined', async () => {
    const mock = stubFetch(() => Promise.resolve(jsonResponse({ data: [], request_id: 'r' })))
    await get('/audit', { query: { limit: 50, cursor: null, scope: 'jobs', archived: undefined } })
    expect(lastCall(mock).url).toBe(`${API_BASE}/audit?limit=50&scope=jobs`)
  })

  it('服务端错误信封转成 ApiError，保留 code/details/request_id/状态码', async () => {
    stubFetch(() =>
      Promise.resolve(
        jsonResponse(
          {
            error: {
              code: 'DEVICE_IN_USE',
              message: '分区正在使用，不能格式化',
              details: { blockers: [{ kind: 'mount', id: 'x', label: '/srv/data' }] },
            },
            request_id: 'rid-409',
          },
          409,
        ),
      ),
    )

    const error = await get('/storage/devices/dev1').then(
      () => null,
      (caught: unknown) => caught,
    )

    expect(error).toBeInstanceOf(ApiError)
    const apiError = error as ApiError
    expect(apiError.code).toBe('DEVICE_IN_USE')
    expect(apiError.message).toBe('分区正在使用，不能格式化')
    expect(apiError.status).toBe(409)
    expect(apiError.requestId).toBe('rid-409')
    expect(apiError.source).toBe('server')
    expect(apiError.details).toEqual({ blockers: [{ kind: 'mount', id: 'x', label: '/srv/data' }] })
  })

  it('反向代理返回 HTML 时判为 INVALID_RESPONSE，不猜业务原因', async () => {
    stubFetch(() => Promise.resolve(new Response('<html>502 Bad Gateway</html>', { status: 502 })))

    const error = (await get('/system/summary').catch((caught: unknown) => caught)) as ApiError

    expect(error).toBeInstanceOf(ApiError)
    expect(error.code).toBe('INVALID_RESPONSE')
    expect(error.source).toBe('client')
    expect(error.status).toBe(502)
  })

  it('成功响应缺少 data 或 request_id 时判为 INVALID_RESPONSE', async () => {
    stubFetch(() => Promise.resolve(jsonResponse({ request_id: 'r' })))
    const missingData = (await get('/x').catch((caught: unknown) => caught)) as ApiError
    expect(missingData.code).toBe('INVALID_RESPONSE')

    stubFetch(() => Promise.resolve(jsonResponse({ data: { ok: true } })))
    const missingRid = (await get('/x').catch((caught: unknown) => caught)) as ApiError
    expect(missingRid.code).toBe('INVALID_RESPONSE')
  })

  it('网络异常转成 NETWORK_ERROR，并带上原因', async () => {
    stubFetch(() => Promise.reject(new TypeError('fetch failed')))

    const error = (await get('/health/live').catch((caught: unknown) => caught)) as ApiError

    expect(error.code).toBe('NETWORK_ERROR')
    expect(error.source).toBe('client')
    expect(error.message).toContain('fetch failed')
  })

  it('调用方取消请求时抛 ABORTED', async () => {
    const controller = new AbortController()
    stubFetch(() => {
      controller.abort()
      return Promise.reject(Object.assign(new Error('aborted'), { name: 'AbortError' }))
    })

    const error = (await get('/health/live', { signal: controller.signal }).catch(
      (caught: unknown) => caught,
    )) as ApiError

    expect(error.code).toBe('ABORTED')
  })

  it('拒绝给 GET 携带请求体（接线错误要在本地就报，不要发出去）', async () => {
    const mock = stubFetch(() => Promise.resolve(jsonResponse({ data: 1, request_id: 'r' })))

    const error = (await request('/x', { method: 'GET', body: { a: 1 } }).catch(
      (caught: unknown) => caught,
    )) as ApiError

    expect(error.code).toBe('INVALID_RESPONSE')
    expect(mock).not.toHaveBeenCalled()
  })

  it('API 前缀默认同源 /api/v1（生产与开发一致，不做跨域）', () => {
    expect(API_BASE).toBe('/api/v1')
  })
})
