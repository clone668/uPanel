// 日志页组件用例（P05.3）。
//
// 环境是 node，没有 DOM，所以用 vue/server-renderer 渲染成字符串再断言：
// 验的是"这段数据最终显示成什么文字"，不是交互（交互要等 DOM 环境，见 DECISIONS Q21）。
//
// 这里的数据是**手写的形状样本**，不是真机抓包。真机回放用例（fixtures/p05-3-vm/）
// 在本轮验收跑完后补上；两者不能互相顶替——手写样本验"每种状态怎么画"，
// 真机样本验"服务端的真实响应能不能画"。

import { describe, expect, it, vi } from 'vitest'
import { createSSRApp, h, type Component, type VNodeChild } from 'vue'
import { renderToString } from 'vue/server-renderer'

import type { LogEntry } from '@/api/logs'
import LogEntryRow from './LogEntryRow.vue'
import LogsView from '../views/LogsView.vue'

// 会话状态在这里只用于"渲染哪一半页面"（管理员 vs 只读）。
// 真正写入会话的是登录流程（P02），本用例不需要拉起整条认证链路。
const sessionState = vi.hoisted(() => ({ isAdmin: false }))

vi.mock('@/stores/session', () => ({
  useSessionStore: () => ({
    get isAdmin() {
      return sessionState.isAdmin
    },
  }),
}))

// naive-ui 的 NSelect / NSwitch 在 SSR 里会去 mount 样式（需要 document），
// 所以这里把它们换成"只渲染插槽内容"的最小组件：本用例要验的是文字，
// 不是那些控件的视觉，控件本身的正确性不属于这一层。
vi.mock('naive-ui', async () => {
  const { h } = await import('vue')
  const stub = (name: string) => ({
    name: `Stub${name}`,
    // 文字既是插槽内容、也常常是 title/description 属性（StatePanel 就是这么用的），
    // 两者都渲染出来，断言才有东西可断。
    props: ['title', 'description', 'status', 'code'],
    setup(
      props: { title?: unknown; description?: unknown },
      ctx: { slots: { default?: () => unknown } },
    ) {
      return () => {
        const children: VNodeChild[] = []
        if (typeof props.title === 'string') children.push(props.title)
        if (typeof props.description === 'string') children.push(props.description)
        const slot = ctx.slots.default?.()
        if (slot !== undefined) children.push(slot as VNodeChild)
        return h('div', { 'data-naive': name }, children)
      }
    },
  })
  return {
    NAlert: stub('alert'),
    NButton: stub('button'),
    NCard: stub('card'),
    NEmpty: stub('empty'),
    NInput: stub('input'),
    NResult: stub('result'),
    NSelect: stub('select'),
    NSpin: stub('spin'),
    NSwitch: stub('switch'),
    NTag: stub('tag'),
    NInputNumber: stub('input-number'),
  }
})

function entry(overrides: Partial<LogEntry> = {}): LogEntry {
  return {
    cursor: 's=8a1e0b6f2c4d4f39;i=41;b=8a1e0b6f2c4d4f39;m=5f0c;t=5f0c;x=9b30',
    timestamp: '2026-10-21T02:59:00Z',
    level: 'warning',
    message: 'sshd: 收到连接',
    truncated: false,
    redacted: false,
    binary: false,
    unit: 'ssh.service',
    identifier: 'sshd',
    comm: 'sshd',
    pid: 900,
    ...overrides,
  }
}

async function render(component: Component, props: Record<string, unknown>): Promise<string> {
  return renderToString(createSSRApp({ render: () => h(component, props) }))
}

describe('日志条目', () => {
  it('普通条目显示时间、级别、来源与消息', async () => {
    const html = await render(LogEntryRow, { entry: entry() })
    expect(html).toContain('ssh.service')
    expect(html).toContain('警告')
    expect(html).toContain('sshd: 收到连接')
    expect(html).toContain('pid 900')
    // 正常的条目不该出现"已过滤/已截断/无文本"三个标记。
    expect(html).not.toContain('已过滤')
    expect(html).not.toContain('已截断')
    expect(html).not.toContain('无文本')
  })

  it('binary 条目不画成空行，而是说明 journald 没有给文本', async () => {
    const html = await render(LogEntryRow, { entry: entry({ binary: true, message: '' }) })
    expect(html).toContain('无文本')
    expect(html).toContain('未提供文本')
  })

  it('截断与脱敏各自有标记：两者都不是"完整原文"', async () => {
    const html = await render(LogEntryRow, { entry: entry({ truncated: true, redacted: true }) })
    expect(html).toContain('已截断')
    expect(html).toContain('已过滤')
  })

  it('未知级别原样显示，不冒充 info', async () => {
    const html = await render(LogEntryRow, { entry: entry({ level: 'verbose' }) })
    expect(html).toContain('verbose')
    expect(html).not.toContain('信息')
  })

  it('没有来源标识时不显示空标签', async () => {
    const html = await render(LogEntryRow, {
      entry: entry({ unit: '', identifier: '', comm: '', pid: null }),
    })
    expect(html).not.toContain('pid ')
  })
})

describe('日志页', () => {
  async function renderView(role: 'admin' | 'viewer'): Promise<string> {
    sessionState.isAdmin = role === 'admin'
    return renderToString(createSSRApp({ render: () => h(LogsView) }))
  }

  it('viewer 看到"没有权限"，看不到查询表单', async () => {
    const html = await renderView('viewer')
    expect(html).toContain('没有权限查看日志')
    expect(html).toContain('只对管理员开放')
    expect(html).not.toContain('查询条件')
  })

  it('admin 看到查询表单与限额说明', async () => {
    const html = await renderView('admin')
    expect(html).toContain('查询条件')
    expect(html).toContain('ssh.service')
    expect(html).toContain('实时订阅')
    // 限额必须写在页面上：用户要知道"为什么只能看 1000 行/24 小时"。
    expect(html).toContain('1000')
    expect(html).toContain('24')
  })
})
