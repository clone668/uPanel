// 用真实 Ubuntu 数据回放“应用卡片页”（P09.5）。
//
// 这些 JSON 不是造的：验收 VM（upanel-2404）上跑完
//   7 个夹具容器（带日志的、200 的、500 的、发布到服务器 LAN 地址的、停着的、
//     restart=always 的、会被重建的）+ 一个每 5 秒写日志的 systemd 单元
//   → 建 13 个应用（link / systemd / docker 各有各的状态）
//   → 健康探测（一个 ok、一个 error）→ 重建容器 → 分组、改名、调序、收藏
//   → 用 service.control 把夹具单元停掉再起来 → 依赖卷离线门禁
//   → viewer 收藏自己的 → 全量列表
// 之后，由 tests/integration/p09-5-apps-ui-acceptance.sh 原样抓回来，
// 存在 src/fixtures/p09-5-vm/ 下（见那里的 README）。
//
// 为什么非要真实载荷：手写夹具天然自洽。这一层锁的是“自洽的假象”——
//   · 手工写的 App 很容易只填一个 `status: 'ok'`，而契约里 `runtime_state` 与
//     `http_state` 是**两个字段**，“容器在跑但 500”这件事只能靠两个字段同时出现才说得出；
//   · 手工写的 capabilities 很容易跟着“它是个 systemd 服务”走，而真机上运行中的单元
//     **没有** start，受保护单元**只有** logs，重建后的绑定**连 logs 都没有**；
//   · 手工写的分组很容易忘记 sort_order 是页面分桶的唯一依据。
// 所以这里的断言全都打在“页面照着说人话”的前提上，不是打在字段名好不好看上。

import { readFileSync } from 'node:fs'

import { describe, expect, it, vi } from 'vitest'
import { createSSRApp, h, type Component, type VNodeChild } from 'vue'
import { renderToString } from 'vue/server-renderer'

import {
  APP_TYPES,
  DEPENDENCY_PROTECTIONS,
  HTTP_STATES,
  HTTP_STATE_LABELS,
  RUNTIME_STATES,
  RUNTIME_STATE_LABELS,
  UNGROUPED_KEY,
  FAVORITES_KEY,
  availableActions,
  bucketApps,
  controlRequest,
  frequentEntries,
  iconGlyph,
  matchesAppQuery,
  openTarget,
  primaryAction,
  sortApps,
  type App,
  type AppDiscoveryData,
  type AppGroup,
  type AppHealthState,
  type AppLogsData,
} from '@/api/apps'
import type { JobDTO } from '@/api/jobs'
import { summarizeAppJob } from '@/utils/jobresult'
import AppCard from './AppCard.vue'
import AppDiscoveryPanel from './AppDiscoveryPanel.vue'

// naive-ui 在 SSR 里要挂样式（需要 document）。换成最小实现：文字、disabled 与 type 带出来。
// 未声明的属性会被 Vue 的属性穿透挂到根节点上，所以 `data-action` / `href` 仍然可断言。
vi.mock('naive-ui', async () => {
  const { h } = await import('vue')
  const stub = (name: string) => ({
    name: `Stub${name}`,
    props: [
      'title', 'description', 'status', 'code', 'disabled', 'value', 'options', 'checked',
      'type', 'size', 'loading', 'placeholder', 'maxlength', 'bordered', 'secondary',
      'autosize', 'showPasswordOn', 'tag', 'href', 'target', 'rel', 'quaternary', 'strong',
      'count', 'text', 'round',
    ],
    setup(
      props: {
        title?: unknown
        description?: unknown
        disabled?: unknown
        type?: unknown
        tag?: unknown
        href?: unknown
        target?: unknown
        rel?: unknown
      },
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
        const { tag, href, target, rel } = props as {
          tag?: unknown
          href?: unknown
          target?: unknown
          rel?: unknown
        }
        if (props.disabled === true) attrs['data-disabled'] = 'true'
        if (typeof props.type === 'string') attrs['data-type'] = props.type
        // naive-ui 的 NButton 带 tag="a" 时真的渲染成一个锚点。这里照做：不然“打开
        // 是个真链接”这条只能靠读源码相信，渲染出来的 HTML 里看不到 href。
        if (tag === 'a') {
          if (typeof href === 'string') attrs['href'] = href
          if (typeof target === 'string') attrs['target'] = target
          if (typeof rel === 'string') attrs['rel'] = rel
          return h('a', attrs, children)
        }
        return h('div', attrs, children)
      }
    },
  })
  return {
    NAlert: stub('alert'),
    NButton: stub('button'),
    NCard: stub('card'),
    NEmpty: stub('empty'),
    NInput: stub('input'),
    NResult: stub('result'),
    NSelect: stub('select'),
    NSpin: stub('spin'),
    NSwitch: stub('switch'),
    NTag: stub('tag'),
    NTooltip: stub('tooltip'),
    useMessage: () => ({ success: () => {}, error: () => {}, warning: () => {}, info: () => {} }),
  }
})

function rawText(name: string): string {
  return readFileSync(new URL(`../fixtures/p09-5-vm/${name}`, import.meta.url), 'utf8')
}

interface Envelope<T> {
  data: T
  request_id: string
}

function payload<T>(name: string): T {
  return (JSON.parse(rawText(name)) as Envelope<T>).data
}

/** 夹具里必须有；没有就当场炸掉（不回一个 undefined 让下面的断言在错的地方通过）。 */
function need<T>(value: T | undefined | null, what: string): T {
  if (value === undefined || value === null) throw new Error(`夹具里没有${what}`)
  return value
}

async function render(component: Component, props: Record<string, unknown>): Promise<string> {
  const app = createSSRApp({ render: () => h(component, props) })
  return renderToString(app)
}

// ---------------------------------------------------------------- 现场

const apps = payload<{ items: App[]; next_cursor: string | null }>('apps.json')
const groups = payload<{ items: AppGroup[]; next_cursor: string | null }>('app-groups.json')
const discovery = payload<AppDiscoveryData>('discovery.json')

const linkApp = need(apps.items.find((a) => a.type === 'link'), '入口（link）应用')
const fiveHundred = need(apps.items.find((a) => a.http_state === 'error'), '500 的 docker 应用')
const healthy = need(apps.items.find((a) => a.http_state === 'ok'), '探测 ok 的 docker 应用')
const rebuilt = need(
  apps.items.find((a) => a.type === 'docker' && a.runtime_state === 'unknown'),
  '重建后旧绑定的 docker 应用',
)
const protectedUnit = need(
  apps.items.find((a) => a.binding.unit === 'ssh.service'),
  '受保护单元的应用',
)
const managed = need(
  apps.items.find((a) => a.dependency_protection === 'managed'),
  '受依赖卷保护的应用',
)
// 停机那一刻的库里没有“stopped 的 systemd 应用”这一条——收尾时它已经重新启动了。
// 但停机瞬间的载荷是真实抓下来的（wait_app 轮询时写的那一份），所以用单条载荷，
// 而不是从最终列表里找一个当时并不存在的成员。
const stoppedUnit = need(
  payload<App>('app-sysd-stopped.json'),
  '停机瞬间的 systemd 应用载荷',
)
const runningUnit = need(
  payload<App>('app-sysd-started.json'),
  '重新启动后的 systemd 应用载荷',
)

describe('真机载荷：App 的字段全在契约的闭集里', () => {
  it('列表是 {items, next_cursor}，且有内容', () => {
    expect(Array.isArray(apps.items)).toBe(true)
    expect(apps.items.length).toBeGreaterThan(0)
    expect(apps.next_cursor === null || typeof apps.next_cursor === 'string').toBe(true)
  })

  it('每一条应用的键都在 openapi App 的闭集里（多一个都算编）', () => {
    const allowed = new Set([
      'id',
      'name',
      'description',
      'icon_key',
      'group_id',
      'type',
      'binding',
      'public_url',
      'health_check',
      'volume_dependencies',
      'favorite',
      'version',
      'created_at',
      'updated_at',
      'runtime_state',
      'runtime_note',
      'http_state',
      'last_checked_at',
      'dependency_protection',
      'dependency_protection_note',
      'capabilities',
    ])
    for (const app of apps.items) {
      const extra = Object.keys(app).filter((k) => !allowed.has(k))
      expect({ id: app.id, extra }).toEqual({ id: app.id, extra: [] })
    }
  })

  it('健康检查的定义里没有地方填主机、IP 或 URL（SSRF 是结构性的）', () => {
    // 这条是“不可探测云元数据/其他主机”的结构证据：不是“填完再校验”，是没有地方填。
    const allowed = new Set([
      'enabled',
      'target_id',
      'scheme',
      'port',
      'path',
      'expected_status_codes',
      'interval_seconds',
      'timeout_seconds',
    ])
    for (const app of apps.items) {
      const extra = Object.keys(app.health_check).filter((k) => !allowed.has(k))
      expect({ id: app.id, extra }).toEqual({ id: app.id, extra: [] })
      if (app.health_check.enabled) {
        expect(app.health_check.target_id).toMatch(/^(local|cntr)_[a-z0-9][a-z0-9_-]{0,47}$/)
      } else {
        expect(app.health_check.target_id).toBe('')
      }
    }
  })

  it('闭集字段的取值也都在闭集里', () => {
    for (const app of apps.items) {
      expect(APP_TYPES).toContain(app.type)
      expect(RUNTIME_STATES).toContain(app.runtime_state)
      expect(HTTP_STATES).toContain(app.http_state)
      expect(DEPENDENCY_PROTECTIONS).toContain(app.dependency_protection)
      expect(typeof app.favorite).toBe('boolean')
      expect(app.version).toBeGreaterThanOrEqual(1)
      for (const cap of Object.values(app.capabilities)) expect(typeof cap).toBe('boolean')
    }
  })

  it('绑定只有一个字段，由 type 决定', () => {
    for (const app of apps.items) {
      const keys = Object.keys(app.binding).filter(
        (k) => (app.binding as Record<string, unknown>)[k] !== undefined,
      )
      expect({ id: app.id, type: app.type, count: keys.length }).toEqual({
        id: app.id,
        type: app.type,
        count: app.type === 'link' ? 0 : 1,
      })
    }
  })
})

describe('真机载荷：运行时与入口状态是两个字段，谁也不代表谁', () => {
  it('容器在跑但 HTTP 500：两个事实同时成立', () => {
    expect(fiveHundred.runtime_state).toBe('running')
    expect(fiveHundred.http_state).toBe('error')
    expect(fiveHundred.last_checked_at).not.toBeNull()
  })

  it('两个状态的中文标签不是同一个词（合成一句就会把它藏起来）', () => {
    expect(RUNTIME_STATE_LABELS[fiveHundred.runtime_state]).not.toBe(
      HTTP_STATE_LABELS[fiveHundred.http_state],
    )
    expect(RUNTIME_STATE_LABELS[fiveHundred.runtime_state]).toContain('运行')
    expect(HTTP_STATE_LABELS[fiveHundred.http_state]).toContain('异常')
  })

  it('探测 ok 的那一个：两个字段都说好，但它们是各自得出的', () => {
    expect(healthy.http_state).toBe('ok')
    expect(healthy.runtime_state).toBe('running')
    expect(healthy.last_checked_at).not.toBeNull()
  })

  it('探测本身只走回环：结论里的地址与 URL 都在 127.0.0.1 上', () => {
    const bad = payload<AppHealthState>('health-error.json')
    expect(bad.status_code).toBe(500)
    expect(bad.address).toBe('127.0.0.1')
    expect(bad.url ?? '').toMatch(/^http:\/\/127\.0\.0\.1:\d+/)
    const good = payload<AppHealthState>('health-ok.json')
    expect(good.status_code).toBe(200)
    expect(good.address).toBe('127.0.0.1')
    expect(good.url ?? '').toMatch(/^http:\/\/127\.0\.0\.1:\d+/)
  })

  it('没启用健康检查的应用是 not_applicable，不是 unknown', () => {
    const noProbe = need(
      apps.items.find((a) => a.type === 'docker' && a.runtime_state === 'running' && !a.health_check.enabled),
      '没启用健康检查的 docker 应用',
    )
    expect(noProbe.http_state).toBe('not_applicable')
    expect(noProbe.last_checked_at).toBeNull()
  })

  it('入口（link）没有运行时：runtime_state 与 protection 都是 not_applicable，note 是 null', () => {
    expect(linkApp.runtime_state).toBe('not_applicable')
    expect(linkApp.runtime_note).toBeNull()
    expect(linkApp.dependency_protection).toBe('not_applicable')
    expect(linkApp.dependency_protection_note).toBeNull()
  })

  it('重建之后旧绑定是 unknown 并且说清为什么', () => {
    expect(rebuilt.runtime_state).toBe('unknown')
    expect(rebuilt.runtime_note).toContain('重新确认绑定')
  })
})

describe('真机载荷：能力来自底层对象——按钮不是猜出来的', () => {
  it('入口只有 open：没有启停，也没有日志', () => {
    expect(linkApp.capabilities.open).toBe(true)
    expect(linkApp.capabilities.start).toBe(false)
    expect(linkApp.capabilities.stop).toBe(false)
    expect(linkApp.capabilities.restart).toBe(false)
    expect(linkApp.capabilities.logs).toBe(false)
    expect(availableActions(linkApp.capabilities)).toEqual(['open'])
    expect(primaryAction(linkApp.capabilities)).toBe('open')
  })

  it('受保护单元：三项生命周期全 false，但日志还在', () => {
    expect(protectedUnit.runtime_state).toBe('running')
    expect(protectedUnit.capabilities.start).toBe(false)
    expect(protectedUnit.capabilities.stop).toBe(false)
    expect(protectedUnit.capabilities.restart).toBe(false)
    expect(protectedUnit.capabilities.logs).toBe(true)
    expect(availableActions(protectedUnit.capabilities)).toEqual(['logs'])
  })

  it('重建后的绑定连日志都没有（看不见的东西没有按钮）', () => {
    expect(rebuilt.capabilities.start).toBe(false)
    expect(rebuilt.capabilities.stop).toBe(false)
    expect(rebuilt.capabilities.restart).toBe(false)
    expect(rebuilt.capabilities.logs).toBe(false)
    expect(availableActions(rebuilt.capabilities)).toEqual([])
  })

  it('停下来的 systemd 应用：出现 start，不出现 stop', () => {
    expect(stoppedUnit.capabilities.start).toBe(true)
    expect(stoppedUnit.capabilities.stop).toBe(false)
    expect(primaryAction(stoppedUnit.capabilities)).toBe('start')
  })

  it('同一条应用停机再启动：ID、单元、版本都没变，变的只有状态和能力', () => {
    // 这两份载荷是同一次验收里、同一条应用的前后两拍（真停过一次、真起过一次）。
    // 如果卡片在这里“换了个应用”，那说明摘要不是从绑定算出来的。
    expect(runningUnit.id).toBe(stoppedUnit.id)
    expect(runningUnit.binding).toEqual(stoppedUnit.binding)
    expect(runningUnit.runtime_state).toBe('running')
    expect(stoppedUnit.runtime_state).toBe('stopped')
    expect(runningUnit.capabilities.stop).toBe(true)
    expect(runningUnit.capabilities.start).toBe(false)
    // 主按钮只给“打开”和“启动”：一个正在跑的东西不该把“停止”摆在最显眼的位置。
    expect(availableActions(runningUnit.capabilities)).toContain('stop')
    expect(primaryAction(runningUnit.capabilities)).toBeNull()
    // 读日志两拍都给——它跟着绑定走，不跟着运行状态走。
    expect(stoppedUnit.capabilities.logs).toBe(true)
    expect(runningUnit.capabilities.logs).toBe(true)
  })

  it('每个 true 的能力都能翻成一条真的请求或一个真的链接', () => {
    for (const app of apps.items) {
      for (const action of availableActions(app.capabilities)) {
        if (action === 'open') {
          expect(openTarget(app).href).not.toBe('')
          continue
        }
        if (action === 'logs') continue
        // 生命周期动作：必须能落成一条 service.control / container.control。
        const request = controlRequest(app, action)
        expect({ id: app.id, action, ok: request !== null }).toEqual({
          id: app.id,
          action,
          ok: true,
        })
      }
    }
  })

  it('启停请求打的是已有的动作名，没有 app.control 这条捷径', () => {
    expect(controlRequest(stoppedUnit, 'start')?.action).toBe('service.control')
    expect(controlRequest(stoppedUnit, 'start')?.params).toEqual({
      unit: stoppedUnit.binding.unit,
      operation: 'start',
    })
    const noTarget = { ...linkApp, type: 'link' as const }
    expect(controlRequest(noTarget, 'start')).toBeNull()
  })
})

describe('真机载荷：入口是服务器地址，打开是锚点不是请求', () => {
  it('入口地址是服务器的非回环地址（回环入口在生产模式被拒）', () => {
    const url = need(linkApp.public_url, '入口地址')
    const host = new URL(url).hostname
    expect(host).not.toBe('127.0.0.1')
    expect(host).not.toBe('localhost')
    expect(host).not.toBe('0.0.0.0')
    expect(url).toMatch(/^https?:\/\//)
  })

  it('没登记入口地址的应用不假装能打开', () => {
    const target = openTarget(stoppedUnit)
    expect(target.href).toBe('')
    expect(target.reason).not.toBe('')
  })

  it('卡片上的“打开”是一个真的链接：href 就是服务器地址，新标签、noopener', async () => {
    const html = await render(AppCard, { app: linkApp, canAct: true })
    expect(html).toContain('data-action="open"')
    expect(html).toContain(`href="${linkApp.public_url ?? ''}"`)
    expect(html).toContain('target="_blank"')
    expect(html).toContain('rel="noopener noreferrer"')
  })

  it('入口卡片上没有启停按钮，也没有日志按钮', async () => {
    const html = await render(AppCard, { app: linkApp, canAct: true })
    expect(html).not.toContain('data-action="start"')
    expect(html).not.toContain('data-action="stop"')
    expect(html).not.toContain('data-action="restart"')
    expect(html).not.toContain('data-action="logs"')
  })
})

describe('真机载荷：卡片画出来的是四件事，不是一句“状态良好”', () => {
  it('500 那一张同时写着“运行中”与“入口异常”，并且给得出停止', async () => {
    const html = await render(AppCard, { app: fiveHundred, canAct: true })
    expect(html).toContain('运行中')
    expect(html).toContain('入口异常')
    expect(html).toContain(`data-http="${fiveHundred.http_state}"`)
    expect(html).toContain('data-action="stop"')
    expect(html).not.toContain('data-action="start"')
    // 反面：不许出现把两件事合成一句的写法。
    expect(html).not.toContain('状态良好')
    expect(html).not.toContain('一切正常')
  })

  it('重建那一张：运行时说明上屏，生命周期与日志按钮一个都不给', async () => {
    const html = await render(AppCard, { app: rebuilt, canAct: true })
    expect(html).toContain(rebuilt.runtime_note ?? '')
    for (const action of ['start', 'stop', 'restart', 'logs']) {
      expect(html).not.toContain(`data-action="${action}"`)
    }
    expect(html).toContain('这个应用此刻没有可执行的动作')
  })

  it('受保护单元：只有日志按钮，并说明它被保护（能力来自对象）', async () => {
    const html = await render(AppCard, { app: protectedUnit, canAct: true })
    expect(html).toContain('data-action="logs"')
    for (const action of ['start', 'stop', 'restart']) {
      expect(html).not.toContain(`data-action="${action}"`)
    }
  })

  it('依赖保护那一张：标签与理由都上屏，说的是“面板是唯一的自动启动者”', async () => {
    const html = await render(AppCard, { app: managed, canAct: true })
    expect(managed.dependency_protection).toBe('managed')
    expect(html).toContain('data-testid="app-protection-note"')
    expect(html).toContain(managed.dependency_protection_note ?? '')
  })

  it('没登记的入口地址：卡片明说没有，而不是给一个空链接', async () => {
    const html = await render(AppCard, { app: stoppedUnit, canAct: true })
    expect(html).toContain('没登记入口地址')
    expect(html).not.toContain('data-action="open"')
  })

  it('viewer 看到的是同一张卡，但编辑与删除不出现', async () => {
    const html = await render(AppCard, { app: healthy, canAct: false })
    expect(html).not.toContain('data-action="edit"')
    expect(html).not.toContain('data-action="remove"')
    expect(html).toContain('data-action="stop"')
  })

  it('图标键来自真机数据，翻得出一个字形', () => {
    for (const app of apps.items) {
      expect(iconGlyph(app.icon_key)).not.toBe('')
    }
  })
})

describe('真机载荷：分组、搜索与常用入口', () => {
  it('分组按 sort_order 稳定升序，页面照这个顺序排', () => {
    expect(groups.items.length).toBeGreaterThanOrEqual(2)
    const orders = groups.items.map((g) => g.sort_order)
    expect(orders).toEqual([...orders].sort((a, b) => a - b))
    for (const g of groups.items) expect(g.name.length).toBeGreaterThan(0)
  })

  it('分桶：每一条应用恰好进一次自己的格，收藏格是额外的快捷格', () => {
    const buckets = bucketApps(apps.items, groups.items)
    const favKeys = new Set([FAVORITES_KEY])
    const withoutFavorites = buckets.filter((b) => !favKeys.has(b.key))
    const ids = withoutFavorites.flatMap((b) => b.apps.map((a) => a.id))
    // 收藏格是重复放一次（同一条应用既在“常用入口”里，也在它自己的分组里），
    // 所以“恰好放一次”只能对收藏格之外的那些格去说。
    expect(ids.length).toBe(apps.items.length)
    expect(new Set(ids).size).toBe(apps.items.length)
    for (const key of favKeys) {
      const bucket = buckets.find((b) => b.key === key)
      if (!bucket) continue
      for (const app of bucket.apps) expect(app.favorite).toBe(true)
    }
    if (apps.items.some((a) => a.favorite)) {
      expect(buckets[0]?.key).toBe(FAVORITES_KEY)
    }
    expect(buckets[buckets.length - 1]?.key).toBe(UNGROUPED_KEY)
  })

  it('搜索时不摆收藏格（否则结果看起来像被吞了一半）', () => {
    const buckets = bucketApps(apps.items, groups.items, apps.items[0]?.name.slice(0, 2) ?? '')
    expect(buckets.some((b) => b.key === FAVORITES_KEY)).toBe(false)
  })

  it('分组格的顺序就是 sort_order 的顺序（真机改过名与排序的那两个）', () => {
    const buckets = bucketApps(apps.items, groups.items)
    const groupKeys = buckets
      .map((b) => b.key)
      .filter((key) => key !== FAVORITES_KEY && key !== UNGROUPED_KEY)
    expect(groupKeys).toEqual(groups.items.map((g) => g.id))
  })

  it('收藏格里的都是 favorite=true，顺序是收藏优先再按名字', () => {
    const buckets = bucketApps(apps.items, groups.items)
    const fav = buckets.find((b) => b.key === FAVORITES_KEY)
    if (!fav) return
    for (const app of fav.apps) expect(app.favorite).toBe(true)
    expect(fav.apps.map((a) => a.id)).toEqual(sortApps(fav.apps).map((a) => a.id))
  })

  it('搜索匹配名字与绑定对象，不匹配 id，也不会把 undefined 当成命中', () => {
    expect(matchesAppQuery(linkApp, linkApp.name.slice(0, 2))).toBe(true)
    expect(matchesAppQuery(stoppedUnit, need(stoppedUnit.binding.unit, '单元名'))).toBe(true)
    expect(matchesAppQuery(linkApp, 'zzz-not-here')).toBe(false)
    // 这条是回归锁：曾经把 [name, desc, unit, container_id].join('\n') 里的 undefined
    // 也当成了可匹配文本，搜 "undefined" 会命中所有 link。
    expect(matchesAppQuery(linkApp, 'undefined')).toBe(false)
    expect(matchesAppQuery(linkApp, 'null')).toBe(false)
    // link 的容器 ID 是 undefined，搜它的 id 不该命中。
    expect(matchesAppQuery(linkApp, linkApp.id)).toBe(false)
  })

  it('搜索时“常用入口”格与空分组一起消失', () => {
    const buckets = bucketApps(apps.items, groups.items, 'zzz-not-here')
    expect(buckets).toEqual([])
    const hit = bucketApps(apps.items, groups.items, linkApp.name.slice(0, 2))
    expect(hit.some((b) => b.key === FAVORITES_KEY)).toBe(false)
    for (const b of hit) expect(b.apps.length).toBeGreaterThan(0)
  })

  it('常用入口只收能打开、且入口地址是 http(s) 的那些', () => {
    const entries = frequentEntries(apps.items, 8)
    expect(entries.length).toBeGreaterThan(0)
    expect(entries.length).toBeLessThanOrEqual(8)
    for (const app of entries) {
      expect(app.capabilities.open).toBe(true)
      expect(openTarget(app).href).toMatch(/^https?:\/\//)
    }
    expect(entries.some((a) => a.id === linkApp.id)).toBe(true)
    // 打不开的东西一个都不许进首页。
    expect(entries.some((a) => a.id === rebuilt.id)).toBe(false)
  })
})

describe('真机载荷：本机候选与失效绑定', () => {
  it('三个来源各自报告状态，候选的 target_id 形状与契约一致', () => {
    expect(['ok', 'partial', 'error']).toContain(discovery.status)
    expect(discovery.sources.length).toBeGreaterThan(0)
    for (const source of discovery.sources) {
      expect(['systemd', 'local_listen', 'docker']).toContain(source.source)
      expect(['ok', 'error']).toContain(source.status)
      if (source.status === 'error') expect(source.error).toBeTruthy()
    }
    for (const candidate of discovery.candidates) {
      expect(candidate.target_id).toMatch(/^(local|cntr)_[a-z0-9][a-z0-9_-]{0,47}$/)
    }
  })

  it('已登记的目标带着它属于哪个应用（一个目标只属于一个应用）', () => {
    const bound = discovery.candidates.filter((c) => c.bound_app_id !== null)
    expect(bound.length).toBeGreaterThan(0)
    const appIds = new Set(apps.items.map((a) => a.id))
    for (const c of bound) expect(appIds.has(c.bound_app_id as string)).toBe(true)
  })

  it('重建之后旧绑定出现在 missing_bindings 里（页面据此点名要求重新确认）', () => {
    expect(discovery.missing_bindings.length).toBeGreaterThan(0)
    const issue = need(
      discovery.missing_bindings.find((b) => b.app_id === rebuilt.id),
      '重建那条失效绑定',
    )
    expect(issue.reason).not.toBe('')
  })

  it('发现面板把 status 与“读不到 ≠ 本机什么都没有”写出来', async () => {
    const html = await render(AppDiscoveryPanel, {
      data: discovery,
      canAct: false,
      busy: false,
    })
    expect(html).toContain('data-testid="discovery-candidates"')
    if (discovery.missing_bindings.length > 0) {
      expect(html).toContain('data-testid="missing-bindings"')
    }
  })
})

describe('真机载荷：应用日志按绑定判别', () => {
  const journal = payload<AppLogsData>('app-logs-journal.json')
  const docker = payload<AppLogsData>('app-logs-docker.json')

  it('systemd 应用给的是 journal：unit 等于绑定值，页里没有 docker 那套字段', () => {
    if (journal.source !== 'journal') throw new Error('夹具的 source 不是 journal')
    expect(journal.unit).toBe(stoppedUnit.binding.unit ?? journal.unit)
    expect((journal.page.entries ?? []).length).toBeGreaterThan(0)
    expect(journal.page).not.toHaveProperty('container_id')
  })

  it('docker 应用给的是 docker 日志：container_id 等于绑定值，页里没有 unit/level', () => {
    if (docker.source !== 'docker') throw new Error('夹具的 source 不是 docker')
    const owner = need(
      apps.items.find((a) => a.binding.container_id === docker.container_id),
      '配对到日志的 docker 应用',
    )
    expect(owner.type).toBe('docker')
    expect((docker.page.entries ?? []).length).toBeGreaterThan(0)
    expect(docker.page).not.toHaveProperty('unit')
    expect(docker.page).not.toHaveProperty('level')
  })
})

describe('真机载荷：启停真的走预检与任务', () => {
  const stopJob = payload<JobDTO>('job-appstop.json')
  const observed = (job: JobDTO): Record<string, unknown> =>
    (job.result['observed_state'] ?? {}) as Record<string, unknown>

  it('stop 预检允许、任务成功、执行后的现场读数来自代理', () => {
    expect(stopJob.action).toBe('service.control')
    expect(stopJob.status).toBe('succeeded')
    const after = observed(stopJob)
    expect(after['operation']).toBe('stop')
    expect(after['active_state']).toBe('inactive')
    expect(after['previous_state']).toBe('active')
    expect(stopJob.result['verified_at'] ?? null).not.toBeNull()
  })

  it('任务结果翻成人话时说的是现场读数，不是“已成功”', () => {
    const summary = summarizeAppJob(stopJob)
    expect(summary.ok).toBe(true)
    const text = summary.lines.join('\n')
    expect(text).toContain('active → inactive')
    expect(text).toContain('执行的动作：stop')
  })

  it('start 也走同一条路（停机之后是启动，读数反过来）', () => {
    const startJob = payload<JobDTO>('job-appstart.json')
    expect(startJob.status).toBe('succeeded')
    const after = observed(startJob)
    expect(after['operation']).toBe('start')
    expect(after['active_state']).toBe('active')
    expect(after['previous_state']).toBe('inactive')
  })

  it('依赖卷离线时 start 预检被钉成不允许，且阻塞项点名卷', () => {
    const pre = payload<{
      allowed: boolean
      blockers: { code: string; message: string }[]
    }>('preflight-start-gated.json')
    expect(pre.allowed).toBe(false)
    expect(pre.blockers.map((b) => b.code)).toContain('VOLUME_OFFLINE')
  })

  it('卷挂着时同一份预检放行（拦住它的是卷的状态）', () => {
    const pre = payload<{ allowed: boolean; blockers: { code: string }[] }>(
      'preflight-start-mounted.json',
    )
    expect(pre.allowed).toBe(true)
    expect(pre.blockers.some((b) => b.code.startsWith('VOLUME_'))).toBe(false)
  })

  it('stop 预检在同一个未挂载的卷上不被拦（门禁只拦启动）', () => {
    const pre = payload<{ allowed: boolean; blockers: { code: string }[] }>(
      'preflight-stop-running.json',
    )
    expect(pre.allowed).toBe(true)
    expect(pre.blockers.some((b) => b.code.startsWith('VOLUME_'))).toBe(false)
  })

  it('受保护单元的 stop 预检也是 allowed=false（判决在 allowed 里，不是 HTTP 错误）', () => {
    const pre = payload<{ allowed: boolean; blockers: { code: string }[] }>(
      'preflight-sshstop.json',
    )
    expect(pre.allowed).toBe(false)
    expect(pre.blockers.length).toBeGreaterThan(0)
  })
})
