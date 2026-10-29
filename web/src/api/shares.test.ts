// 共享 API 层：状态翻译、客户端说明、通知码（P08.4）。
//
// 这一层不碰网络，只测“给定服务端载荷，页面会说什么”。三条底线在这里是可断言的：
//   1. **三个状态字段各答一个问题**：config_hash / directory_prepared / config_applied
//      必须分开表达，不能被合成一句“状态良好”（CONTRACTS §7.1）；
//   2. **命令里没有密码**：`password=` 只能出现在 here-doc 的正文行里（那行走标准输入），
//      挂载命令行上只有 `credentials=`；
//   3. **说明不等于“可用”**：卷掉线、配置没写、被运行时撤销时，地址照给，第一条注意
//      必须写明此刻连不上。
//
// 文件末尾那一段是**与真机脚本互相钉住**的命令文本：`tests/integration/
// p08-4-share-ui-client-acceptance.sh` 在 VM 上逐条执行的是同一批字符串（只把主机名
// 换成 127.0.0.1、账号/密码换成夹具的值）。改一边不改另一边，这里会红。

import { describe, expect, it } from 'vitest'

import {
  capabilityAvailable,
  clientHints,
  clientHintsUsable,
  nfsCapabilityAvailable,
  shareNoticeLabel,
  summarizeShareState,
  SHARE_ACCESS_LABELS,
  SHARE_NOTICE_LABELS,
  type ShareCapability,
  type ShareRecord,
} from './shares'

function record(overrides: Partial<ShareRecord> = {}): ShareRecord {
  const base: ShareRecord = {
    id: 'shr_0123456789abcdef0123456789abcdef',
    definition: {
      name: 'data',
      protocol: 'smb',
      volume_id: 'vol_0123456789abcdef0123456789abcdef',
      relative_path: 'team/docs',
      enabled: true,
      read_only: false,
      allowed_cidrs: ['192.168.1.0/24'],
      smb_users: [
        { account_id: 'acc_write', access: 'write' },
        { account_id: 'acc_read', access: 'read' },
      ],
    },
    config_hash: 'a'.repeat(64),
    version: '7',
    directory_prepared: true,
    config_applied: true,
    created_at: '2026-10-22T08:00:00Z',
    updated_at: '2026-10-22T08:10:00Z',
  }
  return { ...base, ...overrides }
}

function capability(overrides: Partial<ShareCapability> = {}): ShareCapability {
  return {
    status: 'available',
    samba_version: '4.19.5',
    tools: {},
    config_path: '/etc/samba/smb.conf',
    config_readable: true,
    include_dir: '/etc/samba/upanel.d',
    include_ready: true,
    include_present: true,
    config_write: 'available',
    service_state: 'active',
    service_units: ['smbd.service', 'smbd.socket'],
    managed_config: '/etc/samba/upanel.d/upanel.conf',
    managed_config_present: true,
    managed_config_hash: 'b'.repeat(64),
    managed_shares: ['data'],
    nfs: {
      status: 'blocked',
      reason: '这台机器上没有 exportfs',
      exports_dir: '/etc/exports.d',
      exports_file: '/etc/exports.d/upanel.exports',
      exports_dir_ready: false,
      exports_present: false,
      exported_paths: [],
      main_exports_readable: true,
      service_state: 'unknown',
      config_write: 'unsupported',
      notices: [],
    },
    collected_at: '2026-10-22T08:00:00Z',
    ...overrides,
  }
}

describe('共享此刻处于哪种状态', () => {
  it('正在提供服务', () => {
    const state = summarizeShareState(record())
    expect(state.kind).toBe('serving')
    expect(state.label).toBe('已写入配置')
    // “已写入配置”不等于“客户端连得上”：这句话必须留在这里，别被人改成“正常运行”。
    expect(state.detail).toContain('smbd')
  })

  it('运行时撤销优先于一切：卷回来之后也不会自己恢复', () => {
    const state = summarizeShareState(
      record({
        runtime_revoked: {
          at: '2026-10-22T09:00:00Z',
          volume_id: 'vol_0123456789abcdef0123456789abcdef',
          volume_name: '数据卷',
          volume_state: 'unmounted',
          reason: '卷「数据卷」已不在挂载状态',
        },
      }),
    )
    expect(state.kind).toBe('revoked')
    expect(state.detail).toContain('重新应用')
  })

  it('卷没挂时不说“已写入配置”', () => {
    const state = summarizeShareState(record(), {
      id: 'vol_0123456789abcdef0123456789abcdef',
      name: '数据卷',
      state: 'unmounted',
    })
    expect(state.kind).toBe('volume_offline')
    expect(state.tone).toBe('error')
    expect(state.detail).toContain('数据卷')
  })

  it('卷状态读不出来时说“读不出来”，不冒充“没挂”', () => {
    // 渲染层对 unknown/error/mismatch 同样不写配置段，但话不能一样说：
    // 说成“没挂”会把查故障的人支到一块没坏的盘上。
    for (const state of ['unknown', 'error', 'mismatch']) {
      const summary = summarizeShareState(record(), { id: 'vol_x', name: '数据卷', state })
      expect(summary.kind).toBe('volume_offline')
      expect(summary.label).toBe('卷状态读不出来')
      expect(summary.tone).toBe('warning')
      expect(summary.detail).toContain(state)
    }
    expect(
      summarizeShareState(record(), { id: 'vol_x', name: '数据卷', state: 'missing' }).label,
    ).toBe('卷此刻没挂')
  })

  it('卷状态没取到时按“不知道”处理，不猜成 offline', () => {
    // 卷列表还没回来、或那个卷已不在受管理卷里——两种都不该画出“卷此刻没挂”。
    expect(summarizeShareState(record()).kind).toBe('serving')
    expect(summarizeShareState(record(), null).kind).toBe('serving')
    expect(summarizeShareState(record(), { id: 'vol_x', name: '', state: '' }).kind).toBe('serving')
  })

  it('已禁用排在“配置没写”之前：那是人主动关的', () => {
    const state = summarizeShareState(
      record({ config_applied: false, directory_prepared: false, definition: { ...record().definition, enabled: false } }),
    )
    expect(state.kind).toBe('disabled')
  })

  it('目录准备好了但配置没写：说清楚此刻不提供访问', () => {
    const state = summarizeShareState(record({ config_applied: false }))
    expect(state.kind).toBe('not_applied')
    expect(state.detail).toContain('不提供访问')
  })

  it('目录也没准备时换一句话（不是同一句“未写入”）', () => {
    const state = summarizeShareState(record({ config_applied: false, directory_prepared: false }))
    expect(state.detail).toContain('尚未准备')
  })
})

describe('能力报告', () => {
  it('只有 available 算可用', () => {
    expect(capabilityAvailable(capability())).toBe(true)
    expect(capabilityAvailable(capability({ status: 'blocked', reason: '没有 smbd' }))).toBe(false)
    expect(capabilityAvailable(null)).toBe(false)
    expect(capabilityAvailable(undefined)).toBe(false)
  })
})

describe('客户端说明里的命令', () => {
  const hints = clientHints(record(), 'panel.example')
  const byKind = (kind: string) => {
    const found = hints.find((h) => h.kind === kind)
    if (!found) throw new Error(`没有 ${kind} 这一条`)
    return found
  }

  it('四种客户端都有，地址形状正确', () => {
    expect(hints.map((h) => h.kind)).toEqual(['windows', 'macos', 'linux-mount', 'linux-smbclient'])
    expect(byKind('windows').address).toBe('\\\\panel.example\\data')
    expect(byKind('macos').address).toBe('smb://panel.example/data')
  })

  it('主机名用的是调用方给的地址，不是猜的', () => {
    const other = clientHints(record(), '10.1.2.3')
    expect(other[0]?.address).toBe('\\\\10.1.2.3\\data')
  })

  it('挂载命令只有 credentials=，没有 password=', () => {
    const mount = byKind('linux-mount')
    const mountLine = mount.commands.find((c) => c.startsWith('sudo mount'))
    expect(mountLine).toBeDefined()
    expect(mountLine).toContain('credentials=/root/.smb/data.cred')
    expect(mountLine).not.toContain('password=')
    // 整条命令清单里唯一带 password= 的行是 here-doc 的正文（走标准输入）。
    const withPassword = mount.commands.filter((c) => c.includes('password='))
    expect(withPassword).toEqual(['password=<密码>'])
    const heredocIndex = mount.commands.indexOf("sudo sh -c 'umask 077; cat > /root/.smb/data.cred' <<'CRED'")
    expect(heredocIndex).toBeGreaterThanOrEqual(0)
    expect(mount.commands[heredocIndex + 1]).toBe('username=<共享账号>')
    expect(mount.commands[heredocIndex + 2]).toBe('password=<密码>')
    expect(mount.commands[heredocIndex + 3]).toBe('CRED')
  })

  it('smbclient 只用 -U 给账号名，密码靠交互输入', () => {
    const cli = byKind('linux-smbclient')
    const line = cli.commands.find((c) => c.startsWith('smbclient'))
    expect(line).toBe('smbclient //panel.example/data -U <共享账号> -m SMB3')
    expect(cli.commands.join('\n')).not.toContain('password=')
  })

  it('来源网段原样列出，并写明名单管不住本机', () => {
    const notes = byKind('windows').notes.join('\n')
    expect(notes).toContain('192.168.1.0/24')
    expect(notes).toContain('127.0.0.1')
  })

  it('不限来源时说的是“任何能连到 445 的来源都能尝试认证”', () => {
    const share = record({ definition: { ...record().definition, allowed_cidrs: [] } })
    const notes = clientHints(share, 'h')[0]?.notes.join('\n') ?? ''
    expect(notes).toContain('没有限制来源网段')
    expect(notes).toContain('445')
  })

  it('只读共享在第一条注意里就说只读', () => {
    const share = record({ definition: { ...record().definition, read_only: true } })
    const notes = clientHints(share, 'h')[0]?.notes.join('\n') ?? ''
    expect(notes).toContain('只读')
  })

  it('配置没写时，说明里第一条就说此刻连不上', () => {
    const share = record({ config_applied: false })
    const notes = clientHints(share, 'h')[0]?.notes ?? []
    expect(notes[0]).toContain('此刻没有写在 Samba 配置里')
  })

  it('被运行时撤销时说同样的话（不要给一个指向空处的地址）', () => {
    const share = record({
      runtime_revoked: {
        at: '2026-10-22T09:00:00Z',
        volume_id: 'vol_0123456789abcdef0123456789abcdef',
        volume_name: '数据卷',
        volume_state: 'unmounted',
        reason: 'x',
      },
    })
    expect(clientHints(share, 'h')[0]?.notes[0]).toContain('此刻没有写在 Samba 配置里')
  })

  it('没有读写账号时提前说“即使连上也只能读”', () => {
    const share = record({
      definition: {
        ...record().definition,
        smb_users: [{ account_id: 'acc_read', access: 'read' }],
      },
    })
    const notes = clientHints(share, 'h')[0]?.notes.join('\n') ?? ''
    expect(notes).toContain('只能读')
  })
})

describe('说明是否当前可用', () => {
  it('正在服务时可用', () => {
    expect(clientHintsUsable(record())).toBe(true)
  })

  it('没写配置 / 被撤销 / 已禁用时不可用', () => {
    expect(clientHintsUsable(record({ config_applied: false }))).toBe(false)
    expect(clientHintsUsable(record({ definition: { ...record().definition, enabled: false } }))).toBe(false)
    expect(
      clientHintsUsable(
        record({
          runtime_revoked: {
            at: '2026-10-22T09:00:00Z',
            volume_id: 'vol_x',
            volume_name: 'x',
            volume_state: 'unmounted',
            reason: 'x',
          },
        }),
      ),
    ).toBe(false)
  })
})

describe('通知码与访问级别', () => {
  it('认识的码翻成中文，不认识的码原样返回（不编）', () => {
    expect(shareNoticeLabel('SHARE_NO_USERS')).not.toBe('SHARE_NO_USERS')
    expect(shareNoticeLabel('SOMETHING_NEW')).toBe('SOMETHING_NEW')
  })

  it('每个码的中文标签都不等于码本身', () => {
    for (const [code, label] of Object.entries(SHARE_NOTICE_LABELS)) {
      expect(label, code).not.toBe(code)
      expect(label.length, code).toBeGreaterThan(0)
    }
  })

  it('访问级别只有 read / write', () => {
    expect(Object.keys(SHARE_ACCESS_LABELS).sort()).toEqual(['read', 'write'])
  })
})

// ---------------------------------------------------------------------------
// 与真机脚本互相钉住的命令文本。
//
// 下面三条是 `tests/integration/p08-4-share-ui-client-acceptance.sh` 在 VM 上**原样执行**
// 的那几行（主机名换成 127.0.0.1、共享名 data、账号与密码换成夹具的值）。
// 任何一边改动都会让这里变红——这是“文档里写的命令真的跑过”的唯一机械保证。
// ---------------------------------------------------------------------------
export const VM_MOUNT_LINE =
  'sudo mount -t cifs //127.0.0.1/data /mnt/data -o credentials=/root/.smb/data.cred,vers=3.0,uid=$(id -u),gid=$(id -g)'
export const VM_CRED_HEREDOC =
  "sudo sh -c 'umask 077; cat > /root/.smb/data.cred' <<'CRED'"
export const VM_SMBCLIENT_LINE = 'smbclient //127.0.0.1/data -U <共享账号> -m SMB3'

describe('与真机脚本钉住的命令', () => {
  const hints = clientHints(record(), '127.0.0.1')
  const mount = hints.find((h) => h.kind === 'linux-mount')
  const cli = hints.find((h) => h.kind === 'linux-smbclient')

  it('挂载那一行就是脚本执行的那一行', () => {
    expect(mount?.commands).toContain(VM_MOUNT_LINE)
  })

  it('凭据 here-doc 那一行就是脚本执行的那一行', () => {
    expect(mount?.commands).toContain(VM_CRED_HEREDOC)
  })

  it('smbclient 那一行就是脚本执行的那一行', () => {
    expect(cli?.commands[0]).toBe(VM_SMBCLIENT_LINE)
  })
})

// ---------------------------------------------------------------------------
// NFS 导出（P14.4）。
//
// SMB 与 NFS 的客户端说明**不能共用**：一边的访问控制是账号 + ACL，另一边是网段 +
// root_squash。这几条盯住 NFS 说明里的三个具体风险：
//   1. 命令里出现账号或密码——NFS 根本没有这一层，写出来就是误导；
//   2. 直接让人 mount 而不让先 `findmnt`——挂载失败时客户端会**静默写本地目录**；
//   3. 地址里编一个面板并不知道的绝对路径——记录里没有卷挂载点，能给的是问出真路径的命令。
// ---------------------------------------------------------------------------

/** 一份 NFS 导出记录（默认读写、点名一个网段）。 */
function nfsRecord(overrides: Partial<ShareRecord['definition']> = {}): ShareRecord {
  return record({
    definition: {
      name: 'media',
      protocol: 'nfs',
      volume_id: 'vol_0123456789abcdef0123456789abcdef',
      relative_path: 'team/docs',
      enabled: true,
      read_only: false,
      allowed_cidrs: ['10.0.2.0/24'],
      smb_users: [],
      nfs_options: { root_squash: true, sync: true },
      ...overrides,
    },
  })
}

describe('NFS 能力报告', () => {
  it('NFS 可用与否只看 nfs.status，与 Samba 那一路互不影响', () => {
    expect(nfsCapabilityAvailable(capability({ nfs: { ...capability().nfs, status: 'available' } }))).toBe(true)
    // Samba 可用不代表 NFS 可用：两套工具、两个服务。
    expect(nfsCapabilityAvailable(capability())).toBe(false)
    expect(nfsCapabilityAvailable(null)).toBe(false)
    expect(nfsCapabilityAvailable(undefined)).toBe(false)
  })

  it('缺少 nfs 字段的旧载荷不会崩，按不可用处理', () => {
    const legacy = { ...capability() } as Partial<ShareCapability>
    delete legacy.nfs
    expect(nfsCapabilityAvailable(legacy as ShareCapability)).toBe(false)
  })
})

describe('NFS 客户端说明', () => {
  const hints = clientHints(nfsRecord(), 'panel.example')
  const byKind = (kind: string) => {
    const found = hints.find((h) => h.kind === kind)
    if (!found) throw new Error(`没有 ${kind} 这一条`)
    return found
  }

  it('三种客户端都是 NFS 那一套，没有一条 SMB 的痕迹', () => {
    expect(hints.map((h) => h.kind)).toEqual(['linux-nfs-mount', 'macos-nfs', 'windows-nfs'])
    const text = hints.flatMap((h) => [h.address, ...h.commands, ...h.notes]).join('\n')
    for (const smbOnly of ['mount.cifs', 'smbclient', 'credentials=', 'password=', 'smb://']) {
      expect(text).not.toContain(smbOnly)
    }
  })

  it('导出目录不编绝对路径：给 showmount 让人问出真路径', () => {
    const linux = byKind('linux-nfs-mount')
    expect(linux.commands[0]).toBe('showmount -e panel.example')
    expect(linux.notes.join('\n')).toContain('showmount -e')
    // 记录里没有卷挂载点，所以地址里是占位符而不是一个像模像样的 /srv/...
    expect(linux.address).toBe('panel.example:<卷挂载点>/team/docs')
    expect(linux.address).not.toContain('/srv/')
  })

  it('挂载之后先 findmnt：不然静默写本地盘', () => {
    const linux = byKind('linux-nfs-mount')
    const mountIdx = linux.commands.findIndex((c) => c.startsWith('sudo mount -t nfs'))
    const findmntIdx = linux.commands.findIndex((c) => c.startsWith('findmnt'))
    expect(mountIdx).toBeGreaterThanOrEqual(0)
    expect(findmntIdx).toBeGreaterThan(mountIdx)
    expect(linux.notes.join('\n')).toContain('静默')
  })

  it('只读/读写分别说出导出行与目录权限，两者都对得上', () => {
    const rw = clientHints(nfsRecord(), 'panel.example')[0]!.notes.join('\n')
    expect(rw).toContain('0777')
    expect(rw).toContain('rw')

    const ro = clientHints(nfsRecord({ read_only: true }), 'panel.example')[0]!.notes.join('\n')
    expect(ro).toContain('0755')
    expect(ro).toContain('ro')
  })

  it('root_squash 那条固定说法在每一条说明里（客户端 root 会被 squash）', () => {
    for (const hint of hints) {
      const text = hint.notes.join('\n')
      expect(text).toContain('root_squash')
      expect(text).toContain('nobody')
    }
    expect(hints[0]!.notes.join('\n')).toContain('root_squash=true、sync=true')
  })

  it('来源网段原样列出，并说明 NFS 没有账号这一层', () => {
    const text = byKind('windows-nfs').notes.join('\n')
    expect(text).toContain('10.0.2.0/24')
    expect(text).toContain('没有账号')
  })

  it('没写进托管导出文件时第一条就说不通', () => {
    const notApplied = clientHints({ ...nfsRecord(), config_applied: false }, 'panel.example')[0]!
    expect(notApplied.notes[0]).toContain('不在托管导出文件里')
    // 被运行时撤销同理：卷掉线之后句子不能还是“去连吧”。
    const revoked = clientHints(
      {
        ...nfsRecord(),
        config_applied: false,
        runtime_revoked: {
          at: '2026-10-22T09:00:00Z',
          volume_id: 'vol_0123456789abcdef0123456789abcdef',
          volume_name: '数据卷',
          volume_state: 'unmounted',
          reason: '卷「数据卷」已不在挂载状态',
        },
      },
      'panel.example',
    )[0]!
    expect(revoked.notes[0]).toContain('不在托管导出文件里')
  })

  it('Windows 那条给的是 UNC 形状的地址', () => {
    expect(byKind('windows-nfs').address).toBe(String.raw`\\panel.example<卷挂载点>\team\docs`)
  })

  it('说明可用性与 SMB 用同一条判据（卷挂载、已应用、未撤销）', () => {
    expect(clientHintsUsable(nfsRecord(), null)).toBe(true)
    expect(clientHintsUsable({ ...nfsRecord(), config_applied: false }, null)).toBe(false)
    expect(clientHintsUsable(nfsRecord(), { id: 'v', name: 'v', state: 'unmounted' })).toBe(false)
  })
})

// ---------------------------------------------------------------------------
// 与真机脚本互相钉住的 NFS 命令。
//
// 导出目录在脚本里是 `showmount -e` 问出来的真路径，所以**只有挂载选项那一截**
// 能逐字钉住：`tests/integration/p14-4-acceptance.sh` 用的是同一个字符串。
// ---------------------------------------------------------------------------
export const VM_NFS_MOUNT_OPTS = 'vers=3,proto=tcp'

describe('与真机脚本钉住的 NFS 命令', () => {
  const linux = clientHints(nfsRecord(), '127.0.0.1').find((h) => h.kind === 'linux-nfs-mount')!

  it('挂载行用的选项就是脚本里用的那串', () => {
    const mountLine = linux.commands.find((c) => c.startsWith('sudo mount -t nfs'))
    expect(mountLine).toBe(`sudo mount -t nfs -o ${VM_NFS_MOUNT_OPTS} 127.0.0.1:<卷挂载点>/team/docs /mnt/media`)
  })

  it('脚本先确认挂上了再动手（findmnt 那一行也在）', () => {
    expect(linux.commands).toContain('findmnt -T /mnt/media')
  })
})
