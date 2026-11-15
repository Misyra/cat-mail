import type { HTMLAttributes } from 'react'
import { useTranslation } from 'react-i18next'

import { cn } from '@/lib/utils'

/**
 * 语义徽章色系统（全局唯一映射，禁止页面内散落写状态色）：
 * - success = emerald：成功类（发送成功、渠道启用）
 * - info    = sky：进行中/信息（发送中、mock 演示、普通提示）
 * - warning = amber：降级/等待（冷却中）
 * - danger  = red：失败/危险（发送失败、自动禁用）
 * - neutral = slate：中性（手动停用、占位）
 */
export type BadgeVariant = 'success' | 'info' | 'warning' | 'danger' | 'neutral'

/** 淡底色 + 深色文字（浅色实底，深色半透明底） */
const variantClasses: Record<BadgeVariant, string> = {
  success:
    'bg-emerald-50 text-emerald-700 ring-emerald-600/20 dark:bg-emerald-500/10 dark:text-emerald-300 dark:ring-emerald-400/20',
  danger:
    'bg-red-50 text-red-700 ring-red-600/20 dark:bg-red-500/10 dark:text-red-300 dark:ring-red-400/20',
  warning:
    'bg-amber-50 text-amber-700 ring-amber-600/20 dark:bg-amber-500/10 dark:text-amber-300 dark:ring-amber-400/20',
  neutral:
    'bg-slate-100 text-slate-600 ring-slate-500/20 dark:bg-slate-500/10 dark:text-slate-400 dark:ring-slate-400/20',
  info: 'bg-sky-50 text-sky-700 ring-sky-600/20 dark:bg-sky-500/10 dark:text-sky-300 dark:ring-sky-400/20',
}

export interface BadgeProps extends HTMLAttributes<HTMLSpanElement> {
  variant?: BadgeVariant
}

/** 通用圆角状态徽章 */
export function Badge({ variant = 'neutral', className, ...rest }: BadgeProps) {
  return (
    <span
      className={cn(
        'inline-flex items-center gap-1 whitespace-nowrap rounded-full px-2 py-0.5 text-xs font-medium ring-1 ring-inset',
        variantClasses[variant],
        className,
      )}
      {...rest}
    />
  )
}

/* ---------------------------------- 渠道状态（SPEC §6） ---------------------------------- */

/** 后端 Channel.Status：0=enabled 1=manual_disabled 2=auto_disabled 3=cooldown */
export type ChannelStatus = 'enabled' | 'manual_disabled' | 'auto_disabled' | 'cooldown' | number

const CHANNEL_STATUS_BY_CODE = ['enabled', 'manual_disabled', 'auto_disabled', 'cooldown'] as const

type ChannelStatusKey = (typeof CHANNEL_STATUS_BY_CODE)[number]

/** 渠道状态 → 显示名 i18n key（组件内 t 渲染；as const 保证 key 为字面量联合供 TS 校验） */
const CHANNEL_STATUS_LABEL_KEYS = {
  enabled: 'ui:channelStatus.enabled',
  manual_disabled: 'ui:channelStatus.manualDisabled',
  auto_disabled: 'ui:channelStatus.autoDisabled',
  cooldown: 'ui:channelStatus.cooldown',
} as const

/** 渠道状态 → 徽章语义色（全局唯一映射：启用=success 停用=neutral 禁用=danger 冷却=warning） */
export const CHANNEL_STATUS_VARIANT: Record<ChannelStatusKey, BadgeVariant> = {
  enabled: 'success',
  manual_disabled: 'neutral',
  auto_disabled: 'danger',
  cooldown: 'warning',
}

/** 渠道状态枚举值（数字或字符串）→ 徽章 variant；未知值回落 neutral */
export function channelStatusVariant(status: ChannelStatus): BadgeVariant {
  const key =
    typeof status === 'number'
      ? (CHANNEL_STATUS_BY_CODE[status] ?? 'manual_disabled')
      : status
  return CHANNEL_STATUS_VARIANT[key as ChannelStatusKey] ?? 'neutral'
}

export function ChannelStatusBadge({ status }: { status: ChannelStatus }) {
  // 数组形式（i18next 27 类型）：映射存带 ns 前缀的 key，需绑定 ui 与 common
  const { t } = useTranslation(['ui', 'common'])
  const key =
    typeof status === 'number'
      ? (CHANNEL_STATUS_BY_CODE[status] ?? 'manual_disabled')
      : status
  const labelKey = CHANNEL_STATUS_LABEL_KEYS[key as ChannelStatusKey]
  return <Badge variant={channelStatusVariant(status)}>{labelKey ? t(labelKey) : String(status)}</Badge>
}

/* ---------------------------------- 发送日志状态 ---------------------------------- */

/** 后端 SendLog.Status：0=pending 1=sent 2=failed */
export type LogStatus = 'pending' | 'sent' | 'failed' | number

const LOG_STATUS_BY_CODE = ['pending', 'sent', 'failed'] as const

type LogStatusKey = (typeof LOG_STATUS_BY_CODE)[number]

/** 日志状态 → 显示名 i18n key（成功/失败与 common 同义，直接复用 common key） */
const LOG_STATUS_LABEL_KEYS = {
  pending: 'ui:logStatus.pending',
  sent: 'common:state.success',
  failed: 'common:state.failed',
} as const

/** 日志状态 → 徽章语义色（全局唯一映射：成功=success 进行中=info 失败=danger） */
export const LOG_STATUS_VARIANT: Record<LogStatusKey, BadgeVariant> = {
  pending: 'info',
  sent: 'success',
  failed: 'danger',
}

/** 日志状态枚举值（数字或字符串）→ 徽章 variant；未知值回落 danger */
export function logStatusVariant(status: LogStatus): BadgeVariant {
  const key = typeof status === 'number' ? (LOG_STATUS_BY_CODE[status] ?? 'failed') : status
  return LOG_STATUS_VARIANT[key as LogStatusKey] ?? 'danger'
}

export function LogStatusBadge({ status }: { status: LogStatus }) {
  // 数组形式（i18next 27 类型）：映射存带 ns 前缀的 key，需绑定 ui 与 common
  const { t } = useTranslation(['ui', 'common'])
  const key = typeof status === 'number' ? (LOG_STATUS_BY_CODE[status] ?? 'failed') : status
  const labelKey = LOG_STATUS_LABEL_KEYS[key as LogStatusKey]
  return <Badge variant={logStatusVariant(status)}>{labelKey ? t(labelKey) : String(status)}</Badge>
}

/* ---------------------------------- 平台徽章 ---------------------------------- */

const PROVIDER_LABELS: Record<string, string> = {
  resend: 'Resend',
  brevo: 'Brevo',
  mailjet: 'Mailjet',
  smtp2go: 'SMTP2GO',
  mailgun: 'Mailgun',
  smtp: 'SMTP',
  mock: 'Mock',
}

export function ProviderBadge({ provider }: { provider: string }) {
  return (
    <Badge variant="info" className="font-mono text-[11px]">
      {PROVIDER_LABELS[provider] ?? provider}
    </Badge>
  )
}

/* ---------------------------------- 审计动作徽章（E4） ---------------------------------- */

/**
 * 审计动作 → 前缀着色（全局唯一映射，同 variantClasses 风格的手写淡底色）：
 * channel 蓝 / key 紫 / user 琥珀 / settings 红 / purge·log 深灰 / backup 绿 /
 * template 青绿。未知前缀（新版本后端新增动作）回落深灰中性色。
 */
const AUDIT_PREFIX_CLASSES: Record<string, string> = {
  channel:
    'bg-sky-50 text-sky-700 ring-sky-600/20 dark:bg-sky-500/10 dark:text-sky-300 dark:ring-sky-400/20',
  key: 'bg-violet-50 text-violet-700 ring-violet-600/20 dark:bg-violet-500/10 dark:text-violet-300 dark:ring-violet-400/20',
  user:
    'bg-amber-50 text-amber-700 ring-amber-600/20 dark:bg-amber-500/10 dark:text-amber-300 dark:ring-amber-400/20',
  settings:
    'bg-red-50 text-red-700 ring-red-600/20 dark:bg-red-500/10 dark:text-red-300 dark:ring-red-400/20',
  purge:
    'bg-slate-100 text-slate-600 ring-slate-500/20 dark:bg-slate-500/10 dark:text-slate-400 dark:ring-slate-400/20',
  log: 'bg-slate-100 text-slate-600 ring-slate-500/20 dark:bg-slate-500/10 dark:text-slate-400 dark:ring-slate-400/20',
  backup:
    'bg-emerald-50 text-emerald-700 ring-emerald-600/20 dark:bg-emerald-500/10 dark:text-emerald-300 dark:ring-emerald-400/20',
  // 邮件模板动作（template_create/update/delete）：青绿系，与 backup 绿区分
  template:
    'bg-teal-50 text-teal-700 ring-teal-600/20 dark:bg-teal-500/10 dark:text-teal-300 dark:ring-teal-400/20',
}

const AUDIT_FALLBACK_CLASSES = AUDIT_PREFIX_CLASSES.purge

/** 审计动作前缀（首段，如 channel_create → channel）→ 徽章配色类；未知前缀回落深灰 */
export function auditActionClasses(action: string): string {
  const prefix = action.split('_', 1)[0]
  return AUDIT_PREFIX_CLASSES[prefix] ?? AUDIT_FALLBACK_CLASSES
}

/** 审计动作徽章：按动作前缀着色，展示中文标签、悬停显原始动作枚举（E4 审计页专用） */
export function AuditActionBadge({ action, label }: { action: string; label: string }) {
  return (
    <span
      title={action}
      className={cn(
        'inline-flex items-center whitespace-nowrap rounded-full px-2 py-0.5 text-xs font-medium ring-1 ring-inset',
        auditActionClasses(action),
      )}
    >
      {label}
    </span>
  )
}
