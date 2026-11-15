import { fileURLToPath, URL } from 'node:url'

import tailwindcss from '@tailwindcss/vite'
import react from '@vitejs/plugin-react'
import { defineConfig } from 'vite'

// https://vite.dev/config/
export default defineConfig({
  base: './',
  plugins: [react(), tailwindcss()],
  resolve: {
    alias: {
      '@': fileURLToPath(new URL('./src', import.meta.url)),
    },
  },
  build: {
    // 产物交给后端 go:embed 打包（SPEC §1）
    outDir: '../backend/internal/web/dist',
    emptyOutDir: true,
    // vite 8 换用 rolldown 内核：rollupOptions 改名 rolldownOptions，
    // manualChunks 函数式弃用，改为 codeSplitting.groups（test 按模块完整 id 匹配，
    // 且默认 includeDependenciesRecursively 会把匹配模块的传递依赖一并捕获）。
    rolldownOptions: {
      output: {
        // 拆分大依赖，避免单 chunk 超过 500kB：图表库与 react 全家桶各自独立。
        // recharts 及其传递依赖只随 Dashboard 路由 chunk 懒加载（App.tsx React.lazy）。
        codeSplitting: {
          groups: [
            // recharts 全家桶（recharts 3：状态管理换 redux 系，动画内置移除 react-smooth；
            // 传递依赖：victory-vendor/d3 系列、es-toolkit、@reduxjs/toolkit、react-redux、
            // redux、redux-thunk、reselect、immer、clsx 等）
            {
              name: 'recharts',
              test: /[\\/]node_modules[\\/](recharts|victory-vendor|d3-[a-z0-9-]+|internmap|decimal\.js-light|eventemitter3|tiny-invariant|clsx|es-toolkit|react-is|use-sync-external-store|react-redux|@reduxjs[\\/]toolkit|redux|redux-thunk|reselect|immer)[\\/]/,
            },
            // react 全家桶：react-dom 实现与其依赖 scheduler 一并归入，防止被 recharts 组
            // 递归捕获（react 是 recharts 的 peer dep）。priority 需高于 recharts 组（默认 0）。
            {
              name: 'react-vendor',
              priority: 1,
              test: /[\\/]node_modules[\\/](react|react-dom|react-router|scheduler|@tanstack[\\/]react-query)[\\/]/,
            },
          ],
        },
      },
    },
  },
  server: {
    proxy: {
      '/api': { target: 'http://localhost:8080', changeOrigin: true },
      '/v1': { target: 'http://localhost:8080', changeOrigin: true },
    },
  },
})
