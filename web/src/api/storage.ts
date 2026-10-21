// 存储接口 DTO（GET /storage/topology、/storage/devices、/storage/devices/{id}）。
//
// 与 api/openapi.yaml 的 Storage* schema 逐字对应，字段名保持 snake_case；
// 与 CONTRACTS.md 第 6、6.1、6.2 节一起看。
//
// 三个要点在这里写成类型与注释，页面不再各自解释：
//   1) 未知量是 null，不是 0 或空串；字节数是十进制字符串，展示走 utils/format；
//   2) health.status 是封闭集合，healthy 必须有来源自己的判定支撑（P04.3）；
//   3) capabilities 在 P04.4 恒为空数组——写操作能力此时不启用，界面不能预告动作。

import { get, type ApiResult } from './client'

// ---------------------------------------------------------------- 设备类型

export const DEVICE_KINDS = [
  'disk',
  'partition',
  'lvm',
  'raid',
  'crypt',
  'loop',
  'rom',
  'other',
] as const

export type DeviceKind = (typeof DEVICE_KINDS)[number]

export const DEVICE_KIND_LABELS: Record<DeviceKind, string> = {
  disk: '磁盘',
  partition: '分区',
  lvm: 'LVM 逻辑卷',
  raid: '软 RAID',
  crypt: '加密卷',
  loop: '循环设备',
  rom: '光驱',
  other: '其他',
}

/**
 * 默认是否折起来（不删除）。
 *
 * loop 里有大量快照/镜像占位设备，rom 通常没有介质：它们在磁盘列表里默认收起，
 * 但可以显式展开。折叠是**显示**选择，不是“假装它们不存在”。
 */
export function isBackgroundDevice(kind: DeviceKind): boolean {
  return kind === 'loop' || kind === 'rom'
}

// ---------------------------------------------------------------- 健康

export const HEALTH_STATUSES = [
  'healthy',
  'warning',
  'critical',
  'unknown',
  'unsupported',
  'sleeping',
] as const

export type HealthStatus = (typeof HEALTH_STATUSES)[number]

export const HEALTH_STATUS_LABELS: Record<HealthStatus, string> = {
  healthy: '正常',
  warning: '注意',
  critical: '危险',
  unknown: '未知',
  unsupported: '不支持检测',
  sleeping: '休眠中',
}

export const HEALTH_STATUS_TYPES: Record<
  HealthStatus,
  'default' | 'info' | 'success' | 'warning' | 'error'
> = {
  healthy: 'success',
  warning: 'warning',
  critical: 'error',
  unknown: 'default',
  unsupported: 'info',
  sleeping: 'info',
}

/** 需要人看一眼的状态（正常、休眠、不支持检测都不需要）。 */
export function healthNeedsAttention(status: HealthStatus): boolean {
  return status === 'warning' || status === 'critical'
}

// ---------------------------------------------------------------- 身份

export const IDENTITY_CONFIDENCES = ['high', 'medium', 'low', 'unknown'] as const

export type IdentityConfidence = (typeof IDENTITY_CONFIDENCES)[number]

export const IDENTITY_CONFIDENCE_LABELS: Record<IdentityConfidence, string> = {
  high: '高（有 WWN 或序列号）',
  medium: '中（只有稳定的 scsi-/ata- 别名）',
  low: '低（只有型号与拓扑地址）',
  unknown: '未知（连型号都没有）',
}

// ---------------------------------------------------------------- 结构

export type EdgeRelation = 'partition_of' | 'depends_on'

export const EDGE_RELATION_LABELS: Record<EdgeRelation, string> = {
  partition_of: '所属整盘',
  depends_on: '依赖下层',
}

export interface StorageEdge {
  from_device_id: string
  to_device_id: string
  relation: EdgeRelation
}

export interface StorageIORate {
  interval_ms: number
  read_bytes_per_second: string
  write_bytes_per_second: string
  read_iops: number
  write_iops: number
  /** 没有完成任何读时为 null，不能当作 0 ms。 */
  read_latency_ms: number | null
  write_latency_ms: number | null
}

export interface StorageIO {
  sampled_at: string
  read_bytes_total: string
  write_bytes_total: string
  reads_total: string
  writes_total: string
  /** null = 本次没有可用采样窗口（首次采样、计数器变小、窗口异常）。 */
  rate: StorageIORate | null
}

export interface StorageCapacity {
  total_bytes: string
  available_bytes: string
  used_percent: number
  inode_total: string
  inode_available: string
  inode_used_percent: number
  collected_at: string
}

export interface StorageMount {
  path: string
  source: string
  fstype: string
  options: string[]
  /** 来自挂载选项里的 ro，不是设备本身的只读标记。 */
  read_only: boolean
  /** 读不到容量时是 null，不是全 0。 */
  capacity: StorageCapacity | null
  /** 说明 capacity 为什么是 null（含网络挂载的超时与重试间隔）。 */
  capacity_error?: string | undefined
}

export interface StorageFilesystem {
  type: string | null
  uuid: string | null
  label: string | null
  mounts: StorageMount[]
}

export interface StoragePartition {
  device_id: string
  number: number
  start_bytes: string
  size_bytes: string
}

export interface StorageFreeRegion {
  start_bytes: string
  size_bytes: string
}

export type PartitionTableType = 'gpt' | 'mbr' | 'none' | 'unknown'

export type PartitionTableSource = 'device' | 'kernel' | 'skipped'

export const PARTITION_TABLE_TYPE_LABELS: Record<PartitionTableType, string> = {
  gpt: 'GPT',
  mbr: 'MBR',
  none: '无分区表',
  unknown: '未知',
}

export const PARTITION_TABLE_SOURCE_LABELS: Record<PartitionTableSource, string> = {
  device: '读了磁盘上的分区表',
  kernel: '只用了内核的分区列表（类型未知）',
  skipped: '盘在休眠或读取失败，没有去读它',
}

export interface StoragePartitionTable {
  type: PartitionTableType
  partitions: StoragePartition[]
  free_regions: StorageFreeRegion[]
  source: PartitionTableSource
  warning?: string | undefined
}

export interface StorageHealth {
  status: HealthStatus
  /** 没有可读温度时是 null（不是 0 °C）。 */
  temperature_celsius: number | null
  /** 数据来源（smartctl、nvme、none）。 */
  source: string
  checked_at: string | null
  /** 来源原话（smartctl 的 messages），用于解释判定。 */
  details: string | null
}

export type CapabilityState = 'available' | 'blocked' | 'unsupported'

export interface StorageCapability {
  action: string
  state: CapabilityState
  reason?: string | undefined
}

export interface StorageProtection {
  system_disk: boolean
  reasons: string[]
  sources: string[]
}

export interface StorageSwap {
  type: 'partition' | 'file'
  size_bytes: string
  used_bytes: string
  priority: number
}

export interface StorageDevice {
  /** 内核设备名（sda、sda1、dm-0）。一次启动内唯一，用作 API 路径参数。 */
  id: string
  kind: DeviceKind
  name: string
  size_bytes: string
  logical_sector_bytes: number
  physical_sector_bytes: number
  model: string | null
  serial: string | null
  wwn: string | null
  /** virtio、nvme、usb、scsi、ata、mmc、unknown 或 null。 */
  transport: string | null
  rotational: boolean
  read_only: boolean
  /** 休眠的设备仍然 online。 */
  online: boolean
  identity_confidence: IdentityConfidence
  identity_fingerprint: string
  filesystem: StorageFilesystem | null
  partition_table: StoragePartitionTable | null
  health: StorageHealth
  /** P04.4 恒为空：写动作登记表要到 P07 才有内容。 */
  capabilities: StorageCapability[]
  protection: StorageProtection
  /** 直接依赖这台设备的设备 id（sysfs holders）。 */
  holders?: string[] | undefined
  swap?: StorageSwap | null | undefined
  /** 这块设备自己的 I/O 计数与速率；读不到 /proc/diskstats 时为 null。 */
  io: StorageIO | null
  collected_at: string
}

export interface StorageNetworkMount {
  path: string
  source: string
  fstype: string
  read_only: boolean
  /** true=容量读到了；false=确定失败；null=还没查过。三者不能混用。 */
  reachable: boolean | null
  capacity: StorageCapacity | null
  capacity_error?: string | undefined
}

export type SnapshotStatus = 'ok' | 'error'

export interface StorageTopology {
  status: SnapshotStatus
  /** 扫描失败的原因（status=error 时存在）。 */
  error?: string | undefined
  snapshot_version: string
  collected_at: string
  interval_ms: number
  nodes: StorageDevice[]
  edges: StorageEdge[]
  network_mounts: StorageNetworkMount[]
}

export interface StorageDeviceList {
  status: SnapshotStatus
  error?: string | undefined
  snapshot_version: string
  collected_at: string
  interval_ms: number
  devices: StorageDevice[]
  edges: StorageEdge[]
}

export interface StorageDeviceDetail {
  status: SnapshotStatus
  error?: string | undefined
  snapshot_version: string
  collected_at: string
  interval_ms: number
  device: StorageDevice
  edges: StorageEdge[]
}

// ---------------------------------------------------------------- 查询

export function fetchStorageTopology(signal?: AbortSignal): Promise<ApiResult<StorageTopology>> {
  return get<StorageTopology>('/storage/topology', {
    timeoutMs: 10_000,
    ...(signal === undefined ? {} : { signal }),
  })
}

export function fetchStorageDevices(signal?: AbortSignal): Promise<ApiResult<StorageDeviceList>> {
  return get<StorageDeviceList>('/storage/devices', {
    timeoutMs: 10_000,
    ...(signal === undefined ? {} : { signal }),
  })
}

/**
 * 单台设备详情。
 *
 * id 是内核设备名，只作为路径参数使用；服务端要求 ^[A-Za-z0-9._-]+$，
 * 因此这里先挡掉不合法的 id（带斜杠的名字永远不该被拼进 URL）。
 */
export function fetchStorageDevice(
  id: string,
  signal?: AbortSignal,
): Promise<ApiResult<StorageDeviceDetail>> {
  if (!DEVICE_ID_PATTERN.test(id)) {
    return Promise.reject(new RangeError(`设备 id 不合法：${id}`))
  }
  return get<StorageDeviceDetail>(`/storage/devices/${encodeURIComponent(id)}`, {
    timeoutMs: 10_000,
    ...(signal === undefined ? {} : { signal }),
  })
}

/** 与服务端 *id* 参数一致的语法（openapi：minLength 1、maxLength 64）。 */
export const DEVICE_ID_PATTERN = /^[A-Za-z0-9._-]{1,64}$/

// ---------------------------------------------------------------- 纯函数

/** 按 id 找设备；找不到返回 undefined（页面据此显示“设备已不存在”）。 */
export function findDevice(
  devices: readonly StorageDevice[],
  id: string,
): StorageDevice | undefined {
  return devices.find((device) => device.id === id)
}

/**
 * 拆成“主要设备”和“背景设备”（loop/rom）。
 * 两者都返回，页面自己决定是否折叠；不在这里偷偷丢掉任何设备。
 */
export function splitDevices(devices: readonly StorageDevice[]): {
  primary: StorageDevice[]
  background: StorageDevice[]
} {
  const primary: StorageDevice[] = []
  const background: StorageDevice[] = []
  for (const device of devices) {
    if (isBackgroundDevice(device.kind)) background.push(device)
    else primary.push(device)
  }
  return { primary, background }
}

/** 设备是否已挂载（有挂载点）。 */
export function isMounted(device: StorageDevice): boolean {
  return (device.filesystem?.mounts.length ?? 0) > 0
}

/** 第一个挂载点的容量；没有挂载或容量没读到返回 null。 */
export function primaryCapacity(device: StorageDevice): StorageCapacity | null {
  const mounts = device.filesystem?.mounts ?? []
  for (const mount of mounts) {
    if (mount.capacity !== null) return mount.capacity
  }
  return null
}

/** 最满的那个挂载点的使用率，用于列表里的容量条；没有读数返回 null。 */
export function highestUsedPercent(device: StorageDevice): number | null {
  const mounts = device.filesystem?.mounts ?? []
  let highest: number | null = null
  for (const mount of mounts) {
    if (mount.capacity === null) continue
    if (highest === null || mount.capacity.used_percent > highest) {
      highest = mount.capacity.used_percent
    }
  }
  return highest
}

/** 设备上所有挂载点；没有文件系统时是空数组（不是 null）。 */
export function mountsOf(device: StorageDevice): StorageMount[] {
  return device.filesystem?.mounts ?? []
}

/** 是否有任何一块盘需要人注意（用于页面标题上的提醒）。 */
export function attentionDevices(devices: readonly StorageDevice[]): StorageDevice[] {
  return devices.filter((device) => healthNeedsAttention(device.health.status))
}

/**
 * 网络挂载的可达性有三种取值，说明必须区分开：
 * true=容量读到了；false=**确定失败**；null=还没查过。
 * 三者混用会让人把“还没查”当成“挂了”，或者把“挂了”当成“正常”。
 */
export function reachabilityLabel(reachable: boolean | null): string {
  if (reachable === true) return '可达（容量已读到）'
  if (reachable === false) return '不可达（读取失败）'
  return '尚未检查（刚挂上或刚启动）'
}

/** 网络挂载是否需要人处理：只有明确失败才算。null 不算。 */
export function needsAttentionNetworkMount(mount: StorageNetworkMount): boolean {
  return mount.reachable === false
}
