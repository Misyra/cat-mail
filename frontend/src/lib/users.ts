import { useQuery, type QueryClient } from '@tanstack/react-query'

import { api } from '@/lib/api'

/**
 * 管理账号数据层（SPEC §5，snake_case 原样字段名）。
 * 登录用户均为管理员角色；不能删自己、至少保留一个账号（后端兜底校验）。
 */

/** GET /api/users 列表项 */
export interface UserItem {
  id: number
  username: string
  created_at: string
}

export interface UserListResponse {
  items: UserItem[]
}

export const USERS_QUERY_KEY = ['users', 'list'] as const

/** GET /api/users */
export function useUsers() {
  return useQuery({
    queryKey: USERS_QUERY_KEY,
    queryFn: () => api<UserListResponse>('/api/users'),
  })
}

/** 账号写操作成功后失效列表缓存 */
export function invalidateUsers(queryClient: QueryClient): void {
  void queryClient.invalidateQueries({ queryKey: ['users'] })
}
