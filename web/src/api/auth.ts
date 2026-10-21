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
}

export interface SessionData extends LoginData {
  reauth_expires_at: string | null
}

/** POST /auth/login。用户名或密码错误统一返回 UNAUTHENTICATED，不区分是哪一项。 */
export function login(username: string, password: string): Promise<ApiResult<LoginData>> {
  return request<LoginData>('/auth/login', {
    method: 'POST',
    body: { username, password },
  })
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
 */
export function reauthPassword(password: string): Promise<ApiResult<ReauthData>> {
  return request<ReauthData>('/auth/reauth', {
    method: 'POST',
    body: { password },
    timeoutMs: 15_000,
  })
}
