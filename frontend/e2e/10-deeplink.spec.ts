/**
 * 场景 10 深链：直接打开 /logs /settings（及其余后台路由）不 404、不被踢回登录页，
 * 对应页面标题正常渲染（SPA history 路由 + go:embed 静态回退）。
 */
import { expect, test } from '@playwright/test'

const ROUTES: Array<[string, string]> = [
  ['/logs', '发送日志'],
  ['/settings', '系统设置'],
  ['/channels', '渠道管理'],
  ['/keys', 'API Keys'],
  ['/users', '账号管理'],
]

for (const [path, heading] of ROUTES) {
  test(`深链直开 ${path} 不 404`, async ({ page }) => {
    const resp = await page.goto(path)
    // 静态资源回退正常（非 404），且未被重定向到登录页
    expect(resp?.status(), `HTTP 状态码`).toBe(200)
    await expect(page).not.toHaveURL(/\/login$/)
    await expect(page.getByRole('heading', { name: heading })).toBeVisible()
  })
}
