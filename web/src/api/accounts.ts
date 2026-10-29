// 受管理 Linux 账号、SSH 公钥与受限 sudo 模板（P14.5）。
//
// 与 api/openapi.yaml 的 ManagedAccount / ManagedAccountSSHKey 逐字对应，语义来源是
// docs/CONTRACTS.md §13.8。
//
// 这一层只做四件事：取数据、把状态翻成人话、给闭集（壳档、sudo 模板）配标签、
// 构造六个动作的**参数**。判断“能不能做”不在这里：`account.apply` 能不能落下去取决于
// 名字有没有被占、uid 落不落在 1000–59999、home 是不是符号链接——那是现场的事，
// 一条预检说了算。这里连“看起来能不能”都不猜。
//
// 三条必须写在前面的口径：
//
//   1) **账号列表里有两种 type。** `share` 是给 Samba 认证用的系统账号，
//      `regular` 是人能登录的普通账号。它们在同一张表、同一条接口里，但写走两条动作：
//      共享账号是 `share.account.apply`，普通账号是 `account.apply`。页面不能因为它们
//      长得一样就把按钮合并——那会把两条不同的读回核验混成一个。
//   2) **`shell` 与 `shell_profile` 不是一回事。** 前者是系统上此刻的壳，后者是
//      “启用之后该回到哪一档”。禁用一个账号时 shell 变成 nologin，profile 仍然是
//      restricted——否则启用就无从下手。页面必须两个都显示，只显示一个就会让人以为
//      “这个账号本来就是 nologin”。
//   3) **公钥列表读的是登记表，不是 authorized_keys。** 文件里此刻有没有这些行属于
//      执行器的读回核验；两件事分开问，因为它们的差距恰恰是要给人看的。

import { get, request, type ApiResult } from './client'
import type { ShareNotice } from './shares'

// ---------------------------------------------------------------- 阻塞项与告警

/**
 * 账号模块的阻塞码与告警码 → 中文短名。
 *
 * 与 SHARE_NOTICE_LABELS 分开维护：两张表有交集（ACCOUNT_NOT_MISSING 之类），
 * 但账号这边的码更多是从 getent / passwd / visudo 的现场读出来的，混进共享那张表
 * 会让共享页显示一堆它永远不会遇到的名字。
 *
 * 表里没有的码原样显示——未知判决不该被静默吞掉。
 */
export const ACCOUNT_NOTICE_LABELS: Record<string, string> = {
  ACCOUNT_NAME_INVALID: '账号名不合法',
  ACCOUNT_RESERVED: '这是系统保留名',
  ACCOUNT_TYPE_INVALID: '类型不合法',
  ACCOUNT_SHELL_PROFILE_INVALID: '壳档不合法',
  ACCOUNT_DISPLAY_NAME_INVALID: '展示名不合法',
  ACCOUNT_SECRET_INVALID: '密码引用无效',
  ACCOUNT_NAME_TAKEN: '系统里已有同名账号',
  ACCOUNT_NOT_MANAGED: '不是面板创建的账号',
  ACCOUNT_MISSING: '账号已不存在',
  ACCOUNT_UID_OUT_OF_RANGE: 'uid 不在 1000–59999',
  ACCOUNT_HAS_SHARES: '账号仍被共享引用',
  ACCOUNT_LOOKUP_TIMEOUT: '查账号超时',
  ACCOUNT_NO_PASSWORD: '没有设置口令',
  SSH_KEY_INVALID: '公钥格式不合法',
  SSH_KEY_DUPLICATE: '这把钥匙已经配过',
  SSH_KEY_TOO_MANY: '公钥数量已达上限',
  SSH_KEY_NOT_FOUND: '没有这把公钥',
  SSH_KEY_HOME_INVALID: 'home 目录不可安全写入',
  SSH_KEY_BLOCK_DAMAGED: '托管区标记不完整',
  SUDO_TEMPLATE_INVALID: 'sudo 模板不在闭集里',
  SUDO_ACCOUNT_NOT_ELIGIBLE: '这个账号不能拿 sudo',
  SUDO_RULE_MISSING: '没有这条规则',
  SUDO_FILE_DAMAGED: 'sudoers 文件不属于面板',
  SUDO_VISUDO_FAILED: 'visudo 校验没通过',
  SUDO_VISUDO_UNAVAILABLE: '本机没有 visudo',
  ACCOUNTS_UNSUPPORTED: '本机不支持账号管理',
  ACCOUNT_SHELL_MISSING: '缺少这一档壳',
}

/** 取一条条目的短标签；未知码原样返回。 */
export function accountNoticeLabel(code: string): string {
  return ACCOUNT_NOTICE_LABELS[code] ?? code
}

// ---------------------------------------------------------------- 记录

/** 账号类型闭集。 */
export const ACCOUNT_TYPES = ['regular', 'share'] as const
export type AccountType = (typeof ACCOUNT_TYPES)[number]

export const ACCOUNT_TYPE_LABELS: Record<AccountType, string> = {
  regular: '普通账号',
  share: '共享账号',
}

/**
 * 一条受管理账号（GET /accounts 的元素）。
 *
 * 没有密码、没有哈希、没有 secret_ref：DTO 上就没有那些字段，代理也不会带出来。
 * `has_password` 只回答“设过没有”。
 */
export interface ManagedAccount {
  id: string
  name: string
  display_name: string
  type: AccountType
  managed: boolean
  disabled: boolean
  has_password: boolean
  uid: number
  gid: number
  /** 此刻系统上的登录壳（禁用后是 nologin）。 */
  shell: string
  /** 期望的那一档（disabled/restricted）；空串按 disabled 读。 */
  shell_profile: string
  home: string
  version: string
  last_operation_id?: string
  created_at: string
  updated_at: string
}

export interface ManagedAccountListPayload {
  capability: AccountCapability
  accounts: ManagedAccount[]
}

/** 一条已登记的公钥。**不含密钥体**：Blob 在代理的 DTO 上是 json:"-"。 */
export interface ManagedAccountSSHKey {
  id: string
  account_id: string
  /** 算法名（ssh-ed25519 / ssh-rsa / ecdsa-sha2-nistp256…）。 */
  type: string
  /** 公钥行尾的注释（alice@laptop），可以为空。 */
  comment: string
  /** OpenSSH 的 SHA256 指纹；“是不是同一把钥匙”的唯一判据。 */
  fingerprint: string
  created_at: string
}

export interface ManagedAccountKeysPayload {
  account_id: string
  ssh_keys: ManagedAccountSSHKey[]
}

// ---------------------------------------------------------------- 壳档

export const SHELL_PROFILES = ['disabled', 'restricted'] as const
export type ShellProfile = (typeof SHELL_PROFILES)[number]

export const SHELL_PROFILE_LABELS: Record<ShellProfile, string> = {
  disabled: '禁用登录（nologin）',
  restricted: '受限登录（rbash）',
}

/**
 * 记录里的那一档壳；空串按 `disabled` 读。
 *
 * 空串是旧记录（P14.5 之前建的共享账号）的形态：代理在写入时已经归一化成 disabled，
 * 但一条从库里读出来的旧行仍可能没有这个字段。按 disabled 读是安全的默认——
 * 把 nologin 读成 restricted 会让人以为它本来能登录。
 */
export function accountShellProfile(account: ManagedAccount): ShellProfile {
  return account.shell_profile === 'restricted' ? 'restricted' : 'disabled'
}

/** 给人看的名字：有展示名就用展示名，否则用账号名。 */
export function accountDisplayLabel(account: ManagedAccount): string {
  const display = account.display_name.trim()
  return display === '' ? account.name : display
}

/**
 * 账号名的形状判定，与 `internal/accounts/definition.go` 的 `ValidateName` 同一套。
 *
 * 三条细节都不能省：**下划线开头**会和 `_apt`、`_chrony` 这类系统账号混在同一个
 * 名字空间里；**连字符结尾**和**超过 32 字节**`useradd` 会拒。
 *
 * 它只是让“新建”按钮在明显填错时就是灰的，**不是**判据：保留名（root、www-data…）
 * 只在代理那边有一份，这里不复制——复制一份就等于多一处会漂移的真值。代理带着
 * `ACCOUNT_NAME_INVALID` / `ACCOUNT_RESERVED` 回来时页面照原样显示。
 */
export function accountNameProblem(name: string): string | null {
  if (name === '') return '账号名不能为空'
  if (name.length > 32) return '账号名过长（上限 32 个字符）'
  if (!/^[a-z]/.test(name)) {
    return '账号名要以小写字母开头；下划线开头会和 _apt、_chrony 这类系统账号混在同一个名字空间里'
  }
  if (!/^[a-z][a-z0-9_-]*$/.test(name)) {
    return '账号名只能包含小写字母、数字、下划线、连字符'
  }
  if (name.endsWith('-')) return '账号名不能以连字符结尾'
  return null
}

// ---------------------------------------------------------------- 状态

export interface AccountState {
  kind: 'regular' | 'share' | 'disabled' | 'no_password'
  label: string
  detail: string
  tone: 'success' | 'info' | 'warning' | 'error' | 'default'
}

/**
 * 一条账号此刻该显示成什么状态。
 *
 * 顺序不是随意的：**禁用最优先**（它决定“能不能登录”，比有没有口令更基本），然后是
 * 共享账号、没有口令，最后才是普通账号的常态。一个禁用且没有口令的共享账号会显示成
 * “已禁用”，因为那才是此刻的真相；其余两条事实在字段区里照样逐条写着。
 *
 * `ACCOUNT_NO_PASSWORD` 的口径是**真机验过的**（Q479）：publickey 认证不看 shadow 的
 * 口令字段，一个没有任何口令、只配了公钥的账号（key-only 账号）照样能登进来。所以这里
 * 说的是“只能靠公钥登录”，不是首轮那句反了的“配了公钥也登不上”。真正拦人的是壳：
 * 禁用档把壳换成 nologin，那一刀切下去立刻登不上。
 */
export function summarizeAccountState(account: ManagedAccount): AccountState {
  if (account.disabled) {
    return {
      kind: 'disabled',
      label: '已禁用',
      detail:
        account.shell_profile === 'restricted'
          ? '登录壳已换成 /usr/sbin/nologin；启用之后会回到 /bin/rbash。已配的公钥仍在，但这个账号此刻登不上。'
          : '登录壳是 /usr/sbin/nologin：这个账号此刻登不上。',
      tone: 'default',
    }
  }
  if (account.type === 'share') {
    return {
      kind: 'share',
      label: '共享账号',
      detail: '只用于 Samba 认证：系统段 uid、登录壳 nologin、home 指向 /nonexistent。它不是一个能登录的账号。',
      tone: 'info',
    }
  }
  if (!account.has_password) {
    return {
      kind: 'no_password',
      label: '没有口令',
      // Q479 真机读数：没有口令不等于登不上。别把“没设口令”说成“进不去”。
      detail: '这个账号还没有设置口令，只能用已配的公钥登录。没有口令本身不拦人；要拦住它，把账号禁用（壳会换成 nologin）。',
      tone: 'warning',
    }
  }
  return {
    kind: 'regular',
    label: '普通账号',
    detail: `uid ${account.uid}，登录壳 ${account.shell}，home ${account.home}。`,
    tone: 'success',
  }
}

/** 一条记录能不能在账号页上被改：共享账号的写走共享那条动作。 */
export function editableAsRegular(account: ManagedAccount): boolean {
  return account.type === 'regular'
}

function formatTime(value: string): string {
  if (!value) return '未知时间'
  const d = new Date(value)
  if (Number.isNaN(d.getTime())) return value
  return d.toLocaleString('zh-CN', { hour12: false })
}

export { formatTime as formatAccountTime }

// ---------------------------------------------------------------- sudo 模板

export interface SudoTemplate {
  id: string
  display_name: string
  purpose: string
  /** 逐条对应 sudoers 里的一行。没有通配符，也不接受单元名参数。 */
  commands: string[]
}

/**
 * sudo 模板闭集（与 internal/accounts/sudo.go 的 sudoTemplates 一一对应）。
 *
 * 两份表会漂移，所以页面**不**用它来决定“这个模板存在吗”——预检会拒绝闭集之外的
 * 值，判据在代理那边。这里只用来把 id 翻成人话、把命令列出来给人看。真值以代理为准：
 * 它回了 `SUDO_TEMPLATE_INVALID` 就是不行，页面不会因为自己表里有就去争。
 */
export const SUDO_TEMPLATES: readonly SudoTemplate[] = [
  {
    id: 'system_status',
    display_name: '查看整机状态',
    purpose: '只读：systemctl status，不加 --lines 之外的任何参数',
    commands: ['/usr/bin/systemctl status --no-pager'],
  },
  {
    id: 'journal_recent',
    display_name: '查看最近日志',
    purpose: '只读：journalctl 最近 200 行，不落盘、不过滤单元',
    commands: ['/usr/bin/journalctl -n 200 --no-pager'],
  },
]

/** 模板的中文名；未知 id 原样返回。 */
export function sudoTemplateLabel(id: string): string {
  return SUDO_TEMPLATES.find((t) => t.id === id)?.display_name ?? id
}

// ---------------------------------------------------------------- 读接口

export function getAccounts(): Promise<ApiResult<ManagedAccountListPayload>> {
  return get<ManagedAccountListPayload>('/accounts')
}

export function getAccountKeys(id: string): Promise<ApiResult<ManagedAccountKeysPayload>> {
  return get<ManagedAccountKeysPayload>(`/accounts/${encodeURIComponent(id)}/ssh-keys`)
}

// ---------------------------------------------------------------- 秘密通道

/**
 * 账号口令的用途名。
 *
 * 与 `smb_password` 是两个不同的用途：代理侧的用途闭集里它们是两条（各 4 KiB 上限），
 * 混用会让审计里分不清“这次设的是登录口令还是 Samba 口令”。明文同样只进不出。
 */
export const SECRET_PURPOSE_ACCOUNT_PASSWORD = 'account_password'

export interface AccountSecretCreated {
  secret_ref: string
  purpose: string
  state: string
  expires_at?: string
}

/**
 * 提交一份账号口令，拿回引用（`sec_` 开头）。
 *
 * 明文只走这一次请求：响应里没有 value、没有 hash；页面拿到引用后立刻丢弃明文，
 * 任务参数里只有引用。`chpasswd` 从标准输入读明文，不进 argv、不进日志。
 */
export function createAccountSecret(
  value: string,
  reference?: string,
): Promise<ApiResult<AccountSecretCreated>> {
  return request<AccountSecretCreated>('/secrets', {
    method: 'POST',
    body: {
      purpose: SECRET_PURPOSE_ACCOUNT_PASSWORD,
      value,
      ...(reference ? { reference } : {}),
    },
  })
}

// ---------------------------------------------------------------- 动作与参数

/**
 * 六个动作名（CONTRACTS §13.8）。
 *
 * 页面用它们去要预检，不直接调代理。
 */
export const ACCOUNT_ACTIONS = {
  apply: 'account.apply',
  delete: 'account.delete',
  keyApply: 'ssh_key.apply',
  keyDelete: 'ssh_key.delete',
  sudoApply: 'sudo_rule.apply',
  sudoDelete: 'sudo_rule.delete',
} as const

/** 新建账号的表单（页面填的）。 */
export interface AccountForm {
  name: string
  display_name: string
  shell_profile: ShellProfile
  disabled: boolean
  /** 密码密文的引用（POST /secrets 的返回）。空串表示这次不设口令。 */
  password_secret_ref: string
}

/** 账号定义（发给代理的 definition）。**没有 uid/gid/groups/home**：那四项由系统分配。 */
export interface ManagedAccountDefinition {
  name: string
  display_name: string
  type: 'regular'
  disabled: boolean
  password_secret_ref?: string
  shell_profile: ShellProfile
}

export function buildAccountDefinition(form: AccountForm): ManagedAccountDefinition {
  const name = form.name.trim()
  const display = form.display_name.trim()
  const ref = form.password_secret_ref.trim()
  return {
    name,
    display_name: display,
    type: 'regular',
    disabled: form.disabled,
    shell_profile: form.shell_profile,
    // 空串是“这次不设口令”，不是“把口令改成空”。省略字段让代理按“没给”处理。
    ...(ref === '' ? {} : { password_secret_ref: ref }),
  }
}

/**
 * `account.apply` 的参数。
 *
 * `account_id` 为 null 才是**新建**；给了 id 是改一条已存在的账号，此时必须带
 * `version`——它是并发校验，不是可选填的备注。
 */
export function accountApplyParams(
  definition: ManagedAccountDefinition,
  target?: { id: string; version: string } | null,
): Record<string, unknown> {
  return {
    account_id: target ? target.id : null,
    version: target ? target.version : null,
    definition,
  }
}

export function accountDeleteParams(account: ManagedAccount): Record<string, unknown> {
  return { account_id: account.id, version: account.version }
}

/**
 * `ssh_key.apply` 的参数。
 *
 * `key_id` 给 null 是**新增一把**；给了一个已有的 id 是**替换**那一条的公钥内容
 * （同一个 id、新指纹），不会在账号上留两把。`version` 同样可选：新建时账号的版本
 * 用来做并发校验，代理的判决层会给出当前值。
 */
export function sshKeyApplyParams(
  account: ManagedAccount,
  publicKey: string,
  replace?: ManagedAccountSSHKey | null,
): Record<string, unknown> {
  return {
    account_id: account.id,
    version: account.version,
    key_id: replace ? replace.id : null,
    public_key: publicKey,
  }
}

export function sshKeyDeleteParams(
  account: ManagedAccount,
  key: ManagedAccountSSHKey,
): Record<string, unknown> {
  return { account_id: account.id, version: account.version, key_id: key.id }
}

export function sudoRuleApplyParams(
  account: ManagedAccount,
  templateId: string,
): Record<string, unknown> {
  return { account_id: account.id, version: account.version, template_id: templateId }
}

export function sudoRuleDeleteParams(account: ManagedAccount): Record<string, unknown> {
  return { account_id: account.id, version: account.version }
}

// ---------------------------------------------------------------- 能力

/** 探到的工具与壳的绝对路径；缺的项不出现（omitempty）。 */
export interface AccountTools {
  useradd?: string
  usermod?: string
  userdel?: string
  getent?: string
  passwd?: string
  chpasswd?: string
  visudo?: string
  restricted_shell?: string
  disabled_shell?: string
}

/**
 * 账号页要读的那份能力报告（GET /accounts 的 `capability`）。
 *
 * **这不是 ShareCapability。** 两者的判据完全不相交：共享那份问的是
 * smbd / smbpasswd / pdbedit 在不在，账号这份问的是
 * useradd / usermod / userdel / getent / passwd / chpasswd，外加两档壳与 visudo。
 * 曾经借用过共享那一份，结果是一台没装 Samba 的机器上账号页显示“不支持账号管理”、
 * 新建按钮是灰的——而建普通账号一个 Samba 工具都不需要。所以形状分开定义，
 * 页面也分开判断。
 */
export interface AccountCapability {
  status: 'available' | 'blocked' | (string & {})
  reason?: string
  tools: AccountTools
  /** sudo 规则能不能下发：`available` / `unsupported`（缺 visudo）。 */
  sudo_rule_write: string
  /** 这台机器上真的能落的壳档（disabled / restricted 的子集）。 */
  shell_profiles: string[]
  notices: ShareNotice[]
  collected_at: string
}

/**
 * 账号能力是否可用。
 *
 * `status=blocked` 时页面必须把 `reason` 原样显示——一个灰掉的“新建账号”按钮
 * 后面没有原因，人只会以为面板坏了。
 */
export function accountsAvailable(capability: AccountCapability | null | undefined): boolean {
  return capability?.status === 'available'
}

/** sudo 规则那一半是否可写（缺 visudo 时不可写，但账号照样能管）。 */
export function accountSudoWritable(capability: AccountCapability | null | undefined): boolean {
  return capability?.sudo_rule_write === 'available'
}

/** 能力报告里的 notices 统一转成页面用的形状。 */
export function accountCapabilityNotices(capability: AccountCapability | null | undefined): ShareNotice[] {
  return capability?.notices ?? []
}
