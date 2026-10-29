// 账号 API 层：状态翻译、闭集标签、动作参数（P14.5）。
//
// 这一层不碰网络，只测“给定服务端载荷，页面会说什么、会提交什么”。四条底线
// 在这里是可断言的：
//   1. `shell` 与 `shell_profile` 是两个问题：禁用之后 shell 是 nologin，而 profile
//      仍然记着“启用之后该回哪一档”；
//   2. 空串的 `shell_profile` 按 disabled 读——把 nologin 读成 restricted 会让人以为
//      这个账号本来能登录；
//   3. 参数里**没有** uid/gid/groups/home：那四项由系统分配，能提交它们就等于能
//      提交 uid=0；
//   4. 新建与改是同一个动作的两条路：`account_id:null` 才是新建，给了 id 就必须带
//      version（并发校验，不是可选备注）。

import { describe, expect, it } from 'vitest'

import {
  ACCOUNT_ACTIONS,
  ACCOUNT_NOTICE_LABELS,
  accountApplyParams,
  accountDeleteParams,
  accountDisplayLabel,
  accountNameProblem,
  accountNoticeLabel,
  accountShellProfile,
  accountSudoWritable,
  accountsAvailable,
  buildAccountDefinition,
  editableAsRegular,
  sshKeyApplyParams,
  sshKeyDeleteParams,
  sudoRuleApplyParams,
  sudoRuleDeleteParams,
  sudoTemplateLabel,
  SUDO_TEMPLATES,
  summarizeAccountState,
  type AccountCapability,
  type ManagedAccount,
  type ManagedAccountSSHKey,
} from './accounts'

function account(overrides: Partial<ManagedAccount> = {}): ManagedAccount {
  const base: ManagedAccount = {
    id: 'sac_00112233445566778899aabbccddeeff',
    name: 'alice',
    display_name: 'Alice',
    type: 'regular',
    managed: true,
    disabled: false,
    has_password: true,
    uid: 1000,
    gid: 1000,
    shell: '/bin/rbash',
    shell_profile: 'restricted',
    home: '/home/alice',
    version: '3',
    created_at: '2026-11-04T09:00:00Z',
    updated_at: '2026-11-04T09:05:00Z',
  }
  return { ...base, ...overrides }
}

function sshKey(overrides: Partial<ManagedAccountSSHKey> = {}): ManagedAccountSSHKey {
  return {
    id: 'ssk_0102030405060708090a0b0c0d0e0f10',
    account_id: 'sac_00112233445566778899aabbccddeeff',
    type: 'ssh-ed25519',
    comment: 'alice@laptop',
    fingerprint: 'SHA256:Zm9vYmFy',
    created_at: '2026-11-04T09:10:00Z',
    ...overrides,
  }
}

function capability(overrides: Partial<AccountCapability> = {}): AccountCapability {
  return {
    status: 'available',
    tools: {
      useradd: '/usr/sbin/useradd',
      usermod: '/usr/sbin/usermod',
      userdel: '/usr/sbin/userdel',
      getent: '/usr/bin/getent',
      passwd: '/usr/bin/passwd',
      chpasswd: '/usr/sbin/chpasswd',
      visudo: '/usr/sbin/visudo',
      restricted_shell: '/bin/rbash',
      disabled_shell: '/usr/sbin/nologin',
    },
    sudo_rule_write: 'available',
    shell_profiles: ['disabled', 'restricted'],
    notices: [],
    collected_at: '2026-11-04T09:00:00Z',
    ...overrides,
  }
}

describe('账号状态翻译', () => {
  it('禁用优先于其他一切状态', () => {
    const state = summarizeAccountState(account({ disabled: true, has_password: false }))
    expect(state.kind).toBe('disabled')
    expect(state.label).toBe('已禁用')
    // profile 仍是 restricted，所以文案要说清“启用之后回到哪一档”。
    expect(state.detail).toContain('/bin/rbash')
  })

  it('共享账号不是一个能登录的账号', () => {
    const state = summarizeAccountState(
      account({ type: 'share', shell: '/usr/sbin/nologin', shell_profile: 'disabled', home: '/nonexistent' }),
    )
    expect(state.kind).toBe('share')
    expect(state.detail).toContain('/nonexistent')
  })

  it('没有口令是告警，但要说清它不等于登不上（Q479 真机读数）', () => {
    const state = summarizeAccountState(account({ has_password: false }))
    expect(state.kind).toBe('no_password')
    expect(state.tone).toBe('warning')
    // 首轮这里写的是“配了公钥也登不上”，真机否了它：key-only 账号能登。
    expect(state.detail).toContain('公钥')
    expect(state.detail).not.toContain('配了公钥也登不上')
    expect(state.detail).toContain('禁用')
  })

  it('普通账号的常态把 uid、壳与 home 一起说出来', () => {
    const state = summarizeAccountState(account())
    expect(state.kind).toBe('regular')
    expect(state.detail).toContain('1000')
    expect(state.detail).toContain('/bin/rbash')
    expect(state.detail).toContain('/home/alice')
  })
})

describe('壳档与展示名', () => {
  it('空串的 shell_profile 按 disabled 读', () => {
    expect(accountShellProfile(account({ shell_profile: '' }))).toBe('disabled')
    expect(accountShellProfile(account({ shell_profile: 'restricted' }))).toBe('restricted')
    // 认不出来的值也按 disabled：把 nologin 读成能登录是更糟的那个方向。
    expect(accountShellProfile(account({ shell_profile: 'nonsense' }))).toBe('disabled')
  })

  it('没有展示名就用账号名', () => {
    expect(accountDisplayLabel(account({ display_name: '  ' }))).toBe('alice')
    expect(accountDisplayLabel(account({ display_name: '张记备份' }))).toBe('张记备份')
  })

  it('共享账号不在这个页面上改', () => {
    expect(editableAsRegular(account())).toBe(true)
    expect(editableAsRegular(account({ type: 'share' }))).toBe(false)
  })
})

describe('闭集与标签', () => {
  it('未知的阻塞码原样返回，不被吞掉', () => {
    expect(accountNoticeLabel('ACCOUNT_RESERVED')).toBe('这是系统保留名')
    expect(accountNoticeLabel('SOMETHING_NEW')).toBe('SOMETHING_NEW')
    // 表里没有空串这一项：空码不该被翻译成任何一句人话。
    expect(ACCOUNT_NOTICE_LABELS['']).toBeUndefined()
  })

  it('sudo 模板只有两条，而且不带通配符', () => {
    expect(SUDO_TEMPLATES.map((t) => t.id)).toEqual(['system_status', 'journal_recent'])
    for (const template of SUDO_TEMPLATES) {
      expect(template.commands.length).toBeGreaterThan(0)
      for (const command of template.commands) {
        expect(command.startsWith('/')).toBe(true)
        expect(command).not.toContain('*')
      }
    }
    expect(sudoTemplateLabel('journal_recent')).toBe('查看最近日志')
    expect(sudoTemplateLabel('all')).toBe('all')
  })

  it('能力不可用时页面能说清原因', () => {
    expect(accountsAvailable(capability())).toBe(true)
    expect(accountsAvailable(capability({ status: 'blocked' }))).toBe(false)
    expect(accountsAvailable(null)).toBe(false)
  })

  // 账号能力与共享能力是两份，判据不相交。曾经借过共享那一份，结果是一台没装
  // Samba 的机器上账号页显示“不支持账号管理”——而建普通账号一个 Samba 工具都
  // 不需要。这条锁住“账号能力只看账号要的东西”。
  it('缺 visudo 只关掉 sudo 那一半，账号仍然可用', () => {
    const noVisudo = capability({ sudo_rule_write: 'unsupported' })
    expect(accountsAvailable(noVisudo)).toBe(true)
    expect(accountSudoWritable(noVisudo)).toBe(false)
    expect(accountSudoWritable(capability())).toBe(true)
    expect(accountSudoWritable(null)).toBe(false)
    // 能力报告里只说账号的工具：出现 Samba 的字段就说明又接错了来源。
    expect(Object.keys(capability().tools)).not.toContain('smbd')
    expect(capability().shell_profiles).toEqual(['disabled', 'restricted'])
  })
})

describe('动作参数', () => {
  it('定义里没有 uid/gid/groups/home', () => {
    const definition = buildAccountDefinition({
      name: '  bob  ',
      display_name: '',
      shell_profile: 'restricted',
      disabled: false,
      password_secret_ref: '',
    })
    expect(definition.name).toBe('bob')
    expect(definition.type).toBe('regular')
    expect(Object.keys(definition).sort()).toEqual([
      'disabled',
      'display_name',
      'name',
      'shell_profile',
      'type',
    ])
    // 空引用是“这次不设口令”，不是“设成空口令”：字段直接不出现。
    expect('password_secret_ref' in definition).toBe(false)
  })

  it('给了引用就带上引用，明文永远不在这里', () => {
    const definition = buildAccountDefinition({
      name: 'bob',
      display_name: 'Bob',
      shell_profile: 'disabled',
      disabled: true,
      password_secret_ref: ' sec_0123456789abcdef0123456789abcdef ',
    })
    expect(definition.password_secret_ref).toBe('sec_0123456789abcdef0123456789abcdef')
    expect(definition.disabled).toBe(true)
  })

  it('新建是 account_id:null，改一条就必须带 version', () => {
    const definition = buildAccountDefinition({
      name: 'carol',
      display_name: '',
      shell_profile: 'restricted',
      disabled: false,
      password_secret_ref: '',
    })
    const create = accountApplyParams(definition)
    expect(create.account_id).toBeNull()
    expect(create.version).toBeNull()

    const update = accountApplyParams(definition, { id: 'sac_x', version: '7' })
    expect(update.account_id).toBe('sac_x')
    expect(update.version).toBe('7')
  })

  it('删除带上版本号（并发校验不是可选备注）', () => {
    expect(accountDeleteParams(account())).toEqual({
      account_id: 'sac_00112233445566778899aabbccddeeff',
      version: '3',
    })
  })

  it('公钥：新增给 key_id:null，替换给已有的 id', () => {
    const key = sshKey()
    const add = sshKeyApplyParams(account(), 'ssh-ed25519 AAAA comment')
    expect(add.key_id).toBeNull()
    expect(add.version).toBe('3')

    const replace = sshKeyApplyParams(account(), 'ssh-ed25519 BBBB comment', key)
    expect(replace.key_id).toBe(key.id)
  })

  it('删公钥与 sudo 都指明目标', () => {
    expect(sshKeyDeleteParams(account(), sshKey())).toEqual({
      account_id: 'sac_00112233445566778899aabbccddeeff',
      version: '3',
      key_id: 'ssk_0102030405060708090a0b0c0d0e0f10',
    })
    expect(sudoRuleApplyParams(account(), 'system_status')).toEqual({
      account_id: 'sac_00112233445566778899aabbccddeeff',
      version: '3',
      template_id: 'system_status',
    })
    expect(sudoRuleDeleteParams(account())).toEqual({
      account_id: 'sac_00112233445566778899aabbccddeeff',
      version: '3',
    })
  })

  it('六个动作名与契约一致', () => {
    expect(ACCOUNT_ACTIONS).toEqual({
      apply: 'account.apply',
      delete: 'account.delete',
      keyApply: 'ssh_key.apply',
      keyDelete: 'ssh_key.delete',
      sudoApply: 'sudo_rule.apply',
      sudoDelete: 'sudo_rule.delete',
    })
  })
})

// 名字形状：与 internal/accounts/definition.go 的 ValidateName 同一套。
//
// 这里断言的是“页面会不会把明显填错的输入放行”，不是安全边界：保留名（root、
// www-data…）只在代理那边有一份，页面不复制它——复制一份就是多一处会漂移的真值。
describe('账号名形状（accountNameProblem）', () => {
  it('空名字直接拒', () => {
    expect(accountNameProblem('')).toBe('账号名不能为空')
  })

  it('首字符不是小写字母就拒，并把下划线开头单独说清', () => {
    expect(accountNameProblem('_apt')).toContain('下划线开头')
    expect(accountNameProblem('Alice')).toContain('小写字母开头')
    // useradd 自己也不接受数字开头；这里与首字母判据是同一句。
    expect(accountNameProblem('9lives')).toContain('小写字母开头')
  })

  it('连字符不能收尾，长度上限是 32', () => {
    expect(accountNameProblem('alice-')).toContain('连字符结尾')
    expect(accountNameProblem('a'.repeat(33))).toContain('过长')
    expect(accountNameProblem('a'.repeat(32))).toBeNull()
  })

  it('字母、数字、下划线、连字符之外一律拒', () => {
    expect(accountNameProblem('alice.b')).toContain('只能包含')
    expect(accountNameProblem('alice b')).toContain('只能包含')
    // 中文名不是 Linux 账号名：SMB 名与 Linux 名必须一一对应。
    expect(accountNameProblem('张三')).not.toBeNull()
  })

  it('合法的名字原样通过', () => {
    expect(accountNameProblem('alice')).toBeNull()
    expect(accountNameProblem('a')).toBeNull()
    expect(accountNameProblem('alice-01_x')).toBeNull()
  })
})
