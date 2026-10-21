// 终端 DTO 与帧编解码的纯逻辑用例（P06.3）。
//
// 这里不渲染页面、不连真代理，只验五件容易出错且各自对应一条验收要求的事：
//   1) WebSocket 地址怎么拼——ws/wss 推导错了，浏览器只给一句“连接失败”；
//   2) 帧怎么编解码——客户端只发 input/resize，服务端帧缺字段不能被编出默认值
//      （`exit_code` 缺失表示被信号终止，不是 0）；
//   3) base64 往返——中文与二进制都不能在中间被截断；
//   4) 连接许可只经子协议传递，且它绝不进 URL 查询参数；
//   5) 文案与状态闭集的兜底：未知值原样显示，不假装成已知状态。

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { API_BASE, setCsrfTokenProvider } from './client'
import {
  MAX_FRAME_PAYLOAD,
  TERMINAL_GRANT_PREFIX,
  TERMINAL_PROTOCOL,
  base64ToBytes,
  bytesToBase64,
  closeTerminalSession,
  createTerminalGrant,
  decodeTerminalFrame,
  encodeInputFrame,
  encodeResizeFrame,
  exitReasonLabel,
  fetchTerminalSessions,
  inDimensionRange,
  openTerminalSocket,
  outputBytes,
  resolveSocketUrl,
  sessionStateLabel,
} from './terminal'

function stubFetch(
  impl: (input: RequestInfo | URL, init?: RequestInit) => Promise<Response>,
): ReturnType<typeof vi.fn> {
  const mock = vi.fn(impl)
  vi.stubGlobal('fetch', mock)
  return mock
}

function dataResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify({ data: body, request_id: 'rid' }), {
    status,
    headers: { 'Content-Type': 'application/json' },
  })
}

beforeEach(() => {
  setCsrfTokenProvider(() => 'csrf-token-1')
})

afterEach(() => {
  vi.unstubAllGlobals()
  setCsrfTokenProvider(() => null)
})

describe('resolveSocketUrl', () => {
  it('相对基址按页面协议推导 ws/wss', () => {
    expect(resolveSocketUrl('/terminal/sessions/x/connect', API_BASE, {
      protocol: 'https:',
      host: 'panel.example',
    })).toBe('wss://panel.example/api/v1/terminal/sessions/x/connect')
    expect(resolveSocketUrl('/terminal/sessions/x/connect', API_BASE, {
      protocol: 'http:',
      host: '127.0.0.1:8080',
    })).toBe('ws://127.0.0.1:8080/api/v1/terminal/sessions/x/connect')
  })

  it('绝对基址跟着换协议，且去掉多余的斜杠', () => {
    expect(
      resolveSocketUrl('/terminal/sessions/x/connect', 'https://panel.example/api/v1/', {
        protocol: 'https:',
        host: 'ignored',
      }),
    ).toBe('wss://panel.example/api/v1/terminal/sessions/x/connect')
  })

  it('connectPath 没有前导斜杠也能拼', () => {
    expect(resolveSocketUrl('connect', '/api/v1', { protocol: 'http:', host: 'h' })).toBe(
      'ws://h/api/v1/connect',
    )
  })
})

describe('帧编码', () => {
  it('input 帧用 base64 承载 UTF-8 字节', () => {
    const raw = encodeInputFrame('ls 中文\n')
    expect(raw).not.toBeNull()
    const frame = JSON.parse(raw as string) as { type: string; data_b64: string }
    expect(frame.type).toBe('input')
    const bytes = base64ToBytes(frame.data_b64)
    expect(bytes).not.toBeNull()
    expect(new TextDecoder().decode(bytes as Uint8Array)).toBe('ls 中文\n')
  })

  it('空输入与超过 64 KiB 的输入在本地就被拒', () => {
    expect(encodeInputFrame('')).toBeNull()
    expect(encodeInputFrame('a'.repeat(MAX_FRAME_PAYLOAD + 1))).toBeNull()
    // 刚好 64 KiB 的 ASCII 是合法的（多字节字符按字节算）。
    expect(encodeInputFrame('a'.repeat(MAX_FRAME_PAYLOAD))).not.toBeNull()
  })

  it('resize 帧只在 1—500 内生成', () => {
    expect(encodeResizeFrame(120, 40)).toBe(JSON.stringify({ type: 'resize', cols: 120, rows: 40 }))
    expect(encodeResizeFrame(0, 24)).toBeNull()
    expect(encodeResizeFrame(80, 501)).toBeNull()
    expect(encodeResizeFrame(80.5, 24)).toBeNull()
    expect(inDimensionRange(500)).toBe(true)
    expect(inDimensionRange(501)).toBe(false)
  })
})

describe('帧解码', () => {
  it('解出 output 的字节', () => {
    const frame = decodeTerminalFrame(
      JSON.stringify({ type: 'output', data_b64: bytesToBase64(new TextEncoder().encode('hi')) }),
    )
    expect(frame?.type).toBe('output')
    const bytes = outputBytes(frame as NonNullable<typeof frame>)
    expect(new TextDecoder().decode(bytes as Uint8Array)).toBe('hi')
  })

  it('exit 帧没有 exit_code 时不编一个 0 出来', () => {
    const frame = decodeTerminalFrame(JSON.stringify({ type: 'exit', reason: 'idle' }))
    expect(frame?.exit_code).toBeUndefined()
    expect(frame?.reason).toBe('idle')
    const killed = decodeTerminalFrame(JSON.stringify({ type: 'exit', signal: 15 }))
    expect(killed?.exit_code).toBeUndefined()
    expect(killed?.signal).toBe(15)
  })

  it('非对象、缺 type、坏 JSON、数组都返回 null', () => {
    for (const raw of ['', 'null', '[]', '{"cols":80}', 'not json', '"text"']) {
      expect(decodeTerminalFrame(raw), raw).toBeNull()
    }
  })

  it('未知字段被丢弃，已知字段保留', () => {
    const frame = decodeTerminalFrame(
      JSON.stringify({ type: 'ready', account: 'upanel-shell', account_uid: 998, extra: 'x' }),
    )
    expect(frame).toEqual({ type: 'ready', account: 'upanel-shell', account_uid: 998 })
  })

  it('output 帧 base64 不合法时 outputBytes 返回 null 而不是抛异常', () => {
    const frame = decodeTerminalFrame(JSON.stringify({ type: 'output', data_b64: '!!!' }))
    expect(frame?.type).toBe('output')
    expect(outputBytes(frame as NonNullable<typeof frame>)).toBeNull()
  })

  it('不是 output 的帧没有字节', () => {
    const frame = decodeTerminalFrame(JSON.stringify({ type: 'ready' }))
    expect(outputBytes(frame as NonNullable<typeof frame>)).toBeNull()
  })
})

describe('文案兜底', () => {
  it('已知状态给中文，未知值原样显示', () => {
    expect(sessionStateLabel('attached')).toBe('已连接')
    expect(sessionStateLabel('detached')).toBe('等待重连（宽限期内）')
    expect(sessionStateLabel('weird')).toBe('weird')
    expect(exitReasonLabel('idle')).toContain('没有输入')
    expect(exitReasonLabel('weird')).toBe('weird')
  })
})

describe('接口调用', () => {
  it('会话列表是 GET /terminal/sessions', async () => {
    const mock = stubFetch(async () =>
      dataResponse({
        account: 'upanel-shell',
        account_uid: 998,
        idle_timeout_seconds: 900,
        max_duration_seconds: 3600,
        disconnect_grace_seconds: 60,
        max_per_user: 1,
        max_sessions: 2,
        sessions: [],
      }),
    )
    const result = await fetchTerminalSessions()
    expect(result.data.sessions).toEqual([])
    const [url, init] = mock.mock.calls[0] as [string, RequestInit]
    expect(url).toBe(`${API_BASE}/terminal/sessions`)
    expect(init.method).toBe('GET')
  })

  it('领许可用 POST + CSRF，且不带任何查询参数', async () => {
    const mock = stubFetch(async () =>
      dataResponse({
        session_id: 'a'.repeat(32),
        grant: 'b'.repeat(32),
        expires_at: '2026-10-21T03:00:30Z',
        protocol: TERMINAL_PROTOCOL,
        grant_protocol: TERMINAL_GRANT_PREFIX + 'b'.repeat(32),
        connect_path: `/terminal/sessions/${'a'.repeat(32)}/connect`,
      }),
    )
    const result = await createTerminalGrant('a'.repeat(32))
    expect(result.data.protocol).toBe(TERMINAL_PROTOCOL)
    const [url, init] = mock.mock.calls[0] as [string, RequestInit]
    expect(url).toBe(`${API_BASE}/terminal/sessions/${'a'.repeat(32)}/connection-grants`)
    expect(url).not.toContain('?')
    expect(init.method).toBe('POST')
    const headers = init.headers as Record<string, string>
    expect(headers['X-CSRF-Token']).toBe('csrf-token-1')
  })

  it('关闭会话用 DELETE，reason 走查询参数，会话 id 被编码', async () => {
    const mock = stubFetch(async () => dataResponse({ session_id: 'x', closed: true }))
    await closeTerminalSession('id/with slash')
    const [url, init] = mock.mock.calls[0] as [string, RequestInit]
    expect(url).toBe(`${API_BASE}/terminal/sessions/id%2Fwith%20slash?reason=user`)
    expect(init.method).toBe('DELETE')
  })

  it('连接许可只经子协议传递：URL 上不带许可', () => {
    const urls: string[] = []
    const protocols: string[][] = []
    openTerminalSocket(
      {
        connectPath: `/terminal/sessions/${'a'.repeat(32)}/connect`,
        protocol: TERMINAL_PROTOCOL,
        grantProtocol: TERMINAL_GRANT_PREFIX + 'b'.repeat(32),
      },
      (url, list) => {
        urls.push(url)
        protocols.push(list)
        return { url, protocols } as unknown as WebSocket
      },
    )
    expect(urls[0]).not.toContain('grant')
    expect(urls[0]).not.toContain('?')
    expect(protocols[0]).toEqual([
      TERMINAL_PROTOCOL,
      TERMINAL_GRANT_PREFIX + 'b'.repeat(32),
    ])
  })
})
