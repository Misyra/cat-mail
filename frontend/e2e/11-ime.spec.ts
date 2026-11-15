/**
 * 场景 11 输入法（O21 IME 回车防护）：日志/审计分页跳转输入框在中文输入法组词期间
 * 确认候选的 Enter（isComposing + keyCode 229）不得触发跳页；非组合态的正常 Enter 才跳页。
 * 事件用 dispatchEvent 注入（React 的事件委托挂在 root 上，冒泡可触发）；
 * 造数走 API：默认每页 20 条，各造 21 条保证第 2 页存在，跳页目标固定为 2。
 */
import { expect, test } from '@playwright/test'

import { apiCreateChannel, apiCreateKey, apiSend, uniqueName } from './helpers'

/** 保证第 2 页存在的最小记录数（默认每页 20 条） */
const RECORDS_FOR_TWO_PAGES = 21

test('日志页：IME 组合态回车不跳页，确认候选后正常回车才跳页', async ({ page, request }) => {
  const ch = await apiCreateChannel(request, uniqueName('e2e-ime-ch'))
  const key = await apiCreateKey(request, uniqueName('e2e-ime-key'))
  for (let i = 0; i < RECORDS_FOR_TWO_PAGES; i += 1) {
    await apiSend(request, key.key, ch.name, uniqueName('e2e-ime-mail'))
  }

  await page.goto('/logs')
  await expect(page.getByRole('heading', { name: '发送日志' })).toBeVisible()
  const jumpInput = page.getByLabel('跳转到指定页（回车确认）')

  // IME 组合态 Enter：确认候选，不应触发跳页
  await jumpInput.fill('2')
  await jumpInput.dispatchEvent('keydown', {
    key: 'Enter',
    code: 'Enter',
    keyCode: 229,
    isComposing: true,
  })
  // 路由同步在处理器返回后的微任务内完成，稍等确认 URL 未被改写
  await page.waitForTimeout(300)
  expect(new URL(page.url()).searchParams.get('page')).toBeNull()

  // 非组合态正常回车：跳到第 2 页
  await jumpInput.fill('2')
  await jumpInput.dispatchEvent('keydown', {
    key: 'Enter',
    code: 'Enter',
    keyCode: 13,
    isComposing: false,
  })
  await expect(page).toHaveURL(/\/logs\?page=2$/)
})

test('审计页：IME 组合态回车不跳页，确认候选后正常回车才跳页', async ({ page, request }) => {
  // 渠道新建属于管理操作，每次都会落一条审计记录
  for (let i = 0; i < RECORDS_FOR_TWO_PAGES; i += 1) {
    await apiCreateChannel(request, uniqueName('e2e-ime-audit-ch'))
  }

  await page.goto('/audit')
  await expect(page.getByRole('heading', { name: '审计日志' })).toBeVisible()
  const jumpInput = page.getByLabel('跳转到指定页（回车确认）')

  // IME 组合态 Enter：确认候选，不应触发跳页
  await jumpInput.fill('2')
  await jumpInput.dispatchEvent('keydown', {
    key: 'Enter',
    code: 'Enter',
    keyCode: 229,
    isComposing: true,
  })
  await page.waitForTimeout(300)
  expect(new URL(page.url()).searchParams.get('page')).toBeNull()

  // 非组合态正常回车：跳到第 2 页
  await jumpInput.fill('2')
  await jumpInput.dispatchEvent('keydown', {
    key: 'Enter',
    code: 'Enter',
    keyCode: 13,
    isComposing: false,
  })
  await expect(page).toHaveURL(/\/audit\?page=2$/)
})
