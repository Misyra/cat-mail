import { Inbox, type LucideIcon } from 'lucide-react'
import type { ReactNode } from 'react'

import { cn } from '@/lib/utils'

export interface EmptyStateProps {
  icon?: LucideIcon
  title: string
  /** 一句话补充说明（可选） */
  description?: string
  /** 主操作按钮（如「添加第一个渠道」「清除筛选」） */
  action?: ReactNode
  className?: string
}

/** 空态：居中、大图标淡色、说明文案 + 主操作（SPEC §8 空态统一出口） */
export function EmptyState({
  icon: Icon = Inbox,
  title,
  description,
  action,
  className,
}: EmptyStateProps) {
  return (
    <div className={cn('flex flex-col items-center gap-4 px-6 py-14 text-center', className)}>
      <div className="flex h-12 w-12 items-center justify-center rounded-xl bg-slate-100 dark:bg-slate-800">
        <Icon className="h-6 w-6 text-slate-400 dark:text-slate-500" aria-hidden />
      </div>
      <div className="space-y-1">
        <p className="text-sm font-medium text-text-primary">{title}</p>
        {description && <p className="text-xs text-text-muted">{description}</p>}
      </div>
      {action && <div className="mt-1">{action}</div>}
    </div>
  )
}
