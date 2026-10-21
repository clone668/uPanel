// 用真实 Ubuntu 数据渲染服务页面组件（P05.1）。
//
// 这里的 JSON 不是造的：它们是验收 VM（upanel-2404，Ubuntu 24.04.5 / systemd 255）上
// 真实运行 upanel-web（production 模式、root 代理）后用
// tests/integration/p05-1-services-acceptance.sh 抓回来的响应，原样存在
// src/fixtures/p05-1-vm/ 下（连 {data, request_id} / {error, request_id} 外壳都没拆）。
//
// 为什么要这一层：这一轮的真机验收抓到了两个“看起来完全正常、数据却是错的”问题——
// 282 个单元的机器上列表只有 7 行（模板名让 systemctl show 中断在中途），
// 以及别名单元既没有出现在列表里、也没有被拒绝的解释。这些都不是我自己编的夹具能编出来的。

import { readFileSync } from 'node:fs'

import { describe, expect, it } from 'vitest'
import { createSSRApp, h, type Component } from 'vue'
import { createMemoryHistory, createRouter, type Router } from 'vue-router'
import { renderToString } from 'vue/server-renderer'

import { isTerminal, jobErrorMessage, type JobDTO } from '@/api/jobs'
import {
  canSubmitPreflight,
  requiresConfirmation,
  type PreflightData,
} from '@/api/preflights'
import {
  canOperate,
  filterUnits,
  groupUnits,
  isMissingUnit,
  needsAttention,
  protectionCategoryLabel,
  summarizeUnits,
  type ServiceListData,
  type ServiceUnit,
} from '@/api/services'
import ServiceUnitCard from './ServiceUnitCard.vue'
import ServiceUnitPanel from './ServiceUnitPanel.vue'

function readFixture(name: string): Record<string, unknown> {
  return JSON.parse(
    readFileSync(new URL(`../fixtures/p05-1-vm/${name}`, import.meta.url), 'utf8'),
  ) as Record<string, unknown>
}

function fixture<T>(name: string): T {
  return readFixture(name)['data'] as T
}

function errorFixture(name: string): { code: string; message: string } {
  return readFixture(name)['error'] as { code: string; message: string }
}

const list = fixture<ServiceListData>('services.json')
const inactiveTarget = fixture<{ unit: ServiceUnit }>('detail-target.json').unit
const detailAfterStop = fixture<{ unit: ServiceUnit }>('detail-stopped.json').unit
const sshDetail = fixture<{ unit: ServiceUnit }>('detail-protected-ssh.json').unit
const preflightStart = fixture<PreflightData>('preflight-start-ok.json')
const preflightProtected = fixture<PreflightData>('preflight-protected.json')
const preflightNotFound = fixture<PreflightData>('preflight-notfound.json')
const preflightTemplate = fixture<PreflightData>('preflight-template.json')
const jobOK = fixture<JobDTO>('job-start-ok.json')
const jobFail = fixture<JobDTO>('job-start-fail.json')

/** 契约里的 sha256 指纹：完整 64 位十六进制，短了就是漂移。 */
const SHA256 = /^sha256:[0-9a-f]{64}$/

function testRouter(): Router {
  return createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: '/', component: { render: () => null } },
      { path: '/services/:unit', component: { render: () => null } },
    ],
  })
}

/**
 * 渲染并去掉 scoped 样式加的 data-v-* 属性。
 *
 * 属性只在 DOM 上起作用，跟“页面上写了什么”无关；留着它会让断言变成
 * `<strong data-v-1ce82e51>`——测的是编译产物，不是文案。
 */
async function render(component: Component, props: Record<string, unknown> = {}): Promise<string> {
  const app = createSSRApp({ render: () => h(component, props) })
  app.use(testRouter())
  const html = await renderToString(app)
  // SSR 下 scoped id 不带值（`<strong data-v-1ce82e51>`），所以按“后面是空格或 >”来切。
  return html.replace(/ data-v-[0-9a-z]+(?=[ >])/g, '')
}

function unitNamed(name: string): ServiceUnit {
  const hit = list.units.find((unit) => unit.unit === name)
  if (hit === undefined) throw new Error(`夹具里没有 ${name}`)
  return hit
}

/** 预检的 expires_at 是验收当时的时刻，用“还差 1 秒过期”的 now 判断新鲜度。 */
function justBeforeExpiry(preflight: PreflightData): Date {
  return new Date(new Date(preflight.expires_at).getTime() - 1000)
}

describe('真实 VM 快照：服务列表', () => {
  it('快照字段与契约一致：ok、10 秒、版本是完整 sha256', () => {
    expect(list.status).toBe('ok')
    expect(list.interval_ms).toBe(10000)
    expect(list.truncated).toBe(false)
    expect(list.snapshot_version).toMatch(SHA256)
    expect(list.collected_at.endsWith('Z')).toBe(true)
  })

  it('列表规模与真机相当：239 个单元，不是“只回来了前几行”', () => {
    // 239 = 真机枚举到的规范单元名，去掉 19 个别名、26 个模板之后的全部。
    // 钉住这个数：解析一旦又在中途中断，这里会立刻从 239 掉到个位数。
    expect(list.units).toHaveLength(239)
    for (const unit of list.units) {
      expect(unit.unit.endsWith('.service')).toBe(true)
      // 模板单元与别名单元都不进列表（CONTRACTS.md 10.1）。
      expect(unit.template).toBe(false)
      // 模板单元是 `foo@.service`；`foo@实例.service` 是真实单元，要留下。
      expect(unit.unit.endsWith('@.service')).toBe(false)
      expect(unit.unit.includes('@.')).toBe(false)
      expect(unit.unit.startsWith('dbus-org.')).toBe(false)
    }
  })

  it('别名单元被换成规范名列出，不是消失', () => {
    // dbus-org.freedesktop.ModemManager1.service 是 ModemManager.service 的别名。
    expect(list.units.some((unit) => unit.unit === 'dbus-org.freedesktop.ModemManager1.service')).toBe(
      false,
    )
    expect(unitNamed('ModemManager.service').load_state).toBe('loaded')
  })

  it('闭集与 null 语义：没有的东西是 null，不是 0 或空串', () => {
    for (const unit of list.units) {
      expect([
        'loaded',
        'not-found',
        'bad-setting',
        'error',
        'merged',
        'masked',
      ]).toContain(unit.load_state)
      expect([
        'active',
        'reloading',
        'inactive',
        'failed',
        'activating',
        'deactivating',
        'maintenance',
      ]).toContain(unit.active_state)
      expect(unit.checked_at.endsWith('Z')).toBe(true)
      if (unit.main_pid === null) {
        // 没进程号的单元不写成 0：那会被读成“有个 PID 0 的进程”。
        expect(typeof unit.sub_state).toBe('string')
      }
    }
    const missing = unitNamed('auditd.service')
    expect(isMissingUnit(missing)).toBe(true)
    expect(missing.main_pid).toBeNull()
    expect(missing.started_at).toBeNull()
    expect(missing.memory_bytes).toBeNull()
    expect(missing.capabilities).toEqual([])
  })

  it('摘要与分组都覆盖全部单元，不静默丢行', () => {
    const counts = summarizeUnits(list.units)
    expect(counts.total).toBe(list.units.length)
    expect(counts.running + counts.stopped + counts.failed).toBeLessThanOrEqual(counts.total)
    // 真机数字：86 个受保护、21 个 not-found。受保护这么多是因为 systemd-* 全在名单内。
    expect(counts.protected).toBe(86)
    expect(counts.missing).toBe(21)

    const groups = groupUnits(list.units)
    expect(
      groups.attention.length +
        groups.running.length +
        groups.stopped.length +
        groups.other.length,
    ).toBe(list.units.length)
    // 失败的、not-found 的都进“需要注意”，不会因为“它没在跑”就被藏进未运行组。
    expect(list.units.filter(needsAttention).length).toBe(counts.attention)
    expect(groups.attention.some((unit) => unit.unit === 'auditd.service')).toBe(true)
    expect(groups.attention.length).toBe(counts.attention)
  })

  it('受保护单元没有任何能力，普通单元按当前状态给动作', () => {
    const journald = unitNamed('systemd-journald.service')
    expect(journald.protection.protected).toBe(true)
    expect(journald.capabilities).toEqual([])
    expect(canOperate(journald, 'stop')).toBe(false)
    expect(canOperate(journald, 'disable')).toBe(false)

    expect(sshDetail.protection.category).toBe('ssh')
    expect(sshDetail.capabilities).toEqual([])

    // 真机上正在跑的 cron：停止/重启/取消开机启动可用，启动不可用（状态已满足）。
    const cron = unitNamed('cron.service')
    expect(cron.capabilities).toEqual(['stop', 'restart', 'disable'])
    expect(canOperate(cron, 'start')).toBe(false)
    expect(canOperate(cron, 'stop')).toBe(true)
  })

  it('筛选只按单元名与描述过滤，不改变单元本身', () => {
    const hits = filterUnits(list.units, 'SSH')
    expect(hits.length).toBeGreaterThan(0)
    expect(
      hits.every((unit) =>
        `${unit.unit}${unit.description}`.toLowerCase().includes('ssh'),
      ),
    ).toBe(true)
    expect(filterUnits(list.units, '')).toHaveLength(list.units.length)
  })
})

describe('真实 VM 数据：详情面板', () => {
  it('受保护单元写明“受保护 + 分类 + 原因”，并明确没有可执行动作', async () => {
    const html = await render(ServiceUnitPanel, { unit: sshDetail })
    expect(html).toContain('ssh.service')
    expect(html).toContain(`受保护：${protectionCategoryLabel(sshDetail.protection.category)}`)
    expect(html).toContain('远程登录')
    expect(html).toContain('SSH 远程登录：停止后可能无法再连上这台服务器')
    expect(html).toContain('这个单元在保护名单里，面板不会对它执行任何操作。')
    expect(html).toMatch(/可执行动作：<strong>无<\/strong>/)
    // 受保护不等于“没数据”：真机上它的 PID / 内存都读到了。
    expect(html).toContain('5173248 字节')
  })

  it('不存在的单元：面板说清“这台机器上没有”，不给按钮', async () => {
    const html = await render(ServiceUnitPanel, { unit: unitNamed('auditd.service') })
    expect(html).toContain('不在这台机器上')
    expect(html).toContain('这台机器上没有这个单元，没有可操作的对象。')
    expect(html).toMatch(/可执行动作：<strong>无<\/strong>/)
  })

  it('运行中的普通单元：动作按 capabilities 逐条显示', async () => {
    const html = await render(ServiceUnitPanel, { unit: unitNamed('cron.service') })
    expect(html).toContain('Regular background program processing daemon')
    expect(html).toContain('运行中')
    expect(html).toMatch(/可执行动作：<strong>停止、重启、取消开机启动<\/strong>/)
    expect(html).toContain('进程号：781')
    expect(html).toContain('483328 字节')
    expect(html).not.toContain('保护名单里，面板不会对它执行任何操作')
  })

  it('未运行的验收单元：只有启动与开机启动，没有停止', async () => {
    const html = await render(ServiceUnitPanel, { unit: inactiveTarget })
    expect(html).toMatch(/可执行动作：<strong>启动、设为开机启动<\/strong>/)
    // 停掉之后重新读：能力是“启动 / 设为开机启动”，与未运行时一致。
    expect(detailAfterStop.capabilities).toEqual(['start', 'enable'])
    const afterHTML = await render(ServiceUnitPanel, { unit: detailAfterStop })
    expect(afterHTML).toMatch(/可执行动作：<strong>启动、设为开机启动<\/strong>/)
    // 内存没采到就是一个 em dash，不是 “0 B”。
    expect(afterHTML).toContain('内存：<span>—</span>')
  })

  it('列表卡片把单元链接到详情页', async () => {
    const html = await render(ServiceUnitCard, { unit: unitNamed('cron.service') })
    expect(html).toContain('href="/services/cron.service"')
    expect(html).toContain('cron.service')
    expect(html).toContain('开机启动')
  })
})

describe('真实 VM 数据：预检结论', () => {
  it('受保护单元：allowed=false，阻塞码 SERVICE_PROTECTED，指纹是完整 sha256', () => {
    expect(preflightProtected.allowed).toBe(false)
    expect(preflightProtected.action).toBe('service.control')
    expect(preflightProtected.normalized_params).toEqual({
      operation: 'stop',
      unit: 'systemd-journald.service',
    })
    expect(preflightProtected.blockers.map((notice) => notice.code)).toEqual(['SERVICE_PROTECTED'])
    expect(preflightProtected.target.identity_fingerprint).toMatch(SHA256)
    expect(preflightProtected.requires_reauth).toBe(false)
    expect(preflightProtected.confirmation_phrase).toBeNull()
    expect(preflightProtected.cancellable).toBe(false)
    expect(preflightProtected.locks).toEqual(['service:systemd-journald.service'])
    expect(canSubmitPreflight(preflightProtected, justBeforeExpiry(preflightProtected))).toBe(false)
    expect(requiresConfirmation(preflightProtected)).toBe(false)
  })

  it('不存在的单元：阻塞码是 SERVICE_NOT_FOUND', () => {
    expect(preflightNotFound.allowed).toBe(false)
    expect(preflightNotFound.blockers.map((notice) => notice.code)).toEqual(['SERVICE_NOT_FOUND'])
    expect(preflightNotFound.locks).toEqual(['service:upanel-doesnotexist.service'])
  })

  it('模板单元：阻塞码是 SERVICE_STATE_CONFLICT（名字不完整，不是“没这个单元”）', () => {
    expect(preflightTemplate.allowed).toBe(false)
    expect(preflightTemplate.blockers.map((notice) => notice.code)).toEqual([
      'SERVICE_STATE_CONFLICT',
    ])
    expect(preflightTemplate.blockers[0]?.message).toContain('实例')
    expect(preflightTemplate.normalized_params).toEqual({
      operation: 'start',
      unit: 'apport-coredump-hook@.service',
    })
  })

  it('可执行的预检：没有阻塞项，锁是 service:<unit>，允许提交', () => {
    expect(preflightStart.allowed).toBe(true)
    expect(preflightStart.blockers).toEqual([])
    expect(preflightStart.normalized_params).toEqual({
      operation: 'start',
      unit: 'upanel-accept.service',
    })
    expect(preflightStart.target.identity_fingerprint).toMatch(SHA256)
    expect(preflightStart.locks).toEqual(['service:upanel-accept.service'])
    expect(preflightStart.requires_reauth).toBe(false)
    expect(canSubmitPreflight(preflightStart, justBeforeExpiry(preflightStart))).toBe(true)
    // 过期之后同一份计划就不许提交了。
    expect(
      canSubmitPreflight(preflightStart, new Date(new Date(preflightStart.expires_at).getTime())),
    ).toBe(false)
    // 服务启停不设确认短语（契约明确不加这道人为确认）。
    expect(preflightStart.confirmation_phrase).toBeNull()
    expect(preflightStart.cancellable).toBe(false)
  })
})

describe('真实 VM 数据：任务结果', () => {
  it('成功的任务：outcome 是 verified，读回的状态与操作一致', () => {
    expect(jobOK.status).toBe('succeeded')
    expect(isTerminal(jobOK.status)).toBe(true)
    expect(jobErrorMessage(jobOK)).toBeNull()
    const result = jobOK.result as {
      outcome: string
      observed_state: {
        verified: boolean
        operation: string
        active_state: string
        previous_state: string
        main_pid: number
      }
      data: { exit_code: number; unit: string; operation: string }
    }
    expect(result.outcome).toBe('verified')
    expect(result.observed_state.verified).toBe(true)
    expect(result.observed_state.operation).toBe('start')
    expect(result.observed_state.active_state).toBe('active')
    expect(result.observed_state.previous_state).toBe('inactive')
    expect(result.data.exit_code).toBe(0)
    expect(result.data.unit).toBe('upanel-accept.service')
  })

  it('失败的任务：result 是空对象（不伪造 observed_state），错误在 error 里', () => {
    expect(jobFail.status).toBe('failed')
    expect(isTerminal(jobFail.status)).toBe(true)
    // 这一条是本轮修掉的真问题：失败任务曾经被当成“带着结果对象的失败”，
    // 契约要求就是空对象，失败原因只放在 error 里。
    expect(jobFail.result).toEqual({})
    const detail = jobErrorMessage(jobFail)
    expect(detail?.code).toBe('COMMAND_FAILED')
    expect(detail?.message).toBe('动作执行失败')
    expect(detail?.hint).toBeNull()
  })

  it('两个任务都指向同一个验收单元，可以并排对比', () => {
    expect(jobOK.action).toBe('service.control')
    expect(jobFail.action).toBe('service.control')
    expect(jobOK.job_id).not.toBe(jobFail.job_id)
    expect(jobOK.cancellable).toBe(false)
    expect(jobOK.finished_at).not.toBeNull()
    expect(jobFail.finished_at).not.toBeNull()
  })
})

describe('真实 VM 数据：404 的两种理由分得开', () => {
  it('别名：说“不存在”，让人改用规范名', () => {
    const alias = errorFixture('detail-alias.json')
    expect(alias.code).toBe('NOT_FOUND')
    expect(alias.message).toBe('服务单元不存在：dbus-org.freedesktop.ModemManager1.service')
  })

  it('模板：说“需要指定实例”，而不是“不存在”', () => {
    const template = errorFixture('detail-template.json')
    expect(template.code).toBe('NOT_FOUND')
    expect(template.message).toContain('模板单元 apport-coredump-hook@.service')
    expect(template.message).toContain('需要指定实例')
  })

  it('真不存在的名字：也是 404，但原文给出了查的那个名字', () => {
    const notfound = errorFixture('detail-notfound.json')
    expect(notfound.code).toBe('NOT_FOUND')
    expect(notfound.message).toContain('upanel-doesnotexist.service')
  })
})
