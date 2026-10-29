// 把一次存储写任务的 result 翻译成几行“到底做了什么、凭什么说成功了”。
//
// 为什么单独一个模块：存储动作的结果里最值钱的不是 status=succeeded，而是
// `result.observed_state` 里的**第二份证据**——写回 fstab 的那一行原文、备份文件路径、
// `checked_via`（用哪两路读回来的）、挂载后 findmnt 读到的 source/options。
// 这些字段在五个卷动作与四个设备动作之间形状不同，但读法一致，所以集中一处解析，
// 页面各处（确认框、任务详情）用同一份翻译，不会出现“这里说有备份、那里说没验证”。
//
// 全部按“读不到就不说”处理：字段缺失时不编造值，也不显示 `undefined`。

import type { JobDTO, JobStatus } from '@/api/jobs'

export interface JobOutcomeSummary {
  /** 任务是否成功（只有 succeeded 算成功；interrupted 是“不知道”，不算）。 */
  ok: boolean
  /** 一句话结论。 */
  headline: string
  /** 支撑这句话的证据行，顺序固定。 */
  lines: string[]
  /** 失败时服务端给的建议（recovery_hint），没有就不显示。 */
  hint: string | null
}

function asRecord(value: unknown): Record<string, unknown> | null {
  if (value === null || typeof value !== 'object' || Array.isArray(value)) return null
  return value as Record<string, unknown>
}

function readString(record: Record<string, unknown> | null, key: string): string | null {
  if (record === null) return null
  const value = record[key]
  return typeof value === 'string' && value !== '' ? value : null
}

function readBool(record: Record<string, unknown> | null, key: string): boolean | null {
  if (record === null) return null
  const value = record[key]
  return typeof value === 'boolean' ? value : null
}

function readStringList(record: Record<string, unknown> | null, key: string): string[] {
  if (record === null) return []
  const value = record[key]
  if (!Array.isArray(value)) return []
  return value.filter((item): item is string => typeof item === 'string' && item !== '')
}

/**
 * 取第一个非空的列表。
 *
 * 不能写成 `readStringList(a, k) ?? readStringList(b, k2)`：readStringList 永远返回数组，
 * 空数组不是 null，`??` 就**永远不会**走到第二个来源——字段只在 data 里给时那一行会凭空消失。
 * 这个坑是 NFS 用例抓出来的（`startup_paths` 只在 data 里，`exports` 与 `exported_paths` 不同名）。
 */
function firstList(...lists: string[][]): string[] {
  for (const list of lists) {
    if (list.length > 0) return list
  }
  return []
}

/** 核验来源的中文说法（与服务端 checked_via 的取值一一对应）。 */
const CHECKED_VIA_LABELS: Record<string, string> = {
  fstab_reread: '重新读 /etc/fstab 的托管块',
  systemctl_show: 'systemctl show 读回挂载单元',
  findmnt: 'findmnt 读回真实挂载',
  topology_scan: '重新扫描块设备拓扑',
  superblock: '直读文件系统超级块',
  signature_probe: '重新探测设备签名',
  disk_bytes: '直读盘上的分区表字节',
  kernel_sysfs: '读内核 sysfs 的分区视图',
}

export function checkedViaLabel(raw: string): string {
  return CHECKED_VIA_LABELS[raw] ?? raw
}

const OUTCOME_LABELS: Record<string, string> = {
  verified: '两源核验一致',
  unverified: '执行完成但没有通过核验',
}

/**
 * 汇总一个任务的结果。
 *
 * 只认 `result.observed_state` 与 `result.data` 两块：前者的语义是“执行完再读一遍看到什么”，
 * 后者是“这次做了什么”。两者都缺时如实说“没有返回值”，不把空对象说成成功。
 */
export function summarizeStorageJob(job: Pick<JobDTO, 'status' | 'result' | 'error'>): JobOutcomeSummary {
  const status: JobStatus = job.status
  const error = asRecord(job.error)
  const errorMessage = readString(error, 'message')
  const hint = readString(error, 'recovery_hint')

  if (status !== 'succeeded') {
    const headline =
      status === 'interrupted'
        ? '任务被中断，结果未知：不要当作成功，也不要在没确认现场前重复提交'
        : status === 'canceled'
          ? '任务已取消'
          : '任务失败'
    return {
      ok: false,
      headline,
      lines: errorMessage === null ? [] : [errorMessage],
      hint,
    }
  }

  const result = asRecord(job.result) ?? {}
  const observed = asRecord(result['observed_state'])
  const data = asRecord(result['data'])
  const lines: string[] = []

  const outcome = readString(result, 'outcome')
  if (outcome !== null) lines.push(OUTCOME_LABELS[outcome] ?? `服务端结论：${outcome}`)

  // 1) 写进 fstab 的那一行原文，或“fstab 里已经没有这条”。
  const fstabEntry = readString(observed, 'fstab_entry')
  if (fstabEntry !== null) {
    lines.push(`fstab 托管块里的这一行：${fstabEntry}`)
  } else if (readBool(observed, 'fstab_entry') === false) {
    lines.push('fstab 托管块里已经没有这个卷的条目')
  }

  // 2) 现场读回的挂载事实。
  const mounted = readBool(observed, 'mounted')
  if (mounted !== null) {
    const mountPath = readString(observed, 'mount_path')
    const source = readString(observed, 'source')
    const options = readString(observed, 'options')
    const where = mountPath === null ? '' : `（${mountPath}）`
    lines.push(
      mounted
        ? `现在挂着${where}${source === null ? '' : `，来源 ${source}`}${options === null ? '' : `，选项 ${options}`}`
        : `现在没有挂载${where}`,
    )
  }

  const registered = readBool(observed, 'registered')
  if (registered !== null) {
    lines.push(registered ? '登记记录仍在' : '登记记录已移除（卷本身和里面的数据没动）')
  }

  // 3) 设备侧：分区表与新建的分区。
  const tableType = readString(observed, 'table_type')
  if (tableType !== null) {
    const kernel = readStringList(observed, 'kernel_partitions')
    lines.push(
      `盘上的分区表：${tableType}${kernel.length === 0 ? '（内核没看到任何分区）' : `，内核看到 ${kernel.join('、')}`}`,
    )
  }
  const diskGuid = readString(observed, 'disk_guid')
  if (diskGuid !== null) lines.push(`盘上的 DiskGUID：${diskGuid}`)
  const created = asRecord(data?.['created_partition'])
  if (created !== null) {
    const deviceID = readString(created, 'device_id') ?? '?'
    const start = readString(created, 'start_bytes')
    const size = readString(created, 'size_bytes')
    const name = readString(created, 'name')
    const parts = [`新建分区 ${deviceID}`]
    if (start !== null) parts.push(`起点 ${start} 字节`)
    if (size !== null) parts.push(`大小 ${size} 字节`)
    if (name !== null) parts.push(`名字 ${name}`)
    lines.push(parts.join('，'))
  }

  // 4) 文件系统。
  const filesystem = readString(observed, 'filesystem') ?? readString(data, 'filesystem')
  const uuid = readString(observed, 'uuid') ?? readString(data, 'uuid')
  const label = readString(data, 'label')
  if (filesystem !== null || uuid !== null) {
    const parts: string[] = []
    if (filesystem !== null) parts.push(filesystem)
    if (uuid !== null) parts.push(`UUID ${uuid}`)
    if (label !== null) parts.push(`卷标 ${label}`)
    lines.push(`文件系统：${parts.join('，')}`)
  }
  const nodePresent = readBool(observed, 'node_present')
  if (nodePresent === false) {
    lines.push('设备节点现在不在 /dev 下（可能已被移除）')
  }

  // 5) 备份与核验来源——这两样是“能回滚”的依据，必须写在结果里。
  const backup = readString(observed, 'backup') ?? readString(data, 'fstab_backup')
  if (backup !== null) lines.push(`改动前的 fstab 备份：${backup}`)
  const checkedVia = readStringList(observed, 'checked_via')
  if (checkedVia.length > 0) {
    lines.push(`核验来源：${checkedVia.map(checkedViaLabel).join(' + ')}`)
  }
  const command = readStringList(data, 'command')
  if (command.length > 0) lines.push(`执行的外部命令：${command.join(' ')}`)

  const headline =
    lines.length === 0
      ? '任务已成功，但代理没有返回可展示的现场读数；请到设备页刷新确认'
      : '任务已成功，下面是执行后的现场读数'
  return { ok: true, headline, lines, hint }
}

// ---------------------------------------------------------------- 共享动作

/**
 * 共享动作的结果翻译（P08.4）。
 *
 * 与存储的区别在这一段最要紧：存储动作的成功是“盘上真的变了”，共享动作的成功可以是
 * `accepted`——配置写进去了，但 `smbd` 没在跑，或者卷此刻没挂，于是它**还没来得及对外
 * 提供服务**。所以这里不把 `accepted` 说成“成功”，而是把三件事分开摆：
 *   - 配置这一侧做了什么（写入/未变、include 装没装、重载结果、服务状态）；
 *   - `served` 到底是不是 true（只有它能说明“此刻在提供访问”）；
 *   - 开机依赖写没写（drop-in）。
 */
const SHARE_CONFIG_LABELS: Record<string, string> = {
  written: '托管配置已整份重写',
  unchanged: '内容没变，没有重写也没有重载',
  not_written: '配置没有写入',
}
const SHARE_INCLUDE_LABELS: Record<string, string> = {
  installed: 'include 已装进 smb.conf',
  present: 'include 本来就在',
  missing: 'smb.conf 里没有 include',
}
const SHARE_RELOAD_LABELS: Record<string, string> = {
  done: '已让 Samba 重载配置',
  skipped_service_inactive: 'Samba 没在运行，因此没有重载',
  skipped_unchanged: '内容没变，跳过了重载',
  failed: '重载失败',
}
const SHARE_STARTUP_LABELS: Record<string, string> = {
  written: '开机依赖已写入 smbd 单元',
  unchanged: '开机依赖没有变化',
  removed: '开机依赖已随最后一条共享一起收回',
  failed: '开机依赖写入失败',
  skipped_no_service: '没找到 smbd 单元，开机依赖没写',
}

/**
 * NFS 那一半的措辞（P14.4）。
 *
 * 单独一套而不是复用 SMB 的：两边的字段名不同（`exports_file` vs `samba_config`），
 * 动的文件不同（upanel.exports vs smb.conf），重载的对象也不同（exportfs vs smbd）。
 * 拿 SMB 的句子去解释一次 NFS 应用，会让人以为“Samba 那边也被改了”。
 */
const EXPORTS_FILE_LABELS: Record<string, string> = {
  written: '托管导出文件已整份重写（只含面板自己那几条）',
  unchanged: '导出文件内容没变，没有重写',
  not_written: '导出文件没有写入',
}
const EXPORTS_RELOAD_LABELS: Record<string, string> = {
  done: '已让 exportfs 重新读一遍导出表',
  skipped_service_inactive: 'nfs-server 没在运行，因此没有刷表',
  skipped_unchanged: '内容没变，跳过了刷表',
  failed: '刷表失败',
}
const EXPORTS_STARTUP_LABELS: Record<string, string> = {
  written: '开机依赖已写入 nfs-server 单元',
  unchanged: '开机依赖没有变化',
  removed: '开机依赖已随最后一条 NFS 导出一起收回',
  failed: '开机依赖写入失败',
  skipped_no_service: '没找到 nfs-server 单元，开机依赖没写',
}

/** 汇总一次共享/账号动作的结果。 */
export function summarizeShareJob(job: Pick<JobDTO, 'status' | 'result' | 'error'>): JobOutcomeSummary {
  const error = asRecord(job.error)
  const errorMessage = readString(error, 'message')
  const hint = readString(error, 'recovery_hint')

  if (job.status !== 'succeeded') {
    const headline =
      job.status === 'interrupted'
        ? '任务被中断，结果未知：配置可能写了一半，先去共享页看现在的状态再决定'
        : job.status === 'canceled'
          ? '任务已取消'
          : '任务失败'
    return { ok: false, headline, lines: errorMessage === null ? [] : [errorMessage], hint }
  }

  const result = asRecord(job.result) ?? {}
  const observed = asRecord(result['observed_state'])
  const data = asRecord(result['data'])
  const lines: string[] = []

  const outcome = readString(result, 'outcome')
  if (outcome !== null) lines.push(OUTCOME_LABELS[outcome] ?? `服务端结论：${outcome}`)

  // NFS 与 SMB 的结果字段名刻意不同（`exports_file` vs `samba_config`），所以“这是
  // 哪一边”能从字段本身读出来，不需要调用方传协议进来。
  const exportsFile = readString(observed, 'exports_file') ?? readString(data, 'exports_file')
  const isNFS = exportsFile !== null || readString(data, 'protocol') === 'nfs'

  if (isNFS) {
    if (exportsFile !== null) {
      lines.push(EXPORTS_FILE_LABELS[exportsFile] ?? `导出文件：${exportsFile}`)
    }
    const reloadNFS = readString(observed, 'reload') ?? readString(data, 'reload')
    if (reloadNFS !== null) lines.push(EXPORTS_RELOAD_LABELS[reloadNFS] ?? `刷表：${reloadNFS}`)
    const serviceNFS = readString(observed, 'service') ?? readString(data, 'service')
    if (serviceNFS !== null) lines.push(`nfs-server 此刻：${serviceNFS}`)
    const servedNFS = readBool(observed, 'served') ?? readBool(data, 'served')
    if (servedNFS !== null) {
      lines.push(
        servedNFS
          ? '这条导出此刻在提供服务（导出文件在、exportfs 读回的表里有它、nfs-server 在跑、卷仍挂载）'
          : '这条导出此刻没有在提供服务（导出表、服务或卷有一项没就位）',
      )
    }
    const startupNFS = readString(observed, 'startup_deps') ?? readString(data, 'startup_deps')
    if (startupNFS !== null) lines.push(EXPORTS_STARTUP_LABELS[startupNFS] ?? `开机依赖：${startupNFS}`)
    const startupPathsNFS = firstList(
      readStringList(observed, 'startup_paths'),
      readStringList(data, 'startup_paths'),
    )
    if (startupPathsNFS.length > 0) lines.push(`开机前必须先就位：${startupPathsNFS.join('、')}`)
    const managedExports = readString(data, 'managed_exports')
    if (managedExports !== null) lines.push(`托管导出文件：${managedExports}`)
    const exportedPaths = firstList(
      readStringList(observed, 'exported_paths'),
      readStringList(data, 'exports'),
    )
    if (exportedPaths.length > 0) {
      lines.push(`现在导出表里由面板写着的路径：${exportedPaths.join('、')}`)
    }
    const offlineNFS = firstList(
      readStringList(observed, 'volume_offline_shares'),
      readStringList(data, 'volume_offline'),
    )
    if (offlineNFS.length > 0) {
      lines.push(`因为卷没挂而被跳过、这次没有写进导出表的共享：${offlineNFS.join('、')}`)
    }
    const exportPreview = readString(data, 'export_preview')
    if (exportPreview !== null) lines.push(`这一次的导出行：${exportPreview}`)
    const aclNote = readString(observed, 'acl_note')
    if (aclNote !== null) lines.push(aclNote)
    const hashNFS = readString(data, 'config_hash') ?? readString(observed, 'config_hash')
    if (hashNFS !== null) lines.push(`这一版导出的哈希：${hashNFS.slice(0, 16)}…`)
    const headlineNFS =
      lines.length === 0
        ? '任务已成功，但代理没有返回可展示的现场读数；请刷新共享列表确认'
        : '任务已成功，下面是执行后的现场读数'
    return { ok: true, headline: headlineNFS, lines, hint }
  }

  const sambaConfig = readString(observed, 'samba_config') ?? readString(data, 'samba_config')
  if (sambaConfig !== null) lines.push(SHARE_CONFIG_LABELS[sambaConfig] ?? `配置写入：${sambaConfig}`)
  const include = readString(observed, 'include') ?? readString(data, 'include')
  if (include !== null) lines.push(SHARE_INCLUDE_LABELS[include] ?? `include：${include}`)
  const reload = readString(observed, 'reload') ?? readString(data, 'reload')
  if (reload !== null) lines.push(SHARE_RELOAD_LABELS[reload] ?? `重载：${reload}`)
  const service = readString(observed, 'service') ?? readString(data, 'service')
  if (service !== null) lines.push(`smbd 此刻：${service}`)

  const served = readBool(observed, 'served') ?? readBool(data, 'served')
  if (served !== null) {
    lines.push(
      served
        ? '这份共享此刻在提供服务（配置在、include 在、smbd 在跑、卷仍挂载）'
        : '这份共享此刻没有在提供服务（配置或服务有一项没就位）',
    )
  }

  const startup = readString(observed, 'startup_deps') ?? readString(data, 'startup_deps')
  if (startup !== null) lines.push(SHARE_STARTUP_LABELS[startup] ?? `开机依赖：${startup}`)
  const startupPaths = firstList(
    readStringList(observed, 'startup_paths'),
    readStringList(data, 'startup_paths'),
  )
  if (startupPaths.length > 0) lines.push(`开机前必须先就位：${startupPaths.join('、')}`)

  const offlineShares = firstList(
    readStringList(observed, 'volume_offline_shares'),
    readStringList(data, 'volume_offline_shares'),
  )
  if (offlineShares.length > 0) {
    lines.push(`因为卷没挂而被跳过、没有重新渲染的共享：${offlineShares.join('、')}`)
  }

  const configHash = readString(data, 'config_hash') ?? readString(observed, 'config_hash')
  if (configHash !== null) lines.push(`这一版配置的哈希：${configHash.slice(0, 16)}…`)
  const managedConfig = readString(data, 'managed_config')
  if (managedConfig !== null) lines.push(`托管配置文件：${managedConfig}`)

  // 账号侧：三条独立读回的结果，与 P08.1 的核验口径一致。
  const name = readString(observed, 'name')
  if (name !== null) lines.push(`账号 ${name}：uid ${String(observed?.['uid'] ?? '?')}，shell ${readString(observed, 'shell') ?? '?'}`)
  const linuxPassword = readString(observed, 'linux_password')
  if (linuxPassword !== null) lines.push(`Linux 侧口令状态：${linuxPassword}（不是 NOPASSWD）`)
  const sambaAccount = readBool(observed, 'samba_account')
  if (sambaAccount !== null) lines.push(sambaAccount ? 'Samba passdb 里能查到这条账号' : 'Samba passdb 里查不到这条账号')
  const passwordSet = readBool(observed, 'password_set')
  if (passwordSet !== null) {
    const at = readString(observed, 'password_last_set')
    lines.push(passwordSet ? `密码已设置${at === null ? '' : `（${at}）`}` : '没有设置密码')
  }
  const secretPersisted = readBool(observed, 'secret_persisted')
  if (secretPersisted === false) {
    const note = readString(observed, 'secret_note')
    lines.push(`密码密文没能转成持久秘密，10 分钟后会销毁${note === null ? '' : `：${note}`}`)
  }

  const aclRevoked = readBool(observed, 'acl_revoked')
  if (aclRevoked !== null) {
    lines.push(aclRevoked ? '目录 ACL 已清空（目录与文件都还在）' : '目录 ACL 没有变化')
  }

  const headline =
    lines.length === 0
      ? '任务已成功，但代理没有返回可展示的现场读数；请刷新共享列表确认'
      : '任务已成功，下面是执行后的现场读数'
  return { ok: true, headline, lines, hint }
}

// ---------------------------------------------------------------- 网络写（P14.2）

const NETWORK_OUTCOME_LABELS: Record<string, string> = {
  verified: '读回内核事实：新配置确实生效了',
  rolled_back: '已经退回改之前的样子',
  unknown: '不知道现在是什么状态',
}

const NETWORK_MODE_LABELS: Record<string, string> = {
  dhcp: 'DHCP',
  static: '静态地址',
}

/**
 * 一次网络写的结果翻译（P14.2）。
 *
 * 三条规矩，每一条都对应"这句话说错了会让人做错事"：
 *
 *  1. **`outcome` 决定结论，不是 `status`。** `rolled_back` 的任务 status 也是
 *     succeeded——它确实跑完了，只是改动没留下。把它渲染成绿色的"成功"，人就以为
 *     新地址生效了；渲染成红色的"失败"，人又会以为机器现在坏了。两句话都不对。
 *  2. **`unknown` 不说成失败也不说成成功。** 那意味着"我们不知道现在是什么样"，
 *     唯一正确的下一步是去看一眼现场，所以 headine 直接说这句话，并带上
 *     recovery_hint。
 *  3. **确认/回滚这两个动作没有"改了什么"**：它们一个停 timer、一个把配置退回去。
 *     所以按存在性读字段，不按动作名分叉——分叉会随着动作变多而烂掉。
 */
export function summarizeNetworkJob(job: Pick<JobDTO, 'status' | 'result' | 'error'>): JobOutcomeSummary {
  const error = asRecord(job.error)
  const errorMessage = readString(error, 'message')
  const hint = readString(error, 'recovery_hint')

  if (job.status !== 'succeeded') {
    const headline =
      job.status === 'interrupted'
        ? '任务被中断，结果未知：网络可能改了一半，先确认自己还能不能连上这台机器'
        : job.status === 'canceled'
          ? '任务已取消（网络动作不可取消，走到这一步说明是被外部打断的）'
          : '任务失败'
    return { ok: false, headline, lines: errorMessage === null ? [] : [errorMessage], hint }
  }

  const result = asRecord(job.result) ?? {}
  const observed = asRecord(result['observed_state'])
  const data = asRecord(result['data'])
  const lines: string[] = []

  const outcome = readString(result, 'outcome')
  if (outcome !== null) lines.push(NETWORK_OUTCOME_LABELS[outcome] ?? `服务端结论：${outcome}`)

  const iface = readString(observed, 'interface')
  const mode = readString(observed, 'mode')
  const address = readString(observed, 'address')
  if (iface !== null) {
    const modeText = mode === null ? '' : `，模式 ${NETWORK_MODE_LABELS[mode] ?? mode}`
    const addressText = address === null ? '' : `，地址 ${address}`
    lines.push(`${iface}${modeText}${addressText}`)
  }

  // 回滚记录的状态：确认与回滚两个动作都只给这一个字段。
  const state = readString(observed, 'state') ?? readString(data, 'state')
  const stateLabels: Record<string, string> = {
    pending: '记录仍是待确认',
    confirmed: '记录已确认：回滚 timer 已停，新配置保留',
    rolled_back: '记录已标为已回滚',
    failed: '记录已标为失败：回滚没走完，需要到现场处理',
  }
  if (state !== null) lines.push(stateLabels[state] ?? `记录状态：${state}`)

  // 退回的是什么：静态配置时有旧地址，DHCP 时说"改之前那份配置"。
  const previous = readString(data, 'previous')
  if (previous !== null) lines.push(`改之前的样子：${previous}`)

  const windowSeconds = data?.['window_seconds']
  if (typeof windowSeconds === 'number') {
    lines.push(`回滚窗口：${windowSeconds} 秒内不确认就自动退回`)
  }

  const deadline = readString(observed, 'deadline') ?? readString(data, 'deadline')
  if (deadline !== null) lines.push(`到期时刻：${deadline}`)

  const token = readString(data, 'rollback_token') ?? readString(observed, 'rollback_token')
  if (token !== null) lines.push(`回滚标识：${token}`)

  const configHash = readString(data, 'config_hash_now')
  if (configHash !== null) lines.push(`改动之后的配置指纹：${configHash.slice(0, 16)}…`)

  // 失败路径留下的原因：netplan generate 失败、回滚装不上、回滚自己失败……
  for (const [key, label] of [
    ['netplan_generate', 'netplan generate'],
    ['netplan_apply', 'netplan apply'],
    ['verify_error', '核验'],
    ['arm_error', '回滚 timer'],
    ['rollback_error', '回滚'],
    ['restore_error', '恢复面板文件'],
  ] as const) {
    const value = readString(observed, key)
    if (value !== null && value !== '') lines.push(`${label}：${value}`)
  }

  const rollbackState = readString(observed, 'rollback_state')
  if (rollbackState !== null) {
    lines.push(stateLabels[rollbackState] ?? `回滚记录状态：${rollbackState}`)
  }

  let headline: string
  let ok: boolean
  if (outcome === 'rolled_back') {
    ok = false
    headline = '改动没有留在机器上：已按记录退回改之前的样子'
  } else if (outcome === 'unknown') {
    ok = false
    headline = '执行完了但结果未知：现在这台机器的网络状态没有被验证过，先去看一眼现场'
  } else {
    ok = true
    headline =
      lines.length === 0
        ? '任务已成功，但代理没有返回可展示的现场读数；请刷新网络页确认'
        : '任务已成功，下面是读回来的现场事实'
  }
  return { ok, headline, lines, hint }
}

// ---------------------------------------------------------------- 防火墙写（P14.3）

const FIREWALL_OUTCOME_LABELS: Record<string, string> = {
  verified: '读回 ufw 的实际规则集：与提交的定义一致',
  rolled_back: '已经退回改之前的样子',
  unknown: '不知道现在是什么状态',
}

/**
 * 一次防火墙写的结果翻译（P14.3）。
 *
 * 与 `summarizeNetworkJob` 同一个骨架（`outcome` 决定结论、`unknown` 不说成成功也不
 * 说成失败），但**证据行不同**：防火墙那边最值钱的读数是"ufw 现在到底有哪些规则"，
 * 不是地址。所以这里把 `observed_state.managed_rules` 逐条摊开——`ufw show added`
 * 的原话（`allow 443/tcp # upanel:web`）。把规则集只报个条数，人就得自己再开一个终端
 * 去核对；而这次改动最怕的恰恰是"多了一条不该有的"或"少了一条该有的"。
 *
 * 规则集超过 8 条时只说条数与前 8 条：剩下的那几十行会把上面那句结论挤出屏幕，
 * 而结论才是这一屏要回答的问题。
 */
export function summarizeFirewallJob(
  job: Pick<JobDTO, 'status' | 'result' | 'error'>,
): JobOutcomeSummary {
  const error = asRecord(job.error)
  const errorMessage = readString(error, 'message')
  const hint = readString(error, 'recovery_hint')

  if (job.status !== 'succeeded') {
    const headline =
      job.status === 'interrupted'
        ? '任务被中断，结果未知：防火墙可能改了一半，先确认管理通路还在，再去看 ufw status'
        : job.status === 'canceled'
          ? '任务已取消（防火墙动作不可取消，走到这一步说明是被外部打断的）'
          : '任务失败'
    return { ok: false, headline, lines: errorMessage === null ? [] : [errorMessage], hint }
  }

  const result = asRecord(job.result) ?? {}
  const observed = asRecord(result['observed_state'])
  const data = asRecord(result['data'])
  const lines: string[] = []

  const outcome = readString(result, 'outcome')
  if (outcome !== null) lines.push(FIREWALL_OUTCOME_LABELS[outcome] ?? `服务端结论：${outcome}`)

  // 启用状态是这次改动的一半：只加规则不启用等于没改，而启用恰恰是最容易把人关在门外的那一步。
  const enabled = readBool(observed, 'enabled')
  if (enabled !== null) {
    lines.push(enabled ? '读回来 UFW 是启用状态' : '读回来 UFW 没有启用（规则在，但不生效）')
  }

  const manager = readString(observed, 'manager')
  if (manager !== null) lines.push(`防火墙管理器：${manager}`)

  const rules = readStringList(observed, 'managed_rules')
  if (rules.length === 0) {
    lines.push('读回来没有任何面板管理的规则')
  } else {
    lines.push(`读回来的面板规则（${rules.length} 条）：`)
    for (const rule of rules.slice(0, 8)) lines.push(`· ${rule}`)
    if (rules.length > 8) lines.push(`…还有 ${rules.length - 8} 条没列出来`)
  }

  // 退回的是什么：这是"点确认之前该看一眼"的那句话。
  const previous = readString(data, 'previous')
  if (previous !== null) lines.push(`改之前的样子：${previous}`)

  const windowSeconds = data?.['window_seconds']
  if (typeof windowSeconds === 'number') {
    lines.push(`回滚窗口：${windowSeconds} 秒内不确认就自动退回`)
  }

  const deadline = readString(data, 'deadline')
  if (deadline !== null) lines.push(`到期时刻：${deadline}`)

  const token = readString(data, 'rollback_token')
  if (token !== null) lines.push(`回滚标识：${token}`)

  const configHash = readString(observed, 'config_hash')
  if (configHash !== null) lines.push(`改动之后的规则集指纹：${configHash.slice(0, 20)}…`)

  let headline: string
  let ok: boolean
  if (outcome === 'rolled_back') {
    ok = false
    headline = '改动没有留在机器上：已按记录退回改之前的样子'
  } else if (outcome === 'unknown') {
    ok = false
    headline = '执行完了但结果未知：现在 UFW 到底是什么规则没有被验证过，先去看一眼现场'
  } else {
    ok = true
    headline =
      lines.length === 0
        ? '任务已成功，但代理没有返回可展示的现场读数；请刷新防火墙页确认'
        : '任务已成功，下面是读回来的规则集'
  }
  const recoveryHint = readString(result, 'recovery_hint') ?? hint
  return { ok, headline, lines, hint: recoveryHint }
}

// ---------------------------------------------------------------- 应用启停

/**
 * 一次应用启停的结果翻译（P09.5）。
 *
 * 与应用页其它地方同一个口径：**运行状态与入口状态是两件事**。所以这里只报运行时那一侧
 * 的读数（单元/容器的状态、期望值、有没有读回来核对过），绝不说“应用已恢复”——
 * 入口通不通要看下一次健康探测，不在这条任务里。
 *
 * 两个动作（service.control / container.control）的字段名不同但语义一致：
 * `active_state` ↔ `state`，`enablement` 只在单元上有。按存在性读，不按动作名分叉。
 */
const APP_CONTROL_OUTCOME_LABELS: Record<string, string> = {
  verified: '执行后读回来的状态与期望一致',
  unknown: '执行完了，但读回来的状态与期望不一致',
}

export function summarizeAppJob(
  job: Pick<JobDTO, 'status' | 'result' | 'error'>,
): JobOutcomeSummary {
  const error = asRecord(job.error)
  const errorMessage = readString(error, 'message')
  const hint = readString(error, 'recovery_hint')

  if (job.status !== 'succeeded') {
    const headline =
      job.status === 'interrupted'
        ? '任务被中断，结果未知：先刷新应用状态再决定，不要盲目再点一次'
        : job.status === 'canceled'
          ? '任务已取消'
          : '任务失败'
    return { ok: false, headline, lines: errorMessage === null ? [] : [errorMessage], hint }
  }

  const result = asRecord(job.result) ?? {}
  const observed = asRecord(result['observed_state'])
  const data = asRecord(result['data'])
  const lines: string[] = []

  const outcome = readString(result, 'outcome')
  if (outcome !== null) lines.push(APP_CONTROL_OUTCOME_LABELS[outcome] ?? `服务端结论：${outcome}`)

  const unit = readString(observed, 'unit')
  if (unit !== null) lines.push(`单元：${unit}`)
  const containerId = readString(observed, 'container_id')
  if (containerId !== null) lines.push(`容器：${containerId.slice(0, 12)}…`)

  const operation = readString(observed, 'operation')
  if (operation !== null) lines.push(`执行的动作：${operation}`)

  const previous = readString(observed, 'previous_state')
  const after = readString(observed, 'active_state') ?? readString(observed, 'state')
  if (after !== null) {
    lines.push(previous === null ? `现在：${after}` : `状态：${previous} → ${after}`)
  }
  const subState = readString(observed, 'sub_state')
  if (subState !== null) lines.push(`子状态：${subState}`)
  const enablement = readString(observed, 'enablement')
  if (enablement !== null) lines.push(`开机启动：${enablement}`)
  const expected = readString(observed, 'expected')
  if (expected !== null) lines.push(`期望的状态：${expected}`)

  const verified = readBool(observed, 'verified')
  if (verified !== null) {
    lines.push(verified ? '执行后重新读了一遍，状态与期望一致' : '执行后重新读了一遍，状态与期望不一致')
  }

  const exitCode = data?.['exit_code']
  if (typeof exitCode === 'number') lines.push(`外部命令退出码：${exitCode}`)
  const stderr = readString(data, 'stderr')
  if (stderr !== null) lines.push(`命令输出（stderr）：${stderr}`)
  const stdout = readString(data, 'stdout')
  if (stdout !== null) lines.push(`命令输出：${stdout}`)

  const headline =
    lines.length === 0
      ? '任务已成功，但代理没有返回可展示的现场读数；请刷新应用列表确认'
      : '任务已成功，下面是执行后的现场读数（入口通不通要看下一次健康探测）'
  return { ok: true, headline, lines, hint }
}

// ---------------------------------------------------------------- 软件包写事务

/**
 * 一次 APT 写事务的结果翻译（P11.2）。
 *
 * 这一处的口径与别处不同，值得写清楚：**apt 的退出码不是结论**。代理执行完之后会
 * 重新读一遍 dpkg 数据库，逐条比对预检计划里的目标版本；只有每一跳都对上，
 * `observed_state.verified` 才是 true，`outcome` 才是 verified。所以这里第一行
 * 永远说"核对过了没有"，而不是"命令返回 0"。
 *
 * 四个动作的字段是同一套（install / upgrade / remove 共用 packageResult，
 * refresh 走 refreshResult），因此按存在性读，不按动作名分叉。
 */
const PACKAGE_OUTCOME_LABELS: Record<string, string> = {
  verified: '执行后逐包核对过：读回的版本与预检的计划一致',
  partial: '执行完成，但读回的状态与预检的计划不一致',
  unknown: '执行完成，但没能核对出结论',
}

export function summarizePackageJob(
  job: Pick<JobDTO, 'status' | 'result' | 'error'>,
): JobOutcomeSummary {
  const error = asRecord(job.error)
  const errorMessage = readString(error, 'message')
  const hint = readString(error, 'recovery_hint')

  if (job.status !== 'succeeded') {
    const headline =
      job.status === 'interrupted'
        ? '任务被中断，结果未知：dpkg 可能停在半配置状态，先去软件包页看现场再决定'
        : job.status === 'canceled'
          ? '任务已取消'
          : '任务失败'
    return { ok: false, headline, lines: errorMessage === null ? [] : [errorMessage], hint }
  }

  const result = asRecord(job.result) ?? {}
  const observed = asRecord(result['observed_state'])
  const data = asRecord(result['data'])
  const lines: string[] = []

  const outcome = readString(result, 'outcome')
  if (outcome !== null) lines.push(PACKAGE_OUTCOME_LABELS[outcome] ?? `服务端结论：${outcome}`)

  const verified = readBool(observed, 'verified')
  if (verified === false) {
    lines.push('没有通过核对：apt 报告成功，但读回的软件包状态与预检的计划不一致')
  }

  const operation = readString(observed, 'operation') ?? readString(data, 'operation')
  if (operation !== null) lines.push(`执行的写操作：${operation}`)

  const packages = readStringList(data, 'packages')
  if (packages.length > 0) lines.push(`目标软件包：${packages.join('、')}`)

  // 逐包的真实状态：`pkg:` 前缀由代理写进 observed_state（`ii 版本` / `未安装`）。
  // 顺序按字典序，页面每次打开都一样。
  if (observed !== null) {
    const pkgLines = Object.keys(observed)
      .filter((key) => key.startsWith('pkg:'))
      .sort()
      .map((key) => {
        const value = observed[key]
        return `${key.slice(4)}：${typeof value === 'string' ? value : '状态未知'}`
      })
    for (const line of pkgLines) lines.push(line)
  }

  const mismatches = readStringList(data, 'mismatches')
  if (mismatches.length > 0) lines.push(`对不上的地方：${mismatches.join('；')}`)

  const indexBefore = readString(data, 'index_before')
  const indexAfter = readString(data, 'index_after')
  if (indexAfter !== null) {
    lines.push(
      indexBefore === null
        ? `执行后索引时间：${indexAfter}`
        : `索引时间：${indexBefore} → ${indexAfter}`,
    )
  }
  const refreshErrors = readStringList(data, 'refresh_errors')
  if (refreshErrors.length > 0) lines.push(`apt-get update 报告的错误行：${refreshErrors.join('；')}`)
  const refreshWarns = readStringList(data, 'refresh_warns')
  if (refreshWarns.length > 0) lines.push(`apt-get update 的告警：${refreshWarns.join('；')}`)

  const exitCode = data?.['exit_code']
  if (typeof exitCode === 'number') lines.push(`apt-get 退出码：${exitCode}`)
  const stdout = readString(data, 'stdout')
  if (stdout !== null) lines.push(`apt-get 输出：${stdout}`)
  const stderr = readString(data, 'stderr')
  if (stderr !== null) lines.push(`apt-get 错误输出：${stderr}`)

  const headline =
    lines.length === 0
      ? '任务已成功，但代理没有返回可展示的现场读数；请刷新软件包列表确认'
      : verified === true
        ? '执行完成，并且逐包核对通过'
        : '执行完成，下面是读回的现场读数'
  return { ok: true, headline, lines, hint }
}

// ── 电源任务（P11.3） ──────────────────────────────────────────────────────
//
// 电源任务的结果只有一种：accepted。它说的是“命令交出去了”，不是“机器重启了”。
// 所以这里的翻译不写“重启成功”，而是把三件事摊开：
//
//   1. 交出去的是什么命令、什么时候交的（observed_state 的原文）；
//   2. 受理不等于发生——这句话必须出现在结论行里，不能只在文档里；
//   3. 如果服务端已经在详情里做了 boot_id 核对（power_check），把那句话带上：
//      变了才是 confirmed，没变就是“还没发生”，连不上就直说连不上。

const POWER_OUTCOME_LABELS: Record<string, string> = {
  accepted: '命令已交给 systemctl 并拿到返回码——这与“机器真的重启/关机了”是两件事',
  canceled: '倒计时期间取消：命令没有交出去，机器没有动',
}

export function summarizePowerJob(
  job: Pick<JobDTO, 'status' | 'result' | 'error' | 'power_check'>,
): JobOutcomeSummary {
  const error = asRecord(job.error)
  const errorMessage = readString(error, 'message')
  const hint = readString(error, 'recovery_hint')

  if (job.status !== 'succeeded') {
    const canceled = job.status === 'canceled'
    const headline = canceled
      ? '已取消：倒计时里停下的，systemctl 没有被调用'
      : job.status === 'interrupted'
        ? '任务被中断，结果未知：命令可能已经交出去，先确认机器是否还连着'
        : '任务失败：命令没有交出去，机器不受影响'
    return { ok: false, headline, lines: errorMessage === null ? [] : [errorMessage], hint }
  }

  const result = asRecord(job.result) ?? {}
  const observed = asRecord(result['observed_state'])
  const lines: string[] = []

  const outcome = readString(result, 'outcome')
  if (outcome !== null) lines.push(POWER_OUTCOME_LABELS[outcome] ?? `服务端结论：${outcome}`)

  const operation = readString(observed, 'operation')
  if (operation !== null) lines.push(`操作：${operation === 'poweroff' ? '关机' : '重启'}`)

  const delay = observed?.['delay_seconds']
  if (typeof delay === 'number') lines.push(`倒计时：${delay} 秒`)

  const bootBefore = readString(observed, 'boot_id_before')
  if (bootBefore !== null) lines.push(`下发前的 boot_id：${bootBefore}`)

  const issuedAt = readString(observed, 'issued_at')
  if (issuedAt !== null) lines.push(`下发时间：${issuedAt}`)

  const command = readString(observed, 'command')
  if (command !== null) lines.push(`命令：${command}`)

  const exitCode = observed?.['exit_code']
  if (typeof exitCode === 'number') lines.push(`systemctl 退出码：${exitCode}`)

  const check = job.power_check ?? null
  if (check !== null && check.message !== '') {
    lines.push(`核对（${check.state}）：${check.message}`)
    if (check.boot_id_now !== '') lines.push(`现在的 boot_id：${check.boot_id_now}`)
  } else if (operation === 'poweroff') {
    lines.push('关机之后代理会消失，面板读不到任何状态：连接断开符合预期，但不构成关机成功的证据。')
  } else {
    lines.push('重启是否真的发生要看 boot_id 有没有变化；任务中心的任务详情里会给出核对结果。')
  }

  const headline =
    check === null
      ? '请求已受理（不是已重启）：下面这些是下发那一刻的现场读数'
      : check.state === 'confirmed'
        ? '已确认：boot_id 变了，机器确实重新启动过'
        : check.state === 'unchanged'
          ? '请求已受理，但机器仍在原来那次开机上：重启还没有发生'
          : check.state === 'unreachable'
            ? '请求已受理，但代理现在连不上：最终状态读不到'
            : '请求已受理，但这次核对查不清：boot_id 读不出来'
  return { ok: true, headline, lines, hint }
}

// ---------------------------------------------------------------- 备份与恢复

/** 备份动作的 outcome（internal/agent 的 Result.Outcome）。 */
const BACKUP_OUTCOME_LABELS: Record<string, string> = {
  verified: '执行后读回来核对过',
  unknown: '执行完了，但读回来的结果对不上',
  partial: '只做成了一部分',
}

/**
 * 一次备份/恢复/验证/清理的结果翻译（P12.5）。
 *
 * 四个动作（backup.run / restore / verify / prune）的字段不同，但都是同一个形状：
 * `result.outcome` 说结论，`result.observed_state` 说执行后读到了什么。**按字段存在性
 * 读，不按动作名分叉**——动作名分叉会随着动作变多而烂掉，而且这里没有任何一步需要
 * 知道"我是谁"，只需要知道"这条结果里有什么"。
 *
 * 一条绝对不省的话：**备份成功不等于恢复已验证**。run 的 verified 说的是"快照写完后
 * 读回来对得上"，不是"这份数据能恢复到别的机器上"。要那个结论只能单独做一次
 * backup.verify，所以这里每次都把它写出来。
 */
export function summarizeBackupJob(job: Pick<JobDTO, 'status' | 'result' | 'error'>): JobOutcomeSummary {
  const error = asRecord(job.error)
  const errorMessage = readString(error, 'message')
  const hint = readString(error, 'recovery_hint')

  if (job.status !== 'succeeded') {
    const headline =
      job.status === 'interrupted'
        ? '任务被中断，结果未知：先读一次快照列表确认仓库里多了什么，再决定要不要重跑（重跑不会覆盖已有快照，只会再写一份）'
        : job.status === 'canceled'
          ? '任务已取消'
          : '任务失败'
    return { ok: false, headline, lines: errorMessage === null ? [] : [errorMessage], hint }
  }

  const result = asRecord(job.result) ?? {}
  const observed = asRecord(result['observed_state'])
  const data = asRecord(result['data'])
  const lines: string[] = []

  const outcome = readString(result, 'outcome')
  if (outcome !== null) lines.push(BACKUP_OUTCOME_LABELS[outcome] ?? `服务端结论：${outcome}`)

  // 快照标识：run / restore / verify 都会给。
  const snapshotId = readString(observed, 'snapshot_id') ?? readString(data, 'snapshot_id')
  if (snapshotId !== null) {
    lines.push(`快照：${snapshotId}`)
  }

  // 仓库本身的读数：backup.repository.initialize 会给全，别的动作给到就报。
  const repoID = readString(observed, 'repo_id')
  if (repoID !== null) lines.push(`restic 仓库标识：${repoID}`)
  const resticVersion = readString(observed, 'restic_version')
  if (resticVersion !== null) lines.push(`restic 版本：${resticVersion}`)
  const absolutePath = readString(observed, 'absolute_path')
  if (absolutePath !== null) lines.push(`实际位置：${absolutePath}`)
  const hostKey = readString(observed, 'host_key')
  if (hostKey !== null) {
    lines.push(`远端主机公钥：${hostKey}（known_hosts 里用的就是它）`)
  }
  const initializedAt = readString(observed, 'initialized_at')
  if (initializedAt !== null) lines.push(`初始化完成于：${initializedAt}`)

  // run：写完之后读回来核对的结果，以及 restic 自己的统计。
  const verifiedBy = readString(observed, 'verified_by')
  if (verifiedBy !== null) lines.push(`核验方式：${verifiedBy}`)
  const verifyError = readString(observed, 'verify_error')
  if (verifyError !== null) lines.push(`读回核对没通过：${verifyError}`)
  const filesNew = observed?.['files_new']
  if (typeof filesNew === 'number') {
    const changed = observed?.['files_changed']
    const total = observed?.['total_files_processed']
    const added = observed?.['data_added']
    lines.push(
      `restic 统计：新增 ${filesNew} 个文件` +
        (typeof changed === 'number' ? `、变化 ${changed} 个` : '') +
        (typeof total === 'number' ? `，共处理 ${total} 个` : '') +
        (typeof added === 'number' ? `，写入 ${added} 字节` : ''),
    )
  }

  // run：这份快照读了哪些来源（读不到的也要说出来，那是最该知道的一行）。
  const sources = Array.isArray(observed?.['sources']) ? (observed?.['sources'] as unknown[]) : []
  for (const entry of sources) {
    const row = asRecord(entry)
    const name = readString(row, 'source')
    if (name === null) continue
    const exists = readBool(row, 'exists')
    const path = readString(row, 'path')
    lines.push(
      `来源 ${name}${path === null ? '' : `（${path}）`}：` +
        (exists === false ? '执行时读不到（这份快照里没有它）' : '已包含在这份快照里'),
    )
  }

  const panel = asRecord(observed?.['panel_database'])
  if (panel !== null) {
    const integrity = readString(panel, 'integrity')
    lines.push(
      `面板数据库副本：${String(panel['size_bytes'] ?? '?')} 字节，schema ${String(panel['schema_version'] ?? '?')}` +
        (integrity === null ? '' : `，完整性 ${integrity}`),
    )
  }

  // 停过哪些应用、拉起成没成。备份成功但对机器状态只字不提，管理员就得自己去 systemctl 里逐个确认。
  const appsRestored = readBool(observed, 'apps_restored')
  if (appsRestored !== null) {
    lines.push(
      appsRestored
        ? '备份前停掉的应用都已按原状态拉起'
        : '有应用没能按原状态拉起：先看下面的逐条结果，再决定是否手工拉起',
    )
  }
  const apps = Array.isArray(observed?.['apps']) ? (observed?.['apps'] as unknown[]) : []
  for (const entry of apps) {
    const row = asRecord(entry)
    const object = readString(row, 'object')
    if (object === null) continue
    const operation = readString(row, 'operation') ?? '?'
    const done = readBool(row, 'done')
    const appVerified = readBool(row, 'verified')
    const note = readString(row, 'note')
    lines.push(
      `应用 ${object}：${operation}` +
        (done === null ? '' : done ? '，已执行' : '，没执行') +
        (appVerified === null ? '' : appVerified ? '，读回核对通过' : '，读回核对不一致') +
        (note === null ? '' : `（${note}）`),
    )
  }

  // restore：恢复目标与规模。published 是"目录已经发布"这一件事，不是"业务恢复了"。
  const published = readBool(observed, 'published')
  if (published !== null) {
    const root = readString(observed, 'target_root_id')
    const relative = observed?.['relative_path']
    const where = typeof relative === 'string' ? (relative === '' ? '/' : `/${relative}`) : '?'
    lines.push(
      published
        ? `目录已发布到文件根 ${root ?? '?'} 下的 ${where}`
        : `目录没有发布：恢复到文件根 ${root ?? '?'} 下的 ${where} 没有完成`,
    )
  }
  const files = observed?.['files']
  if (typeof files === 'number') {
    const directories = observed?.['directories']
    const bytes = observed?.['bytes']
    lines.push(
      `恢复规模：${files} 个文件` +
        (typeof directories === 'number' ? `、${directories} 个目录` : '') +
        (typeof bytes === 'number' ? `，共 ${bytes} 字节` : ''),
    )
  }

  // verify：核对到了哪一层。
  const checked = readString(observed, 'checked')
  if (checked !== null) lines.push(`核对范围：${checked}`)
  const sampleRestore = readBool(observed, 'sample_restore')
  const restoreVerified = readBool(observed, 'restore_verified')
  if (sampleRestore === true) {
    const checkedFiles = observed?.['files_checked']
    const checkedBytes = observed?.['bytes_checked']
    lines.push(
      restoreVerified === true
        ? `抽样恢复通过：真的恢复了 ${String(checkedFiles ?? '?')} 个文件、${String(checkedBytes ?? '?')} 字节并比对内容`
        : '抽样恢复没有通过：这份快照读不回来，不要指望它',
    )
  } else if (sampleRestore === false) {
    lines.push('这次只核对了结构与索引，没有抽文件真恢复：内容能不能读回来不在这条结论里')
  }

  // prune：删了什么、留了什么。
  const deleted = readStringList(observed, 'deleted_ids')
  const kept = readStringList(observed, 'kept_ids')
  if (deleted.length > 0 || kept.length > 0) {
    lines.push(`删除 ${deleted.length} 份、保留 ${kept.length} 份`)
    if (deleted.length > 0) lines.push(`已删除：${deleted.join('、')}`)
  }

  const headline =
    lines.length === 0
      ? '任务已成功，但代理没有返回可展示的现场读数；请刷新快照列表确认'
      : '任务已成功，下面是执行后的现场读数（备份成功不等于恢复已验证）'
  return { ok: true, headline, lines, hint }
}

// ---------------------------------------------------------------- 计划任务与脚本

/**
 * 一次计划任务或脚本动作的结果翻译（P12.5）。
 *
 * schedule.apply / schedule.delete / script.apply / script.delete / script.run
 * 共用这一份：它们都返回 `observed_state`，字段按存在性读。
 *
 * 两条必须说出来的话：
 *   1) schedule.apply 成功**只代表定义落库、单元装上**。它是不是真的会跑，要看
 *      installed / unit_file_state / active_state 三个观测值——所以这里逐条报出来。
 *   2) script.run 成功代表退出码是 0，不代表脚本做的事是对的。输出不落库，这一点
 *      每次都要说，否则"任务成功"会被读成"事情办成了"。
 */
export function summarizeScheduleJob(job: Pick<JobDTO, 'status' | 'result' | 'error'>): JobOutcomeSummary {
  const error = asRecord(job.error)
  const errorMessage = readString(error, 'message')
  const hint = readString(error, 'recovery_hint')

  if (job.status !== 'succeeded') {
    const headline =
      job.status === 'interrupted'
        ? '任务被中断，结果未知：先刷新列表看单元与定义现在是什么样，不要直接重提'
        : job.status === 'canceled'
          ? '任务已取消'
          : '任务失败'
    return { ok: false, headline, lines: errorMessage === null ? [] : [errorMessage], hint }
  }

  const result = asRecord(job.result) ?? {}
  const observed = asRecord(result['observed_state'])
  const lines: string[] = []

  const scheduleId = readString(observed, 'schedule_id')
  const deleted = readBool(observed, 'deleted')
  if (scheduleId !== null) {
    const name = readString(observed, 'name')
    lines.push(`计划：${name ?? scheduleId}（${scheduleId}）`)
  }
  if (deleted === true) lines.push('记录已删除，timer 与 service 单元已移除')

  if (scheduleId !== null && deleted === null) {
    const installed = readBool(observed, 'installed')
    const unitFile = readString(observed, 'unit_file_state')
    const active = readString(observed, 'active_state')
    const enabled = readBool(observed, 'enabled')
    const timerRef = readString(observed, 'timer_ref')
    if (timerRef !== null) lines.push(`单元：${timerRef}`)
    if (enabled !== null) {
      lines.push(enabled ? '定义里写的是“启用”' : '定义里写的是“停用”：单元在盘上，timer 不会自己起来')
    }
    if (installed === false) {
      lines.push('单元没装上：这条定义现在不会触发')
    } else if (unitFile !== null || active !== null) {
      lines.push(`单元现状：${unitFile ?? '?'} / ${active ?? '?'}`)
    }
  }

  const scriptId = readString(observed, 'script_id')
  if (scriptId !== null) {
    const name = readString(observed, 'name')
    lines.push(`脚本：${name ?? scriptId}（${scriptId}）`)
    const contentHash = readString(observed, 'content_hash')
    if (contentHash !== null) lines.push(`内容指纹：${contentHash}`)
    const path = readString(observed, 'path')
    if (path !== null) lines.push(`落盘位置：${path}`)
    const bytes = observed?.['bytes']
    if (typeof bytes === 'number') lines.push(`正文 ${bytes} 字节`)

    const exitCode = observed?.['exit_code']
    if (typeof exitCode === 'number') {
      const durationMs = observed?.['duration_ms']
      const stdoutBytes = observed?.['stdout']
      const stderrBytes = observed?.['stderr']
      const truncated = readBool(observed, 'truncated')
      lines.push(
        `退出码 ${exitCode}` +
          (typeof durationMs === 'number' ? `，耗时 ${durationMs} 毫秒` : '') +
          (typeof stdoutBytes === 'number' ? `，标准输出 ${stdoutBytes} 字节` : '') +
          (typeof stderrBytes === 'number' ? `，标准错误 ${stderrBytes} 字节` : '') +
          (truncated === true ? '（输出被截断）' : ''),
      )
      lines.push('输出不落库：要留证据就在脚本里写日志文件')
    }
  }

  const headline =
    lines.length === 0
      ? '任务已成功，但代理没有返回可展示的现场读数；请刷新列表确认'
      : deleted === true
        ? '单元与记录已删除'
        : '任务已成功，下面是执行后的现场读数'
  return { ok: true, headline, lines, hint }
}

/**
 * 汇总一次受管理账号动作的结果（P14.5）。
 *
 * 六个动作共用这一个翻译器，分支靠**结果字段**而不是动作名：调用方只有
 * `result/observed_state`，没有 action（`StorageActionDialog` 的 summarizer 签名就是
 * 这样）。用字段名分派在这里是可靠的，因为四条路各自带一个别人没有的键：
 *
 *   account.delete    `linux_removed`（账号在系统上还在不在）
 *   ssh_key.*         `managed_keys` + `fingerprint`（托管区里现在有几把）
 *   sudo_rule.*       `rule_id` + `template_id`（规则文件与读回的命令条数）
 *   account.apply     `password_state`（新建，passwd -S 读回的锁定态）或
 *                     `shell_changed`（改壳），两者都是这一路独有的
 *
 * 顺序是先删除、再 sudo、再公钥、最后账号：`sudo_rule.delete` 的结果里也有
 * `account_id`，靠它先判会被错认成账号动作。
 *
 * 全部按“读不到就不说”处理：字段缺失时不出那一行，也不编造值。
 */
export function summarizeAccountJob(job: Pick<JobDTO, 'status' | 'result' | 'error'>): JobOutcomeSummary {
  const error = asRecord(job.error)
  const errorMessage = readString(error, 'message')
  const hint = readString(error, 'recovery_hint')

  if (job.status !== 'succeeded') {
    const headline =
      job.status === 'interrupted'
        ? '任务被中断，结果未知：账号或 sudoers 可能只改了一半，先去账号页看现在的状态再决定'
        : job.status === 'canceled'
          ? '任务已取消'
          : '任务失败'
    return { ok: false, headline, lines: errorMessage === null ? [] : [errorMessage], hint }
  }

  const result = asRecord(job.result) ?? {}
  const observed = asRecord(result['observed_state'])
  const lines: string[] = []

  const outcome = readString(result, 'outcome')
  if (outcome !== null) lines.push(OUTCOME_LABELS[outcome] ?? `服务端结论：${outcome}`)

  const name = readString(observed, 'name')
  const where = name === null ? '' : `账号 ${name}`

  // ---- account.delete：账号在系统上没了，但 home 里的数据还在 ----
  if (observed !== null && ('linux_removed' in observed || 'home_data_kept' in observed)) {
    const removed = readBool(observed, 'linux_removed')
    const wasAbsent = readBool(observed, 'linux_was_absent')
    if (wasAbsent === true) {
      lines.push(`${where} 的系统账号本来就已经不在（getent 读不到），这次只清了面板记录`)
    } else if (removed === true) {
      lines.push(`${where} 的系统账号已删除（getent passwd 读回为空）`)
    }
    const ruleRemoved = readBool(observed, 'sudo_rule_removed')
    if (ruleRemoved === true) lines.push('这条账号的 sudo 规则文件已一并撤销')
    const home = readString(observed, 'home')
    if (home !== null) {
      lines.push(`home 目录 ${home} 没有删（userdel 不带 -r）：${readBool(observed, 'home_still_present') === true ? '目录仍在，里面的文件一个没动' : '目录已经不在'}`)
    }
    return {
      ok: true,
      headline: wasAbsent === true ? '记录已清理，系统账号本来就不在' : '账号已删除，数据保留',
      lines,
      hint,
    }
  }

  // ---- sudo_rule.*：文件哈希版本 + sudo 自己解析出来的条数 ----
  const templateId = readString(observed, 'template_id')
  if (observed !== null && templateId !== null && ('rule_id' in observed)) {
    const path = readString(observed, 'path')
    if (readBool(observed, 'removed') === true) {
      lines.push(`${where} 的 sudo 规则已撤销（visudo -c 整份校验通过）`)
    } else if (readBool(observed, 'unchanged') === true) {
      lines.push(`${where} 的 sudo 规则内容没有变化，没有重写文件`)
    } else {
      lines.push(`${where} 获得 sudo 模板「${sudoTemplateLabel(templateId)}」`)
    }
    if (path !== null) lines.push(`规则文件：${path}`)
    const commands = observed['commands']
    if (typeof commands === 'number') {
      lines.push(`写入 ${commands} 条授权，逐条来自模板；没有通配符，也不接受单元名参数`)
    }
    const rules = observed['sudo_rules']
    if (typeof rules === 'number') {
      lines.push(`sudo -l -U 读回 ${rules} 条授权，与模板逐条一致`)
    }
    const remaining = observed['sudo_rules_remaining']
    if (typeof remaining === 'number') {
      lines.push(`撤销之后 sudo -l -U 还剩 ${remaining} 条授权`)
    }
    const version = readString(observed, 'version')
    if (version !== null) lines.push(`文件内容哈希：${version.slice(0, 16)}…`)
    return {
      ok: true,
      headline: readBool(observed, 'removed') === true ? 'sudo 规则已撤销' : 'sudo 规则已生效',
      lines,
      hint,
    }
  }

  // ---- ssh_key.*：托管区里现在有几把、文件在哪 ----
  const fingerprint = readString(observed, 'fingerprint')
  const managedKeys = observed?.['managed_keys']
  if (observed !== null && fingerprint !== null && typeof managedKeys === 'number') {
    const keyId = readString(observed, 'key_id')
    const keyType = readString(observed, 'key_type')
    if (readBool(observed, 'removed') === true || !('key_type' in observed)) {
      lines.push(`${where} 的公钥 ${fingerprint} 已从托管区撤销`)
    } else {
      lines.push(
        `${where} 的公钥已写入（${keyType ?? '未知算法'}，${fingerprint}）` +
          (readBool(observed, 'replaced') === true ? '，替换了同一条记录的内容' : ''),
      )
    }
    const path = readString(observed, 'path')
    if (path !== null) lines.push(`${path} 已整份重读：托管区里现在 ${managedKeys} 把，托管区外的行逐字节没动`)
    const fileHash = readString(observed, 'file_hash')
    if (fileHash !== null) lines.push(`文件哈希：${fileHash.slice(0, 16)}…`)
    if (keyId !== null) lines.push(`公钥记录：${keyId}`)
    return {
      ok: true,
      headline: '公钥已生效',
      lines,
      hint,
    }
  }

  // ---- account.apply：新建 / 改壳 / 改口令 ----
  const passwordState = readString(observed, 'password_state')
  const shellChanged = readBool(observed, 'shell_changed')
  const passwordChanged = readBool(observed, 'password_changed')

  if (shellChanged !== null || passwordChanged !== null) {
    if (shellChanged === true) {
      const shell = readString(observed, 'shell')
      const profile = readString(observed, 'shell_profile')
      lines.push(
        `${where} 的登录壳已改成 ${shell ?? '（读不到）'}` +
          (profile === null ? '' : `（这一档是 ${SHELL_PROFILE_TEXT[profile] ?? profile}）`),
      )
    } else if (shellChanged === false) {
      lines.push('登录壳没有变化')
    }
    if (passwordChanged === true) lines.push('口令已通过标准输入交给 chpasswd，没有经过命令行或日志')
    if (readBool(observed, 'disabled') === true) lines.push('账号此刻是禁用的：shell 指向 nologin，登不上')
    const hasPassword = readBool(observed, 'has_password')
    if (hasPassword === false) lines.push('这个账号仍然没有口令')
  } else {
    const uid = observed?.['uid']
    if (typeof uid === 'number') lines.push(`已建好 ${where}：uid ${uid}（落在 1000–59999）`)
    const shell = readString(observed, 'shell')
    if (shell !== null) lines.push(`登录壳 ${shell}`)
    const home = readString(observed, 'home')
    if (home !== null) lines.push(`home 目录 ${home}`)
    if (passwordState !== null) {
      lines.push(
        passwordState === 'P'
          ? 'passwd -S 读回 P：口令已生效'
          : `passwd -S 读回 ${passwordState}：这个账号没有可用口令，只能用已配的公钥登录（没有口令不拦人，见 Q479）`,
      )
    }
    if (readBool(observed, 'secret_persisted') === true) {
      lines.push('密码密文已挂上持久引用（不再随临时秘密过期）')
    }
  }

  return {
    ok: true,
    headline: lines.length === 0 ? '任务已成功，但代理没有返回可展示的现场读数；请刷新列表确认' : '任务已成功，下面是执行后的现场读数',
    lines,
    hint,
  }
}

/** 壳档的中文说法（结果翻译专用；闭集在 api/accounts.ts 里，这里只为了不出循环依赖）。 */
const SHELL_PROFILE_TEXT: Record<string, string> = {
  disabled: '禁用登录',
  restricted: '受限登录',
}

/** sudo 模板的中文名（与 api/accounts.ts 的 SUDO_TEMPLATES 同源）。 */
const SUDO_TEMPLATE_TEXT: Record<string, string> = {
  system_status: '查看整机状态',
  journal_recent: '查看最近日志',
}

function sudoTemplateLabel(id: string): string {
  return SUDO_TEMPLATE_TEXT[id] ?? id
}

// ---------------------------------------------------------------- 受管部署（P15.4）

/**
 * 一次部署动作的结果翻译（P15.4）。
 *
 * 三个动作（apply / update / remove）共用这一个翻译器，按**结果字段**分派而不是按
 * 动作名：调用方手里只有 `result / observed_state`。各自的独有键：
 *
 *   deployment.apply   `written`（这次有没有真的写盘）
 *   deployment.update  `image_digest` 与 `previous_config_digest`（换了哪一版镜像、配置变没变）
 *   deployment.remove  `data_kept`（数据一个字节都没动）
 *
 * 两种“既不是成功也不是失败”的结论必须说清楚：
 *
 *  - `outcome=rolled_back`：任务状态是 succeeded、现场已经回到上一版，剩下的问题只有
 *    “新版本为什么没成”。所以 headline 不能说“已更新”——那句话会让人以为换成了。
 *  - `outcome=partial`：单元 active，但 compose 项目下没有容器，或者健康探测没通。
 *    这时候**部署确实落地了**，只是它跑起来的那一面不对，两句话都要说。
 */
const DEPLOYMENT_OUTCOME_LABELS: Record<string, string> = {
  verified: '执行后读回：现场与预检的计划一致',
  partial: '执行完成，但读回的现场与预检的计划不完全一致',
  rolled_back: '这次更新没有成功：现场已经回切到上一版配置与镜像（数据不跟着回切）',
  unknown: '执行完成，但没能核对出结论',
}

/** 摘要的短形式：`sha256:` 之后留 12 位。 */
function shortDigest(value: string): string {
  const hex = value.startsWith('sha256:') ? value.slice('sha256:'.length) : value
  if (hex.length <= 12) return value
  return `${value.slice(0, value.length - hex.length + 12)}…`
}

export function summarizeDeploymentJob(
  job: Pick<JobDTO, 'status' | 'result' | 'error'>,
): JobOutcomeSummary {
  const error = asRecord(job.error)
  const errorMessage = readString(error, 'message')
  const errorHint = readString(error, 'recovery_hint')

  if (job.status !== 'succeeded') {
    const headline =
      job.status === 'interrupted'
        ? '任务被中断，结果未知：先刷新部署列表看清现场，不要直接再点一次'
        : job.status === 'canceled'
          ? '任务已取消'
          : '任务失败'
    return {
      ok: false,
      headline,
      lines: errorMessage === null ? [] : [errorMessage],
      hint: errorHint,
    }
  }

  const result = asRecord(job.result) ?? {}
  const observed = asRecord(result['observed_state'])
  const data = asRecord(result['data'])
  const lines: string[] = []

  const outcome = readString(result, 'outcome')
  if (outcome !== null) lines.push(DEPLOYMENT_OUTCOME_LABELS[outcome] ?? `服务端结论：${outcome}`)
  // 回切成功时那句失败原因放在 result.recovery_hint 里（不是 error 里）。
  const hint = readString(result, 'recovery_hint') ?? errorHint

  const project = readString(observed, 'project') ?? readString(data, 'project')
  if (project !== null) lines.push(`Compose 项目：${project}`)
  const unit = readString(observed, 'unit') ?? readString(data, 'unit')
  if (unit !== null) lines.push(`包装单元：${unit}`)

  const written = readBool(observed, 'written')
  if (written !== null) {
    lines.push(written ? '这一次写下了新的单元文件与渲染配置' : '盘上已经是这份内容，没有重写（幂等重放）')
  }
  if (readBool(observed, 'idempotent') === true) {
    lines.push('读回判定为幂等重放：现场与你看到的那一份相同')
  }

  if (readBool(observed, 'unit_restarted') === true) {
    lines.push('单元被 restart 过一次（ExecStart 真的重跑了）')
  }
  if (readBool(observed, 'unit_stopped') === true) {
    lines.push('更新前单元被停过（备份要的是停止写入的那份数据）')
  }
  const active = readBool(observed, 'unit_active')
  const enabled = readBool(observed, 'unit_enabled')
  if (active !== null || enabled !== null) {
    const parts: string[] = []
    if (active !== null) parts.push(active ? 'active' : '不是 active')
    if (enabled !== null) parts.push(enabled ? '开机会自动启动' : '开机不会自动启动')
    lines.push(`systemctl 读回：${parts.join('，')}`)
  }

  const previousDigest = readString(observed, 'previous_config_digest')
  const configDigest = readString(observed, 'config_digest') ?? readString(data, 'config_digest')
  if (previousDigest !== null && configDigest !== null) {
    lines.push(
      previousDigest === configDigest
        ? `配置没有变：${shortDigest(configDigest)}`
        : `配置摘要：${shortDigest(previousDigest)} → ${shortDigest(configDigest)}`,
    )
  } else if (configDigest !== null) {
    lines.push(`配置摘要：${shortDigest(configDigest)}`)
  }

  const image = readString(observed, 'image') ?? readString(data, 'image')
  if (image !== null) lines.push(`镜像引用：${image}`)
  const imageDigest = readString(observed, 'image_digest')
  if (imageDigest !== null) lines.push(`镜像 digest：${shortDigest(imageDigest)}`)
  if (readBool(observed, 'image_digest_known') === false) {
    lines.push('镜像 digest 这次没有读数：空着表示“不知道”，不表示“和上一版一样”')
  }

  const backupSnapshot = readString(observed, 'backup_snapshot_id')
  if (backupSnapshot !== null) {
    const verified = readBool(observed, 'backup_verified')
    lines.push(
      `更新前备份：快照 ${backupSnapshot}` +
        (verified === null ? '' : verified ? '（独立读过，在仓库里）' : '（没能独立读回核对）'),
    )
  }
  const backupSummary = readString(observed, 'backup_summary')
  if (backupSummary !== null) lines.push(`备份摘要：${backupSummary}`)

  const healthChecked = readBool(observed, 'health_checked')
  if (healthChecked === false) {
    const healthError = readString(observed, 'health_error')
    lines.push(
      healthError === null
        ? '这次没有做健康探测（定义里没打开探测）'
        : `这次没有做健康探测：${healthError}`,
    )
  } else if (healthChecked === true) {
    const healthOK = readBool(observed, 'health_ok')
    const healthURL = readString(observed, 'health_url')
    const statusCode = observed?.['health_status_code']
    const parts: string[] = [healthOK === true ? '探测通过' : '探测没通过']
    if (healthURL !== null) parts.push(healthURL)
    if (typeof statusCode === 'number') parts.push(`状态码 ${statusCode}`)
    const duration = observed?.['health_duration_ms']
    if (typeof duration === 'number') parts.push(`${duration} 毫秒`)
    lines.push(`健康探测：${parts.join(' · ')}`)
    const healthError = readString(observed, 'health_error')
    if (healthError !== null) lines.push(`探测的最后一个错误：${healthError}`)
    if (readBool(observed, 'health_truncated') === true) {
      lines.push('探测应答被截断（超过读取上限）：截断不算通')
    }
  }

  if (readBool(observed, 'rolled_back') === true) {
    const rollbackOK = readBool(observed, 'rollback_verified')
    const rollbackHealth = readBool(observed, 'rollback_health_ok')
    lines.push(
      '回切后的现场：' +
        (rollbackOK === true ? '上一版配置与镜像已恢复' : '上一版配置是否恢复没能核对') +
        (rollbackHealth === null ? '' : rollbackHealth ? '，探测已通' : '，探测仍不通'),
    )
  }

  const containersChecked = readBool(observed, 'containers_checked')
  if (containersChecked === true) {
    const total = observed?.['containers_total']
    const running = observed?.['containers_running']
    if (typeof total === 'number' && typeof running === 'number') {
      lines.push(`这个 compose 项目下读到 ${total} 个容器，其中 ${running} 个在跑`)
    }
  } else if (containersChecked === false) {
    const containersError = readString(observed, 'containers_error')
    const tail = '：“有没有真的跑起来”没有独立读数'
    lines.push(
      containersError === null ? `容器清单这次没读到（docker 不通）${tail}` : `容器清单没读到：${containersError}${tail}`,
    )
  }

  if (readBool(observed, 'unit_removed') === true) lines.push('包装单元文件已删除')
  if (readBool(observed, 'config_removed') === true) lines.push('渲染的 Compose 配置已删除')
  if (readBool(observed, 'data_kept') === true) {
    lines.push('数据卷与业务目录一个字节都没有动（删除只删单元与配置）')
  }

  const headline =
    outcome === 'rolled_back'
      ? '这次更新没有成功，现场已经回切到上一版；下面是读回的现场'
      : outcome === 'partial'
        ? '这一步做了，但读回来有一项不对（见下）：不要当成“已经好了”'
        : lines.length === 0
          ? '任务已成功，但代理没有返回可展示的现场读数；请刷新部署列表确认'
          : '任务已成功，下面是执行后的现场读数'

  return { ok: true, headline, lines, hint }
}
