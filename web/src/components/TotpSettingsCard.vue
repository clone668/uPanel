<script setup lang="ts">
// 二次验证（TOTP）设置卡片（P14.6）。
//
// 三件事按顺序摆在一张卡里：登记（生成种子 → 用手机扫码/手抄 → 输一次码确认）、
// 一次性恢复码的展示、以及关闭。
//
// 几个规则是硬写在界面上的，因为它们对应服务端真实的约束：
//
//   1. `otpauth_uri` 只显示一次，**不落任何存储**（不写 localStorage、不进 URL）。
//      它就是种子本身；这一层不做二维码图片——P00 冻结的依赖集里没有 QR 编码器，
//      为一个二维码扩依赖不划算，手抄 URI 或复制粘贴同样能让 authenticator 收下。
//   2. 十枚恢复码只在确认成功那一刻出现。关掉这张卡就再也拿不到，服务端只留哈希。
//   3. 关闭要两步：第 1 步用口令 + 当前验证码完成再认证；第 2 步用**恢复码**或
//      「下一个」验证码关闭。同一个六位数不能在两步里各用一次（服务端按时间步防重放），
//      所以界面上直接这么写，而不是等人试了两次再说"码刚刚用过"。
//   4. 关闭后会作废这个账号的其它会话；发起关闭的这一条保留。

import { computed, ref } from 'vue'
import { NAlert, NButton, NCheckbox, NCard, NForm, NFormItem, NInput, NRadio, NRadioGroup, NSpace, NTag } from 'naive-ui'

import { ApiError } from '@/api/client'
import { reauthPassword } from '@/api/auth'
import { totpConfirm, totpDisable, totpSetup, type TotpSetupData } from '@/api/totp'
import SecondFactorField from '@/components/SecondFactorField.vue'
import { isCodeComplete, isRecoveryCodeShape, sanitizeCode } from '@/components/secondfactor'

const props = withDefaults(
  defineProps<{
    /** 这个账号当前是否已启用二次验证（来自 /auth/session 的 totp_enabled）。 */
    enabled: boolean
  }>(),
  { enabled: false },
)

const emit = defineEmits<{ 'enabled-change': [boolean] }>()

// ---------------------------------------------------------------- 公共状态

const password = ref('')
const busy = ref(false)
const error = ref<string | null>(null)
const notice = ref<string | null>(null)

// ---------------------------------------------------------------- 登记

const enrollment = ref<TotpSetupData | null>(null)
const enrollCode = ref('')
const recoveryCodes = ref<string[] | null>(null)
const acknowledged = ref(false)

const canBeginEnroll = computed(
  () => password.value !== '' && enrollment.value === null && !busy.value,
)
const canConfirmEnroll = computed(
  () => enrollment.value !== null && isCodeComplete(enrollCode.value) && !busy.value,
)

/** 第 1 步：证明是本人（口令），然后让代理生成一条种子。 */
async function beginEnroll(): Promise<void> {
  if (!canBeginEnroll.value) return
  busy.value = true
  error.value = null
  notice.value = null
  try {
    await reauthPassword(password.value)
    enrollment.value = (await totpSetup()).data
    enrollCode.value = ''
    notice.value =
      '已生成一条尚未启用的登记：把下面的字符加进 authenticator，再用它显示的六位数确认。' +
      '十分钟不确认，种子会自动销毁。'
  } catch (caught) {
    error.value = describe(caught, '生成登记失败')
  } finally {
    busy.value = false
  }
}

/** 第 2 步：用手机上的六位数确认。确认之后才真正启用，并发放十枚恢复码。 */
async function confirmEnroll(): Promise<void> {
  if (!canConfirmEnroll.value || enrollment.value === null) return
  busy.value = true
  error.value = null
  try {
    const res = await totpConfirm(enrollment.value.enrollment_id, enrollCode.value)
    enrollment.value = null
    enrollCode.value = ''
    password.value = ''
    recoveryCodes.value = res.data.recovery_codes
    acknowledged.value = false
    notice.value = null
    emit('enabled-change', true)
  } catch (caught) {
    // 码不对时保留登记：换一个码再试，不用重新生成种子。
    error.value = describe(caught, '确认失败')
  } finally {
    busy.value = false
  }
}

/** 放弃这次登记。代理侧的临时种子十分钟后自己销毁。 */
function cancelEnroll(): void {
  enrollment.value = null
  enrollCode.value = ''
  notice.value = null
  error.value = null
}

function finishRecovery(): void {
  recoveryCodes.value = null
  acknowledged.value = false
  notice.value = '二次验证已启用。下次登录需要手机上的六位数；恢复码只在关闭时还能用一次。'
}

// ---------------------------------------------------------------- 关闭

const reauthenticated = ref(false)
const secondFactorKind = ref<'recovery' | 'totp'>('recovery')
const secondFactor = ref('')

const secondFactorReady = computed(() => {
  if (secondFactorKind.value === 'recovery') return isRecoveryCodeShape(secondFactor.value)
  return isCodeComplete(secondFactor.value)
})
const canDisable = computed(
  () => reauthenticated.value && password.value !== '' && secondFactorReady.value && !busy.value,
)

/** 第 1 步：口令 + 当前验证码，把再认证窗口推到 5 分钟后。 */
async function startDisable(): Promise<void> {
  if (password.value === '' || !isCodeComplete(secondFactor.value) || busy.value) return
  busy.value = true
  error.value = null
  notice.value = null
  try {
    await reauthPassword(password.value, secondFactor.value)
    reauthenticated.value = true
    secondFactor.value = ''
    secondFactorKind.value = 'recovery'
    notice.value =
      '再认证完成（5 分钟内有效）。现在用一枚恢复码关闭；要用验证码的话，请等手机上出现下一个六位数——' +
      '刚才那个已经用掉了。'
  } catch (caught) {
    error.value = describe(caught, '再认证失败')
  } finally {
    busy.value = false
  }
}

/** 第 2 步：口令 + 恢复码（或下一个验证码）真正关闭。 */
async function confirmDisable(): Promise<void> {
  if (!canDisable.value) return
  busy.value = true
  error.value = null
  try {
    const input =
      secondFactorKind.value === 'recovery'
        ? { password: password.value, recovery_code: secondFactor.value.trim() }
        : { password: password.value, totp: secondFactor.value }
    const res = await totpDisable(input)
    resetDisable()
    emit('enabled-change', false)
    notice.value =
      `二次验证已关闭，同时作废了 ${res.data.revoked_sessions} 条其它会话。` +
      '这台服务器现在只靠口令登录了，建议尽快重新登记。'
  } catch (caught) {
    error.value = describe(caught, '关闭失败')
  } finally {
    busy.value = false
  }
}

function resetDisable(): void {
  reauthenticated.value = false
  secondFactorKind.value = 'recovery'
  secondFactor.value = ''
  password.value = ''
  error.value = null
  notice.value = null
}

// ---------------------------------------------------------------- 小工具

/** 把错误翻成一句人话；再认证窗口过期时顺手把关闭流程退回第 1 步。 */
function describe(caught: unknown, fallback: string): string {
  if (caught instanceof ApiError) {
    if (caught.code === 'REAUTH_REQUIRED') {
      reauthenticated.value = false
      return '这次操作需要 5 分钟内的再认证，请先做第 1 步'
    }
    return caught.message
  }
  return fallback
}

function copyURI(): void {
  const text = enrollment.value?.otpauth_uri ?? ''
  if (text === '') return
  // 剪贴板可能不可用（非安全上下文、权限被拒）。失败不假装成功：uri 就在页面上。
  void navigator.clipboard?.writeText(text).then(
    () => {
      notice.value = '已复制。粘到 authenticator 之后请把剪贴板清掉。'
    },
    () => {
      notice.value = '浏览器拒绝了剪贴板访问，请手动选中复制。'
    },
  )
}
</script>

<template>
  <NCard title="二次验证（TOTP）" size="small" data-testid="totp-card">
    <NSpace vertical :size="12">
      <NSpace align="center">
        <span>状态</span>
        <NTag v-if="enabled" type="success" size="small" data-testid="totp-state">已启用</NTag>
        <NTag v-else type="warning" size="small" data-testid="totp-state">未启用</NTag>
        <span class="hint">只影响本机面板登录；丢手机时用恢复码或服务器上的 upanelctl 恢复。</span>
      </NSpace>

      <NAlert v-if="error" type="error" :bordered="false" data-testid="totp-error">{{ error }}</NAlert>
      <NAlert v-if="notice" type="info" :bordered="false" data-testid="totp-notice">{{ notice }}</NAlert>

      <!-- 恢复码：只在确认成功那一刻出现 -->
      <div v-if="recoveryCodes" data-testid="totp-recovery">
        <NAlert type="warning" :bordered="false">
          这 {{ recoveryCodes.length }} 枚恢复码<strong>只显示这一次</strong>。抄到纸上或存进密码管理器；
          关掉这一块就再也拿不到了（服务端只留哈希）。
        </NAlert>
        <ul class="codes">
          <li v-for="code in recoveryCodes" :key="code"><code>{{ code }}</code></li>
        </ul>
        <NCheckbox v-model:checked="acknowledged" data-testid="totp-recovery-ack">
          我已经把这十枚码记下来了
        </NCheckbox>
        <NButton
          class="full"
          type="primary"
          :disabled="!acknowledged"
          data-testid="totp-recovery-done"
          @click="finishRecovery"
        >
          完成
        </NButton>
      </div>

      <!-- 登记中：显示 URI，等一个六位数 -->
      <div v-else-if="enrollment" data-testid="totp-enrollment">
        <p class="hint">
          issuer {{ enrollment.issuer }} / 账号 {{ enrollment.account }} / {{ enrollment.digits }} 位、
          {{ enrollment.period }} 秒一格{{ enrollment.expires_at ? `；这条登记在 ${enrollment.expires_at} 失效` : '' }}
        </p>
        <pre class="uri" data-testid="totp-uri">{{ enrollment.otpauth_uri }}</pre>
        <p class="hint">
          这串字符就是种子本身：不要截图发给别人，也不要存进浏览器。把整串复制进 authenticator
          （或手动输入其中的 secret），它会在应用里显示成「{{ enrollment.issuer }} / {{ enrollment.account }}」。
        </p>
        <NButton size="small" :disabled="busy" data-testid="totp-copy" @click="copyURI">
          复制这串字符
        </NButton>
        <NForm @submit.prevent="confirmEnroll">
          <SecondFactorField
            :model-value="enrollCode"
            :disabled="busy"
            label="手机上的六位数"
            testid="totp-enroll-code"
            @update:model-value="(next: string) => (enrollCode = sanitizeCode(next))"
          />
          <NSpace>
            <NButton
              type="primary"
              :loading="busy"
              :disabled="!canConfirmEnroll"
              attr-type="submit"
              data-testid="totp-enroll-confirm"
            >
              确认并启用
            </NButton>
            <NButton :disabled="busy" data-testid="totp-enroll-cancel" @click="cancelEnroll">
              放弃这次登记
            </NButton>
          </NSpace>
        </NForm>
      </div>

      <!-- 未启用：开始登记 -->
      <NForm v-else-if="!enabled" @submit.prevent="beginEnroll" data-testid="totp-enroll-form">
        <p class="hint">
          启用之后登录要额外输入手机上的六位数。请先确认手机与服务器的时间是准的（相差超过 30 秒会一直对不上）。
        </p>
        <NFormItem label="面板密码">
          <NInput
            v-model:value="password"
            type="password"
            show-password-on="click"
            autocomplete="current-password"
            :disabled="busy"
            data-testid="totp-password"
          />
        </NFormItem>
        <NButton type="primary" :loading="busy" :disabled="!canBeginEnroll" attr-type="submit"
          data-testid="totp-enroll-start">
          开始登记
        </NButton>
      </NForm>

      <!-- 已启用：关闭 -->
      <NForm v-else @submit.prevent="reauthenticated ? confirmDisable() : startDisable()"
        data-testid="totp-disable-form">
        <p class="hint">
          第 1 步：口令 + 手机上的当前验证码（完成再认证）。第 2 步：一枚恢复码，或手机上<strong>下一个</strong>六位数。
          关闭会作废这个账号的其它会话。
        </p>
        <NFormItem label="面板密码">
          <NInput
            v-model:value="password"
            type="password"
            show-password-on="click"
            autocomplete="current-password"
            :disabled="busy"
            data-testid="totp-disable-password"
          />
        </NFormItem>

        <SecondFactorField
          v-if="!reauthenticated"
          :model-value="secondFactor"
          :disabled="busy"
          label="当前验证码"
          testid="totp-disable-reauth-code"
          @update:model-value="(next: string) => (secondFactor = sanitizeCode(next))"
        />

        <template v-else>
          <NRadioGroup v-model:value="secondFactorKind" data-testid="totp-disable-kind">
            <NSpace>
              <NRadio value="recovery">用恢复码</NRadio>
              <NRadio value="totp">用下一个验证码</NRadio>
            </NSpace>
          </NRadioGroup>
          <NFormItem :label="secondFactorKind === 'recovery' ? '恢复码' : '验证码'">
            <NInput
              :value="secondFactor"
              :placeholder="secondFactorKind === 'recovery' ? '3F7K9-QM2XP' : '六位数字'"
              :disabled="busy"
              data-testid="totp-disable-second-factor"
              @update:value="(next: string) => (secondFactor = next)"
            />
          </NFormItem>
        </template>

        <NSpace>
          <NButton
            type="primary"
            :loading="busy"
            :disabled="reauthenticated ? !canDisable : !isCodeComplete(secondFactor) || password === ''"
            attr-type="submit"
            data-testid="totp-disable-submit"
          >
            {{ reauthenticated ? '关闭二次验证' : '第 1 步：再认证' }}
          </NButton>
          <NButton v-if="reauthenticated" :disabled="busy" data-testid="totp-disable-cancel" @click="resetDisable">
            取消
          </NButton>
        </NSpace>
      </NForm>
    </NSpace>
  </NCard>
</template>

<style scoped>
.hint {
  font-size: var(--fs-sm);
  line-height: 1.6;
  opacity: 0.78;
}

.uri {
  margin: 8px 0;
  padding: 8px;
  font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
  font-size: var(--fs-sm);
  line-height: 1.5;
  white-space: pre-wrap;
  word-break: break-all;
  background: var(--surface-2);
  border-radius: var(--r-2);
}

.codes {
  margin: 8px 0;
  padding-left: 20px;
  font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
}

.full {
  width: 100%;
  margin-top: 8px;
}
</style>
