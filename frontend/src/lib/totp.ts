/**
 * 两步验证（TOTP，C4）数据层：全部新请求函数集中在本文件（不改 lib/api.ts）。
 * 复用统一 fetch 封装（same-origin cookie、统一错误体解析）。
 */
import { api } from '@/lib/api'

/** POST /api/auth/totp/setup 响应：secret 手输备用 + 认证器扫码 URL */
export interface TotpSetupResponse {
  secret: string
  otpauth_url: string
}

/** POST /api/auth/totp/enable 响应：恢复码明文，仅本次返回（后端库存 sha256） */
export interface TotpEnableResponse {
  recovery_codes: string[]
}

/** POST /api/auth/login 响应：开启两步验证时返回 totp_required + 短票据（不建会话） */
export interface LoginResponse {
  username?: string
  totp_required?: boolean
  ticket?: string
}

/** POST /api/auth/totp/enable */
export function totpSetup(): Promise<TotpSetupResponse> {
  return api<TotpSetupResponse>('/api/auth/totp/setup', { method: 'POST' })
}

/** POST /api/auth/totp/enable {code}：启用并返回一次性恢复码 */
export function totpEnable(code: string): Promise<TotpEnableResponse> {
  return api<TotpEnableResponse>('/api/auth/totp/enable', { method: 'POST', body: { code } })
}

/** POST /api/auth/totp/disable {password}：凭密码解绑（不用 TOTP 码，手机丢了也能解绑） */
export function totpDisable(password: string): Promise<void> {
  return api<void>('/api/auth/totp/disable', { method: 'POST', body: { password } })
}

/** POST /api/auth/login/totp {ticket, code}：登录第二步（6 位 TOTP 码或恢复码） */
export function loginTotp(ticket: string, code: string): Promise<{ username: string }> {
  return api<{ username: string }>('/api/auth/login/totp', {
    method: 'POST',
    body: { ticket, code },
  })
}

/** 恢复码是否为已知形态（10 字符 A-Z2-7），仅用于界面提示口径 */
export function isRecoveryCode(code: string): boolean {
  return /^[A-Z2-7]{10}$/i.test(code.trim())
}
