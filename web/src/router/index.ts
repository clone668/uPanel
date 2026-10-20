// 路由实例。
//
// 路由表在 routes.ts（纯数据，可在 node 环境单测），这里只负责创建实例。
// history 使用 createWebHistory：生产由 Go 服务同源提供 / 与静态资源，
// 深链接（/storage 等）在 P01.4 由服务端回落到 index.html；未知 **API** 路径
// 仍返回 JSON 404，两者互不影响（CONTRACTS.md 第 1 节 + P01 验收条款）。

import { createRouter, createWebHistory } from 'vue-router'

import { appRoutes, moduleTitle } from './routes'

export const router = createRouter({
  history: createWebHistory(),
  routes: [...appRoutes],
  scrollBehavior() {
    return { top: 0 }
  },
})

router.afterEach((to) => {
  const label = moduleTitle(to.meta.moduleKey)
  if (typeof document !== 'undefined') {
    document.title = label ? `${label} · uPanel` : 'uPanel'
  }
})
