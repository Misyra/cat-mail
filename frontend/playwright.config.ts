import { defineConfig } from '@playwright/test'
import { fileURLToPath } from 'node:url'

import { BASE_URL, PORT } from './e2e/helpers'

/**
 * C1 自动化 E2E（Playwright）：
 * - webServer 一次性后端实例（scripts/e2e-server.sh：前端构建 + go build + 临时库 + :18100），
 *   Playwright 轮询 /api/health 就绪后跑测试，结束时自动收掉进程并清理临时目录。
 * - 单 worker：后端为 SQLite 单库共享状态，避免并发测试串数据。
 * - 登录态：setup 项目 UI 登录一次 → storageState 复用（登录场景本身除外）。
 */
export default defineConfig({
  testDir: './e2e',
  testMatch: '**/*.spec.ts',
  // 单 worker + 串行：共享一个后端实例（SQLite + 种子数据），并发会互相污染
  fullyParallel: false,
  workers: 1,
  timeout: 30_000,
  expect: { timeout: 7_000 },
  retries: 1,
  reporter: [['list']],
  outputDir: fileURLToPath(new URL('./e2e/.tmp/test-results', import.meta.url)),
  use: {
    baseURL: BASE_URL,
    // 主题测试依赖可预期的系统明暗（跟随系统 → 解析为浅色）
    colorScheme: 'light',
    locale: 'zh-CN',
    screenshot: 'only-on-failure',
    trace: 'retain-on-failure',
  },
  projects: [
    {
      name: 'setup',
      testMatch: '**/*.setup.ts',
    },
    {
      name: 'chromium',
      dependencies: ['setup'],
      use: {
        storageState: fileURLToPath(new URL('./e2e/.auth/user.json', import.meta.url)),
      },
    },
  ],
  webServer: {
    // 以仓库根为 cwd 启动一次性后端（脚本内部自行定位仓库与前端目录）
    command: 'bash scripts/e2e-server.sh',
    cwd: fileURLToPath(new URL('..', import.meta.url)),
    url: `http://127.0.0.1:${PORT}/api/health`,
    reuseExistingServer: false,
    timeout: 300_000,
  },
})
