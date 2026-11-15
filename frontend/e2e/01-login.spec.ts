/**
 * 场景 1 登录：错口令显示错误提示；正确口令进入仪表盘。
 * 本文件是登录功能本身，使用空白 storageState（不走 setup 项目的登录态）。
 */
import { expect, test } from '@playwright/test'

import { ADMIN_PASS, ADMIN_USER } from './helpers'

test.describe('登录', () => {
  // 空会话：强制走完整登录流程
  test.use({ storageState: { cookies: [], origins: [] } })

  test('错误口令提示错误，正确口令进入仪表盘', async ({ page }) => {
    // 未登录访问任意页 → 重定向登录页
    await page.goto('/')
    await expect(page).toHaveURL(/\/login$/)
    await expect(page.getByRole('heading', { name: '猫邮管理后台' })).toBeVisible()

    // 错口令：服务端统一文案「用户名或密码错误」（内联错误 + toast 各一处）
    await page.getByLabel(/^用户名/).fill(ADMIN_USER)
    await page.getByLabel(/^密码/).fill('definitely-wrong-pass')
    await page.getByRole('button', { name: '登 录' }).click()
    // 内联错误在登录表单内（toast 在表单外的通知区域）
    await expect(page.locator('form').getByText('用户名或密码错误')).toBeVisible()
    // 仍停留在登录页
    await expect(page).toHaveURL(/\/login$/)

    // 正确口令：进入仪表盘（URL / + 页面标题）
    await page.getByLabel(/^密码/).fill(ADMIN_PASS)
    await page.getByRole('button', { name: '登 录' }).click()
    await expect(page).toHaveURL(/\/$/)
    await expect(page.getByRole('heading', { name: '仪表盘' })).toBeVisible()
  })
})
