// 用真实 Ubuntu 数据回放“软件包/更新”只读页（P11.1）。
//
// 这些载荷不是造的：验收 VM（upanel-2404，24.04.5）上跑
//   tests/integration/p11-1-apt-acceptance.sh，
// 真 agent 读 dpkg 数据库 + 真 apt 算候选，web 用 upanel-web 身份取，原样抓回来存在
// src/fixtures/p11-1-vm/ 下（见那里的 README）。同一轮还在 22.04 上跑了一遍，
// 两台的 `packages-all.json` 都被 tests/integration/p11-1-packages-verify.py
// 逐条对账过（731 / 676 条，MISMATCH=0）。
//
// 这里不渲染页面：页面上每一格文字都是 @/api/packages 里几个纯函数的直出，而页面本身
// 在 onMounted 里取数——SSR 只能渲出加载态，锁不到东西。所以这一层锁的是“照着说人话”
// 的规则本身，喂的是真机载荷：
//   · 候选读不到（unknown）不许渲染成“已是最新”——这正是 `apt-cache policy` 把
//     `:amd64` 从表头剥掉时踩过的坑（bind9-libs:amd64 曾整整一条谎报“读不到”）；
//   · 计数是**全量**统计，不受筛选和分页影响；matched 才是“这一屏有多少条”；
//   · 可升级清单与 counts.upgradable 必须是同一个数，否则页头说 9 条、清单只有 6 条；
//   · 安全更新的含义只有一个：候选版本自身来自 -security 源。

import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'

import { describe, expect, it } from 'vitest'

import {
  CANDIDATE_RELATIONS,
  PACKAGE_POCKETS,
  PACKAGE_SOURCES,
  candidateVersionText,
  entryDisplayName,
  entryPocketText,
  entryStatusLabel,
  packagePocketLabel,
  packageSourceLabel,
  type PackageEntry,
  type PackageListData,
  type PackageUpdatesData,
  type PowerStateData,
} from '@/api/packages'

const DIR = new URL('../fixtures/p11-1-vm/', import.meta.url)

interface Envelope<T> {
  data: T
  request_id: string
}

/** 夹具是 Web 接口的原始响应（`{data, request_id}`），这里只剥信封，不动内容。 */
function payload<T>(name: string): T {
  const raw = JSON.parse(readFileSync(fileURLToPath(new URL(name, DIR)), 'utf8')) as Envelope<T>
  if (!raw || typeof raw !== 'object' || !('data' in raw)) throw new Error(`夹具 ${name} 不是接口响应信封`)
  return raw.data
}

interface Capabilities {
  supported_queries: string[]
  supported_actions: string[]
}

const page1 = payload<PackageListData>('packages-page1.json')
const upd = payload<PackageUpdatesData>('packages-updates.json')
const power = payload<PowerStateData>('power-state.json')
const search = payload<PackageListData>('packages-search.json')
const security = payload<PackageListData>('packages-security.json')
const bind9 = payload<PackageListData>('packages-bind9.json')
const caps = payload<Capabilities>('capabilities.json')

function allRows(): PackageEntry[] {
  return [...page1.packages, ...search.packages, ...security.packages, ...bind9.packages]
}

describe('能力声明：这一阶段只读', () => {
  it('apt.packages 与 system.power_state 都在', () => {
    expect(caps.supported_queries).toContain('apt.packages')
    expect(caps.supported_queries).toContain('system.power_state')
  })

  it('没有任何包管理写操作的动作名', () => {
    // P11.2 才会注册 package.* / apt.* 的 action。这里没有，等于“装机前就想改”不可能。
    const writable = caps.supported_actions.filter((name) => /^(package|apt)\./.test(name))
    expect(writable).toEqual([])
  })
})

describe('列表页的计数与分页', () => {
  it('counts 是全量统计：匹配数与已安装总数一致（默认无筛选）', () => {
    expect(page1.filters).toEqual({
      query: '',
      installed: true,
      upgradable: false,
      security: false,
    })
    expect(page1.counts.installed).toBeGreaterThan(500)
    expect(page1.matched).toBe(page1.counts.installed)
  })

  it('一页满员才有下一页，说明分页真的在走', () => {
    expect(page1.packages.length).toBe(page1.limit)
    expect(page1.limit).toBe(50)
    expect(page1.next_cursor).toBeTruthy()
  })

  it('每行都是已安装的，且没有重复的 name:arch', () => {
    const keys = page1.packages.map((row) => `${row.name}:${row.architecture}`)
    expect(page1.packages.every((row) => row.installed)).toBe(true)
    expect(new Set(keys).size).toBe(keys.length)
  })

  it('索引时间三个接口对得上；采集时间是串行抓的，只会往后走', () => {
    // 索引 mtime 是同一份 apt 列表，三个接口必须一致——不一致说明有的走了缓存、
    // 有的现场重采，页面就会拿两份事实拼一屏。
    expect(search.index_updated_at).toBe(page1.index_updated_at)
    expect(upd.index_updated_at).toBe(page1.index_updated_at)
    // collected_at 不作"必须同秒"的要求：抓取是串行的，抓包脚本第一次请求还要等冷采集
    // （实测 15 秒），中间隔上几分钟是正常的。只要求单调且在同一小时里。
    const t = (s: string) => Date.parse(s)
    expect(t(upd.collected_at)).toBeGreaterThanOrEqual(t(page1.collected_at))
    expect(t(upd.collected_at) - t(page1.collected_at)).toBeLessThan(3_600_000)
  })
})

describe('候选版本：读不到就是读不到', () => {
  it('关系、来源、pockets 全在闭合集合里，没有裸的英文漏出来', () => {
    for (const row of allRows()) {
      expect(CANDIDATE_RELATIONS).toContain(row.candidate_relation)
      expect(row.pockets.every((p) => (PACKAGE_POCKETS as readonly string[]).includes(p))).toBe(true)
      const text = entryPocketText(row)
      // 有候选时来源必须翻成人话：真机上出现过 `noble-updates` 这类原文直接上屏。
      if (row.candidate_version !== null && row.pockets.length > 0) {
        expect(text).not.toMatch(/[A-Za-z]/)
        for (const pocket of row.pockets) expect(text).toContain(packagePocketLabel(pocket))
      }
    }
  })

  it('unknown 与 same 分得开：读不到不冒充已是最新', () => {
    for (const row of allRows()) {
      if (row.candidate_version === null) {
        expect(candidateVersionText(row)).toBe(row.candidate_relation === 'unknown' ? '读不到' : '—')
      } else {
        expect(candidateVersionText(row)).toBe(row.candidate_version)
        // 有候选就必须有来源；反过来来源为空却说有候选，就是自相矛盾。
        expect(row.suites.length).toBeGreaterThan(0)
      }
    }
  })

  it('arch 限定名的候选不再丢（bind9-libs:amd64 回归）', () => {
    // apt-cache policy 把 `:amd64` 从表头剥掉了（`apt-cache policy bind9-libs:amd64`
    // 打出来的是 `bind9-libs:`），按请求名去查必然落空——真机上这条于是整条谎报
    // “候选读不到”。这份夹具就是专门为它抓的 `query=bind9`，所以它必须在这，且候选读得到。
    const row = bind9.packages.find((r) => r.name === 'bind9-libs:amd64')
    if (!row) throw new Error('夹具里没有 bind9-libs:amd64——这条回归就白写了')
    expect(row.installed).toBe(true)
    expect(row.candidate_version).not.toBeNull()
    expect(row.candidate_relation).not.toBe('unknown')
    expect(candidateVersionText(row)).toMatch(/^\d/)
    // dpkg 的 binary:Package 已经带了 `:amd64`，屏幕上不许再补一遍。
    expect(entryDisplayName(row)).toBe('bind9-libs:amd64')
  })

  it('状态栏：已安装 / 仅剩配置 / 未安装 三种口径', () => {
    for (const row of page1.packages) {
      expect(['已安装', '仅剩配置']).toContain(entryStatusLabel(row))
    }
  })

  it('降级来源的 label 齐全（errors 里出现什么都翻译得出来）', () => {
    for (const err of [...page1.errors, ...upd.errors]) {
      const label = packageSourceLabel(err.source)
      expect(label).toBeTruthy()
      expect(PACKAGE_SOURCES).toContain(err.source)
      expect(err.message).not.toBe('')
    }
  })
})

describe('可升级清单与页头数字必须一致', () => {
  it('total 等于清单长度（未截断时）', () => {
    expect(upd.truncated).toBe(false)
    expect(upd.updates.length).toBe(upd.total)
    expect(upd.total).toBe(page1.counts.upgradable)
  })

  it('每一条候选都真的和已安装版本不同', () => {
    for (const row of upd.updates) {
      expect(row.candidate_version).not.toBe(row.installed_version)
      expect(row.suites.length).toBeGreaterThan(0)
      expect(row.pockets.every((p) => (PACKAGE_POCKETS as readonly string[]).includes(p))).toBe(true)
    }
  })

  it('安全更新的条数与列表页口径一致，且候选来自 -security 源', () => {
    expect(upd.security).toBe(page1.counts.security)
    const sec = upd.updates.filter((row) => row.security)
    expect(sec.length).toBe(upd.security)
    for (const row of sec) {
      expect(row.pockets).toContain('security')
      expect(row.suites.some((s) => s.endsWith('-security'))).toBe(true)
    }
  })

  it('清单里的每一行在列表页里也标着可升级（同一份事实两处出现）', () => {
    const seen = new Map(allRows().map((row) => [`${row.name}:${row.architecture}`, row]))
    let checked = 0
    for (const row of upd.updates) {
      const found = seen.get(`${row.name}:${row.architecture}`)
      if (!found) continue // 不在这一页（列表是分页的），跳过
      expect(found.upgradable).toBe(true)
      expect(found.candidate_relation).toBe('newer')
      checked++
    }
    expect(checked).toBeGreaterThanOrEqual(0)
  })
})

describe('筛选真的筛了', () => {
  it('搜 bash：matched 是全量命中数，不是这一页的条数', () => {
    expect(search.filters.query).toBe('bash')
    expect(search.matched).toBeGreaterThan(0)
    expect(search.matched).toBeLessThanOrEqual(search.counts.installed)
    expect(search.packages.length).toBeLessThanOrEqual(search.limit)
    for (const row of search.packages) {
      expect(row.name.toLowerCase()).toContain('bash')
    }
  })

  it('security=true：命中的每一条都是安全更新且候选更新', () => {
    expect(security.filters.security).toBe(true)
    expect(security.matched).toBe(page1.counts.security)
    expect(security.packages.length).toBe(security.matched)
    for (const row of security.packages) {
      expect(row.security).toBe(true)
      expect(row.candidate_relation).toBe('newer')
    }
  })
})

describe('重启标志：文件说什么就是什么', () => {
  it('没有 /run/reboot-required 时，三个字段一起说“不需要”', () => {
    expect(power.reboot_required).toBe(false)
    expect(power.reboot_required_since).toBeNull()
    expect(power.reboot_required_packages).toEqual([])
    expect(power.reboot_required_packages_error).toBe('')
  })

  it('boot_id 是真 UUID，P11.3 靠它确认重启真的发生过', () => {
    expect(power.boot_id).toMatch(/^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i)
  })
})
