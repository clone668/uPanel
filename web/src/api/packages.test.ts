// 软件包 DTO 的纯逻辑测试（P11.1）。
//
// 只测判决与"不撒谎"的部分，不测渲染：
//   - 闭合集合（候选关系、源类别、来源）一个不漏地有中文标签，未知值原样透出；
//   - `unknown` 不是 `same`：候选读不到不能渲染成"已是最新"；
//   - 查询参数不把默认值/空值拼进去，免得审计里的 URL 和页面上看到的不一致；
//   - 三个接口都带得起冷路径的预算（默认 15 秒会先超时）。

import { afterEach, describe, expect, it, vi } from 'vitest'

import {
  CANDIDATE_RELATIONS,
  CANDIDATE_RELATION_LABELS,
  CANDIDATE_RELATION_TYPES,
  PACKAGE_POCKETS,
  PACKAGE_POCKET_LABELS,
  PACKAGE_SOURCES,
  PACKAGE_SOURCE_LABELS,
  PACKAGE_QUERY_TIMEOUT_MS,
  buildPackageQuery,
  candidateRelationLabel,
  candidateVersionText,
  entryDisplayName,
  entryPocketText,
  entryStatusLabel,
  fetchPackageUpdates,
  fetchPackages,
  fetchPowerState,
  packagePocketLabel,
  packageSourceLabel,
  type PackageEntry,
} from './packages'

function entry(overrides: Partial<PackageEntry> = {}): PackageEntry {
  return {
    name: 'bash',
    architecture: 'amd64',
    status: 'ii',
    installed: true,
    installed_version: '5.1-6ubuntu1.1',
    candidate_version: '5.1-6ubuntu1.2',
    candidate_relation: 'newer',
    upgradable: true,
    security: false,
    suites: ['jammy-updates'],
    pockets: ['updates'],
    ...overrides,
  }
}

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('闭合集合都有中文标签', () => {
  it('候选关系的四种关系各有标签和展示类型', () => {
    for (const relation of CANDIDATE_RELATIONS) {
      expect(CANDIDATE_RELATION_LABELS[relation]).toBeTruthy()
      expect(CANDIDATE_RELATION_TYPES[relation]).toBeTruthy()
    }
    expect(CANDIDATE_RELATIONS).toEqual(['newer', 'same', 'older', 'unknown'])
  })

  it('候选读不到不是"已是最新"：unknown 不许用 success 样式', () => {
    expect(CANDIDATE_RELATION_TYPES.unknown).not.toBe('success')
    expect(CANDIDATE_RELATION_LABELS.unknown).not.toBe(CANDIDATE_RELATION_LABELS.same)
    // 候选更旧是真实情形，必须有独立的说法。
    expect(CANDIDATE_RELATION_LABELS.older).not.toBe(CANDIDATE_RELATION_LABELS.same)
  })

  it('源类别与来源各有标签', () => {
    for (const pocket of PACKAGE_POCKETS) {
      expect(PACKAGE_POCKET_LABELS[pocket]).toBeTruthy()
    }
    for (const source of PACKAGE_SOURCES) {
      expect(PACKAGE_SOURCE_LABELS[source]).toBeTruthy()
    }
    // `other` 是认不出的第三方源，不能猜成发行版。
    expect(PACKAGE_POCKET_LABELS.other).not.toBe(PACKAGE_POCKET_LABELS.release)
  })

  it('未知值原样透出，不静默吞掉', () => {
    expect(candidateRelationLabel('future-relation')).toBe('future-relation')
    expect(packagePocketLabel('ppa')).toBe('ppa')
    expect(packageSourceLabel('dpkg_query')).toBe('dpkg 数据库')
    expect(packageSourceLabel('who_knows')).toBe('who_knows')
  })
})

describe('查询参数', () => {
  it('空查询不带任何参数', () => {
    expect(buildPackageQuery({})).toEqual({})
  })

  it('默认值不拼进去', () => {
    // installed 缺省由服务端当 true；显式传 undefined 不该变成 installed=false。
    expect(buildPackageQuery({ installed: undefined })).toEqual({})
    expect(buildPackageQuery({ installed: false })).toEqual({
      installed: false,
    })
    expect(buildPackageQuery({ upgradable: false, security: false })).toEqual({})
  })

  it('空串的 query 与 cursor 不算筛选', () => {
    expect(buildPackageQuery({ query: '', cursor: '' })).toEqual({})
  })

  it('筛选与分页原样带上', () => {
    expect(
      buildPackageQuery({
        query: 'libstdc++',
        upgradable: true,
        security: true,
        limit: 50,
        cursor: 'abc',
      }),
    ).toEqual({
      query: 'libstdc++',
      upgradable: true,
      security: true,
      limit: 50,
      cursor: 'abc',
    })
  })
})

describe('查询函数走统一客户端', () => {
  function jsonResponse(body: unknown, status = 200): Response {
    return new Response(JSON.stringify(body), {
      status,
      headers: {
        'Content-Type': 'application/json',
        'X-Request-ID': 'rid-packages',
      },
    })
  }

  it('包列表打到 /packages 并带上筛选', async () => {
    const mock = vi.fn(() =>
      Promise.resolve(
        jsonResponse({
          data: { packages: [entry()] },
          request_id: 'rid-packages',
        }),
      ),
    )
    vi.stubGlobal('fetch', mock)

    await fetchPackages({ query: 'bash', upgradable: true, limit: 20 })

    const url = String((mock.mock.calls as unknown as [string][])[0]?.[0])
    expect(url).toMatch(/\/api\/v1\/packages\?/)
    expect(url).toContain('query=bash')
    expect(url).toContain('upgradable=true')
    expect(url).toContain('limit=20')
    expect(url).not.toContain('installed=')
  })

  it('可升级清单不带参数', async () => {
    const mock = vi.fn(() => Promise.resolve(jsonResponse({ data: {}, request_id: 'rid-updates' })))
    vi.stubGlobal('fetch', mock)

    await fetchPackageUpdates()

    const url = String((mock.mock.calls as unknown as [string][])[0]?.[0])
    expect(url).toMatch(/\/api\/v1\/packages\/updates$/)
  })

  it('重启标志打到 /system/power-state', async () => {
    const mock = vi.fn(() => Promise.resolve(jsonResponse({ data: {}, request_id: 'rid-power' })))
    vi.stubGlobal('fetch', mock)

    await fetchPowerState()

    const url = String((mock.mock.calls as unknown as [string][])[0]?.[0])
    expect(url).toMatch(/\/api\/v1\/system\/power-state$/)
  })

  it('三个接口的预算都覆盖得了冷路径', () => {
    // 冷路径实测 21～27 秒；默认 15 秒会先超时，页面于是报"请求超时"。
    expect(PACKAGE_QUERY_TIMEOUT_MS).toBeGreaterThanOrEqual(30_000)
  })
})

describe('一行的展示文本：读不到不许说成"已是最新"', () => {
  it('有候选就原样显示候选版本', () => {
    expect(candidateVersionText(entry())).toBe('5.1-6ubuntu1.2')
  })

  it('候选读不到（unknown）说"读不到"，不是"已是最新"', () => {
    const row = entry({
      candidate_version: null,
      candidate_relation: 'unknown',
    })
    expect(candidateVersionText(row)).toBe('读不到')
    expect(candidateVersionText(row)).not.toBe(CANDIDATE_RELATION_LABELS.same)
  })

  it('apt 明说没有候选时是"—"，与读不到区分开', () => {
    expect(candidateVersionText(entry({ candidate_version: null, candidate_relation: 'same' }))).toBe('—')
  })

  it('rc 是"仅剩配置"，既不是装着也不是没装过', () => {
    expect(entryStatusLabel(entry())).toBe('已安装')
    expect(entryStatusLabel(entry({ installed: false, status: 'rc' }))).toBe('仅剩配置')
    expect(entryStatusLabel(entry({ installed: false, status: 'un' }))).toBe('未安装（un）')
  })

  it('来源翻成中文标签；没有候选时明说"无候选"', () => {
    expect(entryPocketText(entry())).toBe(PACKAGE_POCKET_LABELS.updates)
    expect(entryPocketText(entry({ pockets: ['updates', 'security'] }))).toBe(
      `${PACKAGE_POCKET_LABELS.updates} / ${PACKAGE_POCKET_LABELS.security}`,
    )
    expect(
      entryPocketText(
        entry({
          candidate_version: null,
          candidate_relation: 'unknown',
          pockets: [],
        }),
      ),
    ).toBe('无候选')
    expect(entryPocketText(entry({ pockets: [] }))).toBe('—')
  })
})

describe('包名展示：架构限定只出现一次', () => {
  it('dpkg 已经带上 :amd64 的名字不再补一遍', () => {
    const row = entry({ name: 'bind9-libs:amd64', architecture: 'amd64' })
    expect(entryDisplayName(row)).toBe('bind9-libs:amd64')
    expect(entryDisplayName(row)).not.toContain(':amd64:amd64')
  })

  it('不带限定的名字补上架构；all 架构不补', () => {
    expect(entryDisplayName(entry({ name: 'bash', architecture: 'amd64' }))).toBe('bash:amd64')
    expect(entryDisplayName(entry({ name: 'systemd-timesyncd', architecture: 'all' }))).toBe('systemd-timesyncd')
  })

  it('名字里带别的架构后缀时按原样保留，不拼错', () => {
    const row = entry({ name: 'bash:i386', architecture: 'i386' })
    expect(entryDisplayName(row)).toBe('bash:i386')
  })
})
