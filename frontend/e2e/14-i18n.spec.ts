/**
 * I1 i18n E2E：界面语言切换（设置页下拉）、localStorage 持久化、浏览器语言探测。
 * 运行前提：playwright.config 全局 use.locale=zh-CN，因此「未手动切换」时界面恒为中文；
 * 英文界面只能经下拉切换或预置 localStorage（两种路径都覆盖）。
 */
import { expect, test } from '@playwright/test'

import { BASE_URL } from './helpers'

/** 设置页「界面语言」下拉的锚点（Field 把 hint 包进 label，用前缀正则定位） */
const LANG_LABEL_ZH = /^界面语言/
const LANG_LABEL_EN = /^Interface language/

test.describe('i18n 语言切换', () => {
  test('下拉切换到 English：即时生效 + localStorage 持久化 + 刷新保持', async ({ page }) => {
    await page.goto('/settings')
    await expect(page).toHaveURL(`${BASE_URL}/settings`)

    // 初始：全局 locale=zh-CN → 浏览器语言探测 → 中文界面
    await expect(page.locator('html')).toHaveAttribute('lang', 'zh-CN')
    const langSelect = page.getByLabel(LANG_LABEL_ZH)
    await expect(langSelect).toHaveValue('zh-CN')

    // 切换到 English：无需 reload，全树即时重渲染（语言卡自身 label 也变英文）
    await langSelect.selectOption('en-US')
    await expect(page.locator('html')).toHaveAttribute('lang', 'en-US')
    await expect(page.getByLabel(LANG_LABEL_EN)).toHaveValue('en-US')
    // 已迁移页面的固定词汇即时变英文（设置页多个卡片各有「保存设置」按钮，取其一断言）
    await expect(page.getByRole('button', { name: 'Save Settings' }).first()).toBeVisible()

    // localStorage 持久化 + reload 后保持英文
    await expect(async () => {
      const stored = await page.evaluate(() => localStorage.getItem('catmail-lang'))
      expect(stored).toBe('en-US')
    }).toPass()
    await page.reload()
    await expect(page.locator('html')).toHaveAttribute('lang', 'en-US')
    await expect(page.getByLabel(LANG_LABEL_EN)).toHaveValue('en-US')

    // 切回中文：恢复初始界面
    await page.getByLabel(LANG_LABEL_EN).selectOption('zh-CN')
    await expect(page.locator('html')).toHaveAttribute('lang', 'zh-CN')
    await expect(page.getByLabel(LANG_LABEL_ZH)).toHaveValue('zh-CN')
  })

  test('预置 localStorage en-US：首次进入直接英文渲染', async ({ page }) => {
    // 与 storageState 同源预置语言（先访问触发同源，再落 localStorage 再进目标页）
    await page.goto('/login')
    await page.evaluate(() => localStorage.setItem('catmail-lang', 'en-US'))
    await page.goto('/settings')
    await expect(page).toHaveURL(`${BASE_URL}/settings`)
    await expect(page.locator('html')).toHaveAttribute('lang', 'en-US')
    await expect(page.getByLabel(LANG_LABEL_EN)).toHaveValue('en-US')
  })
})
