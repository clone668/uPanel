// 计划任务与受管理脚本的 DTO、状态翻译与动作参数（P12.5）。
//
// 与 api/openapi.yaml 的计划任务/脚本段、docs/CONTRACTS.md 第 5、12、15 节逐字对应；
// 字段形状的来源是 internal/api/schedule.go 与 internal/schedules/schedule.go。
//
// 四件事必须写清楚，否则页面会开始撒谎：
//   1) 这里只有**读**接口。/schedules 与 /scripts 的写路径不在 REST 里：建立、修改、
//      删除是 schedule.apply / schedule.delete / script.apply / script.delete 的
//      预检 → 任务，立即执行是 script.run。在这个模块里开一个"直接写定义"的函数，
//      等于把那整条链路（再认证、任务、审计）绕过去。
//   2) 定义存在代理那边（面板不存第二份）。代理不在时整组回 503，而不是给空列表——
//      空列表会被读成"这台机器上没有定时任务"。
//   3) `enabled` 是**意图**，不是事实。它是否真的生效，要看 installed / unit_file_state /
//      active_state 三个观测字段与自己那份 blockers。把 enabled 画成绿灯就是把
//      "我们说想开"当成"它在跑"。
//   4) 证书续期计划要把域名与 ACME 环境抄进定义（`definition.certificate`）：触发发生
//      在 systemd 里，那一刻没有面板可问。面板在预检阶段按账本核对这两个值，不信调用方。
//
// 定时触发没有公开入口：POST /internal/v1/schedules/:id/trigger 是代理内部的，页面
// 不能"立即触发一条计划"。要立即跑一次，正确做法是直接执行它的目标动作（脚本用
// script.run，备份用 backup.run）。这里不提供假按钮。

import { get, request, type ApiResult } from './client'

// ---------------------------------------------------------------- 阻塞项与告警

/** 代理侧的阻塞/告警条目。message 是人读的，code 是闭集。 */
export interface ScheduleNotice {
  code: string
  message: string
}

/**
 * 已知的阻塞码与告警码 → 中文短名。
 *
 * 页面不拿这张表替代 message：message 是代理按现场写的（带着计划名、目标 id、
 * systemd 的原话）。表里没有的码原样显示 code——未知判决不该看起来像没有判决。
 */
export const SCHEDULE_NOTICE_LABELS: Record<string, string> = {
  SCHEDULE_NOT_FOUND: '计划不存在',
  SCHEDULE_INVALID: '定义不合法',
  SCHEDULE_NAME_TAKEN: '名字被占用',
  SCHEDULE_RENAMED: '改名会重建单元',
  SCHEDULE_KIND_UNSUPPORTED: '这个种类暂不支持',
  SCHEDULE_ACTION_UNSUPPORTED: '动作不允许被定时',
  SCHEDULE_CALENDAR_INVALID: '表达式不合法',
  SCHEDULE_DISABLED: '计划已停用',
  SCHEDULE_TARGET_NOT_FOUND: '目标不存在',
  SCHEDULE_TARGET_NOT_READY: '目标还没就绪',
  SCHEDULE_TARGET_TAKEN: '目标已被别的计划占用',
  SCHEDULE_TARGET_UNREADABLE: '读不到目标',
  SCHEDULE_UNIT_STATUS_UNREADABLE: '读不到单元状态',
  SCRIPT_NOT_FOUND: '脚本不存在',
  SCRIPT_INVALID: '脚本不合法',
  SCRIPT_IN_USE: '脚本仍被计划引用',
  SCRIPT_CONTENT_CHANGED: '磁盘上的内容变了',
  SCRIPT_IDENTITY_MISSING: '执行账号不存在',
  SCRIPT_RUNTIME_INVALID: '运行时长不合法',
  VERSION_CONFLICT: '版本冲突',
}

export function scheduleNoticeLabel(code: string): string {
  return SCHEDULE_NOTICE_LABELS[code] ?? code
}

// ---------------------------------------------------------------- 定义

export const SCHEDULE_KINDS = ['backup', 'certificate_renewal', 'user_script'] as const
export type ScheduleKind = (typeof SCHEDULE_KINDS)[number]

export const SCHEDULE_KIND_LABELS: Record<ScheduleKind, string> = {
  backup: '定时备份',
  certificate_renewal: '证书续期',
  user_script: '定时运行脚本',
}

/** 各固定的动作名（定时触发最终落到的那个动作，只读展示用）。 */
export const SCHEDULE_KIND_ACTIONS: Record<ScheduleKind, string> = {
  backup: 'backup.run',
  certificate_renewal: 'certificate.renew',
  user_script: 'script.run',
}

/** 目标 id 的形状（与服务端 idPatterns 一致）。 */
export const SCHEDULE_TARGET_PATTERNS: Record<ScheduleKind, RegExp> = {
  backup: /^pol_[0-9a-f]{32}$/,
  certificate_renewal: /^crt_[0-9a-f]{32}$/,
  user_script: /^scr_[0-9a-f]{32}$/,
}

/**
 * 证书续期计划的种类参数。
 *
 * `staging` 必须显式记下来：`certificate.renew` 是拿 `--server` 说话的动作，
 * staging 证书用正式入口续会换掉一个本不该上生产的对象——这不是能从主机状态
 * 反推的事实，所以它随定义一起版本化。
 */
export interface ScheduleCertificateParams {
  domain: string
  staging: boolean
}

export const MAX_CALENDAR_LENGTH = 128
export const MAX_TIMEZONE_LENGTH = 64
export const MAX_SCHEDULE_NAME_LENGTH = 80
/** 单次运行时长上限（24 小时），与服务端的 MaxRuntimeSecondsLimit 同一个数。 */
export const MAX_RUNTIME_SECONDS = 24 * 60 * 60

/** 一条计划定义（CONTRACTS §12 的 ScheduleDefinition）。 */
export interface ScheduleDefinition {
  name: string
  kind: ScheduleKind
  target_id: string
  /** 只有证书续期用它；别的种类带上会被服务端拒绝（不静默忽略）。 */
  certificate?: ScheduleCertificateParams
  calendar_expression: string
  timezone: string
  persistent: boolean
  max_runtime_seconds: number
  enabled: boolean
}

export interface ScheduleNoticeItem {
  code: string
  message: string
}

/**
 * 一条计划记录。
 *
 * 三个观测字段（installed / unit_file_state / active_state）与 `enabled` 答的是
 * 不同问题，页面上分开显示：
 *   enabled           定义的意图（我们想不想让它跑）
 *   installed         两个单元文件此刻在不在盘上
 *   unit_file_state   是否已 enable（enabled / disabled / ...）
 *   active_state      timer 此刻是否 active（active / inactive / failed）
 */
export interface ScheduleRecord {
  id: string
  name: string
  kind: ScheduleKind
  target_id: string
  definition: ScheduleDefinition
  enabled: boolean
  version: number
  timer_ref: string
  created_at: string
  updated_at: string
  installed: boolean
  unit_file_state: string
  active_state: string
  next_elapse?: string
  blockers: ScheduleNoticeItem[]
}

export interface ScheduleTrigger {
  trigger_id: string
  slot_at: string
  action: string
  operation_id: string
  outcome: string
  reason?: string
  created_at: string
}

export interface ScheduleList {
  items: ScheduleRecord[]
}

/** GET /schedules/:id 的响应体：一条计划加它的触发历史。 */
export interface ScheduleDetail {
  schedule: ScheduleRecord
  triggers: ScheduleTrigger[]
}

export function fetchSchedules(signal?: AbortSignal): Promise<ApiResult<ScheduleList>> {
  return get<ScheduleList>('/schedules', { ...(signal === undefined ? {} : { signal }) })
}

export function fetchSchedule(id: string): Promise<ApiResult<ScheduleDetail>> {
  return get<ScheduleDetail>(`/schedules/${encodeURIComponent(id)}`)
}

// ---------------------------------------------------------------- 脚本

export interface ScriptRecord {
  id: string
  name: string
  content_hash: string
  interpreter: string
  version: number
  created_at: string
  updated_at: string
  /** 只有 GET /scripts/:id 带正文；列表不带。 */
  content?: string
}

export interface ScriptList {
  items: ScriptRecord[]
}

/** GET /scripts/:id 的响应体：外面包一层 `script`（与列表项同形，多一个 content）。 */
export interface ScriptDetail {
  script: ScriptRecord
}

export function fetchScripts(signal?: AbortSignal): Promise<ApiResult<ScriptList>> {
  return get<ScriptList>('/scripts', { ...(signal === undefined ? {} : { signal }) })
}

export function fetchScript(id: string): Promise<ApiResult<ScriptDetail>> {
  return get<ScriptDetail>(`/scripts/${encodeURIComponent(id)}`)
}

// ---------------------------------------------------------------- 动作参数

export const SCHEDULE_APPLY_ACTION = 'schedule.apply'
export const SCHEDULE_DELETE_ACTION = 'schedule.delete'
export const SCRIPT_APPLY_ACTION = 'script.apply'
export const SCRIPT_DELETE_ACTION = 'script.delete'
export const SCRIPT_RUN_ACTION = 'script.run'

/**
 * schedule.apply 的参数：schedule_id、version、definition。
 *
 * 新建时 `schedule_id` 是 **null**（不是缺字段、也不是空串），version 必须是 0；
 * 修改时带上当前 version——它就是这条计划的乐观并发令牌。
 */
export function scheduleApplyParams(
  definition: ScheduleDefinition,
  existing?: Pick<ScheduleRecord, 'id' | 'version'>,
): Record<string, unknown> {
  return {
    schedule_id: existing?.id ?? null,
    version: existing?.version ?? 0,
    definition,
  }
}

export function scheduleDeleteParams(
  record: Pick<ScheduleRecord, 'id' | 'version'>,
): Record<string, unknown> {
  return { schedule_id: record.id, version: record.version }
}

/** script.apply 的参数：script_id（新建为 null）、version、name、content。 */
export function scriptApplyParams(
  input: { name: string; content: string },
  existing?: Pick<ScriptRecord, 'id' | 'version'>,
): Record<string, unknown> {
  return {
    script_id: existing?.id ?? null,
    version: existing?.version ?? 0,
    name: input.name,
    content: input.content,
  }
}

export function scriptDeleteParams(
  record: Pick<ScriptRecord, 'id' | 'version'>,
): Record<string, unknown> {
  return { script_id: record.id, version: record.version }
}

/** script.run 的参数。max_runtime_seconds 是这次运行的时间上限。 */
export function scriptRunParams(
  record: Pick<ScriptRecord, 'id' | 'version'>,
  maxRuntimeSeconds: number,
): Record<string, unknown> {
  return {
    script_id: record.id,
    version: record.version,
    max_runtime_seconds: maxRuntimeSeconds,
  }
}

// ---------------------------------------------------------------- 证书选项

/**
 * 一张可被定时续期的证书（续期计划的选项目录）。
 *
 * 只取计划需要的四个字段：id 做 target_id，domain + staging 抄进种类参数，
 * source 用来排除 certbot 签不出来的那些（手动导入的证书得手动换，指向它的
 * 续期计划只会每晚失败一次——这句话由界面说，不由这里猜）。
 */
export interface CertificateOption {
  id: string
  domain: string
  source: string
  staging: boolean
  not_after: string
  renewal_state: string
}

export interface CertificateOptionList {
  items: CertificateOption[]
  /** true 表示证书比一次读回来的多：服务端还有下一页游标。 */
  truncated: boolean
}

/** 证书选项一次取多少张（服务端单页上限是 200）。 */
export const CERTIFICATE_OPTION_LIMIT = 200

/**
 * 取可选的证书。
 *
 * 只取一页并如实报告 `truncated`：这里没有"把全部证书拉完"的循环。证书超过 200
 * 张时界面会说"只列出前 200 张"，而不是悄悄少给——少给会让人以为某张证书不存在。
 */
export async function fetchCertificateOptions(): Promise<ApiResult<CertificateOptionList>> {
  const result = await request<{
    items: CertificateOption[]
    next_cursor: string | null
  }>('/certificates', {
    method: 'GET',
    query: { limit: CERTIFICATE_OPTION_LIMIT },
  })
  return {
    requestId: result.requestId,
    data: {
      items: result.data.items ?? [],
      truncated: result.data.next_cursor !== null && result.data.next_cursor !== '',
    },
  }
}

/** 这张证书能不能被 certbot 定时续期。理由由界面用 canRenewCertificateReason 说。 */
export function canRenewCertificate(cert: Pick<CertificateOption, 'source'>): boolean {
  return cert.source === 'certbot'
}

/** 不能续期的原因（一句话）；能续期返回 null。 */
export function canRenewCertificateReason(cert: Pick<CertificateOption, 'source'>): string | null {
  if (cert.source === 'certbot') return null
  return '手动导入的证书：certbot 续不了它，指向它的续期计划只会每晚失败一次——证书要换得手动重新导入'
}

// ---------------------------------------------------------------- 翻译

/**
 * timer 状态的短说法。
 *
 * 顺序是有讲究的：先看"盘上有没有单元"（没有任何单元时，enable 状态与 active 状态
 * 都无从谈起），再看 enable，最后看此刻是否 active。三者合成一句话，但**分开**
 * 也能在详情里看到原值——这一句只是列表里的一格。
 */
export function scheduleRuntimeLabel(record: Pick<ScheduleRecord, 'installed' | 'unit_file_state' | 'active_state'>): string {
  if (!record.installed) return '单元未安装'
  const enabled = record.unit_file_state === 'enabled'
  const active = record.active_state === 'active'
  if (active && enabled) return '已启用并在运行'
  if (active && !enabled) return '在运行但没 enable（重启后不会自己起来）'
  if (!active && enabled) return '已 enable 但 timer 没在跑'
  return `已安装但没跑（${record.unit_file_state} / ${record.active_state}）`
}

/**
 * 触发结果的短说法。闭集来自 internal/schedules/record.go：
 * `triggered`（真的提交了操作）、`blocked`（被拦，原因在 reason 里）、
 * `duplicate`（同一时刻已经触发过）、`failed`（提交失败）。
 */
export const TRIGGER_OUTCOME_LABELS: Record<string, string> = {
  triggered: '已提交操作',
  blocked: '被拦下',
  duplicate: '跳过（同一时刻已触发过）',
  failed: '提交失败',
}

export function triggerOutcomeLabel(outcome: string): string {
  return TRIGGER_OUTCOME_LABELS[outcome] ?? outcome
}

/** 时长的中文说法（秒 → "90 秒" / "2 小时" / "1 小时 30 分"）。 */
export function runtimeLabel(seconds: number): string {
  if (!Number.isFinite(seconds) || seconds <= 0) return '—'
  if (seconds < 60) return `${seconds} 秒`
  const minutes = Math.floor(seconds / 60)
  if (minutes < 60) return `${minutes} 分钟`
  const hours = Math.floor(minutes / 60)
  const restMinutes = minutes % 60
  return restMinutes === 0 ? `${hours} 小时` : `${hours} 小时 ${restMinutes} 分`
}
