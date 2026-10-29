import { describe, expect, it } from 'vitest'

import { visibleModules } from '@/modules/registry'
import { MODULE_ICONS, moduleIconPath } from './icons'

describe('模块图标覆盖', () => {
  it('每个会出现在导航里的模块都有图标', () => {
    for (const module of visibleModules()) {
      expect(MODULE_ICONS[module.key], `缺少图标：${module.key}`).toBeTruthy()
    }
  })

  it('图标是一条能画的路径（以 M 开头、非空）', () => {
    for (const [key, path] of Object.entries(MODULE_ICONS)) {
      expect(path.startsWith('M'), `路径没有起点：${key}`).toBe(true)
      expect(path.length).toBeGreaterThan(8)
    }
  })

  it('可见模块的图标两两不同（复制粘贴不会静默通过）', () => {
    const paths = visibleModules().map((module) => moduleIconPath(module.key))
    expect(new Set(paths).size).toBe(paths.length)
  })

  it('未知模块退回中性方块，不渲染成空白', () => {
    const fallback = moduleIconPath('no-such-module')
    expect(fallback.startsWith('M')).toBe(true)
    expect(fallback).not.toBe(moduleIconPath('overview'))
  })
})
