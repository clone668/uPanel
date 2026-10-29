<script setup lang="ts">
// 设置页（P14.6 开出第一块：本机账号的二次验证）。
//
// 设置这一项在 PLAN 的 16 项导航里列在最末，它的第一块内容就是 P14.6 要求
// "Web 端有启用确认与恢复码展示"的落点：把 `POST /auth/totp/setup`、
// `/confirm`、`DELETE /auth/totp` 三条接口接成一个能走完的流程。
//
// 这一页刻意很薄：状态与动作全在 TotpSettingsCard 里，页面只负责两件面板级的事——
//   1. 把 `enabled` 从会话状态里取出来（`/auth/session` 的 `totp_enabled`）；
//   2. 启用/关闭之后重新拉一次会话，因为"这个账号有没有二次验证"这件事
//      已经变了，界面上其它地方（再认证表单要不要验证码）都读同一个值。

import { computed, ref } from 'vue'
import { NAlert, NCard } from 'naive-ui'
import { RouterLink } from 'vue-router'

import TotpSettingsCard from '@/components/TotpSettingsCard.vue'
import { useSessionStore } from '@/stores/session'

const session = useSessionStore()

const refreshError = ref<string | null>(null)
const refreshing = ref(false)

const roleHint = computed(() =>
  session.isAdmin
    ? '当前账号是管理员：二次验证保护的是本机面板的登录与再认证。'
    : '当前账号是只读账号：二次验证只影响你自己的登录；账号类操作本来就不对你开放。',
)

/** 启用或关闭之后重新读一次会话，让界面上的 totp_enabled 与库里一致。 */
async function onEnabledChange(): Promise<void> {
  refreshError.value = null
  refreshing.value = true
  try {
    await session.restore()
  } catch {
    // 读不回来不影响刚才那一步已经生效的事实；如实说，并让人手动刷新。
    refreshError.value = '状态已变更，但重新读取会话失败。刷新页面即可看到最新状态。'
  } finally {
    refreshing.value = false
  }
}
</script>

<template>
  <section class="settings">
    <!-- 页名就是页名：不再拿一张叫“设置”的卡片当页头，那会让页面看起来没有标题。 -->
    <header class="settings__head">
      <h1 class="settings__title">设置</h1>
      <!-- 两条说明包一层再放进页头：页头是 flex 行，直接放 <p> 会被 max-width
           夹住后并排贴在标题右边；包一层让它整行占满、从标题下面开始。 -->
      <div class="settings__lead">
        <p class="settings__meta">
          目前只有本机账号安全这一块。系统级设置（备份、通知、更新策略等）随各自阶段接入。
        </p>
        <p class="settings__meta">{{ roleHint }}</p>
      </div>
    </header>

    <NAlert v-if="refreshError" type="warning" :bordered="false" data-testid="settings-refresh-error">
      {{ refreshError }}
    </NAlert>

    <div class="settings__grid">
      <NCard title="这台机器能做什么" size="small" data-testid="settings-host-caps-entry">
        <p class="settings__meta">
          外部程序能力页列出这台机器上装了哪些外部程序、什么版本、在哪，以及每一项缺席时
          是“没装”还是“说不清”。它只转述事实，不做判决。
        </p>
        <p v-if="session.isAdmin" class="settings__meta">
          <RouterLink to="/settings/host">打开外部程序能力</RouterLink>
        </p>
        <p v-else class="settings__meta">
          这一页只对管理员开放：这份清单合起来就是主机指纹。
        </p>
      </NCard>

      <TotpSettingsCard
        :enabled="session.totpEnabled"
        :data-refreshing="refreshing ? 'true' : undefined"
        @enabled-change="onEnabledChange"
      />
    </div>
  </section>
</template>

<style scoped>
.settings__lead {
  flex: 1 0 100%;
  display: flex;
  flex-direction: column;
  gap: 2px;
  min-width: 0;
}

/* 两列：左边“这台机器能做什么”，右边二次验证。窄屏落成一列。
   原来是单列 760px，右侧空出一大片，看起来像还没渲染完。 */
.settings__grid {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(360px, 1fr));
  gap: var(--sp-3);
  align-items: start;
}
</style>
