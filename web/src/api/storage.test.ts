// 存储 DTO 的纯逻辑测试。
//
// 这里不测“渲染好不好看”，只测判决逻辑：
//   - 每个封闭集合都有中文标签（缺一个就说明契约没对齐）；
//   - null 不被当成 0（容量、温度、速率）；
//   - 折叠只影响“主列表/背景列表”的划分，不丢设备；
//   - 设备 id 只作为路径参数，非法 id 不进 URL。

import { afterEach, describe, expect, it, vi } from 'vitest'

import {
  DEVICE_ID_PATTERN,
  DEVICE_KINDS,
  DEVICE_KIND_LABELS,
  EDGE_RELATION_LABELS,
  HEALTH_STATUSES,
  HEALTH_STATUS_LABELS,
  HEALTH_STATUS_TYPES,
  IDENTITY_CONFIDENCES,
  IDENTITY_CONFIDENCE_LABELS,
  PARTITION_TABLE_SOURCE_LABELS,
  PARTITION_TABLE_TYPE_LABELS,
  attentionDevices,
  fetchStorageDevice,
  fetchStorageDevices,
  fetchStorageTopology,
  findDevice,
  healthNeedsAttention,
  highestUsedPercent,
  isBackgroundDevice,
  isMounted,
  mountsOf,
  needsAttentionNetworkMount,
  primaryCapacity,
  reachabilityLabel,
  splitDevices,
  type HealthStatus,
  type StorageCapacity,
  type StorageDevice,
  type StorageMount,
  type StorageNetworkMount,
} from './storage'

function capacity(usedPercent: number): StorageCapacity {
  return {
    total_bytes: '100000000000',
    available_bytes: '40000000000',
    used_percent: usedPercent,
    inode_total: '6100000',
    inode_available: '3000000',
    inode_used_percent: 50.8,
    collected_at: '2026-10-21T09:00:00Z',
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
    collected_at: '2026-10-21T09:00:00Z',
    ...overrides,
  }
}

afterEach(() => {
  vi.unstubAllGlobals()
  vi.restoreAllMocks()
})

describe('封闭集合的中文标签', () => {
  it('八种设备类型都有标签，且 loop / rom 是背景设备', () => {
    for (const kind of DEVICE_KINDS) {
      expect(DEVICE_KIND_LABELS[kind], kind).toBeTruthy()
    }
    expect(DEVICE_KINDS).toHaveLength(8)
    expect(DEVICE_KINDS.filter((kind) => isBackgroundDevice(kind))).toEqual(['loop', 'rom'])
  })

  it('六种健康状态都有标签与颜色，且只有 warning/critical 需要关注', () => {
    for (const status of HEALTH_STATUSES) {
      expect(HEALTH_STATUS_LABELS[status], status).toBeTruthy()
      expect(HEALTH_STATUS_TYPES[status], status).toBeTruthy()
    }
    expect(HEALTH_STATUSES).toHaveLength(6)
    const attention = HEALTH_STATUSES.filter((status: HealthStatus) => healthNeedsAttention(status))
    expect(attention).toEqual(['warning', 'critical'])
  })

  it('健康未知与休眠都不算“需要注意”，也不允许被显示成正常', () => {
    expect(healthNeedsAttention('unknown')).toBe(false)
    expect(healthNeedsAttention('sleeping')).toBe(false)
    expect(healthNeedsAttention('unsupported')).toBe(false)
    expect(HEALTH_STATUS_LABELS.unknown).not.toBe(HEALTH_STATUS_LABELS.healthy)
    expect(HEALTH_STATUS_LABELS.sleeping).not.toBe(HEALTH_STATUS_LABELS.healthy)
  })

  it('身份可信度、分区表类型/来源、边关系都有标签', () => {
    for (const value of IDENTITY_CONFIDENCES) {
      expect(IDENTITY_CONFIDENCE_LABELS[value], value).toBeTruthy()
    }
    expect(Object.keys(PARTITION_TABLE_TYPE_LABELS)).toEqual(['gpt', 'mbr', 'none', 'unknown'])
    expect(Object.keys(PARTITION_TABLE_SOURCE_LABELS)).toEqual(['device', 'kernel', 'skipped'])
    expect(EDGE_RELATION_LABELS.partition_of).toBeTruthy()
    expect(EDGE_RELATION_LABELS.depends_on).toBeTruthy()
  })
})

describe('设备列表的划分与查找', () => {
  const devices = [
    device({ id: 'sda' }),
    device({ id: 'sda1', kind: 'partition' }),
    device({ id: 'loop0', kind: 'loop' }),
    device({ id: 'sr0', kind: 'rom' }),
  ]

  it('背景设备单独一组，但两台都被保留', () => {
    const { primary, background } = splitDevices(devices)
    expect(primary.map((d) => d.id)).toEqual(['sda', 'sda1'])
    expect(background.map((d) => d.id)).toEqual(['loop0', 'sr0'])
    expect(primary.length + background.length).toBe(devices.length)
  })

  it('按 id 查找，找不到是 undefined 而不是空对象', () => {
    expect(findDevice(devices, 'sda1')?.kind).toBe('partition')
    expect(findDevice(devices, 'sdz')).toBeUndefined()
  })
})

describe('容量读取', () => {
  it('没有文件系统时挂载点是空数组，容量是 null', () => {
    const disk = device()
    expect(mountsOf(disk)).toEqual([])
    expect(isMounted(disk)).toBe(false)
    expect(primaryCapacity(disk)).toBeNull()
    expect(highestUsedPercent(disk)).toBeNull()
  })

  it('多挂载点时取最大使用率，并跳过没读到容量的挂载点', () => {
    const disk = device({
      filesystem: {
        type: 'ext4',
        uuid: '0f0e0d0c',
        label: null,
        mounts: [
          mount({ path: '/', capacity: capacity(42) }),
          mount({ path: '/srv/data', capacity: null }),
          mount({ path: '/srv/mm/b', capacity: capacity(88.5) }),
        ],
      },
    })
    expect(isMounted(disk)).toBe(true)
    expect(mountsOf(disk)).toHaveLength(3)
    expect(highestUsedPercent(disk)).toBe(88.5)
    // 第一个“读到了容量”的挂载点，不是第一个挂载点。
    expect(primaryCapacity(disk)?.used_percent).toBe(42)
  })

  it('容量全是 null 时使用率是 null，不是 0', () => {
    const disk = device({
      filesystem: {
        type: 'ext4',
        uuid: null,
        label: null,
        mounts: [mount({ capacity: null, capacity_error: 'statfs 超时' })],
      },
    })
    expect(highestUsedPercent(disk)).toBeNull()
    expect(primaryCapacity(disk)).toBeNull()
  })
})

describe('需要关注的设备', () => {
  it('只挑出 warning 与 critical', () => {
    const devices = [
      device({ id: 'sda', health: { ...device().health, status: 'healthy' } }),
      device({ id: 'sdb', health: { ...device().health, status: 'warning' } }),
      device({ id: 'sdc', health: { ...device().health, status: 'critical' } }),
      device({ id: 'sdd', health: { ...device().health, status: 'sleeping' } }),
      device({ id: 'sde', health: { ...device().health, status: 'unknown' } }),
    ]
    expect(attentionDevices(devices).map((d) => d.id)).toEqual(['sdb', 'sdc'])
  })
})

describe('网络挂载的可达性', () => {
  const base: StorageNetworkMount = {
    path: '/mnt/nfs',
    source: '10.0.2.2:/export',
    fstype: 'nfs4',
    read_only: false,
    reachable: true,
    capacity: null,
  }

  it('三态各有各的说法，不合并', () => {
    expect(reachabilityLabel(true)).toContain('可达')
    expect(reachabilityLabel(false)).toContain('不可达')
    expect(reachabilityLabel(null)).toContain('尚未检查')
    const labels = [reachabilityLabel(true), reachabilityLabel(false), reachabilityLabel(null)]
    expect(new Set(labels).size).toBe(3)
  })

  it('只有确定失败才算需要处理，没查过不算', () => {
    expect(needsAttentionNetworkMount(base)).toBe(false)
    expect(needsAttentionNetworkMount({ ...base, reachable: null })).toBe(false)
    expect(needsAttentionNetworkMount({ ...base, reachable: false })).toBe(true)
  })
})

describe('设备 id 只作为路径参数', () => {  it('合法内核名通过', () => {
    for (const id of ['sda', 'sda1', 'dm-0', 'md0', 'nvme0n1p1', 'loop0']) {
      expect(DEVICE_ID_PATTERN.test(id), id).toBe(true)
    }
  })

  it('带斜杠、空格、点段或超长的 id 被拒', () => {
    for (const id of ['', '../etc/passwd', 'sda/../sdb', 'sda1 ', 'a'.repeat(65), 'sda;rm']) {
      expect(DEVICE_ID_PATTERN.test(id), id).toBe(false)
    }
  })

  it('非法 id 在发请求之前就被拒绝，不拼进 URL', async () => {
    const mock = vi.fn()
    vi.stubGlobal('fetch', mock)
    await expect(fetchStorageDevice('../../etc/shadow')).rejects.toThrow(RangeError)
    expect(mock).not.toHaveBeenCalled()
  })
})

describe('查询函数走统一客户端', () => {
  function jsonResponse(body: unknown, status = 200): Response {
    return new Response(JSON.stringify(body), {
      status,
      headers: { 'Content-Type': 'application/json', 'X-Request-ID': 'rid-storage' },
    })
  }

  it('拓扑、列表、详情各自打到约定路径', async () => {
    const mock = vi.fn(() => Promise.resolve(jsonResponse({ data: {}, request_id: 'rid-storage' })))
    vi.stubGlobal('fetch', mock)

    await fetchStorageTopology()
    await fetchStorageDevices()
    await fetchStorageDevice('sda1')

    const urls = (mock.mock.calls as unknown as [string][]).map((call) => String(call[0]))
    expect(urls[0]).toMatch(/\/api\/v1\/storage\/topology$/)
    expect(urls[1]).toMatch(/\/api\/v1\/storage\/devices$/)
    expect(urls[2]).toMatch(/\/api\/v1\/storage\/devices\/sda1$/)
  })
})
