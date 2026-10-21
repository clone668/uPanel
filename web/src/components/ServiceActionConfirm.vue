<script setup lang="ts">
// 操作确认卡（P05.1）：把预检结论原样摊开，然后才允许提交。
//
// 顺序故意是“影响 → 阻塞项 → 短语 → 按钮”：
//   - 阻塞项（blockers）非空时按钮直接不出现，因为服务端已经判了不行；
//   - 告警与影响必须显示在前面，别让人点完才看见“会中断服务”；
//   - 确认短语由服务端生成、明文只出现一次，这里只做“输对了没”的判断。
//
// 这个组件不发请求：它只把“要不要提交、带什么短语”告诉父组件。

import { computed, ref } from 'vue'
import { NAlert, NButton, NInput, NTag } from 'naive-ui'

import { requiresConfirmation, type PreflightData } from '@/api/preflights'
import {
  SERVICE_OPERATION_VERBS,
  interruptsService,
  type ServiceOperation,
} from '@/api/services'
import { formatTimestamp } from '@/utils/format'

const props = defineProps<{
  unit: string
  operation: ServiceOperation
  /** null 表示预检还没拿到（正在请求或已失败）。 */
  preflight: PreflightData | null
  /** 正在请求预检 / 正在提交任务。 */
  busy: boolean
  /** 请求失败时的说明（已翻译成人话）。 */
  errorMessage?: string | null
  /** 预检是否已经过期（父组件按秒刷新）。过期后只能重新预检，不能提交。 */
  expired?: boolean
  /** 预检还剩多少秒；null 表示不显示倒计时。 */
  remainingSeconds?: number | null
}>()

const emit = defineEmits<{ confirm: [phrase: string]; cancel: [] }>()

const typed = ref('')

const verb = computed(() => SERVICE_OPERATION_VERBS[props.operation])
const needsPhrase = computed(() => (props.preflight ? requiresConfirmation(props.preflight) : false))
const blocked = computed(() => (props.preflight ? !props.preflight.allowed : true))
const phraseMatches = computed(() => !needsPhrase.value || typed.value === props.preflight?.confirmation_phrase)
const canConfirm = computed(
  () =>
    props.preflight !== null &&
    props.preflight.allowed &&
    phraseMatches.value &&
    !props.busy &&
    props.expired !== true,
)
const interruptive = computed(() => interruptsService(props.operation))
</script>

<template>
  <section class="action" :data-operation="operation" :data-allowed="preflight?.allowed ?? false">
    <p class="action__line">
      即将对 <code>{{ unit }}</code> 执行：<strong>{{ verb }}</strong>
    </p>

    <p v-if="interruptive" class="action__line action__warn">
      这是一次会中断服务可用性的操作：操作期间该服务提供的能力会中断，已建立的连接可能被断开。
    </p>

    <p v-if="busy" class="action__line action__muted">正在处理，请勿关闭页面…</p>

    <NAlert v-if="errorMessage" type="error" :title="errorMessage" />

    <template v-if="preflight">
      <p class="action__line action__muted">
        目标：{{ preflight.target.display_name }} · 预检到期：{{ formatTimestamp(preflight.expires_at) }}
        <template v-if="remainingSeconds !== null && remainingSeconds !== undefined && !expired">
          （还剩 {{ remainingSeconds }} 秒）
        </template>
      </p>

      <NAlert v-if="expired" type="warning" :bordered="false">
        预检已过期（有效期 120 秒）。预检绑定的是提交那一刻的状态，过期后必须重新生成，
        不能拿旧计划硬提交。
      </NAlert>

      <NAlert
        v-if="blocked"
        type="warning"
        title="预检不允许执行这个操作"
        :bordered="false"
      >
        <ul class="action__list">
          <li v-for="blocker in preflight.blockers" :key="blocker.code + blocker.message">
            <code>{{ blocker.code }}</code> {{ blocker.message }}
          </li>
        </ul>
      </NAlert>

      <div v-if="preflight.warnings.length > 0" class="action__block">
        <p class="action__line">告警</p>
        <ul class="action__list">
          <li v-for="warning in preflight.warnings" :key="warning.code + warning.message">
            <code>{{ warning.code }}</code> {{ warning.message }}
          </li>
        </ul>
      </div>

      <div v-if="preflight.impacts.length > 0" class="action__block">
        <p class="action__line">影响</p>
        <ul class="action__list">
          <li v-for="impact in preflight.impacts" :key="impact.kind + impact.description">
            <NTag size="small">{{ impact.kind }}</NTag> {{ impact.description }}
          </li>
        </ul>
      </div>

      <p v-if="preflight.locks.length > 0" class="action__line action__muted">
        资源锁：<code v-for="lock in preflight.locks" :key="lock">{{ lock }}</code>
      </p>

      <div v-if="needsPhrase" class="action__phrase">
        <p class="action__line">
          请输入确认短语（区分大小写）：
          <code class="action__phrase-text">{{ preflight.confirmation_phrase }}</code>
        </p>
        <NInput v-model:value="typed" placeholder="在此输入上面的短语" :disabled="busy" />
        <p v-if="typed !== '' && !phraseMatches" class="action__line action__warn">
          短语与预检给出的不一致。
        </p>
      </div>
    </template>

    <div class="action__buttons">
      <NButton v-if="canConfirm" type="primary" :loading="busy" @click="emit('confirm', typed)">
        确认{{ verb }}
      </NButton>
      <NButton secondary :disabled="busy" @click="emit('cancel')">取消</NButton>
    </div>
  </section>
</template>

<style scoped>
.action {
  display: flex;
  flex-direction: column;
  gap: 8px;
  padding: 12px 14px;
  border: 1px solid rgba(128, 128, 128, 0.3);
  border-radius: 10px;
}

.action__line {
  margin: 0;
  line-height: 1.7;
}

.action__muted {
  font-size: 13px;
  opacity: 0.78;
}

.action__warn {
  color: #d03050;
}

.action__block {
  display: flex;
  flex-direction: column;
  gap: 4px;
}

.action__list {
  margin: 0;
  padding-left: 20px;
  line-height: 1.8;
}

.action__phrase {
  display: flex;
  flex-direction: column;
  gap: 6px;
}

.action__phrase-text {
  font-weight: 700;
  letter-spacing: 0.04em;
}

.action__buttons {
  display: flex;
  gap: 10px;
}
</style>
