// 验证码输入的小工具用例（P14.6）。
//
// 三条函数各自对应一处真实的错法：
//   - `sanitizeCode` 不洗掉字母，字母就跟着口令一起发上去，撞一次限速；
//   - `isCodeComplete` 不卡六位，一个输到一半的码会让服务端记一次失败——
//     而失败计数是共享的，输得慢的人会把猜码的人一起锁在外面；
//   - `isRecoveryCodeShape` 认不出连字符/小写，人就会以为"我的恢复码无效"，
//     而实际上服务端的大小写与连字符不敏感。

import { describe, expect, it } from 'vitest'

import { isCodeComplete, isRecoveryCodeShape, sanitizeCode } from './secondfactor'

describe('sanitizeCode', () => {
  it('只留数字', () => {
    expect(sanitizeCode('12a34b')).toBe('1234')
    expect(sanitizeCode('abc')).toBe('')
    expect(sanitizeCode('1 2 3 4 5 6')).toBe('123456')
    expect(sanitizeCode('１２３')).toBe('')
  })

  it('最多六位，多的截掉', () => {
    expect(sanitizeCode('1234567890')).toBe('123456')
  })

  it('已经合格的输入原样返回', () => {
    expect(sanitizeCode('004312')).toBe('004312')
  })
})

describe('isCodeComplete', () => {
  it('恰好六位数字才算输完', () => {
    expect(isCodeComplete('123456')).toBe(true)
    expect(isCodeComplete('004312')).toBe(true)
  })

  it('不足、超出、含非数字都不算', () => {
    expect(isCodeComplete('')).toBe(false)
    expect(isCodeComplete('12345')).toBe(false)
    expect(isCodeComplete('1234567')).toBe(false)
    expect(isCodeComplete('12345a')).toBe(false)
    expect(isCodeComplete(' 123456')).toBe(false)
  })
})

describe('isRecoveryCodeShape', () => {
  it('带连字符与不带连字符都认', () => {
    expect(isRecoveryCodeShape('3F7K9-QM2XP')).toBe(true)
    expect(isRecoveryCodeShape('3F7K9QM2XP')).toBe(true)
  })

  it('小写也认，前后空格不算错', () => {
    expect(isRecoveryCodeShape('3f7k9-qm2xp')).toBe(true)
    expect(isRecoveryCodeShape('  3F7K9-QM2XP  ')).toBe(true)
  })

  it('长度不对或带别的字符就不认', () => {
    expect(isRecoveryCodeShape('')).toBe(false)
    expect(isRecoveryCodeShape('3F7K9-QM2X')).toBe(false)
    expect(isRecoveryCodeShape('3F7K9-QM2XPQ')).toBe(false)
    expect(isRecoveryCodeShape('3F7K9-QM2-P')).toBe(false)
    expect(isRecoveryCodeShape('3F7K9_QM2XP')).toBe(false)
  })
})
