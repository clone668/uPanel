// 用真实 Ubuntu 数据渲染页面组件（P04.4）。
//
// 这里的 JSON 不是造的：它们是验收 VM（upanel-2404，Ubuntu 24.04.5）上真实运行
// upanel-web（production 模式、root 代理）后用 tests/integration/p04-4-ui-acceptance.sh
// 抓回来的响应，原样存在 src/fixtures/p04-4-vm/ 下（连 {data, request_id} 外壳都没拆）。
//
// 为什么要这一层：单元测试里的夹具是我自己编的，容易“编得比现实干净”。真实数据里有
// 序列号读不到的 virtio 盘、没有采样窗口所以 rate 为 null 的 I/O、只用掉 6.6% 的根分区、
// 被保护的 /boot 与 EFI 分区 —— 这些正是界面最容易悄悄显示成 0 / 正常 / 未知的地方。
// 这里把它们逐个钉住。

import { readFileSync } from 'node:fs'

import { describe, expect, it } from 'vitest'
import { createSSRApp, h, type Component } from 'vue'
import { createMemoryHistory, createRouter, type Router } from 'vue-router'
import { renderToString } from 'vue/server-renderer'

import type { SystemSummary } from '@/api/system'
import {
  attentionDevices,
  splitDevices,
  type StorageDevice,
  type StorageDeviceDetail,
  type StorageDeviceList,
  type StorageTopology,
} from '@/api/storage'
import DeviceCard from './DeviceCard.vue'
import DeviceDetailPanel from './DeviceDetailPanel.vue'
import SystemOverviewPanel from './SystemOverviewPanel.vue'
import { formatBytes, formatPercent, formatRate } from '@/utils/format'

function fixture<T>(name: string): T {
  const raw = readFileSync(new URL(`../fixtures/p04-4-vm/${name}`, import.meta.url), 'utf8')
  // 服务端统一包一层 {data, request_id}，前端 client 也只读 data。
  const body = JSON.parse(raw) as { data: T }
  return body.data
}

const summary = fixture<SystemSummary>('summary.json')
const devices = fixture<StorageDeviceList>('devices.json')
const topology = fixture<StorageTopology>('topology.json')
const detail = fixture<StorageDeviceDetail>('detail-sda.json')

// 判“界面有没有把不该说的说出口”：healthy 的解释句只有真渲染成 healthy 才会出现，
// 比直接搜“正常”两个字可靠（unknown 的解释句里本身就带“不能当作正常”）。
const HEALTHY_CLAIM = '健康数据来源判定通过'

function find(id: string): StorageDevice {
  const hit = devices.devices.find((device) => device.id === id)
  if (!hit) throw new Error(`夹具里没有 ${id}`)
  return hit
}

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

// scoped 样式会给每个元素加 data-v-xxx，所以匹配标签时不能写死成 <strong>。
function strongContaining(text: string): RegExp {
  const escaped = text.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')
  return new RegExp(`<strong[^>]*>${escaped}</strong>`)
}

describe('真实 VM 数据：四份响应的自洽性', () => {
  it('同一份快照：版本号与采集时间一致，拓扑节点就是设备列表里的设备', () => {
    expect(topology.snapshot_version).toBe(devices.snapshot_version)
    expect(topology.collected_at).toBe(devices.collected_at)
    expect(detail.snapshot_version).toBe(devices.snapshot_version)

    const ids = devices.devices.map((device) => device.id).sort()
    expect(topology.nodes.map((node) => node.id).sort()).toEqual(ids)
    expect(topology.edges).toEqual(devices.edges)
  })

  it('每条关系边的两端都在这份快照里', () => {
    const ids = new Set(topology.nodes.map((node) => node.id))
    for (const edge of topology.edges) {
      expect(ids.has(edge.from_device_id)).toBe(true)
      expect(ids.has(edge.to_device_id)).toBe(true)
    }
  })

  it('列表拆分：整盘与分区进主列表，8 个 loop 进背景区', () => {
    const split = splitDevices(topology.nodes)
    // fd0 的 kind 是 other（不是 loop/rom），所以它按现状进主列表；这里如实记录。
    expect(split.primary.map((device) => device.id)).toEqual([
      'sda',
      'sdb',
      'sdc',
      'sdd',
      'sda1',
      'sda14',
      'sda15',
      'sda16',
      'fd0',
    ])
    expect(split.background.map((device) => device.id)).toEqual([
      'loop0',
      'loop1',
      'loop2',
      'loop3',
      'loop4',
      'loop5',
      'loop6',
      'loop7',
    ])
    // 全是 unknown / unsupported：不该有人被点亮成告警。
    expect(attentionDevices(topology.nodes)).toEqual([])
    expect(topology.network_mounts).toEqual([])
  })
})

describe('真实 VM 数据：系统概况', () => {
  it('显示主机名、内核与发行版，CPU 使用率按真实区间算出来', async () => {
    const html = await render(SystemOverviewPanel, { summary })
    expect(html).toContain('upanel-2404')
    expect(html).toContain('Ubuntu 24.04.5 LTS')
    expect(html).toContain('6.8.0-147-generic')
    expect(html).toContain('QEMU Virtual CPU version 2.5+')

    // 验收时等过第一次采样，所以这里有真实的区间读数（不是 null，也不是编的 0）。
    expect(summary.cpu.usage_percent).toBeGreaterThanOrEqual(0)
    expect(summary.cpu.usage_percent).toBeLessThanOrEqual(100)
    expect(html).toContain(formatPercent(summary.cpu.usage_percent, 1))
    expect(summary.cpu.per_cpu?.length).toBe(2)
  })

  it('内存按已用/总量说，swap 为 0 时说未启用而不是 0 B', async () => {
    const html = await render(SystemOverviewPanel, { summary })
    expect(summary.memory.total_bytes).toBe('4106088448')
    expect(html).toContain('3.82 GiB')
    expect(summary.memory.swap_total_bytes).toBe('0')
    expect(html).toContain('未启用')
  })

  it('网卡用真实名字与状态，速率没算出来就是 —', async () => {
    const html = await render(SystemOverviewPanel, { summary })
    expect(html).toContain('ens4')
    expect(html).toContain('lo')
    expect(html).toContain('up')
    expect(html).not.toContain('0 B/s')
  })
})

describe('真实 VM 数据：设备卡片', () => {
  it('40 GiB 的 virtio 盘：容量、型号、介质照实写，健康是“不支持检测”而不是正常', async () => {
    const sda = find('sda')
    const html = await render(DeviceCard, { device: sda })

    expect(html).toContain('data-device-id="sda"')
    expect(html).toContain('40 GiB')
    expect(html).toContain('42949672960 字节')
    expect(html).toContain('QEMU HARDDISK')
    expect(html).toContain('virtio')
    expect(html).toContain('旋转介质（HDD）')

    // QEMU 的虚拟盘：smartctl 说 smart_support.available=false，
    // 所以是“不支持检测”，而不是“没读到”，更不是“正常”。
    expect(sda.health.status).toBe('unsupported')
    expect(html).toContain('data-health="unsupported"')
    expect(html).toContain('不支持检测')
    expect(html).not.toContain(HEALTHY_CLAIM)
    expect(html).not.toContain('健康需要处理')

    // 温度没读到 → —，不是 0 °C。
    expect(sda.health.temperature_celsius).toBeNull()
    expect(html).not.toContain('0 °C')

    // 这块盘承载根文件系统：必须打上受保护标签，理由来自真实探测而不是盘名猜测。
    expect(sda.protection.system_disk).toBe(true)
    expect(html).toContain('系统盘（受保护）')
  })

  it('整盘本身没挂载就写未挂载，不借用分区的容量', async () => {
    const html = await render(DeviceCard, { device: find('sda') })
    expect(find('sda').filesystem).toBeNull()
    expect(html).toContain('未挂载')
    expect(html).not.toContain('容量未知')
    expect(html).not.toContain('已用')
  })

  it('有采样窗口就显示真实速率，读 0 B/s 是真 0；累计计数照实显示', async () => {
    const sda1 = find('sda1')
    const rate = sda1.io?.rate
    expect(rate).toBeTruthy()
    if (!rate) throw new Error('夹具里 sda1 没有速率')
    expect(rate.interval_ms).toBeGreaterThanOrEqual(1)
    // 计数器是累计值，每次开机都不同：只钉格式与单调性。
    expect(sda1.io?.read_bytes_total).toMatch(/^[0-9]+$/)
    expect(BigInt(sda1.io?.read_bytes_total ?? '0')).toBeGreaterThan(BigInt(0))

    const html = await render(DeviceCard, { device: sda1 })
    expect(html).toContain(formatRate(rate.read_bytes_per_second))
    expect(html).toContain(formatRate(rate.write_bytes_per_second))
    // 这一次真的算出了速率，就不该再出现“没有采样窗口”的说明。
    expect(html).not.toContain('没有可用采样窗口，不是 0')
  })

  it('根分区显示真实使用率与容量，不四舍五入也不画成 0%', async () => {
    const sda1 = find('sda1')
    const capacity = sda1.filesystem?.mounts[0]?.capacity
    expect(capacity).toBeTruthy()
    if (!capacity) throw new Error('夹具里 sda1 没有容量')

    const html = await render(DeviceCard, { device: sda1 })
    expect(html).toContain('挂载点')
    // 百分比与容量都按服务端给的真实数字渲染（这个数每次开机都会变，所以从数据里取）。
    expect(html).toMatch(strongContaining(formatPercent(capacity.used_percent, 1)))
    expect(html).toContain(formatBytes(capacity.total_bytes))
    expect(html).toContain(formatBytes(capacity.available_bytes))
    expect(html).toContain('ext4')
    expect(capacity.used_percent).toBeGreaterThan(0)
    expect(html).not.toContain('容量未知')
  })

  it('/boot 与 EFI 分区各自显示自己的容量与挂载点', async () => {
    const boot = find('sda16')
    const bootCapacity = boot.filesystem?.mounts[0]?.capacity
    expect(bootCapacity).toBeTruthy()
    if (!bootCapacity) throw new Error('夹具里 sda16 没有容量')

    const bootHtml = await render(DeviceCard, { device: boot })
    expect(bootHtml).toContain('/boot<')
    expect(bootHtml).toMatch(strongContaining(formatPercent(bootCapacity.used_percent, 1)))
    expect(bootHtml).toContain('系统盘（受保护）')

    const efi = await render(DeviceCard, { device: find('sda15') })
    expect(efi).toContain('/boot/efi')
    expect(efi).toContain('vfat')
  })

  it('两块同容量、都没有序列号的盘不会互相顶替（真实数据里 sdc 与 sdd 都是 4 GiB、serial=null）', async () => {
    const sdc = find('sdc')
    const sdd = find('sdd')
    expect(sdc.size_bytes).toBe(sdd.size_bytes)
    expect(sdc.serial).toBeNull()
    expect(sdd.serial).toBeNull()
    // 序列号读不到，身份就只能靠稳定标识：两者必须不同。
    expect(sdc.identity_fingerprint).not.toBe(sdd.identity_fingerprint)

    const a = await render(DeviceCard, { device: sdc })
    const b = await render(DeviceCard, { device: sdd })
    expect(a).toContain('data-device-id="sdc"')
    expect(b).toContain('data-device-id="sdd"')
    const prefix = (dev: StorageDevice) => dev.identity_fingerprint.slice(0, 19)
    expect(a).toContain(`${prefix(sdc)}…`)
    expect(b).toContain(`${prefix(sdd)}…`)
    expect(a).not.toContain(prefix(sdd))
    // 读不到序列号就写未读到，不借用别的东西冒充。
    expect(a).toContain('序列号：未读到')
  })

  it('loop 设备的健康是“不支持检测”，不冒充正常', async () => {
    const html = await render(DeviceCard, { device: find('loop0') })
    expect(find('loop0').health.status).toBe('unsupported')
    expect(html).toContain('不支持检测')
    expect(html).not.toContain(HEALTHY_CLAIM)
  })
})

describe('真实 VM 数据：设备详情', () => {
  const detailProps = () => ({
    device: detail.device,
    edges: detail.edges,
    intervalMs: detail.interval_ms,
    collectedAt: detail.collected_at,
  })

  it('GPT 分区表、四块分区与两段空闲区都能看到', async () => {
    const html = await render(DeviceDetailPanel, detailProps())
    expect(html).toContain('GPT')
    expect(html).toContain('读了磁盘上的分区表')
    for (const id of ['sda1', 'sda14', 'sda15', 'sda16']) {
      expect(html).toContain(id)
    }
    expect(detail.device.partition_table?.free_regions.length).toBe(2)
    expect(html).toContain('空闲区（2）')
  })

  it('健康结论在详情页带上 smartctl 的原话与检查时间', async () => {
    const html = await render(DeviceDetailPanel, detailProps())
    expect(detail.device.health.status).toBe('unsupported')
    expect(detail.device.health.checked_at).toBeTruthy()
    expect(html).toContain('设备不支持 SMART')
    // 不能只给一句中文结论：要能看到这句话是从 smartctl 的哪个字段来的。
    expect(html).toContain('smart_support.available=false')
    expect(html).toContain('来源：smartctl')
  })

  it('系统盘保护列出理由，并指出是哪几块分区撑起来的', async () => {
    const html = await render(DeviceDetailPanel, detailProps())
    expect(detail.device.protection.system_disk).toBe(true)
    expect(detail.device.protection.reasons).toContain('承载根文件系统')
    expect(html).toContain('承载根文件系统')
    expect(html).toContain('承载 /boot（启动所需）')
    expect(html).toContain('受保护')
    for (const id of ['sda1', 'sda15', 'sda16']) {
      expect(detail.device.protection.sources).toContain(id)
    }
  })

  it('本阶段不显示任何可执行动作', async () => {
    const html = await render(DeviceDetailPanel, detailProps())
    expect(detail.device.capabilities).toEqual([])
    expect(html).toContain('当前阶段没有可执行的动作')
    // 没有按钮：不给“点了什么都不会发生”的入口。
    expect(html).not.toContain('<button')
  })
})
