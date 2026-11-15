/**
 * C1 E2E 共享辅助：常量、命名、API 造数（mock 渠道零外网）、控制台错误收集。
 *
 * 选择器约定：本项目的 Field 组件把 hint/error 也包进 <label> 内，
 * 控件的可访问名是「label 文字 + hint 文字」，因此统一用 /^中文锚点/ 正则
 * 做 getByLabel/getByRole 定位（避开「按平台筛选」等 contains 误匹配，
 * 也避开 E3/E6 正在加的重发/复制等新元素——全部用稳定文本/role）。
 */
import type { APIRequestContext, Page } from '@playwright/test'

/** E2E 专用端口：避开 smoke(18080)/走查(18081)，与 scripts/e2e-server.sh、playwright.config.ts 保持一致 */
export const PORT = 18100
export const BASE_URL = `http://127.0.0.1:${PORT}`

/** 首启管理员（后端 config.go 默认值；与 scripts/e2e-server.sh 启动参数一致） */
export const ADMIN_USER = 'admin'
export const ADMIN_PASS = 'admin123'

let nameSeq = 0

/** 测试间数据不假设唯一：时间戳 + 递增序号后缀（重跑/重试不撞名） */
export function uniqueName(prefix: string): string {
  nameSeq += 1
  return `${prefix}-${Date.now().toString(36)}${nameSeq.toString(36)}`
}

/* ---------------------------------- API 造数（arrange 用，页面只做 act/assert） ---------------------------------- */

async function assertOk(res: import('@playwright/test').APIResponse, what: string): Promise<unknown> {
  if (!res.ok()) {
    throw new Error(`${what} 失败：HTTP ${res.status()} ${await res.text()}`)
  }
  return res.json()
}

export interface MockChannel {
  id: number
  name: string
}

/** 新建 mock 渠道（不填写任何真实平台凭据，发送走内存 mock，零外网） */
export async function apiCreateChannel(
  request: APIRequestContext,
  name: string,
): Promise<MockChannel> {
  const res = await request.post('/api/channels', {
    data: {
      name,
      provider: 'mock',
      from_email: 'e2e@example.com',
      from_name: 'CatMail E2E',
    },
  })
  const body = (await assertOk(res, `新建渠道 ${name}`)) as MockChannel
  return body
}

export interface MockKey {
  id: number
  key: string
}

/** 新建 ApiKey（明文 key 仅创建响应返回一次，调用方自行持有） */
export async function apiCreateKey(request: APIRequestContext, name: string): Promise<MockKey> {
  const res = await request.post('/api/keys', { data: { name } })
  return (await assertOk(res, `新建 Key ${name}`)) as MockKey
}

export interface SendResult {
  id: string
  accepted: boolean
}

/** 走 /v1/send 用 mock 渠道发一封（仅入本地库，不出网） */
export async function apiSend(
  request: APIRequestContext,
  apiKey: string,
  channelName: string,
  subject: string,
): Promise<SendResult> {
  const res = await request.post('/v1/send', {
    headers: { Authorization: `Bearer ${apiKey}` },
    data: {
      from: 'e2e@example.com',
      to: ['e2e-rcpt@example.com'],
      subject,
      html: '<p>CatMail E2E</p>',
      channel: channelName,
    },
  })
  return (await assertOk(res, `发送 ${subject}`)) as SendResult
}

/* ---------------------------------- 控制台错误收集 ---------------------------------- */

/**
 * 收集 console error 与未捕获 pageerror。
 * 仪表盘「无控制台错误」断言用；favicon 是内联 data URL，无资源 404 噪音。
 */
export function collectConsoleErrors(page: Page): string[] {
  const errors: string[] = []
  page.on('console', (msg) => {
    if (msg.type() === 'error') errors.push(msg.text())
  })
  page.on('pageerror', (err) => errors.push(`pageerror: ${err.message}`))
  return errors
}
