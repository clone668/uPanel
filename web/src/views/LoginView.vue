<script setup lang="ts">
// 登录页（CONTRACTS.md 第 2 节 / P14.6）。
//
// 界面上只做三件事：收用户名密码、调用 /auth/login、失败时把真实的错误码摆出来。
// 不显示“忘记密码”——本项目没有找回流程（PLAN.md 里改密码由管理员在账号管理里做），
// 摆一个点了没反应的链接比不摆更糟。
//
// 二次验证（P14.6）在这里是**第二步**，不是另一个页面：口令过了但没带码时服务端回
// 401 + details.totp_required，页面据此把验证码输入框亮出来，用户名与口令留在内存里
// 等用户补一个码。口令不因此失效，也**没有**任何半张票被下发。

import { ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { NButton, NCard, NForm, NFormItem, NInput } from 'naive-ui'

import { ApiError } from '@/api/client'
import { secondFactorRequired } from '@/api/totp'
import ApiErrorPanel from '@/components/ApiErrorPanel.vue'
import SecondFactorField from '@/components/SecondFactorField.vue'
import { isCodeComplete, sanitizeCode } from '@/components/secondfactor'
import { safeRedirect } from '@/router/redirect'
import { useSessionStore } from '@/stores/session'

const route = useRoute()
const router = useRouter()
const session = useSessionStore()

const username = ref('')
const password = ref('')
const code = ref('')
/** 服务端已经明确要验证码：此时才显示第二步的那一行。 */
const needsCode = ref(false)
const submitting = ref(false)
const restoring = ref(false)
const error = ref<ApiError | null>(null)

function submit(): Promise<void> {
  return run()
}

async function run(): Promise<void> {
  if (submitting.value || restoring.value) return
  const submitCode = needsCode.value
  if (username.value.trim() === '' || password.value === '') {
    error.value = null
    return
  }
  if (submitCode && !isCodeComplete(code.value)) {
    error.value = null
    return
  }
  submitting.value = true
  error.value = null
  try {
    await session.signIn(username.value.trim(), password.value, submitCode ? code.value : undefined)
    password.value = ''
    code.value = ''
    await router.replace(safeRedirect(route.query.redirect))
  } catch (caught) {
    if (caught instanceof ApiError) {
      error.value = caught
      // 码不对时保留验证码输入框（并把输入清掉），但**不清口令**：
      // 用户要补的是那个每 30 秒变一次的六位数，不是重新打一遍口令。
      if (secondFactorRequired(caught)) {
        needsCode.value = true
        code.value = ''
        return
      }
      password.value = ''
      needsCode.value = false
      code.value = ''
      return
    }
    throw caught
  } finally {
    submitting.value = false
  }
}

/** 退回第一步：换账号，或换口令重来。 */
function backToPassword(): void {
  needsCode.value = false
  code.value = ''
  error.value = null
}

function onCodeInput(next: string): void {
  code.value = sanitizeCode(next)
}

/** 初始会话恢复失败时，登录页给出真实错误和一条独立的重试路径。 */
async function retryRestore(): Promise<void> {
  if (restoring.value || submitting.value) return
  restoring.value = true
  try {
    await session.restore()
    if (session.isAuthenticated) {
      await router.replace(safeRedirect(route.query.redirect))
    }
  } catch {
    // restore 已把原始 ApiError 留在 store，模板会把它显示出来。
  } finally {
    restoring.value = false
  }
}
</script>

<template>
  <section class="login">
    <NCard class="login__card" title="登录 uPanel" size="small">
      <template #header-extra>
        <span class="login__hint">单台 Ubuntu 服务器管理面板</span>
      </template>

      <NForm @submit.prevent="submit">
        <NFormItem label="用户名">
          <NInput
            v-model:value="username"
            placeholder="管理员用户名"
            autocomplete="username"
            :disabled="submitting || restoring || needsCode"
            data-testid="login-username"
          />
        </NFormItem>
        <NFormItem label="密码">
          <NInput
            v-model:value="password"
            type="password"
            show-password-on="click"
            placeholder="密码"
            autocomplete="current-password"
            :disabled="submitting || restoring || needsCode"
            @keyup.enter="needsCode ? undefined : submit()"
            data-testid="login-password"
          />
        </NFormItem>

        <SecondFactorField
          v-if="needsCode"
          :model-value="code"
          :disabled="submitting || restoring"
          label="二次验证码"
          testid="login-totp"
          @update:model-value="onCodeInput"
        />

        <ApiErrorPanel
          v-if="session.restoreError"
          :error="session.restoreError"
          title="无法确认面板会话"
          :allow-retry="!restoring && !submitting"
          @retry="retryRestore"
        />
        <ApiErrorPanel v-if="error" :error="error" @retry="submit" />

        <NButton
          class="login__submit"
          type="primary"
          :loading="submitting"
          :disabled="
            restoring ||
            username.trim() === '' ||
            password === '' ||
            (needsCode && !isCodeComplete(code))
          "
          attr-type="submit"
          data-testid="login-submit"
        >
          {{ needsCode ? '验证并登录' : '登录' }}
        </NButton>

        <NButton
          v-if="needsCode"
          class="login__back"
          quaternary
          size="small"
          :disabled="submitting || restoring"
          data-testid="login-back"
          @click="backToPassword"
        >
          改用其他账号或口令
        </NButton>

        <p v-if="needsCode" class="login__note">
          口令已通过，还差手机上的六位数。这个码每 30 秒变一次，同一个码不能用第二次。
        </p>
        <p v-else class="login__note">
          没有默认密码。首个管理员由服务器上的管理员用 <code>upanelctl init-admin</code> 创建。
        </p>
      </NForm>
    </NCard>
  </section>
</template>

<style scoped>
/* 登录页是这台机器的"锁屏"：同一张桌布、点阵、中间一块纸面。
   它不套外壳（未登录时导航每一项都会跳回登录页），但观感要跟桌面接得上，
   否则一进门就是另一个产品。 */
.login {
  display: flex;
  align-items: center;
  justify-content: center;
  min-height: 100vh;
  min-height: 100dvh;
  padding: 24px 16px;
  background-color: var(--desk);
  background-image:
    radial-gradient(var(--desk-dot) 1px, transparent 1px),
    linear-gradient(180deg, var(--desk-top) 0%, rgba(233, 230, 221, 0) 46%);
  background-size:
    22px 22px,
    100% 100%;
}

.login__card {
  width: 100%;
  max-width: 420px;
  box-shadow: var(--shadow-win);
}

.login__hint {
  font-size: var(--fs-sm);
  opacity: 0.7;
}

.login__submit {
  width: 100%;
}

.login__back {
  width: 100%;
  margin-top: 8px;
}

.login__note {
  margin: 12px 0 0;
  font-size: var(--fs-sm);
  line-height: 1.6;
  opacity: 0.75;
}
</style>
