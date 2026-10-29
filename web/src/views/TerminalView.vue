<script setup lang="ts">
// 受限 Web 终端（P06.3，CONTRACTS.md 第 10 节）。
//
// 这条页面把三件事摆明：
//   1) **它不是 root 终端**。会话以 upanel-shell 运行，uid 由服务端下发并显示出来；
//      面板里没有任何“提权”入口，也没有 sudo 密码框可以填。
//   2) **开会话是一次受管动作**：预检 → （需要时）密码再认证 → 任务 → 拿到 session_id。
//      页面上没有“直接开一个 shell”的按钮。
//   3) **连接要一次性许可**：许可由带 CSRF 的 POST 领取，随 WebSocket 子协议进握手，
//      30 秒、只能用一次。它永远不出现在 URL 里（那会落进访问日志与浏览器历史）。
//
// 帧是结构化的（input/resize 出、ready/output/exit/error 入）。xterm 只负责把字节
// 画成终端；它拿到的每一帧都是从同一套 DTO 解出来的，没有第二条通道。

import { computed, nextTick, onBeforeUnmount, onMounted, ref } from 'vue'
import { NAlert, NButton, NCard, NInput, NModal, NTag } from 'naive-ui'
import { Terminal } from '@xterm/xterm'
import { FitAddon } from '@xterm/addon-fit'
import '@xterm/xterm/css/xterm.css'

import { reauthPassword } from '@/api/auth'
import { ApiError } from '@/api/client'
import {
  isTerminal as isTerminalJob,
  jobErrorMessage,
  type JobDTO,
} from '@/api/jobs'
import {
  createPreflight,
  fetchJobStatus,
  newIdempotencyKey,
  submitJob,
  type PreflightData,
} from '@/api/preflights'
import {
  DEFAULT_COLS,
  DEFAULT_ROWS,
  closeTerminalSession,
  createTerminalGrant,
  decodeTerminalFrame,
  encodeInputFrame,
  encodeResizeFrame,
  exitReasonLabel,
  fetchTerminalSessions,
  openTerminalSocket,
  outputBytes,
  sessionStateLabel,
  type TerminalSessionsData,
} from '@/api/terminal'
import ApiErrorPanel from '@/components/ApiErrorPanel.vue'
import SecondFactorField from '@/components/SecondFactorField.vue'
import StatePanel from '@/components/StatePanel.vue'
import { useSessionStore } from '@/stores/session'
import { formatTimestamp } from '@/utils/format'
import { apiPanelState } from '@/utils/viewstate'

type Phase = 'loading' | 'ready' | 'error'
/** 连接状态：没有任何连接 / 正在领许可并拨号 / 已连上 / 已断开 / 出错。 */
type ConnState = 'idle' | 'opening' | 'open' | 'closed' | 'error'

/** 任务轮询：500 ms 一次，最多 30 次（15 秒）。创建终端是一次 fork+exec，不该更久。 */
const POLL_INTERVAL_MS = 500
const POLL_LIMIT = 30

const session = useSessionStore()

const phase = ref<Phase>('loading')
const error = ref<ApiError | null>(null)
const data = ref<TerminalSessionsData | null>(null)

const actionError = ref<string | null>(null)
const jobMessage = ref('')
const busy = ref(false)

const pendingPreflight = ref<PreflightData | null>(null)
const reauthVisible = ref(false)
const reauthBusy = ref(false)
const reauthError = ref<string | null>(null)
const password = ref('')
// 启用二次验证的账号还要在这一步带上六位数（服务端的两步入口同样管再认证）。
const reauthCode = ref('')
const reauthReady = computed(() => password.value !== '' && (!session.totpEnabled || reauthCode.value !== ''))

const connState = ref<ConnState>('idle')
const connError = ref<string | null>(null)
const frameError = ref<string | null>(null)
const exitInfo = ref<{ reason: string; exitCode: number | null; signal: number | null } | null>(null)
const readyInfo = ref<{ account: string; accountUid: number; sessionId: string } | null>(null)

const hostRef = ref<HTMLElement | null>(null)

// xterm 实例与 socket 用普通变量保存：Vue 的响应式代理会包住它们内部的对象，
// xterm 的渲染循环不接受被代理的 DOM/Canvas 引用。
let term: Terminal | null = null
let fitAddon: FitAddon | null = null
let observer: ResizeObserver | null = null
let socket: WebSocket | null = null
let disposed = false

const sessions = computed(() => data.value?.sessions ?? [])
/** 每个管理员最多一个会话（CONTRACTS.md 第 10 节），因此这里取第一个。 */
const activeSession = computed(() => sessions.value[0] ?? null)
const accountLabel = computed(() => {
  const info = data.value
  if (info === null) return ''
  return `${info.account}（uid ${info.account_uid}）`
})
const limitsLine = computed(() => {
  const info = data.value
  if (info === null) return ''
  const idle = Math.round(info.idle_timeout_seconds / 60)
  const maxMin = Math.round(info.max_duration_seconds / 60)
  return `无输入 ${idle} 分钟自动关闭 · 最长 ${maxMin} 分钟 · 断流宽限 ${info.disconnect_grace_seconds} 秒 · 每账号 ${info.max_per_user} 个、全局 ${info.max_sessions} 个`
})
const panelState = computed(() => (error.value ? apiPanelState(error.value) : null))
const canCreate = computed(() => session.isAdmin && activeSession.value === null && !busy.value)
const connected = computed(() => connState.value === 'open')

async function load(): Promise<void> {
  try {
    const result = await fetchTerminalSessions()
    data.value = result.data
    phase.value = 'ready'
    error.value = null
  } catch (caught) {
    if (caught instanceof ApiError) {
      error.value = caught
      phase.value = 'error'
      return
    }
    throw caught
  }
}

// ---------------------------------------------------------------- 创建

/** 走预检：结论、阻塞项与“是否需要再认证”全部来自服务端。 */
async function startCreate(): Promise<void> {
  actionError.value = null
  jobMessage.value = ''
  busy.value = true
  try {
    const pre = await createPreflight('terminal.create', {
      cols: DEFAULT_COLS,
      rows: DEFAULT_ROWS,
    })
    pendingPreflight.value = pre.data
    jobMessage.value = ''
    if (!pre.data.allowed) return
    if (pre.data.requires_reauth) {
      password.value = ''
      reauthCode.value = ''
      reauthError.value = null
      reauthVisible.value = true
      return
    }
    await submitPending()
  } catch (caught) {
    actionError.value = errorText(caught)
  } finally {
    busy.value = false
  }
}

/** 提交任务并等到拿到 session_id。 */
async function submitPending(): Promise<void> {
  const pre = pendingPreflight.value
  if (pre === null) return
  busy.value = true
  actionError.value = null
  jobMessage.value = '正在创建终端会话…'
  try {
    const submitted = await submitJob({
      preflightId: pre.preflight_id,
      idempotencyKey: newIdempotencyKey(),
    })
    const sessionId = await trackJob(submitted.data.job_id)
    pendingPreflight.value = null
    readyInfo.value = null
    exitInfo.value = null
    connError.value = null
    frameError.value = null
    await connect(sessionId)
    await load()
  } catch (caught) {
    if (caught instanceof ApiError && caught.code === 'REAUTH_REQUIRED') {
      // 再认证窗口在提交时已经过期：重新要密码，再提交同一份预检（120 秒内仍有效）。
      password.value = ''
      reauthCode.value = ''
      reauthError.value = null
      reauthVisible.value = true
      actionError.value = '再认证已过期，请重新验证密码后再提交'
      return
    }
    actionError.value = errorText(caught)
    jobMessage.value = ''
  } finally {
    busy.value = false
  }
}

/** 轮询任务直到终态，成功时返回新会话 id。 */
async function trackJob(jobId: string): Promise<string> {
  for (let attempt = 0; attempt < POLL_LIMIT; attempt += 1) {
    if (disposed) throw new Error('页面已关闭')
    const result = await fetchJobStatus(jobId)
    const job: JobDTO = result.data
    if (!isTerminalJob(job.status)) {
      jobMessage.value = `任务 ${job.status}${job.step ? `（${job.step}）` : ''}…`
      await sleep(POLL_INTERVAL_MS)
      continue
    }
    if (job.status !== 'succeeded') {
      const detail = jobErrorMessage(job)
      throw new Error(detail?.message ?? '任务失败（代理未提供说明）')
    }
    const sessionId = readSessionId(job)
    if (sessionId === null) {
      throw new Error('任务成功但没有返回 session_id：请到任务中心核对这次创建')
    }
    return sessionId
  }
  throw new Error('任务在 15 秒内没有到达终态，请到任务中心核对')
}

/** 从任务结果里取 session_id（result.data.session_id）。 */
function readSessionId(job: JobDTO): string | null {
  const dataValue = job.result['data']
  if (dataValue === null || typeof dataValue !== 'object') return null
  const raw = (dataValue as Record<string, unknown>)['session_id']
  return typeof raw === 'string' && raw !== '' ? raw : null
}

async function confirmReauth(): Promise<void> {
  if (!reauthReady.value) return
  reauthBusy.value = true
  reauthError.value = null
  try {
    await reauthPassword(password.value, reauthCode.value === '' ? undefined : reauthCode.value)
    // 再认证成功只改了服务端的窗口：顺带取回会话（同时轮换 CSRF 令牌），
    // 让页面上的 CSRF 与再认证状态一致。
    await session.restore()
    password.value = ''
    reauthCode.value = ''
    reauthVisible.value = false
    await submitPending()
  } catch (caught) {
    reauthError.value = errorText(caught)
  } finally {
    reauthBusy.value = false
  }
}

// ---------------------------------------------------------------- 连接

/** 领一次性许可 → 拨号 → 连上后挂 xterm。 */
async function connect(sessionId: string): Promise<void> {
  teardownSocket()
  connState.value = 'opening'
  connError.value = null
  frameError.value = null
  exitInfo.value = null
  try {
    const grant = await createTerminalGrant(sessionId)
    if (disposed) return
    const next = openTerminalSocket({
      connectPath: grant.data.connect_path,
      protocol: grant.data.protocol,
      grantProtocol: grant.data.grant_protocol,
    })
    socket = next
    next.onopen = () => {
      connState.value = 'open'
      void mountTerminal()
    }
    next.onmessage = (event: MessageEvent) => {
      handleFrame(event.data)
    }
    next.onerror = () => {
      // 浏览器的 WebSocket 错误不带原因（安全考虑）。可查的东西都在服务端日志与
      // 上面那次“领许可”的结果里，所以这里只说实话：连接建立失败。
      if (connState.value !== 'open') {
        connError.value = '无法建立终端连接：许可可能已过期，或服务端拒绝了这次握手。'
        connState.value = 'error'
      }
    }
    next.onclose = () => {
      socket = null
      if (exitInfo.value !== null) {
        connState.value = 'closed'
        return
      }
      if (connState.value === 'open' || connState.value === 'opening') {
        connState.value = 'closed'
        connError.value = '连接已断开。若仍在断流宽限期内，可以重新连接并接上同一个会话。'
      }
    }
  } catch (caught) {
    connState.value = 'error'
    connError.value = errorText(caught)
  }
}

async function mountTerminal(): Promise<void> {
  await nextTick()
  const host = hostRef.value
  if (host === null || socket === null) return
  teardownTerminal()
  const instance = new Terminal({
    cols: DEFAULT_COLS,
    rows: DEFAULT_ROWS,
    cursorBlink: true,
    fontSize: 13,
    fontFamily: 'ui-monospace, SFMono-Regular, Menlo, Consolas, monospace',
    scrollback: 2000,
    theme: { background: '#16181d', foreground: '#e8e8e8', cursor: '#7ad1a1' },
  })
  const addon = new FitAddon()
  instance.loadAddon(addon)
  instance.open(host)
  instance.onData((text: string) => {
    sendText(encodeInputFrame(text))
  })
  instance.onResize((size: { cols: number; rows: number }) => {
    sendText(encodeResizeFrame(size.cols, size.rows))
  })
  term = instance
  fitAddon = addon
  addon.fit()
  observeHost(host)
  instance.focus()
}

function observeHost(host: HTMLElement): void {
  if (typeof ResizeObserver === 'undefined') return
  observer = new ResizeObserver(() => {
    fitAddon?.fit()
  })
  observer.observe(host)
}

function sendText(payload: string | null): void {
  if (payload === null) return
  const current = socket
  if (current === null || current.readyState !== WebSocket.OPEN) return
  current.send(payload)
}

/** 处理一条服务端帧。坏帧丢弃并计数，不断开连接。 */
function handleFrame(raw: unknown): void {
  if (typeof raw !== 'string') return
  const frame = decodeTerminalFrame(raw)
  if (frame === null) return
  switch (frame.type) {
    case 'ready': {
      readyInfo.value = {
        account: frame.account ?? data.value?.account ?? '未知',
        accountUid: frame.account_uid ?? data.value?.account_uid ?? 0,
        sessionId: frame.session_id ?? '',
      }
      fitAddon?.fit()
      break
    }
    case 'output': {
      const bytes = outputBytes(frame)
      if (bytes !== null) term?.write(bytes)
      break
    }
    case 'exit': {
      const reason = frame.reason ?? 'exit'
      exitInfo.value = {
        reason,
        exitCode: frame.exit_code ?? null,
        signal: frame.signal ?? null,
      }
      term?.write(`\r\n\u001b[2m—— ${exitReasonLabel(reason)} ——\u001b[0m\r\n`)
      connState.value = 'closed'
      teardownSocket()
      void load()
      break
    }
    case 'error': {
      frameError.value = `${frame.code ?? 'ERROR'}：${frame.message ?? '服务端返回了一个错误帧'}`
      break
    }
    default:
      // 未知 type 不做任何事：服务端新增帧不该让旧页面崩掉。
      break
  }
}

function teardownSocket(): void {
  const current = socket
  socket = null
  if (current === null) return
  current.onopen = null
  current.onmessage = null
  current.onerror = null
  current.onclose = null
  try {
    current.close()
  } catch {
    // 已经关掉的连接再关一次会抛异常；这里没有需要补救的东西。
  }
}

function teardownTerminal(): void {
  observer?.disconnect()
  observer = null
  fitAddon = null
  const current = term
  term = null
  current?.dispose()
}

// ---------------------------------------------------------------- 关闭

/** 主动关闭自己的会话（DELETE，审计）。 */
async function closeSession(): Promise<void> {
  const target = activeSession.value
  if (target === null) return
  busy.value = true
  actionError.value = null
  try {
    await closeTerminalSession(target.session_id)
    teardownSocket()
    teardownTerminal()
    connState.value = 'idle'
    readyInfo.value = null
    exitInfo.value = null
    connError.value = null
    await load()
  } catch (caught) {
    actionError.value = errorText(caught)
  } finally {
    busy.value = false
  }
}

/** 重连已有会话（断流宽限内接上同一个 PTY）。 */
async function reconnect(): Promise<void> {
  const target = activeSession.value
  if (target === null) return
  await connect(target.session_id)
}

function errorText(caught: unknown): string {
  if (caught instanceof ApiError) return caught.message
  if (caught instanceof Error) return caught.message
  return '请求失败，请稍后重试'
}

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => {
    setTimeout(resolve, ms)
  })
}

onMounted(() => {
  if (session.isAdmin) void load()
  else phase.value = 'ready'
})

onBeforeUnmount(() => {
  disposed = true
  teardownSocket()
  teardownTerminal()
})
</script>

<template>
  <section class="terminal">
    <div class="terminal__head">
      <h1 class="terminal__title">终端</h1>
      <NTag v-if="activeSession" size="small" :type="connected ? 'success' : 'warning'">
        {{ sessionStateLabel(activeSession.state) }}
      </NTag>
      <NTag v-else size="small">没有会话</NTag>
    </div>

    <StatePanel
      v-if="!session.isAdmin"
      state="forbidden"
      title="终端需要管理员权限"
      description="终端能读到服务器上的几乎一切内容，PLAN.md 第 15 节把它列为管理员能力。当前账号是只读角色。"
    />

    <template v-else>
      <NAlert type="info" :bordered="false" title="这是一个受限终端，不是 root shell">
        会话以 <code>{{ accountLabel || 'upanel-shell' }}</code> 运行：不是 root，也没有 sudo，
        面板不提供任何提权入口。终端只有字节通道，没有命令接口；输入只是写进 tty 的字节。
        <template v-if="limitsLine"> {{ limitsLine }}。 </template>
      </NAlert>

      <StatePanel v-if="phase === 'loading'" state="loading" title="正在读取终端会话" />
      <StatePanel
        v-else-if="panelState && error && panelState !== 'error'"
        :state="panelState"
        :description="error.message"
        :code="error.code"
        :request-id="error.requestId"
        @retry="load"
      />
      <ApiErrorPanel v-else-if="error" :error="error" @retry="load" />

      <template v-else>
        <NAlert v-if="actionError" type="error" :bordered="false" closable @close="actionError = null">
          {{ actionError }}
        </NAlert>
        <NAlert v-if="frameError" type="warning" :bordered="false" closable @close="frameError = null">
          服务端返回了一个错误帧：{{ frameError }}
        </NAlert>

        <NCard v-if="activeSession === null" size="small" title="新建终端会话">
          <p class="terminal__line">
            创建会话是一次受管动作：先生成预检（看阻塞项与影响），需要时再做一次密码再认证，
            然后提交任务。任务成功后会拿到一个 session_id，页面据此领取一次性连接许可并连上去。
          </p>
          <p v-if="jobMessage" class="terminal__meta">{{ jobMessage }}</p>
          <ul v-if="pendingPreflight && !pendingPreflight.allowed" class="terminal__blockers">
            <li v-for="notice in pendingPreflight.blockers" :key="notice.code + notice.message">
              <code>{{ notice.code }}</code> {{ notice.message }}
            </li>
          </ul>
          <NButton type="primary" :disabled="!canCreate" :loading="busy" @click="startCreate">
            创建终端会话（{{ DEFAULT_COLS }}×{{ DEFAULT_ROWS }}）
          </NButton>
          <p class="terminal__meta">
            每个账号最多 1 个会话，全局最多 {{ data?.max_sessions ?? 2 }} 个；上限属于服务端策略，
            页面不提供绕过。
          </p>
        </NCard>

        <NCard v-else size="small" title="当前会话">
          <p class="terminal__line">
            会话 <code>{{ activeSession.session_id }}</code> 以
            <code>{{ activeSession.account }}（uid {{ activeSession.account_uid }}）</code> 运行。
          </p>
          <p class="terminal__meta">
            创建于 {{ formatTimestamp(activeSession.created_at) }} ·
            无输入将于 {{ formatTimestamp(activeSession.idle_deadline) }} 关闭 ·
            最长到 {{ formatTimestamp(activeSession.expires_at) }}
          </p>
          <div class="terminal__actions">
            <NButton
              v-if="!connected"
              type="primary"
              :loading="connState === 'opening'"
              :disabled="busy"
              @click="reconnect"
            >
              {{ connState === 'closed' ? '重新连接（宽限期内可接上同一个会话）' : '连接' }}
            </NButton>
            <NButton :disabled="busy" @click="closeSession">
              关闭会话
            </NButton>
            <span v-if="connState === 'opening'" class="terminal__meta">正在领取连接许可…</span>
            <span v-else-if="connState === 'closed'" class="terminal__meta">
              连接已断开。宽限期过后服务端会终止会话。
            </span>
          </div>

          <NAlert v-if="connError" type="warning" :bordered="false" class="terminal__gap">
            {{ connError }}
          </NAlert>
          <NAlert v-if="exitInfo" type="info" :bordered="false" class="terminal__gap">
            {{ exitReasonLabel(exitInfo.reason) }}<template v-if="exitInfo.exitCode !== null">
              （退出码 {{ exitInfo.exitCode }}）</template><template v-else-if="exitInfo.signal !== null">
              （信号 {{ exitInfo.signal }}）</template>。
          </NAlert>
          <p v-if="readyInfo" class="terminal__meta">
            已连接：{{ readyInfo.account }}（uid {{ readyInfo.accountUid }}）。
          </p>
        </NCard>

        <div v-if="connState !== 'idle' && activeSession !== null" class="terminal__pane">
          <div ref="hostRef" class="terminal__host" data-testid="terminal-host" />
        </div>
      </template>
    </template>

    <NModal
      v-model:show="reauthVisible"
      preset="card"
      title="需要重新验证密码"
      class="terminal__modal"
      :mask-closable="false"
    >
      <p class="terminal__line">
        创建终端会话要求 5 分钟内的密码再认证（CONTRACTS.md 第 5 节的动作策略）。
        面板没有“跳过再认证”的开关。
      </p>
      <NInput
        v-model:value="password"
        type="password"
        show-password-on="click"
        placeholder="当前账号的密码"
        @keyup.enter="confirmReauth"
      />
      <SecondFactorField
        :show="session.totpEnabled"
        :model-value="reauthCode"
        :disabled="reauthBusy"
        testid="terminal-reauth-code"
        @update:model-value="(value: string) => (reauthCode = value)"
        @keyup.enter="confirmReauth"
      />
      <NAlert v-if="reauthError" type="error" :bordered="false" class="terminal__gap">
        {{ reauthError }}
      </NAlert>
      <template #footer>
        <div class="terminal__actions">
          <NButton :disabled="reauthBusy" @click="reauthVisible = false">取消</NButton>
          <NButton type="primary" :loading="reauthBusy" :disabled="!reauthReady" @click="confirmReauth">
            验证并继续
          </NButton>
        </div>
      </template>
    </NModal>
  </section>
</template>

<style scoped>

.terminal__gap {
  margin-top: 10px;
}

.terminal__blockers {
  margin: 0 0 10px;
  padding-left: 18px;
  font-size: var(--fs-md);
  line-height: 1.7;
}

.terminal__actions {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 10px;
  margin-top: 8px;
}

.terminal__pane {
  border-radius: var(--r-2);
  overflow: hidden;
  background: var(--rail);
  padding: 8px;
}

.terminal__host {
  height: 420px;
  width: 100%;
}

.terminal__modal {
  max-width: 520px;
}
</style>
