import { beforeEach, describe, expect, it, vi } from 'vitest'
import { createPinia, setActivePinia } from 'pinia'

import { ApiError } from '@/api/client'
import { useSessionStore } from './session'

const mocks = vi.hoisted(() => ({
  fetchSession: vi.fn(),
  login: vi.fn(),
  logout: vi.fn(),
}))

vi.mock('@/api/auth', () => ({
  fetchSession: mocks.fetchSession,
  login: mocks.login,
  logout: mocks.logout,
}))

function apiError(code: ApiError['code'], source: ApiError['source']): ApiError {
  return new ApiError({ code, message: '恢复失败', status: source === 'server' ? 401 : 0, requestId: 'req-1', source })
}

function sessionData() {
  return {
    user: { id: 1, username: 'admin', display_name: '管理员', role: 'admin' },
    csrf_token: 'csrf-new',
    expires_at: '2026-10-30T12:00:00Z',
    reauth_expires_at: null,
    totp_enabled: false,
  }
}

beforeEach(() => {
  setActivePinia(createPinia())
  mocks.fetchSession.mockReset()
  mocks.login.mockReset()
  mocks.logout.mockReset()
})

describe('会话恢复', () => {
  it('传输失败保留真实错误，不伪装成普通的未登录', async () => {
    const failure = apiError('NETWORK_ERROR', 'client')
    mocks.fetchSession.mockRejectedValueOnce(failure)
    const session = useSessionStore()

    await expect(session.restore()).rejects.toBe(failure)

    expect(session.status).toBe('anonymous')
    expect(session.restoreError).toBe(failure)
    expect(session.csrfToken).toBeNull()
  })

  it('401 是正常匿名状态，不留下服务故障提示', async () => {
    mocks.fetchSession.mockRejectedValueOnce(apiError('UNAUTHENTICATED', 'server'))
    const session = useSessionStore()

    await expect(session.restore()).resolves.toBeUndefined()

    expect(session.status).toBe('anonymous')
    expect(session.restoreError).toBeNull()
  })

  it('重试成功后清掉上一次恢复错误并装入新会话', async () => {
    mocks.fetchSession
      .mockRejectedValueOnce(apiError('NETWORK_ERROR', 'client'))
      .mockResolvedValueOnce({ data: sessionData(), requestId: 'req-2' })
    const session = useSessionStore()

    await expect(session.restore()).rejects.toThrow()
    expect(session.restoreError).not.toBeNull()
    await session.restore()

    expect(session.status).toBe('authenticated')
    expect(session.restoreError).toBeNull()
    expect(session.csrfToken).toBe('csrf-new')
    expect(session.user?.username).toBe('admin')
  })

  it('退出请求失败时保留已认证界面，允许用户知道并重试', async () => {
    mocks.fetchSession.mockResolvedValueOnce({ data: sessionData(), requestId: 'req-2' })
    const failure = apiError('NETWORK_ERROR', 'client')
    mocks.logout.mockRejectedValueOnce(failure)
    const session = useSessionStore()
    await session.restore()

    await expect(session.signOut()).rejects.toBe(failure)

    expect(session.status).toBe('authenticated')
    expect(session.user?.username).toBe('admin')
    expect(session.csrfToken).toBe('csrf-new')
  })

  it('服务端确认退出后才清空会话', async () => {
    mocks.fetchSession.mockResolvedValueOnce({ data: sessionData(), requestId: 'req-2' })
    mocks.logout.mockResolvedValueOnce({ data: {}, requestId: 'req-3' })
    const session = useSessionStore()
    await session.restore()

    await session.signOut()

    expect(session.status).toBe('anonymous')
    expect(session.user).toBeNull()
    expect(session.csrfToken).toBeNull()
  })

  it('服务端明确说会话已失效时，退出可以完成本地清理', async () => {
    mocks.fetchSession.mockResolvedValueOnce({ data: sessionData(), requestId: 'req-2' })
    mocks.logout.mockRejectedValueOnce(apiError('UNAUTHENTICATED', 'server'))
    const session = useSessionStore()
    await session.restore()

    await expect(session.signOut()).resolves.toBeUndefined()

    expect(session.status).toBe('anonymous')
    expect(session.user).toBeNull()
    expect(session.csrfToken).toBeNull()
  })
})
