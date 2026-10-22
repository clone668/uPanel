// 用真实 Ubuntu 数据回放“磁盘页面完整交互”（P07.5）。
//
// 这些 JSON 不是造的：验收 VM（upanel-2404）上跑完
//   GPT → 建分区 → mkfs ext4 → 纳入管理 → 开自动挂载 → 挂载 → 写入文件
//   → 占用扫描 → 卸载 → 重挂载 → 退出管理
// 之后，由 tests/integration/p07-5-storage-page-acceptance.sh 原样抓回来，
// 存在 src/fixtures/p07-5-vm/ 下（见那里的 README）。
//
// 为什么非要真实载荷：手写夹具天然自洽，测不出“字段缺失/空值语义/两源对不上”。
// 这一层就是 P07.5 第一轮真机跑撞出的那个缺陷的回归锁——未挂载的分区在真机上
// `filesystem.type` 是 null（CONTRACTS 第 231 行），而当时的候选判据要求它非 null，
// 于是导入表单在真机上永远是空的（DECISIONS Q208）。
//
// 断言的都是不变量：字段形状、闭集成员、空值语义、两源一致。
// 不写死 UUID、pid、字节数——那是某一次运行的样子，不是产品的不变量。

import { readFileSync } from 'node:fs'

import { describe, expect, it, vi } from 'vitest'
import { createSSRApp, h, type Component, type VNodeChild } from 'vue'
import { createMemoryHistory, createRouter, type Router } from 'vue-router'
import { renderToString } from 'vue/server-renderer'

import type { StorageDevice, StorageDeviceDetail, StorageTopology } from '@/api/storage'
import {
  busySummary,
  importCandidates,
  type Volume,
  type VolumeBlockers,
} from '@/api/volumes'
import DeviceDetailPanel from './DeviceDetailPanel.vue'
import VolumeCard from './VolumeCard.vue'

// naive-ui 在 SSR 里要挂样式（需要 document）。换成最小实现：文字与 disabled 原样带出来，
// 断言仍然断的是“这份真实载荷被画成了什么”。
vi.mock('naive-ui', async () => {
  const { h: hh } = await import('vue')
  const stub = (name: string) => ({
    name: `Stub${name}`,
    props: ['title', 'description', 'status', 'disabled', 'value', 'options', 'checked', 'type', 'size', 'placeholder', 'maxlength', 'min', 'max', 'step', 'bordered', 'secondary', 'loading', 'percentage', 'showIndicator', 'height'],
    setup(
      props: { title?: unknown; description?: unknown; disabled?: unknown; type?: unknown },
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
        if (typeof props.type === 'string') attrs['data-type'] = props.type
        return hh('div', attrs, children)
      }
    },
  })
  return {
    NAlert: stub('alert'),
    NButton: stub('button'),
    NCard: stub('card'),
    NCheckbox: stub('checkbox'),
    NEmpty: stub('empty'),
    NInput: stub('input'),
    NInputNumber: stub('input-number'),
    NModal: stub('modal'),
    NProgress: stub('progress'),
    NResult: stub('result'),
    NSelect: stub('select'),
    NSpin: stub('spin'),
    NSwitch: stub('switch'),
    NTag: stub('tag'),
  }
})

interface Envelope<T> {
  data: T
}

function fixture<T>(name: string): T {
  const raw = readFileSync(new URL(`../fixtures/p07-5-vm/${name}`, import.meta.url), 'utf8')
  // 服务端统一包一层 {data, request_id}，前端 client 也只读 data。
  return (JSON.parse(raw) as Envelope<T>).data
}

const topology = fixture<StorageTopology>('page-topology.json')
const diskDetail = fixture<StorageDeviceDetail>('page-device-disk.json')
const partitionUnmounted = fixture<StorageDeviceDetail>('page-device-partition.json')
const partitionMounted = fixture<StorageDeviceDetail>('page-device-partition-mounted.json')
const volumeMounted = fixture<Volume>('page-volume-mounted.json')
const volumeUnmounted = fixture<Volume>('page-volume-unmounted.json')
const volumesList = fixture<{ volumes: Volume[] }>('page-volumes-one-mounted.json')
const blockersBusy = fixture<VolumeBlockers>('page-blockers-busy.json')
const blockersIdle = fixture<VolumeBlockers>('page-blockers-idle.json')
const capabilityDoc = fixture<{
  supported_actions?: string[]
  supported_queries?: string[]
} | null>('agent-capabilities.json')

// 字节数一律十进制字符串（CONTRACTS §6），不是数字、不是 "64MiB"。
const DECIMAL = /^[0-9]+$/
// 存储写动作是闭集里的九个；页面只认这些名字。
const STORAGE_ACTIONS = new Set([
  'storage.partition_table.initialize',
  'storage.partition.create',
  'storage.partition.delete',
  'storage.filesystem.format',
  'storage.volume.import',
  'storage.volume.mount',
  'storage.volume.unmount',
  'storage.volume.configure',
  'storage.volume.forget',
])

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

describe('真实 VM 数据：拓扑', () => {
  it('每台设备的身份与保护原因都是服务端给的，不是页面推的', () => {
    expect(topology.nodes.length).toBeGreaterThan(0)
    for (const device of topology.nodes) {
      expect(device.id).toBeTruthy()
      expect(device.size_bytes).toMatch(DECIMAL)
      expect(device.identity_confidence).toBeTruthy()
    }
    // 系统盘必须被标出来，而且要说得出为什么——否则页面没法解释“为什么这块不能格式化”。
    const system = topology.nodes.filter((device) => device.protection.system_disk)
    expect(system.length).toBeGreaterThan(0)
    expect(system.every((device) => device.protection.reasons.length > 0)).toBe(true)
  })

  it('未挂载的分区没有文件系统类型（CONTRACTS 第 231 行，不是猜的）', () => {
    const unmountedPartitions = topology.nodes.filter(
      (device) =>
        device.kind === 'partition' &&
        !device.protection.system_disk &&
        (device.filesystem === null || device.filesystem.mounts.length === 0),
    )
    expect(unmountedPartitions.length).toBeGreaterThan(0)
    for (const device of unmountedPartitions) {
      // 要么整个 filesystem 为 null（udev 抢跑），要么有 UUID 但类型为空。
      expect(device.filesystem?.type ?? null).toBeNull()
    }
  })

  it('回归（Q208）：把这份真实拓扑交给候选判据，未挂载的分区仍然列得出来', () => {
    // 缺陷原样：候选判据要求“未挂载的分区也有文件系统类型”，而真机上那个字段就是 null，
    // 加上“已挂载的排除”，两个条件互为补集 ⇒ 任何真机上候选都永远是空的。
    const candidates = importCandidates(topology.nodes)
    const ids = new Set(candidates.map((device) => device.id))

    // 系统盘上那块未挂载的数据分区（真机上的 sda14）必须列得出来。
    const unmounted = topology.nodes.filter(
      (device) =>
        device.kind === 'partition' &&
        !device.protection.system_disk &&
        (device.filesystem === null || device.filesystem.mounts.length === 0),
    )
    expect(unmounted.length).toBeGreaterThan(0)
    for (const device of unmounted) {
      expect(ids.has(device.id), `${device.id} 应该出现在候选里`).toBe(true)
    }

    // 系统盘保护、已挂载、整盘、loop、其它类型：一个都不能混进来。
    for (const device of candidates) {
      expect(device.kind).toBe('partition')
      expect(device.protection?.system_disk ?? false).toBe(false)
      expect((device.filesystem?.mounts.length ?? 0) > 0).toBe(false)
      const type = device.filesystem?.type ?? null
      // 类型读得出的，只能是面板管的那两种。
      if (type !== null) expect(['ext4', 'xfs']).toContain(type)
    }
  })

  it('刚格式化、还没挂载的那块分区，拿真机载荷过判据也是候选', () => {
    // 这一条用设备详情而不是拓扑：拓扑是页面进来那一刻抓的（那时盘还是空的），
    // 设备详情是产品自己写完盘之后读回来的同一台设备。
    const composed = importCandidates([partitionUnmounted.device, diskDetail.device])
    expect(composed.map((device) => device.id)).toEqual([partitionUnmounted.device.id])
  })
})

describe('真实 VM 数据：设备详情', () => {
  it('整盘：四个动作判定齐全，全落在闭集里', () => {
    const caps = diskDetail.device.capabilities
    expect(caps).toHaveLength(4)
    for (const cap of caps) {
      expect(STORAGE_ACTIONS.has(cap.action)).toBe(true)
      expect(['available', 'blocked', 'unsupported']).toContain(cap.state)
      // 不可做的动作必须给出理由，页面才能照实解释而不是灰掉一个按钮了事。
      if (cap.state !== 'available') expect(cap.reason ?? '').not.toBe('')
    }
  })

  it('整盘：条目表与内核分区列表按槽位号一一对应（两源一致）', () => {
    const table = diskDetail.device.partition_table
    expect(table?.type).toBe('gpt')
    const entries = table?.disk_entries ?? []
    const kernel = table?.partitions ?? []
    expect(entries.length).toBe(kernel.length)
    expect(entries.length).toBeGreaterThan(0)
    const byNumber = new Map(kernel.map((part) => [part.number, part]))
    for (const entry of entries) {
      const part = byNumber.get(entry.number)
      expect(part, `内核里没有槽位 ${entry.number}`).toBeDefined()
      expect(entry.start_bytes).toBe(part?.start_bytes)
      expect(entry.size_bytes).toBe(part?.size_bytes)
    }
  })

  it('同一个分区：没挂载时读不到类型，挂上之后 type 是 ext4 且能看到挂载点', () => {
    // 对照组。这两条放在一起才说明前一条不是实现偷懒。
    expect(partitionUnmounted.device.id).toBe(partitionMounted.device.id)
    expect(partitionUnmounted.device.filesystem?.type ?? null).toBeNull()

    const mounted = partitionMounted.device.filesystem
    expect(mounted).not.toBeNull()
    expect(mounted?.type).toBe('ext4')
    expect(mounted?.mounts.length ?? 0).toBeGreaterThan(0)
    expect(mounted?.mounts[0]?.path).toBe(volumeMounted.mount_path)
    // UUID 两源一致：卷的登记 UUID 就是设备上读到的那个。
    expect(mounted?.uuid).toBe(volumeMounted.filesystem_uuid)
  })

  it('详情页把这份载荷画出来，动作状态与服务端判定一致', async () => {
    const html = await render(DeviceDetailPanel, {
      device: diskDetail.device as StorageDevice,
      edges: diskDetail.edges,
      intervalMs: diskDetail.interval_ms,
      collectedAt: diskDetail.collected_at,
    })
    expect(html).toContain('GPT')
    for (const cap of diskDetail.device.capabilities) {
      expect(html).toContain(`${cap.action}（${cap.state}）`)
    }
    // 整盘上不该出现“格式化”这个可执行动作：那是分区才有的事。
    expect(html).not.toContain('storage.filesystem.format（available）')
  })
})

describe('真实 VM 数据：受管卷', () => {
  it('挂着的卷：capacity 六个字段都是十进制字符串，不是数字也不是可读文本', () => {
    expect(volumeMounted.observed_state).toBe('mounted')
    expect(volumeMounted.desired_state).toBe('mounted')
    const capacity = volumeMounted.capacity
    expect(capacity).not.toBeNull()
    for (const key of [
      'total_bytes',
      'available_bytes',
      'inode_total',
      'inode_available',
    ] as const) {
      expect(capacity?.[key]).toMatch(DECIMAL)
    }
    expect(Number(capacity?.total_bytes)).toBeGreaterThan(0)
    expect(capacity?.used_percent).toBeGreaterThanOrEqual(0)
    expect(capacity?.used_percent).toBeLessThanOrEqual(100)
  })

  it('没挂的卷：capacity 是 null，不是 0（页面上写“没有容量读数”）', async () => {
    expect(volumeUnmounted.observed_state).not.toBe('mounted')
    expect(volumeUnmounted.capacity).toBeNull()

    const html = await render(VolumeCard, { volume: volumeUnmounted, canAct: true })
    expect(html).toContain('没有容量读数')
    expect(html).not.toContain('0 B')
    // 没挂 => 给的是“挂载”，不是“卸载”。
    expect(html).toContain('data-action="storage.volume.mount"')
    expect(html).not.toContain('data-action="storage.volume.unmount"')
  })

  it('挂着的卷卡片：挂载点、UUID、选项、容量都在，动作换成“卸载”', async () => {
    const html = await render(VolumeCard, { volume: volumeMounted, canAct: true })
    expect(html).toContain(volumeMounted.mount_path)
    expect(html).toContain(volumeMounted.filesystem_uuid)
    expect(html).toContain('nofail')
    expect(html).toContain('开机自动挂载')
    expect(html).not.toContain('没有容量读数')
    expect(html).toContain('data-action="storage.volume.unmount"')
    expect(html).not.toContain('data-action="storage.volume.mount"')
  })

  it('只读账号看不到任何写按钮（这份载荷配 canAct=false）', async () => {
    const html = await render(VolumeCard, { volume: volumeMounted, canAct: false })
    expect(html).toContain('只读账号')
    expect(html).not.toContain('data-action=')
  })

  it('列表载荷里只有服务端报上来的卷，且列表与详情说的是同一个状态', () => {
    expect(Array.isArray(volumesList.volumes)).toBe(true)
    expect(volumesList.volumes.length).toBeGreaterThan(0)
    const same = volumesList.volumes.find((volume) => volume.id === volumeMounted.id)
    expect(same).toBeDefined()
    expect(same?.observed_state).toBe(volumeMounted.observed_state)
    for (const volume of volumesList.volumes) {
      // 不挂载就没有容量——列表不许自己补一个 0。
      if (volume.observed_state !== 'mounted') expect(volume.capacity).toBeNull()
    }
  })
})

describe('真实 VM 数据：占用列表', () => {
  it('忙的时候点名到 pid，pid 与 comm 都是 /proc 里的原话', () => {
    expect(blockersBusy.busy).toBe(true)
    expect(blockersBusy.processes.length).toBeGreaterThan(0)
    for (const process of blockersBusy.processes) {
      expect(process.pid).toMatch(DECIMAL)
      expect(process.pid).not.toBe('0')
      expect(process.comm).not.toBe('')
    }
    // 页面那行摘要用的是同一个函数，说的是“几个进程打开着它”。
    expect(busySummary(blockersBusy)).toContain('个进程打开着它')
  })

  it('占用者退出之后回到不忙：挂着但空闲不算忙（Q192）', () => {
    expect(blockersIdle.busy).toBe(false)
    expect(blockersIdle.mounted).toBe(true)
    expect(blockersIdle.processes).toEqual([])
    expect(busySummary(blockersIdle)).toBe('没有占用')
  })
})

describe('真实 VM 数据：代理能力表', () => {
  it('九个存储写动作全部登记，且写动作不出现在只读查询列表里', () => {
    expect(capabilityDoc).not.toBeNull()
    const actions = new Set(capabilityDoc?.supported_actions ?? [])
    for (const action of STORAGE_ACTIONS) {
      expect(actions.has(action), `能力表里缺 ${action}`).toBe(true)
    }
    const queries = new Set(capabilityDoc?.supported_queries ?? [])
    for (const action of STORAGE_ACTIONS) {
      expect(queries.has(action)).toBe(false)
    }
  })
})
