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
    // 进程列表属于“服务与进程”模块：meta.moduleKey 指回 services，
    // 导航高亮与页面标题都据此取。
    path: '/processes',
    name: 'processes',
    component: () => import('@/views/ProcessesView.vue'),
    meta: { moduleKey: 'services' },
  },
  {
    path: '/logs',
    name: 'logs',
    component: () => import('@/views/LogsView.vue'),
    meta: { moduleKey: 'logs' },
  },
  {
    // 文件页只管理服务端登记的根；根与路径都是查询参数，不进 URL 路径段，
    // 因此这里没有 :id 之类的动态段——页面内部用相对路径导航。
    path: '/files',
    name: 'files',
    component: () => import('@/views/FilesView.vue'),
    meta: { moduleKey: 'files' },
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
