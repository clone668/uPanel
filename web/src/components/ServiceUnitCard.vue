<script setup lang="ts">
// 一个 systemd 单元的列表卡片（P05.1）。
//
// 列表页只给“够用来做判断”的信息：运行状态、开机启动、是否受保护、有没有失败。
// PID / 内存 / 启动时间是**快照**读数，可能缺失——缺失就写“未读到”，不补 0。
//
// 这里没有按钮：启停入口只放在详情页，而且只按服务端给的 capabilities 渲染。
// 列表里放按钮会让人在没看影响说明的情况下直接点下去。

import { computed } from 'vue'
import { NTag } from 'naive-ui'
import { RouterLink } from 'vue-router'

import {
  activeStateLabel,
  activeStateType,
  enablementLabel,
  loadStateLabel,
  loadStateType,
  needsAttention,
  protectionCategoryLabel,
  type ServiceUnit,
} from '@/api/services'
import { UNKNOWN_TEXT, bytesTitle, formatBytes, formatTimestamp } from '@/utils/format'

const props = defineProps<{ unit: ServiceUnit }>()

const detailLink = computed(() => `/services/${encodeURIComponent(props.unit.unit)}`)
const attention = computed(() => needsAttention(props.unit))
const memory = computed(() => formatBytes(props.unit.memory_bytes))
</script>

<template>
  <article class="service-card" :data-unit="unit.unit" :data-active="unit.active_state">
    <header class="service-card__head">
      <RouterLink class="service-card__name" :to="detailLink">{{ unit.unit }}</RouterLink>
      <!-- 标签整体靠右：名称左对齐、读数右对齐，换行时也是“右边那一簇继续换”。
           逐个散在标题行里时，名称后面会拖出一条空白，看起来像漏了什么。 -->
      <span class="service-card__tags">
        <NTag size="small" :type="activeStateType(unit.active_state)">
          {{ activeStateLabel(unit.active_state) }}
        </NTag>
        <NTag size="small" type="default">{{ enablementLabel(unit.enablement) }}</NTag>
        <NTag v-if="unit.protection.protected" size="small" type="warning">
          受保护：{{ protectionCategoryLabel(unit.protection.category) }}
        </NTag>
        <NTag v-if="unit.load_state !== 'loaded'" size="small" :type="loadStateType(unit.load_state)">
          {{ loadStateLabel(unit.load_state) }}
        </NTag>
        <NTag v-if="unit.template" size="small" type="info">模板单元</NTag>
        <NTag v-if="attention" size="small" type="error">需要注意</NTag>
      </span>
    </header>

    <p class="service-card__line">
      {{ unit.description === '' ? '（没有描述）' : unit.description }}
    </p>

    <p class="service-card__line service-card__muted">
      子状态：{{ unit.sub_state === '' ? UNKNOWN_TEXT : unit.sub_state }} · 进程号：
      {{ unit.main_pid ?? UNKNOWN_TEXT }} · 任务数：{{ unit.tasks_current ?? UNKNOWN_TEXT }} ·
      内存：<span :title="bytesTitle(unit.memory_bytes)">{{ memory }}</span>
    </p>

    <p class="service-card__line service-card__muted">
      启动于：{{ formatTimestamp(unit.started_at) }} · 重启次数：
      {{ unit.restart_count ?? UNKNOWN_TEXT }}
    </p>
  </article>
</template>

<style scoped>
.service-card {
  display: flex;
  flex-direction: column;
  gap: 6px;
  padding: 12px 14px;
  border: 1px solid var(--rule);
  border-radius: var(--r-2);
  background: var(--surface-2);
}

/* 名称一行、标签一行，都左对齐：并排时卡片宽度一变标签就换行，
   右对齐还会在名称后面留一大片空白。竖排是可预期的。 */
.service-card__head {
  display: flex;
  flex-direction: column;
  align-items: flex-start;
  gap: 5px;
}

.service-card__name {
  font-size: var(--fs-md);
  font-weight: 600;
  text-decoration: none;
  word-break: break-all;
  min-width: 0;
}

.service-card__tags {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 4px;
}

.service-card__line {
  margin: 0;
  line-height: 1.6;
  color: var(--ink-2);
}

.service-card__muted {
  font-size: var(--fs-sm);
  color: var(--ink-3);
}
</style>
