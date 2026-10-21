// 进程接口 DTO（GET /processes）。
//
// 字段与 api/openapi.yaml 的 ProcessEntry / ProcessListData 逐字对应；
// 语义来源是 CONTRACTS.md 第 10.2 节与 PLAN.md 第 14 节。
//
// 三条纪律（每条都对应一条验收要求）：
//   1) 身份是 pid + start_time_ticks。**任何**终止请求都必须带上 start_time_ticks，
//      界面上只用 pid 展示——但发出去的东西不能只有 pid，否则旧预检会杀到被重用的 PID。
//   2) 按钮只按服务端给的 capabilities 渲染：受保护、zombie/dead 的进程没有按钮。
//      界面不自己推“这个应该能杀”。
//   3) TERM 与 KILL 是两次独立操作：KILL 的预检要求再认证与确认短语。
//      界面不提供“TERM 失败就自动 KILL”的开关。
//
// 命令摘要是**脱敏后**的（服务端把敏感参数的值替换成 ***）。它只用于识别，
// 不能当审计全文，也不能拿来做“按命令名匹配”的判断。

import { get, type ApiResult } from './client'

// ---------------------------------------------------------------- 进程状态

export const PROCESS_STATES = [
  'running',
  'sleeping',
  'disk_sleep',
  'zombie',
  'stopped',
  'tracing',
  'dead',
  'idle',
  'paging',
  'unknown',
] as const

export type ProcessState = (typeof PROCESS_STATES)[number]

export const PROCESS_STATE_LABELS: Record<ProcessState, string> = {
  running: '运行中',
  sleeping: '可中断睡眠',
  disk_sleep: '不可中断等待',
  zombie: '僵尸（等待父进程回收）',
  stopped: '已停止',
  tracing: '被跟踪',
  dead: '已结束',
  idle: '空闲',
  paging: '换页中',
  unknown: '未知状态',
}

export const PROCESS_STATE_TYPES: Record<
  ProcessState,
  'default' | 'info' | 'success' | 'warning' | 'error'
> = {
  running: 'success',
  sleeping: 'default',
  disk_sleep: 'warning',
  zombie: 'warning',
  stopped: 'info',
  tracing: 'info',
  dead: 'default',
  idle: 'default',
  paging: 'info',
  unknown: 'default',
}

/** 未知取值不假装正常：原样显示。 */
export function processStateLabel(state: string): string {
  return (PROCESS_STATE_LABELS as Record<string, string | undefined>)[state] ?? state
}

export function processStateType(
  state: string,
): 'default' | 'info' | 'success' | 'warning' | 'error' {
  return (PROCESS_STATE_TYPES as Record<string, 'default' | 'info' | 'success' | 'warning' | 'error'>)[
    state
  ] ?? 'default'
}

/** 进程是否已经结束（僵尸或已死）：这种进程没有可发送的信号。 */
export function isExited(process: Pick<ProcessEntry, 'state'>): boolean {
  return process.state === 'zombie' || process.state === 'dead'
}

// ---------------------------------------------------------------- 保护名单

export const PROCESS_PROTECTION_CATEGORIES = [
  'init',
  'kernel_thread',
  'self',
  'ssh',
  'systemd_core',
  'network',
  'other_module',
] as const

export type ProcessProtectionCategory = (typeof PROCESS_PROTECTION_CATEGORIES)[number]

export const PROCESS_PROTECTION_LABELS: Record<ProcessProtectionCategory, string> = {
  init: 'init（PID 1）',
  kernel_thread: '内核线程',
  self: '面板或代理自身',
  ssh: '远程登录',
  systemd_core: '系统核心服务',
  network: '网络基础服务',
  other_module: '由其他模块管理',
}

export function processProtectionLabel(category: string | undefined): string {
  if (category === undefined || category === '') return '受保护'
  return (PROCESS_PROTECTION_LABELS as Record<string, string | undefined>)[category] ?? '受保护'
}

// ---------------------------------------------------------------- 信号

export const PROCESS_SIGNALS = ['TERM', 'KILL'] as const

export type ProcessSignal = (typeof PROCESS_SIGNALS)[number]

export const SIGNAL_LABELS: Record<ProcessSignal, string> = {
  TERM: '请求退出（SIGTERM）',
  KILL: '强制结束（SIGKILL）',
}

/** 二次确认卡上的动词（“确定要请求退出 PID 39515 吗？”）。 */
export const SIGNAL_VERBS: Record<ProcessSignal, string> = {
  TERM: '请求退出',
  KILL: '强制结束',
}

/** 把任意字符串判成信号；不是闭集里的值返回 null。 */
export function toProcessSignal(value: unknown): ProcessSignal | null {
  if (typeof value !== 'string') return null
  return (PROCESS_SIGNALS as readonly string[]).includes(value)
    ? (value as ProcessSignal)
    : null
}

/** KILL 需要再认证与确认短语；TERM 不需要。 */
export function signalNeedsPhrase(signal: ProcessSignal): boolean {
  return signal === 'KILL'
}

// ---------------------------------------------------------------- DTO

export interface ProcessProtection {
  protected: boolean
  category?: string
  reasons: string[]
}

/**
 * 一个进程。
 *
 * capabilities 来自服务端：受保护与已退出的进程一律是空数组。
 * 界面**只能**按它渲染按钮，不自己推“现在应该能 KILL”。
 */
export interface ProcessEntry {
  pid: number
  /** 开机以来的时钟滴答数；与 pid 一起构成身份。 */
  start_time_ticks: number
  ppid: number
  owner: string
  state: string
  /** 相对单核的百分比；第一次采集为 null（不是 0）。 */
  cpu_percent: number | null
  /** 十进制字符串；内核线程与僵尸为 null。 */
  memory_bytes: string | null
  /** 脱敏后的命令摘要，不是完整命令行。 */
  command: string
  /** 所属 systemd 单元；空串表示不在任何单元里。 */
  unit: string
  protection: ProcessProtection
  capabilities: ProcessSignal[]
}

export interface ProcessListData {
  status: 'ok' | 'error'
  error?: string
  collected_at: string
  interval_ms: number
  total: number
  truncated: boolean
  processes: ProcessEntry[]
}

export function fetchProcesses(signal?: AbortSignal): Promise<ApiResult<ProcessListData>> {
  return get<ProcessListData>('/processes', {
    timeoutMs: 10_000,
    ...(signal === undefined ? {} : { signal }),
  })
}

// ---------------------------------------------------------------- 纯函数

/** 进程当前是否有能力发送某个信号（唯一判据是服务端的 capabilities）。 */
export function canSignal(process: ProcessEntry, signal: ProcessSignal): boolean {
  return process.capabilities.includes(signal)
}

/** 进程身份指纹的输入。**不能**只有 pid：pid 会被重用。 */
export interface ProcessIdentity {
  pid: number
  start_time_ticks: number
}

/**
 * process.signal 的参数。
 *
 * 只有三个字段，且 start_time_ticks 必填：服务端会拒绝缺少它的请求，
 * 界面在这里就不给“只带 pid 的请求”留下可能。
 */
export function signalParams(
  process: ProcessIdentity,
  signal: ProcessSignal,
): { pid: number; start_time_ticks: number; signal: ProcessSignal } {
  return { pid: process.pid, start_time_ticks: process.start_time_ticks, signal }
}

/** 身份是否相同：两个界面对象指向同一个进程时才算同一个。 */
export function sameProcess(a: ProcessIdentity, b: ProcessIdentity): boolean {
  return a.pid === b.pid && a.start_time_ticks === b.start_time_ticks
}

/**
 * 按关键字过滤（pid、属主、命令摘要、单元名，大小写不敏感）。
 *
 * 这是**客户端过滤**，不是查询参数：processes.list 不接受任何参数，
 * 也就不可能被用来向内核传条件。
 */
export function filterProcesses(
  processes: readonly ProcessEntry[],
  keyword: string,
): ProcessEntry[] {
  const needle = keyword.trim().toLowerCase()
  if (needle === '') return [...processes]
  return processes.filter(
    (process) =>
      String(process.pid) === needle ||
      String(process.pid).startsWith(needle) ||
      process.owner.toLowerCase().includes(needle) ||
      process.command.toLowerCase().includes(needle) ||
      process.unit.toLowerCase().includes(needle),
  )
}

/** 列表摘要：统计而不是让页面自己数。 */
export interface ProcessSummaryCounts {
  total: number
  running: number
  sleeping: number
  exited: number
  protected: number
  killable: number
}

export function summarizeProcesses(processes: readonly ProcessEntry[]): ProcessSummaryCounts {
  const counts: ProcessSummaryCounts = {
    total: processes.length,
    running: 0,
    sleeping: 0,
    exited: 0,
    protected: 0,
    killable: 0,
  }
  for (const process of processes) {
    if (process.state === 'running') counts.running += 1
    if (process.state === 'sleeping' || process.state === 'idle') counts.sleeping += 1
    if (isExited(process)) counts.exited += 1
    if (process.protection.protected) counts.protected += 1
    if (process.capabilities.length > 0) counts.killable += 1
  }
  return counts
}

/** CPU 百分比的显示文本（null 表示这一轮没有基线，不显示成 0%）。 */
export function formatCPUPercent(value: number | null): string {
  if (value === null) return '—'
  return `${value.toFixed(1)}%`
}
