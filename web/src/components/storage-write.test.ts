// 存储写操作界面的渲染用例（P07.5）。
//
// 环境是 node，没有 DOM，所以用 vue/server-renderer 渲染成字符串再断言：
// 验的是"这份数据最终显示成什么文字"，不是交互（见 docs/DECISIONS.md Q21）。
//
// 这一层盯住的是最容易溜进界面的四种假象：
//   1. 把 desired_state 当成 observed_state 显示（“已挂载”其实没挂上）；
//   2. 容量没读到时画成 0 字节可用；
//   3. unsupported 的动作仍然画一颗点不动的按钮；
//   4. 只读账号看到可点的写入口。
//
// 真机回放用例在 web/src/components/realdata-*.test.ts，数据来自 VM 上抓的真实响应。

import { describe, expect, it, vi } from 'vitest'
import { createSSRApp, h, type Component, type VNodeChild } from 'vue'
import { createMemoryHistory, createRouter, type Router } from 'vue-router'
import { renderToString } from 'vue/server-renderer'

import type { StorageCapacity, StorageDevice } from '@/api/storage'
import type { Volume } from '@/api/volumes'
import DeviceDetailPanel from './DeviceDetailPanel.vue'
import DeviceStorageActions from './DeviceStorageActions.vue'
import VolumeCard from './VolumeCard.vue'
import VolumeImportForm from './VolumeImportForm.vue'
import VolumeSection from './VolumeSection.vue'

// 会话状态只决定“管理员视图还是只读视图”。
const sessionState = vi.hoisted(() => ({ isAdmin: true }))

vi.mock('@/stores/session', () => ({
  useSessionStore: () => ({
    get isAdmin() {
      return sessionState.isAdmin
    },
    restore: () => Promise.resolve(),
  }),
}))

// naive-ui 在 SSR 里会去挂样式（需要 document）。换成最小实现：文字与 disabled 带出来。
vi.mock('naive-ui', async () => {
  const { h } = await import('vue')
  const stub = (name: string) => ({
    name: `Stub${name}`,
    props: ['title', 'description', 'status', 'code', 'disabled', 'value', 'options', 'checked', 'type', 'size', 'loading', 'placeholder', 'maxlength', 'min', 'max', 'step', 'bordered', 'secondary'],
    setup(
      props: { title?: unknown; description?: unknown; disabled?: unknown; type?: unknown; checked?: unknown },
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
        return h('div', attrs, children)
      }
    },
  })
  // 对话框只在 show 为真时渲染：关着的时候页面里不该留下输入框。
  const modal = {
    name: 'StubNModal',
    props: ['show', 'title', 'preset', 'maskClosable'],
    setup(props: { show?: unknown; title?: unknown }, ctx: { slots: { default?: () => unknown } }) {
      return () => {
        if (props.show !== true) return null
        const children: VNodeChild[] = []
        if (typeof props.title === 'string') children.push(props.title)
        const slot = ctx.slots.default?.()
        if (slot !== undefined) children.push(slot as VNodeChild)
        return h('div', { 'data-naive': 'modal' }, children)
      }
    },
  }
  return {
    NAlert: stub('alert'),
    NButton: stub('button'),
    NCard: stub('card'),
    NCheckbox: stub('checkbox'),
    NEmpty: stub('empty'),
    NInput: stub('input'),
    NInputNumber: stub('input-number'),
    NModal: modal,
    NProgress: stub('progress'),
    NResult: stub('result'),
    NSelect: stub('select'),
    NSpin: stub('spin'),
    NSwitch: stub('switch'),
    NTag: stub('tag'),
  }
})

function testRouter(): Router {
  return createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: '/', component: { render: () => null } },
      { path: '/storage', component: { render: () => null } },
      { path: '/storage/devices/:id', component: { render: () => null } },
    ],
  })
}

async function render(component: Component, props: Record<string, unknown> = {}): Promise<string> {
  const app = createSSRApp({ render: () => h(component, props) })
  app.use(testRouter())
  const html = await renderToString(app)
  // scoped 样式加的 data-v-* 只作用于 DOM，留着它会让断言变成测编译产物。
  return html.replace(/ data-v-[0-9a-z]+(?=[ >])/g, '')
}

function capacity(): StorageCapacity {
  return {
    total_bytes: '1000204886016',
    available_bytes: '400081954037',
    used_percent: 60,
    inode_total: '61000000',
    inode_available: '30000000',
    inode_used_percent: 50.8,
    collected_at: '2026-10-21T09:31:45Z',
  }
}

function volume(overrides: Partial<Volume> = {}): Volume {
  return {
    id: 'vol_0123456789abcdef0123456789abcdef',
    version: '3',
    display_name: '数据盘',
    slug: 'data',
    device_id: 'sdb1',
    filesystem_uuid: '6f1a2b3c-4d5e-6f70-8192-a3b4c5d6e7f8',
    filesystem_type: 'ext4',
    mount_path: '/srv/upanel/volumes/data',
    automount: true,
    read_only: false,
    nodev: false,
    nosuid: false,
    desired_state: 'mounted',
    observed_state: 'mounted',
    capacity: capacity(),
    dependencies: [],
    updated_at: '2026-10-21T09:31:45Z',
    ...overrides,
  }
}

function device(overrides: Partial<StorageDevice> = {}): StorageDevice {
  return {
    id: 'sdb1',
    kind: 'partition',
    name: 'sdb1',
    size_bytes: '107374182400',
    logical_sector_bytes: 512,
    physical_sector_bytes: 4096,
    model: 'TEST DISK',
    serial: 'SN-TEST',
    wwn: null,
    transport: 'sata',
    rotational: false,
    read_only: false,
    online: true,
    identity_confidence: 'high',
    identity_fingerprint: 'sha256:abcdef',
    protection: { system_disk: false, reasons: [], sources: [] },
    capabilities: [],
    partition_table: {
      type: 'gpt',
      source: 'device',
      disk_guid: '0F1A2B3C-4D5E-6F70-8192-A3B4C5D6E7F8',
      partitions: [],
      free_regions: [],
    },
    filesystem: { type: 'ext4', uuid: '6f1a2b3c-4d5e-6f70-8192-a3b4c5d6e7f8', label: 'data', mounts: [] },
    holders: [],
    health: {
      status: 'healthy',
      temperature_celsius: 34,
      source: 'smartctl',
      checked_at: '2026-10-21T09:31:45Z',
      details: 'SMART overall-health self-assessment test result: PASSED',
    },
    io: null,
    swap: null,
    collected_at: '2026-10-21T09:31:45Z',
    ...overrides,
  }
}

describe('VolageCard：状态说现场那一个', () => {
  it('挂载中的卷显示挂载点、UUID、选项与容量', async () => {
    const html = await render(VolumeCard, { volume: volume(), canAct: true })
    expect(html).toContain('数据盘')
    expect(html).toContain('/srv/upanel/volumes/data')
    expect(html).toContain('6f1a2b3c-4d5e-6f70-8192-a3b4c5d6e7f8')
    expect(html).toContain('nofail')
    expect(html).toContain('开机自动挂载')
    // 容量读到了才画条。
    expect(html).toContain('60')
  })

  it('期望已挂载但现场不在：不显示“已挂载”，而是警告 + 说明', async () => {
    const html = await render(VolumeCard, {
      volume: volume({ observed_state: 'missing', capacity: null }),
      canAct: true,
    })
    expect(html).toContain('不在这台机器上')
    expect(html).toContain('期望与实际不一致')
    // 关键：不能出现“已挂载”这个观察状态的标签（desired 是 mounted）。
    expect(html).not.toContain('>已挂载<')
  })

  it('容量没读到时明说“没有容量读数”，不画成 0 字节', async () => {
    const html = await render(VolumeCard, {
      volume: volume({ observed_state: 'unmounted', capacity: null }),
      canAct: true,
    })
    expect(html).toContain('没有容量读数')
    expect(html).not.toContain('0 B')
  })

  it('只读账号看不到任何写按钮，但看得到原因', async () => {
    const html = await render(VolumeCard, { volume: volume(), canAct: false })
    expect(html).toContain('只读账号')
    expect(html).not.toContain('data-action=')
    expect(html).not.toContain('看占用')
  })

  it('管理员看到与现场状态对应的那一个动作：挂着给卸载，没挂给挂载', async () => {
    const mounted = await render(VolumeCard, { volume: volume(), canAct: true })
    expect(mounted).toContain('data-action="storage.volume.unmount"')
    expect(mounted).not.toContain('data-action="storage.volume.mount"')
    expect(mounted).toContain('data-action="storage.volume.forget"')

    const unmounted = await render(VolumeCard, {
      volume: volume({ observed_state: 'unmounted', capacity: null }),
      canAct: true,
    })
    expect(unmounted).toContain('data-action="storage.volume.mount"')
    expect(unmounted).not.toContain('data-action="storage.volume.unmount"')
  })

  it('依赖关系照实列出来', async () => {
    const html = await render(VolumeCard, {
      volume: volume({ dependencies: [{ kind: 'share', id: 'shr_1', name: 'team-share' }] }),
      canAct: true,
    })
    expect(html).toContain('依赖它的东西')
    expect(html).toContain('team-share')
  })
})

describe('VolumeSection：加载中不等于没有卷', () => {
  it('首帧是“正在读取”，不是“还没有受管的卷”', async () => {
    const html = await render(VolumeSection, { devices: [], canAct: true })
    expect(html).toContain('正在读取受管卷')
    // 没拿到数据之前就把空状态画出来，等于编了一个结论。
    expect(html).not.toContain('还没有受管的卷')
  })

  it('计数在读取中写“读取中”，不写 0（0 是一个结论）', async () => {
    const html = await render(VolumeSection, { devices: [], canAct: true })
    expect(html).toContain('读取中')
    expect(html).not.toContain('0 个卷')
  })
})

describe('VolumeImportForm：候选、挂载名与只读', () => {
  it('候选为空时说明为什么空，而不是给一个空下拉框', async () => {
    const html = await render(VolumeImportForm, { devices: [device()], canAct: true })
    // device() 是已分区、有 ext4、未挂载的非系统盘 → 应当被算作候选，
    // 所以“候选为空”的说明不该出现（下拉框本身由 naive-ui 渲染选项，SSR 里是空壳）。
    expect(html).toContain('data-testid="import-device"')
    expect(html).not.toContain('import-no-candidates')
  })

  it('没有候选时（整盘/已挂载/系统盘）给出原因', async () => {
    const html = await render(VolumeImportForm, {
      devices: [device({ id: 'sdb', kind: 'disk', filesystem: null })],
      canAct: true,
    })
    expect(html).toContain('import-no-candidates')
    expect(html).toContain('候选为空')
  })

  it('未挂载所以读不到文件系统类型的分区仍然是候选，并说明类型由服务端直读确认', async () => {
    // 真机（P07.5 验收）上就是这个形状：分区建好、还没挂载，按 CONTRACTS 第 231 行
    // filesystem.type 为 null，甚至会整个 filesystem 为 null。
    // 如果把它们当“不合格”，导入表单永远是空的——这正是真机跑出来的缺陷。
    for (const filesystem of [
      null,
      { type: null, uuid: null, label: null, mounts: [] },
    ]) {
      const html = await render(VolumeImportForm, {
        devices: [device({ filesystem })],
        canAct: true,
      })
      expect(html).not.toContain('import-no-candidates')
      expect(html).toContain('import-unknown-type')
      expect(html).toContain('不调用 blkid')
    }
  })

  it('类型读得出但不在托管范围的分区不当候选', async () => {
    const html = await render(VolumeImportForm, {
      devices: [device({ filesystem: { type: 'ntfs', uuid: 'u', label: null, mounts: [] } })],
      canAct: true,
    })
    expect(html).toContain('import-no-candidates')
  })

  it('挂载点预览说明它由挂载名派生；没填时写“还不可用”', async () => {
    const html = await render(VolumeImportForm, { devices: [device()], canAct: true })
    expect(html).toContain('还不可用')
    expect(html).toContain('由挂载名派生')
    expect(html).toContain('data-testid="import-submit"')
    // 挂载名没填之前提交按钮是灰的。
    expect(html).toContain('data-disabled="true"')
  })

  it('只读账号看到一句解释，没有表单', async () => {
    const html = await render(VolumeImportForm, { devices: [device()], canAct: false })
    expect(html).toContain('volume-import-readonly')
    expect(html).not.toContain('import-submit')
  })
})

describe('DeviceStorageActions：按钮由 capabilities 决定', () => {
  // 一台“已经分好区”的整盘上，服务端会这样投影（internal/storage/capabilities.go）：
  //   初始化 = blocked（盘上已经有分区表）、新建 = available、
  //   删除 = unsupported（只有分区能删除）、格式化 = unsupported（整盘没有文件系统）。
  const diskCapabilities = [
    { action: 'storage.partition_table.initialize', state: 'blocked' as const, reason: '盘上已经有分区表' },
    { action: 'storage.partition.create', state: 'available' as const },
    { action: 'storage.partition.delete', state: 'unsupported' as const, reason: '只有分区能删除，当前目标是整盘' },
    { action: 'storage.filesystem.format', state: 'unsupported' as const, reason: '这是整盘，没有可格式化的文件系统' },
  ]

  it('unsupported 的动作连按钮都不画（整盘格式化、整盘删分区）', async () => {
    const html = await render(DeviceStorageActions, {
      device: device({ id: 'sdb', kind: 'disk', capabilities: diskCapabilities }),
      canAct: true,
    })
    expect(html).not.toContain('data-action="storage.filesystem.format"')
    expect(html).not.toContain('data-action="storage.partition.delete"')
    expect(html).not.toContain('现在不能格式化')
    expect(html).not.toContain('现在不能删除')
    // blocked 的那个仍然画出来（初始化分区表：盘上已经有分区表）。
    expect(html).toContain('data-action="storage.partition_table.initialize"')
    expect(html).toContain('现在不能初始化：盘上已经有分区表')
  })

  it('blocked 画出来但灰着，reason 照抄服务端的话', async () => {
    const html = await render(DeviceStorageActions, {
      device: device({
        capabilities: [
          { action: 'storage.partition.delete', state: 'blocked', reason: '分区正在被挂载' },
        ],
      }),
      canAct: true,
    })
    expect(html).toContain('data-action="storage.partition.delete"')
    expect(html).toContain('data-disabled="true"')
    expect(html).toContain('现在不能删除：分区正在被挂载')
  })

  it('available 的新建分区给出空闲区、MiB 输入与将请求的字节数', async () => {
    const html = await render(DeviceStorageActions, {
      device: device({
        id: 'sdb',
        kind: 'disk',
        partition_table: {
          type: 'gpt',
          source: 'device',
          disk_guid: '0F1A2B3C-4D5E-6F70-8192-A3B4C5D6E7F8',
          partitions: [],
          free_regions: [{ start_bytes: '1048576', size_bytes: '10736369664' }],
        },
        capabilities: [{ action: 'storage.partition.create', state: 'available' }],
      }),
      canAct: true,
    })
    expect(html).toContain('data-action="storage.partition.create"')
    expect(html).toContain('将请求：起点 1048576 字节，大小 10736369664 字节')
  })

  it('受管理卷在这台设备上时提醒先卸载并退出管理', async () => {
    const html = await render(DeviceStorageActions, {
      device: device({ capabilities: [{ action: 'storage.filesystem.format', state: 'available' }] }),
      volumeMountPaths: ['/srv/upanel/volumes/data'],
      canAct: true,
    })
    expect(html).toContain('/srv/upanel/volumes/data')
    expect(html).toContain('先到存储页把那个卷卸载并退出管理')
  })

  it('只读账号看不到四个动作，只看得到一句解释', async () => {
    const html = await render(DeviceStorageActions, {
      device: device({
        capabilities: [
          { action: 'storage.filesystem.format', state: 'available' },
          { action: 'storage.partition.delete', state: 'available' },
        ],
      }),
      canAct: false,
    })
    expect(html).toContain('device-actions-readonly')
    expect(html).not.toContain('data-action=')
  })

  it('整盘页面上给分区各自的入口，不提供“删某个分区”的按钮', async () => {
    const html = await render(DeviceStorageActions, {
      device: device({
        id: 'sdb',
        kind: 'disk',
        partition_table: {
          type: 'gpt',
          source: 'device',
          disk_guid: '0F1A2B3C-4D5E-6F70-8192-A3B4C5D6E7F8',
          partitions: [
            { device_id: 'sdb1', number: 1, start_bytes: '1048576', size_bytes: '10736369664' },
          ],
          free_regions: [],
        },
        capabilities: diskCapabilities,
      }),
      canAct: true,
    })
    expect(html).toContain('/storage/devices/sdb1')
    expect(html).toContain('这块盘上的分区')
    expect(html).not.toContain('data-action="storage.partition.delete"')
  })

  it('载荷里一个动作都没报告时收成一句“版本不一致”，不画四颗无意义的灰按钮', async () => {
    const html = await render(DeviceStorageActions, {
      device: device({ capabilities: [] }),
      canAct: true,
    })
    expect(html).toContain('device-actions-unreported')
    expect(html).toContain('代理与页面版本不一致')
    expect(html).not.toContain('data-action=')
  })
})

describe('DeviceDetailPanel：把服务端的判决原样摊开', () => {
  it('unsupported 的动作与理由出现在判决列表里，并说明它有 30 秒缓存', async () => {
    const html = await render(DeviceDetailPanel, {
      device: device({
        id: 'sdb',
        kind: 'disk',
        capabilities: [
          { action: 'storage.filesystem.format', state: 'unsupported', reason: '这是整盘，没有可格式化的文件系统' },
        ],
      }),
      edges: [],
      intervalMs: 5000,
      collectedAt: '2026-10-21T09:31:45Z',
      canAct: true,
    })
    expect(html).toContain('storage.filesystem.format（unsupported）')
    expect(html).toContain('这是整盘，没有可格式化的文件系统')
    expect(html).toContain('30 秒缓存')
  })

  it('载荷里没有 capabilities 时说清是版本不一致，不画成“没有动作”', async () => {
    const html = await render(DeviceDetailPanel, {
      device: device({ capabilities: [] }),
      edges: [],
      intervalMs: 5000,
      collectedAt: '2026-10-21T09:31:45Z',
      canAct: true,
    })
    expect(html).toContain('代理与页面的版本不一致')
  })
})

describe('会话不是管理员时整块界面都退到只读', () => {
  it('isAdmin 为假时 VolumeCard 与 DeviceStorageActions 都不给写入口', async () => {
    sessionState.isAdmin = false
    try {
      const card = await render(VolumeCard, { volume: volume(), canAct: sessionState.isAdmin })
      const actions = await render(DeviceStorageActions, {
        device: device({ capabilities: [{ action: 'storage.filesystem.format', state: 'available' }] }),
        canAct: sessionState.isAdmin,
      })
      expect(card).not.toContain('data-action=')
      expect(actions).not.toContain('data-action=')
    } finally {
      sessionState.isAdmin = true
    }
  })
})
