// 登录回跳地址的白名单测试。
//
// `?redirect=` 是用户输入。这里只证明一件事：它永远不会把人带出本机站点，
// 也不会在登录成功后回到登录页。兜底目标是桌面 /desk（Q533）：
// 桌面式外壳下登录完应该落在摆着应用的桌上，不是被直接推进某一页数据。

import { describe, expect, it } from 'vitest'

import { HOME_PATH, safeRedirect } from './redirect'

describe('safeRedirect', () => {
  it('站内路径原样保留', () => {
    expect(safeRedirect('/jobs')).toBe('/jobs')
    expect(safeRedirect('/jobs?x=1')).toBe('/jobs?x=1')
  })

  it('缺失或非字符串回到桌面', () => {
    expect(HOME_PATH).toBe('/desk')
    expect(safeRedirect(undefined)).toBe(HOME_PATH)
    expect(safeRedirect(null)).toBe(HOME_PATH)
    expect(safeRedirect(['/jobs'])).toBe(HOME_PATH)
    expect(safeRedirect(42)).toBe(HOME_PATH)
  })

  it('拒绝离站与协议相对地址', () => {
    expect(safeRedirect('https://evil.example/')).toBe(HOME_PATH)
    expect(safeRedirect('//evil.example/')).toBe(HOME_PATH)
    expect(safeRedirect('/\\evil.example')).toBe(HOME_PATH)
    expect(safeRedirect('javascript:alert(1)')).toBe(HOME_PATH)
  })

  it('拒绝回到登录页（否则登录成功又看到登录表单）', () => {
    expect(safeRedirect('/login')).toBe(HOME_PATH)
    expect(safeRedirect('/login?redirect=/jobs')).toBe(HOME_PATH)
  })
})
