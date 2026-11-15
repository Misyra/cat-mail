import { useQuery, type QueryClient } from '@tanstack/react-query'

import { api } from '@/lib/api'

/**
 * API Key 数据层（SPEC §5，snake_case 原样字段名）。
 * 列表只返回掩码 key（cm-abcd…wxyz）；明文 key 仅 POST /api/keys 创建时一次性返回。
 */

/** GET /api/keys 列表项 */
export interface ApiKeyItem {
  id: number
  name: string
  /** 掩码形态，如 cm-abcd…wxyz */
  masked_key: string
  enabled: boolean
  /** 调用方回执 Webhook 地址（D1，空=不推送） */
  webhook_url: string
  /** 回执事件白名单 csv（send_accepted|send_failed 子集，空=不推送） */
  webhook_events: string
  /** 回执签名密钥（安全加固三件套，B）：创建 Key 自动生成，明文回显供查看/复制，
   * POST /api/keys/:id/webhook-secret 轮换 */
  webhook_secret: string
  /** 日额度：每日发送成功条数上限（B5，0=不限） */
  daily_quota: number
  /** 限速：发送接口每秒请求数上限（B5，0=不限） */
  rate_per_sec: number
  /** 联动停用原因（安全加固三件套，C）：password_reset=密码重置联动停用
   * （enabled=false 时展示提示徽标，手动启用即清空），空串=常规停用 */
  disabled_reason: string
  /** 该 Key 历史累计成功发送条数 */
  total_sent: number
  last_used_at: string | null
  created_at: string
}

export interface ApiKeyListResponse {
  items: ApiKeyItem[]
}

/** POST /api/keys 响应：明文 key 仅此一次返回 */
export interface CreatedKey {
  id: number
  key: string
}

/** POST /api/keys 请求体：daily_quota/rate_per_sec 为 0=不限（B5） */
export interface ApiKeyCreatePayload {
  name: string
  daily_quota?: number
  rate_per_sec?: number
}

/** PUT /api/keys/:id 请求体（均可选：未提供=保留；webhook_* 空串=清除；
 * daily_quota/rate_per_sec 显式 0=改为不限，B5） */
export interface ApiKeyUpdatePayload {
  name?: string
  enabled?: boolean
  webhook_url?: string
  webhook_events?: string
  daily_quota?: number
  rate_per_sec?: number
}

/** 回执事件元数据（Key 编辑弹窗复选框，与后端 receipt.Events 对齐；label/description 为
 * 词典 key——lib 非组件环境不取译文，由 KeysPage 消费处经 t() 渲染） */
export const WEBHOOK_EVENTS = [
  {
    key: 'send_accepted',
    labelKey: 'events.sendAccepted',
    descriptionKey: 'events.sendAcceptedDesc',
  },
  {
    key: 'send_failed',
    labelKey: 'events.sendFailed',
    descriptionKey: 'events.sendFailedDesc',
  },
] as const

/* ---------------------------------- 回执测试（D1） ---------------------------------- */

/** POST /api/keys/:id/webhook/test 响应：推送失败也是 HTTP 200，成败经 ok/error 字段上报 */
export interface KeyWebhookTestResponse {
  ok: boolean
  /** 仅失败（ok=false）时携带错误文案（网络不可达 / 非 2xx 等） */
  error?: string
}

/** 回执连通性测试（无请求体，后端按该 Key 已保存的 webhook_url 推送测试回执） */
export function testKeyWebhook(id: number): Promise<KeyWebhookTestResponse> {
  return api<KeyWebhookTestResponse>(`/api/keys/${id}/webhook/test`, { method: 'POST' })
}

/* ------------------------------ 回执签名密钥（安全加固三件套，B） ------------------------------ */

/** POST /api/keys/:id/webhook-secret 响应：轮换后的新签名密钥（仅此一次返回明文） */
export interface KeyWebhookSecretResponse {
  webhook_secret: string
}

/** 轮换该 Key 的回执签名密钥（32 字节随机 hex；轮换后调用方需同步更新校验密钥） */
export function resetKeyWebhookSecret(id: number): Promise<KeyWebhookSecretResponse> {
  return api<KeyWebhookSecretResponse>(`/api/keys/${id}/webhook-secret`, { method: 'POST' })
}

/** 将 csv 白名单解析为事件集合（供复选框回显） */
export function parseWebhookEvents(csv: string): Set<string> {
  return new Set(
    csv
      .split(',')
      .map((e) => e.trim())
      .filter(Boolean),
  )
}

export const KEYS_QUERY_KEY = ['keys', 'list'] as const

/** GET /api/keys */
export function useApiKeys() {
  return useQuery({
    queryKey: KEYS_QUERY_KEY,
    queryFn: () => api<ApiKeyListResponse>('/api/keys'),
  })
}

/** Key 写操作成功后失效列表缓存 */
export function invalidateKeys(queryClient: QueryClient): void {
  void queryClient.invalidateQueries({ queryKey: ['keys'] })
}
