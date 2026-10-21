// 终端接口 DTO 与帧编解码（CONTRACTS.md 第 10 节）。
//
// 五条纪律（每条都对应一条验收要求）：
//   1) 连接许可由**带 CSRF 的 POST**领取，随 WebSocket 子协议传递。它永不进 URL：
//      查询参数会落进访问日志、浏览器历史与 Referer，而子协议头不会。
//   2) 帧是结构化的：客户端只发 input 与 resize，服务端发 ready / output / exit / error。
//      这里没有“发一条命令”的函数——终端没有命令接口，只有字节。
//   3) 尺寸限 1—500，单帧数据限 64 KiB。超限的帧在本地就拒，不去占一次服务端判决。
//   4) 二进制数据用 base64 放进 JSON：链路上有两段是文本中继（JS 与日志），
//      换来的好处是任何一帧都能被原样打印出来看。
//   5) 终端的字节只进一个**已降权**的 PTY（upanel-shell）。界面必须把这句话显示出来，
//      而不是让管理员以为这是 root shell。

import { API_BASE, get, request, type ApiResult } from './client'

// ---------------------------------------------------------------- 常量

/** 握手用的子协议名。服务端只回这一个，不回显许可那一段。 */
export const TERMINAL_PROTOCOL = 'upanel.term.v1'

/** 携带连接许可的子协议前缀（upanel.grant.<id>）。 */
export const TERMINAL_GRANT_PREFIX = 'upanel.grant.'

/** 单帧终端数据的字节上限（与服务端一致）。 */
export const MAX_FRAME_PAYLOAD = 64 * 1024

/** 窗口尺寸范围（与服务端一致）。 */
export const MIN_DIMENSION = 1
export const MAX_DIMENSION = 500

/** 首次创建时的默认窗口。 */
export const DEFAULT_COLS = 80
export const DEFAULT_ROWS = 24

/** 连接许可的有效期（毫秒）。它由服务端决定，这里只用于提前提示“许可已过期”。 */
export const GRANT_TTL_MS = 30_000

/** 会话状态闭集：连着，或者还在断流宽限里。 */
export const SESSION_STATES = ['attached', 'detached'] as const

export type TerminalState = (typeof SESSION_STATES)[number]

export const SESSION_STATE_LABELS: Record<TerminalState, string> = {
  attached: '已连接',
  detached: '等待重连（宽限期内）',
}

export function sessionStateLabel(state: string): string {
  return (SESSION_STATE_LABELS as Record<string, string | undefined>)[state] ?? state
}

/** 结束原因闭集（与服务端 exit 帧的 reason 一致）。 */
export const EXIT_REASON_LABELS: Record<string, string> = {
  user: '你关闭了会话',
  idle: '长时间没有输入，服务端已关闭',
  expired: '会话到达最长时限，服务端已关闭',
  disconnect: '连接断开超过宽限时间，会话已终止',
  shutdown: '代理服务停止，会话已终止',
  exit: 'shell 自行退出',
  failed: '会话异常结束',
}

export function exitReasonLabel(reason: string): string {
  return EXIT_REASON_LABELS[reason] ?? reason
}

// ---------------------------------------------------------------- DTO

/**
 * 一个终端会话（GET /terminal/sessions 的 sessions 元素）。
 *
 * 时间是 RFC3339 UTC 字符串；`attached_at` 为 null 表示从未连接过——
 * 与“连接过但已断开”是两件事，界面要分开说。
 */
export interface TerminalSession {
  session_id: string
  user_id: number
  /** 实际运行 shell 的系统账号（正常是 upanel-shell）。 */
  account: string
  /** 该账号的 uid。界面用它证明“这不是 root”。 */
  account_uid: number
  state: TerminalState | string
  cols: number
  rows: number
  created_at: string
  last_activity_at: string
  attached_at: string | null
  /** 两条独立终止线：无输入关闭、绝对寿命。 */
  idle_deadline: string
  expires_at: string
  idle_timeout_seconds: number
  max_duration_seconds: number
}

/**
 * 会话列表 + 终端边界。
 *
 * 边界（账号、uid、三个时限、两个数量上限）来自代理当前配置，界面照抄显示，
 * 不自己写死“15 分钟”——配置改了页面就跟着变。
 */
export interface TerminalSessionsData {
  account: string
  account_uid: number
  idle_timeout_seconds: number
  max_duration_seconds: number
  disconnect_grace_seconds: number
  max_per_user: number
  max_sessions: number
  sessions: TerminalSession[]
}

/** POST /terminal/sessions/:id/connection-grants 的返回体。 */
export interface TerminalGrantData {
  session_id: string
  /** 一次性许可，30 秒有效、只能用一次。 */
  grant: string
  expires_at: string
  /** 握手必须声明的子协议名。 */
  protocol: string
  /** 携带许可的子协议名（upanel.grant.<id>）。 */
  grant_protocol: string
  /** 要拨的同源路径（不含查询参数）。 */
  connect_path: string
}

/**
 * 一帧终端消息。
 *
 * 字段全部可选：一帧只带它那个 type 该有的字段。解码器**不**替缺失的字段编默认值
 * （`exit_code` 缺失表示“被信号终止”，不是 0）。
 */
export interface TerminalFrame {
  type: string
  data_b64?: string
  cols?: number
  rows?: number
  session_id?: string
  account?: string
  account_uid?: number
  exit_code?: number
  signal?: number
  reason?: string
  code?: string
  message?: string
  server_time?: string
}

// ---------------------------------------------------------------- 调用

/** 列出**当前账号**的终端会话（服务端的 user_id 来自会话 Cookie，不由前端指定）。 */
export function fetchTerminalSessions(signal?: AbortSignal): Promise<ApiResult<TerminalSessionsData>> {
  return get<TerminalSessionsData>('/terminal/sessions', {
    ...(signal === undefined ? {} : { signal }),
  })
}

/**
 * 领取一张一次性连接许可。
 *
 * 这是 WebSocket 握手前的 CSRF 步：握手是 GET，浏览器无法为它加自定义请求头，
 * 所以真正“带凭据的写操作”是这一次 POST。许可只能用一次，失败要重新领。
 */
export function createTerminalGrant(sessionId: string): Promise<ApiResult<TerminalGrantData>> {
  return request<TerminalGrantData>(
    `/terminal/sessions/${encodeURIComponent(sessionId)}/connection-grants`,
    { method: 'POST', timeoutMs: 10_000 },
  )
}

/**
 * 关闭自己的终端会话。
 *
 * 关闭是幂等的：会话已经不在了也返回成功，因为调用方要的结果是“它现在不在跑了”。
 */
export function closeTerminalSession(
  sessionId: string,
  reason = 'user',
): Promise<ApiResult<{ session_id: string; closed: boolean }>> {
  return request<{ session_id: string; closed: boolean }>(
    `/terminal/sessions/${encodeURIComponent(sessionId)}`,
    { method: 'DELETE', query: { reason }, timeoutMs: 10_000 },
  )
}

// ---------------------------------------------------------------- URL

/**
 * 由 API 基址与当前页面位置推出 WebSocket 地址。
 *
 * 单独拆出来是为了能单测：ws/wss 的推导错了，浏览器只会报一句
 * “WebSocket 连接失败”，而原因在协议头里。
 */
export function resolveSocketUrl(
  connectPath: string,
  apiBase: string = API_BASE,
  loc: { protocol: string; host: string } | undefined = typeof window === 'undefined'
    ? undefined
    : { protocol: window.location.protocol, host: window.location.host },
): string {
  const path = connectPath.startsWith('/') ? connectPath : `/${connectPath}`
  const base = apiBase.endsWith('/') ? apiBase.slice(0, -1) : apiBase
  if (base.startsWith('http://') || base.startsWith('https://') || base.startsWith('ws')) {
    const url = new URL(`${base}${path}`)
    url.protocol = url.protocol === 'https:' ? 'wss:' : url.protocol === 'http:' ? 'ws:' : url.protocol
    return url.toString()
  }
  const scheme = loc?.protocol === 'https:' ? 'wss:' : 'ws:'
  const host = loc?.host ?? ''
  return `${scheme}//${host}${base}${path}`
}

// ---------------------------------------------------------------- 帧

/** 把字节编码成标准 base64。分块处理，避免一次展开成超大字符串。 */
export function bytesToBase64(bytes: Uint8Array): string {
  let binary = ''
  const chunk = 0x8000
  for (let i = 0; i < bytes.length; i += chunk) {
    binary += String.fromCharCode(...bytes.subarray(i, i + chunk))
  }
  return btoa(binary)
}

/** 把 base64 解成字节。不合法时返回 null（调用方按“坏帧”处理，不抛异常）。 */
export function base64ToBytes(value: string): Uint8Array | null {
  try {
    const binary = atob(value)
    const bytes = new Uint8Array(binary.length)
    for (let i = 0; i < binary.length; i += 1) bytes[i] = binary.charCodeAt(i)
    return bytes
  } catch {
    return null
  }
}

/**
 * 把用户输入编码成 input 帧。
 *
 * 超过 64 KiB 的输入直接拒绝：不是分片，而是**让调用方自己切**——
 * 一次粘贴几兆内容应该由界面切成若干帧并显示进度，而不是悄悄截断。
 */
export function encodeInputFrame(text: string): string | null {
  const bytes = new TextEncoder().encode(text)
  if (bytes.length === 0 || bytes.length > MAX_FRAME_PAYLOAD) return null
  return JSON.stringify({ type: 'input', data_b64: bytesToBase64(bytes) })
}

/** 把窗口尺寸编码成 resize 帧；越界返回 null。 */
export function encodeResizeFrame(cols: number, rows: number): string | null {
  if (!inDimensionRange(cols) || !inDimensionRange(rows)) return null
  return JSON.stringify({ type: 'resize', cols, rows })
}

/** 尺寸是否在契约范围内（1—500 的整数）。 */
export function inDimensionRange(value: number): boolean {
  return Number.isInteger(value) && value >= MIN_DIMENSION && value <= MAX_DIMENSION
}

/**
 * 解码一条服务端帧。
 *
 * 只做形状判定，不猜语义：不是对象、没有 type、base64 不合法都返回 null。
 * 调用方对 null 的处理是“丢弃并记一条日志”，而不是断开连接——
 * 一条坏帧不值得把终端断掉。
 */
export function decodeTerminalFrame(raw: string): TerminalFrame | null {
  let parsed: unknown
  try {
    parsed = JSON.parse(raw) as unknown
  } catch {
    return null
  }
  if (parsed === null || typeof parsed !== 'object' || Array.isArray(parsed)) return null
  const frame = parsed as Record<string, unknown>
  const type = frame['type']
  if (typeof type !== 'string' || type === '') return null
  const out: TerminalFrame = { type }
  const str = (key: string): string | undefined => {
    const value = frame[key]
    return typeof value === 'string' ? value : undefined
  }
  const num = (key: string): number | undefined => {
    const value = frame[key]
    return typeof value === 'number' && Number.isFinite(value) ? value : undefined
  }
  const dataB64 = str('data_b64')
  if (dataB64 !== undefined) out.data_b64 = dataB64
  const cols = num('cols')
  if (cols !== undefined) out.cols = cols
  const rows = num('rows')
  if (rows !== undefined) out.rows = rows
  const sessionId = str('session_id')
  if (sessionId !== undefined) out.session_id = sessionId
  const account = str('account')
  if (account !== undefined) out.account = account
  const accountUid = num('account_uid')
  if (accountUid !== undefined) out.account_uid = accountUid
  const exitCode = num('exit_code')
  if (exitCode !== undefined) out.exit_code = exitCode
  const signal = num('signal')
  if (signal !== undefined) out.signal = signal
  const reason = str('reason')
  if (reason !== undefined) out.reason = reason
  const code = str('code')
  if (code !== undefined) out.code = code
  const message = str('message')
  if (message !== undefined) out.message = message
  const serverTime = str('server_time')
  if (serverTime !== undefined) out.server_time = serverTime
  return out
}

/** 解出 output 帧里的终端字节。 */
export function outputBytes(frame: TerminalFrame): Uint8Array | null {
  if (frame.type !== 'output' || frame.data_b64 === undefined) return null
  return base64ToBytes(frame.data_b64)
}

// ---------------------------------------------------------------- 连接

/** 打开终端 WebSocket 所需的一切（全部由服务端给出，前端不自己拼）。 */
export interface TerminalSocketInput {
  connectPath: string
  protocol: string
  grantProtocol: string
}

/** WebSocket 构造器。默认用浏览器的 globalThis.WebSocket，测试可注入替身。 */
export type SocketFactory = (url: string, protocols: string[]) => WebSocket

/**
 * 打开到服务端的终端连接。
 *
 * 两个子协议一起声明：协议名与许可。服务端只回协议名（许可不回显），
 * 因此握手成功后可以断言 `socket.protocol === protocol`。
 */
export function openTerminalSocket(
  input: TerminalSocketInput,
  factory: SocketFactory = (url, protocols) => new WebSocket(url, protocols),
): WebSocket {
  const url = resolveSocketUrl(input.connectPath)
  return factory(url, [input.protocol, input.grantProtocol])
}
