import { AlertCircle, Cat } from 'lucide-react'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { useState, type FormEvent } from 'react'
import { Navigate, useNavigate } from 'react-router'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import { api } from '@/lib/api'
import { ME_QUERY_KEY, useMe } from '@/lib/auth'
import { loginTotp, type LoginResponse } from '@/lib/totp'
import { Button } from '@/components/ui/Button'
import { Field, Input } from '@/components/ui/Input'

interface LoginRequest {
  username: string
  password: string
}

export function LoginPage() {
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [error, setError] = useState<string | null>(null)
  const [fieldErrors, setFieldErrors] = useState<{ username?: string; password?: string }>({})
  // 两步验证（C4）：第一步密码通过后进入第二步（凭后端签发的 t| 短票据）
  const [ticket, setTicket] = useState<string | null>(null)
  const [code, setCode] = useState('')
  const [useRecovery, setUseRecovery] = useState(false)
  const navigate = useNavigate()
  const queryClient = useQueryClient()
  const me = useMe()
  const { t } = useTranslation('auth')

  const loginMutation = useMutation({
    mutationFn: (req: LoginRequest) =>
      api<LoginResponse>('/api/auth/login', { method: 'POST', body: req }),
    onSuccess: (data) => {
      // 已启用两步验证：后端不建会话，进入第二步（ticket 仅此一处持有）
      if (data.totp_required && data.ticket) {
        setTicket(data.ticket)
        setCode('')
        setUseRecovery(false)
        setError(null)
        return
      }
      finishLogin(data.username ?? '')
    },
    onError: (err) => {
      setError(err.message)
      toast.error(err.message)
    },
  })

  const totpMutation = useMutation({
    mutationFn: (tkt: string) => loginTotp(tkt, code.trim()),
    onSuccess: (data) => finishLogin(data.username),
    onError: (err) => {
      setError(err.message)
      toast.error(err.message)
    },
  })

  /** 登录完成（两步共用）：写 me 缓存保证即时进入后台；/api/auth/me 契约为
   * {id, username, totp_enabled}，登录响应只有 username —— 再失效一次让缓存补全 */
  function finishLogin(name: string) {
    queryClient.setQueryData(ME_QUERY_KEY, { username: name })
    void queryClient.invalidateQueries({ queryKey: ME_QUERY_KEY })
    toast.success(t('toast.welcome', { name }))
    navigate('/', { replace: true })
  }

  function handleSubmit(e: FormEvent<HTMLFormElement>) {
    e.preventDefault()
    setError(null)
    const nextErrors: typeof fieldErrors = {}
    if (!username.trim()) nextErrors.username = t('form.usernameRequired')
    if (!password) nextErrors.password = t('form.passwordRequired')
    setFieldErrors(nextErrors)
    if (Object.values(nextErrors).some(Boolean)) return
    loginMutation.mutate({ username: username.trim(), password })
  }

  function handleTotpSubmit(e: FormEvent<HTMLFormElement>) {
    e.preventDefault()
    setError(null)
    if (!code.trim()) {
      setError(useRecovery ? t('totp.recoveryRequired') : t('totp.codeRequired'))
      return
    }
    if (!useRecovery && !/^\d{6}$/.test(code.trim())) {
      setError(t('totp.codeInvalid'))
      return
    }
    if (ticket) totpMutation.mutate(ticket)
  }

  // 「返回重填」：丢弃票据回到第一步（密码保留，重新提交即签发新票据）
  function backToCredentials() {
    setTicket(null)
    setCode('')
    setError(null)
    totpMutation.reset()
  }

  // 已登录则直接进后台
  if (me.isSuccess) {
    return <Navigate to="/" replace />
  }

  return (
    <div className="app-login-bg flex min-h-dvh items-center justify-center bg-gradient-to-br from-sky-50 via-white to-sky-100 px-4 dark:from-slate-950 dark:via-slate-950 dark:to-sky-950/40">
      <div className="glass-tint w-full max-w-sm rounded-2xl border border-border-subtle bg-surface-raised p-8 shadow-xl shadow-slate-200/60 dark:shadow-black/20">
        {/* Logo */}
        <div className="mb-8 flex flex-col items-center gap-3">
          <div className="flex h-12 w-12 items-center justify-center rounded-2xl bg-sky-500 text-white shadow-lg shadow-sky-500/30">
            <Cat className="h-6 w-6" />
          </div>
          <div className="text-center">
            <h1 className="text-xl font-semibold tracking-tight text-slate-900 dark:text-white">
              {t('brand.title')}
            </h1>
            <p className="mt-1 text-sm text-slate-500 dark:text-slate-400">
              {t('brand.subtitle')}
            </p>
          </div>
        </div>

        {ticket === null ? (
          <form onSubmit={handleSubmit} className="space-y-4" noValidate>
            <Field label={t('form.username')} error={fieldErrors.username}>
              <Input
                value={username}
                onChange={(e) => {
                  setUsername(e.target.value)
                  setFieldErrors((prev) => ({ ...prev, username: undefined }))
                }}
                name="username"
                placeholder={t('form.username')}
                autoComplete="username"
                autoCapitalize="none"
                autoCorrect="off"
                spellCheck={false}
                enterKeyHint="next"
                autoFocus
              />
            </Field>
            <Field label={t('form.password')} error={fieldErrors.password}>
              <Input
                type="password"
                value={password}
                onChange={(e) => {
                  setPassword(e.target.value)
                  setFieldErrors((prev) => ({ ...prev, password: undefined }))
                }}
                name="password"
                placeholder={t('form.password')}
                autoComplete="current-password"
                enterKeyHint="done"
              />
            </Field>

            {error && (
              <p
                role="alert"
                className="flex items-start gap-1.5 text-sm text-red-600 dark:text-red-400"
              >
                <AlertCircle className="mt-0.5 h-4 w-4 shrink-0" aria-hidden />
                {error}
              </p>
            )}

            <Button type="submit" loading={loginMutation.isPending} className="w-full">
              {loginMutation.isPending ? t('form.submitting') : t('form.submit')}
            </Button>
          </form>
        ) : (
          <form onSubmit={handleTotpSubmit} className="space-y-4" noValidate>
            <div>
              <h2 className="text-base font-semibold text-slate-900 dark:text-white">
                {t('totp.title')}
              </h2>
              <p className="mt-1 text-sm text-slate-500 dark:text-slate-400">
                {useRecovery
                  ? t('totp.recoveryHint')
                  : t('totp.codeHint')}
              </p>
            </div>
            <Field label={useRecovery ? t('totp.recoveryCode') : t('totp.code')}>
              <Input
                value={code}
                onChange={(e) => {
                  setCode(e.target.value)
                  setError(null)
                }}
                name={useRecovery ? 'recovery_code' : 'code'}
                placeholder={useRecovery ? t('totp.recoveryPlaceholder') : t('totp.codePlaceholder')}
                inputMode={useRecovery ? 'text' : 'numeric'}
                autoComplete="one-time-code"
                autoCapitalize="none"
                autoCorrect="off"
                spellCheck={false}
                enterKeyHint="done"
                autoFocus
                maxLength={useRecovery ? 20 : 6}
              />
            </Field>

            {error && (
              <p
                role="alert"
                className="flex items-start gap-1.5 text-sm text-red-600 dark:text-red-400"
              >
                <AlertCircle className="mt-0.5 h-4 w-4 shrink-0" aria-hidden />
                {error}
              </p>
            )}

            <Button
              type="submit"
              loading={totpMutation.isPending}
              className="w-full"
            >
              {totpMutation.isPending ? t('totp.submitting') : t('totp.submit')}
            </Button>
            <div className="flex items-center justify-between text-sm">
              <button
                type="button"
                onClick={() => {
                  setUseRecovery((v) => !v)
                  setCode('')
                  setError(null)
                }}
                className="font-medium text-sky-600 underline-offset-2 transition-colors hover:text-sky-700 hover:underline dark:text-sky-400 dark:hover:text-sky-300"
              >
                {useRecovery ? t('totp.useCode') : t('totp.useRecovery')}
              </button>
              <button
                type="button"
                onClick={backToCredentials}
                className="text-slate-500 underline-offset-2 transition-colors hover:text-slate-700 hover:underline dark:text-slate-400 dark:hover:text-slate-200"
              >
                {t('totp.back')}
              </button>
            </div>
          </form>
        )}
      </div>
    </div>
  )
}
