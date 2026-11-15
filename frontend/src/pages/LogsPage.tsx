/**
 * 发送日志页（SPEC §6）：筛选条（状态/渠道/关键词防抖 + 排序/实时/导出，条件同步 URL query）
 * + 日志表格 + 分页（每页条数 / 页码跳转）+ 详情抽屉（全部字段 + 错误信息 + 正文预览 tab）。
 * 数据：GET /api/logs?page&page_size&status&channel_id&q&sort、GET /api/logs/:id、
 *       GET /api/logs/export（CSV 下载流）（SPEC §5，snake_case 原样字段名）。
 */
import { ChevronLeft, ChevronRight, Copy, Download, Inbox, Radio, RotateCcw, SearchX, X } from 'lucide-react'
import {
  useCallback,
  useEffect,
  useRef,
  useState,
  type ReactNode,
} from 'react'
import { useTranslation } from 'react-i18next'
import i18next from 'i18next'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { useSearchParams } from 'react-router'
import { toast } from 'sonner'

import { ApiError } from '@/lib/api'
import { PageHeader } from '@/components/PageHeader'
import { QueryError, QueryLoading } from '@/components/QueryState'
import { RelativeTime } from '@/components/RelativeTime'
import { Badge, LogStatusBadge, ProviderBadge } from '@/components/badges'
import { Button } from '@/components/ui/Button'
import { Card } from '@/components/ui/Card'
import { EmptyState } from '@/components/ui/EmptyState'
import { Input, Select } from '@/components/ui/Input'
import { Modal } from '@/components/ui/Modal'
import { TD, TH, TBody, THead, TRow, Table, TableScroll } from '@/components/ui/Table'
import { useNativeDialog } from '@/lib/useNativeDialog'
import { useChannels } from '@/lib/channels'
import { useTemplates } from '@/lib/templates'
import { copyText as copyToClipboard } from '@/lib/clipboard'
import { formatDateTime, formatRelativeTime } from '@/lib/format'
import {
  EXPORT_ERROR_KEYS,
  LOGS_PAGE_SIZE,
  LOGS_PAGE_SIZES,
  downloadLogsCsv,
  normalizePageSize,
  normalizeSort,
  parseToAddrs,
  resendLog,
  useDebouncedValue,
  useLogDetail,
  useLogs,
  type LogStatusFilter,
} from '@/lib/logs'
import { cn } from '@/lib/utils'

const DEBOUNCE_MS = 300

/**
 * O33 表格移动端卡片化的单元格堆叠类：容器（TableScroll）宽 ≤600px 时每格变 flex 行——
 * ::before 以 data-col 注入对应表头文案靠左，内容靠右；解除表格态的挤压约束。
 * 表头文案必须与对应 <TH> 完全一致；桌面（>600px）不产生任何堆叠规则。
 */
const tdStack =
  '@max-[600px]:flex @max-[600px]:items-start @max-[600px]:justify-between @max-[600px]:gap-4 @max-[600px]:py-1.5 @max-[600px]:pl-5 @max-[600px]:pr-5 @max-[600px]:border-0 @max-[600px]:before:content-[attr(data-col)] @max-[600px]:before:text-xs @max-[600px]:before:leading-5 @max-[600px]:before:text-text-muted @max-[600px]:before:shrink-0'

/**
 * 复制 Message ID 等长串字段到剪贴板（共享降级实现），成功/失败均有 toast 反馈。
 * 非组件上下文拿不到 hook，运行时经 i18next 单例取译文。
 */
async function copyText(text: string) {
  try {
    await copyToClipboard(text)
    toast.success(i18next.t('common:actions.copied'))
  } catch {
    toast.error(i18next.t('logs:toast.copyFailedManual'))
  }
}

/** URL query 中由 setFilter 管理的键 */
type FilterParamKey = 'status' | 'channel_id' | 'q' | 'page' | 'sort' | 'page_size'

/* ---------------------------------- 通用小部件 ---------------------------------- */

/** 收件人单元格：首地址 + 「+N」徽章，悬停展示全部 */
function RecipientsCell({ addrs }: { addrs: string[] }) {
  if (addrs.length === 0) {
    return <span className="text-slate-500 dark:text-slate-400">—</span>
  }
  const rest = addrs.length - 1
  return (
    <span
      className="flex max-w-44 items-center gap-1 text-slate-600 dark:text-slate-300"
      title={addrs.join('、')}
    >
      <span className="truncate">{addrs[0]}</span>
      {rest > 0 && (
        <Badge variant="neutral" className="shrink-0 px-1.5 py-0 text-[10px]">
          +{rest}
        </Badge>
      )}
    </span>
  )
}

/** 详情抽屉的 label/value 行 */
function DetailRow({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="flex gap-3 py-1.5 text-sm">
      <span className="w-32 shrink-0 text-slate-500 dark:text-slate-400">{label}</span>
      <span className="min-w-0 flex-1 break-words text-slate-700 dark:text-slate-200">
        {children}
      </span>
    </div>
  )
}

function BodyTabButton({
  active,
  onClick,
  children,
}: {
  active: boolean
  onClick: () => void
  children: ReactNode
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      className={cn(
        'rounded-lg px-3 py-1 text-xs font-medium transition-colors',
        active
          ? 'bg-sky-600 text-white shadow-sm'
          : 'bg-slate-100 text-slate-600 hover:bg-slate-200 dark:bg-slate-800 dark:text-slate-300 dark:hover:bg-slate-700',
      )}
    >
      {children}
    </button>
  )
}

/**
 * 模板引用（邮件模板功能包）：日志详情显示所用邮件模板名称（经 /api/templates
 * 解析）；模板已删除后仅余 ID，标注「已删除」。
 */
function TemplateRef({ id }: { id: number }) {
  const { t } = useTranslation(['logs', 'common'])
  const templatesQuery = useTemplates()
  const name = templatesQuery.data?.items.find((tpl) => tpl.id === id)?.name
  return (
    <span className="inline-flex items-center gap-1.5">
      <Badge variant="info">{name ?? t('detail.templateFallback', { id })}</Badge>
      {!name && <span className="text-xs text-slate-400 dark:text-slate-500">{t('detail.templateDeleted')}</span>}
    </span>
  )
}

/* ---------------------------------- 详情停靠面板 ---------------------------------- */

/**
 * 日志详情右侧停靠面板（Supabase Logs 风格）：原生 <dialog> 全高停靠右侧，宽 min(520px, 90vw)，
 * <lg 时占满全屏；::backdrop 点击可关闭，Esc 关闭；打开期间主列表仍隐约可见，便于连续对比。
 * 开关与事件汇流复用 useNativeDialog（组件条件挂载，open 恒 true）。
 * 「重发」操作（E3）：按原内容重新发送并走轮换，红色系弱化按钮 + 确认弹窗；
 * 确认弹窗是另一个原生 modal dialog，top layer 自动叠放，Esc 只关最上层，关闭后焦点自动归还。
 */
function LogDetailPanel({ id, onClose }: { id: number; onClose: () => void }) {
  const { t } = useTranslation(['logs', 'common'])
  const queryClient = useQueryClient()
  const detailQuery = useLogDetail(id)
  const [bodyTab, setBodyTab] = useState<'html' | 'text'>('html')
  const [resendConfirm, setResendConfirm] = useState(false)
  const dialogRef = useNativeDialog(true, onClose)

  const detail = detailQuery.data
  const htmlBody = detail?.html_body ?? ''
  const textBody = detail?.text_body ?? ''
  const hasHtml = htmlBody.trim().length > 0
  const hasText = textBody.trim().length > 0
  // 期望 tab 无内容时回落到另一个（只存了一种正文的情况）
  const activeTab = bodyTab === 'html' && hasHtml ? 'html' : 'text'
  // 未记录正文（隐私开关历史/为空）时后端必然 400，不展示重发入口
  const canResend = hasHtml || hasText

  const resendMutation = useMutation({
    mutationFn: () => resendLog(id),
    onSuccess: (res) => {
      // 重发会落一条新日志：失效日志列表/详情缓存（新 message_id 出现在列表顶部）
      void queryClient.invalidateQueries({ queryKey: ['logs'] })
      toast.success(t('toast.resendSuccess', { messageId: res.id }))
      setResendConfirm(false)
    },
    onError: (err) => toast.error(err.message),
  })

  return (
    <>
      <dialog
        ref={dialogRef}
        aria-label={t('detail.title')}
        className="glass-surface app-dialog app-dock flex w-full flex-col border-l border-border-subtle bg-surface-raised shadow-xl animate-dock-in-right max-w-none max-h-none h-dvh m-0 ml-auto lg:w-[min(520px,90vw)]"
      >
        <div className="flex shrink-0 items-center justify-between border-b border-slate-200 px-5 py-4 dark:border-slate-800">
          <h2 className="text-base font-semibold text-slate-900 dark:text-white">{t('detail.title')}</h2>
          <div className="flex items-center gap-1">
            {canResend && (
              <button
                type="button"
                onClick={() => setResendConfirm(true)}
                disabled={resendMutation.isPending}
                title={t('detail.resendTitle')}
                className="inline-flex h-7 items-center gap-1 whitespace-nowrap rounded-lg px-2 text-xs text-red-600 transition-colors hover:bg-red-50 hover:text-red-700 disabled:cursor-not-allowed disabled:opacity-60 dark:text-red-400 dark:hover:bg-red-500/10 dark:hover:text-red-300"
              >
                <RotateCcw className="h-3.5 w-3.5" aria-hidden />
                {t('detail.resend')}
              </button>
            )}
            <button
              type="button"
              onClick={onClose}
              aria-label={t('common:actions.close')}
              className="flex h-7 w-7 items-center justify-center rounded-lg text-slate-400 transition-colors hover:bg-slate-100 hover:text-slate-600 dark:hover:bg-slate-800 dark:hover:text-slate-200"
            >
              <X className="h-4 w-4" />
            </button>
          </div>
        </div>
        <div className="min-h-0 flex-1 overflow-y-auto px-5 py-4">
          {detailQuery.isPending ? (
        <QueryLoading skeleton="block" />
      ) : detailQuery.isError ? (
        <QueryError message={detailQuery.error.message} onRetry={() => detailQuery.refetch()} />
      ) : detail ? (
        <div className="space-y-5">
          {/* 全部字段 */}
          <div className="rounded-lg border border-slate-200 px-4 py-2 dark:border-slate-800">
            <div className="divide-y divide-slate-100 dark:divide-slate-800">
              <DetailRow label={t('common:fields.time')}>
                <span title={formatRelativeTime(detail.created_at)}>
                  {formatDateTime(detail.created_at, true)}
                </span>
              </DetailRow>
              <DetailRow label={t('common:fields.status')}>
                <LogStatusBadge status={detail.status} />
              </DetailRow>
              <DetailRow label={t('common:fields.type')}>
                <Badge variant={detail.is_test ? 'info' : 'neutral'}>
                  {detail.is_test ? t('detail.testSend') : t('detail.normalSend')}
                </Badge>
              </DetailRow>
              <DetailRow label={t('fields.channel')}>{detail.channel_name}</DetailRow>
              <DetailRow label={t('fields.platform')}>
                <ProviderBadge provider={detail.provider} />
              </DetailRow>
              <DetailRow label={t('fields.from')}>{detail.from_addr || '—'}</DetailRow>
              <DetailRow label={t('fields.to')}>
                {parseToAddrs(detail.to_addrs).join('、') || '—'}
              </DetailRow>
              <DetailRow label={t('fields.subject')}>{detail.subject || t('detail.noSubject')}</DetailRow>
              {detail.template_id > 0 && (
                <DetailRow label={t('fields.template')}>
                  <TemplateRef id={detail.template_id} />
                </DetailRow>
              )}
              <DetailRow label={t('fields.latency')}>
                <span className="tabular-nums">{detail.latency_ms} ms</span>
                <span className="ml-2 text-xs text-slate-500 dark:text-slate-400">
                  {t('detail.attempts', { count: detail.attempts })}
                </span>
              </DetailRow>
              <DetailRow label={t('detail.messageId')}>
                <span className="flex items-center gap-1.5">
                  <code className="break-all font-mono text-xs">{detail.message_id}</code>
                  {detail.message_id && (
                    <button
                      type="button"
                      onClick={() => void copyText(detail.message_id)}
                      title={t('detail.copyMessageId')}
                      aria-label={t('detail.copyMessageId')}
                      className="shrink-0 rounded-md p-1 text-slate-400 transition-colors hover:bg-slate-100 hover:text-slate-600 dark:hover:bg-slate-800 dark:hover:text-slate-300"
                    >
                      <Copy className="h-3.5 w-3.5" aria-hidden />
                    </button>
                  )}
                </span>
              </DetailRow>
              <DetailRow label={t('detail.providerMessageId')}>
                {detail.provider_message_id ? (
                  <code className="break-all font-mono text-xs">{detail.provider_message_id}</code>
                ) : (
                  '—'
                )}
              </DetailRow>
              <DetailRow label={t('detail.apiKeyId')}>
                {detail.api_key_id > 0 ? (
                  <span className="tabular-nums">{detail.api_key_id}</span>
                ) : (
                  '—'
                )}
              </DetailRow>
            </div>
          </div>

          {/* 错误信息（红色块） */}
          {detail.error && (
            <div className="rounded-lg border border-red-200 bg-red-50 p-3 dark:border-red-500/30 dark:bg-red-500/10">
              <p className="text-xs font-semibold text-red-600 dark:text-red-400">{t('common:fields.error')}</p>
              <p className="mt-1 whitespace-pre-wrap break-words text-xs leading-relaxed text-red-600 dark:text-red-400">
                {detail.error}
              </p>
            </div>
          )}

          {/* 正文预览 tab */}
          <section>
            <h3 className="mb-2 text-xs font-semibold uppercase tracking-wide text-slate-500 dark:text-slate-400">
              {t('detail.bodyPreview')}
            </h3>
            {!hasHtml && !hasText ? (
              <div className="rounded-lg border border-dashed border-slate-300 px-4 py-6 text-center text-xs text-slate-500 dark:border-slate-700 dark:text-slate-400">
                {t('detail.noBody')}
              </div>
            ) : (
              <>
                <div className="mb-2 flex gap-1.5">
                  {hasHtml && (
                    <BodyTabButton active={activeTab === 'html'} onClick={() => setBodyTab('html')}>
                      HTML
                    </BodyTabButton>
                  )}
                  {hasText && (
                    <BodyTabButton active={activeTab === 'text'} onClick={() => setBodyTab('text')}>
                      {t('detail.textBody')}
                    </BodyTabButton>
                  )}
                </div>
                {activeTab === 'html' ? (
                  // sandbox="" 完全隔离：不执行脚本、不加载外部表单，安全渲染邮件 HTML；
                  // no-referrer 避免邮件里的远程图片把后台地址作为 Referer 泄露出去
                  <iframe
                    sandbox=""
                    referrerPolicy="no-referrer"
                    srcDoc={htmlBody}
                    title={t('detail.htmlPreviewTitle')}
                    className="h-64 w-full rounded-lg border border-slate-200 bg-white dark:border-slate-700"
                  />
                ) : (
                  <pre className="max-h-64 overflow-auto whitespace-pre-wrap break-words rounded-lg border border-slate-200 bg-slate-50 p-3 font-mono text-xs leading-relaxed text-slate-700 dark:border-slate-700 dark:bg-slate-800 dark:text-slate-200">
                    {textBody}
                  </pre>
                )}
              </>
            )}
          </section>
        </div>
        ) : null}
        </div>
      </dialog>

      {/* 重发确认弹窗：按原内容重新发送并走轮换；pending 期间阻断关闭与重复提交 */}
      {resendConfirm && (
        <Modal
          open
          onClose={() => {
            if (!resendMutation.isPending) setResendConfirm(false)
          }}
          title={t('resend.title')}
          footer={
            <>
              <Button
                variant="secondary"
                onClick={() => setResendConfirm(false)}
                disabled={resendMutation.isPending}
              >
                {t('common:actions.cancel')}
              </Button>
              <Button
                variant="danger"
                loading={resendMutation.isPending}
                onClick={() => {
                  if (!resendMutation.isPending) resendMutation.mutate()
                }}
              >
                {t('resend.confirm')}
              </Button>
            </>
          }
        >
          <p className="text-sm text-slate-600 dark:text-slate-300">
            {t('resend.bodyPrefix')}{' '}
            <span className="font-medium text-slate-900 dark:text-white">
              {parseToAddrs(detail?.to_addrs).join('、') || '—'}
            </span>
            {t('resend.bodySuffix')}
          </p>
          <p className="mt-2 text-xs text-slate-500 dark:text-slate-400">{t('resend.note')}</p>
        </Modal>
      )}
    </>
  )
}

/* ---------------------------------- 页面 ---------------------------------- */

export function LogsPage() {
  const { t } = useTranslation(['logs', 'common'])
  const [searchParams, setSearchParams] = useSearchParams()

  // 筛选/分页/排序/实时开关全部来自 URL query（刷新、分享、前进后退不丢）
  const rawStatus = searchParams.get('status')
  const status: LogStatusFilter =
    rawStatus === 'sent' || rawStatus === 'failed' || rawStatus === 'pending' ? rawStatus : ''
  const rawChannelId = searchParams.get('channel_id')
  const channelId = rawChannelId && /^\d+$/.test(rawChannelId) ? Number.parseInt(rawChannelId, 10) : 0
  const q = searchParams.get('q') ?? ''
  const page = Math.max(1, Number.parseInt(searchParams.get('page') ?? '1', 10) || 1)
  const sort = normalizeSort(searchParams.get('sort'))
  const pageSize = normalizePageSize(searchParams.get('page_size'))
  const live = searchParams.get('live') === '1'

  const channelsQuery = useChannels()
  const logsQuery = useLogs({ page, pageSize, status, channelId, q, sort, live })
  const [detailId, setDetailIdState] = useState<number | null>(null)
  const [exporting, setExporting] = useState(false)
  // 页码跳转输入框（回车提交，提交后清空）
  const [pageInput, setPageInput] = useState('')

  // 关键词输入本地态 → 300ms 防抖后写入 URL
  const [keywordInput, setKeywordInput] = useState(q)
  const debouncedKeyword = useDebouncedValue(keywordInput, DEBOUNCE_MS)
  // 记录本组件推送的 q，避免把防抖回写误判为外部导航而回填输入框打断输入
  const pushedQRef = useRef<string | null>(null)

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
    const trimmed = debouncedKeyword.trim()
    // 同一次输入的防抖结果只推送一次：外部导航改变 q 时（前进/后退）不回写
    if (trimmed === pushedQRef.current) return
    pushedQRef.current = trimmed
    if (trimmed !== q) setFilter('q', trimmed)
  }, [debouncedKeyword, q, setFilter])

  useEffect(() => {
    // 外部 q 变化（前进/后退、分享链接）→ 回填输入框
    if (pushedQRef.current !== q) setKeywordInput(q)
  }, [q])

  const items = logsQuery.data?.items ?? []

  const total = logsQuery.data?.total ?? 0
  const totalPages = Math.max(1, Math.ceil(total / pageSize))
  // 分页范围文案：第 21–40 条 · 共 142 条 · 第 2 / 7 页
  const rangeStart = (page - 1) * pageSize + 1
  const rangeEnd = Math.min(total, page * pageSize)
  const hasPrev = page > 1
  const hasNext = total > 0 ? page < totalPages : items.length >= pageSize
  const hasActiveFilter = Boolean(status || channelId || q)

  // URL 页码超界（如分享链接里 page 超过总页数）→ 自动回到第 1 页
  useEffect(() => {
    if (total > 0 && page > totalPages) setFilter('page', '1')
  }, [total, page, totalPages, setFilter])

  function toggleLive() {
    updateParams((p) => {
      if (live) p.delete('live')
      else p.set('live', '1')
    })
  }

  /** 按当前筛选导出 CSV（status/channel_id/q/sort，与列表一致） */
  async function handleExport() {
    if (exporting) return
    setExporting(true)
    try {
      await downloadLogsCsv({ status, channelId, q, sort })
      toast.success(t('toast.exported'))
    } catch (err) {
      // downloadLogsCsv 抛出的 ApiError.message 是 'logs:' 前缀词典 key（EXPORT_ERROR_KEYS），在此翻译渲染；
      // 其余 message（后端错误体、浏览器中断信息）原样透传
      if (err instanceof ApiError && err.message === EXPORT_ERROR_KEYS.timeout) {
        toast.error(t('export.timeout'))
      } else if (err instanceof ApiError && err.message === EXPORT_ERROR_KEYS.networkError) {
        toast.error(t('export.networkError'))
      } else if (err instanceof ApiError && err.message === EXPORT_ERROR_KEYS.failedHttp) {
        toast.error(t('export.failedHttp', { httpStatus: err.status }))
      } else {
        toast.error(err instanceof Error && err.message ? err.message : t('toast.exportFailed'))
      }
    } finally {
      setExporting(false)
    }
  }

  /** 页码跳转：回车触发，越界收敛到 [1, totalPages]，非法输入忽略 */
  function handlePageJump() {
    const target = Number.parseInt(pageInput.trim(), 10)
    setPageInput('')
    if (!Number.isFinite(target)) return
    setFilter('page', String(Math.min(Math.max(target, 1), totalPages)))
  }

  /** 清空全部筛选（空态引导用），保留排序/每页条数/实时开关 */
  function clearFilters() {
    setKeywordInput('')
    pushedQRef.current = ''
    updateParams((p) => {
      p.delete('status')
      p.delete('channel_id')
      p.delete('q')
      p.delete('page')
    })
  }

  return (
    <div className="space-y-6">
      <PageHeader title={t('page.title')} description={t('page.description')} />

      {/* 筛选条 */}
      <search className="flex flex-wrap items-center gap-3">
        <Select
          aria-label={t('filter.statusAria')}
          className="w-32"
          value={status}
          onChange={(e) => setFilter('status', e.target.value)}
        >
          <option value="">{t('filter.allStatus')}</option>
          <option value="sent">{t('common:state.success')}</option>
          <option value="failed">{t('common:state.failed')}</option>
          <option value="pending">{t('filter.pending')}</option>
        </Select>
        <Select
          aria-label={t('filter.channelAria')}
          className="w-44"
          value={channelId ? String(channelId) : ''}
          onChange={(e) => setFilter('channel_id', e.target.value)}
        >
          <option value="">{t('filter.allChannels')}</option>
          {channelsQuery.isPending && <option disabled>{t('filter.channelLoading')}</option>}
          {(channelsQuery.data?.items ?? []).map((channel) => (
            <option key={channel.id} value={String(channel.id)}>
              {channel.name}
            </option>
          ))}
        </Select>
        <Input
          aria-label={t('filter.searchAria')}
          className="w-64"
          placeholder={t('filter.searchPlaceholder')}
          value={keywordInput}
          onChange={(e) => setKeywordInput(e.target.value)}
        />
        <div className="ml-auto flex flex-wrap items-center gap-3">
          <Select
            aria-label={t('filter.sortAria')}
            className="w-32"
            value={sort}
            onChange={(e) => setFilter('sort', e.target.value === 'newest' ? '' : e.target.value)}
          >
            <option value="newest">{t('filter.newest')}</option>
            <option value="oldest">{t('filter.oldest')}</option>
            <option value="latency_desc">{t('filter.latencyDesc')}</option>
          </Select>
          <button
            type="button"
            onClick={toggleLive}
            aria-pressed={live}
            title={live ? t('filter.liveOnTitle') : t('filter.liveOffTitle')}
            className={cn(
              'inline-flex items-center gap-1.5 rounded-lg border px-3 py-2 text-sm font-medium transition-colors',
              live
                ? 'border-sky-500 bg-sky-50 text-sky-700 dark:border-sky-500/40 dark:bg-sky-500/10 dark:text-sky-400'
                : 'border-slate-300 bg-white text-slate-600 shadow-sm hover:bg-slate-50 dark:border-slate-700 dark:bg-slate-800 dark:text-slate-300 dark:hover:bg-slate-700',
            )}
          >
            <Radio className={cn('h-4 w-4', live && 'animate-pulse motion-reduce:animate-none')} aria-hidden />
            {t('filter.live')}
          </button>
          <Button
            variant="secondary"
            loading={exporting}
            onClick={() => void handleExport()}
            title={t('filter.exportTitle')}
          >
            <Download className="h-4 w-4" aria-hidden />
            {t('filter.exportCsv')}
          </Button>
        </div>
      </search>

      <Card>
        {logsQuery.isPending ? (
          <QueryLoading skeleton="table" />
        ) : logsQuery.isError ? (
          <QueryError message={logsQuery.error.message} onRetry={() => logsQuery.refetch()} />
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
              title={t('empty.noLogsTitle')}
              description={t('empty.noLogsDesc')}
            />
          )
        ) : (
          <TableScroll
            label={t('page.title')}
            className={cn(logsQuery.isPlaceholderData && 'opacity-60')}
          >
            {/* O33：容器 ≤600px 时折叠为卡片——表头隐藏、表格与行退块级；列名由每格 data-col 的 ::before 注入 */}
            <Table label={t('page.title')} className="@max-[600px]:block">
              <THead className="@max-[600px]:hidden">
                <tr>
                  <TH>{t('common:fields.time')}</TH>
                  <TH>{t('common:fields.status')}</TH>
                  <TH>{t('fields.channel')}</TH>
                  <TH>{t('fields.platform')}</TH>
                  <TH>{t('fields.from')}</TH>
                  <TH>{t('fields.to')}</TH>
                  <TH>{t('fields.subject')}</TH>
                  <TH numeric>{t('fields.latency')}</TH>
                  <TH className="text-right">{t('common:fields.actions')}</TH>
                </tr>
              </THead>
              {/* O33：堆叠态下 tbody 同步退块级（避免匿名表格包裹），并停用 divide——
                  行分隔改由 TRow 的 border-b 承担，防止相邻行双线 */}
              <TBody className="@max-[600px]:block @max-[600px]:divide-y-0">
                {/* O47 方案 A：行不再可聚焦（无 roving tabindex），原生表格语义；键盘入口是行内「详情」按钮，行点击仅服务鼠标 */}
                {items.map((log) => (
                  <TRow
                    key={log.id}
                    onClick={() => setDetailIdState(log.id)}
                    className="cursor-pointer @max-[600px]:block @max-[600px]:border-b @max-[600px]:border-border-subtle @max-[600px]:px-5 @max-[600px]:py-2"
                  >
                    <TD data-col={t('common:fields.time')} className={tdStack}>
                      {/* RelativeTime 自带 title 绝对时间与 <time> 语义（O30），样式内聚在组件里 */}
                      <RelativeTime iso={log.created_at} />
                    </TD>
                    <TD data-col={t('common:fields.status')} className={tdStack}>
                      <LogStatusBadge status={log.status} />
                    </TD>
                    <TD
                      data-col={t('fields.channel')}
                      className={cn(
                        'max-w-36',
                        tdStack,
                        // 堆叠态解除表格态的宽度挤压：整宽换行显示
                        '@max-[600px]:max-w-none @max-[600px]:whitespace-normal',
                      )}
                    >
                      <span
                        className="block truncate font-medium text-slate-900 dark:text-white"
                        title={log.channel_name}
                      >
                        {log.channel_name}
                      </span>
                    </TD>
                    <TD data-col={t('fields.platform')} className={tdStack}>
                      <ProviderBadge provider={log.provider} />
                    </TD>
                    <TD
                      data-col={t('fields.from')}
                      className={cn(
                        'max-w-44',
                        tdStack,
                        '@max-[600px]:max-w-none @max-[600px]:whitespace-normal',
                      )}
                    >
                      <span
                        className="block truncate text-slate-600 dark:text-slate-300"
                        title={log.from_addr}
                      >
                        {log.from_addr || '—'}
                      </span>
                    </TD>
                    <TD data-col={t('fields.to')} className={tdStack}>
                      <RecipientsCell addrs={parseToAddrs(log.to_addrs)} />
                    </TD>
                    <TD
                      data-col={t('fields.subject')}
                      className={cn(
                        'max-w-56',
                        tdStack,
                        '@max-[600px]:max-w-none @max-[600px]:whitespace-normal',
                      )}
                    >
                      <span
                        className="block truncate text-slate-600 dark:text-slate-300"
                        title={log.subject}
                      >
                        {log.subject || t('detail.noSubject')}
                      </span>
                    </TD>
                    <TD numeric data-col={t('fields.latency')} className={tdStack}>
                      {log.latency_ms} ms
                    </TD>
                    <TD data-col={t('common:fields.actions')} className={cn('text-right', tdStack)}>
                      <Button
                        variant="ghost"
                        className="h-7 px-2 text-xs"
                        onClick={() => setDetailIdState(log.id)}
                      >
                        {t('common:fields.detail')}
                      </Button>
                    </TD>
                  </TRow>
                ))}
              </TBody>
            </Table>
          </TableScroll>
        )}

        {/* 分页：每页条数 + 页码跳转；O31：触屏（pointer-coarse）下控件提升到 44px 触摸目标，桌面密度不变 */}
        {!logsQuery.isPending && !logsQuery.isError && total > 0 && (
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
                    setFilter('page_size', e.target.value === String(LOGS_PAGE_SIZE) ? '' : e.target.value)
                  }
                >
                  {LOGS_PAGE_SIZES.map((size) => (
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

      {detailId !== null && (
        <LogDetailPanel id={detailId} onClose={() => setDetailIdState(null)} />
      )}
    </div>
  )
}
