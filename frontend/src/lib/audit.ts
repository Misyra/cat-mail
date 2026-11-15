import { keepPreviousData, useQuery } from '@tanstack/react-query'
import i18next from 'i18next'

import { api } from '@/lib/api'

/**
 * 管理操作审计数据层（E4，GET /api/audit，snake_case 原样字段名）。
 * 审计行为后端在各管理 handler 成功路径写入（失败不记），本层只读。
 */

/** 审计日志行（GET /api/audit 列表项） */
export interface AuditItem {
  id: number
  /** 操作者（会话用户名） */
  username: string
  /** 动作枚举（AUDIT_ACTIONS 的取值；前缀标识资源类别） */
  action: string
  /** 目标摘要，形如 "channel#3 name=示例渠道" */
  target: string
  /** 补充摘要（后端截断至 500 字符；仅字段名/计数，不含敏感值） */
  detail: string
  /** 操作来源 IP */
  ip: string
  created_at: string
}

export interface AuditListResponse {
  items: AuditItem[]
  total: number
}

/**
 * 动作枚举（硬编码，与后端 model.Audit* 常量一一对应）：用于筛选下拉与本地化标签。
 * labelKey 为 audit 词典的显示文案 key（value 是发往后端的筛选字段值，保持原样）。
 * 后端 action 筛选为前缀匹配，自由输入 "channel" 可命中全部 channel_* 动作。
 */
export const AUDIT_ACTIONS = [
  { value: 'channel_create', labelKey: 'audit:actions.channelCreate' },
  { value: 'channel_update', labelKey: 'audit:actions.channelUpdate' },
  { value: 'channel_delete', labelKey: 'audit:actions.channelDelete' },
  { value: 'channel_status', labelKey: 'audit:actions.channelStatus' },
  { value: 'channel_duplicate', labelKey: 'audit:actions.channelDuplicate' },
  { value: 'channel_webhook_token', labelKey: 'audit:actions.channelWebhookToken' },
  { value: 'template_create', labelKey: 'audit:actions.templateCreate' },
  { value: 'template_update', labelKey: 'audit:actions.templateUpdate' },
  { value: 'template_delete', labelKey: 'audit:actions.templateDelete' },
  { value: 'key_create', labelKey: 'audit:actions.keyCreate' },
  { value: 'key_update', labelKey: 'audit:actions.keyUpdate' },
  { value: 'key_delete', labelKey: 'audit:actions.keyDelete' },
  { value: 'key_webhook_secret', labelKey: 'audit:actions.keyWebhookSecret' },
  { value: 'user_create', labelKey: 'audit:actions.userCreate' },
  { value: 'user_reset_password', labelKey: 'audit:actions.userResetPassword' },
  { value: 'user_delete', labelKey: 'audit:actions.userDelete' },
  { value: 'sessions_revoke', labelKey: 'audit:actions.sessionsRevoke' },
  { value: 'password_reset_linkage', labelKey: 'audit:actions.passwordResetLinkage' },
  { value: 'settings_update', labelKey: 'audit:actions.settingsUpdate' },
  { value: 'settings_webhook_secret', labelKey: 'audit:actions.settingsWebhookSecret' },
  { value: 'purge_logs', labelKey: 'audit:actions.purgeLogs' },
  { value: 'backup_create', labelKey: 'audit:actions.backupCreate' },
  { value: 'backup_delete', labelKey: 'audit:actions.backupDelete' },
  { value: 'suppression_create', labelKey: 'audit:actions.suppressionCreate' },
  { value: 'suppression_delete', labelKey: 'audit:actions.suppressionDelete' },
] as const

/** 动作枚举值 → 本地化标签（渲染时取当前语言）；未知取值（新版本后端）原样展示 */
export function auditActionLabel(action: string): string {
  const found = AUDIT_ACTIONS.find((a) => a.value === action)
  return found ? i18next.t(found.labelKey) : action
}

/** 每页条数可选项（后端上限 100） */
export const AUDIT_PAGE_SIZES = [20, 50, 100] as const

export const AUDIT_PAGE_SIZE = 20

/** 校验 URL 里的 page_size（非法值回落默认 20） */
export function normalizeAuditPageSize(raw: string | null): number {
  const n = raw === null ? NaN : Number.parseInt(raw, 10)
  return (AUDIT_PAGE_SIZES as readonly number[]).includes(n) ? n : AUDIT_PAGE_SIZE
}

export interface AuditListParams {
  page: number
  /** 默认 20，后端上限 100 */
  pageSize?: number
  /** 动作前缀筛选（空串 = 全部；"channel" 命中全部 channel_*） */
  action?: string
  /** 操作者精确筛选（空串 = 全部） */
  username?: string
}

/** GET /api/audit?page&page_size&action&username（created_at 倒序；翻页保留前页数据避免闪烁） */
export function useAuditLogs(params: AuditListParams) {
  const { page, pageSize = AUDIT_PAGE_SIZE, action = '', username = '' } = params
  return useQuery({
    queryKey: ['audit', 'list', { page, pageSize, action, username }],
    queryFn: () => {
      const sp = new URLSearchParams()
      sp.set('page', String(page))
      sp.set('page_size', String(pageSize))
      if (action) sp.set('action', action)
      if (username) sp.set('username', username)
      return api<AuditListResponse>(`/api/audit?${sp.toString()}`)
    },
    placeholderData: keepPreviousData,
  })
}
