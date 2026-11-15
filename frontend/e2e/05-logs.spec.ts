/**
 * 场景 5 日志：列表含记录 → 详情停靠面板打开 → CSV 导出触发下载事件且内容正确。
 * 造数走 API（mock 渠道 + /v1/send），UI 只点主题单元格打开详情（避开行操作区新元素）。
 */
import { readFileSync } from 'node:fs'
import { expect, test } from '@playwright/test'

import { apiCreateChannel, apiCreateKey, apiSend, uniqueName } from './helpers'

test('日志列表、详情面板、CSV 导出', async ({ page, request }) => {
  const chName = uniqueName('e2e-log-ch')
  const subject = uniqueName('e2e-log-mail')
  const ch = await apiCreateChannel(request, chName)
  const key = await apiCreateKey(request, uniqueName('e2e-log-key'))
  const sent = await apiSend(request, key.key, ch.name, subject)
  expect(sent.accepted).toBe(true)

  await page.goto('/logs')
  await expect(page.getByRole('heading', { name: '发送日志' })).toBeVisible()

  // 列表含该记录（按主题定位主题单元格）
  const subjectCell = page.getByRole('cell', { name: subject })
  await expect(subjectCell).toBeVisible()

  // 打开详情停靠面板
  await subjectCell.click()
  const panel = page.getByRole('dialog', { name: '日志详情' })
  await expect(panel).toBeVisible()
  // 面板字段含主题与 message id
  await expect(panel.getByText(subject).first()).toBeVisible()
  await expect(panel.getByText(sent.id).first()).toBeVisible()

  // 关闭面板：面板遮罩盖住工具栏，导出须在面板关闭后进行（与真实用户操作一致）
  await panel.getByRole('button', { name: '关闭' }).click()
  await expect(panel).toHaveCount(0)

  // CSV 导出：等待下载事件，校验文件名与内容（表头「时间」+ 主题）
  const downloadPromise = page.waitForEvent('download')
  await page.getByRole('button', { name: '导出 CSV' }).click()
  const download = await downloadPromise
  expect(download.suggestedFilename()).toMatch(/\.csv$/)
  const csvPath = await download.path()
  const csv = readFileSync(csvPath, 'utf8')
  expect(csv).toContain('时间')
  expect(csv).toContain(subject)
})
