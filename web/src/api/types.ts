// 契约类型：与 CONTRACTS.md 第 1 节一一对应。
//
// 规则（不得违反）：
//   - 字段名保持 snake_case，不做驼峰转换；
//   - 字节数等大计数器是十进制字符串，不用 number 承载；
//   - 服务端错误码是封闭枚举，前端按 code 分支，不按 message 分支。

/** 服务端错误码（CONTRACTS.md 第 1 节的封闭枚举，26 个）。 */
export const SERVER_ERROR_CODES = [
  'VALIDATION_ERROR',
  'UNAUTHENTICATED',
  'FORBIDDEN',
  'REAUTH_REQUIRED',
  'CSRF_FAILED',
  'NOT_FOUND',
  'VERSION_CONFLICT',
  'RESOURCE_BUSY',
  'DEVICE_IN_USE',
  'PROTECTED_DEVICE',
  'UNSUPPORTED_DEVICE',
  'DEVICE_IDENTITY_CHANGED',
  'PATH_OUTSIDE_ROOT',
  'DEPENDENCY_BLOCKED',
  'PREFLIGHT_EXPIRED',
  'PREFLIGHT_STALE',
  'CONFIRMATION_MISMATCH',
  'IDEMPOTENCY_CONFLICT',
  'AGENT_UNAVAILABLE',
  'DEPENDENCY_MISSING',
  'NOT_IMPLEMENTED',
  'COMMAND_FAILED',
  'OPERATION_INTERRUPTED',
  'ROLLBACK_FAILED',
  'ROOT_UNAVAILABLE',
  'FILE_NOT_TEXT',
] as const

export type ServerErrorCode = (typeof SERVER_ERROR_CODES)[number]

/**
 * 客户端自造的错误码。它们**永远不会**由服务端返回，只描述“请求没走完”。
 * 与 ServerErrorCode 分开，避免前端把网络故障当成业务错误处理。
 */
export type ClientErrorCode = 'NETWORK_ERROR' | 'INVALID_RESPONSE' | 'ABORTED'

export type ApiErrorCode = ServerErrorCode | ClientErrorCode

export interface DataEnvelope<T> {
  data: T
  request_id: string
}

export interface ErrorBody {
  code: ServerErrorCode
  message: string
  details?: unknown
}

export interface ErrorEnvelope {
  error: ErrorBody
  request_id: string
}

/** 列表响应的 data 形状（CONTRACTS.md 第 1 节）。 */
export interface ListData<T> {
  items: T[]
  next_cursor: string | null
}

export function isServerErrorCode(value: string): value is ServerErrorCode {
  return (SERVER_ERROR_CODES as readonly string[]).includes(value)
}
