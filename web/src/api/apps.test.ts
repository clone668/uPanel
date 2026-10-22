// 应用 API 层：状态区分、搜索、分组、一键打开（P09.5）。
//
// 这一层不碰网络，只测“给一份服务端载荷，页面会摆成什么样、会说什么”。三条底线在
// 这里是可断言的，它们直接对应 STAGES §11 的验收行：
//   1. **两个状态不相干**：容器 running、HTTP 500 必须同时显示成“运行中”和“异常”，
//      不能被合成一句“正常”——一边是运行时的观测，一边是对外那一面的探测；
//   2. **没登记的入口不打开**：`public_url` 为空、或不是 http/https 时，一键打开
//      不给出地址（`javascript:` 之类的协议不该由页面来兜底）；
//   3. **能力来自服务端**：link 类型没有启停，`capabilities` 全 false 时卡片上不出现
//      生命周期按钮——先渲染再让点击失败，比不渲染更糟。
//
// 搜索与分组是纯函数，因此可以断言“哪些应用会出现在哪一格”，而不必渲染 DOM。

import { describe, expect, it } from 'vitest'

import {
  APP_ACTION_LABELS,
  APP_ICON_GLYPHS,
  APP_TYPE_LABELS,
  DEPENDENCY_PROTECTION_LABELS,
  HTTP_STATE_LABELS,
  RUNTIME_STATE_LABELS,
  UNGROUPED_KEY,
  FAVORITES_KEY,
  availableActions,
  bucketApps,
  describeBinding,
  frequentEntries,
  iconGlyph,
  matchesAppQuery,
  openTarget,
  primaryAction,
  sortApps,
  type App,
  type AppGroup,
  type AppCapabilities,
} from './apps'

function capabilities(overrides: Partial<AppCapabilities> = {}): AppCapabilities {
  return { open: false, start: false, stop: false, restart: false, logs: false, ...overrides }
}

function app(overrides: Partial<App> = {}): App {
  const base: App = {
    id: 'app_0123456789abcdef0123456789abcdef',
    name: '示例应用',
    description: '',
    icon_key: 'app',
    group_id: null,
    type: 'link',
    binding: {},
    public_url: null,
    health_check: {
      enabled: false,
      target_id: '',
      scheme: 'http',
      port: 0,
      path: '/',
      expected_status_codes: [],
      interval_seconds: 60,
      timeout_seconds: 5,
    },
    volume_dependencies: [],
    favorite: false,
    version: 1,
    created_at: '2026-10-22T08:00:00Z',
    updated_at: '2026-10-22T08:00:00Z',
    runtime_state: 'not_applicable',
    runtime_note: null,
    http_state: 'not_applicable',
    last_checked_at: null,
    dependency_protection: 'not_applicable',
    dependency_protection_note: null,
    capabilities: capabilities(),
  }
  return { ...base, ...overrides }
}

function group(overrides: Partial<AppGroup> = {}): AppGroup {
  const base: AppGroup = {
    id: 'grp_0123456789abcdef0123456789abcdef',
    name: '默认分组',
    sort_order: 0,
    version: 1,
    created_at: '2026-10-22T08:00:00Z',
    updated_at: '2026-10-22T08:00:00Z',
  }
  return { ...base, ...overrides }
}

describe('状态标签是封闭集合，缺一个键就当编译错误', () => {
  it('三个状态枚举各有中文说法', () => {
    expect(RUNTIME_STATE_LABELS.running).toBe('运行中')
    expect(RUNTIME_STATE_LABELS.stopped).toBe('已停止')
    expect(RUNTIME_STATE_LABELS.failed).toBe('启动失败')
    expect(RUNTIME_STATE_LABELS.unknown).toBe('状态未知')
    expect(RUNTIME_STATE_LABELS.not_applicable).toBe('无运行时')

    expect(HTTP_STATE_LABELS.ok).toBe('正常')
    expect(HTTP_STATE_LABELS.error).toBe('异常')
    expect(HTTP_STATE_LABELS.unknown).toBe('还没结论')
    expect(HTTP_STATE_LABELS.not_applicable).toBe('未启用')

    expect(DEPENDENCY_PROTECTION_LABELS.managed).toBe('依赖由面板接管')
    expect(DEPENDENCY_PROTECTION_LABELS.unmanaged).toBe('依赖未接管')
    expect(DEPENDENCY_PROTECTION_LABELS.not_applicable).toBe('无依赖卷')

    expect(APP_TYPE_LABELS.systemd).toBe('systemd 服务')
    expect(APP_TYPE_LABELS.docker).toBe('Docker 容器')
  })

  it('图标键画成字符，认不出的退回 ?', () => {
    expect(iconGlyph('globe')).toBe(APP_ICON_GLYPHS.globe)
    expect(iconGlyph('mystery')).toBe('?')
    expect(iconGlyph('')).toBe('?')
  })
})

describe('两个状态不相干：容器在跑但 HTTP 500 → “运行中 + 异常”', () => {
  it('runtime_state 与 http_state 同时保留，谁也不推导谁', () => {
    const running = app({ type: 'docker', runtime_state: 'running', http_state: 'error' })
    expect(RUNTIME_STATE_LABELS[running.runtime_state]).toBe('运行中')
    expect(HTTP_STATE_LABELS[running.http_state]).toBe('异常')
    // 合成一句话就会丢掉其中一个事实，这里断言它们不冲突：
    expect(RUNTIME_STATE_LABELS[running.runtime_state]).not.toBe(HTTP_STATE_LABELS[running.http_state])
  })

  it('docker 侧不会出现 failed：崩退的容器是 stopped', () => {
    const crashed = app({ type: 'docker', runtime_state: 'stopped', http_state: 'unknown' })
    expect(RUNTIME_STATE_LABELS[crashed.runtime_state]).toBe('已停止')
    // runtime_note 是唯一能说清“为什么”的地方，null 表示没有话要说。
    expect(crashed.runtime_note).toBeNull()
  })

  it('去重保护状态是观测结果，不是类型属性', () => {
    expect(
      app({ type: 'docker', dependency_protection: 'managed', dependency_protection_note: null })
        .dependency_protection,
    ).toBe('managed')
    expect(
      app({ type: 'systemd', dependency_protection: 'unmanaged', dependency_protection_note: '开机时 systemd 会直接拉起它' })
        .dependency_protection,
    ).toBe('unmanaged')
  })
})

describe('绑定的人类说法', () => {
  it('每种类型只说一个字段', () => {
    expect(describeBinding('systemd', { unit: 'nginx.service' })).toBe('单元 nginx.service')
    expect(describeBinding('docker', { container_id: 'a'.repeat(64) })).toBe(`容器 ${'a'.repeat(12)}`)
    expect(describeBinding('compose', { deployment_id: 'dep_x' })).toBe('部署 dep_x')
    expect(describeBinding('link', {})).toBe('外部链接')
  })

  it('该有绑定却没有时如实说“未绑定”，不显示 undefined', () => {
    expect(describeBinding('systemd', {})).toBe('未绑定单元')
    expect(describeBinding('docker', {})).toBe('未绑定容器')
    expect(describeBinding('compose', {})).toBe('未绑定部署')
  })
})

describe('按钮来自 capabilities，不来自 type', () => {
  it('link 没有生命周期按钮', () => {
    const link = app({ type: 'link', public_url: 'https://example.com', capabilities: capabilities({ open: true }) })
    expect(availableActions(link.capabilities)).toEqual(['open'])
    expect(primaryAction(link.capabilities)).toBe('open')
  })

  it('全 false 时什么按钮都没有', () => {
    expect(availableActions(capabilities())).toEqual([])
    expect(primaryAction(capabilities())).toBeNull()
  })

  it('运行中的容器：打开 + 停止 + 重启 + 日志，主按钮是打开', () => {
    const caps = capabilities({ open: true, stop: true, restart: true, logs: true })
    expect(availableActions(caps)).toEqual(['open', 'stop', 'restart', 'logs'])
    expect(primaryAction(caps)).toBe('open')
  })

  it('停着的东西没有打开按钮，主按钮退回启动', () => {
    const caps = capabilities({ start: true, logs: true })
    expect(availableActions(caps)).toEqual(['start', 'logs'])
    expect(primaryAction(caps)).toBe('start')
  })

  it('受保护单元只能读日志：日志按钮在，生命周期按钮不在', () => {
    const caps = capabilities({ logs: true })
    expect(availableActions(caps)).toEqual(['logs'])
    expect(primaryAction(caps)).toBeNull()
    // 动作名与按钮文案是同一份表，避免“按钮写停止、请求发 stop ”这类分叉。
    expect(APP_ACTION_LABELS.logs).toBe('日志')
  })

  it('capabilities 缺失时不崩，按“什么都不能”处理', () => {
    expect(availableActions(null)).toEqual([])
    expect(availableActions(undefined)).toEqual([])
    expect(primaryAction(null)).toBeNull()
  })
})

describe('一键打开', () => {
  it('http/https 的入口给出地址，并标成新标签打开', () => {
    const target = openTarget(app({ public_url: 'http://192.168.1.10:8080/ui' }))
    expect(target).toEqual({ href: 'http://192.168.1.10:8080/ui', external: true, reason: '' })
  })

  it('没有入口地址时不给地址，并说明原因', () => {
    const target = openTarget(app({ public_url: null }))
    expect(target.href).toBe('')
    expect(target.reason).toContain('没有登记入口地址')
  })

  it('只有空白的入口也算没有入口', () => {
    expect(openTarget(app({ public_url: '   ' })).href).toBe('')
  })

  it('不是 http/https 的协议一律不打开', () => {
    for (const url of ['javascript:alert(1)', 'data:text/html,<script>', 'file:///etc/passwd']) {
      const target = openTarget(app({ public_url: url }))
      expect(target.href).toBe('')
      expect(target.reason).toContain('http/https')
    }
  })
})

describe('搜索', () => {
  const nginx = app({
    id: 'app_nginx',
    name: 'Nginx 面板',
    description: '反向代理入口',
    type: 'systemd',
    binding: { unit: 'nginx.service' },
  })

  it('空查询匹配一切（包括纯空白）', () => {
    expect(matchesAppQuery(nginx, '')).toBe(true)
    expect(matchesAppQuery(nginx, '   ')).toBe(true)
  })

  it('匹配名称、描述、绑定对象名，大小写不敏感', () => {
    expect(matchesAppQuery(nginx, 'nginx')).toBe(true)
    expect(matchesAppQuery(nginx, 'NGINX')).toBe(true)
    expect(matchesAppQuery(nginx, '反向')).toBe(true)
    expect(matchesAppQuery(nginx, 'nginx.service')).toBe(true)
  })

  it('不匹配 id：那是 32 位十六进制，没人拿它搜', () => {
    expect(matchesAppQuery(nginx, 'app_nginx')).toBe(false)
  })

  it('不匹配没有的字段', () => {
    expect(matchesAppQuery(nginx, 'postgres')).toBe(false)
    // 绑定是空对象时不能匹配成 undefined 字符串。
    expect(matchesAppQuery(app({ binding: {} }), 'undefined')).toBe(false)
  })
})

describe('排序与分组', () => {
  const g1 = group({ id: 'grp_1', name: '基础设施', sort_order: 10 })
  const g2 = group({ id: 'grp_2', name: '媒体', sort_order: 20 })

  const apps = [
    app({ id: 'app_c', name: 'zeta', group_id: 'grp_2' }),
    app({ id: 'app_a', name: 'Alpha', group_id: 'grp_1' }),
    app({ id: 'app_b', name: 'beta', group_id: null }),
    app({ id: 'app_d', name: 'delta', group_id: 'grp_1', favorite: true }),
  ]

  it('收藏优先，其次按名称（大小写不敏感）', () => {
    expect(sortApps(apps).map((a) => a.id)).toEqual(['app_d', 'app_a', 'app_b', 'app_c'])
  })

  it('分组顺序：常用入口 → 按 sort_order 的分组 → 未分组', () => {
    const buckets = bucketApps(apps, [g2, g1])
    expect(buckets.map((b) => b.key)).toEqual([FAVORITES_KEY, 'grp_1', 'grp_2', UNGROUPED_KEY])
    expect(buckets[0]?.apps.map((a) => a.id)).toEqual(['app_d'])
    expect(buckets[1]?.apps.map((a) => a.id)).toEqual(['app_d', 'app_a'])
    expect(buckets[2]?.apps.map((a) => a.id)).toEqual(['app_c'])
    expect(buckets[3]?.apps.map((a) => a.id)).toEqual(['app_b'])
  })

  it('空分组仍然出现（分组存在但空 ≠ 没有这个分组），但只在没搜索时', () => {
    const empty = group({ id: 'grp_3', name: '空分组', sort_order: 30 })
    const buckets = bucketApps(apps, [g1, empty])
    expect(buckets.map((b) => b.key)).toContain('grp_3')
    expect(buckets.find((b) => b.key === 'grp_3')?.apps).toEqual([])

    const searched = bucketApps(apps, [g1, empty], 'alpha')
    expect(searched.map((b) => b.key)).not.toContain('grp_3')
  })

  it('搜索时不拆“常用入口”那一格：结果已经很少，再拆一层会像被吞了', () => {
    const searched = bucketApps(apps, [g1, g2], 'delta')
    expect(searched.map((b) => b.key)).not.toContain(FAVORITES_KEY)
    expect(searched.flatMap((b) => b.apps.map((a) => a.id))).toEqual(['app_d'])
  })

  it('指向不存在分组的应用归到未分组，不会消失', () => {
    const orphan = app({ id: 'app_e', name: '孤儿', group_id: 'grp_gone' })
    const buckets = bucketApps([orphan], [g1])
    const ungrouped = buckets.find((b) => b.key === UNGROUPED_KEY)
    expect(ungrouped?.apps.map((a) => a.id)).toEqual(['app_e'])
  })

  it('一条应用都没有时给一个空的未分组格，页面据此显示空状态', () => {
    const buckets = bucketApps([], [])
    expect(buckets).toEqual([{ key: UNGROUPED_KEY, label: '未分组', apps: [] }])
  })

  it('搜索结果为空时不留空壳分组', () => {
    const buckets = bucketApps(apps, [g1, g2], 'no-such-thing')
    expect(buckets).toEqual([])
  })
})

describe('首页常用入口', () => {
  const openable = (id: string, name: string, favorite = false) =>
    app({
      id,
      name,
      favorite,
      public_url: `https://${name}.example.com`,
      capabilities: capabilities({ open: true }),
    })

  it('只取能打开的：没有入口的不进首页', () => {
    const entries = frequentEntries([
      openable('app_1', 'a'),
      app({ id: 'app_2', name: 'b', capabilities: capabilities({ open: false }) }),
      app({ id: 'app_3', name: 'c', public_url: 'https://c.example.com' }),
    ])
    expect(entries.map((e) => e.id)).toEqual(['app_1'])
  })

  it('收藏优先', () => {
    const entries = frequentEntries([
      openable('app_1', 'a'),
      openable('app_2', 'b', true),
    ])
    expect(entries.map((e) => e.id)).toEqual(['app_2', 'app_1'])
  })

  it('默认最多 8 条，可以改', () => {
    const many = Array.from({ length: 12 }, (_, i) => openable(`app_${i}`, `app${i}`))
    expect(frequentEntries(many)).toHaveLength(8)
    expect(frequentEntries(many, 3)).toHaveLength(3)
  })

  it('capabilities.open 为真但没有地址时也不进首页（点不动的入口不是入口）', () => {
    const entries = frequentEntries([
      app({ id: 'app_x', name: 'x', public_url: null, capabilities: capabilities({ open: true }) }),
    ])
    expect(entries).toEqual([])
  })
})
