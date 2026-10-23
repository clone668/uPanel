// 软件包接口 DTO（P11.1）：GET /packages、GET /packages/updates、GET /system/power-state。
//
// 字段与 api/openapi.yaml 的 PackageEntry / PackageList / PackageUpdates / PowerState
// 逐字对应；语义来源是 CONTRACTS.md 第 12.1 节。
//
// 这一层**只读**。安装、卸载、升级、刷新索引是 P11.2 的动作，走预检 → 任务链路，
// 所以本模块里没有任何写函数，页面上也没有任何写按钮——不是"还没接上"，是这一阶段
// 本来就不该有。
//
// 三条纪律，每条都对应一条真机验收：
//
//   1) `candidate_relation` 是四种，不是两种。候选比已安装**更旧**是真实情形
//      （pin、回退、第三方源降级），页面必须把它说出来；`unknown` 表示候选**读不到**，
//      不能渲染成"已是最新"——那会把一次采集失败伪装成一个好消息。
//   2) `errors[]` 非空是**降级但仍有内容**，不是整页失败。页面照常显示列表，同时把
//      是哪一路来源读不到写出来。`dpkg_query` 不会出现在这里（它失败就是 503）。
//   3) `counts` 是全量统计，不受筛选和分页影响：它是"这台机器上有多少"，不是
//      "这一页有多少"。页面上的 9 个可升级和列表里的 9 行对上，是因为 filters 恰好
//      什么都没筛——不能拿 counts 当 matched 用。

import { get, type ApiResult } from './client'

// ---------------------------------------------------------------- 超时

/**
 * 这两个接口的客户端超时。
 *
 * 不是随手放大：代理侧 30 秒快照过期后的第一次读取要现采，`apt-cache policy` 在
 * apt 2.8（24.04）上要 21～27 秒（候选版本按 (索引 mtime, 已安装版本) 缓存，之后
 * 都是 0.3 秒）。客户端的默认 15 秒会在这条冷路径上先超时，页面上于是显示"请求超时"，
 * 而事实其实正在被读——正是我们要避免的那种"看起来失败"。
 */
export const PACKAGE_QUERY_TIMEOUT_MS = 60_000

// ---------------------------------------------------------------- 候选关系

export const CANDIDATE_RELATIONS = ['newer', 'same', 'older', 'unknown'] as const

export type CandidateRelation = (typeof CANDIDATE_RELATIONS)[number]

/**
 * 候选版本相对已安装版本的关系 → 中文短名。
 *
 * `newer` 只说"候选更新"，不说"这次会被升级"：分阶段推送（phased）与被保留
 * （held back）的包要 `apt-get -s upgrade` 才判得出，那是 P11.2 的事。
 */
export const CANDIDATE_RELATION_LABELS: Record<CandidateRelation, string> = {
  newer: '候选更新',
  same: '已是最新',
  older: '候选更旧',
  unknown: '候选读不到',
}

export const CANDIDATE_RELATION_TYPES: Record<CandidateRelation, 'default' | 'info' | 'success' | 'warning'> = {
  newer: 'warning',
  same: 'success',
  older: 'info',
  // 读不到不是"没问题"：用 default 而不是 success，页面上另有一句说明。
  unknown: 'default',
}

/** 取一条关系的短标签；未知值原样返回，不静默吞掉。 */
export function candidateRelationLabel(relation: string): string {
  return CANDIDATE_RELATION_LABELS[relation as CandidateRelation] ?? relation
}

// ---------------------------------------------------------------- 源类别

export const PACKAGE_POCKETS = ['release', 'updates', 'security', 'backports', 'proposed', 'other'] as const

export type PackagePocket = (typeof PACKAGE_POCKETS)[number]

/** 源类别 → 中文短名。`other` 是认不出的第三方源名，不猜成 release。 */
export const PACKAGE_POCKET_LABELS: Record<PackagePocket, string> = {
  release: '发行版',
  updates: '更新',
  security: '安全',
  backports: '向后移植',
  proposed: '预发布',
  other: '其他源',
}

export function packagePocketLabel(pocket: string): string {
  return PACKAGE_POCKET_LABELS[pocket as PackagePocket] ?? pocket
}

// ---------------------------------------------------------------- 来源错误

export const PACKAGE_SOURCES = ['dpkg_query', 'apt_cache', 'reboot_pkgs', 'boot_id'] as const

export type PackageSource = (typeof PACKAGE_SOURCES)[number]

/** 哪一路来源读不到 → 给人看的一句话。message 才是细节，这里只给名字。 */
export const PACKAGE_SOURCE_LABELS: Record<PackageSource, string> = {
  dpkg_query: 'dpkg 数据库',
  apt_cache: 'APT 候选版本',
  reboot_pkgs: '重启涉及的包清单',
  boot_id: '本次启动标识',
}

export function packageSourceLabel(source: string): string {
  return PACKAGE_SOURCE_LABELS[source as PackageSource] ?? source
}

/** 取一条降级来源。message 含退出码或解析失败位置，不含命令输出全文。 */
export interface PackageSourceError {
  source: string
  message: string
}

// ---------------------------------------------------------------- 一行的展示文本
//
// 三个纯函数，从这里取而不是写在页面里：页面只负责摆位置，`unknown`（读不到）
// 与 `(none)`（apt 说没有候选）怎么读、`rc` 怎么叫，都是"不许撒谎"的规则，
// 必须能被测试直接钉住。真实夹具回放（realdata-p11-1）用的就是这几个函数。

/** 候选版本的展示文本：null 是"读不到"，不是空串，也不是"已是最新"。 */
export function candidateVersionText(row: PackageEntry): string {
  if (row.candidate_version !== null) return row.candidate_version
  return row.candidate_relation === 'unknown' ? '读不到' : '—'
}

/** 状态的展示文本。`rc` 是"包删了、配置还在"，它既不是装着也不是没装过。 */
export function entryStatusLabel(row: PackageEntry): string {
  if (row.installed) return '已安装'
  return row.status === 'rc' ? '仅剩配置' : `未安装（${row.status}）`
}

/** 一行的来源：pockets 翻成人话；没有候选时明说"无候选"。 */
export function entryPocketText(row: PackageEntry): string {
  if (row.candidate_version === null) return '无候选'
  if (row.pockets.length === 0) return '—'
  return row.pockets.map((pocket) => packagePocketLabel(pocket)).join(' / ')
}

/**
 * 包名的展示文本。
 *
 * dpkg 的 `${binary:Package}` 本来就带架构限定（`bind9-libs:amd64`），而 `all` 架构的包
 * 不带。所以名字里已经有 `:arch` 时不能再补一遍——否则屏幕上会出现 `bind9-libs:amd64:amd64`。
 */
export function entryDisplayName(row: { name: string; architecture: string }): string {
  const qualified = row.architecture !== '' && row.name.endsWith(`:${row.architecture}`)
  if (qualified || row.architecture === 'all' || row.architecture === '') return row.name
  return `${row.name}:${row.architecture}`
}

// ---------------------------------------------------------------- 包条目

/** 一个包的一行。全部是事实加面板的判定，没有"能不能装"的承诺。 */
export interface PackageEntry {
  name: string
  architecture: string
  /** dpkg 的 `${db:Status-Abbrev}` 原文：`ii` 已装、`rc` 只剩配置残留。 */
  status: string
  installed: boolean
  installed_version: string
  /** 候选读不到时为 null——不是空串，页面据此说"读不到"而不是"没有候选"。 */
  candidate_version: string | null
  candidate_relation: CandidateRelation
  /** 等价于 candidate_relation === 'newer'。不代表 apt-get upgrade 一定会升它。 */
  upgradable: boolean
  /** 候选版本（不是别的版本）来自 -security 源，且比已安装新。 */
  security: boolean
  suites: string[]
  pockets: string[]
}

export interface PackageCounts {
  installed: number
  upgradable: number
  security: number
}

export interface PackageFilters {
  query: string
  installed: boolean
  upgradable: boolean
  security: boolean
}

export interface PackageListData {
  collected_at: string
  /** /var/lib/apt/lists 的 mtime；null 表示读不到，页面说"索引时间未知"。 */
  index_updated_at: string | null
  counts: PackageCounts
  filters: PackageFilters
  /** 符合筛选的总条数（分页前）。与 counts 不是一回事。 */
  matched: number
  limit: number
  next_cursor: string | null
  packages: PackageEntry[]
  errors: PackageSourceError[]
}

export interface PackageUpdateEntry {
  name: string
  architecture: string
  installed_version: string
  candidate_version: string
  security: boolean
  suites: string[]
  pockets: PackagePocket[]
}

export interface PackageUpdatesData {
  collected_at: string
  index_updated_at: string | null
  total: number
  security: number
  /** 超过 1000 条时为 true，updates 只给前 1000 条。 */
  truncated: boolean
  updates: PackageUpdateEntry[]
  errors: PackageSourceError[]
}

export interface PowerStateData {
  collected_at: string
  /** /run/reboot-required 是否存在。面板只报告它，不推断"装了内核所以要重启"。 */
  reboot_required: boolean
  reboot_required_since: string | null
  reboot_required_packages: string[]
  reboot_required_packages_error: string
  /** P11.3 用它确认重启真的发生过：同一个值说明系统没有重启。 */
  boot_id: string
  boot_id_error: string
}

// ---------------------------------------------------------------- 查询参数

export interface PackageListQuery {
  /** 包名子串，由代理侧做匹配。空串表示不筛。 */
  query?: string | undefined
  /** 默认 true：只列真正装着的包。false 才把只剩配置残留（`rc`）的包算进来。 */
  installed?: boolean | undefined
  upgradable?: boolean | undefined
  security?: boolean | undefined
  limit?: number | undefined
  cursor?: string | undefined
}

/**
 * 把查询参数整理成 client 要的形状。
 *
 * 显式不传 undefined 的项，**不**把 "false" 拼进去：服务端对缺省有默认值
 * （installed 缺省 true），多传一个空值只会让审计里的请求看不懂。
 */
export function buildPackageQuery(q: PackageListQuery): Record<string, string | number | boolean> {
  const out: Record<string, string | number | boolean> = {}
  if (q.query !== undefined && q.query !== '') out.query = q.query
  if (q.installed !== undefined) out.installed = q.installed
  if (q.upgradable) out.upgradable = true
  if (q.security) out.security = true
  if (q.limit !== undefined) out.limit = q.limit
  if (q.cursor !== undefined && q.cursor !== '') out.cursor = q.cursor
  return out
}

// ---------------------------------------------------------------- 请求

/** GET /packages：一页包事实。 */
export function fetchPackages(q: PackageListQuery = {}): Promise<ApiResult<PackageListData>> {
  return get<PackageListData>('/packages', {
    query: buildPackageQuery(q),
    timeoutMs: PACKAGE_QUERY_TIMEOUT_MS,
  })
}

/** GET /packages/updates：可升级清单（全量，超过 1000 条截断）。 */
export function fetchPackageUpdates(): Promise<ApiResult<PackageUpdatesData>> {
  return get<PackageUpdatesData>('/packages/updates', {
    timeoutMs: PACKAGE_QUERY_TIMEOUT_MS,
  })
}

/**
 * GET /system/power-state：重启标志与本次启动标识。
 *
 * 它和上面两个接口共用一份代理快照，所以冷路径同样可能慢；给它同样的预算。
 */
export function fetchPowerState(): Promise<ApiResult<PowerStateData>> {
  return get<PowerStateData>('/system/power-state', {
    timeoutMs: PACKAGE_QUERY_TIMEOUT_MS,
  })
}
