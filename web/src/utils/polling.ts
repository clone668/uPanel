// 轮询调度：页面按服务端给的 interval_ms 自己刷新。
//
// 为什么不用 setInterval：
//   - setInterval 会在上一次请求还没回来时继续发，慢请求会叠成雪崩；
//   - 采集节奏由服务端决定（CONTRACTS 里 interval_ms 是契约字段），页面只跟随；
//   - 页面不可见时应当停下来（后台标签页不该一直打服务），重新可见时立刻补一次。
//
// 这里是“何时再问一次”的唯一实现，三个视图共用，避免各写一套 setTimeout 链。

export interface PollerOptions {
  /** 下一次执行间隔（毫秒）。每次调度时重新读取，因此可以直接读服务端的 interval_ms。 */
  intervalMs: () => number
  /** 间隔下限：服务端给了异常小的值也不把浏览器打满。默认 2000。 */
  minIntervalMs?: number
  /** 间隔上限：默认 60000，慢一点也总比永远不再问好。 */
  maxIntervalMs?: number
  /** 页面不可见时暂停，重新可见时立刻补一次。默认 true。 */
  pauseWhenHidden?: boolean
  /** 任务抛错时的出口（默认打到 console.error，便于在控制台看见）。 */
  onError?: (error: unknown) => void
}

export interface Poller {
  /** 开始轮询：立刻执行一次，然后按间隔排下一次。重复调用不会叠加。 */
  start(): void
  /** 停止并清掉待执行的任务。 */
  stop(): void
  /** 立刻再执行一次（重试按钮、切换设备）。已在执行时排队，不并发。 */
  trigger(): void
  /** 是否正在轮询。 */
  readonly running: boolean
}

const DEFAULT_MIN_MS = 2_000
const DEFAULT_MAX_MS = 60_000

function defaultOnError(error: unknown): void {
  console.error('[poller] 轮询任务抛出未处理异常：', error)
}

export function createPoller(task: () => Promise<void>, options: PollerOptions): Poller {
  const minMs = options.minIntervalMs ?? DEFAULT_MIN_MS
  const maxMs = options.maxIntervalMs ?? DEFAULT_MAX_MS
  const pauseWhenHidden = options.pauseWhenHidden ?? true

  let timer: ReturnType<typeof setTimeout> | undefined
  let running = false
  let inFlight = false
  let again = false

  function clear(): void {
    if (timer !== undefined) {
      clearTimeout(timer)
      timer = undefined
    }
  }

  function delay(): number {
    const raw = options.intervalMs()
    if (!Number.isFinite(raw)) return minMs
    return Math.min(maxMs, Math.max(minMs, Math.round(raw)))
  }

  function hidden(): boolean {
    return pauseWhenHidden && typeof document !== 'undefined' && document.visibilityState === 'hidden'
  }

  function schedule(): void {
    clear()
    if (!running) return
    timer = setTimeout(() => {
      timer = undefined
      void run()
    }, delay())
  }

  async function run(): Promise<void> {
    if (!running) return
    if (hidden()) {
      // 不可见：不请求，但保留轮询状态，等重新可见时立刻补。
      return
    }
    if (inFlight) {
      // 上一次还没回来：记一笔，等它结束后立刻再来一次（不并发）。
      again = true
      return
    }
    inFlight = true
    try {
      await task()
    } catch (error) {
      // 任务自己负责把失败画到界面上；这里只保证轮询不会因为一次异常就停摆。
      ;(options.onError ?? defaultOnError)(error)
    } finally {
      inFlight = false
      if (again) {
        // 欠了一次立刻重跑：不排定时器，重跑结束后自己会排。
        again = false
        void run()
        return
      }
      schedule()
    }
  }

  function onVisibilityChange(): void {
    if (hidden()) {
      clear()
      return
    }
    if (running && timer === undefined && !inFlight) void run()
  }

  let listening = false

  function listen(): void {
    if (listening || typeof document === 'undefined') return
    document.addEventListener('visibilitychange', onVisibilityChange)
    listening = true
  }

  function unlisten(): void {
    if (!listening || typeof document === 'undefined') return
    document.removeEventListener('visibilitychange', onVisibilityChange)
    listening = false
  }

  return {
    start(): void {
      if (running) return
      running = true
      listen()
      void run()
    },
    stop(): void {
      running = false
      again = false
      clear()
      unlisten()
    },
    trigger(): void {
      if (!running) return
      clear()
      void run()
    },
    get running(): boolean {
      return running
    },
  }
}
