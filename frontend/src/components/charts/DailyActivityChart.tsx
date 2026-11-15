/**
 * 发送活动主图卡（Supabase Logs 时间轴风格）：sent/failed 堆叠柱 + 成功率折线（右副轴）。
 * 头部右侧：时间范围 segmented control（days → URL query）+ 渠道筛选下拉（channel_id）。
 * 渠道筛选仅影响本图（KPI 统计卡为全量口径），头部有一句克制说明。
 */
import { useMemo } from 'react'
import { useTranslation } from 'react-i18next'
import {
  Bar,
  CartesianGrid,
  ComposedChart,
  Line,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from 'recharts'
import type { TooltipContentProps } from 'recharts'

import { QueryError, QueryLoading } from '@/components/QueryState'
import { Card } from '@/components/ui/Card'
import { EmptyState } from '@/components/ui/EmptyState'
import { Select } from '@/components/ui/Input'
import {
  CHART_COLORS,
  DAYS_OPTIONS,
  type DailyStat,
  type DaysOption,
} from '@/lib/dashboard'
import { useIsDarkMode } from '@/lib/theme'
import { cn } from '@/lib/utils'

/* ---------------------------------- 类型 ---------------------------------- */

/** 图表卡外部受控的渠道选项（由 overview.channels 派生） */
export interface ChannelOption {
  id: number
  name: string
}

/** 逐日图表数据：在 DailyStat 上派生当日成功率（无发送日为 null，断线） */
interface ActivityDatum {
  date: string
  sent: number
  failed: number
  rate: number | null
}

export interface DailyActivityChartProps {
  items: DailyStat[]
  isPending: boolean
  isError: boolean
  errorMessage: string
  onRetry: () => void
  days: DaysOption
  channelId: number
  channels: ChannelOption[]
  onDaysChange: (days: DaysOption) => void
  onChannelChange: (channelId: number) => void
}

/* ---------------------------------- 头部控件（本卡专用，不导出） ---------------------------------- */

/** days 档位 → 词典 key（文案在组件内 t() 渲染，模块层不持翻译结果） */
const DAYS_LABEL_KEYS = { 7: 'chart.days7', 30: 'chart.days30', 90: 'chart.days90' } as const

/** 时间范围 segmented control（对应 days 参数） */
function DaysSegmented({
  value,
  onChange,
}: {
  value: DaysOption
  onChange: (days: DaysOption) => void
}) {
  const { t } = useTranslation(['dashboard', 'common'])
  return (
    <div
      role="group"
      aria-label={t('chart.timeRangeAria')}
      className="inline-flex items-center rounded-lg border border-border-subtle bg-slate-100 p-0.5 dark:bg-slate-800"
    >
      {DAYS_OPTIONS.map((days) => (
        <button
          key={days}
          type="button"
          aria-pressed={value === days}
          onClick={() => onChange(days)}
          className={cn(
            'rounded-md px-2.5 py-1 text-xs font-medium transition-colors',
            value === days
              ? 'bg-surface-raised text-text-primary shadow-sm'
              : 'text-text-muted hover:text-text-secondary',
          )}
        >
          {t(DAYS_LABEL_KEYS[days])}
        </button>
      ))}
    </div>
  )
}

/** 渠道筛选下拉（对应 channel_id 参数；value 0 = 全部渠道） */
function ChannelFilter({
  value,
  options,
  onChange,
}: {
  value: number
  options: ChannelOption[]
  onChange: (channelId: number) => void
}) {
  const { t } = useTranslation(['dashboard', 'common'])
  return (
    <Select
      aria-label={t('chart.channelFilterAria')}
      className="w-36 text-base sm:text-xs"
      value={value === 0 ? '' : String(value)}
      onChange={(e) => onChange(e.target.value === '' ? 0 : Number.parseInt(e.target.value, 10))}
    >
      <option value="">{t('chart.allChannels')}</option>
      {options.map((channel) => (
        <option key={channel.id} value={String(channel.id)}>
          {channel.name}
        </option>
      ))}
    </Select>
  )
}

/* ---------------------------------- 自定义 Tooltip ---------------------------------- */

/** recharts Tooltip payload[0].payload 即原始数据项；类型守卫收窄 unknown（零 any） */
function isActivityDatum(value: unknown): value is ActivityDatum {
  if (typeof value !== 'object' || value === null) return false
  const rec = value as Record<string, unknown>
  return (
    typeof rec.date === 'string' &&
    typeof rec.sent === 'number' &&
    typeof rec.failed === 'number' &&
    (rec.rate === null || typeof rec.rate === 'number')
  )
}

/**
 * 深浅色适配的四行 Tooltip：日期 / 发送 / 失败 / 成功率。
 * recharts 3：自定义 content 的 props 由 TooltipContentProps 提供（v2 为 TooltipProps，
 * label 类型放宽为 string | number）。泛型沿用库默认（ValueType/NameType，与 <Tooltip>
 * 的推断一致）；entry.payload 在库类型里是 any，先归入 unknown 再用类型守卫收窄，保持零 any。
 */
function ActivityTooltip({
  active,
  payload,
}: TooltipContentProps) {
  // hook 须在任何提前 return 之前调用
  const { t } = useTranslation(['dashboard', 'common'])
  if (!active || payload.length === 0) return null
  const datum: unknown = payload[0]?.payload
  if (!isActivityDatum(datum)) return null

  const rateText =
    datum.rate === null ? '—' : Number.isInteger(datum.rate) ? `${datum.rate}%` : `${datum.rate.toFixed(1)}%`
  const rows = [
    { color: CHART_COLORS.sent, label: t('chart.sent'), value: datum.sent.toLocaleString() },
    { color: CHART_COLORS.failed, label: t('common:state.failed'), value: datum.failed.toLocaleString() },
    { color: CHART_COLORS.rate, label: t('successRate'), value: rateText },
  ]

  return (
    <div className="rounded-lg border border-border-subtle bg-surface-raised px-3 py-2 text-xs shadow-lg">
      <p className="mb-1 font-medium text-text-primary">{datum.date}</p>
      {rows.map((row) => (
        <p
          key={row.label}
          className="flex items-center gap-1.5 tabular-nums text-text-secondary"
        >
          <span className="inline-block h-2 w-2 rounded-full" style={{ backgroundColor: row.color }} />
          {t('chart.tooltipLine', { label: row.label, value: row.value })}
        </p>
      ))}
    </div>
  )
}

/* ---------------------------------- 组件 ---------------------------------- */

export function DailyActivityChart({
  items,
  isPending,
  isError,
  errorMessage,
  onRetry,
  days,
  channelId,
  channels,
  onDaysChange,
  onChannelChange,
}: DailyActivityChartProps) {
  const { t } = useTranslation(['dashboard', 'common'])
  const isDark = useIsDarkMode()
  const gridColor = isDark ? '#334155' : '#e2e8f0'
  const tickColor = isDark ? '#94a3b8' : '#64748b'

  const data = useMemo<ActivityDatum[]>(
    () =>
      items.map((it) => ({
        ...it,
        rate: it.sent + it.failed > 0 ? Math.round((it.sent / (it.sent + it.failed)) * 100) : null,
      })),
    [items],
  )
  const hasActivity = items.some((it) => it.sent > 0 || it.failed > 0)

  // O42 图表可访问摘要：图表区以 role="img" + aria-label 提供一句话汇总，读屏不必逐个消化轴刻度；
  // 图表仅在 hasActivity 时渲染（总量必大于 0），成功率无需判空
  const totalSent = data.reduce((sum, it) => sum + it.sent, 0)
  const totalFailed = data.reduce((sum, it) => sum + it.failed, 0)
  const totalRate = Math.round((totalSent / (totalSent + totalFailed)) * 100)
  const channelName = channelId > 0 ? channels.find((channel) => channel.id === channelId)?.name : undefined
  const chartAriaLabel =
    t('chart.ariaSummary', {
      range: t(DAYS_LABEL_KEYS[days]),
      sent: totalSent.toLocaleString(),
      failed: totalFailed.toLocaleString(),
      rate: totalRate,
    }) + (channelName ? t('chart.ariaChannelOnly', { name: channelName }) : '')

  return (
    <Card>
      <div className="flex flex-wrap items-center justify-between gap-3 border-b border-border-subtle px-5 py-4">
        <div className="flex flex-wrap items-baseline gap-x-2 gap-y-1">
          <h2 className="text-sm font-semibold text-text-primary">{t('chart.title')}</h2>
          <p className="text-xs text-text-muted">{t('chart.filterHint')}</p>
        </div>
        <div className="flex flex-wrap items-center gap-2">
          <ChannelFilter value={channelId} options={channels} onChange={onChannelChange} />
          <DaysSegmented value={days} onChange={onDaysChange} />
        </div>
      </div>

      {isPending ? (
        <QueryLoading skeleton="chart" />
      ) : isError ? (
        <QueryError message={errorMessage} onRetry={onRetry} />
      ) : !hasActivity ? (
        <EmptyState
          title={t('chart.emptyTitle')}
          description={t('chart.emptyDescription')}
        />
      ) : (
        <div className="px-2 pb-2" role="img" aria-label={chartAriaLabel}>
          {/* 图例：柱=发送/失败，线=成功率 */}
          <div className="mb-2 flex items-center gap-4 px-1 text-xs text-text-secondary">
            <span className="flex items-center gap-1.5">
              <span className="h-2 w-2 rounded-sm" style={{ backgroundColor: CHART_COLORS.sent }} />
              {t('chart.sent')}
            </span>
            <span className="flex items-center gap-1.5">
              <span className="h-2 w-2 rounded-sm" style={{ backgroundColor: CHART_COLORS.failed }} />
              {t('common:state.failed')}
            </span>
            <span className="flex items-center gap-1.5">
              <span
                className="inline-block h-0.5 w-4 rounded-full"
                style={{ backgroundColor: CHART_COLORS.rate }}
              />
              {t('successRate')}
            </span>
          </div>
          <ResponsiveContainer width="100%" height={280}>
            <ComposedChart data={data} margin={{ top: 8, right: 8, left: 0, bottom: 0 }}>
              {/* recharts 3：CartesianGrid 不再自动选第一条轴，须显式绑定 yAxisId（横向网格线的刻度来源） */}
              <CartesianGrid strokeDasharray="3 3" stroke={gridColor} vertical={false} yAxisId="count" />
              <XAxis
                dataKey="date"
                tick={{ fill: tickColor, fontSize: 11 }}
                tickLine={false}
                axisLine={{ stroke: gridColor }}
                interval="preserveStartEnd"
                minTickGap={24}
              />
              {/* 左轴：件数，整数刻度 */}
              <YAxis
                yAxisId="count"
                allowDecimals={false}
                width={36}
                tick={{ fill: tickColor, fontSize: 11 }}
                tickLine={false}
                axisLine={false}
              />
              {/* 右副轴：成功率 0-100% */}
              <YAxis
                yAxisId="rate"
                orientation="right"
                domain={[0, 100]}
                ticks={[0, 25, 50, 75, 100]}
                tickFormatter={(v: number) => `${v}%`}
                width={40}
                tick={{ fill: tickColor, fontSize: 11 }}
                tickLine={false}
                axisLine={false}
              />
              <Tooltip
                // recharts 3：TooltipContentProps 的 active/payload 等为必填，ReactElement 形式
                // 需传入完整 props；官方类型路径是函数形式（由 recharts 回调注入完整 props）
                content={(props) => <ActivityTooltip {...props} />}
                cursor={{ fill: isDark ? 'rgba(51, 65, 85, 0.4)' : 'rgba(148, 163, 184, 0.12)' }}
              />
              <Bar
                yAxisId="count"
                dataKey="sent"
                name={t('chart.sent')}
                stackId="activity"
                fill={CHART_COLORS.sent}
                radius={[2, 2, 0, 0]}
                maxBarSize={28}
                // 仪表盘筛选切换频繁重绘：关闭入场动画保证即时响应与截图环境确定性
                isAnimationActive={false}
              />
              <Bar
                yAxisId="count"
                dataKey="failed"
                name={t('common:state.failed')}
                stackId="activity"
                fill={CHART_COLORS.failed}
                radius={[2, 2, 0, 0]}
                maxBarSize={28}
                isAnimationActive={false}
              />
              <Line
                yAxisId="rate"
                dataKey="rate"
                name={t('successRate')}
                stroke={CHART_COLORS.rate}
                strokeWidth={2}
                dot={false}
                isAnimationActive={false}
                // 无发送日 rate 为 null：默认断线，不虚构连续性
                connectNulls={false}
              />
            </ComposedChart>
          </ResponsiveContainer>
        </div>
      )}
    </Card>
  )
}
