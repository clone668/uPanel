// 主题桥：把 tokens.css 那套颜色接到 naive-ui 上。
//
// 为什么必须两边都写：naive-ui 的组件（Button/Tag/Input/DataTable…）自己算派生色，
// 只给它 `var(--accent)` 这种字符串它算不出 hover/pressed，所以这里必须有**实色**。
// 于是颜色有两个来源：CSS 变量给我们的组件用，PALETTE 给 naive-ui 用。
// 两份不许漂移——src/ui/tokens.test.ts 会读 tokens.css 逐条比对，漂了就红。
//
// 主题模式三态（auto/light/dark）：auto 跟随系统，显式选择写进 <html data-theme>，
// 与 tokens.css 里的选择器一一对应。用户选择存 localStorage，读不到就当没存过。

import { computed, ref, watch } from 'vue'

import type { GlobalThemeOverrides } from 'naive-ui'
import { darkTheme } from 'naive-ui'

export type ThemeMode = 'auto' | 'light' | 'dark'
export type ResolvedTheme = 'light' | 'dark'

/** 与 tokens.css 一一对应的实色（naive-ui 需要能参与计算的颜色值）。 */
export const PALETTE = {
  light: {
    desk: '#e9e6dd',
    surface: '#fbfaf6',
    surface2: '#f4f2ec',
    ink: '#16181b',
    ink2: '#4a4f57',
    ink3: '#83888f',
    rule: '#e0dbcf',
    ruleStrong: '#c3bcab',
    accent: '#1f47c9',
    accentInk: '#ffffff',
    accentSoft: '#e6eaf9',
    good: '#16724a',
    warn: '#a55a00',
    bad: '#a8261f',
    unknown: '#6b6f76',
  },
  dark: {
    desk: '#0a0c0f',
    surface: '#16191d',
    surface2: '#1c2026',
    ink: '#ecebe4',
    ink2: '#a6a9ad',
    ink3: '#74787d',
    rule: '#2a2f36',
    ruleStrong: '#3d434b',
    accent: '#829bff',
    accentInk: '#0b0f1f',
    accentSoft: '#1b2240',
    good: '#4fbf8f',
    warn: '#e0a03c',
    bad: '#ff7b70',
    unknown: '#8b8f94',
  },
} as const

const FONT_SANS =
  "system-ui, -apple-system, 'Segoe UI', 'Noto Sans SC', 'PingFang SC', 'Microsoft YaHei', sans-serif"
const FONT_MONO = "ui-monospace, 'Cascadia Code', 'JetBrains Mono', Consolas, monospace"

/** naive-ui 的全局变量覆盖。只覆盖“看得见的骨架”，不逐组件重画。 */
export function buildOverrides(theme: ResolvedTheme): GlobalThemeOverrides {
  const c = PALETTE[theme]
  return {
    common: {
      fontFamily: FONT_SANS,
      fontFamilyMono: FONT_MONO,
      fontSize: '13px',
      fontSizeMini: '11px',
      fontSizeTiny: '11px',
      fontSizeSmall: '12px',
      fontSizeMedium: '13px',
      fontSizeLarge: '15px',
      fontWeightStrong: '600',
      lineHeight: '1.5',

      primaryColor: c.accent,
      primaryColorHover: theme === 'light' ? '#2f56d6' : '#93a9ff',
      primaryColorPressed: theme === 'light' ? '#1a3cad' : '#7190f5',
      primaryColorSuppl: c.accent,

      bodyColor: c.surface,
      cardColor: c.surface,
      modalColor: c.surface,
      popoverColor: c.surface,
      tableColor: c.surface,
      inputColor: c.surface,
      actionColor: c.surface2,

      borderColor: c.rule,
      dividerColor: c.rule,
      hoverColor: c.surface2,

      textColorBase: c.ink,
      textColor1: c.ink,
      textColor2: c.ink2,
      textColor3: c.ink3,
      textColorDisabled: c.ink3,

      successColor: c.good,
      warningColor: c.warn,
      errorColor: c.bad,
      infoColor: c.accent,

      borderRadius: '4px',
      borderRadiusSmall: '2px',
    },
    Button: {
      // 按钮的“身高 / 内边距 / 方角”全部放在这里，**不放在 CSS 里**。
      // 原因：naive 把 `--n-height`、`--n-padding`、`--n-border-radius` 写进元素的
      // style 属性，只有 !important 能盖过它；而文字按钮（`text`/`link`）恰恰是靠把
      // 这三个变量设成 `initial` 来“跟着行内文字走”的。用 !important 或高特异性
      // 规则去改，文字按钮就会被撑成 28px 高、左缩 12px 的实心块。
      // themeOverrides 喂的正是这些内联变量，所以这里改的是“默认值”，
      // 文字按钮把默认值清空的行为原样保留。
      borderRadiusTiny: '2px',
      borderRadiusSmall: '2px',
      borderRadiusMedium: '2px',
      borderRadiusLarge: '2px',
      heightTiny: '22px',
      heightSmall: '26px',
      heightMedium: '28px',
      heightLarge: '32px',
      paddingTiny: '0 6px',
      paddingSmall: '0 9px',
      paddingMedium: '0 12px',
      paddingLarge: '0 14px',
      fontWeight: '500',
    },
    Card: {
      borderRadius: '4px',
      color: c.surface,
      borderColor: c.rule,
    },
    Tag: {
      borderRadius: '2px',
      colorBordered: 'transparent',
    },
    Input: {
      borderRadius: '2px',
      border: `1px solid ${c.rule}`,
      borderHover: `1px solid ${c.ruleStrong}`,
      borderFocus: `1px solid ${c.accent}`,
      boxShadowFocus: `0 0 0 2px ${c.accentSoft}`,
      caretColor: c.accent,
    },
    DataTable: {
      thColor: c.surface2,
      thTextColor: c.ink3,
      thFontWeight: '500',
      tdColorHover: c.surface2,
      borderColor: c.rule,
    },
    Menu: {
      itemTextColor: c.ink2,
    },
    Progress: {
      // 轨道必须看得见：容量条只画填充时，14% 的填充在纸面上就是一根孤零零的短线，
      // 看图的人分不清“用了 14%”和“这里出错了”。surface2 与 surface 只差 7% 亮度，
      // 在浅色主题下等于没有轨道。
      railColor: c.rule,
    },
  }
}

const STORAGE_KEY = 'upanel.theme'

function readStored(): ThemeMode {
  if (typeof window === 'undefined') return 'auto'
  try {
    const raw = window.localStorage.getItem(STORAGE_KEY)
    if (raw === 'auto' || raw === 'light' || raw === 'dark') return raw
  } catch {
    // 隐私模式/存储被禁：当作没存过，不影响页面渲染。
  }
  return 'auto'
}

function systemPrefersDark(): boolean {
  if (typeof window === 'undefined' || typeof window.matchMedia !== 'function') return false
  return window.matchMedia('(prefers-color-scheme: dark)').matches
}

/**
 * 主题控制器：写 <html data-theme>、记住选择、算出 naive-ui 该用哪套。
 * 单例——整个应用只应该有一个 data-theme 来源。
 */
const mode = ref<ThemeMode>(readStored())
const systemDark = ref(systemPrefersDark())

if (typeof window !== 'undefined' && typeof window.matchMedia === 'function') {
  const query = window.matchMedia('(prefers-color-scheme: dark)')
  const onChange = (event: MediaQueryListEvent) => {
    systemDark.value = event.matches
  }
  if (typeof query.addEventListener === 'function') query.addEventListener('change', onChange)
}

export const resolvedTheme = computed<ResolvedTheme>(() => {
  if (mode.value === 'light') return 'light'
  if (mode.value === 'dark') return 'dark'
  return systemDark.value ? 'dark' : 'light'
})

// auto 时**不写** data-theme，让 CSS 的 prefers-color-scheme 分支生效；
// 显式选择时才钉住，两套主题不会互相打架。
watch(
  resolvedTheme,
  (next) => {
    if (typeof document === 'undefined') return
    const root = document.documentElement
    if (mode.value === 'auto') root.removeAttribute('data-theme')
    else root.setAttribute('data-theme', next)
  },
  { immediate: true },
)

export function useTheme() {
  const overrides = computed(() => buildOverrides(resolvedTheme.value))
  const naiveTheme = computed(() => (resolvedTheme.value === 'dark' ? darkTheme : null))

  function setMode(next: ThemeMode): void {
    mode.value = next
    if (typeof window === 'undefined') return
    try {
      window.localStorage.setItem(STORAGE_KEY, next)
    } catch {
      // 存不下就算了：这次会话照样生效。
    }
  }

  function cycle(): void {
    const order: ThemeMode[] = ['auto', 'light', 'dark']
    setMode(order[(order.indexOf(mode.value) + 1) % order.length] ?? 'auto')
  }

  return { mode, resolved: resolvedTheme, overrides, naiveTheme, setMode, cycle }
}
