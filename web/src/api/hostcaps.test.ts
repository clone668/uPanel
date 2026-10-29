// 外部程序能力 DTO 与翻译的测试（P16.1）。
//
// 锁的是三件在这一页最容易撒谎的事：
//   · `ready=false` 必须说“还没探完”，不能说“什么都没装”；
//   · `unknown` 不许并进 `absent`（计数与文案都要分开）；
//   · 缺席行必须带着理由，不能是一格空白。

import { afterEach, describe, expect, it, vi } from 'vitest'

import {
  HOST_CAPS_STATE_HINTS,
  HOST_CAPS_STATE_LABELS,
  countHostCaps,
  describeHostCapsItem,
  fetchHostCapabilities,
  hostCapsHeadline,
  hostCapsVersionLabel,
  type HostCapsItem,
  type HostCapsReport,
  type SystemCapabilitiesData,
} from './hostcaps'

function item(overrides: Partial<HostCapsItem> = {}): HostCapsItem {
  return { id: 'restic', kind: 'program', feature: '备份与恢复', state: 'present', ...overrides }
}

function report(overrides: Partial<HostCapsReport> = {}): HostCapsReport {
  return {
    generated_at: '2026-10-27T13:49:22Z',
    items: [],
    partial: false,
    ...overrides,
  }
}

function payload(overrides: Partial<SystemCapabilitiesData> = {}): SystemCapabilitiesData {
  return { ready: true, host_caps: report(), ...overrides }
}

afterEach(() => {
  vi.unstubAllGlobals()
  vi.restoreAllMocks()
})

describe('三态', () => {
  it('三态各有中文词与说明，缺一不可', () => {
    for (const state of ['present', 'absent', 'unknown'] as const) {
      expect(HOST_CAPS_STATE_LABELS[state], state).toBeTruthy()
      expect(HOST_CAPS_STATE_HINTS[state], state).toBeTruthy()
    }
    expect(HOST_CAPS_STATE_LABELS.unknown).not.toBe(HOST_CAPS_STATE_LABELS.absent)
  })

  it('计数把 unknown 单独算，不并进 absent', () => {
    const counts = countHostCaps([
      item(),
      item({ id: 'docker', state: 'absent' }),
      item({ id: 'samba', state: 'absent' }),
      item({ id: 'nginx', state: 'unknown' }),
    ])
    expect(counts).toEqual({ total: 4, present: 1, absent: 2, unknown: 1 })
    expect(counts.absent + counts.unknown).toBe(3)
  })

  it('空清单的计数是四个 0，不是 NaN、不是 null', () => {
    expect(countHostCaps([])).toEqual({ total: 0, present: 0, absent: 0, unknown: 0 })
  })
})

describe('一行怎么念', () => {
  it('present 给路径；版本读不出时把原因带上', () => {
    expect(describeHostCapsItem(item({ path: '/usr/bin/restic' }))).toBe('/usr/bin/restic')
    expect(
      describeHostCapsItem(
        item({ path: '/usr/bin/restic', reason: '版本命令没有按时结束（超时 3s）' }),
      ),
    ).toBe('/usr/bin/restic（版本命令没有按时结束（超时 3s））')
  })

  it('absent 给理由与提示，两样都要在', () => {
    const text = describeHostCapsItem(
      item({ state: 'absent', reason: '固定目录里没有 restic', hint: 'apt install restic' }),
    )
    expect(text).toContain('固定目录里没有 restic')
    expect(text).toContain('apt install restic')
  })

  it('unknown 不会被念成“没装”', () => {
    const text = describeHostCapsItem(
      item({
        state: 'unknown',
        reason: '固定目录 /usr/bin 读不出来：权限不足',
        hint: 'apt install restic',
      }),
    )
    expect(text).toContain('读不出来')
    expect(text).not.toContain('没有 restic')
    expect(text).toContain('apt install restic')
  })

  it('没有版本时给破折号，不留空白', () => {
    expect(hostCapsVersionLabel(item({ version: '' }))).toBe('—')
    // 字段整个缺席（探测没填）与空串是同一个意思：没有版本。
    expect(hostCapsVersionLabel(item())).toBe('—')
    expect(hostCapsVersionLabel(item({ version: '0.12.1' }))).toBe('0.12.1')
  })
})

describe('报告顶端那句话', () => {
  it('ready=false 说“还没探完”，不说“没有依赖”', () => {
    const text = hostCapsHeadline(payload({ ready: false, host_caps: null }))
    expect(text).toContain('还没探测完')
    expect(text).not.toContain('未安装')
  })

  it('ready=true 但 host_caps 仍是 null 也不画空表', () => {
    const text = hostCapsHeadline(payload({ ready: true, host_caps: null }))
    expect(text).toContain('还没探测完')
  })

  it('partial 时明说“不能当成真的没有”', () => {
    const text = hostCapsHeadline(payload({ host_caps: report({ partial: true }) }))
    expect(text).toContain('不能当成真的没有')
  })

  it('完整时只说可以照着读，不额外承诺', () => {
    const text = hostCapsHeadline(payload())
    expect(text).toContain('可以照着读')
    expect(text).not.toContain('未安装')
  })
})

describe('取数', () => {
  it('打到 /system/capabilities 并原样解出 ready 与 host_caps', async () => {
    const body = {
      data: {
        ready: true,
        host_caps: report({ items: [item({ version: '0.12.1', path: '/usr/bin/restic' })] }),
      },
      request_id: 'rid-hostcaps',
    }
    const mock = vi.fn(() =>
      Promise.resolve(
        new Response(JSON.stringify(body), {
          status: 200,
          headers: { 'Content-Type': 'application/json', 'X-Request-ID': 'rid-hostcaps' },
        }),
      ),
    )
    vi.stubGlobal('fetch', mock)

    const result = await fetchHostCapabilities()
    const url = String((mock.mock.calls as unknown as [string][])[0]?.[0])
    expect(url).toMatch(/\/api\/v1\/system\/capabilities$/)
    expect(result.data.ready).toBe(true)
    expect(result.data.host_caps?.items[0]?.version).toBe('0.12.1')
    expect(result.data.host_caps?.partial).toBe(false)
  })

  it('代理还没探完时 host_caps 是 null，不会被补成空数组', async () => {
    const body = { data: { ready: false, host_caps: null }, request_id: 'rid-hostcaps' }
    vi.stubGlobal(
      'fetch',
      vi.fn(() =>
        Promise.resolve(
          new Response(JSON.stringify(body), {
            status: 200,
            headers: { 'Content-Type': 'application/json', 'X-Request-ID': 'rid-hostcaps' },
          }),
        ),
      ),
    )

    const result = await fetchHostCapabilities()
    expect(result.data.host_caps).toBeNull()
    expect(result.data.ready).toBe(false)
  })
})
