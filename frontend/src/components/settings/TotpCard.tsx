/**
 * 两步验证（TOTP，C4）设置卡片：
 * - 未启用 → 「开始绑定」弹窗（QR 码 + secret 手输备用 + 输入 6 位码确认）
 *   → 成功后进入恢复码展示弹窗（一次性、复制按钮、强制勾选确认已保存才能关闭）
 * - 已启用 → 「已启用」徽章 + 「解除绑定」弹窗（输密码，不用 TOTP 码——手机丢了也能解绑）
 * - 恢复码剩余数量不展示（不泄露库存状态）
 * 数据：POST /api/auth/totp/setup|enable|disable（lib/totp.ts），启用状态取 /api/auth/me。
 */
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { AlertCircle, Check, Copy, ShieldCheck } from 'lucide-react'
import QRCode from 'qrcode'
import { useEffect, useState, type FormEvent } from 'react'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import { Badge } from '@/components/badges'
import { Button } from '@/components/ui/Button'
import { Card } from '@/components/ui/Card'
import { Field, Input } from '@/components/ui/Input'
import { Modal } from '@/components/ui/Modal'
import { ME_QUERY_KEY, useMe } from '@/lib/auth'
import { copyText } from '@/lib/clipboard'
import { totpDisable, totpEnable, totpSetup, type TotpSetupResponse } from '@/lib/totp'
import { cn } from '@/lib/utils'

/* ---------------------------------- 二维码 ---------------------------------- */

/** otpauth URL → data URL 二维码（qrcode 本地生成，不出网） */
function QrImage({ text }: { text: string }) {
  const { t } = useTranslation(['settings', 'common'])
  const [src, setSrc] = useState<string | null>(null)
  const [failed, setFailed] = useState(false)

  useEffect(() => {
    let alive = true
    setSrc(null)
    setFailed(false)
    QRCode.toDataURL(text, { width: 192, margin: 1 })
      .then((url) => {
        if (alive) setSrc(url)
      })
      .catch(() => {
        if (alive) setFailed(true)
      })
    return () => {
      alive = false
    }
  }, [text])

  if (failed) {
    return (
      <p className="flex h-48 w-48 items-center justify-center rounded-lg border border-border-subtle text-xs text-slate-500 dark:text-slate-400">
        {t('totp.qrFailed')}
      </p>
    )
  }
  if (!src) {
    return <div className="h-48 w-48 animate-pulse rounded-lg bg-slate-100 dark:bg-slate-800" />
  }
  return (
    <img
      src={src}
      alt={t('totp.qrAlt')}
      className="h-48 w-48 rounded-lg border border-border-subtle bg-white p-1.5"
    />
  )
}

/* ---------------------------------- 绑定弹窗 ---------------------------------- */

/** 绑定弹窗：setup → 展示 QR + secret 手输备用 → 输 6 位码确认 enable */
function TotpSetupModal({
  open,
  onClose,
  onEnabled,
}: {
  open: boolean
  onClose: () => void
  onEnabled: (codes: string[]) => void
}) {
  const { t } = useTranslation(['settings', 'common'])
  const [setup, setSetup] = useState<TotpSetupResponse | null>(null)
  const [code, setCode] = useState('')
  const [error, setError] = useState<string | undefined>(undefined)

  const setupMutation = useMutation({
    mutationFn: totpSetup,
    onSuccess: (data) => {
      setSetup(data)
      setError(undefined)
    },
    onError: (err) => toast.error(err.message),
  })

  const enableMutation = useMutation({
    mutationFn: () => totpEnable(code.trim()),
    onSuccess: (data) => {
      setSetup(null)
      setCode('')
      setError(undefined)
      onEnabled(data.recovery_codes)
    },
    onError: (err) => {
      setError(err.message)
    },
  })

  // 打开即发起 setup（与命令面板等「打开即加载」一致）：每次打开都重取，
  // 不留上次未用完的旧 secret；失败后关闭重开亦会重新发起
  useEffect(() => {
    if (!open) return
    setSetup(null)
    setCode('')
    setError(undefined)
    setupMutation.mutate()
    // setupMutation 为稳定引用，仅需随 open 变化触发
  }, [open])

  function handleClose() {
    // 半途关闭即放弃本次绑定（pending secret 未生效，可随时重新开始）
    setSetup(null)
    setCode('')
    setError(undefined)
    onClose()
  }

  function handleSubmit(e: FormEvent<HTMLFormElement>) {
    e.preventDefault()
    if (!/^\d{6}$/.test(code.trim())) {
      setError(t('totp.codeInvalid'))
      return
    }
    enableMutation.mutate()
  }

  return (
    <Modal
      open={open}
      onClose={handleClose}
      title={setup ? t('totp.setupTitle') : t('totp.start')}
      className="max-w-md"
    >
      {setupMutation.isPending ? (
        <div className="flex h-48 items-center justify-center">
          <div className="h-48 w-48 animate-pulse rounded-lg bg-slate-100 dark:bg-slate-800" />
        </div>
      ) : setupMutation.isError ? (
        <div className="flex flex-col gap-4 py-2">
          {/* 全局错误（非字段级）：role="alert" 让读屏立即播报 */}
          <p role="alert" className="flex items-start gap-1.5 text-sm text-red-600 dark:text-red-400">
            <AlertCircle className="mt-0.5 h-4 w-4 shrink-0" aria-hidden />
            {setupMutation.error.message}
          </p>
          <div className="flex justify-end gap-2">
            <Button type="button" variant="secondary" onClick={handleClose}>
              {t('common:actions.close')}
            </Button>
            <Button type="button" onClick={() => setupMutation.mutate()}>
              {t('common:actions.retry')}
            </Button>
          </div>
        </div>
      ) : setup ? (
        <form onSubmit={handleSubmit} className="space-y-4" noValidate>
          <div className="flex flex-col items-center gap-3">
            <QrImage text={setup.otpauth_url} />
            <p className="text-center text-xs text-slate-500 dark:text-slate-400">
              {t('totp.scanHint')}
            </p>
            <div className="w-full rounded-lg border border-border-subtle bg-slate-50 px-3 py-2 dark:bg-slate-900/40">
              <p className="text-xs text-slate-500 dark:text-slate-400">{t('totp.secretLabel')}</p>
              <p className="mt-0.5 select-all break-all font-mono text-sm text-slate-800 dark:text-slate-200">
                {setup.secret}
              </p>
            </div>
          </div>
          <Field label={t('totp.codeLabel')} error={error}>
            <Input
              name="code"
              value={code}
              onChange={(e) => {
                setCode(e.target.value)
                setError(undefined)
              }}
              placeholder={t('totp.codePlaceholder')}
              inputMode="numeric"
              autoComplete="one-time-code"
              maxLength={6}
              autoFocus
            />
          </Field>
          <div className="flex justify-end gap-2">
            <Button type="button" variant="secondary" onClick={handleClose}>
              {t('common:actions.cancel')}
            </Button>
            <Button type="submit" loading={enableMutation.isPending}>
              {t('totp.confirmEnable')}
            </Button>
          </div>
        </form>
      ) : null}
    </Modal>
  )
}

/* ---------------------------------- 恢复码弹窗 ---------------------------------- */

/**
 * 恢复码展示弹窗：明文仅此一次。强制确认已保存——未勾选时遮罩/Esc/关闭按钮
 * 均不关闭（提示先保存），确认后才能「完成」。
 */
function TotpRecoveryModal({
  codes,
  onDone,
}: {
  codes: string[] | null
  onDone: () => void
}) {
  const { t } = useTranslation(['settings', 'common'])
  const [saved, setSaved] = useState(false)
  const [copied, setCopied] = useState(false)
  const open = codes !== null

  function guardClose() {
    if (!saved) {
      toast.warning(t('totp.saveFirstWarning'))
      return
    }
    setSaved(false)
    setCopied(false)
    onDone()
  }

  async function copyAll() {
    if (!codes) return
    try {
      // 共享降级实现：非安全源（http）下自动落到 execCommand
      await copyText(codes.join('\n'))
      setCopied(true)
      toast.success(t('totp.recoveryCopied'))
    } catch {
      toast.error(t('totp.copyFailed'))
    }
  }

  return (
    <Modal
      open={open}
      onClose={guardClose}
      title={t('totp.recoveryTitle')}
      className="max-w-md"
    >
      <div className="space-y-4">
        <p className="flex items-start gap-1.5 rounded-lg border border-amber-300 bg-amber-50 px-3 py-2.5 text-xs text-amber-800 dark:border-amber-400/30 dark:bg-amber-400/10 dark:text-amber-200">
          <AlertCircle className="mt-0.5 h-3.5 w-3.5 shrink-0" aria-hidden />
          {t('totp.recoveryWarning')}
        </p>
        <div className="grid grid-cols-2 gap-2">
          {codes?.map((code, i) => (
            <div
              key={code}
              className={cn(
                'flex items-center justify-between rounded-lg border border-border-subtle bg-slate-50 px-3 py-2',
                'font-mono text-sm tracking-wider text-slate-800 dark:bg-slate-900/40 dark:text-slate-200',
              )}
            >
              <span className="select-all">{code}</span>
              <span className="text-xs text-slate-500 dark:text-slate-400">{i + 1}</span>
            </div>
          ))}
        </div>
        <Button type="button" variant="secondary" className="w-full" onClick={() => void copyAll()}>
          {copied ? (
            <>
              <Check className="h-4 w-4" aria-hidden /> {t('copied')}
            </>
          ) : (
            <>
              <Copy className="h-4 w-4" aria-hidden /> {t('totp.copyAll')}
            </>
          )}
        </Button>
        <label className="flex cursor-pointer items-start gap-2.5 text-sm text-slate-700 dark:text-slate-300">
          <input
            type="checkbox"
            className="mt-0.5 h-4 w-4 shrink-0 accent-sky-600"
            checked={saved}
            onChange={(e) => setSaved(e.target.checked)}
          />
          {t('totp.savedConfirm')}
        </label>
        <Button
          type="button"
          className="w-full"
          disabled={!saved}
          onClick={guardClose}
        >
          {t('totp.done')}
        </Button>
      </div>
    </Modal>
  )
}

/* ---------------------------------- 解绑弹窗 ---------------------------------- */

/** 解绑弹窗：凭当前密码解绑（不用 TOTP 码——手机丢了也能解绑） */
function TotpDisableModal({
  open,
  onClose,
  onDisabled,
}: {
  open: boolean
  onClose: () => void
  onDisabled: () => void
}) {
  const { t } = useTranslation(['settings', 'common'])
  const [password, setPassword] = useState('')
  const [error, setError] = useState<string | undefined>(undefined)

  const mutation = useMutation({
    mutationFn: () => totpDisable(password),
    onSuccess: () => {
      setPassword('')
      setError(undefined)
      onDisabled()
    },
    onError: (err) => setError(err.message),
  })

  function handleClose() {
    setPassword('')
    setError(undefined)
    onClose()
  }

  function handleSubmit(e: FormEvent<HTMLFormElement>) {
    e.preventDefault()
    if (!password) {
      setError(t('totp.passwordRequired'))
      return
    }
    mutation.mutate()
  }

  return (
    <Modal open={open} onClose={handleClose} title={t('totp.disableTitle')} className="max-w-sm">
      <form onSubmit={handleSubmit} className="space-y-4" noValidate>
        <p className="text-sm text-slate-500 dark:text-slate-400">
          {t('totp.disableHint')}
        </p>
        <Field label={t('currentPassword')} error={error}>
          <Input
            name="password"
            type="password"
            value={password}
            onChange={(e) => {
              setPassword(e.target.value)
              setError(undefined)
            }}
            placeholder={t('currentPassword')}
            autoComplete="current-password"
            autoFocus
          />
        </Field>
        <div className="flex justify-end gap-2">
          <Button type="button" variant="secondary" onClick={handleClose}>
            {t('common:actions.cancel')}
          </Button>
          <Button type="submit" variant="danger" loading={mutation.isPending}>
            {t('totp.unbind')}
          </Button>
        </div>
      </form>
    </Modal>
  )
}

/* ---------------------------------- 卡片 ---------------------------------- */

export function TotpCard() {
  const me = useMe()
  const queryClient = useQueryClient()
  const { t } = useTranslation(['settings', 'common'])
  const [setupOpen, setSetupOpen] = useState(false)
  const [recoveryCodes, setRecoveryCodes] = useState<string[] | null>(null)
  const [disableOpen, setDisableOpen] = useState(false)

  const enabled = me.data?.totp_enabled === true

  function refreshMe() {
    // 启用/解绑后 me 缓存先行更新（totp_enabled 切换卡片形态），再失效复核；
    // 后端已重签当前会话 cookie，same-origin 自动携带，无需重新登录
    void queryClient.invalidateQueries({ queryKey: ME_QUERY_KEY })
  }

  return (
    <Card>
      <div className="border-b border-slate-200 px-5 py-4 dark:border-slate-800">
        <h2 className="flex items-center gap-2 text-sm font-semibold text-slate-900 dark:text-white">
          {t('totp.title')}
          {enabled && (
            <Badge variant="success">
              <ShieldCheck className="h-3 w-3" aria-hidden />
              {t('totp.enabledBadge')}
            </Badge>
          )}
        </h2>
        <p className="mt-0.5 text-xs text-slate-500 dark:text-slate-400">
          {enabled
            ? t('totp.descEnabled')
            : t('totp.descDisabled')}
        </p>
      </div>
      <div className="flex items-center justify-between gap-4 px-5 py-4">
        <p className="min-w-0 text-sm text-slate-600 dark:text-slate-300">
          {enabled
            ? t('totp.hintEnabled')
            : t('totp.hintDisabled')}
        </p>
        {enabled ? (
          <Button variant="secondary" className="shrink-0" onClick={() => setDisableOpen(true)}>
            {t('totp.unbind')}
          </Button>
        ) : (
          <Button className="shrink-0" onClick={() => setSetupOpen(true)}>
            {t('totp.start')}
          </Button>
        )}
      </div>

      <TotpSetupModal
        open={setupOpen}
        onClose={() => setSetupOpen(false)}
        onEnabled={(codes) => {
          setSetupOpen(false)
          setRecoveryCodes(codes)
          refreshMe()
        }}
      />
      <TotpRecoveryModal
        codes={recoveryCodes}
        onDone={() => {
          setRecoveryCodes(null)
          toast.success(t('totp.enabledToast'))
        }}
      />
      <TotpDisableModal
        open={disableOpen}
        onClose={() => setDisableOpen(false)}
        onDisabled={() => {
          setDisableOpen(false)
          refreshMe()
          toast.success(t('totp.disabledToast'))
        }}
      />
    </Card>
  )
}
