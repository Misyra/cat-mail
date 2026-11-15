/**
 * 数据备份数据层（A2，snake_case 原样字段名）：
 * 整库备份的创建/列表/下载/删除与发送日志手动清理（POST /api/logs/purge）。
 * 下载走 <a href> 直接触发浏览器下载（Cookie 会话鉴权，后端 Content-Disposition 带原名）。
 */
import { api } from '@/lib/api'
import type { SystemSettingsWithNotify } from '@/lib/notifications'
import type { SystemSettings } from '@/lib/settings'

/** 备份文件条目（GET /api/backups 列表项，按创建时间倒序） */
export interface BackupItem {
  /** 文件名（后端严格白名单 catmail-backup-YYYYMMDD-HHMMSS.db） */
  name: string
  /** 字节数 */
  size: number
  /** ISO 时间（文件创建时间） */
  created_at: string
}

/** GET /api/backups 响应体 */
export interface BackupListResponse {
  items: BackupItem[]
}

/** POST /api/backups 响应体：新建备份的文件名与字节数 */
export interface BackupCreated {
  name: string
  size: number
}

/** 备份列表 react-query key（与后端 GET /api/backups 对应） */
export const BACKUPS_QUERY_KEY = ['backups'] as const

/** GET /api/backups */
export function listBackups(): Promise<BackupListResponse> {
  return api<BackupListResponse>('/api/backups')
}

/** POST /api/backups：立即备份（服务端同时保留最近 10 份）。
 *  VACUUM INTO 是整库快照，大库耗时远超普通接口，单独放宽到 5 分钟，避免被默认 30s 超时误杀 */
export function createBackup(): Promise<BackupCreated> {
  return api<BackupCreated>('/api/backups', { method: 'POST', timeoutMs: 5 * 60_000 })
}

/** DELETE /api/backups/:name */
export function deleteBackup(name: string): Promise<void> {
  return api<void>(`/api/backups/${encodeURIComponent(name)}`, { method: 'DELETE' })
}

/** 备份下载地址（后端 Content-Disposition 附件直出，<a> 点击即下载） */
export function backupDownloadURL(name: string): string {
  return `/api/backups/${encodeURIComponent(name)}/download`
}

/* ------------------------------- 日志手动清理 ------------------------------- */

/** 日志清理模式：before_days=按时间 channel=按渠道 all=清空（保留表结构） */
export type LogPurgeMode = 'before_days' | 'channel' | 'all'

/** POST /api/logs/purge 请求体 */
export interface LogPurgeRequest {
  mode: LogPurgeMode
  /** before_days 模式：删除多少天之前的日志（1-3650） */
  days?: number
  /** channel 模式：目标渠道 ID */
  channel_id?: number
}

/** POST /api/logs/purge：返回实际删除条数。
 *  单次最多清 10 万行且后端分批删除，耗时可能超默认 30s，单独放宽到 5 分钟 */
export function purgeLogs(req: LogPurgeRequest): Promise<{ deleted: number }> {
  return api<{ deleted: number }>('/api/logs/purge', { method: 'POST', body: req, timeoutMs: 5 * 60_000 })
}

/* --------------------------- 设置类型扩展与格式化 --------------------------- */

/** GET/PUT /api/settings 的每日自动备份字段（A2，与既有设置字段同一体） */
export interface BackupSettingsFields {
  backup_daily_enabled: boolean
}

export type SystemSettingsWithBackup = SystemSettingsWithNotify & BackupSettingsFields

/** 每日自动备份开关收窄（后端未返回该字段时回落默认 false = 不自动备份） */
export function normalizeBackupDailyEnabled(s: SystemSettings): boolean {
  const withBackup = s as Partial<SystemSettingsWithBackup>
  return withBackup.backup_daily_enabled ?? false
}

/** 字节数 → 可读大小（B/KB/MB/GB，1024 进制；异常输入显示 —） */
export function formatBytes(n: number): string {
  if (!Number.isFinite(n) || n < 0) return '—'
  if (n < 1024) return `${n} B`
  const units = ['KB', 'MB', 'GB', 'TB']
  let v = n
  let i = -1
  do {
    v /= 1024
    i++
  } while (v >= 1024 && i < units.length - 1)
  return `${v.toFixed(v >= 100 ? 0 : 1)} ${units[i]}`
}
