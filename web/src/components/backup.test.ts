// 备份页纯逻辑测试（P12.5）：表单 → 请求体、形状校验、确认文案。
//
// 这里的每一条都对应一次真的会弄坏现场的错误：
//   - `local` 后端带了 `host`，服务端会**报错**而不是忽略 —— 所以构造器必须按后端分叉；
//   - 保留条数留空不是 0、也不是 NaN —— 留空意味着"不设这一项"，写进请求体会变成
//     "永远只留 0 份"，那等于每次清理都删光；
//   - 一致性口径选了"停应用"却没列应用 —— 快照会被打上标签，而实际是在应用
//     写着的中间态读的，比失败更糟。

import { describe, expect, it } from 'vitest'

import {
  MAX_POLICY_APPS,
  MAX_RETENTION,
  buildPolicyInput,
  buildRepositoryInput,
  emptyPolicyForm,
  emptyRepositoryForm,
  initializeConfirmLines,
  parseIdList,
  policyFormOf,
  pruneConfirmLines,
  repositoryFormOf,
  restoreConfirmLines,
  validatePolicyForm,
  validateRepositoryForm,
  verifyConfirmLines,
  type PolicyFormState,
  type RepositoryFormState,
} from './backup'
import type { BackupPolicy, BackupRepository } from '@/api/backup'

const SECRET = 'sec_0123456789abcdef0123456789abcdef'

function localRepo(overrides: Partial<RepositoryFormState> = {}): RepositoryFormState {
  return {
    ...emptyRepositoryForm('root1'),
    name: '每日备份',
    passwordSecretRef: SECRET,
    ...overrides,
  }
}

function sftpRepo(overrides: Partial<RepositoryFormState> = {}): RepositoryFormState {
  return {
    ...localRepo(),
    backend: 'sftp',
    host: 'backup.example.com',
    port: '2222',
    username: 'bkuser',
    remotePath: '/srv/restic',
    hostKeyFingerprint: 'SHA256:abcdef',
    credentialSecretRef: SECRET,
    ...overrides,
  }
}

function policyForm(overrides: Partial<PolicyFormState> = {}): PolicyFormState {
  return {
    ...emptyPolicyForm('rep_x'),
    name: '每晚全量',
    sources: [{ kind: 'root', rootId: 'root1', relativePath: 'srv/www' }],
    ...overrides,
  }
}

describe('仓库表单 → 请求体', () => {
  it('local 只带根与相对路径，凭据引用留空', () => {
    const input = buildRepositoryInput(localRepo())
    expect(input.backend).toBe('local')
    expect(input.location).toEqual({ root_id: 'root1', relative_path: 'upanel-restic' })
    expect(input.credential_secret_ref).toBe('')
    expect(input.location).not.toHaveProperty('host')
  })

  it('sftp 只带远端五字段，不带根', () => {
    const input = buildRepositoryInput(sftpRepo())
    expect(input.location).toEqual({
      host: 'backup.example.com',
      port: 2222,
      username: 'bkuser',
      remote_path: '/srv/restic',
      host_key_fingerprint: 'SHA256:abcdef',
    })
    expect(input.credential_secret_ref).toBe(SECRET)
    expect(input.location).not.toHaveProperty('root_id')
  })

  it('端口读不成数字时回落到 22（校验会另外拦住它）', () => {
    expect((buildRepositoryInput(sftpRepo({ port: 'abc' })).location as { port: number }).port).toBe(22)
  })

  it('version 由调用方给，新建是 0', () => {
    expect(buildRepositoryInput(localRepo()).version).toBe(0)
    expect(buildRepositoryInput(localRepo(), 6).version).toBe(6)
  })

  it('首尾空白被剪掉（名字里带空格是手误，不是意图）', () => {
    expect(buildRepositoryInput(localRepo({ name: '  每日  ' })).name).toBe('每日')
  })
})

describe('仓库表单校验', () => {
  it('合法的本地与 SFTP 表单没有问题', () => {
    expect(validateRepositoryForm(localRepo())).toEqual([])
    expect(validateRepositoryForm(sftpRepo())).toEqual([])
  })

  it('口令引用必须是 sec_ 加 32 位十六进制：面板从不持有明文', () => {
    expect(validateRepositoryForm(localRepo({ passwordSecretRef: 'hunter2' })).join()).toContain('sec_')
  })

  it('本地仓库带上私钥引用会被拦下（没有远端凭据这回事）', () => {
    expect(validateRepositoryForm(localRepo({ credentialSecretRef: SECRET })).join()).toContain(
      '本地仓库没有远端凭据',
    )
  })

  it('SFTP 必须给指纹：known_hosts 只来自显式核对', () => {
    expect(validateRepositoryForm(sftpRepo({ hostKeyFingerprint: '' })).join()).toContain('指纹')
  })

  it('SFTP 主机不能以 - 开头（会被 ssh 当成选项）', () => {
    expect(validateRepositoryForm(sftpRepo({ host: '-oProxyCommand=evil' })).join()).toContain('不能以 - 开头')
  })

  it('远端路径必须是绝对路径', () => {
    expect(validateRepositoryForm(sftpRepo({ remotePath: 'srv/restic' })).join()).toContain('绝对路径')
  })

  it('相对路径不能以 / 开头，也不能含 ..', () => {
    expect(validateRepositoryForm(localRepo({ relativePath: '/etc' })).join()).toContain('不能以 / 开头')
    expect(validateRepositoryForm(localRepo({ relativePath: 'a/../b' })).join()).toContain('..')
  })

  it('端口越界被拦下', () => {
    expect(validateRepositoryForm(sftpRepo({ port: '70000' })).join()).toContain('65535')
  })
})

describe('已有定义摊回表单', () => {
  it('SFTP 仓库摊开后原样能存回去（读出来、改一处、存回去）', () => {
    const repo: BackupRepository = {
      id: 'rep_x',
      name: '远端',
      backend: 'sftp',
      location: {
        host: 'h',
        port: 2222,
        username: 'bkuser',
        remote_path: '/srv/bk',
        host_key_fingerprint: 'SHA256:X',
      },
      password_secret_ref: SECRET,
      credential_secret_ref: SECRET,
      version: 3,
    }
    const form = repositoryFormOf(repo)
    expect(validateRepositoryForm(form)).toEqual([])
    const back = buildRepositoryInput(form, repo.version)
    expect(back.location).toEqual(repo.location)
    expect(back.version).toBe(3)
  })
})

describe('策略表单 → 请求体', () => {
  it('保留条数留空表示不设这一项，不会写成 0 份', () => {
    const input = buildPolicyInput(policyForm({ keepLast: '', keepDaily: '', keepWeekly: '' }))
    expect(input.retention).toEqual({})
  })

  it('留空的项与填了数字的项混在一起时，只带填了的那几项', () => {
    const input = buildPolicyInput(policyForm({ keepLast: '7', keepDaily: '', keepWeekly: '4' }))
    expect(input.retention).toEqual({ keep_last: 7, keep_weekly: 4 })
  })

  it('files_only 时 app_ids 一律为空（选了不匹配的清单反而是噪声）', () => {
    const input = buildPolicyInput(
      policyForm({ consistencyMode: 'files_only', appIds: 'app_0123456789abcdef0123456789abcdef' }),
    )
    expect(input.app_ids).toEqual([])
  })

  it('stop_managed_apps 时把应用清单拆开', () => {
    const input = buildPolicyInput(
      policyForm({
        consistencyMode: 'stop_managed_apps',
        appIds: 'app_a, app_b\napp_a',
      }),
    )
    expect(input.app_ids).toEqual(['app_a', 'app_b'])
  })

  it('面板数据库来源只写 kind，不带根与路径', () => {
    const input = buildPolicyInput(
      policyForm({ sources: [{ kind: 'panel_database', rootId: 'ignored', relativePath: 'x' }] }),
    )
    expect(input.sources).toEqual([{ kind: 'panel_database' }])
  })
})

describe('策略表单校验', () => {
  it('合法表单没有问题', () => {
    expect(validatePolicyForm(policyForm())).toEqual([])
  })

  it('没指向仓库时被拦下', () => {
    expect(validatePolicyForm(policyForm({ repositoryId: '' })).join()).toContain('仓库')
  })

  it('选了"停应用"却没列应用时被拦下，并说清为什么这不是小事', () => {
    const problems = validatePolicyForm(
      policyForm({ consistencyMode: 'stop_managed_apps', appIds: '   ' }),
    ).join()
    expect(problems).toContain('stop_managed_apps')
    expect(problems).toContain('中间态')
  })

  it('应用数量有上限', () => {
    const many = Array.from({ length: MAX_POLICY_APPS + 1 }, (_, index) => `app_${index}`).join(',')
    expect(
      validatePolicyForm(policyForm({ consistencyMode: 'stop_managed_apps', appIds: many })).join(),
    ).toContain(`最多 ${MAX_POLICY_APPS}`)
  })

  it('保留条数写成了非整数时被拦下（不能悄悄当 0）', () => {
    expect(validatePolicyForm(policyForm({ keepLast: '七' })).join()).toContain('整数')
    expect(validatePolicyForm(policyForm({ keepLast: String(MAX_RETENTION + 1) })).join()).toContain('整数')
  })

  it('一条来源都没有时被拦下', () => {
    expect(validatePolicyForm(policyForm({ sources: [] })).join()).toContain('至少')
  })
})

describe('id 清单解析', () => {
  it('逗号、中文逗号、分号与换行都算分隔，去空去重保序', () => {
    expect(parseIdList(' app_a ,app_b，app_a; app_c\n\n')).toEqual(['app_a', 'app_b', 'app_c'])
  })

  it('空串得到空清单', () => {
    expect(parseIdList('   ')).toEqual([])
  })
})

describe('策略摊回表单', () => {
  it('读出来再存回去，不会把"没设保留"变成"留 0 份"', () => {
    const policy: BackupPolicy = {
      id: 'pol_x',
      name: '每晚',
      repository_id: 'rep_x',
      sources: [{ kind: 'root', root_id: 'root1', relative_path: 'srv' }],
      consistency_mode: 'files_only',
      app_ids: [],
      retention: {},
      version: 2,
    }
    const form = policyFormOf(policy)
    expect(form.keepLast).toBe('')
    expect(buildPolicyInput(form, policy.version).retention).toEqual({})
  })
})

describe('确认文案', () => {
  const repo = {
    name: '每日备份',
    backend: 'sftp' as const,
    location: { host: 'h', port: 22, username: 'u', remote_path: '/b' },
  }

  it('初始化仓库说清"登记"与"真的建仓库"是两件事', () => {
    const lines = initializeConfirmLines(repo).join('\n')
    expect(lines).toContain('restic init')
    expect(lines).toContain('登记一条定义只是记下')
  })

  it('恢复的确认里写明目标根与是否覆盖', () => {
    const lines = restoreConfirmLines({
      repositoryName: '每日备份',
      snapshot: { short_id: 'abcdef01', time: '2026-10-24T03:00:00Z', paths: ['/srv/www'] },
      targetRootName: 'data',
      relativePath: 'restored',
      overwrite: false,
    }).join('\n')
    expect(lines).toContain('/restored')
    expect(lines).toContain('不会覆盖已有文件')
  })

  it('抽样恢复与只核结构的说法不能混', () => {
    const withSample = verifyConfirmLines({ short_id: 'abcdef01', time: 't' }, true).join('\n')
    const without = verifyConfirmLines({ short_id: 'abcdef01', time: 't' }, false).join('\n')
    expect(withSample).toContain('真的抽几个文件')
    expect(without).toContain('不证明内容读得回来')
  })

  it('清理的确认指向预检的影响，而不是自己编一份待删名单', () => {
    const lines = pruneConfirmLines({ name: '每晚全量', retention: { keep_last: 7 } }).join('\n')
    expect(lines).toContain('真的删数据')
    expect(lines).toContain('PREFLIGHT_STALE')
    // 不列具体快照 id：那份名单是执行状态，由预检算。
    expect(lines).not.toMatch(/[0-9a-f]{64}/)
  })
})
