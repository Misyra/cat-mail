import { keepPreviousData, useQuery } from '@tanstack/react-query'
import { useEffect, useState } from 'react'

import { api, ApiError } from '@/lib/api'

/**
 * 发送日志数据层（SPEC §4 SendLog / §5 GET /api/logs，snake_case 原样字段名）。
 * 后端 ToAddrs 落库为 JSON 字符串；DTO 形态可能是数组或 JSON 字符串，parseToAddrs 做兼容。
 */

/** SendLog.Status：0=pending 1=sent 2=failed（SPEC §4） */
export type LogStatusCode = 0 | 1 | 2

/** 发送日志实体（GET /api/logs 列表项 / GET /api/logs/:id 完整详情） */
export interface SendLog {
  id: number
  /** 请求级 uuid（= /v1/send 响应的 id），贯穿一条日志 */
  message_id: string
  api_key_id: number
  channel_id: number
  /** 冗余快照：渠道名 */
  channel_name: string
  provider: string
  /** 所用邮件模板 ID（0 = 未使用模板；名称经 /api/templates 解析，已删除的仅显示 ID） */
  template_id: number
  /** 是否为渠道测试直发产生的日志 */
  is_test: boolean
  status: LogStatusCode
  from_addr: string
  /** 落库为 JSON 字符串；兼容 DTO 直接返回数组 */
  to_addrs: string[] | string
  subject: string
  html_body?: string
  text_body?: string
  provider_message_id?: string
  error?: string
  attempts: number
  latency_ms: number
  created_at: string
}

export interface LogListResponse {
  items: SendLog[]
  total: number
}

/** 列表筛选：后端 status 枚举 sent/failed/pending（0=发送中），空串 = 全部 */
export type LogStatusFilter = '' | 'sent' | 'failed' | 'pending'

/** GET /api/logs 的 sort 参数（默认 newest = created_at 倒序） */
export type LogSortOption = 'newest' | 'oldest' | 'latency_desc'

/** 每页条数可选项（后端上限 100） */
export const LOGS_PAGE_SIZES = [20, 50, 100] as const

export const LOGS_PAGE_SIZE = 20

/** 校验 URL 里的 page_size（非法值回落默认 20） */
export function normalizePageSize(raw: string | null): number {
  const n = raw === null ? NaN : Number.parseInt(raw, 10)
  return (LOGS_PAGE_SIZES as readonly number[]).includes(n) ? n : LOGS_PAGE_SIZE
}

/** 校验 URL 里的 sort（非法值回落默认 newest） */
export function normalizeSort(raw: string | null): LogSortOption {
  return raw === 'oldest' || raw === 'latency_desc' ? raw : 'newest'
}

/** 兼容解析 to_addrs：数组直接用；字符串按 JSON 解析，失败再按逗号/空白拆分 */
export function parseToAddrs(value: string[] | string | null | undefined): string[] {
  if (Array.isArray(value)) {
    return value.filter((addr) => typeof addr === 'string' && addr.length > 0)
  }
  if (typeof value === 'string' && value.trim()) {
    try {
      const parsed: unknown = JSON.parse(value)
      if (Array.isArray(parsed)) return parsed.map(String).filter(Boolean)
      if (typeof parsed === 'string' && parsed) return [parsed]
    } catch {
      return value.split(/[,，\s]+/).filter(Boolean)
    }
  }
  return []
}

export interface LogListParams {
  page: number
  /** 默认 20，后端上限 100 */
  pageSize?: number
  status?: LogStatusFilter
  channelId?: number
  /** 主题 / 收件人模糊搜索 */
  q?: string
  /** 排序（默认 newest） */
  sort?: LogSortOption
  /** 实时刷新：每 5s 轮询一次 */
  live?: boolean
}

/** GET /api/logs?page&page_size&status&channel_id&q&sort（默认 newest；翻页保留前页数据避免闪烁） */
export function useLogs(params: LogListParams) {
  const { page, pageSize = LOGS_PAGE_SIZE, status = '', channelId = 0, q = '', sort = 'newest', live = false } = params
  return useQuery({
    queryKey: ['logs', 'list', { page, pageSize, status, channelId, q, sort }],
    queryFn: () => {
      const sp = new URLSearchParams()
      sp.set('page', String(page))
      sp.set('page_size', String(pageSize))
      if (status) sp.set('status', status)
      if (channelId > 0) sp.set('channel_id', String(channelId))
      if (q) sp.set('q', q)
      if (sort !== 'newest') sp.set('sort', sort)
      return api<LogListResponse>(`/api/logs?${sp.toString()}`)
    },
    placeholderData: keepPreviousData,
    refetchInterval: live ? 5000 : undefined,
  })
}

/** GET /api/logs/:id 完整详情（含正文与错误信息） */
export function useLogDetail(id: number | null) {
  return useQuery({
    queryKey: ['logs', 'detail', id],
    queryFn: () => api<SendLog>(`/api/logs/${id}`),
    enabled: id !== null,
  })
}

/* ---------------------------------- 重发（E3） ---------------------------------- */

/**
 * POST /api/logs/:id/resend 响应（复用发送成功体，id 为全新的 message_id）。
 * 重发走正常轮换（不指定原渠道）、is_test=false，并落一条新发送日志。
 */
export interface ResendLogResponse {
  id: string
  channel: string
  provider: string
  provider_message_id?: string
  accepted: boolean
}

/**
 * 按日志快照原样重发（后端会话鉴权）。
 * 防滥用：同一日志 id 30 秒内仅允许一次（重复 → 429，message 由后端统一错误体给出）；
 * 正文未落库（隐私开关历史）→ 400。
 */
export function resendLog(id: number): Promise<ResendLogResponse> {
  return api<ResendLogResponse>(`/api/logs/${id}/resend`, { method: 'POST' })
}

/** 值防抖（LogsPage / ChannelsPage 输入 300ms 后才生效） */
export function useDebouncedValue<T>(value: T, delayMs: number): T {
  const [debounced, setDebounced] = useState(value)
  useEffect(() => {
    const timer = window.setTimeout(() => setDebounced(value), delayMs)
    return () => window.clearTimeout(timer)
  }, [value, delayMs])
  return debounced
}

/* ---------------------------------- 导出 CSV ---------------------------------- */

/** GET /api/logs/export 筛选参数（与列表一致，无分页） */
export interface LogExportParams {
  status?: LogStatusFilter
  channelId?: number
  q?: string
  sort?: LogSortOption
}

/**
 * 导出错误文案的词典 key（'logs:' 前缀标记命名空间）：lib 非组件环境不取译文，
 * downloadLogsCsv 抛出的 ApiError.message 即此 key，由 LogsPage 消费处经 t() 渲染。
 */
export const EXPORT_ERROR_KEYS = {
  timeout: 'logs:export.timeout',
  networkError: 'logs:export.networkError',
  failedHttp: 'logs:export.failedHttp',
} as const

/** 从 Content-Disposition 提取后端建议文件名（RFC 5987 filename* 优先），取不到返回 null */
function parseFilenameFromDisposition(disposition: string | null): string | null {
  if (!disposition) return null
  const utf8Match = /filename\*=(?:UTF-8'')?([^;]+)/i.exec(disposition)
  if (utf8Match?.[1]) {
    try {
      return decodeURIComponent(utf8Match[1].trim().replace(/^"|"$/g, ''))
    } catch {
      return utf8Match[1].trim().replace(/^"|"$/g, '')
    }
  }
  const plainMatch = /filename="?([^";]+)"?/i.exec(disposition)
  return plainMatch?.[1]?.trim() || null
}

/**
 * 按当前筛选导出日志 CSV（后端返回 text/csv 带 BOM 的下载流）。
 * 独立于 api() 封装：这里是二进制流而非 JSON；401 场景由页面 toast 提示（列表页本身受 RequireAuth 保护）。
 * 导出上限 1 万行：独立放宽到 2 分钟超时（api() 默认 30s 不够），超时/下载流中断归一为「导出超时」。
 * 成功时创建 <a download> 触发浏览器下载并释放 ObjectURL。
 */
export async function downloadLogsCsv(params: LogExportParams): Promise<void> {
  const { status = '', channelId = 0, q = '', sort = 'newest' } = params
  const sp = new URLSearchParams()
  if (status) sp.set('status', status)
  if (channelId > 0) sp.set('channel_id', String(channelId))
  if (q) sp.set('q', q)
  if (sort !== 'newest') sp.set('sort', sort)

  // 超时控制（无调用方 signal，仅定时 abort）；timedOut 区分超时与其他失败
  const controller = new AbortController()
  let timedOut = false
  const timer = window.setTimeout(() => {
    timedOut = true
    controller.abort()
  }, 120_000)

  let res: Response
  let blob: Blob
  try {
    try {
      res = await fetch(`/api/logs/export?${sp.toString()}`, {
        credentials: 'same-origin',
        signal: controller.signal,
      })
    } catch (err) {
      if (err instanceof DOMException && err.name === 'AbortError') {
        if (timedOut) throw new ApiError(0, EXPORT_ERROR_KEYS.timeout)
        throw err
      }
      throw new ApiError(0, EXPORT_ERROR_KEYS.networkError)
    }
    if (!res.ok) {
      // message 为词典 key 或后端返回的错误文案；HTTP 状态码经 ApiError.status 携带，渲染时插值
      let message: string = EXPORT_ERROR_KEYS.failedHttp
      try {
        const data = (await res.json()) as { error?: { message?: unknown } }
        if (typeof data?.error?.message === 'string' && data.error.message) message = data.error.message
      } catch {
        // 保留默认错误文案
      }
      throw new ApiError(res.status, message)
    }
    try {
      blob = await res.blob()
    } catch (err) {
      // 下载流读取阶段被超时中断：归一为导出超时错误
      if (timedOut) throw new ApiError(0, EXPORT_ERROR_KEYS.timeout)
      throw err
    }
  } finally {
    window.clearTimeout(timer)
  }

  const fallbackName = `catmail-logs-${new Date().toISOString().slice(0, 19).replace(/[T:]/g, '-')}.csv`
  const filename = parseFilenameFromDisposition(res.headers.get('Content-Disposition')) ?? fallbackName

  const url = URL.createObjectURL(blob)
  try {
    const anchor = document.createElement('a')
    anchor.href = url
    anchor.download = filename
    document.body.appendChild(anchor)
    anchor.click()
    anchor.remove()
  } finally {
    URL.revokeObjectURL(url)
  }
}
