// 运行模式（production / mock）。
//
// 为什么要一个 store：mock 模式下页面必须**持续**显示“演示数据，未操作服务器”
// （CONTRACTS.md 第 3 节）。横幅不能靠用户手动忽略，也不能因为某个页面忘了加就消失，
// 所以由 App.vue 挂在布局最上方，一次性读取这里的值。
//
// 模式来源（按优先级）：
//   1. 服务端注入的 window.__UPANEL_MODE__（P01.4 嵌入前端时写入）；
//   2. 构建/开发期的 VITE_UPANEL_MODE（本地跑 mock 服务时用）；
//   3. 兜底 production。
//
// 已知缺口（P02 修复）：目前没有公开接口返回服务端模式（GET /meta 属 P02，需要登录），
// 因此“服务端是 mock、前端却拿到 production”在 P01.3 无法自动发现。
// 见 docs/PROGRESS.md 的未验证项与 docs/DECISIONS.md Q21。

import { computed, ref } from 'vue'
import { defineStore } from 'pinia'

export type AppMode = 'production' | 'mock'

/** 只认识两个字面值；其他输入（拼错、空串、非字符串）一律返回 null，不做猜测。 */
export function normalizeMode(value: unknown): AppMode | null {
  if (value === 'mock') return 'mock'
  if (value === 'production') return 'production'
  return null
}

/**
 * 纯函数：按优先级决定模式。便于单测，不依赖 window。
 */
export function resolveMode(sources: {
  injected?: unknown
  fromEnv?: unknown
}): AppMode {
  return normalizeMode(sources.injected) ?? normalizeMode(sources.fromEnv) ?? 'production'
}

/** 读取服务端注入值；非浏览器环境（单测、SSR）返回 undefined。 */
export function readInjectedMode(): unknown {
  if (typeof window === 'undefined') return undefined
  return window.__UPANEL_MODE__
}

export const useModeStore = defineStore('mode', () => {
  const mode = ref<AppMode>(
    resolveMode({ injected: readInjectedMode(), fromEnv: import.meta.env.VITE_UPANEL_MODE }),
  )

  const isMock = computed(() => mode.value === 'mock')

  return { mode, isMock }
})
