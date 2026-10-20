<script setup lang="ts">
// 未知路径。
//
// 前端路由没登记的路径（包括尚未实现的模块）都到这里，明确说“没有这个页面”，
// 不用空白页或重定向首页掩盖（PLAN.md 第 7 节：不摆放无效入口）。
// 注意：这与 API 的 JSON 404 是两件事——未知 /api/v1 路径由服务端返回 JSON 404，
// 不会落到这个页面（P01.4 的服务端路由分离保证）。

import { useRouter } from 'vue-router'
import { NButton, NResult } from 'naive-ui'

const router = useRouter()
</script>

<template>
  <NResult status="404" title="页面不存在" description="该地址没有对应的页面；它可能属于尚未实现的模块。">
    <template #footer>
      <NButton secondary type="primary" @click="router.push('/')">返回概览</NButton>
    </template>
  </NResult>
</template>
