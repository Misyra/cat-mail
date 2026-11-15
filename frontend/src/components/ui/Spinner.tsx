import { LoaderCircle } from 'lucide-react'
import { useTranslation } from 'react-i18next'

import { cn } from '@/lib/utils'

export interface SpinnerProps {
  className?: string
}

export function Spinner({ className }: SpinnerProps) {
  // 数组形式（i18next 27 类型）：只用 ui 命名空间（「加载中」无省略号，与 common:state.loading 文案不同）
  const { t } = useTranslation(['ui'])
  return (
    <LoaderCircle
      className={cn('h-5 w-5 animate-spin text-sky-500', className)}
      aria-label={t('spinner.loading')}
    />
  )
}
