// 令牌与 naive-ui 桥的防漂移测试。
//
// 首页的颜色有两个来源：tokens.css（我们的组件用）和 PALETTE（naive-ui 要用实色算派生色）。
// 两份都手写，就一定会有人只改一边。这个测试读一遍 tokens.css，逐条比对：
// PALETTE 里每个值都必须能在 CSS 里找到对应的 `--变量: 值;`，否则红。

import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'

import { describe, expect, it } from 'vitest'

import { PALETTE, buildOverrides } from '@/ui/theme'

const CSS = readFileSync(fileURLToPath(new URL('../styles/tokens.css', import.meta.url)), 'utf8')

// token 名 → PALETTE 键（两边命名风格不同，映射写在这里，看得见）
const MAP: Record<string, keyof (typeof PALETTE)['light']> = {
  '--desk': 'desk',
  '--surface': 'surface',
  '--surface-2': 'surface2',
  '--ink': 'ink',
  '--ink-2': 'ink2',
  '--ink-3': 'ink3',
  '--rule': 'rule',
  '--rule-strong': 'ruleStrong',
  '--accent': 'accent',
  '--accent-ink': 'accentInk',
  '--accent-soft': 'accentSoft',
  '--good': 'good',
  '--warn': 'warn',
  '--bad': 'bad',
  '--unknown': 'unknown',
}

function declarations(): Map<string, string[]> {
  const map = new Map<string, string[]>()
  const pattern = /(--[a-z0-9-]+)\s*:\s*([^;]+);/g
  let match: RegExpExecArray | null = pattern.exec(CSS)
  while (match !== null) {
    const name = match[1] ?? ''
    const value = (match[2] ?? '').trim().toLowerCase()
    const list = map.get(name) ?? []
    list.push(value)
    map.set(name, list)
    match = pattern.exec(CSS)
  }
  return map
}

describe('tokens.css 与 PALETTE 不漂移', () => {
  const decls = declarations()

  it('每个 token 都在 CSS 里声明过', () => {
    for (const name of Object.keys(MAP)) {
      expect(decls.has(name), `${name} 没有在 tokens.css 里声明`).toBe(true)
    }
  })

  it('浅色值对得上', () => {
    for (const [name, key] of Object.entries(MAP)) {
      const values = decls.get(name) ?? []
      const want = PALETTE.light[key].toLowerCase()
      expect(values, `${name} 的浅色值应与 PALETTE.light.${key} 一致`).toContain(want)
    }
  })

  it('深色值对得上（两套深色选择器都要有）', () => {
    for (const [name, key] of Object.entries(MAP)) {
      const values = decls.get(name) ?? []
      const want = PALETTE.dark[key].toLowerCase()
      const hits = values.filter((v) => v === want).length
      // tokens.css 里深色写了两遍：prefers-color-scheme 和 [data-theme=dark]
      expect(hits, `${name} 的深色值应出现两次且等于 PALETTE.dark.${key}`).toBe(2)
    }
  })
})

describe('buildOverrides', () => {
  it('两套主题的主色分别来自 PALETTE，且不相等', () => {
    expect(buildOverrides('light').common?.primaryColor).toBe(PALETTE.light.accent)
    expect(buildOverrides('dark').common?.primaryColor).toBe(PALETTE.dark.accent)
    expect(PALETTE.light.accent).not.toBe(PALETTE.dark.accent)
  })

  it('字体与字号骨架固定：13px 正文、等宽字体单独给', () => {
    const overrides = buildOverrides('light')
    expect(overrides.common?.fontSize).toBe('13px')
    expect(overrides.common?.fontFamily).toContain('system-ui')
    expect(overrides.common?.fontFamilyMono).toContain('ui-monospace')
  })

  it('状态色接的是 tokens 的语义色，不是 naive-ui 默认调色板', () => {
    const overrides = buildOverrides('light')
    expect(overrides.common?.successColor).toBe(PALETTE.light.good)
    expect(overrides.common?.warningColor).toBe(PALETTE.light.warn)
    expect(overrides.common?.errorColor).toBe(PALETTE.light.bad)
  })
})
