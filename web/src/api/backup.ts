// 备份的 DTO、状态翻译与动作参数（P12.5）。
//
// 与 api/openapi.yaml 的备份段、docs/CONTRACTS.md 第 5、12 节逐字对应。
//
// 这一层有三件事必须写清楚，否则页面会开始撒谎：
//   1) 仓库定义与策略**存在代理那边**（面板不存第二份）。代理不在时整组回 503，
//      而不是给一份空列表——空列表会被读成"这台机器上还没有配置备份"。
//   2) 备份、恢复、验证、清理**不在这里执行**：它们是 backup.run / backup.restore /
//      backup.verify / backup.prune 的预检 → 任务。这个模块只拼参数，判断交给预检。
//   3) 快照大小只有详情里才有。列表不含大小是**故意的**：每条都要单独跑一次
//      restic stats，那是分钟级的代价（backupSnapshotListResponse.size_included）。
//      列表里显示"没有大小"是真的没有，不是 0。
//
// 名称与形状的来源是 internal/api/backup.go 里的 DTO；改字段时两边一起改。

import { get, request, type ApiResult } from './client'

/** 快照查询的超时。服务端给这类查询的写截止时间是 60 秒（slowQueryBudget）。 */
export const SNAPSHOT_QUERY_TIMEOUT_MS = 75_000

// ---------------------------------------------------------------- 阻塞项与告警

/** 代理侧的阻塞/告警条目。message 是人读的，code 是闭集。 */
export interface BackupNotice {
  code: string
  message: string
}

/**
 * 已知的备份阻塞码与告警码 → 中文短名。
 *
 * 页面**不**用这张表替代 message：message 是代理按现场写的（带着仓库名、路径、
 * 指纹），这里只给列表里一个短标签。表里没有的码原样显示 code——未知的码不该被
 * 静默吞掉，那会让"面板不认识这个判决"看起来像"没有判决"。
 */
export const BACKUP_NOTICE_LABELS: Record<string, string> = {
  BACKUP_REPOSITORY_MISSING: '仓库未登记',
  BACKUP_REPOSITORY_EXISTS: '仓库已存在',
  BACKUP_REPOSITORY_NOT_READY: '仓库还没初始化',
  BACKUP_REPOSITORY_BUSY: '仓库正被占用',
  BACKUP_POLICY_NOT_FOUND: '策略不存在',
  BACKUP_POLICY_VERSION_STALE: '策略版本已变',
  BACKUP_SNAPSHOT_NOT_FOUND: '快照不存在',
  BACKUP_RESTIC_MISSING: '没装 restic',
  BACKUP_RESTIC_VERSION_UNKNOWN: '读不到 restic 版本',
  BACKUP_SECRET_UNRESOLVED: '秘密取不到',
  BACKUP_SECRET_PURPOSE: '秘密用途不符',
  BACKUP_SFTP_UNREACHABLE: '连不上 SFTP',
  BACKUP_SFTP_AUTH: 'SFTP 认证失败',
  BACKUP_HOST_KEY_MISMATCH: '主机指纹不符',
  BACKUP_SSH_MISSING: '缺 ssh 客户端',
  BACKUP_ROOT_UNKNOWN: '文件根未登记',
  BACKUP_SOURCE_MISSING: '备份源不存在',
  BACKUP_SOURCE_BLOCKED: '备份源被拒',
  BACKUP_PATH_NOT_DIR: '目标不是目录',
  BACKUP_NO_SPACE: '空间不够',
  BACKUP_SPACE_LOW: '空间偏紧',
  BACKUP_CAPACITY_UNKNOWN: '读不到容量',
  BACKUP_PANEL_DATABASE_MISSING: '面板库找不到',
  BACKUP_APP_UNAVAILABLE: '停机对象不可用',
  BACKUP_FILES_ONLY: '只备份文件（不停应用）',
  BACKUP_RETENTION_INVALID: '保留策略不合法',
  BACKUP_RETENTION_NOT_ENFORCED: '保留策略不会执行',
  BACKUP_RESTORE_UNAVAILABLE: '这台机器不能恢复',
  BACKUP_RESTORE_TARGET_BLOCKED: '恢复目标被拒',
  BACKUP_RESTORE_TREE_UNSUPPORTED: '快照目录结构不支持',
  APP_PROTECTED: '受保护的应用',
  PREFLIGHT_STALE: '预检已过期',
  VERSION_CONFLICT: '版本冲突',
}

/** 取一条条目的短标签；未知码原样返回。 */
export function backupNoticeLabel(code: string): string {
  return BACKUP_NOTICE_LABELS[code] ?? code
}

// ---------------------------------------------------------------- 仓库

export const BACKUP_BACKENDS = ['local', 'sftp'] as const
export type BackupBackend = (typeof BACKUP_BACKENDS)[number]

export const BACKUP_BACKEND_LABELS: Record<BackupBackend, string> = {
  local: '本机目录',
  sftp: 'SFTP 服务器',
}

/**
 * 仓库位置。两种后端共用一张结构，字段按 backend 互斥。
 *
 * 这里保留全部字段（而不是两条联合类型）：服务端就是一张扁平结构，且
 * 不属于该后端的字段会被**拒绝**——把互斥关系画进类型只会让"多带了一个 host
 * 字段"从编译错误变成运行时 400，反而不如让表单一次只填该填的几个。
 */
export interface BackupLocation {
  root_id?: string
  relative_path?: string
  host?: string
  port?: number
  username?: string
  remote_path?: string
  host_key_fingerprint?: string
}

/**
 * 一条仓库定义。
 *
 * password_secret_ref / credential_secret_ref 是**引用**而不是秘密：面板从不持有
 * 仓库口令或 SFTP 私钥的明文，它们只以 `sec_` 引用的形式出现，由代理在执行时
 * 从自己的加密存储里取。所以这两个字段可以出现在页面上。
 */
export interface BackupRepository {
  id: string
  name: string
  backend: BackupBackend
  location: BackupLocation
  password_secret_ref: string
  credential_secret_ref?: string
  version: number
}

/** 写仓库时的请求体（新建与修改同一份，修改要带 version）。 */
export interface BackupRepositoryInput {
  name: string
  backend: BackupBackend
  location: BackupLocation
  password_secret_ref: string
  credential_secret_ref: string
  version: number
}

export interface BackupRepositoryList {
  items: BackupRepository[]
  /** 仓库现状的只读查询名（代理侧；界面不调它，但它是契约的一部分）。 */
  repository_state_query: string
}

export function fetchBackupRepositories(signal?: AbortSignal): Promise<ApiResult<BackupRepositoryList>> {
  return get<BackupRepositoryList>('/backup/repositories', { ...(signal === undefined ? {} : { signal }) })
}

export function fetchBackupRepository(id: string): Promise<ApiResult<BackupRepository>> {
  return get<BackupRepository>(`/backup/repositories/${encodeURIComponent(id)}`)
}

/**
 * 登记一条仓库定义（admin + CSRF + 再认证）。
 *
 * 要再认证的理由：这份定义引用着仓库口令与 SFTP 私钥。`REAUTH_REQUIRED` 由调用方
 * 先走 /auth/reauth 再重试一次——密码永远不进这个请求体。
 */
export function createBackupRepository(
  input: BackupRepositoryInput,
  idempotencyKey: string,
): Promise<ApiResult<BackupRepository>> {
  return request<BackupRepository>('/backup/repositories', {
    method: 'POST',
    body: input,
    idempotencyKey,
    timeoutMs: 30_000,
  })
}

/** 修改一条仓库定义。version 是乐观并发令牌：它回答"我改的是哪一版"。 */
export function updateBackupRepository(
  id: string,
  input: BackupRepositoryInput,
): Promise<ApiResult<BackupRepository>> {
  return request<BackupRepository>(`/backup/repositories/${encodeURIComponent(id)}`, {
    method: 'PATCH',
    body: input,
    timeoutMs: 30_000,
  })
}

// ---------------------------------------------------------------- 策略

export const BACKUP_SOURCE_KINDS = ['root', 'panel_database'] as const
export type BackupSourceKind = (typeof BACKUP_SOURCE_KINDS)[number]

export const BACKUP_CONSISTENCY_MODES = ['files_only', 'stop_managed_apps'] as const
export type BackupConsistencyMode = (typeof BACKUP_CONSISTENCY_MODES)[number]

export const BACKUP_CONSISTENCY_LABELS: Record<BackupConsistencyMode, string> = {
  files_only: '只备份文件',
  stop_managed_apps: '备份前停掉受管理应用',
}

/** 一条备份来源。kind 缺省按 root 解释（P12.2 之前的记录里没有这个字段）。 */
export interface BackupSource {
  kind?: BackupSourceKind
  root_id?: string
  relative_path?: string
}

/** 保留策略。0 表示"不设这一项"（Agent 侧是 omitempty，不会出现 0）。 */
export interface BackupRetention {
  keep_last?: number
  keep_daily?: number
  keep_weekly?: number
}

export interface BackupPolicy {
  id: string
  name: string
  repository_id: string
  sources: BackupSource[]
  consistency_mode: BackupConsistencyMode
  app_ids: string[]
  retention: BackupRetention
  version: number
}

export interface BackupPolicyInput {
  name: string
  repository_id: string
  sources: BackupSource[]
  consistency_mode: BackupConsistencyMode
  app_ids: string[]
  retention: BackupRetention
  version: number
}

export interface BackupPolicyList {
  items: BackupPolicy[]
}

export function fetchBackupPolicies(signal?: AbortSignal): Promise<ApiResult<BackupPolicyList>> {
  return get<BackupPolicyList>('/backup/policies', { ...(signal === undefined ? {} : { signal }) })
}

export function fetchBackupPolicy(id: string): Promise<ApiResult<BackupPolicy>> {
  return get<BackupPolicy>(`/backup/policies/${encodeURIComponent(id)}`)
}

/** 新建策略（admin + CSRF；不再认证——策略不引用秘密，也不动主机状态）。 */
export function createBackupPolicy(input: BackupPolicyInput): Promise<ApiResult<BackupPolicy>> {
  return request<BackupPolicy>('/backup/policies', {
    method: 'POST',
    body: input,
    timeoutMs: 30_000,
  })
}

export function updateBackupPolicy(id: string, input: BackupPolicyInput): Promise<ApiResult<BackupPolicy>> {
  return request<BackupPolicy>(`/backup/policies/${encodeURIComponent(id)}`, {
    method: 'PATCH',
    body: input,
    timeoutMs: 30_000,
  })
}

/** 删除策略。删策略不动仓库，也不动仓库里已有的快照。 */
export function deleteBackupPolicy(id: string): Promise<ApiResult<Record<string, never>>> {
  return request<Record<string, never>>(`/backup/policies/${encodeURIComponent(id)}`, {
    method: 'DELETE',
    timeoutMs: 30_000,
  })
}

// ---------------------------------------------------------------- 快照

export interface BackupSnapshot {
  snapshot_id: string
  short_id: string
  time: string
  paths: string[]
  tags: string[]
  hostname?: string
  policy_id?: string
  consistency_mode?: string
  /** 字符串（十进制字节数）：超出 JS 安全整数的大小时也不失真。 */
  size_bytes?: string
  file_count?: string
  /** null 表示这份快照没做过恢复验证——不是"验证失败"。 */
  restore_verified_at: string | null
}

/**
 * 一个仓库（或一条策略）的快照列表。
 *
 * `size_included = false` 时每条 item 都不带 size_bytes / file_count：列表不跑
 * restic stats，要看大小与文件数得点开详情。这在界面上要明说，否则"没有大小"
 * 会被读成"快照是空的"。
 */
export interface BackupSnapshotList {
  repository_id: string
  policy_id?: string
  registered: boolean
  count: number
  items: BackupSnapshot[]
  size_included: boolean
  checked_at: string
}

/** 单份快照详情。found=false 时 snapshot 不存在于响应里（不是空对象）。 */
export interface BackupSnapshotDetail {
  repository_id: string
  registered: boolean
  found: boolean
  snapshot?: BackupSnapshot
  checked_at: string
}

export function fetchBackupSnapshots(
  repositoryId: string,
  policyId?: string,
): Promise<ApiResult<BackupSnapshotList>> {
  return request<BackupSnapshotList>('/backup/snapshots', {
    method: 'GET',
    query: {
      repository_id: repositoryId,
      ...(policyId === undefined || policyId === '' ? {} : { policy_id: policyId }),
    },
    timeoutMs: SNAPSHOT_QUERY_TIMEOUT_MS,
  })
}

/** 读一份快照的详情（含大小与文件数：这一条会跑 restic stats）。 */
export function fetchBackupSnapshot(
  repositoryId: string,
  snapshotId: string,
): Promise<ApiResult<BackupSnapshotDetail>> {
  return request<BackupSnapshotDetail>(
    `/backup/snapshots/${encodeURIComponent(snapshotId)}`,
    {
      method: 'GET',
      query: { repository_id: repositoryId },
      timeoutMs: SNAPSHOT_QUERY_TIMEOUT_MS,
    },
  )
}

// ---------------------------------------------------------------- 动作参数

export const BACKUP_RUN_ACTION = 'backup.run'
export const BACKUP_RESTORE_ACTION = 'backup.restore'
export const BACKUP_VERIFY_ACTION = 'backup.verify'
export const BACKUP_PRUNE_ACTION = 'backup.prune'
export const BACKUP_INITIALIZE_ACTION = 'backup.repository.initialize'

/**
 * backup.repository.initialize 的参数：仓库 id 与它的版本。
 *
 * 登记一条定义只是"记下仓库在哪"，它不代表目标位置上已经有一个 restic 仓库。
 * 初始化会在目标位置真的写一个仓库出来（`restic init`），因此要再认证一次。
 */
export function backupInitializeParams(
  repo: Pick<BackupRepository, 'id' | 'version'>,
): Record<string, unknown> {
  return { repository_id: repo.id, version: repo.version }
}

/**
 * backup.run 的参数：只有策略与版本。
 *
 * `apps` 不在其中，而且**不能**由调用方给：面板按策略里的 app_ids 把"要停哪个
 * 应用"展开成此刻的运行对象（哪个 systemd 单元、哪个容器 id）。自己带 apps 会被
 * 拒——那是"参数不能扩大策略授权范围"这条规则要挡的东西。
 */
export function backupRunParams(
  policy: Pick<BackupPolicy, 'id' | 'version'>,
): Record<string, unknown> {
  return { backup_policy_id: policy.id, version: policy.version }
}

/**
 * backup.restore 的五个字段。`apps` 同样由服务端展开：覆盖恢复时面板会保守地
 * 停掉全部可停的受管理应用，有一个停不了就阻塞——绝不漏停后继续覆盖。
 */
export function backupRestoreParams(input: {
  repositoryId: string
  snapshotId: string
  targetRootId: string
  relativePath: string
  overwrite: boolean
}): Record<string, unknown> {
  return {
    repository_id: input.repositoryId,
    snapshot_id: input.snapshotId,
    target_root_id: input.targetRootId,
    relative_path: input.relativePath,
    overwrite: input.overwrite,
  }
}

/** backup.verify：核对快照结构；sample_restore 时再抽几个文件真恢复一遍。 */
export function backupVerifyParams(input: {
  repositoryId: string
  snapshotId: string
  sampleRestore: boolean
}): Record<string, unknown> {
  return {
    repository_id: input.repositoryId,
    snapshot_id: input.snapshotId,
    sample_restore: input.sampleRestore,
  }
}

/**
 * backup.prune：按保留策略清理。
 *
 * 参数只有策略与版本，**不带** `expected_delete_ids`。那份名单是代理的可选字段，含义是
 * "我确认过要删的是这几条"——而"确认"发生在预检这一屏：预检的 impacts 里列的就是
 * 这一次将要删掉的快照，提交时服务端还会拿同一个预检 id 复核现场（PREFLIGHT_STALE）。
 * 把这个名单从页面再抄一遍进参数，只会让人以为它是来自另一处的证据。
 *
 * 清理是这四个动作里唯一**真的会删数据**的那一个，所以调用方要给这一屏加
 * 二次确认（`irreversible`）。
 */
export function backupPruneParams(policy: Pick<BackupPolicy, 'id' | 'version'>): Record<string, unknown> {
  return { backup_policy_id: policy.id, version: policy.version }
}

// ---------------------------------------------------------------- 翻译

/** 保留策略的一句话说法；一项都没设时如实说"不清理"。 */
export function retentionLabel(retention: BackupRetention): string {
  const parts: string[] = []
  const last = retention.keep_last ?? 0
  const daily = retention.keep_daily ?? 0
  const weekly = retention.keep_weekly ?? 0
  if (last > 0) parts.push(`最近 ${last} 份`)
  if (daily > 0) parts.push(`每天 ${daily} 份`)
  if (weekly > 0) parts.push(`每周 ${weekly} 份`)
  return parts.length === 0 ? '不清理（不清任何旧快照）' : parts.join('、')
}

/** 一条来源的说法（不含绝对路径：绝对路径是代理读出来的事实，不在定义里）。 */
export function sourceLabel(source: BackupSource): string {
  const kind = source.kind ?? 'root'
  if (kind === 'panel_database') return '面板数据库（SQLite 在线副本）'
  const root = source.root_id ?? '?'
  const path = source.relative_path ?? ''
  return path === '' ? `文件根 ${root} 的整个根` : `文件根 ${root} 下的 /${path}`
}

/** 仓库位置的短说法（列表里一行）。 */
export function locationLabel(repo: Pick<BackupRepository, 'backend' | 'location'>): string {
  if (repo.backend === 'sftp') {
    const host = repo.location.host ?? '?'
    const port = repo.location.port ?? 22
    const user = repo.location.username ?? '?'
    return `${user}@${host}:${port} ${repo.location.remote_path ?? ''}`.trim()
  }
  const root = repo.location.root_id ?? '?'
  const path = repo.location.relative_path ?? ''
  return path === '' ? `文件根 ${root}:/` : `文件根 ${root}:/${path}`
}

/**
 * 快照时间的排序键。
 *
 * 列表按时间倒序显示：restic 自己给的是 RFC3339 字符串，直接比字符串在
 * "全部同一时区、同一精度"时可用，但那不是能依赖的前提。解析不了就排到最后，
 * 而不是当成最早——一份读不出时间的快照被排到列表末尾再说话，比它被当成
 * "最新的一份"安全。
 */
export function snapshotTimeKey(snapshot: Pick<BackupSnapshot, 'time'>): number {
  const at = new Date(snapshot.time).getTime()
  return Number.isNaN(at) ? Number.NEGATIVE_INFINITY : at
}

/** 按时间倒序（新→旧）。不修改入参。 */
export function sortSnapshotsNewestFirst(snapshots: readonly BackupSnapshot[]): BackupSnapshot[] {
  return [...snapshots].sort((a, b) => snapshotTimeKey(b) - snapshotTimeKey(a))
}
