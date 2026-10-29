<script setup lang="ts">
// 外部程序能力页（P16.1）。
//
// 这一页回答一句话：**这台机器能做什么**。数据只有一条来源——
// GET /system/capabilities（仅 admin），后端是代理启动时探一次、之后每 10 分钟
// 重探一次的缓存快照。
//
// 三件在这一页刻意做成这样的东西：
//
//  1. **“还没探完”不是空表**。`ready=false` 时后端给的是 `host_caps: null`，
//     页面画一句“还没探完”，一条表都不渲染。空表在任何人眼里都是
//     “这台机器什么都没装”，而那会让人把已经装好的东西再装一遍。
//
//  2. **三态原样呈现**。`unknown`（固定目录读不出来）单独一列标签、单独一段计数，
//     不并进 `absent`。并进去的后果同上，只是更隐蔽：表格看着整齐，结论是假的。
//
//  3. **刷新按钮只重读快照**。它打的还是同一条只读接口，不会触发探测——
//     探测节奏在代理那边。按钮旁边就这么写，因为“点了刷新版本还是旧的”与
//     “点刷新会重新探测”这两种理解会导出完全不同的排障结论。
//
// 这一页没有安装/卸载按钮：装什么包是 apt 的事，走的是软件包页那条
// 预检 → 任务的链路，不是这里。这里给的是 hint（apt 包名），不是入口。

import { computed, onMounted, ref } from 'vue'

import { NAlert, NButton, NCard, NCheckbox, NTag } from 'naive-ui'

import { ApiError } from '@/api/client'
import {
  HOST_CAPS_STATE_HINTS,
  HOST_CAPS_STATE_LABELS,
  countHostCaps,
  describeHostCapsItem,
  fetchHostCapabilities,
  hostCapsHeadline,
  hostCapsVersionLabel,
  type HostCapsItem,
  type HostCapsState,
  type SystemCapabilitiesData,
} from '@/api/hostcaps'
import ApiErrorPanel from '@/components/ApiErrorPanel.vue'
import StatePanel from '@/components/StatePanel.vue'
import { formatTimestamp } from '@/utils/format'
import { apiPanelState, retryablePanelState } from '@/utils/viewstate'

type Phase = 'loading' | 'ready' | 'error'

const phase = ref<Phase>('loading')
const error = ref<ApiError | null>(null)
const data = ref<SystemCapabilitiesData | null>(null)
const refreshing = ref(false)
/** 只看需要处理的行：未安装与说不清。默认关着——先看全貌。 */
const onlyMissing = ref(false)

const report = computed(() => data.value?.host_caps ?? null)
const ready = computed(() => data.value !== null && data.value.ready && report.value !== null)
const counts = computed(() => countHostCaps(report.value?.items ?? []))
const headline = computed(() =>
  data.value === null ? '' : hostCapsHeadline(data.value),
)
const issues = computed(() => report.value?.issues ?? [])

const rows = computed<HostCapsItem[]>(() => {
  const items = report.value?.items ?? []
  if (!onlyMissing.value) return items
  return items.filter((item) => item.state !== 'present')
})

/** 状态标签：三态各有中文词与颜色，颜色只是辅助，词才是判据。 */
const STATE_TAG_TYPE: Record<HostCapsState, 'success' | 'warning' | 'default'> = {
  present: 'success',
  absent: 'warning',
  unknown: 'default',
}

const panelState = computed(() => (error.value === null ? null : apiPanelState(error.value)))

/**
 * 取一次快照。
 *
 * 非 ApiError 一律往上抛：客户端已经把传输故障包成 ApiError（source 为 client），
 * 剩下的“不是 ApiError 的东西”是代码缺陷，吞掉它只会让页面显示一句编出来的话。
 */
async function pull(): Promise<void> {
  error.value = null
  try {
    const result = await fetchHostCapabilities()
    data.value = result.data
    phase.value = 'ready'
  } catch (caught) {
    if (caught instanceof ApiError) {
      error.value = caught
      phase.value = 'error'
      return
    }
    throw caught
  }
}

async function load(): Promise<void> {
  phase.value = 'loading'
  await pull()
}

/** 只重读一遍快照，不触发探测（探测节奏在代理那边）。 */
async function refresh(): Promise<void> {
  refreshing.value = true
  try {
    await pull()
  } finally {
    refreshing.value = false
  }
}

onMounted(load)
</script>

<template>
  <section class="hostcaps">
    <NCard title="外部程序能力" size="small">
      <p class="hint">
        这张表列的是这台机器上的外部程序与版本：程序只从
        <code>/usr/sbin</code>、<code>/sbin</code>、<code>/usr/bin</code>、<code>/bin</code>
        四个固定目录里找，不走 <code>PATH</code>。
      </p>
      <p class="hint">
        它只转述事实，不做判决：某一行写着“已安装”，不等于对应的动作一定能成——那件事由
        那个动作自己的预检回答。反过来，这里写着“未安装”，也只是说那个程序没在固定目录里。
      </p>
      <div class="hostcaps__actions">
        <NButton
          size="small"
          :loading="refreshing"
          data-testid="hostcaps-refresh"
          @click="refresh"
        >
          重新读取
        </NButton>
        <span class="hostcaps__refresh-note">
          只重读代理已经探好的快照，不会触发新的探测（代理每 10 分钟自己探一次）。
        </span>
      </div>
    </NCard>

    <StatePanel v-if="phase === 'loading'" state="loading" title="正在读取能力快照" />

    <ApiErrorPanel
      v-else-if="phase === 'error' && error"
      :error="error"
      title="读不到外部程序能力"
      :allow-retry="panelState !== null && retryablePanelState(panelState)"
      @retry="load"
    />

    <template v-else>
      <NAlert
        v-if="!ready"
        type="info"
        :bordered="false"
        data-testid="hostcaps-not-ready"
      >
        {{ headline }}
      </NAlert>

      <template v-else>
        <NAlert
          :type="report?.partial ? 'warning' : 'success'"
          :bordered="false"
          data-testid="hostcaps-headline"
        >
          {{ headline }}
          <template v-if="report">
            探测时间 {{ formatTimestamp(report.generated_at) }}。
          </template>
        </NAlert>

        <NCard size="small" title="一眼看完">
          <ul class="hostcaps__counts" data-testid="hostcaps-counts">
            <li>共 {{ counts.total }} 项</li>
            <li>已安装 {{ counts.present }}</li>
            <li>未安装 {{ counts.absent }}</li>
            <li>说不清 {{ counts.unknown }}</li>
          </ul>
          <p class="hint">
            未安装的条目带着补齐提示（一般是 apt 包名）；说不清的条目表示固定目录读不出来，
            既不能说有、也不能说没有。
          </p>
          <NCheckbox v-model:checked="onlyMissing" data-testid="hostcaps-only-missing">
            只看未安装与说不清（{{ counts.total - counts.present }} 项）
          </NCheckbox>
        </NCard>

        <NAlert
          v-if="issues.length > 0"
          type="warning"
          :bordered="false"
          data-testid="hostcaps-issues"
        >
          <p>这一轮探测遇到的问题：</p>
          <ul>
            <li v-for="issue in issues" :key="issue">{{ issue }}</li>
          </ul>
        </NAlert>

        <NCard size="small" title="逐项读数">
          <p v-if="rows.length === 0" class="hint" data-testid="hostcaps-filtered-empty">
            按当前筛选条件没有可显示的行——这表示上面那 {{ counts.present }} 项都装好了，
            不是探测没结果。
          </p>
          <table v-else class="hostcaps__table" data-testid="hostcaps-table">
            <thead>
              <tr>
                <th scope="col">状态</th>
                <th scope="col">标识</th>
                <th scope="col">依赖它的能力</th>
                <th scope="col">版本</th>
                <th scope="col">路径 / 说明</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="item in rows" :key="item.id" :data-item="item.id" :data-state="item.state">
                <td>
                  <NTag size="small" :type="STATE_TAG_TYPE[item.state]">
                    {{ HOST_CAPS_STATE_LABELS[item.state] }}
                  </NTag>
                </td>
                <td><code>{{ item.id }}</code></td>
                <td>{{ item.feature ?? '—' }}</td>
                <td>{{ hostCapsVersionLabel(item) }}</td>
                <td class="hostcaps__detail">{{ describeHostCapsItem(item) }}</td>
              </tr>
            </tbody>
          </table>
        </NCard>

        <NCard size="small" title="三态是什么意思">
          <ul class="hostcaps__legend">
            <li v-for="state in (['present', 'absent', 'unknown'] as HostCapsState[])" :key="state">
              <strong>{{ HOST_CAPS_STATE_LABELS[state] }}</strong>：{{ HOST_CAPS_STATE_HINTS[state] }}
            </li>
          </ul>
        </NCard>
      </template>
    </template>
  </section>
</template>

<style scoped>
.hostcaps {
  display: flex;
  flex-direction: column;
  gap: 16px;
  max-width: 1080px;
}

.hint {
  margin: 0 0 8px;
  font-size: var(--fs-md);
  line-height: 1.7;
  opacity: 0.8;
}

.hostcaps__actions {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 12px;
  margin-top: 4px;
}

.hostcaps__refresh-note {
  font-size: var(--fs-sm);
  line-height: 1.6;
  opacity: 0.7;
}

.hostcaps__counts {
  display: flex;
  flex-wrap: wrap;
  gap: 16px;
  margin: 0 0 8px;
  padding: 0;
  list-style: none;
  font-size: var(--fs-md);
}

.hostcaps__table {
  width: 100%;
  border-collapse: collapse;
  font-size: var(--fs-md);
}

.hostcaps__table th,
.hostcaps__table td {
  padding: 6px 8px;
  text-align: left;
  vertical-align: top;
  border-bottom: 1px solid var(--n-border-color, var(--rule));
}

.hostcaps__table th {
  font-weight: 600;
  opacity: 0.75;
}

.hostcaps__detail {
  line-height: 1.6;
  word-break: break-word;
}

.hostcaps__legend {
  margin: 0;
  padding-left: 20px;
  font-size: var(--fs-md);
  line-height: 1.8;
}
</style>
