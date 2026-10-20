import { fileURLToPath, URL } from 'node:url'

import vue from '@vitejs/plugin-vue'
import { defineConfig } from 'vite'

// 构建配置：静态资源输出到 web/dist，由 P01.4 嵌入 Go Web 二进制（不要求生产机装 Node）。
// 生产与开发不共用 API 地址：两者都用同源 /api/v1（PLAN.md 第 10 节），
// 因此这里不配置跨域代理，避免开发期形成“跨域也能跑”的假象。
export default defineConfig({
  plugins: [vue()],
  resolve: {
    alias: {
      '@': fileURLToPath(new URL('./src', import.meta.url)),
    },
  },
  build: {
    outDir: 'dist',
    emptyOutDir: true,
    sourcemap: false,
    target: 'es2022',
  },
  server: {
    port: 5173,
    strictPort: true,
  },
  preview: {
    port: 4173,
    strictPort: true,
  },
})
