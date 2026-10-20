import { fileURLToPath, URL } from 'node:url'

import vue from '@vitejs/plugin-vue'
import { defineConfig } from 'vitest/config'

// 单元测试配置。
//
// 环境固定为 node：当前冻结的依赖里没有 jsdom / happy-dom，因此组件测试用
// vue/server-renderer 渲染成字符串断言（见 src/components/*.test.ts），
// 交互式 DOM 测试要等补装 DOM 环境并另记决策（DECISIONS Q21）。
export default defineConfig({
  plugins: [vue()],
  resolve: {
    alias: {
      '@': fileURLToPath(new URL('./src', import.meta.url)),
    },
  },
  test: {
    environment: 'node',
    include: ['src/**/*.test.ts'],
    reporters: ['default'],
  },
})
