import { LoaderCircle } from 'lucide-react'
import type { ComponentPropsWithoutRef, ElementType } from 'react'

import { cn } from '@/lib/utils'

type ButtonVariant = 'primary' | 'secondary' | 'ghost' | 'danger'

export interface ButtonOwnProps {
  variant?: ButtonVariant
  /** 为 true 时禁用按钮并显示加载图标（仅原生 button 会在 loading 期间禁用） */
  loading?: boolean
}

/**
 * 多态按钮 props：类型随 as 推导——
 * 默认 button（ButtonHTMLAttributes）；as={Link} 时要求 Link 的 props（如 to）。
 */
export type ButtonProps<T extends ElementType = 'button'> = ButtonOwnProps & {
  /** 渲染的元素/组件（默认 button）：传 react-router Link 等可复用按钮样式 */
  as?: T
} & Omit<ComponentPropsWithoutRef<T>, keyof ButtonOwnProps | 'as'>

const variantClasses: Record<ButtonVariant, string> = {
  primary:
    'bg-sky-600 text-white shadow-sm shadow-sky-600/20 hover:bg-sky-700 focus-visible:ring-sky-500 disabled:bg-sky-300 dark:disabled:bg-sky-900 dark:disabled:text-slate-500',
  secondary:
    'glass-control border border-slate-300 bg-white text-slate-700 shadow-sm hover:bg-slate-50 focus-visible:ring-slate-400 dark:border-slate-700 dark:bg-slate-800 dark:text-slate-200 dark:hover:bg-slate-700',
  ghost:
    'text-slate-600 hover:bg-slate-100 hover:text-slate-900 focus-visible:ring-slate-400 dark:text-slate-400 dark:hover:bg-slate-800 dark:hover:text-slate-100',
  danger:
    'bg-red-600 text-white shadow-sm shadow-red-600/20 hover:bg-red-700 focus-visible:ring-red-500 disabled:bg-red-300',
}

const baseClasses =
  'inline-flex items-center justify-center gap-2 whitespace-nowrap rounded-lg px-3.5 py-2 text-sm font-medium transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-offset-2 disabled:cursor-not-allowed disabled:opacity-70 dark:focus-visible:ring-offset-slate-900'

export function Button<T extends ElementType = 'button'>({
  as,
  variant = 'primary',
  loading = false,
  className,
  children,
  ...rest
}: ButtonProps<T>) {
  const Component: ElementType = as ?? 'button'
  const isNativeButton = Component === 'button'
  // 泛型 rest 无法按 as 收窄：集中断言一次取原生 button 专属属性（不含 any）
  const nativeRest = rest as unknown as ComponentPropsWithoutRef<'button'>

  return (
    <Component
      className={cn(baseClasses, variantClasses[variant], className)}
      {...(rest as Record<string, unknown>)}
      {...(isNativeButton && {
        // 受控属性在 {...rest} 之后展开：调用方显式传 disabled={false} 也不会覆盖 loading 期间的禁用
        type: nativeRest.type ?? ('button' as const),
        disabled: nativeRest.disabled || loading,
        'aria-busy': loading || undefined,
      })}
    >
      {loading && <LoaderCircle className="h-4 w-4 animate-spin" aria-hidden />}
      {children}
    </Component>
  )
}
