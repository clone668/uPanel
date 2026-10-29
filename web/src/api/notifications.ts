// 投递账 / 面板收件箱（P13.3 的接口，P13.4 的读路径）。
//
// 与 api/openapi.yaml 的 NotificationDelivery 逐字对应。
//
// 这里只有一条读路径：`GET /notification-deliveries`。它读的是**面板自己的账**，
// 不经过代理——代理此刻连不连得上，与"三天前那封邮件发出去了没有"是两件不相干的事。
//
// 界面上必须说清的三件事：
//
//   1) in_app 通道的投递**写入即 sent**，`attempts` 停在 0：它不发到任何地方，
//      读这个列表就是读面板内的通知。别把它显示成"发送成功 0 次尝试"那种矛盾话。
//   2) `next_attempt_at` 为 null 表示**不会再排下一次**（已发出或已判死），
//      不是"马上要重试"。拿它当倒计时会把一条死掉的投递画成"马上又试"。
//   3) `last_error` 是**对端原话**（"550 5.1.1 user unknown"），原样显示，
//      不要翻译成"发送失败"——翻译过的错误没法据以动手。

import { get, type ApiResult } from './client'

export type NotificationChannelType = 'in_app' | 'smtp' | 'webhook'

export type NotificationDeliveryKind = 'fired' | 'resolved' | 'test'

export type NotificationDeliveryState = 'pending' | 'sent' | 'failed'

export interface NotificationDelivery {
  id: number
  /** 0 = 通道后来被删掉了（外键置空），快照列仍然可读。 */
  channel_id: number
  channel_name: string
  channel_type: NotificationChannelType
  target: string
  /** 0 = 测试发送（或事件行已被删掉）。 */
  event_id: number
  /** 事件触发时那条规则的名字快照。 */
  event_rule: string
  kind: NotificationDeliveryKind
  title: string
  body: string
  state: NotificationDeliveryState
  /** 已经尝试过的次数；通道被停用/删除导致判死的行不计入。 */
  attempts: number
  max_attempts: number
  last_status: number
  last_error: string | null
  /** null = 不会再排下一次（已发出、已判死）。不是倒计时。 */
  next_attempt_at: string | null
  sent_at: string | null
  created_at: string
}

export interface NotificationDeliveriesQuery {
  state?: NotificationDeliveryState
  kind?: NotificationDeliveryKind
  limit?: number
}

/** 取投递账（admin）。 */
export async function fetchNotificationDeliveries(
  query: NotificationDeliveriesQuery = {},
): Promise<ApiResult<NotificationDelivery[]>> {
  const params: Record<string, string | number | undefined> = {}
  if (query.state) params['state'] = query.state
  if (query.kind) params['kind'] = query.kind
  if (query.limit) params['limit'] = query.limit
  const result = await get<NotificationDelivery[]>('/notification-deliveries', { query: params })
  return { requestId: result.requestId, data: result.data ?? [] }
}

// ---------------------------------------------------------------- 展示口径

export const DELIVERY_STATE_LABELS: Record<NotificationDeliveryState, string> = {
  pending: '等待投递',
  sent: '已投递',
  failed: '投递失败',
}

export const DELIVERY_KIND_LABELS: Record<NotificationDeliveryKind, string> = {
  fired: '告警触发',
  resolved: '告警恢复',
  test: '测试发送',
}

export const CHANNEL_TYPE_LABELS: Record<NotificationChannelType, string> = {
  in_app: '面板内通知',
  smtp: '邮件',
  webhook: 'Webhook',
}

/**
 * 一行投递的状态说明。
 *
 * 失败的三种来源要分开说：对端拒绝（有应答码）、连不上（没拿到应答码）、
 * 通道被删/停用（根本没发）。合成一句"投递失败"会让人去查网络，而真相是通道被关掉了。
 */
export function deliveryStateText(d: NotificationDelivery): string {
  if (d.state === 'sent') {
    if (d.channel_type === 'in_app') return '面板内通知：写到收件箱即完成，不往外发'
    return `已投递（对端应答 ${String(d.last_status)}）`
  }
  if (d.state === 'pending') {
    if (d.attempts === 0) return '排队中，还没试过'
    return `等待第 ${String(d.attempts + 1)} 次尝试（最多 ${String(d.max_attempts)} 次）`
  }
  if (d.last_status === 0) {
    return d.last_error === null || d.last_error === ''
      ? '投递失败：没有拿到对端应答'
      : `投递失败，没有拿到对端应答：${d.last_error}`
  }
  return `投递失败（对端应答 ${String(d.last_status)}）${d.last_error ? `：${d.last_error}` : ''}`
}

/** 这条投递还会不会自己再试。false 时界面不该显示"稍后重试"之类的暗示。 */
export function willRetry(d: NotificationDelivery): boolean {
  return d.state === 'pending' && d.next_attempt_at !== null && d.attempts < d.max_attempts
}
