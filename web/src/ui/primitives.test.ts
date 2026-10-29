// 设计系统原子件的契约测试。
//
// 环境是 node（冻结依赖里没有 jsdom，见 vitest.config.ts 与 DECISIONS Q21），
// 所以组件用 vue/server-renderer 渲染成字符串断言：覆盖模板分支、属性绑定、文案，
// 不覆盖点击/焦点。点击里唯一真正要守的规则（灰着的按钮不许提交）抽成了纯函数
// isInert，在这里直接测。
//
// 守的几件事：
//   - 禁用/加载必须落到 DOM 上（disabled / aria-disabled / aria-busy），不能只改颜色；
//   - 有 href 时是真链接，禁用时把 href 拿掉（点不动就不假装能点）；
//   - 容量为 null 时**不画**填充条（没读到 ≠ 0%）；
//   - 状态永远是“点 + 词”，词必须真的在 DOM 里。

import { describe, expect, it } from 'vitest'
import { createSSRApp, h, type Component } from 'vue'
import { renderToString } from 'vue/server-renderer'

import UBar from '@/ui/UBar.vue'
import UButton from '@/ui/UButton.vue'
import UKpi from '@/ui/UKpi.vue'
import UPanel from '@/ui/UPanel.vue'
import UState from '@/ui/UState.vue'
import UTag from '@/ui/UTag.vue'
import { isInert } from '@/ui/button'

async function render(
  component: Component,
  props: Record<string, unknown> = {},
  slots: Record<string, string> = {},
): Promise<string> {
  return renderToString(
    createSSRApp({
      render: () => h(component, props, Object.fromEntries(Object.entries(slots).map(([k, v]) => [k, () => v]))),
    }),
  )
}

describe('isInert', () => {
  it('禁用或加载时都不许提交', () => {
    expect(isInert(true, false)).toBe(true)
    expect(isInert(false, true)).toBe(true)
    expect(isInert(true, true)).toBe(true)
  })

  it('两者都否才能按', () => {
    expect(isInert(false, false)).toBe(false)
    expect(isInert()).toBe(false)
  })
})

describe('UButton', () => {
  it('默认是 button 元素，type=button（不在表单里误提交）', async () => {
    const html = await render(UButton, {}, { default: '刷新' })
    expect(html).toContain('<button')
    expect(html).toContain('type="button"')
    expect(html).toContain('u-btn--secondary')
    expect(html).toContain('刷新')
  })

  it('disabled 落到 DOM 上', async () => {
    const html = await render(UButton, { disabled: true }, { default: '格式化' })
    expect(html).toContain('disabled')
  })

  it('loading 时 aria-busy=true 并带转圈元素', async () => {
    const html = await render(UButton, { loading: true }, { default: '提交中' })
    expect(html).toContain('aria-busy="true"')
    expect(html).toContain('u-btn__spin')
  })

  it('带 href 时渲染成链接，target=_blank 自动补 noopener', async () => {
    const html = await render(
      UButton,
      { href: 'http://127.0.0.1:8080', target: '_blank' },
      { default: '打开' },
    )
    expect(html).toContain('<a')
    expect(html).toContain('href="http://127.0.0.1:8080"')
    expect(html).toContain('rel="noopener noreferrer"')
    expect(html).not.toContain('<button')
  })

  it('链接禁用时不带 href，但仍标 aria-disabled（存在但按不动）', async () => {
    const html = await render(
      UButton,
      { href: 'http://127.0.0.1:8080', disabled: true },
      { default: '打开' },
    )
    expect(html).not.toContain('href="http://127.0.0.1:8080"')
    expect(html).toContain('aria-disabled="true"')
  })

  it('四种含义各自有 data-variant，方便按含义查而不是按颜色查', async () => {
    for (const variant of ['primary', 'secondary', 'quiet', 'danger'] as const) {
      const html = await render(UButton, { variant }, { default: variant })
      expect(html).toContain(`data-variant="${variant}"`)
    }
  })
})

describe('UBar', () => {
  it('percent=null 时不画填充，并标成 unknown', async () => {
    const html = await render(UBar, { percent: null })
    expect(html).not.toContain('u-bar__fill')
    expect(html).toContain('data-tone="unknown"')
  })

  it('percent=0 反而要画：0% 是一个真实读数', async () => {
    const html = await render(UBar, { percent: 0 })
    expect(html).toContain('u-bar__fill')
    expect(html).toContain('width:0%')
  })

  it('按阈值选色：62% 强调、80% 警告、95% 危险', async () => {
    expect(await render(UBar, { percent: 62 })).toContain('data-tone="accent"')
    expect(await render(UBar, { percent: 80 })).toContain('data-tone="warn"')
    expect(await render(UBar, { percent: 95 })).toContain('data-tone="bad"')
  })

  it('超过 100 的读数被夹到 100，不撑破容器', async () => {
    const html = await render(UBar, { percent: 143 })
    expect(html).toContain('width:100%')
  })

  it('给了 label 就有读屏用的 aria-label，没读到也有说法', async () => {
    expect(await render(UBar, { percent: 62, label: '根分区已用' })).toContain(
      'aria-label="根分区已用：62%"',
    )
    expect(await render(UBar, { percent: null, label: '根分区已用' })).toContain(
      'aria-label="根分区已用：没有读数"',
    )
  })
})

describe('UState / UTag', () => {
  it('状态一定带文字，颜色只是附加', async () => {
    const html = await render(UState, { tone: 'warn' }, { default: '证书待续' })
    expect(html).toContain('data-tone="warn"')
    expect(html).toContain('证书待续')
  })

  it('标签把语气放进属性，不写死在 class 里', async () => {
    const html = await render(UTag, { tone: 'bad' }, { default: 'SYSTEM_DISK_PROTECTED' })
    expect(html).toContain('data-tone="bad"')
    expect(html).toContain('u-tag--mono')
  })

  it('中文词组可以关掉等宽', async () => {
    const html = await render(UTag, { mono: false }, { default: '受保护' })
    expect(html).not.toContain('u-tag--mono')
  })
})

describe('UKpi', () => {
  it('数值与单位分开渲染，脚注走插槽', async () => {
    const html = await render(
      UKpi,
      { label: '根分区 /', value: '62', unit: '%' },
      { foot: '12.4 / 20.0 GiB' },
    )
    expect(html).toContain('u-kpi__value')
    expect(html).toContain('u-kpi__unit')
    expect(html).toContain('12.4 / 20.0 GiB')
  })

  it('没有单位时不渲染空的 small', async () => {
    const html = await render(UKpi, { label: '负载', value: '0.34' })
    expect(html).not.toContain('u-kpi__unit')
  })

  it('数值原样输出，组件不替调用方把 null 变成 0', async () => {
    const html = await render(UKpi, { label: '容量', value: '—' })
    expect(html).toContain('—')
    expect(html).not.toContain('>0<')
  })
})

describe('UPanel', () => {
  it('加载时保留标题，内容区换成骨架，并且是可读的 status', async () => {
    const html = await render(UPanel, { title: '磁盘', loading: true }, { default: '不应出现' })
    expect(html).toContain('磁盘')
    expect((html.match(/u-panel__bar/g) ?? []).length).toBe(3)
    expect(html).toContain('role="status"')
    expect(html).not.toContain('不应出现')
  })

  it('flush 时不加内边距（表格自己贴边）', async () => {
    expect(await render(UPanel, { flush: true })).toContain('u-panel__body--flush')
  })
})
