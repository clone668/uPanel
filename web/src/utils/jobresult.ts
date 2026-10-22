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
