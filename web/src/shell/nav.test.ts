import { describe, expect, it } from 'vitest'

import { MODULES, visibleModules } from '@/modules/registry'
import { resolveActiveLabel, resolveActiveModule } from './nav'

describe('resolveActiveModule：meta 优先', () => {
  it('模块自己的页面按 meta 命中', () => {
    expect(resolveActiveModule('/', 'overview')?.key).toBe('overview')
    expect(resolveActiveModule('/storage', 'storage')?.key).toBe('storage')
    expect(resolveActiveModule('/network', 'network')?.key).toBe('network')
  })

  it('同模块的第二个页面挂在同一个格子上（Q475 / Q495）', () => {
    // 进程列表归"服务与进程"，账号归"共享"，受管部署归"应用"，外部程序能力归"设置"。
    // 这些路径谁都不以自己的 key 开头，只能靠 meta——按前缀匹配会让整页不高亮。
    expect(resolveActiveModule('/processes', 'services')?.key).toBe('services')
    expect(resolveActiveModule('/accounts', 'shares')?.key).toBe('shares')
    expect(resolveActiveModule('/apps/deployments', 'apps')?.key).toBe('apps')
    expect(resolveActiveModule('/settings/host', 'settings')?.key).toBe('settings')
  })

  it('meta 中指回本模块时，仍取该模块（不是按路径另找）', () => {
    expect(resolveActiveModule('/settings/host', 'settings')?.path).toBe('/settings')
  })
})

describe('resolveActiveModule：meta 不可用时退回路径', () => {
  it('动态段页面按前缀命中', () => {
    expect(resolveActiveModule('/storage/devices/sda', null)?.key).toBe('storage')
    expect(resolveActiveModule('/services/nginx.service', null)?.key).toBe('services')
  })

  it('未知路径不高亮任何一项', () => {
    expect(resolveActiveModule('/nowhere', null)).toBeNull()
    expect(resolveActiveModule('/nowhere', 'not-a-module')).toBeNull()
    // 登录页与 404 页的 meta.moduleKey 是 null。
    expect(resolveActiveModule('/login', null)).toBeNull()
  })

  it('未实现模块不算当前模块（导航里根本没有它）', () => {
    // 容器是 16 项里唯一 implementedIn 仍为 null 的一个：即使路由给了一个键也不高亮。
    expect(MODULES.find((module) => module.key === 'containers')?.implementedIn).toBeNull()
    expect(resolveActiveModule('/containers', 'containers')).toBeNull()
  })

  it('模块清单可以由调用方给（空表即全不命中）', () => {
    expect(resolveActiveModule('/storage', 'storage', [])).toBeNull()
  })
})

describe('resolveActiveLabel', () => {
  it('取模块中文名', () => {
    expect(resolveActiveLabel('/', 'overview')).toBe('概览')
    expect(resolveActiveLabel('/jobs', 'jobs')).toBe('任务中心')
  })

  it('不属于任何模块时用兜底文案', () => {
    expect(resolveActiveLabel('/nowhere', null)).toBe('未找到页面')
    expect(resolveActiveLabel('/nowhere', null, '随便')).toBe('随便')
  })

  it('文案与 registry 一致，不在这里另造一套中文名', () => {
    for (const module of visibleModules()) {
      expect(resolveActiveLabel(module.path, module.key)).toBe(module.label)
    }
  })
})
