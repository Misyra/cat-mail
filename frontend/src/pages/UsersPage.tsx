/**
 * 账号管理页（SPEC §6）：账号列表 + 新建账号（用户名 2-32、密码 ≥6 + 确认一致、重名内联报错）
 * + 重置密码 + 删除（不能删自己）。
 * 数据：GET/POST /api/users、PUT /api/users/:id/password、DELETE /api/users/:id（SPEC §5）。
 */
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { AlertCircle, Plus, UserPlus } from 'lucide-react'
import { useState, type FormEvent } from 'react'
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
import { useMe } from '@/lib/auth'
import { invalidateUsers, useUsers, type UserItem } from '@/lib/users'
import { utf8ByteLength } from '@/lib/utils'

/* ---------------------------------- 通用小部件 ---------------------------------- */

/** 服务端错误内联提示（重名 / 账号数上限等 400 响应） */
function ServerError({ message }: { message: string }) {
  return (
    <p className="flex items-start gap-1.5 text-sm text-red-600 dark:text-red-400">
      <AlertCircle className="mt-0.5 h-4 w-4 shrink-0" aria-hidden />
      {message}
    </p>
  )
}

/* ---------------------------------- 新建账号 ---------------------------------- */

function CreateUserModal({ onClose }: { onClose: () => void }) {
  const { t } = useTranslation(['users', 'common'])
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [confirm, setConfirm] = useState('')
  const [errors, setErrors] = useState<{ username?: string; password?: string; confirm?: string }>({})
  const [serverError, setServerError] = useState<string | null>(null)
  const queryClient = useQueryClient()

  const mutation = useMutation({
    mutationFn: (req: { username: string; password: string }) =>
      api<UserItem>('/api/users', { method: 'POST', body: req }),
    onSuccess: (user) => {
      toast.success(t('toast.created', { name: user.username }))
      invalidateUsers(queryClient)
      onClose()
    },
    onError: (err) => setServerError(err.message),
  })

  function setField(field: 'username' | 'password' | 'confirm', value: string) {
    if (field === 'username') setUsername(value)
    else if (field === 'password') setPassword(value)
    else setConfirm(value)
    setErrors((prev) => ({ ...prev, [field]: undefined }))
    setServerError(null)
  }

  function handleSubmit(e: FormEvent<HTMLFormElement>) {
    e.preventDefault()
    const nextErrors: typeof errors = {}
    const trimmed = username.trim()
    // 长度按 Unicode 码点计（对齐后端 utf8.RuneCountInString，emoji 等 ×2 的 UTF-16 length 不准）
    const codePoints = [...trimmed].length
    if (!trimmed) nextErrors.username = t('errors.usernameRequired')
    else if (codePoints < 2) nextErrors.username = t('errors.usernameMinLength')
    else if (codePoints > 32) nextErrors.username = t('errors.usernameMaxLength')
    // 上限按 UTF-8 字节计（对齐后端 validatePassword / bcrypt 72 字节输入上限）
    if (password.length < 6) nextErrors.password = t('errors.passwordMinLength')
    else if (utf8ByteLength(password) > 72) nextErrors.password = t('errors.passwordMaxLength')
    if (confirm !== password) nextErrors.confirm = t('errors.passwordMismatch')
    setErrors(nextErrors)
    if (Object.values(nextErrors).some(Boolean)) return
    mutation.mutate({ username: trimmed, password })
  }

  return (
    <Modal
      open
      onClose={onClose}
      title={t('actions.create')}
      footer={
        <>
          <Button variant="secondary" onClick={onClose} disabled={mutation.isPending}>
            {t('common:actions.cancel')}
          </Button>
          <Button type="submit" form="create-user-form" loading={mutation.isPending}>
            {t('actions.createSubmit')}
          </Button>
        </>
      }
    >
      <form id="create-user-form" onSubmit={handleSubmit} className="space-y-4" noValidate>
        {serverError && <ServerError message={serverError} />}
        <Field label={t('fields.username')} error={errors.username} hint={t('hints.usernameLength')}>
          <Input
            name="username"
            value={username}
            onChange={(e) => setField('username', e.target.value)}
            placeholder={t('fields.username')}
            autoFocus
            autoComplete="off"
          />
        </Field>
        <Field label={t('fields.password')} error={errors.password} hint={t('hints.passwordMinLength')}>
          <Input
            name="password"
            type="password"
            value={password}
            onChange={(e) => setField('password', e.target.value)}
            placeholder={t('fields.password')}
            autoComplete="new-password"
          />
        </Field>
        <Field label={t('fields.confirmPassword')} error={errors.confirm}>
          <Input
            name="confirm"
            type="password"
            value={confirm}
            onChange={(e) => setField('confirm', e.target.value)}
            placeholder={t('placeholders.confirmPassword')}
            autoComplete="new-password"
          />
        </Field>
      </form>
    </Modal>
  )
}

/* ---------------------------------- 重置密码 ---------------------------------- */

function ResetPasswordModal({ user, onClose }: { user: UserItem; onClose: () => void }) {
  const { t } = useTranslation(['users', 'common'])
  const [password, setPassword] = useState('')
  const [confirm, setConfirm] = useState('')
  const [errors, setErrors] = useState<{ password?: string; confirm?: string }>({})
  const [serverError, setServerError] = useState<string | null>(null)

  const mutation = useMutation({
    mutationFn: (newPassword: string) =>
      api<{ ok: boolean }>(`/api/users/${user.id}/password`, {
        method: 'PUT',
        body: { new_password: newPassword },
      }),
    onSuccess: () => {
      toast.success(t('toast.passwordReset', { name: user.username }))
      onClose()
    },
    onError: (err) => setServerError(err.message),
  })

  function handleSubmit(e: FormEvent<HTMLFormElement>) {
    e.preventDefault()
    const nextErrors: typeof errors = {}
    // 上限按 UTF-8 字节计（对齐后端 validatePassword / bcrypt 72 字节输入上限）
    if (password.length < 6) nextErrors.password = t('errors.newPasswordMinLength')
    else if (utf8ByteLength(password) > 72) nextErrors.password = t('errors.passwordMaxLength')
    if (confirm !== password) nextErrors.confirm = t('errors.passwordMismatch')
    setErrors(nextErrors)
    if (Object.values(nextErrors).some(Boolean)) return
    mutation.mutate(password)
  }

  return (
    <Modal
      open
      onClose={onClose}
      title={t('resetModal.title', { name: user.username })}
      footer={
        <>
          <Button variant="secondary" onClick={onClose} disabled={mutation.isPending}>
            {t('common:actions.cancel')}
          </Button>
          <Button type="submit" form="reset-password-form" loading={mutation.isPending}>
            {t('actions.resetPassword')}
          </Button>
        </>
      }
    >
      <form id="reset-password-form" onSubmit={handleSubmit} className="space-y-4" noValidate>
        {serverError && <ServerError message={serverError} />}
        <Field label={t('fields.newPassword')} error={errors.password} hint={t('hints.passwordMinLength')}>
          <Input
            name="password"
            type="password"
            value={password}
            onChange={(e) => {
              setPassword(e.target.value)
              setErrors((prev) => ({ ...prev, password: undefined }))
              setServerError(null)
            }}
            placeholder={t('fields.newPassword')}
            autoFocus
            autoComplete="new-password"
          />
        </Field>
        <Field label={t('fields.confirmNewPassword')} error={errors.confirm}>
          <Input
            name="confirm"
            type="password"
            value={confirm}
            onChange={(e) => {
              setConfirm(e.target.value)
              setErrors((prev) => ({ ...prev, confirm: undefined }))
            }}
            placeholder={t('placeholders.confirmNewPassword')}
            autoComplete="new-password"
          />
        </Field>
        <p className="text-xs text-slate-500 dark:text-slate-400">{t('resetModal.note')}</p>
        <div className="rounded-lg border border-amber-200 bg-amber-50 p-3 text-xs leading-relaxed text-amber-700 dark:border-amber-500/30 dark:bg-amber-500/10 dark:text-amber-400">
          {t('resetModal.apiKeysWarning')}
        </div>
      </form>
    </Modal>
  )
}

/* ---------------------------------- 删除确认 ---------------------------------- */

function DeleteUserModal({ user, onClose }: { user: UserItem; onClose: () => void }) {
  const { t } = useTranslation(['users', 'common'])
  const queryClient = useQueryClient()
  const mutation = useMutation({
    mutationFn: () => api<{ ok: boolean }>(`/api/users/${user.id}`, { method: 'DELETE' }),
    onSuccess: () => {
      toast.success(t('toast.deleted', { name: user.username }))
      invalidateUsers(queryClient)
      onClose()
    },
    onError: (err) => toast.error(err.message),
  })

  return (
    <Modal
      open
      onClose={onClose}
      title={t('deleteModal.title')}
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
        {t('deleteModal.confirmPrefix')}{' '}
        <span className="font-medium text-slate-900 dark:text-white">
          {t('deleteModal.quotedName', { name: user.username })}
        </span>
        {t('deleteModal.confirmSuffix')}
      </p>
      <p className="mt-2 text-xs text-slate-500 dark:text-slate-400">{t('deleteModal.note')}</p>
    </Modal>
  )
}

/* ---------------------------------- 页面 ---------------------------------- */

export function UsersPage() {
  const { t } = useTranslation(['users', 'common'])
  const me = useMe()
  const usersQuery = useUsers()
  const [createOpen, setCreateOpen] = useState(false)
  const [resetTarget, setResetTarget] = useState<UserItem | null>(null)
  const [deleteTarget, setDeleteTarget] = useState<UserItem | null>(null)

  const meData = me.data
  const myUsername = meData?.username
  const items = usersQuery.data?.items ?? []

  return (
    <div className="space-y-6">
      <PageHeader
        title={t('page.title')}
        description={t('page.description')}
        actions={
          <Button onClick={() => setCreateOpen(true)}>
            <Plus className="h-4 w-4" aria-hidden />
            {t('actions.create')}
          </Button>
        }
      />

      <Card>
        {usersQuery.isPending ? (
          <QueryLoading skeleton="table" />
        ) : usersQuery.isError ? (
          <QueryError message={usersQuery.error.message} onRetry={() => usersQuery.refetch()} />
        ) : items.length === 0 ? (
          <EmptyState
            icon={UserPlus}
            title={t('empty.title')}
            description={t('empty.description')}
            action={
              <Button onClick={() => setCreateOpen(true)}>
                <Plus className="h-4 w-4" aria-hidden />
                {t('actions.create')}
              </Button>
            }
          />
        ) : (
          <TableScroll label={t('list.label')}>
            <Table label={t('list.label')}>
              <THead>
                <tr>
                  <TH>{t('fields.username')}</TH>
                  <TH>{t('common:fields.createdAt')}</TH>
                  <TH className="text-right">{t('common:fields.actions')}</TH>
                </tr>
              </THead>
              <TBody>
                {items.map((user) => {
                  // 优先按 id 判断（/api/auth/me 返回 {id, username}）；me 无 id 时回退用户名比对
                  const isSelf =
                    meData?.id !== undefined
                      ? user.id === meData.id
                      : myUsername !== undefined && user.username === myUsername
                  return (
                    <TRow key={user.id}>
                      <TD>
                        <span className="flex items-center gap-2">
                          <span className="font-medium text-slate-900 dark:text-white">
                            {user.username}
                          </span>
                          {isSelf && <Badge variant="info">{t('badge.self')}</Badge>}
                        </span>
                      </TD>
                      <TD>
                        <RelativeTime iso={user.created_at} />
                      </TD>
                      <TD className="text-right">
                        <div className="flex items-center justify-end gap-1">
                          <Button
                            variant="ghost"
                            className="h-7 px-2 text-xs"
                            onClick={() => setResetTarget(user)}
                          >
                            {t('actions.resetPassword')}
                          </Button>
                          <button
                            type="button"
                            disabled={isSelf}
                            title={isSelf ? t('deleteModal.cannotDeleteSelf') : undefined}
                            onClick={() => setDeleteTarget(user)}
                            className="inline-flex h-7 items-center rounded-lg px-2 text-xs text-red-600 transition-colors hover:bg-red-50 hover:text-red-700 disabled:cursor-not-allowed disabled:text-slate-300 disabled:hover:bg-transparent dark:text-red-400 dark:hover:bg-red-500/10 dark:disabled:text-slate-600 dark:disabled:hover:bg-transparent"
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

      {createOpen && <CreateUserModal onClose={() => setCreateOpen(false)} />}
      {resetTarget && (
        <ResetPasswordModal key={resetTarget.id} user={resetTarget} onClose={() => setResetTarget(null)} />
      )}
      {deleteTarget && (
        <DeleteUserModal key={deleteTarget.id} user={deleteTarget} onClose={() => setDeleteTarget(null)} />
      )}
    </div>
  )
}
