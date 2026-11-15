/**
 * 系统设置页（SPEC §6）：轮换调度参数表单（策略/最大尝试/冷却秒数/连续失败阈值/日志保留天数）
 * + 失败通知卡片（站内通知 / Webhook / 通知事件）+ 日志隐私卡片（记录邮件正文开关）
 * + 数据管理卡片（A2：每日自动备份开关 / 立即备份与备份列表 / 日志清理红色确认）
 * + 系统更新卡片（E8：检查更新 / 一键更新，容器部署改示镜像升级提示）
 * + 管理员修改密码独立卡片（成功后当前会话保持登录，本账号其他会话全部失效）+ 弱口令警告条。
 * 数据：GET/PUT /api/settings、PUT /api/auth/password、POST /api/settings/notify/test、
 * GET/POST/DELETE /api/backups、POST /api/logs/purge、GET /api/update/check、
 * POST /api/update/apply（SPEC §5，snake_case 原样字段名）。
 */
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import {
  AlertCircle,
  AlertTriangle,
  Check,
  Copy,
  DatabaseBackup,
  Download,
  ExternalLink,
  RefreshCw,
  Rocket,
  Trash2,
  X,
} from 'lucide-react'
import { useState, type FormEvent } from 'react'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import { PageHeader } from '@/components/PageHeader'
import { QueryError, QueryLoading } from '@/components/QueryState'
import { Badge } from '@/components/badges'
import { SessionsCard } from '@/components/settings/SessionsCard'
import { TotpCard } from '@/components/settings/TotpCard'
import { Button } from '@/components/ui/Button'
import { Card } from '@/components/ui/Card'
import { EmptyState } from '@/components/ui/EmptyState'
import { Field, Input, Select } from '@/components/ui/Input'
import { Modal } from '@/components/ui/Modal'
import { api } from '@/lib/api'
import {
  BACKUPS_QUERY_KEY,
  backupDownloadURL,
  createBackup,
  deleteBackup,
  formatBytes,
  listBackups,
  normalizeBackupDailyEnabled,
  purgeLogs,
  type BackupItem,
  type BackupSettingsFields,
  type SystemSettingsWithBackup,
  type LogPurgeMode,
  type LogPurgeRequest,
} from '@/lib/backups'
import { useChannels, type Channel } from '@/lib/channels'
import { copyText } from '@/lib/clipboard'
import { formatDateTime } from '@/lib/format'
import {
  NOTIFY_EVENTS,
  NOTIFY_LEVEL_LABEL_KEYS,
  normalizeNotifyFields,
  notifyLevelVariant,
  parseNotifyEvents,
  rotateNotifyWebhookSecret,
  serializeNotifyEvents,
  testNotifySettings,
  type NotifyEventKey,
  type NotifyTestResponse,
  type SystemSettingsWithNotify,
} from '@/lib/notifications'
import {
  SETTINGS_QUERY_KEY,
  normalizeBounceRateThreshold,
  normalizeLogBodyEnabled,
  normalizeRotation,
  normalizeSuppressionAutoEnabled,
  useSettings,
  type RotationStrategy,
  type SystemSettings,
} from '@/lib/settings'
import { setLanguage, useLanguage, type Language } from '@/i18n'
import { useTheme, type Theme } from '@/lib/theme'
import {
  UPDATE_CHECK_QUERY_KEY,
  applyUpdate,
  checkUpdate,
  type UpdateApplyResult,
} from '@/lib/update'
import { cn, toInt, utf8ByteLength } from '@/lib/utils'

/* ---------------------------------- 设置响应类型（本地收窄） ---------------------------------- */

/** GET /api/settings 响应：失败通知四字段（lib/notifications.ts）+ 每日自动备份字段（lib/backups.ts）+ 后端弱口令标记（admin 初始口令未改时为 true） */
type SystemSettingsWithWeakPassword = SystemSettingsWithNotify & BackupSettingsFields & {
  weak_admin_password?: boolean
}

/* ---------------------------------- 界面主题 ---------------------------------- */

/** 界面主题选项：label 存 common 词典 key，渲染处 t(labelKey) 出文案 */
const THEME_OPTIONS = [
  { value: 'light', labelKey: 'common:theme.light' },
  { value: 'glass', labelKey: 'common:theme.glass' },
  { value: 'dark', labelKey: 'common:theme.dark' },
  { value: 'system', labelKey: 'common:theme.system' },
] as const

/** 界面语言选项：label 用语言自称（语言中立，不随界面语言翻译） */
const LANGUAGE_OPTIONS = [
  { value: 'zh-CN', label: '中文（简体）' },
  { value: 'en-US', label: 'English' },
] as const

/** 界面主题与语言：与顶栏切换按钮联动（同一 localStorage 键，任一处修改即时同步） */
function AppearanceCard() {
  const { theme, setTheme } = useTheme()
  const language = useLanguage()
  const { t } = useTranslation(['settings', 'common'])

  return (
    <Card>
      <CardHeader title={t('appearance.title')} description={t('appearance.description')} />
      <div className="px-5 py-4">
        <Field label={t('appearance.themeLabel')}>
          <Select
            aria-label={t('appearance.themeLabel')}
            value={theme}
            onChange={(e) => setTheme(e.target.value as Theme)}
          >
            {THEME_OPTIONS.map((opt) => (
              <option key={opt.value} value={opt.value}>
                {t(opt.labelKey)}
              </option>
            ))}
          </Select>
        </Field>
        <Field label={t('appearance.languageLabel')}>
          <Select
            aria-label={t('appearance.languageLabel')}
            value={language}
            onChange={(e) => setLanguage(e.target.value as Language)}
          >
            {LANGUAGE_OPTIONS.map((opt) => (
              <option key={opt.value} value={opt.value}>
                {opt.label}
              </option>
            ))}
          </Select>
        </Field>
      </div>
    </Card>
  )
}

/* ---------------------------------- 表单模型（数字字段以字符串承载，提交时解析） ---------------------------------- */

interface SettingsFormValues {
  rotation: RotationStrategy
  max_attempts: string
  cooldown_seconds: string
  consecutive_failure_threshold: string
  log_retention_days: string
}

/** 校验错误存词典 key（模块级函数不直接调 i18n，渲染处 t() 出文案） */
type SettingsErrorKey =
  | 'errors.maxAttempts'
  | 'errors.cooldownSeconds'
  | 'errors.failureThreshold'
  | 'errors.logRetentionDays'

type SettingsErrors = Partial<
  Record<'max_attempts' | 'cooldown_seconds' | 'consecutive_failure_threshold' | 'log_retention_days', SettingsErrorKey>
>

function valuesFromSettings(s: SystemSettings): SettingsFormValues {
  return {
    rotation: normalizeRotation(s.rotation),
    max_attempts: String(s.max_attempts ?? 3),
    cooldown_seconds: String(s.cooldown_seconds ?? 60),
    consecutive_failure_threshold: String(s.consecutive_failure_threshold ?? 5),
    log_retention_days: String(s.log_retention_days ?? 90),
  }
}

function validateValues(v: SettingsFormValues): SettingsErrors {
  const errors: SettingsErrors = {}
  const maxAttempts = toInt(v.max_attempts)
  if (maxAttempts === undefined || maxAttempts < 1 || maxAttempts > 10) {
    errors.max_attempts = 'errors.maxAttempts'
  }
  const cooldown = toInt(v.cooldown_seconds)
  if (cooldown === undefined || cooldown < 1) {
    errors.cooldown_seconds = 'errors.cooldownSeconds'
  }
  const threshold = toInt(v.consecutive_failure_threshold)
  if (threshold === undefined || threshold < 3 || threshold > 20) {
    errors.consecutive_failure_threshold = 'errors.failureThreshold'
  }
  const retention = toInt(v.log_retention_days)
  if (retention === undefined || retention < 0 || retention > 365) {
    errors.log_retention_days = 'errors.logRetentionDays'
  }
  return errors
}

/** PUT 请求体 = 最新服务端设置整体 + 本卡片轮换字段覆盖（失败通知四字段原样透传，避免整写覆盖） */
function buildPayload(v: SettingsFormValues, base: SystemSettingsWithNotify): SystemSettings {
  return {
    ...base,
    rotation: v.rotation,
    max_attempts: toInt(v.max_attempts) ?? 3,
    cooldown_seconds: toInt(v.cooldown_seconds) ?? 60,
    consecutive_failure_threshold: toInt(v.consecutive_failure_threshold) ?? 5,
    log_retention_days: toInt(v.log_retention_days) ?? 90,
  }
}

/* ---------------------------------- 通用小部件 ---------------------------------- */

function CardHeader({ title, description }: { title: string; description?: string }) {
  return (
    <div className="border-b border-slate-200 px-5 py-4 dark:border-slate-800">
      <h2 className="text-sm font-semibold text-slate-900 dark:text-white">{title}</h2>
      {description && (
        <p className="mt-0.5 text-xs text-slate-500 dark:text-slate-400">{description}</p>
      )}
    </div>
  )
}

/* ---------------------------------- 弱口令警告条 ---------------------------------- */

/** 弱口令警告条：admin 初始口令未修改时在页面顶部展示，锚点滚动到下方改密表单 */
function WeakPasswordBanner() {
  const { t } = useTranslation(['settings', 'common'])

  function goToChangePassword() {
    document
      .getElementById('change-password')
      ?.scrollIntoView({ behavior: 'smooth', block: 'start' })
  }

  return (
    <div
      role="alert"
      className="flex flex-wrap items-center gap-x-3 gap-y-2 rounded-xl border border-amber-300 bg-amber-50 px-4 py-3 dark:border-amber-400/30 dark:bg-amber-400/10"
    >
      <AlertTriangle className="h-4 w-4 shrink-0 text-amber-500" aria-hidden />
      <p className="min-w-0 flex-1 text-sm font-medium text-amber-800 dark:text-amber-200">
        {t('weakPassword.message')}
      </p>
      <button
        type="button"
        onClick={goToChangePassword}
        className="shrink-0 text-sm font-medium text-amber-700 underline-offset-2 transition-colors hover:text-amber-800 hover:underline dark:text-amber-300 dark:hover:text-amber-200"
      >
        {t('weakPassword.action')}
      </button>
    </div>
  )
}

/* ---------------------------------- 轮换调度设置卡片 ---------------------------------- */

function RotationSettingsCard({ settings }: { settings: SystemSettingsWithNotify }) {
  const queryClient = useQueryClient()
  const { t } = useTranslation(['settings', 'common'])
  const [values, setValues] = useState<SettingsFormValues>(() => valuesFromSettings(settings))
  const [errors, setErrors] = useState<SettingsErrors>({})

  const mutation = useMutation({
    mutationFn: (payload: SystemSettings) =>
      api<SystemSettings>('/api/settings', { method: 'PUT', body: payload }),
    onSuccess: (saved) => {
      toast.success(t('toast.saved'))
      // 用服务端最新值重置表单（审计 P2-5）：先写缓存保证即时一致，再失效触发复核
      setValues(valuesFromSettings(saved))
      setErrors({})
      queryClient.setQueryData(SETTINGS_QUERY_KEY, saved)
      void queryClient.invalidateQueries({ queryKey: SETTINGS_QUERY_KEY })
    },
    onError: (err) => toast.error(err.message),
  })

  function setField<K extends keyof SettingsFormValues>(key: K, value: SettingsFormValues[K]) {
    setValues((prev) => ({ ...prev, [key]: value }))
    if (key !== 'rotation') {
      setErrors((prev) => ({ ...prev, [key]: undefined }))
    }
  }

  function handleSubmit(e: FormEvent<HTMLFormElement>) {
    e.preventDefault()
    const nextErrors = validateValues(values)
    setErrors(nextErrors)
    if (Object.values(nextErrors).some(Boolean)) return
    mutation.mutate(buildPayload(values, settings))
  }

  // 日志保留天数动态标注：0=永久保留、>0=保留 N 天；非法输入不显示（跟随当前输入值）
  const retention = toInt(values.log_retention_days)
  const retentionBadge =
    retention !== undefined && retention >= 0 ? (
      <Badge variant={retention === 0 ? 'success' : 'info'} className="shrink-0">
        {retention === 0 ? t('rotation.retentionForever') : t('rotation.retentionDays', { count: retention })}
      </Badge>
    ) : null

  return (
    <Card>
      <CardHeader title={t('rotation.title')} description={t('rotation.description')} />
      <form aria-label={t('rotation.title')} onSubmit={handleSubmit} className="space-y-5 px-5 py-4" noValidate>
        <Field
          label={t('rotation.strategy')}
          hint={t('rotation.strategyHint')}
        >
          <Select
            name="rotation"
            value={values.rotation}
            onChange={(e) => setField('rotation', normalizeRotation(e.target.value))}
          >
            <option value="priority">{t('rotation.priority')}</option>
            <option value="round_robin">{t('rotation.roundRobin')}</option>
          </Select>
        </Field>
        <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
          <Field
            label={t('rotation.maxAttempts')}
            error={errors.max_attempts ? t(errors.max_attempts) : undefined}
            hint={t('rotation.maxAttemptsHint')}
          >
            <Input
              name="max_attempts"
              type="number"
              min={1}
              max={10}
              value={values.max_attempts}
              onChange={(e) => setField('max_attempts', e.target.value)}
            />
          </Field>
          <Field
            label={t('rotation.cooldownSeconds')}
            error={errors.cooldown_seconds ? t(errors.cooldown_seconds) : undefined}
            hint={t('rotation.cooldownHint')}
          >
            <Input
              name="cooldown_seconds"
              type="number"
              min={1}
              value={values.cooldown_seconds}
              onChange={(e) => setField('cooldown_seconds', e.target.value)}
            />
          </Field>
          <Field
            label={t('rotation.failureThreshold')}
            error={errors.consecutive_failure_threshold ? t(errors.consecutive_failure_threshold) : undefined}
            hint={t('rotation.failureThresholdHint')}
          >
            <Input
              name="consecutive_failure_threshold"
              type="number"
              min={3}
              max={20}
              value={values.consecutive_failure_threshold}
              onChange={(e) => setField('consecutive_failure_threshold', e.target.value)}
            />
          </Field>
          <Field
            label={t('rotation.retentionDaysLabel')}
            error={errors.log_retention_days ? t(errors.log_retention_days) : undefined}
            hint={t('rotation.retentionHint')}
            labelExtra={retentionBadge}
          >
            <Input
              name="log_retention_days"
              type="number"
              min={0}
              max={365}
              value={values.log_retention_days}
              onChange={(e) => setField('log_retention_days', e.target.value)}
            />
          </Field>
        </div>
        <div className="flex justify-end">
          <Button type="submit" loading={mutation.isPending}>
            {t('saveSettings')}
          </Button>
        </div>
      </form>
    </Card>
  )
}

/* ---------------------------------- 失败通知卡片 ---------------------------------- */

const WEBHOOK_URL_PATTERN = /^https?:\/\//i

/** 滑动开关（设置页局部实现，switch 语义） */
function ToggleSwitch({
  checked,
  onChange,
  label,
}: {
  checked: boolean
  onChange: (next: boolean) => void
  label: string
}) {
  return (
    <button
      type="button"
      role="switch"
      aria-checked={checked}
      aria-label={label}
      onClick={() => onChange(!checked)}
      className={cn(
        'relative inline-flex h-5 w-9 shrink-0 items-center rounded-full transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-sky-500/40',
        checked ? 'bg-sky-600' : 'bg-slate-300 dark:bg-slate-600',
      )}
    >
      <span
        className={cn(
          'inline-block h-4 w-4 rounded-full bg-white shadow transition-transform',
          checked ? 'translate-x-[1.125rem]' : 'translate-x-0.5',
        )}
      />
    </button>
  )
}

/**
 * 通知 Webhook 签名密钥行（安全加固三件套，B）：展示当前密钥 + 复制；未配置时
 * 「生成签名密钥」，已配置时「重新生成」。变更仅走 POST /api/settings/notify/
 * webhook-secret（PUT /api/settings 忽略该字段，防整表单保存误清）；轮换后存量
 * 未投递的 outbox 通知按新值签名，接收方需同步更新校验密钥（文案提示）。
 */
function WebhookSecretRow({ secret }: { secret: string }) {
  const queryClient = useQueryClient()
  const { t } = useTranslation(['settings', 'common'])
  const [copied, setCopied] = useState(false)

  const mutation = useMutation({
    mutationFn: (rotate: boolean) => rotateNotifyWebhookSecret(rotate),
    onSuccess: (res, rotate) => {
      // 更新设置缓存与本页展示（服务端已持久化，无需重拉）
      queryClient.setQueryData<SystemSettingsWithNotify>(SETTINGS_QUERY_KEY, (old) =>
        old ? { ...old, notify_webhook_secret: res.webhook_secret } : old,
      )
      setCopied(false)
      toast.success(rotate ? t('toast.secretRotated') : t('toast.secretCreated'))
    },
    onError: (err) => toast.error(err.message),
  })

  async function handleCopy() {
    try {
      await copyText(secret)
      setCopied(true)
      toast.success(t('common:actions.copied'))
    } catch {
      toast.error(t('toast.copyFailed'))
    }
  }

  return (
    <div className="rounded-lg border border-border-subtle px-3 py-3">
      <div className="flex items-start justify-between gap-3">
        <div className="min-w-0">
          <p className="text-sm font-medium text-slate-700 dark:text-slate-300">{t('notify.secret')}</p>
          <p className="mt-0.5 text-xs leading-relaxed text-slate-500 dark:text-slate-400">
            {t('notify.secretDesc')}
          </p>
        </div>
        {secret ? (
          <Button
            type="button"
            variant="secondary"
            className="shrink-0"
            loading={mutation.isPending}
            onClick={() => {
              if (window.confirm(t('notify.rotateConfirm'))) {
                mutation.mutate(true)
              }
            }}
          >
            {t('notify.regenerate')}
          </Button>
        ) : (
          <Button
            type="button"
            variant="secondary"
            className="shrink-0"
            loading={mutation.isPending}
            onClick={() => mutation.mutate(false)}
          >
            {t('notify.generate')}
          </Button>
        )}
      </div>
      {secret && (
        <div className="mt-2.5 flex items-center gap-2">
          <code className="min-w-0 flex-1 truncate rounded-md border border-border-subtle bg-slate-50 px-2.5 py-1.5 font-mono text-xs text-slate-700 dark:bg-slate-900/40 dark:text-slate-300">
            {secret}
          </code>
          <Button type="button" variant="ghost" className="h-7 shrink-0 px-2 text-xs" onClick={handleCopy}>
            {copied ? (
              <Check className="h-3.5 w-3.5 text-emerald-500" aria-hidden />
            ) : (
              <Copy className="h-3.5 w-3.5" aria-hidden />
            )}
            {copied ? t('copied') : t('common:actions.copy')}
          </Button>
        </div>
      )}
    </div>
  )
}

/**
 * 失败通知卡片：站内通知 / Webhook 开关与 URL / 通知事件四选一以上。
 * 保存并入现有 PUT /api/settings 整体保存流（请求体 = 最新服务端设置 + 本卡片字段覆盖）。
 */
function NotifySettingsCard({ settings }: { settings: SystemSettingsWithNotify }) {
  const queryClient = useQueryClient()
  const { t } = useTranslation(['settings', 'common', 'notifications'])
  const initial = normalizeNotifyFields(settings)
  const [inappEnabled, setInappEnabled] = useState(initial.notify_inapp_enabled)
  const [webhookEnabled, setWebhookEnabled] = useState(initial.notify_webhook_enabled)
  const [webhookUrl, setWebhookUrl] = useState(initial.notify_webhook_url)
  // 签名密钥（安全加固三件套，B）：仅随专用端点变更（WebhookSecretRow），保存表单
  // 不携带/不覆盖（后端 PUT 整体忽略该字段）
  const [webhookSecret, setWebhookSecret] = useState(initial.notify_webhook_secret)
  const [events, setEvents] = useState<Set<NotifyEventKey>>(() =>
    parseNotifyEvents(initial.notify_events),
  )
  const [errors, setErrors] = useState<{ webhook_url?: string }>({})
  const [testResult, setTestResult] = useState<NotifyTestResponse | null>(null)
  const [testError, setTestError] = useState<string | null>(null)

  const mutation = useMutation({
    mutationFn: (payload: SystemSettingsWithNotify) =>
      api<SystemSettings>('/api/settings', { method: 'PUT', body: payload }),
    onSuccess: (saved) => {
      toast.success(t('toast.saved'))
      // 用服务端最新值重置表单（与轮换卡片同款交互）
      const fields = normalizeNotifyFields(saved)
      setInappEnabled(fields.notify_inapp_enabled)
      setWebhookEnabled(fields.notify_webhook_enabled)
      setWebhookUrl(fields.notify_webhook_url)
      setWebhookSecret(fields.notify_webhook_secret)
      setEvents(parseNotifyEvents(fields.notify_events))
      setErrors({})
      queryClient.setQueryData(SETTINGS_QUERY_KEY, saved)
      void queryClient.invalidateQueries({ queryKey: SETTINGS_QUERY_KEY })
    },
    onError: (err) => toast.error(err.message),
  })

  const testMutation = useMutation({
    mutationFn: testNotifySettings,
    onSuccess: (res) => {
      setTestResult(res)
      setTestError(null)
    },
    onError: (err) => {
      setTestResult(null)
      setTestError(err.message)
    },
  })

  function toggleEvent(key: NotifyEventKey, checked: boolean) {
    setEvents((prev) => {
      const next = new Set(prev)
      if (checked) next.add(key)
      else next.delete(key)
      return next
    })
  }

  function setUrl(value: string) {
    setWebhookUrl(value)
    setErrors((prev) => ({ ...prev, webhook_url: undefined }))
    // 输入已变化，旧测试结果不再代表当前配置
    setTestResult(null)
    setTestError(null)
  }

  function handleWebhookToggle(next: boolean) {
    setWebhookEnabled(next)
    setErrors((prev) => ({ ...prev, webhook_url: undefined }))
    setTestResult(null)
    setTestError(null)
  }

  function handleSubmit(e: FormEvent<HTMLFormElement>) {
    e.preventDefault()
    const nextErrors: typeof errors = {}
    const url = webhookUrl.trim()
    if (webhookEnabled) {
      if (!url) nextErrors.webhook_url = t('notify.urlRequired')
      else if (!WEBHOOK_URL_PATTERN.test(url)) nextErrors.webhook_url = t('notify.urlPattern')
    }
    setErrors(nextErrors)
    if (Object.values(nextErrors).some(Boolean)) return
    mutation.mutate({
      ...settings,
      notify_inapp_enabled: inappEnabled,
      notify_webhook_enabled: webhookEnabled,
      notify_webhook_url: url,
      notify_events: serializeNotifyEvents(events),
    })
  }

  return (
    <Card>
      <CardHeader title={t('notify.title')} description={t('notify.description')} />
      <form aria-label={t('notify.title')} onSubmit={handleSubmit} className="space-y-5 px-5 py-4" noValidate>
        {/* 站内通知开关 */}
        <div className="flex items-center justify-between gap-4">
          <div className="min-w-0">
            <p className="text-sm font-medium text-slate-700 dark:text-slate-300">{t('notify.inapp')}</p>
            <p className="mt-0.5 text-xs text-slate-500 dark:text-slate-400">
              {t('notify.inappDesc')}
            </p>
          </div>
          <ToggleSwitch
            checked={inappEnabled}
            onChange={setInappEnabled}
            label={t('notify.inappSwitch')}
          />
        </div>

        {/* Webhook 开关 + URL */}
        <div className="flex items-center justify-between gap-4">
          <div className="min-w-0">
            <p className="text-sm font-medium text-slate-700 dark:text-slate-300">{t('notify.webhook')}</p>
            <p className="mt-0.5 text-xs text-slate-500 dark:text-slate-400">
              {t('notify.webhookDesc')}
            </p>
          </div>
          <ToggleSwitch
            checked={webhookEnabled}
            onChange={handleWebhookToggle}
            label={t('notify.webhookSwitch')}
          />
        </div>
        {webhookEnabled && (
          <>
            <Field label={t('notify.urlLabel')} error={errors.webhook_url} hint={t('notify.urlHint')}>
              <Input
                name="notify_webhook_url"
                type="url"
                value={webhookUrl}
                onChange={(e) => setUrl(e.target.value)}
                placeholder="https://example.com/hooks/catmail"
              />
            </Field>
            <WebhookSecretRow secret={webhookSecret} />
          </>
        )}

        {/* 通知事件（至少勾选一个） */}
        <fieldset>
          <legend className="mb-1.5 text-sm font-medium text-slate-700 dark:text-slate-300">
            {t('notify.events')}
          </legend>
          <div className="grid grid-cols-1 gap-2 sm:grid-cols-2">
            {NOTIFY_EVENTS.map((meta) => (
              <label
                key={meta.key}
                className="flex cursor-pointer items-start gap-2.5 rounded-lg border border-border-subtle px-3 py-2.5 transition-colors hover:border-slate-300 dark:hover:border-slate-600"
              >
                <input
                  type="checkbox"
                  className="mt-0.5 h-4 w-4 shrink-0 accent-sky-600"
                  checked={events.has(meta.key)}
                  onChange={(e) => toggleEvent(meta.key, e.target.checked)}
                />
                <span className="min-w-0">
                  <span className="flex items-center gap-1.5 text-sm font-medium text-slate-700 dark:text-slate-300">
                    {t(meta.labelKey)}
                    <Badge variant={notifyLevelVariant(meta.level)}>
                      {t(NOTIFY_LEVEL_LABEL_KEYS[meta.level])}
                    </Badge>
                  </span>
                  <span className="mt-0.5 block text-xs text-slate-500 dark:text-slate-400">
                    {t(meta.descriptionKey)}
                  </span>
                </span>
              </label>
            ))}
          </div>
          {events.size === 0 && (
            <p className="mt-2 flex items-start gap-1.5 text-xs text-red-500">
              <AlertCircle className="mt-0.5 h-3.5 w-3.5 shrink-0" aria-hidden />
              {t('notify.noEvents')}
            </p>
          )}
        </fieldset>

        {/* 测试结果内联展示 */}
        {testResult && (
          <div className="space-y-1.5 rounded-lg border border-border-subtle bg-slate-50 px-3 py-2.5 text-sm dark:bg-slate-900/40">
            <p className="flex items-center gap-1.5">
              {testResult.inapp_ok ? (
                <Check className="h-4 w-4 shrink-0 text-emerald-500" aria-hidden />
              ) : (
                <X className="h-4 w-4 shrink-0 text-red-500" aria-hidden />
              )}
              <span className="text-slate-600 dark:text-slate-300">
                {t('notify.inappTestLabel')}
                {testResult.inapp_ok ? t('notify.testPass') : t('notify.testFail')}
              </span>
            </p>
            <p className="flex items-start gap-1.5">
              {testResult.webhook_ok ? (
                <Check className="mt-0.5 h-4 w-4 shrink-0 text-emerald-500" aria-hidden />
              ) : (
                <X className="mt-0.5 h-4 w-4 shrink-0 text-red-500" aria-hidden />
              )}
              <span className="text-slate-600 dark:text-slate-300">
                {t('notify.webhookTestLabel')}
                {testResult.webhook_ok
                  ? t('notify.testPass')
                  : testResult.webhook_error
                    ? t('notify.testFailWithError', { error: testResult.webhook_error })
                    : t('notify.testFail')}
              </span>
            </p>
          </div>
        )}
        {testError && (
          <p className="flex items-start gap-1.5 text-sm text-red-600 dark:text-red-400">
            <AlertCircle className="mt-0.5 h-4 w-4 shrink-0" aria-hidden />
            {testError}
          </p>
        )}

        <div className="flex flex-wrap items-center justify-end gap-2">
          <p className="mr-auto hidden text-xs text-slate-500 sm:block dark:text-slate-400">
            {t('notify.testUsesSaved')}
          </p>
          <Button
            type="button"
            variant="secondary"
            loading={testMutation.isPending}
            onClick={() => testMutation.mutate()}
          >
            {t('notify.sendTest')}
          </Button>
          <Button type="submit" loading={mutation.isPending} disabled={events.size === 0}>
            {t('saveSettings')}
          </Button>
        </div>
      </form>
    </Card>
  )
}

/* ---------------------------------- 日志隐私卡片 ---------------------------------- */

/**
 * 日志隐私卡片：记录邮件正文开关（A4）。关闭后新落库日志不再保存正文快照
 * （元数据照常，只影响新日志、历史行原样保留）。单开关即时生效，切换即保存
 * （并入现有 PUT /api/settings 整体保存流：请求体 = 最新服务端设置 + 本字段覆盖）。
 */
function LogPrivacySettingsCard({ settings }: { settings: SystemSettingsWithNotify }) {
  const queryClient = useQueryClient()
  const { t } = useTranslation(['settings', 'common'])
  const [logBodyEnabled, setLogBodyEnabled] = useState(() => normalizeLogBodyEnabled(settings))

  const mutation = useMutation({
    mutationFn: (payload: SystemSettings) =>
      api<SystemSettings>('/api/settings', { method: 'PUT', body: payload }),
    onSuccess: (saved) => {
      toast.success(t('toast.saved'))
      // 用服务端最新值回同步（与轮换/通知卡片同款交互）
      setLogBodyEnabled(normalizeLogBodyEnabled(saved))
      queryClient.setQueryData(SETTINGS_QUERY_KEY, saved)
      void queryClient.invalidateQueries({ queryKey: SETTINGS_QUERY_KEY })
    },
    onError: (err) => toast.error(err.message),
  })

  function handleToggle(next: boolean) {
    const prev = logBodyEnabled
    setLogBodyEnabled(next)
    mutation.mutate({ ...settings, log_body_enabled: next }, {
      onError: () => setLogBodyEnabled(prev), // 保存失败回滚开关展示，避免与已存值不一致
    })
  }

  return (
    <Card>
      <CardHeader title={t('logPrivacy.title')} description={t('logPrivacy.description')} />
      <div className="space-y-4 px-5 py-4">
        <div className="flex items-center justify-between gap-4">
          <div className="min-w-0">
            <p className="text-sm font-medium text-slate-700 dark:text-slate-300">{t('logPrivacy.logBody')}</p>
            <p className="mt-0.5 text-xs text-slate-500 dark:text-slate-400">
              {t('logPrivacy.logBodyDesc')}
            </p>
          </div>
          <ToggleSwitch
            checked={logBodyEnabled}
            onChange={handleToggle}
            label={t('logPrivacy.logBodySwitch')}
          />
        </div>
        {logBodyEnabled ? (
          <p className="text-xs text-slate-500 dark:text-slate-400">
            {t('logPrivacy.bodyNoteOn')}
          </p>
        ) : (
          <p className="flex items-start gap-1.5 rounded-lg border border-amber-300 bg-amber-50 px-3 py-2.5 text-xs text-amber-800 dark:border-amber-400/30 dark:bg-amber-400/10 dark:text-amber-200">
            <AlertCircle className="mt-0.5 h-3.5 w-3.5 shrink-0" aria-hidden />
            {t('logPrivacy.bodyNoteOff')}
          </p>
        )}
      </div>
    </Card>
  )
}


/* ---------------------------------- 送达与抑制卡片（送达链路三件套） ---------------------------------- */

/**
 * 送达与抑制卡片：抑制列表自动入库开关（hard bounce 与 complained 事件自动
 * 拉黑，即时保存）+ 退信率告警阈值（0-100%，0=关闭告警；单输入框失焦保存）。
 * 告警事件 bounce_rate_exceeded 的开关在上方「失败通知」事件白名单中勾选。
 */
function DeliveryGuardCard({ settings }: { settings: SystemSettingsWithNotify }) {
  const queryClient = useQueryClient()
  const { t } = useTranslation(['settings', 'common'])
  const [autoEnabled, setAutoEnabled] = useState(() => normalizeSuppressionAutoEnabled(settings))
  const [threshold, setThreshold] = useState(() => String(normalizeBounceRateThreshold(settings)))
  const [thresholdError, setThresholdError] = useState<string | undefined>(undefined)

  const mutation = useMutation({
    mutationFn: (payload: SystemSettings) =>
      api<SystemSettings>('/api/settings', { method: 'PUT', body: payload }),
    onSuccess: (saved) => {
      toast.success(t('toast.saved'))
      setAutoEnabled(normalizeSuppressionAutoEnabled(saved))
      setThreshold(String(normalizeBounceRateThreshold(saved)))
      queryClient.setQueryData(SETTINGS_QUERY_KEY, saved)
      void queryClient.invalidateQueries({ queryKey: SETTINGS_QUERY_KEY })
    },
    onError: (err) => toast.error(err.message),
  })

  function handleToggle(next: boolean) {
    const prev = autoEnabled
    setAutoEnabled(next)
    mutation.mutate({ ...settings, suppression_auto_enabled: next }, {
      onError: () => setAutoEnabled(prev), // 保存失败回滚开关展示
    })
  }

  /** 阈值失焦保存：合法（0-100）才提交；非法回显旧值并提示 */
  function handleThresholdCommit() {
    const raw = threshold.trim()
    if (raw === '') {
      setThreshold(String(normalizeBounceRateThreshold(settings)))
      return
    }
    const n = Number(raw)
    if (!Number.isFinite(n) || n < 0 || n > 100) {
      setThresholdError(t('errors.bounceThreshold'))
      return
    }
    setThresholdError(undefined)
    mutation.mutate({ ...settings, bounce_rate_threshold: n })
  }

  return (
    <Card>
      <CardHeader
        title={t('delivery.title')}
        description={t('delivery.description')}
      />
      <div className="space-y-4 px-5 py-4">
        <div className="flex items-center justify-between gap-4">
          <div className="min-w-0">
            <p className="text-sm font-medium text-slate-700 dark:text-slate-300">{t('delivery.autoSuppress')}</p>
            <p className="mt-0.5 text-xs text-slate-500 dark:text-slate-400">
              {t('delivery.autoSuppressDesc')}
            </p>
          </div>
          <ToggleSwitch
            checked={autoEnabled}
            onChange={handleToggle}
            label={t('delivery.autoSuppressSwitch')}
          />
        </div>
        <div className="flex items-center justify-between gap-4">
          <div className="min-w-0">
            <p className="text-sm font-medium text-slate-700 dark:text-slate-300">{t('delivery.threshold')}</p>
            <p className="mt-0.5 text-xs text-slate-500 dark:text-slate-400">
              {t('delivery.thresholdDesc')}
            </p>
          </div>
          <div className="w-28 shrink-0">
            <Input
              aria-label={t('delivery.thresholdAria')}
              inputMode="decimal"
              value={threshold}
              onChange={(e) => {
                setThreshold(e.target.value)
                setThresholdError(undefined)
              }}
              onBlur={handleThresholdCommit}
              onKeyDown={(e) => {
                if (e.nativeEvent.isComposing || e.keyCode === 229) return
                if (e.key === 'Enter') {
                  e.currentTarget.blur()
                }
              }}
            />
            {thresholdError ? (
              <p role="alert" className="mt-1 text-xs text-red-600 dark:text-red-400">
                {thresholdError}
              </p>
            ) : null}
          </div>
        </div>
      </div>
    </Card>
  )
}

/* ---------------------------------- 数据管理卡片 ---------------------------------- */


/** 日志清理模式 → 词典 key（消费处 t() 渲染文案） */
const PURGE_MODE_LABELS = {
  before_days: 'purge.modeBeforeDays',
  channel: 'purge.modeChannel',
  all: 'purge.modeAll',
} as const

/**
 * 数据管理卡片（A2）：每日自动备份开关（即时保存）+ 立即备份 + 备份列表
 * （大小/时间/下载/删除，删除二次确认）+ 日志手动清理（danger 级：红色确认
 * 弹窗二次确认，pending 期间禁用按钮防重复提交）。
 */
function DataManagementCard({ settings }: { settings: SystemSettingsWithWeakPassword }) {
  const queryClient = useQueryClient()
  const { t } = useTranslation(['settings', 'common'])
  const backupsQuery = useQuery({ queryKey: BACKUPS_QUERY_KEY, queryFn: listBackups })
  const channelsQuery = useChannels()

  /* 每日自动备份开关：切换即保存（请求体 = 最新服务端设置 + 本字段覆盖），失败回滚 */
  const [dailyEnabled, setDailyEnabled] = useState(() => normalizeBackupDailyEnabled(settings))
  const settingsMutation = useMutation({
    mutationFn: (payload: SystemSettingsWithBackup) =>
      api<SystemSettings>('/api/settings', { method: 'PUT', body: payload }),
    onSuccess: (saved) => {
      toast.success(t('toast.saved'))
      setDailyEnabled(normalizeBackupDailyEnabled(saved))
      queryClient.setQueryData(SETTINGS_QUERY_KEY, saved)
      void queryClient.invalidateQueries({ queryKey: SETTINGS_QUERY_KEY })
    },
    onError: (err) => toast.error(err.message),
  })

  function handleDailyToggle(next: boolean) {
    const prev = dailyEnabled
    setDailyEnabled(next)
    settingsMutation.mutate(
      { ...settings, backup_daily_enabled: next },
      { onError: () => setDailyEnabled(prev) },
    )
  }

  /* 立即备份 */
  const createMutation = useMutation({
    mutationFn: createBackup,
    onSuccess: (b) => {
      toast.success(t('toast.backupCreated', { name: b.name, size: formatBytes(b.size) }))
      void queryClient.invalidateQueries({ queryKey: BACKUPS_QUERY_KEY })
    },
    onError: (err) => toast.error(err.message),
  })

  /* 删除备份（二次确认） */
  const [deleteTarget, setDeleteTarget] = useState<BackupItem | null>(null)
  const deleteMutation = useMutation({
    mutationFn: (name: string) => deleteBackup(name),
    onSuccess: () => {
      toast.success(t('toast.backupDeleted'))
      setDeleteTarget(null)
      void queryClient.invalidateQueries({ queryKey: BACKUPS_QUERY_KEY })
    },
    onError: (err) => toast.error(err.message),
  })

  /* 日志清理（danger 级） */
  const [purgeMode, setPurgeMode] = useState<LogPurgeMode>('before_days')
  const [purgeDays, setPurgeDays] = useState('30')
  const [purgeChannelId, setPurgeChannelId] = useState('')
  const [purgeConfirmOpen, setPurgeConfirmOpen] = useState(false)
  const purgeMutation = useMutation({
    mutationFn: purgeLogs,
    onSuccess: (res) => {
      toast.success(t('toast.logsPurged', { count: res.deleted }))
      setPurgeConfirmOpen(false)
      void queryClient.invalidateQueries({ queryKey: ['logs'] })
    },
    onError: (err) => toast.error(err.message),
  })

  /** 打开确认弹窗前先做参数校验（与后端 1-3650 同口径） */
  function openPurgeConfirm() {
    if (purgeMode === 'before_days') {
      const days = toInt(purgeDays)
      if (days === undefined || days < 1 || days > 3650) {
        toast.error(t('toast.purgeDaysInvalid'))
        return
      }
    }
    if (purgeMode === 'channel' && (toInt(purgeChannelId) ?? 0) <= 0) {
      toast.error(t('toast.purgeChannelRequired'))
      return
    }
    setPurgeConfirmOpen(true)
  }

  function submitPurge() {
    if (purgeMutation.isPending) return // 防重复提交
    const req: LogPurgeRequest = { mode: purgeMode }
    if (purgeMode === 'before_days') req.days = toInt(purgeDays)
    if (purgeMode === 'channel') req.channel_id = toInt(purgeChannelId)
    purgeMutation.mutate(req)
  }

  /** 确认弹窗里按模式生成的操作描述 */
  function purgeDescription(): string {
    switch (purgeMode) {
      case 'before_days': {
        const days = toInt(purgeDays) ?? 0
        return t('purge.descBeforeDays', { count: days })
      }
      case 'channel': {
        const id = toInt(purgeChannelId) ?? 0
        const ch = (channelsQuery.data?.items ?? []).find((c: Channel) => c.id === id)
        return t('purge.descChannel', { name: ch?.name ?? `#${id}` })
      }
      case 'all':
        return t('purge.descAll')
    }
  }

  const backups = backupsQuery.data?.items ?? []

  return (
    <Card>
      <CardHeader
        title={t('data.title')}
        description={t('data.description')}
      />
      <div className="space-y-6 px-5 py-4">
        {/* 每日自动备份开关 */}
        <div className="flex items-center justify-between gap-4">
          <div className="min-w-0">
            <p className="text-sm font-medium text-slate-700 dark:text-slate-300">{t('data.dailyBackup')}</p>
            <p className="mt-0.5 text-xs text-slate-500 dark:text-slate-400">
              {t('data.dailyBackupDesc')}
            </p>
          </div>
          <ToggleSwitch
            checked={dailyEnabled}
            onChange={handleDailyToggle}
            label={t('data.dailyBackupSwitch')}
          />
        </div>

        {/* 立即备份 + 备份列表 */}
        <div className="space-y-3">
          <div className="flex items-center justify-between gap-4">
            <p className="text-sm font-medium text-slate-700 dark:text-slate-300">{t('data.backupFiles')}</p>
            <Button
              variant="secondary"
              loading={createMutation.isPending}
              onClick={() => createMutation.mutate()}
            >
              {t('data.backupNow')}
            </Button>
          </div>
          {backupsQuery.isPending ? (
            <p className="px-1 text-xs text-slate-500 dark:text-slate-400">{t('common:state.loading')}</p>
          ) : backupsQuery.isError ? (
            <p className="flex items-start gap-1.5 text-sm text-red-600 dark:text-red-400">
              <AlertCircle className="mt-0.5 h-4 w-4 shrink-0" aria-hidden />
              {t('data.loadFailed', { error: backupsQuery.error.message })}
            </p>
          ) : backups.length === 0 ? (
            <EmptyState
              icon={DatabaseBackup}
              title={t('data.emptyTitle')}
              description={t('data.emptyDescription')}
              className="py-8"
            />
          ) : (
            <ul className="divide-y divide-slate-100 overflow-hidden rounded-xl border border-border-subtle dark:divide-slate-800">
              {backups.map((b) => (
                <li key={b.name} className="flex flex-wrap items-center gap-x-3 gap-y-1 px-3.5 py-2.5">
                  <div className="min-w-0 flex-1">
                    <p className="truncate font-mono text-xs text-slate-700 dark:text-slate-300">
                      {b.name}
                    </p>
                    <p className="mt-0.5 text-xs text-slate-500 dark:text-slate-400">
                      {formatBytes(b.size)} · {formatDateTime(b.created_at, true)}
                    </p>
                  </div>
                  <a
                    href={backupDownloadURL(b.name)}
                    className="inline-flex items-center gap-1.5 rounded-lg px-2.5 py-1.5 text-xs font-medium text-sky-600 transition-colors hover:bg-sky-50 hover:text-sky-700 dark:text-sky-400 dark:hover:bg-sky-900/40 dark:hover:text-sky-300"
                  >
                    <Download className="h-3.5 w-3.5" aria-hidden />
                    {t('data.download')}
                  </a>
                  <button
                    type="button"
                    onClick={() => setDeleteTarget(b)}
                    className="inline-flex items-center gap-1.5 rounded-lg px-2.5 py-1.5 text-xs font-medium text-slate-500 transition-colors hover:bg-red-50 hover:text-red-600 dark:text-slate-400 dark:hover:bg-red-900/30 dark:hover:text-red-400"
                  >
                    <Trash2 className="h-3.5 w-3.5" aria-hidden />
                    {t('common:actions.delete')}
                  </button>
                </li>
              ))}
            </ul>
          )}
        </div>

        {/* 日志清理 */}
        <div className="space-y-3 border-t border-slate-200 pt-5 dark:border-slate-800">
          <div>
            <p className="text-sm font-medium text-slate-700 dark:text-slate-300">{t('purge.title')}</p>
            <p className="mt-0.5 text-xs text-slate-500 dark:text-slate-400">
              {t('purge.description')}
            </p>
          </div>
          <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
            <Field label={t('purge.modeLabel')}>
              <Select
                aria-label={t('purge.modeAria')}
                value={purgeMode}
                onChange={(e) => setPurgeMode(e.target.value as LogPurgeMode)}
              >
                {(Object.keys(PURGE_MODE_LABELS) as LogPurgeMode[]).map((mode) => (
                  <option key={mode} value={mode}>
                    {t(PURGE_MODE_LABELS[mode])}
                  </option>
                ))}
              </Select>
            </Field>
            {purgeMode === 'before_days' && (
              <Field label={t('purge.daysLabel')} hint={t('purge.daysHint')}>
                <Input
                  type="number"
                  min={1}
                  max={3650}
                  value={purgeDays}
                  onChange={(e) => setPurgeDays(e.target.value)}
                />
              </Field>
            )}
            {purgeMode === 'channel' && (
              <Field label={t('purge.channelLabel')}>
                <Select
                  aria-label={t('purge.channelAria')}
                  value={purgeChannelId}
                  onChange={(e) => setPurgeChannelId(e.target.value)}
                >
                  <option value="">{t('purge.channelPlaceholder')}</option>
                  {(channelsQuery.data?.items ?? []).map((ch) => (
                    <option key={ch.id} value={ch.id}>
                      {ch.name}
                    </option>
                  ))}
                </Select>
              </Field>
            )}
          </div>
          <div className="flex justify-end">
            <Button
              variant="danger"
              onClick={openPurgeConfirm}
              disabled={purgeMutation.isPending}
            >
              {t('purge.button')}
            </Button>
          </div>
        </div>
      </div>

      {/* 删除备份二次确认 */}
      <Modal
        open={deleteTarget !== null}
        onClose={() => setDeleteTarget(null)}
        title={t('backup.deleteTitle')}
        footer={
          <>
            <Button variant="secondary" onClick={() => setDeleteTarget(null)}>
              {t('common:actions.cancel')}
            </Button>
            <Button
              variant="danger"
              loading={deleteMutation.isPending}
              onClick={() => deleteTarget && deleteMutation.mutate(deleteTarget.name)}
            >
              {t('backup.deleteConfirm')}
            </Button>
          </>
        }
      >
        <p className="text-sm text-slate-600 dark:text-slate-300">
          {t('backup.deleteBodyPrefix')} <span className="font-mono">{deleteTarget?.name}</span>
          {t('backup.deleteBodySuffix', { size: formatBytes(deleteTarget?.size ?? 0) })}
        </p>
      </Modal>

      {/* 日志清理红色确认弹窗（danger 级二次确认） */}
      <Modal
        open={purgeConfirmOpen}
        onClose={() => setPurgeConfirmOpen(false)}
        title={t('purge.confirmTitle')}
        footer={
          <>
            <Button variant="secondary" onClick={() => setPurgeConfirmOpen(false)}>
              {t('common:actions.cancel')}
            </Button>
            <Button variant="danger" loading={purgeMutation.isPending} onClick={submitPurge}>
              {t('purge.confirmButton')}
            </Button>
          </>
        }
      >
        <div className="flex items-start gap-2.5 rounded-xl border border-red-300 bg-red-50 px-4 py-3 dark:border-red-400/30 dark:bg-red-400/10">
          <AlertTriangle className="mt-0.5 h-4 w-4 shrink-0 text-red-500" aria-hidden />
          <div className="min-w-0 text-sm">
            <p className="font-medium text-red-800 dark:text-red-200">
              {purgeDescription()}{t('purge.irreversible')}
            </p>
            <p className="mt-1 text-xs text-red-600/80 dark:text-red-300/80">
              {t('purge.suggestBackup')}
            </p>
          </div>
        </div>
      </Modal>
    </Card>
  )
}

/* ---------------------------------- 系统更新卡片 ---------------------------------- */

/**
 * 系统更新卡片（E8）：当前版本展示 + 检查更新（结果内联：已是最新 / 发现新版本
 * + notes 折叠 + release 链接）+ 一键更新（下载校验后原子替换，重启生效）。
 * 容器部署（in_docker）隐藏一键更新并改示拉取镜像提示；未配置更新源时整卡降级
 * 为 400 提示。数据：GET /api/update/check（后端缓存 1h）、POST /api/update/apply。
 */
function SystemUpdateCard() {
  const checkQuery = useQuery({ queryKey: UPDATE_CHECK_QUERY_KEY, queryFn: checkUpdate })
  const data = checkQuery.data
  const { t } = useTranslation(['settings', 'common'])
  const [applyDone, setApplyDone] = useState<UpdateApplyResult | null>(null)

  const applyMutation = useMutation({
    mutationFn: applyUpdate,
    onSuccess: (res) => {
      setApplyDone(res)
      toast.success(t('update.installed', { version: res.installed_version, hint: res.hint }))
    },
    onError: (err) => toast.error(err.message),
  })

  return (
    <Card>
      <CardHeader
        title={t('update.title')}
        description={t('update.description')}
      />
      <div className="space-y-4 px-5 py-4">
        {checkQuery.isPending ? (
          <p className="px-1 text-xs text-slate-500 dark:text-slate-400">{t('common:state.loading')}</p>
        ) : checkQuery.isError ? (
          <p className="flex items-start gap-1.5 text-sm text-red-600 dark:text-red-400">
            <AlertCircle className="mt-0.5 h-4 w-4 shrink-0" aria-hidden />
            {checkQuery.error.message}
          </p>
        ) : data ? (
          <>
            {/* 当前版本 + 检查更新 */}
            <div className="flex items-center justify-between gap-4">
              <div className="min-w-0">
                <p className="text-sm font-medium text-slate-700 dark:text-slate-300">
                  {t('update.currentVersion')}
                  <span className="ml-2 font-mono text-slate-500 dark:text-slate-400">{data.current}</span>
                </p>
                {data.checked_at && (
                  <p className="mt-0.5 text-xs text-slate-500 dark:text-slate-400">
                    {t('update.checkedAt', { time: formatDateTime(data.checked_at, true) })}
                  </p>
                )}
              </div>
              <Button
                variant="secondary"
                loading={checkQuery.isFetching}
                onClick={() => void checkQuery.refetch()}
              >
                <RefreshCw className="h-4 w-4" aria-hidden />
                {t('update.check')}
              </Button>
            </div>

            {/* 检查结果内联展示 */}
            {data.error ? (
              <p className="flex items-start gap-1.5 text-sm text-red-600 dark:text-red-400">
                <AlertCircle className="mt-0.5 h-4 w-4 shrink-0" aria-hidden />
                {t('update.checkFailed', { error: data.error })}
              </p>
            ) : data.update_available && data.latest ? (
              <div className="space-y-2 rounded-lg border border-border-subtle bg-slate-50 px-3 py-2.5 text-sm dark:bg-slate-900/40">
                <p className="font-medium text-slate-700 dark:text-slate-300">
                  {t('update.newVersion')} <span className="font-mono">{data.latest}</span>
                  {data.published_at && (
                    <span className="ml-2 text-xs font-normal text-slate-500 dark:text-slate-400">
                      {t('update.publishedAt', { time: formatDateTime(data.published_at, true) })}
                    </span>
                  )}
                </p>
                {data.notes && (
                  <details className="text-xs text-slate-500 dark:text-slate-400">
                    <summary className="cursor-pointer select-none text-sky-600 hover:underline dark:text-sky-400">
                      {t('update.notesSummary')}
                    </summary>
                    <pre className="mt-1.5 max-h-48 overflow-auto whitespace-pre-wrap font-sans">{data.notes}</pre>
                  </details>
                )}
                {data.release_url && (
                  <a
                    href={data.release_url}
                    target="_blank"
                    rel="noreferrer"
                    className="inline-flex items-center gap-1 text-xs font-medium text-sky-600 hover:underline dark:text-sky-400"
                  >
                    <ExternalLink className="h-3.5 w-3.5" aria-hidden />
                    {t('update.viewOnGitHub')}
                  </a>
                )}
              </div>
            ) : (
              !data.error && data.latest && (
                <p className="flex items-center gap-1.5 text-sm text-slate-600 dark:text-slate-300">
                  <Check className="h-4 w-4 shrink-0 text-emerald-500" aria-hidden />
                  {t('update.upToDate', { version: data.latest })}
                </p>
              )
            )}

            {/* 一键更新 / 容器提示 / 成功提示 */}
            {data.in_docker ? (
              data.update_available && (
                <p className="flex items-start gap-1.5 rounded-lg border border-sky-300 bg-sky-50 px-3 py-2.5 text-xs text-sky-800 dark:border-sky-400/30 dark:bg-sky-400/10 dark:text-sky-200">
                  <Rocket className="mt-0.5 h-3.5 w-3.5 shrink-0" aria-hidden />
                  {t('update.dockerHint')}
                </p>
              )
            ) : (
              applyDone ? (
                <p className="flex items-start gap-1.5 rounded-lg border border-emerald-300 bg-emerald-50 px-3 py-2.5 text-sm text-emerald-800 dark:border-emerald-400/30 dark:bg-emerald-400/10 dark:text-emerald-200">
                  <Check className="mt-0.5 h-4 w-4 shrink-0" aria-hidden />
                  {t('update.installed', { version: applyDone.installed_version, hint: applyDone.hint })}
                </p>
              ) : (
                data.update_available && (
                  <div className="flex justify-end">
                    <Button loading={applyMutation.isPending} onClick={() => applyMutation.mutate()}>
                      {t('update.applyTo', { version: data.latest ?? '' })}
                    </Button>
                  </div>
                )
              )
            )}
          </>
        ) : null}
      </div>
    </Card>
  )
}

/* ---------------------------------- 修改密码卡片 ---------------------------------- */

function ChangePasswordCard() {
  const { t } = useTranslation(['settings', 'common'])
  const [oldPassword, setOldPassword] = useState('')
  const [newPassword, setNewPassword] = useState('')
  const [confirm, setConfirm] = useState('')
  const [errors, setErrors] = useState<{ old?: string; next?: string; confirm?: string }>({})
  const [serverError, setServerError] = useState<string | null>(null)

  const mutation = useMutation({
    mutationFn: (req: { old_password: string; new_password: string }) =>
      api<unknown>('/api/auth/password', { method: 'PUT', body: req }),
    onSuccess: () => {
      // 后端行为：自己改密 → 当前会话重签 cookie 保持登录，同账号其他会话全部失效
      toast.success(t('toast.passwordChanged'))
      setOldPassword('')
      setNewPassword('')
      setConfirm('')
      setServerError(null)
    },
    onError: (err) => {
      setServerError(err.message)
      toast.error(err.message)
    },
  })

  function handleSubmit(e: FormEvent<HTMLFormElement>) {
    e.preventDefault()
    const nextErrors: typeof errors = {}
    if (!oldPassword) nextErrors.old = t('password.oldRequired')
    // 上限按 UTF-8 字节计（对齐后端 validatePassword / bcrypt 72 字节输入上限）
    if (newPassword.length < 6) nextErrors.next = t('password.tooShort')
    else if (utf8ByteLength(newPassword) > 72) nextErrors.next = t('password.tooLong')
    if (confirm !== newPassword) nextErrors.confirm = t('password.mismatch')
    setErrors(nextErrors)
    if (Object.values(nextErrors).some(Boolean)) return
    mutation.mutate({ old_password: oldPassword, new_password: newPassword })
  }

  return (
    <Card>
      <CardHeader
        title={t('password.title')}
        description={t('password.description')}
      />
      <form aria-label={t('password.title')} onSubmit={handleSubmit} className="space-y-4 px-5 py-4" noValidate>
        {serverError && (
          <p className="flex items-start gap-1.5 text-sm text-red-600 dark:text-red-400">
            <AlertCircle className="mt-0.5 h-4 w-4 shrink-0" aria-hidden />
            {serverError}
          </p>
        )}
        <Field label={t('password.old')} error={errors.old}>
          <Input
            name="old_password"
            type="password"
            value={oldPassword}
            onChange={(e) => {
              setOldPassword(e.target.value)
              setErrors((prev) => ({ ...prev, old: undefined }))
              setServerError(null)
            }}
            placeholder={t('currentPassword')}
            autoComplete="current-password"
          />
        </Field>
        <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
          <Field label={t('password.next')} error={errors.next} hint={t('password.minLengthHint')}>
            <Input
              name="new_password"
              type="password"
              value={newPassword}
              onChange={(e) => {
                setNewPassword(e.target.value)
                setErrors((prev) => ({ ...prev, next: undefined }))
                setServerError(null)
              }}
              placeholder={t('password.next')}
              autoComplete="new-password"
            />
          </Field>
          <Field label={t('password.confirm')} error={errors.confirm}>
            <Input
              name="confirm_password"
              type="password"
              value={confirm}
              onChange={(e) => {
                setConfirm(e.target.value)
                setErrors((prev) => ({ ...prev, confirm: undefined }))
                setServerError(null)
              }}
              placeholder={t('password.confirmPlaceholder')}
              autoComplete="new-password"
            />
          </Field>
        </div>
        <div className="flex justify-end">
          <Button type="submit" loading={mutation.isPending}>
            {t('password.title')}
          </Button>
        </div>
      </form>
    </Card>
  )
}

/* ---------------------------------- 页面 ---------------------------------- */

export function SettingsPage() {
  const { t } = useTranslation(['settings', 'common'])
  const settingsQuery = useSettings()
  // 后端在既有设置对象上新增了失败通知四字段与 weak_admin_password（GET/PUT 同一体）；
  // 类型本地收窄一次供警告条与两张表单卡片共用，不动 lib/settings.ts / lib/api.ts
  const settings = settingsQuery.data as SystemSettingsWithWeakPassword | undefined
  const weakPassword = settings?.weak_admin_password === true

  return (
    <div className="space-y-6">
      <PageHeader title={t('page.title')} description={t('page.description')} />

      {weakPassword && <WeakPasswordBanner />}

      <AppearanceCard />

      {settingsQuery.isPending ? (
        <Card>
          <QueryLoading />
        </Card>
      ) : settingsQuery.isError ? (
        <Card>
          <QueryError message={settingsQuery.error.message} onRetry={() => settingsQuery.refetch()} />
        </Card>
      ) : (
        settings && (
          <>
            <RotationSettingsCard settings={settings} />
            <NotifySettingsCard settings={settings} />
            <LogPrivacySettingsCard settings={settings} />
            <DeliveryGuardCard settings={settings} />
            <DataManagementCard settings={settings} />
            <SystemUpdateCard />
          </>
        )
      )}

      {/* 两步验证（C4）与活跃会话（安全加固三件套，C）：账号安全卡片，与修改密码相邻 */}
      <TotpCard />
      <SessionsCard />

      {/* 弱口令警告条的锚点目标（scroll-mt 对齐 main 滚动容器的 p-6） */}
      <div id="change-password" className="scroll-mt-6">
        <ChangePasswordCard />
      </div>
    </div>
  )
}
