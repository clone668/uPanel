// 路由表（纯数据）。
//
// 只登记**已实现**的模块页面（module.implementedIn !== null）。
// 未实现模块不进路由表：直接访问其路径会落到 404 页面，而不是一个空白或假成功的页面。

import type { RouteRecordRaw } from 'vue-router'

import { findModule } from '@/modules/registry'

export const appRoutes: readonly RouteRecordRaw[] = [
  {
    path: '/login',
    name: 'login',
    component: () => import('@/views/LoginView.vue'),
    // 登录页不属于任何导航模块：它没有侧栏高亮，也不需要登录。
    meta: { moduleKey: null, public: true },
  },
  {
    path: '/',
    name: 'overview',
    component: () => import('@/views/OverviewView.vue'),
    meta: { moduleKey: 'overview' },
  },
  {
    path: '/storage',
    name: 'storage',
    component: () => import('@/views/StorageView.vue'),
    meta: { moduleKey: 'storage' },
  },
  {
    // 设备 id 是内核设备名，只允许契约里的字符集；非法值直接落 404，
    // 不会把一个奇怪的参数带进页面再让它自己出错。
    path: '/storage/devices/:id([A-Za-z0-9._-]+)',
    name: 'storage-device',
    component: () => import('@/views/StorageDeviceView.vue'),
    meta: { moduleKey: 'storage' },
  },
  {
    path: '/services',
    name: 'services',
    component: () => import('@/views/ServicesView.vue'),
    meta: { moduleKey: 'services' },
  },
  {
    // 单元名限定在契约允许的字符集内：奇怪的名字直接落 404，
    // 不会把一个带斜杠或空格的参数带进页面再让它自己出错。
    path: '/services/:unit([A-Za-z0-9][A-Za-z0-9:_.@-]*)',
    name: 'service-unit',
    component: () => import('@/views/ServiceUnitView.vue'),
    meta: { moduleKey: 'services' },
  },
  {
    path: '/jobs',
    name: 'jobs',
    component: () => import('@/views/JobsView.vue'),
    meta: { moduleKey: 'jobs' },
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
