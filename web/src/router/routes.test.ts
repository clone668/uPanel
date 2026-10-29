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

  it('设备详情路由限定 id 字符集，不合法的 id 落到 404 而不是进页面', () => {
    const route = appRoutes.find((entry) => entry.name === 'storage-device')
    expect(route?.path).toBe('/storage/devices/:id([A-Za-z0-9._-]+)')
    expect(route?.meta?.moduleKey).toBe('storage')
  })

  it('服务单元路由限定字符集，带斜杠或空格的名字落到 404', () => {
    const route = appRoutes.find((entry) => entry.name === 'service-unit')
    expect(route?.path).toBe('/services/:unit([A-Za-z0-9][A-Za-z0-9:_.@-]*)')
    expect(route?.meta?.moduleKey).toBe('services')
  })

  it('受管部署页挂在应用模块下，且不去顶“容器”那一格', () => {
    const route = appRoutes.find((entry) => entry.name === 'deployments')
    expect(route?.path).toBe('/apps/deployments')
    expect(route?.meta?.moduleKey).toBe('apps')
    // “容器”模块仍未被实现：导航里那一格不能因为部署页而点亮。
    expect(MODULES.find((module) => module.key === 'containers')?.implementedIn).toBeNull()
  })

  it('moduleTitle 只翻译已知模块键', () => {
    expect(moduleTitle('overview')).toBe('概览')
    expect(moduleTitle('settings')).toBe('设置')
    expect(moduleTitle('nope')).toBeNull()
    expect(moduleTitle(null)).toBeNull()
    expect(moduleTitle(42)).toBeNull()
  })
})
