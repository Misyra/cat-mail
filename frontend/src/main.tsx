import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { BrowserRouter } from 'react-router'

import App from '@/App'
import { ApiError } from '@/lib/api'
import { applyTheme, readStoredTheme } from '@/lib/theme'
import '@/i18n'
import '@/index.css'

// 渲染前恢复主题（与 index.html 内联脚本双保险）；i18n 同步初始化（资源随包内联，无首屏语言闪烁）
applyTheme(readStoredTheme())

// 懒加载 chunk 加载失败（go:embed 单二进制发版后，旧缓存的 index.html 仍引用旧哈希资源 → 404）时
// Vite 会派发 vite:preloadError：阻止默认报错后自动整页重载，拉取新版 index.html 与 chunk。
// 每会话（sessionStorage）最多自动重载一次，防止坏网络下陷入无限刷新循环
window.addEventListener('vite:preloadError', (event) => {
  event.preventDefault()
  if (!sessionStorage.getItem('catmail-chunk-reload')) {
    sessionStorage.setItem('catmail-chunk-reload', '1')
    window.location.reload()
  }
})

const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      // 4xx 为确定性失败（参数/权限/不存在/未登录），重试无意义；
      // 网络错误（ApiError.status = 0）与 5xx 重试 1 次
      retry: (failureCount, error) => {
        if (error instanceof ApiError && error.status >= 400 && error.status < 500) return false
        return failureCount < 1
      },
      refetchOnWindowFocus: false,
    },
  },
})

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <QueryClientProvider client={queryClient}>
      <BrowserRouter>
        <App />
      </BrowserRouter>
    </QueryClientProvider>
  </StrictMode>,
)
