// 任务结果翻译的测试（P07.5）。
//
// 核心断言只有一条：**不 succeeded 就不许说成功**，而且 evidence 只能来自
// result.observed_state / result.data —— 字段缺失时宁可少写一行，也不能显示
// undefined 或者编一句“已完成”。

import { describe, expect, it } from 'vitest'

import type { JobStatus } from '@/api/jobs'
import {
  checkedViaLabel,
  summarizeAccountJob,
  summarizeBackupJob,
  summarizeDeploymentJob,
  summarizeNetworkJob,
  summarizePackageJob,
  summarizePowerJob,
  summarizeScheduleJob,
  summarizeShareJob,
  summarizeStorageJob,
} from './jobresult'

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

// ---------------------------------------------------------------------------
// 共享与账号动作（P08.4）。字段与存储那一套完全不同：samba_config / include /
// reload / served / startup_deps / 账号侧的三条读回。
// ---------------------------------------------------------------------------
describe('共享动作的结果翻译', () => {
  const shareSummary = (result: unknown, status: JobStatus = 'succeeded') =>
    // 契约里 result / error 在“没有”时是空对象 {}，不是 null（openapi Job.result、Job.error）。
    summarizeShareJob({ status, result: result as never, error: {} })

  it('写进配置并重载：把每一步都说出来，不只说“成功”', () => {
    const summary = shareSummary({
      outcome: 'verified',
      observed_state: {
        samba_config: 'written',
        include: 'installed',
        reload: 'done',
        service: 'active',
        served: true,
        startup_deps: 'written',
        config_hash: 'abcdef0123456789'.repeat(4),
      },
      data: { managed_config: '/etc/samba/upanel.d/upanel.conf', startup_paths: ['/srv/upanel/volumes/data'] },
    })
    expect(summary.ok).toBe(true)
    expect(summary.lines).toContain('托管配置已整份重写')
    expect(summary.lines).toContain('include 已装进 smb.conf')
    expect(summary.lines).toContain('已让 Samba 重载配置')
    expect(summary.lines).toContain('smbd 此刻：active')
    expect(summary.lines).toContain('开机依赖已写入 smbd 单元')
    expect(summary.lines).toContain('开机前必须先就位：/srv/upanel/volumes/data')
  })

  it('内容没变时说的是“没写也没重载”，不是“已生效”', () => {
    const summary = shareSummary({
      outcome: 'verified',
      observed_state: {
        samba_config: 'unchanged',
        include: 'present',
        reload: 'skipped_unchanged',
        served: true,
      },
    })
    expect(summary.lines).toContain('内容没变，没有重写也没有重载')
    expect(summary.lines).toContain('include 本来就在')
    expect(summary.lines).toContain('内容没变，跳过了重载')
  })

  it('smbd 没在跑：served 为假时那句话必须是否定的', () => {
    const summary = shareSummary({
      outcome: 'accepted',
      observed_state: { reload: 'skipped_service_inactive', service: 'inactive', served: false },
    })
    expect(summary.lines).toContain('Samba 没在运行，因此没有重载')
    expect(summary.lines.join('\n')).toContain('没有在提供服务')
    expect(summary.lines.join('\n')).not.toContain('**')
  })

  it('被卷掉线跳过的共享点名列出（不是静悄悄跳过）', () => {
    const summary = shareSummary({
      observed_state: { volume_offline_shares: ['data', 'backup'] },
    })
    expect(summary.lines).toContain('因为卷没挂而被跳过、没有重新渲染的共享：data、backup')
  })

  it('账号侧：Linux 口令、passdb、密码时间、秘密是否转持久', () => {
    const summary = shareSummary({
      outcome: 'verified',
      observed_state: {
        name: 'teamdocs',
        uid: 998,
        shell: '/usr/sbin/nologin',
        linux_password: 'L',
        samba_account: true,
        password_set: true,
        password_last_set: '2026-10-22T08:00:00Z',
        secret_persisted: false,
        secret_note: '秘密已被引用但仍然临时',
      },
    })
    const text = summary.lines.join('\n')
    expect(text).toContain('账号 teamdocs：uid 998，shell /usr/sbin/nologin')
    expect(text).toContain('Samba passdb 里能查到这条账号')
    expect(text).toContain('密码已设置')
    expect(text).toContain('密码密文没能转成持久秘密')
  })

  it('失败时不编现场读数，只给错误与恢复建议', () => {
    const summary = summarizeShareJob({
      status: 'failed',
      result: {},
      error: { message: 'testparm 拒绝了候选配置', recovery_hint: '看日志里的 testparm 输出' } as never,
    })
    expect(summary.ok).toBe(false)
    expect(summary.headline).toBe('任务失败')
    expect(summary.lines).toEqual(['testparm 拒绝了候选配置'])
    expect(summary.hint).toBe('看日志里的 testparm 输出')
  })

  it('中断时说“结果未知”，不假装成功也不假装失败', () => {
    const summary = summarizeShareJob({ status: 'interrupted', result: {}, error: {} })
    expect(summary.headline).toContain('结果未知')
  })

  it('结果里什么都没有时明说没有现场读数', () => {
    const summary = shareSummary({})
    expect(summary.headline).toContain('没有返回可展示的现场读数')
  })
})

// ---------------------------------------------------------------- 软件包写事务

// 这些载荷照 internal/agent/packageaction.go 的实际输出拼（`packageResult` /
// `refreshResult` 的字段名逐个对应），不是自己想出来的形状：
//   result.outcome               verified | partial | unknown
//   result.observed_state        operation / verified / mismatch_count /
//                                observed_packages / `pkg:<名字>` → `ii 版本` 或 `未安装`
//   result.data                  operation / packages / exit_code / mismatches /
//                                index_before / index_after（refresh 另有
//                                refresh_errors / refresh_warns）
//
// 时间字段为空时代理给的是空串（`timeText(nil)`），翻译层按"没有"处理——这一条
// 也要锁住，否则索引读不到时会显示成两个冒号。

describe('软件包写事务的结果翻译', () => {
  it('核对通过时说"逐包核对过"，并把每个包的真实状态列出来', () => {
    const summary = summarizePackageJob(
      job({
        result: {
          outcome: 'verified',
          observed_state: {
            operation: 'install',
            verified: true,
            mismatch_count: 0,
            observed_packages: 2,
            'pkg:zlib1g': 'ii 1:1.2.11.dfsg-2ubuntu9.2',
            'pkg:cowsay': 'ii 3.03+dfsg2-8',
          },
          data: {
            operation: 'install',
            packages: ['cowsay', 'zlib1g'],
            exit_code: 0,
            mismatches: [],
            index_before: '2026-10-24T01:02:03Z',
            index_after: '2026-10-24T01:02:03Z',
          },
        },
      }),
    )
    const text = summary.lines.join('\n')
    expect(summary.ok).toBe(true)
    expect(summary.headline).toBe('执行完成，并且逐包核对通过')
    expect(text).toContain('执行后逐包核对过：读回的版本与预检的计划一致')
    expect(text).toContain('执行的写操作：install')
    expect(text).toContain('目标软件包：cowsay、zlib1g')
    expect(text).toContain('cowsay：ii 3.03+dfsg2-8')
    expect(text).toContain('apt-get 退出码：0')
    // 逐包状态按字典序，页面每次打开顺序都一样
    expect(text.indexOf('cowsay：')).toBeLessThan(text.indexOf('zlib1g：'))
  })

  it('核对没过时说"不一致"，并把对不上的地方照抄出来', () => {
    const summary = summarizePackageJob(
      job({
        result: {
          outcome: 'partial',
          observed_state: {
            operation: 'upgrade',
            verified: false,
            mismatch_count: 1,
            observed_packages: 1,
            'pkg:zlib1g': 'ii 1:1.2.11.dfsg-2ubuntu9.1',
          },
          data: {
            operation: 'upgrade',
            packages: ['zlib1g'],
            exit_code: 0,
            mismatches: ['zlib1g：期望 1:1.2.11.dfsg-2ubuntu9.2，实际 1:1.2.11.dfsg-2ubuntu9.1'],
          },
        },
      }),
    )
    const text = summary.lines.join('\n')
    // apt 返回 0 不代表达成：这里 ok 仍是 true（任务跑完了），但结论必须是"不一致"
    expect(summary.ok).toBe(true)
    expect(summary.headline).toBe('执行完成，下面是读回的现场读数')
    expect(text).toContain('执行完成，但读回的状态与预检的计划不一致')
    expect(text).toContain('没有通过核对：apt 报告成功，但读回的软件包状态与预检的计划不一致')
    expect(text).toContain('对不上的地方：zlib1g：期望 1:1.2.11.dfsg-2ubuntu9.2')
  })

  it('卸载之后显示"未安装"，而不是拿旧版本冒充现状', () => {
    const summary = summarizePackageJob(
      job({
        result: {
          outcome: 'verified',
          observed_state: {
            operation: 'remove',
            verified: true,
            'pkg:cowsay': '未安装',
          },
          data: { packages: ['cowsay'], exit_code: 0 },
        },
      }),
    )
    expect(summary.lines.join('\n')).toContain('cowsay：未安装')
  })

  it('刷新索引报出索引时间的前后变化与 apt-get update 的告警', () => {
    const summary = summarizePackageJob(
      job({
        result: {
          outcome: 'verified',
          observed_state: {
            operation: 'refresh',
            verified: true,
            exit_code: 0,
            index_after: '2026-10-24T02:10:00Z',
          },
          data: {
            operation: 'refresh',
            exit_code: 0,
            index_before: '2026-10-23T20:00:00Z',
            index_after: '2026-10-24T02:10:00Z',
            refresh_errors: [],
            refresh_warns: ['W: 仓库 "http://old.example.invalid stable Release" 没有 Release 文件。'],
          },
        },
      }),
    )
    const text = summary.lines.join('\n')
    expect(text).toContain('索引时间：2026-10-23T20:00:00Z → 2026-10-24T02:10:00Z')
    expect(text).toContain('apt-get update 的告警：W: 仓库')
    expect(text).not.toContain('refresh_errors')
  })

  it('索引时间读不到时不渲染成两个空冒号', () => {
    const summary = summarizePackageJob(
      job({
        result: {
          outcome: 'verified',
          observed_state: { operation: 'refresh', verified: true },
          data: { index_before: '', index_after: '' },
        },
      }),
    )
    expect(summary.lines.join('\n')).not.toContain('索引时间')
  })

  it('失败时只给错误与恢复建议，不给现场读数', () => {
    const summary = summarizePackageJob(
      job({
        status: 'failed',
        result: { observed_state: { operation: 'install', verified: false } },
        error: {
          message: 'apt-get 以退出码 100 结束：E: 无法定位软件包 cowsayy',
          recovery_hint: '确认包名拼写；需要新包时先刷新索引',
        },
      }),
    )
    expect(summary.ok).toBe(false)
    expect(summary.headline).toBe('任务失败')
    expect(summary.lines).toEqual(['apt-get 以退出码 100 结束：E: 无法定位软件包 cowsayy'])
    expect(summary.hint).toBe('确认包名拼写；需要新包时先刷新索引')
  })

  it('中断时说"结果未知"，因为 dpkg 可能停在半配置状态', () => {
    const summary = summarizePackageJob({ status: 'interrupted', result: {}, error: {} })
    expect(summary.ok).toBe(false)
    expect(summary.headline).toContain('结果未知')
    expect(summary.headline).toContain('dpkg')
  })

  it('结果里什么都没有时明说没有现场读数，并让人去刷列表', () => {
    const summary = summarizePackageJob(job({ result: {} }))
    expect(summary.headline).toContain('没有返回可展示的现场读数')
  })

  it('observed_state 缺失时不崩，也不编"核对通过"', () => {
    const summary = summarizePackageJob(job({ result: { data: { packages: ['cowsay'] } } }))
    expect(summary.headline).toBe('执行完成，下面是读回的现场读数')
    expect(summary.lines.join('\n')).not.toContain('核对通过')
  })
})

describe('电源任务的结果翻译（P11.3）', () => {
  type PowerInput = Parameters<typeof summarizePowerJob>[0]

  function powerJob(overrides: Partial<PowerInput> = {}): PowerInput {
    return {
      status: 'succeeded',
      result: {
        outcome: 'accepted',
        observed_state: {
          operation: 'reboot',
          delay_seconds: 60,
          boot_id_before: 'ab2eb5e8-c1b4-4a00-a3d0-33ced7870abc',
          issued_at: '2026-10-23T10:00:00Z',
          command: 'systemctl reboot',
          exit_code: 0,
        },
      },
      error: {},
      power_check: null,
      ...overrides,
    }
  }

  it('受理不说成功：结论行必须是“不是已重启”', () => {
    const summary = summarizePowerJob(powerJob())
    expect(summary.ok).toBe(true)
    expect(summary.headline).toContain('请求已受理')
    expect(summary.headline).toContain('不是已重启')
    expect(summary.headline).not.toContain('重启成功')
    const body = summary.lines.join('\n')
    expect(body).toContain('systemctl reboot')
    expect(body).toContain('60 秒')
    expect(body).toContain('ab2eb5e8-c1b4-4a00-a3d0-33ced7870abc')
    expect(body).toContain('systemctl 退出码：0')
  })

  it('boot_id 变了才说“已确认”', () => {
    const summary = summarizePowerJob(
      powerJob({
        power_check: {
          operation: 'reboot',
          state: 'confirmed',
          changed: true,
          boot_id_before: 'ab2eb5e8-c1b4-4a00-a3d0-33ced7870abc',
          boot_id_now: '9f4d2c17-6a55-4c2e-8f31-0b7e5a1d9c40',
          checked_at: '2026-10-23T10:05:00Z',
          message: '重启已确认：boot_id 从 ab2eb5e8 变成 9f4d2c17。',
        },
      }),
    )
    expect(summary.headline).toContain('已确认')
    expect(summary.lines.join('\n')).toContain('现在的 boot_id：9f4d2c17')
  })

  it('没变就是没变：不说“已确认”', () => {
    const summary = summarizePowerJob(
      powerJob({
        power_check: {
          operation: 'reboot',
          state: 'unchanged',
          changed: false,
          boot_id_before: 'ab2eb5e8-c1b4-4a00-a3d0-33ced7870abc',
          boot_id_now: 'ab2eb5e8-c1b4-4a00-a3d0-33ced7870abc',
          checked_at: '2026-10-23T10:00:30Z',
          message: '代理仍在原来的 boot_id 上：重启还没有发生。',
        },
      }),
    )
    expect(summary.headline).toContain('重启还没有发生')
    expect(summary.headline).not.toContain('已确认')
  })

  it('关机连不上：直说读不到，不许滑成“关机成功”', () => {
    const summary = summarizePowerJob(
      powerJob({
        result: {
          outcome: 'accepted',
          observed_state: { operation: 'poweroff', delay_seconds: 30, boot_id_before: 'ab2eb5e8' },
        },
        power_check: {
          operation: 'poweroff',
          state: 'unreachable',
          changed: false,
          boot_id_before: 'ab2eb5e8',
          boot_id_now: '',
          checked_at: '2026-10-23T10:01:00Z',
          message: '代理已不可达。这与下发关机之后的预期一致，但面板无法据此确认电源已切断。',
        },
      }),
    )
    expect(summary.headline).toContain('代理现在连不上')
    const body = summary.lines.join('\n')
    expect(body).toContain('无法据此确认电源已切断')
    expect(body).not.toContain('关机成功')
  })

  it('没有核对时，关机与重启各给一句该怎么继续', () => {
    const poweroff = summarizePowerJob(
      powerJob({
        result: { outcome: 'accepted', observed_state: { operation: 'poweroff', delay_seconds: 30 } },
      }),
    )
    expect(poweroff.lines.join('\n')).toContain('不构成关机成功的证据')

    const reboot = summarizePowerJob(powerJob())
    expect(reboot.lines.join('\n')).toContain('boot_id')
  })

  it('取消与失败照原样说，并且不算成功', () => {
    const canceled = summarizePowerJob(
      // 取消的任务没有结果：契约里 result 是空对象，这里连空对象都不给，翻译层也要扛住。
      powerJob({ status: 'canceled', result: null as unknown as Record<string, unknown>, error: {} }),
    )
    expect(canceled.ok).toBe(false)
    expect(canceled.headline).toContain('取消')
    expect(canceled.headline).toContain('没有被调用')

    const failed = summarizePowerJob(
      powerJob({
        status: 'failed',
        result: {},
        error: { code: 'COMMAND_FAILED', message: 'systemctl: 交互认证被拒绝' },
      }),
    )
    expect(failed.ok).toBe(false)
    expect(failed.lines).toEqual(['systemctl: 交互认证被拒绝'])
  })
})

// 备份动作的结果翻译（P12.5）。
//
// 备份这一块最容易骗人的地方是"成功"两个字：
//   - backup.run 的 verified 说的是"写完读回来对得上"，不是"这份数据能恢复"；
//   - backup.verify 不勾抽样恢复时，什么都没证明；
//   - 恢复的 published 只说明目录发布出去了，不说明业务回来了。
// 所以这里断言的不是"有没有输出"，而是**这几句话有没有被说出来**。
describe('备份动作的结果翻译', () => {
  function backupJob(observed: Record<string, unknown>, extra: Record<string, unknown> = {}): SummaryInput {
    return job({ result: { ...extra, observed_state: observed } })
  }

  it('run 成功说清"备份成功不等于恢复已验证"，并带上 restic 统计与来源', () => {
    const summary = summarizeBackupJob(
      backupJob(
        {
          snapshot_id: 'abcdef0123',
          verified_by: 'restic check --read-data-subset',
          files_new: 12,
          files_changed: 3,
          total_files_processed: 15,
          data_added: 4096,
          sources: [
            { source: 'root1:srv/www', path: '/srv/www', exists: true },
            { source: 'root1:srv/gone', path: '/srv/gone', exists: false },
          ],
          panel_database: { size_bytes: 8192, schema_version: 7, integrity: 'ok' },
          apps_restored: true,
        },
        // outcome 在 result 顶层（代理的 marshalResult 就是这么包的），不在 observed_state 里。
        { outcome: 'verified' },
      ),
    )
    const text = summary.lines.join('\n')
    expect(summary.ok).toBe(true)
    expect(summary.headline).toContain('备份成功不等于恢复已验证')
    expect(text).toContain('执行后读回来核对过')
    expect(text).toContain('快照：abcdef0123')
    expect(text).toContain('核验方式：restic check --read-data-subset')
    expect(text).toContain('新增 12 个文件')
    expect(text).toContain('已包含在这份快照里')
    expect(text).toContain('执行时读不到')
    expect(text).toContain('面板数据库副本')
    expect(text).toContain('都已按原状态拉起')
  })

  it('outcome 是 unknown 时说"读回来的结果对不上"，不粉饰', () => {
    const summary = summarizeBackupJob(
      backupJob({ verify_error: '快照索引损坏' }, { outcome: 'unknown' }),
    )
    const text = summary.lines.join('\n')
    expect(text).toContain('读回来的结果对不上')
    expect(text).toContain('读回核对没通过：快照索引损坏')
  })

  it('有应用没能拉起时逐条报，不汇总成一句"部分失败"', () => {
    const summary = summarizeBackupJob(
      backupJob({
        apps_restored: false,
        apps: [
          { object: 'app_web', operation: 'start', done: true, verified: true },
          { object: 'app_db', operation: 'start', done: false, verified: false, note: '依赖未就绪' },
        ],
      }),
    )
    const text = summary.lines.join('\n')
    expect(text).toContain('有应用没能按原状态拉起')
    expect(text).toContain('app_web：start，已执行，读回核对通过')
    expect(text).toContain('app_db：start，没执行，读回核对不一致（依赖未就绪）')
  })

  it('verify 不勾抽样时说清"什么都没证明"', () => {
    const summary = summarizeBackupJob(
      backupJob({ snapshot_id: 'abcdef0123', checked: 'structure', sample_restore: false }),
    )
    const text = summary.lines.join('\n')
    expect(text).toContain('核对范围：structure')
    expect(text).toContain('没有抽文件真恢复')
  })

  it('verify 勾了抽样但没通过时说"不要指望它"', () => {
    const summary = summarizeBackupJob(backupJob({ sample_restore: true, restore_verified: false }))
    expect(summary.lines.join('\n')).toContain('读不回来')
  })

  it('恢复的 published 只说目录发布，不冒充业务恢复', () => {
    const summary = summarizeBackupJob(
      backupJob({
        snapshot_id: 'abcdef0123',
        published: true,
        target_root_id: 'root1',
        relative_path: 'restored',
        files: 10,
        directories: 2,
        bytes: 1024,
      }),
    )
    const text = summary.lines.join('\n')
    expect(text).toContain('目录已发布到文件根 root1 下的 /restored')
    expect(text).toContain('恢复规模：10 个文件、2 个目录，共 1024 字节')
  })

  it('清理列出删了几份、留了几份，删掉的 id 逐个具名', () => {
    const summary = summarizeBackupJob(backupJob({ deleted_ids: ['aaa', 'bbb'], kept_ids: ['ccc'] }))
    const text = summary.lines.join('\n')
    expect(text).toContain('删除 2 份、保留 1 份')
    expect(text).toContain('已删除：aaa、bbb')
  })

  it('初始化仓库把 restic 标识、版本、实际位置与主机公钥都摊出来', () => {
    const summary = summarizeBackupJob(
      backupJob({
        repo_id: 'repo-uuid',
        restic_version: '0.17.3',
        absolute_path: '/data/root1/upanel-restic',
        host_key: 'SHA256:abc',
        initialized_at: '2026-10-24T03:00:00Z',
      }),
    )
    const text = summary.lines.join('\n')
    expect(text).toContain('restic 仓库标识：repo-uuid')
    expect(text).toContain('restic 版本：0.17.3')
    expect(text).toContain('实际位置：/data/root1/upanel-restic')
    expect(text).toContain('远端主机公钥：SHA256:abc')
    expect(text).toContain('初始化完成于：2026-10-24T03:00:00Z')
  })

  it('中断的任务不说成功，并提醒重跑只会再写一份快照', () => {
    const summary = summarizeBackupJob(job({ status: 'interrupted', result: {} }))
    expect(summary.ok).toBe(false)
    expect(summary.headline).toContain('结果未知')
    expect(summary.headline).toContain('不会覆盖已有快照')
  })

  it('成功但没有任何现场读数时如实说"没有可展示的读数"', () => {
    const summary = summarizeBackupJob(job({ result: {} }))
    expect(summary.ok).toBe(true)
    expect(summary.headline).toContain('没有返回可展示的现场读数')
  })
})

// 计划任务与脚本的结果翻译（P12.5）。
describe('计划与脚本动作的结果翻译', () => {
  it('schedule.apply 逐条报出三个观测值，不许把"定义里开了"说成"在跑"', () => {
    const summary = summarizeScheduleJob(
      job({
        result: {
          observed_state: {
            schedule_id: 'sch_0123',
            name: '每晚全量备份',
            timer_ref: 'upanel-sch-0123.timer',
            enabled: true,
            installed: true,
            unit_file_state: 'enabled',
            active_state: 'inactive',
          },
        },
      }),
    )
    const text = summary.lines.join('\n')
    expect(text).toContain('计划：每晚全量备份（sch_0123）')
    expect(text).toContain('单元：upanel-sch-0123.timer')
    expect(text).toContain('定义里写的是“启用”')
    expect(text).toContain('单元现状：enabled / inactive')
  })

  it('单元没装上时直说这条定义现在不会触发', () => {
    const summary = summarizeScheduleJob(
      job({ result: { observed_state: { schedule_id: 'sch_x', installed: false, enabled: true } } }),
    )
    expect(summary.lines.join('\n')).toContain('不会触发')
  })

  it('schedule.delete 只说删掉了什么，不再谈单元现状', () => {
    const summary = summarizeScheduleJob(
      job({ result: { observed_state: { schedule_id: 'sch_x', deleted: true } } }),
    )
    expect(summary.headline).toContain('已删除')
    const text = summary.lines.join('\n')
    expect(text).toContain('记录已删除')
    expect(text).not.toContain('单元现状')
  })

  it('script.run 报退出码与规模，并且每次都说输出不落库', () => {
    const summary = summarizeScheduleJob(
      job({
        result: {
          observed_state: {
            script_id: 'scr_0123',
            name: '清缓存',
            content_hash: 'sha256:abcd',
            exit_code: 0,
            duration_ms: 1250,
            stdout: 12,
            stderr: 0,
            truncated: true,
          },
        },
      }),
    )
    const text = summary.lines.join('\n')
    expect(text).toContain('脚本：清缓存（scr_0123）')
    expect(text).toContain('内容指纹：sha256:abcd')
    expect(text).toContain('退出码 0，耗时 1250 毫秒，标准输出 12 字节，标准错误 0 字节（输出被截断）')
    expect(text).toContain('输出不落库')
  })

  it('script.apply 报落盘位置与字节数', () => {
    const summary = summarizeScheduleJob(
      job({
        result: {
          observed_state: {
            script_id: 'scr_x',
            content_hash: 'sha256:e',
            path: '/var/lib/upanel/scr_x',
            bytes: 64,
          },
        },
      }),
    )
    const text = summary.lines.join('\n')
    expect(text).toContain('落盘位置：/var/lib/upanel/scr_x')
    expect(text).toContain('正文 64 字节')
  })

  it('中断与失败都不算成功', () => {
    expect(summarizeScheduleJob(job({ status: 'interrupted', result: {} })).ok).toBe(false)
    const failed = summarizeScheduleJob(
      job({ status: 'failed', result: {}, error: { message: 'SCHEDULE_TARGET_NOT_FOUND' } }),
    )
    expect(failed.ok).toBe(false)
    expect(failed.lines).toEqual(['SCHEDULE_TARGET_NOT_FOUND'])
  })
})

// 网络写动作的结果翻译（P14.2）。
//
// 这一组只有一个要害：**结论看 outcome，不看 status**。`rolled_back` 的任务
// status 也是 succeeded——它跑完了，只是改动没留下。渲染成"成功"，人就以为新地址
// 生效了；渲染成"失败"，人以为机器现在坏了。两句都不对。
describe('网络写动作的结果翻译（P14.2）', () => {
  it('verified：说读回了内核事实，并列出网卡/模式/地址', () => {
    const summary = summarizeNetworkJob(
      job({
        result: {
          outcome: 'verified',
          observed_state: { interface: 'ens4', mode: 'static', address: '192.168.1.50/24' },
          data: { rollback_token: 'nrb_abc', window_seconds: 120, config_hash_now: 'sha256:deadbeefcafe1234' },
        },
      }),
    )
    expect(summary.ok).toBe(true)
    expect(summary.headline).toContain('读回来的现场事实')
    const text = summary.lines.join('\n')
    expect(text).toContain('新配置确实生效了')
    expect(text).toContain('ens4，模式 静态地址，地址 192.168.1.50/24')
    expect(text).toContain('回滚窗口：120 秒')
    expect(text).toContain('回滚标识：nrb_abc')
    // 指纹只露前面一小段：这一行是给人核对"是不是我那一版"，不是给人抄走的。
    expect(text).toContain('sha256:deadbeefc…')
    expect(text).not.toContain('sha256:deadbeefcafe1234')
  })

  it('rolled_back：succeeded 也不许说成功', () => {
    const summary = summarizeNetworkJob(
      job({
        result: {
          outcome: 'rolled_back',
          observed_state: { interface: 'ens4' },
          data: { previous: '192.168.1.10/24' },
        },
      }),
    )
    expect(summary.ok).toBe(false)
    expect(summary.headline).toContain('改动没有留在机器上')
    expect(summary.headline).not.toContain('失败')
    expect(summary.lines.join('\n')).toContain('改之前的样子：192.168.1.10/24')
  })

  it('unknown：不说成功也不说失败，让人去看现场', () => {
    const summary = summarizeNetworkJob(
      job({ result: { outcome: 'unknown', observed_state: { verify_error: 'ip -j addr 超时' } } }),
    )
    expect(summary.ok).toBe(false)
    expect(summary.headline).toContain('结果未知')
    expect(summary.headline).toContain('看一眼现场')
    expect(summary.lines.join('\n')).toContain('核验：ip -j addr 超时')
  })

  it('确认与回滚：按字段存在性读，记录状态各说一句', () => {
    const confirmed = summarizeNetworkJob(
      job({ result: { outcome: 'verified', observed_state: { state: 'confirmed' } } }),
    )
    expect(confirmed.lines.join('\n')).toContain('回滚 timer 已停')

    const failed = summarizeNetworkJob(
      job({ result: { outcome: 'unknown', observed_state: { state: 'failed' } } }),
    )
    expect(failed.lines.join('\n')).toContain('回滚没走完')

    // data.state 也认：动作不同，字段落在哪一侧不一样。
    const fromData = summarizeNetworkJob(job({ result: { outcome: 'verified', data: { state: 'rolled_back' } } }))
    expect(fromData.lines.join('\n')).toContain('记录已标为已回滚')
  })

  it('失败路径上的原因逐条列出来，不合并成一句', () => {
    const summary = summarizeNetworkJob(
      job({
        result: {
          outcome: 'unknown',
          observed_state: {
            netplan_generate: '生成失败：网关不在网段内',
            arm_error: 'systemd 拒绝',
            restore_error: 'ENOSPC',
          },
        },
      }),
    )
    const text = summary.lines.join('\n')
    expect(text).toContain('netplan generate：生成失败')
    expect(text).toContain('回滚 timer：systemd 拒绝')
    expect(text).toContain('恢复面板文件：ENOSPC')
  })

  it('没有 outcome 时不说"新配置生效了"', () => {
    const summary = summarizeNetworkJob(job({ result: { observed_state: { interface: 'ens4' } } }))
    expect(summary.ok).toBe(true)
    expect(summary.lines.join('\n')).not.toContain('新配置确实生效了')
  })

  it('中断：明说网络可能改了一半', () => {
    const summary = summarizeNetworkJob(job({ status: 'interrupted', result: {} }))
    expect(summary.ok).toBe(false)
    expect(summary.headline).toContain('可能改了一半')
  })

  it('failed：带服务端的错误原文与恢复建议', () => {
    const summary = summarizeNetworkJob(
      job({ status: 'failed', error: { message: 'NETWORK_UNSUPPORTED', recovery_hint: '手工改 netplan' } }),
    )
    expect(summary.ok).toBe(false)
    expect(summary.lines).toEqual(['NETWORK_UNSUPPORTED'])
    expect(summary.hint).toBe('手工改 netplan')
  })
})

// ---------------------------------------------------------------------------
// NFS 导出的结果翻译（P14.4）。字段照 internal/shares/writeexports.go 的实际输出拼：
//   observed_state  exports_file / reload / service / served / startup_deps /
//                   exported_paths / volume_offline_shares / managed_exports / notes
//   data            export_preview / managed_exports / exports / volume_offline /
//                   protocol=nfs
//
// 判“这是哪一边”不看调用方传什么，只看字段：NFS 的 exports_file 与 SMB 的 samba_config
// 不同名，所以从载荷本身就能读出来。这一条一旦破了，一次 NFS 应用会被翻译成
// “Samba 配置已重写”——那是在说一个根本没发生的动作。
// ---------------------------------------------------------------------------
describe('NFS 导出的结果翻译', () => {
  const shareSummary = (result: unknown, status: JobStatus = 'succeeded') =>
    summarizeShareJob({ status, result: result as never, error: {} })

  it('读回都通过时把每一步说全，且一个字都不提 smbd', () => {
    const summary = shareSummary({
      outcome: 'verified',
      observed_state: {
        managed_exports: '/etc/exports.d/upanel.exports',
        exports_hash: 'abcdef0123456789'.repeat(4),
        exports_file: 'written',
        reload: 'done',
        service: 'active',
        served: true,
        startup_deps: 'written',
        exported_paths: ['/srv/upanel/volumes/data/media'],
      },
      data: {
        protocol: 'nfs',
        export_preview: '/srv/upanel/volumes/data/media 10.0.2.0/24(rw,sync,root_squash,no_subtree_check) # upanel:shr_x',
        managed_exports: '/etc/exports.d/upanel.exports',
        startup_paths: ['/srv/upanel/volumes/data'],
        config_hash: 'abcdef0123456789'.repeat(4),
      },
    })
    expect(summary.ok).toBe(true)
    expect(summary.lines).toContain('托管导出文件已整份重写（只含面板自己那几条）')
    expect(summary.lines).toContain('已让 exportfs 重新读一遍导出表')
    expect(summary.lines).toContain('nfs-server 此刻：active')
    expect(summary.lines).toContain('开机前必须先就位：/srv/upanel/volumes/data')
    expect(summary.lines.join('\n')).toContain('exportfs 读回的表里有它')
    const text = summary.lines.join('\n')
    expect(text).not.toContain('smbd')
    expect(text).not.toContain('Samba')
    expect(text).not.toContain('samba_config')
  })

  it('没刷表时说的是“因为 nfs-server 没在运行”，不是“已生效”', () => {
    const summary = shareSummary({
      outcome: 'accepted',
      observed_state: {
        exports_file: 'written',
        reload: 'skipped_service_inactive',
        service: 'inactive',
        served: false,
      },
    })
    expect(summary.lines).toContain('nfs-server 没在运行，因此没有刷表')
    expect(summary.lines.join('\n')).toContain('没有在提供服务')
    // outcome 是 accepted，不能翻成“读回内核事实”
    expect(summary.lines[0]).not.toContain('读回')
  })

  it('内容没变时导出行照样给出来（不是“什么都没改”就完事）', () => {
    const summary = shareSummary({
      outcome: 'verified',
      observed_state: { exports_file: 'unchanged', reload: 'skipped_unchanged', served: true },
      data: { protocol: 'nfs', exports: ['/srv/upanel/volumes/data/media'] },
    })
    expect(summary.lines).toContain('导出文件内容没变，没有重写')
    expect(summary.lines).toContain('内容没变，跳过了刷表')
    expect(summary.lines).toContain('现在导出表里由面板写着的路径：/srv/upanel/volumes/data/media')
  })

  it('被卷掉线跳过的导出点名列出，且用的是 NFS 的字段名', () => {
    const summary = shareSummary({
      observed_state: {
        exports_file: 'written',
        volume_offline_shares: ['backup'],
      },
      data: { protocol: 'nfs', volume_offline: ['backup'] },
    })
    expect(summary.lines).toContain('因为卷没挂而被跳过、这次没有写进导出表的共享：backup')
  })

  it('停用导出：说清目录还在、NFS 没有 ACL 可收', () => {
    const summary = shareSummary({
      outcome: 'accepted',
      observed_state: {
        acl_revoked: false,
        acl_note:
          'NFS 共享没有 POSIX ACL：访问控制由导出里的网段与 root_squash 决定，摘掉导出行之后网段内也连不上了；目录权限保持原样',
        served: false,
        reload: 'done',
      },
      data: { protocol: 'nfs', managed_exports: '/etc/exports.d/upanel.exports' },
    })
    const text = summary.lines.join('\n')
    expect(text).toContain('NFS 共享没有 POSIX ACL')
    expect(text).not.toContain('目录 ACL 已清空')
    expect(text).not.toContain('目录 ACL 没有变化')
  })

  it('失败时只给错误与恢复建议，不掺现场读数', () => {
    const summary = summarizeShareJob({
      status: 'failed',
      result: {},
      error: {
        message: '刷新导出表之后读回的表里没有这条路径',
        recovery_hint: '导出行已还原；看 /var/log/syslog 里的 rpc.mountd',
      } as never,
    })
    expect(summary.ok).toBe(false)
    expect(summary.lines).toEqual(['刷新导出表之后读回的表里没有这条路径'])
    expect(summary.hint).toContain('rpc.mountd')
  })
})

describe('账号动作的结果翻译（P14.5）', () => {
  const accountSummary = (result: unknown, status: JobStatus = 'succeeded') =>
    summarizeAccountJob({ status, result: result as never, error: {} })

  // 首轮这里写的是“口令锁定 → 配了公钥也登不上”。真机把它否了（Q479）：key-only 账号能登，
  // 拦人的是壳。所以这一条断言的是“没口令 ≠ 登不上”，防止文案再被改回去。
  it('新建账号读到 L 时说“只能用公钥登录”，不说“登不上”', () => {
    const summary = accountSummary({
      outcome: 'verified',
      observed_state: {
        uid: 1002,
        shell: '/bin/rbash',
        home: '/home/bob',
        password_state: 'L',
        has_password: false,
      },
    })
    const text = summary.lines.join('\n')
    expect(text).toContain('只能用已配的公钥登录')
    expect(text).not.toContain('配了公钥也登不上')
  })

  it('禁用那一刀说清是壳挡住了，不是口令', () => {
    const summary = accountSummary({
      outcome: 'verified',
      observed_state: {
        shell: '/usr/sbin/nologin',
        shell_profile: 'restricted',
        shell_changed: true,
        disabled: true,
        has_password: false,
      },
    })
    const text = summary.lines.join('\n')
    expect(text).toContain('/usr/sbin/nologin')
    expect(text).toContain('登不上')
    expect(text).not.toContain('配了公钥也登不上')
  })
})

describe('部署动作的结果翻译（P15.4）', () => {
  const deploySummary = (result: unknown, status: JobStatus = 'succeeded', error: unknown = {}) =>
    summarizeDeploymentJob({ status, result: result as never, error: error as never })

  it('没有读数就不许说“已经在跑”——容器清单没读到时明说没有独立读数', () => {
    const summary = deploySummary({
      outcome: 'verified',
      observed_state: {
        project: 'webapp',
        unit: 'upanel-deploy-webapp.service',
        written: true,
        unit_active: true,
        unit_enabled: true,
        containers_checked: false,
        containers_error: 'docker 没有应答',
      },
    })
    const text = summary.lines.join('\n')
    expect(summary.ok).toBe(true)
    expect(text).toContain('upanel-deploy-webapp.service')
    expect(text).toContain('这一次写下了新的单元文件与渲染配置')
    expect(text).toContain('docker 没有应答')
    expect(text).toContain('没有独立读数')
  })

  it('回切是 succeeded + rolled_back：headline 不能说“已更新”，失败原因要露出来', () => {
    const summary = deploySummary({
      outcome: 'rolled_back',
      recovery_hint: '健康探测没探通：最后一次应答是 502',
      observed_state: {
        project: 'webapp',
        rolled_back: true,
        rollback_verified: true,
        rollback_health_ok: false,
        previous_config_digest: 'sha256:0123456789abcdef',
        config_digest: 'sha256:0123456789abcdef',
        image: 'nginx:1.28',
        image_digest_known: false,
      },
    })
    const text = summary.lines.join('\n')
    expect(summary.headline).toContain('回切')
    expect(summary.headline).not.toContain('已更新')
    expect(summary.hint).toContain('502')
    expect(text).toContain('上一版配置与镜像已恢复')
    expect(text).toContain('探测仍不通')
    // 记录里的 digest 与盘上一致：那句话说“没有变”，而不是编一个箭头。
    expect(text).toContain('配置没有变')
    expect(text).toContain('空着表示“不知道”')
  })

  it('partial 与删除的读数各说各的', () => {
    const partial = deploySummary({
      outcome: 'partial',
      recovery_hint: '单元 active，但 compose 项目下没有容器',
      observed_state: { project: 'webapp', containers_checked: true, containers_total: 0, containers_running: 0 },
    })
    expect(partial.headline).toContain('一项不对')
    expect(partial.lines.join('\n')).toContain('读到 0 个容器')

    const removed = deploySummary({
      outcome: 'verified',
      observed_state: {
        project: 'webapp',
        unit_removed: true,
        config_removed: true,
        data_kept: true,
      },
    })
    const text = removed.lines.join('\n')
    expect(text).toContain('包装单元文件已删除')
    expect(text).toContain('一个字节都没有动')
  })

  it('健康探测：没探测与探测没通过是两件事', () => {
    const notChecked = deploySummary({
      outcome: 'verified',
      observed_state: { health_checked: false, health_error: '定义里没有打开探测' },
    })
    expect(notChecked.lines.join('\n')).toContain('这次没有做健康探测')

    const failed = deploySummary({
      outcome: 'partial',
      observed_state: {
        health_checked: true,
        health_ok: false,
        health_url: 'http://127.0.0.1:8080/',
        health_status_code: 502,
        health_duration_ms: 12,
        health_truncated: true,
      },
    })
    const text = failed.lines.join('\n')
    expect(text).toContain('探测没通过')
    expect(text).toContain('状态码 502')
    expect(text).toContain('截断不算通')
  })

  it('失败与中断不借用成功的话术', () => {
    const failed = deploySummary(null, 'failed', {
      code: 'unit_still_active',
      message: '单元还在 active，没有删文件',
      recovery_hint: '先看 journal',
    })
    expect(failed.ok).toBe(false)
    expect(failed.headline).toBe('任务失败')
    expect(failed.lines).toEqual(['单元还在 active，没有删文件'])
    expect(failed.hint).toBe('先看 journal')

    const interrupted = deploySummary(null, 'interrupted')
    expect(interrupted.headline).toContain('结果未知')
  })
})
