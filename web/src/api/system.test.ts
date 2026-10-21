// 系统快照 DTO 的纯逻辑测试。
//
// 重点：段状态三态（ok/error/unavailable）不能混成“空数据”或“0”，
// degraded 要能列出到底是哪几段不可用。

import { afterEach, describe, expect, it, vi } from 'vitest'

import {
  SYSTEM_SECTIONS,
  SYSTEM_SECTION_LABELS,
  degradedSections,
  fetchSystemSummary,
  isSectionOk,
  type SystemSummary,
} from './system'

function summary(overrides: Partial<SystemSummary> = {}): SystemSummary {
  return {
    collected_at: '2026-10-21T09:00:00Z',
    interval_ms: 5000,
    stale: false,
    degraded: false,
    host: {
      status: 'ok',
      hostname: 'upanel-2404',
      os_pretty_name: 'Ubuntu 24.04.5 LTS',
      kernel_release: '6.8.0-147-generic',
      arch: 'x86_64',
      uptime_seconds: 3600,
    },
    cpu: { status: 'ok', logical_cores: 2, usage_percent: 3.2 },
    memory: {
      status: 'ok',
      total_bytes: '2048000000',
      free_bytes: '1024000000',
      available_bytes: '1536000000',
      buffers_bytes: '10000000',
      cached_bytes: '500000000',
      used_bytes: '512000000',
      swap_total_bytes: '0',
      swap_free_bytes: '0',
    },
    load: { status: 'ok', load1: 0.1, load5: 0.2, load15: 0.15, runnable: 1, threads: 120 },
    network: { status: 'ok', interfaces: [] },
    ...overrides,
  }
}

afterEach(() => {
  vi.unstubAllGlobals()
  vi.restoreAllMocks()
})

describe('段状态', () => {
  it('五段都有中文名，与契约一一对应', () => {
    expect(SYSTEM_SECTIONS).toEqual(['host', 'cpu', 'memory', 'load', 'network'])
    for (const key of SYSTEM_SECTIONS) {
      expect(SYSTEM_SECTION_LABELS[key], key).toBeTruthy()
    }
  })

  it('只有 ok 算可用；error 与 unavailable 都不算', () => {
    expect(isSectionOk({ status: 'ok' })).toBe(true)
    expect(isSectionOk({ status: 'error' })).toBe(false)
    expect(isSectionOk({ status: 'unavailable' })).toBe(false)
  })

  it('degraded 时列出不可用的段，顺序固定', () => {
    const value = summary({
      degraded: true,
      cpu: { status: 'error', error: '读取 /proc/stat 失败', logical_cores: 0, usage_percent: null },
      network: { status: 'unavailable' },
    })
    expect(degradedSections(value)).toEqual(['cpu', 'network'])
  })

  it('全部 ok 时清单为空，不编造“部分不可用”', () => {
    expect(degradedSections(summary())).toEqual([])
  })
})

describe('查询', () => {
  it('打到 /system/summary 并解包信封', async () => {
    const mock = vi.fn(() =>
      Promise.resolve(
        new Response(JSON.stringify({ data: { collected_at: 'x' }, request_id: 'rid-sys' }), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        }),
      ),
    )
    vi.stubGlobal('fetch', mock)

    const result = await fetchSystemSummary()
    expect(result.requestId).toBe('rid-sys')
    const calls = mock.mock.calls as unknown as [string][]
    expect(String(calls[0]?.[0])).toMatch(/\/api\/v1\/system\/summary$/)
  })
})
