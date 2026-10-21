// 展示组件的渲染测试（vue/server-renderer）。
//
// 冻结依赖里没有 jsdom，所以这里断言的是模板分支与文案：容量没读到时不能出现
// “0%”、健康未知不能出现“正常”、速率没有采样窗口不能出现“0 B/s”。
// 这些恰恰是“假正常”最容易溜进界面的地方。
//
// 交互行为（点击、折叠、路由跳转）不在这里覆盖，由 Playwright 冒烟负责。

import { describe, expect, it } from 'vitest'
import { createSSRApp, h, type Component } from 'vue'
import { createMemoryHistory, createRouter, type Router } from 'vue-router'
import { renderToString } from 'vue/server-renderer'

import type { SystemSummary } from '@/api/system'
import type {
  StorageCapacity,
  StorageDevice,
  StorageEdge,
  StorageMount,
} from '@/api/storage'
import CapacityBar from './CapacityBar.vue'
import DeviceCard from './DeviceCard.vue'
import DeviceDetailPanel from './DeviceDetailPanel.vue'
import HealthBadge from './HealthBadge.vue'
import IoStats from './IoStats.vue'
import SystemOverviewPanel from './SystemOverviewPanel.vue'

function testRouter(): Router {
  return createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: '/', component: { render: () => null } },
      { path: '/storage/devices/:id', component: { render: () => null } },
    ],
  })
}

async function render(
  component: Component,
  props: Record<string, unknown> = {},
  options: { router?: boolean } = {},
): Promise<string> {
  const app = createSSRApp({ render: () => h(component, props) })
  if (options.router === true) {
    app.use(testRouter())
  }
  return renderToString(app)
}

function capacity(usedPercent: number): StorageCapacity {
  return {
    total_bytes: '1000204886016',
    available_bytes: '400081954037',
    used_percent: usedPercent,
    inode_total: '61000000',
    inode_available: '30000000',
    inode_used_percent: 50.8,
    collected_at: '2026-10-21T09:31:45Z',
  }
}

function mount(overrides: Partial<StorageMount> = {}): StorageMount {
  return {
    path: '/',
    source: '/dev/sda1',
    fstype: 'ext4',
    options: ['rw', 'relatime'],
    read_only: false,
    capacity: capacity(60),
    ...overrides,
  }
}

function device(overrides: Partial<StorageDevice> = {}): StorageDevice {
  return {
    id: 'sda',
    kind: 'disk',
    name: 'sda',
    size_bytes: '1000204886016',
    logical_sector_bytes: 512,
    physical_sector_bytes: 4096,
    model: 'QEMU HARDDISK',
    serial: null,
    wwn: null,
    transport: 'scsi',
    rotational: true,
    read_only: false,
    online: true,
    identity_confidence: 'low',
    identity_fingerprint: `sha256:${'a'.repeat(64)}`,
    filesystem: null,
    partition_table: null,
    health: {
      status: 'unknown',
      temperature_celsius: null,
      source: 'none',
      checked_at: null,
      details: null,
    },
    capabilities: [],
    protection: { system_disk: false, reasons: [], sources: [] },
    io: null,
    collected_at: '2026-10-21T09:31:45Z',
    ...overrides,
  }
}

describe('HealthBadge', () => {
  it('healthy 显示“正常”，并说明依据是来源判定', async () => {
    const html = await render(HealthBadge, {
      health: {
        status: 'healthy',
        temperature_celsius: 38,
        source: 'nvme',
        checked_at: '2026-10-21T09:31:45Z',
        details: 'SMART overall-health self-assessment test result: PASSED',
      },
      detailed: true,
    })
    expect(html).toContain('data-health="healthy"')
    expect(html).toContain('正常')
    expect(html).toContain('健康数据来源判定通过')
    expect(html).toContain('38 °C')
    expect(html).toContain('SMART overall-health self-assessment test result: PASSED')
  })

  it('unknown 不能显示成正常，且温度显示 — 而不是 0 °C', async () => {
    const html = await render(HealthBadge, {
      health: {
        status: 'unknown',
        temperature_celsius: 0,
        source: 'smartctl',
        checked_at: null,
        details: 'Unknown USB bridge',
      },
    })
    expect(html).toContain('data-health="unknown"')
    expect(html).toContain('未知')
    expect(html).not.toContain('健康数据来源判定通过')
    expect(html).toContain('—')
    expect(html).not.toContain('0 °C')
    expect(html).toContain('没有读数')
  })

  it('sleeping 说明“没有去唤醒它”', async () => {
    const html = await render(HealthBadge, {
      health: {
        status: 'sleeping',
        temperature_celsius: null,
        source: 'sysfs',
        checked_at: null,
        details: null,
      },
    })
    expect(html).toContain('休眠中')
    expect(html).toContain('没有读取健康数据')
  })

  it('warning 与 critical 打上“需要处理”', async () => {
    const warning = await render(HealthBadge, {
      health: { status: 'warning', temperature_celsius: 55, source: 'smartctl', checked_at: null, details: null },
    })
    expect(warning).toContain('需要处理')
    const critical = await render(HealthBadge, {
      health: { status: 'critical', temperature_celsius: null, source: 'smartctl', checked_at: null, details: null },
    })
    expect(critical).toContain('需要处理')
    expect(critical).toContain('尽快备份')
  })
})

describe('CapacityBar', () => {
  it('有读数时显示百分比与总量', async () => {
    const html = await render(CapacityBar, { capacity: capacity(60) })
    expect(html).toContain('data-has-capacity="true"')
    expect(html).toContain('60%')
    expect(html).toContain('931.51 GiB')
    expect(html).toContain('inode 已用 50.8%')
  })

  it('没有读数时写“容量未知”，绝不出现 0%', async () => {
    const html = await render(CapacityBar, {
      capacity: null,
      error: '统计 /mnt/nfs 超时超过 2 秒，5 分钟内不再重试',
    })
    expect(html).toContain('data-has-capacity="false"')
    expect(html).toContain('容量未知（不是 0，是没读到）')
    expect(html).toContain('统计 /mnt/nfs 超时超过 2 秒')
    expect(html).not.toContain('0%')
  })

  it('确实是 0% 时照实显示 0%', async () => {
    const html = await render(CapacityBar, { capacity: capacity(0) })
    expect(html).toContain('0%')
    expect(html).not.toContain('容量未知')
  })
})

describe('IoStats', () => {
  it('rate 为 null 时说明没有采样窗口，而不是 0 B/s', async () => {
    const html = await render(IoStats, {
      io: {
        sampled_at: '2026-10-21T09:31:45Z',
        read_bytes_total: '1048576',
        write_bytes_total: '0',
        reads_total: '120',
        writes_total: '0',
        rate: null,
      },
    })
    expect(html).toContain('1 MiB')
    expect(html).toContain('没有可用的采样窗口')
    expect(html).not.toContain('0 B/s')
  })

  it('有 rate 时显示速率、IOPS 与采样窗口', async () => {
    const html = await render(IoStats, {
      io: {
        sampled_at: '2026-10-21T09:31:45Z',
        read_bytes_total: '1048576',
        write_bytes_total: '2048',
        reads_total: '120',
        writes_total: '8',
        rate: {
          interval_ms: 5000,
          read_bytes_per_second: '2048',
          write_bytes_per_second: '0',
          read_iops: 1.25,
          write_iops: 0,
          read_latency_ms: 0.4567,
          write_latency_ms: null,
        },
      },
    })
    expect(html).toContain('2 KiB/s')
    expect(html).toContain('0 B/s')
    expect(html).toContain('1.25')
    expect(html).toContain('0.457 ms')
    expect(html).toContain('采样窗口 5000 毫秒')
    // 没有完成的写：时延是 —，不是 0 ms。
    expect(html).toContain('—')
  })

  it('io 整体为 null 时说明 /proc/diskstats 没读到', async () => {
    const html = await render(IoStats, { io: null })
    expect(html).toContain('没有 I/O 数据')
  })
})

describe('DeviceCard', () => {
  it('系统盘带受保护标记，容量用 IEC 单位 + 精确字节 title', async () => {
    const html = await render(
      DeviceCard,
      {
        device: device({
          protection: { system_disk: true, reasons: ['承载根文件系统'], sources: ['sda1'] },
          filesystem: { type: 'ext4', uuid: 'abcd', label: null, mounts: [mount()] },
        }),
      },
      { router: true },
    )
    expect(html).toContain('系统盘（受保护）')
    expect(html).toContain('931.51 GiB')
    expect(html).toContain('title="1000204886016 字节"')
    expect(html).toContain('href="/storage/devices/sda"')
    // unknown 是“没读到”，不是“不通过”：不给告警标签，交给 HealthBadge 说清楚。
    expect(html).not.toContain('健康需要处理')
  })

  it('只有 warning/critical 才挂“健康需要处理”', async () => {
    const warning = await render(
      DeviceCard,
      { device: device({ health: { ...device().health, status: 'warning' } }) },
      { router: true },
    )
    expect(warning).toContain('健康需要处理')

    const unsupported = await render(
      DeviceCard,
      {
        device: device({
          kind: 'partition',
          health: { ...device().health, status: 'unsupported' },
        }),
      },
      { router: true },
    )
    expect(unsupported).not.toContain('健康需要处理')
  })

  it('两块容量完全相同的盘不会互相顶替：各自显示自己的名字、序列号与标识', async () => {
    const sdc = device({
      id: 'sdc',
      name: 'sdc',
      size_bytes: '4294967296',
      serial: 'UPANEL-DATA-B-4G',
      identity_fingerprint: `sha256:${'b'.repeat(64)}`,
    })
    const sdd = device({
      id: 'sdd',
      name: 'sdd',
      size_bytes: '4294967296',
      serial: 'UPANEL-DATA-C-4G',
      identity_fingerprint: `sha256:${'c'.repeat(64)}`,
    })
    expect(sdc.size_bytes).toBe(sdd.size_bytes)

    const a = await render(DeviceCard, { device: sdc }, { router: true })
    const b = await render(DeviceCard, { device: sdd }, { router: true })
    for (const [html, id, serial, fp] of [
      [a, 'sdc', 'UPANEL-DATA-B-4G', 'b'],
      [b, 'sdd', 'UPANEL-DATA-C-4G', 'c'],
    ] as const) {
      expect(html).toContain(`data-device-id="${id}"`)
      expect(html).toContain(`href="/storage/devices/${id}"`)
      expect(html).toContain(serial)
      // 标识前缀跟着设备走：同名同容量的两块盘在界面上也是两个东西。
      expect(html).toContain(`sha256:${fp.repeat(12)}…`)
    }
    expect(a).not.toContain('UPANEL-DATA-C-4G')
    expect(b).not.toContain('UPANEL-DATA-B-4G')
  })

  it('dm/LVM 盘与它下面的分区各自只显示自己的容量，从不相加', async () => {
    // 同一份 1 TB 空间在 /sys 里出现两次（底层分区 + dm 设备）：
    // 各自照实显示会看到两个 931.32 GiB，加起来是 1.82 TiB —— 后者绝不能出现在任何一张卡片上。
    const backing = device({
      id: 'sda3',
      name: 'sda3',
      kind: 'partition',
      size_bytes: '1000204886016',
    })
    const dm = device({
      id: 'dm-0',
      name: 'dm-0',
      kind: 'lvm',
      size_bytes: '1000204886016',
      identity_fingerprint: `sha256:${'d'.repeat(64)}`,
    })

    for (const dev of [backing, dm]) {
      const html = await render(DeviceCard, { device: dev }, { router: true })
      expect(html).toContain('931.51 GiB')
      expect(html).not.toContain('1.82 TiB')
      expect(html).not.toContain('1863.02 GiB')
    }
  })

  it('未挂载的盘也照实显示，不编造容量', async () => {
    const html = await render(
      DeviceCard,
      { device: device({ id: 'sdb', health: { ...device().health, status: 'healthy' } }) },
      { router: true },
    )
    expect(html).toContain('未挂载')
    expect(html).not.toContain('容量未知')
    expect(html).not.toContain('健康需要处理')
  })

  it('多挂载点时提示还有几个，并显示最满的那个', async () => {
    const html = await render(
      DeviceCard,
      {
        device: device({
          filesystem: {
            type: 'ext4',
            uuid: null,
            label: null,
            mounts: [
              mount({ path: '/srv/upanel/mm/a', capacity: capacity(20) }),
              mount({ path: '/srv/upanel/mm/b', capacity: capacity(88) }),
            ],
          },
        }),
      },
      { router: true },
    )
    expect(html).toContain('/srv/upanel/mm/b')
    expect(html).toContain('88%')
    expect(html).toContain('另有 1 个挂载点')
  })

  it('没有采样窗口时写明“不是 0”', async () => {
    const html = await render(
      DeviceCard,
      {
        device: device({
          io: {
            sampled_at: '2026-10-21T09:31:45Z',
            read_bytes_total: '0',
            write_bytes_total: '0',
            reads_total: '0',
            writes_total: '0',
            rate: null,
          },
        }),
      },
      { router: true },
    )
    expect(html).toContain('没有可用采样窗口，不是 0')
  })
})

describe('DeviceDetailPanel', () => {
  const edges: StorageEdge[] = [
    { from_device_id: 'sda1', to_device_id: 'sda', relation: 'partition_of' },
  ]

  it('分区表、空闲区、保护理由与交换区都摊开', async () => {
    const html = await render(
      DeviceDetailPanel,
      {
        device: device({
          protection: { system_disk: true, reasons: ['承载根文件系统 /'], sources: ['sda1'] },
          partition_table: {
            type: 'gpt',
            partitions: [
              { device_id: 'sda1', number: 1, start_bytes: '1048576', size_bytes: '1073741824' },
            ],
            free_regions: [{ start_bytes: '1074790400', size_bytes: '999000000000' }],
            source: 'device',
          },
          holders: ['dm-0'],
          swap: { type: 'partition', size_bytes: '2147483648', used_bytes: '0', priority: -2 },
          filesystem: { type: 'ext4', uuid: 'abcd', label: 'root', mounts: [mount()] },
        }),
        edges,
        intervalMs: 5000,
        collectedAt: '2026-10-21T09:31:45Z',
      },
      { router: true },
    )
    expect(html).toContain('GPT')
    expect(html).toContain('读了磁盘上的分区表')
    expect(html).toContain('sda1')
    expect(html).toContain('1 GiB')
    expect(html).toContain('承载根文件系统 /')
    expect(html).toContain('dm-0')
    expect(html).toContain('交换区')
    expect(html).toContain('所属整盘')
    expect(html).toContain('每 5 秒刷新')
  })

  it('没有分区表时说明原因，而不是显示空表', async () => {
    const html = await render(
      DeviceDetailPanel,
      {
        device: device({ kind: 'lvm', id: 'dm-0', name: 'dm-0' }),
        edges: [],
        intervalMs: 5000,
        collectedAt: '2026-10-21T09:31:45Z',
      },
      { router: true },
    )
    expect(html).toContain('没有可读的分区表信息')
    expect(html).toContain('没有与这台设备直接相关的关系边')
  })

  it('capabilities 为空时明说“没有可执行动作”，不摆按钮', async () => {
    const html = await render(
      DeviceDetailPanel,
      {
        device: device(),
        edges: [],
        intervalMs: 5000,
        collectedAt: '2026-10-21T09:31:45Z',
      },
      { router: true },
    )
    expect(html).toContain('data-testid="no-actions"')
    expect(html).toContain('当前阶段没有可执行的动作')
    expect(html).not.toContain('>格式化<')
    expect(html).not.toContain('>删除分区<')
  })
})

describe('SystemOverviewPanel', () => {
  function summary(overrides: Partial<SystemSummary> = {}): SystemSummary {
    return {
      collected_at: '2026-10-21T09:31:45Z',
      interval_ms: 5000,
      stale: false,
      degraded: false,
      host: {
        status: 'ok',
        hostname: 'upanel-2404',
        os_pretty_name: 'Ubuntu 24.04.5 LTS',
        os_id: 'ubuntu',
        kernel_release: '6.8.0-147-generic',
        arch: 'x86_64',
        uptime_seconds: 90061,
      },
      cpu: {
        status: 'ok',
        logical_cores: 2,
        usage_percent: 3.25,
        sample_window_ms: 5000,
        model: 'QEMU Virtual CPU',
        per_cpu: [
          { index: 0, usage_percent: 1.5 },
          { index: 1, usage_percent: null },
        ],
      },
      memory: {
        status: 'ok',
        total_bytes: '2048000000',
        free_bytes: '1024000000',
        available_bytes: '1536000000',
        buffers_bytes: '10000000',
        cached_bytes: '500000000',
        used_bytes: '512000000',
        swap_total_bytes: '0',
        swap_free_bytes: '0',
      },
      load: { status: 'ok', load1: 0.1, load5: 0.2, load15: 0.15, runnable: 1, threads: 120 },
      network: {
        status: 'ok',
        interfaces: [
          {
            name: 'eth0',
            oper_state: 'up',
            loopback: false,
            rx_bytes: '1024',
            tx_bytes: '2048',
            rx_packets: '10',
            tx_packets: '20',
            rx_errors: '0',
            tx_errors: '0',
            rx_dropped: '0',
            tx_dropped: '0',
            rx_bytes_per_second: 1024,
            tx_bytes_per_second: null,
          },
        ],
      },
      ...overrides,
    }
  }

  it('正常快照显示主机、CPU、内存、负载与网卡', async () => {
    const html = await render(SystemOverviewPanel, { summary: summary() })
    expect(html).toContain('upanel-2404')
    expect(html).toContain('Ubuntu 24.04.5 LTS')
    expect(html).toContain('1 天 1 小时')
    expect(html).toContain('3.3%')
    expect(html).toContain('25%')
    expect(html).toContain('未启用')
    expect(html).toContain('eth0')
    expect(html).toContain('1 KiB/s')
    // 单个核没有可比样本时是 —。
    expect(html).toContain('cpu1')
  })

  it('段失败时画状态面板并带出服务端原话，不画数字', async () => {
    const html = await render(SystemOverviewPanel, {
      summary: summary({
        degraded: true,
        cpu: { status: 'error', error: '读取 /proc/stat 失败', logical_cores: 0, usage_percent: null },
      }),
    })
    expect(html).toContain('有采集段不可用')
    expect(html).toContain('CPU信息不可用')
    expect(html).toContain('读取 /proc/stat 失败')
    // 失败段不显示逻辑核数字。
    expect(html).not.toContain('逻辑核')
  })

  it('数据源不存在时判为不支持，而不是“为 0”', async () => {
    const html = await render(SystemOverviewPanel, {
      summary: summary({ degraded: true, network: { status: 'unavailable' } }),
    })
    expect(html).toContain('网络信息不可用')
    expect(html).toContain('数据不支持')
  })

  it('陈旧快照会显式标注', async () => {
    const html = await render(SystemOverviewPanel, { summary: summary({ stale: true }) })
    expect(html).toContain('这份快照已陈旧')
  })
})
