// 服务 DTO 的纯逻辑测试（P05.1）。
//
// 只测判决逻辑，不测“渲染好不好看”：
//   - 每个封闭集合都有中文标签（缺一个就说明契约没对齐）；
//   - null 不被当成 0（PID、内存、启动时间）；
//   - 能力判定只信服务端的 capabilities，不在前端另算一套；
//   - 非法单元名不进 URL。

import { afterEach, describe, expect, it, vi } from 'vitest'

import {
  ACTIVE_STATES,
  ACTIVE_STATE_LABELS,
  ENABLEMENTS,
  ENABLEMENT_LABELS,
  LOAD_STATES,
  LOAD_STATE_LABELS,
  PROTECTION_CATEGORIES,
  PROTECTION_CATEGORY_LABELS,
  SERVICE_OPERATIONS,
  SERVICE_OPERATION_LABELS,
  activeStateLabel,
  canOperate,
  enablementLabel,
  fetchService,
  fetchServices,
  filterUnits,
  groupUnits,
  interruptsService,
  isMissingUnit,
  isValidUnitName,
  loadStateLabel,
  needsAttention,
  protectionCategoryLabel,
  summarizeUnits,
  toServiceOperation,
  type ServiceUnit,
} from './services'

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

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('封闭集合都有中文标签', () => {
  it('装载状态、运行状态、开机启动全部有标签', () => {
    for (const state of LOAD_STATES) expect(LOAD_STATE_LABELS[state]).toBeTruthy()
    for (const state of ACTIVE_STATES) expect(ACTIVE_STATE_LABELS[state]).toBeTruthy()
    for (const state of ENABLEMENTS) expect(ENABLEMENT_LABELS[state]).toBeTruthy()
    for (const op of SERVICE_OPERATIONS) expect(SERVICE_OPERATION_LABELS[op]).toBeTruthy()
    for (const category of PROTECTION_CATEGORIES) {
      expect(PROTECTION_CATEGORY_LABELS[category]).toBeTruthy()
    }
  })

  it('未知取值原样返回，不假装成已知状态', () => {
    expect(loadStateLabel('weird-state')).toBe('weird-state')
    expect(activeStateLabel('unknown-state')).toBe('unknown-state')
    expect(enablementLabel('mystery')).toBe('mystery')
    expect(protectionCategoryLabel(undefined)).toBe('受保护')
    expect(protectionCategoryLabel('nope')).toBe('受保护')
  })
})

describe('能力判定', () => {
  it('canOperate 只信 capabilities', () => {
    const running = unit()
    expect(canOperate(running, 'stop')).toBe(true)
    expect(canOperate(running, 'start')).toBe(false)

    const protectedUnit = unit({
      unit: 'ssh.service',
      protection: { protected: true, category: 'ssh', reasons: ['SSH 远程登录'] },
      capabilities: [],
    })
    for (const op of SERVICE_OPERATIONS) {
      expect(canOperate(protectedUnit, op), `受保护单元不该能 ${op}`).toBe(false)
    }
  })

  it('toServiceOperation 只接受闭集', () => {
    expect(toServiceOperation('restart')).toBe('restart')
    expect(toServiceOperation('RESTART')).toBeNull()
    expect(toServiceOperation('systemctl stop sshd')).toBeNull()
    expect(toServiceOperation(null)).toBeNull()
  })

  it('stop / restart 才算“会中断服务”', () => {
    expect(interruptsService('stop')).toBe(true)
    expect(interruptsService('restart')).toBe(true)
    expect(interruptsService('start')).toBe(false)
    expect(interruptsService('enable')).toBe(false)
  })
})

describe('列表摘要与分组', () => {
  it('统计运行中/未运行/失败/受保护/缺失/需要处理', () => {
    const units = [
      unit(),
      unit({ unit: 'myapp.service', active_state: 'inactive', capabilities: ['start', 'enable'] }),
      unit({ unit: 'broken.service', active_state: 'failed', capabilities: ['start', 'stop'] }),
      unit({
        unit: 'upanel-agent.service',
        protection: { protected: true, category: 'self', reasons: ['面板自身的代理'] },
        capabilities: [],
      }),
      unit({ unit: 'ghost.service', load_state: 'not-found', capabilities: [] }),
    ]
    const counts = summarizeUnits(units)
    expect(counts.total).toBe(5)
    // 运行中按 active_state 数，受保护与否不影响：受保护的单元也可能是运行中。
    expect(counts.running).toBe(3)
    expect(counts.stopped).toBe(1)
    expect(counts.failed).toBe(1)
    expect(counts.protected).toBe(1)
    expect(counts.missing).toBe(1)
    // 需要人看一眼的是 failed 与 not-found 两个；“受保护”不是故障，不参与计数。
    expect(counts.attention).toBe(2)
  })

  it('分组不丢单元：四组加起来等于输入', () => {
    const units = [
      unit(),
      unit({ unit: 'a.service', active_state: 'activating' }),
      unit({ unit: 'b.service', active_state: 'failed' }),
      unit({ unit: 'c.service', active_state: 'inactive' }),
    ]
    const groups = groupUnits(units)
    const total =
      groups.attention.length + groups.running.length + groups.stopped.length + groups.other.length
    expect(total).toBe(units.length)
    expect(groups.attention.map((item) => item.unit)).toEqual(['b.service'])
    // activating 属于“其他”，不能被算成运行中或未运行
    expect(groups.other.map((item) => item.unit)).toEqual(['a.service'])
  })

  it('筛选只作用于名字与描述，空关键字返回全部', () => {
    const units = [
      unit(),
      unit({ unit: 'docker.service', description: 'Docker Application Container Engine' }),
    ]
    expect(filterUnits(units, '')).toHaveLength(2)
    expect(filterUnits(units, '  ')).toHaveLength(2)
    expect(filterUnits(units, 'DOCKER').map((item) => item.unit)).toEqual(['docker.service'])
    expect(filterUnits(units, 'cron').map((item) => item.unit)).toEqual(['cron.service'])
    expect(filterUnits(units, 'nonexistent')).toHaveLength(0)
  })

  it('needsAttention 只标记坏掉/缺失的单元', () => {
    expect(needsAttention(unit())).toBe(false)
    expect(needsAttention(unit({ active_state: 'inactive' }))).toBe(false)
    expect(needsAttention(unit({ active_state: 'failed' }))).toBe(true)
    expect(needsAttention(unit({ load_state: 'not-found' }))).toBe(true)
    expect(needsAttention(unit({ load_state: 'bad-setting' }))).toBe(true)
    expect(isMissingUnit(unit({ load_state: 'not-found' }))).toBe(true)
  })
})

describe('单元名校验', () => {
  it('接受契约允许的名字', () => {
    for (const name of [
      'cron.service',
      'getty@tty1.service',
      'snap.foo.bar.service',
      'systemd-journald.service',
    ]) {
      expect(isValidUnitName(name), name).toBe(true)
    }
  })

  it('拒绝路径穿越、元字符与非 service 单元', () => {
    for (const name of [
      '',
      'cron',
      'cron.socket',
      '../../etc/passwd',
      'a..b.service',
      '-rf.service',
      'cron.service;rm -rf /',
      'cron.service --now',
      'a b.service',
    ]) {
      expect(isValidUnitName(name), name).toBe(false)
    }
  })
})

describe('查询函数走统一客户端', () => {
  function jsonResponse(body: unknown, status = 200): Response {
    return new Response(JSON.stringify(body), {
      status,
      headers: { 'Content-Type': 'application/json', 'X-Request-ID': 'rid-services' },
    })
  }

  it('列表与详情各自打到约定路径', async () => {
    const mock = vi.fn(() => Promise.resolve(jsonResponse({ data: {}, request_id: 'rid-services' })))
    vi.stubGlobal('fetch', mock)

    await fetchServices()
    await fetchService('cron.service')

    const urls = (mock.mock.calls as unknown as [string][]).map((call) => String(call[0]))
    expect(urls[0]).toMatch(/\/api\/v1\/services$/)
    expect(urls[1]).toMatch(/\/api\/v1\/services\/cron\.service$/)
  })

  it('非法单元名连请求都不发', async () => {
    const mock = vi.fn(() => Promise.resolve(jsonResponse({ data: {}, request_id: 'rid-services' })))
    vi.stubGlobal('fetch', mock)

    await expect(fetchService('../../etc/shadow')).rejects.toThrow(RangeError)
    expect(mock).not.toHaveBeenCalled()
  })
})
