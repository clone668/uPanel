// 告警规则、事件与维护静默（P13.2 的接口，P13.4 的页面）。
//
// 与 api/openapi.yaml 的 AlertRule / AlertEvent / AlertSilence 逐字对应。
//
// 三条口径在这里就定死，页面不再自行发挥：
//
//   1) **没有数据不是 0。** `last_value` 为 null 表示这一轮没读到数据，不是"值是 0"；
//      `peak_value` 为 null 表示整个事件区间里一个样本都没有。两者都不能画成 0。
//   2) **事件行里的条件是快照。** rule_name / metric / comparison / threshold 来自
//      事件自己，规则后来改名、改阈值、被删掉都不影响这条历史——所以显示历史时
//      绝不能去规则列表里 join 最新的值，那会让一条旧事件看起来像是按新阈值触发的。
//   3) **确认不改状态。** acked_at 只回答"有没有人看见"，firing 仍然是 firing。
//      界面上把这两件事分开写，别让一次点击看起来像是"解决了"。
//
// 写动作（改规则、设静默、确认）走各自的 POST/PATCH/PUT；确认是唯一一个
// "点一下就完成"的写动作，所以它在这里有请求函数。

import { get, request, type ApiResult } from './client'

/** 规则状态三态（CONTRACTS §13.2）。没有第四个 "unknown"：那是"这一轮没做判断"。 */
export type AlertRuleState = 'ok' | 'pending' | 'firing'

/** 事件状态两态：一个事件周期一行，恢复是原地改这一行。 */
export type AlertEventState = 'firing' | 'resolved'

/** 比较方向闭集。 */
export type AlertComparison = 'gt' | 'gte' | 'lt' | 'lte'

/** 双口径合成方式。 */
export type AlertCombination = 'any' | 'all'

export type AlertScope = 'host' | 'interface' | 'filesystem'

export interface AlertRule {
  id: number
  name: string
  scope: AlertScope
  target_id: string
  metric: string
  comparison: AlertComparison
  threshold: number
  secondary_metric: string | null
  secondary_comparison: AlertComparison | null
  secondary_threshold: number | null
  combination: AlertCombination
  duration_seconds: number
  recovery_threshold: number
  cooldown_seconds: number
  enabled: boolean
  version: number

  state: AlertRuleState
  state_since: string
  recover_since: string
  cooldown_until: string
  last_eval_at: string
  /** null = 这一轮没读到数据（不是 0）。 */
  last_value: number | null
  /** "这条规则为什么一直不响"的唯一答案；正常超限/正常恢复时为空串。 */
  last_reason: string

  created_at: string
  updated_at: string
}

export interface AlertEvent {
  id: number
  /** 规则已被删除时为 0（外键置空），历史行保留。 */
  rule_id: number
  rule_name: string
  scope: AlertScope
  target_id: string
  metric: string
  comparison: AlertComparison
  threshold: number
  state: AlertEventState
  started_at: string
  /** 还在持续时为 null（不是空串）。 */
  resolved_at: string | null
  /** 实际持续时长（恢复时算好写死），不是规则上的配置值。 */
  duration_seconds: number
  /** 整个事件区间里朝告警方向最极端的值；区间里一个样本都没有时为 null。 */
  peak_value: number | null
  /** 触发时是否处在维护静默窗口内（事件照记，只是不发通知）。 */
  silenced: boolean
  acked_at: string | null
  /** 第一个确认的人；第二次确认不会覆盖它。 */
  acked_by: string
}

export interface AlertSilence {
  active: boolean
  starts_at: string | null
  ends_at: string | null
  reason: string
  updated_at: string
  updated_by: string
}

export interface AlertEventsQuery {
  state?: AlertEventState
  ruleId?: number
  limit?: number
}

/** 取规则列表（权限 V）。 */
export async function fetchAlertRules(): Promise<ApiResult<AlertRule[]>> {
  const result = await get<AlertRule[]>('/alerts/rules')
  return { requestId: result.requestId, data: result.data ?? [] }
}

/** 取事件列表，最新在前（权限 V）。 */
export async function fetchAlertEvents(query: AlertEventsQuery = {}): Promise<ApiResult<AlertEvent[]>> {
  const params: Record<string, string | number | undefined> = {}
  if (query.state) params['state'] = query.state
  if (query.ruleId) params['rule_id'] = query.ruleId
  if (query.limit) params['limit'] = query.limit
  const result = await get<AlertEvent[]>('/alerts/events', { query: params })
  return { requestId: result.requestId, data: result.data ?? [] }
}

/** 取当前维护静默窗口（权限 V）。 */
export async function fetchAlertSilence(): Promise<ApiResult<AlertSilence>> {
  return get<AlertSilence>('/alerts/silence')
}

/**
 * 确认一条事件（admin + CSRF）。
 *
 * 幂等：重复确认原样返回第一次的时间与人。成功之后**状态仍然是 firing**——
 * 页面刷新事件列表时不要因为 acked 就把它显示成已解决。
 */
export async function ackAlertEvent(eventId: number): Promise<ApiResult<AlertEvent>> {
  return request<AlertEvent>(`/alerts/events/${String(eventId)}/ack`, { method: 'POST' })
}

// ---------------------------------------------------------------- 展示口径

/** 规则状态 → 界面词。 */
export const ALERT_RULE_STATE_LABELS: Record<AlertRuleState, string> = {
  ok: '正常',
  pending: '正在攒持续',
  firing: '告警中',
}

/** 事件状态 → 界面词。 */
export const ALERT_EVENT_STATE_LABELS: Record<AlertEventState, string> = {
  firing: '还在持续',
  resolved: '已恢复',
}

/** 比较方向 → 符号。 */
export const ALERT_COMPARISON_SYMBOLS: Record<AlertComparison, string> = {
  gt: '>',
  gte: '≥',
  lt: '<',
  lte: '≤',
}

/**
 * 把一条规则的条件写成一句话。
 *
 * 双口径必须带上合成词（任一/全部）：只写两个条件不写"怎么合成"，读的人会以为是
 * 两个都要满足，而 any 是"任一满足就报"——那正好是最容易搞反的一处。
 */
export function alertConditionText(
  rule: Pick<
    AlertRule,
    | 'metric'
    | 'comparison'
    | 'threshold'
    | 'secondary_metric'
    | 'secondary_comparison'
    | 'secondary_threshold'
    | 'combination'
    | 'target_id'
    | 'scope'
  >,
): string {
  const primary = `${rule.metric} ${ALERT_COMPARISON_SYMBOLS[rule.comparison]} ${String(rule.threshold)}`
  const target = rule.scope === 'host' || rule.target_id === '' ? '' : `［${rule.target_id}］`
  if (
    rule.secondary_metric === null ||
    rule.secondary_comparison === null ||
    rule.secondary_threshold === null
  ) {
    return `${target}${primary}`
  }
  const secondary = `${rule.secondary_metric} ${ALERT_COMPARISON_SYMBOLS[rule.secondary_comparison]} ${String(rule.secondary_threshold)}`
  // 连接词随合成方式变：any 是"或"，all 是"且"。写成"或（全部满足才触发）"是自相矛盾。
  const joiner = rule.combination === 'all' ? '且' : '或'
  return `${target}${primary} ${joiner} ${secondary}`
}

/**
 * 一条事件"现在是什么情况"的一句话总结。
 *
 * 顺序是有讲究的：先状态（还在持续 / 已恢复），再持续多久，然后才是"没人确认"
 * 这件事。反过来的话，一条正在报的告警会因为"已确认"而看起来像过去了。
 */
export function alertEventSummary(event: AlertEvent, now: Date = new Date()): string {
  const parts: string[] = []
  if (event.state === 'firing') {
    const seconds = Math.max(0, Math.round((now.getTime() - new Date(event.started_at).getTime()) / 1000))
    parts.push(`还在持续，已经 ${formatDuration(seconds)}`)
  } else {
    parts.push(`已恢复，持续了 ${formatDuration(event.duration_seconds)}`)
  }
  if (event.silenced) parts.push('触发时处在维护静默窗口内，没有发出通知')
  parts.push(
    event.acked_at === null
      ? '还没有人确认'
      : `${event.acked_by || '某人'} 在 ${formatClock(event.acked_at)} 确认过`,
  )
  return parts.join('；')
}

/** 秒 → 人读时长（不出现"0 秒"这种看起来像没发生的说法）。 */
export function formatDuration(seconds: number): string {
  if (!Number.isFinite(seconds) || seconds < 0) return '未知'
  if (seconds < 60) return `${String(Math.round(seconds))} 秒`
  const minutes = Math.floor(seconds / 60)
  if (minutes < 60) return `${String(minutes)} 分钟`
  const hours = Math.floor(minutes / 60)
  const rest = minutes % 60
  if (hours < 24) return rest === 0 ? `${String(hours)} 小时` : `${String(hours)} 小时 ${String(rest)} 分钟`
  const days = Math.floor(hours / 24)
  return `${String(days)} 天 ${String(hours % 24)} 小时`
}

/** RFC3339 → 本地 `MM-DD HH:mm:ss`。解析不了就原样返回（不猜）。 */
export function formatClock(value: string): string {
  const t = new Date(value)
  if (Number.isNaN(t.getTime())) return value
  const pad = (n: number): string => String(n).padStart(2, '0')
  return `${pad(t.getMonth() + 1)}-${pad(t.getDate())} ${pad(t.getHours())}:${pad(t.getMinutes())}:${pad(t.getSeconds())}`
}

/**
 * 到底有没有"人知道"这件事。
 *
 * 分开算而不是一个布尔：界面上"还在报 + 没人确认"才是要红的那一格；
 * "还在报 + 有人确认过"是另一件事（有人在管，但还没解决）。
 */
export function acknowledgementState(event: Pick<AlertEvent, 'state' | 'acked_at'>): {
  unacked: boolean
  needsAttention: boolean
} {
  const unacked = event.acked_at === null
  return { unacked, needsAttention: unacked && event.state === 'firing' }
}
