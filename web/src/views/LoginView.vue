<script setup lang="ts">
// 登录页（CONTRACTS.md 第 2 节）。
//
// 界面上只做三件事：收用户名密码、调用 /auth/login、失败时把真实的错误码摆出来。
// 不显示“忘记密码”——本项目没有找回流程（PLAN.md 里改密码由管理员在账号管理里做），
// 摆一个点了没反应的链接比不摆更糟。

import { ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { NButton, NCard, NForm, NFormItem, NInput } from 'naive-ui'

import { ApiError } from '@/api/client'
import ApiErrorPanel from '@/components/ApiErrorPanel.vue'
import { safeRedirect } from '@/router/redirect'
import { useSessionStore } from '@/stores/session'

const route = useRoute()
const router = useRouter()
const session = useSessionStore()

const username = ref('')
const password = ref('')
const submitting = ref(false)
const error = ref<ApiError | null>(null)

async function submit(): Promise<void> {
  if (submitting.value) return
  if (username.value.trim() === '' || password.value === '') {
    error.value = null
    return
  }
  submitting.value = true
  error.value = null
  try {
    await session.signIn(username.value.trim(), password.value)
    password.value = ''
    await router.replace(safeRedirect(route.query.redirect))
  } catch (caught) {
    if (caught instanceof ApiError) {
      error.value = caught
      password.value = ''
      return
    }
    throw caught
  } finally {
    submitting.value = false
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
            :disabled="submitting"
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
            :disabled="submitting"
            @keyup.enter="submit"
            data-testid="login-password"
          />
        </NFormItem>

        <ApiErrorPanel v-if="error" :error="error" @retry="submit" />

        <NButton
          class="login__submit"
          type="primary"
          :loading="submitting"
          :disabled="username.trim() === '' || password === ''"
          attr-type="submit"
          data-testid="login-submit"
        >
          登录
        </NButton>
        <p class="login__note">
          没有默认密码。首个管理员由服务器上的管理员用 <code>upanelctl init-admin</code> 创建。
        </p>
      </NForm>
    </NCard>
  </section>
</template>

<style scoped>
.login {
  display: flex;
  justify-content: center;
  padding-top: 8vh;
}

.login__card {
  width: 100%;
  max-width: 420px;
}

.login__hint {
  font-size: 12px;
  opacity: 0.7;
}

.login__submit {
  width: 100%;
}

.login__note {
  margin: 12px 0 0;
  font-size: 12px;
  line-height: 1.6;
  opacity: 0.75;
}
</style>
