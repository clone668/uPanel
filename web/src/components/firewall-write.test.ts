// 防火墙规则卡片的渲染测试（P14.3，vue/server-renderer）。
//
// 与 network-write.test.ts 同一个理由：冻结依赖里没有 jsdom，所以这里断言**分支与文案**。
// 要断的四件事：
//
//   1) 不可改时不能只有一个灰按钮，必须把 `reasons` 逐条摆出来；
//   2) 有待确认的改动时不能同时出现规则表——两条回滚记录不许叠加；
//   3) 确认的说明必须写明"这里不存在换地址确认"，否则人会以为自己做错了什么；
//   4) 代理没给 config_hash 时不能给可提交的按钮（提交必得 FIREWALL_CONFIG_CHANGED）。
//
// 点击、轮询、真机回滚不在这里覆盖：前者归 Playwright 冒烟，后两者归 VM 验收。

import { describe, expect, it, vi } from 'vitest'
import { createSSRApp, h, type Component, type VNodeChild } from 'vue'
import { renderToString } from 'vue/server-renderer'

import type { FirewallConfigState, FirewallManagedRule, FirewallRollbackView } from '@/api/firewall'
import FirewallApplyCard from './FirewallApplyCard.vue'

// naive-ui 的组件在 SSR 里会去挂样式（需要 document）。这里换成最小实现：
// 文字与 disabled 原样带出来，断言断的仍然是"这份状态被画成了什么"。
vi.mock('naive-ui', async () => {
  const { h: hh } = await import('vue')
  const stub = (name: string) => ({
    name: `Stub${name}`,
    props: [
      'title',
      'description',
      'status',
      'type',
      'disabled',
      'value',
      'options',
      'checked',
      'size',
      'placeholder',
      'bordered',
      'secondary',
      'loading',
      'show',
    ],
    setup(
      props: { title?: unknown; description?: unknown; disabled?: unknown },
      ctx: { slots: { default?: () => unknown; header?: () => unknown; 'header-extra'?: () => unknown } },
    ) {
      return () => {
        const children: VNodeChild[] = []
        if (typeof props.title === 'string') children.push(props.title)
        if (typeof props.description === 'string') children.push(props.description)
        const header = ctx.slots.header?.()
        if (header !== undefined) children.push(header as VNodeChild)
        const slot = ctx.slots.default?.()
        if (slot !== undefined) children.push(slot as VNodeChild)
        const extra = ctx.slots['header-extra']?.()
        if (extra !== undefined) children.push(extra as VNodeChild)
        const attrs: Record<string, unknown> = { 'data-naive': name }
        if (props.disabled === true) attrs['data-disabled'] = 'true'
        return hh('div', attrs, children)
      }
    },
  })
  return {
    NAlert: stub('alert'),
    NButton: stub('button'),
    NCard: stub('card'),
    NCheckbox: stub('checkbox'),
    NInput: stub('input'),
    NModal: stub('modal'),
    NSelect: stub('select'),
    NSwitch: stub('switch'),
    NTag: stub('tag'),
  }
})

async function render(config: FirewallConfigState | null): Promise<string> {
  const app = createSSRApp({ render: () => h(FirewallApplyCard as Component, { config }) })
  return renderToString(app)
}

function rule(overrides: Partial<FirewallManagedRule> = {}): FirewallManagedRule {
  return { id: 'web', action: 'allow', port: 443, proto: 'tcp', from: '', ...overrides }
}

function config(patch: Partial<FirewallConfigState> = {}): FirewallConfigState {
  const base: FirewallConfigState = {
    collected_at: '2026-10-26T12:00:00Z',
    manager: 'ufw',
    reasons: [],
    installed: true,
    enabled: false,
    editable: true,
    rules: [rule()],
    config_hash: 'sha256:abc123def4567890',
    pending: null,
    issues: [],
  }
  return { ...base, ...patch }
}

function record(patch: Partial<FirewallRollbackView> = {}): FirewallRollbackView {
  // deadline 用"从现在起 90 秒"：SSR 里 now 是 setup 那一刻，这个值一定是正的，
  // 断言的是"倒计时按 deadline 现算"这件事，不是某个具体数字。
  const base: FirewallRollbackView = {
    token: 'frb_0123456789abcdef0123456789abcdef',
    deadline: new Date(Date.now() + 90_000).toISOString(),
    seconds_left: 999,
    previous: '防火墙未启用；面板管理 1 条规则',
  }
  return { ...base, ...patch }
}

describe('防火墙规则卡片', () => {
  it('读不到入口状态时明说读不到，不给表', async () => {
    const html = await render(null)
    expect(html).toContain('读不到防火墙写路径的入口状态')
    expect(html).toContain('-firewall-writes')
    expect(html).not.toContain('预检并应用')
  })

  it('可改且没有待确认改动：给规则表、启用开关与提交按钮', async () => {
    const html = await render(config())
    expect(html).toContain('UFW')
    expect(html).toContain('1 条规则')
    // 表头四列都在，说明铺的是整份规则集而不是一条条手工填。
    expect(html).toContain('标识')
    expect(html).toContain('动作')
    expect(html).toContain('端口')
    expect(html).toContain('协议')
    expect(html).toContain('来源')
    expect(html).toContain('启用 UFW')
    expect(html).toContain('加一条规则')
    expect(html).toContain('预检并应用')
    // 规则预览按人话渲染，不是裸 JSON。
    expect(html).toContain('放行 443/TCP（所有来源）')
    // 没有待确认改动时不该出现那两个出路。
    expect(html).not.toContain('确认保留')
    expect(html).not.toContain('立刻退回')
  })

  it('表里只有面板管理的规则：手写规则要说明不会被这次改动碰', async () => {
    const html = await render(config({ rules: [] }))
    expect(html).toContain('面板还没有管任何规则')
    expect(html).toContain('管理员手写的规则')
  })

  it('不可改：原因逐条摆出来，不给提交按钮', async () => {
    const html = await render(
      config({ editable: false, reasons: ['这台机器上 firewalld 也在跑', 'ufw 报的规则形状读不懂'] }),
    )
    expect(html).toContain('不能由面板修改')
    expect(html).toContain('firewalld 也在跑')
    expect(html).toContain('规则形状读不懂')
    expect(html).not.toContain('预检并应用')
    expect(html).not.toContain('加一条规则')
  })

  it('可改但代理没给指纹：说明提交会被拒，不给按钮', async () => {
    const state = config()
    delete state.config_hash
    const html = await render(state)
    expect(html).toContain('config_hash')
    expect(html).toContain('现场变了')
    expect(html).not.toContain('预检并应用')
  })

  it('待确认的改动：倒计时、两个出路、以及"不需要换地址确认"', async () => {
    const html = await render(config({ pending: record() }))
    expect(html).toContain('自动退回')
    expect(html).toContain('剩余')
    expect(html).toContain('确认保留')
    expect(html).toContain('立刻退回')
    // 退回成什么样必须写出来：这是点确认之前该看一眼的那句话。
    expect(html).toContain('防火墙未启用；面板管理 1 条规则')
    // 防火墙与网络不同：确认不看来源地址，这句话不能少。
    expect(html).toContain('本身就说明面板还进得来')
    // 有改动在等的时候不给规则表：两条回滚记录不许叠加。
    expect(html).not.toContain('预检并应用')
    expect(html).toContain('再改下一次')
  })

  it('要启用且没有任何"所有来源"放行：提前提醒一句，并说明判据在服务端', async () => {
    const html = await render(
      config({ enabled: true, rules: [rule({ from: '10.0.0.0/8' })] }),
    )
    expect(html).toContain('FIREWALL_MANAGEMENT_PATH')
    expect(html).toContain('判据在服务端')
    // 提醒不是判据：按钮照样在，拦人的是预检。
    expect(html).toContain('预检并应用')
  })

  it('有一条所有来源的放行时不要多嘴', async () => {
    const html = await render(config({ enabled: true, rules: [rule()] }))
    expect(html).not.toContain('FIREWALL_MANAGEMENT_PATH')
  })

  it('未启用时要说清"规则在，但不生效"', async () => {
    const html = await render(config({ enabled: false }))
    expect(html).toContain('不生效')
  })
})
