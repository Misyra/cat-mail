import { useCallback, useState } from 'react'

import { useChannels } from '@/lib/channels'
import { useApiKeys } from '@/lib/keys'
import { useSettings, type SystemSettings } from '@/lib/settings'

/**
 * 首启引导（仅仪表盘展示）：三步初始设置向导。
 * 状态全部由现有接口实时推导（GET /api/settings、GET /api/channels、GET /api/keys），零后端改动；
 * 不持久化进度——只持久化「不再显示」（localStorage 'catmail-onboarding-dismissed'），
 * 刷新后已完成步骤随接口数据自然保持打勾。
 */

/** GET /api/settings 附加字段：弱口令标记（后端 handler_settings.go WeakAdminPassword，本地收窄类型） */
export type OnboardingSettings = SystemSettings & { weak_admin_password?: boolean }

const ONBOARDING_DISMISSED_KEY = 'catmail-onboarding-dismissed'

/** 是否已跳过/完成过导览（隐私模式等读取失败时按未处理） */
export function readOnboardingDismissed(): boolean {
  try {
    return localStorage.getItem(ONBOARDING_DISMISSED_KEY) === '1'
  } catch {
    return false
  }
}

/** 写 localStorage 记住不再显示（三步全完成或用户主动跳过时调用；写入失败仅本次会话生效） */
export function writeOnboardingDismissed(): void {
  try {
    localStorage.setItem(ONBOARDING_DISMISSED_KEY, '1')
  } catch {
    // 忽略：隐私模式等场景
  }
}

/** 仪表盘用：是否渲染引导卡 + 跳过回调（刷新后重新读取 localStorage） */
export function useOnboardingDismissed(): [boolean, () => void] {
  const [dismissed, setDismissed] = useState(readOnboardingDismissed)
  const dismiss = useCallback(() => {
    writeOnboardingDismissed()
    setDismissed(true)
  }, [])
  return [dismissed, dismiss]
}

export type OnboardingStepId = 'password' | 'channel' | 'key'

/** 步骤标题 / 说明的词典 key（onboarding 命名空间；lib 层不直接调 i18n，渲染处 t() 出文案） */
export type OnboardingStepTitleKey =
  | 'onboarding:step.passwordTitle'
  | 'onboarding:step.channelTitle'
  | 'onboarding:step.keyTitle'

export type OnboardingStepDescriptionKey =
  | 'onboarding:step.passwordDesc'
  | 'onboarding:step.channelDesc'
  | 'onboarding:step.keyDesc'

export interface OnboardingStep {
  id: OnboardingStepId
  /** 步骤标题词典 key（渲染处 t(titleKey)） */
  titleKey: OnboardingStepTitleKey
  /** 步骤说明词典 key（渲染处 t(descriptionKey)） */
  descriptionKey: OnboardingStepDescriptionKey
  /** 「去完成」跳转页 */
  to: string
  done: boolean
}

/**
 * 三步完成态推导（复用各页既有 query key，不产生重复请求定义）：
 * 1. 修改管理员密码：GET /api/settings 的 weak_admin_password !== true（加载中视为未完成，避免闪现「已完成」）；
 * 2. 创建你的第一个渠道：GET /api/channels 数量 > 1（首启自带 1 条 Mock 示例渠道，不计入）；
 * 3. 创建 API Key：GET /api/keys 列表非空。
 * 三步互相独立、不强制顺序（纯展示进度，不阻塞任何功能）。
 */
export function useOnboardingSteps(): { steps: OnboardingStep[]; allDone: boolean } {
  const settingsQuery = useSettings()
  const channelsQuery = useChannels()
  const keysQuery = useApiKeys()

  // useSettings 返回 SystemSettings，weak_admin_password 为附加字段，本地收窄一次
  const settings = settingsQuery.data as OnboardingSettings | undefined

  const steps: OnboardingStep[] = [
    {
      id: 'password',
      titleKey: 'onboarding:step.passwordTitle',
      descriptionKey: 'onboarding:step.passwordDesc',
      to: '/settings',
      done: !settingsQuery.isPending && settings?.weak_admin_password !== true,
    },
    {
      id: 'channel',
      titleKey: 'onboarding:step.channelTitle',
      descriptionKey: 'onboarding:step.channelDesc',
      to: '/channels',
      done: (channelsQuery.data?.items.length ?? 0) > 1,
    },
    {
      id: 'key',
      titleKey: 'onboarding:step.keyTitle',
      descriptionKey: 'onboarding:step.keyDesc',
      to: '/keys',
      done: (keysQuery.data?.items.length ?? 0) > 0,
    },
  ]

  return { steps, allDone: steps.every((step) => step.done) }
}
