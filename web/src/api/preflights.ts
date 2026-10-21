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
    timeoutMs: 20_000,
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
}): Promise<ApiResult<JobSubmitData>> {
  const body: Record<string, string> = { preflight_id: input.preflightId }
  if (input.confirmationPhrase !== undefined && input.confirmationPhrase !== '') {
    body.confirmation_phrase = input.confirmationPhrase
  }
  return request<JobSubmitData>('/jobs', {
    method: 'POST',
    body,
    idempotencyKey: input.idempotencyKey,
    timeoutMs: 20_000,
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
