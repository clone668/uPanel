// 路由表（纯数据）。
//
// 只登记**已实现**的模块页面（module.implementedIn !== null）。
// 未实现模块不进路由表：直接访问其路径会落到 404 页面，而不是一个空白或假成功的页面。

import type { RouteRecordRaw } from 'vue-router'

import { findModule } from '@/modules/registry'

export const appRoutes: readonly RouteRecordRaw[] = [
  {
    path: '/',
    name: 'overview',
    component: () => import('@/views/OverviewView.vue'),
    meta: { moduleKey: 'overview' },
  },
  {
    path: '/:pathMatch(.*)*',
    name: 'not-found',
    component: () => import('@/views/NotFoundView.vue'),
    meta: { moduleKey: null },
  },
]

/** 由路由 meta 里的模块键取中文标题；未知键返回 null。 */
export function moduleTitle(moduleKey: unknown): string | null {
  if (typeof moduleKey !== 'string') return null
  return findModule(moduleKey)?.label ?? null
}
