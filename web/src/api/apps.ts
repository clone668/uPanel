// 应用的 DTO、状态翻译与页面用的纯函数（P09.1—P09.5）。
//
// 与 api/openapi.yaml 的 App / AppBinding / AppHealthCheck / AppCapabilities /
// AppDiscoveryData / AppJournalLogsData / AppContainerLogsData 逐字对应，与
// docs/CONTRACTS.md 第 8 节一起看。
//
// 这个模块只做四件事：取数据、把状态翻译成人话、把“此刻能不能点”算出来、按搜索词/分组
// 把列表摆好。**判断“能不能做”不在页面里**：能力来自服务端（它又来自底层对象的观测），
// 页面只按 `capabilities` 决定渲不渲染那个按钮——先渲染再让点击失败，比不渲染更糟。
//
// 三个状态必须分开显示，这是 CONTRACTS §8.4 的立场：
//   runtime_state   那个东西此刻在不在跑（来自 systemd/docker 的观测）
//   http_state      它对外的那一面健不健康（来自面板自己的探测）
//   两者互不推导：容器 running 但 HTTP 500 是一个正常要表达的结论，不是矛盾。
//
// `dependency_protection` 的 `managed` 含义被服务端刻意限死（面板是受管理启动路径上唯一的
// 自动启动者）。页面不替它加戏：unmanaged 不是“不能启动”，而是“有条路绕开了面板的检查”。

import { get, request, type ApiResult } from './client'
import type { LogPage } from './logs'
import type { ListData } from './types'

// ---------------------------------------------------------------- 类型与绑定

export const APP_TYPES = ['link', 'systemd', 'docker', 'compose'] as const
export type AppType = (typeof APP_TYPES)[number]

export const APP_TYPE_LABELS: Record<AppType, string> = {
  link: '链接',
  systemd: 'systemd 服务',
  docker: 'Docker 容器',
  compose: 'Compose 部署',
}

/** 绑定对象。三个字段最多只有一个出现，由 type 决定是哪一个。 */
export interface AppBinding {
  unit?: string
  container_id?: string
  deployment_id?: string
}

/**
 * 绑定 → “这个应用指向的是什么”。
 *
 * 页面不自己 switch type 去拼说明文字：同一句话在卡片、详情、日志抽屉里各写一遍，
 * 迟早会分叉成三种说法。
 */
export function describeBinding(type: AppType, binding: AppBinding): string {
  switch (type) {
    case 'systemd':
      return binding.unit ? `单元 ${binding.unit}` : '未绑定单元'
    case 'docker':
      // 只显示前 12 位：完整 ID 是 64 位，在卡片上放不下；需要完整值时看详情。
      return binding.container_id ? `容器 ${binding.container_id.slice(0, 12)}` : '未绑定容器'
    case 'compose':
      return binding.deployment_id ? `部署 ${binding.deployment_id}` : '未绑定部署'
    case 'link':
      return '外部链接'
  }
}

// ---------------------------------------------------------------- 健康检查定义

export const HEALTH_SCHEMES = ['http', 'https'] as const
export type HealthScheme = (typeof HEALTH_SCHEMES)[number]

export interface AppHealthCheck {
  enabled: boolean
  /** 代理发现的探测目标；形状固定为 local_… / cntr_…，写不出 IP 或 URL。 */
  target_id: string
  scheme: HealthScheme
  port: number
  path: string
  /** 空数组表示默认 200—399；不会是 null。 */
  expected_status_codes: number[]
  interval_seconds: number
  timeout_seconds: number
}

export const DEFAULT_HEALTH_CHECK: AppHealthCheck = {
  enabled: false,
  target_id: '',
  scheme: 'http',
  port: 0,
  path: '/',
  expected_status_codes: [],
  interval_seconds: 60,
  timeout_seconds: 5,
}

// ---------------------------------------------------------------- 健康结论

export const HTTP_STATES = ['ok', 'error', 'unknown', 'not_applicable'] as const
export type HttpState = (typeof HTTP_STATES)[number]

export const HTTP_STATE_LABELS: Record<HttpState, string> = {
  ok: '正常',
  error: '异常',
  unknown: '还没结论',
  not_applicable: '未启用',
}

export interface AppHealthState {
  app_id: string
  enabled: boolean
  http_state: HttpState
  checked_at: string | null
  /** 没拿到状态码时是 null，不是 0。 */
  status_code: number | null
  duration_ms: number | null
  target_id: string
  scheme: HealthScheme
  port: number
  path: string
  address: string | null
  url: string | null
  last_error: string
  expected_status_codes: number[]
  note: string
}

// ---------------------------------------------------------------- 运行时与保护

export const RUNTIME_STATES = ['running', 'stopped', 'failed', 'unknown', 'not_applicable'] as const
export type RuntimeState = (typeof RUNTIME_STATES)[number]

export const RUNTIME_STATE_LABELS: Record<RuntimeState, string> = {
  running: '运行中',
  stopped: '已停止',
  failed: '启动失败',
  unknown: '状态未知',
  not_applicable: '无运行时',
}

/** 状态徽标的语义类型（naive-ui 的 tag type）。颜色只是辅助，文字必须能自己说清。 */
export type BadgeTone = 'success' | 'warning' | 'error' | 'info' | 'default'

export const RUNTIME_STATE_TONES: Record<RuntimeState, BadgeTone> = {
  running: 'success',
  stopped: 'default',
  failed: 'error',
  unknown: 'warning',
  not_applicable: 'info',
}

export const DEPENDENCY_PROTECTIONS = ['managed', 'unmanaged', 'not_applicable'] as const
export type DependencyProtection = (typeof DEPENDENCY_PROTECTIONS)[number]

export const DEPENDENCY_PROTECTION_LABELS: Record<DependencyProtection, string> = {
  managed: '依赖由面板接管',
  unmanaged: '依赖未接管',
  not_applicable: '无依赖卷',
}

export const DEPENDENCY_PROTECTION_TONES: Record<DependencyProtection, BadgeTone> = {
  managed: 'success',
  unmanaged: 'warning',
  not_applicable: 'default',
}

export interface AppCapabilities {
  /** 有 public_url 时为真。 */
  open: boolean
  start: boolean
  stop: boolean
  restart: boolean
  /** 读日志不是写操作：受保护单元与高风险容器都可能是 true。 */
  logs: boolean
}

/** 一个应用此刻能点的按钮。页面按它渲染，不按 type 猜。 */
export type AppAction = 'open' | 'start' | 'stop' | 'restart' | 'logs'

export const APP_ACTION_LABELS: Record<AppAction, string> = {
  open: '打开',
  start: '启动',
  stop: '停止',
  restart: '重启',
  logs: '日志',
}

export function availableActions(capabilities: AppCapabilities | null | undefined): AppAction[] {
  if (!capabilities) return []
  return (Object.keys(APP_ACTION_LABELS) as AppAction[]).filter((action) => capabilities[action])
}

/**
 * 卡片主按钮：能打开就打开，否则能启动就启动，什么都不能就不给主按钮。
 *
 * 停止/重启是次按钮——它们是“已经能打开的东西”上的动作，不是入口。
 */
export function primaryAction(capabilities: AppCapabilities | null | undefined): AppAction | null {
  const actions = availableActions(capabilities)
  if (actions.includes('open')) return 'open'
  if (actions.includes('start')) return 'start'
  return null
}

// ---------------------------------------------------------------- 应用

export interface App {
  id: string
  name: string
  description: string
  icon_key: string
  group_id: string | null
  type: AppType
  binding: AppBinding
  public_url: string | null
  health_check: AppHealthCheck
  volume_dependencies: string[]
  /** 当前登录用户自己的收藏状态，不是应用的公共属性。 */
  favorite: boolean
  version: number
  created_at: string
  updated_at: string
  runtime_state: RuntimeState
  runtime_note: string | null
  http_state: HttpState
  last_checked_at: string | null
  dependency_protection: DependencyProtection
  dependency_protection_note: string | null
  capabilities: AppCapabilities
}

export interface AppGroup {
  id: string
  name: string
  sort_order: number
  version: number
  created_at: string
  updated_at: string
}

/** 新建请求（CONTRACTS §8 AppDefinition）。 */
export interface AppDefinition {
  name: string
  type: AppType
  description?: string
  icon_key?: string
  group_id?: string | null
  binding?: AppBinding
  public_url?: string
  health_check?: AppHealthCheck
  volume_dependencies?: string[]
}

/** 局部更新：出现的字段才改。`version` 是乐观锁的**当前**版本。 */
export interface AppUpdate {
  version: number
  name?: string
  description?: string
  icon_key?: string
  group_id?: string | null
  type?: AppType
  binding?: AppBinding
  public_url?: string
  health_check?: AppHealthCheck
  volume_dependencies?: string[]
}

// ---------------------------------------------------------------- 表单草稿

/**
 * 表单编辑中的一份草稿。
 *
 * 与 DTO 的区别只有一处：所有可选字段在这里都有确定的值（空串 / 空数组 / null），
 * 因为输入框的“空”不是一个可以省略的东西。转换成请求体的两个函数在下面，页面不自己拼。
 */
export interface AppDraft {
  name: string
  type: AppType
  description: string
  icon_key: string
  group_id: string | null
  binding: AppBinding
  public_url: string
  health_check: AppHealthCheck
  volume_dependencies: string[]
}

/** 空草稿（新建）。默认 type 是 link：它是唯一不需要绑定对象的类型。 */
export function emptyDraft(): AppDraft {
  return {
    name: '',
    type: 'link',
    description: '',
    icon_key: 'app',
    group_id: null,
    binding: {},
    public_url: '',
    health_check: { ...DEFAULT_HEALTH_CHECK },
    volume_dependencies: [],
  }
}

/** 从一条应用生成草稿（编辑）。不改任何值，只补齐空值。 */
export function draftFromApp(app: App): AppDraft {
  return {
    name: app.name,
    type: app.type,
    description: app.description,
    icon_key: app.icon_key,
    group_id: app.group_id,
    binding: { ...app.binding },
    public_url: app.public_url ?? '',
    health_check: {
      ...app.health_check,
      expected_status_codes: [...app.health_check.expected_status_codes],
    },
    volume_dependencies: [...app.volume_dependencies],
  }
}

/** 深拷贝一份草稿：表单会就地改它，不能把调用方手里的那份也改掉。 */
export function cloneDraft(draft: AppDraft): AppDraft {
  return {
    ...draft,
    binding: { ...draft.binding },
    health_check: {
      ...draft.health_check,
      expected_status_codes: [...draft.health_check.expected_status_codes],
    },
    volume_dependencies: [...draft.volume_dependencies],
  }
}

/**
 * 从一条本机候选生成草稿（确认候选 = 新建应用，CONTRACTS §8.2）。
 *
 * 三样东西直接带过来：绑定的对象、探测目标（target_id 只能由代理分配）、以及建议的
 * 协议与端口。名字用候选的 title，人一眼认得出来；类型由 kind 决定。
 *
 * 候选的 `bound_app_id` 非空表示它已经被某个应用登记了——调用方不应再给“确认”按钮，
 * 这里也不替它做判决。
 */
export function draftFromCandidate(candidate: AppDiscoveryCandidate): AppDraft {
  const draft = emptyDraft()
  draft.name = candidate.title === '' ? candidate.name : candidate.title
  switch (candidate.kind) {
    case 'systemd':
      draft.type = 'systemd'
      draft.icon_key = 'tools'
      draft.binding = candidate.unit ? { unit: candidate.unit } : {}
      break
    case 'docker':
      draft.type = 'docker'
      draft.icon_key = 'docker'
      draft.binding = candidate.container_id ? { container_id: candidate.container_id } : {}
      break
    default:
      draft.type = 'link'
      draft.icon_key = 'globe'
      draft.binding = {}
      break
  }
  if (candidate.probe_available) {
    draft.health_check = {
      ...draft.health_check,
      enabled: true,
      target_id: candidate.target_id,
      scheme: candidate.suggested_scheme,
      port: candidate.suggested_port,
      path: '/',
    }
  }
  return draft
}

/**
 * 只保留与 type 相符的那一个绑字段。
 *
 * 表单切换 type 时旧的绑字段还留在草稿里，直接提交会得到“一个对象同时绑定两个类型”，
 * 服务端一律 400。这里挡在提交之前，并且是唯一的转换处。
 */
export function normalizeDraftBinding(draft: AppDraft): AppBinding {
  switch (draft.type) {
    case 'systemd':
      return draft.binding.unit ? { unit: draft.binding.unit.trim() } : {}
    case 'docker':
      return draft.binding.container_id ? { container_id: draft.binding.container_id.trim() } : {}
    case 'compose':
      return draft.binding.deployment_id ? { deployment_id: draft.binding.deployment_id.trim() } : {}
    case 'link':
      return {}
  }
}

const UNIT_PATTERN = /^[A-Za-z0-9][A-Za-z0-9:_.@-]{0,199}\.service$/
const CONTAINER_PATTERN = /^[0-9a-f]{64}$/
const DEPLOYMENT_PATTERN = /^dep_[0-9a-f]{32}$/

/**
 * 提交前的本地校对：只挡“一定会被服务端拒”的形状错误，业务判决留给服务端。
 *
 * 返回第一条人话错误，没有错误返回 null。空列表而不是全部错误：一次显示五条红字
 * 会让人不知道该先改哪一处，而服务端最终仍会逐条说明。
 */
export function validateDraft(draft: AppDraft): string | null {
  const name = draft.name.trim()
  if (name === '') return '应用名称不能为空'
  if (name.length > 80) return '应用名称最多 80 个字符'
  if (draft.description.length > 1000) return '说明最多 1000 个字符'

  const binding = normalizeDraftBinding(draft)
  switch (draft.type) {
    case 'systemd':
      if (!binding.unit) return 'systemd 类型需要填写单元名（例如 nginx.service）'
      if (!UNIT_PATTERN.test(binding.unit)) return '单元名必须以 .service 结尾，且只能含字母、数字与 : _ . @ -'
      break
    case 'docker':
      if (!binding.container_id) return 'Docker 类型需要填写完整容器 ID'
      if (!CONTAINER_PATTERN.test(binding.container_id)) return '容器 ID 必须是 64 位小写十六进制'
      break
    case 'compose':
      if (!binding.deployment_id) return 'Compose 类型需要填写部署标识（dep_…）'
      if (!DEPLOYMENT_PATTERN.test(binding.deployment_id)) return '部署标识的形状是 dep_ 加 32 位小写十六进制'
      break
    case 'link':
      break
  }

  const url = draft.public_url.trim()
  if (url !== '' && !/^https?:\/\/\S+$/i.test(url)) {
    return '入口地址必须是 http:// 或 https:// 开头，且不含空白'
  }
  if (draft.type === 'link' && url === '') return 'link 类型的应用必须填写入口地址，否则卡片点不开'

  if (draft.health_check.enabled) {
    if (draft.health_check.target_id === '') {
      return '启用了健康检查就必须选一个探测目标（只能选本机发现到的目标）'
    }
    if (draft.health_check.port < 1 || draft.health_check.port > 65535) {
      return '探测端口必须在 1—65535 之间'
    }
    const path = draft.health_check.path.trim()
    if (!path.startsWith('/') || path.includes('://')) {
      return '探测路径必须以 / 开头，不能是完整 URL'
    }
    if (draft.health_check.interval_seconds < 30 || draft.health_check.interval_seconds > 300) {
      return '探测间隔必须在 30—300 秒之间'
    }
    if (draft.health_check.timeout_seconds < 1 || draft.health_check.timeout_seconds > 10) {
      return '探测超时必须在 1—10 秒之间'
    }
  }
  return null
}

/** 草稿 → 新建请求体。 */
export function draftToDefinition(draft: AppDraft): AppDefinition {
  return {
    name: draft.name.trim(),
    type: draft.type,
    description: draft.description,
    icon_key: draft.icon_key,
    group_id: draft.group_id,
    binding: normalizeDraftBinding(draft),
    // 空串表示“没有入口”，服务端据此存 null。
    public_url: draft.public_url.trim(),
    health_check: draft.health_check.enabled
      ? { ...draft.health_check, path: draft.health_check.path.trim() }
      : { ...DEFAULT_HEALTH_CHECK },
    volume_dependencies: [...draft.volume_dependencies],
  }
}

/**
 * 草稿 → 局部更新请求体。
 *
 * 这里**每个字段都带上**，包括 `group_id: null`（移出分组）与 `public_url: ""`（清掉入口）：
 * 表单编辑的语义是“保存成现在这个样子”，只有显式给出才能表达“清空”。
 */
export function draftToUpdate(draft: AppDraft, version: number): AppUpdate {
  return { ...draftToDefinition(draft), version }
}

// ---------------------------------------------------------------- 本机候选

export const DISCOVERY_SOURCES = ['systemd', 'local_listen', 'docker'] as const
export type DiscoverySourceName = (typeof DISCOVERY_SOURCES)[number]

export const DISCOVERY_SOURCE_LABELS: Record<DiscoverySourceName, string> = {
  systemd: 'systemd 单元',
  local_listen: '本机监听',
  docker: 'Docker 容器',
}

export interface AppDiscoveryListener {
  address: string
  port: number
  protocol: 'tcp' | 'tcp6'
  loopback: boolean
}

export interface AppDiscoverySource {
  source: DiscoverySourceName
  status: 'ok' | 'error'
  error?: string
  count: number
}

export interface AppDiscoveryCandidate {
  target_id: string
  kind: 'systemd' | 'docker' | 'local'
  source: DiscoverySourceName
  name: string
  title: string
  reason: string
  observed_state: 'running' | 'stopped' | 'failed' | 'unknown'
  listeners: AppDiscoveryListener[]
  suggested_scheme: HealthScheme
  suggested_port: number
  probe_available: boolean
  probe_note: string
  unit?: string
  container_id?: string
  bound_app_id: string | null
  bound_app_name: string | null
}

export interface AppBindingIssue {
  app_id: string
  name: string
  type: AppType
  binding: string
  reason: string
}

export interface AppDiscoveryData {
  /** error 表示没有任何来源成功——不能当作“本机什么都没有”。 */
  status: 'ok' | 'partial' | 'error'
  error?: string
  collected_at?: string
  sources: AppDiscoverySource[]
  candidates: AppDiscoveryCandidate[]
  missing_bindings: AppBindingIssue[]
  truncated: boolean
}

// ---------------------------------------------------------------- 日志

/** 容器日志的一条（与 /containers/{id}/logs 的 ContainerLogEntry 同一形状）。 */
export interface ContainerLogEntry {
  timestamp: string
  /** 超过 8 KiB 的部分被截断（truncated=true）。 */
  message: string
  truncated: boolean
  /** 命中了秘密脱敏规则。 */
  redacted: boolean
}

export interface ContainerLogPage {
  container_id: string
  /** 从新到旧。没有日志时是空数组，不是 null。 */
  entries: ContainerLogEntry[]
  /** 本页最旧一条的时间戳；没有更早的日志时是 null。 */
  next_cursor: string | null
  scanned: number
  truncated: boolean
  /** true 表示 stdout 与 stderr 被合并读出（按时间交错，不保证同一时刻的先后）。 */
  streams_merged: boolean
}

export interface AppJournalLogsData {
  source: 'journal'
  unit: string
  /** 与 /logs 返回的同一页，字段不再包装一层。 */
  page: LogPage
}

export interface AppContainerLogsData {
  source: 'docker'
  container_id: string
  page: ContainerLogPage
}

export type AppLogsData = AppJournalLogsData | AppContainerLogsData

/** 日志查询参数：两个来源的参数集合互不串味，传错集合服务端回 400。 */
export interface AppLogQuery {
  level?: string
  from?: string
  to?: string
  since?: string
  until?: string
  tail?: number
  cursor?: string
  limit?: number
}

// ---------------------------------------------------------------- 图标

/**
 * 内置图标键。页面**不**从服务器取图标资源：一个可写的图片地址就是一条外链与一次
 * 不可控的渲染。这里的值画成字符，画不出来就退回首字。
 */
export const APP_ICON_KEYS = [
  'app',
  'globe',
  'database',
  'docker',
  'terminal',
  'files',
  'media',
  'monitor',
  'network',
  'shield',
  'tools',
] as const

export type AppIconKey = (typeof APP_ICON_KEYS)[number]

export const APP_ICON_LABELS: Record<AppIconKey, string> = {
  app: '应用',
  globe: '网站',
  database: '数据库',
  docker: '容器',
  terminal: '终端',
  files: '文件',
  media: '媒体',
  monitor: '监控',
  network: '网络',
  shield: '安全',
  tools: '工具',
}

/** 图标键 → 字符。未知键退回 `?`，不是空白（空白会被读成“没有图标”）。 */
export const APP_ICON_GLYPHS: Record<AppIconKey, string> = {
  app: '▣',
  globe: '◎',
  database: '▤',
  docker: '⬢',
  terminal: '⌨',
  files: '▤',
  media: '▶',
  monitor: '◫',
  network: '⇄',
  shield: '⛨',
  tools: '⚙',
}

export function iconGlyph(iconKey: string): string {
  return (APP_ICON_GLYPHS as Record<string, string | undefined>)[iconKey] ?? '?'
}

// ---------------------------------------------------------------- 列表、搜索、分组

/** 未分组应用的伪分组标识。它不是 grp_ 形状，永远不会撞上真分组。 */
export const UNGROUPED_KEY = 'ungrouped'
/** 收藏的伪分组标识。 */
export const FAVORITES_KEY = 'favorites'

export interface AppBucket {
  /** `grp_…` / `ungrouped` / `favorites`。 */
  key: string
  label: string
  apps: App[]
}

/**
 * 搜索匹配：名称、描述、绑定的对象名。
 *
 * 大小写不敏感；匹配的是“人记得住的东西”——应用名、描述、单元名/容器名前缀。
 * 不匹配 id：id 是 32 位十六进制，没人会拿它搜，匹配它只会让“搜 3 条出 40 条”更难解释。
 */
export function matchesAppQuery(app: App, query: string): boolean {
  const needle = query.trim().toLowerCase()
  if (needle === '') return true
  const haystack = [app.name, app.description, app.binding.unit, app.binding.container_id]
    .filter((value): value is string => typeof value === 'string' && value !== '')
    .join('\n')
    .toLowerCase()
  return haystack.includes(needle)
}

/**
 * 卡片排序：收藏优先，然后按名称（大小写不敏感），最后按 id 兜底。
 *
 * 服务端已经按 name/id 稳定排序；这里再排一次是因为**收藏是当前用户的状态**，
 * 服务端不知道“对这个人来说哪个常用”。顺序确定，翻页不会重复也不会漏。
 */
export function sortApps(apps: readonly App[]): App[] {
  return [...apps].sort((left, right) => {
    if (left.favorite !== right.favorite) return left.favorite ? -1 : 1
    const byName = left.name.toLowerCase().localeCompare(right.name.toLowerCase(), 'zh-Hans-CN')
    if (byName !== 0) return byName
    return left.id < right.id ? -1 : left.id > right.id ? 1 : 0
  })
}

/**
 * 把应用摆进分组。
 *
 * 顺序：收藏 → 各分组（按 sort_order、name）→ 未分组。空分组也返回（页面据 `apps.length`
 * 决定是否显示“这个分组还没有应用”），因为“分组存在但空”与“没有这个分组”是两件事。
 */
export function bucketApps(
  apps: readonly App[],
  groups: readonly AppGroup[],
  query = '',
): AppBucket[] {
  const needle = query.trim()
  const filtered = sortApps(apps.filter((app) => matchesAppQuery(app, needle)))
  const buckets: AppBucket[] = []

  const favorites = filtered.filter((app) => app.favorite)
  // 搜索时隐藏“收藏”这一格：搜出来的东西已经很少，再拆一层会让人以为结果被吞了。
  if (favorites.length > 0 && needle === '') {
    buckets.push({ key: FAVORITES_KEY, label: '常用入口', apps: favorites })
  }

  const ordered = [...groups].sort((left, right) => {
    if (left.sort_order !== right.sort_order) return left.sort_order - right.sort_order
    return left.name.localeCompare(right.name, 'zh-Hans-CN')
  })
  for (const group of ordered) {
    const members = filtered.filter((app) => app.group_id === group.id)
    if (members.length === 0 && needle !== '') continue
    buckets.push({ key: group.id, label: group.name, apps: members })
  }

  const ungrouped = filtered.filter(
    (app) => app.group_id === null || !groups.some((group) => group.id === app.group_id),
  )
  if (ungrouped.length > 0 || (needle === '' && buckets.length === 0)) {
    buckets.push({ key: UNGROUPED_KEY, label: '未分组', apps: ungrouped })
  }
  return buckets
}

/**
 * 首页“常用入口”：收藏的在前，其次按最近更新。
 *
 * 只取能打开的应用（`capabilities.open`）：首页放一个点不动的入口，等于把“打不开”
 * 当成“入口”。limit 默认 8——首页是概览，不是应用页。
 */
export function frequentEntries(apps: readonly App[], limit = 8): App[] {
  return sortApps(apps)
    .filter((app) => app.capabilities.open && app.public_url)
    .slice(0, limit)
}

// ---------------------------------------------------------------- 入口地址

export interface AppOpenTarget {
  /** 要跳的地址；为空表示没有入口。 */
  href: string
  /** 是否新标签打开（外链一律新标签，且带 noopener）。 */
  external: boolean
  /** 为什么不能打开（没有入口时）。 */
  reason: string
}

/**
 * 一键打开的目标地址。
 *
 * `public_url` 是给浏览器点的地址（可能是反代后的域名），与健康探测的 `target_id`
 * 是两件事。只允许 http/https：`javascript:` / `data:` 这类协议在服务端就被拒了，
 * 这里再挡一次——页面把一个 `javascript:` 塞进 href 的后果不该靠服务端拦。
 */
export function openTarget(app: App): AppOpenTarget {
  const url = app.public_url?.trim() ?? ''
  if (url === '') {
    return { href: '', external: false, reason: '这个应用没有登记入口地址' }
  }
  if (!/^https?:\/\//i.test(url)) {
    return { href: '', external: false, reason: '入口地址不是 http/https，面板不会打开它' }
  }
  return { href: url, external: true, reason: '' }
}

// ---------------------------------------------------------------- 启停（复用的写路径）

export const APP_CONTROL_OPERATIONS = ['start', 'stop', 'restart'] as const
export type AppControlOperation = (typeof APP_CONTROL_OPERATIONS)[number]

export const APP_CONTROL_LABELS: Record<AppControlOperation, string> = {
  start: '启动',
  stop: '停止',
  restart: '重启',
}

export interface AppControlRequest {
  /** 已有的写动作名：systemd 走 service.control，docker 走 container.control。 */
  action: 'service.control' | 'container.control'
  params: Record<string, unknown>
}

/**
 * 一次启停请求：**没有 app.control 这个动作**，走的是已有的两条写路径。
 *
 * 为什么不在 Web 里新开一个 app.control：那等于给“动作表”再加一行，而它要做的
 * 全部事情——校验、预检、依赖卷门禁、执行前复核、审计——都已经挂在这两个动作上了。
 * 新开一行只会让同一次启动有两条代码路径，其中一条迟早会漏掉门禁。
 *
 * 返回 null 表示这个应用此刻没有可操作的运行时（link / compose / 绑定缺失）。
 */
export function controlRequest(app: App, operation: AppControlOperation): AppControlRequest | null {
  switch (app.type) {
    case 'systemd': {
      const unit = app.binding.unit?.trim() ?? ''
      if (unit === '') return null
      return { action: 'service.control', params: { unit, operation } }
    }
    case 'docker': {
      const containerId = app.binding.container_id?.trim() ?? ''
      if (containerId === '') return null
      return { action: 'container.control', params: { container_id: containerId, operation } }
    }
    default:
      return null
  }
}

// ---------------------------------------------------------------- 读取端

export function getApps(query: { group_id?: string; limit?: number; cursor?: string } = {}) {
  const params: Record<string, string | number> = {}
  if (query.group_id) params.group_id = query.group_id
  if (query.limit && query.limit > 0) params.limit = query.limit
  if (query.cursor) params.cursor = query.cursor
  return get<ListData<App>>('/apps', { query: params })
}

export function getApp(id: string): Promise<ApiResult<App>> {
  return get<App>(`/apps/${encodeURIComponent(id)}`)
}

export function getAppGroups(): Promise<ApiResult<ListData<AppGroup>>> {
  return get<ListData<AppGroup>>('/app-groups')
}

export function getAppDiscovery(): Promise<ApiResult<AppDiscoveryData>> {
  return get<AppDiscoveryData>('/apps/discovery')
}

export function getAppHealth(id: string): Promise<ApiResult<AppHealthState>> {
  return get<AppHealthState>(`/apps/${encodeURIComponent(id)}/health`)
}

/** 应用所绑定对象的日志。参数集合由来源决定，传错集合服务端回 400。 */
export function getAppLogs(
  id: string,
  query: AppLogQuery = {},
  signal?: AbortSignal,
): Promise<ApiResult<AppLogsData>> {
  const params: Record<string, string | number> = {}
  if (query.level) params.level = query.level
  if (query.from) params.from = query.from
  if (query.to) params.to = query.to
  if (query.since) params.since = query.since
  if (query.until) params.until = query.until
  if (query.tail !== undefined && query.tail > 0) params.tail = query.tail
  if (query.cursor) params.cursor = query.cursor
  if (query.limit !== undefined && query.limit > 0) params.limit = query.limit
  const options: { query: Record<string, string | number>; signal?: AbortSignal } = { query: params }
  if (signal) options.signal = signal
  return get<AppLogsData>(`/apps/${encodeURIComponent(id)}/logs`, options)
}

// ---------------------------------------------------------------- 写入口

export function createApp(body: AppDefinition): Promise<ApiResult<App>> {
  return request<App>('/apps', { method: 'POST', body })
}

/** `version` 必须是**当前**版本（乐观锁），不是改完之后的版本。 */
export function updateApp(id: string, body: AppUpdate): Promise<ApiResult<App>> {
  return request<App>(`/apps/${encodeURIComponent(id)}`, { method: 'PATCH', body })
}

export function deleteApp(id: string, version: number): Promise<ApiResult<unknown>> {
  return request<unknown>(`/apps/${encodeURIComponent(id)}`, {
    method: 'DELETE',
    query: { version },
  })
}

/** 收藏是当前用户的偏好：权限 V、幂等、PUT、不需要 version、不写审计。 */
export function setFavorite(id: string, favorite: boolean): Promise<ApiResult<{ favorite: boolean }>> {
  return request<{ favorite: boolean }>(`/apps/${encodeURIComponent(id)}/favorite`, {
    method: 'PUT',
    body: { favorite },
  })
}

export interface AppGroupDefinition {
  name: string
  sort_order?: number
}

export function createAppGroup(body: AppGroupDefinition): Promise<ApiResult<AppGroup>> {
  return request<AppGroup>('/app-groups', { method: 'POST', body })
}

export function updateAppGroup(
  id: string,
  version: number,
  body: AppGroupDefinition,
): Promise<ApiResult<AppGroup>> {
  return request<AppGroup>(`/app-groups/${encodeURIComponent(id)}`, {
    method: 'PATCH',
    body: { version, ...body },
  })
}

export function deleteAppGroup(id: string, version: number): Promise<ApiResult<unknown>> {
  return request<unknown>(`/app-groups/${encodeURIComponent(id)}`, {
    method: 'DELETE',
    query: { version },
  })
}
