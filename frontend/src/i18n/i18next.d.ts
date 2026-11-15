// i18next TS key 校验：以 zh-CN 词典为准推导各命名空间的 key 类型（en-US 须与 zh-CN 同构）。
// 新增命名空间：建 locales/<lang>/<ns>.json 文件对，并在下方补一行 typeof import。
import type audit from './locales/zh-CN/audit.json'
import type auth from './locales/zh-CN/auth.json'
import type channels from './locales/zh-CN/channels.json'
import type common from './locales/zh-CN/common.json'
import type dashboard from './locales/zh-CN/dashboard.json'
import type domains from './locales/zh-CN/domains.json'
import type events from './locales/zh-CN/events.json'
import type keys from './locales/zh-CN/keys.json'
import type layout from './locales/zh-CN/layout.json'
import type logs from './locales/zh-CN/logs.json'
import type notifications from './locales/zh-CN/notifications.json'
import type onboarding from './locales/zh-CN/onboarding.json'
import type settings from './locales/zh-CN/settings.json'
import type suppressions from './locales/zh-CN/suppressions.json'
import type templates from './locales/zh-CN/templates.json'
import type ui from './locales/zh-CN/ui.json'
import type users from './locales/zh-CN/users.json'

declare module 'i18next' {
  interface CustomTypeOptions {
    defaultNS: 'common'
    resources: {
      audit: typeof audit
      auth: typeof auth
      channels: typeof channels
      common: typeof common
      dashboard: typeof dashboard
      domains: typeof domains
      events: typeof events
      keys: typeof keys
      layout: typeof layout
      logs: typeof logs
      notifications: typeof notifications
      onboarding: typeof onboarding
      settings: typeof settings
      suppressions: typeof suppressions
      templates: typeof templates
      ui: typeof ui
      users: typeof users
    }
  }
}
