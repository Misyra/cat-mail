/**
 * 场景 12 韧性回归（W1 防护：O35 ⌘K 与弹窗互斥、O36 懒加载 chunk 失败兜底）。
 * - O35：弹窗（原生 dialog）打开时按 Ctrl+K 不得叠加命令面板；Esc 关弹窗后 Ctrl+K 可正常
 *   打开面板、输入框可聚焦可输入；面板自身 Ctrl+K 开→关切换不受互斥防护拦截。
 * - O36：阻断 UsersPage 懒加载 chunk（route.abort，glob 匹配构建产物哈希文件名），
 *   vite:preloadError 触发一次自动整页 reload 后仍失败，ErrorBoundary 显示可恢复的错误卡。
 *
 * 弹窗/命令面板已迁移到原生 <dialog>，但 #command-palette id 与 role=dialog 语义不变：
 * 断言只依赖 id、role 与中文文本锚点，不假设面板内部结构。
 */
import { expect, test } from '@playwright/test'

test('弹窗打开时 Ctrl+K 不叠加命令面板，关闭后可正常开关面板', async ({ page }) => {
  await page.goto('/channels')
  await expect(page.getByRole('heading', { name: '渠道管理' })).toBeVisible()

  const modal = page.getByRole('dialog', { name: '新建渠道' })
  await page.getByRole('button', { name: '新建渠道' }).click()
  await expect(modal).toBeVisible()

  // 弹窗打开时按 Ctrl+K：全局快捷键的弹窗互斥防护应忽略本次按键，面板不得叠加
  await page.keyboard.press('Control+K')
  await expect(page.locator('#command-palette')).toHaveCount(0)
  // 弹窗仍在：确认按键已被页面处理、防护拦截的是面板而非弹窗本身
  await expect(modal).toBeVisible()

  // Esc 只关当前层（弹窗），命令面板保持关闭
  await page.keyboard.press('Escape')
  await expect(modal).toHaveCount(0)
  await expect(page.locator('#command-palette')).toHaveCount(0)

  // 无其他弹窗时 Ctrl+K 打开面板：可见、输入框聚焦且可输入
  await page.keyboard.press('Control+K')
  const palette = page.locator('#command-palette')
  await expect(palette).toBeVisible()
  const paletteInput = palette.locator('input')
  await expect(paletteInput).toBeFocused()
  await paletteInput.fill('渠道')
  await expect(paletteInput).toHaveValue('渠道')

  // 面板自身再按 Ctrl+K：开→关切换仍然有效（互斥防护只在存在其他弹窗时拦截）
  await page.keyboard.press('Control+K')
  await expect(palette).toHaveCount(0)
})

test.describe('懒加载 chunk 失败兜底', () => {
  // 被 abort 的 UsersPage chunk 请求次数：首次加载 1 次 + vite:preloadError 自动 reload 后 1 次 = 2。
  // 缺失自动 reload 会是 1 次；防循环保护失效（无限 reload）会超过 2 次——两种回归都能被该计数暴露。
  let chunkAttempts: number

  test.beforeEach(async ({ page }) => {
    chunkAttempts = 0
    // glob 匹配构建产物哈希文件名（如 UsersPage-DSsHCrWZ.js）。
    // page.route 的生命周期覆盖整页（含自动 reload 后的第二次加载），不能用 once。
    await page.route('**/assets/UsersPage-*.js', (route) => {
      chunkAttempts += 1
      return route.abort()
    })
  })

  test('UsersPage chunk 加载失败：自动 reload 一次后 ErrorBoundary 兜底', async ({ page }) => {
    await page.goto('/users')

    // 链路：首次动态 import 失败 → vite:preloadError 自动整页 reload → 再次失败
    // → ErrorBoundary 兜底卡。中间隔一次整页 reload，timeout 放宽到 10s 覆盖时序。
    await expect(page.getByText('页面加载失败')).toBeVisible({ timeout: 10_000 })
    await expect(page.getByRole('button', { name: '重新加载' })).toBeVisible({ timeout: 10_000 })

    // 恰好两次 chunk 请求：证明自动 reload 只发生一次（每会话最多重载一次的防循环保护生效）
    expect(chunkAttempts).toBe(2)
  })
})
