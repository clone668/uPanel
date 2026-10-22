// 任务结果翻译的测试（P07.5）。
//
// 核心断言只有一条：**不 succeeded 就不许说成功**，而且 evidence 只能来自
// result.observed_state / result.data —— 字段缺失时宁可少写一行，也不能显示
// undefined 或者编一句“已完成”。

import { describe, expect, it } from 'vitest'

import type { JobStatus } from '@/api/jobs'
import { checkedViaLabel, summarizeStorageJob } from './jobresult'

type SummaryInput = Parameters<typeof summarizeStorageJob>[0]

// 契约里 result/error 是对象，但代理在某些失败路径上会给出 null（任务没跑到写结果那步）。
// 这里按“读到的就是可能为 null 的东西”造样本，翻译层必须扛得住。
function job(overrides: { status?: JobStatus; result?: unknown; error?: unknown } = {}): SummaryInput {
  return {
    status: overrides.status ?? 'succeeded',
    result: (overrides.result ?? null) as SummaryInput['result'],
    error: (overrides.error ?? null) as SummaryInput['error'],
  }
}

describe('核验来源的中文说法', () => {
  it('认得契约里的八种来源', () => {
    for (const via of [
      'fstab_reread',
      'systemctl_show',
      'findmnt',
      'topology_scan',
      'superblock',
      'signature_probe',
      'disk_bytes',
      'kernel_sysfs',
    ]) {
      expect(checkedViaLabel(via)).not.toBe(via)
    }
  })

  it('不认识的来源原样返回，不编中文', () => {
    expect(checkedViaLabel('some_new_probe')).toBe('some_new_probe')
  })
})

describe('失败与中断', () => {
  it('failed：结论是失败，带服务端的错误原文', () => {
    const summary = summarizeStorageJob(
      job({
        status: 'failed',
        error: { message: 'mkfs.ext4 返回 1', recovery_hint: '先卸载再重试' },
      }),
    )
    expect(summary.ok).toBe(false)
    expect(summary.headline).toBe('任务失败')
    expect(summary.lines).toEqual(['mkfs.ext4 返回 1'])
    expect(summary.hint).toBe('先卸载再重试')
  })

  it('interrupted：结果未知，明确要求不要当成成功', () => {
    const summary = summarizeStorageJob(job({ status: 'interrupted' }))
    expect(summary.ok).toBe(false)
    expect(summary.headline).toContain('结果未知')
    expect(summary.headline).toContain('不要当作成功')
    expect(summary.lines).toEqual([])
  })

  it('canceled、queued、running 都不算成功，且不编造证据', () => {
    for (const status of ['canceled', 'queued', 'running'] as const) {
      const summary = summarizeStorageJob(job({ status }))
      expect(summary.ok).toBe(false)
      expect(summary.headline).toBeTruthy()
      expect(summary.lines).toEqual([])
    }
  })
})

describe('成功但没证据', () => {
  it('result 为空时说“没有可展示的现场读数”，而不是一句“操作成功”', () => {
    const summary = summarizeStorageJob(job({ result: null }))
    expect(summary.ok).toBe(true)
    expect(summary.headline).toContain('没有返回可展示的现场读数')
    expect(summary.lines).toEqual([])
  })

  it('result 里只有空对象时同理', () => {
    const summary = summarizeStorageJob(job({ result: { data: {}, observed_state: {} } }))
    expect(summary.lines).toEqual([])
    expect(summary.headline).toContain('没有返回可展示的现场读数')
  })
})

describe('成功的卷动作', () => {
  const result = {
    outcome: 'verified',
    data: { fstab_backup: '/var/lib/upanel-agent/fstab-backups/20261021T093145Z-6f1a2b3c.fstab' },
    observed_state: {
      fstab_entry:
        'UUID=6f1a2b3c-4d5e-6f70-8192-a3b4c5d6e7f8 /srv/upanel/volumes/data ext4 defaults,nofail,x-systemd.device-timeout=30 0 2',
      mounted: true,
      mount_path: '/srv/upanel/volumes/data',
      source: '/dev/sdb1',
      options: 'rw,relatime',
      registered: true,
      checked_via: ['fstab_reread', 'findmnt'],
    },
  }

  it('把 fstab 行、挂载事实、备份路径、核验来源都摊出来，顺序固定', () => {
    const summary = summarizeStorageJob(job({ result }))
    expect(summary.ok).toBe(true)
    expect(summary.headline).toBe('任务已成功，下面是执行后的现场读数')
    expect(summary.lines).toEqual([
      '两源核验一致',
      'fstab 托管块里的这一行：UUID=6f1a2b3c-4d5e-6f70-8192-a3b4c5d6e7f8 /srv/upanel/volumes/data ext4 defaults,nofail,x-systemd.device-timeout=30 0 2',
      '现在挂着（/srv/upanel/volumes/data），来源 /dev/sdb1，选项 rw,relatime',
      '登记记录仍在',
      '改动前的 fstab 备份：/var/lib/upanel-agent/fstab-backups/20261021T093145Z-6f1a2b3c.fstab',
      '核验来源：重新读 /etc/fstab 的托管块 + findmnt 读回真实挂载',
    ])
  })

  it('卸载后没有挂载、fstab 条目已移除，也照实说', () => {
    const summary = summarizeStorageJob(
      job({
        result: {
          outcome: 'verified',
          observed_state: {
            fstab_entry: false,
            mounted: false,
            mount_path: '/srv/upanel/volumes/data',
            registered: false,
            checked_via: ['fstab_reread', 'topology_scan'],
          },
        },
      }),
    )
    expect(summary.lines).toContain('fstab 托管块里已经没有这个卷的条目')
    expect(summary.lines).toContain('现在没有挂载（/srv/upanel/volumes/data）')
    expect(summary.lines).toContain('登记记录已移除（卷本身和里面的数据没动）')
  })

  it('没给 outcome 时不凭空补一句“核验一致”', () => {
    const summary = summarizeStorageJob(
      job({ result: { observed_state: { mounted: false } } }),
    )
    expect(summary.lines).toEqual(['现在没有挂载'])
  })
})

describe('成功的设备动作', () => {
  it('分区表、新建分区、文件系统、命令逐项列出', () => {
    const summary = summarizeStorageJob(
      job({
        result: {
          outcome: 'verified',
          data: {
            created_partition: {
              device_id: 'sdb1',
              start_bytes: '1048576',
              size_bytes: '1073741824',
              name: 'data',
            },
            command: ['sfdisk', '--append', '/dev/sdb'],
          },
          observed_state: {
            table_type: 'gpt',
            kernel_partitions: ['sdb1'],
            disk_guid: '0F1A2B3C-4D5E-6F70-8192-A3B4C5D6E7F8',
            filesystem: 'ext4',
            uuid: '6f1a2b3c-4d5e-6f70-8192-a3b4c5d6e7f8',
            node_present: false,
            checked_via: ['disk_bytes', 'kernel_sysfs'],
          },
        },
      }),
    )
    expect(summary.lines).toContain('盘上的分区表：gpt，内核看到 sdb1')
    expect(summary.lines).toContain('盘上的 DiskGUID：0F1A2B3C-4D5E-6F70-8192-A3B4C5D6E7F8')
    expect(summary.lines).toContain('新建分区 sdb1，起点 1048576 字节，大小 1073741824 字节，名字 data')
    expect(summary.lines).toContain('文件系统：ext4，UUID 6f1a2b3c-4d5e-6f70-8192-a3b4c5d6e7f8')
    expect(summary.lines).toContain('设备节点现在不在 /dev 下（可能已被移除）')
    expect(summary.lines).toContain('执行的外部命令：sfdisk --append /dev/sdb')
    expect(summary.lines).toContain('核验来源：直读盘上的分区表字节 + 读内核 sysfs 的分区视图')
  })

  it('内核没看到分区时说明“内核没看到任何分区”，不是留空', () => {
    const summary = summarizeStorageJob(
      job({ result: { observed_state: { table_type: 'gpt', kernel_partitions: [] } } }),
    )
    expect(summary.lines).toContain('盘上的分区表：gpt（内核没看到任何分区）')
  })

  it('卷标只在 data.label 里给，跟着文件系统那行一起显示', () => {
    const summary = summarizeStorageJob(
      job({ result: { data: { filesystem: 'xfs', uuid: 'abc', label: 'data' } } }),
    )
    expect(summary.lines).toContain('文件系统：xfs，UUID abc，卷标 data')
  })
})
