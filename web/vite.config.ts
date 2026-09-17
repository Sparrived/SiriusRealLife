import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

// 构建产物由 Go 侧当静态目录提供（SIRIUS_STATIC_DIR=web/dist）。
// base 用相对路径，这样挂在前缀下也能工作。
export default defineConfig({
  plugins: [vue()],
  base: './',
  server: {
    port: 5173,
    // 开发时把 API 与 SSE 转发给本机 Go 服务，避免前端写死后端地址。
    proxy: {
      '/api': { target: 'http://127.0.0.1:8080', changeOrigin: true },
      // SSE 必须关掉缓冲，否则事件会被攒着不发。
      '/amkr': { target: 'http://127.0.0.1:8080', changeOrigin: true },
    },
  },
  build: {
    outDir: 'dist',
    emptyOutDir: true,
    // 观测面板不需要兼容很老的浏览器。
    target: 'es2022',
  },
})
