import type { ReactNode } from 'react'

export interface PageHeaderProps {
  title: string
  description?: ReactNode
  /** 右侧操作区（新建按钮等） */
  actions?: ReactNode
}

/** 页面级标题区：大标题 + 描述 + 右侧操作插槽 */
export function PageHeader({ title, description, actions }: PageHeaderProps) {
  return (
    <div className="flex flex-wrap items-start justify-between gap-3">
      <div>
        <h1 className="text-2xl font-semibold tracking-tight text-slate-900 dark:text-white">
          {title}
        </h1>
        {description && (
          <p className="mt-1 text-sm text-slate-500 dark:text-slate-400">{description}</p>
        )}
      </div>
      {actions && <div className="flex items-center gap-2">{actions}</div>}
    </div>
  )
}
