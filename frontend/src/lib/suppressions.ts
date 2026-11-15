import { keepPreviousData, useQuery, type QueryClient } from '@tanstack/react-query'

import { api } from '@/lib/api'

/**
 * 收件人抑制列表数据层（送达链路三件套，snake_case 原样字段名）。
 * reason：bounce（hard bounce 事件自动入库）/ complaint（complained 自动入库）/
 * manual（手动添加）。命中列表的收件人在所有发送入口被拒（400，
 * code=recipient_suppressed）。
 */

/** 抑制原因（空串 = 全部筛选） */
export type SuppressionReason = '' | 'bounce' | 'complaint' | 'manual'

/** 抑制列表项 */
export interface Suppression {
  id: number
  email: string
  reason: Exclude<SuppressionReason, ''>
  channel_id: number
  channel_name: string
  detail: string
  created_at: string
}

export interface SuppressionListResponse {
  items: Suppression[]
  total: number
}

export const SUPPRESSIONS_PAGE_SIZES = [20, 50, 100] as const
export const SUPPRESSIONS_PAGE_SIZE = 20

/** 校验 URL 里的 page_size（非法值回落默认 20） */
export function normalizeSuppressionPageSize(raw: string | null): number {
  const n = raw === null ? NaN : Number.parseInt(raw, 10)
  return (SUPPRESSIONS_PAGE_SIZES as readonly number[]).includes(n) ? n : SUPPRESSIONS_PAGE_SIZE
}

/** 原因 → suppressions 词典 key（文案见 locales/<lang>/suppressions.json 的 reason.*，消费处 t()） */
export const SUPPRESSION_REASON_KEYS = {
  bounce: 'reason.bounce',
  complaint: 'reason.complaint',
  manual: 'reason.manual',
} as const satisfies Record<Exclude<SuppressionReason, ''>, string>

export interface SuppressionListParams {
  page: number
  pageSize?: number
  reason?: SuppressionReason
  channelId?: number
  /** 邮箱模糊搜索 */
  q?: string
}

/** GET /api/suppressions?page&page_size&reason&channel_id&q */
export function useSuppressions(params: SuppressionListParams) {
  const {
    page,
    pageSize = SUPPRESSIONS_PAGE_SIZE,
    reason = '',
    channelId = 0,
    q = '',
  } = params
  return useQuery({
    queryKey: ['suppressions', 'list', { page, pageSize, reason, channelId, q }],
    queryFn: () => {
      const sp = new URLSearchParams()
      sp.set('page', String(page))
      sp.set('page_size', String(pageSize))
      if (reason) sp.set('reason', reason)
      if (channelId > 0) sp.set('channel_id', String(channelId))
      if (q) sp.set('q', q)
      return api<SuppressionListResponse>(`/api/suppressions?${sp.toString()}`)
    },
    placeholderData: keepPreviousData,
  })
}

/** POST /api/suppressions 手动添加（reason 恒为 manual，由后端固定） */
export function createSuppression(email: string, detail: string): Promise<Suppression> {
  return api<Suppression>('/api/suppressions', { method: 'POST', body: { email, detail } })
}

/** DELETE /api/suppressions/:id 解除（物理删除；之后 hard bounce 仍会自动重新入库） */
export function deleteSuppression(id: number): Promise<{ ok: boolean }> {
  return api<{ ok: boolean }>(`/api/suppressions/${id}`, { method: 'DELETE' })
}

/** 抑制列表写操作成功后失效列表缓存 */
export function invalidateSuppressions(queryClient: QueryClient): void {
  void queryClient.invalidateQueries({ queryKey: ['suppressions'] })
}
