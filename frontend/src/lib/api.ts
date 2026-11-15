/**
 * 统一 fetch 封装（SPEC §6 数据层）：
 * - JSON 请求/响应；same-origin cookie 默认携带（管理 API 会话）
 * - 错误体统一 { error: { type, message } }（另兼容 { ok:false, error:"文案" } 扁平形态），
 *   提取 message 抛 ApiError
 * - 默认 30s 超时（timeoutMs 放宽/关闭），超时抛 ApiError(0)；调用方 signal 取消原样透传
 * - 401 → 通知清状态跳 /login（登录/me 接口除外，由页面自行处理）
 */

import i18next from 'i18next'

const AUTH_WHITELIST = ['/api/auth/login', '/api/auth/me']

/** 请求超时默认值（毫秒）：覆盖后端假死 / 网络半开导致请求永久 pending 的场景 */
const DEFAULT_TIMEOUT_MS = 30_000

export class ApiError extends Error {
  /** HTTP 状态码；网络错误为 0 */
  readonly status: number
  /** 后端错误类型：invalid_request | unauthorized | quota_exhausted | provider_error | server_error */
  readonly type?: string

  constructor(status: number, message: string, type?: string) {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.type = type
  }
}

type UnauthorizedHandler = () => void

let unauthorizedHandler: UnauthorizedHandler | null = null

/** 由应用在挂载时注册（清 query 缓存 + 跳转登录页） */
export function setUnauthorizedHandler(handler: UnauthorizedHandler | null): void {
  unauthorizedHandler = handler
}

export interface ApiRequestOptions {
  method?: 'GET' | 'POST' | 'PUT' | 'DELETE' | 'PATCH'
  /** 非 undefined 时序列化为 JSON 请求体 */
  body?: unknown
  signal?: AbortSignal
  /** 请求超时（毫秒），默认 30_000；传 0 关闭超时（长耗时下载/清理类接口单独放宽） */
  timeoutMs?: number
}

export async function api<T>(path: string, options: ApiRequestOptions = {}): Promise<T> {
  const { method = 'GET', body, signal, timeoutMs = DEFAULT_TIMEOUT_MS } = options

  // 超时合并：内部 controller 定时 abort；调用方 signal 取消时同步 abort 内部 controller。
  // 用 timedOut 区分超时（统一抛「请求超时」ApiError）与调用方主动取消（原样透传 AbortError）
  const controller = new AbortController()
  let timedOut = false
  const timer =
    timeoutMs > 0
      ? window.setTimeout(() => {
          timedOut = true
          controller.abort()
        }, timeoutMs)
      : null
  const abortFromOuter = () => controller.abort()
  if (signal) {
    if (signal.aborted) abortFromOuter()
    else signal.addEventListener('abort', abortFromOuter)
  }

  try {
    let res: Response
    try {
      res = await fetch(path, {
        method,
        headers: body !== undefined ? { 'Content-Type': 'application/json' } : undefined,
        body: body !== undefined ? JSON.stringify(body) : undefined,
        credentials: 'same-origin',
        signal: controller.signal,
      })
    } catch (err) {
      if (err instanceof DOMException && err.name === 'AbortError') {
        // 前端兜底错误就地翻译（构造时求值，先例同 lib/audit.ts）；语言切换后已展示的历史错误不重译
        if (timedOut) throw new ApiError(0, i18next.t('ui:errors.timeout'))
        throw err
      }
      throw new ApiError(0, i18next.t('ui:errors.network'))
    }

    if (!res.ok) {
      const { message, type } = await parseErrorBody(res)
      // 401 → 清状态跳登录（登录/查询自身接口除外，避免与页面逻辑冲突）
      if (res.status === 401 && !AUTH_WHITELIST.includes(path)) {
        unauthorizedHandler?.()
      }
      throw new ApiError(res.status, message, type)
    }

    if (res.status === 204) return undefined as T
    let text: string
    try {
      text = await res.text()
    } catch (err) {
      // 响应体读取阶段被超时 abort：同样归一为超时错误（调用方取消仍是原始 AbortError）
      if (timedOut) throw new ApiError(0, i18next.t('ui:errors.timeout'))
      throw err
    }
    if (!text) return undefined as T
    try {
      return JSON.parse(text) as T
    } catch {
      throw new ApiError(res.status, i18next.t('ui:errors.invalidJson'))
    }
  } finally {
    if (timer !== null) window.clearTimeout(timer)
    if (signal) signal.removeEventListener('abort', abortFromOuter)
  }
}

/**
 * 解析统一错误体（SPEC §2）：{ error: { type, message } }。
 * 保留 { ok: false, error: "文案" } 扁平形态的兼容分支，命中时取 error 字符串为文案。
 */
async function parseErrorBody(res: Response): Promise<{ message: string; type?: string }> {
  try {
    const data = (await res.json()) as { error?: unknown }
    if (typeof data?.error === 'string' && data.error) {
      return { message: data.error }
    }
    if (typeof data?.error === 'object' && data.error !== null) {
      const { message, type } = data.error as { message?: unknown; type?: unknown }
      return {
        message:
          typeof message === 'string' && message
            ? message
            : i18next.t('ui:errors.httpStatus', { status: res.status }),
        type: typeof type === 'string' ? type : undefined,
      }
    }
    return { message: i18next.t('ui:errors.httpStatus', { status: res.status }) }
  } catch {
    return { message: i18next.t('ui:errors.httpStatus', { status: res.status }) }
  }
}
