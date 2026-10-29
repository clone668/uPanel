// 导航判定：当前页面属于哪个模块。
//
// 这段规则以前写在 AppShell 里，现在抽成纯函数，因为它决定**哪些页面从哪儿进去、
// 高亮哪一格**，是最该被单测钉住的一段（node 环境没有 DOM，组件本身测不了）。
//
// 规则（顺序不能反）：
//   1. 先看路由 meta.moduleKey。一个模块可以有多个页面：
//      /processes 属于"服务与进程"、/accounts 属于"共享"、/apps/deployments 属于"应用"、
//      /settings/host 属于"设置"（DECISIONS Q475、Q495）。
//      按路径前缀匹配会让第二个页面在整个导航里都不高亮，所以 meta 优先。
//   2. meta 不是已知模块键时，退回路径精确/前缀匹配。
//   3. 都不中就是 null：404 页面不高亮任何一格，也不猜。

import { visibleModules, type ModuleDef } from '@/modules/registry'

export function resolveActiveModule(
  path: string,
  moduleKey: unknown,
  modules: readonly ModuleDef[] = visibleModules(),
): ModuleDef | null {
  if (typeof moduleKey === 'string') {
    const byModule = modules.find((module) => module.key === moduleKey)
    if (byModule !== undefined) return byModule
  }
  const hit = modules.find(
    (module) => path === module.path || path.startsWith(`${module.path}/`),
  )
  return hit ?? null
}

/** 当前页面该显示的标题：模块中文名；不属于任何模块时用给定的兜底文案。 */
export function resolveActiveLabel(
  path: string,
  moduleKey: unknown,
  fallback = '未找到页面',
  modules: readonly ModuleDef[] = visibleModules(),
): string {
  return resolveActiveModule(path, moduleKey, modules)?.label ?? fallback
}
