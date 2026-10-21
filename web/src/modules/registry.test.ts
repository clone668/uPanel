import { describe, expect, it } from 'vitest'

import { MODULES, findModule, visibleModules } from './registry'

// PLAN.md 第 7 节固定的 16 项导航，顺序不可改。
const PLAN_LABELS = [
  '概览',
  '应用',
  '存储',
  '共享',
  '容器',
  '服务与进程',
  '文件',
  '终端',
  '日志',
  '备份',
  '计划任务',
  '网络',
  '更新',
  '监控告警',
  '任务中心',
  '设置',
]

describe('模块清单', () => {
  it('包含且仅包含 PLAN 第 7 节的 16 项，顺序一致', () => {
    expect(MODULES).toHaveLength(PLAN_LABELS.length)
    expect(MODULES.map((module) => module.label)).toEqual(PLAN_LABELS)
  })

  it('key 与 path 都是唯一的，避免菜单标记和路由互相覆盖', () => {
    expect(new Set(MODULES.map((m) => m.key)).size).toBe(MODULES.length)
    expect(new Set(MODULES.map((m) => m.path)).size).toBe(MODULES.length)
  })

  it('导航只包含已实现的模块（当前：概览、存储、任务中心）', () => {
    const visible = visibleModules()
    // 这份清单每实现一个模块就多一项：它是“界面上现在有什么”的唯一凭证。
    expect(visible.map((m) => m.key)).toEqual(['overview', 'storage', 'jobs'])
    expect(visible.every((m) => m.implementedIn !== null)).toBe(true)
  })

  it('已实现模块标注的阶段号形如 P03.3，未实现模块保持 null', () => {
    const implemented = new Set(['overview', 'storage', 'jobs'])
    for (const module of MODULES) {
      if (implemented.has(module.key)) {
        expect(module.implementedIn, `模块 ${module.key} 应标注阶段`).toMatch(/^P\d{2}\.\d$/)
        continue
      }
      expect(module.implementedIn, `模块 ${module.key} 尚未实现，不应标注阶段`).toBeNull()
    }
  })

  it('每个模块都有一句话说明，不留下无文案的空页面', () => {
    for (const module of MODULES) {
      expect(module.summary.length, `模块 ${module.key} 缺少说明`).toBeGreaterThan(0)
    }
  })

  it('findModule 按 key 命中，未知 key 返回 undefined', () => {
    expect(findModule('storage')?.label).toBe('存储')
    expect(findModule('nope')).toBeUndefined()
  })

  it('可见模块的 path 都以 / 开头（便于 createWebHistory 解析）', () => {
    for (const module of visibleModules()) {
      expect(module.path.startsWith('/')).toBe(true)
    }
  })
})
