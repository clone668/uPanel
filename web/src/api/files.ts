// 文件接口 DTO（GET /files/roots|list|text|download）。
//
// 字段与 api/openapi.yaml 的 FileRoot / FileEntry / FileListPage / FileText 逐字对应；
// 语义来源是 PLAN.md 第 15 节与 CONTRACTS.md 第 11 节。
//
// 五条纪律（每条都对应一条验收要求）：
//   1) 根来自服务端配置。界面不提供“输入任意路径”的入口，也不显示服务器上
//      未登记的目录；没有登记根时页面写“未登记任何文件根”，不写“浏览服务器”。
//   2) 路径一律是**根内相对路径**。向上导航到根就停下，拼路径时不做任何
//      `..` 之类的处理——服务端也不会接受。
//   3) 不跟随符号链接：符号链接在列表里标出自己的类型，点它不会进入目标。
//      “根外的东西看不到”这件事不能靠前端自觉，但界面也不该假装能进去。
//   4) 版本来自服务端并对已打开句柄核验。文本读取带上 expected_version，
//      不一致时服务端回 VERSION_CONFLICT，页面必须显示“文件已改变”。
//   5) 错误按 code 分支，不按 message：PATH_OUTSIDE_ROOT（越界）、
//      FILE_NOT_TEXT（不是文本）、ROOT_UNAVAILABLE（根不可用）、
//      VERSION_CONFLICT（版本变了）各有各的说明。

import { API_BASE, ApiError, get, type ApiResult } from './client'
import type { ServerErrorCode } from './types'

// ---------------------------------------------------------------- 常量

/** 文本读取上限（字节）。服务端是权威，这里只用于提前给出提示。 */
export const MAX_TEXT_BYTES = 1 << 20

/** 目录项类型闭集。 */
export const FILE_ENTRY_TYPES = ['file', 'dir', 'symlink', 'other'] as const

export type FileEntryType = (typeof FILE_ENTRY_TYPES)[number]

export const FILE_ENTRY_TYPE_LABELS: Record<FileEntryType, string> = {
  file: '文件',
  dir: '目录',
  symlink: '符号链接',
  other: '特殊文件',
}

export function fileEntryTypeLabel(type: string): string {
  return (FILE_ENTRY_TYPE_LABELS as Record<string, string | undefined>)[type] ?? type
}

// ---------------------------------------------------------------- DTO

/** 目录项的属主。只有数字 uid/gid：服务端不查账号名（NSS 可能连着外部服务）。 */
export interface FileOwner {
  uid: number
  gid: number
}

/** 一个目录项。size_bytes 是十进制字符串（大文件会超出 JS 安全整数范围）。 */
export interface FileEntry {
  name: string
  type: FileEntryType | string
  size_bytes: string
  /** 权限位八进制字符串，例如 "0644"。 */
  mode: string
  owner: FileOwner
  mtime: string
  version: string
}

/**
 * 一次目录列举。
 *
 * `next_cursor` 为 `null` 表示没有下一页——与 /jobs、/logs、/users 的列表形态一致。
 * 用 null 而不是空串，是因为“尾页”和“游标恰好是空”在 JSON 里不该长得一样。
 */
export interface FileListPage {
  root_id: string
  /** 根内相对路径；根自身是空串。 */
  path: string
  entries: FileEntry[]
  next_cursor: string | null
  /** 目录项太多，这次只看了前 N 项。界面必须显示这句话。 */
  truncated: boolean
}

/** 文件根所在文件系统的容量。字段与存储模块一致。 */
export interface FileCapacity {
  total_bytes: string
  available_bytes: string
  used_percent: number
  collected_at: string
}

/** 一个已登记的文件根。 */
export interface FileRoot {
  root_id: string
  name: string
  path: string
  writable: boolean
  available: boolean
  unavailable_reason?: string
  mount_point?: string
  fs_type?: string
  /** null 表示这次没读出来（原因见 capacity_error），不是“0 字节可用”。 */
  capacity: FileCapacity | null
  capacity_error?: string
}

export interface FileRootsResult {
  roots: FileRoot[]
}

export interface FileText {
  root_id: string
  path: string
  version: string
  size_bytes: string
  encoding: string
  content: string
}

export interface FileListQuery {
  root_id: string
  path: string
  cursor?: string
}

export interface FileTextQuery {
  root_id: string
  path: string
  expected_version?: string
}

export interface FileDownloadQuery {
  root_id: string
  path: string
  expected_version?: string
}

// ---------------------------------------------------------------- 调用

/** 列出已登记的根。 */
export function fetchFileRoots(signal?: AbortSignal): Promise<ApiResult<FileRootsResult>> {
  return get<FileRootsResult>('/files/roots', { ...(signal === undefined ? {} : { signal }) })
}

/** 列出根内一个目录。 */
export function fetchFileList(
  query: FileListQuery,
  signal?: AbortSignal,
): Promise<ApiResult<FileListPage>> {
  // 空值不拼进查询串：`cursor=` 这种"看起来翻页了其实没翻"的参数
  // 会让日志与抓包都难读，服务端也不需要它来表达"第一页"。
  const params: Record<string, string> = { root_id: query.root_id, path: query.path }
  if (query.cursor !== undefined && query.cursor !== '') params.cursor = query.cursor
  return get<FileListPage>('/files/list', {
    query: params,
    ...(signal === undefined ? {} : { signal }),
  })
}

/** 读取根内一个文件的文本。 */
export function fetchFileText(
  query: FileTextQuery,
  signal?: AbortSignal,
): Promise<ApiResult<FileText>> {
  const params: Record<string, string> = { root_id: query.root_id, path: query.path }
  if (query.expected_version !== undefined && query.expected_version !== '') {
    params.expected_version = query.expected_version
  }
  return get<FileText>('/files/text', {
    query: params,
    ...(signal === undefined ? {} : { signal }),
  })
}

/** 下载地址（供需要原生连接的场景使用；页面自身走 downloadFile）。 */
export function fileDownloadUrl(query: FileDownloadQuery): string {
  const params = new URLSearchParams({
    root_id: query.root_id,
    path: query.path,
  })
  if (query.expected_version) params.set('expected_version', query.expected_version)
  return `${API_BASE}/files/download?${params.toString()}`
}

/**
 * 下载一个文件。
 *
 * 为什么用 fetch 而不是直接开一个新窗口：下载失败时服务端回的是 JSON 错误信封
 * （越界是 PATH_OUTSIDE_ROOT，根不可用是 ROOT_UNAVAILABLE）。直接导航会把那段 JSON
 * 当成文件内容落到用户磁盘上，或者在一个新标签页里显示一坨 JSON。这里先读响应，
 * 失败了就抛 ApiError 交给页面展示，成功了再触发保存。
 *
 * 代价是内容会经过内存（Blob）。对面板要管理的配置文件、日志、脚本这类对象，
 * 这个代价换来的是“失败一定看得见”。
 */
export async function downloadFile(
  query: FileDownloadQuery,
  signal?: AbortSignal,
): Promise<{ fileName: string; sizeBytes: number }> {
  const url = fileDownloadUrl(query)
  let response: Response
  try {
    response = await fetch(url, {
      credentials: 'same-origin',
      ...(signal === undefined ? {} : { signal }),
    })
  } catch (error) {
    if (signal?.aborted) throw new ApiError({
      code: 'ABORTED',
      message: '下载已取消',
      status: 0,
      requestId: '',
      source: 'client',
    })
    const reason = error instanceof Error ? error.message : String(error)
    throw new ApiError({
      code: 'NETWORK_ERROR',
      message: `无法连接面板服务：${reason}`,
      status: 0,
      requestId: '',
      source: 'client',
    })
  }

  if (!response.ok) {
    // 错误信封与普通接口同形。
    let body: { error?: { code?: string; message?: string }; request_id?: string } | undefined
    try {
      body = (await response.json()) as typeof body
    } catch {
      body = undefined
    }
    const code = body?.error?.code
    if (code) {
      throw new ApiError({
        code: code as ServerErrorCode,
        message: body?.error?.message || '下载失败',
        status: response.status,
        requestId: body?.request_id ?? response.headers.get('X-Request-ID') ?? '',
        source: 'server',
      })
    }
    throw new ApiError({
      code: 'INVALID_RESPONSE',
      message: `服务返回了不符合契约的响应（HTTP ${response.status}）`,
      status: response.status,
      requestId: response.headers.get('X-Request-ID') ?? '',
      source: 'client',
    })
  }

  const blob = await response.blob()
  const fileName = baseName(query.path) || 'download'
  const objectUrl = URL.createObjectURL(blob)
  try {
    const anchor = document.createElement('a')
    anchor.href = objectUrl
    anchor.download = fileName
    anchor.rel = 'noopener'
    document.body.appendChild(anchor)
    anchor.click()
    anchor.remove()
  } finally {
    // 立刻释放会让部分浏览器来不及开始下载；延迟释放是常见做法。
    setTimeout(() => URL.revokeObjectURL(objectUrl), 10_000)
  }
  return { fileName, sizeBytes: blob.size }
}

// ---------------------------------------------------------------- 路径工具

/**
 * 拼根内相对路径。
 *
 * 只做拼接，不做任何 `..` 归一化：服务端不接受 `..`，前端也不该悄悄把它算掉——
 * 那会掩盖一个本来应该报错的输入。
 */
export function joinRelative(dir: string, name: string): string {
  if (dir === '') return name
  return `${dir}/${name}`
}

/** 上一级相对路径；到根时返回 null（根没有“上一级”）。 */
export function parentRelative(path: string): string | null {
  if (path === '') return null
  const index = path.lastIndexOf('/')
  return index < 0 ? '' : path.slice(0, index)
}

/** 文件名（相对路径的最后一段）；根自身返回根名。 */
export function baseName(path: string): string {
  if (path === '') return ''
  const index = path.lastIndexOf('/')
  return index < 0 ? path : path.slice(index + 1)
}

/** 面包屑：根 + 每一层。根那一层的 path 是空串。 */
export function breadcrumbs(path: string): { label: string; path: string }[] {
  const out: { label: string; path: string }[] = [{ label: '根', path: '' }]
  if (path === '') return out
  let current = ''
  for (const part of path.split('/')) {
    if (part === '') continue
    current = current === '' ? part : `${current}/${part}`
    out.push({ label: part, path: current })
  }
  return out
}

/** 字节数（十进制字符串）→ number，仅用于显示；超出安全范围时返回 null。 */
export function safeBytes(value: string): number | null {
  const parsed = Number(value)
  if (!Number.isFinite(parsed) || parsed < 0 || !Number.isSafeInteger(parsed)) return null
  return parsed
}

/** 这个目录项可以进入（只有目录）。符号链接不跟随，也不能进入。 */
export function canEnter(entry: FileEntry): boolean {
  return entry.type === 'dir'
}

/** 这个目录项可以按文本打开预览或按原样下载（只有普通文件）。 */
export function canRead(entry: FileEntry): boolean {
  return entry.type === 'file'
}

/** 这个目录项为什么不能打开（用于界面提示，而不是静默禁用）。 */
export function entryDisabledReason(entry: FileEntry): string | null {
  switch (entry.type) {
    case 'file':
    case 'dir':
      return null
    case 'symlink':
      return '符号链接不跟随：面板只读取根内的真实路径'
    case 'other':
      return '特殊文件（设备、管道、socket）不读取'
    default:
      return `未知类型 ${entry.type}，不打开`
  }
}
