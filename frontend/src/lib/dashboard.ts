import { useQuery } from '@tanstack/react-query'

import { api } from '@/lib/api'

/**
 * 仪表盘数据层（SPEC §5，snake_case 原样字段名）：
 * GET /api/stats/overview、GET /api/stats/daily?days&channel_id，
 * 以及 KPI 迷你趋势图的序列派生工具（纯函数，供 Sparkline 消费）。
 */

/* ---------------------------------- 类型 ---------------------------------- */

/** GET /api/stats/overview 响应 */
export interface StatsOverview {
  today_sent: number
  today_failed: number
  month_sent: number
  /** 0-100（今日口径；今日无发送时后端返回 0，前端按「无数据」展示 —） */
  success_rate: number
  channels: OverviewChannel[]
}

/** overview.channels 元素；last_error 为可选附带字段（渠道列表接口含此字段） */
export interface OverviewChannel {
  id: number
  name: string
  provider: string
  status: number
  used_day: number
  effective_daily_limit: number
  used_month: number
  effective_monthly_limit: number
  last_error?: string
}

/** GET /api/stats/daily 的逐日项（date 为 MM-DD，含今天，缺数据日补 0） */
export interface DailyStat {
  date: string
  sent: number
  failed: number
}

export interface DailyResponse {
  items: DailyStat[]
}

/* ---------------------------------- URL 参数归一化 ---------------------------------- */

/** 主图时间范围可选项（对应 days 参数） */
export const DAYS_OPTIONS = [7, 30, 90] as const
export type DaysOption = (typeof DAYS_OPTIONS)[number]

/** 校验 URL 里的 days（非法值回落默认 30） */
export function normalizeDays(raw: string | null): DaysOption {
  const n = raw === null ? NaN : Number.parseInt(raw, 10)
  return (DAYS_OPTIONS as readonly number[]).includes(n) ? (n as DaysOption) : 30
}

/** 校验 URL 里的 channel_id（非法/0 = 全部渠道，返回 0） */
export function normalizeChannelId(raw: string | null): number {
  if (raw === null || !/^\d+$/.test(raw)) return 0
  const n = Number.parseInt(raw, 10)
  return n > 0 ? n : 0
}

/* ---------------------------------- 数据 hooks ---------------------------------- */

/** GET /api/stats/overview（KPI 卡与渠道用量表共用，全量口径） */
export function useStatsOverview() {
  return useQuery({
    queryKey: ['stats', 'overview'] as const,
    queryFn: () => api<StatsOverview>('/api/stats/overview'),
  })
}

/**
 * GET /api/stats/daily?days&channel_id。
 * channel_id 传 0 = 全部渠道；KPI 迷你趋势固定用 { days: 30, channelId: 0 }
 * 与主图在默认档共享同一份缓存。
 */
export function useStatsDaily(params: { days: number; channelId?: number }) {
  const { days, channelId = 0 } = params
  return useQuery({
    queryKey: ['stats', 'daily', { days, channelId }] as const,
    queryFn: () => {
      const sp = new URLSearchParams()
      sp.set('days', String(days))
      if (channelId > 0) sp.set('channel_id', String(channelId))
      return api<DailyResponse>(`/api/stats/daily?${sp.toString()}`)
    },
  })
}

/* ---------------------------------- 序列派生（KPI sparkline） ---------------------------------- */

/** 逐日 sent 序列 */
export function sentSeries(items: DailyStat[]): number[] {
  return items.map((it) => it.sent)
}

/** 逐日 failed 序列 */
export function failedSeries(items: DailyStat[]): number[] {
  return items.map((it) => it.failed)
}

/** 累计线（本月发送卡：sent 逐日累加） */
export function cumulative(series: number[]): number[] {
  let total = 0
  return series.map((v) => (total += v))
}

/**
 * 逐日成功率序列（0-100，取整）：当日 sent+failed=0 视为无数据 → null，
 * 绘图时跳过（断线）而非按 0 计入，避免无发送日把趋势线拉成假下跌。
 */
export function rateSeries(items: DailyStat[]): Array<number | null> {
  return items.map((it) => {
    const total = it.sent + it.failed
    return total > 0 ? Math.round((it.sent / total) * 100) : null
  })
}

/**
 * 趋势方向：前半段均值 vs 后半段均值，或末值相对前半段基线的跳变
 *（单日突增/突降也是趋势，如新装实例首日爆发）；容差取序列波动幅度的 10%（过滤日常噪声），
 * 有效点少于 2 个或波动为 0 → flat。
 */
export function trendOf(series: Array<number | null>): 'up' | 'down' | 'flat' {
  const values = series.filter((v): v is number => v !== null)
  if (values.length < 2) return 'flat'
  const mid = Math.floor(values.length / 2)
  const avg = (xs: number[]): number =>
    xs.reduce((sum, x) => sum + x, 0) / Math.max(1, xs.length)
  const firstHalfAvg = avg(values.slice(0, mid))
  const delta = avg(values.slice(mid)) - firstHalfAvg
  const tolerance = (Math.max(...values) - Math.min(...values)) * 0.1
  if (tolerance === 0) return 'flat' // 恒定序列无趋势
  const last = values[values.length - 1]
  if (delta > tolerance || last - firstHalfAvg >= tolerance) return 'up'
  if (delta < -tolerance || firstHalfAvg - last >= tolerance) return 'down'
  return 'flat'
}

/* ---------------------------------- 展示格式化 ---------------------------------- */

/** 成功率文案：整数不带小数、非整数保留 1 位；无数据为 — */
export function formatRate(rate: number | undefined | null): string {
  if (rate === undefined || rate === null || !Number.isFinite(rate)) return '—'
  return Number.isInteger(rate) ? `${rate}%` : `${rate.toFixed(1)}%`
}

/**
 * 成功率数值染色（阈值内嵌说明）：
 * ≥ 90% 正常主文字色；< 90% 偏低 → amber 提示；< 70% 明显异常 → red 警示。
 * 仅对「有数据」的成功率生效（无数据时调用方展示 —，走中性色）。
 */
export function successRateValueClass(rate: number | undefined | null): string {
  if (rate === undefined || rate === null || !Number.isFinite(rate)) return ''
  if (rate < 70) return 'text-red-600 dark:text-red-400'
  if (rate < 90) return 'text-amber-600 dark:text-amber-400'
  return ''
}

/* ---------------------------------- 图表语义色（四色约束：emerald/red/sky/amber） ---------------------------------- */

/** 图表语义色 hex（recharts 非 Tailwind 区域用；深浅色共用 500 系，对比度两边均可） */
export const CHART_COLORS = {
  sent: '#10b981', // emerald-500
  failed: '#ef4444', // red-500
  rate: '#0ea5e9', // sky-500
  warn: '#f59e0b', // amber-500
} as const

/** 趋势中性色（flat 时 sparkline 用，slate 非语义四色但属中性灰，不算图表主色） */
export const TREND_NEUTRAL_COLOR = '#94a3b8' // slate-400
