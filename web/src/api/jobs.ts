// 任务中心接口与状态判定（CONTRACTS.md 第 4.3 节、第 3 节）。
//
// 这里的纯函数（isTerminal / canCancel / mergeLogs / jobErrorMessage …）是页面和
// 事件流共用的判决逻辑：状态机、取消规则、日志去重只写一遍，避免列表和详情各说一套。

import { get, request, type ApiResult } from './client'

/** 任务状态机（CONTRACTS.md 4.3）：终态是终点，不能回到运行中。 */
export const JOB_STATUSES = [
  'queued',
  'running',
  'succeeded',
  'failed',
  'canceled',
  'interrupted',
] as const

export type JobStatus = (typeof JOB_STATUSES)[number]

/** 中文标签。前端的每个状态都必须有说法，缺一个就说明契约没对齐。 */
export const JOB_STATUS_LABELS: Record<JobStatus, string> = {
  queued: '排队中',
  running: '执行中',
  succeeded: '已成功',
  failed: '已失败',
  canceled: '已取消',
  interrupted: '结果未知（已中断）',
}

/** Naive UI 标签色：成功/失败/进行中/已取消一眼可分。 */
export const JOB_STATUS_TYPES: Record<JobStatus, 'default' | 'info' | 'success' | 'warning' | 'error'> =
  {
    queued: 'default',
    running: 'info',
    succeeded: 'success',
    failed: 'error',
    canceled: 'warning',
    interrupted: 'warning',
  }

export interface JobDTO {
  job_id: string
  action: string
  status: JobStatus
  step: string
  /** null 表示代理没有上报进度；不能当成 0。 */
  progress_percent: number | null
  result: Record<string, unknown>
  error: Record<string, unknown>
  cancellable: boolean
  cancellation_requested: boolean
  created_at: string
  updated_at: string
  started_at: string | null
  finished_at: string | null
}

export interface JobListData {
  jobs: JobDTO[]
}

export interface JobLogEntry {
  seq: number
  kind: 'status' | 'progress' | 'log' | 'gap'
  payload: Record<string, unknown>
  created_at: string
}

export interface JobLogsData {
  logs: JobLogEntry[]
  next_cursor: string | null
}

/** 终态判定：这六个状态里的四个是终点。 */
export function isTerminal(status: JobStatus): boolean {
  return status === 'succeeded' || status === 'failed' || status === 'canceled' || status === 'interrupted'
}

/**
 * 是否显示取消按钮。
 *
 * 三条同时成立才显示：契约说可取消、任务还没结束、尚未请求过取消。
 * 不可取消的动作（`cancellable=false`）不显示按钮——按 PLAN.md，
 * 摆一个点了会 409 的按钮等于骗人。
 */
export function canCancel(job: JobDTO, role: 'admin' | 'viewer' | null): boolean {
  if (role !== 'admin') return false
  if (!job.cancellable) return false
  if (job.cancellation_requested) return false
  return !isTerminal(job.status)
}

/** 进度文字：null → “未上报”，0 → “0%”。两者意义不同。 */
export function progressText(job: JobDTO): string {
  return job.progress_percent === null ? '未上报' : `${job.progress_percent}%`
}

/** 错误详情：code + message；代理返回的细节在 details 里，不硬编码猜测。 */
export interface JobErrorDetail {
  code: string
  message: string
  hint: string | null
}

export function jobErrorMessage(job: JobDTO): JobErrorDetail | null {
  const raw = job.error
  if (raw === null || typeof raw !== 'object') return null
  const code = typeof raw.code === 'string' ? raw.code : ''
  const message = typeof raw.message === 'string' ? raw.message : ''
  if (code === '' && message === '') return null
  let hint: string | null = null
  if (code === 'AGENT_UNAVAILABLE') hint = '代理不可达或没有这条操作记录；确认服务状态后重新预检提交。'
  if (code === 'OPERATION_INTERRUPTED')
    hint = '执行期间 Web 或代理重启，结果未知；请先核对系统实际状态，不要直接重试。'
  return { code: code || 'UNKNOWN', message: message || '（代理未提供说明）', hint }
}

/** 合并日志页：按 seq 去重并升序，实时事件和分页结果可以直接叠。 */
export function mergeLogs(existing: JobLogEntry[], incoming: JobLogEntry[]): JobLogEntry[] {
  const bySeq = new Map<number, JobLogEntry>()
  for (const entry of existing) bySeq.set(entry.seq, entry)
  for (const entry of incoming) bySeq.set(entry.seq, entry)
  return [...bySeq.values()].sort((a, b) => a.seq - b.seq)
}

/** 裁到上限：日志是长期流，内存里只保留最近 max 条。 */
export function trimLogs(entries: JobLogEntry[], max = 500): JobLogEntry[] {
  return entries.length <= max ? entries : entries.slice(entries.length - max)
}

/** 时间显示：空字符串/null → “—”，不显示 1970 也不显示 Invalid Date。 */
export function formatTime(value: string | null | undefined): string {
  if (!value) return '—'
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return value
  return date.toLocaleString('zh-CN', { hour12: false })
}

/** 任务列表。limit 上限 200（服务端同值）。 */
export function listJobs(limit = 50): Promise<ApiResult<JobListData>> {
  return get<JobListData>('/jobs', { query: { limit } })
}

export function getJob(jobId: string): Promise<ApiResult<JobDTO>> {
  return get<JobDTO>(`/jobs/${encodeURIComponent(jobId)}`)
}

/** 请求取消。不可取消的动作由服务端返回 409，前端不预判成“成功”。 */
export function cancelJob(jobId: string): Promise<ApiResult<JobDTO>> {
  return request<JobDTO>(`/jobs/${encodeURIComponent(jobId)}/cancel`, { method: 'POST' })
}

export function fetchJobLogs(
  jobId: string,
  options: { cursor?: string | null; limit?: number } = {},
): Promise<ApiResult<JobLogsData>> {
  return get<JobLogsData>(`/jobs/${encodeURIComponent(jobId)}/logs`, {
    query: { cursor: options.cursor ?? undefined, limit: options.limit ?? 100 },
  })
}
