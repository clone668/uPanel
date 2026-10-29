// 计划任务与脚本页的纯逻辑测试（P12.5）。
//
// 校验只查**形状**，不假装能裁决语义：日历表达式认不认只有 `systemd-analyze
// calendar` 说了算，那是预检的事。所以这里的用例分成两半：
//   - 形状错了，前端就该在本地拦下（长度、字符集、句首不是 -、时长上限）；
//   - 形状对但语义可疑的（"每天 25 点"），前端不拦，交给预检。
//
// 另一半钉的是两件真的会害人的事：
//   - `certificate` 只在证书续期时出现，别的种类带上它服务端会整份拒绝；
//   - 时区与运行时长不猜默认值——猜错会让计划跑在别人的凌晨。

import { describe, expect, it } from 'vitest'

import {
  CALENDAR_PRESETS,
  DEFAULT_SCRIPT_RUNTIME_SECONDS,
  MAX_SCRIPT_BYTES,
  browserTimezone,
  buildScheduleDefinition,
  calendarPresetOf,
  emptyScheduleForm,
  emptyScriptForm,
  scheduleApplyConfirmLines,
  scheduleDeleteConfirmLines,
  scheduleFormOf,
  scheduleStateLines,
  scriptApplyConfirmLines,
  scriptByteLength,
  scriptDeleteConfirmLines,
  scriptRunConfirmLines,
  validateScheduleForm,
  validateScriptForm,
  type ScheduleFormState,
  type ScriptFormState,
} from './schedules'
import { MAX_RUNTIME_SECONDS, type ScheduleRecord } from '@/api/schedules'

function form(overrides: Partial<ScheduleFormState> = {}): ScheduleFormState {
  return {
    ...emptyScheduleForm('backup'),
    name: '每晚全量备份',
    targetId: 'pol_0123456789abcdef0123456789abcdef',
    calendarExpression: '*-*-* 03:30:00',
    timezone: 'Asia/Shanghai',
    maxRuntimeSeconds: '3600',
    ...overrides,
  }
}

function scriptForm(overrides: Partial<ScriptFormState> = {}): ScriptFormState {
  return { ...emptyScriptForm(), name: '清缓存', ...overrides }
}

function record(overrides: Partial<ScheduleRecord> = {}): ScheduleRecord {
  return {
    id: 'sch_0123456789abcdef0123456789abcdef',
    name: '每晚全量备份',
    kind: 'backup',
    target_id: 'pol_0123456789abcdef0123456789abcdef',
    definition: {
      name: '每晚全量备份',
      kind: 'backup',
      target_id: 'pol_0123456789abcdef0123456789abcdef',
      calendar_expression: '*-*-* 03:30:00',
      timezone: 'Asia/Shanghai',
      persistent: false,
      max_runtime_seconds: 3600,
      enabled: true,
    },
    enabled: true,
    version: 1,
    timer_ref: 'upanel-sch-0123456789abcdef.timer',
    created_at: '2026-10-01T00:00:00Z',
    updated_at: '2026-10-01T00:00:00Z',
    installed: true,
    unit_file_state: 'enabled',
    active_state: 'active',
    blockers: [],
    ...overrides,
  }
}

describe('表单 → 定义', () => {
  it('基本字段按合同拼装，日历里的多余空白被压成一个空格', () => {
    const def = buildScheduleDefinition(form({ calendarExpression: '  *-*-*   03:30:00  ' }))
    expect(def).toEqual({
      name: '每晚全量备份',
      kind: 'backup',
      target_id: 'pol_0123456789abcdef0123456789abcdef',
      calendar_expression: '*-*-* 03:30:00',
      timezone: 'Asia/Shanghai',
      persistent: false,
      max_runtime_seconds: 3600,
      enabled: true,
    })
    // 备份计划不带证书种类参数：带上会被服务端拒绝。
    expect(def).not.toHaveProperty('certificate')
  })

  it('证书续期才带 certificate，域名小写', () => {
    const def = buildScheduleDefinition(
      form({
        kind: 'certificate_renewal',
        targetId: 'crt_0123456789abcdef0123456789abcdef',
        certificateDomain: 'Example.COM',
        certificateStaging: true,
      }),
    )
    expect(def.certificate).toEqual({ domain: 'example.com', staging: true })
  })

  it('别的种类就算表单里残留了域名也不带 certificate', () => {
    const def = buildScheduleDefinition(form({ certificateDomain: 'stale.example.com' }))
    expect(def).not.toHaveProperty('certificate')
  })

  it('时长读不成数字时给 0（校验会拦住它，不会悄悄变成 1 秒）', () => {
    expect(buildScheduleDefinition(form({ maxRuntimeSeconds: 'abc' })).max_runtime_seconds).toBe(0)
  })
})

describe('形状校验', () => {
  it('合法表单没有问题', () => {
    expect(validateScheduleForm(form())).toEqual([])
  })

  it('计划名不能为空', () => {
    expect(validateScheduleForm(form({ name: '  ' })).join()).toContain('计划名不能为空')
  })

  it('目标 id 必须符合该种类的形状（策略 id 塞进脚本计划会被拦）', () => {
    const problems = validateScheduleForm(
      form({ kind: 'user_script', targetId: 'pol_0123456789abcdef0123456789abcdef' }),
    ).join()
    expect(problems).toContain('脚本')
  })

  it('证书续期必须写明域名：触发那一刻没有面板可问', () => {
    const problems = validateScheduleForm(
      form({ kind: 'certificate_renewal', targetId: 'crt_0123456789abcdef0123456789abcdef' }),
    ).join()
    expect(problems).toContain('域名')
  })

  it('日历表达式不能为空、不能以 - 开头、字符集要对', () => {
    expect(validateScheduleForm(form({ calendarExpression: '' })).join()).toContain('不能为空')
    expect(validateScheduleForm(form({ calendarExpression: '--help' })).join()).toContain('不能以 - 开头')
    expect(validateScheduleForm(form({ calendarExpression: '*-*-* 03:30; rm -rf /' })).join()).toContain(
      '非法字符',
    )
  })

  it('时区不能空：猜一个默认值会让计划跑在别人的凌晨', () => {
    expect(validateScheduleForm(form({ timezone: '  ' })).join()).toContain('时区')
  })

  it('时区只接受 IANA 名的字符', () => {
    expect(validateScheduleForm(form({ timezone: 'Asia/Shang hai' })).join()).toContain('时区')
  })

  it('运行时长上限是 1 秒到 24 小时', () => {
    expect(validateScheduleForm(form({ maxRuntimeSeconds: '0' })).join()).toContain('运行上限')
    expect(validateScheduleForm(form({ maxRuntimeSeconds: String(MAX_RUNTIME_SECONDS + 1) })).join()).toContain(
      '运行上限',
    )
    expect(validateScheduleForm(form({ maxRuntimeSeconds: String(MAX_RUNTIME_SECONDS) }))).toEqual([])
  })

  it('形状对但语义可疑的不拦（这不是前端能裁决的事）', () => {
    // "每天 25 点"形状合法；认不认由 systemd-analyze 说。
    expect(validateScheduleForm(form({ calendarExpression: '*-*-* 25:61:00' }))).toEqual([])
  })
})

describe('日历预设', () => {
  it('预设表达式都能反查回自己的标签', () => {
    for (const preset of CALENDAR_PRESETS) {
      expect(calendarPresetOf(preset.expression)).toBe(preset.label)
    }
  })

  it('手写的表达式没有对应预设', () => {
    expect(calendarPresetOf('*-*-* 04:17:00')).toBeNull()
  })
})

describe('脚本表单', () => {
  it('默认正文是一份能跑的骨架，且不带 NUL', () => {
    const state = emptyScriptForm()
    expect(state.content.startsWith('#!/bin/sh')).toBe(true)
    expect(state.content).not.toContain('\u0000')
    expect(scriptByteLength(state.content)).toBeGreaterThan(0)
  })

  it('字节数按 UTF-8 计（中文一个字三个字节）', () => {
    expect(scriptByteLength('中')).toBe(3)
  })

  it('名字与正文都不能空', () => {
    expect(validateScriptForm(scriptForm({ name: ' ' })).join()).toContain('脚本名不能为空')
    expect(validateScriptForm(scriptForm({ content: '   ' })).join()).toContain('正文不能为空')
  })

  it('正文超过 64 KiB 被拦下，并说出当前多少', () => {
    const problems = validateScriptForm(scriptForm({ content: 'a'.repeat(MAX_SCRIPT_BYTES + 1) })).join()
    expect(problems).toContain('64 KiB')
    expect(problems).toContain('当前')
  })

  it('正文含 NUL 被拦下', () => {
    expect(validateScriptForm(scriptForm({ content: '#!/bin/sh\n\u0000' })).join()).toContain('NUL')
  })
})

describe('确认文案', () => {
  it('保存计划时说清这一版会不会真的跑起来', () => {
    const enabled = scheduleApplyConfirmLines(form(), '备份策略「每晚」').join('\n')
    expect(enabled).toContain('备份策略「每晚」')
    expect(enabled).toContain('到点就真的会跑')

    const disabled = scheduleApplyConfirmLines(form({ enabled: false }), '').join('\n')
    expect(disabled).toContain('不会自己起来')
  })

  it('证书续期多说两句：ACME 环境与 --keep-until-expiring', () => {
    const lines = scheduleApplyConfirmLines(
      form({
        kind: 'certificate_renewal',
        targetId: 'crt_0123456789abcdef0123456789abcdef',
        certificateDomain: 'example.com',
      }),
      '证书 example.com',
    ).join('\n')
    expect(lines).toContain('正式环境')
    expect(lines).toContain('keep-until-expiring')
  })

  it('删除计划说清"删的是单元与记录，目标对象不动"', () => {
    const lines = scheduleDeleteConfirmLines({
      name: '每晚全量备份',
      timer_ref: 'upanel-sch-x.timer',
    }).join('\n')
    expect(lines).toContain('upanel-sch-x.timer')
    expect(lines).toContain('不会跟着被删')
  })

  it('立即运行脚本说清身份、指纹与输出不落库', () => {
    const lines = scriptRunConfirmLines(
      { name: '清缓存', content_hash: 'sha256:abcd' },
      DEFAULT_SCRIPT_RUNTIME_SECONDS,
    ).join('\n')
    expect(lines).toContain('upanel-shell')
    expect(lines).toContain('不是 root')
    expect(lines).toContain('sha256:abcd')
    expect(lines).toContain('输出不落库')
  })

  it('保存脚本说清解释器由代理决定', () => {
    const lines = scriptApplyConfirmLines(scriptForm()).join('\n')
    expect(lines).toContain('解释器由代理决定')
    expect(lines).toContain('拿不到 root')
  })

  it('删除脚本提醒先摘掉还挂在它上面的计划', () => {
    expect(scriptDeleteConfirmLines({ name: '清缓存' }).join('\n')).toContain('计划')
  })
})

describe('现状摘要', () => {
  it('读得到下一次触发时如实说，读不到也如实说', () => {
    const withNext = scheduleStateLines(record({ next_elapse: '2026-10-25T03:30:00Z' })).join('\n')
    expect(withNext).toContain('2026-10-25T03:30:00Z')

    const without = scheduleStateLines(record()).join('\n')
    expect(without).toContain('读不到')
  })

  it('有阻塞项时说清"就算到点也不会执行"', () => {
    const lines = scheduleStateLines(
      record({ blockers: [{ code: 'SCHEDULE_TARGET_NOT_FOUND', message: '目标不存在' }] }),
    ).join('\n')
    expect(lines).toContain('不会执行')
  })

  it('三个观测字段分开出现在摘要里', () => {
    const lines = scheduleStateLines(record()).join('\n')
    expect(lines).toContain('已启用并在运行')
    expect(lines).toContain('upanel-sch-0123456789abcdef.timer')
  })
})

describe('时区探测', () => {
  it('拿不到时给空串，让人自己填，不猜一个默认值', () => {
    const zone = browserTimezone()
    expect(typeof zone).toBe('string')
    // 拿不到的情况有兜底；拿到就是 IANA 名。
    if (zone !== '') expect(zone).toMatch(/^[A-Za-z][A-Za-z0-9_+/-]*$/)
  })

  it('摊回表单时保留定义里的时区，不用浏览器时区覆盖它', () => {
    const state = scheduleFormOf(record())
    expect(state.timezone).toBe('Asia/Shanghai')
  })
})
