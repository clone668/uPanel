<script setup lang="ts">
// 概览页（P01.3 版）。
//
// 这个页面的职责只有一个：证明“前端骨架 + 统一 API 客户端 + 状态组件”真的连得上服务。
// 系统采集（版本、CPU、内存、负载、磁盘、服务、应用、任务）属于 P04 及之后，
// 这里**不编造任何数据**，只显示真实的 /health/live 结果与它自己的 request_id。
//
// 页面上明确写出“当前能力到此为止”，避免被误认为系统概览已完成。

import { onMounted, ref } from 'vue'
import { NCard, NTag } from 'naive-ui'

import { ApiError, get } from '@/api/client'
import ApiErrorPanel from '@/components/ApiErrorPanel.vue'
import StatePanel from '@/components/StatePanel.vue'
import { useModeStore } from '@/stores/mode'

interface HealthLive {
  status: string
}

type Phase = 'loading' | 'ready' | 'error'

const modeStore = useModeStore()
const phase = ref<Phase>('loading')
const error = ref<ApiError | null>(null)
const health = ref<HealthLive | null>(null)
const requestId = ref('')
const checkedAt = ref('')

async function load(): Promise<void> {
  phase.value = 'loading'
  error.value = null
  try {
    const result = await get<HealthLive>('/health/live', { timeoutMs: 8000 })
    health.value = result.data
    requestId.value = result.requestId
    checkedAt.value = new Date().toLocaleString('zh-CN', { hour12: false })
    phase.value = 'ready'
  } catch (caught) {
    // 只接受 ApiError：其他异常说明是前端自身 bug，交给全局错误边界处理。
    if (caught instanceof ApiError) {
      error.value = caught
      phase.value = 'error'
      return
    }
    throw caught
  }
}

onMounted(() => {
  void load()
})
</script>

<template>
  <section class="overview">
    <NCard title="服务连通性" size="small">
      <template #header-extra>
        <NTag size="small" :type="modeStore.isMock ? 'warning' : 'success'">
          模式：{{ modeStore.isMock ? 'mock（演示数据）' : 'production' }}
        </NTag>
      </template>

      <StatePanel v-if="phase === 'loading'" state="loading" title="正在检查面板服务" />
      <ApiErrorPanel v-else-if="phase === 'error' && error" :error="error" @retry="load" />
      <div v-else-if="health" class="overview__ready">
        <p class="overview__line">
          存活检查：<strong>{{ health.status === 'ok' ? '服务可响应' : '状态未知' }}</strong>
        </p>
        <p class="overview__meta">检查时间：{{ checkedAt }}</p>
        <p class="overview__meta">request_id：<code>{{ requestId }}</code></p>
      </div>
    </NCard>

    <NCard title="当前阶段能力" size="small">
      <p class="overview__line">
        本页目前只验证前端骨架与 API 客户端。系统概况（系统版本、CPU、内存、负载、磁盘、
        异常服务、应用状态、待更新、最近任务）在 P04 起逐步接入。
      </p>
      <p class="overview__meta">
        未接入的模块不在导航中出现，因此界面上没有可点击但无效的入口。
      </p>
    </NCard>
  </section>
</template>

<style scoped>
.overview {
  display: flex;
  flex-direction: column;
  gap: 16px;
  max-width: 960px;
}

.overview__ready {
  display: flex;
  flex-direction: column;
  gap: 6px;
}

.overview__line {
  margin: 0;
  line-height: 1.7;
}

.overview__meta {
  margin: 0;
  font-size: 13px;
  opacity: 0.8;
  word-break: break-all;
}
</style>
