<script setup lang="ts">
// 进程信号确认卡（P05.2）。
//
// 与服务操作确认卡同一套纪律，但多了两件进程独有的事必须写在卡片上：
//   1) 身份是 pid + starttime。卡片把 starttime 显式打出来，因为“我要杀的是这个
//      进程，不是现在占着这个 pid 的进程”是这一步唯一容易搞错的地方；
//   2) TERM 与 KILL 是两次独立操作。卡片里没有任何“失败后自动升级”的开关，
//      也不给 KILL 提供跳过确认的路径。
//
// 这个组件不发请求：它只把“要不要提交、带什么短语”告诉父组件。

import { computed, ref } from 'vue'
import { NAlert, NButton, NInput, NTag } from 'naive-ui'

import { requiresConfirmation, type PreflightData } from '@/api/preflights'
import {
  SIGNAL_LABELS,
  SIGNAL_VERBS,
  signalNeedsPhrase,
  type ProcessEntry,
  type ProcessSignal,
} from '@/api/processes'
import { formatTimestamp } from '@/utils/format'

const props = defineProps<{
  process: ProcessEntry
  signal: ProcessSignal
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

const verb = computed(() => SIGNAL_VERBS[props.signal])
const needsPhrase = computed(() => (props.preflight ? requiresConfirmation(props.preflight) : false))
const blocked = computed(() => (props.preflight ? !props.preflight.allowed : true))
const phraseMatches = computed(
  () => !needsPhrase.value || typed.value === props.preflight?.confirmation_phrase,
)
const canConfirm = computed(
  () =>
    props.preflight !== null &&
    props.preflight.allowed &&
    phraseMatches.value &&
    !props.busy &&
    props.expired !== true,
)
const needsReauth = computed(() => props.preflight?.requires_reauth ?? signalNeedsPhrase(props.signal))
</script>

<template>
  <section class="signal" :data-signal="signal" :data-allowed="preflight?.allowed ?? false">
    <p class="signal__line">
      即将对 <code>{{ process.command }}</code> 执行：<strong>{{ verb }}</strong>
    </p>

    <p class="signal__line signal__muted">
      PID <code>{{ process.pid }}</code> · starttime
      <code>{{ process.start_time_ticks }}</code> · 属主 {{ process.owner }}
      <template v-if="process.unit !== ''"> · 单元 <code>{{ process.unit }}</code></template>
    </p>

    <p class="signal__line signal__muted">
      面板按“PID + starttime”确认目标：如果这个 PID 在这之后被内核分配给了别的进程，
      starttime 就会不同，这次操作会被拒绝，而不是结束那个新进程。
    </p>

    <p v-if="signal === 'TERM'" class="signal__line">
      请求退出会先给进程一个清理的机会（{{ SIGNAL_LABELS.TERM }}）。如果它在宽限期内没有退出，
      面板会如实报告“未退出”，<strong>不会自动升级</strong>成强制结束——那需要你另外发起一次
      强制结束并重新确认。
    </p>
    <p v-else class="signal__line signal__warn">
      强制结束会立即终止进程（SIGKILL），进程没有机会保存数据或清理临时文件。
      这是独立于“请求退出”的操作，自带再认证与确认短语。
    </p>

    <p v-if="busy" class="signal__line signal__muted">正在处理，请勿关闭页面…</p>

    <NAlert v-if="errorMessage" type="error" :title="errorMessage" />

    <template v-if="preflight">
      <p class="signal__line signal__muted">
        目标：{{ preflight.target.display_name }} · 预检到期：{{ formatTimestamp(preflight.expires_at) }}
        <template v-if="remainingSeconds !== null && remainingSeconds !== undefined && !expired">
          （还剩 {{ remainingSeconds }} 秒）
        </template>
      </p>

      <NAlert v-if="needsReauth" type="info" :bordered="false">
        这次操作要求再认证：服务端在提交时会核对最近的登录状态，必要时会要求重新输入密码。
        拒绝再认证意味着这次操作不会执行。
      </NAlert>

      <NAlert v-if="expired" type="warning" :bordered="false">
        预检已过期（有效期 120 秒）。预检绑定的是提交那一刻的进程身份，过期后必须重新生成，
        不能拿旧计划硬提交。
      </NAlert>

      <NAlert v-if="blocked" type="warning" title="预检不允许执行这个操作" :bordered="false">
        <ul class="signal__list">
          <li v-for="blocker in preflight.blockers" :key="blocker.code + blocker.message">
            <code>{{ blocker.code }}</code> {{ blocker.message }}
          </li>
        </ul>
      </NAlert>

      <div v-if="preflight.warnings.length > 0" class="signal__block">
        <p class="signal__line">告警</p>
        <ul class="signal__list">
          <li v-for="warning in preflight.warnings" :key="warning.code + warning.message">
            <code>{{ warning.code }}</code> {{ warning.message }}
          </li>
        </ul>
      </div>

      <div v-if="preflight.impacts.length > 0" class="signal__block">
        <p class="signal__line">影响</p>
        <ul class="signal__list">
          <li v-for="impact in preflight.impacts" :key="impact.kind + impact.description">
            <NTag size="small">{{ impact.kind }}</NTag> {{ impact.description }}
          </li>
        </ul>
      </div>

      <p v-if="preflight.locks.length > 0" class="signal__line signal__muted">
        资源锁：<code v-for="lock in preflight.locks" :key="lock">{{ lock }}</code>
      </p>

      <div v-if="needsPhrase" class="signal__phrase">
        <p class="signal__line">
          请输入确认短语（区分大小写）：
          <code class="signal__phrase-text">{{ preflight.confirmation_phrase }}</code>
        </p>
        <NInput v-model:value="typed" placeholder="在此输入上面的短语" :disabled="busy" />
        <p v-if="typed !== '' && !phraseMatches" class="signal__line signal__warn">
          短语与预检给出的不一致。
        </p>
      </div>
    </template>

    <div class="signal__buttons">
      <NButton
        v-if="canConfirm"
        :type="signal === 'KILL' ? 'error' : 'primary'"
        :loading="busy"
        @click="emit('confirm', typed)"
      >
        确认{{ verb }}
      </NButton>
      <NButton secondary :disabled="busy" @click="emit('cancel')">取消</NButton>
    </div>
  </section>
</template>

<style scoped>
.signal {
  display: flex;
  flex-direction: column;
  gap: 8px;
  padding: 12px 14px;
  border: 1px solid rgba(128, 128, 128, 0.3);
  border-radius: 10px;
}

.signal__line {
  margin: 0;
  line-height: 1.7;
}

.signal__muted {
  font-size: 13px;
  opacity: 0.78;
}

.signal__warn {
  color: #d03050;
}

.signal__block {
  display: flex;
  flex-direction: column;
  gap: 4px;
}

.signal__list {
  margin: 0;
  padding-left: 20px;
  line-height: 1.8;
}

.signal__phrase {
  display: flex;
  flex-direction: column;
  gap: 6px;
}

.signal__phrase-text {
  font-weight: 700;
  letter-spacing: 0.04em;
}

.signal__buttons {
  display: flex;
  gap: 10px;
}
</style>
