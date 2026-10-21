// 用真实 journald 日志页与真实 SSE 帧渲染日志组件（P05.3）。
//
// 这里的 JSON 与帧不是造的：它们是验收 VM（upanel-2404，Ubuntu 24.04.5 / systemd 255）
// 上真实运行 upanel-web（production 模式、root 代理）+ 真实 journald 之后，由
// tests/integration/p05-3-logs-acceptance.sh 抓回来的响应与 curl 原始字节，原样存在
// src/fixtures/p05-3-vm/ 下（连 {data, request_id} / {error, request_id} 外壳都没拆）。
//
// 为什么需要这一层：logs.test.ts 里的是**手写形状样本**，它验“每种状态怎么画”；
// 这里验“服务端真的回了这些字节，页面能不能画”。两者不能互相顶替——手写样本能覆盖
// binary / truncated 这类真机没造出来的形态，真机样本能抓到手写样本里被我写错的假设。
// 本轮就抓到过：错误消息曾经是三层前缀套娃（“journal: 查询参数不合法: agent: 参数不合法:
// journal: 查询参数不合法: …”），手写样本里那句话是我自己写的，所以永远对得上。
//
// 时间相关的断言一律用“关系”而不是绝对值：每一轮验收的游标、时间戳、PID 都不同，
// 钉死具体值等于让这套用例只能用一次。

import { readFileSync } from 'node:fs'

import { describe, expect, it, vi } from 'vitest'
import { createSSRApp, h, type Component } from 'vue'
import { renderToString } from 'vue/server-renderer'

import {
  LOG_LEVELS,
  entryPlainText,
  logLevelLabel,
  pageEntries,
  parseLogStreamFrame,
  summarizeLogPage,
  type LogEntry,
  type LogPage,
} from '@/api/logs'
import LogEntryRow from './LogEntryRow.vue'

// LogEntryRow 用 NTag 画级别与标记；SSR 里 naive-ui 的样式挂载需要 document。
// 这里只要文字，所以把标签换成一个原样渲染子节点的最小实现。
vi.mock('naive-ui', async () => {
  const { h } = await import('vue')
  const stub = (name: string) => ({
    name: `Stub${name}`,
    props: ['title', 'description', 'status', 'code'],
    setup(
      _props: unknown,
      ctx: { slots: { default?: () => unknown } },
    ) {
      return () => h('div', { 'data-naive': name }, ctx.slots.default?.() as never)
    },
  })
  return { NTag: stub('tag') }
})

function readText(name: string): string {
  return readFileSync(new URL(`../fixtures/p05-3-vm/${name}`, import.meta.url), 'utf8')
}

function readJson(name: string): Record<string, unknown> {
  return JSON.parse(readText(name)) as Record<string, unknown>
}

function fixturePage(name: string): LogPage {
  return readJson(name)['data'] as LogPage
}

function fixtureError(name: string): { code: string; message: string } {
  return readJson(name)['error'] as { code: string; message: string }
}

function requestID(name: string): string {
  return readJson(name)['request_id'] as string
}

async function render(component: Component, props: Record<string, unknown>): Promise<string> {
  const app = createSSRApp({ render: () => h(component, props) })
  const html = await renderToString(app)
  // scoped 样式加的 data-v-* 只作用于 DOM，留着它会让断言变成测编译产物。
  return html.replace(/ data-v-[0-9a-z]+(?=[ >])/g, '')
}

/** RFC3339（UTC，秒可有可无小数）。 */
const RFC3339 = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?Z$/

function at(value: string): number {
  return new Date(value).getTime()
}

function spanSeconds(page: LogPage): number {
  return (at(page.to) - at(page.from)) / 1000
}

/** 条目里的 level 必须在闭集内：多一个值就是 DTO 漂移。 */
const LEVEL_CLOSED = new Set<string>(LOG_LEVELS)

/** 三种 syslog 严重级别（level= 是下限语义，不是相等）。 */
const AT_LEAST_ERROR = new Set(['emergency', 'alert', 'critical', 'error'])

const basic = fixturePage('logs-basic.json')
const kernel = fixturePage('logs-kernel.json')
const levelError = fixturePage('logs-level-error.json')
const otherUnit = fixturePage('logs-other-unit.json')
const limit5 = fixturePage('logs-limit5.json')
const defaultWindow = fixturePage('logs-default-window.json')
const fromOnly = fixturePage('logs-from-only.json')
const toOnly = fixturePage('logs-to-only.json')

const PAGES = [1, 2, 3, 4, 5, 6, 7, 8].map((n) => fixturePage(`logs-page-${n}.json`))

/** 验收脚本写了 30 条可定位的标记行。 */
const MARKERS = Array.from({ length: 30 }, (_, i) => i + 1)

function messages(page: LogPage): string[] {
  return pageEntries(page).map((entry) => entry.message)
}

/**
 * 数每个 marker-i=N 出现了几次。
 *
 * 必须按数字整词匹配：用 `includes('marker-i=1')` 会把 `marker-i=10`…`marker-i=19`
 * 一起算进去，于是“每个标记只出现一次”这条断言永远失败——错的是测试，不是产品。
 */
function markerCounts(source: string[]): Map<number, number> {
  const counts = new Map<number, number>()
  for (const message of source) {
    for (const match of message.matchAll(/marker-i=(\d+)/g)) {
      const n = Number(match[1])
      counts.set(n, (counts.get(n) ?? 0) + 1)
    }
  }
  return counts
}

/** 验收脚本写进日志的三条特殊行（含两条带秘密的）。 */
const SECRET_LINES = ['password=hunter2secret 登录失败', 'token=abcdef123456 已下发']

describe('真实 journald 日志页', () => {
  it('页字段与契约一致：source/from/to/entries/next_cursor/truncated/scanned/duration_ms', () => {
    const base = ['source', 'from', 'to', 'entries', 'next_cursor', 'truncated', 'scanned', 'duration_ms']
    for (const page of [basic, kernel, levelError, otherUnit, limit5, toOnly]) {
      const keys = Object.keys(page)
      for (const key of base) expect(keys).toContain(key)
      // unit / level / truncated_reason 只在给了、或真的截断时出现。
      if (page.unit !== undefined) expect(typeof page.unit).toBe('string')
      if (page.level !== undefined) expect(LEVEL_CLOSED.has(page.level)).toBe(true)
      expect(page.from).toMatch(RFC3339)
      expect(page.to).toMatch(RFC3339)
      expect(Array.isArray(page.entries) || page.entries === null).toBe(true)
      expect(Number.isInteger(page.scanned)).toBe(true)
      expect(Number.isInteger(page.duration_ms)).toBe(true)
      expect(page.duration_ms).toBeGreaterThanOrEqual(0)
    }
  })

  it('条目字段就是那一组：可选的三项是 unit/identifier/comm，pid 允许为 null', () => {
    const required = ['binary', 'cursor', 'level', 'message', 'pid', 'redacted', 'timestamp', 'truncated']
    const optional = ['comm', 'identifier', 'unit']
    for (const entry of pageEntries(basic)) {
      const keys = Object.keys(entry).sort()
      for (const key of required) expect(keys).toContain(key)
      for (const key of keys) expect([...required, ...optional]).toContain(key)
      expect(LEVEL_CLOSED.has(entry.level)).toBe(true)
      expect(entry.timestamp).toMatch(RFC3339)
      expect(entry.cursor).not.toBe('')
      expect(typeof entry.truncated).toBe('boolean')
      expect(typeof entry.redacted).toBe('boolean')
      expect(typeof entry.binary).toBe('boolean')
      // pid 存在时是正整数；journald 没给 sender 时是 null，不是 0。
      if (entry.pid !== null && entry.pid !== undefined) expect(entry.pid).toBeGreaterThan(0)
    }
  })

  it('unit 过滤是限制性的：只有该单元的行，另有 systemd 的 init.scope 生命周期行', () => {
    const unit = basic.unit as string
    expect(unit).toMatch(/^upanel-accept-logs-\d+\.service$/)
    for (const entry of pageEntries(basic)) {
      expect([unit, 'init.scope']).toContain(entry.unit)
      // init.scope 的行必须正文里提到了这个单元——否则就是没被过滤干净的别人的日志。
      if (entry.unit === 'init.scope') expect(entry.message).toContain(unit)
    }
    // 换个 unit 查：既没有标记行，也没有 big- 行，来源是另一个单元。
    expect(otherUnit.unit).toBe('ssh.service')
    for (const message of messages(otherUnit)) {
      expect(message).not.toContain('marker-i=')
      expect(message).not.toContain('big-')
    }
  })

  it('降序、游标唯一、30 个标记各出现一次：翻页的根据都在页里', () => {
    const entries = pageEntries(basic)
    expect(entries.length).toBe(basic.scanned)
    const cursors = new Set(entries.map((entry) => entry.cursor))
    expect(cursors.size).toBe(entries.length)
    for (let i = 1; i < entries.length; i += 1) {
      expect(at(entries[i - 1]!.timestamp)).toBeGreaterThanOrEqual(at(entries[i]!.timestamp))
    }
    const seen = markerCounts(messages(basic))
    expect([...seen.keys()].sort((a, b) => a - b)).toEqual(MARKERS)
    expect([...seen.values()].every((count) => count === 1)).toBe(true)
  })

  it('秘密被抹掉且显式标记：原文一个字节都不剩，redacted 恰好两条', () => {
    const redacted = pageEntries(basic).filter((entry) => entry.redacted)
    expect(redacted).toHaveLength(2)
    for (const entry of redacted) {
      expect(entry.message).toContain('[redacted]')
    }
    const all = JSON.stringify(basic)
    // 秘密真的不在载荷里——包括被截断、被转义后的任何形态。
    expect(all).not.toContain('hunter2secret')
    expect(all).not.toContain('abcdef123456')
    for (const line of SECRET_LINES) expect(all).not.toContain(line)
  })

  it('level= 是下限语义：error 只回 error 及更严重，info 同时包含 error', () => {
    expect(levelError.level).toBe('error')
    for (const entry of pageEntries(levelError)) {
      expect(AT_LEAST_ERROR.has(entry.level)).toBe(true)
    }
    expect(messages(levelError).some((m) => m.startsWith('marker-err-1'))).toBe(true)
    expect(messages(levelError).some((m) => m.includes('marker-i='))).toBe(false)
  })

  it('内核日志源：source=kernel、不带 unit 过滤、pid 为 null', () => {
    expect(kernel.source).toBe('kernel')
    expect(kernel.unit).toBeUndefined()
    const entries = pageEntries(kernel)
    expect(entries.length).toBeGreaterThan(0)
    for (const entry of entries) {
      expect(entry.pid).toBeNull()
      expect(entry.unit).toBeUndefined()
      expect(entry.message).toContain('内核日志注入')
    }
  })

  it('翻页链：8 页无重复、next_cursor 就是本页最后一条、标记跨页恰好一次', () => {
    const sizes = PAGES.map((page) => pageEntries(page).length)
    expect(sizes).toEqual([5, 5, 5, 5, 5, 5, 5, 1])

    const allCursors: string[] = []
    for (const [index, page] of PAGES.entries()) {
      const entries = pageEntries(page)
      allCursors.push(...entries.map((entry) => entry.cursor))
      // 每页的顺序仍是降序（翻页不改变顺序）。
      for (let i = 1; i < entries.length; i += 1) {
        expect(at(entries[i - 1]!.timestamp)).toBeGreaterThanOrEqual(at(entries[i]!.timestamp))
      }
      const last = index === PAGES.length - 1
      if (last) {
        expect(page.next_cursor).toBeNull()
      } else {
        // 有下一页：next_cursor 必须是本页**最后一条**的游标，
        // 不然下一页会重来一遍或者跳过几条。
        expect(page.next_cursor).toBe(entries[entries.length - 1]!.cursor)
        expect(page.truncated).toBe(true)
      }
    }
    expect(new Set(allCursors).size).toBe(allCursors.length)
    const seen = markerCounts(PAGES.flatMap(messages))
    expect([...seen.keys()].sort((a, b) => a - b)).toEqual(MARKERS)
    expect([...seen.values()].every((count) => count === 1)).toBe(true)
  })

  it('最后一页：扫到 2 条却只给出 1 条，truncated=false —— 窗口是按时间在本地过滤的', () => {
    const last = PAGES[PAGES.length - 1]!
    expect(pageEntries(last)).toHaveLength(1)
    expect(last.scanned).toBe(2)
    expect(last.truncated).toBe(false)
    // 给出的那条一定落在窗口里；这才是“少一条”和“丢一条”的区别。
    for (const entry of pageEntries(last)) {
      expect(at(entry.timestamp)).toBeGreaterThanOrEqual(at(last.from))
      expect(at(entry.timestamp)).toBeLessThanOrEqual(at(last.to))
    }
  })

  it('limit=5 的页与翻页第一页同形：5 条、被截断、有下一页', () => {
    expect(pageEntries(limit5)).toHaveLength(5)
    expect(limit5.truncated).toBe(true)
    expect(limit5.next_cursor).toBe(pageEntries(limit5)[4]!.cursor)
    expect(limit5.next_cursor).toBe(PAGES[0]!.next_cursor)
  })

  it('时间窗口：默认正好 1 小时；只给 from 时 to 取当前时间；只给 to 时是 to 前 1 小时', () => {
    expect(spanSeconds(defaultWindow)).toBe(3600)
    // from-only：to 由服务端补成“现在”，所以跨度是 now-from（脚本刚写过日志，只有几秒），
    // 而且 to 带亚秒精度——一个“from+1h”的补法不会长这样。
    expect(spanSeconds(fromOnly)).toBeGreaterThanOrEqual(0)
    expect(spanSeconds(fromOnly)).toBeLessThan(3600)
    expect(fromOnly.to).toMatch(/\.\d+Z$/)
    expect(fromOnly.to).not.toBe(defaultWindow.to)
    // to-only：跨度正好 1 小时；那个时段没有日志 → 空数组而不是 null。
    expect(spanSeconds(toOnly)).toBe(3600)
    expect(toOnly.entries).toEqual([])
    expect(toOnly.scanned).toBe(0)
    expect(toOnly.next_cursor).toBeNull()
  })
})

describe('真实错误信封', () => {
  it('每一种拒绝都带错误码与 request_id，消息里只有一层来源说明', () => {
    const cases: Array<[string, string]> = [
      ['err-cursor-raw.json', 'VALIDATION_ERROR'],
      ['err-span.json', 'VALIDATION_ERROR'],
      ['err-future.json', 'VALIDATION_ERROR'],
      ['err-level-unknown.json', 'VALIDATION_ERROR'],
      ['err-no-auth.json', 'UNAUTHENTICATED'],
      ['logs-viewer.json', 'FORBIDDEN'],
    ]
    for (const [name, code] of cases) {
      expect(fixtureError(name).code).toBe(code)
      expect(fixtureError(name).message).not.toBe('')
      expect(requestID(name)).toMatch(/^[0-9a-f]{32}$/)
    }
  })

  it('参数错误的消息不套娃：出现过的层名前缀最多一次', () => {
    for (const name of ['err-future.json', 'err-span.json', 'err-level-unknown.json']) {
      const message = fixtureError(name).message
      // 曾经的形态：“journal: 查询参数不合法: agent: 参数不合法: journal: 查询参数不合法: …”
      expect(message.split('查询参数不合法').length - 1).toBeLessThanOrEqual(1)
      expect(message).not.toContain('agent:')
    }
  })

  it('未转义的游标不是“没有游标”：说清楚要 percent-encode', () => {
    const message = fixtureError('err-cursor-raw.json').message
    expect(message).toContain('%3B')
  })
})

describe('代理能力表', () => {
  it('logs.query 在登记表里，动作表里没有日志相关动作', () => {
    const data = readJson('agent-capabilities.json')['data'] as {
      mode: string
      supported_queries: string[]
      supported_actions: string[]
    }
    expect(data.mode).toBe('production')
    expect(data.supported_queries).toContain('logs.query')
    // 日志是只读的：能力表里不该出现任何 “log” 开头的动作。
    expect(data.supported_actions.filter((name) => name.startsWith('log'))).toEqual([])
  })
})

// ---------------------------------------------------------------- 渲染

/** 从真实页里按消息片段找一条条目；找不到就是夹具变了，直接失败。 */
function entryWith(page: LogPage, needle: string): LogEntry {
  const hit = pageEntries(page).find((entry) => entry.message.includes(needle))
  if (hit === undefined) throw new Error(`夹具里没有包含 ${needle} 的条目`)
  return hit
}

describe('用真实条目渲染日志行', () => {
  it('普通条目：时间、级别、来源、正文都来自这条真实日志', async () => {
    const entry = entryWith(basic, 'marker-i=30')
    const html = await render(LogEntryRow, { entry })
    expect(html).toContain(entry.message)
    expect(html).toContain(logLevelLabel(entry.level))
    expect(html).toContain(entry.unit as string)
    expect(html).not.toContain('已过滤')
    expect(html).not.toContain('无文本')
  })

  it('被过滤的条目挂“已过滤”，正文里没有原始口令', async () => {
    const entry = entryWith(basic, 'password=')
    expect(entry.redacted).toBe(true)
    const html = await render(LogEntryRow, { entry })
    expect(html).toContain('已过滤')
    expect(html).toContain('password=[redacted]')
    expect(html).not.toContain('hunter2secret')
  })

  it('内核条目没有 pid 时不画 pid，也不画空的来源标签', async () => {
    const entry = pageEntries(kernel)[0]!
    const html = await render(LogEntryRow, { entry })
    expect(html).toContain(entry.message)
    expect(html).not.toContain('pid ')
  })

  it('复制出去的整行带时间、来源与级别，不是裸消息', () => {
    const entry = entryWith(basic, 'marker-i=30')
    const text = entryPlainText(entry)
    expect(text).toContain(entry.timestamp)
    expect(text).toContain(entry.unit as string)
    expect(text).toContain(`[${entry.level}]`)
    expect(text).toContain(entry.message)
  })

  it('这一页的统计是真的：2 条被过滤、0 条截断、0 条无文本', () => {
    const summary = summarizeLogPage(basic)
    expect(summary.shown).toBe(basic.scanned)
    expect(summary.redacted).toBe(2)
    expect(summary.truncatedEntries).toBe(0)
    expect(summary.binary).toBe(0)
    expect(summary.truncated).toBe(basic.truncated)
  })
})

// ---------------------------------------------------------------- 实时流

interface RawFrame {
  id?: string
  event?: string
  data?: string
}

/**
 * 把 curl 抓下来的原始 SSE 字节切成帧。
 *
 * 以空行为界，`:` 开头的是注释（`: connected` / `: keepalive`），
 * 其余按 `field: value` 解析。这里刻意不共用浏览器那条解析路径：
 * 要验的正是“服务端写出来的字节能不能被按 SSE 规则读出来”。
 */
function parseFrames(raw: string): RawFrame[] {
  const frames: RawFrame[] = []
  let current: RawFrame = {}
  for (const line of raw.split('\n')) {
    const text = line.replace(/\r$/, '')
    if (text === '') {
      if (Object.keys(current).length > 0) frames.push(current)
      current = {}
      continue
    }
    if (text.startsWith(':')) continue
    const colon = text.indexOf(':')
    const field = colon === -1 ? text : text.slice(0, colon)
    const value = colon === -1 ? '' : text.slice(colon + 1).replace(/^ /, '')
    if (field === 'id') current.id = value
    else if (field === 'event') current.event = value
    else if (field === 'data') current.data = value
  }
  if (Object.keys(current).length > 0) frames.push(current)
  return frames
}

const liveFrames = parseFrames(readText('live.frames.txt'))
const resumeFrames = parseFrames(readText('resume.frames.txt'))
const idleFrames = parseFrames(readText('idle.frames.txt'))

function entriesOf(frames: RawFrame[]): LogEntry[] {
  const out: LogEntry[] = []
  for (const frame of frames) {
    if (frame.event !== 'entry') continue
    const parsed = parseLogStreamFrame('entry', frame.data)
    expect(parsed?.kind).toBe('entry')
    if (parsed?.kind === 'entry') out.push(parsed.entry)
  }
  return out
}

describe('真实 SSE 帧', () => {
  it('live：id 就是条目的游标，事件名是 entry，正文能解析成 LogEntry', () => {
    const entries = entriesOf(liveFrames)
    expect(entries.length).toBeGreaterThan(0)
    for (const frame of liveFrames) {
      if (frame.event !== 'entry') continue
      expect(frame.id).toBeDefined()
      const entry = parseLogStreamFrame('entry', frame.data)
      expect(entry?.kind).toBe('entry')
      if (entry?.kind === 'entry') expect(entry.entry.cursor).toBe(frame.id)
    }
    // 验收在流开着的时候写了 stream-marker-1：它必须真的到了。
    expect(entries.some((entry) => entry.message.includes('stream-marker-1'))).toBe(true)
  })

  it('resume：断点续传的第一条严格晚于 live 的最后一条', () => {
    const live = entriesOf(liveFrames)
    const resumed = entriesOf(resumeFrames)
    expect(live.length).toBeGreaterThan(0)
    expect(resumed.length).toBeGreaterThan(0)
    const liveLast = live[live.length - 1]!
    // 续传不重复投递已经收到过的条目。
    expect(resumed.map((entry) => entry.cursor)).not.toContain(liveLast.cursor)
    expect(at(resumed[0]!.timestamp)).toBeGreaterThan(at(liveLast.timestamp))
    // 续传是专门为第二段写的：它只应该看到第二次写入之后的行。
    expect(resumed.some((entry) => entry.message.includes('stream-marker-2'))).toBe(true)
  })

  it('idle：安静的系统上服务端主动结束，并说明原因是空闲超时', () => {
    const ends = idleFrames.filter((frame) => frame.event === 'end')
    expect(ends).toHaveLength(1)
    const parsed = parseLogStreamFrame('end', ends[0]!.data)
    expect(parsed).toEqual({ kind: 'end', reason: 'idle_timeout' })
    // 结束帧不是断线：这之前一条 entry 都没有（窗口里本来就没日志）。
    expect(entriesOf(idleFrames)).toHaveLength(0)
  })

  it('坏帧返回 null 而不是猜一个：event 名与内容对不上就不画', () => {
    expect(parseLogStreamFrame('entry', '{"reason":"idle_timeout"}')).toBeNull()
    expect(parseLogStreamFrame('end', '{"cursor":"x","timestamp":"t","level":"info","message":""}')).toBeNull()
    expect(parseLogStreamFrame('entry', 'not json')).toBeNull()
  })
})
