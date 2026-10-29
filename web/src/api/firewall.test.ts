// 防火墙写路径的 DTO 与参数构造（P14.3）。
//
// 这一层能被测的只有**纯函数**和一条请求路径，真机的规则改动归 VM 验收。
// 断言的重点是那几处"说错了人就会做错事"的地方：
//
//   1) 整份规则集提交：少一条就等于删掉它，所以表单校验必须逐行报错、不许静默丢；
//   2) `from` 归一成掩码后的 CIDR（与服务端同一口径），否则表单上写 `10.0.0.5/24`、
//      机器上是 `10.0.0.0/24`，人会以为自己的输入没生效；
//   3) 管理通路的提醒只是提醒，不能变成"能不能提交"的判据；
//   4) 标识会被拼进 ufw 的 comment 与命令行，形状必须在提交前就收窄。

import { afterEach, describe, expect, it, vi } from 'vitest'

import {
  MAX_MANAGED_RULES,
  buildFirewallApplyParams,
  buildFirewallRollbackParams,
  buildRule,
  describeFirewallChange,
  enabledText,
  firewallConfigHeadline,
  firewallPendingHeadline,
  fetchFirewallConfig,
  managementHint,
  parseRuleSource,
  ruleActionLabel,
  ruleFormFromRule,
  ruleProtoLabel,
  ruleText,
  type FirewallConfigState,
  type FirewallManagedRule,
  type FirewallRuleForm,
} from './firewall'

afterEach(() => {
  vi.unstubAllGlobals()
})

function rule(overrides: Partial<FirewallManagedRule> = {}): FirewallManagedRule {
  return { id: 'web', action: 'allow', port: 443, proto: 'tcp', from: '', ...overrides }
}

function form(overrides: Partial<FirewallRuleForm> = {}): FirewallRuleForm {
  return { id: 'web', action: 'allow', port: '443', proto: 'tcp', from: '', ...overrides }
}

function configState(overrides: Partial<FirewallConfigState> = {}): FirewallConfigState {
  const base: FirewallConfigState = {
    collected_at: '2026-10-26T12:00:00Z',
    manager: 'ufw',
    reasons: [],
    installed: true,
    enabled: false,
    editable: true,
    rules: [rule()],
    config_hash: 'sha256:abc123',
    pending: null,
    issues: [],
  }
  return { ...base, ...overrides }
}

describe('标签与文案', () => {
  it('动作与协议有中文标签，闭集外原样透出', () => {
    expect(ruleActionLabel('allow')).toBe('放行')
    expect(ruleActionLabel('limit')).toBe('放行但限速')
    expect(ruleActionLabel('log')).toBe('log')
    expect(ruleProtoLabel('tcp')).toBe('TCP')
    expect(ruleProtoLabel('sctp')).toBe('sctp')
  })

  it('一条规则的一句话：动作 + 端口/协议 + 来源', () => {
    expect(ruleText(rule())).toBe('放行 443/TCP（所有来源）')
    expect(ruleText(rule({ id: 'ssh', port: 22, from: '10.0.0.0/8' }))).toBe(
      '放行 22/TCP（10.0.0.0/8）',
    )
  })

  it('未启用必须说出来"规则在，但不生效"', () => {
    expect(enabledText(true)).toBe('已启用')
    expect(enabledText(false)).toContain('不生效')
  })

  it('卡片头部：可改时带上启用状态与规则条数', () => {
    expect(firewallConfigHeadline(configState())).toContain('UFW')
    expect(firewallConfigHeadline(configState())).toContain('1 条规则')
    expect(firewallConfigHeadline(configState({ rules: [] }))).toContain('还没有管任何规则')
    expect(firewallConfigHeadline(configState({ editable: false }))).toBe(
      '当前防火墙不能由面板修改',
    )
  })

  it('倒计时文案：没过期说还剩多久，过期说随时会退回', () => {
    expect(firewallPendingHeadline(90)).toContain('1 分 30 秒')
    expect(firewallPendingHeadline(0)).toContain('随时会把规则退回')
    expect(firewallPendingHeadline(-5)).toContain('随时会把规则退回')
  })
})

describe('来源解析', () => {
  it('空串是所有来源', () => {
    expect(parseRuleSource('')).toBe('')
    expect(parseRuleSource('   ')).toBe('')
  })

  it('单个地址补成 /32，CIDR 掩码后归一', () => {
    expect(parseRuleSource('10.0.0.5')).toBe('10.0.0.5/32')
    expect(parseRuleSource('10.0.0.5/24')).toBe('10.0.0.0/24')
    expect(parseRuleSource('192.168.1.1/0')).toBe('0.0.0.0/0')
  })

  it('形状不对就返回 null，不猜', () => {
    for (const bad of ['10.0.0.256', '10.0.0', '10.0.0.01', 'abc', '10.0.0.1/33', '10.0.0.1/x', '::1']) {
      expect(parseRuleSource(bad)).toBeNull()
    }
  })
})

describe('单条规则校验', () => {
  it('合法的行拼成一条规则，标识与动作归一成小写', () => {
    const built = buildRule(form({ id: 'WEB', action: 'ALLOW' }), [])
    expect(built.ok).toBe(true)
    if (built.ok) expect(built.rule).toEqual({ id: 'web', action: 'allow', port: 443, proto: 'tcp', from: '' })
  })

  it('标识：形状与重复都要拦', () => {
    const shape = buildRule(form({ id: 'Web UI' }), [])
    expect(shape.ok).toBe(false)
    if (!shape.ok) expect(shape.error).toContain('a-z')
    const dup = buildRule(form({ id: 'web' }), ['web'])
    expect(dup.ok).toBe(false)
    if (!dup.ok) expect(dup.error).toContain('另一条规则')
  })

  it('端口：非数字、0、越界都要拦', () => {
    for (const port of ['', '22abc', '0', '65536', '70000']) {
      expect(buildRule(form({ port }), []).ok).toBe(false)
    }
    expect(buildRule(form({ port: '65535' }), []).ok).toBe(true)
  })

  it('动作与协议：闭集外要拦', () => {
    expect(buildRule(form({ action: 'log' }), []).ok).toBe(false)
    expect(buildRule(form({ proto: 'sctp' }), []).ok).toBe(false)
  })

  it('来源不合法时错误里带上原样输入', () => {
    const built = buildRule(form({ from: '10.0.0.999' }), [])
    expect(built.ok).toBe(false)
    if (!built.ok) expect(built.error).toContain('10.0.0.999')
  })

  it('服务端返回的规则能原样铺回表单再拼回来', () => {
    const source = rule({ id: 'db', action: 'limit', port: 5432, proto: 'tcp', from: '10.0.0.0/8' })
    const built = buildRule(ruleFormFromRule(source), [])
    expect(built.ok).toBe(true)
    if (built.ok) expect(built.rule).toEqual(source)
  })
})

describe('buildFirewallApplyParams', () => {
  it('不可改或没有指纹时给原因，不给参数', () => {
    const notEditable = buildFirewallApplyParams(configState({ editable: false }), [form()], false)
    expect(notEditable.ok).toBe(false)
    const noHash = configState()
    delete noHash.config_hash
    const built = buildFirewallApplyParams(noHash, [form()], false)
    expect(built.ok).toBe(false)
    if (!built.ok) expect(built.error).toContain('config_hash')
  })

  it('成功时把指纹与整份规则集一起交出去', () => {
    const built = buildFirewallApplyParams(
      configState(),
      [form(), form({ id: 'ssh', port: '22' })],
      true,
    )
    expect(built.ok).toBe(true)
    if (!built.ok) return
    expect(built.params.expected_config_hash).toBe('sha256:abc123')
    expect(built.params.definition.enabled).toBe(true)
    expect(built.params.definition.rules.map((item) => item.id)).toEqual(['web', 'ssh'])
  })

  it('空规则集是合法的：那就是"清空面板管理的规则"', () => {
    const built = buildFirewallApplyParams(configState(), [], false)
    expect(built.ok).toBe(true)
    if (built.ok) expect(built.params.definition.rules).toEqual([])
  })

  it('哪一行错了要指出行号，不要只说"参数不合法"', () => {
    const built = buildFirewallApplyParams(configState(), [form(), form({ id: 'ssh', port: '0' })], false)
    expect(built.ok).toBe(false)
    if (!built.ok) expect(built.error).toContain('第 2 条')
  })

  it('超过上限要拦', () => {
    const rows = Array.from({ length: MAX_MANAGED_RULES + 1 }, (_, i) => form({ id: `r${i + 1}`, port: '80' }))
    const built = buildFirewallApplyParams(configState(), rows, false)
    expect(built.ok).toBe(false)
    if (!built.ok) expect(built.error).toContain(String(MAX_MANAGED_RULES))
  })
})

describe('回滚参数', () => {
  it('confirm 与 rollback 共用一份参数：只有 token', () => {
    expect(
      buildFirewallRollbackParams({
        token: 'frb_0123456789abcdef0123456789abcdef',
        deadline: '2026-10-26T12:02:00Z',
        seconds_left: 90,
        previous: '防火墙未启用；面板管理 1 条规则',
      }),
    ).toEqual({ token: 'frb_0123456789abcdef0123456789abcdef' })
  })
})

describe('改动说明', () => {
  it('加、改、删各说一句，并说明不会动管理员手写的规则', () => {
    const lines = describeFirewallChange(
      [rule(), rule({ id: 'old', port: 8080 })],
      [rule({ port: 8443 }), rule({ id: 'ssh', port: 22 })],
      true,
    )
    expect(lines.join('\n')).toContain('加：放行 22/TCP')
    expect(lines.join('\n')).toContain('改：web')
    expect(lines.join('\n')).toContain('删：放行 8080/TCP')
    expect(lines.join('\n')).toContain('手写')
    expect(lines.join('\n')).toContain('120 秒')
  })

  it('没变也要说出来，而不是给一张空表', () => {
    const lines = describeFirewallChange([rule()], [rule()], false)
    expect(lines[0]).toBe('面板管理的规则没有变化')
  })
})

describe('管理通路提醒', () => {
  it('要启用且没有任何"所有来源"的放行时提醒一句', () => {
    const hint = managementHint([rule({ from: '10.0.0.0/8' })], true)
    expect(hint).not.toBeNull()
    expect(hint).toContain('FIREWALL_MANAGEMENT_PATH')
    // 这句必须留着：页面算不准，判据在服务端。
    expect(hint).toContain('判据在服务端')
  })

  it('有一条所有来源的放行、或者根本不启用时不要多嘴', () => {
    expect(managementHint([rule()], true)).toBeNull()
    expect(managementHint([rule({ action: 'deny' })], false)).toBeNull()
    expect(managementHint([], false)).toBeNull()
  })
})

describe('请求', () => {
  it('config 查询打到 /firewall/config', async () => {
    const calls: string[] = []
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL) => {
        calls.push(typeof input === 'string' ? input : input.toString())
        return new Response(JSON.stringify({ data: configState(), request_id: 'req_1' }), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        })
      }),
    )
    const res = await fetchFirewallConfig()
    expect(calls[0]).toContain('/firewall/config')
    expect(res.data.config_hash).toBe('sha256:abc123')
    expect(res.data.rules[0]?.id).toBe('web')
  })
})
