// 统一 API 客户端：所有页面只能通过这里访问后端。
//
// 契约来源 CONTRACTS.md 第 1、2 节：
//   - 同源 /api/v1，cookie 随请求发送（credentials: same-origin），不开放跨域；
//   - JSON、snake_case；
//   - 响应总是 {data, request_id} 或 {error, request_id}；
//   - 已登录的非 GET/HEAD 请求带 X-CSRF-Token（令牌只存内存，由认证模块提供）；
//   - 登录等接口要求 X-UPanel-Request: 1，写请求带 Idempotency-Key。
//
// 这里不做任何“自动重试”：写操作是否幂等由服务端任务与幂等键决定，
// 客户端盲目重试会制造重复任务（PLAN.md 第 6 节）。

import type {
  ApiErrorCode,
  ClientErrorCode,
  DataEnvelope,
  ErrorEnvelope,
  ServerErrorCode,
} from './types'
import { isServerErrorCode } from './types'

export const API_BASE: string = import.meta.env.VITE_UPANEL_API_BASE ?? '/api/v1'

/** 默认请求超时。具体接口可以在调用处收紧。 */
export const DEFAULT_TIMEOUT_MS = 15_000

export type HttpMethod = 'GET' | 'HEAD' | 'POST' | 'PATCH' | 'PUT' | 'DELETE'

export type QueryValue = string | number | boolean | null | undefined

export interface RequestOptions {
  method?: HttpMethod
  /** JSON 请求体。GET/HEAD 不允许带 body。 */
  body?: unknown
  query?: Record<string, QueryValue>
  signal?: AbortSignal
  timeoutMs?: number
  /** 写操作的幂等键（UUID），同一次提交意图重试时复用。 */
  idempotencyKey?: string
  /** 额外请求头，仅用于契约里明确的头（例如 X-UPanel-Request）。 */
  headers?: Record<string, string>
}

export interface ApiResult<T> {
  data: T
  requestId: string
}

/** 统一的 API 错误：页面只按 code 分支，message 只用于展示。 */
export class ApiError extends Error {
  readonly code: ApiErrorCode
  readonly status: number
  readonly details: unknown
  readonly requestId: string
  /** 服务端返回的错误码，还是客户端判决的传输层故障。 */
  readonly source: 'server' | 'client'

  constructor(init: {
    code: ApiErrorCode
    message: string
    status: number
    requestId: string
    details?: unknown
    source: 'server' | 'client'
  }) {
    super(init.message)
    this.name = 'ApiError'
    this.code = init.code
    this.status = init.status
    this.requestId = init.requestId
    this.details = init.details ?? null
    this.source = init.source
  }

  /** 是否需要用户重新登录（P02 起由会话中间件统一处理，这里只保留判定）。 */
  get needsLogin(): boolean {
    return this.code === 'UNAUTHENTICATED'
  }
}

/**
 * CSRF 令牌来源。认证模块（P02）通过 setCsrfTokenProvider 注入；
 * 未注入时写请求不带该头，由服务端返回 403 CSRF_FAILED —— 不会静默成功。
 */
let csrfTokenProvider: () => string | null = () => null

export function setCsrfTokenProvider(provider: () => string | null): void {
  csrfTokenProvider = provider
}

function buildUrl(path: string, query?: Record<string, QueryValue>): string {
  const normalized = path.startsWith('/') ? path : `/${path}`
  const url = `${API_BASE}${normalized}`
  if (!query) return url

  const params = new URLSearchParams()
  for (const [key, value] of Object.entries(query)) {
    if (value === null || value === undefined) continue
    params.append(key, String(value))
  }
  const qs = params.toString()
  return qs === '' ? url : `${url}?${qs}`
}

function isBodyless(method: HttpMethod): boolean {
  return method === 'GET' || method === 'HEAD'
}

function clientError(
  code: ClientErrorCode,
  message: string,
  options: { status?: number; requestId?: string } = {},
): ApiError {
  return new ApiError({
    code,
    message,
    status: options.status ?? 0,
    requestId: options.requestId ?? '',
    source: 'client',
  })
}

async function readJson(response: Response): Promise<unknown | undefined> {
  const text = await response.text()
  if (text.trim() === '') return undefined
  try {
    return JSON.parse(text) as unknown
  } catch {
    return undefined
  }
}

function pickRequestId(payload: unknown): string | undefined {
  if (payload === null || typeof payload !== 'object') return undefined
  const value = (payload as { request_id?: unknown }).request_id
  return typeof value === 'string' ? value : undefined
}

/**
 * 发送一次 API 请求并解包信封。
 *
 * 成功：返回 { data, requestId }。
 * 失败：抛 ApiError（服务端错误码或客户端传输层故障），永不返回半截数据。
 */
export async function request<T>(path: string, options: RequestOptions = {}): Promise<ApiResult<T>> {
  const method = options.method ?? 'GET'
  const timeoutMs = options.timeoutMs ?? DEFAULT_TIMEOUT_MS

  if (isBodyless(method) && options.body !== undefined) {
    throw clientError('INVALID_RESPONSE', `内部错误：${method} 请求不能携带请求体`)
  }

  const headers: Record<string, string> = {
    Accept: 'application/json',
    ...options.headers,
  }

  if (!isBodyless(method)) {
    // 契约要求：非 GET/HEAD 请求必须被服务端识别为“面板发起的 JSON 请求”。
    headers['Content-Type'] = 'application/json'
    headers['X-UPanel-Request'] = headers['X-UPanel-Request'] ?? '1'
    const token = csrfTokenProvider()
    if (token) headers['X-CSRF-Token'] = token
    if (options.idempotencyKey) headers['Idempotency-Key'] = options.idempotencyKey
  }

  const timeoutSignal = AbortSignal.timeout(timeoutMs)
  const signal = options.signal ? AbortSignal.any([options.signal, timeoutSignal]) : timeoutSignal

  let response: Response
  try {
    response = await fetch(buildUrl(path, options.query), {
      method,
      headers,
      credentials: 'same-origin',
      signal,
      ...(options.body === undefined ? {} : { body: JSON.stringify(options.body) }),
    })
  } catch (error) {
    if (options.signal?.aborted) {
      throw clientError('ABORTED', '请求已取消')
    }
    if (timeoutSignal.aborted) {
      throw clientError('NETWORK_ERROR', `请求超时（${Math.round(timeoutMs / 1000)} 秒未响应）`)
    }
    const reason = error instanceof Error ? error.message : String(error)
    throw clientError('NETWORK_ERROR', `无法连接面板服务：${reason}`)
  }

  const payload = await readJson(response)
  const requestId = pickRequestId(payload) ?? response.headers.get('X-Request-ID') ?? ''

  if (!response.ok) {
    const envelope = payload as Partial<ErrorEnvelope> | undefined
    const body = envelope?.error
    if (body && typeof body.code === 'string' && isServerErrorCode(body.code)) {
      throw new ApiError({
        code: body.code as ServerErrorCode,
        message: typeof body.message === 'string' && body.message !== '' ? body.message : '请求失败',
        status: response.status,
        requestId,
        details: body.details,
        source: 'server',
      })
    }
    // 非契约响应（反向代理错误页、被截断的响应）：如实报告，不猜业务原因。
    throw clientError('INVALID_RESPONSE', `服务返回了不符合契约的响应（HTTP ${response.status}）`, {
      status: response.status,
      requestId,
    })
  }

  const envelope = payload as Partial<DataEnvelope<T>> | undefined
  if (!envelope || typeof envelope !== 'object' || !('data' in envelope)) {
    throw clientError('INVALID_RESPONSE', '服务返回了不符合契约的成功响应（缺少 data）', {
      status: response.status,
      requestId,
    })
  }
  if (typeof envelope.request_id !== 'string' || envelope.request_id === '') {
    throw clientError('INVALID_RESPONSE', '服务返回的成功响应缺少 request_id', {
      status: response.status,
      requestId,
    })
  }

  return { data: envelope.data as T, requestId: envelope.request_id }
}

/** GET 的便捷封装。 */
export function get<T>(
  path: string,
  options: Omit<RequestOptions, 'method' | 'body'> = {},
): Promise<ApiResult<T>> {
  return request<T>(path, { ...options, method: 'GET' })
}
