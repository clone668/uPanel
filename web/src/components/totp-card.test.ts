// 二次验证设置卡与设置页的渲染用例（P14.6）。
//
// SSR 渲不出"点一下会发生什么"，但能渲出四句必须先说清楚的话——它们各自对应服务端
// 一条真实约束，写错会让人在错的地方试：
//
//   1. 未启用时只出现登记表单，页面上不出现任何种子字符（`otpauth://`）与恢复码；
//   2. 已启用时只出现关闭表单，且第 1 步要的是**当前**六位数，第 2 步才出现；
//   3. 关闭的文案里必须写明"下一个"六位数——第 1 步的再认证已经用掉当前时间步，
//      同一个码在两步里各用一次会被服务端判成重放；
//   4. 关闭会作废这个账号的其它会话，这句话必须在按按钮之前看得见。
//
// naive-ui 在 SSR 里要挂样式（需要 document），这里换成最小实现：只把 props 与插槽
// 渲出来，属性穿透照旧，所以 `data-testid` 还能被断言。

import { describe, expect, it, vi } from 'vitest'
import { createSSRApp, h, type Component } from 'vue'
import { renderToString } from 'vue/server-renderer'
import { createPinia } from 'pinia'

import SettingsView from '@/views/SettingsView.vue'
import TotpSettingsCard from './TotpSettingsCard.vue'

vi.mock('naive-ui', async () => {
  const { h: render } = await import('vue')
  const stub = (name: string) => ({
    name: `Stub${name}`,
    props: [
      'title', 'label', 'type', 'bordered', 'size', 'checked', 'value', 'disabled',
      'loading', 'placeholder', 'attrType', 'showPasswordOn', 'autocomplete',
      'align', 'vertical', 'inputmode', 'maxlength',
    ],
    setup(
      props: Record<string, unknown>,
      ctx: { slots: Record<string, (() => unknown) | undefined> },
    ) {
      return () => {
        const children: unknown[] = []
        // NCard 的 title 与 NFormItem 的 label 都是 props，不是插槽。
        if (typeof props['title'] === 'string') children.push(props['title'])
        // NForm 的 @submit.prevent 与 NButton 的 attr-type 在这里没有行为，只有文字。
        for (const slot of [ctx.slots.label, ctx.slots.default]) {
          const rendered = slot?.()
          if (rendered !== undefined) children.push(rendered)
        }
        return render('div', { 'data-naive': name }, children as never)
      }
    },
  })
  return {
    NAlert: stub('alert'),
    NButton: stub('button'),
    NCard: stub('card'),
    NCheckbox: stub('checkbox'),
    NForm: stub('form'),
    NFormItem: stub('form-item'),
    NInput: stub('input'),
    NRadio: stub('radio'),
    NRadioGroup: stub('radio-group'),
    NSpace: stub('space'),
    NTag: stub('tag'),
  }
})

async function renderCard(enabled: boolean): Promise<string> {
  const app = createSSRApp({ render: () => h(TotpSettingsCard, { enabled }) })
  return renderToString(app)
}

async function renderSettings(): Promise<string> {
  const app = createSSRApp({ render: () => h(SettingsView as Component) })
  app.use(createPinia())
  return renderToString(app)
}

describe('未启用时', () => {
  it('只给出登记入口，状态写“未启用”', async () => {
    const html = await renderCard(false)

    expect(html).toContain('data-testid="totp-state"')
    expect(html).toContain('未启用')
    expect(html).toContain('data-testid="totp-enroll-form"')
    expect(html).toContain('data-testid="totp-password"')
    expect(html).toContain('data-testid="totp-enroll-start"')
    // 没有登记就没有 URI、没有恢复码、没有关闭表单。
    expect(html).not.toContain('data-testid="totp-uri"')
    expect(html).not.toContain('otpauth://')
    expect(html).not.toContain('data-testid="totp-recovery"')
    expect(html).not.toContain('data-testid="totp-disable-form"')
  })

  it('提醒先对时，并写明丢手机时还有两条退路', async () => {
    const html = await renderCard(false)

    expect(html).toContain('时间')
    expect(html).toContain('恢复码')
    expect(html).toContain('upanelctl')
  })
})

describe('已启用时', () => {
  it('只给出关闭表单，状态写“已启用”', async () => {
    const html = await renderCard(true)

    expect(html).toContain('已启用')
    expect(html).toContain('data-testid="totp-disable-form"')
    expect(html).toContain('data-testid="totp-disable-password"')
    expect(html).not.toContain('data-testid="totp-enroll-form"')
    expect(html).not.toContain('data-testid="totp-enroll-start"')
  })

  it('第 1 步要当前验证码，第 2 步的输入框这时还不出现', async () => {
    const html = await renderCard(true)

    expect(html).toContain('data-testid="totp-disable-reauth-code"')
    expect(html).not.toContain('data-testid="totp-disable-second-factor"')
    expect(html).toContain('第 1 步：再认证')
  })

  it('文案里写明第 2 步要用“下一个”六位数，以及关闭会作废其它会话', async () => {
    const html = await renderCard(true)

    expect(html).toContain('下一个')
    expect(html).toContain('作废')
    expect(html).toContain('其它会话')
  })
})

describe('设置页', () => {
  it('把状态从会话里取出来交给卡片，并说明目前只有这一块', async () => {
    const html = await renderSettings()

    expect(html).toContain('data-testid="totp-card"')
    expect(html).toContain('本机账号安全')
    // 会话是 'unknown'（未登录），卡片就是未启用那一态：不凭空显示已启用。
    expect(html).toContain('未启用')
    expect(html).not.toContain('data-testid="settings-refresh-error"')
  })
})
