/**
 * ⌘K 命令面板（手写实现，shadcn Command 模式，无第三方依赖）：
 * 原生 <dialog> 承载，showModal() 提供 top layer、背景 inert、Esc 关闭与焦点归还（见 useNativeDialog）。
 * 输入即搜（includes 匹配），分组列表（导航 / 操作），↑↓ 循环选择、Enter 执行；
 * 输入框以 combobox 语义接管全部键盘操作，结果条目为 listbox 的 option（不可聚焦），
 * 高亮项经 aria-activedescendant 播报。
 */
import { LogOut, Search, type LucideIcon } from 'lucide-react'
import { useEffect, useId, useMemo, useRef, useState } from 'react'
import { useNavigate } from 'react-router'
import { useTranslation } from 'react-i18next'

import { NAV_ITEMS } from '@/components/layout/nav'
import { useNativeDialog } from '@/lib/useNativeDialog'
import { THEME_META, useTheme } from '@/lib/theme'
import { cn } from '@/lib/utils'

export interface CommandPaletteProps {
  open: boolean
  onClose: () => void
  /** 退出登录（由 AppLayout 传入，登录态/错误处理保持单一来源） */
  onLogout: () => void
  loggingOut?: boolean
}

interface CommandItem {
  icon: LucideIcon
  label: string
  /** 额外匹配关键词（路径、英文别名等） */
  keywords?: string
  run: () => void
  disabled?: boolean
}

interface CommandGroup {
  heading: string
  items: CommandItem[]
}

/** 带扁平序号的条目（键盘 ↑↓ / Enter 按扁平序号定位，跨分组连续移动） */
interface RenderEntry {
  item: CommandItem
  index: number
}

interface RenderGroup {
  heading: string
  entries: RenderEntry[]
}

export function CommandPalette({ open, onClose, onLogout, loggingOut }: CommandPaletteProps) {
  const navigate = useNavigate()
  const { theme, cycleTheme } = useTheme()
  // 数组形式：nav label 存的是带 ns 前缀的 key，且「操作」分组标题复用 common 词汇
  const { t } = useTranslation(['layout', 'common'])
  const dialogRef = useNativeDialog(open, onClose)
  const listboxId = useId()
  const [query, setQuery] = useState('')
  const [active, setActive] = useState(0)
  const itemRefs = useRef<Array<HTMLDivElement | null>>([])

  const themeMeta = THEME_META[theme]

  const groups = useMemo<CommandGroup[]>(
    () => [
      {
        heading: t('palette.navGroup'),
        items: NAV_ITEMS.map((item) => ({
          icon: item.icon,
          label: t(item.label),
          keywords: item.to,
          run: () => {
            navigate(item.to)
            onClose()
          },
        })),
      },
      {
        heading: t('common:fields.actions'),
        items: [
          {
            icon: themeMeta.icon,
            label: t('palette.switchTheme', { current: t(themeMeta.labelKey) }),
            keywords: t('palette.themeKeywords'),
            run: cycleTheme,
          },
          {
            icon: LogOut,
            label: t('sidebar.logout'),
            keywords: t('palette.logoutKeywords'),
            run: onLogout,
            disabled: loggingOut,
          },
        ],
      },
    ],
    [navigate, onClose, themeMeta, cycleTheme, onLogout, loggingOut, t],
  )

  const keyword = query.trim().toLowerCase()
  // 过滤 + 分配跨分组的扁平序号（includes 匹配 label 与 keywords）
  const filteredGroups = useMemo<RenderGroup[]>(() => {
    let index = 0
    return groups
      .map((group) => ({
        heading: group.heading,
        entries: group.items
          .filter(
            (item) =>
              !keyword ||
              item.label.toLowerCase().includes(keyword) ||
              (item.keywords ?? '').toLowerCase().includes(keyword),
          )
          .map((item) => ({ item, index: index++ })),
      }))
      .filter((group) => group.entries.length > 0)
  }, [groups, keyword])
  const flatEntries = useMemo(() => filteredGroups.flatMap((g) => g.entries), [filteredGroups])

  // 打开时重置搜索与选中
  useEffect(() => {
    if (open) {
      setQuery('')
      setActive(0)
    }
  }, [open])
  // 搜索词（过滤条件）变化时把选中重置到第一项：
  // 保证 Enter 执行的就是当前高亮项，不会命中被过滤掉的旧索引（越界兜底见下方 activeIndex 夹紧）
  useEffect(() => {
    setActive(0)
  }, [keyword])

  // 双保险：状态更新与列表变化之间的间隙内夹紧选中索引
  const activeIndex = flatEntries.length > 0 ? Math.min(active, flatEntries.length - 1) : 0

  // 键盘移动时把选中项滚进可视区
  useEffect(() => {
    itemRefs.current[activeIndex]?.scrollIntoView({ block: 'nearest' })
  }, [activeIndex])

  function runEntry(entry: RenderEntry) {
    if (entry.item.disabled) return
    onClose()
    entry.item.run()
  }

  if (!open) return null

  return (
    <dialog
      ref={dialogRef}
      id="command-palette"
      aria-label={t('palette.label')}
      className="glass-surface app-dialog mx-auto flex w-full max-w-xl flex-col overflow-hidden rounded-2xl border border-border-subtle bg-surface-raised shadow-xl animate-pop-in"
    >
      {/* 搜索输入：combobox 模式，键盘操作全部在这里处理，结果项经 id 联动播报 */}
      <div className="flex items-center gap-2.5 border-b border-slate-200 px-4 dark:border-slate-800">
        <Search className="h-4 w-4 shrink-0 text-slate-400" aria-hidden />
        <input
          value={query}
          onChange={(e) => setQuery(e.target.value)}
          onKeyDown={(e) => {
            // 中文等输入法组词期间的方向键/Enter 属于 IME 候选操作（keyCode 229 为旧 Safari 兼容信号）：
            // 不能当作面板快捷键，否则「确认候选」的 Enter 会执行当前高亮项
            if (e.nativeEvent.isComposing || e.keyCode === 229) return
            if (e.key === 'ArrowDown') {
              e.preventDefault()
              if (flatEntries.length > 0) setActive((i) => (i + 1) % flatEntries.length)
            } else if (e.key === 'ArrowUp') {
              e.preventDefault()
              if (flatEntries.length > 0)
                setActive((i) => (i - 1 + flatEntries.length) % flatEntries.length)
            } else if (e.key === 'Enter') {
              e.preventDefault()
              const entry = flatEntries[activeIndex]
              if (entry) runEntry(entry)
            }
          }}
          placeholder={t('palette.searchPlaceholder')}
          aria-label={t('palette.searchAria')}
          role="combobox"
          aria-expanded={flatEntries.length > 0}
          aria-controls={listboxId}
          aria-autocomplete="list"
          aria-activedescendant={
            flatEntries.length > 0 ? `${listboxId}-opt-${activeIndex}` : undefined
          }
          autoFocus
          className="h-12 w-full bg-transparent text-sm text-slate-900 placeholder:text-slate-500 focus:outline-none dark:text-white dark:placeholder:text-slate-400"
        />
        <kbd className="rounded border border-slate-200 bg-slate-50 px-1.5 py-0.5 text-[11px] font-medium text-slate-500 dark:border-slate-700 dark:bg-slate-800 dark:text-slate-400">
          Esc
        </kbd>
      </div>

      {/* 分组结果：无结果时空态不进 listbox（容器只在有结果时才是 listbox） */}
      <div
        id={listboxId}
        role={flatEntries.length > 0 ? 'listbox' : undefined}
        className="max-h-80 overflow-y-auto p-2"
      >
        {flatEntries.length === 0 ? (
          <p className="px-3 py-10 text-center text-sm text-slate-500 dark:text-slate-400">
            {t('palette.noResults')}
          </p>
        ) : (
          filteredGroups.map((group, groupIndex) => (
            <div
              key={group.heading}
              role="group"
              aria-labelledby={`${listboxId}-grp-${groupIndex}`}
              className="mb-1 last:mb-0"
            >
              <p
                id={`${listboxId}-grp-${groupIndex}`}
                className="px-3 pb-1 pt-2 text-xs font-semibold uppercase tracking-wide text-slate-500 dark:text-slate-400"
              >
                {group.heading}
              </p>
              {group.entries.map(({ item, index }) => (
                <div
                  key={item.label}
                  id={`${listboxId}-opt-${index}`}
                  ref={(el) => {
                    itemRefs.current[index] = el
                  }}
                  role="option"
                  aria-selected={index === activeIndex}
                  aria-disabled={item.disabled || undefined}
                  onClick={() => runEntry({ item, index })}
                  onMouseEnter={() => setActive(index)}
                  className={cn(
                    'flex w-full items-center gap-2.5 rounded-lg px-3 py-2 text-left text-sm transition-colors',
                    index === activeIndex
                      ? 'bg-sky-50 text-sky-700 dark:bg-sky-500/10 dark:text-sky-400'
                      : 'text-slate-600 dark:text-slate-300',
                    item.disabled && 'cursor-not-allowed opacity-50',
                  )}
                >
                  <item.icon className="h-4 w-4 shrink-0" aria-hidden />
                  <span className="truncate">{item.label}</span>
                </div>
              ))}
            </div>
          ))
        )}
      </div>

      {/* 快捷键提示 */}
      <div className="flex items-center gap-4 border-t border-slate-200 px-4 py-2 text-[11px] text-slate-500 dark:border-slate-800 dark:text-slate-400">
        <span>{t('palette.selectHint')}</span>
        <span>{t('palette.runHint')}</span>
        <span>{t('palette.closeHint')}</span>
      </div>
    </dialog>
  )
}
