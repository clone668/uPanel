// 文件 DTO 与路径工具的纯逻辑用例（P06.1）。
//
// 这里不渲染页面，只验四件容易出错、且每件都对应一条验收要求的事：
//   1) 路径怎么拼——只做拼接，不做 `..` 归一化（归一化会掩盖本该报错的输入）；
//   2) 哪些条目能进/能读——符号链接与特殊文件必须被明确拒绝，理由要说出来；
//   3) 查询参数怎么拼——空 cursor / 空 expected_version 不变成 `cursor=` 这种噪声；
//   4) 错误按 code 分支，且下载失败时**不落盘**（不把 JSON 错误当文件内容保存）。

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { ApiError, API_BASE, setCsrfTokenProvider } from './client'
import {
  MAX_TEXT_BYTES,
  breadcrumbs,
  canEnter,
  canRead,
  downloadFile,
  entryDisabledReason,
  fetchFileList,
  fetchFileText,
  fileDownloadUrl,
  fileEntryTypeLabel,
  joinRelative,
  parentRelative,
  safeBytes,
  type FileEntry,
} from './files'

type FetchMock = ReturnType<typeof vi.fn>

function stubFetch(
  impl: (input: RequestInfo | URL, init?: RequestInit) => Promise<Response>,
): FetchMock {
  const mock = vi.fn(impl)
  vi.stubGlobal('fetch', mock)
  return mock
}

function dataResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify({ data: body, request_id: 'rid-files' }), {
    status,
    headers: { 'Content-Type': 'application/json', 'X-Request-ID': 'header-rid' },
  })
}

function errorResponse(code: string, status: number, message = '被拒绝'): Response {
  return new Response(JSON.stringify({ error: { code, message }, request_id: 'rid-err' }), {
    status,
    headers: { 'Content-Type': 'application/json', 'X-Request-ID': 'header-rid' },
  })
}

function lastCall(mock: FetchMock): { url: string; init: RequestInit } {
  const call = mock.mock.calls.at(-1)
  if (!call) throw new Error('fetch 未被调用')
  return { url: String(call[0]), init: (call[1] ?? {}) as RequestInit }
}

/** 一个真实形状的目录项：version 是服务端从已打开句柄算出来的串。 */
function entry(overrides: Partial<FileEntry> = {}): FileEntry {
  return {
    name: 'sshd_config',
    type: 'file',
    size_bytes: '3222',
    mode: '0644',
    owner: { uid: 0, gid: 0 },
    mtime: '2026-10-21T02:59:00Z',
    version: 'v1-a1b2-c3d4-e5f6-17a0b1c2d3e4',
    ...overrides,
  }
}

beforeEach(() => {
  setCsrfTokenProvider(() => null)
})

afterEach(() => {
  vi.unstubAllGlobals()
  vi.restoreAllMocks()
})

describe('路径工具', () => {
  it('拼接只加斜杠，到根时就是名字本身', () => {
    expect(joinRelative('', 'nginx')).toBe('nginx')
    expect(joinRelative('nginx', 'sites')).toBe('nginx/sites')
  })

  it('拼接不做 `..` 归一化：那是服务端要拒绝的输入，前端不该悄悄算掉', () => {
    expect(joinRelative('a', '..')).toBe('a/..')
    expect(joinRelative('a/..', 'b')).toBe('a/../b')
  })

  it('上一级到根就停下（根没有上一级）', () => {
    expect(parentRelative('')).toBeNull()
    expect(parentRelative('nginx')).toBe('')
    expect(parentRelative('nginx/sites/enabled')).toBe('nginx/sites')
  })

  it('面包屑是根 + 每层，路径逐层累计', () => {
    expect(breadcrumbs('')).toEqual([{ label: '根', path: '' }])
    expect(breadcrumbs('nginx/sites')).toEqual([
      { label: '根', path: '' },
      { label: 'nginx', path: 'nginx' },
      { label: 'sites', path: 'nginx/sites' },
    ])
  })

  it('字节串超出安全整数范围时返回 null，界面因此显示 — 而不是一个假数字', () => {
    expect(safeBytes('0')).toBe(0)
    expect(safeBytes('3222')).toBe(3222)
    expect(safeBytes('9007199254740993')).toBeNull()
    expect(safeBytes('12abc')).toBeNull()
    expect(safeBytes('-1')).toBeNull()
  })

  it('文本上限与契约一致：1 MiB', () => {
    expect(MAX_TEXT_BYTES).toBe(1048576)
  })
})

describe('条目判定', () => {
  it('只有目录能进入，只有普通文件能读', () => {
    expect(canEnter(entry({ type: 'dir', size_bytes: '0' }))).toBe(true)
    expect(canRead(entry({ type: 'dir', size_bytes: '0' }))).toBe(false)
    expect(canRead(entry())).toBe(true)
    expect(canEnter(entry())).toBe(false)
  })

  it('符号链接既不进入也不读取，并给出理由', () => {
    const link = entry({ type: 'symlink', name: 'shadow' })
    expect(canEnter(link)).toBe(false)
    expect(canRead(link)).toBe(false)
    expect(entryDisabledReason(link)).toContain('符号链接不跟随')
  })

  it('特殊文件（设备、管道、socket）不读取', () => {
    const fifo = entry({ type: 'other', name: 'fifo' })
    expect(canRead(fifo)).toBe(false)
    expect(entryDisabledReason(fifo)).toContain('特殊文件')
  })

  it('普通文件与目录没有禁用理由', () => {
    expect(entryDisabledReason(entry())).toBeNull()
    expect(entryDisabledReason(entry({ type: 'dir' }))).toBeNull()
  })

  it('未知类型不猜测它的行为', () => {
    expect(entryDisabledReason(entry({ type: 'socket' }))).toContain('socket')
    expect(fileEntryTypeLabel('socket')).toBe('socket')
    expect(fileEntryTypeLabel('dir')).toBe('目录')
  })
})

describe('fetchFileList / fetchFileText', () => {
  it('不带 cursor 时查询串里没有 cursor 这个键', async () => {
    const mock = stubFetch(() =>
      Promise.resolve(
        dataResponse({
          root_id: 'etc',
          path: '',
          entries: [],
          next_cursor: null,
          truncated: false,
        }),
      ),
    )

    const result = await fetchFileList({ root_id: 'etc', path: '' })

    expect(result.data.root_id).toBe('etc')
    const { url } = lastCall(mock)
    expect(url).toBe(`${API_BASE}/files/list?root_id=etc&path=`)
    expect(url).not.toContain('cursor')
  })

  it('带 cursor 时原样带上（游标只在同一窗口内有效，不做任何改写）', async () => {
    const mock = stubFetch(() =>
      Promise.resolve(
        dataResponse({ root_id: 'etc', path: 'nginx', entries: [], next_cursor: null, truncated: true }),
      ),
    )

    await fetchFileList({ root_id: 'etc', path: 'nginx', cursor: 'sshd_config' })

    const { url } = lastCall(mock)
    expect(url).toContain('cursor=sshd_config')
    expect(url).toContain('path=nginx')
  })

  it('文本读取带 expected_version 时服务端才能发现"文件已改变"', async () => {
    const mock = stubFetch(() =>
      Promise.resolve(
        dataResponse({
          root_id: 'etc',
          path: 'hostname',
          version: 'v1-1-2-3-4',
          size_bytes: '12',
          encoding: 'utf-8',
          content: 'upanel\n',
        }),
      ),
    )

    const result = await fetchFileText({
      root_id: 'etc',
      path: 'hostname',
      expected_version: 'v1-1-2-3-4',
    })

    expect(result.data.content).toBe('upanel\n')
    expect(lastCall(mock).url).toContain('expected_version=v1-1-2-3-4')
  })

  it('不带 expected_version 时查询串里没有这个键', async () => {
    const mock = stubFetch(() =>
      Promise.resolve(
        dataResponse({
          root_id: 'etc',
          path: 'hostname',
          version: 'v1-1-2-3-4',
          size_bytes: '0',
          encoding: 'utf-8',
          content: '',
        }),
      ),
    )

    await fetchFileText({ root_id: 'etc', path: 'hostname' })

    expect(lastCall(mock).url).not.toContain('expected_version')
  })

  it('越界与版本冲突原样透出成 ApiError，页面据此分支', async () => {
    const cases: { code: string; status: number }[] = [
      { code: 'PATH_OUTSIDE_ROOT', status: 422 },
      { code: 'VERSION_CONFLICT', status: 409 },
      { code: 'ROOT_UNAVAILABLE', status: 409 },
      { code: 'FILE_NOT_TEXT', status: 422 },
      { code: 'NOT_FOUND', status: 404 },
    ]

    for (const item of cases) {
      stubFetch(() => Promise.resolve(errorResponse(item.code, item.status)))
      await expect(fetchFileText({ root_id: 'etc', path: 'x' })).rejects.toMatchObject({
        code: item.code,
        status: item.status,
        source: 'server',
      })
    }
  })
})

describe('fileDownloadUrl', () => {
  it('只有 root_id 与 path 是必需的，expected_version 可选', () => {
    expect(fileDownloadUrl({ root_id: 'etc', path: 'ssh/sshd_config' })).toBe(
      `${API_BASE}/files/download?root_id=etc&path=ssh%2Fsshd_config`,
    )
    expect(
      fileDownloadUrl({ root_id: 'etc', path: 'a b', expected_version: 'v1-1-2-3-4' }),
    ).toContain('expected_version=v1-1-2-3-4')
  })
})

describe('downloadFile', () => {
  interface Anchor {
    href: string
    download: string
    rel: string
    click: () => void
    remove: () => void
  }

  let anchors: Anchor[]
  let revoked: string[]

  beforeEach(() => {
    anchors = []
    revoked = []
    const createObjectURL = vi.fn(() => 'blob:mock')
    const revokeObjectURL = vi.fn((url: string) => {
      revoked.push(url)
    })
    Object.defineProperty(URL, 'createObjectURL', { value: createObjectURL, configurable: true })
    Object.defineProperty(URL, 'revokeObjectURL', { value: revokeObjectURL, configurable: true })
    vi.stubGlobal('document', {
      createElement: () => {
        const anchor: Anchor = {
          href: '',
          download: '',
          rel: '',
          click: () => undefined,
          remove: () => undefined,
        }
        anchors.push(anchor)
        return anchor
      },
      body: { appendChild: () => undefined },
    })
  })

  it('成功后触发保存，文件名取自根内相对路径的最后一段', async () => {
    const payload = 'Port 22\nPermitRootLogin no\n'
    const mock = stubFetch(
      () =>
        Promise.resolve(
          new Response(payload, {
            status: 200,
            headers: { 'Content-Type': 'application/octet-stream' },
          }),
        ),
    )

    const result = await downloadFile({ root_id: 'etc', path: 'ssh/sshd_config' })

    expect(result.fileName).toBe('sshd_config')
    expect(result.sizeBytes).toBe(payload.length)
    expect(anchors).toHaveLength(1)
    expect(anchors[0]?.download).toBe('sshd_config')
    expect(lastCall(mock).url).toContain('/files/download?')
  })

  it('失败时抛 ApiError，且不创建任何下载链接（错误 JSON 不会被当成文件保存）', async () => {
    stubFetch(() => Promise.resolve(errorResponse('PATH_OUTSIDE_ROOT', 422, '路径越界')))

    await expect(
      downloadFile({ root_id: 'etc', path: '../../etc/shadow' }),
    ).rejects.toMatchObject({ code: 'PATH_OUTSIDE_ROOT', status: 422 })
    expect(anchors).toHaveLength(0)
  })

  it('根不可用时同样是 ApiError，而不是一个坏掉的文件', async () => {
    stubFetch(() => Promise.resolve(errorResponse('ROOT_UNAVAILABLE', 409)))

    const error = await downloadFile({ root_id: 'etc', path: 'hostname' }).catch((e) => e)
    expect(error).toBeInstanceOf(ApiError)
    expect(error.code).toBe('ROOT_UNAVAILABLE')
    expect(anchors).toHaveLength(0)
  })

  it('网络层失败归为 NETWORK_ERROR（客户端判定，不是服务端错误码）', async () => {
    stubFetch(() => Promise.reject(new TypeError('Failed to fetch')))

    await expect(downloadFile({ root_id: 'etc', path: 'hostname' })).rejects.toMatchObject({
      code: 'NETWORK_ERROR',
      source: 'client',
    })
  })

  it('取消下载时不假装成功', async () => {
    const controller = new AbortController()
    stubFetch(() => {
      controller.abort()
      return Promise.reject(new DOMException('aborted', 'AbortError'))
    })

    await expect(
      downloadFile({ root_id: 'etc', path: 'hostname' }, controller.signal),
    ).rejects.toMatchObject({ code: 'ABORTED' })
  })
})
