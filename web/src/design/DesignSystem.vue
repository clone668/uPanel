<script setup lang="ts">
// 设计系统的活页面：dev 下打开 /design.html 就能看全所有原子件与两套主题。
//
// 为什么放在 web/ 而不是 docs/：这里的每一块都是**真组件**（src/ui/*.vue），
// 改令牌或组件，这个页面立刻跟着变；docs 里的原型图不会。它既是验收台，
// 也是以后加新组件时的第一站。
//
// 不参与生产构建：vite.config.ts 的 build.input 只列 index.html。

import { computed } from 'vue'
import { NConfigProvider } from 'naive-ui'

import UBar from '@/ui/UBar.vue'
import UButton from '@/ui/UButton.vue'
import UKpi from '@/ui/UKpi.vue'
import UPanel from '@/ui/UPanel.vue'
import UState from '@/ui/UState.vue'
import UTag from '@/ui/UTag.vue'
import { PALETTE, useTheme } from '@/ui/theme'

const theme = useTheme()

const modeLabel = computed(
  () => ({ auto: '跟随系统', light: '纸面', dark: '夜间' })[theme.mode.value],
)

const SPACES = [
  { name: '--sp-1', px: 4 },
  { name: '--sp-2', px: 8 },
  { name: '--sp-3', px: 12 },
  { name: '--sp-4', px: 16 },
  { name: '--sp-5', px: 20 },
  { name: '--sp-6', px: 24 },
  { name: '--sp-8', px: 32 },
]

const swatches = computed(() => {
  const c = PALETTE[theme.resolved.value]
  return [
    { name: 'desk', value: c.desk, note: '桌面/页面底' },
    { name: 'surface', value: c.surface, note: '窗口与面板' },
    { name: 'surface2', value: c.surface2, note: '表头/次层' },
    { name: 'ink', value: c.ink, note: '正文' },
    { name: 'ink-2', value: c.ink2, note: '次要' },
    { name: 'ink-3', value: c.ink3, note: '辅助' },
    { name: 'rule', value: c.rule, note: '发丝线' },
    { name: 'rule-strong', value: c.ruleStrong, note: '控件边框' },
    { name: 'accent', value: c.accent, note: '强调（唯一）' },
    { name: 'good', value: c.good, note: '正常' },
    { name: 'warn', value: c.warn, note: '警告' },
    { name: 'bad', value: c.bad, note: '危险' },
    { name: 'unknown', value: c.unknown, note: '未知' },
  ]
})
</script>

<template>
  <NConfigProvider :theme="theme.naiveTheme.value" :theme-overrides="theme.overrides.value">
    <div class="ds">
      <header class="ds__bar">
        <strong class="ds__brand">uPanel 设计系统</strong>
        <span class="ds__meta u-mono">tokens · primitives · v0</span>
        <span class="ds__spacer"></span>
        <span class="ds__meta">主题</span>
        <UButton size="sm" variant="quiet" @click="theme.cycle()">{{ modeLabel }}</UButton>
      </header>

      <main class="ds__body">
        <!-- 色板 -->
        <section class="ds__section">
          <h2 class="ds__h2">颜色</h2>
          <p class="ds__note">
            两种材料：桌面（外壳）与纸面（窗口）。强调色只有一种，和状态色分开——蓝不表示“正常”。
          </p>
          <ul class="ds__swatches">
            <li v-for="s in swatches" :key="s.name" class="ds__swatch">
              <span class="ds__chip" :style="{ background: s.value }"></span>
              <code class="ds__code">--{{ s.name }}</code>
              <span class="ds__meta u-mono">{{ s.value }} · {{ s.note }}</span>
            </li>
          </ul>
        </section>

        <!-- 字与数 -->
        <section class="ds__section">
          <h2 class="ds__h2">字与数字</h2>
          <p class="ds__note">
            界面用系统无衬线；数字和机器值一律等宽 + tabular-nums，轮询刷新时列宽不跳。
          </p>
          <table class="u-table ds__type">
            <thead>
              <tr>
                <th style="width: 96px">令牌</th>
                <th style="width: 64px">字号</th>
                <th>用在哪</th>
              </tr>
            </thead>
            <tbody>
              <tr>
                <td><code>--fs-2xl</code></td>
                <td class="u-num">28</td>
                <td><span style="font-size: var(--fs-2xl); letter-spacing: -0.025em">指标数值 12.4%</span></td>
              </tr>
              <tr>
                <td><code>--fs-xl</code></td>
                <td class="u-num">20</td>
                <td><span style="font-size: var(--fs-xl)">页面标题</span></td>
              </tr>
              <tr>
                <td><code>--fs-lg</code></td>
                <td class="u-num">15</td>
                <td><span style="font-size: var(--fs-lg)">区块标题</span></td>
              </tr>
              <tr>
                <td><code>--fs-md</code></td>
                <td class="u-num">13</td>
                <td><span style="font-size: var(--fs-md)">正文，表格单元格</span></td>
              </tr>
              <tr>
                <td><code>--fs-sm</code></td>
                <td class="u-num">12</td>
                <td><span style="font-size: var(--fs-sm); color: var(--ink-2)">说明、状态词、按钮小号</span></td>
              </tr>
              <tr>
                <td><code>--fs-xs</code></td>
                <td class="u-num">11</td>
                <td>
                  <span style="font-size: var(--fs-xs); color: var(--ink-3); letter-spacing: 0.08em">
                    表头、标签、脚注
                  </span>
                </td>
              </tr>
            </tbody>
          </table>
          <div class="ds__row ds__row--nums">
            <span class="u-num">12.4%</span>
            <span class="u-num">1.83 / 3.66 GiB</span>
            <span class="u-num">0.34</span>
            <span class="u-mono">dm-0 · ext4 · sda2</span>
            <span class="u-mono">SYSTEM_DISK_PROTECTED</span>
          </div>
        </section>

        <!-- 尺度 -->
        <section class="ds__section">
          <h2 class="ds__h2">尺度</h2>
          <p class="ds__note">
            间距只有 4 的倍数（4/8/12/16/20/24/32），不发明 10px 这种半格；圆角只三档，越大越少见。
          </p>
          <ul class="ds__spaces">
            <li v-for="step in SPACES" :key="step.name" class="ds__space">
              <span class="u-mono ds__meta">{{ step.name }}</span>
              <span class="ds__space-bar" :style="{ width: `${step.px * 3}px` }"></span>
              <span class="u-num ds__meta">{{ step.px }}px</span>
            </li>
          </ul>
          <div class="ds__row">
            <span v-for="r in [1, 2, 3]" :key="r" class="ds__radius" :style="{ borderRadius: `var(--r-${r})` }">
              <span class="u-mono ds__meta">--r-{{ r }}</span>
            </span>
          </div>
        </section>

        <!-- 按钮 -->
        <section class="ds__section">
          <h2 class="ds__h2">按钮</h2>
          <p class="ds__note">
            四种含义：主线（primary）、并列（secondary）、退后（quiet）、破坏（danger）。禁用仍然可聚焦、可读清。
          </p>
          <div class="ds__row">
            <UButton variant="primary">初始化 sdb</UButton>
            <UButton variant="secondary">重新采集</UButton>
            <UButton variant="quiet">刷新</UButton>
            <UButton variant="danger">格式化</UButton>
          </div>
          <div class="ds__row">
            <UButton size="sm" variant="primary">小号主线</UButton>
            <UButton size="sm" variant="secondary">小号并列</UButton>
            <UButton variant="primary" disabled>不可用</UButton>
            <UButton variant="secondary" loading>提交中</UButton>
          </div>
        </section>

        <!-- 状态与标签 -->
        <section class="ds__section">
          <h2 class="ds__h2">状态与标签</h2>
          <p class="ds__note">
            状态永远是“方块 + 词”；标签只说“是什么”。未知有自己的一格，不借用绿色。
          </p>
          <div class="ds__row">
            <UState tone="good">运行中</UState>
            <UState tone="warn">证书待续</UState>
            <UState tone="bad">退出码 1</UState>
            <UState tone="unknown">未采集</UState>
          </div>
          <div class="ds__row">
            <UTag>ext4</UTag>
            <UTag>LVM2_member</UTag>
            <UTag tone="accent">LVM LV</UTag>
            <UTag tone="warn" :mono="false">受保护</UTag>
            <UTag tone="good" :mono="false">可写</UTag>
            <UTag tone="bad">SYSTEM_DISK_PROTECTED</UTag>
          </div>
        </section>

        <!-- 指标格与容量条 -->
        <section class="ds__section">
          <h2 class="ds__h2">指标格</h2>
          <p class="ds__note">
            数值由调用方格式化（null 早就变成 —），组件不猜数。容量条在“没读到”时不画长度。
          </p>
          <div class="ds__kpis">
            <UKpi label="CPU 使用率" value="12.4" unit="%">
              <template #foot>4 核 · 窗口 2s</template>
            </UKpi>
            <UKpi label="内存" value="1.83" unit="/ 3.66 GiB">
              <template #foot>可用 1.42 GiB</template>
              <template #extra><UBar :percent="50" style="width: 56px" /></template>
            </UKpi>
            <UKpi label="根分区 /" value="62" unit="%" tone="warn">
              <template #foot>12.4 / 20.0 GiB</template>
              <template #extra><UBar :percent="62" style="width: 56px" /></template>
            </UKpi>
            <UKpi label="容量" value="—">
              <template #foot>没读到（不是 0）</template>
              <template #extra><UBar :percent="null" style="width: 56px" /></template>
            </UKpi>
          </div>
        </section>

        <!-- 面板与表格 -->
        <section class="ds__section">
          <h2 class="ds__h2">面板与表格</h2>
          <div class="ds__grid2">
            <UPanel title="磁盘" meta="5 个设备" flush>
              <div class="u-table-wrap">
                <table class="u-table">
                  <thead>
                    <tr>
                      <th>设备</th>
                      <th>类型</th>
                      <th class="u-num">容量</th>
                      <th>状态</th>
                    </tr>
                  </thead>
                  <tbody>
                    <tr>
                      <td class="u-mono">sda</td>
                      <td>整盘</td>
                      <td class="u-num">20.0 GiB</td>
                      <td><UTag tone="warn" :mono="false">受保护</UTag></td>
                    </tr>
                    <tr>
                      <td class="u-mono">sda2</td>
                      <td>分区</td>
                      <td class="u-num">20.0 GiB</td>
                      <td><UTag tone="warn" :mono="false">受保护</UTag></td>
                    </tr>
                    <tr>
                      <td class="u-mono">sdb</td>
                      <td>整盘</td>
                      <td class="u-num">4.0 GiB</td>
                      <td><UState tone="good">可写</UState></td>
                    </tr>
                  </tbody>
                </table>
              </div>
            </UPanel>

            <UPanel title="正在读取" meta="骨架" loading>
              <p></p>
            </UPanel>
          </div>
        </section>

        <footer class="ds__foot">
          <span class="ds__meta u-mono">
            令牌：src/styles/tokens.css · 组件：src/ui/ · 本页：src/design/DesignSystem.vue
          </span>
        </footer>
      </main>
    </div>
  </NConfigProvider>
</template>

<style scoped>
.ds {
  min-height: 100vh;
  background: var(--desk);
  color: var(--ink);
}

.ds__bar {
  display: flex;
  align-items: center;
  gap: var(--sp-3);
  height: 44px;
  padding: 0 var(--sp-5);
  background: var(--bar);
  border-bottom: 1px solid var(--rule);
  position: sticky;
  top: 0;
  z-index: var(--z-bar);
}

.ds__brand {
  font-size: var(--fs-md);
}

.ds__meta {
  font-size: var(--fs-xs);
  color: var(--ink-3);
}

.ds__spacer {
  flex: 1;
}

.ds__body {
  max-width: 1080px;
  margin: 0 auto;
  margin: var(--sp-5) auto var(--sp-8);
  padding: var(--sp-6) var(--sp-6) var(--sp-8);
  background: var(--surface);
  border: 1px solid var(--rule);
  border-radius: var(--r-3);
  display: flex;
  flex-direction: column;
  gap: var(--sp-8);
}

.ds__section {
  display: flex;
  flex-direction: column;
  gap: var(--sp-3);
  border-top: 1px solid var(--rule);
  padding-top: var(--sp-4);
}

.ds__h2 {
  margin: 0;
  font-size: var(--fs-lg);
}

.ds__note {
  margin: 0;
  font-size: var(--fs-sm);
  color: var(--ink-2);
  max-width: 68ch;
}

.ds__swatches {
  list-style: none;
  margin: 0;
  padding: 0;
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(190px, 1fr));
  gap: var(--sp-2) var(--sp-3);
}

.ds__swatch {
  display: grid;
  grid-template-columns: 26px minmax(0, 1fr);
  grid-template-rows: auto auto;
  align-items: center;
  gap: 0 var(--sp-2);
}

.ds__chip {
  grid-row: span 2;
  width: 26px;
  height: 26px;
  border: 1px solid var(--rule-strong);
  border-radius: var(--r-2);
}

.ds__code {
  font-size: var(--fs-sm);
}

.ds__row {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: var(--sp-3);
}

.ds__row--nums {
  gap: var(--sp-4);
  color: var(--ink-2);
  font-size: var(--fs-md);
}

.ds__t {
  margin: 0;
}

.ds__type code,
.ds__swatch code {
  font-size: var(--fs-sm);
  color: var(--ink-2);
}

.ds__spaces {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: 5px;
}

.ds__space {
  display: flex;
  align-items: center;
  gap: var(--sp-3);
}

.ds__space span:first-child {
  width: 56px;
}

.ds__space span:last-child {
  width: 44px;
}

.ds__space-bar {
  height: 10px;
  border-radius: 1px;
  background: var(--accent-soft);
  border: 1px solid var(--accent);
}

.ds__radius {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 64px;
  height: 40px;
  border: 1px solid var(--rule-strong);
  background: var(--surface-2);
}

.ds__kpis {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(190px, 1fr));
  border: 1px solid var(--rule);
  border-radius: var(--r-2);
  background: var(--surface);
}

.ds__grid2 {
  display: grid;
  grid-template-columns: minmax(0, 1.3fr) minmax(0, 1fr);
  gap: var(--sp-4);
  align-items: start;
}

.ds__foot {
  border-top: 1px solid var(--rule);
  padding-top: var(--sp-3);
}

@media (max-width: 760px) {
  .ds__body {
    padding: var(--sp-4) var(--sp-4) var(--sp-6);
  }

  .ds__grid2 {
    grid-template-columns: minmax(0, 1fr);
  }
}
</style>
