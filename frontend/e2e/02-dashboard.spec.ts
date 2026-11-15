/**
 * 场景 2 仪表盘：4 统计卡渲染 + 发送活动主图 SVG 出现 + 全程无控制台错误。
 * 先经 API 造一封 mock 发送，保证主图有数据（零数据时页面渲染空态而非图表）。
 */
import { expect, test } from '@playwright/test'

import { apiCreateChannel, apiCreateKey, apiSend, collectConsoleErrors, uniqueName } from './helpers'

test('统计卡渲染、图表 SVG 出现、无控制台错误', async ({ page, request }) => {
  const errors = collectConsoleErrors(page)

  // arrange：一封 mock 发送，让主图/统计卡有数据
  const chName = uniqueName('e2e-dash')
  const ch = await apiCreateChannel(request, chName)
  const key = await apiCreateKey(request, uniqueName('e2e-dash-key'))
  await apiSend(request, key.key, ch.name, uniqueName('e2e-dash-mail'))

  await page.goto('/')
  await expect(page.getByRole('heading', { name: '仪表盘' })).toBeVisible()

  // 4 统计卡（label 文案见 DashboardPage；卡内 label 是 <p>，主图图例「成功率」是 <span>，
  // 用 paragraph role 收窄避免 strict 冲突）
  for (const label of ['今日成功', '今日失败', '本月发送', '成功率']) {
    await expect(
      page.getByRole('paragraph').filter({ hasText: new RegExp(`^${label}$`) }),
    ).toBeVisible()
  }

  // 发送活动主图：recharts 渲染的 svg（有数据时出现）
  await expect(page.locator('svg.recharts-surface').first()).toBeVisible()

  // 渠道用量表：刚造的渠道出现在表中
  await expect(page.getByRole('cell', { name: chName })).toBeVisible()

  // 无控制台错误 / 未捕获异常
  expect(errors, `不应出现控制台错误，实际：\n${errors.join('\n')}`).toEqual([])
})
