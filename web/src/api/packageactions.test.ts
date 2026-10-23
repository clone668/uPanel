// 软件包写动作的前端约定（P11.2）。
//
// 这里锁的是"按钮旁边那句话"的依据，不是页面长什么样：
//   · 四个动作名与 CONTRACTS.md 第 5 节逐字对应，改动要同步两处；
//   · 包名的形状判定与代理侧 `packageNamePattern` 逐字对应——前端放行的东西代理
//     仍会拒，但前端**不该**把明显不像包名的东西送上去换一次往返；
//   · refresh 的参数必须是 `{}`，其余是 `{packages: [...]}`；
//   · 只有卸载要短语，而且这个判断与"要删几个包"无关（策略在模拟之前就定了）。

import { describe, expect, it } from 'vitest'

import {
  MAX_PACKAGE_NAMES,
  PACKAGE_ACTION_LABELS,
  PACKAGE_ACTION_VERBS,
  PACKAGE_ACTIONS,
  invalidPackageNames,
  isValidPackageName,
  packageActionNeedsPhrase,
  packageActionNeedsReauth,
  packageActionParams,
  packageActionPhraseReason,
  parsePackageNames,
} from './packageactions'

describe('动作名与参数形状', () => {
  it('四个动作名就是契约里的那四个', () => {
    expect([...PACKAGE_ACTIONS]).toEqual([
      'package.refresh',
      'package.install',
      'package.upgrade',
      'package.remove',
    ])
  })

  it('每个动作都有中文说法与确认按钮上的词', () => {
    for (const action of PACKAGE_ACTIONS) {
      expect(PACKAGE_ACTION_LABELS[action]).not.toBe('')
      expect(PACKAGE_ACTION_VERBS[action]).not.toBe('')
    }
  })

  it('refresh 的参数是空对象，且没有 packages 字段', () => {
    const params = packageActionParams('package.refresh', [])
    expect(params).toEqual({})
    expect('packages' in params).toBe(false)
  })

  it('refresh 即使被传了包名也不带上 packages', () => {
    // 这条不是理论情形：给 refresh 塞 packages，代理会按未知字段拒绝，
    // 而报错点离原因很远。
    expect(packageActionParams('package.refresh', ['cowsay'])).toEqual({})
  })

  it('其余三个动作把包名原样放进 packages', () => {
    for (const action of ['package.install', 'package.upgrade', 'package.remove'] as const) {
      expect(packageActionParams(action, ['cowsay', 'zlib1g'])).toEqual({
        packages: ['cowsay', 'zlib1g'],
      })
    }
  })

  it('参数里的是副本：调用方之后改数组不会改到已拼好的参数', () => {
    const names = ['cowsay']
    const params = packageActionParams('package.install', names)
    names.push('zlib1g')
    expect(params).toEqual({ packages: ['cowsay'] })
  })
})

describe('包名形状', () => {
  it('接受真实的包名，包括 + . - 与 :架构', () => {
    for (const name of [
      'cowsay',
      'zlib1g',
      'libssl3t64',
      'g++',
      'python3.12',
      'nginx-common',
      'libssl3t64:amd64',
      'linux-modules-extra-6.8.0-45-generic',
    ]) {
      expect(isValidPackageName(name), name).toBe(true)
    }
  })

  it('拒绝版本号、选项、大小写、通配符与空串', () => {
    for (const name of [
      '',
      ' bash',
      'bash ',
      'Bash',
      'bash=5.2',
      'bash>=5',
      '-y',
      '--force-yes',
      'foo bar',
      'foo*',
      'cowsay:',
      ':amd64',
    ]) {
      expect(isValidPackageName(name), name).toBe(false)
    }
  })

  it('一个字符不算包名（与代理的 `[a-z0-9]` + `+` 两条一致）', () => {
    expect(isValidPackageName('a')).toBe(false)
    expect(isValidPackageName('ab')).toBe(true)
  })

  it('不合法清单点名到具体那几个，而不是笼统说格式错', () => {
    expect(invalidPackageNames(['cowsay', 'bash=5.2', 'zlib1g'])).toEqual(['bash=5.2'])
    expect(invalidPackageNames([])).toEqual([])
  })
})

describe('把输入框拆成包名', () => {
  it('按空格、逗号、换行切，空项丢掉', () => {
    expect(parsePackageNames('  cowsay,  zlib1g\nnginx-common ')).toEqual([
      'cowsay',
      'zlib1g',
      'nginx-common',
    ])
  })

  it('重复项保留首次出现的位置', () => {
    expect(parsePackageNames('b a b a c')).toEqual(['b', 'a', 'c'])
  })

  it('空输入就是空数组，不留下空字符串项', () => {
    expect(parsePackageNames('')).toEqual([])
    expect(parsePackageNames('   ,,  ')).toEqual([])
  })

  it('不替用户猜 apt 的语法：`=` 与 `-` 原样留在包名里，交给形状校验去拒', () => {
    expect(parsePackageNames('bash=5.2 -y')).toEqual(['bash=5.2', '-y'])
  })
})

describe('准入门槛（界面提前提示；真正的拦截在服务端）', () => {
  it('只有 refresh 不需要再认证', () => {
    expect(packageActionNeedsReauth('package.refresh')).toBe(false)
    for (const action of ['package.install', 'package.upgrade', 'package.remove'] as const) {
      expect(packageActionNeedsReauth(action)).toBe(true)
    }
  })

  it('只有卸载要确认短语', () => {
    expect(packageActionNeedsPhrase('package.remove')).toBe(true)
    for (const action of ['package.refresh', 'package.install', 'package.upgrade'] as const) {
      expect(packageActionNeedsPhrase(action)).toBe(false)
    }
  })

  it('短语理由只在卸载时给出，且说的是"为什么这一类要停顿"', () => {
    expect(packageActionPhraseReason('package.install')).toBeNull()
    const reason = packageActionPhraseReason('package.remove')
    expect(reason).not.toBeNull()
    expect(reason).toContain('连带删除')
  })
})

describe('上限', () => {
  it('与代理的 MaxPackageNames 一致（Go 侧另有同值断言）', () => {
    expect(MAX_PACKAGE_NAMES).toBe(200)
  })
})
