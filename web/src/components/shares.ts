// 新建共享表单的纯逻辑（P08.4）。
//
// 抽出来的理由与 files.ts 一样：表单里最容易错的是校验，而校验在 SSR 里没法点。
// 这里放的是**服务端规则的复制品**，不是发明——名字字符集、相对路径形状、空账号名单
// 三条的唯一权威在 internal/shares/definition.go；这里写同一套只为少打一次必输的往返。
// 服务端仍然是最终判决者：这里从不说“可以”，只说“这些已经不对”。

import type { ShareAccess, ShareDefinition } from '@/api/shares'

/** 表单的原始输入（字符串就是人打进去的东西）。 */
export interface ShareFormState {
  name: string
  volume_id: string
  relative_path: string
  read_only: boolean
  /** 逗号、空格或换行分隔。 */
  allowed_cidrs: string
  /** account_id → access；'' 表示不授权这个账号。 */
  access: Record<string, ShareAccess | ''>
}

export function emptyShareForm(): ShareFormState {
  return {
    name: '',
    volume_id: '',
    relative_path: '',
    read_only: false,
    allowed_cidrs: '',
    access: {},
  }
}

/** Samba 自己占用的段名：拿它当共享名是往 [global] 里追加键值，不是建共享。 */
export const RESERVED_SHARE_NAMES: readonly string[] = ['global', 'homes', 'printers', 'print$', 'ipc$']

const NAME_PATTERN = /^[A-Za-z0-9][A-Za-z0-9_-]*$/
const CIDR_PATTERN = /^[0-9a-fA-F.:]+\/\d{1,3}$/

/** 名字、路径、网段的空段都要先归一再判断：多打的空格不该变成一条错误。 */
export function parseCidrs(text: string): string[] {
  return text
    .split(/[\s,]+/)
    .map((s) => s.trim())
    .filter((s) => s !== '')
}

export function selectedUsers(state: ShareFormState): { account_id: string; access: ShareAccess }[] {
  const out: { account_id: string; access: ShareAccess }[] = []
  for (const [account_id, access] of Object.entries(state.access)) {
    if (access === 'read' || access === 'write') out.push({ account_id, access })
  }
  return out
}

/** 校验并返回**全部**问题（不是第一个）：一次把话说完，人才不用试错十遍。 */
export function validateShareForm(state: ShareFormState): string[] {
  const errors: string[] = []

  const name = state.name.trim()
  if (name === '') errors.push('共享名不能为空')
  else if (name.length > 64) errors.push('共享名过长（上限 64 字节）')
  else if (RESERVED_SHARE_NAMES.includes(name.toLowerCase())) errors.push(`共享名 ${name} 是 Samba 的保留段名`)
  else if (!NAME_PATTERN.test(name)) errors.push('共享名只能包含字母、数字、下划线、连字符，且以字母或数字开头')
  else if (name.endsWith('-')) errors.push('共享名不能以连字符结尾')

  if (state.volume_id === '') errors.push('必须选一个受管理卷：共享目录只能落在卷里')

  const rel = state.relative_path.trim()
  if (rel.startsWith('/')) errors.push('相对路径不能以 / 开头（它是相对卷挂载点的）')
  else if (rel.includes('\\')) errors.push('相对路径不能包含反斜杠')
  else if (rel !== '') {
    const segments = rel.split('/')
    if (segments.some((seg) => seg === '')) errors.push('相对路径里有多余的 /')
    else if (segments.some((seg) => seg === '.' || seg === '..')) {
      errors.push('相对路径不能包含 . 或 .. 段')
    }
  }

  if (selectedUsers(state).length === 0) {
    errors.push('至少要授权一个账号（SHARE_NO_USERS）：空名单在 Samba 里会退化成 guest 访问')
  }

  for (const cidr of parseCidrs(state.allowed_cidrs)) {
    if (!CIDR_PATTERN.test(cidr)) errors.push(`来源网段格式不像 CIDR：${cidr}`)
  }
  return errors
}

/** 表单 → 契约里的 ShareDefinition（只调用方在“已经校验过”之后才该用）。 */
export function buildShareDefinition(state: ShareFormState): ShareDefinition {
  return {
    name: state.name.trim(),
    protocol: 'smb',
    volume_id: state.volume_id,
    relative_path: state.relative_path.trim(),
    enabled: true,
    read_only: state.read_only,
    allowed_cidrs: parseCidrs(state.allowed_cidrs),
    smb_users: selectedUsers(state),
  }
}
