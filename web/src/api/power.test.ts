// 电源动作的前端约定与结果读法（P11.3）。
//
// 这一组守的是界面上的几句真话，而不是请求：
//   - 参数必须带 operation 与显式的 delay_seconds，且延迟在 30—300 之间；
//   - 关机的话术里必须出现“不会自己开机”和“无法确认电源”，不许出现“已关机/关机成功”；
//   - power_check 的四个状态各有说法，结构看不懂时返回 null 而不是编一个默认值；
//   - 倒计时秒数只从**服务端**进度事件里取，取不到就是 null（界面不显示数字）。

import { describe, expect, it } from 'vitest'

import type { JobDTO, PowerCheckDTO, PowerCheckState } from './jobs'
import {
  POWER_ACTION,
  POWER_CHECK_LABELS,
  POWER_CHECK_TYPES,
  POWER_DELAY_MAX,
  POWER_DELAY_MIN,
  POWER_DELAY_PRESETS,
  POWER_OPERATIONS,
  POWER_OPERATION_LABELS,
  POWER_OPERATION_VERBS,
  POWEROFF_UNCERTAIN_NOTICE,
  acceptedNotice,
  isValidPowerDelay,
  powerConfirmLines,
  powerNeedsReauth,
  powerParams,
  readCountdownSeconds,
  readPowerCheck,
} from './power'

describe('动作与参数', () => {
  it('动作名与两个操作是契约里写死的那几个', () => {
    expect(POWER_ACTION).toBe('system.power')
    expect([...POWER_OPERATIONS]).toEqual(['reboot', 'poweroff'])
    for (const operation of POWER_OPERATIONS) {
      expect(POWER_OPERATION_LABELS[operation]).toBeTruthy()
      expect(POWER_OPERATION_VERBS[operation]).toBeTruthy()
    }
  })

  it('延迟区间是 30—300 秒，没有“立即执行”这一档', () => {
    expect(POWER_DELAY_MIN).toBe(30)
    expect(POWER_DELAY_MAX).toBe(300)
    expect(isValidPowerDelay(30)).toBe(true)
    expect(isValidPowerDelay(300)).toBe(true)
    expect(isValidPowerDelay(29)).toBe(false)
    expect(isValidPowerDelay(301)).toBe(false)
    expect(isValidPowerDelay(0)).toBe(false)
    expect(isValidPowerDelay(60.5)).toBe(false)
    expect(isValidPowerDelay(Number.NaN)).toBe(false)
    for (const preset of POWER_DELAY_PRESETS) expect(isValidPowerDelay(preset)).toBe(true)
  })

  it('参数一定要带 delay_seconds：没有默认值，代理会拒收缺字段的请求', () => {
    expect(powerParams('reboot', 60)).toEqual({ operation: 'reboot', delay_seconds: 60 })
    expect(powerParams('poweroff', 300)).toEqual({ operation: 'poweroff', delay_seconds: 300 })
    // 不合法就给 null：调用方据此不预检，而不是拿一份会被拒的参数去问“能不能”。
    expect(powerParams('reboot', 29)).toBeNull()
    expect(powerParams('reboot', 600)).toBeNull()
    expect(powerParams('shutdown' as never, 60)).toBeNull()
  })

  it('电源动作一律要再认证', () => {
    expect(powerNeedsReauth()).toBe(true)
  })
})

describe('确认框里的话', () => {
  it('重启：说清会断连，也说不替你猜', () => {
    const lines = powerConfirmLines('reboot', 60).join('\n')
    expect(lines).toContain('systemctl reboot')
    expect(lines).toContain('倒计时 60 秒')
    expect(lines).toContain('可以取消')
    expect(lines).toContain('boot_id')
    expect(lines).not.toContain('已重启')
  })

  it('关机：必须说“不会自己开机”和“最终只能现场确认”', () => {
    const lines = powerConfirmLines('poweroff', 300).join('\n')
    expect(lines).toContain('systemctl poweroff')
    expect(lines).toContain('不会自己开机')
    expect(lines).toContain('现场确认')
    expect(lines).not.toContain('关机成功')
    expect(lines).not.toContain('已关机')
  })

  it('受理提示只说“请求已受理”，不说成功', () => {
    const reboot = acceptedNotice('reboot', 60)
    expect(reboot).toContain('请求已受理')
    expect(reboot).toContain('这不是“已重启”')
    expect(reboot).not.toContain('重启成功')

    const poweroff = acceptedNotice('poweroff', null)
    expect(poweroff).toContain('请求已受理')
    expect(poweroff).toContain('不会自己开机')
    // “已关机”只允许出现在否定句里。
    expect(poweroff).toContain('这不是“已关机”')
    expect(poweroff).not.toContain('关机成功')
  })

  it('关机不确定那句话可以直接摆上屏幕', () => {
    expect(POWEROFF_UNCERTAIN_NOTICE).toContain('不构成关机成功的证据')
    expect(POWEROFF_UNCERTAIN_NOTICE).toContain('现场确认')
  })
})

describe('power_check 的读法', () => {
  const states: PowerCheckState[] = ['confirmed', 'unchanged', 'unreachable', 'unknown']

  it('四个状态都有中文说法与标签色', () => {
    for (const state of states) {
      expect(POWER_CHECK_LABELS[state], state).toBeTruthy()
      expect(POWER_CHECK_TYPES[state], state).toBeTruthy()
    }
    // 连不上代理对关机是预期之内，不该染成错误色；但也不能是成功色。
    expect(POWER_CHECK_TYPES.unreachable).not.toBe('error')
    expect(POWER_CHECK_TYPES.unreachable).not.toBe('success')
    expect(POWER_CHECK_TYPES.confirmed).toBe('success')
    expect(POWER_CHECK_TYPES.unchanged).toBe('warning')
  })

  const sample: PowerCheckDTO = {
    operation: 'reboot',
    state: 'confirmed',
    changed: true,
    boot_id_before: 'ab2eb5e8-c1b4-4a00-a3d0-33ced7870abc',
    boot_id_now: '9f4d2c17-6a55-4c2e-8f31-0b7e5a1d9c40',
    checked_at: '2026-10-23T10:00:00Z',
    message: '重启已确认：boot_id 从 ab2eb5e8 变成 9f4d2c17。',
  }

  it('正常结构原样读出来', () => {
    expect(readPowerCheck({ power_check: sample })).toEqual(sample)
  })

  it('没有这一节时返回 null：它对多数任务根本不成立', () => {
    expect(readPowerCheck({ power_check: null })).toBeNull()
    expect(readPowerCheck(null)).toBeNull()
    expect(readPowerCheck(undefined)).toBeNull()
  })

  it('结构不对就返回 null，不猜', () => {
    const broken: unknown[] = [
      { power_check: 'confirmed' },
      { power_check: { ...sample, operation: 'suspend' } },
      { power_check: { ...sample, state: 'probably' } },
      { power_check: { ...sample, state: undefined } },
    ]
    for (const input of broken) {
      expect(readPowerCheck(input as { power_check: PowerCheckDTO | null })).toBeNull()
    }
  })

  it('缺字段按“读不到”处理：空串与 false，不显示 undefined', () => {
    const parsed = readPowerCheck({
      power_check: { operation: 'poweroff', state: 'unreachable' } as unknown as PowerCheckDTO,
    })
    expect(parsed).toEqual({
      operation: 'poweroff',
      state: 'unreachable',
      changed: false,
      boot_id_before: '',
      boot_id_now: '',
      checked_at: '',
      message: '',
    })
  })
})

describe('倒计时读数', () => {
  it('只认 countdown 步骤里的 remaining_seconds', () => {
    expect(readCountdownSeconds({ step: 'countdown', remaining_seconds: 25 })).toBe(25)
    expect(readCountdownSeconds({ step: 'countdown', remaining_seconds: 0 })).toBe(0)
    expect(readCountdownSeconds({ step: 'countdown', remaining_seconds: 2.7 })).toBe(2)
  })

  it('别的步骤、坏数字、缺字段都是 null：界面就不显示秒数', () => {
    expect(readCountdownSeconds({ step: 'issued', remaining_seconds: 5 })).toBeNull()
    expect(readCountdownSeconds({ step: 'countdown' })).toBeNull()
    expect(readCountdownSeconds({ step: 'countdown', remaining_seconds: -1 })).toBeNull()
    expect(readCountdownSeconds({ step: 'countdown', remaining_seconds: '9' })).toBeNull()
    expect(readCountdownSeconds(null)).toBeNull()
    expect(readCountdownSeconds(undefined)).toBeNull()
  })

  it('进度事件里带着 boot_id_before 也不影响读数（它只是随行字段）', () => {
    expect(
      readCountdownSeconds({ step: 'countdown', remaining_seconds: 10, boot_id_before: 'ab2eb5e8' }),
    ).toBe(10)
  })
})

// 让 JobDTO 参与编译期检查：power_check 是必填字段（可以是 null），
// 漏掉它的地方会被 vue-tsc 抓住，而不是悄悄渲染成 undefined。
const _jobShape: Pick<JobDTO, 'power_check'> = { power_check: null }
void _jobShape
