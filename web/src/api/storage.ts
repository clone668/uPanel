// 存储接口 DTO（GET /storage/topology、/storage/devices、/storage/devices/{id}）。
//
// 与 api/openapi.yaml 的 Storage* schema 逐字对应，字段名保持 snake_case；
// 与 CONTRACTS.md 第 6、6.1、6.2、6.3 节一起看。
//
// 三个要点在这里写成类型与注释，页面不再各自解释：
//   1) 未知量是 null，不是 0 或空串；字节数是十进制字符串，展示走 utils/format；
//   2) health.status 是封闭集合，healthy 必须有来源自己的判定支撑（P04.3）；
//   3) capabilities 自 P07.1 起由代理的判决层算出（不再是恒空的占位）：只有
//      **当前这块设备真的能做**的动作才是 available，其余是 blocked/unsupported，
//      并且带原因。界面照它显示，不自己推断“应该能做什么”。
//      P07.2 登记了三个分区动作的执行器，格式化（P07.3）仍未登记。

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

/**
 * 盘上 GPT 条目表里的一个槽位（CONTRACTS 第 6.3 节，P07.2 起）。
 *
 * 它和 `partitions` 是**两个来源**：`partitions` 来自 sysfs（内核此刻认定的分区），
 * 这里来自盘上的字节。看分区表时以后者为准，看"内核有没有重读"时以前者为准。
 */
export interface StorageGPTEntry {
  /** 条目表里的槽位号，也是分区编号；delete 不会重排，删掉 2 号后 1 号仍是 1 号。 */
  number: number
  start_bytes: string
  size_bytes: string
  /** 分区类型 GUID 的规范大写形态（type=linux 即 0FC63DAF-…）。 */
  type_guid: string
  /** 条目里的 UTF-16LE 分区名；空串表示名字为空，不是解析失败。 */
  name: string
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
  /** 盘上 GPT 头里的 DiskGUID。只在 source=device 且真的读到 GPT 时出现（不是空串）。 */
  disk_guid?: string | undefined
  /** 盘上条目表（LBA 2 起）。与 partitions 是两个来源，只在真的读到 GPT 时出现。 */
  disk_entries?: StorageGPTEntry[] | undefined
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

// ------------------------------------------------- 设备写动作（P07.5 的页面入口）
//
// 四个动作都走统一的 POST /operations/preflight + POST /jobs，与其它写操作同一条链路：
// 页面只负责拼参数、显示预检结论、把短语原样带回去。**没有**第二个执行入口。
//
// 哪些动作现在就做得成，由设备上的 capabilities 说了算（P07.1 起由判决层投影）：
//   available    = 现在能做
//   blocked      = 动作适合这种设备，但当前状态被拒（受保护/已挂载/被占用……），带 reason
//   unsupported  = 与这种设备类型无关（对整盘格式化、对分区初始化分区表）
// 界面按它决定按钮是否可点，但**放行判断永远在预检**：capabilities 有 30 秒缓存。

export const STORAGE_ACTIONS = {
  initTable: 'storage.partition_table.initialize',
  createPartition: 'storage.partition.create',
  deletePartition: 'storage.partition.delete',
  format: 'storage.filesystem.format',
} as const

export type StorageAction = (typeof STORAGE_ACTIONS)[keyof typeof STORAGE_ACTIONS]

export const STORAGE_ACTION_LABELS: Record<StorageAction, string> = {
  'storage.partition_table.initialize': '初始化 GPT 分区表',
  'storage.partition.create': '新建分区',
  'storage.partition.delete': '删除分区',
  'storage.filesystem.format': '格式化',
}

/**
 * 各动作的动词（与服务端生成确认短语时用的那个一致）。
 *
 * 短语由服务端生成、明文只在预检响应里出现一次，这里只用来写“你会看到类似 INIT XXXXXX
 * 这样的短语”这类说明——界面**不生成**短语。
 */
export const STORAGE_PHRASE_VERBS: Record<StorageAction, string> = {
  'storage.partition_table.initialize': 'INIT',
  'storage.partition.create': '',
  'storage.partition.delete': 'DELETE',
  'storage.filesystem.format': 'FORMAT',
}

/**
 * 这个存储动作要不要 5 分钟内的再认证（**界面提示**；服务端策略仍是唯一权威）。
 *
 * 设备侧四个动作都要：它们改的是盘上的字节。
 * 卷侧另有 api/volumes.ts 的 volumeActionNeedsReauth（导入与挂载不要，其余要）——
 * 两个函数分开是因为两类动作的理由不同，写在一起会让“为什么这个不要密码”看不见。
 * 这里对 `storage.volume.*` 一律返回 false，只为在有人误用时不会多要一次密码。
 */
export function storageActionNeedsReauth(action: string): boolean {
  switch (action) {
    case STORAGE_ACTIONS.initTable:
    case STORAGE_ACTIONS.createPartition:
    case STORAGE_ACTIONS.deletePartition:
    case STORAGE_ACTIONS.format:
      return true
    default:
      return false
  }
}

export const FILESYSTEM_CHOICES = ['ext4', 'xfs'] as const
export type FilesystemChoice = (typeof FILESYSTEM_CHOICES)[number]

/** ext4 的卷标上限是 16 字节，XFS 是 12 字节（CONTRACTS.md 第 6 节，按实际限制分别校验）。 */
export const FILESYSTEM_LABEL_LIMITS: Record<FilesystemChoice, number> = {
  ext4: 16,
  xfs: 12,
}

/** 1 MiB：分区起点与大小都要对齐到它（PLAN.md 第 9.1 节）。 */
export const PARTITION_ALIGN_BYTES = 1_048_576n

export interface InitTableParams {
  deviceId: string
}

export function initTableParams(input: InitTableParams): Record<string, unknown> {
  return { device_id: input.deviceId, table_type: 'gpt' }
}

export interface CreatePartitionParams {
  deviceId: string
  /** 十进制字节串：由页面从“MiB 数”换算而来，不自己算位移。 */
  startBytes: string
  sizeBytes: string
  name?: string
}

export function createPartitionParams(input: CreatePartitionParams): Record<string, unknown> {
  const params: Record<string, unknown> = {
    device_id: input.deviceId,
    start_bytes: input.startBytes,
    size_bytes: input.sizeBytes,
  }
  if (input.name !== undefined && input.name.trim() !== '') params.name = input.name.trim()
  return params
}

export interface DeletePartitionParams {
  deviceId: string
}

export function deletePartitionParams(input: DeletePartitionParams): Record<string, unknown> {
  return { device_id: input.deviceId }
}

export interface FormatParams {
  deviceId: string
  filesystem: FilesystemChoice
  label?: string
}

export function formatParams(input: FormatParams): Record<string, unknown> {
  const params: Record<string, unknown> = {
    device_id: input.deviceId,
    filesystem: input.filesystem,
  }
  if (input.label !== undefined && input.label.trim() !== '') params.label = input.label.trim()
  return params
}

// ------------------------------------------------- 字节与 MiB 的换算

/**
 * 把 MiB 数换成十进制字节串。
 *
 * 为什么不让用户直接填字节：分区起点/大小必须 1 MiB 对齐，让人填 1000000000 再报
 * “不对齐”是无意义的摩擦；MiB 是唯一能填对的口径。返回**字符串**是因为字节量按
 * CONTRACTS.md 第 1 节一律是十进制字符串（2^53 以上用 JSON number 会丢位）。
 */
export function mibToBytes(mib: number): string {
  if (!Number.isInteger(mib) || mib <= 0) return '0'
  return (BigInt(mib) * PARTITION_ALIGN_BYTES).toString()
}

/** 字节串换成 MiB（向下取整）；不是合法字节串时返回 0。 */
export function bytesToMib(bytes: string): number {
  if (!/^[0-9]{1,20}$/.test(bytes)) return 0
  return Number(BigInt(bytes) / PARTITION_ALIGN_BYTES)
}

/** 一个空闲区能切出的最大整数 MiB。 */
export function maxMibInRegion(sizeBytes: string): number {
  return bytesToMib(sizeBytes)
}

export interface PartitionSizeCheck {
  ok: boolean
  /** 不合法时的原因（已经翻译成人话，可直接显示）。 */
  reason?: string
  sizeBytes?: string
}

/**
 * 校验“在某个空闲区里切 sizeMib 大小的分区”。
 *
 * 三条都查：大于 0、不超过空闲区、1 MiB 对齐（按 MiB 输入天然满足，仍然显式验一次，
 * 免得将来换了输入单位就漏掉）。起点用空闲区的 start_bytes——它由探针给出且已对齐。
 */
export function checkPartitionSize(regionSizeBytes: string, sizeMib: number): PartitionSizeCheck {
  if (!Number.isInteger(sizeMib) || sizeMib <= 0) {
    return { ok: false, reason: '大小必须是正整数 MiB' }
  }
  if (!/^[0-9]{1,20}$/.test(regionSizeBytes)) {
    return { ok: false, reason: '空闲区大小不是合法的字节串，无法计算' }
  }
  const size = BigInt(mibToBytes(sizeMib))
  if (size <= 0n) return { ok: false, reason: '大小必须是正整数 MiB' }
  if (size % PARTITION_ALIGN_BYTES !== 0n) {
    return { ok: false, reason: '大小必须按 1 MiB 对齐' }
  }
  if (size > BigInt(regionSizeBytes)) {
    return { ok: false, reason: '超过这个空闲区的可用大小' }
  }
  return { ok: true, sizeBytes: size.toString() }
}

/** 分区名字的上限（CONTRACTS.md 第 6 节：可打印 ASCII、≤36 字节）。 */
export const MAX_PARTITION_NAME_LENGTH = 36

/** 分区名允许的字符：可打印 ASCII，且不含 fstab/sfdisk 会误解的引号与反斜杠。 */
export function isValidPartitionName(name: string): boolean {
  if (name === '') return true
  if (name.length > MAX_PARTITION_NAME_LENGTH) return false
  return /^[\x20-\x21\x23-\x5B\x5D-\x7E]+$/.test(name)
}

/** 卷标长度是否合法（按文件系统的实际上限，不是通用 255）。 */
export function isValidFilesystemLabel(filesystem: FilesystemChoice, label: string): boolean {
  return checkFilesystemLabel(filesystem, label).ok
}

/**
 * 卷标的完整校验：上限按文件系统分别算（ext4 16 字节、XFS 12 字节），
 * 且只允许可打印 ASCII、不含 `/`。
 *
 * 为什么在页面上也算一遍：超限的标签在 mkfs 那里可能 rc=0 然后被**静默截断**
 * （P07.3 的 Q184 就是实测出来的），那种“成功了但名字短了一截”最难查。
 * 页面提前说清楚，服务端仍然独立校验一次。
 */
export function checkFilesystemLabel(
  filesystem: FilesystemChoice,
  label: string,
): { ok: boolean; reason?: string } {
  if (label === '') return { ok: true }
  // 卷标按**字节**算：ext4 与 XFS 都是按字节写进超级块的，不是按字符。
  const bytes = new TextEncoder().encode(label)
  const limit = FILESYSTEM_LABEL_LIMITS[filesystem]
  if (bytes.length > limit) {
    return { ok: false, reason: `${filesystem} 的卷标最多 ${limit} 字节，当前 ${bytes.length} 字节` }
  }
  for (const byte of bytes) {
    if (byte < 0x20 || byte > 0x7e || byte === 0x2f) {
      return { ok: false, reason: `${filesystem} 的卷标只能包含可打印 ASCII 且不能含 /` }
    }
  }
  return { ok: true }
}

/** 取某台设备上某个动作的能力状态；载荷里没有这个动作时返回 undefined（不是 unsupported）。 */
export function capabilityFor(
  device: Pick<StorageDevice, 'capabilities'>,
  action: string,
): StorageCapability | undefined {
  return device.capabilities.find((capability) => capability.action === action)
}

/**
 * 这个动作的按钮该不该画、能不能点。
 *
 * 三种回答分得很清：
 *   hidden   = 服务端说 unsupported（对整盘格式化、对分区初始化分区表），连按钮都不画；
 *   disabled = blocked，按钮画出来但灰着，旁边写服务端给的 reason；
 *   enabled  = available。
 * 载荷里干脆没有这个动作时按 disabled 处理并说明“服务端没报告这个动作”——
 * 那说明代理与前端版本不一致，不该猜成可用。
 */
export type CapabilityGate = 'hidden' | 'disabled' | 'enabled'

export function capabilityGate(
  device: Pick<StorageDevice, 'capabilities'>,
  action: string,
): { gate: CapabilityGate; reason: string | null } {
  const capability = capabilityFor(device, action)
  if (capability === undefined) {
    return { gate: 'disabled', reason: '服务端没有报告这个动作（代理与页面版本可能不一致）' }
  }
  if (capability.state === 'unsupported') return { gate: 'hidden', reason: capability.reason ?? null }
  if (capability.state === 'blocked') return { gate: 'disabled', reason: capability.reason ?? '当前状态下不允许' }
  return { gate: 'enabled', reason: null }
}
