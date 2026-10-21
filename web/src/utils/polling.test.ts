// 轮询调度的测试。
//
// 要证明的只有四件事：跟着服务端间隔走、不并发、能停、重试能立刻生效。
// 用假定时器，不真等待。

import { afterEach, describe, expect, it, vi } from 'vitest'

import { createPoller } from './polling'

afterEach(() => {
  vi.useRealTimers()
})

describe('createPoller', () => {
  it('start 立刻执行一次，然后按服务端间隔继续', async () => {
    vi.useFakeTimers()
    const task = vi.fn(() => Promise.resolve())
    const poller = createPoller(task, { intervalMs: () => 5000 })

    poller.start()
    await vi.advanceTimersByTimeAsync(0)
    expect(task).toHaveBeenCalledTimes(1)

    await vi.advanceTimersByTimeAsync(5000)
    expect(task).toHaveBeenCalledTimes(2)

    await vi.advanceTimersByTimeAsync(10_000)
    expect(task).toHaveBeenCalledTimes(4)

    poller.stop()
  })

  it('间隔下限 2 秒、上限 60 秒，异常值不会把浏览器打满', async () => {
    vi.useFakeTimers()
    const task = vi.fn(() => Promise.resolve())
    const interval = vi.fn(() => 1)
    const poller = createPoller(task, { intervalMs: interval })

    poller.start()
    await vi.advanceTimersByTimeAsync(0)
    expect(task).toHaveBeenCalledTimes(1)

    // 服务端给了 1ms，但实际要等到 2000ms 才再问。
    await vi.advanceTimersByTimeAsync(1999)
    expect(task).toHaveBeenCalledTimes(1)
    await vi.advanceTimersByTimeAsync(1)
    expect(task).toHaveBeenCalledTimes(2)

    interval.mockReturnValue(Number.POSITIVE_INFINITY)
    await vi.advanceTimersByTimeAsync(1999)
    expect(task).toHaveBeenCalledTimes(2)
    await vi.advanceTimersByTimeAsync(1)
    expect(task).toHaveBeenCalledTimes(3)

    poller.stop()
  })

  it('上一次没回来时不并发，结束后补一次', async () => {
    vi.useFakeTimers()
    let release: (() => void) | undefined
    const task = vi.fn(
      () =>
        new Promise<void>((resolve) => {
          release = resolve
        }),
    )
    const poller = createPoller(task, { intervalMs: () => 2000 })

    poller.start()
    await vi.advanceTimersByTimeAsync(0)
    expect(task).toHaveBeenCalledTimes(1)

    // 请求还挂着，此时用户点重试：不能开第二条请求。
    poller.trigger()
    expect(task).toHaveBeenCalledTimes(1)

    release?.()
    await vi.advanceTimersByTimeAsync(0)
    expect(task).toHaveBeenCalledTimes(2)

    release?.()
    await vi.advanceTimersByTimeAsync(0)
    expect(task).toHaveBeenCalledTimes(2)

    poller.stop()
  })

  it('stop 之后既不再跑，也不会重复 start 叠加', async () => {
    vi.useFakeTimers()
    const task = vi.fn(() => Promise.resolve())
    const poller = createPoller(task, { intervalMs: () => 2000 })

    poller.start()
    poller.start()
    await vi.advanceTimersByTimeAsync(0)
    expect(task).toHaveBeenCalledTimes(1)
    expect(poller.running).toBe(true)

    poller.stop()
    expect(poller.running).toBe(false)
    await vi.advanceTimersByTimeAsync(10_000)
    expect(task).toHaveBeenCalledTimes(1)

    poller.start()
    await vi.advanceTimersByTimeAsync(0)
    expect(task).toHaveBeenCalledTimes(2)
    poller.stop()
  })

  it('任务抛错也继续轮询（失败要显示失败，但不必停摆）', async () => {
    vi.useFakeTimers()
    const onError = vi.fn()
    const task = vi.fn(() => Promise.reject(new Error('boom')))
    const poller = createPoller(task, { intervalMs: () => 2000, onError })

    poller.start()
    await vi.advanceTimersByTimeAsync(0)
    expect(task).toHaveBeenCalledTimes(1)
    expect(onError).toHaveBeenCalledTimes(1)

    await vi.advanceTimersByTimeAsync(2000)
    expect(task).toHaveBeenCalledTimes(2)
    expect(onError).toHaveBeenCalledTimes(2)

    poller.stop()
  })
})
