// 存储写动作的页面侧纯逻辑测试（P07.5）。
//
// 这里断言的是“页面能不能把参数拼对、能不能把服务端的判决翻译对”：
//   - 字节一律十进制字符串（2^53 以上不丢位）；
//   - 分区起点/大小按 1 MiB 对齐，超界要在页面就拦住；
//   - 卷标按文件系统的**字节**上限校验（超限会被 mkfs 静默截断）；
//   - unsupported 不画按钮、blocked 画出来但灰着并照抄 reason、缺字段不当成可用。

import { describe, expect, it } from 'vitest'

import {
  FILESYSTEM_LABEL_LIMITS,
  MAX_PARTITION_NAME_LENGTH,
  PARTITION_ALIGN_BYTES,
  STORAGE_ACTIONS,
  STORAGE_ACTION_LABELS,
  STORAGE_PHRASE_VERBS,
  bytesToMib,
  capabilityGate,
  checkFilesystemLabel,
  checkPartitionSize,
  createPartitionParams,
  deletePartitionParams,
  formatParams,
  initTableParams,
  isValidFilesystemLabel,
  isValidPartitionName,
  maxMibInRegion,
  mibToBytes,
  storageActionNeedsReauth,
  type StorageCapability,
} from './storage'

function deviceWith(capabilities: StorageCapability[]): { capabilities: StorageCapability[] } {
  return { capabilities }
}

describe('动作名与短语动词', () => {
  it('四个动作都有中文标签', () => {
    for (const action of Object.values(STORAGE_ACTIONS)) {
      expect(STORAGE_ACTION_LABELS[action]).toBeTruthy()
    }
  })

  it('新建分区不需要确认短语，其余三个各有动词', () => {
    expect(STORAGE_PHRASE_VERBS['storage.partition.create']).toBe('')
    expect(STORAGE_PHRASE_VERBS['storage.partition_table.initialize']).toBe('INIT')
    expect(STORAGE_PHRASE_VERBS['storage.partition.delete']).toBe('DELETE')
    expect(STORAGE_PHRASE_VERBS['storage.filesystem.format']).toBe('FORMAT')
  })

  it('设备侧四个动作都要再认证；未知动作不要（不额外要密码）', () => {
    expect(storageActionNeedsReauth(STORAGE_ACTIONS.initTable)).toBe(true)
    expect(storageActionNeedsReauth(STORAGE_ACTIONS.createPartition)).toBe(true)
    expect(storageActionNeedsReauth(STORAGE_ACTIONS.deletePartition)).toBe(true)
    expect(storageActionNeedsReauth(STORAGE_ACTIONS.format)).toBe(true)
    expect(storageActionNeedsReauth('storage.volume.import')).toBe(false)
    expect(storageActionNeedsReauth('storage.unknown')).toBe(false)
  })
})

describe('参数形状', () => {
  it('初始化分区表只认 gpt', () => {
    expect(initTableParams({ deviceId: 'sdb' })).toEqual({ device_id: 'sdb', table_type: 'gpt' })
  })

  it('新建分区：字节是十进制字符串，名字空着就不发', () => {
    expect(
      createPartitionParams({ deviceId: 'sdb', startBytes: '1048576', sizeBytes: '1073741824' }),
    ).toEqual({ device_id: 'sdb', start_bytes: '1048576', size_bytes: '1073741824' })
    expect(
      createPartitionParams({
        deviceId: 'sdb',
        startBytes: '1048576',
        sizeBytes: '1073741824',
        name: '   ',
      }).name,
    ).toBeUndefined()
    expect(
      createPartitionParams({
        deviceId: 'sdb',
        startBytes: '1048576',
        sizeBytes: '1073741824',
        name: ' data ',
      }).name,
    ).toBe('data')
  })

  it('删除分区只要设备标识', () => {
    expect(deletePartitionParams({ deviceId: 'sdb1' })).toEqual({ device_id: 'sdb1' })
  })

  it('格式化：卷标空着就不发，不让服务端去猜一个默认名', () => {
    expect(formatParams({ deviceId: 'sdb1', filesystem: 'ext4' })).toEqual({
      device_id: 'sdb1',
      filesystem: 'ext4',
    })
    expect(formatParams({ deviceId: 'sdb1', filesystem: 'xfs', label: '  ' }).label).toBeUndefined()
    expect(formatParams({ deviceId: 'sdb1', filesystem: 'xfs', label: ' data ' }).label).toBe('data')
  })
})

describe('MiB 与字节的换算', () => {
  it('MiB 换字节是十进制字符串，1 MiB 对齐', () => {
    expect(mibToBytes(1)).toBe(PARTITION_ALIGN_BYTES.toString())
    expect(mibToBytes(1024)).toBe('1073741824')
    expect(mibToBytes(0)).toBe('0')
    expect(mibToBytes(-5)).toBe('0')
    expect(mibToBytes(1.5)).toBe('0')
  })

  it('超大值不丢位（2^53 以上也不），因为走的是 BigInt', () => {
    const huge = mibToBytes(10_000_000_000)
    expect(huge).toBe('10485760000000000')
    expect(bytesToMib(huge)).toBe(10_000_000_000)
  })

  it('字节串换 MiB 向下取整，非法输入返回 0', () => {
    expect(bytesToMib('1073741824')).toBe(1024)
    expect(bytesToMib('1073741825')).toBe(1024)
    expect(bytesToMib('abc')).toBe(0)
    expect(bytesToMib('')).toBe(0)
    expect(maxMibInRegion('2147483648')).toBe(2048)
  })
})

describe('分区大小校验', () => {
  const region = '10737418240' // 10 GiB

  it('正常大小通过，并给出要发的字节串', () => {
    const check = checkPartitionSize(region, 4096)
    expect(check.ok).toBe(true)
    expect(check.sizeBytes).toBe('4294967296')
  })

  it('0、负数、小数都拒', () => {
    expect(checkPartitionSize(region, 0).ok).toBe(false)
    expect(checkPartitionSize(region, -1).ok).toBe(false)
    expect(checkPartitionSize(region, 1.5).ok).toBe(false)
    expect(checkPartitionSize(region, 0).reason).toBe('大小必须是正整数 MiB')
  })

  it('超过空闲区就拒，并说明原因', () => {
    const check = checkPartitionSize(region, 10241)
    expect(check.ok).toBe(false)
    expect(check.reason).toBe('超过这个空闲区的可用大小')
  })

  it('刚好等于空闲区大小是允许的', () => {
    expect(checkPartitionSize(region, 10240).ok).toBe(true)
  })

  it('空闲区大小本身不是合法字节串时拒绝计算', () => {
    expect(checkPartitionSize('12.5', 1).reason).toBe('空闲区大小不是合法的字节串，无法计算')
  })
})

describe('分区名校验', () => {
  it('允许可打印 ASCII，空串视为“不填”', () => {
    expect(isValidPartitionName('')).toBe(true)
    expect(isValidPartitionName('data')).toBe(true)
    expect(isValidPartitionName('my data-1')).toBe(true)
  })

  it('拒引号、反斜杠与非 ASCII（fstab/sfdisk 会误解）', () => {
    expect(isValidPartitionName('da"ta')).toBe(false)
    expect(isValidPartitionName('da\\ta')).toBe(false)
    expect(isValidPartitionName('数据')).toBe(false)
    expect(isValidPartitionName('tab\tname')).toBe(false)
  })

  it('超过 36 字符就拒', () => {
    expect(isValidPartitionName('a'.repeat(MAX_PARTITION_NAME_LENGTH))).toBe(true)
    expect(isValidPartitionName('a'.repeat(MAX_PARTITION_NAME_LENGTH + 1))).toBe(false)
    expect(MAX_PARTITION_NAME_LENGTH).toBe(36)
  })
})

describe('卷标校验按文件系统的字节上限', () => {
  it('ext4 上限 16 字节、XFS 12 字节', () => {
    expect(FILESYSTEM_LABEL_LIMITS).toEqual({ ext4: 16, xfs: 12 })
    expect(isValidFilesystemLabel('ext4', 'a'.repeat(16))).toBe(true)
    expect(isValidFilesystemLabel('ext4', 'a'.repeat(17))).toBe(false)
    expect(isValidFilesystemLabel('xfs', 'a'.repeat(12))).toBe(true)
    expect(isValidFilesystemLabel('xfs', 'a'.repeat(13))).toBe(false)
  })

  it('按字节不是按字符：中文一个字符占 3 字节', () => {
    const check = checkFilesystemLabel('xfs', '数据')
    expect(check.ok).toBe(false)
    expect(check.reason).toContain('只能包含可打印 ASCII')
    // 6 个汉字 = 18 字节，超的是字节上限这条，不是字符数（6 < 16）。
    expect(checkFilesystemLabel('ext4', '数据盘卷标一').reason).toContain('最多 16 字节')
    // 5 个汉字 = 15 字节，没超上限，改由“只能可打印 ASCII”拒。两条规则的先后要能分清。
    expect(checkFilesystemLabel('ext4', '数据盘卷标').reason).toContain('只能包含可打印 ASCII')
  })

  it('不能含 /，空串合法', () => {
    expect(checkFilesystemLabel('ext4', 'a/b').ok).toBe(false)
    expect(checkFilesystemLabel('ext4', '').ok).toBe(true)
  })

  it('边界：正好到字节上限的 ASCII 通过', () => {
    expect(checkFilesystemLabel('ext4', 'a'.repeat(16)).ok).toBe(true)
    expect(checkFilesystemLabel('ext4', 'a'.repeat(17)).reason).toBe(
      'ext4 的卷标最多 16 字节，当前 17 字节',
    )
  })
})

describe('capability 的三态翻译', () => {
  it('available → 可点', () => {
    expect(
      capabilityGate(deviceWith([{ action: STORAGE_ACTIONS.format, state: 'available' }]), STORAGE_ACTIONS.format),
    ).toEqual({ gate: 'enabled', reason: null })
  })

  it('unsupported → 不画按钮（对整盘格式化这类）', () => {
    const gate = capabilityGate(
      deviceWith([
        { action: STORAGE_ACTIONS.format, state: 'unsupported', reason: '这是整盘，没有可格式化的文件系统' },
      ]),
      STORAGE_ACTIONS.format,
    )
    expect(gate.gate).toBe('hidden')
    expect(gate.reason).toBe('这是整盘，没有可格式化的文件系统')
  })

  it('blocked → 画出来但灰着，reason 照抄服务端的原话', () => {
    const gate = capabilityGate(
      deviceWith([{ action: STORAGE_ACTIONS.deletePartition, state: 'blocked', reason: '分区正在被挂载' }]),
      STORAGE_ACTIONS.deletePartition,
    )
    expect(gate.gate).toBe('disabled')
    expect(gate.reason).toBe('分区正在被挂载')
  })

  it('blocked 但没给 reason 时有兜底文案，不显示 null', () => {
    const gate = capabilityGate(
      deviceWith([{ action: STORAGE_ACTIONS.deletePartition, state: 'blocked' }]),
      STORAGE_ACTIONS.deletePartition,
    )
    expect(gate.gate).toBe('disabled')
    expect(gate.reason).toBe('当前状态下不允许')
  })

  it('载荷里没有这个动作 → 灰着并说明版本不一致，不猜成可用', () => {
    const gate = capabilityGate(deviceWith([]), STORAGE_ACTIONS.format)
    expect(gate.gate).toBe('disabled')
    expect(gate.reason).toContain('代理与页面版本可能不一致')
  })
})
