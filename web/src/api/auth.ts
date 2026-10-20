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
