/**
 * 顶栏通知铃铛：未读数 30s 轮询（react-query refetchInterval），点击展开通知面板。
 * 面板拉取最近 20 条：未读项左侧色条按级别着色（error 红 / warn 琥珀 / info 蓝），
 * 点击条目标记该条已读并跳转渠道页；支持全部已读、Esc / 点击面板外关闭。
 * 面板 z-40：低于 Modal / ⌘K 命令面板的 z-50 遮罩层。
 */
import { Bell } from 'lucide-react'
import { useEffect, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { useNavigate } from 'react-router'

import { RelativeTime } from '@/components/RelativeTime'
import { Button } from '@/components/ui/Button'
import { Skeleton } from '@/components/ui/Skeleton'
import {
  invalidateNotifications,
  markNotificationsRead,
  notifyLevelBarClass,
  useNotifications,
  useUnreadNotifications,
  type NotificationItem,
} from '@/lib/notifications'
import { cn } from '@/lib/utils'

/** 角标数字 99+ 封顶 */
function formatBadgeCount(count: number): string {
  return count > 99 ? '99+' : String(count)
}

export function NotificationBell() {
  const { t } = useTranslation(['notifications', 'common'])
  const navigate = useNavigate()
  const queryClient = useQueryClient()
  const [open, setOpen] = useState(false)
  const containerRef = useRef<HTMLDivElement>(null)

  // 未读数轮询：面板开合均持续（30s）
  const unreadQuery = useUnreadNotifications()
  // 面板打开才拉取列表（最近 20 条；staleTime 默认 0，每次展开自动复核）
  const listQuery = useNotifications(1, open)

  const markReadMutation = useMutation({
    mutationFn: markNotificationsRead,
    onSuccess: () => invalidateNotifications(queryClient),
  })

  // 打开期间：点击面板外关闭 + Esc 关闭（监听对象含铃铛按钮，避免点按钮时「先关再开」闪烁）
  useEffect(() => {
    if (!open) return
    const onPointerDown = (e: PointerEvent) => {
      if (
        containerRef.current &&
        e.target instanceof Node &&
        !containerRef.current.contains(e.target)
      ) {
        setOpen(false)
      }
    }
    const onKeydown = (e: KeyboardEvent) => {
      if (e.key === 'Escape') setOpen(false)
    }
    document.addEventListener('pointerdown', onPointerDown)
    document.addEventListener('keydown', onKeydown)
    return () => {
      document.removeEventListener('pointerdown', onPointerDown)
      document.removeEventListener('keydown', onKeydown)
    }
  }, [open])

  const unread = unreadQuery.data?.count ?? 0

  function handleItemClick(item: NotificationItem) {
    if (!item.read) markReadMutation.mutate([item.id])
    setOpen(false)
    if (item.channel_id !== null) navigate('/channels')
  }

  return (
    <div ref={containerRef} className="relative">
      <button
        type="button"
        onClick={() => setOpen((v) => !v)}
        aria-expanded={open}
        aria-controls="notification-panel"
        aria-label={unread > 0 ? t('unreadAria', { count: unread }) : t('title')}
        title={unread > 0 ? t('unreadTitle', { count: unread }) : t('title')}
        className="relative flex h-8 w-8 items-center justify-center rounded-lg pointer-coarse:h-11 pointer-coarse:w-11 text-slate-500 transition-colors hover:bg-slate-100 hover:text-slate-700 dark:text-slate-400 dark:hover:bg-slate-800 dark:hover:text-slate-200"
      >
        <Bell className="h-4 w-4" aria-hidden />
        {unread > 0 && (
          <span
            aria-hidden
            className="absolute -right-1.5 -top-1.5 flex h-4 min-w-4 items-center justify-center rounded-full bg-red-500 px-1 text-[10px] font-semibold leading-none text-white ring-2 ring-surface"
          >
            {formatBadgeCount(unread)}
          </span>
        )}
      </button>

      {open && (
        <div
          id="notification-panel"
          role="region"
          aria-label={t('listAria')}
          className="glass-surface absolute right-0 top-full z-40 mt-2 flex max-h-96 w-[22rem] max-w-[min(22rem,calc(100vw-2rem))] flex-col overflow-hidden rounded-xl border border-border-subtle bg-surface-raised shadow-xl animate-pop-in"
        >
          {/* 头部：标题 + 未读数 + 全部已读 */}
          <div className="flex shrink-0 items-center justify-between border-b border-border-subtle px-4 py-2.5">
            <p className="text-sm font-semibold text-slate-900 dark:text-white">
              {t('title')}
              {unread > 0 && (
                <span className="ml-2 text-xs font-normal text-slate-500 dark:text-slate-400">
                  {t('unreadCount', { count: unread })}
                </span>
              )}
            </p>
            <button
              type="button"
              onClick={() => markReadMutation.mutate(null)}
              disabled={unread === 0 || markReadMutation.isPending}
              className="rounded-md px-1.5 py-0.5 text-xs text-sky-600 transition-colors hover:bg-sky-50 hover:text-sky-700 disabled:cursor-not-allowed disabled:text-slate-400 dark:text-sky-400 dark:hover:bg-sky-500/10 dark:disabled:text-slate-600"
            >
              {t('markAllRead')}
            </button>
          </div>

          {/* 列表体 */}
          <div className="min-h-0 flex-1 overflow-y-auto">
            {listQuery.isPending ? (
              <div className="space-y-4 p-4" aria-hidden>
                {Array.from({ length: 3 }, (_, i) => (
                  <div key={i} className="flex gap-2.5">
                    <Skeleton className="h-10 w-1 rounded-full" />
                    <div className="flex-1 space-y-1.5">
                      <Skeleton className="h-3.5 w-32" />
                      <Skeleton className="h-3 w-full" />
                      <Skeleton className="h-3 w-16" />
                    </div>
                  </div>
                ))}
              </div>
            ) : listQuery.isError ? (
              <div className="flex flex-col items-center gap-3 px-4 py-10 text-center">
                <p className="text-sm text-red-600 dark:text-red-400">{listQuery.error.message}</p>
                <Button variant="secondary" onClick={() => void listQuery.refetch()}>
                  {t('common:actions.retry')}
                </Button>
              </div>
            ) : listQuery.data.items.length === 0 ? (
              <p className="px-4 py-12 text-center text-sm text-slate-500 dark:text-slate-400">
                {t('empty')}
              </p>
            ) : (
              <ul className="divide-y divide-border-subtle">
                {listQuery.data.items.map((item) => (
                  <li key={item.id}>
                    <button
                      type="button"
                      onClick={() => handleItemClick(item)}
                      className="flex w-full items-stretch gap-2.5 px-3 py-2.5 text-left transition-colors hover:bg-slate-50 dark:hover:bg-slate-800/60"
                    >
                      {/* 未读色条：按级别着色；已读留空对齐 */}
                      <span
                        aria-hidden
                        className={cn(
                          'w-1 shrink-0 rounded-full',
                          item.read ? 'bg-transparent' : notifyLevelBarClass(item.level),
                        )}
                      />
                      <span className="min-w-0 flex-1">
                        <span
                          className={cn(
                            'block truncate text-sm',
                            item.read
                              ? 'text-slate-500 dark:text-slate-400'
                              : 'font-medium text-slate-900 dark:text-white',
                          )}
                        >
                          {item.title}
                        </span>
                        {item.body && (
                          <span className="mt-0.5 line-clamp-2 block text-xs text-slate-500 dark:text-slate-400">
                            {item.body}
                          </span>
                        )}
                        <RelativeTime iso={item.created_at} className="mt-1 block text-[11px]" />
                      </span>
                    </button>
                  </li>
                ))}
              </ul>
            )}
          </div>
        </div>
      )}
    </div>
  )
}
