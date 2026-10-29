import { chromium } from 'playwright'

const [, , url, out, w, h, theme, clickSel] = process.argv
const browser = await chromium.launch()
const page = await browser.newPage({ viewport: { width: Number(w || 1280), height: Number(h || 900) }, deviceScaleFactor: 1 })
const errors = []
page.on('pageerror', (e) => errors.push('pageerror: ' + e.message))
page.on('console', (m) => { if (m.type() === 'error') errors.push('console: ' + m.text()) })
await page.goto(url, { waitUntil: 'networkidle' })
if (theme) await page.evaluate((t) => document.documentElement.setAttribute('data-theme', t), theme)
if (clickSel) {
  for (const sel of clickSel.split('|')) { await page.click(sel); await page.waitForTimeout(150) }
}
await page.waitForTimeout(600)
await page.screenshot({ path: out, fullPage: false })
const info = await page.evaluate(() => ({
  scrollW: document.documentElement.scrollWidth,
  clientW: document.documentElement.clientWidth,
  bg: getComputedStyle(document.body).backgroundColor,
}))
console.log(JSON.stringify({ out, errors, info }, null, 2))
await browser.close()
