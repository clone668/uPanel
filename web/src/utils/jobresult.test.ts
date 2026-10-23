// 任务结果翻译的测试（P07.5）。
//
// 核心断言只有一条：**不 succeeded 就不许说成功**，而且 evidence 只能来自
// result.observed_state / result.data —— 字段缺失时宁可少写一行，也不能显示
// undefined 或者编一句“已完成”。

import { describe, expect, it } from 'vitest'

import type { JobStatus } from '@/api/jobs'
import {
  checkedViaLabel,
  summarizePackageJob,
  summarizePowerJob,
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
