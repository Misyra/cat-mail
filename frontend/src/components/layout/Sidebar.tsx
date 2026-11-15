import { Cat, ChevronsLeft, ChevronsRight, LogOut } from 'lucide-react'
import { NavLink } from 'react-router'
import { useTranslation } from 'react-i18next'
import type { CSSProperties } from 'react'

import { cn } from '@/lib/utils'
import { useMediaQuery } from '@/lib/media'
import { useNativeDialog } from '@/lib/useNativeDialog'
import { useSidebarCollapsed } from '@/lib/sidebar'
import { NAV_ITEMS } from '@/components/layout/nav'

/** 展开宽度（w-60）/ 折叠宽度（w-14 纯图标态），经 --sidebar-width 变量 + transition 过渡 */
const EXPANDED_WIDTH = '15rem'
const COLLAPSED_WIDTH = '3.5rem'

export interface SidebarProps {
  username?: string
  onLogout: () => void
  loggingOut?: boolean
  /** 移动抽屉（O32）开关状态：传入后渲染 <dialog id="app-drawer">，仅在 <lg 由顶栏菜单按钮驱动 */
  drawerOpen?: boolean
  /** 抽屉关闭回调（Esc / 点击遮罩 / 路由切换都会汇回此处） */
  onDrawerClose?: () => void
}

interface SidebarBodyProps {
  /** false 时恒为展开态（移动抽屉即恒传 false） */
  isCollapsed: boolean
  username?: string
  onLogout: () => void
  loggingOut?: boolean
  /** 传入时渲染折叠开关（仅桌面 aside；抽屉无开关） */
  onToggleCollapse?: () => void
}

/**
 * 侧栏主体（桌面 aside 与移动抽屉的单一标记源）：logo + 导航 + 底部当前用户/退出登录。
 * 折叠态只留图标（title/aria-label 提示完整名称）；O31：触屏下命中区提到 ≥44px，
 * 精确指针（桌面鼠标）密度不变。抽屉复用同一段标记，恒为展开态且无折叠开关。
 */
function SidebarBody({ isCollapsed, username, onLogout, loggingOut, onToggleCollapse }: SidebarBodyProps) {
  // 数组形式：nav label 存的是带 ns 前缀的 key，跨命名空间 key 仅在多 ns 下被类型接受
  const { t } = useTranslation(['layout', 'common'])
  const navItemClasses = ({ isActive }: { isActive: boolean }) =>
    cn(
      'flex items-center gap-3 rounded-lg text-sm font-medium transition-colors pointer-coarse:min-h-11',
      isCollapsed ? 'h-10 justify-center px-0' : 'px-3 py-2',
      isActive
        ? 'bg-sky-50 text-sky-700 dark:bg-sky-500/10 dark:text-sky-400'
        : 'text-slate-600 hover:bg-slate-100 hover:text-slate-900 dark:text-slate-400 dark:hover:bg-slate-800 dark:hover:text-slate-100',
    )

  return (
    <>
      {/* Logo（折叠态只留图标） */}
      <div className={cn('flex items-center gap-3 py-5', isCollapsed ? 'justify-center' : 'px-5')}>
        <div
          className="flex h-9 w-9 shrink-0 items-center justify-center rounded-xl bg-sky-500 text-white shadow-sm shadow-sky-500/30"
          title={isCollapsed ? t('brand.name') : undefined}
        >
          <Cat className="h-5 w-5" />
        </div>
        {!isCollapsed && (
          <div className="min-w-0">
            <p className="truncate text-base font-semibold tracking-tight text-slate-900 dark:text-white">
              {t('brand.name')}
            </p>
            <p className="truncate text-xs text-slate-500 dark:text-slate-400">{t('brand.subtitle')}</p>
          </div>
        )}
      </div>

      {/* 导航（折叠态只留图标 + title/aria-label 提示） */}
      <nav className="flex-1 space-y-1 overflow-y-auto px-3 py-2">
        {NAV_ITEMS.map((item) => (
          <NavLink
            key={item.to}
            to={item.to}
            end={item.end}
            title={t(item.label)}
            aria-label={isCollapsed ? t(item.label) : undefined}
            className={navItemClasses}
          >
            <item.icon className="h-4 w-4 shrink-0" aria-hidden />
            {!isCollapsed && t(item.label)}
          </NavLink>
        ))}
      </nav>

      {/* 底部：折叠开关（仅桌面 aside）+ 当前用户 + 退出登录 */}
      <div className="border-t border-border-subtle p-3">
        {username && !isCollapsed && (
          <p className="truncate px-3 pb-2 text-xs text-slate-500 dark:text-slate-400">
            {t('sidebar.currentUser', { name: username })}
          </p>
        )}
        {onToggleCollapse && (
          <button
            type="button"
            onClick={onToggleCollapse}
            title={isCollapsed ? t('sidebar.expand') : t('sidebar.collapse')}
            aria-label={isCollapsed ? t('sidebar.expand') : t('sidebar.collapse')}
            className={cn(
              'flex w-full items-center gap-3 rounded-lg text-sm font-medium text-slate-600 transition-colors hover:bg-slate-100 hover:text-slate-900 dark:text-slate-400 dark:hover:bg-slate-800 dark:hover:text-slate-100 pointer-coarse:min-h-11',
              isCollapsed ? 'h-10 justify-center px-0' : 'px-3 py-2',
            )}
          >
            {isCollapsed ? (
              <ChevronsRight className="h-4 w-4 shrink-0" aria-hidden />
            ) : (
              <ChevronsLeft className="h-4 w-4 shrink-0" aria-hidden />
            )}
            {!isCollapsed && t('sidebar.collapse')}
          </button>
        )}
        <button
          type="button"
          onClick={onLogout}
          disabled={loggingOut}
          title={isCollapsed ? t('sidebar.logout') : undefined}
          aria-label={isCollapsed ? t('sidebar.logout') : undefined}
          className={cn(
            'flex w-full items-center gap-3 rounded-lg text-sm font-medium text-slate-600 transition-colors hover:bg-red-50 hover:text-red-600 disabled:cursor-not-allowed disabled:opacity-60 dark:text-slate-400 dark:hover:bg-red-500/10 dark:hover:text-red-400 pointer-coarse:min-h-11',
            isCollapsed ? 'mt-1 h-10 justify-center px-0' : 'mt-1 px-3 py-2',
          )}
        >
          <LogOut className="h-4 w-4 shrink-0" aria-hidden />
          {!isCollapsed && t('sidebar.logout')}
        </button>
      </div>
    </>
  )
}

/**
 * 移动端侧栏抽屉（O32，<lg）：<dialog> 左侧停靠（m-0 mr-auto 推左），复用 app-dialog app-dock
 * 的遮罩与 useNativeDialog 的受控开关（Esc / 点击遮罩 / cancel/close 汇流）；路由切换由
 * AppLayout 监听 pathname 自动关闭。恒为展开态（与桌面折叠逻辑解耦，无折叠开关）。
 */
function SidebarDrawer({
  open,
  onClose,
  username,
  onLogout,
  loggingOut,
}: {
  open: boolean
  onClose: () => void
  username?: string
  onLogout: () => void
  loggingOut?: boolean
}) {
  const dialogRef = useNativeDialog(open, onClose)
  // 数组形式：与 SidebarBody 一致（nav label 为带 ns 前缀的 key）
  const { t } = useTranslation(['layout', 'common'])

  return (
    <dialog
      ref={dialogRef}
      id="app-drawer"
      aria-label={t('sidebar.navMenu')}
      className="glass-surface app-dialog app-dock m-0 mr-auto flex h-dvh max-h-none w-60 max-w-[85vw] flex-col border-r border-border-subtle bg-surface-raised shadow-xl animate-dock-in-left"
    >
      <SidebarBody
        isCollapsed={false}
        username={username}
        onLogout={onLogout}
        loggingOut={loggingOut}
      />
    </dialog>
  )
}

/**
 * 应用侧栏（inset 布局的浅色一侧 bg-surface-raised），双形态：
 * - 桌面 aside（≥lg 显示）：可折叠为纯图标态，偏好存 localStorage 并跨标签页同步；
 * - 移动抽屉（<lg）：由 AppLayout 传 drawerOpen / onDrawerClose 驱动的原生 dialog 左侧停靠。
 */
export function Sidebar({
  username,
  onLogout,
  loggingOut,
  drawerOpen,
  onDrawerClose,
}: SidebarProps) {
  const { collapsed, toggleCollapsed } = useSidebarCollapsed()
  const canCollapse = useMediaQuery('(min-width: 1024px)')
  const isCollapsed = canCollapse && collapsed

  return (
    <>
      <aside
        style={{ '--sidebar-width': isCollapsed ? COLLAPSED_WIDTH : EXPANDED_WIDTH } as CSSProperties}
        className="glass-tint hidden w-[var(--sidebar-width)] shrink-0 flex-col border-r border-border-subtle bg-surface-raised transition-[width] duration-200 ease-in-out motion-reduce:transition-none lg:flex"
      >
        <SidebarBody
          isCollapsed={isCollapsed}
          username={username}
          onLogout={onLogout}
          loggingOut={loggingOut}
          onToggleCollapse={toggleCollapsed}
        />
      </aside>
      {drawerOpen !== undefined && onDrawerClose && (
        <SidebarDrawer
          open={drawerOpen}
          onClose={onDrawerClose}
          username={username}
          onLogout={onLogout}
          loggingOut={loggingOut}
        />
      )}
    </>
  )
}
