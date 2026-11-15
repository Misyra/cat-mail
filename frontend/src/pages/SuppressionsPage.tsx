/**
 * 抑制列表页（送达链路三件套）：命中列表的收件人在所有发送入口被拒
 * （400，code=recipient_suppressed）。按渠道/原因/邮箱筛选 + 分页 + 手动添加 +
 * 解除。bounce/complaint 行由 hard bounce 与 complained 事件按系统设置自动入库
 * （手动添加/解除走 POST/DELETE /api/suppressions，审计留痕）。
 */
import { Ban, ChevronLeft, ChevronRight, Inbox, Plus, SearchX, Trash2 } from 'lucide-react'
import { useCallback, useEffect, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { useSearchParams } from 'react-router'
import { toast } from 'sonner'

import { PageHeader } from '@/components/PageHeader'
import { QueryError, QueryLoading } from '@/components/QueryState'
import { RelativeTime } from '@/components/RelativeTime'
import { Badge, type BadgeVariant } from '@/components/badges'
import { Button } from '@/components/ui/Button'
import { Card } from '@/components/ui/Card'
import { EmptyState } from '@/components/ui/EmptyState'
import { Input, Select } from '@/components/ui/Input'
import { Modal } from '@/components/ui/Modal'
import { TD, TH, TBody, THead, TRow, Table, TableScroll } from '@/components/ui/Table'
import { useChannels } from '@/lib/channels'
import { useDebouncedValue } from '@/lib/logs'
import {
  SUPPRESSION_REASON_KEYS,
  SUPPRESSIONS_PAGE_SIZES,
  SUPPRESSIONS_PAGE_SIZE,
  createSuppression,
  deleteSuppression,
  invalidateSuppressions,
  normalizeSuppressionPageSize,
  useSuppressions,
  type SuppressionReason,
} from '@/lib/suppressions'
import { useQueryClient } from '@tanstack/react-query'
import { cn } from '@/lib/utils'

const DEBOUNCE_MS = 300

/** 原因 → 徽章语义色（退信/投诉=danger 手动=warning） */
function reasonVariant(reason: string): BadgeVariant {
  if (reason === 'bounce' || reason === 'complaint') return 'danger'
  if (reason === 'manual') return 'warning'
  return 'neutral'
}

/** 原因徽章 */
function ReasonBadge({ reason }: { reason: string }) {
  const { t } = useTranslation(['suppressions'])
  const key = SUPPRESSION_REASON_KEYS[reason as keyof typeof SUPPRESSION_REASON_KEYS]
  return <Badge variant={reasonVariant(reason)}>{key ? t(key) : reason}</Badge>
}

export function SuppressionsPage() {
  const [searchParams, setSearchParams] = useSearchParams()
  const queryClient = useQueryClient()
  const { t } = useTranslation(['suppressions', 'common'])

  const reason = (searchParams.get('reason') ?? '') as SuppressionReason
  const channelId = Number.parseInt(searchParams.get('channel_id') ?? '0', 10) || 0
  const q = searchParams.get('q') ?? ''
  const page = Math.max(1, Number.parseInt(searchParams.get('page') ?? '1', 10) || 1)
  const pageSize = normalizeSuppressionPageSize(searchParams.get('page_size'))

  const listQuery = useSuppressions({ page, pageSize, reason, channelId, q })
  const channelsQuery = useChannels()
  const [pageInput, setPageInput] = useState('')

  // 手动添加弹窗
  const [addOpen, setAddOpen] = useState(false)
  const [addEmail, setAddEmail] = useState('')
  const [addDetail, setAddDetail] = useState('')
  const [adding, setAdding] = useState(false)
  // 解除确认弹窗
  const [removing, setRemoving] = useState<{ id: number; email: string } | null>(null)
  const [deleting, setDeleting] = useState(false)

  // 邮箱输入本地态 → 300ms 防抖后写入 URL
  const [qInput, setQInput] = useState(q)
  const debouncedQ = useDebouncedValue(qInput, DEBOUNCE_MS)

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

  /** 设置单个筛选参数（空值 = 移除）；筛选变化时重置回第一页 */
  const setFilter = useCallback(
    (key: 'reason' | 'channel_id' | 'q' | 'page' | 'page_size', value: string) => {
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

  useEffect(() => setQInput(q), [q])

  const items = listQuery.data?.items ?? []
  const total = listQuery.data?.total ?? 0
  const totalPages = Math.max(1, Math.ceil(total / pageSize))
  const rangeStart = (page - 1) * pageSize + 1
  const rangeEnd = Math.min(total, page * pageSize)
  const hasPrev = page > 1
  const hasNext = total > 0 ? page < totalPages : items.length >= pageSize
  const hasActiveFilter = Boolean(reason || channelId || q)

  useEffect(() => {
    if (total > 0 && page > totalPages) setFilter('page', '1')
  }, [total, page, totalPages, setFilter])

  function handlePageJump() {
    const target = Number.parseInt(pageInput.trim(), 10)
    setPageInput('')
    if (!Number.isFinite(target)) return
    setFilter('page', String(Math.min(Math.max(target, 1), totalPages)))
  }

  function clearFilters() {
    setQInput('')
    updateParams((p) => {
      p.delete('reason')
      p.delete('channel_id')
      p.delete('q')
      p.delete('page')
    })
  }

  /** 手动添加提交（reason 由后端固定 manual） */
  async function handleAdd() {
    const email = addEmail.trim()
    if (!email) return
    setAdding(true)
    try {
      await createSuppression(email, addDetail.trim())
      toast.success(t('toast.added', { email }))
      setAddOpen(false)
      setAddEmail('')
      setAddDetail('')
      invalidateSuppressions(queryClient)
    } catch (err) {
      toast.error(err instanceof Error ? err.message : t('toast.addFailed'))
    } finally {
      setAdding(false)
    }
  }

  /** 解除（物理删除；之后 hard bounce/complaint 事件仍会自动重新入库） */
  async function handleRemove() {
    if (!removing) return
    setDeleting(true)
    try {
      await deleteSuppression(removing.id)
      toast.success(t('toast.removed', { email: removing.email }))
      setRemoving(null)
      invalidateSuppressions(queryClient)
    } catch (err) {
      toast.error(err instanceof Error ? err.message : t('toast.removeFailed'))
    } finally {
      setDeleting(false)
    }
  }

  return (
    <div className="space-y-6">
      <PageHeader
        title={t('page.title')}
        description={t('page.description')}
        actions={
          <Button onClick={() => setAddOpen(true)}>
            <Plus className="h-4 w-4" aria-hidden />
            {t('actions.add')}
          </Button>
        }
      />

      {/* 筛选条：原因 / 渠道 / 邮箱 */}
      <search className="flex flex-wrap items-center gap-3">
        <Select
          aria-label={t('filter.reasonAria')}
          className="w-36"
          value={reason}
          onChange={(e) => setFilter('reason', e.target.value)}
        >
          <option value="">{t('filter.allReasons')}</option>
          <option value="bounce">{t('reason.bounce')}</option>
          <option value="complaint">{t('reason.complaint')}</option>
          <option value="manual">{t('reason.manual')}</option>
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
          aria-label={t('filter.emailAria')}
          className="w-56"
          placeholder={t('filter.emailPlaceholder')}
          value={qInput}
          onChange={(e) => setQInput(e.target.value)}
        />
        {hasActiveFilter && (
          <Button variant="secondary" className="h-9 px-3 text-xs" onClick={clearFilters}>
            {t('empty.clearFilters')}
          </Button>
        )}
      </search>

      <Card>
        {listQuery.isPending ? (
          <QueryLoading skeleton="table" />
        ) : listQuery.isError ? (
          <QueryError message={listQuery.error.message} onRetry={() => listQuery.refetch()} />
        ) : items.length === 0 ? (
          hasActiveFilter ? (
            <EmptyState
              icon={SearchX}
              title={t('empty.filteredTitle')}
              description={t('empty.filteredDesc')}
              action={
                <Button variant="secondary" onClick={clearFilters}>
                  {t('empty.clearFilters')}
                </Button>
              }
            />
          ) : (
            <EmptyState
              icon={Inbox}
              title={t('empty.noRecordsTitle')}
              description={t('empty.noRecordsDesc')}
              action={
                <Button onClick={() => setAddOpen(true)}>
                  <Plus className="h-4 w-4" aria-hidden />
                  {t('actions.add')}
                </Button>
              }
            />
          )
        ) : (
          <TableScroll
            label={t('page.title')}
            className={cn(listQuery.isPlaceholderData && 'opacity-60')}
          >
            <Table label={t('page.title')} className="@max-[600px]:block">
              <THead className="@max-[600px]:hidden">
                <tr>
                  <TH>{t('fields.addedAt')}</TH>
                  <TH>{t('fields.email')}</TH>
                  <TH>{t('fields.reason')}</TH>
                  <TH>{t('fields.channel')}</TH>
                  <TH>{t('common:fields.description')}</TH>
                  <TH>{t('common:fields.actions')}</TH>
                </tr>
              </THead>
              <TBody className="@max-[600px]:block @max-[600px]:divide-y-0">
                {items.map((sp) => (
                  <TRow
                    key={sp.id}
                    className="@max-[600px]:block @max-[600px]:border-b @max-[600px]:border-border-subtle @max-[600px]:px-5 @max-[600px]:py-2"
                  >
                    <TdStack label={t('fields.addedAt')}>
                      <RelativeTime iso={sp.created_at} />
                    </TdStack>
                    <TdStack label={t('fields.email')}>
                      <span className="block max-w-56 truncate font-mono text-xs" title={sp.email}>
                        {sp.email}
                      </span>
                    </TdStack>
                    <TdStack label={t('fields.reason')}>
                      <ReasonBadge reason={sp.reason} />
                    </TdStack>
                    <TdStack label={t('fields.channel')}>
                      {sp.channel_name ? (
                        <span className="text-slate-700 dark:text-slate-300">{sp.channel_name}</span>
                      ) : (
                        <span className="text-slate-300 dark:text-slate-600">—</span>
                      )}
                    </TdStack>
                    <TdStack label={t('common:fields.description')}>
                      <span
                        className="block max-w-64 truncate text-xs text-slate-600 dark:text-slate-300"
                        title={sp.detail}
                      >
                        {sp.detail || '—'}
                      </span>
                    </TdStack>
                    <TdStack label={t('common:fields.actions')}>
                      <Button
                        variant="secondary"
                        className="h-8 px-2.5 text-xs text-red-600 hover:bg-red-50 dark:text-red-400 dark:hover:bg-red-500/10 pointer-coarse:h-11"
                        onClick={() => setRemoving({ id: sp.id, email: sp.email })}
                        aria-label={t('actions.removeAria', { email: sp.email })}
                      >
                        <Trash2 className="h-3.5 w-3.5" aria-hidden />
                        {t('actions.remove')}
                      </Button>
                    </TdStack>
                  </TRow>
                ))}
              </TBody>
            </Table>
          </TableScroll>
        )}

        {/* 分页（与送达事件页同款） */}
        {!listQuery.isPending && !listQuery.isError && total > 0 && (
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
                      e.target.value === String(SUPPRESSIONS_PAGE_SIZE) ? '' : e.target.value,
                    )
                  }
                >
                  {SUPPRESSIONS_PAGE_SIZES.map((size) => (
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

      {/* 手动添加弹窗 */}
      <Modal
        open={addOpen}
        onClose={() => setAddOpen(false)}
        title={t('addModal.title')}
        footer={
          <>
            <Button variant="secondary" onClick={() => setAddOpen(false)} disabled={adding}>
              {t('common:actions.cancel')}
            </Button>
            <Button onClick={handleAdd} disabled={adding || !addEmail.trim()}>
              {adding ? t('actions.adding') : t('actions.addSubmit')}
            </Button>
          </>
        }
      >
        <div className="space-y-3">
          <label className="block space-y-1.5">
            <span className="text-sm text-slate-700 dark:text-slate-300">{t('addModal.emailLabel')}</span>
            <Input
              type="email"
              placeholder="user@example.com"
              value={addEmail}
              onChange={(e) => setAddEmail(e.target.value)}
              aria-label={t('addModal.emailLabel')}
            />
          </label>
          <label className="block space-y-1.5">
            <span className="text-sm text-slate-700 dark:text-slate-300">{t('addModal.noteLabel')}</span>
            <Input
              placeholder={t('addModal.notePlaceholder')}
              value={addDetail}
              maxLength={500}
              onChange={(e) => setAddDetail(e.target.value)}
              aria-label={t('addModal.noteAria')}
            />
          </label>
          <p className="flex items-start gap-1.5 text-xs text-slate-500 dark:text-slate-400">
            <Ban className="mt-0.5 h-3.5 w-3.5 shrink-0" aria-hidden />
            {t('addModal.hint')}
          </p>
        </div>
      </Modal>

      {/* 解除确认弹窗 */}
      <Modal
        open={removing !== null}
        onClose={() => setRemoving(null)}
        title={t('removeModal.title')}
        footer={
          <>
            <Button variant="secondary" onClick={() => setRemoving(null)} disabled={deleting}>
              {t('common:actions.cancel')}
            </Button>
            <Button variant="danger" onClick={handleRemove} disabled={deleting}>
              {deleting ? t('actions.removing') : t('actions.remove')}
            </Button>
          </>
        }
      >
        <p className="text-sm text-slate-700 dark:text-slate-300">
          {t('removeModal.confirmPrefix')}{' '}
          <span className="font-mono text-xs">{removing?.email}</span>
          {t('removeModal.confirmSuffix')}
        </p>
        <p className="mt-2 text-xs text-slate-500 dark:text-slate-400">{t('removeModal.note')}</p>
      </Modal>
    </div>
  )
}

/**
 * O33 移动端卡片化单元格堆叠（与送达事件页同款）。
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
