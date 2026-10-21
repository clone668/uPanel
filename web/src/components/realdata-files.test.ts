// 用真机响应回放文件页组件（P06.1）。
//
// 这里的 JSON 不是造的：它们是验收 VM（upanel-2404，Ubuntu 24.04.5，ext4 + tmpfs）上
// 真实运行 upanel-web（production 模式、root 代理、openat2 边界）之后，由
// tests/integration/p06-1-files-acceptance.sh 抓回来的响应原样存在
// src/fixtures/p06-1-vm/ 下（连 {data, request_id} / {error, request_id} 外壳都没拆）。
//
// 与 files.test.ts 的分工：那边是**手写形状样本**，验“每种状态怎么画”；这里验
// “服务端真的回了这些字节，组件能不能吃掉”。两者不能互相顶替——手写样本能覆盖
// 真机没造出来的形态，真机样本能抓到手写样本里被我写错的假设。本轮就抓到过：
// 符号链接目录被内核报成 ENOTDIR，我一度把它归成 VALIDATION_ERROR，手写样本里
// 那条错误码是我自己写的，所以永远对得上。
//
// 页面级（FilesView 取数后）的渲染在 SSR 里驱动不了：onMounted 在服务端渲染时
// 不执行。所以这一层回放的是**纯输入的组件**（FileEntryRow、ApiErrorPanel）与
// **真实载荷自身的不变量**（version 形状、游标与页边界），取数后的状态机由
// files.test.ts 与 VM 上的验收脚本负责。

import { readFileSync } from 'node:fs'

import { describe, expect, it, vi } from 'vitest'
import { createSSRApp, h, type Component } from 'vue'
import { renderToString } from 'vue/server-renderer'

import { ApiError } from '@/api/client'
import {
  canEnter,
  canRead,
  entryDisabledReason,
  type FileEntry,
  type FileListPage,
  type FileRoot,
  type FileText,
} from '@/api/files'
import ApiErrorPanel from './ApiErrorPanel.vue'
import FileEntryRow from './FileEntryRow.vue'

// 组件里用到的 naive-ui 控件在 SSR 里会去挂样式（需要 document）。这里只保留文字，
// 让断言看的是“显示成什么话”，不是“挂了哪些样式”。
vi.mock('naive-ui', async () => {
  const { h } = await import('vue')
  const stub = (name: string) => ({
    name: `Stub${name}`,
    props: ['title', 'description', 'status', 'code', 'disabled'],
    setup(
      props: { title?: unknown; description?: unknown; disabled?: unknown },
      ctx: { slots: { default?: () => unknown } },
    ) {
      return () => {
        const children: unknown[] = []
        if (typeof props.title === 'string') children.push(props.title)
        if (typeof props.description === 'string') children.push(props.description)
        const slot = ctx.slots.default?.()
        if (slot !== undefined) children.push(slot)
        const attrs: Record<string, unknown> = { 'data-naive': name }
        if (props.disabled === true) attrs['data-disabled'] = 'true'
        return h('div', attrs, children as never)
      }
    },
  })
  return {
    NAlert: stub('alert'),
    NButton: stub('button'),
    NTag: stub('tag'),
  }
})

function readText(name: string): string {
  return readFileSync(new URL(`../fixtures/p06-1-vm/${name}`, import.meta.url), 'utf8')
}

function readJson(name: string): Record<string, unknown> {
  return JSON.parse(readText(name)) as Record<string, unknown>
}

function payloadPage(name: string): FileListPage {
  return readJson(name)['data'] as FileListPage
}

function payloadRoots(name: string): FileRoot[] {
  return (readJson(name)['data'] as { roots: FileRoot[] }).roots
}

function payloadText(name: string): FileText {
  return readJson(name)['data'] as FileText
}

/** 把真实错误信封还原成页面拿到的 ApiError（status 取验收脚本记录的实测码）。 */
function payloadError(name: string, status: number): ApiError {
  const body = readJson(name)
  const error = body['error'] as { code: string; message: string }
  return new ApiError({
    code: error.code as never,
    message: error.message,
    status,
    requestId: body['request_id'] as string,
    source: 'server',
  })
}

async function render(component: Component, props: Record<string, unknown>): Promise<string> {
  const app = createSSRApp({ render: () => h(component, props) })
  const html = await renderToString(app)
  // scoped 样式加的 data-v-* 只作用于 DOM，留着它会让断言变成测编译产物。
  return html.replace(/ data-v-[0-9a-z]+(?=[ >])/g, '')
}

/** CONTRACTS 11.1 的版本形状。 */
const VERSION = /^v1-[0-9a-f]+-[0-9a-f]+-[0-9a-f]+-[0-9a-f]+$/

/** 这些字符串一旦出现在界面文案里，就说明有根外内容漏进了响应。 */
const LEAK_WORDS = ['/etc/shadow', 'root:x:', 'do-not-leak']

describe('真机载荷：文件根', () => {
  it('三个根都是有具体路径的目录，没有一个是 “/”（否则等于可管理整个服务器）', () => {
    const roots = payloadRoots('files-roots.json')

    expect(roots.map((root) => root.root_id)).toEqual(['work', 'flip', 'shm'])
    for (const root of roots) {
      expect(root.path.startsWith('/')).toBe(true)
      expect(root.path).not.toBe('/')
      expect(root.available).toBe(true)
      expect(root.writable).toBe(false) // P06.1 只有读
    }
  })

  it('挂载点与文件系统类型来自真实 mountinfo：/dev/shm 的根报 tmpfs，不是继承 ext4', () => {
    const roots = payloadRoots('files-roots.json')
    const work = roots.find((root) => root.root_id === 'work')
    const shm = roots.find((root) => root.root_id === 'shm')

    expect(work?.fs_type).toBe('ext4')
    expect(work?.mount_point).toBe('/')
    expect(shm?.fs_type).toBe('tmpfs')
    expect(shm?.mount_point).toBe('/dev/shm')
    // 容量是十进制字符串，且 tmpfs 的 total 与 / 的 total 不同：
    // 如果这条读的是根目录而不是根自己所在的挂载点，两者会相等。
    expect(work?.capacity?.total_bytes).toMatch(/^[0-9]+$/)
    expect(shm?.capacity?.total_bytes).toMatch(/^[0-9]+$/)
    expect(shm?.capacity?.total_bytes).not.toBe(work?.capacity?.total_bytes)
  })

  it('根被替换之后的载荷把 “flip” 标成不可用，且给出原因（不显示成空目录）', () => {
    const roots = payloadRoots('files-roots-after.json')
    const flip = roots.find((root) => root.root_id === 'flip')

    expect(flip).toBeDefined()
    expect(flip?.available).toBe(false)
    expect(typeof flip?.unavailable_reason).toBe('string')
    expect(flip?.unavailable_reason).not.toBe('')
  })
})

describe('真机载荷：目录项', () => {
  const entries = payloadPage('files-list-root.json').entries

  it('每一项的 type 都在闭集内，version 与 mode 形状合法', () => {
    for (const entry of entries) {
      expect(['file', 'dir', 'symlink', 'other']).toContain(entry.type)
      expect(entry.version).toMatch(VERSION)
      // 四位八进制：产品早期返回过 5 位（"0o644" 风格），VM 上才发现。
      expect(entry.mode).toMatch(/^[0-7]{4}$/)
      expect(entry.size_bytes).toMatch(/^[0-9]+$/)
      expect(entry.owner.uid).toBeGreaterThanOrEqual(0)
    }
  })

  it('符号链接与 FIFO 都在真实载荷里，且前端不给任何读取入口', () => {
    const symlink = entries.find((entry) => entry.type === 'symlink')
    const fifo = entries.find((entry) => entry.type === 'other')

    expect(symlink).toBeDefined()
    expect(fifo).toBeDefined()
    expect(canEnter(symlink as FileEntry)).toBe(false)
    expect(canRead(symlink as FileEntry)).toBe(false)
    expect(canRead(fifo as FileEntry)).toBe(false)
    expect(entryDisabledReason(symlink as FileEntry)).toContain('不跟随')
    expect(entryDisabledReason(fifo as FileEntry)).toContain('不读取')
  })

  it('每一个真实目录项都能渲染成一行，类型标签与载荷一致', async () => {
    const labels: Record<string, string> = {
      file: '文件',
      dir: '目录',
      symlink: '符号链接',
      other: '特殊文件',
    }

    for (const entry of entries) {
      const html = await render(FileEntryRow, { entry })
      expect(html).toContain(entry.name)
      expect(html).toContain(labels[entry.type] as string)
      // 目录项的名字里不会有根外路径；真出现就是代理越界了。
      for (const word of LEAK_WORDS) expect(html).not.toContain(word)
    }
  })

  it('超过 1 MiB 的真实文件：给下载，不给预览', async () => {
    const big = entries.find((entry) => entry.name === 'big.log')
    expect(big).toBeDefined()

    const html = await render(FileEntryRow, { entry: big as FileEntry })
    expect(html).toContain('只能下载')
    expect(html).toContain('下载')
  })
})

describe('真机载荷：分页', () => {
  const first = payloadPage('files-list-page1.json')
  const tail = payloadPage('files-list-page2.json')

  it('page1 的游标就是本页最后一个名字，page2 从它之后开始（键集分页）', () => {
    expect(first.entries).toHaveLength(200)
    expect(typeof first.next_cursor).toBe('string')
    expect(first.next_cursor).toBe(first.entries[first.entries.length - 1]?.name)

    expect(tail.entries).toHaveLength(50)
    // page-200 紧接 page-199：没有重叠，也没有跳过。
    expect(first.next_cursor).toBe('page-199.txt')
    expect(tail.entries[0]?.name).toBe('page-200.txt')
  })

  it('尾页的 next_cursor 是 null（不是空串），truncated=false', () => {
    expect(tail.next_cursor).toBeNull()
    expect(tail.truncated).toBe(false)
    expect(first.truncated).toBe(false)
  })

  it('目录里塞了 5 万项以上时 truncated=true：界面必须说明“不是全部”', () => {
    const page = payloadPage('files-truncated.json')

    expect(page.truncated).toBe(true)
    expect(page.entries).toHaveLength(200)
  })
})

describe('真机载荷：文本与错误', () => {
  it('文本载荷：content 的字节数等于 size_bytes，version 形状合法', () => {
    const text = payloadText('files-text-ok.json')

    expect(text.encoding).toBe('utf-8')
    expect(text.path).toBe('sub/sshd_config')
    expect(new TextEncoder().encode(text.content)).toHaveLength(Number(text.size_bytes))
    expect(text.version).toMatch(VERSION)
    expect(text.content).toContain('PermitRootLogin no')
  })

  it('带 expected_version 再读一次，拿到的还是同一个版本（版本取自同一个句柄）', () => {
    const first = payloadText('files-text-ok.json')
    const second = payloadText('files-text-version-ok.json')

    expect(second.version).toBe(first.version)
    expect(second.content).toBe(first.content)
  })

  const cases: { file: string; status: number; code: string; title: string }[] = [
    { file: 'err-path-dotdot.json', status: 422, code: 'PATH_OUTSIDE_ROOT', title: '路径越界' },
    { file: 'err-symlink-text.json', status: 422, code: 'PATH_OUTSIDE_ROOT', title: '路径越界' },
    { file: 'err-magic-list.json', status: 422, code: 'PATH_OUTSIDE_ROOT', title: '路径越界' },
    { file: 'err-xdev.json', status: 422, code: 'PATH_OUTSIDE_ROOT', title: '路径越界' },
    { file: 'err-hardlink-text.json', status: 422, code: 'PATH_OUTSIDE_ROOT', title: '路径越界' },
    { file: 'err-not-text.json', status: 422, code: 'FILE_NOT_TEXT', title: '不是文本文件' },
    { file: 'err-nul-text.json', status: 422, code: 'FILE_NOT_TEXT', title: '不是文本文件' },
    {
      file: 'err-version-conflict.json',
      status: 409,
      code: 'VERSION_CONFLICT',
      title: '数据已被修改',
    },
    {
      file: 'err-root-unavailable.json',
      status: 409,
      code: 'ROOT_UNAVAILABLE',
      title: '文件根不可用',
    },
    { file: 'err-too-large.json', status: 413, code: 'VALIDATION_ERROR', title: '参数不合法' },
    { file: 'err-unknown-root.json', status: 404, code: 'NOT_FOUND', title: '对象不存在' },
  ]

  for (const item of cases) {
    it(`错误信封 ${item.file}：码是 ${item.code}，页面按码给出“${item.title}”`, async () => {
      const error = payloadError(item.file, item.status)
      const html = await render(ApiErrorPanel, { error })

      expect(error.code).toBe(item.code)
      expect(html).toContain(item.title)
      expect(html).toContain(item.code)
      // request_id 必须展示（客服与审计靠它对上服务端日志）。
      expect(html).toContain(error.requestId)
      for (const word of LEAK_WORDS) expect(html).not.toContain(word)
    })
  }

  it('调用方给的具体标题优先于通用标题（同一错误码在不同动作里说法不同）', async () => {
    const html = await render(ApiErrorPanel, {
      error: payloadError('err-path-dotdot.json', 422),
      title: '无法列出目录',
    })

    expect(html).toContain('无法列出目录')
    // 标题是调用方给的；服务端消息里本来就会写“路径越界”，那是内容不是标题。
    expect(html).not.toContain('type="error">路径越界')
  })

  it('只读账号的 403 载荷：不显示“重试”，而是按无权限处理', async () => {
    const error = payloadError('err-viewer-list.json', 403)
    const html = await render(ApiErrorPanel, { error })

    expect(error.code).toBe('FORBIDDEN')
    expect(html).toContain('权限不足')
    expect(html).toContain('需要管理员权限')
    expect(html).not.toContain('>重试<')
  })
})

describe('真机载荷：下载响应头与能力表', () => {
  it('下载的响应头是契约里那一套：octet-stream + nosniff + no-store + attachment', () => {
    const raw = readText('files-download.headers')
    // 头大小写不敏感：Go 写出来的是 X-Upanel-File-Version。
    const headers = new Map<string, string>()
    for (const line of raw.split('\n')) {
      const index = line.indexOf(':')
      if (index < 0) continue
      headers.set(line.slice(0, index).trim().toLowerCase(), line.slice(index + 1).trim())
    }

    expect(raw.startsWith('HTTP/1.1 200')).toBe(true)
    expect(headers.get('content-type')).toBe('application/octet-stream')
    expect(headers.get('x-content-type-options')).toBe('nosniff')
    expect(headers.get('cache-control')).toBe('no-store')
    expect(headers.get('content-disposition')?.startsWith('attachment;')).toBe(true)
    expect(headers.get('content-disposition')).toContain("filename*=UTF-8''")
    expect(headers.get('x-upanel-file-version')).toMatch(VERSION)
    expect(headers.get('content-length')).toMatch(/^[0-9]+$/)
    // 错误响应才有这三个；下载成功时不该出现任何 JSON 痕迹。
    expect(headers.get('content-type')).not.toContain('json')
  })

  it('能力表里只有 files.roots/list/text，没有任何 files 写动作', () => {
    const data = readJson('agent-capabilities.json')['data'] as {
      supported_queries: string[]
      supported_actions: string[]
    }
    const fileQueries = data.supported_queries.filter((name) => name.startsWith('files.'))

    expect(fileQueries.sort()).toEqual(['files.list', 'files.roots', 'files.text'])
    // 写动作（上传/提交/删除/改名）在 P06.2 才出现；现在出现一个都是越界。
    for (const name of data.supported_actions) expect(name.startsWith('files.')).toBe(false)
  })
})
