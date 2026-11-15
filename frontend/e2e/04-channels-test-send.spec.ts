/**
 * 场景 4 渠道测试发送：测试发送成功 toast → 列表「今日 / 本月」用量 0 → 1。
 * 独立新建渠道（时间戳命名），断言不受其他用例造数影响。
 */
import { expect, test } from '@playwright/test'

import { apiCreateChannel, uniqueName } from './helpers'

test('渠道测试发送成功，列表用量变化', async ({ page, request }) => {
  const name = uniqueName('e2e-testsend')
  await apiCreateChannel(request, name)

  await page.goto('/channels')
  const row = page.getByRole('row', { name: new RegExp(name) })
  await expect(row).toBeVisible()

  // 前置：零用量（每日/每月额度均未设覆盖 → ∞）
  await expect(row.getByText('0/∞ · 0/∞')).toBeVisible()

  // 打开测试发送弹窗并发送
  await row.getByRole('button', { name: '测试' }).click()
  const modal = page.getByRole('dialog', { name: `测试发送 · ${name}` })
  await expect(modal).toBeVisible()
  await modal.getByLabel(/^收件邮箱/).fill('e2e-rcpt@example.com')
  await modal.getByRole('button', { name: '发送测试' }).click()

  // 成功 toast（文案含耗时 ms）+ 弹窗关闭
  await expect(page.getByText(new RegExp(`渠道「${name}」测试发送成功，耗时 \\d+ ms`))).toBeVisible()
  await expect(modal).toHaveCount(0)

  // 列表用量变化：日/月额度各 +1
  await expect(row.getByText('1/∞ · 1/∞')).toBeVisible()
})
