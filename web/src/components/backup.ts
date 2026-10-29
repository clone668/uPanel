// 备份页的纯逻辑（P12.5）：表单状态 ↔ 定义、形状校验、给人看的确认文案。
//
// 为什么单独一个模块：备份这一页有四张表单（仓库、策略、恢复、清理）与三份动作
// 参数，形状规则和"这句话该怎么说"挤在视图里就没法单独测。页面只负责取数与摆放。
//
// 一条边界必须写下来：这里的校验**只是形状提示**（非空、长度、字符集、id 形状），
// 与 internal/backup 的 Normalize 同源但不权威。真正的问题——根在不在、主机通不通、
// 指纹对不对、空间够不够、卷挂没挂——只有预检能回答，所以按钮永远不是"校验通过就
// 能点"，而是"预检说 allowed 才能点"。

import {
  BACKUP_BACKENDS,
  type BackupBackend,
  type BackupConsistencyMode,
  type BackupPolicy,
  type BackupPolicyInput,
  type BackupRepository,
  type BackupRepositoryInput,
  type BackupRetention,
  type BackupSnapshot,
  type BackupSource,
  locationLabel,
  retentionLabel,
} from '@/api/backup'

// ---------------------------------------------------------------- 形状常量

/** 与 internal/backup 的 Max* 常量一一对应。 */
export const MAX_REPOSITORY_NAME = 64
export const MAX_POLICY_NAME = 64
export const MAX_PATH = 1024
export const MAX_HOST = 253
export const MAX_POLICY_SOURCES = 32
export const MAX_POLICY_APPS = 32
export const MAX_RETENTION = 1000

/** `sec_` 加 32 位小写十六进制（秘密引用）。 */
const SECRET_REF_PATTERN = /^sec_[0-9a-f]{32}$/
/** 文件根标识：以字母或数字开头，只含字母、数字、下划线、点和连字符。 */
const ROOT_ID_PATTERN = /^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$/
/** SFTP 用户名：小写字母或下划线开头，只含小写字母、数字、点、下划线、连字符。 */
const USERNAME_PATTERN = /^[a-z_][a-z0-9._-]{0,31}$/
/** 主机名或 IP：字母、数字、点、连字符、下划线（服务端的白名单同形）。 */
const HOST_PATTERN = /^[A-Za-z0-9._-]+$/

function trimmed(value: string): string {
  return value.trim()
}

// ---------------------------------------------------------------- 仓库表单

export interface RepositoryFormState {
  name: string
  backend: BackupBackend
  /** local 后端用。 */
  rootId: string
  relativePath: string
  /** sftp 后端用；端口是输入框里的字符串，提交时再转数字。 */
  host: string
  port: string
  username: string
  remotePath: string
  hostKeyFingerprint: string
  passwordSecretRef: string
  credentialSecretRef: string
}

export function emptyRepositoryForm(defaultRootId = ''): RepositoryFormState {
  return {
    name: '',
    backend: 'local',
    rootId: defaultRootId,
    relativePath: 'upanel-restic',
    host: '',
    port: '22',
    username: '',
    remotePath: '',
    hostKeyFingerprint: '',
    passwordSecretRef: '',
    credentialSecretRef: '',
  }
}

/** 把一条已有定义摊回表单（"读出来、改一处、存回去"是这里唯一的编辑方式）。 */
export function repositoryFormOf(repo: BackupRepository): RepositoryFormState {
  return {
    name: repo.name,
    backend: repo.backend,
    rootId: repo.location.root_id ?? '',
    relativePath: repo.location.relative_path ?? '',
    host: repo.location.host ?? '',
    port: String(repo.location.port ?? 22),
    username: repo.location.username ?? '',
    remotePath: repo.location.remote_path ?? '',
    hostKeyFingerprint: repo.location.host_key_fingerprint ?? '',
    passwordSecretRef: repo.password_secret_ref,
    credentialSecretRef: repo.credential_secret_ref ?? '',
  }
}

/**
 * 表单 → 请求体。
 *
 * 只带该后端该带的字段：服务端对"local 带 host"这类串味**报错**而不忽略，
 * 所以这里不能把另一后端的空值一起发过去。
 */
export function buildRepositoryInput(
  state: RepositoryFormState,
  version = 0,
): BackupRepositoryInput {
  const base = {
    name: trimmed(state.name),
    backend: state.backend,
    password_secret_ref: trimmed(state.passwordSecretRef),
    version,
  }
  if (state.backend === 'sftp') {
    const port = Number.parseInt(trimmed(state.port), 10)
    return {
      ...base,
      location: {
        host: trimmed(state.host),
        port: Number.isNaN(port) ? 22 : port,
        username: trimmed(state.username),
        remote_path: trimmed(state.remotePath),
        host_key_fingerprint: trimmed(state.hostKeyFingerprint),
      },
      credential_secret_ref: trimmed(state.credentialSecretRef),
    }
  }
  return {
    ...base,
    location: { root_id: trimmed(state.rootId), relative_path: trimmed(state.relativePath) },
    credential_secret_ref: '',
  }
}

/** 长度与字符集检查（形状提示，不是判决）。 */
function checkPath(field: string, raw: string, problems: string[]): void {
  const value = trimmed(raw)
  if (value === '') return
  if (value.length > MAX_PATH) problems.push(`${field} 不能超过 ${MAX_PATH} 字节`)
  if (value.startsWith('/')) problems.push(`${field} 是相对路径，不能以 / 开头`)
  if (value.split('/').includes('..')) problems.push(`${field} 不能含 ..`)
}

export function validateRepositoryForm(state: RepositoryFormState): string[] {
  const problems: string[] = []
  const name = trimmed(state.name)
  if (name === '') problems.push('仓库名不能为空')
  else if (name.length > MAX_REPOSITORY_NAME) problems.push(`仓库名不能超过 ${MAX_REPOSITORY_NAME} 字节`)

  if (!SECRET_REF_PATTERN.test(trimmed(state.passwordSecretRef))) {
    problems.push('仓库口令引用应为 sec_ 加 32 位小写十六进制（口令本身只经标准输入交给代理，面板从不持有明文）')
  }

  if (state.backend === 'local') {
    const rootId = trimmed(state.rootId)
    if (rootId === '') problems.push('本地仓库必须选一个文件根')
    else if (!ROOT_ID_PATTERN.test(rootId)) problems.push('文件根标识形状不合法')
    checkPath('相对路径', state.relativePath, problems)
    if (trimmed(state.credentialSecretRef) !== '') {
      problems.push('本地仓库没有远端凭据：SFTP 私钥引用必须留空')
    }
  } else {
    const host = trimmed(state.host)
    if (host === '') problems.push('SFTP 主机不能为空')
    else if (host.length > MAX_HOST) problems.push(`主机名不能超过 ${MAX_HOST} 字节`)
    else if (host.startsWith('-')) problems.push('主机名不能以 - 开头（会被 ssh 当成选项）')
    else if (!HOST_PATTERN.test(host)) problems.push('主机名只允许主机名或 IP 的字符')

    const port = Number.parseInt(trimmed(state.port), 10)
    if (Number.isNaN(port) || port < 1 || port > 65535) problems.push('端口必须在 1—65535 之间')

    if (!USERNAME_PATTERN.test(trimmed(state.username))) {
      problems.push('远端用户名应以小写字母或下划线开头，只含小写字母、数字、点、下划线、连字符')
    }
    const remote = trimmed(state.remotePath)
    if (!remote.startsWith('/')) problems.push('远端路径必须是绝对路径（以 / 开头）')
    else checkPath('远端路径', remote.slice(1), problems)
    if (trimmed(state.hostKeyFingerprint) === '') {
      problems.push('必须填主机公钥指纹：known_hosts 只来自显式指纹核对，不接受“先连上再说”')
    }
    if (!SECRET_REF_PATTERN.test(trimmed(state.credentialSecretRef))) {
      problems.push('SFTP 私钥引用应为 sec_ 加 32 位小写十六进制（面板不接受口令式 SSH 认证）')
    }
  }
  return problems
}

/** 后端标签（表单里的选项文案）。 */
export function backendLabel(backend: BackupBackend): string {
  return backend === 'sftp' ? 'SFTP 服务器' : '本机目录'
}

/** 后端选项（顺序固定，与 BACKUP_BACKENDS 一致）。 */
export function backendOptions(): { label: string; value: BackupBackend }[] {
  return BACKUP_BACKENDS.map((backend) => ({ label: backendLabel(backend), value: backend }))
}

// ---------------------------------------------------------------- 策略表单

export interface PolicySourceRow {
  kind: 'root' | 'panel_database'
  rootId: string
  relativePath: string
}

export interface PolicyFormState {
  name: string
  repositoryId: string
  sources: PolicySourceRow[]
  consistencyMode: BackupConsistencyMode
  /** 逗号或换行分隔的应用 id；空表示不停任何应用。 */
  appIds: string
  keepLast: string
  keepDaily: string
  keepWeekly: string
}

export function emptyPolicyForm(repositoryId = ''): PolicyFormState {
  return {
    name: '',
    repositoryId,
    sources: [{ kind: 'root', rootId: '', relativePath: '' }],
    consistencyMode: 'files_only',
    appIds: '',
    keepLast: '7',
    keepDaily: '7',
    keepWeekly: '4',
  }
}

export function policyFormOf(policy: BackupPolicy): PolicyFormState {
  return {
    name: policy.name,
    repositoryId: policy.repository_id,
    sources: policy.sources.map((source) => ({
      kind: source.kind === 'panel_database' ? 'panel_database' : 'root',
      rootId: source.root_id ?? '',
      relativePath: source.relative_path ?? '',
    })),
    consistencyMode: policy.consistency_mode,
    appIds: policy.app_ids.join(', '),
    keepLast: policy.retention.keep_last === undefined ? '' : String(policy.retention.keep_last),
    keepDaily: policy.retention.keep_daily === undefined ? '' : String(policy.retention.keep_daily),
    keepWeekly: policy.retention.keep_weekly === undefined ? '' : String(policy.retention.keep_weekly),
  }
}

/** 把"逗号/换行分隔"的输入拆成 id 列表（去空、去重、保序）。 */
export function parseIdList(raw: string): string[] {
  const seen = new Set<string>()
  const out: string[] = []
  for (const piece of raw.split(/[\s,，;；]+/)) {
    const id = trimmed(piece)
    if (id === '' || seen.has(id)) continue
    seen.add(id)
    out.push(id)
  }
  return out
}

/** 保留条数的输入解析：空串是 0（不设这一项），不是 NaN。 */
function parseRetentionCount(raw: string): number | null {
  const text = trimmed(raw)
  if (text === '') return 0
  if (!/^\d+$/.test(text)) return null
  const value = Number.parseInt(text, 10)
  if (value < 0 || value > MAX_RETENTION) return null
  return value
}

export function buildPolicyInput(state: PolicyFormState, version = 0): BackupPolicyInput {
  const retention: BackupRetention = {}
  const last = parseRetentionCount(state.keepLast)
  const daily = parseRetentionCount(state.keepDaily)
  const weekly = parseRetentionCount(state.keepWeekly)
  if (last !== null && last > 0) retention.keep_last = last
  if (daily !== null && daily > 0) retention.keep_daily = daily
  if (weekly !== null && weekly > 0) retention.keep_weekly = weekly

  const sources: BackupSource[] = state.sources.map((row) =>
    row.kind === 'panel_database'
      ? { kind: 'panel_database' }
      : { kind: 'root', root_id: trimmed(row.rootId), relative_path: trimmed(row.relativePath) },
  )
  const input: BackupPolicyInput = {
    name: trimmed(state.name),
    repository_id: trimmed(state.repositoryId),
    sources,
    consistency_mode: state.consistencyMode,
    app_ids: state.consistencyMode === 'stop_managed_apps' ? parseIdList(state.appIds) : [],
    retention,
    version,
  }
  return input
}

export function validatePolicyForm(state: PolicyFormState): string[] {
  const problems: string[] = []
  const name = trimmed(state.name)
  if (name === '') problems.push('策略名不能为空')
  else if (name.length > MAX_POLICY_NAME) problems.push(`策略名不能超过 ${MAX_POLICY_NAME} 字节`)

  if (trimmed(state.repositoryId) === '') problems.push('策略必须指向一条已登记的仓库')

  if (state.sources.length === 0) problems.push('至少要有一条备份来源')
  if (state.sources.length > MAX_POLICY_SOURCES) {
    problems.push(`来源最多 ${MAX_POLICY_SOURCES} 条`)
  }
  for (const [index, row] of state.sources.entries()) {
    const at = `第 ${index + 1} 条来源`
    if (row.kind === 'panel_database') continue
    const rootId = trimmed(row.rootId)
    if (rootId === '') problems.push(`${at}：必须选一个文件根`)
    else if (!ROOT_ID_PATTERN.test(rootId)) problems.push(`${at}：文件根标识形状不合法`)
    checkPath(`${at} 的相对路径`, row.relativePath, problems)
  }

  if (state.consistencyMode === 'stop_managed_apps') {
    const apps = parseIdList(state.appIds)
    if (apps.length === 0) {
      problems.push(
        '选了“备份前停掉受管理应用”就要列出应用：一条不停意味着这份快照被打上 stop_managed_apps 的标签，而实际是在应用写着的中间态读的',
      )
    }
    if (apps.length > MAX_POLICY_APPS) problems.push(`应用最多 ${MAX_POLICY_APPS} 个`)
  }

  for (const [field, raw] of [
    ['最近几份', state.keepLast],
    ['每天几份', state.keepDaily],
    ['每周几份', state.keepWeekly],
  ] as const) {
    if (parseRetentionCount(raw) === null) {
      problems.push(`保留策略的“${field}”必须是 0—${MAX_RETENTION} 之间的整数（留空表示不设这一项）`)
    }
  }
  return problems
}

// ---------------------------------------------------------------- 确认文案

/** 初始化一个仓库之前给人看的几行。 */
export function initializeConfirmLines(
  repo: Pick<BackupRepository, 'name' | 'backend' | 'location'>,
): string[] {
  return [
    `在 ${locationLabel(repo)} 上执行 restic init，建出一个新的仓库`,
    '登记一条定义只是记下"仓库在哪"，这一步才真的在目标位置写仓库（config、keys 与目录结构）',
    '目标位置上已经有一个仓库时会被拦下：不会覆盖、也不会"复用"一个不认识的仓库',
    repo.backend === 'sftp'
      ? 'SFTP 的 known_hosts 只来自你填的那份指纹核对，不接受"先连上再说"'
      : '会在文件根里建目录；空间不够时预检就会说出来',
  ]
}

/** 跑一次备份之前给人看的几行（不参与请求）。 */
export function backupRunConfirmLines(
  policy: Pick<BackupPolicy, 'name' | 'sources' | 'consistency_mode' | 'app_ids' | 'retention'>,
  repository: Pick<BackupRepository, 'name' | 'backend' | 'location'> | null,
): string[] {
  const lines = [
    `策略：${policy.name}`,
    `仓库：${repository === null ? '（读不到这条仓库的定义）' : `${repository.name}·${locationLabel(repository)}`}`,
    `来源：${policy.sources.map(sourceLabelOf).join('；')}`,
    policy.consistency_mode === 'stop_managed_apps'
      ? `备份前会停掉 ${policy.app_ids.length} 个受管理应用，备份完成后逐个拉起并核对`
      : '只备份文件：不停任何应用，正在写的数据可能是中间态',
    `保留：${retentionLabel(policy.retention)}（清理不在这一次里，要显式发起 backup.prune）`,
  ]
  return lines
}

function sourceLabelOf(source: BackupSource): string {
  if ((source.kind ?? 'root') === 'panel_database') return '面板数据库（SQLite 在线副本）'
  const path = source.relative_path ?? ''
  return `文件根 ${source.root_id ?? '?'} 下的 ${path === '' ? '整个根' : `/${path}`}`
}

/** 恢复之前给人看的几行。 */
export function restoreConfirmLines(input: {
  repositoryName: string
  snapshot: Pick<BackupSnapshot, 'short_id' | 'time' | 'paths'>
  targetRootName: string
  relativePath: string
  overwrite: boolean
}): string[] {
  const target = input.relativePath === '' ? '根目录本身' : `/${input.relativePath}`
  return [
    `从快照 ${input.snapshot.short_id}（${input.snapshot.time}）恢复`,
    `快照里的路径：${input.snapshot.paths.join('、') || '（快照没记录路径）'}`,
    `恢复到：${input.targetRootName} 下的 ${target}`,
    input.overwrite
      ? '会覆盖目标位置已有的文件，并先停掉全部可停的受管理应用（有一个停不了就整次拒绝）'
      : '不会覆盖已有文件：目标位置只要有一个同名文件，这次恢复就会被拦下',
  ]
}

/** 验证之前给人看的几行。 */
export function verifyConfirmLines(
  snapshot: Pick<BackupSnapshot, 'short_id' | 'time'>,
  sampleRestore: boolean,
): string[] {
  return [
    `验证快照 ${snapshot.short_id}（${snapshot.time}）`,
    sampleRestore
      ? '会真的抽几个文件恢复到临时目录再比对内容：慢，但这是唯一能证明“这份快照读得回来”的做法'
      : '只核对快照结构与索引：不写任何文件，也不证明内容读得回来',
  ]
}

/**
 * 清理之前给人看的几行。
 *
 * 不列具体快照 id：待删集合是**执行状态**，由预检算（代理把它写进预检的 normalized_params，
 * 提交时按同一个预检 id 复核）。所以这里指向下面那块预检结论，而不是编一份名单。
 */
export function pruneConfirmLines(policy: Pick<BackupPolicy, 'name' | 'retention'>): string[] {
  return [
    `按保留策略清理策略「${policy.name}」在仓库里的旧快照`,
    `保留：${retentionLabel(policy.retention)}`,
    '预检会先算出"要删几份、留几份"并写在下面的影响里；提交时按同一份预检复核，期间仓库多了一份新快照就会以 PREFLIGHT_STALE 拒绝',
    '这一步**真的删数据**，而且找不回来。删的是仓库里的快照，不是正在跑的系统。',
  ]
}
