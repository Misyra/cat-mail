/**
 * 域名治理页（轻量，无实时 DNS 检测）：从各渠道 allowed_domains 聚合全部发件
 * 域名，展示「域名 × 渠道」矩阵——每个域名一张汇总卡，卡内按平台（provider）
 * 分节展示该平台要求的域名验证记录（SPF/DKIM/DMARC/MX/发件人验证）与配置指引。
 * 纯展示 + 文案复制：不改渠道数据、不发起 DNS 查询；平台静态知识见
 * lib/domainGuides.ts（记录值以各平台控制台为准）。
 */
import { Check, Copy, Inbox, Info } from 'lucide-react'
import { useState } from 'react'
import { useTranslation } from 'react-i18next'
import { Link } from 'react-router'
import { toast } from 'sonner'

import { PageHeader } from '@/components/PageHeader'
import { QueryError, QueryLoading } from '@/components/QueryState'
import { Badge, ChannelStatusBadge, ProviderBadge } from '@/components/badges'
import { Button } from '@/components/ui/Button'
import { Card } from '@/components/ui/Card'
import { EmptyState } from '@/components/ui/EmptyState'
import { TD, TH, TBody, THead, TRow, Table } from '@/components/ui/Table'
import { copyText } from '@/lib/clipboard'
import { useChannels, type Channel } from '@/lib/channels'
import {
  KIND_KEYS,
  getDomainGuide,
  parseAllowedDomains,
  type DomainGuideRecord,
} from '@/lib/domainGuides'

/** 域名聚合条目：一个发件域名 + 涉及的渠道 */
interface DomainEntry {
  domain: string
  channels: Channel[]
}

/**
 * 从渠道列表聚合域名矩阵：allowed_domains（逗号分隔）逐渠道解析，同一域名
 * 多渠道命中时合并；未配置 allowed_domains（空 = 不限制）的渠道单独归入
 * 无限制列表，不参与矩阵。
 */
export function aggregateDomains(channels: Channel[]): { domains: DomainEntry[]; unlimited: Channel[] } {
  const map = new Map<string, Channel[]>()
  const unlimited: Channel[] = []
  for (const ch of channels) {
    const domains = parseAllowedDomains(ch.allowed_domains)
    if (domains.length === 0) {
      unlimited.push(ch)
      continue
    }
    for (const d of domains) {
      const list = map.get(d)
      if (list) {
        if (!list.some((c) => c.id === ch.id)) list.push(ch)
      } else {
        map.set(d, [ch])
      }
    }
  }
  const domains = [...map.entries()]
    .map(([domain, chs]) => ({ domain, channels: chs }))
    .sort((a, b) => a.domain.localeCompare(b.domain))
  return { domains, unlimited }
}

/** 复制按钮：成功打勾短暂反馈，失败 toast 提示 */
function CopyButton({ text, label }: { text: string; label: string }) {
  const { t } = useTranslation(['domains', 'common'])
  const [done, setDone] = useState(false)
  async function handleCopy() {
    try {
      await copyText(text)
      setDone(true)
      toast.success(t('copy.successToast', { label }))
      window.setTimeout(() => setDone(false), 1500)
    } catch {
      toast.error(t('copy.failedHint'))
    }
  }
  return (
    <Button
      variant="secondary"
      className="h-7 gap-1 px-2 text-xs"
      onClick={() => void handleCopy()}
      aria-label={t('copy.buttonAria', { label })}
    >
      {done ? (
        <Check className="h-3 w-3 text-emerald-600 dark:text-emerald-400" aria-hidden />
      ) : (
        <Copy className="h-3 w-3" aria-hidden />
      )}
      {t('common:actions.copy')}
    </Button>
  )
}

/**
 * 单平台验证指引小节：摘要 + 记录表 + 控制台提示 + 整份复制。
 * 整份指引的复制文案在此拼装（原 lib/formatGuideText 逻辑，输出逐字一致）。
 */
function GuideSection({ provider }: { provider: string }) {
  const { t } = useTranslation(['domains', 'common'])
  const guide = getDomainGuide(provider)
  const copyLabel = t('guide.copyTitle', { name: t(guide.label) })
  const copyText = [
    t('copy.guideHeader', { label: t(guide.label), summary: t(guide.summary) }),
    ...guide.records.map((r: DomainGuideRecord) =>
      t('copy.recordLine', {
        kind: t(KIND_KEYS[r.kind]),
        host: t(r.host),
        value: t(r.value),
        requirement: r.required ? t('term.required') : t('term.recommended'),
        desc: t(r.desc),
      }),
    ),
    t(guide.consoleHint),
  ].join('\n')
  return (
    <section className="rounded-lg border border-border-subtle bg-surface p-3">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <div className="flex items-center gap-2">
          <ProviderBadge provider={provider} />
          <span className="text-xs text-slate-500 dark:text-slate-400">{t(guide.summary)}</span>
        </div>
        <CopyButton text={copyText} label={copyLabel} />
      </div>
      <Table label={t('guide.recordsTitle', { name: t(guide.label) })} className="mt-2 text-xs">
        <THead>
          <tr>
            <TH className="w-24">{t('table.kind')}</TH>
            <TH className="w-44">{t('table.host')}</TH>
            <TH>{t('table.value')}</TH>
            <TH className="w-16">{t('table.necessity')}</TH>
            <TH>{t('common:fields.description')}</TH>
          </tr>
        </THead>
        <TBody>
          {guide.records.map((r: DomainGuideRecord) => (
            <TRow key={`${r.kind}-${r.host}`}>
              <TD className="font-medium">
                <Badge variant={r.required ? 'info' : 'neutral'}>{t(KIND_KEYS[r.kind])}</Badge>
              </TD>
              <TD className="font-mono text-[11px]">{t(r.host)}</TD>
              <TD className="max-w-56">
                <span
                  className="block truncate font-mono text-[11px] text-slate-600 dark:text-slate-300"
                  title={t(r.value)}
                >
                  {t(r.value)}
                </span>
              </TD>
              <TD>
                <span className={r.required ? 'text-slate-900 dark:text-white' : 'text-slate-400 dark:text-slate-500'}>
                  {r.required ? t('term.required') : t('term.recommended')}
                </span>
              </TD>
              <TD className="max-w-72 text-slate-500 dark:text-slate-400">{t(r.desc)}</TD>
            </TRow>
          ))}
        </TBody>
      </Table>
      <p className="mt-2 flex items-start gap-1 text-xs text-slate-500 dark:text-slate-400">
        <Info className="mt-0.5 h-3 w-3 shrink-0" aria-hidden />
        {t(guide.consoleHint)}
      </p>
    </section>
  )
}

export function DomainsPage() {
  const { t } = useTranslation(['domains'])
  const channelsQuery = useChannels()

  if (channelsQuery.isPending) {
    return (
      <div className="space-y-6">
        <PageHeader
          title={t('page.title')}
          description={t('page.description')}
        />
        <Card>
          <QueryLoading skeleton="table" />
        </Card>
      </div>
    )
  }
  if (channelsQuery.isError) {
    return (
      <div className="space-y-6">
        <PageHeader
          title={t('page.title')}
          description={t('page.description')}
        />
        <Card>
          <QueryError message={channelsQuery.error.message} onRetry={() => channelsQuery.refetch()} />
        </Card>
      </div>
    )
  }

  const channels = channelsQuery.data?.items ?? []
  const { domains, unlimited } = aggregateDomains(channels)

  return (
    <div className="space-y-6">
      <PageHeader
        title={t('page.title')}
        description={t('page.descriptionFull')}
        actions={
          domains.length > 0 ? (
            <Badge variant="neutral">
              {`${t('page.domainCount', { count: domains.length })} · ${t('page.channelCount', { count: channels.length })}`}
            </Badge>
          ) : undefined
        }
      />

      {/* 未限制发件域名的渠道：不参与矩阵，但提示其无域名约束 */}
      {unlimited.length > 0 && (
        <Card className="flex flex-wrap items-center gap-2 p-4 text-xs text-slate-500 dark:text-slate-400">
          <Info className="h-4 w-4 shrink-0" aria-hidden />
          <span>{t('page.unlimitedNotice', { count: unlimited.length })}</span>
          {unlimited.map((ch) => (
            <span key={ch.id} className="inline-flex items-center gap-1">
              <ProviderBadge provider={ch.provider} />
              <span className="font-medium text-slate-700 dark:text-slate-300">{ch.name}</span>
            </span>
          ))}
        </Card>
      )}

      {domains.length === 0 ? (
        <Card>
          <EmptyState
            icon={Inbox}
            title={t('empty.title')}
            description={t('empty.description')}
            action={
              <Button as={Link} to="/channels" variant="secondary">
                {t('empty.action')}
              </Button>
            }
          />
        </Card>
      ) : (
        <div className="grid grid-cols-1 items-start gap-4 xl:grid-cols-2">
          {domains.map((entry) => {
            // 同一域名下按平台去重：同 provider 的多个渠道共用一份指引
            const providers = [...new Set(entry.channels.map((c) => c.provider))]
            return (
              <Card key={entry.domain} className="space-y-3 p-4">
                <div className="flex flex-wrap items-center justify-between gap-2">
                  <h2 className="font-mono text-sm font-semibold text-slate-900 dark:text-white">
                    {entry.domain}
                  </h2>
                  <Badge variant="info">{t('page.channelCount', { count: entry.channels.length })}</Badge>
                </div>
                <div className="flex flex-wrap items-center gap-1.5">
                  {entry.channels.map((ch) => (
                    <span
                      key={ch.id}
                      className="inline-flex items-center gap-1 rounded-md border border-border-subtle bg-surface px-1.5 py-0.5 text-xs"
                    >
                      <span className="font-medium text-slate-700 dark:text-slate-300">{ch.name}</span>
                      <ChannelStatusBadge status={ch.status} />
                    </span>
                  ))}
                </div>
                {providers.map((p) => (
                  <GuideSection key={p} provider={p} />
                ))}
              </Card>
            )
          })}
        </div>
      )}
    </div>
  )
}
