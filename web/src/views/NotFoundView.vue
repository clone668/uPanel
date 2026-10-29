<script setup lang="ts">
// 未知路径。
//
// 前端路由没登记的路径（包括尚未实现的模块）都到这里，明确说“没有这个页面”，
// 不用空白页或重定向首页掩盖（PLAN.md 第 7 节：不摆放无效入口）。
// 注意：这与 API 的 JSON 404 是两件事——未知 /api/v1 路径由服务端返回 JSON 404，
// 不会落到这个页面（P01.4 的服务端路由分离保证）。
//
// 图标换成排版出来的 "404"：naive 的 NResult 默认插图是一张 🤔 emoji，
// 在一套读数式的界面里像个玩笑。占位符要能被严肃读成“这里没有东西”。

import { useRouter } from 'vue-router'
import { NButton, NResult } from 'naive-ui'

const router = useRouter()
</script>

<template>
  <!-- 垂直居中：404 是“这一页什么都没有”，不该像正常页面那样把内容顶在最上面。 -->
  <div class="nf">
    <NResult title="页面不存在" description="该地址没有对应的页面；它可能属于尚未实现的模块。">
    <template #icon>
      <span class="nf__code u-num">404</span>
    </template>
    <template #footer>
      <NButton type="primary" @click="router.push('/')">返回概览</NButton>
    </template>
    </NResult>
  </div>
</template>

<style scoped>
.nf {
  min-height: 56vh;
  display: grid;
  place-items: center;
}

.nf__code {
  display: block;
  font-size: 44px;
  font-weight: 500;
  letter-spacing: 0.08em;
  color: var(--ink-3);
  line-height: 1;
}
</style>
