/**
 * 场景 6 Keys：创建 → 明文 Key 仅一次展示（关闭需二次确认）→ 列表只显示掩码。
 * 明文格式：cm- + 32 位 hex（后端 handler_keys.go）；列表掩码：前 7 位 + … + 后 4 位。
 */
import { expect, test } from '@playwright/test'

import { uniqueName } from './helpers'

test('Key 创建、明文一次性展示、列表掩码', async ({ page }) => {
  const name = uniqueName('e2e-key')

  await page.goto('/keys')
  await expect(page.getByRole('heading', { name: 'API Keys' })).toBeVisible()

  // 新建
  await page.getByRole('button', { name: '新建 Key' }).click()
  const createModal = page.getByRole('dialog', { name: '新建 API Key' })
  await expect(createModal).toBeVisible()
  await createModal.getByLabel(/^名称/).fill(name)
  await createModal.getByRole('button', { name: '创建 Key' }).click()

  // 明文一次性展示弹窗
  const reveal = page.getByRole('dialog', { name: 'Key 创建成功' })
  await expect(reveal).toBeVisible()
  const plaintext = (await reveal.locator('code').innerText()).trim()
  expect(plaintext).toMatch(/^cm-[0-9a-f]{32}$/)

  // 关闭前二次确认：先点「我已保存，关闭」→ 出现确认按钮
  await reveal.getByRole('button', { name: '我已保存，关闭' }).click()
  await reveal.getByRole('button', { name: '确认关闭，不再显示' }).click()
  await expect(reveal).toHaveCount(0)

  // 明文不再出现在页面上
  await expect(page.getByText(plaintext)).toHaveCount(0)

  // 列表行：掩码展示（含省略号），非明文
  const row = page.getByRole('row', { name: new RegExp(name) })
  await expect(row).toBeVisible()
  const masked = (await row.locator('code').innerText()).trim()
  expect(masked).toContain('…')
  expect(masked).not.toBe(plaintext)
  expect(plaintext.startsWith(masked.split('…')[0])).toBe(true)
})
