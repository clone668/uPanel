// 网络与防火墙只读接口 DTO（P14.1）：GET /network/interfaces、/network/routes、
// /network/listeners、/firewall。
//
// 字段与 api/openapi.yaml 的 NetworkInterfaceList / NetworkRouteList /
// NetworkListenerList / FirewallState 逐字对应；语义来源是 CONTRACTS.md 第 14.1 节。
//
// 这一层是**读 + 参数**，不是"写"。改地址是 network.apply（P14.2）、改规则是
// firewall.apply（P14.3），两者都走预检 → 任务；本模块里没有"直接执行"的函数，
// 只有拼好的参数，谁都不能从这里越过去。
//
// P14.2 在这里只加了三样东西：`GET /network/config` 的 DTO（写路径的入口状态）、
// 三个写动作**参数**的构造函数，以及回滚状态的标签与倒计时。真正的执行仍然只有一条路
// ——预检 → 任务，由共用的确认框发起（见 components/NetworkApplyCard.vue）。
// 参数在这里构造而不是在页面里拼字符串：`expected_config_hash` 必须来自这份 config，
// 让表单各写一遍就迟早会有人拿旧 hash 去提交，而那只会得到 `NETWORK_CONFIG_CHANGED`。
//
// 四条纪律，每条都对应一次真机验收踩过的坑：
//
//   1) `issues[]` 非空是**降级但仍有内容**，不是整页失败。列表照常渲染，同时把
//      "哪一条来源没读到"写出来。把 issues 丢掉，页面就会说"这台机器没有监听端口"。
//   2) `capability.editable` 是代理算的判定，页面只展示不改写：`false` 时必须把
//      reasons 逐条显示——一个灰掉的按钮后面没有原因，人只会以为面板坏了。
//   3) `metric` 与 `pref_set` 成对读：0 是合法优先级，`pref_set=false` 才是"没有"。
//   4) `pid=0` 是"读不到占用进程"，不是"没有进程"。渲染成"—"并说明，不显示成空闲。

import { get, type ApiResult } from './client'

// ---------------------------------------------------------------- 闭集与标签

/** 配置归属。只有 `netplan-networkd` 可能被改。 */
export const BACKENDS = [
  'netplan-networkd',
  'netplan-networkmanager',
  'networkmanager',
  'unknown',
] as const

export type Backend = (typeof BACKENDS)[number]

export const BACKEND_LABELS: Record<Backend, string> = {
  'netplan-networkd': 'netplan + systemd-networkd',
  'netplan-networkmanager': 'netplan + NetworkManager',
  networkmanager: 'NetworkManager 自管',
  unknown: '判不出来',
}

export const KINDS = ['loopback', 'ethernet', 'bridge', 'bond', 'vlan', 'wifi', 'tun', 'other'] as const

export type InterfaceKind = (typeof KINDS)[number]

export const KIND_LABELS: Record<InterfaceKind, string> = {
  loopback: '回环',
  ethernet: '以太网',
  bridge: '网桥',
  bond: 'bond',
  vlan: 'VLAN',
  wifi: 'Wi-Fi',
  tun: '隧道',
  other: '其他',
}

export const FAMILIES = ['ipv4', 'ipv6'] as const

export type Family = (typeof FAMILIES)[number]

export const FAMILY_LABELS: Record<Family, string> = { ipv4: 'IPv4', ipv6: 'IPv6' }

export const ISSUE_CODES = ['missing', 'permission_denied', 'timeout', 'unparsable', 'failed'] as const

export type IssueCode = (typeof ISSUE_CODES)[number]

export const ISSUE_CODE_LABELS: Record<IssueCode, string> = {
  missing: '来源不存在',
  permission_denied: '没有权限',
  timeout: '读取超时',
  unparsable: '格式不认识',
  failed: '读取失败',
}

export const MANAGERS = ['ufw', 'firewalld', 'nftables', 'iptables-persistent', 'none', 'unknown'] as const

export type FirewallManager = (typeof MANAGERS)[number]

export const MANAGER_LABELS: Record<FirewallManager, string> = {
  ufw: 'UFW',
  firewalld: 'firewalld',
  nftables: 'nftables',
  'iptables-persistent': 'iptables-persistent',
  none: '没有发现防火墙管理器',
  unknown: '判不出来',
}

/** P14.2 的写路径只接受这两种模式；`unknown` 只出现在**读出**的模式里。 */
export const APPLY_MODES = ['dhcp', 'static'] as const

export type ApplyMode = (typeof APPLY_MODES)[number]

export const APPLY_MODE_LABELS: Record<ApplyMode, string> = {
  dhcp: 'DHCP（自动获取）',
  static: '静态地址',
}

/** 回滚记录的四种状态（CONTRACTS §13.6）。`failed` 只出现在记录里，不出现成任务结果。 */
export const ROLLBACK_STATES = ['pending', 'confirmed', 'rolled_back', 'failed'] as const

export type RollbackState = (typeof ROLLBACK_STATES)[number]

export const ROLLBACK_STATE_LABELS: Record<RollbackState, string> = {
  pending: '等待确认',
  confirmed: '已确认，保留新配置',
  rolled_back: '已自动退回改之前的样子',
  failed: '回滚没有走完',
}

export const ROLLBACK_STATE_TYPES: Record<RollbackState, 'default' | 'success' | 'warning' | 'error' | 'info'> = {
  pending: 'warning',
  confirmed: 'success',
  rolled_back: 'info',
  failed: 'error',
}

/** 取标签；未知值原样透出，不静默吞掉。 */
export function backendLabel(backend: string): string {
  return BACKEND_LABELS[backend as Backend] ?? backend
}

export function kindLabel(kind: string): string {
  return KIND_LABELS[kind as InterfaceKind] ?? kind
}

export function familyLabel(family: string): string {
  return FAMILY_LABELS[family as Family] ?? family
}

export function issueCodeLabel(code: string): string {
  return ISSUE_CODE_LABELS[code as IssueCode] ?? code
}

export function managerLabel(manager: string): string {
  return MANAGER_LABELS[manager as FirewallManager] ?? manager
}

export function applyModeLabel(mode: string): string {
  return APPLY_MODE_LABELS[mode as ApplyMode] ?? mode
}

export function rollbackStateLabel(state: string): string {
  return ROLLBACK_STATE_LABELS[state as RollbackState] ?? state
}

// ---------------------------------------------------------------- DTO

export interface NetworkIssue {
  source: string
  code: string
  detail?: string
}

export interface NetworkAddress {
  family: string
  address: string
  prefix_len: number
  scope: string
  type?: string
}

export interface NetworkInterface {
  name: string
  index: number
  kind: string
  mac?: string
  mtu: number
  state: string
  /** null 表示读不到（虚拟网卡常常读不到），不是"没有载波"。 */
  carrier: boolean | null
  master?: string
  physical: boolean
  addresses: NetworkAddress[]
}

export interface NetworkCapability {
  backend: string
  editable: boolean
  target?: string
  reasons: string[]
}

export interface NetworkInterfaceList {
  collected_at: string
  interfaces: NetworkInterface[]
  capability: NetworkCapability
  issues: NetworkIssue[]
}

export interface NetworkRoute {
  family: string
  destination: string
  gateway?: string
  device?: string
  metric: number
  pref_set: boolean
  protocol?: string
  scope?: string
  default: boolean
}

export interface NetworkGateway {
  family: string
  address: string
  device?: string
  metric: number
}

export interface NetworkRouteList {
  collected_at: string
  routes: NetworkRoute[]
  gateways: NetworkGateway[]
  issues: NetworkIssue[]
}

export interface NetworkListener {
  family: string
  protocol: string
  address: string
  port: number
  wildcard: boolean
  loopback: boolean
  /** 0 表示读不到占用进程，不是"没有进程"。 */
  pid: number
  process?: string
  unit?: string
}

export interface NetworkListenerList {
  collected_at: string
  listeners: NetworkListener[]
  issues: NetworkIssue[]
  truncated: boolean
}

export interface FirewallRule {
  number: number
  action: string
  direction: string
  interface?: string
  to: string
  from: string
  raw: string
}

export interface FirewallState {
  collected_at: string
  manager: string
  manager_reasons: string[]
  installed: boolean
  enabled: boolean
  default_incoming?: string
  default_outgoing?: string
  default_routed?: string
  logging?: string
  rules: FirewallRule[]
  editable: boolean
  reasons: string[]
  issues: NetworkIssue[]
}

// ---------------------------------------------------------------- P14.2 DTO

/** 静态地址。`dns` 为 null 表示没配（不是空数组，0—3 个才是合法数量）。 */
export interface NetworkIPv4Setting {
  address: string
  prefix_length: number
  gateway: string
  dns: string[] | null
}

/** `/etc/netplan` 里一个文件的名字与内容哈希。 */
export interface NetworkFileRef {
  name: string
  sha256: string
}

/**
 * 一条待确认的回滚记录（对外视图）。
 *
 * `seconds_left` 是**采集那一刻**的剩余秒数，不是"现在"。页面要倒计时必须自己按
 * `deadline` 算（见 `rollbackSecondsLeft`）——拿它原地减会出现一个永远不动的数字。
 */
export interface NetworkRollbackView {
  token: string
  interface: string
  mode: string
  candidate_address?: string
  applied_at: string
  deadline: string
  seconds_left: number
  state: string
  result?: string
}

/**
 * `GET /network/config` 的 data：写路径的入口状态。
 *
 * `config_hash` 只在 `capability.editable=true` 时出现。缺了它就**不能**提交——
 * 提交必得 `NETWORK_CONFIG_CHANGED`，而那个错误在页面上看起来像"有人同时在改机器"。
 */
export interface NetworkConfigState {
  collected_at: string
  capability: NetworkCapability
  interface?: string
  permanent_mac?: string
  config_hash?: string
  mode: string
  ipv4?: NetworkIPv4Setting
  panel_file: NetworkFileRef | null
  netplan_files: NetworkFileRef[]
  unsupported: string[]
  pending: NetworkRollbackView | null
  issues: NetworkIssue[]
}

/** `network.apply` 的参数。指纹由 config 给，定义由表单给。 */
export interface NetworkApplyParams {
  interface_id: string
  expected_config_hash: string
  definition: {
    mode: ApplyMode
    ipv4: NetworkIPv4Setting | null
  }
}

/** `network.confirm` / `network.rollback` 的参数：只有网卡名与回滚标识。 */
export interface NetworkRollbackParams {
  interface_id: string
  token: string
}

// ---------------------------------------------------------------- 请求

/**
 * 四条查询的客户端超时。
 *
 * 代理侧一次采集的上限是 8 秒、单条命令 6 秒（internal/agent/networkquery.go），
 * 比默认的 15 秒短，所以正常情况下永远轮不到客户端先超时。留 20 秒是为了让
 * 一次"代理正在收尾"的请求仍然能拿到结果，而不是在代理已经开始回答时被判成超时。
 */
export const NETWORK_QUERY_TIMEOUT_MS = 20_000

export function fetchNetworkInterfaces(): Promise<ApiResult<NetworkInterfaceList>> {
  return get<NetworkInterfaceList>('/network/interfaces', { timeoutMs: NETWORK_QUERY_TIMEOUT_MS })
}

export function fetchNetworkRoutes(): Promise<ApiResult<NetworkRouteList>> {
  return get<NetworkRouteList>('/network/routes', { timeoutMs: NETWORK_QUERY_TIMEOUT_MS })
}

export function fetchNetworkListeners(): Promise<ApiResult<NetworkListenerList>> {
  return get<NetworkListenerList>('/network/listeners', { timeoutMs: NETWORK_QUERY_TIMEOUT_MS })
}

export function fetchFirewall(): Promise<ApiResult<FirewallState>> {
  return get<FirewallState>('/firewall', { timeoutMs: NETWORK_QUERY_TIMEOUT_MS })
}

/**
 * 写路径的入口状态（P14.2）。
 *
 * 它的超时与四条只读查询一样：代理侧要多算一次 `/etc/netplan` 全目录的内容哈希，
 * 但那仍是本地文件读取。慢的是之后的 `netplan apply`，不是这一次读。
 */
export function fetchNetworkConfig(): Promise<ApiResult<NetworkConfigState>> {
  return get<NetworkConfigState>('/network/config', { timeoutMs: NETWORK_QUERY_TIMEOUT_MS })
}

// ---------------------------------------------------------------- 展示逻辑（纯函数）

/** 地址带前缀长度显示：`192.168.1.10/24`。 */
export function addressText(address: NetworkAddress): string {
  return `${address.address}/${address.prefix_len}`
}

/** 载波三态：有、没有、读不到。读不到不能渲染成"断线"。 */
export function carrierText(carrier: boolean | null): string {
  if (carrier === null) return '读不到'
  return carrier ? '有载波' : '无载波'
}

/** 路由优先级的显示：`pref_set=false` 时是"未设置"，不是 0。 */
export function metricText(route: NetworkRoute): string {
  return route.pref_set ? String(route.metric) : '未设置'
}

/**
 * 一条规则的"能不能改"横幅文案。
 *
 * 可改与不可改必须长得不一样：可改时说清楚"改的是哪张网卡"，不可改时把原因留在这里，
 * 由页面逐条展开。**不要**把 editable=false 渲染成"暂不支持"了事——原因里写着
 * 究竟是网桥、多网卡还是 NetworkManager 在跑。
 */
export function capabilityHeadline(capability: NetworkCapability): string {
  if (capability.editable) {
    return capability.target
      ? `可以由面板修改：${capability.target}（netplan + systemd-networkd）`
      : '可以由面板修改'
  }
  return '当前配置不能由面板修改'
}

/**
 * 监听端口的暴露面：通配对外、仅回环、指定地址。
 *
 * 通配不等于"外面真能连上"——还要看防火墙。所以这个名字只描述绑定，
 * 不描述可达性。
 */
export type Exposure = 'wildcard' | 'loopback' | 'specific'

export function listenerExposure(listener: NetworkListener): Exposure {
  if (listener.wildcard) return 'wildcard'
  if (listener.loopback) return 'loopback'
  return 'specific'
}

export const EXPOSURE_LABELS: Record<Exposure, string> = {
  wildcard: '所有地址',
  loopback: '仅本机',
  specific: '指定地址',
}

export function exposureLabel(listener: NetworkListener): string {
  return EXPOSURE_LABELS[listenerExposure(listener)]
}

/** 端口占用的进程文案：`pid=0` 是读不到，不是空闲。 */
export function listenerOwnerText(listener: NetworkListener): string {
  if (listener.pid === 0) return '占用进程读不到'
  const name = listener.process ?? ''
  const unit = listener.unit ? `（${listener.unit}）` : ''
  return name ? `${name} · ${listener.pid}${unit}` : `pid ${listener.pid}${unit}`
}

/**
 * 防火墙状态的一句话结论。
 *
 * 三种情况必须分开说：在管且启用、装了但没启用、根本没装（或判不出来）。
 * "没发现管理器"与"没有防火墙"不是同一件事——读不到时 issues 里另有记录。
 */
export function firewallHeadline(state: FirewallState): string {
  if (state.manager === 'ufw') {
    if (!state.installed) return 'UFW 不在本机：状态与规则都读不到'
    return state.enabled ? 'UFW 在管，且已启用' : 'UFW 装了，但没有启用'
  }
  if (state.manager === 'none') return '没有发现任何防火墙管理器'
  if (state.manager === 'unknown') return '防火墙管理器判不出来'
  return `防火墙由 ${managerLabel(state.manager)} 管理`
}

/** 一条 issue 的整句文案。 */
export function issueText(issue: NetworkIssue): string {
  const head = `${issue.source}：${issueCodeLabel(issue.code)}`
  return issue.detail ? `${head} — ${issue.detail}` : head
}

// ---------------------------------------------------------------- 写路径（P14.2）

/**
 * 表单里的原始输入。全是字符串——输入框给的就是字符串，转成数字的地方只有一处
 * （`buildDefinition`），这样"12abc"之类的输入不会在三个地方各被解释一遍。
 */
export interface ApplyFormInput {
  mode: ApplyMode
  address: string
  prefixLength: string
  gateway: string
  dns: string
}

export const EMPTY_APPLY_FORM: ApplyFormInput = {
  mode: 'static',
  address: '',
  prefixLength: '24',
  gateway: '',
  dns: '',
}

/** 表单校验结果：要么一份能提交的 definition，要么一句能显示给人看的原因。 */
export type ApplyFormResult =
  | { ok: true; definition: NetworkApplyParams['definition'] }
  | { ok: false; error: string }

const IPV4_PATTERN = /^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/

/** 四个字节，顺序固定。用元组而不是 number[]：位运算那几处就不用再断言非空。 */
export type IPv4Octets = [number, number, number, number]

/** 点分十进制 IPv4；不是就返回 null（不做"宽容解析"，不认省略写法）。 */
export function parseIPv4(text: string): IPv4Octets | null {
  const match = IPV4_PATTERN.exec(text.trim())
  if (match === null) return null
  const octets: number[] = []
  for (let i = 1; i <= 4; i += 1) {
    const raw = match[i] as string
    // 前导零一律拒（`192.168.01.1`）：有人当十进制、有人当八进制，代理那边
    // netip.ParseAddr 也是拒的。这里放过去，只会让用户看到一份"我明明填对了"
    // 却被服务端打回来的参数。
    if (raw.length > 1 && raw.startsWith('0')) return null
    const value = Number(raw)
    if (!Number.isInteger(value) || value < 0 || value > 255) return null
    octets.push(value)
  }
  const [a, b, c, d] = octets
  return [a as number, b as number, c as number, d as number]
}

function toIPv4(octets: IPv4Octets): string {
  return octets.join('.')
}

/**
 * 把表单拼成 `network.apply` 的 definition。
 *
 * 这里只做**形状**检查（地址能不能解析、前缀在不在 1—32、网关在不在同一网段、
 * DNS 不超过三个）。地址段禁令（0.0.0.0/8、回环、链路本地、组播……）是服务端策略，
 * 这里**不复制一份**：复制出来的第二份判据会跟第一份漂开，而且它给不出更好的信息
 * ——预检的阻塞项里写着"这个地址属于 127.0.0.0/8，面板不接受"，比前端的一句
 * "地址不合法"清楚得多。
 */
export function buildDefinition(input: ApplyFormInput): ApplyFormResult {
  if (input.mode === 'dhcp') {
    // dhcp 时 ipv4 必须是 null（不是空对象）：服务端对这两种写法判得不一样。
    return { ok: true, definition: { mode: 'dhcp', ipv4: null } }
  }

  const address = parseIPv4(input.address)
  if (address === null) {
    return { ok: false, error: '地址必须是点分十进制 IPv4，例如 192.168.1.50' }
  }
  const prefixText = input.prefixLength.trim()
  const prefixLength = Number(prefixText)
  if (prefixText === '' || !Number.isInteger(prefixLength) || prefixLength < 1 || prefixLength > 32) {
    return { ok: false, error: '前缀长度必须是 1—32 的整数' }
  }

  const gatewayText = input.gateway.trim()
  let gateway = ''
  if (gatewayText !== '') {
    const parsed = parseIPv4(gatewayText)
    if (parsed === null) {
      return { ok: false, error: '网关必须留空，或者是点分十进制 IPv4' }
    }
    if (prefixLength === 32) {
      // /32 的主机路由没有"同网段"可言，带网关只会让人以为默认路由能走通。
      return { ok: false, error: '前缀长度 32 时不能带网关' }
    }
    const mask = prefixLength === 0 ? 0 : (0xffffffff << (32 - prefixLength)) >>> 0
    const toUint = (octets: IPv4Octets): number =>
      ((octets[0] * 0x1000000) + (octets[1] << 16) + (octets[2] << 8) + octets[3]) >>> 0
    if ((toUint(address) & mask) !== (toUint(parsed) & mask)) {
      return { ok: false, error: `网关 ${gatewayText} 不在 ${input.address.trim()}/${prefixLength} 这个网段里` }
    }
    gateway = toIPv4(parsed)
  }

  const dns = input.dns
    .split(/[\s,，]+/)
    .map((item) => item.trim())
    .filter((item) => item !== '')
  if (dns.length > 3) {
    return { ok: false, error: `DNS 最多三个，现在填了 ${dns.length} 个` }
  }
  for (const server of dns) {
    if (parseIPv4(server) === null) {
      return { ok: false, error: `DNS ${server} 不是点分十进制 IPv4` }
    }
  }

  return {
    ok: true,
    definition: {
      mode: 'static',
      // dns 为空时给 null 而不是 []：代理那边 0—3 个是同一个意思，但 null 更贴近"没配"。
      ipv4: { address: toIPv4(address), prefix_length: prefixLength, gateway, dns: dns.length > 0 ? dns : null },
    },
  }
}

/**
 * 拼 `network.apply` 的参数。
 *
 * `expected_config_hash` 只能来自这份 config：它就是"我看到的那份配置"。
 * 拿不到（`editable=false` 或代理没给）时返回 null——页面据此把提交按钮关掉，
 * 而不是发一份注定被拒的请求。
 */
export function buildApplyParams(
  config: NetworkConfigState,
  input: ApplyFormInput,
): { ok: true; params: NetworkApplyParams } | { ok: false; error: string } {
  if (!config.capability.editable || !config.interface || !config.config_hash) {
    return { ok: false, error: '这台机器的网络配置不能由面板修改（或者没拿到 config_hash）' }
  }
  const definition = buildDefinition(input)
  if (!definition.ok) return definition
  return {
    ok: true,
    params: {
      interface_id: config.interface,
      expected_config_hash: config.config_hash,
      definition: definition.definition,
    },
  }
}

/** `network.confirm` / `network.rollback` 的参数。 */
export function buildRollbackParams(view: NetworkRollbackView): NetworkRollbackParams {
  return { interface_id: view.interface, token: view.token }
}

/**
 * 倒计时：按 `deadline` 现算，**不用** `seconds_left`。
 *
 * `seconds_left` 是采集那一刻的数字。拿它在页面上原地减，会得到一个只在"又采了一次"
 * 之后才跳的数字；而这条倒计时正是"还有多久系统会把网络退回去"，它必须一直在走。
 * 已过期返回 0（不说成负数："还剩 -3 秒"没有意义）。
 */
export function rollbackSecondsLeft(deadline: string, now: Date): number {
  const at = Date.parse(deadline)
  if (Number.isNaN(at)) return 0
  const left = Math.ceil((at - now.getTime()) / 1000)
  return left > 0 ? left : 0
}

/** `87` → `1 分 27 秒`。 */
export function countdownText(seconds: number): string {
  if (seconds <= 0) return '已到期'
  if (seconds < 60) return `${seconds} 秒`
  const minutes = Math.floor(seconds / 60)
  const rest = seconds % 60
  return rest === 0 ? `${minutes} 分` : `${minutes} 分 ${rest} 秒`
}

/**
 * 待确认回滚的一句话状态。
 *
 * 三种走向必须分开说：还在等（有时间）、已到期（等自愈）、已经确认/回滚（不需要动作）。
 * 把它们合成一句"有未确认的改动"，人就不知道现在该不该慌。
 */
export function pendingHeadline(view: NetworkRollbackView, secondsLeft: number): string {
  if (view.state === 'pending') {
    if (secondsLeft <= 0) {
      return `${view.interface} 的改动已到期待回滚：系统随时会把它退回改之前的样子`
    }
    const target = view.candidate_address ? `新地址 ${view.candidate_address}` : '新配置'
    return `${view.interface} 已改为${target}，还有 ${countdownText(secondsLeft)} 自动退回`
  }
  if (view.state === 'confirmed') return `${view.interface} 的改动已确认保留`
  if (view.state === 'rolled_back') return `${view.interface} 已退回改之前的样子`
  if (view.state === 'failed') return `${view.interface} 的回滚没有走完：需要到现场处理`
  return `${view.interface} 的回滚状态：${view.state}`
}

/** 本机地址的一句话（静态配置显示地址/前缀，DHCP 显示模式）。 */
export function currentAddressText(config: NetworkConfigState): string {
  if (config.ipv4) {
    const suffix = config.ipv4.prefix_length ? `/${config.ipv4.prefix_length}` : ''
    return `${config.ipv4.address}${suffix}`
  }
  return config.mode === 'dhcp' ? '由 DHCP 分配（还没拿到地址）' : '没有读到地址'
}
