import { createApp } from 'vue'

import DesignSystem from '@/design/DesignSystem.vue'
import '@/styles/global.css'

// 设计系统展示页的入口（仅 dev / 手动构建，不进生产 dist）：
//   npm --prefix web run dev -- --open /design.html
createApp(DesignSystem).mount('#app')
