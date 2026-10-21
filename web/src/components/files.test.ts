// 文件页与文件行的渲染用例（P06.1 读取 + P06.2 写入）。
//
// 环境是 node，没有 DOM，所以用 vue/server-renderer 渲染成字符串再断言：验的是
// "这份数据最终显示成什么文字"，不是交互（见 docs/DECISIONS.md Q21）。
//
// 这一层要盯住的是**验收条款里的那句话**：
//   - 文件页不能显示"可管理整个服务器所有目录"。它只能说"服务端登记的根"，
//     没有登记根时只能说"未登记任何文件根"——所以下面直接断言页面里
//     **不出现**"整个服务器""浏览服务器"这类说法。
//   - 符号链接与特殊文件必须在行内写明"不跟随/不读取"，而不是给一个灰按钮不解释。
//   - 超过文本上限的文件仍然可以下载：上限是预览的上限，不是文件的上限。
//   - 写入口只在可写根上出现，而且按类型分开给：只读根上一颗按钮都不画，
//     目录不给"编辑文本"，符号链接不给"权限/所有者"（P06.2）。
//
// 数据是**手写形状样本**，与 files.test.ts 的分工一致：这里验"每种状态怎么画"，
// 那边验"参数怎么拼、错误码怎么分支"。真机回放用例在 tests/integration 的
// P06.1/P06.2 验收脚本抓到真实响应后补。

import { describe, expect, it, vi } from 'vitest'
import { createSSRApp, h, type Component, type VNodeChild } from 'vue'
import { renderToString } from 'vue/server-renderer'

import type { FileEntry } from '@/api/files'
import FileEntryRow from './FileEntryRow.vue'
import FilesView from '../views/FilesView.vue'

// 会话状态只决定"渲染哪一半页面"（管理员 vs 只读）。真正的登录流程属于 P02。
const sessionState = vi.hoisted(() => ({ isAdmin: false }))

vi.mock('@/stores/session', () => ({
  useSessionStore: () => ({
    get isAdmin() {
      return sessionState.isAdmin
    },
  }),
}))

// naive-ui 的控件在 SSR 里会去挂样式（需要 document）。这里把它们换成最小实现：
// 文字（插槽 + title/description 属性）与 disabled 状态原样带出来，断言才有东西可断。
vi.mock('naive-ui', async () => {
  const { h } = await import('vue')
  const stub = (name: string) => ({
    name: `Stub${name}`,
    props: ['title', 'description', 'status', 'code', 'disabled', 'value', 'options'],
    setup(
      props: { title?: unknown; description?: unknown; disabled?: unknown },
      ctx: { slots: { default?: () => unknown; 'header-extra'?: () => unknown } },
    ) {
      return () => {
        const children: VNodeChild[] = []
        if (typeof props.title === 'string') children.push(props.title)
        if (typeof props.description === 'string') children.push(props.description)
        const slot = ctx.slots.default?.()
        if (slot !== undefined) children.push(slot as VNodeChild)
        const extra = ctx.slots['header-extra']?.()
        if (extra !== undefined) children.push(extra as VNodeChild)
        const attrs: Record<string, unknown> = { 'data-naive': name }
        if (props.disabled === true) attrs['data-disabled'] = 'true'
        return h('div', attrs, children)
      }
    },
  })
  // 对话框只在 show 为真时渲染内容：关着的对话框不该在页面里留下输入框，
  // 否则“首屏没有输入路径”这条断言会变成测一个假象。
  const modal = (name: string) => ({
    name: `Stub${name}`,
    props: ['show', 'title', 'preset', 'maskClosable', 'class'],
    setup(
      props: { show?: unknown; title?: unknown },
      ctx: { slots: { default?: () => unknown; 'header-extra'?: () => unknown } },
    ) {
      return () => {
        if (props.show !== true) return null
        const children: VNodeChild[] = []
        if (typeof props.title === 'string') children.push(props.title)
        const slot = ctx.slots.default?.()
        if (slot !== undefined) children.push(slot as VNodeChild)
        const extra = ctx.slots['header-extra']?.()
        if (extra !== undefined) children.push(extra as VNodeChild)
        return h('div', { 'data-naive': name }, children)
      }
    },
  })
  return {
    NAlert: stub('alert'),
    NButton: stub('button'),
    NCard: stub('card'),
    NCheckbox: stub('checkbox'),
    NEmpty: stub('empty'),
    NInput: stub('input'),
    NModal: modal('modal'),
    NResult: stub('result'),
    NSelect: stub('select'),
    NSpin: stub('spin'),
    NSwitch: stub('switch'),
    NTag: stub('tag'),
    NInputNumber: stub('input-number'),
  }
})

async function render(component: Component, props: Record<string, unknown> = {}): Promise<string> {
  const app = createSSRApp({ render: () => h(component, props) })
  const html = await renderToString(app)
  // scoped 样式加的 data-v-* 只作用于 DOM，留着它会让断言变成测编译产物。
  return html.replace(/ data-v-[0-9a-z]+(?=[ >])/g, '')
}

function entry(overrides: Partial<FileEntry> = {}): FileEntry {
  return {
    name: 'sshd_config',
    type: 'file',
    size_bytes: '512',
    mode: '0644',
    owner: { uid: 0, gid: 0 },
    mtime: '2026-10-21T02:59:00Z',
    version: 'v1-a1b2-c3d4-200-17a0b1c2d3e4',
    ...overrides,
  }
}

describe('FileEntryRow', () => {
  it('普通文件：显示大小、权限、属主，并给预览与下载两个入口', async () => {
    const html = await render(FileEntryRow, { entry: entry() })

    expect(html).toContain('sshd_config')
    expect(html).toContain('文件')
    expect(html).toContain('512 B')
    expect(html).toContain('0644')
    expect(html).toContain('0:0')
    expect(html).toContain('预览')
    expect(html).toContain('下载')
    // 预览与下载都是可点的：没有 disabled。
    expect(html).not.toContain('data-disabled="true"')
  })

  it('目录：大小显示 U+2014（不是 0 B），且只有进入没有预览/下载', async () => {
    const html = await render(FileEntryRow, { entry: entry({ name: 'nginx', type: 'dir', size_bytes: '0' }) })

    expect(html).toContain('目录')
    expect(html).toContain('—')
    expect(html).not.toContain('0 B')
    expect(html).not.toContain('预览')
    expect(html).not.toContain('下载')
  })

  it('符号链接：不给任何入口，并写明"符号链接不跟随"（不是静默禁用）', async () => {
    const html = await render(FileEntryRow, { entry: entry({ name: 'shadow', type: 'symlink' }) })

    expect(html).toContain('符号链接')
    expect(html).toContain('符号链接不跟随')
    expect(html).not.toContain('预览')
    expect(html).not.toContain('下载')
  })

  it('特殊文件（设备、管道、socket）：写明不读取', async () => {
    const html = await render(FileEntryRow, { entry: entry({ name: 'sda', type: 'other' }) })

    expect(html).toContain('特殊文件')
    expect(html).toContain('不读取')
    expect(html).not.toContain('下载')
  })

  it('超过 1 MiB 的文件：预览被禁用并说明"只能下载"，下载仍然可用', async () => {
    const html = await render(FileEntryRow, {
      entry: entry({ name: 'big.log', size_bytes: '2097152' }),
    })

    expect(html).toContain('2 MiB')
    expect(html).toContain('只能下载')
    expect(html).toContain('data-disabled="true"')
    expect(html).toContain('下载')
  })

  it('下载中的行把 loading 交给按钮（不会显示成"可点但没反应"）', async () => {
    const html = await render(FileEntryRow, { entry: entry(), downloading: true })
    expect(html).toContain('data-naive="button"')
  })

  it('只读根上的行不给任何写入口（画一个点了必然 403 的按钮等于骗人）', async () => {
    const html = await render(FileEntryRow, { entry: entry(), canWrite: false })

    expect(html).not.toContain('编辑')
    expect(html).not.toContain('改名/移动')
    expect(html).not.toContain('权限')
    expect(html).not.toContain('所有者')
    expect(html).not.toContain('删除')
  })

  it('可写根上的普通文件：编辑、改名/移动、权限、所有者、删除都给', async () => {
    const html = await render(FileEntryRow, { entry: entry(), canWrite: true })

    expect(html).toContain('编辑')
    expect(html).toContain('改名/移动')
    expect(html).toContain('权限')
    expect(html).toContain('所有者')
    expect(html).toContain('删除')
    // 读入口仍然在：写权限不是替换读权限。
    expect(html).toContain('预览')
    expect(html).toContain('下载')
  })

  it('可写根上的目录：没有"编辑"（不给目录开文本编辑器），但有改名与删除', async () => {
    const html = await render(FileEntryRow, {
      entry: entry({ name: 'nginx', type: 'dir', size_bytes: '0' }),
      canWrite: true,
    })

    expect(html).not.toContain('编辑')
    expect(html).toContain('改名/移动')
    expect(html).toContain('删除')
  })

  it('可写根上的符号链接：只给改名与删除，不给权限/所有者（删链接就是删链接本身）', async () => {
    const html = await render(FileEntryRow, {
      entry: entry({ name: 'shadow', type: 'symlink' }),
      canWrite: true,
    })

    expect(html).toContain('改名/移动')
    expect(html).toContain('删除')
    expect(html).not.toContain('编辑')
    expect(html).not.toContain('所有者')
  })
})

describe('FilesView', () => {
  it('只读账号看到"没有权限"，而不是一个空目录或去掉几行的只读视图', async () => {
    sessionState.isAdmin = false
    const html = await render(FilesView)

    expect(html).toContain('没有权限访问文件')
    expect(html).not.toContain('正在读取文件根')
    expect(html).not.toContain('选择根')
  })

  it('管理员进页面先显示"正在读取文件根"，并写明范围只到登记的根', async () => {
    sessionState.isAdmin = true
    const html = await render(FilesView)

    expect(html).toContain('正在读取文件根')
    expect(html).toContain('只能访问服务端登记的文件根')
    expect(html).toContain('路径是根内相对路径')
    expect(html).toContain('符号链接不跟随')
  })

  it('页面不宣称能管理整个服务器（验收条款的原文要求）', async () => {
    sessionState.isAdmin = true
    const html = await render(FilesView)

    expect(html).not.toContain('整个服务器')
    expect(html).not.toContain('浏览服务器')
    expect(html).not.toContain('任意路径')
    expect(html).not.toContain('所有目录')
  })

  it('页面写明写操作走预检、上传不等于落地', async () => {
    sessionState.isAdmin = true
    const html = await render(FilesView)

    expect(html).toContain('全部走预检 → 任务')
    expect(html).toContain('落地是单独一步')
  })

  it('首屏没有任何输入框：读文件只能从根点下去', async () => {
    sessionState.isAdmin = true
    const html = await render(FilesView)

    // 写动作的对话框里确实有一个“目标路径”输入框，但那是**写目标**，
    // 而且只在用户点了具体动作之后才出现；首屏没有任何可输入的读路径。
    expect(html).not.toContain('data-naive="input"')
    expect(html).not.toContain('data-naive="modal"')
  })
})
