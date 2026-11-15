import type { HTMLAttributes, Ref, TdHTMLAttributes, ThHTMLAttributes } from 'react'

import { cn } from '@/lib/utils'

/**
 * 管理后台统一表格底座（Geist/shadcn 风格）：
 * 表头弱化小字 + 底边框、行 hover、行分隔线（不用斑马纹）、单元格 padding；
 * 首尾列用 pl-5/pr-5 与卡片内边距对齐（内部统一 px-4）。
 */

export type TableProps = HTMLAttributes<HTMLTableElement> & {
  /** 可访问名：非空时渲染 sr-only caption（读屏先播表名，getByRole('table', { name }) 可定位） */
  label?: string
}

export function Table({ label, className, children, ...rest }: TableProps) {
  return (
    <table
      className={cn(
        'w-full text-sm [&_td:first-child]:pl-5 [&_td:last-child]:pr-5 [&_th:first-child]:pl-5 [&_th:last-child]:pr-5',
        className,
      )}
      {...rest}
    >
      {/* caption 须为 table 第一个子元素 */}
      {label ? <caption className="sr-only">{label}</caption> : null}
      {children}
    </table>
  )
}

export type TableScrollProps = HTMLAttributes<HTMLDivElement> & {
  /** region 可访问名（与内部 Table 的 label 同文案） */
  label: string
}

/**
 * 表格横向滚动容器：region 语义 + 键盘可聚焦（Tab 进入后可用方向键横向滚动）；
 * 焦点环走 focus-visible，鼠标点击不出现。
 * 同时是容器查询锚点（@container，O33）：容器宽 ≤600px 时页面侧把表格折叠为卡片布局，
 * 此时横向滚动让位（overflow-x-visible），桌面（>600px）行为不变。
 */
export function TableScroll({ label, className, children, ...rest }: TableScrollProps) {
  return (
    <div
      role="region"
      aria-label={label}
      tabIndex={0}
      className={cn(
        '@container overflow-x-auto focus-visible:outline-2 focus-visible:outline-offset-[-2px] focus-visible:outline-sky-500 @max-[600px]:overflow-x-visible',
        className,
      )}
      {...rest}
    >
      {children}
    </div>
  )
}

export type THeadProps = HTMLAttributes<HTMLTableSectionElement>

export function THead({ className, ...rest }: THeadProps) {
  return (
    <thead
      className={cn('border-b border-border-subtle text-left text-xs text-text-muted', className)}
      {...rest}
    />
  )
}

export type TBodyProps = HTMLAttributes<HTMLTableSectionElement>

export function TBody({ className, ...rest }: TBodyProps) {
  return <tbody className={cn('divide-y divide-border-subtle', className)} {...rest} />
}

export type TRowProps = HTMLAttributes<HTMLTableRowElement> & {
  /** 行引用（日志页 roving tabindex 键盘导航用；React 19 ref 即普通 prop） */
  ref?: Ref<HTMLTableRowElement>
}

export function TRow({ className, ...rest }: TRowProps) {
  return (
    <tr
      className={cn('transition-colors hover:bg-slate-50 dark:hover:bg-slate-800/50', className)}
      {...rest}
    />
  )
}

export interface THProps extends ThHTMLAttributes<HTMLTableCellElement> {
  /** 数字列表头：右对齐（与 TD numeric 对应） */
  numeric?: boolean
}

/** 表头单元格：继承 THead 的左对齐与弱化色；数字列传 numeric；默认 col 作用域（可覆盖） */
export function TH({ numeric = false, scope = 'col', className, ...rest }: THProps) {
  return (
    <th
      scope={scope}
      className={cn(
        'px-4 py-2.5 font-medium whitespace-nowrap',
        numeric && 'text-right',
        className,
      )}
      {...rest}
    />
  )
}

export interface TDProps extends TdHTMLAttributes<HTMLTableCellElement> {
  /** 数字列：右对齐 + tabular-nums + 不换行 */
  numeric?: boolean
}

export function TD({ numeric = false, className, ...rest }: TDProps) {
  return (
    <td
      className={cn(
        'px-4 py-3',
        numeric && 'text-right tabular-nums whitespace-nowrap',
        className,
      )}
      {...rest}
    />
  )
}
