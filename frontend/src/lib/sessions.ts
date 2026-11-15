import { useQuery, type QueryClient } from '@tanstack/react-query'

import { api } from '@/lib/api'

/**
 * 服务端会话注册表数据层（安全加固三件套，C，snake_case 原样字段名）：
 * - GET /api/sessions 本人活跃会话列表（当前会话带 current 标记）
 * - DELETE /api/sessions/:id 踢指定会话（不能踢当前）
 * - POST /api/sessions/revoke-others 踢其余全部
 * 吊销只写服务端注册表（revoked_at），不影响 API Key（联动停用仅密码重置触发）。
 */

/** GET /api/sessions 列表项 */
export interface SessionItem {
  id: string
  ip: string
  user_agent: string
  created_at: string
  last_seen_at: string
  expires_at: string
  current: boolean
}

export interface SessionListResponse {
  items: SessionItem[]
}

export const SESSIONS_QUERY_KEY = ['sessions', 'list'] as const

/** GET /api/sessions */
export function useSessions() {
  return useQuery({
    queryKey: SESSIONS_QUERY_KEY,
    queryFn: () => api<SessionListResponse>('/api/sessions'),
  })
}

/** 会话写操作成功后失效列表缓存 */
export function invalidateSessions(queryClient: QueryClient): void {
  void queryClient.invalidateQueries({ queryKey: ['sessions'] })
}

/** DELETE /api/sessions/:id：踢指定会话（当前会话后端 400 拒绝） */
export async function revokeSession(id: string): Promise<void> {
  await api<{ ok: boolean }>(`/api/sessions/${encodeURIComponent(id)}`, { method: 'DELETE' })
}

/** POST /api/sessions/revoke-others：下线其余全部，返回吊销条数 */
export function revokeOtherSessions(): Promise<{ revoked: number }> {
  return api<{ revoked: number }>('/api/sessions/revoke-others', { method: 'POST' })
}
