// 展示层格式化：字节、速率、百分比、温度、时间。
//
// 规则来源 CONTRACTS.md 第 1 节：
//   - 字节与累计计数器是**非负十进制字符串**，可能超过 Number 的安全整数范围，
//     因此解析一律走 BigInt，绝不 Number(x) 之后做精确运算；
//   - null 表示“没采到”，不是 0。格式化函数遇到 null 返回 U+2014（—），
//     界面上永远不出现“0 B”冒充“没读到”；
//   - 百分比 0—100，比率带 _per_sec，时延带 _ms，温度带 _celsius。

/** 十进制字符串的语法（与服务端 ByteString 的 pattern 一致）。 */
const BYTE_PATTERN = /^(0|[1-9][0-9]*)$/

/** 未知值的统一占位符：一个 em dash，不用 0、n/a、unknown 混着来。 */
export const UNKNOWN_TEXT = '—'

const BYTE_UNITS = ['B', 'KiB', 'MiB', 'GiB', 'TiB', 'PiB', 'EiB'] as const

/** 解析服务端给的十进制字节串；格式不对返回 null（不猜、不补齐）。 */
export function parseByteString(value: string | null | undefined): bigint | null {
  if (typeof value !== 'string' || !BYTE_PATTERN.test(value)) return null
  return BigInt(value)
}

function scaled(value: bigint, unitIndex: number): string {
  const base = BigInt(1024) ** BigInt(unitIndex)
  const whole = value / base
  if (unitIndex === 0) return whole.toString()
  // 小数部分用余数算，避免把大数转成 Number。
  const remainder = value % base
  const hundredths = (remainder * BigInt(100)) / base
  if (hundredths === BigInt(0)) return whole.toString()
  const fraction = hundredths.toString().padStart(2, '0').replace(/0+$/, '')
  return `${whole.toString()}.${fraction}`
}

/**
 * 格式化字节数（"1000204886016" → "931.32 GiB"）。
 * 传 null/undefined/非法串返回 `—`：界面必须能区分“0 字节”和“没读到”。
 */
export function formatBytes(value: string | null | undefined): string {
  const parsed = parseByteString(value)
  if (parsed === null) return UNKNOWN_TEXT
  let unit = 0
  let limit = BigInt(1024)
  while (unit < BYTE_UNITS.length - 1 && parsed >= limit) {
    unit += 1
    limit *= BigInt(1024)
  }
  const suffix = BYTE_UNITS[unit] ?? 'B'
  return `${scaled(parsed, unit)} ${suffix}`
}

/** 不带单位的字节数（用于卡片副标题等需要紧凑的地方）。 */
export function formatBytesShort(value: string | null | undefined): string {
  return formatBytes(value)
}

/**
 * 悬停用的精确字节数（PLAN.md 第 7 节：容量默认 IEC 单位，悬停显示精确字节）。
 * 没采到时返回 undefined，模板上的 title 属性就会整个不出现。
 */
export function bytesTitle(value: string | null | undefined): string | undefined {
  const parsed = parseByteString(value)
  return parsed === null ? undefined : `${parsed.toString()} 字节`
}

/**
 * 两个字节串的比值（0—100，保留两位小数），用于“已用/总量”这类百分比。
 * 分母为 0 或任一值没采到都返回 null：不拿 0% 冒充“没数据”。
 */
export function percentOf(
  part: string | null | undefined,
  whole: string | null | undefined,
): number | null {
  const numerator = parseByteString(part)
  const denominator = parseByteString(whole)
  if (numerator === null || denominator === null) return null
  if (denominator === BigInt(0)) return null
  const hundredthsOfPercent = (numerator * BigInt(10000)) / denominator
  return Number(hundredthsOfPercent) / 100
}

function trimNumber(value: number, digits: number): string {
  const fixed = value.toFixed(digits)
  return fixed.includes('.') ? fixed.replace(/\.?0+$/, '') : fixed
}

/** 百分比（0—100）；null 是 `—`，不是 0。 */
export function formatPercent(value: number | null | undefined, digits = 0): string {
  if (value === null || value === undefined || !Number.isFinite(value)) return UNKNOWN_TEXT
  return `${trimNumber(value, digits)}%`
}

/** 速率：入参是字节/秒的十进制字符串（rate 里的 *_per_second）。 */
export function formatRate(bytesPerSecond: string | null | undefined): string {
  const parsed = parseByteString(bytesPerSecond)
  if (parsed === null) return UNKNOWN_TEXT
  return `${formatBytes(parsed.toString())}/s`
}

/** 网卡速率在 DTO 里是 number（不是十进制字符串），单独一个入口。 */
export function formatRateNumber(bytesPerSecond: number | null | undefined): string {
  if (bytesPerSecond === null || bytesPerSecond === undefined || !Number.isFinite(bytesPerSecond)) {
    return UNKNOWN_TEXT
  }
  if (bytesPerSecond < 0) return UNKNOWN_TEXT
  return `${formatBytes(BigInt(Math.round(bytesPerSecond)).toString())}/s`
}

/** IOPS（number，服务端保留两位小数）。 */
export function formatIops(value: number | null | undefined): string {
  if (value === null || value === undefined || !Number.isFinite(value)) return UNKNOWN_TEXT
  return trimNumber(value, 2)
}

/** 平均时延（毫秒）；没有样本时是 null，不能显示成 0 ms。 */
export function formatLatency(ms: number | null | undefined): string {
  if (ms === null || ms === undefined || !Number.isFinite(ms)) return UNKNOWN_TEXT
  return `${trimNumber(ms, 3)} ms`
}

/** 温度；≤0 或 null 都当作“没有读数”（CONTRACTS：不能把不可用转成 0 °C）。 */
export function formatTemperature(celsius: number | null | undefined): string {
  if (celsius === null || celsius === undefined || !Number.isFinite(celsius) || celsius <= 0) {
    return UNKNOWN_TEXT
  }
  return `${trimNumber(celsius, 1)} °C`
}

/** 开机时长（秒 → “3 天 4 小时”）；null 是 `—`。 */
export function formatUptime(seconds: number | null | undefined): string {
  if (seconds === null || seconds === undefined || !Number.isFinite(seconds) || seconds < 0) {
    return UNKNOWN_TEXT
  }
  const total = Math.floor(seconds)
  const days = Math.floor(total / 86400)
  const hours = Math.floor((total % 86400) / 3600)
  const minutes = Math.floor((total % 3600) / 60)
  if (days > 0) return `${days} 天 ${hours} 小时`
  if (hours > 0) return `${hours} 小时 ${minutes} 分钟`
  if (minutes > 0) return `${minutes} 分钟`
  return `${total} 秒`
}

/**
 * RFC3339 时间 → 本地时间字符串（24 小时制）。
 * 空值、非法值返回 `—`：宁可显示“没有时间”，也不显示 1970。
 */
export function formatTimestamp(value: string | null | undefined): string {
  const date = parseTimestamp(value)
  if (date === null) return UNKNOWN_TEXT
  return date.toLocaleString('zh-CN', { hour12: false })
}

/** 只显示时分秒，用于“最近一次检查”这类高频刷新的位置。 */
export function formatClock(value: string | null | undefined): string {
  const date = parseTimestamp(value)
  if (date === null) return UNKNOWN_TEXT
  return date.toLocaleTimeString('zh-CN', { hour12: false })
}

export function parseTimestamp(value: string | null | undefined): Date | null {
  if (typeof value !== 'string' || value === '') return null
  const date = new Date(value)
  return Number.isNaN(date.getTime()) ? null : date
}

/**
 * 数据“有多旧”：服务端时间与基准时间的差，用于把 stale 说成人话。
 * 差值为负（时钟漂移）时按 0 处理，不显示“-3 秒前”。
 */
export function formatAge(
  collectedAt: string | null | undefined,
  now: Date = new Date(),
): string {
  const collected = parseTimestamp(collectedAt)
  if (collected === null) return UNKNOWN_TEXT
  const deltaMs = Math.max(0, now.getTime() - collected.getTime())
  const seconds = Math.floor(deltaMs / 1000)
  if (seconds < 2) return '刚刚'
  if (seconds < 60) return `${seconds} 秒前`
  const minutes = Math.floor(seconds / 60)
  if (minutes < 60) return `${minutes} 分钟前`
  const hours = Math.floor(minutes / 60)
  if (hours < 24) return `${hours} 小时前`
  return `${Math.floor(hours / 24)} 天前`
}

/**
 * 三个时间戳拼成一句话的一行摘要，页头统一用它。
 * 任何一项缺失都如实显示 `—`，不省略字段。
 */
export function formatSampleSummary(
  collectedAt: string | null | undefined,
  intervalMs: number | null | undefined,
  now: Date = new Date(),
): string {
  const age = formatAge(collectedAt, now)
  if (intervalMs === null || intervalMs === undefined || !Number.isFinite(intervalMs)) {
    return `采集于 ${formatTimestamp(collectedAt)}（${age}）`
  }
  const interval = intervalMs >= 1000 ? `${trimNumber(intervalMs / 1000, 1)} 秒` : `${intervalMs} 毫秒`
  return `采集于 ${formatTimestamp(collectedAt)}（${age}，每 ${interval}刷新）`
}
