// 外壳预览截图：用**真实抓取的夹具**（src/fixtures 下的 VM 原始响应）跑一遍外壳。
//
// 这是开发用的预览工具，不是验收证据：它拦截 /api/v1/** 的响应，浏览器里的画面
// 由夹具供给，跟真实 VM 上跑出来的不是一回事。验收仍然只能在 Ubuntu 测试 VM 上做。
//
// 用法：node scripts/preview-shell.mjs <baseUrl> <outDir>
// 前置：vite dev server 已经在 <baseUrl> 上跑着（npm run dev）。

import { chromium } from 'playwright'
import { readFileSync, mkdirSync } from 'node:fs'
import { resolve } from 'node:path'
const [, , base = 'http://127.0.0.1:5173', outDir = '/tmp/shots'] = process.argv
mkdirSync(outDir, { recursive: true })

function fixture(name) {
  const path = resolve(import.meta.dirname, '../src/fixtures', name)
  return JSON.parse(readFileSync(path, 'utf8'))
}

const SESSION = {
  // 成功信封必须是 {data, request_id}：缺 request_id 会被客户端判成
  // INVALID_RESPONSE（契约就是契约，预览也不钻空子）。
  request_id: 'preview-session',
  data: {
    user: { id: 1, username: 'admin', display_name: '管理员', role: 'admin' },
    csrf_token: 'preview-csrf',
    expires_at: '2026-12-31T00:00:00Z',
    reauth_expires_at: null,
    totp_enabled: false,
  },
}

// 只覆盖概览页真正会打的几个接口；其余一律 404，页面就应该显示"读不到"，
// 而不是我给它编一个成功的响应。
const ROUTES = [
  [/\/api\/v1\/auth\/session$/, SESSION],
  [/\/api\/v1\/system\/summary$/, fixture('p04-4-vm/summary.json')],
  [/\/api\/v1\/storage\/devices$/, fixture('p04-4-vm/devices.json')],
]

const browser = await chromium.launch()
const results = []

async function shoot({ name, width, height, clicks = [], settle = 700 }) {
  const page = await browser.newPage({
    viewport: { width, height },
    deviceScaleFactor: 1,
  })
  const errors = []
  page.on('pageerror', (e) => errors.push('pageerror: ' + e.message))
  page.on('console', (m) => {
    if (m.type() === 'error') errors.push('console: ' + m.text())
  })

  await page.route('**/api/v1/**', async (route) => {
    const url = route.request().url()
    for (const [pattern, body] of ROUTES) {
      if (pattern.test(url)) {
        return route.fulfill({
          status: 200,
          contentType: 'application/json',
          body: JSON.stringify(body),
        })
      }
    }
    return route.fulfill({
      status: 404,
      contentType: 'application/json',
      body: JSON.stringify({ error: { code: 'NOT_FOUND', message: '预览夹具未覆盖该接口' } }),
    })
  })

  await page.goto(base, { waitUntil: 'networkidle' })
  for (const selector of clicks) {
    await page.click(selector)
    await page.waitForTimeout(180)
  }
  await page.waitForTimeout(settle)

  const out = resolve(outDir, `${name}.png`)
  await page.screenshot({ path: out })
  const info = await page.evaluate(() => ({
    scrollW: document.documentElement.scrollWidth,
    clientW: document.documentElement.clientWidth,
    scrollH: document.documentElement.scrollHeight,
    title: document.title,
  }))
  results.push({ name, out, errors, info })
  await page.close()
}

await shoot({ name: 'shell-overview', width: 1440, height: 900 })
await shoot({ name: 'shell-launcher', width: 1440, height: 900, clicks: ['.bar__brand'] })
await shoot({
  name: 'shell-dark',
  width: 1440,
  height: 900,
  // 主题按钮循环 auto → light → dark：点两次就落在"夜间"，走的是用户真实的路径，
  // 不是从外面硬写 data-theme（那样 PALETTE 驱动的色块会停在浅色值上）。
  clicks: ['button:has-text("主题")', 'button:has-text("主题")'],
})
await shoot({ name: 'shell-storage', width: 1440, height: 900, clicks: ['.dock__item:nth-of-type(4)'] })
await shoot({ name: 'shell-mobile', width: 400, height: 820 })

console.log(JSON.stringify(results, null, 2))
await browser.close()
