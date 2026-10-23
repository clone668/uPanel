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
  const startupPaths = readStringList(data, 'startup_paths')
  if (startupPaths.length > 0) lines.push(`开机前必须先就位：${startupPaths.join('、')}`)

  const offlineShares = readStringList(observed, 'volume_offline_shares') ??
    readStringList(data, 'volume_offline_shares')
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
