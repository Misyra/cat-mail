import { X } from 'lucide-react'
import { useId, type ReactNode } from 'react'
import { useTranslation } from 'react-i18next'

import { useNativeDialog } from '@/lib/useNativeDialog'
import { cn } from '@/lib/utils'

export interface ModalProps {
  open: boolean
  onClose: () => void
  title: string
  children: ReactNode
  /** 底部操作区（确定/取消按钮等） */
  footer?: ReactNode
  className?: string
}

/**
 * 轻量模态框：原生 <dialog> 承载，showModal() 提供 top layer、背景 inert、
 * Esc / 点击遮罩关闭与焦点归还（开关与事件汇流见 useNativeDialog）。圆角面板；
 * 内容无 autofocus 时浏览器聚焦 dialog 自身，首个字段一次 Tab 可达。
 */
export function Modal({ open, onClose, title, children, footer, className }: ModalProps) {
  const dialogRef = useNativeDialog(open, onClose)
  const titleId = useId()
  // 数组形式（i18next 27 类型）：只用 common 的共享词汇
  const { t } = useTranslation(['common'])

  if (!open) return null

  return (
    <dialog
      ref={dialogRef}
      aria-labelledby={titleId}
      className={cn(
        'glass-surface app-dialog flex max-h-[85dvh] w-full max-w-lg flex-col rounded-2xl border border-border-subtle bg-surface-raised shadow-xl animate-pop-in',
        className,
      )}
    >
      <div className="flex shrink-0 items-center justify-between border-b border-slate-200 px-5 py-4 dark:border-slate-800">
        <h2 id={titleId} className="text-base font-semibold text-slate-900 dark:text-white">
          {title}
        </h2>
        <button
          type="button"
          onClick={onClose}
          aria-label={t('actions.close')}
          className="flex h-7 w-7 items-center justify-center rounded-lg text-slate-400 transition-colors hover:bg-slate-100 hover:text-slate-600 dark:hover:bg-slate-800 dark:hover:text-slate-200"
        >
          <X className="h-4 w-4" />
        </button>
      </div>
      <div className="min-h-0 flex-1 overflow-y-auto px-5 py-4">{children}</div>
      {footer && (
        <div className="flex shrink-0 justify-end gap-2 border-t border-slate-200 px-5 py-3 dark:border-slate-800">
          {footer}
        </div>
      )}
    </dialog>
  )
}
