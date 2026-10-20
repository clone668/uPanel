// 基础页面冒烟（P01.3 起，P01.4 扩展为可指向真实 Go 二进制）。
//
// 两种用法：
//   1) 预览模式（默认）：vite preview 托管 web/dist（或 --mock 时 dist-mock）。没有后端，
//      /api/v1 请求必然失败 —— 用来验证“失败也要如实显示”。
//   2) 同源模式：--base http://127.0.0.1:9443（或 18881），直接打真正在跑的 upanel-web。
//      这时验证“静态页 + 同源 API + 成功信封”的完整链路。
//
// 运行：
//   npm --prefix web run build && npm --prefix web run smoke        # 预览 + production 产物
//   npm --prefix web run smoke:mock                                 # mock 构建 + 横幅检查
//   node web/scripts/smoke.mjs --base http://127.0.0.1:18881 --expect mock --no-build
//
// 依赖说明：使用 package.json 已冻结的 @playwright/test（自带 chromium 启动器），
// 不引入新的测试框架。浏览器二进制需已安装（npx playwright install chromium）。
//
// 范围：HTTP + 渲染层冒烟；不是真实 Ubuntu 系统功能验收。

import { spawn } from 'node:child_process'
import { fileURLToPath } from 'node:url'
import path from 'node:path'

import { chromium } from '@playwright/test'

const WEB_DIR = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
const VITE_BIN = path.join(WEB_DIR, 'node_modules', 'vite', 'bin', 'vite.js')
const HOST = '127.0.0.1'
const PORT = 4173

const ARGS = process.argv.slice(2)

function argValue(name) {
  const index = ARGS.indexOf(name)
  return index >= 0 ? ARGS[index + 1] : undefined
}

const BASE_OVERRIDE = argValue('--base')
const EXPECT = argValue('--expect') ?? (ARGS.includes('--mock') ? 'mock' : 'production')
const MOCK = EXPECT === 'mock'
const EXTERNAL_BASE = typeof BASE_OVERRIDE === 'string'

const BASE = EXTERNAL_BASE ? BASE_OVERRIDE.replace(/\/$/, '') : `http://${HOST}:${PORT}`
const OUT_DIR = MOCK ? 'dist-mock' : 'dist'
const SPAWN_PREVIEW = !EXTERNAL_BASE

const results = []

function check(name, ok, detail = '') {
  results.push({ name, ok })
  const suffix = detail === '' ? '' : `（${detail}）`
  process.stdout.write(`<== ${ok ? 'PASS' : 'FAIL'}  ${name}${suffix}\n`)
}

function runVite(args, env = process.env) {
  return new Promise((resolve) => {
    const child = spawn(process.execPath, [VITE_BIN, ...args], {
      cwd: WEB_DIR,
      env,
      stdio: ['ignore', 'pipe', 'pipe'],
    })
    const log = []
    child.stdout.on('data', (chunk) => log.push(chunk.toString()))
    child.stderr.on('data', (chunk) => log.push(chunk.toString()))
    child.on('close', (code) => resolve({ code, log: log.join('') }))
  })
}

async function waitForServer(timeoutMs = 30_000) {
  const deadline = Date.now() + timeoutMs
  while (Date.now() < deadline) {
    try {
      const response = await fetch(`${BASE}/`, { redirect: 'manual' })
      if (response.status === 200) return true
    } catch {
      // 服务还没起来，继续等。
    }
    await new Promise((resolve) => setTimeout(resolve, 250))
  }
  return false
}

if (MOCK && !EXTERNAL_BASE && !ARGS.includes('--no-build')) {
  process.stdout.write(`==> 构建 mock 产物（VITE_UPANEL_MODE=mock → ${OUT_DIR}）\n`)
  const built = await runVite(
    ['build', '--outDir', OUT_DIR, '--emptyOutDir'],
    { ...process.env, VITE_UPANEL_MODE: 'mock' },
  )
  if (built.code !== 0) {
    process.stdout.write(built.log)
    process.stdout.write('<== FAIL  mock 构建失败\n')
    process.exit(1)
  }
  process.stdout.write('<== PASS  mock 构建成功\n')
}

let server
const serverLog = []
if (SPAWN_PREVIEW) {
  server = spawn(
    process.execPath,
    [VITE_BIN, 'preview', '--host', HOST, '--port', String(PORT), '--strictPort', '--outDir', OUT_DIR],
    { cwd: WEB_DIR, stdio: ['ignore', 'pipe', 'pipe'] },
  )
  server.stdout.on('data', (chunk) => serverLog.push(chunk.toString()))
  server.stderr.on('data', (chunk) => serverLog.push(chunk.toString()))
}

let browser

try {
  if (!(await waitForServer())) {
    process.stdout.write(`目标服务未在 30 秒内就绪：${BASE}\n`)
    if (serverLog.length > 0) process.stdout.write(serverLog.join(''))
    process.exitCode = 1
    throw new Error('preview-not-ready')
  }

  browser = await chromium.launch({ headless: true })
  const page = await browser.newPage()

  const pageErrors = []
  page.on('pageerror', (error) => pageErrors.push(String(error)))
  const consoleErrors = []
  page.on('console', (message) => {
    if (message.type() === 'error') consoleErrors.push(message.text())
  })
  const badResponses = []
  page.on('response', (response) => {
    if (response.status() >= 400) badResponses.push(`${response.status()} ${response.url()}`)
  })

  // ---------------------------------------------------------------- 概览页
  const indexResponse = await page.goto(`${BASE}/`, { waitUntil: 'networkidle' })
  check('根路径返回 200', indexResponse?.status() === 200, `HTTP ${indexResponse?.status()}`)
  check('页面语言为 zh-CN', (await page.getAttribute('html', 'lang')) === 'zh-CN')
  const title = (await page.title()).trim()
  check('页面有标题', title.length > 0, title)

  await page.locator('#app').waitFor({ state: 'visible' })
  check('前端挂载成功（#app 有内容）', (await page.locator('#app').innerHTML()).length > 0)

  const bodyText = await page.locator('body').innerText()
  check(
    '显示面板品牌与副标题',
    bodyText.includes('uPanel') && bodyText.includes('单台 Ubuntu 服务器管理面板'),
  )

  const menuText = (await page.locator('.n-menu').innerText()).replace(/\s+/g, ' ').trim()
  check(
    '导航只包含已实现模块（概览），未实现模块被隐藏',
    menuText.includes('概览') &&
      !menuText.includes('存储') &&
      !menuText.includes('终端') &&
      !menuText.includes('设置'),
    menuText,
  )

  if (MOCK) {
    check(
      'mock 构建持续显示演示模式横幅',
      bodyText.includes('演示模式：数据为演示数据，未操作服务器'),
    )
    check('概览页标注 mock 模式', bodyText.includes('模式：mock（演示数据）'))
  } else {
    check('production 构建不显示演示模式横幅', !bodyText.includes('演示模式'))
    check('概览页标注运行模式', bodyText.includes('模式：production'))
  }

  if (EXTERNAL_BASE) {
    // 真后端：成功信封必须被完整消费（status + request_id 都来自服务端）。
    check('存活检查成功（服务可响应）', bodyText.includes('服务可响应'))
    check('页面展示服务端返回的 request_id', bodyText.includes('request_id：'))
    check('成功路径不应出现错误码', !bodyText.includes('错误码：'))
    check(
      '运行模式与后端一致',
      MOCK ? bodyText.includes('模式：mock（演示数据）') : bodyText.includes('模式：production'),
    )
  } else {
    // 预览服务器没有后端：契约校验必须拒绝这次响应，而不是当作成功。
    check('非契约响应被判为 INVALID_RESPONSE，而不是当作成功', bodyText.includes('INVALID_RESPONSE'))
    check(
      '失败时给出错误码与说明',
      bodyText.includes('错误码：') && bodyText.includes('响应不符合接口契约'),
    )
  }
  check('没有停留在加载中', !bodyText.includes('状态：加载中'))

  // ---------------------------------------------------------------- 404 页
  await page.goto(`${BASE}/storage`, { waitUntil: 'networkidle' })
  const notFoundText = await page.locator('body').innerText()
  check('未实现模块的路径落到 404 页面', notFoundText.includes('页面不存在'))
  check('404 页面也渲染完整外壳', notFoundText.includes('uPanel'))
  if (MOCK) {
    check('切换页面后 mock 横幅仍在（不可忽略）', notFoundText.includes('演示模式'))
  }

  check('没有未捕获的页面异常', pageErrors.length === 0, pageErrors.join(' | '))
  if (EXTERNAL_BASE) {
    check('同源请求没有失败项', badResponses.length === 0, badResponses.join(' | '))
    check('没有控制台错误', consoleErrors.length === 0, consoleErrors.join(' | '))
  } else {
    // 预览环境缺少 /api/v1 后端，健康检查必然失败 —— 这是本脚本刻意制造的场景。
    // 要求的是：除这一次预期的 API 探测外，没有别的请求失败。
    const unexpectedResponses = badResponses.filter((entry) => !entry.includes('/api/v1/'))
    check('除预期的 API 探测外没有失败请求', unexpectedResponses.length === 0, unexpectedResponses.join(' | '))
    const unexpectedConsole = consoleErrors.filter((text) => !text.includes('Failed to load resource'))
    check('没有其他控制台错误', unexpectedConsole.length === 0, unexpectedConsole.join(' | '))
  }
} catch (error) {
  if (String(error) !== 'Error: preview-not-ready') {
    check('冒烟脚本执行完成', false, String(error))
  }
} finally {
  if (browser) await browser.close()
  if (server) server.kill()
}

const failed = results.filter((result) => !result.ok).length
const target = EXTERNAL_BASE ? BASE : `${OUT_DIR} 预览`
process.stdout.write(
  `\nPASS ${results.length - failed} / FAIL ${failed}（共 ${results.length} 项，${MOCK ? 'mock' : 'production'}，目标 ${target}）\n`,
)
if (failed > 0) process.exitCode = 1
