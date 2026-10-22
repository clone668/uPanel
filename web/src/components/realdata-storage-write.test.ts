// 用真实 Ubuntu 数据回放“写动作之后”的设备详情（P07.2）。
//
// 这份 JSON 不是造的：验收 VM（upanel-2404）上真实跑完
//   初始化 GPT → 建两个分区（第二个名字写 data）
// 之后，由 tests/integration/p07-2-storage-write-acceptance.sh 抓回来的
// GET /api/v1/storage/devices/<那块盘>，原样存在 src/fixtures/p07-2-vm/ 下。
//
// 为什么要这一层：P07.2 给 partition_table 加了 disk_guid 与 disk_entries，这两个
// 字段的全部意义是“盘上的字节”和“内核的视图”是**两份独立证据**。手写的夹具里两者
// 天然一致，测不出任何东西；只有真实载荷才值得钉住——它来自两次不同的读法。
//
// 这里钉的是不变量，不是某一次运行的数值：
//   1. 条目表的字节数是十进制字符串、起点 1 MiB 对齐（模板与对齐由产品保证）；
//   2. 条目表与内核分区列表**按槽位号一一对应**（两源对不上就是验证失败）；
//   3. 写进去的名字能从盘上读回来，类型 GUID 是 Linux 数据分区那个；
//   4. 详情页拿这份载荷能渲染，且不会假装有可执行动作。

import { readFileSync } from 'node:fs'

import { describe, expect, it, vi } from 'vitest'
import { createSSRApp, h, type Component, type VNodeChild } from 'vue'
import { createMemoryHistory, createRouter, type Router } from 'vue-router'
import { renderToString } from 'vue/server-renderer'

import type {
  StorageDeviceDetail,
  StorageGPTEntry,
  StoragePartition,
} from '@/api/storage'
import DeviceDetailPanel from './DeviceDetailPanel.vue'

// naive-ui 的按钮/输入在 SSR 里会去挂样式（需要 document）。换成最小实现：
// 文字与 disabled 原样带出来，断言仍然断的是“这份真实载荷被画成了什么”。
vi.mock('naive-ui', async () => {
  const { h: hh } = await import('vue')
  const stub = (name: string) => ({
    name: `Stub${name}`,
    props: ['title', 'description', 'status', 'disabled', 'value', 'options', 'checked', 'type', 'size', 'placeholder', 'maxlength', 'min', 'max', 'step', 'bordered', 'secondary', 'loading'],
    setup(
      props: { title?: unknown; description?: unknown; disabled?: unknown },
      ctx: { slots: { default?: () => unknown; header?: () => unknown; 'header-extra'?: () => unknown } },
    ) {
      return () => {
        const children: VNodeChild[] = []
        if (typeof props.title === 'string') children.push(props.title)
        if (typeof props.description === 'string') children.push(props.description)
        const header = ctx.slots.header?.()
        if (header !== undefined) children.push(header as VNodeChild)
        const slot = ctx.slots.default?.()
        if (slot !== undefined) children.push(slot as VNodeChild)
        const extra = ctx.slots['header-extra']?.()
        if (extra !== undefined) children.push(extra as VNodeChild)
        const attrs: Record<string, unknown> = { 'data-naive': name }
        if (props.disabled === true) attrs['data-disabled'] = 'true'
        return hh('div', attrs, children)
      }
    },
  })
  return {
    NAlert: stub('alert'),
    NButton: stub('button'),
    NCard: stub('card'),
    NCheckbox: stub('checkbox'),
    NInput: stub('input'),
    NInputNumber: stub('input-number'),
    NProgress: stub('progress'),
    NSelect: stub('select'),
    NTag: stub('tag'),
  }
})

function fixture<T>(name: string): T {
  const raw = readFileSync(new URL(`../fixtures/p07-2-vm/${name}`, import.meta.url), 'utf8')
  // 服务端统一包一层 {data, request_id}，前端 client 也只读 data。
  const body = JSON.parse(raw) as { data: T }
  return body.data
}

const detail = fixture<StorageDeviceDetail>('device-two-partitions.json')
const device = detail.device
const table = device.partition_table
const entries: StorageGPTEntry[] = table?.disk_entries ?? []
// 内核那份视图在 partition_table.partitions 里（不是 device.partitions）。
const kernelParts: StoragePartition[] = table?.partitions ?? []

// GPT 的规范形态是大写、带连字符；小写或没有连字符说明产品没做规范化。
const GUID = /^[0-9A-F]{8}-[0-9A-F]{4}-[0-9A-F]{4}-[0-9A-F]{4}-[0-9A-F]{12}$/
// 字节数一律十进制字符串（CONTRACTS §6），不是数字、不是 "64MiB"。
const DECIMAL = /^[0-9]+$/
// Linux 数据分区类型的 GUID（sfdisk 的 type=linux）。
const LINUX_DATA = '0FC63DAF-8483-4772-8E79-3D69D8477DE4'
// 起点对齐：1 MiB。分区模板固定成这样，不是“差不多”。
const ALIGN = 1048576

function testRouter(): Router {
  return createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: '/', component: { render: () => null } },
      { path: '/storage/devices/:id', component: { render: () => null } },
    ],
  })
}

async function render(component: Component, props: Record<string, unknown> = {}): Promise<string> {
  const app = createSSRApp({ render: () => h(component, props) })
  app.use(testRouter())
  return renderToString(app)
}

describe('真实 VM 数据：写动作之后的设备详情', () => {
  it('盘上的表是 GPT，并且给了盘 GUID', () => {
    expect(table?.type).toBe('gpt')
    expect(table?.source).toBe('device')
    expect(table?.disk_guid).toMatch(GUID)
  })

  it('条目表与内核分区列表按槽位号一一对应', () => {
    // 长度先对上：盘上有几条，内核就应该看见几个。
    expect(entries.length).toBe(kernelParts.length)
    expect(entries.length).toBeGreaterThan(0)
    const byNumber = new Map(kernelParts.map((part) => [part.number, part]))
    for (const entry of entries) {
      const part = byNumber.get(entry.number)
      expect(part, `内核里没有槽位 ${entry.number}`).toBeDefined()
      // 两源读法不同（一边读盘上的条目表，一边读 sysfs），字节数必须一致。
      expect(entry.start_bytes).toBe(part?.start_bytes)
      expect(entry.size_bytes).toBe(part?.size_bytes)
      expect(part?.device_id).toBe(`${device.id}${entry.number}`)
    }
  })

  it('条目的字节数是十进制字符串，起点 1 MiB 对齐', () => {
    for (const entry of entries) {
      expect(entry.start_bytes).toMatch(DECIMAL)
      expect(entry.size_bytes).toMatch(DECIMAL)
      expect(Number(entry.start_bytes) % ALIGN).toBe(0)
      expect(Number(entry.size_bytes) % ALIGN).toBe(0)
      expect(entry.type_guid).toMatch(GUID)
      expect(entry.type_guid).toBe(LINUX_DATA)
    }
  })

  it('写进去的分区名能从盘上读回来', () => {
    const named = entries.filter((entry) => entry.name !== '')
    expect(named.map((entry) => entry.name)).toEqual(['data'])
    // 名字挂在第二块分区上；空名字是空串，不是 null、不是解析失败。
    expect(named[0]?.number).toBe(2)
    expect(entries.filter((entry) => entry.number !== 2).every((entry) => entry.name === '')).toBe(
      true,
    )
  })

  it('详情页拿这份载荷能渲染，且没有把格式化说成可以做', async () => {
    const html = await render(DeviceDetailPanel, {
      device,
      edges: detail.edges,
      intervalMs: detail.interval_ms,
      collectedAt: detail.collected_at,
    })
    expect(html).toContain('GPT')
    for (const part of kernelParts) {
      expect(html).toContain(part.device_id)
    }
    // 能力只可能是这四个存储动作里的，界面不能凭空多出别的。
    const ACTIONS = new Set([
      'storage.partition_table.initialize',
      'storage.partition.create',
      'storage.partition.delete',
      'storage.filesystem.format',
    ])
    for (const cap of device.capabilities) {
      expect(ACTIONS.has(cap.action)).toBe(true)
    }
    // 格式化（P07.3）还没有执行器，它绝不能以 available 出现在界面上。
    expect(html).not.toContain('storage.filesystem.format（available）')
    // 有动作就逐条列出来；没有时要说清是“载荷里没这个字段”（版本不一致），
    // 不能画成“这台设备没有可做的事”——两种情况都不能不说。
    if (device.capabilities.length === 0) {
      expect(html).toContain('代理与页面的版本不一致')
    } else {
      for (const cap of device.capabilities) {
        expect(html).toContain(`${cap.action}（${cap.state}）`)
      }
    }
  })
})
