// 认证接口（CONTRACTS.md 第 2 节）。
//
// 只有三条：登录、读会话、退出。所有会话状态都在服务端，前端只保存
// **内存里的 CSRF 令牌**与当前用户视图——不写 localStorage，也不在 Cookie 之外留凭据。

import { get, request, type ApiResult } from './client'

export type Role = 'admin' | 'viewer'

/** 与 Go 侧 userDTO 同名字段（snake_case，不做驼峰转换）。 */
export interface SessionUser {
  id: number
  username: string
  display_name: string
  role: Role
}

export interface LoginData {
  user: SessionUser
  csrf_token: string
  expires_at: string
  /**
   * 这个账号是否启用了二次验证（P14.6）。只用于界面提示：真正的判定在服务端，
   * 一次"因为它是 false 所以没带码"的请求仍然可能被 401 + totp_required 拒。
   */
  totp_enabled: boolean
}

export interface SessionData extends LoginData {
  reauth_expires_at: string | null
}

/**
 * POST /auth/login。用户名或密码错误统一返回 UNAUTHENTICATED，不区分是哪一项。
 *
 * 启用了二次验证的账号：第一次不带 totp 会得到 401 + details.totp_required，
 * 此时把口令与验证码一起再发一次。口令不会因为第一次没带码而失效，也不会因此
 * 签发半张票（服务端两步之间一个会话都不发）。
 */
export function login(
  username: string,
  password: string,
  totp?: string,
): Promise<ApiResult<LoginData>> {
  const body: { username: string; password: string; totp?: string } = { username, password }
  if (totp !== undefined && totp !== '') body.totp = totp
  return request<LoginData>('/auth/login', { method: 'POST', body })
}

/** GET /auth/session。未登录时返回 401 UNAUTHENTICATED。 */
export function fetchSession(): Promise<ApiResult<SessionData>> {
  return get<SessionData>('/auth/session')
}

/** POST /auth/logout。服务端销毁会话，前端随后清空内存状态。 */
export function logout(): Promise<ApiResult<unknown>> {
  return request<unknown>('/auth/logout', { method: 'POST' })
}

/** POST /auth/reauth 的返回体：再认证窗口的到期时间。 */
export interface ReauthData {
  reauth_expires_at: string
}

/**
 * 密码再认证（admin + CSRF）。
 *
 * 需要它的动作（账号变更、终端创建、覆盖写……）由服务端的动作策略决定，
 * 前端不自己判断“够不够 5 分钟”。成功后再认证窗口被推到 5 分钟后；
 * 失败与登录共用限速计数。
 *
 * 账号启用了二次验证时必须同时给 totp：服务端把口令与验证码当两件事分别校验，
 * 只带口令会得到 401 + details.totp_required，而再认证窗口**不会**推进。
 */
export function reauthPassword(password: string, totp?: string): Promise<ApiResult<ReauthData>> {
  const body: { password: string; totp?: string } = { password }
  if (totp !== undefined && totp !== '') body.totp = totp
  return request<ReauthData>('/auth/reauth', {
    method: 'POST',
    body,
    timeoutMs: 15_000,
  })
}
