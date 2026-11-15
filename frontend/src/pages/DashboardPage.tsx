/**
 * 仪表盘（SPEC §6）：4 统计卡（右上角迷你趋势图 sparkline）+ 发送活动主图（堆叠柱+成功率折线，
 * days/channel_id 同步 URL query）+ 渠道用量表。
 * 数据：GET /api/stats/overview、GET /api/stats/daily?days&channel_id（SPEC §5，snake_case 原样字段名）。
 */
import { ArrowUpRight, CheckCircle2, Percent, Send, XCircle, type LucideIcon } from 'lucide-react'
import { lazy, Suspense } from 'react'
import { useTranslation } from 'react-i18next'
import { Link, useNavigate, useSearchParams } from 'react-router'

import { PageHeader } from '@/components/PageHeader'
import { QueryError, QueryLoading } from '@/components/QueryState'
import { ChannelStatusBadge, ProviderBadge } from '@/components/badges'
import { Sparkline } from '@/components/charts/Sparkline'
import { OnboardingChecklist } from '@/components/onboarding/OnboardingChecklist'
import { Button } from '@/components/ui/Button'
import { Card } from '@/components/ui/Card'
import { EmptyState } from '@/components/ui/EmptyState'
import { TD, TH, TBody, THead, TRow, Table } from '@/components/ui/Table'
import {
  CHART_COLORS,
  TREND_NEUTRAL_COLOR,
  cumulative,
  failedSeries,
  formatRate,
  normalizeChannelId,
  normalizeDays,
  rateSeries,
  sentSeries,
  successRateValueClass,
  trendOf,
  useStatsDaily,
  useStatsOverview,
  type DaysOption,
} from '@/lib/dashboard'
import { useOnboardingDismissed } from '@/lib/onboarding'
import { cn } from '@/lib/utils'

// O44：recharts 体积大（独立 chunk 约 385KB），图表组件懒加载，KPI 卡首屏不被其阻塞；
// 数据查询仍在页面挂载时发起（useStatsDaily 不动），与 chunk 下载并行
const DailyActivityChart = lazy(() =>
  import('@/components/charts/DailyActivityChart').then((m) => ({ default: m.DailyActivityChart })),
)

/* ---------------------------------- KPI 统计卡 ---------------------------------- */

/**
 * 趋势 → sparkline 着色（语义反转）：
 * 成功类指标 up=emerald / down=red；invert=true 的失败数卡反转（up=red / down=emerald）；
 * flat 用中性灰（slate，非语义四色，仅中性辅助）。
 */
function trendSparkColor(trend: 'up' | 'down' | 'flat', invert = false): string {
  if (trend === 'flat') return TREND_NEUTRAL_COLOR
  const good = (trend === 'up') !== invert
  return good ? CHART_COLORS.sent : CHART_COLORS.failed
}

/** 趋势 → sr-only 描述的词典 key（O42）：Sparkline 对读屏隐藏且仅以颜色辅助，趋势方向须有文本替代 */
const TREND_SR_KEYS = {
  up: 'trend.up',
  down: 'trend.down',
  flat: 'trend.flat',
} as const

function StatCard({
  label,
  value,
  valueClass,
  icon: Icon,
  iconClasses,
  sparkData,
  sparkColor,
  trend,
  to,
}: {
  label: string
  value: string
  /** 数值附加色（如成功率阈值染色）；空串 = 默认主文字色 */
  valueClass?: string
  icon: LucideIcon
  iconClasses: string
  /** 近 30 日迷你趋势序列（无数据时 Sparkline 返回 null 隐藏不占位） */
  sparkData: Array<number | null>
  sparkColor: string
  /** 近 30 日趋势方向：渲染 sr-only 趋势文本（可选，无趋势序列时不传） */
  trend?: 'up' | 'down' | 'flat'
  /** 可选下钻路由：提供时整卡可点击跳转（如 /logs?status=sent） */
  to?: string
}) {
  const { t } = useTranslation(['dashboard', 'common'])
  const card = (
    <Card
      className={cn(
        'p-5',
        to && 'transition-colors hover:border-slate-300 dark:hover:border-slate-700',
      )}
    >
      <div className="flex items-start justify-between gap-3">
        <div className="flex items-center gap-2">
          <span
            className={cn('flex h-7 w-7 shrink-0 items-center justify-center rounded-lg', iconClasses)}
          >
            <Icon className="h-4 w-4" aria-hidden />
          </span>
          <p className="text-sm text-text-muted">{label}</p>
        </div>
        <Sparkline data={sparkData} color={sparkColor} className="h-9 w-16 shrink-0 sm:w-20" />
        {/* O42：趋势仅靠线形/颜色传达，读屏拿不到；补一句 sr-only 趋势描述（sr-only 绝对定位，不影响布局） */}
        {trend && (
          <span className="sr-only">{t('stats.trendSummary', { trend: t(TREND_SR_KEYS[trend]) })}</span>
        )}
      </div>
      <p
        className={cn(
          'mt-2 text-3xl font-semibold tabular-nums',
          // valueClass（如成功率阈值染色）自带 dark: 变体时整体接管文字色，避免与默认色冲突
          valueClass || 'text-text-primary',
        )}
      >
        {value}
      </p>
    </Card>
  )
  if (!to) return card
  return (
    <Link
      to={to}
      title={t('stats.viewLogsTitle')}
      className="block rounded-xl focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-sky-500/60"
    >
      {card}
    </Link>
  )
}

/* ---------------------------------- 渠道用量表部件 ---------------------------------- */

/** 用量：used/limit 文本 + 进度条；limit 0 = 不限（∞，仅轨道无填充） */
function UsageBar({ used, limit }: { used: number; limit: number }) {
  const unlimited = !limit || limit <= 0
  const pct = unlimited ? 0 : Math.min(100, Math.round((used / limit) * 100))
  const fillClass =
    pct >= 100 ? 'bg-red-500' : pct >= 80 ? 'bg-amber-500' : 'bg-sky-500'

  return (
    <div className="ml-auto w-32">
      <div className="mb-1 text-xs tabular-nums text-text-secondary">
        {used} / {unlimited ? '∞' : limit}
      </div>
      <div className="h-1.5 w-full overflow-hidden rounded-full bg-slate-200 dark:bg-slate-700">
        {!unlimited && <div className={cn('h-full rounded-full', fillClass)} style={{ width: `${pct}%` }} />}
      </div>
    </div>
  )
}

/** 最后错误：单行截断 + 悬停气泡展示全文 */
function ErrorHint({ error }: { error?: string }) {
  if (!error) {
    return <span className="text-xs text-slate-500 dark:text-slate-400">—</span>
  }
  return (
    <div className="group relative inline-block max-w-56 cursor-help align-middle">
      <span className="block truncate text-xs text-red-600 dark:text-red-400">{error}</span>
      <span
        role="tooltip"
        className="pointer-events-none absolute left-0 top-full z-20 mt-1 hidden w-80 break-words rounded-lg border border-slate-200 bg-white p-2.5 text-xs leading-relaxed text-slate-600 shadow-lg group-hover:block dark:border-slate-700 dark:bg-slate-800 dark:text-slate-300"
      >
        {error}
      </span>
    </div>
  )
}

/* ---------------------------------- 页面 ---------------------------------- */

export function DashboardPage() {
  const { t } = useTranslation(['dashboard', 'common'])
  const navigate = useNavigate()
  const [searchParams, setSearchParams] = useSearchParams()
  // 首启引导：跳过/三步全完成后（localStorage 'catmail-onboarding-dismissed'）不再渲染
  const [onboardingDismissed, dismissOnboarding] = useOnboardingDismissed()

  // 主图时间范围/渠道筛选同步 URL query（刷新、分享、前进后退不丢）；KPI 卡不随渠道筛选联动（全量口径）
  const days = normalizeDays(searchParams.get('days'))
  const channelId = normalizeChannelId(searchParams.get('channel_id'))
  const setChartParam = (key: 'days' | 'channel_id', value: string) => {
    setSearchParams(
      (prev) => {
        const next = new URLSearchParams(prev)
        if (value) next.set(key, value)
        else next.delete(key)
        return next
      },
      { replace: true },
    )
  }

  const overviewQuery = useStatsOverview()
  const kpiDailyQuery = useStatsDaily({ days: 30 }) // KPI 迷你趋势：近 30 日全量口径
  const chartDailyQuery = useStatsDaily({ days, channelId }) // 主图：受 URL 筛选控制

  const overview = overviewQuery.data
  const kpiItems = kpiDailyQuery.data?.items ?? []
  const chartItems = chartDailyQuery.data?.items ?? []
  const channels = overview?.channels ?? []

  // 各卡趋势方向与着色：成功类正常语义、失败数卡反转
  const sentTrend = trendOf(sentSeries(kpiItems))
  const failedTrend = trendOf(failedSeries(kpiItems))
  const cumulativeSent = cumulative(sentSeries(kpiItems))
  const monthTrend = trendOf(cumulativeSent)
  const rateTrend = trendOf(rateSeries(kpiItems))

  // 成功率数值染色（阈值见 successRateValueClass 注释）；今日无发送时成功率无意义，展示 —
  const hasTodayTraffic = (overview?.today_sent ?? 0) + (overview?.today_failed ?? 0) > 0
  const todayRate = overview?.success_rate
  const rateValue = hasTodayTraffic ? formatRate(todayRate) : '—'
  const rateValueClass = hasTodayTraffic ? successRateValueClass(todayRate) : ''

  return (
    <div className="space-y-6">
      <PageHeader title={t('title')} description={t('description')} />

      {/* 4 统计卡（右上角近 30 日迷你趋势，按趋势语义着色） */}
      <div className="grid grid-cols-1 gap-4 sm:grid-cols-2 xl:grid-cols-4">
        <StatCard
          label={t('stats.todaySent')}
          value={(overview?.today_sent ?? 0).toLocaleString()}
          icon={CheckCircle2}
          iconClasses="bg-emerald-50 text-emerald-600 dark:bg-emerald-500/10 dark:text-emerald-400"
          sparkData={sentSeries(kpiItems)}
          sparkColor={trendSparkColor(sentTrend)}
          trend={sentTrend}
          to="/logs?status=sent"
        />
        <StatCard
          label={t('stats.todayFailed')}
          value={(overview?.today_failed ?? 0).toLocaleString()}
          icon={XCircle}
          iconClasses="bg-red-50 text-red-600 dark:bg-red-500/10 dark:text-red-400"
          sparkData={failedSeries(kpiItems)}
          sparkColor={trendSparkColor(failedTrend, true)}
          trend={failedTrend}
          to="/logs?status=failed"
        />
        <StatCard
          label={t('stats.monthSent')}
          value={(overview?.month_sent ?? 0).toLocaleString()}
          icon={Send}
          iconClasses="bg-sky-50 text-sky-600 dark:bg-sky-500/10 dark:text-sky-400"
          sparkData={cumulativeSent}
          sparkColor={trendSparkColor(monthTrend)}
          trend={monthTrend}
          to="/logs"
        />
        <StatCard
          label={t('successRate')}
          value={rateValue}
          valueClass={rateValueClass}
          icon={Percent}
          iconClasses="bg-violet-50 text-violet-600 dark:bg-violet-500/10 dark:text-violet-400"
          sparkData={rateSeries(kpiItems)}
          sparkColor={trendSparkColor(rateTrend)}
          trend={rateTrend}
        />
      </div>

      {/* 首启引导（仅仪表盘展示；状态由现有接口实时推导，跳过/全完成后隐藏） */}
      {!onboardingDismissed && <OnboardingChecklist onDismiss={dismissOnboarding} />}

      {/* 发送活动主图：days/channel_id 同步 URL；渠道筛选仅影响本图；懒加载 chunk，Suspense 期间展示图表骨架 */}
      <Suspense fallback={<QueryLoading skeleton="chart" />}>
        <DailyActivityChart
          items={chartItems}
          isPending={chartDailyQuery.isPending}
          isError={chartDailyQuery.isError}
          errorMessage={chartDailyQuery.error?.message ?? t('loadFailed')}
          onRetry={() => void chartDailyQuery.refetch()}
          days={days}
          channelId={channelId}
          channels={channels.map((channel) => ({ id: channel.id, name: channel.name }))}
          onDaysChange={(next: DaysOption) => setChartParam('days', String(next))}
          onChannelChange={(next: number) => setChartParam('channel_id', next > 0 ? String(next) : '')}
        />
      </Suspense>

      {/* 渠道用量表 */}
      <Card>
        <div className="border-b border-border-subtle px-5 py-4">
          <h2 className="text-sm font-semibold text-text-primary">{t('usage.title')}</h2>
        </div>
        {overviewQuery.isPending ? (
          <QueryLoading skeleton="table" />
        ) : overviewQuery.isError ? (
          <QueryError
            message={overviewQuery.error.message}
            onRetry={() => void overviewQuery.refetch()}
          />
        ) : channels.length === 0 ? (
          <EmptyState
            title={t('usage.emptyTitle')}
            action={
              <Button as={Link} to="/channels">
                {t('usage.emptyAction')}
              </Button>
            }
          />
        ) : (
          <div className="overflow-x-auto">
            <Table>
              <THead>
                <tr>
                  <TH>{t('usage.colChannel')}</TH>
                  <TH>{t('usage.colPlatform')}</TH>
                  <TH>{t('common:fields.status')}</TH>
                  <TH numeric>{t('usage.colToday')}</TH>
                  <TH numeric>{t('usage.colMonth')}</TH>
                  <TH>{t('usage.colLastError')}</TH>
                </tr>
              </THead>
              <TBody>
                {channels.map((channel) => (
                  <TRow
                    key={channel.id}
                    onClick={() => navigate(`/logs?channel_id=${channel.id}`)}
                    title={t('usage.viewChannelLogsTitle', { name: channel.name })}
                    className="cursor-pointer group/row"
                  >
                    <TD className="font-medium text-text-primary">
                      <span className="flex items-center gap-1.5">
                        {channel.name}
                        <span className="hidden shrink-0 items-center gap-0.5 text-xs font-normal text-sky-600 group-hover/row:inline-flex dark:text-sky-400">
                          {t('usage.viewLogs')}
                          <ArrowUpRight className="h-3 w-3" aria-hidden />
                        </span>
                      </span>
                    </TD>
                    <TD>
                      <ProviderBadge provider={channel.provider} />
                    </TD>
                    <TD>
                      <ChannelStatusBadge status={channel.status} />
                    </TD>
                    <TD numeric>
                      <UsageBar used={channel.used_day} limit={channel.effective_daily_limit} />
                    </TD>
                    <TD numeric>
                      <UsageBar used={channel.used_month} limit={channel.effective_monthly_limit} />
                    </TD>
                    <TD>
                      <ErrorHint error={channel.last_error} />
                    </TD>
                  </TRow>
                ))}
              </TBody>
            </Table>
          </div>
        )}
      </Card>
    </div>
  )
}
