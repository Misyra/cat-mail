/**
 * API Keys 页（SPEC §6）：Key 表格（名称/掩码/状态/最近使用/创建时间）+ 新建（明文 key 一次性
 * 展示与复制，关闭前二次确认）+ 启停 + 删除确认。
 * 数据：GET/POST /api/keys、PUT/DELETE /api/keys/:id、POST /api/keys/:id/webhook/test
 * （SPEC §5，snake_case 原样字段名）。
 */
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { AlertCircle, Check, Copy, KeyRound, Plus, X } from 'lucide-react'
import { useEffect, useState, type FormEvent } from 'react'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import { PageHeader } from '@/components/PageHeader'
import { QueryError, QueryLoading } from '@/components/QueryState'
import { RelativeTime } from '@/components/RelativeTime'
import { Badge } from '@/components/badges'
import { Button } from '@/components/ui/Button'
import { Card } from '@/components/ui/Card'
import { EmptyState } from '@/components/ui/EmptyState'
import { Field, Input } from '@/components/ui/Input'
import { Modal } from '@/components/ui/Modal'
import { TD, TH, TBody, THead, TRow, Table, TableScroll } from '@/components/ui/Table'
import { api } from '@/lib/api'
import { copyText } from '@/lib/clipboard'
import {
  invalidateKeys,
  parseWebhookEvents,
  resetKeyWebhookSecret,
  testKeyWebhook,
  useApiKeys,
  KEYS_QUERY_KEY,
  WEBHOOK_EVENTS,
  type ApiKeyCreatePayload,
  type ApiKeyItem,
  type ApiKeyUpdatePayload,
  type CreatedKey,
  type KeyWebhookTestResponse,
} from '@/lib/keys'

/** 新建成功后一次性持有的信息（明文 key + 名称，用于展示与复制） */
interface CreatedKeyInfo extends CreatedKey {
  name: string
}

/**
 * 解析限额输入（B5）：空串 = 0（不限）；必须是十进制非负整数，否则返回 null
 * （由调用方统一报错）。Number 上限内的整数均接受，与后端 int 语义对齐。
 */
function parseLimit(raw: string): number | null {
  const s = raw.trim()
  if (s === '') return 0
  if (!/^\d+$/.test(s)) return null
  const n = Number(s)
  return Number.isSafeInteger(n) ? n : null
}

/** 限额输入框公共属性（type=number 仍允许键入任意字符，校验交给 parseLimit；placeholder 文案由使用处经 t() 传入） */
const limitInputProps = { type: 'number', min: 0, step: 1 } as const

/* ---------------------------------- 新建 Key ---------------------------------- */

function CreateKeyModal({ onCreated, onClose }: { onCreated: (key: CreatedKeyInfo) => void; onClose: () => void }) {
  const { t } = useTranslation(['keys', 'common'])
  const [name, setName] = useState('')
  const [dailyQuota, setDailyQuota] = useState('')
  const [ratePerSec, setRatePerSec] = useState('')
  const [error, setError] = useState<string | null>(null)
  const queryClient = useQueryClient()

  const mutation = useMutation({
    mutationFn: (payload: ApiKeyCreatePayload) => api<CreatedKey>('/api/keys', { method: 'POST', body: payload }),
    onSuccess: (res, vars) => {
      invalidateKeys(queryClient)
      onCreated({ ...res, name: vars.name })
    },
    onError: (err) => setError(err.message),
  })

  function handleSubmit(e: FormEvent<HTMLFormElement>) {
    e.preventDefault()
    const trimmed = name.trim()
    if (!trimmed) {
      setError(t('form.nameRequired'))
      return
    }
    if (trimmed.length > 64) {
      setError(t('form.nameTooLong'))
      return
    }
    const quota = parseLimit(dailyQuota)
    const rate = parseLimit(ratePerSec)
    if (quota === null || rate === null) {
      setError(t('form.limitInvalidCreate'))
      return
    }
    setError(null)
    mutation.mutate({ name: trimmed, daily_quota: quota, rate_per_sec: rate })
  }

  return (
    <Modal
      open
      onClose={onClose}
      title={t('create.title')}
      footer={
        <>
          <Button variant="secondary" onClick={onClose} disabled={mutation.isPending}>
            {t('common:actions.cancel')}
          </Button>
          <Button type="submit" form="create-key-form" loading={mutation.isPending}>
            {t('actions.createSubmit')}
          </Button>
        </>
      }
    >
      <form id="create-key-form" onSubmit={handleSubmit} className="space-y-3" noValidate>
        <Field label={t('common:fields.name')} error={error ?? undefined} hint={t('form.nameHint')}>
          <Input
            name="name"
            value={name}
            onChange={(e) => {
              setName(e.target.value)
              setError(null)
            }}
            placeholder="my-app"
            autoFocus
            autoComplete="off"
          />
        </Field>
        <div className="grid grid-cols-2 gap-3">
          <Field label={t('form.dailyQuotaLabel')} hint={t('form.dailyQuotaHint')}>
            <Input
              {...limitInputProps}
              placeholder={t('form.limitPlaceholder')}
              name="daily_quota"
              value={dailyQuota}
              onChange={(e) => {
                setDailyQuota(e.target.value)
                setError(null)
              }}
              autoComplete="off"
            />
          </Field>
          <Field label={t('form.rateLabel')} hint={t('form.rateHint')}>
            <Input
              {...limitInputProps}
              placeholder={t('form.limitPlaceholder')}
              name="rate_per_sec"
              value={ratePerSec}
              onChange={(e) => {
                setRatePerSec(e.target.value)
                setError(null)
              }}
              autoComplete="off"
            />
          </Field>
        </div>
        <p className="text-xs leading-relaxed text-slate-500 dark:text-slate-400">
          {t('form.createNote')}
        </p>
      </form>
    </Modal>
  )
}

/* ---------------------------------- 明文 Key 一次性展示 ---------------------------------- */

function RevealKeyModal({ created, onClose }: { created: CreatedKeyInfo; onClose: () => void }) {
  const { t } = useTranslation(['keys', 'common'])
  // 关闭前二次确认：X / 遮罩 / Esc 均先进入确认态
  const [confirmingClose, setConfirmingClose] = useState(false)
  const [copied, setCopied] = useState(false)

  // copied 状态 2 秒后自动复位（审计 P3-11）
  useEffect(() => {
    if (!copied) return
    const timer = window.setTimeout(() => setCopied(false), 2000)
    return () => window.clearTimeout(timer)
  }, [copied])

  async function handleCopy() {
    try {
      await copyText(created.key)
      setCopied(true)
      toast.success(t('common:actions.copied'))
    } catch {
      toast.error(t('toast.copyFailedManual'))
    }
  }

  return (
    <Modal
      open
      onClose={() => setConfirmingClose(true)}
      title={t('reveal.title')}
      footer={
        confirmingClose ? (
          <>
            <Button variant="secondary" onClick={() => setConfirmingClose(false)}>
              {t('actions.backToView')}
            </Button>
            <Button variant="danger" onClick={onClose}>
              {t('actions.confirmCloseNoShow')}
            </Button>
          </>
        ) : (
          <>
            <Button variant="secondary" onClick={handleCopy}>
              {copied ? (
                <Check className="h-4 w-4 text-emerald-500" aria-hidden />
              ) : (
                <Copy className="h-4 w-4" aria-hidden />
              )}
              {copied ? t('actions.copied') : t('actions.copyKey')}
            </Button>
            <Button onClick={() => setConfirmingClose(true)}>{t('actions.savedClose')}</Button>
          </>
        )
      }
    >
      <div className="space-y-3">
        <p className="text-sm text-slate-600 dark:text-slate-300">
          {t('reveal.bodyPrefix')}{' '}
          <span className="font-medium text-slate-900 dark:text-white">{t('nameQuoted', { name: created.name })}</span>{' '}
          {t('reveal.bodySuffix')}
        </p>
        <div className="rounded-lg border border-slate-200 bg-slate-50 p-3 dark:border-slate-700 dark:bg-slate-800">
          <code className="select-all break-all font-mono text-sm text-slate-900 dark:text-slate-100">
            {created.key}
          </code>
        </div>
        <div className="rounded-lg border border-amber-200 bg-amber-50 p-3 text-xs leading-relaxed text-amber-700 dark:border-amber-500/30 dark:bg-amber-500/10 dark:text-amber-400">
          {t('reveal.warning')}
        </div>
        {confirmingClose && (
          <p className="flex items-start gap-1.5 text-xs text-red-600 dark:text-red-400">
            <AlertCircle className="mt-0.5 h-3.5 w-3.5 shrink-0" aria-hidden />
            {t('reveal.closeWarning')}
          </p>
        )}
      </div>
    </Modal>
  )
}

/* ---------------------------------- 删除确认 ---------------------------------- */

function DeleteKeyModal({ apiKey, onClose }: { apiKey: ApiKeyItem; onClose: () => void }) {
  const { t } = useTranslation(['keys', 'common'])
  const queryClient = useQueryClient()
  const mutation = useMutation({
    mutationFn: () => api<{ ok: boolean }>(`/api/keys/${apiKey.id}`, { method: 'DELETE' }),
    onSuccess: () => {
      toast.success(t('toast.deleted', { name: apiKey.name }))
      invalidateKeys(queryClient)
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
            {t('actions.confirmDelete')}
          </Button>
        </>
      }
    >
      <p className="text-sm text-slate-600 dark:text-slate-300">
        {t('delete.confirmPrefix')}{' '}
        <span className="font-medium text-slate-900 dark:text-white">{t('nameQuoted', { name: apiKey.name })}</span>
        {t('delete.maskOpen')}
        <code className="font-mono text-xs">{apiKey.masked_key}</code>
        {t('delete.confirmSuffix')}
      </p>
      <p className="mt-2 text-xs text-slate-500 dark:text-slate-400">{t('delete.note')}</p>
    </Modal>
  )
}

/* ---------------------------------- 编辑回执配置 ---------------------------------- */

/**
 * 编辑 Key（D1 回执配置 + B5 调用方限额 + 回执签名密钥，安全加固三件套 B）：
 * webhook URL + 事件复选框，显式 opt-in——两项都配置（URL 非空且事件至少勾选
 * 一项）后，对应事件才会 POST JSON 到该地址；日额度/限速为该 Key 的调用方维度
 * 配额（0=不限）。保存按指针语义全量提交四个字段（未改动项原值回传 = 保留）。
 * 「发送测试」实测已保存的回执地址（与 SettingsPage 通知测试同款交互：恒 200，
 * 成败内联展示；输入变化即清除旧结果，loading 期间禁用防重复提交）。
 * 「签名密钥」行展示/复制当前回执签名密钥（创建 Key 即自动生成），轮换后接收方
 * 需同步更新校验密钥。
 */
function EditKeyModal({ apiKey, onClose }: { apiKey: ApiKeyItem; onClose: () => void }) {
  const { t } = useTranslation(['keys', 'common'])
  const queryClient = useQueryClient()
  const [webhookUrl, setWebhookUrl] = useState(apiKey.webhook_url)
  const [webhookSecret, setWebhookSecret] = useState(apiKey.webhook_secret)
  const [secretCopied, setSecretCopied] = useState(false)
  const [events, setEvents] = useState<Set<string>>(() => parseWebhookEvents(apiKey.webhook_events))
  const [dailyQuota, setDailyQuota] = useState(() => String(apiKey.daily_quota ?? 0))
  const [ratePerSec, setRatePerSec] = useState(() => String(apiKey.rate_per_sec ?? 0))
  const [error, setError] = useState<string | null>(null)
  const [testResult, setTestResult] = useState<KeyWebhookTestResponse | null>(null)
  const [testError, setTestError] = useState<string | null>(null)

  const secretMutation = useMutation({
    mutationFn: () => resetKeyWebhookSecret(apiKey.id),
    onSuccess: (res) => {
      setWebhookSecret(res.webhook_secret)
      setSecretCopied(false)
      queryClient.invalidateQueries({ queryKey: KEYS_QUERY_KEY })
      toast.success(t('toast.secretRotated'))
    },
    onError: (err) => toast.error(err.message),
  })

  const mutation = useMutation({
    mutationFn: (payload: ApiKeyUpdatePayload) =>
      api<ApiKeyItem>(`/api/keys/${apiKey.id}`, { method: 'PUT', body: payload }),
    onSuccess: () => {
      toast.success(t('toast.configSaved', { name: apiKey.name }))
      invalidateKeys(queryClient)
      onClose()
    },
    onError: (err) => setError(err.message),
  })

  const testMutation = useMutation({
    mutationFn: () => testKeyWebhook(apiKey.id),
    onSuccess: (res) => {
      setTestResult(res)
      setTestError(null)
    },
    onError: (err) => {
      setTestResult(null)
      setTestError(err.message)
    },
  })

  /** 输入已变化，旧测试结果不再代表当前配置（与 SettingsPage setUrl 同款） */
  function clearTestState() {
    setTestResult(null)
    setTestError(null)
  }

  function toggleEvent(key: string, checked: boolean) {
    setEvents((prev) => {
      const next = new Set(prev)
      if (checked) next.add(key)
      else next.delete(key)
      return next
    })
  }

  function handleSubmit(e: FormEvent<HTMLFormElement>) {
    e.preventDefault()
    const url = webhookUrl.trim()
    if (url && !/^https?:\/\//i.test(url)) {
      setError(t('edit.urlRequired'))
      return
    }
    const quota = parseLimit(dailyQuota)
    const rate = parseLimit(ratePerSec)
    if (quota === null || rate === null) {
      setError(t('form.limitInvalidEdit'))
      return
    }
    setError(null)
    // 事件按声明顺序拼接为 csv（与后端规范化口径一致）；URL 清空时事件一并清除。
    // 限额两字段恒随表单提交：显式 0 = 改为不限（后端指针语义，未提供才保留）。
    const base: ApiKeyUpdatePayload = url
      ? { webhook_url: url, webhook_events: WEBHOOK_EVENTS.filter((meta) => events.has(meta.key)).map((meta) => meta.key).join(',') }
      : { webhook_url: '', webhook_events: '' }
    mutation.mutate({ ...base, daily_quota: quota, rate_per_sec: rate })
  }

  return (
    <Modal
      open
      onClose={onClose}
      title={t('edit.title', { name: apiKey.name })}
      footer={
        <>
          <p className="mr-auto hidden text-xs text-slate-500 sm:block dark:text-slate-400">
            {t('edit.testHint')}
          </p>
          <Button variant="secondary" onClick={onClose} disabled={mutation.isPending}>
            {t('common:actions.cancel')}
          </Button>
          <Button
            type="button"
            variant="secondary"
            loading={testMutation.isPending}
            disabled={webhookUrl.trim() === ''}
            onClick={() => testMutation.mutate()}
          >
            {t('actions.sendTest')}
          </Button>
          <Button type="submit" form="edit-key-webhook-form" loading={mutation.isPending}>
            {t('common:actions.save')}
          </Button>
        </>
      }
    >
      <form id="edit-key-webhook-form" onSubmit={handleSubmit} className="space-y-3" noValidate>
        <fieldset>
          <legend className="mb-1.5 text-sm font-medium text-slate-700 dark:text-slate-300">
            {t('edit.callerLimits')}
          </legend>
          <div className="grid grid-cols-2 gap-3">
            <Field label={t('form.dailyQuotaLabel')} hint={t('form.dailyQuotaHint')}>
              <Input
                {...limitInputProps}
              placeholder={t('form.limitPlaceholder')}
                name="daily_quota"
                value={dailyQuota}
                onChange={(e) => {
                  setDailyQuota(e.target.value)
                  setError(null)
                }}
                autoComplete="off"
              />
            </Field>
            <Field label={t('form.rateLabel')} hint={t('form.rateHint')}>
              <Input
                {...limitInputProps}
              placeholder={t('form.limitPlaceholder')}
                name="rate_per_sec"
                value={ratePerSec}
                onChange={(e) => {
                  setRatePerSec(e.target.value)
                  setError(null)
                }}
                autoComplete="off"
              />
            </Field>
          </div>
        </fieldset>
        <p className="text-xs leading-relaxed text-slate-500 dark:text-slate-400">
          {t('edit.webhookDesc')}
        </p>
        <Field
          label="Webhook URL"
          error={error ?? undefined}
          hint={t('edit.webhookUrlHint')}
        >
          <Input
            name="webhook_url"
            type="url"
            value={webhookUrl}
            onChange={(e) => {
              setWebhookUrl(e.target.value)
              setError(null)
              clearTestState()
            }}
            placeholder="https://example.com/hooks/catmail"
            autoComplete="off"
          />
        </Field>
        {/* 回执签名密钥（安全加固三件套，B）：创建 Key 自动生成，可复制/轮换 */}
        <div className="rounded-lg border border-border-subtle px-3 py-3">
          <div className="flex items-start justify-between gap-3">
            <div className="min-w-0">
              <p className="text-sm font-medium text-slate-700 dark:text-slate-300">{t('edit.secretTitle')}</p>
              <p className="mt-0.5 text-xs leading-relaxed text-slate-500 dark:text-slate-400">
                {t('edit.secretDesc')}
              </p>
            </div>
            <Button
              type="button"
              variant="secondary"
              className="shrink-0"
              loading={secretMutation.isPending}
              onClick={() => {
                if (window.confirm(t('edit.secretRotateConfirm'))) {
                  secretMutation.mutate()
                }
              }}
            >
              {t('actions.rotate')}
            </Button>
          </div>
          {webhookSecret && (
            <div className="mt-2.5 flex items-center gap-2">
              <code className="min-w-0 flex-1 truncate rounded-md border border-border-subtle bg-slate-50 px-2.5 py-1.5 font-mono text-xs text-slate-700 dark:bg-slate-900/40 dark:text-slate-300">
                {webhookSecret}
              </code>
              <Button
                type="button"
                variant="ghost"
                className="h-7 shrink-0 px-2 text-xs"
                onClick={async () => {
                  try {
                    await copyText(webhookSecret)
                    setSecretCopied(true)
                    toast.success(t('common:actions.copied'))
                  } catch {
                    toast.error(t('toast.copyFailedManual'))
                  }
                }}
              >
                {secretCopied ? (
                  <Check className="h-3.5 w-3.5 text-emerald-500" aria-hidden />
                ) : (
                  <Copy className="h-3.5 w-3.5" aria-hidden />
                )}
                {secretCopied ? t('actions.copied') : t('common:actions.copy')}
              </Button>
            </div>
          )}
        </div>
        <fieldset>
          <legend className="mb-1.5 text-sm font-medium text-slate-700 dark:text-slate-300">
            {t('edit.eventsTitle')}
          </legend>
          <div className="grid grid-cols-1 gap-2">
            {WEBHOOK_EVENTS.map((meta) => (
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
                  <span className="block text-sm font-medium text-slate-700 dark:text-slate-300">
                    {t(meta.labelKey)}
                    <code className="ml-1.5 font-mono text-xs text-slate-500 dark:text-slate-400">
                      {meta.key}
                    </code>
                  </span>
                  <span className="mt-0.5 block text-xs text-slate-500 dark:text-slate-400">
                    {t(meta.descriptionKey)}
                  </span>
                </span>
              </label>
            ))}
          </div>
          {webhookUrl.trim() !== '' && events.size === 0 && (
            <p className="mt-2 flex items-start gap-1.5 text-xs text-amber-600 dark:text-amber-400">
              <AlertCircle className="mt-0.5 h-3.5 w-3.5 shrink-0" aria-hidden />
              {t('edit.eventsWarning')}
            </p>
          )}
        </fieldset>
        {/* 测试结果内联展示（对齐 SettingsPage notify/test 交互） */}
        {testResult && (
          <p className="flex items-start gap-1.5 rounded-lg border border-border-subtle bg-slate-50 px-3 py-2.5 text-sm dark:bg-slate-900/40">
            {testResult.ok ? (
              <Check className="mt-0.5 h-4 w-4 shrink-0 text-emerald-500" aria-hidden />
            ) : (
              <X className="mt-0.5 h-4 w-4 shrink-0 text-red-500" aria-hidden />
            )}
            <span className="text-slate-600 dark:text-slate-300">
              {t('edit.testPushLabel')}
              {testResult.ok
                ? t('common:state.success')
                : testResult.error
                  ? t('edit.testFailedWithError', { error: testResult.error })
                  : t('common:state.failed')}
            </span>
          </p>
        )}
        {testError && (
          <p className="flex items-start gap-1.5 text-sm text-red-600 dark:text-red-400">
            <AlertCircle className="mt-0.5 h-4 w-4 shrink-0" aria-hidden />
            {testError}
          </p>
        )}
      </form>
    </Modal>
  )
}

/* ---------------------------------- 页面 ---------------------------------- */

export function KeysPage() {
  const { t } = useTranslation(['keys', 'common'])
  const queryClient = useQueryClient()
  const keysQuery = useApiKeys()
  const [createOpen, setCreateOpen] = useState(false)
  const [created, setCreated] = useState<CreatedKeyInfo | null>(null)
  const [deleteTarget, setDeleteTarget] = useState<ApiKeyItem | null>(null)
  const [editTarget, setEditTarget] = useState<ApiKeyItem | null>(null)

  const toggleMutation = useMutation({
    mutationFn: ({ id, enabled }: { id: number; enabled: boolean }) =>
      api<ApiKeyItem>(`/api/keys/${id}`, { method: 'PUT', body: { enabled } }),
    onSuccess: (_data, vars) => {
      toast.success(vars.enabled ? t('toast.enabled') : t('toast.disabled'))
      invalidateKeys(queryClient)
    },
    onError: (err) => toast.error(err.message),
  })

  const items = keysQuery.data?.items ?? []

  return (
    <div className="space-y-6">
      <PageHeader
        title="API Keys"
        description={t('page.description')}
        actions={
          <Button onClick={() => setCreateOpen(true)}>
            <Plus className="h-4 w-4" aria-hidden />
            {t('actions.createKey')}
          </Button>
        }
      />

      <Card>
        {keysQuery.isPending ? (
          <QueryLoading skeleton="table" />
        ) : keysQuery.isError ? (
          <QueryError message={keysQuery.error.message} onRetry={() => keysQuery.refetch()} />
        ) : items.length === 0 ? (
          <EmptyState
            icon={KeyRound}
            title={t('empty.title')}
            description={t('empty.description')}
            action={
              <Button onClick={() => setCreateOpen(true)}>
                <Plus className="h-4 w-4" aria-hidden />
                {t('actions.createKey')}
              </Button>
            }
          />
        ) : (
          <TableScroll label={t('table.label')}>
            <Table label={t('table.label')}>
              <THead>
                <tr>
                  <TH>{t('common:fields.name')}</TH>
                  <TH>Key</TH>
                  <TH>{t('common:fields.status')}</TH>
                  <TH numeric>{t('table.quota')}</TH>
                  <TH numeric>{t('table.totalSent')}</TH>
                  <TH>{t('table.lastUsed')}</TH>
                  <TH>{t('common:fields.createdAt')}</TH>
                  <TH className="text-right">{t('common:fields.actions')}</TH>
                </tr>
              </THead>
              <TBody>
                {items.map((apiKey) => {
                  const togglePending =
                    toggleMutation.isPending && toggleMutation.variables?.id === apiKey.id
                  return (
                    <TRow key={apiKey.id}>
                      <TD className="max-w-48">
                        <p
                          className="truncate font-medium text-slate-900 dark:text-white"
                          title={apiKey.name}
                        >
                          {apiKey.name}
                        </p>
                      </TD>
                      <TD>
                        <code className="font-mono text-xs text-slate-600 dark:text-slate-300">
                          {apiKey.masked_key}
                        </code>
                      </TD>
                      <TD>
                        <div className="flex flex-col items-start gap-1">
                          <Badge variant={apiKey.enabled ? 'success' : 'neutral'}>
                            {apiKey.enabled ? t('table.badgeActive') : t('table.badgeInactive')}
                          </Badge>
                          {/* 联动停用徽标（安全加固三件套，C）：密码重置联动停用的 Key
                              提示原因，可手动启用恢复 */}
                          {!apiKey.enabled && apiKey.disabled_reason === 'password_reset' && (
                            <span
                              className="inline-flex items-center rounded-md border border-amber-200 bg-amber-50 px-1.5 py-0.5 text-[11px] leading-4 text-amber-700 dark:border-amber-500/30 dark:bg-amber-500/10 dark:text-amber-400"
                              title={t('table.passwordResetTitle')}
                            >
                              {t('table.passwordResetBadge')}
                            </span>
                          )}
                        </div>
                      </TD>
                      {/* 限额（B5）：仅展示非零值，两项皆 0（不限）显示灰色「不限」 */}
                      <TD numeric className="text-slate-600 dark:text-slate-300">
                        {apiKey.daily_quota > 0 || apiKey.rate_per_sec > 0 ? (
                          <span className="text-xs">
                            {[
                              apiKey.daily_quota > 0
                                ? t('table.perDay_other', { count: apiKey.daily_quota })
                                : null,
                              apiKey.rate_per_sec > 0 ? `${apiKey.rate_per_sec}/s` : null,
                            ]
                              .filter(Boolean)
                              .join(' · ')}
                          </span>
                        ) : (
                          <span className="text-xs text-slate-500 dark:text-slate-400">{t('table.unlimited')}</span>
                        )}
                      </TD>
                      <TD numeric className="text-slate-600 dark:text-slate-300">
                        {(apiKey.total_sent ?? 0).toLocaleString()}
                      </TD>
                      <TD>
                        {apiKey.last_used_at ? (
                          <RelativeTime iso={apiKey.last_used_at} />
                        ) : (
                          <span className="text-slate-500 dark:text-slate-400" title={t('table.neverUsed')}>
                            —
                          </span>
                        )}
                      </TD>
                      <TD>
                        <RelativeTime iso={apiKey.created_at} />
                      </TD>
                      <TD className="text-right">
                        <div className="flex items-center justify-end gap-1">
                          <Button
                            variant="ghost"
                            className="h-7 px-2 text-xs"
                            onClick={() => setEditTarget(apiKey)}
                          >
                            {t('common:actions.edit')}
                          </Button>
                          <Button
                            variant="ghost"
                            loading={togglePending}
                            className="h-7 px-2 text-xs"
                            onClick={() =>
                              toggleMutation.mutate({ id: apiKey.id, enabled: !apiKey.enabled })
                            }
                          >
                            {apiKey.enabled ? t('common:state.disabled') : t('common:state.enabled')}
                          </Button>
                          <button
                            type="button"
                            onClick={() => setDeleteTarget(apiKey)}
                            className="inline-flex h-7 items-center rounded-lg px-2 text-xs text-red-600 transition-colors hover:bg-red-50 hover:text-red-700 dark:text-red-400 dark:hover:bg-red-500/10 dark:hover:text-red-300"
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
          </TableScroll>
        )}
      </Card>

      {createOpen && (
        <CreateKeyModal
          onCreated={(info) => {
            setCreateOpen(false)
            setCreated(info)
          }}
          onClose={() => setCreateOpen(false)}
        />
      )}
      {created && <RevealKeyModal created={created} onClose={() => setCreated(null)} />}
      {deleteTarget && (
        <DeleteKeyModal apiKey={deleteTarget} onClose={() => setDeleteTarget(null)} />
      )}
      {editTarget && <EditKeyModal apiKey={editTarget} onClose={() => setEditTarget(null)} />}
    </div>
  )
}
