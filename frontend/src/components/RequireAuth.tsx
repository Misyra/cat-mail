import type { ReactNode } from 'react'
import { useTranslation } from 'react-i18next'
import { Navigate } from 'react-router'

import { Button } from '@/components/ui/Button'
import { Card } from '@/components/ui/Card'
import { Spinner } from '@/components/ui/Spinner'
import { ApiError } from '@/lib/api'
import { useMe } from '@/lib/auth'

/**
 * 包裹需登录页面：/api/auth/me 返回 401（未登录）才跳 /login；
 * 其余错误（后端重启/网络断/5xx）不踢用户下线，保持当前路由并展示可重试的错误卡片。
 */
export function RequireAuth({ children }: { children: ReactNode }) {
  const me = useMe()
  // 数组形式（i18next 27 类型）：需跨用 common 的共享词汇
  const { t } = useTranslation(['ui', 'common'])

  if (me.isPending) {
    return (
      <div className="flex min-h-dvh items-center justify-center bg-slate-100 dark:bg-slate-950">
        <Spinner className="h-6 w-6" />
      </div>
    )
  }

  if (me.isError) {
    // 仅 401 视为未登录；ApiError.status 为 0 表示网络错误，同样走重试
    if (me.error instanceof ApiError && me.error.status === 401) {
      return <Navigate to="/login" replace />
    }
    return (
      <div className="flex min-h-dvh items-center justify-center bg-slate-100 px-4 dark:bg-slate-950">
        <Card className="w-full max-w-sm p-8 text-center">
          <h1 className="text-base font-semibold text-slate-900 dark:text-white">
            {t('requireAuth.connectionFailed')}
          </h1>
          <p className="mt-2 break-all text-sm text-slate-500 dark:text-slate-400">
            {me.error.message}
          </p>
          <Button
            variant="secondary"
            className="mt-6"
            loading={me.isFetching}
            onClick={() => void me.refetch()}
          >
            {t('common:actions.retry')}
          </Button>
        </Card>
      </div>
    )
  }

  return <>{children}</>
}
