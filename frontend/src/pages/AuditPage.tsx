/**
 * 审计日志页（E4）：筛选条（动作下拉（枚举硬编码 + datalist 自由输入）/操作者输入，
 * 条件同步 URL query）+ 审计表格 + 分页。数据：GET /api/audit?page&page_size&action&username
 * （created_at 倒序；后端仅记成功路径的管理操作，detail 不含敏感值）。
 */
import { ChevronDown, ChevronLeft, ChevronRight, Inbox, SearchX } from 'lucide-react'
import { useCallback, useEffect, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { useSearchParams } from 'react-router'

import { PageHeader } from '@/components/PageHeader'
import { QueryError, QueryLoading } from '@/components/QueryState'
import { RelativeTime } from '@/components/RelativeTime'
import { AuditActionBadge } from '@/components/badges'
import { Button } from '@/components/ui/Button'
import { Card } from '@/components/ui/Card'
import { EmptyState } from '@/components/ui/EmptyState'
import { Input, Select } from '@/components/ui/Input'
import { TD, TH, TBody, THead, TRow, Table, TableScroll } from '@/components/ui/Table'
import {
  AUDIT_ACTIONS,
  AUDIT_PAGE_SIZE,
  AUDIT_PAGE_SIZES,
  auditActionLabel,
  normalizeAuditPageSize,
  useAuditLogs,
} from '@/lib/audit'
import { useDebouncedValue } from '@/lib/logs'
import { cn } from '@/lib/utils'

const DEBOUNCE_MS = 300

/**
 * O33 表格移动端卡片化的单元格堆叠类：容器（TableScroll）宽 ≤600px 时每格变 flex 行——
 * ::before 以 data-col 注入对应表头文案靠左，内容靠右；解除表格态的挤压约束。
 * 表头文案必须与对应 <TH> 完全一致；桌面（>600px）不产生任何堆叠规则。
 */
const tdStack =
  '@max-[600px]:flex @max-[600px]:items-start @max-[600px]:justify-between @max-[600px]:gap-4 @max-[600px]:py-1.5 @max-[600px]:pl-5 @max-[600px]:pr-5 @max-[600px]:border-0 @max-[600px]:before:content-[attr(data-col)] @max-[600px]:before:text-xs @max-[600px]:before:leading-5 @max-[600px]:before:text-text-muted @max-[600px]:before:shrink-0'

/** URL query 中由 setFilter 管理的键 */
type FilterParamKey = 'action' | 'username' | 'page' | 'page_size'

/**
 * 详情单元格：有明细时可点击展开全量（明细 ≤500 字符，行内展开足够，无需弹窗）；
 * 无明细显示占位符。展开态由页面级 Set 管理，翻页/筛选后整体收起。
 */
function DetailCell({
  id,
  detail,
  expanded,
  onToggle,
}: {
  id: number
  detail: string
  expanded: boolean
  onToggle: (id: number) => void
}) {
  if (!detail) {
    return <span className="text-slate-300 dark:text-slate-600">—</span>
  }
  if (expanded) {
    return (
      <button
        type="button"
        onClick={() => onToggle(id)}
        aria-expanded
        className="flex w-full items-start gap-1 text-left"
      >
        <span className="whitespace-pre-wrap break-all text-xs text-slate-600 dark:text-slate-300">
          {detail}
        </span>
        <ChevronDown className="mt-0.5 h-3 w-3 shrink-0 text-slate-400" aria-hidden />
      </button>
    )
  }
  return (
    <button
      type="button"
      onClick={() => onToggle(id)}
      aria-expanded={false}
      title={detail}
      className="flex max-w-full items-center gap-1 text-left text-slate-600 transition-colors hover:text-slate-900 dark:text-slate-300 dark:hover:text-white"
    >
      <span className="truncate text-xs">{detail}</span>
      <ChevronRight className="h-3 w-3 shrink-0 text-slate-400" aria-hidden />
    </button>
  )
}

export function AuditPage() {
  const { t } = useTranslation(['audit', 'common'])
  const [searchParams, setSearchParams] = useSearchParams()

  // 筛选/分页全部来自 URL query（刷新、分享、前进后退不丢）
  const action = searchParams.get('action') ?? ''
  const username = searchParams.get('username') ?? ''
  const page = Math.max(1, Number.parseInt(searchParams.get('page') ?? '1', 10) || 1)
  const pageSize = normalizeAuditPageSize(searchParams.get('page_size'))

  const auditQuery = useAuditLogs({ page, pageSize, action, username })
  // 详情展开态：翻页/筛选后自动收起
  const [expanded, setExpanded] = useState<Set<number>>(new Set())
  // 页码跳转输入框（回车提交，提交后清空）
  const [pageInput, setPageInput] = useState('')

  // 动作/操作者输入本地态 → 300ms 防抖后写入 URL
  const [actionInput, setActionInput] = useState(action)
  const [usernameInput, setUsernameInput] = useState(username)
  const debouncedAction = useDebouncedValue(actionInput, DEBOUNCE_MS)
  const debouncedUsername = useDebouncedValue(usernameInput, DEBOUNCE_MS)

  // 函数式更新：始终基于最新 searchParams 派生下一状态，消除连续更新间的闭包竞态
  const updateParams = useCallback(
    (mutate: (params: URLSearchParams) => void) => {
      setSearchParams(
        (prev) => {
          const next = new URLSearchParams(prev)
          mutate(next)
          return next
        },
        { replace: true },
      )
    },
    [setSearchParams],
  )

  /** 设置单个筛选参数（空值 = 移除该参数）；筛选变化时重置回第一页 */
  const setFilter = useCallback(
    (key: FilterParamKey, value: string) => {
      updateParams((p) => {
        if (value) p.set(key, value)
        else p.delete(key)
        if (key !== 'page') p.delete('page')
      })
    },
    [updateParams],
  )

  useEffect(() => {
    const trimmed = debouncedAction.trim()
    if (trimmed !== action) setFilter('action', trimmed)
  }, [debouncedAction, action, setFilter])

  useEffect(() => {
    const trimmed = debouncedUsername.trim()
    if (trimmed !== username) setFilter('username', trimmed)
  }, [debouncedUsername, username, setFilter])

  // 外部 action/username 变化（前进/后退、分享链接）→ 回填输入框
  useEffect(() => setActionInput(action), [action])
  useEffect(() => setUsernameInput(username), [username])

  const items = auditQuery.data?.items ?? []
  const total = auditQuery.data?.total ?? 0
  const totalPages = Math.max(1, Math.ceil(total / pageSize))
  // 分页范围文案：第 21–40 条 · 共 142 条 · 第 2 / 7 页
  const rangeStart = (page - 1) * pageSize + 1
  const rangeEnd = Math.min(total, page * pageSize)
  const hasPrev = page > 1
  const hasNext = total > 0 ? page < totalPages : items.length >= pageSize
  const hasActiveFilter = Boolean(action || username)

  // URL 页码超界（如分享链接里 page 超过总页数）→ 自动回到第 1 页
  useEffect(() => {
    if (total > 0 && page > totalPages) setFilter('page', '1')
  }, [total, page, totalPages, setFilter])

  // 翻页/筛选后收起全部展开的详情
  useEffect(() => setExpanded(new Set()), [page, action, username])

  /** 展开态切换（同 id 再点收起） */
  const toggleDetail = useCallback((id: number) => {
    setExpanded((prev) => {
      const next = new Set(prev)
      if (next.has(id)) next.delete(id)
      else next.add(id)
      return next
    })
  }, [])

  /** 页码跳转：回车触发，越界收敛到 [1, totalPages]，非法输入忽略 */
  function handlePageJump() {
    const target = Number.parseInt(pageInput.trim(), 10)
    setPageInput('')
    if (!Number.isFinite(target)) return
    setFilter('page', String(Math.min(Math.max(target, 1), totalPages)))
  }

  /** 清空全部筛选，保留每页条数 */
  function clearFilters() {
    setActionInput('')
    setUsernameInput('')
    updateParams((p) => {
      p.delete('action')
      p.delete('username')
      p.delete('page')
    })
  }

  // O33：表头与移动端卡片 data-col 必须逐字一致，统一取自同一 t 值
  const colTime = t('common:fields.time')
  const colOperator = t('columns.operator')
  const colAction = t('columns.action')
  const colTarget = t('columns.target')
  const colDetail = t('common:fields.detail')
  const colIp = t('columns.ip')

  return (
    <div className="space-y-6">
      <PageHeader title={t('title')} description={t('description')} />

      {/* 筛选条：动作（下拉枚举 + 自由前缀输入）、操作者 */}
      <search className="flex flex-wrap items-center gap-3">
        {/* datalist 提供枚举快捷项；自由输入支持前缀（如 channel 命中全部渠道动作） */}
        <Input
          aria-label={t('filter.actionAria')}
          className="w-64"
          list="audit-action-options"
          placeholder={t('filter.actionPlaceholder')}
          value={actionInput}
          onChange={(e) => setActionInput(e.target.value)}
        />
        <datalist id="audit-action-options">
          {AUDIT_ACTIONS.map((a) => (
            <option key={a.value} value={a.value} />
          ))}
        </datalist>
        <Select
          aria-label={t('filter.actionQuickAria')}
          className="w-44"
          value={AUDIT_ACTIONS.some((a) => a.value === action) ? action : ''}
          onChange={(e) => {
            setActionInput(e.target.value)
            setFilter('action', e.target.value)
          }}
        >
          <option value="">{t('filter.allActions')}</option>
          {AUDIT_ACTIONS.map((a) => (
            <option key={a.value} value={a.value}>
              {t(a.labelKey)}
            </option>
          ))}
        </Select>
        <Input
          aria-label={t('filter.operatorAria')}
          className="w-48"
          placeholder={t('filter.operatorPlaceholder')}
          value={usernameInput}
          onChange={(e) => setUsernameInput(e.target.value)}
        />
        {hasActiveFilter && (
          <Button variant="secondary" className="h-9 px-3 text-xs" onClick={clearFilters}>
            {t('filter.clear')}
          </Button>
        )}
      </search>

      <Card>
        {auditQuery.isPending ? (
          <QueryLoading skeleton="table" />
        ) : auditQuery.isError ? (
          <QueryError message={auditQuery.error.message} onRetry={() => auditQuery.refetch()} />
        ) : items.length === 0 ? (
          hasActiveFilter ? (
            <EmptyState
              icon={SearchX}
              title={t('empty.filteredTitle')}
              description={t('empty.filteredDescription')}
              action={
                <Button variant="secondary" onClick={clearFilters}>
                  {t('filter.clear')}
                </Button>
              }
            />
          ) : (
            <EmptyState
              icon={Inbox}
              title={t('empty.allTitle')}
              description={t('empty.allDescription')}
            />
          )
        ) : (
          <TableScroll
            label={t('title')}
            className={cn(auditQuery.isPlaceholderData && 'opacity-60')}
          >
            {/* O33：容器 ≤600px 时折叠为卡片——表头隐藏、表格与行退块级；列名由每格 data-col 的 ::before 注入 */}
            <Table label={t('title')} className="@max-[600px]:block">
              <THead className="@max-[600px]:hidden">
                <tr>
                  <TH>{colTime}</TH>
                  <TH>{colOperator}</TH>
                  <TH>{colAction}</TH>
                  <TH>{colTarget}</TH>
                  <TH>{colDetail}</TH>
                  <TH>{colIp}</TH>
                </tr>
              </THead>
              {/* O33：堆叠态下 tbody 同步退块级（避免匿名表格包裹），并停用 divide——
                  行分隔改由 TRow 的 border-b 承担，防止相邻行双线 */}
              <TBody className="@max-[600px]:block @max-[600px]:divide-y-0">
                {items.map((item) => (
                  <TRow
                    key={item.id}
                    className="@max-[600px]:block @max-[600px]:border-b @max-[600px]:border-border-subtle @max-[600px]:px-5 @max-[600px]:py-2"
                  >
                    <TD data-col={colTime} className={tdStack}>
                      <RelativeTime iso={item.created_at} />
                    </TD>
                    <TD data-col={colOperator} className={tdStack}>
                      <span className="font-medium text-slate-900 dark:text-white">
                        {item.username}
                      </span>
                    </TD>
                    <TD data-col={colAction} className={tdStack}>
                      <AuditActionBadge action={item.action} label={auditActionLabel(item.action)} />
                    </TD>
                    <TD
                      data-col={colTarget}
                      className={cn(
                        'max-w-52',
                        tdStack,
                        // 堆叠态解除表格态的宽度挤压：整宽换行显示
                        '@max-[600px]:max-w-none @max-[600px]:whitespace-normal',
                      )}
                    >
                      <span
                        className="block truncate font-mono text-xs text-slate-600 dark:text-slate-300"
                        title={item.target}
                      >
                        {item.target}
                      </span>
                    </TD>
                    <TD
                      data-col={colDetail}
                      className={cn(
                        'max-w-64',
                        tdStack,
                        '@max-[600px]:max-w-none @max-[600px]:whitespace-normal',
                      )}
                    >
                      <DetailCell
                        id={item.id}
                        detail={item.detail}
                        expanded={expanded.has(item.id)}
                        onToggle={toggleDetail}
                      />
                    </TD>
                    <TD data-col={colIp} className={tdStack}>
                      <span className="font-mono text-xs text-slate-500 dark:text-slate-400">
                        {item.ip || '—'}
                      </span>
                    </TD>
                  </TRow>
                ))}
              </TBody>
            </Table>
          </TableScroll>
        )}

        {/* 分页：每页条数 + 页码跳转；O31：触屏（pointer-coarse）下控件提升到 44px 触摸目标，桌面密度不变 */}
        {!auditQuery.isPending && !auditQuery.isError && total > 0 && (
          <div className="flex flex-wrap items-center justify-between gap-3 border-t border-border-subtle px-5 py-3">
            <div className="flex flex-wrap items-center gap-3">
              <p className="text-xs tabular-nums text-slate-500 dark:text-slate-400">
                {t('pagination.range', {
                  start: rangeStart.toLocaleString(),
                  end: rangeEnd.toLocaleString(),
                  total: total.toLocaleString(),
                  page,
                  totalPages,
                })}
              </p>
              <label className="flex items-center gap-1.5 text-xs text-slate-500 dark:text-slate-400">
                {t('pagination.perPage')}
                <Select
                  aria-label={t('pagination.perPageAria')}
                  className="w-20 text-base sm:text-xs pointer-coarse:h-11"
                  value={String(pageSize)}
                  onChange={(e) =>
                    setFilter('page_size', e.target.value === String(AUDIT_PAGE_SIZE) ? '' : e.target.value)
                  }
                >
                  {AUDIT_PAGE_SIZES.map((size) => (
                    <option key={size} value={String(size)}>
                      {size}
                    </option>
                  ))}
                </Select>
              </label>
            </div>
            <div className="flex flex-wrap items-center gap-2">
              <label className="flex items-center gap-1.5 text-xs text-slate-500 dark:text-slate-400">
                {t('pagination.jumpTo')}
                <Input
                  aria-label={t('pagination.jumpAria')}
                  inputMode="numeric"
                  className="w-16 text-center text-base sm:text-xs pointer-coarse:h-11"
                  placeholder={t('pagination.pagePlaceholder')}
                  value={pageInput}
                  onChange={(e) => setPageInput(e.target.value)}
                  onKeyDown={(e) => {
                    // 中文等输入法组词期间确认候选的 Enter 属于 IME 操作（keyCode 229 为旧 Safari 兼容信号）：
                    // 不能触发跳页，否则打拼音选词会直接翻页
                    if (e.nativeEvent.isComposing || e.keyCode === 229) return
                    if (e.key === 'Enter') handlePageJump()
                  }}
                />
                {t('pagination.pageSuffix')}
              </label>
              <Button
                variant="secondary"
                className="h-8 px-3 text-xs pointer-coarse:h-11"
                disabled={!hasPrev}
                onClick={() => setFilter('page', String(page - 1))}
              >
                <ChevronLeft className="h-3.5 w-3.5" aria-hidden />
                {t('pagination.prev')}
              </Button>
              <Button
                variant="secondary"
                className="h-8 px-3 text-xs pointer-coarse:h-11"
                disabled={!hasNext}
                onClick={() => setFilter('page', String(page + 1))}
              >
                {t('pagination.next')}
                <ChevronRight className="h-3.5 w-3.5" aria-hidden />
              </Button>
            </div>
          </div>
        )}
      </Card>
    </div>
  )
}
