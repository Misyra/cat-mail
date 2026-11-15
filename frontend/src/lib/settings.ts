import { useQuery } from '@tanstack/react-query'

import { api } from '@/lib/api'

/**
 * 系统设置数据层（SPEC §5，snake_case 原样字段名）。
 * rotation：priority=优先级优先（默认）｜round_robin=轮询。
 */

export type RotationStrategy = 'priority' | 'round_robin'

/** GET /api/settings 响应体，PUT /api/settings 请求体同构 */
export interface SystemSettings {
  rotation: RotationStrategy
  /** 单次发送最多尝试渠道数（1-10） */
  max_attempts: number
  /** 触发限流后渠道冷却秒数（≥1） */
  cooldown_seconds: number
  /** 连续失败自动禁用阈值（3-20） */
  consecutive_failure_threshold: number
  /** 日志保留天数（0 = 永久保留，最长 365） */
  log_retention_days: number
  /** 记录邮件正文开关（A4）：false 时新日志不落正文（历史行不受影响）；默认 true = 保持现状 */
  log_body_enabled: boolean
  /** 抑制列表自动入库开关（送达链路三件套）：hard bounce 与 complained 事件自动写入抑制列表；默认 true */
  suppression_auto_enabled: boolean
  /** 退信率告警阈值（百分比 0-100，0=关闭告警；默认 5）：渠道近 7 天退信率超阈值经通知双通道广播 */
  bounce_rate_threshold: number
}

export const SETTINGS_QUERY_KEY = ['settings'] as const

/** GET /api/settings */
export function useSettings() {
  return useQuery({
    queryKey: SETTINGS_QUERY_KEY,
    queryFn: () => api<SystemSettings>('/api/settings'),
  })
}

/**
 * 记录邮件正文开关收窄（与 lib/notifications.ts 的 normalizeNotifyFields 同款模式）：
 * 后端未返回该字段（旧版本/缓存响应）时回落默认 true（保持现状，行为不变）。
 */
export function normalizeLogBodyEnabled(s: SystemSettings): boolean {
  return (s as Partial<SystemSettings>).log_body_enabled ?? true
}

/** 抑制自动入库开关收窄：旧版本/缓存响应回落默认 true（送达链路三件套默认开） */
export function normalizeSuppressionAutoEnabled(s: SystemSettings): boolean {
  return (s as Partial<SystemSettings>).suppression_auto_enabled ?? true
}

/** 退信率告警阈值收窄：旧版本/缓存响应回落默认 5（任务书口径） */
export function normalizeBounceRateThreshold(s: SystemSettings): number {
  const v = (s as Partial<SystemSettings>).bounce_rate_threshold
  return typeof v === 'number' && Number.isFinite(v) ? v : 5
}

/** rotation 容错归一化（未知值回落到默认 priority） */
export function normalizeRotation(value: string | undefined): RotationStrategy {
  return value === 'round_robin' ? 'round_robin' : 'priority'
}
