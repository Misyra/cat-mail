/**
 * 场景 7 账号：新建账号成功入列表；自己那行「删除」按钮禁用（不能删自己），
 * 新建账号那行删除可用。
 */
import { expect, test } from '@playwright/test'

import { ADMIN_USER, uniqueName } from './helpers'

test('新建账号，不能删除自己', async ({ page }) => {
  const uname = uniqueName('e2e-user')
  const password = 'e2e-pass-123'

  await page.goto('/users')
  await expect(page.getByRole('heading', { name: '账号管理' })).toBeVisible()

  // 新建账号
  await page.getByRole('button', { name: '新建账号' }).click()
  const modal = page.getByRole('dialog', { name: '新建账号' })
  await expect(modal).toBeVisible()
  await modal.getByLabel(/^用户名/).fill(uname)
  // Field 组件把 hint（「至少 6 位」）包进 label，可访问名是「密码 至少 6 位」→ 用前缀锚点；
  // /^密码/ 不会命中「确认密码」
  await modal.getByLabel(/^密码/).fill(password)
  await modal.getByLabel(/^确认密码/).fill(password)
  await modal.getByRole('button', { name: '创建账号' }).click()
  await expect(page.getByText(`账号「${uname}」已创建`)).toBeVisible()
  await expect(modal).toHaveCount(0)

  // 新账号出现在列表，删除按钮可用
  const newRow = page.getByRole('row', { name: new RegExp(uname) })
  await expect(newRow).toBeVisible()
  await expect(newRow.getByRole('button', { name: '删除' })).toBeEnabled()

  // 自己（admin）所在行带「当前账号」徽章，删除按钮禁用
  const selfRow = page.getByRole('row', { name: /当前账号/ })
  await expect(selfRow.getByText(ADMIN_USER, { exact: true })).toBeVisible()
  const selfDelete = selfRow.getByRole('button', { name: '删除' })
  await expect(selfDelete).toBeDisabled()
  await expect(selfDelete).toHaveAttribute('title', '不能删除当前登录的账号')
})
