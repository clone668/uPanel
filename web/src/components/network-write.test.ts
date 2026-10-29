// 网络地址修改卡片的渲染测试（P14.2，vue/server-renderer）。
//
// 冻结依赖里没有 jsdom，也没有 Playwright 之外的交互层，所以这里断言的是**分支与文案**：
// 一条待确认的记录在不在、倒计时有没有在走、不可改时原因有没有摆出来、表单有没有被
// 待处理记录挡住。这些正是"说错了人就会做错事"的地方：
//
//   1) 不可改时不能只有一个灰按钮，必须有原因；
//   2) 有待确认记录时不能同时出现"再改一次"的表单——两条回滚记录不许叠加；
//   3) 确认必须写明"只接受从新地址发起的请求"，否则人会在旧地址上点，然后被拒；
//   4) 代理没给 config_hash 时不能给可提交的按钮（提交必得 NETWORK_CONFIG_CHANGED）。
//
// 点击、轮询、真机回滚不在这里覆盖：前者归 Playwright 冒烟，后两者归 VM 验收。

import { describe, expect, it, vi } from 'vitest'
import { createSSRApp, h, type Component, type VNodeChild } from 'vue'
import { renderToString } from 'vue/server-renderer'

import type { NetworkConfigState, NetworkRollbackView } from '@/api/network'
import NetworkApplyCard from './NetworkApplyCard.vue'

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
    NTag: stub('tag'),
  }
})

async function render(config: NetworkConfigState | null): Promise<string> {
  const app = createSSRApp({ render: () => h(NetworkApplyCard as Component, { config }) })
  return renderToString(app)
}

function config(patch: Partial<NetworkConfigState> = {}): NetworkConfigState {
  const base: NetworkConfigState = {
    collected_at: '2026-10-26T12:00:00Z',
    capability: { backend: 'netplan-networkd', editable: true, target: 'ens4', reasons: [] },
    interface: 'ens4',
    permanent_mac: '52:54:00:aa:bb:cc',
    config_hash: 'sha256:abc123def4567890',
    mode: 'static',
    ipv4: { address: '192.168.1.10', prefix_length: 24, gateway: '192.168.1.1', dns: null },
    panel_file: { name: '90-upanel-ens4.yaml', sha256: 'a1b2c3d4e5f60718' },
    netplan_files: [],
    unsupported: [],
    pending: null,
    issues: [],
  }
  return { ...base, ...patch }
}

function record(patch: Partial<NetworkRollbackView> = {}): NetworkRollbackView {
  // deadline 用"从现在起 90 秒"：SSR 里 now 是 setup 那一刻，这个值一定是正的，
  // 断言的是"倒计时按 deadline 现算"这件事，不是某个具体数字。
  const deadline = new Date(Date.now() + 90_000).toISOString()
  const base: NetworkRollbackView = {
    token: 'nrb_0123456789abcdef0123456789abcdef',
    interface: 'ens4',
    mode: 'static',
    candidate_address: '192.168.1.50',
    applied_at: new Date().toISOString(),
    deadline,
    seconds_left: 999,
    state: 'pending',
  }
  return { ...base, ...patch }
}

describe('网络地址修改卡片', () => {
  it('读不到入口状态时明说读不到，不给表单', async () => {
    const html = await render(null)
    expect(html).toContain('读不到写路径的入口状态')
    expect(html).not.toContain('预检并修改')
  })

  it('可改且没有待确认记录：给表单与提交按钮', async () => {
    const html = await render(config())
    expect(html).toContain('ens4')
    expect(html).toContain('192.168.1.10/24')
    // 面板在管的那份文件要露出来：人得知道是哪个文件。
    expect(html).toContain('90-upanel-ens4.yaml')
    expect(html).toContain('方式')
    expect(html).toContain('地址')
    expect(html).toContain('前缀')
    expect(html).toContain('网关')
    expect(html).toContain('DNS')
    expect(html).toContain('预检并修改')
    // 没有待确认记录时不该出现那两个出路。
    expect(html).not.toContain('从新地址确认保留')
    expect(html).not.toContain('立刻退回')
  })

  it('面板还没写过配置时说清这个网卡现在由别人管', async () => {
    const html = await render(config({ panel_file: null }))
    expect(html).toContain('面板还没有写过配置')
  })

  it('不可改：原因逐条摆出来，不给提交按钮', async () => {
    const state = config({
      capability: { backend: 'unknown', editable: false, reasons: ['这台机器上有网桥'] },
      unsupported: ['/etc/netplan/50-cloud-init.yaml 里有多份网卡定义'],
    })
    delete state.config_hash
    const html = await render(state)
    expect(html).toContain('不能由面板修改')
    expect(html).toContain('这台机器上有网桥')
    expect(html).toContain('多份网卡定义')
    expect(html).not.toContain('预检并修改')
  })

  it('可改但代理没给指纹：说明提交会被拒，不给按钮', async () => {
    const state = config()
    delete state.config_hash
    const html = await render(state)
    expect(html).toContain('config_hash')
    expect(html).toContain('现场变了')
    expect(html).not.toContain('预检并修改')
  })

  it('待确认记录：倒计时、两个出路、以及"只接受从新地址确认"', async () => {
    const html = await render(config({ pending: record() }))
    expect(html).toContain('还有')
    expect(html).toContain('自动退回')
    expect(html).toContain('剩余')
    expect(html).toContain('从新地址确认保留')
    expect(html).toContain('立刻退回')
    // 这句是关键：在旧地址上点确认会被拒，得先在新地址上重新打开面板。
    expect(html).toContain('由服务端从连接里取')
    expect(html).toContain('192.168.1.50')
    expect(html).toContain('旧地址')
    // 有记录在等的时候不给表单：两条回滚记录不许叠加。
    expect(html).not.toContain('预检并修改')
    expect(html).toContain('再改下一次')
  })

  it('已回滚/已确认的记录：只说结果，不再给按钮', async () => {
    const rolled = await render(config({ pending: record({ state: 'rolled_back' }) }))
    expect(rolled).toContain('已退回')
    expect(rolled).not.toContain('从新地址确认保留')
    expect(rolled).not.toContain('立刻退回')

    const confirmed = await render(config({ pending: record({ state: 'confirmed' }) }))
    expect(confirmed).toContain('已确认保留')
    expect(confirmed).not.toContain('从新地址确认保留')
  })

  it('回滚记录里带的结果原文会显示出来', async () => {
    const html = await render(
      config({ pending: record({ state: 'failed', result: 'netplan apply 返回 1：网关不可达' }) }),
    )
    expect(html).toContain('netplan apply 返回 1')
    expect(html).toContain('没有走完')
  })

  it('DHCP 模式：当前地址说成 DHCP，不是空字符串', async () => {
    const state = config({ mode: 'dhcp' })
    delete state.ipv4
    const html = await render(state)
    expect(html).toContain('由 DHCP 分配')
    expect(html).not.toContain('现在：ens4 ·  ·')
  })
})
