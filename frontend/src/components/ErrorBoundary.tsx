import { Component, type ErrorInfo, type ReactNode } from 'react'
import { useTranslation } from 'react-i18next'
import { Link } from 'react-router'

import { Button } from '@/components/ui/Button'
import { Card } from '@/components/ui/Card'

interface ErrorBoundaryProps {
  children: ReactNode
}

interface ErrorBoundaryState {
  error: Error | null
}

/** 兜底卡片：函数组件承载 useTranslation（类组件不能用 hook），渲染文案随语言切换 */
function ErrorFallback({ message }: { message: string }) {
  // 数组形式（i18next 27 类型）：需跨用 common 的共享词汇
  const { t } = useTranslation(['ui', 'common'])
  return (
    <div className="flex min-h-dvh items-center justify-center bg-slate-100 px-4 dark:bg-slate-950">
      <Card className="w-full max-w-sm p-8 text-center">
        <h1 className="text-base font-semibold text-slate-900 dark:text-white">
          {t('errorBoundary.title')}
        </h1>
        <p className="mt-2 break-all text-sm text-slate-500 dark:text-slate-400">{message}</p>
        <div className="mt-6 flex justify-center gap-3">
          <Button onClick={() => window.location.reload()}>{t('common:actions.reload')}</Button>
          <Button as={Link} to="/" variant="secondary">
            {t('errorBoundary.backHome')}
          </Button>
        </div>
      </Card>
    </div>
  )
}

/**
 * 路由级错误边界：捕获渲染期异常（含懒加载路由 chunk 发版后 404 的加载失败），避免整页白屏。
 * 兜底卡片提供「重新加载」与「返回首页」两个恢复入口；
 * 路由切换需重置兜底态，由调用方用 key（如 pathname）重建实例实现。
 */
export class ErrorBoundary extends Component<ErrorBoundaryProps, ErrorBoundaryState> {
  state: ErrorBoundaryState = { error: null }

  static getDerivedStateFromError(error: Error): ErrorBoundaryState {
    return { error }
  }

  componentDidCatch(error: Error, info: ErrorInfo): void {
    // dev 下输出完整错误与组件栈便于定位；生产环境只展示兜底卡片
    if (import.meta.env.DEV) {
      console.error('[ErrorBoundary]', error, info)
    }
  }

  render() {
    const { error } = this.state
    if (!error) return this.props.children

    return <ErrorFallback message={error.message} />
  }
}
