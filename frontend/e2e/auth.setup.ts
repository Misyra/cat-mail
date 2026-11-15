/**
 * setup 项目：UI 登录一次并把会话写入 storageState（e2e/.auth/user.json），
 * 供 chromium 项目所有用例复用登录态（登录场景本身另起空白 storageState）。
 */
import { mkdirSync } from 'node:fs'
import { dirname } from 'node:path'
import { fileURLToPath } from 'node:url'
import { expect, test as setup } from '@playwright/test'

import { ADMIN_PASS, ADMIN_USER, BASE_URL } from './helpers'

const stateFile = fileURLToPath(new URL('./.auth/user.json', import.meta.url))

setup('登录一次并保存会话', async ({ page }) => {
  await page.goto('/login')
  await page.getByLabel(/^用户名/).fill(ADMIN_USER)
  await page.getByLabel(/^密码/).fill(ADMIN_PASS)
  await page.getByRole('button', { name: '登 录' }).click()

  // 进入仪表盘即登录成功（RequireAuth 放行）
  await expect(page).toHaveURL(`${BASE_URL}/`)
  await expect(page.getByRole('heading', { name: '仪表盘' })).toBeVisible()

  mkdirSync(dirname(stateFile), { recursive: true })
  await page.context().storageState({ path: stateFile })
})
