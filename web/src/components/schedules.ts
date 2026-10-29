// 计划任务与脚本页的纯逻辑（P12.5）：表单状态 ↔ 定义、形状校验、确认文案。
//
// 一条边界写在这里：这里的日历校验只查**形状**（长度、字符集、句首不是 -）。
// 表达式到底合不合法只有 `systemd-analyze calendar` 说了算，那是预检的事
// （SCHEDULE_CALENDAR_INVALID）。所以"校验通过"不等于"能落地"，按钮永远等预检。
//
// 另一条同样是硬规矩：**时区与运行时长必须由人给**，不猜默认值。时区猜错会让
// 一个"每天凌晨 3 点"的备份跑在别人的凌晨 3 点；运行时长猜大了会让一个挂住的
// 备份占着定时器到明天。

import {
  MAX_CALENDAR_LENGTH,
  MAX_RUNTIME_SECONDS,
  MAX_SCHEDULE_NAME_LENGTH,
  MAX_TIMEZONE_LENGTH,
  SCHEDULE_TARGET_PATTERNS,
  type ScheduleDefinition,
  type ScheduleKind,
  type ScheduleRecord,
  type ScriptRecord,
  runtimeLabel,
  scheduleRuntimeLabel,
} from '@/api/schedules'

/** OnCalendar 表达式允许出现的字符集（与服务端的白名单同形）。 */
const CALENDAR_CHARS = /^[A-Za-z0-9 :*.,/+~-]+$/
/** IANA 时区名：字母开头，只含字母、数字与 `_ + - /`。 */
const TIMEZONE_CHARS = /^[A-Za-z][A-Za-z0-9_+/-]*$/

/** 脚本正文上限（CONTRACTS §12：64 KiB）。 */
export const MAX_SCRIPT_BYTES = 64 << 10
/** 脚本名的字符数上限。 */
export const MAX_SCRIPT_NAME = 80
/** script.run 的单次时长上限（24 小时）。 */
export const DEFAULT_SCRIPT_RUNTIME_SECONDS = 600

export interface ScheduleFormState {
  name: string
  kind: ScheduleKind
  targetId: string
  /** 证书续期用：域名与 ACME 环境会随定义一起存进代理库。 */
  certificateDomain: string
  certificateStaging: boolean
  calendarExpression: string
  timezone: string
  persistent: boolean
  /** 输入框里的字符串，提交时再转数字。 */
  maxRuntimeSeconds: string
  enabled: boolean
}

/** 浏览器所在时区（无法判定时返回空串，交由人自己填）。 */
export function browserTimezone(): string {
  try {
    const zone = Intl.DateTimeFormat().resolvedOptions().timeZone
    return typeof zone === 'string' ? zone : ''
  } catch {
    return ''
  }
}

export function emptyScheduleForm(kind: ScheduleKind = 'backup'): ScheduleFormState {
  return {
    name: '',
    kind,
    targetId: '',
    certificateDomain: '',
    certificateStaging: false,
    calendarExpression: '*-*-* 03:30:00',
    timezone: browserTimezone(),
    persistent: false,
    maxRuntimeSeconds: '3600',
    enabled: true,
  }
}

/** 把一条已有计划摊回表单。 */
export function scheduleFormOf(record: ScheduleRecord): ScheduleFormState {
  const definition = record.definition
  return {
    name: definition.name,
    kind: definition.kind,
    targetId: definition.target_id,
    certificateDomain: definition.certificate?.domain ?? '',
    certificateStaging: definition.certificate?.staging ?? false,
    calendarExpression: definition.calendar_expression,
    timezone: definition.timezone,
    persistent: definition.persistent,
    maxRuntimeSeconds: String(definition.max_runtime_seconds),
    enabled: definition.enabled,
  }
}

/**
 * 表单 → 定义。
 *
 * `certificate` 只在证书续期时出现：别的种类带上它会被服务端**拒绝**，
 * 而不是忽略——一份带着证书参数的备份计划说明有人在错误的种类上改了半个定义。
 */
export function buildScheduleDefinition(state: ScheduleFormState): ScheduleDefinition {
  const definition: ScheduleDefinition = {
    name: state.name.trim(),
    kind: state.kind,
    target_id: state.targetId.trim(),
    calendar_expression: state.calendarExpression.trim().replace(/\s+/g, ' '),
    timezone: state.timezone.trim(),
    persistent: state.persistent,
    max_runtime_seconds: Number.parseInt(state.maxRuntimeSeconds.trim(), 10) || 0,
    enabled: state.enabled,
  }
  if (state.kind === 'certificate_renewal') {
    definition.certificate = {
      domain: state.certificateDomain.trim().toLowerCase(),
      staging: state.certificateStaging,
    }
  }
  return definition
}

export function validateScheduleForm(state: ScheduleFormState): string[] {
  const problems: string[] = []
  const name = state.name.trim()
  if (name === '') problems.push('计划名不能为空')
  else if ([...name].length > MAX_SCHEDULE_NAME_LENGTH) {
    problems.push(`计划名不能超过 ${MAX_SCHEDULE_NAME_LENGTH} 个字符`)
  }

  const targetId = state.targetId.trim()
  const pattern = SCHEDULE_TARGET_PATTERNS[state.kind]
  if (targetId === '') problems.push(targetKindHint(state.kind))
  else if (!pattern.test(targetId)) problems.push(targetKindHint(state.kind))

  if (state.kind === 'certificate_renewal') {
    if (state.certificateDomain.trim() === '') {
      problems.push('证书续期要写明域名：触发发生在 systemd 里，那一刻没有面板可问，域名只能随定义存下来')
    }
  }

  const calendar = state.calendarExpression.trim()
  if (calendar === '') problems.push('日历表达式不能为空（systemd 的 OnCalendar 表达式，例如 «*-*-* 03:30:00»）')
  else if (calendar.length > MAX_CALENDAR_LENGTH) {
    problems.push(`日历表达式不能超过 ${MAX_CALENDAR_LENGTH} 字节`)
  } else if (calendar.startsWith('-')) {
    problems.push('日历表达式不能以 - 开头（会被 systemd-analyze 当成选项）')
  } else if (!CALENDAR_CHARS.test(calendar)) {
    problems.push('日历表达式含非法字符（只允许字母、数字、空格与 :*.,/+~-）')
  }

  const timezone = state.timezone.trim()
  if (timezone === '') problems.push('必须给出时区：猜一个默认值会让计划跑在别人的凌晨')
  else if (timezone.length > MAX_TIMEZONE_LENGTH) problems.push(`时区名不能超过 ${MAX_TIMEZONE_LENGTH} 字节`)
  else if (!TIMEZONE_CHARS.test(timezone)) problems.push('时区名只允许 IANA 名的字符（字母、数字与 _ + - /）')

  const runtime = Number.parseInt(state.maxRuntimeSeconds.trim(), 10)
  if (Number.isNaN(runtime) || runtime < 1 || runtime > MAX_RUNTIME_SECONDS) {
    problems.push(`单次运行上限必须是 1—${MAX_RUNTIME_SECONDS} 秒之间的整数（${runtimeLabel(MAX_RUNTIME_SECONDS)}封顶）`)
  }
  return problems
}

function targetKindHint(kind: ScheduleKind): string {
  switch (kind) {
    case 'backup':
      return '备份计划要选一条备份策略（pol_ 加 32 位小写十六进制）'
    case 'certificate_renewal':
      return '证书续期计划要选一张证书（crt_ 加 32 位小写十六进制）'
    default:
      return '脚本计划要选一条已登记的脚本（scr_ 加 32 位小写十六进制）'
  }
}

/** 常用的几条日历表达式（只是快捷输入，填进去之后仍可手改）。 */
export const CALENDAR_PRESETS: readonly { label: string; expression: string }[] = [
  { label: '每天 03:30', expression: '*-*-* 03:30:00' },
  { label: '每天 04:00 与 16:00', expression: '*-*-* 04,16:00:00' },
  { label: '每周一 02:00', expression: 'Mon *-*-* 02:00:00' },
  { label: '工作日 06:00', expression: 'Mon..Fri *-*-* 06:00:00' },
  { label: '每月 1 日 01:00', expression: '*-*-01 01:00:00' },
  { label: '每小时整点', expression: '*-*-* *:00:00' },
]

/** 这条表达式命中哪一条预设（没有就返回 null，表示是手写的）。 */
export function calendarPresetOf(expression: string): string | null {
  const normalized = expression.trim().replace(/\s+/g, ' ')
  const hit = CALENDAR_PRESETS.find((preset) => preset.expression === normalized)
  return hit === undefined ? null : hit.label
}

// ---------------------------------------------------------------- 脚本

export interface ScriptFormState {
  name: string
  content: string
}

export function emptyScriptForm(): ScriptFormState {
  return {
    name: '',
    content: '#!/bin/sh\n# 以 upanel-shell 身份运行，永远拿不到 root。输出不落库，需要时写日志文件。\nset -eu\n\n',
  }
}

/** 脚本正文的字节数（按 UTF-8 计，与服务端的上限同一个口径）。 */
export function scriptByteLength(content: string): number {
  return new TextEncoder().encode(content).length
}

export function validateScriptForm(state: ScriptFormState): string[] {
  const problems: string[] = []
  const name = state.name.trim()
  if (name === '') problems.push('脚本名不能为空')
  else if ([...name].length > MAX_SCRIPT_NAME) problems.push(`脚本名不能超过 ${MAX_SCRIPT_NAME} 个字符`)
  if (state.content.trim() === '') problems.push('脚本正文不能为空')
  if (scriptByteLength(state.content) > MAX_SCRIPT_BYTES) {
    problems.push(`脚本正文不能超过 ${MAX_SCRIPT_BYTES / 1024} KiB（当前 ${(scriptByteLength(state.content) / 1024).toFixed(1)} KiB）`)
  }
  if (state.content.includes('\u0000')) problems.push('脚本正文不能含 NUL 字节')
  return problems
}

// ---------------------------------------------------------------- 确认文案

/** 保存一条脚本之前给人看的几行。 */
export function scriptApplyConfirmLines(
  state: ScriptFormState,
  existing?: Pick<ScriptRecord, 'id'>,
): string[] {
  const bytes = scriptByteLength(state.content)
  return [
    existing === undefined ? `新建脚本「${state.name.trim()}」` : `修改脚本「${state.name.trim()}」`,
    `正文 ${bytes} 字节（上限 ${MAX_SCRIPT_BYTES / 1024} KiB）`,
    '解释器由代理决定，调用方指定不了：她按 shebang 选，选不出来就拒绝',
    '脚本以 upanel-shell 身份运行，永远拿不到 root，也没有 sudo',
  ]
}

/** 删除一条脚本之前给人看的几行。 */
export function scriptDeleteConfirmLines(record: Pick<ScriptRecord, 'name'>): string[] {
  return [
    `删除脚本「${record.name}」`,
    '指向它的计划任务不会被自动删除：删之前先确认没有计划还挂在它上面，否则那条计划每次触发都会失败',
    '已经跑出来的输出本来就不落库，删掉脚本也就没有别处可以找回来',
  ]
}

/** 保存一条计划之前给人看的几行。 */
export function scheduleApplyConfirmLines(
  state: ScheduleFormState,
  targetName: string,
  existing?: Pick<ScheduleRecord, 'id'>,
): string[] {
  const lines = [
    existing === undefined
      ? `新建计划「${state.name.trim()}」`
      : `修改计划「${state.name.trim()}」`,
    `种类：${kindLabelOf(state.kind)}${targetName === '' ? '' : ` → ${targetName}`}`,
    `日历：${state.calendarExpression.trim()}（时区 ${state.timezone.trim() || '未填'}）`,
    state.persistent
      ? '停机期间错过的触发会在开机后补跑一次'
      : '停机期间错过的触发不补跑',
    `单次运行上限：${runtimeLabel(Number.parseInt(state.maxRuntimeSeconds.trim(), 10) || 0)}`,
    state.enabled
      ? '保存后会 enable 并启动 timer：到点就真的会跑'
      : '保存后不 enable：单元在盘上，但 timer 不会自己起来',
  ]
  if (state.kind === 'certificate_renewal') {
    lines.push(
      `ACME 环境：${state.certificateStaging ? 'staging（测试环境，签出来的证书浏览器不认）' : '正式环境'}`,
      '定时续期用 --keep-until-expiring：离到期还早就什么都不做，不会每晚重签',
    )
  }
  return lines
}

function kindLabelOf(kind: ScheduleKind): string {
  switch (kind) {
    case 'backup':
      return '定时备份'
    case 'certificate_renewal':
      return '证书续期'
    default:
      return '定时运行脚本'
  }
}

/** 删除一条计划之前给人看的几行。 */
export function scheduleDeleteConfirmLines(record: Pick<ScheduleRecord, 'name' | 'timer_ref'>): string[] {
  return [
    `删除计划「${record.name}」：先停 timer 与 service，再删记录`,
    `单元文件 ${record.timer_ref} 与它的 .service 会一起被移除`,
    '目标对象（策略、证书、脚本）不会跟着被删',
  ]
}

/** 立即运行一条脚本之前给人看的几行。 */
export function scriptRunConfirmLines(
  script: { name: string; content_hash: string },
  maxRuntimeSeconds: number,
): string[] {
  return [
    `以 upanel-shell 身份运行「${script.name}」（不是 root，没有 sudo）`,
    `运行前会核一次内容指纹：${script.content_hash}`,
    `时长上限 ${runtimeLabel(maxRuntimeSeconds)}，到点整棵进程组被杀掉`,
    '输出不落库：需要留证据就在脚本里写日志文件',
  ]
}

/** 一条计划的现状摘要（详情页顶部那几行）。 */
export function scheduleStateLines(record: ScheduleRecord): string[] {
  const lines = [scheduleRuntimeLabel(record)]
  if (record.next_elapse !== undefined && record.next_elapse !== '') {
    lines.push(`下次触发：${record.next_elapse}`)
  } else {
    lines.push('下次触发：读不到（timer 没在跑，或者 systemd 还没排下一次）')
  }
  lines.push(`单元：${record.timer_ref}`)
  if (record.blockers.length > 0) {
    lines.push(`有 ${record.blockers.length} 条阻塞项：这条计划此刻就算到点也不会执行`)
  }
  return lines
}
