// 服务端结果 → 页面状态 的唯一映射。
//
// STAGES/PLAN 要求每个页面都能表达加载、空数据、无权限、依赖未安装、数据不支持、
// 请求失败、部分数据可用七种状态。三个视图如果各自写一套 if-else，迟早会分叉：
// 同一个 AGENT_UNAVAILABLE 在一页说“依赖缺失”、另一页说“请求失败”。
//
// 这里只做判定，不涉及渲染；文案由 StatePanel 负责。

import type { ApiErrorCode } from '@/api/types'
import type { SystemSectionStatus } from '@/api/system'

/** StatePanel 能表达的“非正常”状态子集（加载/空数据由页面自己决定）。 */
export type PanelLikeState = 'error' | 'forbidden' | 'dependency_missing' | 'unsupported'

/**
 * API 错误 → 页面状态。
 *
 * 判据只用错误码（CONTRACTS 第 1 节的封闭枚举），不看 message：
 * message 是给人读的，改文案不该改变页面行为。
 */
export function apiPanelState(error: { code: ApiErrorCode }): PanelLikeState {
  switch (error.code) {
    case 'UNAUTHENTICATED':
    case 'FORBIDDEN':
    case 'REAUTH_REQUIRED':
    case 'CSRF_FAILED':
      return 'forbidden'
    case 'AGENT_UNAVAILABLE':
    case 'DEPENDENCY_MISSING':
      return 'dependency_missing'
    case 'NOT_IMPLEMENTED':
    case 'UNSUPPORTED_DEVICE':
      return 'unsupported'
    default:
      return 'error'
  }
}

/** 采集段状态 → 页面状态。unavailable 表示这台机器上没有这个数据源。 */
export function sectionPanelState(status: SystemSectionStatus): PanelLikeState | null {
  if (status === 'ok') return null
  return status === 'error' ? 'error' : 'unsupported'
}

/** 这份错误是否值得给“重试”（依赖缺失、传输失败可以；无权限不行）。 */
export function retryablePanelState(state: PanelLikeState): boolean {
  return state === 'error' || state === 'dependency_missing'
}
