<script setup lang="ts">
// 顶部面板条：品牌（点开应用抽屉）、当前所在模块、时钟、主题、账号与退出。
//
// 为什么不在这里显示"代理在线/主机名"：那要走 GET /system/summary，而 summary 是
// 一次**实时采样**（代理侧算 CPU 与内存），外壳再采一次就是同一台机器同一秒采两遍，
// 概览页自己已经有一份。外壳宁可少说一句，也不制造第二条读数来源。
//
// 时钟是本机时间，秒级刷新；只在客户端跑（SSR/单测里没有 setInterval 的问题由 onMounted 兜住）。

import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { useRouter } from 'vue-router'

import ModuleIcon from '@/ui/ModuleIcon.vue'
import { useTheme } from '@/ui/theme'
import { useSessionStore } from '@/stores/session'

const props = defineProps<{
  /** 当前模块的 key；未知页面（404）为 null，此时不显示图标。 */
  activeKey: string | null
  /** 当前模块中文名。 */
  title: string
  /** 是否停在桌面上（没有打开任何窗口）。桌面不显示模块名，显示"桌面"。 */
  onDesk?: boolean
}>()

const emit = defineEmits<{ launcher: [] }>()

const router = useRouter()
const session = useSessionStore()
const theme = useTheme()

const now = ref(new Date())
let timer: ReturnType<typeof setInterval> | undefined

const clock = computed(() => {
  const hh = String(now.value.getHours()).padStart(2, '0')
  const mm = String(now.value.getMinutes()).padStart(2, '0')
  const ss = String(now.value.getSeconds()).padStart(2, '0')
  return `${hh}:${mm}:${ss}`
})

// 悬停给完整日期：时钟只显示时分秒，但"今天几号"在排障时是常用信息。
const clockTitle = computed(() => {
  const d = now.value
  return `${d.getFullYear()} 年 ${d.getMonth() + 1} 月 ${d.getDate()} 日（本机时间）`
})

onMounted(() => {
  timer = setInterval(() => {
    now.value = new Date()
  }, 1000)
})

onBeforeUnmount(() => {
  if (timer !== undefined) clearInterval(timer)
})

const themeLabel = computed(() => {
  if (theme.mode.value === 'light') return '纸面'
  if (theme.mode.value === 'dark') return '夜间'
  return '跟随系统'
})

const roleLabel = computed(() =>
  session.role === 'admin' ? '管理员' : session.role === 'viewer' ? '只读账号' : '',
)

const userName = computed(() => session.user?.display_name || session.user?.username || '')

async function signOut(): Promise<void> {
  await session.signOut()
  await router.replace('/login')
}
</script>

<template>
  <header class="bar">
    <button
      class="bar__brand"
      type="button"
      title="打开全部应用"
      aria-label="打开全部应用"
      @click="emit('launcher')"
    >
      <span class="bar__mark" aria-hidden="true">u</span>
      <span class="bar__name">uPanel</span>
      <span class="bar__caret" aria-hidden="true"></span>
    </button>

    <div class="bar__crumb">
      <ModuleIcon v-if="props.activeKey !== null" :name="props.activeKey" :size="15" />
      <span class="bar__title">{{ props.onDesk ? '桌面' : props.title }}</span>
    </div>

    <div class="bar__right">
      <span class="bar__clock u-num" :title="clockTitle">{{ clock }}</span>

      <button
        class="bar__btn"
        type="button"
        :title="`切换主题（当前：${themeLabel}）`"
        @click="theme.cycle()"
      >
        主题<span class="bar__val">{{ themeLabel }}</span>
      </button>

      <span class="bar__sep" aria-hidden="true"></span>

      <span class="bar__user">{{ userName }}</span>
      <span v-if="roleLabel" class="bar__role">{{ roleLabel }}</span>

      <button class="bar__btn" type="button" @click="signOut">退出</button>
    </div>
  </header>
</template>

<style scoped>
.bar {
  position: sticky;
  top: 0;
  z-index: var(--z-bar);
  display: flex;
  align-items: center;
  gap: var(--sp-4);
  height: 44px;
  padding: 0 var(--sp-4);
  background: var(--bar);
  backdrop-filter: blur(10px);
  border-bottom: 1px solid var(--rule);
}

.bar__brand {
  display: inline-flex;
  align-items: center;
  gap: var(--sp-2);
  padding: 3px 6px 3px 3px;
  font: inherit;
  color: var(--ink);
  background: transparent;
  border: 1px solid transparent;
  border-radius: var(--r-2);
  cursor: pointer;
}

.bar__brand:hover {
  background: var(--surface-2);
  border-color: var(--rule);
}

.bar__mark {
  display: grid;
  place-items: center;
  width: 20px;
  height: 20px;
  font-family: var(--font-mono);
  font-size: var(--fs-sm);
  font-weight: 700;
  color: var(--accent-ink);
  background: var(--accent);
  border-radius: var(--r-2);
}

.bar__name {
  font-size: var(--fs-md);
  font-weight: 600;
  letter-spacing: 0.01em;
}

/* 小三角：告诉新手“这块能点开” */
.bar__caret {
  width: 0;
  height: 0;
  border-left: 3.5px solid transparent;
  border-right: 3.5px solid transparent;
  border-top: 4px solid var(--ink-3);
}

.bar__crumb {
  display: flex;
  align-items: center;
  gap: 6px;
  min-width: 0;
  color: var(--ink-2);
}

.bar__title {
  font-size: var(--fs-md);
  font-weight: 500;
  color: var(--ink);
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}

.bar__right {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
  margin-left: auto;
}

.bar__clock {
  font-size: var(--fs-sm);
  color: var(--ink-3);
}

.bar__btn {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  padding: 3px 7px;
  font: inherit;
  font-size: var(--fs-sm);
  color: var(--ink-2);
  background: transparent;
  border: 1px solid transparent;
  border-radius: var(--r-2);
  cursor: pointer;
}

.bar__btn:hover {
  color: var(--ink);
  background: var(--surface-2);
  border-color: var(--rule);
}

.bar__val {
  font-size: var(--fs-xs);
  color: var(--ink-3);
}

.bar__sep {
  width: 1px;
  height: 16px;
  background: var(--rule);
}

.bar__user {
  font-size: var(--fs-sm);
  color: var(--ink);
}

.bar__role {
  padding: 1px 5px;
  font-size: var(--fs-xs);
  font-family: var(--font-mono);
  color: var(--ink-3);
  border: 1px solid var(--rule);
  border-radius: var(--r-1);
}

@media (max-width: 760px) {
  .bar {
    gap: var(--sp-2);
    padding: 0 var(--sp-3);
  }

  /* 窄屏先砍时钟，再砍主题的当前值：账号与退出必须留着。 */
  .bar__clock {
    display: none;
  }

  .bar__val {
    display: none;
  }

  .bar__user,
  .bar__role {
    display: none;
  }
}
</style>
