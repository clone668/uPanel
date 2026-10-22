// 用真实 Ubuntu 数据回放“共享页面与客户端连接说明”（P08.4）。
//
// 这些 JSON 不是造的：验收 VM（upanel-2404）上跑完
//   GPT → 建分区 → mkfs ext4 → 纳入管理 → 挂载 → 三个受管理账号（rw/ro/名单外）
//   → 一条共享（rw+ro 两个授权）→ 逐字执行页面给的 smbclient / mount.cifs 命令
//   → 从外面 umount 复现“卷掉线” → 重新挂回 → 重新应用
// 之后，由 tests/integration/p08-4-share-ui-client-acceptance.sh 原样抓回来，
// 存在 src/fixtures/p08-4-vm/ 下（见那里的 README）。
//
// 为什么非要真实载荷：手写夹具天然自洽。这一层锁的正是“自洽的假象”露头的那次——
// 前端 DTO 里曾经写着 `volume_state`、`volume_name`、`startup_deps`、`startup_paths`，
// 而 openapi 的 ShareRecord **一个都没有**（卷的状态属于卷，开机依赖属于那次操作的结果）。
// 手写夹具带着这些字段，所以单元测试全绿；真机载荷里它们根本不存在，页面上的“卷没挂”
// 永远不会亮（DECISIONS Q246）。这里的断言就是那条回归锁：记录必须长得跟契约一模一样。
//
// 断言的都是不变量：字段闭集、闭集成员的取值、空值语义、页面 join 出来的东西。
// 不写死 id、哈希、账号名以外的时间戳——那是某一次运行的样子。

import { readFileSync } from 'node:fs'

import { describe, expect, it, vi } from 'vitest'
import { createSSRApp, h, type Component, type VNodeChild } from 'vue'
import { renderToString } from 'vue/server-renderer'

import {
  clientHints,
  clientHintsUsable,
  summarizeShareState,
  SHARE_ACCESS_LEVELS,
  type ShareAccount,
  type ShareRecord,
  type ShareVolumeInfo,
  type ShareConnections,
} from '@/api/shares'
import { summarizeShareJob } from '@/utils/jobresult'
import ShareAccountPanel from './ShareAccountPanel.vue'
import ShareCard from './ShareCard.vue'

// naive-ui 在 SSR 里要挂样式（需要 document）。换成最小实现：文字、disabled 与 type 带出来。
vi.mock('naive-ui', async () => {
  const { h } = await import('vue')
  const stub = (name: string) => ({
    name: `Stub${name}`,
    props: [
      'title', 'description', 'status', 'code', 'disabled', 'value', 'options', 'checked',
      'type', 'size', 'loading', 'placeholder', 'maxlength', 'bordered', 'secondary',
      'autosize', 'showPasswordOn',
    ],
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
        return h('div', attrs, children)
      }
    },
  })
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
    NCollapse: stub('collapse'),
    NCollapseItem: stub('collapse-item'),
    NInput: stub('input'),
    NModal: modal,
    NSelect: stub('select'),
    NSwitch: stub('switch'),
    NTag: stub('tag'),
    NTooltip: stub('tooltip'),
    useMessage: () => ({ success: () => {}, error: () => {}, warning: () => {}, info: () => {} }),
  }
})

function rawText(name: string): string {
  return readFileSync(new URL(`../fixtures/p08-4-vm/${name}`, import.meta.url), 'utf8')
}

interface Envelope<T> {
  data: T
  request_id: string
}

function payload<T>(name: string): T {
  return (JSON.parse(rawText(name)) as Envelope<T>).data
}

/** 夹具里的第一条；没有就当场炸掉（不回一个 undefined 让下面的断言在错的地方通过）。 */
function first<T>(list: readonly T[], what: string): T {
  const value = list[0]
  if (value === undefined) throw new Error(`夹具里没有${what}`)
  return value
}

async function render(component: Component, props: Record<string, unknown>): Promise<string> {
  const app = createSSRApp({ render: () => h(component, props) })
  return renderToString(app)
}

// ---------------------------------------------------------------- 现场

const shares = payload<{ capability: { status: string; config_write: string }; shares: ShareRecord[] }>(
  'shares.json',
)
const accounts = payload<{ accounts: ShareAccount[] }>('accounts.json')
const volumes = payload<{ volumes: { id: string; display_name: string; observed_state: string }[] }>(
  'volumes.json',
)
const connections = payload<ShareConnections>('connections.json')

const share = first(shares.shares, '共享记录')
/** 页面就是这样把卷那一侧的事实 join 过来的：按 definition.volume_id 对。 */
function joinVolume(record: ShareRecord): ShareVolumeInfo | null {
  const found = volumes.volumes.find((v) => v.id === record.definition.volume_id)
  if (!found) return null
  return { id: found.id, name: found.display_name, state: found.observed_state }
}
const volume = joinVolume(share)

describe('真机载荷：共享记录必须长得跟契约一模一样', () => {
  it('列表里有这一条，能力报告与记录在同一份响应里', () => {
    expect(shares.capability.status).toBe('available')
    expect(shares.capability.config_write).toBe('available')
    expect(shares.shares.length).toBeGreaterThan(0)
    expect(share.definition.name).not.toBe('')
  })

  it('记录的字段全在 openapi ShareRecord 的闭集里（多一个都算编）', () => {
    const allowed = new Set([
      'id',
      'definition',
      'config_hash',
      'version',
      'directory_prepared',
      'config_applied',
      'runtime_revoked',
      'last_operation_id',
      'created_at',
      'updated_at',
    ])
    const extra = Object.keys(share).filter((k) => !allowed.has(k))
    expect(extra).toEqual([])
  })

  it('卷状态与开机依赖不在共享记录里（它们是外部事实与操作结果）', () => {
    // 这条是 Q246 的回归锁：DTO 里写着它们的时候，页面上的“卷没挂”永远不会亮。
    expect(share).not.toHaveProperty('volume_state')
    expect(share).not.toHaveProperty('volume_name')
    expect(share).not.toHaveProperty('startup_deps')
    expect(share).not.toHaveProperty('startup_paths')
  })

  it('三个准备状态各自是各自的样子，谁也不代表谁', () => {
    expect(share.config_hash).toMatch(/^[0-9a-f]{64}$/)
    expect(typeof share.directory_prepared).toBe('boolean')
    expect(typeof share.config_applied).toBe('boolean')
    expect(share.directory_prepared).toBe(true)
    expect(share.config_applied).toBe(true)
    expect(share.runtime_revoked ?? null).toBeNull()
  })

  it('账号名单是“账号 id + 访问级别”，访问级别在闭集里', () => {
    expect(share.definition.smb_users.length).toBe(2)
    for (const user of share.definition.smb_users) {
      expect(user.account_id).not.toBe('')
      expect(SHARE_ACCESS_LEVELS).toContain(user.access)
    }
    expect(share.definition.smb_users.map((u) => u.access).sort()).toEqual(['read', 'write'])
  })

  it('正文里的 relative_path 是空串（整卷根），页面显示成 “.”', () => {
    expect(share.definition.relative_path).toBe('')
  })
})

describe('真机载荷：卷那一侧 join 上来之后状态才对', () => {
  it('join 得到这块卷，观测值是 mounted', () => {
    expect(volume).not.toBeNull()
    expect(volume?.state).toBe('mounted')
    expect(volume?.name).not.toBe('')
  })

  it('join 之后是“已写入配置”，而不是“卷没挂”', () => {
    const state = summarizeShareState(share, volume)
    expect(state.kind).toBe('serving')
    expect(state.label).toBe('已写入配置')
  })

  it('join 不到卷时不猜成离线', () => {
    // 卷列表没回来、或卷已退出管理：都只能说“不知道”，不能说“没挂”。
    expect(summarizeShareState(share, null).kind).toBe('serving')
  })

  it('说明在这条上是可用的', () => {
    expect(clientHintsUsable(share, volume)).toBe(true)
  })
})

describe('真机载荷：页面画出来的东西', () => {
  it('三个状态分别成行，卷的观测值来自 join', async () => {
    const html = await render(ShareCard, {
      share,
      accounts: accounts.accounts,
      volume,
      canAct: true,
    })
    expect(html).toContain('已写入配置')
    expect(html).toContain(share.config_hash.slice(0, 16))
    expect(html).toContain('目录与 ACL：已准备并核验')
    expect(html).toContain('配置：已写入并重载')
    expect(html).toContain('观测：mounted')
    expect(html).toContain(volume?.name ?? '')
    expect(html).not.toContain('客户端此刻连不上')
    expect(html).not.toContain('卷此刻没挂')
  })

  it('账号名与访问级别都翻成了中文，不显示裸 id', async () => {
    const html = await render(ShareCard, {
      share,
      accounts: accounts.accounts,
      volume,
      canAct: true,
    })
    for (const user of share.definition.smb_users) {
      const name = accounts.accounts.find((a) => a.id === user.account_id)?.name ?? ''
      expect(name).not.toBe('')
      expect(html).toContain(name)
    }
    expect(html).toContain('只读')
    expect(html).toContain('读写')
    expect(html).not.toContain('acc_')
  })

  it('只读账号看到的是一句说明，不是能点的按钮', async () => {
    const html = await render(ShareCard, {
      share,
      accounts: accounts.accounts,
      volume,
      canAct: false,
    })
    expect(html).toContain('要求管理员')
    expect(html).not.toContain('share-apply')
  })
})

describe('真机载荷：卷掉线那一份', () => {
  const offline = payload<{ shares: ShareRecord[] }>('shares-offline.json')
  const offlineShare = first(offline.shares, '掉线的共享记录')

  it('撤销现场在记录里，五个字段齐全', () => {
    const revoked = offlineShare.runtime_revoked
    expect(revoked).toBeTruthy()
    expect(revoked?.at).not.toBe('')
    expect(revoked?.volume_id).toBe(offlineShare.definition.volume_id)
    expect(revoked?.volume_name).not.toBe('')
    expect(revoked?.volume_state).toBe('unmounted')
    expect(revoked?.reason).not.toBe('')
  })

  it('撤销之后 config_applied 就是假的（段真的不在配置里了）', () => {
    expect(offlineShare.config_applied).toBe(false)
  })

  it('撤销状态优先于一切，并且说明“要重新应用”', () => {
    const state = summarizeShareState(offlineShare, { id: 'vol_x', name: '数据卷', state: 'mounted' })
    expect(state.kind).toBe('revoked')
    expect(state.detail).toContain('重新应用')
    expect(clientHintsUsable(offlineShare, null)).toBe(false)
  })

  it('画出来带撤销现场与“不会自动恢复”', async () => {
    const html = await render(ShareCard, {
      share: offlineShare,
      accounts: accounts.accounts,
      volume: null,
      canAct: true,
    })
    expect(html).toContain('data-testid="share-revoked"')
    expect(html).toContain('不会自动恢复')
    expect(html).toContain('客户端此刻连不上')
  })

  it('卷挂回来之后撤销现场还在（只撤不恢复）', () => {
    const restored = payload<{ shares: ShareRecord[] }>('shares-restored.json')
    const restoredShare = first(restored.shares, '恢复后的共享记录')
    expect(restoredShare.runtime_revoked).toBeTruthy()
    expect(clientHintsUsable(restoredShare, { id: 'vol_x', name: '数据卷', state: 'mounted' })).toBe(
      false,
    )
  })
})

describe('真机载荷：账号与连接状态', () => {
  it('账号载荷没有密码、哈希与引用（has_password 是布尔，允许）', () => {
    expect(accounts.accounts.length).toBe(3)
    for (const a of accounts.accounts) {
      const suspicious = Object.keys(a).filter(
        (k) => k !== 'has_password' && /passw|hash|secret/i.test(k),
      )
      expect(suspicious).toEqual([])
      expect(typeof a.has_password).toBe('boolean')
      expect(a.managed).toBe(true)
    }
  })

  it('账号列表画得出名字与 uid，画不出任何秘密', async () => {
    const referencedIds = shares.shares.flatMap((s2) =>
      s2.definition.smb_users.map((u) => u.account_id),
    )
    const html = await render(ShareAccountPanel, {
      accounts: accounts.accounts,
      referencedIds,
      canAct: true,
    })
    for (const a of accounts.accounts) expect(html).toContain(a.name)
    expect(html).not.toContain('sec_')
    expect(html).not.toContain('password=')
  })

  it('连接状态把 service 与列表分开给（空列表不等于服务停了）', () => {
    expect(['active', 'inactive', 'unknown']).toContain(connections.service)
    expect(Array.isArray(connections.connections)).toBe(true)
    expect(connections.share_name.toLowerCase()).toBe(share.definition.name.toLowerCase())
  })
})

describe('真机载荷：说明里的命令与真的跑过的命令是同一条', () => {
  const hints = clientHints(share, '127.0.0.1')
  const mountSteps = rawText('client-mount-steps.txt')
  const smbRun = rawText('client-commands-run.txt')

  it('挂载那一行逐字出现在真机执行过的步骤里（没有占位符残留）', () => {
    const mount = hints.find((h) => h.kind === 'linux-mount')
    const line = mount?.commands.find((c) => c.includes('mount -t cifs'))
    expect(line).toBeTruthy()
    expect(mountSteps).toContain(line as string)
    expect(line).not.toContain('<')
  })

  it('凭据文件用 here-doc 从标准输入写，不在 argv 里放密码', () => {
    const mount = hints.find((h) => h.kind === 'linux-mount')
    const heredoc = mount?.commands.find((c) => c.includes('<<'))
    expect(heredoc).toBeTruthy()
    expect(mountSteps).toContain(heredoc as string)
    // 反面：除了凭据文件正文里那行占位符，任何一条命令里都不该出现 password=；
    // 也不许有 printf 把密码写进 argv（Q245）。
    for (const h of hints) {
      for (const c of h.commands) {
        if (c.includes('password=')) expect(c).toBe('password=<密码>')
        expect(c).not.toContain('printf')
      }
    }
  })

  it('smbclient 那一行在真机上跑过（只把账号占位符换成了真账号）', () => {
    const cli = hints.find((h) => h.kind === 'linux-smbclient')
    const line = cli?.commands[0] ?? ''
    expect(line).toContain('smbclient //127.0.0.1/')
    expect(line).toContain('-m SMB3')
    const runAccount = first(accounts.accounts, '受管理账号').name
    expect(runAccount).not.toBe('')
    expect(smbRun).toContain(`smbclient //127.0.0.1/${share.definition.name}`)
    expect(smbRun).toContain('-m SMB3')
  })

  it('四种客户端各给一条，Windows/macOS 只给地址不给命令', () => {
    expect(hints.map((h) => h.kind).sort()).toEqual(
      ['linux-mount', 'linux-smbclient', 'macos', 'windows'].sort(),
    )
    for (const kind of ['windows', 'macos'] as const) {
      const hint = hints.find((h) => h.kind === kind)
      expect(hint?.commands).toEqual([])
      expect(hint?.address).toContain('127.0.0.1')
    }
  })
})

describe('真机载荷：任务结果里的开机依赖', () => {
  const job = (JSON.parse(rawText('job-apply.json')) as Envelope<Parameters<typeof summarizeShareJob>[0]>)
    .data

  it('开机依赖是操作结果字段，落在 result.data 里', () => {
    const data = job.result['data'] as Record<string, unknown>
    expect(['written', 'unchanged', 'removed', 'failed', 'skipped_no_service']).toContain(
      data['startup_deps'],
    )
    expect(Array.isArray(data['startup_paths'])).toBe(true)
  })

  it('任务结果翻成人话时提到了配置与重载', () => {
    const summary = summarizeShareJob(job)
    expect(summary.ok).toBe(true)
    const text = summary.lines.join('\n')
    expect(text).toContain('托管配置')
    expect(text.toLowerCase()).toContain('samba')
  })
})
