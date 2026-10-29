// 备份 REST 组与动作参数的测试（P12.5）。
//
// 钉住三件事：
//   1. 请求形态与契约一致（路径、方法、幂等键、超时），因为这不是"风格"问题——
//      视图拿错了端点或少了幂等键，代价是一次重复的仓库登记或一份重复的备份。
//   2. 动作参数**只带该带的字段**。`apps` 不能出现在 backup.run 里，`expected_delete_ids`
//      不能出现在 backup.prune 里：前者会扩大策略的授权范围，后者会让人以为那份名单
//      是来自另一处的证据，而它其实是预检算出来的执行状态。
//   3. 翻译函数在"读不到"与"是零"之间不混。`retentionLabel({})` 说的是"不清理"，
//      不是"没有保留策略"这种含糊话。

import { afterEach, describe, expect, it, vi } from 'vitest'

import {
  BACKUP_BACKENDS,
  BACKUP_CONSISTENCY_LABELS,
  BACKUP_INITIALIZE_ACTION,
  BACKUP_PRUNE_ACTION,
  BACKUP_RESTORE_ACTION,
  BACKUP_RUN_ACTION,
  BACKUP_VERIFY_ACTION,
  SNAPSHOT_QUERY_TIMEOUT_MS,
  backupInitializeParams,
  backupNoticeLabel,
  backupPruneParams,
  backupRestoreParams,
  backupRunParams,
  backupVerifyParams,
  createBackupRepository,
  deleteBackupPolicy,
  fetchBackupSnapshots,
  locationLabel,
  retentionLabel,
  sortSnapshotsNewestFirst,
  sourceLabel,
  updateBackupRepository,
  type BackupRepositoryInput,
  type BackupSnapshot,
} from './backup'

afterEach(() => {
  vi.unstubAllGlobals()
})

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json', 'X-Request-ID': 'rid-backup' },
  })
}

function stubFetch(): ReturnType<typeof vi.fn> {
  const mock = vi.fn(() => Promise.resolve(jsonResponse({ data: {}, request_id: 'rid' })))
  vi.stubGlobal('fetch', mock)
  return mock
}

function repositoryInput(): BackupRepositoryInput {
  return {
    name: '每日备份',
    backend: 'local',
    location: { root_id: 'root1', relative_path: 'upanel-restic' },
    password_secret_ref: 'sec_0123456789abcdef0123456789abcdef',
    credential_secret_ref: '',
    version: 0,
  }
}

describe('动作名', () => {
  it('与 CONTRACTS §5 的动作名逐字一致', () => {
    expect(BACKUP_RUN_ACTION).toBe('backup.run')
    expect(BACKUP_RESTORE_ACTION).toBe('backup.restore')
    expect(BACKUP_VERIFY_ACTION).toBe('backup.verify')
    expect(BACKUP_PRUNE_ACTION).toBe('backup.prune')
    expect(BACKUP_INITIALIZE_ACTION).toBe('backup.repository.initialize')
  })
})

describe('动作参数', () => {
  it('backup.run 只有策略与版本：apps 由服务端按策略展开', () => {
    const params = backupRunParams({ id: 'pol_x', version: 3 })
    expect(params).toEqual({ backup_policy_id: 'pol_x', version: 3 })
    expect(params).not.toHaveProperty('apps')
  })

  it('backup.prune 只有策略与版本：待删名单是预检算的，不由页面抄一遍', () => {
    const params = backupPruneParams({ id: 'pol_x', version: 4 })
    expect(params).toEqual({ backup_policy_id: 'pol_x', version: 4 })
    expect(params).not.toHaveProperty('expected_delete_ids')
  })

  it('backup.repository.initialize 带仓库 id 与版本', () => {
    expect(backupInitializeParams({ id: 'rep_x', version: 1 })).toEqual({
      repository_id: 'rep_x',
      version: 1,
    })
  })

  it('backup.restore 正好五个字段', () => {
    const params = backupRestoreParams({
      repositoryId: 'rep_x',
      snapshotId: 'abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789',
      targetRootId: 'root1',
      relativePath: 'restored',
      overwrite: false,
    })
    expect(params).toEqual({
      repository_id: 'rep_x',
      snapshot_id: 'abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789',
      target_root_id: 'root1',
      relative_path: 'restored',
      overwrite: false,
    })
    expect(Object.keys(params)).toHaveLength(5)
  })

  it('backup.verify 带抽样开关', () => {
    expect(backupVerifyParams({ repositoryId: 'rep_x', snapshotId: 'snap', sampleRestore: true })).toEqual({
      repository_id: 'rep_x',
      snapshot_id: 'snap',
      sample_restore: true,
    })
  })
})

describe('仓库写请求', () => {
  it('登记带幂等键与 Idempotency-Key 头', async () => {
    const mock = stubFetch()
    await createBackupRepository(repositoryInput(), 'key-repo-1')

    const [url, init] = mock.mock.calls[0] as unknown as [string, RequestInit]
    expect(String(url)).toMatch(/\/api\/v1\/backup\/repositories$/)
    expect(init.method).toBe('POST')
    const headers = init.headers as Record<string, string>
    expect(headers['Idempotency-Key']).toBe('key-repo-1')
    expect(JSON.parse(String(init.body))).toMatchObject({ name: '每日备份', backend: 'local' })
  })

  it('修改走 PATCH，把 version 放进请求体（乐观并发令牌）', async () => {
    const mock = stubFetch()
    await updateBackupRepository('rep_x', { ...repositoryInput(), version: 7 })

    const [url, init] = mock.mock.calls[0] as unknown as [string, RequestInit]
    expect(String(url)).toMatch(/\/api\/v1\/backup\/repositories\/rep_x$/)
    expect(init.method).toBe('PATCH')
    expect(JSON.parse(String(init.body))).toMatchObject({ version: 7 })
  })

  it('删策略走 DELETE，不带请求体', async () => {
    const mock = stubFetch()
    await deleteBackupPolicy('pol_x')

    const [url, init] = mock.mock.calls[0] as unknown as [string, RequestInit]
    expect(String(url)).toMatch(/\/api\/v1\/backup\/policies\/pol_x$/)
    expect(init.method).toBe('DELETE')
    expect(init.body).toBeUndefined()
  })
})

describe('快照查询', () => {
  it('不带策略时 query 里没有 policy_id（空串不当成过滤条件）', async () => {
    const mock = stubFetch()
    await fetchBackupSnapshots('rep_x')
    const [url] = mock.mock.calls[0] as unknown as [string]
    expect(String(url)).toContain('repository_id=rep_x')
    expect(String(url)).not.toContain('policy_id')
  })

  it('带策略时 query 里带上它', async () => {
    const mock = stubFetch()
    await fetchBackupSnapshots('rep_x', 'pol_x')
    const [url] = mock.mock.calls[0] as unknown as [string]
    expect(String(url)).toContain('repository_id=rep_x')
    expect(String(url)).toContain('policy_id=pol_x')
  })

  it('快照查询用 75 秒：这一步要跑 restic，20 秒会在前端自己掐断', async () => {
    expect(SNAPSHOT_QUERY_TIMEOUT_MS).toBe(75_000)
    const timeout = vi.spyOn(AbortSignal, 'timeout')
    stubFetch()
    await fetchBackupSnapshots('rep_x')
    expect(timeout).toHaveBeenCalledWith(SNAPSHOT_QUERY_TIMEOUT_MS)
  })
})

describe('翻译', () => {
  it('保留策略一项都没设时说的是"不清理"，不是"0 份"', () => {
    expect(retentionLabel({})).toBe('不清理（不清任何旧快照）')
    expect(retentionLabel({ keep_last: 7 })).toBe('最近 7 份')
    expect(retentionLabel({ keep_last: 3, keep_daily: 7, keep_weekly: 4 })).toBe(
      '最近 3 份、每天 7 份、每周 4 份',
    )
  })

  it('来源只说根与相对路径，不出现绝对路径', () => {
    expect(sourceLabel({ kind: 'root', root_id: 'root1', relative_path: 'srv/www' })).toBe(
      '文件根 root1 下的 /srv/www',
    )
    expect(sourceLabel({ kind: 'root', root_id: 'root1' })).toBe('文件根 root1 的整个根')
    expect(sourceLabel({ kind: 'panel_database' })).toBe('面板数据库（SQLite 在线副本）')
  })

  it('仓库位置按后端分两种说法', () => {
    expect(locationLabel({ backend: 'local', location: { root_id: 'root1', relative_path: 'bk' } })).toBe(
      '文件根 root1:/bk',
    )
    expect(
      locationLabel({
        backend: 'sftp',
        location: { host: '10.0.0.5', port: 2222, username: 'bkuser', remote_path: '/srv/bk' },
      }),
    ).toBe('bkuser@10.0.0.5:2222 /srv/bk')
  })

  it('后端闭集与标签一一对应', () => {
    expect([...BACKUP_BACKENDS]).toEqual(['local', 'sftp'])
    expect(Object.keys(BACKUP_CONSISTENCY_LABELS).sort()).toEqual(['files_only', 'stop_managed_apps'])
  })

  it('未知通知码原样返回，不编一句中文', () => {
    expect(backupNoticeLabel('PREFLIGHT_STALE')).toBe('预检已过期')
    expect(backupNoticeLabel('SOMETHING_NEW')).toBe('SOMETHING_NEW')
  })
})

describe('快照排序', () => {
  function snapshot(id: string, time: string): BackupSnapshot {
    return {
      snapshot_id: id,
      short_id: id.slice(0, 8),
      time,
      paths: [],
      tags: [],
      restore_verified_at: null,
    }
  }

  it('新→旧，且不改入参', () => {
    const input = [snapshot('a', '2026-10-20T03:00:00Z'), snapshot('b', '2026-10-22T03:00:00Z'), snapshot('c', '2026-10-21T03:00:00Z')]
    const sorted = sortSnapshotsNewestFirst(input)
    expect(sorted.map((item) => item.snapshot_id)).toEqual(['b', 'c', 'a'])
    expect(input.map((item) => item.snapshot_id)).toEqual(['a', 'b', 'c'])
  })

  it('读不出时间的排到最后，而不是被当成最新的一份', () => {
    const sorted = sortSnapshotsNewestFirst([
      snapshot('bad', 'not-a-time'),
      snapshot('good', '2026-10-22T03:00:00Z'),
    ])
    expect(sorted.map((item) => item.snapshot_id)).toEqual(['good', 'bad'])
  })
})
