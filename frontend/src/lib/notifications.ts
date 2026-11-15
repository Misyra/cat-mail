import { useQuery, type QueryClient } from '@tanstack/react-query'

import { api } from '@/lib/api'
import type { BadgeVariant } from '@/components/badges'
import type { SystemSettings } from '@/lib/settings'

/**
 * 站内通知数据层（后端契约，snake_case 原样字段名）：
 * - GET /api/notifications?page&page_size 列表（附带 total / unread）
 * - GET /api/notifications/unread_count 未读数（顶栏 30s 轮询）
 * - POST /api/notifications/read 标记已读（ids=null 全部已读）
 * - POST /api/settings/notify/test 通知渠道连通性测试
 * 另含「失败通知」设置四字段的枚举与序列化辅助（与 GET/PUT /api/settings 同一体）。
 */

/* ---------------------------------- 通知列表 ---------------------------------- */

/** GET /api/notifications 列表项 */
export interface NotificationItem {
  id: number
  /** 事件类型：channel_disabled | channel_cooldown | channel_recovered | send_failed | update_available */
  event: string
  /** 级别：error | warn | info */
  level: string
  title: string
  body: string
  /** 关联渠道 id；null 表示无关联（点击后不跳转） */
  channel_id: number | null
  read: boolean
  created_at: string
}

export interface NotificationListResponse {
  items: NotificationItem[]
  total: number
  unread: number
}

export interface UnreadCountResponse {
  count: number
}

export const NOTIFICATIONS_PAGE_SIZE = 20

const NOTIFICATIONS_UNREAD_KEY = ['notifications', 'unread'] as const

const notificationsListKey = (page: number) => ['notifications', 'list', page] as const

/** 顶栏未读数：30s 轮询 */
export function useUnreadNotifications() {
  return useQuery({
    queryKey: NOTIFICATIONS_UNREAD_KEY,
    queryFn: () => api<UnreadCountResponse>('/api/notifications/unread_count'),
    refetchInterval: 30_000,
  })
}

/** 通知列表（通知面板打开时才启用请求） */
export function useNotifications(page = 1, enabled = true) {
  return useQuery({
    queryKey: notificationsListKey(page),
    queryFn: () =>
      api<NotificationListResponse>(
        `/api/notifications?page=${page}&page_size=${NOTIFICATIONS_PAGE_SIZE}`,
      ),
    enabled,
  })
}

/** POST /api/notifications/read：ids=null 标记全部已读 */
export async function markNotificationsRead(ids: number[] | null): Promise<void> {
  await api<unknown>('/api/notifications/read', { method: 'POST', body: { ids } })
}

/** 已读写操作成功后失效未读数与列表缓存 */
export function invalidateNotifications(queryClient: QueryClient): void {
  void queryClient.invalidateQueries({ queryKey: ['notifications'] })
}

/* ---------------------------------- 通知测试 ---------------------------------- */

/** POST /api/settings/notify/test 响应：webhook 未启用时 webhook_ok=true 且 webhook_error="" */
export interface NotifyTestResponse {
  inapp_ok: boolean
  webhook_ok: boolean
  webhook_error: string
}

/** 通知连通性测试（无请求体，后端按已保存的设置发送） */
export function testNotifySettings(): Promise<NotifyTestResponse> {
  return api<NotifyTestResponse>('/api/settings/notify/test', { method: 'POST' })
}

/* ---------------------------------- 失败通知设置字段 ---------------------------------- */

export type NotifyEventKey =
  | 'channel_disabled'
  | 'channel_cooldown'
  | 'channel_recovered'
  | 'send_failed'
  | 'update_available'
  | 'bounce_rate_exceeded'

export type NotifyLevel = 'error' | 'warn' | 'info'

/** 事件显示名 / 说明的词典 key（notifications 命名空间；lib 层不直接调 i18n，渲染处 t() 出文案） */
export type NotifyEventLabelKey =
  | 'notifications:event.channelDisabledLabel'
  | 'notifications:event.channelCooldownLabel'
  | 'notifications:event.channelRecoveredLabel'
  | 'notifications:event.sendFailedLabel'
  | 'notifications:event.updateAvailableLabel'
  | 'notifications:event.bounceRateExceededLabel'

export type NotifyEventDescriptionKey =
  | 'notifications:event.channelDisabledDesc'
  | 'notifications:event.channelCooldownDesc'
  | 'notifications:event.channelRecoveredDesc'
  | 'notifications:event.sendFailedDesc'
  | 'notifications:event.updateAvailableDesc'
  | 'notifications:event.bounceRateExceededDesc'

export interface NotifyEventMeta {
  key: NotifyEventKey
  /** 事件显示名词典 key（渲染处 t(labelKey)） */
  labelKey: NotifyEventLabelKey
  level: NotifyLevel
  /** 事件说明词典 key（渲染处 t(descriptionKey)） */
  descriptionKey: NotifyEventDescriptionKey
}

/** 六类通知事件（顺序即序列化顺序），文案存词典 key（notifications 命名空间），级别按后端契约 */
export const NOTIFY_EVENTS: readonly NotifyEventMeta[] = [
  {
    key: 'channel_disabled',
    labelKey: 'notifications:event.channelDisabledLabel',
    level: 'error',
    descriptionKey: 'notifications:event.channelDisabledDesc',
  },
  {
    key: 'channel_cooldown',
    labelKey: 'notifications:event.channelCooldownLabel',
    level: 'warn',
    descriptionKey: 'notifications:event.channelCooldownDesc',
  },
  {
    key: 'channel_recovered',
    labelKey: 'notifications:event.channelRecoveredLabel',
    level: 'info',
    descriptionKey: 'notifications:event.channelRecoveredDesc',
  },
  {
    key: 'send_failed',
    labelKey: 'notifications:event.sendFailedLabel',
    level: 'error',
    descriptionKey: 'notifications:event.sendFailedDesc',
  },
  {
    key: 'update_available',
    labelKey: 'notifications:event.updateAvailableLabel',
    level: 'info',
    descriptionKey: 'notifications:event.updateAvailableDesc',
  },
  {
    key: 'bounce_rate_exceeded',
    labelKey: 'notifications:event.bounceRateExceededLabel',
    level: 'warn',
    descriptionKey: 'notifications:event.bounceRateExceededDesc',
  },
]

const NOTIFY_EVENT_KEYS = new Set<string>(NOTIFY_EVENTS.map((e) => e.key))

/** 级别 → 词典 key（设置页级别徽章，渲染处 t() 出文案） */
export type NotifyLevelLabelKey =
  | 'notifications:level.error'
  | 'notifications:level.warn'
  | 'notifications:level.info'

export const NOTIFY_LEVEL_LABEL_KEYS: Record<NotifyLevel, NotifyLevelLabelKey> = {
  error: 'notifications:level.error',
  warn: 'notifications:level.warn',
  info: 'notifications:level.info',
}

/** 级别 → 现有语义徽章色（全局唯一映射：error=danger warn=warning info=info）；未知值回落 neutral */
export function notifyLevelVariant(level: string): BadgeVariant {
  if (level === 'error') return 'danger'
  if (level === 'warn') return 'warning'
  if (level === 'info') return 'info'
  return 'neutral'
}

/** 级别 → 通知面板未读色条颜色；未知值回落中性 */
export function notifyLevelBarClass(level: string): string {
  if (level === 'error') return 'bg-red-500'
  if (level === 'warn') return 'bg-amber-500'
  if (level === 'info') return 'bg-sky-500'
  return 'bg-slate-400'
}

/** GET/PUT /api/settings 的失败通知字段（与既有设置字段同一体；notify_webhook_secret
 * 为安全加固三件套 B 的签名密钥，GET 明文回显供展示/复制，PUT 整体忽略——变更仅走
 * POST /api/settings/notify/webhook-secret） */
export interface NotifySettingsFields {
  notify_inapp_enabled: boolean
  notify_webhook_enabled: boolean
  notify_webhook_url: string
  /** 通知 Webhook 签名密钥（32 字节随机 hex；空=未配置，投递不带签名头） */
  notify_webhook_secret: string
  /** 逗号分隔的事件 key；空串 = 全部开启 */
  notify_events: string
}

export type SystemSettingsWithNotify = SystemSettings & NotifySettingsFields

/** 从设置响应提取通知字段（后端未就绪/缺字段时回落默认值） */
export function normalizeNotifyFields(s: SystemSettings): NotifySettingsFields {
  const withNotify = s as Partial<SystemSettingsWithNotify>
  return {
    notify_inapp_enabled: withNotify.notify_inapp_enabled ?? true,
    notify_webhook_enabled: withNotify.notify_webhook_enabled ?? false,
    notify_webhook_url: withNotify.notify_webhook_url ?? '',
    notify_webhook_secret: withNotify.notify_webhook_secret ?? '',
    notify_events: withNotify.notify_events ?? '',
  }
}

/** POST /api/settings/notify/webhook-secret 响应：生成/轮换后的签名密钥（明文） */
export interface NotifyWebhookSecretResponse {
  webhook_secret: string
}

/** 生成（rotate=false，仅未配置时）或轮换（rotate=true）通知 Webhook 签名密钥 */
export function rotateNotifyWebhookSecret(rotate: boolean): Promise<NotifyWebhookSecretResponse> {
  return api<NotifyWebhookSecretResponse>('/api/settings/notify/webhook-secret', {
    method: 'POST',
    body: rotate ? { rotate: true } : undefined,
  })
}

/** notify_events 逗号串 → 勾选集合；空串 = 全部开启（契约语义），未知 token 忽略 */
export function parseNotifyEvents(value: string | undefined): Set<NotifyEventKey> {
  const trimmed = (value ?? '').trim()
  if (!trimmed) return new Set(NOTIFY_EVENTS.map((e) => e.key))
  const keys = new Set<NotifyEventKey>()
  for (const token of trimmed.split(',')) {
    const key = token.trim() as NotifyEventKey
    if (NOTIFY_EVENT_KEYS.has(key)) keys.add(key)
  }
  return keys
}

/** 勾选集合 → notify_events 逗号串（按 NOTIFY_EVENTS 固定顺序） */
export function serializeNotifyEvents(events: ReadonlySet<NotifyEventKey>): string {
  return NOTIFY_EVENTS.filter((e) => events.has(e.key))
    .map((e) => e.key)
    .join(',')
}
