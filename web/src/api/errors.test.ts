import { describe, expect, it } from 'vitest'

import { ApiError } from './client'
import { describeApiError } from './errors'

function serverError(code: ApiError['code'], status = 400): ApiError {
  return new ApiError({ code, message: 'x', status, requestId: 'rid', source: 'server' })
}

function clientError(code: ApiError['code']): ApiError {
  return new ApiError({ code, message: 'x', status: 0, requestId: '', source: 'client' })
}

describe('describeApiError', () => {
  it('按 code 分支：401 判定为需要重新登录且不可重试', () => {
    const view = describeApiError(serverError('UNAUTHENTICATED', 401))
    expect(view.needsLogin).toBe(true)
    expect(view.canRetry).toBe(false)
    expect(view.title).toContain('会话')
  })

  it('403/404/409 不提供盲目重试', () => {
    for (const code of ['FORBIDDEN', 'NOT_FOUND', 'VERSION_CONFLICT'] as const) {
      expect(describeApiError(serverError(code)).canRetry).toBe(false)
    }
  })

  it('503 代理不可用允许重试，并说明只读功能不受影响', () => {
    const view = describeApiError(serverError('AGENT_UNAVAILABLE', 503))
    expect(view.canRetry).toBe(true)
    expect(view.hint).toContain('只读')
  })

  it('客户端网络故障与服务端错误分开描述', () => {
    const view = describeApiError(clientError('NETWORK_ERROR'))
    expect(view.title).toContain('无法连接')
    expect(view.canRetry).toBe(true)
  })

  it('未知服务端错误码不隐藏，按 5xx 判定是否可重试', () => {
    const view = describeApiError(serverError('COMMAND_FAILED', 500))
    expect(view.title).toContain('请求失败')
    expect(view.canRetry).toBe(true)
    expect(describeApiError(serverError('COMMAND_FAILED', 422)).canRetry).toBe(false)
  })
})
