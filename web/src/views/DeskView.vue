<script setup lang="ts">
// 桌面本身（/desk）。
//
// 这一页**故意什么都不画**：桌面上的东西——壁纸、图标、程序坞——都长在
// AppShell 上，它们是外壳，不是"一页内容"。这里存在只为一件事：让 URL 与屏幕一致。
//
// 为什么需要一个真实路由而不是外壳里存一个布尔开关：
//   关窗之后屏幕上是桌面，地址栏却还写着 /services，那是撒谎。刷新一下还会
//   把刚关掉的那扇窗又打开。所以"桌面"必须是路由表里的一条：
//     · 登录后落到 /desk；
//     · 窗上的关闭按钮 router.push('/desk')；
//     · 图标再点一下又回到那个模块的路由。
//   这样前进/后退、刷新、收藏都跟屏幕上看到的一致。
//
// 它不需要登录态之外的数据：不发起任何请求，也就不会有一页空转的骨架屏。
</script>

<template>
  <div class="desk-view" aria-hidden="true"></div>
</template>

<style scoped>
.desk-view {
  display: none;
}
</style>
