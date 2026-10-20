// 任务中心纯逻辑测试。
//
// 这些函数是页面与事件流共用的判决逻辑：状态机、取消规则、日志合并。
// 一旦它们和 CONTRACTS.md 4.3 不一致，页面上的每个按钮和每条状态都会跟着错。

import { describe, expect, it } from 'vitest'

import {
  JOB_STATUSES,
  JOB_STATUS_LABELS,
  JOB_STATUS_TYPES,
  canCancel,
  formatTime,
  isTerminal,
  jobErrorMessage,
  mergeLogs,
  progressText,
  trimLogs,
  type JobDTO,
  type JobLogEntry,
} from './jobs'

function job(overrides: Partial<JobDTO> = {}): JobDTO {
  return {
    job_id: 'job_0123456789abcdef0123456789abcdef',
    action: 'acceptance.probe',
    status: 'running',
    step: 'ping',
    progress_percent: 40,
    result: {},
    error: {},
    cancellable: true,
    cancellation_requested: false,
    created_at: '2026-10-20T09:00:00Z',
    updated_at: '2026-10-20T09:00:02Z',
    started_at: '2026-10-20T09:00:01Z',
    finished_at: null,
    ...overrides,
  }
}

describe('状态机', () => {
  it('每个契约状态都有中文标签与标签色', () => {
    for (const status of JOB_STATUSES) {
      expect(JOB_STATUS_LABELS[status], status).toBeTruthy()
      expect(JOB_STATUS_TYPES[status], status).toBeTruthy()
    }
    expect(JOB_STATUSES).toHaveLength(6)
  })

  it('终态只有四个：succeeded/failed/canceled/interrupted', () => {
    expect(JOB_STATUSES.filter((s) => isTerminal(s))).toEqual([
      'succeeded',
      'failed',
      'canceled',
      'interrupted',
    ])
    expect(isTerminal('queued')).toBe(false)
    expect(isTerminal('running')).toBe(false)
  })
})

describe('取消规则', () => {
  it('管理员 + 可取消 + 未结束 → 显示取消', () => {
    expect(canCancel(job(), 'admin')).toBe(true)
  })

  it('不可取消的动作不显示取消按钮（点了只会 409）', () => {
    expect(canCancel(job({ cancellable: false }), 'admin')).toBe(false)
  })

  it('viewer 与未登录都不显示取消', () => {
    expect(canCancel(job(), 'viewer')).toBe(false)
    expect(canCancel(job(), null)).toBe(false)
  })

  it('终态与已请求取消都不再显示取消', () => {
    expect(canCancel(job({ status: 'succeeded' }), 'admin')).toBe(false)
    expect(canCancel(job({ status: 'interrupted' }), 'admin')).toBe(false)
    expect(canCancel(job({ cancellation_requested: true }), 'admin')).toBe(false)
  })
})

describe('进度', () => {
  it('null 是“未上报”，不是 0%', () => {
    expect(progressText(job({ progress_percent: null }))).toBe('未上报')
    expect(progressText(job({ progress_percent: 0 }))).toBe('0%')
    expect(progressText(job({ progress_percent: 100 }))).toBe('100%')
  })
})

describe('错误详情', () => {
  it('没有错误时返回 null，不显示空壳错误框', () => {
    expect(jobErrorMessage(job())).toBeNull()
  })

  it('code 与 message 原样呈现，并按已知码给出处置提示', () => {
    const detail = jobErrorMessage(
      job({ status: 'failed', error: { code: 'AGENT_UNAVAILABLE', message: '代理不可达' } }),
    )
    expect(detail?.code).toBe('AGENT_UNAVAILABLE')
    expect(detail?.message).toBe('代理不可达')
    expect(detail?.hint).toContain('代理不可达或没有这条操作记录')
  })

  it('中断的任务提示先核对实际状态，而不是直接重试', () => {
    const detail = jobErrorMessage(
      job({ status: 'interrupted', error: { code: 'OPERATION_INTERRUPTED', message: '执行期间重启' } }),
    )
    expect(detail?.hint).toContain('结果未知')
  })

  it('缺少 message 时不留空白', () => {
    const detail = jobErrorMessage(job({ error: { code: 'COMMAND_FAILED' } }))
    expect(detail?.message).toBe('（代理未提供说明）')
  })

  it('缺少 code 时标成 UNKNOWN，不猜具体原因', () => {
    const detail = jobErrorMessage(job({ error: { message: '说不清的失败' } }))
    expect(detail?.code).toBe('UNKNOWN')
  })
})

describe('日志合并', () => {
  const entry = (seq: number, kind: JobLogEntry['kind'] = 'log'): JobLogEntry => ({
    seq,
    kind,
    payload: { line: seq },
    created_at: '2026-10-20T09:00:00Z',
  })

  it('按 seq 去重并升序：同一行从分页和实时流各来一次也只显示一次', () => {
    const merged = mergeLogs([entry(3), entry(1)], [entry(2), entry(3)])
    expect(merged.map((e) => e.seq)).toEqual([1, 2, 3])
    expect(merged).toHaveLength(3)
  })

  it('同 seq 以新到达的内容为准（重连补发时刷新覆盖旧值）', () => {
    const merged = mergeLogs([{ ...entry(1), payload: { line: 'old' } }], [
      { ...entry(1), payload: { line: 'new' } },
    ])
    expect(merged[0]?.payload.line).toBe('new')
  })

  it('裁剪只保留最近 max 条', () => {
    const many = Array.from({ length: 10 }, (_, index) => entry(index + 1))
    expect(trimLogs(many, 3).map((e) => e.seq)).toEqual([8, 9, 10])
    expect(trimLogs(many, 100)).toHaveLength(10)
  })
})

describe('时间显示', () => {
  it('空值与 null 显示破折号，不显示 1970', () => {
    expect(formatTime(null)).toBe('—')
    expect(formatTime(undefined)).toBe('—')
    expect(formatTime('')).toBe('—')
  })

  it('无法解析的值原样返回，不显示 Invalid Date', () => {
    expect(formatTime('not-a-time')).toBe('not-a-time')
  })
})
