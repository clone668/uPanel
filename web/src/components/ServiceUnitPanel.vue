<script setup lang="ts">
// 单元详情面板（P05.1）：只是事实，不含任何按钮。
//
// 分开的理由：按钮要跟着“预检 → 确认 → 任务”走，而事实是只读的。
// 把两者写在一个组件里，迟早会出现“其实你点不动，但按钮在那儿”的状态。
//
// 三件必须说清楚的事：
//   1) 受保护单元为什么不能碰（理由逐条列出，而不是只说“受保护”）；
//   2) 开机启动与当前运行是两件事，分开写；
//   3) 能力为空不等于“坏掉了”：可能是受保护、模板、not-found，也可能是当前状态
//      下确实没有可执行的动作。这里如实解释，不藏按钮装没事。

import { computed } from 'vue'
import { NCard, NTag } from 'naive-ui'

import {
  activeStateLabel,
  activeStateType,
  enablementLabel,
  loadStateLabel,
  loadStateType,
  protectionCategoryLabel,
  SERVICE_OPERATION_LABELS,
  type ServiceOperation,
  type ServiceUnit,
} from '@/api/services'
import { UNKNOWN_TEXT, bytesTitle, formatBytes, formatTimestamp } from '@/utils/format'

const props = defineProps<{ unit: ServiceUnit }>()

const memory = computed(() => formatBytes(props.unit.memory_bytes))

const capabilityLabels = computed(() =>
  props.unit.capabilities.map((op: ServiceOperation) => SERVICE_OPERATION_LABELS[op]),
)

/** 没有能力时给出**具体**原因，而不是一句“不可操作”。 */
const emptyCapabilityReason = computed<string | null>(() => {
  if (props.unit.capabilities.length > 0) return null
  if (props.unit.protection.protected) return '这个单元在保护名单里，面板不会对它执行任何操作。'
  if (props.unit.template) return '这是模板单元，必须指定实例（例如 foo@实例名.service）才能操作。'
  if (props.unit.load_state === 'not-found') return '这台机器上没有这个单元，没有可操作的对象。'
  if (props.unit.load_state === 'error' || props.unit.load_state === 'bad-setting') {
    return '单元装载失败或配置有错误，需要先在主机上修好。'
  }
  return '当前状态下没有可执行的动作。'
})
</script>

<template>
  <NCard size="small" title="单元状态">
    <div class="unit-panel">
      <p class="unit-panel__line">
        <strong>{{ unit.unit }}</strong>
        <span class="unit-panel__muted unit-panel__desc">{{ unit.description || '（没有描述）' }}</span>
      </p>

      <p class="unit-panel__line">
        运行状态：
        <NTag size="small" :type="activeStateType(unit.active_state)">
          {{ activeStateLabel(unit.active_state) }}
        </NTag>
        <span class="unit-panel__muted">
          （{{ unit.active_state }} / {{ unit.sub_state === '' ? UNKNOWN_TEXT : unit.sub_state }}）
        </span>
      </p>

      <p class="unit-panel__line">
        开机启动：<strong>{{ enablementLabel(unit.enablement) }}</strong>
        <span class="unit-panel__muted">（UnitFileState = {{ unit.unit_file_state || '空' }}）</span>
      </p>

      <p class="unit-panel__line">
        装载状态：
        <NTag size="small" :type="loadStateType(unit.load_state)">
          {{ loadStateLabel(unit.load_state) }}
        </NTag>
        <span v-if="unit.template" class="unit-panel__muted">模板单元（需要实例名）</span>
      </p>

      <p class="unit-panel__line">
        进程号：{{ unit.main_pid ?? UNKNOWN_TEXT }} · 任务数：{{ unit.tasks_current ?? UNKNOWN_TEXT }}
        · 内存：<span :title="bytesTitle(unit.memory_bytes)">{{ memory }}</span> · 重启次数：{{
          unit.restart_count ?? UNKNOWN_TEXT
        }}
      </p>

      <p class="unit-panel__line">
        启动于：{{ formatTimestamp(unit.started_at) }} · 本次读取：{{
          formatTimestamp(unit.checked_at)
        }}
      </p>

      <p class="unit-panel__line unit-panel__muted">
        单元文件：<code>{{ unit.fragment_path === '' ? '（没有单元文件）' : unit.fragment_path }}</code>
      </p>

      <div class="unit-panel__protection" :data-protected="unit.protection.protected">
        <template v-if="unit.protection.protected">
          <p class="unit-panel__line">
            <NTag size="small" type="warning">
              受保护：{{ protectionCategoryLabel(unit.protection.category) }}
            </NTag>
          </p>
          <ul class="unit-panel__reasons">
            <li v-for="reason in unit.protection.reasons" :key="reason">{{ reason }}</li>
          </ul>
        </template>
        <p v-else class="unit-panel__line unit-panel__muted">
          不在保护名单里（保护名单包含：面板自身、SSH、systemd 核心、网络基础服务，
          以及由反代/共享/容器模块负责的守护进程）。
        </p>
      </div>

      <p class="unit-panel__line">
        可执行动作：<strong>{{ capabilityLabels.length > 0 ? capabilityLabels.join('、') : '无' }}</strong>
      </p>
      <p v-if="emptyCapabilityReason" class="unit-panel__line unit-panel__muted">
        {{ emptyCapabilityReason }}
      </p>
    </div>
  </NCard>
</template>

<style scoped>
.unit-panel {
  display: flex;
  flex-direction: column;
  gap: 8px;
}

.unit-panel__line {
  margin: 0;
  line-height: 1.7;
}

.unit-panel__muted {
  font-size: var(--fs-sm);
  color: var(--ink-3);
}

/* 单元名和描述贴在一起会读成 "ssh.serviceOpenBSD…"，中间必须有间隔。 */
.unit-panel__desc {
  margin-left: 6px;
}

.unit-panel__protection {
  display: flex;
  flex-direction: column;
  gap: 4px;
  padding: 8px 10px;
  border-radius: var(--r-2);
  background: var(--surface-2);
}

.unit-panel__reasons {
  margin: 0;
  padding-left: 20px;
  line-height: 1.7;
}
</style>
