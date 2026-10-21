// 服务单元接口 DTO（GET /services、/services/{unit}）。
//
// 字段与 api/openapi.yaml 的 ServiceUnit / ServiceListData 逐字对应；
// 语义来源是 CONTRACTS.md 第 10 节与 PLAN.md 第 14 节。
//
// 这一层只做三件事：类型、封闭集合的中文标签、以及“能不能点这个按钮”的判决。
// 判决必须与服务端的 capabilities 一致——但**不重复计算**：按钮由服务端给的
// capabilities 决定，这里的函数只做二次校验与显示文案。
//
// 三条纪律：
//   1) 受保护单元（面板/代理/SSH/核心/网络/其他模块）永远没有能力，界面不许给它按钮；
//   2) enablement 是“开机启动”，与 active_state 是两件事，合并显示会把
//      “运行中但没设开机启动”说成“正常”；
//   3) 名单里的 PID、内存、启动时间都可能缺失（null），缺就是缺，不补 0。

import { get, type ApiResult } from './client'

// ---------------------------------------------------------------- 装载状态

export const LOAD_STATES = [
  'loaded',
  'not-found',
  'bad-setting',
  'error',
  'merged',
  'masked',
] as const

export type LoadState = (typeof LOAD_STATES)[number]

export const LOAD_STATE_LABELS: Record<LoadState, string> = {
  loaded: '已装载',
  'not-found': '不在这台机器上',
  'bad-setting': '配置有错误',
  error: '装载失败',
  merged: '已合并',
  masked: '已屏蔽',
}

export const LOAD_STATE_TYPES: Record<
  LoadState,
  'default' | 'info' | 'success' | 'warning' | 'error'
> = {
  loaded: 'success',
  'not-found': 'warning',
  'bad-setting': 'error',
  error: 'error',
  merged: 'info',
  masked: 'warning',
}

/** 未知取值不假装正常：原样显示，标签为“未知”。 */
export function loadStateLabel(state: string): string {
  return (LOAD_STATE_LABELS as Record<string, string | undefined>)[state] ?? state
}

export function loadStateType(
  state: string,
): 'default' | 'info' | 'success' | 'warning' | 'error' {
  return (LOAD_STATE_TYPES as Record<string, 'default' | 'info' | 'success' | 'warning' | 'error'>)[
    state
  ] ?? 'default'
}

// ---------------------------------------------------------------- 运行状态

export const ACTIVE_STATES = [
  'active',
  'reloading',
  'inactive',
  'failed',
  'activating',
  'deactivating',
  'maintenance',
] as const

export type ActiveState = (typeof ACTIVE_STATES)[number]

export const ACTIVE_STATE_LABELS: Record<ActiveState, string> = {
  active: '运行中',
  reloading: '重新加载中',
  inactive: '未运行',
  failed: '失败',
  activating: '启动中',
  deactivating: '停止中',
  maintenance: '维护中',
}

export const ACTIVE_STATE_TYPES: Record<
  ActiveState,
  'default' | 'info' | 'success' | 'warning' | 'error'
> = {
  active: 'success',
  reloading: 'info',
  inactive: 'default',
  failed: 'error',
  activating: 'info',
  deactivating: 'info',
  maintenance: 'warning',
}

export function activeStateLabel(state: string): string {
  return (ACTIVE_STATE_LABELS as Record<string, string | undefined>)[state] ?? state
}

export function activeStateType(
  state: string,
): 'default' | 'info' | 'success' | 'warning' | 'error' {
  return (ACTIVE_STATE_TYPES as Record<
    string,
    'default' | 'info' | 'success' | 'warning' | 'error'
  >)[state] ?? 'default'
}

// ---------------------------------------------------------------- 开机启动

export const ENABLEMENTS = [
  'enabled',
  'disabled',
  'static',
  'indirect',
  'masked',
  'generated',
  'transient',
  'alias',
  'linked',
  'unknown',
] as const

export type Enablement = (typeof ENABLEMENTS)[number]

export const ENABLEMENT_LABELS: Record<Enablement, string> = {
  enabled: '开机启动',
  disabled: '不自动启动',
  static: '无安装段（static）',
  indirect: '间接启用（indirect）',
  masked: '已屏蔽（masked）',
  generated: '生成单元（generated）',
  transient: '临时单元（transient）',
  alias: '别名（alias）',
  linked: '链接单元（linked）',
  unknown: '未知',
}

export function enablementLabel(state: string): string {
  return (ENABLEMENT_LABELS as Record<string, string | undefined>)[state] ?? state
}

// ---------------------------------------------------------------- 保护名单

export const PROTECTION_CATEGORIES = ['self', 'ssh', 'systemd_core', 'network', 'other_module'] as const

export type ProtectionCategory = (typeof PROTECTION_CATEGORIES)[number]

export const PROTECTION_CATEGORY_LABELS: Record<ProtectionCategory, string> = {
  self: '面板自身',
  ssh: '远程登录',
  systemd_core: '系统核心服务',
  network: '网络基础服务',
  other_module: '由其他模块管理',
}

export function protectionCategoryLabel(category: string | undefined): string {
  if (category === undefined || category === '') return '受保护'
  return (PROTECTION_CATEGORY_LABELS as Record<string, string | undefined>)[category] ?? '受保护'
}

// ---------------------------------------------------------------- 操作

export const SERVICE_OPERATIONS = ['start', 'stop', 'restart', 'enable', 'disable'] as const

export type ServiceOperation = (typeof SERVICE_OPERATIONS)[number]

export const SERVICE_OPERATION_LABELS: Record<ServiceOperation, string> = {
  start: '启动',
  stop: '停止',
  restart: '重启',
  enable: '设为开机启动',
  disable: '取消开机启动',
}

/** 二次确认对话框里的动词（“确定要停止 cron.service 吗？”）。 */
export const SERVICE_OPERATION_VERBS: Record<ServiceOperation, string> = {
  start: '启动',
  stop: '停止',
  restart: '重启',
  enable: '设为开机启动',
  disable: '取消开机启动',
}

/** 是否属于“会中断服务”的操作（界面要把后果说在前面）。 */
export function interruptsService(op: ServiceOperation): boolean {
  return op === 'stop' || op === 'restart'
}

/** 把任意字符串判成操作；不是闭集里的值返回 null。 */
export function toServiceOperation(value: unknown): ServiceOperation | null {
  if (typeof value !== 'string') return null
  return (SERVICE_OPERATIONS as readonly string[]).includes(value)
    ? (value as ServiceOperation)
    : null
}

// ---------------------------------------------------------------- DTO

export interface ServiceProtection {
  protected: boolean
  category?: string
  reasons: string[]
}

/**
 * 一个 systemd service 单元。
 *
 * capabilities 来自服务端：受保护、模板、not-found 的单元一律是空数组。
 * 界面**只能**按它渲染按钮，不自己推“现在应该能 stop”这种结论。
 */
export interface ServiceUnit {
  unit: string
  description: string
  load_state: string
  active_state: string
  sub_state: string
  unit_file_state: string
  enablement: string
  template: boolean
  main_pid: number | null
  started_at: string | null
  restart_count: number | null
  memory_bytes: string | null
  tasks_current: number | null
  fragment_path: string
  protection: ServiceProtection
  capabilities: ServiceOperation[]
  checked_at: string
}

export interface ServiceListData {
  status: 'ok' | 'error'
  error?: string
  snapshot_version: string
  collected_at: string
  interval_ms: number
  truncated: boolean
  units: ServiceUnit[]
}

export interface ServiceDetailData {
  unit: ServiceUnit
}

/** 与服务端 *unit* 参数一致的语法（字母数字开头，允许 : _ . @ -，必须以 .service 结尾）。 */
export const SERVICE_UNIT_PATTERN = /^[A-Za-z0-9][A-Za-z0-9:_.@-]{0,255}$/

/**
 * 单元名是否可安全拼进 URL。
 *
 * 服务端会再校验一次；这里先挡掉带斜杠、空格、元字符、不以 .service 结尾的名字，
 * 让“奇怪的参数”连一个请求都发不出去。
 */
export function isValidUnitName(name: string): boolean {
  if (!SERVICE_UNIT_PATTERN.test(name)) return false
  if (name.includes('..')) return false
  return name.endsWith('.service')
}

export function fetchServices(signal?: AbortSignal): Promise<ApiResult<ServiceListData>> {
  return get<ServiceListData>('/services', {
    timeoutMs: 10_000,
    ...(signal === undefined ? {} : { signal }),
  })
}

export function fetchService(
  unit: string,
  signal?: AbortSignal,
): Promise<ApiResult<ServiceDetailData>> {
  if (!isValidUnitName(unit)) {
    return Promise.reject(new RangeError(`服务单元名不合法：${unit}`))
  }
  return get<ServiceDetailData>(`/services/${encodeURIComponent(unit)}`, {
    timeoutMs: 10_000,
    ...(signal === undefined ? {} : { signal }),
  })
}

// ---------------------------------------------------------------- 纯函数

/** 单元当前是否有能力执行某个操作（唯一判据是服务端的 capabilities）。 */
export function canOperate(unit: ServiceUnit, op: ServiceOperation): boolean {
  return unit.capabilities.includes(op)
}

/** 单元是否“不在这台机器上”（装载状态 not-found）。 */
export function isMissingUnit(unit: ServiceUnit): boolean {
  return unit.load_state === 'not-found'
}

/**
 * 需要人看一眼的单元：失败、装载错误、或 not-found。
 * 运行中且已设开机启动的单元不需要标记。
 */
export function needsAttention(unit: ServiceUnit): boolean {
  if (unit.active_state === 'failed') return true
  if (unit.load_state === 'not-found' || unit.load_state === 'error') return true
  if (unit.load_state === 'bad-setting') return true
  return false
}

/** 列表摘要：统计而不是让页面自己数。 */
export interface ServiceSummaryCounts {
  total: number
  running: number
  stopped: number
  failed: number
  protected: number
  missing: number
  attention: number
}

export function summarizeUnits(units: readonly ServiceUnit[]): ServiceSummaryCounts {
  const counts: ServiceSummaryCounts = {
    total: units.length,
    running: 0,
    stopped: 0,
    failed: 0,
    protected: 0,
    missing: 0,
    attention: 0,
  }
  for (const unit of units) {
    switch (unit.active_state) {
      case 'active':
      case 'reloading':
        counts.running += 1
        break
      case 'failed':
        counts.failed += 1
        break
      case 'inactive':
        counts.stopped += 1
        break
      default:
        break
    }
    if (unit.protection.protected) counts.protected += 1
    if (isMissingUnit(unit)) counts.missing += 1
    if (needsAttention(unit)) counts.attention += 1
  }
  return counts
}

/**
 * 按关键字过滤（单元名 + 描述，大小写不敏感）。
 *
 * 这是**客户端过滤**，不是查询参数：services.list 不接受任何参数，
 * 也就不可能被用来向 systemctl 传条件。
 */
export function filterUnits(units: readonly ServiceUnit[], keyword: string): ServiceUnit[] {
  const needle = keyword.trim().toLowerCase()
  if (needle === '') return [...units]
  return units.filter(
    (unit) =>
      unit.unit.toLowerCase().includes(needle) ||
      unit.description.toLowerCase().includes(needle),
  )
}

/**
 * 分组成“需要注意 / 运行中 / 未运行 / 其他”，每组内部保持服务端顺序。
 *
 * 页面按组渲染是为了让人先看到失败的单元；不丢任何单元（每组都返回）。
 */
export function groupUnits(units: readonly ServiceUnit[]): {
  attention: ServiceUnit[]
  running: ServiceUnit[]
  stopped: ServiceUnit[]
  other: ServiceUnit[]
} {
  const groups = {
    attention: [] as ServiceUnit[],
    running: [] as ServiceUnit[],
    stopped: [] as ServiceUnit[],
    other: [] as ServiceUnit[],
  }
  for (const unit of units) {
    if (needsAttention(unit)) {
      groups.attention.push(unit)
    } else if (unit.active_state === 'active' || unit.active_state === 'reloading') {
      groups.running.push(unit)
    } else if (unit.active_state === 'inactive') {
      groups.stopped.push(unit)
    } else {
      groups.other.push(unit)
    }
  }
  return groups
}
