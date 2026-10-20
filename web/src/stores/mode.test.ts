import { describe, expect, it } from 'vitest'

import { normalizeMode, readInjectedMode, resolveMode } from './mode'

describe('运行模式', () => {
  it('只接受两个已知字面值，其余一律判为未知', () => {
    expect(normalizeMode('mock')).toBe('mock')
    expect(normalizeMode('production')).toBe('production')
    for (const value of ['', 'Mock', 'mock ', 'demo', 'prod', null, undefined, 1, {}, []]) {
      expect(normalizeMode(value)).toBeNull()
    }
  })

  it('服务端注入值优先于构建期变量', () => {
    expect(resolveMode({ injected: 'mock', fromEnv: 'production' })).toBe('mock')
    expect(resolveMode({ injected: 'production', fromEnv: 'mock' })).toBe('production')
  })

  it('注入值非法时不报错，回落到构建期变量', () => {
    expect(resolveMode({ injected: 'preview', fromEnv: 'mock' })).toBe('mock')
    expect(resolveMode({ injected: null, fromEnv: 'mock' })).toBe('mock')
  })

  it('两个来源都无法判定时兜底 production', () => {
    expect(resolveMode({})).toBe('production')
    expect(resolveMode({ injected: 'x', fromEnv: 'y' })).toBe('production')
  })

  it('非浏览器环境（单测 / SSR）读不到注入值，不抛异常', () => {
    expect(typeof window).toBe('undefined')
    expect(readInjectedMode()).toBeUndefined()
  })
})
