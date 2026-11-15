import i18next from 'i18next'

/**
 * 时间显示工具（SPEC §8：时间本地化、表格时间相对化 + 悬停绝对时间）。
 * 纯函数在调用时经 i18next 运行时求值（先例：lib/audit.ts auditActionLabel），
 * 组件侧由 RelativeTime 挂 useTranslation 订阅 languageChanged 重渲染。
 */

/** 本地化绝对时间（随当前语言，缺省 zh-CN）：2026/11/11 14:30；withSeconds 时带秒 */
export function formatDateTime(iso: string | null | undefined, withSeconds = false): string {
  if (!iso) return '—'
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return iso
  return d.toLocaleString(i18next.language || 'zh-CN', {
    year: 'numeric',
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
    ...(withSeconds ? { second: '2-digit' } : {}),
  })
}

/**
 * 相对时间：刚刚 / N 秒前 / N 分钟前 / N 小时前 / N 天前；超过 30 天回落绝对时间。
 * now 为可选时间基准（毫秒），供共享时钟传入统一快照，保证同一次渲染内所有实例取值一致；
 * 缺省取当前时间，行为与直接调用一致。
 */
export function formatRelativeTime(iso: string | null | undefined, now: number = Date.now()): string {
  if (!iso) return '—'
  const time = new Date(iso).getTime()
  if (Number.isNaN(time)) return iso
  const diffMs = now - time
  if (diffMs < 10_000) return i18next.t('common:time.justNow')
  const seconds = Math.floor(diffMs / 1000)
  if (seconds < 60) return i18next.t('common:time.secondsAgo', { count: seconds })
  const minutes = Math.floor(seconds / 60)
  if (minutes < 60) return i18next.t('common:time.minutesAgo', { count: minutes })
  const hours = Math.floor(minutes / 60)
  if (hours < 24) return i18next.t('common:time.hoursAgo', { count: hours })
  const days = Math.floor(hours / 24)
  if (days < 30) return i18next.t('common:time.daysAgo', { count: days })
  return formatDateTime(iso)
}
