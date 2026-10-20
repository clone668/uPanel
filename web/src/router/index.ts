// 路由实例 + 登录闸门。
//
// 路由表在 routes.ts（纯数据，可在 node 环境单测），这里只负责创建实例与拦截。
// history 使用 createWebHistory：生产由 Go 服务同源提供 / 与静态资源，
// 深链接（/storage 等）在 P01.4 由服务端回落到 index.html；未知 **API** 路径
// 仍返回 JSON 404，两者互不影响（CONTRACTS.md 第 1 节 + P01 验收条款）。
//
// 闸门规则：
//   - meta.public 的页面（只有 /login）不需要会话；
//   - 其他页面必须先确认会话（GET /auth/session），未登录跳登录页并带回跳地址；
//   - 认证状态未知时**不猜**：先恢复再决定，避免刷新页面时闪一下登录页。

import { createRouter, createWebHistory } from 'vue-router'
import type { Pinia } from 'pinia'

import { useSessionStore } from '@/stores/session'
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

/**
 * 安装登录闸门。必须在 app.use(pinia) 之后调用：守卫里要按同一个 pinia 实例取 store。
 */
export function installAuthGuard(pinia: Pinia): void {
  router.beforeEach(async (to) => {
    const session = useSessionStore(pinia)
    if (session.status === 'unknown') {
      try {
        await session.restore()
      } catch {
        // 传输故障：保持未登录判定，由目标页面或登录页把真实错误显示出来。
      }
    }
    if (to.meta.public === true) {
      return session.isAuthenticated ? { path: '/' } : true
    }
    if (!session.isAuthenticated) {
      return { path: '/login', query: { redirect: to.fullPath } }
    }
    return true
  })
}
