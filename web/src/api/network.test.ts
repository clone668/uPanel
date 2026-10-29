// 网络 DTO 的纯逻辑测试（P14.1）。
//
// 只测"不撒谎"的部分，不测渲染：
//   - 闭合集合一个不漏地有中文标签，未知值原样透出；
//   - 载波三态里"读不到"不等于"断线"；
//   - `metric` 靠 `pref_set` 区分"没设置"与 0；
//   - `pid=0` 说成"读不到占用进程"，不说成空闲；
//   - 能不能改的横幅在 false 时不假装"暂不支持"，而是把原因留在 reasons 里；
//   - 四条请求都带得起代理侧 8 秒预算之外的那点余量。

import { afterEach, describe, expect, it, vi } from 'vitest'

import {
  APPLY_MODES,
  APPLY_MODE_LABELS,
  BACKENDS,
  BACKEND_LABELS,
  EMPTY_APPLY_FORM,
  EXPOSURE_LABELS,
  FAMILIES,
  FAMILY_LABELS,
  ISSUE_CODES,
  ISSUE_CODE_LABELS,
  KINDS,
  KIND_LABELS,
  MANAGERS,
  MANAGER_LABELS,
  NETWORK_QUERY_TIMEOUT_MS,
  ROLLBACK_STATES,
  ROLLBACK_STATE_LABELS,
  ROLLBACK_STATE_TYPES,
  addressText,
  applyModeLabel,
  backendLabel,
  buildApplyParams,
  buildDefinition,
  buildRollbackParams,
  capabilityHeadline,
  carrierText,
  countdownText,
  currentAddressText,
  exposureLabel,
  familyLabel,
  fetchFirewall,
  fetchNetworkConfig,
  fetchNetworkInterfaces,
  fetchNetworkListeners,
  fetchNetworkRoutes,
  firewallHeadline,
  issueCodeLabel,
  issueText,
  kindLabel,
  listenerOwnerText,
  managerLabel,
  metricText,
  parseIPv4,
  pendingHeadline,
  rollbackSecondsLeft,
  rollbackStateLabel,
  type ApplyFormInput,
  type FirewallState,
  type NetworkCapability,
  type NetworkConfigState,
  type NetworkInterface,
  type NetworkListener,
  type NetworkRollbackView,
  type NetworkRoute,
} from './network'

afterEach(() => {
  vi.unstubAllGlobals()
})

// 测试替身允许显式传 undefined（tsconfig 开了 exactOptionalPropertyTypes）：
// "把 target 清掉"本身就是这里要断言的一种状态。
function capability(
  overrides: Partial<NetworkCapability> = {},
): NetworkCapability {
  return {
    backend: 'netplan-networkd',
    editable: true,
    target: 'ens4',
    reasons: [],
    ...overrides,
  }
}

function route(overrides: Partial<NetworkRoute> = {}): NetworkRoute {
  return {
    family: 'ipv4',
    destination: '192.168.1.0/24',
    device: 'ens4',
    metric: 0,
    pref_set: false,
    protocol: 'kernel',
    default: false,
    ...overrides,
  }
}

function listener(
  overrides: Partial<NetworkListener> = {},
): NetworkListener {
  return {
    family: 'ipv4',
    protocol: 'tcp',
    address: '0.0.0.0',
    port: 8080,
    wildcard: true,
    loopback: false,
    pid: 1234,
    process: 'nginx',
    ...overrides,
  }
}

function firewall(overrides: Partial<FirewallState> = {}): FirewallState {
  return {
    collected_at: '2026-10-26T12:00:00Z',
    manager: 'ufw',
    manager_reasons: [],
    installed: true,
    enabled: true,
    rules: [],
    editable: true,
    reasons: [],
    issues: [],
    ...overrides,
  }
}

describe('闭合集合都有中文标签', () => {
  it('配置归属、网卡类型、家族、原因码、管理器一个不漏', () => {
    for (const value of BACKENDS) expect(BACKEND_LABELS[value]).toBeTruthy()
    for (const value of KINDS) expect(KIND_LABELS[value]).toBeTruthy()
    for (const value of FAMILIES) expect(FAMILY_LABELS[value]).toBeTruthy()
    for (const value of ISSUE_CODES) expect(ISSUE_CODE_LABELS[value]).toBeTruthy()
    for (const value of MANAGERS) expect(MANAGER_LABELS[value]).toBeTruthy()
    for (const value of ['wildcard', 'loopback', 'specific'] as const) {
      expect(EXPOSURE_LABELS[value]).toBeTruthy()
    }
  })

  it('未知值原样透出，不吞成空串', () => {
    expect(backendLabel('something-new')).toBe('something-new')
    expect(kindLabel('something-new')).toBe('something-new')
    expect(familyLabel('something-new')).toBe('something-new')
    expect(issueCodeLabel('something-new')).toBe('something-new')
    expect(managerLabel('something-new')).toBe('something-new')
  })
})

describe('载波三态', () => {
  it('读不到不等于断线', () => {
    expect(carrierText(null)).toBe('读不到')
    expect(carrierText(true)).toBe('有载波')
    expect(carrierText(false)).toBe('无载波')
  })
})

describe('地址与优先级', () => {
  it('地址带前缀长度', () => {
    expect(addressText({ family: 'ipv4', address: '192.168.1.10', prefix_len: 24, scope: 'global' })).toBe(
      '192.168.1.10/24',
    )
  })

  it('0 是合法优先级，"没有"要靠 pref_set 说', () => {
    expect(metricText(route({ metric: 0, pref_set: true }))).toBe('0')
    expect(metricText(route({ metric: 0, pref_set: false }))).toBe('未设置')
    expect(metricText(route({ metric: 100, pref_set: true }))).toBe('100')
  })
})

describe('能不能改的横幅', () => {
  it('可改时说清楚改的是哪张网卡', () => {
    expect(capabilityHeadline(capability())).toContain('ens4')
  })

  it('不可改时不假装"暂不支持"，原因留给 reasons', () => {
    const notEditable = capability({ editable: false, reasons: ['本机有 br0、ens4 多张物理网卡'] })
    // 不可改时没有 target：删掉而不是塞一个空串，免得页面显示"修改：（空）"。
    delete notEditable.target
    const text = capabilityHeadline(notEditable)
    expect(text).toBe('当前配置不能由面板修改')
    expect(text).not.toContain('暂不支持')
  })
})

describe('监听端口', () => {
  it('通配 / 仅本机 / 指定地址分开说', () => {
    expect(exposureLabel(listener())).toBe('所有地址')
    expect(exposureLabel(listener({ address: '127.0.0.1', wildcard: false, loopback: true }))).toBe('仅本机')
    expect(exposureLabel(listener({ address: '10.0.0.5', wildcard: false, loopback: false }))).toBe('指定地址')
  })

  it('pid=0 是读不到占用进程，不是没有进程', () => {
    const ownerless = listener({ pid: 0 })
    delete ownerless.process
    const text = listenerOwnerText(ownerless)
    expect(text).toBe('占用进程读不到')
    expect(text).not.toContain('空闲')
  })

  it('有进程时带上单元名', () => {
    expect(listenerOwnerText(listener({ pid: 1, process: 'nginx', unit: 'nginx.service' }))).toBe(
      'nginx · 1（nginx.service）',
    )
  })
})

describe('防火墙结论', () => {
  it('在管且启用 / 装了没启用 / 没发现 / 判不出来 四种分开说', () => {
    expect(firewallHeadline(firewall())).toBe('UFW 在管，且已启用')
    expect(firewallHeadline(firewall({ enabled: false }))).toBe('UFW 装了，但没有启用')
    expect(firewallHeadline(firewall({ installed: false, manager: 'ufw' }))).toContain('读不到')
    expect(firewallHeadline(firewall({ manager: 'none', installed: false }))).toBe('没有发现任何防火墙管理器')
    expect(firewallHeadline(firewall({ manager: 'unknown' }))).toBe('防火墙管理器判不出来')
    expect(firewallHeadline(firewall({ manager: 'firewalld', installed: false }))).toBe('防火墙由 firewalld 管理')
  })
})

describe('issue 文案', () => {
  it('有 detail 时接在后面，没有就只报来源与原因码', () => {
    expect(issueText({ source: 'ip', code: 'missing' })).toBe('ip：来源不存在')
    expect(issueText({ source: 'ip', code: 'timeout', detail: '跑 ip 超时' })).toBe('ip：读取超时 — 跑 ip 超时')
  })
})

describe('请求路径与预算', () => {
  function stubOnce(payload: unknown): string[] {
    const calls: string[] = []
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL) => {
        calls.push(typeof input === 'string' ? input : input.toString())
        return new Response(JSON.stringify({ data: payload, request_id: 'req_1' }), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        })
      }),
    )
    return calls
  }

  it('四个接口各自打到自己的路径', async () => {
    const empty = {
      collected_at: '2026-10-26T12:00:00Z',
      interfaces: [],
      routes: [],
      gateways: [],
      listeners: [],
      truncated: false,
      issues: [],
      manager: 'ufw',
      manager_reasons: [],
      installed: true,
      enabled: false,
      rules: [],
      editable: false,
      reasons: [],
      capability: capability(),
    }

    const ifaceCalls = stubOnce(empty)
    await fetchNetworkInterfaces()
    expect(ifaceCalls[0]).toContain('/network/interfaces')

    const routeCalls = stubOnce(empty)
    await fetchNetworkRoutes()
    expect(routeCalls[0]).toContain('/network/routes')

    const listenerCalls = stubOnce(empty)
    await fetchNetworkListeners()
    expect(listenerCalls[0]).toContain('/network/listeners')

    const firewallCalls = stubOnce(empty)
    await fetchFirewall()
    expect(firewallCalls[0]).toContain('/firewall')
  })

  it('超时预算比代理侧 8 秒采集上限宽，不是默认的 15 秒', () => {
    expect(NETWORK_QUERY_TIMEOUT_MS).toBeGreaterThan(8_000)
  })
})

// 类型只在编译期起作用；这里放一处引用，免得 tsc 把未使用的导入判成错。
const _typeOnly: [NetworkInterface, NetworkRoute, NetworkListener] = [
  { name: 'x', index: 1, kind: 'ethernet', mtu: 1500, state: 'up', carrier: null, physical: true, addresses: [] },
  route(),
  listener(),
]
void _typeOnly

// ---------------------------------------------------------------- P14.2 写路径
//
// 写路径的纯函数只负责两件事：把表单拼成能提交的形状、把倒计时算对。
// 判据（地址段禁令、网卡可改性）在代理那边，这里不复制第二份。

function configState(patch: Partial<NetworkConfigState> = {}): NetworkConfigState {
  const base: NetworkConfigState = {
    collected_at: '2026-10-26T12:00:00Z',
    capability: {
      backend: 'netplan-networkd',
      editable: true,
      target: 'ens4',
      reasons: [],
    },
    interface: 'ens4',
    permanent_mac: '52:54:00:aa:bb:cc',
    config_hash: 'sha256:abc123',
    mode: 'static',
    ipv4: { address: '192.168.1.10', prefix_length: 24, gateway: '192.168.1.1', dns: null },
    panel_file: null,
    netplan_files: [],
    unsupported: [],
    pending: null,
    issues: [],
  }
  return { ...base, ...patch }
}

function rollbackView(patch: Partial<NetworkRollbackView> = {}): NetworkRollbackView {
  const base: NetworkRollbackView = {
    token: 'nrb_0123456789abcdef0123456789abcdef',
    interface: 'ens4',
    mode: 'static',
    candidate_address: '192.168.1.50',
    applied_at: '2026-10-26T12:00:00Z',
    deadline: '2026-10-26T12:02:00Z',
    seconds_left: 120,
    state: 'pending',
  }
  return { ...base, ...patch }
}

describe('网络写路径的表单与倒计时（P14.2）', () => {
  it('两个闭合集合每个值都有中文标签，未知值原样透出', () => {
    for (const mode of APPLY_MODES) expect(APPLY_MODE_LABELS[mode]).toBeTruthy()
    for (const state of ROLLBACK_STATES) {
      expect(ROLLBACK_STATE_LABELS[state]).toBeTruthy()
      expect(ROLLBACK_STATE_TYPES[state]).toBeTruthy()
    }
    // 'failed' 在回滚记录的 state 里存在，在动作结果里不存在——这是两套词表。
    expect(ROLLBACK_STATES).toContain('failed')
    expect(applyModeLabel('weird')).toBe('weird')
    expect(rollbackStateLabel('weird')).toBe('weird')
  })

  it('parseIPv4 只认点分十进制，不做宽容解析', () => {
    expect(parseIPv4('192.168.1.50')).toEqual([192, 168, 1, 50])
    expect(parseIPv4(' 10.0.0.1 ')).toEqual([10, 0, 0, 1])
    for (const bad of ['192.168.1', '192.168.1.256', '192.168.01.1', '', '1.2.3.4.5', '::1', '192.168.1.1/24']) {
      expect(parseIPv4(bad)).toBeNull()
    }
  })

  it('dhcp 的 definition 里 ipv4 是 null，不是空对象', () => {
    const built = buildDefinition({ ...EMPTY_APPLY_FORM, mode: 'dhcp' })
    expect(built.ok).toBe(true)
    if (built.ok) expect(built.definition).toEqual({ mode: 'dhcp', ipv4: null })
  })

  it('静态配置按表单拼出来，DNS 留空是 null', () => {
    const withDns = buildDefinition({
      mode: 'static',
      address: ' 192.168.1.50 ',
      prefixLength: '24',
      gateway: '192.168.1.1',
      dns: '1.1.1.1, 8.8.8.8',
    })
    expect(withDns.ok).toBe(true)
    if (withDns.ok) {
      expect(withDns.definition.ipv4).toEqual({
        address: '192.168.1.50',
        prefix_length: 24,
        gateway: '192.168.1.1',
        dns: ['1.1.1.1', '8.8.8.8'],
      })
    }

    const noDns = buildDefinition({
      mode: 'static',
      address: '192.168.1.50',
      prefixLength: '24',
      gateway: '',
      dns: '   ',
    })
    expect(noDns.ok).toBe(true)
    if (noDns.ok) expect(noDns.definition.ipv4?.dns).toBeNull()
    if (noDns.ok) expect(noDns.definition.ipv4?.gateway).toBe('')
  })

  it('坏输入给出能读的原因，且不悄悄改成合法值', () => {
    const cases: Array<[ApplyFormInput, string]> = [
      [{ mode: 'static', address: 'nope', prefixLength: '24', gateway: '', dns: '' }, '点分十进制'],
      [{ mode: 'static', address: '192.168.1.50', prefixLength: '', gateway: '', dns: '' }, '1—32'],
      [{ mode: 'static', address: '192.168.1.50', prefixLength: '0', gateway: '', dns: '' }, '1—32'],
      [{ mode: 'static', address: '192.168.1.50', prefixLength: '33', gateway: '', dns: '' }, '1—32'],
      [{ mode: 'static', address: '192.168.1.50', prefixLength: '24x', gateway: '', dns: '' }, '1—32'],
      [{ mode: 'static', address: '192.168.1.50', prefixLength: '32', gateway: '192.168.1.1', dns: '' }, '32 时不能带网关'],
      [{ mode: 'static', address: '192.168.1.50', prefixLength: '24', gateway: '10.0.0.1', dns: '' }, '网段'],
      [{ mode: 'static', address: '192.168.1.50', prefixLength: '24', gateway: 'gw', dns: '' }, '点分十进制'],
      [{ mode: 'static', address: '192.168.1.50', prefixLength: '24', gateway: '', dns: '1.1.1.1 2.2.2.2 3.3.3.3 4.4.4.4' }, '最多三个'],
      [{ mode: 'static', address: '192.168.1.50', prefixLength: '24', gateway: '', dns: 'dns.example' }, '不是点分十进制'],
    ]
    for (const [input, keyword] of cases) {
      const built = buildDefinition(input)
      expect(built.ok).toBe(false)
      if (!built.ok) expect(built.error).toContain(keyword)
    }
  })

  it('拿不到 config_hash 或不可改时不拼参数', () => {
    const noHash = configState()
    delete noHash.config_hash
    const missing = buildApplyParams(noHash, EMPTY_APPLY_FORM)
    expect(missing.ok).toBe(false)

    const notEditable = buildApplyParams(
      configState({ capability: { backend: 'unknown', editable: false, reasons: ['有网桥'] } }),
      EMPTY_APPLY_FORM,
    )
    expect(notEditable.ok).toBe(false)
    if (!notEditable.ok) expect(notEditable.error).toContain('不能由面板修改')
  })

  it('拼出来的参数带这一份 config 的指纹与网卡名', () => {
    const built = buildApplyParams(configState(), {
      mode: 'static',
      address: '192.168.1.50',
      prefixLength: '24',
      gateway: '192.168.1.1',
      dns: '',
    })
    expect(built.ok).toBe(true)
    if (built.ok) {
      expect(built.params.interface_id).toBe('ens4')
      expect(built.params.expected_config_hash).toBe('sha256:abc123')
      expect(built.params.definition.mode).toBe('static')
    }
  })

  it('确认与回滚的参数只有网卡名与回滚标识', () => {
    expect(buildRollbackParams(rollbackView())).toEqual({
      interface_id: 'ens4',
      token: 'nrb_0123456789abcdef0123456789abcdef',
    })
  })

  it('倒计时按 deadline 现算，已过期是 0 不是负数', () => {
    const now = new Date('2026-10-26T12:00:30Z')
    expect(rollbackSecondsLeft('2026-10-26T12:02:00Z', now)).toBe(90)
    expect(rollbackSecondsLeft('2026-10-26T11:59:00Z', now)).toBe(0)
    // 读不懂的时刻按 0 处理：宁可说"已到期"，不编一个数字出来。
    expect(rollbackSecondsLeft('不是时间', now)).toBe(0)
  })

  it('倒计时文案', () => {
    expect(countdownText(45)).toBe('45 秒')
    expect(countdownText(87)).toBe('1 分 27 秒')
    expect(countdownText(120)).toBe('2 分')
    expect(countdownText(0)).toBe('已到期')
    expect(countdownText(-3)).toBe('已到期')
  })

  it('待确认记录的四种走向分开说', () => {
    expect(pendingHeadline(rollbackView(), 90)).toContain('还有 1 分 30 秒')
    expect(pendingHeadline(rollbackView(), 0)).toContain('已到期')
    expect(pendingHeadline(rollbackView({ state: 'confirmed' }), 0)).toContain('已确认保留')
    expect(pendingHeadline(rollbackView({ state: 'rolled_back' }), 0)).toContain('已退回')
    expect(pendingHeadline(rollbackView({ state: 'failed' }), 0)).toContain('没有走完')

    const dhcp = rollbackView({ mode: 'dhcp' })
    delete dhcp.candidate_address
    expect(pendingHeadline(dhcp, 90)).toContain('新配置')
    expect(pendingHeadline(dhcp, 90)).not.toContain('地址')
  })

  it('本机地址文案：有地址说地址，DHCP 说 DHCP，读不到就说读不到', () => {
    expect(currentAddressText(configState())).toBe('192.168.1.10/24')
    const dhcp = configState({ mode: 'dhcp' })
    delete dhcp.ipv4
    expect(currentAddressText(dhcp)).toContain('DHCP')
    const unknown = configState({ mode: 'unknown' })
    delete unknown.ipv4
    expect(currentAddressText(unknown)).toContain('没有读到地址')
  })

  it('config 查询打到 /network/config', async () => {
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
    const res = await fetchNetworkConfig()
    expect(calls[0]).toContain('/network/config')
    expect(res.data.config_hash).toBe('sha256:abc123')
  })
})
