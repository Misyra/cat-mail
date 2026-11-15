/**
 * 场景 9 主题：顶栏循环切换（浅色 → 玻璃 → 深色 → 跟随系统）驱动 html.dark / html.glass
 * class 变化。Playwright 默认 colorScheme=light（config 显式固定），system 态解析为浅色；
 * 玻璃档的明暗基色冻结在切换时刻（此处从浅色切入 → 浅玻璃，不出现 dark）。
 */
import { expect, test } from '@playwright/test'

test('主题循环切换改变 html class', async ({ page }) => {
  await page.goto('/')
  await expect(page.getByRole('heading', { name: '仪表盘' })).toBeVisible()

  const html = page.locator('html')
  const toggle = page.getByRole('button', { name: /界面主题/ })

  // 初始：跟随系统（storageState 新上下文无 localStorage 记录）→ 浅色，无 dark/glass
  await expect(html).not.toHaveClass(/dark/)
  await expect(html).not.toHaveClass(/glass/)

  // system → 浅色：仍无 dark/glass
  await toggle.click()
  await expect(html).not.toHaveClass(/dark/)
  await expect(html).not.toHaveClass(/glass/)

  // 浅色 → 玻璃：glass class 出现，基色冻结为当前浅色（仍无 dark）
  await toggle.click()
  await expect(html).toHaveClass(/glass/)
  await expect(html).not.toHaveClass(/dark/)

  // 玻璃 → 深色：dark 出现，glass 移除
  await toggle.click()
  await expect(html).toHaveClass(/dark/)
  await expect(html).not.toHaveClass(/glass/)

  // 深色 → 跟随系统：回到浅色，dark/glass 均移除
  await toggle.click()
  await expect(html).not.toHaveClass(/dark/)
  await expect(html).not.toHaveClass(/glass/)
})
