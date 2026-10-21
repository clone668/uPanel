// 进程 DTO 的纯逻辑测试（P05.2）。
//
// 只测判决逻辑，不测“渲染好不好看”：
//   - 每个封闭集合都有中文标签（缺一个就说明契约没对齐）；
//   - 身份是 pid + starttime：参数构造函数不可能只带 pid；
//   - 能力判定只信服务端的 capabilities，不在前端另算一套；
//   - null 不被当成 0（CPU、内存）。

import { afterEach, describe, expect, it, vi } from 'vitest'

import {
  PROCESS_PROTECTION_CATEGORIES,
  PROCESS_PROTECTION_LABELS,
  PROCESS_SIGNALS,
  PROCESS_STATES,
  PROCESS_STATE_LABELS,
  PROCESS_STATE_TYPES,
  SIGNAL_LABELS,
  SIGNAL_VERBS,
  canSignal,
  fetchProcesses,
  filterProcesses,
  formatCPUPercent,
  isExited,
  processProtectionLabel,
  processStateLabel,
  processStateType,
  sameProcess,
  signalNeedsPhrase,
  signalParams,
  summarizeProcesses,
  toProcessSignal,
  type ProcessEntry,
} from './processes'

function entry(overrides: Partial<ProcessEntry> = {}): ProcessEntry {
  return {
    pid: 39515,
    start_time_ticks: 277319,
    ppid: 1,
    owner: 'upanel',
    state: 'sleeping',
    cpu_percent: 0,
    memory_bytes: '2015232',
    command: 'sleep 15',
    unit: 'session-705.scope',
    protection: { protected: false, reasons: [] },
    capabilities: ['TERM', 'KILL'],
    ...overrides,
  }
}

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('封闭集合都有中文标签', () => {
  it('状态、保护类别、信号全部有标签与颜色', () => {
    for (const state of PROCESS_STATES) {
      expect(PROCESS_STATE_LABELS[state], state).toBeTruthy()
      expect(PROCESS_STATE_TYPES[state], state).toBeTruthy()
    }
    for (const category of PROCESS_PROTECTION_CATEGORIES) {
      expect(PROCESS_PROTECTION_LABELS[category], category).toBeTruthy()
    }
    for (const signal of PROCESS_SIGNALS) {
      expect(SIGNAL_LABELS[signal], signal).toBeTruthy()
      expect(SIGNAL_VERBS[signal], signal).toBeTruthy()
    }
  })

  it('未知取值原样返回，不假装成已知状态', () => {
    expect(processStateLabel('weird-state')).toBe('weird-state')
    expect(processStateType('weird-state')).toBe('default')
    expect(processProtectionLabel(undefined)).toBe('受保护')
    expect(processProtectionLabel('nope')).toBe('受保护')
  })
})

describe('身份是 pid + starttime', () => {
  it('signalParams 一定带上 start_time_ticks', () => {
    expect(signalParams(entry(), 'TERM')).toEqual({
      pid: 39515,
      start_time_ticks: 277319,
      signal: 'TERM',
    })
    // 参数里不能出现 pid 之外的目标描述（名字匹配、批量目标、force 之类的开关）。
    expect(Object.keys(signalParams(entry(), 'KILL')).sort()).toEqual([
      'pid',
      'signal',
      'start_time_ticks',
    ])
  })

  it('sameProcess 区分同一个 pid 上的不同进程', () => {
    const before = { pid: 39515, start_time_ticks: 277319 }
    const after = { pid: 39515, start_time_ticks: 400001 }
    expect(sameProcess(before, before)).toBe(true)
    // PID 被重用后就是另一个进程：预检不能认。
    expect(sameProcess(before, after)).toBe(false)
  })
})

describe('能力判定', () => {
  it('canSignal 只信 capabilities', () => {
    const running = entry()
    expect(canSignal(running, 'TERM')).toBe(true)
    expect(canSignal(running, 'KILL')).toBe(true)

    const protectedProcess = entry({
      pid: 1,
      start_time_ticks: 96,
      command: 'init',
      unit: 'init.scope',
      protection: { protected: true, category: 'init', reasons: ['PID 1 是初始化进程'] },
      capabilities: [],
    })
    for (const signal of PROCESS_SIGNALS) {
      expect(canSignal(protectedProcess, signal), `受保护进程不该能 ${signal}`).toBe(false)
    }
  })

  it('toProcessSignal 只接受闭集', () => {
    expect(toProcessSignal('TERM')).toBe('TERM')
    expect(toProcessSignal('KILL')).toBe('KILL')
    expect(toProcessSignal('term')).toBeNull()
    expect(toProcessSignal('SIGKILL')).toBeNull()
    expect(toProcessSignal('9')).toBeNull()
    expect(toProcessSignal(null)).toBeNull()
  })

  it('只有 KILL 需要确认短语', () => {
    expect(signalNeedsPhrase('KILL')).toBe(true)
    expect(signalNeedsPhrase('TERM')).toBe(false)
  })
})

describe('状态判定', () => {
  it('zombie 与 dead 算已退出', () => {
    expect(isExited(entry({ state: 'zombie' }))).toBe(true)
    expect(isExited(entry({ state: 'dead' }))).toBe(true)
    expect(isExited(entry({ state: 'sleeping' }))).toBe(false)
    expect(isExited(entry({ state: 'running' }))).toBe(false)
  })

  it('CPU 为 null 显示占位符，不显示成 0%', () => {
    expect(formatCPUPercent(null)).toBe('—')
    expect(formatCPUPercent(0)).toBe('0.0%')
    expect(formatCPUPercent(0.44)).toBe('0.4%')
  })
})

describe('筛选与摘要', () => {
  it('筛选命中 pid、属主、命令摘要与单元名', () => {
    const processes = [
      entry(),
      entry({
        pid: 784,
        start_time_ticks: 1201,
        owner: 'root',
        command: '/usr/sbin/cron -f',
        unit: 'cron.service',
      }),
    ]
    expect(filterProcesses(processes, '')).toHaveLength(2)
    expect(filterProcesses(processes, '  ')).toHaveLength(2)
    expect(filterProcesses(processes, 'CRON').map((item) => item.pid)).toEqual([784])
    expect(filterProcesses(processes, '39515').map((item) => item.pid)).toEqual([39515])
    expect(filterProcesses(processes, 'upanel').map((item) => item.pid)).toEqual([39515])
    expect(filterProcesses(processes, 'session-705').map((item) => item.pid)).toEqual([39515])
    expect(filterProcesses(processes, 'nonexistent')).toHaveLength(0)
  })

  it('摘要按状态与能力计数', () => {
    const counts = summarizeProcesses([
      entry(),
      entry({ pid: 2, state: 'running', capabilities: ['TERM', 'KILL'] }),
      entry({ pid: 3, state: 'zombie', capabilities: [], memory_bytes: null }),
      entry({
        pid: 1,
        state: 'running',
        protection: { protected: true, category: 'init', reasons: ['PID 1'] },
        capabilities: [],
      }),
    ])
    expect(counts.total).toBe(4)
    expect(counts.running).toBe(2)
    expect(counts.sleeping).toBe(1)
    expect(counts.exited).toBe(1)
    expect(counts.protected).toBe(1)
    expect(counts.killable).toBe(2)
  })
})

describe('查询函数走统一客户端', () => {
  function jsonResponse(body: unknown, status = 200): Response {
    return new Response(JSON.stringify(body), {
      status,
      headers: { 'Content-Type': 'application/json', 'X-Request-ID': 'rid-processes' },
    })
  }

  it('打到约定路径，且不带任何查询参数', async () => {
    const mock = vi.fn(() => Promise.resolve(jsonResponse({ data: {}, request_id: 'rid-processes' })))
    vi.stubGlobal('fetch', mock)

    await fetchProcesses()

    const url = String((mock.mock.calls as unknown as [string][])[0]?.[0])
    expect(url).toMatch(/\/api\/v1\/processes$/)
    // 快照接口不接受参数：带条件去查等于把过滤推到代理里。
    expect(url).not.toContain('?')
  })
})
