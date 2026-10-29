// 预检 → 任务 的提交链路（CONTRACTS.md 4.1、4.2）。
//
// 写操作在前端只有这一条路：先 POST /operations/preflight 拿计划（阻塞项、告警、
// 影响、是否要确认短语），再 POST /jobs 带 Idempotency-Key 提交。**没有**“直接执行”
// 的快捷方式——那等于绕过预检、再认证与审计。
//
// 三个要在这里写清楚的点：
//   1) 前端不能决定 allowed / requires_reauth / 短语：结论与策略全部来自服务端；
//   2) 幂等键是“一次意图”的标识：同一次提交重试必须复用它，成功后才作废；
//   3) 预检会过期（120 秒）。过期后要重新预检，而不是拿旧计划硬提交。

import { get, request, type ApiResult } from './client'
import type { JobDTO } from './jobs'

/**
 * 预检请求的超时。
 *
 * 20 秒对绝大多数动作都够：它们的预检只是读本地状态。但软件包动作的预检要跑
 * `apt-get -s`——实测（24.04，apt 2.8.3）升级模拟 15.6 秒、冷索引更久，20 秒会把
 * 一次正常的预检在前端自己掐掉，显示成"请求超时"，而服务端其实正在回答。
 */
export const PREFLIGHT_TIMEOUT_MS = 20_000

/**
 * 软件包动作预检的专用超时。
 *
 * 95 秒 = 服务端的上游上限（operations.PreflightAgentTimeout，90 秒）加余量：
 * 让服务端的那句真话（"apt 模拟超过 60 秒未完成"）先到，前端再超时。
 */
export const PACKAGE_PREFLIGHT_TIMEOUT_MS = 95_000

/**
 * 备份动作预检的专用超时（P12.5）。
 *
 * 备份的三个动作在预检阶段就要碰现场：读策略定义、解仓库口令、必要时拨号到远端
 * 仓库（SFTP 的 host key 核对与 restic 的仓库解锁都在这一步），代理给这一段的上限
 * 是 90 秒（备份恢复类动作的 PlanBudget）。20 秒会把一次正常的预检在前端自己掐断，
 * 显示成"请求超时"，而服务端其实正在回答。
 */
export const BACKUP_PREFLIGHT_TIMEOUT_MS = 95_000

/**
 * 备份动作提交的专用超时。
 *
 * 提交不是转发：代理要在这一刻**再核一次**目标（快照还在不在、仓库定义有没有变），
 * 那条路的预算同样是 90 秒。用 20 秒会让人看到"请求超时"，而任务其实已经建好并在跑。
 */
export const BACKUP_SUBMIT_TIMEOUT_MS = 95_000

/**
 * 网络写动作预检的专用超时（P14.2）。
 *
 * 代理侧给这几个动作的预检预算就是 20 秒（`networkApplyPlanBudget`）：预检要读
 * `/etc/netplan`、跑几次 `ip`、读 `/run` 里的渲染产物，正常几十毫秒，20 秒是为了
 * 让"网络栈卡住"变成一条明确的超时结论，而不是挂到 HTTP 超时为止。
 *
 * 前端因此必须**比它宽**：相等的话，代理正在写那句超时结论时前端就先掐断了，
 * 用户看到的是自己的"请求超时"，而不是代理那句"预检超时"。30 秒 = 20 秒加往返与余量。
 */
export const NETWORK_PREFLIGHT_TIMEOUT_MS = 30_000

/**
 * 防火墙写动作预检的专用超时（P14.3）。
 *
 * 代理侧的预算是 20 秒（`firewallPlanBudget`），但这里要留得比网络那边更宽一点：
 * 预检要跑两次 `ufw`（`status verbose numbered` 与 `show added`），而 **ufw 自己会
 * 抢 `/run/ufw/lock`** —— 管理员此刻正在另一个终端里敲 ufw 命令时，我们的命令会排在
 * 那把锁上等。等锁不是故障，可它会把"预检"变成一次长达十几秒的静默。
 * 前端必须比代理宽，否则用户看到的是自己的"请求超时"，而不是代理那句"预检超时"。
 */
export const FIREWALL_PREFLIGHT_TIMEOUT_MS = 30_000

/** 该动作的预检超时。只有 apt 写事务、备份、网络写与防火墙写要放宽。 */
export function preflightTimeoutFor(action: string): number {
  if (action.startsWith('package.')) return PACKAGE_PREFLIGHT_TIMEOUT_MS
  if (action.startsWith('backup.')) return BACKUP_PREFLIGHT_TIMEOUT_MS
  if (action.startsWith('network.')) return NETWORK_PREFLIGHT_TIMEOUT_MS
  if (action.startsWith('firewall.')) return FIREWALL_PREFLIGHT_TIMEOUT_MS
  return PREFLIGHT_TIMEOUT_MS
}

/**
 * 提交任务的默认超时。
 *
 * 大多数动作的提交只是"建一条任务并让代理开始跑"，代理很快就把 201 交回来。
 */
export const SUBMIT_TIMEOUT_MS = 20_000

/**
 * 软件包动作提交的专用超时。
 *
 * 提交不是转发：代理要在这一刻**再跑一次 `apt-get -s`** 复核计划，因为它可能已经
 * 是几十秒前的了（24.04 实测 38.7 秒）。Web 侧为此把写预算放到
 * `slowOperationWriteBudget`（100 秒），代理侧放到 `PreflightTTL`（120 秒）；
 * 前端如果还用 20 秒，用户看到"请求超时"，而任务其实已经建好并在跑——那一屏
 * 会让人再点一次，于是多出一条任务。
 */
export const PACKAGE_SUBMIT_TIMEOUT_MS = 95_000

/**
 * 网络写动作提交的专用超时（P14.2）。
 *
 * 提交时代理会**再跑一遍预检**（现场可能变了），上限同样是 20 秒。但这里还要留出
 * 一种更糟的情形：`network.confirm` 的提交发生在"浏览器刚从新地址连上来"的时刻，
 * 链路本身可能正抖。20 秒的默认值会让这次确认在前端自己超时，而服务端其实已经
 * 把 timer 停了——用户会以为没确认成功，再点一次，然后得到"没有待确认的改动"。
 */
export const NETWORK_SUBMIT_TIMEOUT_MS = 30_000

/**
 * 防火墙写动作提交的专用超时（P14.3）。
 *
 * 与网络那边同一个数字，理由却不同：防火墙改动**不会**掐断浏览器自己那条连接
 * （预检里 `FIREWALL_MANAGEMENT_PATH` 保证了管理通路还在），所以这里不需要为链路
 * 抖动留出余量。要留的是 ufw 的那把锁——提交时代理会**再跑一遍预检**，同样排两次
 * `ufw`。用它自己的 20 秒默认值，一次"管理员正开着 ufw 终端"的提交就会被前端判成
 * 超时，而任务其实已经建好并在跑。
 */
export const FIREWALL_SUBMIT_TIMEOUT_MS = 30_000

/** 该动作的提交超时。 */
export function submitTimeoutFor(action: string): number {
  if (action.startsWith('package.')) return PACKAGE_SUBMIT_TIMEOUT_MS
  if (action.startsWith('backup.')) return BACKUP_SUBMIT_TIMEOUT_MS
  if (action.startsWith('network.')) return NETWORK_SUBMIT_TIMEOUT_MS
  if (action.startsWith('firewall.')) return FIREWALL_SUBMIT_TIMEOUT_MS
  return SUBMIT_TIMEOUT_MS
}

export interface PreflightNotice {
  code: string
  message: string
}

export interface PreflightImpact {
  kind: string
  description: string
}

/** POST /operations/preflight 的返回体（字段与 CONTRACTS.md 4.1 一致）。 */
export interface PreflightData {
  preflight_id: string
  action: string
  normalized_params: Record<string, unknown>
  target: {
    display_name: string
    identity_fingerprint: string
  }
  expires_at: string
  allowed: boolean
  blockers: PreflightNotice[]
  warnings: PreflightNotice[]
  impacts: PreflightImpact[]
  requires_reauth: boolean
  /** null 表示这次操作不需要确认短语（不是“服务端忘了给”）。 */
  confirmation_phrase: string | null
  cancellable: boolean
  locks: string[]
}

/** POST /jobs 的返回体。 */
export interface JobSubmitData {
  job_id: string
  status: string
  events_url: string
}

/** 生成一次意图的幂等键（UUID v4）。 */
export function newIdempotencyKey(): string {
  const webCrypto = globalThis.crypto
  if (webCrypto && typeof webCrypto.randomUUID === 'function') {
    return webCrypto.randomUUID()
  }
  // 回退：只在没有 crypto.randomUUID 的环境里用到（老浏览器/受限预览）。
  // 仍然是 122 bit 随机量，形状与服务端要求一致（非空、无控制字符）。
  const bytes = new Uint8Array(16)
  if (webCrypto && typeof webCrypto.getRandomValues === 'function') {
    webCrypto.getRandomValues(bytes)
  } else {
    for (let i = 0; i < bytes.length; i += 1) {
      bytes[i] = Math.floor(Math.random() * 256)
    }
  }
  bytes[6] = ((bytes[6] ?? 0) & 0x0f) | 0x40
  bytes[8] = ((bytes[8] ?? 0) & 0x3f) | 0x80
  const hex = [...bytes].map((byte) => byte.toString(16).padStart(2, '0')).join('')
  return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`
}

/** 请求一份预检计划（不改变系统状态）。 */
export function createPreflight(
  action: string,
  params: Record<string, unknown>,
): Promise<ApiResult<PreflightData>> {
  return request<PreflightData>('/operations/preflight', {
    method: 'POST',
    body: { action, params },
    timeoutMs: preflightTimeoutFor(action),
  })
}

/**
 * 提交任务。
 *
 * 请求体只有预检 id 与确认短语：目标与参数一律按预检恢复，提交阶段不能替换
 * （CONTRACTS.md 4.2）。幂等键由调用方在一次意图内复用。
 */
export function submitJob(input: {
  preflightId: string
  confirmationPhrase?: string
  idempotencyKey: string
  /** 该动作的提交超时；包类动作请传 `submitTimeoutFor(action)`。 */
  timeoutMs?: number
}): Promise<ApiResult<JobSubmitData>> {
  const body: Record<string, string> = { preflight_id: input.preflightId }
  if (input.confirmationPhrase !== undefined && input.confirmationPhrase !== '') {
    body.confirmation_phrase = input.confirmationPhrase
  }
  return request<JobSubmitData>('/jobs', {
    method: 'POST',
    body,
    idempotencyKey: input.idempotencyKey,
    timeoutMs: input.timeoutMs ?? SUBMIT_TIMEOUT_MS,
  })
}

/** 读取任务状态（提交后轮询用；任务中心也走这里）。 */
export function fetchJobStatus(jobId: string): Promise<ApiResult<JobDTO>> {
  return get<JobDTO>(`/jobs/${encodeURIComponent(jobId)}`)
}

/**
 * 预检是否已过期。expires_at 无法解析时按“已过期”处理：
 * 宁可多要一次预检，也不要拿一个来历不明的时间去提交。
 */
export function isPreflightExpired(preflight: Pick<PreflightData, 'expires_at'>, now: Date): boolean {
  const at = new Date(preflight.expires_at).getTime()
  if (Number.isNaN(at)) return true
  return at <= now.getTime()
}

/** 预检剩余秒数（界面显示“还有 N 秒”，过期返回 0）。 */
export function preflightRemainingSeconds(
  preflight: Pick<PreflightData, 'expires_at'>,
  now: Date,
): number {
  const at = new Date(preflight.expires_at).getTime()
  if (Number.isNaN(at)) return 0
  return Math.max(0, Math.floor((at - now.getTime()) / 1000))
}

/** 这次预检能不能直接提交（允许 + 未过期）。 */
export function canSubmitPreflight(
  preflight: Pick<PreflightData, 'allowed' | 'expires_at'>,
  now: Date,
): boolean {
  return preflight.allowed && !isPreflightExpired(preflight, now)
}

/** 服务端是否要求确认短语。 */
export function requiresConfirmation(preflight: Pick<PreflightData, 'confirmation_phrase'>): boolean {
  return preflight.confirmation_phrase !== null && preflight.confirmation_phrase !== ''
}

/** 短语是否输对了（区分大小写：短语是服务端生成的一次性口令）。 */
export function confirmationMatches(
  preflight: Pick<PreflightData, 'confirmation_phrase'>,
  typed: string,
): boolean {
  if (!requiresConfirmation(preflight)) return true
  return typed === preflight.confirmation_phrase
}
