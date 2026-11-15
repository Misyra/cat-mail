import { Card } from '@/components/ui/Card'
import { cn } from '@/lib/utils'

/** 基础骨架块：脉冲动画占位（加载态统一用骨架，Spinner 仅保留在按钮内联） */
export function Skeleton({ className }: { className?: string }) {
  return (
    <div className={cn('animate-pulse rounded bg-slate-200 motion-reduce:animate-none dark:bg-slate-800', className)} aria-hidden />
  )
}

/** 表格骨架：表头条 + N 行（带分隔线），固定行数防高度跳动 */
export function TableSkeleton({
  rows = 5,
  className,
}: {
  rows?: number
  className?: string
}) {
  return (
    <div className={cn('px-5 py-4', className)} aria-hidden>
      <div className="flex items-center gap-4 border-b border-border-subtle pb-3">
        <Skeleton className="h-3 w-24" />
        <Skeleton className="h-3 w-16" />
        <Skeleton className="ml-auto hidden h-3 w-16 sm:block" />
      </div>
      <div className="divide-y divide-border-subtle">
        {Array.from({ length: rows }, (_, row) => (
          <div key={row} className="flex items-center gap-4 py-3.5">
            <Skeleton className="h-4 w-40 max-w-[30%]" />
            <Skeleton className="h-4 w-20" />
            <Skeleton className="hidden h-4 w-28 md:block" />
            <Skeleton className="ml-auto h-4 w-14" />
          </div>
        ))}
      </div>
    </div>
  )
}

/** 卡片骨架：标题行 + 大数字占位（统计卡/设置表单等） */
export function CardSkeleton({ className }: { className?: string }) {
  return (
    <div className={cn('space-y-4 px-5 py-4', className)} aria-hidden>
      <Skeleton className="h-4 w-32" />
      <Skeleton className="h-4 w-3/4" />
      <Skeleton className="h-9 w-full" />
      <Skeleton className="h-9 w-full" />
    </div>
  )
}

/** 图表骨架：固定高度占位防布局跳动（图表区约 280px + 图例/内边距） */
export function ChartSkeleton({ className }: { className?: string }) {
  return (
    <div className={cn('px-5 py-5', className)} aria-hidden>
      <Skeleton className="mb-2 h-3 w-40" />
      <Skeleton className="h-[280px] w-full" />
    </div>
  )
}

/** 页面骨架：路由懒加载的 Suspense 兜底（标题区 + 统计卡网格 + 卡片表格，与页面加载态同风格） */
export function PageSkeleton() {
  return (
    <div className="mx-auto max-w-6xl space-y-6" aria-hidden>
      <div className="space-y-2">
        <Skeleton className="h-7 w-28" />
        <Skeleton className="h-3.5 w-52" />
      </div>
      <div className="grid grid-cols-1 gap-4 sm:grid-cols-2 xl:grid-cols-4">
        {Array.from({ length: 4 }, (_, i) => (
          <Card key={i} className="p-5">
            <div className="flex items-center gap-2">
              <Skeleton className="h-7 w-7 rounded-lg" />
              <Skeleton className="h-3 w-16" />
            </div>
            <Skeleton className="mt-2.5 h-6 w-20" />
          </Card>
        ))}
      </div>
      <Card>
        <TableSkeleton rows={6} />
      </Card>
    </div>
  )
}
