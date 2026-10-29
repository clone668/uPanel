<script setup lang="ts">
// 系统概况面板：把一份 /system/summary 快照画成人能看的几块。
//
// 每个采集段独立判断：status 不是 ok 的段不画数字，改画状态面板并附带服务端给的
// error 原话。这样“这台机器没有某个数据源”和“真的 0%”永远分得开
// （CONTRACTS 第 6.1 节：三种 status 都不是零值）。
//
// 版式：五段用发丝线隔开，不再一段一个卡片。外层已经是一块面板（UPanel），
// 面板里再套五张带阴影的卡会把眼睛从数字上拽走——这里只留标题与格子。

import { computed } from 'vue'

import { SYSTEM_SECTION_LABELS, type SystemSummary } from '@/api/system'
import StatePanel from '@/components/StatePanel.vue'
import StatTile from '@/components/StatTile.vue'
import UTag from '@/ui/UTag.vue'
import {
  UNKNOWN_TEXT,
  formatBytes,
  formatPercent,
  formatRateNumber,
  formatSampleSummary,
  formatUptime,
  percentOf,
} from '@/utils/format'
import { sectionPanelState } from '@/utils/viewstate'

const props = defineProps<{ summary: SystemSummary }>()

const host = computed(() => props.summary.host)
const cpu = computed(() => props.summary.cpu)
const memory = computed(() => props.summary.memory)
const load = computed(() => props.summary.load)
const network = computed(() => props.summary.network)

const hostState = computed(() => sectionPanelState(host.value.status))
const cpuState = computed(() => sectionPanelState(cpu.value.status))
const memoryState = computed(() => sectionPanelState(memory.value.status))
const loadState = computed(() => sectionPanelState(load.value.status))
const networkState = computed(() => sectionPanelState(network.value.status))

/** 内存使用率：按 total 与 used 自己算，不猜。swap 单独算。 */
const memoryPercent = computed(() => percentOf(memory.value.used_bytes, memory.value.total_bytes))
const swapUsed = computed(() => {
  const total = memory.value.swap_total_bytes
  const free = memory.value.swap_free_bytes
  if (total === null || free === null) return null
  try {
    return (BigInt(total) - BigInt(free)).toString()
  } catch {
    return null
  }
})

const perCpu = computed(() => cpu.value.per_cpu ?? [])
const swapTotalText = computed(() => {
  const total = memory.value.swap_total_bytes
  if (total === null) return UNKNOWN_TEXT
  return total === '0' ? '未启用' : formatBytes(total)
})

const interfaces = computed(() => network.value.interfaces ?? [])
const sampleLine = computed(() =>
  formatSampleSummary(props.summary.collected_at, props.summary.interval_ms),
)
const cpuModel = computed(() => cpu.value.model ?? '未读到型号')
</script>

<template>
  <div class="system" data-testid="system-overview">
    <p class="system__sample">
      <span>{{ sampleLine }}</span>
      <UTag v-if="summary.stale" tone="warn" :mono="false">这份快照已陈旧</UTag>
      <UTag v-if="summary.degraded" tone="warn" :mono="false">有采集段不可用</UTag>
    </p>

    <!-- 主机 -->
    <section class="system__sec">
      <h3 class="system__h">{{ SYSTEM_SECTION_LABELS.host }}</h3>
      <StatePanel
        v-if="hostState"
        :state="hostState"
        :title="`${SYSTEM_SECTION_LABELS.host}信息不可用`"
        :description="host.error ?? '服务端没有给出原因。'"
      />
      <template v-else>
        <div class="system__grid">
          <StatTile label="主机名" :value="host.hostname ?? UNKNOWN_TEXT" />
          <StatTile label="系统" :value="host.os_pretty_name ?? UNKNOWN_TEXT" :hint="host.os_id ?? ''" />
          <StatTile label="内核" :value="host.kernel_release ?? UNKNOWN_TEXT" :hint="host.arch ?? ''" />
          <StatTile label="运行时长" :value="formatUptime(host.uptime_seconds)" />
        </div>
        <p v-if="host.source" class="system__meta">数据来源：{{ host.source }}</p>
      </template>
    </section>

    <!-- CPU -->
    <section class="system__sec">
      <h3 class="system__h">{{ SYSTEM_SECTION_LABELS.cpu }}</h3>
      <StatePanel
        v-if="cpuState"
        :state="cpuState"
        :title="`${SYSTEM_SECTION_LABELS.cpu}信息不可用`"
        :description="cpu.error ?? '服务端没有给出原因。'"
      />
      <template v-else>
        <div class="system__grid">
          <StatTile
            label="使用率"
            :value="formatPercent(cpu.usage_percent, 1)"
            :hint="
              cpu.usage_percent === null
                ? '没有可比较的前一次样本（不是 0%）'
                : `采样窗口 ${cpu.sample_window_ms ?? UNKNOWN_TEXT} 毫秒`
            "
          />
          <StatTile label="逻辑核" :value="String(cpu.logical_cores)" />
          <StatTile label="型号" :value="cpuModel" />
        </div>
        <ul v-if="perCpu.length > 0" class="system__cores">
          <li v-for="core in perCpu" :key="core.index" class="system__core">
            <span class="system__core-label">cpu{{ core.index }}</span>
            <span class="system__core-value">{{ formatPercent(core.usage_percent, 1) }}</span>
          </li>
        </ul>
      </template>
    </section>

    <!-- 内存 -->
    <section class="system__sec">
      <h3 class="system__h">{{ SYSTEM_SECTION_LABELS.memory }}</h3>
      <StatePanel
        v-if="memoryState"
        :state="memoryState"
        :title="`${SYSTEM_SECTION_LABELS.memory}信息不可用`"
        :description="memory.error ?? '服务端没有给出原因。'"
      />
      <template v-else>
        <div class="system__grid">
          <StatTile
            label="已用（可用口径）"
            :value="formatBytes(memory.used_bytes)"
            :hint="`共 ${formatBytes(memory.total_bytes)}，可用 ${formatBytes(memory.available_bytes)}`"
          />
          <StatTile label="使用率" :value="formatPercent(memoryPercent, 1)" hint="已用 / 总量" />
          <StatTile label="空闲 / 缓存" :value="formatBytes(memory.free_bytes)" :hint="`缓存 ${formatBytes(memory.cached_bytes)}，缓冲 ${formatBytes(memory.buffers_bytes)}`" />
          <StatTile
            label="交换区"
            :value="swapTotalText"
            :hint="swapUsed === null ? '没有读到交换区' : `已用 ${formatBytes(swapUsed)}`"
          />
        </div>
      </template>
    </section>

    <!-- 负载 -->
    <section class="system__sec">
      <h3 class="system__h">{{ SYSTEM_SECTION_LABELS.load }}</h3>
      <StatePanel
        v-if="loadState"
        :state="loadState"
        :title="`${SYSTEM_SECTION_LABELS.load}信息不可用`"
        :description="load.error ?? '服务端没有给出原因。'"
      />
      <div v-else class="system__grid">
        <StatTile label="1 分钟" :value="load.load1 === null ? UNKNOWN_TEXT : String(load.load1)" />
        <StatTile label="5 分钟" :value="load.load5 === null ? UNKNOWN_TEXT : String(load.load5)" />
        <StatTile label="15 分钟" :value="load.load15 === null ? UNKNOWN_TEXT : String(load.load15)" />
        <StatTile
          label="可运行 / 进程"
          :value="`${load.runnable ?? UNKNOWN_TEXT} / ${load.threads ?? UNKNOWN_TEXT}`"
        />
      </div>
    </section>

    <!-- 网络 -->
    <section class="system__sec">
      <h3 class="system__h">{{ SYSTEM_SECTION_LABELS.network }}</h3>
      <StatePanel
        v-if="networkState"
        :state="networkState"
        :title="`${SYSTEM_SECTION_LABELS.network}信息不可用`"
        :description="network.error ?? '服务端没有给出原因。'"
      />
      <template v-else>
        <p v-if="interfaces.length === 0" class="system__meta">没有读到任何网卡。</p>
        <ul v-else class="system__ifaces">
          <li v-for="iface in interfaces" :key="iface.name" class="system__iface">
            <p class="system__iface-head">
              <strong class="system__iface-name">{{ iface.name }}</strong>
              <UTag v-if="iface.loopback" tone="neutral" :mono="false">回环</UTag>
              <span class="system__meta">状态：{{ iface.oper_state ?? UNKNOWN_TEXT }}</span>
            </p>
            <p class="system__meta">
              接收 {{ formatRateNumber(iface.rx_bytes_per_second) }} · 发送
              {{ formatRateNumber(iface.tx_bytes_per_second) }}（累计收
              {{ formatBytes(iface.rx_bytes) }} / 发 {{ formatBytes(iface.tx_bytes) }}）
            </p>
            <p class="system__meta">
              丢包 收 {{ iface.rx_dropped }} / 发 {{ iface.tx_dropped }} · 错误 收
              {{ iface.rx_errors }} / 发 {{ iface.tx_errors }}
            </p>
          </li>
        </ul>
      </template>
    </section>
  </div>
</template>

<style scoped>
.system {
  display: flex;
  flex-direction: column;
}

.system__sample {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: var(--sp-2);
  margin: 0 0 var(--sp-1);
  font-size: var(--fs-sm);
  color: var(--ink-3);
}

/* 五段之间只隔一条发丝线：段与段是同一个面板里的内容，不是五张卡。 */
.system__sec {
  padding: var(--sp-3) 0;
  border-top: 1px solid var(--rule);
}

.system__sec:first-of-type {
  border-top: 0;
  padding-top: var(--sp-2);
}

.system__h {
  margin: 0 0 var(--sp-2);
  font-size: var(--fs-sm);
  font-weight: 600;
  color: var(--ink);
}

.system__grid {
  display: flex;
  flex-wrap: wrap;
  gap: 6px;
}

.system__meta {
  margin: 6px 0 0;
  font-size: var(--fs-xs);
  color: var(--ink-3);
  word-break: break-all;
}

.system__cores {
  display: flex;
  flex-wrap: wrap;
  gap: 6px;
  margin: 8px 0 0;
  padding: 0;
  list-style: none;
}

.system__core {
  display: flex;
  gap: 6px;
  padding: 2px 7px;
  font-size: var(--fs-xs);
  border: 1px solid var(--rule);
  border-radius: var(--r-1);
}

.system__core-label {
  color: var(--ink-3);
}

.system__core-value {
  font-variant-numeric: tabular-nums;
  color: var(--ink);
}

.system__ifaces {
  display: flex;
  flex-direction: column;
  margin: 0;
  padding: 0;
  list-style: none;
}

/* 网卡：一行一张，行与行之间发丝线，不再每个网卡一个圆角盒子。 */
.system__iface {
  padding: var(--sp-2) 0;
  border-top: 1px solid var(--rule);
}

.system__iface:first-child {
  padding-top: 0;
  border-top: 0;
}

.system__iface-head {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: var(--sp-2);
  margin: 0;
}

.system__iface-name {
  font-family: var(--font-mono);
  font-size: var(--fs-md);
  font-weight: 600;
}

.system__iface .system__meta {
  margin: 3px 0 0;
}
</style>
