import { AlertCircle, RefreshCw } from 'lucide-react'
import { useTranslation } from 'react-i18next'

import { Button } from '@/components/ui/Button'
import { CardSkeleton, ChartSkeleton, Skeleton, TableSkeleton } from '@/components/ui/Skeleton'
import { cn } from '@/lib/utils'

/** 资源类型约定：loading 分支按此渲染对应骨架（表格页 table、统计卡 card、图表 chart） */
export type QuerySkeleton = 'table' | 'card' | 'chart'

/** 数据加载中态（SPEC §8 三态之一）：按资源类型渲染骨架占位，不再用大范围 Spinner */
export function QueryLoading({
  skeleton = 'card',
  className,
}: {
  /** 缺省渲染中性整块骨架（弹窗内小区域等） */
  skeleton?: QuerySkeleton | 'block'
  className?: string
}) {
  if (skeleton === 'table') return <TableSkeleton rows={6} className={className} />
  if (skeleton === 'chart') return <ChartSkeleton className={className} />
  if (skeleton === 'block') return <Skeleton className={cn('h-64 w-full', className)} />
  return <CardSkeleton className={className} />
}

/** 查询失败态：错误信息（ApiError.message）+ 重试按钮（react-query refetch） */
export function QueryError({
  message,
  onRetry,
  className,
}: {
  message: string
  onRetry: () => void
  className?: string
}) {
  // 数组形式（i18next 27 类型）：只用 common 的共享词汇
  const { t } = useTranslation(['common'])
  return (
    <div className={cn('flex flex-col items-center gap-3 py-10 text-center', className)}>
      <AlertCircle className="h-8 w-8 text-red-400" aria-hidden />
      <p role="alert" className="text-sm text-red-600 dark:text-red-400">
        {message}
      </p>
      <Button variant="secondary" onClick={onRetry}>
        <RefreshCw className="h-4 w-4" aria-hidden />
        {t('actions.retry')}
      </Button>
    </div>
  )
}
