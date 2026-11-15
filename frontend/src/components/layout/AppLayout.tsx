import { Menu, Search } from 'lucide-react'
import { useEffect, useState } from 'react'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { Outlet, useLocation, useNavigate } from 'react-router'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import { api } from '@/lib/api'
import { useMe } from '@/lib/auth'
import { THEME_META, useTheme } from '@/lib/theme'
import { CommandPalette } from '@/components/CommandPalette'
import { NotificationBell } from '@/components/layout/NotificationBell'
import { Sidebar } from '@/components/layout/Sidebar'
import { NAV_ITEMS } from '@/components/layout/nav'

// O49：⌘K 提示按平台显示 —— Mac 显示 ⌘K，其他平台显示 Ctrl K。
// userAgentData 仅 Chromium 实现，TS 未内置其类型，in 收窄后断言最小形状；Safari/Firefox 回退 navigator.platform。
const IS_MAC =
  'userAgentData' in navigator
    ? ((navigator.userAgentData as { platform?: string }).platform ?? navigator.platform)
        .toLowerCase()
        .includes('mac')
    : navigator.platform.toLowerCase().includes('mac')

/**
 * 登录后应用布局（inset 风格）：侧栏 bg-surface-raised（浅）与内容区 bg-surface（深）双色分离；
 * 顶栏 = 菜单按钮（<lg）+ 页面标题 + ⌘K 搜索入口 + 深浅切换；<Outlet /> 为页面主体。
 */
export function AppLayout() {
  const location = useLocation()
  const navigate = useNavigate()
  const queryClient = useQueryClient()
  const { data: me } = useMe()
  const { theme, cycleTheme } = useTheme()
  // 数组形式：nav label 存的是带 ns 前缀的 key，且需跨用 common 的共享词汇
  const { t } = useTranslation(['layout', 'common'])
  // ⌘K / Ctrl+K 命令面板开关
  const [paletteOpen, setPaletteOpen] = useState(false)
  // 移动端侧栏抽屉开关（O32，<lg；桌面 ≥lg 侧栏常驻，不受此状态影响）
  const [drawerOpen, setDrawerOpen] = useState(false)

  const current = NAV_ITEMS.find((item) =>
    item.end ? location.pathname === item.to : location.pathname.startsWith(item.to),
  )

  const logoutMutation = useMutation({
    mutationFn: () => api<{ ok: boolean }>('/api/auth/logout', { method: 'POST' }),
  })

  async function handleLogout() {
    try {
      await logoutMutation.mutateAsync()
    } catch {
      // 后端异常也照常回到登录页，但不提示「已退出登录」（审计 P3-8）
      queryClient.clear()
      toast.error(t('toast.logoutFailed'))
      navigate('/login', { replace: true })
      return
    }
    queryClient.clear()
    toast.success(t('toast.loggedOut'))
    navigate('/login', { replace: true })
  }

  // 全局快捷键：⌘K / Ctrl+K 开关命令面板（面板开着时再按一次关闭）
  useEffect(() => {
    const onKeydown = (e: KeyboardEvent) => {
      // 长按会连续触发 keydown，不拦 e.repeat 会反复开关面板
      if (e.repeat) return
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 'k') {
        e.preventDefault()
        // 面板已开时放行走下方 toggle 关闭；只有「打开」方向需要防叠加：
        // Modal / W2 迁移后的原生 dialog 已打开时再叠命令面板，
        // 两层都监听 window keydown，Esc 会一次按键双关
        if (
          !paletteOpen &&
          document.querySelector('dialog[open]:not(#command-palette), [aria-modal="true"]')
        ) {
          return
        }
        setPaletteOpen((open) => !open)
      }
    }
    window.addEventListener('keydown', onKeydown)
    return () => window.removeEventListener('keydown', onKeydown)
  }, [paletteOpen])

  // 路由切换自动关闭抽屉（O32）：抽屉内点导航 → pathname 变化 → 本 effect 收口，
  // 关闭发生在导航已提交之后，不会打断路由跳转，无竞态；抽屉开着点当前路由不触发此 effect，
  // 由用户再按 Esc / 点遮罩关闭
  useEffect(() => {
    setDrawerOpen(false)
  }, [location.pathname])

  const themeMeta = THEME_META[theme]

  return (
    <div className="app-glass-void flex h-dvh overflow-hidden bg-surface">
      {/* 键盘用户可跳过侧栏导航直达主内容：平时仅读屏可见，Tab 聚焦时固定在左上角 */}
      <a
        href="#main-content"
        className="sr-only focus:not-sr-only focus:fixed focus:left-4 focus:top-4 focus:z-[60] focus:rounded-lg focus:bg-sky-600 focus:px-3 focus:py-2 focus:text-sm focus:font-medium focus:text-white"
      >
        {t('header.skipToContent')}
      </a>
      <Sidebar
        username={me?.username}
        onLogout={handleLogout}
        loggingOut={logoutMutation.isPending}
        drawerOpen={drawerOpen}
        onDrawerClose={() => setDrawerOpen(false)}
      />

      <div className="app-glass-void flex min-w-0 flex-1 flex-col bg-surface">
        {/* 顶栏：与内容区同底色（inset 风格，与侧栏 bg-surface-raised 形成分离）；玻璃档下
            body/页面级容器转透明让极光透出，见 index.css 玻璃段 */}
        <header className="glass-tint flex h-14 shrink-0 items-center justify-between border-b border-border-subtle px-6">
          <div className="flex items-center gap-2">
            {/* 移动端侧栏抽屉入口（O32，<lg 显示；桌面 ≥lg 侧栏常驻无此按钮） */}
            <button
              type="button"
              onClick={() => setDrawerOpen(true)}
              title={t('header.openMenu')}
              aria-label={t('header.openMenu')}
              aria-controls="app-drawer"
              aria-expanded={drawerOpen}
              className="flex h-8 w-8 items-center justify-center rounded-lg text-slate-500 transition-colors hover:bg-slate-100 hover:text-slate-700 dark:text-slate-400 dark:hover:bg-slate-800 dark:hover:text-slate-200 pointer-coarse:h-11 pointer-coarse:w-11 lg:hidden"
            >
              <Menu className="h-4 w-4" aria-hidden />
            </button>
            <span className="text-sm font-medium text-slate-500 dark:text-slate-400">
              {current ? t(current.label) : t('brand.name')}
            </span>
          </div>
          <div className="flex items-center gap-2">
            {me?.username && (
              <span className="hidden text-sm text-slate-500 sm:block dark:text-slate-400">
                {me.username}
              </span>
            )}
            <NotificationBell />
            <button
              type="button"
              onClick={() => setPaletteOpen(true)}
              title={t('header.searchTitle')}
              aria-label={t('header.openPalette')}
              aria-keyshortcuts="Meta+K Control+K"
              className="flex h-8 items-center gap-2 rounded-lg border border-slate-300 bg-white px-2.5 text-slate-500 shadow-sm transition-colors hover:border-slate-400 hover:text-slate-600 dark:border-slate-700 dark:bg-slate-800 dark:text-slate-400 dark:hover:border-slate-600 dark:hover:text-slate-300 pointer-coarse:h-11 pointer-coarse:min-w-11 pointer-coarse:justify-center"
            >
              <Search className="h-4 w-4" aria-hidden />
              <span className="hidden text-sm sm:inline">{t('common:actions.search')}</span>
              <kbd className="hidden items-center rounded border border-slate-200 bg-slate-50 px-1.5 py-0.5 font-sans text-[11px] font-medium text-slate-500 sm:inline-flex dark:border-slate-600 dark:bg-slate-900 dark:text-slate-400">
                {IS_MAC ? '⌘K' : 'Ctrl K'}
              </kbd>
            </button>
            <button
              type="button"
              onClick={cycleTheme}
              aria-label={t('theme.toggleAria', { current: t(themeMeta.labelKey) })}
              title={t('theme.toggleTitle', { current: t(themeMeta.labelKey) })}
              className="flex h-8 w-8 items-center justify-center rounded-lg text-slate-500 transition-colors hover:bg-slate-100 hover:text-slate-700 dark:text-slate-400 dark:hover:bg-slate-800 dark:hover:text-slate-200 pointer-coarse:h-11 pointer-coarse:w-11"
            >
              <themeMeta.icon className="h-4 w-4" aria-hidden />
            </button>
          </div>
        </header>

        {/* 内容区：统一宽度契约 —— 所有页面内容盒居中 max-w-6xl、左右 px-6，PageHeader 与页面内容同容器自然对齐 */}
        <main id="main-content" className="min-h-0 flex-1 overflow-y-auto">
          <div className="mx-auto w-full max-w-6xl px-6 py-6">
            <Outlet />
          </div>
        </main>
      </div>

      <CommandPalette
        open={paletteOpen}
        onClose={() => setPaletteOpen(false)}
        onLogout={() => void handleLogout()}
        loggingOut={logoutMutation.isPending}
      />
    </div>
  )
}
