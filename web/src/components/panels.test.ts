// 组件渲染测试。
//
// 约束：冻结依赖里没有 jsdom / happy-dom，不能挂载到真实 DOM。
// 因此这里用 vue/server-renderer 把组件渲染成 HTML 字符串再断言——
// 覆盖的是模板分支、文案与属性绑定（v-if / props / 文本节点），
// 不覆盖点击、焦点、动画等需要浏览器的行为。
// 详见 docs/DECISIONS.md Q21。

import { describe, expect, it } from 'vitest'
import { createSSRApp, h, type Component } from 'vue'
import { renderToString } from 'vue/server-renderer'

import { ApiError } from '@/api/client'
import ApiErrorPanel from './ApiErrorPanel.vue'
import MockBanner from './MockBanner.vue'
import StatePanel from './StatePanel.vue'

async function render(component: Component, props: Record<string, unknown> = {}): Promise<string> {
  return renderToString(createSSRApp({ render: () => h(component, props) }))
}

function serverError(code: ApiError['code'], status: number, message: string): ApiError {
  return new ApiError({ code, message, status, requestId: 'req-abc', source: 'server' })
}

describe('MockBanner', () => {
  it('mock 模式下持续显示“演示数据、未操作服务器”', async () => {
    const html = await render(MockBanner, { mode: 'mock' })
    expect(html).toContain('data-testid="mock-banner"')
    expect(html).toContain('演示模式：数据为演示数据，未操作服务器')
    expect(html).toContain('所有操作都不会写入本机')
  })

  it('production 模式不显示横幅', async () => {
    const html = await render(MockBanner, { mode: 'production' })
    expect(html).not.toContain('mock-banner')
    expect(html).not.toContain('演示模式')
  })
})

describe('StatePanel', () => {
  const states = [
    ['loading', '加载中'],
    ['empty', '空数据'],
    ['forbidden', '无权限'],
    ['dependency_missing', '依赖未安装'],
    ['unsupported', '数据不支持'],
    ['error', '请求失败'],
    ['partial', '部分数据可用'],
  ] as const

  it.each(states)('状态 %s 有明确的中文标签，不靠颜色区分', async (state, label) => {
    const html = await render(StatePanel, { state })
    expect(html).toContain(`data-state="${state}"`)
    expect(html).toContain(`状态：${label}`)
  })

  it('失败状态可以展示错误码与 request_id', async () => {
    const html = await render(StatePanel, {
      state: 'error',
      code: 'AGENT_UNAVAILABLE',
      requestId: 'req-123',
    })
    // 注：SFC 的 scoped style 会给元素加 data-v-* 属性，因此用正则而不是整串匹配。
    expect(html).toMatch(/错误码：<code[^>]*>AGENT_UNAVAILABLE<\/code>/)
    expect(html).toMatch(/request_id：<code[^>]*>req-123<\/code>/)
  })

  it('部分可用状态列出缺失项', async () => {
    const html = await render(StatePanel, { state: 'partial', missing: ['CPU 使用率', '磁盘 IO'] })
    expect(html).toContain('缺失项：CPU 使用率、磁盘 IO')
  })

  it('只有标记为可重试时才出现重试按钮', async () => {
    expect(await render(StatePanel, { state: 'error' })).not.toContain('>重试<')
    expect(await render(StatePanel, { state: 'error', retryable: true })).toContain('>重试<')
  })

  it('未提供覆盖文案时使用状态默认说明', async () => {
    const html = await render(StatePanel, { state: 'dependency_missing' })
    expect(html).toContain('该功能依赖的软件尚未安装或未启用，安装后可用。')
  })

  it('空状态保留页面给的标题（不是只剩“暂无数据”）', async () => {
    const html = await render(StatePanel, {
      state: 'empty',
      title: '这台机器上还没有受管部署',
      description: '从这一页创建部署会写出包装单元与渲染后的 Compose 配置。',
    })
    expect(html).toContain('这台机器上还没有受管部署')
    expect(html).toContain('从这一页创建部署会写出包装单元与渲染后的 Compose 配置。')
  })
})

// 紧凑版是给概览右侧那种窄面板用的：不许掉文案，也不许把大插图带进去。
describe('StatePanel 紧凑版', () => {
  it('保留原因与错误码，但不渲染结果页插图', async () => {
    const html = await render(StatePanel, {
      state: 'error',
      compact: true,
      description: '预览夹具未覆盖该接口',
      code: 'NOT_FOUND',
      requestId: 'req-abc',
    })
    expect(html).toContain('预览夹具未覆盖该接口')
    expect(html).toContain('NOT_FOUND')
    expect(html).toContain('req-abc')
    // NResult 的插图是 svg；紧凑版里不该出现。
    expect(html).not.toContain('<svg')
    expect(html).toContain('state-panel--compact')
  })

  it('标题与状态同词时不重复写两遍', async () => {
    const same = await render(StatePanel, { state: 'error', compact: true })
    expect(same).not.toContain('状态：请求失败')

    // 标题与状态不同词时，两行各有信息量，都要留。
    const differ = await render(StatePanel, {
      state: 'partial',
      compact: true,
      title: '存储段不可用',
    })
    expect(differ).toContain('状态：部分数据可用')
    expect(differ).toContain('存储段不可用')
  })
})

describe('ApiErrorPanel', () => {
  it('展示 message、错误码与 request_id', async () => {
    const html = await render(ApiErrorPanel, {
      error: serverError('DEVICE_IN_USE', 409, '设备正被占用：/dev/sdb'),
    })
    expect(html).toContain('data-testid="api-error"')
    expect(html).toContain('设备正被占用：/dev/sdb')
    expect(html).toMatch(/错误码：<code[^>]*>DEVICE_IN_USE<\/code>/)
    expect(html).toMatch(/request_id：<code[^>]*>req-abc<\/code>/)
  })

  it('可重试的错误给出重试按钮', async () => {
    const html = await render(ApiErrorPanel, {
      error: serverError('AGENT_UNAVAILABLE', 503, 'agent 未就绪'),
    })
    expect(html).toContain('特权代理不可用')
    expect(html).toContain('>重试<')
  })

  it('401 说明需要重新登录，且不提供重试', async () => {
    const html = await render(ApiErrorPanel, {
      error: serverError('UNAUTHENTICATED', 401, '会话已失效'),
    })
    expect(html).toContain('重新登录功能在 P02 提供。')
    expect(html).not.toContain('重试</button>')
  })

  it('调用方禁止重试时不渲染按钮，即使错误本身可重试', async () => {
    const html = await render(ApiErrorPanel, {
      error: serverError('AGENT_UNAVAILABLE', 503, 'agent 未就绪'),
      allowRetry: false,
    })
    expect(html).not.toContain('重试</button>')
  })
})
