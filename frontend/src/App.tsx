import { useQueryClient } from '@tanstack/react-query'
import { lazy, Suspense, useEffect, type ReactNode } from 'react'
import { Navigate, Route, Routes, useLocation, useNavigate } from 'react-router'
import { Toaster } from 'sonner'

import { ErrorBoundary } from '@/components/ErrorBoundary'
import { AppLayout } from '@/components/layout/AppLayout'
import { RequireAuth } from '@/components/RequireAuth'
import { PageSkeleton } from '@/components/ui/Skeleton'
import { setUnauthorizedHandler } from '@/lib/api'
import { useIsDarkMode } from '@/lib/theme'
import { LoginPage } from '@/pages/LoginPage'

// 登录后页面按路由分割（懒加载）；LoginPage 为首屏路径，保持同步打包
const DashboardPage = lazy(() => import('@/pages/DashboardPage').then((m) => ({ default: m.DashboardPage })))
const ChannelsPage = lazy(() => import('@/pages/ChannelsPage').then((m) => ({ default: m.ChannelsPage })))
const DomainsPage = lazy(() => import('@/pages/DomainsPage').then((m) => ({ default: m.DomainsPage })))
const TemplatesPage = lazy(() => import('@/pages/TemplatesPage').then((m) => ({ default: m.TemplatesPage })))
const LogsPage = lazy(() => import('@/pages/LogsPage').then((m) => ({ default: m.LogsPage })))
const EventsPage = lazy(() => import('@/pages/EventsPage').then((m) => ({ default: m.EventsPage })))
const SuppressionsPage = lazy(() =>
  import('@/pages/SuppressionsPage').then((m) => ({ default: m.SuppressionsPage })),
)
const KeysPage = lazy(() => import('@/pages/KeysPage').then((m) => ({ default: m.KeysPage })))
const UsersPage = lazy(() => import('@/pages/UsersPage').then((m) => ({ default: m.UsersPage })))
const AuditPage = lazy(() => import('@/pages/AuditPage').then((m) => ({ default: m.AuditPage })))
const SettingsPage = lazy(() => import('@/pages/SettingsPage').then((m) => ({ default: m.SettingsPage })))

/** 路由级 Suspense 兜底：骨架占位（与页面加载态同风格），布局/侧栏不受影响 */
function PageSuspense({ children }: { children: ReactNode }) {
  return <Suspense fallback={<PageSkeleton />}>{children}</Suspense>
}

export default function App() {
  const queryClient = useQueryClient()
  const navigate = useNavigate()
  const isDark = useIsDarkMode()
  const location = useLocation()

  // 任意管理 API 返回 401：清空缓存并回登录页
  // （/api/auth/login 与 /api/auth/me 除外，由页面自行处理）
  useEffect(() => {
    setUnauthorizedHandler(() => {
      queryClient.clear()
      navigate('/login', { replace: true })
    })
    return () => setUnauthorizedHandler(null)
  }, [queryClient, navigate])

  return (
    <>
      {/* 路由级错误兜底（含懒加载 chunk 加载失败）；key 随 pathname 变化重建实例，路由切换即重置兜底态 */}
      <ErrorBoundary key={location.pathname}>
        <Routes>
          <Route path="/login" element={<LoginPage />} />
          <Route
            element={
              <RequireAuth>
                <AppLayout />
              </RequireAuth>
            }
          >
            <Route index element={<PageSuspense><DashboardPage /></PageSuspense>} />
            <Route path="channels" element={<PageSuspense><ChannelsPage /></PageSuspense>} />
            <Route path="domains" element={<PageSuspense><DomainsPage /></PageSuspense>} />
            <Route path="templates" element={<PageSuspense><TemplatesPage /></PageSuspense>} />
            <Route path="logs" element={<PageSuspense><LogsPage /></PageSuspense>} />
            <Route path="events" element={<PageSuspense><EventsPage /></PageSuspense>} />
            <Route path="suppressions" element={<PageSuspense><SuppressionsPage /></PageSuspense>} />
            <Route path="keys" element={<PageSuspense><KeysPage /></PageSuspense>} />
            <Route path="users" element={<PageSuspense><UsersPage /></PageSuspense>} />
            <Route path="audit" element={<PageSuspense><AuditPage /></PageSuspense>} />
            <Route path="settings" element={<PageSuspense><SettingsPage /></PageSuspense>} />
          </Route>
          <Route path="*" element={<Navigate to="/" replace />} />
        </Routes>
      </ErrorBoundary>
      <Toaster position="top-center" richColors closeButton theme={isDark ? 'dark' : 'light'} />
    </>
  )
}
