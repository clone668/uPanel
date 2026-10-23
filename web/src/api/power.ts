// 系统电源动作（P11.3）：system.power，只有两个操作——重启与关机。
//
// 与其他动作模块一样，这里**没有**请求函数：写操作在前端只有一条路
// （`createPreflight` + `submitJob`，见 api/preflights.ts）。本模块放的是：
//
//   1) 冻结的动作名、参数形状与延迟区间（CONTRACTS.md 第 5 节、PLAN.md §16）；
//   2) 界面提前说清的规矩：要再认证、要一次独立确认、有 30—300 秒倒计时；
//   3) 结果的读法：`power_check` 的四个状态各自该怎么说，以及怎么从事件流里取
//      倒计时剩余秒数。
//
// 一条不能破的线：**受理不是发生**。`systemctl reboot` 收下命令只证明命令交出去了，
// 机器什么时候重启、有没有重启，只有 boot_id 变了才算数。所以这里没有任何
// "关机成功""重启成功"的说法，只有"请求已受理"和"已确认 / 尚未发生 / 连不上 / 无法确认"。

import type { JobDTO, PowerCheckDTO, PowerCheckState } from './jobs'

/** 动作登记名（CONTRACTS.md 第 5 节）。 */
export const POWER_ACTION = 'system.power'

/** 两个操作（代理侧同一个闭集）。 */
export const POWER_OPERATIONS = ['reboot', 'poweroff'] as const

export type PowerOperation = (typeof POWER_OPERATIONS)[number]

/** 操作 → 界面上的词。 */
export const POWER_OPERATION_LABELS: Record<PowerOperation, string> = {
  reboot: '重启',
  poweroff: '关机',
}

/** 操作 → 提交按钮上的词（比“提交”重一档，与删分区的写法一致）。 */
export const POWER_OPERATION_VERBS: Record<PowerOperation, string> = {
  reboot: '确认重启',
  poweroff: '确认关机',
}

/** 倒计时区间（CONTRACTS.md 第 5 节：30—300 秒）。 */
export const POWER_DELAY_MIN = 30
export const POWER_DELAY_MAX = 300

/**
 * 界面上给的几档延迟。
 *
 * 没有“立即执行”这一档，也不接受 0：那正是倒计时存在的理由——命令发出去之后，
 * 人已经没有机会反悔了。30 秒是最短的一档，也是取消的最后窗口。
 */
export const POWER_DELAY_PRESETS: readonly number[] = [30, 60, 120, 300]

/** 默认延迟：最短那一档。留出取消的窗口，又不至于慢到人都忘了自己在等什么。 */
export const POWER_DELAY_DEFAULT = POWER_DELAY_MIN

/** 延迟是否在契约区间内。 */
export function isValidPowerDelay(seconds: number): boolean {
  return Number.isInteger(seconds) && seconds >= POWER_DELAY_MIN && seconds <= POWER_DELAY_MAX
}

/**
 * 拼出动作参数。
 *
 * 不合法就返回 null：调用方（对话框）据此不预检，而不是拿一份会被代理拒的参数
 * 去问“能不能”。延迟**必须显式给**——代理拒收没有 delay_seconds 的请求。
 */
export function powerParams(
  operation: PowerOperation,
  delaySeconds: number,
): Record<string, unknown> | null {
  if (!POWER_OPERATIONS.includes(operation)) return null
  if (!isValidPowerDelay(delaySeconds)) return null
  return { operation, delay_seconds: delaySeconds }
}

/** 电源动作一律要求 5 分钟内再认证（界面提前提示；真正的拦截在服务端）。 */
export function powerNeedsReauth(): boolean {
  return true
}

/**
 * 提交前摊开给人看的三句话。
 *
 * 每一句都对应一个真实现象：连接会断、要重新开机、受理不等于发生。
 */
export function powerConfirmLines(operation: PowerOperation, delaySeconds: number): string[] {
  const lines = [
    `面板会执行 systemctl ${operation}，倒计时 ${delaySeconds} 秒后真正下发。`,
    `倒计时期间可以取消：命令还没交出去，取消是有效的。`,
  ]
  if (operation === 'reboot') {
    lines.push('下发之后这台机器会失去连接，所有会话、正在跑的任务都会被断开；这是重启的本意。')
    lines.push('是否真的重启，由机器回来后 boot_id 是否变化来确认——面板不会替你猜。')
  } else {
    lines.push('下发之后这台机器会失去连接，而且**不会自己开机**：需要有人到现场或通过带外管理上电。')
    lines.push('面板看不到电源灯：关机请求是否生效、电源是否真的切断，最终只能由现场确认。')
  }
  return lines
}

/** 核对状态的四个值各自的中文说法。 */
export const POWER_CHECK_LABELS: Record<PowerCheckState, string> = {
  confirmed: '已确认',
  unchanged: '尚未发生',
  unreachable: '连不上代理',
  unknown: '无法确认',
}

/** 核对状态 → 标签色。unreachable 对关机是预期之内，所以是 info 不是 error。 */
export const POWER_CHECK_TYPES: Record<
  PowerCheckState,
  'default' | 'info' | 'success' | 'warning' | 'error'
> = {
  confirmed: 'success',
  unchanged: 'warning',
  unreachable: 'info',
  unknown: 'warning',
}

const CHECK_STATES: readonly PowerCheckState[] = ['confirmed', 'unchanged', 'unreachable', 'unknown']

/**
 * 从任务视图里取出 power_check。
 *
 * 结构不对就当没有：这一节是附加说明，宁可少显示一行，也不能把一份看不懂的东西
 * 渲染成“已确认”。任务状态本身不依赖它。
 */
export function readPowerCheck(job: Pick<JobDTO, 'power_check'> | null | undefined): PowerCheckDTO | null {
  const raw: unknown = job?.power_check
  if (raw === null || raw === undefined || typeof raw !== 'object') return null
  const record = raw as Record<string, unknown>
  const operation = record.operation
  const state = record.state
  if (operation !== 'reboot' && operation !== 'poweroff') return null
  if (typeof state !== 'string' || !CHECK_STATES.includes(state as PowerCheckState)) return null
  return {
    operation,
    state: state as PowerCheckState,
    changed: record.changed === true,
    boot_id_before: typeof record.boot_id_before === 'string' ? record.boot_id_before : '',
    boot_id_now: typeof record.boot_id_now === 'string' ? record.boot_id_now : '',
    checked_at: typeof record.checked_at === 'string' ? record.checked_at : '',
    message: typeof record.message === 'string' ? record.message : '',
  }
}

/**
 * 从一条进度事件里读出倒计时剩余秒数；不是倒计时事件就返回 null。
 *
 * 代理每 5 秒写一条 `step=countdown` 的事件，带 `remaining_seconds`。这是**唯一**
 * 权威的剩余时间：页面自己数秒会在切标签页、休眠、时钟漂移时撒谎。
 * 读不到就不显示数字，只显示“倒计时中”。
 */
export function readCountdownSeconds(payload: Record<string, unknown> | null | undefined): number | null {
  if (!payload) return null
  if (payload.step !== 'countdown') return null
  const remaining = payload.remaining_seconds
  if (typeof remaining !== 'number' || !Number.isFinite(remaining) || remaining < 0) return null
  return Math.floor(remaining)
}

/**
 * 一句给界面的“接下来会发生什么”，只在任务受理之后用。
 *
 * 不谈成功，只谈事实与时间：这是 accepted 结果唯一诚实的说法。
 */
export function acceptedNotice(operation: PowerOperation, delaySeconds: number | null): string {
  const delay = delaySeconds === null ? '' : `${delaySeconds} 秒倒计时结束后`
  if (operation === 'poweroff') {
    return `请求已受理：${delay}下发 systemctl poweroff。这不是“已关机”——面板只能确认命令交出去了，机器随后会失去连接，且不会自己开机。`
  }
  return `请求已受理：${delay}下发 systemctl reboot。这不是“已重启”——机器回来后 boot_id 变了才算数。`
}

/** 关机之后没有任何可用证据：这句话要能直接摆在屏幕上。 */
export const POWEROFF_UNCERTAIN_NOTICE =
  '关机之后代理会消失，面板读不到任何状态。连接断开符合预期，但**不构成关机成功的证据**：最终状态需要现场确认。'
