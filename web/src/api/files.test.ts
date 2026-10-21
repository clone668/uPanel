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
  DEFAULT_MAX_UPLOAD_BYTES,
  FILE_ACTIONS,
  FILE_ACTION_LABELS,
  MAX_TEXT_BYTES,
  UPLOAD_STATES,
  breadcrumbs,
  canEnter,
  canRead,
  chmodParams,
  chownParams,
  commitUploadParams,
  copyParams,
  createFileUpload,
  deleteFileUpload,
  deleteParams,
  downloadFile,
  entryDisabledReason,
  fetchFileList,
  fetchFileText,
  fetchFileUpload,
  fileActionNeedsReauth,
  fileDownloadUrl,
  fileEntryTypeLabel,
  joinRelative,
  mkdirParams,
  moveParams,
  parentRelative,
  putFileUploadContent,
  safeBytes,
  uploadStateLabel,
  validMode,
  validRelativePath,
  writeTextParams,
  type FileAction,
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

// ---------------------------------------------------------------- P06.2

describe('暂存上传的 DTO', () => {
  it('默认上限是正的 2 GiB（位运算写法会溢出成负数，这里钉住）', () => {
    expect(DEFAULT_MAX_UPLOAD_BYTES).toBe(2147483648)
    expect(DEFAULT_MAX_UPLOAD_BYTES).toBeGreaterThan(0)
  })

  it('状态闭集只有 open/received/failed，文案不把 received 说成"文件已改"', () => {
    expect([...UPLOAD_STATES]).toEqual(['open', 'received', 'failed'])
    expect(uploadStateLabel('open')).toContain('等待')
    expect(uploadStateLabel('received')).toContain('尚未落地')
    expect(uploadStateLabel('received')).not.toContain('已落地')
    // 未知状态原样透出，不当成 open，也不猜。
    expect(uploadStateLabel('weird')).toBe('weird')
  })

  it('创建暂存：POST /files/uploads，字段名与 openapi 一致，size_bytes 是字符串', async () => {
    const mock = stubFetch(() =>
      Promise.resolve(
        dataResponse({
          upload_id: 'up_0123456789abcdef0123456789abcdef',
          root_id: 'etc',
          path: 'conf/app.conf',
          size_bytes: '512',
          received_bytes: '0',
          state: 'open',
          created_at: '2026-10-21T03:00:00Z',
          expires_at: '2026-10-21T04:00:00Z',
        }),
      ),
    )

    const result = await createFileUpload({
      root_id: 'etc',
      path: 'conf/app.conf',
      size_bytes: '512',
      expected_version: '',
    })

    expect(result.data.state).toBe('open')
    const { url, init } = lastCall(mock)
    expect(url).toBe(`${API_BASE}/files/uploads`)
    expect(init.method).toBe('POST')
    expect(JSON.parse(String(init.body))).toEqual({
      root_id: 'etc',
      path: 'conf/app.conf',
      size_bytes: '512',
      expected_version: '',
    })
  })

  it('送字节：PUT 到 /content，octet-stream + X-UPanel-Request，body 就是 Blob', async () => {
    const mock = stubFetch(() =>
      Promise.resolve(
        dataResponse({
          upload_id: 'up_0123456789abcdef0123456789abcdef',
          root_id: 'etc',
          path: 'conf/app.conf',
          size_bytes: '3',
          received_bytes: '3',
          state: 'received',
          created_at: '2026-10-21T03:00:00Z',
          expires_at: '2026-10-21T04:00:00Z',
        }),
      ),
    )
    const blob = new Blob(['abc'])

    const result = await putFileUploadContent('up_0123456789abcdef0123456789abcdef', blob)

    expect(result.data.state).toBe('received')
    expect(result.data.received_bytes).toBe('3')
    const { url, init } = lastCall(mock)
    expect(url).toBe(`${API_BASE}/files/uploads/up_0123456789abcdef0123456789abcdef/content`)
    expect(init.method).toBe('PUT')
    expect(init.body).toBe(blob)
    const headers = init.headers as Record<string, string>
    expect(headers['Content-Type']).toBe('application/octet-stream')
    // 面板的写接口都要求这个头：没有它服务端按跨站请求拒绝。
    expect(headers['X-UPanel-Request']).toBe('1')
    // 不设默认超时：大文件不该被客户端的 30 秒上限掐断，时限由服务端空闲上限决定。
    expect(init.signal).toBeUndefined()
  })

  it('upload_id 里的特殊字符会被转义，不会拼出另一条路径', async () => {
    const mock = stubFetch(() =>
      Promise.resolve(
        dataResponse({
          upload_id: 'up_x',
          root_id: 'etc',
          path: 'a',
          size_bytes: '0',
          received_bytes: '0',
          state: 'open',
          created_at: '2026-10-21T03:00:00Z',
          expires_at: '2026-10-21T04:00:00Z',
        }),
      ),
    )

    await fetchFileUpload('../jobs')

    expect(lastCall(mock).url).toBe(`${API_BASE}/files/uploads/..%2Fjobs`)
  })

  it('查询与放弃：GET 一次、DELETE 一次，都不带查询串', async () => {
    const mockGet = stubFetch(() =>
      Promise.resolve(
        dataResponse({
          upload_id: 'up_1',
          root_id: 'etc',
          path: 'a',
          size_bytes: '0',
          received_bytes: '0',
          state: 'open',
          created_at: '2026-10-21T03:00:00Z',
          expires_at: '2026-10-21T04:00:00Z',
        }),
      ),
    )
    await fetchFileUpload('up_1')
    expect(lastCall(mockGet).url).toBe(`${API_BASE}/files/uploads/up_1`)

    const mockDelete = stubFetch(() =>
      Promise.resolve(dataResponse({ upload_id: 'up_1', state: 'deleted' })),
    )
    const result = await deleteFileUpload('up_1')
    expect(result.data.state).toBe('deleted')
    expect(lastCall(mockDelete).init.method).toBe('DELETE')
  })

  it('暂存失败按 code 与状态码透出（不新增错误码，判决复用已有码）', async () => {
    // CONTRACTS 11.2：没有为上传新增错误码。目标已存在 / 上传状态不对 / 版本不符
    // 都回 VERSION_CONFLICT(409)；盘满与超限是 413 但复用 VALIDATION_ERROR
    // （请求本身没错，是体积这台机器现在接不住）；根不可用是 ROOT_UNAVAILABLE。
    const cases: { code: string; status: number }[] = [
      { code: 'VERSION_CONFLICT', status: 409 },
      { code: 'VALIDATION_ERROR', status: 413 },
      { code: 'ROOT_UNAVAILABLE', status: 409 },
    ]

    for (const item of cases) {
      stubFetch(() => Promise.resolve(errorResponse(item.code, item.status)))
      await expect(
        createFileUpload({ root_id: 'etc', path: 'a', size_bytes: '1', expected_version: '' }),
      ).rejects.toMatchObject({ code: item.code, status: item.status, source: 'server' })
    }
  })
})

describe('写动作的参数拼装', () => {
  it('每个动作名都在标签表里有中文说法（缺一个就说明契约没对齐）', () => {
    for (const name of Object.values(FILE_ACTIONS)) {
      expect(FILE_ACTION_LABELS[name]).toBeTruthy()
    }
    expect(Object.keys(FILE_ACTION_LABELS)).toHaveLength(Object.keys(FILE_ACTIONS).length)
  })

  it('file.mkdir 只带根与相对路径', () => {
    expect(mkdirParams({ root_id: 'etc', relative_path: 'logs' })).toEqual({
      root_id: 'etc',
      relative_path: 'logs',
    })
  })

  it('file.write_text 必须带 expected_version（覆盖要自证看过旧版本）', () => {
    expect(
      writeTextParams({
        root_id: 'etc',
        relative_path: 'app.conf',
        expected_version: 'v1-1-2-3-4',
        content: 'a=1\n',
      }),
    ).toEqual({
      root_id: 'etc',
      relative_path: 'app.conf',
      expected_version: 'v1-1-2-3-4',
      content: 'a=1\n',
    })
  })

  it('file.commit_upload 带 upload_id、overwrite 与目标版本', () => {
    expect(
      commitUploadParams({ upload_id: 'up_1', overwrite: true, expected_version: 'v1-1-2-3-4' }),
    ).toEqual({ upload_id: 'up_1', overwrite: true, expected_version: 'v1-1-2-3-4' })
  })

  it('file.copy 与 file.move 参数形状相同（源与目标各一套根/路径）', () => {
    const params = {
      source_root_id: 'etc',
      source_path: 'a.conf',
      target_root_id: 'srv',
      target_path: 'b.conf',
      expected_version: 'v1-1-2-3-4',
    }
    expect(copyParams(params)).toEqual(params)
    expect(moveParams(params)).toEqual(params)
  })

  it('file.delete 带 recursive，file.chmod 带 mode，file.chown 带受管理账号/组 id', () => {
    expect(
      deleteParams({
        root_id: 'etc',
        relative_path: 'a',
        expected_version: 'v1-1-2-3-4',
        recursive: true,
      }),
    ).toEqual({
      root_id: 'etc',
      relative_path: 'a',
      expected_version: 'v1-1-2-3-4',
      recursive: true,
    })
    expect(
      chmodParams({ root_id: 'etc', relative_path: 'a', expected_version: '', mode: '0644' }),
    ).toEqual({ root_id: 'etc', relative_path: 'a', expected_version: '', mode: '0644' })
    expect(
      chownParams({
        root_id: 'etc',
        relative_path: 'a',
        expected_version: '',
        managed_account_id: 7,
        managed_group_id: 9,
      }),
    ).toEqual({
      root_id: 'etc',
      relative_path: 'a',
      expected_version: '',
      managed_account_id: 7,
      managed_group_id: 9,
    })
  })

  it('参数里没有意外字段：前端只拼契约里的键，不往里塞界面状态', () => {
    const allowed: Record<FileAction, string[]> = {
      'file.mkdir': ['root_id', 'relative_path'],
      'file.write_text': ['root_id', 'relative_path', 'expected_version', 'content'],
      'file.commit_upload': ['upload_id', 'overwrite', 'expected_version'],
      'file.copy': ['source_root_id', 'source_path', 'target_root_id', 'target_path', 'expected_version'],
      'file.move': ['source_root_id', 'source_path', 'target_root_id', 'target_path', 'expected_version'],
      'file.delete': ['root_id', 'relative_path', 'expected_version', 'recursive'],
      'file.chmod': ['root_id', 'relative_path', 'expected_version', 'mode'],
      'file.chown': ['root_id', 'relative_path', 'expected_version', 'managed_account_id', 'managed_group_id'],
    }
    const built: Record<FileAction, Record<string, unknown>> = {
      'file.mkdir': mkdirParams({ root_id: 'r', relative_path: 'p' }),
      'file.write_text': writeTextParams({
        root_id: 'r',
        relative_path: 'p',
        expected_version: 'v',
        content: 'c',
      }),
      'file.commit_upload': commitUploadParams({
        upload_id: 'u',
        overwrite: false,
        expected_version: '',
      }),
      'file.copy': copyParams({
        source_root_id: 'r',
        source_path: 'p',
        target_root_id: 'r',
        target_path: 'q',
        expected_version: 'v',
      }),
      'file.move': moveParams({
        source_root_id: 'r',
        source_path: 'p',
        target_root_id: 'r',
        target_path: 'q',
        expected_version: 'v',
      }),
      'file.delete': deleteParams({
        root_id: 'r',
        relative_path: 'p',
        expected_version: 'v',
        recursive: false,
      }),
      'file.chmod': chmodParams({
        root_id: 'r',
        relative_path: 'p',
        expected_version: 'v',
        mode: '0644',
      }),
      'file.chown': chownParams({
        root_id: 'r',
        relative_path: 'p',
        expected_version: 'v',
        managed_account_id: 1,
        managed_group_id: 2,
      }),
    }

    for (const [action, params] of Object.entries(built)) {
      expect(Object.keys(params).sort()).toEqual([...(allowed[action as FileAction] ?? [])].sort())
    }
  })

  it('再认证提示与契约一致：删除/权限/所有者/落地上传要再认证，新建与保存不要', () => {
    expect(fileActionNeedsReauth('file.delete')).toBe(true)
    expect(fileActionNeedsReauth('file.chmod')).toBe(true)
    expect(fileActionNeedsReauth('file.chown')).toBe(true)
    expect(fileActionNeedsReauth('file.commit_upload')).toBe(true)
    expect(fileActionNeedsReauth('file.mkdir')).toBe(false)
    expect(fileActionNeedsReauth('file.write_text')).toBe(false)
    expect(fileActionNeedsReauth('file.move')).toBe(false)
    expect(fileActionNeedsReauth('file.copy')).toBe(false)
  })
})

describe('模式与路径的形状校验', () => {
  it('只接受四位八进制且只含普通权限位', () => {
    expect(validMode('0644')).toBe(true)
    expect(validMode('0755')).toBe(true)
    expect(validMode('0600')).toBe(true)
    expect(validMode('0000')).toBe(true)
    // 三位、五位、前缀、非八进制、setuid/setgid/sticky 都要被拒。
    expect(validMode('644')).toBe(false)
    expect(validMode('00644')).toBe(false)
    expect(validMode('0o644')).toBe(false)
    expect(validMode('0899')).toBe(false)
    expect(validMode('4755')).toBe(false)
    expect(validMode('2755')).toBe(false)
    expect(validMode('1755')).toBe(false)
    expect(validMode('')).toBe(false)
  })

  it('根内相对路径：不空、不以 / 开头、不含 . 与 .. 段', () => {
    expect(validRelativePath('app.conf')).toBe(true)
    expect(validRelativePath('conf/app.conf')).toBe(true)
    expect(validRelativePath('')).toBe(false)
    expect(validRelativePath('/etc/passwd')).toBe(false)
    expect(validRelativePath('a//b')).toBe(false)
    expect(validRelativePath('a/./b')).toBe(false)
    expect(validRelativePath('a/../b')).toBe(false)
    expect(validRelativePath('..')).toBe(false)
    expect(validRelativePath('a\u0000b')).toBe(false)
  })
})
