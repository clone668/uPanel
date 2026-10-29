// 多页预览截图：拿**真实抓取的夹具**把每一页都画一遍。
//
// 这是开发用的预览工具，不是验收证据：/api/v1/** 的响应全部由夹具回放，
// 浏览器里的画面跟真实 VM 上跑出来的不是一回事。验收仍然只能在 Ubuntu 测试 VM 上做。
// 夹具没覆盖的接口一律 404，页面就应该显示"读不到"——不替它编一个成功响应。
// 目前没有夹具的页面：/jobs、/backup、/alerts、/schedules、/network、/settings/host、
// /terminal、/apps/deployments、/accounts。这几页在预览里只能看到"读不到"，
// 它们的正常状态必须在 Ubuntu 测试 VM 上看（宿主 + 夹具不算验收）。
//
// 用法：node scripts/preview-pages.mjs <baseUrl> <outDir> [页名…]
// 前置：vite dev server 已在 <baseUrl> 上跑着（npm run dev）。

import { chromium } from 'playwright'
import { readFileSync, mkdirSync } from 'node:fs'
import { resolve } from 'node:path'

const [, , base = 'http://127.0.0.1:5173', outDir = '/tmp/shots', ...only] = process.argv
mkdirSync(outDir, { recursive: true })

function fixture(name) {
  return JSON.parse(readFileSync(resolve(import.meta.dirname, '../src/fixtures', name), 'utf8'))
}

const SESSION = {
  request_id: 'preview-session',
  data: {
    user: { id: 1, username: 'admin', display_name: '管理员', role: 'admin' },
    csrf_token: 'preview-csrf',
    expires_at: '2026-12-31T00:00:00Z',
    reauth_expires_at: null,
    totp_enabled: false,
  },
}

// 顺序有意义：先具体后一般（/storage/devices/sda 必须排在 /storage/devices 前面）。
const ROUTES = [
  [/\/api\/v1\/auth\/session$/, SESSION],
  // 这一条不是抓来的：handleHealthLive 的响应体只有一个字段（internal/api/router.go），
  // 照它写成 {status:"ok"}，request_id 由中间件补。抓不到就不编，这一条属于"照代码抄"。
  [/\/api\/v1\/health\/live$/, { data: { status: 'ok' }, request_id: 'preview-health' }],
  [/\/api\/v1\/system\/summary$/, fixture('p04-4-vm/summary.json')],
  [/\/api\/v1\/storage\/topology$/, fixture('p04-4-vm/topology.json')],
  [/\/api\/v1\/storage\/devices\/[^/?]+$/, fixture('p04-4-vm/detail-sda.json')],
  [/\/api\/v1\/storage\/devices$/, fixture('p04-4-vm/devices.json')],
  [/\/api\/v1\/services\/[^/?]+$/, fixture('p05-1-vm/detail-protected-ssh.json')],
  [/\/api\/v1\/services$/, fixture('p05-1-vm/services.json')],
  [/\/api\/v1\/processes$/, fixture('p05-2-vm/processes-first.json')],
  [/\/api\/v1\/logs$/, fixture('p05-3-vm/logs-basic.json')],
  [/\/api\/v1\/files\/roots$/, fixture('p06-1-vm/files-roots.json')],
  [/\/api\/v1\/files\/list$/, fixture('p06-1-vm/files-list-root.json')],
  [/\/api\/v1\/shares$/, fixture('p08-4-vm/shares.json')],
  [/\/api\/v1\/apps$/, fixture('p09-5-vm/apps.json')],
  [/\/api\/v1\/packages\/updates$/, fixture('p11-1-vm/packages-updates.json')],
  [/\/api\/v1\/app-groups$/, fixture('p09-5-vm/app-groups.json')],
  [/\/api\/v1\/apps\/discovery$/, fixture('p09-5-vm/discovery.json')],
  [/\/api\/v1\/storage\/volumes$/, fixture('p08-4-vm/volumes.json')],
  [/\/api\/v1\/share-accounts$/, fixture('p08-4-vm/accounts.json')],
  [/\/api\/v1\/packages$/, fixture('p11-1-vm/packages-page1.json')],
  [/\/api\/v1\/system\/power-state$/, fixture('p11-1-vm/power-state.json')],
]

const PAGES = [
  { name: 'page-desk', path: '/desk' },
  { name: 'page-overview', path: '/' },
  { name: 'page-storage', path: '/storage' },
  { name: 'page-storage-device', path: '/storage/devices/sda' },
  { name: 'page-services', path: '/services' },
  { name: 'page-service-unit', path: '/services/ssh.service' },
  { name: 'page-processes', path: '/processes' },
  { name: 'page-files', path: '/files' },
  { name: 'page-shares', path: '/shares' },
  { name: 'page-apps', path: '/apps' },
  { name: 'page-updates', path: '/updates' },
  { name: 'page-logs', path: '/logs' },
  { name: 'page-jobs', path: '/jobs' },
  { name: 'page-backup', path: '/backup' },
  { name: 'page-alerts', path: '/alerts' },
  { name: 'page-schedules', path: '/schedules' },
  { name: 'page-network', path: '/network' },
  { name: 'page-settings', path: '/settings' },
  { name: 'page-host', path: '/settings/host' },
  { name: 'page-terminal', path: '/terminal' },
  { name: 'page-deployments', path: '/apps/deployments' },
  { name: 'page-accounts', path: '/accounts' },
  { name: 'page-notfound', path: '/no-such-page' },
  { name: 'page-services-mobile', path: '/services', width: 400, height: 820 },
  { name: 'page-desk-mobile', path: '/desk', width: 400, height: 820 },
  { name: 'page-overview-mobile', path: '/', width: 400, height: 820 },
  { name: 'page-storage-dark', path: '/storage', dark: true },
  { name: 'page-apps-dark', path: '/apps', dark: true },
]

const browser = await chromium.launch()
const results = []

async function shoot({ name, path, width = 1440, height = 900, dark = false }) {
  const page = await browser.newPage({ viewport: { width, height }, deviceScaleFactor: 1 })
  if (dark) await page.emulateMedia({ colorScheme: 'dark' })
  const errors = []
  const unmatched = []
  page.on('pageerror', (e) => errors.push('pageerror: ' + e.message))

  await page.route('**/api/v1/**', async (route) => {
    // 查询串不参与匹配：/apps?include=… 与 /apps 是同一个接口。
    const url = route.request().url().split('?')[0]
    for (const [pattern, body] of ROUTES) {
      if (pattern.test(url)) {
        return route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(body) })
      }
    }
    unmatched.push(url)
    return route.fulfill({
      status: 404,
      contentType: 'application/json',
      body: JSON.stringify({ error: { code: 'NOT_FOUND', message: '预览夹具未覆盖该接口' } }),
    })
  })

  await page.goto(base + path, { waitUntil: 'networkidle' })
  await page.waitForTimeout(600)
  const out = resolve(outDir, `${name}.png`)
  await page.screenshot({ path: out })
  const info = await page.evaluate(() => ({
    scrollW: document.documentElement.scrollWidth,
    clientW: document.documentElement.clientWidth,
    scrollH: document.documentElement.scrollHeight,
    h1: document.querySelector('.win h1')?.textContent?.trim() ?? null,
  }))
  results.push({
    name,
    path,
    out,
    errors,
    unmatched: [...new Set(unmatched)],
    overflow: info.scrollW - info.clientW,
    scrollH: info.scrollH,
    h1: info.h1,
  })
  await page.close()
}

for (const page of PAGES) {
  if (only.length > 0 && !only.includes(page.name)) continue
  await shoot(page)
}

console.log(JSON.stringify(results, null, 2))
await browser.close()
