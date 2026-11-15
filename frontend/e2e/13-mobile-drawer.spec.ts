/**
 * 场景 13 移动端抽屉（O32 / W4）：<1024px 侧栏收纳为左侧抽屉（原生 <dialog>），
 * 顶栏「打开导航菜单」按钮开关；点击导航路由切换后抽屉自动关闭；Esc 关闭；
 * ≥1024px 桌面侧栏常驻，无菜单按钮。
 */
import { expect, test } from '@playwright/test'

// 移动视口（O32 验收口径：390×844）
test.use({ viewport: { width: 390, height: 844 } })

test('抽屉导航：打开 → 点「渠道管理」→ 路由切换且抽屉自动关闭', async ({ page }) => {
  await page.goto('/')
  await expect(page.getByRole('heading', { name: '仪表盘' })).toBeVisible()

  const menu = page.getByRole('button', { name: '打开导航菜单' })
  const drawer = page.locator('#app-drawer')

  await menu.click()
  await expect(drawer).toBeVisible()
  await expect(drawer.getByRole('link', { name: '渠道管理' })).toBeVisible()

  // 抽屉内点导航 → 路由切换，抽屉随 pathname 变化自动关闭
  await drawer.getByRole('link', { name: '渠道管理' }).click()
  await expect(page).toHaveURL(/\/channels$/)
  await expect(drawer).not.toBeVisible()
})

test('Escape 关闭抽屉', async ({ page }) => {
  await page.goto('/')
  await expect(page.getByRole('heading', { name: '仪表盘' })).toBeVisible()

  await page.getByRole('button', { name: '打开导航菜单' }).click()
  const drawer = page.locator('#app-drawer')
  await expect(drawer).toBeVisible()

  // 原生 dialog 的 cancel 事件汇回 onClose，受控关闭
  await page.keyboard.press('Escape')
  await expect(drawer).not.toBeVisible()
})

test.describe('桌面视口（≥lg 侧栏常驻）', () => {
  test.use({ viewport: { width: 1280, height: 800 } })

  test('无「打开导航菜单」按钮', async ({ page }) => {
    await page.goto('/')
    await expect(page.getByRole('heading', { name: '仪表盘' })).toBeVisible()
    // lg:hidden 仅移除显示：role 查询默认排除不可见元素，命 0 即「不存在」
    await expect(page.getByRole('button', { name: '打开导航菜单' })).toHaveCount(0)
  })
})
