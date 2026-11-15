import { useQuery, type UseQueryResult } from '@tanstack/react-query'

import { api } from '@/lib/api'

/** GET /api/auth/me 响应（SPEC §5 + C4 两步验证状态；secret/恢复码不在此出现） */
export interface MeResponse {
  id: number
  username: string
  totp_enabled: boolean
}

export const ME_QUERY_KEY = ['auth', 'me'] as const

/** 当前登录用户；401（未登录）不重试，由 RequireAuth 跳转登录页 */
export function useMe(): UseQueryResult<MeResponse, Error> {
  return useQuery({
    queryKey: ME_QUERY_KEY,
    queryFn: () => api<MeResponse>('/api/auth/me'),
    retry: false,
    staleTime: 5 * 60 * 1000,
  })
}
