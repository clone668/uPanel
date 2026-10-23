// 电源界面在屏幕上到底说了什么（P11.3）。
//
// 这一组用例盯的不是交互（SSR 里没有 DOM 事件），而是**措辞**——P11.3 的验收条件
// 正是两句措辞：「关机界面不伪称已验证」和「重启 boot_id 确认」。
//
// 所以这里逐字锁住：
//   · 关机入口旁边必须出现「不会自己开机」和「不构成关机成功的证据」；
//   · 核对结论为 unreachable（关机后的常态）时，屏幕上不许出现「已关机 / 关机成功」；
//   · 没有核对（null）时整块不渲染，而不是渲染一个「未确认」的空壳；
//   · boot_id 读不到时说「读不到」，不显示 undefined 或空字符串。

import { describe, expect, it, vi } from 'vitest'
import { createSSRApp, h, type Component, type VNodeChild } from 'vue'
import { createMemoryHistory, createRouter, type Router } from 'vue-router'
import { renderToString } from 'vue/server-renderer'

import type { PowerCheckDTO } from '@/api/jobs'
import PowerCheckPanel from './PowerCheckPanel.vue'
import PowerControlCard from './PowerControlCard.vue'

// 渲染只需要文字与标签色：naive-ui 换成最小实现（SSR 里它要 document）。
vi.mock('naive-ui', async () => {
  const { h } = await import('vue')
  const stub = (name: string) => ({
    name: `Stub${name}`,
    props: ['title', 'description', 'status', 'code', 'disabled', 'type', 'size', 'bordered'],
    setup(props: { title?: unknown; type?: unknown }, ctx: { slots: { default?: () => unknown } }) {
      return () => {
        const children: VNodeChild[] = []
        if (typeof props.title === 'string') children.push(props.title)
        const slot = ctx.slots.default?.()
        if (slot !== undefined) children.push(slot as VNodeChild)
        const attrs: Record<string, unknown> = { 'data-naive': name }
        if (typeof props.type === 'string') attrs['data-type'] = props.type
        return h('div', attrs, children)
      }
    },
  })
  return {
    NButton: stub('button'),
    NCard: stub('card'),
    NTag: stub('tag'),
  }
})

function testRouter(): Router {
  return createRouter({
    history: createMemoryHistory(),
    routes: [{ path: '/', component: { render: () => null } }],
  })
}

async function render(component: Component, props: Record<string, unknown> = {}): Promise<string> {
  const app = createSSRApp({ render: () => h(component, props) })
  app.use(testRouter())
  const html = await renderToString(app)
  return html.replace(/ data-v-[0-9a-z]+(?=[ >])/g, '')
}

function check(overrides: Partial<PowerCheckDTO> = {}): PowerCheckDTO {
  return {
    operation: 'reboot',
    state: 'confirmed',
    changed: true,
    boot_id_before: 'ab2eb5e8-c1b4-4a00-a3d0-33ced7870abc',
    boot_id_now: '9f4d2c17-6a55-4c2e-8f31-0b7e5a1d9c40',
    checked_at: '2026-10-23T10:05:00Z',
    message: '重启已确认：boot_id 从 ab2eb5e8 变成 9f4d2c17。',
    ...overrides,
  }
}

describe('重启与关机的入口卡片', () => {
  it('两个操作都在，且都带“确认”两个字', async () => {
    const html = await render(PowerControlCard, {
      rebootRequired: false,
      bootId: 'ab2eb5e8-c1b4-4a00-a3d0-33ced7870abc',
      collectedAt: '2026-10-23T09:30:00Z',
    })
    expect(html).toContain('确认重启')
    expect(html).toContain('确认关机')
    // 没有重启标志不等于不能重启：说明必须写出来。
    expect(html).toContain('没有重启请求')
    expect(html).toContain('没有它也能重启')
    // 倒计时四档都在，且没有 0 秒这一档。
    for (const preset of ['30 秒', '60 秒', '120 秒', '300 秒']) expect(html).toContain(preset)
    expect(html).not.toMatch(/>0 秒/)
  })

  it('关机不伪称已验证：那两句必须真的在屏幕上', async () => {
    const html = await render(PowerControlCard, {
      rebootRequired: true,
      bootId: '',
      collectedAt: null,
    })
    expect(html).toContain('不会自己开机')
    expect(html).toContain('不构成关机成功的证据')
    // boot_id 读不到时说“读不到”，不要留空。
    expect(html).toContain('读不到 boot_id')
    expect(html).not.toContain('undefined')
  })

  it('关键任务会拦下电源操作这件事写在卡片上', async () => {
    const html = await render(PowerControlCard, {
      rebootRequired: false,
      bootId: 'ab2eb5e8',
      collectedAt: '2026-10-23T09:30:00Z',
    })
    expect(html).toContain('关键任务运行期间一律拒绝电源操作')
    expect(html).toContain('boot_id')
  })
})

describe('电源核对那一块', () => {
  it('变了才说“已确认”，并把两个 boot_id 都摊出来', async () => {
    const html = await render(PowerCheckPanel, { check: check() })
    expect(html).toContain('已确认')
    expect(html).toContain('ab2eb5e8-c1b4-4a00-a3d0-33ced7870abc')
    expect(html).toContain('9f4d2c17-6a55-4c2e-8f31-0b7e5a1d9c40')
    expect(html).toContain('data-state="confirmed"')
  })

  it('没变就说没变，一个字都不许多', async () => {
    const html = await render(PowerCheckPanel, {
      check: check({
        state: 'unchanged',
        changed: false,
        boot_id_now: 'ab2eb5e8-c1b4-4a00-a3d0-33ced7870abc',
        message: '代理仍在原来的 boot_id 上：重启还没有发生。',
      }),
    })
    expect(html).toContain('尚未发生')
    expect(html).toContain('重启还没有发生')
    expect(html).not.toContain('已确认')
  })

  it('关机连不上：直说连不上，绝不写成“已关机”', async () => {
    const html = await render(PowerCheckPanel, {
      check: check({
        operation: 'poweroff',
        state: 'unreachable',
        changed: false,
        boot_id_now: '',
        message: '代理已不可达。这与下发关机之后的预期一致，但面板无法据此确认电源已切断。',
      }),
    })
    expect(html).toContain('连不上代理')
    expect(html).toContain('无法据此确认电源已切断')
    expect(html).toContain('读不到')
    expect(html).not.toContain('已关机')
    expect(html).not.toContain('关机成功')
  })

  it('没有核对就整块不渲染：多数任务根本没有这个问题', async () => {
    const html = await render(PowerCheckPanel, { check: null })
    expect(html.trim()).toBe('<!---->')
    expect(html).not.toContain('未确认')
  })
})
