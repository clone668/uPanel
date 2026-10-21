// 用真实 Ubuntu 进程快照渲染进程页面组件（P05.2）。
//
// 这里的 JSON 不是造的：它们是验收 VM（upanel-2404，Ubuntu 24.04.5 / kernel 6.8.0-147）
// 上真实运行 upanel-web（production 模式、root 代理）后由
// tests/integration/p05-2-processes-acceptance.sh 抓回来的响应，原样存在
// src/fixtures/p05-2-vm/ 下（连 {data, request_id} / {error, request_id} 外壳都没拆）。
// 校验器 tests/integration/p05-2-payload-validate.py 对同一批文件做过 9000 多项结构校验。
//
// 为什么需要这一层：这一轮的真机验收在宿主上抓到的第一个问题不是产品缺陷，而是
// **测试自己**读错了时间——进程列表是 5 秒缓存快照，脚本在进程刚起来、快照还没刷新时
// 就断言，于是 8 条断言把“快照滞后”当成了“功能错了”。这些数字来自修好之后的第二轮，
// 所以这里的每一条断言都必须能对上真机抓到的行，而不是我编得像真的。

import { createHash } from 'node:crypto'
import { readFileSync } from 'node:fs'

import { describe, expect, it } from 'vitest'
import { createSSRApp, h, type Component } from 'vue'
import { createMemoryHistory, createRouter, type Router } from 'vue-router'
import { renderToString } from 'vue/server-renderer'

import { isTerminal, jobErrorMessage, type JobDTO } from '@/api/jobs'
import {
  canSubmitPreflight,
  requiresConfirmation,
  confirmationMatches,
  type PreflightData,
} from '@/api/preflights'
import {
  PROCESS_STATES,
  canSignal,
  filterProcesses,
  formatCPUPercent,
  isExited,
  sameProcess,
  summarizeProcesses,
  type ProcessEntry,
  type ProcessListData,
} from '@/api/processes'
import { apiPanelState, retryablePanelState, type PanelLikeState } from '@/utils/viewstate'
import { formatBytes } from '@/utils/format'
import type { ApiErrorCode } from '@/api/types'
import ProcessSignalConfirm from './ProcessSignalConfirm.vue'

function readFixture(name: string): Record<string, unknown> {
  return JSON.parse(
    readFileSync(new URL(`../fixtures/p05-2-vm/${name}`, import.meta.url), 'utf8'),
  ) as Record<string, unknown>
}

function fixture<T>(name: string): T {
  return readFixture(name)['data'] as T
}

function errorFixture(name: string): { code: string; message: string } {
  return readFixture(name)['error'] as { code: string; message: string }
}

/** 契约里的字段集合：多一个少一个都是 DTO 漂移。 */
const PROCESS_KEYS = [
  'capabilities',
  'command',
  'cpu_percent',
  'memory_bytes',
  'owner',
  'pid',
  'ppid',
  'protection',
  'start_time_ticks',
  'state',
  'unit',
]

/** 契约里的指纹：完整 64 位十六进制，短了就是漂移。 */
const SHA256 = /^sha256:[0-9a-f]{64}$/

/** 确认短语的字母表：去掉了容易看错的 I 与 O。 */
const PHRASE = /^KILL [A-HJ-NP-Z2-9]{6}$/

const first = fixture<ProcessListData>('processes-first.json')
const cpu = fixture<ProcessListData>('processes-cpu.json')
const zombie = fixture<ProcessListData>('processes-zombie.json')

const preflightTerm = fixture<PreflightData>('preflight-term-ok.json')
const preflightKill = fixture<PreflightData>('preflight-kill-ok.json')
const preflightInit = fixture<PreflightData>('preflight-protected-init.json')
const preflightSelf = fixture<PreflightData>('preflight-protected-self.json')
const preflightKernel = fixture<PreflightData>('preflight-protected-kernel.json')
const preflightSSH = fixture<PreflightData>('preflight-protected-ssh.json')
const preflightNotFound = fixture<PreflightData>('preflight-notfound.json')
const preflightMismatch = fixture<PreflightData>('preflight-identity-mismatch.json')
const preflightZombie = fixture<PreflightData>('preflight-zombie.json')
const preflightReusedOld = fixture<PreflightData>('preflight-reused-old.json')
const preflightReusedNew = fixture<PreflightData>('preflight-reused-new.json')

const jobTerm = fixture<JobDTO>('job-term-ok.json')
const jobKill = fixture<JobDTO>('job-kill-ok.json')

function testRouter(): Router {
  return createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: '/', component: { render: () => null } },
      { path: '/processes', component: { render: () => null } },
    ],
  })
}

/**
 * 渲染并去掉 scoped 样式加的 data-v-* 属性。
 *
 * 属性只在 DOM 上起作用，跟“页面上写了什么”无关；留着它会让断言变成
 * `<strong data-v-1ce82e51>`——测的是编译产物，不是文案。
 */
async function render(component: Component, props: Record<string, unknown> = {}): Promise<string> {
  const app = createSSRApp({ render: () => h(component, props) })
  app.use(testRouter())
  const html = await renderToString(app)
  return html.replace(/ data-v-[0-9a-z]+(?=[ >])/g, '')
}

/** 服务端的身份指纹公式，独立实现一遍：预检里的指纹必须能算回来。 */
function fingerprint(pid: number, start: number): string {
  return `sha256:${createHash('sha256').update(`process/v1|${pid}|${start}`).digest('hex')}`
}

function row(list: ProcessListData, pid: number): ProcessEntry {
  const hit = list.processes.find((process) => process.pid === pid)
  if (hit === undefined) throw new Error(`快照 ${list.collected_at} 里没有 PID ${pid}`)
  return hit
}

/**
 * 渲染 KILL 卡片需要一个“进程行”，但 KILL 预检的目标（PID 5276）在抓快照之前就退出了，
 * 所以三份快照里都没有它——这是真机事实，不是缺数据（`ps-first.txt` 里也没有它）。
 *
 * 这里只把身份换成预检里那对**真实**的 (pid, start_time_ticks)，其余字段沿用同一台机器上
 * 命令同为 `sleep 600`、属主同为 root 的那一行。卡片上会打印的字段里：pid / starttime
 * 来自预检本身，command 与 owner 来自预检的 display_name（“PID 5276（root：sleep 600）”），
 * unit 留空——预检没有给出单元名，就不把 PID 4811 的单元名安到 PID 5276 头上。
 */
function killTargetRow(): ProcessEntry {
  const params = preflightKill.normalized_params as {
    pid: number
    start_time_ticks: number
  }
  const sameShape = row(first, 4811)
  return {
    ...sameShape,
    pid: params.pid,
    start_time_ticks: params.start_time_ticks,
    unit: '',
  }
}

describe('真实 VM 快照：进程列表', () => {
  it('快照字段与契约一致：ok、5 秒、不截断、收齐 total 条', () => {
    for (const snapshot of [first, cpu, zombie]) {
      expect(snapshot.status).toBe('ok')
      expect(snapshot.interval_ms).toBe(5000)
      expect(snapshot.truncated).toBe(false)
      expect(snapshot.processes).toHaveLength(snapshot.total)
      // UTC、带 Z：本地时间会被读成“刚刚”，跨时区核对时会出错。
      expect(snapshot.collected_at).toMatch(/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$/)
    }
  })

  it('列表规模与真机相当：137 个进程，不是“只回来了前几行”', () => {
    // 钉住这个数：/proc 扫描一旦又在中途失败（读不到 stat / 权限不够），
    // 这里会立刻从 137 掉到个位数。
    expect(first.total).toBe(137)
    expect(first.processes).toHaveLength(137)
    expect(zombie.total).toBe(138)
    expect(cpu.total).toBe(139)
  })

  it('相邻两次采集正好相隔一个 interval（5 秒）', () => {
    const at = (value: string): number => new Date(value).getTime()
    expect(at(zombie.collected_at) - at(first.collected_at)).toBe(5000)
    // 第三份中间跳过了一轮（harness 在等进程出现），但间隔仍是 interval 的整数倍。
    expect((at(cpu.collected_at) - at(zombie.collected_at)) % 5000).toBe(0)
  })

  it('字段集合、PID 升序、身份唯一：三份快照逐行都合规', () => {
    for (const snapshot of [first, cpu, zombie]) {
      let previous = 0
      const seen = new Set<string>()
      for (const process of snapshot.processes) {
        expect(Object.keys(process).sort()).toEqual(PROCESS_KEYS)
        expect(PROCESS_STATES).toContain(process.state)
        expect(process.pid).toBeGreaterThan(previous)
        previous = process.pid
        // 身份是 pid + starttime：同一份快照里不能出现第二个同身份的进程。
        const key = `${process.pid}:${process.start_time_ticks}`
        expect(seen.has(key)).toBe(false)
        seen.add(key)
        expect(Number.isInteger(process.start_time_ticks)).toBe(true)
        expect(process.start_time_ticks).toBeGreaterThanOrEqual(0)
        // 内存是十进制字符串或 null，不是数字、不是 0。
        if (process.memory_bytes !== null) expect(process.memory_bytes).toMatch(/^\d+$/)
        // 能力来自服务端，只能是闭集里的值，且按 TERM → KILL 排序。
        for (const capability of process.capabilities) {
          expect(['TERM', 'KILL']).toContain(capability)
        }
        expect(process.capabilities).toEqual(
          (['TERM', 'KILL'] as const).filter((signal) => process.capabilities.includes(signal)),
        )
        // 受保护的进程必须给出类别与理由；不受保护的不能捏造理由。
        if (process.protection.protected) {
          expect(typeof process.protection.category).toBe('string')
          expect(process.protection.reasons.length).toBeGreaterThan(0)
        } else {
          expect(process.protection.category).toBeUndefined()
          expect(process.protection.reasons).toEqual([])
        }
      }
    }
  })

  it('状态分布是真的读写过的 /proc：sleeping 与 idle 都在，不是清一色 running', () => {
    const histogram: Record<string, number> = {}
    for (const process of first.processes) {
      histogram[process.state] = (histogram[process.state] ?? 0) + 1
    }
    // 真机数字：66 个可中断睡眠 + 71 个空闲（内核线程与已就绪的守护进程）。
    expect(histogram['sleeping']).toBe(66)
    expect(histogram['idle']).toBe(71)
    expect(histogram['running']).toBeUndefined()
    // 高 CPU 的那一轮里才有 running：`yes` 把单核打满。
    expect(cpu.processes.find((process) => process.pid === 5041)?.state).toBe('running')
  })

  it('PID 1、内核线程、sshd、面板与代理自身：受保护、没有任何能力', () => {
    const init = row(first, 1)
    expect(init.start_time_ticks).toBe(94)
    expect(init.command).toBe('init')
    expect(init.unit).toBe('init.scope')
    expect(init.protection.protected).toBe(true)
    expect(init.protection.category).toBe('init')
    expect(init.capabilities).toEqual([])
    // 受保护的进程不是“不显示”，而是照常出现在列表里、只是没有按钮。
    expect(canSignal(init, 'TERM')).toBe(false)
    expect(canSignal(init, 'KILL')).toBe(false)

    const kthreadd = row(first, 2)
    expect(kthreadd.command).toBe('[kthreadd]')
    expect(kthreadd.protection.category).toBe('kernel_thread')
    // 内核线程没有用户空间内存：null，不是 0。
    expect(kthreadd.memory_bytes).toBeNull()
    expect(formatBytes(kthreadd.memory_bytes)).toBe('—')

    const sshd = row(first, 994)
    expect(sshd.protection.category).toBe('ssh')
    expect(sshd.unit).toBe('ssh.service')
    expect(sshd.capabilities).toEqual([])

    const agent = row(first, 4746)
    const web = row(first, 4775)
    expect(agent.protection.category).toBe('self')
    expect(web.protection.category).toBe('self')
    expect(agent.capabilities).toEqual([])
    expect(web.capabilities).toEqual([])
    // 代理自身有两条理由（代理本身 + 它是执行操作的那个进程），面板自身只有一条。
    expect(agent.protection.reasons).toHaveLength(2)
    expect(web.protection.reasons).toHaveLength(1)
    expect(web.owner).toBe('upanel-web')
  })

  it('普通进程按状态给能力：活着的 TERM+KILL，僵尸一个都不给', () => {
    const sleep = row(first, 4811)
    expect(sleep.command).toBe('sleep 600')
    expect(sleep.protection.protected).toBe(false)
    expect(sleep.capabilities).toEqual(['TERM', 'KILL'])
    expect(canSignal(sleep, 'TERM')).toBe(true)
    expect(canSignal(sleep, 'KILL')).toBe(true)

    const dead = row(zombie, 4936)
    expect(dead.state).toBe('zombie')
    expect(isExited(dead)).toBe(true)
    // 僵尸不受保护，但也不能发信号：杀它不会让它更快被回收（CONTRACTS 10.2）。
    expect(dead.protection.protected).toBe(false)
    expect(dead.capabilities).toEqual([])
    expect(dead.memory_bytes).toBeNull()
    expect(dead.command).toBe('[python3]')
  })

  it('cpu_percent 的 null 表示“这一轮没有基线”，不是 0', () => {
    // 4775（upanel-web）与 4811（sleep 600）在第一次采集时刚起来，上一轮采样里还没有它们。
    expect(row(first, 4775).cpu_percent).toBeNull()
    expect(row(first, 4811).cpu_percent).toBeNull()
    expect(formatCPUPercent(null)).toBe('—')
    // 下一轮就有了真实值：基线建立之后不该再是 null。
    expect(row(cpu, 4811).cpu_percent).toBe(0)
    expect(row(cpu, 4775).cpu_percent).toBe(0.4)
    expect(formatCPUPercent(0)).toBe('0.0%')
    // `yes` 单核打满：100%，不是 1000% 也不是 1%。
    expect(row(cpu, 5041).cpu_percent).toBe(100)
    expect(formatCPUPercent(row(cpu, 5041).cpu_percent)).toBe('100.0%')
    expect(formatBytes(row(cpu, 5041).memory_bytes)).toBe('1.79 MiB')
  })

  it('进程出现与消失都会反映在快照里，不会被缓存成一行死数据', () => {
    // `yes`（5041）只在中间那份快照里存在；4936 的僵尸同样只在中后两份里。
    expect(first.processes.some((process) => process.pid === 5041)).toBe(false)
    expect(zombie.processes.some((process) => process.pid === 5041)).toBe(false)
    expect(first.processes.some((process) => process.pid === 4936)).toBe(false)
    expect(zombie.processes.some((process) => process.pid === 4936)).toBe(true)
    // 长时间存在的进程在三份快照里身份不变（pid 与 starttime 都不变）。
    const sleepIds = [first, zombie, cpu].map((snapshot) => {
      const hit = row(snapshot, 4811)
      return `${hit.pid}:${hit.start_time_ticks}`
    })
    expect(new Set(sleepIds).size).toBe(1)
    expect(sleepIds[0]).toBe('4811:27921')
  })

  it('摘要按真实数据统计，不是页面自己数', () => {
    expect(summarizeProcesses(first.processes)).toEqual({
      total: 137,
      running: 0,
      sleeping: 137,
      exited: 0,
      protected: 111,
      killable: 26,
    })
    // 有了 running 的 `yes` 与僵尸 4936 之后：运行中 1、已退出 1、可发信号 27。
    expect(summarizeProcesses(cpu.processes)).toEqual({
      total: 139,
      running: 1,
      sleeping: 137,
      exited: 1,
      protected: 111,
      killable: 27,
    })
    expect(summarizeProcesses(zombie.processes)).toEqual({
      total: 138,
      running: 1,
      sleeping: 136,
      exited: 1,
      protected: 111,
      killable: 26,
    })
    // 受保护 111 个：PID 1、全部内核线程、面板/代理自身、以及受保护单元里的进程。
    expect(first.processes.filter((process) => process.protection.protected)).toHaveLength(111)
  })

  it('筛选是客户端过滤：命中 pid、属主、命令摘要与单元名', () => {
    expect(filterProcesses(first.processes, '')).toHaveLength(137)
    expect(filterProcesses(first.processes, '4811').map((process) => process.pid)).toEqual([4811])
    // 前导数字命中（按 pid 前缀）：4 开头的进程都在 session-65.scope 里。
    expect(filterProcesses(first.processes, '4746').map((process) => process.pid)).toEqual([4746])
    const ssh = filterProcesses(first.processes, 'ssh')
    expect(ssh).toHaveLength(3)
    expect(ssh.some((process) => process.pid === 994)).toBe(true)
    expect(filterProcesses(first.processes, 'UPANEL').length).toBe(11)
    expect(filterProcesses(first.processes, 'zzz-not-here')).toHaveLength(0)
    // 过滤不改变顺序，也不改变对象。
    expect(filterProcesses(first.processes, 'ssh').map((process) => process.pid)).toEqual(
      [...ssh.map((process) => process.pid)].sort((a, b) => a - b),
    )
  })
})

describe('真实 VM 预检：身份、保护、TERM 与 KILL 分开', () => {
  it('TERM 预检：允许、不再认证、没有短语、锁到 pid+starttime', () => {
    expect(preflightTerm.action).toBe('process.signal')
    expect(preflightTerm.normalized_params).toEqual({
      pid: 4811,
      signal: 'TERM',
      start_time_ticks: 27921,
    })
    expect(preflightTerm.allowed).toBe(true)
    expect(preflightTerm.blockers).toEqual([])
    expect(preflightTerm.requires_reauth).toBe(false)
    expect(preflightTerm.confirmation_phrase).toBeNull()
    // 进程终止不可取消：任务一旦交给代理就要跑完，只报告结果。
    expect(preflightTerm.cancellable).toBe(false)
    expect(preflightTerm.locks).toEqual(['process:4811:27921'])
    expect(preflightTerm.target.display_name).toBe('PID 4811（root：sleep 600）')
    // 指纹就是 pid + starttime 的 sha256，少一位都会算不出来。
    expect(preflightTerm.target.identity_fingerprint).toMatch(SHA256)
    expect(preflightTerm.target.identity_fingerprint).toBe(fingerprint(4811, 27921))
    expect(preflightTerm.warnings.map((warning) => warning.code)).toEqual(['process_terminate'])
    expect(preflightTerm.impacts.map((impact) => impact.kind)).toEqual([
      'process_termination',
      'none',
    ])
    // TERM 的第二个影响不是数据丢失，而是“要杀就另外发起 KILL”。
    expect(preflightTerm.impacts[1]?.description).toContain('强制结束')
    expect(requiresConfirmation(preflightTerm)).toBe(false)
    expect(confirmationMatches(preflightTerm, '')).toBe(true)
  })

  it('KILL 预检：要求再认证与确认短语，影响里必须有数据丢失', () => {
    expect(preflightKill.normalized_params).toEqual({
      pid: 5276,
      signal: 'KILL',
      start_time_ticks: 29891,
    })
    expect(preflightKill.allowed).toBe(true)
    expect(preflightKill.requires_reauth).toBe(true)
    expect(preflightKill.confirmation_phrase).toMatch(PHRASE)
    // 真机的短语：一次性、只有哈希落库，这里抓到的是明文的那一次。
    expect(preflightKill.confirmation_phrase).toBe('KILL AW375F')
    expect(preflightKill.target.identity_fingerprint).toBe(fingerprint(5276, 29891))
    expect(preflightKill.locks).toEqual(['process:5276:29891'])
    expect(preflightKill.cancellable).toBe(false)
    expect(preflightKill.warnings.map((warning) => warning.code)).toEqual(['process_force_kill'])
    expect(preflightKill.impacts.map((impact) => impact.kind)).toEqual([
      'process_termination',
      'process_data_loss',
    ])
    expect(requiresConfirmation(preflightKill)).toBe(true)
    // 短语区分大小写，敲错一个字符就是不过。
    expect(confirmationMatches(preflightKill, 'KILL AW375F')).toBe(true)
    expect(confirmationMatches(preflightKill, 'kill aw375f')).toBe(false)
  })

  it('被拒的预检也照常带上策略：拦它的是 409 DEPENDENCY_BLOCKED，不是“没有短语”', () => {
    // 这是一条容易做反的地方：KILL 的正文与短语由“信号”决定，
    // 与 allowed 无关。卡片因此既能显示“要你输短语”，也能显示“这次不允许”。
    for (const preflight of [preflightSelf, preflightSSH]) {
      expect(preflight.allowed).toBe(false)
      expect(preflight.requires_reauth).toBe(true)
      expect(preflight.confirmation_phrase).toMatch(PHRASE)
      expect(preflight.blockers.map((blocker) => blocker.code)).toEqual(['PROCESS_PROTECTED'])
    }
    // 反过来，被拒的 TERM 不凭空要求再认证。
    for (const preflight of [preflightInit, preflightKernel]) {
      expect(preflight.allowed).toBe(false)
      expect(preflight.requires_reauth).toBe(false)
      expect(preflight.confirmation_phrase).toBeNull()
    }
  })

  it('四种受保护进程各给各的理由，不是一句模板文案', () => {
    const reasons = [preflightInit, preflightKernel, preflightSelf, preflightSSH].map(
      (preflight) => preflight.blockers[0]?.message ?? '',
    )
    for (const reason of reasons) expect(reason.length).toBeGreaterThan(20)
    // 两两不相同：理由来自命中的那条规则，不是通用占位语。
    expect(new Set(reasons).size).toBe(4)
    expect(reasons[0]).toContain('PID 1')
    expect(reasons[0]).toContain('systemd')
    expect(reasons[1]).toContain('kthreadd')
    expect(reasons[2]).toContain('代理')
    expect(reasons[3]).toContain('ssh.service')
    // 受保护的预检不给任何能力，也不产生“可期待的变化”。
    for (const preflight of [preflightInit, preflightKernel, preflightSelf, preflightSSH]) {
      expect(preflight.warnings).toEqual([])
      expect(preflight.impacts.map((impact) => impact.kind)).toEqual(['none'])
    }
  })

  it('不存在、身份不符、已退出：三种拒绝各自有码，且都不发信号', () => {
    expect(preflightNotFound.blockers.map((blocker) => blocker.code)).toEqual([
      'PROCESS_NOT_FOUND',
    ])
    expect(preflightMismatch.blockers.map((blocker) => blocker.code)).toEqual([
      'PROCESS_IDENTITY_MISMATCH',
    ])
    expect(preflightZombie.blockers.map((blocker) => blocker.code)).toEqual([
      'PROCESS_ALREADY_EXITED',
    ])
    // 身份不符时的指纹描述的是**现在就占着这个 pid 的那个进程**（starttime 94），
    // 而锁仍然盯住预检当时要的那个身份（starttime 95）——所以提交时才会被拒。
    expect(preflightMismatch.normalized_params).toEqual({
      pid: 1,
      signal: 'TERM',
      start_time_ticks: 95,
    })
    expect(preflightMismatch.locks).toEqual(['process:1:95'])
    expect(preflightMismatch.target.identity_fingerprint).toBe(fingerprint(1, 94))
    expect(preflightMismatch.target.identity_fingerprint).not.toBe(fingerprint(1, 95))
    expect(preflightMismatch.blockers[0]?.message).toContain('starttime=94')
    for (const preflight of [preflightNotFound, preflightMismatch, preflightZombie]) {
      expect(preflight.allowed).toBe(false)
      expect(preflight.warnings).toEqual([])
      expect(preflight.impacts.map((impact) => impact.kind)).toEqual(['none'])
    }
  })

  it('PID 被重用：同一个 pid 换了 starttime 就是另一个进程', () => {
    // 这是本子任务的核心验收点，也是内核真的做到的事：
    // 脚本把 /proc/sys/kernel/ns_last_pid 往回写，同一个 5463 被分配给了新进程。
    const oldParams = preflightReusedOld.normalized_params as { start_time_ticks: number }
    const newParams = preflightReusedNew.normalized_params as { start_time_ticks: number }
    expect(preflightReusedOld.locks).toEqual(['process:5463:30039'])
    expect(preflightReusedNew.locks).toEqual(['process:5463:30049'])
    expect(oldParams.start_time_ticks).toBe(30039)
    expect(newParams.start_time_ticks).toBe(30049)
    // 指纹跟着 starttime 走：两份预检指向两个不同的进程。
    expect(preflightReusedOld.target.identity_fingerprint).toBe(fingerprint(5463, 30039))
    expect(preflightReusedNew.target.identity_fingerprint).toBe(fingerprint(5463, 30049))
    expect(preflightReusedOld.target.identity_fingerprint).not.toBe(
      preflightReusedNew.target.identity_fingerprint,
    )
    // 两份预检本身都是“允许”的：拒绝发生在提交时（代理重新读 /proc 核对身份）。
    expect(preflightReusedOld.allowed).toBe(true)
    expect(preflightReusedNew.allowed).toBe(true)
    expect(
      sameProcess(
        { pid: 5463, start_time_ticks: 30039 },
        { pid: 5463, start_time_ticks: 30049 },
      ),
    ).toBe(false)
  })
})

describe('真实 VM 任务：结果只看读回的进程状态', () => {
  it('TERM 任务成功：信号送达，读回时这个 PID 上已经没有进程', () => {
    expect(jobTerm.job_id).toBe('job_414db6c12fb0a64dc2c0a452714fa249')
    expect(jobTerm.action).toBe('process.signal')
    expect(jobTerm.status).toBe('succeeded')
    expect(isTerminal(jobTerm.status)).toBe(true)
    expect(jobTerm.error).toEqual({})
    expect(jobErrorMessage(jobTerm)).toBeNull()
    // 没有进度百分比的步骤型任务：null，不是 0。
    expect(jobTerm.progress_percent).toBeNull()
    expect(jobTerm.cancellable).toBe(false)

    const delivered = jobTerm.result['data'] as Record<string, unknown>
    expect(delivered['delivered']).toBe(true)
    expect(delivered['pid']).toBe(4811)
    expect(delivered['signal']).toBe('TERM')
    expect(delivered['start_time_ticks']).toBe(27921)
    // 7 ms：kill(2) 只是投递，不等待退出；退出是靠读回 /proc 确认的。
    expect(delivered['duration_ms']).toBe(7)

    expect(jobTerm.result['outcome']).toBe('verified')
    const observed = jobTerm.result['observed_state'] as Record<string, unknown>
    expect(observed['gone']).toBe(true)
    expect(observed['state_before']).toBe('sleeping')
    expect(observed['state_after']).toBe('gone')
    expect(observed['start_time_ticks']).toBe(27921)
    expect(typeof jobTerm.result['verified_at']).toBe('string')
  })

  it('KILL 任务与 TERM 是两条独立的任务，头部字段一模一样、目标不同', () => {
    expect(jobKill.job_id).toBe('job_39020aaf4842a54a9e1531c73cb8b796')
    expect(jobKill.job_id).not.toBe(jobTerm.job_id)
    const delivered = jobKill.result['data'] as Record<string, unknown>
    expect(delivered['pid']).toBe(5276)
    expect(delivered['signal']).toBe('KILL')
    expect(delivered['start_time_ticks']).toBe(29891)
    expect(jobKill.result['outcome']).toBe('verified')
    const observed = jobKill.result['observed_state'] as Record<string, unknown>
    expect(observed['gone']).toBe(true)
    expect(observed['signal']).toBe('KILL')
    // 两条任务的终态都是“读回确认”，不是“命令退出码 0”。
    expect(jobTerm.result['outcome']).toBe(jobKill.result['outcome'])
    expect(jobTerm.status).toBe(jobKill.status)
  })
})

describe('真实 VM 错误：页面状态由错误码决定', () => {
  const cases: Array<[string, ApiErrorCode, PanelLikeState, boolean]> = [
    ['processes-viewer-forbidden.json', 'FORBIDDEN', 'forbidden', false],
    ['preflight-viewer.json', 'FORBIDDEN', 'forbidden', false],
    ['job-submit-kill-no-reauth.json', 'REAUTH_REQUIRED', 'forbidden', false],
    ['job-submit-kill-replay.json', 'PREFLIGHT_STALE', 'error', true],
    ['job-submit-reused-old.json', 'DEVICE_IDENTITY_CHANGED', 'error', true],
  ]

  it.each(cases)('%s → %s → %s', (name, code, state, retryable) => {
    const error = errorFixture(name)
    expect(error.code).toBe(code)
    expect(error.message.length).toBeGreaterThan(4)
    expect(readFixture(name)['request_id']).toMatch(/^[0-9a-f]{32}$/)
    // 判据只用错误码：改文案不该改变页面行为。
    expect(apiPanelState({ code })).toBe(state)
    expect(retryablePanelState(state)).toBe(retryable)
  })

  it('viewer 两次 403 都是权限问题，不是“请求失败”', () => {
    // 进程列表是 admin 接口（CONTRACTS 10.2）：viewer 读到的是 403，
    // 页面必须说“没有权限”，而不是给一个重试按钮。
    expect(errorFixture('processes-viewer-forbidden.json').message).toBe('需要管理员权限')
    expect(errorFixture('preflight-viewer.json').message).toBe('需要管理员权限')
  })

  it('旧预检重放与 PID 重用是两种不同的拒绝，文案指向不同的下一步', () => {
    const replay = errorFixture('job-submit-kill-replay.json')
    const reused = errorFixture('job-submit-reused-old.json')
    expect(replay.code).not.toBe(reused.code)
    // 重放：这份预检已经用过了——重新预检。
    expect(replay.message).toContain('只能提交一次')
    // 重用：目标身份变了——提示里带着人可读的目标描述，让人知道是哪个进程变了。
    expect(reused.message).toContain('PID 5463')
    expect(reused.message).toContain('重新预检')
  })
})

describe('真实 VM 预检：确认卡渲染', () => {
  it('TERM 卡片：显示 pid + starttime、没有短语输入、有确认按钮', async () => {
    const html = await render(ProcessSignalConfirm, {
      process: row(first, 4811),
      signal: 'TERM',
      preflight: preflightTerm,
      busy: false,
      expired: false,
      remainingSeconds: 118,
    })
    expect(html).toContain('sleep 600')
    expect(html).toContain('4811')
    // starttime 必须显式打出来：只显示 pid 就会让人以为“杀的是这个号”。
    expect(html).toContain('starttime')
    expect(html).toContain('27921')
    expect(html).toContain('请求退出')
    expect(html).toContain('process:4811:27921')
    // TERM 不要求短语：既没有输入框，也没有再认证提示。
    expect(html).not.toContain('请输入确认短语')
    expect(html).not.toContain('这次操作要求再认证')
    // 自动升级在卡片上被明确否认——这是验收条款里的“TERM 与 KILL 分开”。
    expect(html).toContain('不会自动升级')
    expect(html).toContain('确认请求退出')
    expect(html).toContain('data-signal="TERM"')
    expect(html).toContain('data-allowed="true"')
    // 倒计时是父组件算好传进来的。
    expect(html).toContain('还剩 118 秒')
  })

  it('KILL 卡片：短语在卡片上、没输之前没有确认按钮', async () => {
    const html = await render(ProcessSignalConfirm, {
      process: killTargetRow(),
      signal: 'KILL',
      preflight: preflightKill,
      busy: false,
      expired: false,
      remainingSeconds: 60,
    })
    expect(html).toContain('5276')
    expect(html).toContain('29891')
    expect(html).toContain('强制结束')
    // 短语明文显示（服务端只在库里留哈希），并且要人照着敲。
    expect(html).toContain('KILL AW375F')
    expect(html).toContain('请输入确认短语')
    expect(html).toContain('这次操作要求再认证')
    expect(html).toContain('这是独立于')
    // 还没输入 → 没有“确认强制结束”按钮；这个按钮只能由输入正确的短语换出来。
    expect(html).not.toContain('确认强制结束')
    expect(html).toContain('data-signal="KILL"')
    expect(html).toContain('data-allowed="true"')
  })

  it('受保护的 KILL：说清为什么不允许，同时照样要求短语', async () => {
    const html = await render(ProcessSignalConfirm, {
      process: row(first, 4746),
      signal: 'KILL',
      preflight: preflightSelf,
      busy: false,
      expired: false,
      remainingSeconds: 60,
    })
    expect(html).toContain('预检不允许执行这个操作')
    expect(html).toContain('PROCESS_PROTECTED')
    expect(html).toContain('代理')
    expect(html).toContain('KILL ') // 策略仍然带短语
    expect(html).toContain('这次操作要求再认证')
    expect(html).not.toContain('确认强制结束')
    expect(html).toContain('data-allowed="false"')
    // 没有告警、影响是 none：卡片不该凭空渲染出“告警”段落。
    expect(html).not.toContain('process_force_kill')
  })

  it('受保护的 TERM：一个按钮都不给，也不要求短语', async () => {
    const html = await render(ProcessSignalConfirm, {
      process: row(first, 1),
      signal: 'TERM',
      preflight: preflightInit,
      busy: false,
      expired: false,
      remainingSeconds: 60,
    })
    expect(html).toContain('PID 1')
    expect(html).toContain('PROCESS_PROTECTED')
    expect(html).toContain('systemd')
    expect(html).not.toContain('确认请求退出')
    expect(html).not.toContain('请输入确认短语')
    // 取消按钮永远都在——不允许的操作也要能退出去。
    expect(html).toContain('取消')
  })

  it('过期之后提示重新预检，且不给提交按钮', async () => {
    const html = await render(ProcessSignalConfirm, {
      process: row(first, 4811),
      signal: 'TERM',
      preflight: preflightTerm,
      busy: false,
      expired: true,
      remainingSeconds: 0,
    })
    expect(html).toContain('预检已过期')
    expect(html).not.toContain('确认请求退出')
    expect(canSubmitPreflight(preflightTerm, new Date(preflightTerm.expires_at))).toBe(false)
    expect(canSubmitPreflight(preflightTerm, new Date(new Date(preflightTerm.expires_at).getTime() - 1000))).toBe(true)
  })
})
