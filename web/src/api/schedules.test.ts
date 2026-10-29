// 计划与脚本的 DTO、动作参数与翻译测试（P12.5）。
//
// 钉住的东西：
//   1. `schedule.apply` 新建时 `schedule_id` 必须是 **null**（不是缺字段、也不是空串），
//      version 必须是 0；修改时 version 是乐观并发令牌。写错这两格，代价是一次
//      "新建变成了改别人的计划"或者一次注定的版本冲突。
//   2. 证书续期的种类参数只在证书续期时出现：别的种类带上它，服务端会**拒绝**整份定义。
//   3. 三个观测字段（installed / unit_file_state / active_state）合成的那句话不能
//      把"定义了要开"说成"在跑"。

import { afterEach, describe, expect, it, vi } from 'vitest'

import {
  SCHEDULE_APPLY_ACTION,
  SCHEDULE_DELETE_ACTION,
  SCHEDULE_KINDS,
  SCHEDULE_KIND_LABELS,
  SCHEDULE_TARGET_PATTERNS,
  SCRIPT_APPLY_ACTION,
  SCRIPT_DELETE_ACTION,
  SCRIPT_RUN_ACTION,
  canRenewCertificate,
  canRenewCertificateReason,
  fetchCertificateOptions,
  fetchScript,
  runtimeLabel,
  scheduleApplyParams,
  scheduleDeleteParams,
  scheduleRuntimeLabel,
  scriptApplyParams,
  scriptDeleteParams,
  scriptRunParams,
  triggerOutcomeLabel,
  type ScheduleDefinition,
  type ScheduleRecord,
} from './schedules'

afterEach(() => {
  vi.unstubAllGlobals()
})

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json', 'X-Request-ID': 'rid-schedule' },
  })
}

function stubFetch(body: unknown = {}): ReturnType<typeof vi.fn> {
  const mock = vi.fn(() => Promise.resolve(jsonResponse({ data: body, request_id: 'rid' })))
  vi.stubGlobal('fetch', mock)
  return mock
}

function definition(overrides: Partial<ScheduleDefinition> = {}): ScheduleDefinition {
  return {
    name: '每晚全量备份',
    kind: 'backup',
    target_id: 'pol_0123456789abcdef0123456789abcdef',
    calendar_expression: '*-*-* 03:30:00',
    timezone: 'Asia/Shanghai',
    persistent: false,
    max_runtime_seconds: 3600,
    enabled: true,
    ...overrides,
  }
}

function record(overrides: Partial<ScheduleRecord> = {}): ScheduleRecord {
  return {
    id: 'sch_0123456789abcdef0123456789abcdef',
    name: '每晚全量备份',
    kind: 'backup',
    target_id: 'pol_0123456789abcdef0123456789abcdef',
    definition: definition(),
    enabled: true,
    version: 2,
    timer_ref: 'upanel-sch-0123456789abcdef.timer',
    created_at: '2026-10-01T00:00:00Z',
    updated_at: '2026-10-20T00:00:00Z',
    installed: true,
    unit_file_state: 'enabled',
    active_state: 'active',
    blockers: [],
    ...overrides,
  }
}

describe('动作名与闭集', () => {
  it('与 CONTRACTS §12 的动作名逐字一致', () => {
    expect(SCHEDULE_APPLY_ACTION).toBe('schedule.apply')
    expect(SCHEDULE_DELETE_ACTION).toBe('schedule.delete')
    expect(SCRIPT_APPLY_ACTION).toBe('script.apply')
    expect(SCRIPT_DELETE_ACTION).toBe('script.delete')
    expect(SCRIPT_RUN_ACTION).toBe('script.run')
  })

  it('三种计划种类各有自己的标签与目标 id 形状', () => {
    expect([...SCHEDULE_KINDS]).toEqual(['backup', 'certificate_renewal', 'user_script'])
    expect(Object.keys(SCHEDULE_KIND_LABELS).sort()).toEqual([
      'backup',
      'certificate_renewal',
      'user_script',
    ])
    expect(SCHEDULE_TARGET_PATTERNS.backup.test('pol_0123456789abcdef0123456789abcdef')).toBe(true)
    expect(SCHEDULE_TARGET_PATTERNS.certificate_renewal.test('crt_0123456789abcdef0123456789abcdef')).toBe(true)
    expect(SCHEDULE_TARGET_PATTERNS.user_script.test('scr_0123456789abcdef0123456789abcdef')).toBe(true)
    // 种类之间不能互相顶替：把策略 id 填进脚本计划会被形状挡下。
    expect(SCHEDULE_TARGET_PATTERNS.user_script.test('pol_0123456789abcdef0123456789abcdef')).toBe(false)
  })
})

describe('schedule.apply 的参数', () => {
  it('新建时 schedule_id 是 null、version 是 0', () => {
    const params = scheduleApplyParams(definition())
    expect(params.schedule_id).toBeNull()
    expect(params.version).toBe(0)
    expect(params.definition).toMatchObject({ name: '每晚全量备份', kind: 'backup' })
  })

  it('修改时带 id 与当前 version', () => {
    const params = scheduleApplyParams(definition(), { id: 'sch_x', version: 5 })
    expect(params).toMatchObject({ schedule_id: 'sch_x', version: 5 })
  })

  it('删除只带 id 与 version', () => {
    expect(scheduleDeleteParams({ id: 'sch_x', version: 3 })).toEqual({ schedule_id: 'sch_x', version: 3 })
  })
})

describe('脚本动作参数', () => {
  it('script.apply 新建 / 修改', () => {
    const fresh = scriptApplyParams({ name: '清缓存', content: '#!/bin/sh\n' })
    expect(fresh).toEqual({ script_id: null, version: 0, name: '清缓存', content: '#!/bin/sh\n' })
    const edit = scriptApplyParams({ name: '清缓存', content: 'x' }, { id: 'scr_x', version: 4 })
    expect(edit).toEqual({ script_id: 'scr_x', version: 4, name: '清缓存', content: 'x' })
  })

  it('script.run 带 id、version 与这一次的时长上限', () => {
    expect(scriptRunParams({ id: 'scr_x', version: 2 }, 600)).toEqual({
      script_id: 'scr_x',
      version: 2,
      max_runtime_seconds: 600,
    })
  })

  it('script.delete 只带 id 与 version', () => {
    expect(scriptDeleteParams({ id: 'scr_x', version: 2 })).toEqual({ script_id: 'scr_x', version: 2 })
  })
})

describe('读接口的响应形状', () => {
  it('GET /scripts/:id 的正文在 script 这一层里（列表不带正文）', async () => {
    const mock = stubFetch({ script: { id: 'scr_x', name: '清缓存', content: '#!/bin/sh\n' } })
    const result = await fetchScript('scr_x')
    expect(String((mock.mock.calls[0] as unknown as [string])[0])).toMatch(/\/api\/v1\/scripts\/scr_x$/)
    expect(result.data.script.content).toBe('#!/bin/sh\n')
  })

  it('证书目录只取一页，并把 next_cursor 翻成 truncated', async () => {
    stubFetch({ items: [{ id: 'crt_x', domain: 'example.com', source: 'certbot', staging: false }], next_cursor: 'c2' })
    const result = await fetchCertificateOptions()
    expect(result.data.truncated).toBe(true)
    expect(result.data.items).toHaveLength(1)
  })

  it('没有下一页时 truncated 是 false', async () => {
    stubFetch({ items: [], next_cursor: null })
    const result = await fetchCertificateOptions()
    expect(result.data.truncated).toBe(false)
  })
})

describe('证书能不能续', () => {
  it('certbot 签的能续，手动导入的不能且给出理由', () => {
    expect(canRenewCertificate({ source: 'certbot' })).toBe(true)
    expect(canRenewCertificateReason({ source: 'manual' })).toContain('手动导入')
    expect(canRenewCertificateReason({ source: 'certbot' })).toBeNull()
  })
})

describe('三个观测字段的说法', () => {
  it('没装单元时先说的是"没装"，不谈 enable 与 active', () => {
    expect(
      scheduleRuntimeLabel({ installed: false, unit_file_state: 'enabled', active_state: 'active' }),
    ).toBe('单元未安装')
  })

  it('enabled 打开但 timer 没在跑，必须说成坏状态而不是绿灯', () => {
    expect(
      scheduleRuntimeLabel({ installed: true, unit_file_state: 'enabled', active_state: 'inactive' }),
    ).toBe('已 enable 但 timer 没在跑')
  })

  it('在跑但没 enable：重启后不会自己起来', () => {
    expect(
      scheduleRuntimeLabel({ installed: true, unit_file_state: 'disabled', active_state: 'active' }),
    ).toBe('在运行但没 enable（重启后不会自己起来）')
  })

  it('两者都好才是"已启用并在运行"', () => {
    expect(
      scheduleRuntimeLabel({ installed: true, unit_file_state: 'enabled', active_state: 'active' }),
    ).toBe('已启用并在运行')
  })
})

describe('触发结果与时长', () => {
  it('四种结果按闭集翻译，未知值原样返回', () => {
    expect(triggerOutcomeLabel('triggered')).toBe('已提交操作')
    expect(triggerOutcomeLabel('blocked')).toBe('被拦下')
    expect(triggerOutcomeLabel('duplicate')).toBe('跳过（同一时刻已触发过）')
    expect(triggerOutcomeLabel('failed')).toBe('提交失败')
    expect(triggerOutcomeLabel('whatever')).toBe('whatever')
  })

  it('时长按人话翻译', () => {
    expect(runtimeLabel(0)).toBe('—')
    expect(runtimeLabel(45)).toBe('45 秒')
    expect(runtimeLabel(600)).toBe('10 分钟')
    expect(runtimeLabel(3600)).toBe('1 小时')
    expect(runtimeLabel(5400)).toBe('1 小时 30 分')
  })

  it('一条计划的 version 就是它的并发令牌（记录里的值不会被翻译函数改掉）', () => {
    expect(record({ version: 9 }).version).toBe(9)
  })
})
