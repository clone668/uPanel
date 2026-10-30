import { beforeEach, describe, expect, it, vi } from 'vitest'

import { AUTH_TIMEOUT_MS, login, reauthPassword } from './auth'

const mocks = vi.hoisted(() => ({
  get: vi.fn(),
  request: vi.fn(),
}))

vi.mock('./client', () => ({
  get: mocks.get,
  request: mocks.request,
}))

beforeEach(() => {
  mocks.get.mockReset()
  mocks.request.mockReset()
  mocks.request.mockResolvedValue({ data: {}, requestId: 'req-auth' })
})

describe('认证请求', () => {
  it('登录使用慢 Argon2 所需的专用超时，而不是默认 15 秒', async () => {
    await login('admin', 'secret', '123456')

    expect(mocks.request).toHaveBeenCalledWith('/auth/login', {
      method: 'POST',
      body: { username: 'admin', password: 'secret', totp: '123456' },
      timeoutMs: AUTH_TIMEOUT_MS,
    })
    expect(AUTH_TIMEOUT_MS).toBeGreaterThan(30_000)
  })

  it('再认证使用同一份慢校验预算', async () => {
    await reauthPassword('secret', '123456')

    expect(mocks.request).toHaveBeenCalledWith('/auth/reauth', {
      method: 'POST',
      body: { password: 'secret', totp: '123456' },
      timeoutMs: AUTH_TIMEOUT_MS,
    })
  })
})
