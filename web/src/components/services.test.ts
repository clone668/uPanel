// 服务相关组件的 SSR 渲染测试（P05.1）。
//
// 服务器端渲染能钉住“界面到底说了什么”，尤其是三类最容易被含糊过去的句子：
//   - 受保护的单元必须写出理由，而不是只挂一个标签；
//   - 缺数据就是缺数据（PID/内存/启动时间），不许渲染成 0；
//   - 被拒绝的预检不能出现“确认执行”按钮。

import { describe, expect, it } from 'vitest'
import { createSSRApp, h, type Component } from 'vue'
import { createMemoryHistory, createRouter, type Router } from 'vue-router'
import { renderToString } from 'vue/server-renderer'

import type { PreflightData } from '@/api/preflights'
import type { ServiceUnit } from '@/api/services'
import ServiceActionConfirm from './ServiceActionConfirm.vue'
import ServiceUnitCard from './ServiceUnitCard.vue'
import ServiceUnitPanel from './ServiceUnitPanel.vue'

function testRouter(): Router {
  return createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: '/', component: { render: () => null } },
      { path: '/services', component: { render: () => null } },
      { path: '/services/:unit', component: { render: () => null } },
      { path: '/jobs', component: { render: () => null } },
    ],
  })
}

async function render(component: Component, props: Record<string, unknown> = {}): Promise<string> {
  const app = createSSRApp({ render: () => h(component, props) })
  app.use(testRouter())
  return renderToString(app)
}

function unit(overrides: Partial<ServiceUnit> = {}): ServiceUnit {
  return {
    unit: 'cron.service',
    description: 'Regular background program processing daemon',
    load_state: 'loaded',
    active_state: 'active',
    sub_state: 'running',
    unit_file_state: 'enabled',
    enablement: 'enabled',
    template: false,
    main_pid: 784,
    started_at: '2026-10-21T00:16:05Z',
    restart_count: 0,
    memory_bytes: '483328',
    tasks_current: 1,
    fragment_path: '/usr/lib/systemd/system/cron.service',
    protection: { protected: false, reasons: [] },
    capabilities: ['stop', 'restart', 'disable'],
    checked_at: '2026-10-21T00:20:00Z',
    ...overrides,
  }
}

function preflight(overrides: Partial<PreflightData> = {}): PreflightData {
  return {
    preflight_id: 'pf_0123456789abcdef0123456789abcdef',
    action: 'service.control',
    normalized_params: { unit: 'cron.service', operation: 'restart' },
    target: { display_name: 'cron.service', identity_fingerprint: 'sha256:fingerprint' },
    expires_at: '2026-10-21T00:22:00Z',
    allowed: true,
    blockers: [],
    warnings: [{ code: 'service_interruption', message: '操作期间该服务提供的能力会中断' }],
    impacts: [{ kind: 'service_availability', description: '服务在操作期间不可用' }],
    requires_reauth: false,
    confirmation_phrase: null,
    cancellable: false,
    locks: ['service:cron.service'],
    ...overrides,
  }
}

describe('ServiceUnitCard', () => {
  it('运行中且开机启动的单元：说清两件事，不挂“需要注意”', async () => {
    const html = await render(ServiceUnitCard, { unit: unit() })
    expect(html).toContain('cron.service')
    expect(html).toContain('运行中')
    expect(html).toContain('开机启动')
    expect(html).not.toContain('需要注意')
    expect(html).not.toContain('受保护')
    expect(html).toContain('href="/services/cron.service"')
  })

  it('缺数据不补 0：null 的 PID / 内存 / 启动时间渲染成“—”', async () => {
    const html = await render(ServiceUnitCard, {
      unit: unit({ main_pid: null, memory_bytes: null, started_at: null, tasks_current: null }),
    })
    expect(html).not.toContain('进程号： 0')
    expect(html).not.toContain('0 B')
    expect(html).toContain('—')
  })

  it('受保护单元标出类别，不显示“需要注意”', async () => {
    const html = await render(ServiceUnitCard, {
      unit: unit({
        unit: 'sshd.service',
        protection: { protected: true, category: 'ssh', reasons: ['远程登录：停掉之后管理员可能失去机器'] },
        capabilities: [],
      }),
    })
    expect(html).toContain('受保护：远程登录')
    expect(html).not.toContain('需要注意')
  })

  it('not-found 单元照实说“不在这台机器上”', async () => {
    const html = await render(ServiceUnitCard, {
      unit: unit({ unit: 'ghost.service', load_state: 'not-found', capabilities: [] }),
    })
    expect(html).toContain('不在这台机器上')
    expect(html).toContain('需要注意')
  })

  it('模板单元不带实例名时标出来', async () => {
    const html = await render(ServiceUnitCard, {
      unit: unit({ unit: 'foo@.service', template: true, capabilities: [] }),
    })
    expect(html).toContain('模板单元')
  })
})

describe('ServiceUnitPanel', () => {
  it('受保护单元：理由逐条列出，并说明“可执行动作：无”', async () => {
    const html = await render(ServiceUnitPanel, {
      unit: unit({
        unit: 'upanel-agent.service',
        protection: {
          protected: true,
          category: 'self',
          reasons: ['面板自身的代理：停掉之后面板失去执行能力', '停掉它会让所有任务失败'],
        },
        capabilities: [],
      }),
    })
    expect(html).toContain('受保护：面板自身')
    expect(html).toContain('面板自身的代理：停掉之后面板失去执行能力')
    expect(html).toContain('停掉它会让所有任务失败')
    expect(html).toContain('可执行动作')
    expect(html).toContain('无')
    expect(html).toContain('不会对它执行任何操作')
  })

  it('模板单元与 not-found 的解释不一样', async () => {
    const template = await render(ServiceUnitPanel, {
      unit: unit({ unit: 'foo@.service', template: true, capabilities: [] }),
    })
    expect(template).toContain('必须指定实例')

    const missing = await render(ServiceUnitPanel, {
      unit: unit({ unit: 'ghost.service', load_state: 'not-found', capabilities: [] }),
    })
    expect(missing).toContain('没有可操作的对象')
  })

  it('开机启动与运行状态分开写，UnitFileState 原值可见', async () => {
    const html = await render(ServiceUnitPanel, {
      unit: unit({ active_state: 'inactive', sub_state: 'dead', enablement: 'disabled', unit_file_state: 'disabled', capabilities: ['start', 'enable'] }),
    })
    expect(html).toContain('未运行')
    expect(html).toContain('不自动启动')
    expect(html).toContain('UnitFileState = disabled')
    expect(html).toContain('启动')
  })
})

describe('ServiceActionConfirm', () => {
  it('允许的重启：先摊开影响与告警，再给确认按钮', async () => {
    const html = await render(ServiceActionConfirm, {
      unit: 'cron.service',
      operation: 'restart',
      preflight: preflight(),
      busy: false,
    })
    expect(html).toContain('cron.service')
    expect(html).toContain('会中断服务可用性')
    expect(html).toContain('service_interruption')
    expect(html).toContain('service_availability')
    expect(html).toContain('确认重启')
  })

  it('被拒绝的预检：列出阻塞项，且没有确认按钮', async () => {
    const html = await render(ServiceActionConfirm, {
      unit: 'sshd.service',
      operation: 'stop',
      preflight: preflight({
        allowed: false,
        blockers: [{ code: 'SERVICE_PROTECTED', message: '受保护的服务，面板不允许对它执行任何操作' }],
      }),
      busy: false,
    })
    expect(html).toContain('SERVICE_PROTECTED')
    expect(html).toContain('受保护的服务')
    expect(html).toContain('预检不允许执行这个操作')
    expect(html).not.toContain('确认停止')
  })

  it('需要确认短语时，未输入前没有确认按钮', async () => {
    const html = await render(ServiceActionConfirm, {
      unit: 'myapp.service',
      operation: 'start',
      preflight: preflight({
        confirmation_phrase: 'PROBE AB2CD3',
        normalized_params: { unit: 'myapp.service', operation: 'start' },
      }),
      busy: false,
    })
    expect(html).toContain('确认短语')
    expect(html).toContain('PROBE AB2CD3')
    expect(html).not.toContain('确认启动')
  })

  it('预检过期后没有确认按钮，只能重新预检', async () => {
    const html = await render(ServiceActionConfirm, {
      unit: 'cron.service',
      operation: 'restart',
      preflight: preflight(),
      busy: false,
      expired: true,
      remainingSeconds: 0,
    })
    expect(html).toContain('预检已过期')
    expect(html).not.toContain('确认重启')
  })

  it('预检还没回来时不显示按钮，只显示正在处理', async () => {
    const html = await render(ServiceActionConfirm, {
      unit: 'myapp.service',
      operation: 'start',
      preflight: null,
      busy: true,
    })
    expect(html).toContain('正在处理')
    expect(html).not.toContain('确认启动')
  })

  it('请求失败时显示可读的说明', async () => {
    const html = await render(ServiceActionConfirm, {
      unit: 'myapp.service',
      operation: 'start',
      preflight: null,
      busy: false,
      errorMessage: '代理不可用，无法读取服务状态',
    })
    expect(html).toContain('代理不可用，无法读取服务状态')
  })
})
