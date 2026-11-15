/**
 * 渠道管理（SPEC §6）：渠道表格（平台/状态/关键词筛选，同步 URL query，附 7 日成功率）
 * + 详情停靠面板（近 7 日健康度：成功率/最近成功失败/Top 失败原因）
 * + 新建/编辑表单（Modal 复用）+ 测试发送（成功扣减日/月额度，429=额度用尽）+ 启停 + 删除。
 * 数据：GET/POST /api/channels、PUT/DELETE /api/channels/:id、
 *       POST /api/channels/:id/test、POST /api/channels/:id/status（SPEC §5 原样字段名）。
 */
import { useMutation, useQueryClient } from '@tanstack/react-query'
import type { TFunction } from 'i18next'
import { Mailbox, Plus, SearchX, X } from 'lucide-react'
import {
  useCallback,
  useEffect,
  useMemo,
  useRef,
  useState,
  type FormEvent,
  type ReactNode,
} from 'react'
import { useTranslation } from 'react-i18next'
import { useSearchParams } from 'react-router'
import { toast } from 'sonner'

import { PageHeader } from '@/components/PageHeader'
import { QueryError, QueryLoading } from '@/components/QueryState'
import { RelativeTime } from '@/components/RelativeTime'
import { Badge, ChannelStatusBadge, ProviderBadge } from '@/components/badges'
import { Button } from '@/components/ui/Button'
import { Card } from '@/components/ui/Card'
import { EmptyState } from '@/components/ui/EmptyState'
import { Field, Input, Select, Textarea } from '@/components/ui/Input'
import { Modal } from '@/components/ui/Modal'
import { TD, TH, TBody, THead, TRow, Table } from '@/components/ui/Table'
import { api } from '@/lib/api'
import {
  PROVIDER_DEFAULTS,
  PROVIDER_LABELS,
  PROVIDER_OPTIONS,
  duplicateChannel,
  invalidateChannelData,
  providerNeedsApiKey,
  rotateChannelWebhookToken,
  useChannels,
  type Channel,
  type ChannelHealth,
  type ChannelStatusAction,
  type ProviderOption,
  type TestSendResponse,
} from '@/lib/channels'
import { copyText } from '@/lib/clipboard'
import { useNativeDialog } from '@/lib/useNativeDialog'
import { useDebouncedValue } from '@/lib/logs'
import { extractTemplateVars, parseSampleVars, useTemplates } from '@/lib/templates'
import { cn, toInt } from '@/lib/utils'

/* ---------------------------------- 类型 ---------------------------------- */

/** 表单值（数字字段以字符串承载，提交时解析） */
interface ChannelFormValues {
  name: string
  provider: ProviderOption
  priority: string
  api_key: string
  api_secret: string
  domain: string
  region: 'us' | 'eu'
  from_email: string
  from_name: string
  daily_limit_override: string
  monthly_limit_override: string
  rate_limit_per_sec: string
  proxy_url: string
  allowed_domains: string
  smtp_host: string
  smtp_port: string
  smtp_user: string
  smtp_pass: string
  smtp_tls: 'none' | 'starttls' | 'tls'
  webhook_secret: string
}

/** POST/POST /api/channels、PUT /api/channels/:id 请求体；密钥类字段按需省略 */
interface ChannelPayload {
  name: string
  provider: string
  priority: number
  daily_limit_override: number
  monthly_limit_override: number
  rate_limit_per_sec: number
  from_email: string
  from_name: string
  allowed_domains: string
  api_key?: string
  api_secret?: string
  domain?: string
  region?: string
  proxy_url?: string
  smtp_host?: string
  smtp_port?: number
  smtp_user?: string
  smtp_pass?: string
  smtp_tls?: string
  /** 平台签名密钥（送达链路三件套：Resend whsec_/Mailgun signing key）；传空 = 保留原值 */
  webhook_secret?: string
  /** 编辑时要求后端清空的密钥字段（合法值：api_key/api_secret/proxy_url/smtp_pass/webhook_secret） */
  clear_secrets?: string[]
}

/** 编辑弹窗里可「清除」的密钥字段（PUT 时写入 clear_secrets） */
type ClearableSecretKey = 'api_secret' | 'proxy_url' | 'smtp_pass' | 'webhook_secret'

/** clear_secrets 合法值与表单字段一一对应 */
const CLEARABLE_SECRET_KEYS: readonly ClearableSecretKey[] = [
  'api_secret',
  'proxy_url',
  'smtp_pass',
  'webhook_secret',
]

type ClearMarks = Partial<Record<ClearableSecretKey, boolean>>

/** SMTP TLS 模式选项（协议名原样保留，说明文案在词典 form.tlsNone/tlsStarttls/tlsTls） */
const SMTP_TLS_OPTIONS: Array<{
  value: ChannelFormValues['smtp_tls']
  labelKey: 'form.tlsNone' | 'form.tlsStarttls' | 'form.tlsTls'
}> = [
  { value: 'none', labelKey: 'form.tlsNone' },
  { value: 'starttls', labelKey: 'form.tlsStarttls' },
  { value: 'tls', labelKey: 'form.tlsTls' },
]

const EMAIL_RE = /^[^\s@]+@[^\s@]+\.[^\s@]+$/

type FormErrors = Partial<Record<keyof ChannelFormValues, string>>

/* ---------------------------------- 工具 ---------------------------------- */

function createEmptyValues(): ChannelFormValues {
  return {
    name: '',
    provider: 'resend',
    priority: '0',
    api_key: '',
    api_secret: '',
    domain: '',
    region: 'us',
    from_email: '',
    from_name: '',
    daily_limit_override: '',
    monthly_limit_override: '',
    rate_limit_per_sec: '',
    proxy_url: '',
    webhook_secret: '',
    allowed_domains: '',
    smtp_host: '',
    smtp_port: '',
    smtp_user: '',
    smtp_pass: '',
    smtp_tls: 'starttls',
  }
}

/** 编辑态：api_key/api_secret/proxy_url 留空（掩码作占位），传空 = 保留原值 */
function valuesFromChannel(ch: Channel): ChannelFormValues {
  const provider = (PROVIDER_OPTIONS as readonly string[]).includes(ch.provider)
    ? (ch.provider as ProviderOption)
    : 'resend'
  const tls = (['none', 'starttls', 'tls'] as const).includes(ch.smtp_tls as 'none')
    ? (ch.smtp_tls as ChannelFormValues['smtp_tls'])
    : 'starttls'
  return {
    name: ch.name,
    provider,
    priority: String(ch.priority ?? 0),
    api_key: '',
    api_secret: '',
    domain: ch.domain ?? '',
    region: ch.region === 'eu' ? 'eu' : 'us',
    from_email: ch.from_email ?? '',
    from_name: ch.from_name ?? '',
    daily_limit_override: ch.daily_limit_override ? String(ch.daily_limit_override) : '',
    monthly_limit_override: ch.monthly_limit_override ? String(ch.monthly_limit_override) : '',
    rate_limit_per_sec: ch.rate_limit_per_sec ? String(ch.rate_limit_per_sec) : '',
    proxy_url: '',
    webhook_secret: '',
    allowed_domains: ch.allowed_domains ?? '',
    smtp_host: ch.smtp_host ?? '',
    smtp_port: ch.smtp_port ? String(ch.smtp_port) : '',
    smtp_user: ch.smtp_user ?? '',
    smtp_pass: '',
    smtp_tls: tls,
  }
}

/**
 * 表单校验。requireCredentials = 新建，或编辑时切换了平台：
 * 此时按新建口径校验新平台必填凭据，错误内联提示；
 * 编辑且未切平台时为 false，密钥留空走「传空 = 保留原值」（不修改）。
 */
function validateValues(
  v: ChannelFormValues,
  requireCredentials: boolean,
  t: TFunction<readonly ['channels', 'common']>,
): FormErrors {
  const errors: FormErrors = {}
  if (!v.name.trim()) errors.name = t('validation.name')
  if (providerNeedsApiKey(v.provider) && requireCredentials && !v.api_key.trim()) {
    errors.api_key = t('validation.apiKey')
  }
  if (v.provider === 'mailjet' && requireCredentials && !v.api_secret.trim()) {
    errors.api_secret = t('validation.apiSecret')
  }
  if (v.provider === 'mailgun' && !v.domain.trim()) {
    errors.domain = t('validation.domain')
  }
  if (v.provider === 'smtp') {
    if (!v.smtp_host.trim()) errors.smtp_host = t('validation.smtpHost')
    const port = toInt(v.smtp_port)
    if (port === undefined) errors.smtp_port = t('validation.smtpPort')
    else if (port < 1 || port > 65535) errors.smtp_port = t('validation.portRange')
    if (!v.from_email.trim()) errors.from_email = t('validation.smtpFromEmail')
  }
  // 发件邮箱：填写时校验格式（请求未提供 from 时作为渠道默认发件人）
  const fromEmail = v.from_email.trim()
  if (fromEmail && !EMAIL_RE.test(fromEmail)) {
    errors.from_email = errors.from_email ?? t('validation.fromEmail')
  }
  const priority = toInt(v.priority)
  if (priority === undefined || priority < 0) {
    errors.priority = t('validation.nonNegativeInt')
  }
  const dailyOverride = toInt(v.daily_limit_override)
  if (v.daily_limit_override.trim() && (dailyOverride === undefined || dailyOverride < 0)) {
    errors.daily_limit_override = t('validation.nonNegativeInt')
  }
  const monthlyOverride = toInt(v.monthly_limit_override)
  if (v.monthly_limit_override.trim() && (monthlyOverride === undefined || monthlyOverride < 0)) {
    errors.monthly_limit_override = t('validation.nonNegativeInt')
  }
  const rateLimit = toInt(v.rate_limit_per_sec)
  if (v.rate_limit_per_sec.trim() && (rateLimit === undefined || rateLimit < 0)) {
    errors.rate_limit_per_sec = t('validation.nonNegativeInt')
  }
  return errors
}

function buildPayload(v: ChannelFormValues, isEdit: boolean, clearMarks: ClearMarks): ChannelPayload {
  const payload: ChannelPayload = {
    name: v.name.trim(),
    provider: v.provider,
    priority: toInt(v.priority) ?? 0,
    from_email: v.from_email.trim(),
    from_name: v.from_name.trim(),
    daily_limit_override: toInt(v.daily_limit_override) ?? 0,
    monthly_limit_override: toInt(v.monthly_limit_override) ?? 0,
    rate_limit_per_sec: toInt(v.rate_limit_per_sec) ?? 0,
    allowed_domains: v.allowed_domains.trim(),
  }
  // 密钥类字段：后端 PUT 语义为「传空 = 保留原值」，编辑时留空则整个字段不传
  // （mock 的 api_key 为失败模拟魔法值，非敏感，保存规则与真实平台一致）
  if ((providerNeedsApiKey(v.provider) || v.provider === 'mock') && (!isEdit || v.api_key.trim())) {
    payload.api_key = v.api_key.trim()
  }
  if (!isEdit || v.proxy_url.trim()) {
    payload.proxy_url = v.proxy_url.trim()
  }
  // 平台签名密钥（送达链路三件套）：与秘钥类同口径，留空 = 保留原值
  if (!isEdit || v.webhook_secret.trim()) {
    payload.webhook_secret = v.webhook_secret.trim()
  }
  if (v.provider === 'mailjet' && (!isEdit || v.api_secret.trim())) {
    payload.api_secret = v.api_secret.trim()
  }
  if (v.provider === 'mailgun') {
    payload.domain = v.domain.trim()
    payload.region = v.region
  }
  if (v.provider === 'smtp') {
    payload.smtp_host = v.smtp_host.trim()
    payload.smtp_port = toInt(v.smtp_port) ?? 0
    payload.smtp_user = v.smtp_user.trim()
    payload.smtp_tls = v.smtp_tls
    if (!isEdit || v.smtp_pass.trim()) payload.smtp_pass = v.smtp_pass.trim()
  }
  // 标记了「清除」的密钥字段：仅编辑时提交 clear_secrets（覆盖「留空 = 保留」语义）
  if (isEdit) {
    const marked = CLEARABLE_SECRET_KEYS.filter((key) => clearMarks[key])
    if (marked.length > 0) payload.clear_secrets = marked
  }
  return payload
}

/** 今日/本月用量文本：37/100、12/3000 或 ∞ */
function usageText(used: number, limit: number): string {
  return `${used}/${!limit || limit <= 0 ? '∞' : limit}`
}

/** 7 日成功率色调（≥95 绿 / 80-95 琥珀 / <80 红；色系与 badges.tsx 徽章一致） */
function successRateTone(rate: number): { bar: string; text: string } {
  if (rate >= 95) {
    return {
      bar: 'bg-emerald-500 dark:bg-emerald-400',
      text: 'text-emerald-700 dark:text-emerald-300',
    }
  }
  if (rate >= 80) {
    return {
      bar: 'bg-amber-500 dark:bg-amber-400',
      text: 'text-amber-700 dark:text-amber-300',
    }
  }
  return {
    bar: 'bg-red-500 dark:bg-red-400',
    text: 'text-red-700 dark:text-red-300',
  }
}

/** 「7 日成功率」单元格：mini 进度条 + 百分比；无数据（health=null）灰色「—」 */
function SuccessRateCell({ health }: { health: ChannelHealth | null | undefined }) {
  const { t } = useTranslation(['channels', 'common'])
  if (!health || health.total === 0) {
    return <span className="text-xs text-slate-500 dark:text-slate-400">—</span>
  }
  const tone = successRateTone(health.success_rate)
  return (
    <div className="flex items-center gap-2">
      <div
        className="h-1.5 w-14 shrink-0 overflow-hidden rounded-full bg-slate-100 dark:bg-slate-800"
        role="img"
        aria-label={t('table.successRateAria', { rate: health.success_rate })}
      >
        <div
          className={cn('h-full rounded-full', tone.bar)}
          style={{ width: `${health.success_rate}%` }}
        />
      </div>
      <span className={cn('text-xs font-medium tabular-nums', tone.text)}>
        {health.success_rate}%
      </span>
    </div>
  )
}

/** 退信率色调（超 5% 红 / 超 2% 琥珀 / 其余绿；与设置页默认告警阈值 5% 口径呼应） */
function bounceRateTone(rate: number): string {
  if (rate > 5) return 'text-red-700 dark:text-red-300'
  if (rate > 2) return 'text-amber-700 dark:text-amber-300'
  return 'text-emerald-700 dark:text-emerald-300'
}

/**
 * 送达侧徽标（送达链路三件套）：送达率（success 徽章）与退信率（按阈值着色的
 * 徽章），投诉率仅在 >0 时出现。health 为 null 或无终态事件数据（rate=null）
 * 时不渲染——「无事件数据」与「0%」语义不同，灰色「—」由调用方兜底。
 */
function DeliveryRateBadges({ health }: { health: ChannelHealth | null | undefined }) {
  const { t } = useTranslation(['channels', 'common'])
  if (!health || health.delivery_rate == null || health.bounce_rate == null) {
    return <span className="text-xs text-slate-500 dark:text-slate-400">—</span>
  }
  return (
    <div className="flex flex-wrap items-center gap-1">
      <Badge
        variant="success"
        className="tabular-nums"
        title={t('badge.deliveredTitle', {
          delivered: health.delivered ?? 0,
          bounced: health.bounced ?? 0,
          complained: health.complained ?? 0,
        })}
      >
        {t('badge.delivered', { rate: health.delivery_rate })}
      </Badge>
      <Badge
        variant={health.bounce_rate > 5 ? 'danger' : health.bounce_rate > 2 ? 'warning' : 'neutral'}
        className="tabular-nums"
        title={t('badge.bounceTitle')}
      >
        {t('badge.bounced', { rate: health.bounce_rate })}
      </Badge>
      {(health.complaint_rate ?? 0) > 0 && (
        <Badge variant="danger" className="tabular-nums">
          {t('badge.complained', { rate: health.complaint_rate })}
        </Badge>
      )}
    </div>
  )
}

/* ---------------------------------- 新建/编辑表单（复用同一表单） ---------------------------------- */

/** 密钥字段旁的「清除」小按钮（Field labelExtra 用）：点击后清空输入并在保存时提交 clear_secrets */
function ClearSecretButton({ onClick }: { onClick: () => void }) {
  const { t } = useTranslation(['channels', 'common'])
  return (
    <button
      type="button"
      onClick={onClick}
      className="text-xs font-medium text-red-500 transition-colors hover:text-red-600 dark:text-red-400 dark:hover:text-red-300"
    >
      {t('form.clearSecret')}
    </button>
  )
}

function ChannelFormModal({ channel, onClose }: { channel: Channel | null; onClose: () => void }) {
  const isEdit = channel !== null
  const { t } = useTranslation(['channels', 'common'])
  const queryClient = useQueryClient()
  // 编辑入口快照的原始平台（弹窗按渠道 id 键控重挂载，弹窗期间 channel 引用不变）：
  // 用于判断是否切换了平台——切平台后密钥留空不能再豁免校验
  const [originalProvider] = useState<string | null>(() => channel?.provider ?? null)
  const [values, setValues] = useState<ChannelFormValues>(() =>
    channel ? valuesFromChannel(channel) : createEmptyValues(),
  )
  const [errors, setErrors] = useState<FormErrors>({})
  // 「清除」标记：isEdit 且原渠道该字段有值时可标记，保存时写入 clear_secrets
  const [clearMarks, setClearMarks] = useState<ClearMarks>({})

  const mutation = useMutation({
    mutationFn: (payload: ChannelPayload) =>
      isEdit
        ? api<Channel>(`/api/channels/${channel.id}`, { method: 'PUT', body: payload })
        : api<Channel>('/api/channels', { method: 'POST', body: payload }),
    onSuccess: () => {
      toast.success(isEdit ? t('toast.updated') : t('toast.created'))
      invalidateChannelData(queryClient)
      onClose()
    },
    onError: (err) => toast.error(err.message),
  })

  /** 标记清除：清空输入框，保存时写入 clear_secrets；再次输入则取消标记 */
  function markClear(key: ClearableSecretKey) {
    setValues((prev) => ({ ...prev, [key]: '' }))
    setClearMarks((prev) => ({ ...prev, [key]: true }))
  }

  function setField<K extends keyof ChannelFormValues>(key: K, value: ChannelFormValues[K]) {
    setValues((prev) => ({ ...prev, [key]: value }))
    setErrors((prev) => (prev[key] ? { ...prev, [key]: undefined } : prev))
    // 重新输入密钥 = 用新值替换，取消已标记的「清除」
    if (CLEARABLE_SECRET_KEYS.includes(key as ClearableSecretKey)) {
      const secretKey = key as ClearableSecretKey
      setClearMarks((prev) => (prev[secretKey] ? { ...prev, [secretKey]: false } : prev))
    }
  }

  function handleSubmit(e: FormEvent<HTMLFormElement>) {
    e.preventDefault()
    // 编辑且未切平台：密钥留空 = 不修改；新建或切了平台：按新建口径校验必填凭据
    const requireCredentials = !isEdit || originalProvider !== values.provider
    const nextErrors = validateValues(values, requireCredentials, t)
    setErrors(nextErrors)
    if (Object.values(nextErrors).some(Boolean)) return
    mutation.mutate(buildPayload(values, isEdit, clearMarks))
  }

  const defaults = PROVIDER_DEFAULTS[values.provider]
  const maskHint = isEdit ? t('form.maskHint') : undefined

  return (
    <Modal
      open
      onClose={onClose}
      title={isEdit ? t('form.editTitle', { name: channel.name }) : t('page.create')}
      className="sm:max-w-2xl"
      footer={
        <>
          <Button variant="secondary" onClick={onClose} disabled={mutation.isPending}>
            {t('common:actions.cancel')}
          </Button>
          <Button type="submit" form="channel-form" loading={mutation.isPending}>
            {isEdit ? t('form.saveChanges') : t('form.submitCreate')}
          </Button>
        </>
      }
    >
      <form id="channel-form" onSubmit={handleSubmit} className="space-y-5" noValidate>
        {/* 基础信息 */}
        <section className="space-y-3">
          <h3 className="text-xs font-semibold uppercase tracking-wide text-slate-500 dark:text-slate-400">
            {t('labels.basicInfo')}
          </h3>
          <Field label={t('form.name')} error={errors.name}>
            <Input
              name="name"
              value={values.name}
              onChange={(e) => setField('name', e.target.value)}
              placeholder={t('form.namePlaceholder')}
              autoFocus
            />
          </Field>
          <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
            <Field label={t('labels.provider')}>
              <Select
                name="provider"
                value={values.provider}
                onChange={(e) => setField('provider', e.target.value as ProviderOption)}
              >
                {PROVIDER_OPTIONS.map((p) => (
                  <option key={p} value={p}>
                    {t(PROVIDER_LABELS[p])}
                  </option>
                ))}
              </Select>
            </Field>
            <Field label={t('labels.priority')} hint={t('form.priorityHint')}>
              <Input
                name="priority"
                type="number"
                min={0}
                value={values.priority}
                onChange={(e) => setField('priority', e.target.value)}
                placeholder="0"
              />
            </Field>
          </div>
        </section>

        {/* 平台凭据（mock 的 api_key 为失败模拟魔法值，同样在此配置） */}
        {values.provider !== 'smtp' && (
          <section className="space-y-3">
            <h3 className="text-xs font-semibold uppercase tracking-wide text-slate-500 dark:text-slate-400">
              {t('form.sectionCredentials')}
            </h3>
            {values.provider === 'mock' && (
              // mock 失败模拟魔法值说明（与后端 mock provider 约定一致，api_key 非敏感）：
              // 魔法值 fail / fail_rate_limit / fail_auth 原样保留，完整文案见词典 form.mockHintCreate/mockHintEdit
              <Field
                label={t('form.mockApiKeyLabel')}
                error={errors.api_key}
                hint={isEdit ? t('form.mockHintEdit') : t('form.mockHintCreate')}
              >
                <Input
                  name="api_key"
                  value={values.api_key}
                  onChange={(e) => setField('api_key', e.target.value)}
                  placeholder={channel?.api_key || t('form.mockApiKeyPlaceholder')}
                  className="font-mono"
                  autoComplete="off"
                />
              </Field>
            )}
            {providerNeedsApiKey(values.provider) && (
              <Field label="API Key" error={errors.api_key} hint={maskHint}>
                <Input
                  name="api_key"
                  value={values.api_key}
                  onChange={(e) => setField('api_key', e.target.value)}
                  placeholder={channel?.api_key || t('form.apiKeyPlaceholder')}
                  className="font-mono"
                  autoComplete="off"
                />
              </Field>
            )}
            {values.provider === 'mailjet' && (
              <Field
                label={t('form.apiSecretLabel')}
                error={errors.api_secret}
                hint={maskHint ?? t('form.apiSecretHint')}
                labelExtra={
                  isEdit && channel?.api_secret ? (
                    <ClearSecretButton onClick={() => markClear('api_secret')} />
                  ) : undefined
                }
              >
                <Input
                  name="api_secret"
                  value={values.api_secret}
                  onChange={(e) => setField('api_secret', e.target.value)}
                  placeholder={clearMarks.api_secret ? t('form.willClear') : channel?.api_secret || t('form.apiSecretPlaceholder')}
                  className="font-mono"
                  autoComplete="off"
                />
              </Field>
            )}
            {values.provider === 'mailgun' && (
              <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
                <Field label={t('form.domain')} error={errors.domain}>
                  <Input
                    name="domain"
                    value={values.domain}
                    onChange={(e) => setField('domain', e.target.value)}
                    placeholder="mg.example.com"
                  />
                </Field>
                <Field label={t('form.region')}>
                  <Select
                    name="region"
                    value={values.region}
                    onChange={(e) => setField('region', e.target.value as 'us' | 'eu')}
                  >
                    <option value="us">{t('form.regionUs')}</option>
                    <option value="eu">{t('form.regionEu')}</option>
                  </Select>
                </Field>
              </div>
            )}
          </section>
        )}

        {/* 发件人 */}
        <section className="space-y-3">
          <h3 className="text-xs font-semibold uppercase tracking-wide text-slate-500 dark:text-slate-400">
            {t('labels.sender')}
          </h3>
          <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
            <Field
              label={t('form.fromEmail')}
              error={errors.from_email}
              hint={values.provider === 'smtp' ? t('form.fromEmailHintSmtp') : t('form.fromEmailHintDefault')}
            >
              <Input
                name="from_email"
                value={values.from_email}
                onChange={(e) => setField('from_email', e.target.value)}
                placeholder="sender@example.com"
              />
            </Field>
            <Field label={t('form.fromName')}>
              <Input
                name="from_name"
                value={values.from_name}
                onChange={(e) => setField('from_name', e.target.value)}
                placeholder="CatMail"
              />
            </Field>
          </div>
        </section>

        {/* 限额与网络 */}
        <section className="space-y-3">
          <h3 className="text-xs font-semibold uppercase tracking-wide text-slate-500 dark:text-slate-400">
            {t('form.sectionLimits')}
          </h3>
          <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
            <Field label={t('form.dailyLimit')} hint={t('form.limitHintDefault')}>
              <Input
                type="number"
                min={0}
                name="daily_limit_override"
                value={values.daily_limit_override}
                onChange={(e) => setField('daily_limit_override', e.target.value)}
                placeholder={t('form.defaultPlaceholder', { value: defaults.daily })}
              />
            </Field>
            <Field label={t('form.monthlyLimit')} hint={t('form.limitHintDefault')}>
              <Input
                type="number"
                min={0}
                name="monthly_limit_override"
                value={values.monthly_limit_override}
                onChange={(e) => setField('monthly_limit_override', e.target.value)}
                placeholder={t('form.defaultPlaceholder', { value: defaults.monthly })}
              />
            </Field>
            <Field
              label={t('form.rateLimitLabel')}
              error={errors.rate_limit_per_sec}
              hint={t('form.rateLimitHint')}
            >
              <Input
                type="number"
                min={0}
                name="rate_limit_per_sec"
                value={values.rate_limit_per_sec}
                onChange={(e) => setField('rate_limit_per_sec', e.target.value)}
                placeholder="0"
              />
            </Field>
          </div>
          <Field
            label={t('labels.proxy')}
            hint={maskHint ?? t('form.proxyHint')}
            labelExtra={
              isEdit && channel?.proxy_url ? (
                <ClearSecretButton onClick={() => markClear('proxy_url')} />
              ) : undefined
            }
          >
            <Input
              name="proxy_url"
              value={values.proxy_url}
              onChange={(e) => setField('proxy_url', e.target.value)}
              placeholder={
                clearMarks.proxy_url ? t('form.willClear') : channel?.proxy_url || 'http://user:pass@host:port'
              }
              className="font-mono"
              autoComplete="off"
            />
          </Field>
          <Field
            label={t('form.webhookSecret')}
            hint={t('form.webhookSecretHint')}
            labelExtra={
              isEdit && channel?.webhook_secret ? (
                <ClearSecretButton onClick={() => markClear('webhook_secret')} />
              ) : undefined
            }
          >
            <Input
              name="webhook_secret"
              value={values.webhook_secret}
              onChange={(e) => setField('webhook_secret', e.target.value)}
              placeholder={
                clearMarks.webhook_secret
                  ? t('form.willClear')
                  : channel?.webhook_secret || t('form.webhookSecretPlaceholder')
              }
              className="font-mono"
              autoComplete="off"
            />
          </Field>
          <Field label={t('form.allowedDomains')} hint={t('form.allowedDomainsHint')}>
            <Input
              name="allowed_domains"
              value={values.allowed_domains}
              onChange={(e) => setField('allowed_domains', e.target.value)}
              placeholder="example.com, mail.example.org"
            />
          </Field>
        </section>

        {/* SMTP 五件套 */}
        {values.provider === 'smtp' && (
          <section className="space-y-3">
            <h3 className="text-xs font-semibold uppercase tracking-wide text-slate-500 dark:text-slate-400">
              {t('form.sectionSmtp')}
            </h3>
            <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
              <Field label={t('form.smtpHost')} error={errors.smtp_host}>
                <Input
                  name="smtp_host"
                  value={values.smtp_host}
                  onChange={(e) => setField('smtp_host', e.target.value)}
                  placeholder="smtp.example.com"
                />
              </Field>
              <Field label={t('form.smtpPort')} error={errors.smtp_port}>
                <Input
                  type="number"
                  min={1}
                  max={65535}
                  name="smtp_port"
                  value={values.smtp_port}
                  onChange={(e) => setField('smtp_port', e.target.value)}
                  placeholder="587"
                />
              </Field>
            </div>
            <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
              <Field label={t('form.smtpUser')}>
                <Input
                  name="smtp_user"
                  value={values.smtp_user}
                  onChange={(e) => setField('smtp_user', e.target.value)}
                  autoComplete="off"
                />
              </Field>
              <Field
                label={t('form.smtpPass')}
                hint={isEdit ? t('form.blankKeep') : undefined}
                labelExtra={
                  isEdit && channel?.smtp_pass ? (
                    <ClearSecretButton onClick={() => markClear('smtp_pass')} />
                  ) : undefined
                }
              >
                <Input
                  type="password"
                  name="smtp_pass"
                  value={values.smtp_pass}
                  onChange={(e) => setField('smtp_pass', e.target.value)}
                  placeholder={clearMarks.smtp_pass ? t('form.willClear') : isEdit ? '••••••••' : ''}
                  autoComplete="new-password"
                />
              </Field>
            </div>
            <Field label={t('form.tlsMode')}>
              <Select
                name="smtp_tls"
                value={values.smtp_tls}
                onChange={(e) => setField('smtp_tls', e.target.value as ChannelFormValues['smtp_tls'])}
              >
                {SMTP_TLS_OPTIONS.map((opt) => (
                  <option key={opt.value} value={opt.value}>
                    {t(opt.labelKey)}
                  </option>
                ))}
              </Select>
            </Field>
          </section>
        )}
      </form>
    </Modal>
  )
}

/* ---------------------------------- 测试发送 ---------------------------------- */

function TestSendModal({ channel, onClose }: { channel: Channel; onClose: () => void }) {
  const { t } = useTranslation(['channels', 'common'])
  const [to, setTo] = useState('')
  // 邮件模板（可选）：选择后用模板渲染结果替代默认测试内容；样本变量 JSON 填充
  const [templateId, setTemplateId] = useState('')
  const [varsText, setVarsText] = useState('')
  const [error, setError] = useState<string | null>(null)
  const queryClient = useQueryClient()
  const templatesQuery = useTemplates()
  const templates = templatesQuery.data?.items ?? []
  const selectedTemplate = templates.find((tpl) => String(tpl.id) === templateId)

  const mutation = useMutation({
    mutationFn: (payload: { to: string; template_id?: number; variables?: Record<string, unknown> }) =>
      api<TestSendResponse>(`/api/channels/${channel.id}/test`, { method: 'POST', body: payload }),
    onSuccess: (res) => {
      // 测试发送会扣减渠道日/月额度并落一条测试日志（失败同样落库）、计入 Key 的 total_sent；
      // 429 额度用尽走 onError（api 层已把统一错误体 {error:{message}} 归一为文案）
      invalidateChannelData(queryClient)
      void queryClient.invalidateQueries({ queryKey: ['logs'] })
      void queryClient.invalidateQueries({ queryKey: ['keys'] })
      if (res.ok) {
        toast.success(t('toast.testSuccess', { name: channel.name, latency: res.latency_ms }))
        // 模板渲染缺失变量（占位符留空策略的响应可见性）
        if (res.missing_variables?.length) {
          toast.warning(t('toast.missingVars', { vars: res.missing_variables.join('、') }))
        }
        onClose()
      } else {
        setError(res.error || t('testSend.failed'))
        toast.error(t('toast.testFailed', { name: channel.name, error: res.error || t('testSend.unknownError') }))
      }
    },
    onError: (err) => {
      setError(err.message)
      toast.error(t('toast.testFailed', { name: channel.name, error: err.message }))
    },
  })

  function handleSubmit(e: FormEvent<HTMLFormElement>) {
    e.preventDefault()
    const target = to.trim()
    if (!EMAIL_RE.test(target)) {
      setError(t('validation.invalidEmail'))
      return
    }
    // 可选模板：解析样本变量 JSON（空 = {}），非法 JSON 报错不发送
    let variables: Record<string, unknown> | undefined
    if (templateId) {
      const parsed = parseSampleVars(varsText)
      if (parsed === null) {
        setError(t('validation.invalidSampleVars'))
        return
      }
      variables = parsed
    }
    setError(null)
    mutation.mutate({
      to: target,
      ...(templateId ? { template_id: Number(templateId) } : {}),
      ...(templateId ? { variables } : {}),
    })
  }

  return (
    <Modal
      open
      onClose={onClose}
      title={t('testSend.title', { name: channel.name })}
      footer={
        <>
          <Button variant="secondary" onClick={onClose} disabled={mutation.isPending}>
            {t('common:actions.cancel')}
          </Button>
          <Button type="submit" form="test-send-form" loading={mutation.isPending}>
            {t('testSend.submit')}
          </Button>
        </>
      }
    >
      <form id="test-send-form" onSubmit={handleSubmit} className="space-y-3" noValidate>
        <Field
          label={t('testSend.to')}
          error={error ?? undefined}
          hint={t('testSend.toHint')}
        >
          <Input
            name="to"
            type="email"
            value={to}
            onChange={(e) => {
              setTo(e.target.value)
              setError(null)
            }}
            placeholder="you@example.com"
            autoFocus
          />
        </Field>
        {/* 邮件模板（可选，邮件模板功能包）：渲染结果整体替换默认测试主题/正文 */}
        <Field label={t('testSend.template')} hint={t('testSend.templateHint')}>
          <Select
            name="template_id"
            value={templateId}
            onChange={(e) => {
              setTemplateId(e.target.value)
              setError(null)
            }}
          >
            <option value="">{t('testSend.noTemplate')}</option>
            {templates.map((tpl) => (
              <option key={tpl.id} value={tpl.id}>
                {t('testSend.templateOption', { name: tpl.name, id: tpl.id })}
              </option>
            ))}
          </Select>
        </Field>
        {selectedTemplate && (
          <Field
            label={t('testSend.sampleVars')}
            hint={t('testSend.varsHint', {
              vars: selectedTemplate.variables?.join('、') || t('testSend.varsEmpty'),
            })}
          >
            <Textarea
              name="variables"
              value={varsText}
              onChange={(e) => {
                setVarsText(e.target.value)
                setError(null)
              }}
              placeholder={'{"code": "123456"}'}
              className="min-h-16 font-mono text-xs"
              spellCheck={false}
            />
            {extractTemplateVars(selectedTemplate.subject).length > 0 && (
              <p className="mt-1 text-xs text-slate-400 dark:text-slate-500">
                {t('testSend.subjectVars', {
                  vars: extractTemplateVars(selectedTemplate.subject)
                    .map((v) => `{{${v}}}`)
                    .join('、'),
                })}
              </p>
            )}
          </Field>
        )}
      </form>
    </Modal>
  )
}

/* ---------------------------------- 删除确认 ---------------------------------- */

function DeleteChannelModal({ channel, onClose }: { channel: Channel; onClose: () => void }) {
  const { t } = useTranslation(['channels', 'common'])
  const queryClient = useQueryClient()
  const mutation = useMutation({
    mutationFn: () => api<{ ok: boolean }>(`/api/channels/${channel.id}`, { method: 'DELETE' }),
    onSuccess: () => {
      toast.success(t('toast.deleted', { name: channel.name }))
      invalidateChannelData(queryClient)
      onClose()
    },
    onError: (err) => toast.error(err.message),
  })

  return (
    <Modal
      open
      onClose={onClose}
      title={t('delete.title')}
      footer={
        <>
          <Button variant="secondary" onClick={onClose} disabled={mutation.isPending}>
            {t('common:actions.cancel')}
          </Button>
          <Button variant="danger" loading={mutation.isPending} onClick={() => mutation.mutate()}>
            {t('delete.confirm')}
          </Button>
        </>
      }
    >
      <p className="text-sm text-slate-600 dark:text-slate-300">
        {t('delete.confirmPrefix')}{' '}
        <span className="font-medium text-slate-900 dark:text-white">
          {t('delete.nameWrapped', { name: channel.name })}
        </span> {t('delete.confirmSuffix')}
      </p>
      <p className="mt-2 text-xs text-slate-500 dark:text-slate-400">{t('delete.warning')}</p>
    </Modal>
  )
}

/* ---------------------------------- 详情停靠面板（含健康区） ---------------------------------- */

/** 面板的 label/value 行（与日志详情面板同款） */
function DetailRow({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="flex gap-3 py-1.5 text-sm">
      <span className="w-24 shrink-0 text-slate-500 dark:text-slate-400">{label}</span>
      <span className="min-w-0 flex-1 break-words text-slate-700 dark:text-slate-200">
        {children}
      </span>
    </div>
  )
}

/**
 * 事件回传地址行（送达链路三件套）：`<origin>/webhooks/mail-events/<token>`
 * 一键复制 + 强制轮换（轮换后平台侧 Webhook 地址需同步更新，旧地址立即失效）。
 * token 由建渠道自动生成；存量渠道（升级前的旧行）token 为空时先补生成。
 */
function WebhookTokenRow({ channel }: { channel: Channel }) {
  const { t } = useTranslation(['channels', 'common'])
  const queryClient = useQueryClient()
  const rotate = useMutation({
    mutationFn: () => rotateChannelWebhookToken(channel.id, true),
    onSuccess: () => {
      toast.success(t('toast.webhookRotated'))
      invalidateChannelData(queryClient)
    },
    onError: (err) => toast.error(err.message),
  })
  if (!channel.webhook_token) {
    return <span className="text-xs text-slate-500 dark:text-slate-400">{t('detail.webhookTokenMissing')}</span>
  }
  const url = `${window.location.origin}/webhooks/mail-events/${channel.webhook_token}`
  return (
    <span className="inline-flex flex-wrap items-center gap-1.5">
      <code
        className="max-w-56 truncate rounded bg-slate-100 px-1.5 py-0.5 font-mono text-[11px] text-slate-600 dark:bg-slate-800 dark:text-slate-300"
        title={url}
      >
        {url}
      </code>
      <Button
        variant="ghost"
        className="h-6 px-1.5 text-xs"
        onClick={() => copyText(url).then(() => toast.success(t('toast.copiedWebhookUrl')))}
        aria-label={t('detail.copyWebhookAria')}
      >
        {t('common:actions.copy')}
      </Button>
      <Button
        variant="ghost"
        loading={rotate.isPending}
        className="h-6 px-1.5 text-xs text-amber-600 dark:text-amber-400"
        onClick={() => rotate.mutate()}
        aria-label={t('detail.rotateAria')}
      >
        {t('detail.rotate')}
      </Button>
    </span>
  )
}

/**
 * 渠道详情右侧停靠面板（与日志详情面板同款：原生 <dialog> 全高停靠右侧，::backdrop 点击/Esc 关闭，
 * <lg 时占满全屏；开关与事件汇流复用 useNativeDialog，组件条件挂载，open 恒 true）。
 * 近 7 日健康区（A3 + 送达侧指标）+ 基础信息；数据直接来自列表行（health 随
 * GET /api/channels 一并返回），无额外请求。
 */
function ChannelDetailPanel({ channel, onClose }: { channel: Channel; onClose: () => void }) {
  const { t } = useTranslation(['channels', 'common'])
  const dialogRef = useNativeDialog(true, onClose)
  // 窗口内零日志（health=null 或 total=0）→ 健康区展示空态，而非 0% 误读为全失败
  const health = channel.health && channel.health.total > 0 ? channel.health : null

  return (
    <dialog
      ref={dialogRef}
      aria-label={t('detail.ariaLabel')}
      className="glass-surface app-dialog app-dock flex w-full flex-col border-l border-border-subtle bg-surface-raised shadow-xl animate-dock-in-right max-w-none max-h-none h-dvh m-0 ml-auto lg:w-[min(520px,90vw)]"
    >
      <div className="flex shrink-0 items-center justify-between border-b border-slate-200 px-5 py-4 dark:border-slate-800">
        <h2 className="text-base font-semibold text-slate-900 dark:text-white">
          {t('detail.title', { name: channel.name })}
        </h2>
        <button
          type="button"
          onClick={onClose}
          aria-label={t('common:actions.close')}
          className="flex h-7 w-7 items-center justify-center rounded-lg text-slate-400 transition-colors hover:bg-slate-100 hover:text-slate-600 dark:hover:bg-slate-800 dark:hover:text-slate-200"
        >
          <X className="h-4 w-4" />
        </button>
      </div>
      <div className="min-h-0 flex-1 overflow-y-auto px-5 py-4">
        <div className="space-y-5">
          {/* 健康区（A3） */}
          <section>
            <h3 className="mb-2 text-xs font-semibold uppercase tracking-wide text-slate-500 dark:text-slate-400">
              {t('detail.healthTitle')}
            </h3>
            {!health ? (
              <div className="rounded-lg border border-dashed border-slate-300 px-4 py-6 text-center text-xs text-slate-500 dark:border-slate-700 dark:text-slate-400">
                {t('detail.noRecords')}
              </div>
            ) : (
              <div className="space-y-3">
                {/* 成功率高亮 */}
                <div className="rounded-lg border border-slate-200 p-4 dark:border-slate-800">
                  <div className="flex items-end justify-between gap-3">
                    <p
                      className={cn(
                        'text-3xl font-semibold tabular-nums',
                        successRateTone(health.success_rate).text,
                      )}
                    >
                      {health.success_rate}%
                    </p>
                    <p className="pb-1 text-xs text-slate-500 dark:text-slate-400">
                      {t('detail.totalLine', {
                        total: health.total,
                        sent: health.sent,
                        failed: health.failed,
                      })}
                    </p>
                  </div>
                  <div className="mt-3 h-2 overflow-hidden rounded-full bg-slate-100 dark:bg-slate-800">
                    <div
                      className={cn('h-full rounded-full', successRateTone(health.success_rate).bar)}
                      style={{ width: `${health.success_rate}%` }}
                    />
                  </div>
                </div>
                {/* 最近成功 / 最近失败 */}
                <div className="rounded-lg border border-slate-200 px-4 py-2 dark:border-slate-800">
                  <div className="divide-y divide-slate-100 dark:divide-slate-800">
                    <DetailRow label={t('detail.lastSuccess')}>
                      <RelativeTime iso={health.last_success_at} />
                    </DetailRow>
                    <DetailRow label={t('detail.lastFailed')}>
                      <RelativeTime iso={health.last_failed_at} />
                    </DetailRow>
                  </div>
                </div>
                {/* 送达侧（送达链路三件套）：mail_events 事件计数与比率；无终态事件展示空态 */}
                <div>
                  <p className="mb-1.5 text-xs font-medium text-slate-500 dark:text-slate-400">
                    {t('detail.deliveryPerf')}
                  </p>
                  {health.delivery_rate == null || health.bounce_rate == null ? (
                    <p className="text-xs text-slate-500 dark:text-slate-400">
                      {t('detail.noDeliveryData')}
                    </p>
                  ) : (
                    <div className="grid grid-cols-3 gap-2">
                      <div className="rounded-lg bg-slate-50 px-3 py-2 text-center dark:bg-slate-800/60">
                        <p className="text-lg font-semibold tabular-nums text-emerald-700 dark:text-emerald-300">
                          {health.delivery_rate}%
                        </p>
                        <p className="text-[11px] text-slate-500 dark:text-slate-400">
                          {t('detail.deliveredCount', { count: health.delivered ?? 0 })}
                        </p>
                      </div>
                      <div className="rounded-lg bg-slate-50 px-3 py-2 text-center dark:bg-slate-800/60">
                        <p
                          className={cn(
                            'text-lg font-semibold tabular-nums',
                            bounceRateTone(health.bounce_rate),
                          )}
                        >
                          {health.bounce_rate}%
                        </p>
                        <p className="text-[11px] text-slate-500 dark:text-slate-400">
                          {t('detail.bouncedCount', { count: health.bounced ?? 0 })}
                        </p>
                      </div>
                      <div className="rounded-lg bg-slate-50 px-3 py-2 text-center dark:bg-slate-800/60">
                        <p className="text-lg font-semibold tabular-nums text-slate-700 dark:text-slate-200">
                          {health.complaint_rate ?? 0}%
                        </p>
                        <p className="text-[11px] text-slate-500 dark:text-slate-400">
                          {t('detail.complainedCount', { count: health.complained ?? 0 })}
                        </p>
                      </div>
                    </div>
                  )}
                </div>
                {/* Top 失败原因（次数徽章） */}
                <div>
                  <p className="mb-1.5 text-xs font-medium text-slate-500 dark:text-slate-400">
                    {t('detail.topErrorsTitle')}
                  </p>
                  {health.top_errors.length === 0 ? (
                    <p className="text-xs text-slate-500 dark:text-slate-400">{t('detail.noErrors')}</p>
                  ) : (
                    <ul className="space-y-1.5">
                      {health.top_errors.map((e) => (
                        <li
                          key={e.reason}
                          className="flex items-start justify-between gap-2 rounded-lg bg-slate-50 px-3 py-2 dark:bg-slate-800/60"
                        >
                          <span className="min-w-0 break-words text-xs leading-relaxed text-slate-700 dark:text-slate-200">
                            {e.reason}
                          </span>
                          <Badge variant="danger" className="shrink-0 tabular-nums">
                            {t('detail.errorCount', { count: e.count })}
                          </Badge>
                        </li>
                      ))}
                    </ul>
                  )}
                </div>
              </div>
            )}
          </section>

          {/* 基础信息 */}
          <section>
            <h3 className="mb-2 text-xs font-semibold uppercase tracking-wide text-slate-500 dark:text-slate-400">
              {t('labels.basicInfo')}
            </h3>
            <div className="rounded-lg border border-slate-200 px-4 py-2 dark:border-slate-800">
              <div className="divide-y divide-slate-100 dark:divide-slate-800">
                <DetailRow label={t('labels.provider')}>
                  <ProviderBadge provider={channel.provider} />
                </DetailRow>
                <DetailRow label={t('common:fields.status')}>
                  <ChannelStatusBadge status={channel.status} />
                </DetailRow>
                <DetailRow label={t('labels.priority')}>{channel.priority}</DetailRow>
                <DetailRow label={t('labels.usage')}>
                  {usageText(channel.used_day, channel.effective_daily_limit)} ·{' '}
                  {usageText(channel.used_month, channel.effective_monthly_limit)}
                </DetailRow>
                <DetailRow label={t('labels.sender')}>{channel.from_email || '—'}</DetailRow>
                <DetailRow label={t('labels.proxy')}>{channel.proxy_url || t('detail.direct')}</DetailRow>
                {/* 送达事件回传地址（送达链路三件套）：token 明文展示（拼平台 Webhook 用），可复制/轮换 */}
                <DetailRow label={t('detail.webhookUrl')}>
                  <WebhookTokenRow channel={channel} />
                </DetailRow>
                {channel.rate_limit_per_sec > 0 && (
                  <DetailRow label={t('labels.rateLimit')}>{channel.rate_limit_per_sec}/s</DetailRow>
                )}
                {channel.cooldown_until && (
                  <DetailRow label={t('detail.cooldownUntil')}>
                    <RelativeTime iso={channel.cooldown_until} />
                  </DetailRow>
                )}
                {channel.last_error && (
                  <DetailRow label={t('detail.lastError')}>
                    <span className="text-red-600 dark:text-red-400">{channel.last_error}</span>
                  </DetailRow>
                )}
                <DetailRow label={t('labels.lastUsed')}>
                  <RelativeTime iso={channel.last_used_at} />
                </DetailRow>
                <DetailRow label={t('common:fields.createdAt')}>
                  <RelativeTime iso={channel.created_at ?? null} />
                </DetailRow>
              </div>
            </div>
          </section>
        </div>
      </div>
    </dialog>
  )
}

/* ---------------------------------- 页面 ---------------------------------- */

type FormModalState = { mode: 'create' } | { mode: 'edit'; channel: Channel }

/** 渠道状态筛选值（'all' 或后端 Channel.Status 枚举） */
type StatusFilterValue = 'all' | 0 | 1 | 2 | 3

const CHANNEL_FILTER_DEBOUNCE_MS = 300

export function ChannelsPage() {
  const { t } = useTranslation(['channels', 'common'])
  const queryClient = useQueryClient()
  const channelsQuery = useChannels()
  const [formModal, setFormModal] = useState<FormModalState | null>(null)
  const [testChannel, setTestChannel] = useState<Channel | null>(null)
  const [deleteTarget, setDeleteTarget] = useState<Channel | null>(null)
  // 详情停靠面板：打开的渠道（健康区数据随列表行携带）
  const [detailChannel, setDetailChannel] = useState<Channel | null>(null)

  // 客户端筛选：平台 / 状态 / 名称关键词；platform/status/q 同步 URL query（刷新、前进后退不丢）
  const [searchParams, setSearchParams] = useSearchParams()
  const updateParams = useCallback(
    (mutate: (params: URLSearchParams) => void) => {
      setSearchParams(
        (prev) => {
          const next = new URLSearchParams(prev)
          mutate(next)
          return next
        },
        { replace: true },
      )
    },
    [setSearchParams],
  )
  const platformParam = searchParams.get('platform')
  const platformFilter: 'all' | ProviderOption = (PROVIDER_OPTIONS as readonly string[]).includes(
    platformParam ?? '',
  )
    ? (platformParam as ProviderOption)
    : 'all'
  const statusParam = searchParams.get('status')
  const statusFilter: StatusFilterValue =
    statusParam === '0' || statusParam === '1' || statusParam === '2' || statusParam === '3'
      ? (Number.parseInt(statusParam, 10) as 0 | 1 | 2 | 3)
      : 'all'
  const qParam = searchParams.get('q') ?? ''

  // 关键词输入本地态 → 300ms 防抖后写入 URL（同 LogsPage：区分自身推送与外部导航，避免打断输入）
  const [keywordInput, setKeywordInput] = useState(qParam)
  const debouncedKeyword = useDebouncedValue(keywordInput, CHANNEL_FILTER_DEBOUNCE_MS)
  const pushedQRef = useRef<string | null>(null)

  useEffect(() => {
    const trimmed = debouncedKeyword.trim()
    if (trimmed === pushedQRef.current) return
    pushedQRef.current = trimmed
    if (trimmed !== qParam) {
      updateParams((p) => {
        if (trimmed) p.set('q', trimmed)
        else p.delete('q')
      })
    }
  }, [debouncedKeyword, qParam, updateParams])

  useEffect(() => {
    // 外部 q 变化（前进/后退、分享链接）→ 回填输入框
    if (pushedQRef.current !== qParam) setKeywordInput(qParam)
  }, [qParam])

  const statusMutation = useMutation({
    mutationFn: ({ id, action }: { id: number; action: ChannelStatusAction }) =>
      api<Channel>(`/api/channels/${id}/status`, { method: 'POST', body: { action } }),
    onSuccess: (_data, vars) => {
      toast.success(vars.action === 'enable' ? t('toast.enabled') : t('toast.disabled'))
      invalidateChannelData(queryClient)
    },
    onError: (err) => toast.error(err.message),
  })

  // 复制渠道（E6）：服务端复制全字段（含 secrets），副本默认停用防意外上线；
  // 成功后提示副本名并刷新列表（新副本出现在同优先级分组中）。
  const duplicateMutation = useMutation({
    mutationFn: (channel: Channel) => duplicateChannel(channel.id),
    onSuccess: (dup) => {
      toast.success(t('toast.duplicated', { name: dup.name }))
      invalidateChannelData(queryClient)
    },
    onError: (err) => toast.error(err.message),
  })

  const items = channelsQuery.data?.items ?? []
  const filterKeyword = debouncedKeyword.trim().toLowerCase()
  const filteredItems = useMemo(
    () =>
      items.filter(
        (channel) =>
          (platformFilter === 'all' || channel.provider === platformFilter) &&
          (statusFilter === 'all' || channel.status === statusFilter) &&
          (!filterKeyword || channel.name.toLowerCase().includes(filterKeyword)),
      ),
    [items, platformFilter, statusFilter, filterKeyword],
  )
  const hasActiveFilter = platformFilter !== 'all' || statusFilter !== 'all' || filterKeyword !== ''

  function clearFilters() {
    setKeywordInput('')
    pushedQRef.current = ''
    updateParams((p) => {
      p.delete('platform')
      p.delete('status')
      p.delete('q')
    })
  }

  return (
    <div className="space-y-6">
      <PageHeader
        title={t('page.title')}
        description={t('page.description')}
        actions={
          <Button onClick={() => setFormModal({ mode: 'create' })}>
            <Plus className="h-4 w-4" aria-hidden />
            {t('page.create')}
          </Button>
        }
      />

      {/* 筛选工具栏（客户端过滤） */}
      <search className="flex flex-wrap items-center gap-3">
        <Select
          aria-label={t('filter.platformAria')}
          className="w-36"
          value={platformFilter}
          onChange={(e) => {
            const value = e.target.value
            updateParams((p) => {
              if (value === 'all') p.delete('platform')
              else p.set('platform', value)
            })
          }}
        >
          <option value="all">{t('filter.allPlatforms')}</option>
          {PROVIDER_OPTIONS.map((provider) => (
            <option key={provider} value={provider}>
              {t(PROVIDER_LABELS[provider])}
            </option>
          ))}
        </Select>
        <Select
          aria-label={t('filter.statusAria')}
          className="w-36"
          value={String(statusFilter)}
          onChange={(e) => {
            const value = e.target.value
            updateParams((p) => {
              if (value === 'all') p.delete('status')
              else p.set('status', value)
            })
          }}
        >
          <option value="all">{t('filter.allStatuses')}</option>
          <option value="0">{t('filter.statusEnabled')}</option>
          <option value="1">{t('filter.statusManualDisabled')}</option>
          <option value="2">{t('filter.statusAutoDisabled')}</option>
          <option value="3">{t('filter.statusCooldown')}</option>
        </Select>
        <Input
          aria-label={t('filter.nameAria')}
          className="w-64"
          placeholder={t('filter.namePlaceholder')}
          value={keywordInput}
          onChange={(e) => setKeywordInput(e.target.value)}
        />
        {hasActiveFilter && (
          <Button variant="ghost" className="h-9 px-2.5 text-xs" onClick={clearFilters}>
            {t('page.clearFilters')}
          </Button>
        )}
      </search>

      <Card>
        {channelsQuery.isPending ? (
          <QueryLoading skeleton="table" />
        ) : channelsQuery.isError ? (
          <QueryError
            message={channelsQuery.error.message}
            onRetry={() => channelsQuery.refetch()}
          />
        ) : items.length === 0 ? (
          <EmptyState
            icon={Mailbox}
            title={t('page.emptyTitle')}
            description={t('page.emptyDescription')}
            action={
              <Button onClick={() => setFormModal({ mode: 'create' })}>
                <Plus className="h-4 w-4" aria-hidden />
                {t('page.emptyAction')}
              </Button>
            }
          />
        ) : filteredItems.length === 0 ? (
          <EmptyState
            icon={SearchX}
            title={t('page.noMatchTitle')}
            description={t('page.noMatchDescription')}
            action={
              hasActiveFilter ? (
                <Button variant="secondary" onClick={clearFilters}>
                  {t('page.clearFilters')}
                </Button>
              ) : undefined
            }
          />
        ) : (
          <div className="overflow-x-auto">
            <Table>
              <THead>
                <tr>
                  <TH>{t('common:fields.name')}</TH>
                  <TH>{t('labels.provider')}</TH>
                  <TH>{t('common:fields.status')}</TH>
                  <TH>{t('table.successRate7d')}</TH>
                  <TH>{t('table.deliveryBounce7d')}</TH>
                  <TH numeric>{t('labels.priority')}</TH>
                  <TH numeric>{t('labels.usage')}</TH>
                  <TH>{t('labels.lastUsed')}</TH>
                  <TH className="text-right">{t('common:fields.actions')}</TH>
                </tr>
              </THead>
              <TBody>
                {filteredItems.map((channel) => {
                  const enabled = channel.status === 0
                  const statusPending =
                    statusMutation.isPending && statusMutation.variables?.id === channel.id
                  const duplicatePending =
                    duplicateMutation.isPending && duplicateMutation.variables?.id === channel.id
                  return (
                    <TRow key={channel.id}>
                      <TD className="max-w-56">
                        <button
                          type="button"
                          onClick={() => setDetailChannel(channel)}
                          title={t('table.viewDetail')}
                          className="group block max-w-full text-left"
                        >
                          <p className="font-medium text-slate-900 transition-colors group-hover:text-sky-600 group-hover:underline dark:text-white dark:group-hover:text-sky-400">
                            {channel.name}
                          </p>
                        </button>
                        {channel.last_error && (
                          <p
                            className="mt-0.5 truncate text-xs text-red-600 dark:text-red-400"
                            title={channel.last_error}
                          >
                            {channel.last_error}
                          </p>
                        )}
                      </TD>
                      <TD>
                        <ProviderBadge provider={channel.provider} />
                      </TD>
                      <TD>
                        <ChannelStatusBadge status={channel.status} />
                      </TD>
                      <TD>
                        <SuccessRateCell health={channel.health} />
                      </TD>
                      <TD>
                        {/* 送达侧徽标（送达链路三件套）：无事件数据展示灰色「—」 */}
                        <DeliveryRateBadges health={channel.health} />
                      </TD>
                      <TD numeric className="text-slate-600 dark:text-slate-300">
                        {channel.priority}
                      </TD>
                      <TD numeric className="text-slate-600 dark:text-slate-300">
                        {usageText(channel.used_day, channel.effective_daily_limit)} ·{' '}
                        {usageText(channel.used_month, channel.effective_monthly_limit)}
                        {channel.rate_limit_per_sec > 0 && (
                          <p className="mt-0.5 text-xs text-slate-500 dark:text-slate-400">
                            {t('table.rateLimitValue', { rate: channel.rate_limit_per_sec })}
                          </p>
                        )}
                      </TD>
                      <TD>
                        <RelativeTime iso={channel.last_used_at} />
                      </TD>
                      <TD className="text-right">
                        <div className="flex items-center justify-end gap-1">
                          <Button
                            variant="ghost"
                            className="h-7 px-2 text-xs"
                            onClick={() => setTestChannel(channel)}
                          >
                            {t('table.test')}
                          </Button>
                          <Button
                            variant="ghost"
                            className="h-7 px-2 text-xs"
                            onClick={() => setFormModal({ mode: 'edit', channel })}
                          >
                            {t('common:actions.edit')}
                          </Button>
                          <Button
                            variant="ghost"
                            loading={duplicatePending}
                            className="h-7 px-2 text-xs"
                            onClick={() => duplicateMutation.mutate(channel)}
                          >
                            {t('common:actions.copy')}
                          </Button>
                          <Button
                            variant="ghost"
                            loading={statusPending}
                            className="h-7 px-2 text-xs"
                            onClick={() =>
                              statusMutation.mutate({
                                id: channel.id,
                                action: enabled ? 'disable' : 'enable',
                              })
                            }
                          >
                            {enabled ? t('common:state.disabled') : t('common:state.enabled')}
                          </Button>
                          <button
                            type="button"
                            onClick={() => setDeleteTarget(channel)}
                            className="inline-flex h-7 items-center whitespace-nowrap rounded-lg px-2 text-xs text-red-600 transition-colors hover:bg-red-50 hover:text-red-700 dark:text-red-400 dark:hover:bg-red-500/10 dark:hover:text-red-300"
                          >
                            {t('common:actions.delete')}
                          </button>
                        </div>
                      </TD>
                    </TRow>
                  )
                })}
              </TBody>
            </Table>
          </div>
        )}
      </Card>

      {formModal && (
        <ChannelFormModal
          key={formModal.mode === 'edit' ? `edit-${formModal.channel.id}` : 'create'}
          channel={formModal.mode === 'edit' ? formModal.channel : null}
          onClose={() => setFormModal(null)}
        />
      )}
      {testChannel && <TestSendModal channel={testChannel} onClose={() => setTestChannel(null)} />}
      {deleteTarget && (
        <DeleteChannelModal channel={deleteTarget} onClose={() => setDeleteTarget(null)} />
      )}
      {detailChannel && (
        <ChannelDetailPanel channel={detailChannel} onClose={() => setDetailChannel(null)} />
      )}
    </div>
  )
}
