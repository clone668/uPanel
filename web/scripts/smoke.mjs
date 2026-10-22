// 页面冒烟（P01.3 起；P04.4 起覆盖登录后的概览与存储页）。
//
// 三种用法：
//   1) 预览模式（默认）：vite preview 托管 web/dist（或 --mock 时 dist-mock）。没有后端，
//      /api/v1 请求必然失败 —— 用来验证“未登录必须落回登录页，失败也要如实显示”。
//   2) 同源模式 + 账号：--base http://127.0.0.1:18881 --login-user admin --login-pass …
//      直接打真正在跑的 upanel-web，登录后逐页检查渲染。
//      账号也可以从环境变量读：UPANEL_SMOKE_USER / UPANEL_SMOKE_PASS。
//   3) mock 构建 + 真二进制：npm run smoke:mock（无账号时只验证登录闸门与品牌）。
//
// 运行：
//   npm --prefix web run build && npm --prefix web run smoke
//   npm --prefix web run smoke:mock
//   node web/scripts/smoke.mjs --base http://127.0.0.1:18881 --no-build \
//     --login-user admin --login-pass correct-horse-9
//
// 依赖说明：使用 package.json 已冻结的 @playwright/test（自带 chromium 启动器），
// 不引入新的测试框架。浏览器二进制需已安装（npx playwright install chromium）。
//
// 范围：HTTP + 渲染层冒烟；不是真实 Ubuntu 系统功能验收。
// 代理不可用时（mock 二进制、没有 agent socket），存储页必须显示“依赖未安装”，
// 这时设备相关断言记为 SKIP，而不是当成通过。

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
const LOGIN_USER = argValue('--login-user') ?? process.env.UPANEL_SMOKE_USER
const LOGIN_PASS = argValue('--login-pass') ?? process.env.UPANEL_SMOKE_PASS
const AUTH = typeof LOGIN_USER === 'string' && LOGIN_USER !== '' && typeof LOGIN_PASS === 'string'

const BASE = EXTERNAL_BASE ? BASE_OVERRIDE.replace(/\/$/, '') : `http://${HOST}:${PORT}`
const OUT_DIR = MOCK ? 'dist-mock' : 'dist'
const SPAWN_PREVIEW = !EXTERNAL_BASE

const results = []

function check(name, ok, detail = '') {
  results.push({ name, ok, kind: 'check' })
  const suffix = detail === '' ? '' : `（${detail}）`
  process.stdout.write(`<== ${ok ? 'PASS' : 'FAIL'}  ${name}${suffix}\n`)
}

function skip(name, reason) {
  results.push({ name, ok: true, kind: 'skip' })
  process.stdout.write(`<== SKIP  ${name}: ${reason}\n`)
}

/**
 * 等页面离开“加载中”再读文字。
 *
 * 为什么要等：视图是挂载后才发请求的（networkidle 只保证文档与静态资源停了），
 * 直接读 innerText 会读到骨架，断言就会变成“测到加载中的样子”。
 * 超时也照样返回当时的文字 —— 后面的断言会把真实状态说出来，而不是掩盖。
 */
async function settle(page, timeoutMs = 20_000) {
  const deadline = Date.now() + timeoutMs
  let text = await page.locator('body').innerText()
  while (Date.now() < deadline && text.includes('状态：加载中')) {
    await page.waitForTimeout(200)
    text = await page.locator('body').innerText()
  }
  return text
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

  // ---------------------------------------------------------------- 外壳与登录闸门
  const indexResponse = await page.goto(`${BASE}/`, { waitUntil: 'networkidle' })
  check('根路径返回 200', indexResponse?.status() === 200, `HTTP ${indexResponse?.status()}`)
  check('页面语言为 zh-CN', (await page.getAttribute('html', 'lang')) === 'zh-CN')
  const title = (await page.title()).trim()
  check('页面有标题', title.length > 0, title)

  await page.locator('#app').waitFor({ state: 'visible' })
  check('前端挂载成功（#app 有内容）', (await page.locator('#app').innerHTML()).length > 0)

  const shellText = await page.locator('body').innerText()
  check(
    '显示面板品牌与副标题',
    shellText.includes('uPanel') && shellText.includes('单台 Ubuntu 服务器管理面板'),
  )

  if (MOCK) {
    check('mock 构建持续显示演示模式横幅', shellText.includes('演示模式：数据为演示数据，未操作服务器'))
  } else {
    check('production 构建不显示演示模式横幅', !shellText.includes('演示模式'))
  }

  // 未登录时必须落在登录页：直接深链接同样要被挡住（不泄漏任何页面骨架以外的内容）。
  const loginShown = async () => {
    await page.waitForURL(/\/login/, { timeout: 15_000 }).catch(() => {})
    const text = await page.locator('body').innerText()
    return text.includes('登录 uPanel')
  }

  if (AUTH) {
    check('未登录访问 / 落到登录页', await loginShown())
    await page.goto(`${BASE}/storage`, { waitUntil: 'networkidle' })
    const blocked = await page.locator('body').innerText()
    check('未登录访问 /storage 同样落到登录页', blocked.includes('登录 uPanel'))
  } else {
    check('未登录访问 / 落到登录页', await loginShown())
  }

  if (AUTH) {
    // -------------------------------------------------------------- 登录
    await page.fill('[data-testid="login-username"] input', LOGIN_USER)
    await page.fill('[data-testid="login-password"] input', LOGIN_PASS)
    await page.click('[data-testid="login-submit"]')
    await page.waitForURL((url) => !url.pathname.startsWith('/login'), { timeout: 20_000 })
    check(
      '登录后回到被拦截的地址（/storage）',
      new URL(page.url()).pathname === '/storage',
      page.url(),
    )

    const menuText = (await page.locator('.n-menu').innerText()).replace(/\s+/g, ' ').trim()
    check(
      '登录后导航包含已实现模块（概览、存储、任务中心）',
      menuText.includes('概览') && menuText.includes('存储') && menuText.includes('任务中心'),
      menuText,
    )
    check(
      '导航不包含未实现模块（终端、设置）',
      !menuText.includes('终端') && !menuText.includes('设置'),
      menuText,
    )

    // -------------------------------------------------------------- 概览页
    await page.goto(`${BASE}/`, { waitUntil: 'networkidle' })
    const overviewText = await settle(page)
    check('概览页渲染标题与模块说明', overviewText.includes('概览') && overviewText.includes('系统概况'))
    check('概览页显示面板服务连通性', overviewText.includes('面板服务'))
    check('概览页没有停在“加载中”', !overviewText.includes('状态：加载中'))
    check(
      '运行模式与后端一致',
      MOCK ? overviewText.includes('模式：mock（演示数据）') : overviewText.includes('模式：production'),
    )

    // 代理不可用时，系统与存储都必须显示“依赖未安装”，绝不能出现 0。
    const agentUnavailable = overviewText.includes('AGENT_UNAVAILABLE')
    if (agentUnavailable) {
      check('代理不可用时概览显示依赖未安装', overviewText.includes('依赖未安装'))
      check('代理不可用时概览给出错误码', overviewText.includes('错误码：AGENT_UNAVAILABLE'))
      check('代理不可用时概览说明原因', overviewText.includes('未配置代理连接'))
      check('代理不可用时概览不出现假的 0%', !/已用（可用口径）[\s\S]{0,40}0 B/.test(overviewText))
      skip('概览页显示采集时间与刷新节奏', '代理不可用（mock 二进制没有 agent socket），没有系统快照')
    } else {
      check('概览页显示真实系统快照', overviewText.includes('主机名') && overviewText.includes('逻辑核'))
      check(
        '概览页显示采集时间与刷新节奏',
        /采集时间：/.test(overviewText) && /每 \d+ 毫秒刷新/.test(overviewText),
        overviewText.match(/采集时间：[^\n]*/)?.[0] ?? '',
      )
    }

    // -------------------------------------------------------------- 存储页
    await page.goto(`${BASE}/storage`, { waitUntil: 'networkidle' })
    const storageText = await settle(page)
    check('存储页渲染标题', storageText.includes('存储'))
    check('存储页没有停在“加载中”', !storageText.includes('状态：加载中'))

    if (agentUnavailable) {
      check('代理不可用时存储页显示依赖未安装', storageText.includes('依赖未安装'))
      check(
        '代理不可用时存储页给出错误码与说明',
        storageText.includes('AGENT_UNAVAILABLE') && storageText.includes('未配置代理连接'),
      )
      check(
        '代理不可用时存储页不显示任何设备卡片',
        (await page.locator('[data-device-id]').count()) === 0,
      )
      skip('存储页渲染设备卡片与 diskstats 口径说明', '代理不可用，拓扑卡片整体不渲染')
      skip(
        '存储页渲染设备卡片、健康与容量',
        '代理不可用（mock 二进制没有 agent socket），没有真实设备可渲染',
      )
      skip('设备卡片与详情页跳转', '同上：没有设备可点')
    } else {
      check(
        '存储页说明容量与 diskstats 口径',
        storageText.includes('IEC') && storageText.includes('diskstats'),
      )
      const deviceCount = await page.locator('[data-device-id]').count()
      check('存储页渲染到设备卡片', deviceCount > 0, `${deviceCount} 台`)
      check('存储页显示容量单位', /(GiB|MiB|TiB|B)/.test(storageText))
      // 受管卷区块（P07.5）：读完了要有结论——要么是卷，要么是“还没有受管的卷”，
      // 不能停在「正在读取受管卷」。
      check(
        '存储页的受管卷区块读到了结果',
        storageText.includes('受管卷') && !storageText.includes('正在读取受管卷'),
      )
      if (deviceCount > 0) {
        const firstId = await page.locator('[data-device-id]').first().getAttribute('data-device-id')
        await page.locator('[data-device-id] a').first().click()
        await page.waitForURL(/\/storage\/devices\//, { timeout: 15_000 })
        const detailText = await settle(page)
        check(
          '设备详情页渲染身份与容量',
          detailText.includes('身份与介质') && detailText.includes('容量与挂载'),
        )
        check('设备详情页有健康与 I/O 区块', detailText.includes('健康') && detailText.includes('I/O'))
        // P07.5 起这个区块的标题是「服务端判定的动作」：页面不再自己说“没有可执行动作”，
        // 而是把判决层给的四种判定（available/blocked/unsupported）与理由摆出来。
        check('设备详情页显示服务端判定的动作', detailText.includes('服务端判定的动作'))
        check('设备详情页显示设备名', detailText.includes(firstId ?? ''), firstId ?? '')

        await page.click('text=返回磁盘列表')
        await page.waitForURL(/\/storage$/, { timeout: 15_000 })
        check('可以从详情页返回磁盘列表', true)
      } else {
        skip('设备卡片与详情页跳转', '代理可用但没有识别到块设备')
      }
    }

    // -------------------------------------------------------------- 深链接与 404
    await page.goto(`${BASE}/storage/devices/sda`, { waitUntil: 'networkidle' })
    const deepLinkText = await settle(page)
    check(
      '设备深链接不会落到“页面不存在”',
      !deepLinkText.includes('页面不存在'),
      agentUnavailable ? '代理不可用 → 依赖未安装' : '',
    )
    if (agentUnavailable) {
      check('代理不可用时设备详情页显示依赖未安装', deepLinkText.includes('依赖未安装'))
      skip('设备详情页的“设备不存在”空状态', '代理不可用，拿不到拓扑，无法区分“不存在”与“读不到”')
    } else {
      check(
        '设备详情页给出可读状态（详情或明确的不存在）',
        deepLinkText.includes('身份与介质') || deepLinkText.includes('设备不存在'),
      )
    }

    await page.goto(`${BASE}/nope`, { waitUntil: 'networkidle' })
    const notFoundText = await page.locator('body').innerText()
    check('未知路径落到 404 页面', notFoundText.includes('页面不存在'))
    check('404 页面也渲染完整外壳', notFoundText.includes('uPanel'))
  }

  // ---------------------------------------------------------------- 全局错误
  check('没有未捕获的页面异常', pageErrors.length === 0, pageErrors.join(' | '))
  if (EXTERNAL_BASE) {
    const unexpected = badResponses.filter(
      (entry) => !entry.startsWith('401 ') && !entry.startsWith('404 ') && !entry.startsWith('503 '),
    )
    check('除预期的 401/404/503 外没有失败请求', unexpected.length === 0, unexpected.join(' | '))
    const unexpectedConsole = consoleErrors.filter((text) => !text.includes('Failed to load resource'))
    check('没有其他控制台错误', unexpectedConsole.length === 0, unexpectedConsole.join(' | '))
  } else {
    // 预览环境没有 /api/v1 后端，会话恢复必然失败 —— 这是本脚本刻意制造的场景。
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
const skipped = results.filter((result) => result.kind === 'skip').length
const target = EXTERNAL_BASE ? BASE : `${OUT_DIR} 预览`
process.stdout.write(
  `\nPASS ${results.length - failed - skipped} / FAIL ${failed} / SKIP ${skipped}` +
    `（共 ${results.length} 项，${MOCK ? 'mock' : 'production'}，目标 ${target}，` +
    `${AUTH ? '已登录' : '未登录'}）\n`,
)
if (failed > 0) process.exitCode = 1
