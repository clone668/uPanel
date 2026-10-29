// 共享界面的渲染用例（P08.4）。
//
// 环境是 node，没有 DOM，所以用 vue/server-renderer 渲染成字符串再断言：
// 验的是“这份数据最终显示成什么文字”（见 docs/DECISIONS.md Q21）。
//
// 这一层盯住四件最容易溜进界面的假象：
//   1. 把三个准备状态压成一句“状态良好”（config_hash / directory_prepared /
//      config_applied 答的不是同一个问题，CONTRACTS §7.1）；
//   2. 被运行时撤销的共享仍然显示成一个正常标签；
//   3. 只读账号看到能点的写入口；
//   4. 密码以任何形式出现在页面上（输入框除外）。
//
// 真机载荷回放在 realdata-p08-4.test.ts。

import { describe, expect, it, vi } from 'vitest'
import { createSSRApp, h, type Component, type VNodeChild } from 'vue'
import { renderToString } from 'vue/server-renderer'

import type { ShareAccount, ShareRecord } from '@/api/shares'
import ShareAccountPanel from './ShareAccountPanel.vue'
import ShareCard from './ShareCard.vue'
import ShareClientHelp from './ShareClientHelp.vue'
import ShareForm from './ShareForm.vue'
import { buildShareDefinition, validateShareForm, type ShareFormState } from './shares'

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
  }
})

async function render(component: Component, props: Record<string, unknown> = {}): Promise<string> {
  const app = createSSRApp({ render: () => h(component, props) })
  const html = await renderToString(app)
  return html.replace(/ data-v-[0-9a-z]+(?=[ >])/g, '')
}

function share(overrides: Partial<ShareRecord> = {}): ShareRecord {
  return {
    id: 'shr_0123456789abcdef0123456789abcdef',
    definition: {
      name: 'data',
      protocol: 'smb',
      volume_id: 'vol_0123456789abcdef0123456789abcdef',
      relative_path: 'team/docs',
      enabled: true,
      read_only: false,
      allowed_cidrs: ['192.168.1.0/24'],
      smb_users: [{ account_id: 'acc_write', access: 'write' }],
    },
    config_hash: 'abcdef0123456789'.repeat(4),
    version: '3',
    directory_prepared: true,
    config_applied: true,
    created_at: '2026-10-22T08:00:00Z',
    updated_at: '2026-10-22T08:05:00Z',
    ...overrides,
  }
}

/** 卷那一侧的现场，页面 join 之后传给卡片（共享记录里没有这些字段）。 */
const mountedVolume = {
  id: 'vol_0123456789abcdef0123456789abcdef',
  name: '数据卷',
  state: 'mounted',
}

function account(overrides: Partial<ShareAccount> = {}): ShareAccount {
  return {
    id: 'acc_write',
    name: 'teamdocs',
    type: 'share',
    managed: true,
    disabled: false,
    has_password: true,
    uid: 998,
    gid: 998,
    shell: '/usr/sbin/nologin',
    home: '/nonexistent',
    version: '2',
    created_at: '2026-10-22T08:00:00Z',
    updated_at: '2026-10-22T08:05:00Z',
    ...overrides,
  }
}

describe('一条共享画成什么', () => {
  it('三个准备状态分开显示，不合并成一句“正常”', async () => {
    const html = await render(ShareCard, {
      share: share(),
      accounts: [account()],
      volume: mountedVolume,
      canAct: true,
    })
    expect(html).toContain('abcdef0123456789')
    expect(html).toContain('目录与 ACL：已准备并核验')
    expect(html).toContain('配置：已写入并重载')
    expect(html).toContain('team/docs')
    expect(html).toContain('数据卷')
  })

  it('被运行时撤销时把现场写出来，并说明不会自动恢复', async () => {
    const html = await render(ShareCard, {
      share: share({
        runtime_revoked: {
          at: '2026-10-22T09:00:00Z',
          volume_id: 'vol_0123456789abcdef0123456789abcdef',
          volume_name: '数据卷',
          volume_state: 'unmounted',
          reason: '卷「数据卷」已不在挂载状态',
        },
      }),
      accounts: [account()],
      canAct: true,
    })
    expect(html).toContain('运行时已撤销')
    expect(html).toContain('不会自动恢复')
    expect(html).toContain('unmounted')
  })

  it('只读账号看不到写入口，但能看到原因', async () => {
    const html = await render(ShareCard, { share: share(), accounts: [account()], canAct: false })
    expect(html).not.toContain('data-testid="share-apply"')
    expect(html).not.toContain('data-testid="share-delete"')
    expect(html).toContain('要求管理员')
  })

  it('账号名单显示成“名字 · 访问级别”，不是裸 id', async () => {
    const html = await render(ShareCard, { share: share(), accounts: [account()], canAct: true })
    expect(html).toContain('teamdocs · 读写')
    expect(html).not.toContain('acc_write ·')
  })

  it('没挂卷的时候标签直接说连不上', async () => {
    const html = await render(ShareCard, {
      share: share(),
      accounts: [account()],
      volume: { ...mountedVolume, state: 'unmounted' },
      canAct: true,
    })
    expect(html).toContain('卷此刻没挂')
    expect(html).toContain('客户端此刻连不上')
  })

  it('卷列表还没回来时不画“卷没挂”，只少一行观测值', async () => {
    const html = await render(ShareCard, { share: share(), accounts: [account()], canAct: true })
    expect(html).not.toContain('卷此刻没挂')
    expect(html).not.toContain('观测：')
    // 卷名取不到就退回 volume_id，不留空。
    expect(html).toContain('vol_0123456789abcdef0123456789abcdef')
  })
})

describe('客户端说明', () => {
  it('关着的时候不渲染任何命令', async () => {
    const html = await render(ShareClientHelp, {
      show: false,
      share: share(),
      host: 'panel.example',
      capability: null,
    })
    expect(html).not.toContain('mount.cifs')
    expect(html).not.toContain('credentials=')
  })

  it('打开时四种客户端都在，且没有一条命令带密码值', async () => {
    const html = await render(ShareClientHelp, {
      show: true,
      share: share(),
      host: 'panel.example',
      capability: null,
    })
    expect(html).toContain('Windows 资源管理器')
    expect(html).toContain('macOS Finder')
    expect(html).toContain('mount.cifs')
    expect(html).toContain('smbclient //panel.example/data')
    expect(html).toContain('credentials=/root/.smb/data.cred')
    expect(html).toContain('密码不进 argv')
    expect(html).toContain('127.0.0.1')
  })

  it('能力不可用时先说清楚连不上', async () => {
    const html = await render(ShareClientHelp, {
      show: true,
      share: share(),
      host: 'panel.example',
      capability: {
        status: 'blocked',
        reason: '没找到 smbd',
        samba_version: '',
        tools: {},
        config_path: '/etc/samba/smb.conf',
        config_readable: false,
        include_dir: '/etc/samba/upanel.d',
        include_ready: false,
        include_present: false,
        config_write: 'unsupported',
        service_state: 'inactive',
        service_units: [],
        managed_config: '',
        managed_config_present: false,
        managed_config_hash: '',
        managed_shares: [],
        collected_at: '2026-10-22T08:00:00Z',
      },
    })
    expect(html).toContain('Samba 能力此刻不可用')
    expect(html).toContain('没找到 smbd')
  })
})

describe('新建共享的表单', () => {
  const volumes = [
    {
      id: 'vol_0123456789abcdef0123456789abcdef',
      version: '1',
      display_name: '数据卷',
      slug: 'data',
      device_id: 'sdb1',
      filesystem_uuid: '6f1a2b3c-4d5e-6f70-8192-a3b4c5d6e7f8',
      filesystem_type: 'ext4',
      mount_path: '/srv/upanel/volumes/data',
      automount: true,
      read_only: false,
      nodev: false,
      nosuid: false,
      desired_state: 'mounted' as const,
      observed_state: 'mounted' as const,
      capacity: null,
      dependencies: [],
      updated_at: '2026-10-22T08:00:00Z',
    },
  ]

  it('空表单一提交就被挡住，把每一条原因都说出来', async () => {
    const html = await render(ShareForm, { accounts: [account()], volumes, busy: false })
    // 未点提交前不吓人
    expect(html).not.toContain('还不能提交')
    // 按钮从一开始就是灰的（rules 里 errors 非空）
    expect(html).toContain('data-testid="share-submit"')
  })

  it('没有可用账号时说清楚先去建账号，而不是给一个空下拉', async () => {
    const html = await render(ShareForm, { accounts: [], volumes, busy: false })
    expect(html).toContain('还没有可用的共享账号')
  })

  it('登记时就把共享名与路径的规矩写在字段旁边（别让人撞墙才知道）', async () => {
    const html = await render(ShareForm, { accounts: [account()], volumes, busy: false })
    expect(html).toContain('字母、数字、下划线、连字符')
    expect(html).toContain('建好之后不能改名')
    expect(html).toContain('留空就是卷根')
    expect(html).toContain('SHARE_NO_USERS')
  })
})

describe('受管理账号面板', () => {
  it('没有密码字段被填上，且密码输入框是 password 类型', async () => {
    const html = await render(ShareAccountPanel, {
      accounts: [account()],
      referencedIds: [],
      canAct: true,
    })
    // 表单默认收起：页面上没有密码输入框
    expect(html).not.toContain('data-type="password"')
    expect(html).toContain('新建账号')
    expect(html).toContain('/usr/sbin/nologin')
    expect(html).toContain('uid 998')
  })

  it('被共享引用的账号不能删，并说明原因', async () => {
    const html = await render(ShareAccountPanel, {
      accounts: [account()],
      referencedIds: ['acc_write'],
      canAct: true,
    })
    expect(html).toContain('被共享引用')
    expect(html).toContain('先从共享名单里摘掉')
  })

  it('只读账号没有新建入口', async () => {
    const html = await render(ShareAccountPanel, {
      accounts: [account()],
      referencedIds: [],
      canAct: false,
    })
    expect(html).not.toContain('data-testid="account-new"')
  })
})

// 校验是纯函数，直接测规则本身：SSR 里点不到，但错误文案必须与服务端同一套说法。
describe('表单校验（与 internal/shares/definition.go 同一套规则）', () => {
  const base: ShareFormState = {
    name: 'data',
    protocol: 'smb',
    volume_id: 'vol_0123456789abcdef0123456789abcdef',
    relative_path: 'team/docs',
    read_only: false,
    allowed_cidrs: '192.168.1.0/24 10.0.0.0/8',
    access: { acc_write: 'write' },
  }

  it('合法输入没有错误', () => {
    expect(validateShareForm(base)).toEqual([])
  })

  it('保留段名被拒（那是往 global 里写键值，不是建共享）', () => {
    for (const name of ['global', 'HOMES', 'printers', 'print$', 'ipc$']) {
      expect(validateShareForm({ ...base, name }).join()).toContain('保留段名')
    }
  })

  it('名字字符集与首尾规则', () => {
    expect(validateShareForm({ ...base, name: '-data' }).join()).toContain('字母或数字开头')
    expect(validateShareForm({ ...base, name: 'data-' }).join()).toContain('连字符结尾')
    expect(validateShareForm({ ...base, name: 'data share' }).join()).toContain('只能包含')
    expect(validateShareForm({ ...base, name: 'a'.repeat(65) }).join()).toContain('过长')
    expect(validateShareForm({ ...base, name: 'Data_1-2' })).toEqual([])
  })

  it('相对路径：不以 / 开头、无反斜杠、没有空段与 . 段', () => {
    expect(validateShareForm({ ...base, relative_path: '/abs' }).join()).toContain('不能以 / 开头')
    expect(validateShareForm({ ...base, relative_path: 'a\\b' }).join()).toContain('反斜杠')
    expect(validateShareForm({ ...base, relative_path: 'a//b' }).join()).toContain('多余的 /')
    expect(validateShareForm({ ...base, relative_path: 'a/../b' }).join()).toContain('.. 段')
    // 空串是合法的：它表示“整个卷根”
    expect(validateShareForm({ ...base, relative_path: '' })).toEqual([])
  })

  it('空账号名单是硬拒绝，不是“稍后再填”', () => {
    expect(validateShareForm({ ...base, access: {} }).join()).toContain('SHARE_NO_USERS')
    expect(validateShareForm({ ...base, access: { acc_write: '' } }).join()).toContain('SHARE_NO_USERS')
  })

  it('CIDR 必须是地址/位数，且不吞掉多打的空格', () => {
    expect(validateShareForm({ ...base, allowed_cidrs: '192.168.1.0' }).join()).toContain('不像 CIDR')
    expect(validateShareForm({ ...base, allowed_cidrs: ' 192.168.1.0/24 ,  10.0.0.0/8 ' })).toEqual([])
    expect(validateShareForm({ ...base, allowed_cidrs: '' })).toEqual([])
  })

  it('多个问题一次说完，不是只报第一个', () => {
    const errors = validateShareForm({
      name: '',
      protocol: 'smb',
      volume_id: '',
      relative_path: '/x',
      read_only: false,
      allowed_cidrs: 'nope',
      access: {},
    })
    expect(errors.length).toBeGreaterThanOrEqual(5)
  })

  it('构造出的定义形状与契约一致', () => {
    const def = buildShareDefinition({ ...base, relative_path: '  team/docs  ' })
    expect(def).toEqual({
      name: 'data',
      protocol: 'smb',
      volume_id: 'vol_0123456789abcdef0123456789abcdef',
      relative_path: 'team/docs',
      enabled: true,
      read_only: false,
      allowed_cidrs: ['192.168.1.0/24', '10.0.0.0/8'],
      smb_users: [{ account_id: 'acc_write', access: 'write' }],
    })
  })
})

// ---------------------------------------------------------------------------
// NFS 导出（P14.4）。
//
// 这一层只盯“说错话会让人做错事”的地方：
//   1. NFS 不点名网段就是向全世界导出——表单必须先挡住，不能等预检；
//   2. `/0` 在 SMB 上只是“不限来源”，在 NFS 上是“向全世界导出”，两条不能合并；
//   3. 切到 NFS 之后，SMB 那半边的字段（账号名单、ACL、Samba 配置）不能再出现在页面上，
//      也不能被悄悄塞进提交的定义里。
// ---------------------------------------------------------------------------

/** NFS 的表单初始状态：只多一条 protocol，其余字段与 SMB 共用同一份表单结构。 */
function nfsForm(overrides: Partial<ShareFormState> = {}): ShareFormState {
  return {
    name: 'media',
    protocol: 'nfs',
    volume_id: 'vol_0123456789abcdef0123456789abcdef',
    relative_path: 'team/docs',
    read_only: false,
    allowed_cidrs: '10.0.2.0/24',
    access: {},
    ...overrides,
  }
}

describe('NFS 表单校验（与 internal/shares/nfs.go 同一套规则）', () => {
  it('合法输入没有错误，且不需要任何账号', () => {
    expect(validateShareForm(nfsForm())).toEqual([])
  })

  it('不点名网段是硬拒绝（不填等于向全世界导出）', () => {
    expect(validateShareForm(nfsForm({ allowed_cidrs: '' })).join()).toContain('SHARE_NO_CIDRS')
    expect(validateShareForm(nfsForm({ allowed_cidrs: '   ' })).join()).toContain('SHARE_NO_CIDRS')
  })

  it('/0 在 NFS 上被拒，在 SMB 上不是错误（两条规则不能合并）', () => {
    expect(validateShareForm(nfsForm({ allowed_cidrs: '0.0.0.0/0' })).join()).toContain('NFS_OPEN_TO_WORLD')
    expect(validateShareForm(nfsForm({ allowed_cidrs: '10.0.2.0/24 0.0.0.0/0' })).join()).toContain('NFS_OPEN_TO_WORLD')
    expect(
      validateShareForm({
        name: 'data',
        protocol: 'smb',
        volume_id: 'vol_0123456789abcdef0123456789abcdef',
        relative_path: '',
        read_only: false,
        allowed_cidrs: '0.0.0.0/0',
        access: { acc_write: 'write' },
      }),
    ).toEqual([])
  })

  it('构造出的 NFS 定义里没有账号名单，选项是固定的两个 true', () => {
    const def = buildShareDefinition(nfsForm({ access: { acc_write: 'write' } }))
    expect(def.protocol).toBe('nfs')
    expect(def.smb_users).toEqual([])
    expect(def.nfs_options).toEqual({ root_squash: true, sync: true })
    expect(def.allowed_cidrs).toEqual(['10.0.2.0/24'])
  })

  it('SMB 定义里不出现 nfs_options（出现即被判决层拒）', () => {
    const def = buildShareDefinition({
      name: 'data',
      protocol: 'smb',
      volume_id: 'vol_0123456789abcdef0123456789abcdef',
      relative_path: '',
      read_only: false,
      allowed_cidrs: '',
      access: { acc_write: 'read' },
    })
    expect(def).not.toHaveProperty('nfs_options')
    expect(def.smb_users).toEqual([{ account_id: 'acc_write', access: 'read' }])
  })
})

describe('NFS 表单在页面上说了什么', () => {
  const volumes = [
    {
      id: 'vol_0123456789abcdef0123456789abcdef',
      version: '1',
      display_name: '数据卷',
      slug: 'data',
      device_id: 'sdb1',
      filesystem_uuid: '6f1a2b3c-4d5e-6f70-8192-a3b4c5d6e7f8',
      filesystem_type: 'ext4',
      mount_path: '/srv/upanel/volumes/data',
      automount: true,
      read_only: false,
      nodev: false,
      nosuid: false,
      desired_state: 'mounted' as const,
      observed_state: 'mounted' as const,
      capacity: null,
      dependencies: [],
      updated_at: '2026-10-22T08:00:00Z',
    },
  ]

  it('NFS 分支里没有账号块，也没有 Samba 的说法', async () => {
    const html = await render(ShareForm, {
      accounts: [account()],
      volumes,
      busy: false,
      initialProtocol: 'nfs',
    })
    expect(html).toContain('data-testid="share-protocol"')
    expect(html).toContain('NFS 导出：访问控制是')
    expect(html).toContain('/etc/exports.d/upanel.exports')
    expect(html).toContain('导出选项')
    expect(html).toContain('root_squash')
    // 账号那一块是 v-if 掉的：连它的错误说法都不该出现
    expect(html).not.toContain('授权账号')
    expect(html).not.toContain('SHARE_NO_USERS')
    // 目录权限的两种说法跟着 read_only 走
    expect(html).toContain('0755')
    expect(html).toContain('0777')
  })

  it('SMB 分支默认就是原来那套，不会冒出 NFS 的选项', async () => {
    const html = await render(ShareForm, { accounts: [account()], volumes, busy: false })
    expect(html).toContain('授权账号')
    expect(html).toContain('SHARE_NO_USERS')
    expect(html).not.toContain('导出选项')
    expect(html).not.toContain('upanel.exports')
  })
})
