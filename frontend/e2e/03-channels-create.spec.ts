/**
 * 场景 3 渠道：新建 mock 渠道（填写默认发件人）→ 列表出现。
 * 选择器全部用稳定文本/role，避开渠道行操作区（E3/E6 正在加重发/复制等元素）。
 */
import { expect, test } from '@playwright/test'

import { uniqueName } from './helpers'

test('新建 mock 渠道并出现在列表', async ({ page }) => {
  const name = uniqueName('e2e-ch')

  await page.goto('/channels')
  await expect(page.getByRole('heading', { name: '渠道管理' })).toBeVisible()

  await page.getByRole('button', { name: '新建渠道' }).click()
  const modal = page.getByRole('dialog', { name: '新建渠道' })
  await expect(modal).toBeVisible()

  // 基础信息（exact 锚点：避免命中筛选工具栏的「按平台筛选」与 webhook 的「平台签名密钥」）
  await modal.getByLabel(/^渠道名称/).fill(name)
  await modal.getByLabel('平台', { exact: true }).selectOption({ label: 'Mock' })
  // 默认发件人
  await modal.getByLabel(/^默认发件邮箱/).fill('e2e@example.com')
  await modal.getByLabel(/^发件人名称/).fill('CatMail E2E')

  await modal.getByRole('button', { name: '创建渠道' }).click()

  // toast 反馈 + 列表出现新渠道
  await expect(page.getByText('渠道已创建')).toBeVisible()
  await expect(modal).toHaveCount(0)
  const row = page.getByRole('row', { name: new RegExp(name) })
  await expect(row).toBeVisible()
  // 平台列显示 Mock 徽章
  await expect(row.getByText('Mock', { exact: true })).toBeVisible()
})
