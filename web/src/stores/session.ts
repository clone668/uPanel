// 会话状态（Pinia）。
//
// 三条规则，来自 CONTRACTS.md 第 2 节：
//   1. Cookie 由服务端下发（HttpOnly/Secure/SameSite=Strict），前端**读不到也写不了**；
//   2. CSRF 令牌只存内存，页面刷新后靠 GET /auth/session 重新取回；
//   3. 未登录的页面统一跳登录页，不渲染“空数据”假装已登录。
//
// 页面只问这里要 user / csrfToken / 是否 admin，不自己发 /auth/session。

import { computed, ref } from 'vue'
import { defineStore } from 'pinia'

import {
  fetchSession,
  login as loginRequest,
  logout as logoutRequest,
  type Role,
  type SessionUser,
} from '@/api/auth'
import { ApiError, setCsrfTokenProvider } from '@/api/client'

export type SessionStatus = 'unknown' | 'anonymous' | 'authenticated'

export const useSessionStore = defineStore('session', () => {
  const user = ref<SessionUser | null>(null)
  const csrfToken = ref<string | null>(null)
  const expiresAt = ref<string | null>(null)
  const reauthExpiresAt = ref<string | null>(null)
  const status = ref<SessionStatus>('unknown')
  /** 首次/重新恢复会话的真实失败原因；登录页据此区分“未登录”与“服务不可用”。 */
  const restoreError = ref<ApiError | null>(null)
  /**
   * 当前账号是否启用了二次验证（P14.6）。只用于界面：决定再认证表单里要不要
   * 多一个验证码输入框。判定永远在服务端——这里为 false 时不带码，被
   * 401 + totp_required 拒掉之后仍然要能把码补上。
   */
  const totpEnabled = ref(false)

  const isAuthenticated = computed(() => status.value === 'authenticated')
  const role = computed<Role | null>(() => user.value?.role ?? null)
  const isAdmin = computed(() => role.value === 'admin')

  function applySession(data: {
    user: SessionUser
    csrf_token: string
    expires_at: string
    reauth_expires_at?: string | null
    totp_enabled?: boolean
  }): void {
    user.value = data.user
    csrfToken.value = data.csrf_token
    expiresAt.value = data.expires_at
    reauthExpiresAt.value = data.reauth_expires_at ?? null
    totpEnabled.value = data.totp_enabled === true
    restoreError.value = null
    status.value = 'authenticated'
  }

  function clear(): void {
    user.value = null
    csrfToken.value = null
    expiresAt.value = null
    reauthExpiresAt.value = null
    totpEnabled.value = false
    restoreError.value = null
    status.value = 'anonymous'
  }

  /**
   * 恢复会话：刷新页面后第一次调用。
   *
   * 401 是**正常结果**（就是没登录），不是错误；其他错误照常抛出，
   * 避免把“服务连不上”显示成“请登录”而让人反复输密码。
   */
  async function restore(): Promise<void> {
    restoreError.value = null
    try {
      const result = await fetchSession()
      applySession(result.data)
    } catch (caught) {
      if (caught instanceof ApiError && caught.code === 'UNAUTHENTICATED') {
        clear()
        return
      }
      status.value = 'anonymous'
      restoreError.value =
        caught instanceof ApiError
          ? caught
          : new ApiError({
              code: 'NETWORK_ERROR',
              message: `无法恢复面板会话：${caught instanceof Error ? caught.message : String(caught)}`,
              status: 0,
              requestId: '',
              source: 'client',
            })
      throw caught
    }
  }

  /**
   * 登录。失败时保持未登录状态，由页面展示错误。
   *
   * totp 为空表示"这次没带验证码"：启用了二次验证的账号会拿到 401 +
   * totp_required，页面据此亮出验证码输入框再调一次。
   */
  async function signIn(username: string, password: string, totp?: string): Promise<void> {
    const result = await loginRequest(username, password, totp)
    applySession(result.data)
  }

  /** 服务端确认销毁（或会话已失效）后才清空本地状态；传输失败时保留界面以便重试。 */
  async function signOut(): Promise<void> {
    try {
      await logoutRequest()
    } catch (caught) {
      if (caught instanceof ApiError && caught.code === 'UNAUTHENTICATED') {
        clear()
        return
      }
      throw caught
    }
    clear()
  }

  // 统一 API 客户端从这里取 CSRF 令牌（不落 localStorage）。
  setCsrfTokenProvider(() => csrfToken.value)

  return {
    user,
    csrfToken,
    expiresAt,
    reauthExpiresAt,
    status,
    restoreError,
    totpEnabled,
    isAuthenticated,
    role,
    isAdmin,
    restore,
    signIn,
    signOut,
    clear,
  }
})
