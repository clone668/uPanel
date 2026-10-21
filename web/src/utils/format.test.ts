// 格式化函数的单元测试。
//
// 重点不在“字符串好不好看”，而在两条硬规则：
//   1) 大计数器必须走 BigInt，不能出现 Number 的静默丢位；
//   2) null（没采到）与 0（真的是 0）必须显示成两种东西。

import { describe, expect, it } from 'vitest'

import {
  UNKNOWN_TEXT,
  bytesTitle,
  formatAge,
  formatBytes,
  formatClock,
  formatIops,
  formatLatency,
  formatPercent,
  formatRate,
  formatRateNumber,
  formatSampleSummary,
  formatTemperature,
  formatTimestamp,
  formatUptime,
  parseByteString,
  parseTimestamp,
  percentOf,
} from './format'

describe('parseByteString', () => {
  it('接受服务端 ByteString 语法', () => {
    expect(parseByteString('0')).toBe(BigInt(0))
    expect(parseByteString('1000204886016')).toBe(BigInt('1000204886016'))
  })

  it('超过 2^53 的数值不丢位', () => {
    // 换成 Number 就丢位：Number("9007199254740993") 得到 9007199254740992。
    expect(parseByteString('9007199254740993')).toBe(BigInt('9007199254740993'))
    expect(Number('9007199254740993')).toBe(9007199254740992)
  })

  it('拒绝一切非规范写法，不做容错猜测', () => {
    for (const bad of [null, undefined, '', '01', '-1', '1e3', ' 12', '12 ', '1.5', 'abc']) {
      expect(parseByteString(bad)).toBeNull()
    }
  })
})

describe('formatBytes', () => {
  it('不足 1 KiB 时按字节显示', () => {
    expect(formatBytes('0')).toBe('0 B')
    expect(formatBytes('512')).toBe('512 B')
    expect(formatBytes('1000')).toBe('1000 B')
  })

  it('按 1024 进制换单位，小数最多两位且去掉尾随零', () => {
    expect(formatBytes('1024')).toBe('1 KiB')
    expect(formatBytes('1536')).toBe('1.5 KiB')
    expect(formatBytes('1048576')).toBe('1 MiB')
    expect(formatBytes('1610612736')).toBe('1.5 GiB')
    expect(formatBytes('1099511627776')).toBe('1 TiB')
  })

  it('最大单位是 EiB，超出不再加单位', () => {
    expect(formatBytes((BigInt(1024) ** BigInt(6)).toString())).toBe('1 EiB')
    expect(formatBytes((BigInt(1024) ** BigInt(7)).toString())).toBe('1024 EiB')
  })

  it('巨大数值不因 Number 精度而显示错值', () => {
    // 2^80 远超 Number 的安全整数范围（Number 会舍入成 1.2089258196146292e+24），
    // 只有 BigInt 除法能给出正确的 2^80 / 2^60 = 1048576。
    const value = (BigInt(2) ** BigInt(80)).toString()
    expect(formatBytes(value)).toBe('1048576 EiB')
  })

  it('没采到显示 —，不是 0 B', () => {
    expect(formatBytes(null)).toBe(UNKNOWN_TEXT)
    expect(formatBytes(undefined)).toBe(UNKNOWN_TEXT)
    expect(formatBytes('')).toBe(UNKNOWN_TEXT)
    expect(formatBytes('01')).toBe(UNKNOWN_TEXT)
  })
})

describe('bytesTitle / percentOf', () => {
  it('悬停显示精确字节；没采到时不生成 title', () => {
    expect(bytesTitle('1000204886016')).toBe('1000204886016 字节')
    expect(bytesTitle('0')).toBe('0 字节')
    expect(bytesTitle(null)).toBeUndefined()
    expect(bytesTitle('01')).toBeUndefined()
  })

  it('按 BigInt 算占比，避免大数相乘后丢精度', () => {
    expect(percentOf('1', '4')).toBe(25)
    expect(percentOf('1', '3')).toBe(33.33)
    expect(percentOf('5', '5')).toBe(100)
    // 2^70 已经在 Number 安全范围之外，这里只看除法结果。
    const half = (BigInt(2) ** BigInt(70)).toString()
    const whole = (BigInt(2) ** BigInt(71)).toString()
    expect(percentOf(half, whole)).toBe(50)
  })

  it('分母为 0 或数据缺失时返回 null，不返回 0%', () => {
    expect(percentOf('0', '0')).toBeNull()
    expect(percentOf('1', '0')).toBeNull()
    expect(percentOf(null, '100')).toBeNull()
    expect(percentOf('100', null)).toBeNull()
  })
})

describe('formatPercent', () => {  it('保留指定小数位并去掉多余的零', () => {
    expect(formatPercent(45.6)).toBe('46%')
    expect(formatPercent(45.6, 1)).toBe('45.6%')
    expect(formatPercent(100)).toBe('100%')
  })

  it('0 是有效读数，null 不是 0', () => {
    expect(formatPercent(0)).toBe('0%')
    expect(formatPercent(null)).toBe(UNKNOWN_TEXT)
    expect(formatPercent(undefined)).toBe(UNKNOWN_TEXT)
    expect(formatPercent(Number.NaN)).toBe(UNKNOWN_TEXT)
  })
})

describe('formatRate / formatIops / formatLatency', () => {
  it('速率入参是十进制字节串', () => {
    expect(formatRate('0')).toBe('0 B/s')
    expect(formatRate('2048')).toBe('2 KiB/s')
    expect(formatRate(null)).toBe(UNKNOWN_TEXT)
  })

  it('网卡速率是 number，单独一个入口', () => {
    expect(formatRateNumber(0)).toBe('0 B/s')
    expect(formatRateNumber(1536)).toBe('1.5 KiB/s')
    expect(formatRateNumber(null)).toBe(UNKNOWN_TEXT)
    expect(formatRateNumber(-1)).toBe(UNKNOWN_TEXT)
  })

  it('IOPS 保留两位小数', () => {
    expect(formatIops(0)).toBe('0')
    expect(formatIops(12.345)).toBe('12.35')
    expect(formatIops(null)).toBe(UNKNOWN_TEXT)
  })

  it('时延 0 ms 是有效读数，null 显示 —', () => {
    expect(formatLatency(0)).toBe('0 ms')
    expect(formatLatency(0.4567)).toBe('0.457 ms')
    expect(formatLatency(null)).toBe(UNKNOWN_TEXT)
  })
})

describe('formatTemperature', () => {
  it('≤0 与 null 都当作没有读数，绝不显示 0 °C 冒充正常', () => {
    expect(formatTemperature(0)).toBe(UNKNOWN_TEXT)
    expect(formatTemperature(-5)).toBe(UNKNOWN_TEXT)
    expect(formatTemperature(null)).toBe(UNKNOWN_TEXT)
  })

  it('正常温度保留一位小数', () => {
    expect(formatTemperature(38)).toBe('38 °C')
    expect(formatTemperature(38.25)).toBe('38.3 °C')
  })
})

describe('formatUptime', () => {
  it('按天/时/分/秒说话', () => {
    expect(formatUptime(0)).toBe('0 秒')
    expect(formatUptime(59)).toBe('59 秒')
    expect(formatUptime(90)).toBe('1 分钟')
    expect(formatUptime(3661)).toBe('1 小时 1 分钟')
    expect(formatUptime(90061)).toBe('1 天 1 小时')
  })

  it('读不到时是 —', () => {
    expect(formatUptime(null)).toBe(UNKNOWN_TEXT)
    expect(formatUptime(-1)).toBe(UNKNOWN_TEXT)
  })
})

describe('时间格式化', () => {
  it('非法与空值不会被当成 1970', () => {
    expect(parseTimestamp(null)).toBeNull()
    expect(parseTimestamp('')).toBeNull()
    expect(parseTimestamp('not-a-time')).toBeNull()
    expect(formatTimestamp(null)).toBe(UNKNOWN_TEXT)
    expect(formatTimestamp('nope')).toBe(UNKNOWN_TEXT)
    expect(formatClock(null)).toBe(UNKNOWN_TEXT)
  })

  it('合法 RFC3339 能格式化', () => {
    expect(formatTimestamp('2026-10-21T09:31:45Z')).toMatch(/^2026/)
    expect(formatClock('2026-10-21T09:31:45Z')).toMatch(/^\d{1,2}:\d{2}:\d{2}$/)
  })

  it('数据新旧按基准时间计算，时钟漂移不显示负数', () => {
    const now = new Date('2026-10-21T10:00:00Z')
    expect(formatAge('2026-10-21T09:59:59Z', now)).toBe('刚刚')
    expect(formatAge('2026-10-21T09:59:30Z', now)).toBe('30 秒前')
    expect(formatAge('2026-10-21T09:58:00Z', now)).toBe('2 分钟前')
    expect(formatAge('2026-10-21T07:00:00Z', now)).toBe('3 小时前')
    expect(formatAge('2026-10-19T10:00:00Z', now)).toBe('2 天前')
    expect(formatAge('2026-10-21T10:00:30Z', now)).toBe('刚刚')
    expect(formatAge(null, now)).toBe(UNKNOWN_TEXT)
  })

  it('页头摘要把采集时间与刷新间隔一起说清楚', () => {
    const now = new Date('2026-10-21T10:00:10Z')
    const line = formatSampleSummary('2026-10-21T10:00:00Z', 15_000, now)
    expect(line).toContain('采集于')
    expect(line).toContain('10 秒前')
    expect(line).toContain('每 15 秒刷新')
  })

  it('间隔小于 1 秒时用毫秒表述', () => {
    const now = new Date('2026-10-21T10:00:10Z')
    expect(formatSampleSummary('2026-10-21T10:00:10Z', 500, now)).toContain('每 500 毫秒刷新')
  })

  it('没有间隔时不编造刷新节奏', () => {
    const now = new Date('2026-10-21T10:00:10Z')
    const line = formatSampleSummary('2026-10-21T10:00:00Z', null, now)
    expect(line).not.toContain('刷新')
    expect(line).toContain('10 秒前')
  })
})
