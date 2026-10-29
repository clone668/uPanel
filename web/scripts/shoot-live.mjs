// 拍真机面板：登录 → 若干页截图。用来看“用户此刻屏幕上是什么”，不是验收证据。
//
// 用法: node scripts/shoot-live.mjs https://127.0.0.1:8443 <outDir> <user> <password>
import { chromium } from 'playwright'
import { mkdirSync } from 'node:fs'
import { resolve } from 'node:path'

const [, , base = 'https://127.0.0.1:8443', outDir = '/tmp/live', user = 'admin', pass = ''] = process.argv
mkdirSync(outDir, { recursive: true })

const browser = await chromium.launch()
const ctx = await browser.newContext({ viewport: { width: 1440, height: 900 }, ignoreHTTPSErrors: true })
const page = await ctx.newPage()
const errs = []
page.on('pageerror', (e) => errs.push(e.message))

await page.goto(base + '/login', { waitUntil: 'networkidle' })
await page.fill('input[autocomplete="username"], input[type="text"]', user)
await page.fill('input[type="password"]', pass)
await page.click('button[type="submit"], button:has-text("登录")')
try {
  await page.waitForURL((u) => !u.pathname.startsWith('/login'), { timeout: 60000 })
} catch {
  console.log('登录后仍停在', page.url())
}
await page.waitForTimeout(3000)
await page.screenshot({ path: resolve(outDir, 'live-after-login.png') })
console.log('after login url=', page.url())

for (const [name, path] of [
  ['live-desk', '/desk'],
  ['live-overview', '/'],
  ['live-services', '/services'],
  ['live-storage', '/storage'],
  ['live-shares', '/shares'],
  ['live-apps', '/apps'],
]) {
  await page.goto(base + path, { waitUntil: 'networkidle' })
  await page.waitForTimeout(1200)
  await page.screenshot({ path: resolve(outDir, `${name}.png`) })
  console.log(name, 'ok')
}

console.log('pageerrors', errs.length, errs.slice(0, 3))
await browser.close()
