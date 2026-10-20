// 登录回跳地址的白名单测试。
//
// `?redirect=` 是用户输入。这里只证明一件事：它永远不会把人带出本机站点，
// 也不会在登录成功后回到登录页。

import { describe, expect, it } from 'vitest'

import { safeRedirect } from './redirect'

describe('safeRedirect', () => {
  it('站内路径原样保留', () => {
    expect(safeRedirect('/jobs')).toBe('/jobs')
    expect(safeRedirect('/jobs?x=1')).toBe('/jobs?x=1')
  })

  it('缺失或非字符串回到首页', () => {
    expect(safeRedirect(undefined)).toBe('/')
    expect(safeRedirect(null)).toBe('/')
    expect(safeRedirect(['/jobs'])).toBe('/')
    expect(safeRedirect(42)).toBe('/')
  })

  it('拒绝离站与协议相对地址', () => {
    expect(safeRedirect('https://evil.example/')).toBe('/')
    expect(safeRedirect('//evil.example/')).toBe('/')
    expect(safeRedirect('/\\evil.example')).toBe('/')
    expect(safeRedirect('javascript:alert(1)')).toBe('/')
  })

  it('拒绝回到登录页（否则登录成功又看到登录表单）', () => {
    expect(safeRedirect('/login')).toBe('/')
    expect(safeRedirect('/login?redirect=/jobs')).toBe('/')
  })
})
