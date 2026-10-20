import { describe, expect, it } from 'vitest'

import { MODULES, visibleModules } from '@/modules/registry'
import { appRoutes, moduleTitle } from './routes'

describe('路由表', () => {
  it('每个可见模块都有真实登记的路由，且 meta 指回模块键', () => {
    const byPath = new Map(appRoutes.map((route) => [route.path, route]))
    for (const module of visibleModules()) {
      const route = byPath.get(module.path)
      expect(route, `可见模块 ${module.key} 缺少路由 ${module.path}`).toBeDefined()
      expect(route?.meta?.moduleKey).toBe(module.key)
    }
  })

  it('未实现模块没有路由：直接访问会落到 404，而不是空白页', () => {
    const registered = new Set(appRoutes.map((route) => route.path))
    for (const module of MODULES) {
      if (module.implementedIn !== null) continue
      expect(registered.has(module.path), `未实现模块 ${module.key} 不应登记路由`).toBe(false)
    }
  })

  it('兜底路由存在且排在最后，不会被前面的路径抢先命中', () => {
    const last = appRoutes.at(-1)
    expect(last?.path).toBe('/:pathMatch(.*)*')
    expect(last?.name).toBe('not-found')
  })

  it('moduleTitle 只翻译已知模块键', () => {
    expect(moduleTitle('overview')).toBe('概览')
    expect(moduleTitle('settings')).toBe('设置')
    expect(moduleTitle('nope')).toBeNull()
    expect(moduleTitle(null)).toBeNull()
    expect(moduleTitle(42)).toBeNull()
  })
})
