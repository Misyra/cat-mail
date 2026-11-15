/**
 * 活跃会话卡片（安全加固三件套，C）：本人活跃会话列表（IP / 设备 UA / 创建 /
 * 最后活跃），当前会话带「本机」标记；支持单条下线（当前会话不可下线——退出
 * 登录走顶栏菜单）与「下线其他会话」。
 * 数据：GET /api/sessions、DELETE /api/sessions/:id、POST /api/sessions/revoke-others
 * （lib/sessions.ts）。吊销只影响会话登录态，不影响 API Key。
 */
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { MonitorSmartphone } from 'lucide-react'
import { useState } from 'react'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import { QueryError, QueryLoading } from '@/components/QueryState'
import { RelativeTime } from '@/components/RelativeTime'
import { Badge } from '@/components/badges'
import { Button } from '@/components/ui/Button'
import { Card } from '@/components/ui/Card'
import { EmptyState } from '@/components/ui/EmptyState'
import { Modal } from '@/components/ui/Modal'
import {
  invalidateSessions,
  revokeOtherSessions,
  revokeSession,
  useSessions,
  type SessionItem,
} from '@/lib/sessions'

/** UA 压缩展示：截断超长串（完整值进 title 悬浮；空值兜底由调用方传入） */
function compactUA(ua: string): string {
  return ua.length > 64 ? ua.slice(0, 63) + '…' : ua
}

/** 单条下线确认弹窗 */
function RevokeSessionModal({ session, onClose }: { session: SessionItem; onClose: () => void }) {
  const queryClient = useQueryClient()
  const { t } = useTranslation(['settings', 'common'])
  const mutation = useMutation({
    mutationFn: () => revokeSession(session.id),
    onSuccess: () => {
      toast.success(t('sessions.revokedToast'))
      invalidateSessions(queryClient)
      onClose()
    },
    onError: (err) => toast.error(err.message),
  })

  return (
    <Modal
      open
      onClose={onClose}
      title={t('sessions.revokeTitle')}
      footer={
        <>
          <Button variant="secondary" onClick={onClose} disabled={mutation.isPending}>
            {t('common:actions.cancel')}
          </Button>
          <Button variant="danger" loading={mutation.isPending} onClick={() => mutation.mutate()}>
            {t('sessions.revokeConfirm')}
          </Button>
        </>
      }
    >
      <p className="text-sm text-slate-600 dark:text-slate-300">
        {t('sessions.revokeBodyPrefix')} <span className="font-medium text-slate-900 dark:text-white">{session.ip || t('sessions.unknownIp')}</span>{' '}
        {t('sessions.revokeBodySuffix')}
      </p>
      <p className="mt-2 text-xs text-slate-500 dark:text-slate-400">
        {t('sessions.revokeNote')}
      </p>
    </Modal>
  )
}

/** 下线其他全部会话确认弹窗 */
function RevokeOthersModal({ count, onClose }: { count: number; onClose: () => void }) {
  const queryClient = useQueryClient()
  const { t } = useTranslation(['settings', 'common'])
  const mutation = useMutation({
    mutationFn: revokeOtherSessions,
    onSuccess: (res) => {
      toast.success(t('sessions.revokedOthersToast', { count: res.revoked }))
      invalidateSessions(queryClient)
      onClose()
    },
    onError: (err) => toast.error(err.message),
  })

  return (
    <Modal
      open
      onClose={onClose}
      title={t('sessions.revokeOthersTitle')}
      footer={
        <>
          <Button variant="secondary" onClick={onClose} disabled={mutation.isPending}>
            {t('common:actions.cancel')}
          </Button>
          <Button variant="danger" loading={mutation.isPending} onClick={() => mutation.mutate()}>
            {t('sessions.revokeOthersConfirm', { count })}
          </Button>
        </>
      }
    >
      <p className="text-sm text-slate-600 dark:text-slate-300">
        {t('sessions.revokeOthersBodyPrefix')} <span className="font-medium text-slate-900 dark:text-white">{count}</span>{' '}
        {t('sessions.revokeOthersBodySuffix', { count })}
      </p>
      <p className="mt-2 text-xs text-slate-500 dark:text-slate-400">{t('sessions.apiKeyNote')}</p>
    </Modal>
  )
}

export function SessionsCard() {
  const sessionsQuery = useSessions()
  const { t } = useTranslation(['settings', 'common'])
  const [revokeTarget, setRevokeTarget] = useState<SessionItem | null>(null)
  const [revokeOthersOpen, setRevokeOthersOpen] = useState(false)

  const items = sessionsQuery.data?.items ?? []
  const others = items.filter((s) => !s.current).length

  return (
    <Card>
      <div className="flex items-center justify-between gap-4 border-b border-slate-200 px-5 py-4 dark:border-slate-800">
        <div className="min-w-0">
          <h2 className="flex items-center gap-2 text-sm font-semibold text-slate-900 dark:text-white">
            <MonitorSmartphone className="h-4 w-4" aria-hidden />
            {t('sessions.title')}
          </h2>
          <p className="mt-0.5 text-xs text-slate-500 dark:text-slate-400">
            {t('sessions.description')}
          </p>
        </div>
        {others > 0 && (
          <Button
            variant="secondary"
            className="shrink-0"
            onClick={() => setRevokeOthersOpen(true)}
          >
            {t('sessions.revokeOthersButton', { count: others })}
          </Button>
        )}
      </div>

      {sessionsQuery.isPending ? (
        <QueryLoading skeleton="table" />
      ) : sessionsQuery.isError ? (
        <QueryError message={sessionsQuery.error.message} onRetry={() => sessionsQuery.refetch()} />
      ) : items.length === 0 ? (
        <EmptyState
          icon={MonitorSmartphone}
          title={t('sessions.emptyTitle')}
          description={t('sessions.emptyDescription')}
        />
      ) : (
        <ul className="divide-y divide-slate-100 dark:divide-slate-800">
          {items.map((s) => (
            <li key={s.id} className="flex items-center justify-between gap-4 px-5 py-3">
              <div className="min-w-0">
                <p className="flex flex-wrap items-center gap-2 text-sm text-slate-700 dark:text-slate-300">
                  <span className="truncate" title={s.user_agent}>
                    {compactUA(s.user_agent) || t('sessions.unknownDevice')}
                  </span>
                  {s.current && <Badge variant="info">{t('sessions.currentBadge')}</Badge>}
                </p>
                <p className="mt-0.5 truncate text-xs text-slate-500 dark:text-slate-400">
                  IP {s.ip || t('common:state.unknown')} · {t('sessions.createdAtLabel')} <RelativeTime iso={s.created_at} /> ·{' '}
                  {t('sessions.lastActiveLabel')} <RelativeTime iso={s.last_seen_at} />
                </p>
              </div>
              {!s.current && (
                <Button
                  variant="ghost"
                  className="h-7 shrink-0 px-2 text-xs"
                  onClick={() => setRevokeTarget(s)}
                >
                  {t('sessions.revoke')}
                </Button>
              )}
            </li>
          ))}
        </ul>
      )}

      {revokeTarget && (
        <RevokeSessionModal
          key={revokeTarget.id}
          session={revokeTarget}
          onClose={() => setRevokeTarget(null)}
        />
      )}
      {revokeOthersOpen && (
        <RevokeOthersModal count={others} onClose={() => setRevokeOthersOpen(false)} />
      )}
    </Card>
  )
}
