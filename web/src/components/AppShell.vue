<script setup lang="ts">
// 应用外壳：品牌、固定导航、内容区、当前账号与退出。
//
// 导航项来自 modules/registry.ts 的 visibleModules()：未实现模块不渲染，
// 因此界面上不会出现“能点但什么都没发生”的入口（PLAN.md 第 7 节）。
//
// 登录页不套外壳（它没有导航，也没有可退出的会话），由 App.vue 的 layout 开关决定。

import { computed } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { NButton, NLayout, NLayoutContent, NLayoutHeader, NMenu, NTag } from 'naive-ui'
import type { MenuOption } from 'naive-ui'

import { visibleModules } from '@/modules/registry'
import { useSessionStore } from '@/stores/session'

const route = useRoute()
const router = useRouter()
const session = useSessionStore()

const menuOptions = computed<MenuOption[]>(() =>
  visibleModules().map((module) => ({
    label: module.label,
    key: module.path,
  })),
)

// 只有会出现在导航里的路径才高亮；未知路径（404 页面）不高亮任何项。
const activeKey = computed<string | null>(() => {
  const hit = visibleModules().find((module) => module.path === route.path)
  return hit ? hit.path : null
})

const roleLabel = computed(() =>
  session.role === 'admin' ? '管理员' : session.role === 'viewer' ? '只读账号' : '',
)

function onNavigate(key: string): void {
  void router.push(key)
}

async function signOut(): Promise<void> {
  await session.signOut()
  await router.replace('/login')
}
</script>

<template>
  <NLayout class="shell" position="absolute">
    <NLayoutHeader class="shell__header" bordered>
      <div class="shell__brand">
        <span class="shell__title">uPanel</span>
        <span class="shell__subtitle">单台 Ubuntu 服务器管理面板</span>
      </div>
      <NMenu
        mode="horizontal"
        responsive
        :options="menuOptions"
        :value="activeKey"
        @update:value="onNavigate"
      />
      <div v-if="session.isAuthenticated" class="shell__account">
        <span class="shell__user">{{ session.user?.display_name || session.user?.username }}</span>
        <NTag size="small" :type="session.isAdmin ? 'success' : 'info'">{{ roleLabel }}</NTag>
        <NButton size="tiny" quaternary @click="signOut">退出</NButton>
      </div>
    </NLayoutHeader>

    <NLayoutContent class="shell__content" content-style="padding: 20px 24px;">
      <slot />
    </NLayoutContent>
  </NLayout>
</template>

<style scoped>
.shell {
  display: flex;
  flex-direction: column;
  min-height: 100vh;
}

.shell__header {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 8px 24px;
  padding: 10px 24px;
}

.shell__brand {
  display: flex;
  align-items: baseline;
  gap: 10px;
}

.shell__title {
  font-size: 18px;
  font-weight: 700;
  letter-spacing: 0.02em;
}

.shell__subtitle {
  font-size: 12px;
  opacity: 0.7;
}

.shell__content {
  flex: 1;
}

.shell__account {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-left: auto;
}

.shell__user {
  font-size: 13px;
  opacity: 0.85;
}
</style>
