import { defineConfig } from 'vitest/config'
import react from '@vitejs/plugin-react'

// Vite 配置统一开发代理、生产构建和浏览器边界单元测试。
export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173,
    strictPort: true,
    proxy: {
      '/health': 'http://127.0.0.1:8080',
    },
  },
  test: {
    environment: 'jsdom',
    setupFiles: './src/test/setup.ts',
    css: true,
  },
})
