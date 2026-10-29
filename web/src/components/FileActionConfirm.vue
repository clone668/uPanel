<script setup lang="ts">
// 文件动作的确认卡（P06.2）。
//
// 与 ProcessSignalConfirm / ServiceActionConfirm 同一套纪律，这里多两件文件独有的事：
//
//   1) **上传不等于落地**。落地上传（file.commit_upload）的卡片必须说清“字节已经在
//      暂存里，这一步才动目标文件”，否则“上传成功”会被读成“文件已经换掉”。
//   2) **版本冲突是正常结局**。卡片把 expected_version 的含义写出来：这次操作认的是
//      你看见的那一份，别人在这之后改过就会被拒绝——那是保护，不是故障。
//
// 这个组件不发请求：它只把“要不要提交、带什么短语”告诉父组件。allowed、
// requires_reauth 与短语本身全部来自服务端（预检），界面不自己推结论。

import { computed, ref } from 'vue'
import { NAlert, NButton, NInput, NTag } from 'naive-ui'

import { requiresConfirmation, type PreflightData } from '@/api/preflights'
import { formatTimestamp } from '@/utils/format'

const props = defineProps<{
  /** 动作的中文名（例如“删除”）。 */
  actionLabel: string
  /** 目标的可读标识（例如 "conf/nginx.conf"）。 */
  targetLabel: string
  /** 这次操作做了什么的一句话说明。 */
  detail?: string
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
  /** 预检还没到时，界面按动作表提前提示“这一步要求再认证”。 */
  needsReauthHint?: boolean
  /** 危险动作的按钮用 error 色（删除）。 */
  danger?: boolean
}>()

const emit = defineEmits<{ confirm: [phrase: string]; cancel: [] }>()

const typed = ref('')

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
const needsReauth = computed(
  () => props.preflight?.requires_reauth ?? props.needsReauthHint === true,
)
</script>

<template>
  <section class="confirm" :data-allowed="preflight?.allowed ?? false">
    <p class="confirm__line">
      即将对 <code>{{ targetLabel }}</code> 执行：<strong>{{ actionLabel }}</strong>
    </p>

    <p v-if="detail" class="confirm__line confirm__muted">{{ detail }}</p>

    <p v-if="busy" class="confirm__line confirm__muted">正在处理，请勿关闭页面…</p>

    <NAlert v-if="errorMessage" type="error" :title="errorMessage" />

    <template v-if="preflight">
      <p class="confirm__line confirm__muted">
        目标：{{ preflight.target.display_name }} · 预检到期：{{ formatTimestamp(preflight.expires_at) }}
        <template v-if="remainingSeconds !== null && remainingSeconds !== undefined && !expired">
          （还剩 {{ remainingSeconds }} 秒）
        </template>
      </p>

      <NAlert v-if="needsReauth" type="info" :bordered="false">
        这次操作要求再认证：服务端在提交时会核对最近的登录状态，再认证窗口已过就会拒绝执行。
        面板没有“跳过”这一步的开关。
      </NAlert>

      <NAlert v-if="expired" type="warning" :bordered="false">
        预检已过期（有效期 120 秒）。预检绑定的是生成那一刻的文件版本与身份，过期后必须重新生成，
        不能拿旧计划硬提交。
      </NAlert>

      <NAlert v-if="blocked" type="warning" title="预检不允许执行这个操作" :bordered="false">
        <ul class="confirm__list">
          <li v-for="blocker in preflight.blockers" :key="blocker.code + blocker.message">
            <code>{{ blocker.code }}</code> {{ blocker.message }}
          </li>
        </ul>
      </NAlert>

      <div v-if="preflight.warnings.length > 0" class="confirm__block">
        <p class="confirm__line">告警</p>
        <ul class="confirm__list">
          <li v-for="warning in preflight.warnings" :key="warning.code + warning.message">
            <code>{{ warning.code }}</code> {{ warning.message }}
          </li>
        </ul>
      </div>

      <div v-if="preflight.impacts.length > 0" class="confirm__block">
        <p class="confirm__line">影响</p>
        <ul class="confirm__list">
          <li v-for="impact in preflight.impacts" :key="impact.kind + impact.description">
            <NTag size="small">{{ impact.kind }}</NTag> {{ impact.description }}
          </li>
        </ul>
      </div>

      <p v-if="preflight.locks.length > 0" class="confirm__line confirm__muted">
        资源锁：<code v-for="lock in preflight.locks" :key="lock">{{ lock }}</code>
      </p>

      <div v-if="needsPhrase" class="confirm__phrase">
        <p class="confirm__line">
          请输入确认短语（区分大小写）：
          <code class="confirm__phrase-text">{{ preflight.confirmation_phrase }}</code>
        </p>
        <NInput v-model:value="typed" placeholder="在此输入上面的短语" :disabled="busy" />
        <p v-if="typed !== '' && !phraseMatches" class="confirm__line confirm__warn">
          短语与预检给出的不一致。
        </p>
      </div>
    </template>

    <div class="confirm__buttons">
      <NButton
        v-if="canConfirm"
        :type="danger ? 'error' : 'primary'"
        :loading="busy"
        @click="emit('confirm', typed)"
      >
        确认{{ actionLabel }}
      </NButton>
      <NButton secondary :disabled="busy" @click="emit('cancel')">取消</NButton>
    </div>
  </section>
</template>

<style scoped>
.confirm {
  display: flex;
  flex-direction: column;
  gap: 8px;
  padding: 12px 14px;
  border: 1px solid var(--rule);
  border-radius: var(--r-2);
}

.confirm__line {
  margin: 0;
  line-height: 1.7;
}

.confirm__muted {
  font-size: var(--fs-md);
  opacity: 0.78;
}

.confirm__warn {
  color: var(--bad);
}

.confirm__block {
  display: flex;
  flex-direction: column;
  gap: 4px;
}

.confirm__list {
  margin: 0;
  padding-left: 20px;
  line-height: 1.8;
}

.confirm__phrase {
  display: flex;
  flex-direction: column;
  gap: 6px;
}

.confirm__phrase-text {
  font-weight: 700;
  letter-spacing: 0.04em;
}

.confirm__buttons {
  display: flex;
  gap: 10px;
}
</style>
