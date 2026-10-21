// 系统资源接口 DTO（GET /system/summary，CONTRACTS.md 第 6.1 节）。
//
// 与 api/openapi.yaml 的 SystemSummary 及其子结构逐字对应，字段名保持 snake_case：
// 前端不另造一套“驼峰视图模型”，否则契约一改，两边含义就会悄悄分叉。
//
// 两条展示规则在这里就定死，页面不再自行发挥：
//   - 段的 status 是 ok / error / unavailable 三态，**不是零值**：
//     error 与 unavailable 都不能把字段画成 0，必须换成对应状态面板；
//   - 字节字段是十进制字符串或 null，null 表示这次没采到。

import { get, type ApiResult } from './client'

/** 采集段最近一次尝试的结果（CONTRACTS.md 6.1）。 */
export type SystemSectionStatus = 'ok' | 'error' | 'unavailable'

export interface SystemCpuCore {
  index: number
  usage_percent: number | null
}

export interface SystemHostSection {
  status: SystemSectionStatus
  error?: string | undefined
  source?: string | undefined
  collected_at?: string | null | undefined
  hostname?: string | undefined
  os_id?: string | undefined
  os_name?: string | undefined
  os_version?: string | undefined
  os_pretty_name?: string | undefined
  kernel_release?: string | undefined
  arch?: string | undefined
  boot_id?: string | undefined
  uptime_seconds: number | null
}

export interface SystemCpuSection {
  status: SystemSectionStatus
  error?: string | undefined
  source?: string | undefined
  collected_at?: string | null | undefined
  model?: string | undefined
  logical_cores: number
  /** null = 没有可比较的前一次样本，不是 0%。 */
  usage_percent: number | null
  sample_window_ms?: number | null | undefined
  per_cpu?: SystemCpuCore[] | undefined
}

export interface SystemMemorySection {
  status: SystemSectionStatus
  error?: string | undefined
  source?: string | undefined
  collected_at?: string | null | undefined
  total_bytes: string | null
  free_bytes: string | null
  available_bytes: string | null
  buffers_bytes: string | null
  cached_bytes: string | null
  /** Total - Available（可用内存口径），不是 Total - Free。 */
  used_bytes: string | null
  swap_total_bytes: string | null
  swap_free_bytes: string | null
}

export interface SystemLoadSection {
  status: SystemSectionStatus
  error?: string | undefined
  source?: string | undefined
  collected_at?: string | null | undefined
  load1: number | null
  load5: number | null
  load15: number | null
  runnable: number | null
  threads: number | null
}

export interface SystemNetInterface {
  name: string
  /** /sys/class/net/<name>/operstate；读不到时字段不存在。 */
  oper_state?: string | undefined
  loopback: boolean
  rx_bytes: string
  tx_bytes: string
  rx_packets: string
  tx_packets: string
  rx_errors: string
  tx_errors: string
  rx_dropped: string
  tx_dropped: string
  /** null = 无可比较样本或计数器重置。 */
  rx_bytes_per_second: number | null
  tx_bytes_per_second: number | null
}

export interface SystemNetworkSection {
  status: SystemSectionStatus
  error?: string | undefined
  source?: string | undefined
  collected_at?: string | null | undefined
  interfaces?: SystemNetInterface[] | undefined
}

export interface SystemSummary {
  collected_at: string
  interval_ms: number
  /** 快照超过 3 个采集间隔。只说明这份快照旧，不说明各段成败。 */
  stale: boolean
  /** 至少一个段的 status 不是 ok。 */
  degraded: boolean
  host: SystemHostSection
  cpu: SystemCpuSection
  memory: SystemMemorySection
  load: SystemLoadSection
  network: SystemNetworkSection
}

/** 各段的键，用于 degraded 时列出“哪几段不可用”。 */
export const SYSTEM_SECTIONS = ['host', 'cpu', 'memory', 'load', 'network'] as const

export type SystemSectionKey = (typeof SYSTEM_SECTIONS)[number]

export const SYSTEM_SECTION_LABELS: Record<SystemSectionKey, string> = {
  host: '主机',
  cpu: 'CPU',
  memory: '内存',
  load: '负载',
  network: '网络',
}

/** 该段是否可用（ok 才是可用）。 */
export function isSectionOk(section: { status: SystemSectionStatus }): boolean {
  return section.status === 'ok'
}

/**
 * 不可用段的清单，用于页头“部分数据不可用”的说明。
 * 只有 status 明确不是 ok 的段才算；这里不推断字段是否为空。
 */
export function degradedSections(summary: SystemSummary): SystemSectionKey[] {
  return SYSTEM_SECTIONS.filter((key) => summary[key].status !== 'ok')
}

/** 取 /system/summary 快照。 */
export function fetchSystemSummary(signal?: AbortSignal): Promise<ApiResult<SystemSummary>> {
  return get<SystemSummary>('/system/summary', {
    timeoutMs: 10_000,
    ...(signal === undefined ? {} : { signal }),
  })
}
