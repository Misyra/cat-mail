/**
 * 场景 8 设置：改「日志保留天数」→ 保存 → 刷新回读一致。
 * 轮换卡片与通知卡片都有「保存设置」按钮 → 用含「轮换策略」的 form 收窄作用域。
 */
import { expect, test } from '@playwright/test'

test('修改日志保留天数，保存后刷新回读一致', async ({ page }) => {
  await page.goto('/settings')
  await expect(page.getByRole('heading', { name: '系统设置' })).toBeVisible()

  const retention = page.getByLabel(/^日志保留天数/)
  const current = await retention.inputValue()
  // 与现值不同的目标值（0-365 合法区间内取不常用值，重跑也成立）
  const next = current === '77' ? '78' : '77'

  await retention.fill(next)
  const rotationForm = page.locator('form', { hasText: '轮换策略' })
  await rotationForm.getByRole('button', { name: '保存设置' }).click()

  // 保存成功反馈
  await expect(page.getByText('设置已保存')).toBeVisible()

  // 刷新回读：新值落库并回显
  await page.reload()
  await expect(page.getByLabel(/^日志保留天数/)).toHaveValue(next)
})
