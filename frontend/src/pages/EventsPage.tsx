/**
 * 送达事件页（送达链路三件套）：按渠道/事件类型/退信类型/收件人筛选 + 分页 +
 * 详情弹窗（raw 原始报文 JSON 格式化展示）。数据：GET /api/mail-events
 * （occurred_at 倒序）。风格对齐审计/发送日志页：URL 同步筛选、Skeleton 骨架、
 * 空态与分页组件同款。
 */
import { ChevronDown, ChevronLeft, ChevronRight, Inbox, SearchX } from 'lucide-react'
import { useCallback, useEffect, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { useSearchParams } from 'react-router'

import { PageHeader } from '@/components/PageHeader'
import { QueryError, QueryLoading } from '@/components/QueryState'
import { RelativeTime } from '@/components/RelativeTime'
import { Badge } from '@/components/badges'
import { Button } from '@/components/ui/Button'
import { Card } from '@/components/ui/Card'
import { EmptyState } from '@/components/ui/EmptyState'
import { Input, Select } from '@/components/ui/Input'
import { Modal } from '@/components/ui/Modal'
import { TD, TH, TBody, THead, TRow, Table, TableScroll } from '@/components/ui/Table'
import { useChannels } from '@/lib/channels'
import {
  MAIL_EVENT_LABELS,
  MAIL_EVENTS_PAGE_SIZES,
  MAIL_EVENTS_PAGE_SIZE,
  MAIL_EVENT_VARIANTS,
  normalizeMailEventPageSize,
  useMailEventDetail,
  useMailEvents,
  type MailBounceType,
  type MailEventType,
} from '@/lib/events'
import { useDebouncedValue } from '@/lib/logs'
import { cn } from '@/lib/utils'

const DEBOUNCE_MS = 300

/** URL query 中由 setFilter 管理的键 */
type FilterParamKey = 'event' | 'bounce_type' | 'channel_id' | 'q' | 'page' | 'page_size'

/** 事件徽章（含退信 hard/soft 细分） */
function MailEventBadge({ event, bounceType }: { event: string; bounceType: string }) {
  const { t } = useTranslation(['events'])
  const variant = MAIL_EVENT_VARIANTS[event as keyof typeof MAIL_EVENT_VARIANTS] ?? 'neutral'
  const labelKey = MAIL_EVENT_LABELS[event as keyof typeof MAIL_EVENT_LABELS]
  return (
    <Badge variant={variant}>
      {labelKey ? t(labelKey) : event}
      {event === 'bounced' && bounceType ? (
        <span className="font-mono text-[10px] opacity-80">
          {bounceType === 'hard' ? t('bounce.hard') : t('bounce.soft')}
        </span>
      ) : null}
    </Badge>
  )
}

export function EventsPage() {
  const { t } = useTranslation(['events', 'common'])
  const [searchParams, setSearchParams] = useSearchParams()

  // 筛选/分页全部来自 URL query（刷新、分享、前进后退不丢）
  const event = (searchParams.get('event') ?? '') as MailEventType
  const bounceType = (searchParams.get('bounce_type') ?? '') as MailBounceType
  const channelId = Number.parseInt(searchParams.get('channel_id') ?? '0', 10) || 0
  const q = searchParams.get('q') ?? ''
  const page = Math.max(1, Number.parseInt(searchParams.get('page') ?? '1', 10) || 1)
  const pageSize = normalizeMailEventPageSize(searchParams.get('page_size'))

  const eventsQuery = useMailEvents({ page, pageSize, event, bounceType, channelId, q })
  const channelsQuery = useChannels()
  // 详情弹窗（id=null 关闭）
  const [detailId, setDetailId] = useState<number | null>(null)
  const detailQuery = useMailEventDetail(detailId)
  // 页码跳转输入框（回车提交，提交后清空）
  const [pageInput, setPageInput] = useState('')

  // 收件人输入本地态 → 300ms 防抖后写入 URL
  const [qInput, setQInput] = useState(q)
  const debouncedQ = useDebouncedValue(qInput, DEBOUNCE_MS)

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
    const trimmed = debouncedQ.trim()
    if (trimmed !== q) setFilter('q', trimmed)
  }, [debouncedQ, q, setFilter])

  // 外部 q 变化（前进/后退、分享链接）→ 回填输入框
  useEffect(() => setQInput(q), [q])

  const items = eventsQuery.data?.items ?? []
  const total = eventsQuery.data?.total ?? 0
  const totalPages = Math.max(1, Math.ceil(total / pageSize))
  const rangeStart = (page - 1) * pageSize + 1
  const rangeEnd = Math.min(total, page * pageSize)
  const hasPrev = page > 1
  const hasNext = total > 0 ? page < totalPages : items.length >= pageSize
  const hasActiveFilter = Boolean(event || bounceType || channelId || q)

  // URL 页码超界（如分享链接里 page 超过总页数）→ 自动回到第 1 页
  useEffect(() => {
    if (total > 0 && page > totalPages) setFilter('page', '1')
  }, [total, page, totalPages, setFilter])

  /** 页码跳转：回车触发，越界收敛到 [1, totalPages]，非法输入忽略 */
  function handlePageJump() {
    const target = Number.parseInt(pageInput.trim(), 10)
    setPageInput('')
    if (!Number.isFinite(target)) return
    setFilter('page', String(Math.min(Math.max(target, 1), totalPages)))
  }

  /** 清空全部筛选，保留每页条数 */
  function clearFilters() {
    setQInput('')
    updateParams((p) => {
      p.delete('event')
      p.delete('bounce_type')
      p.delete('channel_id')
      p.delete('q')
      p.delete('page')
    })
  }

  /** raw 原始报文格式化展示（对象/数组 JSON.stringify 缩进；字符串原样） */
  const rawText = (() => {
    const raw = detailQuery.data?.raw
    if (raw === undefined || raw === null) return ''
    if (typeof raw === 'string') return raw
    try {
      return JSON.stringify(raw, null, 2)
    } catch {
      return String(raw)
    }
  })()

  return (
    <div className="space-y-6">
      <PageHeader title={t('page.title')} description={t('page.description')} />

      {/* 筛选条：事件类型 / 退信类型 / 渠道 / 收件人 */}
      <search className="flex flex-wrap items-center gap-3">
        <Select
          aria-label={t('filter.eventAria')}
          className="w-36"
          value={event}
          onChange={(e) => setFilter('event', e.target.value)}
        >
          <option value="">{t('filter.allEvents')}</option>
          <option value="sent">{t('type.sent')}</option>
          <option value="delivered">{t('type.delivered')}</option>
          <option value="bounced">{t('type.bounced')}</option>
          <option value="complained">{t('type.complained')}</option>
          <option value="raw">{t('type.raw')}</option>
        </Select>
        <Select
          aria-label={t('filter.bounceAria')}
          className="w-32"
          value={bounceType}
          onChange={(e) => setFilter('bounce_type', e.target.value)}
        >
          <option value="">{t('filter.allBounces')}</option>
          <option value="hard">{t('bounce.hardOption')}</option>
          <option value="soft">{t('bounce.softOption')}</option>
        </Select>
        <Select
          aria-label={t('filter.channelAria')}
          className="w-44"
          value={channelId > 0 ? String(channelId) : ''}
          onChange={(e) => setFilter('channel_id', e.target.value)}
        >
          <option value="">{t('filter.allChannels')}</option>
          {(channelsQuery.data?.items ?? []).map((ch) => (
            <option key={ch.id} value={String(ch.id)}>
              {ch.name}
            </option>
          ))}
        </Select>
        <Input
          aria-label={t('filter.recipientAria')}
          className="w-56"
          placeholder={t('filter.recipientPlaceholder')}
          value={qInput}
          onChange={(e) => setQInput(e.target.value)}
        />
        {hasActiveFilter && (
          <Button variant="secondary" className="h-9 px-3 text-xs" onClick={clearFilters}>
            {t('page.clearFilters')}
          </Button>
        )}
      </search>

      <Card>
        {eventsQuery.isPending ? (
          <QueryLoading skeleton="table" />
        ) : eventsQuery.isError ? (
          <QueryError message={eventsQuery.error.message} onRetry={() => eventsQuery.refetch()} />
        ) : items.length === 0 ? (
          hasActiveFilter ? (
            <EmptyState
              icon={SearchX}
              title={t('page.noMatchTitle')}
              description={t('page.noMatchDescription')}
              action={
                <Button variant="secondary" onClick={clearFilters}>
                  {t('page.clearFilters')}
                </Button>
              }
            />
          ) : (
            <EmptyState
              icon={Inbox}
              title={t('page.emptyTitle')}
              description={t('page.emptyDescription')}
            />
          )
        ) : (
          <TableScroll
            label={t('page.title')}
            className={cn(eventsQuery.isPlaceholderData && 'opacity-60')}
          >
            <Table label={t('page.title')} className="@max-[600px]:block">
              <THead className="@max-[600px]:hidden">
                <tr>
                  <TH>{t('columns.occurredAt')}</TH>
                  <TH>{t('common:fields.type')}</TH>
                  <TH>{t('columns.channel')}</TH>
                  <TH>{t('columns.recipient')}</TH>
                  <TH>{t('common:fields.detail')}</TH>
                  <TH>{t('columns.linkedLog')}</TH>
                </tr>
              </THead>
              <TBody className="@max-[600px]:block @max-[600px]:divide-y-0">
                {items.map((ev) => (
                  <TRow
                    key={ev.id}
                    className="cursor-pointer @max-[600px]:block @max-[600px]:border-b @max-[600px]:border-border-subtle @max-[600px]:px-5 @max-[600px]:py-2"
                    onClick={() => setDetailId(ev.id)}
                  >
                    <TdStack label={t('columns.occurredAt')}>
                      <RelativeTime iso={ev.occurred_at} />
                    </TdStack>
                    <TdStack label={t('common:fields.type')}>
                      <MailEventBadge event={ev.event} bounceType={ev.bounce_type} />
                    </TdStack>
                    <TdStack label={t('columns.channel')}>
                      <span className="font-medium text-slate-900 dark:text-white">
                        {ev.channel_name}
                      </span>
                      <span className="ml-1.5 font-mono text-[11px] text-slate-400">{ev.provider}</span>
                    </TdStack>
                    <TdStack label={t('columns.recipient')}>
                      <span className="block max-w-52 truncate font-mono text-xs" title={ev.recipient}>
                        {ev.recipient || '—'}
                      </span>
                    </TdStack>
                    <TdStack label={t('common:fields.detail')}>
                      <span className="block max-w-64 truncate text-xs text-slate-600 dark:text-slate-300" title={ev.detail}>
                        {ev.detail || '—'}
                      </span>
                    </TdStack>
                    <TdStack label={t('columns.linkedLog')}>
                      {ev.message_id ? (
                        <span
                          className="font-mono text-xs text-slate-500 dark:text-slate-400"
                          title={t('columns.linkedLogTitle', { messageId: ev.message_id })}
                        >
                          {ev.message_id.slice(0, 8)}…
                        </span>
                      ) : (
                        <span className="text-slate-300 dark:text-slate-600">—</span>
                      )}
                    </TdStack>
                  </TRow>
                ))}
              </TBody>
            </Table>
          </TableScroll>
        )}

        {/* 分页：每页条数 + 页码跳转（与审计页同款） */}
        {!eventsQuery.isPending && !eventsQuery.isError && total > 0 && (
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
                    setFilter(
                      'page_size',
                      e.target.value === String(MAIL_EVENTS_PAGE_SIZE) ? '' : e.target.value,
                    )
                  }
                >
                  {MAIL_EVENTS_PAGE_SIZES.map((size) => (
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
                    // IME 组词期间的 Enter 不触发跳页（与审计页同口径）
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

      {/* 详情弹窗：归一化字段 + raw 原始报文 */}
      <Modal
        open={detailId !== null}
        onClose={() => setDetailId(null)}
        title={t('detail.title')}
        className="max-w-2xl"
      >
        {detailQuery.isPending ? (
          <QueryLoading skeleton="block" className="h-40" />
        ) : detailQuery.isError ? (
          <QueryError message={detailQuery.error.message} onRetry={() => detailQuery.refetch()} />
        ) : detailQuery.data ? (
          <div className="space-y-4">
            <dl className="grid grid-cols-[5.5rem_1fr] gap-x-3 gap-y-2 text-sm">
              <dt className="text-slate-500 dark:text-slate-400">{t('common:fields.type')}</dt>
              <dd>
                <MailEventBadge event={detailQuery.data.event} bounceType={detailQuery.data.bounce_type} />
              </dd>
              <dt className="text-slate-500 dark:text-slate-400">{t('columns.channel')}</dt>
              <dd className="text-slate-900 dark:text-white">
                {detailQuery.data.channel_name}
                <span className="ml-1.5 font-mono text-xs text-slate-400">{detailQuery.data.provider}</span>
              </dd>
              <dt className="text-slate-500 dark:text-slate-400">{t('columns.recipient')}</dt>
              <dd className="break-all font-mono text-xs text-slate-700 dark:text-slate-300">
                {detailQuery.data.recipient || '—'}
              </dd>
              <dt className="text-slate-500 dark:text-slate-400">{t('detail.providerMessageId')}</dt>
              <dd className="break-all font-mono text-xs text-slate-700 dark:text-slate-300">
                {detailQuery.data.provider_message_id || '—'}
              </dd>
              <dt className="text-slate-500 dark:text-slate-400">{t('columns.occurredAt')}</dt>
              <dd>
                <RelativeTime iso={detailQuery.data.occurred_at} />
              </dd>
              <dt className="text-slate-500 dark:text-slate-400">{t('common:fields.detail')}</dt>
              <dd className="whitespace-pre-wrap break-all text-xs text-slate-600 dark:text-slate-300">
                {detailQuery.data.detail || '—'}
              </dd>
              {detailQuery.data.message_id ? (
                <>
                  <dt className="text-slate-500 dark:text-slate-400">{t('detail.sendLog')}</dt>
                  <dd className="font-mono text-xs text-slate-700 dark:text-slate-300">
                    {detailQuery.data.message_id}
                    {detailQuery.data.send_log_id > 0 ? (
                      <span className="ml-1 text-slate-400">
                        {t('detail.sendLogIdSuffix', { id: detailQuery.data.send_log_id })}
                      </span>
                    ) : null}
                  </dd>
                </>
              ) : null}
            </dl>
            {rawText ? (
              <div>
                <p className="mb-1.5 flex items-center gap-1 text-xs font-medium text-slate-500 dark:text-slate-400">
                  {t('detail.rawTitle')}
                  <ChevronDown className="h-3 w-3" aria-hidden />
                </p>
                <pre className="max-h-64 overflow-auto rounded-lg bg-slate-50 p-3 font-mono text-[11px] leading-relaxed break-all whitespace-pre-wrap text-slate-700 dark:bg-slate-900 dark:text-slate-300">
                  {rawText}
                </pre>
              </div>
            ) : null}
          </div>
        ) : null}
      </Modal>
    </div>
  )
}

/**
 * O33 移动端卡片化单元格堆叠（与审计页 tdStack 同款）：容器 ≤600px 时每格变
 * flex 行，::before 以 data-col 注入表头文案。
 */
function TdStack({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <TD
      data-col={label}
      className="@max-[600px]:flex @max-[600px]:items-start @max-[600px]:justify-between @max-[600px]:gap-4 @max-[600px]:py-1.5 @max-[600px]:pl-5 @max-[600px]:pr-5 @max-[600px]:border-0 @max-[600px]:before:content-[attr(data-col)] @max-[600px]:before:text-xs @max-[600px]:before:leading-5 @max-[600px]:before:text-text-muted @max-[600px]:before:shrink-0"
    >
      {children}
    </TD>
  )
}
