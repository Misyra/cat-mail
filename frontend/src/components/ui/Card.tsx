import type { HTMLAttributes } from 'react'

import { cn } from '@/lib/utils'

export interface CardProps extends HTMLAttributes<HTMLDivElement> {}

/** 圆角卡片容器（管理后台统一风格；glass-tint 只在玻璃主题档生效——半透明+高光无 blur，
 * blur 只留给压在滚动内容上的浮层，见 index.css 玻璃段） */
export function Card({ className, ...rest }: CardProps) {
  return (
    <div
      className={cn(
        'glass-tint rounded-xl border border-border-subtle bg-surface-raised shadow-sm',
        className,
      )}
      {...rest}
    />
  )
}
