import { keepPreviousData, useQuery } from '@tanstack/react-query'

import { api } from '@/lib/api'

/**
 * 送达事件数据层（送达链路三件套，GET /api/mail-events，snake_case 原样字段名）。
 * 事件为后端归一化口径：sent/delivered/bounced（hard|soft）/complained/raw；
 * message_id/send_log_id 为按平台消息 ID 尽力反查的发送日志关联（未解析为空/0）。
 */

/** 归一化事件类型（空串 = 全部筛选） */
export type MailEventType = '' | 'sent' | 'delivered' | 'bounced' | 'complained' | 'raw'

/** 退信类型（仅 bounced 事件携带） */
export type MailBounceType = '' | 'hard' | 'soft'

/** 送达事件实体（GET /api/mail-events 列表项） */
export interface MailEvent {
  id: number
  channel_id: number
  channel_name: string
  provider: string
  event: Exclude<MailEventType, ''>
  bounce_type: MailBounceType
  provider_message_id: string
  /** 关联 SendLog.MessageID（尽力解析，空 = 未关联） */
  message_id: string
  send_log_id: number
  recipient: string
  detail: string
  /** 平台事件时间 */
  occurred_at: string
  /** 摄取时间 */
  created_at: string
}

/** 送达事件详情（列表项 + raw 原始报文） */
export interface MailEventDetail extends MailEvent {
  /** 原始报文（JSON 对象透传；非法 JSON 报文回退为字符串） */
  raw?: unknown
}

export interface MailEventListResponse {
  items: MailEvent[]
  total: number
}

export const MAIL_EVENTS_PAGE_SIZES = [20, 50, 100] as const
export const MAIL_EVENTS_PAGE_SIZE = 20

/** 校验 URL 里的 page_size（非法值回落默认 20） */
export function normalizeMailEventPageSize(raw: string | null): number {
  const n = raw === null ? NaN : Number.parseInt(raw, 10)
  return (MAIL_EVENTS_PAGE_SIZES as readonly number[]).includes(n) ? n : MAIL_EVENTS_PAGE_SIZE
}

/** 事件类型显示名的 i18n key（EventsPage 消费处 t(key) 渲染；未知事件值回退原样展示） */
export const MAIL_EVENT_LABELS: Record<
  Exclude<MailEventType, ''>,
  `events:type.${Exclude<MailEventType, ''>}`
> = {
  sent: 'events:type.sent',
  delivered: 'events:type.delivered',
  bounced: 'events:type.bounced',
  complained: 'events:type.complained',
  raw: 'events:type.raw',
}

/** 事件类型 → 徽章语义色（success=送达 info=受理 danger=退信/投诉 neutral=其他） */
export const MAIL_EVENT_VARIANTS: Record<
  Exclude<MailEventType, ''>,
  'success' | 'info' | 'danger' | 'neutral'
> = {
  sent: 'info',
  delivered: 'success',
  bounced: 'danger',
  complained: 'danger',
  raw: 'neutral',
}

export interface MailEventListParams {
  page: number
  pageSize?: number
  event?: MailEventType
  /** 退信类型筛选（仅 event=bounced 或全部时有意义） */
  bounceType?: MailBounceType
  channelId?: number
  /** 收件人模糊搜索 */
  q?: string
}

/** GET /api/mail-events?page&page_size&event&bounce_type&channel_id&q */
export function useMailEvents(params: MailEventListParams) {
  const {
    page,
    pageSize = MAIL_EVENTS_PAGE_SIZE,
    event = '',
    bounceType = '',
    channelId = 0,
    q = '',
  } = params
  return useQuery({
    queryKey: ['mail-events', 'list', { page, pageSize, event, bounceType, channelId, q }],
    queryFn: () => {
      const sp = new URLSearchParams()
      sp.set('page', String(page))
      sp.set('page_size', String(pageSize))
      if (event) sp.set('event', event)
      if (bounceType) sp.set('bounce_type', bounceType)
      if (channelId > 0) sp.set('channel_id', String(channelId))
      if (q) sp.set('q', q)
      return api<MailEventListResponse>(`/api/mail-events?${sp.toString()}`)
    },
    placeholderData: keepPreviousData,
  })
}

/** GET /api/mail-events/:id 详情（含 raw 报文） */
export function useMailEventDetail(id: number | null) {
  return useQuery({
    queryKey: ['mail-events', 'detail', id],
    queryFn: () => api<MailEventDetail>(`/api/mail-events/${id}`),
    enabled: id !== null,
  })
}
