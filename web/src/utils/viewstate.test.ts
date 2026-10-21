// 页面状态映射的测试。
//
// 这些判定决定“用户看到的是‘没权限’还是‘服务坏了’”，错一次就会把人带到
// 错误的处理路径上（去申请权限 vs 去重启服务）。

import { describe, expect, it } from 'vitest'

import { SERVER_ERROR_CODES, type ApiErrorCode } from '@/api/types'
import { apiPanelState, retryablePanelState, sectionPanelState } from './viewstate'

describe('apiPanelState', () => {
  it('鉴权类错误判为无权限', () => {
    for (const code of ['UNAUTHENTICATED', 'FORBIDDEN', 'REAUTH_REQUIRED', 'CSRF_FAILED'] as const) {
      expect(apiPanelState({ code }), code).toBe('forbidden')
    }
  })

  it('代理不可用与依赖缺失判为依赖未安装', () => {
    expect(apiPanelState({ code: 'AGENT_UNAVAILABLE' })).toBe('dependency_missing')
    expect(apiPanelState({ code: 'DEPENDENCY_MISSING' })).toBe('dependency_missing')
  })

  it('未实现与设备不支持判为数据不支持', () => {
    expect(apiPanelState({ code: 'NOT_IMPLEMENTED' })).toBe('unsupported')
    expect(apiPanelState({ code: 'UNSUPPORTED_DEVICE' })).toBe('unsupported')
  })

  it('其余错误码一律判为请求失败，包括客户端传输故障', () => {
    const others: ApiErrorCode[] = [
      ...SERVER_ERROR_CODES.filter(
        (code) =>
          ![
            'UNAUTHENTICATED',
            'FORBIDDEN',
            'REAUTH_REQUIRED',
            'CSRF_FAILED',
            'AGENT_UNAVAILABLE',
            'DEPENDENCY_MISSING',
            'NOT_IMPLEMENTED',
            'UNSUPPORTED_DEVICE',
          ].includes(code),
      ),
      'NETWORK_ERROR',
      'INVALID_RESPONSE',
      'ABORTED',
    ]
    expect(others.length).toBeGreaterThan(10)
    for (const code of others) {
      expect(apiPanelState({ code }), code).toBe('error')
    }
  })
})

describe('sectionPanelState', () => {
  it('ok 不产生状态面板', () => {
    expect(sectionPanelState('ok')).toBeNull()
  })

  it('error 与 unavailable 分开：一个是坏了，一个是这台机器没有', () => {
    expect(sectionPanelState('error')).toBe('error')
    expect(sectionPanelState('unavailable')).toBe('unsupported')
  })
})

describe('retryablePanelState', () => {
  it('只有请求失败与依赖缺失给重试', () => {
    expect(retryablePanelState('error')).toBe(true)
    expect(retryablePanelState('dependency_missing')).toBe(true)
    expect(retryablePanelState('forbidden')).toBe(false)
    expect(retryablePanelState('unsupported')).toBe(false)
  })
})
