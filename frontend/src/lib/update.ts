import { api } from '@/lib/api'

/**
 * 系统更新数据层（E8 自动更新，后端契约 snake_case 原样字段名）：
 * - GET /api/update/check 检查更新（后端结果进程内缓存 1h；GitHub 失败时
 *   HTTP 200 且 error 字段非空——错误如实返回、不缓存）
 * - POST /api/update/apply 一键更新（下载 + SHA-256 校验 + 原子替换当前二进制，
 *   重启后生效；容器部署恒 400）
 * 两个端点均在 /api 会话组内（登录 + 管理白名单）。
 */

/** GET /api/update/check 响应体 */
export interface UpdateCheckResult {
  /** 当前程序版本（构建注入，dev 构建为 "dev"） */
  current: string
  /** 最新 release tag（GitHub 失败时缺省） */
  latest?: string
  /** 受限 semver 比较：current < latest 才为 true（解析失败如实 false） */
  update_available: boolean
  /** release 页面链接 */
  release_url?: string
  /** release notes 原文 */
  notes?: string
  /** release 发布时间 */
  published_at?: string
  /** 本次检查时间（缓存命中时为缓存时点） */
  checked_at?: string
  /** 容器部署标记：true 时一键更新不可用（应拉取新镜像升级） */
  in_docker: boolean
  /** GitHub 网络/限流等错误文案；非空时 latest 等字段不可信 */
  error?: string
}

/** POST /api/update/apply 成功响应体 */
export interface UpdateApplyResult {
  ok: boolean
  /** 已安装的版本号（重启后生效） */
  installed_version: string
  /** 固定提示：重启 catmail 生效，可用 catmail doctor 验证 */
  hint: string
}

export const UPDATE_CHECK_QUERY_KEY = ['update', 'check'] as const

/** GET /api/update/check */
export function checkUpdate(): Promise<UpdateCheckResult> {
  return api<UpdateCheckResult>('/api/update/check')
}

/** POST /api/update/apply：无请求体；业务性拒绝（容器/校验失败等）经统一错误体抛 ApiError。
 *  下载新版二进制耗时远超普通接口，单独放宽到 10 分钟，避免被默认 30s 超时误杀 */
export function applyUpdate(): Promise<UpdateApplyResult> {
  return api<UpdateApplyResult>('/api/update/apply', { method: 'POST', timeoutMs: 10 * 60_000 })
}
