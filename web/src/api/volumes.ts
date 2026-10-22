// 受管理卷的 DTO 与动作参数（P07.4 的读接口 + P07.5 的页面动作）。
//
// 与 api/openapi.yaml 的 StorageVolume / StorageVolumeBlockers / StorageVolumeDependency /
// StorageBusyProcess 逐字对应，与 docs/CONTRACTS.md 第 6 节、第 6.5 节一起看。
//
// 这个模块只做三件事：取数据、拼参数、把状态翻译成人话。判断“能不能做”仍然在服务端：
//   - 卷对象上**没有** capabilities 字段（设备才有），因为一个卷能不能挂载取决于现场
//     （盘在不在、UUID 对不对、谁开着文件），那不是一份 30 秒缓存能说清楚的事；
//   - 所以界面把动作按钮都画出来，点下去先要一份预检，由预检的 blockers 说话。
//     这里提供的 observedStateHint 只是**提示**，不能拿它代替服务端判决。

import { get, type ApiResult } from './client'
import { FILESYSTEM_CHOICES, type StorageCapacity } from './storage'

// ---------------------------------------------------------------- 卷对象

export const VOLUME_OBSERVED_STATES = [
  'mounted',
  'unmounted',
  'missing',
  'mismatch',
  'error',
  'unknown',
] as const

export type VolumeObservedState = (typeof VOLUME_OBSERVED_STATES)[number]

export const VOLUME_OBSERVED_STATE_LABELS: Record<VolumeObservedState, string> = {
  mounted: '已挂载',
  unmounted: '未挂载',
  missing: '找不到设备',
  mismatch: 'UUID 不符',
  error: '读取失败',
  unknown: '未知',
}

export const VOLUME_OBSERVED_STATE_TYPES: Record<
  VolumeObservedState,
  'default' | 'info' | 'success' | 'warning' | 'error'
> = {
  mounted: 'success',
  unmounted: 'default',
  missing: 'warning',
  mismatch: 'error',
  error: 'error',
  unknown: 'warning',
}

export const VOLUME_DESIRED_STATES = ['mounted', 'unmounted'] as const

export type VolumeDesiredState = (typeof VOLUME_DESIRED_STATES)[number]

export const VOLUME_DESIRED_STATE_LABELS: Record<VolumeDesiredState, string> = {
  mounted: '期望挂载',
  unmounted: '期望不挂载',
}

export interface VolumeDependency {
  kind: 'app' | 'share' | 'backup'
  id: string
  name: string
}

export interface Volume {
  id: string
  /** 乐观并发令牌（十进制字符串）。提交动作时必须原样带回。 */
  version: string
  display_name: string
  slug: string
  device_id: string
  filesystem_uuid: string
  filesystem_type: string
  mount_path: string
  automount: boolean
  read_only: boolean
  nodev: boolean
  nosuid: boolean
  desired_state: VolumeDesiredState
  observed_state: VolumeObservedState
  /** 只在真的挂着且读取成功时存在。 */
  capacity: StorageCapacity | null
  /** 依赖它的应用/共享/备份；P07.5 时恒为空数组（不是 null）。 */
  dependencies: VolumeDependency[]
  /** 说明 observed_state 为什么不是 mounted/unmounted；正常时不出现。 */
  observed_error?: string | undefined
  updated_at: string
}

export interface VolumeList {
  volumes: Volume[]
}

export interface BusyProcess {
  /** 十进制字符串，直接展示。 */
  pid: string
  /** /proc/<pid>/comm；读不到时是 "?"。 */
  comm: string
}

export interface VolumeBlockers {
  volume_id: string
  mounted: boolean
  mounts: string[]
  processes: BusyProcess[]
  holders: string[]
  swap_active: boolean
  apps: VolumeDependency[]
  shares: VolumeDependency[]
  backups: VolumeDependency[]
  /** 结论：现在卸载会被谁挡住。挂着但空闲是 false。 */
  busy: boolean
  checked_at: string
}

// ---------------------------------------------------------------- 查询

/** 卷标识与服务端同一个形状：`vol_` + 32 位小写十六进制。 */
export const VOLUME_ID_PATTERN = /^vol_[0-9a-f]{32}$/

export function fetchVolumes(signal?: AbortSignal): Promise<ApiResult<VolumeList>> {
  return get<VolumeList>('/storage/volumes', {
    timeoutMs: 10_000,
    ...(signal === undefined ? {} : { signal }),
  })
}

/**
 * 单个卷。id 形状先在本层挡住：它会进 URL、日志与审计，
 * 带斜杠或大写的字符串永远不该被拼进路径。
 */
export function fetchVolume(id: string, signal?: AbortSignal): Promise<ApiResult<Volume>> {
  if (!VOLUME_ID_PATTERN.test(id)) {
    return Promise.reject(new RangeError(`卷标识不合法：${id}`))
  }
  return get<Volume>(`/storage/volumes/${encodeURIComponent(id)}`, {
    timeoutMs: 10_000,
    ...(signal === undefined ? {} : { signal }),
  })
}

/** 占用详情（admin）。它比卷详情更细：进程 PID 与挂载点。 */
export function fetchVolumeBlockers(
  id: string,
  signal?: AbortSignal,
): Promise<ApiResult<VolumeBlockers>> {
  if (!VOLUME_ID_PATTERN.test(id)) {
    return Promise.reject(new RangeError(`卷标识不合法：${id}`))
  }
  return get<VolumeBlockers>(`/storage/volumes/${encodeURIComponent(id)}/blockers`, {
    timeoutMs: 10_000,
    ...(signal === undefined ? {} : { signal }),
  })
}

// ---------------------------------------------------------------- 动作

/**
 * 卷的五个写动作。
 *
 * 全部 `cancellable=false`，全部**不要确认短语**：短语只留给“写下去就找不回来”的动作
 * （初始化分区表、格式化、删分区）。导入只是登记 + 写一行 fstab，卸载随时能挂回去，
 * 配置与退出管理都有备份可回滚——用短语只会训练人闭眼确认。
 */
export const VOLUME_ACTIONS = {
  import: 'storage.volume.import',
  mount: 'storage.volume.mount',
  unmount: 'storage.volume.unmount',
  configure: 'storage.volume.configure',
  forget: 'storage.volume.forget',
} as const

export type VolumeAction = (typeof VOLUME_ACTIONS)[keyof typeof VOLUME_ACTIONS]

export const VOLUME_ACTION_LABELS: Record<VolumeAction, string> = {
  'storage.volume.import': '纳入管理',
  'storage.volume.mount': '挂载',
  'storage.volume.unmount': '卸载',
  'storage.volume.configure': '修改挂载选项',
  'storage.volume.forget': '退出管理',
}

/**
 * 哪些动作需要 5 分钟内的再认证（CONTRACTS.md 第 6.5 节）。
 *
 * 这张表是**界面提示**：真正拦人的是服务端策略（403 REAUTH_REQUIRED）。
 * 写成常量是为了在点下去之前就把“要输密码”说清楚，而不是点完才报错。
 */
export function volumeActionNeedsReauth(action: VolumeAction): boolean {
  switch (action) {
    case VOLUME_ACTIONS.import:
    case VOLUME_ACTIONS.mount:
      return false
    case VOLUME_ACTIONS.unmount:
    case VOLUME_ACTIONS.configure:
    case VOLUME_ACTIONS.forget:
      return true
  }
}

/** slug 决定挂载点，只允许小写字母、数字与中划线。 */
export const SLUG_PATTERN = /^[a-z0-9]+(-[a-z0-9]+)*$/
export const MAX_SLUG_LENGTH = 32
export const MAX_DISPLAY_NAME_LENGTH = 64

/**
 * 把用户输入规范化成合法 slug：小写、空格与下划线转中划线、去掉两端中划线。
 *
 * **不做“猜意图”的修补**：规范化之后仍然不合法（中文、斜杠、连续中划线以外的符号）
 * 就返回空串，由调用方明确报错。静默替换字符比报错更糟——挂载点是从它派生的。
 */
export function normalizeSlug(raw: string): string {
  const lowered = raw.trim().toLowerCase().replace(/[\s_]+/g, '-').replace(/^-+|-+$/g, '')
  return lowered
}

export function isValidSlug(raw: string): boolean {
  return raw.length > 0 && raw.length <= MAX_SLUG_LENGTH && SLUG_PATTERN.test(raw)
}

/** 挂载点由 slug 派生，页面不提供“自己填路径”的入口。 */
export const VOLUME_ROOT = '/srv/upanel/volumes'

export function mountPathForSlug(slug: string): string {
  return `${VOLUME_ROOT}/${slug}`
}

// ---------------------------------------------------------------- 参数

export interface ImportActionParams {
  deviceId: string
  slug: string
  displayName?: string
}

export function importParams(input: ImportActionParams): Record<string, unknown> {
  const params: Record<string, unknown> = {
    device_id: input.deviceId,
    slug: input.slug,
  }
  // display_name 可空：空就交给服务端用 slug 补，不在这里编一个“未命名卷”。
  if (input.displayName !== undefined && input.displayName.trim() !== '') {
    params.display_name = input.displayName.trim()
  }
  return params
}

/** 挂载/卸载/退出管理的参数相同：卷标识 + 页面看到的那份版本。 */
export function volumeVersionParams(volume: Pick<Volume, 'id' | 'version'>): Record<string, unknown> {
  return { volume_id: volume.id, version: volume.version }
}

export const mountParams = volumeVersionParams
export const unmountParams = volumeVersionParams
export const forgetParams = volumeVersionParams

export interface ConfigureActionParams {
  volume: Pick<Volume, 'id' | 'version'>
  /** 只传要改的项：缺省字段服务端按“不改”处理（不静默关掉一项加固）。 */
  automount?: boolean
  readOnly?: boolean
  nodev?: boolean
  nosuid?: boolean
}

/**
 * 配置卷。四个开关**只传真的改了的**：服务端的参数用指针区分“缺省”与“显式 false”，
 * 页面这边漏传一个字段就等于“不动它”，这是唯一安全的默认。
 */
export function configureParams(input: ConfigureActionParams): Record<string, unknown> {
  const params: Record<string, unknown> = {
    volume_id: input.volume.id,
    version: input.volume.version,
  }
  if (input.automount !== undefined) params.automount = input.automount
  if (input.readOnly !== undefined) params.read_only = input.readOnly
  if (input.nodev !== undefined) params.nodev = input.nodev
  if (input.nosuid !== undefined) params.nosuid = input.nosuid
  return params
}

/** 从“当前卷”算出这次要提交的差异；没有任何变化时返回空对象。 */
export function configureDiff(
  volume: Pick<Volume, 'automount' | 'read_only' | 'nodev' | 'nosuid'>,
  next: { automount: boolean; readOnly: boolean; nodev: boolean; nosuid: boolean },
): { automount?: boolean; readOnly?: boolean; nodev?: boolean; nosuid?: boolean } {
  const diff: {
    automount?: boolean
    readOnly?: boolean
    nodev?: boolean
    nosuid?: boolean
  } = {}
  if (next.automount !== volume.automount) diff.automount = next.automount
  if (next.readOnly !== volume.read_only) diff.readOnly = next.readOnly
  if (next.nodev !== volume.nodev) diff.nodev = next.nodev
  if (next.nosuid !== volume.nosuid) diff.nosuid = next.nosuid
  return diff
}

// ---------------------------------------------------------------- 提示（不是判决）

/**
 * 这个卷当前状态下最该提醒的一句话；没有特别要提醒的返回 null。
 *
 * 只是**提示**：真正的拦截在预检。这里不返回“能不能挂载”的布尔值，
 * 因为那会变成第二套判决，和预检打架时页面就不知道该信谁。
 */
export function observedStateHint(volume: Volume): string | null {
  switch (volume.observed_state) {
    case 'missing':
      return '这块盘现在不在这台机器上（设备没插、或分区被删了）。挂载会失败，先确认盘在。'
    case 'mismatch':
      return '盘上的 UUID 与登记的不一致：这块设备已经被别的东西占用了，面板不会拿它当原来的卷用。'
    case 'error':
      return volume.observed_error ?? '读取这块盘的状态失败，原因见详情。'
    case 'unknown':
      return '还没确认这块盘的状态（刚启动或刚导入），稍等一次扫描。'
    case 'mounted':
    case 'unmounted':
      return null
  }
}

/** 卷的期望状态与实际状态不一致？那正是页面该显眼提示的事。 */
export function desiredMismatch(volume: Volume): boolean {
  if (volume.desired_state === 'mounted') {
    return volume.observed_state !== 'mounted'
  }
  return volume.observed_state === 'mounted'
}

/** 一行挂载选项，供页面与确认框显示（顺序与服务端渲染 fstab 时一致）。 */
export function volumeOptions(volume: Volume): string[] {
  const options: string[] = [volume.read_only ? 'ro' : 'rw', 'nofail', 'x-systemd.device-timeout=30']
  if (volume.nodev) options.push('nodev')
  if (volume.nosuid) options.push('nosuid')
  if (!volume.automount) options.push('noauto')
  return options
}

/** 占用里的人话摘要：谁在用、几个。 */
export function busySummary(blockers: VolumeBlockers): string {
  const parts: string[] = []
  if (blockers.processes.length > 0) {
    parts.push(`${blockers.processes.length} 个进程打开着它`)
  }
  if (blockers.holders.length > 0) parts.push(`被 ${blockers.holders.join('、')} 依赖`)
  if (blockers.swap_active) parts.push('它是活动交换区')
  const deps = blockers.apps.length + blockers.shares.length + blockers.backups.length
  if (deps > 0) parts.push(`${deps} 个面板侧依赖`)
  if (parts.length === 0) return blockers.busy ? '判定为忙，但没有列出具体占用者' : '没有占用'
  return parts.join('，')
}

// ---------------------------------------------------------------- 候选设备

/** 纳入管理时需要看的设备字段（不依赖整个 StorageDevice，测试可以只造这一小块）。 */
export interface ImportCandidateDevice {
  id: string
  kind: string
  size_bytes: string
  filesystem: {
    type: string | null
    uuid: string | null
    label?: string | null
    mounts: readonly unknown[]
  } | null
  protection?: { system_disk: boolean } | undefined
}

/**
 * 能被“纳入管理”的设备：已经分好区、没被保护、没挂在别处。
 *
 * 这是**界面筛选**，不是放行判断：服务端会自己再算一遍（而且它才是权威）。
 * 之所以还要筛：把 /dev/sda1（系统盘）画进下拉框里，是让人有机会点错。
 *
 * 这里**不要求知道文件系统类型**，理由写在 CONTRACTS 第 6.1 节（第 231 行）：未挂载分区的
 * `filesystem.type` 恒为 null（产品不调用 blkid、不靠猜）。第一版筛掉了
 * `filesystem === null` 与 `type === null`，于是候选**永远是空的**——没挂载的分区类型
 * 读不到，而挂着的分区又按“有人手工挂过”被排除，两条加起来一个都不剩。
 * P07.5 真机上就是这么空着的（DECISIONS Q208）。
 *
 * 现在的判据分三层：
 *   - 类型**已知且不是面板管的那两种** ⇒ 排除（这种分区导入进来也管不了，没必要列）；
 *   - 类型未知 ⇒ 仍然列出，界面上如实写“未挂载时读不到类型”，由预检在执行前直读设备说话；
 *   - 已挂载 ⇒ 排除（导入会多出一条挂载路径，先让人自己看清情况）。
 */
export function importCandidates<T extends ImportCandidateDevice>(devices: readonly T[]): T[] {
  return devices.filter((device) => {
    if (device.kind !== 'partition') return false
    if (device.protection?.system_disk === true) return false
    if (device.filesystem !== null && device.filesystem.mounts.length > 0) return false
    const type = device.filesystem?.type ?? null
    if (type !== null && !isManagedFilesystem(type)) return false
    return true
  })
}

/** 面板能管理的文件系统类型（与 `FILESYSTEM_CHOICES` 同一份）。 */
export function isManagedFilesystem(type: string): boolean {
  return (FILESYSTEM_CHOICES as readonly string[]).includes(type)
}

/**
 * 候选下拉框里那行字：设备名、文件系统、容量。
 *
 * 类型未知时**不写 `?` 也不猜**——写清“未挂载时页面读不到”，因为下一步真由服务端读盘。
 */
export function candidateFilesystemLabel(device: ImportCandidateDevice): string {
  const type = device.filesystem?.type ?? null
  const label = device.filesystem?.label ?? null
  if (type === null) return '文件系统未挂载时读不到'
  return label === null || label === '' ? type : `${type} · ${label}`
}
