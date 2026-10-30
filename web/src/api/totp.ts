// 二次验证（TOTP）的三条接口（CONTRACTS.md 第 2 节 / P14.6）。
//
// 前端在这条链路上**看不到种子**：setup 拿到的是"引用 + 一次性 otpauth URI"，
// 后者只用于把种子搬到 authenticator 里（本页把它画成文本让用户复制，不写任何存储）；
// confirm 拿到的是十枚一次性恢复码，只在成功那一刻显示一次；disable 之后一切归零。
//
// 三条都要 admin + 再认证 + CSRF，所以调用方必须先完成 reauth（第二次因子也要带上）。

import { AUTH_TIMEOUT_MS } from './auth'
import { request, type ApiResult } from './client'

/** POST /auth/totp/setup 的返回。enrollment_id 是引用，otpauth_uri 里带着种子。 */
export interface TotpSetupData {
  enrollment_id: string
  otpauth_uri: string
  issuer: string
  account: string
  digits: number
  period: number
  /** 未确认登记的到期时间（代理侧 10 分钟自毁）；缺省表示代理没给。 */
  expires_at?: string
}

/** POST /auth/totp/confirm 的返回：恢复码只在这一刻出现。 */
export interface TotpConfirmData {
  enabled: boolean
  recovery_codes: string[]
}

/** DELETE /auth/totp 的请求体：口令 + 一种第二因子。 */
export interface TotpDisableInput {
  password: string
  totp?: string
  recovery_code?: string
}

/** DELETE /auth/totp 的返回：作废的其它会话数。 */
export interface TotpDisableData {
  enabled: boolean
  revoked_sessions: number
}

/**
 * 生成一条新种子，返回二维码数据（尚未启用）。
 *
 * 已启用的账号调用会得到 422：换设备要先把旧的关掉，否则库与代理各留一份，
 * 下一次登录到底校验哪一份就没有答案了。
 */
export function totpSetup(): Promise<ApiResult<TotpSetupData>> {
  return request<TotpSetupData>('/auth/totp/setup', { method: 'POST', body: {} })
}

/**
 * 用 authenticator 上的六位数确认登记。
 *
 * 码不对是 401（服务端当作"某人在猜"），不是 400：重试同样的请求是对的，
 * 但每次失败都记进登录限速。
 */
export function totpConfirm(enrollmentId: string, code: string): Promise<ApiResult<TotpConfirmData>> {
  return request<TotpConfirmData>('/auth/totp/confirm', {
    method: 'POST',
    body: { enrollment_id: enrollmentId, totp: code },
  })
}

/** 关闭二次验证。验证码与恢复码二者给一个；两个都给时服务端先看验证码。 */
export function totpDisable(input: TotpDisableInput): Promise<ApiResult<TotpDisableData>> {
  return request<TotpDisableData>('/auth/totp', {
    method: 'DELETE',
    body: input,
    // 这里会再次做 Argon2id 口令校验，与登录/再认证共用慢机器预算。
    timeoutMs: AUTH_TIMEOUT_MS,
  })
}

/**
 * 判断一个错误是不是"需要二次验证码"。
 *
 * 服务端在两种情况下回答 401 + `details.totp_required = true`：这次请求没带码，
 * 或者码不对。前端据此把验证码输入框亮出来，而不是把 401 显示成"口令错误"——
 * 这两句话对用户意味着完全不同的下一步。
 */
export function secondFactorRequired(error: unknown): boolean {
  if (typeof error !== 'object' || error === null) return false
  const details = (error as { details?: unknown }).details
  if (typeof details !== 'object' || details === null) return false
  return (details as { totp_required?: unknown }).totp_required === true
}
