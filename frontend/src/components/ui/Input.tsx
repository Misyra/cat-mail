import { Fragment, cloneElement, isValidElement, useId } from 'react'
import type {
  InputHTMLAttributes,
  ReactElement,
  ReactNode,
  SelectHTMLAttributes,
  TextareaHTMLAttributes,
} from 'react'

import { cn } from '@/lib/utils'

/** 移动端 16px 防 iOS 聚焦缩放，桌面保持 14px（O22）；glass-control 只在玻璃主题档生效
 * （半透明轻玻璃底，状态类 hover/disabled/focus 照常覆盖，见 index.css） */
export const inputClasses =
  'glass-control w-full rounded-lg border border-slate-300 bg-white px-3 py-2 text-base sm:text-sm text-slate-900 shadow-sm transition-colors placeholder:text-slate-500 focus:border-sky-500 focus:outline-none focus:ring-2 focus:ring-sky-500/30 disabled:cursor-not-allowed disabled:bg-slate-50 dark:border-slate-700 dark:bg-slate-800 dark:text-slate-100 dark:placeholder:text-slate-400 dark:disabled:bg-slate-900'

export interface InputProps extends InputHTMLAttributes<HTMLInputElement> {}

export function Input({ className, ...rest }: InputProps) {
  return <input className={cn(inputClasses, className)} {...rest} />
}

export interface TextareaProps extends TextareaHTMLAttributes<HTMLTextAreaElement> {}

export function Textarea({ className, ...rest }: TextareaProps) {
  return <textarea className={cn(inputClasses, 'min-h-20', className)} {...rest} />
}

export interface SelectProps extends SelectHTMLAttributes<HTMLSelectElement> {}

export function Select({ className, children, ...rest }: SelectProps) {
  return (
    <select className={cn(inputClasses, 'appearance-none pr-8', className)} {...rest}>
      {children}
    </select>
  )
}

export interface FieldProps {
  label: string
  children: ReactNode
  /** label 文字旁的附加元素（如动态状态徽章） */
  labelExtra?: ReactNode
  /** 底部灰色提示（无 error 时显示） */
  hint?: string
  /** 底部红色错误提示（优先于 hint） */
  error?: string
  className?: string
}

/** children 为单个合法元素（数组 / Fragment / 文本等降级为原样渲染）时才 clone 注入 id 与 aria-* */
function isInjectableElement(node: ReactNode): node is ReactElement<Record<string, unknown>> {
  return isValidElement(node) && node.type !== Fragment
}

/** label + 控件 + hint/error 的纵向表单字段：label 经 htmlFor 显式关联控件，错误以 role="alert" 播报 */
export function Field({ label, labelExtra, children, hint, error, className }: FieldProps) {
  const controlId = useId()
  const hintId = useId()
  const errorId = useId()

  // 单个合法元素时注入 id / aria-invalid / aria-describedby，让可访问名锚定在 label 文字上
  let htmlFor = controlId
  let control: ReactNode = children
  if (isInjectableElement(children)) {
    // 子元素自带 id 时尊重原值：htmlFor 指向它且不再注入
    const { id } = children.props
    if (typeof id === 'string') htmlFor = id
    control = cloneElement(children, {
      ...(typeof id === 'string' ? null : { id: controlId }),
      'aria-invalid': error ? true : undefined,
      'aria-describedby': error ? errorId : hint ? hintId : undefined,
    })
  }

  return (
    <div className={cn('block', className)}>
      <label
        htmlFor={htmlFor}
        className="mb-1.5 flex items-center gap-2 text-sm font-medium text-slate-700 dark:text-slate-300"
      >
        {label}
        {labelExtra}
      </label>
      {control}
      {error ? (
        <span id={errorId} role="alert" className="mt-1 block text-xs text-red-500">
          {error}
        </span>
      ) : hint ? (
        <span id={hintId} className="mt-1 block text-xs text-slate-500 dark:text-slate-400">
          {hint}
        </span>
      ) : null}
    </div>
  )
}
