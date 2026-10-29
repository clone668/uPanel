<script setup lang="ts">
// 桌面外壳：壁纸 → 桌面图标 → 一扇窗口 → 程序坞 → 启动台。
//
// 这一版把「像 NAS 桌面」这件事落到底：登录后落在 /desk，看到的是壁纸和图标；
// 点一个图标，那个模块在一扇**有标题栏、能拖、能缩放、能关**的窗口里打开；
// 关掉窗口回到桌面。以前那版只是一张铺满屏幕的卡片，那是网页，不是桌面。
//
// 一扇窗而不是多扇（Q524 / Q533）：
//   内容由 vue-router 的一份 <RouterView> 渲染，同一时刻只能挂一个页面；23 个视图
//   都在用 useRoute()/useRouter()，要做真多窗口得给每扇窗塞一套伪路由上下文——
//   那是改视图契约。所以窗口上的三个点只做**真的能发生的事**：关闭回桌面、
//   最大化铺满、还原。最小化不画：它和关闭在单窗模型里没有区别，画出来就是假交互。
//
// 桌面图标常驻在最底层：窗口缩到一半时它们仍在四边露出来，点一下就能换应用。
// 窗口开着时点图标 = 换页（同一扇窗换内容），不是再开一扇。

import { computed, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'

import AppDock from './AppDock.vue'
import AppLauncher from './AppLauncher.vue'
import DeskIcons from './DeskIcons.vue'
import DeskWindow from './DeskWindow.vue'
import DesktopBar from './DesktopBar.vue'
import { resolveActiveModule } from './nav'
import { HOME_PATH } from '@/router/redirect'

const route = useRoute()
const router = useRouter()

const launcherOpen = ref(false)

const active = computed(() => resolveActiveModule(route.path, route.meta.moduleKey))
const activeKey = computed(() => active.value?.key ?? null)
const activePath = computed(() => active.value?.path ?? null)
const title = computed(() => active.value?.label ?? '未找到页面')

// 桌面上没有窗口：/desk 就是"什么都没打开"。
const onDesk = computed(() => route.name === 'desk')

function go(path: string): void {
  launcherOpen.value = false
  // 同一个路径重复点：vue-router 会返回一次失败的解析但不抛错，不需要额外判断。
  void router.push(path)
}

// 关窗 = 回桌面。不是把窗口藏起来——藏起来会让地址栏停在一个已经关掉的页面上。
function closeWindow(): void {
  go(HOME_PATH)
}
</script>

<template>
  <div class="desk" :data-on-desk="onDesk ? 'true' : 'false'">
    <div class="desk__wall" aria-hidden="true"></div>

    <DesktopBar
      :active-key="activeKey"
      :title="title"
      :on-desk="onDesk"
      @launcher="launcherOpen = true"
    />

    <DeskIcons :active-path="onDesk ? null : activePath" @open="go" />

    <DeskWindow
      v-if="!onDesk"
      :title="title"
      :icon-key="activeKey"
      @close="closeWindow"
    >
      <RouterView />
    </DeskWindow>

    <AppDock :active-path="activePath" @select="go" @launcher="launcherOpen = true" />

    <AppLauncher
      :open="launcherOpen"
      :active-path="activePath"
      @close="launcherOpen = false"
      @select="go"
    />
  </div>
</template>

<style scoped>
.desk {
  position: relative;
  min-height: 100vh;
  min-height: 100dvh;
  overflow: hidden;
  background-color: var(--desk);
}

/* 壁纸：点阵 + 顶部一抹更亮的光，让桌面有纵深；固定住不跟着内容滚 */
.desk__wall {
  position: fixed;
  inset: 0;
  z-index: 0;
  pointer-events: none;
  background-color: var(--desk);
  background-image:
    radial-gradient(var(--desk-dot) 1px, transparent 1px),
    linear-gradient(180deg, var(--desk-top) 0%, rgba(233, 230, 221, 0) 46%);
  background-size:
    22px 22px,
    100% 100%;
}

/* 桌面态：底部别把程序坞压住 */
.desk[data-on-desk='true'] {
  min-height: 100dvh;
}
</style>
