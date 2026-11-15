import { useQuery, type QueryClient } from '@tanstack/react-query'

import { api } from '@/lib/api'

/**
 * 渠道实体（SPEC §5 GET /api/channels 的 ChannelDTO，snake_case 原样字段名）。
 * 列表接口脱敏：api_key/api_secret 只返回前4后4掩码，proxy_url 含凭据时掩码。
 */
export interface Channel {
  id: number
  name: string
  provider: string
  /** 0=enabled 1=manual_disabled 2=auto_disabled 3=cooldown */
  status: number
  priority: number
  /** 编辑态为掩码值；PUT 时传空 = 保留原值 */
  api_key?: string
  /** mailjet 私钥；编辑态为掩码值 */
  api_secret?: string
  /** mailgun 发送域名 */
  domain?: string
  /** mailgun: us(默认) | eu */
  region?: string
  /** 出站代理 http:// 或 socks5://；空=直连 */
  proxy_url?: string
  /** 允许的发件域名，逗号分隔；空=不限制 */
  allowed_domains?: string
  from_email?: string
  from_name?: string
  smtp_host?: string
  smtp_port?: number
  smtp_user?: string
  smtp_pass?: string
  /** none | starttls | tls */
  smtp_tls?: string
  /** 0 = 用 provider 默认 */
  daily_limit_override: number
  monthly_limit_override: number
  /** 出站限速（封/秒，令牌桶排队）；0 = 不限 */
  rate_limit_per_sec: number
  used_day: number
  used_month: number
  /** 0 = 不限 */
  effective_daily_limit: number
  effective_monthly_limit: number
  /** 冷却截止时间（status=3 冷却中时有值；ChannelDTO 同名字段） */
  cooldown_until?: string | null
  /** 连续失败次数（达到阈值自动禁用；ChannelDTO 同名字段） */
  consecutive_failures?: number
  last_error?: string
  auto_disabled_reason?: string
  last_used_at?: string | null
  created_at?: string
  /** 入站送达事件 URL token（送达链路三件套；明文回显供拼平台 webhook 地址） */
  webhook_token?: string
  /** 平台签名密钥（Resend whsec_/Mailgun signing key；掩码回显，传空=保留原值） */
  webhook_secret?: string
  /** 近 7 日健康度（仅列表接口携带；零日志渠道为 null） */
  health?: ChannelHealth | null
}

export interface ChannelListResponse {
  items: Channel[]
}

/* ---------------------------------- 渠道健康度（A3） ---------------------------------- */

/** 7 日失败原因聚合项（reason 为 error 前 120 字符归并，count 次数） */
export interface ChannelTopError {
  reason: string
  count: number
}

/**
 * 渠道近 7 日健康（channelDTO.health，仅 GET /api/channels 列表填充）。
 * 口径与 stats 一致：排除测试发送（is_test=false），本地日历日窗口（含今天共 7 天）。
 * success_rate 为 0-100 整数；零日志渠道后端返回 null（前端展示灰色「—」）。
 * 送达侧（送达链路三件套）：delivered/bounced/complained 为 mail_events 事件计数；
 * 三个比率为事件计数 / 终态事件合计（一位小数 0-100），窗口内无终态事件为 null。
 */
export interface ChannelHealth {
  window_days: number
  total: number
  sent: number
  failed: number
  success_rate: number
  last_success_at?: string | null
  last_failed_at?: string | null
  top_errors: ChannelTopError[]
  delivered?: number
  bounced?: number
  complained?: number
  delivery_rate?: number | null
  bounce_rate?: number | null
  complaint_rate?: number | null
}

/** POST /api/channels/:id/webhook-token 响应：token 与相对路径 webhook 地址 */
export interface WebhookTokenResponse {
  webhook_token: string
  webhook_url: string
}

/**
 * POST /api/channels/:id/webhook-token：补生成（存量渠道 token 为空）或轮换
 * （rotate=true）入站送达事件 URL token；轮换后旧 URL 立即失效。
 */
export function rotateChannelWebhookToken(id: number, rotate = false): Promise<WebhookTokenResponse> {
  return api<WebhookTokenResponse>(`/api/channels/${id}/webhook-token`, {
    method: 'POST',
    body: rotate ? { rotate: true } : undefined,
  })
}

/**
 * POST /api/channels/:id/test 响应（SPEC §5）。
 * 测试发送成功会扣减渠道日/月额度（成功体 {ok:true,latency_ms}）；平台失败仍为
 * HTTP 200 + {ok:false,latency_ms,error:"平台错误文案"}（管理端直发诊断接口）；
 * 额度用尽时后端返回 HTTP 429 统一错误体 {error:{type:"quota_exhausted",message:…}}
 * （api 层归一为 ApiError.message 走 onError）。
 */
export interface TestSendResponse {
  ok: boolean
  latency_ms: number
  /** 仅失败（ok=false）时携带平台错误文案 */
  error?: string
  /** 模板渲染缺失变量清单（仅测试发送携带 template_id 且有缺失时返回） */
  missing_variables?: string[]
}

/** POST /api/channels/:id/status 请求体 */
export type ChannelStatusAction = 'enable' | 'disable'

/**
 * POST /api/channels/:id/duplicate：复制渠道（E6）。
 * 服务端复制全字段（含 secrets），副本名 = 原名-副本（重名追加 -2/-3），
 * 副本默认停用（status=1 manual_disabled），用量/错误等运行态归零。
 * 返回新渠道 DTO（secrets 仍为掩码回显）。
 */
export function duplicateChannel(id: number): Promise<Channel> {
  return api<Channel>(`/api/channels/${id}/duplicate`, { method: 'POST' })
}

/* ---------------------------------- 平台常量（SPEC §3） ---------------------------------- */

export const PROVIDER_OPTIONS = [
  'resend',
  'brevo',
  'mailjet',
  'smtp2go',
  'mailgun',
  'smtp',
  'mock',
] as const

export type ProviderOption = (typeof PROVIDER_OPTIONS)[number]

/** 平台显示名的 i18n key（品牌/协议名各语言一致；ChannelsPage 消费处 t(key) 渲染） */
export const PROVIDER_LABELS: Record<ProviderOption, `channels:platform.${ProviderOption}`> = {
  resend: 'channels:platform.resend',
  brevo: 'channels:platform.brevo',
  mailjet: 'channels:platform.mailjet',
  smtp2go: 'channels:platform.smtp2go',
  mailgun: 'channels:platform.mailgun',
  smtp: 'channels:platform.smtp',
  mock: 'channels:platform.mock',
}

/** 各平台默认额度（defaults.go：0=不限），用于新建表单 override 的占位提示 */
export const PROVIDER_DEFAULTS: Record<ProviderOption, { daily: string; monthly: string }> = {
  resend: { daily: '100', monthly: '3000' },
  brevo: { daily: '300', monthly: '∞' },
  mailjet: { daily: '200', monthly: '6000' },
  smtp2go: { daily: '200', monthly: '1000' },
  mailgun: { daily: '100', monthly: '∞' },
  smtp: { daily: '∞', monthly: '∞' },
  mock: { daily: '∞', monthly: '∞' },
}

/** 这些平台需要 API Key（smtp 用五件套、mock 无需凭据） */
const PROVIDERS_NEEDING_KEY: ReadonlySet<string> = new Set([
  'resend',
  'brevo',
  'mailjet',
  'smtp2go',
  'mailgun',
])

export function providerNeedsApiKey(provider: string): boolean {
  return PROVIDERS_NEEDING_KEY.has(provider)
}

/* ---------------------------------- 数据层 ---------------------------------- */

export const CHANNELS_QUERY_KEY = ['channels', 'list'] as const

/** GET /api/channels */
export function useChannels() {
  return useQuery({
    queryKey: CHANNELS_QUERY_KEY,
    queryFn: () => api<ChannelListResponse>('/api/channels'),
  })
}

/** 渠道写操作成功后失效相关缓存（渠道列表 + 仪表盘统计） */
export function invalidateChannelData(queryClient: QueryClient): void {
  void queryClient.invalidateQueries({ queryKey: ['channels'] })
  void queryClient.invalidateQueries({ queryKey: ['stats'] })
}
