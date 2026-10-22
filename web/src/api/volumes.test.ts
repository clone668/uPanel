// 受管卷 DTO 的纯逻辑测试（P07.5）。
//
// 这一层的价值全在“别替服务端下结论”：
//   - 挂载名不合规要明确拒绝，不能被静默改成别的字符（挂载点由它派生）；
//   - 改挂载选项只提交真的改过的项（漏传 ≠ 关掉）；
//   - observed_state 与 desired_state 是两件事，界面不能拿前者当后者用；
//   - 纳入管理的候选只是界面筛选，没挂载、非系统盘、已有文件系统。

import { afterEach, describe, expect, it, vi } from 'vitest'

import {
  MAX_DISPLAY_NAME_LENGTH,
  MAX_SLUG_LENGTH,
  SLUG_PATTERN,
  VOLUME_ACTIONS,
  VOLUME_ACTION_LABELS,
  VOLUME_DESIRED_STATES,
  VOLUME_DESIRED_STATE_LABELS,
  VOLUME_ID_PATTERN,
  VOLUME_OBSERVED_STATES,
  VOLUME_OBSERVED_STATE_LABELS,
  VOLUME_OBSERVED_STATE_TYPES,
  VOLUME_ROOT,
  busySummary,
  candidateFilesystemLabel,
  configureDiff,
  configureParams,
  desiredMismatch,
  fetchVolume,
  fetchVolumeBlockers,
  fetchVolumes,
  forgetParams,
  importCandidates,
  importParams,
  isManagedFilesystem,
  isValidSlug,
  mountParams,
  mountPathForSlug,
  normalizeSlug,
  observedStateHint,
  unmountParams,
  volumeActionNeedsReauth,
  volumeOptions,
  type Volume,
  type VolumeBlockers,
} from './volumes'

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
    capacity: null,
    dependencies: [],
    updated_at: '2026-10-21T09:31:45Z',
    ...overrides,
  }
}

function blockers(overrides: Partial<VolumeBlockers> = {}): VolumeBlockers {
  return {
    volume_id: 'vol_0123456789abcdef0123456789abcdef',
    mounted: true,
    mounts: ['/srv/upanel/volumes/data'],
    processes: [],
    holders: [],
    swap_active: false,
    apps: [],
    shares: [],
    backups: [],
    busy: false,
    checked_at: '2026-10-21T09:31:45Z',
    ...overrides,
  }
}

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('封闭集合都有中文标签', () => {
  it('observed_state 六个状态各有标签与颜色，且不重复', () => {
    expect(VOLUME_OBSERVED_STATES).toHaveLength(6)
    for (const state of VOLUME_OBSERVED_STATES) {
      expect(VOLUME_OBSERVED_STATE_LABELS[state]).toBeTruthy()
      expect(VOLUME_OBSERVED_STATE_TYPES[state]).toBeTruthy()
    }
    const labels = VOLUME_OBSERVED_STATES.map((state) => VOLUME_OBSERVED_STATE_LABELS[state])
    expect(new Set(labels).size).toBe(labels.length)
  })

  it('desired_state 只有 mounted/unmounted 两种，都有标签', () => {
    expect([...VOLUME_DESIRED_STATES]).toEqual(['mounted', 'unmounted'])
    for (const state of VOLUME_DESIRED_STATES) {
      expect(VOLUME_DESIRED_STATE_LABELS[state]).toBeTruthy()
    }
  })

  it('五个卷动作都有中文标签', () => {
    for (const action of Object.values(VOLUME_ACTIONS)) {
      expect(VOLUME_ACTION_LABELS[action]).toBeTruthy()
    }
  })
})

describe('挂载名（slug）', () => {
  it('规范化只做大小写与分隔符，不猜意图', () => {
    expect(normalizeSlug('  Data_Disk  ')).toBe('data-disk')
    expect(normalizeSlug('my  volume')).toBe('my-volume')
    expect(normalizeSlug('--edge--')).toBe('edge')
    // 中文不会被“翻译”成别的字符：规范化之后仍然不合规，由调用方报错。
    expect(isValidSlug(normalizeSlug('数据盘'))).toBe(false)
  })

  it('合不合规按契约的模式判，不做模糊放行', () => {
    expect(isValidSlug('data')).toBe(true)
    expect(isValidSlug('data-2')).toBe(true)
    expect(isValidSlug('')).toBe(false)
    expect(isValidSlug('-data')).toBe(false)
    expect(isValidSlug('data-')).toBe(false)
    expect(isValidSlug('data--disk')).toBe(false)
    expect(isValidSlug('Data')).toBe(false)
    expect(isValidSlug('data/disk')).toBe(false)
    expect(isValidSlug('a'.repeat(MAX_SLUG_LENGTH))).toBe(true)
    expect(isValidSlug('a'.repeat(MAX_SLUG_LENGTH + 1))).toBe(false)
    expect(SLUG_PATTERN.test('data')).toBe(true)
  })

  it('挂载点由挂载名派生，页面没有自己填路径的入口', () => {
    expect(mountPathForSlug('data')).toBe(`${VOLUME_ROOT}/data`)
  })
})

describe('动作参数', () => {
  it('纳入管理：display_name 空着就不发，交给服务端用 slug 补', () => {
    expect(importParams({ deviceId: 'sdb1', slug: 'data' })).toEqual({
      device_id: 'sdb1',
      slug: 'data',
    })
    expect(importParams({ deviceId: 'sdb1', slug: 'data', displayName: '   ' })).toEqual({
      device_id: 'sdb1',
      slug: 'data',
    })
    expect(importParams({ deviceId: 'sdb1', slug: 'data', displayName: ' 数据盘 ' }).display_name).toBe(
      '数据盘',
    )
  })

  it('挂载/卸载/退出管理都带上页面看到的版本号', () => {
    const target = { id: 'vol_0123456789abcdef0123456789abcdef', version: '7' }
    expect(mountParams(target)).toEqual({ volume_id: target.id, version: '7' })
    expect(unmountParams(target)).toEqual({ volume_id: target.id, version: '7' })
    expect(forgetParams(target)).toEqual({ volume_id: target.id, version: '7' })
  })

  it('改配置只提交真的改过的项：没动的一项不能被当成显式 false', () => {
    const params = configureParams({
      volume: { id: 'vol_0123456789abcdef0123456789abcdef', version: '2' },
      readOnly: true,
    })
    expect(params).toEqual({
      volume_id: 'vol_0123456789abcdef0123456789abcdef',
      version: '2',
      read_only: true,
    })
    // automount/nodev/nosuid 一个都不在载荷里 —— 服务端按“不改”处理。
    expect('automount' in params).toBe(false)
    expect('nodev' in params).toBe(false)
    expect('nosuid' in params).toBe(false)
  })

  it('差异计算：一样就不产生改动，false 与缺省分得开', () => {
    const current = volume({ automount: true, read_only: false, nodev: true, nosuid: false })
    expect(
      configureDiff(current, { automount: true, readOnly: false, nodev: true, nosuid: false }),
    ).toEqual({})
    expect(
      configureDiff(current, { automount: false, readOnly: false, nodev: true, nosuid: true }),
    ).toEqual({ automount: false, nosuid: true })
    // 显式改成 false 是一个真实改动，必须出现在差异里。
    expect(
      configureDiff(current, { automount: true, readOnly: false, nodev: false, nosuid: false }),
    ).toEqual({ nodev: false })
  })
})

describe('状态提示不是判决', () => {
  it('mounted/unmounted 没有额外提示，其余四种都要说清楚', () => {
    expect(observedStateHint(volume({ observed_state: 'mounted' }))).toBeNull()
    expect(observedStateHint(volume({ observed_state: 'unmounted' }))).toBeNull()
    expect(observedStateHint(volume({ observed_state: 'missing' }))).toContain('不在这台机器上')
    expect(observedStateHint(volume({ observed_state: 'mismatch' }))).toContain('UUID')
    expect(observedStateHint(volume({ observed_state: 'unknown' }))).toContain('还没确认')
    expect(
      observedStateHint(volume({ observed_state: 'error', observed_error: 'blkid 超时' })),
    ).toBe('blkid 超时')
    // 没有 observed_error 时不能显示空字符串，要有兜底说明。
    expect(observedStateHint(volume({ observed_state: 'error' }))).toBeTruthy()
    expect(observedStateHint(volume({ observed_state: 'error', observed_error: undefined }))).toBe(
      '读取这块盘的状态失败，原因见详情。',
    )
  })

  it('期望与实际不一致只在两种组合下成立', () => {
    expect(desiredMismatch(volume({ desired_state: 'mounted', observed_state: 'mounted' }))).toBe(false)
    expect(desiredMismatch(volume({ desired_state: 'mounted', observed_state: 'unmounted' }))).toBe(true)
    expect(desiredMismatch(volume({ desired_state: 'mounted', observed_state: 'unknown' }))).toBe(true)
    expect(desiredMismatch(volume({ desired_state: 'unmounted', observed_state: 'unmounted' }))).toBe(false)
    expect(desiredMismatch(volume({ desired_state: 'unmounted', observed_state: 'mounted' }))).toBe(true)
    expect(desiredMismatch(volume({ desired_state: 'unmounted', observed_state: 'missing' }))).toBe(false)
  })

  it('挂载选项按服务端渲染 fstab 的顺序拼出来', () => {
    expect(volumeOptions(volume())).toEqual(['rw', 'nofail', 'x-systemd.device-timeout=30'])
    expect(
      volumeOptions(volume({ read_only: true, nodev: true, nosuid: true, automount: false })),
    ).toEqual(['ro', 'nofail', 'x-systemd.device-timeout=30', 'nodev', 'nosuid', 'noauto'])
  })

  it('占用摘要说人话，且“忙但列不出来”不画成“没有占用”', () => {
    expect(busySummary(blockers())).toBe('没有占用')
    expect(busySummary(blockers({ busy: true }))).toBe('判定为忙，但没有列出具体占用者')
    expect(
      busySummary(
        blockers({
          busy: true,
          processes: [{ pid: '1234', comm: 'nginx' }],
          holders: ['/dev/mapper/crypt'],
          swap_active: true,
        }),
      ),
    ).toBe('1 个进程打开着它，被 /dev/mapper/crypt 依赖，它是活动交换区')
  })
})

describe('纳入管理的候选', () => {
  it('只放已分区、没挂载、非系统盘的分区', () => {
    const devices = [
      { id: 'sdb1', kind: 'partition', size_bytes: '100', filesystem: { type: 'ext4', uuid: 'u1', mounts: [] }, protection: { system_disk: false } },
      // 整盘不行：得先建分区。
      { id: 'sdb', kind: 'disk', size_bytes: '200', filesystem: { type: null, uuid: null, mounts: [] }, protection: { system_disk: false } },
      // 系统盘不行。
      { id: 'sda1', kind: 'partition', size_bytes: '300', filesystem: { type: 'ext4', uuid: 'u2', mounts: [] }, protection: { system_disk: true } },
      // 没有 filesystem 字段不是“不合格”，是“还没挂载所以读不到”——这种人正是要导入的人。
      { id: 'sdc1', kind: 'partition', size_bytes: '400', filesystem: null },
      // 类型读不到同理：列出来，由服务端导入前直读确认。
      { id: 'sdd1', kind: 'partition', size_bytes: '500', filesystem: { type: null, uuid: null, mounts: [] } },
      // 已经挂着的先让人自己看清情况。
      { id: 'sde1', kind: 'partition', size_bytes: '600', filesystem: { type: 'xfs', uuid: 'u3', mounts: [{ path: '/mnt/x' }] } },
      // 类型读得出但不在托管范围（ntfs）：列进去只会换一条拒绝。
      { id: 'sdf1', kind: 'partition', size_bytes: '700', filesystem: { type: 'ntfs', uuid: 'u4', mounts: [] } },
    ]
    expect(importCandidates(devices).map((device) => device.id)).toEqual([
      'sdb1',
      'sdc1',
      'sdd1',
    ])
  })

  it('全都不合格时返回空数组（页面据此说明为什么空）', () => {
    expect(importCandidates([])).toEqual([])
    expect(
      importCandidates([{ id: 'sdb', kind: 'disk', size_bytes: '1', filesystem: null }]),
    ).toEqual([])
  })

  it('“类型读得到”才拿来判托管范围，读不到不算不合格', () => {
    expect(isManagedFilesystem('ext4')).toBe(true)
    expect(isManagedFilesystem('xfs')).toBe(true)
    expect(isManagedFilesystem('ntfs')).toBe(false)
    expect(isManagedFilesystem('vfat')).toBe(false)
  })

  it('候选的说明文字分得清“没类型”和“有类型没卷标”', () => {
    expect(candidateFilesystemLabel({ id: 'a', kind: 'partition', size_bytes: '1', filesystem: null })).toBe(
      '文件系统未挂载时读不到',
    )
    expect(
      candidateFilesystemLabel({
        id: 'a',
        kind: 'partition',
        size_bytes: '1',
        filesystem: { type: null, uuid: null, mounts: [] },
      }),
    ).toBe('文件系统未挂载时读不到')
    expect(
      candidateFilesystemLabel({
        id: 'a',
        kind: 'partition',
        size_bytes: '1',
        filesystem: { type: 'ext4', uuid: 'u', mounts: [] },
      }),
    ).toBe('ext4')
    expect(
      candidateFilesystemLabel({
        id: 'a',
        kind: 'partition',
        size_bytes: '1',
        filesystem: { type: 'xfs', uuid: 'u', label: 'logs', mounts: [] },
      }),
    ).toBe('xfs · logs')
  })
})

describe('再认证策略', () => {
  it('导入与挂载不要密码，卸载/改配置/退出管理要', () => {
    expect(volumeActionNeedsReauth(VOLUME_ACTIONS.import)).toBe(false)
    expect(volumeActionNeedsReauth(VOLUME_ACTIONS.mount)).toBe(false)
    expect(volumeActionNeedsReauth(VOLUME_ACTIONS.unmount)).toBe(true)
    expect(volumeActionNeedsReauth(VOLUME_ACTIONS.configure)).toBe(true)
    expect(volumeActionNeedsReauth(VOLUME_ACTIONS.forget)).toBe(true)
  })
})

describe('卷 id 只作为路径参数', () => {
  it('合法形状通过，非法形状在发请求之前就被拒', async () => {
    expect(VOLUME_ID_PATTERN.test('vol_0123456789abcdef0123456789abcdef')).toBe(true)
    expect(VOLUME_ID_PATTERN.test('vol_0123456789ABCDEF0123456789abcdef')).toBe(false)
    expect(VOLUME_ID_PATTERN.test('vol_0123')).toBe(false)
    expect(VOLUME_ID_PATTERN.test('../../etc/shadow')).toBe(false)

    const mock = vi.fn()
    vi.stubGlobal('fetch', mock)
    await expect(fetchVolume('../../etc/shadow')).rejects.toThrow(RangeError)
    await expect(fetchVolumeBlockers('vol_zzz')).rejects.toThrow(RangeError)
    expect(mock).not.toHaveBeenCalled()
  })
})

describe('查询函数走统一客户端', () => {
  function jsonResponse(body: unknown, status = 200): Response {
    return new Response(JSON.stringify(body), {
      status,
      headers: { 'Content-Type': 'application/json', 'X-Request-ID': 'rid-volumes' },
    })
  }

  it('列表、详情、占用各自打到约定路径', async () => {
    const mock = vi.fn(() => Promise.resolve(jsonResponse({ data: {}, request_id: 'rid-volumes' })))
    vi.stubGlobal('fetch', mock)

    await fetchVolumes()
    await fetchVolume('vol_0123456789abcdef0123456789abcdef')
    await fetchVolumeBlockers('vol_0123456789abcdef0123456789abcdef')

    const urls = (mock.mock.calls as unknown as [string][]).map((call) => String(call[0]))
    expect(urls[0]).toMatch(/\/api\/v1\/storage\/volumes$/)
    expect(urls[1]).toMatch(/\/api\/v1\/storage\/volumes\/vol_0123456789abcdef0123456789abcdef$/)
    expect(urls[2]).toMatch(
      /\/api\/v1\/storage\/volumes\/vol_0123456789abcdef0123456789abcdef\/blockers$/,
    )
  })
})

describe('显示名上限', () => {
  it('上限与契约一致（64）', () => {
    expect(MAX_DISPLAY_NAME_LENGTH).toBe(64)
  })
})
