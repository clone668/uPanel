import { chromium } from 'playwright'
import { readFileSync, writeFileSync, mkdtempSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'

const [, , file, out, w, h, theme, clickSel] = process.argv
const body = readFileSync(file, 'utf8')
const dir = mkdtempSync(join(tmpdir(), 'shot-'))
const wrapped = join(dir, 'page.html')
writeFileSync(wrapped, `<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<style>html,body{margin:0}</style>
</head><body>${body}</body></html>`)

const browser = await chromium.launch()
const page = await browser.newPage({ viewport: { width: Number(w || 1440), height: Number(h || 900) }, deviceScaleFactor: 1 })
const errors = []
page.on('pageerror', (e) => errors.push('pageerror: ' + e.message))
page.on('console', (m) => { if (m.type() === 'error') errors.push('console: ' + m.text()) })
await page.goto('file://' + wrapped.replace(/\\/g, '/'), { waitUntil: 'networkidle' })
if (theme) await page.evaluate((t) => document.documentElement.setAttribute('data-theme', t), theme)
if (clickSel) {
  const sel = clickSel.split('|')
  for (const s of sel) { await page.click(s); await page.waitForTimeout(80) }
}
await page.waitForTimeout(350)
await page.screenshot({ path: out, fullPage: false })
const overflow = await page.evaluate(() => ({
  scrollW: document.documentElement.scrollWidth,
  clientW: document.documentElement.clientWidth
}))
console.log(JSON.stringify({ out, errors, overflow }, null, 2))
await browser.close()
