<script setup lang="ts">
// 本机候选（P09.2 的 /apps/discovery，P09.5 做界面）。
//
// 这一块回答的是“这台机器上还有什么没登记成应用”，它不是应用列表的一部分：
// 候选随时会消失（服务停了），应用记录不会。所以两边分开放，候选列表里的每一行都
// 只有两个出口——**确认成应用**（走 POST /apps）或什么都不做。
//
// 三条显示纪律：
//   1. `status=error` 不能被读成“本机什么都没有”：那是一个来源都没采成；
//   2. 单个来源失败要单独说，因为另外两个来源的结果仍然可用（status=partial）；
//   3. `missing_bindings` 里的条目是“以前绑过、现在找不到了”，与候选不是一回事——
//      容器重建后 ID 变了，旧的绑定必须重新确认，页面不能当作还在。

import { computed } from 'vue'
import { NCard, NTag } from 'naive-ui'

import {
  DISCOVERY_SOURCE_LABELS,
  type AppDiscoveryCandidate,
  type AppDiscoveryData,
} from '@/api/apps'
import StatePanel from '@/components/StatePanel.vue'
import { formatTimestamp } from '@/utils/format'

const props = defineProps<{
  data: AppDiscoveryData | null
  /** 读取失败时的说明；空串表示读到了。 */
  errorMessage?: string
  /** 能不能确认候选（只有 admin 能创建应用）。 */
  canAct: boolean
  busy?: boolean
}>()

const emit = defineEmits<{ adopt: [candidate: AppDiscoveryCandidate] }>()

/** 已被某个应用登记的候选不再显示“确认”，而是显示它属于谁。 */
const rows = computed(() => props.data?.candidates ?? [])

const collectedLine = computed(() => {
  const at = props.data?.collected_at
  return at === undefined || at === '' ? '' : `采集于 ${formatTimestamp(at)}`
})

function observedLabel(state: AppDiscoveryCandidate['observed_state']): string {
  switch (state) {
    case 'running':
      return '运行中'
    case 'stopped':
      return '已停止'
    case 'failed':
      return '启动失败'
    default:
      return '状态未知'
  }
}

function observedTone(state: AppDiscoveryCandidate['observed_state']): 'success' | 'default' | 'error' | 'warning' {
  switch (state) {
    case 'running':
      return 'success'
    case 'stopped':
      return 'default'
    case 'failed':
      return 'error'
    default:
      return 'warning'
  }
}

function listenersText(candidate: AppDiscoveryCandidate): string {
  if (candidate.listeners.length === 0) return '没有监听端口'
  return candidate.listeners
    .map(
      (listener) =>
        `${listener.loopback ? '127.0.0.1' : listener.address}:${listener.port}/${listener.protocol}`,
    )
    .join('、')
}
</script>

<template>
  <NCard size="small" title="本机候选（还没登记成应用的东西）">
    <div class="discovery">
      <p class="discovery__muted">
        候选来自代理：监听中的端口、持有套接字的 systemd 单元、Docker 容器。
        <strong>确认候选就是新建应用</strong>——没有第二个“确认”接口，因此这里也不会
        出现“一键接管全部”这类动作。
        <span v-if="collectedLine !== ''"> · {{ collectedLine }}</span>
      </p>

      <p v-if="errorMessage && errorMessage !== ''" class="discovery__warn" data-testid="discovery-error">
        读取本机候选失败：{{ errorMessage }}。下面的内容不是“本机什么都没有”，而是这一轮没读到。
      </p>

      <template v-if="data">
        <div class="discovery__sources">
          <NTag
            v-for="source in data.sources"
            :key="source.source"
            size="small"
            :type="source.status === 'ok' ? 'success' : 'error'"
          >
            {{ DISCOVERY_SOURCE_LABELS[source.source] }}：{{ source.status === 'ok' ? `${source.count} 条` : `采集失败${source.error ? `（${source.error}）` : ''}` }}
          </NTag>
          <NTag v-if="data.truncated" size="small" type="warning">候选过多，列表被截断</NTag>
        </div>

        <p v-if="data.status === 'error'" class="discovery__warn">
          三路来源这一轮都失败了（{{ data.error ?? '代理没有给出原因' }}）。
          这一份结果<strong>不能</strong>读成本机什么都没有。
        </p>
        <p v-else-if="data.status === 'partial'" class="discovery__muted">
          部分来源失败：上面失败的来源对应的候选这一轮看不到，其余来源仍然有效。
        </p>

        <StatePanel
          v-if="rows.length === 0 && data.status !== 'error'"
          state="empty"
          title="没有待确认的候选"
          description="本机当前没有“在监听但还没登记成应用”的东西，或者它们都已经登记过了。"
        />

        <ul v-else class="discovery__list" data-testid="discovery-candidates">
          <li v-for="candidate in rows" :key="candidate.target_id" class="discovery__row">
            <div class="discovery__row-head">
              <span class="discovery__name">{{ candidate.title === '' ? candidate.name : candidate.title }}</span>
              <NTag size="small">{{ DISCOVERY_SOURCE_LABELS[candidate.source] }}</NTag>
              <NTag size="small" :type="observedTone(candidate.observed_state)">
                {{ observedLabel(candidate.observed_state) }}
              </NTag>
              <NTag v-if="!candidate.probe_available" size="small" type="warning">
                不可探测
              </NTag>
              <NTag v-if="candidate.bound_app_id !== null" size="small" type="info">
                已属于「{{ candidate.bound_app_name ?? candidate.bound_app_id }}」
              </NTag>
            </div>

            <p class="discovery__muted">{{ candidate.reason }}</p>
            <p class="discovery__muted">
              监听：{{ listenersText(candidate) }} · 探测目标 <code>{{ candidate.target_id }}</code>
              <template v-if="candidate.probe_available">
                （{{ candidate.suggested_scheme }}:{{ candidate.suggested_port }}）
              </template>
              <template v-else> · {{ candidate.probe_note }}</template>
            </p>

            <div class="discovery__actions">
              <NTag v-if="!canAct" size="small">只读账号不能确认候选</NTag>
              <NTag
                v-else-if="candidate.bound_app_id !== null"
                size="small"
                type="info"
              >
                同一个对象只能属于一个应用
              </NTag>
              <button
                v-else
                type="button"
                class="discovery__button"
                :disabled="busy === true"
                :data-target="candidate.target_id"
                @click="emit('adopt', candidate)"
              >
                确认成应用…
              </button>
            </div>
          </li>
        </ul>

        <div v-if="data.missing_bindings.length > 0" class="discovery__missing" data-testid="missing-bindings">
          <p class="discovery__label">绑定已经不在本机了（需要重新确认）</p>
          <ul class="discovery__list">
            <li v-for="issue in data.missing_bindings" :key="issue.app_id">
              <strong>{{ issue.name }}</strong>（{{ issue.app_id }}）绑定
              <code>{{ issue.binding }}</code>：{{ issue.reason }}
            </li>
          </ul>
          <p class="discovery__muted">
            容器被重建后 ID 会变，旧绑定指向的那个对象已经不存在：点“编辑”重新选一次绑定，
            而不是以为它还在跑。
          </p>
        </div>
      </template>
    </div>
  </NCard>
</template>

<style scoped>
.discovery {
  display: flex;
  flex-direction: column;
  gap: 10px;
}

.discovery__muted {
  margin: 0;
  font-size: 13px;
  line-height: 1.7;
  opacity: 0.82;
}

.discovery__warn {
  margin: 0;
  font-size: 13px;
  line-height: 1.7;
  color: #d03050;
}

.discovery__sources {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 8px;
}

.discovery__list {
  margin: 0;
  padding: 0;
  list-style: none;
  display: flex;
  flex-direction: column;
  gap: 10px;
}

.discovery__row {
  display: flex;
  flex-direction: column;
  gap: 4px;
  padding: 10px 12px;
  border: 1px solid rgba(128, 128, 128, 0.22);
  border-radius: 9px;
}

.discovery__row-head {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 8px;
}

.discovery__name {
  font-weight: 700;
  word-break: break-all;
}

.discovery__actions {
  display: flex;
  align-items: center;
  gap: 8px;
}

.discovery__button {
  padding: 3px 10px;
  border: 1px solid rgba(128, 128, 128, 0.45);
  border-radius: 7px;
  background: transparent;
  cursor: pointer;
  font: inherit;
  font-size: 13px;
}

.discovery__button:disabled {
  cursor: not-allowed;
  opacity: 0.6;
}

.discovery__missing {
  display: flex;
  flex-direction: column;
  gap: 6px;
  padding: 10px 12px;
  border: 1px solid rgba(208, 48, 80, 0.35);
  border-radius: 9px;
}

.discovery__label {
  margin: 0;
  font-weight: 600;
}
</style>
