<script setup lang="ts">
// 概览页（P04.4）。
//
// 数据全部来自服务端，页面只做两件事：按服务端给的 interval_ms 轮询、把结果画出来。
//   1) /health/live：匿名存活检查，只证明“面板服务能应答”；
//   2) /system/summary：主机、CPU、内存、负载、网络；
//   3) /storage/devices：磁盘数量与健康提醒（详情在存储页）；
//   4) /apps：常用入口与入口探测异常。
//
// 四份数据各自有加载/失败状态：一份挂了不影响另外三份显示，
// 但页头会明确写出“哪一块没取到”，不做“看起来一切正常”的默认。
//
// 版式：顶部四格指标（一屏能扫完的数），左侧一块系统概况（细节），右侧一组窄面板
// （面板服务、存储、常用入口、尚未接入）。指标格与面板取的是同一个快照对象，
// 所以两处数字不会互相打架；没有读数的格子写 —，不写 0。

import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { RouterLink } from 'vue-router'

import { ApiError, get } from '@/api/client'
import { frequentEntries, getApps, openTarget, type App } from '@/api/apps'
import { fetchSystemSummary, type SystemSummary } from '@/api/system'
import { attentionDevices, type StorageDeviceList } from '@/api/storage'
import ApiErrorPanel from '@/components/ApiErrorPanel.vue'
import StatePanel from '@/components/StatePanel.vue'
import SystemOverviewPanel from '@/components/SystemOverviewPanel.vue'
import { useModeStore } from '@/stores/mode'
import UBar from '@/ui/UBar.vue'
import UButton from '@/ui/UButton.vue'
import UKpi from '@/ui/UKpi.vue'
import UPanel from '@/ui/UPanel.vue'
import UTag from '@/ui/UTag.vue'
import {
  UNKNOWN_TEXT,
  formatBytes,
  formatPercent,
  formatTimestamp,
  percentOf,
} from '@/utils/format'
import { createPoller, type Poller } from '@/utils/polling'
import { apiPanelState, retryablePanelState } from '@/utils/viewstate'

interface HealthLive {
  status: string
}

type Phase = 'loading' | 'ready' | 'error'

const modeStore = useModeStore()

const healthPhase = ref<Phase>('loading')
const healthError = ref<ApiError | null>(null)
const health = ref<HealthLive | null>(null)
const healthRequestId = ref('')
const healthCheckedAt = ref('')

const summaryPhase = ref<Phase>('loading')
const summaryError = ref<ApiError | null>(null)
const summary = ref<SystemSummary | null>(null)

const storagePhase = ref<Phase>('loading')
const storageError = ref<ApiError | null>(null)
const storage = ref<StorageDeviceList | null>(null)

const appsPhase = ref<Phase>('loading')
const appsError = ref<ApiError | null>(null)
const apps = ref<App[]>([])

const DEFAULT_INTERVAL_MS = 5000

const summaryPanelState = computed(() =>
  summaryError.value
    ? apiPanelState(summaryError.value)
    : summaryPhase.value === 'error'
      ? 'error'
      : null,
)
const storagePanelState = computed(() => (storageError.value ? apiPanelState(storageError.value) : null))
const appsPanelState = computed(() => (appsError.value ? apiPanelState(appsError.value) : null))

/**
 * 常用入口：只放**能打开**的应用（有 http/https 入口）。
 *
 * 首页是概览，不是应用页：把点不动的入口放上来，等于把“打不开”当成“入口”。
 */
const entries = computed(() => frequentEntries(apps.value, 8))
/** 入口异常的条数：只有真的探测过并拿到结论的才算，unknown 不算异常。 */
const httpErrors = computed(() => apps.value.filter((item) => item.http_state === 'error').length)

const diskCount = computed(() => storage.value?.devices.filter((d) => d.kind === 'disk').length ?? 0)
const needsAttention = computed(() => attentionDevices(storage.value?.devices ?? []).length)
/** 所有整盘的容量之和；一块都没采到时不显示合计，而不是显示 0 B。 */
const totalDiskBytes = computed<string | null>(() => {
  const disks = storage.value?.devices.filter((d) => d.kind === 'disk') ?? []
  if (disks.length === 0) return null
  let total = BigInt(0)
  for (const disk of disks) {
    try {
      total += BigInt(disk.size_bytes)
    } catch {
      // 非法字符串说明契约被破坏，合计就不给，避免显示一个假的数。
      return null
    }
  }
  return total.toString()
})

// ---- 顶部四格指标 ----
//
// 每一格都只管一个数，并且**只在对应采集段 ok 时才画数**：
// 段不可用时写 —，不让“读不到”看起来像 0（CONTRACTS 第 6.1 节）。

const cpuPercent = computed(() => {
  const section = summary.value?.cpu
  if (!section || section.status !== 'ok') return null
  return section.usage_percent
})

const memoryPercent = computed(() => {
  const section = summary.value?.memory
  if (!section || section.status !== 'ok') return null
  return percentOf(section.used_bytes, section.total_bytes)
})

const memoryFoot = computed(() => {
  const section = summary.value?.memory
  if (!section || section.status !== 'ok') return '内存段不可用'
  return `已用 ${formatBytes(section.used_bytes)} / 共 ${formatBytes(section.total_bytes)}`
})

const cpuFoot = computed(() => {
  const section = summary.value?.cpu
  if (!section || section.status !== 'ok') return 'CPU 段不可用'
  if (section.usage_percent === null) return '没有可比较的前一次样本'
  return `逻辑核 ${section.logical_cores} · 窗口 ${section.sample_window_ms ?? UNKNOWN_TEXT} 毫秒`
})

const diskFoot = computed(() => {
  if (storage.value === null) return '磁盘段不可用'
  return `整盘 ${diskCount.value} 块`
})

const attentionFoot = computed(() => {
  if (storage.value === null) return '磁盘段不可用'
  return needsAttention.value > 0 ? '详情见存储页' : '没有需要处理的盘'
})

async function loadHealth(): Promise<void> {
  healthPhase.value = 'loading'
  healthError.value = null
  try {
    const result = await get<HealthLive>('/health/live', { timeoutMs: 8000 })
    health.value = result.data
    healthRequestId.value = result.requestId
    healthCheckedAt.value = new Date().toLocaleString('zh-CN', { hour12: false })
    healthPhase.value = 'ready'
  } catch (caught) {
    if (caught instanceof ApiError) {
      healthError.value = caught
      healthPhase.value = 'error'
      return
    }
    throw caught
  }
}

async function loadSummary(): Promise<void> {
  try {
    const result = await fetchSystemSummary()
    summary.value = result.data
    summaryPhase.value = 'ready'
    summaryError.value = null
  } catch (caught) {
    if (caught instanceof ApiError) {
      summaryError.value = caught
      summaryPhase.value = 'error'
      return
    }
    throw caught
  }
}

async function loadStorage(): Promise<void> {
  try {
    const result = await get<StorageDeviceList>('/storage/devices', { timeoutMs: 10_000 })
    storage.value = result.data
    storagePhase.value = 'ready'
    storageError.value = null
  } catch (caught) {
    if (caught instanceof ApiError) {
      storageError.value = caught
      storagePhase.value = 'error'
      return
    }
    throw caught
  }
}

/** 常用入口与应用异常摘要。读不到应用不该让概览整页失败，所以它有自己的状态。 */
async function loadApps(): Promise<void> {
  try {
    const result = await getApps({ limit: 200 })
    apps.value = result.data.items
    appsPhase.value = 'ready'
    appsError.value = null
  } catch (caught) {
    if (caught instanceof ApiError) {
      appsError.value = caught
      appsPhase.value = 'error'
      return
    }
    throw caught
  }
}

/** 几份快照并行取，互不阻塞；各自的失败各自显示。 */
async function refreshAll(): Promise<void> {
  await Promise.all([loadSummary(), loadStorage(), loadApps()])
}

const poller: Poller = createPoller(refreshAll, {
  intervalMs: () => summary.value?.interval_ms ?? DEFAULT_INTERVAL_MS,
})

onMounted(() => {
  void loadHealth()
  poller.start()
})

onBeforeUnmount(() => {
  poller.stop()
})
</script>

<template>
  <section class="ov">
    <header class="ov__head">
      <h1 class="ov__title">概览</h1>
      <UTag :tone="modeStore.isMock ? 'warn' : 'good'" :mono="false">
        模式：{{ modeStore.isMock ? 'mock（演示数据）' : 'production' }}
      </UTag>
      <UButton size="sm" variant="secondary" @click="poller.trigger()">刷新</UButton>
      <p v-if="summary" class="ov__meta">
        采集时间：{{ formatTimestamp(summary.collected_at) }}（每 {{ summary.interval_ms }} 毫秒刷新）
      </p>
    </header>

    <!-- 四格指标：一屏最常看的四个数 -->
    <div class="ov__kpis">
      <UKpi label="CPU 使用率" :value="formatPercent(cpuPercent, 1)">
        <template #foot>{{ cpuFoot }}</template>
        <template #extra>
          <span class="ov__gauge"><UBar :percent="cpuPercent" label="CPU 使用率" /></span>
        </template>
      </UKpi>
      <UKpi label="内存使用率" :value="formatPercent(memoryPercent, 1)">
        <template #foot>{{ memoryFoot }}</template>
        <template #extra>
          <span class="ov__gauge"><UBar :percent="memoryPercent" label="内存使用率" /></span>
        </template>
      </UKpi>
      <UKpi label="整盘容量" :value="formatBytes(totalDiskBytes)">
        <template #foot>{{ diskFoot }}</template>
      </UKpi>
      <UKpi
        label="健康需要处理"
        :value="storage === null ? UNKNOWN_TEXT : String(needsAttention)"
        :tone="needsAttention > 0 ? 'warn' : 'default'"
      >
        <template #foot>{{ attentionFoot }}</template>
      </UKpi>
    </div>

    <div class="ov__cols">
      <div class="ov__main">
        <UPanel title="系统概况">
          <template #actions>
            <UTag v-if="summary?.degraded" tone="warn" :mono="false">部分数据不可用</UTag>
          </template>

          <StatePanel v-if="summaryPhase === 'loading'" state="loading" title="正在读取系统快照" />
          <StatePanel
            v-else-if="summaryError && summaryPanelState && summaryPanelState !== 'error'"
            :state="summaryPanelState"
            :description="summaryError.message"
            :code="summaryError.code"
            :request-id="summaryError.requestId"
            :retryable="retryablePanelState(summaryPanelState)"
            @retry="loadSummary"
          />
          <ApiErrorPanel v-else-if="summaryError" :error="summaryError" @retry="loadSummary" />
          <SystemOverviewPanel v-else-if="summary" :summary="summary" />
        </UPanel>
      </div>

      <aside class="ov__side">
        <UPanel title="面板服务">
          <StatePanel v-if="healthPhase === 'loading'" compact state="loading" title="正在检查面板服务" />
          <ApiErrorPanel
            v-else-if="healthPhase === 'error' && healthError"
            :error="healthError"
            @retry="loadHealth"
          />
          <div v-else-if="health" class="ov__stack">
            <p class="ov__line">
              存活检查：<strong>{{ health.status === 'ok' ? '服务可响应' : '状态未知' }}</strong>
            </p>
            <p class="ov__meta">检查时间：{{ healthCheckedAt }}</p>
            <p class="ov__meta">request_id：<code class="u-mono">{{ healthRequestId }}</code></p>
            <p class="ov__meta">这一项只说明面板服务能处理请求，不代表采集或写操作可用。</p>
          </div>
        </UPanel>

        <UPanel title="存储">
          <template #actions>
            <RouterLink class="ov__link" to="/storage">磁盘列表 →</RouterLink>
          </template>

          <StatePanel v-if="storagePhase === 'loading'" compact state="loading" title="正在读取磁盘快照" />
          <StatePanel
            v-else-if="storagePanelState && storageError"
            compact
            :state="storagePanelState"
            :description="storageError.message"
            :code="storageError.code"
            :request-id="storageError.requestId"
            :retryable="retryablePanelState(storagePanelState)"
            @retry="loadStorage"
          />
          <div v-else-if="storage" class="ov__stack">
            <p class="ov__line">
              整盘 <strong class="u-num">{{ diskCount }}</strong> 块，共
              <strong class="u-num">{{ formatBytes(totalDiskBytes) }}</strong>
            </p>
            <p class="ov__line">
              健康需要处理：<strong class="u-num">{{ needsAttention }}</strong> 块
              <span v-if="needsAttention > 0" class="ov__meta">（详情见存储页）</span>
            </p>
            <p v-if="storage.status === 'error'" class="ov__meta">
              注意：最近一次扫描失败（{{ storage.error ?? '未给出原因' }}），
              下面显示的是上一次成功的快照。
            </p>
            <p class="ov__meta">
              快照版本：<code class="u-mono">{{ storage.snapshot_version.slice(0, 19) }}…</code>
            </p>
          </div>
        </UPanel>

        <UPanel title="常用入口">
          <template #actions>
            <RouterLink class="ov__link" to="/apps">全部应用 →</RouterLink>
          </template>

          <StatePanel v-if="appsPhase === 'loading'" compact state="loading" title="正在读取应用列表" />
          <StatePanel
            v-else-if="appsPanelState && appsError"
            compact
            :state="appsPanelState"
            :description="appsError.message"
            :code="appsError.code"
            :request-id="appsError.requestId"
            :retryable="retryablePanelState(appsPanelState)"
            @retry="loadApps"
          />
          <div v-else class="ov__stack">
            <p class="ov__line">
              共 <strong class="u-num">{{ apps.length }}</strong> 个应用，其中
              <strong class="u-num">{{ entries.length }}</strong> 个能一键打开。
              <span v-if="httpErrors > 0" class="ov__meta">
                （{{ httpErrors }} 个入口探测异常，详情见应用页）
              </span>
            </p>

            <StatePanel
              v-if="entries.length === 0"
              compact
              state="empty"
              title="还没有能打开的入口"
              description="应用页可以把本机发现到的服务确认成应用，或者手动登记一条带入口地址的应用。"
            />
            <div v-else class="ov__entries" data-testid="overview-app-entries">
              <a
                v-for="app in entries"
                :key="app.id"
                class="ov__entry"
                :href="openTarget(app).href"
                target="_blank"
                rel="noopener noreferrer"
                :data-app="app.id"
              >
                <span class="ov__entry-name">{{ app.name }}</span>
                <span class="ov__entry-url u-mono">{{ app.public_url }}</span>
              </a>
            </div>

            <p class="ov__meta">
              收藏过的应用排在前面；入口地址是登记时的 public_url，可能与面板本身不在同一个域名。
            </p>
          </div>
        </UPanel>

        <UPanel title="尚未接入">
          <p class="ov__line">
            异常服务汇总、待更新与最近任务在后续阶段接入；没有接入之前，它们不出现在导航里，
            这里也不显示任何示例数字。
          </p>
        </UPanel>
      </aside>
    </div>
  </section>
</template>

<style scoped>
.ov {
  display: flex;
  flex-direction: column;
  gap: var(--sp-3);
}

.ov__head {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: var(--sp-2) var(--sp-3);
}

.ov__title {
  margin: 0;
  font-size: var(--fs-xl);
  font-weight: 600;
  letter-spacing: -0.01em;
}

.ov__kpis {
  display: grid;
  grid-template-columns: repeat(4, minmax(0, 1fr));
  background: var(--surface);
  border: 1px solid var(--rule);
  border-radius: var(--r-2);
  overflow: hidden;
}

.ov__cols {
  display: grid;
  grid-template-columns: minmax(0, 1.75fr) minmax(300px, 1fr);
  gap: var(--sp-3);
  align-items: start;
}

.ov__main,
.ov__side {
  display: flex;
  flex-direction: column;
  gap: var(--sp-3);
  min-width: 0;
}

.ov__stack {
  display: flex;
  flex-direction: column;
  gap: var(--sp-1);
}

/* 指标格里的容量条：铺满格宽、4px 高，是一条刻度轨，不是脚注旁的小配饰。 */
.ov__gauge {
  display: block;
}

.ov__entries {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(190px, 1fr));
  gap: 6px;
  margin: 2px 0 var(--sp-1);
}

.ov__entry {
  display: flex;
  flex-direction: column;
  gap: 1px;
  padding: 6px 9px;
  color: inherit;
  text-decoration: none;
  background: var(--surface-2);
  border: 1px solid transparent;
  border-radius: var(--r-2);
}

.ov__entry:hover {
  border-color: var(--rule-strong);
  background: var(--surface-3);
}

.ov__entry-name {
  font-size: var(--fs-md);
  font-weight: 600;
  word-break: break-all;
}

.ov__entry-url {
  font-size: var(--fs-xs);
  color: var(--ink-3);
  word-break: break-all;
}

.ov__link {
  font-size: var(--fs-xs);
  color: var(--accent);
  text-decoration: none;
}

.ov__link:hover {
  text-decoration: underline;
}

.ov__line {
  margin: 0;
  font-size: var(--fs-md);
  line-height: 1.6;
}

.ov__meta {
  margin: 0;
  font-size: var(--fs-xs);
  color: var(--ink-3);
  word-break: break-all;
}

@media (max-width: 1100px) {
  .ov__cols {
    grid-template-columns: minmax(0, 1fr);
  }
}

@media (max-width: 760px) {
  .ov__kpis {
    grid-template-columns: repeat(2, minmax(0, 1fr));
  }

  /* 两列时第 3 格开始是新的一行：去掉那条会把两行接起来的左发丝线。 */
  .ov__kpis > :nth-child(3) {
    border-left: 0;
  }

  /* 窄屏的指标格里，口径那行会折成两三行，再塞一根 84px 的条就挤了——
     条本身是"看一眼大概"用的，手机上不如把那两行字读顺。 */
  .ov__gauge {
    display: none;
  }
}
</style>
