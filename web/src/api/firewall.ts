// 防火墙写路径的 DTO 与参数构造（P14.3）。
//
// 与 network.ts 的分工：那一份管地址（`network.apply`），这一份管规则集
// （`firewall.apply`）。两者共用同一套纪律（预检 → 任务 → 回滚窗口 → 读回核验），
// 但**不共用代码**，因为规则集这一侧有两个地址那边不存在的性质：
//
//   1) 面板只碰**带自己标记的规则**（`comment upanel:<id>`）。这份 DTO 里的
//      `rules` 因此只是"面板管的那些"，不是"这台机器上的全部规则"。管理员手写的
//      规则仍然在 `GET /firewall` 里完整可见——把它从这里也拿一份过来，页面就会
//      让人以为那些可以在这里删掉，而实际后果是"删不掉"或者更糟。
//   2) 提交的是**整份规则集**，不是一条。apply 的语义是"加缺的、删多的"，
//      所以少了任何一条都等于删掉它。表单必须一开始就拿全部规则铺满，
//      而不是从空白开始填。
//
// 三条不能在前端含糊的事：
//
//   * `config_hash` 只在 `editable=true` 时出现。缺了它就不能提交——提交必得
//     `FIREWALL_CONFIG_CHANGED`，而那个错误在页面上看起来像"有人同时在改机器"。
//   * **管理通路的判据在服务端**。这里不复制一份"SSH 端口应该有放行"的判断：
//     判断需要的是监听端口、进程名、面板绑定地址，页面看到的只有规则集，算不准。
//     复制出来的第二份判据会跟第一份漂开，而且它拦不住任何东西——真正的门在预检。
//     页面能做的只有一件事：把"启用之后一条对外放行都没有"这种**明显**的输入提前
//     说一句（见 `managementHint`），并且明说这只是提醒。
//   * 倒计时按 `deadline` 现算，不用 `seconds_left`（那是采集那一刻的数字）。
//     这条倒计时决定"还来不来得及确认"，一秒不动就是在骗人。所以这里不重写一份，
//     直接用 network.ts 里那两个函数。

import { get, type ApiResult } from './client'
import {
  NETWORK_QUERY_TIMEOUT_MS,
  countdownText,
  parseIPv4,
  type NetworkIssue,
} from './network'

// ---------------------------------------------------------------- 闭集与标签

/** 规则动作闭集，与 UFW 的四个动作一一对应。不做同义词：加了 reject，读回来还是 reject。 */
export const FIREWALL_RULE_ACTIONS = ['allow', 'deny', 'reject', 'limit'] as const

export type FirewallRuleAction = (typeof FIREWALL_RULE_ACTIONS)[number]

export const FIREWALL_RULE_ACTION_LABELS: Record<FirewallRuleAction, string> = {
  allow: '放行',
  deny: '丢弃（不回话）',
  reject: '拒绝（回话）',
  limit: '放行但限速',
}

/** 协议闭集。一条规则一个协议：`ufw allow 22` 同时开 tcp+udp，在面板里表达不出来。 */
export const FIREWALL_PROTOS = ['tcp', 'udp'] as const

export type FirewallProto = (typeof FIREWALL_PROTOS)[number]

export const FIREWALL_PROTO_LABELS: Record<FirewallProto, string> = { tcp: 'TCP', udp: 'UDP' }

/** 一次定义里允许的规则条数上限，与服务端 `MaxManagedRules` 一致。 */
export const MAX_MANAGED_RULES = 64

/** 规则标识的形状，与服务端的 `ruleIDPattern` 逐字一致。 */
const RULE_ID_PATTERN = /^[a-z0-9][a-z0-9-]{0,31}$/

/** 取标签，未知值原样透出：闭集外的值意味着服务端和前端漂了，静默吞掉只会更难查。 */
export function ruleActionLabel(action: string): string {
  return FIREWALL_RULE_ACTION_LABELS[action as FirewallRuleAction] ?? action
}

export function ruleProtoLabel(proto: string): string {
  return FIREWALL_PROTO_LABELS[proto as FirewallProto] ?? proto
}

// ---------------------------------------------------------------- DTO

/** 一条面板管理的规则。`from` 为空串表示 `from any`（服务端总是会带这个字段）。 */
export interface FirewallManagedRule {
  id: string
  action: string
  port: number
  proto: string
  from: string
}

/**
 * 一条待确认的防火墙回滚（对外视图）。
 *
 * 与 `NetworkRollbackView` 的差别不只是字段少：这里**没有 `state`**。原因是
 * `pending` 只会是 `pending` 那条——不是 pending 的记录服务端根本不返回。
 * 所以这个字段一出现，就一定是"还来得及确认"。
 */
export interface FirewallRollbackView {
  token: string
  deadline: string
  seconds_left: number
  previous: string
}

/**
 * `GET /firewall/config` 的 data：防火墙写路径的入口状态。
 *
 * `rules` 是**面板管理的**那些，不是全部。`reasons` 是 `editable=false` 的原因，
 * 页面必须逐条显示——一个灰掉的按钮后面没有原因，人只会以为面板坏了。
 */
export interface FirewallConfigState {
  collected_at: string
  manager: string
  reasons: string[]
  installed: boolean
  enabled: boolean
  editable: boolean
  rules: FirewallManagedRule[]
  config_hash?: string
  pending: FirewallRollbackView | null
  issues: NetworkIssue[]
}

/** `firewall.apply` 的参数：指纹来自这份 config，整份规则集来自表单。 */
export interface FirewallApplyParams {
  expected_config_hash: string
  definition: {
    enabled: boolean
    rules: FirewallManagedRule[]
  }
}

/**
 * `firewall.confirm` / `firewall.rollback` 的参数。
 *
 * 与网络那边不同，这里**只有 token**：防火墙改动不许把面板挡在外面（预检里的
 * `FIREWALL_MANAGEMENT_PATH` 就是这道门），所以"你是不是从新地址来"这个问题
 * 在这里不存在。两个动作共用同一个形状，就别写两个类型。
 */
export interface FirewallRollbackParams {
  token: string
}

// ---------------------------------------------------------------- 请求

/**
 * 写路径的入口状态（P14.3）。
 *
 * 超时沿用 `NETWORK_QUERY_TIMEOUT_MS`：这一侧要跑 `ufw status` 与 `ufw show added`
 * 两次本地命令，比 netplan 的全目录哈希快，没有理由给得更宽。
 */
export function fetchFirewallConfig(): Promise<ApiResult<FirewallConfigState>> {
  return get<FirewallConfigState>('/firewall/config', { timeoutMs: NETWORK_QUERY_TIMEOUT_MS })
}

// ---------------------------------------------------------------- 展示逻辑（纯函数）

/** 一条规则的一句话：`放行 22/TCP（所有来源）`。 */
export function ruleText(rule: FirewallManagedRule): string {
  const from = rule.from === '' ? '所有来源' : rule.from
  return `${ruleActionLabel(rule.action)} ${rule.port}/${ruleProtoLabel(rule.proto)}（${from}）`
}

/** 启用状态的说法。`enabled=false` 时规则存在但不生效——这句话必须说出来。 */
export function enabledText(enabled: boolean): string {
  return enabled ? '已启用' : '未启用（规则已经在了，但不生效）'
}

/** 卡片头部的一句结论。可改与不可改必须长得不一样。 */
export function firewallConfigHeadline(config: FirewallConfigState): string {
  if (!config.editable) return '当前防火墙不能由面板修改'
  const count = config.rules.length === 0 ? '面板还没有管任何规则' : `面板管着 ${config.rules.length} 条规则`
  return `可以由面板修改：UFW，${enabledText(config.enabled)}；${count}`
}

/**
 * 待确认记录的一句话状态。
 *
 * 只吃 `secondsLeft` 而不是整条记录：网络那边的 `pendingHeadline` 要拿 `interface`
 * 和 `candidate_address`，而防火墙改动没有"哪块网卡"可言（规则是全局的），
 * 所以传整条记录进来只会诱使调用方以为这里读得出更多东西。
 *
 * 与网络同一条理由：这条倒计时是"多久之后系统会把规则退回去"，必须一直在走，
 * 所以数字由调用方按 `deadline` 现算（`rollbackSecondsLeft`）。
 */
export function firewallPendingHeadline(secondsLeft: number): string {
  if (secondsLeft <= 0) {
    return '这次改动已到期待回滚：系统随时会把规则退回改之前的样子'
  }
  return `防火墙改动还有 ${countdownText(secondsLeft)} 自动退回`
}

/**
 * "启用之后一条对外放行都没有"的提前提醒——**只是提醒，不是判据**。
 *
 * 判据在服务端（`FIREWALL_MANAGEMENT_PATH`）：它知道 SSH 端口、知道面板绑在哪、
 * 也知道管理员手写的那条 `allow 22/tcp`。页面只看得见面板管的规则，所以这里
 * 只在最明显的情况下说话：要启用、且面板管理的规则里没有任何 `from any` 的放行。
 * 即便这样也可能没问题（手写规则里有），所以措辞是"可能"而不是"一定会被拦"。
 *
 * 返回 null 表示"没什么可提醒的"。**不要**拿这个函数的返回值去关按钮。
 */
export function managementHint(rules: FirewallManagedRule[], enabled: boolean): string | null {
  if (!enabled) return null
  const anyAllow = rules.some((rule) => rule.action === 'allow' && rule.from === '')
  if (anyAllow) return null
  return '面板管理的规则里没有任何"所有来源"的放行：如果管理员手写的规则里也没有 SSH 与面板端口的放行，预检会以 FIREWALL_MANAGEMENT_PATH 拦下这次改动。判据在服务端，这里只是提醒。'
}

// ---------------------------------------------------------------- 表单与参数构造

/**
 * 表单里的一行。全是字符串——输入框给的就是字符串，转成数字的地方只有一处
 * （`buildRule`），这样"22abc"之类的输入不会在两个地方各被解释一遍。
 */
export interface FirewallRuleForm {
  id: string
  action: string
  port: string
  proto: string
  from: string
}

export const EMPTY_RULE_FORM: FirewallRuleForm = {
  id: '',
  action: 'allow',
  port: '',
  proto: 'tcp',
  from: '',
}

/** 把服务端返回的一条规则铺进表单行（编辑现存规则时用）。 */
export function ruleFormFromRule(rule: FirewallManagedRule): FirewallRuleForm {
  return {
    id: rule.id,
    action: rule.action,
    port: String(rule.port),
    proto: rule.proto,
    from: rule.from,
  }
}

/**
 * 解析 `from`：空串（所有来源）、单个 IPv4、或 IPv4 CIDR。
 *
 * 归一成**掩码之后**的 CIDR，与服务端 `parseFirewallSource` 的口径一致：
 * 单个地址变成 `/32`，`10.0.0.5/24` 变成 `10.0.0.0/24`。服务端会再归一一次
 * （这里不是判据），但表单上显示的就是即将生效的那个，人改之前能看见。
 *
 * 前导零一律拒（与 `parseIPv4` 同一条理由：有人当十进制、有人当八进制）。
 */
export function parseRuleSource(text: string): string | null {
  const trimmed = text.trim()
  if (trimmed === '') return ''
  const slash = trimmed.indexOf('/')
  const addressText = slash < 0 ? trimmed : trimmed.slice(0, slash)
  const octets = parseIPv4(addressText)
  if (octets === null) return null
  let prefixLength = 32
  if (slash >= 0) {
    const raw = trimmed.slice(slash + 1)
    if (!/^\d{1,2}$/.test(raw)) return null
    prefixLength = Number(raw)
    if (!Number.isInteger(prefixLength) || prefixLength < 0 || prefixLength > 32) return null
  }
  // 掩码：主机位清零。`0.0.0.0/0` 是"所有地址"，由空串表达，不从这里走。
  const mask = prefixLength === 0 ? 0 : (0xffffffff << (32 - prefixLength)) >>> 0
  const value = ((octets[0] * 0x1000000) + (octets[1] << 16) + (octets[2] << 8) + octets[3]) >>> 0
  const masked = (value & mask) >>> 0
  const out = [(masked >>> 24) & 0xff, (masked >>> 16) & 0xff, (masked >>> 8) & 0xff, masked & 0xff]
  return `${out.join('.')}/${prefixLength}`
}

/** 一行的校验结果：要么一条能提交的规则，要么一句能显示给人看的原因。 */
export type RuleFormResult =
  | { ok: true; rule: FirewallManagedRule }
  | { ok: false; error: string }

/**
 * 把一行表单拼成一条规则。
 *
 * 只做**形状**检查，与 `buildDefinition` 同一条边界：真正的判据（规则形状是否在
 * 闭集里、有没有挡住管理通路、现场指纹对不对）都在服务端预检里。前端放过去一条
 * 坏规则不是漏洞——它会得到 `FIREWALL_DEFINITION_INVALID` 并被拦在写操作之前；
 * 前端拦下一条好规则才是问题，所以这里的检查只覆盖服务端一定也会拒的那些。
 */
export function buildRule(form: FirewallRuleForm, takenIds: readonly string[]): RuleFormResult {
  const id = form.id.trim().toLowerCase()
  if (!RULE_ID_PATTERN.test(id)) {
    return { ok: false, error: `规则标识只能用 a-z、0-9、-，最长 32 个字符（收到「${form.id}」）` }
  }
  if (takenIds.includes(id)) {
    return { ok: false, error: `规则标识 ${id} 已经有另一条规则在用了` }
  }
  const portText = form.port.trim()
  if (!/^\d{1,5}$/.test(portText)) {
    return { ok: false, error: `端口必须是 1—65535 的整数（收到「${form.port}」）` }
  }
  const port = Number(portText)
  if (port < 1 || port > 65535) {
    return { ok: false, error: `端口必须在 1—65535 之间（收到 ${port}）` }
  }
  const action = form.action.trim().toLowerCase()
  if (!(FIREWALL_RULE_ACTIONS as readonly string[]).includes(action)) {
    return { ok: false, error: `动作只有 allow/deny/reject/limit（收到「${form.action}」）` }
  }
  const proto = form.proto.trim().toLowerCase()
  if (!(FIREWALL_PROTOS as readonly string[]).includes(proto)) {
    return { ok: false, error: `协议只有 tcp/udp（收到「${form.proto}」）` }
  }
  const from = parseRuleSource(form.from)
  if (from === null) {
    return { ok: false, error: `来源必须是空的（所有来源）、IPv4 地址或 IPv4 CIDR（收到「${form.from}」）` }
  }
  return { ok: true, rule: { id, action, port, proto, from } }
}

/**
 * 拼 `firewall.apply` 的参数。
 *
 * `expected_config_hash` 只能来自这份 config：它就是"我看到的那份规则集"。
 * 拿不到（`editable=false` 或代理没给）时返回原因——页面据此把提交按钮关掉，
 * 而不是发一份注定被拒的请求。
 *
 * 整份规则集一起提交：apply 的语义是"加缺的、删多的"，少一条就等于删掉它。
 */
export function buildFirewallApplyParams(
  config: FirewallConfigState,
  forms: readonly FirewallRuleForm[],
  enabled: boolean,
): { ok: true; params: FirewallApplyParams } | { ok: false; error: string } {
  if (!config.editable || !config.config_hash) {
    return { ok: false, error: '这台机器的防火墙不能由面板修改（或者没拿到 config_hash）' }
  }
  if (forms.length > MAX_MANAGED_RULES) {
    return { ok: false, error: `规则最多 ${MAX_MANAGED_RULES} 条，现在有 ${forms.length} 条` }
  }
  const rules: FirewallManagedRule[] = []
  const seen: string[] = []
  for (const [index, form] of forms.entries()) {
    const built = buildRule(form, seen)
    if (!built.ok) {
      return { ok: false, error: `第 ${index + 1} 条规则：${built.error}` }
    }
    seen.push(built.rule.id)
    rules.push(built.rule)
  }
  return {
    ok: true,
    params: {
      expected_config_hash: config.config_hash,
      definition: { enabled, rules },
    },
  }
}

/** `firewall.confirm` / `firewall.rollback` 的参数。 */
export function buildFirewallRollbackParams(view: FirewallRollbackView): FirewallRollbackParams {
  return { token: view.token }
}

/**
 * 这次提交相对现在改了什么——给确认框上的一行行说明用。
 *
 * 只描述**面板管理的规则**：这是 apply 唯一会动的东西，说成"改防火墙"会让人以为
 * 手写规则也会被重排。
 */
export function describeFirewallChange(
  current: readonly FirewallManagedRule[],
  next: readonly FirewallManagedRule[],
  enabled: boolean,
): string[] {
  const lines: string[] = []
  const currentById = new Map(current.map((rule) => [rule.id, rule]))
  const nextById = new Map(next.map((rule) => [rule.id, rule]))
  const added = next.filter((rule) => !currentById.has(rule.id))
  const removed = current.filter((rule) => !nextById.has(rule.id))
  const changed = next.filter((rule) => {
    const before = currentById.get(rule.id)
    if (before === undefined) return false
    return (
      before.action !== rule.action ||
      before.port !== rule.port ||
      before.proto !== rule.proto ||
      before.from !== rule.from
    )
  })

  for (const rule of added) lines.push(`加：${ruleText(rule)}`)
  for (const rule of changed) {
    const before = currentById.get(rule.id)
    lines.push(`改：${rule.id} 从 ${before === undefined ? '' : ruleText(before)} 变成 ${ruleText(rule)}`)
  }
  for (const rule of removed) lines.push(`删：${ruleText(rule)}`)
  if (added.length === 0 && changed.length === 0 && removed.length === 0) {
    lines.push('面板管理的规则没有变化')
  }
  lines.push(`防火墙${enabled ? '启用' : '保持关闭'}（规则集本身不改默认策略，也不动管理员手写的规则）`)
  lines.push('120 秒内不确认，系统会自动退回改之前的样子')
  return lines
}
