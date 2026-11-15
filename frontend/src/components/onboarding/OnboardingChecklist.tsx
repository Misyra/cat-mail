import { ArrowRight, CheckCircle2 } from 'lucide-react'
import { useEffect } from 'react'
import { useTranslation } from 'react-i18next'
import { Link } from 'react-router'

import { Button } from '@/components/ui/Button'
import { Card } from '@/components/ui/Card'
import { useOnboardingSteps, type OnboardingStep } from '@/lib/onboarding'
import { cn } from '@/lib/utils'

/**
 * 首启引导卡（仅仪表盘统计卡下方展示）：三步初始设置，一行一步，紧凑布局。
 * 完成态绿勾 / 待办态序号 + 「去完成」跳转；三步全完成或点「跳过导览」→ 记入 localStorage 后不再显示。
 */
export function OnboardingChecklist({ onDismiss }: { onDismiss: () => void }) {
  const { t } = useTranslation(['onboarding'])
  const { steps, allDone } = useOnboardingSteps()
  const doneCount = steps.filter((step) => step.done).length

  // 三步全部完成 → 自动记忆不再显示（只存 dismiss，不存进度）
  useEffect(() => {
    if (allDone) onDismiss()
  }, [allDone, onDismiss])

  return (
    <Card className="p-5">
      <div className="flex items-start justify-between gap-3">
        <div>
          <h2 className="text-sm font-semibold text-text-primary">{t('title')}</h2>
          <p className="mt-0.5 text-xs text-text-muted">{t('intro')}</p>
        </div>
        <div className="flex shrink-0 items-center gap-1.5">
          <span
            className="rounded-full bg-sky-50 px-2 py-0.5 text-xs font-medium tabular-nums text-sky-700 dark:bg-sky-500/10 dark:text-sky-400"
            role="img"
            aria-label={t('progressAria', { count: doneCount, total: steps.length })}
          >
            {doneCount}/{steps.length}
          </span>
          <Button variant="ghost" className="px-2 py-1 text-xs" onClick={onDismiss}>
            {t('skip')}
          </Button>
        </div>
      </div>

      <ol className="mt-2">
        {steps.map((step, index) => (
          <OnboardingStepRow key={step.id} step={step} index={index} />
        ))}
      </ol>
    </Card>
  )
}

/** 单步状态图标：完成 = 绿勾；待办 = 序号圆圈 */
function StepBadge({ step, index }: { step: OnboardingStep; index: number }) {
  const { t } = useTranslation(['onboarding'])
  if (step.done) {
    return (
      <CheckCircle2
        className="h-5 w-5 shrink-0 text-emerald-500 dark:text-emerald-400"
        role="img"
        aria-label={t('step.done')}
      />
    )
  }
  return (
    <span
      className="flex h-5 w-5 shrink-0 items-center justify-center rounded-full border border-slate-300 text-[11px] font-medium text-text-muted dark:border-slate-600"
      role="img"
      aria-label={t('step.pendingAria')}
    >
      {index + 1}
    </span>
  )
}

function OnboardingStepRow({ step, index }: { step: OnboardingStep; index: number }) {
  const { t } = useTranslation(['onboarding'])
  return (
    <li
      className={cn(
        'flex items-center gap-3 py-3',
        index > 0 && 'border-t border-border-subtle',
      )}
    >
      <StepBadge step={step} index={index} />
      <div className="min-w-0 flex-1">
        <p className={cn('text-sm font-medium', step.done ? 'text-text-muted' : 'text-text-primary')}>
          {t(step.titleKey)}
        </p>
        <p className="mt-0.5 truncate text-xs text-text-muted">{t(step.descriptionKey)}</p>
      </div>
      {step.done ? (
        <span className="shrink-0 text-xs font-medium text-emerald-600 dark:text-emerald-400">{t('step.done')}</span>
      ) : (
        <Button as={Link} to={step.to} variant="secondary" className="shrink-0 px-3 py-1.5 text-xs">
          {t('step.go')}
          <ArrowRight className="h-3.5 w-3.5" aria-hidden />
        </Button>
      )}
    </li>
  )
}
