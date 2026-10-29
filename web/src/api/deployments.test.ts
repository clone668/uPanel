// 受管部署 API 层（P15.4）。
//
// 这一层的价值全在“别替服务端下结论、别替它猜”：
//  1. **两张表两个问题**：管理面的 `unit_state` 只说 systemd 怎么看这条包装单元，
//     容器在不在跑是观测面（§8.3）的事，两边不一致要同时显示，不能合成一句；
//  2. **提示不是判决**：`updateHint` 只解释已知拦路条件（没有定义记录 / 盘上被改过），
//     真正的判决在预检，所以它返回 null 也只代表“没有已知的拦路条件”；
//  3. **危险选项没有输入框**：定义的形状里没有 privileged / compose_yaml /
//     network_mode / cap_add，草稿也就变不出来——这一条是断言出来的，不是口头承诺；
//  4. **名字在发请求之前就被拒**：它是 URL 路径段，`../` 不该由服务端的路径规范化兜底。

import { afterEach, describe, expect, it, vi } from 'vitest'

import {
  BLOCKED_CONTAINER_PATHS,
  DEPLOYMENT_NAME_PATTERN,
  MAX_CPU_LIMIT,
  MAX_DEPLOYMENT_NAME_LENGTH,
  MAX_MOUNTS,
  MEBIBYTE,
  MIN_CPU_LIMIT,
  applyParams,
  definitionFromDraft,
  deploymentPhase,
  deploymentPhaseDetail,
  draftFromDefinition,
  emptyDeploymentDraft,
  getManagedDeployment,
  isValidDeploymentName,
  listManagedDeployments,
  removeHint,
  removeParams,
  shortDigest,
  shortVersion,
  updateHint,
  updateParams,
  validateDeploymentDraft,
  type ManagedDeployment,
  type ManagedDeploymentDefinition,
  type ManagedDeploymentUnitState,
} from './deployments'

function unitState(overrides: Partial<ManagedDeploymentUnitState> = {}): ManagedDeploymentUnitState {
  return { known: true, enabled: true, active: true, ...overrides }
}

function deployment(overrides: Partial<ManagedDeployment> = {}): ManagedDeployment {
  return {
    name: 'webapp',
    deployment_id: 'dep_0123456789abcdef0123456789abcdef',
    project: 'webapp',
    unit: 'upanel-deploy-webapp.service',
    unit_path: '/etc/upanel/deploy/upanel-deploy-webapp.service',
    config_path: '/etc/upanel/deploy/webapp.compose.yaml',
    config_present: true,
    config_digest: 'sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa',
    version: 'sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa',
    previous_config_present: false,
    record_present: true,
    record: {
      schema: 1,
      deployment_id: 'dep_0123456789abcdef0123456789abcdef',
      name: 'webapp',
      definition: {
        name: 'webapp',
        image_reference: 'nginx:1.27',
        container_port: 80,
        host_bind_address: '127.0.0.1',
        host_port: 8080,
        resource_limits: { memory_limit_bytes: 512 * MEBIBYTE, cpu_limit: '0.5' },
      },
      config_digest: 'sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa',
      image: {
        requested: 'nginx:1.27',
        digest: 'sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb',
        config_digest: 'sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa',
        resolved_at: '2026-10-29T10:00:00Z',
      },
      updated_at: '2026-10-29T10:00:00Z',
    },
    config_drift: false,
    unit_state: unitState(),
    ...overrides,
  }
}

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('阶段只读管理面的 unit_state', () => {
  it('三种阶段各自成立，且缺失读数时必须说清原因', () => {
    expect(deploymentPhase(deployment())).toBe('running')
    expect(deploymentPhase(deployment({ unit_state: unitState({ active: false }) }))).toBe('stopped')
    expect(
      deploymentPhase(deployment({ unit_state: unitState({ known: false, enabled: false, active: false }) })),
    ).toBe('unit_missing')

    // 容器 running 不会把管理面说成 running：这里只读 unit_state。
    const stopped = deployment({ unit_state: unitState({ active: false, enabled: false }) })
    expect(deploymentPhaseDetail(stopped)).toContain('没在跑')
    expect(deploymentPhaseDetail(stopped)).toContain('开机不会自动启动')

    const unknown = deployment({
      unit_state: unitState({
        known: false,
        enabled: false,
        active: false,
        reason: '单元文件不在磁盘上（没有可读的单元状态）',
      }),
    })
    expect(deploymentPhaseDetail(unknown)).toBe('单元文件不在磁盘上（没有可读的单元状态）')

    // reason 为空时也要给出两条可能原因，而不是一句“未知”。
    const quiet = deployment({ unit_state: unitState({ known: false, enabled: false, active: false }) })
    expect(deploymentPhaseDetail(quiet)).toContain('单元文件不在磁盘上')
    expect(deploymentPhaseDetail(quiet)).toContain('systemctl')
  })
})

describe('版本与 digest 的短形式', () => {
  it('sha256 截前 12 位，unit-only / absent 原样，空串说未知', () => {
    expect(shortVersion(`sha256:${'a'.repeat(64)}`)).toBe(`sha256:${'a'.repeat(12)}…`)
    expect(shortVersion('unit-only')).toBe('unit-only')
    expect(shortVersion('absent')).toBe('absent')
    expect(shortVersion('')).toBe('未知')
    expect(shortDigest(`sha256:${'b'.repeat(12)}`)).toBe('b'.repeat(12))
    expect(shortDigest(`sha256:${'b'.repeat(64)}`)).toBe(`${'b'.repeat(12)}…`)
    expect(shortDigest('')).toBe('没有读数')
    expect(shortDigest(undefined)).toBe('没有读数')
  })
})

describe('更新/删除的提示只说明已知条件', () => {
  it('没有定义记录与盘上被改过各有自己的话', () => {
    expect(updateHint(deployment())).toBeNull()

    const noRecord = deployment({ record_present: false })
    expect(updateHint(noRecord)).toContain('没有定义记录')
    expect(updateHint(noRecord)).toContain('删除后重新部署')

    const brokenRecord = deployment({ record_present: false, record_error: '不是合法 JSON' })
    expect(updateHint(brokenRecord)).toContain('不是合法 JSON')

    const drifted = deployment({ config_drift: true })
    expect(updateHint(drifted)).toContain('record_mismatch')

    expect(removeHint(deployment())).toBeNull()
    expect(removeHint(deployment({ version: '' }))).not.toBeNull()
  })
})

describe('名字在发请求之前就被拒', () => {
  it('非法形状抛 RangeError，且一个请求都没发', async () => {
    expect(DEPLOYMENT_NAME_PATTERN.test('Web-App')).toBe(false)
    expect(isValidDeploymentName(' webapp ')).toBe(true)
    expect(isValidDeploymentName('1webapp')).toBe(false)
    expect(isValidDeploymentName(`a${'b'.repeat(MAX_DEPLOYMENT_NAME_LENGTH)}`)).toBe(false)

    const mock = vi.fn()
    vi.stubGlobal('fetch', mock)
    await expect(getManagedDeployment('../../etc/shadow')).rejects.toThrow(RangeError)
    await expect(getManagedDeployment('Web App')).rejects.toThrow(RangeError)
    expect(mock).not.toHaveBeenCalled()
  })
})

describe('查询函数走统一客户端', () => {
  function jsonResponse(body: unknown, status = 200): Response {
    return new Response(JSON.stringify(body), {
      status,
      headers: { 'Content-Type': 'application/json', 'X-Request-ID': 'rid-deployments' },
    })
  }

  it('列表与详情各自打到约定的管理面路径', async () => {
    const mock = vi.fn(() =>
      Promise.resolve(
        jsonResponse({ data: { deployments: [], truncated: false }, request_id: 'rid-deployments' }),
      ),
    )
    vi.stubGlobal('fetch', mock)

    await listManagedDeployments()
    await getManagedDeployment('webapp')

    const urls = (mock.mock.calls as unknown as [string][]).map((call) => String(call[0]))
    expect(urls[0]).toMatch(/\/api\/v1\/deployments\/managed$/)
    expect(urls[1]).toMatch(/\/api\/v1\/deployments\/managed\/webapp$/)
  })
})

describe('预检参数', () => {
  it('apply 明确是“新部署”：deployment_id 为 null、version 为空串', () => {
    const definition = definitionFromDraft({ ...emptyDeploymentDraft(), name: 'webapp', image_reference: 'nginx:1.27' })
    const params = applyParams(definition)
    expect(params['deployment_id']).toBeNull()
    expect(params['version']).toBe('')
    expect(params['definition']).toBe(definition)
  })

  it('update 带上版本令牌，备份策略没选就给 null（与“没给这个键”区分开）', () => {
    const dep = deployment()
    const noBackup = updateParams(dep, ' nginx:1.28 ')
    expect(noBackup['deployment_id']).toBe(dep.deployment_id)
    expect(noBackup['version']).toBe(dep.version)
    expect(noBackup['image_reference']).toBe('nginx:1.28')
    expect(noBackup['backup_policy_id']).toBeNull()

    const withBackup = updateParams(dep, 'nginx:1.28', 'bkp_0123456789abcdef')
    expect(withBackup['backup_policy_id']).toBe('bkp_0123456789abcdef')

    expect(() => updateParams(dep, '   ')).toThrow(RangeError)
    expect(() => updateParams(deployment({ version: '' }), 'nginx:1.28')).toThrow(RangeError)
  })

  it('remove 只带标识与版本，没有任何“顺便清理”的开关', () => {
    const dep = deployment()
    expect(removeParams(dep)).toEqual({ deployment_id: dep.deployment_id, version: dep.version })
    expect(() => removeParams(deployment({ version: '' }))).toThrow(RangeError)
  })
})

describe('定义是闭集', () => {
  it('草稿变出来的定义里没有 privileged / compose_yaml / network_mode / cap_add', () => {
    const definition = definitionFromDraft({
      ...emptyDeploymentDraft(),
      name: 'webapp',
      image_reference: 'nginx:1.27',
    })
    const keys = Object.keys(definition)
    for (const forbidden of ['privileged', 'compose_yaml', 'network_mode', 'cap_add', 'devices', 'user']) {
      expect(keys).not.toContain(forbidden)
    }
    // 十个字段的闭集里，这次填了六个（environment/mounts 为空时不出现）。
    expect(keys.sort()).toEqual(
      ['container_port', 'health_check', 'host_bind_address', 'host_port', 'image_reference', 'name', 'resource_limits', 'volume_dependencies'].sort(),
    )
  })

  it('内存以 MiB 输入、以字节落盘；禁用探测时只留 enabled:false', () => {
    const definition = definitionFromDraft({
      ...emptyDeploymentDraft(),
      name: 'webapp',
      image_reference: 'nginx:1.27',
      memory_limit_mib: 512,
      cpu_limit: '0.50',
    })
    expect(definition.resource_limits.memory_limit_bytes).toBe(512 * MEBIBYTE)
    expect(definition.resource_limits.cpu_limit).toBe('0.50')
    expect(definition.health_check).toEqual({ enabled: false })
  })

  it('秘密引用与公开值二选一；空行不提交', () => {
    const draft = {
      ...emptyDeploymentDraft(),
      name: 'webapp',
      image_reference: 'nginx:1.27',
      environment: [
        { name: 'MODE', kind: 'value' as const, value: 'prod', secret_ref: '' },
        { name: 'DB_PASSWORD', kind: 'secret' as const, value: '', secret_ref: 'sec_0123456789abcdef0123456789abcdef' },
        { name: '', kind: 'value' as const, value: '', secret_ref: '' },
      ],
    }
    expect(validateDeploymentDraft(draft)).toBeNull()
    const definition = definitionFromDraft(draft)
    expect(definition.environment).toEqual([
      { name: 'MODE', value: 'prod' },
      { name: 'DB_PASSWORD', secret_ref: 'sec_0123456789abcdef0123456789abcdef' },
    ])
  })
})

describe('草稿校验', () => {
  function draftWith(overrides: Partial<ReturnType<typeof emptyDeploymentDraft>>) {
    return { ...emptyDeploymentDraft(), name: 'webapp', image_reference: 'nginx:1.27', ...overrides }
  }

  it('基线草稿通过', () => {
    expect(validateDeploymentDraft(draftWith({}))).toBeNull()
  })

  it('名字、镜像、端口、地址逐条拒，并给出行内字段名', () => {
    expect(validateDeploymentDraft(draftWith({ name: '' }))?.field).toBe('name')
    expect(validateDeploymentDraft(draftWith({ name: 'Web' }))?.field).toBe('name')
    expect(validateDeploymentDraft(draftWith({ image_reference: '' }))?.field).toBe('image_reference')
    expect(validateDeploymentDraft(draftWith({ image_reference: 'nginx 1.27' }))?.field).toBe('image_reference')
    expect(validateDeploymentDraft(draftWith({ container_port: 0 }))?.field).toBe('container_port')
    expect(validateDeploymentDraft(draftWith({ host_port: 70000 }))?.field).toBe('host_port')
    expect(validateDeploymentDraft(draftWith({ host_bind_address: 'example.com' }))?.field).toBe(
      'host_bind_address',
    )
    expect(validateDeploymentDraft(draftWith({ host_bind_address: '192.0.2.300' }))?.field).toBe(
      'host_bind_address',
    )
    // 回环与 IPv6 字面量都放过：能不能绑由预检读本机地址表决定。
    expect(validateDeploymentDraft(draftWith({ host_bind_address: '127.0.0.1' }))).toBeNull()
    expect(validateDeploymentDraft(draftWith({ host_bind_address: '::1' }))).toBeNull()
  })

  it('环境变量：标识符、重复、秘密引用形状', () => {
    const env = (overrides: Record<string, unknown>) => ({
      name: 'FOO',
      kind: 'value' as const,
      value: '',
      secret_ref: '',
      ...overrides,
    })
    expect(
      validateDeploymentDraft(draftWith({ environment: [env({ name: '1BAD' })] as never }))?.field,
    ).toBe('environment[0].name')
    expect(
      validateDeploymentDraft(draftWith({ environment: [env({}), env({})] as never }))?.message,
    ).toContain('重复')
    expect(
      validateDeploymentDraft(draftWith({ environment: [env({ kind: 'secret' })] as never }))?.field,
    ).toBe('environment[0].secret_ref')
    // 明文不能顶替秘密引用：这条路径上填的要么是公开值，要么是一条 sec_ 引用。
    expect(
      validateDeploymentDraft(
        draftWith({ environment: [env({ kind: 'secret', secret_ref: 'hunter2' })] as never }),
      )?.field,
    ).toBe('environment[0].secret_ref')
  })

  it('挂载：宿主路径表达不出来，容器路径有禁则', () => {
    const mount = (overrides: Record<string, unknown>) => ({
      source_root_id: 'appdata',
      relative_path: 'db',
      container_path: '/data',
      read_only: false,
      ...overrides,
    })
    expect(validateDeploymentDraft(draftWith({ mounts: [mount({})] as never }))).toBeNull()
    expect(
      validateDeploymentDraft(draftWith({ mounts: [mount({ source_root_id: '/etc' })] as never }))?.field,
    ).toBe('mounts[0].source_root_id')
    expect(
      validateDeploymentDraft(draftWith({ mounts: [mount({ relative_path: '/abs' })] as never }))?.field,
    ).toBe('mounts[0].relative_path')
    expect(
      validateDeploymentDraft(draftWith({ mounts: [mount({ relative_path: 'a/../b' })] as never }))?.field,
    ).toBe('mounts[0].relative_path')
    expect(
      validateDeploymentDraft(draftWith({ mounts: [mount({ container_path: '/' })] as never }))?.message,
    ).toContain('根目录')
    for (const blocked of BLOCKED_CONTAINER_PATHS) {
      expect(
        validateDeploymentDraft(draftWith({ mounts: [mount({ container_path: blocked })] as never }))?.field,
        blocked,
      ).toBe('mounts[0].container_path')
    }
    // 子路径同样拒绝：/proc/cpuinfo 也是内核的。
    expect(
      validateDeploymentDraft(draftWith({ mounts: [mount({ container_path: '/proc/cpuinfo' })] as never }))
        ?.field,
    ).toBe('mounts[0].container_path')
    expect(
      validateDeploymentDraft(
        draftWith({ mounts: [mount({}), mount({ container_path: '/data' })] as never }),
      )?.message,
    ).toContain('重复')
    expect(
      validateDeploymentDraft(
        draftWith({
          mounts: Array.from({ length: MAX_MOUNTS + 1 }, (_, i) => mount({ container_path: `/d${i}` })) as never,
        }),
      )?.field,
    ).toBe('mounts')
  })

  it('资源限制：下限不是摆设，CPU 上下限与十进制形状', () => {
    expect(validateDeploymentDraft(draftWith({ memory_limit_mib: 8 }))?.field).toBe('memory_limit_mib')
    expect(validateDeploymentDraft(draftWith({ memory_limit_mib: 256 * 1024 }))?.field).toBe(
      'memory_limit_mib',
    )
    expect(validateDeploymentDraft(draftWith({ cpu_limit: '0.5核' }))?.field).toBe('cpu_limit')
    expect(validateDeploymentDraft(draftWith({ cpu_limit: String(MIN_CPU_LIMIT / 2) }))?.field).toBe('cpu_limit')
    expect(validateDeploymentDraft(draftWith({ cpu_limit: String(MAX_CPU_LIMIT + 1) }))?.field).toBe('cpu_limit')
  })

  it('探测描述：http 路径与时间窗', () => {
    const health = (overrides: Record<string, unknown>) => ({
      ...emptyDeploymentDraft().health_check,
      enabled: true,
      ...overrides,
    })
    expect(validateDeploymentDraft(draftWith({ health_check: health({}) as never }))).toBeNull()
    expect(validateDeploymentDraft(draftWith({ health_check: health({ path: 'healthz' }) as never }))?.field).toBe(
      'health_check.path',
    )
    expect(
      validateDeploymentDraft(draftWith({ health_check: health({ interval_seconds: 1 }) as never }))?.field,
    ).toBe('health_check.interval_seconds')
    expect(
      validateDeploymentDraft(draftWith({ health_check: health({ timeout_seconds: 0 }) as never }))?.field,
    ).toBe('health_check.timeout_seconds')
    expect(validateDeploymentDraft(draftWith({ health_check: health({ retries: 99 }) as never }))?.field).toBe(
      'health_check.retries',
    )
    // tcp 探测不需要路径。
    expect(
      validateDeploymentDraft(draftWith({ health_check: health({ kind: 'tcp', path: '' }) as never })),
    ).toBeNull()
    // 关掉的时候其余字段一概不看（草稿里留着上次填的也照样能存）。
    expect(
      validateDeploymentDraft(draftWith({ health_check: health({ enabled: false, path: 'x' }) as never })),
    ).toBeNull()
  })

  it('卷依赖：形状与去重', () => {
    expect(
      validateDeploymentDraft(draftWith({ volume_dependencies: ['vol_0123456789abcdef0123456789abcdef'] })),
    ).toBeNull()
    expect(validateDeploymentDraft(draftWith({ volume_dependencies: ['sdb1'] }))?.field).toBe(
      'volume_dependencies',
    )
    expect(
      validateDeploymentDraft(
        draftWith({
          volume_dependencies: [
            'vol_0123456789abcdef0123456789abcdef',
            'vol_0123456789abcdef0123456789abcdef',
          ],
        }),
      )?.message,
    ).toContain('重复')
  })
})

describe('定义 ⇄ 草稿', () => {
  it('从记录里的定义预填，再转回去是同一份东西（名称与资源不动）', () => {
    const definition: ManagedDeploymentDefinition = {
      name: 'webapp',
      image_reference: 'nginx:1.27',
      container_port: 80,
      host_bind_address: '127.0.0.1',
      host_port: 8080,
      environment: [
        { name: 'MODE', value: 'prod' },
        { name: 'DB_PASSWORD', secret_ref: 'sec_0123456789abcdef0123456789abcdef' },
      ],
      mounts: [
        { source_root_id: 'appdata', relative_path: 'db', container_path: '/data', read_only: true },
      ],
      resource_limits: { memory_limit_bytes: 512 * MEBIBYTE, cpu_limit: '0.5' },
      health_check: { enabled: true, kind: 'http', port: 0, path: '/healthz', interval_seconds: 30, timeout_seconds: 3, retries: 3 },
      volume_dependencies: ['vol_0123456789abcdef0123456789abcdef'],
    }
    const draft = draftFromDefinition(definition)
    expect(draft.environment[0]?.kind).toBe('value')
    expect(draft.environment[1]?.kind).toBe('secret')
    expect(draft.health_check.path).toBe('/healthz')

    const round = definitionFromDraft(draft)
    expect(round.name).toBe(definition.name)
    expect(round.image_reference).toBe(definition.image_reference)
    expect(round.resource_limits).toEqual(definition.resource_limits)
    expect(round.environment).toEqual(definition.environment)
    expect(round.mounts).toEqual(definition.mounts)
    expect(round.volume_dependencies).toEqual(definition.volume_dependencies)
  })
})
