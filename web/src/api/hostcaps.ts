// 外部程序能力探测的 DTO 与翻译（P16.1）。
//
// 数据来自 GET /api/v1/system/capabilities（仅 admin）。这份清单列的是
// **这台机器上装了什么外部程序、什么版本、在哪**——它是主机指纹，所以权限是 admin。
//
// 三条纪律，与接口那头同一套（internal/api/hostcaps.go 文件头）：
//
//  1. `ready=false` 不是“什么都没有”。代理还没探完时 `host_caps` 是 null，
//     页面必须画“还没探完”，不能画一张空表。空表等于说“这台机器什么都没装”，
//     那是一句假话——而这句话恰好会让人去重装已经装好的东西。
//
//  2. 三态不许并成两态。`unknown`（固定目录读不出来）既不是 present 也不是 absent；
//     把它画成“未安装”会让人按提示装一遍，装完发现本来就有。
//
//  3. 这里只是转述。`present` 只代表程序在固定目录里、版本读得到；某个动作能不能做，
//     仍然由那个动作自己的预检回答。所以这个模块里没有任何“可以/不可以”的判断函数。

import { get, type ApiResult } from './client'

// ---------------------------------------------------------------- 形状

/** 三态。`unknown` 是“这一轮说不清”，与 `absent` 是两件事。 */
export type HostCapsState = 'present' | 'absent' | 'unknown'

/** `system` 是按目录/内核事实判断的系统能力，`program` 是固定目录里的可执行文件。 */
export type HostCapsKind = 'system' | 'program'

export interface HostCapsItem {
  /** 稳定标识，如 `restic`、`docker.compose`；表格与测试都按它定位。 */
  id: string
  kind: HostCapsKind
  /** 依赖这个程序的能力，中文短语（由探测方给出，页面不重编）。 */
  feature?: string
  state: HostCapsState
  /** 找到时的绝对路径；没找到时为空。 */
  path?: string
  /** 版本号；只在这条命令真的跑完并匹配到锚点时才非空。 */
  version?: string
  /** 为什么是现在这个状态：缺席原因、版本读不出的原因、超时等。 */
  reason?: string
  /** 补齐它的提示（一般是 apt 包名）。 */
  hint?: string
}

export interface HostCapsReport {
  generated_at: string
  /** 每条规格一项，缺席的也在里面——不许省略缺席项。 */
  items: HostCapsItem[]
  /** 有固定目录读不出来时为 true：此时标着 absent 的条目不能当成“没有”。 */
  partial: boolean
  issues?: string[]
}

export interface SystemCapabilitiesData {
  /** 代理探过了没有。false 时 `host_caps` 必定为 null。 */
  ready: boolean
  /** null 表示“还没探完”，不是“没有依赖”。 */
  host_caps: HostCapsReport | null
}

// ---------------------------------------------------------------- 取数据

/** GET /system/capabilities（仅 admin；viewer 会拿到 403）。 */
export function fetchHostCapabilities(): Promise<ApiResult<SystemCapabilitiesData>> {
  return get<SystemCapabilitiesData>('/system/capabilities')
}

// ---------------------------------------------------------------- 翻译

export const HOST_CAPS_STATE_LABELS: Record<HostCapsState, string> = {
  present: '已安装',
  absent: '未安装',
  unknown: '说不清',
}

export const HOST_CAPS_STATE_HINTS: Record<HostCapsState, string> = {
  present: '程序在固定目录里，版本读得到。能不能用它做事仍由对应动作的预检决定。',
  absent: '固定目录里没有这个程序。按提示装完不必重启代理，下一轮探测会自己更新。',
  unknown: '有固定目录读不出来，所以既不能说有、也不能说没有。',
}

export interface HostCapsCounts {
  total: number
  present: number
  absent: number
  unknown: number
}

/** 数一遍三态。`total` 与 `items.length` 相同，留着是为了让调用处不必自己算。 */
export function countHostCaps(items: readonly HostCapsItem[]): HostCapsCounts {
  const counts: HostCapsCounts = { total: items.length, present: 0, absent: 0, unknown: 0 }
  for (const item of items) {
    if (item.state === 'present') counts.present += 1
    else if (item.state === 'absent') counts.absent += 1
    else counts.unknown += 1
  }
  return counts
}

/**
 * 一行的说明列：present 给路径（版本读不出时带上原因），其余给原因 + 提示。
 *
 * 缺席行不省略、不空白：这张表存在的意义就是回答“为什么那个功能不出现”，
 * 把缺席的理由藏掉等于把答案藏掉。
 */
export function describeHostCapsItem(item: HostCapsItem): string {
  if (item.state === 'present') {
    const path = item.path ?? ''
    if (item.reason === undefined || item.reason === '') return path
    return path === '' ? item.reason : `${path}（${item.reason}）`
  }
  const reason = item.reason ?? ''
  if (item.hint === undefined || item.hint === '') return reason
  return reason === '' ? item.hint : `${reason}；${item.hint}`
}

/** 版本列：没有版本时给一个破折号，不留空白格——空白看起来像“加载中”。 */
export function hostCapsVersionLabel(item: HostCapsItem): string {
  return item.version === undefined || item.version === '' ? '—' : item.version
}

/**
 * 报告顶端那句话：先说探没探过，再说这一轮能不能当结论。
 *
 * 三种情况必须是三句不同的话，因为它们要人做的事完全不同：
 * 还没探完（等一下）、探完了但有目录读不出来（去 SSH 看目录）、探完了且完整（可以照着读）。
 */
export function hostCapsHeadline(data: SystemCapabilitiesData): string {
  if (!data.ready || data.host_caps === null) {
    return '代理还没探测完，这里暂时没有结论。探测在代理启动时自动跑一次，之后每 10 分钟重探一次。'
  }
  if (data.host_caps.partial) {
    return '探测跑完了，但有固定目录读不出来：标着“未安装”的条目不能当成真的没有。'
  }
  return '探测已跑完，结果可以照着读。'
}
